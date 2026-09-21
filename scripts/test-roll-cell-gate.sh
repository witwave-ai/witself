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
BACKUP_REGISTRY_CASE=index
POSTGRES_REGISTRY_CASE=index
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
mkdir -p "$REPO_ROOT/images/postgresql"
cp "$SOURCE_ROOT/images/postgresql/mirror.json" "$REPO_ROOT/images/postgresql/"
# The fixture source digest is calculated from the same exact bytes returned by
# the offline registry below. It deliberately differs from the real deployment.
# Compile a fixture-local generator so its embedded overlays and generated
# values agree on the synthetic content. Never let disk override the overlay.
GENERATOR_ROOT="$TEST_ROOT/generator-source"
mkdir -p "$GENERATOR_ROOT/internal/cmd"
cp "$SOURCE_ROOT/go.mod" "$SOURCE_ROOT/go.sum" "$GENERATOR_ROOT/"
cp -R "$SOURCE_ROOT/internal/gitopsvalues" "$GENERATOR_ROOT/internal/"
cp -R "$SOURCE_ROOT/internal/cmd/gitops-cell-values" "$GENERATOR_ROOT/internal/cmd/"
python3 - "$REPO_ROOT/images/postgresql/mirror.json" "$REPO_ROOT/.gitops/cells" "$GENERATOR_ROOT/internal/gitopsvalues/overlays" <<'EOF_POSTGRES_SOURCE'
import hashlib
import json
from pathlib import Path
import re
import sys
descriptor, cells, overlays = map(str, sys.argv[1:])
config = json.loads(Path(descriptor).read_text())
for cell in config['cells']:
    # Distinct shapes prove per-cell source selection. An upstream single
    # manifest must remain single; an upstream index must remain an index.
    single = cell.endswith('-serving')
    body = {'schemaVersion': 2,
            'mediaType': 'application/vnd.oci.image.manifest.v1+json' if single else 'application/vnd.oci.image.index.v1+json',
            'annotations': {'fixture.repository': 'witwave-ai/images/postgresql'}}
    if single:
        body.update(config={'digest': 'sha256:' + '1' * 64, 'size': 42}, layers=[])
    else:
        body['manifests'] = [
            {'digest': 'sha256:' + char * 64, 'size': 42,
             'mediaType': 'application/vnd.oci.image.manifest.v1+json',
             'platform': {'os': 'linux', 'architecture': arch}}
            for char, arch in [('1', 'amd64'), ('2', 'arm64')]]
    digest = 'sha256:' + hashlib.sha256(json.dumps(body).encode()).hexdigest()
    config['cells'][cell]['digest'] = digest
    values = Path(cells) / cell / 'values.yaml'
    text, count = re.subn(r'(?m)^(      digest: )sha256:[a-f0-9]{64}$',
                          lambda match: match[1] + digest, values.read_text())
    assert count == 1
    values.write_text(text)
    overlay = Path(overlays) / (cell + '.yaml.tmpl')
    text, count = re.subn(r'(?m)^(      digest: )sha256:[a-f0-9]{64}$',
                          lambda match: match[1] + digest, overlay.read_text())
    assert count == 1
    overlay.write_text(text)
Path(descriptor).write_text(json.dumps(config))
EOF_POSTGRES_SOURCE
(cd "$GENERATOR_ROOT" && go build -o "$TEST_ROOT/generator" ./internal/cmd/gitops-cell-values)
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
git -C "$REPO_ROOT" -c tag.gpgsign=false tag v1.2.4
BACKUP_RELEASE_SCHEMA="$REPO_ROOT/.gitops/charts/apps/values.schema.json"
cp "$BACKUP_RELEASE_SCHEMA" "$TEST_ROOT/apps-schema-current.json"
printf '%s\n' '{"properties":{"apps":{"properties":{"civoPostgres":{"properties":{"backup":{"properties":{"image":{"type":"string"}}}}}}}}}' >"$BACKUP_RELEASE_SCHEMA"
git -C "$REPO_ROOT" add -- .gitops/charts/apps/values.schema.json
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release with legacy backup image schema'
git -C "$REPO_ROOT" -c tag.gpgsign=false tag v1.2.5
cp "$TEST_ROOT/apps-schema-current.json" "$BACKUP_RELEASE_SCHEMA"
git -C "$REPO_ROOT" add -- .gitops/charts/apps/values.schema.json
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release with backup image pin support'
git -C "$REPO_ROOT" -c tag.gpgsign=false tag v1.2.6
git -C "$REPO_ROOT" add -- images/postgresql/mirror.json
git -C "$REPO_ROOT" -c core.hooksPath=/dev/null -c commit.gpgsign=false \
  commit -qm 'Release with PostgreSQL mirror source'
