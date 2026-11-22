/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// newWalletCmd is the `zs-proxy wallet` group.
func newWalletCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wallet",
		Short: "Manage the local Algorand wallet",
	}
	cmd.AddCommand(
		newWalletShowCmd(),
		newWalletAddressCmd(),
		newWalletNewCmd(),
		newWalletImportCmd(),
		newWalletLoginCmd(),
		newWalletExportCmd(),
		newWalletOptInCmd(),
	)
	return cmd
}

func newWalletOptInCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "opt-in",
		Short:        "Opt the account in to USDC (idempotent; needs ALGO for the fee)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := f.resolveConfig()
			if err != nil {
				return err
			}
			if cfg.OnSolana() {
				// Solana has no opt-in: a token account is created for the
				// mint, which fund does.
				fmt.Fprintln(cmd.OutOrStdout(), "no opt-in on Solana: `zs-proxy fund` creates the USDC token account")
				return nil
			}
			rt, err := f.openRuntime(ctx)
			if err != nil {
				return err
			}
			ks, err := buildKeystore(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			txid, err := account.OptInUSDC(ctx, rt.algc, ks, rt.meta.Address, rt.usdc)
			if err != nil {
				return err
			}
			if txid == "" {
				fmt.Fprintln(out, "already opted in to USDC")
			} else {
				fmt.Fprintf(out, "✔ opted in to USDC (%s)\n", txid)
			}
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

func newWalletShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "show",
		Short:        "Show the wallet address, network, and source",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := defaultMetadata()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "address: %s\n", m.Address)
			// The same phrase pays on Solana as another address. An older
			// wallet hasn't recorded it, and showing needs no unlock.
			switch {
			case m.SolanaAddress != "":
				fmt.Fprintf(out, "solana:  %s\n", m.SolanaAddress)
			case m.Kind == wallet.KindAlgorand25:
				fmt.Fprintln(out, "solana:  (none: a 25-word Algorand mnemonic has no Solana key)")
			default:
				fmt.Fprintln(out, "solana:  (not recorded yet: `zs-proxy wallet address --network solana-devnet` derives it)")
			}
			fmt.Fprintf(out, "network: %s\n", m.Network)
			fmt.Fprintf(out, "kind:    %s\n", m.Kind)
			fmt.Fprintf(out, "source:  %s\n", m.Source)
			if m.Backend != "" {
				fmt.Fprintf(out, "backend: %s\n", m.Backend)
			}
			return nil
		},
	}
}

func newWalletAddressCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "address",
		Short:        "Print the bare wallet address on the configured network's rail (scriptable)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := f.resolveConfig()
			if err != nil {
				return err
			}
			m, err := defaultMetadata()
			if err != nil {
				return err
			}
			addr, err := payerAddress(cfg, m)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), addr)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

// payerAddress is the address m's phrase pays as on cfg's rail.
func payerAddress(cfg *config.Config, m wallet.Metadata) (string, error) {
	if cfg.OnSolana() {
		return wallet.SolanaAddress(m)
	}
	return m.Address, nil
}

func newWalletNewCmd() *cobra.Command {
	var (
		network string
		force   bool
	)
	cmd := &cobra.Command{
		Use:          "new",
		Short:        "Generate a fresh 24-word wallet (importable into the client)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := wallet.OpenDefault()
			if err != nil {
				return err
			}
			meta, err := w.Generate(network, force)
			if err != nil {
				return keyringHint(wallet.WithForceHint(err))
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✔ generated wallet (%s)\n  %s\n", meta.Network, railAddress(network, meta))
			fmt.Fprintln(out, "Back it up now: `zs-proxy wallet export` reveals the recovery phrase.")
			return nil
		},
	}
	cmd.Flags().StringVar(&network, "network", build.DefaultNetwork, "network this wallet is for")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing wallet")
	return cmd
}

func newWalletImportCmd() *cobra.Command {
	var (
		network   string
		force     bool
		fromFile  string
		fromStdin bool
		assumeYes bool
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a recovery phrase (24-word client phrase, or 25-word Algorand off Solana)",
		Long: "Import your wallet by pasting the 24-word recovery phrase you exported from\n" +
			"the client (Settings → Recovery). The derived accounts are identical, so the\n" +
			"CLI and the browser app share one account. A 25-word Algorand phrase has no\n" +
			"Solana account, so it is refused on a Solana network.\n\n" +
			"The phrase is read from a hidden prompt by default (never an argv flag, which\n" +
			"would leak into shell history); use --stdin or --file for automation.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := wallet.OpenDefault()
			if err != nil {
				return err
			}
			if w.Exists() && !force {
				return wallet.ErrExists
			}

			phrase, err := readMnemonic(cmd, fromFile, fromStdin)
			if err != nil {
				return err
			}

			return importPhrase(cmd, w, phrase, network, force, assumeYes, "import aborted — address not confirmed")
		},
	}
	cmd.Flags().StringVar(&network, "network", build.DefaultNetwork, "network this wallet is for")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing wallet")
	cmd.Flags().StringVar(&fromFile, "file", "", "read the phrase from a file instead of prompting")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the phrase from stdin (pipe)")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the address-match confirmation")
	return cmd
}

func newWalletExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "export",
		Short:        "Reveal the recovery phrase (guarded)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := wallet.OpenDefault()
			if err != nil {
				return err
			}
			if !w.Exists() {
				return errors.New("no wallet to export")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Your recovery phrase grants full control of this wallet's funds.")
			fmt.Fprintln(out, "Anyone who sees it can spend everything. Reveal only somewhere private.")
			if term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Fprint(out, "Type 'reveal' to continue: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != "reveal" {
					return errors.New("export cancelled")
				}
			}
			phrase, err := w.Export() // triggers any OS keychain prompt
			if err != nil {
				return err
			}
			fmt.Fprintln(out, phrase)
			return nil
		},
	}
}

// defaultMetadata reads the wallet's descriptor alone: show and address need
// no secret, so they work where the recorded backend does not open.
func defaultMetadata() (wallet.Metadata, error) {
	m, err := wallet.DefaultMetadata()
	if errors.Is(err, fs.ErrNotExist) {
		return m, errors.New("no wallet yet — run `zs-proxy wallet login`, `zs-proxy wallet import`, or `zs-proxy wallet new`")
	}
	return m, err
}

// readMnemonic gets the phrase securely: a file, a stdin pipe, or — by
// default — a hidden no-echo terminal prompt. Never an argv flag.
func readMnemonic(cmd *cobra.Command, fromFile string, fromStdin bool) (string, error) {
	switch {
	case fromFile != "":
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case fromStdin:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	default:
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", errors.New("no terminal for interactive paste — use --stdin or --file")
		}
		fmt.Fprint(cmd.ErrOrStderr(), "Paste your recovery phrase (input hidden): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
}

// showOtherRailAddress prints, ahead of confirmAddress's prompt, a 24-word
// phrase's address on the rail the prompt doesn't name. The browser that
// handed the phrase over shows the address of ITS rail, which a per-browser
// setting picks and the /cli URL can't tell it, so either address must confirm.
func showOtherRailAddress(cmd *cobra.Command, phrase, kind, network, algorandAddr string, assumeYes bool) {
	if assumeYes || kind != wallet.KindXHD24 || !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}
	if isSolanaNetwork(network) {
		fmt.Fprintf(cmd.OutOrStdout(), "\nAlgorand account: %s", algorandAddr)
		return
	}
	if sol, err := wallet.DeriveSolanaAddress(phrase); err == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "\nSolana account:  %s", sol)
	}
}

// importPhrase derives and confirms phrase's account, then stores it: the one
// tail `wallet import`, `wallet login` and first-run setup share, so none of
// them can store an unconfirmed phrase, or name the other rail's address as
// the one to fund. Derived BEFORE storing, so a typo'd phrase never touches
// the keychain.
func importPhrase(cmd *cobra.Command, w *wallet.Wallet, phrase, network string, force, assumeYes bool, aborted string) error {
	addr, kind, err := wallet.DeriveAddress(phrase)
	if err != nil {
		return err
	}
	target, err := confirmTarget(phrase, network, addr)
	if err != nil {
		return err
	}
	showOtherRailAddress(cmd, phrase, kind, network, addr, assumeYes)
	if !confirmAddress(cmd, target, assumeYes) {
		return errors.New(aborted)
	}
	meta, err := w.Import(phrase, network, force)
	if err != nil {
		return keyringHint(fmt.Errorf("import wallet: %w", wallet.WithForceHint(err)))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✔ imported wallet (%s)\n  %s\n", meta.Network, railAddress(network, meta))
	return nil
}

// confirmTarget is the address an import asks the user to confirm: the one
// network's rail pays from, which is the one the client shows for that rail.
// A phrase with no Solana account is refused on a Solana network before it is
// stored, since the run would refuse it anyway.
func confirmTarget(phrase, network, algorandAddr string) (string, error) {
	if !isSolanaNetwork(network) {
		return algorandAddr, nil
	}
	return wallet.DeriveSolanaAddress(phrase)
}

// confirmAddress asks the user to verify the derived address matches their
// client before committing. Non-interactive input (pipe) or --yes skips it.
func confirmAddress(cmd *cobra.Command, addr string, assumeYes bool) bool {
	if assumeYes || !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nDerived account: %s\n", addr)
	fmt.Fprint(out, "Does this match the address shown in your client? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
