package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nedanwr/vario/internal/model"
)

// Mutable entities use ReplacingMergeTree(version): the highest version of a
// key wins. Merges are asynchronous, so readers must use FINAL (the *_latest
// views) or argMax(..., version). Partition keys only use immutable values so
// that all versions of a row share a partition and deduplicate.
const (
	ts      = "DateTime64(3, 'UTC')"
	tsNull  = "Nullable(DateTime64(3, 'UTC'))"
	lc      = "LowCardinality(String)"
	u64Null = "Nullable(UInt64)"
	common  = "version UInt64,\n  ingested_at DateTime64(3, 'UTC') DEFAULT now64(3)"
	dedup   = "SETTINGS non_replicated_deduplication_window = 1000"
)

func tableDDL(db string) map[string]string {
	t := func(name string) string { return quoteIdent(db) + "." + quoteIdent(name) }
	return map[string]string{
		model.TableSources: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  source_kind %s,
  host_name String,
  location String,
  collection_mode %s,
  %s
) ENGINE = ReplacingMergeTree(version)
ORDER BY source_id
%s`, t(model.TableSources), lc, lc, common, dedup),

		model.TableProjects: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  project_id String,
  title String,
  workspace_root String,
  created_at %s,
  updated_at %s,
  deleted_at %s,
  is_deleted UInt8,
  %s
) ENGINE = ReplacingMergeTree(version)
ORDER BY (source_id, project_id)
%s`, t(model.TableProjects), ts, ts, tsNull, common, dedup),

		model.TableConversations: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  conversation_id String,
  project_id String,
  title String,
  provider %[2]s,
  provider_instance_id %[2]s,
  model %[2]s,
  runtime_mode %[2]s,
  interaction_mode %[2]s,
  branch String,
  parent_conversation_id String,
  root_conversation_id String,
  relationship %[2]s,
  created_by %[2]s,
  creation_source %[2]s,
  created_at %[3]s,
  updated_at %[3]s,
  archived_at %[4]s,
  deleted_at %[4]s,
  is_deleted UInt8,
  attributes String CODEC(ZSTD(3)),
  %[5]s,
  INDEX title_idx lower(title) TYPE tokenbf_v1(8192, 3, 0) GRANULARITY 1
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(created_at)
ORDER BY (source_id, conversation_id)
%[6]s`, t(model.TableConversations), lc, ts, tsNull, common, dedup),

		model.TableRuns: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  conversation_id String,
  conversation_created_at %[3]s,
  run_id String,
  ordinal Int64,
  provider %[2]s,
  provider_instance_id %[2]s,
  model %[2]s,
  model_options String,
  status %[2]s,
  user_message_id String,
  requested_at %[3]s,
  started_at %[4]s,
  completed_at %[4]s,
  attributes String CODEC(ZSTD(3)),
  %[5]s
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(conversation_created_at)
ORDER BY (source_id, conversation_id, run_id)
%[6]s`, t(model.TableRuns), lc, ts, tsNull, common, dedup),

		model.TableMessages: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  conversation_id String,
  conversation_created_at %[3]s,
  message_id String,
  run_id String,
  node_id String,
  role %[2]s,
  created_by %[2]s,
  creation_source %[2]s,
  streaming UInt8,
  text String COMMENT 'Empty unless privacy mode is messages or full' CODEC(ZSTD(3)),
  text_chars UInt64,
  content_included UInt8,
  attachment_count UInt32,
  attachments String,
  redactions UInt32,
  truncated UInt8,
  created_at %[3]s,
  updated_at %[3]s,
  %[4]s,
  INDEX text_idx lower(text) TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 1
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(conversation_created_at)
ORDER BY (source_id, conversation_id, message_id)
%[5]s`, t(model.TableMessages), lc, ts, common, dedup),

		model.TableExecutionEvents: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  conversation_id String,
  conversation_created_at %[3]s,
  event_id String,
  run_id String,
  node_id String,
  provider_thread_id String,
  provider_turn_id String,
  parent_event_id String,
  ordinal Int64,
  event_type %[2]s,
  status %[2]s,
  is_error UInt8,
  tool_name %[2]s,
  message_id String COMMENT 'Set for user_message/assistant_message items; their text lives in messages',
  exit_code Nullable(Int64),
  title String,
  input String COMMENT 'Empty unless privacy mode is full' CODEC(ZSTD(3)),
  output String COMMENT 'Empty unless privacy mode is full' CODEC(ZSTD(3)),
  input_bytes UInt64,
  output_bytes UInt64,
  file_name String,
  content_included UInt8,
  redactions UInt32,
  truncated UInt8,
  started_at %[4]s,
  completed_at %[4]s,
  updated_at %[3]s,
  duration_ms Nullable(Int64),
  attributes String CODEC(ZSTD(3)),
  %[5]s,
  INDEX type_idx event_type TYPE set(64) GRANULARITY 4,
  INDEX tool_idx tool_name TYPE set(256) GRANULARITY 4
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(conversation_created_at)
ORDER BY (source_id, conversation_id, event_id)
%[6]s`, t(model.TableExecutionEvents), lc, ts, tsNull, common, dedup),

		model.TableProviderTurns: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  conversation_id String,
  conversation_created_at %[3]s,
  provider_turn_id String,
  provider_thread_id String,
  run_id String,
  run_attempt_id String,
  node_id String,
  ordinal Int64,
  status %[2]s,
  provider %[2]s,
  provider_instance_id %[2]s,
  model %[2]s,
  model_source %[2]s COMMENT 'usage: reported with token usage; run: requested model selection',
  models Array(String),
  started_at %[4]s,
  completed_at %[4]s,
  duration_ms Nullable(Int64),
  usage_source %[2]s,
  usage_scope %[2]s,
  usage_status %[2]s,
  has_subagents Nullable(UInt8),
  input_tokens %[5]s COMMENT 'Turn total input; already includes cached and cache-creation tokens',
  cached_input_tokens %[5]s COMMENT 'Subset of input_tokens',
  cache_creation_input_tokens %[5]s COMMENT 'Subset of input_tokens',
  uncached_input_tokens %[5]s COMMENT 'Derived: input - cached - cache_creation, when all are reported',
  output_tokens %[5]s COMMENT 'Turn total output as reported by the provider',
  reasoning_tokens %[5]s COMMENT 'Reported reasoning tokens; not added to output_tokens',
  other_input_tokens %[5]s,
  other_output_tokens %[5]s,
  context_used_tokens %[5]s COMMENT 'Context window snapshot, not consumption; do not sum',
  context_max_tokens %[5]s,
  context_input_tokens %[5]s,
  context_cached_input_tokens %[5]s,
  context_output_tokens %[5]s,
  context_reasoning_output_tokens %[5]s,
  context_updated_at %[4]s,
  attributes String CODEC(ZSTD(3)),
  %[6]s
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(conversation_created_at)
ORDER BY (source_id, conversation_id, provider_turn_id)
%[7]s`, t(model.TableProviderTurns), lc, ts, tsNull, u64Null, common, dedup),

		model.TableRawEvents: fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  source_id String,
  sequence Int64,
  event_id String,
  aggregate_kind %[2]s,
  stream_id String,
  event_type %[2]s,
  occurred_at %[3]s,
  metadata String CODEC(ZSTD(3)),
  payload String COMMENT 'Empty unless privacy mode is full' CODEC(ZSTD(3)),
  payload_included UInt8,
  %[4]s
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (source_id, sequence, event_id)
%[5]s`, t(model.TableRawEvents), lc, ts, common, dedup),
	}
}

