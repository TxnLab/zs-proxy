/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/cli/service"
	"github.com/TxnLab/zs-proxy/internal/daemon"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// newProxyInstallServiceCmd: `zs proxy install-service`. Generates a per-user
// launchd/systemd unit that runs THIS binary in the foreground (with whatever
// --network/--config/--port you pass baked in) so the proxy starts at login and
// restarts on failure — replacing the manual "find the bundled unit, edit the
// path, copy it" dance.
func newProxyInstallServiceCmd() *cobra.Command {
	var (
		f              proxyFlags
		printOnly      bool
		serviceAccount string
		uacRelaunch    bool
	)
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Install zs-proxy as an OS service (launchd/systemd/Windows SCM) — starts on boot/login, restarts on failure",
		Long: "Registers this binary to run as `proxy start --foreground` with the resolved path\n" +
			"baked in: a launchd LaunchAgent (macOS), a systemd user unit (Linux), or a Windows\n" +
			"service via the SCM. Pass --network/--config/--port to pin the service to a\n" +
			"configuration. On Windows this needs administrator rights — if you aren't elevated\n" +
			"it raises a UAC prompt and continues in a new window.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := runInstallService(cmd, &f, printOnly, serviceAccount, uacRelaunch)
			if uacRelaunch {
				// We're the elevated child UAC opened in a fresh console, which
				// closes the instant this process exits. Surface the outcome and
				// wait so the user can read it (errors aren't returned to cobra
				// here — that would print them AFTER the pause prompt).
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "\n✘ %v\n", err)
				}
				waitForEnter(cmd)
				return nil
			}
			return err
		},
	}
	f.bindCommon(cmd) // --config, --network
	cmd.Flags().IntVar(&f.port, "port", 0, "listen port override baked into the service")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the unit to stdout instead of installing it")
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "Windows only: account to run the service as (default: the installing user)")
	cmd.Flags().BoolVar(&uacRelaunch, "uac-relaunch", false, "internal: marks the UAC-elevated relaunch (Windows)")
	_ = cmd.Flags().MarkHidden("uac-relaunch")
	return cmd
}

// runInstallService renders/installs the OS service. On Windows it first ensures
// administrator rights (self-elevating via UAC if needed), then resolves the
// run-as account, prompts for its password, grants it the "Log on as a service"
// right, and registers the SCM service. On macOS/Linux it writes the per-user
// unit. uacRelaunch is true only in the elevated child spawned by the UAC prompt.
func runInstallService(cmd *cobra.Command, f *proxyFlags, printOnly bool, serviceAccount string, uacRelaunch bool) error {
	if err := refuseInOtherHome(); err != nil {
		return err
	}
	if !service.Supported() {
		return service.ErrUnsupported
	}
	spec, err := f.serviceSpec()
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if printOnly {
		unit, err := service.Render(spec)
		if err != nil {
			return err
		}
		fmt.Fprint(out, unit)
		return nil
	}

	// The service's first start has no TTY, so it refuses without a wallet (see
	// bootstrapWallet) — and the unit restarts on failure, so that refusal would
	// repeat every few seconds into a log nobody is reading. Refuse here instead,
	// where the user is at a terminal. Only the keychain wallet counts: an env
	// mnemonic in this shell never reaches the service.
	if serviceWalletCheckApplies(runtime.GOOS, serviceAccount, uacRelaunch, service.DefaultServiceAccount) {
		if !wallet.DefaultExists() {
			return errNoServiceWallet(hasEnvMnemonic())
		}
	} else if !uacRelaunch {
		fmt.Fprintf(cmd.ErrOrStderr(), "⚠ can't check from here that %s has a wallet; the service won't start without one\n", serviceAccount)
	}

	// If we're going to enable the service now (the OS-keychain backend reads
	// unattended), a running self-managed daemon from `proxy start` holds the
	// listen port and the service couldn't bind alongside it. Stop it first —
	// here, in the original user process before any Windows elevation, so
	// daemon.Running() resolves the payer's own state dir. A file-backend install
	// isn't auto-enabled (it needs ZEROSIGNAL_KEYSTORE_PASSPHRASE wired in first),
	// so leave the daemon running until then. Idempotent on the Windows elevated
	// relaunch: the parent already stopped it, so this is a no-op there.
	fileBackend := wallet.UsesFileBackend()
	if !fileBackend {
		if rec, running, _ := daemon.Running(); running {
			fmt.Fprintf(out, "→ stopping the self-managed daemon (pid %d) so the service can take over…\n", rec.PID)
			if err := stopDaemon(cmd, rec, false); err != nil {
				return fmt.Errorf("stop the running daemon before installing the service: %w", err)
			}
		}
	}

	// On Windows the SCM service must run as a user account to reach that user's
	// Credential Manager wallet + per-user state dir (a LocalSystem service sees
	// neither). Resolve the account (default: the installing user), self-elevate
	// if needed, then prompt for the account's Windows password — hidden, never an
	// argv flag, never logged. No-op on macOS/Linux (per-user units already).
	if runtime.GOOS == "windows" {
		acct := serviceAccount
		if acct == "" {
			acct, err = service.DefaultServiceAccount()
			if err != nil {
				return err
			}
		}

		// Installing the SCM service (CreateService + the LSA logon-right grant)
		// needs administrator rights. If we aren't elevated, raise a UAC prompt and
		// relaunch elevated — pinning the resolved account onto the relaunch so the
		// service still runs as the intended user even under over-the-shoulder
		// elevation. When relaunched, this (non-elevated) process is done.
		relaunched, err := ensureElevatedForInstall(cmd, acct, uacRelaunch)
		if err != nil {
			return err
		}
		if relaunched {
			return nil
		}

		pw, err := readServicePassword(cmd, acct)
		if err != nil {
			return err
		}
		spec.Account = acct
		spec.Password = pw
	}

	// A file-backend wallet can't be unlocked unattended (no TTY, and the service
	// doesn't inherit the shell env), so write the unit but DON'T auto-enable it
	// into a crash loop — warn and let the user wire the passphrase in first. The
	// OS-keychain backend reads unattended. fileBackend was resolved above (it also
	// gates the daemon-stop).
	path, enabled, err := service.Install(spec, !fileBackend)
	if err != nil {
		// The unit was written if path is set; surface the enable failure with the
		// manual command rather than failing silently.
		if path != "" {
			fmt.Fprintf(out, "✔ installed service: %s\n", path)
			fmt.Fprintf(cmd.ErrOrStderr(), "⚠ could not enable it automatically: %v\n  Enable it manually:\n    %s\n", err, service.EnableHint())
			return nil
		}
		return err
	}

	fmt.Fprintf(out, "✔ wrote service unit: %s\n", path)
	if enabled {
		fmt.Fprintln(out, "✔ enabled — the proxy starts now and on boot/login (restarts on failure)")
		fmt.Fprintln(out, "  Manage it (these now target the installed service):")
		fmt.Fprintln(out, "    status:   zs-proxy proxy status")
		fmt.Fprintln(out, "    stop:     zs-proxy proxy stop")
		fmt.Fprintln(out, "    restart:  zs-proxy proxy restart")
		fmt.Fprintln(out, "    logs:     zs-proxy proxy logs -f")
		fmt.Fprintln(out, "    remove:   zs-proxy proxy uninstall-service")
	}
	if fileBackend {
		ew := cmd.ErrOrStderr()
		fmt.Fprintln(ew, "⚠ your wallet uses the encrypted-file backend; a background service can't prompt for")
		fmt.Fprintln(ew, "  its passphrase. Provide ZEROSIGNAL_KEYSTORE_PASSPHRASE to the service environment, then enable it:")
		fmt.Fprintf(ew, "    %s\n", service.EnableHint())
		fmt.Fprintln(ew, "  (A wallet in the OS keychain, as signed releases create, needs no passphrase.)")
	}
	return nil
}

