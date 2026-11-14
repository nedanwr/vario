package t3code_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

func policy(t *testing.T, mode string) *privacy.Policy {
	t.Helper()
	c := config.Default().Privacy
	c.Mode = mode
	c.AllowSensitiveContent = mode != config.ModeMetrics
	p, err := privacy.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type collected struct {
	byTable map[string][]model.Record
}

func (c *collected) emit(recs []model.Record) error {
	for _, r := range recs {
		c.byTable[r.Table] = append(c.byTable[r.Table], r)
	}
	return nil
}

func snapshot(t *testing.T, src *t3code.Source, prev string, history bool) (*collected, string) {
	t.Helper()
	c := &collected{byTable: map[string][]model.Record{}}
	cur, err := src.Snapshot(context.Background(), prev, history, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	return c, cur
}

func find[T model.Row](t *testing.T, recs []model.Record, key string) T {
	t.Helper()
	for _, r := range recs {
		if r.Key == key {
			return r.Row.(T)
		}
	}
	t.Fatalf("record %q not found", key)
	var zero T
	return zero
}

func cursorSeq(t *testing.T, c string) int64 {
	var v struct {
		Epoch int64 `json:"epoch"`
		Seq   int64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(c), &v); err != nil {
		t.Fatal(err)
	}
	return v.Seq
}

func TestDetect(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	d := t3code.Detect(context.Background(), []string{f.Path, filepath.Join(t.TempDir(), "missing.sqlite")})
	if !d[0].Compatible || !d[0].Exists {
		t.Fatalf("fixture not detected as compatible: %+v", d[0])
	}
	if d[1].Exists || d[1].Compatible || d[1].Detail != "not found" {
		t.Fatalf("missing path: %+v", d[1])
	}
	if _, err := f.DB.Exec(`DROP TABLE orchestration_v2_projection_turn_items`); err != nil {
		t.Fatal(err)
	}
	d = t3code.Detect(context.Background(), []string{f.Path})
	if d[0].Compatible || !strings.Contains(d[0].Detail, "missing table orchestration_v2_projection_turn_items") {
		t.Fatalf("incompatible schema not reported: %+v", d[0])
	}
}

func TestSourceIDUsesEnvironmentID(t *testing.T) {
	f := t3codetest.New(t)
	if got := t3code.SourceID(f.Path); got != "t3code:env-fixture-0001" {
		t.Fatalf("SourceID = %q", got)
	}
	other := filepath.Join(t.TempDir(), "statev2.sqlite")
	a, b := t3code.SourceID(other), t3code.SourceID(other)
	if a != b || !strings.HasPrefix(a, "t3code:") {
		t.Fatalf("fallback id not stable: %q %q", a, b)
	}
}

func TestSnapshotMetricsMode(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 2)
	defer src.Close()
	c, cur := snapshot(t, src, "", true)

	counts := map[string]int{}
	for k, v := range c.byTable {
		counts[k] = len(v)
	}
	want := map[string]int{model.TableSources: 1, model.TableProjects: 1, model.TableConversations: 1, model.TableRuns: 1,
		model.TableMessages: 2, model.TableExecutionEvents: 4, model.TableProviderTurns: 1}
	for table, n := range want {
		if counts[table] != n {
			t.Errorf("%s: got %d records, want %d", table, counts[table], n)
		}
	}
	var maxSeq int64
	_ = f.DB.QueryRow(`SELECT MAX(sequence) FROM orchestration_events`).Scan(&maxSeq)
	if counts[model.TableRawEvents] != int(maxSeq) {
		t.Errorf("raw events: got %d, want %d", counts[model.TableRawEvents], maxSeq)
	}
	if cursorSeq(t, cur) != maxSeq {
		t.Errorf("cursor seq %d, want %d", cursorSeq(t, cur), maxSeq)
	}

	conv := find[*model.ConversationRow](t, c.byTable[model.TableConversations], "thread-1")
	if conv.Title != "" || conv.Branch != "" {
		t.Errorf("metrics mode leaked title/branch: %+v", conv)
	}
	if conv.Model != "claude-test-1" || conv.ProjectID != "proj-1" || conv.RootConversationID != "thread-1" {
		t.Errorf("conversation fields: %+v", conv)
	}
	if strings.Contains(conv.Attributes, "lastVisitedAt") || strings.Contains(conv.Attributes, "worktreePath") {
		t.Errorf("attributes contain volatile or content fields: %s", conv.Attributes)
	}
	if conv.Version != uint64(1)<<44|uint64(maxSeq) {
		t.Errorf("version = %d", conv.Version)
	}

	msg := find[*model.MessageRow](t, c.byTable[model.TableMessages], "msg-user-1")
	if msg.Text != "" || msg.ContentIncluded != 0 || msg.TextChars == 0 {
		t.Errorf("metrics message: %+v", msg)
	}
	if strings.Contains(msg.Attachments, "secret-plan") || msg.AttachmentCount != 1 || !strings.Contains(msg.Attachments, "image/png") {
		t.Errorf("metrics attachments: %q", msg.Attachments)
	}
	if !msg.ConversationCreatedAt.Equal(conv.CreatedAt.Time) {
		t.Errorf("conversation_created_at %v != %v", msg.ConversationCreatedAt, conv.CreatedAt)
	}

	cmd := find[*model.ExecutionEventRow](t, c.byTable[model.TableExecutionEvents], "item-cmd-1")
	if cmd.Input != "" || cmd.Output != "" || cmd.Title != "" || cmd.InputBytes == 0 || cmd.OutputBytes == 0 {
		t.Errorf("metrics command content: %+v", cmd)
	}
	if cmd.ExitCode == nil || *cmd.ExitCode != 1 || cmd.IsError != 1 || cmd.DurationMs == nil || *cmd.DurationMs != 2500 {
		t.Errorf("command metadata: exit=%v err=%d dur=%v", cmd.ExitCode, cmd.IsError, cmd.DurationMs)
	}
	tool := find[*model.ExecutionEventRow](t, c.byTable[model.TableExecutionEvents], "item-tool-1")
	if tool.ToolName != "Read" || tool.Input != "" || tool.IsError != 0 {
		t.Errorf("tool: %+v", tool)
	}
	for _, r := range c.byTable[model.TableRawEvents] {
		if ev := r.Row.(*model.RawEventRow); ev.Payload != "" || ev.PayloadIncluded != 0 {
			t.Fatalf("metrics mode raw payload included: %+v", ev)
		}
	}
}

func TestMessagesAreNotDuplicatedByTurnItems(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeFull), 100)
	defer src.Close()
	c, _ := snapshot(t, src, "", false)
	item := find[*model.ExecutionEventRow](t, c.byTable[model.TableExecutionEvents], "item-user-1")
	if item.MessageID != "msg-user-1" || item.Input != "" || item.Output != "" || strings.Contains(item.Attributes, "fix login") {
		t.Fatalf("message turn item should reference the message without duplicating text: %+v", item)
	}
	if len(c.byTable[model.TableMessages]) != 2 {
		t.Fatalf("messages: %d", len(c.byTable[model.TableMessages]))
	}
}

