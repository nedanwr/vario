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
- Sequences may have gaps (compaction). The cursor is "last processed sequence", queried with `sequence > cursor`, so gaps are harmless. If the `event-log-prune-mark` is beyond the cursor, or the log's maximum sequence is below the cursor (database replaced), the collector reconciles by re-reading all projections. A periodic reconciliation (default 6 h) also covers missed events and schema drift.
- The source database is opened with `mode=ro` and `query_only`. Vario never writes, migrates, checkpoints or vacuums it. The runtime schema is inspected (cached by `PRAGMA schema_version`) and optional columns are only selected when present.

## Durability and idempotency

- Records and the cursor that produced them are committed in **one spool transaction**. The cursor never advances past data that is not durably queued. A snapshot only writes its cursor after every batch is committed; an interrupted snapshot is simply repeated.
- **Fingerprints** (hash of the normalized row without its version, plus the privacy policy fingerprint) suppress re-sending unchanged records. This keeps reconciliation and noisy events (for example `thread.visited`) cheap. Changing the privacy settings changes the fingerprint and re-emits rows.
- The spool is bounded (`spool.max_size_mb`). When full, collection pauses without advancing cursors.
- Delivery uses `insert_deduplication_token` (tables set `non_replicated_deduplication_window`), so a retried identical insert is dropped by ClickHouse. Correctness does not depend on this: see versions.

## Versions and deduplication in ClickHouse

- Mutable tables use `ReplacingMergeTree(version)`. `version` is `epoch << 44 | P`: the projector sequence at read time, with an epoch that increments if the source event log restarts, so newer states always win.
- Merges are asynchronous. Query through the `*_latest` views (`FINAL`) or `argMax(..., version)`. The provided `token_usage_daily` and `tool_usage` views already do this, so updated or retried provider turns are counted once.
- Partition keys use only immutable values. Conversation-scoped tables are partitioned by `toYYYYMM(conversation_created_at)` so all versions of a row share a partition and replace each other. Rows whose conversation is missing from the source fall back to 1970-01.
- `raw_events` is keyed by `(source_id, sequence, event_id)`.

## Token usage semantics (validated against real T3 Code data)

- `turnTokenUsage` is the provider-reported consumption of a turn. In every observed record `inputTokens >= cachedInputTokens + cacheCreationTokens` and `outputTokens >= reasoningTokens`: cache and reasoning counts are subsets. Vario stores them as reported and never adds them together. It derives `uncached_input_tokens` only when input, cached and cache-creation are all reported and consistent.
- `tokenUsage` is a context-window snapshot (`usedTokens = inputTokens + outputTokens` in every observed record). It is stored in `context_*` columns and must not be summed as consumption.
- Missing, `partial` or `unavailable` usage is stored as NULL with `usage_status`, never as zero. The raw usage objects are kept in `attributes` for provenance.
- `model` comes from `turnTokenUsage.models` when reported (`model_source = 'usage'`), otherwise from the run's model selection (`'run'`).

## Messages vs. turn items

User and assistant messages appear both in the messages projection and as `user_message` / `assistant_message` turn items. Text is stored once, in `messages`. The corresponding `execution_events` rows carry `message_id` and no text, so they can be used for ordering without double-counting.

## Privacy enforcement point

Privacy filtering and redaction run during normalization, before records reach the spool, so the local spool never holds more than the configured mode allows. Non-`full` modes keep only an allow-list of metadata keys in `attributes`; unknown fields are only kept in `full` mode.
