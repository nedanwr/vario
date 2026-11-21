package claudecode

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	cct "github.com/nedanwr/vario/internal/sources/claudecode/claudecodetest"
)

var ctx = context.Background()

func policy(t testing.TB, mode string) *privacy.Policy {
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

// db keeps the latest version of every row, like ReplacingMergeTree FINAL.
type db struct {
	t        testing.TB
	rows     map[string]map[string]model.Row
	versions map[string]map[string]uint64
	raw      int // raw event rows received, including duplicates
	all      []model.Record
}

func newDB(t testing.TB) *db {
	return &db{t: t, rows: map[string]map[string]model.Row{}, versions: map[string]map[string]uint64{}}
}

func (d *db) add(recs []model.Record) {
	for _, r := range recs {
		if r.Table == model.TableRawEvents {
			d.raw++
		}
		d.all = append(d.all, r)
		if d.rows[r.Table] == nil {
			d.rows[r.Table] = map[string]model.Row{}
			d.versions[r.Table] = map[string]uint64{}
		}
		v := r.Row.Env().Version
		if old, ok := d.versions[r.Table][r.Key]; ok && v < old {
			continue
		}
		if old, ok := d.versions[r.Table][r.Key]; ok && v == old && !reflect.DeepEqual(norm(d.rows[r.Table][r.Key]), norm(r.Row)) {
			d.t.Errorf("%s/%s: two different revisions with the same version %d", r.Table, r.Key, v)
		}
		d.rows[r.Table][r.Key] = r.Row
		d.versions[r.Table][r.Key] = v
	}
}

func (d *db) emit(recs []model.Record) error { d.add(recs); return nil }

// norm returns a row's JSON without its version.
func norm(r model.Row) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "version")
	delete(m, "source_id")
	delete(m, "location")
	return m
}

// state returns every row without versions, for comparison.
func (d *db) state() map[string]map[string]map[string]any {
	out := map[string]map[string]map[string]any{}
	for t, rows := range d.rows {
		if t == model.TableSources {
			continue
		}
		out[t] = map[string]map[string]any{}
		for k, r := range rows {
			out[t][k] = norm(r)
		}
	}
	return out
}

func (d *db) count(table string) int { return len(d.rows[table]) }

func (d *db) turn(id string) *model.ProviderTurnRow {
	r, _ := d.rows[model.TableProviderTurns][id].(*model.ProviderTurnRow)
	return r
}

func (d *db) event(id string) *model.ExecutionEventRow {
	r, _ := d.rows[model.TableExecutionEvents][id].(*model.ExecutionEventRow)
	return r
}

func (d *db) conv(id string) *model.ConversationRow {
	r, _ := d.rows[model.TableConversations][id].(*model.ConversationRow)
	return r
}

