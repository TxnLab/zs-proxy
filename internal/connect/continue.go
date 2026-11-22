/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"bytes"
	"fmt"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// continueDev configures Continue (https://continue.dev).
//
// Config is YAML at ~/.continue/config.yaml with a top-level "models" LIST.
// Each OpenAI-compatible model is its own list entry (name / provider:"openai" /
// model / apiBase / apiKey / roles). Because it's a list (not a keyed map), the
// merge appends an entry per advertised model and dedups prior zs-proxy entries
// by their "zs-proxy/" name prefix, so re-running stays idempotent.
type continueDev struct{}

func (continueDev) ID() string          { return "continue" }
func (continueDev) DisplayName() string { return "Continue" }

func (continueDev) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return homeJoin(".continue", "config.yaml")
}

func (c continueDev) Detect() (string, bool) {
	path, err := c.resolvePath("")
	if err != nil {
		return "", false
	}
	dir, _ := homeJoin(".continue")
	return path, fileExists(path) || dirExists(dir)
}

// namePrefix labels every entry this integration writes so re-runs can replace
// them without touching the user's other models.
const continueNamePrefix = ProviderID + "/"

func (c continueDev) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := c.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	root := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return Result{}, fmt.Errorf("parse existing Continue config: %w", err)
		}
		if root == nil {
			root = map[string]any{}
		}
	}
	// A fresh config.yaml needs the top-level header fields Continue requires.
	if _, ok := root["name"]; !ok {
		root["name"] = "My Config"
	}
	if _, ok := root["version"]; !ok {
		root["version"] = "0.0.1"
	}
	if _, ok := root["schema"]; !ok {
		root["schema"] = "v1"
	}

	// Preserve the user's non-zs-proxy model entries.
	var kept []any
	if cur, ok := root["models"].([]any); ok {
		for _, e := range cur {
			if m, ok := e.(map[string]any); ok {
				if name, _ := m["name"].(string); strings.HasPrefix(name, continueNamePrefix) {
					continue // drop our previous entries; we re-add fresh below
				}
			}
			kept = append(kept, e)
		}
	}

	add := models
	if len(add) == 0 && opts.DefaultModel != "" {
		add = []Model{{ID: opts.DefaultModel}}
	}
	for _, m := range add {
		// Continue's capability vocabulary is exactly {tool_use, image_input}
		// — tool_use is what gates Agent mode, image_input what gates image
		// upload. An unadvertised tool_use stays enabled (most chat models
		// tool-call, and losing Agent mode is the costlier mistake); an operator
		// saying false is honored.
		var caps []any
		if m.ToolUse == nil || *m.ToolUse {
			caps = append(caps, "tool_use")
		}
		for _, mod := range m.InputModalities {
			if mod == "image" {
				caps = append(caps, "image_input")
			}
		}
		entry := map[string]any{
			"name":     continueNamePrefix + m.ID,
			"provider": "openai",
			"model":    m.ID,
			"apiBase":  ep.BaseURL,
			"apiKey":   ep.Key(),
			"roles":    []any{"chat", "edit", "apply"},
		}
		if len(caps) > 0 {
			entry["capabilities"] = caps
		}
		// Sizing rides defaultCompletionOptions, not the entry root. Without it
		// Continue falls back to a generic context length and prunes history
		// against the wrong number.
		completion := map[string]any{}
		if m.ContextWindow > 0 {
			completion["contextLength"] = m.ContextWindow
		}
		if out := maxOutputTokens(m); out > 0 {
			completion["maxTokens"] = out
		}
		if len(completion) > 0 {
			entry["defaultCompletionOptions"] = completion
		}
		kept = append(kept, entry)
	}
	root["models"] = kept

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return Result{}, fmt.Errorf("encode Continue config: %w", err)
	}
	_ = enc.Close()
	merged := buf.Bytes()

	hint := fmt.Sprintf("Continue lists each model as %q<id>; pick one from the model dropdown.", continueNamePrefix)
	if opts.Print {
		return Result{Path: path, Rendered: string(merged), Hint: hint}, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Wrote: true, Hint: hint}, nil
}
