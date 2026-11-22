/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/TxnLab/zerosignal/go/wallet"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/connect"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// newConnectCmd is `zs-proxy connect`: autoconfigure third-party AI tools to
// talk to the running proxy. With no argument it detects installed tools and
// configures them interactively; `connect <tool>` targets one; `--all` does
// every detected tool. The proxy must be running so the live endpoint + model
// list can be read.
func newConnectCmd() *cobra.Command {
	var (
		f          proxyFlags
		all        bool
		listOnly   bool
		printOnly  bool
		assumeYes  bool
		modelFlag  string
		urlFlag    string
		configPath string
		withPlugin bool
	)
	cmd := &cobra.Command{
		Use:   "connect [tool]",
		Short: "Point AI tools (opencode, hermes, openclaw, aider, continue, codex…) at the proxy",
		Long: "Autoconfigure third-party AI tools to use the running proxy as their OpenAI-compatible\n" +
			"endpoint. With no argument, detects installed tools and configures them. Name a tool to\n" +
			"configure just that one, or use --all. Known tools: " + strings.Join(connect.IDs(), ", ") + ".",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			// Ahead of every other branch, --list included: a flag that is refused
			// in all but one combination must not be silently ignored in the one
			// branch that returns early.
			if withPlugin {
				if err := validatePluginFlag(args, all, listOnly, printOnly, configPath); err != nil {
					return err
				}
			}

			// --list is pure detection — it needs neither a running proxy nor a
			// model list, so handle it before resolving the endpoint.
			if listOnly {
				printDetection(cmd)
				return nil
			}
			if err := refuseConnectInOtherHome(printOnly, configPath); err != nil {
				return err
			}

			// 1. Resolve the live endpoint (or the --url override).
			ep, err := resolveProxyEndpoint(cmd, &f, urlFlag)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Proxy endpoint: %s", ep.BaseURL)
			if ep.Network != "" {
				fmt.Fprintf(out, "  (%s)", strings.ToUpper(ep.Network))
			}
			fmt.Fprintln(out)

			// 2. Fetch the advertised models (best-effort) and keep only those
			// usable by text chat / coding tools (text in, at least text out).
			models, ferr := connect.FetchModels(cmd.Context(), ep.BaseURL)
			if ferr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not list models (%v) — writing provider without a model list\n", ferr)
				models = nil
			} else {
				raw := len(models)
				models = connect.ChatModels(models)
				switch {
				case raw == 0:
					fmt.Fprintln(cmd.ErrOrStderr(), "warning: proxy advertised no models — writing provider without a model list")
				case len(models) == 0:
					fmt.Fprintln(cmd.ErrOrStderr(), "warning: proxy advertised no text-capable models — writing provider without a model list")
				}
			}
			// One ordering for the menu, the number the user types, and every
			// generated config — the registry's order is arbitrary.
			connect.SortModels(models)

			// 3. Select target integrations.
			targets, listed, err := selectTargets(cmd, args, all, assumeYes, configPath)
			if err != nil {
				return err
			}
			if listed || len(targets) == 0 {
				return nil
			}

			// 4. Resolve the default model to set (flag → interactive pick → heuristic).
			defaultModel := resolveDefaultModel(cmd, modelFlag, models, assumeYes, printOnly)

			// 5. Apply each integration.
			return applyTargets(cmd, targets, ep, models, connect.ApplyOptions{
				Print:              printOnly,
				DefaultModel:       defaultModel,
				ConfigPathOverride: configPath,
				InstallPlugin:      withPlugin,
			})
		},
	}
	f.bindCommon(cmd) // --config, --network — for the installed-service endpoint resolution
	cmd.Flags().BoolVar(&all, "all", false, "configure every detected tool (non-interactive)")
	cmd.Flags().BoolVar(&listOnly, "list", false, "list detected tools and their config paths, then exit")
	// "tool config" rather than "any files": resolving the live endpoint runs the
	// shared lifecycle-target resolver, which self-heals an unusable daemon record
	// on every command. --print suppresses the writes this command exists to make,
	// not that housekeeping.
	cmd.Flags().BoolVar(&printOnly, "print", false, "render the resulting config to stdout without writing any tool config files")
	cmd.Flags().BoolVar(&printOnly, "dry-run", false, "alias for --print")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip confirmation prompts")
	cmd.Flags().StringVar(&modelFlag, "model", "", "default model id to set in each tool")
	cmd.Flags().StringVar(&urlFlag, "url", "", "advanced: target this OpenAI base URL instead of the local proxy (skips the running check)")
	cmd.Flags().StringVar(&configPath, "config-path", "", "override the target config file (only with a single named tool)")
	cmd.Flags().BoolVar(&withPlugin, "plugin", false, "hermes only: also install the ZeroSignal Hermes plugin (runs `hermes plugins install`)")
	return cmd
}

