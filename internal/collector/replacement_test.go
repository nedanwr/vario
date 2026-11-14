package collector

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/spool"
)

// syncBuffer collects log output from the collector's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logged builds a prepared collector on the harness that logs into the
// returned buffer.
func (h *harness) logged(t *testing.T) (*Collector, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	c := New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(logs, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	return c, logs
}

func (h *harness) setServerID(id string) {
	h.sink.mu.Lock()
	h.sink.serverID = id
	h.sink.mu.Unlock()
}

// emptySink makes the sink an empty database, as after DROP and CREATE.
func (h *harness) emptySink() {
	h.sink.mu.Lock()
	h.sink.rows, h.sink.tokens, h.sink.checkpoints = map[string][]map[string]any{}, map[string]bool{}, map[string]uint64{}
	h.sink.mu.Unlock()
}

// replacedWithBufferedRows synchronizes the source and delivers everything to
// the database with destination id id-A, queues one more message that is not
// delivered yet, and then replaces the database (id-B) with the next identity
// check due. It returns the collector, its log and the queued row count.
func replacedWithBufferedRows(t *testing.T) (*harness, *Collector, *syncBuffer, int64) {
	t.Helper()
	h := setup(t)
	h.setServerID("id-A")
	c, logs := h.logged(t)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("drain: %+v %v", dr, err)
	}
	h.fix.Message("msg-buffered", "thread-1", "run-1", "assistant", "collected before the replacement was noticed", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued := pending(t, h)
	if queued == 0 {
		t.Fatal("expected buffered rows")
	}
	// The database is replaced; the next periodic identity check is due.
	h.setServerID("id-B")
	c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	return h, c, logs, queued
}

// C-2: when the periodic identity check finds another database, nothing more
// is delivered, every buffered row stays in the spool, the recorded identity
// is kept, and the replacement is reported with the recovery steps. Only the
// explicit switch makes the new database a destination, and whatever is
// decided about the buffered rows, it then receives the complete history.
func TestReplacementSuspendsDeliveryAndKeepsBufferedData(t *testing.T) {
	for _, policy := range []string{"move", "discard", "keep"} {
		t.Run(policy, func(t *testing.T) {
			h, c, logs, queued := replacedWithBufferedRows(t)
			before := h.sink.count(model.TableMessages)
			oldID := c.Destination().ID

			var de *DestinationError
			var fe *fatalError
			_, err := c.deliverRound(ctx, time.Now())
			if !errors.As(err, &fe) || !errors.As(err, &de) || !de.Replaced || de.Found != "id-B" || de.Configured.ServerID != "id-A" || de.Configured.Pending != queued {
				t.Fatalf("delivery must stop with a replaced-destination error: %v (%+v)", err, de)
			}
			for _, want := range []string{"was replaced", "destination id id-A", "now carries id-B", "Delivery is suspended",
				"vario destination switch --move-pending", "vario destination switch --discard-pending", "vario destination switch --keep-pending",
				"reads every source again in full", "vario migrate"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the error must explain the recovery (missing %q):\n%s", want, err)
				}
			}
			// Suspended, not retried: later rounds and a forced drain deliver nothing.
			for i := 0; i < 3; i++ {
				if _, err := c.deliverRound(ctx, time.Now().Add(time.Hour)); !errors.As(err, &fe) {
					t.Fatalf("delivery must stay suspended: %v", err)
				}
			}
			if _, err := c.Drain(ctx); !errors.As(err, &de) {
				t.Fatalf("drain must stay suspended: %v", err)
			}
			if n := h.sink.count(model.TableMessages); n != before {
				t.Fatalf("rows were delivered to the replaced database: %d, had %d", n, before)
			}
			if got := pending(t, h); got != queued {
				t.Fatalf("buffered rows must be kept: %d of %d", got, queued)
			}
			// Reported for logs, status and doctor; the identity is not overwritten.
			if l := logs.String(); !strings.Contains(l, "ClickHouse database was replaced; delivery is suspended and queued rows are kept") ||
				!strings.Contains(l, "recorded_destination_id=id-A") || !strings.Contains(l, "found_destination_id=id-B") {
				t.Fatalf("the replacement must be logged:\n%s", l)
			}
			rep, ok, err := h.store.Replaced(ctx)
			if err != nil || !ok || rep.Destination.ID != oldID || rep.Destination.ServerID != "id-A" || rep.Found != "id-B" || rep.Destination.Pending != queued {
				t.Fatalf("the replacement must be recorded: %+v %v %v", rep, ok, err)
			}
			if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); !strings.Contains(v, "id-A") || !strings.Contains(v, "id-B") || !strings.Contains(v, "vario destination switch") {
				t.Fatalf("status should name both ids and the command: %q", v)
			}

			// A restart does not authorize the new database either.
			restarted := New(h.store, h.sink, []sources.Source{h.src}, h.opts, c.log)
			if _, err := restarted.Prepare(ctx, true); !errors.As(err, &de) || !de.Replaced || de.Found != "id-B" {
				t.Fatalf("a restart must be refused: %v", err)
			}
			if got := pending(t, h); got != queued || h.sink.count(model.TableMessages) != before {
				t.Fatal("a refused restart must not deliver or drop anything")
			}

			// `vario destination switch`: the explicit decision.
			res, err := h.store.Resolve(ctx, h.opts.Endpoint, h.opts.Database, "id-B")
			if err != nil || !res.Replaced || res.Configured.ID != oldID {
				t.Fatalf("resolve: %+v %v", res, err)
			}
			repl, err := h.store.Replace(ctx, oldID, "id-B")
			if err != nil {
				t.Fatal(err)
			}
			switch policy {
			case "move":
				if n, err := h.store.MovePending(ctx, oldID, repl.ID); err != nil || n != queued {
					t.Fatalf("move: %d %v", n, err)
				}
			case "discard":
				if n, err := h.store.DiscardPending(ctx, oldID); err != nil || n != queued {
					t.Fatalf("discard: %d %v", n, err)
				}
			}
			if err := h.store.Activate(ctx, repl.ID); err != nil {
				t.Fatal(err)
			}
			h.emptySink() // the new database holds nothing of the old one
			if _, ok, _ := h.store.Replaced(ctx); ok {
				t.Fatal("the switch must resolve the reported replacement")
			}

			fresh := h.collector(t, h.opts)
			if fresh.Destination().ID != repl.ID || fresh.Destination().ServerID != "id-B" {
				t.Fatalf("the collector must use the new destination: %+v", fresh.Destination())
			}
			if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); ok {
				t.Fatal("the new destination must not inherit a cursor")
			}
			if err := fresh.syncOnce(ctx, h.src, fresh.log); err != nil {
				t.Fatal(err)
			}
			if dr, err := fresh.Drain(ctx); err != nil || dr.Remaining != 0 {
				t.Fatalf("drain: %+v %v", dr, err)
			}
			// Complete history, whatever happened to the buffered rows: the two
			// messages delivered before the replacement and the buffered one.
			got := h.sink.latest(model.TableMessages, "message_id")
			for _, id := range []string{"msg-user-1", "msg-asst-1", "msg-buffered"} {
				if _, ok := got[id]; !ok {
					t.Fatalf("the new database is incomplete: %d of 3 messages, %s missing", len(got), id)
				}
			}
			if len(got) != 3 {
				t.Fatalf("the new database must hold every message once, got %d", len(got))
			}
			if n := len(h.sink.latest(model.TableConversations, "conversation_id")); n != 1 {
				t.Fatalf("conversations in the new database: %d", n)
			}
			old, _, _ := h.store.Destination(ctx, oldID)
			if want := map[string]int64{"move": 0, "discard": 0, "keep": queued}[policy]; old.Pending != want || old.Retired.IsZero() {
				t.Fatalf("rows left with the replaced destination: %d, want %d (%+v)", old.Pending, want, old)
			}
		})
	}
}

