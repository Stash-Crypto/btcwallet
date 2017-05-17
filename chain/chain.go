// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

type Client interface {
	// SendRawTransaction submits the encoded transaction to the server which will
	// then relay it to the network.
	SendRawTransaction(tx *wire.MsgTx, alloHighFees bool) (*chainhash.Hash, error)

	// BlockStamp returns the latest block notified by the client, or an error
	// if the client has been shut down.
	BlockStamp() (*waddrmgr.BlockStamp, error)

	// GetBestBlock returns the hash and height of the block in the longest (best)
	// chain.
	GetBestBlock() (*chainhash.Hash, int32, error)

	// GetBlock returns a raw block from the server given its hash.
	//
	// See GetBlockVerbose to retrieve a data structure with information about the
	// block instead.
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)

	// GetBlockHash returns the hash of the block in the best block chain at the
	// given height.
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)

	// GetBlockVerbose returns a data structure from the server with information
	// about a block given its hash.
	//
	// See GetBlockVerboseTx to retrieve transaction data structures as well.
	// See GetBlock to retrieve a raw block instead.
	GetBlockVerbose(blockHash *chainhash.Hash) (*btcjson.GetBlockVerboseResult, error)

	// Rescan rescans the block chain starting from the provided starting block to
	// the end of the longest chain for transactions that pay to the passed
	// addresses and transactions which spend the passed outpoints.
	//
	// The notifications of found transactions are delivered to the notification
	// handlers associated with client and this call will not return until the
	// rescan has completed.  Calling this function has no effect if there are no
	// notification handlers and will result in an error if the client is configured
	// to run in HTTP POST mode.
	//
	// The notifications delivered as a result of this call will be via one of
	// OnRedeemingTx (for transactions which spend from the one of the
	// passed outpoints), OnRecvTx (for transactions that receive funds
	// to one of the passed addresses), and OnRescanProgress (for rescan progress
	// updates).
	//
	// See RescanEndBlock to also specify an ending block to finish the rescan
	// without continuing through the best block on the main chain.
	//
	// NOTE: Rescan requests are not issued on client reconnect and must be
	// performed manually (ideally with a new start height based on the last
	// rescan progress notification).  See the OnClientConnected notification
	// callback for a good callsite to reissue rescan requests on connect and
	// reconnect.
	//
	// NOTE: This is a btcd extension and requires a websocket connection.
	//
	// NOTE: Deprecated. Use RescanBlocks instead.
	Rescan(startBlock *chainhash.Hash,
		addresses []btcutil.Address,
		outpoints []*wire.OutPoint) error

	NotifyBlocks() error
	NotifyReceived(addresses []btcutil.Address) error

	// Notifications returns a channel of parsed notifications sent by the remote
	// bitcoin RPC server.  This channel must be continually read or the process
	// may abort for running out memory, as unread notifications are queued for
	// later reads.
	Notifications() <-chan Notification

	// These functions I think should be removable from this interface.
	GetInfo() (*btcjson.InfoWalletResult, error)
	POSTClient() (*rpcclient.Client, error)

	// These functions I think should be removable from the program itself.
	GetBlockVerboseAsync(blockHash *chainhash.Hash) rpcclient.FutureGetBlockVerboseResult

	// This one can probably be replaced by the non-async version.
	GetTxOutAsync(txHash *chainhash.Hash, index uint32, mempool bool) rpcclient.FutureGetTxOutResult
}

// Notification types.  These are defined here and processed from reading
// a notificationChan to avoid handling these notifications directly in
// rpcclient callbacks, which isn't very Go-like and doesn't allow
// blocking client calls.
type (
	// Notification is an abstract type representing any notification.
	// It contains an exported function that doesn't do anything to ensure
	// that only those types defined here can be notification types.
	Notification interface {
		isChainNotification()
	}

	// ClientConnected is a notification for when a client connection is
	// opened or reestablished to the chain server.
	ClientConnected struct{}

	// BlockConnected is a notification for a newly-attached block to the
	// best chain.
	BlockConnected wtxmgr.BlockMeta

	// BlockDisconnected is a notifcation that the block described by the
	// BlockStamp was reorganized out of the best chain.
	BlockDisconnected wtxmgr.BlockMeta

	// RelevantTx is a notification for a transaction which spends wallet
	// inputs or pays to a watched address.
	RelevantTx struct {
		TxRecord *wtxmgr.TxRecord
		Block    *wtxmgr.BlockMeta // nil if unmined
	}

	// RescanProgress is a notification describing the current status
	// of an in-progress rescan.
	RescanProgress struct {
		Hash   *chainhash.Hash
		Height int32
		Time   time.Time
	}

	// RescanFinished is a notification that a previous rescan request
	// has finished.
	RescanFinished struct {
		Hash   *chainhash.Hash
		Height int32
		Time   time.Time
	}
)

// Define isChanNotification for each notification type.
func (x ClientConnected) isChainNotification()   {}
func (x BlockConnected) isChainNotification()    {}
func (x BlockDisconnected) isChainNotification() {}
func (x RelevantTx) isChainNotification()        {}
func (x RescanProgress) isChainNotification()    {}
func (x RescanFinished) isChainNotification()    {}
