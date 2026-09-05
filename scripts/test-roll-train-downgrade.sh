#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-downgrade.XXXXXX")"
VALUES="$TEST_ROOT/.gitops/cells/fixture/values.yaml"
DEFAULTS="$TEST_ROOT/.gitops/charts/apps/values.yaml"
trap 'rm -f "$TEST_ROOT/output" "$TEST_ROOT/calls" "$VALUES" "$DEFAULTS"; rmdir "$TEST_ROOT/.gitops/charts/apps" "$TEST_ROOT/.gitops/charts" "$TEST_ROOT/.gitops/cells/fixture" "$TEST_ROOT/.gitops/cells" "$TEST_ROOT/.gitops" "$TEST_ROOT"' EXIT
# These overrides let each regression run against a saved pre-fix script.
# shellcheck source=scripts/roll-train.sh
source "${ROLL_TRAIN_TEST_SOURCE:-$SOURCE_ROOT/scripts/roll-train.sh}"
command -v jq >/dev/null 2>&1 || die 'jq is required for downgrade tests'
command -v yq >/dev/null 2>&1 || die 'yq is required for downgrade tests'
mkdir -p "$(dirname "$VALUES")" "$(dirname "$DEFAULTS")"
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: false\n' >"$DEFAULTS"

VERSION=1.2.3
ARGO_GOOD='{"metadata":{"labels":{"witself.io/cell":"fixture"}},"status":{"sync":{"status":"Synced","revision":"1.2.2"},"health":{"status":"Healthy"}}}'
PODS_GOOD=$(jq -n '
  {items: ["witself-server", "witself-worker"] | map(. as $name | {
    metadata: {name: ($name + "-fixture"), labels: {"app.kubernetes.io/name": $name}},
    spec: {containers: [{name: $name, image: "ghcr.io/witwave-ai/witself-server:1.2.2"}]},
    status: {containerStatuses: [{name: $name, image: "ghcr.io/witwave-ai/witself-server:1.2.2"}]}
  })}')
DEPLOYMENTS_GOOD=$(jq -n '
  {items: ["witself-server", "witself-worker"] | map(. as $name | {
    metadata: {name: $name, labels: {"app.kubernetes.io/name": $name}},
    spec: {template: {spec: {containers: [{name: $name, image: "ghcr.io/witwave-ai/witself-server:1.2.2"}]}}}
  })}')

fail() {
  printf 'roll train downgrade test: FAIL: %s\n' "$*" >&2
  if [ -f "$TEST_ROOT/output" ]; then cat "$TEST_ROOT/output" >&2; fi
  exit 1
}

worker_values() {
  printf 'apps:\n  witselfServer:\n    worker:\n      enabled: %s\n' "$1" >"$VALUES"
}

# Match the complete set-based selector. The old selector intentionally returns
# only server items, reproducing Kubernetes filtering in pre-fix verification.
kubectl() {
  printf '%s\n' "$*" >>"$TEST_ROOT/calls"
  case "$*" in
    *'get applications.argoproj.io witself-server -o json') printf '%s\n' "${FIXTURE_ARGO:-$ARGO_GOOD}" ;;
    *'get pods -l app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_PODS"
      ;;
    *'get deployments -l app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_DEPLOYMENTS"
      ;;
    *'get pods -l app.kubernetes.io/name=witself-server,app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_PODS" | jq '.items |= map(select(.metadata.labels["app.kubernetes.io/name"] == "witself-server"))'
      ;;
    *'get deployments -l app.kubernetes.io/name=witself-server,app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_DEPLOYMENTS" | jq '.items |= map(select(.metadata.labels["app.kubernetes.io/name"] == "witself-server"))'
      ;;
    *) die "unexpected kubectl call: $*" ;;
  esac
}

assert_refused() {
  local label=$1 diagnostic=$2 status=0
  (require_live_not_newer fixture witself "$VALUES") >"$TEST_ROOT/output" 2>&1 || status=$?
  [ "$status" -eq 1 ] || fail "$label was not refused (exit $status)"
  grep -Eq "$diagnostic" "$TEST_ROOT/output" || fail "$label failed without its expected diagnostic: $diagnostic"
}

