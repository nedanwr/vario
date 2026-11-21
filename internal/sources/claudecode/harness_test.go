package claudecode

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	cct "github.com/nedanwr/vario/internal/sources/claudecode/claudecodetest"
)

// harnessCorpus is a set of sessions run by T3 Code, by Claude Code itself
// and by other Agent SDK applications.
type harnessCorpus struct {
	t3, legacy, app, appFile, nested, resumed, cli *cct.Session
}

func t3Session(f *cct.Fixture, entry string, marker bool) *cct.Session {
	s := f.Session(cct.DefaultCwd)
	s.Entrypoint = entry
	// T3 Code's transcripts start with lines that have no entrypoint.
	s.Append(s.CustomTitle("t3 "+cct.TitleMarker), s.SessionOwner(), s.QueueOperation())
	tools := []string{"WebFetch"}
	if marker {
		tools = cct.T3CodeTools
	}
	s.Append(s.Prompt("hello "+cct.PromptMarker), s.DeferredTools(tools...))
	return s
}

func subagentOf(s *cct.Session, agent, prefix string) {
	sub := s.Subagent(agent, "toolu_"+prefix+"_agent")
	sub.Append(sub.Prompt("survey "+cct.PromptMarker), sub.DeferredTools("WebFetch"))
	sub.Append(sub.Response("msg_"+prefix+"_s1", cct.DefaultModel, "tool_use", cct.Usage{Input: 5, CacheRead: 100, Output: 7},
		cct.ToolUse("toolu_"+prefix+"_s", "Bash", map[string]any{"command": "ls"}))...)
	sub.Append(sub.ToolResult("toolu_"+prefix+"_s", "out "+cct.ToolOutMarker, false))
	sub.Append(sub.Response("msg_"+prefix+"_s2", cct.DefaultModel, "end_turn", cct.Usage{Input: 6, CacheRead: 200, Output: 9},
		cct.Text("found "+cct.ReplyMarker))...)
}

func buildHarnessCorpus(f *cct.Fixture) harnessCorpus {
	var c harnessCorpus
	// The current T3 Code entrypoint, with a subagent.
	c.t3 = t3Session(f, "t3-code", true)
	c.t3.Conversation("t3")
	subagentOf(c.t3, "t3sub", "t3")
	// Earlier T3 Code builds: Agent SDK lines, recognized by T3 Code's MCP
	// tools; the subagent has no marker of its own.
	c.legacy = t3Session(f, "sdk-ts", true)
	c.legacy.Conversation("leg")
	subagentOf(c.legacy, "legsub", "leg")
	// Another Agent SDK application.
	c.app = t3Session(f, "sdk-ts", false)
	c.app.Conversation("app")
	subagentOf(c.app, "appsub", "app")
	// One whose prompt attaches a file that names T3 Code's tools: only the
	// tools available to the session count.
	c.appFile = t3Session(f, "sdk-ts", false)
	c.appFile.Append(c.appFile.FileAttachment("notes.md", `uses "mcp__t3-code__send_to_user"`))
	c.appFile.Conversation("appf")
	// A claude started by Claude in T3 Code (`claude -p` in a shell
	// command): it inherits T3 Code's entrypoint, but T3 Code knows nothing
	// of it.
	c.nested = t3Session(f, "t3-code", false)
	c.nested.Conversation("nest")
	subagentOf(c.nested, "nestsub", "nest")
	// A T3 Code session resumed in the terminal.
	c.resumed = t3Session(f, "t3-code", true)
	c.resumed.Conversation("r1")
	c.resumed.Entrypoint = "cli"
	c.resumed.Conversation("r2")
	// Claude Code itself.
	c.cli = f.Session(cct.DefaultCwd)
	c.cli.Conversation("cli")
	subagentOf(c.cli, "clisub", "cli")
	return c
}

// rowsMentioning returns the tables with a row whose JSON contains s.
func rowsMentioning(d *db, s string) []string {
	var out []string
	for table, rows := range d.rows {
		for _, r := range rows {
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), s) {
				out = append(out, table)
				break
			}
		}
	}
	return out
}

