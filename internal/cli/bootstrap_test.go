/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/mnemonic"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/cli/service"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/daemon"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// isolateWalletState points the per-OS state and home dirs at a temp dir, forces
// the encrypted-file keystore (never the developer's real keychain), and clears
// every env signer, so the test sees a machine with no wallet at all.
func isolateWalletState(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("ZEROSIGNAL_KEYRING_BACKEND", "file")
	t.Setenv("ZEROSIGNAL_KEYSTORE_PASSPHRASE", "test-passphrase")
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if strings.HasSuffix(name, "_MNEMONIC") || name == keystore.MnemonicURLsEnv {
			t.Setenv(name, "") // registers the restore
			os.Unsetenv(name)  // then remove it outright, leaving nothing to count
		}
	}
	if wallet.DefaultExists() || hasEnvMnemonic() {
		t.Fatal("isolation failed: a wallet or env signer is still visible")
	}
}

// feedStdin replaces os.Stdin with a pipe holding "n\n". A test that expects a
// refusal still feeds [n], so a regression that falls through to the prompt
// takes the harmless create-in-a-temp-dir branch (and fails the assertion)
// instead of blocking on the real stdin or opening a browser for [w].
func feedStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig; r.Close() })
}

func setStdinIsTerminal(t *testing.T, v bool) {
	t.Helper()
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return v }
	t.Cleanup(func() { stdinIsTerminal = orig })
}

func bootstrapCmd() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

func bootstrapConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Algod.Network = "mainnet"
	return cfg
}

func TestBootstrapWallet_NoTTYRefusesAndCreatesNothing(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)
	setStdinIsTerminal(t, false)

	cmd, _ := bootstrapCmd()
	err := bootstrapWallet(cmd, bootstrapConfig())
	if err == nil {
		t.Fatal("bootstrapWallet with no TTY and no wallet returned nil; want a refusal")
	}
	for _, want := range []string{"zs-proxy wallet login", "zs-proxy wallet import --stdin", "zs-proxy wallet new", "_MNEMONIC"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal doesn't name %q:\n%v", want, err)
		}
	}
	if wallet.DefaultExists() {
		t.Error("a wallet was created despite the refusal")
	}
	// Stronger than DefaultExists: nothing at all may be written under the state
	// dir, not just the wallet metadata.
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state dir %s exists after a refusal (stat err: %v); want nothing created", dir, err)
	}
}

// A phrase the store holds with no wallet.json (a deleted state dir) is not
// overwritten by first-run setup, and the refusal names the way back to it.
func TestBootstrapWallet_OrphanPhraseNamesTheRestore(t *testing.T) {
	isolateWalletState(t)
	orphanPhrase(t)
	feedStdin(t) // [n]: create a new wallet over it
	setStdinIsTerminal(t, true)

	cmd, _ := bootstrapCmd()
	err := bootstrapWallet(cmd, bootstrapConfig())
	if !wallet.IsExists(err) || !strings.Contains(err.Error(), "wallet import --force") {
		t.Errorf("setup over an orphan phrase = %v, want ErrExists naming `wallet import --force`", err)
	}
}

// orphanPhrase leaves a phrase in the isolated store with no wallet.json
// naming it, as a deleted state dir does.
func orphanPhrase(t *testing.T) {
	t.Helper()
	w, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Generate("mainnet", false); err != nil {
		t.Fatal(err)
	}
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "wallet.json")); err != nil {
		t.Fatal(err)
	}
}

// A first-run import on Solana refuses a 25-word phrase before storing it,
// since it has no Solana key.
func TestFinishBootstrapImport_SolanaRefusesAlgorandMnemonic(t *testing.T) {
	isolateWalletState(t)
	w, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := bootstrapCmd()
	if err := finishBootstrapImport(cmd, w, phrase, "solana-devnet"); err == nil || !strings.Contains(err.Error(), "no Solana key") {
		t.Fatalf("err %v, want the 25-word refusal", err)
	}
	if wallet.DefaultExists() {
		t.Error("the refused phrase was stored")
	}
}

