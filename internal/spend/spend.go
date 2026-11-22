/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package spend is the per-process spend guard: a configurable per-request and
// per-day USDC ceiling enforced before the proxy opens an escrow. It bounds
// the *aggregate* drain of the auto-spending wallet (a runaway agent loop),
// which the protocol's per-request max_price does not.
//
// Two quantities back the daily cap. Spent is the persisted ledger of what
// requests actually cost — the sum of the operator's signed receipt
// AmountCharged (<= the ticket's MaxPrice) for the current UTC day, recorded
// via Settle. It survives a restart so the cap holds across the day, and is the
// real spend `zs status` reports. Outstanding is the sum of MaxPrice for
// tickets currently open (reserved but not yet settled), persisted beside it:
// their escrows outlive a restart, and so does their exposure. Reserve admits a
// request only if spent + outstanding + this ticket's MaxPrice fits the daily
// cap, so the worst-case ceiling stays bounded while refunded reservations
// free their headroom the moment they settle: Settle drops the ceiling from outstanding
// and adds the far smaller actual charge to spent. This replaces the old
// monotonic "reserved" tally, which counted ceilings that were almost entirely
// refunded and so tripped the cap long before real spend warranted it. Reserve
// folds the check and the add into one locked operation, so concurrent requests
// near the boundary can't all pass a check that only one should.
package spend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TxnLab/zerosignal/go/zs"
)

// ErrDuplicateTicket is returned (wrapped) by Reserve when ticketID already has
// an open reservation. It is a sentinel rather than a bare error string because
// the two Reserve failures are diagnostically opposite and the caller has to
// tell them apart: a cap breach is the payer's own configured limit (actionable
// — raise the cap or wait for the day to roll), while a duplicate is the
// operator's node handing out a ticket id it already used, which no local config
// change can fix. Reporting the latter as a cap breach points the payer at a
// knob that cannot help.
//
// It is the SDK's sentinel, not a copy: the dispatch path matches it with
// errors.Is against zs.ErrDuplicateTicket, so a separate errors.New here would
// turn every duplicate into a cap breach.
var ErrDuplicateTicket = zs.ErrDuplicateTicket

// Config holds the caps in microUSDC. Zero means "unlimited" for that axis.
type Config struct {
	DailyCapMicroUSDC      uint64
	PerRequestCapMicroUSDC uint64
}

// Guard enforces Config against a persisted daily spent ledger plus its
// currently-outstanding reservations.
//
// Reservations are keyed by ticket id so the accounting is EXACTLY-ONCE by
// construction rather than by convention. Settle and Release both resolve the
// amount from the recorded reservation and drop it, so whichever runs first wins
// and every later call for the same ticket is a no-op. Two things follow: the
// amount can no longer be mismatched against what was reserved (the old
// amount-passing API relied on the caller passing the same MaxPrice back, and
// floored a mismatch at zero to stay safe), and a component that learns a
// ticket's outcome out of band can resolve it without having to know whether the
// in-band path already did.
//
// A reservation with no Settle or Release stays outstanding through the rest of
// its UTC day and the next calendar day. That is deliberate, not a leak: while the escrow
// is open the operator can still claim up to MaxPrice via settleLapsed, so the
// exposure is real, and a ticket open at midnight settles onto the new day's
// tally. The SDK's refund watchdog and ticket reconciler resolve one once its
// outcome is fixed on chain, a reservation restored after a restart included;
// what neither sees end is held at the full ceiling rather than the eventual
// charge, and for up to a day longer than the on-chain outcome takes.
type Guard struct {
	cfg  Config
	path string
	now  func() time.Time
	// lock is held while this Guard owns the open reservations; nil when
	// another Guard does, or nothing is persisted, and then notKept says why.
	lock    *os.File
	notKept error
	// saveFailing dedupes the save-failure warning until a save succeeds.
	saveFailing bool

	mu sync.Mutex
	// open maps ticket id → the reservation for it. outstanding is kept
	// alongside as the running sum so Reserve stays O(1) rather than re-summing
	// the map on every admission.
	open        map[string]reservation
	outstanding uint64 // sum of open, recomputed by rollOutstanding
	day         string // UTC day of the last roll; see rollOutstanding
}