// pluginTool is the one integration that ships a plugin of its own. It is
// compared against a resolved Integration.ID() rather than the raw argument, so
// a rename in the registry shows up as every --plugin run being refused, which
// is what TestConnect_PluginFlagReachesApply catches.
const pluginTool = "hermes"

// refuseConnectInOtherHome stops connect from another home writing the
// tools' own config files, which are the user's and not the home's: a test
// proxy's run would point their everyday tools at it. --print writes nothing,
// and --config-path names the one file to write.
func refuseConnectInOtherHome(printOnly bool, configPath string) error {
	if paths.OtherHome() && !printOnly && configPath == "" {
		return fmt.Errorf("%s is set: connect would point your tools' own configs at this home's proxy; pass --print to see the config, or --config-path to write one file", wallet.HomeEnv)
	}
	return nil
}

func unknownToolError(name string) error {
	return fmt.Errorf("unknown tool %q — known tools: %s", name, strings.Join(connect.IDs(), ", "))
}

// validatePluginFlag refuses every --plugin combination that cannot mean what
// it says. The install runs a third party's code installer, so it is only ever
// done for a tool the user named — never as a side effect of --all or of
// detection — and never where its result would disagree with the files written:
// --print writes nothing, and --config-path moves config.yaml while the
// installer still targets Hermes' real home.
func validatePluginFlag(args []string, all, listOnly, printOnly bool, configPath string) error {
	named := ""
	if len(args) == 1 {
		in, ok := connect.Lookup(args[0])
		if !ok {
			// A typo is an unknown tool, not a --plugin misuse. Reporting the
			// flag here would send someone who typed `connect Hermes --plugin`
			// looking at the flag rather than at the name.
			return unknownToolError(args[0])
		}
		named = in.ID()
	}
	switch {
	case all || named != pluginTool:
		return fmt.Errorf("--plugin only applies to `zs-proxy connect %s`", pluginTool)
	case listOnly:
		return fmt.Errorf("--plugin can't be combined with --list, which only prints detection")
	case printOnly:
		return fmt.Errorf("--plugin can't be combined with --print: installing a plugin can't be previewed")
	case configPath != "":
		return fmt.Errorf("--plugin can't be combined with --config-path: the plugin installs into Hermes' own home (set HERMES_HOME to relocate both)")
	}
	return nil
}

// resolveProxyEndpoint returns the running proxy's OpenAI endpoint, for
// `connect` to point tools at and `models` to read. A --url override is taken
// verbatim (no running check); otherwise the live listen address is read from
// the daemon record (or resolved config when an OS service is installed) and
// confirmed reachable via /healthz.
func resolveProxyEndpoint(cmd *cobra.Command, f *proxyFlags, urlFlag string) (connect.Endpoint, error) {
	if urlFlag != "" {
		return connect.Endpoint{BaseURL: strings.TrimRight(urlFlag, "/"), APIKey: connect.PlaceholderAPIKey}, nil
	}

	listen, network, reachable, err := liveProxyListen(cmd, f)
	if err != nil {
		return connect.Endpoint{}, err
	}
	if listen == "" {
		return connect.Endpoint{}, fmt.Errorf("proxy isn't running — start it with `zs-proxy proxy start`, then re-run `%s`", cmd.CommandPath())
	}
	if !reachable {
		return connect.Endpoint{}, fmt.Errorf("proxy is registered on %s but not answering /healthz — check `zs-proxy proxy status` / `zs-proxy proxy logs`", baseURL(listen))
	}
	return connect.Endpoint{
		BaseURL: baseURL(listen) + "/v1",
		APIKey:  connect.PlaceholderAPIKey,
		Network: network,
	}, nil
}

