/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package service renders and installs a per-user OS service unit (launchd
// LaunchAgent on macOS, systemd user unit on Linux) that runs the proxy in the
// foreground so it starts at login and restarts on failure. The binary emits
// the unit with its own resolved path baked in, so there's no file to find in a
// Caskroom and no path to hand-edit. Per-user (not system) on purpose: the
// service runs in the login session, where the wallet keychain / Secret Service
// is reachable.
package service

import (
	"bytes"
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/launchd.plist.tmpl templates/systemd.service.tmpl
var templatesFS embed.FS

const (
	// LaunchdLabel is the launchd job label (reverse-DNS) and the plist base name.
	LaunchdLabel = "dev.txnlab.zs-proxy"
	// systemdUnit is the systemd user-unit filename / name.
	systemdUnit = "zs-proxy.service"
	systemdName = "zs-proxy"

	// WindowsServiceName is the SCM service key. The serving binary passes it to
	// svc.Run when launched by the SCM, so it MUST match what scmInstall
	// registers. WindowsServiceDisplay / WindowsServiceDesc are what services.msc
	// shows. (Windows is a machine-level SCM service, unlike the per-user
	// launchd/systemd units — its key store reads unattended via DPAPI.)
	WindowsServiceName    = "zs-proxy"
	WindowsServiceDisplay = "zs-proxy (ZeroSignal proxy)"
	WindowsServiceDesc    = "Drop-in, private, OpenAI-compatible inference proxy"
)

// ErrUnsupported is returned on platforms without a service backend here.
var ErrUnsupported = errors.New("install-service is not supported on this OS; " +
	"it works on macOS (launchd), Linux (systemd), and Windows (SCM)")

// Spec is what the caller knows: the proxy binary to run and the args to run it
// with (e.g. ["proxy","start","--foreground","--network","testnet"]). Paths,
// labels, and the log location are filled in per-OS by the package.
type Spec struct {
	BinPath string
	Args    []string
	// Account / Password are used ONLY by the Windows SCM backend: the service
	// runs as this account, so it reads that user's Credential Manager wallet +
	// per-user state dir (a LocalSystem service would see neither). Empty Account
	// → LocalSystem. The CLI prompts for the password (hidden) and never logs it;
	// both are ignored on macOS/Linux (those units are already per-user).
	Account  string
	Password string
}

// Supported reports whether the current OS has a backend here.
func Supported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"
}

// Render returns the unit text for the current OS.
func Render(s Spec) (string, error) { return renderFor(runtime.GOOS, s) }

// DefaultServiceAccount returns the account a Windows service should run as by
// default — the installing (current) user — so the service reads that user's
// Credential Manager wallet and per-user state dir rather than LocalSystem's.
// Non-Windows returns ErrUnsupported (launchd/systemd units are already per-user).
func DefaultServiceAccount() (string, error) {
	if runtime.GOOS != "windows" {
		return "", ErrUnsupported
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	return u.Username, nil // e.g. MACHINE\user or DOMAIN\user — valid as ServiceStartName
}

type tmplData struct {
	Label     string
	BinPath   string
	Args      []string
	LogPath   string
	ExecStart string
}

// renderFor is the GOOS-parameterized core (so tests can render both platforms
// on any host).
func renderFor(goos string, s Spec) (string, error) {
	var name string
	var data tmplData
	switch goos {
	case "darwin":
		logPath, err := darwinLogPath()
		if err != nil {
			return "", err
		}
		// launchd reads a plist (XML) — escape every interpolated value.
		name = "launchd.plist.tmpl"
		data = tmplData{
			Label:   LaunchdLabel,
			BinPath: xmlEscape(s.BinPath),
			Args:    xmlEscapeAll(s.Args),
			LogPath: xmlEscape(logPath),
		}
	case "linux":
		// systemd logs to the journal; ExecStart needs systemd quoting.
		name = "systemd.service.tmpl"
		data = tmplData{ExecStart: execStartLine(s.BinPath, s.Args)}
	case "windows":
		// SCM has no unit file — render a human-readable summary (+ the
		// equivalent sc.exe command) for `--print`. Pure formatting, so it works
		// on any host (the install itself is scmInstall in service_windows.go).
		return scmRender(s), nil
	default:
		return "", ErrUnsupported
	}

	tmpl, err := template.ParseFS(templatesFS, "templates/"+name)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// UnitPath is where the unit file installs for the current OS.
func UnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
	default:
		return "", ErrUnsupported
	}
}

// Install renders the unit, writes it (0600 — it may later hold a passphrase),
// and, when enable is true, loads/enables it so it starts now and on login.
// Returns the unit path and whether it was enabled. A best-effort enable
// failure is returned as an error after the file is written, so the caller can
// surface the manual EnableHint.
func Install(s Spec, enable bool) (path string, enabled bool, err error) {
	if !Supported() {
		return "", false, ErrUnsupported
	}
	// Windows SCM isn't file-based — register/start via the service manager.
	// "path" is the service name there (for the caller's confirmation line).
	if runtime.GOOS == "windows" {
		return scmInstall(s, enable)
	}
	unit, err := Render(s)
	if err != nil {
		return "", false, err
	}
	path, err = UnitPath()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, fmt.Errorf("create unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o600); err != nil {
		return "", false, fmt.Errorf("write unit: %w", err)
	}
	if !enable {
		return path, false, nil
	}
	if err := enableUnit(path); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// Uninstall stops/disables the service and removes the unit file. Missing
// pieces are not an error (idempotent).
func Uninstall() (path string, err error) {
	if !Supported() {
		return "", ErrUnsupported
	}
	if runtime.GOOS == "windows" {
		return scmUninstall()
	}
	path, err = UnitPath()
	if err != nil {
		return "", err
	}
	disableUnit() // best-effort; ignore "not loaded" errors
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	if runtime.GOOS == "linux" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return path, nil
}

// EnableHint is the manual enable command for the current OS — shown when
// auto-enable was skipped (file-backend wallet) or failed.
func EnableHint() string {
	switch runtime.GOOS {
	case "darwin":
		p, _ := UnitPath()
		return fmt.Sprintf("launchctl bootstrap gui/%d %q", os.Getuid(), p)
	case "linux":
		return "systemctl --user daemon-reload && systemctl --user enable --now " + systemdName
	case "windows":
		return "sc.exe start " + WindowsServiceName
	default:
		return ""
	}
}

// Installed reports whether a service is registered for the current OS: the
// per-user unit file is present (macOS/Linux), or the SCM entry exists
// (Windows). The CLI lifecycle commands branch on this to target the installed
// service rather than the self-managed daemon.
func Installed() (bool, error) {
	switch runtime.GOOS {
	case "darwin", "linux":
		p, err := UnitPath()
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(p); err == nil {
			return true, nil
		} else if errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else {
			return false, err
		}
	case "windows":
		return scmInstalled()
	default:
		return false, nil
	}
}

// Start loads/starts the installed service so it runs now (and at login/boot).
// On macOS/Linux it reuses enableUnit (launchd bootstrap+enable / systemd
// daemon-reload + enable --now), the same path install and EnableHint use.
func Start() error {
	switch runtime.GOOS {
	case "darwin", "linux":
		p, err := UnitPath()
		if err != nil {
			return err
		}
		return enableUnit(p)
	case "windows":
		return scmStart()
	default:
		return ErrUnsupported
	}
}

// Stop stops the running service. macOS boots it out of the login session (a
// KeepAlive job can't be paused in place — Start rebootstraps it); Linux stops
// the unit but leaves it enabled for next boot; Windows stops the SCM service.
func Stop() error {
	if runtime.GOOS == "windows" {
		return scmStop()
	}
	if !Supported() {
		return ErrUnsupported
	}
	return stopSpec(runtime.GOOS, os.Getuid()).run()
}

// Restart restarts the running service in place.
func Restart() error {
	if runtime.GOOS == "windows" {
		return scmRestart()
	}
	if !Supported() {
		return ErrUnsupported
	}
	return restartSpec(runtime.GOOS, os.Getuid()).run()
}

// ServiceStatus is the installed service's liveness, queried from the OS service
// manager. PID is 0 when not running or unknown.
type ServiceStatus struct {
	Running bool
	PID     int
}

// Status queries the OS service manager for the installed service's liveness.
// A not-loaded / inactive service is reported as ServiceStatus{} (not running),
// not an error — only a query that itself fails returns one.
func Status() (ServiceStatus, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", darwinTarget(os.Getuid())).CombinedOutput()
		if err != nil {
			// Non-zero ⇒ "Could not find service" (booted out / never loaded).
			return ServiceStatus{}, nil
		}
		running, pid := parseDarwinStatus(string(out))
		return ServiceStatus{Running: running, PID: pid}, nil
	case "linux":
		// is-active exits 0 only when the unit is active; MainPID is 0 when not.
		running := exec.Command("systemctl", "--user", "is-active", systemdName).Run() == nil
		pid := 0
		if out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", systemdName).Output(); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
		return ServiceStatus{Running: running, PID: pid}, nil
	case "windows":
		return scmStatus()
	default:
		return ServiceStatus{}, ErrUnsupported
	}
}

// LogSource describes where the installed service writes logs: either a File to
// tail, or a Command to stream (Linux's systemd journal has no file). Exactly
// one is set; both empty ⇒ the caller falls back to its own default log path
// (Windows, where the SCM service logs to the same proxy.log the daemon uses).
type LogSource struct {
	File    string
	Command []string
}

// ServiceLogSource returns where the installed service logs for the current OS.
func ServiceLogSource() (LogSource, error) {
	switch runtime.GOOS {
	case "darwin":
		p, err := darwinLogPath()
		if err != nil {
			return LogSource{}, err
		}
		return LogSource{File: p}, nil
	case "linux":
		// Base command; the CLI appends -n <lines> and -f (follow).
		return LogSource{Command: []string{"journalctl", "--user", "-u", systemdName}}, nil
	case "windows":
		return LogSource{}, nil // CLI falls back to its proxy.log path
	default:
		return LogSource{}, ErrUnsupported
	}
}

// scmRender is the `--print` summary for the Windows SCM service: the resolved
// config plus the equivalent sc.exe command. Pure formatting (no Windows APIs),
// so renderFor("windows", …) is exercisable on any host.
func scmRender(s Spec) string {
	args := strings.Join(s.Args, " ")
	runAs := s.Account
	if runAs == "" {
		runAs = "(the installing user — prompted for the password at install time)"
	}
	return fmt.Sprintf(""+
		"Windows service (SCM), created by `zs-proxy proxy install-service`:\n"+
		"  Name:        %s\n"+
		"  Display:     %s\n"+
		"  Description: %s\n"+
		"  BinPath:     %s %s\n"+
		"  RunAs:       %s\n"+
		"  StartType:   automatic, restart-on-failure\n\n"+
		"Equivalent sc.exe command:\n"+
		"  sc.exe create %s binPath= \"%s %s\" start= auto obj= \"%s\" DisplayName= \"%s\"\n",
		WindowsServiceName, WindowsServiceDisplay, WindowsServiceDesc,
		s.BinPath, args, runAs,
		WindowsServiceName, s.BinPath, args, runAs, WindowsServiceDisplay)
}

func enableUnit(path string) error {
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		// Bout out a prior instance first so bootstrap doesn't fail with
		// "service already loaded"; ignore its error (it may not be loaded).
		_ = exec.Command("launchctl", "bootout", domain+"/"+LaunchdLabel).Run()
		if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		_ = exec.Command("launchctl", "enable", domain+"/"+LaunchdLabel).Run() // clear any prior `disable`
		return nil
	case "linux":
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user daemon-reload failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", systemdName).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user enable --now failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return ErrUnsupported
	}
}

