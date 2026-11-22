/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TxnLab/zerosignal/go/escrow"
	"github.com/TxnLab/zerosignal/go/settle"
	"github.com/TxnLab/zerosignal/go/solana/zsescrow"
	"github.com/TxnLab/zerosignal/go/zs"
)

// solanaTicketView is ticketView on the Solana rail: one of this payer's
// ticket accounts and the program's verdict on it. A ticket account exists
// only until its ticket ends, as an Algorand box does, so the listing is
// what still holds this wallet's USDC and a rent slot.
type solanaTicketView struct {
	pt      zs.SolanaPayerTicket
	idB64   string
	verdict escrow.Verdict
	// decimals is the Config's mint's: a ticket's amounts are in its base
	// units, as status's balance is.
	decimals uint8
}

func (v solanaTicketView) deadline() time.Time { return time.Unix(v.pt.Ticket.Deadline(), 0) }

func (v solanaTicketView) usdc(amount uint64) string { return tokenUnits(amount, v.decimals, "USDC") }

// solanaTickets is payerTickets on Solana. Unlike Algorand's undecodable
// boxes, an unreadable account here matched this payer's key, so it is this
// wallet's.
type solanaTickets struct {
	views      []solanaTicketView
	unreadable []zs.SolanaUnreadableTicket
	decimals   uint8
	// counted is the rent pool's open-ticket counter, read by another route,
	// when the read worked.
	counted *uint64
}

func loadSolanaTickets(ctx context.Context, e *solanaEnv) (solanaTickets, error) {
	got, err := zs.FetchSolanaPayerTickets(ctx, e.rpc, e.d, e.payer)
	if err != nil {
		return solanaTickets{}, err
	}
	now := time.Now()
	out := solanaTickets{unreadable: got.Unreadable, decimals: e.conf.USDCDecimals}
	// The SDK orders the soonest deadline first, Algorand's order here.
	for _, pt := range got.Tickets {
		out.views = append(out.views, solanaTicketView{
			pt:       pt,
			idB64:    base64.StdEncoding.EncodeToString(pt.Ticket.TicketID[:]),
			verdict:  zsescrow.Classify(pt.Ticket, now),
			decimals: e.conf.USDCDecimals,
		})
	}
	return out, nil
}

func solanaVerdicts(views []solanaTicketView) []escrow.Verdict {
	vs := make([]escrow.Verdict, len(views))
	for i, v := range views {
		vs[i] = v.verdict
	}
	return vs
}

// findSolanaTicket is the payer's ticket with id. A ticket account is keyed
// by its node as well, which a ticket id alone doesn't name, so this reads
// the payer's listing; the listing's unreadable accounts come back too, since
// any of them could be the one asked for.
func findSolanaTicket(ctx context.Context, e *solanaEnv, arg string) (solanaTicketView, bool, []zs.SolanaUnreadableTicket, error) {
	idRaw, _, err := parseTicketID(arg)
	if err != nil {
		return solanaTicketView{}, false, nil, fmt.Errorf("ticket id %q: %w", arg, err)
	}
	st, err := loadSolanaTickets(ctx, e)
	if err != nil {
		return solanaTicketView{}, false, nil, err
	}
	v, found, unreadable := matchSolanaTicket(st, idRaw)
	return v, found, unreadable, nil
}

func matchSolanaTicket(st solanaTickets, idRaw []byte) (solanaTicketView, bool, []zs.SolanaUnreadableTicket) {
	for _, v := range st.views {
		if bytes.Equal(v.pt.Ticket.TicketID[:], idRaw) {
			return v, true, nil
		}
	}
	return solanaTicketView{}, false, st.unreadable
}

// solanaTicketNotFound is what `tickets get` says of an id the listing
// lacks. With accounts it couldn't read, it can't say the ticket ended.
func solanaTicketNotFound(id string, unreadable []zs.SolanaUnreadableTicket) string {
	if len(unreadable) == 0 {
		return fmt.Sprintf("ticket %s: no ticket account of this wallet's — already ended (settled or refunded) and its rent returned to the pool.", id)
	}
	return fmt.Sprintf("ticket %s: not among this wallet's readable tickets, but %d of its ticket account(s) could not be read by this build, "+
		"so it may be one of them — `zs-proxy tickets` lists them; upgrading zs-proxy is the fix.", id, len(unreadable))
}

