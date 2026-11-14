package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

// replacementFixture is migrateFixture with access to the synthetic source.
func replacementFixture(t *testing.T) (*t3codetest.Fixture, *fakeCH) {
	t.Helper()
	fix, _ := initFixture(t)
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
	return fix, ch
}

func migrateAsAdmin(t *testing.T) result {
	t.Helper()
	r := vario(t, "adminpw", "migrate", "--non-interactive", "--yes", "--admin-user", "default", "--admin-password-stdin")
	if r.code != 0 {
		t.Fatalf("migrate: %+v", r)
	}
	return r
}

var spooledRe = regexp.MustCompile(`spooled\s+(\d+) rows`)

func spooled(t *testing.T) string {
	t.Helper()
	m := spooledRe.FindStringSubmatch(vario(t, "", "status").stdout)
	if m == nil {
		t.Fatal("status shows no spool line")
	}
	return m[1]
}

func wantAll(t *testing.T, what, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("%s: missing %q in:\n%s", what, w, text)
		}
	}
}

// C-2 end to end: the configured database is dropped and recreated behind
// Vario. Every command refuses to deliver into it, the buffered rows stay,
// status, doctor and destination explain what happened and how to recover,
// nothing is authorized or migrated implicitly, and after the explicit switch
// the new database receives the complete history.
func TestDatabaseReplacementIsReportedAndRecovered(t *testing.T) {
	fix, ch := replacementFixture(t)
	migrateAsAdmin(t)
	idA := ch.destinationID("vario")
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("initial delivery: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "destination id "+idA) || strings.Contains(r.stdout, "REPLACED") {
		t.Fatalf("status before: %s", r.stdout)
	}
	// Rows collected but not delivered yet.
	fix.Message("msg-buffered", "thread-1", "run-1", "assistant", "collected before the replacement", false)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 || strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("queue: %+v", r)
	}
	queued := spooled(t)
	if queued == "0" {
		t.Fatal("expected buffered rows")
	}

	// DROP DATABASE; CREATE DATABASE; vario migrate (by someone else).
	ch.recreate("vario")
	migrateAsAdmin(t)
	idB := ch.destinationID("vario")
	if idB == "" || idB == idA {
		t.Fatalf("a recreated database must get a new destination id: %q, was %q", idB, idA)
	}
	statements := len(ch.changes())

	// Delivery is refused by every collector command, with the recovery steps.
	for _, args := range [][]string{{"backfill", "--source", "t3code"}, {"run"}} {
		r := vario(t, "", args...)
		if r.code != ExitUsage {
			t.Fatalf("%v must refuse the replaced database: %+v", args, r)
		}
		wantAll(t, strings.Join(args, " "), r.stderr, "was replaced", "destination id "+idA, "now carries "+idB, "Delivery is suspended",
			"vario destination switch --move-pending", "vario destination switch --discard-pending", "vario destination switch --keep-pending",
			"reads every source again in full")
	}
	if n := ch.inserted("vario", "messages"); n != 0 {
		t.Fatalf("%d rows were delivered to the replaced database", n)
	}
	if got := spooled(t); got != queued {
		t.Fatalf("buffered rows must be kept: %s, had %s", got, queued)
	}

	// Reported by status (without contacting ClickHouse), destination and doctor.
	r := vario(t, "", "status")
	wantAll(t, "status", r.stdout, "! DATABASE REPLACED (detected ", "now carries destination id "+idB, "Vario delivered to "+idA,
		"Delivery is suspended", queued+" queued rows are kept in the local spool", "vario destination switch --move-pending", "vario migrate")
	r = vario(t, "", "destination")
	wantAll(t, "destination", r.stdout, "! DATABASE REPLACED", "vario destination switch --move-pending")
	r = vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor", r.stdout, "✗ the database of ", "was replaced (detected ", queued+" queued rows are kept in the local spool",
		"vario destination switch --move-pending", "✗ destination id "+idB+" does not match "+idA, `See the recovery steps under "Local state" above.`)

	// Nothing was authorized or migrated on the way.
	if n := len(ch.changes()); n != statements {
		t.Fatalf("reporting must not change ClickHouse: %v", ch.changes()[statements:])
	}
	if got := ch.destinationID("vario"); got != idB {
		t.Fatalf("the database's identity changed: %s", got)
	}

	// A replaced database that cannot be identified is not accepted blindly,
	// and the switch does not migrate it.
	ch.recreate("vario")
	r = vario(t, "", "destination", "switch", "--move-pending")
	if r.code != ExitFailure || len(ch.changes()) != statements {
		t.Fatalf("switch without a schema: %+v %v", r, ch.changes()[statements:])
	}
	wantAll(t, "switch without a schema", r.stderr, "was replaced", "it cannot be checked now (migration required)", "vario migrate", "Nothing was changed")
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "DATABASE REPLACED") || spooled(t) != queued {
		t.Fatalf("a refused switch must keep everything: %s", r.stdout)
	}

	// The same for a schema that carries no destination id.
	migrateAsAdmin(t)
	ch.stripMeta("vario")
	r = vario(t, "", "destination", "switch", "--move-pending")
	if r.code != ExitFailure || spooled(t) != queued {
		t.Fatalf("switch without a destination id: %+v", r)
	}
	wantAll(t, "switch without a destination id", r.stderr, "was replaced", "it reports no destination id", "vario migrate", "Nothing was changed")

	// The explicit decision.
	migrateAsAdmin(t)
	idC := ch.destinationID("vario")
	r = vario(t, "", "destination", "switch", "--move-pending")
	if r.code != 0 {
		t.Fatalf("switch: %+v", r)
	}
	wantAll(t, "switch", r.stdout, "carries destination id "+idC, "Vario delivered to "+idA, "recorded as a new destination without cursors",
		queued+" queued rows will be delivered", "reads every source in full")
	r = vario(t, "", "status")
	if strings.Contains(r.stdout, "REPLACED") || !strings.Contains(r.stdout, "destination id "+idC) || !strings.Contains(r.stdout, "cursor         -\n") {
		t.Fatalf("status after the switch: %s", r.stdout)
	}
	if got := spooled(t); got != queued {
		t.Fatalf("moved rows must be queued for the new destination: %s, had %s", got, queued)
	}

	// Complete reconciliation: the two messages delivered to the lost
	// database and the buffered one all reach the new one.
	r = vario(t, "", "backfill", "--source", "t3code")
	if r.code != 0 || strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("the new destination must receive a full backfill: %+v", r)
	}
	if n := ch.inserted("vario", "messages"); n < 3 {
		t.Fatalf("the new database is incomplete: %d message rows, want every one of 3 messages", n)
	}
	for _, table := range []string{"sources", "projects", "conversations", "runs", "raw_events"} {
		if ch.inserted("vario", table) == 0 {
			t.Fatalf("the new database received no %s", table)
		}
	}
	if got := spooled(t); got != "0" {
		t.Fatalf("everything must be delivered: %s rows left", got)
	}
	r = vario(t, "", "doctor")
	if r.code != 0 || strings.Contains(r.stdout, "replaced") {
		t.Fatalf("doctor after recovery: %+v", r)
	}
	wantAll(t, "doctor after recovery", r.stdout, "✓ destination id "+idC+" (the database Vario delivered to before)")
	if r := vario(t, "", "status"); strings.Contains(r.stdout, "last error") {
		t.Fatalf("the stale error must be gone: %s", r.stdout)
	}
}

