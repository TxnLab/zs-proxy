/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/cli/service"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// This file holds the service-path branches of the `proxy` lifecycle commands.
// When a launchd/systemd/SCM service is installed and no self-managed daemon is
// running, start/stop/restart/status/logs target that service via the OS manager.
// A live daemon takes precedence when both exist; lifecycle_target.go owns that
// decision. The daemon-path branches live in daemon.go.

// startInstalledService is the `proxy start` branch when a service is installed:
// start the OS service instead of spawning a self-managed daemon that would just
// fight it for the port. A file-backend wallet can't be unlocked unattended, so
// (mirroring the install-time guard) we don't auto-start it into a crash loop.
func startInstalledService(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	warnIgnoredServiceFlags(cmd)
	if st, err := service.Status(); err == nil && st.Running {
		fmt.Fprintf(out, "proxy is already running as an OS service (pid %d)\n", st.PID)
		fmt.Fprintln(out, "  manage it: zs-proxy proxy stop|status|restart|logs")
		return nil
	}
	if wallet.UsesFileBackend() {
		ew := cmd.ErrOrStderr()
		fmt.Fprintln(ew, "a zs-proxy service is installed, but your wallet uses the encrypted-file backend,")
		fmt.Fprintln(ew, "which a background service can't unlock. Provide ZEROSIGNAL_KEYSTORE_PASSPHRASE to the")
		fmt.Fprintln(ew, "service environment, then start it:")
		fmt.Fprintf(ew, "    %s\n", service.EnableHint())
		return nil
	}
	if err := service.Start(); err != nil {
		return fmt.Errorf("start the installed service: %w", err)
	}
	fmt.Fprintln(out, "✔ started the installed zs-proxy service")
	fmt.Fprintln(out, "  manage it: zs-proxy proxy stop|status|restart|logs")
	return nil
}

// serviceStop is the `proxy stop` branch for an installed service. A Status
// error (couldn't determine liveness) does NOT short-circuit to "not running" —
// we attempt the stop anyway rather than leave a possibly-running service up.
func serviceStop(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if st, err := service.Status(); err == nil && !st.Running {
		fmt.Fprintln(out, "proxy service is not running")
		return nil
	}
	if err := service.Stop(); err != nil {
		return fmt.Errorf("stop the service: %w", err)
	}
	fmt.Fprintln(out, "✔ proxy service stopped")
	return nil
}

// serviceRestart is the `proxy restart` branch for an installed service. If it's
// confirmed not running (e.g. macOS bootout, where kickstart can't act on an
// unloaded job) it bootstraps it fresh; otherwise (running, or status unknown)
// it restarts in place.
func serviceRestart(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	warnIgnoredServiceFlags(cmd)
	if st, err := service.Status(); err == nil && !st.Running {
		if err := service.Start(); err != nil {
			return fmt.Errorf("start the service: %w", err)
		}
		fmt.Fprintln(out, "✔ proxy service started")
		return nil
	}
	if err := service.Restart(); err != nil {
		return fmt.Errorf("restart the service: %w", err)
	}
	fmt.Fprintln(out, "✔ proxy service restarted")
	return nil
}

// serviceStatus is the `proxy status` branch for an installed service: liveness
// from the OS service manager plus a best-effort /healthz probe on the listen
// address the current flags resolve to (the service may have been installed with
// other flags, so the probe is labeled best-effort).
func serviceStatus(cmd *cobra.Command, f *proxyFlags) error {
	out := cmd.OutOrStdout()
	st, err := service.Status()
	if err != nil {
		return err
	}
	logDisplay := serviceLogDisplay()
	if !st.Running {
		fmt.Fprintln(out, "proxy: not running (OS service installed)")
		fmt.Fprintf(out, "  manager: %s\n", managerName())
		if logDisplay != "" {
			fmt.Fprintf(out, "  logs:    %s\n", logDisplay)
		}
		fmt.Fprintln(out, "  start:   zs-proxy proxy start")
		return nil
	}
	fmt.Fprintln(out, "proxy: running (OS service)")
	if st.PID > 0 {
		fmt.Fprintf(out, "  pid:     %d\n", st.PID)
	}
	fmt.Fprintf(out, "  manager: %s\n", managerName())
	if logDisplay != "" {
		fmt.Fprintf(out, "  logs:    %s\n", logDisplay)
	}
	if cfg, cerr := f.resolveConfig(); cerr == nil {
		listen := cfg.Server.Listen
		switch {
		case probeHealthz(cmd.Context(), listen):
			fmt.Fprintf(out, "  health:  ok (%s/v1)\n", baseURL(listen))
		case len(changedFlags(cmd, listenAffectingFlags)) > 0:
			// The user explicitly pointed a flag at a listen address — a failed
			// probe is a meaningful signal, so report it.
			fmt.Fprintf(out, "  health:  not responding on %s\n", baseURL(listen))
		default:
			// Otherwise we're probing a guessed default address; a failed probe
			// against the authoritative "running" line above is just noise — omit
			// it. Pass --config/--port/--network to probe a specific address.
		}
	}
	return nil
}

