package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// backup takes a real backup of db (BACKUP DATABASE ... TO File) and returns
// its name. The file lives in the test server's backup directory and goes
// away with the throwaway server. Servers without a backup location skip.
func (e *recoveryEnv) backup(db string) string {
	e.t.Helper()
	name := fmt.Sprintf("%s_%d", db, time.Now().UnixNano())
	if err := e.clients[db].Exec(ctx, fmt.Sprintf("BACKUP DATABASE `%s` TO File('%s')", db, name)); err != nil {
		var ce *clickhouse.Error
		if errors.As(err, &ce) && (ce.Code == 36 || ce.Code == 497 || ce.Code == 627) {
			e.t.Skipf("the test server does not allow BACKUP ... TO File: %v", err)
		}
		e.t.Fatal(err)
	}
	return name
}

// restore replaces db with a backup: DROP DATABASE, RESTORE DATABASE.
func (e *recoveryEnv) restore(db, name string) {
	e.t.Helper()
	for _, q := range []string{"DROP DATABASE `" + db + "` SYNC", fmt.Sprintf("RESTORE DATABASE `%s` FROM File('%s')", db, name)} {
		if err := e.clients[db].Exec(ctx, q); err != nil {
			e.t.Fatalf("%s: %v", q, err)
		}
	}
}

// complete is what a database holds that received the source's whole history
// without interruption: a second database, fed by a second spool.
func (e *recoveryEnv) complete(db string) logical {
	e.t.Helper()
	e.migrate(db)
	store, err := spool.Open(filepath.Join(e.t.TempDir(), "reference.db"), 64<<20)
	if err != nil {
		e.t.Fatal(err)
	}
	defer store.Close()
	c := New(store, e.clients[db], []sources.Source{e.src}, e.collector(db).opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		e.t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, e.src, false, true); err != nil {
		e.t.Fatal(err)
	}
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
		e.t.Fatalf("reference: %+v %v", dr, err)
	}
	return e.mustRead(db)
}

func (e *recoveryEnv) checkpoints(db string, dest int64) (local spool.Checkpoint, held uint64) {
	e.t.Helper()
	local, err := e.store.Checkpoint(ctx, dest)
	if err != nil {
		e.t.Fatal(err)
	}
	if local.Writer == "" {
		return local, 0
	}
	held, _, err = e.clients[db].ReadCheckpoint(ctx, local.Writer)
	if err != nil {
		e.t.Fatal(err)
	}
	return local, held
}

