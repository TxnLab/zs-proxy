//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows/svc"

	"github.com/TxnLab/zs-proxy/internal/cli/service"
)

// runningAsWindowsService reports whether the SCM launched this process (vs an
// interactive `proxy start --foreground`). When true, serve() dispatches to the
// SCM handler instead of the signal path.
func runningAsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// serveAsWindowsService runs the proxy under the SCM dispatcher. Serving goes
// through the same serveWithContext path, but the SCM Stop/Shutdown control
// event cancels the context — the Windows analog of the SIGTERM the signal path
// relies on — so the graceful drain (app.Run → srv.Shutdown) is identical.
func (f *proxyFlags) serveAsWindowsService(cmd *cobra.Command) error {
	return svc.Run(service.WindowsServiceName, &windowsServiceHandler{f: f, cmd: cmd})
}

type windowsServiceHandler struct {
	f   *proxyFlags
	cmd *cobra.Command
}

// Execute is the svc.Handler entry point. It starts the serve loop, reports
// Running, then maps SCM Stop/Shutdown to a context cancel and waits for the
// drain — returning only once serving has fully stopped.
func (h *windowsServiceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	// An SCM-launched process's stderr is discarded, so send the proxy logger to
	// the daemon logfile (so `zs-proxy proxy logs` works for the service too).
	// Best-effort: on open failure fall back to stderr (nil) rather than refuse
	// to start.
	var logTo io.Writer
	if lf, err := openServiceLog(); err == nil {
		defer lf.Close()
		logTo = lf
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	// allowBootstrap=false: a service has no TTY and must not silently create an
	// unfunded wallet. It runs as the installing user, so that user's wallet is
	// visible — a missing wallet is a clear error, not a silent create.
	go func() { errc <- h.f.serveWithContext(h.cmd, ctx, false, logTo) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-errc:
			// The serve loop exited on its own (e.g. failed to bind, or a fatal
			// config/keystore error). Report Stopped before returning so the SCM
			// never sees a "running but gone" window; a non-nil err is reported as
			// a service-specific non-clean stop.
			changes <- svc.Status{State: svc.Stopped}
			if err != nil {
				// A startup refusal (no wallet, bad config) returns before app.Run
				// sets up logging, and the SCM discards stderr — without this line
				// `proxy logs` shows nothing about why the service stopped.
				if logTo != nil {
					fmt.Fprintf(logTo, "zs-proxy service stopped: %v\n", err)
				}
				return true, 1
			}
			return false, 0
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				changes <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel() // triggers app.Run's graceful srv.Shutdown
				<-errc   // wait for the drain to complete
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}

// openServiceLog opens the daemon logfile (append) so the SCM service's logs are
// observable — an SCM process's stderr goes nowhere. It reuses proxy.log (the
// self-managed daemon and the SCM service never run at once), so `proxy logs`
// tails either. The service runs as the installing user, so the path resolves
// to that user's log dir.
func openServiceLog() (*os.File, error) {
	path, err := daemonLogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
