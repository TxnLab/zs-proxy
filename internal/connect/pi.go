/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// pi configures the Pi coding agent (https://pi.dev). Pi is the agent stack
// OpenClaw is built on, so its provider/model schema matches openclaw's
// field-for-field (camelCase cost, input[], contextWindow/maxTokens).
//
// Pi splits what every other integration keeps in one file across two, both
// under ~/.pi/agent (overridable via PI_CODING_AGENT_DIR, which names the
// *directory*, unlike the file-path env vars the other tools use):
//
//   - models.json   {"providers": {"<name>": {baseUrl, apiKey, api, models[]}}}
//   - settings.json {"defaultProvider": "<name>", "defaultModel": "<bare id>"}
//
// Two traps, both verified against pi's loader:
//
//   - The default model is a *bare* id paired with a separate defaultProvider —
//     pi takes --provider and --model as distinct flags and looks the model up
//     within the provider. It is NOT the "<provider>/<model>" form opencode and
//     openclaw use.
//   - models.json is comment-tolerant (pi strips comments before parsing) but
//     settings.json is parsed as strict JSON. Neither is seeded with a comment
//     header for that reason.
//
// models.json is schema-validated as a whole: pi rejects the entire file (not
// just the offending entry) if it doesn't validate, so the shape here is
// deliberate. A non-built-in provider defining models must carry baseUrl,
// apiKey, and an api; a cost block must carry all four rates; contextWindow and
// maxTokens must be > 0 when present. Custom models are upserted by id and
// built-ins are preserved, so there is no merge-mode key to set.
type pi struct{}

func (pi) ID() string          { return "pi" }
func (pi) DisplayName() string { return "Pi" }

// agentDir is pi's config directory: $PI_CODING_AGENT_DIR (tilde-expanded by pi
// itself; a bare ~ is left alone here since Go's file APIs don't expand it) or
// ~/.pi/agent.
func (pi) agentDir() (string, error) {
	if env := os.Getenv("PI_CODING_AGENT_DIR"); env != "" {
		return env, nil
	}
	return homeJoin(".pi", "agent")
}

func (p pi) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir, err := p.agentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models.json"), nil
}

// settingsPath is models.json's sibling, so a --config-path override keeps both
// files together (which is also what lets the tests target a temp dir).
func (p pi) settingsPath(modelsPath string) string {
	return filepath.Join(filepath.Dir(modelsPath), "settings.json")
}

func (p pi) Detect() (string, bool) {
	path, err := p.resolvePath("")
	if err != nil {
		return "", false
	}
	dir, _ := p.agentDir()
	installed := fileExists(path) || dirExists(dir)
	if _, err := exec.LookPath("pi"); err == nil {
		installed = true
	}
	return path, installed
}

func (p pi) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := p.resolvePath(opts.ConfigPathOverride)
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
		// Pi's cost schema is camelCase USD/1M and requires all four rates once
		// the block is present. Populate the advertised (payer-net, already
		// fee-grossed) rates so pi's own spend display is a real estimate; fall
		// back to a zeroed block for an unpriced/free model. The proxy meters
		// and bills the authoritative amount on-chain regardless; this is
		// advisory. A cached read bills at the advertised discount, else at the
		// input rate; ZeroSignal has no cache-write premium (cache writes bill
		// at input).
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
			// input takes only "text" and "image" here (pi's schema enumerates
			// exactly those two), and reasoning is a plain bool — both are part
			// of the validated model shape, and both default to the pessimistic
			// value when omitted. False reasoning also covers "unadvertised": it
			// only ever turns pi's thinking display on.
			"input":     input,
			"cost":      cost,
			"reasoning": m.Reasoning,
		}
		if m.ContextWindow > 0 {
			entry["contextWindow"] = m.ContextWindow
		}
		if out := maxOutputTokens(m); out > 0 {
			entry["maxTokens"] = out
		}
		modelList = append(modelList, entry)
	}

	provider := map[string]any{
		"name":    ProviderDisplay,
		"baseUrl": ep.BaseURL,
		"apiKey":  ep.Key(),
		"api":     "openai-completions",
		"models":  modelList,
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	merged, err := mergeJSONC(existing, []jsonSet{
		{Pointer: "/providers/" + ProviderID, Value: provider},
	})
	if err != nil {
		return Result{}, err
	}

	hint := fmt.Sprintf("Run `pi --provider %s --model <model>`, or pick a %s model with /model.", ProviderID, ProviderDisplay)
	res := Result{Path: path, Hint: hint}

	// The default model lives in settings.json, not models.json. Leave the file
	// alone entirely when there's no model to set.
	var settings []byte
	settingsPath := p.settingsPath(path)
	if opts.DefaultModel != "" {
		existingSettings, err := readFileOrEmpty(settingsPath)
		if err != nil {
			return Result{}, err
		}
		settings, err = mergeJSONC(existingSettings, []jsonSet{
			{Pointer: "/defaultProvider", Value: ProviderID},
			{Pointer: "/defaultModel", Value: opts.DefaultModel},
		})
		if err != nil {
			return Result{}, err
		}
	}

	if opts.Print {
		res.Rendered = string(merged)
		if settings != nil {
			res.Extra = []Written{{Path: settingsPath, Rendered: string(settings)}}
		}
		return res, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	if settings != nil {
		if err := atomicWriteWithBackup(settingsPath, settings, 0o644); err != nil {
			return Result{}, err
		}
		res.Extra = []Written{{Path: settingsPath}}
	}
	res.Wrote = true
	return res, nil
}