// P1 against a real server: BACKUP DATABASE, further deliveries, DROP and
// RESTORE DATABASE. The restored database carries the same destination id,
// accepts inserts and lacks what was delivered after the backup. The
// collector must refuse it, keep what is queued, and after the explicit
// acceptance deliver the complete history, counted once by the views.
func TestIntegrationRollbackAfterBackupIsDetectedAndRecovered(t *testing.T) {
	e := newRecoveryEnv(t)
	suffix := time.Now().UnixNano()
	db := fmt.Sprintf("vario_rb_%d", suffix)
	id := e.migrate(db)
	c := e.prepared(db)
	destID := c.Destination().ID
	e.syncAndDrain(c)
	atBackup := e.mustRead(db)
	name := e.backup(db)

	e.moreHistory(1)
	e.syncAndDrain(c)
	if got := e.mustRead(db); got.Convs != atBackup.Convs+1 || got.Turns != atBackup.Turns+1 || got.Input != atBackup.Input+1000 {
		t.Fatalf("delivery after the backup: %+v, backup had %+v", got, atBackup)
	}
	e.moreHistory(2) // collected, not delivered yet
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, err := e.store.Stats(ctx)
	if err != nil || queued.Rows == 0 {
		t.Fatalf("expected queued rows: %+v %v", queued, err)
	}
	want := e.complete(fmt.Sprintf("vario_rb_ref_%d", suffix))
	if want.Convs != atBackup.Convs+2 || want.Turns != atBackup.Turns+2 || want.Input != atBackup.Input+2000 {
		t.Fatalf("reference: %+v", want)
	}

	for round := 1; round <= 2; round++ {
		e.restore(db, name)
		info, err := e.clients[db].CheckDestination(ctx)
		if err != nil || info.ServerID != id || !info.Checkpoints {
			t.Fatalf("round %d: the restored database must keep its identity: %+v %v", round, info, err)
		}
		if got := e.mustRead(db); got != atBackup {
			t.Fatalf("round %d: the restored database holds the backup: %+v, want %+v", round, got, atBackup)
		}
		local, held := e.checkpoints(db, destID)
		if local.Confirmed == 0 || held >= local.Confirmed {
			t.Fatalf("round %d: the restored database must hold a lower checkpoint: %d, acknowledged %d", round, held, local.Confirmed)
		}
		pendingBefore, _ := e.store.Stats(ctx)

		// The running collector: its next destination check stops delivery.
		c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
		var de *DestinationError
		if _, err := c.Drain(ctx); !errors.As(err, &de) || !de.RolledBack || de.Replaced || de.Checkpoint != local.Confirmed || de.FoundCheckpoint != held {
			t.Fatalf("round %d: delivery must stop with a rollback error: %v", round, err)
		}
		// A restart is refused as well.
		if _, err := e.collector(db).Prepare(ctx, true); !errors.As(err, &de) || !de.RolledBack {
			t.Fatalf("round %d: a restart must be refused: %v", round, err)
		}
		if got := e.mustRead(db); got != atBackup {
			t.Fatalf("round %d: rows were delivered into the rolled-back database: %+v", round, got)
		}
		if st, _ := e.store.Stats(ctx); st.Rows != pendingBefore.Rows {
			t.Fatalf("round %d: queued rows must be kept: %d of %d", round, st.Rows, pendingBefore.Rows)
		}
		if _, now := e.checkpoints(db, destID); now != held {
			t.Fatalf("round %d: a checkpoint was written into the rolled-back database: %d, was %d", round, now, held)
		}
		rb, ok, err := e.store.RolledBack(ctx)
		if err != nil || !ok || rb.Destination.ID != destID || rb.Destination.ServerID != id || rb.Confirmed != local.Confirmed || rb.Found != held {
			t.Fatalf("round %d: the rollback must be recorded: %+v %v %v", round, rb, ok, err)
		}
		if _, ok, _ := e.store.Replaced(ctx); ok {
			t.Fatal("a rollback is not a replacement")
		}

		// `vario destination switch`, then the complete reconciliation.
		if err := e.store.AcceptRollback(ctx, destID); err != nil {
			t.Fatal(err)
		}
		c = e.prepared(db)
		if c.Destination().ID != destID {
			t.Fatalf("round %d: the destination must stay the same: %+v", round, c.Destination())
		}
		e.syncAndDrain(c)
		if got := e.mustRead(db); got != want {
			t.Fatalf("round %d: after the reconciliation the database must hold the complete history once:\n got %+v\nwant %+v", round, got, want)
		}
		next, heldNow := e.checkpoints(db, destID)
		if next.Writer == local.Writer || next.Confirmed == 0 || heldNow != next.Confirmed {
			t.Fatalf("round %d: a new checkpoint history must start: %+v, held %d", round, next, heldNow)
		}
		// Reconciling again, forced, changes no result.
		if _, err := c.Snapshot(ctx, e.src, true, true); err != nil {
			t.Fatal(err)
		}
		e.syncAndDrain(c)
		if got := e.mustRead(db); got != want {
			t.Fatalf("round %d: a repeated reconciliation must not change results: %+v, want %+v", round, got, want)
		}
		// Second round: the same old backup once more. It knows nothing of the
		// checkpoint history that started with the recovery.
	}

	// Normal operation continues incrementally.
	e.moreHistory(3)
	e.syncAndDrain(c)
	if got := e.mustRead(db); got.Convs != want.Convs+1 || got.Turns != want.Turns+1 || got.Input != want.Input+1000 || got.Msgs != want.Msgs+1 {
		t.Fatalf("after the recovery: %+v, was %+v", got, want)
	}
	if _, ok, _ := e.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported after the recovery")
	}
}

// The backup is restored behind a running, idle collector. Its periodic
// check must notice (while the database is gone, delivery is only retried),
// stop the collector and record the rollback.
func TestIntegrationRollbackWhileRunning(t *testing.T) {
	old := identityCheckInterval
	identityCheckInterval = 200 * time.Millisecond
	t.Cleanup(func() { identityCheckInterval = old })

	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_rb_run_%d", time.Now().UnixNano())
	id := e.migrate(db)
	first := e.prepared(db)
	e.syncAndDrain(first)
	atBackup := e.mustRead(db)
	name := e.backup(db)

	c := e.prepared(db)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	e.moreHistory(1)
	waitFor(t, func() bool {
		got, err := e.read(db)
		return err == nil && got.Convs == atBackup.Convs+1 && got.Turns == atBackup.Turns+1
	})
	waitFor(t, func() bool { st, err := e.store.Stats(ctx); return err == nil && st.Rows == 0 }) // idle
	time.Sleep(3 * identityCheckInterval)
	if _, ok, _ := e.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported for an unchanged database")
	}

	e.restore(db, name)
	var de *DestinationError
	select {
	case err := <-done:
		if !errors.As(err, &de) || !de.RolledBack || de.Configured.ServerID != id {
			t.Fatalf("Run should stop with a rollback error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the running collector did not notice the restored database")
	}
	if got := e.mustRead(db); got != atBackup {
		t.Fatalf("nothing may be delivered into the restored database: %+v, backup had %+v", got, atBackup)
	}
	if rb, ok, _ := e.store.RolledBack(ctx); !ok || rb.Destination.ServerID != id {
		t.Fatalf("the rollback must be recorded for status and doctor: %+v %v", rb, ok)
	}
	if d, _, _ := e.store.Active(ctx); d.ServerID != id || !d.Retired.IsZero() {
		t.Fatalf("the destination and its identity are unchanged: %+v", d)
	}
}

