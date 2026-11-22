/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/zs"
)

func fixedClock(ts *time.Time) func() time.Time {
	return func() time.Time { return *ts }
}

// newGuard is New, closed when the test ends: an open lock file keeps the
// temp dir from being removed on Windows.
func newGuard(t *testing.T, cfg Config, path string) *Guard {
	t.Helper()
	g := New(cfg, path)
	t.Cleanup(func() { g.Close() })
	return g
}

// Admit applies both caps to the estimate, counting today's spend and every
// open reservation, and records nothing.
func TestAdmit_ChecksBothCapsAndRecordsNothing(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000, PerRequestCapMicroUSDC: 500}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Admit(501); err == nil {
		t.Error("an estimate over the per-request cap was admitted")
	}
	if err := g.Admit(500); err != nil {
		t.Errorf("an estimate at the per-request cap was refused: %v", err)
	}
	if err := g.Reserve("t1", 400); err != nil {
		t.Fatal(err)
	}
	g.Settle("t1", 300)
	if err := g.Reserve("t2", 400); err != nil {
		t.Fatal(err)
	}
	// 300 spent + 400 open + 300 = 1000.
	if err := g.Admit(300); err != nil {
		t.Errorf("an estimate that reaches the daily cap exactly was refused: %v", err)
	}
	if err := g.Admit(301); err == nil {
		t.Error("an estimate past the daily cap was admitted")
	}
	if g.outstanding != 400 || len(g.open) != 1 {
		t.Errorf("outstanding %d, open %v: Admit recorded exposure", g.outstanding, g.open)
	}

}

// Yesterday's unsettled reservations count against today's cap until they
// resolve; a second roll drops them.
func TestAdmit_CarriesYesterdaysReservationsOneDay(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 900); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if err := g.Admit(500); err == nil {
		t.Error("next day: 900 carried + 500 was admitted")
	}
	now = now.Add(24 * time.Hour)
	if err := g.Admit(500); err != nil {
		t.Errorf("two days on, the reservation should be dropped: %v", err)
	}
	if g.outstanding != 0 || len(g.open) != 0 {
		t.Errorf("outstanding %d, open %v after the second roll", g.outstanding, g.open)
	}
}

// After an idle stretch the first call's roll drops a reservation older than
// yesterday, even though no roll ran in between.
func TestRoll_AfterAnIdleStretchDropsOldReservations(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 900); err != nil {
		t.Fatal(err)
	}
	now = now.Add(72 * time.Hour)
	if err := g.Admit(500); err != nil {
		t.Errorf("three days on, a 900 reservation still counted: %v", err)
	}
	if g.outstanding != 0 || len(g.open) != 0 {
		t.Errorf("outstanding %d, open %v after an idle stretch", g.outstanding, g.open)
	}
	g.Settle("t1", 700)
	if got := g.SpentToday(); got != 0 {
		t.Errorf("spent = %d: a dropped reservation's Settle landed on today's tally", got)
	}
}

// Days are UTC days whatever the local zone: a reservation from two UTC days
// ago is dropped even where the local date says it is only yesterday's.
func TestRoll_UsesUTCDays(t *testing.T) {
	east := time.FixedZone("+10", 10*3600)
	now := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC).In(east)
	g := newGuard(t, Config{DailyCapMicroUSDC: 100}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("a", 100); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 6, 17, 0, 30, 0, 0, time.UTC).In(east)
	if err := g.Admit(100); err != nil {
		t.Errorf("two UTC days on, the reservation still counted: %v", err)
	}
}

// Settle rolls the day first, so a reservation already older than yesterday
// is dropped and its late Settle records nothing. Only a turn left idle for
// two UTC days can get here.
func TestSettle_AfterTheReservationWasDropped(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("a", 50); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 6, 17, 0, 5, 0, 0, time.UTC)
	g.Settle("a", 30)
	if got := g.SpentToday(); got != 0 {
		t.Errorf("spent = %d, want 0 for a reservation older than yesterday", got)
	}
}