// liveProxyListen reports the live listen address + network and whether the
// proxy answers /healthz. It uses the same ownership resolution as the
// lifecycle commands, so a live daemon wins when a service is also installed.
// listen is "" when nothing runs.
func liveProxyListen(cmd *cobra.Command, f *proxyFlags) (listen, network string, reachable bool, err error) {
	target := resolveTarget()
	announceTargetPrecedence(cmd, target)
	if target.kind == targetService {
		cfg, cerr := f.resolveConfig()
		if cerr != nil {
			return "", "", false, cerr
		}
		return cfg.Server.Listen, cfg.NetworkName(), probeHealthz(cmd.Context(), cfg.Server.Listen), nil
	}
	if target.kind != targetDaemon {
		return "", "", false, nil
	}
	rec := target.daemon
	return rec.Listen, rec.Network, probeHealthz(cmd.Context(), rec.Listen), nil
}

// selectTargets resolves which integrations to configure. It returns listed=true
// when it only printed a detection table (no work to do).
func selectTargets(cmd *cobra.Command, args []string, all, assumeYes bool, configPath string) (targets []connect.Integration, listed bool, err error) {
	out := cmd.OutOrStdout()

	// Explicit single tool: run it regardless of detection.
	if len(args) == 1 {
		in, ok := connect.Lookup(args[0])
		if !ok {
			return nil, false, unknownToolError(args[0])
		}
		return []connect.Integration{in}, false, nil
	}
	if configPath != "" {
		return nil, false, fmt.Errorf("--config-path only applies when configuring a single named tool")
	}

	detected := detectInstalled()
	if all {
		if len(detected) == 0 {
			fmt.Fprintln(out, "No supported tools detected.")
		}
		return detected, false, nil
	}

	// No argument: interactive detect → confirm.
	if len(detected) == 0 {
		fmt.Fprintln(out, "No supported tools detected on this machine.")
		fmt.Fprintf(out, "Run `zs-proxy connect <tool>` to configure one anyway (known: %s),\n", strings.Join(connect.IDs(), ", "))
		fmt.Fprintln(out, "or `zs-proxy connect generic` to print the OpenAI env vars.")
		return nil, true, nil
	}
	printDetection(cmd)
	if !assumeYes && !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(out, "\nNon-interactive: re-run with --all to configure the detected tools, or name one.")
		return nil, true, nil
	}
	names := make([]string, len(detected))
	for i, in := range detected {
		names[i] = in.DisplayName()
	}
	if !confirmYesNo(cmd, fmt.Sprintf("\nConfigure %s?", strings.Join(names, ", ")), true, assumeYes) {
		fmt.Fprintln(out, "Aborted.")
		return nil, true, nil
	}
	return detected, false, nil
}

// detectInstalled returns the integrations that appear installed (excludes the
// generic env-var fallback, which is never auto-detected).
func detectInstalled() []connect.Integration {
	var found []connect.Integration
	for _, in := range connect.All() {
		if _, installed := in.Detect(); installed {
			found = append(found, in)
		}
	}
	return found
}

// printDetection prints the detection table for every known tool.
func printDetection(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Detected tools:")
	for _, in := range connect.All() {
		path, installed := in.Detect()
		mark := " "
		if installed {
			mark = "x"
		}
		if path == "" {
			path = "(prints env vars)"
		}
		fmt.Fprintf(out, "  [%s] %-9s %s\n", mark, in.ID(), path)
	}
}