// lossySink commits every insert and every checkpoint, but reports every
// other one as failed, like connections that drop after ClickHouse accepted
// the data.
type lossySink struct {
	*clickhouse.Client
	inserts, checkpoints int
}

func (s *lossySink) Insert(ctx context.Context, table string, body []byte, token string) error {
	if err := s.Client.Insert(ctx, table, body, token); err != nil {
		return err
	}
	if s.inserts++; s.inserts%2 == 1 {
		return &clickhouse.Error{Message: "read: connection reset by peer", Transient: true}
	}
	return nil
}

func (s *lossySink) WriteCheckpoint(ctx context.Context, writer string, seq uint64) error {
	if err := s.Client.WriteCheckpoint(ctx, writer, seq); err != nil {
		return err
	}
	if s.checkpoints++; s.checkpoints%2 == 1 {
		return &clickhouse.Error{Message: "read: connection reset by peer", Transient: true}
	}
	return nil
}

// Ambiguous acknowledgements of inserts and of checkpoints, against a real
// server: rows are resent until a checkpoint covers them, results are not
// inflated, the acknowledged checkpoint never exceeds what the database
// holds, and the checkpoints that arrived unnoticed are not taken for a
// rollback by a later start.
func TestIntegrationAmbiguousCheckpointsAndInserts(t *testing.T) {
	e := newRecoveryEnv(t)
	suffix := time.Now().UnixNano()
	db := fmt.Sprintf("vario_rb_amb_%d", suffix)
	e.migrate(db)
	e.moreHistory(1)
	sink := &lossySink{Client: e.clients[db]}
	c := New(e.store, sink, []sources.Source{e.src}, e.collector(db).opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	destID := c.Destination().ID
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		dr, err := c.Drain(ctx)
		local, held := e.checkpoints(db, destID)
		if local.Confirmed > held {
			t.Fatalf("checkpoint %d is recorded as acknowledged, the database holds %d", local.Confirmed, held)
		}
		var de *DestinationError
		if errors.As(err, &de) {
			t.Fatalf("an unnoticed checkpoint must not be taken for a rollback: %v", err)
		}
		if err == nil && dr.Remaining == 0 {
			break
		}
		if i == 300 {
			t.Fatalf("spool did not drain: %+v %v", dr, err)
		}
	}
	if sink.inserts < 2 || sink.checkpoints < 2 {
		t.Fatalf("the test must exercise lost responses: %d inserts, %d checkpoints", sink.inserts, sink.checkpoints)
	}
	want := e.complete(fmt.Sprintf("vario_rb_amb_ref_%d", suffix))
	if got := e.mustRead(db); got != want {
		t.Fatalf("ambiguous deliveries changed results:\n got %+v\nwant %+v", got, want)
	}
	local, held := e.checkpoints(db, destID)
	if local.Confirmed == 0 || held < local.Confirmed {
		t.Fatalf("checkpoints: acknowledged %d, held %d", local.Confirmed, held)
	}
	// A restart with a reliable connection: no rollback, nothing to resend.
	restarted := e.prepared(db)
	e.syncAndDrain(restarted)
	if got := e.mustRead(db); got != want {
		t.Fatalf("after the restart: %+v, want %+v", got, want)
	}
	if _, ok, _ := e.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported")
	}
}

