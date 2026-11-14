package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// D1 against a real ClickHouse: a batch is inserted, the checkpoint after it
// is not confirmed, the partitions are restored from a snapshot that lacks
// the batch but holds the confirmed checkpoint (ClickHouse keeps remembering
// the batch's insert_deduplication_token), and the batch is retried. The
// retry must store the rows again before the queue is cleared.

// faultSink is the operational client with injected faults. It records every
// insert that reached ClickHouse.
type faultSink struct {
	*clickhouse.Client
	mu sync.Mutex
	// checkpoint: "" passes writes through, "fail" fails them without sending,
	// "lost" sends them and then reports a lost response.
	checkpoint string
	// insertLost reports a lost response after every insert ClickHouse committed.
	insertLost bool
	inserts    []recordedInsert
}

type recordedInsert struct {
	table, token string
	body         []byte
}

var errInjectedLost = &clickhouse.Error{Message: "read: connection reset by peer", Transient: true}

func (s *faultSink) Insert(ctx context.Context, table string, body []byte, token string) error {
	if err := s.Client.Insert(ctx, table, body, token); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserts = append(s.inserts, recordedInsert{table, token, append([]byte(nil), body...)})
	if s.insertLost {
		return errInjectedLost
	}
	return nil
}

func (s *faultSink) WriteCheckpoint(ctx context.Context, writer string, seq uint64) error {
	s.mu.Lock()
	mode := s.checkpoint
	s.mu.Unlock()
	switch mode {
	case "fail":
		return &clickhouse.Error{Status: 503, Message: "injected checkpoint failure", Transient: true}
	case "lost":
		if err := s.Client.WriteCheckpoint(ctx, writer, seq); err != nil {
			return err
		}
		return errInjectedLost
	}
	return s.Client.WriteCheckpoint(ctx, writer, seq)
}

func (s *faultSink) set(checkpoint string, insertLost bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint, s.insertLost = checkpoint, insertLost
}

func (s *faultSink) recorded() []recordedInsert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedInsert(nil), s.inserts...)
}

