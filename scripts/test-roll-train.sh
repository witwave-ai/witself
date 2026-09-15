#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT_RAW="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT_RAW" && pwd -P)"

fail() {
  printf 'roll train test: FAIL: %s\n' "$1" >&2
  if [ -f "$TEST_ROOT/output" ]; then cat "$TEST_ROOT/output" >&2; fi
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  find "$TEST_ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$TEST_ROOT" 2>/dev/null || true
  exit "$status"
}
trap cleanup EXIT INT TERM

command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v yq >/dev/null 2>&1 || fail 'yq is required'
ROLL_TRAIN_REAL_YQ=$(command -v yq)
export ROLL_TRAIN_REAL_YQ
FIXTURE_ROOT="$TEST_ROOT/repo"
STUB_BIN="$TEST_ROOT/bin"
STATE_DIR="$TEST_ROOT/state"
TRAIN="$FIXTURE_ROOT/scripts/roll-train.sh"
BACKUP=civo-sandbox-use1-backup
SERVING=civo-sandbox-usw2-dev
VERSION=1.2.3
ORIGINAL_PATH=$PATH
mkdir -p "$FIXTURE_ROOT/scripts" "$FIXTURE_ROOT/.git" "$STUB_BIN" "$STATE_DIR"
mkdir -p "$FIXTURE_ROOT/.gitops/charts/apps"
cp "$SOURCE_ROOT/.gitops/charts/apps/values.yaml" "$FIXTURE_ROOT/.gitops/charts/apps/values.yaml"
cp "$SOURCE_ROOT/scripts/roll-train.sh" "$TRAIN"
for cell in "$BACKUP" "$SERVING"; do
  mkdir -p "$FIXTURE_ROOT/.gitops/cells/$cell"
  cat >"$FIXTURE_ROOT/.gitops/cells/$cell/values.yaml" <<EOF_VALUES
cell:
  name: $cell
  apiHost: api.$cell.invalid
apps:
  witselfServer:
    enabled: true
    namespace: witself
    chartVersion: 1.2.2
    imageTag: 1.2.2
    worker:
      enabled: true
EOF_VALUES
done

# Every operational command is intercepted, including git commit. No test
# reaches a network, a cell, the operator's credentials, or the source index.
cat >"$STUB_BIN/git" <<'EOF_GIT'
#!/usr/bin/env bash
set -euo pipefail
printf 'git' >>"$TEST_LOG"
printf ' <%s>' "$@" >>"$TEST_LOG"
printf '\n' >>"$TEST_LOG"
cwd=$FIXTURE_ROOT
while [ "$#" -gt 0 ]; do
  case "$1" in
    -C) cwd=$2; shift 2 ;;
    -c) shift 2 ;;
    *) break ;;
  esac