// A carried reservation counts against the new day's cap and keeps its id,
// so the operator can't reuse it after midnight.
func TestReserve_CarriedReservationCountsAndKeepsItsID(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 59, 50, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 10_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 5_000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	if err := g.Reserve("t2", 6_000); err == nil {
		t.Error("5000 carried + 6000 > 10000 was reserved")
	}
	if err := g.Reserve("t1", 1); !errors.Is(err, zs.ErrDuplicateTicket) {
		t.Errorf("reusing a carried id: err = %v, want ErrDuplicateTicket", err)
	}
	if err := g.Reserve("t2", 5_000); err != nil {
		t.Errorf("5000 carried + 5000 == 10000 should be allowed: %v", err)
	}
	if g.outstanding != 10_000 {
		t.Errorf("outstanding = %d, want 10000", g.outstanding)
	}
}

// Only the previous day's reservations carry: on the next roll, the
// two-day-old ones go and the one-day-old ones stay.
func TestRoll_KeepsOnlyThePreviousDay(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 10_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("old", 3_000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if err := g.Reserve("young", 2_000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if err := g.Admit(0); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.open["old"]; ok {
		t.Error("a two-day-old reservation survived the roll")
	}
	if g.outstanding != 2_000 || len(g.open) != 1 {
		t.Errorf("outstanding %d, open %v: want only young's 2000", g.outstanding, g.open)
	}
	g.Settle("young", 1_500)
	if got := g.SpentToday(); got != 1_500 {
		t.Errorf("spent = %d, want 1500 for the carried ticket", got)
	}
}

// Spend already over the cap (the cap was lowered and the proxy restarted)
// refuses everything for the rest of the day: budget-spent must not wrap.
func TestCheckDaily_SpentOverTheCapRefusesEverything(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 5000}, path)
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 1200); err != nil {
		t.Fatal(err)
	}
	g.Settle("t1", 1200)

	lowered := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	lowered.now = fixedClock(&now)
	if err := lowered.Admit(1); err == nil {
		t.Error("Admit with today's spend over the cap was admitted")
	}
	if err := lowered.Reserve("t2", 1); err == nil {
		t.Error("Reserve with today's spend over the cap was admitted")
	}
}

// A restart keeps the open reservations: their escrows are still open, so
// forgetting them would fail the cap open by their ceilings. A released one
// stays released, and one older than yesterday is dropped as the roll drops it.
func TestReserve_OpenReservationsSurviveARestart(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	for id, amt := range map[string]uint64{"kept": 600, "released": 300} {
		if err := g.Reserve(id, amt); err != nil {
			t.Fatal(err)
		}
	}
	g.Release("released")
	g.Close()

	restarted := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	restarted.now = fixedClock(&now)
	if err := restarted.Reserve("over", 401); err == nil {
		t.Error("a restart forgot the open 600")
	}
	if err := restarted.Reserve("fits", 400); err != nil {
		t.Errorf("the released 300 came back after the restart: %v", err)
	}
	// The restored reservation still resolves exactly once.
	restarted.Settle("kept", 50)
	restarted.Settle("kept", 50)
	if got := restarted.SpentToday(); got != 50 {
		t.Errorf("spent %d after settling the restored ticket, want 50", got)
	}

	restarted.Close()
	later := now.AddDate(0, 0, 2)
	old := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	old.now = fixedClock(&later)
	if err := old.Reserve("next", 1000); err != nil {
		t.Errorf("a reservation two days old still held headroom: %v", err)
	}
}