// resolveDefaultModel picks the default model to write: the --model flag if
// given (warns if absent from the advertised list), an interactive pick from the
// printed catalog when on a TTY, else the heuristic default. Empty when no
// models are available.
func resolveDefaultModel(cmd *cobra.Command, modelFlag string, models []connect.Model, assumeYes, printOnly bool) string {
	out := cmd.OutOrStdout()
	if modelFlag != "" {
		if len(models) > 0 && !connect.HasModel(models, modelFlag) {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: model %q isn't in the proxy's advertised list — setting it anyway\n", modelFlag)
		}
		return modelFlag
	}
	suggested := connect.DefaultModel(models)
	if suggested == "" {
		return "" // proxy advertised no models
	}
	// Non-interactive: take the suggested default, but say which one and how to
	// change it (skipped under --print, which only renders configs).
	if assumeYes || printOnly || !term.IsTerminal(int(os.Stdin.Fd())) {
		if !printOnly {
			fmt.Fprintf(out, "Default model: %s  (pass --model to choose another)\n", suggested)
		}
		return suggested
	}
	// Interactive: show the catalog and accept a number or a model id.
	printModelMenu(out, models, suggested)
	fmt.Fprintf(out, "Choose a model by number or id [%s]: ", suggested)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	choice := strings.TrimSpace(line)
	switch {
	case choice == "":
		return suggested
	case isModelIndex(choice, len(models)):
		n, _ := strconv.Atoi(choice)
		return models[n-1].ID
	default:
		if !connect.HasModel(models, choice) {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: model %q isn't in the advertised list — setting it anyway\n", choice)
		}
		return choice
	}
}

// isModelIndex reports whether s is a 1-based index into a list of n models.
func isModelIndex(s string, n int) bool {
	i, err := strconv.Atoi(s)
	return err == nil && i >= 1 && i <= n
}

// printModelMenu lists the advertised models with their context window and a
// vision marker, flagging the suggested default.
func printModelMenu(out io.Writer, models []connect.Model, suggested string) {
	fmt.Fprintf(out, "\nModels available from the proxy (%d):\n", len(models))
	for i, m := range models {
		meta := ""
		if m.ContextWindow > 0 {
			meta += fmt.Sprintf("  %dK ctx", m.ContextWindow/1024)
		}
		for _, mod := range m.InputModalities {
			if mod == "image" {
				meta += "  vision"
				break
			}
		}
		if m.ID == suggested {
			meta += "  ← default"
		}
		fmt.Fprintf(out, "  %2d. %-30s%s\n", i+1, m.ID, meta)
	}
}

// applyTargets runs Apply for each integration and prints the outcome.
func applyTargets(cmd *cobra.Command, targets []connect.Integration, ep connect.Endpoint, models []connect.Model, opts connect.ApplyOptions) error {
	out := cmd.OutOrStdout()
	var firstErr error
	for _, in := range targets {
		res, err := in.Apply(ep, models, opts)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  ✗ %s: %v\n", in.ID(), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch {
		case opts.Print:
			fmt.Fprintf(out, "\n# %s → %s\n%s\n", in.ID(), res.Path, res.Rendered)
			for _, ex := range res.Extra {
				fmt.Fprintf(out, "\n# %s → %s\n%s\n", in.ID(), ex.Path, ex.Rendered)
			}
		case res.Path == "": // stdout-only (generic)
			fmt.Fprintf(out, "\n# %s\n%s\n", in.ID(), res.Rendered)
		default:
			fmt.Fprintf(out, "  ✓ %-9s → %s\n", in.ID(), res.Path)
			// Continuation lines: same tool, additional files (pi's settings.json,
			// aider's model-metadata sidecar, hermes' .env).
			for _, ex := range res.Extra {
				fmt.Fprintf(out, "    %-9s → %s\n", "", ex.Path)
			}
		}
		if res.Hint != "" && !opts.Print {
			fmt.Fprintf(out, "      %s\n", res.Hint)
		}
	}
	return firstErr
}

// confirmYesNo asks a [Y/n] (default-yes) or [y/N] question, honoring assumeYes
// and non-TTY (which take the default).
func confirmYesNo(cmd *cobra.Command, prompt string, defaultYes, assumeYes bool) bool {
	if assumeYes || !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	fmt.Fprint(cmd.OutOrStdout(), prompt+suffix)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return defaultYes
	}
	return ans == "y" || ans == "yes"
}
