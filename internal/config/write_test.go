/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sep builds a Unicode line separator from its code point. Written this way,
// not as a literal: these characters are invisible in an editor, and a fixture
// that silently lost one would assert nothing while still passing.
func sep(r rune) string { return string(r) }

// TestSpliceScalarTouchesOneLine is the core claim of the whole writer: setting
// a value rewrites exactly one line and leaves every other byte alone. A
// decode/re-encode would pass a "value is correct" assertion while silently
// eating the comments, so the assertion has to be on the untouched lines.
func TestSpliceScalarTouchesOneLine(t *testing.T) {
	src := `# top comment

server:
  listen: "127.0.0.1:9376"   # loopback only

# A long explanation of the pool that must survive.
# Second line of it.
zs:
  escrow_app_id: 3628061142
  concurrent_slots: 10                # concurrent open tickets
  privacy: true
`
	got, err := spliceScalar([]byte(src), []string{"zs", "concurrent_slots"}, "25")
	if err != nil {
		t.Fatalf("spliceScalar: %v", err)
	}
	srcLines := strings.Split(src, "\n")
	gotLines := strings.Split(string(got), "\n")
	if len(srcLines) != len(gotLines) {
		t.Fatalf("line count changed: %d → %d\n%s", len(srcLines), len(gotLines), got)
	}
	// Pin WHICH line may change, by index. Merely counting changes and checking
	// that the changed line contains the new text passes when the writer
	// clobbers the WRONG line — the clobbered line contains the new text too,
	// and the count is still one. The target index is derived from the source
	// rather than hard-coded so editing the fixture can't silently unpin it.
	wantIdx := -1
	for i, line := range srcLines {
		if strings.Contains(line, "concurrent_slots:") {
			wantIdx = i
			break
		}
	}
	if wantIdx < 0 {
		t.Fatal("fixture no longer contains concurrent_slots")
	}
	for i := range srcLines {
		switch {
		case i == wantIdx:
			if !strings.Contains(gotLines[i], "concurrent_slots: 25") {
				t.Errorf("target line %d = %q, want the new value", i+1, gotLines[i])
			}
		case srcLines[i] != gotLines[i]:
			t.Errorf("line %d changed but should not have: %q → %q", i+1, srcLines[i], gotLines[i])
		}
	}
}

