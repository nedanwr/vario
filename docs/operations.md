# Operations

How the collector runs, how it handles destinations and failures, and how to recover. Schema and permissions are in [clickhouse.md](clickhouse.md); internal design in [architecture.md](architecture.md).

## Collector operation

- **Backfill** reads a consistent snapshot of the source's projections plus its event history, bounded by the source's projection high-water mark, and records that boundary as the incremental cursor. There is no gap or overlap between backfill and live sync.
- **Incremental sync** polls the event log every `poll_interval` and re-reads the entities touched by new events. Sequence gaps from compaction are handled.
- **Reconciliation** re-reads all source state every `reconcile_interval` (and whenever the event log was compacted past the cursor or replaced). Unchanged records are not re-sent.
- **Spool**: records and the cursor are committed atomically to a local SQLite database. Delivery batches rows per table, compresses them, and retries with exponential backoff and jitter. The spool is bounded; collection pauses when it is full.
- **Only one collector** runs per data directory (lease). `vario backfill` refuses to run while the collector is active, because the collector backfills new sources itself. A lease left by a collector that crashed on the same host is taken over immediately; otherwise it expires after 30 s. A collector that loses its lease (for example after the machine slept past the expiry and another collector started) stops with exit code 1.
- **Source replacement**: the cursor records the id of the last processed event. If the T3 Code database is replaced or restored from a backup, the collector detects it, re-reads the full state, and starts a new version epoch so the replacement's state wins.
- **Shutdown**: `SIGINT`/`SIGTERM` stop polling and delivery; pending data stays spooled and is delivered on the next start.
- **No administrative operations**: at startup, about once a minute while running, and after an "unknown table" error, the collector inspects the schema and the destination id read-only. A missing or incompatible schema stops delivery (data stays spooled) and is reported in the log, `vario status` and `vario doctor`, pointing to `vario migrate`; outdated views are only a warning.

## Destinations and queued data

Every queued record, every source cursor and every change fingerprint belongs to one **destination**. Data queued for one destination is never delivered to another, and a destination has its own cursors, so switching to a new database reads every source again in full for it. Vario does not assume that the new database already has what was delivered to the old one.

**Identity.** A destination is identified by the destination id stored in its `vario_meta` table (schema version 2). Until that id is known (ClickHouse unreachable, or a version 1 schema), it is identified by the normalized endpoint (lower-case scheme and host, explicit port, no trailing slash) and the database name. Credentials are never part of it: changing the operational user or rotating its password keeps the destination. The same database reached through another URL is recognized by its id and keeps its state. A database that was dropped and recreated (and migrated again) has a new id, and is treated as a new destination that must be repopulated. The collector compares the id at every start and about once a minute while it runs, also when it is idle.

**A replaced database.** If the comparison finds another id (the database was dropped and recreated, restored from elsewhere, or the URL now leads to another database), the collector stops at once instead of delivering only later changes. Nothing is changed or discarded: queued rows stay in the local spool, the recorded id is kept, and every later start is refused. The log, `vario status`, `vario destination` and `vario doctor` report "DATABASE REPLACED" with both ids, the number of queued rows and the steps below; `vario doctor` also compares the ids itself, so it finds a replacement no collector has seen yet. Vario never accepts the new database or migrates it on its own:

