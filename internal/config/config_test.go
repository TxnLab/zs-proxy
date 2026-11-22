/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"
)

func baseValidConfig(t *testing.T) *Config {
	t.Helper()
	payerAddr, _, _, err := ticket.GenerateAlgorandKeypair()
	if err != nil {
		t.Fatalf("generate payer keypair: %v", err)
	}
	cfg := &Config{
		Server:  ServerConfig{Listen: "127.0.0.1:8080"},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
	cfg.Algod.ResolvedEndpoint = "http://localhost:4001"
	cfg.Zs.EscrowAppID = 1
	cfg.Zs.ProxyPayerAddr = payerAddr
	return cfg
}

// setValidEscrowEnv sets the minimal env vars required for zs validation
// to pass in Load-based tests. Must be called after clearProxyEnv.
func setValidEscrowEnv(t *testing.T) {
	t.Helper()
	payerAddr, _, _, err := ticket.GenerateAlgorandKeypair()
	if err != nil {
		t.Fatalf("generate payer keypair: %v", err)
	}
	t.Setenv("PROXY_ALGOD_NETWORK", "localnet")
	t.Setenv("PROXY_ZS_ESCROW_APP_ID", "1")
	t.Setenv("PROXY_ZS_PROXY_PAYER_ADDR", payerAddr)
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PROXY_SERVER_LISTEN",
		"PROXY_SERVER_READ_TIMEOUT",
		"PROXY_SERVER_WRITE_TIMEOUT",
		"PROXY_SERVER_IDLE_TIMEOUT",
		"PROXY_LOGGING_LEVEL",
		"PROXY_LOGGING_FORMAT",
		"PROXY_ALGOD_NETWORK",
		"PROXY_ALGOD_ENDPOINT",
		"PROXY_ALGOD_TOKEN",
		"PROXY_SOLANA_CLUSTER",
		"PROXY_SOLANA_RPC_ENDPOINT",
		"PROXY_SOLANA_PROGRAM",
		"PROXY_ZS_AFFINITY_POLICY",
		"PROXY_ZS_AFFINITY_TTL",
		"PROXY_ZS_AFFINITY_MAX",
		"PROXY_ZS_ESCROW_APP_ID",
		"PROXY_ZS_PROXY_PAYER_ADDR",
		"PROXY_ZS_PRIVACY",
		"PROXY_ZS_ALLOW_PRIVATE_OPERATORS",
		"PROXY_ZS_ALLOW_STAGING",
		"PROXY_ZS_TEE_VERIFIER",
		"PROXY_ZS_TEE_ALLOW_STUB",
		"PROXY_ZS_TEE_REQUIRE_FOR_MODELS",
		"PROXY_ZS_CONCURRENT_SLOTS",
		"PROXY_CONFIG",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoad_MissingFileTolerated(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)

	cfg, err := Load("/nonexistent/file/path.yaml")
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:9376" {
		t.Errorf("default Listen = %q", cfg.Server.Listen)
	}
}