// A second guard on the file shares its spent ledger but not its reservations:
// it neither restores the holder's nor overwrites them, so the holder's
// restart still sees exactly its own open tickets.
func TestReserve_SecondGuardLeavesTheHoldersReservations(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	a := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	a.now = fixedClock(&now)
	if err := a.ReservationsNotKept(); err != nil {
		t.Fatalf("the first guard on a file does not hold it: %v", err)
	}
	if err := a.Reserve("a1", 600); err != nil {
		t.Fatal(err)
	}
	b := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	b.now = fixedClock(&now)
	if b.ReservationsNotKept() == nil {
		t.Fatal("a second guard on a held file claims it too")
	}
	// b did not take a's 600 in: its own 900 fits.
	if err := b.Reserve("b1", 300); err != nil {
		t.Fatal(err)
	}
	b.Settle("b1", 100)
	a.Settle("a1", 10)
	if err := a.Reserve("a2", 500); err != nil {
		t.Fatal(err)
	}
	// b's save after a2 must leave a2 in place.
	if err := b.Reserve("b2", 50); err != nil {
		t.Fatal(err)
	}
	b.Settle("b2", 0)
	a.Close()

	restarted := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	restarted.now = fixedClock(&now)
	// 110 spent + a2's 500: a1 settled and b's tickets were never a's.
	if err := restarted.Reserve("fits", 390); err != nil {
		t.Errorf("the restart counted reservations that were not open: %v", err)
	}
	if err := restarted.Reserve("over", 1); err == nil {
		t.Error("the restart forgot a2's open 500")
	}
}

// Guards on one file all write its spent ledger: no charge one records may be
// lost to another's write.
func TestSettle_GuardsOnOneFileLoseNoCharge(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	guards := []*Guard{newGuard(t, Config{}, path), newGuard(t, Config{}, path), newGuard(t, Config{}, path)}
	const each = 40
	var wg sync.WaitGroup
	for gi, g := range guards {
		g.now = fixedClock(&now)
		for i := range each {
			id := strconv.Itoa(gi) + "-" + strconv.Itoa(i)
			if err := g.Reserve(id, 1); err != nil {
				t.Fatal(err)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				g.Settle(strconv.Itoa(gi)+"-"+strconv.Itoa(i), 1)
			}
		}()
	}
	wg.Wait()
	if got, want := guards[0].SpentToday(), uint64(len(guards)*each); got != want {
		t.Errorf("spent %d after %d charges of 1: a write replaced another's", got, want)
	}
}

// Reservations live apart from the spent ledger, so reserving and releasing
// never write the ledger: a write there would replace a charge another guard
// recorded between this guard's read and its write.
func TestReserve_LeavesTheSpentLedgerAlone(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	if err := g.Reserve("a", 10); err != nil {
		t.Fatal(err)
	}
	g.Release("a")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reserving and releasing wrote the spent ledger (stat: %v)", err)
	}
	if _, err := os.Stat(g.openPath()); err != nil {
		t.Errorf("the open reservations were not written: %v", err)
	}
}

// An open-reservations file the guard cannot parse is left as it is: the
// guard keeps its reservations in memory and says why, rather than write over
// what the file still records.
func TestNew_UnreadableOpenFileIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	openPath := filepath.Join(filepath.Dir(path), "spend.open.json")
	if err := os.WriteFile(openPath, []byte(`{"a":`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	if g.ReservationsNotKept() == nil {
		t.Error("a guard over an unreadable file claims to keep its reservations")
	}
	if err := g.Reserve("x", 10); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(openPath); string(b) != `{"a":` {
		t.Errorf("the unreadable file became %q", b)
	}
}

// Two guards share one tally file, so one's settle can raise the spent the
// other reads past what its own reservations leave room for. budget-spent-
// outstanding must not wrap into "anything fits".
func TestCheckDaily_OutstandingOverWhatAnotherGuardLeft(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	a := newGuard(t, Config{DailyCapMicroUSDC: 100}, path)
	b := newGuard(t, Config{DailyCapMicroUSDC: 100}, path)
	a.now, b.now = fixedClock(&now), fixedClock(&now)
	if err := a.Reserve("a", 60); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve("b", 50); err != nil {
		t.Fatal(err)
	}
	b.Settle("b", 50) // 50 spent; a still holds 60 > 100-50
	if err := a.Reserve("c", 1); err == nil {
		t.Error("Reserve admitted with outstanding over what the cap leaves")
	}
	if err := a.Admit(1); err == nil {
		t.Error("Admit admitted with outstanding over what the cap leaves")
	}
}

// A request costing nothing fits a cap that is exactly used up.
func TestCheckDaily_ZeroFitsAFullCap(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 100}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("a", 100); err != nil {
		t.Fatal(err)
	}
	if err := g.Reserve("free1", 0); err != nil {
		t.Errorf("reserved to the cap: a zero ceiling was refused: %v", err)
	}
	g.Settle("a", 100)
	if err := g.Reserve("free2", 0); err != nil {
		t.Errorf("spent to the cap: a zero ceiling was refused: %v", err)
	}
	if err := g.Admit(0); err != nil {
		t.Errorf("spent to the cap: a zero estimate was refused: %v", err)
	}
}

