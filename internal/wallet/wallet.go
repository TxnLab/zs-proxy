/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package wallet is the zs-proxy CLI's wallet: the SDK's go/wallet in the
// proxy's state dir, with the CLI's backend choices. A release build stamps
// build.KeyringBackend ("os"); ZEROSIGNAL_KEYRING_BACKEND overrides it, even
// for a wallet that records another backend; the encrypted file's passphrase
// comes from ZEROSIGNAL_KEYSTORE_PASSPHRASE or a terminal prompt.
package wallet

import (
	"cmp"
	"errors"
	"fmt"

	sdk "github.com/TxnLab/zerosignal/go/wallet"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

type (
	Wallet   = sdk.Wallet
	Metadata = sdk.Metadata
)

const (
	KindXHD24      = sdk.KindXHD24
	KindAlgorand25 = sdk.KindAlgorand25
)

// ErrExists is the SDK's, with the flag that replaces a wallet.
var ErrExists = fmt.Errorf("%w (use --force to replace)", sdk.ErrExists)

// ErrNotFound is the SDK's: no phrase where one was expected.
var ErrNotFound = sdk.ErrNotFound

// IsExists reports whether err says a wallet is already there, from the SDK
// or as ErrExists.
func IsExists(err error) bool { return errors.Is(err, sdk.ErrExists) }

// WithForceHint adds the flag that replaces a wallet to an SDK ErrExists, as
// Generate and Import return it. A phrase the store holds with no wallet.json
// gets the way back to it as well, since --force alone would replace it.
func WithForceHint(err error) error {
	switch {
	case !errors.Is(err, sdk.ErrExists) || errors.Is(err, ErrExists):
		return err
	case err == sdk.ErrExists:
		return fmt.Errorf("%w (use --force to replace)", err)
	default:
		return fmt.Errorf("%w. If it is your phrase, `zs-proxy wallet import --force` with it restores the wallet; "+
			"any other --force replaces it", err)
	}
}

// DefaultMetadata reads the default wallet's descriptor without opening its
// store, so showing a wallet works even where its recorded backend does not
// open.
func DefaultMetadata() (Metadata, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return Metadata{}, err
	}
	return sdk.ReadMetadata(dir)
}

// DeriveAddress validates a recovery phrase and returns its address and kind.
func DeriveAddress(mnemonic string) (addr, kind string, err error) {
	return sdk.DeriveAddress(mnemonic)
}

// DeriveSolanaAddress validates a recovery phrase and returns the address it
// pays as on the Solana rail; a 25-word Algorand mnemonic has none.
func DeriveSolanaAddress(mnemonic string) (string, error) {
	return sdk.DeriveSolanaAddress(mnemonic)
}

// ErrNoSolanaKey is the SDK's ErrWalletNoSolanaKey with the commands that
// back up and replace the wallet. Replacing it destroys the only copy of the
// Algorand account's phrase, which may hold funds, so the backup comes first.
var ErrNoSolanaKey = fmt.Errorf("%w. "+
	"Back it up first with `zs-proxy wallet export`: replacing the wallet deletes the phrase, and with it the Algorand account. "+
	"Then `zs-proxy wallet new --force` generates a 24-word phrase, or `zs-proxy wallet import --force` takes one; "+
	"or leave the wallet and set a 24-word phrase in a *_MNEMONIC environment variable", sdk.ErrWalletNoSolanaKey)

// PayerOptions is how the SDK's key loaders find this wallet: in the state
// dir, on the CLI's backend.
func PayerOptions() (sdk.PayerOptions, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return sdk.PayerOptions{}, err
	}
	return sdk.PayerOptions{Dir: dir, Backend: build.KeyringBackend}, nil
}

// SolanaAddress is the default wallet's Solana address: the one meta
// records, else the one unlocking the store records, which a wallet written
// before the address was recorded needs once.
func SolanaAddress(meta Metadata) (string, error) {
	switch {
	case meta.SolanaAddress != "":
		return meta.SolanaAddress, nil
	case meta.Kind == KindAlgorand25:
		return "", ErrNoSolanaKey
	}
	w, err := OpenDefault()
	if err != nil {
		return "", fmt.Errorf("open wallet: %w", err)
	}
	// Reading the phrase records its address, derived from the phrase as
	// stored, as the keystore takes it; deriving here from Export's copy
	// could name a key the keystore wouldn't load.
	if _, err := w.Export(); err != nil {
		return "", err
	}
	m, err := DefaultMetadata()
	if err != nil {
		return "", err
	}
	if m.SolanaAddress == "" {
		return "", errors.New("the wallet recorded no Solana address")
	}
	return m.SolanaAddress, nil
}

func options() sdk.KeyringOptions {
	return sdk.KeyringOptions{
		Backend:    build.KeyringBackend,
		Override:   sdk.OverrideFromEnv(),
		Passphrase: sdk.EnvOrPromptPassphrase,
	}
}

// OpenDefault opens the wallet in the proxy's state dir. It does not prompt;
// the first read or write may.
func OpenDefault() (*Wallet, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	return sdk.Open(dir, options())
}

// DefaultExists reports whether the state dir holds a wallet, without opening
// its store, so it never prompts.
func DefaultExists() bool {
	dir, err := paths.StateDir()
	if err != nil {
		return false
	}
	return sdk.ExistsIn(dir)
}

// FileBackendNeedsPassphrase reports whether unlocking the wallet will prompt
// on the terminal. The daemonizer prompts once before detaching, since the
// detached child has no terminal.
func FileBackendNeedsPassphrase() bool {
	return sdk.NeedsPassphrasePrompt(defaultBackend(true))
}

// UsesFileBackend reports whether a service would find the wallet on the
// encrypted file rather than the OS keychain. install-service asks it to
// decide whether a service needs ZEROSIGNAL_KEYSTORE_PASSPHRASE wired in. It
// ignores the installing shell's environment — both the passphrase and
// ZEROSIGNAL_KEYRING_BACKEND — because the service does not inherit it.
func UsesFileBackend() bool {
	return sdk.UsesFileBackend(defaultBackend(false))
}

// defaultBackend is the backend the default wallet opens on: the recorded one
// unless an honored override names another, else what the override or the
// build's backend resolves to on this machine — "os" is the keychain or, where
// none opens, the encrypted file. Only resolving opens a store; when it will
// not open, the configured name stands in.
func defaultBackend(honorOverride bool) string {
	o := options()
	if !honorOverride {
		o.Override = ""
	}
	if o.Override == "" {
		if m, err := DefaultMetadata(); err == nil && m.Backend != "" {
			return m.Backend
		}
	}
	if dir, err := paths.StateDir(); err == nil {
		if w, err := sdk.Open(dir, o); err == nil {
			return w.Backend()
		}
	}
	return cmp.Or(o.Override, o.Backend)
}
