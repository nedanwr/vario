# Distribution

Vario is currently distributed through **GitHub Releases**. Each release has the six archives, `checksums.txt`, `install.sh` and `install.ps1` attached. The installers download from there, and the commands people copy run the installer scripts from `raw.githubusercontent.com`. No other infrastructure is needed.

Download URLs are built in exactly one place in each installer, and the release workflow follows the installers' setting. Moving to Vario's own download domain (Cloudflare R2) later changes a few constants and turns on two dormant workflow jobs; see [Moving to a custom domain](#moving-to-a-custom-domain).

This document covers the URLs, the installers, the release workflow, publishing and the future migration. The build itself (targets, reproducibility, versioning) is described in [Development](development.md#releases).

## Install commands

| Platform     | Command                                                                                          |
| ------------ | ------------------------------------------------------------------------------------------------ |
| macOS, Linux | `curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh \| sh`       |
| Windows      | `irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 \| iex`           |

A specific version, a channel or another directory:

```bash
curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh -s -- --version v0.1.0-beta.1
curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh -s -- --channel stable --install-dir ~/bin
```

```powershell
$env:VARIO_VERSION = 'v0.1.0-beta.1'; irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1))) -Version v0.1.0-beta.1
```

To read the installer before running it: `curl -fsSLO https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh && less install.sh && sh install.sh`.

## URLs

| What                         | URL                                                                                              |
| ---------------------------- | ------------------------------------------------------------------------------------------------ |
| Installer (follows `master`) | `https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh` (and `install.ps1`)  |
| Installer of one release     | `https://github.com/nedanwr/vario/releases/download/<tag>/install.sh` (and `install.ps1`)        |
| Archives                     | `https://github.com/nedanwr/vario/releases/download/<tag>/vario_<version>_<os>_<arch>.tar.gz` (`.zip` for Windows) |
| Checksums                    | `https://github.com/nedanwr/vario/releases/download/<tag>/checksums.txt`                         |
| All releases                 | `https://github.com/nedanwr/vario/releases`                                                      |
| Release list (installers)    | `https://api.github.com/repos/nedanwr/vario/releases?per_page=100`                               |

`<tag>` is the Git tag (`v0.1.0-beta.1`) and `<version>` is the tag without `v`. `<os>` is `darwin`, `linux` or `windows`, and `<arch>` is `amd64` or `arm64`. For example: `https://github.com/nedanwr/vario/releases/download/v0.1.0-beta.1/vario_0.1.0-beta.1_darwin_arm64.tar.gz`.

- **The raw installer URL is stable.** It serves `scripts/install.sh` from `master`, which only changes through a release merge. GitHub caches it for about five minutes. After the move to a custom domain it keeps working, because the installer on `master` will then download from the new domain.
- **Do not link `releases/latest/download/...`.** GitHub's "latest" release skips prereleases, and every beta is a prerelease, so it returns 404 until the first stable release.
- **The website** uses the two install commands above and links to `https://github.com/nedanwr/vario/releases` for manual downloads, until the custom domain exists. Avoid building archive URLs on the website. If it must, follow the pattern above with an explicit tag.

## Channels

Without `--version`, the installers install the newest release of a channel (`beta` by default, or `stable`):

- **`stable`**: releases without a prerelease part (`v1.2.0`).
- **`beta`**: stable releases plus `-beta`, `-beta.N`, `-rc` and `-rc.N` prereleases, so beta users are never behind stable.
- Any other prerelease (e.g. `v0.2.0-alpha.1`) is on no channel and is installed only with `--version`.
- "Newest" means SemVer precedence, not publication date: `v0.1.0-rc.1` is newer than `v0.1.0-beta.10`.

With the GitHub source, the installers find the channel's newest release by listing the repository's releases through the GitHub REST API (`/repos/nedanwr/vario/releases`). They do not use `/releases/latest`, because it skips prereleases.

- Anonymous API requests only see **published** releases. The release workflow publishes a release only after all its assets are uploaded and verified, so publishing is what moves the channels, and a half-uploaded release is never installed.
- The API allows 60 anonymous requests an hour per IP address. When that limit is reached, the installer says so and suggests `--version`, which makes no API request: an explicit version downloads straight from `releases/download/<tag>/`.
- Only the 100 most recently created releases are considered.
- `--channel stable` fails with a clear message until the first stable release exists.

`scripts/release/r2-publish.sh` applies the same channel rules to the mirror's `channels/*.txt`, and the installer tests check that all three implementations agree.

## Installers

`scripts/install.sh` (POSIX `sh`; macOS and Linux) and `scripts/install.ps1` (Windows PowerShell 5.1 and PowerShell 7) behave the same way:

1. **Platform**:
   - Detects the OS and CPU architecture: `amd64` or `arm64`.
   - Uses the native arm64 build under Rosetta 2, and for a 32-bit or x64-emulated PowerShell on ARM64.
   - Refuses anything else (FreeBSD, 32-bit, ARMv7, …) with a clear message. Git Bash/MSYS is pointed to `install.ps1`.
2. **Version**: `--version`/`-Version` or `VARIO_VERSION`. Otherwise the channel's newest release (see [Channels](#channels)).
3. **Existing installation**: the installer checks it before downloading anything.
   - **Same version**: nothing happens (`--force`/`-Force` reinstalls).
   - **Newer version installed**: refused, because Vario does not support downgrades (`--force` overrides).
   - **Symlink** (e.g. from a package manager): not replaced without `--force`.
4. **Download**:
   - HTTPS only (TLS 1.2+), and any final status other than 200 fails.
   - GitHub release downloads redirect to GitHub's storage host. Up to 5 redirects are followed, each only to an `https://` URL. A mirror must answer directly; redirects from it are refused.
   - Transient errors are retried.
   - Uses `curl`, or GNU `wget`. BusyBox `wget` is refused because it may not verify certificates.
5. **Verification**:
   - The archive's SHA-256 must match exactly one entry in the release's `checksums.txt`, or nothing is installed.
   - The extracted binary is run (`vario version --short`) and must report the expected tag before it replaces anything.
6. **Install**: into a user-writable directory, without administrator rights:
   - macOS and Linux: `~/.local/bin`, or `--install-dir`/`VARIO_INSTALL_DIR`.
   - Windows: `%LOCALAPPDATA%\Programs\Vario`, or `-InstallDir`.

   The new binary is staged in the same directory and renamed into place. A running `vario.exe` on Windows is renamed out of the way first.
7. **PATH**:
   - `install.sh` never edits shell startup files. If the directory is not on `PATH`, it prints the exact line to add for zsh, bash or fish.
   - `install.ps1` adds the directory to the *user* `PATH` (HKCU, preserving `%VARIABLES%`) unless `-NoModifyPath` or `VARIO_NO_MODIFY_PATH=1` is given.
   - Both warn if another `vario` earlier on `PATH` would shadow the new one.
8. **Never** touches `~/.vario` (configuration, password file, state). It never runs `vario init`, `migrate` or `service`, and never installs, starts or restarts the background service.
   - Next steps: `vario init` for a new installation, or `vario doctor` when a configuration exists.
   - On macOS with an installed service, `vario service restart` when the service runs the same path. If it runs another path, the installer says how to point it at the new binary.
9. **Failures**: a rate-limited or failed API request, an interrupted download, a checksum mismatch, malformed metadata, a 404, an unwritable directory or Ctrl-C all leave the existing installation untouched and remove temporary files. Under `irm | iex`, errors do not close the PowerShell window.

There is no telemetry and no `sudo`. The only tools needed are `curl`/`wget`, `tar`, `gzip` and `sha256sum`/`shasum`/`openssl`; PowerShell needs only built-in cmdlets. Release lists are parsed without `jq`, and with `ConvertFrom-Json`'s size limit in Windows PowerShell 5.1 avoided.

### Download source

`--source`/`-Source` (or `VARIO_DOWNLOAD_SOURCE`) selects where releases come from:

| Source         | Release files                                     | Channel lookup                       |
| -------------- | ------------------------------------------------- | ------------------------------------ |
| `github`       | `https://github.com/<repo>/releases/download/<tag>/<file>` | GitHub REST API release list |
| `https://HOST` | `https://HOST/releases/<tag>/<file>`              | `https://HOST/channels/<channel>.txt` |

The default is the `DEFAULT_SOURCE` constant (`$DefaultSource` in PowerShell), currently `github`. In each installer, the settings sit together under "Download source" at the top: the default source, the repository (`nedanwr/vario`) and the install commands shown in messages. Only two functions build URLs: `resolve_channel` and `asset_url` (`Resolve-Channel` and `Get-AssetUrl`). `VARIO_GITHUB_URL` and `VARIO_GITHUB_API_URL` (`https://` only) point the GitHub source elsewhere. They exist for the tests and for GitHub mirrors.

## Release workflow

`.github/workflows/release.yml` runs only when a tag matching `v*` is pushed. Pull requests and pushes to `master` run the separate Build workflow, which publishes nothing.

| Job       | Runs                                     | Permissions                  | Does                                                                                                                                                       |
| --------- | ---------------------------------------- | ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `build`   | always                                   | `contents: read`             | Validates the tag (SemVer) and that it points to a commit on `master`, and checks the changelog section and the installers' download source. Runs vet, unit tests and the installer tests, then builds and checks all six archives **once**. |
| `r2`      | mirror only                              | none (environment `release`) | Uploads to R2. Skipped while the installers use `github`. See [Moving to a custom domain](#moving-to-a-custom-domain).                                      |
| `publish` | after `build` (and `r2`, if it ran)      | `contents: write`            | Creates a **draft** GitHub Release with the archives, `checksums.txt`, `install.sh` and `install.ps1`. It checks the asset list, downloads every asset back and compares its SHA-256 with the build, then publishes. Prerelease tags become GitHub prereleases. |
| `promote` | mirror only                              | none (environment `release`) | Moves the mirror's channel files. Skipped while the installers use `github`.                                                                               |
| `verify`  | after `publish`                          | none                         | Installs the published release with its own attached installers on `ubuntu-24.04`, `macos-15` and `windows-2025`, the last with both Windows PowerShell 5.1 (`-File`) and PowerShell 7 (`irm \| iex`). It checks that `vario version --short` reports the tag and that `.vario` was not created. On Linux it also resolves the beta channel; that step is non-blocking because of API rate limits. |

- **One build.** `publish` attaches exactly the files that `build` produced and tested. The installers attached to a release are the scripts at the tagged commit.
- **The installers decide the source.** `build` reads `DEFAULT_SOURCE` from `install.sh` and `$DefaultSource` from `install.ps1`.
  - They must be equal, and the repository constant in each must match the repository running the workflow.
  - `github` skips `r2` and `promote`; no Cloudflare secrets are read, and none need to exist.
  - The workflow's `DOWNLOAD_BASE_URL` turns them on.
  - Any other value stops the release.
- **Channels move when the release is published.** Until `publish` succeeds there is no new published release, so `curl … | sh` keeps installing the previous one. A failure while uploading leaves a draft, which anonymous users cannot see.
- **No silent overwrite.** An existing draft stops the run. An existing published release is accepted on a re-run only if its assets are identical, by name and SHA-256, to the new build; otherwise the run stops without changing it.
- **`verify` runs after publication.** If it fails, the release is already public. Fix the problem in a new version, and delete or edit the broken release by hand if needed.

### Re-runs

| Situation                                              | What to do                                                                                                                    |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------- |
| `build` failed                                         | Fix the cause on `master`, delete the tag and tag again. Nothing was published.                                                |
| `publish` failed after creating a draft                | Delete the draft release, then **Re-run failed jobs**.                                                                        |
| `publish` failed after publishing (e.g. at the last step) | **Re-run failed jobs**. The published release is accepted if its assets are identical.                                     |
| `verify` failed                                        | Read the log. If the cause is GitHub flakiness, **Re-run failed jobs**; otherwise fix it in a new version.                     |
| "already published with different assets"              | A rebuild produced different bytes. Published releases are never replaced; investigate, then release a new version.           |

Prefer **Re-run failed jobs** over **Re-run all jobs**: it reuses the original build artifacts. The build is reproducible for the same Go version and runner tools, so a full re-run normally produces identical archives anyway.

## Repository setup

Nothing is needed beyond the defaults for GitHub distribution: no secrets, variables or environments. Recommended:

1. **Workflow permissions** (Settings → Actions → General): *Read repository contents permission*. The workflow requests `contents: write` only for the `publish` job.
2. **Tag ruleset** (Settings → Rules → Rulesets → New tag ruleset): target `v*`, restrict creation, update and deletion to maintainers. Anyone who can push a `v*` tag on `master` can publish.
3. The repository must be **public**: the installers and `raw.githubusercontent.com` downloads are anonymous.

## Publishing the beta

Follow [Development → Releases](development.md#releases) for the changelog, checks and the `master` merge. Specific to distribution:

1. Locally, run `scripts/release/test-installers.sh`.
2. Merge to `master`, then push the tag (`git push origin v0.1.0-beta.1`) and watch the run. All `verify` jobs should pass.
3. Afterwards, on clean accounts:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh
   vario version
   ```

   On Windows: `irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex`, then `vario version` in a new terminal.

## Pre-launch checks

- [ ] The repository is public, and `https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh` serves the new installer once it is merged. Before then, the README's install commands return 404, so merge the documentation together with, or just before, the tag.
- [ ] Before the first release, `curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh` fails with "no beta release of Vario is published on GitHub yet" (and does not install anything).
- [ ] After the release: the `verify` jobs passed; a clean-machine install works on macOS (arm64 and, if possible, Intel), Linux (amd64 and arm64) and Windows (amd64; arm64 if available); the installed binary reports `v0.1.0-beta.1`.
- [ ] The release page lists the six archives, `checksums.txt`, `install.sh` and `install.ps1`, and is marked as a pre-release.

## Security notes and limitations

- **Unsigned binaries.** Release binaries are not code-signed, and the macOS binaries are not notarized.
  - Files fetched by the installers (curl, PowerShell) do not get the quarantine attribute or Mark of the Web, so Gatekeeper and SmartScreen normally do not block them.
  - Archives downloaded with a **browser** do: on macOS, Gatekeeper refuses to open the binary until the quarantine attribute is removed (`xattr -d com.apple.quarantine vario`, after verifying the checksum); on Windows, SmartScreen may warn. See [Code signing](getting-started.md#code-signing).
  - Notarization would need an Apple Developer ID; Authenticode signing would need a certificate. Neither is set up.
- **Checksums prove integrity, not authorship.** `checksums.txt` comes from the same GitHub release as the archives. It detects corrupted or truncated downloads, but someone who could edit the release could replace both. The protections are TLS, GitHub account security and the tag ruleset. Signed checksums or build provenance (e.g. Sigstore, GitHub artifact attestations) would add publisher authenticity and are not implemented yet.
- **`curl | sh` trusts GitHub and the `master` branch.** Users who prefer to inspect first can download the script, read it and run it. They can also use the copy attached to a release, or download manually.
- **API rate limit.** Channel lookups use GitHub's anonymous API (60 requests an hour per IP). Shared addresses (offices, CI) can hit it; `--version` avoids the API entirely.
- The installers are tested on Linux and macOS (`install.sh`) and with PowerShell 7 on Linux simulating Windows (`install.ps1`), with Windows PowerShell 5.1 compatibility checked statically (PSScriptAnalyzer). Real Windows runs happen in the Release workflow's `verify` job, after publication.

## Testing

| Command                              | Covers                                                                                                                         | Needs                                   |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------- |
| `scripts/release/test-installers.sh` | Both installers against synthetic archives over local HTTPS, through a fake GitHub and a static mirror. It covers channel resolution from API responses (prereleases, ordering, unrelated tags, quoted text in release notes, pretty-printed JSON, empty lists, HTML, rate limits), GitHub's download redirects (and refusing an `http://` redirect or a redirect loop), platform selection, versions, upgrades, downgrades, checksum and download failures, PATH and configuration handling, and the built-in GitHub URLs (without contacting GitHub). It also checks that SemVer ordering and channel membership agree across all three implementations, and that both installers default to the same source. Runs in the Build and Release workflows. | `python3`, `openssl`, `curl`, `zip`; `pwsh` for `install.ps1` |
| `scripts/release/test-r2-publish.sh` | The R2 path for the future mirror: `r2-publish.sh` end to end against an S3 emulator (moto) and a local HTTPS "custom domain", including a real install of the built binary from the published files. | Go, Docker                              |

## Moving to a custom domain

The R2 publishing path is already in the repository and tested (`scripts/release/r2-publish.sh`, `test-r2-publish.sh`, and the `r2` and `promote` jobs). The workflow's `DOWNLOAD_BASE_URL` is currently `https://vario.nedanwr.dev`; change it if the domain ends up being, for example, `https://downloads.<domain>`. To switch:

1. **Cloudflare**: set up the bucket, custom domain and token as in [R2 and DNS](#r2-and-dns).
2. **GitHub**: create the `release` environment with its secrets and variables, as in [GitHub environment](#github-environment).
3. **Workflow**: set `DOWNLOAD_BASE_URL` in `.github/workflows/release.yml` to the download host, e.g. `https://downloads.<domain>`.
4. **Installers**: in both scripts, change the "Download source" block.
   - Set `DEFAULT_SOURCE` / `$DefaultSource` to the same URL.
   - Update `INSTALL_SH_COMMAND`, `INSTALL_PS1_COMMAND` and `$InstallShCommand`, and the usage comments at the top of each file.
   - In `scripts/release/test-installers.sh`, update the "Defaults" check, which pins `github` on purpose.
   - Then `build` turns on `r2` and `promote`, and refuses to release if the two installers disagree.
5. **Install commands**: if the installers should be served from their own host (`https://install.<domain>/install.sh`), connect that host to the same bucket as a second custom domain. The `promote` job publishes the root `install.sh` and `install.ps1`. To serve the script at the bare host (`curl -fsSL https://install.<domain> | sh`), add a Cloudflare URL rewrite from `/` to `/install.sh` (a rewrite, not a redirect). Update the README, `docs/getting-started.md`, this document and the website.
6. **Older releases**: releases published before the switch exist only on GitHub. To make them installable from the mirror, download each release's assets from GitHub and run `TAG=<tag> DIST_DIR=<assets> INSTALLERS_DIR=<scripts> scripts/release/r2-publish.sh upload` with the R2 credentials. The checksums must match the GitHub assets. Then run `promote` for the newest one. Otherwise, only releases made after the switch are on the mirror, and `--version` for older ones must use `--source github`.
7. GitHub Releases stay as a second copy: `publish` keeps attaching the same files. The old `raw.githubusercontent.com` commands keep working, because the installer on `master` then downloads from the mirror.

### Mirror layout

| Path                                                | Content                                                         | Changes                    | `Cache-Control`                       |
| --------------------------------------------------- | --------------------------------------------------------------- | -------------------------- | ------------------------------------- |
| `releases/<tag>/vario_<version>_<os>_<arch>.tar.gz` | macOS and Linux archives                                        | never                      | `public, max-age=31536000, immutable` |
| `releases/<tag>/vario_<version>_windows_<arch>.zip` | Windows archives                                                | never                      | same                                  |
| `releases/<tag>/checksums.txt`                      | SHA-256 of the six archives (`sha256sum` format)                | never                      | same                                  |
| `releases/<tag>/install.sh`, `install.ps1`          | the installers as they were at `<tag>`                          | never                      | same                                  |
| `channels/beta.txt`, `channels/stable.txt`          | one line: the channel's current tag                             | moved forward by `promote` | `public, max-age=60`                  |
| `install.sh`, `install.ps1`                         | installers of the newest release on any channel                 | updated by `promote`       | `public, max-age=300`                 |

- **Immutability**: an object under `releases/` is never overwritten. If a key already exists, the publish script compares its bytes. If they are identical, the upload is skipped. If they differ, the release stops with an error.
- **Order**: `r2` uploads and verifies every file through the public domain before the GitHub Release is published. `promote` moves the channels last. A channel only moves forward in SemVer order, and re-running an older tag never moves it back.
- Each object carries its SHA-256 in the `x-amz-meta-sha256` metadata, for auditing.

### R2 and DNS

1. **Zone**: the domain must be an active zone in the same Cloudflare account as the bucket; R2 custom domains require it.
2. **Bucket**: create a bucket, e.g. `vario-downloads` (standard storage class). Do **not** enable the `r2.dev` public development URL: it is rate-limited and not meant for production.
3. **Custom domain**: Bucket → Settings → Custom Domains → connect the download host. Cloudflare creates the proxied DNS record and the edge certificate. Set the minimum TLS version to 1.2 or higher.
4. **Immutability (recommended)**: Bucket → Settings → Bucket lock rules → add a rule for prefix `releases/` with indefinite retention. Do not add lifecycle rules that expire `releases/`.
5. **Caching**: keep Cloudflare's default behaviour of honouring each object's `Cache-Control`. Do not give `channels/*` or `/install.*` a long edge TTL, and do not add redirects for release paths: the installers refuse redirects from a mirror.
6. **API token**: R2 → Manage R2 API Tokens → **Account API token** with **Object Read & Write** on this bucket **only**, optionally with a TTL. Note the Access Key ID, Secret Access Key (shown once) and the account ID.

R2 does not accept GitHub's OIDC tokens, so the token is long-lived. Its exposure is limited by bucket scoping, the environment rules below, and rotation.

### GitHub environment

1. **Environment** `release` (Settings → Environments):
   - **Deployment branches and tags**: *Selected branches and tags*, with a **tag** rule `v*`.
   - Optional: **Required reviewers**.
   - **Secrets**: `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`.
   - **Variables**: `R2_ACCOUNT_ID`, `R2_BUCKET`.
2. `r2` and `promote` have no repository permissions. Credentials reach only the two steps that call R2, as environment variables, and are never echoed.

### Mirror pre-launch checks

- [ ] `curl -sI https://<download host>/channels/beta.txt` returns 404 from Cloudflare before the first mirrored release: no TLS or DNS error, and no redirect. Plain `http://` does not serve content.
- [ ] The bucket's `r2.dev` URL is disabled, and the token cannot access other buckets.
- [ ] R2 honours create-only uploads. Run this twice; the second call must fail with `PreconditionFailed`:

  ```bash
  aws --endpoint-url https://<account-id>.r2.cloudflarestorage.com --region auto s3api put-object \
    --bucket vario-downloads --key test/precondition --body /dev/null --if-none-match '*'
  ```

  Delete `test/precondition` afterwards. If R2 rejects the header outright, remove `--if-none-match` from `put_object` in `scripts/release/r2-publish.sh`; the script's existence-and-hash check still protects published files.
- [ ] The bucket lock rule on `releases/` rejects an overwrite.
- [ ] `scripts/release/test-r2-publish.sh` passes.
