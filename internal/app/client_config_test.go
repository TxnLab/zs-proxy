/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/solana"
	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"
	server "github.com/TxnLab/zerosignal/go/zs/handler"
	"github.com/TxnLab/zerosignal/go/zs/tee"
	"github.com/TxnLab/zerosignal/go/zs/tokenize"

	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/spend"
)

func algorandKeys() payerKeys { return payerKeys{algorand: &keystore.KeyStore{}} }

// A Solana config gives the SDK the Solana network, the Solana signer alone
// (it refuses a config carrying both), and a funding remedy in SOL.
func TestClientConfigFor_Solana(t *testing.T) {
	cfg := &config.Config{Solana: config.SolanaConfig{Cluster: "localnet", RPCEndpoint: "http://127.0.0.1:8899"}}
	program := solana.PublicKey{7}
	cfg.Solana.ResolvedProgram = program
	cfg.Zs.ProxyPayerAddr = "SOLPAYER"
	got := clientConfigFor(cfg, payerKeys{solana: &keystore.SolanaKeys{}}, "", "", nil, Options{}, quietLogger())
	if got.Signer != nil {
		t.Errorf("Signer = %v, want none on Solana", got.Signer)
	}
	if got.SolanaSigner == nil || got.SolanaSigner.Address() != "SOLPAYER" {
		t.Errorf("SolanaSigner = %v, want the payer's", got.SolanaSigner)
	}
	want := zs.Network{Rail: ticket.RailSolana, Solana: zs.SolanaNetwork{Cluster: "localnet", RPCEndpoint: "http://127.0.0.1:8899", Program: program}}
	if got.Network != want {
		t.Errorf("Network = %+v, want %+v", got.Network, want)
	}
	if !strings.Contains(got.Remedies.Fund, "SOL") || strings.Contains(got.Remedies.Fund, "ALGO") {
		t.Errorf("Remedies.Fund = %q, want SOL", got.Remedies.Fund)
	}
	if algo := clientConfigFor(&config.Config{}, algorandKeys(), "", "", nil, Options{}, quietLogger()); algo.SolanaSigner != nil || algo.Remedies != Remedies {
		t.Errorf("Algorand: SolanaSigner %v, Remedies %+v", algo.SolanaSigner, algo.Remedies)
	}
}