// `wallet import` refuses a 25-word phrase on a Solana network before storing
// it, and reports a 24-word one by its Solana address there.
func TestWalletImport_SolanaNetwork(t *testing.T) {
	isolateWalletState(t)
	algoPhrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	run := func(phrase string) (string, error) {
		file := filepath.Join(t.TempDir(), "phrase")
		if err := os.WriteFile(file, []byte(phrase), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := newWalletImportCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--file", file, "--network", "solana-devnet", "--yes"})
		err := cmd.Execute()
		return out.String(), err
	}

	if _, err := run(algoPhrase); err == nil || !strings.Contains(err.Error(), "no Solana key") {
		t.Fatalf("25-word: err %v, want the no-Solana-key refusal", err)
	}
	if wallet.DefaultExists() {
		t.Fatal("the refused phrase was stored")
	}

	sol, err := wallet.DeriveSolanaAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	algo, _, err := wallet.DeriveAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(abandonArt)
	if err != nil {
		t.Fatalf("24-word: %v", err)
	}
	if !strings.Contains(out, sol) || strings.Contains(out, algo) {
		t.Errorf("success line %q: want the Solana address %s, not the Algorand one", out, sol)
	}
}

// stubWalletLogin hands the login paths phrase in place of the browser.
func stubWalletLogin(t *testing.T, phrase string) {
	t.Helper()
	orig := walletLoginPhrase
	walletLoginPhrase = func(*cobra.Command, string, bool) (string, error) { return phrase, nil }
	t.Cleanup(func() { walletLoginPhrase = orig })
}

// `wallet login` refuses a 25-word phrase on a Solana network before storing
// it, and reports a 24-word one by its Solana address there.
func TestWalletLogin_SolanaNetwork(t *testing.T) {
	isolateWalletState(t)
	run := func(phrase string) (string, error) {
		stubWalletLogin(t, phrase)
		cmd := newWalletLoginCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--network", "solana-devnet", "--yes"})
		err := cmd.Execute()
		return out.String(), err
	}
	algoPhrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(algoPhrase); err == nil || !strings.Contains(err.Error(), "no Solana key") {
		t.Fatalf("25-word: err %v, want the no-Solana-key refusal", err)
	}
	if wallet.DefaultExists() {
		t.Fatal("the refused phrase was stored")
	}
	assertSolanaLine(t, func() (string, error) { return run(abandonArt) })
}

// First-run setup names the Solana address on a Solana network, whether it
// imports a phrase or creates one.
func TestBootstrapWallet_SolanaNetworkNamesTheSolanaAddress(t *testing.T) {
	t.Run("import", func(t *testing.T) {
		isolateWalletState(t)
		w, err := wallet.OpenDefault()
		if err != nil {
			t.Fatal(err)
		}
		assertSolanaLine(t, func() (string, error) {
			cmd, out := bootstrapCmd()
			err := finishBootstrapImport(cmd, w, abandonArt, "solana-devnet")
			return out.String(), err
		})
	})
	t.Run("new", func(t *testing.T) {
		isolateWalletState(t)
		feedStdin(t) // answers [n]
		setStdinIsTerminal(t, true)
		cfg := &config.Config{}
		cfg.Solana.Cluster = "devnet"
		cmd, out := bootstrapCmd()
		if err := bootstrapWallet(cmd, cfg); err != nil {
			t.Fatal(err)
		}
		assertNamesStoredSolanaAddress(t, out.String())
	})
}

// `wallet new` on a Solana network names the account it will pay from.
func TestWalletNew_SolanaNetworkNamesTheSolanaAddress(t *testing.T) {
	isolateWalletState(t)
	cmd := newWalletNewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--network", "solana-devnet"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	assertNamesStoredSolanaAddress(t, out.String())
}

// assertSolanaLine runs an import of abandonArt and checks its success line
// names the Solana address, not the Algorand one.
func assertSolanaLine(t *testing.T, run func() (string, error)) {
	t.Helper()
	sol, err := wallet.DeriveSolanaAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	algo, _, err := wallet.DeriveAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err != nil {
		t.Fatalf("24-word: %v", err)
	}
	if !strings.Contains(out, sol) || strings.Contains(out, algo) {
		t.Errorf("output %q: want the Solana address %s, not the Algorand one", out, sol)
	}
}

// assertNamesStoredSolanaAddress checks out names the stored wallet's Solana
// address and not its Algorand one.
func assertNamesStoredSolanaAddress(t *testing.T, out string) {
	t.Helper()
	meta, err := wallet.DefaultMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta.SolanaAddress == "" || meta.SolanaAddress == meta.Address {
		t.Fatalf("stored wallet has no distinct Solana address: %+v", meta)
	}
	if !strings.Contains(out, meta.SolanaAddress) || strings.Contains(out, meta.Address) {
		t.Errorf("output %q: want the Solana address %s, not %s", out, meta.SolanaAddress, meta.Address)
	}
}

// An import confirms the address the network's rail pays from: the Solana one
// on a Solana network, the Algorand one otherwise.
func TestConfirmTarget_ByRail(t *testing.T) {
	algo, _, err := wallet.DeriveAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	sol, err := wallet.DeriveSolanaAddress(abandonArt)
	if err != nil {
		t.Fatal(err)
	}
	if algo == sol {
		t.Fatal("the fixture's two addresses are equal; the test can't tell them apart")
	}
	for network, want := range map[string]string{
		"mainnet": algo, "testnet": algo, "solana-devnet": sol, "solana-localnet": sol,
	} {
		if got, err := confirmTarget(abandonArt, network, algo); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", network, got, err, want)
		}
	}
	phrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := confirmTarget(phrase, "solana-devnet", "ALGO"); err == nil {
		t.Error("a 25-word phrase confirmed on Solana; want the no-Solana-key refusal")
	}
	if got, err := confirmTarget(phrase, "mainnet", "ALGO"); err != nil || got != "ALGO" {
		t.Errorf("25-word on mainnet: %q, %v", got, err)
	}
}

