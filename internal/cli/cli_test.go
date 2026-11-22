/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/zs/tee"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// The TEE collateral cache must persist at the proxy's state directory: a
// cache directory gets swept, and a cold start during an Intel PCS outage
// takes every TEE operator out of routing.
//
// The attach lasts for the process, so the state dir is a temporary one: the
// rest of the package must not warm-start from a developer's real file.
func TestConfigureSDK_PointsCollateralAtStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	want, err := paths.StateDir()
	if err != nil {
		t.Skip("no state directory on this system")
	}
	configureSDK()
	// The cache persists to one directory. Another being refused shows
	// configureSDK attached one; the state dir being accepted shows which.
	if err := tee.PersistCollateral(t.TempDir()); err == nil {
		t.Fatal("configureSDK attached no directory: any other was accepted")
	}
	if err := tee.PersistCollateral(want); err != nil {
		t.Errorf("the collateral cache does not persist at the state dir: %v", err)
	}
}

// configureSDK has to run before any command verifies a quote, so the cache
// starts warm from disk; Execute calls it before building or running the
// command tree.
func TestExecute_ConfiguresSDKFirst(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "cli.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Execute" || fn.Recv != nil {
			continue
		}
		if len(fn.Body.List) > 0 {
			if stmt, ok := fn.Body.List[0].(*ast.ExprStmt); ok {
				if call, ok := stmt.X.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "configureSDK" {
						return
					}
				}
			}
		}
		t.Fatal("Execute's first statement is not configureSDK()")
	}
	t.Fatal("no Execute function in cli.go")
}

func TestDiscoverConfigPath_FromEnv(t *testing.T) {
	t.Setenv("PROXY_CONFIG", "/etc/proxy/thing.yaml")
	if got := discoverConfigPath("/some/state/config.yaml"); got != "/etc/proxy/thing.yaml" {
		t.Errorf("got %q, want /etc/proxy/thing.yaml", got)
	}
}

func TestRouteStartToService(t *testing.T) {
	// The foreground guard is load-bearing: the service runs `proxy start
	// --foreground`, so a foreground invocation must never route back to the
	// service (that would recurse / no-op the service's own serving child).
	cases := []struct {
		foreground, installed, daemonRunning, want bool
	}{
		{foreground: false, installed: true, want: true},                       // service only → hand off
		{foreground: false, installed: true, daemonRunning: true, want: false}, // both → daemon wins
		{foreground: false, installed: false, want: false},                     // neither → daemonize
		{foreground: true, installed: true, want: false},                       // service child → never recurse
		{foreground: true, installed: true, daemonRunning: true, want: false},  // foreground always serves
		{foreground: true, installed: false, daemonRunning: false, want: false},
	}
	for _, c := range cases {
		if got := routeStartToService(c.foreground, c.installed, c.daemonRunning); got != c.want {
			t.Errorf("routeStartToService(foreground=%v, installed=%v, daemonRunning=%v) = %v, want %v",
				c.foreground, c.installed, c.daemonRunning, got, c.want)
		}
	}
}

