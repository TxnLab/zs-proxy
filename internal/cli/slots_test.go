/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/escrow"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/config"
)

const testTicketMbr = 115_500

func TestParseSlots(t *testing.T) {
	tests := []struct {
		arg     string
		want    uint64
		wantErr string
	}{
		{arg: "1", want: 1},
		{arg: "25", want: 25},
		// Base 10 explicitly: with base 0, `010` would resize to 8 slots and
		// `0x10` to 16, both silently and both wrong.
		{arg: "010", want: 10},
		{arg: "0x10", wantErr: "invalid slot count"},
		{arg: "100000", want: maxConcurrentSlots},
		{arg: "100001", wantErr: "above the maximum"},
		// The wrap band: slots*ticketMbr overflows uint64, producing a tiny
		// target that turns this RAISE into a drain of the pool's free
		// headroom. Must be refused before it reaches any sizing.
		{arg: "159712069902248", wantErr: "above the maximum"},
		{arg: "18446744073709551615", wantErr: "above the maximum"},
		// 0 means "use the built-in default" in config, so accepting it here
		// would set the opposite of what someone typing a number intends —
		// and reading it as "no limit" would be backwards entirely.
		{arg: "0", wantErr: "at least 1"},
		{arg: "-1", wantErr: "invalid slot count"},
		{arg: "ten", wantErr: "invalid slot count"},
		{arg: "2.5", wantErr: "invalid slot count"},
		{arg: "", wantErr: "invalid slot count"},
	}
	for _, tc := range tests {
		t.Run(tc.arg, func(t *testing.T) {
			got, err := parseSlots(tc.arg)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSlots(%q) = %d, want an error", tc.arg, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSlots(%q): %v", tc.arg, err)
			}
			if got != tc.want {
				t.Errorf("parseSlots(%q) = %d, want %d", tc.arg, got, tc.want)
			}
		})
	}
}

// TestParseSlotsZeroNamesTheDefault: the refusal has to tell the user what to
// do instead, and the default it names must be the real one — not a literal
// that can drift from zs.DefaultDepositSlots.
func TestParseSlotsZeroNamesTheDefault(t *testing.T) {
	_, err := parseSlots("0")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "concurrent_slots") {
		t.Errorf("refusal should name the config key: %q", err)
	}
	if !strings.Contains(err.Error(), "10") || zs.DefaultDepositSlots != 10 {
		t.Errorf("refusal should name the live default %d: %q", zs.DefaultDepositSlots, err)
	}
}

func poolFor(deposited, open uint64) poolStatus {
	return poolStatus{
		dep:       escrow.PayerDeposit{AlgoBalance: deposited, OpenTickets: open},
		ticketMbr: testTicketMbr,
	}
}

// poolWithQuota adds the free-quota box term, which poolFor leaves at zero.
// Without a fixture that sets it, every call site could pass a hard 0 for that
// argument and no test would notice.
func poolWithQuota(deposited, open, quotaMbrDue uint64) poolStatus {
	ps := poolFor(deposited, open)
	ps.quotaMbrDue = quotaMbrDue
	return ps
}

// TestSlotsViewBackedVsConfigured pins the distinction the whole display rests
// on: `configured` is the funding TARGET, `backed` is what the deposit actually
// buys, and the contract admits against backed. A view that reported the
// configured number as the limit would tell a user with an under-funded pool
// they have headroom they don't have.
func TestSlotsViewBackedVsConfigured(t *testing.T) {
	tests := []struct {
		name          string
		configured    uint64
		deposited     uint64
		open          uint64
		wantBacked    uint64
		wantShortfall bool
		wantExcess    bool
	}{
		{
			name: "pool matches the target", configured: 10,
			deposited: 10 * testTicketMbr, wantBacked: 10,
		},
		{
			name: "pool under the target", configured: 10,
			deposited: 4 * testTicketMbr, wantBacked: 4, wantShortfall: true,
		},
		{
			name: "pool over the target", configured: 10,
			deposited: 14 * testTicketMbr, wantBacked: 14, wantExcess: true,
		},
		{
			// 12 deposited against a target of 10 is a genuine 2-slot excess,
			// and 5 tickets in flight must not change that: sizing is gross.
			name: "open tickets do not reduce backed slots", configured: 10,
			deposited: 12 * testTicketMbr, open: 5, wantBacked: 12, wantExcess: true,
		},
		{
			// Sizing is invariant to in-flight tickets on purpose: counting
			// them would read normal load as a funding gap, and the pool would
			// then ratchet up with concurrency rather than with need.
			name: "in-flight tickets do not create a shortfall", configured: 10,
			deposited: 10 * testTicketMbr, open: 9, wantBacked: 10,
		},
		{
			// A partial slot must TRUNCATE, not round. Every other fixture is
			// an exact multiple, where truncation and rounding agree — so
			// without this row the choice between them is unpinned, and
			// rounding up would advertise a slot the contract won't admit.
			name: "a partial slot rounds down", configured: 10,
			deposited: 10*testTicketMbr + testTicketMbr/2 + 1, wantBacked: 10, wantExcess: true,
		},
		{
			name: "just under a whole slot still rounds down", configured: 10,
			deposited: 4*testTicketMbr - 1, wantBacked: 3, wantShortfall: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := slotsViewFrom(tc.configured, poolFor(tc.deposited, tc.open))
			if !v.known {
				t.Fatal("view should be known when ticketMbr is set")
			}
			if v.backed != tc.wantBacked {
				t.Errorf("backed = %d, want %d", v.backed, tc.wantBacked)
			}
			if (v.shortfall > 0) != tc.wantShortfall {
				t.Errorf("shortfall = %d, wantShortfall = %v", v.shortfall, tc.wantShortfall)
			}
			if (v.excess > 0) != tc.wantExcess {
				t.Errorf("excess = %d, wantExcess = %v", v.excess, tc.wantExcess)
			}
			if v.shortfall > 0 && v.excess > 0 {
				t.Error("shortfall and excess are mutually exclusive")
			}
		})
	}
}

