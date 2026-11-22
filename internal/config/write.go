/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultCreatePerm is the mode a config file created by SetScalar gets. 0600
// rather than 0644: the file sits in the user's state dir beside wallet
// metadata, and while secrets are meant to come from the environment (never
// YAML), a config narrower than the user's umask is the safe direction to err.
const defaultCreatePerm os.FileMode = 0o600

// createdHeader is the only prose a freshly created config carries. The file is
// deliberately NOT seeded from config.example.yaml: materializing that template
// would freeze every current default into the user's file, so a later change to
// a default we own would silently not reach them.
const createdHeader = "# zs-proxy configuration — see config.example.yaml for every available key.\n"

// ErrUnsupportedShape is returned for any file this writer will not edit. It is
// a deliberate floor, not a gap — see SetScalar.
var ErrUnsupportedShape = errors.New("config: this file's shape is not one zs-proxy will edit automatically")

// SetScalar sets one scalar config value in the YAML file at path, creating the
// file (and its parent directory) when absent.
//
// It is a TEXT SPLICE, not a decode/re-encode: the file is parsed only to
// locate the target line, then that single line is rewritten and every other
// byte is preserved. A yaml.Marshal round-trip would be far shorter but drops
// blank lines and every comment, and a user's config.yaml is derived from
// config.example.yaml — ~14KB of prose explaining each knob. Destroying that to
// change one integer is a bad trade, and it is also why no .bak is kept: a
// one-line splice has nothing to lose.
//
// It handles the ordinary shapes — LF or CRLF, a plain block mapping, one
// document — and REFUSES everything else with ErrUnsupportedShape rather than
// trying to be clever. That floor is the design, not a limitation: a line
// editor that also handles bare-CR breaks, mixed endings, anchors, aliases,
// multi-document files and block scalars is most of a YAML implementation, and
// every one of those paths failed silently the first time it was written —
// producing a valid-looking file that had quietly lost an unrelated setting.
// The caller has already moved ALGO on chain by the time this runs, so a loud
// refusal naming the line to add by hand beats a clever rewrite that might be
// wrong.
//
// keyPath is the nested key sequence (e.g. {"zs", "concurrent_slots"}). Only
// depth 1 and 2 are supported, which is every key this config has.
func SetScalar(path string, keyPath []string, value string) error {
	if len(keyPath) == 0 || len(keyPath) > 2 {
		return fmt.Errorf("config: SetScalar needs a key path of depth 1 or 2, got %d", len(keyPath))
	}
	for _, k := range keyPath {
		if k == "" {
			return errors.New("config: SetScalar got an empty key path segment")
		}
	}

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return createWithScalar(path, keyPath, value)
	default:
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	updated, err := spliceScalar(existing, keyPath, value)
	if err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	// Re-read our own output before committing it. Every known way to get this
	// wrong is refused upstream, so this should never fire — which is exactly
	// why it stays: the failure it guards against is a config file that still
	// parses but no longer says what the caller was told it says, written after
	// ALGO has already moved. TestVerifyScalar pins its logic directly.
	if err := verifyScalar(updated, keyPath, value); err != nil {
		return fmt.Errorf("config: %s: refusing to write: %w", path, err)
	}
	perm := defaultCreatePerm
	if fi, statErr := os.Stat(path); statErr == nil {
		perm = fi.Mode().Perm()
	}
	return writeFileAtomic(path, updated, perm)
}

// verifyScalar re-parses spliced output and confirms keyPath now holds value.
func verifyScalar(out []byte, keyPath []string, value string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return fmt.Errorf("the result is not valid YAML: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return errors.New("the result is not a mapping")
	}
	node := root
	for i, seg := range keyPath {
		key, val := findChild(node, seg)
		if key == nil {
			return fmt.Errorf("%s is missing from the result", strings.Join(keyPath[:i+1], "."))
		}
		node = val
	}
	if node.Kind != yaml.ScalarNode || node.Value != value {
		return fmt.Errorf("%s is %q in the result, want %q", strings.Join(keyPath, "."), node.Value, value)
	}
	return nil
}

// createWithScalar writes a brand-new config holding ONLY this key. See
// createdHeader for why it isn't seeded from the example file.
func createWithScalar(path string, keyPath []string, value string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("config: create %s: %w", dir, err)
		}
	}
	var b strings.Builder
	b.WriteString(createdHeader)
	b.WriteString("\n")
	b.WriteString(renderBlock(keyPath, value, "\n"))
	return writeFileAtomic(path, []byte(b.String()), defaultCreatePerm)
}

