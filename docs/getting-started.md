# Getting started

Install Vario, connect it to ClickHouse and collect your first data. Configuration details are in [configuration.md](configuration.md), ClickHouse provisioning in [clickhouse.md](clickhouse.md), and what is collected in [privacy.md](privacy.md).

## Install and build

Prebuilt binaries for macOS, Linux and Windows (amd64 and arm64) are attached to each [GitHub Release](https://github.com/nedanwr/vario/releases), together with a `checksums.txt` file. Download the archive for your platform, [verify its checksum](#verifying-downloads), extract it, and put `vario` (`vario.exe` on Windows) on your `PATH`. The macOS binaries are not signed or notarized; see [Code signing](#code-signing).

To build from source, you need Go 1.25 or newer. The binary is a standalone executable with no CGO and no runtime dependencies.

```bash
git clone https://github.com/nedanwr/vario.git
cd vario
make build            # writes bin/vario with version information
# or: go build -o bin/vario ./cmd/vario
```

Cross-compile with `GOOS`/`GOARCH`, e.g. `GOOS=linux GOARCH=arm64 make build`.

## Verifying downloads

Download `checksums.txt` from the same release, place it next to the archive, and run:

```bash
sha256sum --check --ignore-missing checksums.txt         # Linux
shasum -a 256 --check --ignore-missing checksums.txt     # macOS
```

On Windows (PowerShell), compare the output of `Get-FileHash vario_<version>_windows_amd64.zip -Algorithm SHA256` with the corresponding line in `checksums.txt`.

## Code signing

Release binaries are **not signed**. The macOS binaries are not notarized, so Gatekeeper may refuse to run a binary downloaded through a browser; after verifying the checksum, remove the quarantine attribute (`xattr -d com.apple.quarantine vario`). Windows SmartScreen may also warn about the unsigned executable.

## Quick start

You need a reachable ClickHouse server (see [ClickHouse requirements](clickhouse.md#requirements)).

```bash
vario init                      # interactive setup: ClickHouse access, database, user, collection mode, sources
vario migrate                   # create Vario's schema, if init did not (administrative credentials)
vario doctor                    # check configuration, source access, ClickHouse
vario backfill --source t3code  # import existing history
vario run                       # collect continuously in the foreground (Ctrl-C to stop)
vario service install           # or: run in the background at login (macOS)
vario status                    # cursor, spool, delivery and service state
```

## `vario init`

The wizard asks for the **ClickHouse endpoint** (the HTTP(S) interface, default `http://localhost:8123`; plain `http://` to a remote host is refused unless you accept it) and then for one of two setup paths.

**Guided setup** (recommended) signs in with an administrative account, which is not saved, and then:

1. **Database**: use an existing database (picked from a list; system databases such as `system`, `default` and `INFORMATION_SCHEMA` are hidden unless you reveal them; long lists can be filtered; names can be entered manually if the account cannot list databases) or create a new one. An existing database with the chosen name is reused, never dropped or recreated.
2. **Operational user**: create a dedicated user (default name `vario`, the default for new databases) or use an existing one. For a new user you type a password or leave it empty to have Vario generate one: 32 characters from `crypto/rand`. You can choose to see it once (for a password manager). It is never logged. The new user's password is written to a private file before the user is created and kept there until a saved configuration uses it (see [If setup is interrupted](#if-setup-is-interrupted)). Vario creates the user with a salted SHA-256 hash that it computes locally, so the password itself is never sent in a statement. For an existing user, Vario first inspects its grants (directly and through roles) and says whether the required privileges appear to be present, missing, or cannot be determined, for example because of partial revokes, non-default roles or no access to `system.grants`. If privileges are missing you can pick another user, display the required SQL, or authorize Vario to grant them. Users defined in server configuration files or LDAP, which cannot be changed with SQL, are labelled and are never offered grants. Vario never changes an existing user's password or authentication settings.
3. **Schema**: offer to create or migrate Vario's schema in the database.
4. **Review**: every change is listed by category (database, schema, user, privileges) and applied only after you confirm. You can display the exact SQL first. If a step fails, the steps already applied are reported (ClickHouse DDL is not transactional), and you can retry the remaining steps, change your selections, or quit.
5. **Verification**: Vario logs in as the operational user, independently of what the administrator saw, and checks what the collector will actually do (see [How access is verified](clickhouse.md#how-access-is-verified)). The insert probe sends no rows, so nothing is written.

**Manually managed setup** uses an existing database and operational user without any administrative credentials. You enter the database, user and password; Vario can verify the login (read-only) and, if the database, user or privileges are missing, print the SQL an administrator needs to run. The schema is then created with `vario migrate` (by anyone with administrative credentials; `vario migrate --dry-run` shows its SQL).

Both paths continue with the **collection mode** (metrics only by default; messages or full execution telemetry require typing `yes`, see [Privacy](privacy.md)), the **data sources** (T3 Code is detected automatically), and a **summary**. The summary shows the endpoint, database and operational user with how each was obtained, the password status (never the password), the privileges and schema status, the connection check, the collection mode, the sources and the configuration file. Only the operational configuration and password are saved. Changes made in ClickHouse were applied when you confirmed them; quitting or interrupting init says which ClickHouse changes, if any, remain. Init never installs or starts the background service.

### If setup is interrupted

A user created by guided setup must never be left with a password nobody has. Before Vario sends `CREATE USER`, it writes the new user's password to `~/.vario/pending-credentials.json` (next to the configuration, mode `0600`; never administrative credentials). The entry is removed only after a configuration that uses the password has been saved. If the user certainly was not created (the statement failed and the user does not exist, or setup stopped before it), the entry is removed at once. If the file cannot be written, the user is not created.

- **Quit, Ctrl-C, closed input, a failed step, a crash or `SIGKILL`**: the password stays in the file. Ctrl-C restores the terminal (also during a hidden password prompt) and prints what was applied and where the password is kept; a crash or `SIGKILL` prints nothing, but the file was written before the user existed.
- **Next `vario init`**: it offers to _finish the setup with this user_ (it verifies the login and continues with the remaining steps; if ClickHouse never created the user, guided setup creates it with the kept password), to continue without it (the file is kept), or to delete the saved password (confirmation required). When the same user is chosen again in guided setup, the kept password is offered. `vario doctor` reports unfinished setups.
- **If the password is lost anyway** (for example the file was deleted): Vario cannot recover it and never changes an existing user's credentials. An administrator can set a new one (`ALTER USER vario IDENTIFIED WITH sha256_password BY '...'`) and you then enter it with `vario init` (manually managed setup) or `vario config set-password`. Alternatively, drop the user (`DROP USER vario`) and run guided setup again.

**Re-running init** is safe: existing databases, users and schema objects are discovered and reused, so nothing is duplicated, and only the settings you change are rewritten, preserving comments and formatting. The stored password is kept unless you change it. You can also keep the configuration unchanged or start over from defaults; the old file is kept as `config.toml.bak-<timestamp>`.

For scripts, init runs the manually managed path non-interactively: `vario init --non-interactive --clickhouse-url https://ch.example.com:8443 --username vario --database vario --password-stdin < pw.txt`, then `vario migrate --non-interactive --yes --admin-user default --admin-password-stdin < admin-pw.txt`. With an existing configuration, add `--update` (apply only the given flags) or `--force` (start over, with a backup). Other flags: `--mode`, `--allow-sensitive-content`, `--t3code-path`, `--no-t3code`, `--data-dir`, `--skip-connection-check`. If stdin is not a terminal, init runs non-interactively. `--create-database` is deprecated and ignored.
