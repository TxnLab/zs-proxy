/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// runSolanaFund is fund on the Solana rail. Where Algorand opts in to USDC
// and fills the MBR pool, Solana creates the USDC token account and fills the
// ticket rent pool; a test cluster's faucet supplies the SOL, and a localnet
// mint's authority the test USDC.
func runSolanaFund(ctx context.Context, out io.Writer, cfg *config.Config, wait bool, mintAuthority string) error {
	e, err := openSolanaRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	test, err := e.testCluster(ctx)
	if err != nil {
		return err
	}
	if err := checkMintAuthority(mintAuthority, test); err != nil {
		return err
	}
	info, err := e.read(ctx)
	if err != nil {
		return err
	}

	if !info.funded() {
		if test {
			fmt.Fprintf(out, "requesting %s from the cluster's faucet…\n", lamportsSOL(airdropLamports))
			if aerr := e.airdrop(ctx); aerr != nil {
				fmt.Fprintf(out, "airdrop failed (%v) — fund manually:\n", aerr)
				printSolanaFundManual(out, e, test)
			} else {
				fmt.Fprintln(out, "✔ airdropped SOL")
			}
		} else {
			printSolanaFundManual(out, e, test)
			suggestSpendCapIfUnset(out, cfg)
		}
		if info, err = e.read(ctx); err != nil {
			return err
		}
		if !info.funded() {
			if !wait {
				fmt.Fprintln(out, "\nRe-run `zs-proxy fund` (or add --wait) once SOL arrives to create the USDC account.")
				return nil
			}
			if info, err = pollSolanaFunded(ctx, out, e); err != nil {
				return err
			}
		}
	}

	w, err := e.wallet(ctx)
	if err != nil {
		return err
	}
	if !info.usdcAccount {
		fmt.Fprintln(out, "creating the USDC token account…")
		sig, err := w.EnsureUSDCAccount(ctx)
		if err != nil {
			return fmt.Errorf("create the USDC token account: %w", err)
		}
		if sig != "" {
			fmt.Fprintf(out, "✔ created the USDC token account %s (%s)\n", w.USDCAccount(), sig)
		}
	}
	if mintAuthority != "" {
		sig, amount, err := e.mintTestUSDC(ctx, mintAuthority)
		if err != nil {
			return fmt.Errorf("mint test USDC: %w", err)
		}
		fmt.Fprintf(out, "✔ minted %s (%s)\n", tokenUnits(amount, e.conf.USDCDecimals, "USDC"), sig)
	}

	slots := slotsOrDefault(cfg.Zs.ConcurrentSlots)
	if ps, perr := e.pool(ctx); perr == nil {
		if line := rentPoolFundingLine(slots, ps); line != "" {
			fmt.Fprintln(out, line)
		}
	}
	sig, deposited, err := w.RentPool().EnsureDeposit(ctx, slots)
	switch {
	case errors.Is(err, zs.ErrRentDepositBelowTarget):
		fmt.Fprintf(out, "✔ deposited %s into the ticket rent pool (%s)\n", lamportsSOL(deposited), sig)
		fmt.Fprintf(out, "⚠ the rent pool is still below the configured working set — %v\n", err)
	case err != nil:
		return fmt.Errorf("fund the ticket rent pool: %w", err)
	case sig == "":
		fmt.Fprintln(out, "✔ ticket rent pool already funded")
	default:
		fmt.Fprintf(out, "✔ deposited %s into the ticket rent pool for %d concurrent ticket slot(s) (%s)\n", lamportsSOL(deposited), slots, sig)
	}

	if info, err = e.read(ctx); err == nil && info.usdc == 0 && mintAuthority == "" {
		fmt.Fprintf(out, "⚠ the USDC account holds no USDC: send USDC (mint %s) to %s\n", e.d.Mint, e.payer)
	}
	fmt.Fprintln(out, "✔ wallet ready")
	return nil
}

// checkMintAuthority refuses a mint authority off a test cluster: a real
// mint's authority is no one's to hand the CLI.
func checkMintAuthority(mintAuthority string, testCluster bool) error {
	if mintAuthority != "" && !testCluster {
		return errors.New("--mint-authority mints test USDC, on devnet or a local validator only")
	}
	return nil
}

// rentPoolFundingLine is Algorand fund's "funding MBR pool" line: what the
// top-up will take before it is sent, the pool's own rent included on a
// first deposit, which deposit_rent charges on top of the slots.
func rentPoolFundingLine(slots uint64, ps zs.SolanaRentPoolStatus) string {
	short := ps.Shortfall(slots)
	if short == 0 {
		return ""
	}
	if !ps.Exists {
		return fmt.Sprintf("funding the ticket rent pool: depositing up to %s for %d slot(s) (first deposit: plus %s for the pool's own rent, returned on close-deposit)",
			lamportsSOL(short), slots, lamportsSOL(ps.PoolRent))
	}
	return fmt.Sprintf("funding the ticket rent pool: depositing up to %s for %d slot(s)", lamportsSOL(short), slots)
}

// pollSolanaFunded is pollFunded on Solana.
func pollSolanaFunded(ctx context.Context, out io.Writer, e *solanaEnv) (solanaInfo, error) {
	fmt.Fprintln(out, "waiting for SOL… (Ctrl-C to stop)")
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		info, err := e.read(ctx)
		if err == nil && info.funded() {
			fmt.Fprintf(out, "✔ funded: %s\n", lamportsSOL(info.lamports))
			return info, nil
		}
		select {
		case <-ctx.Done():
			return solanaInfo{}, ctx.Err()
		case <-t.C:
		}
	}
}