// viewDef is a Vario-owned view. base is the table it reads from; it is used
// to recognize views created by earlier Vario versions, which had no comment.
type viewDef struct {
	name, base, query string
}

func viewDefs(db string) []viewDef {
	t := func(name string) string { return quoteIdent(db) + "." + quoteIdent(name) }
	var out []viewDef
	for _, name := range model.Tables {
		out = append(out, viewDef{name + "_latest", name, fmt.Sprintf("SELECT * FROM %s FINAL", t(name))})
	}
	out = append(out,
		viewDef{"token_usage_daily", model.TableProviderTurns, fmt.Sprintf(`SELECT
  toDate(coalesce(started_at, conversation_created_at)) AS day,
  source_id,
  provider,
  model,
  usage_scope,
  count() AS provider_turns,
  countIf(p.input_tokens IS NOT NULL) AS turns_with_usage,
  countIf(usage_status != 'complete') AS turns_incomplete_usage,
  sum(p.input_tokens) AS input_tokens,
  sum(p.cached_input_tokens) AS cached_input_tokens,
  sum(p.cache_creation_input_tokens) AS cache_creation_input_tokens,
  sum(p.uncached_input_tokens) AS uncached_input_tokens,
  sum(p.output_tokens) AS output_tokens,
  sum(p.reasoning_tokens) AS reasoning_tokens
FROM %s AS p FINAL
GROUP BY day, source_id, provider, model, usage_scope`, t(model.TableProviderTurns))},
		viewDef{"tool_usage", model.TableExecutionEvents, fmt.Sprintf(`SELECT
  source_id,
  event_type,
  tool_name,
  count() AS executions,
  countIf(is_error = 1) AS failures,
  avg(duration_ms) AS avg_duration_ms,
  min(started_at) AS first_seen,
  max(started_at) AS last_seen
FROM %s FINAL
WHERE event_type IN ('command_execution', 'dynamic_tool', 'file_change', 'web_search')
GROUP BY source_id, event_type, tool_name`, t(model.TableExecutionEvents))},
	)
	return out
}