func disableUnit() {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", darwinTarget(os.Getuid())).Run()
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdName).Run()
	}
}

// darwinTarget is the launchd service target in the per-user gui domain, e.g.
// "gui/501/dev.txnlab.zs-proxy" — what launchctl bootout/kickstart/print address.
func darwinTarget(uid int) string {
	return "gui/" + strconv.Itoa(uid) + "/" + LaunchdLabel
}

// cmdSpec is a resolved OS-service-manager invocation. Built by the pure
// stopSpec/restartSpec helpers (parameterized on GOOS so the command shapes are
// testable on any host) and run by Stop/Restart on the host OS.
type cmdSpec struct {
	Bin  string
	Args []string
}

func (c cmdSpec) run() error {
	if c.Bin == "" {
		return ErrUnsupported
	}
	if out, err := exec.Command(c.Bin, c.Args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s failed: %v: %s", c.Bin, strings.Join(c.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopSpec is the OS-manager stop command for goos (darwin needs uid).
func stopSpec(goos string, uid int) cmdSpec {
	switch goos {
	case "darwin":
		return cmdSpec{"launchctl", []string{"bootout", darwinTarget(uid)}}
	case "linux":
		return cmdSpec{"systemctl", []string{"--user", "stop", systemdName}}
	default:
		return cmdSpec{}
	}
}

// restartSpec is the OS-manager restart command for goos (darwin needs uid).
func restartSpec(goos string, uid int) cmdSpec {
	switch goos {
	case "darwin":
		return cmdSpec{"launchctl", []string{"kickstart", "-k", darwinTarget(uid)}}
	case "linux":
		return cmdSpec{"systemctl", []string{"--user", "restart", systemdName}}
	default:
		return cmdSpec{}
	}
}

// parseDarwinStatus pulls the running state and pid out of `launchctl print`
// output (lines like "\tstate = running" and "\tpid = 12345"). It takes the
// FIRST occurrence of each key — the job's own top-level state — because nested
// sub-dictionaries (endpoints, machservices) emit their own later "state = "
// lines (e.g. "state = active") that would otherwise clobber the real answer.
func parseDarwinStatus(out string) (running bool, pid int) {
	var gotState, gotPID bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case !gotState && strings.HasPrefix(line, "state = "):
			running = strings.TrimSpace(strings.TrimPrefix(line, "state = ")) == "running"
			gotState = true
		case !gotPID && strings.HasPrefix(line, "pid = "):
			pid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
			gotPID = true
		}
		if gotState && gotPID {
			break
		}
	}
	return running, pid
}

// darwinLogPath is the absolute log file launchd redirects stdout/stderr to
// (launchd does not expand ~ or env vars in path keys).
func darwinLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "zs-proxy.log"), nil
}

// execStartLine builds a systemd ExecStart value, quoting any token that
// contains whitespace (e.g. a config path with spaces) per systemd's rules.
func execStartLine(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, systemdQuote(bin))
	for _, a := range args {
		parts = append(parts, systemdQuote(a))
	}
	return strings.Join(parts, " ")
}

func systemdQuote(s string) string {
	if strings.ContainsAny(s, " \t\"\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlEscapeAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = xmlEscape(s)
	}
	return out
}
