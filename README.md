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

It runs as a single process, either in the foreground or as a per-user background service. Data is buffered in a local spool, so the collector tolerates network outages, ClickHouse downtime, application restarts, and reboots. Delivery is at-least-once; retried or repeated rows carry the same key and version, so the `*_latest` views and the aggregate views count each record once (see [Delivery semantics](#delivery-semantics)).

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
vario init                      # interactive setup: ClickHouse, database, collection mode, sources
vario doctor                    # check configuration, source access, ClickHouse
vario backfill --source t3code  # import existing history
vario run                       # collect continuously in the foreground (Ctrl-C to stop)
vario service install           # or: run in the background at login (macOS)
vario status                    # cursor, spool, delivery and service state
```

### `vario init`

The setup wizard asks, in order, for:

1. **ClickHouse endpoint**: the HTTP(S) interface, default `http://localhost:8123`. Use `https://` for remote servers. Plain `http://` to a remote host is refused unless you explicitly accept it.
2. **Username and password**: the password is read without echo and stored in `~/.vario/clickhouse-password` (mode `0600`), never in the configuration file.
3. **Database**: the name (default `vario`), and whether to **use an existing database** (the default) or **allow Vario to create it if missing**.
4. **Connection test** (optional, read-only): reachability, authentication, whether the database exists, whether existing Vario tables are compatible, and the privileges Vario needs. Failures are reported as network, TLS, authentication, missing database, permission or schema problems. If the check fails you can retry, change the settings, or **save without verification**: a running ClickHouse is not required to configure Vario.
5. **Collection mode**: metrics only (default), messages and metrics, or full execution telemetry. The latter two require typing `yes` (see [Privacy](#privacy)).
6. **Data sources**: T3 Code is detected automatically, or you can enter a custom path. Vario can be configured without T3 Code installed.
7. **Summary**: confirm, or go back to any step. Nothing is written before you confirm.

Init then writes the configuration, prints the next steps, and offers (default: no) to create the database and tables right away. It never installs or starts the background service. Interrupting init (Ctrl-C or end of input) writes nothing.

**Re-running init** is safe. An existing configuration is offered for review, with its current values as defaults; only the settings you change are rewritten (comments, formatting and other settings are preserved), the stored password is kept unless you change it, and the spool, cursors and ClickHouse data are not touched. You can also keep the configuration unchanged, or start over from defaults (the old file is kept as `config.toml.bak-<timestamp>`).

For scripts, every prompt has a flag: `vario init --non-interactive --clickhouse-url https://ch.example.com:8443 --username vario --database vario --password-stdin < pw.txt`. With an existing configuration, add `--update` (apply only the given flags) or `--force` (start over, with a backup). Other flags: `--create-database`, `--mode`, `--allow-sensitive-content`, `--t3code-path`, `--no-t3code`, `--data-dir`, `--skip-connection-check`. If stdin is not a terminal, init runs non-interactively.

## Commands

| Command | Description |
| --- | --- |
| `vario init` | Set up or update the configuration interactively (see [above](#vario-init)). All prompts have flags; `--non-interactive` with `--update` or `--force` for existing configurations. Never installs the service. |
| `vario config path` | Print the configuration file in use. |
| `vario config show` | Show every setting with its effective value; defaults are marked, the password is never shown. |
| `vario config get <key>` / `set <key> <value>` | Read or change one setting, validated and written in place. |
| `vario config set-password [--stdin \| --clear]` | Store (hidden prompt) or remove the ClickHouse password. |
| `vario config edit` | Edit the file in `$VISUAL`/`$EDITOR`; invalid edits are not applied. |
| `vario sources detect` | Find supported sources in default and configured locations. Read-only. |
| `vario sources list` | Show configured sources with their cursor and last sync time. |
| `vario sources add t3code [--path P]` | Validate and enable a source. |
| `vario backfill --source t3code` | Import current state and the event history, then deliver. `--force` re-sends everything; `--no-deliver` only fills the spool. |
| `vario run [--log-file F]` | Run the collector in the foreground. Sources without a cursor are backfilled automatically. |
| `vario status` | Configuration, service, collector, per-source sync and delivery status. Does not contact ClickHouse. |
| `vario doctor [--create-schema]` | Validate configuration and permissions, source access, spool, ClickHouse reachability, authentication, database, schema compatibility and privileges. `--create-schema` creates missing Vario tables and views (and the database, if `create_database = true`). |
| `vario service install` / `restart` / `uninstall` / `status` | Manage the background service (macOS). `restart` applies configuration changes. |
| `vario version` | Print version, commit, and build information. |

Global flags: `--config <path>` (or `$VARIO_CONFIG`), `-v/--verbose`.

Exit codes: `0` success, `1` failure (including failed `doctor` checks), `2` usage or configuration error, `3` incomplete (for example, backfill data spooled but ClickHouse unreachable).

## Configuration

Vario keeps its configuration and local state in **`~/.vario/`** (mode `0700`) on every platform:

| Path | Contents |
| --- | --- |
| `~/.vario/config.toml` | Configuration (mode `0600`). No secrets. |
| `~/.vario/clickhouse-password` | ClickHouse password (mode `0600`), unless `clickhouse.password_file` points elsewhere. |
| `~/.vario/data/` | Spool, cursors and lease (`collector.data_dir`). |

**Which file is used.** Every command, the collector, `backfill` and the background service resolve the configuration the same way:

1. `--config <path>`
2. `$VARIO_CONFIG`
3. `$VARIO_HOME/config.toml`, where `$VARIO_HOME` replaces `~/.vario` (also for the password file and data directory; useful for isolated installations and testing)
4. `~/.vario/config.toml`

`vario config path` prints the result. See [`config.example.toml`](config.example.toml) for every option. Unknown keys are rejected so typos do not go unnoticed.

**Changing settings** without rerunning init:

```bash
vario config show
vario config get clickhouse.url
vario config set clickhouse.url https://localhost:8443
vario config set clickhouse.username vario
vario config set clickhouse.database vario
vario config set clickhouse.create_database false
vario config set sources.t3code.path ~/.t3/userdata/statev2.sqlite
vario config set-password           # hidden prompt; or --stdin, --clear
vario config edit                   # $VISUAL, $EDITOR, else vi (notepad on Windows)
```

Keys use the file's `section.name` form (an unknown key lists all supported ones; `sources.t3code.enabled` and `sources.t3code.path` address the source). Every change is validated before it is written: invalid values, remote plain `http://`, or a content-collecting mode without `privacy.allow_sensitive_content = true` are refused, and the file is left unchanged. Writes are atomic, keep mode `0600`, rewrite only the affected lines, and are serialized between concurrent Vario processes; a change made by another process in the meantime (for example while `vario init` is open) is detected and not overwritten. `vario config edit` runs the editor directly, without a shell, on a copy of the file and only replaces the configuration if the result is valid; otherwise you can edit again, or the copy is kept.

**Credentials.** The ClickHouse password is stored in a separate file (`~/.vario/clickhouse-password`, mode `0600`) or supplied with `$VARIO_CLICKHOUSE_PASSWORD`, which takes precedence. It is never accepted as a command-line argument, never shown by `vario config show`/`get`, and sent only in request headers, never in URLs, logs, or the service definition. OS keychain storage is not implemented yet.

## ClickHouse requirements

- A ClickHouse server reachable over its **HTTP(S) interface** (default ports 8123/8443). Vario does not use the native protocol and needs no Docker or administrative access on the client.
- Tested with ClickHouse **25.8 LTS**. Older versions may work but are untested (privilege checks need 24.5 or newer). ClickHouse Cloud and other managed services should work over HTTPS but have not been tested.
- Plain `http://` is accepted only for `localhost`/loopback unless `allow_insecure_http = true`. Use `https://` for anything remote; custom CAs are supported via `ca_file`.

### Database selection and shared servers

Vario writes only to the database named by `clickhouse.database` (default `vario`). The server can host any number of other databases (Grafana, application telemetry, logs, ...): Vario never reads or changes them, never assumes it owns the server, never uses the `default` database unless you configure it, and needs no global or administrative privileges. A dedicated database for Vario is the recommended convention, not a requirement.

- **Existing database** (`create_database = false`, the default): the database must already exist. Vario never runs `CREATE DATABASE` and needs no database-creation privilege. If it is missing, `vario doctor` and the collector report it (`database ... does not exist or user ... has no access to it`) and data stays spooled locally until it is fixed.
- **Vario-managed database** (`create_database = true`): Vario runs `CREATE DATABASE IF NOT EXISTS` when the database is missing, at first delivery or on `vario doctor --create-schema`. An existing database is reused, never dropped or recreated. Without the `CREATE DATABASE` privilege this is reported as a permission problem.

### Schema initialization

Creating the database and creating Vario's tables are separate steps, so a user who may create tables in an existing database but not create databases is fully supported. The collector (at its first delivery) and `vario doctor --create-schema` use the same idempotent procedure:

1. Check that the database exists; create it only if it is missing and `create_database = true`.
2. Inspect existing objects with Vario's names. A table with a different engine or a missing or differently typed column, or a view or table Vario did not create, is reported as an **incompatible schema** and nothing is created or changed (Vario does not rely on `CREATE TABLE IF NOT EXISTS` to imply compatibility). Extra columns are allowed.
3. Create only the missing tables, then the missing views.
4. Views carry a version tag in their comment. An outdated Vario view (after an upgrade) is replaced; this needs `DROP VIEW` and is otherwise skipped with a warning while the old view keeps working.

Tables and databases are never dropped, truncated or altered. `vario init` and `vario doctor` (without `--create-schema`) only read; the wizard creates the database or tables only if you confirm its final offer.

### ClickHouse permissions

Privileges for the configured user, all on the target database only:

| Privilege | Needed for |
| --- | --- |
| `SELECT`, `INSERT` on `vario.*` | Always |
| `CREATE TABLE`, `CREATE VIEW` on `vario.*` | Creating the schema (not needed if an administrator created it) |
| `DROP VIEW` on `vario.*` | Optional: updating Vario's views after an upgrade |
| `CREATE DATABASE` on `vario.*` | Only with `create_database = true` and a missing database |

Vario also reads `system.databases`, `system.tables` and `system.columns`, which ClickHouse users can read by default (rows are limited to what the user may access). For example, with an administrator creating the database:

```sql
CREATE DATABASE IF NOT EXISTS vario;
CREATE USER vario IDENTIFIED BY '...';
GRANT SELECT, INSERT, CREATE TABLE, CREATE VIEW, DROP VIEW ON vario.* TO vario;
```

`vario doctor` (and the init connection test) checks these with `CHECK GRANT` and lists missing ones.

### Connection check results

| Reported as | Meaning and fix |
| --- | --- |
| network | Server not reachable: check that it is running, the host and port, firewalls and proxies. |
| tls | TLS failed: use `https://` with a TLS port; set `clickhouse.ca_file` for a private CA. |
| authentication | Wrong username or password: `vario config set clickhouse.username ...`, `vario config set-password`. |
| database missing | The database does not exist or the user cannot see it: create it, grant access, or `vario config set clickhouse.create_database true`. |
| permission | A privilege from the table above is missing. |
| schema | Objects with Vario's names that Vario cannot use: choose a dedicated database or remove them. |

None of these prevent saving the configuration. Without a reachable ClickHouse, `vario run` and `vario backfill` keep collecting into the local spool and deliver once the problem is fixed.

### Schema

| Table | Contents | Engine / key |
| --- | --- | --- |
| `sources` | One row per collected installation: host name, source database path, collection mode | `ReplacingMergeTree(version)` by `source_id` |
| `projects` | Projects/workspaces; title and workspace path in messages+ mode | by `(source_id, project_id)` |
| `conversations` | Threads: provider, model, mode, lineage, timestamps, title (messages+ mode) | by `(source_id, conversation_id)` |
| `runs` | User requests and their model selection | by `(source_id, conversation_id, run_id)` |
| `messages` | User/assistant messages; text in messages+ mode, token bloom-filter index | by `(source_id, conversation_id, message_id)` |
| `execution_events` | Turn items: commands, tools, file changes, reasoning, errors; status, exit code, duration, sizes; payloads in full mode | by `(source_id, conversation_id, event_id)` |
| `provider_turns` | Token usage and context snapshot per provider turn | by `(source_id, conversation_id, provider_turn_id)` |
| `raw_events` | Source event log: type, ids, time, known metadata keys; payloads in full mode | by `(source_id, sequence, event_id)` |

Mutable tables keep every version until ClickHouse merges them; the highest `version` wins. Because merges are asynchronous, query the `*_latest` views (which use `FINAL`, including `raw_events_latest`) or use `argMax(..., version)`; plain `SELECT`s on the base tables can return several versions of a row. Two aggregate views are provided: `token_usage_daily` (per day, source, provider, model and usage scope) and `tool_usage`.

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

Token columns follow the source's reporting: `input_tokens` already includes cached and cache-creation tokens, and reasoning tokens are not added to `output_tokens`. `context_*` columns are context-window snapshots and must not be summed. Missing usage is `NULL`, not zero. Usage the source marks `partial` is stored as reported (a lower bound) and counted in `turns_incomplete_usage`. `token_usage_daily` keeps `usage_scope` as a grouping column; T3 Code currently reports only `main_agent`, and other scopes should not be added together until their relationship is known. See [docs/architecture.md](docs/architecture.md) for details.

## Collector operation

- **Backfill** reads a consistent snapshot of the source's projections plus its event history, bounded by the source's projection high-water mark, and records that boundary as the incremental cursor. There is no gap or overlap between backfill and live sync.
- **Incremental sync** polls the event log every `poll_interval` and re-reads the entities touched by new events. Sequence gaps from compaction are handled.
- **Reconciliation** re-reads all source state every `reconcile_interval` (and whenever the event log was compacted past the cursor or replaced). Unchanged records are not re-sent.
- **Spool**: records and the cursor are committed atomically to a local SQLite database. Delivery batches rows per table, compresses them, and retries with exponential backoff and jitter. The spool is bounded; collection pauses when it is full.
- **Only one collector** runs per data directory (lease). `vario backfill` refuses to run while the collector is active, because the collector backfills new sources itself. A lease left by a collector that crashed on the same host is taken over immediately; otherwise it expires after 30 s. A collector that loses its lease (for example after the machine slept past the expiry and another collector started) stops with exit code 1.
- **Source replacement**: the cursor records the id of the last processed event. If the T3 Code database is replaced or restored from a backup, the collector detects it, re-reads the full state, and starts a new version epoch so the replacement's state wins.
- **Shutdown**: `SIGINT`/`SIGTERM` stop polling and delivery; pending data stays spooled and is delivered on the next start.

### Delivery semantics

- Transport is **at-least-once**. A record is removed from the spool only after ClickHouse acknowledged the insert. If a response is lost after ClickHouse committed the insert (timeout, dropped connection, termination), the batch is sent again.
- Re-sent rows are identical (same key and version), and a newer state always has a higher version, so `FINAL` views, `argMax(..., version)` and the aggregate views return each logical record once. Physical duplicates exist in the base tables until background merges remove them (`OPTIMIZE TABLE ... FINAL` forces this).
- `insert_deduplication_token` additionally drops an identical retried batch, but only within ClickHouse's deduplication window (last 1000 blocks per table, non-replicated tables). Correctness does not depend on it.
- Vario does not provide exactly-once delivery, and ClickHouse is not told about records deleted in the source (see limitations).

### Recovery

- **ClickHouse unreachable**: data stays spooled; delivery retries with exponential backoff up to `retry_max_backoff`. `vario status` shows pending rows and the last error. Batches rejected with a non-retryable error (for example a schema mismatch) are retried every `retry_max_backoff` and do not block other batches.
- **Spool full** (`spool.max_size_mb`): collection pauses without advancing cursors and resumes when delivery catches up; nothing is dropped.
- **Re-sending everything** (for example after dropping the ClickHouse database): stop the collector, then `vario backfill --source t3code --force`.
- **Resetting local state**: stop the collector and delete `state.db*` in the data directory. Undelivered spooled data is lost; the next start re-reads the source's current state (events that T3 Code has already pruned cannot be recovered).

## Background service

```bash
vario service install     # per-user LaunchAgent, starts now and at every login
vario service status
vario service uninstall   # stops gracefully and removes the agent; data and config are kept
```

On macOS, Vario installs a per-user LaunchAgent (`~/Library/LaunchAgents/io.github.nedanwr.vario.plist`) that runs `vario run` with your configuration, starts at login, restarts after abnormal exits (throttled to once per 30 s), and runs at background priority. Logs go to `~/Library/Logs/Vario/vario.log` (mode `0600`, rotated at 10 MiB, one previous file kept); `vario.stdout.log` and `vario.stderr.log` in the same directory receive only startup failures and are not rotated. No privileged daemon is installed and nothing is installed unless you run `vario service install`.

The agent runs `vario run --config <path>` with the configuration file resolved at install time (normally `~/.vario/config.toml`); the plist contains no credentials, and the service reads the password file like the CLI. **Configuration changes do not require reinstalling**: after `vario config set`, `vario config edit` or rerunning `vario init`, Vario reminds you that the running service still uses the previous settings, and `vario service restart` applies them (the configuration is validated first, so a broken edit does not stop a working collector; the spool and cursors are kept). Vario never restarts the service on its own. Re-run `vario service install` only after moving the executable or switching to a different configuration file. A configuration error makes the service exit with a failure, so launchd retries it every 30 s until it is fixed or uninstalled.

Linux (systemd user units) and Windows service installation are **not implemented**. On those platforms, run `vario run` under your own supervisor.

## Privacy

Vario handles potentially sensitive content. Transcripts and command output can contain credentials, private files, and proprietary source code.

- **Collection modes** (shown in the configuration and in `vario status`):
  - `metrics` (default): token usage, models, timings, tool names, statuses, exit codes, and content sizes. No message text, conversation or project titles, workspace paths, tool inputs/outputs, or file names. Identifiers (conversation, message, project ids) are exported as-is.
  - `messages`: adds user/assistant message text, conversation and project titles, workspace paths, branch names, linked pull requests, attachment metadata, and changed file names.
  - `full`: adds command and tool inputs/outputs, reasoning text, file diffs, web searches, raw event payloads and metadata, and every other source field.
- Every mode exports the machine's host name and the source database path (`sources` table) so installations can be told apart. Raw source events outside `full` mode carry only their type, ids, timestamp and a fixed allow-list of operational metadata keys; outside `full` mode, unknown source fields are dropped.
- `messages` and `full` require `allow_sensitive_content = true`, which `vario init` only sets after you type `yes`.
- **Redaction** runs before data is written to the spool, on every exported text field and kept JSON attribute. Built-in rules cover private keys, Anthropic/OpenAI/AWS/GitHub/Slack/Stripe/Google keys, JWTs, `Authorization` headers (bearer, basic and others), credentials in URLs and connection strings, and assignments to credential-named keys (`password=`, `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=`, `"apiKey": "..."`, `token: ...`), including inside JSON. Add your own with `extra_redaction_patterns`. Redaction counts are recorded per row.
- **What redaction does not do**: it matches known formats and credential-named assignments. It does not detect secrets without a recognizable shape or name (for example a bare random password in prose, or a key pasted on its own), personal data, or proprietary code. Treat `messages` and `full` data in ClickHouse as sensitive, and protect it accordingly.
- Long fields are truncated at `max_field_bytes` and flagged.
- Logs contain counts, identifiers, and error summaries, never message contents or credentials. ClickHouse errors that echo row data are sanitized.
- Local files: the configuration, password file, spool database (including its `-wal`/`-shm` files) and log files are created with mode `0600`, and directories Vario creates with `0700`. The password is sent only in the `X-ClickHouse-Key` header; HTTP redirects are never followed, so it cannot be forwarded to another host.
- **No telemetry.** Vario sends data only to the ClickHouse endpoint you configure.
- Source databases are opened read-only and are never modified.

Changing the collection or redaction settings applies to records collected afterwards. Existing records are re-sent under the new policy at the next periodic reconciliation (`reconcile_interval`) or on `vario backfill --source t3code --force`; this replaces the latest version of each row, but older versions remain in the base tables until ClickHouse merges them. Lowering the mode therefore does not immediately remove content already stored in ClickHouse; to purge it, delete it in ClickHouse yourself (for example `OPTIMIZE TABLE vario.messages FINAL` after re-sending, or dropping the affected tables and re-sending with `--force`).

## Current limitations

- T3 Code is the only supported source.
- Service installation is implemented for macOS only.
- Credentials are stored in a `0600` file, not in the OS keychain.
- Tables are created as non-replicated `ReplacingMergeTree`; for ClickHouse clusters, create replicated equivalents yourself before running Vario.
- Deletions in the source are reflected only for conversations and projects (`deleted_at` / `is_deleted`); messages or items hard-deleted from T3 Code remain archived in ClickHouse.
- Events T3 Code prunes from its event log before Vario reads them are missing from `raw_events`; the projected state is still collected through reconciliation.
- A replaced T3 Code database is detected by the event id at the cursor. If T3 Code has also pruned that event, detection falls back to the log-length check and the periodic reconciliation.
- Delivery is at-least-once; see [Delivery semantics](#delivery-semantics). Base tables can contain duplicate rows until merges.
- Schema evolution of the ClickHouse tables is not automated: missing tables are created and incompatible ones are reported, but existing tables are never altered. Views are updated automatically.
- Privilege checks use `CHECK GRANT` (ClickHouse 24.5+); on older servers they are reported as unverified.
- Configuration writes are serialized between Vario processes with a lock file. Changes made by other programs at the same moment are detected by `vario init`, `vario config set` and `vario config edit` (content fingerprint), but editors that keep the file open may still overwrite Vario's changes when they save.
- Not tested against ClickHouse Cloud or ClickHouse versions other than 25.8.

## Development

```bash
make test             # unit tests (synthetic fixture databases; no private data)
make race             # tests with the race detector
make lint             # gofmt, go vet, staticcheck
make clickhouse-up    # start a local ClickHouse 25.8 in Docker on 127.0.0.1:18123
make integration      # ClickHouse integration tests (schema, versioned dedup, aggregation, ambiguous retries)
make clickhouse-down
```

Integration tests are skipped unless `VARIO_TEST_CLICKHOUSE_URL` is set (optionally with `VARIO_TEST_CLICKHOUSE_USER` and `VARIO_TEST_CLICKHOUSE_PASSWORD`). Each run uses a throwaway database.

Layout:

```text
cmd/vario/                 entry point
internal/cli/              commands
internal/config/           configuration file, validation, paths, in-place editing
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
