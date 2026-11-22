/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// isolateState points paths.StateDir() at a per-test temp dir by overriding
// HOME (os.UserConfigDir derives from it on darwin/linux), so Write/Read/Remove
// never touch the developer's real config dir.
func isolateState(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestWriteReadRemove(t *testing.T) {
	isolateState(t)

	if _, ok, err := Read(); ok || err != nil {
		t.Fatalf("expected no record initially: ok=%v err=%v", ok, err)
	}

	rec := Record{
		PID:        os.Getpid(),
		Listen:     "127.0.0.1:8080",
		Network:    "testnet",
		ConfigPath: "/some/config.yaml",
		LogPath:    "/some/proxy.log",
		StartedAt:  time.Now().Truncate(time.Second),
	}
	if err := Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, ok, err := Read()
	if err != nil || !ok {
		t.Fatalf("Read after Write: ok=%v err=%v", ok, err)
	}
	if got.PID != rec.PID || got.Listen != rec.Listen || got.Network != rec.Network ||
		got.ConfigPath != rec.ConfigPath || got.LogPath != rec.LogPath || !got.StartedAt.Equal(rec.StartedAt) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, rec)
	}

	// The plain pidfile mirror exists and parses to the same pid.
	pp, _ := pidPath()
	b, err := os.ReadFile(pp)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid != rec.PID {
		t.Errorf("pidfile = %q, want pid %d", strings.TrimSpace(string(b)), rec.PID)
	}

	if err := Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, _ := Read(); ok {
		t.Error("record still present after Remove")
	}
	// Remove is idempotent.
	if err := Remove(); err != nil {
		t.Errorf("second Remove should be a no-op, got %v", err)
	}
}

func TestRunning_LiveVsStale(t *testing.T) {
	isolateState(t)

	// Live: our own pid is, by definition, running.
	if err := Write(Record{PID: os.Getpid(), Listen: ":8080"}); err != nil {
		t.Fatal(err)
	}
	if _, running, err := Running(); err != nil || !running {
		t.Fatalf("expected running for own pid: running=%v err=%v", running, err)
	}

	// Stale: a reaped child's pid is gone, so Running reports false (the record
	// is still returned so the caller can clean it).
	if err := Write(Record{PID: reapedPID(t), Listen: ":8080"}); err != nil {
		t.Fatal(err)
	}
	rec, running, err := Running()
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Error("expected not running for a dead pid")
	}
	if rec.PID == 0 {
		t.Error("Running should still return the stale record")
	}
}

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("own pid should be alive")
	}
	if Alive(0) {
		t.Error("pid 0 should not be reported alive")
	}
	if Alive(-1) {
		t.Error("pid -1 should not be reported alive")
	}
	if Alive(reapedPID(t)) {
		t.Error("a reaped pid should not be alive")
	}
}

func TestLogTailLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "proxy.log")
	if err := os.WriteFile(logPath, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LogTailLines(logPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c", "d"}; !equalLines(got, want) {
		t.Errorf("tail 2 = %v, want %v", got, want)
	}

	got, _ = LogTailLines(logPath, 100)
	if want := []string{"a", "b", "c", "d"}; !equalLines(got, want) {
		t.Errorf("tail >file = %v, want %v", got, want)
	}

	// Missing file → empty, no error (the "no log yet" case).
	got, err = LogTailLines(filepath.Join(dir, "nope.log"), 5)
	if err != nil || len(got) != 0 {
		t.Errorf("missing file: got %v err %v, want empty/nil", got, err)
	}
}

// TestSpawnAndTerminate exercises the real detached-spawn + graceful-stop path
// end-to-end against a live child: Spawn (setsid + logfile), Alive (signal 0),
// and Terminate (SIGTERM). It uses `sleep` as a stand-in long-running process,
// so it's unix-only (Windows graceful stop ships later).
func TestSpawnAndTerminate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful Terminate is unix-only for now")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not found: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "child.log")

	pid, err := Spawn(sleep, []string{"30"}, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = terminate(pid) }) // belt-and-suspenders

	if !Alive(pid) {
		t.Fatalf("spawned child (pid %d) should be alive", pid)
	}
	// The logfile is created even before the child writes anything.
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected logfile at %s: %v", logPath, err)
	}

	if err := Terminate(pid); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Alive(pid) {
		t.Errorf("child (pid %d) still alive after SIGTERM", pid)
	}
}

func TestLogTailLines_FromEnd_CrossesChunk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	var sb strings.Builder
	for i := 0; i < 5000; i++ { // ~45 KiB, comfortably past the 16 KiB read chunk
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LogTailLines(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"line-4997", "line-4998", "line-4999"}; !equalLines(got, want) {
		t.Errorf("tail 3 of large file = %v, want %v", got, want)
	}

	// No trailing newline: the final partial line must still be the last one.
	if err := os.WriteFile(p, []byte("x\ny\nlast-no-newline"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = LogTailLines(p, 2)
	if want := []string{"y", "last-no-newline"}; !equalLines(got, want) {
		t.Errorf("tail 2 (no trailing newline) = %v, want %v", got, want)
	}
}

func TestRotateIfLarge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "proxy.log")

	// Under the cap → no rotation.
	if err := os.WriteFile(p, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIfLarge(p)
	if _, err := os.Stat(p + ".1"); !os.IsNotExist(err) {
		t.Error("small log should not have rotated")
	}

	// Over the cap → rolls to .1 and the primary path is freed.
	if err := os.WriteFile(p, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIfLarge(p)
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Errorf("oversized log should roll to .1: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("primary log should be moved away after rotation")
	}
}

func TestProcessMatches(t *testing.T) {
	// Empty recorded exe → fail open (older record / nothing to compare).
	if !ProcessMatches(Record{PID: os.Getpid()}) {
		t.Error("empty ExePath should fail open (true)")
	}
	// Own pid + own exe → matches where the exe is readable, fails open elsewhere.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !ProcessMatches(Record{PID: os.Getpid(), ExePath: self}) {
		t.Error("own pid+exe should match (symlink-tolerant)")
	}
	// Mismatched exe: use a real, existing-but-different file as the recorded
	// path (a non-existent path would fail open, since we can't canonicalize it).
	// Platforms where processExe can read the live path (linux, darwin+cgo)
	// disprove the match; platforms that can't (darwin without cgo, other
	// unixes, windows) fail open. Derive the expectation from processExe's real
	// capability on this build rather than hardcoding by GOOS.
	other := filepath.Join(t.TempDir(), "other-bin")
	if err := os.WriteFile(other, []byte("not the test binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, canReadExe := processExe(os.Getpid())
	got := ProcessMatches(Record{PID: os.Getpid(), ExePath: other})
	if canReadExe && got {
		t.Error("exe-readable platform should detect the mismatch (false)")
	}
	if !canReadExe && !got {
		t.Error("non-exe-readable platform should fail open (true)")
	}
}

// reapedPID runs a trivial process to completion and returns its (now-dead) pid.
func reapedPID(t *testing.T) int {
	t.Helper()
	// `true` exits 0 immediately; after Run it's reaped, so the pid is dead.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
