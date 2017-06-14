package spvfallback

import (
	"sync"

	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/chain/rpc"
	"github.com/btcsuite/btcwallet/chain/spv"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

// TODO make this a setting that can be changed by the user. Right now
// it is set to one for testing purposes.
const TargetOutbound = 1

type fallback struct {
	rpcChain *rpc.RPCClient
	spvChain *spv.SPV
	primary  chain.Client
	mtx      sync.Mutex

	dequeue chan chain.Notification

	wg   sync.WaitGroup
	quit chan struct{}
}

func New(rpcChain *rpc.RPCClient, spvChain *spv.SPV) chain.Client {
	return &fallback{
		rpcChain: rpcChain,
		spvChain: spvChain,
		primary:  rpcChain,
		dequeue:  make(chan chain.Notification, 1),
		quit:     make(chan struct{}),
	}
}

func (f *fallback) Start() {
	f.wg.Add(2)
	queue := make(chan chain.Notification)
	go f.inHandler(2, f.rpcChain.Notifications(), f.spvChain.Notifications(), queue)
	go f.outHandler(queue, f.dequeue, f.quit)
}

func (f *fallback) Stop() {
	close(f.quit)
}

func (f *fallback) WaitForShutdown() {
	<-f.quit
}

func (f *fallback) inHandler(swapHeight uint32, rpcEnqueue,
	spvEnqueue <-chan chain.Notification, dequeue chan<- chain.Notification) {
	defer func() {
		f.wg.Done()
	}()

	// Keep track of the last block notification we've received from each channel.
	var rpcLast, spvLast, last *chain.BlockConnected

	var primary, secondary <-chan chain.Notification = rpcEnqueue, spvEnqueue
	var primaryLast, secondaryLast **chain.BlockConnected = &rpcLast, &spvLast

	// We keep track of all block notifications which we recieve
	// from the secondary channel ahead of the primary.
	var secondaryBlocks []*chain.BlockConnected

	// We also keep track of all other notifications from the secondary
	// channel which we have received since the last BlockConnected
	// notification.
	var secondaryNotifications []chain.Notification

	// We keep track of notifications from the secondary channel that we
	// receive following each BlockConnected
	pastNotifications := make(map[wtxmgr.Block][]chain.Notification)

	// updatePrimary handles notifications from the primary channel.
	updatePrimary := func(n chain.Notification) {
		switch v := n.(type) {
		case chain.BlockConnected:
			*primaryLast = &v

			if secondaryBlocks != nil {
				for len(secondaryBlocks) > 0 && secondaryBlocks[0].Height <= v.Height {
					delete(pastNotifications, secondaryBlocks[0].Block)
					secondaryBlocks = secondaryBlocks[1:]
				}

				//
				if len(secondaryBlocks) == 0 {
					secondaryBlocks = nil

					// Otherwise, copy the array so that we don't end up
					// using a lot of memory.
				} else {
					newList := make([]*chain.BlockConnected, len(secondaryBlocks))
					copy(newList, secondaryBlocks)
					secondaryBlocks = newList
				}
			}

			// If this is a BlockConnected message that we haven't
			// seen before, save it.
			if last == nil || last.Height < v.Height {
				last = &v
			}

		// In the case of a reorganization, we don't try to figure anything
		// out. We just reset as if we haven't heard any notifications yet.
		case chain.BlockDisconnected:
			rpcLast = nil
			spvLast = nil
			last = nil
			secondaryBlocks = nil
			pastNotifications = make(map[wtxmgr.Block][]chain.Notification)
		}

		// All notifications are passed along to the wallet.
		dequeue <- n
	}

	// swap switches the primary channel and takes the appropriate
	// action depending on which is the new primary.
	swap := func() {
		f.mtx.Lock()
		defer f.mtx.Unlock()

		// secondaryBlocks should not be nil here.
		swappedNotifications := pastNotifications
		swappedBlocks := make([]*chain.BlockConnected, len(secondaryBlocks))
		copy(swappedBlocks, secondaryBlocks)
		pastNotifications = make(map[wtxmgr.Block][]chain.Notification)
		secondaryBlocks = nil

		// Replay all notifications we saved from the secondary channel.
		for _, v := range swappedBlocks {
			for _, w := range swappedNotifications[v.Block] {
				updatePrimary(w)
			}

			updatePrimary(*v)
		}

		// Replay notifications that we've received since the latest
		// BlockConnected message.
		for _, w := range secondaryNotifications {
			updatePrimary(w)
		}

		// Take actions appropriate to specific kinds of channels.
		if primary == rpcEnqueue {
			primary = spvEnqueue
			secondary = rpcEnqueue
			primaryLast = &spvLast
			secondaryLast = &rpcLast

			// We've been listening over spv with an empty filter, so
			// we want to go back a bit with a real filter just in case
			// we missed something.

			// Some might wonder why
			// TODO finish this message.

			if f.spvChain != nil {
				spvwallet.ProcessReorg(f.spvChain.TxStore, uint32(rpcLast.Height))
			}

			// Detach the last few blocks.
			for i := len(swappedBlocks) - 1; i >= 0; i-- {
				dequeue <- (chain.BlockDisconnected)(*swappedBlocks[i])
			}

			f.spvChain.Activate()
		} else {
			primary = rpcEnqueue
			secondary = spvEnqueue
			primaryLast = &rpcLast
			secondaryLast = &spvLast

			f.spvChain.Deactivate()
		}

		// reset saved notifications.
		rpcLast = nil
		spvLast = nil
		last = nil

		pastNotifications = make(map[wtxmgr.Block][]chain.Notification)
		secondaryBlocks = nil
		secondaryNotifications = nil
	}

	for {
		select {

		// If we hear from the primary channel, we want to analyze
		case n := <-primary:
			// If the channel has been closed, we need to set
			// the secondary as the primary and then switch
			// to the next for loop.
			if n == nil {
				swap()
				break
			}

			updatePrimary(n)

		//
		case n := <-secondary:
			// If the channel is closed, then we want to just listen
			// to the primary from now on.
			if n == nil {
				break
			}

			// Save this notification in case we have to replay it later.
			switch v := n.(type) {
			case chain.BlockConnected:
				*secondaryLast = &v

				// If this is a BlockConnected message that we haven't
				// seen before, save it.
				if last == nil || last.Height < v.Height {
					last = &v
					secondaryBlocks = append(secondaryBlocks, &v)
					pastNotifications[v.Block] = secondaryNotifications
				}

				secondaryNotifications = nil

			case chain.BlockDisconnected:
				*secondaryLast = nil
				secondaryBlocks = nil
				secondaryNotifications = nil
				pastNotifications = make(map[wtxmgr.Block][]chain.Notification)

			// We save all other notifications
			default:
				secondaryNotifications = append(secondaryNotifications, n)
			}

			if len(secondaryBlocks) >= int(swapHeight) {
				swap()
			}

		case <-f.quit:
			return
		}
	}

	// If one of the channels has closed, then there is only one left
	// for us to listen to and nothing to analyze.
	for {
		select {
		case n := <-primary:
			if n == nil {
				f.Stop()
				break
			}

			// All notifications are passed along to the wallet.
			dequeue <- n
		case <-f.quit:
			return
		}
	}
}

// handler maintains a queue of notifications and the current state (best
// block) of the chain.
func (f *fallback) outHandler(fenqueue <-chan chain.Notification,
	fdequeue chan<- chain.Notification, quit <-chan struct{}) error {
	defer func() {
		close(fdequeue)
		f.wg.Done()
	}()

	var notifications []chain.Notification
	enqueue := fenqueue
	var next chain.Notification
	var dequeue chan<- chain.Notification

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

			if dequeue == nil {
				next = n
				// Enable the sending of notifications.
				dequeue = fdequeue
			} else {
				notifications = append(notifications, n)
			}

		case dequeue <- next:
			if len(notifications) != 0 {
				next = notifications[0]
				notifications[0] = nil
				notifications = notifications[1:]
			} else if enqueue == nil {
				return nil
			} else {
				// If no more notifications can be enqueued, the
				// queue is finished.
				dequeue = nil
				notifications = nil
			}

		case <-quit:
			return nil
		}
	}
}

