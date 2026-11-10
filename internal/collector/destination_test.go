package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

func pending(t *testing.T, h *harness) int64 {
	t.Helper()
	st, err := h.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st.Rows
}

func TestDestinationChangeWithPendingDataIsRefused(t *testing.T) {
	h := setup(t)
	if _, err := h.c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	queued := pending(t, h)
	if queued == 0 {
		t.Fatal("expected queued rows")
	}
	// The database changes (config set / edit / direct TOML edit) while rows are queued.
	opts := h.opts
	opts.Database = "vario_new"
	c := New(h.store, h.sink, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := c.Prepare(ctx, true)
	var de *DestinationError
	if !errors.As(err, &de) || de.Replaced || de.Previous == nil || de.Previous.Pending != queued {
		t.Fatalf("expected a refused destination change, got %v", err)
	}
	for _, s := range []string{"vario destination switch --keep-pending", "--move-pending", "--discard-pending"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("the error should explain %q: %v", s, err)
		}
	}
	// Nothing can be delivered or collected through the unprepared collector.
	if _, err := c.Drain(ctx); err == nil {
		t.Fatal("an unprepared collector must not deliver")
	}
	if n := h.sink.count(model.TableMessages); n != 0 {
		t.Fatalf("queued rows must not be delivered to the new destination: %d", n)
	}
	// An endpoint change is a destination change too.
	opts = h.opts
	opts.Endpoint = "https://other.example.com:8443"
	c = New(h.store, h.sink, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); !errors.As(err, &de) {
		t.Fatalf("endpoint change: %v", err)
	}
	// The original destination still works and delivers its rows.
	c = h.collector(t, h.opts)
	if dr, err := c.Drain(ctx); err != nil || dr.Remaining != 0 || int64(dr.Delivered) != queued {
		t.Fatalf("drain to the original destination: %+v %v", dr, err)
	}
}