func TestSessionsRunByT3CodeAreNotCollectedTwice(t *testing.T) {
	f := cct.New(t)
	c := buildHarnessCorpus(f)
	for _, mode := range []string{config.ModeMetrics, config.ModeFull} {
		d := newDB(t)
		snapshot(t, New(f.Root, policy(t, mode), 50), "", true, d)

		// Main-agent work run by T3 Code is not collected at all.
		for _, s := range []*cct.Session{c.t3, c.legacy} {
			for _, table := range rowsMentioning(d, s.ID) {
				if table != model.TableProviderTurns {
					t.Fatalf("%s: session %s run by T3 Code has rows in %s", mode, s.ID, table)
				}
			}
			if d.conv(s.ID) != nil {
				t.Fatalf("%s: conversation of T3 Code session %s collected", mode, s.ID)
			}
		}
		for _, id := range []string{"msg_t3_1", "msg_t3_2", "msg_leg_1", "msg_leg_2", "msg_r1_1", "msg_r1_2"} {
			if d.turn(id) != nil {
				t.Fatalf("%s: response %s run by T3 Code collected", mode, id)
			}
		}
		for _, id := range []string{"toolu_t3", "toolu_leg", "toolu_r1", "toolu_t3_s", "toolu_leg_s"} {
			if d.event(id) != nil {
				t.Fatalf("%s: tool call %s run by T3 Code collected", mode, id)
			}
		}

		// Of T3 Code's subagents, only the usage is collected.
		for _, sc := range []struct {
			s             *cct.Session
			prefix, agent string
		}{{c.t3, "t3", "t3sub"}, {c.legacy, "leg", "legsub"}} {
			conv := sc.s.ID + "/agent-" + sc.agent
			if d.conv(conv) != nil {
				t.Fatalf("%s: conversation of T3 Code subagent %s collected", mode, conv)
			}
			for _, id := range []string{"msg_" + sc.prefix + "_s1", "msg_" + sc.prefix + "_s2"} {
				p := d.turn(id)
				if p == nil || p.ConversationID != conv || p.UsageScope != "subagent" || p.RunID != "" ||
					!strings.Contains(p.Attributes, `"harness":"t3code"`) || !strings.Contains(p.Attributes, `"agentId":"`+sc.agent+`"`) {
					t.Fatalf("%s: usage of T3 Code subagent response %s: %+v", mode, id, p)
				}
			}
		}

		// Everything else is collected in full.
		for _, id := range []string{"msg_app_1", "msg_app_2", "msg_app_s1", "msg_appf_1", "msg_appf_2", "msg_nest_1", "msg_nest_s2", "msg_r2_1", "msg_r2_2", "msg_cli_1", "msg_cli_s2"} {
			if p := d.turn(id); p == nil || strings.Contains(p.Attributes, "harness") {
				t.Fatalf("%s: response %s: %+v", mode, id, p)
			}
		}
		if cv := d.conv(c.app.ID); cv == nil || cv.CreationSource != "sdk-ts" || (mode == config.ModeFull && cv.Title == "") {
			t.Fatalf("%s: Agent SDK application conversation: %+v", mode, cv)
		}
		if cv := d.conv(c.nested.ID); cv == nil || cv.CreationSource != "t3-code" || d.event("toolu_nest") == nil {
			t.Fatalf("%s: session started within T3 Code: %+v", mode, cv)
		}
		if d.conv(c.app.ID+"/agent-appsub") == nil || d.conv(c.nested.ID+"/agent-nestsub") == nil || d.conv(c.cli.ID+"/agent-clisub") == nil {
			t.Fatalf("%s: subagent conversations of other sessions missing", mode)
		}
		// The resumed session's terminal segment is collected as its own run.
		if cv := d.conv(c.resumed.ID); cv == nil || cv.CreationSource != "cli" {
			t.Fatalf("%s: resumed session: %+v", mode, cv)
		}
		runs := 0
		for _, r := range d.rows[model.TableRuns] {
			if r.(*model.RunRow).ConversationID == c.resumed.ID {
				runs++
			}
		}
		if p := d.turn("msg_r2_1"); runs != 1 || p.Ordinal != 1 {
			t.Fatalf("%s: resumed session has %d runs, first turn %+v", mode, runs, p)
		}
		if e := d.event("toolu_r2"); e == nil || e.Status != "completed" {
			t.Fatalf("%s: resumed tool call: %+v", mode, e)
		}
	}
}

func TestIncrementalReadsMatchFullReadWithHarnesses(t *testing.T) {
	incrementalMatchesFull(t, func(f *cct.Fixture) { buildHarnessCorpus(f) })
}

