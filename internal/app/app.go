/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package app builds and runs the proxy HTTP server from a resolved
// configuration. It is the importable counterpart of the former main():
// no flag parsing, no os.Exit — callers (the CLI) own those and supply a
// cancelable context plus an already-loaded *config.Config.
package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/httpx"
	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/selection"
	"github.com/TxnLab/zerosignal/go/wallet"
	"github.com/TxnLab/zerosignal/go/zs"
	server "github.com/TxnLab/zerosignal/go/zs/handler"
	"github.com/TxnLab/zerosignal/go/zs/handler/transform"
	"github.com/TxnLab/zerosignal/go/zs/tee"
	"github.com/TxnLab/zerosignal/go/zs/tokenize"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/spend"
)

// Options carries the runtime toggles that used to be top-level flags.
// Everything else is derived from the *config.Config passed to Run.
type Options struct {
	// TrackCryptoTimes logs per-request encrypt/decrypt durations.
	TrackCryptoTimes bool
	// DebugLogBodies dumps full plaintext request bodies to stderr —
	// violates the non-retention rule; local debugging only.
	DebugLogBodies bool
	// Keystore, when non-nil, is the loaded signing keystore Run uses. The
	// CLI builds it (OS-keychain wallet provider + env/cloud fallback) and
	// passes it in. When nil, Run falls back to keystore.LoadFromEnv — the
	// env-only path, unchanged from the original main().
	Keystore *keystore.KeyStore
	// SolanaKeys is Keystore's twin on the Solana rail, which Run reads in
	// its place there; nil falls back to keystore.LoadSolanaFromEnv.
	SolanaKeys *keystore.SolanaKeys
	// SpendGuard, when non-nil, bounds aggregate USDC drain before each escrow
	// open. The CLI builds it from the resolved spend caps. Nil disables it
	// (the env-only path keeps the protocol's per-request max_price as the only
	// ceiling).
	SpendGuard *spend.Guard
	// ShutdownToken, when non-empty, enables the loopback POST /shutdown control
	// channel and is the token it requires. The CLI forwards it from the
	// daemonizing parent (via the environment) on the backgrounded child only;
	// it's the Windows graceful-stop path (no SIGTERM). Empty for foreground /
	// service-managed runs ⇒ the route is not registered.
	ShutdownToken string
	// LogWriter, when non-nil, is where the process logger writes instead of
	// stderr. The Windows SCM service sets it to a logfile because an
	// SCM-launched process's stderr is discarded; everything else leaves it nil
	// (stderr — captured by the daemon's stdout redirect / launchd / systemd).
	LogWriter io.Writer

	// buildClient is zs.New outside tests, which stub it to run Run without
	// a chain.
	buildClient func(context.Context, zs.Config) (*zs.Client, error)
	// stateDir is paths.StateDir outside tests, which point it at a temp dir.
	stateDir func() (string, error)
}

// networkLabel names the chain the proxy is bound to for startup logging.
// The normal resolution path stamps a concrete name (mainnet|testnet|localnet),
// but a config that supplies a raw algod.endpoint with no network name is left
// fully manual (cli.applyNetwork), so report that as "custom" — and "unset"
// when neither is configured — rather than an empty string.
func networkLabel(a algod.Config) string {
	switch {
	case a.Network != "":
		return a.Network
	case a.ResolvedEndpoint != "":
		return "custom"
	default:
		return "unset"
	}
}

