// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package transaction

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/bittorrent/go-btfs/transaction/crypto"
	"github.com/bittorrent/go-btfs/transaction/sctx"
	"github.com/bittorrent/go-btfs/transaction/storage"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"golang.org/x/net/context"

	logging "github.com/ipfs/go-log"
)

var logTran = logging.Logger("transaction:transactionService")

const (
	noncePrefix              = "transaction_nonce_"
	storedTransactionPrefix  = "transaction_stored_"
	pendingTransactionPrefix = "transaction_pending_"
)

var (
	// ErrTransactionReverted denotes that the sent transaction has been
	// reverted.
	ErrTransactionReverted = errors.New("transaction reverted")
	ErrUnknownTransaction  = errors.New("unknown transaction")
	ErrAlreadyImported     = errors.New("already imported")
	ErrGasPriceTooLow      = errors.New("gas price too low")
)

// TxRequest describes a request for a transaction that can be executed.
type TxRequest struct {
	To          *common.Address // recipient of the transaction
	Data        []byte          // transaction data
	GasPrice    *big.Int        // gas price or nil if suggested gas price should be used
	GasLimit    uint64          // gas limit or 0 if it should be estimated
	Value       *big.Int        // amount of wei to send
	Description string          // optional description
}

type StoredTransaction struct {
	To          *common.Address // recipient of the transaction
	Data        []byte          // transaction data
	GasPrice    *big.Int        // used gas price
	GasLimit    uint64          // used gas limit
	Value       *big.Int        // amount of wei to send
	Nonce       uint64          // used nonce
	Created     int64           // creation timestamp
	Description string          // description
}

// Service is the service to send transactions. It takes care of gas price, gas
// limit and nonce management.
type Service interface {
	io.Closer
	SenderAddress(ctx context.Context) (addr common.Address)
	EthereumAddress(ctx context.Context) (addr common.Address)
	OverlayEthAddress(ctx context.Context) (addr common.Address)
	// Send creates a transaction based on the request and sends it.
	Send(ctx context.Context, request *TxRequest) (txHash common.Hash, err error)
	// Call simulate a transaction based on the request.
	Call(ctx context.Context, request *TxRequest) (result []byte, err error)
	// WaitForReceipt waits until either the transaction with the given hash has been mined or the context is cancelled.
	// This is only valid for transaction sent by this service.
	WaitForReceipt(ctx context.Context, txHash common.Hash) (receipt *types.Receipt, err error)
	// WatchSentTransaction start watching the given transaction.
	// This wraps the monitors watch function by loading the correct nonce from the store.
	// This is only valid for transaction sent by this service.
	WatchSentTransaction(txHash common.Hash) (<-chan types.Receipt, <-chan error, error)
	// StoredTransaction retrieves the stored information for the transaction
	StoredTransaction(txHash common.Hash) (*StoredTransaction, error)
	// PendingTransactions retrieves the list of all pending transaction hashes
	PendingTransactions() ([]common.Hash, error)
	// ResendTransaction resends a previously sent transaction
	// This operation can be useful if for some reason the transaction vanished from the eth networks pending pool
	ResendTransaction(ctx context.Context, txHash common.Hash) error
	// CancelTransaction cancels a previously sent transaction by double-spending its nonce with zero-transfer one
	CancelTransaction(ctx context.Context, originalTxHash common.Hash) (common.Hash, error)
	// BalanceAt get btt balance from backend
	BttBalanceAt(ctx context.Context, address common.Address, block *big.Int) (*big.Int, error)
	// MyBttBalance get btt balance of current BTTC address
	MyBttBalance(ctx context.Context) (*big.Int, error)
}

type transactionService struct {
	wg     sync.WaitGroup
	lock   sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	backend Backend
	signer  crypto.Signer
	sender  common.Address
	store   storage.StateStorer
	chainID *big.Int
	monitor Monitor
}

