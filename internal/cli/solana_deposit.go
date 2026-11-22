/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/TxnLab/zerosignal/go/solana/rpc"
	"github.com/TxnLab/zerosignal/go/zs"
)

// runSolanaWithdraw is withdraw on Solana: SOL from the ticket rent pool.
// The program reverts an ask above the pool's free balance, as the escrow
// app does, and the SDK refuses it first so the error names the headroom.
func runSolanaWithdraw(ctx context.Context, out io.Writer, e *solanaEnv, amount string) error {
	lamports, err := solanaWithdrawLamports(amount)
	if err != nil {
		return err
	}
	if err := e.requireFee(ctx); err != nil {
		return err
	}
	w, err := e.wallet(ctx)
	if err != nil {
		return err
	}
	sig, err := w.RentPool().Withdraw(ctx, lamports)
	if errors.Is(err, zs.ErrNoRentPool) {
		return errors.New("this wallet has no rent pool yet — nothing to withdraw")
	}
	if err != nil {
		return fmt.Errorf("withdraw from the rent pool: %w", err)
	}
	fmt.Fprintf(out, "✔ withdrew %s from the rent pool (%s)\n", lamportsSOL(lamports), sig)
	return nil
}

// runSolanaCloseDeposit is close-deposit on Solana: close the rent pool and
// return all it holds. Like the Algorand guard, the listing explains a
// refusal and the program's counter has the last word, since an empty
// listing doesn't prove an empty pool.
func runSolanaCloseDeposit(ctx context.Context, out io.Writer, e *solanaEnv) error {
	st, lerr := loadSolanaTickets(ctx, e)
	ps, perr := e.pool(ctx)
	noPool, err := solanaCloseRefusal(st, lerr, ps, perr)
	if err != nil {
		return err
	}
	if noPool {
		fmt.Fprintln(out, "this wallet has no rent pool — nothing to close.")
		return nil
	}
	if err := e.requireFee(ctx); err != nil {
		return err
	}
	w, err := e.wallet(ctx)
	if err != nil {
		return err
	}
	sig, returned, err := w.CloseRentPool(ctx)
	switch {
	case errors.Is(err, zs.ErrNoRentPool):
		fmt.Fprintln(out, "this wallet has no rent pool — nothing to close.")
		return nil
	case errors.Is(err, zs.ErrRentPoolTicketsOpen):
		return fmt.Errorf("cannot close the rent pool: %w — `zs-proxy tickets` shows their state", err)
	case err != nil:
		return fmt.Errorf("close the rent pool: %w", err)
	}
	fmt.Fprintf(out, "✔ closed the rent pool — %s returned (%s)\n", lamportsSOL(returned), sig)
	return nil
}

// solanaWithdrawLamports is withdraw's amount: whole SOL, exactly, and above
// zero, which the program would refuse less legibly.
func solanaWithdrawLamports(amount string) (uint64, error) {
	lamports, err := solToLamports(amount)
	if err != nil {
		return 0, err
	}
	if lamports == 0 {
		return 0, errors.New("withdraw amount must be > 0")
	}
	return lamports, nil
}

// solanaCloseRefusal is why close-deposit won't send, from the listing and
// the pool read: a listed ticket names what blocks the close, and the
// program's counter, which the close asserts on, covers the accounts the
// listing couldn't read. A failed read refuses nothing; the SDK and the
// program still do. noPool is a wallet with nothing to close.
func solanaCloseRefusal(st solanaTickets, listErr error, ps zs.SolanaRentPoolStatus, poolErr error) (noPool bool, err error) {
	if listErr == nil && len(st.views) > 0 {
		return false, solanaCloseBlockedError(st.views)
	}
	if poolErr != nil {
		return false, nil
	}
	if !ps.Exists {
		return true, nil
	}
	if ps.OpenTickets > 0 {
		return false, fmt.Errorf("cannot close the rent pool: the program counts %d ticket(s) still open for this wallet — `zs-proxy tickets` shows their state", ps.OpenTickets)
	}
	return false, nil
}

// requireFee refuses a signing command the wallet can't pay the fee for,
// with the remedy, ahead of the cluster's less legible refusal.
func (e *solanaEnv) requireFee(ctx context.Context) error {
	lamports, err := e.rpc.GetBalance(ctx, e.payer, rpc.Confirmed)
	if err != nil {
		return fmt.Errorf("read SOL balance: %w", err)
	}
	if lamports < solanaTxFee {
		return fmt.Errorf("wallet %s has %s, short of a transaction fee — run `zs-proxy fund` first", e.payer, lamportsSOL(lamports))
	}
	return nil
}
