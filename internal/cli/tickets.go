/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/algo"
	protoalgod "github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/escrow"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// ticketView is one live ticket box owned by this payer, with the contract's
// verdict on it. The box is the only durable record of an outstanding ticket —
// the refund watchdog's tracking set is in-memory and a restart drops it — so
// every command here reconstructs from chain rather than from proxy state.
type ticketView struct {
	idB64   string
	idRaw   []byte
	rec     *escrow.TicketRecord
	verdict escrow.Verdict
}

// recoverable reports whether a deadline call is legal on v right now. These
// are exactly the two verdicts postResolution can compose, so this predicate
// and that switch must name the same set.
func recoverable(v escrow.Verdict) bool {
	return v == escrow.VerdictRefund || v == escrow.VerdictLapse
}

// verdictTally is the CLI's one reading of escrow.Classify's five answers.
// Four commands report on the same stuck tickets, and when each derived its own
// buckets they disagreed: closeDepositBlockedError folded VerdictUnknown in with
// the waiting tickets and told the payer to "retry once they settle", which is
// the exact reading escrow.Classify's godoc forbids — an unrecognized status
// usually means bindings older than the deployed contract, and waiting never
// makes a call legal. Unknown gets its own count here so no site can absorb it.
type verdictTally struct {
	recoverable int // refundInactive or settleLapsed is legal now
	waiting     int // inside the settle window; the deadline makes a call legal
	frozen      int // disputed; only the protocol admin's resolveFrozen closes it
	unknown     int // a status this build can't name; only an upgrade helps
}

func tallyVerdicts(views []ticketView) verdictTally {
	vs := make([]escrow.Verdict, len(views))
	for i, v := range views {
		vs[i] = v.verdict
	}
	return tallyOf(vs)
}

// tallyOf is tallyVerdicts over either rail's verdicts.
func tallyOf(verdicts []escrow.Verdict) verdictTally {
	var t verdictTally
	for _, v := range verdicts {
		switch v {
		case escrow.VerdictRefund, escrow.VerdictLapse:
			t.recoverable++
		case escrow.VerdictWait:
			t.waiting++
		case escrow.VerdictArbitrate:
			t.frozen++
		case escrow.VerdictUnknown:
			t.unknown++
		default:
			// A verdict this build can't name is the Unknown story exactly: no
			// call is known to be legal. Counting it anywhere else would either
			// promise a resolution or lose it from the total, and the four
			// callers rely on the buckets summing to len(views).
			t.unknown++
		}
	}
	return t
}

func newTicketsCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:   "tickets",
		Short: "Show this wallet's open escrow tickets and what can resolve them",
		Long: "Lists every live ticket box this wallet has escrowed USDC in, with its state,\n" +
			"deadline, and the call that can resolve it. Read from chain, so it also sees\n" +
			"tickets opened before the last proxy restart.\n\n" +
			"A ticket box exists only while it is unresolved — settling or refunding deletes\n" +
			"it — so anything listed here still has USDC locked and is holding an MBR slot\n" +
			"(which is what blocks `zs-proxy close-deposit`).",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := f.solanaRuntime(cmd.Context())
			if err != nil {
				return err
			}
			if e != nil {
				st, err := loadSolanaTickets(cmd.Context(), e)
				if err != nil {
					return err
				}
				if ps, perr := e.pool(cmd.Context()); perr == nil {
					st.counted = &ps.OpenTickets
				}
				return printSolanaTicketList(cmd.OutOrStdout(), st)
			}
			rt, err := f.openRuntime(cmd.Context())
			if err != nil {
				return err
			}
			pt, err := loadPayerTickets(cmd.Context(), rt.cfg, rt.meta.Address)
			if err != nil {
				return err
			}
			return printTicketList(cmd.Context(), cmd.OutOrStdout(), rt, pt)
		},
	}
	f.bindCommon(cmd)
	cmd.AddCommand(newTicketsGetCmd())
	cmd.AddCommand(newTicketsRecoverCmd())
	return cmd
}

func newTicketsGetCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "get <ticket-id>",
		Short:        "Show one ticket in full (ticket id as base64, from the logs)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := f.solanaRuntime(ctx)
			if err != nil {
				return err
			}
			if e != nil {
				v, found, unreadable, err := findSolanaTicket(ctx, e, args[0])
				if err != nil {
					return err
				}
				if !found {
					fmt.Fprintln(cmd.OutOrStdout(), solanaTicketNotFound(args[0], unreadable))
					return nil
				}
				printSolanaTicketDetail(cmd.OutOrStdout(), v)
				return nil
			}
			rt, err := f.openRuntime(ctx)
			if err != nil {
				return err
			}
			idRaw, idB64, err := parseTicketID(args[0])
			if err != nil {
				return fmt.Errorf("ticket id %q: %w", args[0], err)
			}
			ec, err := newReadOnlyEscrow(ctx, rt.cfg)
			if err != nil {
				return err
			}
			rec, err := ec.ReadTicketBox(ctx, idRaw)
			if err != nil {
				return fmt.Errorf("read ticket box: %w", err)
			}
			if rec == nil {
				// Box deletion IS the terminal state for settle and refund
				// alike, so "gone" is the good outcome, not an error.
				fmt.Fprintf(cmd.OutOrStdout(),
					"ticket %s: no box on chain — already finalized (settled or refunded) and its MBR slot released.\n", idB64)
				return nil
			}
			printTicketDetail(cmd.OutOrStdout(), ticketView{
				idB64: idB64, idRaw: idRaw, rec: rec, verdict: escrow.Classify(rec, time.Now()),
			}, rt.meta.Address)
			return nil
		},
	}
	f.bindCommon(cmd)
	return cmd
}

func newTicketsRecoverCmd() *cobra.Command {
	var f proxyFlags
	var all, dryRun bool
	cmd := &cobra.Command{
		Use:   "recover [ticket-id]",
		Short: "Post the deadline call that resolves a stuck ticket",
		Long: "Resolves tickets whose settlement never landed, by posting whichever call the\n" +
			"contract allows — both are permissionless:\n\n" +
			"  refundInactive  past-deadline ticket with no operator claim; returns the full\n" +
			"                  locked amount to this wallet.\n" +
			"  settleLapsed    past-deadline ticket carrying an operator claim the payer never\n" +
			"                  matched. PAYS the operator's claimed amount and refunds the\n" +
			"                  rest. Normally the operator's own node does this; drive it here\n" +
			"                  only when the node has stalled.\n\n" +
			"The refund watchdog already attempts both automatically while the proxy is up.\n" +
			"This is the manual path for what it missed — a ticket opened before a restart, or\n" +
			"one it gave up on after exhausting its retries.\n\n" +
			"Frozen (disputed) tickets are skipped: only the protocol admin can resolve them.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) == 0 && !all {
				return errors.New("pass a ticket id, or --all to sweep every recoverable ticket")
			}
			if len(args) == 1 && all {
				return errors.New("pass a ticket id or --all, not both")
			}
			e, err := f.solanaRuntime(ctx)
			if err != nil {
				return err
			}
			if e != nil {
				return recoverSolanaTickets(ctx, cmd.OutOrStdout(), e, all, args, dryRun)
			}
			rt, err := f.openRuntime(ctx)
			if err != nil {
				return err
			}
			var views []ticketView
			if all {
				pt, lerr := loadPayerTickets(ctx, rt.cfg, rt.meta.Address)
				if lerr != nil {
					return lerr
				}
				// Recovering only what we could decode is fine — each ticket is
				// independent — but say so, or `--all` silently means "all but
				// those".
				if pt.undecodable > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n", undecodableNote(pt.undecodable))
				}
				views = pt.views
			} else {
				v, err := loadOneTicket(ctx, rt.cfg, rt.meta.Address, args[0])
				if err != nil {
					return err
				}
				views = []ticketView{v}
			}
			return runRecover(ctx, cmd.OutOrStdout(), rt, views, dryRun)
		},
	}
	f.bindCommon(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "act on every recoverable ticket this wallet owns")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be posted, submit nothing")
	return cmd
}