func snapshot(t testing.TB, s *Source, prev string, history bool, d *db) string {
	t.Helper()
	cur, err := s.Snapshot(ctx, prev, history, d.emit)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

// drain applies Changes until nothing more is immediately available.
func drain(t testing.TB, s *Source, cur string, limit int, d *db) string {
	t.Helper()
	for i := 0; ; i++ {
		cs, err := s.Changes(ctx, cur, limit)
		if err != nil {
			t.Fatal(err)
		}
		if cs.Reconcile != "" {
			t.Fatalf("unexpected reconcile: %s", cs.Reconcile)
		}
		d.add(cs.Records)
		cur = cs.Cursor
		if !cs.More {
			return cur
		}
		if i > 100000 {
			t.Fatal("Changes does not make progress")
		}
	}
}

func u64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func TestBackfillMapsSessionsUsageAndTools(t *testing.T) {
	f := cct.New(t)
	s1 := f.Session(cct.DefaultCwd)
	s1.Append(s1.CustomTitle("old "+cct.TitleMarker), s1.SessionOwner())
	r1, r2, tool := s1.Conversation("a")
	s1.Append(s1.AITitle("ai "+cct.TitleMarker), s1.RequestBlob(), s1.APIError(), s1.SyntheticError(), s1.CompactBoundary())

	src := New(f.Config, policy(t, config.ModeMetrics), 3)
	if src.Location() != f.Root {
		t.Fatalf("the configuration directory should resolve to its projects directory: %s", src.Location())
	}
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)

	// One provider turn per response, although every response line repeats its usage.
	if d.count(model.TableProviderTurns) != 2 {
		t.Fatalf("provider turns: %d", d.count(model.TableProviderTurns))
	}
	p := d.turn(r1)
	if u64(p.InputTokens) != 6010 || u64(p.UncachedInputTokens) != 10 || u64(p.CachedInputTokens) != 5000 ||
		u64(p.CacheCreationInputTokens) != 1000 || u64(p.OutputTokens) != 200 || u64(p.ReasoningTokens) != 50 {
		t.Fatalf("usage of %s: %+v", r1, p)
	}
	if p.Provider != Provider || p.Model != cct.DefaultModel || p.ModelSource != "usage" || p.UsageScope != "main_agent" ||
		p.UsageStatus != "complete" || p.Ordinal != 1 || p.UsageSource != UsageSource || p.ConversationID != s1.ID {
		t.Fatalf("turn: %+v", p)
	}
	if q := d.turn(r2); q.Ordinal != 2 || u64(q.InputTokens) != 6503 || q.RunID != p.RunID || q.RunID == "" {
		t.Fatalf("second turn: %+v", q)
	}
	// The tool call and its result form one execution event.
	e := d.event(tool)
	if e == nil || e.EventType != "command_execution" || e.ToolName != "Bash" || e.Status != "completed" || e.IsError != 0 ||
		e.ProviderTurnID != r1 || e.RunID != p.RunID || e.DurationMs == nil || *e.DurationMs <= 0 || e.InputBytes == 0 || e.OutputBytes == 0 {
		t.Fatalf("tool event: %+v", e)
	}
	types := map[string]int{}
	for _, r := range d.rows[model.TableExecutionEvents] {
		types[r.(*model.ExecutionEventRow).EventType]++
	}
	if !reflect.DeepEqual(types, map[string]int{"command_execution": 1, "reasoning": 1, "error": 1, "context_compaction": 1}) {
		t.Fatalf("execution event types: %v", types)
	}
	// Prompt, two replies and the synthetic error text.
	if d.count(model.TableMessages) != 4 || d.count(model.TableRuns) != 1 || d.count(model.TableConversations) != 1 || d.count(model.TableProjects) != 1 {
		t.Fatalf("counts: %v", d.state())
	}
	run := d.rows[model.TableRuns][p.RunID].(*model.RunRow)
	if run.Status != "completed" || run.Ordinal != 1 || run.Model != cct.DefaultModel || run.UserMessageID == "" || run.CompletedAt.IsZero() {
		t.Fatalf("run: %+v", run)
	}
	c := d.conv(s1.ID)
	if c.Provider != Provider || c.Model != cct.DefaultModel || c.RuntimeMode != "acceptEdits" || c.CreationSource != "cli" ||
		c.ProjectID != ProjectID(cct.DefaultCwd) || c.CreatedAt.IsZero() || !c.UpdatedAt.After(c.CreatedAt.Time) || c.Title != "" || c.Branch != "" {
		t.Fatalf("conversation: %+v", c)
	}
	// Every line is a raw event, and every row of a conversation shares its creation time.
	lines := strings.Count(read(t, s1.Path), "\n")
	if d.count(model.TableRawEvents) != lines {
		t.Fatalf("raw events: %d, lines %d", d.count(model.TableRawEvents), lines)
	}
	for _, r := range d.all {
		if sc := scopedOf(r.Row); sc != nil && !sc.ConversationCreatedAt.Equal(c.CreatedAt.Time) {
			t.Fatalf("%s/%s: conversation_created_at %v, conversation created %v", r.Table, r.Key, sc.ConversationCreatedAt, c.CreatedAt)
		}
	}
	if Summary(cur) == "-" {
		t.Fatal("the cursor should be readable")
	}

	// A second snapshot from the cursor reads nothing new.
	d2 := newDB(t)
	cur2 := snapshot(t, src, cur, false, d2)
	if d2.count(model.TableRawEvents) != 0 || len(d2.all) != 1 {
		t.Fatalf("re-snapshot of unchanged transcripts: %d records", len(d2.all))
	}
	if cs, err := src.Changes(ctx, cur2, 10); err != nil || len(cs.Records) != 0 || cs.More {
		t.Fatalf("changes of unchanged transcripts: %+v %v", cs, err)
	}
}

