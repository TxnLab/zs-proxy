/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/attest"
	"github.com/TxnLab/zerosignal/go/selection"
	"github.com/TxnLab/zerosignal/go/solana"
	"github.com/TxnLab/zerosignal/go/solana/rpc"
	"github.com/TxnLab/zerosignal/go/ticket"
	"github.com/TxnLab/zerosignal/go/zs"
)

// EnvConcurrentSlots is the environment variable that overrides
// zs.concurrent_slots. Exported because `zs-proxy slots` must warn when it is
// set — env beats the file, so a written value would otherwise never take
// effect while the command reported success. A private copy of the string on
// the CLI side could drift from the one applyEnv reads, and the drift is
// invisible: the warning simply stops firing.
const EnvConcurrentSlots = "PROXY_ZS_CONCURRENT_SLOTS"

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Algod   algod.Config  `yaml:"algod"`
	Solana  SolanaConfig  `yaml:"solana"`
	Logging LoggingConfig `yaml:"logging"`
	// Zs is the operator/escrow config section, YAML key `zs:`.
	Zs    ZsConfig    `yaml:"zs"`
	Spend SpendConfig `yaml:"spend"`
}

// SolanaConfig selects the Solana rail: setting Cluster or RPCEndpoint pays on
// Solana instead of Algorand, and then the algod and escrow_app_id settings are
// refused rather than ignored, so a config can't read as one rail while paying
// on the other. Cluster is a preset ("devnet" or "localnet"); RPCEndpoint
// overrides its endpoint verbatim, since a provider's key may ride in its path
// or query. Program is the zs_escrow program id in base58; empty takes the
// preset's. The USDC mint is read from the program's Config.
type SolanaConfig struct {
	Cluster     string `yaml:"cluster"`
	RPCEndpoint string `yaml:"rpc_endpoint"`
	Program     string `yaml:"program"`

	// ResolvedProgram is Program parsed, which validation sets, as
	// algod.Config's ResolvedEndpoint is: a caller of app.Run that skips
	// Finalize gets the preset's program.
	ResolvedProgram solana.PublicKey `yaml:"-"`
}

// OnSolana reports whether the config pays on the Solana rail.
func (c *Config) OnSolana() bool {
	return c.Solana.Cluster != "" || c.Solana.RPCEndpoint != ""
}

// NetworkName names the network for display and for the network a new wallet
// records: the algod network on Algorand, solana-<cluster> on Solana.
func (c *Config) NetworkName() string {
	if !c.OnSolana() {
		return c.Algod.Network
	}
	if c.Solana.Cluster == "" {
		return "solana-custom"
	}
	return "solana-" + strings.ToLower(c.Solana.Cluster)
}

