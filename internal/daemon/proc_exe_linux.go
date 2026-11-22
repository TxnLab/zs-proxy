//go:build linux

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

import (
	"fmt"
	"os"
)

// processExe reads the live process's executable path from the /proc/<pid>/exe
// symlink. ok=false when the pid is gone or unreadable, so ProcessMatches fails
// open rather than guessing.
func processExe(pid int) (string, bool) {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", false
	}
	return exe, true
}
