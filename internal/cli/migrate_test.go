package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
)

// migrateFixture configures Vario for a fake server with an administrator
// (default/adminpw) and a restricted operational user vario/opw that holds
// SELECT and INSERT on the vario database.
func migrateFixture(t *testing.T) (*fakeCH, string) {
	t.Helper()
	_, cfgPath := initFixture(t)
	ch, url := newFakeCH(t, "unused", "vario")
	ch.addUser("default", "adminpw", "users_xml", true)
	ch.addUser("vario", "opw", "local_directory", false)
	ch.grant("vario", "SELECT vario.*", "INSERT vario.*")
	for _, kv := range [][2]string{{"clickhouse.url", url}, {"clickhouse.username", "vario"}} {
		if r := vario(t, "", "config", "set", kv[0], kv[1]); r.code != 0 {
			t.Fatal(r)
		}
	}
	vario(t, "opw\n", "config", "set-password", "--stdin")
	return ch, cfgPath
}

func TestMigrate(t *testing.T) {
	ch, cfgPath := migrateFixture(t)

	// Dry run: no administrative credentials, nothing changed, SQL shown.
	r := vario(t, "", "migrate", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, "no Vario schema") || !strings.Contains(r.stdout, "CREATE TABLE IF NOT EXISTS `vario`.`messages`") ||
		!strings.Contains(r.stdout, "Dry run: nothing was changed") || strings.Contains(r.stdout, "Administrative") {
		t.Fatalf("dry run: %+v", r)
	}
	if c := ch.changes(); len(c) != 0 {
		t.Fatalf("a dry run must not change anything: %v", c)
	}
	// Non-interactive without --yes: refused after showing the plan.
	r = vario(t, "adminpw", "migrate", "--non-interactive", "--admin-user", "default", "--admin-password-stdin")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--yes") || len(ch.changes()) != 0 {
		t.Fatalf("unconfirmed migration: %+v", r)
	}
	// Wrong administrative password.
	r = vario(t, "nope", "migrate", "--non-interactive", "--yes", "--admin-user", "default", "--admin-password-stdin")
	if r.code == 0 || !strings.Contains(r.stderr, "administrative login as default failed (authentication)") || len(ch.changes()) != 0 {
		t.Fatalf("wrong admin password: %+v", r)
	}
	// Applied, verified, and checked as the collector user.
	r = vario(t, "adminpw", "migrate", "--non-interactive", "--yes", "--admin-user", "default", "--admin-password-stdin")
	if r.code != 0 || !strings.Contains(r.stdout, "✓ schema verified: version 3") || !strings.Contains(r.stdout, "Checking access as the collector user vario") ||
		!strings.Contains(r.stdout, "inserts accepted") {
		t.Fatalf("migrate: %+v", r)
	}
	n := len(ch.changes())
	if n == 0 {
		t.Fatal("expected schema statements")
	}
	for _, q := range ch.changes() {
		for _, bad := range []string{"DROP", "ALTER", "TRUNCATE", "CREATE USER", "CREATE DATABASE"} {
			if strings.HasPrefix(q, bad) {
				t.Fatalf("unexpected statement: %s", q)
			}
		}
	}
	noSecrets(t, r, "adminpw", "opw")
	for _, f := range []string{cfgPath, filepath.Join(filepath.Dir(cfgPath), "clickhouse-password")} {
		if strings.Contains(readFile(t, f), "adminpw") {
			t.Fatal("the administrative password must never be stored")
		}
	}
	// Up to date: nothing to do, no credentials needed.
	r = vario(t, "", "migrate", "--non-interactive")
	if r.code != 0 || !strings.Contains(r.stdout, "up to date (version 3). Nothing to do.") || len(ch.changes()) != n {
		t.Fatalf("repeated migrate: %+v", r)
	}
}