// newReadOnlyEscrow builds a signer-less escrow client for box reads. No
// RefreshConfig: the ticket-box reads below need no cached globals, and
// RefreshConfig simulates from the payer, which would make an unfunded wallet
// unable to even look at its own stuck tickets. Recovery, which does need the
// cached treasury, refreshes separately.
func newReadOnlyEscrow(ctx context.Context, cfg *config.Config) (*escrow.Client, error) {
	if cfg.Zs.EscrowAppID == 0 {
		return nil, errors.New("no escrow app id configured")
	}
	ac, err := protoalgod.NewClient(cfg.Algod.ResolvedEndpoint, cfg.Algod.ResolvedToken)
	if err != nil {
		return nil, err
	}
	return escrow.NewClient(cfg.Zs.EscrowAppID, ac)
}

// payerTickets is what one chain read says about this wallet: its live tickets,
// plus how many `t:` boxes couldn't be decoded at all.
//
// The undecodable count can't be attributed to a payer — the payer address lives
// inside the value that failed to parse — so those boxes may or may not be this
// wallet's. That is exactly why the count has to travel with the list instead of
// silently shrinking it: "no open tickets" and "no open tickets that I could
// read" are very different answers for someone hunting locked USDC.
type payerTickets struct {
	views       []ticketView
	undecodable int
}

// loadPayerTickets fetches the app's live ticket boxes and keeps this payer's.
// There is no on-chain index by payer — ticket boxes are keyed by a random id —
// so a full fetch plus a client-side filter is the only route. It stays cheap
// because a box exists only until its ticket resolves (the set is the
// protocol's currently-unresolved tickets, not its history) and because
// FetchAllTickets pulls names+values in one prefix-filtered paginated sweep
// rather than a round trip per id.
func loadPayerTickets(ctx context.Context, cfg *config.Config, payerAddr string) (payerTickets, error) {
	ec, err := newReadOnlyEscrow(ctx, cfg)
	if err != nil {
		return payerTickets{}, err
	}
	fetched, err := ec.FetchAllTickets(ctx)
	if err != nil {
		return payerTickets{}, fmt.Errorf("fetch ticket boxes (escrow app %d): %w", cfg.Zs.EscrowAppID, err)
	}
	now := time.Now()
	out := payerTickets{
		views:       make([]ticketView, 0, 8),
		undecodable: len(fetched.Unreadable),
	}
	for i := range fetched.Tickets {
		if fetched.Tickets[i].Record.PayerAddr != payerAddr {
			continue
		}
		rec := &fetched.Tickets[i].Record
		out.views = append(out.views, ticketView{
			idB64:   base64.StdEncoding.EncodeToString(fetched.Tickets[i].IDRaw),
			idRaw:   fetched.Tickets[i].IDRaw,
			rec:     rec,
			verdict: escrow.Classify(rec, now),
		})
	}
	// Most-urgent first: furthest past its deadline leads.
	sort.Slice(out.views, func(i, j int) bool {
		return out.views[i].rec.SettlementDeadline().Before(out.views[j].rec.SettlementDeadline())
	})
	return out, nil
}

func loadOneTicket(ctx context.Context, cfg *config.Config, payerAddr, arg string) (ticketView, error) {
	idRaw, idB64, err := parseTicketID(arg)
	if err != nil {
		return ticketView{}, fmt.Errorf("ticket id %q: %w", arg, err)
	}
	ec, err := newReadOnlyEscrow(ctx, cfg)
	if err != nil {
		return ticketView{}, err
	}
	rec, err := ec.ReadTicketBox(ctx, idRaw)
	if err != nil {
		return ticketView{}, fmt.Errorf("read ticket box: %w", err)
	}
	if rec == nil {
		return ticketView{}, fmt.Errorf("ticket %s: no box on chain — already finalized", idB64)
	}
	if rec.PayerAddr != payerAddr {
		// Refund and lapse are both permissionless, so this would actually
		// succeed — refunding USDC to a stranger at our expense. Refuse.
		return ticketView{}, fmt.Errorf("ticket %s belongs to %s, not this wallet (%s)", idB64, rec.PayerAddr, payerAddr)
	}
	return ticketView{idB64: idB64, idRaw: idRaw, rec: rec, verdict: escrow.Classify(rec, time.Now())}, nil
}

