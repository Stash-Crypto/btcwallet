package spv

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"

	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
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

type Config struct {
	// The version to be returned from a getinfo request.
	Version int32

	//
	Proxy string

	//
	MinRelayTxFee btcutil.Amount

	// Whether to start in passive mode.
	Passive *bool

	// The maximum number of
	MaxFilterNewMatches *uint32

	// Config for the spvwallet Peers type.
	Peers *spvwallet.PeerManagerConfig
}

// SPV is a way of queryng the blockchain that relies on being
// connected to other peers and using the spv protocol.
type SPV struct {
	headers    *Headers
	manager    *spvwallet.SPVManager
	config     *Config
	txStore    *TxStore
	timeSource blockchain.MedianTimeSource

	enqueue chan chain.Notification
	dequeue chan chain.Notification
	quit    chan struct{}
	wg      sync.WaitGroup
	
	// Whether to run in passive mode. In passive mode, the spv manager
	// does not ask for new transactions. 
	passive bool
}

func initializeHeaders(db walletdb.DB, params *chaincfg.Params) (*Headers, error) {
	headers, err := NewHeaders(db)
	
	if err == nil {
		return headers, nil
	}
			
	// If there is a problem, delete the spv data and start over. 
	deleteSPVData(db)
	headers, err = NewHeaders(db)
	if err != nil {
		return nil, err
	}
	
	// Insert the genesis block. 
	headers.Put(spvwallet.StoredHeader{
		Header: params.GenesisBlock.Header, 
		Height: 0, 
		TotalWork: big.NewInt(0),
	}, true)
	
	return headers, nil
}

// New creates a new SPV type.
func New(accounts []uint32, db walletdb.DB, addrmgr *waddrmgr.Manager,
	txs *wtxmgr.Store, config *Config) (*SPV, error) {
	if config == nil || config.Peers == nil {
		return nil, errors.New("Must include peer config parameters")
	}
	
	params := config.Peers.Params

	var err error

	headers, err := initializeHeaders(db, params)
	if err != nil {
		return nil, err
	}

	spv := &SPV{
		config:     config,
		headers:    headers,
		timeSource: blockchain.NewMedianTime(),
		enqueue:    make(chan chain.Notification),
		dequeue:    make(chan chain.Notification),
		quit:       make(chan struct{}),
	}

	var maxFilterNewMatches uint32
	spvconfig := &spvwallet.Config{
		CreationDate: headers.CreationDate(),
	}
	if config.MaxFilterNewMatches != nil {
		maxFilterNewMatches = *config.MaxFilterNewMatches
	} else {
		maxFilterNewMatches = spvwallet.DefaultMaxFilterNewMatches
	}
	spvconfig.MaxFilterNewMatches = maxFilterNewMatches
	spv.txStore, err = newTxStore(accounts, headers, db,
		addrmgr, txs, params, maxFilterNewMatches, spv.enqueue)
	if err != nil {
		return nil, err
	}
	
	blockchain, err := spvwallet.NewBlockchain(headers, spvconfig.CreationDate, params)
	if err != nil {
		return nil, err
	}
	
	spv.manager, err = spvwallet.NewSPVManager(spv.txStore, blockchain, config.Peers, spvconfig)
	if err != nil {
		return nil, err
	}

	spv.config.Peers.Listeners = &peer.MessageListeners{
		OnVersion: func(p *peer.Peer, m *wire.MsgVersion) {
			spv.timeSource.AddTimeSample(p.Addr(), m.Timestamp)
		},
		OnMerkleBlock: spv.manager.OnMerkleBlock,
		OnInv:         spv.manager.OnInv,
		OnTx:          spv.manager.OnTx,
		OnGetData:     spv.manager.OnGetData,
	}

	if config.Passive != nil && *config.Passive == true {
		spv.Deactivate()
	} else {
		spv.Activate()
	}

	// Set the headers to notify the wallet when there is a new block.
	headers.notifications = spv.enqueue

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
	spv.headers.Close()
	close(spv.quit)
	close(spv.enqueue)
}