// TestSlotsViewNeverBacksBelowOpen: the contract's invariant makes this
// unreachable, but rendering a denominator below the numerator ("5 of 3 in
// use") would be nonsense on a screen, so the floor is asserted.
func TestSlotsViewNeverBacksBelowOpen(t *testing.T) {
	v := slotsViewFrom(10, poolFor(2*testTicketMbr, 5))
	if v.backed < v.open {
		t.Errorf("backed %d < open %d", v.backed, v.open)
	}
}

func TestRenderSlots(t *testing.T) {
	t.Run("unknown pool says so and stops", func(t *testing.T) {
		// Routed through the REAL constructor with no ticket MBR, not a hand-
		// built slotsView{}: a hand-built literal can't observe whether
		// slotsViewFrom actually detects the unknown case. If it didn't, the
		// render path divides by a zero ticketMbr and `zs slots` panics.
		v := slotsViewFrom(10, poolStatus{dep: escrow.PayerDeposit{AlgoBalance: 5_000_000}})
		if v.known {
			t.Fatal("a pool with no per-ticket MBR must not be reported as known")
		}
		lines := renderSlots(10, v)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "configured:") {
			t.Error("configured line should always render")
		}
		if !strings.Contains(joined, "unknown") {
			t.Errorf("expected an unknown-pool line:\n%s", joined)
		}
		for _, absent := range []string{"backed:", "in use:", "per slot:"} {
			if strings.Contains(joined, absent) {
				t.Errorf("must not render %q without pool data:\n%s", absent, joined)
			}
		}
	})

	t.Run("full view renders every figure", func(t *testing.T) {
		lines := renderSlots(10, slotsViewFrom(10, poolFor(12*testTicketMbr, 2)))
		joined := strings.Join(lines, "\n")
		for _, want := range []string{
			"configured:  10 slot(s)",
			"zs.concurrent_slots",
			// The full line, including the deposited figure: stopping at the
			// slot count leaves the ALGO half of that same line unasserted.
			"backed:      12 slot(s)   " + microAlgos(12*testTicketMbr) + " deposited",
			"in use:      2 slot(s)",
			"per slot:    0.115500 ALGO",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %q:\n%s", want, joined)
			}
		}
	})

	t.Run("shortfall note points at the fix", func(t *testing.T) {
		joined := strings.Join(renderSlots(10, slotsViewFrom(10, poolFor(4*testTicketMbr, 0))), "\n")
		if !strings.Contains(joined, "short of 10 slot(s)") {
			t.Errorf("missing shortfall note:\n%s", joined)
		}
		if !strings.Contains(joined, "zs-proxy slots 10") {
			t.Errorf("shortfall note should name the command that fixes it:\n%s", joined)
		}
	})

	t.Run("excess note offers the reclaim", func(t *testing.T) {
		joined := strings.Join(renderSlots(10, slotsViewFrom(10, poolFor(20*testTicketMbr, 0))), "\n")
		if !strings.Contains(joined, "more than 10 slot(s)") {
			t.Errorf("missing excess note:\n%s", joined)
		}
	})

	t.Run("balanced pool has no note", func(t *testing.T) {
		joined := strings.Join(renderSlots(10, slotsViewFrom(10, poolFor(10*testTicketMbr, 3))), "\n")
		if strings.Contains(joined, "note:") {
			t.Errorf("a pool at target should carry no note:\n%s", joined)
		}
	})
}

