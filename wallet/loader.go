// Copyright (c) 2015-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"errors"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/internal/prompt"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
)

var (
	// ErrLoaded describes the error condition of attempting to load or
	// create a wallet when the loader has already done so.
	ErrLoaded = errors.New("wallet already loaded")

	// ErrNotLoaded describes the error condition of attempting to close a
	// loaded wallet when a wallet has not been loaded.
	ErrNotLoaded = errors.New("wallet is not loaded")

	// ErrExists describes the error condition of attempting to create a new
	// wallet when one exists already.
	ErrExists = errors.New("wallet already exists")

	ErrErrorState = errors.New("Loader has entered an error state.")

	ErrRunning = errors.New("Loader is running.")

	ErrNotRunning = errors.New("Loader is not running.")
)

type LoaderState uint32

const (
	LoaderError   LoaderState = LoaderState(0)
	LoaderEmpty   LoaderState = LoaderState(1)
	LoaderLoaded  LoaderState = LoaderState(2)
	LoaderRunning LoaderState = LoaderState(3)
)

// Loader implements the creating of new and opening of existing wallets, while
// providing a callback system for other subsystems to handle the loading of a
// wallet.  This is primarely intended for use by the RPC servers, to enable
// methods and services which require the wallet when the wallet is loaded by
// another subsystem.
//
// Loader is safe for concurrent access.
type Loader struct {
	mu          sync.Mutex
	state       LoaderState
	chainParams *chaincfg.Params
	db          walletdb.DB
	wallet      *Wallet
	session     *Session
}

// NewLoader constructs a Loader.
func NewLoader(chainParams *chaincfg.Params, db walletdb.DB) *Loader {
	return &Loader{
		state:       LoaderEmpty,
		chainParams: chainParams,
		db:          db,
	}
}

// CreateNewWallet creates a new wallet using the provided public and private
// passphrases.  The seed is optional.  If non-nil, addresses are derived from
// this seed.  If nil, a secure random seed is generated.
func (l *Loader) CreateNewWallet(pubPassphrase, privPassphrase, seed []byte) (LoaderState, error) {
	defer l.mu.Unlock()
	l.mu.Lock()

	switch l.state {
	case LoaderError:
		return LoaderError, ErrErrorState
	case LoaderRunning:
		return LoaderRunning, ErrRunning
	case LoaderLoaded:
		return LoaderLoaded, ErrLoaded
	}

	// Initialize the newly created database for the wallet before opening.
	err := Create(l.db, pubPassphrase, privPassphrase, seed, l.chainParams)
	if err != nil {
		return l.state, err
	}

	// Open the newly-created wallet.
	w, err := Open(l.db, pubPassphrase, nil, l.chainParams)
	if err != nil {
		return l.state, err
	}
	l.wallet = w
	l.state = LoaderLoaded
	return LoaderLoaded, nil
}

var errNoConsole = errors.New("db upgrade requires console access for additional input")

func noConsole() ([]byte, error) {
	return nil, errNoConsole
}

func defaultPassword() ([]byte, error) {
	return []byte(InsecurePrivPassphrase), nil
}

// OpenExistingWallet opens the wallet from the loader's wallet database path
// and the public passphrase.  If the loader is being called by a context where
// standard input prompts may be used during wallet upgrades, setting
// canConsolePrompt will enables these prompts.
func (l *Loader) OpenExistingWallet(pubPassphrase []byte, canConsolePrompt bool) (LoaderState, error) {
	defer l.mu.Unlock()
	l.mu.Lock()

	switch l.state {
	case LoaderError:
		return LoaderError, ErrErrorState
	case LoaderRunning:
		return LoaderRunning, ErrRunning
	case LoaderLoaded:
		return LoaderLoaded, ErrLoaded
	}

	var cbs *waddrmgr.OpenCallbacks
	if canConsolePrompt {
		cbs = &waddrmgr.OpenCallbacks{
			ObtainSeed:        prompt.ProvideSeed,
			ObtainPrivatePass: prompt.ProvidePrivPassphrase,
		}
	} else {
		cbs = &waddrmgr.OpenCallbacks{
			ObtainSeed:        noConsole,
			ObtainPrivatePass: defaultPassword,
		}
	}
	w, err := Open(l.db, pubPassphrase, cbs, l.chainParams)
	if err != nil {
		return l.state, err
	}
	l.wallet = w
	l.state = LoaderLoaded
	return LoaderLoaded, nil
}

// LoadedWallet returns the loaded wallet, if any, and an error if no
// wallet was found.
func (l *Loader) LoadedWallet() (*Wallet, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case LoaderError:
		return nil, ErrErrorState
	case LoaderEmpty:
		return nil, ErrNotLoaded
	default:
		return l.wallet, nil
	}
}

func (l *Loader) Session(client chain.Client, lifecycle func(*Session) error) (LoaderState, error) {
	defer l.mu.Unlock()
	l.mu.Lock()

	switch l.state {
	case LoaderError:
		return LoaderError, ErrErrorState
	case LoaderRunning:
		return LoaderRunning, ErrRunning
	case LoaderEmpty:
		return LoaderEmpty, ErrNotLoaded
	}

	connected := make(chan struct{})

	go func() {
		err := l.wallet.synchronizeChain(client, func(s *Session) error {
			// Because func Session does not close until chan connected is
			// closed, we know that the lock is still locked.
			l.session = s

			l.state = LoaderRunning

			// By the time this is run, the wallet has started synchronizing.
			// Now the Session function can close and the wallet's normal
			// lifecycle can begin.
			connected <- struct{}{}

			err := lifecycle(s)

			// The session has stopped, so we need to change the state
			// of the Loader.
			l.mu.Lock()
			if err != nil {
				l.state = LoaderError
			} else {
				l.state = LoaderLoaded
			}
			l.mu.Unlock()

			return err
		})

		if err != nil {
			log.Errorf("Session crashed: %s", err)
		}

		// close the channel again in case anything went wrong when
		// we tried to sync the wallet. In other words, if an error
		// was returned by synchronizeChain before before lifecycle even
		// started running.
		close(connected)
	}()

	//
	<-connected
	return l.state, nil
}

// UnloadWallet stops the loaded wallet, if any, and closes the wallet database.
// This returns ErrNotLoaded if the wallet has not been loaded with
// CreateNewWallet or LoadExistingWallet.  The Loader may be reused if this
// function returns without error.
func (l *Loader) UnloadWallet() error {
	defer l.mu.Unlock()
	l.mu.Lock()

	if l.wallet == nil {
		return ErrNotLoaded
	}

	if l.session != nil {
		l.session.Stop()
		l.wallet.WaitForShutdown()
	}
	err := l.db.Close()
	if err != nil {
		return err
	}

	l.wallet = nil
	l.db = nil
	return nil
}
