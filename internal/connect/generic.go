/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import "fmt"

// generic is the universal fallback: it prints the OpenAI environment variables
// that point any OpenAI-compatible CLI/SDK at the proxy, rather than writing a
// tool-specific config file. It is never auto-detected (Detect reports not
// installed) — run it explicitly with `zs-proxy connect generic`.
type generic struct{}

func (generic) ID() string          { return "generic" }
func (generic) DisplayName() string { return "generic (env vars)" }

// Detect always reports not-installed so generic is excluded from auto-detect
// and --all; it only runs when named explicitly.
func (generic) Detect() (string, bool) { return "", false }

func (generic) Apply(ep Endpoint, _ []Model, opts ApplyOptions) (Result, error) {
	model := ""
	if opts.DefaultModel != "" {
		model = "\n# Pass a model id, e.g.  --model " + opts.DefaultModel
	}
	snippet := fmt.Sprintf(`# zs-proxy — OpenAI-compatible endpoint (%s)
export OPENAI_BASE_URL=%s
export OPENAI_API_KEY=%s
# Some tools read OPENAI_API_BASE instead of OPENAI_BASE_URL:
export OPENAI_API_BASE=%s%s`, ep.Network, ep.BaseURL, ep.Key(), ep.BaseURL, model)

	return Result{
		Rendered: snippet,
		Hint:     "Add these to your shell profile (e.g. ~/.zshrc) or run them in your current shell.",
	}, nil
}
