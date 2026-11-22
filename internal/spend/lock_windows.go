//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f without waiting. Closing f releases
// it, and so does the process exiting.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}

// lockFileWait is lockFile, waiting for the lock.
func lockFileWait(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped))
}

// replaceFile renames from over to, retrying for a second while another
// process has either file open: Go opens files without FILE_SHARE_DELETE, so a
// reader, or a virus scanner on the new temp file, fails the rename.
func replaceFile(from, to string) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := os.Rename(from, to)
		if err == nil || time.Now().After(deadline) ||
			!(errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
