package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// ClickHouse remembers the insert_deduplication_token of recent inserts per
// table partition (non_replicated_deduplication_window). The memory survives
// ALTER ... DELETE, REPLACE PARTITION and RESTORE into existing tables, so a
// historical replay that re-sends the same batches under the same tokens is
// acknowledged without writing a row. These tests take rows away in those
// ways and require that the explicit replay (`vario backfill --force`, or
// the reconciliation after `vario destination switch` accepted a rollback)
// brings every record back, counted once.

// deleteRows deletes every row of the data tables with a mutation, which
// keeps the tables' deduplication memory.
func (e *recoveryEnv) deleteRows(db string) {
	e.t.Helper()
	for _, table := range model.Tables {
		q := fmt.Sprintf("ALTER TABLE `%s`.`%s` DELETE WHERE 1 SETTINGS mutations_sync = 2", db, table)
		if err := e.clients[db].Exec(ctx, q); err != nil {
			e.t.Fatalf("%s: %v", q, err)
		}
	}
}

// partitions lists the active partitions of db.table.
func (e *recoveryEnv) partitions(client *clickhouse.Client, db, table string) []string {
	e.t.Helper()
	var rows []struct {
		P string `json:"p"`
	}
	q := fmt.Sprintf("SELECT DISTINCT partition_id AS p FROM system.parts WHERE database = '%s' AND table = '%s' AND active ORDER BY p", db, table)
	if err := client.Query(ctx, q, &rows); err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.P
	}
	return out
}

// restorePartitions brings the data tables and the checkpoint table of db
// back to the state held by the same tables in from, partition by partition:
// REPLACE PARTITION where from has the partition, DROP PARTITION where it
// has not. vario_meta is not touched, so the database keeps its id.
func (e *recoveryEnv) restorePartitions(db, from string) (replaced, dropped int) {
	e.t.Helper()
	client := e.clients[db]
	for _, table := range append(slices.Clone(model.Tables), clickhouse.CheckpointTable) {
		have := e.partitions(client, from, table)
		for _, p := range e.partitions(client, db, table) {
			var q string
			if slices.Contains(have, p) {
				q = fmt.Sprintf("ALTER TABLE `%s`.`%s` REPLACE PARTITION ID '%s' FROM `%s`.`%s`", db, table, p, from, table)
				replaced++
			} else {
				q = fmt.Sprintf("ALTER TABLE `%s`.`%s` DROP PARTITION ID '%s'", db, table, p)
				dropped++
			}
			if err := client.Exec(ctx, q); err != nil {
				e.t.Fatalf("%s: %v", q, err)
			}
		}
		for _, p := range have {
			if !slices.Contains(e.partitions(client, db, table), p) {
				q := fmt.Sprintf("ALTER TABLE `%s`.`%s` REPLACE PARTITION ID '%s' FROM `%s`.`%s`", db, table, p, from, table)
				if err := client.Exec(ctx, q); err != nil {
					e.t.Fatalf("%s: %v", q, err)
				}
				replaced++
			}
		}
	}
	return replaced, dropped
}

// copyTables copies the data tables and the checkpoint table of db, rows
// included, into a new database.
func (e *recoveryEnv) copyTables(db, to string) {
	e.t.Helper()
	client := e.clients[db]
	stmts := []string{"CREATE DATABASE `" + to + "`"}
	for _, table := range append(slices.Clone(model.Tables), clickhouse.CheckpointTable) {
		stmts = append(stmts,
			fmt.Sprintf("CREATE TABLE `%s`.`%s` AS `%s`.`%s`", to, table, db, table),
			fmt.Sprintf("INSERT INTO `%s`.`%s` SELECT * FROM `%s`.`%s`", to, table, db, table))
	}
	e.t.Cleanup(func() { _ = client.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+to+"` SYNC") })
	for _, q := range stmts {
		if err := client.Exec(ctx, q); err != nil {
			e.t.Fatalf("%s: %v", q, err)
		}
	}
}

// queueAndDeliverPartly queues the source's complete history (the initial
// backfill) and delivers only the first rounds of it.
func (e *recoveryEnv) queueAndDeliverPartly(c *Collector, rounds int) {
	e.t.Helper()
	if err := c.syncOnce(ctx, e.src, c.log); err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i < rounds; i++ {
		if n, err := c.deliverRound(ctx, time.Now()); err != nil || n == 0 {
			e.t.Fatalf("round %d: %d rows, %v", i, n, err)
		}
	}
}

