#!/bin/sh
# Vario installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh -s -- --version v0.1.0-beta.1
#
# Downloads a release archive (from GitHub Releases by default; see "Download
# source" below), verifies its SHA-256 checksum, and installs the `vario`
# executable into a user-writable directory (default ~/.local/bin). It needs no administrator
# rights, sends no telemetry, never edits shell startup files, never touches
# Vario's configuration or data (~/.vario), and never installs or starts the
# background service.
#
# Options (each also settable through the environment variable in brackets):
#   --version VERSION   install this version, e.g. v0.1.0-beta.1   [VARIO_VERSION]
#   --channel CHANNEL   release channel when no version is given:
#                       beta (default) or stable                   [VARIO_CHANNEL]
#   --install-dir DIR   where to put vario (default ~/.local/bin)  [VARIO_INSTALL_DIR]
#   --source SOURCE     github, or the https:// URL of a download
#                       mirror (releases/<tag>/, channels/)        [VARIO_DOWNLOAD_SOURCE]
#   --force             reinstall the same version, downgrade, or replace a symlink
#   -h, --help          show this help
#
# Requirements: curl (or GNU wget), tar, gzip, and sha256sum, shasum or openssl.

set -eu

# --- Download source ----------------------------------------------------------
# Every download URL is built in this file by resolve_channel and asset_url
# below, from these settings. Moving to Vario's own download domain means
# changing DEFAULT_SOURCE and the two commands (docs/distribution.md, "Moving to
# a custom domain"); the release workflow checks that both installers agree.
#
#   github        GitHub Releases of GITHUB_REPO. A channel's newest release is
#                 found through the GitHub REST API.
#   https://...   a static mirror (e.g. Cloudflare R2 behind a custom domain)
#                 serving releases/<tag>/<file> and channels/<channel>.txt.
DEFAULT_SOURCE='github'
GITHUB_REPO='nedanwr/vario'
INSTALL_SH_COMMAND='curl -fsSL https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh'
INSTALL_PS1_COMMAND='irm https://raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1 | iex'
# GitHub endpoints; overridable (https:// only) for tests and GitHub mirrors.
GITHUB_URL=${VARIO_GITHUB_URL:-https://github.com}
GITHUB_API_URL=${VARIO_GITHUB_API_URL:-https://api.github.com}

VERSION=${VARIO_VERSION:-}
CHANNEL=${VARIO_CHANNEL:-beta}
INSTALL_DIR=${VARIO_INSTALL_DIR:-}
SOURCE=${VARIO_DOWNLOAD_SOURCE:-}
FORCE=0
FOLLOW_REDIRECTS=0

tmp_dir=
staged=

say() { printf '%s\n' "$*"; }
warn() { printf 'vario-install: warning: %s\n' "$*" >&2; }
die() {
  printf 'vario-install: error: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [ -n "$staged" ] && [ -e "$staged" ]; then rm -f "$staged"; fi
  if [ -n "$tmp_dir" ] && [ -d "$tmp_dir" ]; then rm -rf "$tmp_dir"; fi
}
trap cleanup EXIT
trap 'cleanup; trap - EXIT; say "" >&2; die "interrupted; nothing was installed"' INT TERM HUP

usage() {
  cat <<'USAGE'
Usage: install.sh [options]

Installs the vario executable for this user.

  --version VERSION   install this version, e.g. v0.1.0-beta.1   [VARIO_VERSION]
  --channel CHANNEL   beta (default) or stable, when no version   [VARIO_CHANNEL]
  --install-dir DIR   default ~/.local/bin                        [VARIO_INSTALL_DIR]
  --source SOURCE     github (default) or an https:// mirror URL  [VARIO_DOWNLOAD_SOURCE]
  --force             reinstall the same version, downgrade, or replace a symlink
  -h, --help          show this help

USAGE
  printf 'Piped:  %s -s -- --version v0.1.0-beta.1\n' "$INSTALL_SH_COMMAND"
}

need_arg() {
  if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$1 needs a value"; fi
}

while [ "$#" -gt 0 ]; do
  case $1 in
    --version) need_arg "$@"; VERSION=$2; shift 2 ;;
    --version=*) VERSION=${1#*=}; shift ;;
    --channel) need_arg "$@"; CHANNEL=$2; shift 2 ;;
    --channel=*) CHANNEL=${1#*=}; shift ;;
    --install-dir) need_arg "$@"; INSTALL_DIR=$2; shift 2 ;;
    --install-dir=*) INSTALL_DIR=${1#*=}; shift ;;
    --source) need_arg "$@"; SOURCE=$2; shift 2 ;;
    --source=*) SOURCE=${1#*=}; shift ;;
    --force) FORCE=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