// The keys serve loads are the configured rail's: Solana's for a Solana
// config, the Algorand keystore otherwise, never both.
func TestPayerKeyOptions_ByRail(t *testing.T) {
	isolateWalletState(t)
	t.Setenv("ZSTEST_MNEMONIC", abandonArt)

	sol, err := payerKeyOptions(t.Context(), &config.Config{Solana: config.SolanaConfig{Cluster: "devnet"}})
	if err != nil || sol.SolanaKeys == nil || sol.Keystore != nil {
		t.Errorf("Solana config: solana %v, algorand %v, err %v; want the Solana keys alone", sol.SolanaKeys != nil, sol.Keystore != nil, err)
	}
	algo, err := payerKeyOptions(t.Context(), &config.Config{})
	if err != nil || algo.Keystore == nil || algo.SolanaKeys != nil {
		t.Errorf("Algorand config: solana %v, algorand %v, err %v; want the keystore alone", algo.SolanaKeys != nil, algo.Keystore != nil, err)
	}
}

// A 25-word wallet has no Solana key; the refusal is the one that names the
// backup before a replace. Any other failure keeps its own error.
func TestBuildSolanaKeystore_25WordWalletRefusal(t *testing.T) {
	isolateWalletState(t)
	w, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Import(phrase, "testnet", false); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSolanaKeystore(t.Context()); !errors.Is(err, wallet.ErrNoSolanaKey) {
		t.Errorf("25-word wallet: %v, want ErrNoSolanaKey", err)
	}

	// Beside an env 24-word phrase it pays with that.
	t.Setenv("ZSTEST_MNEMONIC", abandonArt)
	if sk, err := buildSolanaKeystore(t.Context()); err != nil || sk == nil {
		t.Errorf("25-word wallet beside an env phrase: %v", err)
	}
	// Beside a malformed env phrase it fails with that phrase's error, not
	// the advice to replace the wallet.
	t.Setenv("ZSTEST_MNEMONIC", strings.Repeat("abandon ", 11)+"zoo")
	if _, err := buildSolanaKeystore(t.Context()); err == nil || errors.Is(err, wallet.ErrNoSolanaKey) {
		t.Errorf("25-word wallet beside a malformed env phrase: %v, want the phrase's own error", err)
	}
	os.Unsetenv("ZSTEST_MNEMONIC")

	// A 24-word wallet whose store won't open fails with the store's error,
	// not the 25-word advice.
	if _, err := w.Import(abandonArt, "testnet", true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEROSIGNAL_KEYSTORE_PASSPHRASE", "not-the-passphrase")
	if _, err := buildSolanaKeystore(t.Context()); err == nil || errors.Is(err, wallet.ErrNoSolanaKey) {
		t.Errorf("24-word wallet that won't open: %v, want its own error", err)
	}
}

// The address a user is told to fund is the one the network's rail pays from.
func TestRailAddress(t *testing.T) {
	m := wallet.Metadata{Address: "ALGO", SolanaAddress: "SOL"}
	if got := railAddress("solana-devnet", m); got != "SOL" {
		t.Errorf("Solana network: %q", got)
	}
	if got := railAddress("testnet", m); got != "ALGO" {
		t.Errorf("Algorand network: %q", got)
	}
	if got := railAddress("solana-devnet", wallet.Metadata{Address: "ALGO"}); got != "ALGO" {
		t.Errorf("nothing recorded: %q", got)
	}
}

func TestPayerAddress(t *testing.T) {
	m := wallet.Metadata{Address: "ALGO", SolanaAddress: "SOL"}
	if got, err := payerAddress(&config.Config{Solana: config.SolanaConfig{Cluster: "devnet"}}, m); err != nil || got != "SOL" {
		t.Errorf("Solana config: %q, %v", got, err)
	}
	if got, err := payerAddress(&config.Config{}, m); err != nil || got != "ALGO" {
		t.Errorf("Algorand config: %q, %v", got, err)
	}
}

// An Algorand-reading command says what's wrong on a Solana network rather
// than failing on the empty escrow app id.
func TestOpenRuntime_RefusesSolana(t *testing.T) {
	isolateWalletState(t)
	f := proxyFlags{network: "solana-devnet"}
	if _, err := f.openRuntime(t.Context()); err == nil || !strings.Contains(err.Error(), "solana-devnet is a Solana network") {
		t.Errorf("err %v", err)
	}
}

func TestWalletOptIn_Solana(t *testing.T) {
	isolateWalletState(t)
	var out bytes.Buffer
	cmd := newWalletOptInCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--network", "solana-devnet"})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "no opt-in on Solana") {
		t.Errorf("out %q, err %v", out.String(), err)
	}
}