func TestLoad_YAMLOverride(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := `
logging:
  level: debug
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q", cfg.Logging.Level)
	}
	// Unset field keeps its default.
	if cfg.Server.Listen != "127.0.0.1:9376" {
		t.Errorf("Listen default lost: %q", cfg.Server.Listen)
	}
}

// TestConcurrentSlots_LoadOrder pins the whole precedence chain for the
// concurrency knob: built-in default → YAML → env. The default matters on its
// own — zs.DefaultDepositSlots is a second, independent literal, and a drift
// between the two would change how much ALGO the pool targets.
func TestConcurrentSlots_LoadOrder(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")

	cfg, err := Load(path) // missing file → embedded default
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Zs.ConcurrentSlots != zs.DefaultDepositSlots {
		t.Errorf("default ConcurrentSlots = %d, want %d", cfg.Zs.ConcurrentSlots, zs.DefaultDepositSlots)
	}

	if err := os.WriteFile(path, []byte("zs:\n  concurrent_slots: 33\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(path); err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Zs.ConcurrentSlots != 33 {
		t.Errorf("YAML ConcurrentSlots = %d, want 33", cfg.Zs.ConcurrentSlots)
	}

	t.Setenv("PROXY_ZS_CONCURRENT_SLOTS", "7")
	if cfg, err = Load(path); err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Zs.ConcurrentSlots != 7 {
		t.Errorf("env ConcurrentSlots = %d, want 7 (env must beat YAML)", cfg.Zs.ConcurrentSlots)
	}
}

// TestConcurrentSlots_OldKeyIsInert documents the breaking rename: the former
// `mbr_deposit_slots` is not read at all, so a config still carrying it falls
// back to the default rather than keeping the operator's raised value. This is
// the accepted cost of the hard rename — pinned here so it is a decision on
// record, not an accident someone "fixes" by re-adding an alias unknowingly.
func TestConcurrentSlots_OldKeyIsInert(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(path, []byte("zs:\n  mbr_deposit_slots: 40\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Zs.ConcurrentSlots != zs.DefaultDepositSlots {
		t.Errorf("ConcurrentSlots = %d, want the default %d — the old key must not apply",
			cfg.Zs.ConcurrentSlots, zs.DefaultDepositSlots)
	}
}

// Privacy stays unset unless the payer sets it, so the SDK picks the network's
// default: relays everywhere but on a local chain. Seeding true here would
// make a localnet payer demand a relay its one node can't provide.
func TestPrivacy_UnsetUntilSet(t *testing.T) {
	for _, env := range []string{"false", "true"} {
		clearProxyEnv(t)
		if p := defaults().Zs.Privacy; p != nil {
			t.Fatalf("defaults().Zs.Privacy = %v, want unset", *p)
		}
		cfg := defaults()
		t.Setenv("PROXY_ZS_PRIVACY", env)
		applyEnv(cfg)
		if cfg.Zs.Privacy == nil || strconv.FormatBool(*cfg.Zs.Privacy) != env {
			t.Errorf("PROXY_ZS_PRIVACY=%s → %v", env, cfg.Zs.Privacy)
		}
	}
}

// The SSRF dial guard's opt-out stays unset unless set, so the SDK keeps the
// guard on everywhere but a local chain, and an explicit value wins either way.
func TestAllowPrivateOperators_UnsetUntilSet(t *testing.T) {
	for _, env := range []string{"true", "false"} {
		clearProxyEnv(t)
		if a := defaults().Zs.AllowPrivateOperators; a != nil {
			t.Fatalf("defaults().Zs.AllowPrivateOperators = %v, want unset", *a)
		}
		cfg := defaults()
		t.Setenv("PROXY_ZS_ALLOW_PRIVATE_OPERATORS", env)
		applyEnv(cfg)
		if cfg.Zs.AllowPrivateOperators == nil || strconv.FormatBool(*cfg.Zs.AllowPrivateOperators) != env {
			t.Errorf("PROXY_ZS_ALLOW_PRIVATE_OPERATORS=%s → %v", env, cfg.Zs.AllowPrivateOperators)
		}
	}
}

// TestAllowStaging_DefaultOffEnvFlips confirms staging nodes are held out of
// routing by default (AllowStaging=false) and that PROXY_ZS_ALLOW_STAGING
// flips it on — the env half of the config/flag plumbing that feeds
// selection.Constraints.AllowStaging.
func TestAllowStaging_DefaultOffEnvFlips(t *testing.T) {
	clearProxyEnv(t)

	if defaults().Zs.AllowStaging {
		t.Error("defaults().Zs.AllowStaging = true, want false (skip staging nodes by default)")
	}

	cfg := defaults()
	t.Setenv("PROXY_ZS_ALLOW_STAGING", "true")
	applyEnv(cfg)
	if !cfg.Zs.AllowStaging {
		t.Error("PROXY_ZS_ALLOW_STAGING=true did not enable staging routing")
	}
}

// TestPrivacy_YAMLDisables confirms `privacy: false` in YAML is kept as set,
// through the full Load path, and an omitted key stays unset.
func TestPrivacy_YAMLDisables(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(path, []byte("zs:\n  privacy: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg.Zs.Privacy == nil || *cfg.Zs.Privacy {
		t.Errorf("yaml privacy: false → %v, want false", cfg.Zs.Privacy)
	}

	// And a config that omits the key leaves it to the SDK.
	path2 := filepath.Join(dir, "cfg2.yaml")
	if err := os.WriteFile(path2, []byte("logging:\n  level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load err = %v", err)
	}
	if cfg2.Zs.Privacy != nil {
		t.Errorf("YAML omitting privacy → %v, want unset", *cfg2.Zs.Privacy)
	}
}

func TestLoad_YAMLParseError(t *testing.T) {
	clearProxyEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(path, []byte("::: not yaml :::"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("want 'parse' in error, got %v", err)
	}
}

func TestApplyEnv_AllKeys(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("PROXY_SERVER_LISTEN", "1.2.3.4:9999")
	t.Setenv("PROXY_SERVER_READ_TIMEOUT", "15s")
	t.Setenv("PROXY_SERVER_WRITE_TIMEOUT", "25s")
	t.Setenv("PROXY_SERVER_IDLE_TIMEOUT", "35s")
	t.Setenv("PROXY_LOGGING_LEVEL", "warn")
	t.Setenv("PROXY_LOGGING_FORMAT", "json")
	t.Setenv("PROXY_ZS_AFFINITY_POLICY", "strict")
	t.Setenv("PROXY_ZS_AFFINITY_TTL", "2m")
	t.Setenv("PROXY_ZS_AFFINITY_MAX", "42")

	cfg := defaults()
	applyEnv(cfg)

	if cfg.Server.Listen != "1.2.3.4:9999" {
		t.Errorf("Listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.ReadTimeout != 15*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 25*time.Second {
		t.Errorf("WriteTimeout = %v", cfg.Server.WriteTimeout)
	}
	if cfg.Server.IdleTimeout != 35*time.Second {
		t.Errorf("IdleTimeout = %v", cfg.Server.IdleTimeout)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("Logging.Level = %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format = %q", cfg.Logging.Format)
	}
	if cfg.Zs.AffinityPolicy != "strict" {
		t.Errorf("Zs.AffinityPolicy = %q", cfg.Zs.AffinityPolicy)
	}
	if cfg.Zs.AffinityTTL != 2*time.Minute {
		t.Errorf("Zs.AffinityTTL = %v", cfg.Zs.AffinityTTL)
	}
	if cfg.Zs.AffinityMax != 42 {
		t.Errorf("Zs.AffinityMax = %d", cfg.Zs.AffinityMax)
	}
}

func TestApplyEnv_DurationInvalid(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("PROXY_SERVER_READ_TIMEOUT", "banana")

	cfg := defaults()
	before := cfg.Server.ReadTimeout
	applyEnv(cfg)
	if cfg.Server.ReadTimeout != before {
		t.Errorf("bad duration overrode default: got %v, want %v", cfg.Server.ReadTimeout, before)
	}
}

func TestLookupDuration(t *testing.T) {
	clearProxyEnv(t)

	if _, ok := lookupDuration("PROXY_MISSING"); ok {
		t.Error("missing key returned ok=true")
	}

	t.Setenv("PROXY_D_VALID", "2m")
	if d, ok := lookupDuration("PROXY_D_VALID"); !ok || d != 2*time.Minute {
		t.Errorf("valid: ok=%v d=%v", ok, d)
	}

	t.Setenv("PROXY_D_BAD", "not-a-duration")
	if _, ok := lookupDuration("PROXY_D_BAD"); ok {
		t.Error("invalid duration returned ok=true")
	}
}

func TestValidate_RequiresListen(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Server.Listen = ""
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "server.listen") {
		t.Errorf("want server.listen error, got %v", err)
	}
}

func TestValidate_LoggingLevel(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		cfg := baseValidConfig(t)
		cfg.Logging.Level = lvl
		if err := validate(cfg); err != nil {
			t.Errorf("level %q rejected: %v", lvl, err)
		}
	}
	cfg := baseValidConfig(t)
	cfg.Logging.Level = "verbose"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "logging.level") {
		t.Errorf("want level error, got %v", err)
	}
}

func TestValidate_LoggingFormat(t *testing.T) {
	for _, f := range []string{"text", "json"} {
		cfg := baseValidConfig(t)
		cfg.Logging.Format = f
		if err := validate(cfg); err != nil {
			t.Errorf("format %q rejected: %v", f, err)
		}
	}
	cfg := baseValidConfig(t)
	cfg.Logging.Format = "xml"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "logging.format") {
		t.Errorf("want format error, got %v", err)
	}
}

// TestValidate_EmptyOperatorIDs verifies that no operator_ids is accepted
// at config-load time — the empty list means "fetch all operators from chain",
// which happens in main.go after config loads.
func TestValidate_EmptyOperatorIDs(t *testing.T) {
	cfg := baseValidConfig(t)
	if err := validate(cfg); err != nil {
		t.Errorf("empty operator_ids should be valid (fetch all from chain): %v", err)
	}
}

func TestValidate_DefaultsAffinity(t *testing.T) {
	cfg := baseValidConfig(t)
	if err := validate(cfg); err != nil {
		t.Fatal(err)
	}
	if string(cfg.Zs.ResolvedAffinity) != "prefer" {
		t.Errorf("default affinity = %q, want prefer", cfg.Zs.ResolvedAffinity)
	}
	if cfg.Zs.AffinityTTL != defaultAffinityTTL {
		t.Errorf("default AffinityTTL = %v", cfg.Zs.AffinityTTL)
	}
	if cfg.Zs.AffinityMax != defaultAffinityMax {
		t.Errorf("default AffinityMax = %d", cfg.Zs.AffinityMax)
	}
}

// TestValidate_AllowsEmptyProxyPayerAddr verifies that config-level
// validation no longer rejects an empty proxy_payer_addr — the value is
// resolved in main.go from the keystore (auto-derived when exactly one
// mnemonic is loaded). A bad keystore state surfaces there, not here.
func TestValidate_AllowsEmptyProxyPayerAddr(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Zs.ProxyPayerAddr = ""
	if err := validate(cfg); err != nil {
		t.Errorf("empty proxy_payer_addr should pass static validation: %v", err)
	}
}

// TestValidate_RejectsMalformedProxyPayerAddr keeps the format
// check live for explicit values — a typoed YAML/env entry should fail
// fast at config load rather than dragging the failure into runtime.
func TestValidate_RejectsMalformedProxyPayerAddr(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Zs.ProxyPayerAddr = "not-a-real-algorand-address"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "proxy_payer_addr") {
		t.Errorf("want proxy_payer_addr format error, got %v", err)
	}
}

func TestValidate_RejectsBadAffinityPolicy(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Zs.AffinityPolicy = "maybe"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "affinity_policy") {
		t.Errorf("want affinity_policy error, got %v", err)
	}
}

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.Server.Listen == "" {
		t.Error("defaults missing values")
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("default ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 0 {
		t.Errorf("default WriteTimeout = %v, want 0 (streaming)", cfg.Server.WriteTimeout)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("default logging wrong: %+v", cfg.Logging)
	}
}

func TestTEEConfig_DefaultsToBuiltin(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)

	cfg, err := Load("/missing/cfg.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Zs.TEE.Verifier != TEEVerifierBuiltin {
		t.Errorf("default TEE.Verifier = %q, want %q", cfg.Zs.TEE.Verifier, TEEVerifierBuiltin)
	}
	if cfg.Zs.TEE.AllowStub {
		t.Errorf("default AllowStub should be false")
	}
	if len(cfg.Zs.TEE.RequireForModels) != 0 {
		t.Errorf("default RequireForModels should be empty: %v", cfg.Zs.TEE.RequireForModels)
	}
}

// The rename from "nras" to "builtin" is only safe if the old value
// still loads: config.example.yaml shipped `verifier: "nras"`, so it is
// in somebody's file, and validate rejects an unknown kind outright —
// a bare rename would fail their next start on a value we told them to
// write. The normalization matters as much as the acceptance, since
// buildTEEVerifier and the "no allowlist" notice both switch on the
// value that comes out of here.
//
// The reject case pins a platform name specifically. That is the wrong
// value an operator actually reaches for — it is what their own logs
// call the thing being verified — so it is the one whose error text has
// to say more than "not supported".
func TestTEEConfig_VerifierAliasAndRejection(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty normalizes", "", TEEVerifierBuiltin, false},
		{"legacy nras alias normalizes", TEEVerifierLegacyNRAS, TEEVerifierBuiltin, false},
		{"canonical passes through", TEEVerifierBuiltin, TEEVerifierBuiltin, false},
		{"a platform name is not a verifier", "dstack-tdx", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Zs.TEE.Verifier = tc.in

			err := validateTEE(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateTEE(%q) = nil, want an error", tc.in)
				}
				// The message has to name the canonical value, or it
				// tells the operator they are wrong without telling
				// them what is right.
				if !strings.Contains(err.Error(), TEEVerifierBuiltin) {
					t.Errorf("error does not name %q: %v", TEEVerifierBuiltin, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateTEE(%q): %v", tc.in, err)
			}
			if cfg.Zs.TEE.Verifier != tc.want {
				t.Errorf("Verifier = %q, want %q", cfg.Zs.TEE.Verifier, tc.want)
			}
		})
	}
}

func TestTEEConfig_EnvOverrides(t *testing.T) {
	clearProxyEnv(t)
	setValidEscrowEnv(t)
	t.Setenv("PROXY_ZS_TEE_ALLOW_STUB", "true")
	t.Setenv("PROXY_ZS_TEE_REQUIRE_FOR_MODELS", " gpt-4o-mini , my/model ,, ")

	cfg, err := Load("/missing/cfg.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Zs.TEE.AllowStub {
		t.Errorf("AllowStub should be true after env override")
	}
	want := []string{"gpt-4o-mini", "my/model"}
	got := cfg.Zs.TEE.RequireForModels
	if len(got) != len(want) {
		t.Fatalf("RequireForModels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RequireForModels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTEEConfig_RejectsUnknownVerifier(t *testing.T) {
	cfg := baseValidConfig(t)
	cfg.Zs.TEE.Verifier = "bogus"
	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "zs.tee.verifier") {
		t.Fatalf("want tee.verifier error, got %v", err)
	}
}

func TestTEEConfig_IsRequiredForModel(t *testing.T) {
	c := TEEConfig{RequireForModels: []string{"gpt-4o-mini", "claude-opus"}}
	if !c.IsRequiredForModel("gpt-4o-mini") {
		t.Errorf("model in list should be required")
	}
	if c.IsRequiredForModel("not-listed") {
		t.Errorf("model not in list should not be required")
	}
	empty := TEEConfig{}
	if empty.IsRequiredForModel("anything") {
		t.Errorf("empty list should never require TEE")
	}
}

func TestApplyEnv_Spend(t *testing.T) {
	clearProxyEnv(t)
	cfg := defaults()
	t.Setenv("PROXY_SPEND_DAILY_CAP_USDC", "25.5")
	t.Setenv("PROXY_SPEND_PER_REQUEST_CAP_USDC", "2")
	applyEnv(cfg)
	if cfg.Spend.DailyCapUSDC != 25.5 {
		t.Errorf("daily cap = %g, want 25.5", cfg.Spend.DailyCapUSDC)
	}
	if cfg.Spend.PerRequestCapUSDC != 2 {
		t.Errorf("per-request cap = %g, want 2", cfg.Spend.PerRequestCapUSDC)
	}
}

func TestValidate_RejectsNegativeSpendCap(t *testing.T) {
	cfg := defaults()
	cfg.Zs.EscrowAppID = 1
	cfg.Spend.DailyCapUSDC = -1
	if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "spend.daily_cap_usdc") {
		t.Fatalf("want spend.daily_cap_usdc error, got %v", err)
	}
}
