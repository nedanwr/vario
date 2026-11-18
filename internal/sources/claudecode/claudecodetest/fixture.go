// Package claudecodetest writes synthetic Claude Code transcripts for tests.
// Their shape follows the transcripts Claude Code writes; the content is invented.
package claudecodetest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Markers appear in the synthetic content of the corresponding kind, so that
// tests can check where each kind of content ends up.
const (
	PromptMarker    = "PROMPT-MARKER-7f3a"
	ReplyMarker     = "REPLY-MARKER-91be"
	ThinkingMarker  = "THINKING-MARKER-c04d"
	ToolInMarker    = "TOOLIN-MARKER-55e2"
	ToolOutMarker   = "TOOLOUT-MARKER-0a19"
	TitleMarker     = "TITLE-MARKER-d8c1"
	PathMarker      = "pathmarker6b2e"
	BranchMarker    = "BRANCH-MARKER-3e7f"
	SubagentMarker  = "SUBAGENT-MARKER-a4f0"
	AccountMarker   = "ACCOUNT-MARKER-1c9d"
	ErrorMarker     = "ERROR-MARKER-77aa"
	FakeAPIKey      = "sk-ant-api03-FAKEFAKEFAKEFAKEFAKEFAKEFAKE0000"
	DefaultModel    = "claude-opus-5-5"
	DefaultCwd      = "/work/" + PathMarker + "/demo"
	DefaultVersion  = "2.1.323"
	DefaultBranch   = "feature/" + BranchMarker
	defaultBaseTime = "2026-03-04T05:06:07.000Z"
)

// Fixture is a Claude Code configuration directory with a projects directory.
type Fixture struct {
	t      testing.TB
	Config string // the configuration directory (~/.claude)
	Root   string // its projects directory
	clock  time.Time
	seq    int
}

func New(t testing.TB) *Fixture {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), ".claude")
	root := filepath.Join(cfg, "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	base, _ := time.Parse(time.RFC3339Nano, defaultBaseTime)
	return &Fixture{t: t, Config: cfg, Root: root, clock: base}
}

// now advances the fixture clock by one second and returns it.
func (f *Fixture) now() string {
	f.clock = f.clock.Add(time.Second)
	return f.clock.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ID returns a deterministic, UUID-shaped identifier.
func (f *Fixture) ID() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
}

// Session is one transcript file.
type Session struct {
	f       *Fixture
	Path    string
	ID      string
	Cwd     string
	agent   string
	parent  string
	prompt  string
	Version string
}

// EncodeProject returns the directory name Claude Code uses for a working directory.
func EncodeProject(cwd string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-", ".", "-").Replace(cwd)
}

