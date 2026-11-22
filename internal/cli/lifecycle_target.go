/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"fmt"

	"github.com/TxnLab/zerosignal/go/wallet"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/cli/service"
	"github.com/TxnLab/zs-proxy/internal/daemon"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// targetKind identifies the process owner a lifecycle command must control.
// A live self-managed daemon deliberately wins over an installed OS service:
// install-service leaves that daemon running for encrypted-file wallets, so
// both forms of ownership can legitimately exist at once.
type targetKind uint8

const (
	targetNone targetKind = iota
	targetDaemon
	targetService
)

type lifecycleTarget struct {
	kind             targetKind
	daemon           daemon.Record
	serviceInstalled bool
	stale            string
	serviceProbeErr  string
}

type lifecycleTargetResolver struct {
	readDaemon       func() (daemon.Record, bool, error)
	daemonAlive      func(int) bool
	processMatches   func(daemon.Record) bool
	serviceInstalled func() (bool, error)
	removeDaemon     func() error
}

// resolveTarget computes process ownership once for a command. Besides the
// selected owner it retains the daemon record and a reason when that record is
// stale, so callers can report the state consistently and still find its log.
//
// It cannot fail, deliberately: discovery is not the operation the user asked
// for, so both probe errors below degrade into the resolved state (and into
// announceTargetPrecedence's output) rather than aborting the command. That is
// what keeps `stop` / `status` a recovery path when the record is corrupt or SCM
// inspection is denied — so there is no error to return and no caller branch to
// write for one.
func resolveTarget() lifecycleTarget {
	return (lifecycleTargetResolver{
		readDaemon:       daemon.Read,
		daemonAlive:      daemon.Alive,
		processMatches:   daemon.ProcessMatches,
		serviceInstalled: serviceInstalledHere,
		removeDaemon:     daemon.Remove,
	}).resolve()
}

// serviceInstalledHere hides the OS service from a proxy in another home, so
// its lifecycle commands drive only that home's daemon, never the service that
// runs the user's own proxy.
func serviceInstalledHere() (bool, error) {
	if paths.OtherHome() {
		return false, nil
	}
	return service.Installed()
}

// refuseInOtherHome stops install-service and uninstall-service in another
// home: the OS service is one per user and runs the default home's proxy.
func refuseInOtherHome() error {
	if paths.OtherHome() {
		return fmt.Errorf("%s is set: the OS service runs the default home's proxy, so it is not installed or removed from another home; use `zs-proxy proxy start` here", wallet.HomeEnv)
	}
	return nil
}

func (r lifecycleTargetResolver) resolve() lifecycleTarget {
	rec, recorded, readErr := r.readDaemon()
	if readErr != nil {
		// A corrupt or otherwise unreadable record cannot identify a process.
		// Preserve the pre-refactor best-effort behavior: clear it and continue
		// so lifecycle commands remain a recovery path rather than requiring a
		// manual state-directory edit.
		rec, recorded = daemon.Record{}, false
	}
	alive := recorded && r.daemonAlive(rec.PID)
	matches := alive && r.processMatches(rec)

	installed, installedErr := r.serviceInstalled()
	if installedErr != nil {
		// Installed is discovery, not the operation the user requested. In
		// particular, a non-elevated Windows user must still be able to target
		// their self-managed daemon when SCM inspection is denied.
		installed = false
	}

	target := classifyTarget(rec, recorded, alive, matches, installed)
	if readErr != nil {
		target.stale = fmt.Sprintf("unreadable daemon record: %v", readErr)
	}
	if installedErr != nil {
		target.serviceProbeErr = installedErr.Error()
	}
	if target.stale != "" {
		// A stale record must not keep affecting later commands. Removal is
		// best-effort, matching the existing status/stop cleanup behaviour.
		_ = r.removeDaemon()
	}
	return target
}

// classifyTarget is the pure ownership truth table behind resolveTarget.
//
// matches is the pid-reuse guard (daemon.ProcessMatches): recorded && alive is
// not enough to own a process, because a crashed daemon's pid can already
// belong to something else. Only recorded && alive && matches yields
// targetDaemon — everything short of it is stale, which is what keeps
// stopDaemon's precondition true for every caller that starts here.
func classifyTarget(rec daemon.Record, recorded, alive, matches, installed bool) lifecycleTarget {
	target := lifecycleTarget{daemon: rec, serviceInstalled: installed}
	if recorded && alive && matches {
		target.kind = targetDaemon
		return target
	}
	if recorded {
		if !alive {
			target.stale = fmt.Sprintf("pid %d is no longer running", rec.PID)
		} else {
			target.stale = fmt.Sprintf("pid %d now belongs to a different program", rec.PID)
		}
	}
	if installed {
		target.kind = targetService
	}
	return target
}

// announceTargetPrecedence makes non-obvious ownership choices visible. In
// particular, the both-true state must never silently switch commands from an
// already-running daemon to an idle installed service.
func announceTargetPrecedence(cmd *cobra.Command, target lifecycleTarget) {
	if target.serviceProbeErr != "" {
		// Deliberately does NOT claim the daemon is the answer: a denied SCM
		// query is exactly the case where an installed service may be running
		// and we cannot see it, so a command that goes on to report "not
		// running" must not have promised otherwise one line earlier.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"→ OS service status unknown (%s); reporting self-managed daemon state only\n",
			target.serviceProbeErr)
	}
	switch {
	case target.kind == targetDaemon && target.serviceInstalled:
		fmt.Fprintf(cmd.ErrOrStderr(),
			"→ self-managed daemon (pid %d) is running while an OS service is installed; targeting the daemon\n",
			target.daemon.PID)
	case target.kind == targetService && target.stale != "":
		fmt.Fprintf(cmd.ErrOrStderr(),
			"→ ignoring stale self-managed daemon record (%s); targeting the installed OS service\n",
			target.stale)
	}
}
