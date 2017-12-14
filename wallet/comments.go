// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcwallet/walletdb"
)

var databaseCommentsKey = []byte("comments")

// comments manages the association of transactions and
// addresses to comments.
type Comments struct {
	comments map[chainhash.Hash]string
	update   map[*chainhash.Hash]struct{}
}

// NewComments creates a new comments object and initialize
// the database if necessary.
func NewComments(db walletdb.Namespace) Comments {
	comments := make(map[chainhash.Hash]string)

	db.Update(func(dbtx walletdb.Tx) error {
		root := dbtx.RootBucket()
		bucket := root.Bucket(databaseCommentsKey)

		// Create a bucket for comments in the database, if it
		// doesn't exist.
		if bucket == nil {
			root.CreateBucket(databaseCommentsKey)
			return nil
		}

		// If it does exist, go through it and grap all comments.
		err := bucket.ForEach(func(k, v []byte) error {
			if v != nil {
				hash, err := chainhash.NewHash(k)
				if err != nil {
					return err
				}

				comments[*hash] = string(v)
			}

			return nil
		})

		if err != nil {
			return err
		}

		return nil
	})

	return Comments{
		comments: comments,
		update:   make(map[*chainhash.Hash]struct{}),
	}
}

func (c Comments) GetTransactionComment(hash *chainhash.Hash) string {
	return c.comments[*hash]
}

func (c Comments) SetTransactionComment(hash *chainhash.Hash, comment string) {
	c.comments[*hash] = comment
	c.update[hash] = struct{}{}
}

func (c Comments) Update(db walletdb.Namespace) error {
	err := db.Update(func(dbtx walletdb.Tx) error {
		bucket := dbtx.RootBucket().Bucket(databaseCommentsKey)

		for hash, _ := range c.update {
			err := bucket.Put(hash[:], []byte(c.comments[*hash]))

			if err != nil {
				return err
			}
		}

		return nil
	})

	c.update = make(map[*chainhash.Hash]struct{})

	if err != nil {
		return err
	}

	return nil
}
