/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package scripts is test-only: it exercises install.sh, the POSIX shell
// installer published behind https://zerosignal.ai/install.sh, against a fake
// GitHub release. A test-only package matched through a WILDCARD is silently
// ignored by `go build`/`go install` (see Go's own
// cmd/go/testdata/script/build_test_only.txt), and .github/workflows/ci.yml
// only ever uses `go build ./...` / `go vet ./...`. Naming this package
// explicitly in a build step would break it.
//
// The script is what turns a release archive into a working install, so the
// assertions here are about the things a silent regression would break: which
// asset gets fetched for a given OS/arch, that a bad checksum installs nothing,
// and that the paths it records are the ones the Go side reads back.
package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/TxnLab/zs-proxy/internal/paths"
)

// The script under test is run as a child process, so the Go test cache — which
// hashes the files the TEST process opens — never sees it. Editing install.sh
// and re-running would otherwise report `ok (cached)` on a script nothing
// executed. Embedding it puts the file in the cache key.
//
//go:embed install.sh
var installScriptSource string

const (
	testVersion = "9.8.7"
	testProduct = "zs-proxy"
)

// basePATH deliberately excludes Homebrew and any other user prefix: a
// developer running these tests almost certainly has a real zs-proxy on their
// PATH, which the script's own brew guard would (correctly) refuse to shadow.
const basePATH = "/usr/bin:/bin:/usr/sbin:/sbin"

// fakeRelease serves the two things install.sh fetches — the "latest release"
// API document and the release assets — and records what was asked for.
type fakeRelease struct {
	srv     *httptest.Server
	repo    string
	product string
	version string

	mu       sync.Mutex
	apiHits  int
	assets   map[string][]byte
	assetLog []string
	tag      string // what /releases/latest reports; overridable per test
}

// setTag makes the API report a different tag_name. Guarded rather than swapping
// srv.Config.Handler, which races the running server.
func (r *fakeRelease) setTag(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tag = tag
}

// newFakeRelease publishes archives for the platforms goreleaser really ships,
// with the real archive shape: flat, no wrapper directory, LICENSE + README.md
// alongside the binary. corruptChecksums serves digests that don't match.
func newFakeRelease(t *testing.T, product string, corruptChecksums bool) *fakeRelease {
	t.Helper()
	r := &fakeRelease{
		repo:    "txnlab/" + product,
		product: product,
		version: testVersion,
		assets:  map[string][]byte{},
		tag:     "v" + testVersion,
	}

	for _, p := range []struct{ os, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "all"},
	} {
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", product, testVersion, p.os, p.arch)
		r.assets[name] = buildArchive(t, product, p.os, p.arch)
	}

	var sums bytes.Buffer
	for name, body := range r.assets {
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		if corruptChecksums {
			digest = strings.Repeat("0", len(digest))
		}
		fmt.Fprintf(&sums, "%s  %s\n", digest, name)
	}
	r.assets["checksums.txt"] = sums.Bytes()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+r.repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.apiHits++
		tag := r.tag
		r.mu.Unlock()
		fmt.Fprintf(w, `{"url":"x","tag_name":%q,"name":"whatever"}`, tag)
	})
	prefix := "/" + r.repo + "/releases/download/v" + testVersion + "/"
	mux.HandleFunc(prefix, func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, prefix)
		r.mu.Lock()
		r.assetLog = append(r.assetLog, name)
		body, ok := r.assets[name]
		r.mu.Unlock()
		if !ok {
			// A wrong asset name must fail loudly rather than silently.
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	})

	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelease) hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apiHits
}

func (r *fakeRelease) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.assetLog...)
}

// requestedArchive returns the single .tar.gz name the run asked for.
func (r *fakeRelease) requestedArchive(t *testing.T) string {
	t.Helper()
	for _, n := range r.requested() {
		if strings.HasSuffix(n, ".tar.gz") {
			return n
		}
	}
	t.Fatalf("no archive was requested (asked for %v)", r.requested())
	return ""
}