// warnOutputCeilingDivergence flags a fallback output ceiling that differs from
// the shared default, in EITHER direction.
//
// That value is the clamp on every omitting caller's per-operator reservation,
// and eligibility is evaluated against the DERIVED number — so any value other
// than the shared default makes this proxy disagree with the chat client about
// which operators can serve the same request, which is the divergence the single
// ceiling exists to remove. That argument is symmetric: at ctx 65536 a ceiling of
// 8192 and one of 65536 both pick a different operator set than the client's
// 16384, and neither direction is the safe one.
//
// The truncation argument is not symmetric, and only the low side gets it: the
// ceiling used to be floored at 32768 for reasoning models regardless of what was
// configured here — a thinking model spends budget on chain-of-thought BEFORE the
// visible answer, so a low ceiling truncates the answer to nothing (the
// blank-bubble failure). That floor is gone, there being one ceiling now, so a low
// value carried over from an older config silently reintroduces the truncation.
// The high side has its own cost instead: a larger reservation locks more USDC in
// escrow per request (refunded at settlement, but held meanwhile).
//
// A WARN and not a hard error either way: an operator may have a real reason to
// cap output spend or to serve unusually long single turns, and refusing to start
// would be a worse failure than saying so loudly. Zero is left alone — that is the
// deliberate fail-closed setting, which rejects omitting callers outright rather
// than sizing a reservation for them.
func warnOutputCeilingDivergence(cfg *config.Config, log *slog.Logger) {
	n := cfg.Zs.FallbackMaxOutputTokens
	if n == 0 || n == selection.DefaultMaxOutputCeiling {
		return
	}
	msg := "zs.fallback_max_output_tokens is ABOVE the shared default — this proxy will route differently from the chat client for the same request, and each omitting caller locks more USDC in escrow than it needs"
	if n < selection.DefaultMaxOutputCeiling {
		msg = "zs.fallback_max_output_tokens is BELOW the shared default — reasoning models may return truncated or empty answers, and this proxy will route differently from the chat client for the same request"
	}
	log.Warn(msg,
		slog.Uint64("configured", n),
		slog.Uint64("shared_default", selection.DefaultMaxOutputCeiling),
	)
}

