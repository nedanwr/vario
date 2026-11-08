package collector

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

type fakeSink struct {
	mu          sync.Mutex
	rows        map[string][]map[string]any
	tokens      map[string]bool
	failNext    int
	failErr     error
	schemaCalls int
}

func newSink() *fakeSink {
	return &fakeSink{rows: map[string][]map[string]any{}, tokens: map[string]bool{}}
}

func (s *fakeSink) EnsureSchema(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemaCalls++
	return nil
}

func (s *fakeSink) Insert(_ context.Context, table string, body []byte, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return s.failErr
	}
	if s.tokens[token] {
		return nil
	}
	s.tokens[token] = true
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(zr)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return err
		}
		s.rows[table] = append(s.rows[table], m)
	}
	return nil
}

func (s *fakeSink) count(table string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows[table])
}

// latest returns the highest-version row per key, like ReplacingMergeTree FINAL.
func (s *fakeSink) latest(table, key string) map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]any{}
	for _, r := range s.rows[table] {
		k := r[key].(string)
		if cur, ok := out[k]; !ok || r["version"].(float64) >= cur["version"].(float64) {
			out[k] = r
		}
	}
	return out
}

type harness struct {
	fix   *t3codetest.Fixture
	store *spool.Store
	sink  *fakeSink
	src   *t3code.Source
	c     *Collector
}

