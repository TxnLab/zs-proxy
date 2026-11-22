/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// stdinIsTerminal is a var so tests can take the no-TTY branch regardless of
// what stdin `go test` hands the binary.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// bootstrapWallet sets up a wallet on a fresh `proxy start` when there is no
// signer at all — neither a zerosignal wallet nor an env mnemonic — by asking
// on the TTY. A user who brings their own env mnemonic is left untouched.
//
// Without a TTY it refuses rather than generating. The no-TTY callers are a
// launchd/systemd service, a script, or a piped run, and in each the user almost
// always meant an account they already funded: a silently generated wallet is
// empty, so every request 402s while the funded account sits unused, and
// nothing on screen says why.
func bootstrapWallet(cmd *cobra.Command, cfg *config.Config) error {
	if wallet.DefaultExists() || hasEnvMnemonic() {
		return nil
	}
	if !stdinIsTerminal() {
		return errNoWallet()
	}
	w, err := wallet.OpenDefault()
	if err != nil {
		return fmt.Errorf("open wallet: %w", err)
	}
	out := cmd.OutOrStdout()
	network := cfg.NetworkName()

	switch chooseWalletSetup(cmd, network) {
	case setupLogin:
		// Browser handoff: sign in to the hosted client with a passkey; the phrase
		// is delivered back over a one-shot loopback listener (see wallet login).
		phrase, err := walletLoginPhrase(cmd, build.ClientURL, false)
		if err != nil {
			return err
		}
		return finishBootstrapImport(cmd, w, phrase, network)
	case setupImport:
		phrase, err := readMnemonic(cmd, "", false)
		if err != nil {
			return err
		}
		return finishBootstrapImport(cmd, w, phrase, network)
	default: // setupNew
		meta, err := w.Generate(network, false)
		if err != nil {
			return keyringHint(fmt.Errorf("create wallet: %w", wallet.WithForceHint(err)))
		}
		fmt.Fprintf(out, "✔ created a new %s wallet: %s\n", meta.Network, railAddress(network, meta))
		fmt.Fprintln(out, "  Back it up now with `zs-proxy wallet export`.")
		return nil
	}
}

// walletSetupCommands names every way to get a keychain wallet, for refusals
// whose caller can't be asked which one they meant. `wallet login` completes
// through a browser on THIS machine (a loopback listener), so the headless
// route is the pipe-in import.
const walletSetupCommands = "" +
	"  zs-proxy wallet login            sign in with your zerosignal.ai passkey in a browser\n" +
	"  zs-proxy wallet import --stdin   pipe in a 24-word recovery phrase (works on a headless machine)\n" +
	"  zs-proxy wallet new              create a fresh, empty wallet"

// errNoWallet is the refusal for a serving entry point that has no signer and
// no TTY to set one up on.
func errNoWallet() error {
	return errors.New("no wallet configured, and no terminal to set one up on. Set one up first, then retry:\n" +
		walletSetupCommands + "\n" +
		"  or set a *_MNEMONIC or ZS_MNEMONIC_URLS environment variable")
}

// errNoServiceWallet is the refusal from install-service and from the Windows
// SCM service when there is no keychain wallet. It omits the env-var route the
// plain refusal offers, because a launchd/systemd unit or SCM service carries
// none of the installing shell's environment — envInShell says one is set
// there, which is exactly the trap worth naming.
func errNoServiceWallet(envInShell bool) error {
	msg := "no wallet configured for the service, and it can't set one up (it has no terminal). Set one up first, then retry:\n" +
		walletSetupCommands
	if envInShell {
		msg += "\nThe *_MNEMONIC / ZS_MNEMONIC_URLS set in this shell won't reach the service. To run the service on one," +
			"\nrender the unit with --print and add the variable to it yourself."
	}
	return errors.New(msg)
}

// finishBootstrapImport stores an obtained phrase (paste or browser handoff),
// the shared tail of the setup's import and login branches.
func finishBootstrapImport(cmd *cobra.Command, w *wallet.Wallet, phrase, network string) error {
	return importPhrase(cmd, w, phrase, network, false, false, "setup aborted — address not confirmed")
}

// isSolanaNetwork is whether network, a config's NetworkName, is on Solana.
func isSolanaNetwork(network string) bool { return strings.HasPrefix(network, "solana-") }

