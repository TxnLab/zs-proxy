/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package paths resolves the per-OS state, config, and log locations for the
// zerosignal CLI from stdlib os.UserConfigDir / os.UserCacheDir, so the
// layout is correct on macOS, Linux, and Windows without hardcoding.
//
//	macOS:    ~/Library/Application Support/zerosignal/   (state, keystore, config.yaml)
//	          ~/Library/Caches/zerosignal/logs/
//	Linux:    ~/.config/zerosignal/    ~/.cache/zerosignal/logs/
//	Windows:  %AppData%\zerosignal\    %LocalAppData%\zerosignal\logs\
//
// ZEROSIGNAL_HOME (wallet.HomeEnv) replaces both with one directory, holding
// its logs under logs/, so a second proxy keeps all of its state apart.
package paths

import (
	"os"
	"path/filepath"

	"github.com/TxnLab/zerosignal/go/wallet"
)

// appDir is the per-OS subdirectory the CLI owns under the config/cache roots.
const appDir = "zerosignal"

// installMarkerPrefix names the file scripts/install.sh writes to record that it
// installed a binary, and where. One file per product, because zs-proxy and
// zs-node share the single zerosignal brand directory.
const installMarkerPrefix = "install-source-"

// StateDir is where config.yaml, the wallet metadata, and the pidfile live
// (os.UserConfigDir/zerosignal, or ZEROSIGNAL_HOME).
func StateDir() (string, error) {
	return wallet.HomeDir()
}

// OtherHome reports whether ZEROSIGNAL_HOME names a home other than the
// default. The OS service belongs to the default home, so a proxy run in
// another must neither install nor drive it: it would start or stop the
// user's own proxy. A variable naming the default dir is the default home, as
// the wallet's keychain item already treats it; one that can't be resolved
// counts as another, so the service stays out of reach.
func OtherHome() bool {
	if os.Getenv(wallet.HomeEnv) == "" {
		return false
	}
	home, herr := wallet.HomeDir()
	def, derr := wallet.DefaultDir()
	return herr != nil || derr != nil || filepath.Clean(home) != filepath.Clean(def)
}

// LogDir is where the daemon logfile lives (os.UserCacheDir/zerosignal/logs,
// or ZEROSIGNAL_HOME/logs).
func LogDir() (string, error) {
	if OtherHome() {
		home, err := wallet.HomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "logs"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDir, "logs"), nil
}

// InstallMarkerFile is where scripts/install.sh records that it installed
// product, and to which path. It lives under the STATE dir rather than the
// cache dir on purpose: clearing a cache must not silently relabel how the
// binary was installed. Absent for every other install channel. It records the
// binary, not a home, so it stays in the default state dir under any home.
func InstallMarkerFile(product string) (string, error) {
	dir, err := wallet.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, installMarkerPrefix+product), nil
}

// ConfigFile is the default config path, StateDir/config.yaml. It is not
// required to exist — a config-less run falls back to embedded defaults.
func ConfigFile() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}
