# Vario

**Observe everything AI.**

Vario is a lightweight, cross-platform command-line tool for AI observability. It collects, archives, and analyzes your AI interactions: conversations, token usage, tool executions, and other execution telemetry.

> **Status:** v0.1 (development). The T3 Code → ClickHouse pipeline is functional. Interfaces and the ClickHouse schema may still change before a tagged release.

## What it does

Vario reads the local data of AI applications on your machine (read-only), normalizes it, and delivers it to a ClickHouse database you control:

- **Conversations**, runs, and messages
- **Execution events**: commands, tool calls, file changes, web searches, reasoning, errors, checkpoints
- **Token usage** per provider turn, with cached, cache-creation, uncached, output, and reasoning tokens tracked separately and their provenance preserved
- **Raw source events** as an audit log

It runs as a single process, either in the foreground or as a per-user background service. Data is buffered in a local spool, so the collector tolerates network outages, ClickHouse downtime, application restarts, and reboots without losing or duplicating data.

## Supported integrations

| Source | Status |
| --- | --- |
| T3 Code (`~/.t3/userdata/statev2.sqlite`) | Supported: backfill, continuous sync, token usage |
| ChatGPT, Claude web/mobile, Codex CLI, Claude Code, browser extensions | Not implemented |

## Install and build

Requires Go 1.25 or newer. The binary is a standalone executable with no CGO and no runtime dependencies.

```bash
git clone https://github.com/nedanwr/vario.git
cd vario
make build            # writes bin/vario with version information
# or: go build -o bin/vario ./cmd/vario
```

Cross-compile with `GOOS`/`GOARCH`, e.g. `GOOS=linux GOARCH=arm64 make build`.

## Quick start

```bash
vario init                      # interactive setup: source, ClickHouse, collection mode
vario doctor                    # check configuration, source access, ClickHouse
vario backfill --source t3code  # import existing history
vario run                       # collect continuously in the foreground (Ctrl-C to stop)
vario service install           # or: run in the background at login (macOS)
vario status                    # cursor, spool, delivery and service state
```

## Commands

| Command | Description |
| --- | --- |
| `vario init` | Create the configuration interactively. All prompts have flags (`--clickhouse-url`, `--mode`, `--password-stdin`, `--non-interactive`, ...). Never installs the service. |
| `vario sources detect` | Find supported sources in default and configured locations. Read-only. |
| `vario sources list` | Show configured sources with their cursor and last sync time. |
| `vario sources add t3code [--path P]` | Validate and enable a source. |
| `vario backfill --source t3code` | Import current state and the event history, then deliver. `--force` re-sends everything; `--no-deliver` only fills the spool. |
| `vario run [--log-file F]` | Run the collector in the foreground. Sources without a cursor are backfilled automatically. |
| `vario status` | Configuration, service, collector, per-source sync and delivery status. Does not contact ClickHouse. |
| `vario doctor [--create-schema]` | Validate configuration and permissions, source access, spool, ClickHouse reachability, authentication and schema. |
| `vario service install` / `uninstall` / `status` | Manage the background service (macOS). |
| `vario version` | Print version, commit, and build information. |

Global flags: `--config <path>` (or `$VARIO_CONFIG`), `-v/--verbose`.

Exit codes: `0` success, `1` failure (including failed `doctor` checks), `2` usage or configuration error, `3` incomplete (for example, backfill data spooled but ClickHouse unreachable).

## Configuration

`vario init` writes the configuration file with mode `0600`:

| Platform | Configuration | Data (spool, cursors) |
| --- | --- | --- |
| macOS | `~/Library/Application Support/vario/config.toml` | `~/Library/Application Support/vario/` |
| Linux | `$XDG_CONFIG_HOME/vario/config.toml` | `$XDG_STATE_HOME/vario/` (`~/.local/state/vario/`) |
| Windows | `%AppData%\vario\config.toml` | `%LocalAppData%\vario\` |

See [`config.example.toml`](config.example.toml) for every option. Unknown keys are rejected so typos do not go unnoticed.

**Credentials.** The ClickHouse password is stored in a separate file (`clickhouse-password` next to the configuration, mode `0600`) or supplied with `$VARIO_CLICKHOUSE_PASSWORD`, which takes precedence. It is sent only in request headers, never in URLs, logs, or the service definition. OS keychain storage is not implemented yet.

## ClickHouse requirements

- A ClickHouse server reachable over its **HTTP(S) interface** (default ports 8123/8443). Vario does not use the native protocol and needs no Docker or administrative access on the client.
- Tested with ClickHouse **25.8 LTS**. Older versions may work but are untested. ClickHouse Cloud and other managed services should work over HTTPS but have not been tested.
- Plain `http://` is accepted only for `localhost`/loopback unless `allow_insecure_http = true`. Use `https://` for anything remote; custom CAs are supported via `ca_file`.
- Privileges for the configured user on the target database: `CREATE TABLE`, `CREATE VIEW`, `INSERT`, `SELECT`, plus `SELECT` on `system.tables` and `system.databases`. `CREATE DATABASE` is only needed if the database does not exist yet. For example:

  ```sql
  CREATE DATABASE IF NOT EXISTS vario;
  CREATE USER vario IDENTIFIED BY '...';
  GRANT CREATE TABLE, CREATE VIEW, INSERT, SELECT ON vario.* TO vario;
  GRANT SELECT ON system.tables TO vario;
  GRANT SELECT ON system.databases TO vario;
  ```

