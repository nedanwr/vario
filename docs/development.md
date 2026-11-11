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

Each archive contains a single directory with the executable, `LICENSE`, `README.md`, and `THIRD_PARTY_LICENSES.txt`. Tags follow [Semantic Versioning](https://semver.org) with a `v` prefix: `v0.1.0`, `v1.2.3`, or a prerelease like `v0.2.0-rc.1` (published as a GitHub prerelease). The tag is embedded in the binary, so `vario version` reports it along with the commit.

Before publishing, the workflow checks that the tag is valid SemVer and points to a commit in `master`'s history, then runs vet and tests and builds all archives and `checksums.txt` from a clean module cache. A separate job, the only one with write access, uploads them to a draft release with generated release notes, checks the upload, and then publishes the release. If a release or draft for the tag already exists, the workflow stops without changing it; to publish again, delete the release and re-run the workflow.

To release, merge the changes into `master`, then tag the merged commit and push the tag:

```bash
git switch master && git pull --ff-only
git tag -a v0.1.0 -m "Vario v0.1.0"
git push origin v0.1.0
```

Builds are reproducible: given the same commit and Go version, `make dist VERSION=v0.1.0` produces the same binaries (the embedded build date is the commit time).
