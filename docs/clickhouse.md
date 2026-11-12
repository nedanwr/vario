# ClickHouse

Requirements, provisioning, permissions, schema and migrations. For day-to-day operation (destinations, recovery, troubleshooting) see [operations.md](operations.md).

## Access model: administrative and operational credentials

Vario uses two kinds of ClickHouse credentials:

|            | Administrative                                                                                                                     | Operational                                                                                                                                         |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| Used by    | `vario init` (guided setup), `vario migrate`                                                                                       | `vario run`, `vario backfill`, the background service, `vario doctor`                                                                               |
| Used for   | listing databases and users, creating the database and a dedicated user, granting privileges, creating or migrating Vario's schema | inserting collected data, reading Vario's schema and destination id                                                                                 |
| Stored     | **never**: asked for when needed (hidden prompt or `--admin-password-stdin`), kept in memory for that command only                 | user name in `clickhouse.username`; password in `~/.vario/clickhouse-password` (mode `0600`) or `$VARIO_CLICKHOUSE_PASSWORD`                        |
| Privileges | whatever your administrative account has                                                                                           | `SELECT, INSERT` on Vario's eight tables and `vario_checkpoints`, `SELECT` on its views and `vario_meta`; nothing global, no DDL, no `GRANT OPTION` |

The collector never creates or changes databases, users, privileges, tables or views. If its schema is missing or incompatible it keeps data spooled, reports the problem and points to `vario migrate`.

## Requirements

