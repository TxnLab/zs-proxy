//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"errors"
	"os"
)

// lockFile has no lock to take here, so no guard persists reservations.
func lockFile(*os.File) error { return errors.ErrUnsupported }

// lockFileWait takes no lock either; spent writes then race as they did
// before there was a lock.
func lockFileWait(*os.File) error { return nil }

// replaceFile renames from over to.
func replaceFile(from, to string) error { return os.Rename(from, to) }