func TestSpliceScalar(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		keyPath []string
		value   string
		want    string
	}{
		{
			name:    "replaces an existing nested key",
			src:     "zs:\n  concurrent_slots: 10\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "25",
			want:    "zs:\n  concurrent_slots: 25\n",
		},
		{
			name:    "preserves a trailing comment",
			src:     "zs:\n  concurrent_slots: 10    # concurrent open tickets\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "25",
			want:    "zs:\n  concurrent_slots: 25  # concurrent open tickets\n",
		},
		{
			name:    "preserves non-standard indentation",
			src:     "zs:\n    concurrent_slots: 10\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "3",
			want:    "zs:\n    concurrent_slots: 3\n",
		},
		{
			// A VALUE whose text equals the key being looked for. findChild
			// walks key/value pairs with a stride of 2; a stride of 1 would
			// match this value node and rewrite a different line.
			name:    "a value equal to the key name is not mistaken for the key",
			src:     "zs:\n  note: concurrent_slots\n  concurrent_slots: 10\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "25",
			want:    "zs:\n  note: concurrent_slots\n  concurrent_slots: 25\n",
		},
		{
			name:    "inserts into an existing block that lacks the key",
			src:     "zs:\n  privacy: true\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "25",
			want:    "zs:\n  concurrent_slots: 25\n  privacy: true\n",
		},
		{
			name:    "inserts under a bare parent",
			src:     "server:\n  listen: \"x\"\nzs:\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\n  listen: \"x\"\nzs:\n  concurrent_slots: 7\n",
		},
		{
			name:    "appends a whole block when the parent is absent",
			src:     "server:\n  listen: \"x\"\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\n  listen: \"x\"\n\nzs:\n  concurrent_slots: 7\n",
		},
		{
			name:    "appends when the file has no trailing newline",
			src:     "server:\n  listen: \"x\"",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\n  listen: \"x\"\n\nzs:\n  concurrent_slots: 7\n",
		},
		{
			name:    "handles a comments-only file",
			src:     "# nothing but a comment\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "# nothing but a comment\n\nzs:\n  concurrent_slots: 7\n",
		},
		{
			name:    "appends a top-level key to a comments-only file",
			src:     "# just a comment\n",
			keyPath: []string{"top"},
			value:   "9",
			want:    "# just a comment\n\ntop: 9\n",
		},
		{
			name:    "replaces a top-level scalar",
			src:     "top: 1\nother: 2\n",
			keyPath: []string{"top"},
			value:   "9",
			want:    "top: 9\nother: 2\n",
		},
		{
			name:    "inserts a top-level scalar at column 1",
			src:     "other: 2\n",
			keyPath: []string{"top"},
			value:   "9",
			want:    "top: 9\nother: 2\n",
		},
		// CRLF is the one non-default shape that IS supported — zs-proxy ships
		// Windows binaries, so it is ordinary, not exotic. Each operation gets
		// a CRLF twin.
		{
			name:    "CRLF: replaces",
			src:     "zs:\r\n  concurrent_slots: 10\r\n  privacy: true\r\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "4",
			want:    "zs:\r\n  concurrent_slots: 4\r\n  privacy: true\r\n",
		},
		{
			name:    "CRLF: replaces and keeps a trailing comment",
			src:     "zs:\r\n  concurrent_slots: 10  # note\r\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "25",
			want:    "zs:\r\n  concurrent_slots: 25  # note\r\n",
		},
		{
			name:    "CRLF: inserts into an existing block",
			src:     "zs:\r\n  privacy: true\r\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "4",
			want:    "zs:\r\n  concurrent_slots: 4\r\n  privacy: true\r\n",
		},
		{
			name:    "CRLF: inserts under a bare parent",
			src:     "server:\r\n  listen: \"x\"\r\nzs:\r\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\r\n  listen: \"x\"\r\nzs:\r\n  concurrent_slots: 7\r\n",
		},
		{
			name:    "CRLF: appends a whole block",
			src:     "server:\r\n  listen: \"x\"\r\n",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\r\n  listen: \"x\"\r\n\r\nzs:\r\n  concurrent_slots: 7\r\n",
		},
		{
			name:    "CRLF: appends with no trailing terminator",
			src:     "server:\r\n  listen: \"x\"",
			keyPath: []string{"zs", "concurrent_slots"},
			value:   "7",
			want:    "server:\r\n  listen: \"x\"\r\n\r\nzs:\r\n  concurrent_slots: 7\r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spliceScalar([]byte(tc.src), tc.keyPath, tc.value)
			if err != nil {
				t.Fatalf("spliceScalar: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tc.want)
			}
			// Every success must also PARSE. Asserting the string alone would
			// pass for output the proxy then refuses to start with.
			var m map[string]any
			if perr := yaml.Unmarshal(got, &m); perr != nil {
				t.Errorf("result does not parse as YAML: %v\n%q", perr, got)
			}
		})
	}
}

