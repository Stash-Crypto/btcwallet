package spv

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

var (
	// ErrNotImplemented is returned for functions which should work,
	// but don't.
	ErrNotImplemented = errors.New("Not yet implemented.")

	// ErrInvalidSPVRequest is returned for functions that do not work
	// in spv mode.
	ErrInvalidSPVRequest = errors.New("This function does not work in spv mode.")
)

// SPV is a way of queryng the blockchain that relies on being
// connected to other peers and using the spv protocol.
type SPV struct {
	headers *Headers
	manager *spvwallet.SPVManager
	config  *spvwallet.PeerManagerConfig
	txStore *TxStore

	enqueue chan chain.Notification
	dequeue chan chain.Notification
	quit    chan struct{}
	wg      sync.WaitGroup
}

// New creates a new SPV type.
func New(accounts []uint32, db walletdb.DB, addrmgr *waddrmgr.Manager,
	txs *wtxmgr.Store, config *spvwallet.PeerManagerConfig) (*SPV, error) {

	var err error

	println("About to delete spv data.")

	deleteSPVData(db)

	headers, err := NewHeaders(db)
	if err != nil {
		return nil, err
	}

	spvconfig := &spvwallet.Config{
		CreationDate:        headers.CreationDate(),
		MaxFilterNewMatches: spvwallet.DefaultMaxFilterNewMatches,
	}
	/*var maxFilterNewMatches uint32
	if config.MaxFilterNewMatches != nil {
		maxFilterNewMatches = *config.MaxFilterNewMatches
	} else {
		maxFilterNewMatches = spvwallet.DefaultMaxFilterNewMatches
	}
	spvconfig.MaxFilterNewMatches = spvwallet.DefaultMaxFilterNewMatches
	spv.txStore, err = newTxStore(accounts, headers, db,
		addrmgr, txs, params, maxFilterNewMatches, spv.enqueue)
	if err != nil {
		return nil, err
	}*/

	spv := &SPV{
		config:  config,
		headers: headers,
		enqueue: make(chan chain.Notification),
		dequeue: make(chan chain.Notification),
		quit:    make(chan struct{}),
	}

	blockchain, err := spvwallet.NewBlockchain(headers, spvconfig.CreationDate, config.Params)
	if err != nil {
		return nil, err
	}

	spv.txStore, err = newTxStore(accounts, headers, db,
		addrmgr, txs, config.Params, spv.enqueue)
	if err != nil {
		return nil, err
	}
	spv.config.GetFilter = spv.txStore.GimmeFilter
	spv.manager, err = spvwallet.NewSPVManager(spv.txStore, blockchain, spv.config, spvconfig)
	if err != nil {
		return nil, err
	}

	spv.config.Listeners = &peer.MessageListeners{
		OnMerkleBlock: spv.manager.OnMerkleBlock,
		OnInv:         spv.manager.OnInv,
		OnTx:          spv.manager.OnTx,
		OnGetData:     spv.manager.OnGetData,
	}

	return spv, nil
}

// Start starts the spv client.
func (spv *SPV) Start() error {
	spv.manager.Start()
	spv.wg.Add(1)
	go spv.handler()
	return nil
}

// WaitForShutdown blocks until both the client has finished disconnecting
// and all handlers have exited.
func (spv *SPV) WaitForShutdown() {
	spv.manager.WaitForShutdown()
}

// Stop disconnects the client and signals the shutdown of all goroutines
// started by Start.
func (spv *SPV) Stop() {
	spv.manager.Close()
	close(spv.quit)
	close(spv.enqueue)
}

// handler maintains a queue of notifications and the current state (best
// block) of the chain.
func (spv *SPV) handler() error {
	defer func() {
		close(spv.dequeue)
		spv.wg.Done()
	}()

	/*hash, height, err := spv.GetBestBlock()
	if err != nil {
		return err
	}*/

	//bs := &waddrmgr.BlockStamp{Hash: *hash, Height: height}

	// TODO: Rather than leaving this as an unbounded queue for all types of
	// notifications, try dropping ones where a later enqueued notification
	// can fully invalidate one waiting to be processed.  For example,
	// blockconnected notifications for greater block heights can remove the
	// need to process earlier blockconnected notifications still waiting
	// here.

	var notifications []chain.Notification
	enqueue := spv.enqueue
	var dequeue chan chain.Notification
	var next chain.Notification

	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no notifications are queued for handling,
				// the queue is finished.
				if len(notifications) == 0 {
					return nil
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}

			if len(notifications) == 0 {
				next = n
			} else {
				next = notifications[0]
				notifications[0] = nil
				notifications = notifications[1:]
				notifications = append(notifications, n)
			}

			dequeue = spv.dequeue
			//pingChan = time.After(time.Minute)

		case dequeue <- next:
			/*if n, ok := next.(chain.BlockConnected); ok {
				bs = &waddrmgr.BlockStamp{
					Height: n.Height,
					Hash:   n.Hash,
				}
			}*/

			if len(notifications) != 0 {
				next = notifications[0]
				notifications[0] = nil
				notifications = notifications[1:]
			} else {
				// If no more notifications can be enqueued, the
				// queue is finished.
				if enqueue == nil {
					return nil
				}
				dequeue = nil
			}

		// TODO fill this back in.
		//case spv.currentBlock <- bs:

		case <-spv.quit:
			return nil
		}
	}
}

