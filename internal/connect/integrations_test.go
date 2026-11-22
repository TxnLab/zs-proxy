/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	yaml "gopkg.in/yaml.v3"
)

func testEndpoint() Endpoint {
	return Endpoint{BaseURL: "http://127.0.0.1:8080/v1", APIKey: PlaceholderAPIKey, Network: "testnet"}
}

func testModels() []Model {
	return []Model{
		{ID: "qwen3-coder", ContextWindow: 262144, MaxOutputTokens: 65536, InputModalities: []string{"text"}},
		{ID: "gemini-img", ContextWindow: 32768, InputModalities: []string{"text", "image"}},
	}
}

// fileIntegrations are the registry entries that write a config file (excludes
// generic, which only prints). Each test writes into a temp file via the
// ConfigPathOverride escape hatch.
func fileIntegrations() map[string]struct {
	in       Integration
	filename string
	// markers that must appear in the written config
	wantSubstrings []string
} {
	return map[string]struct {
		in             Integration
		filename       string
		wantSubstrings []string
	}{
		"opencode": {opencode{}, "opencode.jsonc", []string{ProviderID, "@ai-sdk/openai-compatible", "qwen3-coder", `"model"`}},
		"openclaw": {openclaw{}, "openclaw.json", []string{ProviderID, "openai-completions", "primary", "qwen3-coder"}},
		"pi":       {pi{}, "models.json", []string{ProviderID, "openai-completions", `"providers"`, "qwen3-coder"}},
		"hermes":   {hermes{}, "config.yaml", []string{ProviderID, "base_url", "transport: chat_completions", "qwen3-coder"}},
		"aider":    {aider{}, ".aider.conf.yml", []string{"openai-api-base", "openai/qwen3-coder", "model-metadata-file"}},
		"continue": {continueDev{}, "config.yaml", []string{continueNamePrefix + "qwen3-coder", "apiBase", "image_input"}},
		"codex":    {codex{}, "config.toml", []string{ProviderID, "responses", "base_url", "qwen3-coder"}},
	}
}

func TestIntegrations_ApplyWritesAndIsIdempotent(t *testing.T) {
	ep, models := testEndpoint(), testModels()
	for name, tc := range fileIntegrations() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.filename)
			opts := ApplyOptions{DefaultModel: "qwen3-coder", ConfigPathOverride: path}

			res, err := tc.in.Apply(ep, models, opts)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !res.Wrote || res.Path != path {
				t.Fatalf("Result = %+v, want Wrote=true Path=%s", res, path)
			}
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for _, sub := range tc.wantSubstrings {
				if !contains(first, sub) {
					t.Errorf("written config missing %q:\n%s", sub, first)
				}
			}

			// Re-apply: must be byte-stable (idempotent) and back up the prior file.
			if _, err := tc.in.Apply(ep, models, opts); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			second, _ := os.ReadFile(path)
			if string(first) != string(second) {
				t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
			}
			if _, err := os.Stat(path + ".bak"); err != nil {
				t.Errorf("no backup after re-apply: %v", err)
			}
		})
	}
}

func TestIntegrations_PrintWritesNothing(t *testing.T) {
	ep, models := testEndpoint(), testModels()
	for name, tc := range fileIntegrations() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.filename)
			res, err := tc.in.Apply(ep, models, ApplyOptions{Print: true, DefaultModel: "qwen3-coder", ConfigPathOverride: path})
			if err != nil {
				t.Fatalf("Apply(print): %v", err)
			}
			if res.Wrote {
				t.Error("Print mode reported Wrote=true")
			}
			if res.Rendered == "" {
				t.Error("Print mode produced no Rendered output")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("Print mode created a file")
			}
		})
	}
}

// TestOpencode_PreservesExistingProvider proves a hand-configured provider and a
// comment survive the merge.
func TestOpencode_PreservesExistingProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	seed := []byte("{\n  // keep me\n  \"provider\": { \"mine\": { \"name\": \"Mine\" } },\n  \"theme\": \"dark\"\n}\n")
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (opencode{}).Apply(testEndpoint(), testModels(), ApplyOptions{DefaultModel: "qwen3-coder", ConfigPathOverride: path}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, _ := os.ReadFile(path)
	for _, sub := range []string{"keep me", `"mine"`, `"theme"`, ProviderID} {
		if !contains(out, sub) {
			t.Errorf("missing %q after merge:\n%s", sub, out)
		}
	}
}

// TestOpencode_LimitRequiresContextAndOutput proves every emitted model limit
// carries both context and output (opencode rejects a partial limit), and that
// a model without an advertised max_output_tokens falls back to the default
// output rather than emitting context alone.
func TestOpencode_LimitRequiresContextAndOutput(t *testing.T) {
	res, err := (opencode{}).Apply(testEndpoint(), testModels(), ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	std, err := hujson.Standardize([]byte(res.Rendered))
	if err != nil {
		t.Fatalf("standardize JSONC: %v\n%s", err, res.Rendered)
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Limit *struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(std, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, std)
	}
	models := cfg.Provider[ProviderID].Models
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2:\n%s", len(models), std)
	}
	for id, m := range models {
		if m.Limit == nil {
			t.Errorf("%s: missing limit (both test models set a context window)", id)
			continue
		}
		if m.Limit.Context <= 0 || m.Limit.Output <= 0 {
			t.Errorf("%s: limit must have both context and output, got %+v", id, *m.Limit)
		}
	}
	// gemini-img advertises no max_output_tokens → context-scaled fallback:
	// min(derivedOutputCap, 32768/4) = min(60000, 8192) = 8192 (the
	// context/4 term is under the cap, so it wins).
	if got := models["gemini-img"].Limit.Output; got != 8192 {
		t.Errorf("gemini-img output = %d, want context-scaled fallback 8192", got)
	}
	// qwen3-coder advertises one → preserved verbatim.
	if got := models["qwen3-coder"].Limit.Output; got != 65536 {
		t.Errorf("qwen3-coder output = %d, want 65536 (advertised value preserved)", got)
	}

	// A small-context model with no advertised output scales below the cap:
	// context 16000 → 16000/4 = 4000.
	small := []Model{{ID: "tiny", ContextWindow: 16000}}
	res2, err := (opencode{}).Apply(testEndpoint(), small, ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply small: %v", err)
	}
	std2, err := hujson.Standardize([]byte(res2.Rendered))
	if err != nil {
		t.Fatalf("standardize small: %v", err)
	}
	if err := json.Unmarshal(std2, &cfg); err != nil {
		t.Fatalf("unmarshal small: %v", err)
	}
	if got := cfg.Provider[ProviderID].Models["tiny"].Limit.Output; got != 4000 {
		t.Errorf("tiny output = %d, want 4000 (context/4, below cap)", got)
	}
}

func fptr(f float64) *float64 { return &f }

