/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	yaml "gopkg.in/yaml.v3"
)

// jsonSet sets one JSON-pointer location to a value during a JSONC merge.
type jsonSet struct {
	// Pointer is RFC-6901, e.g. "/provider/zs-proxy". Most keys need no
	// escaping; run any that can contain a ~ or / through jsonPointerEscape
	// first (aider's model keys are "openai/<id>", which would otherwise split
	// into two path segments).
	Pointer string
	Value   any // marshaled to JSON for the patch
}

// jsonPointerEscape escapes one JSON-pointer path segment per RFC-6901: ~ → ~0
// and / → ~1, in that order (reversing it would double-escape the tilde).
func jsonPointerEscape(segment string) string {
	return strings.ReplaceAll(strings.ReplaceAll(segment, "~", "~0"), "/", "~1")
}

// mergeJSONC merges the given setters into existing JSON/JSONC bytes, preserving
// comments and untouched formatting (via hujson's RFC-6902 Patch). An empty
// input starts from {}. Each setter's ancestor objects are created only when
// missing, so an existing sibling provider/object is never clobbered.
func mergeJSONC(existing []byte, sets []jsonSet) ([]byte, error) {
	src := bytes.TrimSpace(existing)
	if len(src) == 0 {
		src = []byte("{}")
	}
	v, err := hujson.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse existing config: %w", err)
	}
	for _, s := range sets {
		if err := ensureAndSet(&v, s.Pointer, s.Value); err != nil {
			return nil, err
		}
	}
	v.Format()
	return v.Pack(), nil
}

// ensureAndSet creates each missing ancestor object of pointer, then adds (or
// replaces, per RFC-6902 add semantics on an object member) the leaf value.
func ensureAndSet(v *hujson.Value, pointer string, value any) error {
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i := 1; i < len(parts); i++ {
		anc := "/" + strings.Join(parts[:i], "/")
		if v.Find(anc) == nil {
			if err := v.Patch([]byte(`[{"op":"add","path":"` + anc + `","value":{}}]`)); err != nil {
				return fmt.Errorf("create %s: %w", anc, err)
			}
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value for %s: %w", pointer, err)
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": pointer, "value": json.RawMessage(raw)}})
	if err != nil {
		return err
	}
	if err := v.Patch(patch); err != nil {
		return fmt.Errorf("set %s: %w", pointer, err)
	}
	return nil
}

// mergeYAML deep-merges add into existing YAML bytes, applies any prunes, and
// re-marshals. Comments in the existing file are not preserved
// (decode→merge→encode); the .bak backup and --print preview mitigate.
func mergeYAML(existing []byte, add map[string]any, prune ...mergePrune) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return nil, fmt.Errorf("parse existing YAML: %w", err)
		}
		if root == nil {
			root = map[string]any{}
		}
	}
	deepMerge(root, add)
	for _, p := range prune {
		pruneKey(root, p)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// mergePrune removes one key from the merged tree — the counterpart to deepMerge,
// which only ever adds. It exists so an integration can retire a setting it used
// to write: re-running connect then repairs an already-configured file instead of
// leaving the stale line behind.
type mergePrune struct {
	// Path is the key chain to the leaf, e.g. {"model_providers", "zerosignal",
	// "env_key"}. A path whose ancestors are missing is a silent no-op, which is
	// what keeps re-apply idempotent.
	Path []string
	// OnlyValue, when non-empty, restricts the removal to that exact string value.
	// Use it to delete only what connect itself wrote and leave a value the user
	// set by hand alone.
	OnlyValue string
}

// mergeTOML deep-merges add into existing TOML bytes, applies any prunes, and
// re-marshals (comments not preserved).
func mergeTOML(existing []byte, add map[string]any, prune ...mergePrune) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := toml.Unmarshal(existing, &root); err != nil {
			return nil, fmt.Errorf("parse existing TOML: %w", err)
		}
	}
	deepMerge(root, add)
	for _, p := range prune {
		pruneKey(root, p)
	}
	out, err := toml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode TOML: %w", err)
	}
	return out, nil
}

// pruneKey deletes p.Path's leaf from root, honoring p.OnlyValue. A missing
// ancestor, a non-map ancestor, or a value that doesn't match OnlyValue all leave
// the tree untouched.
func pruneKey(root map[string]any, p mergePrune) {
	if len(p.Path) == 0 {
		return
	}
	parent := root
	for _, seg := range p.Path[:len(p.Path)-1] {
		next, ok := asStringMap(parent[seg])
		if !ok {
			return
		}
		parent = next
	}
	leaf := p.Path[len(p.Path)-1]
	if _, present := parent[leaf]; !present {
		return
	}
	if p.OnlyValue != "" && parent[leaf] != p.OnlyValue {
		return
	}
	delete(parent, leaf)
}

// deepMerge recursively merges src into dst: nested maps merge key-by-key, all
// other values (scalars, slices) overwrite. dst is mutated in place.
func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := asStringMap(sv); ok {
			if dm, ok := asStringMap(dst[k]); ok {
				deepMerge(dm, sm)
				dst[k] = dm
				continue
			}
		}
		dst[k] = sv
	}
}