git -C "$REPO_ROOT" -c tag.gpgsign=false tag "v$VERSION"
cp "$REPO_ROOT/images/postgresql/mirror.json" "$TEST_ROOT/postgres-mirror.baseline.json"
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
url = args[-1]
if url == 'https://ghcr.io/token':
    scope = next(arg for arg in args if arg.startswith('scope=repository:'))
    repository = scope.removeprefix('scope=repository:').removesuffix(':pull')
else:
    repository = url.removeprefix('https://ghcr.io/v2/').split('/manifests/')[0]
assert repository in ['witwave-ai/images/witself-server', 'witwave-ai/images/witself-postgres-backup', 'witwave-ai/images/postgresql']
backup = repository.endswith('/witself-postgres-backup')
postgres = repository.endswith('/postgresql')
case = os.environ['WITSELF_POSTGRES_REGISTRY_CASE' if postgres else 'WITSELF_BACKUP_REGISTRY_CASE' if backup else 'WITSELF_REGISTRY_CASE']
with open(os.environ['WITSELF_REGISTRY_LOG'], 'a') as log:
    log.write(('auth' if url.endswith('/token') else 'manifest') + ' ' + repository + '\n')
if url == 'https://ghcr.io/token':
    assert 'scope=repository:' + repository + ':pull' in args
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
tag = '1.2.3-' + os.environ['WITSELF_POSTGRES_CELL'] if postgres else '1.2.3'
assert url == 'https://ghcr.io/v2/' + repository + '/manifests/' + tag
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
body = {'schemaVersion': 2, 'mediaType': media_type, 'annotations': {'fixture.repository': repository}}
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
Path(os.environ['WITSELF_EXPECTED_DIGEST'] + ('.postgres' if postgres else '.backup' if backup else '')).write_text(digest)
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
  cp "$TEST_ROOT/postgres-mirror.baseline.json" "$REPO_ROOT/images/postgresql/mirror.json"
  rm -f "$ADMIN_LOG" "$CASE_OUTPUT" "$REGISTRY_LOG" "$TEST_ROOT/expected-digest" "$TEST_ROOT/expected-digest.backup" "$TEST_ROOT/expected-digest.postgres"
  ROLL_PATH="$DEFAULT_ROLL_PATH"
  ADMIN_BIN=
  ADMIN_EXIT=0
  REGISTRY_CASE=index
  BACKUP_REGISTRY_CASE=index
  POSTGRES_REGISTRY_CASE=index
}