// TestSlotsPlan pins what the confirmation prompt is consenting to. The
// distinction that matters is the third case: an oversized pool whose ALGO is
// held by in-flight tickets must not read as "reclaiming 0.000000 ALGO".
func TestSlotsPlan(t *testing.T) {
	tests := []struct {
		name                            string
		current, want                   uint64
		shortfall, excess, withdrawable uint64
		wantContains                    []string
		wantAbsent                      []string
	}{
		{
			// The µALGO figure is the whole point of the line — it is what the
			// user is consenting to. Matching only the word "depositing" would
			// pass while the prompt says 0.000000 ALGO.
			name: "raise deposits", current: 10, want: 25, shortfall: 15 * testTicketMbr,
			wantContains: []string{"10 → 25", "depositing up to " + microAlgos(15*testTicketMbr), "recoverable"},
		},
		{
			name: "lower reclaims", current: 25, want: 10,
			excess: 15 * testTicketMbr, withdrawable: 15 * testTicketMbr,
			wantContains: []string{"25 → 10", "reclaiming " + microAlgos(15*testTicketMbr)},
		},
		{
			// The case that separates the two figures — a busy oversized pool,
			// which is exactly what the function exists to describe. With both
			// fixtures equal, printing `excess` instead of `withdrawable` is
			// invisible: the user consents to reclaiming 1.7325 ALGO and 0.5775
			// arrives.
			name: "lower reclaims only what is free", current: 25, want: 10,
			excess: 15 * testTicketMbr, withdrawable: 5 * testTicketMbr,
			wantContains: []string{"reclaiming " + microAlgos(5*testTicketMbr)},
			wantAbsent:   []string{microAlgos(15 * testTicketMbr)},
		},
		{
			name: "lower blocked by in-flight tickets", current: 25, want: 10,
			excess: 15 * testTicketMbr, withdrawable: 0,
			wantContains: []string{"nothing reclaimable", "in-flight"},
			wantAbsent:   []string{"reclaiming 0"},
		},
		{
			name: "already at target is config-only", current: 10, want: 10,
			wantContains: []string{"already matches", "config only"},
			wantAbsent:   []string{"depositing", "reclaiming"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := slotsPlan(algorandMBRPool, tc.current, tc.want, tc.shortfall, tc.excess, tc.withdrawable)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("plan %q missing %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("plan %q should not contain %q", got, absent)
				}
			}
		})
	}
}

// TestWithdrawableFor asserts on the PRODUCTION function, not on arithmetic
// recomputed in the test. Recomputing `min(excess, headroom)` here would mirror
// the predicate and pass no matter what the real code does — and the real code
// decides how much ALGO leaves the pool.
//
// The at-target row is the one that matters most: a correct pool must reclaim
// NOTHING. If the bound ever inverted, `zs slots 10` on an already-correct pool
// would take `min`'s other operand — the entire unreserved balance — and drain
// the pool while reporting success.
func TestWithdrawableFor(t *testing.T) {
	tests := []struct {
		name      string
		want      uint64
		deposited uint64
		open      uint64
		expect    uint64
	}{
		{
			name: "at target reclaims nothing", want: 10,
			deposited: 10 * testTicketMbr, expect: 0,
		},
		{
			name: "at target with tickets in flight reclaims nothing", want: 10,
			deposited: 10 * testTicketMbr, open: 4, expect: 0,
		},
		{
			name: "under target reclaims nothing", want: 10,
			deposited: 4 * testTicketMbr, expect: 0,
		},
		{
			name: "idle oversized pool reclaims the whole excess", want: 10,
			deposited: 25 * testTicketMbr, expect: 15 * testTicketMbr,
		},
		{
			// Oversized on paper, mostly locked in practice: the contract
			// admits only the unreserved balance and reverts on an over-ask.
			name: "busy oversized pool reclaims only the unreserved part", want: 10,
			deposited: 25 * testTicketMbr, open: 20, expect: 5 * testTicketMbr,
		},
		{
			name: "every slot in flight reclaims nothing", want: 10,
			deposited: 25 * testTicketMbr, open: 25, expect: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := poolFor(tc.deposited, tc.open)
			if got := withdrawableFor(tc.want, ps); got != tc.expect {
				t.Errorf("withdrawableFor = %d, want %d", got, tc.expect)
			}
		})
	}

	// And the two terms really can differ — otherwise the bound above would be
	// satisfied by either operand and the table would prove nothing.
	ps := poolFor(25*testTicketMbr, 20)
	if poolExcess(10, ps.ticketMbr, 0, ps.dep) <= zs.FreePoolHeadroom(ps.ticketMbr, ps.dep) {
		t.Fatal("this fixture must have excess strictly above headroom")
	}
}

