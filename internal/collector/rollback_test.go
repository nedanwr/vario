package collector

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

func init() { rollbackRereadDelay = time.Millisecond }

// results is what the *_latest and usage views show: one row per record (the
// highest version) and token counts summed over provider turns.
type results struct {
	Messages, Conversations, Runs, Turns, Events, Raw int
	Input, Output                                     uint64
}

func (s *fakeSink) logical() results {
	turns := s.latest(model.TableProviderTurns, "provider_turn_id")
	l := results{
		Messages:      len(s.latest(model.TableMessages, "message_id")),
		Conversations: len(s.latest(model.TableConversations, "conversation_id")),
		Runs:          len(s.latest(model.TableRuns, "run_id")),
		Turns:         len(turns),
		Events:        len(s.latest(model.TableExecutionEvents, "event_id")),
		Raw:           len(s.latest(model.TableRawEvents, "event_id")),
	}
	for _, r := range turns {
		if v, ok := r["input_tokens"].(float64); ok {
			l.Input += uint64(v)
		}
		if v, ok := r["output_tokens"].(float64); ok {
			l.Output += uint64(v)
		}
	}
	return l
}

// reference delivers the source's complete history into an empty database
// with a new spool: what a database that lost nothing holds.
func (h *harness) reference(t *testing.T) results {
	t.Helper()
	store, err := spool.Open(filepath.Join(t.TempDir(), "reference.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sink := newSink()
	c := New(store, sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("reference drain: %+v %v", dr, err)
	}
	return sink.logical()
}

func (h *harness) checkpoint(t *testing.T) spool.Checkpoint {
	t.Helper()
	d, ok, err := h.store.Lookup(ctx, h.opts.Endpoint, h.opts.Database)
	if err != nil || !ok {
		t.Fatalf("configured destination: %v %v", ok, err)
	}
	cp, err := h.store.Checkpoint(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// held is the checkpoint the database holds for the spool's writer.
func (h *harness) held(t *testing.T) uint64 {
	t.Helper()
	cp := h.checkpoint(t)
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	return h.sink.checkpoints[cp.Writer]
}

func (h *harness) writes() int {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	return h.sink.cpWrites
}

func (h *harness) syncDrain(t *testing.T, c *Collector) {
	t.Helper()
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("drain: %+v %v", dr, err)
	}
}

// due makes the next delivery round repeat the destination check.
func due(c *Collector) { c.checkedAt = time.Now().Add(-2 * identityCheckInterval) }

func isRollback(err error) (*DestinationError, bool) {
	var de *DestinationError
	return de, errors.As(err, &de) && de.RolledBack
}

// rolledBackWithBufferedRows delivers the standard history to a database
// with destination id id-A, takes a backup, delivers a second turn and a
// message (which the backup lacks), queues one more message that is not
// delivered yet, and restores the backup. The database keeps its id.
func rolledBackWithBufferedRows(t *testing.T) (h *harness, c *Collector, logs *syncBuffer, backup sinkBackup, queued int64) {
	t.Helper()
	h = setup(t)
	h.setServerID("id-A")
	c, logs = h.logged(t)
	h.syncDrain(t, c)
	backup = h.sink.backup()

	h.fix.Message("msg-after-backup", "thread-1", "run-1", "assistant", "delivered after the backup", false)
	h.fix.ProviderTurn("turn-2", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 1000, "outputTokens": 10}, nil)
	h.syncDrain(t, c)
	if got := h.sink.logical(); got.Messages != 3 || got.Turns != 2 || got.Input != 818885+1000 {
		t.Fatalf("before the restore: %+v", got)
	}

	h.fix.Message("msg-buffered", "thread-1", "run-1", "assistant", "collected, not delivered yet", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if queued = pending(t, h); queued == 0 {
		t.Fatal("expected buffered rows")
	}
	h.sink.restore(backup)
	return h, c, logs, backup, queued
}

// P1: backup, further deliveries, restore of the backup. The database keeps
// its destination id, so only the delivery checkpoint shows that rows are
// missing. Delivery stops, nothing queued is lost or sent, the rollback is
// reported, a restart is refused too, and only the explicit acceptance leads
// to the complete reconciliation, after which the database holds the whole
// history exactly once. Doing all of it again changes nothing.
func TestRollbackAfterBackupIsDetectedAndRecovered(t *testing.T) {
	h, c, logs, backup, queued := rolledBackWithBufferedRows(t)
	want := h.reference(t)
	if want.Messages != 4 || want.Turns != 2 || want.Input != 818885+1000 {
		t.Fatalf("reference: %+v", want)
	}
	destID := c.Destination().ID

	for round := 1; round <= 2; round++ {
		restored := h.sink.logical()
		if restored.Messages >= want.Messages || restored.Turns >= want.Turns {
			t.Fatalf("round %d: the restored database must lack rows: %+v", round, restored)
		}
		acknowledged, held, writes := h.checkpoint(t), h.held(t), h.writes()
		if held >= acknowledged.Confirmed {
			t.Fatalf("round %d: the restored database must hold a lower checkpoint: %d, acknowledged %d", round, held, acknowledged.Confirmed)
		}

		due(c)
		_, err := c.deliverRound(ctx, time.Now())
		de, ok := isRollback(err)
		if !ok || !isFatal(err) || de.Replaced || de.Checkpoint != acknowledged.Confirmed || de.FoundCheckpoint != held || de.Configured.ServerID != "id-A" || de.Configured.Pending != queued {
			t.Fatalf("round %d: delivery must stop with a rollback error: %v (%+v)", round, err, de)
		}
		for _, text := range []string{"restored to an earlier state", "destination id id-A", "Delivery is suspended", "vario destination switch",
			"reads every source again in full", "complete history", "count each record once"} {
			if !strings.Contains(err.Error(), text) {
				t.Fatalf("the error must explain the recovery (missing %q):\n%s", text, err)
			}
		}
		// Suspended, not retried: later rounds and a forced drain deliver nothing,
		// and no checkpoint is written that would hide the rollback.
		for i := 0; i < 3; i++ {
			if _, err := c.deliverRound(ctx, time.Now().Add(time.Hour)); !isFatal(err) {
				t.Fatalf("round %d: delivery must stay suspended: %v", round, err)
			}
		}
		if _, err := c.Drain(ctx); err == nil {
			t.Fatal("drain must stay suspended")
		}
		if got := h.sink.logical(); got != restored {
			t.Fatalf("round %d: rows were delivered to the rolled-back database: %+v, had %+v", round, got, restored)
		}
		if h.writes() != writes || h.held(t) != held {
			t.Fatalf("round %d: a checkpoint was written into the rolled-back database", round)
		}
		if got := pending(t, h); got != queued {
			t.Fatalf("round %d: queued rows must be kept: %d of %d", round, got, queued)
		}
		if cp := h.checkpoint(t); cp != acknowledged {
			t.Fatalf("round %d: the acknowledged checkpoint must be kept: %+v, was %+v", round, cp, acknowledged)
		}
		// Reported for logs, status and doctor. It is not a replacement.
		if l := logs.String(); !strings.Contains(l, "ClickHouse database was restored to an earlier state; delivery is suspended and queued rows are kept") ||
			!strings.Contains(l, "destination_id=id-A") || !strings.Contains(l, "queued_rows=") {
			t.Fatalf("round %d: the rollback must be logged:\n%s", round, l)
		}
		rb, ok, err := h.store.RolledBack(ctx)
		if err != nil || !ok || rb.Destination.ID != destID || rb.Confirmed != acknowledged.Confirmed || rb.Found != held || rb.Destination.Pending != queued {
			t.Fatalf("round %d: the rollback must be recorded: %+v %v %v", round, rb, ok, err)
		}
		if _, ok, _ := h.store.Replaced(ctx); ok {
			t.Fatal("a rollback keeps the destination id and is not a replacement")
		}
		if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); !strings.Contains(v, "restored to an earlier state") || !strings.Contains(v, "vario destination switch") {
			t.Fatalf("status should name the rollback and the command: %q", v)
		}

		// A restart (a new process on the same spool) is refused as well.
		restarted := New(h.store, h.sink, []sources.Source{h.src}, h.opts, c.log)
		if _, err := restarted.Prepare(ctx, true); err == nil {
			t.Fatal("a restart must be refused")
		} else if de, ok := isRollback(err); !ok || isFatal(err) || de.FoundCheckpoint != held {
			t.Fatalf("round %d: a restart must be refused with the rollback: %v", round, err)
		}
		if got := pending(t, h); got != queued || h.sink.logical() != restored || h.writes() != writes {
			t.Fatal("a refused restart must not deliver, drop or write anything")
		}

		// `vario destination switch`: the explicit decision to keep the database.
		if err := h.store.AcceptRollback(ctx, destID); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); ok {
			t.Fatal("accepting must reset the cursor")
		}
		if got := pending(t, h); got != queued {
			t.Fatalf("accepting must keep the queued rows: %d of %d", got, queued)
		}
		c = h.collector(t, h.opts)
		if c.Destination().ID != destID || c.Destination().ServerID != "id-A" {
			t.Fatalf("the same destination must be used: %+v", c.Destination())
		}
		if cp := h.checkpoint(t); cp.Writer == acknowledged.Writer || cp.Confirmed == 0 || h.held(t) != cp.Confirmed {
			t.Fatalf("round %d: a new checkpoint history must start: %+v (was %+v)", round, cp, acknowledged)
		}
		h.syncDrain(t, c)
		if got := h.sink.logical(); got != want {
			t.Fatalf("round %d: after the reconciliation the database must hold the complete history once:\n got %+v\nwant %+v", round, got, want)
		}
		if cp := h.checkpoint(t); h.held(t) != cp.Confirmed {
			t.Fatalf("round %d: the database must hold the acknowledged checkpoint: %d, acknowledged %d", round, h.held(t), cp.Confirmed)
		}
		// The collector keeps running normally afterwards.
		due(c)
		if _, err := c.deliverRound(ctx, time.Now()); err != nil {
			t.Fatalf("round %d: after the recovery: %v", round, err)
		}
		// A forced, repeated reconciliation changes nothing logically.
		if _, err := c.Snapshot(ctx, h.src, true, true); err != nil {
			t.Fatal(err)
		}
		if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
			t.Fatalf("round %d: repeated reconciliation: %+v %v", round, dr, err)
		}
		if got := h.sink.logical(); got != want {
			t.Fatalf("round %d: a repeated reconciliation must not change results: %+v, want %+v", round, got, want)
		}

		// Second round: the same old backup is restored once more. It knows
		// nothing of the new checkpoint history, so this is detected again.
		h.sink.restore(backup)
		queued = 0
		logs = &syncBuffer{}
		c = New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(logs, nil)))
		c.store.SelectDestination(destID)
		c.dest, c.schemaReady, c.checkpoints = h.retained(t, destID), true, true
	}
}

