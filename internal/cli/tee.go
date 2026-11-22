/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/attest"
	"github.com/TxnLab/zerosignal/go/inject"
	"github.com/TxnLab/zerosignal/go/zs"
	"github.com/TxnLab/zerosignal/go/zs/tee"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/config"
)

// newTeeCmd is the `zs-proxy tee` inspection group.
func newTeeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tee",
		Short: "Inspect confidential-mode (TEE) attestation evidence",
	}
	cmd.AddCommand(newTeeInspectCmd())
	return cmd
}

// teeInspect holds one run's inputs.
//
// It binds --config on its own rather than reusing proxyFlags.bindCommon,
// which would also bind a --network that this command ignores. The config is
// read only for the zs.tee block; nothing here talks to a chain.
type teeInspect struct {
	configPath string
	file       string
	jsonOut    bool
	chain      bool

	// verifyQuoteChain is tee.VerifyTDXQuoteChainStale in production. A
	// field for the same reason DstackVerifier keeps one: the states that
	// matter most here — a quote that FAILS the chain, and the report refusing
	// to hand out allowlist values from it — are unreachable in a test that has
	// to reach Intel's PCS, so without a seam the highest-risk branch in this
	// file would be the one branch no test covers.
	//
	// It reports staleness because this is a DIAGNOSTIC: routing treats a
	// stale pass as a pass, but an operator reading "verified with revocation
	// and TCB collateral" is owed the difference.
	verifyQuoteChain func(context.Context, []byte) (bool, error)

	// verifyCarriedChain is tee.VerifyTDXQuoteChainCarried in
	// production — SPEC §3e's last resort, the node's own copy of the
	// Intel collateral, tried only once PCS and our cache have both come
	// up empty.
	//
	// A SEPARATE SEAM rather than folding the fallback into the one
	// above, because this tool must be able to say WHICH source a pass
	// came from. The routing verifier is allowed to collapse them (a
	// pass is a pass, and it has no report to write); a diagnostic that
	// printed "verified against Intel's PCK chain" over documents the
	// attested node supplied would be the exact quiet inaccuracy this
	// command exists to avoid.
	verifyCarriedChain func(context.Context, []byte, *inject.TEECollateral) error
}

func newTeeInspectCmd() *cobra.Command {
	cmd, _ := teeInspectCmd()
	return cmd
}

// teeInspectCmd exposes the flag struct alongside the command so a test can
// install the quote-chain seam. See teeInspect.verifyQuoteChain.
func teeInspectCmd() (*cobra.Command, *teeInspect) {
	f := &teeInspect{}
	cmd := &cobra.Command{
		Use:   "inspect [node-base-url]",
		Short: "Report what a node's measured compose says, and the values to allowlist",
		Long: "Fetches /v1/zs/attestation from a node, replays its event log, checks the published\n" +
			"app-compose document against the measurement, and reports what the document says —\n" +
			"including the two values the release allowlists hold: the compose SKELETON digest\n" +
			"(zs.tee.trusted_compose_skeletons) and the image reference (zs.tee.trusted_node_images).\n" +
			"Nothing else computes a skeleton digest, so this is how those lists get populated.\n\n" +
			"PASS --chain BEFORE PASTING ANYTHING INTO AN ALLOWLIST. Without it the quote's\n" +
			"signature is never examined, so the replay is arithmetic over bytes the node chose and\n" +
			"a hostile node can name its own skeleton digest. It is opt-in because it reaches\n" +
			"Intel's PCS over the network, and a diagnostic that fails when Intel is having a bad\n" +
			"day is a diagnostic you cannot run when you need it.\n\n" +
			"With --file it reads a saved app-compose.json, or a raw docker-compose YAML, instead.\n" +
			"That is how a release list is generated from a published compose before any node runs\n" +
			"it — but a file has no measurement to be checked against, so the report says so. The\n" +
			"bytes must be EXACTLY the ones that will be measured: a differing trailing newline is\n" +
			"enough to move the skeleton digest, and a listed digest that is off by one byte\n" +
			"refuses every node.\n\n" +
			"It reports; it does not gate. The exit code is 0 whatever it finds, so a violation is\n" +
			"something to read rather than something to trap. The content rules are the shared\n" +
			"policy from the protocol module, with this proxy's own zs.tee overrides applied — the\n" +
			"same policy the running verifier uses.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.run(cmd, args)
		},
	}
	cmd.Flags().StringVar(&f.configPath, "config", defaultConfigPath(),
		"path to YAML config file — read for zs.tee only, so the report uses the same policy this proxy would")
	cmd.Flags().StringVar(&f.file, "file", "",
		"read a saved app-compose.json (or raw docker-compose YAML) instead of fetching from a node")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, "emit the report as JSON")
	cmd.Flags().BoolVar(&f.chain, "chain", false,
		"also verify the quote against Intel's PCK chain (needs network access to Intel PCS)")
	return cmd, f
}