// TestWithdrawableForKeepsTheFreeQuotaTerm: the quota box's MBR is part of the
// target, so a shrink must leave it behind. Dropping the term one layer up —
// passing a hard 0 where quotaMbrDue belongs — reclaims it, and the very next
// `zs status` reports "short of N slot(s)" while the next `zs fund` puts it
// back. zs.DepositExcess has its own test for this; nothing pinned the CLI's
// wiring of it, because every fixture left quotaMbrDue at zero.
func TestWithdrawableForKeepsTheFreeQuotaTerm(t *testing.T) {
	const quota = 22_500
	ps := poolWithQuota(25*testTicketMbr+quota, 0, quota)

	got := withdrawableFor(10, ps)
	want := uint64(15 * testTicketMbr) // the quota term stays in the pool
	if got != want {
		t.Errorf("withdrawableFor = %d, want %d — the free-quota box MBR must not be reclaimed", got, want)
	}
	// Same term, same reasoning, on the display path.
	if v := slotsViewFrom(10, ps); v.excess != want {
		t.Errorf("slotsViewFrom excess = %d, want %d", v.excess, want)
	}
}

// TestSlotsViewShortfallKeepsTheFreeQuotaTerm is the raise-side twin: a pool
// holding exactly N slots is SHORT by the quota term it still owes.
func TestSlotsViewShortfallKeepsTheFreeQuotaTerm(t *testing.T) {
	const quota = 22_500
	v := slotsViewFrom(10, poolWithQuota(10*testTicketMbr, 0, quota))
	if v.shortfall != quota {
		t.Errorf("shortfall = %d, want the quota term %d", v.shortfall, quota)
	}
	if v.excess != 0 {
		t.Errorf("excess = %d, want 0", v.excess)
	}
}