run_roll() {
  (
    export PATH="$ROLL_PATH"
    export WITSELF_TEST_GENERATOR="$TEST_ROOT/generator"
    export WITSELF_REGISTRY_CASE="$REGISTRY_CASE"
    export WITSELF_BACKUP_REGISTRY_CASE="$BACKUP_REGISTRY_CASE"
    export WITSELF_POSTGRES_REGISTRY_CASE="$POSTGRES_REGISTRY_CASE"
    export WITSELF_POSTGRES_CELL="$CELL"
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
    python3 - "$BASELINE" "$ROLLED" "$VERSION" "$TEST_ROOT/expected-digest" "${3:-false}" "${4:-false}" "$CELL" <<'EOF_EXPECTED'
from pathlib import Path
import sys
baseline, rolled, version, digest_file, with_backup, with_postgres, cell = sys.argv[1:]
lines = []
in_server = False
in_postgres = False
in_backup = False
skip_backup_image = False
in_postgres_image = False
for line in Path(baseline).read_text().splitlines(keepends=True):
    if skip_backup_image:
        if line.startswith('        '):
            continue
        skip_backup_image = False
    if line == '  witselfServer:\n':
        in_server = True
    elif line.strip() and not line.startswith('    '):
        in_server = False
    if line == '  civoPostgres:\n':
        in_postgres = True
    elif line.strip() and not line.startswith('    '):
        in_postgres = False
    if in_postgres and line == '    backup:\n':
        in_backup = True
    elif line.strip() and not line.startswith('      '):
        in_backup = False
    # Rebuild only the opted-in backup image at its canonical position. The
    # PostgreSQL image and a server-only roll's existing backup pins stay intact.
    if with_backup == 'true' and in_backup and line.startswith('      image:'):
        skip_backup_image = True
        continue
    if in_postgres and line == '    image:\n':
        in_postgres_image = True
    elif line.strip() and not line.startswith('      '):
        in_postgres_image = False
    if with_postgres == 'true' and in_postgres and line == '    image:\n':
        line = '    allowInsecureImages: true\n' + line
    if with_postgres == 'true' and in_postgres_image:
        if line.startswith('      registry:'):
            line = '      registry: "ghcr.io"\n'
        elif line.startswith('      repository:'):
            line = ('      repository: "witwave-ai/images/postgresql"\n'
                    '      tag: "' + version + '-' + cell + '"\n')
        elif line.startswith('      digest:'):
            line = '      digest: ' + Path(digest_file + '.postgres').read_text() + '\n'
    if with_backup == 'true' and in_postgres and in_backup and line.startswith('      enabled:'):
        line += ('      image:\n'
                 '        repository: "ghcr.io/witwave-ai/images/witself-postgres-backup"\n'
                 '        tag: "' + version + '"\n'
                 '        digest: ' + Path(digest_file + '.backup').read_text() + '\n')
    if in_server and line.startswith('    chartVersion:'):
        line = '    chartVersion: ' + version + '\n'
    # The generated digest belongs immediately after imageTag; discard any
    # prior pin instead of appending a second imageDigest key to expectations.
    if in_server and line.startswith('    imageDigest:'):
        continue
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

# Exercise both starting states regardless of the repository's current pins.
# Build fixture inputs independently of the expected-output builder above.
python3 - "$BASELINE" "$TEST_ROOT" <<'EOF_BASELINES'
from pathlib import Path
import re
import sys

baseline, root = map(Path, sys.argv[1:])
for state in ('unpinned', 'stale'):
    def server(match):
        block = re.sub(r'^    imageDigest:.*\n', '', match[0], flags=re.M)
        if state == 'stale':
            block = re.sub(r'(^    imageTag:.*\n)',
                           r'\g<1>    imageDigest: sha256:' + 'a' * 64 + '\n',
                           block, flags=re.M)
        return block

    def postgres(match):
        def backup(backup_match):
            block = re.sub(r'^      image:[^\n]*\n(?:        [^\n]*\n)*',
                           '', backup_match[0], flags=re.M)
            if state == 'stale':
                block = re.sub(r'(^      enabled:.*\n)',
                               r'\g<1>      image:\n'
                               '        repository: "ghcr.io/witwave-ai/images/witself-postgres-backup"\n'
                               '        tag: "0.0.1"\n'
                               '        digest: sha256:' + 'b' * 64 + '\n',
                               block, flags=re.M)
            return block
        return re.sub(r'^    backup:\n.*?(?=^    \S|\Z)', backup, match[0], flags=re.M | re.S)

    values = re.sub(r'^  witselfServer:\n.*?(?=^  \S|\Z)', server,
                    baseline.read_text(), flags=re.M | re.S)
    values = re.sub(r'^  civoPostgres:\n.*?(?=^  \S|\Z)', postgres,
                    values, flags=re.M | re.S)
    (root / ('values.' + state + '.yaml')).write_text(values)
EOF_BASELINES

COMMITTED_BASELINE="$BASELINE"
for baseline_state in unpinned stale; do
  BASELINE="$TEST_ROOT/values.$baseline_state.yaml"
  for with_backup in false true; do
    reset_case
    "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
      fail "$baseline_state baseline has generation drift"
    roll_args=("$CELL" "$VERSION" --no-schema-change)
    if [ "$with_backup" = true ]; then roll_args+=(--backup-image); fi
    run_roll "${roll_args[@]}" >"$CASE_OUTPUT" 2>&1 ||
      fail "$baseline_state baseline roll (backup=$with_backup) failed"
    assert_values "$ROLLED" "$baseline_state baseline roll (backup=$with_backup)" "$with_backup"
    "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
      fail "$baseline_state baseline roll (backup=$with_backup) did not survive generation"
    if [ "$with_backup" = false ] && grep -Fq 'witself-postgres-backup' "$REGISTRY_LOG"; then
      fail "$baseline_state server-only roll contacted backup registry"
    fi
  done
done
BASELINE="$COMMITTED_BASELINE"

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
expect_output "rolled $CELL to $VERSION (apps.witselfServer.chartVersion + imageTag + imageDigest)" \
  "server-only roll summary"
if grep -Fq 'witself-postgres-backup' "$REGISTRY_LOG"; then
  fail 'server-only roll contacted backup registry before opt-in'
fi
if grep -Fq 'witwave-ai/images/postgresql' "$REGISTRY_LOG"; then
  fail 'server-only roll contacted PostgreSQL registry before opt-in'
fi

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

# Backup image opt-in requires support in the target release's apps schema,
# even though the current checkout supports structured image pins.
for release in 1.2.4 1.2.5; do
  reset_case
  if run_roll "$CELL" "$release" --backup-image --no-schema-change >"$CASE_OUTPUT" 2>&1; then
    fail "release $release without backup image pin support succeeded"
  fi
  case "$release" in
    1.2.4) expect_output 'cannot verify backup image pin support' "backup release $release" ;;
    1.2.5) expect_output 'does not declare backup image repository, tag, and digest' "backup release $release" ;;
  esac
  [ ! -e "$REGISTRY_LOG" ] || fail "unsupported backup release $release contacted registry"
  assert_values "$BASELINE" "unsupported backup release $release"