// wallet show's Solana line for each kind of wallet: recorded, an older
// wallet that hasn't recorded it, and a 25-word one that has none.
func TestWalletShow_SolanaLine(t *testing.T) {
	isolateWalletState(t)
	show := func() string {
		t.Helper()
		var out bytes.Buffer
		cmd := newWalletShowCmd()
		cmd.SetOut(&out)
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	w, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	m, err := w.Import(abandonArt, "testnet", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := show(); !strings.Contains(got, "solana:  "+m.SolanaAddress+"\n") {
		t.Errorf("recorded:\n%s", got)
	}

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
	raw, _ = json.Marshal(fields)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := show(); !strings.Contains(got, "solana:  (not recorded yet") {
		t.Errorf("not recorded:\n%s", got)
	}

	phrase, err := mnemonic.FromPrivateKey(crypto.GenerateAccount().PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Import(phrase, "testnet", true); err != nil {
		t.Fatal(err)
	}
	if got := show(); !strings.Contains(got, "solana:  (none: a 25-word") {
		t.Errorf("25-word:\n%s", got)
	}
}

const abandonArt = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
	"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

// Every way to make a wallet over an orphan phrase names the restore: the
// first-run import, and `wallet import` and `wallet new` themselves.
func TestOrphanPhrase_EveryCreatePathNamesTheRestore(t *testing.T) {
	isolateWalletState(t)
	orphanPhrase(t)
	other, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	phrase := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon " +
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"
	cmd, _ := bootstrapCmd()
	if err := finishBootstrapImport(cmd, other, phrase, "mainnet"); !wallet.IsExists(err) || !strings.Contains(err.Error(), "wallet import --force") {
		t.Errorf("first-run import over an orphan = %v, want the restore named", err)
	}

	newCmd := newWalletNewCmd()
	newCmd.SetOut(io.Discard)
	newCmd.SetArgs(nil)
	if err := newCmd.Execute(); !wallet.IsExists(err) || !strings.Contains(err.Error(), "wallet import --force") {
		t.Errorf("wallet new over an orphan = %v, want the restore named", err)
	}
}

// Over a described wallet, `wallet new` names the flag that replaces it.
func TestWalletNew_OverAWalletNamesForce(t *testing.T) {
	isolateWalletState(t)
	for i := range 2 {
		c := newWalletNewCmd()
		c.SetOut(io.Discard)
		c.SetArgs(nil)
		err := c.Execute()
		switch {
		case i == 0 && err != nil:
			t.Fatal(err)
		case i == 1 && (!wallet.IsExists(err) || !strings.Contains(err.Error(), "--force")):
			t.Errorf("second wallet new = %v, want ErrExists naming --force", err)
		}
	}
}

// show and address read wallet.json alone; show names the recorded backend,
// and both say how to make a wallet when there is none.
func TestWalletShowAndAddress(t *testing.T) {
	isolateWalletState(t)
	for _, c := range []*cobra.Command{newWalletShowCmd(), newWalletAddressCmd()} {
		c.SetOut(io.Discard)
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no wallet yet") {
			t.Errorf("%s with no wallet = %v, want the no-wallet pointer", c.Use, err)
		}
	}
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wallet.json"),
		[]byte(`{"address":"ADDR","network":"mainnet","backend":"no-such-backend"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	show := newWalletShowCmd()
	var out bytes.Buffer
	show.SetOut(&out)
	if err := show.Execute(); err != nil || !strings.Contains(out.String(), "address: ADDR") || !strings.Contains(out.String(), "backend: no-such-backend") {
		t.Errorf("show = %q, %v; want the address and the recorded backend", out.String(), err)
	}
}

func TestBootstrapWallet_NoTTYWithEnvMnemonicProceeds(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)
	setStdinIsTerminal(t, false)
	t.Setenv("TEST_PAYER_MNEMONIC", "present")

	cmd, _ := bootstrapCmd()
	if err := bootstrapWallet(cmd, bootstrapConfig()); err != nil {
		t.Fatalf("an env signer must bypass setup entirely, got: %v", err)
	}
	if wallet.DefaultExists() {
		t.Error("a wallet was created although an env signer was configured")
	}
}

func TestBootstrapWallet_TTYStillPrompts(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t) // answers [n]
	setStdinIsTerminal(t, true)

	cmd, out := bootstrapCmd()
	if err := bootstrapWallet(cmd, bootstrapConfig()); err != nil {
		t.Fatalf("bootstrapWallet on a TTY answering [n]: %v", err)
	}
	if !strings.Contains(out.String(), "No wallet found") {
		t.Errorf("no setup prompt shown on a TTY; output:\n%s", out.String())
	}
	if !wallet.DefaultExists() {
		t.Error("an explicit [n] on a TTY didn't create a wallet")
	}
}

func TestInstallService_RefusesWithoutWallet(t *testing.T) {
	if runtime.GOOS == "windows" || !service.Supported() {
		// The Windows path would reach the SCM if the check regressed.
		t.Skip("needs a file-based (launchd/systemd) service manager")
	}
	isolateWalletState(t)
	feedStdin(t)

	cmd, _ := bootstrapCmd()
	err := runInstallService(cmd, &proxyFlags{}, false, "", false)
	if err == nil || !strings.Contains(err.Error(), "zs-proxy wallet login") {
		t.Fatalf("install-service with no wallet: got %v, want the no-wallet refusal", err)
	}
	if strings.Contains(err.Error(), "--print") {
		t.Errorf("refusal claims a shell env mnemonic when none is set:\n%v", err)
	}
	unit, uerr := service.UnitPath()
	if uerr != nil {
		t.Fatal(uerr)
	}
	if _, serr := os.Stat(unit); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("unit %s was written despite the refusal (stat err: %v)", unit, serr)
	}
}

// An env mnemonic in the installing shell must NOT satisfy the check: the unit
// carries none of this shell's environment, so the service would start with no
// signer and restart-loop on the refusal the check exists to surface.
func TestInstallService_EnvMnemonicInShellStillRefuses(t *testing.T) {
	if runtime.GOOS == "windows" || !service.Supported() {
		t.Skip("needs a file-based (launchd/systemd) service manager")
	}
	isolateWalletState(t)
	feedStdin(t)
	t.Setenv("TEST_PAYER_MNEMONIC", "present")

	cmd, _ := bootstrapCmd()
	err := runInstallService(cmd, &proxyFlags{}, false, "", false)
	if err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("install-service with only a shell env mnemonic: got %v, want the refusal pointing at --print", err)
	}
	unit, uerr := service.UnitPath()
	if uerr != nil {
		t.Fatal(uerr)
	}
	if _, serr := os.Stat(unit); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("unit %s was written despite the refusal (stat err: %v)", unit, serr)
	}
}

// The UAC-elevated child may run as a different (admin) account with no wallet,
// so it must not re-run the check the unelevated parent already passed. Run on
// darwin/linux, where uacRelaunch reaches the same path: the forced file backend
// means Install only writes the unit (enable=false) and never calls
// launchctl/systemctl.
func TestInstallService_UACRelaunchSkipsWalletCheck(t *testing.T) {
	if runtime.GOOS == "windows" || !service.Supported() {
		t.Skip("on Windows this path would reach the real SCM")
	}
	isolateWalletState(t)
	feedStdin(t)

	cmd, _ := bootstrapCmd()
	if err := runInstallService(cmd, &proxyFlags{}, false, "", true); err != nil {
		t.Fatalf("install-service on the UAC relaunch refused: %v", err)
	}
	unit, err := service.UnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("unit %s not written on the UAC relaunch: %v", unit, err)
	}
}

func TestServiceWalletCheckApplies(t *testing.T) {
	current := func() (string, error) { return `DESKTOP\alice`, nil }
	failing := func() (string, error) { return "", errors.New("no token") }
	for _, tc := range []struct {
		name           string
		goos, account  string
		uacRelaunch    bool
		currentAccount func() (string, error)
		want           bool
	}{
		{"unix always checks", "darwin", "", false, current, true},
		{"unix ignores the Windows-only flag", "linux", `DESKTOP\bob`, false, current, true},
		{"windows, default account", "windows", "", false, current, true},
		{"windows, flag names the current user", "windows", `desktop\ALICE`, false, current, true},
		{"windows, flag names someone else", "windows", `DESKTOP\bob`, false, current, false},
		{"windows, current user unknown", "windows", `DESKTOP\alice`, false, failing, false},
		{"UAC relaunch never checks", "windows", `DESKTOP\alice`, true, current, false},
		{"UAC relaunch on unix never checks", "darwin", "", true, current, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceWalletCheckApplies(tc.goos, tc.account, tc.uacRelaunch, tc.currentAccount); got != tc.want {
				t.Errorf("serviceWalletCheckApplies(%q, %q, %v) = %v, want %v", tc.goos, tc.account, tc.uacRelaunch, got, tc.want)
			}
		})
	}
}

// The keystore skips an empty *_MNEMONIC, so counting one as a signer would skip
// the guided refusal only to fail later on zero loaded signers.
func TestHasEnvMnemonic_EmptyValuesDoNotCount(t *testing.T) {
	isolateWalletState(t)
	t.Setenv("TEST_PAYER_MNEMONIC", " \t")
	t.Setenv(keystore.MnemonicURLsEnv, "  ")
	if hasEnvMnemonic() {
		t.Error("blank *_MNEMONIC / ZS_MNEMONIC_URLS counted as a signer")
	}
	t.Setenv("TEST_PAYER_MNEMONIC", "word1 word2")
	if !hasEnvMnemonic() {
		t.Error("a non-empty *_MNEMONIC wasn't counted")
	}
	t.Setenv("TEST_PAYER_MNEMONIC", "")
	t.Setenv(keystore.MnemonicURLsEnv, "signing=awssecretsmanager://x")
	if !hasEnvMnemonic() {
		t.Error("a non-empty ZS_MNEMONIC_URLS wasn't counted")
	}
}

// generateWallet creates a keychain (file-backend, under the isolated state dir)
// wallet, as `wallet new` would.
func generateWallet(t *testing.T) {
	t.Helper()
	w, err := wallet.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Generate("mainnet", false); err != nil {
		t.Fatal(err)
	}
}

// Every launchd/systemd service start and scripted `proxy start --foreground`
// is a no-TTY start with an existing wallet; refusing it would restart-loop
// the service.
func TestBootstrapWallet_NoTTYWithExistingWalletProceeds(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)
	setStdinIsTerminal(t, false)
	generateWallet(t)

	cmd, out := bootstrapCmd()
	if err := bootstrapWallet(cmd, bootstrapConfig()); err != nil {
		t.Fatalf("no-TTY start with a wallet refused: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no-TTY start with a wallet printed setup output:\n%s", out.String())
	}
}

func TestBootstrapWallet_NoTTYWithMnemonicURLsProceeds(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)
	setStdinIsTerminal(t, false)
	t.Setenv(keystore.MnemonicURLsEnv, "signing=awssecretsmanager://x")

	cmd, _ := bootstrapCmd()
	if err := bootstrapWallet(cmd, bootstrapConfig()); err != nil {
		t.Fatalf("a ZS_MNEMONIC_URLS signer must bypass setup, got: %v", err)
	}
	if wallet.DefaultExists() {
		t.Error("a wallet was created although ZS_MNEMONIC_URLS was set")
	}
}

// The production detector, not the test override: a service's stdin is never a
// terminal, and misreading it as one would run the browser-login listener
// inside a service.
func TestStdinIsTerminal_PipeIsNotATerminal(t *testing.T) {
	feedStdin(t)
	if stdinIsTerminal() {
		t.Error("stdinIsTerminal() = true for a pipe")
	}
}

func TestFundHint(t *testing.T) {
	for _, tc := range []struct {
		network    string
		wantWebApp bool
	}{
		{"mainnet", true},
		{"testnet", false},
		{"localnet", false},
		{"", false},
	} {
		got := fundHint(tc.network)
		if strings.Contains(got, build.ClientURL) != tc.wantWebApp {
			t.Errorf("fundHint(%q) = %q; names the web app = %v, want %v", tc.network, got, !tc.wantWebApp, tc.wantWebApp)
		}
		if !strings.Contains(got, "zs-proxy fund") {
			t.Errorf("fundHint(%q) = %q; want it to name `zs-proxy fund`", tc.network, got)
		}
	}
}

// The Windows SCM path (allowBootstrap=false) refuses through serveWithContext;
// reachable on any OS because the refusal returns before app.Run.
func TestServeWithContext_ServiceModeRefusesWithoutWallet(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)

	cmd, _ := bootstrapCmd()
	err := (&proxyFlags{}).serveWithContext(cmd, t.Context(), false, nil)
	if err == nil || !strings.Contains(err.Error(), "for the service") {
		t.Fatalf("service-mode serve with no wallet: got %v, want the service refusal", err)
	}
	if strings.Contains(err.Error(), "--print") {
		t.Errorf("service refusal claims a shell env mnemonic:\n%v", err)
	}
	if wallet.DefaultExists() {
		t.Error("service-mode serve created a wallet")
	}
}

func TestInstallService_ProceedsWithWallet(t *testing.T) {
	if runtime.GOOS == "windows" || !service.Supported() {
		t.Skip("on Windows this path would reach the real SCM")
	}
	isolateWalletState(t)
	feedStdin(t)
	generateWallet(t)

	cmd, _ := bootstrapCmd()
	if err := runInstallService(cmd, &proxyFlags{}, false, "", false); err != nil {
		t.Fatalf("install-service with a wallet refused: %v", err)
	}
	unit, err := service.UnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("unit %s not written: %v", unit, err)
	}
}

// A refused install must leave a running daemon alone. With the keychain
// backend (the release default) install-service stops the daemon before
// installing; if the wallet check came after that, a refusal would stop the
// daemon and then install nothing, leaving no proxy serving at all.
func TestInstallService_RefusalLeavesRunningDaemonAlone(t *testing.T) {
	if runtime.GOOS == "windows" || !service.Supported() {
		t.Skip("uses a unix child process as the stand-in daemon")
	}
	isolateWalletState(t)
	feedStdin(t)
	t.Setenv("ZEROSIGNAL_KEYRING_BACKEND", "keychain") // any non-file backend: never opened on this path
	t.Setenv("TEST_PAYER_MNEMONIC", "present")         // the daemon's signer, which the service can't see

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap the child the moment it exits. An unreaped zombie still answers
	// kill(pid, 0), so daemon.Alive can't tell a stopped daemon from a live one.
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-exited })
	if err := daemon.Write(daemon.Record{PID: child.Process.Pid, Listen: "127.0.0.1:9376"}); err != nil {
		t.Fatal(err)
	}
	if _, running, _ := daemon.Running(); !running {
		t.Fatal("stand-in daemon not seen as running; the test would prove nothing")
	}

	cmd, _ := bootstrapCmd()
	cmd.SetContext(t.Context()) // as cobra does in production; stopDaemon selects on it
	if err := runInstallService(cmd, &proxyFlags{}, false, "", false); err == nil {
		t.Fatal("install-service with only a shell env mnemonic succeeded; want the refusal")
	}
	select {
	case <-exited:
		t.Error("the refused install stopped the running daemon")
	case <-time.After(200 * time.Millisecond):
	}
}

// The Windows SCM path must still run on an env signer: its guard refuses only
// when there is neither a keychain wallet nor an env mnemonic. The bogus
// mnemonic makes the correct code fail in buildKeystore, before app.Run, so the
// test never starts a server.
func TestServeWithContext_ServiceModeWithEnvMnemonicPassesTheGuard(t *testing.T) {
	isolateWalletState(t)
	feedStdin(t)
	t.Setenv("TEST_PAYER_MNEMONIC", "not a real mnemonic")

	cmd, _ := bootstrapCmd()
	err := (&proxyFlags{}).serveWithContext(cmd, t.Context(), false, nil)
	if err == nil {
		t.Fatal("serveWithContext returned nil; want the keystore error from the bogus mnemonic")
	}
	if strings.Contains(err.Error(), "for the service") {
		t.Errorf("service-mode serve with an env signer hit the no-wallet refusal:\n%v", err)
	}
}

func TestInstallService_PrintNeedsNoWallet(t *testing.T) {
	if !service.Supported() {
		t.Skip("no service manager on this OS")
	}
	isolateWalletState(t)

	cmd, out := bootstrapCmd()
	if err := runInstallService(cmd, &proxyFlags{}, true, "", false); err != nil {
		t.Fatalf("install-service --print with no wallet: %v", err)
	}
	if !strings.Contains(out.String(), "--foreground") {
		t.Errorf("--print didn't render the unit; output:\n%s", out.String())
	}
}