done
case "$1" in
  rev-parse)
    case "$*" in
      *--show-toplevel*) printf '%s\n' "$cwd" ;;
      *--git-common-dir*) printf '%s\n' "$FIXTURE_ROOT/.git" ;;
      *HEAD^*|*origin/main*) printf 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n' ;;
      *HEAD*) printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' ;;
      *) exit 64 ;;
    esac
    ;;
  fetch|add|commit|push|branch|merge-base|switch) ;;
  check-attr)
    attr=unspecified
    [ "$SCENARIO" != unsafe_merge_driver ] || attr=union
    printf '%s: merge: %s\n' "${!#}" "$attr"
    ;;
  update-ref) [ "$SCENARIO" != cleanup_failure ] || exit 1 ;;
  status) printf ' M .gitops/cells/%s/values.yaml\n' "$(cat "$STATE_DIR/cell")" ;;
  log) printf 'Signed-off-by: Fixture Operator <operator@example.invalid>\n' ;;
  ls-remote) printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t%s\n' "$4" ;;
  show)
    path=${2#*:}
    if [ "$SCENARIO" = concurrent_pins ] && [ -f "$STATE_DIR/checks_seen" ]; then
      sed 's/1.2.2/1.2.4/g' "$FIXTURE_ROOT/$path"
    else
      cat "$FIXTURE_ROOT/$path"
    fi
    ;;
  diff)
    case "$*" in
      *--name-only*) printf '.gitops/cells/%s/values.yaml\n' "$(cat "$STATE_DIR/cell")" ;;
      *--quiet*) exit 1 ;;
      *) printf 'fixture values diff\n' ;;
    esac
    ;;
  worktree)
    case "$2" in
      add)
        # add -b BRANCH PATH BASE_OID
        path=${5}
        mkdir -p "$path/scripts"
        cp "$FIXTURE_ROOT/scripts/roll-cell.sh" "$path/scripts/roll-cell.sh"
        cp -R "$FIXTURE_ROOT/.gitops" "$path/.gitops"
        case "$SCENARIO:$path" in
          backup_chart_newer:*|backup_image_newer:*|partial_pin:*)
            field=chartVersion; pin=1.2.4
            [ "$SCENARIO" != backup_image_newer ] || field=imageTag
            [ "$SCENARIO" != partial_pin ] || pin=1.2.3
            sed "s/$field: 1.2.2/$field: $pin/" \
              "$path/.gitops/cells/civo-sandbox-use1-backup/values.yaml" >"$STATE_DIR/new-values"
            cp "$STATE_DIR/new-values" "$path/.gitops/cells/civo-sandbox-use1-backup/values.yaml"
            ;;
          serving_pins_newer:*wave-2-*)
            sed 's/1.2.2/1.2.4/g' "$path/.gitops/cells/civo-sandbox-usw2-dev/values.yaml" >"$STATE_DIR/new-values"
            cp "$STATE_DIR/new-values" "$path/.gitops/cells/civo-sandbox-usw2-dev/values.yaml"
            ;;
        esac
        printf '%s\n' "$path" >>"$STATE_DIR/worktrees"
        ;;
      remove)
        path=$3
        find "$path" -depth -mindepth 1 -delete
        rmdir "$path"
        ;;
      *) exit 64 ;;
    esac
    ;;
  config)
    case "$*" in
      *user.email*) printf 'operator@example.invalid\n' ;;
      *user.name*) printf 'Fixture Operator\n' ;;
      *) exit 64 ;;
    esac
    ;;
  *) printf 'unhandled git: %s\n' "$*" >&2; exit 64 ;;
esac
EOF_GIT

cat >"$STUB_BIN/gh" <<'EOF_GH'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh' >>"$TEST_LOG"
printf ' <%s>' "$@" >>"$TEST_LOG"
printf '\n' >>"$TEST_LOG"
case "$1 $2" in
  'auth status') ;;
  'release view')
    printf '{"tagName":"v1.2.3","isDraft":false}\n'
    ;;
  'run list')
    case "$*" in
      *release.yml*)
        if [ "$SCENARIO" = release_missing ]; then printf '[]\n'; exit 0; fi
        conclusion=success
        [ "$SCENARIO" != release_failure ] || conclusion=failure
        printf '[{"status":"completed","conclusion":"%s","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","event":"push"}]\n' "$conclusion"
        ;;
      *ci.yml*)
        if [ "$SCENARIO" = slow_ci ]; then /bin/sleep 3; fi
        conclusion=success
        oid=cccccccccccccccccccccccccccccccccccccccc
        [ "$SCENARIO" != postmerge_cancelled ] || conclusion=cancelled
        [ "$SCENARIO" != postmerge_wrong_sha ] || oid=dddddddddddddddddddddddddddddddddddddddd
        printf '[{"status":"completed","conclusion":"%s","headSha":"%s","event":"push"}]\n' "$conclusion" "$oid"
        ;;
      *) exit 64 ;;
    esac
    ;;
  'pr create')
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --head ]; then printf '%s\n' "$2" >"$STATE_DIR/branch"; break; fi
      shift
    done
    printf 'https://github.com/fixture/repo/pull/42\n'
    ;;
  'pr checks')
    touch "$STATE_DIR/checks_seen"
    if [ "$SCENARIO" = checks_transport_failure ]; then
      printf 'HTTP 503: fixture GitHub service unavailable\n' >&2
      exit 1
    fi
    if [ "$SCENARIO" = delayed_checks ] && [ ! -f "$STATE_DIR/registration_delayed" ]; then
      touch "$STATE_DIR/registration_delayed"
      printf "no checks reported on the '%s' branch\n" "$(cat "$STATE_DIR/branch")" >&2
      exit 1
    fi
    if [ "$SCENARIO" = delayed_required ] && [[ "$*" = *--required* ]] && [ ! -f "$STATE_DIR/registration_delayed" ]; then
      touch "$STATE_DIR/registration_delayed"
      printf "no required checks reported on the '%s' branch\n" "$(cat "$STATE_DIR/branch")" >&2
      exit 1
    fi
    checks='[{"name":"go","bucket":"pass","state":"SUCCESS"},{"name":"release-config","bucket":"pass","state":"SUCCESS"},{"name":"homebrew-formula","bucket":"pass","state":"SUCCESS"},{"name":"helm","bucket":"pass","state":"SUCCESS"},{"name":"avatar-renderer-portability (ubuntu-latest)","bucket":"pass","state":"SUCCESS"},{"name":"avatar-renderer-portability (ubuntu-24.04-arm)","bucket":"pass","state":"SUCCESS"}]'
    case "$SCENARIO" in
      check_failure) printf '%s\n' "$checks" | jq '.[0].bucket = "fail" | .[0].state = "FAILURE"' ;;
      missing_matrix) printf '%s\n' "$checks" | jq '.[0:5]' ;;
      required_pending)
        printf '%s\n' "$checks" | jq '.[0].bucket = "pending" | .[0].state = "PENDING"'
        exit 8
        ;;
      *) printf '%s\n' "$checks" ;;
    esac
    ;;
  'pr view')
    oid=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    if [ "$SCENARIO" = moved_head ] && [ -f "$STATE_DIR/checks_seen" ]; then
      oid=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    fi
    printf '{"baseRefName":"main","headRefOid":"%s","state":"MERGED","mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"}}\n' "$oid"
    ;;
  'pr merge') touch "$STATE_DIR/merged"; printf 'merged\n' ;;
  *) printf 'unhandled gh: %s\n' "$*" >&2; exit 64 ;;
