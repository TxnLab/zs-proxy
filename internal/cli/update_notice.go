/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/update"
)

// updateCheckTimeout bounds the whole preamble so no command is delayed more
// than this by the (at-most-once-daily) network check.
const updateCheckTimeout = 2 * time.Second

// maybeNotifyUpdate is the root PersistentPreRun preamble: before any command
// runs, it prints a "newer version available" notice to stderr when
// appropriate. Every guard below is a reason to stay silent; the check itself
// (internal/update) is fully fail-silent.
//
// NOTE: cobra invokes only the *nearest* PersistentPreRun in the command chain.
// No subcommand defines one today, so this fires for every command. If a
// subcommand ever adds its own PersistentPreRun, it must call this helper too.
func maybeNotifyUpdate(cmd *cobra.Command) {
	// Source/`go build` binaries have no release to compare against.
	if build.Version == "dev" {
		return
	}
	// Opt-out (matches the PROXY_ZS_* env convention). Enabled by default.
	if v, ok := os.LookupEnv("PROXY_ZS_CHECK_UPDATES"); ok {
		if enabled, err := strconv.ParseBool(v); err == nil && !enabled {
			return
		}
	}
	// Only speak to an interactive terminal. This single guard skips the
	// detached `--foreground` daemon child, OS-service runs, and piped/scripted
	// invocations, and keeps the notice off stdout so parseable output
	// (`version`, `status`, …) is never corrupted.
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), updateCheckTimeout)
	defer cancel()
	update.Notify(ctx, cmd.ErrOrStderr(), build.Version)
}