// Run builds the proxy server from cfg and serves until ctx is canceled
// (e.g. a signal the caller wired into the context) or ListenAndServe
// fails. It returns nil on a clean, graceful shutdown and a wrapped error
// otherwise. Run does not call os.Exit; the caller decides the exit code.
func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	log := newLoggerTo(cfg.Logging, opts.LogWriter)
	// Make the configured logger the process default so EVERY line the proxy
	// emits carries the operator's level and format — and, on Windows, lands in
	// opts.LogWriter (proxy.log) rather than an SCM process's discarded stderr.
	// Without this, anything reaching slog.Default() — the nil-logger fallbacks
	// in the zs package, and the standard log package used by dependencies —
	// goes to slog's built-in handler as unstructured stderr text with no
	// severity field, which collectors bucket as ERROR regardless of level.
	// Safe here because Run is the process entry point in practice (one caller,
	// internal/cli), not a library helper; it is global state, so nothing under
	// internal/ may call it.
	slog.SetDefault(log)

	if opts.DebugLogBodies {
		log.Warn("DEBUG body logging enabled: full plaintext request AND response bodies (streaming frames included) will be written to stderr — never enable in production (breaks the non-retention guarantee)")
	}

	// Announce the target chain up front — the single most consequential startup
	// fact (mainnet vs testnet vs localnet decides whose real USDC moves), and
	// the easiest to get wrong across the config/env/--network precedence chain.
	// The resolved algod endpoint + escrow app id make it verifiable at a glance
	// rather than trusting the network name alone.
	dep, warnings, err := zs.ResolveNetwork(NetworkFor(cfg))
	if err != nil {
		return fmt.Errorf("resolve the network: %w", err)
	}
	if cfg.OnSolana() {
		// Scheme and host only: a provider's key may ride in the path or query.
		log.Info("connecting to Solana",
			slog.String("network", cfg.NetworkName()),
			slog.String("rpc_endpoint", endpointHost(dep.Solana.RPCEndpoint)),
			slog.String("program", dep.Solana.Program.String()),
		)
		for _, w := range warnings {
			log.Warn(w)
		}
	} else {
		log.Info("connecting to Algorand network",
			slog.String("network", networkLabel(cfg.Algod)),
			slog.String("algod_endpoint", cfg.Algod.ResolvedEndpoint),
			slog.Uint64("escrow_app_id", cfg.Zs.EscrowAppID),
		)
	}

	// Warn when algod.endpoint silently overrides an algod.network preset —
	// easy config footgun (declared network = mainnet but endpoint left
	// pointing at testnet-legacy means on-chain traffic goes to the wrong
	// chain and the operator won't know until a transaction lands).
	if cfg.Algod.EndpointOverrides() {
		log.Warn("algod.endpoint overrides algod.network preset",
			slog.String("network", cfg.Algod.Network),
			slog.String("endpoint", cfg.Algod.Endpoint),
			slog.String("resolved_endpoint", cfg.Algod.ResolvedEndpoint),
		)
	}

	warnOutputCeilingDivergence(cfg, log)

	// runCtx is the master shutdown context: either a signal (via the caller's
	// ctx) or the loopback POST /shutdown control channel (cancelRun, the Windows
	// graceful-stop path) cancels it, and the serve loop below drains in
	// response.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	keys, err := loadPayerKeys(ctx, cfg, opts)
	if err != nil {
		return err
	}

	if err := resolveProxyPayerAddr(&cfg.Zs, keys, log); err != nil {
		return fmt.Errorf("proxy payer address resolution failed: %w", err)
	}
	warnEnvProxy(log, *cmp.Or(cfg.Zs.AllowPrivateOperators, new(dep.Local())), http.ProxyFromEnvironment)

	build := opts.buildClient
	if build == nil {
		build = zs.New
	}
	stateDir := opts.stateDir
	if stateDir == nil {
		stateDir = paths.StateDir
	}
	// dep is the same resolution zs.New makes from clientConfigFor's Network,
	// so the files are named for the deployment the client runs on.
	journal := openTicketJournal(stateDir, dep, log)
	catalog, catalogLegacy := resolveCatalogPath(stateDir, cfg.Zs.CatalogPath, dep, log)
	client, err := newClient(ctx, clientConfigFor(cfg, keys, catalog, catalogLegacy, journal, opts, log), build)
	if err != nil || client == nil {
		closeJournal(journal)
		return err
	}
	defer closeClientThenJournal(client, journal)

	handler, srv := serveClient(client, cfg, opts, cancelRun, log)

	log.Info("proxy starting",
		slog.String("listen", cfg.Server.Listen),
		slog.Int("keystore_addresses", len(keys.addresses())),
	)
	// Announce a spend cap only when one is actually configured — caps are
	// optional, so a quiet startup means "no cap" (the guard still tracks the
	// day's spend for `zs status`).
	if cfg.Spend.DailyCapUSDC > 0 || cfg.Spend.PerRequestCapUSDC > 0 {
		log.Info("spend cap active",
			slog.Float64("daily_cap_usdc", cfg.Spend.DailyCapUSDC),
			slog.Float64("per_request_cap_usdc", cfg.Spend.PerRequestCapUSDC),
		)
	}
	if cfg.Zs.TEE.AllowStub {
		log.Info("zs.tee.allow_stub is true — accepting stub-mode TEE evidence; DO NOT use in production",
			slog.String("verifier", cfg.Zs.TEE.Verifier))
	}
	if len(cfg.Zs.TEE.RequireForModels) > 0 {
		log.Info("zs.tee.require_for_models active — these models route only to attested operators",
			slog.Any("models", cfg.Zs.TEE.RequireForModels))
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-runCtx.Done():
		log.Info("shutdown signal received")
	case err, ok := <-errCh:
		if ok && err != nil {
			return fmt.Errorf("server exited with error: %w", err)
		}
		return nil
	}

	// Shutdown must outlive the (now-canceled) run context, so it uses a
	// fresh background deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	// Non-streaming settles are submitted after their response is written,
	// so they can still be in flight once every connection has closed. Drain
	// them even when Shutdown timed out, so the abandoned tickets get named.
	if err := handler.WaitSettles(shutdownCtx); err != nil {
		log.Warn("shutdown: escrow settle submits abandoned. An unfinished ack is finalized by the node's settleLapsed; "+
			"an unfinished protest, or a ticket that needs a refund, needs `zs-proxy tickets` to inspect and recover it",
			slog.Any("err", err))
	}
	if shutdownErr != nil {
		return fmt.Errorf("graceful shutdown failed: %w", shutdownErr)
	}
	return nil
}

