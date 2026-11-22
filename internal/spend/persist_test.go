/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Settle writes the charge before it drops the reservation from the open file:
// stopped between the two (the spent ledger's write lock held elsewhere), the
// open file must still list the ticket, or a crash there forgets the charge.
func TestSettle_WritesTheChargeBeforeDroppingTheReservation(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	if err := g.Reserve("t1", 600); err != nil {
		t.Fatal(err)
	}
	wl, err := os.OpenFile(path+".wlock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileWait(wl); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { g.Settle("t1", 100); close(done) }()
	time.Sleep(200 * time.Millisecond)
	rows, err := readOpen(g.openPath())
	wl.Close()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows["t1"]; !ok {
		t.Error("the reservation left the open file before its charge was written")
	}
	if got := g.SpentToday(); got != 100 {
		t.Errorf("spent %d, want 100", got)
	}
}

// A closed guard no longer owns the open file: it says so, and a late resolve
// on it must not rewrite the file the next holder now owns.
func TestClose_ClosedGuardNoLongerOwnsTheReservations(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	if err := g.Reserve("g1", 100); err != nil {
		t.Fatal(err)
	}
	g.Close()
	if g.ReservationsNotKept() == nil {
		t.Error("a closed guard claims to keep its reservations")
	}
	h := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	h.now = fixedClock(&now)
	if err := h.ReservationsNotKept(); err != nil {
		t.Fatalf("the next guard does not hold the file: %v", err)
	}
	if err := h.Reserve("h1", 500); err != nil {
		t.Fatal(err)
	}
	g.Release("g1")
	h.Close()

	r := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	r.now = fixedClock(&now)
	// g1's 100 + h1's 500 are open: 400 fits, 401 does not.
	if err := r.Reserve("over", 401); err == nil {
		t.Error("the closed guard's late write erased the holder's open reservations")
	}
}

// New clears temp files a crash left, but not one young enough to be another
// guard's write in progress.
func TestNew_RemovesOnlyStaleTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spend.json")
	stale := path + ".tmp-stale"
	staleOpen := filepath.Join(dir, "spend.open.json.tmp-stale")
	fresh := filepath.Join(dir, "spend.open.json.tmp-fresh")
	old := time.Now().Add(-2 * time.Minute)
	for _, p := range []string{stale, staleOpen, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if p != fresh {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	newGuard(t, Config{}, path)
	for _, p := range []string{stale, staleOpen} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("stale temp %s survived", filepath.Base(p))
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a temp under a minute old was removed: %v", err)
	}
}

// The first charge of a new day starts the new day's ledger: yesterday's spend
// neither carries into it nor swallows it.
func TestSettle_FirstChargeOfANewDay(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "spend.json")
	g := newGuard(t, Config{DailyCapMicroUSDC: 1000}, path)
	g.now = fixedClock(&now)
	if err := g.Reserve("a", 500); err != nil {
		t.Fatal(err)
	}
	g.Settle("a", 400)
	now = now.AddDate(0, 0, 1)
	if err := g.Reserve("b", 300); err != nil {
		t.Fatal(err)
	}
	g.Settle("b", 30)
	if got := g.SpentToday(); got != 30 {
		t.Errorf("spent %d on the new day, want 30", got)
	}
}

// A failed save is logged, once per run of failures; a success re-arms it.
func TestSaved_LogsTheFirstOfARunOfFailures(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	g := newGuard(t, Config{}, filepath.Join(t.TempDir(), "spend.json"))
	g.now = fixedClock(&now)
	block := func() {
		t.Helper()
		// A directory where the open file goes fails its rename.
		if err := os.Mkdir(g.openPath(), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	warnings := func() int { return strings.Count(buf.String(), "could not save") }
	block()
	_ = g.Reserve("a", 1)
	_ = g.Reserve("b", 1)
	if got := warnings(); got != 1 {
		t.Errorf("%d warnings for two failed saves in a row, want 1", got)
	}
	if err := os.Remove(g.openPath()); err != nil {
		t.Fatal(err)
	}
	_ = g.Reserve("c", 1)
	if err := os.Remove(g.openPath()); err != nil {
		t.Fatal(err)
	}
	block()
	_ = g.Reserve("d", 1)
	if got := warnings(); got != 2 {
		t.Errorf("%d warnings after a success then a failure, want 2", got)
	}
}

// New creates the state directory, so a first run still keeps its reservations.
func TestNew_CreatesTheStateDir(t *testing.T) {
	g := newGuard(t, Config{}, filepath.Join(t.TempDir(), "state", "spend.json"))
	if err := g.ReservationsNotKept(); err != nil {
		t.Errorf("a guard whose state dir did not exist yet keeps nothing: %v", err)
	}
}
