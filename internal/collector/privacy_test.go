package collector

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// withPolicy restarts the harness's collector under other privacy settings.
// Failed entries are not due again for an hour.
func (h *harness) withPolicy(t *testing.T, mode string) *Collector {
	t.Helper()
	pc := config.Default().Privacy
	pc.Mode, pc.AllowSensitiveContent = mode, mode != config.ModeMetrics
	pol, err := privacy.New(pc)
	if err != nil {
		t.Fatal(err)
	}
	h.src.Close()
	h.src = t3code.New(h.fix.Path, pol, 3)
	t.Cleanup(func() { h.src.Close() })
	h.opts.PolicyFingerprint = pol.Fingerprint()
	h.opts.RetryInitial, h.opts.RetryMax = time.Hour, time.Hour
	return h.collector(t, h.opts)
}

// deliverAt delivers every entry due at now.
func deliverAt(t *testing.T, c *Collector, now time.Time) {
	t.Helper()
	for i := 0; i < 500; i++ {
		n, err := c.deliverRound(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("delivery did not finish")
}

// F1 on the fake database: a row of the messages mode whose insert landed
// without an acknowledgement is retried after the record was reprocessed
// under the metrics mode, and must not become its latest version.
func TestPolicyChangeOutranksQueuedRows(t *testing.T) {
	h := setup(t) // messages mode
	c := h.withPolicy(t, config.ModeMessages)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	deliverAt(t, c, time.Now())
	h.fix.Message("msg-f1", "thread-1", "run-1", "user", "SYNTH-MSG private", false)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	h.sink.mu.Lock()
	h.sink.ambiguousData = 100
	h.sink.mu.Unlock()
	if _, err := c.deliverRound(ctx, time.Now()); err == nil {
		t.Fatal("expected a lost response")
	}
	h.sink.mu.Lock()
	h.sink.ambiguousData = 0
	h.sink.mu.Unlock()

	c = h.withPolicy(t, config.ModeMetrics)
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	deliverAt(t, c, time.Now())                   // the reprocessed rows
	deliverAt(t, c, time.Now().Add(48*time.Hour)) // then the stale retry
	if st, _ := h.store.Stats(ctx); st.Entries != 0 {
		t.Fatalf("spool not drained: %+v", st)
	}
	stale := 0
	for _, r := range h.sink.rows[model.TableMessages] {
		if strings.Contains(r["text"].(string), "SYNTH") {
			stale++
		}
	}
	if stale < 2 {
		t.Fatalf("the stale row must have been stored twice (lost response, retry): %d", stale)
	}
	for table, key := range map[string]string{model.TableMessages: "message_id", model.TableConversations: "conversation_id", model.TableRawEvents: "event_id"} {
		for k, r := range h.sink.latest(table, key) {
			for col, v := range r {
				if s, ok := v.(string); ok && s != "" && (strings.Contains(s, "SYNTH") || (table == model.TableConversations && col == "title")) {
					t.Fatalf("latest %s %s exposes %s=%q under metrics", table, k, col, s)
				}
			}
		}
	}
	if g := model.Generation(h.sink.latest(model.TableMessages, "message_id")["msg-f1"][exactVersion].(uint64)); g != 2 {
		t.Fatalf("latest generation %d, want 2", g)
	}
}

// Nothing is delivered before the policy generation was verified against
// the database.
func TestDeliveryWaitsForGenerationCheck(t *testing.T) {
	h := setup(t)
	h.sink.maxVersionErr = &clickhouse.Error{Status: 503, Message: "unavailable", Transient: true}
	c := New(h.store, h.sink, []sources.Source{h.src}, h.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, h.src, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.deliverRound(ctx, time.Now()); err == nil || !strings.Contains(err.Error(), "policy generation") {
		t.Fatalf("expected the generation check to stop delivery, got %v", err)
	}
	if n := len(h.sink.inserts); n != 0 {
		t.Fatalf("%d inserts before the check", n)
	}
	h.sink.mu.Lock()
	h.sink.maxVersionErr = nil
	h.sink.mu.Unlock()
	deliverAt(t, c, time.Now())
	if h.sink.count(model.TableMessages) == 0 {
		t.Fatal("delivery must resume once the check succeeds")
	}
}

// A database holding rows of a later generation than the spool (for
// example of a deleted spool) is noticed before delivery, and every record
// is queued again above it.
func TestGenerationRaisedAboveDatabase(t *testing.T) {
	h := setup(t)
	// Rows of an earlier spool under generation 7.
	h.sink.rows[model.TableMessages] = append(h.sink.rows[model.TableMessages], map[string]any{
		"source_id": h.src.ID(), "message_id": "msg-user-1", "text": "SYNTH-FOREIGN", exactVersion: uint64(7)<<model.GenerationShift | 1<<44 | 99})
	c, logs := h.logged(t)
	if gen, _, _ := h.store.Generation(ctx); gen != 8 {
		t.Fatalf("generation %d, want 8", gen)
	}
	if !strings.Contains(logs.String(), "later privacy policy generation") {
		t.Fatalf("the raise must be logged: %s", logs)
	}
	if err := c.syncOnce(ctx, h.src, c.log); err != nil {
		t.Fatal(err)
	}
	deliverAt(t, c, time.Now())
	if r := h.sink.latest(model.TableMessages, "message_id")["msg-user-1"]; strings.Contains(r["text"].(string), "SYNTH") {
		t.Fatalf("the earlier spool's row still wins: %v", r)
	}
}