// unicodeBreaks are the non-ASCII line breaks gopkg.in/yaml.v3's scanner honors
// (its is_break): NEL, LINE SEPARATOR and PARAGRAPH SEPARATOR. Written as code
// points rather than literals because they are invisible in an editor, and a
// stray "whitespace cleanup" that mangled them would silently reopen the
// line-numbering drift lineEnding exists to prevent.
var unicodeBreaks = []rune{0x0085, 0x2028, 0x2029}

// lineEnding reports the file's single line ending, "\n" or "\r\n".
//
// It REFUSES a file that mixes them, or that uses any other break the YAML
// parser honors — a bare "\r", or NEL / LS / PS. That refusal is what lets
// every function below assume "one ending, split on it, index == parser line".
// Without it those assumptions are silently false: the split yields fewer
// elements than the parser counted lines, a node's Line lands on the wrong
// element, and the rewrite replaces an unrelated key. The result still parses,
// so nothing downstream notices.
func lineEnding(src []byte) (string, error) {
	s := string(src)
	for _, r := range unicodeBreaks {
		if strings.ContainsRune(s, r) {
			return "", fmt.Errorf("%w: it contains a Unicode line separator (U+%04X)", ErrUnsupportedShape, r)
		}
	}
	// A "\r" not followed by "\n" is a bare-CR break to the parser.
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && (i+1 >= len(s) || s[i+1] != '\n') {
			return "", fmt.Errorf("%w: it contains a bare carriage-return line break", ErrUnsupportedShape)
		}
	}
	crlf := strings.Count(s, "\r\n")
	if crlf == 0 {
		return "\n", nil
	}
	// Every "\n" must belong to a "\r\n", or the file mixes conventions.
	if strings.Count(s, "\n") != crlf {
		return "", fmt.Errorf("%w: it mixes LF and CRLF line endings", ErrUnsupportedShape)
	}
	return "\r\n", nil
}

// spliceScalar is the pure core: given the file's bytes, return them with the
// key set, touching at most one line. Kept separate from SetScalar so the line
// surgery is testable without a filesystem.
func spliceScalar(src []byte, keyPath []string, value string) ([]byte, error) {
	ending, err := lineEnding(src)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	// yaml.Unmarshal decodes only the FIRST document, so a target living in a
	// later one would look absent and the append below would land where the
	// loader never reads — valid YAML in the wrong place.
	if bytes.Contains(src, []byte("\n---")) || bytes.HasPrefix(src, []byte("---")) {
		return nil, fmt.Errorf("%w: it contains more than one YAML document", ErrUnsupportedShape)
	}

	// An empty (or comments-only) file parses to a zero Node with no content.
	root := documentRoot(&doc)
	if root == nil {
		return appendBlock(src, keyPath, value, ending), nil
	}
	if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("%w: its top level is not a plain block mapping", ErrUnsupportedShape)
	}

	parent := root
	if len(keyPath) == 2 {
		parentKey, parentVal := findChild(root, keyPath[0])
		if parentKey == nil {
			return appendBlock(src, keyPath, value, ending), nil
		}
		switch {
		case parentVal.Kind == yaml.MappingNode && parentVal.Style&yaml.FlowStyle == 0 && len(parentVal.Content) > 0:
			parent = parentVal
		case isBareKey(parentVal):
			// `zs:` with nothing after it — insert the first child directly
			// under the parent key, two columns further in.
			return insertLine(src, parentKey.Line, parentKey.Column+2, keyPath[1], value, ending), nil
		default:
			// Anything else already occupies the line: a scalar (`zs: 5`, even
			// `zs: ~` or `zs: ""`), a sequence, a flow mapping, an alias.
			// Indenting a child beneath any of them does not parse.
			return nil, fmt.Errorf("%w: %s is not a plain block mapping", ErrUnsupportedShape, keyPath[0])
		}
	}

	leafKey, leafVal := findChild(parent, keyPath[len(keyPath)-1])
	if leafKey == nil {
		// Insert before the block's first key, matching its indentation.
		return insertLine(src, parent.Content[0].Line-1, parent.Content[0].Column, keyPath[len(keyPath)-1], value, ending), nil
	}
	// A line splice can only rewrite what lives on the key's own line. A block
	// or folded scalar puts its value on the lines below; an anchor definition
	// would be erased, leaving every alias to it dangling.
	if leafVal.Kind != yaml.ScalarNode ||
		leafVal.Line != leafKey.Line ||
		leafVal.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 ||
		leafVal.Anchor != "" {
		return nil, fmt.Errorf("%w: %s is not a plain single-line value", ErrUnsupportedShape, strings.Join(keyPath, "."))
	}
	return replaceLine(src, leafKey, value, ending)
}

