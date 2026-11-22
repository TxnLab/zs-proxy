/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	algodsdk "github.com/algorand/go-algorand-sdk/v2/client/v2/algod"

	protoalgod "github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/escrow"
	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/account"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// runtimeEnv bundles the resolved config + wallet + algod client + USDC asset
// id that the wallet-facing commands (status/doctor/fund) share.
type runtimeEnv struct {
	cfg  *config.Config
	meta wallet.Metadata
	algc *algodsdk.Client
	usdc uint64
}

// openRuntime resolves config, requires a wallet, dials algod, and resolves
// the USDC asset id the escrow contract uses (on-chain).
func (f *proxyFlags) openRuntime(ctx context.Context) (*runtimeEnv, error) {
	cfg, err := f.resolveConfig()
	if err != nil {
		return nil, err
	}
	// Every caller reads algod; on Solana it would fail on the empty escrow
	// app id, which names the wrong cause.
	if cfg.OnSolana() {
		return nil, fmt.Errorf("this command reads the Algorand rail, and %s is a Solana network", cfg.NetworkName())
	}
	if !wallet.DefaultExists() {
		return nil, errors.New("no wallet yet — run `zs-proxy wallet login`, `zs-proxy wallet import`, or `zs-proxy wallet new`")
	}
	// The address alone, so a wallet whose backend does not open in this
	// session (a desktop keyring over SSH) can still be inspected and funded.
	meta, err := wallet.DefaultMetadata()
	if err != nil {
		return nil, err
	}
	c, err := account.Dial(cfg.Algod.ResolvedEndpoint, cfg.Algod.ResolvedToken)
	if err != nil {
		return nil, err
	}
	usdc, err := escrowUSDCAssetID(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("read USDC asset id from escrow contract: %w", err)
	}
	return &runtimeEnv{cfg: cfg, meta: meta, algc: c, usdc: usdc}, nil
}

// escrowUSDCAssetID returns the USDC ASA the ZeroSignalEscrow contract is configured
// to escrow — read from the contract's on-chain global state, the single source
// of truth (there is no embedded asset-id fallback: an asset id is a property
// of the deployed contract, not the network name). RefreshAssetIDs is a pure
// read with no payer simulate, so it works even for an unfunded wallet.
func escrowUSDCAssetID(ctx context.Context, cfg *config.Config) (uint64, error) {
	if cfg.Zs.EscrowAppID == 0 {
		return 0, errors.New("no escrow app id configured")
	}
	ac, err := protoalgod.NewClient(cfg.Algod.ResolvedEndpoint, cfg.Algod.ResolvedToken)
	if err != nil {
		return 0, err
	}
	esc, err := escrow.NewClient(cfg.Zs.EscrowAppID, ac)
	if err != nil {
		return 0, err
	}
	if err := esc.RefreshAssetIDs(ctx); err != nil {
		return 0, err
	}
	return esc.UsdcAssetID, nil
}

// buildDepositWallet constructs a zs.Wallet for prepaid-MBR-pool
// operations (deposit / withdraw / close) from the CLI runtime. Mirrors
// the server's wallet build: a fresh escrow client with RefreshConfig
// loading TicketMbr + the USDC asset id (simulated from the payer, which
// the caller has confirmed is funded). Distinct from the read-only `algc`
// SDK client the status/fund balance reads use.
func buildDepositWallet(ctx context.Context, rt *runtimeEnv, ks *keystore.KeyStore) (*zs.Wallet, error) {
	if rt.cfg.Zs.EscrowAppID == 0 {
		return nil, errors.New("no escrow app id configured")
	}
	ac, err := protoalgod.NewClient(rt.cfg.Algod.ResolvedEndpoint, rt.cfg.Algod.ResolvedToken)
	if err != nil {
		return nil, err
	}
	ec, err := escrow.NewClient(rt.cfg.Zs.EscrowAppID, ac)
	if err != nil {
		return nil, err
	}
	if err := ec.RefreshConfig(ctx, rt.meta.Address); err != nil {
		return nil, err
	}
	return zs.NewWallet(zs.WalletConfig{
		Escrow:       ec,
		Keystore:     ks,
		PayerAddr:    rt.meta.Address,
		DepositSlots: rt.cfg.Zs.ConcurrentSlots,
	})
}