// buildArchive mirrors goreleaser's archive layout. The "binary" is a shell
// script so the test can prove the real payload landed by running it.
func buildArchive(t *testing.T, product, goos, arch string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	add := func(name, body string, mode int64) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("LICENSE", "fake license\n", 0o644)
	add("README.md", "fake readme\n", 0o644)
	add(product, fmt.Sprintf("#!/bin/sh\necho '%s %s (%s/%s)'\n", product, testVersion, goos, arch), 0o755)

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// run invokes install.sh under shell with a fully constructed environment —
// never the inherited one, so a bug can't reach the developer's real HOME.
func run(t *testing.T, shell string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(shell, append([]string{"./install.sh"}, args...)...)
	cmd.Env = []string{}
	if _, ok := env["PATH"]; !ok {
		cmd.Env = append(cmd.Env, "PATH="+basePATH)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// baseEnv points the script at the fake release and a throwaway HOME.
func baseEnv(t *testing.T, r *fakeRelease, home, installDir string) map[string]string {
	t.Helper()
	return map[string]string{
		"HOME":           home,
		"ZS_REPO":        r.repo,
		"ZS_GITHUB":      r.srv.URL,
		"ZS_GITHUB_API":  r.srv.URL,
		"ZS_INSTALL_DIR": installDir,
	}
}

// forEachShell runs fn under every shell available here. On Ubuntu CI /bin/sh
// is dash; on a macOS laptop it is bash-3.2 in sh mode. That difference IS the
// portability risk this script carries, so both get exercised where present.
func forEachShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh installer; Windows installs with Scoop")
	}
	shells := []string{}
	for _, name := range []string{"sh", "dash", "bash"} {
		if p, err := exec.LookPath(name); err == nil {
			shells = append(shells, p)
		}
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell on PATH")
	}
	for _, shell := range shells {
		t.Run(filepath.Base(shell), func(t *testing.T) { fn(t, shell) })
	}
}

// TestInstallScript_IsTruncationSafe pins the structural rule that exists
// solely because this script is piped into a shell: everything is defined in a
// function and `main "$@"` is the last line, so a download cut short defines a
// partial set of functions and exits instead of executing half an installer.
// Nothing about that is observable from running the script successfully, which
// is exactly why it needs asserting here.
func TestInstallScript_IsTruncationSafe(t *testing.T) {
	if !strings.HasPrefix(installScriptSource, "#!/bin/sh\n") {
		t.Errorf("must be POSIX sh (it runs on stock Alpine/Debian), got %.20q", installScriptSource)
	}

	lines := strings.Split(strings.TrimRight(installScriptSource, "\n"), "\n")
	if last := lines[len(lines)-1]; last != `main "$@"` {
		t.Errorf("last line must be `main \"$@\"`, got %q", last)
	}

	// Every other statement must be inside a function or a top-level
	// assignment. A bare command at column 0 runs while the download is still
	// arriving.
	for i, line := range lines[:len(lines)-1] {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(line, " ") || strings.HasPrefix(line, "}") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "set "), strings.HasPrefix(line, "umask "):
		case strings.Contains(line, "() {"): // function definition
		case strings.Contains(line, "="): // top-level assignment
		default:
			t.Errorf("line %d executes at parse time, before the script is fully read: %q", i+1, line)
		}
	}
}

func TestInstall_HappyPath(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		home, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, home, dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		out, err := run(t, shell, env)
		if err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}
		if got := r.requestedArchive(t); got != testProduct+"_"+testVersion+"_linux_amd64.tar.gz" {
			t.Errorf("fetched %q", got)
		}
		if r.hits() != 1 {
			t.Errorf("expected exactly one API call, got %d", r.hits())
		}

		target := filepath.Join(dir, testProduct)
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("binary not installed: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("installed binary is not executable (mode %v)", info.Mode())
		}
		// Running it proves the archive payload landed, not a placeholder.
		got, err := exec.Command(target).CombinedOutput()
		if err != nil {
			t.Fatalf("installed binary did not run: %v\n%s", err, got)
		}
		if !strings.Contains(string(got), testVersion) {
			t.Errorf("installed binary printed %q", got)
		}
	})
}

