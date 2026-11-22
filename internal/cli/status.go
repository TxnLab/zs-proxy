/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/escrow"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/config"
)

func newStatusCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show wallet, balance, and funding status",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := f.resolveConfig()
			if err != nil {
				return err
			}
			if cfg.OnSolana() {
				return runSolanaStatus(cmd.Context(), cmd.OutOrStdout(), cfg)
			}
			rt, err := f.openRuntime(cmd.Context())
			if err != nil {
				return err
			}
			info, err := account.Read(cmd.Context(), rt.algc, rt.meta.Address, rt.usdc)
			if err != nil {
				return fmt.Errorf("read account: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "address:  %s\n", rt.meta.Address)
			fmt.Fprintf(out, "network:  %s\n", rt.cfg.Algod.Network)
			fmt.Fprintf(out, "ALGO:     %s\n", microAlgos(info.MicroAlgos))
			if info.OptedInUSDC {
				fmt.Fprintf(out, "USDC:     %s\n", microUSDC(info.USDC))
			} else {
				fmt.Fprintln(out, "USDC:     not opted in")
			}
			// Prepaid MBR pool (best-effort; the read simulates from the
			// payer, so it needs a funded account). Show the deposit, slot
			// utilization, and — if the configured working set isn't fully
			// backed — the remaining shortfall so the user knows to top up
			// via `zs fund`.
			if info.Funded() {
				if ps, derr := readPoolStatus(cmd.Context(), rt.cfg, rt.meta.Address); derr == nil {
					fmt.Fprintln(out, poolLine(ps.dep, ps.ticketMbr))
					if short := poolShortfall(slotsOrDefault(rt.cfg.Zs.ConcurrentSlots), ps.ticketMbr, ps.quotaMbrDue, ps.dep); short > 0 {
						fmt.Fprintf(out, "%s%s short of %d slot(s); run `zs-proxy fund`\n",
							poolNoteIndent, microAlgos(short), slotsOrDefault(rt.cfg.Zs.ConcurrentSlots))
					}
					// An open ticket is normally an in-flight request and gone in
					// seconds. One that's past its deadline or frozen is stuck and
					// holding both USDC and an MBR slot, which is worth saying
					// here rather than leaving to a bare count. Only walks the
					// boxes when the counter says there's something to explain.
					if ps.dep.OpenTickets > 0 {
						printStuckTicketNote(cmd.Context(), out, rt)
					}
					// Free-ticket allowance, whenever the deployed app has the
					// quota at all (quotaMbr == 0 means it doesn't — an older
					// contract — so there is nothing to report). Best-effort like
					// the rest of the block: an extra simulate that fails just
					// drops the line. Sender and payer are both this wallet,
					// which info.Funded() has already established.
					if ps.quotaMbr > 0 {
						if al, aerr := ps.esc.ReadFreeAllowance(cmd.Context(), rt.meta.Address, rt.meta.Address); aerr == nil {
							if line := freeQuotaLine(al, ps.quotaMbrDue > 0, time.Now()); line != "" {
								fmt.Fprintln(out, line)
							}
						}
					}
				}
			}
			ready := info.Funded() && info.OptedInUSDC
			if ready {
				fmt.Fprintln(out, "ready:    yes")
			} else {
				fmt.Fprintf(out, "ready:    no — %s\n", fundHint(rt.cfg.Algod.Network))
			}

			printSpendPosture(out, rt.cfg)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

// printSpendPosture is today's actual settled spend (the sum of the operator
// receipt charges, reset at midnight UTC) against the optional daily cap,
// plus the optional per-request ceiling. What's shown is real spend — the
// daily cap additionally counts in-flight reservations when it enforces, but
// those aren't a status concern.
func printSpendPosture(out io.Writer, cfg *config.Config) {
	guard := readSpendGuard(cfg)
	spentToday := float64(guard.SpentToday()) / 1_000_000
	guard.Close()
	if cfg.Spend.DailyCapUSDC > 0 {
		fmt.Fprintf(out, "spend:    $%.2f of $%.2f spent today (daily cap)\n", spentToday, cfg.Spend.DailyCapUSDC)
	} else {
		fmt.Fprintf(out, "spend:    $%.2f spent today (no cap)\n", spentToday)
	}
	if cfg.Spend.PerRequestCapUSDC > 0 {
		fmt.Fprintf(out, "per-req:  $%.2f max per request\n", cfg.Spend.PerRequestCapUSDC)
	} else {
		fmt.Fprintln(out, "per-req:  no limit")
	}
}

// poolNoteIndent aligns a continuation line under the `MBR pool:` block with
// the value column of the labelled lines above it.
const poolNoteIndent = "          "

// poolLine renders the prepaid pool: what's deposited and how much of it is
// currently on loan to live ticket boxes.
//
// The denominator is backedSlots — the slots the DEPOSIT backs, not the
// configured zs.concurrent_slots working set, and shared with `zs slots` so the
// two commands can't disagree about the same pool. The configured target still
// shows up in the shortfall note when the pool is under it.
//
// ticketMbr == 0 means RefreshConfig didn't load it, so no slot count is
// knowable; fall back to the bare open count rather than printing a zero
// denominator.
func poolLine(dep escrow.PayerDeposit, ticketMbr uint64) string {
	if ticketMbr == 0 {
		return fmt.Sprintf("MBR pool: %s deposited, %d ticket(s) open",
			microAlgos(dep.AlgoBalance), dep.OpenTickets)
	}
	return fmt.Sprintf("MBR pool: %s deposited, %d of %d ticket slot(s) in use",
		microAlgos(dep.AlgoBalance), dep.OpenTickets, backedSlots(ticketMbr, dep))
}

// freeQuotaLine renders the payer's remaining free-ticket (maxPrice == 0)
// allowance for the contract's current 24h window. boxPending says the payer
// has no quota box yet — the one-time MBR for it hasn't been drawn from their
// pool, which is worth naming since it's charged once and never refunded.
//
// Returns "" when there's nothing to say (a zero cap can only mean a contract
// this build doesn't understand).
func freeQuotaLine(al escrow.FreeAllowance, boxPending bool, now time.Time) string {
	if al.CapPerDay == 0 {
		return ""
	}
	if al.Remaining >= al.CapPerDay {
		// A full allowance means no window is running (or the last one
		// lapsed), and WindowEndsAt is then the end of the window that WOULD
		// start on the next free open — a real timestamp, but not a deadline
		// the payer is under. Naming it here would read as one.
		line := fmt.Sprintf("free:     %d of %d free tickets available today", al.CapPerDay, al.CapPerDay)
		if boxPending {
			line += " (quota box not yet created)"
		}
		return line
	}
	resets := time.Unix(int64(al.WindowEndsAt), 0)
	return fmt.Sprintf("free:     %d of %d free tickets left today (window resets %s, %s)",
		al.Remaining, al.CapPerDay, resets.Format(time.RFC3339), relTime(resets.Sub(now)))
}