// reservation is a ticket's reserved MaxPrice and the UTC day it was made.
type reservation struct {
	amount uint64
	day    string
}

type tally struct {
	Date  string `json:"date"`            // YYYY-MM-DD (UTC)
	Spent uint64 `json:"spent_microusdc"` // actual settled charges recorded via Settle
}

// openRow is one unresolved reservation in the open file, whatever its day: a
// restart must not forget the escrows still open, or the cap fails open by
// their ceilings for the rest of the day.
type openRow struct {
	Amount uint64 `json:"amount_microusdc"`
	Day    string `json:"day"`
}

// New builds a Guard persisting its tally to path. path may be empty, in which
// case the per-request cap still applies but the *daily* tally is not tracked
// (the file is the day's running total; with no file every Reserve starts from
// zero). Production always supplies a state-dir path.
//
// The spent ledger at path is shared by every Guard on it. The open
// reservations live in a file of their own, which only the Guard holding
// path's lock reads or writes: a second Guard writing its own set there would
// erase the holder's, and reading the holder's would revive settled ones. The
// holder restores what the file holds. No request resolves those; the SDK's
// reconciler frees one when it sees the ticket end, and rollOutstanding drops
// the rest.
func New(cfg Config, path string) *Guard {
	g := &Guard{cfg: cfg, path: path, now: time.Now, open: make(map[string]reservation)}
	g.notKept = g.hold()
	return g
}

// hold takes path's lock and restores the open reservations, or says why not.
func (g *Guard) hold() error {
	if g.path == "" {
		return errors.New("no state directory")
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(g.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		if errors.Is(err, errors.ErrUnsupported) {
			return errors.New("file locks are not supported here")
		}
		return fmt.Errorf("another zs-proxy holds %s (%w)", g.path, err)
	}
	rows, err := readOpen(g.openPath())
	if err != nil {
		// Left in place for a person to look at: overwriting it would drop
		// whatever it still records.
		lock.Close()
		return err
	}
	g.lock = lock
	for id, r := range rows {
		g.open[id] = reservation{amount: r.Amount, day: r.Day}
	}
	removeStaleTemps(g.path)
	removeStaleTemps(g.openPath())
	return nil
}

// ReservationsNotKept says why this Guard's open reservations do not survive a
// restart, or is nil when they do.
func (g *Guard) ReservationsNotKept() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lock == nil && g.notKept == nil {
		return errors.New("closed")
	}
	return g.notKept
}

// Close releases path's lock, so another Guard can own the reservations.
func (g *Guard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lock == nil {
		return nil
	}
	err := g.lock.Close()
	g.lock = nil
	return err
}

// Reserve atomically checks a prospective request whose ticket ceiling is
// amount microUSDC against both caps and, if it fits, records that ceiling
// against ticketID as outstanding exposure — the check and the add happen under
// one lock, so concurrent callers can't both pass a check that only one should
// (the check-then-act race an Allow/Record split has). The daily check counts
// today's settled spend plus the ceilings of every open ticket plus this one, so
// the worst-case drain stays bounded by the cap. On a cap breach it reserves
// nothing and returns the error.
//
// The caller pairs a successful Reserve with Settle (the receipt verified) or
// Release (the open never committed, or the receipt failed verification). Both
// are keyed by the same ticketID and both are idempotent, so it is safe to
// resolve a ticket from more than one place without double counting.
//
// A ticket id that already has an open reservation is REFUSED, not silently
// accepted. ticketID is minted by the operator's node and the proxy only checks
// that it is non-empty, so it is not a value this process controls: accepting a
// duplicate would admit a request without evaluating the daily cap at all (the
// dedup branch runs before the cap check), and would then let the second
// request's Settle/Release resolve the FIRST request's reservation. Refusing
// fails closed — the cost is one wasted operator reservation against a node that
// is already misbehaving. That refusal wraps ErrDuplicateTicket so the caller can
// report it as operator misbehaviour rather than as a spend-cap breach.
func (g *Guard) Reserve(ticketID string, amount uint64) error {
	if err := g.checkPerRequest(amount); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollOutstanding()
	if _, dup := g.open[ticketID]; dup {
		return fmt.Errorf("%w: %q (operator reused a ticket id)", ErrDuplicateTicket, ticketID)
	}
	if err := g.checkDaily(amount); err != nil {
		return err
	}
	g.open[ticketID] = reservation{amount: amount, day: g.day}
	g.outstanding += amount
	g.saveOpen()
	return nil
}