// A database migrated before delivery checkpoints existed (schema version 2)
// keeps receiving data. After `vario migrate` the collector writes its first
// checkpoint without reading the source again, and a restore is detected
// from then on, including one to the backup from before the migration.
func TestIntegrationVersion2DatabaseGetsCheckpoints(t *testing.T) {
	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_rb_v2_%d", time.Now().UnixNano())
	id := e.migrate(db)
	admin := e.clients[db]
	// The schema as version 2 left it. The test drops the table; Vario never does.
	for _, q := range []string{"DROP TABLE `" + db + "`.vario_checkpoints", "TRUNCATE TABLE `" + db + "`.vario_meta",
		fmt.Sprintf("INSERT INTO `%s`.vario_meta (key, value) VALUES ('destination_id', '%s'), ('schema_version', '2')", db, id)} {
		if err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	info, err := admin.CheckDestination(ctx)
	if err != nil || info.Version != 2 || info.Checkpoints || info.ServerID != id {
		t.Fatalf("version 2 schema: %+v %v", info, err)
	}

	c := e.prepared(db)
	destID := c.Destination().ID
	e.syncAndDrain(c)
	before := e.mustRead(db)
	if before.Msgs != 2 || before.Convs != 1 {
		t.Fatalf("delivery to a version 2 schema: %+v", before)
	}
	if cp, _ := e.store.Checkpoint(ctx, destID); cp != (spool.Checkpoint{}) {
		t.Fatalf("no checkpoint can exist yet: %+v", cp)
	}
	old := e.backup(db)
	cursor, _, _ := e.store.Cursor(ctx, e.src.ID())

	if again := e.migrate(db); again != id { // `vario migrate`
		t.Fatalf("the migration must keep the destination id: %s, was %s", again, id)
	}
	c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	e.moreHistory(1)
	e.syncAndDrain(c)
	local, held := e.checkpoints(db, destID)
	if local.Confirmed == 0 || held != local.Confirmed {
		t.Fatalf("checkpoints must start after the migration: %+v, held %d", local, held)
	}
	if cur, _, _ := e.store.Cursor(ctx, e.src.ID()); cur == cursor || cur == "" {
		t.Fatalf("the destination must keep synchronizing incrementally: %q, was %q", cur, cursor)
	}
	if got := e.mustRead(db); got.Msgs != before.Msgs+1 || got.Convs != before.Convs+1 || got.Events != before.Events {
		t.Fatalf("after the migration: %+v, before %+v plus one conversation", got, before)
	}

	// Restored to the backup from before the migration: no checkpoint table.
	e.restore(db, old)
	var de *DestinationError
	if _, err := e.collector(db).Prepare(ctx, true); !errors.As(err, &de) || !de.RolledBack || de.FoundCheckpoint != 0 {
		t.Fatalf("a restore to the state before the migration must be refused: %v", err)
	}
}

// P2: a vario_checkpoints table that carries Vario's comment and columns but
// discards rows (ENGINE = Null) stops delivery at startup and while running:
// rows stay spooled, nothing is acknowledged and no checkpoint is recorded,
// until the table is replaced by an operator and `vario migrate` was run.
func TestIntegrationIncompatibleCheckpointTableStopsDelivery(t *testing.T) {
	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_cp_null_%d", time.Now().UnixNano())
	e.migrate(db)
	admin := e.clients[db]
	table := "`" + db + "`." + clickhouse.CheckpointTable
	toNull := func() {
		t.Helper()
		// The test replaces the table; Vario never does.
		for _, q := range []string{"DROP TABLE " + table, "CREATE TABLE " + table +
			" (writer_id String, seq UInt64, written_at DateTime64(3, 'UTC') DEFAULT now64(3)) ENGINE = Null COMMENT 'vario:checkpoints'"} {
			if err := admin.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	refused := func(c *Collector) {
		t.Helper()
		dr, err := c.Drain(ctx)
		var se *clickhouse.SchemaError
		if !errors.As(err, &se) || !strings.Contains(err.Error(), "engine is Null") || dr.Remaining == 0 {
			t.Fatalf("delivery must be refused with rows kept: %+v %v", dr, err)
		}
	}

	// At startup.
	toNull()
	c := e.collector(db)
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		t.Fatal(err)
	}
	refused(c)
	if got := e.mustRead(db); got != (logical{}) {
		t.Fatalf("nothing may be delivered: %+v", got)
	}
	destID := c.Destination().ID
	if cp, _ := e.store.Checkpoint(ctx, destID); cp != (spool.Checkpoint{}) {
		t.Fatalf("no checkpoint may be recorded: %+v", cp)
	}

	// Replaced by an operator and migrated: the spooled rows are delivered.
	if err := admin.Exec(ctx, "DROP TABLE "+table); err != nil {
		t.Fatal(err)
	}
	e.migrate(db)
	e.syncAndDrain(c)
	delivered := e.mustRead(db)
	if delivered.Msgs != 2 || delivered.Convs != 1 {
		t.Fatalf("after the migration: %+v", delivered)
	}
	local, held := e.checkpoints(db, destID)
	if local.Confirmed == 0 || held != local.Confirmed {
		t.Fatalf("checkpoints after the migration: %+v, held %d", local, held)
	}

	// While running: the next destination check refuses delivery.
	toNull()
	c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	e.moreHistory(1)
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		t.Fatal(err)
	}
	refused(c)
	if got := e.mustRead(db); got != delivered {
		t.Fatalf("nothing more may be delivered: %+v, was %+v", got, delivered)
	}
	if cp, _ := e.store.Checkpoint(ctx, destID); cp != local {
		t.Fatalf("the checkpoint must not advance: %+v, was %+v", cp, local)
	}
}