esac
EOF_GH

cat >"$STUB_BIN/kubectl" <<'EOF_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf 'kubectl' >>"$TEST_LOG"
printf ' <%s>' "$@" >>"$TEST_LOG"
printf '\n' >>"$TEST_LOG"
version=1.2.3
case "$SCENARIO:$*" in
  backup_live_newer:*witself-civo-sandbox-use1-backup*) version=1.2.4 ;;
  serving_live_newer:*witself-civo-sandbox-usw2-dev*) version=1.2.4 ;;
  concurrent_live:*) [ ! -f "$STATE_DIR/checks_seen" ] || version=1.2.4 ;;
esac
case "$*" in
  *'get ns argocd'*) printf '{"metadata":{"name":"argocd"}}\n' ;;
  *'get application'*|*'get applications'*|*'get app '*)
    if [ "$SCENARIO" = slow_argo ] && [ -f "$STATE_DIR/merged" ]; then /bin/sleep 3; fi
    revision=$version
    [ "$SCENARIO" != argo_timeout ] || revision=1.2.2
    # Label the Application with the cell named by the kube context, as the live
    # platform chart does; the train binds every live read to that label.
    cell_label=$(printf '%s\n' "$*" | sed -nE 's/.*--context witself-([a-z0-9-]+).*/\1/p')
    printf '{"metadata":{"labels":{"witself.io/cell":"%s"}},"status":{"sync":{"status":"Synced","revision":"%s"},"health":{"status":"Healthy"}}}\n' "$cell_label" "$revision"
    ;;
  *'get pods -l app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server'*)
    if [ "$SCENARIO" = malformed_running_inventory ]; then
      printf '{"items":[{"spec":{"containers":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3"}]},"status":{"containerStatuses":false}}]}\n'
      exit
    fi
    spec_version=$version; running_version=$version
    [ "$SCENARIO" != newer_pod_spec ] || spec_version=1.2.4
    [ "$SCENARIO" != newer_running_image ] || running_version=1.2.4
    jq -n --arg spec_version "$spec_version" --arg running_version "$running_version" '{items: [
      ("witself-server", "witself-worker") as $name | {
        metadata: {name: ($name + "-pod"), labels: {
          "app.kubernetes.io/name": $name, "app.kubernetes.io/instance": "witself-server",
          "app.kubernetes.io/component": (if $name == "witself-worker" then "worker" else "server" end)
        }},
        spec: {containers: [{name: $name, image: ("ghcr.io/witwave-ai/witself-server:" + $spec_version)}]},
        status: {phase: "Running", conditions: [{type: "Ready", status: "True"}],
          containerStatuses: [{name: $name, image: ("ghcr.io/witwave-ai/witself-server:" + $running_version),
            ready: true, state: {running: {}}}]}
      }
    ]}'
    ;;
  *'get deployments -l app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server'*)
    [ "$SCENARIO" != newer_deployment ] || version=1.2.4
    jq -n --arg version "$version" '{items: [
      ("witself-server", "witself-worker") as $name | {
        metadata: {name: $name, generation: 1},
        spec: {replicas: 1, selector: {matchLabels: ({
          "app.kubernetes.io/name": $name, "app.kubernetes.io/instance": "witself-server"
        } + if $name == "witself-worker" then {"app.kubernetes.io/component": "worker"} else {} end)},
        template: {spec: {containers: [
          {name: $name, image: ("ghcr.io/witwave-ai/witself-server:" + $version)}
        ]}}},
        status: {observedGeneration: 1, replicas: 1, updatedReplicas: 1, readyReplicas: 1, availableReplicas: 1}
      }
    ]}'
    ;;
  *) printf 'unhandled kubectl: %s\n' "$*" >&2; exit 64 ;;