// The SDK calls the guard directly; there is no adapter.
var _ zs.SpendGuard = (*Guard)(nil)

// Admit runs Reserve's two cap checks on the SDK's pre-reserve estimate and
// records nothing, so a request already over a cap is refused before any node
// makes a reservation for it. Reserve checks again against the signed ceiling.
// The estimate is priced from advertised rates, so near a cap it can refuse a
// request whose node would have signed a ticket just under it.
func (g *Guard) Admit(estimate uint64) error {
	if err := g.checkPerRequest(estimate); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollOutstanding()
	return g.checkDaily(estimate)
}

func (g *Guard) checkPerRequest(amount uint64) error {
	if g.cfg.PerRequestCapMicroUSDC > 0 && amount > g.cfg.PerRequestCapMicroUSDC {
		return fmt.Errorf("per-request spend cap exceeded: %d > %d microUSDC", amount, g.cfg.PerRequestCapMicroUSDC)
	}
	return nil
}

// checkDaily runs under g.mu. It compares by subtraction: the sum it bounds
// can wrap uint64, and a wrapped sum passes.
func (g *Guard) checkDaily(amount uint64) error {
	budget := g.cfg.DailyCapMicroUSDC
	if budget == 0 {
		return nil
	}
	spent := g.load().Spent
	if spent > budget || g.outstanding > budget-spent || amount > budget-spent-g.outstanding {
		return fmt.Errorf("daily spend cap exceeded: %d spent + %d reserved + %d > %d microUSDC",
			spent, g.outstanding, amount, g.cfg.DailyCapMicroUSDC)
	}
	return nil
}

// Settle records a completed request whose signed receipt verified: it drops the
// ticket's reservation from the outstanding exposure and adds the
// operator's actual charge (spent microUSDC, <= the reserved MaxPrice) to
// today's persisted spent ledger. A zero charge is fine.
//
// Idempotent: a second Settle (or a Settle after Release) for the same ticket
// records nothing further. Best-effort persistence: a write failure, which is
// logged, only risks under-counting spent across a restart.
//
// A ticket reserved before midnight UTC and settled after it still has its
// reservation (see rollOutstanding), so its charge lands on the new day's tally.
//
// The charge is written before the reservation is dropped from the open file:
// a crash between the two counts the ticket twice until the SDK's reconciler
// releases the reservation or it ages out, where the other order would forget
// the charge.
func (g *Guard) Settle(ticketID string, spent uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollOutstanding()
	if !g.dropOpen(ticketID) {
		return
	}
	g.saved(g.addSpent(spent))
	g.saveOpen()
}

// Release backs a ticket's reservation out of the outstanding exposure without
// recording any spend — the escrow open never committed, the operator's
// receipt failed verification, or a refund closed the ticket. Call it only
// when no charge can land:
// a request whose escrow IS open must keep its reservation (see Guard).
//
// Idempotent: releasing a ticket that already settled, or was already released,
// does nothing.
func (g *Guard) Release(ticketID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollOutstanding()
	if g.dropOpen(ticketID) {
		g.saveOpen()
	}
}

// SpentToday returns the microUSDC actually charged against the current UTC day
// — the sum of receipt AmountCharged values recorded via Settle, which resets
// at midnight UTC. This is the real spend `zs status` shows, not a reservation
// ceiling.
func (g *Guard) SpentToday() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.load().Spent
}

// dropOpen removes ticketID's reservation and subtracts it from the outstanding
// exposure, reporting whether there was one to remove. A false return is what
// makes Settle and Release idempotent. Caller holds mu.
func (g *Guard) dropOpen(ticketID string) bool {
	r, ok := g.open[ticketID]
	if !ok {
		return false
	}
	delete(g.open, ticketID)
	amount := r.amount
	// Exact by construction now that the amount comes from the reservation
	// itself, but keep the floor: an underflow here would wrap to ~1.8e19 and
	// wedge the daily cap shut for the rest of the day.
	if amount >= g.outstanding {
		g.outstanding = 0
	} else {
		g.outstanding -= amount
	}
	return true
}

