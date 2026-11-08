// Package t3codetest builds synthetic T3 Code databases for tests.
package t3codetest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Schema mirrors the relevant parts of T3 Code's statev2.sqlite.
const Schema = `
CREATE TABLE orchestration_events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id TEXT NOT NULL UNIQUE,
  aggregate_kind TEXT NOT NULL,
  stream_id TEXT NOT NULL,
  stream_version INTEGER NOT NULL,
  event_type TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  command_id TEXT,
  actor_kind TEXT NOT NULL DEFAULT 'server',
  payload_json TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  application_event_version INTEGER NOT NULL DEFAULT 2
);
CREATE TABLE orchestration_v2_projection_threads (
  thread_id TEXT PRIMARY KEY, project_id TEXT NOT NULL, title TEXT NOT NULL, default_provider TEXT NOT NULL,
  runtime_mode TEXT NOT NULL, interaction_mode TEXT NOT NULL, active_provider_thread_id TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, archived_at TEXT, deleted_at TEXT, payload_json TEXT NOT NULL,
  provider_instance_id TEXT
);
CREATE TABLE orchestration_v2_projection_messages (
  message_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, run_id TEXT, node_id TEXT, role TEXT NOT NULL,
  streaming INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, payload_json TEXT NOT NULL
);
CREATE TABLE orchestration_v2_projection_turn_items (
  turn_item_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, run_id TEXT, node_id TEXT, provider_thread_id TEXT,
  provider_turn_id TEXT, parent_item_id TEXT, ordinal INTEGER NOT NULL, type TEXT NOT NULL, status TEXT NOT NULL,
  updated_at TEXT NOT NULL, payload_json TEXT NOT NULL
);
CREATE TABLE orchestration_v2_projection_provider_turns (
  provider_turn_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, provider_thread_id TEXT NOT NULL, node_id TEXT NOT NULL,
  run_attempt_id TEXT, ordinal INTEGER NOT NULL, status TEXT NOT NULL, started_at TEXT, completed_at TEXT,
  payload_json TEXT NOT NULL
);
CREATE TABLE orchestration_v2_projection_runs (
  run_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, ordinal INTEGER NOT NULL, provider TEXT NOT NULL,
  provider_thread_id TEXT, status TEXT NOT NULL, requested_at TEXT NOT NULL, completed_at TEXT, payload_json TEXT NOT NULL,
  provider_instance_id TEXT
);
CREATE TABLE orchestration_v2_projection_run_attempts (
  attempt_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, run_id TEXT NOT NULL, attempt_ordinal INTEGER NOT NULL,
  root_node_id TEXT NOT NULL, provider TEXT NOT NULL, provider_thread_id TEXT NOT NULL, provider_turn_id TEXT,
  status TEXT NOT NULL, payload_json TEXT NOT NULL, provider_instance_id TEXT
);
CREATE TABLE projection_projects (
  project_id TEXT PRIMARY KEY, title TEXT NOT NULL, workspace_root TEXT NOT NULL, scripts_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, deleted_at TEXT
);
CREATE TABLE orchestration_v2_projection_metadata (
  projection_name TEXT PRIMARY KEY, schema_version INTEGER NOT NULL, last_sequence INTEGER NOT NULL, updated_at TEXT NOT NULL
);
`

type Fixture struct {
	Path string
	DB   *sql.DB
	t    testing.TB
	// AutoProject keeps the projector high-water mark at the latest event.
	AutoProject bool
}

// New creates an empty fixture database in a temporary directory.
func New(t testing.TB) *Fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "userdata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "statev2.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environment-id"), []byte("env-fixture-0001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &Fixture{Path: path, DB: db, t: t, AutoProject: true}
	t.Cleanup(func() { db.Close() })
	f.SetMetadata("thread-projections", 0)
	return f
}

func (f *Fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.DB.Exec(q, args...); err != nil {
		f.t.Fatalf("fixture exec %q: %v", q, err)
	}
}

