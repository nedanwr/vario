package collector

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// D1: ClickHouse acknowledges a batch, the delivery checkpoint after it is
// not confirmed, and the batch stays queued. The database is then restored
// to an earlier state (partitions replaced from a snapshot) that lacks the
// batch but still holds the confirmed checkpoint, and that keeps the
// deduplication memory, as REPLACE PARTITION, DROP PARTITION, ALTER ...
// DELETE and RESTORE into existing tables do. A retry under the batch's
// original insert_deduplication_token would be acknowledged without writing
// a row, the next checkpoint would succeed, and the queue would be cleared
// while the records are missing. Every retry must therefore carry a new token.

// d1Env is a collector that delivered the standard history (checkpoint
// confirmed), a snapshot of that state, and one more conversation turn
// (a message and a provider turn with token usage) queued, not delivered.
type d1Env struct {
	*harness
	c        *Collector
	snapshot sinkBackup
	base     spool.Checkpoint
	queued   int64
	want     results
}

func newD1Env(t *testing.T) *d1Env {
	t.Helper()
	h := setup(t)
	c, _ := h.logged(t)
	h.syncDrain(t, c)
	e := &d1Env{harness: h, c: c, snapshot: h.sink.backup(), base: h.checkpoint(t)}
	if e.base.Confirmed == 0 || h.held(t) != e.base.Confirmed {
		t.Fatalf("setup: checkpoint %+v, held %d", e.base, h.held(t))
	}
	h.fix.Message("msg-d1", "thread-1", "run-1", "assistant", "inserted, checkpoint not confirmed", false)
	h.fix.ProviderTurn("turn-d1", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 1000, "outputTokens": 10}, nil)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if e.queued = pending(t, h); e.queued == 0 {
		t.Fatal("expected queued rows")
	}
	e.want = h.reference(t)
	return e
}

// tokens returns the tokens of every insert that reached the database.
func (e *d1Env) tokens() map[string]bool {
	e.sink.mu.Lock()
	defer e.sink.mu.Unlock()
	m := map[string]bool{}
	for _, in := range e.sink.inserts {
		m[in.token] = true
	}
	return m
}

func (e *d1Env) hasD1Rows() bool {
	_, msg := e.sink.latest(model.TableMessages, "message_id")["msg-d1"]
	_, turn := e.sink.latest(model.TableProviderTurns, "provider_turn_id")["turn-d1"]
	return msg && turn
}

// restorePartitions brings the data and checkpoint tables back to the
// snapshot, keeping every remembered token.
func (e *d1Env) restorePartitions(t *testing.T) {
	t.Helper()
	e.sink.loseRowsKeepTokens(&e.snapshot)
	if e.hasD1Rows() {
		t.Fatal("the restore must remove the uncheckpointed rows")
	}
	if e.held(t) != e.base.Confirmed {
		t.Fatalf("the restore must keep the confirmed checkpoint %d, holds %d", e.base.Confirmed, e.held(t))
	}
}

// requireInsertedNotCheckpointed checks the state after a round whose
// inserts were acknowledged and whose checkpoint was not confirmed.
func (e *d1Env) requireInsertedNotCheckpointed(t *testing.T) {
	t.Helper()
	if !e.hasD1Rows() {
		t.Fatal("the rows must have been inserted")
	}
	if got := pending(t, e.harness); got != e.queued {
		t.Fatalf("rows not covered by a confirmed checkpoint must stay queued: %d of %d", got, e.queued)
	}
	if cp := e.checkpoint(t); cp != e.base {
		t.Fatalf("no checkpoint may be confirmed: %+v, was %+v", cp, e.base)
	}
}

// requireOldTokensSwallowed requires the database to still remember the
// tokens of the first attempt, so that a retry under them would be
// acknowledged without writing: the precondition of D1.
func (e *d1Env) requireOldTokensSwallowed(t *testing.T, old map[string]bool) {
	t.Helper()
	e.sink.mu.Lock()
	defer e.sink.mu.Unlock()
	for tok := range old {
		if !e.sink.tokens[tok] {
			t.Fatalf("the database must still remember token %s", tok)
		}
	}
}