func TestInstall_ArchAliases(t *testing.T) {
	cases := []struct{ uname, want string }{
		{"x86_64", "amd64"},
		{"amd64", "amd64"},
		{"aarch64", "arm64"},
		{"arm64", "arm64"},
	}
	forEachShell(t, func(t *testing.T, shell string) {
		for _, c := range cases {
			r := newFakeRelease(t, testProduct, false)
			env := baseEnv(t, r, t.TempDir(), filepath.Join(t.TempDir(), "bin"))
			env["ZS_OS"], env["ZS_ARCH"] = "linux", c.uname
			if out, err := run(t, shell, env); err != nil {
				t.Fatalf("uname -m %q: %v\n%s", c.uname, err, out)
			}
			want := fmt.Sprintf("%s_%s_linux_%s.tar.gz", testProduct, testVersion, c.want)
			if got := r.requestedArchive(t); got != want {
				t.Errorf("uname -m %q fetched %q, want %q", c.uname, got, want)
			}
		}
	})
}

// TestInstall_DarwinIsUniversal pins the fact most likely to be broken by a
// later edit: goreleaser fuses both darwin slices into ONE universal archive,
// so there is no darwin_amd64 / darwin_arm64 asset to ask for.
func TestInstall_DarwinIsUniversal(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		for _, arch := range []string{"arm64", "x86_64"} {
			r := newFakeRelease(t, testProduct, false)
			env := baseEnv(t, r, t.TempDir(), filepath.Join(t.TempDir(), "bin"))
			env["ZS_OS"], env["ZS_ARCH"] = "darwin", arch
			if out, err := run(t, shell, env); err != nil {
				t.Fatalf("darwin/%s: %v\n%s", arch, err, out)
			}
			want := testProduct + "_" + testVersion + "_darwin_all.tar.gz"
			if got := r.requestedArchive(t); got != want {
				t.Errorf("darwin/%s fetched %q, want %q", arch, got, want)
			}
		}
	})
}

func TestInstall_ChecksumMismatchInstallsNothing(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, true) // corrupt digests
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		// A `tar` shim records whether extraction was ever attempted. The
		// install dir alone can't see this: an extract-before-verify script
		// unpacks into its own temp dir and then deletes it on the way out, so
		// "nothing installed" holds either way while unverified bytes have
		// already been written to disk.
		shimDir, tarLog := tarRecordingPATH(t)
		env["PATH"] = shimDir + ":" + basePATH

		out, err := run(t, shell, env)
		if err == nil {
			t.Fatalf("a checksum mismatch must fail, got success:\n%s", out)
		}
		if !strings.Contains(out, "checksum mismatch") {
			t.Errorf("expected a checksum error, got:\n%s", out)
		}
		assertNothingInstalled(t, dir)
		if _, err := os.Stat(tarLog); err == nil {
			t.Error("unverified bytes were extracted: verification must run before tar")
		}
	})
}

// tarRecordingPATH returns a PATH entry whose `tar` touches a sentinel before
// delegating to the real one, plus that sentinel's path.
func tarRecordingPATH(t *testing.T) (dir, sentinel string) {
	t.Helper()
	realTar, err := lookPathIn(basePATH, "tar")
	if err != nil {
		t.Skip("no tar on the test PATH")
	}
	dir = t.TempDir()
	sentinel = filepath.Join(t.TempDir(), "tar-was-run")
	shim := fmt.Sprintf("#!/bin/sh\n: > %q\nexec %q \"$@\"\n", sentinel, realTar)
	if err := os.WriteFile(filepath.Join(dir, "tar"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, sentinel
}

func TestInstall_AssetMissingFromChecksums(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		// A checksums.txt that lists some other release's assets: the digest
		// lookup is by filename, so ours must not be satisfied by a stray line.
		r.mu.Lock()
		r.assets["checksums.txt"] = []byte(strings.Repeat("0", 64) + "  some-other-file.tar.gz\n")
		r.mu.Unlock()

		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		out, err := run(t, shell, env)
		if err == nil {
			t.Fatalf("an unlisted asset must fail, got success:\n%s", out)
		}
		if !strings.Contains(out, "not listed in checksums.txt") {
			t.Errorf("expected an unlisted-asset error, got:\n%s", out)
		}
		assertNothingInstalled(t, dir)
	})
}

