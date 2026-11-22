/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/escrow"
	"github.com/TxnLab/zerosignal/go/solana/zsescrow"
	"github.com/TxnLab/zerosignal/go/zs"
)

const (
	testTicketRent = 2_916_240
	testPoolRent   = 1_197_120
)

func TestSolToLamports(t *testing.T) {
	tests := []struct {
		in      string
		want    uint64
		wantErr string
	}{
		{in: "1", want: 1_000_000_000},
		// A float would land 0.1 SOL a lamport off.
		{in: "0.1", want: 100_000_000},
		{in: " 0.000000001 ", want: 1},
		{in: "18446744073.709551615", want: 18_446_744_073_709_551_615},
		{in: "0", want: 0},
		{in: "0.0000000001", wantErr: "finer than a lamport"},
		{in: "-1", wantErr: "invalid SOL amount"},
		{in: "1/2", wantErr: "invalid SOL amount"},
		// big.Rat reads these as 16, 100 and 1 SOL.
		{in: "0x10", wantErr: "invalid SOL amount"},
		{in: "1e2", wantErr: "invalid SOL amount"},
		{in: "0b1", wantErr: "invalid SOL amount"},
		{in: ".5", wantErr: "invalid SOL amount"},
		{in: "1.", wantErr: "invalid SOL amount"},
		{in: "ten", wantErr: "invalid SOL amount"},
		{in: "", wantErr: "invalid SOL amount"},
		{in: "18446744074", wantErr: "too large"},
	}
	for _, tc := range tests {
		got, err := solToLamports(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("solToLamports(%q) = %d, %v; want an error containing %q", tc.in, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("solToLamports(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestSolanaUnits(t *testing.T) {
	if got := lamportsSOL(1_500_000_001); got != "1.500000001 SOL" {
		t.Errorf("lamportsSOL = %q", got)
	}
	if got := lamportsSOL(0); got != "0.000000000 SOL" {
		t.Errorf("lamportsSOL(0) = %q", got)
	}
	if got := tokenUnits(1_000_000_001, 6, "USDC"); got != "1000.000001 USDC" {
		t.Errorf("tokenUnits = %q", got)
	}
	if got := tokenUnits(42, 0, "T"); got != "42 T" {
		t.Errorf("tokenUnits with no decimals = %q", got)
	}
}

func TestSolanaFundedFloor(t *testing.T) {
	// The token account's rent counts only while there is no account.
	i := solanaInfo{lamports: 2*solanaTxFee + 99, accountRent: 100}
	if i.funded() {
		t.Error("funded without the token account's rent")
	}
	i.usdcAccount = true
	if !i.funded() {
		t.Error("not funded with an account and two fees")
	}
	i.lamports = 2*solanaTxFee - 1
	if i.funded() {
		t.Error("funded below two fees")
	}
	// The floor itself is enough.
	if !(solanaInfo{lamports: 2 * solanaTxFee, usdcAccount: true}).funded() {
		t.Error("not funded at exactly two fees")
	}
	if !(solanaInfo{lamports: 2*solanaTxFee + 100, accountRent: 100}).funded() {
		t.Error("not funded at exactly two fees and the account's rent")
	}
}

func TestSolanaReady(t *testing.T) {
	ok := solanaInfo{lamports: 1_000_000, usdcAccount: true}
	if !solanaReady(ok) {
		t.Error("a funded wallet with a USDC account reads not ready")
	}
	frozen := ok
	frozen.usdcFrozen = true
	if solanaReady(frozen) {
		t.Error("ready with a frozen USDC account")
	}
	noAcct := ok
	noAcct.usdcAccount = false
	if solanaReady(noAcct) {
		t.Error("ready without a USDC account")
	}
	if solanaReady(solanaInfo{usdcAccount: true}) {
		t.Error("ready with no SOL")
	}
}

func TestRentPoolLine(t *testing.T) {
	if got := rentPoolLine(zs.SolanaRentPoolStatus{TicketRent: testTicketRent}); !strings.Contains(got, "none yet") {
		t.Errorf("no pool: %q", got)
	}
	// Two slots free, two in tickets: four backed.
	ps := zs.SolanaRentPoolStatus{Exists: true, Lamports: testPoolRent + 2*testTicketRent, OpenTickets: 2, TicketRent: testTicketRent, PoolRent: testPoolRent}
	got := rentPoolLine(ps)
	if !strings.Contains(got, "2 of 4 ticket slot(s) in use") || !strings.Contains(got, lamportsSOL(2*testTicketRent)+" free") {
		t.Errorf("rentPoolLine = %q", got)
	}
}

func TestSolanaUnfundedLines(t *testing.T) {
	if got := solanaUnfundedLines(solanaInfo{lamports: 1_000_000, usdcAccount: true}, 6); len(got) != 0 {
		t.Errorf("a ready wallet warns: %q", got)
	}
	got := strings.Join(solanaUnfundedLines(solanaInfo{lamports: 7, accountRent: 100}, 6), "\n")
	if !strings.Contains(got, "needs SOL — have "+lamportsSOL(7)) || !strings.Contains(got, "no USDC token account") {
		t.Errorf("unfunded, no account: %q", got)
	}
	got = strings.Join(solanaUnfundedLines(solanaInfo{lamports: 1_000_000, usdcAccount: true, usdcFrozen: true, usdc: 5}, 6), "\n")
	if !strings.Contains(got, "frozen") || strings.Contains(got, "needs SOL") {
		t.Errorf("frozen account: %q", got)
	}
}

func rentPoolFor(free, open uint64) zs.SolanaRentPoolStatus {
	return zs.SolanaRentPoolStatus{Exists: true, Lamports: testPoolRent + free, OpenTickets: open, TicketRent: testTicketRent, PoolRent: testPoolRent}
}

func TestDecideSolanaSlots(t *testing.T) {
	t.Run("raise deposits the shortfall", func(t *testing.T) {
		d := decideSolanaSlots(10, 25, rentPoolFor(10*testTicketRent, 0), false)
		if d.resize.act != slotsActionDeposit || d.resize.want != 25 || d.resize.pool != solanaRentPool || !d.needsConsent {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.Contains(d.plan, "depositing up to "+lamportsSOL(15*testTicketRent)) {
			t.Errorf("plan = %q", d.plan)
		}
	})
	t.Run("a lowering reclaims only what is free", func(t *testing.T) {
		// Twenty-five backed, twenty of them in live tickets: lowering to ten
		// is fifteen over, of which the pool holds five.
		d := decideSolanaSlots(25, 10, rentPoolFor(5*testTicketRent, 20), false)
		if d.resize.act != slotsActionWithdraw || d.resize.excess != 15*testTicketRent || d.resize.withdrawable != 5*testTicketRent {
			t.Fatalf("resize = %+v", d.resize)
		}
		if d.resize.openTickets != 20 || !d.lowered {
			t.Errorf("open %d, lowered %v", d.resize.openTickets, d.lowered)
		}
		if !strings.Contains(d.plan, "reclaiming "+lamportsSOL(5*testTicketRent)) {
			t.Errorf("plan = %q", d.plan)
		}
	})
	t.Run("a lowering reclaims the excess, not all that is free", func(t *testing.T) {
		ps := rentPoolFor(3*testTicketRent, 0)
		d := decideSolanaSlots(3, 1, ps, false)
		if d.resize.withdrawable != 2*testTicketRent {
			t.Errorf("withdrawable %d; want %d", d.resize.withdrawable, 2*testTicketRent)
		}
	})
	t.Run("dry run moves nothing", func(t *testing.T) {
		d := decideSolanaSlots(10, 25, rentPoolFor(0, 0), true)
		if d.resize.act != slotsActionNone || d.needsConsent || !d.resize.dryRun {
			t.Errorf("dry run decision = %+v", d)
		}
	})
	t.Run("a first deposit counts the pool's own rent", func(t *testing.T) {
		d := decideSolanaSlots(10, 4, zs.SolanaRentPoolStatus{TicketRent: testTicketRent, PoolRent: testPoolRent}, false)
		if d.resize.act != slotsActionDeposit || !strings.Contains(d.plan, "depositing up to "+lamportsSOL(4*testTicketRent+testPoolRent)) {
			t.Errorf("decision = %+v", d)
		}
	})
}

func TestApplySlotsNamesTheRentPool(t *testing.T) {
	var out bytes.Buffer
	f := &fakeExec{}
	d := decideSolanaSlots(25, 10, rentPoolFor(5*testTicketRent, 20), false)
	if err := applySlots(context.Background(), &out, d.resize, f.exec()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "reclaimed "+lamportsSOL(5*testTicketRent)+" from the rent pool") {
		t.Errorf("output = %q", got)
	}
	if !strings.Contains(got, lamportsSOL(10*testTicketRent)+" stays in the pool — 20 ticket(s)") {
		t.Errorf("output = %q", got)
	}
	if strings.Contains(got, "ALGO") || strings.Contains(got, "MBR") {
		t.Errorf("Algorand units on Solana: %q", got)
	}
}

func TestSolanaSlotsView(t *testing.T) {
	v := solanaSlotsView(10, rentPoolFor(2*testTicketRent, 5))
	if !v.known || v.backed != 7 || v.open != 5 || v.deposited != 7*testTicketRent || v.shortfall != 3*testTicketRent || v.excess != 0 {
		t.Fatalf("view = %+v", v)
	}
	joined := strings.Join(renderSlots(10, v), "\n")
	for _, want := range []string{"backed:      7 slot(s)   " + lamportsSOL(7*testTicketRent), "per slot:    " + lamportsSOL(testTicketRent), lamportsSOL(3*testTicketRent) + " short of 10"} {
		if !strings.Contains(joined, want) {
			t.Errorf("render missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ALGO") {
		t.Errorf("ALGO on Solana:\n%s", joined)
	}
	if v := solanaSlotsView(10, zs.SolanaRentPoolStatus{}); v.known {
		t.Error("the zero status reads as known")
	}
}

func solanaView(status zsescrow.TicketStatus, deadline time.Time, pending uint64) solanaTicketView {
	tk := &zsescrow.Ticket{
		Status:    status,
		MaxPrice:  10_000,
		FeeBps:    500,
		ExpiresAt: deadline.Unix(),
		Pending:   zsescrow.ClaimFields{Amount: pending},
	}
	tk.TicketID[0] = byte(status) + 1
	v := solanaTicketView{pt: zs.SolanaPayerTicket{OperatorID: 3, NodeID: 4, Ticket: tk}, idB64: "id" + string(rune('A'+status)), decimals: 6}
	v.verdict = zsescrow.Classify(tk, time.Now())
	return v
}

func TestSolanaResolution(t *testing.T) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	tests := []struct {
		name string
		v    solanaTicketView
		want []string
	}{
		{"open in its window waits", solanaView(zsescrow.TicketOpen, future, 0), []string{"settle window still open"}},
		{"open past its deadline refunds", solanaView(zsescrow.TicketOpen, past, 0), []string{"refund_inactive", "returns " + microUSDC(10_000)}},
		// 4,000 to the operator, 5% of it as the fee, the rest back.
		{"a lapse pays the claim and the fee", solanaView(zsescrow.TicketPendingByOperator, past, 4_000), []string{"settle_lapsed", "pays operator " + microUSDC(4_000), "returns " + microUSDC(5_800)}},
		{"frozen waits for the admin", solanaView(zsescrow.TicketFrozen, past, 4_000), []string{"protocol admin", "rent slot locked"}},
		{"an unknown status is no wait", solanaView(zsescrow.TicketStatus(9), past, 0), []string{"unrecognized state", "upgrade zs-proxy"}},
	}
	for _, tc := range tests {
		got := solanaResolution(tc.v)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q missing %q", tc.name, got, want)
			}
		}
	}
	if got := solanaTicketStatus(&zsescrow.Ticket{Status: 9}); got != "UNKNOWN(9)" {
		t.Errorf("unknown status = %q", got)
	}
}

func TestSolanaCloseBlockedError(t *testing.T) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	err := solanaCloseBlockedError([]solanaTicketView{
		solanaView(zsescrow.TicketOpen, past, 0),
		solanaView(zsescrow.TicketOpen, future, 0),
		solanaView(zsescrow.TicketFrozen, past, 1),
	})
	got := err.Error()
	for _, want := range []string{"3 ticket(s) still open", "1 recoverable now", "1 still inside its settle window", "1 frozen (disputed)", "No call this wallet can make"} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q:\n%s", want, got)
		}
	}

	// Distinct counts, so each line is seen to count its own tickets.
	got = solanaCloseBlockedError([]solanaTicketView{
		solanaView(zsescrow.TicketOpen, past, 0), solanaView(zsescrow.TicketOpen, past, 0),
		solanaView(zsescrow.TicketOpen, future, 0), solanaView(zsescrow.TicketOpen, future, 0), solanaView(zsescrow.TicketOpen, future, 0),
	}).Error()
	if !strings.Contains(got, "2 recoverable now") || !strings.Contains(got, "3 still inside its settle window") {
		t.Errorf("distinct counts:\n%s", got)
	}
}

func TestSolanaTicketStatus(t *testing.T) {
	for status, want := range map[zsescrow.TicketStatus]string{
		zsescrow.TicketOpen:              "OPEN",
		zsescrow.TicketFrozen:            "FROZEN",
		zsescrow.TicketPendingByOperator: "PENDING(operator)",
		zsescrow.TicketPendingByPayer:    "PENDING(payer)",
		9:                                "UNKNOWN(9)",
	} {
		if got := solanaTicketStatus(&zsescrow.Ticket{Status: status}); got != want {
			t.Errorf("status %d = %q, want %q", status, got, want)
		}
	}
}

func TestSolanaTicketDetailPending(t *testing.T) {
	for _, status := range []zsescrow.TicketStatus{zsescrow.TicketPendingByOperator, zsescrow.TicketPendingByPayer} {
		var out bytes.Buffer
		printSolanaTicketDetail(&out, solanaView(status, time.Now().Add(time.Hour), 4_000))
		if !strings.Contains(out.String(), "claimed        "+microUSDC(4_000)) || !strings.Contains(out.String(), "claimed usage") {
			t.Errorf("status %d hides its claim:\n%s", status, out.String())
		}
	}
}

func TestMatchSolanaTicket(t *testing.T) {
	a, b := solanaView(zsescrow.TicketOpen, time.Now(), 0), solanaView(zsescrow.TicketOpen, time.Now(), 0)
	a.pt.Ticket.TicketID, b.pt.Ticket.TicketID = [16]byte{0: 7}, [16]byte{0: 7, 15: 1}
	b.idB64 = "second"
	unreadable := []zs.SolanaUnreadableTicket{{Err: errors.New("x")}}
	st := solanaTickets{views: []solanaTicketView{a, b}, unreadable: unreadable}
	// Ids that differ only in their last byte are told apart.
	if v, ok, _ := matchSolanaTicket(st, b.pt.Ticket.TicketID[:]); !ok || v.idB64 != "second" {
		t.Errorf("matched %q, %v; want the second", v.idB64, ok)
	}
	if _, ok, un := matchSolanaTicket(st, make([]byte, 16)); ok || len(un) != 1 {
		t.Errorf("no match: found %v, unreadable %d; want none found and the unreadable back", ok, len(un))
	}
}

func TestSolanaSlotsViewOversized(t *testing.T) {
	v := solanaSlotsView(10, rentPoolFor(12*testTicketRent, 0))
	if v.excess != 2*testTicketRent || v.shortfall != 0 {
		t.Fatalf("view = %+v", v)
	}
	if joined := strings.Join(renderSlots(10, v), "\n"); !strings.Contains(joined, lamportsSOL(2*testTicketRent)+" more than 10 slot(s) needs") {
		t.Errorf("render:\n%s", joined)
	}
}

func TestDecideSolanaSlots_AllHeldByTickets(t *testing.T) {
	// Oversized, but live tickets hold all of it.
	d := decideSolanaSlots(25, 10, rentPoolFor(0, 25), false)
	if d.resize.act != slotsActionNone || !strings.Contains(d.plan, "nothing reclaimable right now ("+lamportsSOL(15*testTicketRent)) || strings.Contains(d.plan, "ALGO") {
		t.Errorf("plan = %q", d.plan)
	}
}

func TestApplySlotsNamesTheMBRPool(t *testing.T) {
	var out bytes.Buffer
	f := &fakeExec{}
	if err := applySlots(context.Background(), &out, slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 100, excess: 100}, f.exec()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "from the MBR pool") || strings.Contains(out.String(), "SOL") {
		t.Errorf("Algorand output = %q", out.String())
	}
	f = &fakeExec{withdrawErr: errors.New("reverted")}
	if err := applySlots(context.Background(), &out, slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 100}, f.exec()); err == nil || !strings.Contains(err.Error(), "reclaim from MBR pool") {
		t.Errorf("Algorand error = %v", err)
	}
}

