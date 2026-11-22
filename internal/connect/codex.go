/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"os"
	"os/exec"
	"path/filepath"
)

// legacyCodexEnvKey is what connect used to write into the provider's env_key.
// Codex treats env_key as optional (a provider that sets neither env_key nor
// requires_openai_auth is treated as needing no authentication — `codex doctor`
// reports "OpenAI auth is not required for the active model provider") and the
// proxy admits via the on-chain seal, not a key — so we now omit the field
// entirely and prune this line off configs we previously wrote. Without the
// prune, a re-run would leave the stale key and Codex would keep erroring on the
// unset env var.
const legacyCodexEnvKey = "ZEROSIGNAL_API_KEY"

// codex configures the OpenAI Codex CLI (github.com/openai/codex).
//
// Config is TOML at ~/.codex/config.toml (relocatable via CODEX_HOME). A custom
// provider lives under [model_providers.<id>] with base_url + wire_api; the
// active provider/model are the top-level model_provider / model. We write no
// env_key — see legacyCodexEnvKey — so Codex sends no Authorization header, which
// is what the proxy wants anyway (it discards client auth).
//
// Codex is Responses-API only now (the chat wire API was removed in Feb 2026), so
// wire_api MUST be "responses" — the proxy serves POST /v1/responses, so this
// works. "responses" is also the default when the field is omitted; we keep
// writing it explicitly so a config produced here still says the right thing to
// an older Codex.
//
// Three constraints shape everything below, all verified against Codex 0.146:
//
//   - Everything goes in the *user-level* config. Codex ignores model_provider,
//     model_providers and model_catalog_json when it finds them in a
//     project-local .codex/config.toml.
//   - Profile files (~/.codex/<name>.config.toml + --profile) would scope our
//     settings cleanly, but they are CLI-only: the desktop app cannot select a
//     profile, and exposing them in the model pickers is closed as not-planned
//     upstream. So the global config is the only thing that works for everyone.
//   - The provider block alone tells Codex nothing about the model. Context
//     window, modalities and reasoning levels come from Codex's *model catalog*,
//     which only ever contains OpenAI slugs — see codexModelKeys.
//
// Deliberately NOT written: model_catalog_json. It looks like the right fix for
// the third point — it is the only way to give a model a real modality set and
// reasoning ladder — but every catalog entry must carry a non-null
// `base_instructions` string, and Codex offers no "use your default" value for
// it (an explicit null is a parse error). Synthesizing a catalog therefore means
// either shipping an empty agent prompt, which is strictly worse than the
// fallback Codex already applies to an unknown model, or copying OpenAI's own
// per-model Codex prompt out of the installed binary. Neither is acceptable, so
// we set model_context_window instead and leave the instructions alone. Verify
// against `codex debug models` before revisiting.
type codex struct{}

func (codex) ID() string          { return "codex" }
func (codex) DisplayName() string { return "Codex CLI" }

// codexHome is the directory holding config.toml: $CODEX_HOME or ~/.codex.
func (codex) codexHome() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home, nil
	}
	return homeJoin(".codex")
}

func (c codex) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := c.codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

func (c codex) Detect() (string, bool) {
	path, err := c.resolvePath("")
	if err != nil {
		return "", false
	}
	// The directory probe has to come from the same resolution as the file, or a
	// relocated CODEX_HOME reports not-installed while we happily write into it.
	dir, _ := c.codexHome()
	installed := fileExists(path) || dirExists(dir)
	// Best-effort: the desktop app ships its CLI inside the application bundle
	// (…/ChatGPT.app/Contents/Resources/codex) and does not put it on PATH, so a
	// LookPath miss proves nothing — the directory probe above is what carries
	// detection there.
	if _, err := exec.LookPath("codex"); err == nil {
		installed = true
	}
	return path, installed
}

func (c codex) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	path, err := c.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}

	provider := map[string]any{
		"name":     ProviderDisplay,
		"base_url": ep.BaseURL,
		"wire_api": "responses",
	}
	add := map[string]any{
		"model_provider":  ProviderID,
		"model_providers": map[string]any{ProviderID: provider},
	}
	prunes := []mergePrune{{
		Path:      []string{"model_providers", ProviderID, "env_key"},
		OnlyValue: legacyCodexEnvKey,
	}}
	if opts.DefaultModel != "" {
		add["model"] = opts.DefaultModel
	}
	prunes = append(prunes, codexModelKeys(models, opts.DefaultModel, add)...)

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	merged, err := mergeTOML(existing, add, prunes...)
	if err != nil {
		return Result{}, err
	}

	if opts.Print {
		return Result{Path: path, Rendered: string(merged)}, nil
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Wrote: true}, nil
}

// codexModelKeys fills in the top-level model metadata Codex would otherwise
// take from its model catalog, returning the prunes needed to keep a re-apply
// honest.
//
// Codex resolves the active `model` against a catalog that only ever holds
// OpenAI slugs, so a ZeroSignal model matches nothing and silently falls back to
// a generic profile — wrong context window, wrong truncation policy, no
// model-specific instructions. model_context_window is the documented top-level
// escape hatch for exactly that, and unlike model_catalog_json it *adds* to
// Codex's knowledge instead of replacing its model list.
//
// These keys are global, not per-provider, so they are only written alongside
// `model` and only when the proxy actually advertised capability data for the
// selected model. When it did not, they are left completely alone — including
// the prunes: a value we can't ground is worse than Codex's own default,
// deleting one we can't ground is no better, and silently retargeting a key the
// user may have set for a different provider is not ours to do.
func codexModelKeys(models []Model, selected string, add map[string]any) []mergePrune {
	m, ok := findModel(models, selected)
	// A model the proxy merely *named* is the same amount of evidence as one it
	// never mentioned, so it takes the same untouched path — see
	// hasAdvertisedMetadata for the two states that produce it. Without this,
	// re-running connect against a cold fleet silently deleted a correct
	// model_context_window, which is the exact failure this function exists to
	// prevent.
	if !ok || !hasAdvertisedMetadata(m) {
		return nil
	}
	var prunes []mergePrune
	if m.ContextWindow > 0 {
		add["model_context_window"] = m.ContextWindow
	} else {
		// Switching to a model with no advertised window must clear the previous
		// model's number rather than silently keep applying it.
		prunes = append(prunes, mergePrune{Path: []string{"model_context_window"}})
	}
	// Model.Reasoning is "advertised", not "supported" — false also covers
	// unadvertised, which is why the house rule is that it may only ever turn a
	// reasoning feature ON, never suppress one. So we never write `false` here;
	// we assert true when it is advertised and otherwise remove the key, which
	// hands the decision back to Codex.
	if m.Reasoning {
		add["model_supports_reasoning_summaries"] = true
	} else {
		prunes = append(prunes, mergePrune{Path: []string{"model_supports_reasoning_summaries"}})
	}
	return prunes
}
