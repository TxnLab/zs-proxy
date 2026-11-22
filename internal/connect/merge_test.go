/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tailscale/hujson"
	yaml "gopkg.in/yaml.v3"
)

func contains(b []byte, sub string) bool { return bytes.Contains(b, []byte(sub)) }

// stdJSON parses (possibly JSONC) bytes into a generic map for assertions.
func stdJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	std, err := hujson.Standardize(b)
	if err != nil {
		t.Fatalf("standardize: %v\n%s", err, b)
	}
	var m map[string]any
	if err := json.Unmarshal(std, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, std)
	}
	return m
}

func TestMergeJSONC_PreservesCommentsAndSiblings(t *testing.T) {
	existing := []byte(`{
  // my provider — keep this comment
  "provider": {
    "mine": { "name": "Mine" }
  },
  "theme": "dark"
}`)
	sets := []jsonSet{
		{Pointer: "/provider/zs-proxy", Value: map[string]any{"name": "ZS"}},
		{Pointer: "/model", Value: "zs-proxy/x"},
	}
	out, err := mergeJSONC(existing, sets)
	if err != nil {
		t.Fatalf("mergeJSONC: %v", err)
	}
	if !contains(out, "keep this comment") {
		t.Errorf("comment not preserved:\n%s", out)
	}
	m := stdJSON(t, out)
	prov := m["provider"].(map[string]any)
	if _, ok := prov["mine"]; !ok {
		t.Error("existing provider 'mine' was dropped")
	}
	if _, ok := prov["zs-proxy"]; !ok {
		t.Error("zs-proxy provider not added")
	}
	if m["theme"] != "dark" {
		t.Error("unrelated key 'theme' lost")
	}
	if m["model"] != "zs-proxy/x" {
		t.Errorf("model = %v", m["model"])
	}
}