esac
EOF_KUBECTL

cat >"$STUB_BIN/curl" <<'EOF_CURL'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl' >>"$TEST_LOG"
printf ' <%s>' "$@" >>"$TEST_LOG"
printf '\n' >>"$TEST_LOG"
version=1.2.2
if [ -f "$STATE_DIR/cell" ] && [ "$(cat "$STATE_DIR/cell")" = civo-sandbox-usw2-dev ]; then version=1.2.3; fi
printf '{"version":"%s"}\n' "$version"
EOF_CURL

cat >"$STUB_BIN/yq" <<'EOF_YQ'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *'.cell.apiHost'*)
    file=${!#}
    if [[ "$file" = - || "$file" = .* ]]; then
      awk '/apiHost:/ { print $2; found=1 } END { if (!found) exit 65 }'
    else
      awk '/apiHost:/ { print $2; found=1 } END { if (!found) exit 65 }' "$file"
    fi
    ;;
  *'.apps.witselfServer.chartVersion'*|*'.apps.witselfServer.imageTag'*)
    field=${2##*.}
    awk -v field="$field:" '$1 == field { print $2; found=1 } END { if (!found) exit 65 }' "${!#}"
    ;;
  *'.apps.witselfServer.namespace'*) printf 'witself\n' ;;
  *'.apps.witselfServer.worker.enabled | select(tag == "!!bool") | to_string'*)
    exec "$ROLL_TRAIN_REAL_YQ" "$@"
    ;;
  *) printf 'unhandled yq: %s\n' "$*" >&2; exit 64 ;;
esac
EOF_YQ

cat >"$STUB_BIN/witself-infra" <<'EOF_INFRA'
#!/usr/bin/env bash
set -euo pipefail
printf 'witself-infra <%s> <%s>\n' "$1" "$2" >>"$TEST_LOG"
printf '{"healthy":true}\n'
EOF_INFRA

cat >"$STUB_BIN/sleep" <<'EOF_SLEEP'
#!/usr/bin/env bash
exec /bin/sleep "$@"
EOF_SLEEP

cat >"$FIXTURE_ROOT/scripts/roll-cell.sh" <<'EOF_ROLL_CELL'
#!/usr/bin/env bash
set -euo pipefail
printf 'roll-cell' >>"$TEST_LOG"
printf ' <%s>' "$@" >>"$TEST_LOG"
printf '\n' >>"$TEST_LOG"
printf '%s\n' "$1" >"$STATE_DIR/cell"
EOF_ROLL_CELL
chmod +x "$STUB_BIN/"* "$FIXTURE_ROOT/scripts/roll-cell.sh"

export FIXTURE_ROOT STATE_DIR
export PATH="$STUB_BIN:$ORIGINAL_PATH"
export TEST_LOG="$TEST_ROOT/calls"
export SCENARIO=success

reset_case() {
  find "$STATE_DIR" -depth -mindepth 1 -delete
  if [ -d "$TEST_ROOT/work" ]; then
    find "$TEST_ROOT/work" -depth -mindepth 1 -delete
    rmdir "$TEST_ROOT/work"
  fi
  : >"$TEST_LOG"
  SCENARIO=success
}

expect_failure() {
  local result=0
  bash "$TRAIN" "$@" >"$TEST_ROOT/output" 2>&1 || result=$?
  [ "$result" -ne 0 ] || fail "expected refusal: $*"
}