func (e *recoveryEnv) drain(c *Collector) {
	e.t.Helper()
	for i := 0; i < 300; i++ {
		dr, err := c.Drain(ctx)
		var de *DestinationError
		if errors.As(err, &de) {
			e.t.Fatal(err)
		}
		if err == nil && dr.Remaining == 0 {
			return
		}
	}
	e.t.Fatal("spool did not drain")
}

// forceBackfill is `vario backfill --force`: a forced snapshot with the
// complete event history, then delivery of everything queued.
func (e *recoveryEnv) forceBackfill(c *Collector) {
	e.t.Helper()
	if _, err := c.Snapshot(ctx, e.src, true, true); err != nil {
		e.t.Fatal(err)
	}
	e.drain(c)
}

// physical counts the rows ClickHouse stores in a table, duplicates included.
func (e *recoveryEnv) physical(db, table string) int {
	e.t.Helper()
	var out []struct {
		N int `json:"n,string"`
	}
	if err := e.clients[db].Query(ctx, fmt.Sprintf("SELECT toString(count()) AS n FROM `%s`.`%s`", db, table), &out); err != nil {
		e.t.Fatal(err)
	}
	return out[0].N
}

func (e *recoveryEnv) acceptRolledBack(c *Collector, destID int64) *Collector {
	e.t.Helper()
	due(c)
	if _, err := c.Drain(ctx); err == nil {
		e.t.Fatal("delivery must stop at the restored database")
	} else if _, ok := isRollback(err); !ok {
		e.t.Fatalf("delivery must stop with a rollback error: %v", err)
	}
	// `vario destination switch`.
	if err := e.store.AcceptRollback(ctx, destID); err != nil {
		e.t.Fatal(err)
	}
	return e.prepared(c.opts.Database)
}

// `vario backfill --force` after rows were deleted: ClickHouse still
// remembers the tokens of the batches that delivered them.
func TestIntegrationForcedBackfillRestoresDeletedRows(t *testing.T) {
	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_replay_del_%d", time.Now().UnixNano())
	e.migrate(db)
	e.moreHistory(1)
	c := e.prepared(db)
	e.syncAndDrain(c)
	want := e.mustRead(db)
	if want.Msgs != 3 || want.Turns != 2 || want.Input != 818885+1000 || want.Raw == 0 {
		t.Fatalf("initial delivery: %+v", want)
	}

	for round := 1; round <= 2; round++ {
		e.deleteRows(db)
		if got := e.mustRead(db); got.Msgs != 0 || got.Turns != 0 || got.Raw != 0 {
			t.Fatalf("round %d: rows were not deleted: %+v", round, got)
		}
		e.forceBackfill(c)
		if got := e.mustRead(db); got != want {
			t.Fatalf("round %d: the forced backfill must restore every deleted record once:\n got %+v\nwant %+v", round, got, want)
		}
	}
	// Repeated without a loss, it changes no result.
	e.forceBackfill(c)
	if got := e.mustRead(db); got != want {
		t.Fatalf("a repeated forced backfill changed results: %+v, want %+v", got, want)
	}
	// Normal operation continues incrementally, and a plain backfill sends nothing.
	e.moreHistory(2)
	e.syncAndDrain(c)
	after := e.mustRead(db)
	if after.Msgs != want.Msgs+1 || after.Turns != want.Turns+1 || after.Input != want.Input+1000 {
		t.Fatalf("after the replays: %+v, was %+v", after, want)
	}
	// A plain backfill skips every delivered record (raw events are never
	// fingerprinted and are sent again) and changes no result.
	res, err := c.Snapshot(ctx, e.src, false, true)
	if err != nil || res.Skipped == 0 {
		t.Fatalf("a plain backfill: %+v %v", res, err)
	}
	e.drain(c)
	if got := e.mustRead(db); got != after {
		t.Fatalf("a plain backfill changed results: %+v, want %+v", got, after)
	}
}

