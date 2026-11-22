/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"math"
	"os/exec"
	"path/filepath"
)

// aider configures aider (https://aider.chat).
//
// Config is YAML at ~/.aider.conf.yml. aider routes through LiteLLM, so an
// OpenAI-compatible endpoint is selected with the openai-api-base / openai-api-key
// keys and a model named "openai/<model>".
//
// Because the models are ours, LiteLLM's catalog knows nothing about them —
// aider warns "Unknown context window size and costs" and falls back to generic
// behavior for everything it gates on model_info. So this integration writes a
// second file, the model metadata sidecar (`.aider.model.metadata.json`), keyed
// by the same "openai/<id>" name, and points `model-metadata-file` at it. Keys
// are upserted, so a user's hand-written entries for other models survive.
type aider struct{}

func (aider) ID() string          { return "aider" }
func (aider) DisplayName() string { return "aider" }

func (aider) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return homeJoin(".aider.conf.yml")
}

// metadataPath is the conf file's sibling, so a --config-path override keeps
// both files together (which is also what lets the tests target a temp dir).
func (aider) metadataPath(confPath string) string {
	return filepath.Join(filepath.Dir(confPath), ".aider.model.metadata.json")
}

func (a aider) Detect() (string, bool) {
	path, err := a.resolvePath("")
	if err != nil {
		return "", false
	}
	installed := fileExists(path)
	if _, err := exec.LookPath("aider"); err == nil {
		installed = true
	}
	return path, installed
}

func (a aider) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := a.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}
	metaPath := a.metadataPath(path)

	add := map[string]any{
		"openai-api-base": ep.BaseURL,
		"openai-api-key":  ep.Key(),
	}
	if opts.DefaultModel != "" {
		add["model"] = "openai/" + opts.DefaultModel
	}
	if len(models) > 0 {
		add["model-metadata-file"] = metaPath
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	merged, err := mergeYAML(existing, add)
	if err != nil {
		return Result{}, err
	}

	hint := "aider uses model ids like `openai/<model>`; switch in-session with `/model openai/<model>`."
	res := Result{Path: path, Hint: hint}

	// Leave the sidecar alone entirely when there are no models to describe.
	var metadata []byte
	if len(models) > 0 {
		existingMeta, err := readFileOrEmpty(metaPath)
		if err != nil {
			return Result{}, err
		}
		sets := make([]jsonSet, 0, len(models))
		for _, m := range models {
			sets = append(sets, jsonSet{
				Pointer: "/" + jsonPointerEscape(aiderModelKey(m.ID)),
				Value:   aiderModelInfo(m),
			})
		}
		metadata, err = mergeJSONC(existingMeta, sets)
		if err != nil {
			return Result{}, err
		}
	}

	if opts.Print {
		res.Rendered = string(merged)
		if metadata != nil {
			res.Extra = []Written{{Path: metaPath, Rendered: string(metadata)}}
		}
		return res, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	if metadata != nil {
		if err := atomicWriteWithBackup(metaPath, metadata, 0o644); err != nil {
			return Result{}, err
		}
		res.Extra = []Written{{Path: metaPath}}
	}
	res.Wrote = true
	return res, nil
}

// aiderModelKey is the metadata key for a model id: the same "openai/<id>" name
// aider addresses the model by (LiteLLM looks model_info up by the
// fully-qualified name, not the bare id).
func aiderModelKey(id string) string { return "openai/" + id }

// aiderModelInfo is one LiteLLM model_info entry. The cost keys are LiteLLM's
// per-TOKEN rates (hence the /1e6 off our per-1M advertising rates), and the
// supports_* booleans are what aider reads to decide whether it may send images
// or tool definitions. Unpriced models get no cost keys at all: LiteLLM reads a
// present 0 as "free", which isn't a claim we can make about a rate that simply
// wasn't advertised.
func aiderModelInfo(m Model) map[string]any {
	info := map[string]any{
		"litellm_provider": "openai",
		"mode":             "chat",
		// Unadvertised tool use stays on — same reasoning as the other
		// integrations: most chat models tool-call, and aider's editing flow
		// leans on it.
		"supports_function_calling": m.ToolUse == nil || *m.ToolUse,
		"supports_vision":           hasNonTextModality(normalizeModalities(m.InputModalities)),
	}
	if m.ContextWindow > 0 {
		info["max_input_tokens"] = m.ContextWindow
	}
	if out := maxOutputTokens(m); out > 0 {
		// max_tokens is LiteLLM's legacy alias for max_output_tokens and aider
		// reads both depending on the code path, so write both.
		info["max_output_tokens"] = out
		info["max_tokens"] = out
	}
	if m.InputUSDPer1M > 0 || m.OutputUSDPer1M > 0 {
		info["input_cost_per_token"] = perTokenCost(m.InputUSDPer1M)
		info["output_cost_per_token"] = perTokenCost(m.OutputUSDPer1M)
		// A cached read bills at the advertised discount, else at the input rate.
		cacheRead := m.InputUSDPer1M
		if m.CacheReadUSDPer1M != nil {
			cacheRead = *m.CacheReadUSDPer1M
		}
		info["cache_read_input_token_cost"] = perTokenCost(cacheRead)
	}
	return info
}

// perTokenCost converts a USD-per-1M-token rate to LiteLLM's per-token cost,
// rounding at 1e-15 so the JSON carries 1.53e-07 rather than a float artifact.
func perTokenCost(usdPer1M float64) float64 {
	return math.Round(usdPer1M*1e9) / 1e15
}
