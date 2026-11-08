# Architecture notes

This document records the design decisions that are not obvious from the code.

## Pipeline

```
source adapter (reads the source database read-only)
    │  normalize, apply collection mode, redact secrets
    ▼
spool (local SQLite): queue · cursors · fingerprints · lease
    │  deliver: batched, gzip JSONEachRow over HTTP(S), retried with backoff
    ▼
ClickHouse
```

`vario run` (foreground or under launchd) and `vario backfill` use the same `collector` package. One goroutine per source synchronizes into the spool; one delivery goroutine drains the spool.

## T3 Code: snapshot boundary and change detection

- T3 Code is event-sourced. Projection tables (`orchestration_v2_projection_*`) hold current state; `orchestration_events` is the event log.
- `orchestration_v2_projection_metadata` row `thread-projections` holds the projector's high-water sequence `P`. Every read happens inside one SQLite read transaction (a WAL snapshot) and is bounded by `P`, so a snapshot of the projections and the event cursor agree exactly. Events after `P` are left for a later poll.
- Events are used only as **change notifications**. Several event types carry partial payloads (for example, `provider-turn.updated` events contain either `tokenUsage` or `turnTokenUsage`, never both), so the touched entities are re-read from the projections inside the same transaction.
- Sequences may have gaps (compaction). The cursor is "last processed sequence", queried with `sequence > cursor`, so gaps are harmless. If the `event-log-prune-mark` is beyond the cursor the collector reconciles by re-reading all projections. A periodic reconciliation (default 6 h) also covers missed events and schema drift.
- The cursor also stores the `event_id` at its sequence (`anchor`). Event ids are unique and immutable, so a different id at that sequence means the database was replaced or restored, even when the new log is longer than the old one. A log whose maximum sequence is below the cursor is treated the same way. Both start a new epoch (see versions) and a full re-read. A missing anchor event (pruned) is not treated as a replacement.
- The source database is opened with `mode=ro` and `query_only`. Vario never writes, migrates, checkpoints or vacuums it. The runtime schema is inspected (cached by `PRAGMA schema_version`) and optional columns are only selected when present.

## Durability and idempotency

- Records and the cursor that produced them are committed in **one spool transaction**. The cursor never advances past data that is not durably queued. A snapshot only writes its cursor after every batch is committed; an interrupted snapshot is simply repeated.
- **Fingerprints** (hash of the normalized row without its version, plus the privacy policy fingerprint) suppress re-sending unchanged records. This keeps reconciliation and noisy events (for example `thread.visited`) cheap. Changing the privacy settings changes the fingerprint and re-emits rows.
- The spool is bounded (`spool.max_size_mb`). When full, collection pauses without advancing cursors.
- Delivery is at-least-once: entries are deleted from the spool only after ClickHouse acknowledges the insert, so a lost acknowledgement causes a resend, possibly grouped differently. Delivery uses `insert_deduplication_token` (tables set `non_replicated_deduplication_window = 1000`), so a retried identical batch is usually dropped by ClickHouse, but only within that window. Correctness does not depend on this: see versions.
- One collector per data directory holds a lease in the spool (TTL 30 s, renewed every 10 s). The lease records host and PID; a lease whose process no longer exists on this host is taken over immediately. If renewal finds the lease taken by another process, the collector cancels itself so two collectors never keep running.

## Versions and deduplication in ClickHouse

- Mutable tables use `ReplacingMergeTree(version)`. `version` is `epoch << 44 | P`: the projector sequence at read time, with an epoch that increments if the source database is replaced or its event log restarts, so newer states always win. Re-reading unchanged state yields the same version, so re-sent rows are identical.
- Merges are asynchronous. Query through the `*_latest` views (`FINAL`, which also deduplicates across partitions) or `argMax(..., version)`. The provided `token_usage_daily` and `tool_usage` views already do this, so updated or retried provider turns are counted once. This is covered by integration tests against ClickHouse 25.8, including ambiguous (committed but unacknowledged) inserts.
- Partition keys use only immutable values. Conversation-scoped tables are partitioned by `toYYYYMM(conversation_created_at)` so all versions of a row share a partition and replace each other. Rows whose conversation is missing from the source fall back to 1970-01.
- `raw_events` is keyed by `(source_id, sequence, event_id)`. Events are re-sent with a newer version when a snapshot is repeated; `raw_events_latest` collapses them.

## Token usage semantics (validated against real T3 Code data)