func (f *teeInspect) run(cmd *cobra.Command, args []string) error {
	switch {
	case f.file != "" && len(args) > 0:
		return errors.New("pass a node URL or --file, not both")
	case f.file == "" && len(args) == 0:
		return errors.New("pass a node base URL, or --file to read a saved document")
	case f.file != "" && f.chain:
		// Refused rather than ignored: a --chain that silently did nothing
		// would let someone believe a file had been checked against Intel.
		return errors.New("--chain needs a quote, which only a node has; drop it or pass a node URL")
	}

	// LoadBase rather than the full resolution path: this command needs the
	// zs.tee block and nothing else, and a diagnostic that refuses to run
	// because an unrelated field fails validation is a diagnostic nobody can
	// use on the machine that has the problem.
	cfg, err := config.LoadBase(f.configPath)
	if err != nil {
		return fmt.Errorf("config load failed: %w", err)
	}
	policy := app.ComposePolicyFromTEEConfig(cfg.Zs.TEE, nil)

	var rep teeReport
	if f.file != "" {
		rep, err = f.inspectFile(policy)
	} else {
		rep, err = f.inspectNode(cmd.Context(), cfg.Zs.TEE, policy, args[0])
	}
	if err != nil {
		return err
	}

	if f.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.writeText(cmd.OutOrStdout())
	return nil
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// teeReport is one inspection's findings. It is the --json shape, so field
// names are a contract for whatever generates a release list from it.
//
// Stop carries the reason the report ends early, and it exists because the
// alternative — printing document-derived values under a failure banner — is
// how someone pastes a skeleton digest recovered from a document the hardware
// never measured. See the ordering rule in proto/SPEC.md §3e: everything below
// the hash gate is meaningless if the gate fails.
type teeReport struct {
	Source string `json:"source"`

	// Verified means ALL of: the quote verified against Intel's PCK chain,
	// the event log replayed to that quote's RTMR3, the boot registers
	// matched the measured row for the image the replay names, and the
	// published document hashed to the compose-hash the replay recovered.
	// Without the boot term a foreign kernel's RTMR3 would verify; an image
	// with no row (unlisted, or listed but unmeasured) is therefore false.
	//
	// THE CHAIN TERM IS NOT OPTIONAL TO THIS FIELD even though --chain is
	// optional to the command, and that is the whole point of the field. The
	// replay reads RTMR3 out of the quote bytes; nothing in it authenticates
	// them. A node serving fabricated bytes with an RTMR3 of its choosing, a
	// matching event log and a matching document satisfies every other step,
	// so a `verified` that did not require the chain would be true for a
	// forgery. It is therefore false on every run without --chain.
	Verified bool   `json:"verified"`
	Offline  bool   `json:"offline"`
	Stop     string `json:"stop,omitempty"`

	Mode        string `json:"mode,omitempty"`
	GeneratedAt string `json:"generated_at,omitempty"`

	ComposeHashGate string `json:"compose_hash_gate,omitempty"`
	QuoteChain      string `json:"quote_chain,omitempty"`

	Measurements   map[string]string `json:"measurements,omitempty"`
	OSImageTrusted *bool             `json:"os_image_on_trusted_list,omitempty"`
	ComposeTrusted *bool             `json:"compose_hash_on_trusted_list,omitempty"`
	// nil when the replay carries no storage-encrypted event, which is every
	// dstack 0.5.x image: nothing was recorded, so there is no verdict.
	StorageEncrypted *bool `json:"storage_encrypted,omitempty"`

	SkeletonDigest string `json:"trusted_compose_skeletons_value,omitempty"`
	ImageRef       string `json:"trusted_node_images_value,omitempty"`
	SkeletonError  string `json:"skeleton_error,omitempty"`
	// Membership of the two values above in THIS proxy's release lists — the
	// rules that actually gate on a stock config. nil when the release match
	// is off, which trustNote renders as no annotation rather than a failure.
	SkeletonTrusted *bool `json:"skeleton_on_trusted_list,omitempty"`
	ImageTrusted    *bool `json:"node_image_on_trusted_list,omitempty"`

	PreLaunchBytes  int    `json:"pre_launch_script_bytes"`
	PreLaunchDigest string `json:"pre_launch_script_sha256,omitempty"`

	AllowedEnvs      []string `json:"allowed_envs,omitempty"`
	Features         []string `json:"features,omitempty"`
	StorageFS        string   `json:"storage_fs,omitempty"`
	LocalKeyProvider *bool    `json:"local_key_provider_enabled,omitempty"`

	// ContentRulesRan separates "the policy found nothing wrong" from "the
	// policy never looked". Raw docker-compose text carries none of the fields
	// the rules read, so an empty Violations list there means nothing at all —
	// and an empty list is precisely what a reader takes for a pass.
	ContentRulesRan      bool         `json:"content_rules_ran"`
	ReleaseMatchEnforced bool         `json:"release_match_enforced"`
	TrustedSkeletonCount int          `json:"trusted_compose_skeletons_configured"`
	TrustedImageCount    int          `json:"trusted_node_images_configured"`
	Violations           []teeFinding `json:"violations"`

	// PublishedModels is the bundle's app_models, not verified: the aux
	// binding hashes the node's on-chain id, which this command does not look
	// up. Printed so an operator chasing aux_binding_mismatch sees the list as
	// a verifier receives it, e.g. to spot a middlebox that rewrote it. Named
	// "published", not "attested", because /v1/zs/operators uses
	// attested_model_ids for a list that was verified.
	PublishedModels []teePublishedModel `json:"published_models,omitempty"`
	BundleNonce     string              `json:"bundle_nonce,omitempty"`
	// AuxBindingPresent is true when report_data's upper 32 bytes are
	// non-zero. `app_models` is `omitempty`, so an empty catalog and a
	// pre-9.9 node send identical JSON; the quote tells them apart.
	//
	// Both are pointers so --file, which has no quote, omits them rather than
	// reporting false as if it had read one.
	AuxBindingPresent *bool `json:"aux_binding_present,omitempty"`
	// QuoteReadable is true when the quote decoded, is a TDX quote, and is
	// long enough to hold report_data. Only then does AuxBindingPresent mean
	// anything.
	QuoteReadable *bool `json:"quote_readable,omitempty"`
}

type teePublishedModel struct {
	ModelID string `json:"model_id"`
	Source  string `json:"source,omitempty"`
	Digest  string `json:"weights_digest,omitempty"`
	// Empty on a malformed entry, whose zeroed state would read as a value
	// the node sent.
	State string `json:"weights_state,omitempty"`
	// The strict decoder zeroes a malformed entry, so without this flag it
	// would print as a blank row: the one a happ_preimage_invalid hunt needs.
	Malformed bool `json:"malformed,omitempty"`
}

type teeFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// notVerifiedBanner is deliberately more than one line. The values below it
// are perfectly real facts about the bytes that were read and perfectly
// useless as evidence about a deployment, and that is exactly the output
// someone screenshots as proof a node is attested.
const notVerifiedBanner = "NOT VERIFIED — this is a document, not a node.\n" +
	"  There is no measurement to check it against, so everything below describes\n" +
	"  these bytes only. A node publishing an entirely different document would\n" +
	"  print the same values."

func (r teeReport) writeText(w io.Writer) {
	fmt.Fprintf(w, "source:        %s\n", r.Source)
	if r.Mode != "" {
		fmt.Fprintf(w, "mode:          %s\n", r.Mode)
	}
	if r.GeneratedAt != "" {
		fmt.Fprintf(w, "generated at:  %s\n", r.GeneratedAt)
	}
	fmt.Fprintln(w)

	// The gate first, always. The banner is keyed on Offline rather than on
	// the gate line being absent: a node whose event log fails to replay also
	// has no gate line, and telling that operator "this is a document, not a
	// node" would name the wrong problem.
	switch {
	case r.Offline:
		fmt.Fprintln(w, notVerifiedBanner)
	case r.ComposeHashGate != "":
		fmt.Fprintf(w, "compose-hash gate:  %s\n", r.ComposeHashGate)
	}
	if r.QuoteChain != "" {
		fmt.Fprintf(w, "quote chain:        %s\n", r.QuoteChain)
	}
	// The measurements come off the replay, which was checked against the
	// quote — so they survive a failed compose gate and are printed before it
	// stops the report. Only DOCUMENT-derived values are meaningless below a
	// gate failure, and those all live after the Stop check.
	if len(r.Measurements) > 0 {
		fmt.Fprintln(w, "\nmeasurements (replayed from the event log)")
		// key-provider is in the map and deliberately not on this list: its
		// payload is a hex-encoded certificate blob that would bury the values
		// a reader is here for. --json carries the whole replay.
		for _, name := range []string{
			attest.EventComposeHash, attest.EventOSImageHash,
			attest.EventAppID, attest.EventInstanceID, attest.EventStorageEncrypted,
		} {
			v, ok := r.Measurements[name]
			if !ok {
				continue
			}
			line := fmt.Sprintf("  %-19s %s", name+":", v)
			// Annotated because each refuses independently, and a report that
			// prints a measurement without saying whether it is accepted looks
			// healthy for the most common refusal there is. The compose-hash
			// note is absent on a stock proxy, where that pin is skipped — see
			// where ComposeTrusted is set. The release match, which IS the gate
			// there, is reported under "release allowlist values" below.
			switch name {
			case attest.EventOSImageHash:
				line += trustNote(r.OSImageTrusted, "zs.tee.trusted_os_images")
			case attest.EventComposeHash:
				line += trustNote(r.ComposeTrusted, "zs.tee.trusted_compose_hashes")
			case attest.EventStorageEncrypted:
				// No list to name: every verifier refuses a recorded value
				// other than "1", because the host picks it on a kernel cmdline
				// no verifier of ours pins. The payload is hex, so say which.
				switch {
				case r.StorageEncrypted == nil:
				case *r.StorageEncrypted:
					line += "  [encrypted]"
				default:
					line += "  [REFUSED — the data disk is not recorded as encrypted]"
				}
			}
			fmt.Fprintln(w, line)
		}
	}

	// --file has no bundle, catalog or quote to describe.
	if !r.Offline {
		writePublishedModels(w, r)
	}

	if r.Stop != "" {
		fmt.Fprintf(w, "\n%s\n", r.Stop)
		return
	}

	fmt.Fprintln(w, "\nrelease allowlist values")
	if r.SkeletonError != "" {
		fmt.Fprintf(w, "  none — %s\n", r.SkeletonError)
	} else {
		// Membership, not just the value. These two ARE the gate on a stock
		// proxy, so printing them bare left the report silent about the only
		// rule that was going to refuse — while the compose-hash line above
		// carried a verdict for a rule nothing consults.
		fmt.Fprintf(w, "  trusted_compose_skeletons:  %s%s\n", r.SkeletonDigest,
			trustNote(r.SkeletonTrusted, "zs.tee.trusted_compose_skeletons"))
		fmt.Fprintf(w, "  trusted_node_images:        %s%s\n", r.ImageRef,
			trustNote(r.ImageTrusted, "zs.tee.trusted_node_images"))
	}

	if !r.ContentRulesRan {
		fmt.Fprintln(w, "\ncontent rules:  DID NOT RUN — this is raw docker-compose text, not an\n"+
			"                app-compose.json document, so allowed_envs, pre_launch_script,\n"+
			"                features and the rest are simply absent. Nothing was checked.")
		return
	}

	fmt.Fprintln(w, "\npre_launch_script")
	if r.PreLaunchDigest == "" {
		fmt.Fprintln(w, "  none")
	} else {
		fmt.Fprintf(w, "  %d bytes, sha256 %s\n", r.PreLaunchBytes, r.PreLaunchDigest)
	}

	fmt.Fprintln(w, "\nfields the policy reads")
	fmt.Fprintf(w, "  %-28s %s\n", "allowed_envs:", joinOrNone(r.AllowedEnvs))
	fmt.Fprintf(w, "  %-28s %s\n", "features:", joinOrNone(r.Features))
	fmt.Fprintf(w, "  %-28s %s\n", "storage_fs:", orNone(r.StorageFS))
	if r.LocalKeyProvider != nil {
		fmt.Fprintf(w, "  %-28s %t\n", "local_key_provider_enabled:", *r.LocalKeyProvider)
	}

	fmt.Fprintln(w)
	switch {
	case !r.ReleaseMatchEnforced && (r.TrustedSkeletonCount > 0 || r.TrustedImageCount > 0):
		// Populated lists nobody consults. Called out separately from the
		// plain off state because the operator DID paste the values and would
		// otherwise read "release match off" as still needing to — sending
		// them to re-derive digests that are already correct, when the actual
		// remedy is one boolean. attest reports this against the verifier as
		// policy_misconfigured; the wording follows.
		fmt.Fprintf(w, "policy:        shared ZeroSignal rules; release match off — "+
			"the workload is unchecked\n"+
			"               WARNING: %d skeleton(s) and %d image(s) are configured but "+
			"NEITHER list is consulted\n"+
			"               set zs.tee.enforce_compose_skeleton: true\n",
			r.TrustedSkeletonCount, r.TrustedImageCount)
	case !r.ReleaseMatchEnforced:
		fmt.Fprintln(w, "policy:        shared ZeroSignal rules; release match off — "+
			"the workload is unchecked\n"+
			"               (populate zs.tee.trusted_compose_skeletons and trusted_node_images,\n"+
			"                then set zs.tee.enforce_compose_skeleton: true)")
	case r.TrustedSkeletonCount == 0 || r.TrustedImageCount == 0:
		// The half-configured state is the rollout footgun, and this is the
		// command an operator runs when every node is being refused — so it
		// says so here rather than only in a startup log they are not reading.
		fmt.Fprintf(w, "policy:        shared ZeroSignal rules; release match ENFORCED "+
			"(%d skeleton(s), %d image(s))\n"+
			"               WARNING: one list is empty, so every node is refused on it — "+
			"populate both, or neither\n", r.TrustedSkeletonCount, r.TrustedImageCount)
	default:
		fmt.Fprintf(w, "policy:        shared ZeroSignal rules; release match ENFORCED "+
			"(%d skeleton(s), %d image(s))\n", r.TrustedSkeletonCount, r.TrustedImageCount)
	}
	if len(r.Violations) == 0 {
		fmt.Fprintln(w, "violations:    none")
		return
	}
	fmt.Fprintf(w, "violations:    %d\n", len(r.Violations))
	for _, v := range r.Violations {
		fmt.Fprintf(w, "  %s: %s\n", v.Code, v.Detail)
	}
}

// writePublishedModels prints bundle.AppModels, then what report_data's upper
// half says about them. An empty catalog and a pre-9.9 node send identical
// JSON (`app_models` is `omitempty`), so only the quote tells them apart.
//
// It runs before the Stop check, so a bundle refused by an earlier gate still
// shows its list. On such a bundle the payer-facing consequence is omitted:
// a payer stops at the earlier refusal and never reaches the aux check.
func writePublishedModels(w io.Writer, r teeReport) {
	fmt.Fprintln(w, "\npublished models (app_models) — NOT VERIFIED")
	fmt.Fprintln(w, "  (the check needs the node's on-chain id, which this command does not look up)")
	// At least 28 so short catalogs line up with the rest of the report; wider
	// when a real id (org/Model-Name-Quant) would push the columns out.
	width := 28
	for _, m := range r.PublishedModels {
		width = max(width, len(m.ModelID))
	}
	for _, m := range r.PublishedModels {
		if m.Malformed {
			fmt.Fprintln(w, "  (malformed entry: a missing key, a null, or a wrong type)")
			continue
		}
		fmt.Fprintf(w, "  %-*s %-12s %s%s\n", width, m.ModelID, m.State,
			orNone(m.Digest), sourceNote(m.Source))
	}
	if len(r.PublishedModels) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	if r.BundleNonce != "" {
		fmt.Fprintf(w, "  %-*s %s\n", width, "nonce:", r.BundleNonce)
	}
	fmt.Fprintln(w, "  "+auxStateNote(r))
}

func auxStateNote(r teeReport) string {
	empty := len(r.PublishedModels) == 0
	readable := r.QuoteReadable != nil && *r.QuoteReadable
	present := r.AuxBindingPresent != nil && *r.AuxBindingPresent
	switch {
	case !readable && empty:
		return "The quote is missing, not TDX, or too short, so this run cannot tell an " +
			"empty catalog from a node predating proto 9.9."
	case !readable:
		return "The quote is missing, not TDX, or too short, so report_data cannot be read."
	case !present:
		note := "report_data's upper 32 bytes are zero, which is the pre-9.9 shape."
		if r.Stop == "" {
			// This command does not check the key half; a payer checks it
			// first and names that failure instead when it also fails.
			note += " Payers on proto 9.9 or later refuse it (aux_binding_absent, if the " +
				"key half matches) until the node is upgraded."
		}
		return note
	case empty:
		return "An empty list means the node claims to serve no models, which is normal " +
			"until its catalog loads after boot. This run cannot tell that from a node " +
			"withholding its catalog. A node older than proto 9.10 with an empty list is " +
			"refused by 9.10 payers as happ_preimage_missing."
	default:
		return "report_data's upper 32 bytes are populated. This run does not recompute them; " +
			"a node older than proto 9.10 binds a different preimage, which payers on 9.10 " +
			"or later refuse as aux_binding_mismatch."
	}
}

func sourceNote(s string) string {
	if s == "" {
		return ""
	}
	return "  source: " + s
}

// trustNote annotates a measurement with its allowlist membership. A nil
// verdict prints nothing rather than "not on the list": the two are different
// claims and only one of them is about the node.
func trustNote(on *bool, list string) string {
	switch {
	case on == nil:
		return ""
	case *on:
		return "  [on " + list + "]"
	default:
		return "  [NOT on " + list + "]"
	}
}

func joinOrNone(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ---------------------------------------------------------------------------
// Node mode
// ---------------------------------------------------------------------------

func (f *teeInspect) inspectNode(ctx context.Context, cfg config.TEEConfig, policy attest.ComposePolicy, arg string) (teeReport, error) {
	baseURL := arg
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	bundle, err := zs.NewAttestationClient(nil, 0).Fetch(ctx, baseURL, "", nil)
	if err != nil {
		return teeReport{}, fmt.Errorf("fetch attestation from %s: %w", baseURL, err)
	}

	rep := teeReport{
		Source:               baseURL,
		Mode:                 bundle.Mode,
		ReleaseMatchEnforced: policy.EnforceComposeSkeleton,
		TrustedSkeletonCount: len(policy.TrustedComposeSkeletons),
		TrustedImageCount:    len(policy.TrustedNodeImages),
		Violations:           []teeFinding{},
	}
	if !bundle.GeneratedAt.IsZero() {
		rep.GeneratedAt = bundle.GeneratedAt.UTC().Format(time.RFC3339)
	}
	// Filled before the stop branches below, so the catalog still prints on a
	// bundle whose quote is unusable.
	rep.BundleNonce = bundle.Nonce
	for _, m := range bundle.AppModels {
		row := teePublishedModel{ModelID: m.ModelID, Source: m.Source, Digest: m.WeightsDigest,
			Malformed: m.Malformed()}
		if !row.Malformed {
			row.State = m.State.String()
		}
		rep.PublishedModels = append(rep.PublishedModels, row)
	}
	// False until the quote is read below: every stop before that point is a
	// quote that could not be read.
	rep.QuoteReadable, rep.AuxBindingPresent = new(false), new(false)

	quote, qerr := base64.StdEncoding.DecodeString(bundle.CPUReport)
	if qerr != nil || len(quote) == 0 {
		rep.Stop = "this bundle carries no decodable cpu_report, so there is no quote to " +
			"replay the event log against. Only " + inject.TEEModeDstackTDX + " publishes one."
		return rep, nil
	}
	// The platform comes from the REPORT BYTES, never from bundle.Mode — the
	// same rule the verifier follows, and the reason it is worth following in a
	// report is that mode is node-authored and everything below reads TDX quote
	// offsets. A SEV-SNP report parsed at those offsets replays to nonsense.
	if teeType, ok := attest.QuoteTEEType(quote); !ok || teeType != attest.TEETypeTDX {
		rep.Stop = fmt.Sprintf("this is not an Intel TDX v4 quote (tee_type %#x), and the "+
			"event-log replay below only reads TDX", teeType)
		return rep, nil
	}
	// Read before the chain gate so the model section can say whether the
	// upper half is populated even on a bundle that stops below. This only
	// reads the bytes; checking their value needs the chain-resolved node id.
	_, splitErr := attest.SplitReportData(quote)
	*rep.QuoteReadable = !errors.Is(splitErr, attest.ErrReportDataAbsent)
	*rep.AuxBindingPresent = splitErr == nil

	// THE CHAIN GOES FIRST, matching SPEC §3e step 5, and its verdict gates
	// everything below rather than riding alongside it. Every step after this
	// reads fields out of the quote — RTMR3 for the replay, and through it the
	// compose-hash the document is gated on — so on unauthenticated bytes they
	// are arithmetic over numbers the node picked. A forged quote whose RTMR3
	// matches its own event log satisfies the replay and the hash gate
	// perfectly, and the report's most dangerous output is a skeleton digest
	// an operator then pastes into an allowlist.
	chainOK := false
	if f.chain {
		var fail bool
		rep.QuoteChain, chainOK, fail = f.checkChain(ctx, quote, bundle.Collateral)
		if fail {
			// A refusal, not an annotation: the hardware measured nothing
			// here, which is strictly worse than the substituted-document
			// case below, and that one stops too.
			rep.Stop = "the quote does not verify against Intel's PCK chain, so it attests " +
				"nothing — no measurement below it would be evidence about any hardware, and " +
				"none is reported."
			return rep, nil
		}
	} else {
		// Printed on every run without --chain, because silence here is
		// indistinguishable from a pass and this is the step the rest rests on.
		rep.QuoteChain = "NOT CHECKED — pass --chain to verify against Intel's PCK chain. " +
			"Until then the quote's signature is unexamined, so everything below is arithmetic " +
			"over bytes this node supplied."
	}

	// Every payer refuses a debug TD before the replay, so the report stops
	// here too, or it would print trust values from an RTMR3 the host could
	// write.
	if err := attest.VerifyNotDebug(quote); err != nil {
		rep.Stop = "the quote was taken in a debug TD, so its host can read and write guest " +
			"memory and no measurement below it would be evidence: " + err.Error()
		return rep, nil
	}

	// The replay, and only the replay, is where a measurement may come from.
	// Reading compose-hash off the bundle would be the cheap wrong verifier
	// this whole area exists to refuse — and it would agree with the correct
	// one on every honest node.
	measurements, err := attest.VerifyEventLog(bundle.EventLog, quote)
	if err != nil {
		rep.Stop = "the event log does not replay to the quote's RTMR3: " + err.Error()
		return rep, nil
	}
	rep.Measurements = measurements
	osOnList := tee.MeasurementTrusted(cfg.TrustedOSImages, measurements[attest.EventOSImageHash])
	rep.OSImageTrusted = &osOnList
	// The os-image-hash and compose-hash below are RTMR3 events a foreign
	// kernel could write, so a mismatch stops the report rather than print
	// them as trust values, whether or not the image is on the list. The
	// error names the register, which on an RTMR0-only mismatch is the new
	// shape's value to measure. An image with no measured row is a gap in
	// our table, not evidence of forgery, so it gets its own message, and
	// stops only when the list would otherwise have admitted it: an
	// unlisted image is already annotated above.
	bootErr := attest.VerifyBootChain(quote, measurements[attest.EventOSImageHash], attest.TrustedDstackBootChains())
	switch {
	case bootErr == nil:
	case errors.Is(bootErr, attest.ErrBootChainUnknown):
		if osOnList {
			rep.Stop = "this OS image has no measured boot chain in this build, so every payer " +
				"refuses it until one is measured and released (" + bootErr.Error() + "). " +
				"This quote's registers: " + bootRegisters(quote)
			return rep, nil
		}
	default:
		rep.Stop = "the quote's boot registers (MRTD, RTMR0-2) are not the trusted image's, so " +
			"the runtime events it replays were not necessarily written by that image: " + bootErr.Error()
		return rep, nil
	}
	if _, recorded := measurements[attest.EventStorageEncrypted]; recorded {
		encrypted := attest.VerifyStorageEncrypted(measurements) == nil
		rep.StorageEncrypted = &encrypted
	}

	// THE COMPOSE-HASH PIN IS ONLY ANNOTATED WHEN IT WILL BE CONSULTED, and
	// the condition mirrors the verifier's guard exactly (see
	// tee.DstackVerifier.Verify). On a stock proxy the pin is empty and the
	// release match is on, so it is SKIPPED — annotating it anyway printed
	// `[NOT on zs.tee.trusted_compose_hashes]` beside a healthy node, which is
	// a refusal-shaped line for a list nothing reads. In a command whose only
	// job is answering "why am I being refused", that is worse than silent.
	//
	// trustNote prints nothing for a nil verdict, so leaving it nil is how the
	// report says "this rule did not apply" rather than "this rule failed".
	if len(cfg.TrustedComposeHashes) > 0 || !policy.EnforceComposeSkeleton {
		composeOnList := tee.MeasurementTrusted(
			cfg.TrustedComposeHashes, measurements[attest.EventComposeHash])
		rep.ComposeTrusted = &composeOnList
	}

	if err := attest.VerifyComposeHash(bundle.AppCompose, measurements); err != nil {
		rep.ComposeHashGate = "FAIL — " + err.Error()
		rep.Stop = composeHashStop(err)
		return rep, nil
	}
	rep.ComposeHashGate = "PASS — the published document is the one this quote measured"
	rep.Verified = chainOK && bootErr == nil

	rep.fillFromDocument(bundle.AppCompose, measurements, policy)
	return rep, nil
}

// bootRegisters prints the four boot registers, the values a maintainer
// needs to add the image's row once the measurement is confirmed.
func bootRegisters(quote []byte) string {
	reg := func(b []byte, ok bool) string {
		if !ok {
			return "(absent)"
		}
		return hex.EncodeToString(b)
	}
	r0, ok0 := attest.QuoteRTMR(quote, 0)
	r1, ok1 := attest.QuoteRTMR(quote, 1)
	r2, ok2 := attest.QuoteRTMR(quote, 2)
	mrtd, okm := attest.QuoteMRTD(quote)
	return "MRTD " + reg(mrtd, okm) + ", RTMR0 " + reg(r0, ok0) + ", RTMR1 " + reg(r1, ok1) + ", RTMR2 " + reg(r2, ok2)
}

// composeHashStop names why the gate refused. Three causes, three messages,
// because they send an operator to three different places and two of them
// are not the operator's fault at all:
//
//   - no compose-hash event: the log replayed but anchors nothing. SPEC §3e
//     names this state and expects such a node to publish quote and log
//     without a preimage.
//   - no app_compose: the node published no preimage. Not a failure of the
//     document, an absence of one.
//   - anything else: the document is not the measured one.
//
// A separate function so the mapping is testable: the branches are otherwise
// only reachable through a bundle whose quote would have to be forged to
// construct, which is why they went untested when they were inline.
func composeHashStop(err error) string {
	switch {
	case errors.Is(err, attest.ErrNoComposeHash):
		return "the replayed event log carries no compose-hash event, so there is nothing " +
			"for a published document to be checked against. A node that cannot anchor one " +
			"is expected to publish its quote and event log without a preimage."
	case errors.Is(err, attest.ErrNoAppCompose):
		return "this node publishes no app_compose preimage, so its compose content cannot " +
			"be read at all. That is not the same as passing: run the node on proto 9.6 or later."
	default:
		return "the published document is not the one this quote measured, so nothing in it " +
			"describes this node. Use --file if you want to inspect the document anyway."
	}
}

func (f *teeInspect) verifyCarried(ctx context.Context, quote []byte, c *inject.TEECollateral) error {
	if f.verifyCarriedChain != nil {
		return f.verifyCarriedChain(ctx, quote, c)
	}
	return tee.VerifyTDXQuoteChainCarried(ctx, quote, c)
}

// checkChain reports the chain verdict as (line, ok, fail).
//
// ok and fail are NOT complements, and the gap between them is the point: an
// unreachable Intel PCS is our outage and says nothing about this node, so it
// is neither a pass nor an accusation. The report continues (a diagnostic that
// went dark during an Intel outage would be useless exactly when it is needed)
// but Verified stays false, because nothing authenticated the quote.
//
// FOUR OUTCOMES NOW, NOT THREE, and the fourth is why the sentence above no
// longer reads "an unreachable PCS means Verified stays false". Since the
// carried rung landed, an unreachable PCS can still end in a PASS: the node's
// own collateral is Intel-signed and the library validates it against the root
// it embeds, so the quote IS authenticated and Verified is true. What that pass
// does not carry is a vintage of our choosing, which is what its line says out
// loud. The ordering here is deliberate — the carried attempt runs before the
// switch, so its success cannot be reported as either of the outage arms.
func (f *teeInspect) checkChain(ctx context.Context, quote []byte, carried *inject.TEECollateral) (line string, ok, fail bool) {
	verify := f.verifyQuoteChain
	if verify == nil {
		verify = tee.VerifyTDXQuoteChainStale
	}
	stale, err := verify(ctx, quote)
	var carriedErr error
	if errors.Is(err, tee.ErrCollateralUnavailable) {
		carriedErr = f.verifyCarried(ctx, quote, carried)
		if carriedErr == nil {
			// A PASS, and the weakest one this tool reports. Intel signed
			// every document, so nothing here is forged — but the node
			// being attested CHOSE which validly-signed vintage to hand
			// over, inside attest.EmbeddedCollateralMaxAge. Naming the
			// source is the whole point: an operator reading this should
			// fix their egress, not conclude the check is equivalent.
			return "PASS — but on collateral THIS NODE supplied, because Intel's PCS and our " +
					"own cache were both unavailable. Intel signed those documents, so they are " +
					"not forged; the node did choose which vintage to serve, so a revocation or " +
					"TCB downgrade published since could be missing. Re-run when PCS is reachable.",
				true, false
		}
	}
	switch {
	case err == nil && stale:
		// Still a PASS — the documents are Intel-signed and the library
		// enforced their own validity windows. But an operator debugging
		// their node must not read this as confirmation that PCS is
		// reachable, because it is the opposite.
		return "PASS — verified against Intel's PCK chain, but Intel's PCS was unreachable " +
			"and cached revocation/TCB collateral was used", true, false
	case err == nil:
		return "PASS — verified against Intel's PCK chain, with revocation and TCB collateral", true, false
	case errors.Is(err, tee.ErrCollateralUnavailable):
		// NAME WHY THE LAST RESORT DID NOT SAVE IT. Both of its refusals are
		// things the operator can act on — their published set is past
		// attest.EmbeddedCollateralMaxAge, or it is incomplete/absent because
		// the node predates 9.7 — and this is the command whose job is
		// answering "why am I being refused". Dropping carriedErr left the
		// daemon's Debug log saying more than the diagnostic.
		line := "NOT CHECKED — Intel PCS was unreachable, which says nothing about this quote: " +
			err.Error()
		if carriedErr != nil {
			line += "\n    the node's own collateral was tried and did not help: " + carriedErr.Error()
		}
		return line, false, false
	case errors.Is(err, tee.ErrStaleCollateralRejected):
		// A FAILURE, not a "not checked", and the distinction is this
		// tool's whole reason for stopping. Collateral WAS consulted and
		// it refused; only the CAUSE is ambiguous. Continuing would print
		// the release-allowlist values a rejected quote names for itself,
		// which is the one output an operator then pastes into their
		// config. Say plainly that the cause is undetermined rather than
		// implying the node is at fault.
		return "FAIL — refused by cached Intel collateral, and the cause cannot be " +
			"determined from here: Intel's PCS is unreachable, so this may be a superseded " +
			"document of ours rather than bad evidence from the node. Re-run when PCS is " +
			"reachable: " + err.Error(), false, true
	default:
		return "FAIL — " + err.Error(), false, true
	}
}

// ---------------------------------------------------------------------------
// File mode
// ---------------------------------------------------------------------------

func (f *teeInspect) inspectFile(policy attest.ComposePolicy) (teeReport, error) {
	raw, err := os.ReadFile(f.file)
	if err != nil {
		return teeReport{}, fmt.Errorf("read %s: %w", f.file, err)
	}
	rep := teeReport{
		Source:               f.file,
		Offline:              true,
		ReleaseMatchEnforced: policy.EnforceComposeSkeleton,
		TrustedSkeletonCount: len(policy.TrustedComposeSkeletons),
		TrustedImageCount:    len(policy.TrustedNodeImages),
		Violations:           []teeFinding{},
	}

	// An app-compose.json is a JSON object; a docker-compose.yaml never is.
	// Both are worth accepting: the first is what a node publishes, the second
	// is what a release ships, and a release list has to be computable from the
	// published file before any node has run it.
	doc := string(raw)
	if !looksLikeJSONObject(doc) {
		skeleton, imageRef, serr := attest.ExtractComposeSkeleton(doc)
		if serr != nil {
			rep.SkeletonError = strings.TrimPrefix(serr.Error(), "attest: ")
		} else {
			rep.SkeletonDigest = attest.ComposeSkeletonDigest(skeleton)
			rep.ImageRef = imageRef
		}
		return rep, nil
	}

	rep.fillFromDocument(doc, selfMeasurement(doc), policy)
	return rep, nil
}

// looksLikeJSONObject distinguishes an app-compose.json from raw compose YAML.
// Only the first non-space byte can decide it, because a document that fails to
// parse as JSON must still be reported as malformed JSON rather than silently
// re-read as YAML — the two produce entirely different reports and guessing
// wrong is how a typo becomes "no violations".
func looksLikeJSONObject(s string) bool {
	return strings.HasPrefix(strings.TrimLeft(s, " \t\r\n"), "{")
}

// selfMeasurement is the document's own digest, handed to VerifyComposeHash so
// the gate passes by construction.
//
// THAT IS THE POINT AND IT IS ALSO THE DANGER. In file mode there is no replay,
// so the gate has nothing real to compare against; making it compare the
// document to itself is the only honest way to reach the content rules, and it
// is exactly why file mode prints notVerifiedBanner instead of a gate line.
// Never reachable from the node path, where measurements come from
// attest.VerifyEventLog.
func selfMeasurement(doc string) map[string]string {
	sum := sha256.Sum256([]byte(doc))
	return map[string]string{attest.EventComposeHash: hex.EncodeToString(sum[:])}
}

// ---------------------------------------------------------------------------
// Shared: everything derived from the document
// ---------------------------------------------------------------------------

// fillFromDocument runs the shared verifier over raw and records what it found.
// measurements must be the replay's answer on the node path; the file path
// substitutes the document's own digest and says so.
func (r *teeReport) fillFromDocument(raw string, measurements map[string]string, policy attest.ComposePolicy) {
	doc, err := attest.VerifyAppCompose(raw, measurements, policy)
	var pe *attest.ComposePolicyError
	switch {
	case err == nil, errors.As(err, &pe):
		// Both carry a parsed document: VerifyAppCompose returns violations
		// alongside it rather than instead of it.
	default:
		r.Stop = "the document could not be read: " + err.Error()
		return
	}
	if pe != nil {
		for _, v := range pe.Violations {
			r.Violations = append(r.Violations, teeFinding{Code: string(v.Code), Detail: v.Detail})
		}
	}
	if doc == nil {
		return
	}
	r.ContentRulesRan = true

	// The two values a release list holds. Computed even when the policy is
	// not enforcing the match — that is the whole reason this command exists,
	// since the list cannot be populated with values nobody can compute.
	skeleton, imageRef, serr := attest.ExtractComposeSkeleton(doc.DockerCompose)
	if serr != nil {
		r.SkeletonError = strings.TrimPrefix(serr.Error(), "attest: ")
	} else {
		r.SkeletonDigest = attest.ComposeSkeletonDigest(skeleton)
		r.ImageRef = imageRef
		// Membership only where the match is live. Under `--file` the policy
		// is this proxy's, so the annotation answers the operator's actual
		// question — "would the payer I am asking about accept this?" — which
		// the raw digest alone does not.
		if policy.EnforceComposeSkeleton {
			skelOK := tee.MeasurementTrusted(policy.TrustedComposeSkeletons, r.SkeletonDigest)
			r.SkeletonTrusted = &skelOK
			// Images compare VERBATIM and case-sensitively, not as hex
			// measurements, so MeasurementTrusted is the wrong predicate here.
			imgOK := slices.Contains(policy.TrustedNodeImages, imageRef)
			r.ImageTrusted = &imgOK
		}
	}

	if doc.PreLaunchScript != "" {
		sum := sha256.Sum256([]byte(doc.PreLaunchScript))
		r.PreLaunchBytes = len(doc.PreLaunchScript)
		r.PreLaunchDigest = hex.EncodeToString(sum[:])
	}
	r.AllowedEnvs = doc.AllowedEnvs
	r.Features = doc.Features
	r.StorageFS = doc.StorageFS
	lkp := doc.LocalKeyProvide
	r.LocalKeyProvider = &lkp
}
