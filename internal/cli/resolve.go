/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/TxnLab/zerosignal/go/solana/zsescrow"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// resolveConfig builds the effective *config.Config from the precedence
// chain (lowest → highest): embedded defaults → config.yaml → PROXY_* env →
// CLI flags. It is shared by `proxy start` (which then serves) and
// `config print-effective` (which prints the result), so what gets printed is
// exactly what would run.
func (f *proxyFlags) resolveConfig() (*config.Config, error) {
	cfg, err := config.LoadBase(f.configPath)
	if err != nil {
		return nil, fmt.Errorf("config load failed: %w", err)
	}

	if err := applyNetwork(cfg, f.network); err != nil {
		return nil, err
	}

	if f.port != 0 {
		host, _, err := net.SplitHostPort(cfg.Server.Listen)
		if err != nil {
			return nil, fmt.Errorf("cannot parse configured listen %q: %w", cfg.Server.Listen, err)
		}
		cfg.Server.Listen = net.JoinHostPort(host, strconv.Itoa(f.port))
	}

	// --allow-staging is an enable-only switch (no "unset" sentinel for a bool):
	// it can flip staging routing on over a config/env default of false, but
	// never forces it off — set zs.allow_staging / PROXY_ZS_ALLOW_STAGING
	// to false to keep it disabled.
	if f.allowStaging {
		cfg.Zs.AllowStaging = true
	}

	if err := config.Finalize(cfg); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}
	return cfg, nil
}

// applyNetwork selects the effective network and stamps the embedded preset
// (algod.network + zs.escrow_app_id) onto cfg. The USDC asset id moves with
// the network too — it's consumed by wallet opt-in / funding in a later phase
// via build.NetworkPreset(cfg.Algod.Network).
//
//   - explicit --network: authoritative for both fields, overriding file/env.
//     A network with no embedded app id (localnet — deploy-specific) is an
//     error unless the config already supplies one.
//   - no --network: use the file/env algod.network if set, else the embedded
//     default (mainnet); fill escrow_app_id from the preset only when the
//     file/env left it unset, so a self-hoster's custom app id is preserved.
func applyNetwork(cfg *config.Config, flagNetwork string) error {
	// Explicit --network is authoritative for both fields, overriding file/env.
	if flagNetwork != "" {
		preset, ok := build.NetworkPreset(flagNetwork)
		if !ok {
			return fmt.Errorf("unknown --network %q (want %s)", flagNetwork, build.NetworkNames)
		}
		if preset.SolanaCluster != "" {
			// The rail moves with the flag, so the other rail's fields go:
			// validation refuses a config naming both.
			cfg.Algod.Network, cfg.Algod.Endpoint, cfg.Zs.EscrowAppID = "", "", 0
			cfg.Solana.Cluster = preset.SolanaCluster
			// The devnet program and RPC are the preset's, as testnet's app
			// id is: the program id is the same on every cluster, so a
			// file's localnet endpoint would pay there under devnet's name.
			// A localnet's are deploy-specific, so the file's stay.
			if preset.SolanaCluster != zsescrow.ClusterLocalnet {
				cfg.Solana.Program, cfg.Solana.RPCEndpoint = "", ""
			}
			return nil
		}
		cfg.Solana = config.SolanaConfig{}
		cfg.Algod.Network = preset.Name
		switch {
		case preset.EscrowAppID != 0:
			cfg.Zs.EscrowAppID = preset.EscrowAppID
		case cfg.Zs.EscrowAppID == 0:
			return fmt.Errorf("no embedded ZeroSignalEscrow app id for %q — set zs.escrow_app_id in config, or use --network testnet", flagNetwork)
		}
		return nil
	}

	// No --network: a config that gives a raw algod.endpoint with no network is
	// fully manual — don't stamp a default network or app id onto it (that would
	// also pick a possibly-wrong escrow app id); the user supplies escrow_app_id.
	if cfg.Algod.Network == "" && cfg.Algod.Endpoint != "" {
		return nil
	}
	// A solana: block selects the Solana rail; stamping the Algorand default
	// onto it would make validation refuse the pair.
	if cfg.OnSolana() {
		return nil
	}

	// Otherwise fill the network from the file/env value, else the embedded
	// default, and gap-fill escrow_app_id from the preset only when one isn't
	// set. An unrecognized file network is left untouched for algod.Resolve /
	// validate to handle (so a network algod supports but we ship no preset for
	// isn't rejected here).
	name := cfg.Algod.Network
	if name == "" {
		name = build.DefaultNetwork
		cfg.Algod.Network = name
	}
	if preset, ok := build.NetworkPreset(name); ok && cfg.Zs.EscrowAppID == 0 {
		cfg.Zs.EscrowAppID = preset.EscrowAppID
	}
	return nil
}

// defaultConfigPath is the cobra flag default for --config. It honors
// PROXY_CONFIG, then a state-dir config.yaml, then a cwd config.yaml (legacy),
// and finally "" — meaning no file, boot from embedded defaults.
func defaultConfigPath() string {
	stateFile, _ := paths.ConfigFile()
	return discoverConfigPath(stateFile)
}

// discoverConfigPath is the pure, testable core of defaultConfigPath.
// stateFile is passed in because os.UserConfigDir isn't overridable on macOS.
func discoverConfigPath(stateFile string) string {
	if v, ok := os.LookupEnv("PROXY_CONFIG"); ok {
		return v
	}
	if stateFile != "" {
		if _, err := os.Stat(stateFile); err == nil {
			return stateFile
		}
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return ""
}