// A restore that happens after the rows of a round were inserted, but before
// its checkpoint, must not be hidden by that checkpoint: the collector reads
// the checkpoint right before it writes the next one.
func TestRollbackBetweenInsertAndCheckpointIsNotMasked(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	h.syncDrain(t, c)
	backup := h.sink.backup()
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "after the backup", false)
	h.syncDrain(t, c)
	h.fix.Message("msg-3", "thread-1", "run-1", "assistant", "in flight", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, acknowledged, writes := pending(t, h), h.checkpoint(t), h.writes()

	// The destination check is not due: only the round's own read can notice.
	h.sink.beforeCPRead = func() {
		h.sink.beforeCPRead = nil
		h.sink.restore(backup)
	}
	_, err := c.deliverRound(ctx, time.Now())
	if de, ok := isRollback(err); !ok || !isFatal(err) || de.Checkpoint != acknowledged.Confirmed {
		t.Fatalf("the rollback must be noticed before the checkpoint is written: %v", err)
	}
	if h.writes() != writes || h.held(t) >= acknowledged.Confirmed {
		t.Fatal("a checkpoint written now would hide the rollback from every later check")
	}
	if pending(t, h) != queued || h.checkpoint(t) != acknowledged {
		t.Fatal("rows sent into the rolled-back database must stay queued and unacknowledged")
	}
}