func scopedOf(r model.Row) *model.Scoped {
	switch v := r.(type) {
	case *model.RunRow:
		return &v.Scoped
	case *model.MessageRow:
		return &v.Scoped
	case *model.ExecutionEventRow:
		return &v.Scoped
	case *model.ProviderTurnRow:
		return &v.Scoped
	}
	return nil
}

func read(t testing.TB, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStreamedUsageSnapshotsAreNotSummed(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.Prompt("hi"))
	s.Append(s.StreamedResponse("msg_stream", cct.DefaultModel, cct.Usage{Input: 7, CacheCreate: 70, CacheRead: 700, Output: 99},
		cct.Thinking("t"), cct.Text("a"), cct.Text("b"))...)
	d := newDB(t)
	snapshot(t, New(f.Root, policy(t, config.ModeMetrics), 100), "", false, d)
	p := d.turn("msg_stream")
	if d.count(model.TableProviderTurns) != 1 || u64(p.OutputTokens) != 99 || u64(p.InputTokens) != 777 || p.UsageStatus != "complete" {
		t.Fatalf("streamed usage: %d turns, %+v", d.count(model.TableProviderTurns), p)
	}
}

func TestMalformedUsageDetailsKeepMessageAndCounts(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.Prompt("hi"))
	var line map[string]any
	if err := json.Unmarshal([]byte(s.Response("msg_bad", cct.DefaultModel, "end_turn", cct.Usage{Input: 3, Output: 5}, cct.Text("reply"))[0]), &line); err != nil {
		t.Fatal(err)
	}
	u := line["message"].(map[string]any)["usage"].(map[string]any)
	u["output_tokens_details"], u["server_tool_use"], u["cache_creation"] = "bad", 7, []any{1}
	s.Append(cct.Line(line))
	d := newDB(t)
	snapshot(t, New(f.Root, policy(t, config.ModeFull), 100), "", false, d)
	p := d.turn("msg_bad")
	if p == nil || u64(p.UncachedInputTokens) != 3 || u64(p.OutputTokens) != 5 || p.ReasoningTokens != nil {
		t.Fatalf("turn: %+v", p)
	}
	found := false
	for _, r := range d.rows[model.TableMessages] {
		if m := r.(*model.MessageRow); m.Role == "assistant" && m.Text == "reply" {
			found = true
		}
	}
	if !found {
		t.Fatal("assistant message dropped")
	}
}