done

# Both complete multi-platform indexes are resolved before one generator write.
for kind in index list proxy; do
  reset_case
  BACKUP_REGISTRY_CASE=$kind
  run_roll "$CELL" "$VERSION" --backup-image --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
    fail "$kind backup manifest did not resolve"
  assert_values "$ROLLED" "$kind backup manifest" true
  expect_output "backup image pinned to ghcr.io/witwave-ai/images/witself-postgres-backup:$VERSION by digest" \
    "$kind backup roll summary"
  [ "$(wc -l <"$REGISTRY_LOG" | tr -d ' ')" = 4 ] || fail 'backup roll did not resolve both images exactly once'
  "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
    fail "$kind backup roll did not survive generation"
done

# An ordinary roll preserves a prior backup pin without refreshing its release.
cp "$VALUES" "$TEST_ROOT/pinned-values"
rm -f "$REGISTRY_LOG"
BACKUP_REGISTRY_CASE=unreachable
run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
  fail 'ordinary roll failed with an existing backup pin'
cmp -s "$VALUES" "$TEST_ROOT/pinned-values" || fail 'ordinary roll changed existing backup pin'
if grep -Fq 'witself-postgres-backup' "$REGISTRY_LOG"; then
  fail 'ordinary roll unnecessarily refreshed an existing backup pin'
fi

# An explicit reroll replaces the existing backup index without duplicating it.
BACKUP_REGISTRY_CASE=index
run_roll "$CELL" "$VERSION" --backup-image --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
  fail 'explicit backup reroll failed'
assert_values "$ROLLED" 'explicit backup reroll' true