// An idle collector (nothing queued) notices the rollback by its periodic
// check, stops, and says why.
func TestRollbackWhileIdleStopsCollectorAndIsReported(t *testing.T) {
	old := identityCheckInterval
	identityCheckInterval = 30 * time.Millisecond
	t.Cleanup(func() { identityCheckInterval = old })

	h := setup(t)
	h.setServerID("id-A")
	c, logs := h.logged(t)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { return h.sink.count(model.TableMessages) == 2 && pending(t, h) == 0 })
	backup := h.sink.backup()
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "after the backup", false)
	waitFor(t, func() bool { return h.sink.count(model.TableMessages) == 3 && pending(t, h) == 0 })
	time.Sleep(3 * identityCheckInterval) // idle; periodic checks pass
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported for an unchanged database")
	}

	h.sink.restore(backup)
	select {
	case err := <-done:
		if de, ok := isRollback(err); !ok || de.Configured.ServerID != "id-A" {
			t.Fatalf("Run should stop with a rollback error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle collector did not notice the rolled-back database")
	}
	if n := h.sink.count(model.TableMessages); n != 2 {
		t.Fatalf("rows were delivered after the rollback: %d", n)
	}
	l := logs.String()
	for _, want := range []string{"ClickHouse database was restored to an earlier state", "delivery stopped", "vario destination switch"} {
		if !strings.Contains(l, want) {
			t.Fatalf("log misses %q:\n%s", want, l)
		}
	}
	if _, ok, _ := h.store.RolledBack(ctx); !ok {
		t.Fatal("the rollback must be recorded for status and doctor")
	}
}