// buildCorpus writes a varied set of transcripts and returns their paths.
func buildCorpus(t testing.TB, f *cct.Fixture) []*cct.Session {
	main := f.Session(cct.DefaultCwd)
	main.Append(main.CustomTitle("t "+cct.TitleMarker), main.SessionOwner())
	_, _, _ = main.Conversation("m1")
	// A subagent spawned by an Agent tool call, in the current layout.
	main.Append(main.Prompt("delegate " + cct.PromptMarker))
	main.Append(main.Response("msg_m2", cct.DefaultModel, "tool_use", cct.Usage{Input: 1, CacheCreate: 2, CacheRead: 3, Output: 4},
		cct.ToolUse("toolu_agent", "Agent", map[string]any{"subagent_type": "Explore", "prompt": cct.ToolInMarker, "description": "d"}))...)
	sub := main.Subagent("a1b2c3", "toolu_agent")
	sub.Append(sub.Prompt("explore " + cct.SubagentMarker))
	sub.Append(sub.Response("msg_s1", "claude-fable-5-2", "tool_use", cct.Usage{Input: 11, CacheCreate: 22, CacheRead: 33, Output: 44},
		cct.ToolUse("toolu_s1", "Read", map[string]any{"file_path": "/work/" + cct.PathMarker + "/x.go"}))...)
	sub.Append(sub.ToolResult("toolu_s1", "content "+cct.ToolOutMarker, false))
	sub.Append(sub.Response("msg_s2", "claude-fable-5-2", "end_turn", cct.Usage{Input: 5, CacheCreate: 0, CacheRead: 60, Output: 6},
		cct.Text("found "+cct.ReplyMarker))...)
	main.Append(main.ToolResult("toolu_agent", "report "+cct.ToolOutMarker, false))
	main.Append(main.Response("msg_m3", cct.DefaultModel, "end_turn", cct.Usage{Input: 2, Output: 8}, cct.Text("ok"))...)
	main.Append(main.MetaPrompt("caveat"), main.Prompt("[Request interrupted by user]"))

	// A second project with a failing edit and a legacy interleaved sidechain.
	other := f.Session("/work/" + cct.PathMarker + "/other")
	other.Append(other.Prompt("edit " + cct.PromptMarker))
	other.Append(other.Response("msg_o1", cct.DefaultModel, "tool_use", cct.Usage{Input: 9, CacheCreate: 9, CacheRead: 9, Output: 9},
		cct.ToolUse("toolu_e", "Edit", map[string]any{"file_path": "/work/" + cct.PathMarker + "/a.go", "old_string": cct.ToolInMarker}))...)
	other.Append(other.ToolResult("toolu_e", "String not found "+cct.ToolOutMarker, true))
	side := other.LegacySidechain("old1")
	side.Append(side.Prompt("legacy task"))
	side.Append(side.Response("msg_l1", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("legacy"))...)
	legacy := other.LegacyAgentFile("old2")
	legacy.Append(legacy.Prompt("legacy file task"))
	legacy.Append(legacy.Response("msg_l2", cct.DefaultModel, "end_turn", cct.Usage{Input: 2, Output: 2}, cct.Text("legacy2"))...)
	other.Append(other.Response("msg_o2", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("gave up"))...)
	return []*cct.Session{main, sub, other, legacy}
}

