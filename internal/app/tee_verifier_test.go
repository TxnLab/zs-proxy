/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/attest"
	"github.com/TxnLab/zerosignal/go/zs/tee"
	"github.com/TxnLab/zs-proxy/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The measurement allowlists are the only thing standing between "this
// quote is signed by real hardware" and "this quote is signed by real
// hardware running software I recognise". If they do not reach the
// verifier, every dstack operator fails measurement_mismatch no matter
// how the proxy is configured — fail-closed, but the feature never
// works and nothing anywhere notices.
//
// Nothing outside this function constructs the verifier, so this is the
// only place the wiring can be asserted.
func TestBuildTEEVerifier_CarriesTheAllowlists(t *testing.T) {
	cfg := config.TEEConfig{
		AllowStub:            true,
		TrustedComposeHashes: []string{"aa11"},
		TrustedOSImages:      []string{"bb22", "cc33"},
	}

	v, ok := buildTEEVerifier(cfg, quietLogger()).(*tee.BundleVerifier)
	if !ok {
		t.Fatal("buildTEEVerifier did not return an *BundleVerifier")
	}
	if !slices.Equal(v.TrustedComposeHashes, cfg.TrustedComposeHashes) {
		t.Errorf("TrustedComposeHashes = %v, want %v", v.TrustedComposeHashes, cfg.TrustedComposeHashes)
	}
	if !slices.Equal(v.TrustedOSImages, cfg.TrustedOSImages) {
		t.Errorf("TrustedOSImages = %v, want %v", v.TrustedOSImages, cfg.TrustedOSImages)
	}
	if !v.AllowStub {
		t.Error("AllowStub did not reach the verifier")
	}
	// The compose policy is the third reference, and unlike the two
	// above it comes from a shared constructor rather than from cfg —
	// so a dropped line here reads as "no config to carry" rather than
	// as a missing assignment. It fails OPEN in the direction that
	// matters: the zero policy still refuses the unconditional rules,
	// so a node with a root back door is still caught while every
	// env-allowlist and boot-hook rule silently stops running.
	if !v.ComposePolicy.EnforceEnvAllowlist {
		t.Error("ComposePolicy did not reach the verifier, or reached it without " +
			"the env allowlist — the unmeasured env channel is then unconstrained")
	}
	if !slices.Equal(v.ComposePolicy.AllowedEnvNames, attest.ZeroSignalSecretEnvNames()) {
		t.Errorf("AllowedEnvNames = %v, want the shared %v — a locally spelled "+
			"list is how the proxy and the browser client start disagreeing",
			v.ComposePolicy.AllowedEnvNames, attest.ZeroSignalSecretEnvNames())
	}
	// No override configured: the built-in list must survive intact.
	if !slices.Equal(v.ComposePolicy.PreLaunchScriptDigests, attest.PhalaPreLaunchDigests()) {
		t.Errorf("PreLaunchScriptDigests = %v, want the built-in %v",
			v.ComposePolicy.PreLaunchScriptDigests, attest.PhalaPreLaunchDigests())
	}
}

// TestBuildTEEVerifier_PreLaunchOverride covers the one reference an operator
// can replace, and the reason it exists rather than being a hardcoded list.
//
// The dstack pre-launch script is injected by the Phala Cloud API at deploy
// time — it is not in the CLI — so its digest moves on Phala's schedule with
// no version to track. The rule that checks it is unconditional, so the first
// time Phala edits their template every newly deployed node is refused while
// behaving correctly. Without a lever an operator waits for a zs-proxy release.
//
// It REPLACES rather than appends, and that is the half worth pinning: an
// operator naming a set is naming the whole set, and quietly keeping ours
// underneath would make removing a digest impossible while looking like it
// worked.
func TestBuildTEEVerifier_PreLaunchOverride(t *testing.T) {
	mine := []string{"aa11", "bb22"}
	cfg := config.TEEConfig{
		TrustedOSImages:         []string{"cc33"},
		TrustedPreLaunchDigests: mine,
	}

	v, ok := buildTEEVerifier(cfg, quietLogger()).(*tee.BundleVerifier)
	if !ok {
		t.Fatal("buildTEEVerifier did not return an *BundleVerifier")
	}
	if !slices.Equal(v.ComposePolicy.PreLaunchScriptDigests, mine) {
		t.Errorf("PreLaunchScriptDigests = %v, want exactly %v",
			v.ComposePolicy.PreLaunchScriptDigests, mine)
	}
	// Precondition, so this cannot pass by the built-in list happening to
	// equal the override.
	if slices.Equal(mine, attest.PhalaPreLaunchDigests()) {
		t.Fatal("the fixture equals the built-in list, so it proves nothing")
	}
	// Everything else must still be the shared policy — an override of one
	// field must not quietly reset the rest.
	if !v.ComposePolicy.EnforceEnvAllowlist {
		t.Error("overriding the pre-launch list dropped the env allowlist")
	}
}

