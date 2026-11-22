//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// Windows process-creation flags (values from winbase.h). Defined locally so
// the daemon package doesn't pull in golang.org/x/sys/windows just for two
// constants. DETACHED_PROCESS drops the console; CREATE_NEW_PROCESS_GROUP makes
// the child its own group so it survives the launcher exiting.
const (
	flagDetachedProcess       = 0x00000008
	flagCreateNewProcessGroup = 0x00000200
)

// detachedSysProcAttr backgrounds the child without a console — the Windows
// analog of setsid. (Not a unix fork, which doesn't exist here.)
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: flagDetachedProcess | flagCreateNewProcessGroup}
}

// processAlive: on Windows os.FindProcess opens the process and errors when it
// no longer exists, which is the liveness signal we need.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}

// terminate is the pid-addressed stop primitive behind Terminate. Windows can't
// receive SIGTERM and a hard kill would sever active SSE streams, so the
// graceful Windows stop goes through RequestStop → the loopback control channel
// (POST /shutdown), not this path. This remains a clear error for any direct
// Terminate(pid) call on Windows.
func terminate(int) error {
	return errors.New("graceful stop on Windows goes through the loopback control channel — use `zs-proxy proxy stop` " +
		"(which calls RequestStop), or stop the process via Task Manager")
}

// processExe: reading another process's image path on Windows needs
// QueryFullProcessImageName via x/sys/windows; deferred with the rest of the
// Windows service work. ok=false → ProcessMatches fails open.
func processExe(int) (string, bool) { return "", false }