// TestSpliceScalarRefusesUnsupportedShapes is the floor the whole design rests
// on. Each of these was, at some point, code that tried to be clever and got it
// wrong — writing a valid-looking file that had quietly lost an unrelated
// setting. Refusing is the feature: the caller has already moved ALGO on chain,
// so a loud "edit this by hand" beats a rewrite that might be wrong.
func TestSpliceScalarRefusesUnsupportedShapes(t *testing.T) {
	tests := []struct {
		name string
		src  string
		key  []string
	}{
		// Line-ending shapes. Each breaks the "index == parser line" invariant
		// that every splice below depends on.
		{name: "bare CR break", src: "zs:\n  concurrent_slots: 10\r  privacy: true\n"},
		{name: "bare CR throughout", src: "top: 1\rother: 2\r", key: []string{"top"}},
		{name: "mixed LF and CRLF", src: "server:\r\n  listen: \"x\"\r\nzs:\n  concurrent_slots: 10\n"},
		{name: "NEL separator", src: "a: \"x" + sep(0x0085) + "y\"\nzs:\n  concurrent_slots: 10\n"},
		{name: "LS separator", src: "a: \"x" + sep(0x2028) + "y\"\nzs:\n  concurrent_slots: 10\n"},
		{name: "PS separator", src: "a: \"x" + sep(0x2029) + "y\"\nzs:\n  concurrent_slots: 10\n"},

		// Document shapes.
		{name: "multi-document", src: "server:\n  listen: \"x\"\n---\nzs:\n  concurrent_slots: 10\n"},
		{name: "leading document marker", src: "---\nzs:\n  concurrent_slots: 10\n"},
		{name: "top level is a sequence", src: "- a\n- b\n", key: []string{"top"}},
		{name: "top level is a flow mapping", src: "{zs: 1}\n", key: []string{"zs"}},

		// Parent shapes that already occupy the line, so an indented child has
		// nowhere to attach.
		{name: "flow-style parent", src: "zs: {concurrent_slots: 10}\n"},
		{name: "scalar parent", src: "zs: 5\nother: 1\n"},
		{name: "string parent", src: "zs: hello\n"},
		{name: "sequence parent", src: "zs: [a, b]\n"},
		{name: "empty-string parent", src: "zs: \"\"\n"},
		{name: "explicit null parent (tilde)", src: "zs: ~\n"},
		{name: "explicit null parent (null)", src: "zs: null\n"},
		{name: "aliased parent", src: "base: &b\n  x: 1\nzs: *b\n"},

		// Leaf shapes a single-line rewrite cannot express.
		{name: "block scalar leaf", src: "zs:\n  concurrent_slots: |\n    hello\n    there\n"},
		{name: "plain multi-line leaf", src: "zs:\n  concurrent_slots:\n    hello world\n"},
		{name: "sequence leaf", src: "zs:\n  concurrent_slots:\n    - a\n    - b\n"},
		{name: "anchored leaf", src: "zs:\n  concurrent_slots: &n 10\n  other: *n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := tc.key
			if key == nil {
				key = []string{"zs", "concurrent_slots"}
			}
			got, err := spliceScalar([]byte(tc.src), key, "25")
			if err == nil {
				t.Fatalf("expected a refusal, got:\n%q", got)
			}
			if !errors.Is(err, ErrUnsupportedShape) {
				t.Errorf("error %v should wrap ErrUnsupportedShape so callers can tell "+
					"'edit by hand' apart from a real failure", err)
			}
		})
	}

	// Programming errors, distinct from a shape refusal.
	if err := SetScalar(filepath.Join(t.TempDir(), "c.yaml"), []string{"a", "b", "c"}, "1"); err == nil {
		t.Error("expected a refusal for a depth-3 key path")
	}
	if err := SetScalar(filepath.Join(t.TempDir(), "c.yaml"), nil, "1"); err == nil {
		t.Error("expected a refusal for an empty key path")
	}
	if err := SetScalar(filepath.Join(t.TempDir(), "c.yaml"), []string{"zs", ""}, "1"); err == nil {
		t.Error("expected a refusal for an empty key path segment")
	}
}