// C-2: an idle collector (nothing queued, no insert that could fail) notices
// the replacement by its periodic check, stops, and says why.
func TestReplacementWhileIdleStopsCollectorAndIsReported(t *testing.T) {
	old := identityCheckInterval
	identityCheckInterval = 30 * time.Millisecond
	t.Cleanup(func() { identityCheckInterval = old })

	h := setup(t)
	h.setServerID("id-A")
	c, logs := h.logged(t)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { return h.sink.count(model.TableMessages) == 2 && pending(t, h) == 0 })
	time.Sleep(3 * identityCheckInterval) // idle; periodic checks pass
	if _, ok, _ := h.store.Replaced(ctx); ok {
		t.Fatal("no replacement may be reported for an unchanged database")
	}
	delivered := h.sink.count(model.TableMessages)

	h.setServerID("id-B")
	var de *DestinationError
	select {
	case err := <-done:
		if !errors.As(err, &de) || !de.Replaced || de.Found != "id-B" {
			t.Fatalf("Run should stop with a replaced-destination error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an idle collector did not notice the replaced database")
	}
	if n := h.sink.count(model.TableMessages); n != delivered {
		t.Fatalf("rows were delivered after the replacement: %d, had %d", n, delivered)
	}
	l := logs.String()
	for _, want := range []string{"ClickHouse database was replaced", "delivery stopped", "vario destination switch --move-pending"} {
		if !strings.Contains(l, want) {
			t.Fatalf("log misses %q:\n%s", want, l)
		}
	}
	if rep, ok, _ := h.store.Replaced(ctx); !ok || rep.Found != "id-B" || rep.Destination.ServerID != "id-A" {
		t.Fatalf("the replacement must be recorded for status and doctor: %+v %v", rep, ok)
	}
	if _, at, ok, _ := h.store.KV(ctx, "collector.stopped_at"); !ok || at.IsZero() {
		t.Fatal("the collector must record that it stopped")
	}
}