func TestInstall_PinnedVersionSkipsTheAPI(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		for _, pin := range []string{testVersion, "v" + testVersion} {
			r := newFakeRelease(t, testProduct, false)
			env := baseEnv(t, r, t.TempDir(), filepath.Join(t.TempDir(), "bin"))
			env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
			env["ZS_VERSION"] = pin

			if out, err := run(t, shell, env); err != nil {
				t.Fatalf("ZS_VERSION=%s: %v\n%s", pin, err, out)
			}
			if r.hits() != 0 {
				t.Errorf("ZS_VERSION=%s still called the API %d time(s)", pin, r.hits())
			}
			want := testProduct + "_" + testVersion + "_linux_amd64.tar.gz"
			if got := r.requestedArchive(t); got != want {
				t.Errorf("ZS_VERSION=%s fetched %q, want %q", pin, got, want)
			}
		}
	})
}

// TestInstall_HostileTagRejected covers the one place remote input reaches a
// URL and a filename. It asserts the REASON, not just a non-zero exit: every
// one of these tags also happens to 404, so "the install failed" is satisfied
// whether or not the guard exists. Without it, `../../pwned` becomes
// `curl -o "$tmp/zs-proxy_../../pwned_linux_amd64.tar.gz"` — an arbitrary write
// two directories above the temp dir on any 200 response.
func TestInstall_HostileTagRejected(t *testing.T) {
	tags := []string{
		"v1.0.0; touch /tmp/zs-pwned",
		"../../etc/passwd",
		"v1.0.0 --insecure",
		"9.8.7;x", // would otherwise 404 exactly like a real missing asset
		"9.8.7/../9.8.7",
	}
	forEachShell(t, func(t *testing.T, shell string) {
		for _, tag := range tags {
			r := newFakeRelease(t, testProduct, false)
			r.setTag(tag)

			dir := filepath.Join(t.TempDir(), "bin")
			env := baseEnv(t, r, t.TempDir(), dir)
			env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

			out, err := run(t, shell, env)
			if err == nil {
				t.Fatalf("tag %q must be refused, got success:\n%s", tag, out)
			}
			if !strings.Contains(out, "refusing an unexpected version string") {
				t.Errorf("tag %q must be refused by the sanitizer, not incidentally by a 404:\n%s", tag, out)
			}
			if len(r.requested()) != 0 {
				t.Errorf("tag %q reached the download stage: %v", tag, r.requested())
			}
			assertNothingInstalled(t, dir)
		}
	})
}

func TestInstall_UnsupportedArchMakesNoRequest(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "riscv64"

		out, err := run(t, shell, env)
		if err == nil {
			t.Fatalf("riscv64 must be refused, got success:\n%s", out)
		}
		if !strings.Contains(out, "riscv64") {
			t.Errorf("the error should name the architecture, got:\n%s", out)
		}
		if r.hits() != 0 || len(r.requested()) != 0 {
			t.Errorf("an unsupported platform must not touch the network")
		}
		assertNothingInstalled(t, dir)
	})
}