// TestClientConfigFor_ThreadsEveryField pins the config→SDK mapping, which
// nothing else in the tree exercises: Run needs a keystore, an algod and a bound
// port. A field dropped here is silent — the daemon starts, the suite stays
// green, and the knob does nothing.
//
// Every value is distinct from every other and from the zero value, so a
// copy-paste that threads one field into two slots fails.
func TestClientConfigFor_ThreadsEveryField(t *testing.T) {
	cfg := &config.Config{}
	cfg.Algod = algod.Config{Network: "testnet", Endpoint: "http://algod:4001"}
	h := &cfg.Zs
	h.ProxyPayerAddr = "PAYER"
	h.EscrowAppID = 77
	h.Privacy = new(true)
	h.AllowPrivacyOverride = true
	h.TEE.RequireForModels = []string{"m1", "m2"}
	h.TEE.TrustedComposeHashes = []string{"ab01"}
	h.ResolvedAffinity = zs.AffinityStrict
	h.AffinityTTL = 3 * time.Minute
	h.AffinityMax = 5
	h.ConcurrentSlots = 6
	h.RefreshInterval = 11 * time.Second
	h.RefreshTimeout = 12 * time.Second
	h.MaxCacheAge = 13 * time.Second
	h.MaxCarryAge = 14 * time.Second
	h.ProbeBudget = 15
	h.TargetTopK = 16
	h.LatencyStateMax = 17
	h.DetailsTimeout = 18 * time.Second
	h.DetailsConcurrency = 19
	h.OperatorIDs = []uint64{20, 21}
	h.ReserveTimeout = 22 * time.Second
	h.AllowStaging = true
	h.AllowPrivateOperators = new(true)

	opts := Options{DebugLogBodies: true, SpendGuard: &spend.Guard{}}
	journal := &zs.FileJournal{}
	got := clientConfigFor(cfg, algorandKeys(), "/tmp/catalog.json", "/tmp/catalog-old.json", journal, opts, quietLogger())

	if got.Signer == nil || got.Signer.Address() != "PAYER" {
		t.Errorf("Signer = %v, want the payer's", got.Signer)
	}
	if got.Network != (zs.Network{Algod: cfg.Algod, EscrowAppID: 77}) {
		t.Errorf("Network = %+v", got.Network)
	}
	if got.Privacy != zs.PrivacyRelayRequired || !got.AllowPrivacyOverride {
		t.Errorf("Privacy %q, AllowPrivacyOverride %v", got.Privacy, got.AllowPrivacyOverride)
	}
	if !slices.Equal(got.RequireTEEModels, []string{"m1", "m2"}) || got.TEEVerifier == nil || got.DisableTEE {
		t.Errorf("RequireTEEModels %v, TEEVerifier %v, DisableTEE %v",
			got.RequireTEEModels, got.TEEVerifier, got.DisableTEE)
	}
	// The proxy's own verifier, not the SDK default, which would carry the
	// same release lists and so match on everything but this pin.
	if bv, ok := got.TEEVerifier.(*tee.BundleVerifier); !ok || !slices.Equal(bv.TrustedComposeHashes, []string{"ab01"}) {
		t.Errorf("TEEVerifier = %#v, want the one built from zs.tee", got.TEEVerifier)
	}
	// An unrecognized verifier disables TEE outright; a bare nil would get
	// the SDK's default verifier instead.
	cfg.Zs.TEE.Verifier = "bogus"
	if off := clientConfigFor(cfg, algorandKeys(), "", "", nil, opts, quietLogger()); off.TEEVerifier != nil || !off.DisableTEE {
		t.Errorf("unknown verifier: TEEVerifier %v, DisableTEE %v", off.TEEVerifier, off.DisableTEE)
	}
	if got.Affinity != (zs.Affinity{Policy: zs.AffinityStrict, TTL: 3 * time.Minute, Max: 5}) {
		t.Errorf("Affinity = %+v", got.Affinity)
	}
	if got.ConcurrentSlots != 6 || got.ReserveTimeout != 22*time.Second {
		t.Errorf("ConcurrentSlots %d, ReserveTimeout %v", got.ConcurrentSlots, got.ReserveTimeout)
	}
	wantD := zs.Discovery{
		RefreshInterval:    11 * time.Second,
		RefreshTimeout:     12 * time.Second,
		MaxCacheAge:        13 * time.Second,
		MaxCarryAge:        14 * time.Second,
		ProbeBudget:        15,
		TargetTopK:         16,
		LatencyStateMax:    17,
		DetailsTimeout:     18 * time.Second,
		DetailsConcurrency: 19,
		OperatorIDs:        []uint64{20, 21},
		CatalogPath:        "/tmp/catalog.json",
		CatalogLegacyPath:  "/tmp/catalog-old.json",
	}
	if !reflect.DeepEqual(got.Discovery, wantD) {
		t.Errorf("Discovery =\n  %+v\nwant\n  %+v", got.Discovery, wantD)
	}
	// A Discovery field added in the SDK fails here until it is mapped.
	v := reflect.ValueOf(wantD)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("Discovery.%s is not covered by this test", v.Type().Field(i).Name)
		}
	}
	if !got.AllowStaging || got.AllowPrivateHosts == nil || !*got.AllowPrivateHosts || !got.UseEnvProxy || !got.DebugLogBodies {
		t.Errorf("AllowStaging %v, AllowPrivateHosts %v, UseEnvProxy %v, DebugLogBodies %v",
			got.AllowStaging, got.AllowPrivateHosts, got.UseEnvProxy, got.DebugLogBodies)
	}
	if g, ok := got.SpendGuard.(*spend.Guard); !ok || g != opts.SpendGuard {
		t.Errorf("SpendGuard = %#v, want the proxy's guard", got.SpendGuard)
	}
	if got.Stores.Journal != journal {
		t.Errorf("Stores.Journal = %#v, want the proxy's journal", got.Stores.Journal)
	}
	if got.Logger == nil {
		t.Error("Logger dropped")
	}
	if got.Remedies != Remedies {
		t.Errorf("Remedies = %+v, want the proxy's", got.Remedies)
	}
	// A Remedies field added in the SDK would fall back to its generic wording
	// until the proxy names its own command for it.
	r := reflect.ValueOf(Remedies)
	for i := range r.NumField() {
		if r.Field(i).IsZero() {
			t.Errorf("Remedies.%s is unset", r.Type().Field(i).Name)
		}
	}
}

