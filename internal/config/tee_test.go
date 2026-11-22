/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/attest"
)

// The allowlist keys are what an operator types into config.yaml, and a
// struct-tag typo — or documenting a different spelling than the code
// reads — silently yields an EMPTY allowlist. The symptom is then
// identical to a correctly-configured proxy meeting a fleet of bad
// nodes: every dstack operator fails measurement_mismatch. So the
// spelling is pinned against literal YAML rather than against the
// struct.
func TestLoad_TEEMeasurementAllowlists(t *testing.T) {
	const (
		compose = "6e6f3898b6c20ee50c12713a80453da2c992de95e857d64791b909037f157485"
		osImage = "bd369a8c2f9edb2b52dad48ac8e0b32dde5f1337c423a506b48d07403a7d8033"
	)
	yaml := `
algod:
  network: testnet
zs:
  escrow_app_id: 1
  tee:
    allow_stub: false
    trusted_compose_hashes:
      - "` + compose + `"
    trusted_os_images:
      - "` + osImage + `"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Zs.TEE.TrustedComposeHashes; !slices.Equal(got, []string{compose}) {
		t.Errorf("trusted_compose_hashes = %v, want [%s]", got, compose)
	}
	if got := cfg.Zs.TEE.TrustedOSImages; !slices.Equal(got, []string{osImage}) {
		t.Errorf("trusted_os_images = %v, want [%s]", got, osImage)
	}
}

// The release lists carry the same struct-tag risk as the two above. The
// consequence is the LOUD direction, and it is worth being exact about which:
// app.ComposePolicyFromTEEConfig assigns EnforceComposeSkeleton from the
// config field, never from the lists' length (tee_policy.go), so a typo yields
// an empty list with the match still ON — which refuses every node
// immediately, rather than silently verifying nothing. Deriving the flag from
// length is precisely the fail-open shape both consumers refuse; SPEC §3e
// forbids it.
func TestLoad_TEEReleaseLists(t *testing.T) {
	const (
		skeleton = "b66a7d03d5c42a4094a9d0042bd6950ee9304d4837b9fe75c447a699115e7ad6"
		image    = "ghcr.io/txnlab/zs-node@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)
	yaml := `
algod:
  network: testnet
zs:
  escrow_app_id: 1
  tee:
    trusted_compose_skeletons:
      - "` + skeleton + `"
    trusted_node_images:
      - "` + image + `"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Zs.TEE.TrustedComposeSkeletons; !slices.Equal(got, []string{skeleton}) {
		t.Errorf("trusted_compose_skeletons = %v, want [%s]", got, skeleton)
	}
	if got := cfg.Zs.TEE.TrustedNodeImages; !slices.Equal(got, []string{image}) {
		t.Errorf("trusted_node_images = %v, want [%s]", got, image)
	}
}

// Both release lists ship seeded, WITH the flag, and the three move together.
//
// THIS REVERSES AN EARLIER RULE, so the earlier reasoning is worth keeping
// rather than quietly dropping. It read: "neither list may acquire a default. A
// shipped skeleton digest would bless a deployment shape nobody chose, and —
// because enforcement is derived from length — it would also turn the release
// match on for every proxy at once."
//
// Both halves have since stopped being true. Enforcement is NOT derived from
// length; it is a written-down flag, deliberately (see EnforceComposeSkeleton's
// field doc and app.ComposePolicyFromTEEConfig), so seeding a list turns
// nothing on by itself. And the shape is no longer one "nobody chose": a
// published zs-node release is one artifact shared by every operator, which is
// exactly the property that made trusted_os_images defaultable in the test
// below. A per-deployment compose HASH still has no correct default, and still
// gets none.
//
// The flag is asserted alongside the lists because seeding them without it is a
// defect SPEC §3e names: populated lists with enforcement off consult nothing
// while looking configured, and attest reports it against the verifier
// (ViolationPolicyMisconfigured). A default install must not be born in that
// state.
func TestLoad_TEEReleaseListsShipSeededWithTheFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	minimal := "algod:\n  network: testnet\nzs:\n  escrow_app_id: 1\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Zs.TEE.TrustedComposeSkeletons) == 0 {
		t.Error("trusted_compose_skeletons is empty by default — an unconfigured " +
			"proxy would enforce over an empty list and refuse every node")
	}
	if len(cfg.Zs.TEE.TrustedNodeImages) == 0 {
		t.Error("trusted_node_images is empty by default — same refuse-everything state")
	}
	// The protocol module's lists, not a copy: zs-node's release CI appends to
	// those, so a local list would never receive a new release.
	if !slices.Equal(cfg.Zs.TEE.TrustedComposeSkeletons, attest.TrustedComposeSkeletons()) ||
		!slices.Equal(cfg.Zs.TEE.TrustedNodeImages, attest.TrustedNodeImages()) {
		t.Error("the default release lists are not attest.TrustedComposeSkeletons / TrustedNodeImages")
	}
	if !cfg.Zs.TEE.EnforceComposeSkeleton {
		t.Error("enforce_compose_skeleton is false with both lists seeded — the " +
			"policy_misconfigured state: configured-looking, checking nothing")
	}
	// The compose-hash pin is the one that stays unseeded: it is
	// per-deployment, so there is no fleet-wide value to ship.
	if len(cfg.Zs.TEE.TrustedComposeHashes) != 0 {
		t.Errorf("trusted_compose_hashes acquired a default: %v",
			cfg.Zs.TEE.TrustedComposeHashes)
	}
}

