/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package update surfaces a best-effort "a newer release is available" notice.
// It queries txnlab/zs-proxy's latest GitHub Release (where goreleaser publishes
// the signed binaries), compares the latest vX.Y.Z tag against the running
// build.Version, caches the answer on disk with a 24h TTL so at most one command
// per day touches the network, and tailors the upgrade hint to how the binary
// was installed (Homebrew / Scoop / scripts/install.sh / direct download).
//
// Everything here is fail-silent: no network, a rate-limit, a parse error, or a
// source/dev build all degrade to "print nothing". The notice must never delay
// (bounded by the caller's context) or break a command.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TxnLab/zs-proxy/internal/build"
	"github.com/TxnLab/zs-proxy/internal/paths"
)

const (
	// releaseOwner/releaseRepo is the PUBLIC repo goreleaser publishes to.
	releaseOwner = "txnlab"
	releaseRepo  = "zs-proxy"

	latestReleaseURL = "https://api.github.com/repos/" + releaseOwner + "/" + releaseRepo + "/releases/latest"
	releasesPageURL  = "https://github.com/" + releaseOwner + "/" + releaseRepo + "/releases"

	// productName is the binary scripts/install.sh installs, and the suffix of
	// the marker it leaves behind.
	productName = "zs-proxy"

	// installScriptCmd is only correct because the script is idempotent and
	// re-installs into the directory it recorded last time — re-running it
	// upgrades in place rather than leaving a second copy on PATH.
	installScriptCmd = "curl -fsSL https://zerosignal.ai/install.sh | sh"

	// checkTTL bounds how often we hit the network — at most one request per
	// window, enforced by the on-disk cache timestamp.
	checkTTL = 24 * time.Hour

	// fetchTimeout is a floor on the single GET so the notice can't stall a
	// command even if the caller passes a longer-lived context.
	fetchTimeout = 2 * time.Second

	cacheFileName = "update-check.json"
)

// Indirected for tests: a deterministic fetcher / clock / state dir keeps the
// unit tests hermetic (no network, no real state-dir writes).
var (
	fetcher      = fetchLatest
	nowFn        = time.Now
	stateDirFn   = paths.StateDir
	markerFileFn = paths.InstallMarkerFile
	executableFn = os.Executable
)

// Notify prints a one-line "newer version available" notice to w when a newer
// release exists, tailored to how this binary was installed. Fully best-effort:
// a source/dev build, an unresolved latest version, or an already-current build
// all print nothing. current is the running build.Version.
func Notify(ctx context.Context, w io.Writer, current string) {
	if current == "" || current == "dev" {
		return
	}
	latest, ok := latestVersion(ctx)
	if !ok || compareVersions(current, latest) >= 0 {
		return
	}
	// Print the canonicalized latest, never the raw fetched tag, so a crafted
	// remote tag can't carry a pre-release suffix or terminal control bytes to
	// the TTY. `current` is a trusted local build value, printed as-is.
	label, hint := upgradeHint(installSource())
	fmt.Fprintf(w, "⚠ zs-proxy %s available (you have %s)\n", canonicalVersion(latest), current)
	fmt.Fprintf(w, "  %s: %s\n", label, hint)
}

// latestVersion resolves the latest published version, serving from the on-disk
// cache while it's fresh (< checkTTL) and only otherwise hitting the network. A
// failed fetch falls back to any cached value so a transient outage still
// surfaces a previously-seen newer release. ok is false when nothing resolved.
func latestVersion(ctx context.Context) (string, bool) {
	cached, haveCache := readCache()
	if haveCache && cached.LatestVersion != "" && nowFn().Sub(cached.CheckedAt) < checkTTL {
		return cached.LatestVersion, true
	}
	latest, err := fetcher(ctx)
	if err != nil {
		if haveCache && cached.LatestVersion != "" {
			// Back off a full TTL even on failure, so a persistent outage can't
			// make every command re-attempt the (bounded) fetch — the "at most
			// once a day" property must hold on the sad path too. Keep serving
			// the last-known version so the notice still appears.
			writeCache(cache{CheckedAt: nowFn(), LatestVersion: cached.LatestVersion})
			return cached.LatestVersion, true
		}
		return "", false
	}
	writeCache(cache{CheckedAt: nowFn(), LatestVersion: latest})
	return latest, true
}

// fetchLatest GETs the latest published release tag and returns it normalized to
// "X.Y.Z" (leading "v" stripped). GitHub's API 403s a request with no
// User-Agent, so one is always set.
func fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "zs-proxy/"+build.Version)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: fetchTimeout}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, res.Body)
		return "", fmt.Errorf("GET %s: status %d", latestReleaseURL, res.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(body.TagName), "v")
	if v == "" {
		return "", fmt.Errorf("empty tag_name from %s", latestReleaseURL)
	}
	return v, nil
}

