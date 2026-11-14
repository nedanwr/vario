#!/usr/bin/env bash
# Tests scripts/install.sh (and scripts/install.ps1 when pwsh is available)
# against synthetic release archives served over local HTTPS, through both
# download sources: a fake GitHub (REST API release list, and release
# downloads that redirect to an asset host, like the real ones) and a static
# mirror (releases/<tag>/, channels/<channel>.txt).
#
# Usage: scripts/release/test-installers.sh        (from the repository root)
#
# Needs python3, openssl, curl, tar, gzip, zip. Nothing outside a temporary
# directory is touched: HOME, VARIO_HOME and the install directories all point
# into it, and uname/sysctl are replaced by fakes to simulate other platforms.
# The archives contain shell scripts posing as vario, not real binaries. No
# request leaves the machine.
#
# Environment:
#   SKIP_PWSH=1   skip the PowerShell tests even if pwsh is installed
# Checks are written as `test && ok ... || bad ...`; ok and bad always succeed.
# shellcheck disable=SC2015,SC2001,SC2016 # SC2016: '$Name' patterns are literal
set -euo pipefail

root=$(pwd)
[ -f "$root/scripts/install.sh" ] || { echo "run from the repository root" >&2; exit 1; }
for tool in python3 openssl curl tar gzip zip; do
  command -v "$tool" >/dev/null || { echo "test-installers: $tool is required" >&2; exit 1; }
done

t=$(mktemp -d)
server_pid=
cleanup() {
  if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  rm -rf "$t"
}
trap cleanup EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   - $*"; }
bad() { fail=$((fail + 1)); echo "FAIL - $*"; }

if command -v sha256sum >/dev/null; then sha() { sha256sum "$@"; }; else sha() { shasum -a 256 "$@"; }; fi

# --- Version ordering ---------------------------------------------------------
# install.sh, install.ps1 and r2-publish.sh must agree on SemVer precedence:
# it decides downgrades, a channel's newest release, and whether a channel moves.

ordered="v0.1.0-alpha v0.1.0-alpha.1 v0.1.0-alpha.beta v0.1.0-beta v0.1.0-beta.1 v0.1.0-beta.2 v0.1.0-beta.10
v0.1.0-rc.1 v0.1.0 v0.1.1 v0.9.0 v0.10.0 v1.0.0-rc.1 v1.0.0 v10.0.0 v18446744073709551616.0.0"
# shellcheck disable=SC2086,SC2116 # join the lines into one word list
ordered=$(echo $ordered)
semver_check() { # semver_check NAME FILE: FILE defines semver_cmp
  local name=$1 file=$2 errs=0 i=0 j a b want got
  for a in $ordered; do
    i=$((i + 1)); j=0
    for b in $ordered; do
      j=$((j + 1))
      if [ $i -lt $j ]; then want=-1; elif [ $i -gt $j ]; then want=1; else want=0; fi
      got=$(sh -c ". '$file'; semver_cmp $a $b")
      [ "$got" = "$want" ] || { errs=$((errs + 1)); echo "    $name: $a vs $b = $got, want $want"; }
    done
  done
  [ $errs = 0 ] && ok "$name: SemVer precedence" || bad "$name: SemVer precedence ($errs wrong)"
}
for src in scripts/install.sh scripts/release/r2-publish.sh; do
  sed -n '/^semver_cmp() {/,/^}/p' "$root/$src" >"$t/semver.sh"
  semver_check "$src" "$t/semver.sh"
done

# --- Channel membership -------------------------------------------------------
# install.sh (GitHub source), install.ps1 (below) and r2-publish.sh (mirror
# channel files) must agree on which tags belong to which channel.

channel_tags="v1.0.0 v1.0.0-beta v1.0.0-beta.2 v1.0.0-rc v1.0.0-rc.1 v1.0.0-alpha.1 v1.0.0-x-beta v1.0.0-betax v1.0.0-rc1 v1.0.0-beta-2"
want_channels="v1.0.0=stable,beta v1.0.0-beta=beta v1.0.0-beta.2=beta v1.0.0-rc=beta v1.0.0-rc.1=beta v1.0.0-alpha.1= v1.0.0-x-beta= v1.0.0-betax= v1.0.0-rc1= v1.0.0-beta-2="
sed -n '/^in_channel() {/,/^}/p' "$root/scripts/install.sh" >"$t/channel.sh"
sed -n '/^channels_for_tag() {/,/^}/p' "$root/scripts/release/r2-publish.sh" >"$t/channel-r2.sh"
got_sh=
got_r2=
for tag in $channel_tags; do
  chans=$(sh -c ". '$t/channel.sh'; for c in stable beta; do in_channel \$c $tag && printf '%s,' \$c; done; true")
  got_sh="$got_sh $tag=${chans%,}"
  chans=$(bash -c ". '$t/channel-r2.sh'; version=$tag; channels_for_tag" | tr ' ' ',')
  got_r2="$got_r2 $tag=$chans"
done
[ "${got_sh# }" = "$want_channels" ] && ok "install.sh: channel membership" || bad "install.sh: channel membership:$got_sh"
[ "${got_r2# }" = "$want_channels" ] && ok "r2-publish.sh: channel membership" || bad "r2-publish.sh: channel membership:$got_r2"

# --- Defaults -----------------------------------------------------------------
# Both installers must default to the same source and repository.

default_of() { sed -n "s/^$2 *= *'\\(.*\\)'\$/\\1/p" "$root/scripts/$1"; }
[ "$(default_of install.sh DEFAULT_SOURCE)" = github ] && [ "$(default_of install.ps1 '\$DefaultSource')" = github ] &&
  ok "installers default to the github source" ||
  bad "installers' default source: '$(default_of install.sh DEFAULT_SOURCE)' / '$(default_of install.ps1 '\$DefaultSource')'"