// doctor compares the database's destination id with the recorded one, so it
// finds a replacement before any collector has seen it.
func TestDoctorDetectsReplacementBeforeTheCollector(t *testing.T) {
	_, ch := replacementFixture(t)
	migrateAsAdmin(t)
	idA := ch.destinationID("vario")
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 {
		t.Fatalf("backfill: %+v", r)
	}
	ch.recreate("vario")
	migrateAsAdmin(t)
	idB := ch.destinationID("vario")
	r := vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor", r.stdout, "✗ destination id "+idB+" does not match "+idA, "the database was replaced",
		"vario destination switch --move-pending", "vario destination switch --discard-pending", "reads every source again in full")
	if strings.Contains(r.stdout, "See the recovery steps") {
		t.Fatalf("the recovery steps must be given where the problem is reported: %s", r.stdout)
	}
	// Doctor only reads: the decision is still open.
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != ExitUsage || !strings.Contains(r.stderr, "was replaced") {
		t.Fatalf("doctor must not authorize the database: %+v", r)
	}
	// Without rows to decide about, the switch needs no flag.
	if r := vario(t, "", "destination", "switch"); r.code != 0 || !strings.Contains(r.stdout, "recorded as a new destination") {
		t.Fatalf("switch: %+v", r)
	}
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("full backfill after the switch: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
}

// A database migrated by Vario 0.1 (no vario_meta) has no destination id.
// The supported way to get one is `vario migrate`; the id is then bound to
// the existing destination without a second backfill, it persists, and it
// protects the destination like any other.
func TestLegacyDatabaseAcquiresDestinationID(t *testing.T) {
	_, ch := replacementFixture(t)
	migrateAsAdmin(t)
	ch.stripMeta("vario") // the schema as version 1 left it
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("delivery to a legacy schema: %+v", r)
	}
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "(destination id -)") {
		t.Fatalf("no identity yet: %s", r.stdout)
	}
	r := vario(t, "", "doctor")
	wantAll(t, "doctor on a legacy schema", r.stdout, "! the database has no destination id yet", "a replaced database cannot be detected", "run `vario migrate` to record one")
	if id := ch.destinationID("vario"); id != "" {
		t.Fatalf("only migrate may record an identity, found %s", id)
	}

	r = migrateAsAdmin(t)
	id := ch.destinationID("vario")
	if id == "" || !strings.Contains(r.stdout, "record destination id "+id) {
		t.Fatalf("migrate must record a destination id: %q\n%s", id, r.stdout)
	}
	for _, q := range ch.changes() {
		for _, bad := range []string{"DROP", "ALTER", "TRUNCATE"} {
			if strings.HasPrefix(q, bad) {
				t.Fatalf("the legacy tables must not be touched: %s", q)
			}
		}
	}
	// Bound to the existing destination: nothing is sent again.
	r = vario(t, "", "backfill", "--source", "t3code")
	if r.code != 0 || !strings.Contains(r.stdout, "Queued 0 records") || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("acquiring the identity must not cause a new backfill: %+v", r)
	}
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "(destination id "+id+")") {
		t.Fatalf("the identity must be recorded locally: %s", r.stdout)
	}
	if r := vario(t, "", "doctor"); r.code != 0 || !strings.Contains(r.stdout, "✓ destination id "+id+" (the database Vario delivered to before)") {
		t.Fatalf("doctor after migrating: %+v", r)
	}
	// Persistent: migrating again keeps it.
	if r := vario(t, "", "migrate", "--non-interactive"); r.code != 0 || !strings.Contains(r.stdout, "Nothing to do") || ch.destinationID("vario") != id {
		t.Fatalf("repeated migrate: %+v", r)
	}
	// Losing vario_meta afterwards is noticed instead of delivering blindly.
	ch.stripMeta("vario")
	r = vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor without vario_meta", r.stdout, "✗ the database reports no destination id, but Vario recorded "+id, "vario migrate")
}
