/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// concurrentSlotsKeyPath is the config location `slots <n>` writes. Kept next
// to the command rather than in internal/config so the writer stays a generic
// scalar setter with exactly one caller naming the key.
var concurrentSlotsKeyPath = []string{"zs", "concurrent_slots"}

// slotsEnvVar shadows a written value; applyEnv gives env precedence over the
// file, so writing config while this is set is a silent no-op from the user's
// point of view unless we say so. Taken from config rather than spelled out
// again here: a local copy could drift from the variable applyEnv actually
// reads, and that drift is invisible — the warning just stops firing.
const slotsEnvVar = config.EnvConcurrentSlots

// newSlotsCmd is `zs slots [n]` — show, or resize, how many escrow tickets this
// payer can hold open at once.
//
// The knob is a funding target, not a limiter: the contract admits an open on
// (openTickets+1) × mbrForTicket() <= algoBalance, so the pool's ALGO IS the
// cap. That is why setting it is a two-part operation — write the config value
// AND move the pool to match — and why either half alone leaves the user with a
// number that isn't true.
func newSlotsCmd() *cobra.Command {
	var (
		f        proxyFlags
		assumeY  bool
		dryRun   bool
		wantHelp = "Show how many escrow tickets can be open at once, or set a new limit.\n" +
			"With no argument this only reads — it never signs or spends. With <n> it\n" +
			"writes zs.concurrent_slots to your config AND resizes the prepaid ticket-MBR\n" +
			"pool on chain to match: depositing when you raise it, reclaiming the\n" +
			"unreserved excess when you lower it. The ALGO is fully recoverable.\n\n" +
			"On Solana the pool is the ticket rent pool and the amounts are SOL."
	)
	cmd := &cobra.Command{
		Use:          "slots [n]",
		Short:        "Show or set how many tickets can be open at once",
		Long:         wantHelp,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var want uint64
			if len(args) == 1 {
				n, err := parseSlots(args[0])
				if err != nil {
					return err
				}
				want = n
			}
			e, err := f.solanaRuntime(ctx)
			if err != nil {
				return err
			}
			switch {
			case e != nil && len(args) == 0:
				return runSolanaSlotsShow(ctx, cmd, e)
			case e != nil:
				return runSolanaSlotsSet(ctx, cmd, &f, e, want, assumeY, dryRun)
			case len(args) == 0:
				return runSlotsShow(ctx, cmd, &f)
			}
			return runSlotsSet(ctx, cmd, &f, want, assumeY, dryRun)
		},
	}
	f.bindCommon(cmd)
	cmd.Flags().BoolVarP(&assumeY, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would change; write nothing, submit nothing")
	return cmd
}

// maxConcurrentSlots is the upper bound `slots <n>` accepts. It is a
// correctness guard first and a sanity check second: the pool sizes as
// `slots * mbrForTicket()`, so a large enough count WRAPS uint64 into a tiny
// target. That inverts the command — a raise computes an excess, reclaims the
// pool's free headroom, and writes a number the daemon then reads as already
// satisfied forever, so it never re-funds and can no longer open tickets.
//
// 100k slots is ~11,550 ALGO of recoverable deposit, orders of magnitude past
// any real workload, and leaves a factor of ~10^11 of headroom under the wrap.
const maxConcurrentSlots = 100_000

// parseSlots validates the requested slot count. 0 is refused rather than
// accepted: in config it means "fall back to the built-in default", which is
// not what someone typing a number is asking for — and reading it as "no limit"
// would be exactly backwards.
func parseSlots(arg string) (uint64, error) {
	n, err := strconv.ParseUint(arg, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid slot count %q: want a whole number of concurrent tickets", arg)
	}
	if n == 0 {
		return 0, fmt.Errorf("slot count must be at least 1 (omit %s from your config to use the default of %d)",
			concurrentSlotsKeyPath[1], zs.DefaultDepositSlots)
	}
	if n > maxConcurrentSlots {
		return 0, fmt.Errorf("slot count %d is above the maximum of %d — each slot locks ~0.115 ALGO (~0.003 SOL) of recoverable deposit, so that is far past any real workload",
			n, maxConcurrentSlots)
	}
	return n, nil
}

