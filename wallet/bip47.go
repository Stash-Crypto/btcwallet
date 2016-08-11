// Copyright (c) 2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

func (s *Session) CreateBip47NotificationTx(account uint32, alice,
	bob *hdkeychain.PaymentCode, amount int64, minconf int32) (*txauthor.AuthoredTx, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Address manager must be unlocked to compose transaction.  Grab
	// the unlock if possible (to prevent future unlocks), or return the
	// error if already locked.
	heldUnlock, err := s.Wallet.HoldUnlock()
	if err != nil {
		return nil, err
	}
	defer heldUnlock.Release()

	redeem, _, changeScript, err := s.fundTransaction(
		account, amount, minconf, false,
		true,
		func(output wtxmgr.Credit) bool {
			return txscript.GetScriptClass(output.PkScript) == txscript.PubKeyHashTy
		})
	if err != nil {
		return nil, err
	}

	if len(redeem) < 1 {
		return nil, errors.New("Insufficient funds.")
	}

	var from wtxmgr.Credit
	availableFunds := false
	for _, credit := range redeem {
		if int64(credit.Amount) >= 2*amount {
			from = credit
			availableFunds = true
			break
		}
	}
	if !availableFunds {
		return nil, errors.New("No output with sufficient funds available to construct a notification transaction.")
	}

	// get script for first output
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(from.PkScript, s.Wallet.ChainParams())
	if err != nil {
		return nil, err
	}

	if len(addrs) != 1 {
		// This should not really happen.
		return nil, errors.New("Could not process script.")
	}

	// Generate bob's notification address.
	bobNotify, err := bob.NotificationAddress(s.Wallet.ChainParams())
	if err != nil {
		return nil, err
	}

	// Create the list of outputs.
	outputs := make([]*wire.TxOut, 0, 3)

	// Create a script to pubkey hash for the output.
	payment, err := txscript.PayToAddrScript(bobNotify)
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, &wire.TxOut{
		Value:    amount,
		PkScript: payment,
	})

	outputs = append(outputs, &wire.TxOut{
		Value:    int64(from.Amount) - 2*amount,
		PkScript: changeScript,
	})

	// Create op_return data.
	key, err := s.Wallet.DumpWIFPrivateKey(addrs[0])
	if err != nil {
		return nil, err
	}
	wif, err := btcutil.DecodeWIF(key)
	if err != nil {
		return nil, err
	}

	// Create op_return output.
	var buf bytes.Buffer
	from.Serialize(&buf)
	code, err := hdkeychain.NotificationCode(alice, bob, wif.PrivKey, buf.Bytes(), s.Wallet.ChainParams())
	if err != nil {
		return nil, err
	}
	data, err := txscript.NullDataScript(code)
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, &wire.TxOut{
		Value:    0,
		PkScript: data,
	})

	// Sign and publish transaction.
	return s.createSimpleTx(makeInputSource(redeem), outputs, account)
}