// buildTokenizer translates ZsConfig into a tokenize.Registry. The
// registry's default tokenizer is the model-agnostic SafeEstimator;
// the only per-model wiring needed at startup is the max_output
// defaults (per-model overrides plus the global fallback) used when the
// request body omits one.
func buildTokenizer(cfg config.ZsConfig) *tokenize.Registry {
	return tokenize.BuildRegistry(cfg.DefaultMaxOutputTokens, cfg.FallbackMaxOutputTokens)
}

// buildTEEVerifier constructs the configured TEEVerifier, or returns
// nil when no verifier is implemented for the configured kind. Nil is
// a refusal, and only reaches the SDK as one alongside
// Config.DisableTEE (clientConfigFor): a bare nil there means the SDK's
// default verifier. "builtin" (and the empty
// string, and the legacy "nras" alias config normalizes away) wires
// BundleVerifier; an out-of-process or alternative-vendor verifier
// would land behind the same constructor.
//
// The default arm stays reachable even though config validation
// rejects an unknown kind first: buildTEEVerifier is also called with
// a hand-built TEEConfig by embedders of app.Run, which never passes
// through validate.
func buildTEEVerifier(cfg config.TEEConfig, log *slog.Logger) tee.Verifier {
	switch cfg.Verifier {
	case "", config.TEEVerifierBuiltin, config.TEEVerifierLegacyNRAS:
		// Not an error: a proxy with no allowlist is a proxy that verifies no
		// dstack evidence, which is the correct fail-closed outcome. Said out
		// loud because the symptom — every dstack operator failing
		// measurement_mismatch — is otherwise indistinguishable from a fleet
		// of bad nodes.
		//
		// NAMED SEPARATELY because they no longer fail for the same reason.
		// trusted_os_images is seeded from config's defaults, so empty means
		// the operator explicitly cleared it; trusted_compose_hashes has no
		// default and empty is simply the unconfigured state. One message
		// covering both told an operator who had done nothing wrong to go
		// looking for a setting they had never touched.
		switch {
		case len(cfg.TrustedOSImages) == 0:
			log.Info("zs.tee: trusted_os_images is empty, so dstack-tdx evidence cannot verify; " +
				"it ships with the production dstack images preset, so an empty list means it was " +
				"cleared — or that this TEEConfig was built without config.defaults()")
		// With the release match on, the skeleton lists admit a compose
		// without any pinned hash, so an empty pin list is the default
		// working state rather than a gap.
		case len(cfg.TrustedComposeHashes) == 0 && !cfg.EnforceComposeSkeleton:
			log.Info("zs.tee: trusted_compose_hashes is empty and enforce_compose_skeleton is off, so " +
				"dstack-tdx evidence cannot verify; turn enforce_compose_skeleton on. A pinned " +
				"compose-hash would verify the evidence, but with the release match off no node " +
				"counts as attested, so TEE-required requests still find no node")
		}
		return &tee.BundleVerifier{
			AllowStub:            cfg.AllowStub,
			TrustedComposeHashes: cfg.TrustedComposeHashes,
			TrustedOSImages:      cfg.TrustedOSImages,
			ComposePolicy:        ComposePolicyFromTEEConfig(cfg, log),
			Log:                  log,
		}
	default:
		log.Warn("zs.tee.verifier is not recognized — TEE verification disabled",
			slog.String("verifier", cfg.Verifier))
		return nil
	}
}