// If the recorded database comes back (the URL pointed elsewhere for a
// while, or it was restored), no switch is needed: the collector resumes,
// delivers what it kept, and the report is withdrawn.
func TestOriginalDatabaseBackResumesDelivery(t *testing.T) {
	h, c, _, queued := replacedWithBufferedRows(t)
	if _, err := c.deliverRound(ctx, time.Now()); err == nil {
		t.Fatal("expected the replacement to be detected")
	}
	h.setServerID("id-A")
	resumed, logs := h.logged(t)
	if _, ok, _ := h.store.Replaced(ctx); ok {
		t.Fatal("the report must be withdrawn once the recorded database is back")
	}
	if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); v != "" {
		t.Fatalf("the stale error must be cleared: %q", v)
	}
	if !strings.Contains(logs.String(), "carries the recorded destination id again") {
		t.Fatalf("resuming should be logged:\n%s", logs.String())
	}
	if resumed.Destination().ID != c.Destination().ID {
		t.Fatal("the same destination must be used")
	}
	dr, err := resumed.Drain(ctx)
	if err != nil || int64(dr.Delivered) != queued || dr.Remaining != 0 {
		t.Fatalf("kept rows must be delivered: %+v %v (queued %d)", dr, err, queued)
	}
	if n := len(h.sink.latest(model.TableMessages, "message_id")); n != 3 {
		t.Fatalf("messages: %d", n)
	}
}