func TestPrintSolanaTicketList(t *testing.T) {
	var out bytes.Buffer
	if err := printSolanaTicketList(&out, solanaTickets{}); err != nil || !strings.Contains(out.String(), "no open tickets") {
		t.Errorf("empty list = %q, %v", out.String(), err)
	}

	out.Reset()
	past := time.Now().Add(-time.Hour)
	three := uint64(3)
	st := solanaTickets{
		views:      []solanaTicketView{solanaView(zsescrow.TicketOpen, past, 0), solanaView(zsescrow.TicketFrozen, past, 1)},
		unreadable: []zs.SolanaUnreadableTicket{{Err: errors.New("bad version")}},
		decimals:   6,
		counted:    &three,
	}
	if err := printSolanaTicketList(&out, st); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"2 open — " + microUSDC(20_000) + " locked", "1 recoverable now", "1 frozen (disputed)",
		"1 ticket account(s) of this wallet could not be read", "bad version", "3/4", "the program counts 3 open ticket(s) for this wallet, 2 listed"} {
		if !strings.Contains(got, want) {
			t.Errorf("list missing %q:\n%s", want, got)
		}
	}

	// A counter below the listing is a mismatch too.
	out.Reset()
	one := uint64(1)
	st.counted = &one
	_ = printSolanaTicketList(&out, st)
	if !strings.Contains(out.String(), "the program counts 1 open ticket(s) for this wallet, 2 listed") {
		t.Errorf("a low counter isn't reported:\n%s", out.String())
	}

	// A counter that agrees says nothing.
	out.Reset()
	two := uint64(2)
	st.counted = &two
	_ = printSolanaTicketList(&out, st)
	if strings.Contains(out.String(), "the program counts") {
		t.Errorf("an agreeing counter is reported:\n%s", out.String())
	}

	// Only unreadable accounts still say so, rather than "no open tickets",
	// and lock no zero total.
	out.Reset()
	_ = printSolanaTicketList(&out, solanaTickets{unreadable: st.unreadable})
	if got := out.String(); strings.Contains(got, "no open tickets") || !strings.Contains(got, "no readable open tickets") ||
		!strings.Contains(got, "could not be read") || strings.Contains(got, "0 open") {
		t.Errorf("unreadable only = %q", got)
	}
}

