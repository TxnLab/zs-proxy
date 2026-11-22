/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.3.0", "1.4.0", -1},
		{"1.4.0", "1.3.0", 1},
		{"1.3.0", "1.3.0", 0},
		{"1.3.0", "1.3.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0},       // short form fills zeros
		{"1", "1.0.1", -1},        // short form fills zeros
		{"1.4.0-dev", "1.4.0", 0}, // pre-release suffix trimmed → equal core
		{"v1.4.0", "1.4.0", 0},    // leading v tolerated
		{"1.10.0", "1.9.0", 1},    // numeric, not lexicographic
		{"bogus", "1.0.0", 0},     // unparseable → "no newer"
		{"1.0.0", "bogus", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCanonicalVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.4.0", "1.4.0"},
		{"v1.4.0", "1.4.0"},
		{"1.4", "1.4.0"},                    // short form zero-filled
		{"1.5.0-rc1", "1.5.0"},              // pre-release suffix stripped
		{"1.5.0-\x1b[2Jmalicious", "1.5.0"}, // control bytes never reach the terminal
		{"bogus", ""},                       // unparseable → empty (caller won't print)
	}
	for _, c := range cases {
		if got := canonicalVersion(c.in); got != c.want {
			t.Errorf("canonicalVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifyPath(t *testing.T) {
	cases := []struct {
		path string
		want source
	}{
		{"/opt/homebrew/Caskroom/zs-proxy/1.3.0/zs-proxy", sourceBrew},
		{"/usr/local/Caskroom/zs-proxy/1.3.0/zs-proxy", sourceBrew},
		{"/usr/local/Cellar/zs-proxy/1.3.0/bin/zs-proxy", sourceBrew},
		{"/opt/homebrew/bin/zs-proxy", sourceBrew},
		{`C:\Users\me\scoop\apps\zs-proxy\current\zs-proxy.exe`, sourceScoop},
		{`C:\Users\me\scoop\shims\zs-proxy.exe`, sourceScoop},
		{"/home/me/.local/bin/zs-proxy", sourceDirect},
		{"/usr/local/bin/zs-proxy", sourceDirect},
		// A Linux user named "scoop" is not a Scoop install.
		{"/home/scoop/.local/bin/zs-proxy", sourceDirect},
	}
	for _, c := range cases {
		if got := classifyPath(c.path); got != c.want {
			t.Errorf("classifyPath(%q) = %d, want %d", c.path, got, c.want)
		}
	}
}

func TestUpgradeHint(t *testing.T) {
	if label, hint := upgradeHint(sourceBrew); label != "upgrade" || hint != "brew upgrade zs-proxy" {
		t.Errorf("brew hint = (%q, %q)", label, hint)
	}
	if label, hint := upgradeHint(sourceScoop); label != "upgrade" || hint != "scoop update zs-proxy" {
		t.Errorf("scoop hint = (%q, %q)", label, hint)
	}
	if label, hint := upgradeHint(sourceScript); label != "upgrade" || hint != "curl -fsSL https://zerosignal.ai/install.sh | sh" {
		t.Errorf("script hint = (%q, %q)", label, hint)
	}
	if label, hint := upgradeHint(sourceDirect); label != "download" || !strings.HasPrefix(hint, "https://github.com/") {
		t.Errorf("direct hint = (%q, %q)", label, hint)
	}
}

func TestMarkerMatches(t *testing.T) {
	const exe = "/usr/local/bin/zs-proxy"
	// The exact bytes scripts/install.sh writes; scripts/install_test.go asserts
	// the script still produces them.
	good := "source=script\nbin=" + exe + "\nversion=0.16.1\n"

	cases := []struct {
		name    string
		content string
		exe     string
		want    bool
	}{
		{"marker for this binary", good, exe, true},
		{"no trailing newline", "source=script\nbin=" + exe, exe, true},
		{"stale marker for another path", good, "/home/me/.local/bin/zs-proxy", false},
		{"another channel's binary", good, "/opt/homebrew/bin/zs-proxy", false},
		{"missing bin line", "source=script\n", exe, false},
		{"missing source line", "bin=" + exe + "\n", exe, false},
		{"different source value", "source=cask\nbin=" + exe + "\n", exe, false},
		{"empty", "", exe, false},
		{"junk", "not a marker at all", exe, false},
	}
	for _, c := range cases {
		if got := markerMatches(c.content, c.exe); got != c.want {
			t.Errorf("%s: markerMatches() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScriptInstalled(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "zs-proxy")
	marker := filepath.Join(dir, "install-source-zs-proxy")

	orig := markerFileFn
	markerFileFn = func(string) (string, error) { return marker, nil }
	t.Cleanup(func() { markerFileFn = orig })

	if scriptInstalled(exe) {
		t.Error("no marker on disk must not report a script install")
	}

	if err := os.WriteFile(marker, []byte("source=script\nbin="+exe+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !scriptInstalled(exe) {
		t.Error("a marker naming this binary must report a script install")
	}

	// An oversized file is not ours; refusing to scan it is the fail-silent
	// path. The fixture has to be an OTHERWISE-VALID marker padded past the
	// cap, or it is rejected on shape and the size check is decorative.
	padded := "source=script\nbin=" + exe + "\n# " + strings.Repeat("x", maxMarkerBytes) + "\n"
	if err := os.WriteFile(marker, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}
	if scriptInstalled(exe) {
		t.Error("an oversized marker must not report a script install")
	}

	// ...and the boundary itself is inclusive: exactly maxMarkerBytes is ours.
	body := "source=script\nbin=" + exe + "\n"
	atCap := body + "# " + strings.Repeat("x", maxMarkerBytes-len(body)-3) + "\n"
	if len(atCap) != maxMarkerBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(atCap), maxMarkerBytes)
	}
	if err := os.WriteFile(marker, []byte(atCap), 0o600); err != nil {
		t.Fatal(err)
	}
	if !scriptInstalled(exe) {
		t.Error("a marker exactly at the cap must still be read")
	}
}

// TestInstallSource_MarkerOutranksPathGuess drives the real entry point. The
// two facts (classifyPath says brew, the marker says script) are independently
// true; only calling installSource asserts which one wins, and getting that
// backwards tells a script-installed user to `brew upgrade`, leaving them with
// a second copy.
func TestInstallSource_MarkerOutranksPathGuess(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "homebrew", "bin", "zs-proxy")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := classifyPath(exe); got != sourceBrew {
		t.Fatalf("precondition: classifyPath(%q) = %d, want sourceBrew", exe, got)
	}

	marker := filepath.Join(dir, "install-source-zs-proxy")
	origMarker, origExe := markerFileFn, executableFn
	markerFileFn = func(string) (string, error) { return marker, nil }
	executableFn = func() (string, error) { return exe, nil }
	t.Cleanup(func() { markerFileFn, executableFn = origMarker, origExe })

	// No marker: the path guess is all there is.
	if got := installSource(); got != sourceBrew {
		t.Errorf("without a marker, installSource() = %d, want sourceBrew", got)
	}

	if err := os.WriteFile(marker, []byte("source=script\nbin="+exe+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := installSource(); got != sourceScript {
		t.Errorf("installSource() = %d, want sourceScript (the marker must outrank classifyPath)", got)
	}
	if _, hint := upgradeHint(installSource()); !strings.Contains(hint, "install.sh") {
		t.Errorf("script install was told to upgrade with %q", hint)
	}
}

// TestMarkerMatches_ResolvesBothSides pins the symmetry explicitly. It is
// otherwise asserted only by accident of macOS putting t.TempDir() under
// /var → /private/var; on Linux CI, where /tmp is real, dropping the
// resolution from either side is a no-op.
func TestMarkerMatches_ResolvesBothSides(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	exe := filepath.Join(real, "zs-proxy")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, "zs-proxy")

	if !markerMatches("source=script\nbin="+linked+"\n", exe) {
		t.Error("a marker recorded through a symlink must match the resolved binary")
	}
	if !markerMatches("source=script\nbin="+exe+"\n", linked) {
		t.Error("a resolved marker must match a binary reached through a symlink")
	}
}

// withStubs points the package indirection at a temp state dir + fixed clock and
// a caller-supplied fetcher, restoring the originals on cleanup.
func withStubs(t *testing.T, fetch func(context.Context) (string, error)) {
	t.Helper()
	dir := t.TempDir()
	origFetcher, origNow, origState := fetcher, nowFn, stateDirFn
	fetcher = fetch
	nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	stateDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() {
		fetcher, nowFn, stateDirFn = origFetcher, origNow, origState
	})
}

func TestNotify_NewerAvailable(t *testing.T) {
	withStubs(t, func(context.Context) (string, error) { return "1.4.0", nil })
	var buf strings.Builder
	Notify(context.Background(), &buf, "1.3.0")
	out := buf.String()
	if !strings.Contains(out, "1.4.0 available") || !strings.Contains(out, "1.3.0") {
		t.Errorf("expected a newer-version notice, got %q", out)
	}
}

func TestNotify_AlreadyCurrent(t *testing.T) {
	withStubs(t, func(context.Context) (string, error) { return "1.4.0", nil })
	var buf strings.Builder
	Notify(context.Background(), &buf, "1.4.0")
	if buf.Len() != 0 {
		t.Errorf("expected no notice when current, got %q", buf.String())
	}
}

func TestNotify_DevBuildSkips(t *testing.T) {
	called := false
	withStubs(t, func(context.Context) (string, error) { called = true; return "1.4.0", nil })
	var buf strings.Builder
	Notify(context.Background(), &buf, "dev")
	if buf.Len() != 0 || called {
		t.Errorf("dev build must not fetch or notify (called=%v, out=%q)", called, buf.String())
	}
}

func TestNotify_FetchErrorNoCacheSilent(t *testing.T) {
	withStubs(t, func(context.Context) (string, error) { return "", errors.New("network down") })
	var buf strings.Builder
	Notify(context.Background(), &buf, "1.3.0")
	if buf.Len() != 0 {
		t.Errorf("expected silence on fetch error with no cache, got %q", buf.String())
	}
}

func TestLatestVersion_CachesAndAvoidsSecondFetch(t *testing.T) {
	calls := 0
	withStubs(t, func(context.Context) (string, error) { calls++; return "1.4.0", nil })

	if v, ok := latestVersion(context.Background()); !ok || v != "1.4.0" {
		t.Fatalf("first latestVersion = (%q, %v)", v, ok)
	}
	// Second call within TTL must serve from the cache written by the first.
	if v, ok := latestVersion(context.Background()); !ok || v != "1.4.0" {
		t.Fatalf("second latestVersion = (%q, %v)", v, ok)
	}
	if calls != 1 {
		t.Errorf("expected exactly one network fetch (cache hit on the second), got %d", calls)
	}
}

func TestLatestVersion_FetchErrorFallsBackToCache(t *testing.T) {
	// Prime the cache with a good fetch, then make the next fetch fail *and*
	// force it to be attempted by expiring the TTL via an advanced clock.
	dir := t.TempDir()
	origFetcher, origNow, origState := fetcher, nowFn, stateDirFn
	stateDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() { fetcher, nowFn, stateDirFn = origFetcher, origNow, origState })

	fetcher = func(context.Context) (string, error) { return "1.4.0", nil }
	nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	if v, ok := latestVersion(context.Background()); !ok || v != "1.4.0" {
		t.Fatalf("prime latestVersion = (%q, %v)", v, ok)
	}

	fetcher = func(context.Context) (string, error) { return "", errors.New("down") }
	nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).Add(2 * checkTTL) } // TTL expired
	if v, ok := latestVersion(context.Background()); !ok || v != "1.4.0" {
		t.Errorf("expected fallback to cached 1.4.0 on fetch error, got (%q, %v)", v, ok)
	}
}
