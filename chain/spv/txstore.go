package spv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

var (
	metaBucket     = []byte("txmetabucket")
	errNoMetaFound = errors.New("No meta found")
)

// TxStore is an implementation of spvwallet.TxStore that relies
// on btcwallet's database.
type TxStore struct {
	// The account numbers associated with this TxStore
	accounts []uint32

	// The address database.
	wmgr *waddrmgr.Manager

	// The transaction database.
	txStore *wtxmgr.Store

	// SPV headers database.
	headers *Headers

	// Database in which to store transaction metadata.
	db walletdb.Namespace

	// chain parameters for the wallet.
	param *chaincfg.Params

	notifyRec     map[string]struct{}
	notifications chan<- chain.Notification
}

var _ spvwallet.TxStore = (*TxStore)(nil)

type txMeta struct {
	id     string
	value  int64
	dead   bool
	height uint32
	watch  bool
}

func btob(b bool) byte {
	if b {
		return 1
	}

	return 0
}

func newTxStore(accounts []uint32,
	headers *Headers,
	db walletdb.DB,
	addrmgr *waddrmgr.Manager,
	txs *wtxmgr.Store,
	param *chaincfg.Params, notifications chan<- chain.Notification) (*TxStore, error) {
	namespace, err := db.Namespace(spvNamespace)
	if err != nil {
		return nil, err
	}

	err = namespace.Update(func(tx walletdb.Tx) error {
		_, err := tx.RootBucket().CreateBucketIfNotExists(metaBucket)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &TxStore{
		accounts:      accounts,
		wmgr:          addrmgr,
		txStore:       txs,
		headers:       headers,
		db:            namespace,
		param:         param,
		notifications: notifications,
	}, nil
}

func (tm *txMeta) Encode(w io.Writer) error {
	err := binary.Write(w, binary.BigEndian, uint32(len(tm.id)))
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(tm.id))
	if err != nil {
		return err
	}
	err = binary.Write(w, binary.BigEndian, tm.value)
	if err != nil {
		return err
	}
	err = binary.Write(w, binary.BigEndian, tm.height)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte{btob(tm.dead), btob(tm.watch)})
	return err
}

func (tm *txMeta) Decode(r io.Reader) error {
	var idlen uint32
	err := binary.Read(r, binary.BigEndian, &idlen)
	if err != nil {
		return err
	}
	id := make([]byte, idlen)
	_, err = r.Read(id)
	if err != nil {
		return err
	}
	tm.id = string(id)
	err = binary.Read(r, binary.BigEndian, &tm.value)
	if err != nil {
		return err
	}
	err = binary.Read(r, binary.BigEndian, &tm.height)
	if err != nil {
		return err
	}
	remainder := make([]byte, 2)
	_, err = r.Read(remainder)
	if err != nil {
		return err
	}
	tm.dead = remainder[0] != 0
	tm.watch = remainder[1] != 0
	return nil
}

func toSpvTxn(m *txMeta, r *wtxmgr.TxRecord) *spvwallet.Txn {
	var serialized []byte
	if r.SerializedTx == nil {
		buf := bytes.NewBuffer(make([]byte, 0, r.MsgTx.SerializeSize()))
		r.MsgTx.Serialize(buf)
		serialized = buf.Bytes()
	} else {
		serialized = r.SerializedTx
	}

	return &spvwallet.Txn{
		Txid:      m.id,
		Value:     m.value,
		Height:    int32(m.height),
		Timestamp: r.Received,
		WatchOnly: m.watch,
		Bytes:     serialized,
	}
}

func (tx *TxStore) getMeta(hash *chainhash.Hash) (*txMeta, error) {
	meta := &txMeta{}
	err := tx.db.View(func(t walletdb.Tx) error {
		mb := t.RootBucket().Bucket(metaBucket).Get(hash[:])
		if mb == nil {
			return errNoMetaFound
		}
		return meta.Decode(bytes.NewReader(mb))
	})
	if err != nil {
		return nil, err
	}
	return meta, nil
}

func (tx *TxStore) putMeta(hash *chainhash.Hash, m *txMeta) error {
	var b bytes.Buffer
	err := m.Encode(&b)
	if err != nil {
		return err
	}

	return tx.db.Update(func(t walletdb.Tx) error {
		t.RootBucket().Bucket(metaBucket).Put(hash[:], b.Bytes())
		return nil
	})
}

