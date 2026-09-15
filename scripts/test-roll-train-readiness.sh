#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-readiness.XXXXXX")"
cleanup() {
  find "$TEST_ROOT" -depth -mindepth 1 -delete
  rmdir "$TEST_ROOT"
}
trap cleanup EXIT
# shellcheck source=scripts/roll-train.sh
source "${ROLL_TRAIN_TEST_SOURCE:-$SOURCE_ROOT/scripts/roll-train.sh}"
command -v jq >/dev/null 2>&1 || die 'jq is required for readiness tests'
command -v yq >/dev/null 2>&1 || die 'yq is required for readiness tests'
VALUES_ENABLED="$TEST_ROOT/.gitops/cells/enabled/values.yaml"
VALUES_DISABLED="$TEST_ROOT/.gitops/cells/disabled/values.yaml"
mkdir -p "$(dirname "$VALUES_ENABLED")" "$(dirname "$VALUES_DISABLED")" "$TEST_ROOT/.gitops/charts/apps"
cp "$SOURCE_ROOT/.gitops/charts/apps/values.yaml" "$TEST_ROOT/.gitops/charts/apps/values.yaml"
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: true\n' >"$VALUES_ENABLED"
printf 'apps:\n  witselfServer:\n    worker:\n      enabled: false\n' >"$VALUES_DISABLED"
READINESS_CASE=${ROLL_TRAIN_READINESS_CASE:-all}
ALL_WORKLOAD_SELECTOR='app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server'

VERSION=1.2.3
ARGO_TIMEOUT=5
POLL_INTERVAL=1
ARGO_GOOD='{"metadata":{"labels":{"witself.io/cell":"fixture"}},"status":{"sync":{"status":"Synced","revision":"1.2.3"},"health":{"status":"Healthy"}}}'
PODS_GOOD='{"items":[{"metadata":{"name":"witself-server-fixture"},"spec":{"containers":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3","ready":true,"state":{"running":{}}}]}}]}'
DEPLOYMENTS_GOOD='{"items":[{"metadata":{"name":"witself-server","generation":2},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3"}]}}},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}]}'

fail() {
  printf 'roll train readiness test: FAIL: %s\n' "$*" >&2
  if [ -f "$TEST_ROOT/output" ]; then cat "$TEST_ROOT/output" >&2; fi
  exit 1
}

if [ "$READINESS_CASE" = all ] || [ "$READINESS_CASE" = core ]; then
printf '%s\n' "$PODS_GOOD" | pods_converged "$VERSION" || fail 'ready pod rejected'
for mutation in \
  '.items = []' \
  '.items[0].metadata.deletionTimestamp = "2026-09-05T00:00:00Z"' \
  '.items[0].status.phase = "Pending"' \
  '.items[0].status.conditions[0].status = "False"' \
  'del(.items[0].status.conditions)' \
  '.items[0].status.conditions += [{"type":"Ready","status":"False"}]' \
  '.items[0].status.containerStatuses[0].ready = false' \
  '.items[0].status.containerStatuses[0].state = {"waiting":{"reason":"CrashLoopBackOff"}}' \
  '.items[0].status.containerStatuses[0].state = {"terminated":{"exitCode":1}}' \
  '.items[0].status.containerStatuses[0].name = "unrelated-container"' \
  '.items[0].status.containerStatuses = []' \
  '.items[0].status.containerStatuses += [.items[0].status.containerStatuses[0]]' \
  'del(.items[0].status.containerStatuses)' \
  '.items[0].status.containerStatuses[0].image = "ghcr.io/witwave-ai/witself-server:1.2.2"' \
  '.items[0].spec.containers[0].image = "ghcr.io/witwave-ai/witself-server:1.2.2"'; do
  if printf '%s\n' "$PODS_GOOD" | jq "$mutation" | pods_converged "$VERSION" >/dev/null 2>&1; then
    fail "unready pod accepted: $mutation"
  fi
done

