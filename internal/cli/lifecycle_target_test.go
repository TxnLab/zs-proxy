/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/TxnLab/zerosignal/go/wallet"
	"github.com/spf13/cobra"

	"github.com/TxnLab/zs-proxy/internal/daemon"
)

func TestClassifyTarget(t *testing.T) {
	rec := daemon.Record{PID: 42}
	// staleHas asserts WHICH stale reason, not just that one exists: a dead pid
	// and a recycled pid are different things to tell the user, and they are the
	// two halves of stopDaemon's precondition.
	tests := []struct {
		name                                string
		recorded, alive, matches, installed bool
		want                                targetKind
		staleHas                            string
	}{
		{name: "neither", want: targetNone},
		{name: "service only", installed: true, want: targetService},
		{name: "daemon only", recorded: true, alive: true, matches: true, want: targetDaemon},
		{name: "daemon wins when both true", recorded: true, alive: true, matches: true, installed: true, want: targetDaemon},
		{name: "dead daemon", recorded: true, want: targetNone, staleHas: "no longer running"},
		{name: "reused pid, no service", recorded: true, alive: true, want: targetNone, staleHas: "different program"},
		{name: "reused pid falls through to service", recorded: true, alive: true, installed: true, want: targetService, staleHas: "different program"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyTarget(rec, tt.recorded, tt.alive, tt.matches, tt.installed)
			if got.kind != tt.want {
				t.Fatalf("kind = %v, want %v", got.kind, tt.want)
			}
			if (got.stale != "") != (tt.staleHas != "") {
				t.Fatalf("stale = %q, want present=%v", got.stale, tt.staleHas != "")
			}
			if tt.staleHas != "" && !strings.Contains(got.stale, tt.staleHas) {
				t.Errorf("stale = %q, want it to mention %q", got.stale, tt.staleHas)
			}
		})
	}
}

func TestResolveTarget_UnreadableDaemonRecordSelfHeals(t *testing.T) {
	removed := false
	r := lifecycleTargetResolver{
		readDaemon: func() (daemon.Record, bool, error) {
			return daemon.Record{}, false, errors.New("truncated JSON")
		},
		daemonAlive:      func(int) bool { return false },
		processMatches:   func(daemon.Record) bool { return false },
		serviceInstalled: func() (bool, error) { return true, nil },
		removeDaemon: func() error {
			removed = true
			return nil
		},
	}

	got := r.resolve()
	if got.kind != targetService {
		t.Fatalf("kind = %v, want service", got.kind)
	}
	if !strings.Contains(got.stale, "unreadable daemon record") || !removed {
		t.Fatalf("stale = %q, removed = %v; want recorded self-heal", got.stale, removed)
	}
}

func TestResolveTarget_ServiceProbeErrorDoesNotHideDaemon(t *testing.T) {
	rec := daemon.Record{PID: 42}
	r := lifecycleTargetResolver{
		readDaemon:       func() (daemon.Record, bool, error) { return rec, true, nil },
		daemonAlive:      func(pid int) bool { return pid == rec.PID },
		processMatches:   func(got daemon.Record) bool { return got == rec },
		serviceInstalled: func() (bool, error) { return false, errors.New("access denied") },
		removeDaemon:     func() error { return nil },
	}

	got := r.resolve()
	if got.kind != targetDaemon {
		t.Fatalf("kind = %v, want daemon", got.kind)
	}
	if !strings.Contains(got.serviceProbeErr, "access denied") {
		t.Fatalf("serviceProbeErr = %q, want recorded probe failure", got.serviceProbeErr)
	}

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	announceTargetPrecedence(cmd, got)
	if !strings.Contains(stderr.String(), "service status unknown") || !strings.Contains(stderr.String(), "access denied") {
		t.Fatalf("stderr = %q, want recorded service probe reason", stderr.String())
	}
	// The wording must not promise the daemon is the answer — a denied SCM query
	// is precisely when an installed service may be running unseen, and the
	// command may still go on to print "not running".
	if strings.Contains(stderr.String(), "continuing with") {
		t.Errorf("stderr = %q, want it to scope the claim, not assert the daemon is authoritative", stderr.String())
	}
}

// TestAnnounceTargetPrecedence_StaleRecordTargetsService: the other non-obvious
// ownership choice. A stale record is why the service won, so the reason has to
// reach the user — otherwise `stop` silently switches targets between runs.
func TestAnnounceTargetPrecedence_StaleRecordTargetsService(t *testing.T) {
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	announceTargetPrecedence(cmd, lifecycleTarget{
		kind:             targetService,
		serviceInstalled: true,
		stale:            "pid 42 is no longer running",
	})

	got := stderr.String()
	if !strings.Contains(got, "stale") || !strings.Contains(got, "pid 42 is no longer running") {
		t.Fatalf("precedence output = %q, want the stale reason", got)
	}
	if !strings.Contains(got, "installed OS service") {
		t.Fatalf("precedence output = %q, want the resulting target named", got)
	}
}

func TestAnnounceTargetPrecedence_BothTargetsDaemon(t *testing.T) {
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	announceTargetPrecedence(cmd, lifecycleTarget{
		kind:             targetDaemon,
		daemon:           daemon.Record{PID: 42},
		serviceInstalled: true,
	})

	got := stderr.String()
	if !strings.Contains(got, "targeting the daemon") || !strings.Contains(got, "pid 42") {
		t.Fatalf("precedence output = %q, want daemon choice and pid", got)
	}
}

// A proxy in another home must not see the OS service, which runs the user's
// own proxy: `proxy stop` there would stop it. Only the other-home branch is
// tested, since the default one asks the real OS manager.
func TestOtherHome_HidesAndRefusesTheService(t *testing.T) {
	t.Setenv(wallet.HomeEnv, t.TempDir())
	if installed, err := serviceInstalledHere(); installed || err != nil {
		t.Errorf("serviceInstalledHere() = %v, %v in another home; want false, nil", installed, err)
	}
	if err := refuseInOtherHome(); err == nil || !strings.Contains(err.Error(), wallet.HomeEnv) {
		t.Errorf("refuseInOtherHome() = %v, want a refusal naming %s", err, wallet.HomeEnv)
	}
	t.Setenv(wallet.HomeEnv, "")
	if err := refuseInOtherHome(); err != nil {
		t.Errorf("refuseInOtherHome() = %v in the default home", err)
	}
}

// The commands themselves, not just the helpers, keep another home away from
// the service. Each case stops short of the OS if the wiring regresses:
// resolveTarget reads the temp home's daemon record, install-service only
// renders with --print, and uninstall-service gets a fake removal.
func TestOtherHome_CommandsLeaveTheServiceAlone(t *testing.T) {
	t.Setenv(wallet.HomeEnv, t.TempDir())

	if target := resolveTarget(); target.serviceInstalled || target.kind == targetService {
		t.Errorf("resolveTarget in another home = %+v, want no service", target)
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runInstallService(cmd, &proxyFlags{}, true, "", false); err == nil || !strings.Contains(err.Error(), wallet.HomeEnv) {
		t.Errorf("install-service --print in another home: %v, want the refusal (rendered %d bytes)", err, out.Len())
	}

	removed := false
	err := runUninstallService(cmd, true, func() (string, error) { removed = true; return "unit", nil })
	if removed || err == nil || !strings.Contains(err.Error(), wallet.HomeEnv) {
		t.Errorf("uninstall-service in another home: removed %v, err %v", removed, err)
	}
}