// A database whose partitions were restored from an older copy (REPLACE
// PARTITION where the copy has the partition, DROP PARTITION where it has
// not) together with its checkpoint table: a rollback. The copy was taken
// during the initial backfill, so the reconciliation re-sends exactly the
// batches that were lost, which ClickHouse remembers.
func TestIntegrationRollbackByPartitionReplaceIsRecovered(t *testing.T) {
	e := newRecoveryEnv(t)
	suffix := time.Now().UnixNano()
	db, snap := fmt.Sprintf("vario_replay_part_%d", suffix), fmt.Sprintf("vario_replay_part_copy_%d", suffix)
	e.migrate(db)
	c := e.prepared(db)
	destID := c.Destination().ID
	e.queueAndDeliverPartly(c, 3)
	e.copyTables(db, snap)
	e.drain(c)
	want := e.mustRead(db)
	if want != e.complete(fmt.Sprintf("vario_replay_part_ref_%d", suffix)) {
		t.Fatalf("initial delivery is incomplete: %+v", want)
	}

	replaced, dropped := e.restorePartitions(db, snap)
	if replaced == 0 || dropped == 0 {
		t.Fatalf("the test must replace and drop partitions: %d replaced, %d dropped", replaced, dropped)
	}
	lost := e.mustRead(db)
	if lost == want {
		t.Fatal("restoring the partitions must lose rows")
	}
	c = e.acceptRolledBack(c, destID)
	e.syncAndDrain(c)
	if got := e.mustRead(db); got != want {
		t.Fatalf("after the reconciliation the database must hold the complete history once:\n got %+v\nwant %+v (had %+v)", got, want, lost)
	}
	// The same partitions once more: the new checkpoint history detects it again.
	e.restorePartitions(db, snap)
	c = e.acceptRolledBack(c, destID)
	e.syncAndDrain(c)
	if got := e.mustRead(db); got != want {
		t.Fatalf("second restore: %+v, want %+v", got, want)
	}
}

// A real backup, taken during the initial backfill, restored partition by
// partition (RESTORE DATABASE ... AS, then REPLACE / DROP PARTITION), and
// restored into the existing tables (RESTORE ... allow_non_empty_tables
// after the rows were deleted): both keep the deduplication memory.
func TestIntegrationRollbackByBackupIntoExistingTablesIsRecovered(t *testing.T) {
	for _, how := range []string{"partitions", "in-place"} {
		t.Run(how, func(t *testing.T) {
			e := newRecoveryEnv(t)
			suffix := time.Now().UnixNano()
			db := fmt.Sprintf("vario_replay_bk_%d", suffix)
			e.migrate(db)
			c := e.prepared(db)
			destID := c.Destination().ID
			e.queueAndDeliverPartly(c, 3)
			name := e.backup(db)
			e.drain(c)
			want := e.mustRead(db)

			client := e.clients[db]
			switch how {
			case "partitions":
				restored := db + "_restored"
				t.Cleanup(func() { _ = client.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+restored+"` SYNC") })
				if err := client.Exec(ctx, fmt.Sprintf("RESTORE DATABASE `%s` AS `%s` FROM File('%s')", db, restored, name)); err != nil {
					t.Fatal(err)
				}
				e.restorePartitions(db, restored)
			case "in-place":
				e.deleteRows(db)
				if err := client.Exec(ctx, fmt.Sprintf("ALTER TABLE `%s`.`%s` DELETE WHERE 1 SETTINGS mutations_sync = 2", db, clickhouse.CheckpointTable)); err != nil {
					t.Fatal(err)
				}
				tables := ""
				for i, table := range append(slices.Clone(model.Tables), clickhouse.CheckpointTable) {
					if i > 0 {
						tables += ", "
					}
					tables += fmt.Sprintf("TABLE `%s`.`%s`", db, table)
				}
				if err := client.Exec(ctx, fmt.Sprintf("RESTORE %s FROM File('%s') SETTINGS allow_non_empty_tables = true", tables, name)); err != nil {
					t.Fatal(err)
				}
			}
			lost := e.mustRead(db)
			if lost == want {
				t.Fatal("restoring the backup must lose rows")
			}
			c = e.acceptRolledBack(c, destID)
			e.syncAndDrain(c)
			if got := e.mustRead(db); got != want {
				t.Fatalf("after the reconciliation the database must hold the complete history once:\n got %+v\nwant %+v (had %+v)", got, want, lost)
			}
		})
	}
}

// tokenSink records the token of every insert and, while lossy, loses the
// response of every insert after ClickHouse committed it.
type tokenSink struct {
	*clickhouse.Client
	mu       sync.Mutex
	attempts map[string]int
	lossy    bool
	lost     int
}

func (s *tokenSink) Insert(ctx context.Context, table string, body []byte, token string) error {
	if err := s.Client.Insert(ctx, table, body, token); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[token]++
	if s.lossy {
		s.lost++
		return &clickhouse.Error{Message: "read: connection reset by peer", Transient: true}
	}
	return nil
}