// TestDecideSlots pins everything the command body hands to the helpers below
// it. That wiring is unreachable by any other test — it needs a chain, a
// keystore and a config file — so before this existed, swapping current/want,
// passing excess where withdrawable belongs, or losing the dry-run guard were
// all one-token edits with a green suite and real money at stake.
func TestDecideSlots(t *testing.T) {
	const quota = 22_500

	t.Run("a raise deposits, states the deposit, and asks first", func(t *testing.T) {
		d := decideSlots(10, 25, poolFor(10*testTicketMbr, 0), false)
		if d.resize.act != slotsActionDeposit {
			t.Errorf("act = %d, want deposit", d.resize.act)
		}
		if d.resize.want != 25 {
			t.Errorf("want = %d, want 25 — the REQUESTED count, not the current one", d.resize.want)
		}
		if !d.needsConsent {
			t.Error("a deposit must be consented to")
		}
		if d.lowered {
			t.Error("a raise is not a lowering")
		}
		// Direction and figure both, so a swapped (current, want) is visible.
		if !strings.Contains(d.plan, "10 → 25") {
			t.Errorf("plan %q should read 10 → 25", d.plan)
		}
		if !strings.Contains(d.plan, microAlgos(15*testTicketMbr)) {
			t.Errorf("plan %q should name the deposit amount", d.plan)
		}
	})

	t.Run("a lowering reclaims only the free part and says so", func(t *testing.T) {
		// 25 slots deposited, 20 in flight: oversized by 15, but only 5 free.
		d := decideSlots(25, 10, poolFor(25*testTicketMbr, 20), false)
		if d.resize.act != slotsActionWithdraw {
			t.Errorf("act = %d, want withdraw", d.resize.act)
		}
		if d.resize.withdrawable != 5*testTicketMbr {
			t.Errorf("withdrawable = %d, want %d — an over-ask reverts on chain",
				d.resize.withdrawable, 5*testTicketMbr)
		}
		if d.resize.excess != 15*testTicketMbr {
			t.Errorf("excess = %d, want %d", d.resize.excess, 15*testTicketMbr)
		}
		if d.resize.openTickets != 20 {
			t.Errorf("openTickets = %d, want 20 — needed to explain what holds the rest", d.resize.openTickets)
		}
		if !d.lowered {
			t.Error("25 → 10 is a lowering")
		}
		// The plan must quote what will ACTUALLY be reclaimed.
		if !strings.Contains(d.plan, microAlgos(5*testTicketMbr)) {
			t.Errorf("plan %q should name the reclaimable amount", d.plan)
		}
		if strings.Contains(d.plan, microAlgos(15*testTicketMbr)) {
			t.Errorf("plan %q quotes the full excess, which is not what arrives", d.plan)
		}
	})

	t.Run("an at-target pool moves nothing and asks nothing", func(t *testing.T) {
		d := decideSlots(10, 10, poolFor(10*testTicketMbr, 3), false)
		if d.resize.act != slotsActionNone {
			t.Errorf("act = %d, want none", d.resize.act)
		}
		if d.needsConsent {
			t.Error("a config-only change has nothing to consent to")
		}
		if d.resize.withdrawable != 0 {
			t.Errorf("withdrawable = %d, want 0 — an at-target pool must not be drained", d.resize.withdrawable)
		}
	})

	t.Run("dry run decides to do nothing, whatever the pool says", func(t *testing.T) {
		for _, ps := range []poolStatus{
			poolFor(10*testTicketMbr, 0),  // would deposit
			poolFor(25*testTicketMbr, 0),  // would withdraw
			poolFor(25*testTicketMbr, 25), // would do nothing anyway
		} {
			d := decideSlots(10, 25, ps, true)
			if d.resize.act != slotsActionNone {
				t.Errorf("act = %d under --dry-run, want none", d.resize.act)
			}
			if !d.resize.dryRun {
				t.Error("--dry-run must reach applySlots, which owns the write half of the promise")
			}
			if d.needsConsent {
				t.Error("--dry-run must not prompt")
			}
			// It still describes what it WOULD do — that is the point of the flag.
			if d.plan == "" {
				t.Error("--dry-run should still render a plan")
			}
			// And the promise itself: nothing submitted AND nothing written.
			f := &fakeExec{}
			var out strings.Builder
			if err := applySlots(context.Background(), &out, d.resize, f.exec()); err != nil {
				t.Fatalf("applySlots: %v", err)
			}
			if len(f.calls) != 0 {
				t.Errorf("--dry-run performed %v; it must write nothing and submit nothing", f.calls)
			}
		}
	})

	t.Run("the free-quota term survives the wiring", func(t *testing.T) {
		d := decideSlots(25, 10, poolWithQuota(25*testTicketMbr+quota, 0, quota), false)
		if d.resize.withdrawable != 15*testTicketMbr {
			t.Errorf("withdrawable = %d, want %d — the quota box MBR must stay in the pool",
				d.resize.withdrawable, 15*testTicketMbr)
		}
	})

	t.Run("plan and action never contradict each other", func(t *testing.T) {
		// A busy oversized pool: oversized on paper, nothing free to take. The
		// plan must not promise a reclaim the action won't perform.
		d := decideSlots(25, 10, poolFor(25*testTicketMbr, 25), false)
		if d.resize.act != slotsActionNone {
			t.Errorf("act = %d, want none when nothing is reclaimable", d.resize.act)
		}
		if strings.Contains(d.plan, "reclaiming") {
			t.Errorf("plan %q promises a reclaim that will not happen", d.plan)
		}
		if d.needsConsent {
			t.Error("nothing moves, so nothing to consent to")
		}
	})
}

// fakeExec records what applySlots did, in order.
type fakeExec struct {
	calls        []string
	depositErr   error
	withdrawErr  error
	writeErr     error
	wroteValue   uint64
	depositedFor uint64
	withdrewAmt  uint64
}

func (f *fakeExec) exec() slotsExec {
	return slotsExec{
		deposit: func(_ context.Context, want uint64) error {
			f.calls = append(f.calls, "deposit")
			f.depositedFor = want
			return f.depositErr
		},
		withdraw: func(_ context.Context, amount uint64) (string, error) {
			f.calls = append(f.calls, "withdraw")
			f.withdrewAmt = amount
			return "TXID", f.withdrawErr
		},
		writeConfig: func(v uint64) error {
			f.calls = append(f.calls, "write")
			f.wroteValue = v
			return f.writeErr
		},
		configPath: "/tmp/config.yaml",
	}
}

