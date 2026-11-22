/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/daemon"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// healthWait bounds how long daemonize waits for the child to bind before it
// stops blocking. A fast failure (port in use, bad config, keystore decrypt
// error) trips the process-died check well inside this; a slow-but-healthy boot
// (a sluggish chain read in app.Run) is reported as "still starting" rather than
// blocking the caller for the full chain timeout.
const healthWait = 12 * time.Second

// envShutdownToken forwards the loopback control-channel token from the
// daemonizing parent to the detached serving child via the environment (passing
// it on argv would leak it into `ps`). The child reads it in serve(); empty in
// foreground / service-managed runs, which disables the POST /shutdown route.
const envShutdownToken = "ZEROSIGNAL_SHUTDOWN_TOKEN"

// daemonize backgrounds the proxy: it does the interactive setup (wallet
// bootstrap, funding preflight, keychain unlock) here on the parent's TTY —
// the detached child has no terminal — then re-execs itself with --foreground
// in a new session, confirms it bound, records the pidfile, and returns to the
// shell with the live URL.
func (f *proxyFlags) daemonize(cmd *cobra.Command) error {
	cfg, err := f.resolveConfig()
	if err != nil {
		return err
	}

	// Unlock the encrypted-file wallet here so the passphrase can be forwarded
	// to the TTY-less child via the inherited environment. Must run before
	// bootstrap so a first-run wallet is created under the same passphrase
	// (no second prompt). No-op for the OS-keychain backend (unattended-read).
	if err := ensureKeystorePassphrase(cmd); err != nil {
		return err
	}

	// Interactive first-run setup, identical to serve. Bootstrap must happen
	// before we detach (it can prompt import-vs-new); the funding check only
	// warns now (the daemon comes up unfunded and serves 402s).
	if err := bootstrapWallet(cmd, cfg); err != nil {
		return err
	}
	if err := warnIfUnfunded(cmd, cfg); err != nil {
		return err
	}

	// Validate the wallet actually loads before backgrounding, so a wrong
	// passphrase / denied keychain surfaces on the TTY instead of being buried
	// in the daemon log.
	if _, err := payerKeyOptions(cmd.Context(), cfg); err != nil {
		return keyringHint(err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	logPath, err := daemonLogPath()
	if err != nil {
		return err
	}

	// Generate the loopback control-channel token and forward it to the detached
	// child via the environment (Spawn inherits the parent env). It's recorded
	// below so `proxy stop` can present it — the Windows graceful-stop path (no
	// SIGTERM). On unix it's recorded but unused (stop uses SIGTERM).
	shutdownToken, err := daemon.NewShutdownToken()
	if err != nil {
		return err
	}
	if err := os.Setenv(envShutdownToken, shutdownToken); err != nil {
		return fmt.Errorf("set shutdown token env: %w", err)
	}

	pid, err := daemon.Spawn(exe, f.childArgs(), logPath)
	if err != nil {
		return err
	}

	// Note: the pidfile is written only after the healthz confirm below, so if
	// this parent is killed (Ctrl-C) during the poll window the child keeps
	// listening with no record — `proxy status` will show "not running" while
	// :9376 is held. The recovery is benign: the next `proxy start` spawns a
	// child that fails to bind and surfaces the port-in-use log tail.
	out := cmd.OutOrStdout()
	listen := cfg.Server.Listen
	healthy := waitForHealthy(cmd.Context(), pid, listen)

	if !daemon.Alive(pid) {
		// Died during startup — surface why instead of a phantom URL.
		tail, _ := daemon.LogTailLines(logPath, 15)
		msg := "daemon exited during startup"
		if len(tail) > 0 {
			msg += "\n--- last log lines (" + logPath + ") ---\n" + strings.Join(tail, "\n")
		} else {
			msg += " (see " + logPath + ")"
		}
		return errors.New(msg)
	}

	if err := daemon.Write(daemon.Record{
		PID:           pid,
		Listen:        listen,
		Network:       cfg.NetworkName(),
		NetworkFlag:   f.network,
		ConfigPath:    f.configPath,
		LogPath:       logPath,
		ExePath:       exe,
		ShutdownToken: shutdownToken,
		StartedAt:     time.Now(),
	}); err != nil {
		return fmt.Errorf("record daemon: %w", err)
	}

	fmt.Fprintf(out, "✔ proxy started (pid %d)\n", pid)
	if healthy {
		fmt.Fprintf(out, "→ %s/v1   (%s)\n", baseURL(listen), strings.ToUpper(cfg.NetworkName()))
	} else {
		// Alive but not yet answering — typically a slow initial chain read.
		fmt.Fprintf(out, "  still binding on %s (chain read may be slow) — `zs-proxy proxy status` / `zs-proxy proxy logs -f`\n", baseURL(listen))
	}
	fmt.Fprintln(out, "  logs: zs-proxy proxy logs -f     stop: zs-proxy proxy stop")
	return nil
}

// childArgs reconstructs the foreground invocation for the detached child,
// mirroring exactly the flags the user passed so the child resolves config
// identically (same --config/--network/--port precedence). It deliberately does
// NOT synthesize --network from the resolved value: forcing an explicit
// --network would override a self-hoster's config-supplied escrow_app_id.
func (f *proxyFlags) childArgs() []string {
	args := []string{"proxy", "start", "--foreground"}
	if f.configPath != "" {
		args = append(args, "--config", f.configPath)
	}
	if f.network != "" {
		args = append(args, "--network", f.network)
	}
	if f.port != 0 {
		args = append(args, "--port", strconv.Itoa(f.port))
	}
	if f.trackTimes {
		args = append(args, "--times")
	}
	if f.debugBodies {
		args = append(args, "--debug-bodies")
	}
	return args
}

// ensureKeystorePassphrase unlocks the encrypted-file wallet on the parent's
// TTY and forwards the passphrase to the child via the environment (the child
// is detached and can't prompt). No-op for the OS-keychain backend, which reads
// unattended.
func ensureKeystorePassphrase(cmd *cobra.Command) error {
	if !wallet.FileBackendNeedsPassphrase() {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("the encrypted-file wallet needs a passphrase but stdin isn't a terminal — " +
			"set ZEROSIGNAL_KEYSTORE_PASSPHRASE before backgrounding, or run `zs-proxy proxy start --foreground`")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Wallet passphrase (unlocks the encrypted-file wallet; forwarded to the background daemon): ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("read passphrase: %w", err)
	}
	return os.Setenv("ZEROSIGNAL_KEYSTORE_PASSPHRASE", string(b))
}

// waitForHealthy polls the daemon's /healthz until it returns 200, the process
// dies, or healthWait elapses. It returns true only on a healthy response;
// false means "died or still starting" — the caller distinguishes via Alive.
func waitForHealthy(ctx context.Context, pid int, listen string) bool {
	deadline := time.Now().Add(healthWait)
	for time.Now().Before(deadline) {
		if !daemon.Alive(pid) {
			return false
		}
		if probeHealthz(ctx, listen) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false
}

// probeHealthz does a single GET /healthz, returning true on a 200.
func probeHealthz(ctx context.Context, listen string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(listen)+"/healthz", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// baseURL renders the dial/display URL for a listen address, normalizing a
// wildcard/empty host to loopback (you dial 127.0.0.1, not 0.0.0.0).
func baseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// daemonLogPath is <logs>/proxy.log — the rotating, metadata-only daemon log.
func daemonLogPath() (string, error) {
	dir, err := paths.LogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "proxy.log"), nil
}

// newProxyStopCmd: `zs proxy stop`.
func newProxyStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the running proxy daemon (graceful drain)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := resolveTarget()
			announceTargetPrecedence(cmd, target)
			switch target.kind {
			case targetDaemon:
				return stopDaemon(cmd, target.daemon, true)
			case targetService:
				return serviceStop(cmd)
			default:
				if target.stale != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "proxy is not running (cleared stale pidfile: %s)\n", target.stale)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "proxy is not running")
				}
				return nil
			}
		},
	}
}