// Remedies names the CLI commands and config keys that fix what the SDK
// reports, in place of its adapter-neutral wording. Every backticked command
// must be one the CLI registers; internal/cli's tests check that.
//
// The spend guard is built once at startup, so a raised cap needs a restart.
// The recovery log carries the ticket id as ticket_id, which both commands
// take.
var Remedies = zs.Remedies{
	Fund:          "send ALGO and USDC to the payer address (`zs-proxy wallet address`), then run `zs-proxy fund`",
	RaiseSlots:    "raise zs.concurrent_slots with `zs-proxy slots <n>`, which also funds the pool",
	RaiseSpendCap: "raise spend.daily_cap_usdc or spend.per_request_cap_usdc in the config (`zs-proxy config path`) and run `zs-proxy proxy restart`; the daily cap also resets at 00:00 UTC",
	RecoverTicket: "inspect it with `zs-proxy tickets get <ticket_id>`, and finish it with `zs-proxy tickets recover <ticket_id>`",
}

// clientConfigFor maps the proxy config onto the SDK's.
//
// Nearly every field here is a silent no-op when mis-wired: the daemon starts,
// the suite stays green, and the knob simply does nothing. The Run tests stub
// the client builder, so they never see this Config; client_config_test.go is
// what pins it.
//
// The proxy keeps HTTP_PROXY working, which the SDK's default ignores; Run
// warns when one is set (warnEnvProxy).
func clientConfigFor(cfg *config.Config, keys payerKeys, catalogPath, catalogLegacyPath string, journal zs.TicketJournal, opts Options, log *slog.Logger) zs.Config {
	c := DoctorConfig(cfg, log)
	// The SDK refuses the other rail's signer, so only one is set.
	if keys.solana != nil {
		c.SolanaSigner = keys.solana.Signer(cfg.Zs.ProxyPayerAddr)
	} else {
		c.Signer = keys.algorand.Signer(cfg.Zs.ProxyPayerAddr)
	}
	c.Discovery.CatalogPath = catalogPath
	c.Discovery.CatalogLegacyPath = catalogLegacyPath
	c.DebugLogBodies = opts.DebugLogBodies
	c.Stores.Journal = journal
	// A typed-nil *spend.Guard would satisfy the SDK's non-nil check, so only
	// set it when a guard was actually built.
	if opts.SpendGuard != nil {
		c.SpendGuard = opts.SpendGuard
		if err := opts.SpendGuard.ReservationsNotKept(); err != nil {
			log.Warn("spend: open reservations are not kept across a restart, so one cut short by a restart frees its headroom early", slog.Any("reason", err))
		}
	}
	return c
}

// NetworkFor is the SDK network the proxy runs on. Run names its state files
// from it, and the spend tally is named beside them, so it must be the one the
// client gets.
func NetworkFor(cfg *config.Config) zs.Network {
	if cfg.OnSolana() {
		return cfg.SolanaNetwork()
	}
	return zs.Network{Algod: cfg.Algod, EscrowAppID: cfg.Zs.EscrowAppID}
}

// SolanaRemedies is Remedies on the Solana rail, where a payer funds with
// SOL, for fees and the ticket rent pool, rather than ALGO.
var SolanaRemedies = func() zs.Remedies {
	r := Remedies
	r.Fund = "send SOL and USDC to the payer address (`zs-proxy wallet address`), then run `zs-proxy fund`"
	return r
}()

// RemediesFor is the remedies for the rail cfg pays on.
func RemediesFor(cfg *config.Config) zs.Remedies {
	if cfg.OnSolana() {
		return SolanaRemedies
	}
	return Remedies
}

// payerKeys is the signing keys of the rail the config pays on; only that
// rail's is set.
type payerKeys struct {
	algorand *keystore.KeyStore
	solana   *keystore.SolanaKeys
}

func (k payerKeys) addresses() []string {
	if k.solana != nil {
		return k.solana.Addresses()
	}
	return k.algorand.Addresses()
}

// choose is the SDK's payer choice for the keys' rail.
func (k payerKeys) choose(want string) (string, error) {
	if k.solana != nil {
		return wallet.ChooseSolanaPayer(want, k.solana)
	}
	return wallet.ChoosePayer(want, k.algorand)
}