- A ClickHouse server reachable over its **HTTP(S) interface** (default ports 8123/8443). Vario does not use the native protocol and needs no Docker or administrative access on the client.
- Tested with self-hosted ClickHouse **24.8 LTS** (24.8.14.39) and **25.8 LTS** (25.8.33.6) in Docker: user creation with a locally computed salted hash, generated passwords and login, grants, the operational login check, schema creation and migrations, and destination switching. Other versions are untested. On 24.8, `CHECK GRANT` does not exist, so the privilege list in the login check is not shown; the insert probe still verifies inserts (see [How access is verified](#how-access-is-verified)).
- Plain `http://` is accepted only for `localhost`/loopback unless `allow_insecure_http = true`. Use `https://` for anything remote; custom CAs are supported via `ca_file`.

## Managed services, external authentication and clusters

- **ClickHouse Cloud and other managed services have not been validated.** User provisioning, privilege management and schema operations may differ there: discovery may be restricted (init then asks for names), and user-management rules (password policies, built-in roles, restricted `CREATE USER`) may reject Vario's statements; failures are reported step by step. A manually provisioned operational account (manually managed setup) is the expected path where guided setup does not work, provided the service exposes the HTTP(S) interface and accepts password authentication with the `X-ClickHouse-User`/`X-ClickHouse-Key` headers.
- **External authentication**: Vario does not provision LDAP, Kerberos, certificate-authenticated, SSH-key or other externally managed users. Guided setup can _select_ an existing account of that kind as the operational user (it is labelled and never offered grants), but Vario can only _authenticate_ with a user name and password sent over HTTP(S). An LDAP user whose password ClickHouse checks against the directory may work that way (untested); Kerberos, certificate and SSH-key logins do not. The login check shows whether authentication works.
- **Clusters**: Vario does not use `ON CLUSTER` and does not manage replicated schemas. Its tables are created as non-replicated `ReplacingMergeTree` on the server it connects to, and users and grants exist only there unless your access storage is replicated. For a cluster, create the database, replicated tables and grants yourself and let `vario migrate` add only what is missing. Cluster deployments are untested.

## Database selection and shared servers

Vario writes only to the database named by `clickhouse.database` (default `vario`). The server can host any number of other databases (Grafana, application telemetry, logs, ...): Vario never reads or changes them, never assumes it owns the server, and never uses the `default` database unless you choose it. A dedicated database is recommended. In a shared database, the operational user's grants are limited to Vario's own tables and views, so it cannot read or change unrelated tables.

The database is created only by guided `vario init` or `vario migrate`, after you confirm, with administrative credentials. An existing database is reused, never dropped or recreated. The collector never creates databases.

## Schema migrations

`vario migrate` is the only command that changes Vario's schema:

1. It reads the configured endpoint and database and inspects the schema, first with the operational user (no administrative credentials are needed when nothing has to change).
2. It shows the planned operations, grouped by kind; `--dry-run` also prints their SQL and stops there. Add `--admin-user` to make the dry run inspect with administrative credentials, which may see more.
3. It asks for administrative credentials, re-plans with them and asks for confirmation. Creating a missing database, changing the schema, and granting the operational user access to new objects are confirmed separately; `--non-interactive` requires `--yes`.
4. It applies the steps in order, verifies the result, and checks access as the operational user. A failure is reported with the steps that were and were not applied; running `vario migrate` again resumes.

Migrations are versioned (current schema version: **3**; version 1 is the schema of Vario 0.1, which recorded no version), idempotent and additive: missing tables, views and metadata are created and Vario's own outdated views are replaced. Databases and tables are never dropped, truncated or altered. Objects with Vario's names that Vario did not create, or tables with a different engine, sorting key (`ORDER BY`), partition key (`PARTITION BY`), `ReplacingMergeTree` version column or column types, are reported as an **incompatible schema** and stop the migration (extra columns are allowed). The sorting key and the version column decide which rows count as the same record, and rows are only ever merged within one partition, so a table that differs in any of them would silently merge records or keep superseded versions; Vario refuses to deliver into it and never alters it. A schema migrated by a newer Vario is left alone. Version 2 adds `vario_meta`, which records the schema version and a random **destination id** (see [Destinations](operations.md#destinations-and-queued-data)). Version 3 adds `vario_checkpoints`, the **delivery checkpoints** by which a database that was restored to an earlier state is recognized (see [A database restored to an earlier state](operations.md#a-database-restored-to-an-earlier-state)); migrating from version 2 creates that one table, keeps the destination id and changes nothing else.

Administrative credentials are requested only for these authorized changes; routine collection (`vario run`, `vario backfill`, the background service) uses the restricted operational account and never needs them. When a new Vario version needs a newer schema, the collector keeps collecting into the spool and reports "migration required" until an administrator runs `vario migrate` (preview with `--dry-run`). The migration path from version 1 to 2 is tested against ClickHouse 24.8 and 25.8.

Views carry a version tag in their comment. A view is replaced only if it is Vario's own: it carries that tag, or it has no comment and its query is exactly one that Vario 0.1 created. Any other view with one of Vario's view names is yours: it is kept as it is and reported as a conflict (rename or drop it, or use another database, then run `vario migrate` again). Updating an outdated view needs `DROP TABLE` (for `CREATE OR REPLACE`) or `DROP VIEW`; without either, the old view is kept with a warning.

## ClickHouse permissions

The **operational user** needs, on the target database only:

| Privilege        | Objects                                                                                                                   | Needed for                                                      |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| `SELECT, INSERT` | the tables `sources`, `projects`, `conversations`, `runs`, `messages`, `execution_events`, `provider_turns`, `raw_events` | inserting collected data; seeing the tables in the schema check; reading the highest stored version (policy generation check) |
| `SELECT`         | the `*_latest`, `token_usage_daily` and `tool_usage` views, and `vario_meta`                                              | the schema check, the destination id, and querying Vario's data |
| `SELECT, INSERT` | the table `vario_checkpoints`                                                                                             | reading and writing the collector's delivery checkpoint         |

It needs no global privileges, no `CREATE`/`DROP`/`ALTER`, no access management and no `GRANT OPTION`. Its sessions must not be read-only (`readonly = 0`). The guided setup and `vario migrate` grant exactly this, one `GRANT` per object. To provision by hand:

```sql
CREATE DATABASE IF NOT EXISTS vario;
CREATE USER vario IDENTIFIED WITH sha256_password BY '...' DEFAULT DATABASE vario;
GRANT SELECT, INSERT ON vario.messages TO vario;   -- and the other seven tables
GRANT SELECT ON vario.messages_latest TO vario;    -- and the other views, and vario.vario_meta
GRANT SELECT, INSERT ON vario.vario_checkpoints TO vario;
-- then, as an administrator: vario migrate
```

(`GRANT SELECT, INSERT ON vario.* TO vario` also works for a dedicated database.) `vario init` prints the complete list for your database and user.

The **administrative account** needs what the chosen operations need: `SHOW USERS`/`SHOW DATABASES` (or the ability to read `system.users`, `system.databases`, `system.grants`, `system.role_grants`) for discovery; `CREATE DATABASE`; `CREATE USER` and the privileges it grants (with `GRANT OPTION`, or access management); `CREATE TABLE`, `CREATE VIEW`, `INSERT` (for `vario_meta`) and optionally `DROP VIEW` on the database for migrations. If discovery is not permitted, init falls back to manual entry.

`vario doctor` warns if the operational user holds administrative privileges (`CREATE USER`, `CREATE DATABASE`, `DROP DATABASE` on `*.*`); the collector logs the same warning at startup.

## How access is verified

Grant inspection during guided setup reads `system.grants` and `system.role_grants` as the administrator. It is **advisory**: settings profiles, settings constraints, quotas and host restrictions are not visible there, and partial revokes or non-default roles make it inconclusive.

The stronger evidence is the **operational login check** (`vario init`, `vario migrate`, `vario doctor`), which runs as the actual operational user and password from this computer:

| Check             | How                                                                                                            | Confirms                                                                                                                                                    |
| ----------------- | -------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| authentication    | a query as the operational user                                                                                | the credentials and the host restrictions for this computer                                                                                                 |
| schema access     | reads Vario's tables, columns and `vario_meta`                                                                 | everything the collector reads at runtime                                                                                                                   |
| read-only session | `getSetting('readonly')`, evaluated by the server                                                              | the settings profile allows writes                                                                                                                          |
| privileges        | `CHECK GRANT` for `INSERT` on each table and `SELECT` on `vario_meta` (ClickHouse 25.8; not available on 24.8) | explains a failure; not proof on its own                                                                                                                    |
| inserts           | an `INSERT` into every Vario table **with exactly the settings of a delivery** and no rows                     | privileges, revokes, roles, `readonly`, settings constraints on those settings, and already exhausted quotas, as ClickHouse applies them to the real insert |
| checkpoint        | the same two checks for `vario_checkpoints`; `vario doctor` also reads the checkpoint the database holds       | whether the collector can keep its delivery checkpoint, and whether the database still holds the one it acknowledged                                        |

Each result is reported as confirmed (✓), failed (✗) or **not verified** (!), for example when the read-only setting cannot be read or the schema does not exist yet, so inserts cannot be tried. The check needs no DDL or administrative privileges and writes nothing. Because it writes no rows, quotas on written bytes, disk space, memory limits and server-side insert limits are confirmed only by the first delivery (`vario status` shows it). Vario does not write test rows into the telemetry tables, and it does not create a separate probe table, which would need extra provisioning.

## Connection check results

| Reported as         | Meaning and fix                                                                                                                                                                                                    |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| network             | Server not reachable: check that it is running, the host and port, firewalls and proxies.                                                                                                                          |
| tls                 | TLS failed: use `https://` with a TLS port; set `clickhouse.ca_file` for a private CA.                                                                                                                             |
| authentication      | Wrong username or password: `vario config set clickhouse.username ...`, `vario config set-password`. For administrative logins, re-enter the credentials.                                                          |
| database missing    | The database does not exist or the user has no access to it: create it with `vario init` (guided) or `vario migrate`, or grant access.                                                                             |
| permission          | A privilege is missing (possibly through a revoke): rerun `vario init` to inspect and fix grants, or see the table above.                                                                                          |
| session restriction | The user's settings profile makes sessions read-only, a settings constraint forbids a setting Vario's inserts use, or a quota is exhausted. An administrator has to change the profile or quota; Vario never does. |
| migration required  | Vario's schema is missing or incomplete: run `vario migrate`.                                                                                                                                                      |
| schema              | Objects with Vario's names that Vario cannot use: choose a dedicated database or remove them.                                                                                                                      |

None of these prevent saving the configuration. Without a reachable or ready ClickHouse, `vario run` and `vario backfill` keep collecting into the local spool and deliver once the problem is fixed.

## Schema

| Table              | Contents                                                                                                                | Engine / key                                        |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- |
| `sources`          | One row per collected installation: host name, source database path, collection mode                                    | `ReplacingMergeTree(version)` by `source_id`        |
| `projects`         | Projects/workspaces; title and workspace path in messages+ mode                                                         | by `(source_id, project_id)`                        |
| `conversations`    | Threads: provider, model, mode, lineage, timestamps, title (messages+ mode)                                             | by `(source_id, conversation_id)`                   |
| `runs`             | User requests and their model selection                                                                                 | by `(source_id, conversation_id, run_id)`           |
| `messages`         | User/assistant messages; text in messages+ mode, token bloom-filter index                                               | by `(source_id, conversation_id, message_id)`       |
| `execution_events` | Turn items: commands, tools, file changes, reasoning, errors; status, exit code, duration, sizes; payloads in full mode | by `(source_id, conversation_id, event_id)`         |
| `provider_turns`   | Token usage and context snapshot per provider turn                                                                      | by `(source_id, conversation_id, provider_turn_id)` |
| `raw_events`       | Source event log: type, ids, time, known metadata keys; payloads in full mode                                           | by `(source_id, sequence, event_id)`                |

Two more tables hold no collected data: `vario_meta` (schema version and destination id, written only by `vario migrate`) and `vario_checkpoints` (one logical row per collector: the highest delivery checkpoint it wrote, `ReplacingMergeTree(seq)` by `writer_id`).

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

Token columns follow the source's reporting: `input_tokens` already includes cached and cache-creation tokens, and reasoning tokens are not added to `output_tokens`. `context_*` columns are context-window snapshots and must not be summed. Missing usage is `NULL`, not zero. Usage the source marks `partial` is stored as reported (a lower bound) and counted in `turns_incomplete_usage`. `token_usage_daily` keeps `usage_scope` as a grouping column; T3 Code currently reports only `main_agent`, and other scopes should not be added together until their relationship is known. See [docs/architecture.md](architecture.md) for details.
