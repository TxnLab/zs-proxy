//go:build !windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

import (
	"os"
	"syscall"
)

// detachedSysProcAttr starts the child in a new session (setsid) so it survives
// the parent exiting and detaches from the controlling terminal — the unix
// equivalent of "go run it in the background".
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive uses signal 0, which performs error checking (permission /
// existence) without delivering a signal — the standard liveness probe.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // never fails on unix
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but is owned by another user; for our
	// single-user localhost daemon that shouldn't happen, but treat it as alive
	// rather than reporting a stale record we can't actually clean.
	return err == syscall.EPERM
}

// terminate sends SIGTERM, which the serving process already wires (via
// signal.NotifyContext) to a graceful srv.Shutdown that drains in-flight SSE
// streams. We never SIGKILL — that would cut active streams.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// processExe (the pid-reuse guard's exe-path probe) is per-OS:
// proc_exe_linux.go (/proc), proc_exe_darwin.go (proc_pidpath, cgo),
// proc_exe_stub.go (everything else — fail open).