// TestLineEnding is the gate that makes every splice below it safe: one ending
// per file, so element i is line i+1 as the parser counted it. Everything it
// rejects is a shape where that equality silently fails.
func TestLineEnding(t *testing.T) {
	ok := []struct {
		name, src, want string
	}{
		{name: "empty", src: "", want: "\n"},
		{name: "no breaks", src: "a: 1", want: "\n"},
		{name: "LF", src: "a: 1\nb: 2\n", want: "\n"},
		{name: "LF unterminated", src: "a: 1\nb: 2", want: "\n"},
		{name: "CRLF", src: "a: 1\r\nb: 2\r\n", want: "\r\n"},
		{name: "CRLF unterminated", src: "a: 1\r\nb: 2", want: "\r\n"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lineEnding([]byte(tc.src))
			if err != nil {
				t.Fatalf("lineEnding(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("lineEnding(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}

	bad := []struct{ name, src string }{
		{name: "bare CR", src: "a: 1\rb: 2\n"},
		{name: "trailing bare CR", src: "a: 1\n\r"},
		{name: "mixed LF then CRLF", src: "a: 1\nb: 2\r\n"},
		{name: "mixed CRLF then LF", src: "a: 1\r\nb: 2\n"},
		{name: "NEL", src: "a: 1" + sep(0x0085) + "b: 2\n"},
		{name: "LS", src: "a: 1" + sep(0x2028) + "b: 2\n"},
		{name: "PS", src: "a: 1" + sep(0x2029) + "b: 2\n"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := lineEnding([]byte(tc.src)); err == nil {
				t.Errorf("lineEnding(%q) = %q, want a refusal", tc.src, got)
			} else if !errors.Is(err, ErrUnsupportedShape) {
				t.Errorf("error %v should wrap ErrUnsupportedShape", err)
			}
		})
	}
}

// TestLineEndingAgreesWithParser is the empirical half of that gate: for every
// ending lineEnding ACCEPTS, splitting on it must put the parser's line numbers
// on the right elements. A break the parser honors but the splitter misses is
// silent — one element spans several parser lines, and rewriting it deletes the
// others.
func TestLineEndingAgreesWithParser(t *testing.T) {
	for _, src := range []string{
		"server:\n  listen: \"x\"\nzs:\n  concurrent_slots: 10\n",
		"server:\r\n  listen: \"x\"\r\nzs:\r\n  concurrent_slots: 10\r\n",
		"# lead\n\nserver:\n  listen: \"x\"\n\nzs:\n  concurrent_slots: 10\n",
	} {
		ending, err := lineEnding([]byte(src))
		if err != nil {
			t.Fatalf("lineEnding(%q): %v", src, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, zsVal := findChild(documentRoot(&doc), "zs")
		leafKey := zsVal.Content[0]
		lines := splitLines([]byte(src), ending)
		idx := leafKey.Line - 1
		if idx < 0 || idx >= len(lines) {
			t.Fatalf("parser line %d is outside the %d split elements", leafKey.Line, len(lines))
		}
		if !strings.Contains(lines[idx], "concurrent_slots") {
			t.Errorf("parser says line %d but element %d is %q — the two disagree",
				leafKey.Line, idx, lines[idx])
		}
	}
}

// TestSplitLinesRoundTrip: joinLines(splitLines(x)) must return x byte for
// byte. The result is written to the user's config, so a splitter that loses or
// invents a byte corrupts a file nobody asked it to touch.
func TestSplitLinesRoundTrip(t *testing.T) {
	for _, in := range []string{
		"", "\n", "\r\n", "a", "a\n", "a\nb", "a\r\nb\r\n",
		"no breaks at all", "\x00binary\x00", "\xff\xfe invalid utf8",
		"\ttab: 1\n", "zs:\n  concurrent_slots: 10  # note\n",
	} {
		ending, err := lineEnding([]byte(in))
		if err != nil {
			t.Fatalf("lineEnding(%q): %v", in, err)
		}
		if got := string(joinLines(splitLines([]byte(in), ending), ending)); got != in {
			t.Errorf("round trip lost bytes:\n in: %q\nout: %q", in, got)
		}
	}
}

// TestTrailingCommentScansAfterTheKey pins the `afterKey` offset, which exists
// solely so a `#` belonging to the KEY isn't mistaken for a trailing comment.
// Every ordinary `key: value # note` line works identically with or without the
// offset, so without a key containing `#` the offset is untested.
func TestTrailingCommentScansAfterTheKey(t *testing.T) {
	const key = `"a#b"`
	if got := trailingComment(`  `+key+`: 1  # real note`, 2+len(key)); got != "# real note" {
		t.Errorf("trailingComment = %q, want %q — the scan must start after the key", got, "# real note")
	}
	if got := trailingComment("  slots: 10", 2+len("slots")); got != "" {
		t.Errorf("trailingComment = %q, want empty", got)
	}
	if got := trailingComment("  slots: 10  # note   ", 2+len("slots")); got != "# note" {
		t.Errorf("trailingComment = %q, want %q (trimmed)", got, "# note")
	}
	if got := trailingComment("  slots: 10", 999); got != "" {
		t.Errorf("trailingComment = %q, want empty for an out-of-range offset", got)
	}
}

// TestVerifyScalar tests the safety net head-on. Every way to trip it is
// refused upstream, so no end-to-end test can observe it — which is exactly why
// its logic needs pinning directly.
func TestVerifyScalar(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		keyPath []string
		value   string
		wantErr bool
	}{
		{
			name: "the value took", out: "zs:\n  concurrent_slots: 25\n",
			keyPath: []string{"zs", "concurrent_slots"}, value: "25",
		},
		{
			name: "the value did not take", out: "zs:\n  concurrent_slots: 10\n",
			keyPath: []string{"zs", "concurrent_slots"}, value: "25", wantErr: true,
		},
		{
			name: "the key is missing entirely", out: "zs:\n  privacy: true\n",
			keyPath: []string{"zs", "concurrent_slots"}, value: "25", wantErr: true,
		},
		{
			name: "the parent block is missing", out: "server:\n  listen: \"x\"\n",
			keyPath: []string{"zs", "concurrent_slots"}, value: "25", wantErr: true,
		},
		{
			name: "the result is not valid YAML", out: "zs:\n  note: 25\n    orphan\n",
			keyPath: []string{"zs", "note"}, value: "25", wantErr: true,
		},
		{
			name: "the result is not a mapping", out: "- a\n- b\n",
			keyPath: []string{"top"}, value: "1", wantErr: true,
		},
		{
			name: "a top-level key took", out: "top: 9\n",
			keyPath: []string{"top"}, value: "9",
		},
		{
			name: "the target is not a scalar", out: "zs:\n  concurrent_slots:\n    a: 1\n",
			keyPath: []string{"zs", "concurrent_slots"}, value: "25", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyScalar([]byte(tc.out), tc.keyPath, tc.value)
			if tc.wantErr && err == nil {
				t.Errorf("verifyScalar accepted output it should have rejected:\n%s", tc.out)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("verifyScalar rejected valid output: %v\n%s", err, tc.out)
			}
		})
	}
}

// TestSetScalarEitherWritesOrRefuses is the end-to-end guarantee. `slots <n>`
// calls SetScalar only AFTER moving ALGO on chain, so the one outcome that must
// be impossible is "✔ wrote …" over a file that did not change — or that lost
// something else while changing.
//
// Every fixture carries SIBLING settings. Asserting only that the target took
// cannot see content loss: a splice that deletes `privacy` while setting
// `concurrent_slots` passes that check while reverting the operator to defaults.
func TestSetScalarEitherWritesOrRefuses(t *testing.T) {
	for _, src := range []string{
		// Supported shapes: these must succeed and keep their siblings.
		"server:\n  listen: \"1.2.3.4:1\"\nzs:\n  concurrent_slots: 10\n  privacy: false\n",
		"server:\r\n  listen: \"1.2.3.4:1\"\r\nzs:\r\n  concurrent_slots: 10\r\n  privacy: false\r\n",
		"server:\n  listen: \"1.2.3.4:1\"\nzs:\n  privacy: false\n",
		"server:\n  listen: \"1.2.3.4:1\"\nzs:\n  privacy: false\n  concurrent_slots: 10",
		// Unsupported shapes: these must refuse WITHOUT touching the file.
		"server:\n  listen: \"1.2.3.4:1\"\nzs: \"\"\n",
		"server:\n  listen: \"1.2.3.4:1\"\nzs: ~\n",
		"server:\n  listen: \"1.2.3.4:1\"\nzs: 5\n",
		"server:\n  listen: \"1.2.3.4:1\"\n---\nzs:\n  concurrent_slots: 10\n",
		"server:\n  listen: \"1.2.3.4:1\"\nzs:\r  concurrent_slots: 10\n",
		"server:\r\n  listen: \"1.2.3.4:1\"\r\nzs:\n  concurrent_slots: 10\n",
	} {
		t.Run("", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			err := SetScalar(path, []string{"zs", "concurrent_slots"}, "25")
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if err != nil {
				if string(after) != src {
					t.Errorf("SetScalar failed (%v) but changed the file anyway:\n%q", err, after)
				}
				return
			}
			cfg, lerr := LoadBase(path)
			if lerr != nil {
				t.Fatalf("SetScalar reported success but the result does not load: %v\n%q", lerr, after)
			}
			if cfg.Zs.ConcurrentSlots != 25 {
				t.Errorf("SetScalar reported success but the config still reads %d\nfile: %q",
					cfg.Zs.ConcurrentSlots, after)
			}
			if cfg.Server.Listen != "1.2.3.4:1" {
				t.Errorf("the server block was lost: listen = %q\nfile: %q", cfg.Server.Listen, after)
			}
			if cfg.Zs.Privacy == nil || *cfg.Zs.Privacy {
				t.Errorf("`privacy: false` was lost — the file reverted to the default\nfile: %q", after)
			}
		})
	}
}

// TestSetScalarCreatesMinimalFile pins the "just works" behavior: a missing
// config is created with ONLY the key that was set. Seeding it from
// config.example.yaml would freeze every current default into the user's file.
func TestSetScalarCreatesMinimalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := SetScalar(path, []string{"zs", "concurrent_slots"}, "25"); err != nil {
		t.Fatalf("SetScalar: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "zs:\n  concurrent_slots: 25\n") {
		t.Errorf("created file missing the key:\n%s", got)
	}
	for _, unwanted := range []string{"listen:", "privacy:", "escrow_app_id:", "affinity_policy:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("created file leaked a default (%q):\n%s", unwanted, got)
		}
	}

	cfg, err := LoadBase(path)
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	if cfg.Zs.ConcurrentSlots != 25 {
		t.Errorf("ConcurrentSlots = %d, want 25", cfg.Zs.ConcurrentSlots)
	}
	if cfg.Server.Listen != defaults().Server.Listen {
		t.Errorf("listen = %q, want the default %q", cfg.Server.Listen, defaults().Server.Listen)
	}

	// Literal 0600/0700, NOT the constants: comparing a constant to itself
	// would pass for any value it happened to hold.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("created mode = %v, want 0600 — the config sits beside wallet metadata", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("created dir mode = %v, want 0700", perm)
	}
}