func TestSolanaTicketAmountsUseTheMintsDecimals(t *testing.T) {
	v := solanaView(zsescrow.TicketOpen, time.Now().Add(-time.Hour), 0)
	v.decimals = 2
	if got := solanaResolution(v); !strings.Contains(got, "returns 100.00 USDC") {
		t.Errorf("resolution = %q", got)
	}
}

func TestSolanaTicketNotFound(t *testing.T) {
	if got := solanaTicketNotFound("abc", nil); !strings.Contains(got, "already ended") {
		t.Errorf("none unreadable = %q", got)
	}
	// An unreadable account may be the ticket, so it can't be said to have ended.
	got := solanaTicketNotFound("abc", []zs.SolanaUnreadableTicket{{Err: errors.New("x")}})
	if strings.Contains(got, "already ended") || !strings.Contains(got, "1 of its ticket account(s) could not be read") {
		t.Errorf("one unreadable = %q", got)
	}
}

func TestSolanaTicketDetailFrozen(t *testing.T) {
	var out bytes.Buffer
	v := solanaView(zsescrow.TicketFrozen, time.Now().Add(-time.Hour), 0)
	printSolanaTicketDetail(&out, v)
	got := out.String()
	// A freeze over a zero claim still shows the claim it froze over.
	if !strings.Contains(got, "claimed") || !strings.Contains(got, "cause not recorded, time not recorded") {
		t.Errorf("detail:\n%s", got)
	}
	v.pt.Ticket.FreezeOrigin, v.pt.Ticket.FrozenAt = zsescrow.FreezeOriginProtest, 1_700_000_000
	if got := solanaFrozenDetail(v.pt.Ticket); !strings.Contains(got, "by payer protest, at ") {
		t.Errorf("protest = %q", got)
	}
	if got := solanaFrozenDetail(&zsescrow.Ticket{FreezeOrigin: 7}); !strings.Contains(got, "cause 7") {
		t.Errorf("unknown cause = %q", got)
	}
	out.Reset()
	printSolanaTicketDetail(&out, solanaView(zsescrow.TicketOpen, time.Now().Add(time.Hour), 0))
	if strings.Contains(out.String(), "claimed") || strings.Contains(out.String(), "frozen") {
		t.Errorf("an open ticket shows a claim:\n%s", out.String())
	}
}

