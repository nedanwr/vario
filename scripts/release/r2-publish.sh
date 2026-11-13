#!/usr/bin/env bash
# Publish a Vario release to Cloudflare R2 (S3 API), served from the download domain.
#
# Usage:
#   scripts/release/r2-publish.sh upload    # immutable release files, then verify them publicly
#   scripts/release/r2-publish.sh promote   # move channel pointers and root installers
#
# Run `upload` first and `promote` only after every other part of the release
# (including the GitHub Release) succeeded. Both steps are safe to re-run.
#
# Bucket layout:
#   releases/<tag>/vario_<version>_<os>_<arch>.tar.gz|zip   immutable
#   releases/<tag>/checksums.txt                            immutable
#   releases/<tag>/install.sh, install.ps1                  immutable (the installers as of <tag>)
#   channels/beta.txt, channels/stable.txt                  "<tag>\n", only ever moved forward
#   install.sh, install.ps1                                 installers of the newest channel release
#
# Environment:
#   TAG                    release tag, e.g. v0.1.0-beta.1 (required)
#   DIST_DIR               archives and checksums.txt from scripts/dist.sh (default dist)
#   INSTALLERS_DIR         directory with install.sh and install.ps1 (default scripts)
#   DOWNLOAD_BASE_URL      public https:// base URL of the bucket (required)
#   R2_BUCKET              bucket name (required)
#   R2_ACCOUNT_ID          Cloudflare account id; sets the default endpoint
#   R2_ENDPOINT            S3 endpoint (default https://<R2_ACCOUNT_ID>.r2.cloudflarestorage.com)
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY   R2 API token (Object Read & Write, this bucket only)
#   PUBLIC_VERIFY_ATTEMPTS how often to retry a public download (default 6)
#
# Never run with `set -x`: the AWS CLI reads the credentials from the environment
# and this script never prints them.
set -euo pipefail

die() {
  echo "::error::$*" >&2
  exit 1
}
note() { echo "$*"; }

cmd=${1:-}
case $cmd in upload | promote) ;; *) die "usage: $0 upload|promote" ;; esac

: "${TAG:?TAG is required}"
: "${DOWNLOAD_BASE_URL:?DOWNLOAD_BASE_URL is required}"
: "${R2_BUCKET:?R2_BUCKET is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required (R2 access key id)}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required (R2 secret access key)}"
DIST_DIR=${DIST_DIR:-dist}
INSTALLERS_DIR=${INSTALLERS_DIR:-scripts}
PUBLIC_VERIFY_ATTEMPTS=${PUBLIC_VERIFY_ATTEMPTS:-6}
if [ -z "${R2_ENDPOINT:-}" ]; then
  : "${R2_ACCOUNT_ID:?R2_ACCOUNT_ID or R2_ENDPOINT is required}"
  [[ $R2_ACCOUNT_ID =~ ^[0-9a-f]{32}$ ]] || die "R2_ACCOUNT_ID is not a Cloudflare account id"
  R2_ENDPOINT=https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com