- If it is unintended (a wrong URL, or the original database is being restored), fix that. Once the database reports the recorded id again, the collector starts and delivers normally and the report disappears.
- To accept it as a new, empty destination, run `vario destination switch` (collector stopped, see below). `--move-pending` also delivers the queued rows to it (recommended), `--discard-pending` deletes them, and `--keep-pending` leaves them in the spool, bound to the replaced database, where they are not delivered until you decide with `vario destination pending` (see [Rows kept for a previous destination](#rows-kept-for-a-previous-destination)). In every case the new destination starts without cursors, so the next `vario run` reads every source again in full and delivers the complete history. (Rows delivered in the minute before the replacement was noticed are simply sent again.)
- The switch needs to read the new database's id: if the database has no Vario schema yet, run `vario migrate` first. The switch changes nothing while the database cannot be checked.

If the database stops reporting an id at all although one is recorded (`vario_meta` was dropped or is no longer readable for the collector's user), nothing is delivered and nothing is lost until the id is readable again; `vario migrate` on a recreated database gives it a new id, which is then a replacement as above.

### A database restored to an earlier state

Restoring a backup brings back the database's own `vario_meta`, so the destination id is the same as before while every row delivered after the backup is gone. The id alone cannot show that. Vario therefore keeps a **delivery checkpoint** (schema version 3):

- After the inserts of a delivery round were acknowledged, the collector inserts one small row with the next sequence number into `vario_checkpoints`. Only when that insert was acknowledged too are the delivered rows removed from the local spool and the number recorded locally, in one transaction. A checkpoint whose insert failed, or whose outcome is unknown, is never recorded; the rows stay queued and are sent again under a new, higher checkpoint. The local number therefore never runs ahead of what ClickHouse acknowledged, and every row that left the spool is covered by a checkpoint written after it.
- The collector reads the checkpoint the database holds at every start, about once a minute while it runs (also when idle), and immediately before it writes the next one. A database that holds **less than it acknowledged** was restored to an earlier state. A lower value is read again a few times first, so a replica that lags behind is not mistaken for a rollback; a higher value is fine (a checkpoint arrived without its response).
- The cost is one single-row `SELECT` and one single-row `INSERT` per delivery round and one `SELECT` per minute. Nothing is compared row by row.

When a rollback is found, the collector stops at once, as for a replaced database. Nothing is changed or discarded: queued rows stay in the local spool, cursors are kept, no checkpoint is written into the restored database, and every later start is refused. The log, `vario status`, `vario destination` and `vario doctor` report "DATABASE RESTORED TO AN EARLIER STATE" with both checkpoints, the number of queued rows and the steps below; `vario doctor` also reads the checkpoint itself, so it finds a rollback no collector has seen yet.

- If it is unintended (the wrong backup, or the newer state is being restored), fix that. Once the database holds the acknowledged checkpoint again, the collector starts and delivers normally and the report disappears.
- To keep the restored database, stop the collector and run `vario destination switch`. No choice about queued rows is needed: they belong to this database and are delivered to it. The switch resets this destination's cursors and fingerprints, so the next `vario run` (or `vario service install`) reads every source again in full and delivers the complete history. Vario cannot tell which rows the database lost, so recovery is always this complete reconciliation; rows the database still has are sent again with the same keys and versions, so the `*_latest`, `token_usage_daily` and `tool_usage` views count every record once. The switch also starts a new checkpoint history, so restoring the same old backup once more is detected again. It changes nothing while the database cannot be checked, and repeating it does nothing.

What the checkpoint does and does not guarantee:

- It detects a restore to any state from before the last acknowledged checkpoint: the usual case of an older backup, a volume snapshot or a re-synchronized replica. It also reports `vario_checkpoints` being emptied or dropped, or the collector's user losing all access to it, since the database can then no longer be verified.
- It needs schema version 3 and `SELECT, INSERT` on `vario_checkpoints` for the collector's user. A database from an earlier version keeps receiving data, and the collector, `vario status` and `vario doctor` say that a rollback cannot be detected until `vario migrate` has been run. Protection starts with the first checkpoint, which the collector writes at its next start after the migration: nothing delivered before it is vouched for, and a restore that happens before it goes unnoticed.
- It covers what was delivered, not the content of tables. Rows deleted or altered by hand, partitions dropped, a `TRUNCATE` of a data table, or acknowledged inserts that ClickHouse lost in a crash before they reached the disk (ClickHouse does not `fsync` inserts by default) leave the checkpoint in place and are not detected. Use `vario backfill --source t3code --force` after such an incident.
- A backup that is not a consistent point-in-time copy of the whole database can contain a checkpoint without the rows delivered just before it (the checkpoint table copied a moment later than a data table). If nothing else was delivered between that backup and its restore, the missing rows of that one round go unnoticed. Consistent snapshots (a volume or file-system snapshot) do not have this window; for other tools it is the time they take between capturing two tables.
- A restore that happens entirely between the read and the write of one checkpoint (a few milliseconds) would be hidden by that write.
- With several replicas, the read asks for sequential consistency (`select_sequential_consistency`), which covers shared and quorum-replicated tables. A replica that lags for longer than the repeated reads is reported as a rollback; the report is withdrawn by the next start that finds the checkpoint, without a reconciliation.
- If the local state (`state.db`) is itself restored from an older copy, the database is ahead of it. That is not a rollback: the collector continues above the database's checkpoint and re-reads from its older cursor.

**Changing the destination** (`vario config set clickhouse.url|database`, `vario config edit`, `vario init`, or editing the file directly; the collector itself checks at every start):

- If nothing is queued for the previous destination, the collector switches automatically, logs it, and backfills the new destination in full. The previous destination's cursors are kept, so switching back resumes where it left off.
- If rows are still queued for the previous destination, or the configured database was replaced or restored to an earlier state, `vario run` and `vario backfill` refuse to start and explain why. Decide with `vario destination switch`:

  - `--keep-pending` keeps the rows for the previous destination. They are not delivered while it is not in use; configure it again, or decide later with `vario destination pending` (below).
  - `--move-pending` delivers them to the configured destination instead. Re-sent rows carry the same keys and versions, so the `*_latest` and aggregate views count each record once.
  - `--discard-pending` deletes them (asks for confirmation unless `--yes`). The new destination gets a full backfill anyway; only events that the source has already pruned are lost.

  With `--move-pending` and `--discard-pending` the previous destination never receives those rows, so its cursors and fingerprints are reset together with them. If you configure it again later, every source is read again in full for it (no `backfill --force` needed); rows it already has are re-sent with the same keys and versions and count once.

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

### Rows kept for a previous destination

Rows kept with `--keep-pending` stay in the local spool, bound to the destination they were queued for. Nothing delivers them elsewhere or deletes them on its own, and they are never out of reach, also when their database was replaced and can no longer be configured:

```bash
vario destination pending               # what is kept, per destination
vario destination pending 1 --retry     # deliver them to the database they were queued for
vario destination pending 1 --move      # deliver them to the active destination instead
vario destination pending 1 --discard   # delete them
```

Without an action, `vario destination pending` lists each destination that holds kept rows: its number (as in `vario destination`), endpoint, database and destination id, the number of rows and batches, the tables, the age of the oldest row and the last delivery failure, followed by the commands that apply. It only reads local state and also works while the collector runs.

- `--retry` sends the rows to their own database and nowhere else. That database must be the configured one (`clickhouse.url`, `clickhouse.database`) and report the destination id recorded for the rows; if it is another database, reports no id or cannot be reached, nothing is sent and nothing changes. Use it when a replaced database has been restored, or after configuring a previous destination again. The active destination and all cursors stay as they are. Every batch leaves the spool only after ClickHouse accepted it, so a failed or interrupted retry keeps the rest (exit code 3); run it again to continue.
- `--move` binds the rows to the active destination, which must be the configured one; the collector delivers them at its next start. This needs no connection to ClickHouse. Rows keep their keys and versions, so records the destination already has count once.
- `--discard` deletes them. Other destinations are synchronized from the sources on their own, so only events a source has already pruned are lost.

`--move` and `--discard` cannot be undone: they state what will happen and ask for confirmation (`--yes` confirms in advance; without a terminal it is required). The previous destination never receives those rows, so its cursors and fingerprints are reset in the same transaction, as with `switch --move-pending`. Each of the two is one transaction: after an interruption or a crash either everything happened or nothing did. Repeating a finished action reports that nothing is queued and changes nothing. Like `vario destination switch`, the actions need the collector to be stopped and fail at once, naming it, while one runs.

## Delivery semantics

- Transport is **at-least-once**. A record is removed from the spool only after ClickHouse acknowledged its insert and, on schema version 3, the delivery checkpoint written after it (see [A database restored to an earlier state](#a-database-restored-to-an-earlier-state)). If a response is lost after ClickHouse committed an insert or a checkpoint (timeout, dropped connection, termination), the batch is sent again.
- Re-sent rows are identical (same key and version), and a newer state always has a higher version, so `FINAL` views, `argMax(..., version)` and the aggregate views return each logical record once. Physical duplicates exist in the base tables until background merges remove them (`OPTIMIZE TABLE ... FINAL` forces this).
- `insert_deduplication_token` additionally drops an identical retried batch, but only within ClickHouse's deduplication window (last 1000 blocks per table, non-replicated tables). Correctness does not depend on it.
- ClickHouse keeps remembering those tokens after the rows are gone (`ALTER ... DELETE`, `REPLACE PARTITION`, `RESTORE` into existing tables). A historical replay is therefore never sent under the tokens of an earlier delivery: `vario backfill --force`, and the complete re-read after `vario destination switch` reset a destination's cursors (an accepted rollback, moved or discarded queued rows), queue their rows under a new replay identity, so ClickHouse stores them instead of acknowledging them as already seen. Retries of a queued batch keep its token.
- Vario does not provide exactly-once delivery, and ClickHouse is not told about records deleted in the source (see limitations).

## Recovery

- **ClickHouse unreachable**: data stays spooled; delivery retries with exponential backoff up to `retry_max_backoff`. `vario status` shows pending rows and the last error. Batches rejected with a non-retryable error (for example a schema mismatch) are retried every `retry_max_backoff` and do not block other batches.
- **Spool full** (`spool.max_size_mb`): collection pauses without advancing cursors and resumes when delivery catches up; nothing is dropped.
- **Re-sending everything**: stop the collector, then `vario backfill --source t3code --force`. After dropping and recreating the database, run `vario migrate` (new destination id), then `vario destination switch`; the collector repopulates the new database by itself. After restoring the database from a backup, run `vario destination switch` as well: the collector then delivers the complete history again (see [A database restored to an earlier state](#a-database-restored-to-an-earlier-state)).
- **Resetting local state**: stop the collector and delete `state.db*` in the data directory. Undelivered spooled data is lost; the next start re-reads the source's current state (events that T3 Code has already pruned cannot be recovered).

## Upgrading from Vario 0.1

Nothing has to be recreated:

1. **Local state**: on its first start, the new version converts the spool in place. Queued rows, cursors and fingerprints are kept and assigned to the configured destination. If you change `clickhouse.url` or `clickhouse.database` with `vario config set`, `config edit` or `init` before the collector has run with the new version, the old state is bound to the previous destination first. If you edit the file by hand before that first start, it is assigned to the new settings, as 0.1 would have done. The conversion refuses to run while a collector of the old version still holds the lease; restart the service (`vario service restart`).
2. **Schema**: a 0.1 schema (version 1) keeps working: the collector delivers to it and warns. Run `vario migrate` once to add `vario_meta`; this records the destination id and, if the collector user lacks `SELECT` on the new table, offers to grant it. The collector binds the id to the existing destination at its next check (no new backfill). Until then a replaced database cannot be detected, which `vario doctor` reports as a warning. The same migration adds `vario_checkpoints` (schema version 3) and offers to grant `SELECT, INSERT` on it; a version 2 schema is upgraded the same way and keeps its destination id. The collector writes its first delivery checkpoint at its next start, without reading any source again; until then a database restored to an earlier state cannot be detected, which is reported as a warning too.
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

The service uses only the operational credentials and never needs administrative access, so it can restart unattended. When a destination change requires a decision, or the database was replaced or restored to an earlier state, it exits with an explanation in `~/Library/Logs/Vario/vario.log` and launchd retries every 30 s until `vario destination switch` resolves it (see [Destinations](#destinations-and-queued-data) for the stop/switch/start sequence).

The service does not inherit your shell's environment. `vario init` therefore writes `collector.data_dir` as an absolute path, so the CLI and the service share the same spool, cursors and lease even when `$VARIO_HOME` is set. `vario service install` refuses a configuration whose empty `data_dir` would resolve to a different directory for the service, and `vario doctor` reports it. If the password comes only from `$VARIO_CLICKHOUSE_PASSWORD`, install warns that the service cannot see it; store it with `vario config set-password`.

Linux (systemd user units) and Windows service installation are **not implemented**. On those platforms, run `vario run` under your own supervisor.

## Troubleshooting

- **"administrative login ... failed (authentication)"**: wrong administrative user or password. Init offers to retry, enter other credentials, or continue with manually managed setup.
- **Databases or users cannot be listed**: the administrative account lacks `SHOW DATABASES`/`SHOW USERS` (common on managed services). Init asks for the name instead.
- **"privileges could not be fully determined"**: partial revokes, non-default roles, column-level grants or no access to `system.grants`. Continue: the operational login check is authoritative.
- **"sessions are read-only (readonly = 1)"**: the operational user has a read-only settings profile; assign a profile with `readonly = 0`.
- **"insert into ... rejected"**: the user lacks `INSERT` on a table (possibly through a revoke; rerun `vario init` or grant it), or a session restriction applies (settings constraint, exhausted quota; see the error code).
- **"privileges could not be listed: ... does not support CHECK GRANT"** (ClickHouse 24.8): informational; the insert probe still verifies inserts.
- **"! not verified: ..."** in the login check: that check could not be performed. The other results still apply; check again once the cause (for example a missing schema) is fixed.
- **"An earlier `vario init` created ClickHouse user ..."**: setup was interrupted after creating a user. Run `vario init` and choose "Finish the setup with this user"; see [If setup is interrupted](getting-started.md#if-setup-is-interrupted).
- **`vario destination switch` / `vario destination pending`: "a collector is running" / "the background service is running the collector"**: stop it as shown in the message, run the command, and start it again.
- **"rows are queued for … and are not delivered while it is not in use"**: they were kept with `--keep-pending`. `vario destination pending` shows them and retries, moves or discards them (see [Rows kept for a previous destination](#rows-kept-for-a-previous-destination)).
- **`vario service install`: "the configuration leaves collector.data_dir empty"**: run the `vario config set collector.data_dir ...` command it prints, then install again.
- **"migration required"** in the log or `vario status`: run `vario migrate`. The data stays spooled.
- **"the ClickHouse destination changed"**: see [Destinations](#destinations-and-queued-data).
- **"DATABASE REPLACED" / "not the one Vario delivered to before"**: the database was recreated (new destination id) or the URL now leads to another database. Delivery is suspended and queued rows are kept; restore the original database or run `vario destination switch` (see [A replaced database](#destinations-and-queued-data)).
- **"DATABASE RESTORED TO AN EARLIER STATE" / "was restored to an earlier state"**: the database carries the recorded destination id but holds a lower delivery checkpoint than it acknowledged: a backup was restored, or `vario_checkpoints` was emptied or is no longer readable for the collector's user. Delivery is suspended and queued rows are kept; restore the newer state (or the privilege), or run `vario destination switch` to keep the database and deliver the complete history again (see [A database restored to an earlier state](#a-database-restored-to-an-earlier-state)).
- **"delivery checkpoint: rows were sent but stay queued"**: the rows were inserted, but the checkpoint that covers them could not be read or written, so they are sent again later. This resolves itself when ClickHouse is reachable again; if it names a missing privilege, grant `SELECT, INSERT` on `vario_checkpoints` (`vario migrate`).
- **"no longer reports a destination id"**: `vario_meta` is missing or not readable for the collector's user. Restore `SELECT` on it, or run `vario migrate` if the database was recreated.
- **"the local state was created by an earlier Vario version whose collector is still running"**: `vario service restart`.

## Current limitations

- T3 Code is the only supported source.
- Service installation is implemented for macOS only.
- Credentials are stored in a `0600` file, not in the OS keychain.
- Clusters: no `ON CLUSTER` provisioning and no replicated schema management; untested (see [Managed services, external authentication and clusters](clickhouse.md#managed-services-external-authentication-and-clusters)).
- Users created by Vario use `sha256_hash ... SALT` password authentication, verified with ClickHouse 24.8 and 25.8; other versions are untested. LDAP, Kerberos, certificate, SSH-key and other external identities are not provisioned; existing ones can be selected, but Vario authenticates only with a user name and password.
- Grant inspection is advisory. The operational login check is stronger evidence but writes no rows, so quotas on written data, disk space and server-side insert limits are confirmed only by the first delivery. On ClickHouse 24.8 the privilege list (`CHECK GRANT`) is not available, and the warning about administrative privileges held by the operational user is not shown by `vario doctor` or the collector (guided setup still reports it from `system.grants`).
- A generated password is kept in `pending-credentials.json` until the configuration is saved; if that file is deleted before then, only an administrator can reset the password.
- Rollback detection needs schema version 3 (`vario migrate`) and has the limits listed under [A database restored to an earlier state](#a-database-restored-to-an-earlier-state): it compares delivery checkpoints, not table contents.
- The destination id needs schema version 2 (`vario migrate`). Until then, and while ClickHouse is unreachable, a destination is identified by endpoint and database, so a different host name for the same server counts as a new destination (the full backfill to it is harmless: rows carry the same keys and versions).
- Deletions in the source are reflected only for conversations and projects (`deleted_at` / `is_deleted`); messages or items hard-deleted from T3 Code remain archived in ClickHouse.
- Events T3 Code prunes from its event log before Vario reads them are missing from `raw_events`; the projected state is still collected through reconciliation.
- A replaced T3 Code database is detected by the event id at the cursor. If T3 Code has also pruned that event, detection falls back to the log-length check and the periodic reconciliation.
- Delivery is at-least-once; see [Delivery semantics](#delivery-semantics). Base tables can contain duplicate rows until merges.
- Migrations never alter existing tables: a table missing a column Vario expects is reported as incompatible.
- Configuration writes are serialized between Vario processes with a lock file. Changes made by other programs at the same moment are detected by `vario init`, `vario config set` and `vario config edit` (content fingerprint), but editors that keep the file open may still overwrite Vario's changes when they save.
- Tested only against self-hosted ClickHouse 24.8 and 25.8 (including users defined in `users.xml`). **ClickHouse Cloud and other managed services are untested.**
