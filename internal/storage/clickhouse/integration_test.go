package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
)

// Integration tests run only when VARIO_TEST_CLICKHOUSE_URL is set, e.g.:
//
//	docker run -d -p 127.0.0.1:18123:8123 -e CLICKHOUSE_USER=vario -e CLICKHOUSE_PASSWORD=testpass clickhouse/clickhouse-server:25.8
//	VARIO_TEST_CLICKHOUSE_URL=http://127.0.0.1:18123 VARIO_TEST_CLICKHOUSE_USER=vario VARIO_TEST_CLICKHOUSE_PASSWORD=testpass go test ./internal/storage/clickhouse/
func integrationClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	cfg := config.Default().ClickHouse
	cfg.URL = url
	cfg.Username = envOr("VARIO_TEST_CLICKHOUSE_USER", "default")
	cfg.Database = fmt.Sprintf("vario_test_%d", time.Now().UnixNano())
	c, err := New(cfg, os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(cfg.Database)) })
	return c
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func rowsBody(t *testing.T, rows ...any) []byte {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		j, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return gz(b.String())
}

func u64(v uint64) *uint64 { return &v }

func turn(version uint64, input *uint64, status string) model.ProviderTurnRow {
	return model.ProviderTurnRow{
		Scoped: model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: version}, ConversationID: "c1",
			ConversationCreatedAt: model.ParseTime("2026-03-01T09:00:00Z")},
		ProviderTurnID: "turn-1", Provider: "claudeAgent", Model: "m", Models: []string{}, UsageStatus: status,
		StartedAt: model.ParseTime("2026-03-01T10:00:00Z"), InputTokens: input, CachedInputTokens: u64(5), OutputTokens: u64(7),
	}
}

