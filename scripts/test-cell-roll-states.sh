#!/usr/bin/env bash
# Run cell-derived contracts against real writer output, without credentials,
# Git mutations, or live cell access. Only the verified test chart may be fetched.
set -euo pipefail

mode=contracts
states=(committed serving-backup serving-postgres all-pins unpinned)
# Repeated selectors allow the same subset to exercise sequential and parallel runs.
selected_states=()
while [[ "$#" -gt 0 ]]; do
case "$1" in
  --state)
    [[ "$#" -ge 2 ]] || exit 2
    case "$2" in
      committed|serving-backup|serving-postgres|all-pins|unpinned) selected_states+=("$2") ;;
      *) printf 'unknown roll state\n' >&2; exit 2 ;;
    esac
    shift 2
    ;;
  --monitoring-only)
    [[ "$#" -eq 1 && ${#selected_states[@]} -eq 0 ]] || exit 2
    mode=monitoring
    : "${WITSELF_TEST_MONITORING_CHART:?supply the verified monitoring chart}"
    : "${WITSELF_TEST_PROMTOOL:?supply promtool}"
    shift
    ;;
  *) printf 'usage: %s [--monitoring-only | --state STATE ...]\n' "$0" >&2; exit 2 ;;
esac
done
if [[ ${#selected_states[@]} -gt 0 ]]; then
  # Filter, deduplicate and replay in canonical order, regardless of selector order.
  canonical_states=("${states[@]}")
  states=()
  for state in "${canonical_states[@]}"; do
    for selected in "${selected_states[@]}"; do
      if [[ "$state" == "$selected" ]]; then
        states+=("$state")
        break
      fi
    done
  done
fi
jobs=${WITSELF_ROLL_STATES_JOBS:-1}
if [[ ! "$jobs" =~ ^[1-9][0-9]*$ ]]; then
  printf 'WITSELF_ROLL_STATES_JOBS must be a positive integer\n' >&2
  exit 2
fi
# No more workers than states; avoid arithmetic overflow for a large job setting.
if [[ ${#jobs} -gt 1 ]] || [[ "$jobs" -gt ${#states[@]} ]]; then
  jobs=${#states[@]}
fi

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$source_root/scripts/lib/test-cell-roll-states.sh"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-cell-roll-states.XXXXXX")
pids=()
cleanup() {
  local pid
  if [[ ${#pids[@]} -gt 0 ]]; then
    for pid in "${pids[@]}"; do
      kill -- "-$pid" 2>/dev/null || true
    done
    for pid in "${pids[@]}"; do
      wait "$pid" 2>/dev/null || true
    done
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# A source snapshot includes uncommitted test changes but neither .git nor local
# operator state. All generated cell edits happen only under this temporary root.
python3 - "$source_root" "$work_dir/source" <<'PY'
import pathlib
import shutil
import subprocess
import sys
source, target = map(pathlib.Path, sys.argv[1:])
target.mkdir()
paths = subprocess.check_output(
    ["git", "-C", str(source), "ls-files", "-z", "--cached", "--others", "--exclude-standard"]
).decode().split("\0")
for name in paths:
    path = pathlib.Path(name)
    if (not name or path.name.endswith((".token", "-report.md"))
            or path.name in ("tokens", ".credentials.yaml")
            or path.parts[:2] == (".gitops", "secrets")):
        continue
    if not (source / path).is_file():
        continue
    (target / path).parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source / path, target / path)
PY
(cd "$work_dir/source" && go build -o "$work_dir/generator" ./internal/cmd/gitops-cell-values)
# Fetch once before starting workers. A failed verification/download is fatal.
# The committed self-check and monitoring mode do not need this child chart.
if [[ "$mode" == contracts && "${WITSELF_TEST_SKIP_POSTGRESQL_CHART:-}" != 1 ]]; then
  for state in "${states[@]}"; do
    if [[ "$state" != committed ]]; then
      if ! WITSELF_TEST_POSTGRESQL_CHART=$(bash "$source_root/scripts/fetch-test-postgresql-chart.sh" 2>"$work_dir/chart-fetch.log"); then
        ruby -e 'STDOUT.binmode; print STDIN.binmode.read.gsub(/[a-fA-F0-9]{64}/n, "<digest-redacted>")' <"$work_dir/chart-fetch.log" >&2
        printf 'cell roll states: PostgreSQL child chart fetch failed\n' >&2
        exit 1
      fi
      cat "$work_dir/chart-fetch.log" >&2
      export WITSELF_TEST_POSTGRESQL_CHART
      break
    fi
  done
fi

run_check() {
  local name=$1
  shift
  if (cd "$fixture" && "$@") >"$state_dir/check.log" 2>&1; then
    printf 'cell roll states: %s / %s: PASS\n' "$state" "$name"
  else
    # Digests are irrelevant to diagnosing a fixture failure; never emit them.
    ruby -e 'STDOUT.binmode; print STDIN.binmode.read.gsub(/[a-fA-F0-9]{64}/n, "<digest-redacted>")' <"$state_dir/check.log" >&2
    printf 'cell roll states: %s / %s: FAIL\n' "$state" "$name" >&2
    failures=$((failures + 1))
  fi
}

run_state() (
  # Descendants stay in the process group assigned to this background worker.
  set +m
  state=$1
  state_dir="$work_dir/$state"
  fixture="$state_dir/fixture"
  failures=0
  start=$SECONDS
  # All subprocess scratch space, test homes and Helm state belong to this worker.
  export TMPDIR="$state_dir/tmp" WITSELF_HOME="$state_dir/homes/witself" DSH_HOME="$state_dir/homes/dsh"
  export HELM_REGISTRY_CONFIG="$state_dir/helm/registry.json"
  export HELM_CACHE_HOME="$state_dir/helm/cache" HELM_CONFIG_HOME="$state_dir/helm/config" HELM_DATA_HOME="$state_dir/helm/data"
  mkdir -p "$TMPDIR" "$WITSELF_HOME" "$DSH_HOME" "$HELM_CACHE_HOME" "$HELM_CONFIG_HOME" "$HELM_DATA_HOME"
  cp -R "$work_dir/source" "$fixture"
  if apply_roll_state "$fixture" "$work_dir/generator" "$state" >"$state_dir/setup.log" 2>&1; then
    printf 'cell roll states: %s / setup: PASS\n' "$state"
  else
    ruby -e 'STDOUT.binmode; print STDIN.binmode.read.gsub(/[a-fA-F0-9]{64}/n, "<digest-redacted>")' <"$state_dir/setup.log" >&2
    printf 'cell roll states: %s / setup: FAIL\n' "$state" >&2
    printf '1\n' >"$state_dir/failures"
    rm -rf "$fixture"
    exit 0
  fi
  if [[ "$mode" == monitoring ]]; then
    scratch="$state_dir/monitoring"
    mkdir -p "$scratch"
    run_check monitoring-extensions ruby scripts/testdata/monitoring-extensions.rb \
      "$fixture" "$scratch" "$WITSELF_TEST_MONITORING_CHART"
    for suite in test-monitoring-postgres-backup test-monitoring-memory-alerts; do
      run_check "$suite" ruby "scripts/testdata/$suite.rb" \
        "$fixture" "$scratch" "$WITSELF_TEST_MONITORING_CHART" "$WITSELF_TEST_PROMTOOL"
    done
    run_check test-monitoring-recovery ruby scripts/testdata/test-monitoring-recovery.rb \
      "$fixture" "$WITSELF_TEST_MONITORING_CHART"
  elif [[ "$state" == committed ]]; then
    # Surrounding CI/Make targets already run the suites on committed values.
    run_check generation-check bash scripts/gitops-cell-values.sh --check
  else
    # A suite belongs in this matrix iff it reads committed .gitops/cells/ values
    # or the generator's output. Excluded invariant/duplicate suites:
    # - test-roll-train: reads only five synthetic non-Civo cells, untouched here.
    # - test-roll-train-evidence: only synthetic train fixtures, untouched here.
    # - test-postgres-backup: already run by test-helm-rollout in every state.
    # - test-agent-email-receipt-proof: reads no cells or generator output.
    # - internal/backupevidence: reads no cells or generator output.
    # - internal/client: reads no cells or generator output.
    run_check go-tests go test -count=1 ./internal/gitopsvalues/... ./internal/gitopscheck/... \
      ./internal/cmd/gitops-cell-values/...
    for suite in test-helm-rollout test-roll-cell-gate test-gitops-cell-values; do
      run_check "$suite" bash "scripts/$suite.sh"
    done
    for suite in test-monitoring-memory-alerts test-monitoring-collector-alerts; do
      run_check "$suite" ruby "scripts/testdata/$suite.rb" "$fixture"
    done
    if [[ "${WITSELF_TEST_SKIP_POSTGRESQL_CHART:-}" == 1 ]]; then
      printf 'cell roll states: NOT RUN test-civo-postgres-chart (WITSELF_TEST_SKIP_POSTGRESQL_CHART=1) — this run is not a full gate [%s]\n' "$state" >&2
    else
      run_check test-civo-postgres-chart bash scripts/test-civo-postgres-chart.sh
    fi
    run_check generation-check bash scripts/gitops-cell-values.sh --check
  fi
  printf '%s\n' "$failures" >"$state_dir/failures"
  printf 'cell roll states: %s / elapsed: %ss\n' "$state" "$((SECONDS - start))"
  rm -rf "$fixture"
)

# Background subshells keep worker variables private. Waiting for the oldest
# active worker supports macOS Bash without wait -n or any external scheduler.
pids=()
worker_states=()
failures=0
wait_worker() {
  local pid=$1 state=$2
  if ! wait "$pid"; then
    printf 'cell roll states: %s / runner: FAIL\n' "$state" >>"$work_dir/$state/state.log"
    failures=$((failures + 1))
  fi
}
replay_log() {
  # The merged worker logs stay ordered; opt-out warnings must remain on stderr.
  ruby -e '
    [STDIN, STDOUT, STDERR].each(&:binmode)
    STDOUT.sync = true
    STDIN.each_line do |line|
      line = line.gsub(/[a-fA-F0-9]{64}/n, "<digest-redacted>")
      stream = line.start_with?("cell roll states: NOT RUN") ? STDERR : STDOUT
      stream.print(line)
    end
  ' <"$1"
}
# Separate process groups let cancellation stop each worker and its children.
set -m
for state in "${states[@]}"; do
  mkdir -p "$work_dir/$state"
  run_state "$state" >"$work_dir/$state/state.log" 2>&1 &
  pids+=("$!")
  worker_states+=("$state")
  if [[ ${#pids[@]} -ge "$jobs" ]]; then
    wait_worker "${pids[0]}" "${worker_states[0]}"
    if [[ "$jobs" -eq 1 ]]; then
      replay_log "$work_dir/${worker_states[0]}/state.log"
    fi
    pids=("${pids[@]:1}")
    worker_states=("${worker_states[@]:1}")
  fi
done
while [[ ${#pids[@]} -gt 0 ]]; do
  wait_worker "${pids[0]}" "${worker_states[0]}"
  pids=("${pids[@]:1}")
  worker_states=("${worker_states[@]:1}")
done
set +m
for state in "${states[@]}"; do
  if [[ "$jobs" -gt 1 ]]; then
    replay_log "$work_dir/$state/state.log"
  fi
  if [[ -f "$work_dir/$state/failures" ]]; then
    count=$(cat "$work_dir/$state/failures")
    failures=$((failures + count))
  fi
done
if [[ "$failures" -ne 0 ]]; then
  printf 'cell roll states: %s checks failed\n' "$failures" >&2
  exit 1
fi
printf 'cell roll states: all selected states passed\n'
