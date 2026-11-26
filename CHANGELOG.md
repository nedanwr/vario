# Changelog

Notable changes to Vario are listed here. Versions follow [Semantic Versioning](https://semver.org); before 1.0, and especially between beta releases, minor versions and prereleases may contain incompatible changes, which are always called out in this file.

## [Unreleased]

## [0.1.0-beta.3]

Adds Claude Code as a second source. No schema migration or configuration change is needed: an existing installation keeps collecting T3 Code as before, and Claude Code is collected only once you enable it (`vario sources add claudecode`; `vario doctor` mentions it when Claude Code is installed).

### Added

- **Claude Code source** (`claudecode`): collects Claude Code's local session transcripts (`~/.claude/projects`, or `$CLAUDE_CONFIG_DIR`), including subagent transcripts, into the existing tables: conversations, runs, messages, tool calls, reasoning, API errors, compactions, raw events and token usage per API response (cache reads and writes tracked separately, each response counted once). Enable it with `vario init` (offered when detected) or `vario sources add claudecode`, import history with `vario backfill --source claudecode`. No schema migration is needed. See [docs/claude-code.md](docs/claude-code.md).
- **No double counting with T3 Code.** T3 Code runs Claude through the Agent SDK, so its Claude threads are also Claude Code transcripts. Those threads are collected only from T3 Code. The Claude Code source skips them, and keeps just the token usage of subagents Claude starts inside them, which T3 Code does not record. They are recognized by T3 Code's MCP tools together with T3 Code's entrypoint (`t3-code`, or `sdk-ts` from earlier builds). Terminal sessions, other Agent SDK applications and `claude` sessions that Claude starts from a shell command inside T3 Code (which inherit its entrypoint) are collected in full. This attribution does not depend on whether T3 Code is enabled, and `vario doctor` warns when it is not. See [Sessions run by T3 Code](docs/claude-code.md#sessions-run-by-t3-code).
- `vario sources` without a subcommand shows every supported source, whether it is installed and how it is configured. `vario doctor` checks Claude Code transcripts for unreadable files and malformed lines and mentions detected but unconfigured sources.
- `vario init` flags `--claudecode-path` and `--no-claudecode`.

### Privacy

- The collection modes apply to Claude Code as to T3 Code. In the default `metrics` mode no prompt, reply, reasoning, tool input or output, title, branch, path or file name is collected from transcripts. The `session-owner` line (account and organization ids) is never collected, in any mode.
- As for T3 Code, every mode exports the machine's host name and the transcript directory (`sources` table).

### Known limitations

- Claude Code deletes transcripts older than `cleanupPeriodDays` (30 days by default) when it starts. Older history cannot be backfilled, and work is kept only if Vario collected it before then.
- After `vario backfill`, the first `vario run` re-reads and re-sends every source in full once. The rows are the same, so the views and totals are unaffected. Earlier betas behave the same way.

The full list is in [docs/claude-code.md](https://github.com/nedanwr/vario/blob/master/docs/claude-code.md#known-limitations).

## [0.1.0-beta.2]

Fixes the Windows installer on PowerShell 7. Vario itself is unchanged from 0.1.0-beta.1; no migration or configuration change is needed.

### Fixed

- `install.ps1` failed on PowerShell 7 with "the downloaded binary does not run on this system or reports ''". Windows PowerShell 5.1 was not affected. The `install.ps1` attached to the 0.1.0-beta.1 release still has the bug; use the installer from `master` or this release.

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

[0.1.0-beta.3]: https://github.com/nedanwr/vario/releases/tag/v0.1.0-beta.3
[0.1.0-beta.2]: https://github.com/nedanwr/vario/releases/tag/v0.1.0-beta.2
[0.1.0-beta.1]: https://github.com/nedanwr/vario/releases/tag/v0.1.0-beta.1