[ "$(default_of install.sh GITHUB_REPO)" = nedanwr/vario ] && [ "$(default_of install.ps1 '\$GitHubRepo')" = nedanwr/vario ] &&
  ok "installers name the nedanwr/vario repository" || bad "installers' GitHub repository"

# --- Synthetic releases -------------------------------------------------------
# Release files live in $srv/<site>/releases/<tag>/; a site is either a static
# mirror (with channels/) or the asset host behind a fake GitHub site.

srv=$t/srv
make_release() { # make_release SITE TAG [reported-version]
  local site=$1 tag=$2 reported=${3:-$2} v=${2#v} os arch base dir exe
  local rel=$srv/$site/releases/$tag stage=$t/stage-$site-$tag
  mkdir -p "$rel" "$stage"
  for os in darwin linux windows; do
    for arch in amd64 arm64; do
      base=vario_${v}_${os}_${arch}
      dir=$stage/$base
      mkdir -p "$dir"
      exe=vario
      [ "$os" = windows ] && exe=vario.exe
      cat >"$dir/$exe" <<EOF
#!/bin/sh
# platform: ${os}/${arch}
case "\$1 \${2:-}" in
  "version --short") echo "$reported" ;;
  "version "*) echo "vario $reported (synthetic, ${os}/${arch})" ;;
  *) echo "synthetic vario" ;;
esac
EOF
      chmod 0755 "$dir/$exe"
      echo license >"$dir/LICENSE"
      if [ "$os" = windows ]; then
        (cd "$stage" && zip -q -X "$rel/$base.zip" "$base/$exe" "$base/LICENSE")
      else
        (cd "$stage" && tar -cf - "$base" | gzip -n >"$rel/$base.tar.gz")
      fi
    done
  done
  (cd "$rel" && sha vario_* | LC_ALL=C sort -k2 >checksums.txt)
}

# "good": several releases; as a mirror, channels pointing at two of them.
make_release good v0.1.0-beta.1
make_release good v0.1.0-beta.2
make_release good v0.1.0-rc.1
make_release good v0.2.0
mkdir -p "$srv/good/channels"
echo v0.1.0-beta.2 >"$srv/good/channels/beta.txt"
echo v0.2.0 >"$srv/good/channels/stable.txt"

# "corrupt": archives that do not match checksums.txt.
make_release corrupt v0.1.0-beta.2
mkdir -p "$srv/corrupt/channels"
echo v0.1.0-beta.2 >"$srv/corrupt/channels/beta.txt"
printf 'garbage' >>"$srv/corrupt/releases/v0.1.0-beta.2/vario_0.1.0-beta.2_linux_amd64.tar.gz"
printf 'garbage' >>"$srv/corrupt/releases/v0.1.0-beta.2/vario_0.1.0-beta.2_windows_amd64.zip"

# "nosum": checksums.txt without the linux/amd64 and windows/amd64 entries; also malformed channels.
make_release nosum v0.1.0-beta.2
grep -v '_amd64\.' "$srv/nosum/releases/v0.1.0-beta.2/checksums.txt" >"$t/sums" || true
mv "$t/sums" "$srv/nosum/releases/v0.1.0-beta.2/checksums.txt"
mkdir -p "$srv/nosum/channels"
printf 'v0.1.0-beta.2\nv0.1.0-beta.1\n' >"$srv/nosum/channels/beta.txt"
printf '<html>not found</html>\n' >"$srv/nosum/channels/stable.txt"

# "wrongbin": the binary reports another version than its archive name.
make_release wrongbin v0.1.0-beta.2 v9.9.9
mkdir -p "$srv/wrongbin/channels"
echo v0.1.0-beta.2 >"$srv/wrongbin/channels/beta.txt"

# GitHub release lists (GET /repos/nedanwr/vario/releases), one per fake GitHub
# site. Drafts are never listed to anonymous clients, so none appear here.
mkdir -p "$srv/api"
cat >"$t/releases.py" <<'PY'
import json, sys
out, style, tags = sys.argv[1], sys.argv[2], sys.argv[3:]
rel = [{
    "url": "https://api.github.com/repos/nedanwr/vario/releases/%d" % i,
    "id": i,
    "tag_name": tag,
    "name": "Vario " + tag,
    "draft": False,
    "prerelease": "-" in tag,
    "assets": [{"name": "checksums.txt", "uploader": {"login": "github-actions[bot]"}}],
    # Release notes quoting a tag_name must not be mistaken for a release.
    "body": 'Upgrade notes: {"tag_name": "v99.0.0", "x": 1}, "tag_name":"v98.0.0"',
} for i, tag in enumerate(tags)]
with open(out, "w") as f:
    if style == "pretty":
        json.dump(rel, f, indent=2)
    else:
        json.dump(rel, f, separators=(",", ":"))
PY
releases() { python3 "$t/releases.py" "$srv/api/$1.json" "${@:2}"; }
# Not in precedence order: the installers must not rely on GitHub's sorting.
releases good compact v0.3.0-alpha.1 nightly v0.1.0-beta.1 v0.1.0-beta.2
releases pretty pretty v0.1.0-beta.1 v0.3.0-alpha.1 v0.1.0-beta.2
releases mixed compact v0.1.0-beta.1 v0.2.0 v0.1.0-beta.2 v0.3.0-alpha.1
releases rc compact v0.1.0-beta.10 v0.1.0-rc.1 v0.1.0-beta.2
releases empty compact
releases alphaonly compact v0.3.0-alpha.1
for site in corrupt nosum wrongbin; do releases "$site" compact v0.1.0-beta.2; done
for site in truncate insecure loop; do cp "$srv/api/good.json" "$srv/api/$site.json"; done
printf '<!doctype html><html>Service unavailable</html>\n' >"$srv/api/html.json"

# --- HTTPS server -------------------------------------------------------------

openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=vario-test" \
  -addext "subjectAltName=DNS:localhost,DNS:host.docker.internal,IP:127.0.0.1" \
  -keyout "$t/key.pem" -out "$t/cert.pem" >/dev/null 2>&1