// TestApplySlotsOrdering pins the chain-before-config sequence and the value
// written. Everything here was previously unreachable by any test: `zs slots
// 25` writing `10` was a green-suite mutation.
func TestApplySlotsOrdering(t *testing.T) {
	tests := []struct {
		name      string
		plan      slotsResize
		wantCalls []string
	}{
		{
			name:      "deposit runs before the config write",
			plan:      slotsResize{want: 25, act: slotsActionDeposit},
			wantCalls: []string{"deposit", "write"},
		},
		{
			name:      "withdraw runs before the config write",
			plan:      slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 100, excess: 100},
			wantCalls: []string{"withdraw", "write"},
		},
		{
			name:      "a no-op writes the config and touches no chain",
			plan:      slotsResize{want: 10, act: slotsActionNone},
			wantCalls: []string{"write"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{}
			var out strings.Builder
			if err := applySlots(context.Background(), &out, tc.plan, f.exec()); err != nil {
				t.Fatalf("applySlots: %v", err)
			}
			if strings.Join(f.calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Errorf("calls = %v, want %v", f.calls, tc.wantCalls)
			}
			if f.wroteValue != tc.plan.want {
				t.Errorf("wrote %d to config, want the requested %d", f.wroteValue, tc.plan.want)
			}
		})
	}
}

// TestApplySlotsPassesTheRightAmounts: the deposit targets the requested SLOT
// COUNT while the withdraw takes a µALGO AMOUNT. Swapping them is a type-clean
// mistake that would withdraw 4 µALGO or deposit for 100 slots.
func TestApplySlotsPassesTheRightAmounts(t *testing.T) {
	f := &fakeExec{}
	var out strings.Builder
	_ = applySlots(context.Background(), &out, slotsResize{want: 25, act: slotsActionDeposit}, f.exec())
	if f.depositedFor != 25 {
		t.Errorf("deposit target = %d, want the slot count 25", f.depositedFor)
	}

	f = &fakeExec{}
	out.Reset()
	_ = applySlots(context.Background(), &out,
		slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 5 * testTicketMbr, excess: 5 * testTicketMbr}, f.exec())
	if f.withdrewAmt != 5*testTicketMbr {
		t.Errorf("withdrew %d, want the µALGO amount %d", f.withdrewAmt, 5*testTicketMbr)
	}
}

// TestApplySlotsFailedWriteAfterAChainMove: the user's ALGO has already moved,
// so the error must say the pool WAS resized. Reporting a plain write failure
// would read as "nothing happened" and invite a re-run that moves ALGO twice.
func TestApplySlotsFailedWriteAfterAChainMove(t *testing.T) {
	f := &fakeExec{writeErr: errors.New("disk full")}
	var out strings.Builder
	err := applySlots(context.Background(), &out, slotsResize{want: 25, act: slotsActionDeposit}, f.exec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "pool was resized") {
		t.Errorf("error %q must say the pool was already resized", err)
	}

	// The withdraw arm must set the same context: a successful reclaim followed
	// by a write failure has already moved ALGO, so reporting a plain write
	// error reads as "nothing happened" and invites a re-run that moves it
	// twice — in the opposite direction this time.
	f = &fakeExec{writeErr: errors.New("disk full")}
	out.Reset()
	err = applySlots(context.Background(), &out,
		slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 100, excess: 100}, f.exec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "pool was resized") {
		t.Errorf("error %q after a successful withdraw must say the pool was already resized", err)
	}

	// With no chain move there is nothing to warn about, so the message must
	// NOT claim a resize that never happened.
	f = &fakeExec{writeErr: errors.New("disk full")}
	out.Reset()
	err = applySlots(context.Background(), &out, slotsResize{want: 10, act: slotsActionNone}, f.exec())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "pool was resized") {
		t.Errorf("error %q claims a resize that did not happen", err)
	}
}

// TestApplySlotsChainFailureSkipsTheWrite: a written target the pool doesn't
// back is a number that lies to every later read.
func TestApplySlotsChainFailureSkipsTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeExec
		plan slotsResize
	}{
		{"deposit fails", &fakeExec{depositErr: errors.New("no algo")}, slotsResize{want: 25, act: slotsActionDeposit}},
		{"withdraw fails", &fakeExec{withdrawErr: errors.New("reverted")}, slotsResize{want: 4, act: slotsActionWithdraw, withdrawable: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := applySlots(context.Background(), &out, tc.plan, tc.f.exec()); err == nil {
				t.Fatal("expected an error")
			}
			for _, c := range tc.f.calls {
				if c == "write" {
					t.Error("the config was written after the chain leg failed")
				}
			}
		})
	}
}

