package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// provisionSchema creates Vario's schema in db on the fake, as an administrator would.
func provisionSchema(t *testing.T, url, user, pw, db string) {
	t.Helper()
	cfg := config.Default().ClickHouse
	cfg.URL, cfg.Database = url, db
	a, err := clickhouse.NewAdmin(cfg, user, pw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.PlanSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res := a.Apply(context.Background(), p, nil); !res.OK() {
		t.Fatal(res.Err)
	}
}

func noSecrets(t *testing.T, r result, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
			t.Fatalf("a secret was printed: %q", s)
		}
	}
}

func TestInitManualFresh(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	r := vario(t, answers(
		unreachable, // endpoint
		"2",         // manually managed setup
		"analytics", // database
		"collector", // operational user
		"pa ss",     // password
		"n",         // do not verify the connection
		"2",         // messages mode
		"yes",       // confirm sensitive content
		"",          // enable T3 Code (detected)
		"",          // keep detected path
		"",          // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("interactive init: %+v", r)
	}
	for _, want := range []string{"sends no telemetry", "❯ 1) Guided setup", "Manually managed", "[x] T3 Code — detected", "Summary",
		"manually managed (no administrative credentials)", "✓ Configuration saved", "vario migrate", "vario backfill --source t3code", "vario service install"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q", want)
		}
	}
	noSecrets(t, r, "pa ss")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClickHouse.Database != "analytics" || cfg.ClickHouse.Username != "collector" || cfg.ClickHouse.CreateDatabase ||
		cfg.Privacy.Mode != config.ModeMessages || !cfg.Privacy.AllowSensitiveContent {
		t.Fatalf("config: %+v %+v", cfg.ClickHouse, cfg.Privacy)
	}
	if strings.Contains(readFile(t, cfgPath), "create_database") {
		t.Fatal("new configurations must not contain the deprecated create_database setting")
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
	r := vario(t, answers(unreachable, "2", "vario", "default", "", "n", "3", "no", "", ""), "init")
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
	r := vario(t, answers(unreachable, "2"), "init")
	if r.code == 0 || !strings.Contains(r.stderr, "nothing was saved") {
		t.Fatalf("interrupted init: %+v", r)
	}
	if fileExists(cfgPath) {
		t.Fatal("an interrupted init must not create a configuration")
	}
	// The same applies to an interrupted reinitialization.
	initFixture(t)
	cfgPath = os.Getenv(config.EnvConfig)
	before := readFile(t, cfgPath)
	r = vario(t, answers("1", "https://changed.example.com"), "init")
	if r.code == 0 || readFile(t, cfgPath) != before {
		t.Fatalf("interrupted reinit must leave the file untouched: %+v", r)
	}
	// Quitting at the summary saves nothing either.
	r = vario(t, answers("1", "https://changed.example.com", "2", "", "", "", "n", "", "", "", "5"), "init")
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
	keep := answers("1", "", "2", "", "", "", "n", "", "", "", "")
	r := vario(t, keep, "init")
	if r.code != 0 || !strings.Contains(r.stdout, "already configured") || !strings.Contains(r.stdout, "No configuration changes") {
		t.Fatalf("reinit keep: %+v", r)
	}
	if readFile(t, cfgPath) != text {
		t.Fatal("reinit without changes must not rewrite the file")
	}

	// Change only the database: one line changes, everything else is preserved.
	r = vario(t, answers("1", "", "2", "telemetry", "", "", "n", "", "", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("reinit change: %+v", r)
	}
	after := readFile(t, cfgPath)
	for _, want := range []string{`database = "telemetry"`, "batch_size = 777  # tuned", "# my note", fix.Path} {
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
	if r.code != 0 || !strings.Contains(r.stderr, "--create-database is deprecated and ignored") {
		t.Fatalf("--update: %+v", r)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.ClickHouse.Database != "other" || cfg.ClickHouse.CreateDatabase || cfg.ClickHouse.Username != "alice" || cfg.ClickHouse.URL != unreachable {
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
	r = vario(t, answers("3", unreachable, "2", "", "", "1", "n", "", "", ""), "init")
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

func TestInitManualValidationFailures(t *testing.T) {
	cfgPath, _ := isolate(t)

	// Offline: check again, then save without verification. Entered values are kept.
	r := vario(t, answers(unreachable, "2", "mydb", "bob", "", "y", "1", "5", "", "", ""), "init")
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
	ch, url := newFakeCH(t, "admin-pw", "mydb")
	ch.addUser("bob", "right", "local_directory", false)
	provisionSchema(t, url, "default", "admin-pw", "mydb")
	ch.grant("bob", "SELECT mydb.*", "INSERT mydb.*")
	before := len(ch.changes())
	r = vario(t, answers(
		"1",                       // review the existing configuration
		url, "2", "", "", "wrong", // endpoint, manual, database, user, password
		"y", "3", // verify: authentication fails; change the settings
		"", "", "2", "right", // keep database and user, enter a different password
		"y",        // verify again: succeeds
		"", "", "", // mode, sources, save
	), "init")
	if r.code != 0 {
		t.Fatalf("auth init: %+v", r)
	}
	if !strings.Contains(r.stdout, "authentication failure") || !strings.Contains(r.stdout, "✓ authenticated as bob") || !strings.Contains(r.stdout, "inserts accepted") {
		t.Fatalf("expected an authentication failure, then success: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, "verified as bob (ClickHouse 25.8.0.1)") {
		t.Fatalf("summary should show the verified connection: %s", r.stdout)
	}
	if len(ch.changes()) != before {
		t.Fatalf("manual setup must not change ClickHouse: %v", ch.changes()[before:])
	}
	noSecrets(t, r, "right", "wrong")
}

func TestInitManualMissingDatabaseShowsSQL(t *testing.T) {
	isolate(t)
	ch, url := newFakeCH(t, "", "other_db")
	r := vario(t, answers(url, "2", "vario", "collector", "", "y", "2", "5", "", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	for _, want := range []string{"✗ database vario does not exist", "CREATE DATABASE IF NOT EXISTS `vario`", "CREATE USER `collector`",
		"GRANT SELECT, INSERT ON `vario`.`messages` TO `collector`", "vario migrate --dry-run"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if regexp.MustCompile(`GRANT [^\n]*ON \*\.\*`).MatchString(r.stdout) {
		t.Fatal("the suggested SQL must not grant global privileges")
	}
	if c := ch.changes(); len(c) != 0 {
		t.Fatalf("validation must not create anything: %v", c)
	}
}

// guidedFake returns a fake server with an administrative user.
func guidedFake(t *testing.T, dbs ...string) (*fakeCH, string) {
	ch, url := newFakeCH(t, "unused-catch-all", dbs...)
	ch.addUser("default", "adminpw", "users_xml", true)
	return ch, url
}

func TestInitGuidedCreatesDatabaseAndUser(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	ch, url := guidedFake(t, "analytics")
	r := vario(t, answers(
		url,        // endpoint
		"",         // guided setup (default)
		"",         // administrative user: default
		"adminpw",  // administrative password
		"2",        // create a new database
		"",         // name: vario
		"",         // create a dedicated user (default)
		"",         // name: vario
		"",         // password: generate
		"",         // do not show it
		"",         // create the schema now (default yes)
		"",         // apply the plan
		"", "", "", // mode, enable T3 Code, path
		"", // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("guided init: %+v", r)
	}
	for _, want := range []string{"Administrative credentials are used only for this command and are never saved", "✓ authenticated as default",
		"will be created after you review all changes", "Planned changes in ClickHouse", "  Database\n", "  Vario schema\n", "  User\n", "  Privileges\n",
		"Nothing has been changed yet", "✓ create database vario", "✓ create user vario", "Verifying the operational login as vario",
		"inserts accepted", "created by this setup", "dedicated user created by this setup", "generated, saved to", "(credentials not saved)",
		"✓ Administrative credentials were used only during setup and were not saved"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(r.stdout, "vario migrate ") {
		t.Errorf("the schema is complete; migrate should not be suggested:\n%s", r.stdout)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClickHouse.Database != "vario" || cfg.ClickHouse.Username != "vario" {
		t.Fatalf("config: %+v", cfg.ClickHouse)
	}
	pw, _ := cfg.Password()
	if len(pw) != 32 {
		t.Fatalf("a 32-character password should be generated and stored, got %d characters", len(pw))
	}
	noSecrets(t, r, pw, "adminpw")
	for _, f := range []string{cfgPath, cfg.PasswordPath()} {
		if strings.Contains(readFile(t, f), "adminpw") {
			t.Fatalf("the administrative password must never be stored (%s)", f)
		}
	}
	var created, grants int
	for _, q := range ch.changes() {
		if strings.Contains(q, pw) {
			t.Fatal("the password must never be sent in a statement (only its salted hash)")
		}
		if strings.HasPrefix(q, "CREATE USER `vario` IDENTIFIED WITH sha256_hash") {
			created++
		}
		if strings.HasPrefix(q, "GRANT") {
			grants++
			if strings.Contains(q, "*") || strings.Contains(q, "GRANT OPTION") || !strings.HasSuffix(q, "TO `vario`") || !strings.Contains(q, "ON `vario`.") {
				t.Fatalf("grant not scoped to Vario's objects: %s", q)
			}
		}
	}
	if created != 1 || grants != 20 {
		t.Fatalf("expected one user and 20 object grants: %d %d\n%v", created, grants, ch.changes())
	}
	if u := ch.user("vario"); u == nil || u.admin {
		t.Fatal("the operational user must not be an administrator")
	}

	// Re-running guided setup reuses everything and changes nothing.
	n := len(ch.changes())
	r = vario(t, answers(
		"1", "", "", // review, keep endpoint, guided
		"", "adminpw", // administrative credentials
		"1", "", // existing database, the configured one (default)
		"", "", // existing user (default for a configured user), the configured one (default)
		"",         // keep the stored password
		"", "", "", // mode, enable T3 Code, path
		"", // save
	), "init")
	if r.code != 0 {
		t.Fatalf("guided reinit: %+v", r)
	}
	if len(ch.changes()) != n {
		t.Fatalf("a repeated guided setup must not change ClickHouse: %v", ch.changes()[n:])
	}
	for _, want := range []string{"vario  (currently configured)", "appears to have the privileges", "✓ up to date; nothing to create", "No configuration changes"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("reinit output missing %q", want)
		}
	}
	if got, _ := cfg.Password(); got != pw {
		t.Fatal("the stored password must be kept")
	}
}

func TestInitGuidedShowsGeneratedPasswordOnce(t *testing.T) {
	cfgPath, _ := isolate(t)
	_, url := guidedFake(t)
	r := vario(t, answers(url, "", "", "adminpw", "2", "", "", "", "", "y", "", "", "", "", ""), "init")
	if r.code != 0 {
		t.Fatalf("guided init: %+v", r)
	}
	cfg, _ := config.Load(cfgPath)
	pw, _ := cfg.Password()
	if strings.Count(r.stdout, pw) != 1 || !strings.Contains(r.stdout, "It will not be shown again") {
		t.Fatalf("the generated password must be shown exactly once when asked:\n%s", r.stdout)
	}
}

func TestInitGuidedExistingUserWithMissingGrants(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t, "analytics", "metrics")
	ch.addUser("collector", "cpw", "local_directory", false)
	ch.addUser("xmluser", "xpw", "users_xml", false)
	r := vario(t, answers(
		url, "", "", "adminpw", // endpoint, guided, administrative credentials
		"1", "2", // existing database: metrics
		"2", "1", // existing user: collector
		"2",        // show the required SQL
		"1",        // authorize Vario to grant the missing privileges
		"cpw",      // collector's password
		"",         // create the schema
		"2",        // show the SQL of the plan
		"",         // apply
		"", "", "", // mode, T3 Code not detected: do not enable, save
	), "init")
	if r.code != 0 {
		t.Fatalf("guided init: %+v", r)
	}
	for _, want := range []string{"Databases visible to default", "Users visible to default", "xmluser  (defined in the server configuration files",
		"default  (defined in the server configuration files; privileges cannot be changed with SQL)  (the administrative account; not recommended)",
		"✗ collector is missing privileges", "Statements an administrator can run", "GRANT SELECT, INSERT ON `metrics`.`sources` TO `collector`",
		"Authorize Vario to grant", "✓ grant SELECT, INSERT on metrics.sources to collector", "inserts accepted", "existing user"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(r.stdout, "CREATE USER") {
		t.Fatal("an existing user must not be created or re-created")
	}
	for _, q := range ch.changes() {
		if strings.Contains(q, "collector") && !strings.HasPrefix(q, "GRANT") {
			t.Fatalf("an existing user may only receive grants, never other changes: %s", q)
		}
		if strings.Contains(q, "metrics") == false && !strings.HasPrefix(q, "INSERT") {
			t.Fatalf("statement outside the selected database: %s", q)
		}
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.ClickHouse.Username != "collector" || cfg.ClickHouse.Database != "metrics" {
		t.Fatalf("config: %+v", cfg.ClickHouse)
	}
	if pw, _ := cfg.Password(); pw != "cpw" {
		t.Fatal("the existing user's password must be stored for the collector")
	}
	noSecrets(t, r, "cpw", "adminpw")
}

func TestInitGuidedReadOnlyUserAndCancel(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t, "metrics")
	ch.addUser("xmluser", "xpw", "users_xml", false)
	r := vario(t, answers(
		url, "", "", "adminpw",
		"1", "1", // existing database: metrics
		"2", "2", // existing user: xmluser (users are sorted: default, xmluser)
		"3",   // continue without changing privileges (no "authorize" option)
		"xpw", // password
		"",    // create the schema
		"4",   // cancel setup
	), "init")
	if r.code != 0 {
		t.Fatalf("guided init: %+v", r)
	}
	if strings.Contains(r.stdout, "Authorize Vario to grant") {
		t.Fatal("users that cannot be changed with SQL must not be offered grants")
	}
	if !strings.Contains(r.stdout, "privileges cannot be changed with SQL") || !strings.Contains(r.stdout, "Setup cancelled.\nNothing was saved.") {
		t.Fatalf("output: %s", r.stdout)
	}
	if c := ch.changes(); len(c) != 0 {
		t.Fatalf("cancelling must not change ClickHouse: %v", c)
	}
	if fileExists(cfgPath) {
		t.Fatal("cancelling must not write a configuration")
	}
}

func TestInitGuidedAdminFailureFallsBackToManual(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t, "vario")
	r := vario(t, answers(
		url, "", "", "wrong", // guided; the administrative login fails
		"4",                   // continue without administrative access
		"", "collector", "pw", // manual: database, user, password
		"n", "", "", "", // do not verify; mode; T3 Code; save
	), "init")
	if r.code != 0 {
		t.Fatalf("init: %+v", r)
	}
	if !strings.Contains(r.stdout, "ClickHouse rejected the administrative credentials") || !strings.Contains(r.stdout, "manually managed") {
		t.Fatalf("output: %s", r.stdout)
	}
	cfg, _ := config.Load(cfgPath)
	if cfg.ClickHouse.Username != "collector" {
		t.Fatalf("config: %+v", cfg.ClickHouse)
	}
	if len(ch.changes()) != 0 {
		t.Fatal("nothing may be changed")
	}
	noSecrets(t, r, "wrong", "pw\n")
}

func TestInitGuidedSelectingAdminAccountWarns(t *testing.T) {
	isolate(t)
	_, url := guidedFake(t, "vario")
	// Choose the administrative account itself as the operational user, then decline.
	r := vario(t, answers(url, "", "", "adminpw", "1", "1", "2", "1", "n", "3"), "init")
	if !strings.Contains(r.stdout, "is the administrative account you signed in with") {
		t.Fatalf("output: %s", r.stdout)
	}
}