# --- Versions -----------------------------------------------------------------

# A release tag: vMAJOR.MINOR.PATCH with an optional prerelease, no build metadata.
is_release_tag() {
  printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

# semver_cmp A B prints -1, 0 or 1 (SemVer 2.0 precedence; leading "v" ignored).
semver_cmp() {
  awk -v a="${1#v}" -v b="${2#v}" '
    function isnum(s) { return s ~ /^[0-9]+$/ }
    function cmpnum(x, y) {
      # Compare digit strings without overflow: by length, then lexically.
      if (length(x) != length(y)) return length(x) < length(y) ? -1 : 1
      return x < y ? -1 : (x > y ? 1 : 0)
    }
    function cmpid(x, y) {
      if (isnum(x) && isnum(y)) return cmpnum(x, y)
      if (isnum(x)) return -1
      if (isnum(y)) return 1
      return x < y ? -1 : (x > y ? 1 : 0)
    }
    BEGIN {
      ap = index(a, "-"); bp = index(b, "-")
      acore = ap ? substr(a, 1, ap - 1) : a; apre = ap ? substr(a, ap + 1) : ""
      bcore = bp ? substr(b, 1, bp - 1) : b; bpre = bp ? substr(b, bp + 1) : ""
      split(acore, ac, "."); split(bcore, bc, ".")
      for (i = 1; i <= 3; i++) { c = cmpnum(ac[i], bc[i]); if (c) { print c; exit } }
      if (apre == "" && bpre == "") { print 0; exit }
      if (apre == "") { print 1; exit }
      if (bpre == "") { print -1; exit }
      an = split(apre, ai, "."); bn = split(bpre, bi, ".")
      for (i = 1; i <= an && i <= bn; i++) { c = cmpid(ai[i], bi[i]); if (c) { print c; exit } }
      print (an < bn ? -1 : (an > bn ? 1 : 0))
    }'
}

# --- Platform -----------------------------------------------------------------

detect_platform() {
  kernel=$(uname -s 2>/dev/null || echo unknown)
  machine=$(uname -m 2>/dev/null || echo unknown)
  case $kernel in
    Darwin) OS=darwin ;;
    Linux) OS=linux ;;
    MINGW* | MSYS* | CYGWIN* | Windows_NT)
      die "this is Windows; use the PowerShell installer instead: $INSTALL_PS1_COMMAND" ;;
    *) die "unsupported operating system: $kernel (Vario is built for macOS, Linux and Windows)" ;;
  esac
  case $machine in
    x86_64 | amd64) ARCH=amd64 ;;
    arm64 | aarch64 | armv8*) ARCH=arm64 ;;
    *) die "unsupported CPU architecture: $machine on $kernel (Vario is built for amd64 and arm64)" ;;
  esac
  # An x86_64 shell under Rosetta 2 on Apple silicon: install the native build.
  if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] &&
    [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
    ARCH=arm64
  fi
}

# --- Tools --------------------------------------------------------------------

pick_tools() {
  if command -v curl >/dev/null 2>&1; then
    DOWNLOADER=curl
  elif command -v wget >/dev/null 2>&1; then
    # BusyBox wget may not verify TLS certificates; only GNU wget is accepted.
    wget --help 2>&1 | grep -q -- '--https-only' || die "curl is required (this wget cannot enforce HTTPS certificate checks)"
    DOWNLOADER=wget
  else
    die "curl or wget is required"
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    HASHER=sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    HASHER=shasum
  elif command -v openssl >/dev/null 2>&1; then
    HASHER=openssl
  else
    die "sha256sum, shasum or openssl is required to verify the download"
  fi
  for tool in tar gzip awk mktemp; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
  done
}

