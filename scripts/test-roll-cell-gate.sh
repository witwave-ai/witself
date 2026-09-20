#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT_RAW="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-cell-gate-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT_RAW" && pwd -P)"

fail() {
  printf 'roll cell gate test: FAIL: %s\n' "$1" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  find "$TEST_ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$TEST_ROOT" 2>/dev/null || true
  exit "$status"
}
trap cleanup EXIT INT TERM

REPO_ROOT="$TEST_ROOT/repo"
CELL="civo-sandbox-use1-backup"
VERSION="1.2.3"
VALUES="$REPO_ROOT/.gitops/cells/$CELL/values.yaml"
ROLL_CELL="$REPO_ROOT/scripts/roll-cell.sh"
STUB_BIN="$TEST_ROOT/bin"
NO_ADMIN_BIN="$TEST_ROOT/no-admin-bin"
OVERRIDE_ADMIN="$TEST_ROOT/override/custom-admin"
ADMIN_LOG="$TEST_ROOT/admin.argv"
CASE_OUTPUT="$TEST_ROOT/case.output"
BASELINE="$TEST_ROOT/values.baseline.yaml"
ROLLED="$TEST_ROOT/values.rolled.yaml"
EVIDENCE_A="$TEST_ROOT/evidence/civo-sandbox-use1-backup"
EVIDENCE_B="$TEST_ROOT/evidence/civo-sandbox-use1-serving"
ORIGINAL_PATH="$PATH"
DEFAULT_ROLL_PATH="$STUB_BIN:$ORIGINAL_PATH"
ROLL_PATH="$DEFAULT_ROLL_PATH"
ADMIN_BIN=
ADMIN_EXIT=0
REGISTRY_CASE=index
REGISTRY_LOG="$TEST_ROOT/registry.log"

mkdir -p \
  "$REPO_ROOT/.gitops/cells/$CELL" \
  "$REPO_ROOT/scripts" \
  "$STUB_BIN" \
  "$NO_ADMIN_BIN" \
  "$TEST_ROOT/override" \
  "$EVIDENCE_A" \
  "$EVIDENCE_B"
cp "$SOURCE_ROOT/scripts/roll-cell.sh" "$ROLL_CELL"
chmod +x "$ROLL_CELL"

# Use the real generator and catalog in an isolated fixture repository.
cp -R "$SOURCE_ROOT/.gitops/cells" "$REPO_ROOT/.gitops/"
mkdir -p "$REPO_ROOT/.gitops/charts"
cp -R "$SOURCE_ROOT/.gitops/charts/apps" "$REPO_ROOT/.gitops/charts/"
cp -R "$SOURCE_ROOT/.gitops/charts/platform" "$REPO_ROOT/.gitops/charts/"
cp "$SOURCE_ROOT/scripts/resolve-server-image-digest.sh" "$REPO_ROOT/scripts/"
(cd "$SOURCE_ROOT" && go build -o "$TEST_ROOT/generator" ./internal/cmd/gitops-cell-values)
cat >"$REPO_ROOT/scripts/gitops-cell-values.sh" <<'EOF_GENERATOR'
#!/usr/bin/env bash
set -euo pipefail
exec "$WITSELF_TEST_GENERATOR" "$@"
EOF_GENERATOR
cp "$VALUES" "$BASELINE"

# Only this temporary repository receives fixture commits and release tags.
# The live working schema lacks digest support, so acceptance must inspect the
# target tag's schema instead of the checkout's file.
git -C "$REPO_ROOT" init -q
git -C "$REPO_ROOT" config user.name 'Roll cell fixture'
git -C "$REPO_ROOT" config user.email 'roll-cell-fixture@example.invalid'
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release without chart schema' --allow-empty
git -C "$REPO_ROOT" -c tag.gpgsign=false tag v1.2.0
mkdir -p "$REPO_ROOT/charts/witself-server"
RELEASE_SCHEMA="$REPO_ROOT/charts/witself-server/values.schema.json"
printf '%s\n' '{"properties":{"image":{"properties":{"tag":{"type":"string"}}}}}' >"$RELEASE_SCHEMA"
git -C "$REPO_ROOT" add -- charts/witself-server/values.schema.json
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release without digest support'
git -C "$REPO_ROOT" -c tag.gpgsign=false tag v1.2.1
cp "$SOURCE_ROOT/charts/witself-server/values.schema.json" "$RELEASE_SCHEMA"
git -C "$REPO_ROOT" add -- charts/witself-server/values.schema.json
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release with digest support'
git -C "$REPO_ROOT" -c tag.gpgsign=false tag "v$VERSION"
printf '%s\n' '{"properties":{"image":{"properties":{"tag":{"type":"string"}}}}}' >"$RELEASE_SCHEMA"

