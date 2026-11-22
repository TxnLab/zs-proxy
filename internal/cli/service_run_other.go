//go:build !windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// runningAsWindowsService is always false off Windows. serveAsWindowsService is
// therefore never reached (serve() guards on it) and exists only so cli.go
// compiles everywhere.
func runningAsWindowsService() bool { return false }

func (f *proxyFlags) serveAsWindowsService(*cobra.Command) error {
	return errors.New("windows service mode is not available on this OS")
}
