/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/zs"
)

// solanaSlotsView is slotsViewFrom for the rent pool, where a slot is a
// ticket's rent.
func solanaSlotsView(configured uint64, ps zs.SolanaRentPoolStatus) slotsView {
	v := slotsView{
		pool:      solanaRentPool,
		known:     ps.TicketRent != 0,
		deposited: rentPoolDeposited(ps),
		ticketMbr: ps.TicketRent,
		open:      ps.OpenTickets,
	}
	if !v.known {
		return v
	}
	v.backed = ps.BackedSlots()
	v.shortfall = ps.Shortfall(configured)
	v.excess = ps.Excess(configured)
	return v
}

// decideSolanaSlots is decideSlots for the rent pool. A withdraw is bounded by
// Available, as Algorand's is by FreePoolHeadroom: the program refuses more,
// and live tickets hold the rest. A first deposit also pays the pool's own
// rent, which the plan the user consents to counts.
func decideSolanaSlots(current, want uint64, ps zs.SolanaRentPoolStatus, dryRun bool) slotsDecision {
	excess := ps.Excess(want)
	shortfall := ps.Shortfall(want)
	if shortfall > 0 && !ps.Exists {
		shortfall += ps.PoolRent
	}
	return slotsDecisionFor(solanaRentPool, current, want, slotsSizing{
		shortfall:    shortfall,
		excess:       excess,
		withdrawable: min(excess, ps.Available()),
		openTickets:  ps.OpenTickets,
	}, dryRun)
}

func runSolanaSlotsShow(ctx context.Context, cmd *cobra.Command, e *solanaEnv) error {
	configured := slotsOrDefault(e.cfg.Zs.ConcurrentSlots)
	// Unlike Algorand's, the pool read needs no funded account; a failed one
	// still leaves the configured half of the answer.
	var view slotsView
	if ps, err := e.pool(ctx); err == nil {
		view = solanaSlotsView(configured, ps)
	}
	for _, line := range renderSlots(configured, view) {
		fmt.Fprintln(cmd.OutOrStdout(), line)
	}
	warnSlotsEnvShadow(cmd)
	return nil
}

func runSolanaSlotsSet(ctx context.Context, cmd *cobra.Command, f *proxyFlags, e *solanaEnv, want uint64, assumeYes, dryRun bool) error {
	out := cmd.OutOrStdout()
	current := slotsOrDefault(e.cfg.Zs.ConcurrentSlots)
	if err := e.requireFee(ctx); err != nil {
		return err
	}
	ps, err := e.pool(ctx)
	if err != nil {
		return fmt.Errorf("read the rent pool: %w", err)
	}

	d := decideSolanaSlots(current, want, ps, dryRun)
	fmt.Fprintln(out, d.plan)
	if d.needsConsent {
		if !confirmYesNo(cmd, "Continue?", false, assumeYes) {
			return errors.New("aborted")
		}
	}

	target, err := slotsConfigTarget(f)
	if err != nil {
		return err
	}
	ex := slotsExec{
		deposit: func(ctx context.Context, want uint64) error {
			w, err := e.wallet(ctx)
			if err != nil {
				return err
			}
			return depositForSolanaSlots(ctx, out, w.RentPool(), want)
		},
		withdraw: func(ctx context.Context, amount uint64) (string, error) {
			w, err := e.wallet(ctx)
			if err != nil {
				return "", err
			}
			return w.RentPool().Withdraw(ctx, amount)
		},
		writeConfig: func(v uint64) error {
			return writeSlotsConfig(target, v)
		},
		configPath: target,
	}
	if err := applySlots(ctx, out, d.resize, ex); err != nil {
		return err
	}
	slotsWarnings(cmd, d, resolveTarget())
	return nil
}

// depositForSolanaSlots is depositForSlots for the rent pool.
func depositForSolanaSlots(ctx context.Context, out io.Writer, p *zs.SolanaRentPool, want uint64) error {
	sig, deposited, err := p.EnsureDeposit(ctx, want)
	switch {
	case errors.Is(err, zs.ErrRentDepositBelowTarget):
		fmt.Fprintf(out, "✔ deposited %s into the rent pool (%s)\n", lamportsSOL(deposited), sig)
		fmt.Fprintf(out, "⚠ the pool is still below %d slot(s) — %v\n", want, err)
		return nil
	case err != nil:
		return fmt.Errorf("fund the rent pool: %w", err)
	case sig == "":
		fmt.Fprintln(out, "✔ rent pool already backs the new limit")
		return nil
	default:
		fmt.Fprintf(out, "✔ deposited %s into the rent pool (%s)\n", lamportsSOL(deposited), sig)
		return nil
	}
}