func parseTicketID(s string) (raw []byte, normB64 string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, "", errors.New("empty")
	}
	raw, err = base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, "", fmt.Errorf("not valid base64: %w", err)
	}
	if len(raw) != 16 {
		return nil, "", fmt.Errorf("decodes to %d bytes, want 16", len(raw))
	}
	return raw, base64.StdEncoding.EncodeToString(raw), nil
}

// ---- rendering -------------------------------------------------------------

func printTicketList(ctx context.Context, out io.Writer, rt *runtimeEnv, pt payerTickets) error {
	if len(pt.views) == 0 {
		// Only a clean read licenses "nothing is locked". With undecodable boxes
		// in the app, this wallet's ticket could be one of them.
		if pt.undecodable > 0 {
			fmt.Fprintf(out, "no readable open tickets for this wallet.\n%s\n", undecodableNote(pt.undecodable))
			return nil
		}
		fmt.Fprintln(out, "no open tickets — nothing of this wallet's is locked in escrow.")
		return nil
	}
	if err := writeTicketTable(out, pt.views, time.Now()); err != nil {
		return err
	}
	printTicketFooter(ctx, out, rt, pt)
	return nil
}

// undecodableNote explains boxes the current bindings couldn't parse. It never
// says "not yours": the payer address is inside the value that failed to parse,
// so attribution is exactly what's unavailable.
func undecodableNote(n int) string {
	return fmt.Sprintf("note: %d ticket box(es) on chain could not be decoded by this build, so they are missing from the list above.\n"+
		"      They may belong to this wallet — a box's payer is stored inside the value that failed to parse.\n"+
		"      This usually means the binary is older than the deployed escrow contract; upgrading zs-proxy is the fix.", n)
}

// writeTicketTable renders the rows. Split from printTicketList so the display
// — the whole point of the command — is testable without a chain.
func writeTicketTable(out io.Writer, views []ticketView, now time.Time) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TICKET\tOP/NODE\tLOCKED\tSTATUS\tDEADLINE\tRESOLVES BY")
	for _, v := range views {
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\t%s\n",
			v.idB64,
			v.rec.OperatorID, v.rec.NodeID,
			microUSDC(v.rec.MaxPrice),
			ticketStatus(v.rec),
			relTime(v.rec.SettlementDeadline().Sub(now)),
			resolution(v),
		)
	}
	return tw.Flush()
}

// printTicketFooter ties the list back to the two things a stuck ticket costs:
// locked USDC and the MBR slot that gates close-deposit.
func printTicketFooter(ctx context.Context, out io.Writer, rt *runtimeEnv, pt payerTickets) {
	var locked uint64
	for _, v := range pt.views {
		locked += v.rec.MaxPrice
	}
	tally := tallyVerdicts(pt.views)
	fmt.Fprintf(out, "\n%d open — %s locked.\n", len(pt.views), microUSDC(locked))
	if pt.undecodable > 0 {
		fmt.Fprintln(out, undecodableNote(pt.undecodable))
	}

	// Cross-check against the contract's own per-payer counter, which comes from
	// chain by a different route (local-state openTickets vs. this box listing).
	// Deliberately soft: the two reads aren't atomic with each other and the
	// listing isn't round-pinned, so a ticket opening or finalizing between them
	// — routine on a proxy serving requests — moves one and not the other. Only
	// a mismatch that persists across runs means something leaked.
	if ps, err := readPoolStatus(ctx, rt.cfg, rt.meta.Address); err == nil && ps.dep.OpenTickets != uint64(len(pt.views)) {
		fmt.Fprintf(out, "note: the contract counts %d open ticket(s) for this wallet, %d listed above — expected if one opened or settled just now; a mismatch that persists is worth reporting.\n",
			ps.dep.OpenTickets, len(pt.views))
	}
	if tally.recoverable > 0 {
		fmt.Fprintf(out, "%d recoverable now — `zs-proxy tickets recover --all --dry-run` to preview.\n", tally.recoverable)
	}
	if tally.frozen > 0 {
		fmt.Fprintf(out, "%d frozen (disputed) — waiting for the protocol admin's resolution, which splits the escrow\n"+
			"between the operator and this wallet. Until then their USDC and MBR slots stay locked,\n"+
			"so `zs-proxy close-deposit` cannot succeed.\n", tally.frozen)
	}
	if tally.unknown > 0 {
		fmt.Fprintf(out, "%d in a state this build doesn't recognize — no call is known to be legal and waiting won't change that;\n"+
			"this usually means the binary is older than the deployed escrow contract, so upgrading zs-proxy is the fix.\n", tally.unknown)
	}
}

