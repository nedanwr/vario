package cli

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	cct "github.com/nedanwr/vario/internal/sources/claudecode/claudecodetest"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

// claudeFixture writes synthetic Claude Code transcripts and makes them the
// default location ($CLAUDE_CONFIG_DIR) for the test.
func claudeFixture(t *testing.T) *cct.Fixture {
	t.Helper()
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Conversation("a")
	sub := s.Subagent("s1", "toolu_a")
	sub.Append(sub.Prompt("explore"))
	sub.Append(sub.Response("msg_sub", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 2}, cct.Text("ok"))...)
	t.Setenv("CLAUDE_CONFIG_DIR", f.Config)
	return f
}

func TestInitInteractiveOffersDetectedClaudeCode(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	cf := claudeFixture(t)
	r := vario(t, answers(
		unreachable, // endpoint
		"2",         // manually managed setup
		"vario",     // database
		"default",   // operational user
		"",          // password
		"n",         // do not verify the connection
		"1",         // metrics mode
		"",          // enable T3 Code (detected)
		"",          // keep detected path
		"",          // enable Claude Code (detected)
		"",          // keep detected path
		"",          // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("interactive init: %+v", r)
	}
	for _, want := range []string{"[x] Claude Code — detected at", "Enable Claude Code?", "Claude Code data directory",
		"vario backfill --source t3code", "vario backfill --source claudecode"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if sc, ok := cfg.Source(config.SourceClaudeCode); !ok || !sc.Enabled || sc.Path != cf.Root {
		t.Fatalf("claudecode source: %+v", cfg.Sources)
	}
	if sc, ok := cfg.Source(config.SourceT3Code); !ok || !sc.Enabled {
		t.Fatalf("t3code source: %+v", cfg.Sources)
	}
}

func TestInitWithoutClaudeCodeDoesNotAsk(t *testing.T) {
	isolate(t)
	// Not installed: listed, but no question is asked (the scripted answers
	// of earlier versions stay valid).
	r := vario(t, answers(unreachable, "2", "vario", "default", "", "n", "1", "n", ""), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "[ ] Claude Code — not detected") || strings.Contains(r.stdout, "Enable Claude Code") {
		t.Fatalf("init: %+v", r)
	}
}

func TestInitNonInteractiveClaudeCode(t *testing.T) {
	cfgPath, data := isolate(t)
	cf := claudeFixture(t)
	r := vario(t, "", "init", "--non-interactive", "--clickhouse-url", unreachable, "--skip-connection-check", "--data-dir", data)
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	cfg, _ := config.Load(cfgPath)
	if sc, ok := cfg.Source(config.SourceClaudeCode); !ok || !sc.Enabled || sc.Path != cf.Root {
		t.Fatalf("a detected Claude Code should be enabled on a fresh init: %+v", cfg.Sources)
	}
	if r := vario(t, "", "init", "--non-interactive", "--update", "--no-claudecode", "--skip-connection-check"); r.code != 0 {
		t.Fatalf("update: %+v", r)
	}
	cfg, _ = config.Load(cfgPath)
	if sc, _ := cfg.Source(config.SourceClaudeCode); sc.Enabled {
		t.Fatalf("--no-claudecode: %+v", cfg.Sources)
	}
	if r := vario(t, "", "init", "--non-interactive", "--update", "--claudecode-path", cf.Config, "--skip-connection-check"); r.code != 0 {
		t.Fatalf("update: %+v", r)
	}
	cfg, _ = config.Load(cfgPath)
	if sc, _ := cfg.Source(config.SourceClaudeCode); !sc.Enabled || sc.Path != cf.Root {
		t.Fatalf("--claudecode-path: %+v", cfg.Sources)
	}
}

var queuedRe = regexp.MustCompile(`Queued (\d+) records`)

// An existing T3 Code installation enables Claude Code without touching its
// destination or its T3 Code data.
func TestEnableClaudeCodeForExistingT3User(t *testing.T) {
	_, ch := replacementFixture(t)
	migrateAsAdmin(t)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 {
		t.Fatalf("t3 backfill: %+v", r)
	}
	t3Turns, t3Messages := ch.inserted("vario", "provider_turns"), ch.inserted("vario", "messages")
	dest := ch.destinationID("vario")

	cf := claudeFixture(t)
	r := vario(t, "", "sources")
	if r.code != 0 || !strings.Contains(r.stdout, "Claude Code (claudecode)") || !strings.Contains(r.stdout, "configured: no (enable with `vario sources add claudecode`)") ||
		!strings.Contains(r.stdout, "✓ "+cf.Root) || !strings.Contains(r.stdout, "sessions: 1") {
		t.Fatalf("sources: %+v", r)
	}
	if r := vario(t, "", "backfill", "--source", "claudecode"); r.code != ExitUsage || !strings.Contains(r.stderr, "vario sources add claudecode") {
		t.Fatalf("backfill of an unconfigured source: %+v", r)
	}
	if r := vario(t, "", "sources", "add", "claudecode"); r.code != 0 || !strings.Contains(r.stdout, "Source claudecode enabled") {
		t.Fatalf("add: %+v", r)
	}
	r = vario(t, "", "backfill", "--source", "claudecode")
	if r.code != 0 {
		t.Fatalf("claudecode backfill: %+v", r)
	}
	m := queuedRe.FindStringSubmatch(r.stdout)
	if n, _ := strconv.Atoi(m[1]); n == 0 {
		t.Fatalf("nothing queued: %s", r.stdout)
	}
	if got := ch.inserted("vario", "provider_turns") - t3Turns; got != 3 {
		t.Fatalf("claude code provider turns delivered: %d", got)
	}
	if ch.inserted("vario", "messages") <= t3Messages || ch.destinationID("vario") != dest {
		t.Fatal("claude code rows should go to the same destination")
	}
	// Idempotent.
	if r := vario(t, "", "backfill", "--source", "claudecode"); r.code != 0 || !strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("repeated backfill: %+v", r)
	}
	r = vario(t, "", "status")
	if r.code != 0 || !strings.Contains(r.stdout, "claudecode  enabled=true") || !strings.Contains(r.stdout, "2 transcripts") ||
		!strings.Contains(r.stdout, "t3code  enabled=true") || !strings.Contains(r.stdout, "cursor         seq") {
		t.Fatalf("status: %+v", r)
	}
	r = vario(t, "", "sources", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "claudecode") || !strings.Contains(r.stdout, cf.Root) {
		t.Fatalf("list: %+v", r)
	}
	r = vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "claudecode: "+cf.Root) || !strings.Contains(r.stdout, "claudecode: 2 transcript(s)") {
		t.Fatalf("doctor: %+v", r)
	}
	// Malformed lines are reported, without their content.
	s := cf.Session(cct.DefaultCwd)
	s.Append("{broken " + cct.PromptMarker)
	r = vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "1 line(s) in 1 transcript(s) are not valid JSON") || strings.Contains(r.stdout, cct.PromptMarker) {
		t.Fatalf("doctor with a malformed line: %+v", r)
	}
	// Sessions run by T3 Code are collected from T3 Code; doctor warns
	// when it is not enabled.
	t3 := cf.Session(cct.DefaultCwd)
	t3.Entrypoint = "t3-code"
	t3.Append(t3.DeferredTools(cct.T3CodeTools...))
	t3.Conversation("t3")
	r = vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "1 session(s) run by T3 Code are collected from T3 Code") {
		t.Fatalf("doctor with a T3 Code session: %+v", r)
	}
	if r := vario(t, "", "config", "set", "sources.t3code.enabled", "false"); r.code != 0 {
		t.Fatalf("disable t3code: %+v", r)
	}
	r = vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "1 session(s) were run by T3 Code; they are collected from T3 Code, which is not enabled") ||
		!strings.Contains(r.stdout, "vario sources add t3code") {
		t.Fatalf("doctor with T3 Code disabled: %+v", r)
	}
}

func TestDoctorSuggestsDetectedClaudeCode(t *testing.T) {
	initFixture(t)
	claudeFixture(t)
	r := vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "Claude Code data found at") || !strings.Contains(r.stdout, "vario sources add claudecode") {
		t.Fatalf("doctor: %+v", r)
	}
	if r := vario(t, "", "sources", "add", "claudecode", "--path", t.TempDir()+"/missing"); r.code == 0 {
		t.Fatalf("adding a missing directory should fail: %+v", r)
	}
}
