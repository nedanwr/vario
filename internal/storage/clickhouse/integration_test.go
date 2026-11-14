package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
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
	cfg.Database = fmt.Sprintf("vario_test_%d", time.Now().UnixNano()) // throwaway database
	c, err := New(cfg, os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(cfg.Database)) })
	return c
}

// migrateAll applies a full schema plan as the client's user (the
// integration user has administrative privileges).
func migrateAll(t *testing.T, c *Client) ApplyResult {
	t.Helper()
	a := &Admin{c}
	p, err := a.PlanSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := a.Apply(context.Background(), p, nil)
	if !res.OK() {
		t.Fatal(res.Err)
	}
	return res
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
	migrateAll(t, c)
	if res := migrateAll(t, c); len(res.Applied) != 0 {
		t.Fatalf("a repeated migration must be a no-op: %+v", res)
	}
	if st, err := c.VerifyMigrated(ctx); err != nil || !st.Complete() {
		t.Fatalf("verify: %+v %v", st, err)
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
	migrateAll(t, c)
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

type tableMeta struct {
	Name       string `json:"name"`
	Engine     string `json:"engine"`
	SortingKey string `json:"sorting_key"`
	EngineFull string `json:"engine_full"`
	AsSelect   string `json:"as_select"`
	Comment    string `json:"comment"`
	UUID       string `json:"uuid"`
}

func tableMetaOf(t *testing.T, c *Client, name string) tableMeta {
	t.Helper()
	rows := queryOne[tableMeta](t, c, fmt.Sprintf("SELECT name, engine, sorting_key, engine_full, as_select, comment, toString(uuid) AS uuid FROM system.tables WHERE database = '%s' AND name = '%s'",
		escapeString(c.Database()), name))
	if len(rows) != 1 {
		t.Fatalf("object %s: %+v", name, rows)
	}
	return rows[0]
}

// B-1: a table with Vario's columns but another sorting key or version
// argument passes a column check and silently collapses records. It must be
// rejected by the collector's check, doctor and migrate, and left as it is.
func TestIntegrationSortingKeyAndVersion(t *testing.T) {
	ctx := context.Background()
	msgDDL := func(c *Client) string { return tableDDL(c.Database())[model.TableMessages] }
	prepare := func(t *testing.T, rewrite func(ddl string) string) *Client {
		t.Helper()
		c := integrationClient(t)
		if err := c.Exec(ctx, CreateDatabaseStep(c.Database()).SQL); err != nil {
			t.Fatal(err)
		}
		ddl := rewrite(msgDDL(c))
		if ddl == msgDDL(c) {
			t.Fatal("the DDL was not rewritten")
		}
		if err := c.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
		return c
	}
	rejected := func(t *testing.T, c *Client, want string) {
		t.Helper()
		before := tableMetaOf(t, c, model.TableMessages)
		var se *SchemaError
		p, err := (&Admin{c}).PlanSchema(ctx)
		if !errors.As(err, &se) || !p.Empty() || len(se.Problems) != 1 || !strings.Contains(se.Problems[0], want) {
			t.Fatalf("migrate must reject the table (%q): %+v %v", want, p, err)
		}
		if _, err := c.CheckDestination(ctx); !errors.As(err, &se) {
			t.Fatalf("the collector must not deliver into it: %v", err)
		}
		r, err := c.CheckConnection(ctx)
		if err != nil || r.Ready() || r.Issues[0].Kind != KindSchema || !strings.Contains(r.Issues[0].Message, want) {
			t.Fatalf("doctor must report it: %+v %v", r, err)
		}
		// Nothing was created next to it, and the table itself is untouched.
		if objs := queryOne[tableMeta](t, c, fmt.Sprintf("SELECT name FROM system.tables WHERE database = '%s'", escapeString(c.Database()))); len(objs) != 1 {
			t.Fatalf("objects were created next to an incompatible table: %+v", objs)
		}
		if after := tableMetaOf(t, c, model.TableMessages); after != before {
			t.Fatalf("the incompatible table was modified:\n%+v\n%+v", before, after)
		}
	}

	t.Run("sorting key", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, "ORDER BY (source_id, conversation_id, message_id)", "ORDER BY (source_id, conversation_id)", 1)
		})
		// The defect: 12 messages of one conversation are one row to this table.
		var rows []any
		for i := 0; i < 12; i++ {
			rows = append(rows, model.MessageRow{Scoped: model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: uint64(i + 1)}, ConversationID: "c1",
				ConversationCreatedAt: model.ParseTime("2026-03-01T09:00:00Z")}, MessageID: fmt.Sprintf("m%d", i), Role: "user"})
		}
		if err := c.Insert(ctx, model.TableMessages, rowsBody(t, rows...), "b1"); err != nil {
			t.Fatal(err)
		}
		if n := queryOne[struct {
			N int `json:"n"`
		}](t, c, "SELECT count() AS n FROM DB.messages FINAL"); n[0].N != 1 {
			t.Fatalf("expected the wrong key to collapse the messages (the reproduced defect), got %d rows", n[0].N)
		}
		rejected(t, c, "sorting key (ORDER BY) is source_id, conversation_id, expected source_id, conversation_id, message_id")
	})
	t.Run("no version", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, "ReplacingMergeTree(version)", "ReplacingMergeTree", 1)
		})
		rejected(t, c, "version argument is none")
	})
	t.Run("other version column", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, "ReplacingMergeTree(version)", "ReplacingMergeTree(updated_at)", 1)
		})
		rejected(t, c, "version argument is updated_at")
	})
	t.Run("is_deleted", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, "ReplacingMergeTree(version)", "ReplacingMergeTree(version, streaming)", 1)
		})
		rejected(t, c, "version argument is version, streaming")
	})
	const partition = "PARTITION BY toYYYYMM(conversation_created_at)\n"
	t.Run("partition key", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, partition, "PARTITION BY toYYYYMM(updated_at)\n", 1)
		})
		// The defect: an update in a later month lands in another partition,
		// and merges never cross partitions, so the old version stays forever.
		for i, updated := range []string{"2026-03-31T23:00:00Z", "2026-04-01T01:00:00Z"} {
			row := model.MessageRow{Scoped: model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: uint64(i + 1)}, ConversationID: "c1",
				ConversationCreatedAt: model.ParseTime("2026-03-01T09:00:00Z")}, MessageID: "m1", Role: "user", UpdatedAt: model.ParseTime(updated)}
			if err := c.Insert(ctx, model.TableMessages, rowsBody(t, row), fmt.Sprintf("p%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Exec(ctx, "OPTIMIZE TABLE "+quoteIdent(c.Database())+".messages FINAL"); err != nil {
			t.Fatal(err)
		}
		if n := queryOne[struct {
			N int `json:"n"`
		}](t, c, "SELECT count() AS n FROM DB.messages"); n[0].N != 2 {
			t.Fatalf("expected both versions to survive a full merge under the wrong partition key (the reproduced defect), got %d rows", n[0].N)
		}
		rejected(t, c, "partition key (PARTITION BY) is toYYYYMM(updated_at), expected toYYYYMM(conversation_created_at)")
	})
	t.Run("no partition key", func(t *testing.T) {
		c := prepare(t, func(ddl string) string { return strings.Replace(ddl, partition, "", 1) })
		rejected(t, c, "partition key (PARTITION BY) is none, expected toYYYYMM(conversation_created_at)")
	})
	t.Run("other partition granularity", func(t *testing.T) {
		c := prepare(t, func(ddl string) string {
			return strings.Replace(ddl, partition, "PARTITION BY toYYYYMMDD(conversation_created_at)\n", 1)
		})
		rejected(t, c, "partition key (PARTITION BY) is toYYYYMMDD(conversation_created_at)")
	})
}