// stopDaemon asks the daemon to drain gracefully — SIGTERM on unix, the loopback
// POST /shutdown control channel on Windows (via daemon.RequestStop) — and waits
// for the process to drain in-flight streams (the server's shutdown grace is
// 10s), then clears the pidfile. verbose controls progress/completion chatter
// so restart can reuse it quietly; target resolution owns "not running" output.
//
// PRECONDITION: rec must be a record the caller has already established is BOTH
// alive and still this program — daemon.Alive plus daemon.ProcessMatches, which
// is what resolveTarget's targetDaemon and daemon.Running both mean. This
// function does not re-check, and RequestStop signals rec.PID unconditionally.
//
// The identity half is the one that bites: a crashed daemon's pid can be
// recycled by an unrelated process before `proxy stop` runs, so handing over an
// unvalidated record SIGTERMs a stranger. ProcessMatches fails open where the
// exe can't be read (everything but Linux), so it only ever refuses on a
// positive mismatch — which is why the check has to happen, not why it's cheap
// to skip.
func stopDaemon(cmd *cobra.Command, rec daemon.Record, verbose bool) error {
	out := cmd.OutOrStdout()
	if err := daemon.RequestStop(rec); err != nil {
		return err
	}
	if verbose {
		fmt.Fprintf(out, "stopping proxy (pid %d)…\n", rec.PID)
	}

	deadline := time.Now().Add(15 * time.Second)
	for daemon.Alive(rec.PID) && time.Now().Before(deadline) {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if daemon.Alive(rec.PID) {
		return fmt.Errorf("proxy (pid %d) did not exit within 15s — it may be draining a long stream; retry `zs-proxy proxy stop`", rec.PID)
	}
	_ = daemon.Remove()
	if verbose {
		fmt.Fprintln(out, "✔ proxy stopped")
	}
	return nil
}

// newProxyStatusCmd: `zs proxy status` — daemon liveness, distinct from the
// top-level `zs status` (wallet/funding health). It reports a live daemon even
// when a service is also installed; otherwise an installed service is queried
// through the OS manager. Config/network/port flags feed that service probe.
func newProxyStatusCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show whether the proxy daemon is running (pid, uptime, listen, network)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := resolveTarget()
			announceTargetPrecedence(cmd, target)
			switch target.kind {
			case targetService:
				return serviceStatus(cmd, &f)
			case targetNone:
				if target.stale != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "proxy: not running (cleared stale pidfile: %s)\n", target.stale)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "proxy: not running")
				}
				return nil
			}

			out := cmd.OutOrStdout()
			rec := target.daemon
			fmt.Fprintln(out, "proxy: running")
			fmt.Fprintf(out, "  pid:     %d\n", rec.PID)
			fmt.Fprintf(out, "  uptime:  %s\n", uptime(rec.StartedAt))
			fmt.Fprintf(out, "  listen:  %s/v1\n", baseURL(rec.Listen))
			fmt.Fprintf(out, "  network: %s\n", rec.Network)
			fmt.Fprintf(out, "  logs:    %s\n", rec.LogPath)
			if probeHealthz(cmd.Context(), rec.Listen) {
				fmt.Fprintln(out, "  health:  ok")
			} else {
				fmt.Fprintln(out, "  health:  not responding (still starting or wedged — check `zs-proxy proxy logs`)")
			}
			return nil
		},
	}
	f.bindCommon(cmd) // --config, --network — for the installed-service health probe
	cmd.Flags().IntVar(&f.port, "port", 0, "listen port override for the installed-service health probe")
	return cmd
}