# Failures in the second lookup preserve the original server pins as well.
for kind in auth-unreachable auth-empty auth-invalid unreachable redirect missing malformed mismatch duplicate unsupported empty-index invalid-json; do
  reset_case
  BACKUP_REGISTRY_CASE=$kind
  if run_roll "$CELL" "$VERSION" --backup-image --no-schema-change >"$CASE_OUTPUT" 2>&1; then
    fail "$kind backup registry response unexpectedly succeeded"
  fi
  expect_output 'backup image digest resolution failed' "$kind backup registry response"
  grep -Fq 'manifest witwave-ai/images/witself-server' "$REGISTRY_LOG" ||
    fail 'backup failure fixture did not first resolve the server image'
  assert_values "$BASELINE" "$kind backup registry response"
done

# Backup opt-in cannot bypass the original evidence gate.
reset_case
ADMIN_EXIT=1
if run_roll "$CELL" "$VERSION" --backup-image --backup-evidence "$EVIDENCE_A" >"$CASE_OUTPUT" 2>&1; then
  fail 'backup image opt-in bypassed failed evidence gate'
fi
[ ! -e "$REGISTRY_LOG" ] || fail 'failed backup opt-in gate contacted registry'
assert_values "$BASELINE" 'backup image opt-in evidence failure'

# Repository parameter validation happens before any anonymous registry request.
for repository in 'ghcr.io/witwave-ai/images/witself-postgres-backup:latest' 'other.example/a/b' 'ghcr.io/a/../b' 'ghcr.io/a/b?x=y'; do
  reset_case
  if PATH="$DEFAULT_ROLL_PATH" WITSELF_REGISTRY_LOG="$REGISTRY_LOG" \
    bash "$REPO_ROOT/scripts/resolve-server-image-digest.sh" "$VERSION" --repository "$repository" >"$CASE_OUTPUT" 2>&1; then
    fail 'resolver accepted a noncanonical repository'
  fi
  [ ! -e "$REGISTRY_LOG" ] || fail 'invalid repository reached registry'
done

# PostgreSQL mirroring requires the exact target release schema and descriptor,
# even though the current checkout has support for both.
for release in 1.2.4 1.2.5 1.2.6; do
  reset_case
  if run_roll "$CELL" "$release" --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
    fail "release $release without PostgreSQL mirror support succeeded"
  fi
  case "$release" in
    1.2.4) expect_output 'cannot verify PostgreSQL image pin support' "PostgreSQL release $release" ;;
    1.2.5) expect_output 'does not declare PostgreSQL image registry, repository, tag, and digest' "PostgreSQL release $release" ;;
    1.2.6) expect_output 'has no PostgreSQL mirror descriptor' "PostgreSQL release $release" ;;
  esac
  [ ! -e "$REGISTRY_LOG" ] || fail "unsupported PostgreSQL release $release contacted registry"
  assert_values "$BASELINE" "unsupported PostgreSQL release $release"
done

# Each registry response must verify against its own bytes and the pinned
# upstream digest, and all optional image resolutions precede the single write.
for with_backup in false true; do
  reset_case
  extra_args=()
  if [ "$with_backup" = true ]; then extra_args+=(--backup-image); fi
  run_roll "$CELL" "$VERSION" --postgres-image ${extra_args[@]+"${extra_args[@]}"} --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1 ||
    fail "PostgreSQL mirror roll (with backup $with_backup) did not proceed"
  assert_values "$ROLLED" "PostgreSQL mirror roll (with backup $with_backup)" "$with_backup" true
  expect_output "PostgreSQL image mirrored to ghcr.io/witwave-ai/images/postgresql:$VERSION-$CELL at its existing digest" \
    'PostgreSQL mirror roll summary'
  "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
    fail 'PostgreSQL mirror roll did not survive generation'
done

# Ordinary rolls preserve PostgreSQL mirror pins and need no mirror registry.
cp "$VALUES" "$TEST_ROOT/postgres-pinned-values"
rm -f "$REGISTRY_LOG"
POSTGRES_REGISTRY_CASE=unreachable
run_roll "$CELL" "$VERSION" --no-schema-change >"$CASE_OUTPUT" 2>&1 ||
  fail 'ordinary roll failed with an existing PostgreSQL mirror pin'