// If the newer state comes back (the right backup is restored after all), no
// decision is needed: the report is withdrawn and delivery resumes with
// everything that stayed queued.
func TestRollbackIsWithdrawnWhenTheCheckpointIsBack(t *testing.T) {
	h, c, _, _, queued := rolledBackWithBufferedRows(t)
	// An even older state: a database that holds nothing at all.
	h.sink.restore(sinkBackup{})
	due(c)
	if _, err := c.deliverRound(ctx, time.Now()); !isFatal(err) {
		t.Fatalf("an empty database must be reported as rolled back: %v", err)
	}
	if rb, ok, _ := h.store.RolledBack(ctx); !ok || rb.Found != 0 {
		t.Fatalf("recorded rollback: %+v %v", rb, ok)
	}
	// The database comes back holding the acknowledged checkpoint.
	cp := h.checkpoint(t)
	h.sink.mu.Lock()
	h.sink.checkpoints[cp.Writer] = cp.Confirmed
	h.sink.mu.Unlock()

	logs := &syncBuffer{}
	restarted := New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(logs, nil)))
	if _, err := restarted.Prepare(ctx, true); err != nil {
		t.Fatalf("the collector must start again: %v", err)
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("the report must be withdrawn")
	}
	if !strings.Contains(logs.String(), "holds the acknowledged delivery checkpoint again; delivery resumes") {
		t.Fatalf("the withdrawal must be logged:\n%s", logs.String())
	}
	if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); v != "" {
		t.Fatalf("the error must be cleared: %q", v)
	}
	if dr, err := restarted.Drain(ctx); err != nil || dr.Remaining != 0 || int64(dr.Delivered) != queued {
		t.Fatalf("the queued rows must be delivered: %+v %v", dr, err)
	}
	if got := h.checkpoint(t); got.Writer != cp.Writer || got.Confirmed != cp.Confirmed+1 || h.held(t) != got.Confirmed {
		t.Fatalf("the checkpoint history continues: %+v (was %+v)", got, cp)
	}
}

