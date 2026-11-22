/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package paths

import (
	"path/filepath"
	"testing"

	"github.com/TxnLab/zerosignal/go/wallet"
)

// Another home moves the state and the logs, and leaves the install marker
// where the installer wrote it: the marker names the binary, which both homes
// run.
func TestOtherHome(t *testing.T) {
	t.Setenv(wallet.HomeEnv, "")
	defState, _ := StateDir()
	defLogs, _ := LogDir()
	defMarker, _ := InstallMarkerFile("zs-proxy")
	if OtherHome() {
		t.Fatal("OtherHome with the variable empty")
	}

	home := t.TempDir()
	t.Setenv(wallet.HomeEnv, home)
	if !OtherHome() {
		t.Fatal("OtherHome false with the variable set")
	}
	for _, tc := range []struct {
		name      string
		fn        func() (string, error)
		want, def string
	}{
		{"StateDir", StateDir, home, defState},
		{"LogDir", LogDir, filepath.Join(home, "logs"), defLogs},
		{"ConfigFile", ConfigFile, filepath.Join(home, "config.yaml"), ""},
	} {
		got, err := tc.fn()
		if err != nil || got != tc.want || got == tc.def {
			t.Errorf("%s = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if got, _ := InstallMarkerFile("zs-proxy"); got != defMarker {
		t.Errorf("InstallMarkerFile = %q under another home, want the default %q", got, defMarker)
	}

	// The variable naming the default dir is the default home.
	t.Setenv(wallet.HomeEnv, defState+string(filepath.Separator))
	if OtherHome() {
		t.Error("OtherHome true for the default dir")
	}
	if got, _ := LogDir(); got != defLogs {
		t.Errorf("LogDir = %q for the default dir, want %q", got, defLogs)
	}
}