printf '%s\n' "$DEPLOYMENTS_GOOD" | deployments_converged "$VERSION" || fail 'converged deployment rejected'
for mutation in \
  '.items = []' \
  '.items[0].metadata.deletionTimestamp = "2026-09-05T00:00:00Z"' \
  '.items[0].status.observedGeneration = 1' \
  'del(.items[0].status.observedGeneration)' \
  '.items[0].spec.replicas = 0' \
  '.items[0].status.replicas = 2' \
  '.items[0].status.updatedReplicas = 0' \
  '.items[0].status.readyReplicas = 0' \
  '.items[0].status.availableReplicas = 0' \
  '.items[0].status.unavailableReplicas = 1' \
  'del(.items[0].status)' \
  '.items[0].spec.template.spec.containers[0].image = "ghcr.io/witwave-ai/witself-server:1.2.2"'; do
  if printf '%s\n' "$DEPLOYMENTS_GOOD" | jq "$mutation" | deployments_converged "$VERSION" >/dev/null 2>&1; then
    fail "incomplete deployment accepted: $mutation"
  fi
done

fi

# Polling is entirely offline. A failed predicate must reach the bounded wait;
# use a distinct exit code so unexpected command failures cannot pass the test.
PRODUCTION_PAUSE_UNTIL=$(declare -f pause_until)
run_before() { shift 2; "$@"; }
pause_until() { exit 80; }
kubectl() {
  printf '%s\n' "$*" >>"$TEST_ROOT/calls"
  case "$*" in
    *'get applications.argoproj.io witself-server -o json') printf '%s\n' "${FIXTURE_ARGO:-$ARGO_GOOD}" ;;
    *"get pods -l $ALL_WORKLOAD_SELECTOR -o json")
      printf '%s\n' "$FIXTURE_PODS"
      ;;
    *"get deployments -l $ALL_WORKLOAD_SELECTOR -o json")
      printf '%s\n' "$FIXTURE_DEPLOYMENTS"
      ;;
    # Preserve Kubernetes selector behavior for pre-fix regression runs: the
    # old selector hides workers, rather than failing as an unknown shim call.
    *'get pods -l app.kubernetes.io/name=witself-server,app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_PODS" | jq '.items |= map(select(.metadata.labels["app.kubernetes.io/name"] == "witself-server"))'
      ;;
    *'get deployments -l app.kubernetes.io/name=witself-server,app.kubernetes.io/instance=witself-server -o json')
      printf '%s\n' "$FIXTURE_DEPLOYMENTS" | jq '.items |= map(select(.metadata.labels["app.kubernetes.io/name"] == "witself-server"))'
      ;;
    *) die "unexpected kubectl call: $*" ;;
  esac
}

