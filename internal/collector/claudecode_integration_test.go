package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources"
	cct "github.com/nedanwr/vario/internal/sources/claudecode/claudecodetest"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// Claude Code and T3 Code rows share one database; the aggregate and
// deduplicated views count every Claude Code response once, across
// ambiguous redeliveries, repeated and forced backfills and live updates.
func TestIntegrationClaudeCodeAlongsideT3Code(t *testing.T) {
	url := os.Getenv("VARIO_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set VARIO_TEST_CLICKHOUSE_URL to run ClickHouse integration tests")
	}
	cfg := config.Default().ClickHouse
	cfg.URL = url
	cfg.Username = os.Getenv("VARIO_TEST_CLICKHOUSE_USER")
	cfg.Database = fmt.Sprintf("vario_cc_%d", time.Now().UnixNano())
	pw := os.Getenv("VARIO_TEST_CLICKHOUSE_PASSWORD")
	ch, err := clickhouse.New(cfg, pw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Exec(ctx, "DROP DATABASE IF EXISTS `"+cfg.Database+"`") })
	admin, err := clickhouse.NewAdmin(cfg, cfg.Username, pw)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := admin.PlanSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res := admin.Apply(ctx, plan, nil); !res.OK() {
		t.Fatal(res.Err)
	}

	h := dualSetup(t, config.ModeMessages)
	// A subagent and a second project.
	sub := h.main.Subagent("s1", "toolu_a")
	sub.Append(sub.Prompt("explore"))
	sub.Append(sub.Response("msg_sub", "claude-fable-5-2", "tool_use", cct.Usage{Input: 4, CacheCreate: 40, CacheRead: 400, Output: 7},
		cct.ToolUse("toolu_sub", "Read", map[string]any{"file_path": "/x"}))...)
	sub.Append(sub.ToolResult("toolu_sub", "data", false))
	other := h.cc.Session("/work/other")
	other.Append(other.Prompt("hi"))
	other.Append(other.Response("msg_o", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, CacheRead: 9, Output: 3}, cct.Text("yo"))...)

	sink := &ambiguousSink{Client: ch}
	c := New(h.store, sink, []sources.Source{h.t3src, h.ccsrc}, Options{BatchSize: 3, MaxInsertRows: 4, RetryInitial: time.Millisecond,
		RetryMax: time.Millisecond, PolicyFingerprint: h.policy.Fingerprint(), Endpoint: cfg.Endpoint(), Database: cfg.Database},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		t.Helper()
		for i := 0; i < 500; i++ {
			dr, err := c.Drain(ctx)
			if err == nil && dr.Remaining == 0 {
				return
			}
			var chErr *clickhouse.Error
			if err != nil && !errors.As(err, &chErr) {
				t.Fatal(err)
			}
		}
		t.Fatal("spool did not drain")
	}
	db := "`" + cfg.Database + "`"
	query := func(q string) []map[string]any {
		t.Helper()
		var out []map[string]any
		if err := ch.Query(ctx, strings.ReplaceAll(q, "$db", db), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	type totals struct {
		Turns, Input, Cached, Creation, Uncached, Output, Reasoning uint64
		Msgs, Events, Convs, Subagents, Raw, Tools                  uint64
	}
	read := func() totals {
		t.Helper()
		ccID := h.ccsrc.ID()
		r := query(fmt.Sprintf(`SELECT
			(SELECT sum(provider_turns) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS turns,
			(SELECT sum(input_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS input,
			(SELECT sum(cached_input_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS cached,
			(SELECT sum(cache_creation_input_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS creation,
			(SELECT sum(uncached_input_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS uncached,
			(SELECT sum(output_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS output,
			(SELECT sum(reasoning_tokens) FROM $db.token_usage_daily WHERE source_id = '%[1]s') AS reasoning,
			(SELECT count() FROM $db.messages_latest WHERE source_id = '%[1]s') AS msgs,
			(SELECT count() FROM $db.execution_events_latest WHERE source_id = '%[1]s') AS events,
			(SELECT count() FROM $db.conversations_latest WHERE source_id = '%[1]s') AS convs,
			(SELECT count() FROM $db.conversations_latest WHERE source_id = '%[1]s' AND relationship = 'subagent') AS subagents,
			(SELECT count() FROM $db.raw_events_latest WHERE source_id = '%[1]s') AS raw,
			(SELECT sum(executions) FROM $db.tool_usage WHERE source_id = '%[1]s') AS tools`, ccID))
		b, _ := json.Marshal(r[0])
		var tot struct {
			Turns, Input, Cached, Creation, Uncached, Output, Reasoning json.Number
			Msgs, Events, Convs, Subagents, Raw, Tools                  json.Number
		}
		dec := json.NewDecoder(strings.NewReader(strings.ToLower(string(b))))
		dec.UseNumber()
		if err := dec.Decode(&tot); err != nil {
			t.Fatal(err)
		}
		n := func(v json.Number) uint64 {
			x, _ := v.Int64()
			if x == 0 {
				f, _ := v.Float64()
				x = int64(f)
			}
			return uint64(x)
		}
		return totals{n(tot.Turns), n(tot.Input), n(tot.Cached), n(tot.Creation), n(tot.Uncached), n(tot.Output), n(tot.Reasoning),
			n(tot.Msgs), n(tot.Events), n(tot.Convs), n(tot.Subagents), n(tot.Raw), n(tot.Tools)}
	}

	for _, src := range []sources.Source{h.t3src, h.ccsrc} {
		if _, err := c.Snapshot(ctx, src, false, true); err != nil {
			t.Fatal(err)
		}
	}
	drain()
	// Responses: msg_a_1, msg_a_2 (Conversation), msg_sub, msg_o.
	want := totals{
		Turns: 4, Cached: 5000 + 6200 + 400 + 9, Creation: 1000 + 300 + 40, Uncached: 10 + 3 + 4 + 1,
		Output: 200 + 40 + 7 + 3, Reasoning: 50,
		// Prompt + 2 replies, subagent prompt, other prompt + reply.
		Msgs: 6,
		// Reasoning + Bash in the main session, Read in the subagent.
		Events: 3, Convs: 3, Subagents: 1, Tools: 2,
	}
	want.Input = want.Cached + want.Creation + want.Uncached
	got := read()
	want.Raw = got.Raw
	if got != want || got.Raw == 0 {
		t.Fatalf("totals after backfill:\n got  %+v\n want %+v", got, want)
	}
	// T3 Code's rows are in the same database, under their own source id.
	if r := query(`SELECT count() AS n FROM $db.provider_turns_latest WHERE source_id LIKE 't3code:%'`); fmt.Sprint(r[0]["n"]) != "1" {
		t.Fatalf("t3 code turns: %v", r)
	}

	// Repeated and forced backfills do not change logical results.
	for _, force := range []bool{false, true, false} {
		if _, err := c.Snapshot(ctx, h.ccsrc, force, force); err != nil {
			t.Fatal(err)
		}
		drain()
		if got := read(); got != want {
			t.Fatalf("after backfill (force=%t): %+v", force, got)
		}
	}

	// Live activity: new lines are read incrementally; the pending tool's result follows.
	h.main.Append(h.main.Prompt("again"))
	h.main.Append(h.main.Response("msg_new", cct.DefaultModel, "tool_use", cct.Usage{Input: 2, CacheRead: 20, Output: 11},
		cct.ToolUse("toolu_new", "WebSearch", map[string]any{"query": "q"}))...)
	if err := c.syncOnce(ctx, h.ccsrc, c.log); err != nil {
		t.Fatal(err)
	}
	drain()
	if r := query(`SELECT status FROM $db.execution_events_latest WHERE event_id = 'toolu_new'`); len(r) != 1 || r[0]["status"] != "running" {
		t.Fatalf("pending tool: %v", r)
	}
	h.main.Append(h.main.ToolResult("toolu_new", "results", false))
	if err := c.syncOnce(ctx, h.ccsrc, c.log); err != nil {
		t.Fatal(err)
	}
	drain()
	if r := query(`SELECT status, tool_name, event_type FROM $db.execution_events_latest WHERE event_id = 'toolu_new'`); len(r) != 1 || r[0]["status"] != "completed" || r[0]["event_type"] != "web_search" {
		t.Fatalf("completed tool: %v", r)
	}
	got = read()
	if got.Turns != want.Turns+1 || got.Output != want.Output+11 || got.Input != want.Input+22 || got.Tools != want.Tools+1 {
		t.Fatalf("after live activity: %+v", got)
	}
	// Relationships: the subagent's parent and its spawning tool call.
	r := query(`SELECT parent_conversation_id AS p, JSONExtractString(attributes, 'parentToolUseId') AS tool FROM $db.conversations_latest WHERE relationship = 'subagent'`)
	if len(r) != 1 || r[0]["p"] != h.main.ID || r[0]["tool"] != "toolu_a" {
		t.Fatalf("subagent relationship: %v", r)
	}
	r = query(`SELECT count() AS n FROM $db.provider_turns_latest p INNER JOIN $db.runs_latest r ON p.source_id = r.source_id AND p.conversation_id = r.conversation_id AND p.run_id = r.run_id WHERE p.source_id LIKE 'claudecode:%'`)
	if fmt.Sprint(r[0]["n"]) != "5" {
		t.Fatalf("every turn belongs to a run: %v", r)
	}
}