func js(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// SetMetadata sets a projection metadata row (thread-projections or event-log-prune-mark).
func (f *Fixture) SetMetadata(name string, seq int64) {
	f.exec(`INSERT INTO orchestration_v2_projection_metadata VALUES (?, 2, ?, '2026-01-01T00:00:00.000Z')
		ON CONFLICT (projection_name) DO UPDATE SET last_sequence = excluded.last_sequence`, name, seq)
}

// Event appends an event and returns its sequence.
func (f *Fixture) Event(eventType, streamID string, payload any) int64 {
	f.t.Helper()
	aggregate := "thread"
	if len(eventType) > 8 && eventType[:8] == "project." {
		aggregate = "project"
	}
	var n int64
	_ = f.DB.QueryRow(`SELECT COALESCE(MAX(stream_version), -1) + 1 FROM orchestration_events WHERE stream_id = ?`, streamID).Scan(&n)
	res, err := f.DB.Exec(`INSERT INTO orchestration_events (event_id, aggregate_kind, stream_id, stream_version, event_type, occurred_at, payload_json, metadata_json)
		VALUES (?, ?, ?, ?, ?, '2026-03-01T10:00:00.000Z', ?, '{"driver":"claudeAgent"}')`,
		fmt.Sprintf("evt-%s-%d", streamID, n), aggregate, streamID, n, eventType, js(payload))
	if err != nil {
		f.t.Fatal(err)
	}
	seq, _ := res.LastInsertId()
	if f.AutoProject {
		f.SetMetadata("thread-projections", seq)
	}
	return seq
}

// SkipSequences advances the sequence counter, simulating compacted events.
func (f *Fixture) SkipSequences(n int64) {
	var max int64
	_ = f.DB.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM orchestration_events`).Scan(&max)
	f.exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'orchestration_events'`, max+n)
}

func (f *Fixture) Project(id, title, root string) {
	f.exec(`INSERT OR REPLACE INTO projection_projects (project_id, title, workspace_root, created_at, updated_at) VALUES (?, ?, ?, '2026-02-01T00:00:00.000Z', '2026-02-01T00:00:00.000Z')`, id, title, root)
	f.Event("project.created", id, map[string]any{"projectId": id, "title": title})
}