// A sum past uint64 must not wrap below the cap.
func TestCheckDaily_DoesNotWrap(t *testing.T) {
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, "")
	if err := g.Reserve("t1", 600); err != nil {
		t.Fatal(err)
	}
	if err := g.Admit(math.MaxUint64 - 100); err == nil {
		t.Error("an estimate that wraps the daily sum was admitted")
	}
	if err := g.Reserve("t2", math.MaxUint64-100); err == nil {
		t.Error("a ceiling that wraps the daily sum was reserved")
	}
}

func TestReserve_PerRequestCap(t *testing.T) {
	g := newGuard(t, Config{PerRequestCapMicroUSDC: 1000}, "")
	if err := g.Reserve("t1", 1000); err != nil {
		t.Errorf("at-cap should be allowed: %v", err)
	}
	if err := g.Reserve("t2", 1001); err == nil {
		t.Error("over per-request cap should be rejected")
	}
}

// TestReserve_DailyCapCountsOutstanding: the daily cap gates on
// spent + outstanding + this ceiling, so a burst of reservations is bounded by
// the cap even before any of them settle.
func TestReserve_DailyCapCountsOutstanding(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 600); err != nil {
		t.Fatal(err)
	}
	// 600 outstanding + 500 > 1000 → rejected (and not added).
	if err := g.Reserve("t2", 500); err == nil {
		t.Error("expected daily cap rejection")
	}
	// 600 + 400 == 1000 → allowed.
	if err := g.Reserve("t3", 400); err != nil {
		t.Errorf("at exact daily cap should be allowed: %v", err)
	}
	if g.outstanding != 1000 {
		t.Errorf("outstanding = %d, want 1000", g.outstanding)
	}
	if got := g.SpentToday(); got != 0 {
		t.Errorf("nothing settled yet, spent = %d, want 0", got)
	}
}

// TestSettle_RecordsActualAndFreesCeiling is the crux of the fix: a high ticket
// ceiling that settles at a tiny actual charge must record only the actual
// charge and free the ceiling back to the daily budget — the old monotonic
// "reserved" tally did neither.
func TestSettle_RecordsActualAndFreesCeiling(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1_500_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 1_500_000); err != nil { // reserve the $1.50 ceiling
		t.Fatal(err)
	}
	g.Settle("t1", 1_000) // ...but only $0.001 was actually charged
	if g.outstanding != 0 {
		t.Errorf("outstanding after settle = %d, want 0 (ceiling freed)", g.outstanding)
	}
	if got := g.SpentToday(); got != 1_000 {
		t.Errorf("spent = %d, want 1000 (the actual charge, not the $1.50 ceiling)", got)
	}
	// Nearly the whole cap is available again — the refunded ceiling did not
	// permanently consume the daily budget.
	if err := g.Reserve("t2", 1_499_000); err != nil {
		t.Errorf("freed headroom should be reusable: %v", err)
	}
}