// newProxyRestartCmd: `zs proxy restart`. Shares the start flags so you can
// restart onto a different --network/--port. Always backgrounds (the
// --foreground flag from the shared set is ignored here).
// restartNetwork is the --network a bare restart passes on: the one rec's
// daemon was started with, else, for a record without it, the resolved
// network when it is an Algorand preset. A Solana network name is no
// --network value when the solana: block selected it, and passing a preset's
// name would reset a program or endpoint the file sets.
func restartNetwork(rec daemon.Record) string {
	if rec.NetworkFlag != "" {
		return rec.NetworkFlag
	}
	if p, ok := build.NetworkPreset(rec.Network); ok && p.SolanaCluster == "" {
		return rec.Network
	}
	return ""
}

func newProxyRestartCmd() *cobra.Command {
	var f proxyFlags
	cmd := &cobra.Command{
		Use:          "restart",
		Short:        "Restart the proxy daemon",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := resolveTarget()
			announceTargetPrecedence(cmd, target)
			if target.kind == targetService {
				return serviceRestart(cmd)
			}
			// Inherit the running daemon's network unless the user overrode it,
			// so a bare `restart` can't silently drift off the network the
			// daemon was started on (e.g. flag-selected mainnet → default
			// testnet). Read the record before stopDaemon clears it.
			if target.kind == targetDaemon && f.network == "" {
				f.network = restartNetwork(target.daemon)
			}
			if target.kind == targetDaemon {
				if err := stopDaemon(cmd, target.daemon, false); err != nil {
					return err
				}
			}
			return f.daemonize(cmd)
		},
	}
	f.bind(cmd)
	return cmd
}