func (f *Fixture) Thread(id, projectID, title, createdAt string) {
	p := map[string]any{
		"id": id, "projectId": projectID, "title": title, "providerInstanceId": "claudeAgent",
		"modelSelection": map[string]any{"instanceId": "claudeAgent", "model": "claude-test-1"},
		"runtimeMode":    "full-access", "interactionMode": "default", "branch": "feature/x",
		"lineage":   map[string]any{"parentThreadId": nil, "relationshipToParent": nil, "rootThreadId": id},
		"createdBy": "user", "creationSource": "web", "createdAt": createdAt, "updatedAt": createdAt,
		"lastVisitedAt": createdAt, "worktreePath": "/work/tree",
	}
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_threads (thread_id, project_id, title, default_provider, runtime_mode, interaction_mode, created_at, updated_at, payload_json, provider_instance_id)
		VALUES (?, ?, ?, 'claudeAgent', 'full-access', 'default', ?, ?, ?, 'claudeAgent')`, id, projectID, title, createdAt, createdAt, js(p))
	f.Event("thread.created", id, p)
}

// TouchThread records a thread metadata update with a new lastVisitedAt only.
func (f *Fixture) TouchThread(id, visitedAt string) {
	var payload string
	if err := f.DB.QueryRow(`SELECT payload_json FROM orchestration_v2_projection_threads WHERE thread_id = ?`, id).Scan(&payload); err != nil {
		f.t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(payload), &p)
	p["lastVisitedAt"] = visitedAt
	f.exec(`UPDATE orchestration_v2_projection_threads SET payload_json = ? WHERE thread_id = ?`, js(p), id)
	f.Event("thread.visited", id, p)
}

func (f *Fixture) Run(id, threadID string, ordinal int, model string) {
	p := map[string]any{"id": id, "threadId": threadID, "ordinal": ordinal, "providerInstanceId": "claudeAgent",
		"modelSelection": map[string]any{"instanceId": "claudeAgent", "model": model, "options": []any{map[string]any{"id": "effort", "value": "high"}}},
		"status":         "completed", "requestedAt": "2026-03-01T10:00:00.000Z", "startedAt": "2026-03-01T10:00:01.000Z", "completedAt": "2026-03-01T10:01:00.000Z"}
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_runs (run_id, thread_id, ordinal, provider, status, requested_at, completed_at, payload_json, provider_instance_id)
		VALUES (?, ?, ?, 'claudeAgent', 'completed', '2026-03-01T10:00:00.000Z', '2026-03-01T10:01:00.000Z', ?, 'claudeAgent')`, id, threadID, ordinal, js(p))
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_run_attempts (attempt_id, thread_id, run_id, attempt_ordinal, root_node_id, provider, provider_thread_id, status, payload_json)
		VALUES (?, ?, ?, 1, 'node-root', 'claudeAgent', 'pt-1', 'completed', '{}')`, id+":attempt:1", threadID, id)
	f.Event("run.updated", threadID, p)
}

func (f *Fixture) Message(id, threadID, runID, role, text string, streaming bool) {
	p := map[string]any{"id": id, "threadId": threadID, "runId": runID, "nodeId": "node-1", "role": role, "text": text,
		"attachments": []any{map[string]any{"type": "image", "id": "att-1", "name": "secret-plan.png", "mimeType": "image/png", "sizeBytes": 1234}},
		"streaming":   streaming, "createdAt": "2026-03-01T10:00:02.000Z", "updatedAt": "2026-03-01T10:00:03.000Z", "createdBy": "user", "creationSource": "web"}
	st := 0
	if streaming {
		st = 1
	}
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_messages (message_id, thread_id, run_id, node_id, role, streaming, created_at, updated_at, payload_json)
		VALUES (?, ?, ?, 'node-1', ?, ?, '2026-03-01T10:00:02.000Z', '2026-03-01T10:00:03.000Z', ?)`, id, threadID, runID, role, st, js(p))
	f.Event("message.updated", threadID, p)
}

