package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Change sets one configuration key (see Keys) to a TOML literal.
type Change struct {
	Key     string
	Literal string
}

// ErrModified is returned when the configuration file changed after it was
// read, so that concurrent edits are never silently overwritten.
var ErrModified = errors.New("the configuration file was modified by another process")

// Update applies changes to the configuration file at path in place. Only the
// affected lines are rewritten: comments, formatting, ordering and every other
// setting are preserved. The result must parse and validate, otherwise nothing
// is written. If expect is not empty, the file must still have that
// fingerprint (see Config.Fingerprint). The write is atomic and keeps mode 0600.
func Update(path, expect string, changes []Change) (*Config, error) {
	unlock, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (run `vario init`)", ErrNotFound, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if expect != "" && fingerprint(data) != expect {
		return nil, fmt.Errorf("%w (%s); nothing was written, please retry", ErrModified, path)
	}
	out, err := ApplyChanges(data, changes)
	if err != nil {
		return nil, err
	}
	cfg, err := checkContent(path, out)
	if err != nil {
		return nil, err
	}
	if string(out) == string(data) {
		return cfg, nil
	}
	if err := writePrivate(path, out); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Replace atomically replaces the configuration file with data after checking
// that it parses and validates and that the current file still has the
// fingerprint expect (if set).
func Replace(path, expect string, data []byte) (*Config, error) {
	cfg, err := checkContent(path, data)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if expect != "" {
		cur, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read configuration: %w", err)
		}
		if fingerprint(cur) != expect {
			return nil, fmt.Errorf("%w (%s); nothing was written", ErrModified, path)
		}
	}
	return cfg, writePrivate(path, data)
}

// ValidationError reports a configuration that parses but is not valid.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string {
	return "the resulting configuration is invalid:\n  " + strings.ReplaceAll(e.Err.Error(), "\n", "\n  ")
}
func (e *ValidationError) Unwrap() error { return e.Err }

func checkContent(path string, data []byte) (*Config, error) {
	cfg, err := Parse(path, data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, &ValidationError{err}
	}
	return cfg, nil
}

// ApplyChanges edits TOML text in place. Each change replaces the value of an
// existing key (keeping a trailing comment), or adds the key to its table, or
// adds the table. The edit is verified by decoding: the result must differ
// from the input exactly by the requested values, otherwise an error is
// returned and the caller should ask the user to edit the file manually.
func ApplyChanges(data []byte, changes []Change) ([]byte, error) {
	text := string(data)
	var want map[string]any
	if _, err := toml.Decode(text, &want); err != nil {
		return nil, fmt.Errorf("the configuration file does not parse: %w", err)
	}
	for _, ch := range changes {
		path, err := keyPath(ch.Key)
		if err != nil {
			return nil, err
		}
		v, err := decodeLiteral(ch.Literal)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid value %s: %w", ch.Key, ch.Literal, err)
		}
		if text, err = setInText(text, path, ch.Literal); err != nil {
			return nil, fmt.Errorf("cannot update %s automatically (%v); use `vario config edit`", ch.Key, err)
		}
		setInMap(want, path, v)
	}
	var got map[string]any
	if _, err := toml.Decode(text, &got); err != nil {
		return nil, fmt.Errorf("internal error: edited configuration does not parse (%v); use `vario config edit`", err)
	}
	if !reflect.DeepEqual(normalize(want), normalize(got)) {
		return nil, errors.New("cannot update the configuration file automatically (unusual layout); use `vario config edit`")
	}
	return []byte(text), nil
}

// keyLoc locates a key: a plain table ("clickhouse") or a [[sources]] entry
// selected by its kind.
type keyLoc struct {
	table string
	kind  string // set for [[sources]]
	key   string
}

func keyPath(name string) (keyLoc, error) {
	parts := strings.Split(name, ".")
	switch {
	case len(parts) == 2 && parts[0] != "sources":
		return keyLoc{table: parts[0], key: parts[1]}, nil
	case len(parts) == 3 && parts[0] == "sources":
		return keyLoc{table: "sources", kind: parts[1], key: parts[2]}, nil
	}
	return keyLoc{}, fmt.Errorf("unknown configuration key %q", name)
}

func decodeLiteral(lit string) (any, error) {
	var m map[string]any
	if _, err := toml.Decode("v = "+lit, &m); err != nil {
		return nil, err
	}
	return m["v"], nil
}

func setInMap(m map[string]any, k keyLoc, v any) {
	if k.kind == "" {
		t, _ := m[k.table].(map[string]any)
		if t == nil {
			t = map[string]any{}
			m[k.table] = t
		}
		t[k.key] = v
		return
	}
	arr, _ := m[k.table].([]map[string]any)
	for _, e := range arr {
		if e["kind"] == k.kind {
			e[k.key] = v
			return
		}
	}
	m[k.table] = append(arr, map[string]any{"kind": k.kind, k.key: v})
}

// normalize converts decoded TOML into comparable values ([]map and []any of
// tables compare equal).
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalize(e)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	}
	return v
}