sha256_of() {
  case $HASHER in
    sha256sum) sha256sum "$1" | awk '{print $1}' ;;
    shasum) shasum -a 256 "$1" | awk '{print $1}' ;;
    openssl) openssl dgst -sha256 -r "$1" | awk '{print $1}' ;;
  esac
}

# download URL FILE [ACCEPT]: HTTPS only; non-200 responses fail. Redirects
# are followed only for GitHub (whose release downloads redirect to its storage
# host), at most 5 and only to https:// URLs; a mirror must answer directly.
download() {
  case $DOWNLOADER in
    curl)
      if [ "$FOLLOW_REDIRECTS" = 1 ]; then
        code=$(curl_get "$1" "$2" "${3:-*/*}" --location --max-redirs 5 --proto-redir '=https') || return 1
      else
        code=$(curl_get "$1" "$2" "${3:-*/*}") || return 1
      fi
      [ "$code" = 200 ] || {
        printf 'vario-install: %s: HTTP %s\n' "$1" "$code" >&2
        return 1
      }
      ;;
    wget)
      wget --https-only --max-redirect="$((FOLLOW_REDIRECTS * 5))" --tries=3 --timeout=20 -q \
        --header="Accept: ${3:-*/*}" -O "$2" "$1" || return 1 ;;
  esac
}

curl_get() { # curl_get URL FILE ACCEPT [curl options...]; prints the HTTP status
  url=$1 out=$2 accept=$3
  shift 3
  curl "$@" --proto '=https' --tlsv1.2 --silent --show-error --fail \
    --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 900 \
    --header "Accept: $accept" --write-out '%{http_code}' --output "$out" "$url"
}

fetch() {
  download "$1" "$2" || die "download failed: $1 (check your connection and that the version exists; nothing was installed)"
  [ -s "$2" ] || die "empty response from $1; nothing was installed"
}

# --- Releases -----------------------------------------------------------------
# The only code that knows how a source lays out its files.

check_https_url() { # check_https_url NAME URL
  case $2 in
    https://*) ;;
    *) die "the $1 must be an https:// URL (got '$2')" ;;
  esac
  printf '%s\n' "$2" | grep -Eq '^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$' ||
    die "invalid $1: '$2'"
}

setup_source() {
  [ -n "$SOURCE" ] || SOURCE=$DEFAULT_SOURCE
  if [ "$SOURCE" = github ]; then
    GITHUB_URL=${GITHUB_URL%/}
    GITHUB_API_URL=${GITHUB_API_URL%/}
    check_https_url "GitHub URL" "$GITHUB_URL"
    check_https_url "GitHub API URL" "$GITHUB_API_URL"
    FOLLOW_REDIRECTS=1
  else
    SOURCE=${SOURCE%/}
    check_https_url "download source (github or an https:// URL)" "$SOURCE"
  fi
}

# in_channel CHANNEL TAG: stable releases are on both channels; beta and rc
# prereleases (v1.2.0-beta.3, v1.2.0-rc.1) only on beta; other prereleases
# (alpha, ...) on neither. scripts/release/r2-publish.sh follows the same rule.
in_channel() {
  case $2 in *-*) ;; *) return 0 ;; esac
  [ "$1" = beta ] || return 1
  case ${2#*-} in beta | beta.* | rc | rc.*) return 0 ;; esac
  return 1
}

