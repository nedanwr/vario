package collector

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
)

// collectorAt is collectorFor with the destination reached under endpoint
// (an alias of the same server, for example).
func (e *privEnv) collectorAt(endpoint, db string, sink Sink) *Collector {
	e.t.Helper()
	opts := e.opts(db)
	opts.Endpoint = endpoint
	c := New(e.store, sink, []sources.Source{e.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// reopen closes and reopens the local state, as a restart does.
func (e *privEnv) reopen() {
	e.t.Helper()
	path := e.store.Path()
	if err := e.store.Close(); err != nil {
		e.t.Fatal(err)
	}
	st, err := spool.Open(path, 64<<20)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	e.store = st
}

// requireLatestGeneration checks the generation of the sensitive records'
// latest rows.
func (e *privEnv) requireLatestGeneration(db string, want uint64, when string) {
	e.t.Helper()
	for _, c := range []struct{ table, col, key string }{
		{model.TableMessages, "message_id", "msg-s1"},
		{model.TableConversations, "conversation_id", "thread-s1"},
		{model.TableExecutionEvents, "event_id", "item-s1"},
	} {
		if g := generationOf(e.t, e.latest(db, c.table, c.col, c.key)); g != want {
			e.t.Fatalf("%s: latest %s row of generation %d, want %d", when, c.table, g, want)
		}
	}
}

// F1 (second round): rows queued for destination A under full are moved to
// destination B, which was synchronized under metrics at an earlier
// generation and whose fingerprints therefore match the current (metrics)
// settings again. Without resetting B, its re-read skipped every record and
// the moved full rows, of a later generation than B's sanitized ones,
// became the latest version.
//
// Variants: how the rows are moved (`destination switch --move-pending`, or
// `destination pending --move` after B already finished its re-read), how
// A's delivery failed (never arrived, or arrived with a lost response), the
// delivery order of moved and re-read rows, a collector restart before the
// re-read, and B reached under an alias endpoint.
func TestIntegrationF1MovedPendingIntoSynchronizedDestination(t *testing.T) {
	type variant struct {
		how, fault  string
		movedLast   bool // the moved rows are delivered after B's re-read
		restart     bool
		alias       bool
		offlineMove bool // B is unreachable when the rows are moved and re-read
	}
	var vs []variant
	for _, how := range []string{"switch", "pending"} {
		for _, fault := range []string{"fail", "lost"} {
			for _, last := range []bool{false, true} {
				vs = append(vs, variant{how: how, fault: fault, movedLast: last})
			}
		}
	}
	vs = append(vs,
		variant{how: "switch", fault: "lost", restart: true},
		variant{how: "switch", fault: "fail", alias: true, movedLast: true},
		variant{how: "pending", fault: "lost", alias: true},
		variant{how: "switch", fault: "fail", offlineMove: true, restart: true},
		variant{how: "pending", fault: "lost", offlineMove: true, movedLast: true},
	)
	for _, v := range vs {
		name := fmt.Sprintf("%s_%s_movedLast=%v", v.how, v.fault, v.movedLast)
		if v.restart {
			name += "_restart"
		}
		if v.alias {
			name += "_alias"
		}
		if v.offlineMove {
			name += "_offline"
		}
		t.Run(name, func(t *testing.T) {
			// B is synchronized under metrics (generation 1).
			e := newPrivEnv(t, spec(config.ModeMetrics))
			dbB := e.db
			sinkB := e.sink
			e.sensitive(1)
			b := e.collector()
			e.sync(b)
			e.deliverAll(b)
			e.requireMode(dbB, config.ModeMetrics, 1, "B before")
			base := e.mustRead(dbB)
			idB := b.Destination().ID

			// The user switches to A under full (generation 2); A's rows stay queued.
			dbA := fmt.Sprintf("vario_f1a_%d", time.Now().UnixNano())
			idA := e.migrate(dbA)
			sinkA := &gateSink{Client: e.clients[dbA]}
			e.setPolicy(spec(config.ModeFull))
			if _, err := e.store.Resolve(ctx, e.endpoint, dbA, idA); err != nil {
				t.Fatal(err)
			}
			a := e.collectorFor(dbA, sinkA, e.src)
			e.sync(a)
			e.sink = sinkA
			e.queueStale(a, v.fault)
			e.sink = sinkB
			if gen, _, _ := e.store.Generation(ctx); gen != 2 {
				t.Fatalf("generation %d, want 2", gen)
			}
			fromA := a.Destination().ID

			// Back to metrics (generation 3), and back to B.
			e.setPolicy(spec(config.ModeMetrics))
			endpointB := e.endpoint
			if v.alias {
				// The same server under another name: recognized by its destination id.
				endpointB = strings.Replace(e.endpoint, "127.0.0.1", "localhost", 1)
				if endpointB == e.endpoint {
					t.Skip("the alias variant needs a test server at 127.0.0.1")
				}
			}
			res, err := e.store.Resolve(ctx, endpointB, dbB, b.Destination().ServerID)
			if err != nil || res.Configured.ID != idB || res.Previous == nil || res.Previous.ID != fromA || res.Aliased != v.alias {
				t.Fatalf("resolve B: %+v %v", res, err)
			}
			if v.offlineMove {
				sinkB.set("down")
			}
			move := func() {
				n, err := e.store.MovePending(ctx, fromA, idB)
				if err != nil || n == 0 {
					t.Fatalf("move: %d %v", n, err)
				}
			}
			if v.how == "switch" {
				move() // destination switch --move-pending
			}
			if err := e.store.Activate(ctx, idB); err != nil {
				t.Fatal(err)
			}
			if v.restart {
				e.reopen()
			}
			b = e.collectorAt(endpointB, dbB, sinkB)
			if v.how == "pending" {
				// B completes its re-read first: its fingerprints match the
				// metrics settings again, so it skips every record.
				e.sync(b)
				if need, _, _ := e.store.NeedsResync(ctx, e.src.ID(), e.pol.Fingerprint()); need {
					t.Fatal("B should have finished its re-read")
				}
				if !v.offlineMove {
					e.deliverAll(b)
				}
				move() // destination pending <A> --move
				if need, force, _ := e.store.NeedsResync(ctx, e.src.ID(), e.pol.Fingerprint()); !need || !force {
					t.Fatalf("moving rows into B must require a forced re-read: need %v force %v", need, force)
				}
				if v.restart {
					e.reopen()
				}
				b = e.collectorAt(endpointB, dbB, sinkB)
			}
			if v.movedLast {
				if !v.offlineMove {
					e.queueStale(b, "fail") // the moved rows are attempted and fail; due in an hour
				}
			}
			e.sync(b) // B's re-read under generation 3
			if v.offlineMove {
				sinkB.set("")
				b = e.collectorAt(endpointB, dbB, sinkB)
				e.sync(b)
			}
			if v.movedLast {
				e.deliverFresh(b)
				e.requireMode(dbB, config.ModeMetrics, 1, "B, re-read delivered before the moved rows")
			}
			e.deliverAll(b)
			e.requireMode(dbB, config.ModeMetrics, 1, "B after the moved rows")
			e.requireLatestGeneration(dbB, 3, "B")
			if e.physical(dbB, model.TableMessages, "SYNTH-MSG-1") == 0 {
				t.Fatal("the moved full rows should have been delivered to B (and outranked)")
			}
			e.requireCounts(dbB, base, "B")
			// Further syncs, reconciliations and restarts change nothing.
			e.reopen()
			b = e.collectorAt(endpointB, dbB, sinkB)
			e.sync(b)
			if _, err := b.Snapshot(ctx, e.src, false, false); err != nil {
				t.Fatal(err)
			}
			e.deliverAll(b)
			e.requireMode(dbB, config.ModeMetrics, 1, "B after reconciliation")
			e.requireCounts(dbB, base, "B after reconciliation")

			// A gets a full re-read under metrics when used again ("lost": it
			// holds A's full rows of generation 2).
			if _, err := e.store.Resolve(ctx, e.endpoint, dbA, idA); err != nil {
				t.Fatal(err)
			}
			a = e.collectorFor(dbA, sinkA, e.src)
			e.sync(a)
			e.sink = sinkA
			e.deliverAll(a)
			e.sink = sinkB
			e.requireMode(dbA, config.ModeMetrics, 1, "A after switching back")
			e.requireCounts(dbA, base, "A")
		})
	}
}

// F2: the local state is restored from an older copy, after both
// destinations received rows of later generations under other settings.
// When one destination's check starts a generation above its database, the
// other destination's verification from the old copy is stale and must be
// withdrawn; that destination's fingerprints from the old copy must not keep
// its re-read from replacing the rows of the lost generations.
//
// "tie": B holds the generation that A's check starts (a stale verification
// would hide the tie). "gap": B holds an earlier generation than the one A's
// check starts (B's old fingerprints would skip its records). "offline": as
// gap, with B's re-read done while B is unreachable, before its check.
func TestIntegrationF2RestoredStateAcrossDestinations(t *testing.T) {
	for _, how := range []string{"tie", "gap", "offline"} {
		t.Run(how, func(t *testing.T) {
			e := newPrivEnv(t, spec(config.ModeMetrics))
			dbA := e.db
			sinkA := e.sink
			dbB := fmt.Sprintf("vario_f2b_%d", time.Now().UnixNano())
			idB := e.migrate(dbB)
			sinkB := &gateSink{Client: e.clients[dbB]}
			idA := ""
			e.sensitive(1)

			onA := func() *Collector {
				if _, err := e.store.Resolve(ctx, e.endpoint, dbA, idA); err != nil {
					t.Fatal(err)
				}
				e.sink = sinkA
				return e.collectorFor(dbA, sinkA, e.src)
			}
			onB := func() *Collector {
				if _, err := e.store.Resolve(ctx, e.endpoint, dbB, idB); err != nil {
					t.Fatal(err)
				}
				e.sink = sinkB
				return e.collectorFor(dbB, sinkB, e.src)
			}
			// Both synchronized and verified under metrics (generation 1).
			a := onA()
			idA = a.Destination().ServerID
			e.sync(a)
			e.deliverAll(a)
			base := e.mustRead(dbA)
			b := onB()
			e.sync(b)
			e.deliverAll(b)
			a = onA()
			e.sync(a)
			e.deliverAll(a)

			backup := filepath.Join(t.TempDir(), "backup.db")
			path := e.store.Path()
			if err := e.store.Close(); err != nil {
				t.Fatal(err)
			}
			copyFile(t, path, backup)
			e.store = nil
			e.reopenAt(path)

			// Lost history. tie: A gets generation 2 (messages), B 3 (full).
			// gap/offline: B gets 2 (full), A 3 (messages).
			steps := []struct {
				p  policySpec
				on func() *Collector
			}{{spec(config.ModeMessages), onA}, {spec(config.ModeFull), onB}}
			if how != "tie" {
				steps = []struct {
					p  policySpec
					on func() *Collector
				}{{spec(config.ModeFull), onB}, {spec(config.ModeMessages), onA}}
			}
			for _, s := range steps {
				e.setPolicy(s.p)
				c := s.on()
				e.sync(c)
				e.deliverAll(c)
			}
			e.requireMode(dbB, config.ModeFull, 1, "B before the restore")
			if err := e.store.Close(); err != nil {
				t.Fatal(err)
			}

			// state.db is restored from the copy; the collector restarts under metrics on A.
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
			copyFile(t, backup, path)
			e.reopenAt(path)
			e.setPolicy(spec(config.ModeMetrics))
			a = onA()
			if gen, _, _ := e.store.Generation(ctx); gen != map[string]uint64{"tie": 3, "gap": 4, "offline": 4}[how] {
				t.Fatalf("generation %d after A's check", gen)
			}
			e.sync(a)
			e.deliverAll(a)
			e.requireMode(dbA, config.ModeMetrics, 1, "A after the restore")
			e.requireCounts(dbA, base, "A after the restore")

			// Then to B.
			if how == "offline" {
				sinkB.set("down")
				if _, err := e.store.Resolve(ctx, e.endpoint, dbB, idB); err != nil {
					t.Fatal(err)
				}
				e.sink = sinkB
				b = e.collectorFor(dbB, sinkB, e.src) // the check fails: B collects offline
				e.sync(b)
				if need, _, _ := e.store.NeedsResync(ctx, e.src.ID(), e.pol.Fingerprint()); need {
					t.Fatal("B's offline re-read should be complete")
				}
				sinkB.set("")
			}
			b = onB()
			e.sync(b)
			e.deliverAll(b)
			e.requireMode(dbB, config.ModeMetrics, 1, "B after the restore")
			gen, _, _ := e.store.Generation(ctx)
			e.requireLatestGeneration(dbB, gen, "B")
			e.requireCounts(dbB, base, "B after the restore")

			// Switching back and forth is stable.
			a = onA()
			e.sync(a)
			e.deliverAll(a)
			e.requireMode(dbA, config.ModeMetrics, 1, "A at the end")
			e.requireCounts(dbA, base, "A at the end")
			b = onB()
			e.sync(b)
			e.deliverAll(b)
			e.requireMode(dbB, config.ModeMetrics, 1, "B at the end")
			e.requireCounts(dbB, base, "B at the end")
		})
	}
}

func (e *privEnv) reopenAt(path string) {
	e.t.Helper()
	st, err := spool.Open(path, 64<<20)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	e.store = st
}