func TestDestinationSwitchIsolatesCursorsAndFingerprints(t *testing.T) {
	h := setup(t)
	first, err := h.c.Snapshot(ctx, h.src, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	oldCursor, _, _ := h.store.Cursor(ctx, h.src.ID())

	// Nothing is queued, so a new destination is accepted automatically, and it
	// has no cursor: the source is read again in full for it.
	opts := h.opts
	opts.Database = "vario_b"
	b := h.collector(t, opts)
	if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); ok {
		t.Fatal("a new destination must not inherit the previous destination's cursor")
	}
	res, err := b.Snapshot(ctx, h.src, false, true)
	if err != nil || res.Enqueued != first.Enqueued {
		t.Fatalf("the new destination must receive a full backfill (fingerprints are per destination): %+v, first %+v, %v", res, first, err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	// Switching back resumes the original destination's state: nothing is re-sent.
	a := h.collector(t, h.opts)
	if cur, ok, _ := h.store.Cursor(ctx, h.src.ID()); !ok || cur != oldCursor {
		t.Fatalf("switching back must restore the cursor: %q %v", cur, ok)
	}
	if res, err := a.Snapshot(ctx, h.src, false, false); err != nil || res.Enqueued != 0 {
		t.Fatalf("unchanged records must not be re-sent to the original destination: %+v %v", res, err)
	}

	// A collector restarted on destination B continues where B left off.
	b2 := h.collector(t, opts)
	if res, err := b2.Snapshot(ctx, h.src, false, false); err != nil || res.Enqueued != 0 {
		t.Fatalf("restart on destination B must keep its fingerprints: %+v %v", res, err)
	}
}

func TestDestinationIdentityByServerID(t *testing.T) {
	h := setup(t)
	h.sink.serverID = "id-A"
	h.c = h.collector(t, h.opts)
	if _, err := h.c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if d := h.c.Destination(); d.ServerID != "id-A" {
		t.Fatalf("destination id should be recorded: %+v", d)
	}
	cursor, _, _ := h.store.Cursor(ctx, h.src.ID())

	// The same database under another URL keeps its state (alias).
	opts := h.opts
	opts.Endpoint = "http://localhost:8123"
	c := New(h.store, h.sink, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := c.Prepare(ctx, true)
	if err != nil || !res.Aliased || res.Configured.ID != h.c.Destination().ID {
		t.Fatalf("alias: %+v %v", res, err)
	}
	if cur, _, _ := h.store.Cursor(ctx, h.src.ID()); cur != cursor {
		t.Fatal("an alias must keep the cursor")
	}

	// The database was dropped and recreated (new destination id): refused at startup.
	h.sink.serverID = "id-B"
	c = New(h.store, h.sink, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var de *DestinationError
	if _, err := c.Prepare(ctx, true); !errors.As(err, &de) || !de.Replaced {
		t.Fatalf("replaced database at startup: %v", err)
	}

	// Detected while running (unreachable at startup): the collector stops
	// instead of delivering, and keeps the data.
	h.sink.schemaErr = &clickhouse.Error{Message: "dial tcp: connection refused", Transient: true}
	c = New(h.store, h.sink, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatalf("offline start must succeed: %v", err)
	}
	h.fix.Message("msg-new", "thread-1", "run-1", "assistant", "new", false)
	h.sink.schemaErr = nil
	before := h.sink.count(model.TableMessages)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		if !errors.As(err, &de) || !de.Replaced {
			t.Fatalf("Run should stop with a replaced-destination error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on a replaced destination")
	}
	if h.sink.count(model.TableMessages) != before {
		t.Fatal("nothing may be delivered to a replaced destination")
	}
}

func TestCredentialRotationKeepsDestination(t *testing.T) {
	a := config.ClickHouseConfig{URL: "https://CH.example.com/", Database: "vario", Username: "vario"}
	b := config.ClickHouseConfig{URL: "https://ch.example.com:443", Database: "vario", Username: "vario_v2"}
	if a.Endpoint() != b.Endpoint() {
		t.Fatalf("endpoint normalization: %q vs %q", a.Endpoint(), b.Endpoint())
	}
	h := setup(t)
	if _, err := h.c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	// Options carry no credentials: a new user or password yields the same destination.
	c := h.collector(t, h.opts)
	if c.Destination().ID != h.c.Destination().ID {
		t.Fatal("the same endpoint and database must be the same destination")
	}
}

func TestMissingSchemaKeepsDataSpooled(t *testing.T) {
	h := setup(t)
	h.sink.schemaErr = &clickhouse.MigrationRequiredError{Database: "vario", Missing: model.Tables}
	h.c = New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := h.c.Prepare(ctx, true); err != nil {
		t.Fatalf("a missing schema must not prevent collection: %v", err)
	}
	if _, err := h.c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	dr, err := h.c.Drain(ctx)
	if err == nil || !strings.Contains(err.Error(), "vario migrate") || dr.Remaining == 0 {
		t.Fatalf("delivery must stop with a pointer to vario migrate and keep the data: %+v %v", dr, err)
	}
	v, _, _, _ := h.store.KV(ctx, "delivery.last_error")
	if !strings.Contains(v, "vario migrate") {
		t.Fatalf("status should show the reason: %q", v)
	}
	h.sink.schemaErr = nil
	if dr, err := h.c.Drain(ctx); err != nil || dr.Remaining != 0 {
		t.Fatalf("after migrating, the spooled data is delivered: %+v %v", dr, err)
	}
}

// ddlRecorder is a minimal ClickHouse HTTP endpoint that answers inspection
// queries with an empty database and records every statement.
type ddlRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (d *ddlRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ping" {
		io.WriteString(w, "Ok.\n")
		return
	}
	b, _ := io.ReadAll(r.Body)
	q := string(b)
	if qq := r.URL.Query().Get("query"); qq != "" {
		q = qq
	}
	d.mu.Lock()
	d.queries = append(d.queries, q)
	d.mu.Unlock()
	switch {
	case strings.Contains(q, "system.databases"):
		io.WriteString(w, `{"data":[{"n":1}]}`)
	case strings.HasPrefix(q, "INSERT"):
		w.Header().Set("X-ClickHouse-Exception-Code", "60")
		w.WriteHeader(404)
		io.WriteString(w, "Code: 60. Unknown table")
	default:
		io.WriteString(w, `{"data":[]}`)
	}
}

func (d *ddlRecorder) mutating() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, q := range d.queries {
		u := strings.ToUpper(strings.TrimSpace(q))
		for _, p := range []string{"CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "RENAME", "TRUNCATE", "INSERT", "OPTIMIZE", "EXCHANGE", "ATTACH", "DETACH", "SET "} {
			if strings.HasPrefix(u, p) {
				out = append(out, q)
			}
		}
	}
	return out
}

// The collector, with the real ClickHouse client, never issues DDL: a
// missing schema is reported and data stays spooled.
func TestRuntimeIssuesNoDDL(t *testing.T) {
	rec := &ddlRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	cfg := config.Default().ClickHouse
	cfg.URL, cfg.Database, cfg.CreateDatabase = srv.URL, "vario", true // the deprecated setting must have no effect
	client, err := clickhouse.New(cfg, "pw")
	if err != nil {
		t.Fatal(err)
	}
	h := setup(t)
	opts := h.opts
	opts.Endpoint, opts.Database = cfg.Endpoint(), cfg.Database
	c := New(h.store, client, []sources.Source{h.src}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = c.Run(runCtx)
	dr, err := c.Drain(ctx)
	var me *clickhouse.MigrationRequiredError
	if !errors.As(err, &me) || dr.Remaining == 0 {
		t.Fatalf("expected a migration-required error with data kept: %+v %v", dr, err)
	}
	if m := rec.mutating(); len(m) != 0 {
		t.Fatalf("the collector issued statements that change ClickHouse: %v", m)
	}
	if len(rec.queries) == 0 {
		t.Fatal("expected inspection queries")
	}
}

// `vario destination switch` moves (or discards) queued rows and then
// activates the configured destination in separate atomic steps. If it is
// interrupted in between, nothing is lost or misdirected: the moved rows
// belong to the configured destination, and the next start switches to it.
func TestInterruptedSwitchLosesNothing(t *testing.T) {
	h := setup(t)
	if _, err := h.c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	queued := pending(t, h)
	opts := h.opts
	opts.Database = "vario_b"
	res, err := h.store.Resolve(ctx, opts.Endpoint, opts.Database, "")
	if err != nil || res.Previous == nil {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	n, err := h.store.MovePending(ctx, res.Previous.ID, res.Configured.ID)
	if err != nil || n != queued {
		t.Fatalf("move: %d of %d, %v", n, queued, err)
	}
	// Interrupted here, before Activate. The next start accepts B (nothing is
	// queued for A any more) and delivers the moved rows to B.
	b := h.collector(t, opts)
	dr, err := b.Drain(ctx)
	if err != nil || int64(dr.Delivered) != queued || dr.Remaining != 0 {
		t.Fatalf("moved rows after an interrupted switch: %+v %v (queued %d)", dr, err, queued)
	}
}
