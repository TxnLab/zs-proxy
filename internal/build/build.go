/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package build holds compile-time constants injected via -ldflags -X at
// release time, plus the per-network presets that make a config-less run work.
// The canonical ZeroSignalEscrow app ids come from proto's shared algod package
// (the single source of truth, also consumed by the node) so the two services
// can't drift on which contract a network points at; a self-hoster overrides
// them via zs.escrow_app_id in config. The proxy-specific bits (testnet
// dispenser, genesis fee-sink) stay here.
package build

import (
	"github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"
)

// NetworkNames are the --network values, for help and error text.
const NetworkNames = zs.NetworkNames

// Release-stamped via -ldflags "-X .../internal/build.<Name>=<value>".
// The defaults below are the dev/source values.
var (
	Version = "dev"
	Commit  = "none"

	// KeyringBackend declares where a NEW wallet's secret is stored; an
	// existing wallet opens on the backend its wallet.json records. Release
	// builds (which are code-signed) stamp "os" so the OS keychain is used
	// prompt-free; an un-stamped dev/`go build` binary leaves it "" → the
	// encrypted file backend, avoiding the repeated keychain auth prompts an
	// unsigned binary triggers. ZEROSIGNAL_KEYRING_BACKEND overrides at runtime.
	KeyringBackend = ""

	// TestnetDispenserURL is the TxnLab testnet funding endpoint; empty until one ships.
	TestnetDispenserURL = ""

	// ClientURL is the hosted zerosignal web client `wallet login` opens for the
	// browser→CLI recovery-phrase handoff. The passkey is origin-bound, so the
	// export ceremony must run on the real hosted origin — there is no localnet
	// equivalent. Overridable per-invocation via `wallet login --client-url`
	// (e.g. http://localhost:3000 for staging/dev).
	ClientURL = "https://zerosignal.ai"
)

// DefaultNetwork is the zero-config network: a fresh `proxy start` with no
// config and no --network boots here. Mainnet is the default — a config-less
// run opens real escrow against the production ZeroSignalEscrow contract. Select
// `--network testnet` (or set algod.network) to run against free testnet.
const DefaultNetwork = "mainnet"

// Network is the set of embedded presets for one network.
type Network struct {
	// Name is the --network value: an algod.network ("testnet" | "mainnet" |
	// "localnet"), or "solana-" and a Solana cluster preset.
	Name string
	// SolanaCluster is the solana.cluster a Solana network sets, "" on
	// Algorand. Its RPC endpoint and program id are the SDK's presets.
	SolanaCluster string
	// EscrowAppID is the canonical ZeroSignalEscrow app id for this network (from
	// algod.EscrowAppID), or 0 when none is shipped (localnet — deploy-specific)
	// — in which case it must come from config. USDC/HAY asset ids are NOT
	// embedded; they come from this contract's on-chain global state.
	EscrowAppID uint64
	// DispenserURL is the testnet funding endpoint, empty elsewhere.
	DispenserURL string
}

// NetworkPreset returns the embedded preset for a known network name.
// ok is false for an unrecognized name.
func NetworkPreset(name string) (Network, bool) {
	n, ok := zs.NetworkNamed(name)
	if !ok {
		return Network{}, false
	}
	if n.Rail == ticket.RailSolana {
		return Network{Name: name, SolanaCluster: n.Solana.Cluster}, true
	}
	// localnet's app id is deploy-specific, so algod has none: it comes from
	// config / --config, never an embedded constant.
	p := Network{Name: name, EscrowAppID: algod.EscrowAppID(name)}
	if name == "testnet" {
		p.DispenserURL = TestnetDispenserURL
	}
	return p, true
}
