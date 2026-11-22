/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func TestAdopt_MovesTheTallyAndItsReservations(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	writeFile(t, OpenPath(legacy), `open`)
	got, err := Adopt(legacy, path)
	if err != nil || got != path {
		t.Fatalf("Adopt = %s, %v; want %s", got, err, path)
	}
	if readFile(path) != "ledger" || readFile(OpenPath(path)) != "open" {
		t.Errorf("moved ledger %q, open %q", readFile(path), readFile(OpenPath(path)))
	}
	if !gone(legacy) || !gone(OpenPath(legacy)) {
		t.Error("the earlier names are still there")
	}
}

// A crash after the open file moved and before the ledger did: the next start
// finishes the move.
func TestAdopt_FinishesAHalfMove(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	writeFile(t, OpenPath(path), `open`)
	if got, err := Adopt(legacy, path); err != nil || got != path {
		t.Fatalf("Adopt = %s, %v; want %s", got, err, path)
	}
	if readFile(path) != "ledger" || readFile(OpenPath(path)) != "open" || !gone(legacy) {
		t.Errorf("ledger %q, open %q, legacy gone %v", readFile(path), readFile(OpenPath(path)), gone(legacy))
	}
}

// holding is a Guard serving from path, as an older proxy would, or skips t
// where file locks are not supported.
func holding(t *testing.T, path string) *Guard {
	t.Helper()
	g := New(Config{}, path)
	if err := g.ReservationsNotKept(); err != nil {
		if strings.Contains(err.Error(), "not supported") {
			t.Skipf("no file locks here: %v", err)
		}
		t.Fatalf("the holding Guard did not take the lock: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

const openRows = `{"t1":{"amount_microusdc":5,"day":"2026-10-19"}}`

// A proxy still serving from the earlier name keeps writing there; moving the
// files from under it would drop every charge it records afterwards.
func TestAdopt_StaysOnTheEarlierNameWhileAProxyHoldsIt(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	writeFile(t, OpenPath(legacy), openRows)
	old := holding(t, legacy)
	got, err := Adopt(legacy, path)
	if err != nil || got != legacy {
		t.Fatalf("Adopt = %s, %v; want the earlier name %s", got, err, legacy)
	}
	if readFile(legacy) != "ledger" || readFile(OpenPath(legacy)) != openRows || !gone(path) || !gone(OpenPath(path)) {
		t.Error("Adopt moved a tally another proxy holds")
	}

	old.Close()
	if got, err := Adopt(legacy, path); err != nil || got != path || readFile(path) != "ledger" {
		t.Errorf("after the proxy stopped: Adopt = %s, %v, ledger %q", got, err, readFile(path))
	}
}

// A hold that ends within adoptWait is another start or a status check, not a
// proxy serving: Adopt waits it out and moves the tally.
func TestAdopt_WaitsOutABriefHold(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	brief := holding(t, legacy)
	// A fixed delay, not one derived from adoptWait: shrinking the wait to
	// nothing must fail this.
	go func() {
		time.Sleep(50 * time.Millisecond)
		brief.Close()
	}()
	if got, err := Adopt(legacy, path); err != nil || got != path || readFile(path) != "ledger" {
		t.Errorf("Adopt = %s, %v, ledger %q; want the tally moved to %s", got, err, readFile(path), path)
	}
}

// Open reservations and no settled charge yet: the reservations alone are
// still the day's, and dropping them would hand out their ceilings again.
func TestAdopt_MovesReservationsWithNoLedger(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, OpenPath(legacy), openRows)
	if got, err := Adopt(legacy, path); err != nil || got != path {
		t.Fatalf("Adopt = %s, %v; want %s", got, err, path)
	}
	if readFile(OpenPath(path)) != openRows || !gone(OpenPath(legacy)) {
		t.Errorf("open file %q, earlier gone %v", readFile(OpenPath(path)), gone(OpenPath(legacy)))
	}
}

// A ledger write in flight under the earlier name finishes before the move,
// or it would land on a name nothing reads.
func TestAdopt_WaitsForALedgerWrite(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	w, err := os.OpenFile(legacy+".wlock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := lockFile(w); err != nil {
		t.Skipf("no file locks here: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		got, _ := Adopt(legacy, path)
		done <- got
	}()
	select {
	case got := <-done:
		t.Fatalf("Adopt returned %s while a ledger write held the lock", got)
	case <-time.After(100 * time.Millisecond):
	}
	if !gone(path) {
		t.Fatal("the tally moved while a ledger write held the lock")
	}
	w.Close()
	if got := <-done; got != path || readFile(path) != "ledger" {
		t.Errorf("Adopt = %s, ledger %q; want the tally moved to %s", got, readFile(path), path)
	}
}

// The open file moves first, so a move that fails there has moved nothing and
// the tally stays whole under the earlier name.
func TestAdopt_AFailedFirstMoveMovesNothing(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	// A directory cannot be hard-linked.
	if err := os.Mkdir(OpenPath(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Adopt(legacy, path)
	if err == nil || got != legacy {
		t.Fatalf("Adopt = %s, %v; want the earlier name and the move's error", got, err)
	}
	if readFile(legacy) != "ledger" || !gone(path) {
		t.Errorf("ledger %q under the earlier name, new name gone %v", readFile(legacy), gone(path))
	}
}

// Reservations a Guard already saved under the new name stay; the ledger
// still moves, rather than the day counting from zero.
func TestAdopt_MovesTheLedgerBesideExistingReservations(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `ledger`)
	writeFile(t, OpenPath(legacy), `old open`)
	writeFile(t, OpenPath(path), openRows)
	if got, err := Adopt(legacy, path); err != nil || got != path {
		t.Fatalf("Adopt = %s, %v; want %s", got, err, path)
	}
	if readFile(path) != "ledger" || readFile(OpenPath(path)) != openRows {
		t.Errorf("ledger %q, open %q", readFile(path), readFile(OpenPath(path)))
	}
}

// A file under the new name is never replaced, nor merged with the earlier one.
func TestAdopt_KeepsAnExistingTally(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	writeFile(t, legacy, `old`)
	writeFile(t, path, `new`)
	if got, err := Adopt(legacy, path); err != nil || got != path || readFile(path) != "new" || readFile(legacy) != "old" {
		t.Errorf("Adopt = %s, %v; tally %q, earlier %q", got, err, readFile(path), readFile(legacy))
	}
}

func TestKept(t *testing.T) {
	dir := t.TempDir()
	legacy, path := filepath.Join(dir, "spend-8.json"), filepath.Join(dir, "spend-new.json")
	if got := Kept(legacy, path); got != path {
		t.Errorf("with neither file, Kept = %s, want %s", got, path)
	}
	writeFile(t, legacy, `old`)
	if got := Kept(legacy, path); got != legacy {
		t.Errorf("with only the earlier file, Kept = %s, want %s", got, legacy)
	}
	writeFile(t, path, `new`)
	if got := Kept(legacy, path); got != path {
		t.Errorf("with both, Kept = %s, want %s", got, path)
	}
	if gone(legacy) {
		t.Error("Kept moved a file")
	}
}