// An operator turning the match off must be able to, and it must take the
// written-down decision rather than emptying a list.
//
// Both directions matter. Clearing the lists while the flag stays true is the
// refuse-everything state, which is safe but is not "off"; setting the flag
// false is. Without this, seeding the defaults above would be a one-way door
// for anyone running a node built from an unpublished branch.
func TestLoad_TEEReleaseMatchCanBeTurnedOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "algod:\n  network: testnet\nzs:\n  escrow_app_id: 1\n" +
		"  tee:\n    enforce_compose_skeleton: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Zs.TEE.EnforceComposeSkeleton {
		t.Error("enforce_compose_skeleton: false did not survive the defaults — " +
			"the release match cannot be turned off")
	}
}

// The two allowlists default DIFFERENTLY, and the asymmetry is deliberate.
//
// The original rule here was "empty must stay empty rather than acquiring a
// default, because a defaulted allowlist would be a proxy that accepts a
// measurement nobody chose to trust." That holds for compose hashes, which are
// per-operator-deployment — a default would be blessing someone's specific
// workload. It does not hold for the OS image: that digest is identical for
// every operator on a dstack release and names Phala's guest image, not any
// operator's software, so making each payer paste the same fleet-wide constant
// bought nothing and cost the check outright whenever they didn't.
//
// What must survive the change is the fail-closed property, which is asserted
// separately below.
func TestLoad_TEEAllowlistDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	minimal := "algod:\n  network: testnet\nzs:\n  escrow_app_id: 1\n  tee:\n    allow_stub: false\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Zs.TEE.TrustedComposeHashes) != 0 {
		t.Errorf("trusted_compose_hashes acquired a default: %v — a compose hash is "+
			"per-deployment, so a default blesses a workload nobody chose to trust",
			cfg.Zs.TEE.TrustedComposeHashes)
	}
	if len(cfg.Zs.TEE.TrustedOSImages) == 0 {
		t.Error("trusted_os_images is empty by default — the preset is what stops an " +
			"operator having to paste a fleet-wide constant, and without it dstack " +
			"evidence cannot verify at all")
	}
	// PIN THE VALUE, not the shape. A length check cannot tell a production
	// digest from a dev-image one or from a compose hash — all three are 64
	// hex characters — so on its own it would stay green while the preset
	// blessed the wrong image entirely, which fails CLOSED and therefore
	// looks like nothing at all.
	//
	// The reference is the os-image-hash the dstack verifier's own capture
	// was taken on, read out of that testdata rather than restated here: a
	// second literal would only assert that this file agrees with itself.
	want := capturedOSImageHash(t)
	if !slices.Contains(cfg.Zs.TEE.TrustedOSImages, want) {
		t.Errorf("the preset %v does not contain the OS image the verifier's capture was taken on (%s) — "+
			"a wrong digest rejects every real node while every test stays green",
			cfg.Zs.TEE.TrustedOSImages, want)
	}
}

// capturedOSImageHash reads the os-image-hash out of the dstack verifier's
// event-log capture.
//
// Reaching into the SDK package's testdata is deliberate. The alternative —
// a copy of the digest in this file — is the failure being guarded against:
// two literals that agree with each other and with nothing real.
func capturedOSImageHash(t *testing.T) string {
	t.Helper()
	dir, err := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/TxnLab/zerosignal/go/zs/tee").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("locate the zs/tee package: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("locate the zs/tee package: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(dir)), "testdata", "ds_event_log.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var events []struct {
		Event        string `json:"event"`
		EventPayload string `json:"event_payload"`
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, e := range events {
		if e.Event == "os-image-hash" {
			return strings.ToLower(strings.TrimPrefix(e.EventPayload, "0x"))
		}
	}
	t.Fatalf("%s carries no os-image-hash event, so the assertion above would be vacuous", path)
	return ""
}

// The property the preset must not cost: an operator can still turn the check
// off, and turning it off means NOTHING VERIFIES rather than everything does.
// A default seeded in defaults() is replaced wholesale by any explicit YAML
// value, including an empty one — this pins that, because the obvious
// "merge the preset in" refactor would make the list impossible to clear.
func TestLoad_TEEExplicitEmptyOSImagesOverridesThePreset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	minimal := "algod:\n  network: testnet\nzs:\n  escrow_app_id: 1\n  tee:\n    trusted_os_images: []\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Zs.TEE.TrustedOSImages) != 0 {
		t.Errorf("explicit empty trusted_os_images did not clear the preset: %v",
			cfg.Zs.TEE.TrustedOSImages)
	}
}

// The preset must not be a shared backing array. defaults() lands in a config
// struct callers mutate, and app.Run is importable — one embedder appending to
// the list would otherwise be observed by the next.
func TestDefaultTrustedOSImages_IsNotShared(t *testing.T) {
	a := defaultTrustedOSImages()
	b := defaultTrustedOSImages()
	if len(a) == 0 {
		t.Fatal("preset is empty, so this test asserts nothing")
	}
	a[0] = "mutated"
	if b[0] == "mutated" {
		t.Error("defaultTrustedOSImages returns a shared slice")
	}
}