func runSlotsShow(ctx context.Context, cmd *cobra.Command, f *proxyFlags) error {
	rt, err := f.openRuntime(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	configured := slotsOrDefault(rt.cfg.Zs.ConcurrentSlots)

	// The pool read simulates from the payer, so it needs a funded account —
	// same gate status uses. An unfunded wallet still gets the configured
	// number, which is the half of the answer that doesn't need a chain.
	var view slotsView
	info, ierr := account.Read(ctx, rt.algc, rt.meta.Address, rt.usdc)
	if ierr == nil && info.Funded() {
		if ps, derr := readPoolStatus(ctx, rt.cfg, rt.meta.Address); derr == nil {
			view = slotsViewFrom(configured, ps)
		}
	}
	for _, line := range renderSlots(configured, view) {
		fmt.Fprintln(out, line)
	}
	warnSlotsEnvShadow(cmd)
	return nil
}

// slotsPool is the pool a rail backs its slots with, which names it and its
// unit to the user. The zero value is Algorand's, the rail every caller
// predates.
type slotsPool uint8

const (
	algorandMBRPool slotsPool = iota
	solanaRentPool
)

func (p slotsPool) name() string {
	if p == solanaRentPool {
		return "rent pool"
	}
	return "MBR pool"
}

func (p slotsPool) amount(v uint64) string {
	if p == solanaRentPool {
		return lamportsSOL(v)
	}
	return microAlgos(v)
}

// slotsView is the on-chain half of the display, absent when the pool couldn't
// be read (unfunded wallet, chain unreachable). known distinguishes "no pool
// data" from "a pool of zero".
type slotsView struct {
	pool      slotsPool
	known     bool
	deposited uint64
	ticketMbr uint64
	open      uint64
	backed    uint64
	shortfall uint64
	excess    uint64
}

func slotsViewFrom(configured uint64, ps poolStatus) slotsView {
	v := slotsView{
		known:     ps.ticketMbr != 0,
		deposited: ps.dep.AlgoBalance,
		ticketMbr: ps.ticketMbr,
		open:      ps.dep.OpenTickets,
	}
	if !v.known {
		return v
	}
	v.backed = backedSlots(ps.ticketMbr, ps.dep)
	v.shortfall = poolShortfall(configured, ps.ticketMbr, ps.quotaMbrDue, ps.dep)
	v.excess = poolExcess(configured, ps.ticketMbr, ps.quotaMbrDue, ps.dep)
	return v
}

// renderSlots is the pure display core, so the layout is testable without a
// chain. `configured` is the target; `backed` is what the deposit actually
// buys, and the two genuinely differ — the contract admits on the deposit, so
// backed is the real limit and configured is only what the next top-up aims for.
func renderSlots(configured uint64, v slotsView) []string {
	lines := []string{
		fmt.Sprintf("configured:  %d slot(s)   zs.%s", configured, concurrentSlotsKeyPath[1]),
	}
	if !v.known {
		return append(lines,
			"pool:        unknown — fund the wallet first (`zs-proxy fund`), then re-run")
	}
	lines = append(lines,
		fmt.Sprintf("backed:      %d slot(s)   %s deposited", v.backed, v.pool.amount(v.deposited)),
		fmt.Sprintf("in use:      %d slot(s)", v.open),
		fmt.Sprintf("per slot:    %s", v.pool.amount(v.ticketMbr)),
	)
	switch {
	case v.shortfall > 0:
		lines = append(lines, fmt.Sprintf(
			"note:        %s short of %d slot(s) — run `zs-proxy slots %d` to top up",
			v.pool.amount(v.shortfall), configured, configured))
	case v.excess > 0:
		lines = append(lines, fmt.Sprintf(
			"note:        %s more than %d slot(s) needs — lower the number to reclaim it",
			v.pool.amount(v.excess), configured))
	}
	return lines
}

func runSlotsSet(ctx context.Context, cmd *cobra.Command, f *proxyFlags, want uint64, assumeYes, dryRun bool) error {
	rt, err := f.openRuntime(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	current := slotsOrDefault(rt.cfg.Zs.ConcurrentSlots)

	info, err := account.Read(ctx, rt.algc, rt.meta.Address, rt.usdc)
	if err != nil {
		return fmt.Errorf("read account: %w", err)
	}
	if !info.Funded() {
		return fmt.Errorf("wallet %s is not funded — run `zs-proxy fund` first", rt.meta.Address)
	}
	ps, err := readPoolStatus(ctx, rt.cfg, rt.meta.Address)
	if err != nil {
		return fmt.Errorf("read prepaid pool: %w", err)
	}
	if ps.ticketMbr == 0 {
		return errors.New("the escrow contract did not report a per-ticket MBR — cannot size the pool")
	}

	d := decideSlots(current, want, ps, dryRun)
	fmt.Fprintln(out, d.plan)

	// Only prompt when ALGO actually moves. A config-only change (already at
	// target, or nothing reclaimable) has nothing to consent to.
	//
	// confirmYesNo proceeds on a non-TTY, which is deliberate and matches the
	// rest of the module: `fund` and `withdraw` move ALGO with no prompt at
	// all, so blocking here would make this the one funding command that can't
	// be scripted. The prompt exists because `slots <n>` is the only one whose
	// argument is a bare count rather than an amount — the plan line above is
	// what turns that count into an ALGO figure before anyone agrees to it.
	if d.needsConsent {
		if !confirmYesNo(cmd, "Continue?", false, assumeYes) {
			return errors.New("aborted")
		}
	}

	target, err := slotsConfigTarget(f)
	if err != nil {
		return err
	}
	ex := slotsExec{
		deposit: func(ctx context.Context, want uint64) error {
			w, err := openDepositWallet(ctx, rt)
			if err != nil {
				return err
			}
			return depositForSlots(ctx, out, w, want)
		},
		withdraw: func(ctx context.Context, amount uint64) (string, error) {
			w, err := openDepositWallet(ctx, rt)
			if err != nil {
				return "", err
			}
			return w.WithdrawDeposit(ctx, amount)
		},
		writeConfig: func(v uint64) error {
			return writeSlotsConfig(target, v)
		},
		configPath: target,
	}
	if err := applySlots(ctx, out, d.resize, ex); err != nil {
		return err
	}

	slotsWarnings(cmd, d, resolveTarget())
	return nil
}

// slotsWarnings emits the post-write advisories. It takes the resolved
// lifecycle target rather than calling resolveTarget itself so the
// running-vs-not predicate is reachable from a test: the `kind != targetNone`
// comparison lives at exactly one place, and inverting it there used to fire
// the restart warning when NO proxy was running and stay silent on the
// lowerings the warning exists for — the failure restartWarningNeeded is
// written to prevent, left open one layer above it.
func slotsWarnings(cmd *cobra.Command, d slotsDecision, target lifecycleTarget) {
	warnSlotsEnvShadow(cmd)
	if restartWarningNeeded(d, target.kind != targetNone) {
		warnSlotsRestart(cmd)
	}
}

// slotsDecision is everything `slots <n>` decides before touching anything:
// what to do, what to tell the user it will do, whether to ask first, and
// whether a running proxy would undo it.
//
// It exists because the command body around it cannot be unit-tested — it needs
// a chain, a keystore and a config file — so every argument passed from there
// into the well-tested helpers below was itself unverified. Swapping
// `current`/`want`, handing `excess` where `withdrawable` belongs, or dropping
// the dry-run return are all one-token edits that no test could see. Deriving
// them once, here, from the chain reads makes the whole set assertable.
type slotsDecision struct {
	resize       slotsResize
	plan         string
	needsConsent bool
	lowered      bool
}

// decideSlots turns the current config and a pool snapshot into that decision.
func decideSlots(current, want uint64, ps poolStatus, dryRun bool) slotsDecision {
	return slotsDecisionFor(algorandMBRPool, current, want, slotsSizing{
		shortfall:    poolShortfall(want, ps.ticketMbr, ps.quotaMbrDue, ps.dep),
		excess:       poolExcess(want, ps.ticketMbr, ps.quotaMbrDue, ps.dep),
		withdrawable: withdrawableFor(want, ps),
		openTickets:  ps.dep.OpenTickets,
	}, dryRun)
}

// slotsSizing is a pool's answer to `want` slots, in its rail's unit.
type slotsSizing struct {
	shortfall, excess, withdrawable, openTickets uint64
}

// slotsDecisionFor is the decision from a rail's sizing, so both rails take
// the same action, plan and consent from the same figures.
func slotsDecisionFor(pool slotsPool, current, want uint64, s slotsSizing, dryRun bool) slotsDecision {
	act := actionFor(s.shortfall, s.withdrawable, dryRun)
	return slotsDecision{
		resize: slotsResize{
			pool:         pool,
			want:         want,
			act:          act,
			withdrawable: s.withdrawable,
			excess:       s.excess,
			openTickets:  s.openTickets,
			dryRun:       dryRun,
		},
		plan: slotsPlan(pool, current, want, s.shortfall, s.excess, s.withdrawable),
		// Consent is owed exactly when ALGO moves — which is what act says, so
		// asking the same question twice from two different expressions can't
		// drift into a prompt that fires when nothing happens (or, worse, one
		// that stays silent when something does).
		needsConsent: act != slotsActionNone,
		lowered:      want < current,
	}
}

// slotsResize is the decided plan applySlots carries out — everything the
// sequencing needs, with the chain reads already done.
type slotsResize struct {
	pool         slotsPool
	want         uint64
	act          slotsAction
	withdrawable uint64
	excess       uint64
	openTickets  uint64
	// dryRun makes applySlots a no-op. It lives here, rather than as an early
	// return in the command body, because --dry-run promises to write nothing
	// AND submit nothing — and the config write is as much a side effect as the
	// chain leg. Guarding only the caller leaves the write half of that promise
	// outside anything a test can reach.
	dryRun bool
}

// slotsExec is the seam between the decision and the two things that actually
// change state. It exists so the ORDER below — and the error wrapping when the
// second step fails after the first succeeded — can be asserted without a
// chain, a keystore, or a config file. That sequencing is the part a reader
// can't verify by inspection and the part that costs real ALGO to get wrong.
type slotsExec struct {
	deposit     func(ctx context.Context, want uint64) error
	withdraw    func(ctx context.Context, amount uint64) (txid string, err error)
	writeConfig func(value uint64) error
	configPath  string
}

// applySlots runs the chain leg then the config write, in that order.
//
// Chain FIRST: a written target the pool doesn't back is a number that lies to
// every later read. The reverse — a resized pool with a stale config — is
// merely a value the next top-up re-aims at, and the error below says so
// explicitly rather than letting a failed write read as a failed resize.
func applySlots(ctx context.Context, out io.Writer, p slotsResize, ex slotsExec) error {
	if p.dryRun {
		fmt.Fprintln(out, "(dry run — nothing written, nothing submitted)")
		return nil
	}
	moved := false
	switch p.act {
	case slotsActionDeposit:
		if err := ex.deposit(ctx, p.want); err != nil {
			return err
		}
		moved = true
	case slotsActionWithdraw:
		txid, err := ex.withdraw(ctx, p.withdrawable)
		if err != nil {
			return fmt.Errorf("reclaim from %s: %w", p.pool.name(), err)
		}
		moved = true
		fmt.Fprintf(out, "✔ reclaimed %s from the %s (%s)\n", p.pool.amount(p.withdrawable), p.pool.name(), txid)
		// The gap between what the pool is oversized by and what the contract
		// would admit is exactly the ALGO in-flight tickets are holding. Naming
		// it is the difference between "we took less than we said" and a user
		// who knows to re-run.
		if left := p.excess - p.withdrawable; left > 0 {
			fmt.Fprintf(out, "⚠ %s stays in the pool — %d ticket(s) are still in flight holding it; re-run once they settle\n",
				p.pool.amount(left), p.openTickets)
		}
	}

	if err := ex.writeConfig(p.want); err != nil {
		if moved {
			return fmt.Errorf("the pool was resized but the config was not updated: %w", err)
		}
		return fmt.Errorf("update the config: %w", err)
	}
	fmt.Fprintf(out, "✔ wrote zs.%s: %d to %s\n", concurrentSlotsKeyPath[1], p.want, ex.configPath)
	return nil
}

// openDepositWallet builds the signing wallet for a pool move. Split out so the
// keystore is touched only on the paths that actually submit a transaction.
func openDepositWallet(ctx context.Context, rt *runtimeEnv) (*zs.Wallet, error) {
	ks, err := buildKeystore(ctx)
	if err != nil {
		return nil, err
	}
	w, err := buildDepositWallet(ctx, rt, ks)
	if err != nil {
		return nil, fmt.Errorf("build deposit wallet: %w", err)
	}
	return w, nil
}

// slotsAction is what `slots <n>` will do on chain. Naming the three outcomes
// (rather than re-deriving them from two µALGO figures at each use) is what
// lets the deposit-vs-withdraw choice be decided once and tested without a
// chain — every branch below it moves real ALGO.
type slotsAction uint8

const (
	slotsActionNone slotsAction = iota
	slotsActionDeposit
	slotsActionWithdraw
)

// withdrawableFor is how much a shrink to `want` can actually reclaim right now.
//
// The two terms answer different questions and the smaller one wins: excess is
// how oversized the pool is (gross basis), while FreePoolHeadroom is what the
// contract will admit a withdraw against — it reverts on anything above the
// unreserved balance. They diverge by exactly the tickets in flight, so an
// oversized-but-busy pool reclaims less than its excess, and the caller reports
// the difference rather than silently taking less than it said it would.
func withdrawableFor(want uint64, ps poolStatus) uint64 {
	excess := poolExcess(want, ps.ticketMbr, ps.quotaMbrDue, ps.dep)
	return min(excess, zs.FreePoolHeadroom(ps.ticketMbr, ps.dep))
}

// actionFor decides deposit / withdraw / nothing. A shortfall always outranks a
// withdrawal — they are mutually exclusive by construction, and preferring the
// deposit means a mis-sized pool grows rather than drains if that ever changes.
// --dry-run collapses to None here, so the flag's "submit nothing" contract is
// one testable decision instead of a return statement's placement.
func actionFor(shortfall, withdrawable uint64, dryRun bool) slotsAction {
	if dryRun {
		return slotsActionNone
	}
	switch {
	case shortfall > 0:
		return slotsActionDeposit
	case withdrawable > 0:
		return slotsActionWithdraw
	default:
		return slotsActionNone
	}
}

// restartWarningNeeded is deliberately asymmetric — see warnSlotsRestart for
// why only a lowering is dangerous. It takes the decision rather than a
// (current, want) pair so the two cannot be handed over in the wrong order at
// the one call site, which would flip the warning onto raises and silence it on
// exactly the lowerings it exists for.
func restartWarningNeeded(d slotsDecision, proxyRunning bool) bool {
	return proxyRunning && d.lowered
}

// depositForSlots runs the top-up, reusing EnsureDeposit's own sentinels for
// the partial and unaffordable outcomes rather than re-deriving their messages
// — they already name the exact µALGO involved.
func depositForSlots(ctx context.Context, out io.Writer, w *zs.Wallet, want uint64) error {
	txid, deposited, err := w.EnsureDeposit(ctx, want)
	switch {
	case errors.Is(err, zs.ErrDepositBelowTarget):
		fmt.Fprintf(out, "✔ deposited %s into the MBR pool (%s)\n", microAlgos(deposited), txid)
		fmt.Fprintf(out, "⚠ the pool is still below %d slot(s) — %v\n", want, err)
		return nil
	case err != nil:
		return fmt.Errorf("fund the MBR pool: %w", err)
	case txid == "":
		fmt.Fprintln(out, "✔ MBR pool already backs the new limit")
		return nil
	default:
		fmt.Fprintf(out, "✔ deposited %s into the MBR pool (%s)\n", microAlgos(deposited), txid)
		return nil
	}
}

// slotsPlan is the one-line statement of intent shown before anything happens —
// and the thing the confirmation prompt is consenting to.
func slotsPlan(pool slotsPool, current, want, shortfall, excess, withdrawable uint64) string {
	switch {
	case shortfall > 0:
		// "up to": a wallet short of the whole shortfall funds the slots it
		// affords, and the post-deposit line names what landed.
		return fmt.Sprintf("%d → %d slot(s): depositing up to %s (recoverable)",
			current, want, pool.amount(shortfall))
	case withdrawable > 0:
		return fmt.Sprintf("%d → %d slot(s): reclaiming %s to your wallet",
			current, want, pool.amount(withdrawable))
	case excess > 0:
		return fmt.Sprintf("%d → %d slot(s): nothing reclaimable right now (%s is held by in-flight tickets)",
			current, want, pool.amount(excess))
	default:
		return fmt.Sprintf("%d → %d slot(s): the pool already matches; updating the config only",
			current, want)
	}
}

// writeSlotsConfig persists the new slot count, turning the writer's
// deliberate refusal into something the user can act on.
//
// SetScalar edits only ordinary configs and refuses the rest by design, so
// "unsupported shape" is a likely outcome rather than an exotic one — and on
// its own it leaves the user stuck: the pool has already been resized, so the
// config is the one outstanding half and they need the exact line to add.
func writeSlotsConfig(target string, v uint64) error {
	err := config.SetScalar(target, concurrentSlotsKeyPath, strconv.FormatUint(v, 10))
	if errors.Is(err, config.ErrUnsupportedShape) {
		return fmt.Errorf("%w\n  → the pool is resized; add this to %s by hand:\n\n      %s:\n        %s: %d",
			err, target, concurrentSlotsKeyPath[0], concurrentSlotsKeyPath[1], v)
	}
	return err
}

// slotsConfigTarget picks the file to write: an explicitly resolved --config /
// PROXY_CONFIG path wins, otherwise the state-dir default, which SetScalar
// creates when it doesn't exist yet.
func slotsConfigTarget(f *proxyFlags) (string, error) {
	if f.configPath != "" {
		return f.configPath, nil
	}
	p, err := paths.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return p, nil
}

// warnSlotsEnvShadow says so when the env var outranks the file, because
// applyEnv gives env the last word — without this the user sees "✔ wrote …"
// and a number that never takes effect.
func warnSlotsEnvShadow(cmd *cobra.Command) {
	v, ok := os.LookupEnv(slotsEnvVar)
	if !ok {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"⚠ %s=%s is set and overrides the config file — unset it for the written value to apply\n",
		slotsEnvVar, v)
}

// warnSlotsRestart tells the user when a restart is actually needed, which is
// not symmetric — see restartWarningNeeded for the predicate. The resized pool
// takes effect immediately either way, because the contract admits on the
// deposit rather than on our config. But a running proxy holds the OLD target
// in memory, and its next top-up re-aims at it — so after a LOWERING an
// untouched daemon can quietly re-deposit the ALGO that was just reclaimed. A
// raise has no such failure mode, and warning on it would train the user to
// ignore the warning in the one case that matters.
func warnSlotsRestart(cmd *cobra.Command) {
	fmt.Fprintln(cmd.ErrOrStderr(),
		"⚠ a proxy is running with the previous, higher limit — restart it (`zs-proxy proxy restart`), "+
			"or its next pool top-up will re-deposit what was just reclaimed")
}
