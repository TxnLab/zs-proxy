/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package cli is the zerosignal command-line surface: a thin cobra wrapper
// over internal/app. It owns flag/arg parsing, config resolution (embedded
// defaults + file + env + flags), the signal context, and the process exit
// code — app.Run owns the server.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TxnLab/zerosignal/go/keystore"
	sdkwallet "github.com/TxnLab/zerosignal/go/wallet"
	"github.com/TxnLab/zerosignal/go/zs/tee"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// Execute builds the root command and runs it, exiting non-zero on error.
// This is the single entry point main() calls.
func Execute() {
	configureSDK()
	if err := newRootCmd().Execute(); err != nil {
		// cobra already printed the error (SilenceErrors is false); just
		// translate it into a non-zero exit, matching the old os.Exit(1).
		os.Exit(1)
	}
}

// configureSDK sets the SDK's process-wide state before any command runs.
// The detached daemon re-enters through Execute too, so this covers it.
//
// The TEE collateral cache persists under the state directory, not the
// cache directory, so a cache sweep cannot discard what a restart during an
// Intel PCS outage needs. Without a state directory it stays in memory.
func configureSDK() {
	dir, err := paths.StateDir()
	if err != nil {
		slog.Debug("no state directory, TEE collateral cache will not persist", slog.Any("err", err))
		return
	}
	if err := tee.PersistCollateral(dir); err != nil {
		slog.Debug("TEE collateral cache will not persist", slog.Any("err", err))
	}
}

// newRootCmd is the `zs-proxy` root. It carries the proxy-start flags and
// RunE directly so the legacy invocation — `zs-proxy --config config.yaml`
// (and bare `zs-proxy`, the old `go run .` shape) — keeps starting the proxy.
//
// The legacy root form runs in the FOREGROUND (it serves directly, Ctrl-C to
// stop), preserving the old `go run .` behavior and not breaking existing
// deploys / service units that expect a non-forking process. Backgrounding is
// the job of the documented `proxy start` surface, which daemonizes by default
// (and shares this exact flag set).
func newRootCmd() *cobra.Command {
	var f proxyFlags
	root := &cobra.Command{
		Use:   "zs-proxy",
		Short: "Drop-in, private, OpenAI-compatible inference proxy",
		// Don't dump usage on a runtime error from RunE — only on actual
		// flag/arg misuse. Errors are still printed (SilenceErrors stays false).
		SilenceUsage: true,
		// Preamble before every command: a best-effort, at-most-once-daily
		// "newer version available" notice to stderr. Fully guarded + fail-silent
		// (see maybeNotifyUpdate). Cobra runs only the nearest PersistentPreRun,
		// and no subcommand defines one, so this fires for all of them.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { maybeNotifyUpdate(cmd) },
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Compat path stays foreground regardless of --foreground.
			return f.serve(cmd)
		},
	}
	f.bind(root)
	root.AddCommand(newProxyCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newConnectCmd())
	root.AddCommand(newModelsCmd())
	root.AddCommand(newWalletCmd())
	root.AddCommand(newFundCmd())
	root.AddCommand(newSlotsCmd())
	root.AddCommand(newWithdrawCmd())
	root.AddCommand(newCloseDepositCmd())
	root.AddCommand(newTicketsCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newTeeCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newVersionCmd())
	return root
}

// proxyFlags is the shared flag set + run logic for both the root (legacy)
// and `proxy start` invocations.
type proxyFlags struct {
	configPath   string
	network      string
	port         int
	trackTimes   bool
	debugBodies  bool
	foreground   bool
	allowStaging bool
}

// bindCommon binds the flags shared with read-only inspection commands
// (config print-effective): the config path and the network selector.
func (f *proxyFlags) bindCommon(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.configPath, "config", defaultConfigPath(), "path to YAML config file (optional — embedded defaults are used when absent)")
	cmd.Flags().StringVar(&f.network, "network", "", "network: "+build.NetworkNames+" (default: config value, else mainnet)")
}

// bind binds the full proxy-start flag set.
func (f *proxyFlags) bind(cmd *cobra.Command) {
	f.bindCommon(cmd)
	cmd.Flags().IntVar(&f.port, "port", 0, "listen port override (0 = use config)")
	cmd.Flags().BoolVar(&f.trackTimes, "times", false, "log per-request encrypt/decrypt durations")
	cmd.Flags().BoolVar(&f.debugBodies, "debug-bodies", false,
		"DEBUG: dump full plaintext request AND response bodies (streaming frames included) to stderr — violates the non-retention rule; local debugging only")
	cmd.Flags().BoolVar(&f.foreground, "foreground", false,
		"run in the foreground instead of backgrounding (used by service managers and for debugging)")
	cmd.Flags().BoolVar(&f.allowStaging, "allow-staging", false,
		"route to nodes flagged on-chain as staging (default: skip them); also settable via zs.allow_staging / PROXY_ZS_ALLOW_STAGING")
}

// run dispatches `proxy start`: by default it daemonizes (re-execs itself in
// the foreground, detached, and returns to the shell with :9376 live); with
// --foreground it serves directly in this process — the entrypoint a daemon
// child, launchd/systemd/SCM, or an interactive debug run all use.
func (f *proxyFlags) run(cmd *cobra.Command) error {
	if f.foreground {
		// The serving path — an interactive debug run AND the invocation the
		// installed service itself runs (`proxy start --foreground`). It must
		// serve directly, never route back to the service below (that would
		// recurse / no-op the service's own child).
		return f.serve(cmd)
	}
	target := resolveTarget()
	announceTargetPrecedence(cmd, target)
	// An interactive `proxy start` with a service installed and no live daemon:
	// start the OS service rather than spawn a daemon that would fight it.
	if routeStartToService(f.foreground, target.serviceInstalled, target.kind == targetDaemon) {
		return startInstalledService(cmd)
	}
	if target.kind == targetDaemon {
		return fmt.Errorf("proxy already running (pid %d on %s) — use `zs-proxy proxy restart` or `zs-proxy proxy stop`",
			target.daemon.PID, baseURL(target.daemon.Listen))
	}
	return f.daemonize(cmd)
}