// poolStatus is one read-only snapshot of the payer's prepaid-MBR-pool
// position, plus the escrow client it came from so a caller that wants more
// (the free-ticket allowance) doesn't pay for a second client build.
type poolStatus struct {
	dep       escrow.PayerDeposit
	ticketMbr uint64 // per-ticket box MBR; 0 = unknown, so slot counts are unavailable
	// quotaMbr is the app's free-quota box term (0 = the deployed app has no
	// free-ticket quota at all); quotaMbrDue is what THIS payer still owes for
	// it (0 once they have a box). quotaMbr > 0 && quotaMbrDue > 0 therefore
	// means "the quota exists and this payer hasn't created their box yet".
	quotaMbr    uint64
	quotaMbrDue uint64
	esc         *escrow.Client
}

// readPoolStatus reads the payer's prepaid-MBR-pool state + the per-ticket box
// MBR read-only (no keystore / no signing prompt) — for status/doctor display.
// Builds a signer-less escrow client; RefreshConfig loads TicketMbr and the
// payerDeposit(payer) simulate runs with the payer as sender, so it needs a
// funded on-chain account (callers gate on info.Funded() and treat errors as
// "unknown"). Returns the deposit + the box MBR so the caller can size the
// shortfall against the configured working set via zs.DepositShortfall.
func readPoolStatus(ctx context.Context, cfg *config.Config, payerAddr string) (poolStatus, error) {
	if cfg.Zs.EscrowAppID == 0 {
		return poolStatus{}, errors.New("no escrow app id configured")
	}
	ac, err := protoalgod.NewClient(cfg.Algod.ResolvedEndpoint, cfg.Algod.ResolvedToken)
	if err != nil {
		return poolStatus{}, err
	}
	ec, err := escrow.NewClient(cfg.Zs.EscrowAppID, ac)
	if err != nil {
		return poolStatus{}, err
	}
	if err := ec.RefreshConfig(ctx, payerAddr); err != nil {
		return poolStatus{}, err
	}
	dep, err := ec.ReadPayerDeposit(ctx, payerAddr)
	if err != nil {
		return poolStatus{}, err
	}
	return poolStatus{
		dep:       dep,
		ticketMbr: ec.TicketMbr,
		quotaMbr:  ec.FreeQuotaMbr,
		// Exact per-payer term: 0 once this payer already has a quota box.
		quotaMbrDue: ec.FreeQuotaMbrDueFor(ctx, payerAddr),
		esc:         ec,
	}, nil
}

// poolShortfall reports the µALGO the pool still needs to TOTAL `slots` ticket
// boxes' worth of MBR (plus any free-quota box term still owed) — the same
// figure EnsureDeposit would top up. 0 (or ticketMbr unknown) means fully
// backed. Sized on gross deposit, so a busy proxy is not reported as short
// merely because tickets are in flight; see zs.DepositShortfall.
func poolShortfall(slots, ticketMbr, freeQuotaMbr uint64, dep escrow.PayerDeposit) uint64 {
	if ticketMbr == 0 {
		return 0
	}
	return zs.DepositShortfall(slots, ticketMbr, freeQuotaMbr, dep)
}

// poolExcess reports the µALGO the pool holds ABOVE `slots` ticket boxes'
// worth of MBR (plus any free-quota term still owed) — what shrinking the
// working set to `slots` would release. 0 (or ticketMbr unknown) means there is
// nothing to reclaim. Sizing only; bound an actual withdraw by
// zs.FreePoolHeadroom, which is what the contract admits against.
func poolExcess(slots, ticketMbr, freeQuotaMbr uint64, dep escrow.PayerDeposit) uint64 {
	if ticketMbr == 0 {
		return 0
	}
	return zs.DepositExcess(slots, ticketMbr, freeQuotaMbr, dep)
}

