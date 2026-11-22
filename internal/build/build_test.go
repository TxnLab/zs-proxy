/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package build

import (
	"strings"
	"testing"
)

// The help and error text name exactly the presets --network accepts.
func TestNetworkNamesArePresets(t *testing.T) {
	names := strings.Split(NetworkNames, "|")
	for _, n := range names {
		if _, ok := NetworkPreset(n); !ok {
			t.Errorf("%q is named but no preset", n)
		}
	}
	for _, n := range []string{"testnet", "mainnet", "localnet", "solana-devnet", "solana-localnet"} {
		if !strings.Contains("|"+NetworkNames+"|", "|"+n+"|") {
			t.Errorf("preset %q is not named", n)
		}
	}
}
