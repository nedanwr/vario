#!/usr/bin/env bash
# End-to-end test of scripts/release/r2-publish.sh against a local S3 emulator
# (moto) behind a local HTTPS endpoint, followed by a real install through it.
#
# Usage: scripts/release/test-r2-publish.sh        (from the repository root)
#
# Needs Go and Docker. Builds real release archives with scripts/dist.sh, runs
# everything that talks to "R2" inside a throwaway container, and never
# contacts Cloudflare. Nothing outside a temporary directory is written.
set -euo pipefail

root=$(pwd)
[ -f "$root/scripts/release/r2-publish.sh" ] || { echo "run from the repository root" >&2; exit 1; }
command -v docker >/dev/null || { echo "test-r2-publish: docker is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "Building release archives (scripts/dist.sh)"
VERSION=v0.1.0-beta.1 DIST_DIR="$work/dist" COMMIT=$(git rev-parse HEAD) "$root/scripts/dist.sh" >/dev/null

image=vario-r2-publish-test
docker build -q -t "$image" - >/dev/null <<'EOF'
FROM python:3.12-slim
# AWS CLI v2, as on GitHub's ubuntu-24.04 runners; moto as the S3 emulator.
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends curl openssl unzip ca-certificates >/dev/null \
 && curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m)-2.31.0.zip" -o /tmp/aws.zip \
 && unzip -q /tmp/aws.zip -d /tmp && /tmp/aws/install && rm -rf /tmp/aws /tmp/aws.zip \
 && pip install -q --no-cache-dir 'moto[server]==5.2.5'
EOF

docker run --rm -v "$root":/src:ro -v "$work":/work "$image" bash -c '
set -euo pipefail
cd /work
pass=0 fail=0
ok() { pass=$((pass + 1)); echo "ok   - $*"; }
bad() { fail=$((fail + 1)); echo "FAIL - $*"; }

# S3 emulator on :5000 (no auth checks), HTTPS "custom domain" on :8443 that
# serves the bucket anonymously, like an R2 custom domain.
moto_server -H 127.0.0.1 -p 5000 >/dev/null 2>&1 &
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost \
  -keyout key.pem -out cert.pem >/dev/null 2>&1
cat >proxy.py <<"PY"
import http.server, ssl, boto3, botocore
# Plays the R2 custom domain: anonymous HTTPS GETs served from the bucket.
s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:5000", region_name="us-east-1",
                  aws_access_key_id="public", aws_secret_access_key="public")
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if "?" in self.path:
            self.send_error(400); return
        try:
            body, code = s3.get_object(Bucket="vario-downloads", Key=self.path.lstrip("/"))["Body"].read(), 200
        except botocore.exceptions.ClientError:
            body, code = b"", 404
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
s = http.server.ThreadingHTTPServer(("127.0.0.1", 8443), H)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain("cert.pem", "key.pem")
s.socket = c.wrap_socket(s.socket, server_side=True)
s.serve_forever()
PY
python3 proxy.py &
sleep 2
export CURL_CA_BUNDLE=/work/cert.pem
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test-secret-do-not-print
export R2_ENDPOINT=http://127.0.0.1:5000 R2_BUCKET=vario-downloads DOWNLOAD_BASE_URL=https://localhost:8443
export PUBLIC_VERIFY_ATTEMPTS=1
aws --endpoint-url "$R2_ENDPOINT" --region us-east-1 s3api create-bucket --bucket vario-downloads >/dev/null

# The installers must default to the publish domain; use copies pointing at the test domain.
mkdir inst
sed "s|^DEFAULT_SOURCE=.*|DEFAULT_SOURCE='"'"'https://localhost:8443'"'"'|" /src/scripts/install.sh >inst/install.sh
sed "s|^\$DefaultSource = .*|\$DefaultSource = '"'"'https://localhost:8443'"'"'|" /src/scripts/install.ps1 >inst/install.ps1
grep -q "^DEFAULT_SOURCE=.https://localhost:8443.\$" inst/install.sh && grep -q "^.DefaultSource = .https://localhost:8443.\$" inst/install.ps1 ||
  { echo "could not point the installers at the test domain"; exit 1; }