// A checkpoint whose insert fails, or whose outcome is unknown, is never
// recorded as acknowledged, and the rows it would have covered stay queued.
// They are sent again with a new checkpoint; nothing is duplicated logically
// and a checkpoint that did reach the database is not mistaken for a rollback.
func TestFailedAndAmbiguousCheckpointWritesKeepRowsQueued(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, base := pending(t, h), h.checkpoint(t)
	if queued == 0 || base.Confirmed == 0 || h.held(t) != base.Confirmed {
		t.Fatalf("setup: %d rows, checkpoint %+v, held %d", queued, base, h.held(t))
	}
	invariant := func(what string) {
		t.Helper()
		if cp := h.checkpoint(t); cp.Confirmed > h.held(t) {
			t.Fatalf("%s: checkpoint %d is recorded as acknowledged, the database holds %d", what, cp.Confirmed, h.held(t))
		}
	}

	// 1. The write fails outright: the rows were inserted, but stay queued.
	h.sink.cpWriteErr = &clickhouse.Error{Status: 503, Message: "unavailable", Transient: true}
	dr, err := c.Drain(ctx)
	if err == nil || isFatal(err) || dr.Delivered != 0 || !strings.Contains(err.Error(), "was not confirmed") || !strings.Contains(err.Error(), "stay queued") {
		t.Fatalf("failed checkpoint write: %+v %v", dr, err)
	}
	if h.sink.count(model.TableMessages) != 2 {
		t.Fatal("the rows themselves were inserted")
	}
	if pending(t, h) != queued || h.checkpoint(t) != base {
		t.Fatalf("rows not covered by a checkpoint must stay queued: %d of %d, %+v", pending(t, h), queued, h.checkpoint(t))
	}
	invariant("failed write")
	if st, _ := h.store.Stats(ctx); st.FailingEntries == 0 || !strings.Contains(st.LastError, "delivery checkpoint") {
		t.Fatalf("the failure must be visible in the queue: %+v", st)
	}
	if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); !strings.Contains(v, "delivery checkpoint") {
		t.Fatalf("status: %q", v)
	}

	// 2. The write reaches the database, but the response is lost.
	h.sink.cpWriteErr = nil
	h.sink.cpAmbiguous = 1
	if dr, err = c.Drain(ctx); err == nil || isFatal(err) || dr.Delivered != 0 {
		t.Fatalf("ambiguous checkpoint write: %+v %v", dr, err)
	}
	if pending(t, h) != queued || h.checkpoint(t) != base || h.held(t) != base.Confirmed+1 {
		t.Fatalf("an unconfirmed checkpoint must not be recorded: %+v, held %d", h.checkpoint(t), h.held(t))
	}
	invariant("ambiguous write")
	// The database being ahead is not a rollback: the next check passes.
	due(c)
	if dr, err = c.Drain(ctx); err != nil || dr.Remaining != 0 || int64(dr.Delivered) != queued {
		t.Fatalf("retry: %+v %v", dr, err)
	}
	// Above the one whose outcome was unknown; a number is never reused.
	if cp := h.checkpoint(t); cp.Confirmed < base.Confirmed+2 || h.held(t) != cp.Confirmed {
		t.Fatalf("the retry writes a new, higher checkpoint: %+v, held %d", cp, h.held(t))
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be reported")
	}
	// Sent three times, present once.
	if got, want := h.sink.logical(), h.reference(t); got != want {
		t.Fatalf("resent rows must not change results: %+v, want %+v", got, want)
	}
}

// An insert that was committed but reported as failed (a lost response) is
// sent again; the other tables of the round are acknowledged under their own
// checkpoint, and results are unchanged.
func TestAmbiguousInsertIsResentUnderALaterCheckpoint(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, base := pending(t, h), h.checkpoint(t)
	h.sink.ambiguousData = 1
	dr, err := c.Drain(ctx)
	if err == nil || dr.Delivered == 0 || dr.Remaining == 0 || dr.Remaining >= queued {
		t.Fatalf("one table must stay queued, the others be acknowledged: %+v %v", dr, err)
	}
	if cp := h.checkpoint(t); cp.Confirmed != base.Confirmed+1 || h.held(t) != cp.Confirmed {
		t.Fatalf("the acknowledged tables are covered by a checkpoint: %+v", cp)
	}
	if dr, err = c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("retry: %+v %v", dr, err)
	}
	if got, want := h.sink.logical(), h.reference(t); got != want {
		t.Fatalf("an ambiguous insert must not change results: %+v, want %+v", got, want)
	}
}

// A collector that dies between the inserts of a round and its checkpoint
// leaves the rows queued. Whatever happens to the database meanwhile, the
// next collector sends them again, so the gap can never lose rows: here the
// database is even restored to a backup that lacks them and still holds the
// acknowledged checkpoint, which no checkpoint comparison could notice.
func TestInterruptedRoundIsResentAfterRestart(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	h.syncDrain(t, c)
	backup := h.sink.backup() // holds the acknowledged checkpoint
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "inserted, never covered by a checkpoint", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, base := pending(t, h), h.checkpoint(t)

	// The process dies after the inserts: the checkpoint is never written.
	h.sink.cpWriteErr = errors.New("killed")
	if _, err := c.deliverRound(ctx, time.Now()); err == nil {
		t.Fatal("the round must not complete")
	}
	h.sink.cpWriteErr = nil
	if h.sink.count(model.TableMessages) != 3 || pending(t, h) != queued || h.checkpoint(t) != base {
		t.Fatalf("after the crash: %d messages, %d rows queued, %+v", h.sink.count(model.TableMessages), pending(t, h), h.checkpoint(t))
	}
	// Restored like REPLACE PARTITION does: the deduplication memory stays,
	// so a retry under the tokens of the interrupted round would be swallowed.
	h.sink.loseRowsKeepTokens(&backup)

	restarted, _ := h.logged(t)
	if dr, err := restarted.Drain(ctx); err != nil || dr.Remaining != 0 || int64(dr.Delivered) != queued {
		t.Fatalf("after the restart: %+v %v", dr, err)
	}
	if got, want := h.sink.logical(), h.reference(t); got != want {
		t.Fatalf("the interrupted rows must not be lost: %+v, want %+v", got, want)
	}
}