cat >"$t/serve.py" <<'PY'
import http.server, os, ssl, sys
root, certdir, portfile = sys.argv[1], sys.argv[2], sys.argv[3]

# Fake GitHub sites whose release downloads come from another site's files.
ASSETS = {"pretty": "good", "mixed": "good", "rc": "good", "insecure": "good",
          "loop": "good", "ratelimit": "good", "html": "good"}

class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=root, **kw)
    def log_message(self, *a):
        pass
    def reply(self, code, body=b"", headers=()):
        self.send_response(code)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        parts = self.path.split("?")[0].split("/")
        # Fake GitHub REST API: /api/<site>/repos/nedanwr/vario/releases?per_page=100
        if parts[1] == "api":
            site = parts[2]
            if parts[3:] != ["repos", "nedanwr", "vario", "releases"] or "per_page=100" not in self.path:
                return self.reply(404, b'{"message":"Not Found"}')
            if "application/vnd.github+json" not in self.headers.get("Accept", ""):
                return self.reply(415, b'{"message":"unexpected Accept header"}')
            if site == "ratelimit":
                return self.reply(403, b'{"message":"API rate limit exceeded"}')
            p = os.path.join(root, "api", site + ".json")
            if not os.path.isfile(p):
                return self.reply(404, b'{"message":"Not Found"}')
            return self.reply(200, open(p, "rb").read(), [("Content-Type", "application/json")])
        # Fake github.com: /gh/<site>/nedanwr/vario/releases/download/<tag>/<file>
        # redirects to the asset host, as the real one does.
        if parts[1] == "gh":
            site, rest = parts[2], parts[3:]
            if len(rest) != 6 or rest[:4] != ["nedanwr", "vario", "releases", "download"]:
                return self.reply(404, b"Not Found")
            target = "/%s/releases/%s/%s" % (ASSETS.get(site, site), rest[4], rest[5])
            if site == "insecure":
                target = "http://127.0.0.1:%d%s" % (self.server.server_address[1], target)
            if site == "loop":
                target = self.path
            return self.reply(302, headers=[("Location", target)])
        # /redirect/...: a mirror that redirects (installers must refuse it).
        if self.path.startswith("/redirect/"):
            return self.reply(302, headers=[("Location", "/good/" + self.path[len("/redirect/"):])])
        # /truncate/...: the good file, cut off half-way (interrupted download).
        if self.path.startswith("/truncate/"):
            p = os.path.join(root, "good", self.path[len("/truncate/"):].lstrip("/"))
            if os.path.isfile(p):
                data = open(p, "rb").read()
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if p.endswith(".txt"):
                    self.wfile.write(data)
                else:
                    self.wfile.write(data[: len(data) // 2])
                    self.wfile.flush()
                    self.connection.shutdown(2)
                return
        super().do_GET()

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(os.path.join(certdir, "cert.pem"), os.path.join(certdir, "key.pem"))
httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
with open(portfile, "w") as f:
    f.write(str(httpd.server_address[1]))
httpd.serve_forever()
PY
python3 "$t/serve.py" "$srv" "$t" "$t/port" &
server_pid=$!
for _ in $(seq 50); do [ -s "$t/port" ] && break; sleep 0.1; done
port=$(cat "$t/port")
base=https://127.0.0.1:$port
export CURL_CA_BUNDLE=$t/cert.pem SSL_CERT_FILE=$t/cert.pem
curl -fsS "$base/good/channels/beta.txt" >/dev/null || { echo "test server not reachable" >&2; exit 1; }

# --- install.sh ---------------------------------------------------------------

fakebin=$t/fakebin
mkdir -p "$fakebin"
cat >"$fakebin/uname" <<'EOF'
#!/bin/sh
case $1 in -s) echo "$FAKE_UNAME_S" ;; -m) echo "$FAKE_UNAME_M" ;; *) echo "$FAKE_UNAME_S" ;; esac
EOF
cat >"$fakebin/sysctl" <<'EOF'
#!/bin/sh
[ "$*" = "-n sysctl.proc_translated" ] && [ -n "${FAKE_TRANSLATED:-}" ] && { echo "$FAKE_TRANSLATED"; exit 0; }
exit 1
EOF
chmod +x "$fakebin/uname" "$fakebin/sysctl"
syspath="$fakebin:/usr/bin:/bin:/usr/sbin:/sbin:$(dirname "$(command -v curl)")"

n=0
# run_sh OS MACHINE SITE [install.sh args...]: runs the installer in a fresh HOME
# unless $H is set; output in $out, exit status in $rc. The default (github)
# source is pointed at the fake GitHub SITE; pass --source "$base/SITE" to use
# the static mirror SITE instead. EXTRA_ENV=NAME=VALUE adds or overrides one
# environment variable.
run_sh() {
  local s=$1 m=$2 site=$3
  shift 3
  if [ -z "${H:-}" ]; then n=$((n + 1)); home=$t/home$n; mkdir -p "$home"; else home=$H; fi
  set +e
  out=$(env -i PATH="$syspath" HOME="$home" SHELL=/bin/zsh CURL_CA_BUNDLE="$CURL_CA_BUNDLE" \
    FAKE_UNAME_S="$s" FAKE_UNAME_M="$m" FAKE_TRANSLATED="${TRANSLATED:-}" \
    VARIO_GITHUB_URL="$base/gh/$site" VARIO_GITHUB_API_URL="$base/api/$site" ${EXTRA_ENV:+"$EXTRA_ENV"} \
    sh "$root/scripts/install.sh" "$@" 2>&1)
  rc=$?
  set -e
}
expect_ok() { if [ "$rc" = 0 ]; then ok "$1"; else bad "$1 (exit $rc)"; echo "$out" | sed 's/^/    /'; fi; }
expect_fail() { # expect_fail NAME PATTERN
  if [ "$rc" != 0 ] && echo "$out" | grep -q -- "$2"; then ok "$1"; else bad "$1 (exit $rc, want failure matching '$2')"; echo "$out" | sed 's/^/    /'; fi
}
expect_out() { # expect_out NAME PATTERN: succeeded and printed PATTERN
  if [ "$rc" = 0 ] && echo "$out" | grep -q -- "$2"; then ok "$1"; else bad "$1 (exit $rc, want output matching '$2')"; echo "$out" | sed 's/^/    /'; fi
}
platform_of() { sed -n 's/^# platform: //p' "$1" 2>/dev/null; }
no_leftovers() { # no temporary files in DIR
  if [ -z "$(find "$1" -name '.vario.install.*' 2>/dev/null)" ]; then ok "$2: no staged files left"; else bad "$2: staged files left in $1"; fi
}

