// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package legacyrpc

import (
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/wallet"
)

const (
	notificationAmount = 10000 // 10000 satoshis.
	minConf            = 6
)

func bip47TranslateError(err error) error {
	return &btcjson.RPCError{
		Code:    -1,
		Message: err.Error(),
	}
}

// bip47Notify handles BIP 47 notification transaction commands.
func bip47Notify(icmd interface{}, s *wallet.Session) (interface{}, error) {
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

	// Create transaction and publish it.
	authored, err := s.CreateBip47NotificationTx(cmd.Account, alice, bob, notificationAmount, minConf)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	err = s.PublishTransaction(authored.Tx)
	if err != nil {
		return nil, bip47TranslateError(err)
	}

	return authored.Tx.TxHash().String(), nil
}
