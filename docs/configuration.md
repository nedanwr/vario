# Configuration and commands

Collection modes and redaction settings are described in [privacy.md](privacy.md); ClickHouse access in [clickhouse.md](clickhouse.md).

## Commands

| Command                                                            | Description                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `vario init`                                                       | Set up or update ClickHouse access and the configuration (see [Getting started](getting-started.md#vario-init)): guided (administrative credentials, not saved) or manually managed. `--non-interactive` with `--update` or `--force` for existing configurations. Never installs the service.                                                                                                                                |
| `vario migrate [--dry-run]`                                        | Create or update Vario's schema after showing the plan and asking for confirmation; asks for administrative credentials only when something has to change (see [Schema migrations](clickhouse.md#schema-migrations)).                                                                                                                                                                                          |
| `vario destination` / `destination switch` / `destination pending` | Show which destination queued data belongs to; switch to the configured destination after a change, keeping, moving or discarding queued rows, or accept a database that was restored to an earlier state; show, retry, move or discard rows kept for a previous destination (see [Destinations](operations.md#destinations-and-queued-data)).                                                                 |
| `vario config path`                                                | Print the configuration file in use.                                                                                                                                                                                                                                                                                                                                                              |
| `vario config show`                                                | Show every setting with its effective value; defaults are marked, the password is never shown.                                                                                                                                                                                                                                                                                                    |
| `vario config get <key>` / `set <key> <value>`                     | Read or change one setting, validated and written in place.                                                                                                                                                                                                                                                                                                                                       |
| `vario config set-password [--stdin \| --clear]`                   | Store (hidden prompt) or remove the operational ClickHouse password.                                                                                                                                                                                                                                                                                                                              |
| `vario config edit`                                                | Edit the file in `$VISUAL`/`$EDITOR`; invalid edits are not applied.                                                                                                                                                                                                                                                                                                                              |
| `vario sources detect`                                             | Find supported sources in default and configured locations. Read-only.                                                                                                                                                                                                                                                                                                                            |
| `vario sources list`                                               | Show configured sources with their cursor and last sync time.                                                                                                                                                                                                                                                                                                                                     |
| `vario sources add t3code [--path P]`                              | Validate and enable a source.                                                                                                                                                                                                                                                                                                                                                                     |
| `vario backfill --source t3code`                                   | Import current state and the event history, then deliver. `--force` re-sends everything; `--no-deliver` only fills the spool.                                                                                                                                                                                                                                                                     |
| `vario run [--log-file F]`                                         | Run the collector in the foreground. Sources without a cursor are backfilled automatically.                                                                                                                                                                                                                                                                                                       |
| `vario status`                                                     | Configuration, service, collector, per-source sync and delivery status. Does not contact ClickHouse.                                                                                                                                                                                                                                                                                              |
| `vario doctor`                                                     | Validate configuration and permissions, source access, spool and destinations, and the operational ClickHouse login: reachability, authentication, database, schema version and compatibility, read-only settings, privileges, an insert probe, and whether the database still holds the acknowledged delivery checkpoint. Read-only. (`--create-schema` is deprecated and runs `vario migrate`.) |
| `vario service install` / `restart` / `uninstall` / `status`       | Manage the background service (macOS). `restart` applies configuration changes.                                                                                                                                                                                                                                                                                                                   |
| `vario version`                                                    | Print version, commit, and build information.                                                                                                                                                                                                                                                                                                                                                     |

Global flags: `--config <path>` (or `$VARIO_CONFIG`), `-v/--verbose`.

Exit codes: `0` success, `1` failure (including failed `doctor` checks), `2` usage or configuration error, `3` incomplete (for example, backfill data spooled but ClickHouse unreachable).

## Configuration file

Vario keeps its configuration and local state in **`~/.vario/`** (mode `0700`) on every platform:

| Path                           | Contents                                                                                                                                      |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `~/.vario/config.toml`         | Configuration (mode `0600`). No secrets.                                                                                                      |
| `~/.vario/clickhouse-password` | Operational ClickHouse password (mode `0600`), unless `clickhouse.password_file` points elsewhere. Administrative passwords are never stored. |
| `~/.vario/data/`               | Spool, cursors, destinations and lease (`collector.data_dir`).                                                                                |

**Which file is used.** Every command, the collector, `backfill` and the background service resolve the configuration the same way:

1. `--config <path>`
2. `$VARIO_CONFIG`
3. `$VARIO_HOME/config.toml`, where `$VARIO_HOME` replaces `~/.vario` (also for the password file and data directory; useful for isolated installations and testing)
4. `~/.vario/config.toml`

`vario config path` prints the result. See [`config.example.toml`](../config.example.toml) for every option. Unknown keys are rejected so typos do not go unnoticed.

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

Changing `clickhouse.url` or `clickhouse.database` (with `config set`, `config edit`, `init` or by editing the file) changes the destination: see [Destinations](operations.md#destinations-and-queued-data). `clickhouse.create_database` is deprecated and ignored.