// viewTagPrefix marks views created by Vario. The suffix identifies the view
// definition, so an outdated view can be recognized and replaced without
// touching up-to-date views (replacing requires the DROP VIEW privilege).
const viewTagPrefix = "vario:"

func viewTag(name string) string {
	for _, v := range viewDefs("\x00") {
		if v.name == name {
			h := sha256.Sum256([]byte(v.query))
			return viewTagPrefix + hex.EncodeToString(h[:6])
		}
	}
	return ""
}

func (v viewDef) ddl(db string, replace bool) string {
	verb := "CREATE VIEW IF NOT EXISTS"
	if replace {
		verb = "CREATE OR REPLACE VIEW"
	}
	return fmt.Sprintf("%s %s.%s AS %s COMMENT '%s'", verb, quoteIdent(db), quoteIdent(v.name), v.query, escapeString(viewTag(v.name)))
}

type column struct{ name, typ string }

var colLineRe = regexp.MustCompile(`^  ([a-z_]+) (.+?),?$`)

// expectedColumns extracts column names and types from Vario's table DDL.
func expectedColumns(ddl string) []column {
	var out []column
	for _, line := range strings.Split(ddl, "\n") {
		m := colLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		typ := m[2]
		for _, cut := range []string{" COMMENT ", " CODEC(", " DEFAULT "} {
			if i := strings.Index(typ, cut); i >= 0 {
				typ = typ[:i]
			}
		}
		out = append(out, column{m[1], strings.TrimSpace(typ)})
	}
	return out
}

// ErrDatabaseMissing is returned when the configured database does not exist
// (or is not visible to the user) and database creation is not enabled.
var ErrDatabaseMissing = errors.New("database does not exist")

// SchemaError reports existing objects in the target database that are not
// compatible with Vario. Vario never alters or replaces them.
type SchemaError struct {
	Database string
	Problems []string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("incompatible objects in database %s (Vario will not modify them; use another database or remove them): %s",
		e.Database, strings.Join(e.Problems, "; "))
}

// SchemaStatus describes Vario's objects in the target database.
type SchemaStatus struct {
	DatabaseExists bool
	MissingTables  []string
	MissingViews   []string
	// OutdatedViews were created by Vario but have an older definition.
	OutdatedViews []string
	// Problems lists incompatible existing objects (see SchemaError).
	Problems []string
}

// Complete reports whether every table and view exists and is up to date.
func (s SchemaStatus) Complete() bool {
	return s.DatabaseExists && len(s.MissingTables) == 0 && len(s.MissingViews) == 0 && len(s.OutdatedViews) == 0 && len(s.Problems) == 0
}