func TestSolanaRecoverLine(t *testing.T) {
	v := solanaView(zsescrow.TicketOpen, time.Now().Add(-time.Hour), 0)
	tests := []struct {
		name    string
		verdict escrow.Verdict
		sig     string
		err     error
		want    string
		ok      bool
	}{
		{"sent", escrow.VerdictRefund, "SIG", nil, "✔ idA  " + escrow.VerdictRefund.String() + " (SIG)", true},
		// The node's sweep won the race: the ticket ended, which is the goal.
		{"gone", escrow.VerdictUnknown, "", zs.ErrSolanaTicketGone, "✔ idA  already ended", true},
		{"nothing to send", escrow.VerdictWait, "", nil, "· idA  nothing sent: the program allows " + escrow.VerdictWait.String() + " now", true},
		{"failed", escrow.VerdictUnknown, "", errors.New("rpc down"), "✘ idA  " + v.verdict.String() + " failed: rpc down", false},
	}
	for _, tc := range tests {
		line, ok := solanaRecoverLine(v, tc.verdict, tc.sig, tc.err)
		if line != tc.want || ok != tc.ok {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, line, ok, tc.want, tc.ok)
		}
	}
}

func TestSolanaCloseRefusal(t *testing.T) {
	open := solanaTickets{views: []solanaTicketView{solanaView(zsescrow.TicketOpen, time.Now().Add(time.Hour), 0)}}
	empty := rentPoolFor(testTicketRent, 0)
	tests := []struct {
		name       string
		st         solanaTickets
		listErr    error
		ps         zs.SolanaRentPoolStatus
		poolErr    error
		wantErr    string
		wantNoPool bool
	}{
		// The listing's refusal names the ticket and what resolves it, which
		// the counter's can't.
		{name: "a listed ticket blocks it", st: open, ps: rentPoolFor(0, 1), wantErr: "cannot close the rent pool: 1 ticket(s) still open\n  idA"},
		{name: "a listed ticket blocks it unread pool", st: open, poolErr: errors.New("rpc"), wantErr: "1 ticket(s) still open\n  idA"},
		// The listing missed it (an unreadable account, or a read that raced
		// an open); the counter the close asserts on still blocks it.
		{name: "the counter blocks it", ps: rentPoolFor(0, 2), wantErr: "the program counts 2 ticket(s)"},
		{name: "a failed listing leaves the counter", listErr: errors.New("rpc"), ps: rentPoolFor(0, 1), wantErr: "the program counts 1"},
		{name: "no pool", ps: zs.SolanaRentPoolStatus{TicketRent: testTicketRent}, wantNoPool: true},
		{name: "an empty pool closes", ps: empty},
		// Neither read refuses on its own failure: the SDK and the program
		// still do.
		{name: "both reads failed", listErr: errors.New("rpc"), poolErr: errors.New("rpc")},
	}
	for _, tc := range tests {
		noPool, err := solanaCloseRefusal(tc.st, tc.listErr, tc.ps, tc.poolErr)
		switch {
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err %v; want %q", tc.name, err, tc.wantErr)
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: err %v", tc.name, err)
		case noPool != tc.wantNoPool:
			t.Errorf("%s: noPool %v", tc.name, noPool)
		}
	}
}

