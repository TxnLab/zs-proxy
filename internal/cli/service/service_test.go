/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package service

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wellFormedXML reports whether s parses as well-formed XML (token stream to EOF
// with no error) — enough to catch unescaped interpolation breaking the plist.
func wellFormedXML(s string) error {
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestRenderFor_Darwin(t *testing.T) {
	out, err := renderFor("darwin", Spec{
		BinPath: "/opt/homebrew/bin/zs-proxy",
		Args:    []string{"proxy", "start", "--foreground", "--network", "testnet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wellFormedXML(out); err != nil {
		t.Fatalf("rendered plist is not well-formed XML: %v\n%s", err, out)
	}
	for _, want := range []string{
		"<string>" + LaunchdLabel + "</string>",
		"<string>/opt/homebrew/bin/zs-proxy</string>",
		"<string>--network</string>",
		"<string>testnet</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n%s", want, out)
		}
	}
}

func TestRenderFor_Darwin_EscapesXML(t *testing.T) {
	// A path with an XML-special char must be escaped, and the result must still
	// be well-formed.
	out, err := renderFor("darwin", Spec{BinPath: "/tmp/a&b/zs-proxy", Args: []string{"proxy"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wellFormedXML(out); err != nil {
		t.Fatalf("escaped plist not well-formed: %v", err)
	}
	if !strings.Contains(out, "a&amp;b") {
		t.Errorf("`&` not XML-escaped:\n%s", out)
	}
	if strings.Contains(out, "a&b/") {
		t.Errorf("raw unescaped `&` present:\n%s", out)
	}
}

func TestRenderFor_Linux_ExecStartQuotesSpaces(t *testing.T) {
	out, err := renderFor("linux", Spec{
		BinPath: "/usr/bin/zs-proxy",
		Args:    []string{"proxy", "start", "--foreground", "--config", "/a b/c.yaml"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `ExecStart=/usr/bin/zs-proxy proxy start --foreground --config "/a b/c.yaml"`) {
		t.Errorf("ExecStart not built/quoted as expected:\n%s", out)
	}
	for _, want := range []string{"Restart=on-failure", "KillSignal=SIGTERM", "WantedBy=default.target"} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

func TestRenderFor_Windows(t *testing.T) {
	out, err := renderFor("windows", Spec{
		BinPath: `C:\Program Files\zs-proxy\zs-proxy.exe`,
		Args:    []string{"proxy", "start", "--foreground", "--network", "testnet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		WindowsServiceName,
		WindowsServiceDisplay,
		`C:\Program Files\zs-proxy\zs-proxy.exe`,
		"--network testnet",
		"sc.exe create",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("windows render missing %q\n%s", want, out)
		}
	}
}

func TestRenderFor_Windows_RunAsHidesPassword(t *testing.T) {
	out, err := renderFor("windows", Spec{
		BinPath:  `C:\zs-proxy.exe`,
		Args:     []string{"proxy", "start", "--foreground"},
		Account:  `MACHINE\alice`,
		Password: "supersecret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `MACHINE\alice`) {
		t.Errorf("render missing the run-as account:\n%s", out)
	}
	if !strings.Contains(out, `obj= "MACHINE\alice"`) {
		t.Errorf("sc.exe command missing obj= account:\n%s", out)
	}
	if strings.Contains(out, "supersecret") {
		t.Errorf("render LEAKED the password:\n%s", out)
	}
}

func TestRenderFor_Unsupported(t *testing.T) {
	// A GOOS with no backend here still returns ErrUnsupported (darwin/linux/
	// windows are all supported now).
	if _, err := renderFor("plan9", Spec{BinPath: "x"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("plan9 render err = %v, want ErrUnsupported", err)
	}
}

func TestDefaultServiceAccount_NonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows this returns the current user")
	}
	if _, err := DefaultServiceAccount(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DefaultServiceAccount err = %v, want ErrUnsupported off Windows", err)
	}
}

func TestExecStartLine_Quoting(t *testing.T) {
	got := execStartLine("/bin/zs-proxy", []string{"proxy", "--config", "/a b/c", "plain"})
	want := `/bin/zs-proxy proxy --config "/a b/c" plain`
	if got != want {
		t.Errorf("execStartLine = %q, want %q", got, want)
	}
}

func TestDarwinTarget(t *testing.T) {
	if got, want := darwinTarget(501), "gui/501/"+LaunchdLabel; got != want {
		t.Errorf("darwinTarget(501) = %q, want %q", got, want)
	}
}

func TestStopSpec(t *testing.T) {
	cases := []struct {
		goos    string
		uid     int
		wantBin string
		wantArg string
	}{
		{"darwin", 501, "launchctl", "bootout gui/501/" + LaunchdLabel},
		{"linux", 0, "systemctl", "--user stop " + systemdName},
		{"plan9", 0, "", ""},
	}
	for _, c := range cases {
		spec := stopSpec(c.goos, c.uid)
		if spec.Bin != c.wantBin {
			t.Errorf("stopSpec(%q).Bin = %q, want %q", c.goos, spec.Bin, c.wantBin)
		}
		if got := strings.Join(spec.Args, " "); got != c.wantArg {
			t.Errorf("stopSpec(%q).Args = %q, want %q", c.goos, got, c.wantArg)
		}
	}
}

func TestRestartSpec(t *testing.T) {
	cases := []struct {
		goos    string
		uid     int
		wantBin string
		wantArg string
	}{
		{"darwin", 501, "launchctl", "kickstart -k gui/501/" + LaunchdLabel},
		{"linux", 0, "systemctl", "--user restart " + systemdName},
		{"plan9", 0, "", ""},
	}
	for _, c := range cases {
		spec := restartSpec(c.goos, c.uid)
		if spec.Bin != c.wantBin {
			t.Errorf("restartSpec(%q).Bin = %q, want %q", c.goos, spec.Bin, c.wantBin)
		}
		if got := strings.Join(spec.Args, " "); got != c.wantArg {
			t.Errorf("restartSpec(%q).Args = %q, want %q", c.goos, got, c.wantArg)
		}
	}
}

func TestParseDarwinStatus(t *testing.T) {
	// Mirrors real `launchctl print` output: the job's own "state = running" near
	// the top, then nested sub-dictionaries with their own "state = active" lines
	// further down. The top-level state must win (regression: a last-match parser
	// reported "active" ≠ "running" → not running, even when it was running).
	out := "gui/502/" + LaunchdLabel + " = {\n" +
		"\tstate = running\n" +
		"\tprogram = /opt/homebrew/bin/zs-proxy\n" +
		"\tpid = 23660\n" +
		"\tendpoints = {\n\t\t\"x\" = {\n\t\t\tstate = active\n\t\t}\n\t}\n" +
		"\tmachservices = {\n\t\tstate = active\n\t}\n}\n"
	running, pid := parseDarwinStatus(out)
	if !running || pid != 23660 {
		t.Errorf("parseDarwinStatus(running w/ nested active) = (%v, %d), want (true, 23660)", running, pid)
	}
	// Loaded but not running, no pid.
	running, pid = parseDarwinStatus("\tstate = not running\n")
	if running || pid != 0 {
		t.Errorf("parseDarwinStatus(stopped) = (%v, %d), want (false, 0)", running, pid)
	}
}

func TestServiceLogSource_HostOS(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		src, err := ServiceLogSource()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(src.File, "zs-proxy.log") {
			t.Errorf("darwin log file = %q, want a zs-proxy.log path", src.File)
		}
	case "linux":
		src, err := ServiceLogSource()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(src.Command, " "); got != "journalctl --user -u "+systemdName {
			t.Errorf("linux log command = %q, want journalctl --user -u %s", got, systemdName)
		}
	default:
		t.Skip("log source shape checked on darwin/linux")
	}
}

func TestInstalled_HostOS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows installation state is queried from the SCM, not a unit file")
	}
	if !Supported() {
		t.Skip("no service backend on this OS")
	}
	// Isolate HOME so UnitPath resolves under a clean temp dir.
	home := t.TempDir()
	t.Setenv("HOME", home)

	if ok, err := Installed(); err != nil || ok {
		t.Fatalf("Installed() with no unit = (%v, %v), want (false, nil)", ok, err)
	}

	p, err := UnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := Installed(); err != nil || !ok {
		t.Fatalf("Installed() with a unit present = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestUnitPath_HostOS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows SCM service has no unit file")
	}
	if !Supported() {
		t.Skip("no service backend on this OS")
	}
	p, err := UnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, ".plist") && !strings.HasSuffix(p, ".service") {
		t.Errorf("unexpected unit path: %s", p)
	}
}