var (
	headerRe      = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_\-]+)\s*\]\s*(#.*)?$`)
	arrayHeaderRe = regexp.MustCompile(`^\s*\[\[\s*([A-Za-z0-9_\-]+)\s*\]\]\s*(#.*)?$`)
	anyHeaderRe   = regexp.MustCompile(`^\s*\[`)
)

type section struct {
	name       string
	array      bool
	start, end int // start is the header line; body is (start, end)
}

// editor works on lines. A line only counts as the start of a statement when
// the text before it is a complete TOML document; this keeps header- or
// key-like lines inside multi-line strings and arrays from being mistaken for
// real ones without a hand-written lexer.
type editor struct{ lines []string }

func (e *editor) boundary(i int) bool {
	var m map[string]any
	_, err := toml.Decode(strings.Join(e.lines[:i], "\n"), &m)
	return err == nil
}

func (e *editor) sections() ([]section, error) {
	var out []section
	cur := section{start: -1}
	for i, l := range e.lines {
		if !anyHeaderRe.MatchString(l) || !e.boundary(i) {
			continue
		}
		cur.end = i
		out = append(out, cur)
		switch {
		case arrayHeaderRe.MatchString(l):
			cur = section{name: arrayHeaderRe.FindStringSubmatch(l)[1], array: true, start: i}
		case headerRe.MatchString(l):
			cur = section{name: headerRe.FindStringSubmatch(l)[1], start: i}
		default:
			return nil, fmt.Errorf("unsupported table header %q", strings.TrimSpace(l))
		}
	}
	cur.end = len(e.lines)
	return append(out, cur), nil
}

func setInText(text string, k keyLoc, literal string) (string, error) {
	e := &editor{lines: strings.Split(text, "\n")}
	secs, err := e.sections()
	if err != nil {
		return "", err
	}
	if root := secs[0]; root.end > 0 {
		var m map[string]any
		if _, err := toml.Decode(strings.Join(e.lines[:root.end], "\n"), &m); err == nil && len(m) > 0 {
			return "", errors.New("top-level keys outside a table are not supported")
		}
	}
	var target *section
	for i := range secs {
		s := &secs[i]
		if s.name != k.table || s.array != (k.kind != "") {
			continue
		}
		if k.kind == "" {
			target = s
			break
		}
		var body struct{ Kind string }
		if _, err := toml.Decode(strings.Join(e.lines[s.start+1:s.end], "\n"), &body); err == nil && body.Kind == k.kind {
			target = s
			break
		}
	}
	line := k.key + " = " + literal
	if target == nil {
		block := []string{"[" + k.table + "]", line}
		if k.kind != "" {
			block = []string{"[[" + k.table + "]]", "kind = " + tomlString(k.kind), line}
		}
		return appendBlock(e.lines, block), nil
	}
	keyRe := regexp.MustCompile(`^(\s*)(` + regexp.QuoteMeta(k.key) + `|"` + regexp.QuoteMeta(k.key) + `")\s*=(.*)$`)
	for j := target.start + 1; j < target.end; j++ {
		m := keyRe.FindStringSubmatch(e.lines[j])
		if m == nil || !e.boundary(j) {
			continue
		}
		// The value ends at the first following line at which the document parses again.
		end := -1
		for n := j + 1; n <= target.end; n++ {
			if e.boundary(n) {
				end = n
				break
			}
		}
		if end < 0 {
			return "", errors.New("cannot determine where the current value ends")
		}
		repl := m[1] + line
		if end == j+1 {
			if c := trailingComment(m[3]); c != "" {
				repl += " " + c
			}
		}
		e.lines = append(e.lines[:j], append([]string{repl}, e.lines[end:]...)...)
		return strings.Join(e.lines, "\n"), nil
	}
	// Insert after the last statement of the table.
	at := target.start + 1
	for j := target.end - 1; j > target.start; j-- {
		t := strings.TrimSpace(e.lines[j])
		if t != "" && !strings.HasPrefix(t, "#") {
			at = j + 1
			break
		}
	}
	if target.start < 0 {
		return "", errors.New("no table header found")
	}
	e.lines = append(e.lines[:at], append([]string{line}, e.lines[at:]...)...)
	return strings.Join(e.lines, "\n"), nil
}

// trailingComment returns the comment after a single-line value, if any.
func trailingComment(rest string) string {
	for i := 0; i < len(rest); i++ {
		if rest[i] != '#' {
			continue
		}
		var m map[string]any
		if _, err := toml.Decode("v = "+rest[:i], &m); err == nil {
			return strings.TrimSpace(rest[i:])
		}
	}
	return ""
}

func appendBlock(lines, block []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, block...), "\n") + "\n"
}

// lockFile serializes writers of a configuration file across processes with
// an exclusive lock file. A lock older than 30 seconds is considered stale
// (configuration writes take milliseconds).
func lockFile(path string) (func(), error) {
	lp := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
			f.Close()
			return func() { os.Remove(lp) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock configuration: %w", err)
		}
		if st, err := os.Stat(lp); err == nil && time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(lp)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("configuration %s is being changed by another vario process (if none is running, remove %s)", path, lp)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
