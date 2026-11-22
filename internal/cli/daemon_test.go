/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"strings"
	"testing"

	"github.com/TxnLab/zs-proxy/internal/daemon"
)

// A bare restart passes on the --network the daemon started with, and for a
// record without one an Algorand preset only: a Solana name is the solana:
// block's, and as a flag it would reset the block's program and endpoint, or
// name no preset at all.
func TestRestartNetwork(t *testing.T) {
	for _, tc := range []struct {
		rec  daemon.Record
		want string
	}{
		{daemon.Record{Network: "solana-devnet", NetworkFlag: "solana-devnet"}, "solana-devnet"},
		{daemon.Record{Network: "testnet", NetworkFlag: "testnet"}, "testnet"},
		{daemon.Record{Network: "testnet"}, "testnet"},
		{daemon.Record{Network: "solana-devnet"}, ""},
		{daemon.Record{Network: "solana-custom"}, ""},
		{daemon.Record{Network: "custom"}, ""},
		{daemon.Record{}, ""},
	} {
		if got := restartNetwork(tc.rec); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.rec, got, tc.want)
		}
	}
}

// TestChildArgs_MirrorsUserFlags checks the detached child gets --foreground
// plus exactly the config-determining flags the user passed — and crucially
// does NOT synthesize --network (which would override a self-hoster's
// config-supplied escrow_app_id).
func TestChildArgs_MirrorsUserFlags(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		f := proxyFlags{}
		got := strings.Join(f.childArgs(), " ")
		if got != "proxy start --foreground" {
			t.Errorf("childArgs = %q, want bare foreground start", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		f := proxyFlags{
			configPath:  "/etc/zs/config.yaml",
			network:     "mainnet",
			port:        9090,
			trackTimes:  true,
			debugBodies: true,
		}
		got := f.childArgs()
		want := []string{
			"proxy", "start", "--foreground",
			"--config", "/etc/zs/config.yaml",
			"--network", "mainnet",
			"--port", "9090",
			"--times", "--debug-bodies",
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("childArgs = %v\nwant %v", got, want)
		}
	})

	t.Run("no synthesized network", func(t *testing.T) {
		// User didn't pass --network: the child must not get one either, so it
		// re-resolves the same way (preserving a config-supplied app id).
		f := proxyFlags{configPath: "/etc/zs/config.yaml"}
		for _, a := range f.childArgs() {
			if a == "--network" {
				t.Fatal("childArgs synthesized --network when the user gave none")
			}
		}
	})
}

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		":8080":          "http://127.0.0.1:8080",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"[::]:8080":      "http://127.0.0.1:8080",
		"10.0.0.5:1234":  "http://10.0.0.5:1234",
	}
	for in, want := range cases {
		if got := baseURL(in); got != want {
			t.Errorf("baseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProxyLifecycleCommands asserts the lifecycle subcommands are wired under
// `proxy` and `proxy start` carries the new --foreground flag.
func TestProxyLifecycleCommands(t *testing.T) {
	root := newRootCmd()
	for _, sub := range []string{"start", "stop", "status", "restart", "logs"} {
		if _, _, err := root.Find([]string{"proxy", sub}); err != nil {
			t.Errorf("proxy %s not found: %v", sub, err)
		}
	}
	start, _, err := root.Find([]string{"proxy", "start"})
	if err != nil {
		t.Fatalf("proxy start not found: %v", err)
	}
	if start.Flags().Lookup("foreground") == nil {
		t.Error("proxy start missing --foreground flag")
	}
}