// An unrecognized verifier kind must disable TEE verification entirely
// rather than fall through to one that accepts things.
func TestBuildTEEVerifier_UnknownKindDisables(t *testing.T) {
	if v := buildTEEVerifier(config.TEEConfig{Verifier: "not-a-verifier"}, quietLogger()); v != nil {
		t.Errorf("buildTEEVerifier = %T, want nil for an unknown kind", v)
	}
}

// Config normalizes the legacy "nras" alias away before this is
// reached, so the case below can only arrive from an embedder building
// a TEEConfig by hand. That is exactly why it is here: the default arm
// returns nil, and nil means TEE verification off for the whole
// process. A rename that accepted the old value only in the config
// package would silently disable the feature for every caller of
// app.Run that does not go through Load.
func TestBuildTEEVerifier_AcceptsTheLegacyAliasAndTheEmptyString(t *testing.T) {
	for _, kind := range []string{"", config.TEEVerifierBuiltin, config.TEEVerifierLegacyNRAS} {
		t.Run("verifier="+kind, func(t *testing.T) {
			if v := buildTEEVerifier(config.TEEConfig{Verifier: kind}, quietLogger()); v == nil {
				t.Errorf("buildTEEVerifier(%q) = nil, want a verifier", kind)
			}
		})
	}
}

// The "no allowlist configured" notice, whose whole purpose is that the
// symptom — every dstack operator failing measurement_mismatch — is
// otherwise indistinguishable from a fleet of bad nodes.
//
// THE HALF-CONFIGURED CASE IS THE POINT. It is the likeliest real
// misconfiguration (an operator fills in compose hashes and misses the
// OS images), and it is exactly the case an `&&` guard would drop
// silently. Both single-sided cases are asserted so the guard cannot be
// narrowed to either one.
// wantNames is what makes the per-list message worth having: the notice now
// names WHICH list is empty, because the two no longer fail for the same
// reason (os images ship with a preset, compose hashes have no default), and
// a message that named neither sent an operator looking for a setting they had
// never touched. Asserting only "something was logged" would let both branches
// collapse back into one generic line with the suite green.
func TestBuildTEEVerifier_WarnsOnAnIncompleteAllowlist(t *testing.T) {
	cases := []struct {
		name      string
		cfg       config.TEEConfig
		wantLog   bool
		wantNames string
		// absentName is what makes this test able to fail. The message this
		// replaced named BOTH lists on one line, and a "does it mention
		// trusted_os_images" check passes against that too — so asserting only
		// presence would have stayed green against the very code being
		// changed. The point of the change is that exactly one is named.
		absentName string
	}{
		{"nothing configured", config.TEEConfig{}, true, "trusted_os_images", "trusted_compose_hashes"},
		{
			"compose set, os images missing",
			config.TEEConfig{TrustedComposeHashes: []string{"aa11"}}, true,
			"trusted_os_images", "trusted_compose_hashes",
		},
		{
			"os images set, compose missing",
			config.TEEConfig{TrustedOSImages: []string{"bb22"}}, true,
			"trusted_compose_hashes", "trusted_os_images",
		},
		{
			"os images set, compose missing, release match on",
			config.TEEConfig{TrustedOSImages: []string{"bb22"}, EnforceComposeSkeleton: true},
			false, "", "",
		},
		{
			"both configured",
			config.TEEConfig{TrustedComposeHashes: []string{"aa11"}, TrustedOSImages: []string{"bb22"}},
			false, "", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			buildTEEVerifier(tc.cfg, log)

			// Matched on the CONSEQUENCE rather than a field name, so the
			// assertion survives a reworded message while still failing if the
			// notice disappears.
			logged := strings.Contains(buf.String(), "cannot verify")
			if logged != tc.wantLog {
				t.Errorf("allowlist notice logged = %v, want %v; output: %s",
					logged, tc.wantLog, buf.String())
			}
			if tc.wantNames != "" && !strings.Contains(buf.String(), tc.wantNames) {
				t.Errorf("notice does not name %s; output: %s", tc.wantNames, buf.String())
			}
			if tc.absentName != "" && strings.Contains(buf.String(), tc.absentName) {
				t.Errorf("notice also names %s, so it is not telling the operator which "+
					"list to look at; output: %s", tc.absentName, buf.String())
			}
		})
	}
}