// A transcript is collected only once the lines that decide who runs it
// are written; nothing is collected from it before.
func TestAttributionWaitsForDecidingLines(t *testing.T) {
	f := cct.New(t)
	src := New(f.Root, policy(t, config.ModeFull), 50)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)

	app := f.Session(cct.DefaultCwd)
	app.Entrypoint = "sdk-ts"
	t3 := f.Session(cct.DefaultCwd)
	t3.Entrypoint = "sdk-ts"
	nested := f.Session(cct.DefaultCwd)
	nested.Entrypoint = "t3-code"
	meta := f.Session(cct.DefaultCwd)
	for _, s := range []*cct.Session{app, t3, nested} {
		s.Append(s.CustomTitle("title"), s.QueueOperation(), s.Prompt("hello "+cct.PromptMarker))
	}
	meta.Append(meta.CustomTitle("title"), meta.QueueOperation())

	cur = drain(t, src, cur, 50, d)
	if n := len(d.rows[model.TableConversations]) + len(d.rows[model.TableMessages]) + d.raw; n != 0 {
		t.Fatalf("%d rows collected before attribution", n)
	}
	c, _ := parseCursor(cur)
	for _, fs := range c.Files {
		if fs.Off != 0 || fs.Own != "" {
			t.Fatalf("file consumed before attribution: %+v", fs)
		}
	}

	// A synthetic error (a failed request) is not a response: it decides
	// nothing.
	t3.Append(t3.SyntheticError())
	cur = drain(t, src, cur, 50, d)
	if got := rowsMentioning(d, t3.ID); len(got) != 0 {
		t.Fatalf("session attributed on a synthetic error: %v", got)
	}

	// The first response decides: without T3 Code's tools the session is
	// another application's, and everything from its start is collected.
	app.Append(app.Response("msg_a", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 2}, cct.Text("hi"))...)
	nested.Append(nested.Response("msg_n", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 2}, cct.Text("hi"))...)
	t3.Append(t3.DeferredTools(cct.T3CodeTools...))
	t3.Append(t3.Response("msg_t", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 2}, cct.Text("hi"))...)
	cur = drain(t, src, cur, 50, d)
	if cv := d.conv(app.ID); cv == nil || cv.Title != "title" || d.turn("msg_a") == nil {
		t.Fatalf("Agent SDK session: %+v", cv)
	}
	if cv := d.conv(nested.ID); cv == nil || cv.Title != "title" || d.turn("msg_n") == nil {
		t.Fatalf("session started within T3 Code: %+v", cv)
	}
	if got := rowsMentioning(d, t3.ID); len(got) != 0 {
		t.Fatalf("T3 Code session collected into %v", got)
	}

	// A transcript that stays undecided is attributed once it settles.
	old := time.Now().Add(-2 * settleAfter)
	if err := os.Chtimes(meta.Path, old, old); err != nil {
		t.Fatal(err)
	}
	cur = drain(t, src, cur, 50, d)
	c, _ = parseCursor(cur)
	for rel, fs := range c.Files {
		if strings.Contains(rel, meta.ID) && (fs.Own != ownSelf || fs.Off == 0) {
			t.Fatalf("settled transcript: %+v", fs)
		}
	}
	if d.conv(meta.ID) == nil {
		t.Fatal("settled transcript not collected")
	}
}

func TestInspectCountsSessionsRunByT3Code(t *testing.T) {
	f := cct.New(t)
	buildHarnessCorpus(f)
	rep, err := Inspect(ctx, f.Root)
	if err != nil {
		t.Fatal(err)
	}
	// t3, legacy and resumed, and the subagents of the first two.
	if rep.T3Sessions != 3 || rep.T3Subagents != 2 {
		t.Fatalf("T3 Code sessions %d, subagents %d", rep.T3Sessions, rep.T3Subagents)
	}
}

func TestLineOwner(t *testing.T) {
	for _, tc := range []struct{ entry, sdk, want string }{
		{"t3-code", HarnessT3Code, HarnessT3Code},
		{"t3-code", ownSelf, ownSelf},
		{"t3-code", "", ownSelf},
		{"sdk-ts", HarnessT3Code, HarnessT3Code},
		{"sdk-py", HarnessT3Code, HarnessT3Code},
		{"sdk-ts", ownSelf, ownSelf},
		{"cli", HarnessT3Code, ownSelf},
		{"claude-vscode", "", ownSelf},
	} {
		if got := lineOwner(tc.entry, tc.sdk); got != tc.want {
			t.Errorf("lineOwner(%q, %q) = %q, want %q", tc.entry, tc.sdk, got, tc.want)
		}
	}
}