func TestSnapshotFullModeRedacts(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeFull), 100)
	defer src.Close()
	c, _ := snapshot(t, src, "", true)

	msg := find[*model.MessageRow](t, c.byTable[model.TableMessages], "msg-user-1")
	if !strings.Contains(msg.Text, "Please fix login") || strings.Contains(msg.Text, "sk-ant-api03") || msg.Redactions != 1 {
		t.Errorf("full message text not redacted: %q (%d)", msg.Text, msg.Redactions)
	}
	if !strings.Contains(msg.Attachments, "secret-plan.png") {
		t.Errorf("full mode should keep attachment metadata: %s", msg.Attachments)
	}
	cmd := find[*model.ExecutionEventRow](t, c.byTable[model.TableExecutionEvents], "item-cmd-1")
	if strings.Contains(cmd.Input+cmd.Output, "hunter2secret") || !strings.Contains(cmd.Output, "config.go") || cmd.ContentIncluded != 1 {
		t.Errorf("command content: in=%q out=%q", cmd.Input, cmd.Output)
	}
	tool := find[*model.ExecutionEventRow](t, c.byTable[model.TableExecutionEvents], "item-tool-1")
	if !strings.Contains(tool.Input, "login.go") || tool.Output != "package main" {
		t.Errorf("tool content: %+v", tool)
	}
	conv := find[*model.ConversationRow](t, c.byTable[model.TableConversations], "thread-1")
	if conv.Title != "Fix the login bug" || conv.Branch != "feature/x" {
		t.Errorf("conversation title: %+v", conv)
	}
	raw := 0
	for _, r := range c.byTable[model.TableRawEvents] {
		ev := r.Row.(*model.RawEventRow)
		if ev.PayloadIncluded == 1 {
			raw++
			if !json.Valid([]byte(ev.Payload)) || strings.Contains(ev.Payload, "sk-ant-api03") {
				t.Fatalf("raw payload invalid or unredacted: %s", ev.Payload)
			}
		}
	}
	if raw == 0 {
		t.Error("full mode should include raw payloads")
	}
}

