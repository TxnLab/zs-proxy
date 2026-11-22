/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package wallet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/TxnLab/zerosignal/go/wallet"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

// The TUI and other SDK callers find the proxy's wallet because the SDK's
// default directory is the proxy's state dir.
func TestStateDirIsTheSDKDefault(t *testing.T) {
	state, err := paths.StateDir()
	if err != nil {
		t.Skip(err)
	}
	def, err := sdk.DefaultDir()
	if err != nil || def != state {
		t.Errorf("sdk.DefaultDir = %q, %v; proxy state dir %q", def, err, state)
	}
}

// The build stamp is the default and the env var an override, so a user told
// to set ZEROSIGNAL_KEYRING_BACKEND reaches past a recorded backend.
func TestOptions(t *testing.T) {
	prev := build.KeyringBackend
	t.Cleanup(func() { build.KeyringBackend = prev })
	build.KeyringBackend = "os"
	t.Setenv(sdk.EnvBackend, "file")
	o := options()
	if o.Backend != "os" || o.Override != "file" || o.Passphrase == nil {
		t.Errorf("options = backend %q, override %q, passphrase set %v", o.Backend, o.Override, o.Passphrase != nil)
	}
}

func TestErrExistsIsTheSDKs(t *testing.T) {
	if !errors.Is(ErrExists, sdk.ErrExists) {
		t.Error("ErrExists does not wrap the SDK's")
	}
}

// With no wallet, the backend the build's name resolves to decides; for an
// unstamped build that is the encrypted file.
func TestDefaultBackend_FileWhenUnstamped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	t.Setenv(sdk.EnvBackend, "")
	prev := build.KeyringBackend
	t.Cleanup(func() { build.KeyringBackend = prev })
	build.KeyringBackend = ""
	if !UsesFileBackend() {
		t.Error("an unstamped build without an override is not on the encrypted file")
	}
	t.Setenv(sdk.EnvPassphrase, "x")
	if FileBackendNeedsPassphrase() {
		t.Error("prompts although the passphrase env var is set")
	}
}

// "os" names no backend: it is whichever opens here, the encrypted file where
// no keychain does. Asked for by the build stamp or by the override, the
// answer is the one a wallet would actually open on, or a daemon skips the
// passphrase prompt it needs before detaching.
func TestDefaultBackend_ResolvesOS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	prev := build.KeyringBackend
	t.Cleanup(func() { build.KeyringBackend = prev })

	for _, tc := range []struct{ stamp, override string }{{"os", ""}, {"", "os"}} {
		build.KeyringBackend = tc.stamp
		t.Setenv(sdk.EnvBackend, tc.override)
		w, err := OpenDefault()
		if err != nil {
			t.Skipf("no backend opens for %q here: %v", "os", err)
		}
		if got := defaultBackend(true); got != w.Backend() || got == sdk.BackendOS {
			t.Errorf("stamp %q override %q: defaultBackend = %q, the wallet opens on %q", tc.stamp, tc.override, got, w.Backend())
		}
	}
}

// A backend that does not open is still the answer when it is the one asked
// for: a wallet.json from before backends were recorded opens on the build's,
// and an override names its own. Neither is the encrypted file.
func TestDefaultBackend_UnopenableNamesStand(t *testing.T) {
	isolate := func() {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("AppData", t.TempDir())
		t.Setenv(sdk.EnvPassphrase, "")
		os.Unsetenv(sdk.EnvPassphrase)
	}
	prev := build.KeyringBackend
	t.Cleanup(func() { build.KeyringBackend = prev })
	const unopenable = "no-such-backend"

	isolate()
	build.KeyringBackend = unopenable
	t.Setenv(sdk.EnvBackend, "")
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "wallet.json"), []byte(`{"address":"ADDR","network":"mainnet"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultBackend(false); got != unopenable || UsesFileBackend() {
		t.Errorf("unrecorded wallet on a %q build = %q, file %v; want the build's backend", unopenable, got, UsesFileBackend())
	}

	isolate()
	build.KeyringBackend = ""
	t.Setenv(sdk.EnvBackend, unopenable)
	if got := defaultBackend(true); got != unopenable || FileBackendNeedsPassphrase() {
		t.Errorf("override %q = %q, needs passphrase %v; want the override", unopenable, got, FileBackendNeedsPassphrase())
	}
}

// Generate and Import return the SDK's ErrExists; the CLI adds its flag once.
func TestWithForceHint(t *testing.T) {
	got := WithForceHint(sdk.ErrExists)
	if !IsExists(got) || !strings.Contains(got.Error(), "--force") {
		t.Errorf("WithForceHint(sdk.ErrExists) = %v", got)
	}
	if again := WithForceHint(ErrExists); strings.Count(again.Error(), "--force") != 1 {
		t.Errorf("hinted twice: %v", again)
	}
	// The SDK's refusal over a phrase no wallet.json names: --force alone
	// would destroy it, so the hint names the restore.
	orphan := fmt.Errorf("%w: the keychain store holds a recovery phrase", sdk.ErrExists)
	if got := WithForceHint(orphan); !IsExists(got) || !strings.Contains(got.Error(), "wallet import --force") {
		t.Errorf("WithForceHint(orphan) = %v", got)
	}
	other := errors.New("x")
	if WithForceHint(other) != other {
		t.Error("an unrelated error was changed")
	}
}

// A wallet whose recorded backend does not open here can still be shown, and
// its backend is the recorded one, read without opening any store.
func TestRecordedBackendIsReadWithoutOpening(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv(sdk.EnvBackend, "")
	state, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	const unopenable = "no-such-backend"
	if err := os.WriteFile(filepath.Join(state, "wallet.json"),
		[]byte(`{"address":"ADDR","network":"mainnet","backend":"`+unopenable+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDefault(); err == nil {
		t.Fatal("the recorded backend opened; the test needs one that does not")
	}
	m, err := DefaultMetadata()
	if err != nil || m.Address != "ADDR" {
		t.Errorf("DefaultMetadata = %+v, %v", m, err)
	}
	if got := defaultBackend(true); got != unopenable {
		t.Errorf("defaultBackend = %q, want the recorded %q", got, unopenable)
	}
	// The override decides for this process, never for a service, which does
	// not inherit it.
	t.Setenv(sdk.EnvBackend, "file")
	t.Setenv(sdk.EnvPassphrase, "")
	os.Unsetenv(sdk.EnvPassphrase)
	if !FileBackendNeedsPassphrase() {
		t.Error("the override does not win over the recorded backend")
	}
	if UsesFileBackend() {
		t.Error("the service check followed the installing shell's override")
	}
}
