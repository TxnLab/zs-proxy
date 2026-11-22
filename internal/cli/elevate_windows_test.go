//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"strings"
	"testing"
)

// TestCommandLine_Quoting checks that arguments are quoted per Windows
// CommandLineToArgvW rules, so a relaunched elevated copy parses them back
// identically (paths with spaces, embedded backslashes/quotes).
func TestCommandLine_Quoting(t *testing.T) {
	got := commandLine([]string{
		"proxy", "install-service",
		"--config", `C:\Program Files\zs\config.yaml`,
		"--service-account", `MACHINE\alice`,
		"--uac-relaunch",
	})
	// A path with a space must be wrapped in quotes; a plain token must not be.
	if !strings.Contains(got, `"C:\Program Files\zs\config.yaml"`) {
		t.Errorf("config path with spaces not quoted:\n%s", got)
	}
	if !strings.Contains(got, "proxy install-service") {
		t.Errorf("plain tokens should pass through unquoted:\n%s", got)
	}
	if !strings.HasSuffix(got, "--uac-relaunch") {
		t.Errorf("relaunch marker should be the final arg:\n%s", got)
	}
}