// TestInstall_NoDigestToolAborts: "verify only if a tool happens to exist" is
// worse than not verifying, because it is trivially arranged.
func TestInstall_NoDigestToolAborts(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		cmd := exec.Command(shell, "./install.sh")
		cmd.Env = []string{"PATH=" + shimPATHWithout(t, "sha256sum", "shasum", "openssl")}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("no digest tool must abort, got success:\n%s", out)
		}
		if !strings.Contains(string(out), "sha256") {
			t.Errorf("expected a digest-tool error, got:\n%s", out)
		}
		assertNothingInstalled(t, dir)
	})
}

// shimPATHWithout builds a PATH holding symlinks to the tools install.sh needs,
// minus the named ones.
func shimPATHWithout(t *testing.T, drop ...string) string {
	t.Helper()
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	dir := t.TempDir()
	// gzip/gunzip are here for `tar -xzf`: GNU tar (Linux, CI) forks the
	// external gzip binary, while macOS's libarchive tar decompresses
	// in-process — so omitting them passes locally and fails only in CI.
	for _, tool := range []string{
		"uname", "mktemp", "curl", "wget", "sed", "tr", "head", "awk", "rm",
		"tar", "gzip", "gunzip", "cp", "mv", "chmod", "mkdir", "basename",
		"dirname", "cat", "sha256sum", "shasum", "openssl",
	} {
		if dropped[tool] {
			continue
		}
		src, err := lookPathIn(basePATH, tool)
		if err != nil {
			continue // not every tool exists on every platform (wget, sha256sum)
		}
		if err := os.Symlink(src, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func lookPathIn(path, tool string) (string, error) {
	for dir := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		p := filepath.Join(dir, tool)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// TestInstall_RefusesToShadowHomebrew also pins the substrings shared with
// internal/update/update.go::classifyPath — the two read the same evidence and
// must agree about what a managed install looks like.
func TestInstall_RefusesToShadowHomebrew(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		brewDir := filepath.Join(t.TempDir(), "opt", "homebrew", "bin")
		if err := os.MkdirAll(brewDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(brewDir, testProduct), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		env["PATH"] = brewDir + ":" + basePATH

		out, err := run(t, shell, env)
		if err == nil {
			t.Fatalf("must refuse to shadow a Homebrew install, got success:\n%s", out)
		}
		if !strings.Contains(out, "brew upgrade") {
			t.Errorf("expected a brew-upgrade hint, got:\n%s", out)
		}
		if r.hits() != 0 {
			t.Errorf("the guard must run before any network call")
		}
		assertNothingInstalled(t, dir)

		env["ZS_FORCE"] = "1"
		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("ZS_FORCE=1 must install anyway: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, testProduct)); err != nil {
			t.Errorf("ZS_FORCE=1 did not install: %v", err)
		}
	})
}

// TestInstall_RefusesToShadowScoop is the shell twin of TestClassifyPath's
// scoop cases. The Go side's anchoring is tested; without this the shell half
// of the "these substrings stay in lockstep" claim is unasserted, so the two
// can drift with the suite green — which is the exact failure the claim exists
// to prevent.
func TestInstall_RefusesToShadowScoop(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		for _, layout := range []string{
			filepath.Join("scoop", "shims"),
			filepath.Join("scoop", "apps", testProduct, "current"),
		} {
			r := newFakeRelease(t, testProduct, false)
			scoopDir := filepath.Join(t.TempDir(), layout)
			if err := os.MkdirAll(scoopDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scoopDir, testProduct), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			dir := filepath.Join(t.TempDir(), "bin")
			env := baseEnv(t, r, t.TempDir(), dir)
			env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
			env["PATH"] = scoopDir + ":" + basePATH

			out, err := run(t, shell, env)
			if err == nil {
				t.Fatalf("%s: must refuse to shadow a Scoop install:\n%s", layout, out)
			}
			if !strings.Contains(out, "scoop update") {
				t.Errorf("%s: expected a scoop-update hint, got:\n%s", layout, out)
			}
			assertNothingInstalled(t, dir)
		}

		// The negative half: a Linux user whose home is /home/scoop is not a
		// Scoop install, and refusing them would be a hard block with no
		// package manager to point at.
		r := newFakeRelease(t, testProduct, false)
		homeBin := filepath.Join(t.TempDir(), "home", "scoop", ".local", "bin")
		if err := os.MkdirAll(homeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(homeBin, testProduct), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		env := baseEnv(t, r, t.TempDir(), homeBin)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
		env["PATH"] = homeBin + ":" + basePATH

		if out, err := run(t, shell, env); err != nil {
			t.Errorf("a user named scoop must not be blocked: %v\n%s", err, out)
		}
	})
}

// TestInstall_WritesMarkerWhereGoReadsIt binds the shell's reimplementation of
// os.UserConfigDir() to the Go one, rather than hardcoding either.
func TestInstall_WritesMarkerWhereGoReadsIt(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		home, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, home, dir)
		// The marker path is per-OS on both sides, so compare like for like.
		env["ZS_OS"], env["ZS_ARCH"] = runtime.GOOS, runtime.GOARCH

		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}

		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		want, err := paths.InstallMarkerFile(testProduct)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(want)
		if err != nil {
			t.Fatalf("no marker at the path the binary reads (%s): %v", want, err)
		}
		if !strings.Contains(string(body), "source=script") {
			t.Errorf("marker is missing source=script:\n%s", body)
		}
		// internal/update compares this against a symlink-resolved
		// os.Executable(), so the recorded path is resolved too — on macOS
		// t.TempDir() sits under /var, which is a symlink to /private/var.
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "bin="+filepath.Join(resolved, testProduct)) {
			t.Errorf("marker does not name the installed binary:\n%s", body)
		}
		// Both internal/update packages' TestMarkerMatches fixtures claim to be
		// "the exact bytes install.sh writes". This is what makes that true.
		if !strings.Contains(string(body), "version="+testVersion) {
			t.Errorf("marker does not record the installed version:\n%s", body)
		}
	})
}