// serviceLogs is the `proxy logs` branch for an installed service: tail the log
// file (macOS/Windows) or stream the systemd journal (Linux).
func serviceLogs(cmd *cobra.Command, lines int, follow bool) error {
	src, err := service.ServiceLogSource()
	if err != nil {
		return err
	}
	if src.File != "" {
		return tailLog(cmd, src.File, lines, follow)
	}
	if len(src.Command) > 0 {
		return streamLogCommand(cmd, src.Command, lines, follow)
	}
	// Windows: the SCM service writes the same proxy.log the daemon uses.
	p, err := daemonLogPath()
	if err != nil {
		return err
	}
	return tailLog(cmd, p, lines, follow)
}

// streamLogCommand runs a log-streaming command (Linux journalctl), appending
// -n <lines> and, when following, -f, and streams its output until the command
// exits or the user interrupts (Ctrl-C).
func streamLogCommand(cmd *cobra.Command, base []string, lines int, follow bool) error {
	args := append([]string{}, base[1:]...)
	if lines > 0 {
		args = append(args, "-n", strconv.Itoa(lines))
	}
	if follow {
		args = append(args, "-f")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := exec.CommandContext(ctx, base[0], args...)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	err := c.Run()
	if err != nil && ctx.Err() != nil {
		return nil // killed by Ctrl-C — a clean stop, not a failure
	}
	return err
}

// serviceLogDisplay is the human-readable log location for an installed service:
// a file path, the journalctl command, or (Windows) the proxy.log fallback.
func serviceLogDisplay() string {
	src, err := service.ServiceLogSource()
	if err != nil {
		return ""
	}
	if src.File != "" {
		return src.File
	}
	if len(src.Command) > 0 {
		return strings.Join(src.Command, " ")
	}
	if p, err := daemonLogPath(); err == nil {
		return p
	}
	return ""
}

// managerName names the OS service manager backing the installed service.
func managerName() string { return managerNameFor(runtime.GOOS) }

func managerNameFor(goos string) string {
	switch goos {
	case "darwin":
		return "launchd"
	case "linux":
		return "systemd (user)"
	case "windows":
		return "Windows SCM"
	default:
		return goos
	}
}

// listenAffectingFlags are the start-flag names that change which config / listen
// address the proxy would use. An installed service bakes these in at install
// time, so passing them to start/restart can't take effect — they're meaningful
// only at install (or, for status, to point the health probe at a specific
// address).
var listenAffectingFlags = []string{"config", "network", "port"}

// changedFlags returns the subset of names the user explicitly set on cmd.
func changedFlags(cmd *cobra.Command, names []string) []string {
	var out []string
	for _, n := range names {
		if fl := cmd.Flags().Lookup(n); fl != nil && fl.Changed {
			out = append(out, n)
		}
	}
	return out
}

// warnIgnoredServiceFlags warns when the user passed config-changing flags that
// an installed service can't honor (its flags are fixed at install time) — so a
// `restart --network mainnet` doesn't silently report success while leaving the
// service on its original network.
func warnIgnoredServiceFlags(cmd *cobra.Command) {
	for _, n := range changedFlags(cmd, listenAffectingFlags) {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"⚠ --%s is ignored for an installed service (its flags are fixed at install time); "+
				"re-run `zs-proxy proxy install-service` to change them\n", n)
	}
}