# Platform selection, default install directory, beta channel from GitHub.
for case in "Linux x86_64 linux/amd64" "Linux aarch64 linux/arm64" "Darwin arm64 darwin/arm64" "Darwin x86_64 darwin/amd64"; do
  # shellcheck disable=SC2086 # split the case into its words
  set -- $case
  run_sh "$1" "$2" good
  expect_ok "install.sh: $1 $2 installs the newest beta from GitHub"
  bin=$home/.local/bin/vario
  if [ "$(platform_of "$bin")" = "$3" ]; then ok "install.sh: $1 $2 selects $3"; else bad "install.sh: $1 $2 selected '$(platform_of "$bin")', want $3"; fi
done
echo "$out" | grep -q 'Installing Vario v0.1.0-beta.2 (beta channel)' && ok "install.sh: beta resolves to v0.1.0-beta.2 (alpha, invalid tags and release-note text skipped)" || bad "install.sh: beta resolution"
echo "$out" | grep -q "Downloading $base/gh/good/nedanwr/vario/releases/download/v0.1.0-beta.2/vario_0.1.0-beta.2_darwin_amd64.tar.gz" &&
  ok "install.sh: downloads from GitHub release URLs" || bad "install.sh: download URL"
echo "$out" | grep -q 'Verified SHA-256 checksum' && ok "install.sh: checksum verified" || bad "install.sh: checksum message"
echo "$out" | grep -q 'Installed Vario v0.1.0-beta.2' && ok "install.sh: success message names the version" || bad "install.sh: success message"
echo "$out" | grep -q 'is not on your PATH' && echo "$out" | grep -q '.zshrc' && ok "install.sh: explains a missing PATH entry" || bad "install.sh: PATH hint missing"
echo "$out" | grep -q 'vario init' && ok "install.sh: suggests vario init without a configuration" || bad "install.sh: next steps"
[ "$(stat -c %a "$bin" 2>/dev/null || stat -f %Lp "$bin")" = 755 ] && ok "install.sh: binary mode 0755" || bad "install.sh: binary mode"
[ ! -e "$home/.vario" ] && ok "install.sh: does not create ~/.vario" || bad "install.sh: created ~/.vario"
[ ! -e "$home/Library" ] && [ ! -e "$home/.zshrc" ] && [ ! -e "$home/.config" ] && ok "install.sh: no service or shell startup files written" || bad "install.sh: wrote service or startup files"
[ "$(find "$home" -type f | wc -l | tr -d ' ')" = 1 ] && ok "install.sh: the binary is the only file written" || { bad "install.sh: other files written"; find "$home" -type f; }

TRANSLATED=1 run_sh Darwin x86_64 good
expect_ok "install.sh: Rosetta shell"
[ "$(platform_of "$home/.local/bin/vario")" = darwin/arm64 ] && ok "install.sh: Rosetta shell gets the native arm64 build" || bad "install.sh: Rosetta selection"

# Channel resolution through the GitHub API.
run_sh Linux x86_64 pretty
expect_out "install.sh: pretty-printed API response" 'Installed Vario v0.1.0-beta.2'
run_sh Linux x86_64 rc
expect_out "install.sh: beta channel includes release candidates (rc.1 > beta.10)" 'Installed Vario v0.1.0-rc.1'
run_sh Linux x86_64 mixed
expect_out "install.sh: beta channel includes a newer stable release" 'Installing Vario v0.2.0 (beta channel)'
run_sh Linux x86_64 mixed --channel stable
expect_out "install.sh: stable channel skips prereleases" 'Installing Vario v0.2.0 (stable channel)'
run_sh Linux x86_64 good --channel stable
expect_fail "install.sh: stable channel before any stable release" 'no stable release of Vario is published on GitHub yet'
run_sh Linux x86_64 empty
expect_fail "install.sh: no releases at all" 'no beta release'
run_sh Linux x86_64 alphaonly
expect_fail "install.sh: alpha releases are on no channel" 'no beta release'
run_sh Linux x86_64 html
expect_fail "install.sh: non-JSON API response" 'unexpected response'
run_sh Linux x86_64 ratelimit
expect_fail "install.sh: API rate limit explained" '60 anonymous API requests'
run_sh Linux x86_64 missing
expect_fail "install.sh: API unavailable (404)" 'could not list Vario releases'
run_sh Linux x86_64 ratelimit --version v0.1.0-beta.1
expect_out "install.sh: --version needs no API request" 'Installed Vario v0.1.0-beta.1'

# Explicit versions.
run_sh Linux x86_64 good --version 0.1.0-beta.1
expect_ok "install.sh: explicit version without v"
[ "$("$home/.local/bin/vario" version --short)" = v0.1.0-beta.1 ] && ok "install.sh: explicit version installed" || bad "install.sh: explicit version"
EXTRA_ENV=VARIO_VERSION=v0.1.0-beta.1 run_sh Linux x86_64 good
echo "$out" | grep -q 'Installing Vario v0.1.0-beta.1 for' && ok "install.sh: VARIO_VERSION" || bad "install.sh: VARIO_VERSION"
run_sh Linux x86_64 good --version v0.9.0
expect_fail "install.sh: unknown version fails cleanly" 'download failed'
run_sh Linux x86_64 good --version 'v1.0; rm -rf /'
expect_fail "install.sh: invalid version rejected" 'invalid version'
run_sh Linux x86_64 good --channel nightly
expect_fail "install.sh: unknown channel rejected" 'unknown channel'

