# Changelog

Notable changes to Vario are listed here. Versions follow [Semantic Versioning](https://semver.org); before 1.0, and especially between beta releases, minor versions and prereleases may contain incompatible changes, which are always called out in this file.

## [0.1.0-beta.1]

First public beta. Vario is usable for day-to-day collection, but it has not yet been run widely outside its development environment. Expect rough edges and report problems at <https://github.com/nedanwr/vario/issues>.

### Capabilities

- **T3 Code collector** (the only supported source): reads T3 Code's local database (`~/.t3/userdata/statev2.sqlite`) read-only, imports existing history (`vario backfill`), then syncs continuously and reconciles periodically (`vario run`).
- **Collected data**: conversations, runs and messages; execution events (commands, tool calls, file changes, web searches, reasoning, errors, checkpoints); token usage per provider turn with cached, cache-creation, uncached, output and reasoning tokens tracked separately; raw source events as an audit log. What is collected depends on the collection mode (see Privacy).
- **ClickHouse delivery** over the HTTP(S) interface into a database you choose, with ready-made views (`*_latest`, `token_usage_daily`, `tool_usage`) that count each record once.
- **Durable local spool**: collection survives network outages, ClickHouse downtime and reboots. Delivery is at-least-once.
- **Setup and administration**: `vario init` (guided setup with administrative credentials that are never stored, or a manually managed setup), `vario migrate` (additive, previewable schema migrations), `vario doctor`, `vario status`, `vario config`, `vario sources`.
- **Destination safety**: a ClickHouse database that was replaced or restored from a backup, or a changed URL or database name, stops delivery until you decide with `vario destination switch`; queued rows are never sent to the wrong database.
- **Background service on macOS**: `vario service install` sets up a per-user LaunchAgent.
- **Release archives** for macOS, Linux and Windows (amd64 and arm64) with SHA-256 checksums and reproducible builds. `vario version` reports the version, commit and build date.
- **Installers**: `curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh` (macOS, Linux) and `irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex` (Windows) install the newest beta, or a given `--version`, from GitHub Releases. They verify checksums, install per user without administrator rights, and never touch the configuration or the background service. Each GitHub Release also carries the archives, `checksums.txt` and both installers.

### Support

- **Sources**: T3 Code only.
- **ClickHouse**: tested with self-hosted **24.8 LTS** and **25.8 LTS**. ClickHouse Cloud, other managed services and clusters are untested.
- **Platforms**: the background service is **macOS only**. On Linux and Windows, run `vario run` under your own supervisor (systemd, Task Scheduler, ...). The test suite runs on Linux in CI; macOS and Windows binaries are cross-compiled there and not executed. Binaries are not signed, and the macOS binaries are not notarized.

### Privacy

- The default `metrics` mode collects token usage, models, timings, tool names, statuses, exit codes and sizes, but no message text, titles, paths or tool inputs/outputs.
- `messages` and `full` collect conversation content, tool inputs/outputs, reasoning and diffs. They must be enabled explicitly (`allow_sensitive_content = true`, which `vario init` sets only after you type `yes`).
- Every mode exports the machine's host name and the T3 Code database path.
- Redaction of known credential formats is applied before anything is written, but it cannot detect every secret or any personal data. Treat `messages` and `full` data in ClickHouse as sensitive.
- Lowering the collection mode does not erase content already stored in ClickHouse, its earlier row versions or backups.
- No telemetry: Vario sends data only to the ClickHouse endpoint you configure.

### Beta compatibility

- The configuration file, command-line flags and output, and the ClickHouse schema may still change in later betas. Schema changes are shipped as additive migrations applied with `vario migrate`; Vario never drops or alters existing tables. Any change that needs manual action will be described in this file.
- Downgrading is not supported. An older Vario refuses to migrate a schema migrated by a newer one, and local state (`~/.vario/data/state.db`) converted by a newer version is not guaranteed to work with an older one.
- The output of `vario status` and `vario doctor` is meant for people, not scripts. Exit codes are documented in [docs/configuration.md](https://github.com/nedanwr/vario/blob/master/docs/configuration.md).
- Installations built from source before this beta are upgraded in place; see [Upgrading from a development build](https://github.com/nedanwr/vario/blob/master/docs/operations.md#upgrading-from-a-development-build).

### Known limitations

- Delivery is at-least-once: base tables can hold duplicate rows until ClickHouse merges them. Query the views, or use `FINAL` or `argMax(..., version)`.
- Messages or items hard-deleted in T3 Code remain in ClickHouse. Events that T3 Code prunes before Vario reads them are missing from `raw_events`.
- Changing the privacy settings re-sends every record and sanitizes only the latest version of each; at most 1023 such changes are possible per database.
- The operational ClickHouse password is stored in a `0600` file (or `$VARIO_CLICKHOUSE_PASSWORD`), not in the OS keychain.
- On ClickHouse 24.8, `CHECK GRANT` is unavailable, so the login check cannot list privileges (inserts are still verified).
- Vario authenticates to ClickHouse only with a user name and password; it does not provision LDAP, Kerberos, certificate or SSH-key users.

The full list is in [docs/operations.md](https://github.com/nedanwr/vario/blob/master/docs/operations.md#current-limitations).

[0.1.0-beta.1]: https://github.com/nedanwr/vario/releases/tag/v0.1.0-beta.1