// compareVersions compares two dotted numeric versions, returning -1 if a<b, 0
// if equal, +1 if a>b. Any pre-release/build suffix (after the first '-' or '+')
// is trimmed first; if either side doesn't parse it returns 0 ("no newer" — the
// safe default, so the caller stays silent).
func compareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0
	}
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // drop pre-release / build metadata
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// canonicalVersion renders a version back to a clean "X.Y.Z" built from its
// parsed integer components — stripping any pre-release/build suffix and any
// stray bytes — so remotely-fetched input never reaches the terminal verbatim.
// Returns "" when v doesn't parse; callers only print it after compareVersions
// has already proven it parses.
func canonicalVersion(v string) string {
	p, ok := parseVersion(v)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", p[0], p[1], p[2])
}

// cache is the on-disk memo of the last successful (or last-known) check.
type cache struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
}

func cachePath() (string, error) {
	dir, err := stateDirFn()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheFileName), nil
}

func readCache() (cache, bool) {
	var c cache
	p, err := cachePath()
	if err != nil {
		return c, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, false
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, false
	}
	return c, true
}

// writeCache is best-effort — a failed write just means we re-check next time.
func writeCache(c cache) {
	p, err := cachePath()
	if err != nil {
		return
	}
	if b, err := json.Marshal(c); err == nil {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, b, 0o600)
	}
}

// source is how this binary was most likely installed.
type source int

const (
	sourceDirect source = iota
	sourceBrew
	sourceScoop
	sourceScript
)

// installSource guesses the install channel from this binary's own path.
func installSource() source {
	exe, err := executableFn()
	if err != nil {
		return sourceDirect
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved // casks symlink into <prefix>/bin → resolve to the Caskroom
	}
	// The marker records an exact path, so it outranks classifyPath's substring
	// guess — which cannot tell a real Scoop install from a Linux user whose
	// $HOME happens to be /home/scoop.
	if scriptInstalled(exe) {
		return sourceScript
	}
	return classifyPath(exe)
}

// maxMarkerBytes caps the marker read. The file is three short lines; anything
// larger is not ours and is not worth scanning.
const maxMarkerBytes = 4096

// scriptInstalled reports whether scripts/install.sh installed the binary at
// exe. Fail-silent in every direction: an unreadable, oversized, missing, or
// stale marker just means "some other channel", which is the pre-existing
// behavior.
func scriptInstalled(exe string) bool {
	p, err := markerFileFn(productName)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	if err != nil || info.Size() > maxMarkerBytes {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	return markerMatches(string(b), exe)
}

// markerMatches reports whether the marker's contents describe THIS binary. The
// path check is what makes a stale marker harmless: a script install that was
// later replaced by a cask (or simply deleted) leaves the marker behind, and
// without this it would relabel an unrelated binary as script-installed and
// hand the user an upgrade command that installs a second copy elsewhere.
//
// Both sides are symlink-resolved before comparison, and both must be: the
// caller resolves os.Executable() while the marker holds whatever path the
// installer wrote, and resolving only one turns /var/… vs /private/var/… into a
// spurious mismatch.
func markerMatches(content, exe string) bool {
	var sawSource, sawBin bool
	for line := range strings.SplitSeq(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "source":
			sawSource = value == "script"
		case "bin":
			sawBin = value != "" && resolvePath(value) == resolvePath(exe)
		}
	}
	return sawSource && sawBin
}

// resolvePath resolves symlinks where it can, and otherwise hands back what it
// was given — a path that no longer exists still compares fine against itself.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func classifyPath(p string) source {
	// Normalize both separators regardless of the host OS (filepath.ToSlash is a
	// no-op off Windows) so a captured path classifies the same way anywhere.
	lower := strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	switch {
	case strings.Contains(lower, "/caskroom/"),
		strings.Contains(lower, "/cellar/"),
		strings.Contains(lower, "/homebrew/"):
		return sourceBrew
	// Anchored on Scoop's own layout, not a bare "/scoop/": that would classify
	// /home/scoop/.local/bin/zs-proxy — a Linux user named scoop — as a Scoop
	// install and tell them to run a Windows package manager.
	case strings.Contains(lower, "/scoop/apps/"),
		strings.Contains(lower, "/scoop/shims/"):
		return sourceScoop
	default:
		return sourceDirect
	}
}

// upgradeHint returns the imperative label and the command/URL for an install
// source. The label is chosen here (at the source) rather than re-derived from
// the value's shape by the caller.
func upgradeHint(src source) (label, hint string) {
	switch src {
	case sourceBrew:
		return "upgrade", "brew upgrade " + productName
	case sourceScoop:
		return "upgrade", "scoop update " + productName
	case sourceScript:
		return "upgrade", installScriptCmd
	default:
		return "download", releasesPageURL
	}
}