assert_waits() {
  local label=$1 status=0
  (wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || status=$?
  [ "$status" -eq 80 ] || fail "$label did not wait for convergence (exit $status)"
  if grep -Fq VERIFIED "$TEST_ROOT/output"; then fail "$label was reported verified"; fi
}

# Use labels to make the old-selector comparison a faithful Kubernetes filter.
PODS_GOOD=$(printf '%s\n' "$PODS_GOOD" | jq '.items[].metadata.labels = {"app.kubernetes.io/name":"witself-server","app.kubernetes.io/instance":"witself-server"}')
DEPLOYMENTS_GOOD=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items[].metadata.labels = {"app.kubernetes.io/name":"witself-server","app.kubernetes.io/instance":"witself-server"}')
DEPLOYMENTS_GOOD=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items[].spec.selector.matchLabels = .items[0].metadata.labels')
PODS_BOTH=$(printf '%s\n' "$PODS_GOOD" | jq '.items += [.items[0] |
  .metadata.name = "witself-worker-fixture" |
  .metadata.labels["app.kubernetes.io/name"] = "witself-worker" |
  .metadata.labels["app.kubernetes.io/component"] = "worker" |
  .spec.containers[0].name = "witself-worker" |
  .status.containerStatuses[0].name = "witself-worker"]')
DEPLOYMENTS_BOTH=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items += [.items[0] |
  .metadata.name = "witself-worker" |
  .metadata.labels["app.kubernetes.io/name"] = "witself-worker" |
  .spec.selector.matchLabels["app.kubernetes.io/name"] = "witself-worker" |
  .spec.selector.matchLabels["app.kubernetes.io/component"] = "worker" |
  .spec.template.spec.containers[0].name = "witself-worker"]')

if [ "$READINESS_CASE" = all ] || [ "$READINESS_CASE" = core ]; then
FIXTURE_VALUES=$VALUES_DISABLED
FIXTURE_PODS=$PODS_GOOD
FIXTURE_DEPLOYMENTS=$DEPLOYMENTS_GOOD
(wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || fail 'healthy rollout did not converge'
grep -Fq 'CELL fixture VERIFIED' "$TEST_ROOT/output" || fail 'healthy rollout verification missing'

# Original reproducer: Argo still says Synced/Healthy and the tag matches,
# while the direct pod read says the replacement has entered CrashLoopBackOff.
FIXTURE_PODS=$(printf '%s\n' "$PODS_GOOD" | jq '
  .items[0].status.conditions[0].status = "False" |
  .items[0].status.containerStatuses[0].ready = false |
  .items[0].status.containerStatuses[0].state = {"waiting":{"reason":"CrashLoopBackOff"}}')
assert_waits 'stale Healthy with CrashLoopBackOff'

FIXTURE_PODS=$PODS_GOOD
FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '.items[0].status.updatedReplicas = 0')
assert_waits 'deployment with old replicas'

FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_GOOD" | jq '
  .items[0].spec.replicas = 2 | .items[0].status |=
  (.replicas = 2 | .updatedReplicas = 2 | .readyReplicas = 2 | .availableReplicas = 2)')
assert_waits 'deployment claiming more replicas than the live pod list'

FIXTURE_PODS=$(printf '%s\n' "$PODS_GOOD" | jq '.items += [(.items[0] | .metadata.name += "-extra")]')
(wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || fail 'two healthy replicas did not converge'
fi

assert_dies() {
  local label=$1 status=0
  (wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || status=$?
  [ "$status" -ne 0 ] && [ "$status" -ne 80 ] || fail "$label did not fail immediately (exit $status)"
  grep -Fq unexpected "$TEST_ROOT/output" || fail "$label failed without an unexpected-workload diagnostic"
  if grep -Fq VERIFIED "$TEST_ROOT/output"; then fail "$label was reported verified"; fi
}

assert_times_out() {
  local label=$1 status=0
  (
    # Restore the real deadline/sleep behavior only for this bounded check.
    eval "$PRODUCTION_PAUSE_UNTIL"
    ARGO_TIMEOUT=1
    wait_argo fixture witself "$FIXTURE_VALUES"
  ) >"$TEST_ROOT/output" 2>&1 || status=$?
  [ "$status" -eq 1 ] || fail "$label did not fail at its deadline (exit $status)"
  grep -Fq 'Waiting for Argo' "$TEST_ROOT/output" || fail "$label timed out without first waiting for convergence"
  grep -Fq 'timed out' "$TEST_ROOT/output" || fail "$label timeout diagnostic missing"
  if grep -Fq VERIFIED "$TEST_ROOT/output"; then fail "$label was reported verified"; fi
}

run_worker_case() {
  local test_case=$1
  FIXTURE_VALUES=$VALUES_ENABLED
  FIXTURE_PODS=$PODS_BOTH
  FIXTURE_DEPLOYMENTS=$DEPLOYMENTS_BOTH
  FIXTURE_ARGO=$ARGO_GOOD
  : >"$TEST_ROOT/calls"
  case "$test_case" in
    wrong_cell_identity)
      # An aliased kube context would return another cell's healthy Application.
      FIXTURE_ARGO=$(printf '%s\n' "$ARGO_GOOD" | jq '.metadata.labels["witself.io/cell"] = "civo-sandbox-use1-backup"')
      assert_dies 'Application labelled for another cell'
      ;;
    missing_cell_identity)
      FIXTURE_ARGO=$(printf '%s\n' "$ARGO_GOOD" | jq 'del(.metadata.labels)')
      assert_dies 'Application without a cell label'
      ;;
    worker_crashloop|worker_crashloop_timeout)
      FIXTURE_PODS=$(printf '%s\n' "$PODS_BOTH" | jq '
        .items[1].status.conditions[0].status = "False" |
        .items[1].status.containerStatuses[0].ready = false |
        .items[1].status.containerStatuses[0].state = {"waiting":{"reason":"CrashLoopBackOff"}}')
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '
        .items[1].status.readyReplicas = 0 |
        .items[1].status.availableReplicas = 0 |
        .items[1].status.unavailableReplicas = 1')
      if [ "$test_case" = worker_crashloop_timeout ]; then
        assert_times_out 'crash-looping worker with zero available replicas'
      else
        assert_waits 'crash-looping worker with zero available replicas'
      fi
      ;;
    worker_missing|worker_missing_timeout)
      FIXTURE_PODS=$PODS_GOOD
      FIXTURE_DEPLOYMENTS=$DEPLOYMENTS_GOOD
      if [ "$test_case" = worker_missing_timeout ]; then
        assert_times_out 'missing enabled worker deployment'
      else
        assert_waits 'missing enabled worker deployment'
      fi
      ;;
    worker_disabled)
      FIXTURE_VALUES=$VALUES_DISABLED
      assert_dies 'worker present while disabled in cell values'
      ;;
    both_healthy)
      (wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || fail 'healthy server and worker did not converge'
      grep -Fq 'CELL fixture VERIFIED' "$TEST_ROOT/output" || fail 'healthy server and worker verification missing'
      grep -Fq "get pods -l $ALL_WORKLOAD_SELECTOR -o json" "$TEST_ROOT/calls" || fail 'healthy server and worker pods were not both selected'
      grep -Fq "get deployments -l $ALL_WORKLOAD_SELECTOR -o json" "$TEST_ROOT/calls" || fail 'healthy server and worker deployments were not both selected'
      ;;
    surplus_server_masks_worker|surplus_worker_masks_server)
      local source_index=0
      [ "$test_case" != surplus_worker_masks_server ] || source_index=1
      FIXTURE_PODS=$(printf '%s\n' "$PODS_BOTH" | jq --argjson index "$source_index" '
        .items = [.items[$index], (.items[$index] | .metadata.name += "-extra")]')
      assert_waits "$test_case with stale converged deployment status"
      ;;
    surplus_server_masks_worker_replica)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '
        .items[].spec.replicas = 2 | .items[].status |=
          (.replicas = 2 | .updatedReplicas = 2 | .readyReplicas = 2 | .availableReplicas = 2)')
      FIXTURE_PODS=$(printf '%s\n' "$PODS_BOTH" | jq '
        .items += [(.items[0] | .metadata.name += "-extra-1"),
                   (.items[0] | .metadata.name += "-extra-2")]')
      assert_waits 'three server pods mask one missing worker replica'
      ;;
    worker_selector_mismatch)
      FIXTURE_PODS=$(printf '%s\n' "$PODS_BOTH" | jq '
        del(.items[1].metadata.labels["app.kubernetes.io/component"])')
      assert_waits 'worker pod missing a required deployment selector label'
      ;;
    unequal_healthy_replicas)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '
        .items[0].spec.replicas = 2 | .items[0].status |=
          (.replicas = 2 | .updatedReplicas = 2 | .readyReplicas = 2 | .availableReplicas = 2)')
      FIXTURE_PODS=$(printf '%s\n' "$PODS_BOTH" | jq '
        .items += [(.items[0] | .metadata.name += "-extra")]')
      (wait_argo fixture witself "$FIXTURE_VALUES") >"$TEST_ROOT/output" 2>&1 || fail 'two server pods and one worker pod did not converge'
      grep -Fq 'CELL fixture VERIFIED' "$TEST_ROOT/output" || fail 'unequal healthy replica verification missing'
      ;;
    server_duplicate|worker_duplicate)
      if [ "$test_case" = server_duplicate ]; then
        FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '.items += [.items[0]]')
      else
        FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '.items += [.items[1]]')
      fi
      assert_dies "$test_case"
      ;;
    unexpected_deployment)
      FIXTURE_DEPLOYMENTS=$(printf '%s\n' "$DEPLOYMENTS_BOTH" | jq '.items[1].metadata.name = "witself-worker-extra"')
      assert_dies 'unexpected selected deployment name'
      ;;
    *) fail "unknown readiness case: $test_case" ;;
  esac
}

if [ "$READINESS_CASE" = all ]; then
  for test_case in worker_crashloop worker_missing worker_disabled both_healthy \
    worker_crashloop_timeout worker_missing_timeout \
    surplus_server_masks_worker surplus_worker_masks_server \
    surplus_server_masks_worker_replica worker_selector_mismatch unequal_healthy_replicas \
    server_duplicate worker_duplicate unexpected_deployment \
    wrong_cell_identity missing_cell_identity; do
    run_worker_case "$test_case"
  done
elif [ "$READINESS_CASE" != core ]; then
  run_worker_case "$READINESS_CASE"
fi

printf 'roll train readiness tests passed (%s)\n' "$READINESS_CASE"
