/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
)

func newFundCmd() *cobra.Command {
	var (
		f             proxyFlags
		wait          bool
		mintAuthority string
	)
	cmd := &cobra.Command{
		Use:   "fund",
		Short: "Fund the wallet — testnet auto-dispense, mainnet guided",
		Long: "On testnet, requests free funds from the dispenser (or prints the address and\n" +
			"dispenser link when none is configured). On mainnet, prints the deposit address\n" +
			"+ QR and on-ramp links. Once ALGO has arrived it opts the account in to USDC.\n\n" +
			"On Solana it airdrops SOL on devnet or a local validator, creates the USDC token\n" +
			"account, and fills the ticket rent pool. --mint-authority <keyfile> mints test\n" +
			"USDC with a test mint's solana-keygen keypair.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := f.resolveConfig()
			if err != nil {
				return err
			}
			if cfg.OnSolana() {
				return runSolanaFund(ctx, cmd.OutOrStdout(), cfg, wait, mintAuthority)
			}
			if mintAuthority != "" {
				return errors.New("--mint-authority is for a Solana test cluster")
			}
			rt, err := f.openRuntime(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			info, err := account.Read(ctx, rt.algc, rt.meta.Address, rt.usdc)
			if err != nil {
				return fmt.Errorf("read account: %w", err)
			}

			if !info.Funded() {
				switch rt.cfg.Algod.Network {
				case "testnet":
					if build.TestnetDispenserURL != "" {
						if derr := dripTestnet(ctx, rt.meta.Address); derr != nil {
							fmt.Fprintf(out, "auto-fund failed (%v) — fund manually:\n", derr)
							printTestnetManual(out, rt.meta.Address)
						} else {
							fmt.Fprintln(out, "✔ requested testnet funds from the dispenser")
						}
					} else {
						printTestnetManual(out, rt.meta.Address)
					}
				case "mainnet":
					printMainnetFund(out, rt.meta.Address)
					suggestSpendCapIfUnset(out, rt.cfg)
				default:
					printTestnetManual(out, rt.meta.Address)
				}

				if wait {
					if info, err = pollFunded(ctx, out, rt); err != nil {
						return err
					}
				} else if !info.Funded() {
					fmt.Fprintln(out, "\nRe-run `zs-proxy fund` (or add --wait) once funds arrive to opt in to USDC.")
					return nil
				}
			}

			// Funded → ensure the USDC opt-in + the prepaid MBR pool, both
			// needed before the first open (USDC locks max_price; the pool
			// backs the per-ticket box MBR so open() carries no ALGO leg).
			if info.Funded() {
				ks, err := buildKeystore(ctx)
				if err != nil {
					return err
				}
				if !info.OptedInUSDC {
					fmt.Fprintln(out, "opting in to USDC…")
					if _, err := account.OptInUSDC(ctx, rt.algc, ks, rt.meta.Address, rt.usdc); err != nil {
						return fmt.Errorf("usdc opt-in: %w", err)
					}
					fmt.Fprintln(out, "✔ opted in to USDC")
				}

				w, err := buildDepositWallet(ctx, rt, ks)
				if err != nil {
					return fmt.Errorf("build deposit wallet: %w", err)
				}
				// Surface the prepaid-pool requirement BEFORE submitting so the
				// user sees what the deposit needs — it's a separate, larger,
				// fully-recoverable ALGO requirement on top of the 0.3-ALGO
				// onboarding floor (it backs the per-ticket box MBR so open()
				// carries no ALGO leg).
				if shortfall, firstDeposit, serr := w.DepositShortfall(ctx, rt.cfg.Zs.ConcurrentSlots); serr == nil && shortfall > 0 {
					note := ""
					if firstDeposit {
						note = " (first deposit — also opts the wallet into the escrow app)"
					}
					fmt.Fprintf(out, "funding MBR pool: depositing up to %s for %d concurrent ticket slot(s)%s…\n",
						microAlgos(shortfall), slotsOrDefault(rt.cfg.Zs.ConcurrentSlots), note)
				}
				txid, deposited, err := w.EnsureDeposit(ctx, rt.cfg.Zs.ConcurrentSlots)
				switch {
				case errors.Is(err, zs.ErrDepositBelowTarget):
					// A real deposit landed, but short of the working set: the
					// wallet afforded only some of the slots, or only the
					// free-quota box term. Report it honestly rather than as
					// either a clean success or a hard failure: the pool IS better
					// off, and the sentinel's message names the remaining shortfall.
					fmt.Fprintf(out, "✔ deposited %s into the MBR pool (%s)\n", microAlgos(deposited), txid)
					fmt.Fprintf(out, "⚠ MBR pool is still below the configured working set — %v\n", err)
				case err != nil:
					return fmt.Errorf("ensure MBR deposit: %w", err)
				case txid == "":
					fmt.Fprintln(out, "✔ MBR deposit pool already funded")
				default:
					fmt.Fprintf(out, "✔ deposited %s into the MBR pool (%s)\n", microAlgos(deposited), txid)
				}
			}
			fmt.Fprintln(out, "✔ wallet ready")
			return nil
		},
	}
	f.bindCommon(cmd)
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until ALGO (or SOL) arrives, then opt in to USDC (or create its account)")
	cmd.Flags().StringVar(&mintAuthority, "mint-authority", "", "Solana test clusters: mint test USDC with this mint-authority keypair file")
	return cmd
}

