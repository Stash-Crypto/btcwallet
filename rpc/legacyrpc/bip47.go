// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package legacyrpc

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

const (
	notificationAmount = 10000 // 10000 satoshis.
)

func bip47TranslateError(err error) error {
	return &btcjson.RPCError{
		Code:    -1,
		Message: err.Error(),
	}
}

// bip47Notify handles BIP 47 notification transaction commands.
func bip47Notify(icmd interface{}, w *wallet.Wallet) (interface{}, error) {
	cmd := icmd.(*btcjson.Bip47NotifyCmd)

	// Create payment code for Alice.
	alice, err := hdkeychain.ReadPaymentCode(cmd.Alice)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	// Create payment code for bob.
	bob, err := hdkeychain.ReadPaymentCode(cmd.Bob)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	// We can generate a change address automatically if this
	// is not the imported account.
	var automaticChange bool = cmd.Account != waddrmgr.ImportedAddrAccount

	redeem, _, changeScript, err := w.FundTransaction(
		cmd.Account, notificationAmount, 6, false,
		automaticChange,
		func(output wtxmgr.Credit) bool {
			return txscript.GetScriptClass(output.PkScript) == txscript.PubKeyHashTy
		})

	if err != nil {
		return nil, bip47TranslateError(err)
	}

	if len(redeem) < 1 {
		return nil, bip47TranslateError(errors.New("Insufficient funds."))
	}

	var from wtxmgr.Credit
	availableFunds := false
	for _, credit := range redeem {
		if credit.Amount >= 2*notificationAmount {
			from = credit
			availableFunds = true
			break
		}
	}
	if !availableFunds {
		return nil, bip47TranslateError(errors.New("No output with sufficient funds available to construct a notification transaction."))
	}

	// get script for first output
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(from.PkScript, w.ChainParams())
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	if len(addrs) != 1 {
		// This should not really happen.
		return nil, bip47TranslateError(errors.New("Could not process script."))
	}

	// Generate bob's notification address.
	bobNotify, err := bob.NotificationAddress(w.ChainParams())
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	// Create the list of outputs.
	outputs := make([]*wire.TxOut, 0, 3)

	// Create a script to pubkey hash for the output.
	payment, err := txscript.PayToAddrScript(bobNotify)
	if err != nil {
		return nil, bip47TranslateError(err)
	}
	outputs = append(outputs, &wire.TxOut{
		Value:    notificationAmount,
		PkScript: payment,
	})

	// Send the change back to myself.
	// TODO get rid of this part.
	if !automaticChange {
		changeAddr, _ := btcutil.DecodeAddress("1H4YBXGabz7GMNsdAijkxQPS1ftmViLjvh", w.ChainParams())
		changeScript, err = txscript.PayToAddrScript(changeAddr)
		if err != nil {
			return nil, bip47TranslateError(err)
		}
	}
	outputs = append(outputs, &wire.TxOut{
		Value:    int64(from.Amount) - 2*notificationAmount,
		PkScript: changeScript,
	})

	// Create op_return data.
	key, err := w.DumpWIFPrivateKey(addrs[0])
	if err != nil {
		return nil, bip47TranslateError(err)
	}
	wif, err := btcutil.DecodeWIF(key)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	// Create op_return output.
	var buf bytes.Buffer
	from.Serialize(&buf)
	code, err := hdkeychain.NotificationCode(alice, bob, wif.PrivKey, buf.Bytes(), w.ChainParams())
	if err != nil {
		return nil, bip47TranslateError(err)
	}
	data, err := txscript.NullDataScript(code)
	if err != nil {
		return nil, bip47TranslateError(err)
	}
	outputs = append(outputs, &wire.TxOut{
		Value:    0,
		PkScript: data,
	})

	// Sign and publish transaction.
	authored, err := w.CreateTx(cmd.Account, outputs, redeem)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	err = w.PublishTransaction(authored.Tx)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	return authored.Tx.TxHash().String(), nil
}