assert_no_wave() {
  if grep -Eq '^(roll-cell|git.*<(worktree|commit|push)>|gh <pr> <(create|merge)>)' "$TEST_LOG"; then
    fail 'refusal performed a wave mutation'
  fi
}

for scenario in backup_chart_newer backup_image_newer partial_pin backup_live_newer \
  newer_pod_spec newer_running_image newer_deployment malformed_running_inventory unsafe_merge_driver; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
  grep -Eq 'strictly lower|newer than|standard text merge|invalid live workload' "$TEST_ROOT/output" || fail "$scenario missed version/merge guard"
  if grep -Eq '^(roll-cell|git <(add|commit|push)>|gh <pr> <(create|merge)>)' "$TEST_LOG"; then
    fail "$scenario edited pins or published a wave"
  fi
done
printf 'roll train test: per-cell desired and live downgrade guards stop before pin edits\n'

for scenario in serving_pins_newer serving_live_newer; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
  [ "$(grep -Fc 'gh <pr> <merge>' "$TEST_LOG")" -eq 1 ] || fail "$scenario did not verify backup first"
  if grep -Fq "roll-cell <$SERVING>" "$TEST_LOG"; then fail "$scenario overwrote a newer serving cell"; fi
  grep -Eq 'strictly lower|newer than' "$TEST_ROOT/output" || fail "$scenario missed the wave-two version guard"
done
printf 'roll train test: serving advancement between waves stops before serving pin edits\n'

for scenario in concurrent_pins concurrent_live; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
  [ -f "$STATE_DIR/checks_seen" ] || fail "$scenario did not reach PR checks"
  grep -Eq 'desired values changed|newer than' "$TEST_ROOT/output" || fail "$scenario missed pre-merge revalidation"
  if grep -Fq 'gh <pr> <merge>' "$TEST_LOG"; then fail "$scenario merged over concurrent advancement"; fi
done
printf 'roll train test: concurrent desired and live advancement during checks stops before merge\n'

for args in '' 'v1.2.3' '1.2' '1.2.3 --unknown' '1.2.3 --cells' '1.2.3 --cells one' \
  '1.2.3 --cells one,one' '1.2.3 --cells ../escape,serving' '1.2.3 --workdir' \
  '1.2.3 --serving-url' '1.2.3 --ci-timeout 0' '1.2.3 --argo-timeout -1' \
  '1.2.3 --poll-interval nope' '1.2.3 --backup-evidence'; do
  reset_case
  # Inputs are fixed test tokens, deliberately split into argv.
  # shellcheck disable=SC2086
  expect_failure $args
  assert_no_wave
done
printf 'roll train test: usage refusals passed\n'

reset_case
bash "$TRAIN" "$VERSION" --no-schema-change --dry-run --workdir "$TEST_ROOT/work" >"$TEST_ROOT/output" 2>&1 \
  || fail 'dry run failed'
grep -Fq "$BACKUP" "$TEST_ROOT/output" || fail 'dry run omitted backup wave'
grep -Fq "$SERVING" "$TEST_ROOT/output" || fail 'dry run omitted serving wave'
grep -Fq "$VERSION" "$TEST_ROOT/output" || fail 'dry run omitted target version'
[ ! -e "$TEST_ROOT/work" ] || fail 'dry run created its work directory'
[ ! -e "$STATE_DIR/worktrees" ] || fail 'dry run created a worktree'
if grep -Eq '^(gh|kubectl|curl|roll-cell|witself-infra)|git.*<(fetch|worktree|commit|push|add|branch)>' "$TEST_LOG"; then
  fail 'dry run invoked an operational command'
fi
printf 'roll train test: dry run is read-only and prints both waves\n'

reset_case
SCENARIO=release_failure
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
grep -Eiq 'release.*(success|green|fail)|release.yml' "$TEST_ROOT/output" || fail 'release refusal lacks clear message'
assert_no_wave
printf 'roll train test: failed release run stops before wave mutations\n'

reset_case
SCENARIO=release_missing
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
assert_no_wave
printf 'roll train test: missing release run fails closed\n'

reset_case
SCENARIO=moved_head
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --poll-interval 1
grep -Eiq 'head.*(mov|chang|match)|mov.*head' "$TEST_ROOT/output" || fail 'moved head refusal lacks clear message'
if grep -Fq 'gh <pr> <merge>' "$TEST_LOG"; then fail 'moved head reached merge'; fi
[ -f "$STATE_DIR/worktrees" ] || fail 'moved head fixture never reached a worktree'
while IFS= read -r path; do [ -d "$path" ] || fail 'failed wave worktree was removed'; done <"$STATE_DIR/worktrees"
if grep -Fq "roll-cell <$SERVING>" "$TEST_LOG"; then fail 'moved backup head started serving wave'; fi
printf 'roll train test: moved head stops before merge and retains worktree\n'