func (e *recoveryEnv) faultCollector(db string, store *spool.Store, sink *faultSink) *Collector {
	e.t.Helper()
	opts := e.collector(db).opts
	opts.MaxInsertRows = 1000 // one round sends the whole batch
	c := New(store, sink, []sources.Source{e.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *recoveryEnv) heldCheckpoint(db string, store *spool.Store, destID int64) uint64 {
	e.t.Helper()
	cp, err := store.Checkpoint(ctx, destID)
	if err != nil {
		e.t.Fatal(err)
	}
	seq, _, err := e.clients[db].ReadCheckpoint(ctx, cp.Writer)
	if err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func TestIntegrationD1UncheckpointedInsertSurvivesPartitionRestore(t *testing.T) {
	type scenario struct {
		name string
		// fault makes the first round's inserts reach ClickHouse without a
		// confirmed checkpoint.
		fault   func(e *recoveryEnv, c *Collector, sink *faultSink)
		restart bool
	}
	round := func(checkpoint string, insertLost bool) func(*recoveryEnv, *Collector, *faultSink) {
		return func(e *recoveryEnv, c *Collector, sink *faultSink) {
			sink.set(checkpoint, insertLost)
			if _, err := c.deliverRound(ctx, time.Now()); err == nil || isFatal(err) {
				e.t.Fatalf("the round must fail without stopping delivery: %v", err)
			}
			sink.set("", false)
		}
	}
	// The process dies after ClickHouse acknowledged the inserts, before
	// anything about them reached the spool.
	crash := func(e *recoveryEnv, c *Collector, sink *faultSink) {
		entries, err := e.store.Due(ctx, time.Now(), 1<<30)
		if err != nil {
			e.t.Fatal(err)
		}
		order, groups := byTable(entries)
		for _, table := range order {
			if _, err := c.insertGroup(ctx, table, groups[table]); err != nil {
				e.t.Fatal(err)
			}
		}
	}
	var scenarios []scenario
	for _, restart := range []bool{false, true} {
		scenarios = append(scenarios,
			scenario{"checkpoint failed", round("fail", false), restart},
			scenario{"checkpoint ack lost", round("lost", false), restart},
			scenario{"insert ack lost", round("", true), restart},
		)
	}
	scenarios = append(scenarios, scenario{"crash after insert", crash, true})

	for i, sc := range scenarios {
		name := sc.name
		if sc.restart {
			name += " then restart"
		}
		t.Run(name, func(t *testing.T) {
			e := newRecoveryEnv(t)
			suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), i)
			db, snap := "vario_d1_"+suffix, "vario_d1_snap_"+suffix
			e.migrate(db)
			sink := &faultSink{Client: e.clients[db]}
			c := e.faultCollector(db, e.store, sink)
			destID := c.Destination().ID

			// Delivered and checkpointed; the snapshot holds checkpoint N.
			e.syncAndDrain(c)
			base, err := e.store.Checkpoint(ctx, destID)
			if err != nil || base.Confirmed == 0 {
				t.Fatalf("checkpoint after the initial delivery: %+v %v", base, err)
			}
			delivered := e.mustRead(db)
			e.copyTables(db, snap)

			// One more conversation is queued.
			e.moreHistory(1)
			if err := c.syncOnce(ctx, e.src, c.log); err != nil {
				t.Fatal(err)
			}
			st, _ := e.store.Stats(ctx)
			queued := st.Rows
			if queued == 0 {
				t.Fatal("expected queued rows")
			}
			firstInsert := len(sink.recorded())

			// 1-3. ClickHouse accepts the batch; the checkpoint is not confirmed.
			sc.fault(e, c, sink)
			inserted := e.mustRead(db)
			if inserted.Msgs != delivered.Msgs+1 || inserted.Turns != delivered.Turns+1 || inserted.Input != delivered.Input+1000 {
				t.Fatalf("the batch must have been stored: %+v, was %+v", inserted, delivered)
			}
			if st, _ := e.store.Stats(ctx); st.Rows != queued {
				t.Fatalf("the rows must stay queued: %d of %d", st.Rows, queued)
			}
			if cp, _ := e.store.Checkpoint(ctx, destID); cp.Confirmed != base.Confirmed {
				t.Fatalf("no checkpoint may be confirmed: %+v, was %+v", cp, base)
			}
			first := sink.recorded()[firstInsert:]
			if len(first) == 0 {
				t.Fatal("no insert was recorded")
			}

			// 4. The partitions are restored: the batch is gone, checkpoint N stays.
			e.restorePartitions(db, snap)
			if got := e.mustRead(db); got != delivered {
				t.Fatalf("the restore must remove the batch: %+v, want %+v", got, delivered)
			}
			if held := e.heldCheckpoint(db, e.store, destID); held != base.Confirmed {
				t.Fatalf("the restore must keep the confirmed checkpoint %d, holds %d", base.Confirmed, held)
			}
			// Precondition: ClickHouse swallows the batch under its original tokens.
			for _, in := range first {
				if err := e.clients[db].Insert(ctx, in.table, in.body, in.token); err != nil {
					t.Fatal(err)
				}
			}
			if got := e.mustRead(db); got != delivered {
				t.Fatalf("this server stores a batch under a remembered token, the scenario does not apply: %+v", got)
			}
			used := map[string]bool{}
			for _, in := range sink.recorded() {
				used[in.token] = true
			}

			// 5-7. Retry, by the same collector or a restarted one.
			store, rc := e.store, c
			if sc.restart {
				if store, err = spool.Open(e.store.Path(), 64<<20); err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				rc = e.faultCollector(db, store, sink)
			}
			retryFrom := len(sink.recorded())
			dr, err := rc.Drain(ctx)
			if err != nil || dr.Remaining != 0 || int64(dr.Delivered) != queued {
				t.Fatalf("recovery: %+v %v", dr, err)
			}

			// The stored records, not the acknowledgements, decide.
			want := e.complete("vario_d1_ref_" + suffix)
			if got := e.mustRead(db); got != want {
				t.Fatalf("D1: the queue was cleared, but the database does not hold the records:\n got %+v\nwant %+v", got, want)
			}
			for _, in := range sink.recorded()[retryFrom:] {
				if used[in.token] {
					t.Fatalf("%s was retried under a token used before", in.table)
				}
			}
			if got := e.mustRead(db); got.Msgs != delivered.Msgs+1 || got.Input != delivered.Input+1000 {
				t.Fatalf("token totals: %+v, delivered before %+v", got, delivered)
			}
			for table, n := range map[string]int{model.TableMessages: want.Msgs, model.TableProviderTurns: want.Turns} {
				if p := e.physical(db, table); p < n {
					t.Fatalf("%s: %d rows stored for %d records", table, p, n)
				}
			}
			cp, err := store.Checkpoint(ctx, destID)
			if err != nil || cp.Confirmed <= base.Confirmed || cp.Writer != base.Writer {
				t.Fatalf("the recovery must be covered by a new checkpoint: %+v, was %+v (%v)", cp, base, err)
			}
			if held := e.heldCheckpoint(db, store, destID); held != cp.Confirmed {
				t.Fatalf("the database holds checkpoint %d, the spool confirmed %d", held, cp.Confirmed)
			}
			if st, _ := store.Stats(ctx); st.Rows != 0 || st.Entries != 0 {
				t.Fatalf("the queue must be empty: %+v", st)
			}
			if _, ok, _ := store.RolledBack(ctx); ok {
				t.Fatal("no rollback may be reported")
			}

			// Normal operation continues and counts every record once.
			e.moreHistory(2)
			e.syncAndDrain(rc)
			if got := e.mustRead(db); got.Msgs != want.Msgs+1 || got.Turns != want.Turns+1 || got.Input != want.Input+1000 {
				t.Fatalf("after the recovery: %+v, was %+v", got, want)
			}
		})
	}
}