// loadPayerKeys is opts' keys for cfg's rail, else that rail's from the
// environment.
func loadPayerKeys(ctx context.Context, cfg *config.Config, opts Options) (payerKeys, error) {
	if cfg.OnSolana() {
		sk := opts.SolanaKeys
		if sk == nil {
			var err error
			if sk, err = keystore.LoadSolanaFromEnv(ctx); err != nil {
				return payerKeys{}, fmt.Errorf("keystore load failed: %w", err)
			}
		}
		return payerKeys{solana: sk}, nil
	}
	ks := opts.Keystore
	if ks == nil {
		var err error
		if ks, err = keystore.LoadFromEnv(ctx); err != nil {
			return payerKeys{}, fmt.Errorf("keystore load failed: %w", err)
		}
	}
	return payerKeys{algorand: ks}, nil
}

// endpointHost is u's scheme and host, for a log line.
func endpointHost(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "(unparsable)"
	}
	return p.Scheme + "://" + p.Host
}

// DoctorConfig is the part of clientConfigFor that needs no wallet, journal or
// spend guard, which is all zs.Diagnose reads. Sharing it keeps the doctor
// judging the network, privacy and slot count the daemon would run with.
func DoctorConfig(cfg *config.Config, log *slog.Logger) zs.Config {
	h := cfg.Zs
	// A nil verifier is buildTEEVerifier's refusal; passed on bare, the SDK
	// would read it as "use the default".
	verifier := buildTEEVerifier(h.TEE, log)
	return zs.Config{
		Network:              NetworkFor(cfg),
		Privacy:              privacyMode(h.Privacy),
		AllowPrivacyOverride: h.AllowPrivacyOverride,
		RequireTEEModels:     h.TEE.RequireForModels,
		TEEVerifier:          verifier,
		DisableTEE:           verifier == nil,
		Affinity:             zs.Affinity{Policy: h.ResolvedAffinity, TTL: h.AffinityTTL, Max: h.AffinityMax},
		ConcurrentSlots:      int(h.ConcurrentSlots),
		Discovery: zs.Discovery{
			RefreshInterval:    h.RefreshInterval,
			RefreshTimeout:     h.RefreshTimeout,
			ProbeBudget:        h.ProbeBudget,
			TargetTopK:         h.TargetTopK,
			MaxCacheAge:        h.MaxCacheAge,
			MaxCarryAge:        h.MaxCarryAge,
			LatencyStateMax:    h.LatencyStateMax,
			DetailsTimeout:     h.DetailsTimeout,
			DetailsConcurrency: h.DetailsConcurrency,
			OperatorIDs:        h.OperatorIDs,
		},
		ReserveTimeout:    h.ReserveTimeout,
		AllowStaging:      h.AllowStaging,
		AllowPrivateHosts: h.AllowPrivateOperators,
		UseEnvProxy:       true,
		Tokenizer:         buildTokenizer(h),
		Logger:            log,
		Remedies:          RemediesFor(cfg),
	}
}

// warnEnvProxy names an environment proxy that carries node traffic. The proxy
// dials the nodes, so the private-address guard checks only the proxy's own
// address: a node that registers a LAN address reaches the payer's network
// through it. And a proxy on a private address is itself refused, failing
// every node request, unless the guard is off.
//
// Only the proxy's scheme and host are logged: a proxy URL often carries an
// API key as its username, which url.Redacted leaves in place, and this line
// lands in a persistent daemon log.
//
// proxyFor is http.ProxyFromEnvironment outside tests: that reads the
// environment once per process, so a test cannot set it.
func warnEnvProxy(log *slog.Logger, allowPrivate bool, proxyFor func(*http.Request) (*url.URL, error)) {
	seen := map[string]bool{}
	for _, scheme := range []string{"https", "http"} {
		u, err := proxyFor(&http.Request{URL: &url.URL{Scheme: scheme, Host: "node.example"}})
		if err != nil || u == nil {
			continue
		}
		proxy := u.Scheme + "://" + u.Host
		if seen[proxy] {
			continue
		}
		seen[proxy] = true
		if ip := net.ParseIP(u.Hostname()); !allowPrivate && (u.Hostname() == "localhost" || ip != nil && httpx.IsPrivateIP(ip)) {
			log.Error("the environment proxy is on a private address, which the private-address guard refuses: every node request will fail. "+
				"Unset the proxy for zs-proxy, or set zs.allow_private_operators (which also lets nodes on private addresses be dialed)",
				slog.String("for", scheme), slog.String("proxy", proxy))
			continue
		}
		log.Warn("an environment proxy carries node traffic; the private-address guard checks only the proxy's own address, not the nodes it reaches",
			slog.String("for", scheme), slog.String("proxy", proxy))
	}
}

