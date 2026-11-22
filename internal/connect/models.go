/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// FetchModels lists the models the proxy advertises. It prefers
// /v1/zs/details (carries context_window / max_output_tokens / modalities)
// and falls back to the plain OpenAI /v1/models (id only) if details is empty
// or unavailable. baseURL is the OpenAI base ending in /v1.
//
// Both are plaintext discovery GETs — no auth, no wallet. The short timeout
// matches the CLI's other proxy probes (probeHealthz).
func FetchModels(ctx context.Context, baseURL string) ([]Model, error) {
	base := strings.TrimRight(baseURL, "/")
	if models, err := fetchDetails(ctx, base+"/zs/details"); err == nil && len(models) > 0 {
		return models, nil
	}
	return fetchModelList(ctx, base+"/models")
}

func fetchDetails(ctx context.Context, url string) ([]Model, error) {
	// The proxy's aggregate /v1/zs/details uses input_usd_per_1m /
	// output_usd_per_1m / cache_read_usd_per_1m (no "_rate_" infix, unlike the
	// node's own endpoint) and carries fee_bps at the top level. Rates are the
	// cheapest operator's advertised (pre-fee) price; we gross them up to
	// payer-net here so every integration writes payer-facing numbers.
	var resp struct {
		FeeBps uint64 `json:"fee_bps"`
		Models []struct {
			ID                string   `json:"id"`
			ContextWindow     int      `json:"context_window"`
			MaxOutputTokens   int      `json:"max_output_tokens"`
			InputModalities   []string `json:"input_modalities"`
			OutputModalities  []string `json:"output_modalities"`
			InputUSDPer1M     float64  `json:"input_usd_per_1m"`
			OutputUSDPer1M    float64  `json:"output_usd_per_1m"`
			CacheReadUSDPer1M *float64 `json:"cache_read_usd_per_1m"`
			ToolUse           *bool    `json:"tool_use"`
			Tags              []string `json:"tags"`
			Reasoning         *struct {
				Supported bool `json:"supported"`
			} `json:"reasoning"`
		} `json:"models"`
	}
	if err := getJSON(ctx, url, &resp); err != nil {
		return nil, err
	}
	// grossFactor turns an operator's advertised rate into the payer-net rate:
	// the payer pays rate × (1 + feeBps/10000) (the protocol fee rides on top of
	// amount_charged — mirrors escrow.NetFee and the client's cost display). A
	// cold/absent fee (0) leaves rates raw.
	grossFactor := 1.0 + float64(resp.FeeBps)/10000.0
	models := make([]Model, 0, len(resp.Models))
	for _, m := range resp.Models {
		if m.ID == "" {
			continue
		}
		var cacheRead *float64
		if m.CacheReadUSDPer1M != nil {
			r := grossRate(*m.CacheReadUSDPer1M, grossFactor)
			cacheRead = &r
		}
		models = append(models, Model{
			ID:                m.ID,
			ContextWindow:     m.ContextWindow,
			MaxOutputTokens:   m.MaxOutputTokens,
			InputModalities:   m.InputModalities,
			OutputModalities:  m.OutputModalities,
			InputUSDPer1M:     grossRate(m.InputUSDPer1M, grossFactor),
			OutputUSDPer1M:    grossRate(m.OutputUSDPer1M, grossFactor),
			CacheReadUSDPer1M: cacheRead,
			ToolUse:           m.ToolUse,
			Reasoning:         m.Reasoning != nil && m.Reasoning.Supported,
			Tags:              m.Tags,
		})
	}
	return models, nil
}

// grossRate applies the protocol-fee gross-up factor to an advertised rate and
// rounds to 6 decimals (microUSDC-per-1M granularity) so generated configs carry
// clean numbers instead of float artifacts like 0.15300000000000002.
func grossRate(usdPer1M, grossFactor float64) float64 {
	return math.Round(usdPer1M*grossFactor*1e6) / 1e6
}

func fetchModelList(ctx context.Context, url string) ([]Model, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, url, &resp); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID != "" {
			models = append(models, Model{ID: m.ID})
		}
	}
	return models, nil
}

func getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, res.Body)
		return fmt.Errorf("GET %s: status %d", url, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(dst)
}

// supportsText reports whether a modality list permits text. An empty list is
// "no signal", which the node treats as text-only — so empty counts as text.
func supportsText(modalities []string) bool {
	if len(modalities) == 0 {
		return true
	}
	for _, m := range modalities {
		if m == "text" {
			return true
		}
	}
	return false
}

// derivedOutputCap bounds the fallback for a model's output-token budget when
// the node advertised a context window but no max_output_tokens. Every tool
// either requires an output budget alongside the context length (opencode) or
// substitutes a fixed default that's wrong in both directions (openclaw/pi
// assume 8192 — a cap on a 262k-context model, a lie on a tiny one), so we
// derive one: min(cap, context/4). The proxy meters and bills on actual usage
// regardless, so this only affects the tool's own client-side reservation.
const derivedOutputCap = 60000