reset_case
SCENARIO=check_failure
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --poll-interval 1
if grep -Fq 'gh <pr> <merge>' "$TEST_LOG"; then fail 'failed required check reached merge'; fi
grep -Eiq '(check|CI).*(fail|success|green)|fail.*check' "$TEST_ROOT/output" || fail 'check refusal lacks clear message'
printf 'roll train test: failed required check stops before merge\n'

for scenario in required_pending missing_matrix; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --ci-timeout 1 --poll-interval 1
  grep -Fq 'timed out' "$TEST_ROOT/output" || fail "$scenario did not honor the CI deadline"
  if grep -Fq 'gh <pr> <merge>' "$TEST_LOG"; then fail "$scenario reached merge"; fi
done
printf 'roll train test: pending and missing matrix checks time out before merge\n'

reset_case
SCENARIO=checks_transport_failure
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --ci-timeout 1 --poll-interval 1
if grep -Fq 'gh <pr> <merge>' "$TEST_LOG"; then fail 'checks transport failure reached merge'; fi
if grep -Fq 'timed out' "$TEST_ROOT/output"; then fail 'checks transport failure was retried as pending'; fi
grep -Fq 'gh exit 1' "$TEST_ROOT/output" || fail 'checks transport failure lost the CLI error'
printf 'roll train test: checks transport failure stops immediately\n'

for scenario in postmerge_cancelled postmerge_wrong_sha argo_timeout; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --argo-timeout 1 --poll-interval 1
  [ "$(grep -Fc 'gh <pr> <merge>' "$TEST_LOG")" -eq 1 ] || fail "$scenario did not stop after the first merge"
  if grep -Fq "roll-cell <$SERVING>" "$TEST_LOG"; then fail "$scenario started serving wave"; fi
  while IFS= read -r path; do [ -d "$path" ] || fail "$scenario removed its failed worktree"; done <"$STATE_DIR/worktrees"
  if [ "$scenario" = argo_timeout ]; then
    grep -Fq 'Argo convergence' "$TEST_ROOT/output" || fail 'Argo timeout lacks convergence context'
    grep -Fq 'timed out' "$TEST_ROOT/output" || fail 'Argo did not honor convergence deadline'
  else
    grep -Fq 'post-merge CI failed' "$TEST_ROOT/output" || fail "$scenario lacks a post-merge failure message"
  fi
done
printf 'roll train test: cancelled or unrelated merge CI and Argo timeout block serving wave\n'

# A provider returning success after the deadline must still fail. Real sleep
# also exercises the watchdog for a read-only provider call that is hung.
# Allow the preceding PR checks two seconds so the three-second provider
# delay exercises the intended post-merge watchdog under normal test load.
for scenario in slow_ci slow_argo; do
  reset_case
  SCENARIO=$scenario
  expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" \
    --ci-timeout 2 --argo-timeout 1 --poll-interval 1
  grep -Fq 'timed out' "$TEST_ROOT/output" || fail "$scenario accepted a success after its deadline"
  [ "$(grep -Fc 'gh <pr> <merge>' "$TEST_LOG")" -eq 1 ] || fail "$scenario did not reach the intended post-merge deadline"
  if grep -Fq "roll-cell <$SERVING>" "$TEST_LOG"; then fail "$scenario started serving wave"; fi
  while IFS= read -r path; do [ -d "$path" ] || fail "$scenario removed its failed worktree"; done <"$STATE_DIR/worktrees"
done
printf 'roll train test: slow GitHub and kubectl success cannot bypass polling deadlines\n'

reset_case
SCENARIO=cleanup_failure
expect_failure "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work"
grep -Fq '<switch> <--detach> <aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>' "$TEST_LOG" \
  || fail 'cleanup did not detach the verified worktree at its exact head'
