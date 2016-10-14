// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import(
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

var databaseCommentsKey = []byte("comments")

// comments manages the association of transactions and
// addresses to comments. 
type Comments struct{
	db walletdb.Namespace
}

// NewComments creates a new comments object and initialize
// the database if necessary. 
func NewComments(db walletdb.Namespace) Comments {
	// Create a bucket for comments in the database, if it
	// doesn't exist. 
	db.Update(func(dbtx walletdb.Tx) error {
		root := dbtx.RootBucket()
		bucket := root.Bucket(databaseCommentsKey)
		
		if bucket == nil {
			root.CreateBucket(databaseCommentsKey)
		}
		
		return nil
	})

	return Comments{db:db}
}

func (c Comments) GetAddressComment(address btcutil.Address) string {
	var comment string
	
	c.db.Update(func(dbtx walletdb.Tx) error {
		comments := dbtx.RootBucket().Bucket(databaseCommentsKey)
		
		cmt := comments.Get([]byte(address.EncodeAddress()))
		
		if cmt != nil {
			comment = string(cmt)
		}
		
		return nil
	}) 
	
	return comment
}

func (c Comments) GetTransactionComment(hash *chainhash.Hash) string {
	var comment string
	c.db.Update(func(dbtx walletdb.Tx) error {
		comments := dbtx.RootBucket().Bucket(databaseCommentsKey)
		
		cmt := comments.Get(hash[:])
		
		if cmt != nil {
			comment = string(cmt)
		}
		
		return nil
	}) 
	
	return comment
}

func (c Comments) SetAddressComment(address btcutil.Address, comment string) error {
	err := c.db.Update(func(dbtx walletdb.Tx) error {
		bucket := dbtx.RootBucket().Bucket(databaseCommentsKey)
		
		return bucket.Put([]byte(address.EncodeAddress()), []byte(comment))
	}) 
	
	if err != nil {
		return err
	}
	
	return nil
}

func (c Comments) SetTransactionComment(hash *chainhash.Hash, comment string) error {
	err := c.db.Update(func(dbtx walletdb.Tx) error {
		bucket := dbtx.RootBucket().Bucket(databaseCommentsKey)
		
		return bucket.Put(hash[:], []byte(comment))
	}) 
	
	if err != nil {
		return err
	}
	
	return nil 
}