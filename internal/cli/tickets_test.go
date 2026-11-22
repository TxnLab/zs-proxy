/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/escrow"
)

func TestParseTicketID(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(make([]byte, 16))
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"valid", valid, ""},
		{"valid with surrounding space", "  " + valid + "\n", ""},
		{"empty", "", "empty"},
		{"not base64", "!!!not-base64!!!", "not valid base64"},
		{"wrong length", base64.StdEncoding.EncodeToString(make([]byte, 8)), "decodes to 8 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, norm, err := parseTicketID(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseTicketID(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTicketID(%q) = %v", tc.in, err)
			}
			if len(raw) != 16 {
				t.Fatalf("raw len = %d, want 16", len(raw))
			}
			if norm != valid {
				t.Fatalf("normalized = %q, want %q", norm, valid)
			}
		})
	}
}

// lapseSplit decides real payouts, so it must mirror the contract's
// finalizeSettlement math exactly: operator gets amountCharged, the protocol
// takes amountCharged*feeBps/1e4, and the payer gets whatever max_price has
// left over.
func TestLapseSplit(t *testing.T) {
	tests := []struct {
		name                 string
		maxPrice, pending    uint64
		feeBps               uint64
		wantOperator, wantTo uint64
	}{
		{
			name: "typical claim with 2% fee",
			// fee = 3100 * 200 / 10000 = 62; payer gets 150000 - 3100 - 62
			maxPrice: 150_000, pending: 3_100, feeBps: 200,
			wantOperator: 3_100, wantTo: 146_838,
		},
		{
			name:     "zero-cost claim returns the whole lock",
			maxPrice: 150_000, pending: 0, feeBps: 200,
			wantOperator: 0, wantTo: 150_000,
		},
		{
			name: "claim consuming the entire lock leaves the payer nothing",
			// fee = 98039 * 200 / 10000 = 1960; 98039 + 1960 = 99999 <= 100000
			maxPrice: 100_000, pending: 98_039, feeBps: 200,
			wantOperator: 98_039, wantTo: 1,
		},
		{
			name:     "no fee configured",
			maxPrice: 50_000, pending: 10_000, feeBps: 0,
			wantOperator: 10_000, wantTo: 40_000,
		},
		{
			name: "claim exceeding max price cannot underflow",
			// Unreachable on chain (settle asserts against it); guard anyway —
			// a wrapped uint64 would render an absurd refund.
			maxPrice: 1_000, pending: 5_000, feeBps: 200,
			wantOperator: 5_000, wantTo: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &escrow.TicketRecord{MaxPrice: tc.maxPrice, PendingAmount: tc.pending, FeeBps: tc.feeBps}
			gotOp, gotPayer := lapseSplit(rec)
			if gotOp != tc.wantOperator || gotPayer != tc.wantTo {
				t.Fatalf("lapseSplit() = (operator %d, payer %d), want (%d, %d)",
					gotOp, gotPayer, tc.wantOperator, tc.wantTo)
			}
			// The contract never disburses more than was locked.
			if gotOp+gotPayer > tc.maxPrice && tc.pending <= tc.maxPrice {
				t.Fatalf("disbursed %d > max price %d", gotOp+gotPayer, tc.maxPrice)
			}
		})
	}
}

func TestTicketStatus(t *testing.T) {
	tests := []struct {
		status, pendingBy uint64
		want              string
	}{
		{escrow.StatusOpen, escrow.PendingNone, "OPEN"},
		{escrow.StatusFrozen, escrow.PendingNone, "FROZEN"},
		{escrow.StatusPendingSettle, escrow.PendingByOperator, "PENDING(operator)"},
		{escrow.StatusPendingSettle, escrow.PendingByPayer, "PENDING(payer)"},
		{99, escrow.PendingNone, "UNKNOWN(99)"},
	}
	for _, tc := range tests {
		rec := &escrow.TicketRecord{Status: tc.status, PendingBy: tc.pendingBy}
		if got := ticketStatus(rec); got != tc.want {
			t.Fatalf("ticketStatus(%d/%d) = %q, want %q", tc.status, tc.pendingBy, got, tc.want)
		}
	}
}