func printTicketDetail(out io.Writer, v ticketView, payerAddr string) {
	now := time.Now()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	p := func(k string, format string, args ...any) {
		fmt.Fprintf(tw, "%s\t%s\n", k, fmt.Sprintf(format, args...))
	}
	p("ticket", "%s", v.idB64)
	p("payer", "%s%s", v.rec.PayerAddr, sameAddrNote(v.rec.PayerAddr, payerAddr))
	p("operator/node", "%d/%d", v.rec.OperatorID, v.rec.NodeID)
	p("operator owner", "%s", v.rec.OperatorOwnerAddr)
	p("node signing", "%s", v.rec.NodeSigningAddr)
	p("locked", "%s (max price)", microUSDC(v.rec.MaxPrice))
	p("status", "%s", ticketStatus(v.rec))
	// A ticket frozen over a pending claim keeps it on the box, and the claim
	// is what the payer disputed, so it stays in view.
	if v.rec.Status == escrow.StatusPendingSettle ||
		(v.rec.Status == escrow.StatusFrozen && v.rec.PendingBy != escrow.PendingNone) {
		p("claimed", "%s by %s", microUSDC(v.rec.PendingAmount), pendingSide(v.rec.PendingBy))
		p("claimed usage", "%d input, %d output (type %d)",
			v.rec.PendingInputCount, v.rec.PendingOutputCount, v.rec.PendingOutputUsageType)
	}
	if v.rec.Status == escrow.StatusFrozen {
		p("frozen", "%s", frozenDetail(v.rec))
	}
	p("opened", "%s", time.Unix(int64(v.rec.OpenedAt), 0).Format(time.RFC3339))
	p("expires", "%s", time.Unix(int64(v.rec.ExpiresAt), 0).Format(time.RFC3339))
	p("grace", "%ds", v.rec.SettlementGraceSeconds)
	p("deadline", "%s (%s)", v.rec.SettlementDeadline().Format(time.RFC3339),
		relTime(v.rec.SettlementDeadline().Sub(now)))
	p("fee", "%d bps", v.rec.FeeBps)
	p("resolves by", "%s", resolution(v))
	_ = tw.Flush()
}

// frozenDetail says how and when a ticket froze. A settle mismatch and a protest
// leave otherwise identical boxes, and the admin's split can turn on which one it
// was. A ticket frozen before the contract recorded either field reads zero.
func frozenDetail(rec *escrow.TicketRecord) string {
	var how string
	switch rec.FreezeOrigin {
	case escrow.FreezeOriginSettleMismatch:
		how = "by settlement (payer and operator claims differed)"
	case escrow.FreezeOriginProtest:
		how = "by payer protest"
	case escrow.FreezeOriginNone:
		how = "cause not recorded"
	default:
		how = fmt.Sprintf("cause %d", rec.FreezeOrigin)
	}
	if rec.FrozenAt == 0 {
		return how + ", time not recorded"
	}
	return how + ", at " + time.Unix(int64(rec.FrozenAt), 0).Format(time.RFC3339)
}

func sameAddrNote(a, b string) string {
	if a == b {
		return " (this wallet)"
	}
	return ""
}

func ticketStatus(rec *escrow.TicketRecord) string {
	switch rec.Status {
	case escrow.StatusOpen:
		return "OPEN"
	case escrow.StatusFrozen:
		return "FROZEN"
	case escrow.StatusPendingSettle:
		return fmt.Sprintf("PENDING(%s)", pendingSide(rec.PendingBy))
	case escrow.StatusSettled:
		return "SETTLED"
	case escrow.StatusRefunded:
		return "REFUNDED"
	}
	return fmt.Sprintf("UNKNOWN(%d)", rec.Status)
}

func pendingSide(by uint64) string {
	switch by {
	case escrow.PendingByOperator:
		return "operator"
	case escrow.PendingByPayer:
		return "payer"
	case escrow.PendingNone:
		return "none"
	}
	return fmt.Sprintf("side %d", by)
}

