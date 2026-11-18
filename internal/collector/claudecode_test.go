package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/claudecode"
	cct "github.com/nedanwr/vario/internal/sources/claudecode/claudecodetest"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
	"github.com/nedanwr/vario/internal/spool"
)

type dualHarness struct {
	t3     *t3codetest.Fixture
	cc     *cct.Fixture
	main   *cct.Session
	store  *spool.Store
	sink   *fakeSink
	t3src  *t3code.Source
	ccsrc  *claudecode.Source
	policy *privacy.Policy
	opts   Options
}

func dualSetup(t *testing.T, mode string) *dualHarness {
	t.Helper()
	h := &dualHarness{t3: t3codetest.New(t), cc: cct.New(t), sink: newSink()}
	h.t3.Standard()
	h.main = h.cc.Session(cct.DefaultCwd)
	h.main.Conversation("a")
	var err error
	if h.store, err = spool.Open(filepath.Join(t.TempDir(), "state.db"), 64<<20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	h.setPolicy(t, mode)
	return h
}

func (h *dualHarness) setPolicy(t *testing.T, mode string) {
	t.Helper()
	pc := config.Default().Privacy
	pc.Mode = mode
	pc.AllowSensitiveContent = mode != config.ModeMetrics
	pol, err := privacy.New(pc)
	if err != nil {
		t.Fatal(err)
	}
	h.policy = pol
	h.t3src = t3code.New(h.t3.Path, pol, 3)
	t.Cleanup(func() { h.t3src.Close() })
	h.ccsrc = claudecode.New(h.cc.Config, pol, 3)
	h.opts = Options{
		PollInterval: 20 * time.Millisecond, ReconcileInterval: time.Hour, BatchSize: 4,
		FlushInterval: 20 * time.Millisecond, MaxInsertRows: 7, RetryInitial: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
		PolicyFingerprint: pol.Fingerprint(), Endpoint: "http://127.0.0.1:8123", Database: "vario",
	}
}

func (h *dualHarness) collector(t *testing.T) *Collector {
	t.Helper()
	c := New(h.store, h.sink, []sources.Source{h.t3src, h.ccsrc}, h.opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	return c
}

// latestOf returns the latest rows of one source.
func (h *dualHarness) latestOf(table, key, sourceID string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, r := range h.sink.latest(table, key) {
		if r["source_id"] == sourceID {
			out[k] = r
		}
	}
	return out
}

func (h *dualHarness) outputTokens(sourceID string) float64 {
	var sum float64
	for _, r := range h.latestOf(model.TableProviderTurns, "provider_turn_id", sourceID) {
		if v, ok := r["output_tokens"].(float64); ok {
			sum += v
		}
	}
	return sum
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Both collectors run together; Claude Code activity is collected while it
// is being written, and a restart re-sends nothing.
func TestRunCollectsT3CodeAndClaudeCodeTogether(t *testing.T) {
	h := dualSetup(t, config.ModeMetrics)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	c := h.collector(t)
	go func() { done <- c.Run(runCtx) }()

	ccID := h.ccsrc.ID()
	waitUntil(t, "both backfills", func() bool {
		return len(h.latestOf(model.TableProviderTurns, "provider_turn_id", ccID)) == 2 &&
			len(h.latestOf(model.TableProviderTurns, "provider_turn_id", h.t3src.ID())) == 1
	})
	t3Before := h.latestOf(model.TableMessages, "message_id", h.t3src.ID())

	// Claude Code keeps writing: a response line by line, a tool call whose
	// result arrives later, and a second session started concurrently.
	h.main.Append(h.main.Prompt("more"))
	lines := h.main.Response("msg_live", cct.DefaultModel, "tool_use", cct.Usage{Input: 1, CacheCreate: 2, CacheRead: 3, Output: 50},
		cct.Thinking("t"), cct.Text("x"), cct.ToolUse("toolu_live", "Bash", map[string]any{"command": "make"}))
	for _, l := range lines {
		h.main.Append(l)
		time.Sleep(15 * time.Millisecond)
	}
	second := h.cc.Session("/work/other")
	second.Conversation("b")
	waitUntil(t, "the running tool", func() bool {
		r := h.sink.latest(model.TableExecutionEvents, "event_id")["toolu_live"]
		return r != nil && r["status"] == "running"
	})
	h.main.Append(h.main.ToolResult("toolu_live", "ok", false))
	h.main.Append(h.main.Response("msg_live2", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 5}, cct.Text("done"))...)
	waitUntil(t, "live activity", func() bool {
		r := h.sink.latest(model.TableExecutionEvents, "event_id")["toolu_live"]
		return r != nil && r["status"] == "completed" && len(h.latestOf(model.TableProviderTurns, "provider_turn_id", ccID)) == 6
	})
	// 200+40 per Conversation, twice, plus the live responses.
	if got := h.outputTokens(ccID); got != 2*(200+40)+50+5 {
		t.Fatalf("claude code output tokens: %v", got)
	}
	if got := h.latestOf(model.TableMessages, "message_id", h.t3src.ID()); len(got) != len(t3Before) {
		t.Fatalf("t3 code messages changed: %d -> %d", len(t3Before), len(got))
	}
	waitUntil(t, "an empty spool", func() bool { st, _ := h.store.Stats(ctx); return st.Rows == 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A restart finds nothing new to send.
	inserts := len(h.sink.inserts)
	runCtx, cancel = context.WithCancel(ctx)
	c = h.collector(t)
	go func() { done <- c.Run(runCtx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(h.sink.inserts) - inserts; n != 0 {
		var tables []string
		for _, in := range h.sink.inserts[inserts:] {
			tables = append(tables, in.table)
		}
		t.Fatalf("a restart without new activity inserted %d batches: %v", n, tables)
	}
	if st, _ := h.store.Stats(ctx); st.Rows != 0 {
		t.Fatalf("spool not drained: %+v", st)
	}
}

// Rows collected while ClickHouse is offline are delivered once it is back,
// and repeated backfills do not inflate results.
func TestClaudeCodeOfflineDeliveryAndRepeatedBackfills(t *testing.T) {
	h := dualSetup(t, config.ModeMetrics)
	c := h.collector(t)
	h.sink.failNext = 1 << 30
	h.sink.failErr = errors.New("connection refused")
	if _, err := c.Snapshot(ctx, h.ccsrc, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err == nil {
		t.Fatal("delivery should fail while offline")
	}
	// More activity while offline, collected incrementally.
	s := h.cc.Session(cct.DefaultCwd)
	s.Conversation("off")
	if err := c.syncOnce(ctx, h.ccsrc, c.log); err != nil {
		t.Fatal(err)
	}
	h.sink.failNext = 0
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	want := float64(2 * (200 + 40))
	if got := h.outputTokens(h.ccsrc.ID()); got != want {
		t.Fatalf("output tokens after recovery: %v, want %v", got, want)
	}
	rows := h.sink.count(model.TableProviderTurns)
	for i := 0; i < 3; i++ {
		if _, err := c.Snapshot(ctx, h.ccsrc, false, false); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if h.sink.count(model.TableProviderTurns) != rows {
		t.Fatal("repeated backfills re-sent unchanged provider turns")
	}
	// A forced backfill re-sends everything, but logical results stay the same.
	if _, err := c.Snapshot(ctx, h.ccsrc, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.outputTokens(h.ccsrc.ID()); got != want || len(h.latestOf(model.TableProviderTurns, "provider_turn_id", h.ccsrc.ID())) != 4 {
		t.Fatalf("forced backfill changed the logical results: %v", got)
	}
}

// A privacy change re-reads Claude Code transcripts in full under the new
// policy, and the new rows win regardless of delivery order.
func TestClaudeCodePrivacyModeChange(t *testing.T) {
	h := dualSetup(t, config.ModeFull)
	c := h.collector(t)
	if err := c.syncOnce(ctx, h.ccsrc, c.log); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.contains(cct.ToolOutMarker) {
		t.Fatal("full mode should collect tool output")
	}
	h.setPolicy(t, config.ModeMetrics)
	c = h.collector(t)
	if err := c.syncOnce(ctx, h.ccsrc, c.log); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []struct{ name, key string }{
		{model.TableMessages, "message_id"}, {model.TableExecutionEvents, "event_id"}, {model.TableRawEvents, "event_id"},
		{model.TableConversations, "conversation_id"}, {model.TableProjects, "project_id"},
	} {
		for k, r := range h.latestOf(table.name, table.key, h.ccsrc.ID()) {
			s := fmt.Sprint(r)
			for _, m := range []string{cct.PromptMarker, cct.ReplyMarker, cct.ToolOutMarker, cct.ToolInMarker, cct.ThinkingMarker, cct.PathMarker} {
				if strings.Contains(s, m) {
					t.Fatalf("%s/%s still holds %s after switching to metrics: %s", table.name, k, m, s)
				}
			}
		}
	}
}

func (h *dualHarness) contains(marker string) bool {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	for _, rows := range h.sink.rows {
		for _, r := range rows {
			if strings.Contains(fmt.Sprint(r), marker) {
				return true
			}
		}
	}
	return false
}