// SpendConfig bounds the *aggregate* USDC the auto-spending wallet can drain —
// a safety backstop on top of the protocol's per-request max_price (which only
// bounds a single request). The proxy opens an escrow signing real USDC on
// every request with no human in the loop, so pointed at an agent loop a
// runaway could empty the wallet; these caps are enforced before each open.
// Both are in whole USDC (e.g. 25.00); 0 means unlimited for that axis. The
// daily tally is persisted under the state dir so a restart doesn't reset it.
type SpendConfig struct {
	DailyCapUSDC      float64 `yaml:"daily_cap_usdc"`
	PerRequestCapUSDC float64 `yaml:"per_request_cap_usdc"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// ZsConfig controls the encrypted-envelope dispatch layer. Outbound
// requests are always wrapped in the zs envelope format, dispatched to
// one of the operators sourced from the chain per SPEC.md § 3a, and
// decrypted on the way back.
//
// OperatorIDs lists the specific on-chain operator IDs the proxy should use.
// When empty or omitted, all currently-registered operators are fetched from
// the ZeroSignalEscrow contract at startup. Operator addresses, age public keys,
// and base URLs are always read from the chain — the config carries only IDs
// as a filter/allowlist, not the full operator details.
//
// AffinityPolicy controls how strictly the proxy re-routes tool-call
// continuation requests back to the operator that served the previous
// turn; see zs.AffinityPolicy.
type ZsConfig struct {
	OperatorIDs    []uint64      `yaml:"operator_ids"`
	AffinityPolicy string        `yaml:"affinity_policy"`
	AffinityTTL    time.Duration `yaml:"affinity_ttl"`
	AffinityMax    int           `yaml:"affinity_max"`

	// DefaultMaxOutputTokens is a per-model override for the
	// max_output_tokens ReserveRequest field when the request body has
	// no max_tokens / max_completion_tokens / max_output_tokens. A model
	// without an entry falls through to FallbackMaxOutputTokens.
	DefaultMaxOutputTokens map[string]uint64 `yaml:"default_max_output_tokens"`

	// FallbackMaxOutputTokens is the global catch-all used when the request
	// body omits max_tokens / max_completion_tokens / max_output_tokens AND
	// the model has no per-model DefaultMaxOutputTokens entry. Standard Chat
	// Completions treats the field as optional, so most clients (IDEs, SDKs)
	// never send it; this keeps the proxy OpenAI-compatible for them.
	//
	// It is not applied as a flat ceiling: in this omitted-and-no-default
	// case the proxy sizes the reserve PER-OPERATOR from each operator's
	// declared capacity (selection.DeriveMaxOutput, mirroring the client) —
	// a declared max_output verbatim, else a proportionate slice of the
	// operator's context window, else this value as the flat fallback — with
	// this value as the base ceiling. So a small-window operator isn't
	// excluded on a guessed ceiling it never has to honor. The node bills
	// against actual usage from its signed receipt and refunds the unspent
	// reserve margin, so an over-generous ceiling only widens the (recovered)
	// reserve — runaway spend stays bounded by SpendConfig.
	//
	// Default selection.DefaultMaxOutputCeiling (32768), which is also the
	// client's DEFAULT_MAX_OUTPUT_TOKENS. Keep the two equal: the sizing filter
	// evaluates Fits against the DERIVED number, so a proxy-only ceiling makes
	// the proxy and the client disagree about which operators are eligible for
	// the same request (at ctx 32768 with 28000 input, a 4096 ceiling keeps the
	// operator and an 8192 one drops it), and the proxy is meant to route
	// identically to the client. The value is also large enough that an omitting
	// coding client's big single turn (file writes/refactors) isn't truncated.
	//
	// Set to 0 to fail closed instead: dispatch then rejects an omitting caller
	// with max_output_required rather than sizing a reservation.
	FallbackMaxOutputTokens uint64 `yaml:"fallback_max_output_tokens"`

	// EscrowAppID is the deployed ZeroSignalEscrow application id. Required.
	EscrowAppID uint64 `yaml:"escrow_app_id"`

	// ConcurrentSlots is how many escrow tickets this payer can hold open at
	// once — parallel tool calls plus the settle grace window, not a lifetime
	// count. It is a working-set size, not a hard limiter: the proxy owns no
	// ticket semaphore, so the mechanism is funding. Each ticket box needs
	// box MBR, which open() draws from the payer's prepaid pool instead of a
	// per-turn ALGO leg, so the proxy auto-deposits `slots × mbrForTicket()`
	// (≈0.115 ALGO/slot, fully recoverable) on the first served request and
	// on `zs fund` / `zs slots <n>`. Default 10 (seeded in defaults()); 0
	// falls back to the built-in default. See proto/SPEC.md "Settlement asset
	// model".
	ConcurrentSlots uint64 `yaml:"concurrent_slots"`

	// Privacy enables transport-privacy mode (single-hop operator-as-relay,
	// SPEC.md § 3f): every operator-bound HTTP request — inference, reserve,
	// and discovery probes — is routed through another operator acting as a
	// relay so the target never sees the proxy's IP, and the reserve body is
	// sealed so the relay can't read the caller's payer_addr. Unset, it is on
	// (as of the 3.0 cutover), except on a local chain (the localnet preset at
	// a loopback endpoint, zs.Deployment.Local), whose one node has no relay
	// owned by someone else. `privacy: false` or `PROXY_ZS_PRIVACY=false`
	// turns it off, which makes the proxy talk directly to the target,
	// byte-identical to pre-relay behavior; `true` keeps it on even locally.
	// A pointer so the SDK can tell unset from off. See
	// proto/go/wire/version.go (3.0).
	Privacy *bool `yaml:"privacy"`

	// AllowPrivacyOverride governs whether a caller may weaken transport-privacy
	// per request via `provider.relay: "off"` (or the `X-Zs-Relay: off`
	// header), which skips the relay hop and sends directly to the target.
	// Default true: the proxy is a localhost single-user (payer) process, so the
	// caller is the owner and disabling their own privacy is a legitimate
	// latency tradeoff. Set false (or `PROXY_ZS_ALLOW_PRIVACY_OVERRIDE=false`)
	// to forbid it — e.g. when the proxy is exposed to other local apps and the
	// owner wants privacy enforced regardless of what a caller requests. Only the
	// *weakening* direction is gated: `relay: "required"` (force a relay, or
	// hard-fail) is always honored since it can only strengthen privacy.
	AllowPrivacyOverride bool `yaml:"allow_privacy_override"`

	// AllowPrivateOperators disables the SSRF dial guard on the proxy's
	// operator-facing HTTP clients (inference forward, reserve, and discovery
	// probes). Unset, those clients refuse to dial an operator whose on-chain
	// BaseURL — or the relay BaseURL a request is routed through — resolves
	// to a private / loopback / link-local / CGNAT / ULA address, except on a
	// local chain (as for Privacy), whose nodes are on this machine. The proxy
	// is co-located with the user, so its localhost is the user's machine;
	// without the guard a malicious operator could register such a BaseURL
	// and make the proxy probe the user's own LAN. Set to true (or
	// PROXY_ZS_ALLOW_PRIVATE_OPERATORS=true) ONLY for a dev chain reached
	// another way, where operators legitimately run on 10.x; never in
	// production. A pointer so the SDK can tell unset from false.
	AllowPrivateOperators *bool `yaml:"allow_private_operators"`

	// AllowStaging includes nodes whose on-chain NodeRecord.staging flag is set
	// in target selection. By default (false) the proxy drops staging nodes, so
	// the standard routing path never targets a node an operator brought up only
	// for testing. Set to true (or PROXY_ZS_ALLOW_STAGING=true, or
	// `--allow-staging` on `proxy start`) to route to staging nodes — e.g. when
	// validating your own node before flipping it to production. Feeds
	// selection.Constraints.AllowStaging, mirroring the client's Model Routing
	// "allow staging" toggle so both route identically.
	AllowStaging bool `yaml:"allow_staging"`

	// ProxyPayerAddr is the Algorand address the wallet signs both
	// group members with. Must be present in the keystore loaded at
	// startup. Optional in config when exactly one mnemonic is loaded —
	// main.go auto-derives it from the lone keystore entry in that case.
	// When two or more mnemonics are loaded this field is required to
	// disambiguate.
	ProxyPayerAddr string `yaml:"proxy_payer_addr"`

	// SettlementGrace is informational on the proxy side — the
	// contract enforces the canonical value. The proxy uses it for
	// client-visible timeout hints when a response lags past the
	// refund-inactive window.
	SettlementGrace time.Duration `yaml:"settlement_grace"`

	// RefreshInterval is how often the proxy re-fetches the on-chain
	// operator set and re-probes each operator's /v1/zs/details
	// endpoint. Default zs.DefaultRefreshInterval (5m).
	RefreshInterval time.Duration `yaml:"refresh_interval"`

	// RefreshTimeout bounds a single refresh cycle (chain read, the
	// signing-account balance pass, then the per-operator details probes).
	// Default zs.DefaultRefreshTimeout (30s). The balance pass runs
	// before the probes and takes up to 10s of this budget on a large
	// fleet or a slow algod. A cycle that exceeds this deadline is
	// abandoned and the last-known-good snapshot is retained.
	RefreshTimeout time.Duration `yaml:"refresh_timeout"`

	// MaxCacheAge is the hard ceiling on snapshot age. A registry
	// read that observes a snapshot older than this triggers a
	// synchronous refresh before returning. Zero or negative takes
	// zs.DefaultMaxCacheAge (15m) — should be comfortably larger
	// than RefreshInterval so the lazy path rarely fires during
	// healthy operation.
	MaxCacheAge time.Duration `yaml:"max_cache_age"`

	// MaxCarryAge is the discovery/dispatch carry window: how long a node
	// keeps advertising its last-probed capabilities after its most recent
	// successful probe. Default zs.DefaultMaxCarryAge (90m). It MUST exceed
	// the sweep period (~ node count / probe_budget * refresh_interval) or nodes
	// flicker out of discovery between bounded-probing sweeps — raise it for
	// large fleets or a small probe_budget.
	MaxCarryAge time.Duration `yaml:"max_carry_age"`

	// ProbeBudget caps how many per-operator /v1/zs/details probes one
	// background refresh issues once the fleet exceeds it (bounded probing).
	// Default zs.DefaultProbeBudget (96); a fleet at or under the budget is
	// probed in full. Keep budget/details_concurrency * details_timeout under
	// refresh_timeout minus the 10s balance pass so a cycle completes.
	ProbeBudget int `yaml:"probe_budget"`

	// TargetTopK is how many of a demanded model's fastest known servers a
	// bounded cycle re-probes. Default zs.DefaultTargetTopK (8).
	TargetTopK int `yaml:"target_topk"`

	// CatalogPath is where the routing/discovery catalog is persisted so a
	// restart warm-starts from the last-known fleet instead of rediscovering it
	// over probe cycles. Empty resolves to
	// StateDir/operator-catalog-<deployment key>.json; set to "off" to disable
	// persistence. An explicit path is used verbatim, so it is shared across
	// networks.
	CatalogPath string `yaml:"catalog_path"`

	// DetailsTimeout bounds a single per-operator /v1/zs/details
	// probe. Default zs.DefaultDetailsFetchTimeout (2s). Keep this
	// small so a slow operator doesn't starve the rest of the cycle.
	DetailsTimeout time.Duration `yaml:"details_timeout"`

	// DetailsConcurrency caps how many per-operator details probes run at
	// once in a refresh cycle. Default zs.DefaultDetailsConcurrency (24).
	// Bounds the probe fan-out so a large fleet doesn't open a socket per
	// node at once and blow the refresh timeout.
	DetailsConcurrency int `yaml:"details_concurrency"`

	// LatencyStateMax caps the live entry count of each client-observed
	// latency EWMA map. Default zs.DefaultLatencyStateMax (4096). The
	// (relay,target) pair map is the O(n²)-shaped one; this is a hard backstop
	// on top of the natural staleness prune.
	LatencyStateMax int `yaml:"latency_state_max"`

	// ReserveTimeout bounds a single POST /v1/zs/reserve to a
	// candidate operator. Default zs.DefaultReserveTimeout (5s).
	// Cross-internet operators whose reserve path includes algod
	// simulation in escrow.ComposeOpenGroup may need this raised
	// (10–30s) — too small a value here turns slow operators into
	// silent operators_busy responses to the client. The dispatcher
	// still iterates to the next candidate on a timeout, so raising
	// this only delays the failover, never blocks the request
	// indefinitely.
	ReserveTimeout time.Duration `yaml:"reserve_timeout"`

	// TEE governs the proxy-side verification of operator
	// confidential-mode (TEE) attestation evidence and the per-request
	// routing filter. Operators that don't advertise TEE on
	// /v1/zs/details are unaffected. See proto/TEE.md for the
	// threat model.
	TEE TEEConfig `yaml:"tee"`

	ResolvedAffinity zs.AffinityPolicy `yaml:"-"`
}

// TEEVerifierBuiltin selects the verifier compiled into this proxy —
// the only kind wired today, and the value the empty string normalizes
// to. The field shape stays so an out-of-process or alternative-vendor
// verifier can land later without a YAML migration.
const TEEVerifierBuiltin = "builtin"

// TEEVerifierLegacyNRAS is accepted as an alias for
// TEEVerifierBuiltin and normalized away on load.
//
// It was the original name, and it was wrong in the way that costs an
// afternoon: the builtin verifier's ONLY working path is Intel TDX via
// dstack, which never contacts NVIDIA's Remote Attestation Service at
// all. Reading the config told an operator the proxy was doing
// something it wasn't, and the obvious correction — writing the
// platform name they could see in their own logs, `dstack-tdx` —
// failed load with "not supported", which reads as the feature being
// absent rather than the value being spelled differently.
//
// Kept rather than removed because config.example.yaml shipped it, so
// somebody's file has it. Removing it would fail their next start with
// an error about a value we told them to write.
const TEEVerifierLegacyNRAS = "nras"

// TEEConfig configures the proxy's relying-party side of the
// confidential-mode protocol described in proto/TEE.md. AllowStub
// gates dev-laptop pipelines (matching the node's tee.mode=stub);
// production deployments leave it false. RequireForModels is a
// blanket policy that forces the TEE filter for every request whose
// model id appears in the list — clients can additionally opt in
// per-request via the X-Zs-Require-TEE header.
type TEEConfig struct {
	Verifier         string   `yaml:"verifier"`
	AllowStub        bool     `yaml:"allow_stub"`
	RequireForModels []string `yaml:"require_for_models"`

	// TrustedComposeHashes and TrustedOSImages are the known-good
	// measurement allowlists for platforms that publish a runtime
	// measurement log (tee.mode=dstack-tdx). A quote's signature says
	// "some software in a real TEE asked me to sign these bytes"; only
	// matching its measurement against a reference says WHICH software
	// — and that is the step the whole key-binding guarantee rests on,
	// because report_data is signed blindly.
	//
	// Values are lowercase hex digests, no 0x prefix.
	//
	// EMPTY MEANS NOTHING VERIFIES, deliberately. An empty allowlist
	// read as "accept anything" would turn an unconfigured proxy into
	// one that accepts a quote from any CVM running any image — which
	// is precisely the check being skipped, and it would look like the
	// feature working.
	//
	// A config list is the source of truth today; a transparency log
	// replaces it later.
	// TrustedOSImages is REQUIRED in production rather than optional
	// hardening. Without it a node can present a perfectly valid quote
	// taken on a dstack DEVELOPMENT OS image — where every other check
	// in the chain passes identically and the platform's hardening
	// guarantees do not hold.
	//
	// It can only narrow: the verifier also requires the image's boot
	// chain from attest.TrustedDstackBootChains, so an image added here
	// with no row there is refused. There is deliberately no config key
	// for rows — they are measured values, and a served or typed-in row
	// would let whoever wrote it decide which kernels count.
	//
	// The membership test deliberately lives with the verifier
	// (tee.DstackVerifier), next to the failure tag it produces,
	// rather than as a method here. A second copy on the config type
	// is the kind that gets called later and drifts from the one that
	// decides routing.
	TrustedComposeHashes []string `yaml:"trusted_compose_hashes"`
	TrustedOSImages      []string `yaml:"trusted_os_images"`

	// TrustedPreLaunchDigests overrides the compose policy's list of
	// recognized dstack pre-launch scripts — the ~17KB of bash Phala runs
	// as root before the container starts.
	//
	// IT EXISTS BECAUSE THIS ONE REFERENCE IS NOT OURS TO SCHEDULE. The
	// script is not in the `phala` CLI (verified against phala@1.1.21); the
	// Phala Cloud API injects it server-side at deploy time, so its digest
	// moves whenever Phala edits their template — no version, no release
	// note, nothing to track. The rule is unconditional, so the first time
	// that happens every newly deployed node is refused with
	// measurement_mismatch while behaving perfectly, and without this an
	// operator's only recourse is waiting for a zs-proxy release.
	//
	// EMPTY MEANS "use the built-in list", NOT "accept anything" — the
	// opposite of trusted_os_images above, and the difference is worth
	// keeping straight. There the empty case is the unconfigured state and
	// must fail closed. Here the built-in default is already a real
	// reference, so empty is simply "no override"; the fail-open reading
	// would be an entry of `""`, which containsFold's empty-want guard
	// refuses like every other list in this package.
	//
	// Setting it blesses whatever bash that digest names, sight unseen. Read
	// the script first — `curl -s <node>/v1/zs/attestation` carries the
	// document it hashes.
	TrustedPreLaunchDigests []string `yaml:"trusted_pre_launch_digests"`

	// TrustedComposeSkeletons and TrustedNodeImages are the per-RELEASE
	// half of the compose check: which published zs-node deployment
	// SHAPES, and which published images, this proxy will accept. Both
	// are read by attest.ComposePolicy's template match — see
	// attest.ExtractComposeSkeleton for what a skeleton is and why the
	// two lists move on different cadences.
	//
	// Values come from `zs-proxy tee inspect`, which computes them from
	// a deployed node's published document. Nothing else can: a skeleton
	// digest is over text this proxy never assembles, so hand-deriving
	// one means reimplementing the extractor.
	//
	// Populating these does NOT turn the check on —
	// EnforceComposeSkeleton below does, and the split is deliberate.
	TrustedComposeSkeletons []string `yaml:"trusted_compose_skeletons"`
	TrustedNodeImages       []string `yaml:"trusted_node_images"`

	// EnforceComposeSkeleton turns the release match on. It is a
	// written-down flag rather than being derived from whether the two
	// lists above are empty, and proto/SPEC.md §3e requires exactly that:
	// "empty means no constraint" is the shape that makes a check vanish
	// while looking like the feature working.
	//
	// The concrete failure that shape permits, in this file specifically:
	// Load uses a plain yaml.Unmarshal with no KnownFields(true), so a
	// misspelled `trusted_compose_skeleton` is silently discarded. Under a
	// derived flag that yields an empty list, no enforcement, and a proxy
	// that routes to anything while its config reads as configured. With
	// the flag written down the same typo enforces over an empty list and
	// refuses every node — loud, immediate, and trivially diagnosed.
	//
	// So the two error directions are not symmetric, and this picks the
	// one that fails closed. The cost is that `true` with both lists empty
	// is reachable by typing; that state refuses everything, which is the
	// safe direction, and ComposePolicyFromTEEConfig logs it.
	//
	// Populated lists with this false is the converse defect. SPEC calls
	// it out as a violation against the VERIFIER — attest reports
	// ViolationPolicyMisconfigured — because it performs no check while
	// appearing configured. Startup surfaces it rather than treating it as
	// a lenient policy.
	EnforceComposeSkeleton bool `yaml:"enforce_compose_skeleton"`
}

// defaultTrustedOSImages are the PRODUCTION dstack guest images, seeded so an
// operator does not hand-paste a value that is identical fleet-wide.
//
// THE LIST ITSELF LIVES IN attest.TrustedDstackOSImages, not here, because a
// second Go consumer arrived: a node appraising its own ACI/1 upstream asks the
// same question about the same fleet of guest images. Two Go copies reaching
// different verdicts on identical evidence is the specific failure the shared
// attest module exists to prevent, so the values, the dev-image rule and the
// "nothing pins the GPU digest" warning are all recorded there. This function
// stays as the config seam — the field is operator-overridable — and keeps the
// fresh-slice-per-call contract, which the shared helper also honours. The
// browser client reads the same list through the TS SDK's generated twin.
func defaultTrustedOSImages() []string {
	return attest.TrustedDstackOSImages()
}

// IsRequiredForModel reports whether the blanket TEE-required policy
// covers this model. Case-sensitive — model ids on the wire are
// already canonicalized by the time they reach dispatch.
func (c TEEConfig) IsRequiredForModel(model string) bool {
	for _, m := range c.RequireForModels {
		if m == model {
			return true
		}
	}
	return false
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			// 9376 is unassigned in the IANA port registry and absent from
			// /etc/services. The previous default, :8080, is llama.cpp's own
			// default — i.e. exactly what zs-node points its local upstream at
			// — so a node and a proxy on one box raced for the same socket.
			Listen:       "127.0.0.1:9376",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 0,
			IdleTimeout:  120 * time.Second,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		Zs: ZsConfig{
			// Privacy and AllowPrivateOperators are left unset: the SDK
			// defaults them from the network (see their fields).
			// Callers may weaken their own privacy per request by default
			// (single-user payer process). Same load order — set
			// `allow_privacy_override: false` / PROXY_ZS_ALLOW_PRIVACY_OVERRIDE=false
			// to forbid `provider.relay: off`.
			AllowPrivacyOverride: true,
			// Graceful max_output ceiling for OpenAI-compatible clients
			// (IDEs, SDKs) that omit the field. It's the base for the
			// per-operator derivation, not a flat value — sized for
			// coding-style single turns (large file writes/refactors) without
			// truncating; the node reconciles actual usage and refunds the
			// unused reserve, so a generous ceiling only widens the (recovered)
			// escrow. Same load order as above — an explicit
			// `fallback_max_output_tokens: 0` in YAML/env overrides this back to
			// fail-closed.
			FallbackMaxOutputTokens: selection.DefaultMaxOutputCeiling,
			// Concurrent open tickets, backed by the prepaid ticket-MBR pool
			// the auto-deposit sizes. Same load order — an explicit
			// `concurrent_slots:` in YAML / PROXY_ZS_CONCURRENT_SLOTS
			// overrides; 0 falls back to zs.DefaultDepositSlots.
			ConcurrentSlots: 10,
			TEE: TEEConfig{
				Verifier: TEEVerifierBuiltin,
				// SEEDED, unlike TrustedComposeHashes, and the asymmetry is
				// the point. An OS image digest is identical for every
				// operator on a given dstack release and moves only when
				// Phala ships a new guest image, so making each payer paste
				// the same fleet-wide constant bought nothing and cost the
				// check whenever they didn't. A compose hash is
				// per-deployment and has no correct default.
				//
				// This does NOT weaken the fail-closed rule: an explicit
				// `trusted_os_images: []` in YAML still replaces this with an
				// empty slice, and containsHexFold refuses an empty `want`,
				// so nothing verifies in that state.
				TrustedOSImages: defaultTrustedOSImages(),

				// The release match, on by default. Unlike the compose-hash
				// pin above there IS a correct fleet-wide value here — a
				// published release is the same artifact for every operator —
				// so an unconfigured proxy gets the strong check.
				//
				// The flag is seeded alongside the lists deliberately: SPEC
				// §3e makes populated-lists-with-the-flag-off a defect
				// (ViolationPolicyMisconfigured), so shipping the lists
				// without it would put every default install in that state.
				//
				// Same fail-closed rule as trusted_os_images: an explicit
				// `trusted_node_images: []` still replaces this with an empty
				// slice, and with the flag on that refuses every node. An
				// operator turning the match OFF sets
				// enforce_compose_skeleton: false, which is the written-down
				// decision the flag exists to require.
				TrustedComposeSkeletons: attest.TrustedComposeSkeletons(),
				TrustedNodeImages:       attest.TrustedNodeImages(),
				EnforceComposeSkeleton:  true,
			},
		},
	}
}

// Load reads YAML from path (missing file is tolerated), applies PROXY_* env
// overrides, resolves the algod endpoint, validates required fields, and
// returns the resolved config. It is LoadBase + Finalize — kept for callers
// that don't inject network presets between the two (tests, the legacy path).
func Load(path string) (*Config, error) {
	cfg, err := LoadBase(path)
	if err != nil {
		return nil, err
	}
	if err := Finalize(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadBase reads YAML from path (missing file is tolerated) and applies
// PROXY_* env overrides, returning a config that is NOT yet network-resolved
// or validated. The CLI calls this, stamps embedded per-network presets
// (algod.network + zs.escrow_app_id) onto the result, then calls Finalize
// — so a config-less run can boot from embedded defaults.
func LoadBase(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// tolerated — all required values may come from env or
			// embedded network presets applied by the caller.
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(cfg)
	return cfg, nil
}

// Finalize resolves the algod endpoint from algod.network and validates all
// required fields. Call it after any post-load mutation (network presets,
// port override). On Solana there is no algod to resolve.
func Finalize(cfg *Config) error {
	if !cfg.OnSolana() {
		if err := cfg.Algod.Resolve(); err != nil {
			return err
		}
	}
	return validate(cfg)
}

func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("PROXY_SERVER_LISTEN"); ok {
		cfg.Server.Listen = v
	}
	if v, ok := lookupDuration("PROXY_SERVER_READ_TIMEOUT"); ok {
		cfg.Server.ReadTimeout = v
	}
	if v, ok := lookupDuration("PROXY_SERVER_WRITE_TIMEOUT"); ok {
		cfg.Server.WriteTimeout = v
	}
	if v, ok := lookupDuration("PROXY_SERVER_IDLE_TIMEOUT"); ok {
		cfg.Server.IdleTimeout = v
	}

	if v, ok := os.LookupEnv("PROXY_ALGOD_NETWORK"); ok {
		cfg.Algod.Network = v
	}
	if v, ok := os.LookupEnv("PROXY_ALGOD_ENDPOINT"); ok {
		cfg.Algod.Endpoint = v
	}
	if v, ok := os.LookupEnv("PROXY_ALGOD_TOKEN"); ok {
		cfg.Algod.Token = v
	}

	if v, ok := os.LookupEnv("PROXY_SOLANA_CLUSTER"); ok {
		cfg.Solana.Cluster = v
	}
	if v, ok := os.LookupEnv("PROXY_SOLANA_RPC_ENDPOINT"); ok {
		cfg.Solana.RPCEndpoint = v
	}
	if v, ok := os.LookupEnv("PROXY_SOLANA_PROGRAM"); ok {
		cfg.Solana.Program = v
	}

	if v, ok := os.LookupEnv("PROXY_LOGGING_LEVEL"); ok {
		cfg.Logging.Level = v
	}
	if v, ok := os.LookupEnv("PROXY_LOGGING_FORMAT"); ok {
		cfg.Logging.Format = v
	}

	if v, ok := os.LookupEnv("PROXY_ZS_AFFINITY_POLICY"); ok {
		cfg.Zs.AffinityPolicy = v
	}
	if v, ok := lookupDuration("PROXY_ZS_AFFINITY_TTL"); ok {
		cfg.Zs.AffinityTTL = v
	}
	if v, ok := os.LookupEnv("PROXY_ZS_AFFINITY_MAX"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Zs.AffinityMax = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_ESCROW_APP_ID"); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Zs.EscrowAppID = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_PROXY_PAYER_ADDR"); ok {
		cfg.Zs.ProxyPayerAddr = v
	}
	if v, ok := os.LookupEnv("PROXY_ZS_FALLBACK_MAX_OUTPUT_TOKENS"); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Zs.FallbackMaxOutputTokens = n
		}
	}
	if v, ok := os.LookupEnv(EnvConcurrentSlots); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			cfg.Zs.ConcurrentSlots = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_PRIVACY"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Zs.Privacy = new(b)
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_ALLOW_PRIVACY_OVERRIDE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Zs.AllowPrivacyOverride = b
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_ALLOW_PRIVATE_OPERATORS"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Zs.AllowPrivateOperators = new(b)
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_ALLOW_STAGING"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Zs.AllowStaging = b
		}
	}
	if v, ok := lookupDuration("PROXY_ZS_SETTLEMENT_GRACE"); ok {
		cfg.Zs.SettlementGrace = v
	}
	if v, ok := lookupDuration("PROXY_ZS_REFRESH_INTERVAL"); ok {
		cfg.Zs.RefreshInterval = v
	}
	if v, ok := lookupDuration("PROXY_ZS_REFRESH_TIMEOUT"); ok {
		cfg.Zs.RefreshTimeout = v
	}
	if v, ok := lookupDuration("PROXY_ZS_MAX_CACHE_AGE"); ok {
		cfg.Zs.MaxCacheAge = v
	}
	if v, ok := lookupDuration("PROXY_ZS_MAX_CARRY_AGE"); ok {
		cfg.Zs.MaxCarryAge = v
	}
	if v, ok := os.LookupEnv("PROXY_ZS_PROBE_BUDGET"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Zs.ProbeBudget = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_TARGET_TOPK"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Zs.TargetTopK = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_CATALOG_PATH"); ok {
		cfg.Zs.CatalogPath = v
	}
	if v, ok := lookupDuration("PROXY_ZS_DETAILS_TIMEOUT"); ok {
		cfg.Zs.DetailsTimeout = v
	}
	if v, ok := os.LookupEnv("PROXY_ZS_DETAILS_CONCURRENCY"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Zs.DetailsConcurrency = n
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_LATENCY_STATE_MAX"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Zs.LatencyStateMax = n
		}
	}
	if v, ok := lookupDuration("PROXY_ZS_RESERVE_TIMEOUT"); ok {
		cfg.Zs.ReserveTimeout = v
	}

	if v, ok := lookupFloat("PROXY_SPEND_DAILY_CAP_USDC"); ok {
		cfg.Spend.DailyCapUSDC = v
	}
	if v, ok := lookupFloat("PROXY_SPEND_PER_REQUEST_CAP_USDC"); ok {
		cfg.Spend.PerRequestCapUSDC = v
	}

	if v, ok := os.LookupEnv("PROXY_ZS_TEE_VERIFIER"); ok {
		cfg.Zs.TEE.Verifier = v
	}
	if v, ok := os.LookupEnv("PROXY_ZS_TEE_ALLOW_STUB"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Zs.TEE.AllowStub = b
		}
	}
	if v, ok := os.LookupEnv("PROXY_ZS_TEE_REQUIRE_FOR_MODELS"); ok {
		// Comma-separated. Empty entries dropped so PROXY_..._MODELS=""
		// resets to the empty list rather than [""].
		parts := strings.Split(v, ",")
		out := parts[:0]
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		cfg.Zs.TEE.RequireForModels = out
	}
}

func lookupDuration(key string) (time.Duration, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}

func lookupFloat(key string) (float64, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func validate(cfg *Config) error {
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	switch cfg.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug|info|warn|error, got %q", cfg.Logging.Level)
	}
	switch cfg.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format must be one of text|json, got %q", cfg.Logging.Format)
	}
	if cfg.Spend.DailyCapUSDC < 0 {
		return fmt.Errorf("spend.daily_cap_usdc must be >= 0, got %g", cfg.Spend.DailyCapUSDC)
	}
	if cfg.Spend.PerRequestCapUSDC < 0 {
		return fmt.Errorf("spend.per_request_cap_usdc must be >= 0, got %g", cfg.Spend.PerRequestCapUSDC)
	}
	if err := validateAffinity(cfg); err != nil {
		return err
	}
	if err := validatePayment(cfg); err != nil {
		return err
	}
	if err := validateTEE(cfg); err != nil {
		return err
	}
	return nil
}

// validateTEE normalizes Verifier — empty or the legacy "nras"
// alias both become TEEVerifierBuiltin — and rejects unknown kinds. The
// AllowStub field is a runtime-time concern — a loud Info log fires at
// proxy startup so dev/test deployments don't quietly mask a missing
// production AllowStub=false. RequireForModels needs no normalization.
//
// The error names a PLATFORM as the likely mistake because that is the
// mistake: this field selects a verifier implementation, not the
// hardware it verifies, and the platform is derived from the quote
// bytes rather than configured. Somebody who has just read
// `dstack-tdx` off their own logs will try it here first.
func validateTEE(cfg *Config) error {
	switch cfg.Zs.TEE.Verifier {
	case "", TEEVerifierLegacyNRAS:
		cfg.Zs.TEE.Verifier = TEEVerifierBuiltin
	case TEEVerifierBuiltin:
	default:
		return fmt.Errorf("zs.tee.verifier %q is not supported (use %q, or omit it) — "+
			"this selects the verifier, not the TEE platform; the platform is read "+
			"from the quote and needs no configuration",
			cfg.Zs.TEE.Verifier, TEEVerifierBuiltin)
	}
	return nil
}

// validatePayment enforces that all escrow-related fields are
// present and well-formed. ProxyPayerAddr may be left empty here:
// app.Run's resolveProxyPayerAddr fills it in from the keys once they load.
// A value that is supplied is still format-checked, so an obviously
// malformed YAML/env value fails before any key is unlocked.
func validatePayment(cfg *Config) error {
	if cfg.OnSolana() {
		return validateSolana(cfg)
	}
	if cfg.Solana.Program != "" {
		return fmt.Errorf("solana.program is set without solana.cluster or solana.rpc_endpoint, which select the Solana rail")
	}
	if cfg.Algod.ResolvedEndpoint == "" {
		return fmt.Errorf("algod.network (or algod.endpoint) is required")
	}
	if !strings.HasPrefix(cfg.Algod.ResolvedEndpoint, "http://") && !strings.HasPrefix(cfg.Algod.ResolvedEndpoint, "https://") {
		return fmt.Errorf("algod.endpoint must start with http:// or https://")
	}
	if cfg.Zs.EscrowAppID == 0 {
		return fmt.Errorf("zs.escrow_app_id is required")
	}
	if cfg.Zs.ProxyPayerAddr != "" {
		if _, err := ticket.DecodeAlgorandAddress(cfg.Zs.ProxyPayerAddr); err != nil {
			return fmt.Errorf("zs.proxy_payer_addr: %w", err)
		}
	}
	return nil
}

// validateSolana checks the Solana rail's fields and resolves them as the SDK
// will, so a bad cluster or endpoint fails at load rather than at the first
// chain read. The Algorand fields are refused: a leftover escrow_app_id or
// algod.network would otherwise read as the network the proxy pays on.
func validateSolana(cfg *Config) error {
	switch {
	case cfg.Algod.Network != "" || cfg.Algod.Endpoint != "":
		return fmt.Errorf("algod.network and algod.endpoint are the Algorand rail's; remove them, or remove the solana: block to pay on Algorand")
	case cfg.Zs.EscrowAppID != 0:
		return fmt.Errorf("zs.escrow_app_id is the Algorand rail's; on Solana set solana.program, or leave it to the cluster's preset")
	}
	cfg.Solana.ResolvedProgram = solana.PublicKey{}
	if cfg.Solana.Program != "" {
		p, err := solana.ParsePublicKey(cfg.Solana.Program)
		if err != nil {
			return fmt.Errorf("solana.program: %w", err)
		}
		cfg.Solana.ResolvedProgram = p
	}
	dep, _, err := zs.ResolveNetwork(cfg.SolanaNetwork())
	if err != nil {
		return fmt.Errorf("solana: %w", err)
	}
	// The client's own check (http or https, no credentials in the URL),
	// whose errors never quote the endpoint.
	if _, err := rpc.NewClient(dep.Solana.RPCEndpoint); err != nil {
		return fmt.Errorf("solana.rpc_endpoint: %w", err)
	}
	if cfg.Zs.ProxyPayerAddr != "" {
		if _, err := solana.ParsePublicKey(cfg.Zs.ProxyPayerAddr); err != nil {
			return fmt.Errorf("zs.proxy_payer_addr is not a Solana address: %w", err)
		}
	}
	return nil
}

// SolanaNetwork is the SDK network of a Solana config.
func (c *Config) SolanaNetwork() zs.Network {
	return zs.Network{Rail: ticket.RailSolana, Solana: zs.SolanaNetwork{
		Cluster:     c.Solana.Cluster,
		RPCEndpoint: c.Solana.RPCEndpoint,
		Program:     c.Solana.ResolvedProgram,
	}}
}

// defaultAffinityTTL / defaultAffinityMax mirror the affinity plan defaults:
// long enough to cover a realistic tool-loop (minutes), big enough to
// not thrash under moderate concurrency.
const (
	defaultAffinityTTL = 10 * time.Minute
	defaultAffinityMax = 1024
)

func validateAffinity(cfg *Config) error {
	policy := zs.AffinityPolicy(cfg.Zs.AffinityPolicy)
	if policy == "" {
		policy = zs.AffinityPrefer
	}
	if !zs.ValidAffinityPolicy(policy) {
		return fmt.Errorf("zs.affinity_policy must be one of prefer|strict|none, got %q", cfg.Zs.AffinityPolicy)
	}
	cfg.Zs.ResolvedAffinity = policy
	if cfg.Zs.AffinityTTL == 0 {
		cfg.Zs.AffinityTTL = defaultAffinityTTL
	}
	if cfg.Zs.AffinityMax == 0 {
		cfg.Zs.AffinityMax = defaultAffinityMax
	}
	// DetailsConcurrency / LatencyStateMax are intentionally NOT defaulted here:
	// like DetailsTimeout / RefreshInterval, a zero (or negative) value is
	// resolved at the use site (enrich's `limit <= 0` fallback and
	// ChainOperatorSource.latStateMax), which also protects direct construction
	// in tests. Defaulting them here too would be redundant and drift from that
	// pattern.
	return nil
}
