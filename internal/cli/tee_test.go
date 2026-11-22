/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/attest"
	"github.com/TxnLab/zerosignal/go/inject"
	"github.com/TxnLab/zerosignal/go/zs/tee"
)

// teeCaptureDir is the SDK package's testdata: the same live Phala tdx.small
// CVM the verifier's own suite runs against — quote, event log and /Info
// response off ONE machine. Reading it rather than taking a copy: a copy is
// how the three files stop describing the same deployment, which is precisely
// the substitution the hash gate exists to catch.
func teeCaptureDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/TxnLab/zerosignal/go/zs/tee").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("locate the zs/tee package: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("locate the zs/tee package: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "testdata")
}

// capturedComposeHash is the compose-hash the capture's event log replays to.
// A literal, and legitimately so: it is a fact about the capture, and pinning
// it is what makes the gate assertion below able to fail. Matches the
// constant of the same meaning in the zs/tee package and internal/config.
const capturedComposeHash = "6e6f3898b6c20ee50c12713a80453da2c992de95e857d64791b909037f157485"

func teeCaptureBundle(t *testing.T) inject.TEEEvidenceBundle {
	t.Helper()
	qhex, err := os.ReadFile(filepath.Join(teeCaptureDir(t), "ds_quote_hex.txt"))
	if err != nil {
		t.Fatalf("read quote: %v", err)
	}
	quote, err := hex.DecodeString(strings.TrimSpace(string(qhex)))
	if err != nil {
		t.Fatalf("decode quote: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(teeCaptureDir(t), "ds_event_log.json"))
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	return inject.TEEEvidenceBundle{
		Mode:           inject.TEEModeDstackTDX,
		CPUReport:      base64.StdEncoding.EncodeToString(quote),
		EventLog:       string(log),
		AppCompose:     teeCaptureAppCompose(t),
		GeneratedAt:    time.Now().UTC(),
		RefreshSeconds: 1200,
	}
}

// teeCaptureAppCompose recovers the measured document from the guest agent's
// /Info response, which double-encodes it: tcb_info is a JSON string whose
// contents are JSON, and app_compose is a string inside that. Its exact bytes
// are what dstack hashed, so nothing here may re-marshal.
func teeCaptureAppCompose(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(teeCaptureDir(t), "ds_info.json"))
	if err != nil {
		t.Fatalf("read /Info capture: %v", err)
	}
	var info struct {
		TCBInfo string `json:"tcb_info"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse capture: %v", err)
	}
	var tcb struct {
		AppCompose string `json:"app_compose"`
	}
	if err := json.Unmarshal([]byte(info.TCBInfo), &tcb); err != nil {
		t.Fatalf("parse tcb_info: %v", err)
	}
	if tcb.AppCompose == "" {
		t.Fatal("the capture carries no app_compose, so every assertion below is vacuous")
	}
	return tcb.AppCompose
}

// teeCaptureNode serves the capture at /v1/zs/attestation, so the command
// under test walks its real fetch → replay → gate path rather than a shortcut.
func teeCaptureNode(t *testing.T, bundle inject.TEEEvidenceBundle) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/zs/attestation" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", inject.AttestationContentType)
		_ = json.NewEncoder(w).Encode(bundle)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runTeeInspect drives the real cobra command.
//
// --config points at a path that does not exist, which config.LoadBase
// tolerates and answers with the embedded defaults. Without it the test would
// read whatever config the developer's machine happens to have, and the
// release-match assertions would pass or fail on somebody's YAML.
func runTeeInspect(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), nil, args...)
}

// runTeeInspectWith additionally installs a config path and the quote-chain
// seam, so the chain verdicts — the branch that decides whether any allowlist
// value is handed out at all — are reachable without talking to Intel.
func runTeeInspectWith(t *testing.T, configPath string, chain func(context.Context, []byte) error, args ...string) (string, error) {
	t.Helper()
	return runTeeInspectStale(t, configPath, chain, false, args...)
}

// runTeeInspectStale additionally drives the collateral-staleness half of the
// chain verdict, which the error-only helper above cannot express.
func runTeeInspectStale(
	t *testing.T,
	configPath string,
	chain func(context.Context, []byte) error,
	stale bool,
	args ...string,
) (string, error) {
	t.Helper()
	cmd, f := teeInspectCmd()
	if chain != nil {
		f.verifyQuoteChain = func(ctx context.Context, q []byte) (bool, error) {
			return stale, chain(ctx, q)
		}
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--config", configPath}, args...))
	err := cmd.Execute()
	return buf.String(), err
}

func writeTeeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTeeInspect_AgainstTheRealCapture is the assertion that connects the tool
// to something that exists.
//
// Everything else here could be satisfied by a report generator agreeing with
// itself. This one drives the command end to end over evidence a real CVM
// minted, and pins the two values the whole command exists to produce: the
// compose-hash the replay recovers, and the skeleton digest of the document
// that hash is over.
func TestTeeInspect_AgainstTheRealCapture(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))

	out, err := runTeeInspect(t, srv.URL)
	if err != nil {
		t.Fatalf("tee inspect: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	if !strings.Contains(out, "compose-hash gate:  PASS") {
		t.Fatalf("the gate did not pass on a document that IS the measured one:\n%s", out)
	}
	if !strings.Contains(out, capturedComposeHash) {
		t.Errorf("report does not carry the replayed compose-hash %s:\n%s", capturedComposeHash, out)
	}

	// Computed by calling the shared extractor directly, NOT restated as a
	// literal. The point of the assertion is that the command reports what
	// attest computes rather than a second implementation of its own; a
	// literal here would be a value only this file believes.
	skel, imageRef, serr := attest.ExtractComposeSkeleton(teeCaptureDockerCompose(t))
	if serr != nil {
		t.Fatalf("the capture's compose does not extract: %v", serr)
	}
	wantDigest := attest.ComposeSkeletonDigest(skel)
	if !strings.Contains(out, wantDigest) {
		t.Errorf("report does not carry the skeleton digest %s:\n%s", wantDigest, out)
	}
	if !strings.Contains(out, imageRef) {
		t.Errorf("report does not carry the image reference %s:\n%s", imageRef, out)
	}

	// The capture is a NEGATIVE fixture — `phala deploy` appended the
	// deploying machine's SSH key as DSTACK_AUTHORIZED_KEYS. A report that
	// found nothing wrong with it would mean the shared policy never ran.
	if !strings.Contains(out, string(attest.ViolationRootBackdoorEnv)) {
		t.Errorf("the root back door in the captured compose is not reported:\n%s", out)
	}
}

// teeCaptureDockerCompose pulls docker_compose_file out of the captured
// document, so the expected skeleton is derived from the same bytes the
// command read rather than from a transcription of them.
func teeCaptureDockerCompose(t *testing.T) string {
	t.Helper()
	var doc struct {
		DockerCompose string `json:"docker_compose_file"`
	}
	if err := json.Unmarshal([]byte(teeCaptureAppCompose(t)), &doc); err != nil {
		t.Fatalf("parse app_compose: %v", err)
	}
	return doc.DockerCompose
}

// releaseCompose is the shape zs-node's released dstack compose template
// produces, byte-identical to the `release` skeleton vector in
// proto/testdata/compose_vectors.json.
//
// A COPY, AND A SELF-CHECKING ONE. proxy/ cannot read proto/testdata — an
// out-of-workspace CI build clones this repo alone — so the text is duplicated
// here, and a duplicated fixture is normally how two files start quietly
// describing different things. It cannot happen here: the digest asserted
// against it is goSkeletonDigestFixture, a value neither this file nor this
// repo computes. Drift in either direction turns the test red.
const releaseCompose = "services:\n" +
	"  zs-node:\n" +
	"    image: ghcr.io/txnlab/zs-node@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111\n" +
	"    volumes:\n" +
	"      - /var/run/dstack.sock:/var/run/dstack.sock\n" +
	"      - zs-node-state:/data\n" +
	"    ports:\n" +
	"      - \"9090:9090\"\n" +
	"    environment:\n" +
	"      NODE_CONFIG_YAML: |\n" +
	"        zs:\n" +
	"          operator_id: 1\n" +
	"          node_id: 2\n" +
	"      NODE_LLM_OPENAI_API_KEY: ${NODE_LLM_OPENAI_API_KEY}\n" +
	"    restart: always\n" +
	"volumes:\n" +
	"  zs-node-state: {}\n"

// goSkeletonDigest is the digest proto/go/attest's TestSkeletonDigest_GoTSParity
// and proto/ts's attest-compose.test.ts both pin over releaseCompose.
//
// It is the one value in this file that comes from outside it, which is what
// makes the assertion mean something: if the command's skeleton digest is not
// this, then the value an operator pastes into trusted_compose_skeletons is
// not the value the browser client would compute for the same node — the
// silent Go↔TS split proto/SPEC.md §3e is written to prevent.
const goSkeletonDigest = "b66a7d03d5c42a4094a9d0042bd6950ee9304d4837b9fe75c447a699115e7ad6"

// TestTeeInspect_ReleaseSkeletonMatchesTheGoTSPin drives the release-list
// generation path: a published compose, before any node has run it.
func TestTeeInspect_ReleaseSkeletonMatchesTheGoTSPin(t *testing.T) {
	path := writeTeeTempFile(t, "docker-compose.yaml", releaseCompose)

	out, err := runTeeInspect(t, "--file", path)
	if err != nil {
		t.Fatalf("tee inspect --file: %v\n%s", err, out)
	}
	if !strings.Contains(out, goSkeletonDigest) {
		t.Errorf("skeleton digest is not the Go↔TS pinned %s:\n%s", goSkeletonDigest, out)
	}
	// Raw compose text carries none of the fields the content rules read, so
	// reporting an empty violation list over it would read as a pass on a
	// document nothing examined.
	if strings.Contains(out, "violations:    none") {
		t.Errorf("raw docker-compose text was reported as having no violations, which "+
			"reads as a pass on rules that never ran:\n%s", out)
	}
	if !strings.Contains(out, "DID NOT RUN") {
		t.Errorf("the report does not say the content rules were skipped:\n%s", out)
	}
}

// TestTeeInspect_FileModeSaysItVerifiedNothing covers the output most likely
// to be misread as a verdict.
//
// A document read off disk has no replay to be checked against, so the gate is
// unrunnable — and the values below it are then facts about a file that any
// node could have published. The banner is the only thing separating this
// report from the node one, so its absence is a defect even though every value
// printed is correct.
func TestTeeInspect_FileModeSaysItVerifiedNothing(t *testing.T) {
	path := writeTeeTempFile(t, "app-compose.json", teeCaptureAppCompose(t))

	out, err := runTeeInspect(t, "--file", path)
	if err != nil {
		t.Fatalf("tee inspect --file: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOT VERIFIED") {
		t.Errorf("file mode did not say it verified nothing:\n%s", out)
	}
	if strings.Contains(out, "compose-hash gate") {
		t.Errorf("file mode printed a hash-gate verdict, which it cannot have reached:\n%s", out)
	}
	// The policy still ran, or the banner would be the whole output.
	if !strings.Contains(out, string(attest.ViolationRootBackdoorEnv)) {
		t.Errorf("file mode ran no content rules:\n%s", out)
	}
	// A local document has no bundle, catalog or quote to describe.
	if strings.Contains(out, "published models") {
		t.Errorf("file mode printed a published-models section:\n%s", out)
	}
	jsonOut, err := runTeeInspect(t, "--file", path, "--json")
	if err != nil {
		t.Fatalf("tee inspect --file --json: %v\n%s", err, jsonOut)
	}
	if strings.Contains(jsonOut, "quote_readable") || strings.Contains(jsonOut, "aux_binding_present") {
		t.Errorf("file mode --json reports on a quote it never had:\n%s", jsonOut)
	}
}

// TestTeeInspect_SubstitutedDocumentStopsAtTheGate is the ordering rule from
// proto/SPEC.md §3e made observable.
//
// A document that is not the measured one describes no deployment, so the
// report must not go on to print a skeleton digest from it — that value is
// exactly what someone would paste into trusted_compose_skeletons, and it
// would bless a document the hardware never saw.
func TestTeeInspect_SubstitutedDocumentStopsAtTheGate(t *testing.T) {
	bundle := teeCaptureBundle(t)
	// Policy-clean, well-formed, and not the measured document.
	bundle.AppCompose = `{"manifest_version":2,"runner":"docker-compose",` +
		`"docker_compose_file":"services:\n  zs-node:\n    image: ghcr.io/txnlab/zs-node@sha256:00\n"}`
	srv := teeCaptureNode(t, bundle)

	out, err := runTeeInspect(t, srv.URL)
	// Exit 0: this is a diagnostic, and one that exits non-zero on a finding
	// gets wrapped in `|| true` and then reports nothing.
	if err != nil {
		t.Fatalf("a failed gate must still exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "compose-hash gate:  FAIL") {
		t.Fatalf("a substituted document passed the gate:\n%s", out)
	}
	if strings.Contains(out, "trusted_compose_skeletons:") {
		t.Errorf("a skeleton digest was printed from an unverified document:\n%s", out)
	}
	// The capture is pre-9.9, but a payer refuses this bundle at the compose
	// gate and never reads the upper half, so the report must not say it would.
	if strings.Contains(out, "aux_binding_absent") {
		t.Errorf("a stopped report predicted a refusal the payer never reaches:\n%s", out)
	}
}

// TestTeeInspect_ChainIsOptIn pins the flag's default rather than its
// behaviour: the chain check reaches Intel's PCS, so a run that made it
// mandatory would fail whenever Intel is having a bad day, in a command whose
// job is offline arithmetic over a document. Nothing else in this file would
// notice the default flipping — the capture verifies without it.
func TestTeeInspect_ChainIsOptIn(t *testing.T) {
	f := newTeeInspectCmd().Flags().Lookup("chain")
	if f == nil {
		t.Fatal("no --chain flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--chain defaults to %q, want false — the quote chain needs Intel PCS "+
			"and must never be on the offline path", f.DefValue)
	}
	// And it is refused rather than ignored on a path with no quote: a --chain
	// that quietly did nothing would leave someone believing a file had been
	// checked against Intel.
	path := writeTeeTempFile(t, "docker-compose.yaml", releaseCompose)
	if _, err := runTeeInspect(t, "--file", path, "--chain"); err == nil {
		t.Error("--chain on --file exited 0; there is no quote in a file to verify")
	}
}

// TestTeeInspect_RequiresExactlyOneSource: with neither a URL nor --file there
// is nothing to report on, and with both it is ambiguous which one the report
// describes. Both are usage errors rather than reports, so unlike a finding
// they DO exit non-zero.
func TestTeeInspect_RequiresExactlyOneSource(t *testing.T) {
	cases := [][]string{
		{},
		{"https://example.invalid", "--file", "x.json"},
	}
	for _, args := range cases {
		if _, err := runTeeInspect(t, args...); err == nil {
			t.Errorf("tee inspect %v exited 0, want a usage error", args)
		}
	}
}

// TestTeeInspect_JSONCarriesTheAllowlistValues is what a release-list
// generator would consume. Field names are the contract; a rename that only
// broke the text output would be caught by nothing else.
func TestTeeInspect_JSONCarriesTheAllowlistValues(t *testing.T) {
	path := writeTeeTempFile(t, "docker-compose.yaml", releaseCompose)

	out, err := runTeeInspect(t, "--file", path, "--json")
	if err != nil {
		t.Fatalf("tee inspect --json: %v\n%s", err, out)
	}
	var rep struct {
		Verified  bool   `json:"verified"`
		Skeletons string `json:"trusted_compose_skeletons_value"`
		Images    string `json:"trusted_node_images_value"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
	}
	if rep.Skeletons != goSkeletonDigest {
		t.Errorf("trusted_compose_skeletons_value = %q, want %q", rep.Skeletons, goSkeletonDigest)
	}
	if !strings.HasPrefix(rep.Images, "ghcr.io/txnlab/zs-node@sha256:") {
		t.Errorf("trusted_node_images_value = %q, want the release image reference", rep.Images)
	}
	// The one field a script should branch on. File mode verified nothing, so
	// a consumer that treats this report as evidence is doing so against an
	// explicit false rather than against silence.
	if rep.Verified {
		t.Error("verified is true for a document read off disk, which was checked against nothing")
	}
}

// TestTeeInspect_ForgedQuoteYieldsNoAllowlistValues is the one that matters
// most, and it was missing.
//
// attest.VerifyEventLog reads RTMR3 out of the quote BYTES; nothing in the
// replay authenticates them. A node serving fabricated bytes with an RTMR3 of
// its own choosing, an event log that replays to it, and a matching
// app_compose satisfies the replay and the hash gate perfectly. If the report
// went on to print a skeleton digest and an image reference from that, the
// operator pastes an attacker-chosen deployment shape into
// trusted_compose_skeletons and blesses it fleet-wide — using the exact
// workflow the command's own help text prescribes.
//
// So a chain FAIL must stop the report, not annotate it.
func TestTeeInspect_ForgedQuoteYieldsNoAllowlistValues(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	forged := func(context.Context, []byte) error { return errors.New("tdx quote verification: bad signature") }

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), forged, srv.URL, "--chain")
	// Still a report, so still exit 0.
	if err != nil {
		t.Fatalf("a failed chain must still exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "quote chain:        FAIL") {
		t.Fatalf("the chain failure is not reported:\n%s", out)
	}
	// The capture's own event log DOES replay to its own quote, so without the
	// gate these values would print — which is what makes this assertion able
	// to fail rather than passing on a document that yields nothing anyway.
	if strings.Contains(out, "trusted_compose_skeletons:") {
		t.Errorf("allowlist values were printed from a quote that attests nothing:\n%s", out)
	}
	if strings.Contains(out, "compose-hash gate") {
		t.Errorf("the compose gate ran on a forged quote's measurements:\n%s", out)
	}
}

