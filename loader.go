// Copyright (c) 2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"

	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

func fileExists(filePath string) (bool, error) {
	_, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func CreateDB(dbDirPath string) (walletdb.DB, error) {
	dbPath := filepath.Join(dbDirPath, walletDbName)
	exists, err := fileExists(dbPath)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, wallet.ErrExists
	}

	// Create the wallet database backed by bolt db.
	err = os.MkdirAll(dbDirPath, 0700)
	if err != nil {
		return nil, err
	}
	db, err := walletdb.Create("bdb", dbPath)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func LoadDB(dbDirPath string) (walletdb.DB, error) {
	// Ensure that the network directory exists.
	if err := checkCreateDir(dbDirPath); err != nil {
		return nil, err
	}

	// Open the database using the boltdb backend.
	dbPath := filepath.Join(dbDirPath, walletDbName)
	return walletdb.Open("bdb", dbPath)
}
