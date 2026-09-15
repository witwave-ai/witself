#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-values.XXXXXX")"
DEFAULTS="$TEST_ROOT/.gitops/charts/apps/values.yaml"
VALUES="$TEST_ROOT/.gitops/cells/fixture/values.yaml"
trap 'rm -f "$TEST_ROOT/output" "$DEFAULTS" "$VALUES"; rmdir "$TEST_ROOT/.gitops/charts/apps" "$TEST_ROOT/.gitops/charts" "$TEST_ROOT/.gitops/cells/fixture" "$TEST_ROOT/.gitops/cells" "$TEST_ROOT/.gitops" "$TEST_ROOT"' EXIT
# shellcheck source=scripts/roll-train.sh
source "${ROLL_TRAIN_TEST_SOURCE:-$SOURCE_ROOT/scripts/roll-train.sh}"
command -v yq >/dev/null 2>&1 || die 'yq is required for values tests'
mkdir -p "$(dirname "$DEFAULTS")" "$(dirname "$VALUES")"

fail() {
  printf 'roll train values test: FAIL: %s\n' "$*" >&2
  if [ -f "$TEST_ROOT/output" ]; then cat "$TEST_ROOT/output" >&2; fi
  exit 1
}

assert_enabled() {
  local label=$1 expected=$2 values=$3 actual
  actual=$(cell_worker_enabled "$values" 2>"$TEST_ROOT/output") || fail "$label was rejected"
  [ "$actual" = "$expected" ] || fail "$label returned '$actual', expected '$expected'"
}

# These repository cells omit worker.enabled and inherit false from the apps
# chart. Keep each cell explicit so losing support for any one is a regression.
for cell in aws-sandbox-use1-dev aws-sandbox-usw2-dev \
  azure-sandbox-use2-dev azure-sandbox-usw2-dev gcp-sandbox-usw2-dev; do
  assert_enabled "$cell inherited worker default" false "$SOURCE_ROOT/.gitops/cells/$cell/values.yaml"
done

printf 'apps:\n  witselfServer:\n    worker:\n      enabled: false\n' >"$DEFAULTS"
printf '{}\n' >"$VALUES"
assert_enabled 'omitted apps block' false "$VALUES"
printf 'apps:\n  witselfServer:\n    worker:\n      replicaCount: 3\n' >"$VALUES"
assert_enabled 'partial worker overrides' false "$VALUES"
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: true\n' >"$VALUES"
assert_enabled 'explicit true overrides false default' true "$VALUES"

# Read the defaults next to the supplied cell, including when that worktree has
# different defaults from the script's source checkout.
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: true\n' >"$DEFAULTS"
printf '{}\n' >"$VALUES"
assert_enabled 'cell worktree default' true "$VALUES"
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: false\n' >"$VALUES"
assert_enabled 'explicit false overrides true default' false "$VALUES"

for value in '"true"' '"false"' '1' '0' 'null' '[]' '{}'; do
  printf 'apps:\n  witselfServer:\n    worker:\n      enabled: %s\n' "$value" >"$VALUES"
  status=0
  (cell_worker_enabled "$VALUES") >"$TEST_ROOT/output" 2>&1 || status=$?
  [ "$status" -eq 1 ] || fail "non-boolean worker.enabled $value was not rejected (exit $status)"
  grep -Fq worker.enabled "$TEST_ROOT/output" || fail "non-boolean $value failed without worker.enabled diagnostic"
done

printf 'apps:\n  witselfServer:\n    worker:\n      enabled: false\n' >"$VALUES"
rm "$DEFAULTS"
status=0
(cell_worker_enabled "$VALUES") >"$TEST_ROOT/output" 2>&1 || status=$?
[ "$status" -eq 1 ] || fail "missing chart defaults were not rejected (exit $status)"

printf 'roll train values tests passed\n'