// The precondition for the test above: with the chain accepted, the very same
// bundle DOES print those values. Without this, the assertion above would pass
// against a command that never printed them at all.
func TestTeeInspect_VerifiedRequiresTheChain(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	good := func(context.Context, []byte) error { return nil }

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), good, srv.URL, "--chain", "--json")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	var rep struct {
		Verified  bool   `json:"verified"`
		Skeletons string `json:"trusted_compose_skeletons_value"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
	}
	if !rep.Verified {
		t.Error("verified is false with the chain passing, the log replaying and the gate passing")
	}
	if rep.Skeletons == "" {
		t.Error("no skeleton digest on a fully verified bundle, so the negative test above " +
			"proves nothing")
	}
}

// A debug TD passes the chain, the replay and the gate, so only the attribute
// check stands between it and a printed skeleton digest. The control is
// TestTeeInspect_VerifiedRequiresTheChain: the same run without the bit
// verifies and prints one.
func TestTeeInspect_DebugTDStopsTheReport(t *testing.T) {
	bundle := teeCaptureBundle(t)
	quote, err := base64.StdEncoding.DecodeString(bundle.CPUReport)
	if err != nil {
		t.Fatal(err)
	}
	attrs, ok := attest.QuoteTDAttributes(quote)
	if !ok {
		t.Fatal("the capture has no TDATTRIBUTES")
	}
	attrs[0] |= 0x01
	bundle.CPUReport = base64.StdEncoding.EncodeToString(quote)
	srv := teeCaptureNode(t, bundle)
	good := func(context.Context, []byte) error { return nil }

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), good, srv.URL, "--chain", "--json")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	var rep struct {
		Verified  bool   `json:"verified"`
		Stop      string `json:"stop"`
		Skeletons string `json:"trusted_compose_skeletons_value"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
	}
	if rep.Verified || rep.Skeletons != "" {
		t.Errorf("a debug TD reported verified=%v skeleton=%q", rep.Verified, rep.Skeletons)
	}
	if !strings.Contains(rep.Stop, "debug TD") {
		t.Errorf("stop = %q, want the debug-TD refusal", rep.Stop)
	}
}