// The tokenizer carries both output-reservation settings: a dropped fallback
// fails closed and refuses every request that omits max_tokens.
func TestClientConfigFor_Tokenizer(t *testing.T) {
	cfg := &config.Config{}
	cfg.Zs.DefaultMaxOutputTokens = map[string]uint64{"m": 1234}
	cfg.Zs.FallbackMaxOutputTokens = 4321
	tok := clientConfigFor(cfg, algorandKeys(), "", "", nil, Options{}, quietLogger()).Tokenizer
	for model, want := range map[string]uint64{"m": 1234, "other": 4321} {
		est, err := tokenize.EstimateWithRegistry(tok, tokenize.Request{Model: model, Body: []byte(`{}`)})
		if err != nil || est.MaxOutputTokens != want {
			t.Errorf("%s: max output %d (err %v), want %d", model, est.MaxOutputTokens, err, want)
		}
	}
}

// Left unset, the debug dump stays off — it writes plaintext prompts to the
// daemon log — and the environment proxy stays honored, as before the SDK.
func TestClientConfigFor_ZeroOptions(t *testing.T) {
	c := clientConfigFor(&config.Config{}, algorandKeys(), "", "", nil, Options{}, quietLogger())
	if c.DebugLogBodies {
		t.Error("DebugLogBodies on without -debug-bodies")
	}
	if !c.UseEnvProxy {
		t.Error("UseEnvProxy off: HTTP_PROXY would stop carrying node traffic")
	}
}

// The SDK's zero Privacy is the network's default, so the mapping must name
// both set directions and pass an unset one through: a privacy: false proxy
// left unset would relay on testnet, one that mapped true to anything but
// relay_required would go direct, and an unset one mapped to either would
// override the localnet default.
func TestClientConfigFor_PrivacyBothDirections(t *testing.T) {
	for _, tc := range []struct {
		relay *bool
		want  zs.Privacy
	}{
		{new(true), zs.PrivacyRelayRequired},
		{new(false), zs.PrivacyDirect},
		{nil, ""},
	} {
		cfg := &config.Config{}
		cfg.Zs.Privacy = tc.relay
		if got := clientConfigFor(cfg, algorandKeys(), "", "", nil, Options{}, quietLogger()).Privacy; got != tc.want {
			t.Errorf("privacy: %v → %q, want %q", tc.relay, got, tc.want)
		}
	}
}

// An unset allow_private_operators must reach the SDK unset, so the SDK keeps
// the dial guard on everywhere but a local chain; mapping it to false would
// block the localnet node, to true would drop the guard on testnet.
func TestClientConfigFor_AllowPrivateOperatorsPassesThrough(t *testing.T) {
	for _, set := range []*bool{nil, new(false), new(true)} {
		cfg := &config.Config{}
		cfg.Zs.AllowPrivateOperators = set
		got := clientConfigFor(cfg, algorandKeys(), "", "", nil, Options{}, quietLogger()).AllowPrivateHosts
		if (got == nil) != (set == nil) || got != nil && *got != *set {
			t.Errorf("allow_private_operators %v → %v", set, got)
		}
	}
}