# Custom install directory, existing installations, upgrades and downgrades.
n=$((n + 1)); H=$t/home$n; mkdir -p "$H"
dir=$H/custom/bin
run_sh Linux x86_64 good --install-dir "$dir" --version v0.1.0-beta.1
expect_ok "install.sh: custom install directory (created)"
run_sh Linux x86_64 good --install-dir "$dir"
expect_ok "install.sh: upgrade"
echo "$out" | grep -q 'Upgraded Vario v0.1.0-beta.1 -> v0.1.0-beta.2' && ok "install.sh: upgrade message" || bad "install.sh: upgrade message"
before=$(ls -li "$dir/vario")
run_sh Linux x86_64 good --install-dir "$dir"
expect_ok "install.sh: same version again"
echo "$out" | grep -q 'already installed' && [ "$(ls -li "$dir/vario")" = "$before" ] && ok "install.sh: same version is left alone" || bad "install.sh: same version reinstalled"
run_sh Linux x86_64 good --install-dir "$dir" --force
expect_ok "install.sh: --force reinstalls"
[ "$(ls -li "$dir/vario")" != "$before" ] && ok "install.sh: --force replaced the file" || bad "install.sh: --force did not replace"
run_sh Linux x86_64 good --install-dir "$dir" --version v0.1.0-beta.1
expect_fail "install.sh: downgrade refused" 'Downgrades are not supported'
[ "$("$dir/vario" version --short)" = v0.1.0-beta.2 ] && ok "install.sh: refused downgrade kept the binary" || bad "install.sh: downgrade changed the binary"
run_sh Linux x86_64 good --install-dir "$dir" --version v0.1.0-beta.1 --force
expect_ok "install.sh: downgrade with --force"

# Existing configuration is never touched.
mkdir -p "$H/.vario"
printf '# my config\n' >"$H/.vario/config.toml"
cfg_before=$(sha "$H/.vario/config.toml")
run_sh Linux x86_64 good --install-dir "$dir"
expect_ok "install.sh: upgrade with an existing configuration"
[ "$(sha "$H/.vario/config.toml")" = "$cfg_before" ] && [ "$(ls -A "$H/.vario")" = config.toml ] && echo "$out" | grep -q 'left unchanged' &&
  ok "install.sh: configuration untouched and reported" || bad "install.sh: configuration handling"

# A symlinked target (package manager) is not replaced without --force.
mkdir -p "$H/elsewhere" "$H/linkdir"
cp "$dir/vario" "$H/elsewhere/vario"
ln -s "$H/elsewhere/vario" "$H/linkdir/vario"
run_sh Linux x86_64 good --install-dir "$H/linkdir"
expect_fail "install.sh: symlinked target refused" 'symbolic link'
[ -L "$H/linkdir/vario" ] && ok "install.sh: symlink left in place" || bad "install.sh: symlink replaced"

# PATH already containing the directory: no PATH hint; shadowing copy is reported.
mkdir -p "$H/shadow"
cp "$dir/vario" "$H/shadow/vario"
out=$(env -i PATH="$H/shadow:$dir:$syspath" HOME="$H" CURL_CA_BUNDLE="$CURL_CA_BUNDLE" FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 \
  VARIO_GITHUB_URL="$base/gh/good" VARIO_GITHUB_API_URL="$base/api/good" \
  sh "$root/scripts/install.sh" --install-dir "$dir" --force 2>&1) && rc=0 || rc=$?
expect_ok "install.sh: install with the directory on PATH"
! echo "$out" | grep -q 'is not on your PATH' && echo "$out" | grep -q "resolves to $H/shadow/vario" && ok "install.sh: reports a shadowing copy on PATH" || { bad "install.sh: PATH checks"; echo "$out"; }
unset H

# Failures: nothing may be installed or replaced, and nothing left behind.
n=$((n + 1)); H=$t/home$n; mkdir -p "$H"
dir=$H/.local/bin
run_sh Linux x86_64 good --version v0.1.0-beta.1
keep=$(sha "$dir/vario")
check_kept() { [ "$(sha "$dir/vario")" = "$keep" ] && ok "$1: existing binary kept" || bad "$1: existing binary changed"; no_leftovers "$dir" "$1"; }

run_sh Linux x86_64 corrupt
expect_fail "install.sh: checksum mismatch" 'checksum mismatch'
check_kept "install.sh: checksum mismatch"
run_sh Linux x86_64 nosum
expect_fail "install.sh: missing checksum entry" 'no valid entry'
check_kept "install.sh: missing checksum entry"
run_sh Linux arm64 nosum --install-dir "$H/other"
expect_ok "install.sh: other entries in the same checksums.txt still work"
run_sh Linux x86_64 truncate --version v0.1.0-beta.2
expect_fail "install.sh: interrupted download" 'download failed\|checksum mismatch'
check_kept "install.sh: interrupted download"
run_sh Linux x86_64 wrongbin
expect_fail "install.sh: binary reporting the wrong version" 'reports .v9.9.9.'
check_kept "install.sh: binary reporting the wrong version"
run_sh Linux x86_64 insecure
expect_fail "install.sh: redirect to plain http refused" 'download failed'
check_kept "install.sh: redirect to plain http"
run_sh Linux x86_64 loop
expect_fail "install.sh: redirect loop stops" 'download failed'
EXTRA_ENV="VARIO_GITHUB_API_URL=http://127.0.0.1:$port/api/good" run_sh Linux x86_64 good
expect_fail "install.sh: plain http GitHub API refused" 'GitHub API URL must be an https:// URL'
unset H