// TestApplySlotsReportsTheUnreclaimableRemainder: taking less than the plan
// announced is fine; doing it silently is not.
func TestApplySlotsReportsTheUnreclaimableRemainder(t *testing.T) {
	f := &fakeExec{}
	var out strings.Builder
	err := applySlots(context.Background(), &out, slotsResize{
		want: 10, act: slotsActionWithdraw,
		withdrawable: 5 * testTicketMbr, excess: 15 * testTicketMbr, openTickets: 20,
	}, f.exec())
	if err != nil {
		t.Fatalf("applySlots: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, microAlgos(10*testTicketMbr)) {
		t.Errorf("output should name the %s left behind:\n%s", microAlgos(10*testTicketMbr), got)
	}
	if !strings.Contains(got, "20 ticket(s)") {
		t.Errorf("output should say what is holding it:\n%s", got)
	}

	// Nothing left behind → no warning at all.
	f = &fakeExec{}
	out.Reset()
	_ = applySlots(context.Background(), &out, slotsResize{
		want: 10, act: slotsActionWithdraw, withdrawable: 5 * testTicketMbr, excess: 5 * testTicketMbr,
	}, f.exec())
	if strings.Contains(out.String(), "stays in the pool") {
		t.Errorf("a full reclaim should carry no leftover warning:\n%s", out.String())
	}
}

// TestActionFor pins the deposit/withdraw/none decision — every branch under it
// moves real ALGO, and none of them is otherwise reachable without a chain.
func TestActionFor(t *testing.T) {
	tests := []struct {
		name                  string
		shortfall, withdrawal uint64
		dryRun                bool
		want                  slotsAction
	}{
		{name: "shortfall deposits", shortfall: 100, want: slotsActionDeposit},
		{name: "withdrawable reclaims", withdrawal: 100, want: slotsActionWithdraw},
		{name: "neither does nothing", want: slotsActionNone},
		// --dry-run's entire contract is "submit nothing", so it must collapse
		// every acting case to None.
		{name: "dry run never deposits", shortfall: 100, dryRun: true, want: slotsActionNone},
		{name: "dry run never withdraws", withdrawal: 100, dryRun: true, want: slotsActionNone},
		{name: "dry run on a no-op", dryRun: true, want: slotsActionNone},
		// Defensive: a shortfall outranks a withdrawal, so a pool that somehow
		// reported both grows rather than drains.
		{name: "shortfall outranks withdrawal", shortfall: 100, withdrawal: 100, want: slotsActionDeposit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionFor(tc.shortfall, tc.withdrawal, tc.dryRun); got != tc.want {
				t.Errorf("actionFor(%d, %d, dryRun=%v) = %d, want %d",
					tc.shortfall, tc.withdrawal, tc.dryRun, got, tc.want)
			}
		})
	}
}

// TestRestartWarningNeeded pins the asymmetry the warning's comment argues for:
// only a LOWERING can be undone by a running daemon's next top-up. Firing on a
// raise instead would be silent in exactly the dangerous case.
func TestRestartWarningNeeded(t *testing.T) {
	tests := []struct {
		name          string
		current, want uint64
		running       bool
		expect        bool
	}{
		{name: "lowering with a proxy running", current: 25, want: 10, running: true, expect: true},
		{name: "lowering with nothing running", current: 25, want: 10, expect: false},
		{name: "raising with a proxy running", current: 10, want: 25, running: true, expect: false},
		{name: "unchanged with a proxy running", current: 10, want: 10, running: true, expect: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := decideSlots(tc.current, tc.want, poolFor(10*testTicketMbr, 0), false)
			if got := restartWarningNeeded(d, tc.running); got != tc.expect {
				t.Errorf("restartWarningNeeded(current=%d, want=%d, running=%v) = %v, want %v",
					tc.current, tc.want, tc.running, got, tc.expect)
			}
		})
	}
}

