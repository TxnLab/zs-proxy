/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/solana"
	"github.com/TxnLab/zerosignal/go/solana/zsescrow"
	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"
)

// solanaValidConfig is baseValidConfig moved to the Solana rail.
func solanaValidConfig(t *testing.T) *Config {
	cfg := baseValidConfig(t)
	cfg.Algod.ResolvedEndpoint = ""
	cfg.Zs.EscrowAppID = 0
	cfg.Zs.ProxyPayerAddr = ""
	cfg.Solana.Cluster = "devnet"
	return cfg
}

func TestApplyEnv_Solana(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("PROXY_SOLANA_CLUSTER", "localnet")
	t.Setenv("PROXY_SOLANA_RPC_ENDPOINT", "http://127.0.0.1:8899")
	t.Setenv("PROXY_SOLANA_PROGRAM", zsescrow.ProgramID.String())
	cfg := defaults()
	applyEnv(cfg)
	want := SolanaConfig{Cluster: "localnet", RPCEndpoint: "http://127.0.0.1:8899", Program: zsescrow.ProgramID.String()}
	if cfg.Solana != want {
		t.Errorf("Solana = %+v, want %+v", cfg.Solana, want)
	}
}

// Either field selects the rail; the network name follows it.
func TestOnSolana_AndNetworkName(t *testing.T) {
	for _, tc := range []struct {
		s     SolanaConfig
		algod string
		on    bool
		name  string
	}{
		{SolanaConfig{}, "testnet", false, "testnet"},
		{SolanaConfig{Cluster: "Devnet"}, "", true, "solana-devnet"},
		{SolanaConfig{RPCEndpoint: "http://rpc"}, "", true, "solana-custom"},
		// A program alone selects nothing; validation refuses it.
		{SolanaConfig{Program: "x"}, "mainnet", false, "mainnet"},
	} {
		cfg := &Config{Solana: tc.s}
		cfg.Algod.Network = tc.algod
		if cfg.OnSolana() != tc.on || cfg.NetworkName() != tc.name {
			t.Errorf("%+v: OnSolana %v, NetworkName %q; want %v, %q", tc.s, cfg.OnSolana(), cfg.NetworkName(), tc.on, tc.name)
		}
	}
}

func TestValidate_Solana(t *testing.T) {
	cfg := solanaValidConfig(t)
	cfg.Solana.Program = zsescrow.ProgramID.String()
	cfg.Zs.ProxyPayerAddr = zsescrow.ProgramID.String()
	if err := validate(cfg); err != nil {
		t.Fatalf("valid Solana config: %v", err)
	}
	if cfg.Solana.ResolvedProgram != zsescrow.ProgramID {
		t.Errorf("ResolvedProgram = %s, want %s", cfg.Solana.ResolvedProgram, zsescrow.ProgramID)
	}
	want := zs.Network{Rail: ticket.RailSolana, Solana: zs.SolanaNetwork{Cluster: "devnet", Program: zsescrow.ProgramID}}
	if cfg.SolanaNetwork() != want {
		t.Errorf("SolanaNetwork = %+v, want %+v", cfg.SolanaNetwork(), want)
	}

	// An empty program takes the preset's, and a program cleared after a
	// first validation doesn't keep the old one.
	cfg.Solana.Program = ""
	if err := validate(cfg); err != nil || cfg.Solana.ResolvedProgram != (solana.PublicKey{}) {
		t.Errorf("empty program: err %v, ResolvedProgram %s", err, cfg.Solana.ResolvedProgram)
	}

	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"algod network", func(c *Config) { c.Algod.Network = "testnet" }, "algod.network"},
		{"algod endpoint", func(c *Config) { c.Algod.Endpoint = "http://algod" }, "algod.network"},
		{"escrow app", func(c *Config) { c.Zs.EscrowAppID = 9 }, "zs.escrow_app_id"},
		{"bad program", func(c *Config) { c.Solana.Program = "not-base58!" }, "solana.program"},
		{"unknown cluster", func(c *Config) { c.Solana.Cluster = "mainnet-beta" }, "solana:"},
		{"endpoint scheme", func(c *Config) { c.Solana.RPCEndpoint = "ws://127.0.0.1:8900" }, "solana.rpc_endpoint"},
		{"endpoint credentials", func(c *Config) { c.Solana.RPCEndpoint = "https://u:p@rpc.example" }, "solana.rpc_endpoint"},
		{"Algorand payer", func(c *Config) { c.Zs.ProxyPayerAddr = "not-base58!" }, "not a Solana address"},
	} {
		c := solanaValidConfig(t)
		tc.edit(c)
		if err := validate(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A program with nothing selecting the rail would be ignored on Algorand, so
// it is refused.
func TestValidate_SolanaProgramWithoutRail(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Solana.Program = zsescrow.ProgramID.String()
	if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "solana.program") {
		t.Errorf("err %v, want a solana.program refusal", err)
	}
}

// Finalize resolves no algod on Solana, so a Solana config with no algod
// settings loads.
func TestFinalize_SolanaSkipsAlgod(t *testing.T) {
	cfg := solanaValidConfig(t)
	if err := Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if cfg.Algod.ResolvedEndpoint != "" {
		t.Errorf("ResolvedEndpoint = %q, want none on Solana", cfg.Algod.ResolvedEndpoint)
	}
}