// A compacted transcript is replayed from its start with the attribution of
// its first lines, not that of its last ones.
func TestCompactedFileReplaysAttribution(t *testing.T) {
	f := cct.New(t)
	s := t3Session(f, "t3-code", true)
	s.Conversation("r1")
	s.Entrypoint = "cli"
	s.Conversation("r2")
	src := New(f.Root, policy(t, config.ModeFull), 100)
	d := newDB(t)
	cur := snapshot(t, src, "", false, d)
	old := mustStat(t, s.Path).ModTime().Add(-2 * idleAfter)
	if err := os.Chtimes(s.Path, old, old); err != nil {
		t.Fatal(err)
	}
	cs, err := src.Changes(ctx, cur, 100)
	if err == nil {
		cs, err = src.Changes(ctx, cs.Cursor, 100)
	}
	if err != nil || len(cs.Records) != 0 {
		t.Fatalf("%+v %v", cs, err)
	}
	if c, _ := parseCursor(cs.Cursor); !c.Files[firstKey(c)].Compact {
		t.Fatal("not compacted")
	}
	s.Conversation("r3")
	drain(t, src, cs.Cursor, 100, d)

	ref := newDB(t)
	snapshot(t, New(f.Root, policy(t, config.ModeFull), 100), "", true, ref)
	got, want := d.conv(s.ID), ref.conv(s.ID)
	if got == nil || want == nil || got.Title != want.Title || strings.Contains(got.Title, "t3 ") {
		t.Fatalf("conversation after compaction %+v, full read %+v", got, want)
	}
	if p, q := d.turn("msg_r3_1"), ref.turn("msg_r3_1"); p == nil || q == nil || p.Ordinal != q.Ordinal {
		t.Fatalf("turn after compaction %+v, full read %+v", p, q)
	}
}

func firstKey(c cursor) string {
	for k := range c.Files {
		return k
	}
	return ""
}

func TestAttributionScansLongStartsInSteps(t *testing.T) {
	for name, filler := range map[string][]string{
		// Lines that end exactly at the step boundary.
		"lines": {strings.Repeat("x", headScanMax/2-64), strings.Repeat("y", headScanMax/2-64)},
		// One line that crosses it.
		"line": {strings.Repeat("x", headScanMax+1)},
	} {
		t.Run(name, func(t *testing.T) {
			f := cct.New(t)
			s := f.Session(cct.DefaultCwd)
			s.Entrypoint = "sdk-ts"
			s.Append(s.Prompt("hello"))
			for _, x := range filler {
				s.AppendRaw(`{"type":"progress","data":"` + x + "\"}\n")
			}
			tr := transcript{rel: "p/" + s.ID + ".jsonl", main: true, session: s.ID}
			fst := &fileState{}
			attr := func() bool {
				t.Helper()
				file, err := os.Open(s.Path)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				ok, err := attribute(file, tr, fst, nil, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return ok
			}

			// Past the step budget, the tool list is not yet written: wait,
			// keeping the progress.
			if attr() {
				t.Fatalf("attributed before the first response: %+v", fst)
			}
			st, _ := os.Stat(s.Path)
			if fst.Scan == nil || fst.Scan.Off != st.Size() || fst.Scan.Entry != "sdk-ts" {
				t.Fatalf("progress: %+v (size %d)", fst.Scan, st.Size())
			}

			s.Append(s.DeferredTools(cct.T3CodeTools...))
			s.Append(s.Response("msg_1", cct.DefaultModel, "end_turn", cct.Usage{Input: 1, Output: 1}, cct.Text("hi"))...)
			if !attr() || fst.SDK != HarnessT3Code || fst.Own != HarnessT3Code || fst.Scan != nil {
				t.Fatalf("sdk %q own %q scan %+v", fst.SDK, fst.Own, fst.Scan)
			}

			// A file whose scanned bytes changed is scanned from the start.
			fst = &fileState{Scan: &headScan{Off: 10, Tail: "stale"}}
			if !attr() || fst.SDK != HarnessT3Code {
				t.Fatalf("replaced file: %+v", fst)
			}
		})
	}
}