# resolve_channel CHANNEL: prints the newest release tag on CHANNEL.
resolve_channel() {
  if [ "$SOURCE" = github ]; then
    # Not /releases/latest: it skips prereleases, and every beta is one.
    # Anonymous requests list only published releases, and the release workflow
    # publishes a release only once all of its assets are uploaded.
    api="$GITHUB_API_URL/repos/$GITHUB_REPO/releases?per_page=100"
    download "$api" "$tmp_dir/releases.json" application/vnd.github+json ||
      die "could not list Vario releases from GitHub ($api). GitHub allows 60 anonymous API requests an hour per address; try again later, or pass --version (e.g. --version v0.1.0-beta.1), which needs no API request"
    [ "$(tr -d ' \t\r\n' <"$tmp_dir/releases.json" | head -c 1)" = '[' ] ||
      die "unexpected response from $api; nothing was installed"
    # Every unescaped "tag_name" key is a release's own (quotes inside release
    # notes are escaped); invalid tags are skipped.
    newest=
    set -f
    # shellcheck disable=SC2020 # each of , { } becomes a newline
    for tag in $(tr ',{}' '\n\n\n' <"$tmp_dir/releases.json" |
      sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"\\]*\)"[[:space:]]*$/\1/p'); do
      if ! is_release_tag "$tag" || ! in_channel "$1" "$tag"; then continue; fi
      if [ -z "$newest" ] || [ "$(semver_cmp "$tag" "$newest")" = 1 ]; then newest=$tag; fi
    done
    set +f
    [ -n "$newest" ] || die "no $1 release of Vario is published on GitHub yet; pass --version to choose one"
    printf '%s\n' "$newest"
  else
    url=$SOURCE/channels/$1.txt
    fetch "$url" "$tmp_dir/channel"
    [ "$(wc -c <"$tmp_dir/channel")" -le 64 ] || die "malformed channel metadata at $url"
    tag=$(tr -d '\r' <"$tmp_dir/channel" | awk 'NF { n++; v = $0 } END { if (n == 1) print v }' | tr -d ' \t')
    is_release_tag "$tag" || die "malformed channel metadata at $url"
    printf '%s\n' "$tag"
  fi
}

# asset_url FILE: URL of FILE in release $VERSION.
asset_url() {
  if [ "$SOURCE" = github ]; then
    printf '%s\n' "$GITHUB_URL/$GITHUB_REPO/releases/download/$VERSION/$1"
  else
    printf '%s\n' "$SOURCE/releases/$VERSION/$1"
  fi
}

# --- Main ---------------------------------------------------------------------

[ "$(id -u 2>/dev/null || echo 1)" != 0 ] || warn "running as root is not needed; Vario installs per user"

detect_platform
pick_tools

setup_source

case $CHANNEL in
  beta | stable) ;;
  *) die "unknown channel '$CHANNEL' (expected beta or stable)" ;;
esac

