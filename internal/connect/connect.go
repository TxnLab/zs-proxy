/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package connect autoconfigures third-party AI tools (opencode, openclaw, pi,
// hermes, aider, continue, codex, …) to talk to the running zs-proxy.
//
// The proxy already serves a drop-in OpenAI-compatible API on localhost; the
// only friction is the last mile of pointing each tool's config at it. Each
// Integration knows one tool's config file location + schema and merges a
// single provider named "zs-proxy" into it (preserving the user's existing
// settings), pointing at the proxy's base URL with a placeholder API key (the
// proxy discards Authorization — admission is the on-chain seal, not a key).
//
// The cobra command lives in internal/cli/connect.go; this package is the
// importable, unit-testable core.
package connect

import "sort"

const (
	// ProviderID is the provider key written into every tool's config. It uses
	// the ZeroSignal brand (matching the env/keychain/datadir naming), not the
	// zs-proxy CLI binary name.
	ProviderID = "zerosignal"
	// ProviderDisplay is the human-facing provider name where a tool shows one.
	ProviderDisplay = "ZeroSignal"
	// PlaceholderAPIKey satisfies tools that require a non-empty key. The proxy
	// ignores it — there is no upstream API key at the proxy layer.
	PlaceholderAPIKey = "zerosignal-local"
)

// Endpoint is the live proxy the tools are pointed at.
type Endpoint struct {
	BaseURL string // OpenAI base, ending in /v1 (e.g. http://127.0.0.1:9376/v1)
	APIKey  string // placeholder; defaults to PlaceholderAPIKey when empty
	Network string // testnet|mainnet|localnet — display only
}

// Key returns the API key to write, defaulting to the placeholder.
func (e Endpoint) Key() string {
	if e.APIKey != "" {
		return e.APIKey
	}
	return PlaceholderAPIKey
}

// Model is one model advertised by the proxy (from /v1/zs/details, falling
// back to /v1/models).
//
// /v1/models now carries the same rates and capability fields in an
// OpenRouter-shaped entry, so the fallback is no longer id-only — but
// fetchModelList still decodes `id` alone, deliberately. Its rates are ALREADY
// payer-net (the proxy grosses them at that handler), so reading them here
// would apply the fee twice; taking only the id keeps one gross-up site.
//
// The *USDPer1M rate fields are already grossed up to payer-net (advertised
// operator rate × (1 + feeBps/10000)) — the gross-up happens once, at fetch
// (fetchDetails), so integrations write them verbatim into a tool's cost model.
// They are 0/nil when the proxy advertised no rate (or on the /v1/models
// fallback), which integrations treat as "unpriced" (no cost block).
type Model struct {
	ID               string
	ContextWindow    int
	MaxOutputTokens  int
	InputModalities  []string
	OutputModalities []string
	// InputUSDPer1M / OutputUSDPer1M are payer-net USD-per-1M-token rates; 0
	// means unpriced.
	InputUSDPer1M  float64
	OutputUSDPer1M float64
	// CacheReadUSDPer1M is the payer-net cache-read rate. nil = no advertised
	// discount (cached reads bill at the input rate); a pointed-to 0 = free
	// cached reads (the maximum discount, kept distinct from nil).
	CacheReadUSDPer1M *float64
	// ToolUse is the advertised function-calling capability: nil = no operator
	// declared one (tools default to on — most OpenAI-compatible chat models
	// tool-call), a pointed-to false = an explicit "does not tool-call".
	ToolUse *bool
	// Reasoning reports whether any operator advertises a reasoning/thinking
	// pass for the model. False also covers "unadvertised" — integrations only
	// use it to turn a reasoning display ON, never to suppress one.
	Reasoning bool
	// Tags is the union of every operator's labels for the model ("code",
	// "nsfw", …), as the operators spelled them.
	Tags []string
}

// ApplyOptions tunes a single Apply call.
type ApplyOptions struct {
	// Print renders the would-be config to Result.Rendered and writes nothing.
	Print bool
	// DefaultModel, when set, is written as the tool's default/active model.
	// Resolved by the caller (flag or interactive pick); empty means leave the
	// tool's default untouched.
	DefaultModel string
	// ConfigPathOverride forces the target file (the --config-path escape hatch
	// for non-standard install locations). Empty uses the integration default.
	ConfigPathOverride string
	// InstallPlugin asks the integration to also install the tool's own
	// ZeroSignal plugin once the config is written. Only hermes has one; the
	// CLI refuses the flag for every other tool, so the rest never see it set.
	InstallPlugin bool
}

// Written is one additional file an Apply touched beyond Result.Path.
type Written struct {
	Path     string
	Rendered string
}

// Result reports what an Apply did (or would do under Print).
type Result struct {
	Path     string // target file ("" for stdout-only integrations like generic)
	Wrote    bool   // false under Print or for stdout-only output
	Rendered string // the merged file content (Print) or the snippet to show
	Hint     string // post-setup instruction surfaced to the user
	// Extra lists files touched beyond Path. Most tools keep everything in one
	// file and leave this empty; pi splits its provider catalog (models.json)
	// from its default-model setting (settings.json), aider takes a LiteLLM
	// model-metadata sidecar alongside its conf, and hermes pairs its provider
	// block with the env vars its plugin reads (.env). Wrote/Print apply to
	// these identically — under Print they are rendered, not written.
	Extra []Written
}

// Integration configures one tool.
type Integration interface {
	// ID is the stable command-line identifier (e.g. "opencode").
	ID() string
	// DisplayName is the human-facing tool name.
	DisplayName() string
	// Detect reports the config path this integration would write and whether
	// the tool appears installed (binary on PATH or an existing config file/dir).
	Detect() (configPath string, installed bool)
	// Apply merges the zs-proxy provider into the tool's config. models may be
	// empty (the proxy advertised none, or wasn't asked); integrations that need
	// an explicit model list degrade to a provider-only entry.
	Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error)
}

// registry is the ordered set of known integrations. Append here to add a tool.
func registry() []Integration {
	return []Integration{
		opencode{},
		openclaw{},
		pi{},
		hermes{},
		aider{},
		continueDev{},
		codex{},
		generic{},
	}
}

// All returns every known integration in registration order.
func All() []Integration { return registry() }

// Lookup finds an integration by ID.
func Lookup(id string) (Integration, bool) {
	for _, in := range registry() {
		if in.ID() == id {
			return in, true
		}
	}
	return nil, false
}

// IDs returns the sorted integration identifiers (for help text / validation).
func IDs() []string {
	ids := make([]string, 0)
	for _, in := range registry() {
		ids = append(ids, in.ID())
	}
	sort.Strings(ids)
	return ids
}