func (spv *SPV) Activate() error {
	if spv == nil {
		return nil
	}
	
	spv.passive = false

	// Generate new filter.
	f, err := spv.txStore.GimmeFilter()
	if err != nil {
		return err
	}

	// Send the filter around to everybody.
	return spv.manager.PeerManager.FilterLoad(f.MsgFilterLoad())
}

func (spv *SPV) Deactivate() error {
	if spv == nil {
		return nil
	}
	
	spv.passive = true

	return spv.manager.PeerManager.FilterLoad(BlankFilter().MsgFilterLoad())
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

func (spv *SPV) rollback(lastGoodHeight uint32) error {
	err := spvwallet.ProcessReorg(spv.txStore, lastGoodHeight)
	if err != nil {
		return err
	}
	
	err = spv.txStore.txStore.Rollback(int32(lastGoodHeight) + 1)
	if err != nil {
		return err
	}
	
	return rollback(spv.headers, lastGoodHeight)
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
	best, err := spv.headers.GetBestHeader()
	if err != nil {
		return nil, err
	}
	
	sh, err := spv.headers.GetHeader(*blockHash)
	if err != nil {
		return nil, err
	}
	blockHeader := sh.Header
	
	return &btcjson.GetBlockVerboseResult {
		Version:       blockHeader.Version,
		VersionHex:    fmt.Sprintf("%08x", blockHeader.Version),
		MerkleRoot:    blockHeader.MerkleRoot.String(),
		PreviousHash:  blockHeader.PrevBlock.String(),
		Nonce:         blockHeader.Nonce,
		Time:          blockHeader.Timestamp.Unix(),
		Bits:          strconv.FormatInt(int64(blockHeader.Bits), 16),
		Difficulty:    spv.getDifficultyRatio(blockHeader.Bits),
		Confirmations: uint64(1 + best.Height - sh.Height),
		Hash: blockHash.String(), 
		Height: int64(sh.Height), 
	}, nil
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

// TODO update comment. 
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
	if spv.passive {
		return nil
	}
	
	f, err := spv.txStore.GimmeFilter()
	if err != nil {
		return err
	}
	
	return spv.manager.PeerManager.FilterLoad(f.MsgFilterLoad())
}

func (spv *SPV) GetBlockVerboseAsync(blockHash *chainhash.Hash) rpcclient.FutureGetBlockVerboseResult {
	panic("Not implemented.")
}

// getDifficultyRatio returns the proof-of-work difficulty as a multiple of the
// minimum difficulty using the passed bits field from the header of a block.
func (spv *SPV) getDifficultyRatio(bits uint32) float64 {
	// The minimum difficulty is the max possible proof-of-work limit bits
	// converted back to a number.  Note this is not the same as the proof of
	// work limit directly because the block difficulty is encoded in a block
	// with the compact form which loses precision.
	max := blockchain.CompactToBig(spv.config.Peers.Params.PowLimitBits)
	target := blockchain.CompactToBig(bits)

	difficulty := new(big.Rat).SetFrac(max, target)
	outString := difficulty.FloatString(8)
	diff, err := strconv.ParseFloat(outString, 64)
	if err != nil {
		//rpcsLog.Errorf("Cannot get difficulty: %v", err)
		return 0
	}
	return diff
}

//
func (spv *SPV) GetInfo() (*btcjson.InfoWalletResult, error) {
	best, err := spv.headers.GetBestHeader()
	if err != nil {
		return nil, err
	}
	return &btcjson.InfoWalletResult{
		Version:         spv.config.Version,
		ProtocolVersion: int32(peer.MaxProtocolVersion),
		Blocks:          int32(best.Height),
		TimeOffset:      int64(spv.timeSource.Offset().Seconds()),
		Connections:     int32(len(spv.manager.PeerManager.ReadyPeers())),
		Proxy:           spv.config.Proxy,
		Difficulty:      spv.getDifficultyRatio(best.Header.Bits),
		TestNet:         spv.config.Peers.Params == &chaincfg.TestNet3Params,
		RelayFee:        spv.config.MinRelayTxFee.ToBTC(),
	}, nil
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