// routeStartToService decides whether `proxy start` hands off to the installed
// OS service instead of serving/daemonizing itself. The foreground guard is
// load-bearing: the service runs `proxy start --foreground`, so a foreground
// invocation must NEVER route back to the service (infinite recursion / no-op).
func routeStartToService(foreground, serviceInstalled, daemonRunning bool) bool {
	return !foreground && serviceInstalled && !daemonRunning
}

// serve builds and runs the proxy in the current process until a signal
// (Interrupt/SIGTERM) triggers a graceful drain. This is the actual serving
// path — both the backgrounded daemon child and a foreground debug run land here.
func (f *proxyFlags) serve(cmd *cobra.Command) error {
	// Under the Windows SCM, control events (not OS signals) drive shutdown —
	// dispatch to the service handler, which serves via serveWithContext with a
	// context canceled on the SCM Stop event. No-op off Windows / interactive.
	if runningAsWindowsService() {
		return f.serveAsWindowsService(cmd)
	}

	// Signal-driven shutdown: Interrupt + SIGTERM cancel the context that
	// app.Run watches, which triggers a graceful drain. `proxy stop` sends
	// SIGTERM to reach this path (unix); on Windows the daemon uses the loopback
	// control channel, and the SCM uses the branch above.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return f.serveWithContext(cmd, ctx, true, nil)
}

// serveWithContext builds and runs the proxy until ctx is canceled. The cancel
// source differs per entrypoint (signal, Windows SCM, or the loopback control
// channel inside app.Run); the body is shared. allowBootstrap gates first-run
// wallet setup: true for the signal path (a TTY can prompt import/create, and
// without one bootstrapWallet refuses); false for the Windows SCM service, which
// never has a TTY and gets a service-specific refusal instead (it runs as the
// installing user, so that user's wallet is visible to it). logTo redirects the
// process logger off stderr (the Windows SCM service passes a logfile; nil ⇒ stderr).
func (f *proxyFlags) serveWithContext(cmd *cobra.Command, ctx context.Context, allowBootstrap bool, logTo io.Writer) error {
	cfg, err := f.resolveConfig()
	if err != nil {
		return err
	}

	if allowBootstrap {
		// First run with no signer: sign in / import / create a wallet for the
		// active network on the TTY, or refuse without one.
		if err := bootstrapWallet(cmd, cfg); err != nil {
			return err
		}
	} else if !wallet.DefaultExists() && !hasEnvMnemonic() {
		// Service mode (no TTY): never silently create an unfunded wallet.
		return errNoServiceWallet(false)
	}

	// Warn (don't block) if the managed wallet isn't funded yet — it comes up
	// and returns 402 wallet_unfunded per request until funded.
	if err := warnIfUnfunded(cmd, cfg); err != nil {
		return err
	}

	opts, err := payerKeyOptions(ctx, cfg)
	if err != nil {
		return err
	}

	guard := buildSpendGuard(cfg, cmd.ErrOrStderr())
	defer guard.Close()
	return app.Run(ctx, cfg, app.Options{
		Keystore:         opts.Keystore,
		SolanaKeys:       opts.SolanaKeys,
		SpendGuard:       guard,
		TrackCryptoTimes: f.trackTimes,
		DebugLogBodies:   f.debugBodies,
		// Forwarded by the daemonizing parent (see daemonize) on the
		// backgrounded child only; empty for foreground / service runs, which
		// leaves the loopback POST /shutdown control channel unregistered.
		ShutdownToken: os.Getenv(envShutdownToken),
		LogWriter:     logTo,
	})
}

// buildKeystore assembles the signing keystore: the OS-keychain wallet first
// (only when one is configured — checked via the metadata file, so no keychain
// prompt when there's no wallet), then the env/cloud providers as fallback. An
// env-only deployment (no zerosignal wallet) yields exactly LoadFromEnv's
// result, preserving the legacy path.
func buildKeystore(ctx context.Context) (*keystore.KeyStore, error) {
	opts, err := wallet.PayerOptions()
	if err != nil {
		return nil, err
	}
	return sdkwallet.LoadKeys(ctx, opts)
}

// payerKeyOptions loads the keys of the rail cfg pays on, as Run takes them.
func payerKeyOptions(ctx context.Context, cfg *config.Config) (app.Options, error) {
	var opts app.Options
	var err error
	if cfg.OnSolana() {
		opts.SolanaKeys, err = buildSolanaKeystore(ctx)
	} else {
		opts.Keystore, err = buildKeystore(ctx)
	}
	return opts, err
}

// buildSolanaKeystore is buildKeystore on the Solana rail, from the same
// providers. The keystore skips a 25-word phrase, so a 25-word wallet beside
// an env phrase pays with the env's; with nothing else, the refusal says how
// to replace the wallet without losing it.
func buildSolanaKeystore(ctx context.Context) (*keystore.SolanaKeys, error) {
	opts, err := wallet.PayerOptions()
	if err != nil {
		return nil, err
	}
	sk, err := sdkwallet.LoadSolanaKeys(ctx, opts)
	if errors.Is(err, sdkwallet.ErrWalletNoSolanaKey) {
		return nil, wallet.ErrNoSolanaKey
	}
	return sk, err
}