func TestMigrateCreatesDatabaseAndGrantsNewObjects(t *testing.T) {
	ch, _ := migrateFixture(t)
	// The collector user has table-level grants from an older setup and the
	// database does not exist yet.
	u := ch.user("vario")
	u.grants = map[string]bool{}
	for _, tbl := range []string{"sources", "projects", "conversations", "runs", "messages", "execution_events", "provider_turns", "raw_events"} {
		u.grants["SELECT fresh."+tbl], u.grants["INSERT fresh."+tbl] = true, true
	}
	vario(t, "", "config", "set", "clickhouse.database", "fresh")
	r := vario(t, "adminpw", "migrate", "--dry-run", "--non-interactive", "--admin-user", "default", "--admin-password-stdin")
	if r.code != 0 || !strings.Contains(r.stdout, "create database fresh") || !strings.Contains(r.stdout, "GRANT SELECT ON `fresh`.`vario_meta` TO `vario`") || len(ch.changes()) != 0 {
		t.Fatalf("dry run with administrative inspection: %+v", r)
	}
	// Interactive: the database, the schema and the grants are confirmed separately.
	r = vario(t, answers("", "adminpw", "y", "", ""), "migrate")
	if r.code != 0 || !strings.Contains(r.stdout, "Database fresh does not exist. Create it") || !strings.Contains(r.stdout, "Grant these privileges to vario?") {
		t.Fatalf("interactive migrate: %+v", r)
	}
	if !u.grants["SELECT fresh.vario_meta"] || !strings.Contains(r.stdout, "inserts accepted") {
		t.Fatalf("the collector user should be granted the new objects: %v\n%s", u.grants, r.stdout)
	}
	// Declining everything changes nothing.
	ch2, _ := migrateFixture(t)
	vario(t, "", "config", "set", "clickhouse.database", "other")
	if r := vario(t, answers("", "adminpw", "n"), "migrate"); r.code != 0 || len(ch2.changes()) != 0 {
		t.Fatalf("declined: %+v %v", r, ch2.changes())
	}
}

func TestMigrateStopsAtIncompatibleObjects(t *testing.T) {
	ch, _ := migrateFixture(t)
	ch.objects["vario"] = map[string]*fakeObj{"messages": {engine: "MergeTree", cols: [][2]string{{"id", "UInt64"}}}}
	ch.grant("vario", "SELECT vario.messages")
	r := vario(t, "adminpw", "migrate", "--non-interactive", "--yes", "--admin-user", "default", "--admin-password-stdin")
	if r.code != ExitFailure || !strings.Contains(r.stdout, "incompatible objects") || len(ch.changes()) != 0 {
		t.Fatalf("incompatible: %+v %v", r, ch.changes())
	}
}