- `turnTokenUsage` is the provider-reported consumption of a turn. In every observed record `inputTokens >= cachedInputTokens + cacheCreationTokens` and `outputTokens >= reasoningTokens`: cache and reasoning counts are subsets. Vario stores them as reported and never adds them together. It derives `uncached_input_tokens` only when input, cached and cache-creation are all reported and consistent.
- `tokenUsage` is a context-window snapshot (`usedTokens = inputTokens + outputTokens` in every observed record). It is stored in `context_*` columns and must not be summed as consumption.
- Missing or `unavailable` usage is stored as NULL with `usage_status`, never as zero. `partial` usage carries the counts T3 Code reported; they are stored as reported (a lower bound) and the view counts such turns in `turns_incomplete_usage`. The raw usage objects are kept in `attributes` for provenance.
- `usageScope` is stored in `usage_scope` and `token_usage_daily` groups by it. Only `main_agent` (with `hasSubagents = false`) has been observed. If T3 Code starts reporting subagent or cumulative scopes, summing across scopes could double count; that aggregation policy is deliberately left to the query.
- `model` comes from `turnTokenUsage.models` when reported (`model_source = 'usage'`), otherwise from the run's model selection (`'run'`).

## Messages vs. turn items

User and assistant messages appear both in the messages projection and as `user_message` / `assistant_message` turn items. Text is stored once, in `messages`. The corresponding `execution_events` rows carry `message_id` and no text, so they can be used for ordering without double-counting.

## Privacy enforcement point

Privacy filtering and redaction run during normalization, before records reach the spool, so the local spool never holds more than the configured mode allows. Non-`full` modes keep only an allow-list of keys in `attributes` and in raw event `metadata`; unknown fields are only kept in `full` mode. Kept attributes and metadata are redacted in every mode. Project titles and workspace paths count as content (messages mode and above).

Redaction is a set of regular expressions for known credential formats and credential-named assignments. Each rule declares literal hints (for example `sk-ant-`, `authorization`, `token`); a rule is only evaluated on the lines that contain one of its hints and the following line. Every match of a hinted rule contains a hint on its first line, so results equal a full scan (verified by a test against a naive implementation), at several times the throughput.

## Configuration

- One resolution function (`config.Resolve`) is used by every command, the collector, backfill and the background service: `--config`, `$VARIO_CONFIG`, `$VARIO_HOME/config.toml`, `~/.vario/config.toml`. `service install` embeds the resolved absolute path in the LaunchAgent, so the service keeps reading the same file; configuration changes need only `service restart`.
- The default data directory is `$VARIO_HOME/data` or `~/.vario/data`.
- Writes never regenerate an existing file. `config.Update` edits only the lines of the changed keys and verifies the result by decoding: the new document must equal the old one plus exactly the requested values, otherwise the edit is refused and the user is pointed to `vario config edit`. A line counts as a key or table header only if the text before it is a complete TOML document, which keeps look-alike lines inside multi-line strings and arrays from being edited. The result must parse (unknown keys are rejected) and validate before it is written.
- Writes go to a `0600` temporary file in the same directory, are fsynced and renamed over the original, and the directory is synced; a failure leaves the original intact. A `config.toml.lock` file (O_EXCL, considered stale after 30 s) serializes writers across processes, and callers that read the file earlier (interactive `init`, `config set`, `config edit`) pass its SHA-256 fingerprint so a concurrent change is detected instead of overwritten.
- `init` keeps all answers in memory and writes once, after the summary is confirmed; interrupting it writes nothing. Re-running it diffs the reviewed configuration against the loaded one (`config.Diff`) and applies only the changed keys. Credentials stay in the separate password file and are never accepted as arguments.

## ClickHouse schema management

- Database creation and schema creation are separate. `create_database = false` (default) never issues `CREATE DATABASE`; ClickHouse checks the privilege before existence, so even `CREATE DATABASE IF NOT EXISTS` on an existing database fails for users without it. Existence is therefore checked first through `system.databases` (which only lists databases the user can access).
- `InitSchema` (collector first delivery, `doctor --create-schema`, the confirmed offer in `init`) inspects `system.tables` and `system.columns` before any DDL. Existing objects with Vario's names must match: tables need a `ReplacingMergeTree`-family engine and every expected column with the expected type (parsed from the DDL); extra columns are allowed. Otherwise a `SchemaError` is returned and nothing is created. Only missing tables are created, so a schema provisioned by an administrator works for a user with just `SELECT` and `INSERT`.
- Views carry `COMMENT 'vario:<hash of definition>'`. Up-to-date views are left alone (no privilege needed); outdated Vario views (different tag, or untagged with a definition referencing their base table, i.e. created by an earlier version) are replaced with `CREATE OR REPLACE VIEW`, which ClickHouse implements via a temporary table and therefore needs `DROP TABLE`; when that is denied Vario falls back to `DROP VIEW` + `CREATE VIEW`, and without `DROP VIEW` keeps the old view with a warning. Views that Vario did not create are reported, never replaced.
- `CheckConnection` is read-only: ping (network/TLS), `SELECT version()` (authentication), schema inspection, and `CHECK GRANT` for the privileges the current state requires. Errors are classified by ClickHouse error code (516/192/193/194 authentication, 497 permission, 81 unknown database) and transport error text (TLS vs. network).
