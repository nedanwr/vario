package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

type result struct {
	code           int
	stdout, stderr string
}

func vario(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return result{code, out.String(), errb.String()}
}

// isolate points Vario at a temporary config and data directory.
func isolate(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.toml")
	t.Setenv(config.EnvConfig, cfgPath)
	t.Setenv(config.EnvPassword, "")
	os.Unsetenv(config.EnvPassword)
	return cfgPath, filepath.Join(dir, "data")
}

const unreachable = "http://127.0.0.1:1"

func initFixture(t *testing.T, extra ...string) (*t3codetest.Fixture, string) {
	t.Helper()
	cfgPath, data := isolate(t)
	fix := t3codetest.New(t)
	fix.Standard()
	args := append([]string{"init", "--non-interactive", "--clickhouse-url", unreachable, "--skip-connection-check",
		"--t3code-path", fix.Path, "--data-dir", data}, extra...)
	if r := vario(t, "", args...); r.code != 0 {
		t.Fatalf("init failed: %+v", r)
	}
	return fix, cfgPath
}

func TestVersionAndUsageErrors(t *testing.T) {
	if r := vario(t, "", "version"); r.code != 0 || !strings.HasPrefix(r.stdout, "vario ") {
		t.Fatalf("version: %+v", r)
	}
	if r := vario(t, "", "frobnicate"); r.code != ExitUsage {
		t.Fatalf("unknown command: %+v", r)
	}
	if r := vario(t, "", "backfill"); r.code != ExitUsage {
		t.Fatalf("missing --source: %+v", r)
	}
	isolate(t)
	if r := vario(t, "", "run"); r.code != ExitUsage || !strings.Contains(r.stderr, "vario init") {
		t.Fatalf("run without config: %+v", r)
	}
}

func TestInitNonInteractive(t *testing.T) {
	fix, cfgPath := initFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Privacy.Mode != config.ModeMetrics || cfg.Privacy.AllowSensitiveContent {
		t.Fatalf("init must default to metrics: %+v", cfg.Privacy)
	}
	if sc, ok := cfg.Source("t3code"); !ok || sc.Path != fix.Path || !sc.Enabled {
		t.Fatalf("source: %+v", cfg.Sources)
	}
	if r := vario(t, "", "init", "--non-interactive", "--clickhouse-url", unreachable); r.code != ExitUsage || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("init must not overwrite silently: %+v", r)
	}
}

func TestInitRequiresOptInAndTLS(t *testing.T) {
	isolate(t)
	if r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--skip-connection-check", "--clickhouse-url", unreachable, "--mode", "full"); r.code != ExitUsage || !strings.Contains(r.stderr, "allow-sensitive-content") {
		t.Fatalf("full mode without opt-in: %+v", r)
	}
	if r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--skip-connection-check", "--clickhouse-url", "http://ch.example.com:8123"); r.code != ExitUsage {
		t.Fatalf("remote http must be refused: %+v", r)
	}
}

func TestInitInteractive(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	answers := strings.Join([]string{
		"y",         // enable T3 Code
		fix.Path,    // database path
		unreachable, // ClickHouse URL
		"analytics", // database
		"collector", // user
		"pa ss",     // password
		"messages",  // mode
		"yes",       // confirm sensitive content
	}, "\n") + "\n"
	r := vario(t, answers, "init", "--skip-connection-check", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("interactive init: %+v", r)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClickHouse.Database != "analytics" || cfg.ClickHouse.Username != "collector" || cfg.Privacy.Mode != config.ModeMessages || !cfg.Privacy.AllowSensitiveContent {
		t.Fatalf("config: %+v %+v", cfg.ClickHouse, cfg.Privacy)
	}
	if pw, _ := cfg.Password(); pw != "pa ss" {
		t.Fatalf("password = %q", pw)
	}
	b, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(b), "pa ss") {
		t.Fatal("password must not be written into the config file")
	}
}

func TestSourcesAddAndList(t *testing.T) {
	cfgPath, data := isolate(t)
	if r := vario(t, "", "sources", "add", "t3code"); r.code != ExitUsage || !strings.Contains(r.stderr, "vario init") {
		t.Fatalf("add without config: %+v", r)
	}
	if r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--skip-connection-check", "--clickhouse-url", unreachable, "--data-dir", data); r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	if r := vario(t, "", "sources", "add", "t3code", "--path", filepath.Join(t.TempDir(), "nope.sqlite")); r.code == 0 {
		t.Fatalf("adding a missing database should fail: %+v", r)
	}
	fix := t3codetest.New(t)
	if r := vario(t, "", "sources", "add", "t3code", "--path", fix.Path); r.code != 0 || !strings.Contains(r.stdout, "enabled") {
		t.Fatalf("add: %+v", r)
	}
	if r := vario(t, "", "sources", "add", "chatgpt"); r.code != ExitUsage {
		t.Fatalf("unsupported source: %+v", r)
	}
	r := vario(t, "", "sources", "list")
	if r.code != 0 || !strings.Contains(r.stdout, fix.Path) || !strings.Contains(r.stdout, "true") {
		t.Fatalf("list: %+v", r)
	}
	if r := vario(t, "", "sources", "detect"); r.code != 0 || !strings.Contains(r.stdout, fix.Path) {
		t.Fatalf("detect should include the configured path: %+v", r)
	}
	_ = cfgPath
}

func TestBackfillSpoolsWhenClickHouseUnreachable(t *testing.T) {
	initFixture(t)
	r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver")
	if r.code != 0 || !strings.Contains(r.stdout, "Queued") {
		t.Fatalf("backfill --no-deliver: %+v", r)
	}
	r = vario(t, "", "backfill", "--source", "t3code")
	if r.code != ExitIncomplete || !strings.Contains(r.stderr, "remain spooled") {
		t.Fatalf("backfill with unreachable ClickHouse should exit %d: %+v", ExitIncomplete, r)
	}
	r = vario(t, "", "status")
	if r.code != 0 || !strings.Contains(r.stdout, "cursor         seq") || !strings.Contains(r.stdout, "messages=2") {
		t.Fatalf("status: %+v", r)
	}
	if r := vario(t, "", "backfill", "--source", "chatgpt"); r.code != ExitUsage {
		t.Fatalf("unsupported source: %+v", r)
	}
}

func TestDoctor(t *testing.T) {
	initFixture(t)
	r := vario(t, "", "doctor")
	if r.code != ExitFailure || !strings.Contains(r.stdout, "✓ t3code") || !strings.Contains(r.stdout, "✗ endpoint unreachable") {
		t.Fatalf("doctor: %+v", r)
	}
}