func TestRelease_BacksOutReservationNoSpend(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 800); err != nil {
		t.Fatal(err)
	}
	g.Release("t1") // e.g. the open failed, or the receipt didn't verify
	if g.outstanding != 0 {
		t.Errorf("outstanding after release = %d, want 0", g.outstanding)
	}
	if got := g.SpentToday(); got != 0 {
		t.Errorf("release records no spend, got %d", got)
	}
	// The released headroom is reusable.
	if err := g.Reserve("t2", 1000); err != nil {
		t.Errorf("full cap should be available after release: %v", err)
	}
	// Releasing an unknown ticket, or one already released, is a no-op rather
	// than an over-release — the old amount-passing API floored a mismatched
	// release at zero instead, which silently hid the caller bug.
	g.Release("t1")
	g.Release("never-reserved")
	if g.outstanding != 1000 {
		t.Errorf("outstanding = %d, want 1000 — a no-op release must not touch t2", g.outstanding)
	}
}

// TestSettleThenRelease_Idempotent pins the exactly-once property in the
// direction that is actually reachable: a Settle followed by a stray Release must
// not double-subtract or alter the ledger.
//
// The reverse order is deliberately NOT asserted here. A Release followed by a
// late Settle means money moved after we decided it wouldn't, and treating that
// as a no-op is what let an operator emitting two zs-receipt frames hide every
// charge from the payer's daily cap. That ordering is now prevented at its source
// by the one-receipt-per-stream latch in the dispatch frame factory.
func TestSettleThenRelease_Idempotent(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 10_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 5_000); err != nil {
		t.Fatal(err)
	}
	g.Settle("t1", 100)
	g.Release("t1") // a late release after the receipt already settled
	if g.outstanding != 0 {
		t.Errorf("outstanding = %d, want 0", g.outstanding)
	}
	if got := g.SpentToday(); got != 100 {
		t.Errorf("spent = %d, want 100 — the backstop release must not alter the ledger", got)
	}
}

// TestSettle_AcrossUTCDayRoll: a ticket reserved before midnight UTC and
// settled after it records its charge on the new day. Losing it would drop
// every charge in flight across midnight from the ledger at once.
func TestSettle_AcrossUTCDayRoll(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 59, 50, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1_000_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 5_000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second) // crosses midnight into 2026-06-16
	g.Settle("t1", 4_321)

	if got := g.SpentToday(); got != 4_321 {
		t.Errorf("spent = %d, want 4321 — a charge settled after the day roll must still be recorded", got)
	}
	if g.outstanding != 0 {
		t.Errorf("outstanding = %d, want 0 after the roll", g.outstanding)
	}
	// Still idempotent across the roll: a second Settle adds nothing.
	g.Settle("t1", 4_321)
	if got := g.SpentToday(); got != 4_321 {
		t.Errorf("spent = %d after a duplicate Settle, want 4321", got)
	}
}

// The roll can come from any call, not just the Settle: a Reserve, Release or
// Admit after midnight must not make yesterday's in-flight charge look
// already resolved.
func TestSettle_AfterAnotherCallRolledTheDay(t *testing.T) {
	for name, roll := range map[string]func(g *Guard){
		"reserve": func(g *Guard) { _ = g.Reserve("t2", 1) },
		"release": func(g *Guard) { g.Release("other") },
		"admit":   func(g *Guard) { _ = g.Admit(1) },
	} {
		now := time.Date(2026, 6, 15, 23, 59, 50, 0, time.UTC)
		g := newGuard(t, Config{DailyCapMicroUSDC: 1_000_000}, filepath.Join(t.TempDir(), "spend.json"))
		g.now = fixedClock(&now)
		if err := g.Reserve("t1", 5_000); err != nil {
			t.Fatal(err)
		}
		now = now.Add(20 * time.Second)
		roll(g)
		g.Settle("t1", 4_321)
		if got := g.SpentToday(); got != 4_321 {
			t.Errorf("%s rolled the day: spent = %d, want 4321", name, got)
		}
		g.Settle("t1", 4_321)
		g.Release("t1")
		if got := g.SpentToday(); got != 4_321 {
			t.Errorf("%s rolled the day: spent = %d after a repeat, want 4321", name, got)
		}
	}
}

