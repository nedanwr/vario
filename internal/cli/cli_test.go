package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/service"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

// TestMain isolates every test from the developer's real home directory,
// configuration, T3 Code history and background service.
func TestMain(m *testing.M) {
	if os.Getenv("VARIO_TEST_HELPER") == "1" {
		// Subprocess mode (see runProcess): behave like the vario binary.
		os.Exit(run(strings.Split(os.Getenv("VARIO_TEST_ARGS"), "\x1f"), os.Stdin, os.Stdout, os.Stderr))
	}
	home, err := os.MkdirTemp("", "vario-cli-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	for _, k := range []string{config.EnvConfig, config.EnvHome, config.EnvPassword, "XDG_CONFIG_HOME", "XDG_STATE_HOME", "VISUAL", "EDITOR"} {
		os.Unsetenv(k)
	}
	serviceManager = func() (service.Manager, error) { return nil, service.ErrUnsupported }
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

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

// isolate gives the test its own home directory and points Vario at a
// temporary configuration file.
func isolate(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	home := isolateHome(t)
	cfgPath = filepath.Join(home, "cfg", "config.toml")
	t.Setenv(config.EnvConfig, cfgPath)
	return cfgPath, filepath.Join(home, "data")
}

// isolateHome gives the test its own home directory with no overrides, so
// that the default ~/.vario resolution applies.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{config.EnvConfig, config.EnvHome, config.EnvPassword} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return home
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

func answers(a ...string) string { return strings.Join(a, "\n") + "\n" }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// ---- fake ClickHouse ---------------------------------------------------------

type fakeCH struct {
	mu        sync.Mutex
	password  string
	databases map[string]bool
	denied    map[string]bool // CHECK GRANT statements that return 0
	queries   []string
}

func newFakeCH(t *testing.T, password string, dbs ...string) (*fakeCH, string) {
	f := &fakeCH{password: password, databases: map[string]bool{}, denied: map[string]bool{}}
	for _, d := range dbs {
		f.databases[d] = true
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/ping" {
		io.WriteString(w, "Ok.\n")
		return
	}
	if r.Header.Get("X-ClickHouse-Key") != f.password {
		w.Header().Set("X-ClickHouse-Exception-Code", "516")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "Code: 516. DB::Exception: Authentication failed. (AUTHENTICATION_FAILED)")
		return
	}
	b, _ := io.ReadAll(r.Body)
	q := string(b)
	f.queries = append(f.queries, q)
	data := []map[string]any{}
	switch {
	case strings.Contains(q, "version()"):
		data = append(data, map[string]any{"v": "25.8.0.1"})
	case strings.Contains(q, "system.databases"):
		n := 0
		for d := range f.databases {
			if strings.Contains(q, "'"+d+"'") {
				n = 1
			}
		}
		data = append(data, map[string]any{"n": n})
	case strings.HasPrefix(q, "CHECK GRANT"):
		res := 1
		if f.denied[q] {
			res = 0
		}
		data = append(data, map[string]any{"result": res})
	case strings.HasPrefix(q, "CREATE"), strings.HasPrefix(q, "DROP"):
		w.Header().Set("X-ClickHouse-Exception-Code", "497")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "Code: 497. DB::Exception: not allowed in the fake. (ACCESS_DENIED)")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakeCH) ddl() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.queries {
		if strings.HasPrefix(q, "CREATE") || strings.HasPrefix(q, "DROP") || strings.HasPrefix(q, "ALTER") || strings.HasPrefix(q, "TRUNCATE") {
			out = append(out, q)
		}
	}
	return out
}

// ---- fake service manager ------------------------------------------------------

type fakeService struct {
	installed, running bool
	args               []string
	restarts           int
}

func (f *fakeService) Install(s service.Spec) error {
	f.installed, f.running, f.args = true, true, s.Args
	return nil
}
func (f *fakeService) Uninstall() error { f.installed, f.running = false, false; return nil }
func (f *fakeService) Status() (service.Status, error) {
	return service.Status{Installed: f.installed, Running: f.running}, nil
}
func (f *fakeService) Restart() error {
	if !f.installed {
		return service.ErrNotLoaded
	}
	f.restarts++
	return nil
}
func (f *fakeService) Args() ([]string, error) {
	if !f.installed {
		return nil, service.ErrNotInstalled
	}
	return f.args, nil
}
func (f *fakeService) Definition() string { return "/fake/io.github.nedanwr.vario.plist" }

