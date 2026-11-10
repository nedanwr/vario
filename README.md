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

Prebuilt binaries for macOS, Linux and Windows (amd64 and arm64) are attached to each [GitHub Release](https://github.com/nedanwr/vario/releases), together with a `checksums.txt` file. Download the archive for your platform, [verify its checksum](#verifying-downloads), extract it, and put `vario` (`vario.exe` on Windows) on your `PATH`. The macOS binaries are not signed or notarized; see [Releases](#releases).

To build from source, you need Go 1.25 or newer. The binary is a standalone executable with no CGO and no runtime dependencies.

```bash
git clone https://github.com/nedanwr/vario.git
cd vario
make build            # writes bin/vario with version information
# or: go build -o bin/vario ./cmd/vario
```

Cross-compile with `GOOS`/`GOARCH`, e.g. `GOOS=linux GOARCH=arm64 make build`.

## Quick start

```bash
vario init                      # interactive setup: ClickHouse access, database, user, collection mode, sources
vario migrate                   # create Vario's schema, if init did not (administrative credentials)
vario doctor                    # check configuration, source access, ClickHouse
vario backfill --source t3code  # import existing history
vario run                       # collect continuously in the foreground (Ctrl-C to stop)
vario service install           # or: run in the background at login (macOS)
vario status                    # cursor, spool, delivery and service state
```

### Credentials: administrative and operational

Vario uses two kinds of ClickHouse credentials:

| | Administrative | Operational |
| --- | --- | --- |
| Used by | `vario init` (guided setup), `vario migrate` | `vario run`, `vario backfill`, the background service, `vario doctor` |
| Used for | listing databases and users, creating the database and a dedicated user, granting privileges, creating or migrating Vario's schema | inserting collected data, reading Vario's schema and destination id |
| Stored | **never**: asked for when needed (hidden prompt or `--admin-password-stdin`), kept in memory for that command only | user name in `clickhouse.username`; password in `~/.vario/clickhouse-password` (mode `0600`) or `$VARIO_CLICKHOUSE_PASSWORD` |
| Privileges | whatever your administrative account has | `SELECT, INSERT` on Vario's eight tables, `SELECT` on its views and `vario_meta`; nothing global, no DDL, no `GRANT OPTION` |

The collector never creates or changes databases, users, privileges, tables or views. If its schema is missing or incompatible it keeps data spooled, reports the problem and points to `vario migrate`.

### `vario init`

The wizard asks for the **ClickHouse endpoint** (the HTTP(S) interface, default `http://localhost:8123`; plain `http://` to a remote host is refused unless you accept it) and then for one of two setup paths.

**Guided setup** (recommended) signs in with an administrative account, which is not saved, and then:

1. **Database**: use an existing database (picked from a list; system databases such as `system`, `default` and `INFORMATION_SCHEMA` are hidden unless you reveal them; long lists can be filtered; names can be entered manually if the account cannot list databases) or create a new one. An existing database with the chosen name is reused, never dropped or recreated.
2. **Operational user**: create a dedicated user (default name `vario`, the default for new databases) or use an existing one. For a new user you type a password or leave it empty to have Vario generate one: 32 characters from `crypto/rand`. You can choose to see it once (for a password manager). It is never logged. The new user's password is written to a private file before the user is created and kept there until a saved configuration uses it (see [If setup is interrupted](#if-setup-is-interrupted)). Vario creates the user with a salted SHA-256 hash that it computes locally, so the password itself is never sent in a statement. For an existing user, Vario first inspects its grants (directly and through roles) and says whether the required privileges appear to be present, missing, or cannot be determined, for example because of partial revokes, non-default roles or no access to `system.grants`. If privileges are missing you can pick another user, display the required SQL, or authorize Vario to grant them. Users defined in server configuration files or LDAP, which cannot be changed with SQL, are labelled and are never offered grants. Vario never changes an existing user's password or authentication settings.
3. **Schema**: offer to create or migrate Vario's schema in the database.
4. **Review**: every change is listed by category (database, schema, user, privileges) and applied only after you confirm. You can display the exact SQL first. If a step fails, the steps already applied are reported (ClickHouse DDL is not transactional), and you can retry the remaining steps, change your selections, or quit.
5. **Verification**: Vario logs in as the operational user, independently of what the administrator saw, and checks what the collector will actually do (see [How access is verified](#how-access-is-verified)). The insert probe sends no rows, so nothing is written.

**Manually managed setup** uses an existing database and operational user without any administrative credentials. You enter the database, user and password; Vario can verify the login (read-only) and, if the database, user or privileges are missing, print the SQL an administrator needs to run. The schema is then created with `vario migrate` (by anyone with administrative credentials; `vario migrate --dry-run` shows its SQL).

Both paths continue with the **collection mode** (metrics only by default; messages or full execution telemetry require typing `yes`, see [Privacy](#privacy)), the **data sources** (T3 Code is detected automatically), and a **summary**. The summary shows the endpoint, database and operational user with how each was obtained, the password status (never the password), the privileges and schema status, the connection check, the collection mode, the sources and the configuration file. Only the operational configuration and password are saved. Changes made in ClickHouse were applied when you confirmed them; quitting or interrupting init says which ClickHouse changes, if any, remain. Init never installs or starts the background service.

#### If setup is interrupted

A user created by guided setup must never be left with a password nobody has. Before Vario sends `CREATE USER`, it writes the new user's password to `~/.vario/pending-credentials.json` (next to the configuration, mode `0600`; never administrative credentials). The entry is removed only after a configuration that uses the password has been saved. If the user certainly was not created (the statement failed and the user does not exist, or setup stopped before it), the entry is removed at once. If the file cannot be written, the user is not created.

- **Quit, Ctrl-C, closed input, a failed step, a crash or `SIGKILL`**: the password stays in the file. Ctrl-C restores the terminal (also during a hidden password prompt) and prints what was applied and where the password is kept; a crash or `SIGKILL` prints nothing, but the file was written before the user existed.
- **Next `vario init`**: it offers to *finish the setup with this user* (it verifies the login and continues with the remaining steps; if ClickHouse never created the user, guided setup creates it with the kept password), to continue without it (the file is kept), or to delete the saved password (confirmation required). When the same user is chosen again in guided setup, the kept password is offered. `vario doctor` reports unfinished setups.
- **If the password is lost anyway** (for example the file was deleted): Vario cannot recover it and never changes an existing user's credentials. An administrator can set a new one (`ALTER USER vario IDENTIFIED WITH sha256_password BY '...'`) and you then enter it with `vario init` (manually managed setup) or `vario config set-password`. Alternatively, drop the user (`DROP USER vario`) and run guided setup again.

**Re-running init** is safe: existing databases, users and schema objects are discovered and reused, so nothing is duplicated, and only the settings you change are rewritten, preserving comments and formatting. The stored password is kept unless you change it. You can also keep the configuration unchanged or start over from defaults; the old file is kept as `config.toml.bak-<timestamp>`.

For scripts, init runs the manually managed path non-interactively: `vario init --non-interactive --clickhouse-url https://ch.example.com:8443 --username vario --database vario --password-stdin < pw.txt`, then `vario migrate --non-interactive --yes --admin-user default --admin-password-stdin < admin-pw.txt`. With an existing configuration, add `--update` (apply only the given flags) or `--force` (start over, with a backup). Other flags: `--mode`, `--allow-sensitive-content`, `--t3code-path`, `--no-t3code`, `--data-dir`, `--skip-connection-check`. If stdin is not a terminal, init runs non-interactively. `--create-database` is deprecated and ignored.

## Commands

| Command | Description |
| --- | --- |
| `vario init` | Set up or update ClickHouse access and the configuration (see [above](#vario-init)): guided (administrative credentials, not saved) or manually managed. `--non-interactive` with `--update` or `--force` for existing configurations. Never installs the service. |
| `vario migrate [--dry-run]` | Create or update Vario's schema after showing the plan and asking for confirmation; asks for administrative credentials only when something has to change (see [Schema migrations](#schema-migrations)). |
| `vario destination` / `destination switch` | Show which destination queued data belongs to; switch to the configured destination after a change, keeping, moving or discarding queued rows (see [Destinations](#destinations-and-queued-data)). |
| `vario config path` | Print the configuration file in use. |
| `vario config show` | Show every setting with its effective value; defaults are marked, the password is never shown. |
| `vario config get <key>` / `set <key> <value>` | Read or change one setting, validated and written in place. |
| `vario config set-password [--stdin \| --clear]` | Store (hidden prompt) or remove the operational ClickHouse password. |
| `vario config edit` | Edit the file in `$VISUAL`/`$EDITOR`; invalid edits are not applied. |
| `vario sources detect` | Find supported sources in default and configured locations. Read-only. |
| `vario sources list` | Show configured sources with their cursor and last sync time. |
| `vario sources add t3code [--path P]` | Validate and enable a source. |
| `vario backfill --source t3code` | Import current state and the event history, then deliver. `--force` re-sends everything; `--no-deliver` only fills the spool. |
| `vario run [--log-file F]` | Run the collector in the foreground. Sources without a cursor are backfilled automatically. |
| `vario status` | Configuration, service, collector, per-source sync and delivery status. Does not contact ClickHouse. |
| `vario doctor` | Validate configuration and permissions, source access, spool and destinations, and the operational ClickHouse login: reachability, authentication, database, schema version and compatibility, read-only settings, privileges and an insert probe. Read-only. (`--create-schema` is deprecated and runs `vario migrate`.) |
| `vario service install` / `restart` / `uninstall` / `status` | Manage the background service (macOS). `restart` applies configuration changes. |
| `vario version` | Print version, commit, and build information. |

Global flags: `--config <path>` (or `$VARIO_CONFIG`), `-v/--verbose`.

Exit codes: `0` success, `1` failure (including failed `doctor` checks), `2` usage or configuration error, `3` incomplete (for example, backfill data spooled but ClickHouse unreachable).

## Configuration

Vario keeps its configuration and local state in **`~/.vario/`** (mode `0700`) on every platform:

| Path | Contents |
| --- | --- |
| `~/.vario/config.toml` | Configuration (mode `0600`). No secrets. |
| `~/.vario/clickhouse-password` | Operational ClickHouse password (mode `0600`), unless `clickhouse.password_file` points elsewhere. Administrative passwords are never stored. |
| `~/.vario/data/` | Spool, cursors, destinations and lease (`collector.data_dir`). |

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
vario config set sources.t3code.path ~/.t3/userdata/statev2.sqlite
vario config set-password           # hidden prompt; or --stdin, --clear
vario config edit                   # $VISUAL, $EDITOR, else vi (notepad on Windows)
```

Keys use the file's `section.name` form (an unknown key lists all supported ones; `sources.t3code.enabled` and `sources.t3code.path` address the source). Every change is validated before it is written: invalid values, remote plain `http://`, or a content-collecting mode without `privacy.allow_sensitive_content = true` are refused, and the file is left unchanged. Writes are atomic, keep mode `0600`, rewrite only the affected lines, and are serialized between concurrent Vario processes; a change made by another process in the meantime (for example while `vario init` is open) is detected and not overwritten. `vario config edit` runs the editor directly, without a shell, on a copy of the file and only replaces the configuration if the result is valid; otherwise you can edit again, or the copy is kept.

**Credentials.** Only the operational password is stored: in a separate file (`~/.vario/clickhouse-password`, mode `0600`) or supplied with `$VARIO_CLICKHOUSE_PASSWORD`, which takes precedence. Administrative credentials are asked for by `vario init` and `vario migrate` and never stored. No password is accepted as a command-line argument, shown by `vario config show`/`get`, or written to logs, error messages or the service definition; passwords are sent only in request headers. OS keychain storage is not implemented yet.

Changing `clickhouse.url` or `clickhouse.database` (with `config set`, `config edit`, `init` or by editing the file) changes the destination: see [Destinations](#destinations-and-queued-data). `clickhouse.create_database` is deprecated and ignored.

## ClickHouse requirements

- A ClickHouse server reachable over its **HTTP(S) interface** (default ports 8123/8443). Vario does not use the native protocol and needs no Docker or administrative access on the client.
- Tested with self-hosted ClickHouse **24.8 LTS** (24.8.14.39) and **25.8 LTS** (25.8.33.6) in Docker: user creation with a locally computed salted hash, generated passwords and login, grants, the operational login check, schema creation and migrations, and destination switching. Other versions are untested. On 24.8, `CHECK GRANT` does not exist, so the privilege list in the login check is not shown; the insert probe still verifies inserts (see [How access is verified](#how-access-is-verified)).
- Plain `http://` is accepted only for `localhost`/loopback unless `allow_insecure_http = true`. Use `https://` for anything remote; custom CAs are supported via `ca_file`.

### Managed services, external authentication and clusters

- **ClickHouse Cloud and other managed services have not been validated.** User provisioning, privilege management and schema operations may differ there: discovery may be restricted (init then asks for names), and user-management rules (password policies, built-in roles, restricted `CREATE USER`) may reject Vario's statements; failures are reported step by step. A manually provisioned operational account (manually managed setup) is the expected path where guided setup does not work, provided the service exposes the HTTP(S) interface and accepts password authentication with the `X-ClickHouse-User`/`X-ClickHouse-Key` headers.
- **External authentication**: Vario does not provision LDAP, Kerberos, certificate-authenticated, SSH-key or other externally managed users. Guided setup can *select* an existing account of that kind as the operational user (it is labelled and never offered grants), but Vario can only *authenticate* with a user name and password sent over HTTP(S). An LDAP user whose password ClickHouse checks against the directory may work that way (untested); Kerberos, certificate and SSH-key logins do not. The login check shows whether authentication works.
- **Clusters**: Vario does not use `ON CLUSTER` and does not manage replicated schemas. Its tables are created as non-replicated `ReplacingMergeTree` on the server it connects to, and users and grants exist only there unless your access storage is replicated. For a cluster, create the database, replicated tables and grants yourself and let `vario migrate` add only what is missing. Cluster deployments are untested.

### Database selection and shared servers

Vario writes only to the database named by `clickhouse.database` (default `vario`). The server can host any number of other databases (Grafana, application telemetry, logs, ...): Vario never reads or changes them, never assumes it owns the server, and never uses the `default` database unless you choose it. A dedicated database is recommended. In a shared database, the operational user's grants are limited to Vario's own tables and views, so it cannot read or change unrelated tables.

The database is created only by guided `vario init` or `vario migrate`, after you confirm, with administrative credentials. An existing database is reused, never dropped or recreated. The collector never creates databases.

### Schema migrations

`vario migrate` is the only command that changes Vario's schema:

1. It reads the configured endpoint and database and inspects the schema, first with the operational user (no administrative credentials are needed when nothing has to change).
2. It shows the planned operations, grouped by kind; `--dry-run` also prints their SQL and stops there. Add `--admin-user` to make the dry run inspect with administrative credentials, which may see more.
3. It asks for administrative credentials, re-plans with them and asks for confirmation. Creating a missing database, changing the schema, and granting the operational user access to new objects are confirmed separately; `--non-interactive` requires `--yes`.
4. It applies the steps in order, verifies the result, and checks access as the operational user. A failure is reported with the steps that were and were not applied; running `vario migrate` again resumes.

Migrations are versioned (current schema version: **2**; version 1 is the schema of Vario 0.1, which recorded no version), idempotent and additive: missing tables, views and metadata are created and Vario's own outdated views are replaced. Databases and tables are never dropped, truncated or altered. Objects with Vario's names that Vario did not create, or tables with a different engine or column types, are reported as an **incompatible schema** and stop the migration (extra columns are allowed). A schema migrated by a newer Vario is left alone. Version 2 adds `vario_meta`, which records the schema version and a random **destination id** (see [Destinations](#destinations-and-queued-data)).

Administrative credentials are requested only for these authorized changes; routine collection (`vario run`, `vario backfill`, the background service) uses the restricted operational account and never needs them. When a new Vario version needs a newer schema, the collector keeps collecting into the spool and reports "migration required" until an administrator runs `vario migrate` (preview with `--dry-run`). The migration path from version 1 to 2 is tested against ClickHouse 24.8 and 25.8.

Views carry a version tag in their comment. Updating an outdated view needs `DROP TABLE` (for `CREATE OR REPLACE`) or `DROP VIEW`; without either, the old view is kept with a warning.

### ClickHouse permissions

The **operational user** needs, on the target database only:

| Privilege | Objects | Needed for |
| --- | --- | --- |
| `SELECT, INSERT` | the tables `sources`, `projects`, `conversations`, `runs`, `messages`, `execution_events`, `provider_turns`, `raw_events` | inserting collected data; seeing the tables in the schema check |
| `SELECT` | the `*_latest`, `token_usage_daily` and `tool_usage` views, and `vario_meta` | the schema check, the destination id, and querying Vario's data |

It needs no global privileges, no `CREATE`/`DROP`/`ALTER`, no access management and no `GRANT OPTION`. Its sessions must not be read-only (`readonly = 0`). The guided setup and `vario migrate` grant exactly this, one `GRANT` per object. To provision by hand:

```sql
CREATE DATABASE IF NOT EXISTS vario;
CREATE USER vario IDENTIFIED WITH sha256_password BY '...' DEFAULT DATABASE vario;
GRANT SELECT, INSERT ON vario.messages TO vario;   -- and the other seven tables
GRANT SELECT ON vario.messages_latest TO vario;    -- and the other views, and vario.vario_meta
-- then, as an administrator: vario migrate
```

(`GRANT SELECT, INSERT ON vario.* TO vario` also works for a dedicated database.) `vario init` prints the complete list for your database and user.

The **administrative account** needs what the chosen operations need: `SHOW USERS`/`SHOW DATABASES` (or the ability to read `system.users`, `system.databases`, `system.grants`, `system.role_grants`) for discovery; `CREATE DATABASE`; `CREATE USER` and the privileges it grants (with `GRANT OPTION`, or access management); `CREATE TABLE`, `CREATE VIEW`, `INSERT` (for `vario_meta`) and optionally `DROP VIEW` on the database for migrations. If discovery is not permitted, init falls back to manual entry.

`vario doctor` warns if the operational user holds administrative privileges (`CREATE USER`, `CREATE DATABASE`, `DROP DATABASE` on `*.*`); the collector logs the same warning at startup.

### How access is verified

Grant inspection during guided setup reads `system.grants` and `system.role_grants` as the administrator. It is **advisory**: settings profiles, settings constraints, quotas and host restrictions are not visible there, and partial revokes or non-default roles make it inconclusive.

The stronger evidence is the **operational login check** (`vario init`, `vario migrate`, `vario doctor`), which runs as the actual operational user and password from this computer:

| Check | How | Confirms |
| --- | --- | --- |
| authentication | a query as the operational user | the credentials and the host restrictions for this computer |
| schema access | reads Vario's tables, columns and `vario_meta` | everything the collector reads at runtime |
| read-only session | `getSetting('readonly')`, evaluated by the server | the settings profile allows writes |
| privileges | `CHECK GRANT` for `INSERT` on each table and `SELECT` on `vario_meta` (ClickHouse 25.8; not available on 24.8) | explains a failure; not proof on its own |
| inserts | an `INSERT` into every Vario table **with exactly the settings of a delivery** and no rows | privileges, revokes, roles, `readonly`, settings constraints on those settings, and already exhausted quotas, as ClickHouse applies them to the real insert |

Each result is reported as confirmed (✓), failed (✗) or **not verified** (!), for example when the read-only setting cannot be read or the schema does not exist yet, so inserts cannot be tried. The check needs no DDL or administrative privileges and writes nothing. Because it writes no rows, quotas on written bytes, disk space, memory limits and server-side insert limits are confirmed only by the first delivery (`vario status` shows it). Vario does not write test rows into the telemetry tables, and it does not create a separate probe table, which would need extra provisioning.

### Connection check results

| Reported as | Meaning and fix |
| --- | --- |
| network | Server not reachable: check that it is running, the host and port, firewalls and proxies. |
| tls | TLS failed: use `https://` with a TLS port; set `clickhouse.ca_file` for a private CA. |
| authentication | Wrong username or password: `vario config set clickhouse.username ...`, `vario config set-password`. For administrative logins, re-enter the credentials. |
| database missing | The database does not exist or the user has no access to it: create it with `vario init` (guided) or `vario migrate`, or grant access. |
| permission | A privilege is missing (possibly through a revoke): rerun `vario init` to inspect and fix grants, or see the table above. |
| session restriction | The user's settings profile makes sessions read-only, a settings constraint forbids a setting Vario's inserts use, or a quota is exhausted. An administrator has to change the profile or quota; Vario never does. |
| migration required | Vario's schema is missing or incomplete: run `vario migrate`. |
| schema | Objects with Vario's names that Vario cannot use: choose a dedicated database or remove them. |

None of these prevent saving the configuration. Without a reachable or ready ClickHouse, `vario run` and `vario backfill` keep collecting into the local spool and deliver once the problem is fixed.

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
- **No administrative operations**: at startup and after an "unknown table" error, the collector inspects the schema read-only. A missing or incompatible schema stops delivery (data stays spooled) and is reported in the log, `vario status` and `vario doctor`, pointing to `vario migrate`; outdated views are only a warning.

### Destinations and queued data

Every queued record, every source cursor and every change fingerprint belongs to one **destination**. Data queued for one destination is never delivered to another, and a destination has its own cursors, so switching to a new database reads every source again in full for it. Vario does not assume that the new database already has what was delivered to the old one.

**Identity.** A destination is identified by the destination id stored in its `vario_meta` table (schema version 2). Until that id is known (ClickHouse unreachable, or a version 1 schema), it is identified by the normalized endpoint (lower-case scheme and host, explicit port, no trailing slash) and the database name. Credentials are never part of it: changing the operational user or rotating its password keeps the destination. The same database reached through another URL is recognized by its id and keeps its state. A database that was dropped and recreated (and migrated again) has a new id, and is treated as a new destination that must be repopulated.

**Changing the destination** (`vario config set clickhouse.url|database`, `vario config edit`, `vario init`, or editing the file directly; the collector itself checks at every start):

- If nothing is queued for the previous destination, the collector switches automatically, logs it, and backfills the new destination in full. The previous destination's cursors are kept, so switching back resumes where it left off.
- If rows are still queued for the previous destination, or the configured database was replaced, `vario run` and `vario backfill` refuse to start and explain why. Decide with `vario destination switch`:
  - `--keep-pending` keeps the rows for the previous destination; they are delivered if you configure it again.
  - `--move-pending` delivers them to the configured destination instead. Re-sent rows carry the same keys and versions, so the `*_latest` and aggregate views count each record once.
  - `--discard-pending` deletes them (asks for confirmation unless `--yes`). The new destination gets a full backfill anyway; only events that the source has already pruned are lost.

  To deliver the queued rows to the previous destination first, restore its setting, let `vario run` drain the spool (`vario status`), then change the setting again.

**Switching needs the collector to be stopped**, so that nothing is collected or delivered while queued rows are reassigned. If a collector holds the lease, `vario destination switch` changes nothing and exits at once, naming the process (`vario run`, its PID, and whether it is the background service or a collector on another host). Vario never stops a collector on its own. With the background service:

```bash
vario service uninstall                    # stops it; configuration, queued rows and cursors are kept
vario destination switch --keep-pending    # or --move-pending / --discard-pending
vario service install                      # starts it again
```

While a destination change is undecided, the service exits at each start (launchd retries every 30 s) and holds the lease only briefly, so `vario destination switch` usually succeeds without stopping it; if it reports the service, use the sequence above.

- **Queued records** are bound to their destination in the local spool and are never sent elsewhere without an explicit `--move-pending`.
- **Historical data is replayed**: a destination without its own cursors (a new or recreated database) receives a full backfill of everything the source still has. Nothing has to be replayed by hand. Events the source has already pruned cannot be replayed.
- **If switching fails or is interrupted**, nothing is lost. Each step (recording a replaced database, moving or deleting queued rows, activating the destination) is atomic. Rows that were already moved belong to the new destination, and the next start activates it. Otherwise the previous state remains and the collector keeps refusing until you run `vario destination switch` again.

`vario destination` lists the known destinations with their queued rows and cursors; `vario status` and `vario doctor` report rows queued for other destinations.

### Delivery semantics

- Transport is **at-least-once**. A record is removed from the spool only after ClickHouse acknowledged the insert. If a response is lost after ClickHouse committed the insert (timeout, dropped connection, termination), the batch is sent again.
- Re-sent rows are identical (same key and version), and a newer state always has a higher version, so `FINAL` views, `argMax(..., version)` and the aggregate views return each logical record once. Physical duplicates exist in the base tables until background merges remove them (`OPTIMIZE TABLE ... FINAL` forces this).
- `insert_deduplication_token` additionally drops an identical retried batch, but only within ClickHouse's deduplication window (last 1000 blocks per table, non-replicated tables). Correctness does not depend on it.
- Vario does not provide exactly-once delivery, and ClickHouse is not told about records deleted in the source (see limitations).

### Recovery

- **ClickHouse unreachable**: data stays spooled; delivery retries with exponential backoff up to `retry_max_backoff`. `vario status` shows pending rows and the last error. Batches rejected with a non-retryable error (for example a schema mismatch) are retried every `retry_max_backoff` and do not block other batches.
- **Spool full** (`spool.max_size_mb`): collection pauses without advancing cursors and resumes when delivery catches up; nothing is dropped.
- **Re-sending everything**: stop the collector, then `vario backfill --source t3code --force`. After dropping and recreating the database, run `vario migrate` (new destination id), then `vario destination switch`; the collector repopulates the new database by itself.
- **Resetting local state**: stop the collector and delete `state.db*` in the data directory. Undelivered spooled data is lost; the next start re-reads the source's current state (events that T3 Code has already pruned cannot be recovered).

### Upgrading from Vario 0.1

Nothing has to be recreated:

1. **Local state**: on its first start, the new version converts the spool in place. Queued rows, cursors and fingerprints are kept and assigned to the configured destination. If you change `clickhouse.url` or `clickhouse.database` with `vario config set`, `config edit` or `init` before the collector has run with the new version, the old state is bound to the previous destination first. If you edit the file by hand before that first start, it is assigned to the new settings, as 0.1 would have done. The conversion refuses to run while a collector of the old version still holds the lease; restart the service (`vario service restart`).
2. **Schema**: a 0.1 schema (version 1) keeps working: the collector delivers to it and warns. Run `vario migrate` once to add `vario_meta`; this records the destination id and, if the collector user lacks `SELECT` on the new table, offers to grant it.
3. **Credentials**: the existing user and password stay in place as the operational credentials. If that account has administrative privileges (common when one account was used for everything), `vario doctor` and the collector log say so. Run `vario init`, choose guided setup and "Create a dedicated Vario user" to switch to a restricted account; Vario does not change or revoke anything on the old account.
4. **`create_database`**: ignored; set it to `false` (`vario config set clickhouse.create_database false`) or delete the line with `vario config edit` to silence the warning. Databases are created by `vario init` or `vario migrate`.

A service installed by 0.1 keeps working after the upgrade without reinstalling. It runs the old binary until it is restarted (`vario service restart`), and the plist only references the configuration file.

## Background service

```bash
vario service install     # per-user LaunchAgent, starts now and at every login
vario service status
vario service uninstall   # stops gracefully and removes the agent; data and config are kept
```

On macOS, Vario installs a per-user LaunchAgent (`~/Library/LaunchAgents/io.github.nedanwr.vario.plist`) that runs `vario run` with your configuration, starts at login, restarts after abnormal exits (throttled to once per 30 s), and runs at background priority. Logs go to `~/Library/Logs/Vario/vario.log` (mode `0600`, rotated at 10 MiB, one previous file kept); `vario.stdout.log` and `vario.stderr.log` in the same directory receive only startup failures and are not rotated. No privileged daemon is installed and nothing is installed unless you run `vario service install`.

The agent runs `vario run --config <path>` with the configuration file resolved at install time (normally `~/.vario/config.toml`); the plist contains no credentials, and the service reads the password file like the CLI. **Configuration changes do not require reinstalling**: after `vario config set`, `vario config edit` or rerunning `vario init`, Vario reminds you that the running service still uses the previous settings, and `vario service restart` applies them (the configuration is validated first, so a broken edit does not stop a working collector; the spool and cursors are kept). Vario never restarts the service on its own. Re-run `vario service install` only after moving the executable or switching to a different configuration file. A configuration error makes the service exit with a failure, so launchd retries it every 30 s until it is fixed or uninstalled.

The service uses only the operational credentials and never needs administrative access, so it can restart unattended. When a destination change requires a decision, or the database was replaced, it exits with an explanation in `~/Library/Logs/Vario/vario.log` and launchd retries every 30 s until `vario destination switch` resolves it (see [Destinations](#destinations-and-queued-data) for the stop/switch/start sequence).

The service does not inherit your shell's environment. `vario init` therefore writes `collector.data_dir` as an absolute path, so the CLI and the service share the same spool, cursors and lease even when `$VARIO_HOME` is set. `vario service install` refuses a configuration whose empty `data_dir` would resolve to a different directory for the service, and `vario doctor` reports it. If the password comes only from `$VARIO_CLICKHOUSE_PASSWORD`, install warns that the service cannot see it; store it with `vario config set-password`.

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
- Local files: the configuration, password file, spool database (including its `-wal`/`-shm` files) and log files are created with mode `0600`, and directories Vario creates with `0700`. Passwords are sent only in the `X-ClickHouse-Key` header; HTTP redirects are never followed, so they cannot be forwarded to another host. Administrative passwords are kept only in memory for the command that asked for them. A user created by Vario is defined with a salted SHA-256 hash computed locally; the password itself is never part of a statement, and hashes are removed from error messages.
- **No telemetry.** Vario sends data only to the ClickHouse endpoint you configure.
- Source databases are opened read-only and are never modified.

Changing the collection or redaction settings applies to records collected afterwards. Existing records are re-sent under the new policy at the next periodic reconciliation (`reconcile_interval`) or on `vario backfill --source t3code --force`; this replaces the latest version of each row, but older versions remain in the base tables until ClickHouse merges them. Lowering the mode therefore does not immediately remove content already stored in ClickHouse; to purge it, delete it in ClickHouse yourself (for example `OPTIMIZE TABLE vario.messages FINAL` after re-sending, or dropping the affected tables and re-sending with `--force`).

## Troubleshooting

- **"administrative login ... failed (authentication)"**: wrong administrative user or password. Init offers to retry, enter other credentials, or continue with manually managed setup.
- **Databases or users cannot be listed**: the administrative account lacks `SHOW DATABASES`/`SHOW USERS` (common on managed services). Init asks for the name instead.
- **"privileges could not be fully determined"**: partial revokes, non-default roles, column-level grants or no access to `system.grants`. Continue: the operational login check is authoritative.
- **"sessions are read-only (readonly = 1)"**: the operational user has a read-only settings profile; assign a profile with `readonly = 0`.
- **"insert into ... rejected"**: the user lacks `INSERT` on a table (possibly through a revoke; rerun `vario init` or grant it), or a session restriction applies (settings constraint, exhausted quota; see the error code).
- **"privileges could not be listed: ... does not support CHECK GRANT"** (ClickHouse 24.8): informational; the insert probe still verifies inserts.
- **"! not verified: ..."** in the login check: that check could not be performed. The other results still apply; check again once the cause (for example a missing schema) is fixed.
- **"An earlier `vario init` created ClickHouse user ..."**: setup was interrupted after creating a user. Run `vario init` and choose "Finish the setup with this user"; see [If setup is interrupted](#if-setup-is-interrupted).
- **`vario destination switch`: "a collector is running" / "the background service is running the collector"**: stop it as shown in the message, switch, and start it again.
- **`vario service install`: "the configuration leaves collector.data_dir empty"**: run the `vario config set collector.data_dir ...` command it prints, then install again.
- **"migration required"** in the log or `vario status`: run `vario migrate`. The data stays spooled.
- **"the ClickHouse destination changed"**: see [Destinations](#destinations-and-queued-data).
- **"not the one Vario delivered to before"**: the database was recreated (new destination id) or the URL now leads to another database; run `vario destination switch`.
- **"the local state was created by an earlier Vario version whose collector is still running"**: `vario service restart`.

## Current limitations

- T3 Code is the only supported source.
- Service installation is implemented for macOS only.
- Credentials are stored in a `0600` file, not in the OS keychain.
- Clusters: no `ON CLUSTER` provisioning and no replicated schema management; untested (see [Managed services, external authentication and clusters](#managed-services-external-authentication-and-clusters)).
- Users created by Vario use `sha256_hash ... SALT` password authentication, verified with ClickHouse 24.8 and 25.8; other versions are untested. LDAP, Kerberos, certificate, SSH-key and other external identities are not provisioned; existing ones can be selected, but Vario authenticates only with a user name and password.
- Grant inspection is advisory. The operational login check is stronger evidence but writes no rows, so quotas on written data, disk space and server-side insert limits are confirmed only by the first delivery. On ClickHouse 24.8 the privilege list (`CHECK GRANT`) is not available, and the warning about administrative privileges held by the operational user is not shown by `vario doctor` or the collector (guided setup still reports it from `system.grants`).
- A generated password is kept in `pending-credentials.json` until the configuration is saved; if that file is deleted before then, only an administrator can reset the password.
- The destination id needs schema version 2 (`vario migrate`). Until then, and while ClickHouse is unreachable, a destination is identified by endpoint and database, so a different host name for the same server counts as a new destination (the full backfill to it is harmless: rows carry the same keys and versions).
- Deletions in the source are reflected only for conversations and projects (`deleted_at` / `is_deleted`); messages or items hard-deleted from T3 Code remain archived in ClickHouse.
- Events T3 Code prunes from its event log before Vario reads them are missing from `raw_events`; the projected state is still collected through reconciliation.
- A replaced T3 Code database is detected by the event id at the cursor. If T3 Code has also pruned that event, detection falls back to the log-length check and the periodic reconciliation.
- Delivery is at-least-once; see [Delivery semantics](#delivery-semantics). Base tables can contain duplicate rows until merges.
- Migrations never alter existing tables: a table missing a column Vario expects is reported as incompatible.
- Configuration writes are serialized between Vario processes with a lock file. Changes made by other programs at the same moment are detected by `vario init`, `vario config set` and `vario config edit` (content fingerprint), but editors that keep the file open may still overwrite Vario's changes when they save.
- Tested only against self-hosted ClickHouse 24.8 and 25.8 (including users defined in `users.xml`). **ClickHouse Cloud and other managed services are untested.**

## Development

```bash
make test             # unit tests (synthetic fixture databases; no private data)
make race             # tests with the race detector
make lint             # gofmt, go vet, staticcheck
make clickhouse-up    # start a local ClickHouse 25.8 in Docker on 127.0.0.1:18123
                      # (CH_IMAGE=clickhouse/clickhouse-server:24.8 for the other tested LTS)
make integration      # ClickHouse integration tests (provisioning, access checks, migrations, destination safety, versioned dedup, aggregation, ambiguous retries)
make clickhouse-down
make dist             # release archives and checksums for all platforms in dist/
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
internal/spool/            local durable queue, cursors, fingerprints, destinations, lease
internal/storage/clickhouse/ HTTP client, schema inspection, migrations, provisioning (admin)
internal/privacy/          collection modes and redaction
internal/service/launchd/  macOS LaunchAgent
internal/model/            normalized row types
```

The module path is `github.com/nedanwr/vario`, derived from the `origin` remote.

## Continuous integration

The **Build** workflow (`.github/workflows/build.yml`) runs on pull requests targeting `master` (when opened, updated with new commits, or reopened) and on every push to `master`, including merges. It does not run for pushes to other branches. On one Ubuntu runner, it:

1. verifies module checksums (`go mod verify`) and `gofmt` formatting,
2. runs `go vet ./...` for Linux, macOS and Windows,
3. runs the unit tests (`make test`),
4. builds and packages every release target with `make dist` and checks the result.

The workflow has read-only repository access and uses no secrets. The ClickHouse integration tests are not part of it; run them locally with `make integration`.

The build for macOS and Windows is **cross-compiled** on Linux (Vario is pure Go with `CGO_ENABLED=0`, including its SQLite driver). This proves the code compiles and links for each platform, but the tests run only on Linux, and the macOS and Windows binaries are not executed in CI.

## Releases

Releases are published by the **Release** workflow (`.github/workflows/release.yml`) only when a maintainer pushes a version tag. Merging into `master` never publishes anything, and the workflow never creates tags.

| OS | Architectures | Archive |
| --- | --- | --- |
| macOS | amd64, arm64 | `vario_<version>_darwin_<arch>.tar.gz` |
| Linux | amd64, arm64 | `vario_<version>_linux_<arch>.tar.gz` |
| Windows | amd64, arm64 | `vario_<version>_windows_<arch>.zip` |

Each archive contains a single directory with the executable, `LICENSE`, `README.md`, and `THIRD_PARTY_LICENSES.txt`. Tags follow [Semantic Versioning](https://semver.org) with a `v` prefix: `v0.1.0`, `v1.2.3`, or a prerelease like `v0.2.0-rc.1` (published as a GitHub prerelease). The tag is embedded in the binary, so `vario version` reports it along with the commit.

Before publishing, the workflow checks that the tag is valid SemVer and points to a commit in `master`'s history, then runs vet and tests and builds all archives and `checksums.txt` from a clean module cache. A separate job, the only one with write access, uploads them to a draft release with generated release notes, checks the upload, and then publishes the release. If a release or draft for the tag already exists, the workflow stops without changing it; to publish again, delete the release and re-run the workflow.

To release, merge the changes into `master`, then tag the merged commit and push the tag:

```bash
git switch master && git pull --ff-only
git tag -a v0.1.0 -m "Vario v0.1.0"
git push origin v0.1.0
```

Builds are reproducible: given the same commit and Go version, `make dist VERSION=v0.1.0` produces the same binaries (the embedded build date is the commit time).

**Code signing:** release binaries are **not signed**. The macOS binaries are not notarized, so Gatekeeper may refuse to run a binary downloaded through a browser; after verifying the checksum, remove the quarantine attribute (`xattr -d com.apple.quarantine vario`). Windows SmartScreen may also warn about the unsigned executable.

### Verifying downloads

Download `checksums.txt` from the same release, place it next to the archive, and run:

```bash
sha256sum --check --ignore-missing checksums.txt         # Linux
shasum -a 256 --check --ignore-missing checksums.txt     # macOS
```

On Windows (PowerShell), compare the output of `Get-FileHash vario_<version>_windows_amd64.zip -Algorithm SHA256` with the corresponding line in `checksums.txt`.

## Principles

Vario is open-source and local-first. Your data stays under your control. Vario works without hosted accounts or external telemetry services.

## License

Vario is licensed under the [Apache License 2.0](LICENSE).
