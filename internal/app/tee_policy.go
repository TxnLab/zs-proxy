/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package app

import (
	"log/slog"
	"strings"

	"github.com/TxnLab/zerosignal/go/attest"

	"github.com/TxnLab/zs-proxy/internal/config"
)

// ComposePolicyFromTEEConfig builds the compose content policy this proxy
// enforces: the shared rules from the protocol module, with the three
// reference lists an operator may supply layered on.
//
// The base is the SHARED policy, not a rule set assembled here — the proxy and
// the browser client have to reach the same verdict on the same document, and
// two hand-maintained rule sets is how they stop doing that.
//
// EXPORTED so `zs-proxy tee inspect` reports against the policy this process
// would actually run rather than against the bare shared defaults. A
// diagnostic that answers "why am I being refused" with a different policy
// than the verifier's is worse than none: it sends an operator to fix a rule
// that is not the one refusing them.
//
// log may be nil, for callers that want the policy without the startup notices.
func ComposePolicyFromTEEConfig(cfg config.TEEConfig, log *slog.Logger) attest.ComposePolicy {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	policy := attest.ZeroSignalComposePolicy()

	// The one reference an operator can override, because it is the one Phala
	// controls rather than us: the pre-launch script is injected server-side
	// at deploy time, so its digest moves with no version to track, and the
	// rule is unconditional. Without this an operator meeting that change
	// waits for a zs-proxy release while every new node reads as compromised.
	// Replaces rather than appends — an operator naming a set is naming THE
	// set, and silently keeping ours would make "I removed that digest" a lie.
	if len(cfg.TrustedPreLaunchDigests) > 0 {
		policy.PreLaunchScriptDigests = cfg.TrustedPreLaunchDigests
		log.Warn("zs.tee.trusted_pre_launch_digests overrides the built-in list; "+
			"the node's root boot hook is checked against YOUR digests only",
			slog.Int("count", len(cfg.TrustedPreLaunchDigests)))
	}

	// The per-RELEASE half: which deployment shapes and which images count as
	// published zs-node. Both are config rather than shipped constants because
	// they move on a release cadence the proxy binary does not share.
	//
	// Assigned unconditionally, unlike PreLaunchScriptDigests above, so an
	// explicit `trusted_node_images: []` can CLEAR a future preset — the same
	// semantics trusted_os_images has. A preset, when one exists, therefore
	// belongs in config.defaults() rather than in ZeroSignalComposePolicy(),
	// which this would silently discard.
	policy.TrustedComposeSkeletons = cfg.TrustedComposeSkeletons
	policy.TrustedNodeImages = cfg.TrustedNodeImages

	// ENFORCEMENT IS WRITTEN DOWN, NOT DERIVED — proto/SPEC.md §3e requires
	// it, and the reason is specific to how this file's config is loaded.
	//
	// Deriving it from list emptiness reads as safer than it is. config.Load
	// runs a plain yaml.Unmarshal with no KnownFields(true), so a misspelled
	// `trusted_compose_skeleton` is silently dropped; under a derived flag
	// that is an empty list, no enforcement, and a proxy routing to anything
	// while its config file reads as configured. The same typo with the flag
	// written down enforces over an empty list and refuses every node. Both
	// are operator error and only one is diagnosable, which is the whole of
	// SPEC's argument: "empty means no constraint" is the shape that makes a
	// check vanish while looking like the feature working.
	//
	// This mirrors client/'s ENFORCE_COMPOSE_SKELETON, which is likewise an
	// explicit constant rather than a length test — so the two consumers now
	// decide the flag the same way, from inputs that differ only in kind (a
	// config file here, a build-time constant there).
	policy.EnforceComposeSkeleton = cfg.EnforceComposeSkeleton

	// TURNING THE MATCH OFF CLEARS THE LISTS, and this MUST stay below the
	// assignment above — reading the flag before it is set means reading the
	// zero value, which clears unconditionally.
	//
	// Without the clearing, the single documented off switch does not work at
	// all. Both lists ship SEEDED (config.defaults), so `enforce_compose_
	// skeleton: false` on its own leaves populated lists beside a false flag —
	// which attest reports as ViolationPolicyMisconfigured against the
	// verifier, on every node that publishes a preimage. An operator turning
	// the match off would find every node refused, for a reason that names OUR
	// configuration rather than theirs.
	//
	// The misconfigured rule is aimed at someone who pasted release data and
	// forgot the flag. Seeded defaults make that premise vacuous here: every
	// install has populated lists, so the state stops distinguishing a mistake
	// from a deliberate opt-out. The flag is the written-down decision, so it
	// wins and the reference data goes with it.
	//
	// The rule stays reachable for library callers and for the browser client,
	// which build their policy directly rather than through this function.
	if !policy.EnforceComposeSkeleton &&
		(len(policy.TrustedComposeSkeletons) > 0 || len(policy.TrustedNodeImages) > 0) {
		policy.TrustedComposeSkeletons = nil
		policy.TrustedNodeImages = nil
		log.Warn("zs.tee.enforce_compose_skeleton is false, so the release lists are " +
			"ignored and a node's workload is NOT matched against a published release — " +
			"zs.tee.trusted_compose_hashes is then the only thing identifying a workload, " +
			"and an empty one refuses every node. No node counts as TEE-attested with the " +
			"match off, so TEE-required requests find no node")
	}

	// A blank entry counts toward the length above and matches nothing, so it
	// enforces-and-refuses-everything. That is the safe direction, and it is
	// also silent — the same `- ""` hazard internal/config's tests pin for the
	// measurement allowlists, where containsFold's empty-want guard makes it
	// fail closed with no symptom to read.
	if blanks := countBlank(policy.TrustedComposeSkeletons) + countBlank(policy.TrustedNodeImages); blanks > 0 {
		log.Warn("zs.tee: a release list contains a blank entry, which matches no node and "+
			"cannot be what was meant — check for a stray `- \"\"`",
			slog.Int("blank_entries", blanks))
	}

	populated := len(policy.TrustedComposeSkeletons) > 0 || len(policy.TrustedNodeImages) > 0
	switch {
	case !policy.EnforceComposeSkeleton:
		// The clearing block above already WARNed if there was anything to
		// clear, so there is no populated-but-unenforced case to report here:
		// ViolationPolicyMisconfigured is unreachable through this function by
		// construction. Do not "restore" a branch for it — the state it names
		// cannot exist on a policy built from this config, and a branch that
		// can never fire is worse than none.
		log.Info("zs.tee: enforce_compose_skeleton is off, so a node's workload is not matched " +
			"against a published release — a clean attestation verdict then means 'nothing " +
			"structurally disqualifying', not 'runs software we recognize', and no node counts " +
			"as TEE-attested, so TEE-required requests find no node")
	case !populated:
		// Fail-closed and almost certainly a typo: the flag is the only thing
		// that can produce this, and it refuses every node on the network.
		// Distinguished from the half-populated case below because the remedy
		// differs — here nothing was pasted at all.
		log.Warn("zs.tee: enforce_compose_skeleton is true with both release lists empty, so " +
			"EVERY node is refused — populate trusted_compose_skeletons and " +
			"trusted_node_images, or turn enforcement off")
	case len(policy.TrustedComposeSkeletons) == 0 || len(policy.TrustedNodeImages) == 0:
		log.Warn("zs.tee: only one of trusted_compose_skeletons / trusted_node_images is "+
			"populated, so every node is refused on the empty list — populate both",
			slog.Int("skeletons", len(policy.TrustedComposeSkeletons)),
			slog.Int("images", len(policy.TrustedNodeImages)))
	default:
		log.Info("zs.tee: the release-compose match is enforced",
			slog.Int("skeletons", len(policy.TrustedComposeSkeletons)),
			slog.Int("images", len(policy.TrustedNodeImages)))
	}
	return policy
}

// countBlank counts entries that name nothing. Deliberately the ASCII trim
// attest itself uses for values, not strings.TrimSpace: this only decides
// whether to print a notice, and matching attest's predicate keeps the notice
// describing the same set of entries the policy will fail to match.
func countBlank(list []string) int {
	n := 0
	for _, v := range list {
		if strings.Trim(v, " \t\n\v\f\r") == "" {
			n++
		}
	}
	return n
}