// A reservation released after the roll records nothing when it later settles.
func TestRelease_AfterTheRollStillResolves(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 59, 50, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1_000_000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 5_000); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	g.Release("t1")
	g.Settle("t1", 4_321)
	if got := g.SpentToday(); got != 0 {
		t.Errorf("spent = %d, want 0 for a released ticket", got)
	}
}

// A ticket id is minted by the operator's node, so a reused one must be REFUSED
// rather than silently accepted. Accepting it skipped the daily-cap check
// entirely (the dedup branch runs first) and let the second request's
// Settle/Release resolve the first request's reservation.
func TestReserve_DuplicateTicketIDRefused(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	if err := g.Reserve("dup", 900); err != nil {
		t.Fatal(err)
	}
	// 900 + 900 > 1000: must be refused on the duplicate id, and would also have
	// breached the cap had the id been fresh. Either way, not admitted.
	err := g.Reserve("dup", 900)
	if err == nil {
		t.Fatal("a reused ticket id was admitted; the cap was never evaluated for it")
	}
	// The sentinel is what lets the caller report this as operator misbehaviour
	// rather than as a cap breach. Both conditions were live here (the duplicate
	// AND a 900+900>1000 overflow), so a plain non-nil check can't tell which
	// refusal fired — and pointing the payer at spend.daily_cap_usdc for a reused
	// id would send them to a knob that cannot help. Matched against the SDK's
	// sentinel because that is the one the dispatch path tests for.
	if !errors.Is(err, zs.ErrDuplicateTicket) {
		t.Errorf("err = %v; want it to wrap zs.ErrDuplicateTicket", err)
	}
	if g.outstanding != 900 {
		t.Errorf("outstanding = %d, want 900 — the refused duplicate must not add exposure", g.outstanding)
	}
	// The first reservation is intact and still resolvable.
	g.Settle("dup", 750)
	if got := g.SpentToday(); got != 750 {
		t.Errorf("spent = %d, want 750 — the original charge must survive", got)
	}
}

// TestSpent_PersistsAndDayRollsOver: the settled-spend ledger survives a restart
// (so the daily cap holds across the day) and resets at the UTC day boundary,
// while a reservation still open then carries into the new day.
func TestSpent_PersistsAndDayRollsOver(t *testing.T) {
	now := time.Date(2026, 6, 15, 23, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)

	if err := g.Reserve("t1", 900); err != nil {
		t.Fatal(err)
	}
	g.Settle("t1", 700) // ceiling 900, actual charge 700
	if got := g.SpentToday(); got != 700 {
		t.Errorf("spent today = %d, want 700", got)
	}

	// A restart sees the persisted spend; t1 settled, so nothing is open.
	g.Close()
	g2 := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g2.now = fixedClock(&now)
	if got := g2.SpentToday(); got != 700 {
		t.Errorf("persisted spent = %d, want 700", got)
	}
	// 700 spent + 0 outstanding + 400 > 1000 → rejected after restart.
	if err := g2.Reserve("t2", 400); err == nil {
		t.Error("700 spent + 400 > 1000 should be rejected after restart")
	}
	// 700 + 300 == 1000 → allowed.
	if err := g2.Reserve("t3", 300); err != nil {
		t.Errorf("700 + 300 == 1000 should be allowed: %v", err)
	}

	// Next UTC day → spent resets, but t3's escrow may still be open, so its
	// 300 counts against the new day's cap.
	now = now.Add(2 * time.Hour) // crosses midnight into 2026-06-16
	if got := g2.SpentToday(); got != 0 {
		t.Errorf("spent after day rollover = %d, want 0", got)
	}
	if err := g2.Reserve("t4", 701); err == nil {
		t.Error("300 carried + 701 > 1000 should be rejected on the new day")
	}
	if err := g2.Reserve("t4", 700); err != nil {
		t.Errorf("300 carried + 700 == 1000 should be allowed: %v", err)
	}
}

