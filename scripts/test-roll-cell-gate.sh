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
CELL="fixture-cell"
VERSION="1.2.3"
VALUES="$REPO_ROOT/.gitops/cells/$CELL/values.yaml"
ROLL_CELL="$REPO_ROOT/scripts/roll-cell.sh"
STUB_BIN="$TEST_ROOT/bin"
YQ_ONLY_BIN="$TEST_ROOT/yq-only-bin"
OVERRIDE_ADMIN="$TEST_ROOT/override/custom-admin"
ADMIN_LOG="$TEST_ROOT/admin.argv"
CASE_OUTPUT="$TEST_ROOT/case.output"
BASELINE="$TEST_ROOT/values.baseline.yaml"
ROLLED="$TEST_ROOT/values.rolled.yaml"
EVIDENCE_A="$TEST_ROOT/evidence/civo-sandbox-use1-backup"
EVIDENCE_B="$TEST_ROOT/evidence/civo-sandbox-usw2-dev"
ORIGINAL_PATH="$PATH"
DEFAULT_ROLL_PATH="$STUB_BIN:$ORIGINAL_PATH"
ROLL_PATH="$DEFAULT_ROLL_PATH"
ADMIN_BIN=
ADMIN_EXIT=0

mkdir -p \
  "$REPO_ROOT/.gitops/cells/$CELL" \
  "$REPO_ROOT/scripts" \
  "$STUB_BIN" \
  "$YQ_ONLY_BIN" \
  "$TEST_ROOT/override" \
  "$EVIDENCE_A" \
  "$EVIDENCE_B" \
  "$TEST_ROOT/empty-git-template"
cp "$SOURCE_ROOT/scripts/roll-cell.sh" "$ROLL_CELL"
chmod +x "$ROLL_CELL"

cat >"$BASELINE" <<'EOF_BASELINE'
apps:
  witselfServer:
    chartVersion: 0.0.1
    imageTag: 0.0.1
EOF_BASELINE
cat >"$ROLLED" <<EOF_ROLLED
apps:
  witselfServer:
    chartVersion: $VERSION
    imageTag: $VERSION
EOF_ROLLED
cp "$BASELINE" "$VALUES"

cat >"$STUB_BIN/yq" <<'EOF_YQ'
#!/usr/bin/env bash
set -euo pipefail

[ "$#" -eq 3 ] || exit 64
[ "$1" = -i ] || exit 64
expression=$2
file=$3
case "$expression" in
  *'.apps.witselfServer.chartVersion'*) key=chartVersion ;;
  *'.apps.witselfServer.imageTag'*) key=imageTag ;;
  *) exit 64 ;;
esac
version=${expression#*\"}
version=${version%%\"*}
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 64
awk -v key="$key" -v version="$version" '
  $0 ~ "^[[:space:]]*" key ":[[:space:]]*" {
    sub(/:.*/, ": " version)
    found = 1
  }
  { print }
  END { if (!found) exit 65 }
' "$file" >"${file}.tmp"
mv "${file}.tmp" "$file"
EOF_YQ
chmod +x "$STUB_BIN/yq"

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

# A yq-only PATH entry lets one scenario prove that an operator host without
# any witself-admin on PATH still fails closed.
cp "$STUB_BIN/yq" "$YQ_ONLY_BIN/yq"
chmod +x "$YQ_ONLY_BIN/yq"

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

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
git init -q --template="$TEST_ROOT/empty-git-template" "$REPO_ROOT"
git -C "$REPO_ROOT" add ".gitops/cells/$CELL/values.yaml"

reset_case() {
  cp "$BASELINE" "$VALUES"
  rm -f "$ADMIN_LOG" "$CASE_OUTPUT"
  ROLL_PATH="$DEFAULT_ROLL_PATH"
  ADMIN_BIN=
  ADMIN_EXIT=0
}

run_roll() {
  (
    export PATH="$ROLL_PATH"
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
  cmp -s "$expected" "$VALUES" || fail "$label changed values.yaml unexpectedly"
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
expect_output "civo-sandbox-usw2-dev" "missing gate options"
assert_values "$BASELINE" "missing gate options"

# The explicit no-schema-change attestation proceeds without the verifier.
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

# A verifier rejection blocks both yq edits.
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
ROLL_PATH="$YQ_ONLY_BIN:/usr/bin:/bin"
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
for smuggled in --cell=civo-sandbox-usw2-dev --no-schema-change -relative-dir ""; do
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

printf 'roll cell backup gate tests passed\n'
