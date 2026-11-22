/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package spend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// adoptWait is how long Adopt waits out another process's hold on the earlier
// name's lock. A start adopting the same tally, or a status check, holds it for
// a moment; a proxy serving holds it for as long as it runs.
const adoptWait = 250 * time.Millisecond

// Adopt returns where a Guard keeps the tally named path: path itself, after
// moving the tally kept under its earlier name, legacy, and that tally's open
// reservations there; or legacy, while a proxy still holds legacy's lock.
//
// That proxy keeps writing under legacy, and each write renames a new file
// over the name. Moved from under it, every charge it records from then on
// lands in a file nothing reads, and the open file it rewrites is lost the
// same way. Staying on legacy keeps one ledger under one .wlock, as two
// proxies on one tally always have, and a later start that finds the lock
// free moves it. A Guard on legacy cannot hold its lock either, so its open
// reservations stay in memory until then.
//
// The error is a move that failed, and the path returned with it is still the
// one to use: legacy when nothing moved, path once the open file has.
func Adopt(legacy, path string) (string, error) {
	if legacy == "" || legacy == path || exists(path) || !exists(legacy) && !exists(OpenPath(legacy)) {
		return path, nil
	}
	lock, err := os.OpenFile(legacy+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return legacy, err
	}
	defer lock.Close()
	if err := lockFileWithin(lock, adoptWait); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return legacy, nil
	}
	// A Guard writing the ledger without the lock, a status check among them,
	// finishes its write first.
	wlock, err := os.OpenFile(legacy+".wlock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return legacy, err
	}
	defer wlock.Close()
	if err := lockFileWait(wlock); err != nil {
		return legacy, err
	}
	// The open file first: once the ledger moves, a crash before the open
	// file follows would leave its reservations under a name nothing reads.
	if err := moveIfAbsent(OpenPath(legacy), OpenPath(path)); err != nil {
		return legacy, err
	}
	return path, moveIfAbsent(legacy, path)
}

// Kept returns where a reader finds the tally named path without moving
// anything: path, or legacy while only legacy holds one.
func Kept(legacy, path string) string {
	if legacy != "" && !exists(path) && exists(legacy) {
		return legacy
	}
	return path
}

// moveIfAbsent links from to to and removes from, leaving both when to exists.
// Linked rather than renamed, so a crash between the two steps leaves the
// tally under both names rather than under neither.
func moveIfAbsent(from, to string) error {
	if err := os.Link(from, to); err != nil {
		if errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("move %s to %s: %w", from, to, err)
	}
	return os.Remove(from)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// lockFileWithin is lockFile, retried until wait has passed.
func lockFileWithin(f *os.File, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := lockFile(f)
		if err == nil || errors.Is(err, errors.ErrUnsupported) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