// A trusted os-image-hash over another kernel's boot registers is what a
// forged RTMR3 looks like, so the report stops before printing trust values.
func TestTeeInspect_ForeignBootChainStopsTheReport(t *testing.T) {
	bundle := teeCaptureBundle(t)
	quote, err := base64.StdEncoding.DecodeString(bundle.CPUReport)
	if err != nil {
		t.Fatal(err)
	}
	rtmr1, ok := attest.QuoteRTMR(quote, 1)
	if !ok {
		t.Fatal("the capture has no RTMR1")
	}
	rtmr1[0] ^= 1
	bundle.CPUReport = base64.StdEncoding.EncodeToString(quote)
	srv := teeCaptureNode(t, bundle)
	good := func(context.Context, []byte) error { return nil }

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), good, srv.URL, "--chain", "--json")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	var rep struct {
		Verified  bool   `json:"verified"`
		Stop      string `json:"stop"`
		Skeletons string `json:"trusted_compose_skeletons_value"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
	}
	if rep.Verified || rep.Skeletons != "" {
		t.Errorf("a foreign boot chain reported verified=%v skeleton=%q", rep.Verified, rep.Skeletons)
	}
	if !strings.Contains(rep.Stop, "RTMR1") {
		t.Errorf("stop = %q, want the boot-chain refusal naming RTMR1", rep.Stop)
	}
}

// withOSImageClaim rewrites the os-image-hash event in b's log and writes the
// edited log's replay into the quote's RTMR3, leaving the boot registers the
// capture's.
func withOSImageClaim(t *testing.T, b inject.TEEEvidenceBundle, osImage string) inject.TEEEvidenceBundle {
	t.Helper()
	var events []map[string]any
	if err := json.Unmarshal([]byte(b.EventLog), &events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e["event"] == attest.EventOSImageHash {
			e["event_payload"] = osImage
		}
	}
	log, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := attest.ParseEventLog(string(log))
	if err != nil {
		t.Fatal(err)
	}
	rtmr3, _, err := attest.ReplayRTMR3(parsed)
	if err != nil {
		t.Fatal(err)
	}
	quote, err := base64.StdEncoding.DecodeString(b.CPUReport)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := attest.QuoteRTMR(quote, 3)
	if !ok {
		t.Fatal("the capture's quote has no RTMR3")
	}
	copy(reg, rtmr3)
	b.CPUReport = base64.StdEncoding.EncodeToString(quote)
	b.EventLog = string(log)
	return b
}

// A listed image with no measured row is a gap in our table, not a forgery,
// and the report must say which so the operator is not sent hunting for one.
// An unlisted image with no row is annotated, not stopped, but never verified.
func TestTeeInspect_UnmeasuredBootChain(t *testing.T) {
	good := func(context.Context, []byte) error { return nil }
	type report struct {
		Verified       bool   `json:"verified"`
		Stop           string `json:"stop"`
		Skeletons      string `json:"trusted_compose_skeletons_value"`
		OSImageTrusted *bool  `json:"os_image_on_trusted_list"`
	}
	run := func(osImage string) report {
		srv := teeCaptureNode(t, withOSImageClaim(t, teeCaptureBundle(t), osImage))
		out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), good, srv.URL, "--chain", "--json")
		if err != nil {
			t.Fatalf("tee inspect --chain: %v\n%s", err, out)
		}
		var rep report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("--json did not emit JSON: %v\n%s", err, out)
		}
		return rep
	}

	// dstack-nvidia-0.5.12: on the shipped list, no measured row.
	rep := run("91fd5d69ccee3e5ea4f2aeb8c9b632c4358c4ecfc29ba1aa6fb1e402af209829")
	if rep.Verified || rep.Skeletons != "" {
		t.Errorf("unmeasured image reported verified=%v skeleton=%q", rep.Verified, rep.Skeletons)
	}
	if !strings.Contains(rep.Stop, "no measured boot chain") || !strings.Contains(rep.Stop, "RTMR0 ") {
		t.Errorf("stop = %q, want the unmeasured-image message with the registers", rep.Stop)
	}
	if strings.Contains(rep.Stop, "not necessarily written") {
		t.Errorf("stop = %q reads as a forgery for a table gap", rep.Stop)
	}
	// The printed registers become a table row, so each must sit under its
	// own label: a swap would ship a row that refuses every node.
	quote, err := base64.StdEncoding.DecodeString(teeCaptureBundle(t).CPUReport)
	if err != nil {
		t.Fatal(err)
	}
	mrtd, _ := attest.QuoteMRTD(quote)
	want := []string{"MRTD " + hex.EncodeToString(mrtd)}
	for n := range 3 {
		r, _ := attest.QuoteRTMR(quote, n)
		want = append(want, fmt.Sprintf("RTMR%d %s", n, hex.EncodeToString(r)))
	}
	for _, w := range want {
		if !strings.Contains(rep.Stop, w) {
			t.Errorf("stop = %q, want it to carry %q", rep.Stop, w)
		}
	}

	rep = run(strings.Repeat("ab", 32))
	if rep.Stop != "" {
		t.Errorf("an unlisted image stopped the report: %q", rep.Stop)
	}
	if rep.OSImageTrusted == nil || *rep.OSImageTrusted {
		t.Errorf("os_image_trusted = %v, want false", rep.OSImageTrusted)
	}
	if rep.Verified {
		t.Error("an image with no measured boot chain reported verified")
	}
}

// Without --chain nothing authenticated the quote, so `verified` must be
// false and the report must SAY the check did not run.
//
// Silence is the failure mode being guarded: an absent chain line is
// indistinguishable from a pass, and `verified: true` here would be true for
// the forgery in the test above.
func TestTeeInspect_WithoutChainIsNotVerifiedAndSaysSo(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))

	out, err := runTeeInspect(t, srv.URL)
	if err != nil {
		t.Fatalf("tee inspect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "quote chain:        NOT CHECKED") {
		t.Errorf("a run without --chain does not say the chain was unchecked:\n%s", out)
	}

	jsonOut, err := runTeeInspect(t, srv.URL, "--json")
	if err != nil {
		t.Fatalf("tee inspect --json: %v\n%s", err, jsonOut)
	}
	var rep struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("--json did not emit JSON: %v\n%s", err, jsonOut)
	}
	if rep.Verified {
		t.Error("verified is true without the quote's signature ever being examined")
	}
}

// An Intel outage is OUR dependency failing, not an accusation about this
// node — so unlike a FAIL it must not stop the report, and unlike a PASS it
// must not set verified. The two are deliberately not complements.
func TestTeeInspect_CollateralUnreachableStillReports(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	down := func(context.Context, []byte) error {
		return fmt.Errorf("%w: dial tcp: i/o timeout", tee.ErrCollateralUnavailable)
	}

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), down, srv.URL, "--chain")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOT CHECKED — Intel PCS was unreachable") {
		t.Errorf("an Intel outage is not distinguished from a bad quote:\n%s", out)
	}
	if !strings.Contains(out, "trusted_compose_skeletons:") {
		t.Errorf("the report went dark during an Intel outage, which is exactly when an "+
			"operator needs it:\n%s", out)
	}
}

// A REFUSAL reached on cached collateral must STOP the report — the other
// side of the pair above, and the distinction the two tags exist for.
//
// "Intel was unreachable" and "cached collateral was consulted and it refused"
// are opposite facts about whether a verdict was reached, so the second may not
// borrow the first's continue-anyway treatment. What continuing costs is
// specific: this command's most dangerous output is the release-allowlist
// values, which an operator pastes into their config, and the replay that
// produces them reads RTMR3 out of the quote and authenticates nothing. So a
// quote that was refused must never reach the part of the report that names
// its own trust values.
func TestTeeInspect_StaleCollateralRejectionStopsTheReport(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	refused := func(context.Context, []byte) error {
		return fmt.Errorf("%w: tdx quote verification: bad signature (Intel PCS was unreachable: dial tcp: i/o timeout)",
			tee.ErrStaleCollateralRejected)
	}

	out, err := runTeeInspectWith(t, filepath.Join(t.TempDir(), "absent.yaml"), refused, srv.URL, "--chain")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("a quote refused by cached collateral was not reported as a failure:\n%s", out)
	}
	if strings.Contains(out, "NOT CHECKED") {
		t.Errorf("a refusal was reported as though nothing had been checked:\n%s", out)
	}
	if strings.Contains(out, "trusted_compose_skeletons:") || strings.Contains(out, "trusted_node_images:") {
		t.Errorf("the report printed the allowlist values a REFUSED quote names for itself, "+
			"which is the value an operator pastes into their config:\n%s", out)
	}
	// The cause is genuinely undetermined, so the operator must not be told
	// their node is at fault when our own document may be the stale one.
	if !strings.Contains(out, "cause cannot be determined") {
		t.Errorf("the refusal reads as an accusation against the node, but Intel being "+
			"unreachable means our own document may be the superseded one:\n%s", out)
	}
}

// A pass reached on cached collateral is still a PASS, but it must not read as
// confirmation that Intel's PCS is reachable — it is the opposite, and this
// command is the one an operator runs to find that out.
//
// The failure this guards is silent by construction: the verdict is correct,
// the quote really did verify, and only the sentence is wrong.
func TestTeeInspect_StaleCollateralIsPassButSaysSo(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	ok := func(context.Context, []byte) error { return nil }

	out, err := runTeeInspectStale(
		t, filepath.Join(t.TempDir(), "absent.yaml"), ok, true, srv.URL, "--chain")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, out)
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("a stale-collateral verification was not reported as a pass:\n%s", out)
	}
	if !strings.Contains(out, "cached revocation/TCB collateral") {
		t.Errorf("the report claims fresh collateral for a verdict that used the cache:\n%s", out)
	}

	// And the fresh path must NOT carry the caveat, or it stops meaning anything.
	fresh, err := runTeeInspectStale(
		t, filepath.Join(t.TempDir(), "absent.yaml"), ok, false, srv.URL, "--chain")
	if err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, fresh)
	}
	if strings.Contains(fresh, "cached revocation/TCB collateral") {
		t.Errorf("a fresh verification carries the stale caveat:\n%s", fresh)
	}
}

// The third source, and the one this command must never report as the first
// two.
//
// A pass reached on the node's own collateral IS a pass — Intel signed every
// document and the library validated them against its embedded Intel root — so
// the report continues and the allowlist values are legitimate. What it is not
// is the same statement: the attested node chose which validly-signed vintage
// to hand over, so a revocation or TCB downgrade published since could be
// absent. Both halves are asserted here, because either alone passes for a
// wrong implementation: report it as "NOT CHECKED" and an operator loses the
// values during exactly the outage this path exists for; report it as an
// ordinary chain pass and they never learn to fix their egress.
func TestTeeInspect_CarriedCollateralPassNamesItsSource(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	cmd, f := teeInspectCmd()
	f.verifyQuoteChain = func(context.Context, []byte) (bool, error) {
		return false, fmt.Errorf("%w: dial tcp: i/o timeout", tee.ErrCollateralUnavailable)
	}
	carriedCalled := false
	f.verifyCarriedChain = func(context.Context, []byte, *inject.TEECollateral) error {
		carriedCalled = true
		return nil
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "absent.yaml"), srv.URL, "--chain"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tee inspect --chain: %v\n%s", err, buf.String())
	}
	out := buf.String()

	if !carriedCalled {
		t.Fatal("the node's own collateral was never tried, so an Intel outage still " +
			"leaves the operator with no verdict")
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("a verification that succeeded was not reported as a pass:\n%s", out)
	}
	if !strings.Contains(out, "collateral THIS NODE supplied") {
		t.Errorf("the report does not say the collateral came from the node being "+
			"attested, which is the only thing distinguishing this from a full check:\n%s", out)
	}
	if !strings.Contains(out, "trusted_compose_skeletons:") {
		t.Errorf("the report stopped on a PASS:\n%s", out)
	}
}

// The fallback is for an outage, not for a verdict. A chain that RAN and
// refused must not get a second opinion from the party it refused — that would
// let any node overturn its own rejection by shipping a friendlier vintage.
func TestTeeInspect_CarriedCollateralIsNotASecondOpinion(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))
	for name, chainErr := range map[string]error{
		"a plain chain failure": errors.New("signature mismatch"),
		"a stale-backed refusal": fmt.Errorf("%w: crl expired",
			tee.ErrStaleCollateralRejected),
	} {
		t.Run(name, func(t *testing.T) {
			cmd, f := teeInspectCmd()
			f.verifyQuoteChain = func(context.Context, []byte) (bool, error) {
				return false, chainErr
			}
			f.verifyCarriedChain = func(context.Context, []byte, *inject.TEECollateral) error {
				t.Error("the node's own collateral was consulted about a verdict that " +
					"had already been reached")
				return nil
			}
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "absent.yaml"), srv.URL, "--chain"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("tee inspect --chain: %v\n%s", err, buf.String())
			}
			out := buf.String()
			if !strings.Contains(out, "FAIL") {
				t.Errorf("a refused quote was not reported as a failure:\n%s", out)
			}
			if strings.Contains(out, "trusted_compose_skeletons:") {
				t.Errorf("a refused quote named its own allowlist values:\n%s", out)
			}
		})
	}
}

// The report must annotate the rules that ACTUALLY GATE, and stay silent about
// one that does not apply.
//
// This inverted with the release lists. The compose-hash pin used to have no
// default and to refuse first, so it was the reason every dstack node was
// dropped and annotating it was the whole point. Now it ships empty while the
// release match ships on, so the verifier SKIPS it — and printing
// `[NOT on zs.tee.trusted_compose_hashes]` beside a healthy node is a
// refusal-shaped line for a list nothing reads, in the one command whose job is
// answering "why am I being refused".
//
// Three verdicts in one run — one positive, one negative, one absent — which is
// what keeps this from passing on a hardcoded note.
func TestTeeInspect_AnnotatesTheRulesThatGate(t *testing.T) {
	srv := teeCaptureNode(t, teeCaptureBundle(t))

	out, err := runTeeInspect(t, srv.URL)
	if err != nil {
		t.Fatalf("tee inspect: %v\n%s", err, out)
	}
	// The OS-image preset ships and the capture matches it.
	if !strings.Contains(out, "[on zs.tee.trusted_os_images]") {
		t.Errorf("os-image-hash carries no allowlist verdict:\n%s", out)
	}
	// The release lists ship and the capture is NOT a published release, so
	// this is the rule that refuses it — and the one an operator must read.
	if !strings.Contains(out, "[NOT on zs.tee.trusted_compose_skeletons]") {
		t.Errorf("the skeleton digest carries no verdict, so the report is silent about the "+
			"rule that actually refuses this node:\n%s", out)
	}
	// And the pin the verifier skipped is not reported as a failure.
	if strings.Contains(out, "zs.tee.trusted_compose_hashes]") {
		t.Errorf("the compose-hash pin was annotated even though the verifier skips it, "+
			"which reads as a refusal for a list nothing consults:\n%s", out)
	}
}

// The report must say when a node records an unencrypted data disk, because
// every payer refuses it and nothing else in the report would. The capture is
// dstack-0.5.9 and records nothing, so the event is appended and its replay
// written into RTMR3; without --chain nothing authenticates the quote, which is
// what makes that edit reachable here. The "31" row is the control.
func TestTeeInspect_AnnotatesAnUnencryptedDataDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		// payload is appended as a storage-encrypted event; nil leaves the
		// capture as minted, which is every 0.5.x node.
		payload *string
		// want is the JSON verdict; nil means no verdict was recorded.
		want *bool
		note string
	}{
		{"absent", nil, nil, ""},
		{"encrypted", new("31"), new(true), "[encrypted]"},
		{"unencrypted", new("30"), new(false), "[REFUSED — the data disk is not recorded as encrypted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := teeCaptureBundle(t)
			if tc.payload != nil {
				bundle = withStorageEvent(t, bundle, *tc.payload)
			}
			srv := teeCaptureNode(t, bundle)
			out, err := runTeeInspect(t, srv.URL, "--json")
			if err != nil {
				t.Fatalf("tee inspect: %v\n%s", err, out)
			}
			var rep teeReport
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("decode report: %v\n%s", err, out)
			}
			// An absent record must stay nil: true would claim an encryption
			// verdict for a node that recorded nothing.
			switch {
			case (rep.StorageEncrypted == nil) != (tc.want == nil):
				t.Fatalf("storage_encrypted = %v, want %v", rep.StorageEncrypted, tc.want)
			case tc.want != nil && *rep.StorageEncrypted != *tc.want:
				t.Fatalf("storage_encrypted = %v, want %v", *rep.StorageEncrypted, *tc.want)
			}

			text, err := runTeeInspect(t, srv.URL)
			if err != nil {
				t.Fatalf("tee inspect: %v\n%s", err, text)
			}
			line := regexp.MustCompile(`(?m)^  storage-encrypted:.*$`).FindString(text)
			switch {
			case tc.payload == nil && line != "":
				t.Errorf("a storage-encrypted line for a node that recorded none: %q", line)
			case tc.payload != nil && !strings.HasSuffix(line, tc.note):
				t.Errorf("storage-encrypted line = %q, want it to end %q:\n%s", line, tc.note, text)
			}
		})
	}
}

// withStorageEvent appends a storage-encrypted runtime event to b's log and
// writes the edited log's replay into the quote's RTMR3.
func withStorageEvent(t *testing.T, b inject.TEEEvidenceBundle, payload string) inject.TEEEvidenceBundle {
	t.Helper()
	var events []map[string]any
	if err := json.Unmarshal([]byte(b.EventLog), &events); err != nil {
		t.Fatal(err)
	}
	events = append(events, map[string]any{
		"imr": 3, "event_type": attest.DstackRuntimeEventType, "digest": "",
		"event": attest.EventStorageEncrypted, "event_payload": payload,
	})
	log, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := attest.ParseEventLog(string(log))
	if err != nil {
		t.Fatal(err)
	}
	rtmr3, _, err := attest.ReplayRTMR3(parsed)
	if err != nil {
		t.Fatal(err)
	}
	quote, err := base64.StdEncoding.DecodeString(b.CPUReport)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := attest.QuoteRTMR(quote, 3)
	if !ok {
		t.Fatal("the capture's quote has no RTMR3")
	}
	copy(reg, rtmr3)
	b.CPUReport = base64.StdEncoding.EncodeToString(quote)
	b.EventLog = string(log)
	return b
}

// A log that replays but carries no compose-hash event is a state SPEC §3e
// names, and it is not document substitution. Reporting it as substitution
// sends an operator looking for a document problem they do not have.
// The three causes must produce three DISTINCT messages. Reaching each one
// through a real bundle would take a forged quote, which is exactly why they
// went untested while inline — and why the mapping is a function.
func TestComposeHashStop_NamesEachCauseDistinctly(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"no compose-hash event": {attest.ErrNoComposeHash, "no compose-hash event"},
		"no preimage":           {attest.ErrNoAppCompose, "publishes no app_compose"},
		"substituted":           {errors.New("digest mismatch"), "not the one this quote measured"},
	}
	seen := map[string]string{}
	for name, tc := range cases {
		got := composeHashStop(tc.err)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: message %q does not name its cause (%q)", name, got, tc.want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s produce the same message, so the report cannot tell an "+
				"operator which of them happened", name, prev)
		}
		seen[got] = name
	}
}

// The release lists must reach the CLI's report from config, and an unlisted
// skeleton must produce a real compose_skeleton violation through the command
// — otherwise `release match ENFORCED` is a label over a check that never ran.
func TestTeeInspect_ConfiguredReleaseListsReachTheReport(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := `
algod:
  network: testnet
zs:
  escrow_app_id: 1
  tee:
    enforce_compose_skeleton: true
    trusted_compose_skeletons:
      - "` + strings.Repeat("a", 64) + `"
    trusted_node_images:
      - "ghcr.io/txnlab/zs-node@sha256:` + strings.Repeat("b", 64) + `"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker-compose.json")
	doc := `{"manifest_version":2,"runner":"docker-compose","docker_compose_file":` +
		mustJSONString(t, releaseCompose) + `}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runTeeInspectWith(t, cfgPath, nil, "--file", path)
	if err != nil {
		t.Fatalf("tee inspect --file: %v\n%s", err, out)
	}
	if !strings.Contains(out, "release match ENFORCED") {
		t.Errorf("configured release lists did not turn enforcement on in the report:\n%s", out)
	}
	if !strings.Contains(out, string(attest.ViolationComposeSkeleton)) {
		t.Errorf("a skeleton absent from the configured list produced no compose_skeleton "+
			"violation, so enforcement is a label over a check that never ran:\n%s", out)
	}
	if !strings.Contains(out, string(attest.ViolationNodeImage)) {
		t.Errorf("an image absent from the configured list produced no node_image violation:\n%s", out)
	}
	// And the misconfigured state the derivation exists to prevent must not
	// appear when both lists are populated.
	if strings.Contains(out, string(attest.ViolationPolicyMisconfigured)) {
		t.Errorf("populated lists reported policy_misconfigured:\n%s", out)
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// teeCaptureWithAux returns the capture with an aux binding spliced into the
// upper half of report_data, making it a 9.9 quote. The real capture is
// pre-9.9 (zero upper half). The splice goes through attest.QuoteReportData,
// which returns a subslice of the quote, so there is no offset literal here.
//
// The spliced quote's signature no longer verifies. `tee inspect` only reads
// whether the upper half is zero, with no chain check unless --chain is set.
func teeCaptureWithAux(t *testing.T, models []inject.ModelEntry) inject.TEEEvidenceBundle {
	t.Helper()
	b := teeCaptureBundle(t)
	quote, err := base64.StdEncoding.DecodeString(b.CPUReport)
	if err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	hApp, err := attest.HApp(models)
	if err != nil {
		t.Fatalf("HApp: %v", err)
	}
	hPosture, err := attest.HPosture(b.Posture)
	if err != nil {
		t.Fatalf("HPosture: %v", err)
	}
	aux := attest.AuxBinding(1, hApp, hPosture, nil)
	rd, ok := attest.QuoteReportData(quote)
	if !ok {
		t.Fatal("capture has no report_data")
	}
	copy(rd[32:], aux[:])
	b.CPUReport = base64.StdEncoding.EncodeToString(quote)
	b.AppModels = models
	return b
}

// TestTeeInspect_AttestedModelSection covers the published-model section.
//
// Every case goes through the real HTTP handler. `app_models` is `omitempty`,
// so an empty catalog marshals to no key, the same as a pre-9.9 node; setting
// AppModels in-process would skip that.
func TestTeeInspect_AttestedModelSection(t *testing.T) {
	models := []inject.ModelEntry{
		{ModelID: "qwen3-30b", Source: "hf/Qwen/Qwen3-30B", WeightsDigest: "sha256:aa11", State: inject.WeightsMeasured},
		{ModelID: "grok-4", State: inject.WeightsUnverifiable},
	}

	t.Run("lists the published catalog", func(t *testing.T) {
		out, err := runTeeInspect(t, teeCaptureNode(t, teeCaptureWithAux(t, models)).URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		for _, want := range []string{"qwen3-30b", "hf/Qwen/Qwen3-30B", "sha256:aa11", "grok-4",
			"measured", "unverifiable"} {
			if !strings.Contains(out, want) {
				t.Errorf("report omits %q:\n%s", want, out)
			}
		}
		// string(WeightsState) once printed a control character here.
		if strings.ContainsAny(out, "\x00\x01\x02\x03") {
			t.Errorf("report contains a control character:\n%q", out)
		}
		// This run cannot recompute the binding, so the list must say so.
		if !strings.Contains(out, "NOT VERIFIED") {
			t.Errorf("the model section is not labelled as unverified:\n%s", out)
		}
		if !strings.Contains(out, "upper 32 bytes are populated") {
			t.Errorf("a non-empty list omits the report_data state:\n%s", out)
		}
	})

	t.Run("--json names the list as published, not attested", func(t *testing.T) {
		out, err := runTeeInspect(t, teeCaptureNode(t, teeCaptureWithAux(t, models)).URL, "--json")
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		var got struct {
			PublishedModels   []map[string]any `json:"published_models"`
			AuxBindingPresent *bool            `json:"aux_binding_present"`
			QuoteReadable     *bool            `json:"quote_readable"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode --json: %v\n%s", err, out)
		}
		if len(got.PublishedModels) != 2 || got.PublishedModels[0]["model_id"] == nil {
			t.Errorf("published_models = %v", got.PublishedModels)
		}
		if got.AuxBindingPresent == nil || !*got.AuxBindingPresent {
			t.Errorf("aux_binding_present missing or false:\n%s", out)
		}
		if got.QuoteReadable == nil || !*got.QuoteReadable {
			t.Errorf("quote_readable missing or false:\n%s", out)
		}
		if strings.Contains(out, "attested_models") {
			t.Errorf("--json still uses attested_models:\n%s", out)
		}
	})

	// The strict decoder zeroes a malformed entry; printed as-is it would be a
	// blank row, which is the entry an operator chasing happ_preimage_invalid
	// is looking for.
	t.Run("a malformed entry is named, not printed blank", func(t *testing.T) {
		raw, err := json.Marshal(teeCaptureWithAux(t, models))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc["app_models"].([]any)[1].(map[string]any), "weights_digest")
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", inject.AttestationContentType)
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		out, err := runTeeInspect(t, srv.URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		if !strings.Contains(out, "malformed entry") {
			t.Errorf("a malformed entry was not named:\n%s", out)
		}
		if !strings.Contains(out, "qwen3-30b") {
			t.Errorf("the well-formed entry vanished:\n%s", out)
		}
		// Named INSTEAD of printed: a row whose model-id column is blank
		// (2 spaces of indent, 28 of id, 1 separator) is the zeroed entry.
		if regexp.MustCompile(`(?m)^ {31}\S`).MatchString(out) {
			t.Errorf("the malformed entry was also printed as a blank row:\n%s", out)
		}
	})

	// A 9.9 node serving nothing sends no app_models key, so only the
	// quote's upper half separates it from a pre-9.9 node. This is the state
	// right after boot, when operators run this command.
	t.Run("an empty catalog is not reported as an out-of-date node", func(t *testing.T) {
		out, err := runTeeInspect(t, teeCaptureNode(t, teeCaptureWithAux(t, nil)).URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		if !strings.Contains(out, "serve no models") {
			t.Errorf("an empty attested catalog was not reported as such:\n%s", out)
		}
		if strings.Contains(out, "pre-9.9 shape") {
			t.Errorf("a 9.9 node attesting an empty catalog was reported as predating 9.9:\n%s", out)
		}
	})

	// The unspliced capture: a genuine pre-9.9 quote, so this is the real
	// hardware-signed shape rather than an approximation of it.
	t.Run("a zero upper half is reported as a node predating 9.9", func(t *testing.T) {
		out, err := runTeeInspect(t, teeCaptureNode(t, teeCaptureBundle(t)).URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		if !strings.Contains(out, "pre-9.9 shape") {
			t.Errorf("a pre-9.9 quote was not identified as one:\n%s", out)
		}
		if strings.Contains(out, "serve no models") {
			t.Errorf("a pre-9.9 node was reported as attesting an empty catalog:\n%s", out)
		}
		if !strings.Contains(out, "refuse it (aux_binding_absent") {
			t.Errorf("a pre-9.9 node's report does not say payers will refuse it:\n%s", out)
		}
	})

	// The echoed nonce is an input to the aux hash, so an operator chasing
	// aux_binding_mismatch needs to see it.
	t.Run("prints the bundle's nonce", func(t *testing.T) {
		nonce := strings.Repeat("a1", 32)
		b := teeCaptureWithAux(t, models)
		b.Nonce = nonce
		srv := teeCaptureNode(t, b)
		out, err := runTeeInspect(t, srv.URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		if !regexp.MustCompile(`nonce:\s+` + nonce).MatchString(out) {
			t.Errorf("the bundle's nonce was not printed:\n%s", out)
		}
		jsonOut, err := runTeeInspect(t, srv.URL, "--json")
		if err != nil {
			t.Fatalf("tee inspect --json: %v\n%s", err, jsonOut)
		}
		var got struct {
			BundleNonce string `json:"bundle_nonce"`
		}
		if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
			t.Fatalf("decode --json: %v\n%s", err, jsonOut)
		}
		if got.BundleNonce != nonce {
			t.Errorf("bundle_nonce = %q, want %q", got.BundleNonce, nonce)
		}
	})

	// The section survives an early stop, and an unusable quote is not read
	// as a zero upper half.
	for _, c := range []struct {
		name   string
		mangle func(t *testing.T, b *inject.TEEEvidenceBundle)
	}{
		{"undecodable", func(_ *testing.T, b *inject.TEEEvidenceBundle) { b.CPUReport = "not base64 at all" }},
		{"truncated before report_data", func(t *testing.T, b *inject.TEEEvidenceBundle) {
			q, err := base64.StdEncoding.DecodeString(b.CPUReport)
			if err != nil {
				t.Fatal(err)
			}
			b.CPUReport = base64.StdEncoding.EncodeToString(q[:600])
		}},
		{"not a TDX quote", func(t *testing.T, b *inject.TEEEvidenceBundle) {
			q, err := base64.StdEncoding.DecodeString(b.CPUReport)
			if err != nil {
				t.Fatal(err)
			}
			q[4] ^= 0xff // tee_type
			b.CPUReport = base64.StdEncoding.EncodeToString(q)
		}},
	} {
		t.Run("an unusable quote is not a pre-9.9 node: "+c.name, func(t *testing.T) {
			b := teeCaptureBundle(t) // zero upper half, so a misread says pre-9.9
			b.AppModels = nil
			c.mangle(t, &b)
			out, err := runTeeInspect(t, teeCaptureNode(t, b).URL)
			if err != nil {
				t.Fatalf("tee inspect: %v\n%s", err, out)
			}
			if strings.Contains(out, "pre-9.9 shape") || strings.Contains(out, "serve no models") {
				t.Errorf("an unusable quote was diagnosed:\n%s", out)
			}
			if !strings.Contains(out, "cannot tell an empty catalog") {
				t.Errorf("an unusable quote did not say it cannot tell:\n%s", out)
			}
			// The JSON carries the same state as the text; the text's switch
			// happens to test QuoteReadable first, and JSON readers get no such
			// shield.
			jsonOut, err := runTeeInspect(t, teeCaptureNode(t, b).URL, "--json")
			if err != nil {
				t.Fatalf("tee inspect --json: %v\n%s", err, jsonOut)
			}
			var got struct {
				AuxBindingPresent *bool `json:"aux_binding_present"`
				QuoteReadable     *bool `json:"quote_readable"`
			}
			if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
				t.Fatalf("decode --json: %v\n%s", err, jsonOut)
			}
			if got.QuoteReadable == nil || *got.QuoteReadable {
				t.Errorf("quote_readable missing or true on an unusable quote:\n%s", jsonOut)
			}
			if got.AuxBindingPresent == nil || *got.AuxBindingPresent {
				t.Errorf("aux_binding_present missing or true on an unusable quote:\n%s", jsonOut)
			}
		})
	}

	t.Run("the catalog survives an early stop", func(t *testing.T) {
		b := teeCaptureWithAux(t, models)
		b.CPUReport = "not base64 at all"
		out, err := runTeeInspect(t, teeCaptureNode(t, b).URL)
		if err != nil {
			t.Fatalf("tee inspect: %v\n%s", err, out)
		}
		if !strings.Contains(out, "qwen3-30b") {
			t.Errorf("the published catalog vanished on a bundle that stopped early:\n%s", out)
		}
		// With models listed there is no empty-catalog ambiguity to report.
		if !strings.Contains(out, "report_data cannot be read") {
			t.Errorf("an unusable quote under a listed catalog did not say so:\n%s", out)
		}
		if strings.Contains(out, "cannot tell an empty catalog") {
			t.Errorf("a listed catalog was described as possibly empty:\n%s", out)
		}
	})
}

// Cobra registration is easy to forget and impossible to notice: the package
// still builds, the tests still pass, and the command simply does not exist.
func TestRootCmd_CarriesTee(t *testing.T) {
	var found *cobra.Command
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "tee" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("`zs-proxy tee` is not registered on the root command")
	}
	if _, _, err := found.Find([]string{"inspect"}); err != nil {
		t.Errorf("`zs-proxy tee inspect` is not registered: %v", err)
	}
}
