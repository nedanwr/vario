#!/usr/bin/env bash
# Build Vario release archives and SHA-256 checksums.
#
# Usage: VERSION=v0.1.0 scripts/dist.sh        (normally via `make dist`)
#
# Environment:
#   VERSION    version embedded in the binaries and used in archive names (required)
#   COMMIT     commit embedded in the binaries (default: git rev-parse HEAD)
#   DIST_DIR   output directory; must not exist or be empty (default: dist)
#   PLATFORMS  space-separated GOOS/GOARCH pairs (default: the supported targets)
#
# Output in DIST_DIR:
#   vario_<version>_<os>_<arch>.tar.gz   (macOS, Linux)
#   vario_<version>_windows_<arch>.zip   (Windows)
#   checksums.txt                        (sha256sum format)
#
# Each archive holds one directory, vario_<version>_<os>_<arch>/, containing only
# the executable, LICENSE, README.md and THIRD_PARTY_LICENSES.txt.
#
# Builds are reproducible: -trimpath, no CGO, and the build date and archive
# timestamps are the commit time, so the same commit and Go version produce the
# same binaries (archive bytes additionally depend on the tar/gzip/zip versions).
set -euo pipefail

: "${VERSION:?VERSION is required, e.g. VERSION=v0.1.0}"
COMMIT=${COMMIT:-$(git rev-parse HEAD)}
DIST_DIR=${DIST_DIR:-dist}
PLATFORMS=${PLATFORMS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64}
PKG=github.com/nedanwr/vario/internal/buildinfo

# File names use the version without the leading "v" (v0.1.0 -> 0.1.0).
name_version=${VERSION#v}
case $name_version in
  "" | *[!0-9A-Za-z.+-]*) echo "dist: unsafe VERSION for file names: $VERSION" >&2; exit 1 ;;
esac

if [ -e "$DIST_DIR" ] && [ -n "$(ls -A "$DIST_DIR")" ]; then
  echo "dist: $DIST_DIR is not empty; remove it first" >&2
  exit 1
fi

root=$(pwd)
for f in go.mod LICENSE README.md cmd/vario; do
  [ -e "$root/$f" ] || { echo "dist: run from the repository root ($f missing)" >&2; exit 1; }
done

# Commit time in UTC, for the embedded build date and archive timestamps.
build_date=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ "$COMMIT")
touch_stamp=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y%m%d%H%M.%S "$COMMIT")
ldflags="-s -w -X $PKG.Version=$VERSION -X $PKG.Commit=$COMMIT -X $PKG.Date=$build_date"

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
else
  sha256() { shasum -a 256 "$@"; }
fi

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  tar_owner=(--owner=0 --group=0 --numeric-owner)
else
  tar_owner=(--uid 0 --gid 0)
fi
export COPYFILE_DISABLE=1 # no macOS ._ resource files in tar archives

mkdir -p "$DIST_DIR"
out=$(cd "$DIST_DIR" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

# Licenses of every third-party module linked into any of the targets.
notices="$stage/THIRD_PARTY_LICENSES.txt"
{
  echo "Vario includes the following third-party Go modules."
  echo "Their license texts are reproduced below."
  for platform in $PLATFORMS; do
    GOOS=${platform%/*} GOARCH=${platform#*/} CGO_ENABLED=0 \
      go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/vario
  done | sort -u | while read -r mod dir; do
    printf '\n================================================================================\n%s\n================================================================================\n' "$mod"
    found=
    for lic in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/PATENTS*; do
      [ -f "$lic" ] || continue
      printf '\n--- %s ---\n\n' "${lic##*/}"
      cat "$lic"
      found=1
    done
    [ -n "$found" ] || { echo "dist: no license file found for $mod" >&2; exit 1; }
  done
} >"$notices"

for platform in $PLATFORMS; do
  goos=${platform%/*}
  goarch=${platform#*/}
  base="vario_${name_version}_${goos}_${goarch}"
  exe=vario
  [ "$goos" = windows ] && exe=vario.exe

  dir="$stage/$base"
  mkdir "$dir"
  echo "dist: building $goos/$goarch"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -trimpath -ldflags "$ldflags" -o "$dir/$exe" ./cmd/vario
  cp "$root/LICENSE" "$root/README.md" "$notices" "$dir/"
  chmod 0755 "$dir" "$dir/$exe"
  chmod 0644 "$dir/LICENSE" "$dir/README.md" "$dir/THIRD_PARTY_LICENSES.txt"
  TZ=UTC touch -t "$touch_stamp" "$dir" "$dir"/*

  # Explicit, sorted member lists keep the archive layout deterministic.
  members=("$base/" "$base/$exe" "$base/LICENSE" "$base/README.md" "$base/THIRD_PARTY_LICENSES.txt")
  if [ "$goos" = windows ]; then
    (cd "$stage" && TZ=UTC zip -q -X -D "$out/$base.zip" "${members[@]:1}")
  else
    (cd "$stage" && tar --format=ustar --no-recursion "${tar_owner[@]}" -cf - "${members[@]}") | gzip -9 -n >"$out/$base.tar.gz"
  fi
done

(cd "$out" && sha256 vario_*.tar.gz vario_*.zip | LC_ALL=C sort -k2 >checksums.txt)
set -- "$out"/*
echo "dist: wrote $# files to $DIST_DIR"