// TestWriteSlotsConfig: the writer refuses unusual configs BY DESIGN, so that
// refusal is an expected outcome the user must be able to act on — not an
// exotic error. By the time it happens the pool is already resized, so the
// message has to carry the exact line to add and say the chain half is done.
func TestWriteSlotsConfig(t *testing.T) {
	dir := t.TempDir()

	// An ordinary config: writes, no advice needed.
	ok := filepath.Join(dir, "ok.yaml")
	if err := os.WriteFile(ok, []byte("zs:\n  concurrent_slots: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSlotsConfig(ok, 25); err != nil {
		t.Fatalf("writeSlotsConfig on a plain config: %v", err)
	}

	// A shape the writer refuses — here a multi-document file.
	odd := filepath.Join(dir, "odd.yaml")
	const src = "server:\n  listen: \"x\"\n---\nzs:\n  concurrent_slots: 10\n"
	if err := os.WriteFile(odd, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeSlotsConfig(odd, 25)
	if err == nil {
		t.Fatal("expected a refusal for a multi-document config")
	}
	if !errors.Is(err, config.ErrUnsupportedShape) {
		t.Errorf("error %v should still wrap ErrUnsupportedShape", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"pool is resized", // the chain half already happened
		odd,               // which file to edit
		"concurrent_slots: 25",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should mention %q, got:\n%s", want, msg)
		}
	}
	after, _ := os.ReadFile(odd)
	if string(after) != src {
		t.Errorf("the refusal rewrote the file anyway:\n%q", after)
	}
}

// TestSlotsWarnings covers the call site, not just the predicate. Inverting
// `kind != targetNone` here fired the restart warning when nothing was running
// and silenced it on every real lowering — one layer above a well-tested
// predicate, and invisible to it.
func TestSlotsWarnings(t *testing.T) {
	tests := []struct {
		name          string
		current, want uint64
		target        lifecycleTarget
		wantWarning   bool
	}{
		{
			name: "lowering with a daemon running", current: 25, want: 10,
			target: lifecycleTarget{kind: targetDaemon}, wantWarning: true,
		},
		{
			name: "lowering with an installed service", current: 25, want: 10,
			target: lifecycleTarget{kind: targetService}, wantWarning: true,
		},
		{
			name: "lowering with nothing running", current: 25, want: 10,
			target: lifecycleTarget{kind: targetNone},
		},
		{
			name: "raising with a daemon running", current: 10, want: 25,
			target: lifecycleTarget{kind: targetDaemon},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var errOut strings.Builder
			cmd.SetErr(&errOut)
			cmd.SetOut(io.Discard)

			d := decideSlots(tc.current, tc.want, poolFor(10*testTicketMbr, 0), false)
			slotsWarnings(cmd, d, tc.target)

			got := strings.Contains(errOut.String(), "restart")
			if got != tc.wantWarning {
				t.Errorf("restart warning present = %v, want %v\nstderr: %s", got, tc.wantWarning, errOut.String())
			}
		})
	}
}

// TestSlotsEnvVarIsTheOneConfigReads ties the constant the shadow warning
// watches to the variable applyEnv actually consumes. Two independent copies of
// the string drift silently: the warning stops firing, and the user sees
// "✔ wrote zs.concurrent_slots: 25" for a value the env keeps overriding.
func TestSlotsEnvVarIsTheOneConfigReads(t *testing.T) {
	t.Setenv(slotsEnvVar, "7")
	cfg, err := config.LoadBase("") // defaults + env, no file
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	if cfg.Zs.ConcurrentSlots != 7 {
		t.Errorf("setting %s did not reach the config (got %d) — the CLI is watching a variable nothing reads",
			slotsEnvVar, cfg.Zs.ConcurrentSlots)
	}
}

// TestSlotsCommandIsRegistered: without this, dropping the AddCommand line
// removes the whole feature from the CLI with a green suite.
func TestSlotsCommandIsRegistered(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"slots"})
	if err != nil || cmd == nil || cmd.Name() != "slots" {
		t.Fatalf("`slots` is not registered on the root command: cmd=%v err=%v", cmd, err)
	}
	for _, flag := range []string{"dry-run", "yes", "config"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("`slots` is missing the --%s flag", flag)
		}
	}
	// Exercise the validator rather than checking it is merely non-nil: with
	// cobra.ArbitraryArgs it is also non-nil, and `zs slots 5 9 12` would then
	// reach RunE and silently use the first.
	if err := cmd.Args(cmd, []string{"1"}); err != nil {
		t.Errorf("one argument should be accepted: %v", err)
	}
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("no arguments should be accepted (the show form): %v", err)
	}
	if err := cmd.Args(cmd, []string{"1", "2"}); err == nil {
		t.Error("two arguments should be rejected, not silently truncated to the first")
	}
}

// TestPoolExcessZeroWhenUnderTarget: growing is not a reclaim. A pool below
// target must report no excess at all, or a raise would try to withdraw.
func TestPoolExcessZeroWhenUnderTarget(t *testing.T) {
	ps := poolFor(4*testTicketMbr, 0)
	if got := poolExcess(10, ps.ticketMbr, 0, ps.dep); got != 0 {
		t.Errorf("excess = %d, want 0 for an under-target pool", got)
	}
	if got := poolShortfall(10, ps.ticketMbr, 0, ps.dep); got != 6*testTicketMbr {
		t.Errorf("shortfall = %d, want %d", got, 6*testTicketMbr)
	}
}

// TestPoolExcessUnknownTicketMbr: without the per-ticket MBR nothing can be
// sized, and returning a bogus figure would drive a real withdraw.
func TestPoolExcessUnknownTicketMbr(t *testing.T) {
	dep := escrow.PayerDeposit{AlgoBalance: 5_000_000}
	if got := poolExcess(10, 0, 0, dep); got != 0 {
		t.Errorf("excess = %d, want 0 when ticketMbr is unknown", got)
	}
}