func TestMergeJSONC_EmptyStartAndIdempotent(t *testing.T) {
	sets := []jsonSet{{Pointer: "/provider/zs-proxy", Value: map[string]any{"name": "ZS"}}}
	first, err := mergeJSONC(nil, sets)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, ok := stdJSON(t, first)["provider"]; !ok {
		t.Fatal("provider missing from fresh merge")
	}
	second, err := mergeJSONC(first, sets)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestMergeYAML_DeepMergePreserves(t *testing.T) {
	existing := []byte("theme: dark\nproviders:\n  other:\n    base_url: http://x\n")
	add := map[string]any{"providers": map[string]any{"zs-proxy": map[string]any{"base_url": "http://p"}}}
	out, err := mergeYAML(existing, add)
	if err != nil {
		t.Fatalf("mergeYAML: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["theme"] != "dark" {
		t.Error("theme lost")
	}
	provs := m["providers"].(map[string]any)
	if _, ok := provs["other"]; !ok {
		t.Error("existing provider 'other' dropped")
	}
	if _, ok := provs["zs-proxy"]; !ok {
		t.Error("zs-proxy not added")
	}
}

func TestMergeTOML_DeepMergePreserves(t *testing.T) {
	existing := []byte("model = \"keep\"\n[model_providers.other]\nbase_url = \"http://x\"\n")
	add := map[string]any{
		"model_provider":  "zs-proxy",
		"model_providers": map[string]any{"zs-proxy": map[string]any{"base_url": "http://p"}},
	}
	out, err := mergeTOML(existing, add)
	if err != nil {
		t.Fatalf("mergeTOML: %v", err)
	}
	if !contains(out, "other") {
		t.Errorf("existing provider 'other' dropped:\n%s", out)
	}
	if !contains(out, "zs-proxy") {
		t.Errorf("zs-proxy not added:\n%s", out)
	}
	if !contains(out, `model = 'keep'`) && !contains(out, `model = "keep"`) {
		t.Errorf("top-level model lost:\n%s", out)
	}
}

// TestMergeTOML_Prune covers the three prune outcomes: a matching value is
// removed, a value the user changed is left alone (that is what OnlyValue buys),
// and a path that isn't there is a no-op rather than an error.
func TestMergeTOML_Prune(t *testing.T) {
	const path0, path1, leaf = "model_providers", "zs-proxy", "env_key"
	prune := func(only string) mergePrune {
		return mergePrune{Path: []string{path0, path1, leaf}, OnlyValue: only}
	}

	tests := []struct {
		name     string
		existing string
		prune    mergePrune
		wantGone bool
	}{
		{
			name:     "matching value removed",
			existing: "[model_providers.zs-proxy]\nenv_key = \"OURS\"\n",
			prune:    prune("OURS"),
			wantGone: true,
		},
		{
			name:     "user's own value preserved",
			existing: "[model_providers.zs-proxy]\nenv_key = \"THEIRS\"\n",
			prune:    prune("OURS"),
			wantGone: false,
		},
		{
			name:     "unconditional removal",
			existing: "[model_providers.zs-proxy]\nenv_key = \"THEIRS\"\n",
			prune:    prune(""),
			wantGone: true,
		},
		{
			name:     "missing ancestor is a no-op",
			existing: "model = \"keep\"\n",
			prune:    prune("OURS"),
			wantGone: true, // nothing to find; assertion below is just "no error"
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mergeTOML([]byte(tc.existing), map[string]any{}, tc.prune)
			if err != nil {
				t.Fatalf("mergeTOML: %v", err)
			}
			if got := contains(out, "env_key"); got == tc.wantGone {
				t.Errorf("env_key present = %v, want %v:\n%s", got, !tc.wantGone, out)
			}
		})
	}
}

func TestAtomicWriteWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "cfg.json") // nested → MkdirAll exercised

	if err := atomicWriteWithBackup(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "v1" {
		t.Fatalf("content = %q", b)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("backup created on first write (none expected)")
	}

	if err := atomicWriteWithBackup(path, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "v2" {
		t.Fatalf("content = %q", b)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != "v1" {
		t.Errorf("backup = %q, want v1", b)
	}
}

// TestMergeDotenv_SpliceRules pins the line-splice contract for Hermes' .env:
// an assignment is rewritten where it sits in every shape Hermes' own writer
// recognizes, a missing key is appended on a terminated line, and every other
// byte — comments, blank lines, each line's own ending, an unterminated tail
// nothing is appended after — comes through untouched. The "parity:" rows are
// Hermes' own writer/loader parity fixture
// (tests/hermes_cli/test_env_export_line_lifecycle.py, hermes-agent 8c8003f):
// a line Hermes treats as an assignment and we don't gets a duplicate appended
// that a later Hermes remove would resurrect, and the reverse clobbers a line
// Hermes considers some other variable.
func TestMergeDotenv_SpliceRules(t *testing.T) {
	sets := []envSet{{Key: "A", Value: "new"}, {Key: "B", Value: "http://x/v1"}}
	const tail = "A=new\nB=http://x/v1\n"
	tests := []struct {
		name string
		in   string
		sets []envSet
		want string
	}{
		{"empty file", "", sets, tail},
		{"replace in place, append missing",
			"# keep\nOTHER=1\n\nA=old\n", sets,
			"# keep\nOTHER=1\n\nA=new\nB=http://x/v1\n"},
		{"parity: plain", "A=old\n", sets, tail},
		{"parity: export form", "export A=old\n", sets, tail},
		{"parity: spaced form", "A = old\n", sets, tail},
		{"parity: tab-spaced form", "A\t=\told\n", sets, tail},
		{"parity: export spaced form", "export A = old\n", sets, tail},
		{"parity: comment is not an assignment", "# A=old\n", sets, "# A=old\n" + tail},
		{"parity: longer name is not the key", "A_EXTRA=old\n", sets, "A_EXTRA=old\n" + tail},
		{"parity: prefixed name is not the key", "MY_A=old\n", sets, "MY_A=old\n" + tail},
		{"key inside a value is not an assignment", "OTHER=A=1\n", sets, "OTHER=A=1\n" + tail},
		{"bom-prefixed first line matches, bom dropped on rewrite", "\uFEFFA=old\nB=old\n", sets, tail},
		{"every duplicate rewritten", "A=1\nA=2\n", sets, "A=new\nA=new\nB=http://x/v1\n"},
		{"trailing inline comment dropped with the rewritten line", "A=old # note\n", sets, tail},
		{"unterminated tail terminated before append", "OTHER=1", sets, "OTHER=1\n" + tail},
		{"unterminated tail kept when nothing is appended", "B=old\nA=old", sets, "B=http://x/v1\nA=new"},
		{"crlf preserved and extended", "OTHER=1\r\nA=old\r\n", sets, "OTHER=1\r\nA=new\r\nB=http://x/v1\r\n"},
		{"mixed endings: each line keeps its own, append uses crlf",
			"OTHER=1\nA=old\r\nZ=2\n", sets,
			"OTHER=1\nA=new\r\nZ=2\nB=http://x/v1\r\n"},
		{"value dotenv would misread is quoted",
			"", []envSet{{Key: "A", Value: `a b#"c`}},
			"A=\"a b#\\\"c\"\n"},
		{"embedded newline stays on one line",
			"", []envSet{{Key: "A", Value: "a\r\nb"}},
			"A=ab\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(mergeDotenv([]byte(tc.in), tc.sets)); got != tc.want {
				t.Errorf("mergeDotenv(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMergeDotenv_CaseFoldOnWindows: Windows env names are case-insensitive and
// Hermes folds them there, so a lower-case line is our variable on Windows and
// some other variable everywhere else.
func TestMergeDotenv_CaseFoldOnWindows(t *testing.T) {
	got := string(mergeDotenv([]byte("a=old\n"), []envSet{{Key: "A", Value: "new"}}))
	want := "a=old\nA=new\n"
	if runtime.GOOS == "windows" {
		want = "A=new\n"
	}
	if got != want {
		t.Errorf("mergeDotenv on %s = %q, want %q", runtime.GOOS, got, want)
	}
}

// TestRedactEnv pins what --print may show of .env: the kept keys verbatim,
// every other assignment's name with its value masked (export, spacing and a
// commenting-out kept, so the shape is faithful), prose comments and blank
// lines as they are, and anything that is not a valid-name assignment masked
// whole — including a PEM body line, whose `=` padding would otherwise leave
// the key material in front of the mask.
func TestRedactEnv(t *testing.T) {
	in := "# Hermes\r\nOPENROUTER_API_KEY=sk-live\r\n\r\nexport OTHER = x\r\n# OLD_KEY=sk-old\r\n" +
		"PEM=\"-----BEGIN PRIVATE KEY-----\r\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC+/Xk==\r\n# see https://x/?a=b\r\nstray-token\r\nA=ours"
	want := "# Hermes\r\nOPENROUTER_API_KEY=<redacted>\r\n\r\nexport OTHER =<redacted>\r\n# OLD_KEY=<redacted>\r\n" +
		"PEM=<redacted>\r\n<redacted>\r\n<redacted>\r\n<redacted>\r\nA=ours"
	if got := string(redactEnv([]byte(in), []envSet{{Key: "A", Value: "ours"}})); got != want {
		t.Errorf("redactEnv\n got: %q\nwant: %q", got, want)
	}
}
