/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"math"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/account"
)

// newWithdrawCmd is `zs withdraw` — reclaim unreserved ALGO from the prepaid
// ticket-MBR pool back to the wallet. The CLI twin of the client's withdraw
// UX. The contract caps the amount at the available (non-reserved) balance
// (algoBalance − openTickets × mbrForTicket()) and reverts otherwise, so a
// stuck/frozen ticket's slot can't be pulled out from under it.
func newWithdrawCmd() *cobra.Command {
	var (
		f      proxyFlags
		amount string
	)
	cmd := &cobra.Command{
		Use:   "withdraw <amount>",
		Short: "Reclaim unreserved ALGO (SOL on Solana) from the prepaid ticket pool",
		Long: "Withdraws ALGO from the prepaid ticket-MBR pool back to the wallet. The amount\n" +
			"is whole ALGO (e.g. 0.5). Only the unreserved balance (not backing a live\n" +
			"ticket) is withdrawable; the contract reverts if you ask for more.\n\n" +
			"On Solana the amount is whole SOL, from the ticket rent pool.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			amount = args[0]
			e, err := f.solanaRuntime(cmd.Context())
			if err != nil {
				return err
			}
			if e != nil {
				return runSolanaWithdraw(cmd.Context(), cmd.OutOrStdout(), e, amount)
			}
			micro, err := algoToMicro(amount)
			if err != nil {
				return err
			}
			if micro == 0 {
				return fmt.Errorf("withdraw amount must be > 0")
			}
			ctx := cmd.Context()
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
				return fmt.Errorf("wallet %s is not funded — run `zs-proxy fund` first", rt.meta.Address)
			}
			ks, err := buildKeystore(ctx)
			if err != nil {
				return err
			}
			w, err := buildDepositWallet(ctx, rt, ks)
			if err != nil {
				return fmt.Errorf("build deposit wallet: %w", err)
			}
			txid, err := w.WithdrawDeposit(ctx, micro)
			if err != nil {
				return fmt.Errorf("withdraw from MBR pool: %w", err)
			}
			fmt.Fprintf(out, "✔ withdrew %s from the MBR pool (%s)\n", microAlgos(micro), txid)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

// newCloseDepositCmd is `zs close-deposit` — close the prepaid pool entirely,
// refunding the full remaining balance + the local-schema opt-in MBR and
// opting the wallet out of the escrow app. The CLI twin of the client's close
// UX. Reverts on chain while any ticket (including frozen) is still live
// (openTickets != 0), so it's safe to call only once nothing is in flight.
func newCloseDepositCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:   "close-deposit",
		Short: "Close the prepaid ticket pool (MBR, or rent on Solana) and refund the full balance",
		Long: "Closes the prepaid ticket-MBR pool, refunding the entire remaining balance\n" +
			"plus the local-schema opt-in MBR, and opts the wallet out of the escrow app.\n" +
			"Reverts while any ticket (including frozen) is still live — close only once no\n" +
			"requests are in flight.\n\n" +
			"On Solana it closes the ticket rent pool and returns all it holds.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			e, err := f.solanaRuntime(ctx)
			if err != nil {
				return err
			}
			if e != nil {
				return runSolanaCloseDeposit(ctx, cmd.OutOrStdout(), e)
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
				return fmt.Errorf("wallet %s is not funded — run `zs-proxy fund` first", rt.meta.Address)
			}
			// Best-effort guard: refuse early with a clear message when tickets
			// are live, rather than surfacing the contract's opaque revert. The
			// contract is still the authority (a ticket could open between this
			// read and the call).
			//
			// Read the boxes rather than just the counter so the refusal can say
			// which tickets block the close and whether they will ever clear.
			// But the counter is what closeDeposit actually asserts on, so it
			// still gets the last word when the boxes show nothing: an empty list
			// is not proof of an empty pool (a box we couldn't decode, or a read
			// that raced an open), and letting that through would surface exactly
			// the revert this guard exists to explain.
			pt, verr := loadPayerTickets(ctx, rt.cfg, rt.meta.Address)
			if verr == nil && len(pt.views) > 0 {
				return closeDepositBlockedError(pt.views)
			}
			if ps, derr := readPoolStatus(ctx, rt.cfg, rt.meta.Address); derr == nil && ps.dep.OpenTickets > 0 {
				return fmt.Errorf("cannot close the MBR pool: the contract counts %d ticket(s) still open for this wallet — `zs-proxy tickets` shows their state", ps.dep.OpenTickets)
			}
			ks, err := buildKeystore(ctx)
			if err != nil {
				return err
			}
			w, err := buildDepositWallet(ctx, rt, ks)
			if err != nil {
				return fmt.Errorf("build deposit wallet: %w", err)
			}
			txid, err := w.CloseDeposit(ctx)
			if err != nil {
				return fmt.Errorf("close MBR pool: %w", err)
			}
			fmt.Fprintf(out, "✔ closed the MBR pool — balance + opt-in MBR refunded (%s)\n", txid)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

// algoToMicro converts a whole-ALGO decimal string (e.g. "0.5", "1.25") to
// µALGO, rounding to the nearest unit. Mirrors usdcToMicro's whole-unit input
// convention for the CLI (the wallet methods take µALGO).
func algoToMicro(s string) (uint64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid ALGO amount %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("ALGO amount must be >= 0, got %s", s)
	}
	return uint64(math.Round(v * 1_000_000)), nil
}