// backedSlots is how many ticket boxes the deposit ACTUALLY backs, which is the
// real concurrency limit: open() admits on
// (openTickets+1) * mbrForTicket() <= algoBalance, so this quotient is what the
// contract enforces — not the configured working set, which is only the target
// the next top-up aims at.
//
// TRUNCATES on purpose. A pool holding 10.6 slots' worth backs ten; rounding
// would report eleven and the eleventh open would revert. Shared by `status`
// and `slots` so the two can never print different denominators for the same
// pool. ticketMbr == 0 means the box MBR wasn't loaded, so no count is knowable.
func backedSlots(ticketMbr uint64, dep escrow.PayerDeposit) uint64 {
	if ticketMbr == 0 {
		return 0
	}
	backed := dep.AlgoBalance / ticketMbr
	if backed < dep.OpenTickets {
		// The contract's pool invariant makes this unreachable; render the
		// honest count rather than a denominator below the numerator.
		backed = dep.OpenTickets
	}
	return backed
}

// slotsOrDefault resolves the configured working set to a concrete slot count
// for display, mirroring EnsureDeposit's 0 → DefaultDepositSlots fallback.
func slotsOrDefault(slots uint64) uint64 {
	if slots == 0 {
		return zs.DefaultDepositSlots
	}
	return slots
}

// microAlgos / microUSDC render a 6-decimal fixed-point amount.
func microAlgos(v uint64) string { return fmt.Sprintf("%s ALGO", fixed6(v)) }
func microUSDC(v uint64) string  { return fmt.Sprintf("%s USDC", fixed6(v)) }

func fixed6(v uint64) string {
	return fmt.Sprintf("%d.%06d", v/1_000_000, v%1_000_000)
}

// check prints a ✔/✘ checklist line, with a hint shown only on failure.
func check(w io.Writer, ok bool, label, hint string) {
	if ok {
		fmt.Fprintf(w, "  ✔ %s\n", label)
		return
	}
	if hint != "" {
		fmt.Fprintf(w, "  ✘ %s — %s\n", label, hint)
	} else {
		fmt.Fprintf(w, "  ✘ %s\n", label)
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// keyringHint annotates an OS-keychain access failure (a cancelled/denied
// prompt — e.g. macOS -128 on an unsigned binary) with how to proceed, so the
// user isn't left with a bare error after dismissing a system dialog.
func keyringHint(err error) error {
	// A missing phrase or an existing wallet is not an access failure, though
	// its message may name the keychain.
	if err == nil || errors.Is(err, wallet.ErrNotFound) || wallet.IsExists(err) {
		return err
	}
	s := err.Error()
	if strings.Contains(s, "-128") || strings.Contains(s, "User canceled") ||
		strings.Contains(s, "Keychain") || strings.Contains(s, "keychain") {
		return fmt.Errorf("%w\n  → keychain access was denied or cancelled. Re-run and approve the system prompt, "+
			"or set ZEROSIGNAL_KEYRING_BACKEND=file to store the wallet in an encrypted file instead", err)
	}
	return err
}

// portFree reports whether the configured listen address can be bound.
func portFree(listen string) bool {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// hasEnvMnemonic reports whether a signing mnemonic is configured via env
// (a *_MNEMONIC var or ZS_MNEMONIC_URLS). A user who brings their own signer is
// neither prompted to set up a wallet nor refused for lacking one. An empty
// value doesn't count, because the keystore skips it: counting it would skip
// the guided refusal only to fail later on zero loaded signers.
func hasEnvMnemonic() bool {
	for _, e := range os.Environ() {
		name, val, _ := strings.Cut(e, "=")
		if strings.HasSuffix(name, "_MNEMONIC") && strings.TrimSpace(val) != "" {
			return true
		}
	}
	return strings.TrimSpace(os.Getenv(keystore.MnemonicURLsEnv)) != ""
}
