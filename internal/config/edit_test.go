package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sample = `# Vario configuration (hand-edited)

[collector]
poll_interval = "5s"   # how often
batch_size = 500

[privacy]
mode = "metrics"
extra_redaction_patterns = [
  "ACME-[0-9]+",   # customer ids
  "[clickhouse]",
]

[clickhouse]
url = "https://ch.example.com:8443"
database = "vario"
note_like = """
[clickhouse]
database = "not-a-key"
"""

[[sources]]
kind = "t3code"
enabled = true
path = "/data/statev2.sqlite"
`

func apply(t *testing.T, text string, ch ...Change) string {
	t.Helper()
	out, err := ApplyChanges([]byte(text), ch)
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	return string(out)
}

func TestApplyChangesPreservesEverythingElse(t *testing.T) {
	got := apply(t, sample,
		Change{"collector.poll_interval", `"10s"`},
		Change{"clickhouse.database", `"telemetry"`},
		Change{"clickhouse.create_database", "true"},
		Change{"sources.t3code.path", `"/new/statev2.sqlite"`},
	)
	for _, want := range []string{
		"# Vario configuration (hand-edited)",
		`poll_interval = "10s" # how often`, // trailing comment kept
		`database = "telemetry"`,
		"create_database = true",
		`database = "not-a-key"`, // inside a multi-line string: untouched
		`"ACME-[0-9]+",   # customer ids`,
		`path = "/new/statev2.sqlite"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("result missing %q:\n%s", want, got)
		}
	}
	// Exactly the requested lines changed.
	if n := len(strings.Split(got, "\n")) - len(strings.Split(sample, "\n")); n != 1 {
		t.Fatalf("expected one added line (create_database), got %d:\n%s", n, got)
	}
	// create_database is inserted into [clickhouse], after its last statement.
	if i, j := strings.Index(got, "create_database"), strings.Index(got, "[[sources]]"); i < 0 || i > j {
		t.Fatalf("create_database placed outside [clickhouse]:\n%s", got)
	}
}

func TestApplyChangesMultilineAndNewTables(t *testing.T) {
	got := apply(t, sample, Change{"privacy.extra_redaction_patterns", `["X-[0-9]+"]`}, Change{"spool.max_size_mb", "64"})
	if strings.Contains(got, "ACME") || !strings.Contains(got, `extra_redaction_patterns = ["X-[0-9]+"]`) {
		t.Fatalf("multi-line value not replaced as a whole:\n%s", got)
	}
	if !strings.HasSuffix(got, "[spool]\nmax_size_mb = 64\n") {
		t.Fatalf("missing table should be appended:\n%s", got)
	}
	// A source block is added when missing.
	got = apply(t, "[clickhouse]\nurl = \"https://x\"\n", Change{"sources.t3code.enabled", "true"}, Change{"sources.t3code.path", `"/p"`})
	if !strings.Contains(got, "[[sources]]\nkind = \"t3code\"\nenabled = true\npath = \"/p\"\n") {
		t.Fatalf("source block:\n%s", got)
	}
	// Empty file.
	if got := apply(t, "", Change{"clickhouse.url", `"https://x"`}); got != "[clickhouse]\nurl = \"https://x\"\n" {
		t.Fatalf("empty file: %q", got)
	}
}

func TestApplyChangesRefusesUnusualLayouts(t *testing.T) {
	for name, text := range map[string]string{
		"inline table": "clickhouse = { url = \"https://x\" }\n",
		"dotted keys":  "clickhouse.url = \"https://x\"\n",
		"invalid toml": "[clickhouse\n",
	} {
		if _, err := ApplyChanges([]byte(text), []Change{{"clickhouse.url", `"https://y"`}}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := ApplyChanges([]byte(sample), []Change{{"nonsense", `"x"`}}); err == nil {
		t.Error("unknown key should fail")
	}
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vario", "config.toml")
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = "# keep\n[clickhouse]\nurl = \"https://ch.example.com\"\n"

func TestUpdate(t *testing.T) {
	p := writeConfig(t, minimal)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Update(p, cfg.Fingerprint(), []Change{{"clickhouse.database", `"other"`}})
	if err != nil || got.ClickHouse.Database != "other" {
		t.Fatalf("update: %+v %v", got, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "# keep\n") {
		t.Fatal("comment lost")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", st.Mode().Perm())
		}
	}

	// A stale fingerprint means someone else changed the file: refuse.
	before, _ := os.ReadFile(p)
	if _, err := Update(p, cfg.Fingerprint(), []Change{{"clickhouse.username", `"x"`}}); !errors.Is(err, ErrModified) {
		t.Fatalf("expected ErrModified, got %v", err)
	}
	// An invalid result is never written.
	var ve *ValidationError
	if _, err := Update(p, "", []Change{{"collector.batch_size", "0"}}); !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("failed updates must not modify the file")
	}
	if _, err := Update(filepath.Join(t.TempDir(), "missing.toml"), "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	// No leftover temporary or lock files.
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestUpdateFailureLeavesFileIntact(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	p := writeConfig(t, minimal)
	dir := filepath.Dir(p)
	// The lock and temporary file cannot be created in a read-only directory.
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if _, err := Update(p, "", []Change{{"clickhouse.database", `"other"`}}); err == nil {
		t.Fatal("expected an error")
	}
	if b, _ := os.ReadFile(p); string(b) != minimal {
		t.Fatal("file changed despite the failure")
	}
}

func TestLockSerializesWriters(t *testing.T) {
	p := writeConfig(t, minimal)
	unlock, err := lockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Update(p, "", []Change{{"clickhouse.database", `"x"`}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("update must wait for the lock, finished with %v", err)
	default:
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("update after unlock: %v", err)
	}
	// A stale lock (left by a crashed process) is taken over.
	os.WriteFile(p+".lock", []byte("999999\n"), 0o600)
	old := mustTime(t, "2000-01-01T00:00:00Z")
	os.Chtimes(p+".lock", old, old)
	if _, err := Update(p, "", []Change{{"clickhouse.database", `"y"`}}); err != nil {
		t.Fatalf("stale lock should be ignored: %v", err)
	}
}

func TestCreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new", "config.toml")
	c := valid()
	c.SetPath(p)
	if _, err := c.Create(false); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Dir(p))
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %o", st.Mode().Perm())
		}
	}
	if _, err := c.Create(false); !errors.Is(err, ErrExists) {
		t.Fatalf("existing file must not be overwritten: %v", err)
	}
	backup, err := c.Create(true)
	if err != nil || backup == "" {
		t.Fatalf("replace: %q %v", backup, err)
	}
	if _, err := Load(backup); err != nil {
		t.Fatalf("backup should be a valid copy: %v", err)
	}
}

func TestKeysRoundTrip(t *testing.T) {
	samples := map[string]string{
		"collector.poll_interval": "7s", "collector.reconcile_interval": "0s", "collector.batch_size": "42",
		"collector.data_dir": "/var/vario", "collector.log_level": "debug",
		"privacy.mode": "full", "privacy.allow_sensitive_content": "true", "privacy.redact_secrets": "false",
		"privacy.max_field_bytes": "100",
		"clickhouse.url":          "https://h:8443", "clickhouse.database": "db1", "clickhouse.create_database": "true",
		"clickhouse.username": "u", "clickhouse.password_file": "/etc/pw", "clickhouse.timeout": "3s",
		"clickhouse.flush_interval": "1s", "clickhouse.max_insert_rows": "9", "clickhouse.retry_initial_backoff": "1s",
		"clickhouse.retry_max_backoff": "1m0s", "clickhouse.ca_file": "/etc/ca.pem", "clickhouse.tls_insecure_skip_verify": "true",
		"clickhouse.allow_insecure_http": "true", "spool.max_size_mb": "64",
		"sources.t3code.enabled": "true", "sources.t3code.path": "/x/statev2.sqlite",
	}
	p := writeConfig(t, "[clickhouse]\nurl = \"https://ch.example.com\"\n")
	cfg, _ := Load(p)
	next := cfg.Clone()
	for _, k := range Keys() {
		if k.ReadOnly {
			continue
		}
		v, ok := samples[k.Name]
		if !ok {
			t.Errorf("no sample for %s", k.Name)
			continue
		}
		if err := k.Set(next, v); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
		if got := k.Value(next); got != v {
			t.Errorf("%s: value %q after setting %q", k.Name, got, v)
		}
	}
	got, err := Update(p, cfg.Fingerprint(), Diff(cfg, next))
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(next, got); len(d) != 0 {
		t.Fatalf("values did not survive a save/load round trip: %v", d)
	}
	if _, ok := LookupKey(PasswordKey); ok {
		t.Fatal("the password must not be a settable key")
	}
	if err := (Key{Name: "x", ReadOnly: true}).Set(next, "1"); err == nil {
		t.Fatal("read-only keys must refuse Set")
	}
}

func TestParseTracksDefinedKeys(t *testing.T) {
	cfg, err := Parse("x.toml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsSet("clickhouse.url") || cfg.IsSet("clickhouse.database") {
		t.Fatal("IsSet should reflect the file")
	}
	if cfg.Fingerprint() == "" || cfg.Fingerprint() != FingerprintBytes([]byte(minimal)) {
		t.Fatal("fingerprint mismatch")
	}
	if cfg.ClickHouse.CreateDatabase {
		t.Fatal("create_database must default to false for existing configurations")
	}
}