// Offline buffering: the collector restarts while ClickHouse is unreachable
// and keeps collecting. When the database comes back restored from an older
// backup, the first destination check stops delivery before anything is sent.
func TestRollbackFoundAfterOfflineRestart(t *testing.T) {
	h, _, _, _, queued := rolledBackWithBufferedRows(t)
	restored := h.sink.logical()

	h.sink.schemaErr = &clickhouse.Error{Message: "dial tcp: connection refused", Transient: true}
	logs := &syncBuffer{}
	offline := New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(logs, nil)))
	if _, err := offline.Prepare(ctx, true); err != nil {
		t.Fatalf("an offline start must not be refused: %v", err)
	}
	h.fix.Message("msg-offline", "thread-1", "run-1", "assistant", "collected while offline", false)
	if err := offline.syncOnce(ctx, h.src, offline.log); err != nil {
		t.Fatal(err)
	}
	buffered := pending(t, h)
	if buffered <= queued {
		t.Fatalf("collection must continue while offline: %d rows, had %d", buffered, queued)
	}
	if _, err := offline.deliverRound(ctx, time.Now()); err == nil || isFatal(err) {
		t.Fatalf("offline delivery is retried, not stopped: %v", err)
	}

	h.sink.schemaErr = nil
	_, err := offline.deliverRound(ctx, time.Now().Add(time.Hour))
	if _, ok := isRollback(err); !ok || !isFatal(err) {
		t.Fatalf("the rollback must be found when ClickHouse is back: %v", err)
	}
	if h.sink.logical() != restored || pending(t, h) != buffered {
		t.Fatal("nothing may be delivered or dropped")
	}
	if _, ok, _ := h.store.RolledBack(ctx); !ok {
		t.Fatal("the rollback must be recorded")
	}
}

// A database migrated before delivery checkpoints existed keeps working as
// before, with a warning that a rollback cannot be detected. After `vario
// migrate` the first checkpoint is written and rollbacks are detected,
// including one to a state from before the migration.
func TestDatabaseWithoutCheckpointsStaysCompatible(t *testing.T) {
	sink := newSink()
	sink.noCheckpoints = true
	h := setupSink(t, sink)
	h.setServerID("id-A")
	c, logs := h.logged(t)
	h.syncDrain(t, c)
	if got, want := h.sink.logical(), h.reference(t); got != want {
		t.Fatalf("delivery without checkpoints: %+v, want %+v", got, want)
	}
	if cp := h.checkpoint(t); cp != (spool.Checkpoint{}) || sink.cpReads != 0 || sink.cpWrites != 0 {
		t.Fatalf("nothing may be recorded or sent without the table: %+v, %d reads, %d writes", cp, sink.cpReads, sink.cpWrites)
	}
	if l := logs.String(); strings.Count(l, "restored to an earlier state cannot be detected; run `vario migrate`") != 1 {
		t.Fatalf("the missing protection must be logged once:\n%s", l)
	}
	old := h.sink.backup()

	// Without a checkpoint a restore goes unnoticed: nothing claims otherwise.
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "lost without notice", false)
	h.syncDrain(t, c)
	h.sink.restore(old)
	due(c)
	if _, err := c.deliverRound(ctx, time.Now()); err != nil {
		t.Fatalf("no checkpoint, no detection: %v", err)
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("nothing can be reported without a checkpoint")
	}

	// `vario migrate` creates the table: the next check writes the first checkpoint.
	sink.mu.Lock()
	sink.noCheckpoints = false
	sink.mu.Unlock()
	due(c)
	if _, err := c.deliverRound(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	cp := h.checkpoint(t)
	if cp.Confirmed != 1 || h.held(t) != 1 || !strings.Contains(logs.String(), "delivery checkpoints enabled") {
		t.Fatalf("first checkpoint: %+v, held %d\n%s", cp, h.held(t), logs.String())
	}
	h.fix.Message("msg-3", "thread-1", "run-1", "assistant", "covered by a checkpoint", false)
	h.syncDrain(t, c)

	// Restored to the state from before the migration: no checkpoint table.
	h.sink.restore(old)
	sink.mu.Lock()
	sink.noCheckpoints = true
	sink.mu.Unlock()
	due(c)
	_, err := c.deliverRound(ctx, time.Now())
	if de, ok := isRollback(err); !ok || !isFatal(err) || de.FoundCheckpoint != 0 || de.Checkpoint != h.checkpoint(t).Confirmed {
		t.Fatalf("a restore to a state without checkpoints must be detected: %v", err)
	}
}