// maxOutputTokens is the output-token budget to write for m: the advertised
// value, else the context-scaled fallback, else 0 when the context window is
// unknown too (nothing to derive from — the caller omits the field).
func maxOutputTokens(m Model) int {
	if m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	if m.ContextWindow > 0 {
		return min(derivedOutputCap, m.ContextWindow/4)
	}
	return 0
}

// knownModalities is the modality vocabulary the tool configs accept. The node
// advertises "text"/"image"/"audio"/"video" (plus "pdf" reserved); anything
// outside this set is dropped rather than written, so an unrecognized future
// value can't make a generated config fail its own schema validation.
var knownModalities = []string{"text", "image", "audio", "video", "pdf"}

// normalizeModalities filters an advertised modality list down to the known
// vocabulary, preserving order and dropping duplicates. An empty or
// fully-unrecognized list yields ["text"] — the node treats "no signal" as
// text-only (see supportsText), so that's the honest default rather than
// "supports nothing".
func normalizeModalities(mods []string) []string {
	out := make([]string, 0, len(mods))
	for _, m := range mods {
		lm := strings.ToLower(strings.TrimSpace(m))
		known := false
		for _, k := range knownModalities {
			if lm == k {
				known = true
				break
			}
		}
		if !known {
			continue
		}
		dup := false
		for _, have := range out {
			if have == lm {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, lm)
		}
	}
	if len(out) == 0 {
		return []string{"text"}
	}
	return out
}

// hasNonTextModality reports whether a normalized modality list carries
// anything beyond text — the signal tools use to enable file/image attachment.
func hasNonTextModality(mods []string) bool {
	for _, m := range mods {
		if m != "text" {
			return true
		}
	}
	return false
}

// ChatModels keeps only models usable by text chat / coding tools: text input
// AND at least text output. Models with no modality signal are treated as
// text-only and kept; image-only-input or image-only-output models (e.g.
// dedicated image generators) are dropped. Vision models (text+image input,
// text output) are kept.
func ChatModels(models []Model) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if supportsText(m.InputModalities) && supportsText(m.OutputModalities) {
			out = append(out, m)
		}
	}
	return out
}

// DefaultModel picks a sensible default from models: the first whose id looks
// coding-oriented (contains "coder"/"code"), else the first model. Returns ""
// when models is empty.
func DefaultModel(models []Model) string {
	if len(models) == 0 {
		return ""
	}
	for _, m := range models {
		lid := strings.ToLower(m.ID)
		if strings.Contains(lid, "coder") || strings.Contains(lid, "code") {
			return m.ID
		}
	}
	return models[0].ID
}

// findModel returns the advertised entry for id. An empty id matches nothing —
// it's the "no model selected" signal, not a lookup.
func findModel(models []Model, id string) (Model, bool) {
	if id == "" {
		return Model{}, false
	}
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// HasModel reports whether id is present in models.
func HasModel(models []Model, id string) bool {
	_, ok := findModel(models, id)
	return ok
}

// WithTags returns the models carrying every one of tags. Matching ignores case
// because operators spell tags freely ("NSFW", "nsfw") and the node merges its
// config tags with the HuggingFace card's verbatim. No tags returns models as is.
func WithTags(models []Model, tags []string) []Model {
	if len(tags) == 0 {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if hasAllTags(m.Tags, tags) {
			out = append(out, m)
		}
	}
	return out
}

func hasAllTags(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

// SortModels orders models by id, case-insensitively, in place.
//
// The registry's own order is operator-iteration order — stable within a
// snapshot but arbitrary and liable to shift between runs — so the interactive
// picker renumbered itself under the user and generated configs churned for no
// reason. Sorting here fixes both at once: the menu, the number the user types
// (which indexes this same slice), and every catalog an integration writes all
// come from one ordering.
func SortModels(models []Model) {
	sort.Slice(models, func(i, j int) bool {
		li, lj := strings.ToLower(models[i].ID), strings.ToLower(models[j].ID)
		if li != lj {
			return li < lj
		}
		// Ids differing only in case still need a total order, or sort.Slice's
		// result depends on the input permutation.
		return models[i].ID < models[j].ID
	})
}

// hasAdvertisedMetadata reports whether the proxy published any capability data
// for m, as opposed to merely naming it.
//
// Two distinct states arrive here with every field but ID zeroed, and neither
// carries information about the model:
//
//   - The /v1/models fallback (fetchModelList) — taken whenever /v1/zs/details
//     errors or comes back empty, and it can only ever populate the id.
//   - A details entry whose node capability probe hasn't landed. The proxy's
//     aggregate walks the on-chain model list and reads probed capacities for
//     each one, so a model that is registered but not yet probed is emitted with
//     its sizing fields absent (they're all omitempty).
//
// Integrations that only ever *add* keys can ignore the distinction. One that
// removes a key on "the proxy publishes no value" must not, or a transient
// probe gap reads as a deliberate retraction.
func hasAdvertisedMetadata(m Model) bool {
	return m.ContextWindow > 0 || m.MaxOutputTokens > 0 ||
		len(m.InputModalities) > 0 || len(m.OutputModalities) > 0 ||
		m.InputUSDPer1M > 0 || m.OutputUSDPer1M > 0 ||
		m.CacheReadUSDPer1M != nil || m.ToolUse != nil || m.Reasoning
}