// TestInstall_MarkerPathIsPerOS asserts BOTH branches of the script's
// os.UserConfigDir() reimplementation on every host. The test above can only
// ever exercise the one matching runtime.GOOS, so on a macOS laptop the Linux
// branch is unasserted and on Linux CI the macOS one is — each platform
// covering exactly the branch the other doesn't.
func TestInstall_MarkerPathIsPerOS(t *testing.T) {
	cases := []struct{ os, rel string }{
		{"darwin", "Library/Application Support/zerosignal"},
		{"linux", ".config/zerosignal"},
	}
	forEachShell(t, func(t *testing.T, shell string) {
		for _, c := range cases {
			r := newFakeRelease(t, testProduct, false)
			home := t.TempDir()
			env := baseEnv(t, r, home, filepath.Join(t.TempDir(), "bin"))
			env["ZS_OS"], env["ZS_ARCH"] = c.os, "x86_64"

			if out, err := run(t, shell, env); err != nil {
				t.Fatalf("%s: %v\n%s", c.os, err, out)
			}
			want := filepath.Join(home, c.rel, "install-source-"+testProduct)
			if _, err := os.Stat(want); err != nil {
				t.Errorf("%s: no marker at %s: %v", c.os, want, err)
			}
		}
	})
}

// TestInstall_NodeMarkerIsProductScoped is the one assertion tying this script
// to the node's reader: they derive the filename independently, in different
// repos, and a shared name would have a zs-node install clobber the proxy's
// marker while leaving the node's own lookup empty.
func TestInstall_NodeMarkerIsProductScoped(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, "zs-node", false)
		home, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, home, dir)
		env["ZS_OS"], env["ZS_ARCH"] = runtime.GOOS, runtime.GOARCH

		if out, err := run(t, shell, env, "zs-node"); err != nil {
			t.Fatalf("install failed: %v\n%s", err, out)
		}

		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		nodeMarker, err := paths.InstallMarkerFile("zs-node")
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(nodeMarker)
		if err != nil {
			t.Fatalf("no zs-node marker at %s: %v", nodeMarker, err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "bin="+filepath.Join(resolved, "zs-node")) {
			t.Errorf("zs-node marker does not name the installed binary:\n%s", body)
		}
		proxyMarker, err := paths.InstallMarkerFile(testProduct)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(proxyMarker); err == nil {
			t.Errorf("installing zs-node wrote the zs-proxy marker (%s)", proxyMarker)
		}
	})
}