// A database that no longer reports the recorded destination id (vario_meta
// dropped or unreadable) cannot be verified. Nothing is delivered and nothing
// is lost, but it is not declared replaced: once the id is readable again,
// delivery continues.
func TestLostDestinationIDSuspendsDelivery(t *testing.T) {
	h := setup(t)
	h.setServerID("id-A")
	c := h.collector(t, h.opts)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	h.fix.Message("msg-new", "thread-1", "run-1", "assistant", "new", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	queued, before := pending(t, h), h.sink.count(model.TableMessages)

	h.setServerID("")
	c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	var fe *fatalError
	dr, err := c.Drain(ctx)
	if !errors.Is(err, spool.ErrIdentityMissing) || errors.As(err, &fe) || !strings.Contains(err.Error(), "vario migrate") || dr.Remaining != queued {
		t.Fatalf("delivery must wait for the identity: %+v %v", dr, err)
	}
	if h.sink.count(model.TableMessages) != before {
		t.Fatal("nothing may be delivered to an unverified database")
	}
	if _, ok, _ := h.store.Replaced(ctx); ok {
		t.Fatal("an unreadable id is not a replacement")
	}
	if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); !strings.Contains(v, "id-A") || !strings.Contains(v, "vario_meta") {
		t.Fatalf("status should explain it: %q", v)
	}
	// A restart behaves the same: it starts, but does not deliver.
	restarted := h.collector(t, h.opts)
	if _, err := restarted.Drain(ctx); !errors.Is(err, spool.ErrIdentityMissing) || h.sink.count(model.TableMessages) != before {
		t.Fatalf("a restart must not deliver either: %v", err)
	}
	if d, _, _ := h.store.Destination(ctx, c.Destination().ID); d.ServerID != "id-A" {
		t.Fatalf("the recorded id must be kept: %+v", d)
	}

	h.setServerID("id-A")
	if dr, err := restarted.Drain(ctx); err != nil || int64(dr.Delivered) != queued || dr.Remaining != 0 {
		t.Fatalf("delivery must continue once the id is back: %+v %v", dr, err)
	}
	if v, _, _, _ := h.store.KV(ctx, "delivery.last_error"); v != "" {
		t.Fatalf("the error must be cleared: %q", v)
	}
}

// A database migrated by Vario 0.1 has no vario_meta and therefore no
// destination id. `vario migrate` adds one; a running collector binds it at
// its next check and keeps its cursors (no second backfill). From then on a
// replacement is detected like for any other destination.
func TestLegacyDestinationAcquiresIdentity(t *testing.T) {
	h := setup(t) // the sink reports no destination id, like a version 1 schema
	c := h.collector(t, h.opts)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := h.store.Destination(ctx, c.Destination().ID); d.ServerID != "" {
		t.Fatalf("no identity is known yet: %+v", d)
	}
	cursor, _, _ := h.store.Cursor(ctx, h.src.ID())
	delivered := h.sink.count(model.TableMessages)

	// `vario migrate` records the destination id while the collector runs.
	h.setServerID("id-legacy")
	c.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	if _, err := c.deliverRound(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, _, _ := h.store.Destination(ctx, c.Destination().ID)
	if d.ServerID != "id-legacy" || !d.Retired.IsZero() {
		t.Fatalf("the identity must be bound to the existing destination: %+v", d)
	}
	if _, ok, _ := h.store.Replaced(ctx); ok {
		t.Fatal("acquiring an identity is not a replacement")
	}
	// Persistent across restarts, with the synchronization state intact.
	restarted := h.collector(t, h.opts)
	if restarted.Destination().ID != d.ID || restarted.Destination().ServerID != "id-legacy" {
		t.Fatalf("restart: %+v", restarted.Destination())
	}
	if cur, _, _ := h.store.Cursor(ctx, h.src.ID()); cur != cursor {
		t.Fatal("binding the identity must keep the cursor")
	}
	if err := restarted.syncOnce(ctx, h.src, restarted.log); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Drain(ctx); err != nil || h.sink.count(model.TableMessages) != delivered {
		t.Fatalf("nothing may be sent again: %v (%d rows, had %d)", err, h.sink.count(model.TableMessages), delivered)
	}

	// The identity now protects the destination.
	h.setServerID("id-other")
	restarted.checkedAt = time.Now().Add(-2 * identityCheckInterval)
	var de *DestinationError
	if _, err := restarted.deliverRound(ctx, time.Now()); !errors.As(err, &de) || !de.Replaced {
		t.Fatalf("a replacement must be detected after the identity was acquired: %v", err)
	}
}