// asStringMap normalizes the map shapes the YAML/TOML decoders produce
// (map[string]any) so deepMerge can recurse uniformly.
func asStringMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// envSet is one KEY=value assignment for mergeDotenv.
type envSet struct {
	Key   string
	Value string
}

// mergeDotenv upserts sets into existing dotenv bytes as a LINE SPLICE, not a
// dotenv round-trip: the one file this serves is Hermes' ~/.hermes/.env, where
// every other line is some other provider's real key and the prose around it,
// and a re-encode would reshape all of that to set two values.
//
// The match rule is Hermes' own _env_line_defines_key (see envLineKey), so a
// line either writer sets is one the other finds — otherwise one side appends a
// duplicate the other cannot see, and a later remove resurrects the value.
// Every duplicate is rewritten rather than only the first because dotenv
// applies assignments in file order, so a stale later line would win over ours.
// CR/LF are stripped from values, as Hermes' writer does, because a newline
// inside one would turn the rest of the value into a line of its own.
func mergeDotenv(existing []byte, sets []envSet) []byte {
	src := string(existing)
	ending := "\n"
	if strings.Contains(src, "\r\n") {
		ending = "\r\n"
	}
	lines := strings.SplitAfter(src, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, s := range sets {
		value := strings.NewReplacer("\r", "", "\n", "").Replace(s.Value)
		rendered := s.Key + "=" + quoteEnvValue(value)
		found := false
		for i, line := range lines {
			if key, ok := envLineKey(line); !ok || !envKeyEqual(key, s.Key) {
				continue
			}
			found = true
			lines[i] = rendered + line[len(strings.TrimRight(line, "\r\n")):]
		}
		if !found {
			if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += ending
			}
			lines = append(lines, rendered+ending)
		}
	}
	return []byte(strings.Join(lines, ""))
}

// envLineKey returns the name a dotenv line assigns, in exactly the shapes
// Hermes' loader parses: plain `KEY=`, `export KEY=`, whitespace around either
// side of the `=`, and never a commented-out `# KEY=`. A leading BOM is ignored
// because Hermes reads the file as utf-8-sig (Notepad adds one), so a BOM'd
// first line is an assignment to it.
func envLineKey(line string) (string, bool) {
	s := strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
	if rest, ok := strings.CutPrefix(s, "export "); ok {
		s = strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	name, _, ok := strings.Cut(s, "=")
	return strings.TrimSpace(name), ok
}

// envKeyEqual compares env names the way the platform does: Windows env names
// are case-insensitive, so Hermes folds them there (_env_var_policy_name) and a
// `zerosignal_api_key=` line is the same variable.
func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// redactEnv masks every value in merged dotenv bytes except the keys in keep.
// It is what --print shows for .env: the real splice, so the preview is
// faithful in shape, but that file holds every other provider's real key and
// --print output gets piped and pasted. The `<redacted>` marker also makes the
// output visibly not something to paste back over the file.
//
// A name is shown only when it is a valid variable name, live or commented out;
// every other line holding an `=` is masked whole. Splitting on the first `=`
// alone would print a hand-pasted multi-line value's body — a base64 PEM line
// ends in `=` padding — almost in full. Blank lines and prose comments with no
// `=` pass through, and anything else is masked whole: a stray token is still a
// secret.
func redactEnv(merged []byte, keep []envSet) []byte {
	lines := strings.SplitAfter(string(merged), "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		ending := line[len(body):]
		trimmed := strings.TrimSpace(strings.TrimPrefix(body, "\uFEFF"))
		comment, isComment := strings.CutPrefix(trimmed, "#")
		switch key, ok := envLineKey(comment); {
		case trimmed == "":
		case isComment && !strings.Contains(comment, "="):
		case ok && validEnvName(key):
			if !isComment && slices.ContainsFunc(keep, func(s envSet) bool { return envKeyEqual(key, s.Key) }) {
				continue
			}
			lines[i] = body[:strings.Index(body, "=")+1] + "<redacted>" + ending
		default:
			lines[i] = "<redacted>" + ending
		}
	}
	return []byte(strings.Join(lines, ""))
}

// validEnvName reports whether name is a POSIX-shaped variable name, the only
// shape Hermes will write (_ENV_VAR_NAME_RE).
func validEnvName(name string) bool {
	for i, r := range name {
		if r != '_' && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

// quoteEnvValue double-quotes a value dotenv would otherwise misread, escaping
// the backslashes and quotes inside it; a plain value is written bare.
func quoteEnvValue(v string) string {
	if !strings.ContainsAny(v, "#\"'") && strings.IndexFunc(v, unicode.IsSpace) < 0 {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// atomicWriteWithBackup writes data to path atomically (see atomicWrite), first
// copying any existing file to path+".bak".
func atomicWriteWithBackup(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", existing, perm); err != nil {
			return fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing %s: %w", path, err)
	}
	return atomicWrite(path, data, perm)
}

// atomicWrite writes data to path via temp file + rename, creating parent dirs.
// The rename replaces the file rather than writing through it, which is what
// lets a read-only (0400) target be updated and still end up at perm.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".zsproxy-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// readFileOrEmpty returns the file's bytes, or nil if it does not exist.
func readFileOrEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}