cmp -s "$VALUES" "$TEST_ROOT/postgres-pinned-values" || fail 'ordinary roll changed existing PostgreSQL mirror pin'
if grep -Fq 'witwave-ai/images/postgresql' "$REGISTRY_LOG"; then
  fail 'ordinary roll unnecessarily refreshed an existing PostgreSQL mirror pin'
fi
POSTGRES_REGISTRY_CASE=index
run_roll "$CELL" "$VERSION" --backup-image --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1 ||
  fail 'explicit PostgreSQL reroll failed'
assert_values "$ROLLED" 'explicit PostgreSQL reroll' true true

# Missing/private release images and invalid manifest responses never write any
# pin, even after server and backup resolution have succeeded.
for kind in auth-unreachable auth-empty auth-invalid unreachable redirect missing malformed mismatch duplicate unsupported empty-index invalid-json; do
  reset_case
  POSTGRES_REGISTRY_CASE=$kind
  if run_roll "$CELL" "$VERSION" --backup-image --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
    fail "$kind PostgreSQL registry response unexpectedly succeeded"
  fi
  expect_output 'PostgreSQL mirror digest resolution failed' "$kind PostgreSQL registry response"
  grep -Fq 'manifest witwave-ai/images/witself-postgres-backup' "$REGISTRY_LOG" ||
    fail 'PostgreSQL failure fixture did not first resolve backup image'
  assert_values "$BASELINE" "$kind PostgreSQL registry response"
done

# A valid registry digest for different bytes is still not the mirror of the
# release's approved upstream image (including a child replacing its index).
for kind in single docker-single list; do
  reset_case
  POSTGRES_REGISTRY_CASE=$kind
  if run_roll "$CELL" "$VERSION" --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
    fail "$kind PostgreSQL source substitution unexpectedly succeeded"
  fi
  expect_output 'does not match the release upstream digest' "$kind PostgreSQL source substitution"
  assert_values "$BASELINE" "$kind PostgreSQL source substitution"
done

# Local source metadata cannot silently select a different image than the
# exact release descriptor. Unknown cells also fail before registry access.
for mutation in digest source missing-cell missing-upstream-tag invalid-upstream-tag; do
  reset_case
  python3 - "$REPO_ROOT/images/postgresql/mirror.json" "$CELL" "$mutation" <<'EOF_MUTATE_SOURCE'
import json
from pathlib import Path
import sys
path, cell, mutation = sys.argv[1:]
config = json.loads(Path(path).read_text())
if mutation == 'digest':
    config['cells'][cell]['digest'] = 'sha256:' + 'f' * 64
elif mutation == 'source':
    config['source_repository'] = 'unreviewed/postgresql'
elif mutation == 'missing-upstream-tag':
    del config['cells'][cell]['upstream_tag']
elif mutation == 'invalid-upstream-tag':
    config['cells'][cell]['upstream_tag'] = '../bad'
else:
    del config['cells'][cell]
Path(path).write_text(json.dumps(config))
EOF_MUTATE_SOURCE
  if run_roll "$CELL" "$VERSION" --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
    fail "PostgreSQL local descriptor $mutation unexpectedly succeeded"
  fi
  [ ! -e "$REGISTRY_LOG" ] || fail "PostgreSQL local descriptor $mutation contacted registry"
  assert_values "$BASELINE" "PostgreSQL local descriptor $mutation"
done

# A valid published mirror cannot change an operator's existing content pin.
reset_case
python3 - "$VALUES" <<'EOF_MUTATE_CURRENT_SOURCE'
from pathlib import Path
import re
import sys
path = Path(sys.argv[1])
path.write_text(re.sub(r'(?m)^(      digest: )sha256:[a-f0-9]{64}$',
                      lambda match: match[1] + 'sha256:' + 'f' * 64, path.read_text()))