// rollOutstanding runs on the first call of a new UTC day, and on a Guard's
// first call. It keeps the reservations made yesterday: their escrows can
// still be open, so they count against today's cap and a late Settle records
// their charge. Today's are kept too, which only a restart can bring here.
// Anything older is dropped, since a ticket lives for minutes and one that
// never resolved would otherwise hold headroom forever. Yesterday is the
// calendar day, not the day of the last roll, which after an idle stretch is
// days earlier. The persisted spent ledger rolls over independently in
// load(). Caller holds mu.
func (g *Guard) rollOutstanding() {
	now := g.now().UTC()
	today := now.Format("2006-01-02")
	if g.day == today {
		return
	}
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	g.day = today
	g.outstanding = 0
	for id, r := range g.open {
		if r.day != yesterday && r.day != today {
			delete(g.open, id)
			continue
		}
		g.outstanding += r.amount
	}
}

func (g *Guard) today() string { return g.now().UTC().Format("2006-01-02") }

// load reads the persisted tally, resetting it when the date has rolled over
// (or when there's no file / a parse error — fail open to an empty day).
func (g *Guard) load() tally {
	t := g.read()
	if t.Date != g.today() {
		return tally{Date: g.today()}
	}
	return t
}

// read is the spent file as written, any day's.
func (g *Guard) read() tally {
	var t tally
	if g.path == "" {
		return t
	}
	if b, err := os.ReadFile(g.path); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	return t
}

// openPath is the open-reservations file beside the spent file.
func (g *Guard) openPath() string { return OpenPath(g.path) }

// OpenPath is the open-reservations file beside the spent file at path, so a
// caller moving a tally can move its reservations with it. "" for "".
func OpenPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + ".open.json"
}

// readOpen reads the open-reservations file; a missing one is empty.
func readOpen(path string) (map[string]openRow, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows map[string]openRow
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", path, err)
	}
	return rows, nil
}

// addSpent adds amount to today's spent ledger. Every Guard on path writes
// it, so the read and the write happen under a lock they all take: without
// it, one Guard's write would replace a charge another just recorded.
func (g *Guard) addSpent(amount uint64) error {
	if g.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return err
	}
	wlock, err := os.OpenFile(g.path+".wlock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer wlock.Close()
	if err := lockFileWait(wlock); err != nil {
		return err
	}
	t := g.load()
	t.Spent += amount
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeFileAtomic(g.path, b)
}

// saveOpen writes this Guard's open reservations, if it holds the lock.
// Caller holds mu.
func (g *Guard) saveOpen() {
	if g.lock == nil {
		return
	}
	rows := make(map[string]openRow, len(g.open))
	for id, r := range g.open {
		rows[id] = openRow{Amount: r.amount, Day: r.day}
	}
	b, err := json.Marshal(rows)
	if err == nil {
		err = writeFileAtomic(g.openPath(), b)
	}
	g.saved(err)
}

// saved logs the first of a run of failed saves. Caller holds mu.
func (g *Guard) saved(err error) {
	if err == nil {
		g.saveFailing = false
		return
	}
	if !g.saveFailing {
		slog.Warn("spend: could not save the daily spend tally; a restart may under-count today's spend", slog.Any("err", err))
	}
	g.saveFailing = true
}

// writeFileAtomic replaces path with b by rename after an fsync, so a crash
// or power loss leaves the old file or the new one, never a truncated file
// the guard would read as an empty day.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+tempSuffix)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = replaceFile(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// tempSuffix names writeFileAtomic's temp files after the file they replace.
const tempSuffix = ".tmp-*"

// removeStaleTemps deletes temp files a crash left beside path. One younger
// than a minute may be another Guard's write in progress.
func removeStaleTemps(path string) {
	temps, _ := filepath.Glob(path + tempSuffix)
	for _, t := range temps {
		if fi, err := os.Stat(t); err == nil && time.Since(fi.ModTime()) > time.Minute {
			os.Remove(t)
		}
	}
}
