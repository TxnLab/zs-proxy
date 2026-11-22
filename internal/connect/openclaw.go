/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"fmt"
	"os"
	"os/exec"
)

// openclaw configures the OpenClaw agent (https://openclaw.ai).
//
// Config is JSON5 (comments/trailing commas allowed; on-disk filename .json) at
// ~/.openclaw/openclaw.json, overridable via OPENCLAW_CONFIG_PATH. A custom
// OpenAI-compatible provider lives under "models.providers.<name>" with
// baseUrl / apiKey / api:"openai-completions" / models[]; "models.mode":"merge"
// keeps the built-in provider catalog. The active model is set at
// "agents.defaults.model.primary" as "<provider>/<model>".
type openclaw struct{}

func (openclaw) ID() string          { return "openclaw" }
func (openclaw) DisplayName() string { return "OpenClaw" }

func (openclaw) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("OPENCLAW_CONFIG_PATH"); env != "" {
		return env, nil
	}
	return homeJoin(".openclaw", "openclaw.json")
}

func (o openclaw) Detect() (string, bool) {
	path, err := o.resolvePath("")
	if err != nil {
		return "", false
	}
	dir, _ := homeJoin(".openclaw")
	installed := fileExists(path) || dirExists(dir)
	if _, err := exec.LookPath("openclaw"); err == nil {
		installed = true
	}
	return path, installed
}

func (o openclaw) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := o.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}

	modelList := make([]any, 0, len(models))
	for _, m := range models {
		input := []any{"text"}
		for _, mod := range m.InputModalities {
			if mod == "image" {
				input = append(input, "image")
			}
		}
		// OpenClaw's cost schema is camelCase USD/1M. Populate the advertised
		// (payer-net, already fee-grossed) rates so OpenClaw's own spend display
		// is a real estimate; fall back to a zeroed block for an unpriced/free
		// model (both 0 in the proxy aggregate). The proxy meters and bills the
		// authoritative amount on-chain regardless; this is advisory. A cached
		// read bills at the advertised discount, else at the input rate;
		// ZeroSignal has no cache-write premium (cache writes bill at input).
		cost := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}
		if m.InputUSDPer1M > 0 || m.OutputUSDPer1M > 0 {
			cost["input"] = m.InputUSDPer1M
			cost["output"] = m.OutputUSDPer1M
			if m.CacheReadUSDPer1M != nil {
				cost["cacheRead"] = *m.CacheReadUSDPer1M
			} else {
				cost["cacheRead"] = m.InputUSDPer1M
			}
			cost["cacheWrite"] = m.InputUSDPer1M
		}
		entry := map[string]any{
			"id":   m.ID,
			"name": m.ID,
			// input is OpenClaw's modality signal (only "text" and "image" are
			// valid there — it has no audio/video/pdf vocabulary and no tool-call
			// flag). Omitted it defaults to ["text"], which strips vision.
			"input": input,
			"cost":  cost,
			// reasoning likewise defaults to false when omitted, hiding the
			// thinking display on a model that has one. False here also covers
			// "unadvertised" — it only ever turns the display on.
			"reasoning": m.Reasoning,
		}
		if m.ContextWindow > 0 {
			entry["contextWindow"] = m.ContextWindow
		}
		// An omitted maxTokens falls back to OpenClaw's flat 8192 default, so
		// emit the derived budget whenever there's a context window to scale from.
		if out := maxOutputTokens(m); out > 0 {
			entry["maxTokens"] = out
		}
		modelList = append(modelList, entry)
	}

	provider := map[string]any{
		"baseUrl": ep.BaseURL,
		"apiKey":  ep.Key(),
		"api":     "openai-completions",
		"models":  modelList,
	}

	sets := []jsonSet{
		{Pointer: "/models/mode", Value: "merge"},
		{Pointer: "/models/providers/" + ProviderID, Value: provider},
	}
	if opts.DefaultModel != "" {
		sets = append(sets, jsonSet{
			Pointer: "/agents/defaults/model/primary",
			Value:   ProviderID + "/" + opts.DefaultModel,
		})
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	merged, err := mergeJSONC(existing, sets)
	if err != nil {
		return Result{}, err
	}

	hint := fmt.Sprintf("In OpenClaw, select a %q model or run `openclaw models set %s/<model>`.", ProviderID, ProviderID)
	if opts.Print {
		return Result{Path: path, Rendered: string(merged), Hint: hint}, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Wrote: true, Hint: hint}, nil
}