# The curl shim cannot access a network. It checks exact registry URLs and
# negotiates each manifest type, with a digest calculated from response bytes.
cat >"$STUB_BIN/curl" <<'EOF_CURL'
#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
assert args[0] == '-q', 'curl must ignore user configuration'
case = os.environ['WITSELF_REGISTRY_CASE']
url = args[-1]
with open(os.environ['WITSELF_REGISTRY_LOG'], 'a') as log:
    log.write(('auth' if url.endswith('/token') else 'manifest') + '\n')
if url == 'https://ghcr.io/token':
    assert 'scope=repository:witwave-ai/images/witself-server:pull' in args
    assert 'service=ghcr.io' in args
    if case == 'auth-unreachable':
        sys.exit(7)
    if case == 'auth-empty':
        print('{}')
    elif case == 'auth-invalid':
        print(json.dumps({'token': 'invalid\nheader'}))
    else:
        print(json.dumps({'token': 'synthetic-anonymous-pull'}))
    sys.exit(0)
assert url == 'https://ghcr.io/v2/witwave-ai/images/witself-server/manifests/1.2.3'
assert 'Authorization: Bearer synthetic-anonymous-pull' in sys.stdin.read()
accept = args[args.index('--header') + 1]
for media_type in ['application/vnd.oci.image.index.v1+json',
                   'application/vnd.docker.distribution.manifest.list.v2+json',
                   'application/vnd.oci.image.manifest.v1+json',
                   'application/vnd.docker.distribution.manifest.v2+json']:
    assert media_type in accept
if case == 'unreachable':
    sys.exit(7)
media_type = {
    'list': 'application/vnd.docker.distribution.manifest.list.v2+json',
    'single': 'application/vnd.oci.image.manifest.v1+json',
    'docker-single': 'application/vnd.docker.distribution.manifest.v2+json',
}.get(case, 'application/vnd.oci.image.index.v1+json')
body = {'schemaVersion': 2, 'mediaType': media_type}
if case in ['single', 'docker-single']:
    body.update(config={'digest': 'sha256:' + '1' * 64, 'size': 42}, layers=[])
else:
    body['manifests'] = [
        {'digest': 'sha256:' + char * 64, 'size': 42,
         'mediaType': 'application/vnd.oci.image.manifest.v1+json',
         'platform': {'os': 'linux', 'architecture': arch}}
        for char, arch in [('1', 'amd64'), ('2', 'arm64')]]
if case == 'unsupported':
    body['mediaType'] = 'application/octet-stream'
if case == 'empty-index':
    body['manifests'] = []
raw = json.dumps(body).encode() if case != 'invalid-json' else b'not-json'
digest = 'sha256:' + hashlib.sha256(raw).hexdigest()
Path(os.environ['WITSELF_EXPECTED_DIGEST']).write_text(digest)
header_digest = digest
if case == 'malformed':
    header_digest = 'sha256:invalid'
if case == 'mismatch':
    header_digest = 'sha256:' + '0' * 64
headers = 'HTTP/1.1 200 OK\r\n'
if case == 'redirect':
    headers = 'HTTP/1.1 302 Found\r\n'
if case == 'proxy':
    headers = 'HTTP/1.1 200 Connection established\r\n\r\n' + headers
if case != 'missing':
    headers += 'Docker-Content-Digest: ' + header_digest + '\r\n'