// documentRoot unwraps the document node yaml.Unmarshal produces, returning nil
// for an empty or comments-only file.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	if doc.Kind == 0 {
		return nil
	}
	return doc
}

// isBareKey reports whether `zs:` has nothing after it on the line, the one
// shape that has room for a freshly indented first child.
//
// The !!null tag alone is not enough: `zs: ~` and `zs: null` carry it too, and
// their value token already occupies the line, so an indented child under them
// is "mapping values are not allowed in this context". Requiring an empty Value
// as well is what distinguishes a bare key from an explicit null.
func isBareKey(n *yaml.Node) bool {
	if n.Kind == yaml.MappingNode {
		return len(n.Content) == 0 && n.Style&yaml.FlowStyle == 0
	}
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null" && n.Value == ""
}

// findChild returns the key and value nodes for name in a mapping, or nils.
func findChild(mapping *yaml.Node, name string) (key, val *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// splitLines splits on the file's single ending. The trailing element is ""
// when the file ends with a break, or the unterminated tail otherwise; either
// way joinLines restores the original bytes exactly.
func splitLines(src []byte, ending string) []string {
	return strings.Split(string(src), ending)
}

func joinLines(lines []string, ending string) []byte {
	return []byte(strings.Join(lines, ending))
}

// replaceLine rewrites the single line holding key, preserving that line's
// indentation and any trailing `#` comment. yaml.Node's Line/Column are
// 1-based; Column points at the first character of the key.
func replaceLine(src []byte, key *yaml.Node, value, ending string) ([]byte, error) {
	lines := splitLines(src, ending)
	idx := key.Line - 1
	if idx < 0 || idx >= len(lines) {
		// Unreachable while lineEnding holds the "one ending" invariant, so no
		// test covers it — which is why it errors rather than returning src
		// unchanged. The caller cannot tell a no-op from a success, and by this
		// point it has already moved ALGO on chain.
		return nil, fmt.Errorf("line %d for key %q is outside the file (%d lines)", key.Line, key.Value, len(lines))
	}
	rebuilt := strings.Repeat(" ", key.Column-1) + key.Value + ": " + value
	if c := trailingComment(lines[idx], key.Column-1+len(key.Value)); c != "" {
		rebuilt += "  " + c
	}
	lines[idx] = rebuilt
	return joinLines(lines, ending), nil
}

// trailingComment extracts a `#` comment from a `key: value  # note` line,
// scanning only after the key so a `#` inside the key itself can't match. It
// deliberately ignores quoting: the values this writer sets are integers, and a
// `#` inside a quoted scalar on the same line would at worst carry a fragment
// of that scalar into the rebuilt comment rather than corrupt the YAML.
func trailingComment(line string, afterKey int) string {
	if afterKey >= len(line) {
		return ""
	}
	i := strings.Index(line[afterKey:], "#")
	if i < 0 {
		return ""
	}
	return strings.TrimRight(line[afterKey+i:], " \t")
}

// insertLine inserts `key: value` at the given indentation, landing at index
// afterLine in the 0-based line slice.
func insertLine(src []byte, afterLine, column int, key, value, ending string) []byte {
	lines := splitLines(src, ending)
	at := min(max(afterLine, 0), len(lines))
	indent := ""
	if column > 1 {
		indent = strings.Repeat(" ", column-1)
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at]...)
	out = append(out, indent+key+": "+value)
	out = append(out, lines[at:]...)
	return joinLines(out, ending)
}

// appendBlock appends a fresh block to a file that has no such key.
func appendBlock(src []byte, keyPath []string, value, ending string) []byte {
	var b bytes.Buffer
	b.Write(src)
	if len(src) > 0 && !bytes.HasSuffix(src, []byte(ending)) {
		b.WriteString(ending) // the file ends mid-line; terminate it first
	}
	if len(src) > 0 {
		b.WriteString(ending) // blank line between the existing content and the new block
	}
	b.WriteString(renderBlock(keyPath, value, ending))
	return b.Bytes()
}

// renderBlock renders the key path as fresh YAML, shared by the append and
// create paths so a new block looks the same however it got there.
func renderBlock(keyPath []string, value, ending string) string {
	if len(keyPath) == 2 {
		return fmt.Sprintf("%s:%s  %s: %s%s", keyPath[0], ending, keyPath[1], value, ending)
	}
	return fmt.Sprintf("%s: %s%s", keyPath[0], value, ending)
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a crash mid-write can't leave a truncated config
// that the next start refuses to parse.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("config: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename onto %s: %w", path, err)
	}
	return nil
}
