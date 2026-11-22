/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"os"
	"path/filepath"
)

// homeJoin joins parts onto the user's home directory.
func homeJoin(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// xdgConfig joins parts onto the XDG config root ($XDG_CONFIG_HOME or ~/.config).
// Several tools (opencode, continue, openclaw) hardcode ~/.config on every OS,
// so this deliberately uses ~/.config rather than os.UserConfigDir() (which is
// ~/Library/Application Support on macOS).
func xdgConfig(parts ...string) (string, error) {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}

// fileExists reports whether path exists and is a regular-ish file (not a dir).
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