assert_allowed() {
  local label=$1 resource
  (require_live_not_newer fixture witself "$VALUES") >"$TEST_ROOT/output" 2>&1 ||
    fail "$label was refused"
  for resource in pods deployments; do
    grep -Fq "get $resource -l app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server -o json" \
      "$TEST_ROOT/calls" || fail "$label did not query $resource with the server and worker selector"
  done
}

for test_case in worker_requested_newer worker_running_newer worker_deployment_newer \
  worker_missing server_missing worker_unexpected enabled_happy disabled_happy \
  aliased_context missing_cell_label; do
  if [ -n "${ROLL_TRAIN_TEST_CASE:-}" ] && [ "$ROLL_TRAIN_TEST_CASE" != "$test_case" ]; then continue; fi
  FIXTURE_PODS=$PODS_GOOD
  FIXTURE_DEPLOYMENTS=$DEPLOYMENTS_GOOD
  worker_values true
  FIXTURE_ARGO=$ARGO_GOOD
  : >"$TEST_ROOT/calls"
  case "$test_case" in
    aliased_context)
      FIXTURE_ARGO=$(printf '%s\n' "$ARGO_GOOD" | jq '.metadata.labels["witself.io/cell"] = "civo-sandbox-use1-backup"')
      assert_refused 'Application labelled for another cell' 'unexpected cell identity'
      ;;
    missing_cell_label)
      FIXTURE_ARGO=$(printf '%s\n' "$ARGO_GOOD" | jq 'del(.metadata.labels)')
      assert_refused 'Application without a cell label' 'unexpected cell identity'
      ;;
    worker_requested_newer)
      FIXTURE_PODS=$(printf '%s\n' "$PODS_GOOD" | jq '.items[1].spec.containers[0].image = "ghcr.io/witwave-ai/witself-server:1.2.4"')
      assert_refused 'worker requested pod image newer than target' "live image 'ghcr.io/witwave-ai/witself-server:1[.]2[.]4' is invalid or newer than 1[.]2[.]3"
      ;;
    worker_running_newer)
      FIXTURE_PODS=$(printf '%s\n' "$PODS_GOOD" | jq '.items[1].status.containerStatuses[0].image = "ghcr.io/witwave-ai/witself-server:1.2.4"')
      assert_refused 'worker running pod image newer than target' "live image 'ghcr.io/witwave-ai/witself-server:1[.]2[.]4' is invalid or newer than 1[.]2[.]3"
      ;;
    worker_deployment_newer)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items[1].spec.template.spec.containers[0].image = "ghcr.io/witwave-ai/witself-server:1.2.4"')
      assert_refused 'worker deployment image newer than target' "live image 'ghcr.io/witwave-ai/witself-server:1[.]2[.]4' is invalid or newer than 1[.]2[.]3"
      ;;
    worker_missing)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items |= map(select(.metadata.name != "witself-worker"))')
      assert_refused 'enabled worker deployment absent' 'missing expected deployment'
      ;;
    server_missing)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items |= map(select(.metadata.name != "witself-server"))')
      assert_refused 'server deployment absent' 'missing expected deployment'
      ;;
    worker_unexpected)
      worker_values false
      assert_refused 'disabled worker deployment present' 'unexpected deployments'
      ;;
    enabled_happy)
      assert_allowed 'enabled server and worker below target'
      ;;
    disabled_happy)
      worker_values false
      FIXTURE_PODS=$(printf '%s\n' "$PODS_GOOD" | jq '.items |= map(select(.metadata.labels["app.kubernetes.io/name"] == "witself-server"))')
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items |= map(select(.metadata.name == "witself-server"))')
      assert_allowed 'disabled worker with server alone below target'
      ;;
  esac
  printf 'roll train downgrade test: %s passed\n' "$test_case"
done

printf 'roll train downgrade tests passed\n'