// The verdict is the whole point of the listing, so each one must render as the
// call it authorizes and name the money that moves.
func TestResolution(t *testing.T) {
	deadline := uint64(time.Now().Add(time.Hour).Unix())
	base := func(status, pendingBy uint64) *escrow.TicketRecord {
		return &escrow.TicketRecord{
			ExpiresAt: deadline, Status: status, PendingBy: pendingBy,
			MaxPrice: 150_000, PendingAmount: 3_100, FeeBps: 200,
		}
	}
	tests := []struct {
		name    string
		verdict escrow.Verdict
		rec     *escrow.TicketRecord
		want    []string
	}{
		{"wait", escrow.VerdictWait, base(escrow.StatusOpen, escrow.PendingNone),
			[]string{"settle window still open"}},
		{"refund names the full lock", escrow.VerdictRefund, base(escrow.StatusOpen, escrow.PendingNone),
			[]string{"refundInactive", "0.150000 USDC"}},
		{"lapse names both sides", escrow.VerdictLapse, base(escrow.StatusPendingSettle, escrow.PendingByOperator),
			[]string{"settleLapsed", "pays operator 0.003100 USDC", "returns 0.146838 USDC"}},
		{"frozen names the admin as its only exit", escrow.VerdictArbitrate, base(escrow.StatusFrozen, escrow.PendingNone),
			[]string{"protocol admin", "MBR slot"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolution(ticketView{rec: tc.rec, verdict: tc.verdict})
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("resolution() = %q, want containing %q", got, want)
				}
			}
		})
	}
}

// A frozen ticket must never be offered as recoverable: refundInactive and
// settleLapsed both revert on it, and "retry once they settle" is a lie —
// only the protocol admin's resolveFrozen closes it.
func TestSkipReasonFrozenNeedsTheAdmin(t *testing.T) {
	rec := &escrow.TicketRecord{Status: escrow.StatusFrozen, ExpiresAt: uint64(time.Now().Add(-time.Hour).Unix())}
	got := skipReason(ticketView{rec: rec, verdict: escrow.VerdictArbitrate})
	for _, want := range []string{"frozen", "protocol admin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipReason() = %q, want it to contain %q", got, want)
		}
	}
}