EOF_MUTATE_CURRENT_SOURCE
cp "$VALUES" "$TEST_ROOT/postgres-current-values"
if run_roll "$CELL" "$VERSION" --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1; then
  fail 'PostgreSQL mirror changed an existing database content pin'
fi
expect_output "PostgreSQL image digest drift" 'existing PostgreSQL content pin'
cmp -s "$VALUES" "$TEST_ROOT/postgres-current-values" || fail 'failed PostgreSQL mirror changed existing values'

# The reference change restarts PostgreSQL even without a schema migration.
reset_case
if run_roll "$CELL" "$VERSION" --postgres-image --no-schema-change >"$CASE_OUTPUT" 2>&1; then
  fail 'PostgreSQL image switch accepted the no-schema-change bypass'
fi
expect_output 'PostgreSQL image switch requires fresh --backup-evidence' 'PostgreSQL evidence requirement'
[ ! -e "$REGISTRY_LOG" ] || fail 'PostgreSQL evidence bypass contacted registry'
[ ! -e "$ADMIN_LOG" ] || fail 'PostgreSQL evidence bypass invoked verifier'
assert_values "$BASELINE" 'PostgreSQL evidence requirement'

# PostgreSQL opt-in cannot bypass the original backup evidence gate.
reset_case
ADMIN_EXIT=1
if run_roll "$CELL" "$VERSION" --postgres-image --backup-evidence "$EVIDENCE_A" >"$CASE_OUTPUT" 2>&1; then
  fail 'PostgreSQL image opt-in bypassed failed evidence gate'
fi
[ ! -e "$REGISTRY_LOG" ] || fail 'failed PostgreSQL opt-in gate contacted registry'
assert_values "$BASELINE" 'PostgreSQL image opt-in evidence failure'

# The serving cell selects its own release tag and distinct approved upstream
# bytes. In this fixture its upstream is a single manifest, not a rebuilt index.
(
  reset_case
  CELL=civo-sandbox-use1-serving
  VALUES="$REPO_ROOT/.gitops/cells/$CELL/values.yaml"
  BASELINE="$TEST_ROOT/serving-values.baseline.yaml"
  cp "$VALUES" "$BASELINE"
  POSTGRES_REGISTRY_CASE=single
  run_roll "$CELL" "$VERSION" --postgres-image --backup-evidence "$EVIDENCE_A" --backup-evidence "$EVIDENCE_B" >"$CASE_OUTPUT" 2>&1 ||
    fail 'serving PostgreSQL mirror did not preserve its distinct single manifest'
  assert_values "$ROLLED" 'serving PostgreSQL mirror' false true
  "$TEST_ROOT/generator" --check --root "$REPO_ROOT" >"$TEST_ROOT/check.output" 2>&1 ||
    fail 'serving PostgreSQL mirror did not survive generation'
)

# Explicit registry tag validation rejects paths, options, and duplicates
# before curl; the PostgreSQL success cases prove valid suffixed tags work.
for tag in '' '-bad' '../tag' 'a/b' 'a?b' 'a@b' 'a b'; do
  reset_case
  if PATH="$DEFAULT_ROLL_PATH" WITSELF_REGISTRY_LOG="$REGISTRY_LOG" \
    bash "$REPO_ROOT/scripts/resolve-server-image-digest.sh" "$VERSION" --tag "$tag" >"$CASE_OUTPUT" 2>&1; then
    fail 'resolver accepted an invalid image tag'
  fi
  [ ! -e "$REGISTRY_LOG" ] || fail 'invalid image tag reached registry'
done
reset_case
if PATH="$DEFAULT_ROLL_PATH" WITSELF_REGISTRY_LOG="$REGISTRY_LOG" \
  bash "$REPO_ROOT/scripts/resolve-server-image-digest.sh" "$VERSION" --tag valid --tag duplicate >"$CASE_OUTPUT" 2>&1; then
  fail 'resolver accepted duplicate image tags'
fi
[ ! -e "$REGISTRY_LOG" ] || fail 'duplicate image tags reached registry'

printf 'roll cell backup gate and server/backup/PostgreSQL image digest tests passed\n'