func setup(t *testing.T) *harness {
	t.Helper()
	fix := t3codetest.New(t)
	fix.Standard()
	pc := config.Default().Privacy
	pc.Mode = config.ModeMessages
	pc.AllowSensitiveContent = true
	pol, err := privacy.New(pc)
	if err != nil {
		t.Fatal(err)
	}
	store, err := spool.Open(filepath.Join(t.TempDir(), "state.db"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	src := t3code.New(fix.Path, pol, 3)
	t.Cleanup(func() { src.Close() })
	sink := newSink()
	c := New(store, sink, []sources.Source{src}, Options{
		PollInterval: 20 * time.Millisecond, ReconcileInterval: time.Hour, BatchSize: 4,
		FlushInterval: 20 * time.Millisecond, MaxInsertRows: 7, RetryInitial: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
		PolicyFingerprint: pol.Fingerprint(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &harness{fix: fix, store: store, sink: sink, src: src, c: c}
}

var ctx = context.Background()

func TestBackfillDeliversOnceAndIsIdempotent(t *testing.T) {
	h := setup(t)
	res, err := h.c.Snapshot(ctx, h.src, false, true)
	if err != nil || res.Enqueued == 0 {
		t.Fatalf("snapshot: %+v %v", res, err)
	}
	dr, err := h.c.Drain(ctx)
	if err != nil || dr.Remaining != 0 || dr.Delivered != res.Enqueued {
		t.Fatalf("drain: %+v %v (enqueued %d)", dr, err, res.Enqueued)
	}
	if h.sink.count(model.TableMessages) != 2 || h.sink.count(model.TableProviderTurns) != 1 || h.sink.count(model.TableConversations) != 1 {
		t.Fatalf("unexpected rows: %v", h.sink.rows)
	}

	res2, err := h.c.Snapshot(ctx, h.src, false, false)
	if err != nil || res2.Enqueued != 0 {
		t.Fatalf("re-snapshot of unchanged data should enqueue nothing: %+v %v", res2, err)
	}
	res3, err := h.c.Snapshot(ctx, h.src, true, true)
	if err != nil || res3.Enqueued != res.Enqueued {
		t.Fatalf("forced snapshot should re-send everything: %+v vs %+v", res3, res)
	}
}

func TestDeliveryRetriesWithoutLoss(t *testing.T) {
	h := setup(t)
	h.sink.failNext = 3
	h.sink.failErr = &clickhouse.Error{Status: 503, Message: "unavailable", Transient: true}
	res, err := h.c.Snapshot(ctx, h.src, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); !ok {
		t.Fatal("cursor should be stored once data is durably spooled, even if delivery fails")
	}
	dr, err := h.c.Drain(ctx)
	if err == nil || dr.Remaining == 0 {
		t.Fatalf("expected failed drain with remaining rows: %+v %v", dr, err)
	}
	st, _ := h.store.Stats(ctx)
	if st.FailingEntries == 0 || st.LastError == "" {
		t.Fatalf("failures should be recorded: %+v", st)
	}
	for i := 0; i < 5; i++ {
		if dr, err = h.c.Drain(ctx); err == nil {
			break
		}
	}
	if err != nil || dr.Remaining != 0 {
		t.Fatalf("drain after recovery: %+v %v", dr, err)
	}
	total := 0
	for _, tbl := range model.Tables {
		total += h.sink.count(tbl)
	}
	if total != res.Enqueued {
		t.Fatalf("delivered %d rows, enqueued %d", total, res.Enqueued)
	}
}

func TestUnknownTableTriggersSchemaRecreation(t *testing.T) {
	h := setup(t)
	if _, err := h.c.Snapshot(ctx, h.src, false, false); err != nil {
		t.Fatal(err)
	}
	h.sink.failNext = 1
	h.sink.failErr = &clickhouse.Error{Status: 404, Code: 60, Message: "unknown table"}
	_, _ = h.c.deliverRound(ctx, time.Now().Add(time.Hour))
	if _, err := h.c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if h.sink.schemaCalls < 2 {
		t.Fatalf("schema should be re-ensured after an unknown-table error (calls=%d)", h.sink.schemaCalls)
	}
}

type failingSource struct {
	sources.Source
	fail bool
}

func (f *failingSource) Snapshot(ctx context.Context, prev string, history bool, emit func([]model.Record) error) (string, error) {
	if !f.fail {
		return f.Source.Snapshot(ctx, prev, history, emit)
	}
	n := 0
	_, err := f.Source.Snapshot(ctx, prev, history, func(r []model.Record) error {
		if n++; n == 3 {
			return errors.New("interrupted")
		}
		return emit(r)
	})
	return "", err
}

func TestInterruptedSnapshotDoesNotAdvanceCursor(t *testing.T) {
	h := setup(t)
	fs := &failingSource{Source: h.src, fail: true}
	if _, err := h.c.Snapshot(ctx, fs, false, true); err == nil {
		t.Fatal("expected interruption")
	}
	if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); ok {
		t.Fatal("cursor must not be stored after an interrupted snapshot")
	}
	fs.fail = false
	res, err := h.c.Snapshot(ctx, fs, false, true)
	if err != nil || res.Skipped == 0 {
		t.Fatalf("resumed snapshot should skip already-queued records: %+v %v", res, err)
	}
	if _, ok, _ := h.store.Cursor(ctx, h.src.ID()); !ok {
		t.Fatal("cursor should be stored after a complete snapshot")
	}
}

func TestRunSyncsIncrementallyAndStopsGracefully(t *testing.T) {
	h := setup(t)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.c.Run(runCtx) }()

	waitFor(t, func() bool { return h.sink.count(model.TableMessages) == 2 })
	h.fix.Message("msg-live", "thread-1", "run-1", "assistant", "streaming...", true)
	waitFor(t, func() bool { _, ok := h.sink.latest(model.TableMessages, "message_id")["msg-live"]; return ok })
	h.fix.Message("msg-live", "thread-1", "run-1", "assistant", "final answer", false)
	waitFor(t, func() bool {
		m := h.sink.latest(model.TableMessages, "message_id")["msg-live"]
		return m != nil && m["text"] == "final answer"
	})
	// A duplicate notification for an unchanged entity must not produce another row.
	before := h.sink.count(model.TableMessages)
	h.fix.TouchThread("thread-1", "2026-04-01T00:00:00.000Z")
	time.Sleep(200 * time.Millisecond)
	if h.sink.count(model.TableMessages) != before {
		t.Fatal("unrelated events must not re-deliver unchanged messages")
	}
	if h.sink.count(model.TableConversations) != 1 {
		t.Fatalf("volatile thread updates must not create new conversation versions: %d", h.sink.count(model.TableConversations))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	cur, _, _ := h.store.Cursor(ctx, h.src.ID())
	var maxSeq int64
	_ = h.fix.DB.QueryRow(`SELECT MAX(sequence) FROM orchestration_events`).Scan(&maxSeq)
	var c struct{ Seq int64 }
	_ = json.Unmarshal([]byte(cur), &c)
	if c.Seq != maxSeq {
		t.Fatalf("cursor %d, want %d", c.Seq, maxSeq)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestBackoff(t *testing.T) {
	if d := backoff(time.Second, time.Minute, 1); d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Fatalf("attempt 1: %v", d)
	}
	if d := backoff(time.Second, time.Minute, 30); d > 66*time.Second {
		t.Fatalf("backoff must be capped: %v", d)
	}
}