# The built-in URLs, without reaching GitHub: an unreachable proxy fails every request.
out=$(env -i PATH="$syspath" HOME="$t/proxyhome" FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 https_proxy=http://127.0.0.1:9 \
  sh "$root/scripts/install.sh" 2>&1) && rc=0 || rc=$?
expect_fail "install.sh: real GitHub API URL" 'could not list Vario releases from GitHub (https://api.github.com/repos/nedanwr/vario/releases?per_page=100)'
out=$(env -i PATH="$syspath" HOME="$t/proxyhome" FAKE_UNAME_S=Linux FAKE_UNAME_M=x86_64 https_proxy=http://127.0.0.1:9 \
  sh "$root/scripts/install.sh" --version v0.1.0-beta.1 2>&1) && rc=0 || rc=$?
expect_fail "install.sh: real GitHub release URL" 'Downloading https://github.com/nedanwr/vario/releases/download/v0.1.0-beta.1/vario_0.1.0-beta.1_linux_amd64.tar.gz'

# Static mirror source (the future custom domain / R2 layout).
run_sh Linux x86_64 none --source "$base/good"
expect_out "install.sh: mirror beta channel" 'Installing Vario v0.1.0-beta.2 (beta channel)'
echo "$out" | grep -q "Downloading $base/good/releases/v0.1.0-beta.2/vario_0.1.0-beta.2_linux_amd64.tar.gz" && ok "install.sh: mirror release URLs" || bad "install.sh: mirror URLs"
EXTRA_ENV="VARIO_DOWNLOAD_SOURCE=$base/good/" run_sh Linux x86_64 none --channel stable
expect_out "install.sh: mirror stable channel via VARIO_DOWNLOAD_SOURCE" 'Installed Vario v0.2.0'
run_sh Linux x86_64 none --source "$base/good" --version v0.1.0-beta.1
expect_out "install.sh: mirror explicit version" 'Installed Vario v0.1.0-beta.1'
run_sh Linux x86_64 none --source "$base/corrupt"
expect_fail "install.sh: mirror checksum mismatch" 'checksum mismatch'
run_sh Linux x86_64 none --source "$base/nosum"
expect_fail "install.sh: malformed channel (two lines)" 'malformed channel metadata'
run_sh Linux x86_64 none --source "$base/nosum" --channel stable
expect_fail "install.sh: malformed channel (HTML)" 'malformed channel metadata'
run_sh Linux x86_64 none --source "$base/missing"
expect_fail "install.sh: missing channel file (404)" 'download failed'
run_sh Linux x86_64 none --source "$base/redirect"
expect_fail "install.sh: mirror redirects are refused" 'HTTP 302'
run_sh Linux x86_64 none --source "http://127.0.0.1:$port/good"
expect_fail "install.sh: plain http mirror refused" 'must be an https:// URL'
run_sh Linux x86_64 none --source "https://127.0.0.1:$port/good?x=\$(id)"
expect_fail "install.sh: unsafe mirror URL refused" 'invalid download source'
run_sh Linux x86_64 none --source GitHub
expect_fail "install.sh: unknown source refused" 'must be an https:// URL'

# Unsupported platforms.
run_sh FreeBSD amd64 good
expect_fail "install.sh: FreeBSD unsupported" 'unsupported operating system'
run_sh Linux i686 good
expect_fail "install.sh: 32-bit x86 unsupported" 'unsupported CPU architecture'
run_sh Linux armv7l good
expect_fail "install.sh: armv7 unsupported" 'unsupported CPU architecture'
run_sh MINGW64_NT-10.0 x86_64 good
expect_fail "install.sh: Git Bash points to install.ps1" 'raw.githubusercontent.com/nedanwr/vario/master/scripts/install.ps1'

# Unwritable install directory (skipped as root, who can write anywhere).
if [ "$(id -u)" != 0 ]; then
  n=$((n + 1)); ro=$t/ro$n; mkdir -p "$ro"; chmod 0555 "$ro"
  run_sh Linux x86_64 good --install-dir "$ro"
  expect_fail "install.sh: unwritable directory" 'not writable'
  run_sh Linux x86_64 good --install-dir "$ro/sub"
  expect_fail "install.sh: uncreatable directory" 'cannot create'
  chmod 0755 "$ro"
fi

# Unknown options; --help.
run_sh Linux x86_64 good --frobnicate
expect_fail "install.sh: unknown option" 'unknown option'
run_sh Linux x86_64 good --help
expect_out "install.sh: --help shows the piped command" 'raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh | sh -s --'

# --- install.ps1 --------------------------------------------------------------