func TestSubagentsAreLinkedConversations(t *testing.T) {
	f := cct.New(t)
	ss := buildCorpus(t, f)
	main, other := ss[0], ss[2]
	d := newDB(t)
	snapshot(t, New(f.Root, policy(t, config.ModeMetrics), 50), "", false, d)

	subID := main.ID + "/agent-a1b2c3"
	c := d.conv(subID)
	if c == nil || c.ParentConversationID != main.ID || c.RootConversationID != main.ID || c.Relationship != "subagent" || c.Title != "" {
		t.Fatalf("subagent conversation: %+v", c)
	}
	var attrs map[string]any
	_ = json.Unmarshal([]byte(c.Attributes), &attrs)
	if attrs["parentToolUseId"] != "toolu_agent" || attrs["agentType"] != "Explore" || attrs["agentId"] != "a1b2c3" {
		t.Fatalf("subagent attributes: %s", c.Attributes)
	}
	for _, id := range []string{"msg_s1", "msg_s2"} {
		if p := d.turn(id); p == nil || p.ConversationID != subID || p.UsageScope != "subagent" || p.Model != "claude-fable-5-2" {
			t.Fatalf("subagent turn %s: %+v", id, p)
		}
	}
	if e := d.event("toolu_agent"); e == nil || e.EventType != "dynamic_tool" || e.ToolName != "Agent" || e.Status != "completed" ||
		!strings.Contains(e.Attributes, `"subagentType":"Explore"`) {
		t.Fatalf("agent tool: %+v", e)
	}
	// Legacy layouts.
	for _, id := range []string{other.ID + "/agent-old1", other.ID + "/agent-old2"} {
		if c := d.conv(id); c == nil || c.ParentConversationID != other.ID || c.Relationship != "subagent" {
			t.Fatalf("legacy subagent %s: %+v", id, c)
		}
	}
	if p := d.turn("msg_l1"); p.ConversationID != other.ID+"/agent-old1" || p.UsageScope != "subagent" {
		t.Fatalf("legacy sidechain turn: %+v", p)
	}
	// The parent's run continues around the interleaved sidechain.
	if p := d.turn("msg_o2"); p.ConversationID != other.ID || p.Ordinal != 2 {
		t.Fatalf("turn after sidechain: %+v", p)
	}
	if e := d.event("toolu_e"); e.EventType != "file_change" || e.Status != "failed" || e.IsError != 1 || e.FileName != "" {
		t.Fatalf("failed edit: %+v", e)
	}
	// Total usage is the sum over all responses, each counted once.
	var out uint64
	for _, r := range d.rows[model.TableProviderTurns] {
		out += u64(r.(*model.ProviderTurnRow).OutputTokens)
	}
	if want := uint64(200 + 40 + 4 + 44 + 6 + 8 + 9 + 1 + 2 + 1); out != want || d.count(model.TableProviderTurns) != 10 {
		t.Fatalf("output tokens %d (want %d) over %d turns", out, want, d.count(model.TableProviderTurns))
	}
	// The interrupt does not start a run; the meta prompt neither.
	runs := 0
	for _, r := range d.rows[model.TableRuns] {
		if r.(*model.RunRow).ConversationID == main.ID {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("main runs: %d", runs)
	}
}

// Reading transcripts in arbitrary increments, including partial lines,
// yields exactly the rows of a single full read.
func TestIncrementalReadsMatchFullRead(t *testing.T) {
	incrementalMatchesFull(t, func(f *cct.Fixture) { buildCorpus(t, f) })
}

func incrementalMatchesFull(t *testing.T, build func(*cct.Fixture)) {
	ref := cct.New(t)
	build(ref)
	files := map[string]string{}
	err := filepath.Walk(ref.Root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(ref.Root, p)
			files[rel] = read(t, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.ModeMetrics, config.ModeFull} {
		pol := policy(t, mode)
		want := newDB(t)
		snapshot(t, New(ref.Root, pol, 7), "", false, want)

		for seed := int64(1); seed <= 5; seed++ {
			rng := rand.New(rand.NewSource(seed))
			dir := filepath.Join(t.TempDir(), "projects")
			written := map[string]int{}
			var names []string
			for rel, data := range files {
				names = append(names, rel)
				if strings.HasSuffix(rel, ".meta.json") {
					if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, rel), []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
					written[rel] = len(data)
				}
			}
			sort.Strings(names)
			src := New(dir, pol, 7)
			got := newDB(t)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			cur := snapshot(t, src, "", false, got)
			for done := false; !done; {
				done = true
				for _, rel := range names {
					data := files[rel]
					if written[rel] >= len(data) {
						continue
					}
					done = false
					if rng.Intn(3) == 0 {
						continue
					}
					n := min(len(data), written[rel]+1+rng.Intn(900))
					p := filepath.Join(dir, rel)
					if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
						t.Fatal(err)
					}
					fh, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := fh.WriteString(data[written[rel]:n]); err != nil {
						t.Fatal(err)
					}
					fh.Close()
					written[rel] = n
				}
				cur = drain(t, src, cur, 1+rng.Intn(5), got)
				if rng.Intn(4) == 0 {
					// A periodic reconciliation from the cursor in between.
					cur = snapshot(t, src, cur, false, got)
				}
			}
			drain(t, src, cur, 3, got)
			if !reflect.DeepEqual(got.state(), want.state()) {
				diff(t, want.state(), got.state())
				t.Fatalf("mode %s seed %d: incremental rows differ from a full read", mode, seed)
			}
			if got.raw != want.raw {
				t.Fatalf("mode %s seed %d: %d raw events, want %d (each line once)", mode, seed, got.raw, want.raw)
			}
		}
	}
}

func diff(t *testing.T, want, got map[string]map[string]map[string]any) {
	for table, rows := range want {
		for k, w := range rows {
			if g, ok := got[table][k]; !ok {
				t.Errorf("%s/%s missing", table, k)
			} else if !reflect.DeepEqual(w, g) {
				wb, _ := json.Marshal(w)
				gb, _ := json.Marshal(g)
				t.Errorf("%s/%s:\n want %s\n got  %s", table, k, wb, gb)
			}
		}
		for k := range got[table] {
			if _, ok := rows[k]; !ok {
				t.Errorf("%s/%s unexpected", table, k)
			}
		}
	}
}