func TestIntegrationSchemaAndDeduplication(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatalf("schema setup must be idempotent: %v", err)
	}
	if missing, err := c.MissingTables(ctx); err != nil || len(missing) != 0 {
		t.Fatalf("missing tables: %v %v", missing, err)
	}

	// The provider turn is updated several times and a batch is retried.
	v1 := rowsBody(t, turn(10, nil, "running"))
	v2 := rowsBody(t, turn(20, u64(100), "complete"))
	for _, step := range []struct {
		body  []byte
		token string
	}{{v1, "t1"}, {v2, "t2"}, {v2, "t2"}, {v1, "t1-retry"}} {
		if err := c.Insert(ctx, model.TableProviderTurns, step.body, step.token); err != nil {
			t.Fatal(err)
		}
	}

	var raw []struct {
		N int `json:"n"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.provider_turns", quoteIdent(c.Database())), &raw); err != nil {
		t.Fatal(err)
	}
	if raw[0].N != 3 {
		t.Fatalf("identical retried block should be dropped by the dedup token: %d rows", raw[0].N)
	}

	var latest []struct {
		Input  *uint64 `json:"input_tokens"`
		Status string  `json:"usage_status"`
		N      int     `json:"n"`
	}
	q := fmt.Sprintf("SELECT any(input_tokens) AS input_tokens, any(usage_status) AS usage_status, count() AS n FROM %s.provider_turns_latest", quoteIdent(c.Database()))
	if err := c.Query(ctx, q, &latest); err != nil {
		t.Fatal(err)
	}
	if latest[0].N != 1 || latest[0].Input == nil || *latest[0].Input != 100 || latest[0].Status != "complete" {
		t.Fatalf("latest view must return only the highest version: %+v", latest[0])
	}

	var daily []struct {
		Input  uint64 `json:"input_tokens"`
		Output uint64 `json:"output_tokens"`
		Turns  int    `json:"provider_turns"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT input_tokens, output_tokens, provider_turns FROM %s.token_usage_daily", quoteIdent(c.Database())), &daily); err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 || daily[0].Input != 100 || daily[0].Output != 7 || daily[0].Turns != 1 {
		t.Fatalf("daily usage must not double count updated or retried turns: %+v", daily)
	}

	msg := model.MessageRow{Scoped: model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: 1}, ConversationID: "c1",
		ConversationCreatedAt: model.ParseTime("2026-03-01T09:00:00Z")}, MessageID: "m1", Role: "user", Text: "deploy the Kubernetes cluster"}
	if err := c.Insert(ctx, model.TableMessages, rowsBody(t, msg), "m1"); err != nil {
		t.Fatal(err)
	}
	var found []struct {
		N int `json:"n"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM %s.messages_latest WHERE hasToken(lower(text), 'kubernetes')", quoteIdent(c.Database())), &found); err != nil {
		t.Fatal(err)
	}
	if found[0].N != 1 {
		t.Fatalf("full-text search: %+v", found)
	}
}

func queryOne[T any](t *testing.T, c *Client, q string) []T {
	t.Helper()
	var out []T
	if err := c.Query(context.Background(), strings.ReplaceAll(q, "DB.", quoteIdent(c.Database())+"."), &out); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func TestIntegrationAggregationSemantics(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	ins := func(table, token string, rows ...any) {
		t.Helper()
		if err := c.Insert(ctx, table, rowsBody(t, rows...), token); err != nil {
			t.Fatal(err)
		}
	}
	epoch := func(e, seq uint64) uint64 { return e<<44 | seq }

	// Turn A: three versions across a source reset (epoch 2 must win even with
	// a lower sequence), delivered twice under different batch tokens.
	a1 := turn(epoch(1, 900), u64(50), "partial")
	a2 := turn(epoch(1, 950), u64(80), "complete")
	a3 := turn(epoch(2, 10), u64(120), "complete")
	ins(model.TableProviderTurns, "a", a1, a2)
	ins(model.TableProviderTurns, "b", a3, a1)
	ins(model.TableProviderTurns, "c", a2, a3) // ambiguous retry with a different grouping

	// Turn B: partial usage in a hypothetical subagent scope.
	b := turn(epoch(1, 5), u64(40), "partial")
	b.ProviderTurnID, b.UsageScope = "turn-2", "subagent"
	// Turn C: no usage reported at all.
	cTurn := turn(epoch(1, 6), nil, "")
	cTurn.ProviderTurnID, cTurn.CachedInputTokens, cTurn.OutputTokens = "turn-3", nil, nil
	ins(model.TableProviderTurns, "d", b, cTurn)

	type daily struct {
		Scope      string  `json:"usage_scope"`
		Turns      int     `json:"provider_turns"`
		WithUsage  int     `json:"turns_with_usage"`
		Incomplete int     `json:"turns_incomplete_usage"`
		Input      *uint64 `json:"input_tokens"`
	}
	rows := queryOne[daily](t, c, "SELECT usage_scope, provider_turns, turns_with_usage, turns_incomplete_usage, input_tokens FROM DB.token_usage_daily ORDER BY usage_scope")
	got := map[string]daily{}
	for _, r := range rows {
		got[r.Scope] = r
	}
	if r := got[""]; r.Turns != 2 || r.WithUsage != 1 || r.Input == nil || *r.Input != 120 || r.Incomplete != 1 {
		t.Errorf("default scope: want 2 turns (1 with usage, 120 input, 1 without usage); got %+v", r)
	}
	if r := got["subagent"]; r.Turns != 1 || r.Incomplete != 1 || r.Input == nil || *r.Input != 40 {
		t.Errorf("scopes must be reported separately and partial usage flagged: %+v", r)
	}

	// Raw events re-delivered by a repeated snapshot (same key, new version)
	// collapse in the latest view.
	ev := model.RawEventRow{Envelope: model.Envelope{SourceID: "src", Version: epoch(1, 3)}, Sequence: 3, EventID: "e3",
		EventType: "message.updated", OccurredAt: model.ParseTime("2026-03-01T10:00:00Z")}
	ev2 := ev
	ev2.Version = epoch(1, 9)
	ins(model.TableRawEvents, "r1", ev)
	ins(model.TableRawEvents, "r2", ev2)
	if n := queryOne[struct {
		N int `json:"n"`
	}](t, c, "SELECT count() AS n FROM DB.raw_events_latest"); n[0].N != 1 {
		t.Errorf("raw_events_latest should collapse re-delivered events: %d", n[0].N)
	}

	// A version whose conversation was unresolved lands in the 1970 partition;
	// the latest view must still return a single row for the key.
	m1 := model.MessageRow{Scoped: model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: 1}, ConversationID: "c1",
		ConversationCreatedAt: model.ParseTime("2026-03-01T09:00:00Z")}, MessageID: "m1", Role: "user"}
	m2 := m1
	m2.Version, m2.ConversationCreatedAt = 2, model.Time{}
	ins(model.TableMessages, "m1", m1)
	ins(model.TableMessages, "m2", m2)
	if n := queryOne[struct {
		N int `json:"n"`
	}](t, c, "SELECT count() AS n FROM DB.messages_latest"); n[0].N != 1 {
		t.Errorf("FINAL must deduplicate across partitions: %d rows", n[0].N)
	}

	// Tool usage counts each execution once despite updates.
	item := model.ExecutionEventRow{Scoped: m1.Scoped, EventID: "i1", EventType: "command_execution", Status: "running",
		StartedAt: model.ParseTime("2026-03-01T10:00:00Z")}
	done := item
	done.Version, done.Status, done.IsError = 5, "failed", 1
	ins(model.TableExecutionEvents, "i1", item)
	ins(model.TableExecutionEvents, "i2", done)
	tu := queryOne[struct {
		E int `json:"executions"`
		F int `json:"failures"`
	}](t, c, "SELECT executions, failures FROM DB.tool_usage")
	if len(tu) != 1 || tu[0].E != 1 || tu[0].F != 1 {
		t.Errorf("tool usage: %+v", tu)
	}
}