// newProxyLogsCmd: `zs proxy logs [-f] [-n]`. The log is metadata-only by
// design (the non-retention rule) — never prompts or completions.
func newProxyLogsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:          "logs",
		Short:        "Show the daemon log (metadata only — never prompts or responses)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := resolveTarget()
			announceTargetPrecedence(cmd, target)
			if target.kind == targetService {
				return serviceLogs(cmd, lines, follow)
			}
			logPath, err := daemonLogPath()
			if err != nil {
				return err
			}
			// Prefer the running daemon's recorded log path over the computed
			// default, so we always tail the file it's actually writing.
			if target.daemon.LogPath != "" {
				logPath = target.daemon.LogPath
			}
			return tailLog(cmd, logPath, lines, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log as it grows (like tail -f); Ctrl-C to stop")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of trailing lines to show")
	return cmd
}

// tailLog prints the last n lines, then (with follow) streams appended bytes
// until interrupted.
func tailLog(cmd *cobra.Command, path string, n int, follow bool) error {
	out := cmd.OutOrStdout()
	tail, err := daemon.LogTailLines(path, n)
	if err != nil {
		return err
	}
	if len(tail) == 0 && !follow {
		fmt.Fprintf(out, "(no daemon log yet at %s)\n", path)
		return nil
	}
	for _, line := range tail {
		fmt.Fprintln(out, line)
	}
	if !follow {
		return nil
	}
	// Clean exit on Ctrl-C; the lifecycle commands don't otherwise wire signals.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return followLog(ctx, out, path)
}

// followLog streams bytes appended to path after the current end, polling for
// growth and handling truncation/rotation (size shrank → restart from 0).
func followLog(ctx context.Context, w io.Writer, path string) error {
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}
	buf := make([]byte, 32*1024)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue // file may be mid-rotation or not yet created
		}
		if fi.Size() < offset {
			offset = 0 // rotated/truncated
		}
		if fi.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			for {
				nr, er := f.Read(buf)
				if nr > 0 {
					_, _ = w.Write(buf[:nr])
					offset += int64(nr)
				}
				if er != nil {
					break
				}
			}
		}
		_ = f.Close()
	}
}

// uptime renders a human duration since the daemon started.
func uptime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return time.Since(t).Round(time.Second).String()
}