func TestTokenAccounting(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	f.ProviderTurn("turn-unavailable", "thread-1", "run-1:attempt:1", "interrupted",
		map[string]any{"usedTokens": 100, "maxTokens": 1000, "updatedAt": "2026-03-01T10:01:00.000Z"},
		map[string]any{"usageScope": "main_agent", "hasSubagents": false, "usageStatus": "unavailable"}, nil)
	f.ProviderTurn("turn-none", "thread-1", "", "running", nil, nil, nil)
	f.ProviderTurn("turn-models", "thread-1", "run-1:attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 100, "cachedInputTokens": 60, "outputTokens": 10,
			"models": []string{"claude-reported-2"}, "otherUsage": map[string]any{"inputTokens": 0, "outputTokens": 0}}, nil)

	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 100)
	defer src.Close()
	c, _ := snapshot(t, src, "", false)
	turns := c.byTable[model.TableProviderTurns]

	full := find[*model.ProviderTurnRow](t, turns, "turn-1")
	eq := func(name string, got *uint64, want uint64) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %d", name, got, want)
		}
	}
	eq("input", full.InputTokens, 818885)
	eq("cached", full.CachedInputTokens, 745279)
	eq("cache_creation", full.CacheCreationInputTokens, 73580)
	eq("uncached", full.UncachedInputTokens, 818885-745279-73580)
	eq("output (reasoning not added)", full.OutputTokens, 17272)
	eq("reasoning", full.ReasoningTokens, 10136)
	eq("context used", full.ContextUsedTokens, 83311)
	eq("context input", full.ContextInputTokens, 83307)
	eq("context cached", full.ContextCachedInputTokens, 79716)
	if full.UsageSource != "t3code.turnTokenUsage" || full.UsageStatus != "complete" || full.UsageScope != "main_agent" {
		t.Errorf("usage provenance: %+v", full)
	}
	if full.RunID != "run-1" || full.Model != "claude-test-1" || full.ModelSource != "run" || full.Provider != "claudeAgent" {
		t.Errorf("run attribution: run=%q model=%q source=%q provider=%q", full.RunID, full.Model, full.ModelSource, full.Provider)
	}
	if !strings.Contains(full.Attributes, `"turnTokenUsage"`) {
		t.Errorf("raw usage should be preserved for provenance: %s", full.Attributes)
	}

	un := find[*model.ProviderTurnRow](t, turns, "turn-unavailable")
	if un.InputTokens != nil || un.OutputTokens != nil || un.UncachedInputTokens != nil || un.UsageStatus != "unavailable" {
		t.Errorf("unavailable usage must stay null: %+v", un)
	}
	eq("unavailable context used", un.ContextUsedTokens, 100)
	if un.ContextInputTokens != nil {
		t.Errorf("missing context input should be null")
	}

	none := find[*model.ProviderTurnRow](t, turns, "turn-none")
	if none.UsageSource != "" || none.InputTokens != nil || none.ContextUsedTokens != nil || none.Models == nil {
		t.Errorf("turn without usage: %+v", none)
	}

	m := find[*model.ProviderTurnRow](t, turns, "turn-models")
	if m.Model != "claude-reported-2" || m.ModelSource != "usage" {
		t.Errorf("reported model should win: %q (%s)", m.Model, m.ModelSource)
	}
	if m.UncachedInputTokens != nil {
		t.Errorf("uncached must not be derived when cache creation is unreported")
	}
	eq("other input", m.OtherInputTokens, 0)
}