func TestToolResultInLaterRead(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.Prompt("go"))
	s.Append(s.Response("msg_1", cct.DefaultModel, "tool_use", cct.Usage{Input: 1, Output: 1}, cct.ToolUse("toolu_1", "WebFetch", map[string]any{"url": "u"}))...)
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	if e := d.event("toolu_1"); e.Status != "running" || e.EventType != "web_search" || !e.CompletedAt.IsZero() {
		t.Fatalf("pending tool: %+v", e)
	}
	if r := d.rows[model.TableRuns]; len(r) != 1 {
		t.Fatalf("runs: %v", r)
	}
	s.Append(s.ToolResult("toolu_1", "page", false))
	s.Append(s.Response("msg_2", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("x"))...)
	drain(t, src, cur, 100, d)
	if e := d.event("toolu_1"); e.Status != "completed" || e.CompletedAt.IsZero() || e.DurationMs == nil {
		t.Fatalf("completed tool: %+v", e)
	}
}

func TestPartialAndMalformedLines(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.Prompt("go"))
	resp := s.Response("msg_1", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 5}, cct.Text("x"))[0]
	s.Append("{not json", `["array"]`, "")
	s.AppendRaw(resp[:len(resp)/2])
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	if d.count(model.TableProviderTurns) != 0 {
		t.Fatal("a partial line must not be read")
	}
	c, _ := parseCursor(cur)
	var st *fileState
	for _, fs := range c.Files {
		st = fs
	}
	if st.Bad != 2 {
		t.Fatalf("malformed lines: %d", st.Bad)
	}
	if !strings.Contains(Summary(cur), "2 unreadable lines") {
		t.Fatalf("summary: %s", Summary(cur))
	}
	// The rest of the line arrives without its newline: a complete JSON
	// value is read, and a later newline is harmless.
	s.AppendRaw(resp[len(resp)/2:])
	cur = drain(t, src, cur, 100, d)
	if p := d.turn("msg_1"); p == nil || u64(p.OutputTokens) != 5 {
		t.Fatalf("completed line: %+v", p)
	}
	s.AppendRaw("\n")
	s.Append(s.Prompt("again"))
	drain(t, src, cur, 100, d)
	if d.count(model.TableRuns) != 2 {
		t.Fatalf("runs: %d", d.count(model.TableRuns))
	}
	rep, err := Inspect(ctx, f.Config)
	if err != nil || rep.Bad != 2 || rep.Files != 1 || len(rep.BadFiles) != 1 || len(rep.Versions) != 1 {
		t.Fatalf("inspect: %+v %v", rep, err)
	}
}