// pricedModels carries payer-net rates (as fetchDetails would produce them) to
// exercise the cost blocks: one fully-priced model with a cache-read discount,
// one priced without a discount (cache_read defaults to the input rate), and one
// unpriced model (no cost block).
func pricedModels() []Model {
	return []Model{
		{ID: "priced", ContextWindow: 100000, MaxOutputTokens: 4096,
			InputUSDPer1M: 0.153, OutputUSDPer1M: 0.612, CacheReadUSDPer1M: fptr(0.0153)},
		{ID: "no-discount", ContextWindow: 100000,
			InputUSDPer1M: 1.0, OutputUSDPer1M: 2.0},
		{ID: "unpriced", ContextWindow: 100000},
	}
}

// TestOpencode_CostFromRates proves opencode gets a snake_case cost block from
// the payer-net rates, cache_read defaults to the input rate without a discount,
// cache_write mirrors the input rate, and an unpriced model gets no cost block.
func TestOpencode_CostFromRates(t *testing.T) {
	res, err := (opencode{}).Apply(testEndpoint(), pricedModels(), ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	std, err := hujson.Standardize([]byte(res.Rendered))
	if err != nil {
		t.Fatalf("standardize: %v\n%s", err, res.Rendered)
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Cost *struct {
					Input      float64 `json:"input"`
					Output     float64 `json:"output"`
					CacheRead  float64 `json:"cache_read"`
					CacheWrite float64 `json:"cache_write"`
				} `json:"cost"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(std, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, std)
	}
	models := cfg.Provider[ProviderID].Models

	priced := models["priced"].Cost
	if priced == nil {
		t.Fatalf("priced: missing cost block")
	}
	if priced.Input != 0.153 || priced.Output != 0.612 || priced.CacheRead != 0.0153 || priced.CacheWrite != 0.153 {
		t.Errorf("priced cost = %+v, want {0.153 0.612 0.0153 0.153}", *priced)
	}

	nd := models["no-discount"].Cost
	if nd == nil {
		t.Fatalf("no-discount: missing cost block")
	}
	// No advertised discount → cache_read and cache_write both fall to input.
	if nd.Input != 1.0 || nd.CacheRead != 1.0 || nd.CacheWrite != 1.0 {
		t.Errorf("no-discount cost = %+v, want cache_read/write == input 1.0", *nd)
	}

	if models["unpriced"].Cost != nil {
		t.Errorf("unpriced: cost block present, want none: %+v", *models["unpriced"].Cost)
	}
}

// TestOpencode_Capabilities proves each model carries an explicit capability
// block — opencode defaults attachment/reasoning/temperature to false and
// modalities to text-only for a config-declared model, so omitting them strips
// vision and thinking from a model that has them.
func TestOpencode_Capabilities(t *testing.T) {
	no := false
	models := []Model{
		// Vision + reasoning, tool use unadvertised.
		{ID: "vision", ContextWindow: 128000, InputModalities: []string{"text", "image"},
			OutputModalities: []string{"text"}, Reasoning: true},
		// Text-only, explicitly no tool calling, and an unknown modality that
		// must not reach the config.
		{ID: "plain", ContextWindow: 8192, InputModalities: []string{"text", "telepathy"}, ToolUse: &no},
		// No modality signal at all → text-only, no attachment.
		{ID: "silent", ContextWindow: 8192},
	}
	res, err := (opencode{}).Apply(testEndpoint(), models, ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	std, err := hujson.Standardize([]byte(res.Rendered))
	if err != nil {
		t.Fatalf("standardize: %v\n%s", err, res.Rendered)
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Attachment  bool `json:"attachment"`
				Reasoning   bool `json:"reasoning"`
				Temperature bool `json:"temperature"`
				ToolCall    bool `json:"tool_call"`
				Modalities  struct {
					Input  []string `json:"input"`
					Output []string `json:"output"`
				} `json:"modalities"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(std, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, std)
	}
	got := cfg.Provider[ProviderID].Models

	vision := got["vision"]
	if !vision.Attachment {
		t.Error("vision: attachment=false, want true (image input)")
	}
	if !vision.Reasoning || !vision.Temperature || !vision.ToolCall {
		t.Errorf("vision: %+v, want reasoning/temperature/tool_call all true", vision)
	}
	if strings.Join(vision.Modalities.Input, ",") != "text,image" {
		t.Errorf("vision input modalities = %v, want [text image]", vision.Modalities.Input)
	}
	if strings.Join(vision.Modalities.Output, ",") != "text" {
		t.Errorf("vision output modalities = %v, want [text]", vision.Modalities.Output)
	}

	plain := got["plain"]
	if plain.Attachment || plain.Reasoning || plain.ToolCall {
		t.Errorf("plain: %+v, want attachment/reasoning/tool_call all false", plain)
	}
	if strings.Join(plain.Modalities.Input, ",") != "text" {
		t.Errorf("plain input modalities = %v, want the unknown value dropped", plain.Modalities.Input)
	}

	silent := got["silent"]
	if silent.Attachment {
		t.Error("silent: attachment=true, want false (no modality signal)")
	}
	// Unadvertised tool_use → true, matching opencode's own default.
	if !silent.ToolCall {
		t.Error("silent: tool_call=false, want true (unadvertised defaults on)")
	}
	if strings.Join(silent.Modalities.Input, ",") != "text" || strings.Join(silent.Modalities.Output, ",") != "text" {
		t.Errorf("silent modalities = %+v, want text in/out", silent.Modalities)
	}
}

// capabilityModels exercises the per-model capability surface: a vision +
// reasoning model, a text-only model that explicitly does NOT tool-call, and a
// model the proxy advertised nothing about beyond its id.
func capabilityModels() []Model {
	no := false
	return []Model{
		{ID: "vision", ContextWindow: 262144, InputModalities: []string{"text", "image"},
			OutputModalities: []string{"text"}, Reasoning: true},
		{ID: "plain", ContextWindow: 16000, MaxOutputTokens: 4096, ToolUse: &no},
		{ID: "silent"},
	}
}

// TestOpenclawPi_Capabilities proves both stacks (they share a model schema) get
// the modality list, the reasoning bool, and a derived maxTokens — each of which
// they otherwise default to the pessimistic value (input ["text"], reasoning
// false, maxTokens a flat 8192).
func TestOpenclawPi_Capabilities(t *testing.T) {
	type modelEntry struct {
		ID            string   `json:"id"`
		Input         []string `json:"input"`
		Reasoning     bool     `json:"reasoning"`
		ContextWindow int      `json:"contextWindow"`
		MaxTokens     int      `json:"maxTokens"`
	}
	for _, tc := range []struct {
		name    string
		in      Integration
		extract func([]byte) []modelEntry
	}{
		{"openclaw", openclaw{}, func(b []byte) []modelEntry {
			var cfg struct {
				Models struct {
					Providers map[string]struct {
						Models []modelEntry `json:"models"`
					} `json:"providers"`
				} `json:"models"`
			}
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, b)
			}
			return cfg.Models.Providers[ProviderID].Models
		}},
		{"pi", pi{}, func(b []byte) []modelEntry {
			var cfg struct {
				Providers map[string]struct {
					Models []modelEntry `json:"models"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, b)
			}
			return cfg.Providers[ProviderID].Models
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.in.Apply(testEndpoint(), capabilityModels(), ApplyOptions{Print: true})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			std, err := hujson.Standardize([]byte(res.Rendered))
			if err != nil {
				t.Fatalf("standardize: %v\n%s", err, res.Rendered)
			}
			byID := map[string]modelEntry{}
			for _, m := range tc.extract(std) {
				byID[m.ID] = m
			}

			vision := byID["vision"]
			if strings.Join(vision.Input, ",") != "text,image" {
				t.Errorf("vision input = %v, want [text image]", vision.Input)
			}
			if !vision.Reasoning {
				t.Error("vision reasoning = false, want true")
			}
			// No advertised max output → min(derivedOutputCap, 262144/4) = 60000.
			if vision.MaxTokens != 60000 {
				t.Errorf("vision maxTokens = %d, want the derived 60000", vision.MaxTokens)
			}

			plain := byID["plain"]
			if plain.Reasoning {
				t.Error("plain reasoning = true, want false")
			}
			if plain.MaxTokens != 4096 {
				t.Errorf("plain maxTokens = %d, want the advertised 4096", plain.MaxTokens)
			}

			// No context window to scale from → maxTokens omitted rather than
			// invented (both schemas require it to be > 0 when present).
			if silent := byID["silent"]; silent.MaxTokens != 0 || silent.ContextWindow != 0 {
				t.Errorf("silent = %+v, want no sizing fields", silent)
			}
		})
	}
}

// TestContinue_Capabilities proves tool_use is driven by the advertised signal
// (not hardcoded) and that sizing lands under defaultCompletionOptions.
func TestContinue_Capabilities(t *testing.T) {
	res, err := (continueDev{}).Apply(testEndpoint(), capabilityModels(), ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var cfg struct {
		Models []struct {
			Name         string   `yaml:"name"`
			Capabilities []string `yaml:"capabilities"`
			Completion   struct {
				ContextLength int `yaml:"contextLength"`
				MaxTokens     int `yaml:"maxTokens"`
			} `yaml:"defaultCompletionOptions"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(res.Rendered), &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Rendered)
	}
	byName := map[string]struct {
		caps []string
		ctx  int
		out  int
	}{}
	for _, m := range cfg.Models {
		byName[m.Name] = struct {
			caps []string
			ctx  int
			out  int
		}{m.Capabilities, m.Completion.ContextLength, m.Completion.MaxTokens}
	}

	vision := byName[continueNamePrefix+"vision"]
	if strings.Join(vision.caps, ",") != "tool_use,image_input" {
		t.Errorf("vision capabilities = %v, want [tool_use image_input]", vision.caps)
	}
	if vision.ctx != 262144 || vision.out != 60000 {
		t.Errorf("vision sizing = ctx %d / out %d, want 262144 / 60000", vision.ctx, vision.out)
	}
	// Explicit tool_use:false drops the capability (and with no image input,
	// the whole list) rather than claiming Agent-mode support.
	if plain := byName[continueNamePrefix+"plain"]; len(plain.caps) != 0 {
		t.Errorf("plain capabilities = %v, want none", plain.caps)
	}
	// Unadvertised tool_use stays on.
	if silent := byName[continueNamePrefix+"silent"]; strings.Join(silent.caps, ",") != "tool_use" {
		t.Errorf("silent capabilities = %v, want [tool_use]", silent.caps)
	}
}

// TestAider_ModelMetadataSidecar proves aider gets a LiteLLM model_info sidecar
// keyed by "openai/<id>" (without it aider warns "Unknown context window size
// and costs" and can't tell a vision model from a text one), that it points
// model-metadata-file at it, and that a user's own entries survive the upsert.
func TestAider_ModelMetadataSidecar(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, ".aider.conf.yml")
	meta := filepath.Join(dir, ".aider.model.metadata.json")
	// A hand-written entry for someone else's model must survive.
	if err := os.WriteFile(meta, []byte(`{"ollama/mine": {"max_input_tokens": 4096}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	models := append(capabilityModels(), Model{ID: "priced", ContextWindow: 100000,
		InputUSDPer1M: 0.153, OutputUSDPer1M: 0.612, CacheReadUSDPer1M: fptr(0.0153)})
	res, err := (aider{}).Apply(testEndpoint(), models, ApplyOptions{DefaultModel: "vision", ConfigPathOverride: conf})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Extra) != 1 || res.Extra[0].Path != meta {
		t.Fatalf("Extra = %+v, want the metadata sidecar at %s", res.Extra, meta)
	}

	confBody, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	var confCfg struct {
		MetadataFile string `yaml:"model-metadata-file"`
	}
	if err := yaml.Unmarshal(confBody, &confCfg); err != nil {
		t.Fatalf("unmarshal conf: %v", err)
	}
	if confCfg.MetadataFile != meta {
		t.Errorf("model-metadata-file = %q, want %q", confCfg.MetadataFile, meta)
	}

	metaBody, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]struct {
		Provider     string  `json:"litellm_provider"`
		Mode         string  `json:"mode"`
		MaxInput     int     `json:"max_input_tokens"`
		MaxOutput    int     `json:"max_output_tokens"`
		MaxTokens    int     `json:"max_tokens"`
		Vision       bool    `json:"supports_vision"`
		FunctionCall bool    `json:"supports_function_calling"`
		InputCost    float64 `json:"input_cost_per_token"`
		OutputCost   float64 `json:"output_cost_per_token"`
		CacheCost    float64 `json:"cache_read_input_token_cost"`
	}
	if err := json.Unmarshal(metaBody, &info); err != nil {
		t.Fatalf("unmarshal metadata: %v\n%s", err, metaBody)
	}
	if _, ok := info["ollama/mine"]; !ok {
		t.Errorf("upsert dropped the user's own entry:\n%s", metaBody)
	}

	vision := info["openai/vision"]
	if !vision.Vision || !vision.FunctionCall {
		t.Errorf("vision = %+v, want supports_vision + supports_function_calling", vision)
	}
	if vision.Provider != "openai" || vision.Mode != "chat" {
		t.Errorf("vision = %+v, want litellm_provider openai / mode chat", vision)
	}
	if vision.MaxInput != 262144 || vision.MaxOutput != 60000 || vision.MaxTokens != 60000 {
		t.Errorf("vision sizing = %+v, want 262144 in / 60000 out (both output keys)", vision)
	}
	if plain := info["openai/plain"]; plain.Vision || plain.FunctionCall {
		t.Errorf("plain = %+v, want neither vision nor function calling", plain)
	}
	// Rates convert from USD/1M to LiteLLM's per-token cost.
	priced := info["openai/priced"]
	if priced.InputCost != 0.153/1e6 || priced.OutputCost != 0.612/1e6 || priced.CacheCost != 0.0153/1e6 {
		t.Errorf("priced costs = %+v, want per-token 1.53e-7 / 6.12e-7 / 1.53e-8", priced)
	}
	// An unpriced model gets no cost keys at all (a present 0 would read as free).
	if silent := info["openai/silent"]; silent.InputCost != 0 || silent.OutputCost != 0 {
		t.Errorf("silent costs = %+v, want none", silent)
	}
}

// TestOpenclaw_CostFromRates proves OpenClaw gets a camelCase cost block from the
// payer-net rates, and an unpriced model keeps a zeroed block (OpenClaw's schema
// emits cost unconditionally).
func TestOpenclaw_CostFromRates(t *testing.T) {
	res, err := (openclaw{}).Apply(testEndpoint(), pricedModels(), ApplyOptions{Print: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	std, err := hujson.Standardize([]byte(res.Rendered))
	if err != nil {
		t.Fatalf("standardize: %v\n%s", err, res.Rendered)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				Models []struct {
					ID   string `json:"id"`
					Cost struct {
						Input      float64 `json:"input"`
						Output     float64 `json:"output"`
						CacheRead  float64 `json:"cacheRead"`
						CacheWrite float64 `json:"cacheWrite"`
					} `json:"cost"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
	}
	if err := json.Unmarshal(std, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, std)
	}
	byID := map[string]struct {
		Input, Output, CacheRead, CacheWrite float64
	}{}
	for _, m := range cfg.Models.Providers[ProviderID].Models {
		byID[m.ID] = struct{ Input, Output, CacheRead, CacheWrite float64 }{m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite}
	}

	if p := byID["priced"]; p.Input != 0.153 || p.Output != 0.612 || p.CacheRead != 0.0153 || p.CacheWrite != 0.153 {
		t.Errorf("priced cost = %+v, want {0.153 0.612 0.0153 0.153}", p)
	}
	if u := byID["unpriced"]; u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheWrite != 0 {
		t.Errorf("unpriced cost = %+v, want all zero", u)
	}
}

// TestContinue_DedupsOnReapply proves re-running replaces the zs-proxy entries
// rather than appending duplicates, while keeping the user's own model entry.
func TestContinue_DedupsOnReapply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := []byte("name: Mine\nversion: 0.0.1\nschema: v1\nmodels:\n  - name: my-local\n    provider: openai\n    model: foo\n")
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := ApplyOptions{DefaultModel: "qwen3-coder", ConfigPathOverride: path}
	if _, err := (continueDev{}).Apply(testEndpoint(), testModels(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := (continueDev{}).Apply(testEndpoint(), testModels(), opts); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	out, _ := os.ReadFile(path)
	if !contains(out, "my-local") {
		t.Errorf("user's own model entry was dropped:\n%s", out)
	}
	// Exactly two zs-proxy entries (one per advertised model), not four.
	if n := countSub(string(out), "name: "+continueNamePrefix); n != 2 {
		t.Errorf("got %d zs-proxy entries, want 2 (no duplication):\n%s", n, out)
	}
}

// piSettings parses pi's settings.json off disk.
func piSettings(t *testing.T, path string) struct {
	DefaultProvider string `json:"defaultProvider"`
	DefaultModel    string `json:"defaultModel"`
	Theme           string `json:"theme"`
} {
	t.Helper()
	var s struct {
		DefaultProvider string `json:"defaultProvider"`
		DefaultModel    string `json:"defaultModel"`
		Theme           string `json:"theme"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	// Parsed with encoding/json, not hujson: pi reads settings.json as strict
	// JSON (unlike models.json, where it strips comments first), so anything we
	// write that plain json can't parse would break pi.
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json is not strict JSON: %v\n%s", err, raw)
	}
	return s
}

// TestPi_WritesSettingsSibling proves the default model lands in settings.json
// next to models.json as a *bare* id paired with defaultProvider — pi takes
// --provider and --model separately, so the "<provider>/<model>" form opencode
// and openclaw write would not resolve.
func TestPi_WritesSettingsSibling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	settingsPath := filepath.Join(dir, "settings.json")
	// Seed an unrelated setting to prove the merge preserves it.
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (pi{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		DefaultModel: "qwen3-coder", ConfigPathOverride: path,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Extra) != 1 || res.Extra[0].Path != settingsPath {
		t.Fatalf("Result.Extra = %+v, want one entry at %s", res.Extra, settingsPath)
	}

	s := piSettings(t, settingsPath)
	if s.DefaultModel != "qwen3-coder" {
		t.Errorf("defaultModel = %q, want bare %q", s.DefaultModel, "qwen3-coder")
	}
	if strings.Contains(s.DefaultModel, "/") {
		t.Errorf("defaultModel = %q: must be a bare id, not provider/model", s.DefaultModel)
	}
	if s.DefaultProvider != ProviderID {
		t.Errorf("defaultProvider = %q, want %q", s.DefaultProvider, ProviderID)
	}
	if s.Theme != "dark" {
		t.Errorf("existing setting clobbered: theme = %q, want dark", s.Theme)
	}
	// The default model belongs in settings.json only — models.json carries the
	// catalog.
	models, _ := os.ReadFile(path)
	if contains(models, "defaultModel") {
		t.Errorf("models.json carries defaultModel, want it only in settings.json:\n%s", models)
	}
}

// TestPi_NoDefaultModelSkipsSettings proves settings.json is left untouched when
// there's no model to set (the proxy advertised none).
func TestPi_NoDefaultModelSkipsSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	res, err := (pi{}).Apply(testEndpoint(), testModels(), ApplyOptions{ConfigPathOverride: path})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Extra) != 0 {
		t.Errorf("Result.Extra = %+v, want empty without a default model", res.Extra)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json created without a default model")
	}
}

// TestPi_PrintWritesNeitherFile guards the second file too — the shared print
// test only stats the primary path.
func TestPi_PrintWritesNeitherFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	res, err := (pi{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		Print: true, DefaultModel: "qwen3-coder", ConfigPathOverride: path,
	})
	if err != nil {
		t.Fatalf("Apply(print): %v", err)
	}
	if len(res.Extra) != 1 || res.Extra[0].Rendered == "" {
		t.Fatalf("Result.Extra = %+v, want one rendered entry", res.Extra)
	}
	if !contains([]byte(res.Extra[0].Rendered), "defaultProvider") {
		t.Errorf("rendered settings missing defaultProvider:\n%s", res.Extra[0].Rendered)
	}
	for _, f := range []string{"models.json", "settings.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("Print mode created %s", f)
		}
	}
}

// TestHermes_TransportNotType pins the provider block's wire key: Hermes reads
// `transport` (its own config migration writes it) and has never read `type`,
// so a block carrying `type: openai` said nothing to it. The rest of the block
// and the model selection must be exactly what they were.
func TestHermes_TransportNotType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		Print: true, DefaultModel: "qwen3-coder", ConfigPathOverride: path,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var cfg struct {
		Providers map[string]map[string]any `yaml:"providers"`
		Model     struct {
			Provider string `yaml:"provider"`
			Default  string `yaml:"default"`
		} `yaml:"model"`
	}
	if err := yaml.Unmarshal([]byte(res.Rendered), &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Rendered)
	}
	block := cfg.Providers[ProviderID]
	if block["transport"] != "chat_completions" {
		t.Errorf("transport = %v, want chat_completions", block["transport"])
	}
	if v, ok := block["type"]; ok {
		t.Errorf("block carries type: %v, want no such key", v)
	}
	if block["base_url"] != testEndpoint().BaseURL || block["api_key"] != PlaceholderAPIKey || block["default_model"] != "qwen3-coder" {
		t.Errorf("block = %v, want base_url/api_key/default_model unchanged", block)
	}
	if cfg.Model.Provider != ProviderID || cfg.Model.Default != "qwen3-coder" {
		t.Errorf("model = %+v, want provider %s / default qwen3-coder", cfg.Model, ProviderID)
	}
}

// TestHermes_PrunesLegacyType: re-applying over a block an earlier connect
// wrote removes its `type: openai` (mergeYAML only adds, so without the prune
// the stale key and Hermes' launch warning outlive every re-run), while a
// `type` anywhere we did not write it — another provider's, or a value the user
// set on ours — survives.
func TestHermes_PrunesLegacyType(t *testing.T) {
	legacy := "providers:\n  " + ProviderID + ":\n    base_url: http://old/v1\n    api_key: x\n    type: openai\n" +
		"  other:\n    base_url: http://other/v1\n    type: openai\n"
	tests := []struct {
		name     string
		ourType  string // the type our block carries after re-apply; "" = pruned
		existing string
	}{
		{"connect-written openai pruned", "", legacy},
		{"hand-set value kept", "mine", strings.Replace(legacy, "type: openai", "type: mine", 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.existing), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{Print: true, ConfigPathOverride: path})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			var cfg struct {
				Providers map[string]map[string]any `yaml:"providers"`
			}
			if err := yaml.Unmarshal([]byte(res.Rendered), &cfg); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, res.Rendered)
			}
			ours := cfg.Providers[ProviderID]
			if got, _ := ours["type"].(string); got != tc.ourType {
				t.Errorf("our type = %q, want %q:\n%s", got, tc.ourType, res.Rendered)
			}
			if ours["transport"] != "chat_completions" {
				t.Errorf("our transport = %v, want chat_completions", ours["transport"])
			}
			if cfg.Providers["other"]["type"] != "openai" {
				t.Errorf("other provider's type = %v, want openai left alone", cfg.Providers["other"]["type"])
			}
		})
	}
}

// TestHermes_WritesEnvSibling proves the plugin's env vars land in .env beside
// config.yaml as a line splice — the stale key line rewritten where it sits,
// the missing base-URL line appended, everything else byte-for-byte, the
// file's own mode kept, no .env.bak — that a re-run is byte-stable, and that
// the block and .env carry the same endpoint and key. The two files are the
// two paths Hermes may take; if they disagree, installing the plugin silently
// moves the user to a different endpoint.
func TestHermes_WritesEnvSibling(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.yaml")
	env := filepath.Join(dir, ".env")
	seed := "# Hermes Agent Environment Configuration\nOPENROUTER_API_KEY=keep-me\n\nZEROSIGNAL_API_KEY=stale\nexport OTHER=x"
	if err := os.WriteFile(env, []byte(seed), 0o640); err != nil {
		t.Fatal(err)
	}
	opts := ApplyOptions{DefaultModel: "qwen3-coder", ConfigPathOverride: conf}

	res, err := (hermes{}).Apply(testEndpoint(), testModels(), opts)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Wrote || len(res.Extra) != 1 || res.Extra[0].Path != env {
		t.Fatalf("Result = %+v, want Wrote with Extra at %s", res, env)
	}
	first, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Hermes Agent Environment Configuration\nOPENROUTER_API_KEY=keep-me\n\n" +
		"ZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\nexport OTHER=x\n" +
		"ZEROSIGNAL_BASE_URL=" + testEndpoint().BaseURL + "\n"
	if string(first) != want {
		t.Errorf(".env after apply:\n got: %q\nwant: %q", first, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(env)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf(".env mode = %o, want the existing 0640 kept", fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(env + ".bak"); !os.IsNotExist(err) {
		t.Errorf(".env.bak exists (stat err %v), want no backup of the secrets file", err)
	}

	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Providers map[string]map[string]any `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal config.yaml: %v", err)
	}
	block := cfg.Providers[ProviderID]
	if block["base_url"] != testEndpoint().BaseURL || block["api_key"] != PlaceholderAPIKey {
		t.Errorf("block base_url/api_key = %v/%v, want the .env values %s/%s",
			block["base_url"], block["api_key"], testEndpoint().BaseURL, PlaceholderAPIKey)
	}

	if _, err := (hermes{}).Apply(testEndpoint(), testModels(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if second, _ := os.ReadFile(env); string(second) != string(first) {
		t.Errorf(".env not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestHermes_ReadOnlyEnvRewritten: a user who chmod-400s .env must still get a
// changed endpoint written, and keep 0400. The write replaces the file by
// rename, so only the directory's permissions matter; a backup copy written
// through the old path is what used to fail here on the second run.
func TestHermes_ReadOnlyEnvRewritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.yaml")
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("OPENROUTER_API_KEY=keep-me\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	opts := ApplyOptions{ConfigPathOverride: conf}
	if _, err := (hermes{}).Apply(testEndpoint(), testModels(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	moved := testEndpoint()
	moved.BaseURL = "http://127.0.0.1:9999/v1"
	if _, err := (hermes{}).Apply(moved, testModels(), opts); err != nil {
		t.Fatalf("second Apply with a changed endpoint: %v", err)
	}
	got, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	want := "OPENROUTER_API_KEY=keep-me\nZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\nZEROSIGNAL_BASE_URL=" + moved.BaseURL + "\n"
	if string(got) != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
	fi, err := os.Stat(env)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o400 {
		t.Errorf(".env mode = %o, want 0400 kept", fi.Mode().Perm())
	}
}

// TestHermes_SymlinkedEnvSurvives: a .env symlinked elsewhere is written
// through, so the link still points at the one real file Hermes and the user
// both edit, and that file keeps its mode.
func TestHermes_SymlinkedEnvSurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(real, []byte("OPENROUTER_API_KEY=keep-me\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, ".env")
	if err := os.Symlink(real, env); err != nil {
		t.Fatal(err)
	}
	if _, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{ConfigPathOverride: filepath.Join(dir, "config.yaml")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fi, err := os.Lstat(env)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal(".env symlink replaced by a regular file")
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	want := "OPENROUTER_API_KEY=keep-me\nZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\nZEROSIGNAL_BASE_URL=" + testEndpoint().BaseURL + "\n"
	if string(got) != want {
		t.Errorf("link target = %q, want %q", got, want)
	}
	rfi, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if rfi.Mode().Perm() != 0o640 {
		t.Errorf("link target mode = %o, want 0640 kept", rfi.Mode().Perm())
	}
}

// TestHermes_NewEnvIsPrivate: a .env created from nothing holds exactly the two
// lines and is 0600 — it is Hermes' secrets file even though our values are
// not secret, so it must not be born at the config file's 0644.
func TestHermes_NewEnvIsPrivate(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if _, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		DefaultModel: "qwen3-coder", ConfigPathOverride: filepath.Join(dir, "config.yaml"),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(env)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	want := "ZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\nZEROSIGNAL_BASE_URL=" + testEndpoint().BaseURL + "\n"
	if string(got) != want {
		t.Errorf("fresh .env = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(env)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("fresh .env mode = %o, want 0600", fi.Mode().Perm())
		}
	}
}

// TestHermes_PrintWritesNeitherFile guards the second file too (the shared
// print test only stats the primary path) and pins what --print shows for it:
// the real splice with every value this run does not set masked. That file
// holds every other provider's real key, and --print output gets piped and
// pasted.
func TestHermes_PrintWritesNeitherFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.yaml")
	env := filepath.Join(dir, ".env")
	seed := "OPENROUTER_API_KEY=hunter2\n"
	if err := os.WriteFile(env, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		Print: true, DefaultModel: "qwen3-coder", ConfigPathOverride: conf,
	})
	if err != nil {
		t.Fatalf("Apply(print): %v", err)
	}
	if res.Wrote {
		t.Error("Print mode reported Wrote=true")
	}
	if len(res.Extra) != 1 || res.Extra[0].Path != env {
		t.Fatalf("Result.Extra = %+v, want one rendered entry at %s", res.Extra, env)
	}
	want := "OPENROUTER_API_KEY=<redacted>\nZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\nZEROSIGNAL_BASE_URL=" + testEndpoint().BaseURL + "\n"
	if got := res.Extra[0].Rendered; got != want {
		t.Errorf("rendered .env:\n got: %q\nwant: %q", got, want)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Error("Print mode created config.yaml")
	}
	if got, _ := os.ReadFile(env); string(got) != seed {
		t.Errorf("Print mode changed .env: %q", got)
	}
}

// TestHermes_DetectHonorsHermesHome: resolvePath honors HERMES_HOME, so the
// installed-ness probe has to resolve from the same place or a relocated Hermes
// reports not-installed while connect happily writes into it.
func TestHermes_DetectHonorsHermesHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERMES_HOME", dir)
	// Neutralize the other two triggers (a real ~/.hermes, `hermes` on PATH) or
	// a developer machine answers "installed" whatever HERMES_HOME says.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)
	t.Setenv("LOCALAPPDATA", empty)
	t.Setenv("PATH", empty)

	path, installed := (hermes{}).Detect()
	if want := filepath.Join(dir, "config.yaml"); path != want {
		t.Errorf("Detect path = %q, want %q", path, want)
	}
	if !installed {
		t.Error("Detect reported not-installed for an existing HERMES_HOME directory")
	}

	// The negative half, without which the above passes if Detect hardcodes
	// installed=true.
	missing := filepath.Join(dir, "not-created")
	t.Setenv("HERMES_HOME", missing)
	path, installed = (hermes{}).Detect()
	if want := filepath.Join(missing, "config.yaml"); path != want {
		t.Errorf("Detect path = %q, want %q", path, want)
	}
	if installed {
		t.Error("Detect reported installed for a HERMES_HOME that doesn't exist")
	}
}

// stubHermes swaps runHermes for one that records its calls and runs during
// (not after) the call, so a test can inspect what is on disk at that moment.
func stubHermes(t *testing.T, during func() error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := runHermes
	runHermes = func(_ context.Context, args ...string) error {
		calls = append(calls, args)
		if during != nil {
			return during()
		}
		return nil
	}
	t.Cleanup(func() { runHermes = prev })
	return &calls
}

// TestHermes_PluginInstallRunsAfterEnv pins the ordering: both files are on
// disk before the installer starts, so a failed install leaves a Hermes that
// works through the provider block rather than one configured by neither path.
// (It also keeps a future requires_env prompt from asking for a value we just
// wrote — see Apply, where that reason is spelled out as contingent.)
// Asserting from inside the stub is the point: a check after Apply returns
// passes just as well with the install moved ahead of the writes.
func TestHermes_PluginInstallRunsAfterEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	var envAtInstall, confAtInstall []byte
	calls := stubHermes(t, func() error {
		envAtInstall, _ = os.ReadFile(filepath.Join(home, ".env"))
		confAtInstall, _ = os.ReadFile(filepath.Join(home, "config.yaml"))
		return nil
	})

	res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{InstallPlugin: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("installer ran %d times, want exactly 1: %v", len(*calls), *calls)
	}
	if got, want := strings.Join((*calls)[0], " "), "plugins install zerosignal --enable"; got != want {
		t.Errorf("installer args = %q, want %q", got, want)
	}
	if want := "ZEROSIGNAL_API_KEY=" + PlaceholderAPIKey + "\n"; !strings.Contains(string(envAtInstall), want) {
		t.Errorf(".env when the installer started = %q, want it to already hold %q", envAtInstall, want)
	}
	if !strings.Contains(string(confAtInstall), "transport: chat_completions") {
		t.Errorf("config.yaml when the installer started = %q, want the provider block already written", confAtInstall)
	}
	if !res.Wrote || !strings.Contains(res.Hint, "installed and enabled") {
		t.Errorf("Result = %+v, want Wrote and a hint reporting the install", res)
	}
}

// TestHermes_NoPluginFlagNeverInstalls: the install is opt-in. A plain
// `connect hermes` — and every --all / detected run, which share this path —
// must not run a third party's installer. It offers the command instead, but
// only while there is something to install: re-running connect is routine, and
// the advice would fail with Hermes' "already exists" for anyone who took it.
func TestHermes_NoPluginFlagNeverInstalls(t *testing.T) {
	const installCmd = "hermes plugins install zerosignal --enable"
	tests := []struct {
		name        string
		preinstall  string
		wantCommand bool
	}{
		{"no plugin yet", "", true},
		{"plugin present", filepath.Join("plugins", "zerosignal"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HERMES_HOME", home)
			if tc.preinstall != "" {
				if err := os.MkdirAll(filepath.Join(home, tc.preinstall), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			calls := stubHermes(t, nil)

			res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(*calls) != 0 {
				t.Errorf("installer ran without InstallPlugin: %v", *calls)
			}
			if got := strings.Contains(res.Hint, installCmd); got != tc.wantCommand {
				t.Errorf("hint offers the install command = %v, want %v:\n%s", got, tc.wantCommand, res.Hint)
			}
		})
	}
}

// TestHermes_PluginInstallFailureKeepsConfig: a failed install is reported, and
// the config it followed stays — the provider block is what Hermes uses while no
// plugin is registered, so rolling it back would trade a working setup for none.
func TestHermes_PluginInstallFailureKeepsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	boom := errors.New("exit status 1")
	stubHermes(t, func() error { return boom })

	_, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{InstallPlugin: true})
	if !errors.Is(err, boom) {
		t.Fatalf("Apply error = %v, want it to wrap the installer's", err)
	}
	// The retry has to be the connect command, not the bare installer: a failure
	// late enough to have moved the plugin into place answers the bare one with
	// Hermes' "already exists", while connect skips and succeeds.
	if !strings.Contains(err.Error(), "zs-proxy connect hermes --plugin") {
		t.Errorf("error = %q, want it to name the connect retry", err)
	}
	if strings.Contains(err.Error(), "plugins install") {
		t.Errorf("error = %q, want it not to send the user to the bare installer", err)
	}
	for _, name := range []string{"config.yaml", ".env"} {
		if _, statErr := os.Stat(filepath.Join(home, name)); statErr != nil {
			t.Errorf("%s missing after a failed install (%v), want the written config kept", name, statErr)
		}
	}
}

// TestHermes_PluginAlreadyInstalledSkips: re-running connect is how a user
// re-points Hermes at a moved proxy, and Hermes answers a second install with
// "already exists". Both discovery locations count, and the config is still
// rewritten — that rewrite is the reason for the re-run.
func TestHermes_PluginAlreadyInstalledSkips(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("plugins", "zerosignal"),
		filepath.Join("plugins", "model-providers", "zerosignal"),
	} {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HERMES_HOME", home)
			if err := os.MkdirAll(filepath.Join(home, rel), 0o755); err != nil {
				t.Fatal(err)
			}
			calls := stubHermes(t, nil)

			res, err := (hermes{}).Apply(testEndpoint(), testModels(), ApplyOptions{InstallPlugin: true})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(*calls) != 0 {
				t.Errorf("installer ran over an existing plugin: %v", *calls)
			}
			if !strings.Contains(res.Hint, "already installed") {
				t.Errorf("hint = %q, want it to say the plugin is already installed", res.Hint)
			}
			env, _ := os.ReadFile(filepath.Join(home, ".env"))
			if want := "ZEROSIGNAL_BASE_URL=" + testEndpoint().BaseURL; !strings.Contains(string(env), want) {
				t.Errorf(".env = %q, want %q rewritten on the re-run", env, want)
			}
		})
	}
}

// TestCodex_OmitsEnvKeyAndPrunesLegacy proves the provider carries no env_key —
// the proxy admits via the on-chain seal, so Codex needs no key at all — and that
// re-applying strips the one earlier versions wrote, without touching the env_key
// of a provider we don't own.
func TestCodex_OmitsEnvKeyAndPrunesLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := []byte(`model_provider = "zerosignal"

[model_providers.zerosignal]
name = "ZeroSignal"
base_url = "http://127.0.0.1:9999/v1"
wire_api = "responses"
env_key = "` + legacyCodexEnvKey + `"

[model_providers.mine]
name = "Mine"
base_url = "https://example.test/v1"
env_key = "MY_KEY"
`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (codex{}).Apply(testEndpoint(), testModels(), ApplyOptions{
		DefaultModel: "qwen3-coder", ConfigPathOverride: path,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Hint != "" {
		t.Errorf("Result.Hint = %q, want empty (no env var to export)", res.Hint)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if contains(out, legacyCodexEnvKey) {
		t.Errorf("legacy env_key survived the merge:\n%s", out)
	}

	var root struct {
		ModelProvider  string `toml:"model_provider"`
		Model          string `toml:"model"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
			WireAPI string `toml:"wire_api"`
			EnvKey  string `toml:"env_key"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(out, &root); err != nil {
		t.Fatalf("parse written TOML: %v\n%s", err, out)
	}
	ours := root.ModelProviders[ProviderID]
	if ours.EnvKey != "" {
		t.Errorf("%s.env_key = %q, want unset", ProviderID, ours.EnvKey)
	}
	if ours.BaseURL != testEndpoint().BaseURL || ours.WireAPI != "responses" {
		t.Errorf("provider = %+v, want base_url=%s wire_api=responses", ours, testEndpoint().BaseURL)
	}
	if root.ModelProvider != ProviderID || root.Model != "qwen3-coder" {
		t.Errorf("model_provider=%q model=%q, want %q/qwen3-coder", root.ModelProvider, root.Model, ProviderID)
	}
	// The prune is scoped to our table: a provider we don't own keeps its key.
	if got := root.ModelProviders["mine"].EnvKey; got != "MY_KEY" {
		t.Errorf("mine.env_key = %q, want MY_KEY (prune escaped our provider)", got)
	}
}

// codexModels pairs a plain model with a reasoning one so the metadata tests can
// switch between them and watch the top-level keys follow.
func codexModels() []Model {
	return []Model{
		{ID: "qwen3-coder", ContextWindow: 262144, InputModalities: []string{"text"}},
		{ID: "thinker", ContextWindow: 131072, InputModalities: []string{"text"}, Reasoning: true},
		{ID: "sizeless", InputModalities: []string{"text"}},
	}
}

// codexRoot is the subset of Codex's top-level config the metadata tests assert
// on. Pointers distinguish "absent" from "present and false/zero", which is the
// whole point of the prune behavior below.
type codexRoot struct {
	Model                string `toml:"model"`
	ModelProvider        string `toml:"model_provider"`
	ModelContextWindow   *int   `toml:"model_context_window"`
	ModelSupportsSummary *bool  `toml:"model_supports_reasoning_summaries"`
}

func applyCodex(t *testing.T, path string, models []Model, model string) codexRoot {
	t.Helper()
	if _, err := (codex{}).Apply(testEndpoint(), models, ApplyOptions{
		DefaultModel: model, ConfigPathOverride: path,
	}); err != nil {
		t.Fatalf("Apply(%s): %v", model, err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var root codexRoot
	if err := toml.Unmarshal(out, &root); err != nil {
		t.Fatalf("parse written TOML: %v\n%s", err, out)
	}
	return root
}

// TestCodex_WritesModelContextWindow covers the reason connect started reading
// the model list at all: Codex resolves the active model against a catalog that
// only holds OpenAI slugs, so without model_context_window a ZeroSignal model
// runs on whatever Codex assumes.
func TestCodex_WritesModelContextWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	root := applyCodex(t, path, codexModels(), "qwen3-coder")

	if root.ModelContextWindow == nil {
		t.Fatal("model_context_window absent, want 262144")
	}
	if *root.ModelContextWindow != 262144 {
		t.Errorf("model_context_window = %d, want 262144", *root.ModelContextWindow)
	}
}

// TestCodex_PrunesStaleModelContextWindow: the key is global, not per-provider,
// so switching to a model with no advertised window has to clear the previous
// model's number instead of silently keeping it applied.
func TestCodex_PrunesStaleModelContextWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if root := applyCodex(t, path, codexModels(), "qwen3-coder"); root.ModelContextWindow == nil {
		t.Fatal("setup: first apply wrote no model_context_window")
	}
	root := applyCodex(t, path, codexModels(), "sizeless")
	if root.ModelContextWindow != nil {
		t.Errorf("model_context_window = %d after switching to an unsized model, want absent", *root.ModelContextWindow)
	}
	if root.Model != "sizeless" {
		t.Errorf("model = %q, want sizeless", root.Model)
	}
}

// TestCodex_ReasoningSummariesOnlyWhenAdvertised pins the house rule that
// Model.Reasoning may only ever turn a reasoning feature ON: false also means
// "unadvertised", so it must never be written out as an explicit false.
func TestCodex_ReasoningSummariesOnlyWhenAdvertised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	root := applyCodex(t, path, codexModels(), "thinker")
	if root.ModelSupportsSummary == nil || !*root.ModelSupportsSummary {
		t.Fatalf("model_supports_reasoning_summaries = %v for a reasoning model, want true", root.ModelSupportsSummary)
	}
	// Switching to a non-reasoning model removes the key rather than asserting
	// false — Codex decides, and the stale true must not survive.
	root = applyCodex(t, path, codexModels(), "qwen3-coder")
	if root.ModelSupportsSummary != nil {
		t.Errorf("model_supports_reasoning_summaries = %v for a non-reasoning model, want absent", *root.ModelSupportsSummary)
	}
}

// TestCodex_LeavesModelKeysAloneWhenModelUnknown: these keys are global, so when
// we can't ground a value in the advertised list we must not touch whatever the
// user set for some other provider.
func TestCodex_LeavesModelKeysAloneWhenModelUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	seed := []byte("model_context_window = 999\nmodel_supports_reasoning_summaries = true\n")
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	// A model the proxy never advertised (the --model warning path), and the
	// no-model-selected path.
	for _, model := range []string{"not-advertised", ""} {
		root := applyCodex(t, path, codexModels(), model)
		if root.ModelContextWindow == nil || *root.ModelContextWindow != 999 {
			t.Errorf("model=%q: model_context_window = %v, want the user's 999 untouched", model, root.ModelContextWindow)
		}
		if root.ModelSupportsSummary == nil || !*root.ModelSupportsSummary {
			t.Errorf("model=%q: model_supports_reasoning_summaries = %v, want the user's true untouched", model, root.ModelSupportsSummary)
		}
	}
}

// TestCodex_LeavesModelKeysAloneWhenMetadataUnadvertised: a model the proxy only
// *named* carries no more information than one it never mentioned, so it must
// take the same untouched path. Two real states produce it — the /v1/models
// fallback, and a details entry whose capability probe hasn't landed — and
// before this, either one silently deleted a correct model_context_window on the
// next `connect codex`.
func TestCodex_LeavesModelKeysAloneWhenMetadataUnadvertised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if root := applyCodex(t, path, codexModels(), "thinker"); root.ModelContextWindow == nil {
		t.Fatal("setup: first apply wrote no model_context_window")
	}
	// Exactly what fetchModelList returns: same ids, id only.
	idOnly := []Model{{ID: "qwen3-coder"}, {ID: "thinker"}, {ID: "sizeless"}}
	root := applyCodex(t, path, idOnly, "thinker")
	if root.ModelContextWindow == nil || *root.ModelContextWindow != 131072 {
		t.Errorf("model_context_window = %v, want the grounded 131072 left in place", root.ModelContextWindow)
	}
	if root.ModelSupportsSummary == nil || !*root.ModelSupportsSummary {
		t.Errorf("model_supports_reasoning_summaries = %v, want the grounded true left in place", root.ModelSupportsSummary)
	}
	// The provider block still lands — an ungrounded model list disables only the
	// global model keys, not the whole apply.
	if root.ModelProvider != ProviderID || root.Model != "thinker" {
		t.Errorf("model_provider=%q model=%q, want %s/thinker", root.ModelProvider, root.Model, ProviderID)
	}
}

// TestCodex_DetectHonorsCodexHome: resolvePath honors CODEX_HOME, so the
// installed-ness probe has to resolve from the same place or a relocated Codex
// reports not-installed while connect happily writes into it.
func TestCodex_DetectHonorsCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	// Detection has three independent triggers (the config file, the Codex home
	// directory, and `codex` on PATH). Neutralize the other two or a developer
	// machine with a real ~/.codex or an installed CLI answers "installed" no
	// matter what CODEX_HOME says, and this test proves nothing.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty) // os.UserHomeDir on Windows
	t.Setenv("PATH", empty)

	path, installed := (codex{}).Detect()
	if want := filepath.Join(dir, "config.toml"); path != want {
		t.Errorf("Detect path = %q, want %q", path, want)
	}
	// CODEX_HOME exists (t.TempDir made it) and is now the only thing that can
	// make this true.
	if !installed {
		t.Error("Detect reported not-installed for an existing CODEX_HOME directory")
	}

	// The negative half, without which every assertion above still passes if
	// Detect hardcodes installed=true: same neutralized environment, but a
	// CODEX_HOME that doesn't exist.
	missing := filepath.Join(dir, "not-created")
	t.Setenv("CODEX_HOME", missing)
	path, installed = (codex{}).Detect()
	if want := filepath.Join(missing, "config.toml"); path != want {
		t.Errorf("Detect path = %q, want %q", path, want)
	}
	if installed {
		t.Error("Detect reported installed for a CODEX_HOME that doesn't exist")
	}
}

func TestGeneric_RendersEnvVarsNoFile(t *testing.T) {
	res, err := (generic{}).Apply(testEndpoint(), nil, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Wrote || res.Path != "" {
		t.Errorf("generic wrote a file: %+v", res)
	}
	for _, sub := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "http://127.0.0.1:8080/v1"} {
		if !contains([]byte(res.Rendered), sub) {
			t.Errorf("rendered env snippet missing %q:\n%s", sub, res.Rendered)
		}
	}
}

func TestDetect_ReturnsPathAndInstalledFlag(t *testing.T) {
	// Every file integration reports a non-empty path; generic reports none.
	for _, in := range All() {
		path, _ := in.Detect()
		if in.ID() == "generic" {
			if path != "" {
				t.Errorf("generic Detect path = %q, want empty", path)
			}
			continue
		}
		if path == "" {
			t.Errorf("%s Detect returned empty path", in.ID())
		}
	}
}

func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