func (tx *TxStore) newTxMeta(txDetails *wtxmgr.TxDetails) *txMeta {
	tm := txMeta{
		id:     txDetails.TxRecord.Hash.String(),
		height: uint32(txDetails.Block.Block.Height),
	}

	for _, c := range txDetails.Credits {
		tm.value += int64(c.Amount)
	}

	// TODO do I use a plus or minus here?
	for _, d := range txDetails.Debits {
		tm.value -= int64(d.Amount)
	}

	return &tm
}

func (tx *TxStore) getMetaOrCreateNew(txRecord *wtxmgr.TxDetails) (*txMeta, error) {
	meta, err := tx.getMeta(&txRecord.Hash)
	if err == nil {
		return meta, nil
	}

	// If there is an error other than the record not being found,
	// return it.
	if err != errNoMetaFound {
		return nil, err
	}

	meta = tx.newTxMeta(txRecord)
	err = tx.putMeta(&txRecord.Hash, meta)
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// Ingest puts a tx into the DB atomically.  This can result in a
// gain, a loss, or no result.  Gain or loss in satoshis is returned.
func (tx *TxStore) Ingest(t *wire.MsgTx, height int32) (uint32, error) {
	// Create a TxRecord for this transaction.
	txRecord, err := wtxmgr.NewTxRecordFromMsgTx(t, time.Now())
	if err != nil {
		return 0, err
	}

	// Check whether the tx already exists.
	var details *wtxmgr.TxDetails
	details, err = tx.txStore.TxDetails(&txRecord.Hash)
	if err != nil {
		return 0, err
	}

	if details != nil {
		return uint32(len(details.Credits) + len(details.Debits)), nil
	}

	// Tx has been OK'd by SPV; check tx sanity
	utilTx := btcutil.NewTx(t) // convert for validation
	// Checks basic stuff like there are inputs and ouputs
	err = blockchain.CheckTransactionSanity(utilTx)
	if err != nil {
		return 0, err
	}

	now := time.Now()

	// Construct the BlockMeta object. The database requires this to
	// insert the tx.
	var blockMeta *wtxmgr.BlockMeta
	if height != 0 {
		if height >= 0 {
			header, err := tx.headers.GetBlockAtHeight(uint32(height))
			if err != nil {
				return 0, err
			}
			blockMeta = &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   header.Header.BlockHash(),
					Height: height,
				},
				Time: now,
			}
		}
	}

	// Check every output to determine whether it is controlled by a wallet
	// key.  If so, mark the output as a credit.
	credits := make([]wtxmgr.CreditRecord, 0)
	for i, output := range t.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(output.PkScript,
			tx.param)
		if err != nil {
			// Non-standard outputs are skipped.
			continue
		}
		for _, addr := range addrs {
			ma, err := tx.wmgr.Address(addr)
			if err == nil {
				credits = append(credits, wtxmgr.CreditRecord{
					Amount: btcutil.Amount(output.Value),
					Index:  uint32(i),
					Change: ma.Internal(),
				})
				continue
			}

			// Missing addresses are skipped.  Other errors should
			// be propagated.
			if !waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) {
				return 0, err
			}
		}
	}

	// Figure out the debits for this tx.
	debits, err := tx.txStore.UnregisteredTxDebits(t)
	if err != nil {
		return 0, err
	}

	// If there are no credits or debits with this tx, don't insert
	// it into the database.
	hits := uint32(len(debits) + len(credits))
	if hits == 0 {
		return 0, nil
	}

	// Insert into database.
	// TODO this system is screwed up. There just shouldn't be
	// notifications with SPV mode.
	tx.notifications <- chain.RelevantTx{
		TxRecord: txRecord,
		Block:    blockMeta,
	}

	return hits, nil
}

