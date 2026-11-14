package t3code

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Table names in T3 Code's statev2.sqlite.
const (
	tThreads       = "orchestration_v2_projection_threads"
	tMessages      = "orchestration_v2_projection_messages"
	tTurnItems     = "orchestration_v2_projection_turn_items"
	tProviderTurns = "orchestration_v2_projection_provider_turns"
	tRuns          = "orchestration_v2_projection_runs"
	tRunAttempts   = "orchestration_v2_projection_run_attempts"
	tProjects      = "projection_projects"
	tMetadata      = "orchestration_v2_projection_metadata"
	tEvents        = "orchestration_events"
)

// openReadOnly opens the database without any ability to write, migrate or checkpoint it.
func openReadOnly(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u := url.URL{Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// schema records which tables and columns exist so optional columns can be
// selected safely across T3 Code versions.
type schema map[string]map[string]bool

func (s schema) has(table string, cols ...string) bool {
	t, ok := s[table]
	if !ok {
		return false
	}
	for _, c := range cols {
		if !t[c] {
			return false
		}
	}
	return true
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func inspect(ctx context.Context, q querier) (schema, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s := schema{}
	for _, t := range tables {
		cols := map[string]bool{}
		r, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var c string
			if err := r.Scan(&c); err != nil {
				r.Close()
				return nil, err
			}
			cols[c] = true
		}
		r.Close()
		s[t] = cols
	}
	return s, nil
}

// required lists the tables and columns Vario cannot work without.
var required = map[string][]string{
	tThreads:       {"thread_id", "created_at", "payload_json"},
	tMessages:      {"message_id", "thread_id", "payload_json"},
	tTurnItems:     {"turn_item_id", "thread_id", "payload_json"},
	tProviderTurns: {"provider_turn_id", "thread_id", "payload_json"},
	tEvents:        {"sequence", "event_id", "event_type", "stream_id", "occurred_at", "payload_json"},
}

func (s schema) validate() error {
	var problems []string
	for _, t := range []string{tThreads, tMessages, tTurnItems, tProviderTurns, tEvents} {
		if _, ok := s[t]; !ok {
			problems = append(problems, "missing table "+t)
			continue
		}
		for _, c := range required[t] {
			if !s[t][c] {
				problems = append(problems, fmt.Sprintf("missing column %s.%s", t, c))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("unsupported T3 Code database schema: %s", strings.Join(problems, "; "))
	}
	return nil
}

// row holds one source row; SQL NULLs are absent keys.
type row map[string]string

func (r row) get(k string) string { return r[k] }

type tableSpec struct {
	table string
	id    string
	cols  []string
}

var (
	specThreads = tableSpec{tThreads, "thread_id", []string{"thread_id", "project_id", "title", "default_provider", "provider_instance_id",
		"runtime_mode", "interaction_mode", "created_at", "updated_at", "archived_at", "deleted_at", "payload_json"}}
	specMessages = tableSpec{tMessages, "message_id", []string{"message_id", "thread_id", "run_id", "node_id", "role", "streaming",
		"created_at", "updated_at", "payload_json"}}
	specItems = tableSpec{tTurnItems, "turn_item_id", []string{"turn_item_id", "thread_id", "run_id", "node_id", "provider_thread_id",
		"provider_turn_id", "parent_item_id", "ordinal", "type", "status", "updated_at", "payload_json"}}
	specProviderTurns = tableSpec{tProviderTurns, "provider_turn_id", []string{"provider_turn_id", "thread_id", "provider_thread_id",
		"node_id", "run_attempt_id", "ordinal", "status", "started_at", "completed_at", "payload_json"}}
	specRuns = tableSpec{tRuns, "run_id", []string{"run_id", "thread_id", "ordinal", "provider", "provider_instance_id",
		"provider_thread_id", "status", "requested_at", "completed_at", "payload_json"}}
	specProjects = tableSpec{tProjects, "project_id", []string{"project_id", "title", "workspace_root", "created_at", "updated_at", "deleted_at"}}
)

func (sp tableSpec) selectList(s schema) string {
	parts := make([]string, len(sp.cols))
	for i, c := range sp.cols {
		if s.has(sp.table, c) {
			parts[i] = `"` + c + `"`
		} else {
			parts[i] = "NULL"
		}
	}
	return strings.Join(parts, ", ")
}

func scanRows(rows *sql.Rows, cols []string) ([]row, error) {
	defer rows.Close()
	var out []row
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make(row, len(cols))
		for i, c := range cols {
			if vals[i].Valid {
				r[c] = vals[i].String
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// page reads rows ordered by id after the given id (keyset pagination).
func (sp tableSpec) page(ctx context.Context, q querier, s schema, after string, limit int) ([]row, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s" WHERE "%s" > ? ORDER BY "%s" LIMIT ?`,
		sp.selectList(s), sp.table, sp.id, sp.id), after, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, sp.cols)
}

// byIDs reads rows with the given ids.
func (sp tableSpec) byIDs(ctx context.Context, q querier, s schema, ids []string) ([]row, error) {
	var out []row
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s" WHERE "%s" IN (%s)`,
			sp.selectList(s), sp.table, sp.id, strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")), args...)
		if err != nil {
			return nil, err
		}
		rs, err := scanRows(rows, sp.cols)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

var eventCols = []string{"sequence", "event_id", "aggregate_kind", "stream_id", "event_type", "occurred_at", "payload_json", "metadata_json"}

func eventSelect(s schema) string {
	return tableSpec{tEvents, "sequence", eventCols}.selectList(s)
}

func readEvents(ctx context.Context, q querier, s schema, after, upTo int64, limit int) ([]row, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s" WHERE sequence > ? AND sequence <= ? ORDER BY sequence LIMIT ?`,
		eventSelect(s), tEvents), after, upTo, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, eventCols)
}

// boundary is the consistent snapshot point: projections reflect every event
// with sequence <= projected. maxSequence is the highest event in the log.
type boundary struct {
	projected   int64
	maxSequence int64
	pruneMark   int64
}

func readBoundary(ctx context.Context, q querier, s schema) (boundary, error) {
	var b boundary
	var maxSeq sql.NullInt64
	if err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT MAX(sequence) FROM "%s"`, tEvents)).Scan(&maxSeq); err != nil {
		return b, err
	}
	b.maxSequence = maxSeq.Int64
	b.projected = b.maxSequence
	if s.has(tMetadata, "projection_name", "last_sequence") {
		var v sql.NullInt64
		err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT last_sequence FROM "%s" WHERE projection_name = 'thread-projections'`, tMetadata)).Scan(&v)
		if err == nil && v.Valid && v.Int64 <= b.maxSequence {
			b.projected = v.Int64
		}
		err = q.QueryRowContext(ctx, fmt.Sprintf(`SELECT last_sequence FROM "%s" WHERE projection_name = 'event-log-prune-mark'`, tMetadata)).Scan(&v)
		if err == nil && v.Valid {
			b.pruneMark = v.Int64
		}
	}
	return b, nil
}
