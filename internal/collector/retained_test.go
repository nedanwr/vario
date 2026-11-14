package collector

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// keptAfterReplacement leaves rows queued for a replaced database (id-A) and
// accepts the replacement (id-B) as `vario destination switch --keep-pending`
// does. It returns the replaced destination, with its kept rows, and the new,
// active one.
func keptAfterReplacement(t *testing.T) (*harness, spool.Destination, spool.Destination) {
	t.Helper()
	h, c, _, queued := replacedWithBufferedRows(t)
	oldID := c.Destination().ID
	repl, err := h.store.Replace(ctx, oldID, "id-B")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Activate(ctx, repl.ID); err != nil {
		t.Fatal(err)
	}
	h.store.SelectDestination(repl.ID)
	old, ok, err := h.store.Destination(ctx, oldID)
	if err != nil || !ok || old.Pending != queued || old.Retired.IsZero() || old.Cursors == 0 {
		t.Fatalf("the rows must be kept for the replaced destination: %+v %v", old, err)
	}
	return h, old, repl
}

func (h *harness) retained(t *testing.T, id int64) spool.Destination {
	t.Helper()
	d, ok, err := h.store.Destination(ctx, id)
	if err != nil || !ok {
		t.Fatalf("destination %d: %v %v", id, ok, err)
	}
	return d
}

// unprepared builds a collector as a new process would, without selecting a
// destination: a retry must not depend on, or change, the active one.
func (h *harness) unprepared(opts Options) *Collector {
	return New(h.store, h.sink, nil, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Rows kept for a replaced database are retried only at that database: not
// at the one that replaced it, not while it cannot be identified. Once it is
// back they are delivered there, an interrupted retry resumes after a
// restart, and repeating it sends nothing twice. The active destination and
// every cursor stay as they are.
func TestRetainedRowsAreRetriedOnlyAtTheirOwnDatabase(t *testing.T) {
	h, old, repl := keptAfterReplacement(t)
	queued := old.Pending
	messages := h.sink.count(model.TableMessages)
	unchanged := func(what string) {
		t.Helper()
		if d := h.retained(t, old.ID); d.Pending != queued || d.Cursors != old.Cursors || d.ServerID != "id-A" {
			t.Fatalf("%s: the kept rows and their state must be untouched: %+v", what, d)
		}
		if n := h.sink.count(model.TableMessages); n != messages {
			t.Fatalf("%s: %d rows were delivered", what, n-messages)
		}
		if a, ok, _ := h.store.Active(ctx); !ok || a.ID != repl.ID {
			t.Fatalf("%s: the active destination changed: %+v", what, a)
		}
		if h.store.Selected() != repl.ID {
			t.Fatalf("%s: the selected destination changed", what)
		}
		if _, ok, _ := h.store.Replaced(ctx); ok {
			t.Fatalf("%s: a refused retry must not report a replacement", what)
		}
	}

	// The database that replaced it carries another id.
	res, err := h.unprepared(h.opts).DeliverRetained(ctx, old)
	if !errors.Is(err, spool.ErrIdentityMismatch) || res.Delivered != 0 || res.Remaining != queued {
		t.Fatalf("retry at the replacing database: %+v %v", res, err)
	}
	unchanged("another database")

	// Another endpoint or database is never used for them.
	other := h.opts
	other.Database = "other"
	h.setServerID("id-A")
	if res, err := h.unprepared(other).DeliverRetained(ctx, old); err == nil || res.Delivered != 0 {
		t.Fatalf("retry with another database configured: %+v %v", res, err)
	}
	unchanged("another configuration")

	// Offline: the database cannot be identified, so nothing is sent.
	h.sink.mu.Lock()
	h.sink.schemaErr = errors.New("dial tcp: connection refused")
	h.sink.mu.Unlock()
	res, err = h.unprepared(h.opts).DeliverRetained(ctx, old)
	if !errors.Is(err, ErrDestinationCheck) || res.Delivered != 0 || res.Remaining != queued {
		t.Fatalf("retry while offline: %+v %v", res, err)
	}
	unchanged("offline")

	// It lost its id: not verifiable either.
	h.sink.mu.Lock()
	h.sink.schemaErr = nil
	h.sink.mu.Unlock()
	h.setServerID("")
	if res, err = h.unprepared(h.opts).DeliverRetained(ctx, old); !errors.Is(err, spool.ErrIdentityMissing) || res.Delivered != 0 {
		t.Fatalf("retry without a destination id: %+v %v", res, err)
	}
	unchanged("no destination id")

	// The original database is back, but delivery breaks off part-way.
	h.setServerID("id-A")
	h.store.SelectDestination(old.ID)
	due, err := h.store.Due(ctx, time.Now().Add(time.Hour), 1000)
	h.store.SelectDestination(repl.ID)
	if err != nil || len(due) < 2 || due[0].Table == due[len(due)-1].Table {
		t.Fatalf("the fixture must keep rows of several tables: %d entries %v", len(due), err)
	}
	h.sink.mu.Lock()
	h.sink.failTable, h.sink.failErr = due[len(due)-1].Table, &clickhouse.Error{Status: 503, Message: "unavailable", Transient: true}
	h.sink.mu.Unlock()
	res, err = h.unprepared(h.opts).DeliverRetained(ctx, old)
	if err == nil || res.Delivered == 0 || res.Remaining == 0 || int64(res.Delivered)+res.Remaining != queued {
		t.Fatalf("an interrupted retry must keep exactly the undelivered rows: %+v %v (queued %d)", res, err, queued)
	}
	if d := h.retained(t, old.ID); d.Pending != res.Remaining || d.Cursors != old.Cursors {
		t.Fatalf("after an interrupted retry: %+v", d)
	}
	h.store.SelectDestination(old.ID)
	st, _ := h.store.Stats(ctx)
	h.store.SelectDestination(repl.ID)
	if st.FailingEntries == 0 || st.LastError == "" {
		t.Fatalf("the failure must be recorded on the kept rows: %+v", st)
	}

	// After a restart the retry continues with what is left.
	h.sink.mu.Lock()
	h.sink.failTable = ""
	h.sink.mu.Unlock()
	left := res.Remaining
	res, err = h.unprepared(h.opts).DeliverRetained(ctx, h.retained(t, old.ID))
	if err != nil || int64(res.Delivered) != left || res.Remaining != 0 {
		t.Fatalf("resumed retry: %+v %v (left %d)", res, err, left)
	}
	if n := h.sink.count(model.TableMessages); n != messages+1 {
		t.Fatalf("the buffered message must arrive exactly once: %d new rows", n-messages)
	}
	if _, ok := h.sink.latest(model.TableMessages, "message_id")["msg-buffered"]; !ok {
		t.Fatal("the buffered message is missing in its database")
	}

	// Repeating the retry sends nothing again.
	res, err = h.unprepared(h.opts).DeliverRetained(ctx, h.retained(t, old.ID))
	if err != nil || res.Delivered != 0 || res.Remaining != 0 || h.sink.count(model.TableMessages) != messages+1 {
		t.Fatalf("repeated retry: %+v %v", res, err)
	}
	// Its state still describes it correctly; the active destination is untouched.
	if d := h.retained(t, old.ID); d.Cursors != old.Cursors || d.Retired.IsZero() || d.ServerID != "id-A" {
		t.Fatalf("the retried destination's state must be kept: %+v", d)
	}
	if a, ok, _ := h.store.Active(ctx); !ok || a.ID != repl.ID || h.store.Selected() != repl.ID {
		t.Fatalf("the active destination changed: %+v", a)
	}
}
