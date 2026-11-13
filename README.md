# Vario

**Observe everything AI.**

Vario is a lightweight, cross-platform command-line tool for AI observability. It reads the local data of AI applications on your machine (read-only), normalizes it, and delivers it to a ClickHouse database you control.

> **Status: public beta (`v0.1.0-beta.1`).** Vario collects from T3 Code into self-hosted ClickHouse. The configuration, command-line interface and ClickHouse schema may still change between betas; schema changes ship as additive migrations (`vario migrate`), and anything that needs manual action is listed in the [changelog](CHANGELOG.md). Downgrades are not supported.

## What it collects

- **Conversations**, runs, and messages
- **Execution events**: commands, tool calls, file changes, web searches, reasoning, errors, checkpoints
- **Token usage** per provider turn, with cached, cache-creation, uncached, output, and reasoning tokens tracked separately
- **Raw source events** as an audit log

Vario runs as a single process, in the foreground or as a per-user background service. Data is buffered in a local spool, so collection survives network outages, ClickHouse downtime, and reboots. Delivery is at-least-once; the provided views count each record once.

## Support

|              | Supported in this beta                                                                                                                         |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Source       | **T3 Code** (`~/.t3/userdata/statev2.sqlite`): backfill, continuous sync, token usage. No other source is supported.                            |
| ClickHouse   | Self-hosted **24.8 LTS** and **25.8 LTS**, over HTTP(S). ClickHouse Cloud, other managed services and clusters are untested.                    |
| Platforms    | macOS, Linux, Windows (amd64, arm64). The background service (`vario service`) is **macOS only**; elsewhere, run `vario run` under your own supervisor. |

## Install

Download the archive for your platform (macOS, Linux, Windows; amd64 and arm64) from [GitHub Releases](https://github.com/nedanwr/vario/releases) (beta builds are marked as pre-releases), [verify its checksum](docs/getting-started.md#verifying-downloads), and put `vario` on your `PATH`. Binaries are not signed; see [Code signing](docs/getting-started.md#code-signing). `vario version` prints the version, commit and build date.

Or build from source with Go 1.25 or newer (no CGO, no runtime dependencies):

```bash
git clone https://github.com/nedanwr/vario.git
cd vario
git checkout v0.1.0-beta.1   # optional: a released version instead of the latest master
make build                   # writes bin/vario with version information
```

## Quick start

```bash
vario init                      # guided setup: ClickHouse access, database, user, collection mode, sources
vario migrate                   # create Vario's schema, if init did not (administrative credentials)
vario doctor                    # check configuration, source access, ClickHouse
vario backfill --source t3code  # import existing history
vario run                       # collect continuously in the foreground (Ctrl-C to stop)
vario service install           # or: run in the background at login (macOS)
vario status                    # cursor, spool, delivery and service state
```

See [Getting started](docs/getting-started.md) for the setup paths, non-interactive setup, and what happens if setup is interrupted.

Example query, once data has arrived:

```sql
SELECT toStartOfMonth(day) AS month, provider, model,
       sum(input_tokens) AS input, sum(output_tokens) AS output
FROM vario.token_usage_daily GROUP BY ALL ORDER BY month DESC, input DESC;
```

## ClickHouse

- Vario talks to ClickHouse over its **HTTP(S) interface** (ports 8123/8443). Plain `http://` is accepted only for localhost unless explicitly allowed.
- Tested with self-hosted ClickHouse **24.8 LTS** and **25.8 LTS**. ClickHouse Cloud, other managed services, and clusters are untested.
- Vario writes only to its own database (default `vario`) and never touches other databases on the server.
- Two kinds of credentials:
  - **Administrative** credentials are used only by `vario init` and `vario migrate` to create the database, a dedicated user, grants, and the schema. They are **never stored**.
  - The **operational** user is used by the collector. It needs only `SELECT, INSERT` on Vario's own tables and `SELECT` on its views: no DDL, no global privileges. Its password is stored in `~/.vario/clickhouse-password` (mode `0600`) or read from `$VARIO_CLICKHOUSE_PASSWORD`.
- The collector never changes the schema. When a new version needs a newer schema, data stays spooled until an administrator runs `vario migrate` (preview with `--dry-run`). Migrations are additive: tables are never dropped or altered.

Details: [ClickHouse requirements, permissions, schema and migrations](docs/clickhouse.md).

## Collection modes and privacy

Transcripts and command output can contain credentials, private files, and proprietary source code. Vario collects as little as possible by default:

| Mode                | Collects                                                                                         |
| ------------------- | ------------------------------------------------------------------------------------------------ |
| `metrics` (default) | Token usage, models, timings, tool names, statuses, exit codes, sizes. No message text or paths. |
| `messages`          | Adds message text, conversation and project titles, workspace paths, changed file names.         |
| `full`              | Adds tool inputs/outputs, reasoning, file diffs, raw event payloads.                             |

- `messages` and `full` require `privacy.allow_sensitive_content = true`, which `vario init` sets only after you type `yes`.
- Redaction of known credential formats runs before anything is written locally, but it **cannot detect every secret or personal data**. Treat `messages` and `full` data in ClickHouse as sensitive.
- Lowering the mode later does **not** remove content already stored in ClickHouse.
- Source databases are opened read-only. **No telemetry**: Vario sends data only to the ClickHouse endpoint you configure.

Read [Privacy and security](docs/privacy.md) before enabling content collection.

## Data integrity

Vario detects when its ClickHouse database is **replaced** (dropped and recreated) or **restored to an earlier state** from a backup. In either case it stops delivering, keeps all queued rows locally, and waits for an explicit decision with `vario destination switch`. Changing `clickhouse.url` or `clickhouse.database` is handled the same way when rows are still queued. See [Destinations and queued data](docs/operations.md#destinations-and-queued-data) and [Recovery](docs/operations.md#recovery).

## Documentation

| Document                                          | Contents                                                                                     |
| ------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| [Getting started](docs/getting-started.md)        | Installation, checksums, `vario init`, interrupted setup, first collection                   |
| [Configuration](docs/configuration.md)            | All commands, exit codes, `~/.vario/` layout, `vario config`, credentials                    |
| [Privacy and security](docs/privacy.md)           | Collection modes, redaction and its limits, local file permissions, purging data             |
| [ClickHouse](docs/clickhouse.md)                  | Requirements, permissions, access checks, schema, migrations, example queries                |
| [Operations](docs/operations.md)                  | Sync, destinations, rollback detection, delivery, recovery, upgrades, service, troubleshooting, limitations |
| [Architecture](docs/architecture.md)              | Design decisions and internals                                                               |
| [Development](docs/development.md)                | Tests, project layout, CI, releases                                                          |
| [Changelog](CHANGELOG.md)                         | Changes, beta compatibility and known limitations per release                               |

Every option is documented in [`config.example.toml`](config.example.toml).

## Development

```bash
make test             # unit tests
make lint             # gofmt, go vet, staticcheck
make clickhouse-up    # local ClickHouse in Docker
make integration      # ClickHouse integration tests
make clickhouse-down
```

See [Development](docs/development.md) for integration test setup, the project layout, CI, and the release process. Contributions are welcome through issues and pull requests against `master`.

## Principles

Vario is open-source and local-first. Your data stays under your control. Vario works without hosted accounts or external telemetry services.

## License

Vario is licensed under the [Apache License 2.0](LICENSE).