// The collector's user may lack privileges on the checkpoint table (a
// migration whose grants were not applied). Before any checkpoint was
// acknowledged, delivery continues unprotected with a warning; afterwards the
// same failure keeps rows queued, because the database cannot be verified.
func TestCheckpointTableWithoutPrivileges(t *testing.T) {
	denied := &clickhouse.Error{Status: 403, Code: 497, Message: "Not enough privileges"}
	sink := newSink()
	sink.cpWriteErr = denied
	h := setupSink(t, sink)
	c, logs := h.logged(t)
	h.syncDrain(t, c)
	if cp := h.checkpoint(t); cp.Confirmed != 0 || h.sink.count(model.TableMessages) != 2 {
		t.Fatalf("delivery must continue without checkpoints: %+v", cp)
	}
	if !strings.Contains(logs.String(), "cannot use the delivery checkpoint table") {
		t.Fatalf("the missing privilege must be logged:\n%s", logs.String())
	}

	// Granted: checkpoints start. Revoked later: rows stay queued.
	sink.mu.Lock()
	sink.cpWriteErr = nil
	sink.mu.Unlock()
	due(c)
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "covered", false)
	h.syncDrain(t, c)
	base := h.checkpoint(t)
	if base.Confirmed == 0 {
		t.Fatal("checkpoints must start once the privilege is there")
	}
	sink.mu.Lock()
	sink.cpReadErr = denied
	sink.mu.Unlock()
	h.fix.Message("msg-3", "thread-1", "run-1", "assistant", "not verifiable", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued := pending(t, h)
	due(c)
	if dr, err := c.Drain(ctx); err == nil || isFatal(err) || dr.Delivered != 0 {
		t.Fatalf("an unverifiable database must not be delivered to: %+v %v", dr, err)
	}
	if pending(t, h) != queued || h.checkpoint(t) != base || h.sink.count(model.TableMessages) != 3 {
		t.Fatal("rows must stay queued and nothing be sent")
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("a failed read is not a rollback")
	}
}