// serviceWalletCheckApplies reports whether install-service can check the
// run-as account's wallet from this process, which reads its OWN state dir. So
// only when the service runs as the current user. Not on the UAC relaunch: under
// over-the-shoulder elevation that child runs as the ADMIN account, whose state
// dir holds no wallet, and the unelevated parent already checked. Not for a
// --service-account naming someone else, whose wallet this process can't see.
func serviceWalletCheckApplies(goos, serviceAccount string, uacRelaunch bool, currentAccount func() (string, error)) bool {
	if uacRelaunch {
		return false
	}
	if goos != "windows" || serviceAccount == "" {
		return true
	}
	cur, err := currentAccount()
	return err == nil && strings.EqualFold(cur, serviceAccount)
}

// waitForEnter blocks until the user presses Enter. Used only by the Windows
// UAC-elevated relaunch, whose fresh console window closes the moment the
// process exits — the pause keeps the install's success/failure on screen.
func waitForEnter(cmd *cobra.Command) {
	fmt.Fprint(cmd.ErrOrStderr(), "\nPress Enter to close this window... ")
	_, _ = bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
}

// readServicePassword prompts (hidden) for the Windows account password used to
// register a user-account service. Never an argv flag (would leak via ps), never
// logged. Requires a TTY — a service install is an interactive, elevated action.
func readServicePassword(cmd *cobra.Command, account string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("installing the service as %q needs that account's Windows password, "+
			"but stdin isn't a terminal; run install-service from an interactive (elevated) console", account)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Windows password for %s (to run the service as this account): ", account)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	pw := string(b)
	for i := range b { // wipe the buffer, mirroring the keystore's secret-wipe
		b[i] = 0 // posture (best-effort; the string copy can't be zeroed)
	}
	return pw, nil
}

// newProxyUninstallServiceCmd: `zs proxy uninstall-service`.
func newProxyUninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall-service",
		Short:        "Stop and remove the installed OS service (launchd/systemd/Windows SCM)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUninstallService(cmd, service.Supported(), service.Uninstall)
		},
	}
}

// runUninstallService takes the removal as a parameter so a test can check
// the other-home refusal comes first without reaching the real service.
func runUninstallService(cmd *cobra.Command, supported bool, uninstall func() (string, error)) error {
	if err := refuseInOtherHome(); err != nil {
		return err
	}
	if !supported {
		return service.ErrUnsupported
	}
	path, err := uninstall()
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✔ removed service: %s\n", path)
	return nil
}

// serviceSpec resolves the binary path and the args the service should run with
// — the same foreground invocation childArgs builds for the self-managed daemon,
// but with an ABSOLUTE config path: a service runs from a different working
// directory (launchd/systemd cwd is `/` or $HOME), so a relative --config (e.g.
// the cwd config.yaml the default discovery may have found) would not resolve.
func (f *proxyFlags) serviceSpec() (service.Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return service.Spec{}, fmt.Errorf("resolve own executable path: %w", err)
	}
	sf := *f
	if sf.configPath != "" && !filepath.IsAbs(sf.configPath) {
		abs, err := filepath.Abs(sf.configPath)
		if err != nil {
			return service.Spec{}, fmt.Errorf("resolve config path %q: %w", sf.configPath, err)
		}
		sf.configPath = abs
	}
	return service.Spec{BinPath: exe, Args: sf.childArgs()}, nil
}
