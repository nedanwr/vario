// Package privacy decides which content may leave the machine and redacts secrets from it.
package privacy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nedanwr/vario/internal/config"
)

// Policy is applied by sources during normalization, before anything is spooled.
type Policy struct {
	mode          string
	rules         []rule
	maxFieldBytes int
	fingerprint   string
}

type rule struct {
	name string
	re   *regexp.Regexp
	repl string
	// skip reports matches that are clearly not secrets, given the match's submatches.
	skip func(sub []string) bool
	// hints are lowercase literals of which at least one must occur in the
	// text for the rule to match at all. They let Redact skip a regular
	// expression scan, which dominates redaction cost.
	hints []string
}

// secretName matches identifiers that name a credential, including prefixed
// environment-variable and camelCase forms such as DB_PASSWORD,
// AWS_SECRET_ACCESS_KEY, x-api-key and accessToken.
const secretName = `\b[a-z0-9_.-]*?(?:password|passwd|secret|api[_-]?key|access[_-]?key|secret[_-]?key|access[_-]?token|auth[_-]?token|refresh[_-]?token|client[_-]?secret|private[_-]?key|token)`

// builtinRules target credentials with recognizable shapes. Order matters:
// more specific patterns run first so their labels win.
var builtinRules = []rule{
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "", nil, nil},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`), "", nil, []string{"sk-ant-"}},
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}`), "", nil, []string{"sk-"}},
	{"aws_access_key_id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "", nil, []string{"akia", "asia"}},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`), "", nil, []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}},
	{"slack_token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), "", nil, []string{"xox"}},
	{"stripe_key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`), "", nil, []string{"_live_", "_test_"}},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), "", nil, []string{"aiza"}},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "", nil, []string{"eyj"}},
	{"bearer_token", regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/\-]{16,}=*`), "${1}[REDACTED:bearer_token]", nil, []string{"bearer"}},
	{"authorization", regexp.MustCompile(`(?i)\b(authorization(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?(?:basic|digest|token)?\s*)([A-Za-z0-9._~+/\-]{8,}=*)`), "${1}[REDACTED:authorization]", nil, []string{"authorization"}},
	{"url_credentials", regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@"'\\]+:)[^\s@/"'\\]+@`), "${1}[REDACTED:url_credentials]@", nil, []string{"://"}},
	{"secret_assignment", regexp.MustCompile(`(?i)(` + secretName + `(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)([^\s"'\\,;}]{6,})`), "${1}[REDACTED:secret_assignment]", placeholderValue, []string{"pass", "secret", "key", "token"}},
}

var shellVarRe = regexp.MustCompile(`^\$\{?[A-Za-z_][A-Za-z0-9_]*\}?$`)

var nonSecretValues = map[string]bool{
	"string": true, "string;": true, "string)": true, "number": true, "boolean": true, "undefined": true,
	"optional": true, "required": true, "redacted": true, "string[]": true, "******": true, "********": true,
}

// placeholderValue skips type annotations, environment lookups and template
// references, which name a secret without containing it.
func placeholderValue(sub []string) bool {
	raw := sub[len(sub)-1]
	v := strings.ToLower(raw)
	if nonSecretValues[v] || shellVarRe.MatchString(raw) {
		return true
	}
	for _, p := range []string{"process.env", "os.environ", "os.getenv", "env.", "env[", "<", "{{", "[redacted", "z.string", "string."} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// New builds a policy from configuration. The configuration must already be validated.
func New(c config.PrivacyConfig) (*Policy, error) {
	p := &Policy{mode: c.Mode, maxFieldBytes: c.MaxFieldBytes}
	h := sha256.New()
	fmt.Fprintf(h, "mode=%s;redact=%t;max=%d;", c.Mode, c.RedactSecrets, c.MaxFieldBytes)
	if c.RedactSecrets {
		for _, r := range builtinRules {
			if r.repl == "" {
				r.repl = "[REDACTED:" + r.name + "]"
			}
			p.rules = append(p.rules, r)
		}
		h.Write([]byte("builtin-v2;"))
	}
	for _, pat := range c.ExtraPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("redaction pattern %q: %w", pat, err)
		}
		p.rules = append(p.rules, rule{name: "custom", re: re, repl: "[REDACTED:custom]"})
		fmt.Fprintf(h, "custom=%s;", pat)
	}
	p.fingerprint = hex.EncodeToString(h.Sum(nil))[:16]
	return p, nil
}

func (p *Policy) Mode() string { return p.mode }

