package clickhouse

import (
	"context"
	"fmt"
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

func viewDDL(db string) []string {
	t := func(name string) string { return quoteIdent(db) + "." + quoteIdent(name) }
	var out []string
	for _, name := range []string{model.TableSources, model.TableProjects, model.TableConversations, model.TableRuns,
		model.TableMessages, model.TableExecutionEvents, model.TableProviderTurns} {
		out = append(out, fmt.Sprintf("CREATE OR REPLACE VIEW %s AS SELECT * FROM %s FINAL", t(name+"_latest"), t(name)))
	}
	out = append(out,
		fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS
SELECT
  toDate(coalesce(started_at, conversation_created_at)) AS day,
  source_id,
  provider,
  model,
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
GROUP BY day, source_id, provider, model`, t("token_usage_daily"), t(model.TableProviderTurns)),
		fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS
SELECT
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
GROUP BY source_id, event_type, tool_name`, t("tool_usage"), t(model.TableExecutionEvents)),
	)
	return out
}

// EnsureSchema creates the database, tables and views if they are missing.
// Every statement is idempotent.
func (c *Client) EnsureSchema(ctx context.Context) error {
	if err := c.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(c.database)); err != nil {
		exists, cerr := c.databaseExists(ctx)
		if cerr != nil || !exists {
			return fmt.Errorf("create database %s: %w", c.database, err)
		}
	}
	ddl := tableDDL(c.database)
	for _, name := range model.Tables {
		if err := c.Exec(ctx, ddl[name]); err != nil {
			return fmt.Errorf("create table %s: %w", name, err)
		}
	}
	for _, v := range viewDDL(c.database) {
		if err := c.Exec(ctx, v); err != nil {
			return fmt.Errorf("create view: %w", err)
		}
	}
	return nil
}

func (c *Client) databaseExists(ctx context.Context) (bool, error) {
	var rows []struct {
		N int `json:"n"`
	}
	err := c.Query(ctx, fmt.Sprintf("SELECT count() AS n FROM system.databases WHERE name = '%s'", escapeString(c.database)), &rows)
	return err == nil && len(rows) == 1 && rows[0].N == 1, err
}

// MissingTables lists Vario tables absent from the configured database.
func (c *Client) MissingTables(ctx context.Context) ([]string, error) {
	var rows []struct {
		Name string `json:"name"`
	}
	if err := c.Query(ctx, fmt.Sprintf("SELECT name FROM system.tables WHERE database = '%s'", escapeString(c.database)), &rows); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[r.Name] = true
	}
	var missing []string
	for _, t := range model.Tables {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

func escapeString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