// B-1: the schema Vario creates passes its own check on a real server, whose
// system.tables reports exactly the partition and sorting keys the unit
// tests assume. Equivalent spellings (an explicit PARTITION BY tuple() for
// an unpartitioned table) are accepted and completed, never altered.
func TestIntegrationPartitionKeysAsReported(t *testing.T) {
	ctx := context.Background()
	c := integrationClient(t)
	if err := c.Exec(ctx, CreateDatabaseStep(c.Database()).SQL); err != nil {
		t.Fatal(err)
	}
	// sources is created by hand with an explicit empty partition key.
	ddl := tableDDL(c.Database())[model.TableSources]
	byHand := strings.Replace(ddl, "ORDER BY source_id\n", "PARTITION BY tuple()\nORDER BY source_id\n", 1)
	if byHand == ddl {
		t.Fatal("the DDL was not rewritten")
	}
	if err := c.Exec(ctx, byHand); err != nil {
		t.Fatal(err)
	}
	before := tableMetaOf(t, c, model.TableSources)
	migrateAll(t, c)
	if st, err := c.VerifyMigrated(ctx); err != nil || !st.Complete() {
		t.Fatalf("verify: %+v %v", st, err)
	}
	if _, err := c.CheckDestination(ctx); err != nil {
		t.Fatalf("the collector must accept the schema: %v", err)
	}
	if after := tableMetaOf(t, c, model.TableSources); after != before {
		t.Fatalf("the existing table was modified:\n%+v\n%+v", before, after)
	}
	type keys struct {
		Name      string `json:"name"`
		Partition string `json:"partition_key"`
		Sorting   string `json:"sorting_key"`
	}
	rows := queryOne[keys](t, c, fmt.Sprintf("SELECT name, partition_key, sorting_key FROM system.tables WHERE database = '%s' AND engine LIKE '%%ReplacingMergeTree' AND name != '%s'", escapeString(c.Database()), CheckpointTable))
	if len(rows) != len(model.Tables) {
		t.Fatalf("tables: %+v", rows)
	}
	for _, r := range rows {
		if normalizePartitionKey(r.Partition) != partitionKeys[r.Name] || r.Sorting != sortingKeys[r.Name] {
			t.Errorf("%s: ClickHouse reports partition key %q and sorting key %q, the tests assume %q and %q", r.Name, r.Partition, r.Sorting, partitionKeys[r.Name], sortingKeys[r.Name])
		}
	}
}