[ -n "$INSTALL_DIR" ] || {
  [ -n "${HOME:-}" ] || die "HOME is not set; pass --install-dir"
  INSTALL_DIR=$HOME/.local/bin
}
case $INSTALL_DIR in
  /*) ;;
  *) INSTALL_DIR=$(pwd)/$INSTALL_DIR ;;
esac
INSTALL_DIR=${INSTALL_DIR%/}

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t vario-install)
[ -d "$tmp_dir" ] || die "cannot create a temporary directory"

if [ -n "$VERSION" ]; then
  case $VERSION in v*) ;; *) VERSION=v$VERSION ;; esac
  is_release_tag "$VERSION" || die "invalid version '$VERSION' (expected e.g. v0.1.0-beta.1)"
  say "Installing Vario $VERSION for $OS/$ARCH"
else
  VERSION=$(resolve_channel "$CHANNEL")
  say "Installing Vario $VERSION ($CHANNEL channel) for $OS/$ARCH"
fi

name_version=${VERSION#v}
base=vario_${name_version}_${OS}_${ARCH}
archive=$base.tar.gz

# Check the existing installation before downloading anything.
target=$INSTALL_DIR/vario
current=
if [ -L "$target" ] && [ "$FORCE" != 1 ]; then
  die "$target is a symbolic link (installed by a package manager?); remove it, choose another --install-dir, or pass --force to replace the link"
fi
if [ -e "$target" ] && [ ! -L "$target" ] && [ ! -f "$target" ]; then
  die "$target exists and is not a regular file"
fi
if [ -e "$target" ]; then
  current=$("$target" version --short 2>/dev/null | head -n 1 | tr -d '\r') || current=
  if is_release_tag "$current"; then
    c=$(semver_cmp "$current" "$VERSION")
    if [ "$c" = 0 ] && [ "$FORCE" != 1 ]; then
      say "Vario $current is already installed at $target. Nothing to do (use --force to reinstall)."
      exit 0
    fi
    if [ "$c" = 1 ] && [ "$FORCE" != 1 ]; then
      die "$target is Vario $current, newer than $VERSION. Downgrades are not supported (local state may not be readable by older versions); pass --force to install anyway"
    fi
  else
    warn "$target exists but does not report a release version (${current:-no output}); it will be replaced"
    current=
  fi
fi

say "Downloading $(asset_url "$archive")"
fetch "$(asset_url checksums.txt)" "$tmp_dir/checksums.txt"
expected=$(tr -d '\r' <"$tmp_dir/checksums.txt" | awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }')
case $expected in
  *[!0-9a-f]* | "") die "checksums.txt has no valid entry for $archive; refusing to install" ;;
esac
[ "${#expected}" = 64 ] || die "checksums.txt has no valid entry for $archive; refusing to install"

fetch "$(asset_url "$archive")" "$tmp_dir/$archive"
actual=$(sha256_of "$tmp_dir/$archive")
if [ "$actual" != "$expected" ]; then
  die "checksum mismatch for $archive (expected $expected, got $actual); the download is incomplete or corrupted. Nothing was installed"
fi
say "Verified SHA-256 checksum"

mkdir "$tmp_dir/x"
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir/x" "$base/vario" 2>/dev/null ||
  die "could not extract vario from $archive"
new=$tmp_dir/x/$base/vario
if [ ! -f "$new" ] || [ -L "$new" ]; then die "$archive does not contain $base/vario"; fi

if [ ! -d "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR" 2>/dev/null || die "cannot create $INSTALL_DIR; choose a writable directory with --install-dir"
  say "Created $INSTALL_DIR"
fi
[ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable; choose a writable directory with --install-dir (administrator rights are not needed)"

# Stage next to the target so the final rename is atomic, and check that the
# new binary runs and reports the expected version before replacing anything.
staged=$INSTALL_DIR/.vario.install.$$
cp "$new" "$staged"
chmod 0755 "$staged"
reported=$("$staged" version --short 2>/dev/null | head -n 1 | tr -d '\r') || reported=
if [ "$reported" != "$VERSION" ]; then
  die "the downloaded binary does not run on this system or reports '${reported:-nothing}' instead of $VERSION; nothing was replaced"
fi
mv -f "$staged" "$target"
staged=

say ""
if [ -n "$current" ]; then
  say "Upgraded Vario $current -> $VERSION at $target"
else
  say "Installed Vario $VERSION at $target"
fi

# --- Next steps ---------------------------------------------------------------

on_path=0
case ":${PATH:-}:" in *":$INSTALL_DIR:"*) on_path=1 ;; esac
resolved=$(command -v vario 2>/dev/null || true)

if [ "$on_path" = 0 ]; then
  say ""
  say "$INSTALL_DIR is not on your PATH. To add it, run:"
  case ${SHELL:-} in
    */zsh) say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc && exec zsh" ;;
    */bash)
      rc=~/.bashrc
      [ "$OS" = darwin ] && rc=~/.bash_profile
      say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> $rc && exec bash" ;;
    */fish) say "  fish_add_path '$INSTALL_DIR'" ;;
    *) say "  export PATH=\"$INSTALL_DIR:\$PATH\"   (and add this line to your shell's startup file)" ;;
  esac
  say "or run Vario as $target"
elif [ -n "$resolved" ] && [ "$resolved" != "$target" ]; then
  say ""
  warn "'vario' on your PATH resolves to $resolved, not $target. Remove the other copy or put $INSTALL_DIR earlier on your PATH."
fi

config_dir=${VARIO_HOME:-${HOME:-}/.vario}
say ""
if [ -f "$config_dir/config.toml" ]; then
  say "Your configuration in $config_dir was left unchanged. Next:"
  say "  vario doctor      # check the configuration; it says if 'vario migrate' is needed"
else
  say "Next:"
  say "  vario init        # guided setup: ClickHouse, collection mode, sources"
  say "  vario doctor      # check everything"
fi

plist=${HOME:-}/Library/LaunchAgents/io.github.nedanwr.vario.plist
if [ "$OS" = darwin ] && [ -f "$plist" ]; then
  program=$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist" 2>/dev/null || true)
  if [ "$program" = "$target" ]; then
    say "  vario service restart   # the background service still runs the previous binary"
  else
    say "  The background service runs ${program:-another executable}. To use this binary instead:"
    say "  $target service install"
  fi
fi
say ""
say "Documentation: https://github.com/nedanwr/vario#readme"
