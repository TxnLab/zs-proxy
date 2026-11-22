/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/attest"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// minimalCompose is a well-formed app-compose document with nothing in it that
// the release rules care about, so a violation reported over it is a statement
// about the POLICY rather than about the document.
const minimalCompose = `{"manifest_version":2,"runner":"docker-compose",` +
	`"docker_compose_file":"services:\n  zs-node:\n    image: ghcr.io/txnlab/zs-node@sha256:00\n"}`

// sealed is the document's own digest as the measurement, which makes
// VerifyAppCompose's provenance gate pass so the policy rules below it run.
// Legitimate only in a test: on a real bundle the measurement comes from
// attest.VerifyEventLog.
func sealed(doc string) map[string]string {
	sum := sha256.Sum256([]byte(doc))
	return map[string]string{attest.EventComposeHash: hex.EncodeToString(sum[:])}
}

func violationCodes(t *testing.T, err error) []attest.ViolationCode {
	t.Helper()
	var pe *attest.ComposePolicyError
	if !errors.As(err, &pe) {
		return nil
	}
	out := make([]attest.ViolationCode, 0, len(pe.Violations))
	for _, v := range pe.Violations {
		out = append(out, v.Code)
	}
	return out
}

// TestComposePolicyFromTEEConfig_PopulatedListsWithTheFlagOffAreCleared covers
// the state SPEC §3e calls a defect — populated lists beside a false flag, a
// verifier that checks nothing while looking configured.
//
// THIS TEST INVERTED, and the reason is the seeded defaults. It used to assert
// that the state is REACHABLE from YAML and merely warned about, which was
// right while the lists were empty by default: reaching it then took a
// deliberate paste, and warning was the most that could be done without
// second-guessing the operator.
//
// Seeding both lists made that reasoning false. Every install now has populated
// lists, so `enforce_compose_skeleton: false` — the one documented way to reach
// an unpublished build — landed in the defect state by default and refused the
// whole fleet, naming OUR config. The flag is the written-down decision, so it
// now wins and the reference data is dropped with it, which makes the state
// unreachable through this function rather than merely reported.
//
// EXECUTED, not asserted as a bool: the field values were never the defect, so
// reading them back is exactly what cannot show it. The rule stays reachable
// for library callers and the browser client, which build policies directly.
func TestComposePolicyFromTEEConfig_PopulatedListsWithTheFlagOffAreCleared(t *testing.T) {
	cfg := config.TEEConfig{
		TrustedComposeSkeletons: []string{"aa11"},
		TrustedNodeImages:       []string{"ghcr.io/txnlab/zs-node@sha256:00"},
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	policy := ComposePolicyFromTEEConfig(cfg, log)

	// Matched on the consequence, so a reworded message survives.
	if !strings.Contains(buf.String(), "the release lists are ignored") {
		t.Errorf("dropping the lists was not reported; output: %s", buf.String())
	}
	_, err := attest.VerifyAppCompose(minimalCompose, sealed(minimalCompose), policy)
	if slices.Contains(violationCodes(t, err), attest.ViolationPolicyMisconfigured) {
		t.Fatalf("policy_misconfigured survived the flag being off, so an operator "+
			"turning the release match off is refused by every node: %v", err)
	}

	// The counter-case: with the flag ON the same lists are carried and the
	// match runs. Without it the assertions above pass for a builder that
	// clears the lists unconditionally and never enforces anything.
	cfg.EnforceComposeSkeleton = true
	buf.Reset()
	policy = ComposePolicyFromTEEConfig(cfg, log)
	if strings.Contains(buf.String(), "the release lists are ignored") {
		t.Errorf("the lists were dropped despite enforce_compose_skeleton: true; output: %s",
			buf.String())
	}
	_, err = attest.VerifyAppCompose(minimalCompose, sealed(minimalCompose), policy)
	if !slices.Contains(violationCodes(t, err), attest.ViolationComposeSkeleton) {
		t.Errorf("the release match did not run with the flag on: %v", err)
	}
}

// A typo in either list key yields an empty list, and THAT is the case the
// written-down flag exists to make loud: enforcement stays on, every node is
// refused, and an operator gets an immediate symptom instead of a proxy that
// routes to anything while its config reads as configured.
func TestComposePolicyFromTEEConfig_EnforcingWithEmptyListsRefusesLoudly(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	policy := ComposePolicyFromTEEConfig(config.TEEConfig{EnforceComposeSkeleton: true}, log)
	if !policy.EnforceComposeSkeleton {
		t.Fatal("the configured flag did not reach the policy")
	}
	if !strings.Contains(buf.String(), "EVERY node is refused") {
		t.Errorf("enforcing over empty lists went unreported; output: %s", buf.String())
	}
	// And it really refuses, rather than merely warning about it.
	if !slices.Contains(violationCodes(t, mustVerify(t, minimalCompose, policy)),
		attest.ViolationComposeSkeleton) {
		t.Error("enforcement over an empty list admitted a node")
	}
}

// Empty lists must leave the check OFF rather than on-over-nothing: enforcing
// an empty list refuses every node, and that is the shipped state until a
// zs-node release compose exists.
func TestComposePolicyFromTEEConfig_UnconfiguredDoesNotEnforce(t *testing.T) {
	policy := ComposePolicyFromTEEConfig(config.TEEConfig{}, quietLogger())
	if policy.EnforceComposeSkeleton {
		t.Fatal("enforcement is on with both lists empty, which refuses every node")
	}
	// And the rule really is dormant, not merely flagged off.
	if slices.Contains(violationCodes(t, mustVerify(t, minimalCompose, policy)),
		attest.ViolationComposeSkeleton) {
		t.Error("compose_skeleton fired with enforcement off")
	}
}

// The half-configured case is the rollout footgun: with enforcement on, one
// populated list still refuses every node on the empty other. That is the
// correct fail-closed direction, and it must be reported rather than left to a
// fleet of measurement_mismatch.
func TestComposePolicyFromTEEConfig_WarnsWhenOnlyOneListIsPopulated(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.TEEConfig
		wantLog bool
	}{
		{"skeletons only", config.TEEConfig{
			EnforceComposeSkeleton:  true,
			TrustedComposeSkeletons: []string{"aa11"},
		}, true},
		{"images only", config.TEEConfig{
			EnforceComposeSkeleton: true,
			TrustedNodeImages:      []string{"img"},
		}, true},
		{
			"both",
			config.TEEConfig{
				EnforceComposeSkeleton:  true,
				TrustedComposeSkeletons: []string{"aa11"},
				TrustedNodeImages:       []string{"img"},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			policy := ComposePolicyFromTEEConfig(tc.cfg, log)
			if !policy.EnforceComposeSkeleton {
				t.Fatal("one populated list did not turn the release match on")
			}
			// Matched on the consequence, so a reworded message survives.
			warned := strings.Contains(buf.String(), "every node is refused on the empty list")
			if warned != tc.wantLog {
				t.Errorf("half-configured warning = %v, want %v; output: %s",
					warned, tc.wantLog, buf.String())
			}
		})
	}
}