// Item upserts a turn item; payload fields are merged over the common ones.
func (f *Fixture) Item(id, threadID, typ, status string, ordinal int, fields map[string]any) {
	p := map[string]any{"id": id, "threadId": threadID, "runId": "run-1", "nodeId": "node-1", "providerThreadId": "pt-1",
		"providerTurnId": "turn-1", "parentItemId": nil, "ordinal": ordinal, "status": status, "title": "Item " + typ,
		"startedAt": "2026-03-01T10:00:05.000Z", "completedAt": "2026-03-01T10:00:07.500Z", "updatedAt": "2026-03-01T10:00:07.500Z",
		"type": typ, "nativeItemRef": map[string]any{"driver": "claudeAgent", "nativeId": "n-" + id}}
	for k, v := range fields {
		p[k] = v
	}
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_turn_items (turn_item_id, thread_id, run_id, node_id, provider_thread_id, provider_turn_id, ordinal, type, status, updated_at, payload_json)
		VALUES (?, ?, 'run-1', 'node-1', 'pt-1', 'turn-1', ?, ?, ?, '2026-03-01T10:00:07.500Z', ?)`, id, threadID, ordinal, typ, status, js(p))
	f.Event("turn-item.updated", threadID, p)
}

// ProviderTurn upserts a provider turn. eventPayload, when non-nil, is used as
// the event payload instead of the full projection payload (T3 emits partial
// payloads for some updates).
func (f *Fixture) ProviderTurn(id, threadID, attemptID, status string, tokenUsage, turnTokenUsage map[string]any, eventPayload map[string]any) {
	p := map[string]any{"id": id, "providerThreadId": "pt-1", "nodeId": "node-1", "runAttemptId": attemptID,
		"nativeTurnRef": map[string]any{"driver": "claudeAgent", "nativeId": "native-" + id}, "ordinal": 1, "status": status,
		"startedAt": "2026-03-01T10:00:01.000Z", "completedAt": "2026-03-01T10:01:00.000Z"}
	if tokenUsage != nil {
		p["tokenUsage"] = tokenUsage
	}
	if turnTokenUsage != nil {
		p["turnTokenUsage"] = turnTokenUsage
	}
	f.exec(`INSERT OR REPLACE INTO orchestration_v2_projection_provider_turns (provider_turn_id, thread_id, provider_thread_id, node_id, run_attempt_id, ordinal, status, started_at, completed_at, payload_json)
		VALUES (?, ?, 'pt-1', 'node-1', ?, 1, ?, '2026-03-01T10:00:01.000Z', '2026-03-01T10:01:00.000Z', ?)`, id, threadID, attemptID, status, js(p))
	if eventPayload == nil {
		eventPayload = p
	}
	f.Event("provider-turn.updated", threadID, eventPayload)
}

// Standard populates a small, realistic dataset: one project, one thread, one
// run, a user and assistant message (each also present as a turn item), a
// command, a tool call, and a completed provider turn with usage.
func (f *Fixture) Standard() {
	f.Project("proj-1", "demo", "/home/dev/demo")
	f.Thread("thread-1", "proj-1", "Fix the login bug", "2026-03-01T09:59:00.000Z")
	f.Run("run-1", "thread-1", 1, "claude-test-1")
	f.Message("msg-user-1", "thread-1", "run-1", "user", "Please fix login. My key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", false)
	f.Item("item-user-1", "thread-1", "user_message", "completed", 1, map[string]any{"messageId": "msg-user-1", "text": "Please fix login. My key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"})
	f.Item("item-cmd-1", "thread-1", "command_execution", "completed", 2, map[string]any{"input": "grep -r password=hunter2secret .", "output": "config.go: password=hunter2secret", "exitCode": 1})
	f.Item("item-tool-1", "thread-1", "dynamic_tool", "completed", 3, map[string]any{"toolName": "Read", "input": map[string]any{"file_path": "/home/dev/demo/login.go"}, "output": "package main"})
	f.Message("msg-asst-1", "thread-1", "run-1", "assistant", "Fixed it.", false)
	f.Item("item-asst-1", "thread-1", "assistant_message", "completed", 4, map[string]any{"messageId": "msg-asst-1", "text": "Fixed it."})
	f.ProviderTurn("turn-1", "thread-1", "run-1:attempt:1", "completed",
		map[string]any{"usedTokens": 83311, "maxTokens": 1000000, "inputTokens": 83307, "cachedInputTokens": 79716, "outputTokens": 4, "reasoningOutputTokens": 0, "updatedAt": "2026-03-01T10:01:00.000Z"},
		map[string]any{"usageScope": "main_agent", "cachedInputTokens": 745279, "cacheCreationTokens": 73580, "reasoningTokens": 10136, "hasSubagents": false, "usageStatus": "complete", "inputTokens": 818885, "outputTokens": 17272},
		nil)
}

// Bytes checkpoints the fixture and returns the database file contents for read-only assertions.
func (f *Fixture) Bytes() []byte {
	f.t.Helper()
	if _, err := f.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		f.t.Fatal(err)
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// EventWithMetadata appends an event with explicit metadata.
func (f *Fixture) EventWithMetadata(eventType, streamID string, payload, metadata any) int64 {
	f.t.Helper()
	seq := f.Event(eventType, streamID, payload)
	f.exec(`UPDATE orchestration_events SET metadata_json = ? WHERE sequence = ?`, js(metadata), seq)
	return seq
}

// ThreadPayload merges fields into a thread's projected payload and records a metadata update.
func (f *Fixture) ThreadPayload(id string, fields map[string]any) {
	f.t.Helper()
	var payload string
	if err := f.DB.QueryRow(`SELECT payload_json FROM orchestration_v2_projection_threads WHERE thread_id = ?`, id).Scan(&payload); err != nil {
		f.t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(payload), &p)
	for k, v := range fields {
		p[k] = v
	}
	f.exec(`UPDATE orchestration_v2_projection_threads SET payload_json = ? WHERE thread_id = ?`, js(p), id)
	f.Event("thread.metadata-updated", id, p)
}
