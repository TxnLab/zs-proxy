/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	protoalgod "github.com/TxnLab/zerosignal/go/algod"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/paths"
	"github.com/TxnLab/zs-proxy/internal/spend"
)

// buildSpendGuard constructs the dispatch-path spend guard from the resolved
// caps, persisting its daily tally under the state dir so a restart doesn't
// reset the day's running total. Caps are entirely optional (0 = unlimited for
// that axis); a guard with no caps still tracks the day's spend so `zs status`
// can show it. A cap is never imposed by default — onboarding suggests one (see
// the mainnet fund flow) but never requires it.
//
// It moves the tally from its earlier name, and warn takes a failed move.
func buildSpendGuard(cfg *config.Config, warn io.Writer) *spend.Guard {
	path := ""
	if dir, err := paths.StateDir(); err == nil && dir != "" {
		var err error
		path, err = spendStatePath(dir, cfg, true)
		if err != nil {
			fmt.Fprintf(warn, "warning: moving the spend tally to its new file failed, so today's spend or open reservations may not count toward the daily cap: %v\n", err)
		}
	}
	return newSpendGuard(cfg, path)
}

// readSpendGuard is buildSpendGuard for status, which only reads and so moves
// nothing: a move it made would race a proxy serving from the earlier name
// without its lock.
func readSpendGuard(cfg *config.Config) *spend.Guard {
	path := ""
	if dir, err := paths.StateDir(); err == nil && dir != "" {
		path, _ = spendStatePath(dir, cfg, false)
	}
	return newSpendGuard(cfg, path)
}

func newSpendGuard(cfg *config.Config, path string) *spend.Guard {
	return spend.New(spend.Config{
		DailyCapMicroUSDC:      usdcToMicro(cfg.Spend.DailyCapUSDC),
		PerRequestCapMicroUSDC: usdcToMicro(cfg.Spend.PerRequestCapUSDC),
	}, path)
}

// legacySpendFile held every network's tally in one file, so testnet spend
// counted against the mainnet cap and the other way round.
const legacySpendFile = "spend.json"

// spendStatePath is one tally per deployment, named as the ticket journal is
// (spend-<deployment key>.json), since the state dir is shared across
// networks. "" keeps the tally in memory, for a network the client would
// refuse anyway.
//
// With adopt, a tally with no file yet takes over the one under its earlier
// name, spend-<app id>.json, along with that file's open reservations
// (spend.Adopt); without them an upgrade mid-day would hand out a fresh cap.
// Without adopt it reads whichever name holds the tally. A mainnet tally with
// neither starts from the older all-networks spend.json: that overstates the
// day, so an upgrade errs toward refusing rather than handing out a fresh cap.
// The error is a failed move; the path is still the one to use.
func spendStatePath(dir string, cfg *config.Config, adopt bool) (string, error) {
	d, _, err := zs.ResolveNetwork(app.NetworkFor(cfg))
	if err != nil {
		return "", nil
	}
	path, legacy := zs.StateFile(dir, "spend", d)
	var moveErr error
	if adopt {
		path, moveErr = spend.Adopt(legacy, path)
	} else {
		path = spend.Kept(legacy, path)
	}
	// By app, not network name: a config naming only an endpoint has no
	// network but is still mainnet's tally.
	if mainnet := protoalgod.EscrowAppID("mainnet"); mainnet == 0 || cfg.Zs.EscrowAppID != mainnet {
		return path, moveErr
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return path, moveErr
	}
	// Best effort: without the copy the day starts at zero, as every
	// network's did before the split. O_EXCL, since a running proxy may have
	// written its own file since the Stat.
	b, err := os.ReadFile(filepath.Join(dir, legacySpendFile))
	if err != nil {
		return path, moveErr
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return path, moveErr
	}
	_, werr := f.Write(b)
	if err := f.Close(); werr != nil || err != nil {
		os.Remove(path)
	}
	return path, moveErr
}

// usdcToMicro converts whole USDC (e.g. 25.00) to microUSDC, rounding to the
// nearest micro-unit. A non-positive value (including the "unlimited" 0) yields 0.
func usdcToMicro(usdc float64) uint64 {
	if usdc <= 0 {
		return 0
	}
	return uint64(math.Round(usdc * 1_000_000))
}