// resolution renders the verdict as what will actually happen, in money terms.
func resolution(v ticketView) string {
	switch v.verdict {
	case escrow.VerdictWait:
		return "settle window still open — operator settles, or the watchdog acts at the deadline"
	case escrow.VerdictRefund:
		return fmt.Sprintf("refundInactive — returns %s", microUSDC(v.rec.MaxPrice))
	case escrow.VerdictLapse:
		pay, back := lapseSplit(v.rec)
		return fmt.Sprintf("settleLapsed — pays operator %s, returns %s", microUSDC(pay), microUSDC(back))
	case escrow.VerdictArbitrate:
		return "disputed — waits for the protocol admin to split the escrow; USDC and MBR slot locked until then"
	}
	// VerdictUnknown: a status this build doesn't recognize, so no call is known
	// to be legal. Don't guess one — say the binary is behind the chain.
	return "unrecognized state — this build may be older than the deployed contract; upgrade zs-proxy"
}

// lapseSplit is what settleLapsed disburses: the operator's claimed amount, and
// the remainder of max_price after the protocol fee returns to the payer.
//
// Mirrors the contract's finalizeSettlement two-step fee exactly, discount and
// all. The HAY discount is retired, so open() snapshots discountBps = 0 on every
// ticket the deployed contract writes and the net fee equals the gross — but
// discountBps is still a live per-ticket field that finalizeSettlement honors,
// and the box hands us the real one. Reading it costs nothing and can't drift;
// hardcoding the zero would quietly overstate the fee the day it comes back.
func lapseSplit(rec *escrow.TicketRecord) (operatorGets, payerGets uint64) {
	operatorGets = rec.PendingAmount
	grossFee := rec.PendingAmount * rec.FeeBps / 10_000
	fee := grossFee * (10_000 - rec.DiscountBps) / 10_000
	spent := operatorGets + fee
	if spent > rec.MaxPrice {
		// settle() asserts amountCharged + fee <= maxPrice, so this is
		// unreachable for a box the contract wrote. Don't render a wrapped
		// uint64 if it ever happens.
		return operatorGets, 0
	}
	return operatorGets, rec.MaxPrice - spent
}

func relTime(d time.Duration) string {
	if d >= 0 {
		return "in " + shortDur(d)
	}
	return shortDur(-d) + " ago"
}

func shortDur(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return d.Truncate(time.Second).String()
}

// ---- recovery --------------------------------------------------------------

func runRecover(ctx context.Context, out io.Writer, rt *runtimeEnv, views []ticketView, dryRun bool) error {
	var actionable []ticketView
	for _, v := range views {
		if recoverable(v.verdict) {
			actionable = append(actionable, v)
			continue
		}
		fmt.Fprintf(out, "%s  %s — skip (%s)\n", v.idB64, ticketStatus(v.rec), skipReason(v))
	}
	if len(actionable) == 0 {
		fmt.Fprintln(out, "\nnothing recoverable.")
		return nil
	}
	for _, v := range actionable {
		fmt.Fprintf(out, "%s  %s — %s\n", v.idB64, ticketStatus(v.rec), resolution(v))
	}
	if dryRun {
		fmt.Fprintf(out, "\n%d actionable. Re-run without --dry-run to submit.\n", len(actionable))
		return nil
	}

	// Signing path: needs the keystore and the cached treasury address that
	// RefreshConfig loads (settleLapsed and refundInactive both reference it).
	ks, err := buildKeystore(ctx)
	if err != nil {
		return err
	}
	ec, err := newReadOnlyEscrow(ctx, rt.cfg)
	if err != nil {
		return err
	}
	if err := ec.RefreshConfig(ctx, rt.meta.Address); err != nil {
		return fmt.Errorf("load escrow config: %w", err)
	}
	signer := algo.SignWithAccountForATC(ks, rt.meta.Address)

	fmt.Fprintln(out)
	var failed int
	for _, v := range actionable {
		txid, err := postResolution(ctx, ec, v, signer, rt.meta.Address)
		if err != nil {
			failed++
			fmt.Fprintf(out, "✘ %s  %s failed: %v\n", v.idB64, v.verdict, err)
			continue
		}
		fmt.Fprintf(out, "✔ %s  %s (%s)\n", v.idB64, v.verdict, txid)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d failed — re-run `zs-proxy tickets` to see current state", failed, len(actionable))
	}
	return nil
}