// NewService creates a new transaction service.
func NewService(backend Backend, signer crypto.Signer, store storage.StateStorer, chainID *big.Int, monitor Monitor) (Service, error) {
	senderAddress, err := signer.EthereumAddress()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	t := &transactionService{
		ctx:     ctx,
		cancel:  cancel,
		backend: backend,
		signer:  signer,
		sender:  senderAddress,
		store:   store,
		chainID: chainID,
		monitor: monitor,
	}

	pendingTxs, err := t.PendingTransactions()
	if err != nil {
		return nil, err
	}
	for _, txHash := range pendingTxs {
		t.waitForPendingTx(txHash)
	}

	return t, nil
}

func (t *transactionService) SenderAddress(ctx context.Context) (addr common.Address) {
	return t.sender
}

func (t *transactionService) EthereumAddress(ctx context.Context) (addr common.Address) {
	return t.sender
}

func (t *transactionService) OverlayEthAddress(ctx context.Context) (addr common.Address) {
	return t.sender
}

// Send creates and signs a transaction based on the request and sends it.
func (t *transactionService) Send(ctx context.Context, request *TxRequest) (txHash common.Hash, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	nonce, err := t.nextNonce(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	tx, err := prepareTransaction(ctx, request, t.sender, t.backend, nonce)
	if err != nil {
		return common.Hash{}, err
	}

	signedTx, err := t.signer.SignTx(tx, t.chainID)
	if err != nil {
		return common.Hash{}, err
	}

	logTran.Infof("sending transaction %x with nonce %d", signedTx.Hash(), nonce)

	err = t.backend.SendTransaction(ctx, signedTx)
	if err != nil {
		return common.Hash{}, err
	}

	err = t.putNonce(nonce + 1)
	if err != nil {
		return common.Hash{}, err
	}

	// checkNextNonce
	//t.checkNextNonce(nonce)

	txHash = signedTx.Hash()

	err = t.store.Put(storedTransactionKey(txHash), StoredTransaction{
		To:          signedTx.To(),
		Data:        signedTx.Data(),
		GasPrice:    signedTx.GasPrice(),
		GasLimit:    signedTx.Gas(),
		Value:       signedTx.Value(),
		Nonce:       signedTx.Nonce(),
		Created:     time.Now().Unix(),
		Description: request.Description,
	})
	if err != nil {
		return common.Hash{}, err
	}

	err = t.store.Put(pendingTransactionKey(txHash), struct{}{})
	if err != nil {
		return common.Hash{}, err
	}

	t.waitForPendingTx(txHash)

	return signedTx.Hash(), nil
}

func (t *transactionService) checkNextNonce(lastNonce uint64) {
	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*3)
	defer cancel()

	for true {
		time.Sleep(time.Duration(time.Second) * 1)
		nonce, err := t.nextNonce(ctx)
		if err != nil {
			return
		}
		if nonce > lastNonce {
			return
		}

		fmt.Println("check next nonce ... lastNonce = ", lastNonce, time.Now())

		select {
		case <-ctx.Done():
			fmt.Println("check next nonce, it does not get next nonce.")
		default:
		}
	}
}

func (t *transactionService) waitForPendingTx(txHash common.Hash) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		_, err := t.WaitForReceipt(t.ctx, txHash)
		if err != nil {
			if !errors.Is(err, ErrTransactionCancelled) {
				logTran.Errorf("error while waiting for pending transaction %x: %v", txHash, err)
				return
			} else {
				logTran.Warningf("pending transaction %x cancelled", txHash)
			}
		} else {
			logTran.Infof("pending transaction %x confirmed", txHash)
		}

		err = t.store.Delete(pendingTransactionKey(txHash))
		if err != nil {
			logTran.Errorf("error while unregistering transaction as pending %x: %v", txHash, err)
		}
	}()
}