// Messages reports whether message text and conversation titles may be collected.
func (p *Policy) Messages() bool { return p.mode == config.ModeMessages || p.mode == config.ModeFull }

// Full reports whether execution payloads and raw event payloads may be collected.
func (p *Policy) Full() bool { return p.mode == config.ModeFull }

// Fingerprint changes whenever the policy would produce different output.
func (p *Policy) Fingerprint() string { return p.fingerprint }

// Redact replaces secrets in s and returns the number of replacements.
func (p *Policy) Redact(s string) (string, int) {
	n := 0
	var lower []byte
	for _, r := range p.rules {
		var matches [][]int
		if len(r.hints) == 0 {
			matches = r.re.FindAllStringSubmatchIndex(s, -1)
		} else {
			if lower == nil {
				lower = asciiLower(s)
			}
			matches = r.findNearHints(s, lower)
		}
		if matches == nil {
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range matches {
			if r.skip != nil {
				sub := make([]string, 0, len(m)/2)
				for i := 0; i < len(m); i += 2 {
					if m[i] >= 0 {
						sub = append(sub, s[m[i]:m[i+1]])
					} else {
						sub = append(sub, "")
					}
				}
				if r.skip(sub) {
					continue
				}
			}
			b.WriteString(s[last:m[0]])
			b.Write(r.re.ExpandString(nil, r.repl, s, m))
			last = m[1]
			n++
		}
		if last > 0 {
			b.WriteString(s[last:])
			s = b.String()
			lower = nil // recompute for the modified text
		}
	}
	return s, n
}

// findNearHints returns the rule's matches, scanning only the lines that
// contain one of its hints plus the line after each (so a value may follow
// its name on the next line). Every match of a hinted rule contains a hint on
// the line where the match starts, so this finds the same matches as a scan
// of the whole text while skipping most of it.
func (r rule) findNearHints(s string, lower []byte) [][]int {
	next := make([]int, len(r.hints)) // next occurrence of each hint at or after from
	for i := range next {
		next[i] = -2 // unknown
	}
	hintFrom := func(from, to int) int {
		best := -1
		for i, h := range r.hints {
			if next[i] == -2 || (next[i] >= 0 && next[i] < from) {
				next[i] = -1
				if from <= len(lower) {
					if j := bytes.Index(lower[from:], []byte(h)); j >= 0 {
						next[i] = from + j
					}
				}
			}
			if next[i] >= 0 && next[i] < to && (best < 0 || next[i] < best) {
				best = next[i]
			}
		}
		return best
	}
	var out [][]int
	end := 0
	for {
		pos := hintFrom(end, len(lower))
		if pos < 0 {
			return out
		}
		start := max(bytes.LastIndexByte(lower[:pos], '\n')+1, end)
		lineStop := lineEnd(lower, pos)
		stop := lineEnd(lower, lineStop+1)
		for {
			q := hintFrom(lineStop+1, stop)
			if q < 0 {
				break
			}
			lineStop = lineEnd(lower, q)
			stop = lineEnd(lower, lineStop+1)
		}
		for _, m := range r.re.FindAllStringSubmatchIndex(s[start:stop], -1) {
			for i := range m {
				if m[i] >= 0 {
					m[i] += start
				}
			}
			out = append(out, m)
		}
		end = stop
	}
}

// lineEnd returns the index of the first newline at or after i, or len(b).
func lineEnd(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	if j := bytes.IndexByte(b[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(b)
}

// asciiLower lowercases ASCII letters only, keeping byte offsets aligned with s.
func asciiLower(s string) []byte {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return b
}

// Text redacts and then truncates free text to the configured limit.
func (p *Policy) Text(s string) (out string, redactions int, truncated bool) {
	out, redactions = p.Redact(s)
	out, truncated = Truncate(out, p.maxFieldBytes)
	return out, redactions, truncated
}

// JSON redacts a serialized JSON document. Documents that would become invalid
// or exceed the size limit are dropped rather than stored partially.
func (p *Policy) JSON(s string) (out string, redactions int, dropped bool) {
	if s == "" {
		return "", 0, false
	}
	out, redactions = p.Redact(s)
	if !json.Valid([]byte(out)) || (p.maxFieldBytes > 0 && len(out) > 4*p.maxFieldBytes) {
		return "", redactions, true
	}
	return out, redactions, false
}

// Truncate shortens s to at most max bytes on a UTF-8 boundary. max <= 0 disables truncation.
func Truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], ""), true
}