// DatabaseExists reports whether the configured database exists and is
// visible to the user. ClickHouse hides databases the user has no grants on.
func (c *Client) DatabaseExists(ctx context.Context) (bool, error) {
	var rows []struct {
		N int `json:"n"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM system.databases WHERE name = '%s'", escapeString(c.database)), &rows); err != nil {
		return false, err
	}
	return len(rows) == 1 && rows[0].N == 1, nil
}

// InspectSchema compares the objects in the target database with the schema
// Vario expects. It only reads system tables.
func (c *Client) InspectSchema(ctx context.Context) (SchemaStatus, error) {
	var st SchemaStatus
	exists, err := c.DatabaseExists(ctx)
	if err != nil {
		return st, err
	}
	st.DatabaseExists = exists
	if !exists {
		st.MissingTables = append([]string(nil), model.Tables...)
		for _, v := range viewDefs(c.database) {
			st.MissingViews = append(st.MissingViews, v.name)
		}
		return st, nil
	}
	db := escapeString(c.database)
	var objects []struct {
		Name     string `json:"name"`
		Engine   string `json:"engine"`
		AsSelect string `json:"as_select"`
		Comment  string `json:"comment"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT name, engine, as_select, comment FROM system.tables WHERE database = '%s'", db), &objects); err != nil {
		return st, err
	}
	var cols []struct {
		Table string `json:"table"`
		Name  string `json:"name"`
		Type  string `json:"type"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT table, name, type FROM system.columns WHERE database = '%s'", db), &cols); err != nil {
		return st, err
	}
	type object struct{ engine, asSelect, comment string }
	have := map[string]object{}
	for _, o := range objects {
		have[o.Name] = object{o.Engine, o.AsSelect, o.Comment}
	}
	colTypes := map[string]map[string]string{}
	for _, col := range cols {
		if colTypes[col.Table] == nil {
			colTypes[col.Table] = map[string]string{}
		}
		colTypes[col.Table][col.Name] = col.Type
	}
	ddl := tableDDL(c.database)
	for _, name := range model.Tables {
		o, ok := have[name]
		if !ok {
			st.MissingTables = append(st.MissingTables, name)
			continue
		}
		if !strings.Contains(o.engine, "ReplacingMergeTree") {
			st.Problems = append(st.Problems, fmt.Sprintf("%s has engine %s, expected ReplacingMergeTree", name, o.engine))
			continue
		}
		var bad []string
		for _, want := range expectedColumns(ddl[name]) {
			got, ok := colTypes[name][want.name]
			switch {
			case !ok:
				bad = append(bad, "missing column "+want.name)
			case got != want.typ:
				bad = append(bad, fmt.Sprintf("column %s is %s, expected %s", want.name, got, want.typ))
			}
		}
		if len(bad) > 0 {
			st.Problems = append(st.Problems, fmt.Sprintf("table %s: %s", name, strings.Join(bad, ", ")))
		}
	}
	for _, v := range viewDefs(c.database) {
		o, ok := have[v.name]
		switch {
		case !ok:
			st.MissingViews = append(st.MissingViews, v.name)
		case o.engine != "View":
			st.Problems = append(st.Problems, fmt.Sprintf("%s is a %s, expected a view", v.name, o.engine))
		case o.comment == viewTag(v.name):
		case strings.HasPrefix(o.comment, viewTagPrefix) || (o.comment == "" && strings.Contains(o.asSelect, v.base)):
			st.OutdatedViews = append(st.OutdatedViews, v.name)
		default:
			st.Problems = append(st.Problems, fmt.Sprintf("view %s exists but was not created by Vario", v.name))
		}
	}
	return st, nil
}

// SchemaResult reports what InitSchema changed.
type SchemaResult struct {
	CreatedDatabase bool
	CreatedTables   []string
	CreatedViews    []string
	UpdatedViews    []string
	// Warnings are problems that do not prevent delivery.
	Warnings []string
}

// InitSchema prepares the target database for delivery. It is idempotent and
// confined to the configured database:
//
//   - The database is created only if it is missing and creation is enabled
//     (clickhouse.create_database); otherwise a missing database is an error
//     wrapping ErrDatabaseMissing.
//   - Existing objects with Vario's names are checked first; incompatible ones
//     yield a *SchemaError and nothing is created or changed.
//   - Only missing tables are created, so users without CREATE TABLE can use a
//     schema provisioned by an administrator.
//   - Missing views are created; outdated Vario views (recognized by their
//     tag or, for earlier versions, their definition) are replaced, which
//     needs DROP VIEW. Without it the existing view is kept with a warning.
//
// Databases and tables are never dropped, truncated or altered.
func (c *Client) InitSchema(ctx context.Context) (SchemaResult, error) {
	var res SchemaResult
	exists, err := c.DatabaseExists(ctx)
	if err != nil {
		return res, fmt.Errorf("check database %s: %w", c.database, err)
	}
	if !exists {
		if !c.createDatabase {
			return res, fmt.Errorf("%w: %s (or user %s has no access to it); create it, grant access, or set clickhouse.create_database = true",
				ErrDatabaseMissing, c.database, c.user)
		}
		if err := c.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(c.database)); err != nil {
			if Classify(err) == KindPermission {
				return res, fmt.Errorf("create database %s: user %s lacks the CREATE DATABASE privilege; ask an administrator to create it: %w", c.database, c.user, err)
			}
			return res, fmt.Errorf("create database %s: %w", c.database, err)
		}
		res.CreatedDatabase = true
	}
	st, err := c.InspectSchema(ctx)
	if err != nil {
		return res, fmt.Errorf("inspect database %s: %w", c.database, err)
	}
	if len(st.Problems) > 0 {
		return res, &SchemaError{Database: c.database, Problems: st.Problems}
	}
	ddl := tableDDL(c.database)
	for _, name := range st.MissingTables {
		if err := c.Exec(ctx, ddl[name]); err != nil {
			return res, fmt.Errorf("create table %s.%s: %w", c.database, name, err)
		}
		res.CreatedTables = append(res.CreatedTables, name)
	}
	views := map[string]viewDef{}
	for _, v := range viewDefs(c.database) {
		views[v.name] = v
	}
	for _, name := range st.MissingViews {
		if err := c.Exec(ctx, views[name].ddl(c.database, false)); err != nil {
			return res, fmt.Errorf("create view %s.%s: %w", c.database, name, err)
		}
		res.CreatedViews = append(res.CreatedViews, name)
	}
	for _, name := range st.OutdatedViews {
		// CREATE OR REPLACE is atomic but needs DROP TABLE as well (ClickHouse
		// swaps in a temporary table). Without it, fall back to dropping and
		// recreating the view, which needs only DROP VIEW. Only views
		// recognized as Vario's own reach this point.
		err := c.Exec(ctx, views[name].ddl(c.database, true))
		if err != nil && Classify(err) == KindPermission {
			if err = c.Exec(ctx, fmt.Sprintf("DROP VIEW IF EXISTS %s.%s", quoteIdent(c.database), quoteIdent(name))); err == nil {
				err = c.Exec(ctx, views[name].ddl(c.database, false))
			} else if Classify(err) == KindPermission {
				res.Warnings = append(res.Warnings, fmt.Sprintf("view %s is outdated and was kept: updating it needs DROP VIEW on %s.*", name, c.database))
				continue
			}
		}
		if err != nil {
			return res, fmt.Errorf("update view %s.%s: %w", c.database, name, err)
		}
		res.UpdatedViews = append(res.UpdatedViews, name)
	}
	return res, nil
}

// EnsureSchema runs InitSchema; it implements the collector's Sink.
func (c *Client) EnsureSchema(ctx context.Context) error {
	_, err := c.InitSchema(ctx)
	return err
}

// MissingTables lists Vario tables absent from the configured database.
func (c *Client) MissingTables(ctx context.Context) ([]string, error) {
	st, err := c.InspectSchema(ctx)
	return st.MissingTables, err
}

func escapeString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