// Session creates (but does not write) the transcript of a new session.
func (f *Fixture) Session(cwd string) *Session {
	id := f.ID()
	dir := filepath.Join(f.Root, EncodeProject(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return &Session{f: f, Path: filepath.Join(dir, id+".jsonl"), ID: id, Cwd: cwd, Version: DefaultVersion}
}

// Subagent creates the transcript of a subagent of s spawned by the tool
// call toolUseID, in the current layout (<session>/subagents/agent-<id>.jsonl
// with a .meta.json).
func (s *Session) Subagent(agentID, toolUseID string) *Session {
	dir := filepath.Join(filepath.Dir(s.Path), s.ID, "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.f.t.Fatal(err)
	}
	sub := &Session{f: s.f, Path: filepath.Join(dir, "agent-"+agentID+".jsonl"), ID: s.ID, Cwd: s.Cwd, agent: agentID, Version: s.Version}
	meta := map[string]any{"agentType": "Explore", "isBuiltIn": true, "description": "Survey " + SubagentMarker,
		"toolUseId": toolUseID, "spawnDepth": 1, "model": DefaultModel, "effort": "high", "finished": false}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(strings.TrimSuffix(sub.Path, ".jsonl")+".meta.json", b, 0o600); err != nil {
		s.f.t.Fatal(err)
	}
	return sub
}

// LegacySidechain returns a writer for subagent lines interleaved in the
// session's own transcript, as earlier Claude Code versions wrote them.
func (s *Session) LegacySidechain(agentID string) *Session {
	return &Session{f: s.f, Path: s.Path, ID: s.ID, Cwd: s.Cwd, agent: agentID, Version: s.Version}
}

// LegacyAgentFile returns a writer for a subagent transcript stored next to
// the session as agent-<id>.jsonl, as earlier Claude Code versions wrote them.
func (s *Session) LegacyAgentFile(agentID string) *Session {
	return &Session{f: s.f, Path: filepath.Join(filepath.Dir(s.Path), "agent-"+agentID+".jsonl"), ID: s.ID, Cwd: s.Cwd, agent: agentID, Version: s.Version}
}

// Line encodes a transcript line.
func Line(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// AppendRaw appends bytes to the transcript.
func (s *Session) AppendRaw(data string) {
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.f.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		s.f.t.Fatal(err)
	}
}

// Append appends complete lines.
func (s *Session) Append(lines ...string) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	s.AppendRaw(b.String())
}

func (s *Session) base(typ string) map[string]any {
	u := s.f.ID()
	m := map[string]any{
		"type": typ, "uuid": u, "parentUuid": nilIfEmpty(s.parent), "timestamp": s.f.now(), "sessionId": s.ID,
		"isSidechain": s.agent != "", "userType": "external", "entrypoint": "cli", "cwd": s.Cwd,
		"version": s.Version, "gitBranch": DefaultBranch,
	}
	if s.agent != "" {
		m["agentId"] = s.agent
	}
	s.parent = u
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// QueueOperation returns the line Claude Code writes when a prompt is queued.
func (s *Session) QueueOperation() string {
	return Line(map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": s.f.now(), "sessionId": s.ID,
		"content": "queued " + PromptMarker})
}

// Prompt returns a user prompt line and starts a new prompt group.
func (s *Session) Prompt(text string) string {
	s.prompt = s.f.ID()
	m := s.base("user")
	m["promptId"] = s.prompt
	m["permissionMode"] = "acceptEdits"
	m["promptSource"] = "typed"
	m["message"] = map[string]any{"role": "user", "content": text}
	return Line(m)
}

// MetaPrompt returns a user line injected by Claude Code (isMeta).
func (s *Session) MetaPrompt(text string) string {
	m := s.base("user")
	m["promptId"] = s.prompt
	m["isMeta"] = true
	m["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}
	return Line(m)
}

// Usage is a response's usage as Claude Code records it.
type Usage struct {
	Input, CacheCreate, CacheRead, Output, Thinking uint64
}

func (u Usage) json() map[string]any {
	return map[string]any{
		"input_tokens": u.Input, "cache_creation_input_tokens": u.CacheCreate, "cache_read_input_tokens": u.CacheRead,
		"output_tokens": u.Output, "output_tokens_details": map[string]any{"thinking_tokens": u.Thinking},
		"server_tool_use": map[string]any{"web_search_requests": 0, "web_fetch_requests": 0}, "service_tier": "standard",
		"cache_creation": map[string]any{"ephemeral_1h_input_tokens": u.CacheCreate, "ephemeral_5m_input_tokens": 0},
		"inference_geo":  "", "iterations": []any{}, "speed": "standard",
	}
}

// Block is one content block of a response.
type Block map[string]any

func Thinking(text string) Block {
	return Block{"type": "thinking", "thinking": text, "signature": "c2lnbmF0dXJl"}
}
func Text(text string) Block { return Block{"type": "text", "text": text} }
func ToolUse(id, name string, input map[string]any) Block {
	return Block{"type": "tool_use", "id": id, "name": name, "input": input, "caller": map[string]any{"type": "direct"}}
}

// Response returns the lines of one API response: one per content block,
// each repeating the response's usage and stop reason, as Claude Code writes
// them. msgID identifies the response.
func (s *Session) Response(msgID, model, stop string, u Usage, blocks ...Block) []string {
	req := "req_" + msgID
	var out []string
	for i, b := range blocks {
		m := s.base("assistant")
		m["requestId"] = req
		m["requestedModel"] = model
		m["apiBlockIndex"] = i
		m["message"] = map[string]any{"model": model, "id": msgID, "type": "message", "role": "assistant",
			"content": []any{map[string]any(b)}, "stop_reason": stop, "stop_sequence": nil, "usage": u.json()}
		out = append(out, Line(m))
	}
	return out
}

// StreamedResponse returns the lines of a response as earlier Claude Code
// versions wrote them while streaming: the usage of earlier lines is partial
// and only the last line carries the final output count and stop reason.
func (s *Session) StreamedResponse(msgID, model string, u Usage, blocks ...Block) []string {
	var out []string
	for i, b := range blocks {
		m := s.base("assistant")
		m["requestId"] = "req_" + msgID
		part := u
		stop := any(nil)
		if i < len(blocks)-1 {
			part.Output = uint64(i + 1)
		} else {
			stop = "end_turn"
		}
		m["message"] = map[string]any{"model": model, "id": msgID, "type": "message", "role": "assistant",
			"content": []any{map[string]any(b)}, "stop_reason": stop, "usage": part.json()}
		out = append(out, Line(m))
	}
	return out
}

// ToolResult returns the user line carrying a tool's result.
func (s *Session) ToolResult(toolUseID, output string, isError bool) string {
	m := s.base("user")
	m["promptId"] = s.prompt
	m["message"] = map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "tool_result", "tool_use_id": toolUseID, "content": output, "is_error": isError}}}
	m["toolUseResult"] = map[string]any{"stdout": output, "stderr": "", "interrupted": false, "isImage": false}
	m["sourceToolAssistantUUID"] = "x"
	return Line(m)
}