func printSolanaTicketList(out io.Writer, st solanaTickets) error {
	if len(st.views) == 0 && len(st.unreadable) == 0 {
		fmt.Fprintln(out, "no open tickets — nothing of this wallet's is locked in escrow.")
		return nil
	}
	if len(st.views) == 0 {
		fmt.Fprintln(out, "no readable open tickets for this wallet.")
	} else {
		if err := writeSolanaTicketTable(out, st.views, time.Now()); err != nil {
			return err
		}
		var locked uint64
		for _, v := range st.views {
			locked += v.pt.Ticket.MaxPrice
		}
		fmt.Fprintf(out, "\n%d open — %s locked.\n", len(st.views), tokenUnits(locked, st.decimals, "USDC"))
	}
	printSolanaUnreadable(out, st.unreadable)
	// Soft, as Algorand's cross-check is: the two reads aren't atomic, so a
	// ticket opening or ending between them moves one and not the other.
	if st.counted != nil && *st.counted != uint64(len(st.views)) {
		fmt.Fprintf(out, "note: the program counts %d open ticket(s) for this wallet, %d listed above — expected if one opened or settled just now; a mismatch that persists is worth reporting.\n",
			*st.counted, len(st.views))
	}
	tally := tallyOf(solanaVerdicts(st.views))
	if tally.recoverable > 0 {
		fmt.Fprintf(out, "%d recoverable now — `zs-proxy tickets recover --all --dry-run` to preview.\n", tally.recoverable)
	}
	if tally.frozen > 0 {
		fmt.Fprintf(out, "%d frozen (disputed) — waiting for the protocol admin's resolution, which splits the escrow\n"+
			"between the operator and this wallet. Until then their USDC and rent slots stay locked,\n"+
			"so `zs-proxy close-deposit` cannot succeed.\n", tally.frozen)
	}
	if tally.unknown > 0 {
		fmt.Fprintf(out, "%d in a state this build doesn't recognize — no call is known to be legal and waiting won't change that;\n"+
			"this usually means the binary is older than the deployed program, so upgrading zs-proxy is the fix.\n", tally.unknown)
	}
	return nil
}

// printSolanaUnreadable names the accounts that carry this wallet's key but
// can't be read as its tickets. They are this wallet's, which Algorand's
// undecodable boxes can't be said to be, so each is listed with why.
func printSolanaUnreadable(out io.Writer, unreadable []zs.SolanaUnreadableTicket) {
	if len(unreadable) == 0 {
		return
	}
	fmt.Fprintf(out, "note: %d ticket account(s) of this wallet could not be read by this build:\n", len(unreadable))
	for _, u := range unreadable {
		fmt.Fprintf(out, "      %s: %v\n", u.Address, u.Err)
	}
	fmt.Fprintln(out, "      This usually means the binary is older than the deployed program; upgrading zs-proxy is the fix.")
}

func writeSolanaTicketTable(out io.Writer, views []solanaTicketView, now time.Time) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TICKET\tOP/NODE\tLOCKED\tSTATUS\tDEADLINE\tRESOLVES BY")
	for _, v := range views {
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\t%s\n",
			v.idB64,
			v.pt.OperatorID, v.pt.NodeID,
			v.usdc(v.pt.Ticket.MaxPrice),
			solanaTicketStatus(v.pt.Ticket),
			relTime(v.deadline().Sub(now)),
			solanaResolution(v),
		)
	}
	return tw.Flush()
}

func printSolanaTicketDetail(out io.Writer, v solanaTicketView) {
	t := v.pt.Ticket
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	p := func(k string, format string, args ...any) {
		fmt.Fprintf(tw, "%s\t%s\n", k, fmt.Sprintf(format, args...))
	}
	p("ticket", "%s", v.idB64)
	p("account", "%s", v.pt.Address)
	p("payer", "%s (this wallet)", t.Payer)
	p("operator/node", "%d/%d", v.pt.OperatorID, v.pt.NodeID)
	p("node signing", "%s", t.NodeSigning)
	p("locked", "%s (max price)", v.usdc(t.MaxPrice))
	p("status", "%s", solanaTicketStatus(t))
	// A frozen ticket keeps the claim it froze over, which is what the payer
	// disputed, so it stays in view. Unlike Algorand's box, the account
	// doesn't say which side claimed, so a zero claim is shown too.
	if t.Status == zsescrow.TicketPendingByOperator || t.Status == zsescrow.TicketPendingByPayer ||
		t.Status == zsescrow.TicketFrozen {
		p("claimed", "%s", v.usdc(t.Pending.Amount))
		p("claimed usage", "%d input, %d output (type %d)", t.Pending.InputUnits, t.Pending.OutputUnits, t.Pending.OutputUsageType)
	}
	if t.Status == zsescrow.TicketFrozen {
		p("frozen", "%s", solanaFrozenDetail(t))
	}
	p("opened", "%s", time.Unix(t.OpenedAt, 0).Format(time.RFC3339))
	p("expires", "%s", time.Unix(t.ExpiresAt, 0).Format(time.RFC3339))
	p("grace", "%ds", t.Grace)
	p("deadline", "%s (%s)", v.deadline().Format(time.RFC3339), relTime(time.Until(v.deadline())))
	p("fee", "%d bps", t.FeeBps)
	p("resolves by", "%s", solanaResolution(v))
	_ = tw.Flush()
}