The schema is created automatically on first delivery (or with `vario doctor --create-schema`). All statements are idempotent.

### Schema

| Table | Contents | Engine / key |
| --- | --- | --- |
| `sources` | One row per collected installation | `ReplacingMergeTree(version)` by `source_id` |
| `projects` | Projects/workspaces | by `(source_id, project_id)` |
| `conversations` | Threads: provider, model, mode, lineage, timestamps, title (messages+ mode) | by `(source_id, conversation_id)` |
| `runs` | User requests and their model selection | by `(source_id, conversation_id, run_id)` |
| `messages` | User/assistant messages; text in messages+ mode, token bloom-filter index | by `(source_id, conversation_id, message_id)` |
| `execution_events` | Turn items: commands, tools, file changes, reasoning, errors; status, exit code, duration, sizes; payloads in full mode | by `(source_id, conversation_id, event_id)` |
| `provider_turns` | Token usage and context snapshot per provider turn | by `(source_id, conversation_id, provider_turn_id)` |
| `raw_events` | Source event log (payloads in full mode) | by `(source_id, sequence, event_id)` |

Mutable tables keep every version; the highest `version` wins. Because ClickHouse merges asynchronously, query the `*_latest` views (which use `FINAL`) or use `argMax(..., version)`. Two aggregate views are provided: `token_usage_daily` and `tool_usage`.

Example queries:

```sql
-- Monthly consumption by model; cached tokens are a subset of input tokens.
SELECT toStartOfMonth(day) AS month, provider, model,
       sum(input_tokens) AS input, sum(cached_input_tokens) AS cached,
       sum(uncached_input_tokens) AS uncached, sum(output_tokens) AS output,
       sum(reasoning_tokens) AS reasoning
FROM vario.token_usage_daily GROUP BY ALL ORDER BY month DESC, input DESC;

-- Conversations per project.
SELECT p.title AS project, count() AS conversations
FROM vario.conversations_latest c
LEFT JOIN vario.projects_latest p USING (source_id, project_id)
GROUP BY project ORDER BY conversations DESC;

-- Tool failure rates.
SELECT event_type, tool_name, executions, failures, round(failures / executions, 3) AS failure_rate
FROM vario.tool_usage ORDER BY executions DESC;

-- One execution session in order.
SELECT ordinal, event_type, tool_name, status, exit_code, duration_ms
FROM vario.execution_events_latest WHERE conversation_id = '...' ORDER BY ordinal;

-- Full-text search (messages or full mode).
SELECT conversation_id, created_at, role, substring(text, 1, 200)
FROM vario.messages_latest WHERE hasToken(lower(text), 'kubernetes') ORDER BY created_at DESC LIMIT 20;
```

Token columns follow the source's reporting: `input_tokens` already includes cached and cache-creation tokens, and reasoning tokens are not added to `output_tokens`. `context_*` columns are context-window snapshots and must not be summed. Missing usage is `NULL`, not zero. See [docs/architecture.md](docs/architecture.md) for details.

## Collector operation

- **Backfill** reads a consistent snapshot of the source's projections plus its event history, bounded by the source's projection high-water mark, and records that boundary as the incremental cursor. There is no gap or overlap between backfill and live sync.
- **Incremental sync** polls the event log every `poll_interval` and re-reads the entities touched by new events. Sequence gaps from compaction are handled.
- **Reconciliation** re-reads all source state every `reconcile_interval` (and whenever the event log was compacted past the cursor or replaced). Unchanged records are not re-sent.
- **Spool**: records and the cursor are committed atomically to a local SQLite database. Delivery batches rows per table, compresses them, and retries with exponential backoff and jitter. The spool is bounded; collection pauses when it is full.
- **Only one collector** runs per data directory (lease). `vario backfill` refuses to run while the collector is active, because the collector backfills new sources itself.
- **Shutdown**: `SIGINT`/`SIGTERM` stop polling and delivery; pending data stays spooled and is delivered on the next start.

