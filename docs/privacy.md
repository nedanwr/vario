# Privacy and security

Read this before enabling `messages` or `full` collection. Credential storage is described under [Credentials](configuration.md#configuration-file) and [Access model](clickhouse.md#access-model-administrative-and-operational-credentials).

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