// SyntheticError returns the assistant line Claude Code writes for a failed request.
func (s *Session) SyntheticError() string {
	m := s.base("assistant")
	m["isApiErrorMessage"] = true
	m["message"] = map[string]any{"model": "<synthetic>", "id": s.f.ID(), "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": "API Error " + ErrorMarker}}, "stop_reason": "stop_sequence",
		"usage": Usage{}.json()}
	return Line(m)
}

// APIError returns a system line for a retried API error.
func (s *Session) APIError() string {
	m := s.base("system")
	m["subtype"] = "api_error"
	m["level"] = "error"
	m["error"] = map[string]any{"status": 529, "message": "overloaded " + ErrorMarker}
	m["retryAttempt"] = 1
	m["maxRetries"] = 10
	m["retryInMs"] = 512.5
	return Line(m)
}

// CompactBoundary returns a system line for a context compaction.
func (s *Session) CompactBoundary() string {
	m := s.base("system")
	m["subtype"] = "compact_boundary"
	m["content"] = "Conversation compacted"
	m["compactMetadata"] = map[string]any{"trigger": "auto", "preTokens": 150000, "postTokens": 12000, "durationMs": 3000}
	return Line(m)
}

// Attachment returns an attachment line (context Claude Code adds).
func (s *Session) Attachment() string {
	m := s.base("attachment")
	m["attachment"] = map[string]any{"type": "file", "filename": "/work/" + PathMarker + "/notes.md", "content": PromptMarker}
	return Line(m)
}

func (s *Session) CustomTitle(t string) string {
	return Line(map[string]any{"type": "custom-title", "customTitle": t, "sessionId": s.ID})
}

func (s *Session) AITitle(t string) string {
	return Line(map[string]any{"type": "ai-title", "aiTitle": t, "sessionId": s.ID})
}

// SessionOwner returns the line identifying the signed-in account.
func (s *Session) SessionOwner() string {
	return Line(map[string]any{"type": "session-owner", "accountUuid": AccountMarker, "organizationUuid": AccountMarker, "sessionId": s.ID})
}

// RequestBlob returns a request-log line, which repeats prompt content.
func (s *Session) RequestBlob() string {
	return Line(map[string]any{"type": "api-request-blob", "sessionId": s.ID, "hash": "h",
		"message": map[string]any{"role": "user", "content": PromptMarker}})
}

// Conversation appends a representative exchange to s: a prompt, a
// response with reasoning, text and a tool call, the tool's result and a
// final response. It returns the ids of the two responses and the tool call.
func (s *Session) Conversation(prefix string) (resp1, resp2, tool string) {
	resp1, resp2, tool = "msg_"+prefix+"_1", "msg_"+prefix+"_2", "toolu_"+prefix
	var lines []string
	lines = append(lines, s.QueueOperation(), s.Prompt("please help "+PromptMarker+" "+FakeAPIKey))
	lines = append(lines, s.Attachment())
	lines = append(lines, s.Response(resp1, DefaultModel, "tool_use", Usage{Input: 10, CacheCreate: 1000, CacheRead: 5000, Output: 200, Thinking: 50},
		Thinking("pondering "+ThinkingMarker),
		Text("working on it "+ReplyMarker),
		ToolUse(tool, "Bash", map[string]any{"command": "echo " + ToolInMarker, "description": "Print " + ToolInMarker}))...)
	lines = append(lines, s.ToolResult(tool, "out "+ToolOutMarker, false))
	lines = append(lines, s.Response(resp2, DefaultModel, "end_turn", Usage{Input: 3, CacheCreate: 300, CacheRead: 6200, Output: 40},
		Text("done "+ReplyMarker))...)
	s.Append(lines...)
	return
}