## Background service

```bash
vario service install     # per-user LaunchAgent, starts now and at every login
vario service status
vario service uninstall   # stops gracefully and removes the agent; data and config are kept
```

On macOS, Vario installs a per-user LaunchAgent (`~/Library/LaunchAgents/io.github.nedanwr.vario.plist`) that runs `vario run` with your configuration, starts at login, restarts after abnormal exits (throttled to once per 30 s), and runs at background priority. Logs go to `~/Library/Logs/Vario/vario.log` (rotated at 10 MiB). No privileged daemon is installed. Re-run `vario service install` after moving the executable or the configuration.

Linux (systemd user units) and Windows service installation are **not implemented**. On those platforms, run `vario run` under your own supervisor.

## Privacy

Vario handles potentially sensitive content. Transcripts and command output can contain credentials, private files, and proprietary source code.

- **Collection modes** (shown in the configuration and in `vario status`):
  - `metrics` (default): token usage, models, timings, tool names, statuses, exit codes, and content sizes. No message text, titles, tool inputs/outputs, or file names.
  - `messages`: adds user/assistant message text, conversation titles, branch names, attachment metadata, and changed file names.
  - `full`: adds command and tool inputs/outputs, reasoning text, file diffs, web searches, and raw event payloads.
- `messages` and `full` require `allow_sensitive_content = true`, which `vario init` only sets after you type `yes`.
- **Redaction** runs before data is written to the spool. Built-in rules cover private keys, Anthropic/OpenAI/AWS/GitHub/Slack/Stripe/Google keys, JWTs, bearer tokens, credentials in URLs, and `password=`/`secret:`-style assignments. Add your own with `extra_redaction_patterns`. Redaction counts are recorded per row. Redaction is best-effort and cannot catch every secret.
- Long fields are truncated at `max_field_bytes` and flagged.
- Logs contain counts, identifiers, and error summaries, never message contents or credentials. ClickHouse errors that echo row data are sanitized.
- **No telemetry.** Vario sends data only to the ClickHouse endpoint you configure.
- Source databases are opened read-only and are never modified.

Changing the collection mode applies to records collected afterwards. Existing records are re-sent under the new policy only when they change or on `vario backfill --source t3code --force`. Lowering the mode does not delete content already stored in ClickHouse.

## Current limitations

- T3 Code is the only supported source.
- Service installation is implemented for macOS only.
- Credentials are stored in a `0600` file, not in the OS keychain.
- Tables are created as non-replicated `ReplacingMergeTree`; for ClickHouse clusters, create replicated equivalents yourself before running Vario.
- Deletions in the source are reflected only for conversations and projects (`deleted_at` / `is_deleted`); messages or items hard-deleted from T3 Code remain archived in ClickHouse.
- If the T3 Code database is replaced by a new one whose event log has already grown past the old cursor, Vario cannot detect the reset until the next periodic reconciliation.
- Schema evolution of the ClickHouse tables is not yet automated beyond `CREATE ... IF NOT EXISTS`.
- Not tested against ClickHouse Cloud or ClickHouse versions other than 25.8.

## Development

```bash
make test             # unit tests (synthetic fixture databases; no private data)
make race             # tests with the race detector
make lint             # gofmt, go vet, staticcheck
make clickhouse-up    # start a local ClickHouse 25.8 in Docker on 127.0.0.1:18123
make integration      # ClickHouse integration tests (schema, versioned dedup, retries, search)
make clickhouse-down
```

Integration tests are skipped unless `VARIO_TEST_CLICKHOUSE_URL` is set (optionally with `VARIO_TEST_CLICKHOUSE_USER` and `VARIO_TEST_CLICKHOUSE_PASSWORD`). Each run uses a throwaway database.

Layout:

```text
cmd/vario/                 entry point
internal/cli/              commands
internal/config/           configuration file, validation, paths
internal/collector/        sync loop, snapshot/reconcile, delivery
internal/sources/          source interface
internal/sources/t3code/   T3 Code adapter (read-only SQLite)
internal/spool/            local durable queue, cursors, fingerprints, lease
internal/storage/clickhouse/ HTTP client and schema
internal/privacy/          collection modes and redaction
internal/service/launchd/  macOS LaunchAgent
internal/model/            normalized row types
```

The module path is `github.com/nedanwr/vario`, derived from the `origin` remote.

## Principles

Vario is open-source and local-first. Your data stays under your control. Vario works without hosted accounts or external telemetry services.

## License

Vario is licensed under the [Apache License 2.0](LICENSE).
