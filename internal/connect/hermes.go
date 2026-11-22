/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// hermesPluginInstallArgs installs the ZeroSignal plugin by its catalog name,
// which pins the reviewed commit rather than tracking a branch. `--enable`
// answers the one prompt this command would otherwise stop on.
var hermesPluginInstallArgs = []string{"plugins", "install", ProviderID, "--enable"}

// runHermes runs the hermes CLI with the terminal passed straight through.
//
// Not because a prompt is expected: for this exact command Hermes asks nothing
// (the catalog install passes its own reviewed pin, so the security scan
// auto-accepts; the pinned plugin.yaml declares neither requires_env nor
// capabilities; and --enable covers the last one). It is that the installer
// clones and may pip-install, so its progress and any failure are the user's to
// watch, and Ctrl-C has to reach it. A prompt added upstream then works by
// default instead of deadlocking against a pipe.
//
// A package variable so tests can observe the call without a Hermes install.
var runHermes = func(ctx context.Context, args ...string) error {
	bin, err := exec.LookPath("hermes")
	if err != nil {
		return errHermesNotOnPath
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = scrubbedEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

var errHermesNotOnPath = errors.New("`hermes` is not on PATH")

// scrubbedEnv is the environment the installer inherits, minus this process's
// signing material. It is the only place zs-proxy runs third-party code, the
// installer's own step runs whatever the plugin declares as dependencies, and
// the pin it installs is advanced by pull requests in a repository we don't
// control — so the wallet's secrets have no business being in reach, however
// small the marginal exposure over the user running the same command by hand.
// Everything else is passed through untouched: HERMES_HOME, PATH and the proxy
// settings all have to survive for the install to work at all.
func scrubbedEnv() []string {
	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if name == "ZEROSIGNAL_KEYSTORE_PASSPHRASE" || name == "ZS_MNEMONIC_URLS" || strings.HasSuffix(name, "_MNEMONIC") {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// hermesEnvAPIKey and hermesEnvBaseURL are the env_vars the standalone
// ZeroSignal Hermes plugin (TxnLab/hermes-zerosignal-provider) declares: a key
// any non-empty value satisfies, and the base-URL override Hermes reads off a
// profile's trailing *_BASE_URL entry.
const (
	hermesEnvAPIKey  = "ZEROSIGNAL_API_KEY"
	hermesEnvBaseURL = "ZEROSIGNAL_BASE_URL"
)

// legacyHermesTypeValue is what connect used to write into the provider block's
// `type`. Hermes has never read that key — it warns "unknown config keys
// ignored: type" on every launch — so we now write `transport` instead and prune
// this line off blocks we previously wrote. The prune matches only this value so
// a `type` the user set by hand survives. Without the prune, mergeYAML's
// add-only merge would leave the stale key, and the warning, forever.
const legacyHermesTypeValue = "openai"

// hermes configures the Hermes agent (Nous Research).
//
// Config is YAML at ~/.hermes/config.yaml (relocatable via HERMES_HOME). A
// named OpenAI-compatible provider lives under "providers.<name>" with
// base_url / api_key / transport / default_model; the active provider is
// selected under "model". The wire key is `transport` — it is what Hermes' own
// config migration writes (`api_mode` is the older spelling it still reads) —
// not the `type` an earlier version wrote (see legacyHermesTypeValue).
//
// It writes two files because Hermes has two provider paths and consults only
// one of them at a time. A `providers.<name>` block is honored only while no
// registered provider profile carries that name
// (runtime_provider_custom._shadowed_by_builtin, verified at hermes-agent
// 8c8003f). The standalone ZeroSignal plugin registers a profile named
// "zerosignal", so once it is installed Hermes ignores this block and resolves
// the provider from ZEROSIGNAL_API_KEY / ZEROSIGNAL_BASE_URL in ~/.hermes/.env
// instead. Writing the block AND those lines is what lets one `connect hermes`
// be right whether the plugin is installed before it, after it, or never. The
// block keeps its inline placeholder api_key so it stays self-contained on the
// no-plugin path.
type hermes struct{}

func (hermes) ID() string          { return "hermes" }
func (hermes) DisplayName() string { return "Hermes" }

// hermesHome is Hermes' home directory, resolved the way Hermes does
// (hermes_constants.get_hermes_home): $HERMES_HOME, else the platform default,
// which on Windows is %LOCALAPPDATA%\hermes rather than ~/.hermes.
func (hermes) hermesHome() (string, error) {
	if home := os.Getenv("HERMES_HOME"); home != "" {
		return home, nil
	}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "hermes"), nil
		}
		return homeJoin("AppData", "Local", "hermes")
	}
	return homeJoin(".hermes")
}

func (h hermes) resolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := h.hermesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

// envPath is config.yaml's sibling, so a --config-path override keeps both
// files together (which is also what lets the tests target a temp dir).
func (hermes) envPath(confPath string) string {
	return filepath.Join(filepath.Dir(confPath), ".env")
}

func (h hermes) Detect() (string, bool) {
	path, err := h.resolvePath("")
	if err != nil {
		return "", false
	}
	// Same resolution as the file, or a relocated HERMES_HOME reports
	// not-installed while we happily write into it.
	dir, _ := h.hermesHome()
	installed := fileExists(path) || dirExists(dir)
	if _, err := exec.LookPath("hermes"); err == nil {
		installed = true
	}
	return path, installed
}

func (h hermes) Apply(ep Endpoint, models []Model, opts ApplyOptions) (Result, error) {
	// The CLI refuses this pair, but the refusal is a package away and this is
	// where the incoherence would bite: the two files would go to the override
	// while the installer and the already-installed check both read the real
	// HERMES_HOME, leaving a plugin configured by neither file.
	if opts.InstallPlugin && opts.ConfigPathOverride != "" {
		return Result{}, errors.New("installing the Hermes plugin is incompatible with a config-path override: the plugin reads its own Hermes home (set HERMES_HOME to relocate both)")
	}
	path, err := h.resolvePath(opts.ConfigPathOverride)
	if err != nil {
		return Result{}, err
	}
	envPath := h.envPath(path)

	provider := map[string]any{
		"base_url":  ep.BaseURL,
		"api_key":   ep.Key(),
		"transport": "chat_completions",
	}
	add := map[string]any{
		"providers": map[string]any{ProviderID: provider},
	}
	if opts.DefaultModel != "" {
		provider["default_model"] = opts.DefaultModel
		add["model"] = map[string]any{"provider": ProviderID, "default": opts.DefaultModel}
	} else {
		add["model"] = map[string]any{"provider": ProviderID}
	}

	existing, err := readFileOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	merged, err := mergeYAML(existing, add, mergePrune{
		Path:      []string{"providers", ProviderID, "type"},
		OnlyValue: legacyHermesTypeValue,
	})
	if err != nil {
		return Result{}, err
	}

	sets := []envSet{{Key: hermesEnvAPIKey, Value: ep.Key()}, {Key: hermesEnvBaseURL, Value: ep.BaseURL}}
	existingEnv, err := readFileOrEmpty(envPath)
	if err != nil {
		return Result{}, err
	}
	env := mergeDotenv(existingEnv, sets)

	hint := fmt.Sprintf("Run `hermes model` to confirm the active provider %q. With the ZeroSignal Hermes plugin installed, Hermes uses the plugin instead of this block, and the live endpoint is ZEROSIGNAL_BASE_URL in %s.", ProviderID, envPath)
	res := Result{Path: path, Hint: hint}

	if opts.Print {
		res.Rendered = string(merged)
		res.Extra = []Written{{Path: envPath, Rendered: string(redactEnv(env, sets))}}
		return res, nil
	}
	// .env goes first so a .env we cannot write fails before the provider block
	// is installed: the block alone is the half-state where the plugin path, if
	// present, finds no variables.
	//
	// No .env.bak, unlike every other file connect writes: a line splice has
	// nothing to lose (the same reasoning as config.SetScalar), Hermes' own
	// writer keeps none, and a backup is a second copy of the secrets file —
	// one that would not inherit a mode the user tightened later.
	if !bytes.Equal(env, existingEnv) {
		target, err := envWriteTarget(envPath)
		if err != nil {
			return Result{}, err
		}
		if err := atomicWrite(target, env, envFileMode(target)); err != nil {
			return Result{}, err
		}
	}
	if err := atomicWriteWithBackup(path, merged, 0o644); err != nil {
		return Result{}, err
	}
	res.Extra = []Written{{Path: envPath}}
	res.Wrote = true

	if !opts.InstallPlugin {
		// Only when there is something to install. Re-running connect is routine
		// (a moved proxy, a new default model), and telling someone who already
		// has the plugin to install it teaches them to disregard the hint — the
		// command would fail with Hermes' "already exists" if they followed it.
		if !h.pluginInstalled() {
			res.Hint += fmt.Sprintf(" To install it: `%s`.", hermesPluginInstallCommand())
		}
		return res, nil
	}
	// Re-running connect is the documented way to re-point Hermes at a moved
	// proxy, and Hermes refuses a second install ("already exists") rather than
	// treating it as done — so an installed plugin is success, not an error.
	if h.pluginInstalled() {
		res.Hint = fmt.Sprintf("ZeroSignal plugin already installed. Run `hermes model` to confirm the active provider %q; the live endpoint is ZEROSIGNAL_BASE_URL in %s.", ProviderID, envPath)
		return res, nil
	}
	// After both writes, never before, for two reasons that outlive each other.
	//
	// The durable one: a failed install then leaves a working config behind —
	// the provider block is what Hermes uses while no plugin is registered — so
	// nothing here is rolled back on the error path below.
	//
	// The contingent one: Hermes prompts for every requires_env variable a
	// plugin declares and .env lacks, and a prompt would be answered by whoever
	// ran connect rather than by the value we just chose. Today it cannot fire
	// for this plugin — the prompt reads the INSTALLED plugin.yaml
	// (_prompt_plugin_env_vars → _missing_env_specs → _read_manifest), which
	// declares no requires_env; only the catalog entry does, and
	// install_catalog_entry does not merge it in. That is a fact about the
	// plugin's current release, not a guarantee, and the catalog entry shows the
	// intent — so the ordering stays cheap insurance rather than something to
	// re-derive when it starts mattering.
	if err := runHermes(context.Background(), hermesPluginInstallArgs...); err != nil {
		// Retry through connect, not through the bare install command: a failure
		// late enough to have moved the plugin into place (a dependency install,
		// or a Ctrl-C after the swap) answers the bare command with Hermes'
		// "already exists", while connect skips and succeeds.
		return Result{}, fmt.Errorf("configured Hermes (%s, %s), but installing the plugin failed: %w — retry with `zs-proxy connect hermes --plugin`",
			path, envPath, err)
	}
	res.Hint = fmt.Sprintf("ZeroSignal plugin installed and enabled. Run `hermes model` to confirm the active provider %q; the live endpoint is ZEROSIGNAL_BASE_URL in %s.", ProviderID, envPath)
	return res, nil
}

// pluginInstalled reports whether a ZeroSignal plugin directory exists in either
// place Hermes discovers model-provider plugins from: plugins/<name>/, where
// `hermes plugins install` clones, and plugins/model-providers/<name>/, where
// the plugin's README has a manual clone go.
func (h hermes) pluginInstalled() bool {
	home, err := h.hermesHome()
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(home, "plugins", ProviderID)) ||
		dirExists(filepath.Join(home, "plugins", "model-providers", ProviderID))
}

func hermesPluginInstallCommand() string {
	return "hermes " + strings.Join(hermesPluginInstallArgs, " ")
}

// envFileMode is the mode ~/.hermes/.env is written with: an existing file
// keeps its own (Hermes preserves a mode it was given, e.g. 0640 for a Docker
// volume), a new one is 0600 — it is Hermes' secrets file, whatever our two
// values are.
func envFileMode(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o600
}

// envWriteTarget is the file the .env write lands on: a symlinked .env (into a
// dotfiles repo or a secrets directory) is resolved so the rename replaces the
// real file and the link survives — which is what Hermes' own writer does
// (utils.atomic_replace). Renaming onto the link itself would swap it for a
// detached regular file that later edits to the real one never reach.
func envWriteTarget(envPath string) (string, error) {
	fi, err := os.Lstat(envPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return envPath, nil
	}
	target, err := filepath.EvalSymlinks(envPath)
	if err != nil {
		return "", fmt.Errorf("resolve symlinked %s: %w", envPath, err)
	}
	return target, nil
}