func solanaFrozenDetail(t *zsescrow.Ticket) string {
	var how string
	switch t.FreezeOrigin {
	case zsescrow.FreezeOriginSettleMismatch:
		how = "by settlement (payer and operator claims differed)"
	case zsescrow.FreezeOriginProtest:
		how = "by payer protest"
	case 0:
		how = "cause not recorded"
	default:
		how = fmt.Sprintf("cause %d", t.FreezeOrigin)
	}
	if t.FrozenAt == 0 {
		return how + ", time not recorded"
	}
	return how + ", at " + time.Unix(t.FrozenAt, 0).Format(time.RFC3339)
}

func solanaTicketStatus(t *zsescrow.Ticket) string {
	switch t.Status {
	case zsescrow.TicketOpen:
		return "OPEN"
	case zsescrow.TicketFrozen:
		return "FROZEN"
	case zsescrow.TicketPendingByOperator:
		return "PENDING(operator)"
	case zsescrow.TicketPendingByPayer:
		return "PENDING(payer)"
	}
	return fmt.Sprintf("UNKNOWN(%d)", t.Status)
}

// solanaResolution is resolution on Solana, where a ticket's ending also
// returns its rent to the pool.
func solanaResolution(v solanaTicketView) string {
	t := v.pt.Ticket
	switch v.verdict {
	case escrow.VerdictWait:
		return "settle window still open — operator settles, or the watchdog acts at the deadline"
	case escrow.VerdictRefund:
		return fmt.Sprintf("refund_inactive — returns %s", v.usdc(t.MaxPrice))
	case escrow.VerdictLapse:
		// The program's protocol_fee has no discount term.
		b := settle.ComputeFeeBreakdown(t.Pending.Amount, t.MaxPrice, uint64(t.FeeBps), 0)
		return fmt.Sprintf("settle_lapsed — pays operator %s, returns %s", v.usdc(b.AmountCharged), v.usdc(b.Refund))
	case escrow.VerdictArbitrate:
		return "disputed — waits for the protocol admin to split the escrow; USDC and rent slot locked until then"
	}
	return "unrecognized state — this build may be older than the deployed program; upgrade zs-proxy"
}

func solanaSkipReason(v solanaTicketView) string {
	switch v.verdict {
	case escrow.VerdictWait:
		return "settle window still open until " + relTime(time.Until(v.deadline()))
	case escrow.VerdictArbitrate:
		return "frozen — only the protocol admin can resolve a disputed ticket"
	}
	return "unrecognized state — this build may be older than the deployed program"
}

// recoverSolanaTickets picks the tickets for `tickets recover` on Solana.
func recoverSolanaTickets(ctx context.Context, out io.Writer, e *solanaEnv, all bool, args []string, dryRun bool) error {
	if all {
		st, err := loadSolanaTickets(ctx, e)
		if err != nil {
			return err
		}
		// Each ticket is independent, so recovering the readable ones is
		// fine, but `--all` must not silently mean "all but those".
		if len(st.unreadable) > 0 {
			printSolanaUnreadable(out, st.unreadable)
			fmt.Fprintln(out)
		}
		return runSolanaRecover(ctx, out, e, st.views, dryRun)
	}
	v, found, unreadable, err := findSolanaTicket(ctx, e, args[0])
	if err != nil {
		return err
	}
	if !found {
		return errors.New(solanaTicketNotFound(args[0], unreadable))
	}
	return runSolanaRecover(ctx, out, e, []solanaTicketView{v}, dryRun)
}