// TestSetScalarCreatesTopLevelKey covers the depth-1 create path, which the
// nested case never exercises — SetScalar documents both depths.
func TestSetScalarCreatesTopLevelKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SetScalar(path, []string{"top"}, "9"); err != nil {
		t.Fatalf("SetScalar: %v", err)
	}
	b, _ := os.ReadFile(path)
	var got map[string]any
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("created file does not parse as YAML: %v\n%s", err, b)
	}
	if got["top"] != 9 {
		t.Errorf("top = %v, want 9", got["top"])
	}
}

// TestSetScalarPreservesMode: an existing file keeps its own permissions rather
// than being narrowed to the create default.
func TestSetScalarPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# keep me\nzs:\n  concurrent_slots: 10   # note\n  privacy: false\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetScalar(path, []string{"zs", "concurrent_slots"}, "42"); err != nil {
		t.Fatalf("SetScalar: %v", err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"# keep me", "# note", "privacy: false", "concurrent_slots: 42"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q after write:\n%s", want, b)
		}
	}
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want the pre-existing 0644", perm)
	}
}

// TestWriteFileAtomicCleansUpOnFailure: the success path consumes the temp file
// via rename, so it can never observe the cleanup. Only a failing write can —
// and a temp file left in the state dir is a mystery config beside a real one.
func TestWriteFileAtomicCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// A directory at the destination makes os.Rename fail after the temp file
	// has already been written and closed.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("x: 1\n"), 0o600); err == nil {
		t.Fatal("expected the rename onto a directory to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("failed write left %v behind, want only config.yaml", names)
	}
}

// TestWriteFileAtomicTempIsSiblingOfTarget pins the temp file's LOCATION, not
// just its eventual absence. os.Rename is atomic only within one filesystem, so
// a temp in the OS temp dir would fail with EXDEV whenever the state dir lives
// on a different device — and only on those machines.
//
// Discriminator: make the TARGET directory unwritable. Creating the temp there
// then fails outright and the error names that directory; a temp created in the
// OS temp dir would succeed and fail later, at the rename.
func TestWriteFileAtomicTempIsSiblingOfTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory is still writable")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o500); err != nil { // r-x: no create allowed
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := writeFileAtomic(filepath.Join(dir, "config.yaml"), []byte("x: 1\n"), 0o600)
	if err == nil {
		t.Fatal("expected a failure creating the temp file in an unwritable directory")
	}
	if !strings.Contains(err.Error(), "create temp in") {
		t.Errorf("error was %q, want the temp-creation step — the temp file must be a sibling "+
			"of the target, or os.Rename is not atomic across filesystems", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q should name the target directory %q", err, dir)
	}
}