if [ "${SKIP_PWSH:-}" != 1 ] && command -v pwsh >/dev/null; then
  # pwsh on Linux/macOS stands in for Windows: $env:OS selects the Windows path
  # and PROCESSOR_ARCHITECTURE the architecture. The PATH registry update is
  # Windows-only, so these runs use -NoModifyPath.
  run_ps() { # run_ps ARCH SITE [args...]
    local arch=$1 site=$2
    shift 2
    set +e
    out=$(env OS=Windows_NT PROCESSOR_ARCHITECTURE="$arch" PROCESSOR_ARCHITEW6432="${W6432:-}" \
      VARIO_HOME="$PH/.vario" USERPROFILE="$PH" LOCALAPPDATA="$PH/AppData/Local" SSL_CERT_FILE="$SSL_CERT_FILE" \
      VARIO_GITHUB_URL="$base/gh/$site" VARIO_GITHUB_API_URL="$base/api/$site" ${EXTRA_ENV:+"$EXTRA_ENV"} \
      pwsh -NoProfile -NonInteractive -File "$root/scripts/install.ps1" -NoModifyPath "$@" 2>&1)
    rc=$?
    set -e
  }
  PH=$t/pshome
  mkdir -p "$PH"
  pdir=$PH/AppData/Local/Programs/Vario
  run_ps AMD64 good
  expect_ok "install.ps1: amd64 install of the newest beta from GitHub"
  [ "$(platform_of "$pdir/vario.exe")" = windows/amd64 ] && ok "install.ps1: selects windows/amd64" || { bad "install.ps1: selection"; echo "$out"; }
  echo "$out" | grep -q 'Installing Vario v0.1.0-beta.2 (beta channel)' && ok "install.ps1: beta resolves to v0.1.0-beta.2" || bad "install.ps1: beta resolution"
  echo "$out" | grep -q "Downloading $base/gh/good/nedanwr/vario/releases/download/v0.1.0-beta.2/vario_0.1.0-beta.2_windows_amd64.zip" &&
    ok "install.ps1: downloads from GitHub release URLs (following the redirect)" || bad "install.ps1: download URL"
  echo "$out" | grep -q 'Installed Vario v0.1.0-beta.2' && ok "install.ps1: success message" || bad "install.ps1: success message"
  echo "$out" | grep -q 'is not on your PATH' && ok "install.ps1: -NoModifyPath explains PATH" || bad "install.ps1: PATH hint"
  run_ps AMD64 good
  echo "$out" | grep -q 'already installed' && ok "install.ps1: same version left alone" || bad "install.ps1: same version"
  run_ps AMD64 good -Version v0.1.0-beta.1
  expect_fail "install.ps1: downgrade refused" 'Downgrades are not supported'
  run_ps AMD64 mixed -Channel stable
  expect_ok "install.ps1: upgrade via the stable channel"
  echo "$out" | grep -q 'Upgraded Vario v0.1.0-beta.2 -> v0.2.0' && ok "install.ps1: upgrade message" || bad "install.ps1: upgrade message"
  [ -z "$(find "$pdir" -name 'vario.exe.*')" ] && ok "install.ps1: no staged or old files left" || bad "install.ps1: leftovers in $pdir"
  run_ps AMD64 pretty -InstallDir "$PH/pretty"
  expect_out "install.ps1: pretty-printed API response" 'Installed Vario v0.1.0-beta.2'
  run_ps AMD64 rc -InstallDir "$PH/rc"
  expect_out "install.ps1: beta channel includes release candidates" 'Installed Vario v0.1.0-rc.1'
  run_ps AMD64 good -Channel stable -InstallDir "$PH/stable"
  expect_fail "install.ps1: stable channel before any stable release" 'no stable release of Vario is published on GitHub yet'
  run_ps AMD64 alphaonly -InstallDir "$PH/alpha"
  expect_fail "install.ps1: alpha releases are on no channel" 'no beta release'
  run_ps AMD64 html -InstallDir "$PH/html"
  expect_fail "install.ps1: non-JSON API response" 'unexpected response'
  run_ps AMD64 ratelimit -InstallDir "$PH/rl"
  expect_fail "install.ps1: API rate limit explained" '60 anonymous API requests'
  run_ps AMD64 ratelimit -Version v0.1.0-beta.1 -InstallDir "$PH/rl"
  expect_out "install.ps1: -Version needs no API request" 'Installed Vario v0.1.0-beta.1'
  run_ps ARM64 good -InstallDir "$PH/arm"
  [ "$(platform_of "$PH/arm/vario.exe")" = windows/arm64 ] && ok "install.ps1: ARM64 selects windows/arm64" || bad "install.ps1: ARM64 selection"
  W6432=ARM64 run_ps x86 good -InstallDir "$PH/arm32"
  [ "$(platform_of "$PH/arm32/vario.exe")" = windows/arm64 ] && ok "install.ps1: 32-bit shell on ARM64 selects arm64" || bad "install.ps1: WOW64 selection"
  W6432=AMD64 run_ps x86 good -InstallDir "$PH/x86on64"
  [ "$(platform_of "$PH/x86on64/vario.exe")" = windows/amd64 ] && ok "install.ps1: 32-bit shell on x64 selects amd64" || bad "install.ps1: WOW64 x64 selection"
  run_ps x86 good -InstallDir "$PH/x86"
  expect_fail "install.ps1: 32-bit Windows unsupported" 'unsupported CPU architecture'
  keep=$(sha "$pdir/vario.exe")
  run_ps AMD64 corrupt -Force
  expect_fail "install.ps1: checksum mismatch" 'checksum mismatch'
  [ "$(sha "$pdir/vario.exe")" = "$keep" ] && ok "install.ps1: checksum mismatch kept the binary" || bad "install.ps1: binary changed"
  run_ps AMD64 nosum -Force
  expect_fail "install.ps1: missing checksum entry" 'no valid entry'
  run_ps AMD64 truncate -Version v0.1.0-beta.2 -Force
  expect_fail "install.ps1: interrupted download" 'download failed\|checksum mismatch'
  run_ps AMD64 insecure -Force
  expect_fail "install.ps1: redirect to plain http refused" 'non-HTTPS'
  run_ps AMD64 loop -Force
  expect_fail "install.ps1: redirect loop stops" 'download failed'
  [ "$(sha "$pdir/vario.exe")" = "$keep" ] && ok "install.ps1: failed installs kept the binary" || bad "install.ps1: binary changed by a failure"
  run_ps AMD64 wrongbin -InstallDir "$PH/wrong"
  expect_fail "install.ps1: wrong binary version" 'reports .v9.9.9.'
  [ ! -e "$PH/wrong/vario.exe" ] && ok "install.ps1: wrong binary not installed" || bad "install.ps1: wrong binary installed"
  run_ps AMD64 good -Version 'v1;calc'
  expect_fail "install.ps1: invalid version rejected" 'invalid version'
  EXTRA_ENV="VARIO_GITHUB_URL=http://127.0.0.1:$port/gh/good" run_ps AMD64 good -Force
  expect_fail "install.ps1: plain http GitHub URL refused" 'must be https:// URLs'
  # Static mirror source.
  run_ps AMD64 none -Source "$base/good" -InstallDir "$PH/mirror"
  expect_out "install.ps1: mirror beta channel" 'Installing Vario v0.1.0-beta.2 (beta channel)'
  echo "$out" | grep -q "Downloading $base/good/releases/v0.1.0-beta.2/vario_0.1.0-beta.2_windows_amd64.zip" && ok "install.ps1: mirror release URLs" || bad "install.ps1: mirror URLs"
  EXTRA_ENV="VARIO_DOWNLOAD_SOURCE=$base/good/" run_ps AMD64 none -Channel stable -InstallDir "$PH/mirror"
  expect_out "install.ps1: mirror stable channel via VARIO_DOWNLOAD_SOURCE" 'Upgraded Vario v0.1.0-beta.2 -> v0.2.0'
  run_ps AMD64 none -Source "$base/nosum" -InstallDir "$PH/mirror2"
  expect_fail "install.ps1: malformed channel" 'malformed channel metadata'
  run_ps AMD64 none -Source "$base/missing" -InstallDir "$PH/mirror2"
  expect_fail "install.ps1: missing channel (404)" 'download failed'
  run_ps AMD64 none -Source "$base/redirect" -InstallDir "$PH/mirror2"
  expect_fail "install.ps1: mirror redirect refused" 'HTTP 302'
  run_ps AMD64 none -Source "http://127.0.0.1:$port/good"
  expect_fail "install.ps1: plain http mirror refused" 'must be github or an https:// URL'
  # The built-in URLs, without reaching GitHub.
  set +e
  out=$(env OS=Windows_NT PROCESSOR_ARCHITECTURE=AMD64 LOCALAPPDATA="$PH/proxy" https_proxy=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
    pwsh -NoProfile -NonInteractive -File "$root/scripts/install.ps1" -Version v0.1.0-beta.1 -NoModifyPath 2>&1)
  rc=$?
  set -e
  expect_fail "install.ps1: real GitHub release URL" 'Downloading https://github.com/nedanwr/vario/releases/download/v0.1.0-beta.1/vario_0.1.0-beta.1_windows_amd64.zip'
  set +e
  out=$(env -u OS pwsh -NoProfile -NonInteractive -File "$root/scripts/install.ps1" 2>&1)
  rc=$?
  set -e
  expect_fail "install.ps1: refuses non-Windows and points to install.sh" 'raw.githubusercontent.com/nedanwr/vario/master/scripts/install.sh'
  # irm | iex style: errors must not exit the caller's session.
  set +e
  out=$(env OS=Windows_NT PROCESSOR_ARCHITECTURE=AMD64 VARIO_GITHUB_API_URL="$base/api/missing" SSL_CERT_FILE="$SSL_CERT_FILE" LOCALAPPDATA="$PH/iex" \
    pwsh -NoProfile -NonInteractive -Command "Get-Content -Raw '$root/scripts/install.ps1' | Invoke-Expression; 'session still alive'" 2>&1)
  set -e
  echo "$out" | grep -q 'could not list Vario releases' && echo "$out" | grep -q 'session still alive' && ok "install.ps1: iex errors do not end the session" || { bad "install.ps1: iex behaviour"; echo "$out"; }
  # The scriptblock form with parameters, as documented.
  set +e
  out=$(env OS=Windows_NT PROCESSOR_ARCHITECTURE=AMD64 VARIO_GITHUB_URL="$base/gh/good" VARIO_GITHUB_API_URL="$base/api/good" SSL_CERT_FILE="$SSL_CERT_FILE" \
    USERPROFILE="$PH" LOCALAPPDATA="$PH/sb" pwsh -NoProfile -NonInteractive -Command \
    "& ([scriptblock]::Create((Get-Content -Raw '$root/scripts/install.ps1'))) -Version v0.1.0-beta.1 -NoModifyPath" 2>&1)
  rc=$?
  set -e
  expect_out "install.ps1: scriptblock form with -Version" 'Installed Vario v0.1.0-beta.1'
  [ ! -e "$PH/.vario" ] && ok "install.ps1: does not create .vario" || bad "install.ps1: created .vario"
  # Compare-SemVer and Test-InChannel, loaded from the script's AST without running the installer.
  cat >"$t/funcs.ps1" <<'PS1'