// The all-true pass above cannot catch a swap between two bools. Two of these
// are security gates — reading AllowPrivateHosts from allow_staging drops the
// SSRF dial guard, and reading AllowPrivacyOverride from another knob lets a
// caller turn relaying off that the payer never allowed.
func TestClientConfigFor_BoolsAreOneHot(t *testing.T) {
	type bools struct{ override, staging, private, debug bool }
	for _, tc := range []struct {
		name string
		set  func(*config.Config, *Options)
		want bools
	}{
		{"allow_privacy_override", func(c *config.Config, _ *Options) { c.Zs.AllowPrivacyOverride = true }, bools{override: true}},
		{"allow_staging", func(c *config.Config, _ *Options) { c.Zs.AllowStaging = true }, bools{staging: true}},
		{"allow_private_operators", func(c *config.Config, _ *Options) { c.Zs.AllowPrivateOperators = new(true) }, bools{private: true}},
		{"-debug-bodies", func(_ *config.Config, o *Options) { o.DebugLogBodies = true }, bools{debug: true}},
	} {
		cfg, opts := &config.Config{}, Options{}
		tc.set(cfg, &opts)
		c := clientConfigFor(cfg, algorandKeys(), "", "", nil, opts, quietLogger())
		if got := (bools{c.AllowPrivacyOverride, c.AllowStaging, c.AllowPrivateHosts != nil && *c.AllowPrivateHosts, c.DebugLogBodies}); got != tc.want {
			t.Errorf("only %s set: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// No guard built means no cap: a typed-nil guard in the interface would make
// the SDK call through a nil *spend.Guard.
func TestClientConfigFor_NoSpendGuardStaysNil(t *testing.T) {
	if g := clientConfigFor(&config.Config{}, algorandKeys(), "", "", nil, Options{}, quietLogger()).SpendGuard; g != nil {
		t.Errorf("SpendGuard = %#v, want nil", g)
	}
}

// testnetApp is the testnet deployment with escrow app id app, as Run resolves
// it from a config.
func testnetApp(t *testing.T, app uint64) zs.Deployment {
	t.Helper()
	d, _, err := zs.ResolveNetwork(zs.Network{Algod: algod.Config{Network: "testnet"}, EscrowAppID: app})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The default catalog is per deployment, so a run on one network never seeds
// a boot on another, and it names the file it had before so the SDK can adopt
// it; "off" and an explicit path are left alone.
func TestResolveCatalogPath(t *testing.T) {
	dir := t.TempDir()
	stateDir := func() (string, error) { return dir, nil }
	app77, app78 := testnetApp(t, 77), testnetApp(t, 78)
	testnet, legacy := resolveCatalogPath(stateDir, "", app77, quietLogger())
	other, _ := resolveCatalogPath(stateDir, "", app78, quietLogger())
	if testnet != filepath.Join(dir, "operator-catalog-algorand-testnet-77.json") {
		t.Errorf("default path = %q", testnet)
	}
	if legacy != filepath.Join(dir, "operator-catalog-77.json") {
		t.Errorf("legacy path = %q, want the app-id name the default had before", legacy)
	}
	if testnet == other {
		t.Errorf("two escrow apps share %q", testnet)
	}
	if got, l := resolveCatalogPath(stateDir, "off", app77, quietLogger()); got != "" || l != "" {
		t.Errorf("off = %q, %q, want persistence disabled", got, l)
	}
	// An explicit path was never renamed, so there is nothing to adopt.
	if got, l := resolveCatalogPath(stateDir, "/x/catalog.json", app77, quietLogger()); got != "/x/catalog.json" || l != "" {
		t.Errorf("explicit path = %q, %q, want it verbatim with no legacy", got, l)
	}
	noHome := func() (string, error) { return "", errors.New("no home") }
	if got, _ := resolveCatalogPath(noHome, "", app77, quietLogger()); got != "" {
		t.Errorf("state dir error: path = %q, want persistence disabled", got)
	}
}

// The journal lives in the state dir under the deployment's key, and a record
// put through it is on disk for the next process to read.
func TestOpenTicketJournal_PerDeploymentFileInTheStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := func() (string, error) { return dir, nil }
	app77 := testnetApp(t, 77)
	j := openTicketJournal(stateDir, app77, quietLogger())
	if j == nil {
		t.Fatal("no journal")
	}
	rec := zs.TicketRecord{TicketID: "t1", EscrowAppID: 77, Payer: "PAYER", Status: zs.TicketOpen}
	if err := j.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := j.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := zs.OpenFileJournal(filepath.Join(dir, "ticket-journal-algorand-testnet-77.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, _ := reopened.List(context.Background(), "PAYER", app77); len(got) != 1 || got[0].TicketID != "t1" {
		t.Errorf("reopened journal lists %+v, want t1", got)
	}
	other := openTicketJournal(stateDir, testnetApp(t, 78), quietLogger())
	if other == nil {
		t.Fatal("no journal for app 78")
	}
	defer other.(io.Closer).Close()
	if got, _ := other.List(context.Background(), "PAYER", app77); len(got) != 0 {
		t.Errorf("another escrow app's journal lists %+v", got)
	}
}

// An upgraded proxy keeps the tickets an older one journaled under the app-id
// name: they are merged into the deployment's file and the old file is
// removed, so the reconciler still finishes them and nothing reads them twice.
func TestOpenTicketJournal_MergesTheAppIDJournal(t *testing.T) {
	dir := t.TempDir()
	old, err := zs.OpenFileJournal(filepath.Join(dir, "ticket-journal-77.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := zs.TicketRecord{TicketID: "t-old", EscrowAppID: 77, Payer: "PAYER", Status: zs.TicketOpen}
	if err := old.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	app77 := testnetApp(t, 77)
	j := openTicketJournal(func() (string, error) { return dir, nil }, app77, quietLogger())
	if j == nil {
		t.Fatal("no journal")
	}
	defer j.(io.Closer).Close()
	if got, _ := j.List(context.Background(), "PAYER", app77); len(got) != 1 || got[0].TicketID != "t-old" {
		t.Errorf("journal lists %+v, want the ticket journaled under the old name", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ticket-journal-77.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old journal is still there (err %v)", err)
	}
}

// A second proxy on the same network finds the journal held and keeps its
// tickets in memory, leaving the first one's file alone.
func TestOpenTicketJournal_HeldByAnotherProxy(t *testing.T) {
	dir := t.TempDir()
	stateDir := func() (string, error) { return dir, nil }
	first := openTicketJournal(stateDir, testnetApp(t, 77), quietLogger())
	if first == nil {
		t.Fatal("no journal")
	}
	defer first.(io.Closer).Close()
	var logs bytes.Buffer
	if second := openTicketJournal(stateDir, testnetApp(t, 77), slog.New(slog.NewTextHandler(&logs, nil))); second != nil {
		t.Errorf("second journal = %#v, want nil while the first holds the file", second)
	}
	// Telling it to move the file aside would point at the file a live proxy holds.
	if got := logs.String(); !strings.Contains(got, "another zs-proxy") || strings.Contains(got, "move the file aside") {
		t.Errorf("warning = %q, want it to name the other proxy", got)
	}
}

// Without a usable file the SDK journals in memory. The nil must be an
// untyped one, and an unreadable file must survive for `zs-proxy tickets`.
func TestOpenTicketJournal_FallsBackToMemory(t *testing.T) {
	app77 := testnetApp(t, 77)
	if j := openTicketJournal(func() (string, error) { return "", errors.New("no home") }, app77, quietLogger()); j != nil {
		t.Errorf("state dir error: journal = %#v, want nil", j)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ticket-journal-algorand-testnet-77.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if j := openTicketJournal(func() (string, error) { return dir, nil }, app77, quietLogger()); j != nil {
		t.Errorf("corrupt file: journal = %#v, want nil", j)
	}
	if b, _ := os.ReadFile(path); string(b) != "not json" {
		t.Errorf("the unreadable file was rewritten: %q", b)
	}
}

// TestListenConfigFor_ThreadsEveryField: Run is the only caller and no test runs
// it. Distinct durations so a swap fails.
func TestListenConfigFor_ThreadsEveryField(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:9999"
	cfg.Server.ReadTimeout = 11 * time.Second
	cfg.Server.WriteTimeout = 22 * time.Second
	cfg.Server.IdleTimeout = 33 * time.Second

	want := server.ListenConfig{
		Addr:         "127.0.0.1:9999",
		ReadTimeout:  11 * time.Second,
		WriteTimeout: 22 * time.Second,
		IdleTimeout:  33 * time.Second,
	}
	if got := listenConfigFor(cfg); got != want {
		t.Fatalf("listenConfigFor = %+v, want %+v", got, want)
	}
}

// A spend guard whose open reservations won't survive a restart is said so,
// with the reason, on the proxy's own logger; one that holds its file is not.
func TestClientConfigFor_WarnsWhenReservationsAreNotKept(t *testing.T) {
	cfg := &config.Config{}
	cfg.Zs.ProxyPayerAddr = "PAYER"
	for _, tc := range []struct {
		name  string
		guard *spend.Guard
		warn  bool
	}{
		{"no state dir", spend.New(spend.Config{}, ""), true},
		{"holds its file", spend.New(spend.Config{}, filepath.Join(t.TempDir(), "spend.json")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { tc.guard.Close() })
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			clientConfigFor(cfg, algorandKeys(), "", "", &zs.FileJournal{}, Options{SpendGuard: tc.guard}, log)
			got := strings.Contains(buf.String(), "not kept across a restart")
			if got != tc.warn {
				t.Errorf("warned %v, want %v: %q", got, tc.warn, buf.String())
			}
			if tc.warn && !strings.Contains(buf.String(), "no state directory") {
				t.Errorf("the warning gives no reason: %q", buf.String())
			}
		})
	}
}