func TestManagerNameFor(t *testing.T) {
	cases := map[string]string{
		"darwin":  "launchd",
		"linux":   "systemd (user)",
		"windows": "Windows SCM",
		"plan9":   "plan9", // unknown ⇒ echo the GOOS
	}
	for goos, want := range cases {
		if got := managerNameFor(goos); got != want {
			t.Errorf("managerNameFor(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestChangedFlags(t *testing.T) {
	var f proxyFlags
	cmd := &cobra.Command{Use: "x"}
	f.bind(cmd) // binds --config/--network/--port (among others)

	if got := changedFlags(cmd, listenAffectingFlags); len(got) != 0 {
		t.Errorf("changedFlags with nothing set = %v, want none", got)
	}

	// A flag's default value does NOT count as changed — only an explicit set.
	if err := cmd.Flags().Set("network", "mainnet"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("port", "9090"); err != nil {
		t.Fatal(err)
	}
	got := changedFlags(cmd, listenAffectingFlags)
	if len(got) != 2 || got[0] != "network" || got[1] != "port" {
		t.Errorf("changedFlags = %v, want [network port] (config not set)", got)
	}

	// A name not bound on the command is safely skipped (no panic).
	if got := changedFlags(cmd, []string{"does-not-exist"}); len(got) != 0 {
		t.Errorf("changedFlags(unbound) = %v, want none", got)
	}
}

func TestDiscoverConfigPath_StateFile(t *testing.T) {
	os.Unsetenv("PROXY_CONFIG")
	dir := t.TempDir()
	state := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(state, []byte("server:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run from a dir with no cwd config.yaml so the state file wins.
	t.Chdir(t.TempDir())
	if got := discoverConfigPath(state); got != state {
		t.Errorf("got %q, want %q", got, state)
	}
}

func TestDiscoverConfigPath_NoneFallsToEmpty(t *testing.T) {
	os.Unsetenv("PROXY_CONFIG")
	// cwd with no config.yaml, and a state path that doesn't exist → "".
	t.Chdir(t.TempDir())
	if got := discoverConfigPath(filepath.Join(t.TempDir(), "nope.yaml")); got != "" {
		t.Errorf("got %q, want empty (use embedded defaults)", got)
	}
}

func TestDiscoverConfigPath_CwdLegacy(t *testing.T) {
	os.Unsetenv("PROXY_CONFIG")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// No PROXY_CONFIG and a non-existent state file → cwd config.yaml.
	if got := discoverConfigPath("/no/such/state.yaml"); got != "config.yaml" {
		t.Errorf("got %q, want config.yaml", got)
	}
}

// TestProxyStartFlags_ShareSurface asserts the legacy root invocation and
// `proxy start` expose the same flags — the contract that keeps
// `go run . --config config.yaml` working alongside `proxy start --config`.
func TestProxyStartFlags_ShareSurface(t *testing.T) {
	root := newRootCmd()
	start, _, err := root.Find([]string{"proxy", "start"})
	if err != nil {
		t.Fatalf("proxy start not found: %v", err)
	}
	for _, name := range []string{"config", "network", "port", "times", "debug-bodies"} {
		if root.Flags().Lookup(name) == nil {
			t.Errorf("root missing legacy flag --%s", name)
		}
		if start.Flags().Lookup(name) == nil {
			t.Errorf("proxy start missing flag --%s", name)
		}
	}
}

func TestApplyNetwork_ExplicitTestnet(t *testing.T) {
	cfg := &config.Config{}
	if err := applyNetwork(cfg, "testnet"); err != nil {
		t.Fatal(err)
	}
	if cfg.Algod.Network != "testnet" {
		t.Errorf("network = %q, want testnet", cfg.Algod.Network)
	}
	want, _ := build.NetworkPreset("testnet")
	if cfg.Zs.EscrowAppID != want.EscrowAppID || want.EscrowAppID == 0 {
		t.Errorf("escrow_app_id = %d, want %d", cfg.Zs.EscrowAppID, want.EscrowAppID)
	}
}

func TestApplyNetwork_DefaultsToMainnet(t *testing.T) {
	cfg := &config.Config{}
	if err := applyNetwork(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Algod.Network != build.DefaultNetwork {
		t.Errorf("network = %q, want %q", cfg.Algod.Network, build.DefaultNetwork)
	}
	if cfg.Zs.EscrowAppID == 0 {
		t.Error("escrow_app_id not filled from embedded default-network preset")
	}
}

func TestApplyNetwork_ExplicitFlagOverridesFileNetwork(t *testing.T) {
	cfg := &config.Config{}
	cfg.Algod.Network = "mainnet" // pretend the file said mainnet
	if err := applyNetwork(cfg, "testnet"); err != nil {
		t.Fatal(err)
	}
	if cfg.Algod.Network != "testnet" {
		t.Errorf("explicit --network testnet did not override file mainnet: %q", cfg.Algod.Network)
	}
}

func TestApplyNetwork_LocalnetWithoutAppIDErrors(t *testing.T) {
	// Localnet ships no embedded app id (deploy-specific); selecting it
	// without a config app id must be a clear error, not a silent misroute.
	// (Mainnet and testnet both embed a canonical id via algod.EscrowAppID.)
	cfg := &config.Config{}
	if err := applyNetwork(cfg, "localnet"); err == nil {
		t.Fatal("expected error selecting localnet with no embedded/config app id")
	}
}

func TestApplyNetwork_MainnetAppIDEmbedded(t *testing.T) {
	// Mainnet now embeds a canonical app id (from proto's algod.EscrowAppID),
	// so selecting it with no config app id resolves rather than errors.
	cfg := &config.Config{}
	if err := applyNetwork(cfg, "mainnet"); err != nil {
		t.Fatalf("mainnet should resolve from the embedded preset: %v", err)
	}
	if cfg.Zs.EscrowAppID == 0 {
		t.Error("escrow_app_id not filled from embedded mainnet preset")
	}
}

func TestApplyNetwork_SelfHostedAppIDPreserved(t *testing.T) {
	// No --network, file sets a custom app id on (default) mainnet → keep it.
	cfg := &config.Config{}
	cfg.Zs.EscrowAppID = 99999999
	if err := applyNetwork(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Zs.EscrowAppID != 99999999 {
		t.Errorf("self-hosted escrow_app_id overwritten: %d", cfg.Zs.EscrowAppID)
	}
}

func TestApplyNetwork_UnknownErrors(t *testing.T) {
	cfg := &config.Config{}
	if err := applyNetwork(cfg, "regtest"); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

// --network moves the rail: a Solana preset clears the Algorand fields it
// would otherwise be refused beside, and an Algorand one clears the solana:
// block.
func TestApplyNetwork_SolanaPresets(t *testing.T) {
	file := func() *config.Config {
		cfg := &config.Config{}
		cfg.Algod.Network, cfg.Algod.Endpoint, cfg.Zs.EscrowAppID = "mainnet", "https://algod", 7
		cfg.Solana = config.SolanaConfig{RPCEndpoint: "http://rpc", Program: "PROG"}
		return cfg
	}
	for _, tc := range []struct {
		flag, cluster, rpc, program string
	}{
		// The devnet program and RPC are the preset's; a localnet's are the
		// deploy's.
		{"solana-devnet", "devnet", "", ""},
		{"solana-localnet", "localnet", "http://rpc", "PROG"},
	} {
		cfg := file()
		if err := applyNetwork(cfg, tc.flag); err != nil {
			t.Fatalf("%s: %v", tc.flag, err)
		}
		want := config.SolanaConfig{Cluster: tc.cluster, RPCEndpoint: tc.rpc, Program: tc.program}
		if cfg.Solana != want || cfg.Algod.Network != "" || cfg.Algod.Endpoint != "" || cfg.Zs.EscrowAppID != 0 {
			t.Errorf("%s: solana %+v, algod %q %q, app %d", tc.flag, cfg.Solana, cfg.Algod.Network, cfg.Algod.Endpoint, cfg.Zs.EscrowAppID)
		}
		if cfg.NetworkName() != tc.flag {
			t.Errorf("%s: NetworkName %q", tc.flag, cfg.NetworkName())
		}
	}

	cfg := file()
	if err := applyNetwork(cfg, "testnet"); err != nil {
		t.Fatal(err)
	}
	if cfg.Solana != (config.SolanaConfig{}) || cfg.OnSolana() {
		t.Errorf("--network testnet kept the solana block: %+v", cfg.Solana)
	}

	// No flag: a solana: block keeps the rail, with no Algorand default.
	cfg = &config.Config{Solana: config.SolanaConfig{Cluster: "devnet"}}
	if err := applyNetwork(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Algod.Network != "" || cfg.Zs.EscrowAppID != 0 {
		t.Errorf("Algorand default stamped onto a Solana config: %q, %d", cfg.Algod.Network, cfg.Zs.EscrowAppID)
	}
}

func TestApplyNetwork_RawEndpointRespected(t *testing.T) {
	// A raw algod.endpoint with no network is fully manual: don't stamp a
	// default network or escrow app id over it.
	cfg := &config.Config{}
	cfg.Algod.Endpoint = "https://my-node.example"
	cfg.Zs.EscrowAppID = 12345
	if err := applyNetwork(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Algod.Network != "" {
		t.Errorf("raw endpoint got a stamped network: %q", cfg.Algod.Network)
	}
	if cfg.Zs.EscrowAppID != 12345 {
		t.Errorf("escrow_app_id changed under raw endpoint: %d", cfg.Zs.EscrowAppID)
	}
}