if case == 'duplicate':
    headers += 'Docker-Content-Digest: ' + header_digest + '\r\n'
headers += '\r\n'
Path(args[args.index('--dump-header') + 1]).write_bytes(headers.encode())
Path(args[args.index('--output') + 1]).write_bytes(raw)
EOF_CURL
chmod +x "$STUB_BIN/curl"

cat >"$STUB_BIN/witself-admin" <<'EOF_ADMIN'
#!/usr/bin/env bash
set -euo pipefail

: "${WITSELF_ADMIN_LOG:?}"
{
  printf 'CALL\n'
  printf '%s\n' "$@"
} >>"$WITSELF_ADMIN_LOG"
exit "${WITSELF_ADMIN_STUB_EXIT:-0}"
EOF_ADMIN
chmod +x "$STUB_BIN/witself-admin"

# A differently named override stub, outside PATH, proves WITSELF_ADMIN_BIN is
# the binary actually invoked and not merely the one checked for existence.
cat >"$OVERRIDE_ADMIN" <<'EOF_OVERRIDE'
#!/usr/bin/env bash
set -euo pipefail

: "${WITSELF_ADMIN_LOG:?}"
{
  printf 'OVERRIDE\n'
  printf '%s\n' "$@"
} >>"$WITSELF_ADMIN_LOG"
exit "${WITSELF_ADMIN_STUB_EXIT:-0}"
EOF_OVERRIDE
chmod +x "$OVERRIDE_ADMIN"

reset_case() {
  cp "$BASELINE" "$VALUES"
  rm -f "$ADMIN_LOG" "$CASE_OUTPUT" "$REGISTRY_LOG" "$TEST_ROOT/expected-digest"
  ROLL_PATH="$DEFAULT_ROLL_PATH"
  ADMIN_BIN=
  ADMIN_EXIT=0
  REGISTRY_CASE=index
}

run_roll() {
  (
    export PATH="$ROLL_PATH"
    export WITSELF_TEST_GENERATOR="$TEST_ROOT/generator"
    export WITSELF_REGISTRY_CASE="$REGISTRY_CASE"
    export WITSELF_REGISTRY_LOG="$REGISTRY_LOG"
    export WITSELF_EXPECTED_DIGEST="$TEST_ROOT/expected-digest"
    export WITSELF_ADMIN_LOG="$ADMIN_LOG"
    export WITSELF_ADMIN_STUB_EXIT="$ADMIN_EXIT"
    if [ -n "$ADMIN_BIN" ]; then
      export WITSELF_ADMIN_BIN="$ADMIN_BIN"
    else
      unset WITSELF_ADMIN_BIN
    fi
    bash "$ROLL_CELL" "$@"
  )
}

assert_values() {
  local expected=$1 label=$2
  if [ "$expected" = "$ROLLED" ]; then
    # Pin-only expected diff, calculated independently of the generator.
    python3 - "$BASELINE" "$ROLLED" "$VERSION" "$TEST_ROOT/expected-digest" <<'EOF_EXPECTED'
from pathlib import Path
import sys
baseline, rolled, version, digest_file = sys.argv[1:]
lines = []
in_server = False
for line in Path(baseline).read_text().splitlines(keepends=True):
    if line == '  witselfServer:\n':
        in_server = True
    elif line.strip() and not line.startswith('    '):
        in_server = False
    if in_server and line.startswith('    chartVersion:'):
        line = '    chartVersion: ' + version + '\n'
    if in_server and line.startswith('    imageTag:'):
        line = ('    imageTag: ' + version + '\n    imageDigest: ' +
                Path(digest_file).read_text() + '\n')
    lines.append(line)
Path(rolled).write_text(''.join(lines))
EOF_EXPECTED
  fi
  if ! cmp -s "$expected" "$VALUES"; then
    diff -u "$expected" "$VALUES" | sed -E 's/[[:xdigit:]]{64}/[REDACTED_DIGEST]/g' >&2 || true
    fail "$label changed values.yaml unexpectedly"
  fi
}