func TestChangesFollowEventsWithGapsAndDuplicates(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeMessages), 100)
	defer src.Close()
	_, cur := snapshot(t, src, "", false)

	cs, err := src.Changes(context.Background(), cur, 100)
	if err != nil || len(cs.Records) != 0 || cs.Cursor != cur || cs.Reconcile != "" {
		t.Fatalf("no-op changes: %+v %v", cs, err)
	}

	f.SkipSequences(50)
	f.Message("msg-asst-2", "thread-1", "run-1", "assistant", "Partial", true)
	f.SkipSequences(7)
	f.Message("msg-asst-2", "thread-1", "run-1", "assistant", "Partial answer, now complete.", false)
	// A partial provider-turn event: only tokenUsage in the payload. The
	// projection still carries the full state, which must be what is emitted.
	f.ProviderTurn("turn-1", "thread-1", "run-1:attempt:1", "completed",
		map[string]any{"usedTokens": 90000, "maxTokens": 1000000, "inputTokens": 89990, "cachedInputTokens": 80000, "outputTokens": 10},
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 900000, "cachedInputTokens": 800000, "cacheCreationTokens": 80000, "outputTokens": 20000, "reasoningTokens": 1},
		map[string]any{"id": "turn-1", "tokenUsage": map[string]any{"usedTokens": 90000}})

	cs, err = src.Changes(context.Background(), cur, 100)
	if err != nil {
		t.Fatal(err)
	}
	var msgs, turns, raws int
	for _, r := range cs.Records {
		switch r.Table {
		case model.TableMessages:
			msgs++
			m := r.Row.(*model.MessageRow)
			if m.Text != "Partial answer, now complete." || m.Streaming != 0 {
				t.Errorf("message should reflect latest projected state: %+v", m)
			}
			if m.ConversationCreatedAt.IsZero() {
				t.Error("conversation_created_at not resolved for incremental record")
			}
		case model.TableProviderTurns:
			turns++
			pt := r.Row.(*model.ProviderTurnRow)
			if pt.InputTokens == nil || *pt.InputTokens != 900000 || pt.RunID != "run-1" {
				t.Errorf("provider turn should be re-read from projection: %+v", pt)
			}
		case model.TableRawEvents:
			raws++
		}
	}
	if msgs != 1 || turns != 1 || raws != 3 {
		t.Fatalf("got msgs=%d turns=%d raws=%d", msgs, turns, raws)
	}
	if cursorSeq(t, cs.Cursor) <= cursorSeq(t, cur)+57 || cs.More {
		t.Fatalf("cursor did not advance past the gap: %s", cs.Cursor)
	}
	if v := cs.Records[0].Row.Env().Version; v <= uint64(1)<<44|uint64(cursorSeq(t, cur)) {
		t.Errorf("incremental version %d must exceed snapshot version", v)
	}
}

func TestChangesRespectProjectionBoundaryAndPaging(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 100)
	defer src.Close()
	_, cur := snapshot(t, src, "", false)
	base := cursorSeq(t, cur)

	f.AutoProject = false
	f.Message("msg-x", "thread-1", "run-1", "user", "not yet projected", false)
	cs, err := src.Changes(context.Background(), cur, 100)
	if err != nil || len(cs.Records) != 0 || cursorSeq(t, cs.Cursor) != base {
		t.Fatalf("events beyond the projector high-water mark must not be consumed: %+v %v", cs, err)
	}

	f.AutoProject = true
	for i := 0; i < 5; i++ {
		f.TouchThread("thread-1", "2026-03-02T00:00:0"+string(rune('0'+i))+".000Z")
	}
	var all []string
	c := cur
	for i := 0; i < 10; i++ {
		cs, err := src.Changes(context.Background(), c, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range cs.Records {
			if r.Table == model.TableRawEvents {
				all = append(all, r.Key)
			}
		}
		c = cs.Cursor
		if !cs.More {
			break
		}
	}
	if len(all) != 6 {
		t.Fatalf("paged raw events: got %d (%v), want 6", len(all), all)
	}
}