// TestInstall_ReinstallIsSticky guards the coupling with `install-service`,
// which bakes the literal binary path into the launchd/systemd unit: an upgrade
// that lands somewhere new leaves the service running a binary nothing updates.
func TestInstall_ReinstallIsSticky(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		home, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, home, dir)
		env["ZS_OS"], env["ZS_ARCH"] = runtime.GOOS, runtime.GOARCH
		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("first install failed: %v\n%s", err, out)
		}

		// Re-run with no ZS_INSTALL_DIR: the recorded directory must win over
		// whatever the fallback ladder would have picked.
		delete(env, "ZS_INSTALL_DIR")
		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("re-install failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, testProduct)); err != nil {
			t.Errorf("re-install did not land in the recorded directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", testProduct)); err == nil {
			t.Errorf("re-install fell back to ~/.local/bin instead of the recorded directory")
		}

		// ...but stickiness must not outrank an explicit request to move the
		// install, or a user can never relocate it.
		moved := filepath.Join(t.TempDir(), "elsewhere")
		env["ZS_INSTALL_DIR"] = moved
		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("relocating install failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(moved, testProduct)); err != nil {
			t.Errorf("ZS_INSTALL_DIR must outrank the recorded directory: %v", err)
		}
	})
}

// TestInstall_UpgradeReplacesByRename pins the reason the script stages and
// renames instead of copying over the target: `rename(2)` unlinks the old
// inode, where a copy-in-place truncates and rewrites it — which fails with
// ETXTBSY on Linux when the old binary is still running.
func TestInstall_UpgradeReplacesByRename(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("first install failed: %v\n%s", err, out)
		}
		target := filepath.Join(dir, testProduct)
		before, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}

		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("second install failed: %v\n%s", err, out)
		}
		after, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(before, after) {
			t.Error("upgrade rewrote the existing file in place; it must rename a staged copy over it")
		}
	})
}

func TestInstall_UpgradeReplacesInPlace(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		home, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, home, dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		for i := range 2 {
			if out, err := run(t, shell, env); err != nil {
				t.Fatalf("run %d failed: %v\n%s", i+1, err, out)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, testProduct)); err != nil {
			t.Fatalf("binary missing after re-run: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("staging file left behind: %s", e.Name())
			}
		}
	})
}

func TestInstall_ProductSelection(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		// zs-node, positionally and via the environment.
		for _, viaEnv := range []bool{false, true} {
			r := newFakeRelease(t, "zs-node", false)
			dir := filepath.Join(t.TempDir(), "bin")
			env := baseEnv(t, r, t.TempDir(), dir)
			env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
			var args []string
			if viaEnv {
				env["ZS_PRODUCT"] = "zs-node"
			} else {
				args = []string{"zs-node"}
			}
			if out, err := run(t, shell, env, args...); err != nil {
				t.Fatalf("zs-node install failed: %v\n%s", err, out)
			}
			if got := r.requestedArchive(t); got != "zs-node_"+testVersion+"_linux_amd64.tar.gz" {
				t.Errorf("fetched %q", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "zs-node")); err != nil {
				t.Errorf("zs-node not installed: %v", err)
			}
		}

		// The documented form is `| sh -s -- zs-node`, so argv must win over an
		// inherited ZS_PRODUCT. With only one of the two ever set, both
		// orderings pass and the contract is unverified.
		r := newFakeRelease(t, "zs-node", false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
		env["ZS_PRODUCT"] = testProduct
		if out, err := run(t, shell, env, "zs-node"); err != nil {
			t.Fatalf("argv must override ZS_PRODUCT: %v\n%s", err, out)
		}
		if got := r.requestedArchive(t); !strings.HasPrefix(got, "zs-node_") {
			t.Errorf("argv did not win over ZS_PRODUCT: fetched %q", got)
		}

		// Anything else is refused before a URL or a path is built from it. An
		// EMPTY ZS_PRODUCT is deliberately absent: `${ZS_PRODUCT:-zs-proxy}`
		// treats empty as unset, which is the shell convention and the right
		// behavior for someone who exported the variable with no value.
		for _, bad := range []string{"../evil", "zs-proxy;rm -rf /", "zsproxy", "zs-proxy "} {
			r := newFakeRelease(t, testProduct, false)
			dir := filepath.Join(t.TempDir(), "bin")
			env := baseEnv(t, r, t.TempDir(), dir)
			env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
			env["ZS_PRODUCT"] = bad

			out, err := run(t, shell, env)
			if err == nil {
				t.Fatalf("product %q must be refused, got success:\n%s", bad, out)
			}
			if r.hits() != 0 || len(r.requested()) != 0 {
				t.Errorf("product %q reached the network", bad)
			}
			assertNothingInstalled(t, dir)
		}
	})
}