// GimmeFilter constructs the bloom filter.
func (tx *TxStore) GimmeFilter() (*bloom.Filter, error) {
	addresses := make([]btcutil.Address, 0)
	for _, account := range tx.accounts {
		err := tx.wmgr.ForEachActiveAccountAddress(account, func(maddr waddrmgr.ManagedAddress) error {
			addresses = append(addresses, maddr.Address())
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	outputs, err := tx.txStore.UnspentOutputs()
	if err != nil {
		return nil, err
	}

	// First make a bloom builder.
	f := bloom.NewFilter(uint32(len(outputs)+len(addresses)), 3, 0.0001, wire.BloomUpdateAll)

	for _, address := range addresses {
		f.Add(address.ScriptAddress())
	}

	for _, op := range outputs {
		f.AddOutPoint(&op.OutPoint)
		println("Adding outpoint ", op.String())
	}

	records, err := GetAllTransactions(tx.txStore)
	if err != nil {
		return nil, err
	}

	details, err := tx.getAllTxs(records, true)

	important := make([]spvwallet.Txn, 0)
	value := int64(0)
	for _, txn := range details {
		if txn.Value != 0 {
			important = append(important, txn)
			value += txn.Value
		}
	}

	return f, nil
}

// GetPendingInv returns an inv message containing all txs known to the
// db which are at height 0 (not known to be confirmed).
// This can be useful on startup or to rebroadcast unconfirmed txs.
func (tx *TxStore) GetPendingInv() (*wire.MsgInv, error) {
	pending, err := tx.txStore.UnminedTxRecords()
	if err != nil {
		return nil, err
	}

	inv := make([]*wire.InvVect, 0, len(pending))
	for _, p := range pending {
		inv = append(inv, wire.NewInvVect(wire.InvTypeTx, &p.Hash))
	}

	return &wire.MsgInv{
		InvList: inv,
	}, nil
}

// MarkAsDead marks a transaction as dead which has been broadcasted
// so long ago that it probably will never be mined.
func (tx *TxStore) MarkAsDead(txid chainhash.Hash) error {
	// Get the transaction from the wallet txmgr.
	txDetail, err := tx.txStore.TxDetails(&txid)
	if err != nil {
		return err
	}

	// Get the metadata.
	txMeta, err := tx.getMetaOrCreateNew(txDetail)
	if err != nil {
		return err
	}

	txMeta.dead = true
	return tx.putMeta(&txDetail.Hash, txMeta)
}

// GetTx fetch a raw tx and its metadata given a hash
func (tx *TxStore) GetTx(txid chainhash.Hash) (*wire.MsgTx, spvwallet.Txn, error) {
	// Get the transaction from the wallet txmgr.
	txDetails, err := tx.txStore.TxDetails(&txid)
	if err != nil {
		return nil, spvwallet.Txn{}, err
	}

	// Get the metadata.
	txMeta, err := tx.getMetaOrCreateNew(txDetails)
	if err != nil {
		return nil, spvwallet.Txn{}, nil
	}

	// Return the tx.
	return &txDetails.MsgTx, *toSpvTxn(txMeta, &txDetails.TxRecord), nil
}

// GetAllTransactions returns all transactions in the db.
func GetAllTransactions(s *wtxmgr.Store) (map[chainhash.Hash]*wtxmgr.TxDetails, error) {
	records := make(map[chainhash.Hash]*wtxmgr.TxDetails)
	// TODO is this how the ranges are really supposed to work?
	err := s.RangeTransactions(-1, 0, func(d []wtxmgr.TxDetails) (bool, error) {
		for _, d := range d {
			records[d.Hash] = &d
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (tx *TxStore) getAllTxs(records map[chainhash.Hash]*wtxmgr.TxDetails, includeWatchOnly bool) ([]spvwallet.Txn, error) {
	txns := make([]spvwallet.Txn, 0, len(records))
	for _, r := range records {
		meta, err := tx.getMetaOrCreateNew(r)
		if err != nil {
			return nil, err
		}

		if !meta.watch || includeWatchOnly {
			txns = append(txns, *toSpvTxn(meta, &r.TxRecord))
		}
	}

	return txns, nil
}

// GetAllTxs fetches all transactions from the db.
func (tx *TxStore) GetAllTxs(includeWatchOnly bool) ([]spvwallet.Txn, error) {
	records, err := GetAllTransactions(tx.txStore)
	if err != nil {
		return nil, err
	}

	return tx.getAllTxs(records, includeWatchOnly)
}

// notifyBlocks registers the client to receive notifications when blocks are
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
func (tx *TxStore) notifyBlocks() error {
	return ErrNotImplemented
}

// notifyReceived registers the client to receive notifications every time a
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
func (tx *TxStore) notifyReceived(addresses []btcutil.Address) error {
	return ErrNotImplemented
}