// The two freeze paths leave otherwise identical boxes, so the detail view is
// where a payer learns which one froze the ticket. A box frozen before the
// contract recorded the fields must not be rendered as the Unix epoch.
func TestFrozenDetail(t *testing.T) {
	at := uint64(1_800_000_000)
	stamp := time.Unix(int64(at), 0).Format(time.RFC3339)
	tests := []struct {
		name         string
		origin, when uint64
		want         string
	}{
		{"settle mismatch", escrow.FreezeOriginSettleMismatch, at, "by settlement (payer and operator claims differed), at " + stamp},
		{"protest", escrow.FreezeOriginProtest, at, "by payer protest, at " + stamp},
		{"frozen before the fields existed", escrow.FreezeOriginNone, 0, "cause not recorded, time not recorded"},
		{"origin this build can't name", 9, at, "cause 9, at " + stamp},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &escrow.TicketRecord{Status: escrow.StatusFrozen, FreezeOrigin: tc.origin, FrozenAt: tc.when}
			if got := frozenDetail(rec); got != tc.want {
				t.Fatalf("frozenDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Only a frozen ticket gets the frozen row; the fields read zero on every other
// status, and a row there would describe a freeze that never happened.
func TestPrintTicketDetailFrozenRow(t *testing.T) {
	mk := func(status uint64) ticketView {
		rec := &escrow.TicketRecord{
			Status: status, FreezeOrigin: escrow.FreezeOriginProtest, FrozenAt: 1_800_000_000,
			ExpiresAt: uint64(time.Now().Add(-time.Hour).Unix()),
		}
		return ticketView{rec: rec, verdict: escrow.Classify(rec, time.Now())}
	}
	var frozen, open strings.Builder
	printTicketDetail(&frozen, mk(escrow.StatusFrozen), "")
	printTicketDetail(&open, mk(escrow.StatusOpen), "")
	if !strings.Contains(frozen.String(), "by payer protest") {
		t.Fatalf("frozen ticket detail missing the frozen row:\n%s", frozen.String())
	}
	if strings.Contains(open.String(), "by payer protest") {
		t.Fatalf("open ticket detail carries a frozen row:\n%s", open.String())
	}
}

// A ticket frozen over an operator claim keeps that claim on the box, and it is
// the amount in dispute; one frozen with no claim has nothing to show.
func TestPrintTicketDetailFrozenClaim(t *testing.T) {
	mk := func(pendingBy uint64) string {
		rec := &escrow.TicketRecord{
			Status: escrow.StatusFrozen, PendingBy: pendingBy, PendingAmount: 3_100,
			ExpiresAt: uint64(time.Now().Add(-time.Hour).Unix()),
		}
		var b strings.Builder
		printTicketDetail(&b, ticketView{rec: rec, verdict: escrow.Classify(rec, time.Now())}, "")
		return b.String()
	}
	if got := mk(escrow.PendingByOperator); !strings.Contains(got, "0.003100 USDC by operator") {
		t.Fatalf("frozen ticket with an operator claim hides it:\n%s", got)
	}
	if got := mk(escrow.PendingNone); strings.Contains(got, "claimed") {
		t.Fatalf("frozen ticket with no claim prints one:\n%s", got)
	}
}

func TestRelTime(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{90 * time.Second, "in 1m30s"},
		{-90 * time.Second, "1m30s ago"},
		{0, "in <1s"},
		{-time.Millisecond, "<1s ago"},
	}
	for _, tc := range tests {
		if got := relTime(tc.d); got != tc.want {
			t.Fatalf("relTime(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// The table is the deliverable: one row per stuck ticket, each naming the call
// that resolves it. Rendered here across every verdict at once, which is also
// the readable record of what the command looks like.
func TestWriteTicketTable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mk := func(id byte, opID, nodeID, status, pendingBy uint64, maxPrice, pending uint64, expiresIn time.Duration) ticketView {
		raw := make([]byte, 16)
		raw[0] = id
		rec := &escrow.TicketRecord{
			OperatorID: opID, NodeID: nodeID,
			Status: status, PendingBy: pendingBy,
			MaxPrice: maxPrice, PendingAmount: pending, FeeBps: 200,
			ExpiresAt:              uint64(now.Add(expiresIn).Unix()),
			SettlementGraceSeconds: 100,
		}
		return ticketView{
			idB64:   base64.StdEncoding.EncodeToString(raw),
			idRaw:   raw,
			rec:     rec,
			verdict: escrow.Classify(rec, now),
		}
	}
	views := []ticketView{
		mk(1, 12, 3, escrow.StatusOpen, escrow.PendingNone, 42_000, 0, time.Minute),
		mk(2, 12, 3, escrow.StatusOpen, escrow.PendingNone, 150_000, 0, -12*time.Minute),
		mk(3, 12, 3, escrow.StatusPendingSettle, escrow.PendingByOperator, 150_000, 3_100, -4*time.Minute),
		mk(4, 7, 1, escrow.StatusFrozen, escrow.PendingNone, 90_000, 0, -time.Hour),
	}
	var sb strings.Builder
	if err := writeTicketTable(&sb, views, now); err != nil {
		t.Fatalf("writeTicketTable: %v", err)
	}
	got := sb.String()
	t.Logf("\n%s", got)

	// Each verdict must reach the row it belongs to.
	for _, want := range []string{
		"settle window still open",                   // before the deadline
		"refundInactive — returns 0.150000 USDC",     // past deadline, no claim
		"settleLapsed — pays operator 0.003100 USDC", // past deadline, operator claim
		"disputed — waits for the protocol admin",    // frozen
		"PENDING(operator)", "FROZEN", "OPEN",
		"12/3", "7/1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("table missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(strings.TrimSpace(got), "\n"); lines != 4 { // header + 4 rows - 1
		t.Fatalf("got %d newlines, want 4 (header + 4 rows):\n%s", lines, got)
	}
}

// mkTicketView builds one live ticket box and asks escrow.Classify what the
// contract allows on it, so a case names a status and a deadline rather than a
// verdict — the mapping between the two is Classify's to own, not the test's.
func mkTicketView(id byte, status, pendingBy uint64, expiresIn time.Duration, now time.Time) ticketView {
	raw := make([]byte, 16)
	raw[0] = id
	rec := &escrow.TicketRecord{
		Status: status, PendingBy: pendingBy, MaxPrice: 150_000, FeeBps: 200,
		ExpiresAt:              uint64(now.Add(expiresIn).Unix()),
		SettlementGraceSeconds: 100,
	}
	return ticketView{idB64: base64.StdEncoding.EncodeToString(raw), rec: rec, verdict: escrow.Classify(rec, now)}
}

// closeDepositSummaryMarkers is every summary line closeDepositBlockedError can
// emit, one marker each. Each marker appears in the summary only — the
// per-ticket lines above it use resolution()'s different wording — so a marker's
// presence is exactly "that bucket's line was printed".
//
// Absence is the half that needs saying out loud: each line sits behind a
// `> 0` guard, and a guard slipping to `>= 0` prints "0 recoverable now — run
// `zs-proxy tickets recover --all`", pointing the payer at a command that will
// do nothing. No presence-only assertion can see that.
var closeDepositSummaryMarkers = []string{
	"recoverable now",
	"still inside its settle window",
	"frozen (disputed)",
	"doesn't recognize",
}

// close-deposit used to say "retry once they settle" for any open ticket. That
// is a lie for a frozen one — only the protocol admin's resolveFrozen closes
// it — and needless for a past-deadline one the owner can clear right now.
func TestCloseDepositBlockedError(t *testing.T) {
	now := time.Now()
	mk := func(id byte, status, pendingBy uint64, expiresIn time.Duration) ticketView {
		return mkTicketView(id, status, pendingBy, expiresIn, now)
	}
	tests := []struct {
		name  string
		views []ticketView
		// want is matched literally and each entry carries its count, so no two
		// summary lines can swap their `%d` argument unnoticed.
		want []string
	}{
		{
			"frozen never promises a settle",
			[]ticketView{mk(1, escrow.StatusFrozen, escrow.PendingNone, -time.Hour)},
			[]string{"1 ticket(s) still open", "1 frozen (disputed)", "protocol admin's resolution", "No call this wallet can make will clear them"},
		},
		{
			"past deadline points at recover",
			[]ticketView{mk(2, escrow.StatusOpen, escrow.PendingNone, -time.Hour)},
			[]string{"1 recoverable now — run `zs-proxy tickets recover --all`, then retry."},
		},
		{
			"still in window says retry",
			[]ticketView{mk(3, escrow.StatusOpen, escrow.PendingNone, time.Hour)},
			[]string{"1 still inside its settle window — retry once they settle."},
		},
		// A status Classify can't name is not "too early" — waiting never makes
		// a call legal on it (escrow/classify.go, VerdictUnknown). This used to
		// fall into the waiting bucket and contradict the per-ticket line right
		// above it, which the absence sweep below now also pins.
		{
			"unrecognized state never promises a settle",
			[]ticketView{mk(4, escrow.StatusSettled, escrow.PendingNone, -time.Hour)},
			[]string{"1 in a state this build doesn't recognize", "upgrading zs-proxy"},
		},
		// Deliberately distinct cardinalities. With one ticket per class every
		// bucket held 1 and any permutation of the four count arguments passed.
		{
			"mixed names every class with its own count",
			[]ticketView{
				mk(1, escrow.StatusFrozen, escrow.PendingNone, -time.Hour),
				mk(2, escrow.StatusFrozen, escrow.PendingNone, -time.Hour),
				mk(3, escrow.StatusFrozen, escrow.PendingNone, -time.Hour),
				mk(4, escrow.StatusOpen, escrow.PendingNone, -time.Hour),
				mk(5, escrow.StatusPendingSettle, escrow.PendingByOperator, -time.Hour),
				mk(6, escrow.StatusOpen, escrow.PendingNone, time.Hour),
				mk(7, escrow.StatusSettled, escrow.PendingNone, -time.Hour),
			},
			[]string{
				"7 ticket(s) still open", "2 recoverable now", "1 still inside its settle window",
				"3 frozen (disputed)", "1 in a state this build doesn't recognize",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := closeDepositBlockedError(tc.views).Error()
			t.Logf("\n%s", got)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("want %q in:\n%s", want, got)
				}
			}
			// The waiting bucket is the only one a settle ever reaches, so this
			// phrase must track it exactly — appearing for a frozen or
			// unrecognized ticket is the original defect.
			wantWaiting := slices.ContainsFunc(tc.want, func(w string) bool {
				return strings.Contains(w, "still inside its settle window")
			})
			if promised := strings.Contains(got, "retry once they settle"); promised != wantWaiting {
				t.Fatalf("promised a settle=%v, want %v — only the waiting bucket settles:\n%s", promised, wantWaiting, got)
			}
			for _, marker := range closeDepositSummaryMarkers {
				if slices.ContainsFunc(tc.want, func(w string) bool { return strings.Contains(w, marker) }) {
					continue
				}
				if strings.Contains(got, marker) {
					t.Fatalf("printed the %q line for an empty bucket:\n%s", marker, got)
				}
			}
		})
	}
}

// runRecover's gate is the only place recoverable() is consumed, so the
// predicate being right buys nothing unless this call site is pinned too:
// inverting the condition, or narrowing it to Refund alone, changes what
// `zs-proxy tickets recover` posts on chain. Reachable without a chain because
// the dry-run path returns before the keystore and algod client are built.
func TestRunRecoverDryRunGate(t *testing.T) {
	now := time.Now()
	views := []ticketView{
		mkTicketView(1, escrow.StatusOpen, escrow.PendingNone, -time.Hour, now),                // Refund
		mkTicketView(2, escrow.StatusPendingSettle, escrow.PendingByOperator, -time.Hour, now), // Lapse
		mkTicketView(3, escrow.StatusOpen, escrow.PendingNone, time.Hour, now),                 // Wait
		mkTicketView(4, escrow.StatusFrozen, escrow.PendingNone, -time.Hour, now),              // Arbitrate
		mkTicketView(5, escrow.StatusSettled, escrow.PendingNone, -time.Hour, now),             // Unknown
	}
	wantActionable := map[string]bool{
		views[0].idB64: true, views[1].idB64: true,
		views[2].idB64: false, views[3].idB64: false, views[4].idB64: false,
	}

	var buf bytes.Buffer
	// nil runtimeEnv on purpose: if the gate ever starts resolving something off
	// the chain before deciding, this panics rather than passing quietly.
	if err := runRecover(context.Background(), &buf, nil, views, true); err != nil {
		t.Fatalf("runRecover: %v", err)
	}
	got := buf.String()
	t.Logf("\n%s", got)

	byID := make(map[string]string, len(views))
	for line := range strings.SplitSeq(got, "\n") {
		if id, _, ok := strings.Cut(line, "  "); ok {
			byID[id] = line
		}
	}
	for id, actionable := range wantActionable {
		line, ok := byID[id]
		if !ok {
			t.Fatalf("ticket %s listed nowhere in:\n%s", id, got)
		}
		if skipped := strings.Contains(line, "— skip ("); skipped == actionable {
			t.Errorf("ticket %s: want actionable=%v, got line %q", id, actionable, line)
		}
	}
	if !strings.Contains(got, "2 actionable.") {
		t.Errorf("want exactly 2 actionable in:\n%s", got)
	}
}

// The four commands that report on stuck tickets read their buckets from here,
// so a verdict this build can't name must land somewhere — and specifically not
// in `waiting`, which is the one bucket whose copy promises the deadline fixes
// it. escrow.Verdict is a plain uint8 with no compiler-enforced exhaustiveness,
// so the sum is what pins it.
func TestTallyVerdicts(t *testing.T) {
	views := func(vs ...escrow.Verdict) []ticketView {
		out := make([]ticketView, 0, len(vs))
		for _, v := range vs {
			out = append(out, ticketView{verdict: v})
		}
		return out
	}
	tests := []struct {
		name string
		in   []ticketView
		want verdictTally
	}{
		{"refund and lapse are both recoverable", views(escrow.VerdictRefund, escrow.VerdictLapse), verdictTally{recoverable: 2}},
		{"wait is the only waiting verdict", views(escrow.VerdictWait), verdictTally{waiting: 1}},
		{"arbitrate is frozen", views(escrow.VerdictArbitrate), verdictTally{frozen: 1}},
		{"unknown is not waiting", views(escrow.VerdictUnknown), verdictTally{unknown: 1}},
		// The zero value and a verdict added by a newer escrow both mean "no
		// call is known to be legal", which is Unknown's answer, not Wait's.
		{"unnamed verdicts fail closed into unknown", views(escrow.Verdict(0), escrow.Verdict(99)), verdictTally{unknown: 2}},
		{"empty", nil, verdictTally{}},
		{
			"every class at once",
			views(escrow.VerdictWait, escrow.VerdictRefund, escrow.VerdictLapse, escrow.VerdictArbitrate, escrow.VerdictUnknown),
			verdictTally{recoverable: 2, waiting: 1, frozen: 1, unknown: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tallyVerdicts(tc.in); got != tc.want {
				t.Fatalf("tallyVerdicts() = %+v, want %+v", got, tc.want)
			}
		})
	}
	// Every case above names all four buckets, so the struct comparison already
	// implies the sum — asserting it alongside them would be true by
	// construction. Standing alone it pins what a table of hand-picked verdicts
	// can't: escrow.Verdict is a uint8 with no compiler-enforced exhaustiveness,
	// so sweep the whole domain and assert only that nothing falls out.
	t.Run("no verdict in the domain is dropped", func(t *testing.T) {
		in := make([]ticketView, 0, 256)
		for v := range 256 {
			in = append(in, ticketView{verdict: escrow.Verdict(v)})
		}
		got := tallyVerdicts(in)
		if sum := got.recoverable + got.waiting + got.frozen + got.unknown; sum != len(in) {
			t.Fatalf("buckets sum to %d over %d verdicts — one was dropped: %+v", sum, len(in), got)
		}
	})
}

// recoverable() gates `tickets recover`; postResolution is what composes the
// call. Drift between them turns a clean skip into a failed submission, so the
// check has to run against postResolution itself — comparing recoverable() to a
// table of expected answers only restates the predicate.
//
// One direction is reachable without a chain: every verdict recoverable()
// rejects must be one postResolution refuses before it touches the client. The
// other direction isn't — composing a real call needs algod — so postResolution
// entering a case at all is the signal, via the nil client it then dereferences.
func TestPostResolutionRefusesWhatRecoverableSkips(t *testing.T) {
	all := []escrow.Verdict{
		escrow.Verdict(0), escrow.VerdictWait, escrow.VerdictRefund,
		escrow.VerdictLapse, escrow.VerdictArbitrate, escrow.VerdictUnknown,
	}
	for _, v := range all {
		if recoverable(v) {
			continue
		}
		t.Run(v.String(), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("postResolution(%v) tried to compose a call for a verdict recoverable() skips — "+
						"runRecover would never send it, so the two switches have drifted (panic: %v)", v, r)
				}
			}()
			_, err := postResolution(context.Background(), nil,
				ticketView{verdict: v, rec: &escrow.TicketRecord{}}, nil, "")
			if err == nil || !strings.Contains(err.Error(), "not actionable") {
				t.Fatalf("postResolution(%v) = %v, want a \"not actionable\" refusal", v, err)
			}
		})
	}
}