func (t *transactionService) Call(ctx context.Context, request *TxRequest) ([]byte, error) {
	msg := ethereum.CallMsg{
		From:     t.sender,
		To:       request.To,
		Data:     request.Data,
		GasPrice: request.GasPrice,
		Gas:      request.GasLimit,
		Value:    request.Value,
	}
	data, err := t.backend.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (t *transactionService) StoredTransaction(txHash common.Hash) (*StoredTransaction, error) {
	var tx StoredTransaction
	err := t.store.Get(storedTransactionKey(txHash), &tx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrUnknownTransaction
		}
		return nil, err
	}
	return &tx, nil
}

// prepareTransaction creates a signable transaction based on a request.
func prepareTransaction(ctx context.Context, request *TxRequest, from common.Address, backend Backend, nonce uint64) (tx *types.Transaction, err error) {
	var gasLimit uint64
	if request.GasLimit == 0 {
		gasLimit, err = backend.EstimateGas(ctx, ethereum.CallMsg{
			From: from,
			To:   request.To,
			Data: request.Data,
		})
		if err != nil {
			return nil, err
		}

		gasLimit += gasLimit / 5 // add 20% on top
	} else {
		gasLimit = request.GasLimit
	}

	var gasPrice *big.Int
	if request.GasPrice == nil {
		/*
			gasPrice, err = backend.SuggestGasPrice(ctx)
			if err != nil {
				return nil, err
			}
		*/
		gasPrice = big.NewInt(300000000000000)
	} else {
		gasPrice = request.GasPrice
	}

	if request.To != nil {
		return types.NewTransaction(
			nonce,
			*request.To,
			request.Value,
			gasLimit,
			gasPrice,
			request.Data,
		), nil
	}

	return types.NewContractCreation(
		nonce,
		request.Value,
		gasLimit,
		gasPrice,
		request.Data,
	), nil
}

func (t *transactionService) nonceKey() string {
	return fmt.Sprintf("%s%x", noncePrefix, t.sender)
}

func storedTransactionKey(txHash common.Hash) string {
	return fmt.Sprintf("%s%x", storedTransactionPrefix, txHash)
}

func pendingTransactionKey(txHash common.Hash) string {
	return fmt.Sprintf("%s%x", pendingTransactionPrefix, txHash)
}

func (t *transactionService) nextNonce(ctx context.Context) (uint64, error) {
	onchainNonce, err := t.backend.PendingNonceAt(ctx, t.sender)
	if err != nil {
		return 0, err
	}

	return onchainNonce, nil

	//var nonce uint64
	//err = t.store.Get(t.nonceKey(), &nonce)
	//if err != nil {
	//	// If no nonce was found locally used whatever we get from the backend.
	//	if errors.Is(err, storage.ErrNotFound) {
	//		return onchainNonce, nil
	//	}
	//	return 0, err
	//}
	//
	//// If the nonce onchain is larger than what we have there were external
	//// transactions and we need to update our nonce.
	//if onchainNonce > nonce {
	//	return onchainNonce, nil
	//}
	//return nonce, nil
}

func (t *transactionService) putNonce(nonce uint64) error {
	return t.store.Put(t.nonceKey(), nonce)
}