// SendRawTransaction submits the encoded transaction to the server which will
// then relay it to the network.
func (f *fallback) SendRawTransaction(tx *wire.MsgTx, alloHighFees bool) (*chainhash.Hash, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.SendRawTransaction(tx, alloHighFees)
}

// BlockStamp returns the latest block notified by the client, or an error
// if the client has been shut down.
//
// TODO
func (f *fallback) BlockStamp() (*waddrmgr.BlockStamp, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.BlockStamp()
}

// GetBlock returns a raw block from the server given its hash.
//
// See GetBlockVerbose to retrieve a data structure with information about the
// block instead.
func (f *fallback) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetBlock(blockHash)
}

// GetBestBlock returns the hash and height of the block in the longest (best)
// chain.
//
// NOTE: This is a btcd extension.
//
// TODO
func (f *fallback) GetBestBlock() (*chainhash.Hash, int32, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetBestBlock()
}

// GetBlockVerbose returns a data structure from the server with information
// about a block given its hash.
//
// See GetBlockVerboseTx to retrieve transaction data structures as well.
// See GetBlock to retrieve a raw block instead.
func (f *fallback) GetBlockVerbose(blockHash *chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetBlockVerbose(blockHash)
}

// Notifications returns a channel of parsed notifications sent by the remote
// bitcoin RPC server.  This channel must be continually read or the process
// may abort for running out memory, as unread notifications are queued for
// later reads.
//
// TODO
func (f *fallback) Notifications() <-chan chain.Notification {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.Notifications()
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
func (f *fallback) NotifyBlocks() error {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.NotifyBlocks()
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
func (f *fallback) NotifyReceived(addresses []btcutil.Address) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.NotifyReceived(addresses)
}

func (f *fallback) GetBlockVerboseAsync(blockHash *chainhash.Hash) rpcclient.FutureGetBlockVerboseResult {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetBlockVerboseAsync(blockHash)
}

func (f *fallback) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetBlockHash(blockHeight)
}

func (f *fallback) GetInfo() (*btcjson.InfoWalletResult, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetInfo()
}

func (f *fallback) GetTxOutAsync(txHash *chainhash.Hash, index uint32, mempool bool) rpcclient.FutureGetTxOutResult {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.GetTxOutAsync(txHash, index, mempool)
}

func (f *fallback) POSTClient() (*rpcclient.Client, error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.POSTClient()
}

func (f *fallback) Rescan(startBlock *chainhash.Hash, addresses []btcutil.Address, outpoints []*wire.OutPoint) error {
	f.mtx.Lock()
	defer f.mtx.Unlock()

	return f.primary.Rescan(startBlock, addresses, outpoints)
}