func TestReserve_UnlimitedWhenZero(t *testing.T) {
	g := newGuard(t, Config{}, "") // both caps 0 = unlimited
	if err := g.Reserve("t1", 1<<60); err != nil {
		t.Errorf("unlimited guard rejected a charge: %v", err)
	}
}

// TestReserve_AtomicUnderConcurrency is the regression for the check-then-act
// race: with a cap of exactly N units of `amount` and no settles, exactly N
// concurrent Reserves must succeed and the rest must be rejected — never an
// overshoot of the outstanding total.
func TestReserve_AtomicUnderConcurrency(t *testing.T) {
	const (
		amount  = 100
		slots   = 10
		callers = 200
	)
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{DailyCapMicroUSDC: amount * slots}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		oks int
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		// Distinct ticket id per caller: reservations are keyed by id, so reusing
		// one would make every caller after the first a dedup no-op (returning
		// nil) and the overshoot this test guards against would go undetected.
		id := "t" + strconv.Itoa(i)
		go func() {
			defer wg.Done()
			<-start // line everyone up so they actually contend
			if err := g.Reserve(id, amount); err == nil {
				mu.Lock()
				oks++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if oks != slots {
		t.Errorf("successful reserves = %d, want exactly %d (no overshoot)", oks, slots)
	}
	if g.outstanding != amount*slots {
		t.Errorf("outstanding total = %d, want %d (cap, never exceeded)", g.outstanding, amount*slots)
	}
}

// Admit runs on every request, beside other requests' Reserve and Settle, and
// a day roll inside it rewrites the open set. Under -race this fails if Admit
// reads or rolls the guard without its lock.
func TestAdmit_ConcurrentWithReserveAcrossMidnight(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 6, 15, 23, 59, 59, 0, time.UTC).UnixNano())
	g := newGuard(t, Config{DailyCapMicroUSDC: 1 << 40}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(2)
		id := "t" + strconv.Itoa(i)
		go func() {
			defer wg.Done()
			<-start
			if err := g.Reserve(id, 1000); err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if i%2 == 0 {
				g.Settle(id, 10)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if i == n/2 {
				clock.Store(time.Date(2026, 6, 16, 0, 0, 1, 0, time.UTC).UnixNano())
			}
			_ = g.Admit(1)
		}()
	}
	close(start)
	wg.Wait()

	g.mu.Lock()
	defer g.mu.Unlock()
	var open uint64
	for _, r := range g.open {
		open += r.amount
	}
	if g.outstanding != open {
		t.Errorf("outstanding = %d, open reservations sum to %d", g.outstanding, open)
	}
}

// TestSettleReleaseConcurrency exercises the Settle/Release paths under the
// race detector: a fleet of reserve→(settle|release) cycles must leave the
// guard consistent (outstanding drained to zero, spent = sum of settled).
func TestSettleReleaseConcurrency(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{}, filepath.Join(t.TempDir(), "spend.json")) // no caps → every reserve admits
	g.now = fixedClock(&now)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		settle := i%2 == 0
		id := "t" + strconv.Itoa(i)
		go func() {
			defer wg.Done()
			if err := g.Reserve(id, 1000); err != nil {
				t.Errorf("uncapped reserve rejected: %v", err)
				return
			}
			if settle {
				g.Settle(id, 10)
			} else {
				g.Release(id)
			}
		}()
	}
	wg.Wait()

	if g.outstanding != 0 {
		t.Errorf("outstanding after all reserve/settle/release = %d, want 0", g.outstanding)
	}
	if got, want := g.SpentToday(), uint64(n/2*10); got != want {
		t.Errorf("spent = %d, want %d (half settled at 10 each)", got, want)
	}
}