func TestChangesRequestReconcile(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 100)
	defer src.Close()
	_, cur := snapshot(t, src, "", false)
	seq := cursorSeq(t, cur)

	cs, err := src.Changes(context.Background(), "", 10)
	if err != nil || cs.Reconcile == "" {
		t.Fatalf("empty cursor should request reconcile: %+v", cs)
	}

	f.Message("msg-new", "thread-1", "run-1", "user", "hello", false)
	f.SetMetadata("event-log-prune-mark", seq+1)
	cs, err = src.Changes(context.Background(), cur, 10)
	if err != nil || !strings.Contains(cs.Reconcile, "compacted") {
		t.Fatalf("prune mark beyond cursor should request reconcile: %+v %v", cs, err)
	}

	future := strings.Replace(cur, `"seq":`, `"seq":9`, 1)
	cs, err = src.Changes(context.Background(), future, 10)
	if err != nil || !strings.Contains(cs.Reconcile, "restarted") {
		t.Fatalf("cursor beyond log should request reconcile: %+v %v", cs, err)
	}
	c, next := snapshot(t, src, future, false)
	if !strings.Contains(next, `"epoch":2`) {
		t.Fatalf("snapshot after reset must bump epoch: %s", next)
	}
	if v := c.byTable[model.TableConversations][0].Row.Env().Version; v>>44 != 2 {
		t.Fatalf("version epoch = %d", v>>44)
	}
}

func TestSourceNeverWritesDatabase(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	before := f.Bytes()
	src := t3code.New(f.Path, policy(t, config.ModeFull), 3)
	_, cur := snapshot(t, src, "", true)
	if _, err := src.Changes(context.Background(), cur, 10); err != nil {
		t.Fatal(err)
	}
	t3code.Detect(context.Background(), []string{f.Path})
	src.Close()
	if !bytes.Equal(before, f.Bytes()) {
		t.Fatal("source database was modified")
	}
}

func TestUnavailableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	src := t3code.New(path, policy(t, config.ModeMetrics), 10)
	defer src.Close()
	if _, err := src.Snapshot(context.Background(), "", false, func([]model.Record) error { return nil }); err == nil {
		t.Fatal("expected error for missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("source must not create a missing database")
	}
}

