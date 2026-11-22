/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/TxnLab/zerosignal/go/zs"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// runSolanaStatus is status on the Solana rail: SOL where Algorand shows
// ALGO, the USDC token account where it shows the opt-in, and the ticket
// rent pool where it shows the MBR pool.
func runSolanaStatus(ctx context.Context, out io.Writer, cfg *config.Config) error {
	e, err := openSolanaRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	info, err := e.read(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "address:  %s\n", e.payer)
	fmt.Fprintf(out, "network:  %s\n", cfg.NetworkName())
	fmt.Fprintf(out, "SOL:      %s\n", lamportsSOL(info.lamports))
	switch {
	case !info.usdcAccount:
		fmt.Fprintln(out, "USDC:     no token account")
	case info.usdcFrozen:
		fmt.Fprintf(out, "USDC:     %s (token account frozen)\n", tokenUnits(info.usdc, e.conf.USDCDecimals, "USDC"))
	default:
		fmt.Fprintf(out, "USDC:     %s\n", tokenUnits(info.usdc, e.conf.USDCDecimals, "USDC"))
	}

	slots := slotsOrDefault(cfg.Zs.ConcurrentSlots)
	// Best-effort, as Algorand's pool block is: a failed read drops it.
	ps, perr := e.pool(ctx)
	if perr == nil {
		fmt.Fprintln(out, rentPoolLine(ps))
		if short := ps.Shortfall(slots); short > 0 {
			fmt.Fprintf(out, "%s%s short of %d slot(s); run `zs-proxy fund`\n", poolNoteIndent, lamportsSOL(short), slots)
		}
		if ps.OpenTickets > 0 {
			printSolanaStuckTicketNote(ctx, out, e)
		}
	}

	if solanaReady(info) {
		fmt.Fprintln(out, "ready:    yes")
	} else {
		fmt.Fprintln(out, "ready:    no — run `zs-proxy fund`")
	}
	printSpendPosture(out, cfg)
	return nil
}

// warnIfSolanaUnfunded is warnIfUnfunded on Solana. Like it, any failed read
// stays quiet and leaves the cause to app.Run.
func warnIfSolanaUnfunded(cmd *cobra.Command, cfg *config.Config) {
	e, err := openSolanaRuntime(cmd.Context(), cfg)
	if err != nil {
		return
	}
	info, err := e.read(cmd.Context())
	if err != nil {
		return
	}
	lines := solanaUnfundedLines(info, e.conf.USDCDecimals)
	if len(lines) == 0 {
		return
	}
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "⚠ wallet %s (%s) is not funded yet — coming up anyway; inference returns 402 wallet_unfunded until funded:\n", e.payer, cfg.NetworkName())
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "To fund it, run `zs-proxy fund`.")
}

// solanaUnfundedLines is what keeps a Solana wallet from paying, one line
// each; none when it can. USDC itself isn't counted, as Algorand's warning
// doesn't count it: the node's refusal names a short balance.
func solanaUnfundedLines(i solanaInfo, decimals uint8) []string {
	var lines []string
	if !i.funded() {
		lines = append(lines, fmt.Sprintf("  • needs SOL — have %s", lamportsSOL(i.lamports)))
	}
	switch {
	case !i.usdcAccount:
		lines = append(lines, "  • no USDC token account")
	case i.usdcFrozen:
		lines = append(lines, fmt.Sprintf("  • the USDC token account is frozen (%s)", tokenUnits(i.usdc, decimals, "USDC")))
	}
	return lines
}

// solanaReady is what a Solana payer needs to serve: the fee floor in SOL and
// a USDC account that isn't frozen. The rent pool isn't counted, as
// Algorand's ready doesn't count the MBR pool: the wallet tops it up before
// it signs an open.
func solanaReady(i solanaInfo) bool {
	return i.funded() && i.usdcAccount && !i.usdcFrozen
}

// rentPoolLine is poolLine for the ticket rent pool; the denominator is the
// slots the pool backs, which is what the program admits an open on. It
// counts the live tickets' rent, so unlike Algorand's quotient it is never
// below the count in use.
func rentPoolLine(ps zs.SolanaRentPoolStatus) string {
	if !ps.Exists {
		return "rent pool: none yet — the first request or `zs-proxy fund` creates it"
	}
	return fmt.Sprintf("rent pool: %s free, %d of %d ticket slot(s) in use",
		lamportsSOL(ps.Available()), ps.OpenTickets, ps.BackedSlots())
}

// rentPoolDeposited is the display's "deposited": what the pool holds above
// its own rent plus the rent its live tickets hold, which their endings
// return, as Algorand's AlgoBalance counts the boxes' MBR.
func rentPoolDeposited(ps zs.SolanaRentPoolStatus) uint64 {
	return ps.Available() + ps.OpenTickets*ps.TicketRent
}