// recover delivers the queue and requires the database to hold the records
// again, counted once, under a new confirmed checkpoint, with no retry
// deduplicated against an earlier attempt.
func (e *d1Env) recover(t *testing.T, c *Collector, old map[string]bool) {
	t.Helper()
	e.sink.mu.Lock()
	deduped, firstNew := e.sink.deduped, len(e.sink.inserts)
	e.sink.mu.Unlock()
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 || int64(dr.Delivered) != e.queued {
		t.Fatalf("recovery: %+v %v", dr, err)
	}
	if !e.hasD1Rows() {
		t.Fatal("D1: the queue was cleared, but the records are missing from the database")
	}
	if got := e.sink.logical(); got != e.want {
		t.Fatalf("after the recovery: %+v, want %+v", got, e.want)
	}
	e.sink.mu.Lock()
	retries := e.sink.inserts[firstNew:]
	swallowed := e.sink.deduped - deduped
	e.sink.mu.Unlock()
	if len(retries) == 0 || swallowed != 0 {
		t.Fatalf("%d retries, %d acknowledged without writing", len(retries), swallowed)
	}
	for _, in := range retries {
		if old[in.token] {
			t.Fatalf("%s was retried under a token an earlier attempt used", in.table)
		}
	}
	cp := e.checkpoint(t)
	if cp.Confirmed <= e.base.Confirmed || e.held(t) != cp.Confirmed || cp.Writer != e.base.Writer {
		t.Fatalf("the recovery must be covered by a new checkpoint: %+v, held %d, was %+v", cp, e.held(t), e.base)
	}
	if pending(t, e.harness) != 0 {
		t.Fatal("the queue must be empty")
	}
	if _, ok, _ := e.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported: the database holds the confirmed checkpoint")
	}
}

// restart opens the spool again and prepares a new collector on it, as a new
// process does.
func (e *d1Env) restart(t *testing.T) *Collector {
	t.Helper()
	store, err := spool.Open(e.store.Path(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c := New(store, e.sink, []sources.Source{e.src}, e.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	e.store = store
	return c
}

// The exact D1 sequence, with the checkpoint write failing outright, retried
// by the same process and by a restarted one.
func TestD1CheckpointFailureThenPartitionRestore(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same process"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			e := newD1Env(t)
			// 1-3. ClickHouse accepts the batch, the checkpoint write fails, the rows stay queued.
			e.sink.cpWriteErr = &clickhouse.Error{Status: 503, Message: "unavailable", Transient: true}
			if _, err := e.c.deliverRound(ctx, time.Now()); err == nil || isFatal(err) {
				t.Fatalf("the round must fail without stopping delivery: %v", err)
			}
			e.sink.cpWriteErr = nil
			e.requireInsertedNotCheckpointed(t)
			old := e.tokens()
			// 4. The partitions are restored: the rows are gone, the checkpoint stays.
			e.restorePartitions(t)
			e.requireOldTokensSwallowed(t, old)
			// 5-7. The retry stores the rows again before the queue is cleared.
			c := e.c
			if restart {
				c = e.restart(t)
			}
			e.recover(t, c, old)
		})
	}
}

// The checkpoint reached the database but its acknowledgement was lost. The
// restore takes the database back below it (to the confirmed checkpoint).
func TestD1AmbiguousCheckpointThenPartitionRestore(t *testing.T) {
	e := newD1Env(t)
	e.sink.cpAmbiguous = 1
	if _, err := e.c.deliverRound(ctx, time.Now()); err == nil {
		t.Fatal("the round must fail")
	}
	e.requireInsertedNotCheckpointed(t)
	if e.held(t) != e.base.Confirmed+1 {
		t.Fatalf("the unconfirmed checkpoint reached the database: holds %d", e.held(t))
	}
	old := e.tokens()
	e.restorePartitions(t)
	e.recover(t, e.c, old)
}

// The checkpoint reached the database, its acknowledgement was lost, and
// the database is not restored: the retry stores the rows a second time
// (same keys and versions) under a checkpoint above the unconfirmed one.
func TestD1AmbiguousCheckpointWithoutRestore(t *testing.T) {
	e := newD1Env(t)
	e.sink.cpAmbiguous = 1
	if _, err := e.c.deliverRound(ctx, time.Now()); err == nil {
		t.Fatal("the round must fail")
	}
	old := e.tokens()
	e.recover(t, e.c, old)
	if cp := e.checkpoint(t); cp.Confirmed < e.base.Confirmed+2 {
		t.Fatalf("checkpoint %d must be above the unconfirmed one", cp.Confirmed)
	}
}

// The insert reached the database but its acknowledgement was lost: its
// outcome is unknown, it is not covered by a checkpoint, and its retry must
// not be swallowed either.
func TestD1AmbiguousInsertThenPartitionRestore(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same process"
		if restart {
			name = "after restart"
		}
		t.Run(name, func(t *testing.T) {
			e := newD1Env(t)
			e.sink.ambiguousData = 1000 // every insert of the round
			if _, err := e.c.deliverRound(ctx, time.Now()); err == nil {
				t.Fatal("the round must fail")
			}
			e.sink.ambiguousData = 0
			e.requireInsertedNotCheckpointed(t)
			if e.writes() != 0 && e.held(t) != e.base.Confirmed {
				t.Fatal("an insert with an unknown outcome must not be covered by a checkpoint")
			}
			old := e.tokens()
			e.restorePartitions(t)
			c := e.c
			if restart {
				c = e.restart(t)
			}
			e.recover(t, c, old)
		})
	}
}

