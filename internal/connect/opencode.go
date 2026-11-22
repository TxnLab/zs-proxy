/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// opencode configures the opencode agent (https://opencode.ai).
//
// Config is JSONC at ~/.config/opencode/opencode.jsonc (preferred) or
// opencode.json, overridable via OPENCODE_CONFIG. opencode loads the .jsonc
// variant first and merges config sources, so a custom provider lives under the
// top-level "provider" key using the @ai-sdk/openai-compatible adapter, with
// the active model named "<provider>/<model>" under the top-level "model" key.
type opencode struct{}

func (opencode) ID() string          { return "opencode" }
func (opencode) DisplayName() string { return "opencode" }

// resolvePath returns the opencode config file to read/write. Priority:
// override → OPENCODE_CONFIG → existing ~/.config/opencode/opencode.jsonc →
// existing opencode.json → default opencode.jsonc (the format opencode loads
// first, so a fresh file can carry comments).
func (opencode) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("OPENCODE_CONFIG"); env != "" {
		return env, nil
	}
	dir, err := xdgConfig("opencode")
	if err != nil {
		return "", err
	}
	jsonc := dir + "/opencode.jsonc"
	plain := dir + "/opencode.json"
	if fileExists(jsonc) {
		return jsonc, nil
	}
	if fileExists(plain) {
		return plain, nil
	}
	return jsonc, nil
}

func (o opencode) Detect() (string, bool) {
	path, err := o.resolvePath("")
	if err != nil {
		return "", false
	}
	dir, _ := xdgConfig("opencode")
	installed := fileExists(path) || dirExists(dir)
	if _, err := exec.LookPath("opencode"); err == nil {
		installed = true
	}
	return path, installed
}

func (o opencode) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := o.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}

	modelMap := map[string]any{}
	for _, m := range models {
		entry := map[string]any{"name": m.ID}

		// opencode defaults every capability flag on a config-declared model to
		// the pessimistic value (attachment/reasoning/temperature false,
		// modalities text-only; tool_call is the one that defaults true), so a
		// bare {name, limit, cost} entry silently strips vision, thinking, and
		// the temperature control from an otherwise capable model. Declare them
		// all explicitly from what the proxy advertises.
		inMods := normalizeModalities(m.InputModalities)
		outMods := normalizeModalities(m.OutputModalities)
		entry["modalities"] = map[string]any{"input": inMods, "output": outMods}
		// attachment gates opencode's file/image attachment UI: on whenever the
		// model takes something beyond text.
		entry["attachment"] = hasNonTextModality(inMods)
		// Reasoning is false when unadvertised — this only ever turns opencode's
		// thinking display on, never suppresses one it would otherwise show.
		entry["reasoning"] = m.Reasoning
		// Every OpenAI-compatible chat model the proxy fronts accepts
		// temperature; there is no per-model signal for it to key off.
		entry["temperature"] = true
		// nil tool_use = unadvertised, which we render as true (matching
		// opencode's own default and the fact that most chat models tool-call);
		// an explicit false from the operator is honored.
		entry["tool_call"] = m.ToolUse == nil || *m.ToolUse

		if m.ContextWindow > 0 {
			// opencode requires context and output together when a limit is set,
			// so an unadvertised max_output_tokens falls back to the derived
			// budget rather than dropping the whole limit block.
			entry["limit"] = map[string]any{
				"context": m.ContextWindow,
				"output":  maxOutputTokens(m),
			}
		}
		// Populate opencode's per-model cost so its session spend display is a
		// real (payer-net) estimate instead of $0. opencode's cost schema is
		// snake_case USD/1M; rates arrive already grossed up for the protocol
		// fee. Only emit when a rate was advertised — a genuinely-free or
		// unpriced model (both 0 in the proxy aggregate) is left without a cost
		// block rather than advertising a misleading $0. The proxy meters and
		// bills the authoritative amount on-chain regardless; this is advisory.
		if m.InputUSDPer1M > 0 || m.OutputUSDPer1M > 0 {
			cost := map[string]any{
				"input":  m.InputUSDPer1M,
				"output": m.OutputUSDPer1M,
			}
			// A cached read bills at the advertised discount, else at the input
			// rate (no discount). ZeroSignal has no cache-write premium — writing
			// to cache bills at the normal input rate.
			if m.CacheReadUSDPer1M != nil {
				cost["cache_read"] = *m.CacheReadUSDPer1M
			} else {
				cost["cache_read"] = m.InputUSDPer1M
			}
			cost["cache_write"] = m.InputUSDPer1M
			entry["cost"] = cost
		}
		modelMap[m.ID] = entry
	}

	provider := map[string]any{
		"npm":  "@ai-sdk/openai-compatible",
		"name": ProviderDisplay,
		"options": map[string]any{
			"baseURL": ep.BaseURL,
			"apiKey":  ep.Key(),
		},
		"models": modelMap,
	}

	sets := []jsonSet{{Pointer: "/provider/" + ProviderID, Value: provider}}
	if opts.DefaultModel != "" {
		sets = append(sets, jsonSet{Pointer: "/model", Value: ProviderID + "/" + opts.DefaultModel})
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	if len(strings.TrimSpace(string(existing))) == 0 {
		// Seed a fresh file with the JSON schema (editor autocomplete) and a
		// marker comment, so a new opencode.jsonc is clean and self-describing.
		existing = []byte("// Managed by `zs-proxy connect`.\n{\n  \"$schema\": \"https://opencode.ai/config.json\"\n}\n")
	}
	merged, err := mergeJSONC(existing, sets)
	if err != nil {
		return Result{}, err
	}

	hint := fmt.Sprintf("Run opencode and pick a %q model (e.g. /models), or it'll use the default if set.", ProviderID)
	if opts.Print {
		return Result{Path: path, Rendered: string(merged), Hint: hint}, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Wrote: true, Hint: hint}, nil
}
