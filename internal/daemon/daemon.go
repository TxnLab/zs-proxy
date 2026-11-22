/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package daemon implements the self-managed background proxy: spawning a
// detached serving process, recording it under the state dir, and the
// liveness/stop primitives the `proxy stop|status|restart|logs` commands use.
//
// The model is deliberately simple and conventional (the plan's "lean pidfile
// + signal for v1"): the daemonizing parent owns the on-disk record — it spawns
// the detached child, confirms it bound, then writes the pidfile. A foreground
// serve (debugging, or a launchd/systemd/SCM-managed run) writes no record;
// those are owned by Ctrl-C or the service manager, not `proxy stop`. A crashed
// daemon leaves a stale record, which Running() detects via process liveness so
// a fresh `proxy start` is never blocked by it.
package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TxnLab/zs-proxy/internal/paths"
)

// pidFileName is the conventional plain-integer pidfile (so `cat proxy.pid`
// and external tooling work); recordFileName holds the richer status the
// `proxy status` command renders.
const (
	pidFileName    = "proxy.pid"
	recordFileName = "daemon.json"

	// maxLogBytes caps the daemon log: when an existing proxy.log exceeds this
	// at Spawn time it's rolled to proxy.log.1. This bounds growth across
	// restarts; within a single long run the log is still append-only (a
	// continuous rotating writer would have to live in app.Run's logger — a
	// larger change deferred for now).
	maxLogBytes = 10 << 20 // 10 MiB
)