// The process dies right after ClickHouse acknowledged the inserts, before
// anything about them reached the spool (no checkpoint attempt, no failure
// recorded). The restarted collector must not reuse their tokens.
func TestD1CrashAfterInsertBeforeCheckpoint(t *testing.T) {
	e := newD1Env(t)
	entries, err := e.store.Due(ctx, time.Now(), 1<<30)
	if err != nil || len(entries) == 0 {
		t.Fatalf("due: %d %v", len(entries), err)
	}
	order, groups := byTable(entries)
	for _, table := range order {
		if _, err := e.c.insertGroup(ctx, table, groups[table]); err != nil {
			t.Fatal(err)
		}
	}
	// Killed here: Fail and confirmDelivered never run.
	e.requireInsertedNotCheckpointed(t)
	old := e.tokens()
	e.restorePartitions(t)
	e.recover(t, e.restart(t), old)
}

// The process dies after the checkpoint was written but before the spool
// recorded it: the database holds a checkpoint above the confirmed one. A
// restore back to the confirmed checkpoint must still be recovered.
func TestD1CrashAfterCheckpointBeforeAck(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "no restore"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			e := newD1Env(t)
			entries, err := e.store.Due(ctx, time.Now(), 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			order, groups := byTable(entries)
			for _, table := range order {
				if _, err := e.c.insertGroup(ctx, table, groups[table]); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.sink.WriteCheckpoint(ctx, e.base.Writer, e.base.Confirmed+1); err != nil {
				t.Fatal(err)
			}
			// Killed here: AckCheckpoint never ran.
			e.requireInsertedNotCheckpointed(t)
			old := e.tokens()
			if restore {
				e.restorePartitions(t)
			}
			e.recover(t, e.restart(t), old)
		})
	}
}

// The failure repeats: several rounds whose checkpoints fail, a restore
// between each, and a restart. Every retry carries a token no earlier
// attempt used, and the records are there at the end.
func TestD1RepeatedCheckpointFailures(t *testing.T) {
	e := newD1Env(t)
	c := e.c
	for i := 0; i < 3; i++ {
		e.sink.cpWriteErr = errors.New("checkpoint lost")
		before := e.tokens()
		e.sink.mu.Lock()
		deduped := e.sink.deduped
		e.sink.mu.Unlock()
		if _, err := c.Drain(ctx); err == nil {
			t.Fatalf("round %d must fail", i)
		}
		e.sink.mu.Lock()
		if e.sink.deduped != deduped {
			e.sink.mu.Unlock()
			t.Fatalf("round %d: a retry was acknowledged without writing", i)
		}
		e.sink.mu.Unlock()
		if len(e.tokens()) == len(before) && i > 0 {
			t.Fatalf("round %d: no new token was used", i)
		}
		e.sink.cpWriteErr = nil
		e.requireInsertedNotCheckpointed(t)
		e.restorePartitions(t)
		if i == 1 {
			c = e.restart(t)
		}
	}
	e.recover(t, c, e.tokens())
}

// Rows kept for a destination other than the active one, retried with
// `vario destination pending --retry`: the same rule applies.
func TestD1RetainedDeliveryCheckpointFailureThenRestore(t *testing.T) {
	h, old, repl := keptAfterReplacement(t)
	h.setServerID("id-A")
	snapshot := h.sink.backup()
	h.sink.cpWriteErr = errors.New("checkpoint lost")
	if res, err := h.unprepared(h.opts).DeliverRetained(ctx, old); err == nil || res.Delivered != 0 || res.Remaining != old.Pending {
		t.Fatalf("a failed checkpoint must keep the rows: %+v %v", res, err)
	}
	h.sink.cpWriteErr = nil
	if _, ok := h.sink.latest(model.TableMessages, "message_id")["msg-buffered"]; !ok {
		t.Fatal("the rows must have been inserted")
	}
	h.sink.loseRowsKeepTokens(&snapshot)
	if _, ok := h.sink.latest(model.TableMessages, "message_id")["msg-buffered"]; ok {
		t.Fatal("the restore must remove the rows")
	}
	h.sink.mu.Lock()
	deduped := h.sink.deduped
	h.sink.mu.Unlock()
	res, err := h.unprepared(h.opts).DeliverRetained(ctx, old)
	if err != nil || res.Remaining != 0 || int64(res.Delivered) != old.Pending {
		t.Fatalf("retry: %+v %v", res, err)
	}
	h.sink.mu.Lock()
	swallowed := h.sink.deduped - deduped
	h.sink.mu.Unlock()
	if swallowed != 0 {
		t.Fatalf("%d retries were acknowledged without writing", swallowed)
	}
	if _, ok := h.sink.latest(model.TableMessages, "message_id")["msg-buffered"]; !ok {
		t.Fatal("D1: the kept rows were acknowledged but are missing")
	}
	if h.store.Selected() != repl.ID {
		t.Fatal("the selected destination changed")
	}
}
