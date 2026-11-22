/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protoalgod "github.com/TxnLab/zerosignal/go/algod"

	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/spend"
)

// spendCfg is a config on network, or on a bare mainnet endpoint when network
// is "", with escrow app id app.
func spendCfg(network string, app uint64) *config.Config {
	c := &config.Config{}
	c.Algod.Network, c.Zs.EscrowAppID = network, app
	if network == "" {
		c.Algod.Endpoint = "https://mainnet-api.algonode.cloud"
	}
	return c
}

// statePath is spendStatePath adopting, failing t on a failed move.
func statePath(t *testing.T, dir string, cfg *config.Config) string {
	t.Helper()
	p, err := spendStatePath(dir, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Each deployment keeps its own tally, and only mainnet's first file starts
// from the shared legacy one; a file already there is never overwritten.
func TestSpendStatePath(t *testing.T) {
	cfgFor := spendCfg
	spendStatePath := func(dir string, cfg *config.Config) string { return statePath(t, dir, cfg) }
	dir := t.TempDir()
	legacy := []byte(`{"legacy":true}`)
	if err := os.WriteFile(filepath.Join(dir, legacySpendFile), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}

	mainApp := protoalgod.EscrowAppID("mainnet")
	if mainApp == 0 {
		t.Skip("no mainnet escrow app is shipped")
	}
	// Mainnet by app id alone: a config naming only an endpoint has no network.
	main := spendStatePath(dir, cfgFor("", mainApp))
	test := spendStatePath(dir, cfgFor("testnet", 8))
	wantMain := fmt.Sprintf("spend-algorand-mainnet-%d.json", mainApp)
	if main == test || filepath.Base(main) != wantMain || filepath.Base(test) != "spend-algorand-testnet-8.json" {
		t.Fatalf("paths %s and %s, want %s and spend-algorand-testnet-8.json", main, test, wantMain)
	}
	if read(spendStatePath(dir, cfgFor("mainnet", 9))) != "" {
		t.Error("a mainnet-named config on another app inherited the legacy file")
	}
	if read(main) != string(legacy) {
		t.Errorf("mainnet's first tally %q, want the legacy file", read(main))
	}
	if read(test) != "" {
		t.Errorf("testnet's tally %q, want none: the legacy file mixed networks", read(test))
	}
	if err := os.WriteFile(main, []byte(`{"own":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spendStatePath(dir, cfgFor("mainnet", mainApp))
	if got := read(main); got != `{"own":true}` {
		t.Errorf("an existing tally became %q", got)
	}
}

// An upgraded proxy carries on the day's tally and open reservations kept
// under the app-id name, rather than handing out a fresh cap mid-day; on
// mainnet that file wins over the older all-networks one.
func TestSpendStatePath_AdoptsTheAppIDTally(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	write("spend-8.json", `{"day":"testnet"}`)
	write("spend-8.open.json", `{"open":"testnet"}`)
	path := statePath(t, dir, spendCfg("testnet", 8))
	if read(path) != `{"day":"testnet"}` || read(spend.OpenPath(path)) != `{"open":"testnet"}` {
		t.Errorf("tally %q, open %q, want the app-id files' contents", read(path), read(spend.OpenPath(path)))
	}
	for _, old := range []string{"spend-8.json", "spend-8.open.json"} {
		if _, err := os.Stat(filepath.Join(dir, old)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (err %v)", old, err)
		}
	}

	mainApp := protoalgod.EscrowAppID("mainnet")
	if mainApp == 0 {
		t.Skip("no mainnet escrow app is shipped")
	}
	write(legacySpendFile, `{"all":"networks"}`)
	write(fmt.Sprintf("spend-%d.json", mainApp), `{"day":"mainnet"}`)
	if got := read(statePath(t, dir, spendCfg("mainnet", mainApp))); got != `{"day":"mainnet"}` {
		t.Errorf("mainnet tally %q, want the per-app file over spend.json", got)
	}
}

// status only reads: it finds the tally under its earlier name and leaves it
// there, since a proxy may be serving from that name without its lock.
func TestSpendStatePath_StatusMovesNothing(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "spend-8.json")
	if err := os.WriteFile(legacy, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := spendStatePath(dir, spendCfg("testnet", 8), false)
	if err != nil || got != legacy {
		t.Fatalf("status read %s (err %v), want %s", got, err, legacy)
	}
	if _, err := os.Stat(filepath.Join(dir, "spend-algorand-testnet-8.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("status moved the tally (err %v)", err)
	}
}

// tempStateDir points paths.StateDir at a fresh directory and returns it.
func tempStateDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	dir, err := paths.StateDir()
	if err != nil {
		t.Skip("no state directory on this system")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serve's guard carries on the day under the earlier name: without it an
// upgrade mid-day hands out a fresh cap.
func TestBuildSpendGuard_CarriesOnTheDay(t *testing.T) {
	dir := tempStateDir(t)
	day := time.Now().UTC().Format(time.DateOnly)
	tally := fmt.Sprintf(`{"date":%q,"spent_microusdc":4200000}`, day)
	if err := os.WriteFile(filepath.Join(dir, "spend-8.json"), []byte(tally), 0o600); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	g := buildSpendGuard(spendCfg("testnet", 8), &warn)
	defer g.Close()
	if got := g.SpentToday(); got != 4_200_000 {
		t.Errorf("spent today %d, want the earlier tally's 4200000", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "spend-algorand-testnet-8.json")); err != nil {
		t.Errorf("the tally did not move: %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("warned %q", warn.String())
	}
}

// status reads the day where it is and moves nothing.
func TestBuildSpendGuard_StatusMovesNothing(t *testing.T) {
	dir := tempStateDir(t)
	day := time.Now().UTC().Format(time.DateOnly)
	tally := fmt.Sprintf(`{"date":%q,"spent_microusdc":4200000}`, day)
	legacy := filepath.Join(dir, "spend-8.json")
	if err := os.WriteFile(legacy, []byte(tally), 0o600); err != nil {
		t.Fatal(err)
	}
	g := readSpendGuard(spendCfg("testnet", 8))
	g.Close()
	if got := g.SpentToday(); got != 4_200_000 {
		t.Errorf("spent today %d, want the earlier tally's 4200000", got)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("status moved the tally: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "spend-algorand-testnet-8.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("status created the new tally (err %v)", err)
	}
}

// A move that fails says so, on mainnet too, which takes its own path past
// the older spend.json: the cap silently miscounting is the failure this
// warning exists for.
func TestBuildSpendGuard_WarnsOnAFailedMove(t *testing.T) {
	type tc struct {
		cfg     *config.Config
		allFile bool // the older all-networks spend.json is there
		openDir bool // the open file's move fails, so the tally stays put
	}
	cases := map[string]tc{"testnet": {cfg: spendCfg("testnet", 8)}}
	if mainApp := protoalgod.EscrowAppID("mainnet"); mainApp != 0 {
		cases["mainnet"] = tc{cfg: spendCfg("mainnet", mainApp)}
		cases["mainnet with spend.json"] = tc{cfg: spendCfg("mainnet", mainApp), allFile: true}
		cases["mainnet, nothing moved"] = tc{cfg: spendCfg("mainnet", mainApp), openDir: true}
	}
	for name, c := range cases {
		cfg := c.cfg
		t.Run(name, func(t *testing.T) {
			dir := tempStateDir(t)
			// A directory cannot be hard-linked, so the move fails.
			legacy := filepath.Join(dir, fmt.Sprintf("spend-%d.json", cfg.Zs.EscrowAppID))
			if c.openDir {
				if err := os.WriteFile(legacy, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				legacy = spend.OpenPath(legacy)
			}
			if err := os.Mkdir(legacy, 0o700); err != nil {
				t.Fatal(err)
			}
			if c.allFile {
				if err := os.WriteFile(filepath.Join(dir, legacySpendFile), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var warn bytes.Buffer
			buildSpendGuard(cfg, &warn).Close()
			if !strings.Contains(warn.String(), "moving the spend tally") {
				t.Errorf("warned %q, want the failed-move warning", warn.String())
			}
		})
	}
}

// A network the client would refuse keeps the tally in memory.
func TestSpendStatePath_UnresolvableNetwork(t *testing.T) {
	if got, err := spendStatePath(t.TempDir(), spendCfg("bogus", 8), true); got != "" || err != nil {
		t.Errorf("spendStatePath = %q, %v; want \"\"", got, err)
	}
}

// The tally is named by the deployment the journal is, an endpoint-only
// config on an app no preset owns included.
func TestSpendStatePath_NamedAsTheJournalIs(t *testing.T) {
	c := &config.Config{}
	c.Algod.Endpoint, c.Zs.EscrowAppID = "http://localhost:4001", 8
	if got := filepath.Base(statePath(t, t.TempDir(), c)); got != "spend-algorand-custom-8.json" {
		t.Errorf("tally %s, want spend-algorand-custom-8.json", got)
	}
}

func TestUsdcToMicro(t *testing.T) {
	cases := map[float64]uint64{
		0:     0,
		-5:    0, // non-positive (incl. "unlimited") → 0
		1:     1_000_000,
		25:    25_000_000,
		0.5:   500_000,
		1.234: 1_234_000,
	}
	for in, want := range cases {
		if got := usdcToMicro(in); got != want {
			t.Errorf("usdcToMicro(%g) = %d, want %d", in, got, want)
		}
	}
}
