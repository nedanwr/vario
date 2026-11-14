package privacy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nedanwr/vario/internal/config"
)

func newPolicy(t testing.TB, mutate func(*config.PrivacyConfig)) *Policy {
	t.Helper()
	c := config.Default().Privacy
	if mutate != nil {
		mutate(&c)
	}
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRedactBuiltins(t *testing.T) {
	p := newPolicy(t, nil)
	cases := map[string]string{
		"anthropic_key":     "key sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA end",
		"openai_key":        "OPENAI=sk-proj-abcdefghijklmnopqrstuvwxyz123456",
		"aws_access_key_id": "AKIAIOSFODNN7EXAMPLE",
		"github_token":      "token ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ",
		"slack_token":       "xoxb-123456789012-abcdefghij",
		"stripe_key":        "sk_live_abcdefghijklmnop1234",
		"google_api_key":    "AIzaSyA-abcdefghijklmnopqrstuvwxyz12345",
		"jwt":               "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"bearer_token":      "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
		"url_credentials":   "postgres://admin:hunter2pass@db.example.com:5432/app",
		"secret_assignment": `password = "correct-horse-battery"`,
		"private_key":       "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	for name, in := range cases {
		out, n := p.Redact(in)
		if n == 0 || !strings.Contains(out, "[REDACTED:") {
			t.Errorf("%s: not redacted: %q", name, out)
		}
		for _, secret := range []string{"AAAAAAAAAAAAAAAAAAAA", "abcdefghijklmnopqrstuvwxyz123456", "IOSFODNN7", "hunter2pass", "correct-horse", "MIIEow", "dozjgNryP4J3"} {
			if strings.Contains(in, secret) && strings.Contains(out, secret) {
				t.Errorf("%s: secret %q survived: %q", name, secret, out)
			}
		}
	}
	if out, _ := p.Redact("postgres://admin:hunter2pass@db"); !strings.HasPrefix(out, "postgres://admin:[REDACTED:url_credentials]@") {
		t.Errorf("url credentials should keep the user: %q", out)
	}
}

func TestRedactSkipsPlaceholders(t *testing.T) {
	p := newPolicy(t, nil)
	for _, in := range []string{
		"password: string;",
		"password = process.env.DB_PASSWORD",
		`password="$DB_PASSWORD"`,
		"secret: ${SECRET}",
		"just some ordinary prose about passwords",
	} {
		if out, n := p.Redact(in); n != 0 {
			t.Errorf("false positive for %q: %q", in, out)
		}
	}
}

func TestRedactDisabledAndCustom(t *testing.T) {
	off := newPolicy(t, func(c *config.PrivacyConfig) { c.RedactSecrets = false })
	if _, n := off.Redact("AKIAIOSFODNN7EXAMPLE"); n != 0 {
		t.Fatal("redaction should be disabled")
	}
	custom := newPolicy(t, func(c *config.PrivacyConfig) {
		c.RedactSecrets = false
		c.ExtraPatterns = []string{`ACME-\d{4}`}
	})
	out, n := custom.Redact("ticket ACME-1234 and ACME-5678")
	if n != 2 || strings.Contains(out, "ACME-") {
		t.Fatalf("custom pattern: %q %d", out, n)
	}
	if off.Fingerprint() == custom.Fingerprint() || off.Fingerprint() == newPolicy(t, nil).Fingerprint() {
		t.Fatal("fingerprint must change with policy")
	}
}

func TestJSONStaysValid(t *testing.T) {
	p := newPolicy(t, nil)
	doc := `{"cmd":"curl -H \"Authorization: Bearer abcdefghijklmnopqrstuvwxyz\" https://u:pw12345@x.io","env":{"password":"hunter2hunter2"}}`
	out, n, dropped := p.JSON(doc)
	if dropped || n < 2 || !json.Valid([]byte(out)) || strings.Contains(out, "hunter2") {
		t.Fatalf("JSON redaction: %q n=%d dropped=%t", out, n, dropped)
	}
	big := `{"x":"` + strings.Repeat("a", 300*1024) + `"}`
	if _, _, dropped := p.JSON(big); !dropped {
		t.Fatal("oversized JSON should be dropped")
	}
}

func TestTextTruncatesOnRuneBoundary(t *testing.T) {
	p := newPolicy(t, func(c *config.PrivacyConfig) { c.MaxFieldBytes = 10 })
	out, _, truncated := p.Text("ééééééééé")
	if !truncated || len(out) > 10 || !utf8.ValidString(out) {
		t.Fatalf("truncate: %q %t", out, truncated)
	}
	if out, truncated := Truncate("short", 0); truncated || out != "short" {
		t.Fatal("max 0 disables truncation")
	}
}

func TestModes(t *testing.T) {
	m := newPolicy(t, nil)
	if m.Messages() || m.Full() {
		t.Fatal("metrics mode collects no content")
	}
	msg := newPolicy(t, func(c *config.PrivacyConfig) { c.Mode = config.ModeMessages })
	if !msg.Messages() || msg.Full() {
		t.Fatal("messages mode")
	}
	full := newPolicy(t, func(c *config.PrivacyConfig) { c.Mode = config.ModeFull })
	if !full.Messages() || !full.Full() {
		t.Fatal("full mode")
	}
}

// Environment-variable style names (prefixed keywords), generic tokens and
// non-bearer authorization headers are redacted.
func TestRedactAssignmentsAndHeaders(t *testing.T) {
	p := newPolicy(t, nil)
	for in, secret := range map[string]string{
		"export DB_PASSWORD=s3cr3t-value-123":                         "s3cr3t-value-123",
		"POSTGRES_PASSWORD: 'pg-pass-9876'":                           "pg-pass-9876",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE": "wJalrXUtnFEMI",
		"MY_SERVICE_TOKEN=abc123def456ghi":                            "abc123def456",
		`{"token": "tok_9f8e7d6c5b4a"}`:                               "tok_9f8e7d6c5b4a",
		`{\"DB_PASSWORD\":\"escaped-secret-1\"}`:                      "escaped-secret-1",
		"Authorization: Basic dXNlcjpodW50ZXIycGFzcw==":               "dXNlcjpodW50ZXIycGFzcw",
		"x-api-key: 0123456789abcdef0123":                             "0123456789abcdef",
		"auth_token=qwertyuiop1234":                                   "qwertyuiop1234",
	} {
		out, n := p.Redact(in)
		if n == 0 || strings.Contains(out, secret) {
			t.Errorf("not redacted: %q -> %q", in, out)
		}
	}
	for _, in := range []string{
		"input_tokens: 818885",
		"max_tokens=4096",
		"const tokenizer = newTokenizer()",
		"A basic configuration works",
		"cd $HOME && pwd=/home/dev",
		"token_count = 12345678",
		"password: string;",
		"api_key = os.environ['API_KEY']",
		"token: ${GITHUB_TOKEN}",
	} {
		if out, n := p.Redact(in); n != 0 {
			t.Errorf("false positive: %q -> %q", in, out)
		}
	}
}

// naiveRedact is the reference implementation: every rule scans the whole text.
func naiveRedact(p *Policy, s string) (string, int) {
	n := 0
	for _, r := range p.rules {
		var b strings.Builder
		last := 0
		for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
			if r.skip != nil {
				sub := []string{}
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
		}
	}
	return s, n
}

func TestHintScanMatchesFullScan(t *testing.T) {
	p := newPolicy(t, func(c *config.PrivacyConfig) { c.ExtraPatterns = []string{"ACME-[0-9]{6}"} })
	corpus := []string{
		"password =\n  hunter2-next-line\nother",
		"x\nAuthorization:\nBasic dXNlcjpodW50ZXIy\n",
		"İSTANBUL password=ünïcødé-sécret ẞ token: ÄÖÜäöü123456",
		"line1 key\nline2 key\nline3 api_key = abcdefgh\nline4 = zzzzzzzzz",
		"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\npostgres://u:p4ssw0rd@h/db ACME-123456 eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijkl",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\npassword=inner-secret\n-----END RSA PRIVATE KEY-----",
		"TOKEN=", "token", "\n\n\n", "",
	}
	files, _ := filepath.Glob("../*/*.go")
	files2, _ := filepath.Glob("../*/*/*.go")
	for _, f := range append(files, files2...) {
		b, _ := os.ReadFile(f)
		corpus = append(corpus, string(b))
	}
	for i, s := range corpus {
		got, gn := p.Redact(s)
		want, wn := naiveRedact(p, s)
		if got != want || gn != wn {
			t.Fatalf("corpus %d: hint scan differs from full scan (%d vs %d redactions)", i, gn, wn)
		}
	}
}