func TestDestinationChangeRequiresDecision(t *testing.T) {
	_, cfgPath := initFixture(t)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("backfill: %+v", r)
	}
	// Rotating credentials keeps the destination.
	if r := vario(t, "", "config", "set", "clickhouse.username", "rotated"); r.code != 0 || strings.Contains(r.stdout, "destination changed") {
		t.Fatalf("credential rotation: %+v", r)
	}
	vario(t, "new-secret\n", "config", "set-password", "--stdin")
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 || !strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("same destination after rotation: %+v", r)
	}

	// Changing the database with queued rows: explained, and the collector refuses.
	r := vario(t, "", "config", "set", "clickhouse.database", "other")
	if r.code != 0 || !strings.Contains(r.stdout, "will NOT be sent to the new one") {
		t.Fatalf("config set: %+v", r)
	}
	for _, args := range [][]string{{"run"}, {"backfill", "--source", "t3code", "--no-deliver"}} {
		r = vario(t, "", args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "the ClickHouse destination changed") || !strings.Contains(r.stderr, "vario destination switch --keep-pending") {
			t.Fatalf("%v must refuse: %+v", args, r)
		}
	}
	if r := vario(t, "", "doctor"); !strings.Contains(r.stdout, "the collector will not start") {
		t.Fatalf("doctor: %s", r.stdout)
	}
	// Keep the rows for the previous destination; the new one gets a full backfill.
	if r = vario(t, "", "destination", "switch", "--keep-pending"); r.code != 0 || !strings.Contains(r.stdout, "stay with") || !strings.Contains(r.stdout, "reads every source in full") {
		t.Fatalf("switch --keep-pending: %+v", r)
	}
	r = vario(t, "", "backfill", "--source", "t3code", "--no-deliver")
	if r.code != 0 || strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("the new destination must receive a full backfill: %+v", r)
	}
	r = vario(t, "", "destination")
	if strings.Count(r.stdout, "sources with a cursor 1") != 2 {
		t.Fatalf("both destinations keep their state: %s", r.stdout)
	}

	// A direct edit of the file (bypassing the CLI) is detected by the collector.
	text := strings.Replace(readFile(t, cfgPath), `database = "other"`, `database = "vario"`, 1)
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if r = vario(t, "", "run"); r.code != ExitUsage || !strings.Contains(r.stderr, "the ClickHouse destination changed") {
		t.Fatalf("direct edit: %+v", r)
	}
	// Discarding requires confirmation in scripts.
	if r = vario(t, "", "destination", "switch", "--discard-pending"); r.code != 0 && !strings.Contains(r.stdout+r.stderr, "Permanently delete") {
		t.Fatalf("discard without --yes: %+v", r)
	}
	if r = vario(t, "", "destination", "switch", "--discard-pending", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "deleted") {
		t.Fatalf("discard: %+v", r)
	}
	// Back on the original destination, which still has its own rows and cursor.
	r = vario(t, "", "status")
	if !strings.Contains(r.stdout, "cursor         seq") || !strings.Contains(r.stdout, "messages=2") {
		t.Fatalf("original destination: %s", r.stdout)
	}
}

func TestDestinationSwitchMovesPending(t *testing.T) {
	initFixture(t)
	vario(t, "", "backfill", "--source", "t3code", "--no-deliver")
	vario(t, "", "config", "set", "clickhouse.url", "http://localhost:1")
	r := vario(t, answers("2"), "destination", "switch")
	if r.code != 0 || !strings.Contains(r.stdout, "will be delivered to http://localhost:1") {
		t.Fatalf("interactive move: %+v", r)
	}
	r = vario(t, "", "status")
	if !strings.Contains(r.stdout, "messages=2") || strings.Contains(r.stdout, "other destinations") {
		t.Fatalf("moved rows belong to the new destination: %s", r.stdout)
	}
}

// State written by Vario 0.1 is bound to the previous destination when the
// configuration changes before the collector ran with the new version.
func TestLegacySpoolAndDestinationChange(t *testing.T) {
	_, cfgPath := initFixture(t)
	cfg, _ := config.Load(cfgPath)
	path := filepath.Join(cfg.Collector.DataDir, config.StateFileName)
	os.MkdirAll(cfg.Collector.DataDir, 0o700)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE queue (id INTEGER PRIMARY KEY AUTOINCREMENT, dest_table TEXT NOT NULL, token TEXT NOT NULL, row_count INTEGER NOT NULL, raw_bytes INTEGER NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE cursors (source_id TEXT PRIMARY KEY, cursor TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE fingerprints (source_id TEXT NOT NULL, record_key TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY (source_id, record_key)) WITHOUT ROWID`,
		`CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO queue (dest_table, token, row_count, raw_bytes, body, created_at) VALUES ('messages', 't', 5, 1, x'00', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "not recorded yet") {
		t.Fatalf("legacy status: %s", r.stdout)
	}
	r := vario(t, "", "config", "set", "clickhouse.database", "elsewhere")
	if !strings.Contains(r.stdout, "5 queued rows belong to that destination") {
		t.Fatalf("legacy rows must stay with the previous destination: %+v", r)
	}
	if r := vario(t, "", "run"); r.code != ExitUsage || !strings.Contains(r.stderr, "5 rows queued") {
		t.Fatalf("run must refuse: %+v", r)
	}
}