// WaitForReceipt waits until either the transaction with the given hash has
// been mined or the context is cancelled.
func (t *transactionService) WaitForReceipt(ctx context.Context, txHash common.Hash) (receipt *types.Receipt, err error) {
	receiptC, errC, err := t.WatchSentTransaction(txHash)
	if err != nil {
		return nil, err
	}
	select {
	case receipt := <-receiptC:
		return &receipt, nil
	case err := <-errC:
		return nil, err
	// don't wait longer than the context that was passed in
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *transactionService) WatchSentTransaction(txHash common.Hash) (<-chan types.Receipt, <-chan error, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	// loading the tx here guarantees it was in fact sent from this transaction service
	// also it allows us to avoid having to load the transaction during the watch loop
	storedTransaction, err := t.StoredTransaction(txHash)
	if err != nil {
		return nil, nil, err
	}

	return t.monitor.WatchTransaction(txHash, storedTransaction.Nonce)
}

func (t *transactionService) PendingTransactions() ([]common.Hash, error) {
	var txHashes []common.Hash = make([]common.Hash, 0)
	err := t.store.Iterate(pendingTransactionPrefix, func(key, value []byte) (stop bool, err error) {
		txHash := common.HexToHash(strings.TrimPrefix(string(key), pendingTransactionPrefix))
		txHashes = append(txHashes, txHash)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return txHashes, nil
}

func (t *transactionService) ResendTransaction(ctx context.Context, txHash common.Hash) error {
	storedTransaction, err := t.StoredTransaction(txHash)
	if err != nil {
		return err
	}

	var tx *types.Transaction
	if storedTransaction.To != nil {
		tx = types.NewTransaction(
			storedTransaction.Nonce,
			*storedTransaction.To,
			storedTransaction.Value,
			storedTransaction.GasLimit,
			storedTransaction.GasPrice,
			storedTransaction.Data,
		)
	} else {
		tx = types.NewContractCreation(
			storedTransaction.Nonce,
			storedTransaction.Value,
			storedTransaction.GasLimit,
			storedTransaction.GasPrice,
			storedTransaction.Data,
		)
	}

	signedTx, err := t.signer.SignTx(tx, t.chainID)
	if err != nil {
		return err
	}

	if signedTx.Hash() != txHash {
		return errors.New("transaction hash changed")
	}

	err = t.backend.SendTransaction(t.ctx, signedTx)
	if err != nil {
		if strings.Contains(err.Error(), "already imported") {
			return ErrAlreadyImported
		}
	}
	return nil
}

func (t *transactionService) CancelTransaction(ctx context.Context, originalTxHash common.Hash) (common.Hash, error) {
	storedTransaction, err := t.StoredTransaction(originalTxHash)
	if err != nil {
		return common.Hash{}, err
	}

	gasPrice := sctx.GetGasPrice(ctx)
	if gasPrice == nil {
		gasPrice = new(big.Int).Add(storedTransaction.GasPrice, big.NewInt(1))
	} else if gasPrice.Cmp(storedTransaction.GasPrice) <= 0 {
		return common.Hash{}, ErrGasPriceTooLow
	}

	signedTx, err := t.signer.SignTx(types.NewTransaction(
		storedTransaction.Nonce,
		t.sender,
		big.NewInt(0),
		21000,
		gasPrice,
		[]byte{},
	), t.chainID)
	if err != nil {
		return common.Hash{}, err
	}

	err = t.backend.SendTransaction(t.ctx, signedTx)
	if err != nil {
		return common.Hash{}, err
	}

	txHash := signedTx.Hash()
	err = t.store.Put(storedTransactionKey(txHash), StoredTransaction{
		To:          signedTx.To(),
		Data:        signedTx.Data(),
		GasPrice:    signedTx.GasPrice(),
		GasLimit:    signedTx.Gas(),
		Value:       signedTx.Value(),
		Nonce:       signedTx.Nonce(),
		Created:     time.Now().Unix(),
		Description: fmt.Sprintf("%s (cancellation)", storedTransaction.Description),
	})
	if err != nil {
		return common.Hash{}, err
	}

	err = t.store.Put(pendingTransactionKey(txHash), struct{}{})
	if err != nil {
		return common.Hash{}, err
	}

	t.waitForPendingTx(txHash)

	return txHash, err
}

func (t *transactionService) Close() error {
	t.cancel()
	t.wg.Wait()
	return nil
}

func (t *transactionService) BttBalanceAt(ctx context.Context, address common.Address, block *big.Int) (*big.Int, error) {
	return t.backend.BalanceAt(ctx, address, block)
}

func (t *transactionService) MyBttBalance(ctx context.Context) (*big.Int, error) {
	return t.BttBalanceAt(ctx, t.sender, nil)
}