// A forced backfill through a connection that loses every response,
// interrupted and continued by a restarted collector on the reopened spool.
// An insert whose response was lost may have stored its rows, so every
// retry carries a new token, stored in the spool before it is sent; no
// insert, before or after the restart, is sent under a token ClickHouse
// already holds. The replay restores
// everything; rows of lost responses may be stored twice with the same keys
// and versions, which the *_latest views count once.
func TestIntegrationInterruptedReplayWithLostResponses(t *testing.T) {
	e := newRecoveryEnv(t)
	db := fmt.Sprintf("vario_replay_amb_%d", time.Now().UnixNano())
	e.migrate(db)
	e.moreHistory(1)
	c := e.prepared(db)
	e.syncAndDrain(c)
	want := e.mustRead(db)
	e.deleteRows(db)

	sink := &tokenSink{Client: e.clients[db], attempts: map[string]int{}, lossy: true}
	lossy := New(e.store, sink, []sources.Source{e.src}, c.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := lossy.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := lossy.Snapshot(ctx, e.src, true, true); err != nil {
		t.Fatal(err)
	}
	queued := func(s *spool.Store) map[int64]string {
		t.Helper()
		es, err := s.Due(ctx, time.Now().Add(time.Hour), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]string{}
		for _, en := range es {
			m[en.ID] = en.Token
		}
		return m
	}
	before := queued(e.store)
	for i := 0; i < 6; i++ { // interrupted after a few rounds
		_, _ = lossy.deliverRound(ctx, time.Now().Add(time.Hour))
	}
	if sink.lost < 2 {
		t.Fatalf("the test must lose responses: %d", sink.lost)
	}
	sink.mu.Lock()
	sink.lossy = false
	sink.mu.Unlock()

	// Restart: a new process opens the spool and delivers the rest.
	store, err := spool.Open(e.store.Path(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SelectDestination(e.store.Selected())
	renewed := 0
	for id, tok := range queued(store) {
		if before[id] != tok {
			renewed++ // stored before its attempt was sent
		}
	}
	if renewed == 0 {
		t.Fatal("no entry was retried under a new token before the restart")
	}
	restarted := New(store, sink, []sources.Source{e.src}, c.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := restarted.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		dr, err := restarted.Drain(ctx)
		if err == nil && dr.Remaining == 0 {
			break
		}
		if i == 300 {
			t.Fatalf("spool did not drain: %+v %v", dr, err)
		}
	}
	if got := e.mustRead(db); got != want {
		t.Fatalf("the interrupted replay must restore everything once:\n got %+v\nwant %+v", got, want)
	}
	// No token was sent twice: no retry could be swallowed by ClickHouse.
	for tok, n := range sink.attempts {
		if n > 1 {
			t.Fatalf("token %s was sent %d times", tok, n)
		}
	}
	// At most the rows of the lost responses are stored twice.
	if n := e.physical(db, model.TableMessages); n < want.Msgs || n > 2*want.Msgs {
		t.Fatalf("%d message rows for %d records", n, want.Msgs)
	}
	if n := e.physical(db, model.TableProviderTurns); n < want.Turns || n > 2*want.Turns {
		t.Fatalf("%d provider turn rows for %d records", n, want.Turns)
	}
}

// The reconciliation after an accepted rollback is interrupted halfway and
// continued by a restarted collector: the part queued before the restart
// keeps its tokens, the rest is queued under the same replay identity, and
// the database ends up complete.
func TestIntegrationInterruptedReconciliationAfterRestart(t *testing.T) {
	e := newRecoveryEnv(t)
	suffix := time.Now().UnixNano()
	db, snap := fmt.Sprintf("vario_replay_rs_%d", suffix), fmt.Sprintf("vario_replay_rs_copy_%d", suffix)
	e.migrate(db)
	c := e.prepared(db)
	destID := c.Destination().ID
	e.queueAndDeliverPartly(c, 2)
	e.copyTables(db, snap)
	e.drain(c)
	want := e.mustRead(db)
	e.restorePartitions(db, snap)
	c = e.acceptRolledBack(c, destID)

	e.queueAndDeliverPartly(c, 2) // the reconciliation, interrupted
	store, err := spool.Open(e.store.Path(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := New(store, e.clients[db], []sources.Source{e.src}, c.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := restarted.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.syncOnce(ctx, e.src, restarted.log); err != nil {
		t.Fatal(err)
	}
	e.drain(restarted)
	if got := e.mustRead(db); got != want {
		t.Fatalf("the interrupted reconciliation must complete after the restart:\n got %+v\nwant %+v", got, want)
	}
}