expect_output() {
  local expected=$1 label=$2
  grep -Fq -- "$expected" "$CASE_OUTPUT" ||
    fail "$label did not print '$expected'"
}

# No gate selection fails closed and explains the documented two-cell gate.
reset_case
if run_roll "$CELL" "$VERSION" >"$CASE_OUTPUT" 2>&1; then
  fail "missing gate options succeeded"
fi
expect_output "docs/runbooks.md" "missing gate options"
expect_output "civo-sandbox-use1-backup" "missing gate options"
expect_output "civo-sandbox-use1-serving" "missing gate options"
assert_values "$BASELINE" "missing gate options"

# Unsupported releases fail before registry access and before any pin write.
for release in 1.2.0 1.2.1 1.2.2; do
  reset_case
  if run_roll "$CELL" "$release" --no-schema-change >"$CASE_OUTPUT" 2>&1; then
    fail "release $release without verified digest support succeeded"
  fi
  case "$release" in
    1.2.0) expect_output 'chart has no readable values.schema.json' "release $release" ;;
    1.2.1) expect_output 'chart does not declare image.digest' "release $release" ;;
    1.2.2) expect_output 'is not available locally' "release $release" ;;
  esac
  [ ! -e "$REGISTRY_LOG" ] || fail "unsupported release $release contacted registry"
  assert_values "$BASELINE" "unsupported release $release"
done

# The digest-aware release proceeds without the verifier with the explicit
# no-schema-change attestation, even though the checkout schema lacks digest.
reset_case
run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
  fail "--no-schema-change did not proceed"
expect_output "warning: operator attests release $VERSION cannot advance the database schema" \
  "--no-schema-change"
[ ! -e "$ADMIN_LOG" ] || fail "--no-schema-change invoked the verifier"
assert_values "$ROLLED" "--no-schema-change"

# Two evidence directories are passed once, in order, before pins are edited.
reset_case
run_roll "$CELL" "$VERSION" \
  --backup-evidence "$EVIDENCE_A" \
  --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1 ||
  fail "verified backup evidence did not proceed"
cat >"$TEST_ROOT/admin.expected" <<EOF_EXPECTED_ADMIN
CALL
backup-evidence
verify
--release
$VERSION
--
$EVIDENCE_A
$EVIDENCE_B
EOF_EXPECTED_ADMIN
cmp -s "$TEST_ROOT/admin.expected" "$ADMIN_LOG" ||
  fail "verifier argv or invocation count was incorrect"
expect_output "backup evidence verified for release $VERSION" "verified backup evidence"
assert_values "$ROLLED" "verified backup evidence"

# A verifier rejection blocks registry access and all edits.
reset_case
ADMIN_EXIT=1
if run_roll "$CELL" "$VERSION" \
  --backup-evidence "$EVIDENCE_A" \
  --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
  fail "rejected backup evidence succeeded"
fi
expect_output "backup evidence verification failed" "rejected backup evidence"
assert_values "$BASELINE" "rejected backup evidence"

# The two gate modes cannot be combined.
reset_case
if run_roll "$CELL" "$VERSION" --no-schema-change \
  --backup-evidence "$EVIDENCE_A" >"$CASE_OUTPUT" 2>&1; then
  fail "mutually exclusive gate options succeeded"
fi
expect_output "mutually exclusive" "mutually exclusive gate options"
[ ! -e "$ADMIN_LOG" ] || fail "usage error invoked the verifier"
assert_values "$BASELINE" "mutually exclusive gate options"

# An unavailable configured verifier fails closed before either edit.
reset_case
ADMIN_BIN="$STUB_BIN/missing-witself-admin"
if run_roll "$CELL" "$VERSION" \
  --backup-evidence "$EVIDENCE_A" \
  --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
  fail "missing WITSELF_ADMIN_BIN executable succeeded"
fi
expect_output "backup evidence verifier is not executable" \
  "missing WITSELF_ADMIN_BIN executable"
[ ! -e "$ADMIN_LOG" ] || fail "missing verifier case recorded an invocation"
assert_values "$BASELINE" "missing WITSELF_ADMIN_BIN executable"