// A lower checkpoint is only believed when repeated reads confirm it: a
// replica that has not caught up with an acknowledged insert must not
// suspend delivery.
func TestLaggingReadIsNotARollback(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	h.syncDrain(t, c)
	cp := h.checkpoint(t)
	reads := 0
	h.sink.beforeCPRead = func() {
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		if reads++; reads == 1 {
			h.sink.checkpoints[cp.Writer] = cp.Confirmed - 1 // a stale replica
		} else {
			h.sink.checkpoints[cp.Writer] = cp.Confirmed
		}
	}
	due(c)
	if _, err := c.deliverRound(ctx, time.Now()); err != nil {
		t.Fatalf("a stale read must be retried: %v", err)
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok || reads < 2 {
		t.Fatalf("no rollback may be reported (%d reads)", reads)
	}
}

// A database that holds a higher checkpoint than the spool recorded (the
// local state was restored from a copy, or a write went unnoticed) is not a
// rollback: delivery continues above the database's number.
func TestDatabaseAheadOfLocalStateIsAccepted(t *testing.T) {
	h := setup(t)
	c, _ := h.logged(t)
	h.syncDrain(t, c)
	cp := h.checkpoint(t)
	h.sink.mu.Lock()
	h.sink.checkpoints[cp.Writer] = cp.Confirmed + 10
	h.sink.mu.Unlock()
	h.fix.Message("msg-2", "thread-1", "run-1", "assistant", "next", false)
	due(c)
	h.syncDrain(t, c)
	if got := h.checkpoint(t); got.Confirmed != cp.Confirmed+11 || h.held(t) != got.Confirmed {
		t.Fatalf("the next checkpoint must be above the database's: %+v, held %d", got, h.held(t))
	}
}

// Regression for destination handling: a recreated database (a new id) is
// reported as replaced, never as rolled back, and the destination recorded
// for it by the switch starts its own checkpoint history.
func TestReplacedDatabaseIsNotARollback(t *testing.T) {
	h, c, _, queued := replacedWithBufferedRows(t)
	oldID, oldCP := c.Destination().ID, h.checkpoint(t)
	h.emptySink()
	_, err := c.deliverRound(ctx, time.Now())
	var de *DestinationError
	if !errors.As(err, &de) || !de.Replaced || de.RolledBack {
		t.Fatalf("a new destination id is a replacement: %v", err)
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("a replacement must not be recorded as a rollback")
	}
	repl, err := h.store.Replace(ctx, oldID, "id-B")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.MovePending(ctx, oldID, repl.ID); err != nil || n != queued {
		t.Fatalf("move: %d %v", n, err)
	}
	if err := h.store.Activate(ctx, repl.ID); err != nil {
		t.Fatal(err)
	}
	fresh := h.collector(t, h.opts)
	h.syncDrain(t, fresh)
	cp, err := h.store.Checkpoint(ctx, repl.ID)
	if err != nil || cp.Writer == "" || cp.Writer == oldCP.Writer || cp.Confirmed < 2 {
		t.Fatalf("the new destination needs its own checkpoint history: %+v %v", cp, err)
	}
	if kept, _ := h.store.Checkpoint(ctx, oldID); kept != oldCP {
		t.Fatalf("the replaced destination's checkpoint must be untouched: %+v, was %+v", kept, oldCP)
	}
	if got, want := h.sink.logical(), h.reference(t); got != want {
		t.Fatalf("the new database must hold the complete history: %+v, want %+v", got, want)
	}
}

// A database that was dropped and recreated between two destination checks
// holds no checkpoint either. The round that notices this first must report
// what it is, a replacement, and not a rollback of "the same database".
func TestRecreatedDatabaseNoticedByCheckpointReadIsAReplacement(t *testing.T) {
	h, c, _, queued := replacedWithBufferedRows(t)
	h.emptySink()
	c.checkedAt = time.Now() // the periodic check is not due: the round goes ahead
	_, err := c.deliverRound(ctx, time.Now())
	var de *DestinationError
	if !errors.As(err, &de) || !isFatal(err) || !de.Replaced || de.RolledBack || de.Found != "id-B" {
		t.Fatalf("a recreated database is a replacement: %v", err)
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("no rollback may be recorded for a replaced database")
	}
	if rep, ok, _ := h.store.Replaced(ctx); !ok || rep.Found != "id-B" {
		t.Fatalf("the replacement must be recorded: %+v %v", rep, ok)
	}
	if pending(t, h) != queued || h.held(t) != 0 {
		t.Fatal("rows must stay queued and no checkpoint be written into the new database")
	}
}

// Rows kept for a previous destination follow the same rule when they are
// retried: they are not sent to a database that was rolled back, and a
// refused retry reports nothing about the active destination.
func TestRetainedRowsAreNotRetriedIntoARolledBackDatabase(t *testing.T) {
	h, old, _ := keptAfterReplacement(t)
	// The original database (id-A) is back, restored from an older backup that
	// knows nothing of this spool's checkpoints.
	h.setServerID("id-A")
	h.sink.mu.Lock()
	h.sink.checkpoints = map[string]uint64{}
	h.sink.mu.Unlock()
	before := h.sink.count(model.TableMessages)
	opts := h.opts
	res, err := h.unprepared(opts).DeliverRetained(ctx, old)
	if de, ok := isRollback(err); !ok || de.Configured.ID != old.ID || res.Delivered != 0 || res.Remaining != old.Pending {
		t.Fatalf("retry into a rolled-back database: %+v %v", res, err)
	}
	if h.sink.count(model.TableMessages) != before {
		t.Fatal("nothing may be sent")
	}
	if _, ok, _ := h.store.RolledBack(ctx); ok {
		t.Fatal("a refused retry must not record a rollback")
	}
}
