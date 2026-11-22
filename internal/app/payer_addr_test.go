/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/mnemonic"

	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/wallet"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// algoKeys is a keystore of n fresh Algorand accounts.
func algoKeys(t *testing.T, n int) *keystore.KeyStore {
	t.Helper()
	var providers []keystore.SecretProvider
	for range n {
		m, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		providers = append(providers, mnemonicProvider{m})
	}
	ks, err := keystore.New(context.Background(), providers...)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

// zs.proxy_payer_addr goes through the SDK's choice for the config's rail:
// filled in from a lone key, checked against the keys when set, and on
// Solana refused when it is not a Solana address.
func TestResolveProxyPayerAddr(t *testing.T) {
	one := algoKeys(t, 1)
	lone := one.Addresses()[0]
	other := algoKeys(t, 1).Addresses()[0]
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	sk, err := keystore.NewSolanaKeys(context.Background(), mnemonicProvider{phrase})
	if err != nil {
		t.Fatal(err)
	}
	sol := sk.Addresses()[0]
	// Spaced by newlines, the phrase also loads its legacy address: signable,
	// unlisted, and what a config written before canonical spacing names.
	spaced := strings.ReplaceAll(phrase, " ", "\n")
	legacy, ok := keystore.LegacySpacingAddress(spaced)
	if !ok {
		t.Fatal("no legacy address for the newline-spaced phrase")
	}
	legacyKeys, err := keystore.New(context.Background(), mnemonicProvider{spaced})
	if err != nil {
		t.Fatal(err)
	}
	canonical := legacyKeys.Addresses()[0]

	tests := []struct {
		name    string
		keys    payerKeys
		set     string
		want    string
		wantErr error
		errHas  string
	}{
		{name: "lone key fills it in", keys: payerKeys{algorand: one}, want: lone},
		{name: "set to the loaded key", keys: payerKeys{algorand: one}, set: lone, want: lone},
		{name: "set to a key not loaded", keys: payerKeys{algorand: one}, set: other, wantErr: wallet.ErrPayerNotLoaded},
		{name: "set to a phrase's legacy address", keys: payerKeys{algorand: legacyKeys}, set: legacy, want: legacy},
		{name: "a legacy phrase unset pays canonically", keys: payerKeys{algorand: legacyKeys}, want: canonical},
		{name: "two keys, unset", keys: payerKeys{algorand: algoKeys(t, 2)}, wantErr: wallet.ErrPayerUnchosen},
		{name: "no keys", keys: payerKeys{algorand: algoKeys(t, 0)}, wantErr: wallet.ErrNoKeys},
		{name: "Solana lone key", keys: payerKeys{solana: sk}, want: sol},
		{name: "Solana, set to an Algorand address", keys: payerKeys{solana: sk}, set: lone,
			wantErr: wallet.ErrPayerNotLoaded, errHas: "not a Solana address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.ZsConfig{ProxyPayerAddr: tt.set}
			err := resolveProxyPayerAddr(&cfg, tt.keys, quietLogger())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || !strings.Contains(err.Error(), "zs.proxy_payer_addr") || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("err = %v, want %v naming zs.proxy_payer_addr and containing %q", err, tt.wantErr, tt.errHas)
				}
				return
			}
			if err != nil || cfg.ProxyPayerAddr != tt.want {
				t.Errorf("= %q, %v; want %q", cfg.ProxyPayerAddr, err, tt.want)
			}
		})
	}
}