$ast = [System.Management.Automation.Language.Parser]::ParseFile($args[0], [ref]$null, [ref]$null)
$ast.FindAll({ $args[0] -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -match '^(Compare-|Test-InChannel)' }, $false) |
  ForEach-Object { Invoke-Expression $_.Extent.Text }
$v = $args[1] -split " "; $errs = 0
for ($i = 0; $i -lt $v.Count; $i++) { for ($j = 0; $j -lt $v.Count; $j++) {
  $want = [Math]::Sign($i - $j); $got = Compare-SemVer $v[$i] $v[$j]
  if ($got -ne $want) { $errs++; "$($v[$i]) vs $($v[$j]) = $got, want $want" } } }
"errors=$errs"
$chans = foreach ($tag in ($args[2] -split " ")) { "$tag=" + ((@('stable', 'beta') | Where-Object { Test-InChannel $_ $tag }) -join ',') }
"channels=$($chans -join ' ')"
PS1
  out=$(pwsh -NoProfile -NonInteractive -File "$t/funcs.ps1" "$root/scripts/install.ps1" "$ordered" "$channel_tags" 2>&1) || true
  echo "$out" | grep -qx 'errors=0' && ok "install.ps1: SemVer precedence" || { bad "install.ps1: SemVer precedence"; echo "$out" | sed 's/^/    /'; }
  echo "$out" | grep -qxF "channels=$want_channels" && ok "install.ps1: channel membership" || { bad "install.ps1: channel membership"; echo "$out" | sed 's/^/    /'; }
else
  echo "skip - install.ps1 tests (pwsh not found)"
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