export INSTALLERS_DIR=/work/inst
pub() { TAG=$1 DIST_DIR=$2 bash /src/scripts/release/r2-publish.sh "$3" >out.txt 2>&1 || true; }
get() { aws --endpoint-url "$R2_ENDPOINT" --region auto s3 cp "s3://vario-downloads/$1" - 2>/dev/null; }
has() { if grep -q -- "$1" out.txt; then ok "$2"; else bad "$2"; sed "s/^/    /" out.txt; fi; }
expect() { if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 (got \"$1\", want \"$2\")"; fi; }

# A second release from the same archives under another name (the script never runs the binaries).
fake_release() { # fake_release TAG -> dist-TAG
  local v=${1#v} f
  mkdir "dist-$1"
  for f in dist/vario_*; do f=${f#dist/}; cp "dist/$f" "dist-$1/${f/0.1.0-beta.1/$v}"; done
  (cd "dist-$1" && sha256sum vario_* | LC_ALL=C sort -k2 >checksums.txt)
}

pub v0.1.0-beta.1 dist promote
has "is not served correctly" "promote before upload refuses (release not served)"
expect "$(get channels/beta.txt)" "" "promote before upload left the channel absent"

pub v0.1.0-beta.1 dist upload; has "uploaded   releases/v0.1.0-beta.1/checksums.txt" "upload: files uploaded"
has "Channels were not changed" "upload: does not touch channels"
expect "$(get channels/beta.txt)" "" "upload: no channel yet"
expect "$(get install.sh | head -c 9)" "" "upload: root installer not published yet"
grep -q "test-secret-do-not-print" out.txt && bad "upload: secret printed" || ok "upload: secret not printed"
pub v0.1.0-beta.1 dist upload; has "unchanged  releases/v0.1.0-beta.1/vario_0.1.0-beta.1_linux_arm64.tar.gz" "upload re-run: unchanged files skipped"
aws --endpoint-url "$R2_ENDPOINT" --region auto s3api head-object --bucket vario-downloads --key releases/v0.1.0-beta.1/vario_0.1.0-beta.1_linux_amd64.tar.gz >head.json 2>&1 || true
grep -q "immutable" head.json && ok "upload: immutable Cache-Control" || bad "upload: Cache-Control"

pub v0.1.0-beta.1 dist promote; has "moved      channels/beta.txt (new) -> v0.1.0-beta.1" "promote: beta channel created"
expect "$(get channels/beta.txt)" "v0.1.0-beta.1" "promote: beta.txt content"
expect "$(get channels/stable.txt)" "" "promote: beta release leaves stable alone"
expect "$(get install.sh | sha256sum)" "$(sha256sum <inst/install.sh)" "promote: root install.sh published"
pub v0.1.0-beta.1 dist promote; has "unchanged  channels/beta.txt" "promote re-run: no change"

# Real install through the published site (native binary of this container).
HOME=/work/home sh -c "curl -fsS https://localhost:8443/install.sh | sh" >install.txt 2>&1 || true
if grep -q "Installed Vario v0.1.0-beta.1" install.txt; then ok "install.sh via the published domain"; else bad "install.sh via the published domain"; cat install.txt; fi
expect "$(/work/home/.local/bin/vario version --short 2>&1)" "v0.1.0-beta.1" "installed real binary reports v0.1.0-beta.1"

# Immutability: a different archive under a published name is never overwritten.
fake_release v0.1.0-beta.2
pub v0.1.0-beta.2 dist-v0.1.0-beta.2 upload
cp -r dist-v0.1.0-beta.2 dist-tampered; echo x >>dist-tampered/vario_0.1.0-beta.2_linux_amd64.tar.gz
(cd dist-tampered && sha256sum vario_* | LC_ALL=C sort -k2 >checksums.txt)
pub v0.1.0-beta.2 dist-tampered upload; has "already published with different content" "upload: changed content refused"
expect "$(get releases/v0.1.0-beta.2/vario_0.1.0-beta.2_linux_amd64.tar.gz | sha256sum | cut -c1-64)" \
  "$(sha256sum <dist-v0.1.0-beta.2/vario_0.1.0-beta.2_linux_amd64.tar.gz | cut -c1-64)" "upload: published archive unchanged"

pub v0.1.0-beta.2 dist-v0.1.0-beta.2 promote; has "v0.1.0-beta.1 -> v0.1.0-beta.2" "promote: beta advances"
pub v0.1.0-beta.1 dist promote; has "kept       channels/beta.txt at v0.1.0-beta.2" "promote of an older tag keeps the channel"
has "kept       install.sh" "promote of an older tag keeps the root installers"
expect "$(get channels/beta.txt)" "v0.1.0-beta.2" "beta still v0.1.0-beta.2"

fake_release v0.2.0-alpha.1
pub v0.2.0-alpha.1 dist-v0.2.0-alpha.1 upload
pub v0.2.0-alpha.1 dist-v0.2.0-alpha.1 promote; has "no channel or root installer was changed" "alpha moves no channel"

fake_release v0.1.0
pub v0.1.0 dist-v0.1.0 upload
pub v0.1.0 dist-v0.1.0 promote
expect "$(get channels/stable.txt)" "v0.1.0" "stable release creates the stable channel"
expect "$(get channels/beta.txt)" "v0.1.0" "stable release also advances beta"

# Failures before publication.
fake_release v0.3.0-beta.1
rm dist-v0.3.0-beta.1/vario_0.3.0-beta.1_windows_arm64.zip
pub v0.3.0-beta.1 dist-v0.3.0-beta.1 upload; has "missing release file" "upload: incomplete dist refused"
expect "$(get releases/v0.3.0-beta.1/checksums.txt)" "" "upload: nothing uploaded for an incomplete dist"
fake_release v0.3.0-beta.2
mkdir inst9; sed "s|localhost:8443|localhost:9|" inst/install.sh >inst9/install.sh; sed "s|localhost:8443|localhost:9|" inst/install.ps1 >inst9/install.ps1
INSTALLERS_DIR=/work/inst9 DOWNLOAD_BASE_URL=https://localhost:9 pub v0.3.0-beta.2 dist-v0.3.0-beta.2 upload; has "is not served correctly" "upload: fails when the public domain does not serve the files"
DOWNLOAD_BASE_URL=https://vario.nedanwr.dev pub v0.3.0-beta.2 dist-v0.3.0-beta.2 upload; has "does not default to" "upload: installer/domain mismatch refused"
echo "garbage here" | aws --endpoint-url "$R2_ENDPOINT" --region auto s3 cp - s3://vario-downloads/channels/beta.txt >/dev/null
pub v0.1.0 dist-v0.1.0 promote; has "fix it by hand" "promote: malformed channel refused"
expect "$(get channels/beta.txt)" "garbage here" "promote: malformed channel not overwritten"
TAG=v1.0 bash /src/scripts/release/r2-publish.sh upload >out.txt 2>&1 || true; has "not a release tag" "invalid tag refused"

echo; echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
'