func TestSchemaChangeIsDetected(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 100)
	defer src.Close()
	_, cur := snapshot(t, src, "", false)
	if _, err := f.DB.Exec(`ALTER TABLE orchestration_v2_projection_messages DROP COLUMN payload_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Changes(context.Background(), cur, 10); err == nil || !strings.Contains(err.Error(), "payload_json") {
		t.Fatalf("schema change must be re-inspected and rejected: %v", err)
	}
}

// Metrics mode must not export project titles or workspace paths, and raw
// event metadata must be reduced to known operational keys outside full mode.
func TestMetricsModeHasNoProjectNamesOrUnknownMetadata(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	f.EventWithMetadata("thread.visited", "thread-1", map[string]any{"id": "thread-1"},
		map[string]any{"driver": "claudeAgent", "origin": map[string]any{"surface": "web", "appVersion": "1.2.3"},
			"note": "customer acme-merger password=hunter2secret"})
	src := t3code.New(f.Path, policy(t, config.ModeMetrics), 100)
	defer src.Close()
	c, _ := snapshot(t, src, "", true)

	pr := find[*model.ProjectRow](t, c.byTable[model.TableProjects], "proj-1")
	if pr.Title != "" || pr.WorkspaceRoot != "" {
		t.Errorf("metrics mode leaked project title/path: %+v", pr)
	}
	sawOrigin := false
	for _, r := range c.byTable[model.TableRawEvents] {
		ev := r.Row.(*model.RawEventRow)
		if strings.Contains(ev.Metadata, "acme-merger") || strings.Contains(ev.Metadata, "hunter2") {
			t.Fatalf("unknown metadata keys must be dropped outside full mode: %s", ev.Metadata)
		}
		if strings.Contains(ev.Metadata, `"surface":"web"`) && strings.Contains(ev.Metadata, `"driver":"claudeAgent"`) {
			sawOrigin = true
		}
	}
	if !sawOrigin {
		t.Error("known operational metadata should be kept")
	}

	full := t3code.New(f.Path, policy(t, config.ModeFull), 100)
	defer full.Close()
	c, _ = snapshot(t, full, "", true)
	for _, r := range c.byTable[model.TableRawEvents] {
		ev := r.Row.(*model.RawEventRow)
		if strings.Contains(ev.Metadata, "hunter2secret") {
			t.Fatalf("full mode metadata must be redacted: %s", ev.Metadata)
		}
	}
	pr = find[*model.ProjectRow](t, c.byTable[model.TableProjects], "proj-1")
	if pr.Title != "demo" || pr.WorkspaceRoot != "/home/dev/demo" {
		t.Errorf("full mode should keep project title/path: %+v", pr)
	}
}

// Attributes kept outside full mode are redacted too.
func TestAttributesAreRedacted(t *testing.T) {
	f := t3codetest.New(t)
	f.Standard()
	f.ThreadPayload("thread-1", map[string]any{"pullRequests": []any{map[string]any{"url": "https://bob:hunter2pass@git.example.com/pr/1"}}})
	src := t3code.New(f.Path, policy(t, config.ModeMessages), 100)
	defer src.Close()
	c, _ := snapshot(t, src, "", false)
	conv := find[*model.ConversationRow](t, c.byTable[model.TableConversations], "thread-1")
	if !strings.Contains(conv.Attributes, "pullRequests") || strings.Contains(conv.Attributes, "hunter2pass") {
		t.Fatalf("attributes not redacted: %s", conv.Attributes)
	}
}

// A replaced database whose event log already extends past the stored cursor
// is detected by the cursor's event-id anchor, and the snapshot that follows
// starts a new version epoch so the replacement's state wins.
func TestReplacedDatabaseWithLongerLogIsDetected(t *testing.T) {
	a := t3codetest.New(t)
	a.Standard()
	srcA := t3code.New(a.Path, policy(t, config.ModeMetrics), 100)
	defer srcA.Close()
	_, cur := snapshot(t, srcA, "", false)

	b := t3codetest.New(t)
	b.Standard()
	for i := 0; i < 5; i++ {
		b.TouchThread("thread-1", "2026-05-01T00:00:00.000Z")
	}
	if _, err := b.DB.Exec(`UPDATE orchestration_events SET event_id = 'other-' || event_id`); err != nil {
		t.Fatal(err)
	}
	srcB := t3code.New(b.Path, policy(t, config.ModeMetrics), 100)
	defer srcB.Close()
	cs, err := srcB.Changes(context.Background(), cur, 100)
	if err != nil || !strings.Contains(cs.Reconcile, "replaced") {
		t.Fatalf("replacement not detected: %+v %v", cs, err)
	}
	_, next := snapshot(t, srcB, cur, false)
	if !strings.Contains(next, `"epoch":2`) {
		t.Fatalf("snapshot after replacement must start a new epoch: %s", next)
	}

	// No false positive when the anchored event was merely pruned.
	_, cur = snapshot(t, srcA, "", false)
	a.Message("msg-new", "thread-1", "run-1", "user", "hello", false)
	if _, err := a.DB.Exec(`DELETE FROM orchestration_events WHERE sequence = ?`, cursorSeq(t, cur)); err != nil {
		t.Fatal(err)
	}
	cs, err = srcA.Changes(context.Background(), cur, 100)
	if err != nil || cs.Reconcile != "" || len(cs.Records) == 0 {
		t.Fatalf("pruned anchor must not force a reset: %+v %v", cs, err)
	}
}
