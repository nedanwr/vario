# Development

Requires Go 1.25 or newer and, for integration tests, Docker.

```bash
make test             # unit tests (synthetic fixture databases; no private data)
make race             # tests with the race detector
make lint             # gofmt, go vet, staticcheck
make clickhouse-up    # start a local ClickHouse 25.8 in Docker on 127.0.0.1:18123
                      # (CH_IMAGE=clickhouse/clickhouse-server:24.8 for the other tested LTS)
make integration      # ClickHouse integration tests (provisioning, access checks, migrations, destination safety, rollback detection with BACKUP/RESTORE, versioned dedup, aggregation, ambiguous retries)
make clickhouse-down
make dist             # release archives and checksums for all platforms in dist/
```

Integration tests are skipped unless `VARIO_TEST_CLICKHOUSE_URL` is set (optionally with `VARIO_TEST_CLICKHOUSE_USER` and `VARIO_TEST_CLICKHOUSE_PASSWORD`). Each run uses a throwaway database. The rollback tests take real backups (`BACKUP DATABASE ... TO File`), which stay in the test server's backup directory until the container is removed (`make clickhouse-down`); a server that does not allow such backups skips them.

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

| OS      | Architectures | Archive                                |
| ------- | ------------- | -------------------------------------- |
| macOS   | amd64, arm64  | `vario_<version>_darwin_<arch>.tar.gz` |
| Linux   | amd64, arm64  | `vario_<version>_linux_<arch>.tar.gz`  |
| Windows | amd64, arm64  | `vario_<version>_windows_<arch>.zip`   |

Each archive contains a single directory with the executable, `LICENSE`, `README.md`, and `THIRD_PARTY_LICENSES.txt`, and nothing else. Tags follow [Semantic Versioning](https://semver.org) with a `v` prefix: `v0.1.0`, `v1.2.3`, or a prerelease like `v0.1.0-beta.1` or `v0.2.0-rc.1`. A tag with a prerelease part (anything after `-`) is published as a GitHub prerelease, which GitHub never marks as the latest release. Build metadata (`+...`) is not accepted.

### Versioning

The tag is the only place the version is defined. `make build` and `make dist` (`scripts/dist.sh`) embed it with `-ldflags -X` into `internal/buildinfo` together with the full commit hash and the build date (for `make dist`, the commit time, which keeps builds reproducible). `vario version` prints all three, plus the Go version and platform; `vario version --short` prints only the version. Without injected values (`go build`, `go run`), the version is `dev` and the commit and date come from the VCS stamp of the Go toolchain; `go install ...@<tag>` reports the module version. A build from a modified working tree reports the commit with `-dirty`. The release workflow refuses to publish unless the Linux binary reports exactly `vario <tag> (commit <first 12 characters of the commit>, ...`.

### Publishing

Before publishing, the workflow checks that the tag is valid SemVer, points to a commit in `master`'s history, and has a section `## [<version without v>]` in `CHANGELOG.md`; then it runs vet and tests and builds all archives and `checksums.txt` from a clean module cache. A separate job, the only one with write access, uploads them to a draft release whose notes are that changelog section followed by GitHub's generated notes, checks the upload, and then publishes the release. If a release or draft for the tag already exists, the workflow stops without changing it; to publish again, delete the release and re-run the workflow.

To release:

1. On `develop`, add the version's section to `CHANGELOG.md` and update any version mentioned in the README and `docs/getting-started.md` (the install examples name the current release).
2. Check locally: `make lint test`, the ClickHouse integration tests on 24.8 and 25.8 (`make integration`), and a release build (`rm -rf dist && make dist VERSION=v0.1.0-beta.1`, then `dist/.../vario version`).
3. Merge `develop` into `master` through a pull request and wait for the Build workflow to pass.
4. Tag the merged commit on `master` and push the tag:

```bash
git switch master && git pull --ff-only
git tag -a v0.1.0-beta.1 -m "Vario v0.1.0-beta.1"
git push origin v0.1.0-beta.1
```

Builds are reproducible: given the same commit and Go version, `make dist VERSION=v0.1.0-beta.1` produces the same binaries (the embedded build date is the commit time).
