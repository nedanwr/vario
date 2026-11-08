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