// privacyMode spells out both directions of a set privacy, and leaves an
// unset one to the SDK, which defaults it from the network.
func privacyMode(relay *bool) zs.Privacy {
	switch {
	case relay == nil:
		return ""
	case *relay:
		return zs.PrivacyRelayRequired
	}
	return zs.PrivacyDirect
}

// newClient is zs.New (build, outside tests) as Run uses it. A signal during
// New's chain reads is a stop, not a failed start, so it returns neither a
// client nor an error.
func newClient(ctx context.Context, zc zs.Config, build func(context.Context, zs.Config) (*zs.Client, error)) (*zs.Client, error) {
	c, err := build(ctx, zc)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("zs client init failed: %w", err)
	}
	return c, nil
}

// serveClient is the handler over client and the server Run serves it with:
// without the token, Windows `proxy stop` gets a 404 and no graceful drain.
func serveClient(client *zs.Client, cfg *config.Config, opts Options, shutdown func(), log *slog.Logger) (*server.Server, *http.Server) {
	hc, ho := handlerArgs(opts)
	h := server.New(client, hc, log, transform.Passthrough{}, transform.Passthrough{}, ho)
	return h, httpServerFor(h, cfg, opts.ShutdownToken, shutdown, log)
}

// handlerArgs is what the proxy hands the SDK handler. The handler keeps both
// private, so this is where a test reads them: without the URL a mainnet 402
// stops naming the web app, and without the switch -times does nothing.
func handlerArgs(opts Options) (*server.Config, server.Options) {
	return &server.Config{FundingURL: build.ClientURL}, server.Options{TrackCryptoTimes: opts.TrackCryptoTimes}
}

// httpServer is what httpServerFor needs of the SDK handler, so a test can
// stand in for one without a Client.
type httpServer interface {
	HTTPServer(server.ListenConfig) *http.Server
}

// httpServerFor is the served server: the handler's, with the Windows
// graceful-stop route in front of it when the daemon supplied a token.
// Extracted from Run so a test reaches it; without the route, `proxy stop` on
// Windows gets a 404 and no graceful drain.
func httpServerFor(h httpServer, cfg *config.Config, shutdownToken string, shutdown func(), log *slog.Logger) *http.Server {
	srv := h.HTTPServer(listenConfigFor(cfg))
	srv.Handler = withShutdownRoute(srv.Handler, shutdownToken, shutdown, log)
	return srv
}

// listenConfigFor is a function rather than a literal in Run so a test can
// reach it: swapping ReadTimeout into WriteTimeout would cut every SSE stream
// at the read deadline.
func listenConfigFor(cfg *config.Config) server.ListenConfig {
	return server.ListenConfig{
		Addr:         cfg.Server.Listen,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}
}