# An operator host with no witself-admin anywhere on PATH and no override also
# fails closed before either edit.
reset_case
ROLL_PATH="$NO_ADMIN_BIN:/usr/bin:/bin"
if run_roll "$CELL" "$VERSION" \
  --backup-evidence "$EVIDENCE_A" \
  --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
  fail "absent default verifier succeeded"
fi
expect_output "backup evidence verifier is not executable" "absent default verifier"
[ ! -e "$ADMIN_LOG" ] || fail "absent default verifier case recorded an invocation"
assert_values "$BASELINE" "absent default verifier"

# WITSELF_ADMIN_BIN selects the binary that is actually invoked, and the gate
# terminates verifier flags with -- before the artifact directories.
reset_case
ADMIN_BIN="$OVERRIDE_ADMIN"
run_roll "$CELL" "$VERSION" \
  --backup-evidence "$EVIDENCE_A" \
  --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1 ||
  fail "override verifier did not proceed"
cat >"$TEST_ROOT/override.expected" <<EOF_EXPECTED_OVERRIDE
OVERRIDE
backup-evidence
verify
--release
$VERSION
--
$EVIDENCE_A
$EVIDENCE_B
EOF_EXPECTED_OVERRIDE
cmp -s "$TEST_ROOT/override.expected" "$ADMIN_LOG" ||
  fail "WITSELF_ADMIN_BIN override was not the verifier actually invoked"
assert_values "$ROLLED" "override verifier"

# An option-looking --backup-evidence value is rejected before the verifier
# runs, so it can never be parsed as a verifier flag that narrows the gate.
for smuggled in --cell=civo-sandbox-use1-serving --no-schema-change -relative-dir ""; do
  reset_case
  if run_roll "$CELL" "$VERSION" \
    --backup-evidence "$smuggled" \
    --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
    fail "option-looking evidence value '$smuggled' succeeded"
  fi
  expect_output "looks like an option" "option-looking evidence value '$smuggled'"
  [ ! -e "$ADMIN_LOG" ] || fail "option-looking evidence value '$smuggled' invoked the verifier"
  assert_values "$BASELINE" "option-looking evidence value '$smuggled'"
done

# Every supported media type pins the response itself, never a list child.
for kind in index list single docker-single proxy; do
  reset_case
  REGISTRY_CASE=$kind
  run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
    fail "$kind manifest did not resolve"
  assert_values "$ROLLED" "$kind manifest"
  grep -Fq '+    imageDigest:' "$CASE_OUTPUT" || fail "$kind roll diff omitted digest"
  "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
    fail "$kind roll did not survive generator check"
done

# Unavailable or untrustworthy registry responses leave every pin untouched.
for kind in auth-unreachable auth-empty auth-invalid unreachable redirect missing malformed mismatch duplicate unsupported empty-index invalid-json; do
  reset_case
  REGISTRY_CASE=$kind
  if run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1; then
    fail "$kind registry response unexpectedly succeeded"
  fi
  expect_output 'image digest resolution failed' "$kind registry response"
  assert_values "$BASELINE" "$kind registry response"
done

# A failed backup gate must not even consult the registry.
reset_case
ADMIN_EXIT=1
if run_roll "$CELL" "$VERSION" --backup-evidence "$EVIDENCE_A" >"$CASE_OUTPUT" 2>&1; then
  fail 'failed backup gate unexpectedly succeeded'
fi
[ ! -e "$REGISTRY_LOG" ] || fail 'failed backup gate contacted registry'
assert_values "$BASELINE" 'failed backup gate'

# Existing unrelated edits are retained when the generator refuses a roll.
reset_case
printf '# operator work in progress\n' >>"$VALUES"
cp "$VALUES" "$TEST_ROOT/drifted-values"
if run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1; then
  fail 'roll overwrote existing generation drift'
fi
cmp -s "$VALUES" "$TEST_ROOT/drifted-values" || fail 'failed roll discarded operator edits'

printf 'roll cell backup gate and image digest tests passed\n'