func useFakeService(t *testing.T, f *fakeService) {
	t.Helper()
	prev := serviceManager
	serviceManager = func() (service.Manager, error) { return f, nil }
	t.Cleanup(func() { serviceManager = prev })
}

// ---- tests ----------------------------------------------------------------------

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
	if cfg.ClickHouse.CreateDatabase {
		t.Fatal("init must default to using an existing database")
	}
	if sc, ok := cfg.Source("t3code"); !ok || sc.Path != fix.Path || !sc.Enabled {
		t.Fatalf("source: %+v", cfg.Sources)
	}
	before := readFile(t, cfgPath)
	r := vario(t, "", "init", "--non-interactive", "--clickhouse-url", unreachable)
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--update") || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("init must not overwrite silently: %+v", r)
	}
	if readFile(t, cfgPath) != before {
		t.Fatal("refused init must not touch the file")
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
	if r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--skip-connection-check", "--clickhouse-url", "https://u:p@ch.example.com"); r.code != ExitUsage || !strings.Contains(r.stderr, "credentials") {
		t.Fatalf("credentials in URL must be refused: %+v", r)
	}
	if r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--skip-connection-check", "--clickhouse-url", unreachable, "--database", "x;DROP"); r.code != ExitUsage || !strings.Contains(r.stderr, "plain identifier") {
		t.Fatalf("bad database name: %+v", r)
	}
}

func TestInitDefaultLocationAndPermissions(t *testing.T) {
	home := isolateHome(t)
	r := vario(t, "", "init", "--non-interactive", "--no-t3code", "--clickhouse-url", unreachable)
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	// ClickHouse is unreachable: the configuration is saved anyway and flagged as unverified.
	if !strings.Contains(r.stdout, "network failure") || !strings.Contains(r.stdout, "not verified") {
		t.Fatalf("unreachable ClickHouse should be reported as an unverified network failure: %s", r.stdout)
	}
	want := filepath.Join(home, ".vario", "config.toml")
	if p := vario(t, "", "config", "path"); strings.TrimSpace(p.stdout) != want {
		t.Fatalf("config path = %q, want %q", p.stdout, want)
	}
	cfg, err := config.Load(want)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.DataDir != filepath.Join(home, ".vario", "data") {
		t.Fatalf("data dir = %s", cfg.Collector.DataDir)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, filepath.Dir(want)); m != 0o700 {
			t.Fatalf("~/.vario mode %o", m)
		}
		if m := mode(t, want); m != 0o600 {
			t.Fatalf("config mode %o", m)
		}
	}
	// --config overrides the default; $VARIO_HOME relocates it.
	if p := vario(t, "", "--config", "/tmp/x/other.toml", "config", "path"); strings.TrimSpace(p.stdout) != "/tmp/x/other.toml" {
		t.Fatalf("--config: %+v", p)
	}
	alt := filepath.Join(home, "alt")
	t.Setenv(config.EnvHome, alt)
	if p := vario(t, "", "config", "path"); strings.TrimSpace(p.stdout) != filepath.Join(alt, "config.toml") {
		t.Fatalf("$VARIO_HOME: %+v", p)
	}
}

