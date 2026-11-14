package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var checkpointRe = regexp.MustCompile(`checkpoint\s+(\d+) acknowledged by ClickHouse`)

// acknowledged returns the delivery checkpoint `vario status` shows.
func acknowledged(t *testing.T) string {
	t.Helper()
	m := checkpointRe.FindStringSubmatch(vario(t, "", "status").stdout)
	if m == nil {
		t.Fatal("status shows no acknowledged checkpoint")
	}
	return m[1]
}

// toVersion2 makes the database what a Vario version before delivery
// checkpoints left: no vario_checkpoints, schema version 2, the same id.
func (f *fakeCH) toVersion2(db string) {
	f.dropCheckpoints(db)
	f.mu.Lock()
	defer f.mu.Unlock()
	var meta []string
	for _, m := range f.meta[db] {
		if !strings.HasPrefix(m, "schema_version|") {
			meta = append(meta, m)
		}
	}
	f.meta[db] = append(meta, "schema_version|2")
}

// P1 end to end: a backup is taken, more rows are delivered, and the backup
// is restored behind Vario. The database keeps its destination id. Every
// collector command refuses to deliver into it, the buffered rows stay,
// status, doctor and destination explain what happened and how to recover,
// nothing is changed in ClickHouse, and after the explicit switch the
// complete history is delivered again.
func TestDatabaseRollbackIsReportedAndRecovered(t *testing.T) {
	fix, ch := replacementFixture(t)
	migrateAsAdmin(t)
	idA := ch.destinationID("vario")
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("initial delivery: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
	r := vario(t, "", "doctor")
	if r.code != 0 {
		t.Fatalf("doctor before: %+v", r)
	}
	wantAll(t, "doctor before", r.stdout, "✓ delivery checkpoint "+acknowledged(t), "is held by the database: no rollback detected")

	backup := ch.backup("vario")
	fix.Message("msg-after-backup", "thread-1", "run-1", "assistant", "delivered after the backup", false)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 3 {
		t.Fatalf("delivery after the backup: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
	confirmed := acknowledged(t)
	fix.Message("msg-buffered", "thread-1", "run-1", "assistant", "collected, not delivered yet", false)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 || strings.Contains(r.stdout, "Queued 0 records") {
		t.Fatalf("queue: %+v", r)
	}
	queued := spooled(t)
	if queued == "0" {
		t.Fatal("expected buffered rows")
	}

	// DROP DATABASE; RESTORE DATABASE from the backup.
	ch.restore("vario", backup)
	held := ch.checkpoint("vario")
	if ch.destinationID("vario") != idA || ch.inserted("vario", "messages") != 2 || fmt.Sprint(held) >= confirmed {
		t.Fatalf("the restored database keeps its id and lacks rows: id %s, %d messages, checkpoint %d (acknowledged %s)",
			ch.destinationID("vario"), ch.inserted("vario", "messages"), held, confirmed)
	}
	statements := len(ch.changes())

	// Doctor compares with the database itself, so it finds the rollback
	// before any collector has run. Status only knows the local state so far.
	r = vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor (live)", r.stdout, "✓ destination id "+idA, "✗ delivery checkpoint: the database acknowledged checkpoint "+confirmed,
		fmt.Sprintf("but holds only checkpoint %d", held), "restored to an earlier state", "vario destination switch", "reads every source again in full")
	if s := vario(t, "", "status").stdout; strings.Contains(s, "RESTORED TO AN EARLIER STATE") {
		t.Fatalf("status must not report what no collector has seen:\n%s", s)
	}

	// Delivery is refused by every collector command, with the recovery steps.
	for _, args := range [][]string{{"backfill", "--source", "t3code"}, {"run"}} {
		r := vario(t, "", args...)
		if r.code != ExitUsage {
			t.Fatalf("%v must refuse the rolled-back database: %+v", args, r)
		}
		wantAll(t, strings.Join(args, " "), r.stderr, "was restored to an earlier state", "acknowledged delivery checkpoint "+confirmed,
			fmt.Sprintf("now holds only checkpoint %d", held), "destination id "+idA, "Delivery is suspended", "keeps the "+queued+" queued rows",
			"vario destination switch", "reads every source again in full", "complete history")
	}
	if n := ch.inserted("vario", "messages"); n != 2 {
		t.Fatalf("%d rows were delivered to the rolled-back database", n-2)
	}
	if ch.checkpoint("vario") != held {
		t.Fatal("a checkpoint was written into the rolled-back database")
	}
	if got := spooled(t); got != queued {
		t.Fatalf("buffered rows must be kept: %s, had %s", got, queued)
	}

	// Reported by status (without contacting ClickHouse), destination and doctor.
	r = vario(t, "", "status")
	wantAll(t, "status", r.stdout, "! DATABASE RESTORED TO AN EARLIER STATE (detected ", "acknowledged delivery checkpoint "+confirmed,
		fmt.Sprintf("but held only checkpoint %d", held), "recorded destination id "+idA, "Delivery is suspended", queued+" queued rows are kept in the local spool",
		"vario destination switch", "last error     destination: the database was restored to an earlier state")
	if strings.Contains(r.stdout, "DATABASE REPLACED") {
		t.Fatalf("a rollback is not a replacement:\n%s", r.stdout)
	}
	r = vario(t, "", "destination")
	wantAll(t, "destination", r.stdout, "! DATABASE RESTORED TO AN EARLIER STATE", "vario destination switch", "[active, configured]")
	r = vario(t, "", "doctor")
	if r.code != ExitFailure {
		t.Fatalf("doctor must fail: %+v", r)
	}
	wantAll(t, "doctor", r.stdout, "✗ the database of ", "was restored to an earlier state (detected ", queued+" queued rows are kept in the local spool",
		"vario destination switch", "✗ delivery checkpoint: the database acknowledged checkpoint "+confirmed, `See the recovery steps under "Local state" above.`)

	// Nothing was changed in ClickHouse on the way.
	if n := len(ch.changes()); n != statements {
		t.Fatalf("reporting must not change ClickHouse: %v", ch.changes()[statements:])
	}

	// A database that cannot be checked is not accepted blindly.
	ch.setDown(true)
	r = vario(t, "", "destination", "switch")
	if r.code != ExitFailure {
		t.Fatalf("switch while offline: %+v", r)
	}
	wantAll(t, "switch while offline", r.stderr, "was restored to an earlier state", "it cannot be checked now", "Nothing was changed")
	ch.setDown(false)
	if r := vario(t, "", "status"); !strings.Contains(r.stdout, "RESTORED TO AN EARLIER STATE") || spooled(t) != queued {
		t.Fatalf("a refused switch must keep everything: %s", r.stdout)
	}

	// The explicit decision: keep the restored database and reconcile.
	r = vario(t, "", "destination", "switch")
	if r.code != 0 || strings.Contains(r.stdout, "nothing to do") {
		t.Fatalf("switch: %+v", r)
	}
	wantAll(t, "switch", r.stdout, "carries the recorded destination id "+idA, "acknowledged delivery checkpoint "+confirmed,
		"✓ accepted the restored database: its cursors and fingerprints were reset", "reads every source again in full and delivers the complete history",
		queued+" queued rows are kept and delivered to it as well")
	r = vario(t, "", "status")
	if strings.Contains(r.stdout, "RESTORED TO AN EARLIER STATE") || spooled(t) != queued {
		t.Fatalf("status after the switch: %s", r.stdout)
	}
	wantAll(t, "status after the switch", r.stdout, "cursor         -", "checkpoint     none yet")
	if strings.Contains(r.stdout, "last error") {
		t.Fatalf("the suspension is over; its error must be cleared:\n%s", r.stdout)
	}
	if n := ch.inserted("vario", "messages"); n != 2 || len(ch.changes()) != statements {
		t.Fatalf("the switch itself delivers and changes nothing: %d messages", n)
	}
	// Repeated: nothing to do.
	if r = vario(t, "", "destination", "switch"); r.code != 0 || !strings.Contains(r.stdout, "nothing to do") || spooled(t) != queued {
		t.Fatalf("repeated switch: %+v", r)
	}

	// The reconciliation: the message that stayed queued is delivered, and
	// every source is read again in full, so all four messages are sent (those
	// the database already has carry the same keys and versions; ClickHouse
	// keeps one row per record, which the integration tests verify).
	r = vario(t, "", "backfill", "--source", "t3code")
	if r.code != 0 || spooled(t) != "0" {
		t.Fatalf("reconciliation: %+v", r)
	}
	const sent = 2 + 1 + 4 // in the restored database, queued, complete history
	if n := ch.inserted("vario", "messages"); n != sent {
		t.Fatalf("the complete history must be delivered again: %d message rows sent in total, want %d", n, sent)
	}
	if ch.checkpoint("vario") == 0 || ch.destinationID("vario") != idA || len(ch.changes()) != statements {
		t.Fatalf("after the reconciliation: checkpoint %d, id %s, %v", ch.checkpoint("vario"), ch.destinationID("vario"), ch.changes()[statements:])
	}
	r = vario(t, "", "doctor")
	if r.code != 0 {
		t.Fatalf("doctor after the recovery: %+v", r)
	}
	wantAll(t, "doctor after the recovery", r.stdout, "✓ delivery checkpoint "+acknowledged(t), "no rollback detected")

	// A second reconciliation sends nothing new and reports nothing.
	if r = vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || !strings.Contains(r.stdout, "Queued 0 records") || ch.inserted("vario", "messages") != sent {
		t.Fatalf("repeated reconciliation: %+v", r)
	}

	// The old backup restored once more is detected again: it knows nothing
	// of the checkpoint history that started with the recovery.
	ch.restore("vario", backup)
	if r = vario(t, "", "backfill", "--source", "t3code"); r.code != ExitUsage || !strings.Contains(r.stderr, "was restored to an earlier state") {
		t.Fatalf("a second restore of the old backup must be detected: %+v", r)
	}
}

// If the newer state is restored after all, no reconciliation is needed: the
// switch withdraws the report and the collector delivers what stayed queued.
func TestDatabaseRollbackIsWithdrawnWhenTheNewerStateIsBack(t *testing.T) {
	fix, ch := replacementFixture(t)
	migrateAsAdmin(t)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 {
		t.Fatalf("initial delivery: %+v", r)
	}
	old := ch.backup("vario")
	fix.Message("msg-2", "thread-1", "run-1", "assistant", "after the old backup", false)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 {
		t.Fatalf("second delivery: %+v", r)
	}
	current := ch.backup("vario")
	fix.Message("msg-3", "thread-1", "run-1", "assistant", "queued", false)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("queue: %+v", r)
	}
	queued := spooled(t)

	ch.restore("vario", old)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != ExitUsage {
		t.Fatalf("the rollback must be refused: %+v", r)
	}
	if !strings.Contains(vario(t, "", "status").stdout, "RESTORED TO AN EARLIER STATE") {
		t.Fatal("the rollback must be reported")
	}

	ch.restore("vario", current)
	r := vario(t, "", "destination", "switch")
	if r.code != 0 {
		t.Fatalf("switch: %+v", r)
	}
	wantAll(t, "switch", r.stdout, "holds the acknowledged delivery checkpoint", "again: nothing to recover", "nothing to do")
	r = vario(t, "", "status")
	if strings.Contains(r.stdout, "RESTORED TO AN EARLIER STATE") || strings.Contains(r.stdout, "cursor         -") || strings.Contains(r.stdout, "last error") || spooled(t) != queued {
		t.Fatalf("the report must be withdrawn and the state kept: %s", r.stdout)
	}
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || spooled(t) != "0" || ch.inserted("vario", "messages") != 4 {
		t.Fatalf("delivery must resume: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
}

// A database migrated by a Vario version without delivery checkpoints keeps
// working with this one. The missing protection is reported, never claimed,
// and `vario migrate` adds it without changing the database's identity.
func TestExistingDatabaseWithoutCheckpointsIsUpgraded(t *testing.T) {
	fix, ch := replacementFixture(t)
	migrateAsAdmin(t)
	ch.toVersion2("vario")
	idA := ch.destinationID("vario")

	r := vario(t, "", "backfill", "--source", "t3code")
	if r.code != 0 || ch.inserted("vario", "messages") != 2 || ch.checkpoint("vario") != 0 {
		t.Fatalf("delivery to a version 2 database: %+v", r)
	}
	wantAll(t, "backfill", r.stderr, "has no delivery checkpoint table (vario_checkpoints)", "restored to an earlier state cannot be detected; run `vario migrate`")
	wantAll(t, "status", vario(t, "", "status").stdout, "checkpoint     none yet (a database restored to an earlier state is not detected")
	r = vario(t, "", "doctor")
	if r.code != 0 {
		t.Fatalf("doctor: a missing checkpoint table is a warning: %+v", r)
	}
	wantAll(t, "doctor", r.stdout, "! the database has no delivery checkpoint table (vario_checkpoints)", "cannot be detected; run `vario migrate`")
	if strings.Contains(r.stdout, "no rollback detected") {
		t.Fatalf("doctor must not claim what it cannot check:\n%s", r.stdout)
	}

	// The migration adds the table and nothing else.
	n := len(ch.changes())
	r = migrateAsAdmin(t)
	added := ch.changes()[n:]
	if len(added) != 2 || !strings.HasPrefix(added[0], "CREATE TABLE IF NOT EXISTS `vario`.`vario_checkpoints`") || !strings.Contains(added[1], "'schema_version', '3'") {
		t.Fatalf("migration from version 2: %v", added)
	}
	wantAll(t, "migrate", r.stdout, "create table vario_checkpoints", "✓ schema verified: version 3, destination id "+idA)
	if ch.destinationID("vario") != idA {
		t.Fatal("the migration must keep the destination id")
	}

	// The next collector run writes the first checkpoint; nothing is re-read.
	fix.Message("msg-2", "thread-1", "run-1", "assistant", "after the migration", false)
	r = vario(t, "", "backfill", "--source", "t3code")
	if r.code != 0 || ch.inserted("vario", "messages") != 3 || ch.checkpoint("vario") == 0 {
		t.Fatalf("first run after the migration: %+v (%d messages, checkpoint %d)", r, ch.inserted("vario", "messages"), ch.checkpoint("vario"))
	}
	wantAll(t, "first run after the migration", r.stderr, "delivery checkpoints enabled")
	r = vario(t, "", "doctor")
	if r.code != 0 {
		t.Fatalf("doctor after the migration: %+v", r)
	}
	wantAll(t, "doctor after the migration", r.stdout, "✓ delivery checkpoint "+acknowledged(t))

	// Restored to the backup from before the migration: no checkpoint table.
	ch.toVersion2("vario")
	if r = vario(t, "", "backfill", "--source", "t3code"); r.code != ExitUsage {
		t.Fatalf("a restore to the pre-migration state must be refused: %+v", r)
	}
	wantAll(t, "pre-migration restore", r.stderr, "was restored to an earlier state", "now holds none")
	r = vario(t, "", "doctor")
	wantAll(t, "doctor after the restore", r.stdout, "✗ the database has no delivery checkpoint table (vario_checkpoints) that this user can read, but it acknowledged checkpoint")
}