func TestSolanaWithdrawLamports(t *testing.T) {
	if got, err := solanaWithdrawLamports("0.5"); err != nil || got != 500_000_000 {
		t.Errorf("0.5 = %d, %v", got, err)
	}
	if _, err := solanaWithdrawLamports("0"); err == nil || !strings.Contains(err.Error(), "> 0") {
		t.Errorf("zero: %v", err)
	}
	if _, err := solanaWithdrawLamports("0x10"); err == nil {
		t.Error("a hex amount is accepted")
	}
}

func TestCheckMintAuthority(t *testing.T) {
	if err := checkMintAuthority("key.json", false); err == nil {
		t.Error("a mint authority is taken off a test cluster")
	}
	if err := checkMintAuthority("key.json", true); err != nil {
		t.Errorf("test cluster: %v", err)
	}
	if err := checkMintAuthority("", false); err != nil {
		t.Errorf("no authority: %v", err)
	}
}

func TestRentPoolFundingLine(t *testing.T) {
	if got := rentPoolFundingLine(3, rentPoolFor(3*testTicketRent, 0)); got != "" {
		t.Errorf("funded pool = %q", got)
	}
	got := rentPoolFundingLine(3, rentPoolFor(testTicketRent, 0))
	if !strings.Contains(got, "depositing up to "+lamportsSOL(2*testTicketRent)+" for 3 slot(s)") || strings.Contains(got, "first deposit") {
		t.Errorf("top-up = %q", got)
	}
	got = rentPoolFundingLine(3, zs.SolanaRentPoolStatus{TicketRent: testTicketRent, PoolRent: testPoolRent})
	if !strings.Contains(got, lamportsSOL(3*testTicketRent)) || !strings.Contains(got, "plus "+lamportsSOL(testPoolRent)+" for the pool's own rent") {
		t.Errorf("first deposit = %q", got)
	}
}

func TestRunSolanaRecoverDryRun(t *testing.T) {
	var out bytes.Buffer
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	views := []solanaTicketView{
		solanaView(zsescrow.TicketOpen, past, 0),
		solanaView(zsescrow.TicketOpen, future, 0),
		solanaView(zsescrow.TicketFrozen, past, 1),
	}
	// An empty env on a machine with no wallet: a dry run that reached for
	// the wallet errors at once here, rather than opening the developer's.
	isolateWalletState(t)
	if err := runSolanaRecover(context.Background(), &out, &solanaEnv{}, views, true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "1 actionable") || strings.Count(got, "skip") != 2 || !strings.Contains(got, "only the protocol admin") {
		t.Errorf("dry run = %q", got)
	}

	out.Reset()
	if err := runSolanaRecover(context.Background(), &out, nil, views[1:], false); err != nil || !strings.Contains(out.String(), "nothing recoverable") {
		t.Errorf("nothing recoverable = %q, %v", out.String(), err)
	}
}