// railAddress is the address meta's phrase pays as on network's rail, the one
// a user funds: a Solana wallet's Algorand address holds nothing it spends.
func railAddress(network string, meta wallet.Metadata) string {
	if isSolanaNetwork(network) && meta.SolanaAddress != "" {
		return meta.SolanaAddress
	}
	return meta.Address
}

// walletSetupChoice is the first-run wallet bootstrap decision.
type walletSetupChoice int

const (
	setupNew    walletSetupChoice = iota // create a fresh wallet (only on an explicit [n])
	setupLogin                           // sign in with the browser (passkey handoff)
	setupImport                          // paste the 24-word phrase
)

// chooseWalletSetup asks how to set up the first wallet: browser sign-in,
// paste-import, or a fresh wallet. The caller guarantees a TTY. Web (browser
// sign-in) is the default — most first-run users already have a web account and
// the passkey handoff is the smoothest way to bring it in (no paste). The hint
// under it exists because the handoff page can only sign in to an existing
// passkey account, never create one.
func chooseWalletSetup(cmd *cobra.Command, network string) walletSetupChoice {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "No wallet found. Set one up for %s:\n", network)
	fmt.Fprintln(out, "  [w] Web     — sign in to the zerosignal web app with your passkey (no paste)")
	fmt.Fprintf(out, "                No account yet? Create one at %s first.\n", build.ClientURL)
	fmt.Fprintln(out, "  [i] Import  — paste the 24-word phrase from the web app (Settings → Recovery)")
	fmt.Fprintln(out, "  [n] New     — create a fresh wallet")
	fmt.Fprint(out, "Choose [W/i/n]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "i", "import":
		return setupImport
	case "n", "new":
		return setupNew
	default: // empty (just Enter) or "w"/"web" → browser sign-in is the default
		return setupLogin
	}
}

// warnIfUnfunded prints actionable funding guidance for a managed (zerosignal)
// wallet that can't pay yet, but never blocks startup — the escrow-config read
// now simulates from the fee sink, so an unfunded payer boots and inference
// returns a 402 wallet_unfunded (see the SDK's runSealed) until funded. This is the
// "always up, tells you what it needs" posture. On Algorand it only returns an
// error for a wallet.json it can't read; a chain it can't reach is left to
// app.Run, as every failure on Solana is. Env-mnemonic deployments (no managed
// wallet) are skipped.
func warnIfUnfunded(cmd *cobra.Command, cfg *config.Config) error {
	if !wallet.DefaultExists() {
		return nil // env-mnemonic / advanced user manages their own funding
	}
	if cfg.OnSolana() {
		warnIfSolanaUnfunded(cmd, cfg)
		return nil
	}
	meta, err := wallet.DefaultMetadata()
	if err != nil {
		return err
	}
	c, err := account.Dial(cfg.Algod.ResolvedEndpoint, cfg.Algod.ResolvedToken)
	if err != nil {
		return nil // can't reach chain to check — let app.Run try
	}
	usdc, err := escrowUSDCAssetID(cmd.Context(), cfg)
	if err != nil {
		return nil // can't read the contract's USDC — let app.Run surface it
	}
	info, err := account.Read(cmd.Context(), c, meta.Address, usdc)
	if err != nil {
		return nil // transient read failure — don't block startup on it
	}
	if info.Funded() && info.OptedInUSDC {
		return nil // ready to serve
	}

	// Not fully funded: warn, but still come up (inference returns 402 until
	// funded). The endpoint is always live; it tells callers what it needs.
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "⚠ wallet %s (%s) is not funded yet — coming up anyway; inference returns 402 wallet_unfunded until funded:\n", meta.Address, cfg.Algod.Network)
	if !info.Funded() {
		fmt.Fprintf(out, "  • needs ALGO — have %s\n", microAlgos(info.MicroAlgos))
	}
	if !info.OptedInUSDC {
		fmt.Fprintln(out, "  • not opted in to USDC")
	}
	fmt.Fprintf(out, "To fund it, %s.\n", fundHint(cfg.Algod.Network))
	return nil
}

// fundHint is the next step for an unfunded wallet. The web app is offered only
// on mainnet: the hosted client is built for one network, so sending a testnet
// payer there would fund the same address on the wrong chain.
func fundHint(network string) string {
	if network == "mainnet" {
		return fmt.Sprintf("add funds in the web app (%s) if this is your web account, "+
			"or run `zs-proxy fund` for a deposit address + QR", build.ClientURL)
	}
	return "run `zs-proxy fund`"
}