// TestInstall_BinaryModeChecksums: sha256sum writes `digest *name` in binary
// mode. goreleaser emits text mode today, so nothing else here exercises the
// `*` strip — and if the release tooling ever switches, every install would die
// with "not listed in checksums.txt".
func TestInstall_BinaryModeChecksums(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		r.mu.Lock()
		r.assets["checksums.txt"] = bytes.ReplaceAll(r.assets["checksums.txt"], []byte("  "), []byte(" *"))
		r.mu.Unlock()

		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"

		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("binary-mode checksums must verify: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, testProduct)); err != nil {
			t.Errorf("not installed: %v", err)
		}
	})
}

// TestInstall_DetectsThisHost is the only case that lets `uname` run. Every
// other test sets ZS_OS/ZS_ARCH to pin the asset matrix, which leaves the whole
// detection ladder — the thing a real user hits first — unexecuted.
func TestInstall_DetectsThisHost(t *testing.T) {
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir) // no ZS_OS / ZS_ARCH

		out, err := run(t, shell, env)
		if err != nil {
			t.Fatalf("detection failed on %s/%s: %v\n%s", runtime.GOOS, runtime.GOARCH, err, out)
		}
		want := fmt.Sprintf("%s_%s_%s_%s.tar.gz", testProduct, testVersion, runtime.GOOS, hostAssetArch())
		if got := r.requestedArchive(t); got != want {
			t.Errorf("fetched %q, want %q", got, want)
		}
	})
}

func hostAssetArch() string {
	if runtime.GOOS == "darwin" {
		return "all"
	}
	return runtime.GOARCH
}

// TestInstall_WgetFallback: the script's header names a stock Alpine/Debian
// container as its reason for being POSIX sh, and those don't always ship curl.
// `have curl` wins everywhere else here, so this is the only run that reaches
// the fallback.
func TestInstall_WgetFallback(t *testing.T) {
	if _, err := lookPathIn(basePATH, "wget"); err != nil {
		t.Skip("no wget on the test PATH")
	}
	forEachShell(t, func(t *testing.T, shell string) {
		r := newFakeRelease(t, testProduct, false)
		dir := filepath.Join(t.TempDir(), "bin")
		env := baseEnv(t, r, t.TempDir(), dir)
		env["ZS_OS"], env["ZS_ARCH"] = "linux", "x86_64"
		env["PATH"] = shimPATHWithout(t, "curl")

		if out, err := run(t, shell, env); err != nil {
			t.Fatalf("install must work without curl: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, testProduct)); err != nil {
			t.Errorf("not installed: %v", err)
		}
	})
}

func assertNothingInstalled(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // never created at all — the strongest form of "nothing installed"
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected an empty install dir, found %v", names)
	}
}