// TestComposePolicyFromTEEConfig_OffSwitchIsCoherent pins the one operator
// action the config file documents for reaching an unpublished build.
//
// It needs a test because the failure was invisible: the lists ship SEEDED, so
// `enforce_compose_skeleton: false` alone left populated lists beside a false
// flag, which attest reports as ViolationPolicyMisconfigured on every node that
// publishes a preimage. The proxy refused the whole fleet, naming its OWN
// configuration, and every existing test passed — the config-level test asserts
// the bool survives Load and cannot see anything downstream of it.
//
// Asserted through a real VerifyAppCompose rather than on the struct, because
// the defect was never in the field values: it was in what attest does with
// them, which is precisely what reading the struct back cannot show.
func TestComposePolicyFromTEEConfig_OffSwitchIsCoherent(t *testing.T) {
	cfg := config.TEEConfig{
		EnforceComposeSkeleton:  false,
		TrustedComposeSkeletons: []string{"aa11"},
		TrustedNodeImages:       []string{"ghcr.io/txnlab/zs-node@sha256:00"},
	}
	policy := ComposePolicyFromTEEConfig(cfg, quietLogger())

	codes := violationCodes(t, mustVerify(t, minimalCompose, policy))
	if slices.Contains(codes, attest.ViolationPolicyMisconfigured) {
		t.Error("turning the release match off left the seeded lists in place, so every " +
			"node is refused as policy_misconfigured — the documented off switch does " +
			"not switch anything off")
	}
	if slices.Contains(codes, attest.ViolationComposeSkeleton) {
		t.Error("compose_skeleton fired with the match off")
	}
}

// The lists must reach the policy VERBATIM. A skeleton digest is compared
// case-insensitively but an image reference is not — registry paths and tags
// are case-sensitive — so anything that normalized them on the way through
// would silently refuse a correctly-listed image.
func TestComposePolicyFromTEEConfig_CarriesTheListsUnchanged(t *testing.T) {
	cfg := config.TEEConfig{
		// Enforcing, because the lists are only carried when the match is on —
		// with it off they are deliberately cleared, so a fixture omitting
		// this would assert against nil and say nothing about normalization.
		EnforceComposeSkeleton:  true,
		TrustedComposeSkeletons: []string{"aa11", "BB22"},
		TrustedNodeImages:       []string{"ghcr.io/TxnLab/zs-node@sha256:00"},
	}
	policy := ComposePolicyFromTEEConfig(cfg, quietLogger())
	if !slices.Equal(policy.TrustedComposeSkeletons, cfg.TrustedComposeSkeletons) {
		t.Errorf("TrustedComposeSkeletons = %v, want %v",
			policy.TrustedComposeSkeletons, cfg.TrustedComposeSkeletons)
	}
	if !slices.Equal(policy.TrustedNodeImages, cfg.TrustedNodeImages) {
		t.Errorf("TrustedNodeImages = %v, want %v", policy.TrustedNodeImages, cfg.TrustedNodeImages)
	}
}

// A nil logger is the `zs-proxy tee inspect` call: it wants the policy without
// the startup notices. A panic there would take out the diagnostic command
// that exists to explain a refusal.
func TestComposePolicyFromTEEConfig_NilLogger(t *testing.T) {
	if p := ComposePolicyFromTEEConfig(config.TEEConfig{}, nil); !p.EnforceEnvAllowlist {
		t.Error("a nil logger produced a policy without the shared rules")
	}
}

func mustVerify(t *testing.T, doc string, policy attest.ComposePolicy) error {
	t.Helper()
	_, err := attest.VerifyAppCompose(doc, sealed(doc), policy)
	return err
}