grep -Fq '<update-ref> <-d>' "$TEST_LOG" || fail 'cleanup failure fixture never reached local ref deletion'
if grep -Fq '<worktree> <remove>' "$TEST_LOG"; then fail 'failed branch cleanup removed the worktree'; fi
if grep -Fq "roll-cell <$SERVING>" "$TEST_LOG"; then fail 'failed backup cleanup started serving wave'; fi
while IFS= read -r path; do [ -d "$path" ] || fail 'failed cleanup did not retain its worktree'; done <"$STATE_DIR/worktrees"
printf 'roll train test: branch cleanup failure retains worktree and stops the train\n'

# macOS Bash 3.2 drops function-local variables before an EXIT trap when a
# sourced subshell helper exits. Check the direct call with nounset enabled,
# outside the main train's dynamic scope, and preserve gh's pending exit code.
mkdir -p "$TEST_ROOT/direct-watchdog"
if ! /bin/bash -u -s -- "$TRAIN" "$TEST_ROOT/direct-watchdog" \
  >"$TEST_ROOT/direct-watchdog.stdout" 2>"$TEST_ROOT/direct-watchdog.stderr" <<'EOF_WATCHDOG'
source "$1"
RUN_DIR=$2
result=0
run_before "$((SECONDS + 10))" 'fixture pending command' \
  /bin/bash -c 'printf "pending fixture\n"; exit 8' || result=$?
[ "$result" -eq 8 ] || { printf 'pending exit code became %s\n' "$result" >&2; exit 1; }
EOF_WATCHDOG
then
  cat "$TEST_ROOT/direct-watchdog.stderr" >&2
  fail 'direct watchdog helper failed to preserve pending exit code'
fi
[ "$(cat "$TEST_ROOT/direct-watchdog.stdout")" = 'pending fixture' ] || fail 'direct watchdog lost command stdout'
[ ! -s "$TEST_ROOT/direct-watchdog.stderr" ] || fail 'direct watchdog emitted a trap or unbound variable error'
printf 'roll train test: sourced watchdog preserves pending exit code and stdout under nounset\n'

# Import in an isolated shell so the script's helper names cannot overwrite
# this harness. Importing the script must not start a train.
run_predicate() {
  bash -c 'source "$1"; "$2" "$3"' _ "$TRAIN" "$1" "$VERSION"
}
argo_good='{"status":{"sync":{"status":"Synced","revision":"1.2.3"},"health":{"status":"Healthy"}}}'
printf '%s\n' "$argo_good" | run_predicate argo_converged || fail 'converged Argo fixture refused'
for bad in '{}' '{"status":{"sync":{"status":"OutOfSync","revision":"1.2.3"},"health":{"status":"Healthy"}}}' \
  '{"status":{"sync":{"status":"Synced","revision":"1.2.2"},"health":{"status":"Healthy"}}}' \
  '{"status":{"sync":{"status":"Synced","revision":"1.2.3"},"health":{"status":"Progressing"}}}' \
  '{"status":{"sync":{"status":"Synced","revision":"1.2.3"}}}' 'invalid JSON'; do
  if printf '%s\n' "$bad" | run_predicate argo_converged >/dev/null 2>&1; then fail 'nonconverged Argo fixture accepted'; fi
done
printf 'roll train test: Argo sync, health, revision, and malformed JSON predicates passed\n'

pods_good='{"items":[{"spec":{"containers":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"witself-server","image":"ghcr.io/witwave-ai/witself-server:1.2.3","ready":true,"state":{"running":{}}}]}}]}'
printf '%s\n' "$pods_good" | run_predicate pods_converged || fail 'matching pod images refused'
for bad in '{}' '{"items":[]}' '{"items":[{"spec":{"containers":[]}}]}' \
  '{"items":[{"spec":{"containers":[{"image":"ghcr.io/witwave-ai/witself-server:1.2.2"}]}}]}' \
  '{"items":[{"spec":{"containers":[{"image":"ghcr.io/witwave-ai/witself-server:11.2.3"}]}}]}' \
  '{"items":[{"spec":{"containers":[{"image":"ghcr.io/witwave-ai/witself-server:1.2.3"}]}},{"spec":{"containers":[{"image":"ghcr.io/witwave-ai/witself-server:1.2.2"}]}}]}' \
  'invalid JSON'; do
  if printf '%s\n' "$bad" | run_predicate pods_converged >/dev/null 2>&1; then fail 'nonconverged pod fixture accepted'; fi
done
printf 'roll train test: pod images require nonempty exact-version matches\n'

reset_case
bash "$TRAIN" "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --poll-interval 1 >"$TEST_ROOT/output" 2>&1 \
  || fail 'offline two-wave train failed'