func TestInitInteractiveFresh(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	r := vario(t, answers(
		unreachable, // endpoint
		"collector", // username
		"pa ss",     // password
		"analytics", // database
		"",          // database management: use an existing database (default)
		"n",         // do not test the connection
		"2",         // messages mode
		"yes",       // confirm sensitive content
		"",          // enable T3 Code (detected)
		"",          // keep detected path
		"",          // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("interactive init: %+v", r)
	}
	for _, want := range []string{"does not send telemetry", "❯ 1) Use an existing database", "[x] T3 Code — detected", "Summary", "✓ Configuration saved", "vario backfill --source t3code", "vario service install"} {
		if !strings.Contains(r.stdout, want) && !strings.Contains(r.stdout, strings.Replace(want, "does not send", "sends no", 1)) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(r.stdout, "pa ss") {
		t.Fatal("the password must never be printed")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClickHouse.Database != "analytics" || cfg.ClickHouse.Username != "collector" || cfg.ClickHouse.CreateDatabase ||
		cfg.Privacy.Mode != config.ModeMessages || !cfg.Privacy.AllowSensitiveContent {
		t.Fatalf("config: %+v %+v", cfg.ClickHouse, cfg.Privacy)
	}
	if sc, ok := cfg.Source(config.SourceT3Code); !ok || !sc.Enabled || sc.Path != fix.Path {
		t.Fatalf("source: %+v", cfg.Sources)
	}
	if pw, _ := cfg.Password(); pw != "pa ss" {
		t.Fatalf("password = %q", pw)
	}
	if strings.Contains(readFile(t, cfgPath), "pa ss") {
		t.Fatal("password must not be written into the config file")
	}
	if runtime.GOOS != "windows" && mode(t, cfg.PasswordPath()) != 0o600 {
		t.Fatal("password file must be 0600")
	}
}

func TestInitSensitiveModeNotConfirmed(t *testing.T) {
	cfgPath, _ := isolate(t)
	r := vario(t, answers(unreachable, "default", "", "vario", "", "n", "3", "no", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Privacy.Mode != config.ModeMetrics || cfg.Privacy.AllowSensitiveContent {
		t.Fatalf("unconfirmed full mode must fall back to metrics: %+v", cfg.Privacy)
	}
}

func TestInitInterruptedSavesNothing(t *testing.T) {
	cfgPath, _ := isolate(t)
	// Input ends in the middle of the wizard.
	r := vario(t, answers(unreachable, "default"), "init")
	if r.code == 0 || !strings.Contains(r.stderr, "nothing was saved") {
		t.Fatalf("interrupted init: %+v", r)
	}
	if fileExists(cfgPath) {
		t.Fatal("an interrupted init must not create a configuration")
	}
	// The same applies to an interrupted reinitialization.
	initFixture(t)
	cfgPath, _ = os.Getenv(config.EnvConfig), ""
	before := readFile(t, cfgPath)
	r = vario(t, answers("1", "https://changed.example.com"), "init")
	if r.code == 0 || readFile(t, cfgPath) != before {
		t.Fatalf("interrupted reinit must leave the file untouched: %+v", r)
	}
	// Quitting at the summary saves nothing either.
	r = vario(t, answers("1", "https://changed.example.com", "", "", "", "", "n", "", "", "", "5"), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing was saved") || readFile(t, cfgPath) != before {
		t.Fatalf("quit at summary: %+v", r)
	}
}

func TestReinitPreservesSettings(t *testing.T) {
	fix, cfgPath := initFixture(t)
	if r := vario(t, "secret-1\n", "config", "set-password", "--stdin"); r.code != 0 {
		t.Fatalf("set-password: %+v", r)
	}
	// Hand edits: a tuned value with a comment and a note of our own.
	text := strings.Replace(readFile(t, cfgPath), "batch_size = 500", "batch_size = 777  # tuned", 1) + "# my note\n"
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	pwBefore := readFile(t, filepath.Join(filepath.Dir(cfgPath), "clickhouse-password"))

	// Review without changing anything: the file stays byte-identical.
	keep := answers("1", "", "", "1", "", "", "n", "", "", "", "")
	r := vario(t, keep, "init")
	if r.code != 0 || !strings.Contains(r.stdout, "already configured") || !strings.Contains(r.stdout, "No configuration changes") {
		t.Fatalf("reinit keep: %+v", r)
	}
	if readFile(t, cfgPath) != text {
		t.Fatal("reinit without changes must not rewrite the file")
	}

	// Change only the database: one line changes, everything else is preserved.
	r = vario(t, answers("1", "", "", "1", "telemetry", "2", "n", "", "", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("reinit change: %+v", r)
	}
	after := readFile(t, cfgPath)
	for _, want := range []string{`database = "telemetry"`, "create_database = true", "batch_size = 777  # tuned", "# my note", fix.Path} {
		if !strings.Contains(after, want) {
			t.Errorf("after reinit, file is missing %q:\n%s", want, after)
		}
	}
	if got := readFile(t, filepath.Join(filepath.Dir(cfgPath), "clickhouse-password")); got != pwBefore {
		t.Fatal("reinit must not change the stored password unless asked")
	}

	// "Keep the current configuration" exits without changes.
	r = vario(t, answers("2"), "init")
	if r.code != 0 || readFile(t, cfgPath) != after {
		t.Fatalf("keep and exit: %+v", r)
	}
}

func TestReinitNonInteractive(t *testing.T) {
	_, cfgPath := initFixture(t, "--username", "alice")
	text := readFile(t, cfgPath) + "# keep me\n"
	os.WriteFile(cfgPath, []byte(text), 0o600)
	r := vario(t, "", "init", "--non-interactive", "--update", "--database", "other", "--create-database", "--skip-connection-check")
	if r.code != 0 {
		t.Fatalf("--update: %+v", r)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.ClickHouse.Database != "other" || !cfg.ClickHouse.CreateDatabase || cfg.ClickHouse.Username != "alice" || cfg.ClickHouse.URL != unreachable {
		t.Fatalf("--update must change only the given settings: %+v", cfg.ClickHouse)
	}
	if !strings.Contains(readFile(t, cfgPath), "# keep me") {
		t.Fatal("--update must preserve comments")
	}
	r = vario(t, "", "init", "--non-interactive", "--force", "--no-t3code", "--clickhouse-url", unreachable, "--skip-connection-check")
	if r.code != 0 || !strings.Contains(r.stdout, "backed up") {
		t.Fatalf("--force: %+v", r)
	}
	backups, _ := filepath.Glob(cfgPath + ".bak-*")
	if len(backups) != 1 || !strings.Contains(readFile(t, backups[0]), "# keep me") {
		t.Fatalf("--force must keep a backup: %v", backups)
	}
	cfg, _ = config.Load(cfgPath)
	if cfg.ClickHouse.Username != "default" {
		t.Fatalf("--force starts from defaults: %+v", cfg.ClickHouse)
	}
	// Starting over interactively offers the stored password instead of hiding it.
	vario(t, "pw\n", "config", "set-password", "--stdin")
	r = vario(t, answers("3", unreachable, "", "1", "", "", "n", "", "", ""), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "A ClickHouse password is already stored") || !strings.Contains(r.stdout, "(unchanged)") {
		t.Fatalf("start over with a stored password: %+v", r)
	}
}

func TestInitMalformedExistingConfig(t *testing.T) {
	cfgPath, _ := isolate(t)
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	bad := "[clickhouse]\nurl = \"https://x\"\npasword = \"typo\"\n"
	os.WriteFile(cfgPath, []byte(bad), 0o600)
	r := vario(t, "", "init", "--non-interactive", "--update", "--skip-connection-check")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "pasword") || !strings.Contains(r.stderr, "config edit") {
		t.Fatalf("malformed config: %+v", r)
	}
	r = vario(t, answers("1"), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "cannot be read") || readFile(t, cfgPath) != bad {
		t.Fatalf("interactive init must offer to exit without touching a malformed file: %+v", r)
	}
	if r := vario(t, "", "doctor"); r.code != ExitFailure || !strings.Contains(r.stdout, "unknown settings") {
		t.Fatalf("doctor on malformed config: %+v", r)
	}
}

func TestInitValidationFailures(t *testing.T) {
	cfgPath, _ := isolate(t)

	// Offline: retry, then save without verification. Entered values are kept.
	r := vario(t, answers(unreachable, "bob", "", "mydb", "", "y", "1", "3", "", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("offline init: %+v", r)
	}
	if strings.Count(r.stdout, "✗ network failure") != 2 || !strings.Contains(r.stdout, "not verified (network failure)") {
		t.Fatalf("expected a classified network failure twice (retry): %s", r.stdout)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.ClickHouse.Username != "bob" || cfg.ClickHouse.Database != "mydb" {
		t.Fatalf("values must survive a failed check: %+v", cfg.ClickHouse)
	}

	// Wrong password: authentication failure, then change the settings and succeed.
	ch, url := newFakeCH(t, "right", "mydb")
	r = vario(t, answers(
		"1",                      // review the existing configuration
		url, "", "wrong", "", "", // endpoint, user, password, database, management
		"y", "2", // test: authentication fails; change the settings
		"", "", "2", "right", "", "", // keep endpoint and user, enter a different password
		"y",             // test again: succeeds
		"", "", "", "n", // mode, sources, save, decline creating tables now
	), "init")
	if r.code != 0 {
		t.Fatalf("auth init: %+v", r)
	}
	if !strings.Contains(r.stdout, "authentication failure") || !strings.Contains(r.stdout, "✓ authenticated as bob") {
		t.Fatalf("expected an authentication failure, then success: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, "verified (ClickHouse 25.8.0.1)") {
		t.Fatalf("summary should show the verified connection: %s", r.stdout)
	}
	if d := ch.ddl(); len(d) != 0 {
		t.Fatalf("validation must not create anything: %v", d)
	}
}

func TestInitMissingDatabase(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := newFakeCH(t, "", "other_db")
	// Missing database: allow creation, re-check, decline provisioning.
	r := vario(t, answers(url, "", "", "vario", "", "y", "1", "", "", "", "", "n"), "init")
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	if !strings.Contains(r.stdout, "✗ database vario does not exist") || !strings.Contains(r.stdout, "will create it on first delivery") {
		t.Fatalf("missing database should be reported, then accepted for creation: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, "Create database vario and Vario's tables now?") {
		t.Fatalf("provisioning should be offered (and declined): %s", r.stdout)
	}
	cfg, _ := config.Load(cfgPath)
	if !cfg.ClickHouse.CreateDatabase {
		t.Fatal("create_database should be recorded")
	}
	if d := ch.ddl(); len(d) != 0 {
		t.Fatalf("declining provisioning must not create anything: %v", d)
	}

	// Missing privilege: reported as a permission problem.
	cfgPath2 := filepath.Join(t.TempDir(), "c.toml")
	t.Setenv(config.EnvConfig, cfgPath2)
	ch.denied["CHECK GRANT INSERT ON `other_db`.*"] = true
	r = vario(t, answers(url, "", "", "other_db", "", "y", "", "", "", ""), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "✗ missing privilege: INSERT ON other_db.*") {
		t.Fatalf("missing privilege: %+v", r)
	}
}

func TestConfigCommands(t *testing.T) {
	_, cfgPath := initFixture(t)
	text := strings.Replace(readFile(t, cfgPath), "[clickhouse]", "# ClickHouse settings\n[clickhouse]", 1)
	os.WriteFile(cfgPath, []byte(text), 0o600)

	if r := vario(t, "", "config", "path"); r.code != 0 || strings.TrimSpace(r.stdout) != cfgPath {
		t.Fatalf("path: %+v", r)
	}
	if r := vario(t, "", "config", "get", "clickhouse.url"); r.code != 0 || strings.TrimSpace(r.stdout) != unreachable {
		t.Fatalf("get: %+v", r)
	}
	r := vario(t, "", "config", "set", "clickhouse.url", "https://localhost:8443")
	if r.code != 0 || !strings.Contains(r.stdout, "Set clickhouse.url = https://localhost:8443") {
		t.Fatalf("set: %+v", r)
	}
	for _, kv := range [][2]string{{"clickhouse.username", "vario"}, {"clickhouse.database", "vario_prod"}, {"clickhouse.create_database", "true"}, {"collector.poll_interval", "10s"}} {
		if r := vario(t, "", "config", "set", kv[0], kv[1]); r.code != 0 {
			t.Fatalf("set %s: %+v", kv[0], r)
		}
		if r := vario(t, "", "config", "get", kv[0]); strings.TrimSpace(r.stdout) != kv[1] {
			t.Fatalf("get %s after set: %+v", kv[0], r)
		}
	}
	after := readFile(t, cfgPath)
	if !strings.Contains(after, "# ClickHouse settings") || !strings.Contains(after, "# Vario configuration") {
		t.Fatal("set must preserve comments")
	}
	if runtime.GOOS != "windows" && mode(t, cfgPath) != 0o600 {
		t.Fatal("set must keep mode 0600")
	}

	// Rejections leave the file untouched.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "clickhouse.pasword", "x"}, "unknown configuration key"},
		{[]string{"get", "nope"}, "Supported keys"},
		{[]string{"set", "clickhouse.password", "hunter2"}, "set-password"},
		{[]string{"set", "clickhouse.create_database", "maybe"}, "expected true or false"},
		{[]string{"set", "clickhouse.database", "bad-name"}, "plain identifier"},
		{[]string{"set", "clickhouse.url", "ftp://x"}, "http or https"},
		{[]string{"set", "clickhouse.url", "http://ch.example.com:8123"}, "unencrypted"},
		{[]string{"set", "privacy.mode", "full"}, "allow_sensitive_content"},
		{[]string{"set", "collector.batch_size", "0"}, "batch_size"},
		{[]string{"set", "privacy.extra_redaction_patterns", "x"}, "config edit"},
	} {
		r := vario(t, "", append([]string{"config"}, tc.args...)...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("config %v: want exit %d with %q, got %+v", tc.args, ExitUsage, tc.want, r)
		}
	}
	if readFile(t, cfgPath) != after {
		t.Fatal("rejected changes must not modify the file")
	}

	// Sensitive mode needs the explicit opt-in first.
	if r := vario(t, "", "config", "set", "privacy.allow_sensitive_content", "true"); r.code != 0 {
		t.Fatalf("opt-in: %+v", r)
	}
	if r := vario(t, "", "config", "set", "privacy.mode", "full"); r.code != 0 {
		t.Fatalf("full after opt-in: %+v", r)
	}

	r = vario(t, "", "config", "show")
	if r.code != 0 || !strings.Contains(r.stdout, `url = "https://localhost:8443"`) || !strings.Contains(r.stdout, "# default") ||
		!strings.Contains(r.stdout, "[sources.t3code]") || !strings.Contains(r.stdout, "# password: not set") {
		t.Fatalf("show: %+v", r)
	}
}

func TestConfigPasswordIsNeverShown(t *testing.T) {
	_, cfgPath := initFixture(t)
	if r := vario(t, "s3cr3t-value\n", "config", "set-password", "--stdin"); r.code != 0 {
		t.Fatalf("set-password: %+v", r)
	}
	pwFile := filepath.Join(filepath.Dir(cfgPath), "clickhouse-password")
	if readFile(t, pwFile) != "s3cr3t-value" {
		t.Fatal("password not stored")
	}
	if runtime.GOOS != "windows" && mode(t, pwFile) != 0o600 {
		t.Fatal("password file must be 0600")
	}
	for _, args := range [][]string{{"config", "show"}, {"config", "get", "clickhouse.password"}, {"status"}} {
		r := vario(t, "", args...)
		if strings.Contains(r.stdout+r.stderr, "s3cr3t-value") {
			t.Fatalf("%v revealed the password", args)
		}
		if len(args) > 1 && args[1] == "get" && !strings.Contains(r.stdout, "stored in") {
			t.Fatalf("get clickhouse.password should report the status: %+v", r)
		}
	}
	if strings.Contains(readFile(t, cfgPath), "s3cr3t-value") {
		t.Fatal("password must not reach the config file")
	}
	if r := vario(t, "", "config", "set-password"); r.code != ExitUsage || !strings.Contains(r.stderr, "--stdin") {
		t.Fatalf("set-password without a terminal: %+v", r)
	}
	if r := vario(t, "", "config", "set-password", "--clear"); r.code != 0 || fileExists(pwFile) {
		t.Fatalf("--clear: %+v", r)
	}
}

func writeEditor(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("editor scripts need a POSIX shell")
	}
	p := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The value is split into words and run without a shell; a quoted path with a space works.
	dir := filepath.Join(t.TempDir(), "my editors")
	os.MkdirAll(dir, 0o755)
	q := filepath.Join(dir, "ed")
	os.Rename(p, q)
	t.Setenv("VISUAL", `"`+q+`" --flag`)
}

func TestConfigEdit(t *testing.T) {
	_, cfgPath := initFixture(t)
	// A valid edit is applied. $1 is --flag, $2 the file.
	writeEditor(t, `sed -i.orig 's/^batch_size = 500$/batch_size = 250/' "$2" && rm -f "$2.orig"`)
	r := vario(t, "", "config", "edit")
	if r.code != 0 || !strings.Contains(r.stdout, "Saved") {
		t.Fatalf("edit: %+v", r)
	}
	if !strings.Contains(readFile(t, cfgPath), "batch_size = 250") {
		t.Fatal("edit not applied")
	}
	before := readFile(t, cfgPath)

	// An invalid edit is rejected; the configuration is unchanged and the copy kept.
	writeEditor(t, `echo 'oops = [' >> "$2"`)
	r = vario(t, "", "config", "edit")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "invalid and was not saved") || !strings.Contains(r.stderr, "edited copy") {
		t.Fatalf("invalid edit: %+v", r)
	}
	if readFile(t, cfgPath) != before {
		t.Fatal("invalid edit must not change the configuration")
	}
	copies, _ := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), ".config-edit-*.toml"))
	if len(copies) != 1 {
		t.Fatalf("the edited copy should be kept: %v", copies)
	}
	os.Remove(copies[0])

	// Semantically invalid values are rejected too.
	writeEditor(t, `sed -i.orig 's/^mode = "metrics"$/mode = "full"/' "$2" && rm -f "$2.orig"`)
	if r := vario(t, "", "config", "edit"); r.code != ExitUsage || !strings.Contains(r.stderr, "allow_sensitive_content") {
		t.Fatalf("invalid value: %+v", r)
	}
	// No changes.
	writeEditor(t, `true`)
	if r := vario(t, "", "config", "edit"); r.code != 0 || !strings.Contains(r.stdout, "No changes") {
		t.Fatalf("no-op edit: %+v", r)
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string]string{
		`vim`:                    `vim`,
		`code --wait`:            `code|--wait`,
		`"/Apps/My Ed/ed" -n`:    `/Apps/My Ed/ed|-n`,
		`'a b' c\ d`:             `a b|c d`,
		`emacs; rm -rf /`:        `emacs;|rm|-rf|/`,
		`vi $(touch /tmp/pwned)`: `vi|$(touch|/tmp/pwned)`,
	} {
		got, err := splitWords(in)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("splitWords(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitWords(`"unterminated`); err == nil {
		t.Error("unterminated quote should fail")
	}
}

func TestServiceCompatibility(t *testing.T) {
	_, cfgPath := initFixture(t)
	svc := &fakeService{}
	useFakeService(t, svc)
	if r := vario(t, "", "service", "restart"); r.code != ExitUsage || !strings.Contains(r.stderr, "not installed") {
		t.Fatalf("restart when not installed: %+v", r)
	}
	svc.installed, svc.running, svc.args = true, true, []string{"run", "--config", cfgPath, "--log-file", "/x/vario.log"}

	// A changed endpoint needs only a restart, never a reinstall.
	r := vario(t, "", "config", "set", "clickhouse.url", "https://ch.example.com:8443")
	if r.code != 0 || !strings.Contains(r.stdout, "vario service restart") || strings.Contains(r.stdout, "service install") {
		t.Fatalf("set with running service: %+v", r)
	}
	if r := vario(t, "", "service", "restart"); r.code != 0 || svc.restarts != 1 {
		t.Fatalf("restart: %+v (restarts %d)", r, svc.restarts)
	}
	if r := vario(t, "", "service", "status"); !strings.Contains(r.stdout, "Config      "+cfgPath) {
		t.Fatalf("status should show the service's configuration: %+v", r)
	}
	// The restarted service reads the new value from the same file.
	if cfg, _ := config.Load(service.ConfigPath(svc.args)); cfg.ClickHouse.URL != "https://ch.example.com:8443" {
		t.Fatalf("service configuration: %+v", cfg.ClickHouse)
	}

	// A broken configuration does not stop a working service.
	os.WriteFile(cfgPath, []byte(readFile(t, cfgPath)+"\nbogus = 1\n"), 0o600)
	if r := vario(t, "", "service", "restart"); r.code != ExitUsage || svc.restarts != 1 {
		t.Fatalf("restart with invalid config: %+v", r)
	}

	// A service installed for another configuration file is pointed out.
	svc.args = []string{"run", "--config", "/elsewhere/config.toml"}
	os.WriteFile(cfgPath, []byte(strings.Replace(readFile(t, cfgPath), "\nbogus = 1\n", "", 1)), 0o600)
	if r := vario(t, "", "config", "set", "clickhouse.database", "x"); !strings.Contains(r.stdout, "reads /elsewhere/config.toml") {
		t.Fatalf("mismatched service config: %+v", r)
	}
	if r := vario(t, "", "doctor"); !strings.Contains(r.stdout, "background service reads /elsewhere/config.toml") {
		t.Fatalf("doctor should flag the service configuration: %s", r.stdout)
	}
}

func TestServiceInstallUsesResolvedConfigWithoutSecrets(t *testing.T) {
	home := isolateHome(t)
	fix := t3codetest.New(t)
	if r := vario(t, "pw-in-stdin\n", "init", "--non-interactive", "--clickhouse-url", unreachable, "--skip-connection-check", "--t3code-path", fix.Path, "--password-stdin"); r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	svc := &fakeService{}
	useFakeService(t, svc)
	if runtime.GOOS != "darwin" {
		t.Skip("service paths are only implemented for macOS")
	}
	exe := filepath.Join(home, "bin", "vario")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, nil, 0o755)
	prev := executable
	executable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executable = prev })
	if r := vario(t, "", "service", "install"); r.code != 0 {
		t.Fatalf("install: %+v", r)
	}
	if got := service.ConfigPath(svc.args); got != filepath.Join(home, ".vario", "config.toml") {
		t.Fatalf("service config = %s", got)
	}
	if strings.Contains(strings.Join(svc.args, " "), "pw-in-stdin") {
		t.Fatal("service arguments must not contain credentials")
	}
}

// runProcess runs vario in a separate process (the test binary in helper mode).
func runProcess(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "VARIO_TEST_HELPER=1", "VARIO_TEST_ARGS="+strings.Join(args, "\x1f"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{code, out.String(), errb.String()}
}

func TestConfigPersistsAcrossProcesses(t *testing.T) {
	home := isolateHome(t)
	if r := runProcess(t, "init", "--non-interactive", "--no-t3code", "--clickhouse-url", unreachable, "--skip-connection-check"); r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	if r := runProcess(t, "config", "set", "clickhouse.database", "persisted"); r.code != 0 {
		t.Fatalf("set: %+v", r)
	}
	r := runProcess(t, "config", "get", "clickhouse.database")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "persisted" {
		t.Fatalf("get in a new process: %+v", r)
	}
	if !fileExists(filepath.Join(home, ".vario", "config.toml")) {
		t.Fatal("default location not used")
	}
}

func TestSourcesAddAndList(t *testing.T) {
	_, data := isolate(t)
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
	// Reinitializing does not reset the spool or the cursor.
	if r := vario(t, "", "init", "--non-interactive", "--update", "--database", "elsewhere", "--skip-connection-check"); r.code != 0 {
		t.Fatalf("reinit: %+v", r)
	}
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "cursor         seq") || !strings.Contains(r.stdout, "messages=2") {
		t.Fatalf("reinit must keep cursor and spool: %+v", r)
	}
}

func TestDoctor(t *testing.T) {
	initFixture(t)
	r := vario(t, "", "doctor")
	if r.code != ExitFailure || !strings.Contains(r.stdout, "✓ t3code") || !strings.Contains(r.stdout, "✗ endpoint unreachable (network)") {
		t.Fatalf("doctor: %+v", r)
	}
	if !strings.Contains(r.stdout, "existing database only (create_database = false)") {
		t.Fatalf("doctor should report the database policy: %s", r.stdout)
	}
}

func TestDoctorClassifiesFailures(t *testing.T) {
	initFixture(t)
	ch, url := newFakeCH(t, "pw", "vario")
	vario(t, "", "config", "set", "clickhouse.url", url)
	r := vario(t, "", "doctor")
	if !strings.Contains(r.stdout, "authentication or query failed (authentication)") {
		t.Fatalf("auth failure: %s", r.stdout)
	}
	vario(t, "pw\n", "config", "set-password", "--stdin")
	vario(t, "", "config", "set", "clickhouse.database", "missing_db")
	r = vario(t, "", "doctor")
	if r.code != ExitFailure || !strings.Contains(r.stdout, "✗ database missing_db does not exist") {
		t.Fatalf("missing database: %s", r.stdout)
	}
	// --create-schema respects create_database = false and creates nothing.
	r = vario(t, "", "doctor", "--create-schema")
	if !strings.Contains(r.stdout, "schema creation failed (database missing)") {
		t.Fatalf("create-schema with a missing database: %s", r.stdout)
	}
	for _, q := range ch.ddl() {
		if strings.HasPrefix(q, "CREATE DATABASE") {
			t.Fatalf("CREATE DATABASE must not run when create_database = false: %v", ch.ddl())
		}
	}
	// With creation enabled, a permission failure is reported as such.
	vario(t, "", "config", "set", "clickhouse.create_database", "true")
	r = vario(t, "", "doctor", "--create-schema")
	if !strings.Contains(r.stdout, "schema creation failed (permission)") || !strings.Contains(r.stdout, "CREATE DATABASE privilege") {
		t.Fatalf("create-schema without privilege: %s", r.stdout)
	}
}