// B-2: migrate replaces only views that are positively Vario's own: tagged,
// or exactly a Vario 0.1 definition. A user's view under one of Vario's
// names is preserved and reported, even when it reads from a table whose
// name contains a Vario table name.
func TestIntegrationViewOwnership(t *testing.T) {
	c := integrationClient(t)
	ctx := context.Background()
	admin := &Admin{c}
	migrateAll(t, c)
	db := quoteIdent(c.Database())
	exec := func(q string) {
		t.Helper()
		if err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Vario 0.1 created its views without a comment. Recreate every view that
	// way, with the oldest definitions: all are recognized and upgraded.
	for _, v := range viewDefs(c.Database()) {
		exec(fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS %s", db, quoteIdent(v.name), v.legacy[len(v.legacy)-1]))
	}
	st, err := c.InspectSchema(ctx)
	if err != nil || len(st.Problems) != 0 || len(st.OutdatedViews) != len(viewDefs(c.Database())) {
		t.Fatalf("genuine Vario 0.1 views must be recognized: problems %v, outdated %v, %v", st.Problems, st.OutdatedViews, err)
	}
	if res := migrateAll(t, c); len(res.Applied) != len(st.OutdatedViews) || len(res.Warnings) != 0 {
		t.Fatalf("legacy views must be upgraded: %+v", res)
	}
	if st, err := c.VerifyMigrated(ctx); err != nil || !st.Complete() {
		t.Fatalf("after upgrading the legacy views: %+v %v", st, err)
	}
	if cols := queryOne[struct {
		N int `json:"n"`
	}](t, c, fmt.Sprintf("SELECT count() AS n FROM system.columns WHERE database = '%s' AND table = 'token_usage_daily' AND name = 'usage_scope'", escapeString(c.Database()))); cols[0].N != 1 {
		t.Fatal("token_usage_daily was not replaced by the current definition")
	}

	// A user replaces tool_usage with their own view over an archive table.
	exec(fmt.Sprintf("CREATE TABLE %s.execution_events_archive AS %s.execution_events", db, db))
	exec(fmt.Sprintf("CREATE OR REPLACE VIEW %[1]s.tool_usage AS SELECT tool_name, count() AS executions FROM %[1]s.execution_events_archive FINAL GROUP BY tool_name", db))
	before := tableMetaOf(t, c, "tool_usage")
	if before.Comment != "" || !strings.Contains(before.AsSelect, "execution_events_archive") {
		t.Fatalf("user view: %+v", before)
	}
	var se *SchemaError
	p, err := admin.PlanSchema(ctx)
	if !errors.As(err, &se) || !p.Empty() || len(se.Problems) != 1 || !strings.Contains(se.Problems[0], "view tool_usage exists but was not created by Vario") {
		t.Fatalf("a user-owned view must stop the migration with a clear conflict: %+v %v", p, err)
	}
	if res := admin.Apply(ctx, p, nil); len(res.Applied) != 0 {
		t.Fatalf("nothing may be applied: %+v", res)
	}
	if after := tableMetaOf(t, c, "tool_usage"); after != before {
		t.Fatalf("the user's view was replaced:\n%+v\n%+v", before, after)
	}

	// Once the user moves their view away, migrate creates Vario's.
	exec(fmt.Sprintf("RENAME TABLE %[1]s.tool_usage TO %[1]s.my_tool_usage", db))
	if res := migrateAll(t, c); len(res.Applied) != 1 {
		t.Fatalf("expected only the missing view to be created: %+v", res)
	}
	if st, err := c.VerifyMigrated(ctx); err != nil || !st.Complete() {
		t.Fatalf("verify: %+v %v", st, err)
	}
	if kept := tableMetaOf(t, c, "my_tool_usage"); kept.UUID != before.UUID {
		t.Fatal("the user's view must survive the migration")
	}
}

// The delivery checkpoint table on a real server: created by the migration,
// one logical row per writer that holds the highest number, readable with
// the consistency setting, and a version 2 database gets it without anything
// else changing.
func TestIntegrationCheckpointTable(t *testing.T) {
	ctx := context.Background()
	c := integrationClient(t)
	migrateAll(t, c)
	info, err := c.CheckDestination(ctx)
	if err != nil || !info.Checkpoints || info.Version != SchemaVersion || len(info.Warnings) != 0 {
		t.Fatalf("migrated schema: %+v %v", info, err)
	}
	if seq, found, err := c.ReadCheckpoint(ctx, "w1"); err != nil || found || seq != 0 {
		t.Fatalf("no checkpoint yet: %d %v %v", seq, found, err)
	}
	for _, seq := range []uint64{1, 2, 7, 5} {
		if err := c.WriteCheckpoint(ctx, "w1", seq); err != nil {
			t.Fatal(err)
		}
	}
	// The same number twice is two inserts, not a deduplicated one.
	for i := 0; i < 2; i++ {
		if err := c.WriteCheckpoint(ctx, "w'2", 3); err != nil {
			t.Fatal(err)
		}
	}
	if seq, found, err := c.ReadCheckpoint(ctx, "w1"); err != nil || !found || seq != 7 {
		t.Fatalf("checkpoint of w1: %d %v %v", seq, found, err)
	}
	if seq, found, err := c.ReadCheckpoint(ctx, "w'2"); err != nil || !found || seq != 3 {
		t.Fatalf("checkpoint of a writer with a quote in its id: %d %v %v", seq, found, err)
	}
	type count struct {
		N int `json:"n"`
	}
	table := quoteIdent(c.Database()) + "." + quoteIdent(CheckpointTable)
	// Merges keep one row per writer, the highest: the table stays small.
	if err := c.Exec(ctx, "OPTIMIZE TABLE "+table+" FINAL"); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[count](t, c, "SELECT count() AS n FROM "+table); n[0].N != 2 {
		t.Fatalf("after merging: %+v", n)
	}
	if seq, _, err := c.ReadCheckpoint(ctx, "w1"); err != nil || seq != 7 {
		t.Fatalf("after merging: %d %v", seq, err)
	}

	// A database as a Vario version before checkpoints left it.
	id := info.ServerID
	meta := quoteIdent(c.Database()) + "." + quoteIdent(MetaTable)
	for _, q := range []string{"DROP TABLE " + table, "TRUNCATE TABLE " + meta,
		fmt.Sprintf("INSERT INTO %s (key, value) VALUES ('destination_id', '%s'), ('schema_version', '2')", meta, id)} {
		if err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	info, err = c.CheckDestination(ctx)
	if err != nil || info.Checkpoints || info.Version != 2 || info.ServerID != id || len(info.Warnings) != 1 {
		t.Fatalf("version 2 schema: %+v %v", info, err)
	}
	var e *Error
	if _, _, err := c.ReadCheckpoint(ctx, "w1"); !errors.As(err, &e) || e.Code != 60 {
		t.Fatalf("a missing table must be an error, not \"no checkpoint\": %v", err)
	}
	p, err := c.PlanSchema(ctx)
	if err != nil || len(p.Steps) != 2 || !strings.Contains(p.Steps[0].SQL, CheckpointTable) {
		t.Fatalf("plan from version 2: %+v %v", p, err)
	}
	migrateAll(t, c)
	st, err := c.VerifyMigrated(ctx)
	if err != nil || !st.Complete() || st.DestinationID != id || st.Recorded != SchemaVersion {
		t.Fatalf("migrated from version 2: %+v %v", st, err)
	}
	if seq, found, err := c.ReadCheckpoint(ctx, "w1"); err != nil || found || seq != 0 {
		t.Fatalf("the new table is empty: %d %v %v", seq, found, err)
	}
}

// P2: a vario_checkpoints table with Vario's comment and columns but another
// engine, key, partitioning, version column or column type passed the check
// before. ENGINE = Null acknowledges checkpoint inserts and discards them.
// On a real server every variant is rejected by migrate (from version 3 and
// from version 2), doctor and the collector's check, and is left as it is.
func TestIntegrationCheckpointSchemaVerified(t *testing.T) {
	ctx := context.Background()
	const engine = "ENGINE = ReplacingMergeTree(seq)\nORDER BY writer_id"
	cases := []struct{ name, from, to, want string }{
		{"null engine", engine, "ENGINE = Null", "engine is Null, expected ReplacingMergeTree"},
		{"plain MergeTree", "ReplacingMergeTree(seq)", "MergeTree", "engine is MergeTree, expected ReplacingMergeTree"},
		{"sorting key", "ORDER BY writer_id", "ORDER BY (writer_id, seq)", "sorting key (ORDER BY) is writer_id, seq, expected writer_id"},
		{"primary key", "ORDER BY writer_id", "PRIMARY KEY tuple()\nORDER BY writer_id", "primary key is empty, expected writer_id"},
		{"partition key", "ORDER BY writer_id", "PARTITION BY toYYYYMM(written_at)\nORDER BY writer_id", "partition key (PARTITION BY) is toYYYYMM(written_at), expected none"},
		{"no version", "ReplacingMergeTree(seq)", "ReplacingMergeTree", "version argument is none, expected seq"},
		{"other version column", "ReplacingMergeTree(seq)", "ReplacingMergeTree(written_at)", "version argument is written_at, expected seq"},
		{"column type", "seq UInt64", "seq UInt32", "column seq is UInt32, expected UInt64"},
	}
	for _, tc := range cases {
		for _, from := range []int{3, 2} {
			t.Run(fmt.Sprintf("%s from version %d", tc.name, from), func(t *testing.T) {
				c := integrationClient(t)
				migrateAll(t, c)
				info, err := c.CheckDestination(ctx)
				if err != nil {
					t.Fatal(err)
				}
				table := quoteIdent(c.Database()) + "." + quoteIdent(CheckpointTable)
				meta := quoteIdent(c.Database()) + "." + quoteIdent(MetaTable)
				// The test replaces the table; Vario never does.
				ddl := checkpointDDL(c.Database())
				bad := strings.Replace(ddl, tc.from, tc.to, 1)
				if bad == ddl {
					t.Fatal("the DDL was not rewritten")
				}
				stmts := []string{"DROP TABLE " + table, bad}
				if from == 2 {
					stmts = append(stmts, "TRUNCATE TABLE "+meta,
						fmt.Sprintf("INSERT INTO %s (key, value) VALUES ('destination_id', '%s'), ('schema_version', '2')", meta, info.ServerID))
				}
				for _, q := range stmts {
					if err := c.Exec(ctx, q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
				if tc.name == "null engine" {
					// The defect: the insert is acknowledged, the checkpoint is gone.
					if err := c.WriteCheckpoint(ctx, "w1", 7); err != nil {
						t.Fatal(err)
					}
					if _, found, err := c.ReadCheckpoint(ctx, "w1"); err != nil || found {
						t.Fatalf("expected ENGINE = Null to discard the checkpoint (the reproduced defect): %v %v", found, err)
					}
				}
				before := tableMetaOf(t, c, CheckpointTable)

				st, err := c.InspectSchema(ctx)
				if err != nil || st.Usable() || st.Complete() || st.Checkpoints || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], tc.want) {
					t.Fatalf("inspect: %q %v", st.Problems, err)
				}
				var se *SchemaError
				if p, err := (&Admin{c}).PlanSchema(ctx); !errors.As(err, &se) || !p.Empty() {
					t.Fatalf("migrate must stop: %+v %v", p, err)
				}
				if _, err := c.VerifyMigrated(ctx); !errors.As(err, &se) {
					t.Fatalf("verify: %v", err)
				}
				if _, err := c.CheckDestination(ctx); !errors.As(err, &se) {
					t.Fatalf("the collector must not deliver: %v", err)
				}
				r, err := c.CheckConnection(ctx)
				if err != nil || r.Ready() || r.Issues[0].Kind != KindSchema || !strings.Contains(r.Issues[0].Message, tc.want) || r.Probe == "ok" {
					t.Fatalf("doctor must report it: %+v %v", r, err)
				}
				if after := tableMetaOf(t, c, CheckpointTable); after != before {
					t.Fatalf("the incompatible table was modified:\n%+v\n%+v", before, after)
				}
				if st, _ := c.InspectSchema(ctx); st.Recorded != from {
					t.Fatalf("the schema version must stay %d: %d", from, st.Recorded)
				}
			})
		}
	}
}