// Record is the on-disk description of the running daemon, written by the
// daemonizing parent once the child is confirmed listening.
type Record struct {
	PID     int    `json:"pid"`
	Listen  string `json:"listen"`
	Network string `json:"network"`
	// NetworkFlag is the --network the daemon was started with, "" for none.
	// Network names the resolved network, which on Solana need not be a
	// --network value at all, so restart passes this one on.
	NetworkFlag string `json:"network_flag,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
	LogPath     string `json:"log_path"`
	ExePath     string `json:"exe_path,omitempty"`
	// ShutdownToken authorizes the loopback POST /shutdown control channel — the
	// graceful-stop path on Windows (no SIGTERM). The daemonizing parent
	// generates it, forwards it to the child via the environment, and records it
	// here so `proxy stop` can present it. Empty for daemons started by an older
	// build (RequestStop then can't stop them gracefully on Windows).
	ShutdownToken string    `json:"shutdown_token,omitempty"`
	StartedAt     time.Time `json:"started_at"`
}

// recordPath / pidPath resolve the two state files. They share StateDir with
// the wallet metadata and config.
func recordPath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, recordFileName), nil
}

func pidPath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pidFileName), nil
}

// Write persists the daemon record (and the plain pidfile mirror), creating
// the state dir if needed. The JSON file is written via a temp+rename so a
// concurrent reader never sees a half-written record.
func Write(r Record) error {
	dir, err := paths.StateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	rp := filepath.Join(dir, recordFileName)
	tmp := rp + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, rp); err != nil {
		return err
	}

	pp := filepath.Join(dir, pidFileName)
	return os.WriteFile(pp, []byte(strconv.Itoa(r.PID)+"\n"), 0o600)
}

// Read loads the daemon record. ok is false (with a nil error) when no record
// exists — the common "not running" case.
func Read() (rec Record, ok bool, err error) {
	rp, err := recordPath()
	if err != nil {
		return Record{}, false, err
	}
	data, err := os.ReadFile(rp)
	if os.IsNotExist(err) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false, fmt.Errorf("parse %s: %w", rp, err)
	}
	return rec, true, nil
}

// Remove deletes both state files (idempotent — a missing file is not an error).
func Remove() error {
	rp, err := recordPath()
	if err != nil {
		return err
	}
	pp, err := pidPath()
	if err != nil {
		return err
	}
	var firstErr error
	for _, p := range []string{rp, pp} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Running reports the recorded daemon when it exists, its process is still
// alive, AND that process is still ours (the pid wasn't recycled by an
// unrelated program). A record whose process is gone (crash, kill -9) or whose
// pid now belongs to a stranger yields running=false; the record is returned
// too, so the caller can report/clean the stale entry.
func Running() (rec Record, running bool, err error) {
	rec, ok, err := Read()
	if err != nil || !ok {
		return rec, false, err
	}
	return rec, Alive(rec.PID) && ProcessMatches(rec), nil
}

// Alive reports whether a process with the given pid currently exists. The
// platform-specific probe lives in proc_unix.go / proc_windows.go.
func Alive(pid int) bool { return processAlive(pid) }

// ProcessMatches guards against the pidfile pid-reuse race: a crashed daemon's
// pid can be recycled by an unrelated process before `proxy stop` runs, and we
// must not SIGTERM a stranger. It compares the live process's executable path to
// the one recorded at spawn. It is intentionally fail-open: an empty recorded
// path (older record) or a platform where the exe can't be read (processExe
// returns ok=false — currently everything but Linux) returns true, so a wedged
// but legitimate daemon is still stoppable. Only a positive mismatch returns
// false.
func ProcessMatches(rec Record) bool {
	if rec.ExePath == "" {
		return true
	}
	exe, ok := processExe(rec.PID)
	if !ok {
		return true
	}
	return sameExe(rec.ExePath, exe)
}

// sameExe compares two executable paths tolerant of symlink representation.
// The recorded path (os.Executable) and the live path (proc_pidpath / proc
// readlink) can spell the same file differently — notably /var vs /private/var
// on macOS — so a raw string compare yields false negatives. We resolve both
// through EvalSymlinks; if either can't be resolved (e.g. the recorded binary
// was upgraded out from under us) we fail open rather than declare a mismatch,
// since we can't actually prove it's a stranger.
func sameExe(recorded, live string) bool {
	if recorded == live {
		return true
	}
	ra, ea := filepath.EvalSymlinks(recorded)
	rb, eb := filepath.EvalSymlinks(live)
	if ea != nil || eb != nil {
		return true
	}
	return ra == rb
}

// Terminate is the pid-addressed SIGTERM primitive (unix), which the serving
// process wires to a graceful drain. Prefer RequestStop — the cross-platform
// entry that, on Windows (no SIGTERM), uses the loopback control channel
// instead. See proc_windows.go.
func Terminate(pid int) error { return terminate(pid) }

// RequestStop asks the running daemon to drain and exit gracefully. On unix
// that's SIGTERM (the serving process wires it to srv.Shutdown). Windows has no
// SIGTERM, so it uses the loopback POST /shutdown control channel recorded in
// the daemon record. Either way the caller (stopDaemon) then polls Alive until
// the process disappears, so the wait/drain loop is unchanged across platforms.
func RequestStop(rec Record) error {
	if runtime.GOOS == "windows" {
		return stopViaControlChannel(rec)
	}
	return terminate(rec.PID)
}

// stopViaControlChannel POSTs to the daemon's loopback /shutdown endpoint with
// the recorded token. It's the Windows stop path (no SIGTERM) and is exercised
// by the cross-platform unit test (which calls it directly).
//
// Limitation: it dials controlBaseURL(rec.Listen). The default 127.0.0.1 /
// 0.0.0.0 binds resolve to loopback and the server's loopback-only /shutdown
// guard accepts them; a daemon bound to a specific non-loopback host (e.g. a LAN
// IP) is dialed at that host and the guard would 403 it. That's an unusual config
// for a single-user localhost proxy — documented rather than special-cased.
func stopViaControlChannel(rec Record) error {
	if rec.ShutdownToken == "" {
		return fmt.Errorf("daemon record has no shutdown token (started by an older build); stop pid %d via Task Manager", rec.PID)
	}
	base := controlBaseURL(rec.Listen)
	req, err := http.NewRequest(http.MethodPost, base+"/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Shutdown-Token", rec.ShutdownToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("contact proxy control channel at %s: %w", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy refused shutdown (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// controlBaseURL renders the loopback dial URL for the daemon's listen address,
// normalizing a wildcard/empty host to 127.0.0.1 (you dial loopback, not
// 0.0.0.0). Mirrors the cli baseURL helper; duplicated here to avoid a
// cli↔daemon import cycle.
func controlBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// NewShutdownToken returns a fresh random token (256 bits, hex) for the loopback
// control channel. The daemonizing parent generates one per launch.
func NewShutdownToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate shutdown token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Spawn launches exe with args as a detached background process, redirecting
// its stdout+stderr to logPath (append). It returns the child's pid. The
// caller writes the daemon record only after confirming the child bound — see
// the cli daemonize flow.
func Spawn(exe string, args []string, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, fmt.Errorf("create log dir: %w", err)
	}
	rotateIfLarge(logPath)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log file: %w", err)
	}
	defer lf.Close() // parent's copy; the child keeps its own descriptor

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = detachedSysProcAttr()
	// cmd.Env left nil → inherit the parent's environment (this carries any
	// ZEROSIGNAL_KEYSTORE_PASSPHRASE the parent resolved for the file backend,
	// so the TTY-less child can still unlock the encrypted-file wallet).
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap in the background rather than Release(): if the child dies during the
	// caller's healthz poll window, an unreaped child lingers as a zombie that
	// still answers kill -0, so Alive() would wrongly report it up and we'd
	// record a pidfile for a dead daemon. Wait() reaps it promptly so Alive()
	// flips to false. For a healthy child this goroutine simply blocks until the
	// short-lived launcher process exits, at which point the daemon is
	// reparented to init and reaped there.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// rotateIfLarge rolls logPath to logPath.1 when it exceeds maxLogBytes, so the
// log doesn't grow without bound across restarts. Best-effort: any error
// (missing file, rename failure) just leaves the existing log in place — losing
// a rotation is never worth failing a daemon start.
func rotateIfLarge(logPath string) {
	fi, err := os.Stat(logPath)
	if err != nil || fi.Size() <= maxLogBytes {
		return
	}
	_ = os.Rename(logPath, logPath+".1") // replaces any prior .1
}

// LogTailLines returns the last n lines of the log file (fewer if the file is
// shorter), oldest-first. A missing file yields an empty slice. It reads from
// the end of the file rather than slurping the whole thing, so tailing a large
// log stays cheap. Used by `proxy logs` and to surface why a daemon failed to
// come up.
func LogTailLines(logPath string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()

	// Read backward in chunks, accumulating until we've seen n+1 newlines (the
	// +1 bounds the first kept line) or reached the start of the file.
	const chunk = 16 << 10
	var buf []byte
	newlines := 0
	for off := size; off > 0 && newlines <= n; {
		readLen := int64(chunk)
		if off < readLen {
			readLen = off
		}
		off -= readLen
		b := make([]byte, readLen)
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(b, buf...)
		newlines = bytes.Count(buf, []byte{'\n'})
	}

	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil // empty file
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