[ "$(grep -Fc 'gh <pr> <merge>' "$TEST_LOG")" -eq 2 ] || fail 'train did not merge exactly two waves'
[ "$(grep -Fc '<--match-head-commit> <aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>' "$TEST_LOG")" -eq 2 ] \
  || fail 'merges were not fenced to the approved OID'
[ "$(grep -Fc '<--workflow> <ci.yml>' "$TEST_LOG")" -eq 2 ] || fail 'post-merge CI was not verified for both waves'
grep -Fq '<--commit> <cccccccccccccccccccccccccccccccccccccccc>' "$TEST_LOG" || fail 'post-merge CI was not pinned to merge OID'
awk -v serving="$SERVING" '
  /^gh <pr> <merge>/ { merged++ }
  /^kubectl/ && /<get> <deployments>/ && merged > 0 { verified++ }
  /^roll-cell/ && index($0, "<" serving ">") { if (verified < 1) exit 1; serving_seen=1 }
  END { if (!serving_seen) exit 1 }
' "$TEST_LOG" || fail 'serving wave preceded backup pod verification'
while IFS= read -r path; do [ ! -d "$path" ] || fail 'successful wave retained its worktree'; done <"$STATE_DIR/worktrees"
grep -Fq '"version":"1.2.3"' "$TEST_ROOT/output" || fail 'train omitted final serving version'
grep -Fq 'witself-infra <health> <--json>' "$TEST_LOG" || fail 'available infra health binary was not invoked'
if grep -Eq '<(--force|--force-with-lease|-f)>' "$TEST_LOG"; then fail 'train used force'; fi
printf 'roll train test: offline two-wave success, merge fences, CI, order, cleanup, and health passed\n'

reset_case
mkdir -p "$TEST_ROOT/evidence backup" "$TEST_ROOT/evidence serving"
(
  cd "$TEST_ROOT"
  bash "$TRAIN" "$VERSION" --backup-evidence 'evidence backup' --backup-evidence 'evidence serving' \
    --workdir relative-work --poll-interval 1
) >"$TEST_ROOT/output" 2>&1 || fail 'backup evidence train failed'
for cell in "$BACKUP" "$SERVING"; do
  grep -Fq "roll-cell <$cell> <$VERSION> <--backup-evidence> <$TEST_ROOT/evidence backup> <--backup-evidence> <$TEST_ROOT/evidence serving>" "$TEST_LOG" \
    || fail 'backup evidence paths were not forwarded intact to both waves'
done
[ -d "$TEST_ROOT/relative-work" ] || fail 'relative --workdir was not resolved from invocation directory'
if grep -Fq '<--no-schema-change>' "$TEST_LOG"; then fail 'evidence path introduced a schema attestation'; fi
printf 'roll train test: evidence paths with spaces reach both gates; relative workdir preserved\n'

for scenario in delayed_checks delayed_required; do
  reset_case
  SCENARIO=$scenario
  bash "$TRAIN" "$VERSION" --no-schema-change --workdir "$TEST_ROOT/work" --poll-interval 1 \
    >"$TEST_ROOT/output" 2>&1 || fail "$scenario was not treated as pending registration"
  [ -f "$STATE_DIR/registration_delayed" ] || fail "$scenario fixture was not exercised"
  [ "$(grep -Fc 'gh <pr> <checks>' "$TEST_LOG")" -ge 6 ] || fail "$scenario skipped rechecking before merge"
  [ "$(grep -Fc 'gh <pr> <merge>' "$TEST_LOG")" -eq 2 ] || fail "$scenario did not complete both waves"
done
printf 'roll train test: initial missing checks and required checks wait for registration\n'
PATH="$ORIGINAL_PATH" bash "$SOURCE_ROOT/scripts/test-roll-train-evidence.sh"
PATH="$ORIGINAL_PATH" bash "$SOURCE_ROOT/scripts/test-roll-train-readiness.sh"
PATH="$ORIGINAL_PATH" bash "$SOURCE_ROOT/scripts/test-roll-train-values.sh"
PATH="$ORIGINAL_PATH" bash "$SOURCE_ROOT/scripts/test-roll-train-downgrade.sh"
PATH="$ORIGINAL_PATH" bash "$SOURCE_ROOT/scripts/test-roll-train-merge-fence.sh"
printf 'roll train tests passed\n'
