package spv

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/walletdb"
)

var (
	spvNamespace = []byte("spv")
)

type Headers struct {
	lock  sync.Mutex
	best  *spvwallet.StoredHeader
	chain map[uint32]*chainhash.Hash
	db    walletdb.Namespace

	notifications chan<- chain.Notification
}

func NewHeaders(db walletdb.DB) (spvwallet.Headers, error) {
	// Get the spv namespace.
	namespace, err := db.Namespace(spvNamespace)
	if err != nil {
		return nil, err
	}

	h := &Headers{
		db: namespace,
	}

	empty, err := walletdb.NamespaceIsEmpty(namespace)
	if err != nil {
		return nil, err
	}
	if empty {
		err = namespace.Update(func(tx walletdb.Tx) error {
			root := tx.RootBucket()
			_, err := root.CreateBucketIfNotExists(spvwallet.BKTHeaders)
			if err != nil {
				return err
			}
			_, err = root.CreateBucketIfNotExists(spvwallet.BKTChainTip)
			if err != nil {
				return err
			}
			return nil
		})
	} else {
		err = namespace.View(func(tx walletdb.Tx) error {
			tip := tx.RootBucket().Bucket(spvwallet.BKTChainTip)
			b := tip.Get(spvwallet.KEYChainTip)
			if b == nil {
				return errors.New("ChainTip not set")
			}
			best, err := spvwallet.DeserializeHeader(b)
			if err != nil {
				return err
			}
			h.best = &best
			hash := best.Header.BlockHash()
			q := &hash

			// Populate the chain by height.
			sh := best
			headers := tx.RootBucket().Bucket(spvwallet.BKTHeaders)
			for {
				h.chain[sh.Height] = q
				prev := sh.Header.PrevBlock
				q = &prev

				b := headers.Get(prev[:])
				if b == nil {
					return nil
				}

				sh, err = spvwallet.DeserializeHeader(b)
				if err != nil {
					return err
				}
			}
		})
	}
	if err != nil {
		return nil, err
	}

	// If the namespace is empty, set it up for storing the block headers.
	return h, nil
}

var ErrHeaderNotFound = errors.New("Header does not exist in database")

func get(bucket walletdb.Bucket, hash *chainhash.Hash) (spvwallet.StoredHeader, error) {
	b := bucket.Get(hash[:])
	if b == nil {
		return spvwallet.StoredHeader{}, ErrHeaderNotFound
	}
	return spvwallet.DeserializeHeader(b)
}

func reorganizeChain(headers walletdb.Bucket, chain map[uint32]*chainhash.Hash, sh spvwallet.StoredHeader) error {
	if sh.Height == 0 {
		return nil
	}

	// get the previous header.
	prev, err := get(headers, &sh.Header.PrevBlock)
	if err != nil {
		if err == ErrHeaderNotFound {
			return nil
		} else {
			return err
		}
	}

	phash, exists := chain[prev.Height]
	if !exists {
		return errors.New("Hash not found")
	}

	bh := prev.Header.BlockHash()
	if *phash == bh {
		return nil
	}

	chain[prev.Height] = &bh

	return reorganizeChain(headers, chain, prev)
}

// Put puts a block header into the database.
// Total work and height are required to be calculated prior to insertion
// If this is the new best header, the chain tip should also be updated
func (h *Headers) Put(sh spvwallet.StoredHeader, newBestHeader bool) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	return h.db.Update(func(tx walletdb.Tx) error {
		hdrs := tx.RootBucket().Bucket(spvwallet.BKTHeaders)
		ser, err := spvwallet.SerializeHeader(sh)
		if err != nil {
			return err
		}
		hash := sh.Header.BlockHash()
		err = hdrs.Put(hash.CloneBytes(), ser)
		if err != nil {
			return err
		}

		h.chain[sh.Height] = &hash

		if !newBestHeader {
			return nil
		}

		best := h.best
		h.best = &sh

		if best != nil && best.Header.BlockHash() != sh.Header.PrevBlock {
			return reorganizeChain(hdrs, h.chain, sh)
		}
		return nil
	})
}

// GetHeader gets a header from a hash.
func (h *Headers) GetHeader(hash chainhash.Hash) (spvwallet.StoredHeader, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var sh spvwallet.StoredHeader
	var err error
	err = h.db.View(func(tx walletdb.Tx) error {
		hdrs := tx.RootBucket().Bucket(spvwallet.BKTHeaders)
		sh, err = get(hdrs, &hash)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return spvwallet.StoredHeader{}, err
	}
	return sh, nil
}

// GetPreviousHeader returns all information about the previous header
func (h *Headers) GetPreviousHeader(header wire.BlockHeader) (spvwallet.StoredHeader, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var sh spvwallet.StoredHeader
	var err error
	err = h.db.View(func(tx walletdb.Tx) error {
		hdrs := tx.RootBucket().Bucket(spvwallet.BKTHeaders)
		sh, err = get(hdrs, &header.PrevBlock)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return spvwallet.StoredHeader{}, err
	}
	return sh, nil
}

// Retreive the best header from the database
func (h *Headers) GetBestHeader() (spvwallet.StoredHeader, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.best == nil {
		return spvwallet.StoredHeader{}, errors.New("No best header known")
	}

	return *h.best, nil
}

// Get the height of chain
func (h *Headers) Height() (uint32, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.best == nil {
		return 0, errors.New("No best header known")
	}

	return h.best.Height, nil
}

// GetBlockHash returns the hash of a block at a given height.
func (h *Headers) GetBlockHash(height int64) (*chainhash.Hash, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	hash, exists := h.chain[uint32(height)]
	if !exists {
		return nil, fmt.Errorf("Height %d not found", height)
	}
	return hash, nil
}

// GetBlockAtHeight returns information about a block given a height.
func (h *Headers) GetBlockAtHeight(height uint32) (spvwallet.StoredHeader, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	hash, exists := h.chain[height]
	if !exists {
		return spvwallet.StoredHeader{}, errors.New("Not found")
	}

	var sh spvwallet.StoredHeader
	err := h.db.View(func(tx walletdb.Tx) error {
		b := tx.RootBucket().Bucket(spvwallet.BKTHeaders).Get(hash[:])
		if b == nil {
			return errors.New("Not Found")
		}
		var err error
		sh, err = spvwallet.DeserializeHeader(b)
		return err
	})
	if err != nil {
		return spvwallet.StoredHeader{}, err
	}

	return sh, nil
}

func (h *Headers) Print(w io.Writer) {
	// I refuse to write this function. It's too stupid. -- Daniel
}

// Close cleanly closes the db
func (h *Headers) Close() error {
	h.lock.Lock()
	defer h.lock.Unlock()

	if h.best == nil {
		return nil
	}

	ser, err := spvwallet.SerializeHeader(*h.best)
	if err != nil {
		return err
	}

	// Write best to the db.
	return h.db.Update(func(tx walletdb.Tx) error {
		tip := tx.RootBucket().Bucket(spvwallet.BKTChainTip)
		err = tip.Put(spvwallet.KEYChainTip, ser)
		if err != nil {
			return err
		}

		return nil
	})
}
