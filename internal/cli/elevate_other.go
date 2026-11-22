//go:build !windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import "github.com/spf13/cobra"

// ensureElevatedForInstall is a no-op off Windows: the launchd/systemd user-unit
// installs need no elevation, so the caller always proceeds in-process. It
// exists only so the cross-platform install command compiles everywhere.
func ensureElevatedForInstall(*cobra.Command, string, bool) (relaunched bool, err error) {
	return false, nil
}