// SendRawTransaction submits the encoded transaction to the server which will
// then relay it to the network.
func (spv *SPV) SendRawTransaction(tx *wire.MsgTx, alloHighFees bool) (*chainhash.Hash, error) {
	if !alloHighFees {
		return nil, errors.New("SPV mode does not check for high fees.")
	}

	err := spv.manager.Broadcast(tx)
	if err != nil {
		return nil, err
	}

	hash := tx.TxHash()

	return &hash, nil
}

// BlockStamp returns the latest block notified by the client, or an error
// if the client has been shut down.
//
// TODO
func (spv *SPV) BlockStamp() (*waddrmgr.BlockStamp, error) {
	/*last := spv.blockchain.GetNPrevBlockHashes(1)
	return &waddrmgr.BlockStamp{
		Hash: last[0],
		Height: ,
	}, nil*/
	return nil, ErrNotImplemented
}

// GetBlock returns a raw block from the server given its hash.
//
// See GetBlockVerbose to retrieve a data structure with information about the
// block instead.
func (spv *SPV) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, ErrNotImplemented
}

// GetBestBlock returns the hash and height of the block in the longest (best)
// chain.
//
// NOTE: This is a btcd extension.
//
// TODO
func (spv *SPV) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, ErrNotImplemented
}

// GetBlockVerbose returns a data structure from the server with information
// about a block given its hash.
//
// See GetBlockVerboseTx to retrieve transaction data structures as well.
// See GetBlock to retrieve a raw block instead.
func (spv *SPV) GetBlockVerbose(blockHash *chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
	return nil, ErrNotImplemented
}

// GetBlockHash returns the hash of the block in the best block chain at the
// given height.
func (spv *SPV) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return spv.headers.GetBlockHash(blockHeight)
}

// Notifications returns a channel of parsed notifications sent by the remote
// bitcoin RPC server.  This channel must be continually read or the process
// may abort for running out memory, as unread notifications are queued for
// later reads.
//
// TODO
func (spv *SPV) Notifications() <-chan chain.Notification {
	return spv.dequeue
}

// RescanBlocks rescans the blocks identified by blockHashes, in order, using
// the client's loaded transaction filter.  The blocks do not need to be on the
// main chain, but they do need to be adjacent to each other.
//
// NOTE: This is a btcsuite extension ported from
// github.com/decred/dcrrpcclient.
//
// TODO
func (spv *SPV) RescanBlocks(blockHashes []chainhash.Hash) ([]btcjson.RescannedBlock, error) {
	//return c.RescanBlocksAsync(blockHashes).Receive()
	panic("Not implemented.")
}

// NotifyBlocks registers the client to receive notifications when blocks are
// connected and disconnected from the main chain.  The notifications are
// delivered to the notification handlers associated with the client.  Calling
// this function has no effect if there are no notification handlers and will
// result in an error if the client is configured to run in HTTP POST mode.
//
// The notifications delivered as a result of this call will be via one of
// OnBlockConnected or OnBlockDisconnected.
//
// NOTE: This is a btcd extension and requires a websocket connection.
//
// TODO
func (spv *SPV) NotifyBlocks() error {
	return spv.txStore.notifyBlocks()
}

// NotifyReceived registers the client to receive notifications every time a
// new transaction which pays to one of the passed addresses is accepted to
// memory pool or in a block connected to the block chain.  In addition, when
// one of these transactions is detected, the client is also automatically
// registered for notifications when the new transaction outpoints the address
// now has available are spent (See NotifySpent).  The notifications are
// delivered to the notification handlers associated with the client.  Calling
// this function has no effect if there are no notification handlers and will
// result in an error if the client is configured to run in HTTP POST mode.
//
// The notifications delivered as a result of this call will be via one of
// *OnRecvTx (for transactions that receive funds to one of the passed
// addresses) or OnRedeemingTx (for transactions which spend from one
// of the outpoints which are automatically registered upon receipt of funds to
// the address).
//
// NOTE: This is a btcd extension and requires a websocket connection.
//
// NOTE: Deprecated. Use LoadTxFilter instead.
func (spv *SPV) NotifyReceived(addresses []btcutil.Address) error {
	return spv.txStore.notifyReceived(addresses)
}

func (spv *SPV) GetBlockVerboseAsync(blockHash *chainhash.Hash) rpcclient.FutureGetBlockVerboseResult {
	panic("Not implemented.")
}

func (spv *SPV) GetInfo() (*btcjson.InfoWalletResult, error) {
	return nil, ErrNotImplemented
}

func (spv *SPV) GetTxOutAsync(txHash *chainhash.Hash, index uint32, mempool bool) rpcclient.FutureGetTxOutResult {
	panic("Not implemented.")
}

func (spv *SPV) POSTClient() (*rpcclient.Client, error) {
	return nil, ErrInvalidSPVRequest
}

func (spv *SPV) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	return nil, ErrInvalidSPVRequest
}

func (spv *SPV) Rescan(startBlock *chainhash.Hash, addresses []btcutil.Address, outpoints []*wire.OutPoint) error {
	return ErrNotImplemented
}

// LoadTxFilter loads, reloads, or adds data to a websocket client's transaction
// filter.  The filter is consistently updated based on inspected transactions
// during mempool acceptance, block acceptance, and for all rescanned blocks.
//
// NOTE: This is a btcd extension ported from github.com/decred/dcrrpcclient
// and requires a websocket connection.
/*func (spv *SPV) LoadTxFilter(reload bool, addresses []btcutil.Address, outPoints []wire.OutPoint) error {
	//return c.LoadTxFilterAsync(reload, addresses, outPoints).Receive()
	panic("Not implemented.")
}*/

func deleteSPVData(db walletdb.DB) {
	db.DeleteNamespace(spvNamespace)
}