func printTestnetManual(out io.Writer, addr string) {
	fmt.Fprintf(out, "Fund this testnet address (free):\n  %s\n", addr)
	fmt.Fprintln(out, "Dispenser: https://bank.testnet.algorand.network/")
}

func printMainnetFund(out io.Writer, addr string) {
	fmt.Fprintf(out, "Send ALGO (and USDC) to:\n  %s\n\n", addr)
	qrterminal.GenerateHalfBlock(addr, qrterminal.L, out)
	fmt.Fprintln(out, "\nBuy/transfer with Pera Wallet (https://perawallet.app) or any ALGO on-ramp.")
}

// suggestSpendCapIfUnset surfaces the optional spend-cap knob at the mainnet
// onboarding moment (this wallet auto-spends real USDC per request). It is a
// suggestion only — a cap is never required; this just points at the lever at
// the natural time, and stays quiet if one is already set.
func suggestSpendCapIfUnset(out io.Writer, cfg *config.Config) {
	if cfg.Spend.DailyCapUSDC > 0 || cfg.Spend.PerRequestCapUSDC > 0 {
		return
	}
	fmt.Fprintln(out, "\nTip (optional): this wallet auto-spends USDC per request. To bound it, set a")
	fmt.Fprintln(out, "daily cap with `spend.daily_cap_usdc` in config (or PROXY_SPEND_DAILY_CAP_USDC).")
}

// dripTestnet POSTs the address to the configured TxnLab testnet funding
// endpoint. The endpoint itself is an out-of-band dependency; until it exists
// build.TestnetDispenserURL is empty and this is never called.
func dripTestnet(ctx context.Context, addr string) error {
	body, _ := json.Marshal(map[string]string{"address": addr})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, build.TestnetDispenserURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("dispenser %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}

// pollFunded waits (bounded) for the account to reach the ALGO floor,
// re-reading every few seconds until funded or the context ends.
func pollFunded(ctx context.Context, out io.Writer, rt *runtimeEnv) (account.Info, error) {
	fmt.Fprintln(out, "waiting for funds… (Ctrl-C to stop)")
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		info, err := account.Read(ctx, rt.algc, rt.meta.Address, rt.usdc)
		if err == nil && info.Funded() {
			fmt.Fprintf(out, "✔ funded: %s\n", microAlgos(info.MicroAlgos))
			return info, nil
		}
		select {
		case <-ctx.Done():
			return account.Info{}, ctx.Err()
		case <-t.C:
		}
	}
}
