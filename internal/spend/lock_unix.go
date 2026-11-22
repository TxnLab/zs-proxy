//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f without waiting. Closing f releases
// it, and so does the process exiting.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockFileWait is lockFile, waiting for the lock.
func lockFileWait(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// replaceFile renames from over to.
func replaceFile(from, to string) error { return os.Rename(from, to) }