// runSolanaRecover is runRecover on Solana, through the SDK's RecoverTicket,
// which reads each ticket again and sends what the program allows then, at
// the reconciler's skew: a ticket that just passed its deadline here can
// still be one it waits on.
func runSolanaRecover(ctx context.Context, out io.Writer, e *solanaEnv, views []solanaTicketView, dryRun bool) error {
	var actionable []solanaTicketView
	for _, v := range views {
		if recoverable(v.verdict) {
			actionable = append(actionable, v)
			continue
		}
		fmt.Fprintf(out, "%s  %s — skip (%s)\n", v.idB64, solanaTicketStatus(v.pt.Ticket), solanaSkipReason(v))
	}
	if len(actionable) == 0 {
		fmt.Fprintln(out, "\nnothing recoverable.")
		return nil
	}
	for _, v := range actionable {
		fmt.Fprintf(out, "%s  %s — %s\n", v.idB64, solanaTicketStatus(v.pt.Ticket), solanaResolution(v))
	}
	if dryRun {
		fmt.Fprintf(out, "\n%d actionable. Re-run without --dry-run to submit.\n", len(actionable))
		return nil
	}

	w, err := e.wallet(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	var failed int
	for _, v := range actionable {
		verdict, sig, err := w.RecoverTicket(ctx, v.pt.OperatorID, v.pt.NodeID, v.pt.Ticket.TicketID)
		line, ok := solanaRecoverLine(v, verdict, sig, err)
		fmt.Fprintln(out, line)
		if !ok {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d failed — re-run `zs-proxy tickets` to see current state", failed, len(actionable))
	}
	return nil
}

// solanaRecoverLine reports one RecoverTicket. A ticket that ended under the
// race with the node's sweep is a success, not a failure. On an error the
// SDK returns no verdict, so the line names the one the listing planned.
func solanaRecoverLine(v solanaTicketView, verdict escrow.Verdict, sig string, err error) (string, bool) {
	switch {
	case errors.Is(err, zs.ErrSolanaTicketGone):
		return fmt.Sprintf("✔ %s  already ended", v.idB64), true
	case err != nil:
		return fmt.Sprintf("✘ %s  %s failed: %v", v.idB64, v.verdict, err), false
	case sig == "":
		return fmt.Sprintf("· %s  nothing sent: the program allows %s now", v.idB64, verdict), true
	}
	return fmt.Sprintf("✔ %s  %s (%s)", v.idB64, verdict, sig), true
}

// printSolanaStuckTicketNote is printStuckTicketNote on Solana.
func printSolanaStuckTicketNote(ctx context.Context, out io.Writer, e *solanaEnv) {
	st, err := loadSolanaTickets(ctx, e)
	if err != nil {
		return
	}
	if n := len(st.unreadable); n > 0 {
		fmt.Fprintf(out, "%s%d ticket account(s) unreadable by this build — see `zs-proxy tickets`\n", poolNoteIndent, n)
	}
	tally := tallyOf(solanaVerdicts(st.views))
	if tally.recoverable > 0 {
		fmt.Fprintf(out, "%s%d past its deadline — `zs-proxy tickets recover --all` to release\n", poolNoteIndent, tally.recoverable)
	}
	if tally.frozen > 0 {
		fmt.Fprintf(out, "%s%d frozen (disputed) — locked until the protocol admin resolves it; see `zs-proxy tickets`\n", poolNoteIndent, tally.frozen)
	}
	if tally.unknown > 0 {
		fmt.Fprintf(out, "%s%d in a state this build doesn't recognize — upgrading zs-proxy is the fix; see `zs-proxy tickets`\n", poolNoteIndent, tally.unknown)
	}
}

// solanaCloseBlockedError is closeDepositBlockedError for the rent pool.
func solanaCloseBlockedError(views []solanaTicketView) error {
	tally := tallyOf(solanaVerdicts(views))
	var b strings.Builder
	fmt.Fprintf(&b, "cannot close the rent pool: %d ticket(s) still open\n", len(views))
	for _, v := range views {
		fmt.Fprintf(&b, "  %s  %s — %s\n", v.idB64, solanaTicketStatus(v.pt.Ticket), solanaResolution(v))
	}
	if tally.recoverable > 0 {
		fmt.Fprintf(&b, "\n%d recoverable now — run `zs-proxy tickets recover --all`, then retry.", tally.recoverable)
	}
	if tally.waiting > 0 {
		fmt.Fprintf(&b, "\n%d still inside its settle window — retry once they settle.", tally.waiting)
	}
	if tally.frozen > 0 {
		fmt.Fprintf(&b, "\n%d frozen (disputed) — only the protocol admin's resolution clears a frozen ticket, "+
			"so the pool cannot be closed until then. No call this wallet can make will clear them.", tally.frozen)
	}
	if tally.unknown > 0 {
		fmt.Fprintf(&b, "\n%d in a state this build doesn't recognize — no known call resolves them and waiting "+
			"will not either; upgrading zs-proxy is the fix.", tally.unknown)
	}
	return errors.New(b.String())
}