// resolveCatalogPath resolves the operator-catalog persistence path:
// "off" disables persistence, empty defaults to
// StateDir/operator-catalog-<deployment key>.json, and any other value is used
// verbatim. A state-dir resolution error disables persistence (non-fatal — a
// warm start is a convenience, not a requirement).
//
// The default is per deployment for the journal's reason: the state dir is
// shared across networks, and a catalog saved on one would seed a boot on
// another with nodes whose ids repeat there but whose URLs and keys don't.
// legacy is the name the default had before it carried the deployment key
// (operator-catalog-<app id>.json), which the SDK adopts when only it exists.
func resolveCatalogPath(stateDir func() (string, error), configured string, d zs.Deployment, log *slog.Logger) (path, legacy string) {
	switch configured {
	case "off":
		return "", ""
	case "":
		dir, err := stateDir()
		if err != nil {
			log.Warn("catalog persistence disabled: cannot resolve state dir", slog.Any("err", err))
			return "", ""
		}
		return zs.StateFile(dir, "operator-catalog", d)
	default:
		return configured, ""
	}
}

// openTicketJournal is the file the SDK records open tickets in, so a ticket
// open when the proxy stops is still settled or refunded after it restarts.
// The state dir is shared by every network, so the file is named for the
// deployment, and a testnet and a mainnet proxy each hold their own. Two
// proxies on one network cannot share it: the SDK locks the file, and the
// second keeps its tickets in memory.
//
// The journal under its earlier name (ticket-journal-<app id>.json) is merged
// in and removed. One the open could not merge, because an older proxy still
// holds it or it will not read, is left in place and merged on a later start.
//
// On failure it returns nil and the SDK journals in memory. An unreadable file
// is left in place for `zs-proxy tickets` rather than overwritten.
func openTicketJournal(stateDir func() (string, error), d zs.Deployment, log *slog.Logger) zs.TicketJournal {
	dir, err := stateDir()
	if err != nil {
		log.Warn("ticket journal kept in memory: cannot resolve the state dir", slog.Any("err", err))
		return nil
	}
	path, legacy := zs.StateFile(dir, "ticket-journal", d)
	var legacies []string
	if legacy != "" {
		legacies = append(legacies, legacy)
	}
	j, err := zs.OpenFileJournal(path, legacies...)
	if errors.Is(err, zs.ErrJournalInUse) {
		log.Warn("ticket journal kept in memory: another zs-proxy on this network holds the journal file; tickets this one opens are not recovered after a restart",
			slog.String("path", path))
		return nil
	}
	if err != nil {
		log.Warn("ticket journal kept in memory: cannot read the journal file. Tickets it lists need `zs-proxy tickets`; move the file aside to journal to disk again",
			slog.String("path", path), slog.Any("err", err))
		return nil
	}
	for _, s := range j.MergeSkipped() {
		log.Warn("ticket journal under its earlier name is not merged yet; the next start tries again. Tickets it lists are still on chain for `zs-proxy tickets`",
			slog.String("path", s.Path), slog.Any("err", s.Err))
	}
	return j
}

// closeClientThenJournal closes in that order because the client's
// reconciler writes the journal until Close returns, and a closed FileJournal
// refuses the write, leaving a stale row for the next start.
func closeClientThenJournal(client interface{ Close() }, journal zs.TicketJournal) {
	client.Close()
	closeJournal(journal)
}

func closeJournal(journal zs.TicketJournal) {
	if c, ok := journal.(io.Closer); ok {
		_ = c.Close()
	}
}

// resolveProxyPayerAddr checks cfg.ProxyPayerAddr against the loaded keys,
// filling it in from the lone key when the config omits it, so a
// single-mnemonic deployment need not repeat the address in YAML. With none
// or several loaded it fails rather than guess.
func resolveProxyPayerAddr(cfg *config.ZsConfig, keys payerKeys, log *slog.Logger) error {
	addr, err := keys.choose(cfg.ProxyPayerAddr)
	if err != nil {
		return fmt.Errorf("zs.proxy_payer_addr: %w", err)
	}
	if cfg.ProxyPayerAddr == "" {
		cfg.ProxyPayerAddr = addr
		log.Info("proxy_payer_addr auto-derived from single keystore entry",
			slog.String("address", cfg.ProxyPayerAddr))
	}
	return nil
}
