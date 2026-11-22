/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package wallet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/TxnLab/zerosignal/go/wallet"

	"github.com/TxnLab/zs-proxy/internal/paths"
)

func TestSolanaAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	t.Setenv(sdk.EnvBackend, "file")
	t.Setenv(sdk.EnvPassphrase, "test-passphrase")

	// A recorded address needs no store, and a 25-word wallet has none.
	if got, err := SolanaAddress(Metadata{SolanaAddress: "X"}); err != nil || got != "X" {
		t.Errorf("recorded: %q, %v", got, err)
	}
	if _, err := SolanaAddress(Metadata{Kind: KindAlgorand25}); !errors.Is(err, ErrNoSolanaKey) {
		t.Errorf("25-word: %v, want ErrNoSolanaKey", err)
	}

	w, err := OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	meta, err := w.Generate("testnet", false)
	if err != nil {
		t.Fatal(err)
	}
	want := meta.SolanaAddress
	if want == "" {
		t.Fatal("Generate recorded no Solana address")
	}

	// A wallet written before the address was recorded: reading its phrase
	// records the address, which is the one Generate derived.
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "wallet.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "solana_address")
	if raw, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := DefaultMetadata()
	if err != nil || legacy.SolanaAddress != "" {
		t.Fatalf("legacy metadata %+v, %v", legacy, err)
	}
	if got, err := SolanaAddress(legacy); err != nil || got != want {
		t.Errorf("legacy wallet: %q, %v; want %q", got, err, want)
	}
	if m, _ := DefaultMetadata(); m.SolanaAddress != want {
		t.Errorf("not recorded after the read: %q", m.SolanaAddress)
	}
}