fi
DOWNLOAD_BASE_URL=${DOWNLOAD_BASE_URL%/}
[[ $DOWNLOAD_BASE_URL =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "DOWNLOAD_BASE_URL must be https://<host>"

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'
[[ $TAG =~ $semver ]] || die "'$TAG' is not a release tag"
version=${TAG#v}

# R2 speaks the S3 API in region "auto". Newer AWS CLIs add CRC checksums to
# every request by default; send them only where the API requires them.
export AWS_DEFAULT_REGION=auto
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
export AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
export AWS_EC2_METADATA_DISABLED=true
export AWS_PAGER=""

s3api() { aws --endpoint-url "$R2_ENDPOINT" --output json s3api "$@"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sha256() { sha256sum "$1" | awk '{print $1}'; }

# semver_cmp A B -> -1, 0, 1 (SemVer 2.0 precedence). Same algorithm as install.sh.
semver_cmp() {
  awk -v a="${1#v}" -v b="${2#v}" '
    function isnum(s) { return s ~ /^[0-9]+$/ }
    function cmpnum(x, y) {
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

content_type() {
  case $1 in
    *.tar.gz) echo application/gzip ;;
    *.zip) echo application/zip ;;
    *) echo "text/plain; charset=utf-8" ;;
  esac
}

# The release files that must exist locally, in a fixed order.
release_files() {
  local os arch
  for os in darwin linux windows; do
    for arch in amd64 arm64; do
      if [ "$os" = windows ]; then echo "vario_${version}_${os}_${arch}.zip"; else echo "vario_${version}_${os}_${arch}.tar.gz"; fi
    done
  done
  echo checksums.txt
}

check_local_files() {
  local f
  for f in $(release_files); do
    [ -f "$DIST_DIR/$f" ] || die "missing release file $DIST_DIR/$f"
  done
  for f in install.sh install.ps1; do
    [ -f "$INSTALLERS_DIR/$f" ] || die "missing installer $INSTALLERS_DIR/$f"
  done
  [ "$(wc -l <"$DIST_DIR/checksums.txt")" -eq 6 ] || die "checksums.txt must list exactly the six archives"
  (cd "$DIST_DIR" && sha256sum --check --strict --quiet checksums.txt) || die "local checksums do not match"
  # Installers published here must download from here (DEFAULT_SOURCE), not
  # from GitHub; see docs/distribution.md, "Moving to a custom domain".
  grep -q "^DEFAULT_SOURCE='$DOWNLOAD_BASE_URL'\$" "$INSTALLERS_DIR/install.sh" ||
    die "install.sh does not default to $DOWNLOAD_BASE_URL (DEFAULT_SOURCE)"
  grep -q "^\$DefaultSource = '$DOWNLOAD_BASE_URL'\$" "$INSTALLERS_DIR/install.ps1" ||
    die "install.ps1 does not default to $DOWNLOAD_BASE_URL (\$DefaultSource)"
}

# head_object KEY -> 0 if it exists, 1 if not; anything else is fatal.
head_object() {
  local err=$work/head.err
  if s3api head-object --bucket "$R2_BUCKET" --key "$1" >/dev/null 2>"$err"; then return 0; fi
  if grep -Eq '\((404|NoSuchKey|NotFound)\)|Not Found' "$err"; then return 1; fi
  sed 's/^/  /' "$err" >&2
  die "cannot check s3://$R2_BUCKET/$1"
}

# get_object KEY FILE -> 0, or 1 if the object does not exist.
get_object() {
  local err=$work/get.err
  if s3api get-object --bucket "$R2_BUCKET" --key "$1" "$2" >/dev/null 2>"$err"; then return 0; fi
  if grep -Eq '\((404|NoSuchKey|NotFound)\)|Not Found|does not exist' "$err"; then return 1; fi
  sed 's/^/  /' "$err" >&2
  die "cannot read s3://$R2_BUCKET/$1"
}

# put_object FILE KEY CACHE_CONTROL [create-only]
put_object() {
  local file=$1 key=$2 cache=$3 mode=${4:-} md5 extra=()
  md5=$(openssl dgst -md5 -binary "$file" | base64)
  [ "$mode" = create-only ] && extra=(--if-none-match '*')
  s3api put-object --bucket "$R2_BUCKET" --key "$key" --body "$file" \
    --content-type "$(content_type "$key")" --cache-control "$cache" \
    --content-md5 "$md5" --metadata "sha256=$(sha256 "$file")" ${extra[@]+"${extra[@]}"} >/dev/null
}

# Upload a file that must never change once published. Re-runs are fine when
# the published bytes are identical; different bytes are an error.
put_immutable() {
  local file=$1 key=$2
  if head_object "$key"; then
    get_object "$key" "$work/existing" || die "s3://$R2_BUCKET/$key disappeared while checking it"
    if [ "$(sha256 "$work/existing")" = "$(sha256 "$file")" ]; then
      note "  unchanged  $key (already published)"
      return
    fi
    die "$key is already published with different content. Published releases are immutable; nothing was overwritten. Investigate (non-reproducible rebuild?) and release a new version instead."
  fi
  if ! put_object "$file" "$key" "public, max-age=31536000, immutable" create-only 2>"$work/put.err"; then
    if grep -q 'PreconditionFailed\|(412)' "$work/put.err"; then
      die "$key was created concurrently by someone else; re-run to compare it"
    fi
    sed 's/^/  /' "$work/put.err" >&2
    die "upload of $key failed"
  fi
  note "  uploaded   $key"
}

# Download URL over HTTPS exactly as users do (no redirects) and compare its hash.
verify_public() {
  local url=$1 want=$2 i code
  for ((i = 1; i <= PUBLIC_VERIFY_ATTEMPTS; i++)); do
    code=$(curl --proto '=https' --tlsv1.2 --silent --show-error --max-time 300 \
      --write-out '%{http_code}' --output "$work/public" "$url" 2>"$work/curl.err") || code=error
    if [ "$code" = 200 ] && [ "$(sha256 "$work/public")" = "$want" ]; then
      note "  verified   $url"
      return 0
    fi
    note "  attempt $i/$PUBLIC_VERIFY_ATTEMPTS: $url -> ${code} $(head -c 200 "$work/curl.err")"
    sleep $((i * 5))
  done
  return 1
}

verify_release_public() {
  local f
  note "Verifying the release through $DOWNLOAD_BASE_URL"
  for f in $(release_files); do
    verify_public "$DOWNLOAD_BASE_URL/releases/$TAG/$f" "$(sha256 "$DIST_DIR/$f")" ||
      die "$DOWNLOAD_BASE_URL/releases/$TAG/$f is not served correctly (bucket public access, custom domain, cache rules?)"
  done
  for f in install.sh install.ps1; do
    verify_public "$DOWNLOAD_BASE_URL/releases/$TAG/$f" "$(sha256 "$INSTALLERS_DIR/$f")" ||
      die "$DOWNLOAD_BASE_URL/releases/$TAG/$f is not served correctly"
  done
}

# Channels a tag moves: a stable release moves stable and beta (so the beta
# channel never falls behind stable), -beta.N and -rc.N prereleases move beta,
# any other prerelease (e.g. -alpha.1) is published but moves no channel.
channels_for_tag() {
  case $version in
    *-*)
      case ${version#*-} in
        beta | beta.* | rc | rc.*) echo beta ;;
      esac
      ;;
    *) echo stable beta ;;
  esac
}

# read_channel NAME -> prints the tag, or nothing if the channel does not exist yet.
read_channel() {
  local file=$work/channel-$1 v
  get_object "channels/$1.txt" "$file" || return 0
  v=$(tr -d '\r' <"$file" | awk 'NF' | head -n 2)
  [[ $v =~ $semver ]] || die "channels/$1.txt holds '$v', which is not a release tag; fix it by hand before promoting"
  echo "$v"
}

do_upload() {
  check_local_files
  note "Uploading $TAG to s3://$R2_BUCKET/releases/$TAG/"
  local f
  # Archives first, then the checksums that make them installable, then the installers.
  for f in $(release_files); do
    put_immutable "$DIST_DIR/$f" "releases/$TAG/$f"
  done
  for f in install.sh install.ps1; do
    put_immutable "$INSTALLERS_DIR/$f" "releases/$TAG/$f"
  done
  verify_release_public
  note "Release files for $TAG are published and verified. Channels were not changed."
}

do_promote() {
  check_local_files
  # Never point a channel at files that are not fully served.
  verify_release_public

  local channels ch cur newest="" c
  channels=$(channels_for_tag)
  if [ -z "$channels" ]; then
    note "$TAG is not a beta, rc or stable release; no channel or root installer was changed."
    return
  fi

  # The root installers follow the newest release on any channel.
  for ch in stable beta; do
    cur=$(read_channel "$ch")
    if [ -n "$cur" ] && { [ -z "$newest" ] || [ "$(semver_cmp "$cur" "$newest")" = 1 ]; }; then newest=$cur; fi
  done
  if [ -z "$newest" ] || [ "$(semver_cmp "$TAG" "$newest")" != -1 ]; then
    for f in install.sh install.ps1; do
      put_object "$INSTALLERS_DIR/$f" "$f" "public, max-age=300"
      note "  updated    $f"
    done
  else
    note "  kept       install.sh, install.ps1 (a newer release, $newest, is already published)"
  fi

  for ch in $channels; do
    cur=$(read_channel "$ch")
    if [ -n "$cur" ]; then c=$(semver_cmp "$TAG" "$cur"); else c=1; fi
    case $c in
      0) note "  unchanged  channels/$ch.txt (already $TAG)" ;;
      -1) note "  kept       channels/$ch.txt at $cur (newer than $TAG)" ;;
      1)
        printf '%s\n' "$TAG" >"$work/pointer"
        put_object "$work/pointer" "channels/$ch.txt" "public, max-age=60"
        get_object "channels/$ch.txt" "$work/readback" || die "channels/$ch.txt vanished after writing it"
        cmp -s "$work/pointer" "$work/readback" || die "channels/$ch.txt does not read back as $TAG"
        note "  moved      channels/$ch.txt ${cur:-(new)} -> $TAG"
        ;;
    esac
  done
  note "Promotion of $TAG complete. Public copies of channel files may be cached for up to 60 seconds."
}

aws --version >/dev/null 2>&1 || die "the AWS CLI is required"
"do_$cmd"
