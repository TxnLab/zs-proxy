//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
)

// ensureElevatedForInstall guarantees the service install runs with
// administrator rights. Installing an SCM service requires them (CreateService
// plus the LSA "Log on as a service" grant), so when this process isn't
// elevated we raise a UAC consent prompt and relaunch an elevated copy of the
// same command — returning relaunched=true so the caller exits and lets the
// elevated copy finish the work.
//
// runAs is the resolved run-as account; it's pinned onto the relaunch via
// --service-account so the service still runs as the intended user even when the
// UAC prompt is satisfied by a different administrator ("over-the-shoulder")
// account. alreadyRelaunched is the --uac-relaunch marker, a guard against an
// elevation loop should elevation silently fail to raise the token.
func ensureElevatedForInstall(cmd *cobra.Command, runAs string, alreadyRelaunched bool) (relaunched bool, err error) {
	elevated, err := processIsElevated()
	if err != nil {
		return false, fmt.Errorf("determine elevation: %w", err)
	}
	if elevated {
		return false, nil
	}
	if alreadyRelaunched {
		return false, errors.New("relaunched for elevation but the new process is still not running as " +
			"Administrator — run install-service from an elevated (Administrator) console")
	}

	ew := cmd.ErrOrStderr()
	fmt.Fprintln(ew, "Installing a Windows service requires administrator rights.")
	fmt.Fprintln(ew, "Requesting elevation — approve the User Account Control prompt.")
	fmt.Fprintln(ew, "Setup (including the service password prompt) continues in the new window.")

	if err := relaunchElevated([]string{"--service-account", runAs, "--uac-relaunch"}); err != nil {
		return false, err
	}
	return true, nil
}

// processIsElevated reports whether the current process token is elevated.
func processIsElevated() (bool, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer tok.Close()
	return tok.IsElevated(), nil
}

// relaunchElevated re-runs this executable with the current arguments plus
// extraArgs under ShellExecute's "runas" verb, which raises the UAC prompt. The
// elevated child gets its own fresh console (the integrity boundary can't share
// ours), which is why callers append --uac-relaunch so it pauses before that
// console closes. A declined prompt is reported as a clear message, not a raw
// Win32 error.
func relaunchElevated(extraArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	args := append(append([]string{}, os.Args[1:]...), extraArgs...)

	verb, _ := windows.UTF16PtrFromString("runas")
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	argPtr, err := windows.UTF16PtrFromString(commandLine(args))
	if err != nil {
		return err
	}
	var cwdPtr *uint16
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		cwdPtr, _ = windows.UTF16PtrFromString(cwd)
	}

	const swShowNormal = 1
	if err := windows.ShellExecute(0, verb, exePtr, argPtr, cwdPtr, swShowNormal); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return errors.New("elevation was declined at the UAC prompt; install-service needs administrator rights")
		}
		return fmt.Errorf("relaunch with elevation: %w", err)
	}
	return nil
}

// commandLine joins args into one Windows command line, quoting each argument
// per the CommandLineToArgvW rules the relaunched process parses them back with
// (handles spaces, backslashes, and embedded quotes).
func commandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	return strings.Join(quoted, " ")
}