func TestReplacedTruncatedAndDeletedFiles(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Conversation("x")
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	before := d.raw

	// Rewritten with different content of a larger size: read again from the start.
	data := read(t, s.Path)
	if err := os.WriteFile(s.Path, []byte(strings.Replace(data, `"output_tokens":40`, `"output_tokens":41`, -1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur = drain(t, src, cur, 1000, d)
	if p := d.turn("msg_x_2"); u64(p.OutputTokens) != 41 {
		t.Fatalf("rewritten transcript not re-read: %+v", p)
	}
	if d.raw != 2*before {
		t.Fatalf("raw events after rewrite: %d (before %d)", d.raw, before)
	}
	c, _ := parseCursor(cur)
	for _, fs := range c.Files {
		if fs.Epoch != 1 {
			t.Fatalf("epoch: %d", fs.Epoch)
		}
	}

	// Truncated: read again from the start; the rows stay identical.
	lines := strings.SplitAfter(read(t, s.Path), "\n")
	if err := os.WriteFile(s.Path, []byte(strings.Join(lines[:3], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(d.all)
	cur = drain(t, src, cur, 1000, d)
	if len(d.all) == n {
		t.Fatal("truncated transcript not re-read")
	}

	// Deleted: dropped from the cursor; collected rows are kept.
	if err := os.Remove(s.Path); err != nil {
		t.Fatal(err)
	}
	cs, err := src.Changes(ctx, cur, 100)
	if err != nil || len(cs.Records) != 0 || cs.Cursor == cur {
		t.Fatalf("deleted transcript: %+v %v", cs, err)
	}
	if c, _ := parseCursor(cs.Cursor); len(c.Files) != 0 {
		t.Fatalf("deleted transcript still in the cursor: %v", c.Files)
	}
	// An invalid cursor asks for reconciliation.
	if cs, _ := src.Changes(ctx, "garbage", 10); cs.Reconcile == "" {
		t.Fatal("an invalid cursor must request a reconcile")
	}
}

func TestCompactedFileRebuildsState(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.Prompt("go"))
	s.Append(s.Response("msg_1", cct.DefaultModel, "tool_use", cct.Usage{Input: 1, Output: 1}, cct.ToolUse("toolu_1", "Bash", map[string]any{"command": "sleep"}))...)
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	// Age the file so that its parse state is dropped.
	old := mustStat(t, s.Path).ModTime().Add(-2 * idleAfter)
	if err := os.Chtimes(s.Path, old, old); err != nil {
		t.Fatal(err)
	}
	// The first read notices the new modification time, the next one the idle file.
	cs, err := src.Changes(ctx, cur, 100)
	if err == nil {
		cs, err = src.Changes(ctx, cs.Cursor, 100)
	}
	if err != nil || len(cs.Records) != 0 {
		t.Fatalf("%+v %v", cs, err)
	}
	c, _ := parseCursor(cs.Cursor)
	for _, fs := range c.Files {
		if !fs.Compact || fs.Convs != nil {
			t.Fatalf("not compacted: %+v", fs)
		}
	}
	rawBefore := d.raw
	s.Append(s.ToolResult("toolu_1", "done", false))
	s.Append(s.Response("msg_2", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("ok"))...)
	drain(t, src, cs.Cursor, 100, d)
	if e := d.event("toolu_1"); e.Status != "completed" || e.Ordinal != 1 {
		t.Fatalf("tool after compaction: %+v", e)
	}
	if p := d.turn("msg_2"); p.Ordinal != 2 || p.RunID == "" {
		t.Fatalf("turn after compaction: %+v", p)
	}
	if d.raw != rawBefore+2 {
		t.Fatalf("only the new lines are raw events: %d new", d.raw-rawBefore)
	}
}

func mustStat(t testing.TB, p string) os.FileInfo {
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestForeignSessionLinesAreNotCollectedTwice(t *testing.T) {
	f := cct.New(t)
	orig := f.Session(cct.DefaultCwd)
	orig.Conversation("o")
	// A forked session whose transcript starts with a copy of the original's lines.
	fork := f.Session(cct.DefaultCwd)
	fork.AppendRaw(read(t, orig.Path))
	fork.Append(fork.Prompt("continue"))
	fork.Append(fork.Response("msg_f", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 3}, cct.Text("x"))...)
	d := newDB(t)
	snapshot(t, New(f.Root, policy(t, config.ModeMetrics), 100), "", false, d)
	if d.count(model.TableProviderTurns) != 3 || d.turn("msg_o_1").ConversationID != orig.ID || d.turn("msg_f").ConversationID != fork.ID {
		t.Fatalf("turns: %v", d.state()[model.TableProviderTurns])
	}
	// Each line is recorded once: the copied lines only from the original.
	origLines := strings.Count(read(t, orig.Path), "\n")
	forkLines := strings.Count(read(t, fork.Path), "\n")
	if d.raw != forkLines {
		t.Fatalf("raw events: %d, want %d (%d copied)", d.raw, forkLines, origLines)
	}
}

func TestPrivacyModes(t *testing.T) {
	f := cct.New(t)
	buildCorpus(t, f)
	s := f.Session(cct.DefaultCwd)
	s.Append(s.APIError(), s.SyntheticError())
	all := []string{cct.PromptMarker, cct.ReplyMarker, cct.ThinkingMarker, cct.ToolInMarker, cct.ToolOutMarker, cct.TitleMarker,
		cct.PathMarker, cct.BranchMarker, cct.SubagentMarker, cct.ErrorMarker, cct.AccountMarker, cct.FakeAPIKey}
	cases := map[string][]string{
		config.ModeMetrics:  nil,
		config.ModeMessages: {cct.PromptMarker, cct.ReplyMarker, cct.TitleMarker, cct.PathMarker, cct.BranchMarker, cct.SubagentMarker, cct.ErrorMarker},
		config.ModeFull:     {cct.PromptMarker, cct.ReplyMarker, cct.TitleMarker, cct.PathMarker, cct.BranchMarker, cct.SubagentMarker, cct.ErrorMarker, cct.ThinkingMarker, cct.ToolInMarker, cct.ToolOutMarker},
	}
	for mode, allowed := range cases {
		d := newDB(t)
		cur := snapshot(t, New(f.Root, policy(t, mode), 13), "", true, d)
		var b strings.Builder
		for _, r := range d.all {
			j, _ := json.Marshal(r.Row)
			b.Write(j)
			b.WriteString(r.Key)
		}
		out := b.String()
		ok := map[string]bool{}
		for _, m := range allowed {
			ok[m] = true
			if !strings.Contains(out, m) {
				t.Errorf("mode %s: %s should be collected", mode, m)
			}
		}
		for _, m := range all {
			if !ok[m] && strings.Contains(out, m) {
				t.Errorf("mode %s: %s leaked", mode, m)
			}
			// The cursor is local state but holds text only when it is
			// collected (transcript file names, like the source location,
			// are local metadata).
			if !ok[m] && m != cct.PathMarker && strings.Contains(cur, m) {
				t.Errorf("mode %s: %s kept in the cursor", mode, m)
			}
		}
		if !strings.Contains(out, "[REDACTED:anthropic_key]") && mode != config.ModeMetrics {
			t.Errorf("mode %s: the API key in the prompt should be redacted", mode)
		}
	}
}

func TestDetect(t *testing.T) {
	f := cct.New(t)
	buildCorpus(t, f)
	d := Detect(ctx, []string{f.Config, filepath.Join(t.TempDir(), "missing")})
	if !d[0].Compatible || d[0].Path != f.Root || d[1].Exists || d[1].Compatible {
		t.Fatalf("%+v", d)
	}
	facts := map[string]string{}
	for _, x := range d[0].Facts {
		facts[x.Name] = x.Value
	}
	if facts["sessions"] != "2" || facts["subagent transcripts"] != "2" || facts["projects"] != "2" || facts["claude code version"] != cct.DefaultVersion ||
		facts["source id"] != SourceID(f.Root) || SourceID(f.Root) != SourceID(f.Config) {
		t.Fatalf("facts: %v", facts)
	}
	t.Setenv(EnvConfigDir, f.Config)
	if p := DefaultPaths(); p[0] != f.Root {
		t.Fatalf("CLAUDE_CONFIG_DIR not honored: %v", p)
	}
}

func TestOversizedFirstLine(t *testing.T) {
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.AppendRaw(strings.Repeat("x", maxLine+1) + "\n")
	s.Append(s.Prompt("go"))
	s.Append(s.Response("msg_1", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("x"))...)
	// Attribution cannot see past the line, so the file must have settled.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(s.Path, old, old); err != nil {
		t.Fatal(err)
	}
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	if d.turn("msg_1") == nil {
		t.Fatal("lines after the oversized line must be read")
	}
	s.Append(s.Prompt("again"))
	cur = drain(t, src, cur, 100, d)
	c, _ := parseCursor(cur)
	for _, st := range c.Files {
		if st.Epoch != 0 || st.Bad != 1 || st.HeadLen != headMax {
			t.Fatalf("state: epoch %d, bad %d, head %d", st.Epoch, st.Bad, st.HeadLen)
		}
	}
}

func TestUnreadableProjectKeepsCursorEntries(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions are not enforced")
	}
	f := cct.New(t)
	s := f.Session(cct.DefaultCwd)
	s.Conversation("x")
	src := New(f.Root, policy(t, config.ModeMetrics), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	want, _ := parseCursor(cur)

	dir := filepath.Dir(s.Path)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	check := func(what, cur string) {
		t.Helper()
		c, _ := parseCursor(cur)
		if len(c.Files) != len(want.Files) {
			t.Fatalf("%s dropped entries of an unreadable project: %v", what, c.Files)
		}
	}
	cs, err := src.Changes(ctx, cur, 100)
	if err != nil {
		t.Fatal(err)
	}
	check("Changes", cs.Cursor)
	check("Snapshot", snapshot(t, src, cur, false, d))

	// Readable again: nothing is collected twice.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	n := len(d.all)
	if cs, err := src.Changes(ctx, cur, 100); err != nil || len(cs.Records) != 0 {
		t.Fatalf("re-collected after the project became readable: %d records, %v", len(cs.Records), err)
	}
	if len(d.all) != n {
		t.Fatal("unexpected records")
	}
}