func postResolution(ctx context.Context, ec *escrow.Client, v ticketView, signer transaction.TransactionSigner, payerAddr string) (string, error) {
	switch v.verdict {
	case escrow.VerdictRefund:
		res, err := ec.ComposeRefundInactive(ctx, escrow.RefundInactiveArgs{
			TicketIDRaw: v.idRaw,
			OperatorID:  v.rec.OperatorID,
			NodeID:      v.rec.NodeID,
			PayerAddr:   payerAddr,
		}, signer, payerAddr)
		if err != nil {
			return "", err
		}
		return res.TxID, nil
	case escrow.VerdictLapse:
		res, err := ec.ComposeSettleLapsed(ctx, escrow.SettleLapsedArgs{
			TicketIDRaw:       v.idRaw,
			OperatorID:        v.rec.OperatorID,
			NodeID:            v.rec.NodeID,
			OperatorOwnerAddr: v.rec.OperatorOwnerAddr,
			PayerAddr:         payerAddr,
		}, signer, payerAddr)
		if err != nil {
			return "", err
		}
		return res.TxID, nil
	}
	return "", fmt.Errorf("verdict %s is not actionable", v.verdict)
}

// printStuckTicketNote adds a line to `status` when any open ticket is past its
// deadline or frozen. Best-effort: a walk failure leaves the plain count alone
// rather than failing a status read.
func printStuckTicketNote(ctx context.Context, out io.Writer, rt *runtimeEnv) {
	pt, err := loadPayerTickets(ctx, rt.cfg, rt.meta.Address)
	if err != nil {
		return
	}
	if pt.undecodable > 0 {
		fmt.Fprintf(out, "%s%d ticket box(es) unreadable by this build — see `zs-proxy tickets`\n", poolNoteIndent, pt.undecodable)
	}
	if len(pt.views) == 0 {
		return
	}
	tally := tallyVerdicts(pt.views)
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

// closeDepositBlockedError names the tickets holding the pool open and says
// whether each will ever clear. The bare count it replaces implied waiting was
// always enough; for a frozen ticket that is false — only the protocol admin's
// resolveFrozen closes it — and for a past-deadline one the owner can act right
// now.
func closeDepositBlockedError(views []ticketView) error {
	tally := tallyVerdicts(views)
	var b strings.Builder
	fmt.Fprintf(&b, "cannot close the MBR pool: %d ticket(s) still open\n", len(views))
	for _, v := range views {
		fmt.Fprintf(&b, "  %s  %s — %s\n", v.idB64, ticketStatus(v.rec), resolution(v))
	}
	if tally.recoverable > 0 {
		fmt.Fprintf(&b, "\n%d recoverable now — run `zs-proxy tickets recover --all`, then retry.", tally.recoverable)
	}
	if tally.waiting > 0 {
		fmt.Fprintf(&b, "\n%d still inside its settle window — retry once they settle.", tally.waiting)
	}
	if tally.frozen > 0 {
		fmt.Fprintf(&b, "\n%d frozen (disputed) — only the protocol admin's resolution clears a frozen ticket, "+
			"by splitting its escrow between the operator and this wallet, so the pool cannot be closed until then. "+
			"No call this wallet can make will clear them.", tally.frozen)
	}
	if tally.unknown > 0 {
		fmt.Fprintf(&b, "\n%d in a state this build doesn't recognize — no known call resolves them and waiting "+
			"will not either. This usually means the binary is older than the deployed escrow contract; "+
			"upgrading zs-proxy is the fix.", tally.unknown)
	}
	return errors.New(b.String())
}

func skipReason(v ticketView) string {
	switch v.verdict {
	case escrow.VerdictWait:
		return "settle window still open until " + relTime(time.Until(v.rec.SettlementDeadline()))
	case escrow.VerdictArbitrate:
		return "frozen — only the protocol admin can resolve a disputed ticket"
	}
	return "unrecognized state — this build may be older than the deployed contract"
}
