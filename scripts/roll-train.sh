#!/usr/bin/env bash
# Local operator train: GitHub PRs supply desired state; kubectl proves each wave.
# Functions are sourceable so the convergence predicates can be tested offline.

usage() {
  cat <<'EOF'
usage: scripts/roll-train.sh VERSION [options]
  --no-schema-change       Attest that this release cannot advance the DB schema
  --backup-evidence DIR    Forward verified backup artifacts (repeat up to twice)
  --cells BACKUP,SERVING   Full cell directory names, in wave order
                          Default: civo-sandbox-use1-backup,civo-sandbox-usw2-dev
  --serving-url URL        Serving HTTP(S) origin (default: https://cell.apiHost)
  --workdir DIR            Parent for isolated run/worktree directories
                          Default: $(git rev-parse --git-common-dir)/../.roll-train
  --ci-timeout SECONDS     Deadline per PR checks / post-merge CI phase (3600)
  --argo-timeout SECONDS   Deadline per cell convergence phase (1200)
  --poll-interval SECONDS  Poll interval (15)
  --dry-run               Print the plan; no writes or network calls
  --help                  Show this help

VERSION must be MAJOR.MINOR.PATCH without v. A real roll requires either
--no-schema-change or --backup-evidence; these options are mutually exclusive.
Backup evidence supports only the default --cells pair, in its default order.
Uses configured Git identity and Signed-off-by, never force-pushes, and leaves
the current worktree and PR for inspection on failure. No automatic resume.
EOF
}

die() { printf 'roll-train: ERROR: %s\n' "$*" >&2; exit 1; }
usage_error() { usage >&2; printf 'roll-train: %s\n' "$*" >&2; exit 2; }
log() { printf 'roll-train: %s\n' "$*"; }

valid_version() { [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

# Compare decimal components as strings: no shell integer overflow or sort -V
# dependency (the latter is unavailable on stock macOS).
version_lower() {
  local left=$1 right=$2 a b
  valid_version "$left" && valid_version "$right" || return 1
  while :; do
    a=${left%%.*}; b=${right%%.*}
    if [ "${#a}" -ne "${#b}" ]; then [ "${#a}" -lt "${#b}" ]; return; fi
    if [ "$a" != "$b" ]; then [[ "$a" < "$b" ]]; return; fi
    [[ "$left" == *.* ]] || return 1
    left=${left#*.}; right=${right#*.}
  done
}

argo_converged() {
  jq -e --arg version "$1" '
    .status.sync.status == "Synced" and .status.health.status == "Healthy" and
    .status.sync.revision == $version
  ' >/dev/null
}

# The kube context name is operator configuration. Bind every live read to the
# Application's own cell label so an aliased or misconfigured context can never
# certify another cell's healthy workloads as this cell.
require_app_cell_identity() {
  local cell=$1 app=$2 observed
  observed=$(printf '%s\n' "$app" | jq -r '.metadata.labels["witself.io/cell"] // ""')
  [ "$observed" = "$cell" ] ||
    die "Argo Application reached through context witself-$cell identifies cell '${observed:-<none>}', expected $cell: unexpected cell identity"
}

pods_converged() {
  jq -e --arg suffix ":$1" '
    (.items | type == "array" and length > 0) and
    all(.items[];
      .metadata.deletionTimestamp == null and .status.phase == "Running" and
      ([.status.conditions[]? | select(.type == "Ready")] |
        length == 1 and all(.[]; .status == "True")) and
      (.spec.containers | type == "array" and length > 0) and
      all(.spec.containers[];
        (.name | type == "string" and length > 0) and
        (.image | type == "string") and (.image | endswith($suffix))) and
      (.status.containerStatuses | type == "array") and
      ([.spec.containers[].name] | sort) ==
        ([.status.containerStatuses[].name] | sort) and
      all(.status.containerStatuses[];
        .ready == true and (.state.running | type == "object") and
        .state.waiting == null and .state.terminated == null and
        (.image | type == "string") and (.image | endswith($suffix))))
  ' >/dev/null
}

deployments_converged() {
  jq -e --arg suffix ":$1" '
    (.items | type == "array" and length > 0) and
    all(.items[];
      .metadata.deletionTimestamp == null and
      (.metadata.generation | type == "number" and . > 0) and
      .status.observedGeneration == .metadata.generation and
      (.spec.replicas | type == "number" and . > 0 and . == floor) and
      .status.replicas == .spec.replicas and
      .status.updatedReplicas == .spec.replicas and
      .status.readyReplicas == .spec.replicas and
      .status.availableReplicas == .spec.replicas and
      (.status.unavailableReplicas // 0) == 0 and
      (.spec.template.spec.containers | type == "array" and length > 0) and
      all(.spec.template.spec.containers[];
        (.image | type == "string") and (.image | endswith($suffix))))
  ' >/dev/null
}

# The chart uses nonempty matchLabels selectors for both workloads. Count each
# pod against its Deployment's complete selector, including the worker component
# label, so surplus replicas cannot hide an absent or undersized workload.
pods_match_deployment_replicas() {
  jq -e --argjson deployments "$1" '
    def matches($pod; $deployment):
      all($deployment.spec.selector.matchLabels | to_entries[];
        $pod.metadata.labels[.key] == .value);
    .items as $pods |
    all($deployments.items[];
      (.spec.selector.matchLabels | type == "object" and length > 0 and
        all(.[]; type == "string")) and
      ((.spec.selector.matchExpressions // []) | length == 0)) and
    all($pods[]; . as $pod |
      [$deployments.items[] | select(matches($pod; .))] | length == 1) and
    all($deployments.items[]; . as $deployment |
      ([$pods[] | select(matches(.; $deployment))] | length) == .spec.replicas)
  ' >/dev/null
}

on_exit() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    printf 'roll-train: stopped during %s (exit %s).\n' "$PHASE" "$status" >&2
    if [ -n "$CURRENT_WT" ]; then
      printf 'Inspect worktree: %s\nBranch: %s\nPR: %s\n' \
        "$CURRENT_WT" "$CURRENT_BRANCH" "${CURRENT_PR:-not created}" >&2
    fi
    [ -z "$RUN_DIR" ] || printf 'Run artifacts retained: %s\n' "$RUN_DIR" >&2
  fi
}

pause_until() {
  local deadline=$1 label=$2 remaining delay
  remaining=$((deadline - SECONDS))
  [ "$remaining" -gt 0 ] || die "$label timed out"
  delay=$POLL_INTERVAL
  [ "$delay" -le "$remaining" ] || delay=$remaining
  sleep "$delay"
  [ "$SECONDS" -lt "$deadline" ] || die "$label timed out"
}

# Bound read-only polling commands too: gh has no default HTTP timeout. A
# subshell isolates traps, and a file captures stdout so an orphaned credential
# helper cannot hold a command-substitution pipe open past the deadline.
run_before() (
  # These belong to this subshell, not function-local scope: Bash 3.2 unwinds
  # locals before executing EXIT traps, which must still see the child PIDs.
  deadline=$1 label=$2 remaining='' output='' command_pid='' timer_pid='' status=0
  shift 2
  remaining=$((deadline - SECONDS))
  [ "$remaining" -gt 0 ] || die "$label timed out"
  output=$(mktemp "$RUN_DIR/poll.XXXXXX") || exit 1
  stop_poll() {
    if [ -n "$command_pid" ]; then
      kill -KILL "$command_pid" 2>/dev/null || :
      wait "$command_pid" 2>/dev/null || :
    fi
    if [ -n "$timer_pid" ]; then
      kill -TERM "$timer_pid" 2>/dev/null || :
      wait "$timer_pid" 2>/dev/null || :
    fi
  }
  trap stop_poll EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "$@" >"$output" &
  command_pid=$!
  (
    sleeper=''
    trap 'if [ -n "$sleeper" ]; then kill "$sleeper" 2>/dev/null || :; wait "$sleeper" 2>/dev/null || :; fi; exit' TERM INT
    sleep "$remaining" &
    sleeper=$!
    wait "$sleeper" || exit
    : >"$output.timeout"
    kill -KILL "$command_pid" 2>/dev/null || :
  ) >/dev/null 2>&1 &
  timer_pid=$!
  wait "$command_pid" 2>/dev/null || status=$?
  command_pid=''
  stop_poll
  timer_pid=''
  if [ -f "$output.timeout" ] || [ "$SECONDS" -ge "$deadline" ]; then
    die "$label timed out (poll output retained at $output)"
  fi
  cat "$output" || exit 1
  rm "$output" || exit 1
  exit "$status"
)

read_checks() {
  local deadline=$1 status=0 error_file="$RUN_DIR/checks-last.stderr" error
  shift
  run_before "$deadline" "PR checks ($CURRENT_PR)" gh pr checks "$CURRENT_PR" "$@" \
    --json name,bucket,state 2>"$error_file" || status=$?
  error=$(cat "$error_file")
  # GitHub needs time to register checks on a new PR. gh returns 1 before
  # JSON export for these two specific empty-result errors. Match the exact
  # branch and message; authentication/transport errors still fail immediately.
  if [ "$status" -eq 1 ] && {
    [ "$error" = "no checks reported on the '$CURRENT_BRANCH' branch" ] ||
    [ "$error" = "no required checks reported on the '$CURRENT_BRANCH' branch" ];
  }; then
    printf '[]\n'
    return
  fi
  [ -z "$error" ] || printf '%s\n' "$error" >&2
  # gh uses 8 for pending; other nonzero results (including transport errors)
  # stop the train. Never turn a failed request into an empty/successful list.
  [ "$status" -eq 0 ] || [ "$status" -eq 8 ] || die "PR checks failed for $CURRENT_PR (gh exit $status)"
}

wait_pr_checks() {
  local deadline=$((SECONDS + CI_TIMEOUT)) checks required
  while :; do
    checks=$(read_checks "$deadline")
    required=$(read_checks "$deadline" --required)
    printf '%s\n' "$checks" | jq -e 'type == "array" and all(.[];
      .bucket == "pass" or .bucket == "pending" or .bucket == "skipping")' >/dev/null ||
      die "PR checks failed, cancelled, or returned an unknown state: $CURRENT_PR"
    printf '%s\n' "$required" | jq -e 'type == "array" and all(.[];
      .bucket == "pass" or .bucket == "pending")' >/dev/null ||
      die "required PR check failed, skipped, or cancelled: $CURRENT_PR"
    # Do not accept an empty/partial list while GitHub is registering jobs.
    # Preserve the known required-check floor as well as any new ruleset checks.
    if printf '%s\n' "$checks" | jq -e '
      . as $checks | ["go", "release-config", "homebrew-formula", "helm",
        "avatar-renderer-portability (ubuntu-latest)",
        "avatar-renderer-portability (ubuntu-24.04-arm)"] |
      all(.[]; . as $name |
        [$checks[] | select(.name == $name)] |
        length > 0 and all(.[]; .bucket == "pass"))
      ' >/dev/null && printf '%s\n' "$required" | jq -e \
        'length > 0 and all(.[]; .bucket == "pass")' >/dev/null; then
      [ "$SECONDS" -lt "$deadline" ] || die "PR checks ($CURRENT_PR) timed out"
      return
    fi
    log "Waiting for required PR checks: $CURRENT_PR"
    pause_until "$deadline" "PR checks ($CURRENT_PR)"
  done
}

wait_merge_ci() {
  local oid=$1 deadline=$((SECONDS + CI_TIMEOUT)) runs
  while :; do
    runs=$(run_before "$deadline" "post-merge CI ($oid)" gh run list \
      --workflow ci.yml --branch main --event push --commit "$oid" \
      --limit 1 --json headSha,status,conclusion)
    printf '%s\n' "$runs" | jq -e --arg oid "$oid" '
      type == "array" and all(.[]; .headSha == $oid and
        (.status == "queued" or .status == "in_progress" or
         .status == "waiting" or .status == "pending" or .status == "requested" or
         (.status == "completed" and .conclusion == "success")))
      ' >/dev/null || die "post-merge CI failed or returned an unexpected run for $oid"
    if printf '%s\n' "$runs" | jq -e \
      'length == 1 and .[0].status == "completed" and .[0].conclusion == "success"' >/dev/null; then
      [ "$SECONDS" -lt "$deadline" ] || die "post-merge CI ($oid) timed out"
      log "Post-merge CI verified: $oid"
      return
    fi
    log "Waiting for post-merge CI: $oid"
    pause_until "$deadline" "post-merge CI ($oid)"
  done
}

cell_worker_enabled() {
  local values=$1 enabled defaults
  # Resolve defaults from this cell's worktree, then overlay its partial values
  # just as the apps chart does. Validate before stringifying so false passes
  # yq -e while strings such as "false" remain invalid.
  defaults="$(dirname "$values")/../../charts/apps/values.yaml"
  # shellcheck disable=SC2016 # $item is a yq variable.
  enabled=$(yq ea -er '
    . as $item ireduce ({}; . * $item) |
    .apps.witselfServer.worker.enabled | select(tag == "!!bool") | to_string
  ' "$defaults" "$values") || die "cannot read boolean worker.enabled from apps defaults and $values"
  case "$enabled" in
    true|false) printf '%s\n' "$enabled" ;;
    *) die "invalid worker.enabled in $values: expected true or false" ;;
  esac
}

# Unexpected or duplicate deployments are errors. Missing expected workloads
# return false so callers can either wait for them or refuse the version guard.
expected_deployments_present() {
  local cell=$1 worker_enabled=$2 deployments=$3
  printf '%s\n' "$deployments" | jq -e --argjson worker "$worker_enabled" '
    (.items | type == "array") and
    ([.items[].metadata.name] |
      all(.[]; . == "witself-server" or ($worker and . == "witself-worker")) and
      ([.[] | select(. == "witself-server")] | length <= 1) and
      ([.[] | select(. == "witself-worker")] | length <= 1))
  ' >/dev/null || die "unexpected deployments for $cell (worker.enabled=$worker_enabled)"
  printf '%s\n' "$deployments" | jq -e --argjson worker "$worker_enabled" '
    ([.items[].metadata.name] | sort) ==
      (if $worker then ["witself-server", "witself-worker"] else ["witself-server"] end)
  ' >/dev/null
}

wait_argo() {
  local cell=$1 namespace=$2 values=$3 deadline=$((SECONDS + ARGO_TIMEOUT)) app pods deployments worker_enabled
  worker_enabled=$(cell_worker_enabled "$values") || die "cannot determine expected workloads for $cell"
  while :; do
    app=$(run_before "$deadline" "Argo convergence ($cell)" \
      kubectl --context "witself-$cell" --request-timeout=20s -n argocd \
      get applications.argoproj.io witself-server -o json)
    pods=$(run_before "$deadline" "Argo convergence ($cell)" \
      kubectl --context "witself-$cell" --request-timeout=20s -n "$namespace" \
      get pods -l 'app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server' -o json)
    deployments=$(run_before "$deadline" "Argo convergence ($cell)" \
      kubectl --context "witself-$cell" --request-timeout=20s -n "$namespace" \
      get deployments -l 'app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server' -o json)
    # Malformed responses are failures, not convergence delays.
    printf '%s\n' "$app" | jq -e 'type == "object"' >/dev/null || die "invalid Argo JSON for $cell"
    require_app_cell_identity "$cell" "$app"
    printf '%s\n' "$pods" | jq -e '.items | type == "array"' >/dev/null || die "invalid pod JSON for $cell"
    printf '%s\n' "$deployments" | jq -e '.items | type == "array"' >/dev/null || die "invalid deployment JSON for $cell"
    if expected_deployments_present "$cell" "$worker_enabled" "$deployments" &&
       printf '%s\n' "$app" | argo_converged "$VERSION" &&
       printf '%s\n' "$pods" | pods_converged "$VERSION" &&
       printf '%s\n' "$deployments" | deployments_converged "$VERSION" &&
       printf '%s\n' "$pods" | pods_match_deployment_replicas "$deployments" &&
       [ "$(printf '%s\n' "$pods" | jq '.items | length')" = \
         "$(printf '%s\n' "$deployments" | jq '[.items[].spec.replicas] | add')" ]; then
      [ "$SECONDS" -lt "$deadline" ] || die "Argo convergence ($cell) timed out"
      log "CELL $cell VERIFIED: Synced Healthy revision $VERSION; ready pods and deployment replicas; pod images:"
      printf '%s\n' "$pods" | jq -r '.items[].spec.containers[].image' | sort -u
      return
    fi
    log "Waiting for Argo, ready server/worker pods, and expected deployment replicas on $cell at $VERSION"
    pause_until "$deadline" "Argo convergence ($cell)"
  done
}

# Both pins must change in this PR. Besides refusing downgrades, this keeps a
# late concurrent upgrade in conflict with our patch at GitHub's three-way
# squash merge. An unchanged pin would leave that field outside the fence.
require_upgrade_pins() {
  local cell=$1 values=$2 field current
  for field in chartVersion imageTag; do
    current=$(yq -er ".apps.witselfServer.$field" "$values")
    version_lower "$current" "$VERSION" ||
      die "$cell desired $field '$current' must be strictly lower than $VERSION; inspect already or partly pinned cells manually"
  done
}

require_live_not_newer() {
  local cell=$1 namespace=$2 values=$3 app pods deployments current images image worker_enabled
  worker_enabled=$(cell_worker_enabled "$values") || die "cannot determine expected workloads for $cell"
  app=$(kubectl --context "witself-$cell" --request-timeout=20s -n argocd \
    get applications.argoproj.io witself-server -o json)
  printf '%s\n' "$app" | jq -e 'type == "object"' >/dev/null || die "invalid Argo JSON for $cell"
  require_app_cell_identity "$cell" "$app"
  current=$(printf '%s\n' "$app" | jq -er '.status.sync.revision | select(type == "string")')
  if ! valid_version "$current" || version_lower "$VERSION" "$current"; then
    die "$cell live Argo revision '$current' is invalid or newer than $VERSION"
  fi
  pods=$(kubectl --context "witself-$cell" --request-timeout=20s -n "$namespace" \
    get pods -l 'app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server' -o json)
  deployments=$(kubectl --context "witself-$cell" --request-timeout=20s -n "$namespace" \
    get deployments -l 'app.kubernetes.io/name in (witself-server,witself-worker),app.kubernetes.io/instance=witself-server' -o json)
  expected_deployments_present "$cell" "$worker_enabled" "$deployments" ||
    die "missing expected deployment for $cell (worker.enabled=$worker_enabled)"
  # Check both requested and running images: a pod can still run a newer
  # image while its spec already requests a lower one. Pending containers may
  # lack status, but their requested images are still checked.
  images=$(printf '%s\n%s\n' "$pods" "$deployments" | jq -ers '
    def containers: type == "array" and length > 0 and
      all(.[]; .image | type == "string");
    if length == 2 and
      (.[0].items | type == "array" and length > 0 and all(.[];
        (.spec.containers | containers) and
        ((.status.containerStatuses | if . == null then [] else . end) | type == "array" and
          all(.[]; .image | type == "string")))) and
      (.[1].items | type == "array" and length > 0 and all(.[];
        .spec.template.spec.containers | containers))
    then .[0].items[] | (.spec.containers[], .status.containerStatuses[]?) | .image
    else error("invalid live workload image inventory") end,
    (.[1].items[].spec.template.spec.containers[].image)
  ') || die "invalid live workload images for $cell"
  while IFS= read -r image; do
    current=${image##*:}
    if [[ "$image" != *:* || "$image" == *@* ]] ||
      ! valid_version "$current" || version_lower "$VERSION" "$current"; then
      die "$cell live image '$image' is invalid or newer than $VERSION"
    fi
  done <<<"$images"
}

roll_wave() {
  local cell=$1 wave=$2 values namespace title body head view merge_oid changed base baseline latest merge_attr
  PHASE="wave $wave ($cell): worktree creation"
  CURRENT_BRANCH="roll-train/$RUN_ID/wave-$wave-$cell"
  CURRENT_WT="$RUN_DIR/wave-$wave-$cell"
  CURRENT_PR=
  git fetch origin main
  base=$(git rev-parse origin/main)
  [[ "$base" =~ ^[0-9a-f]{40}$ ]] || die "invalid base OID"
  git worktree add -b "$CURRENT_BRANCH" "$CURRENT_WT" "$base"
  cd "$CURRENT_WT"
  values=".gitops/cells/$cell/values.yaml"
  baseline=$(cat "$values")
  namespace=$(yq -er '.apps.witselfServer.namespace' "$values")
  [[ "$namespace" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "invalid server namespace for $cell"
  PHASE="wave $wave ($cell): version guards"
  merge_attr=$(git check-attr merge -- "$values")
  case "$merge_attr" in
    "$values: merge: unspecified"|"$values: merge: text") ;;
    *) die "standard text merge required to fence concurrent pin changes for $cell" ;;
  esac
  require_upgrade_pins "$cell" "$values"
  require_live_not_newer "$cell" "$namespace" "$values"
  PHASE="wave $wave ($cell): backup gate and commit"
  if [ "${#GATE_ARGS[@]}" -gt 0 ]; then
    bash scripts/roll-cell.sh "$cell" "$VERSION" "${GATE_ARGS[@]}"
  else
    die "rollout gate required"
  fi
  # Stage only the intended values file. A helper changing anything else fails
  # before push instead of silently including unrelated changes in a roll PR.
  changed=$(git status --porcelain --untracked-files=all)
  [ "$changed" = " M $values" ] || die "expected only $values to change; inspect $CURRENT_WT"
  git add -- "$values"
  title="Roll $cell to v$VERSION (wave $wave)"
  git commit -s -m "$title"
  head=$(git rev-parse HEAD)
  [[ "$head" =~ ^[0-9a-f]{40}$ ]] || die "invalid local head OID"
  [ "$(git rev-parse HEAD^)" = "$base" ] || die "wave commit parent moved from the validated base"
  CURRENT_HEAD=$head
  PHASE="wave $wave ($cell): push and PR"
  git push -u origin "$CURRENT_BRANCH"
  body="$RUN_DIR/wave-$wave-pr.md"
  # Literal Markdown backticks in these printf formats are intentional.
  # shellcheck disable=SC2016
  {
    printf 'Wave %s: roll `%s` to `v%s`.\n\n' "$wave" "$cell" "$VERSION"
    printf '%s\n\n' "$SCHEMA_STATEMENT"
    if [ "$wave" -eq 2 ]; then
      printf 'Wave 1 (`%s`) has passed post-merge CI and Argo Synced/Healthy at revision `%s`, with ready server and enabled worker pods, converged expected deployment replicas, and every selected container image on that version.\n\n' "$BACKUP_CELL" "$VERSION"
    fi
    printf 'Local operator roll via `scripts/roll-train.sh`; tracked in #342.\n'
  } >"$body"
  CURRENT_PR=$(gh pr create --base main --head "$CURRENT_BRANCH" --title "$title" --body-file "$body")
  [[ "$CURRENT_PR" =~ ^https://[^[:space:]]+/pull/[0-9]+$ ]] || die "unexpected PR create response"
  log "Wave $wave PR: $CURRENT_PR"
  PHASE="wave $wave ($cell): PR checks"
  wait_pr_checks
  view=$(gh pr view "$CURRENT_PR" --json headRefOid,baseRefName)
  [ "$(printf '%s\n' "$view" | jq -er '.headRefOid')" = "$head" ] || die "PR head moved before merge: $CURRENT_PR"
  [ "$(printf '%s\n' "$view" | jq -er '.baseRefName')" = main ] || die "PR base moved before merge: $CURRENT_PR"
  git fetch origin main
  latest=$(git show "origin/main:$values")
  [ "$latest" = "$baseline" ] || die "$cell desired values changed before merge; inspect $CURRENT_PR"
  require_live_not_newer "$cell" "$namespace" "$values"
  PHASE="wave $wave ($cell): squash merge"
  # Preserve the configured operator's DCO sign-off in the squash commit too.
  git log -1 --format=%b >"$RUN_DIR/wave-$wave-merge.txt"
  gh pr merge "$CURRENT_PR" --squash --match-head-commit "$head" \
    --subject "$title" --body-file "$RUN_DIR/wave-$wave-merge.txt"
  view=$(gh pr view "$CURRENT_PR" --json state,mergeCommit,headRefOid)
  printf '%s\n' "$view" | jq -e --arg head "$head" \
    '.state == "MERGED" and .headRefOid == $head' >/dev/null || die "PR is not merged at the checked head"
  merge_oid=$(printf '%s\n' "$view" | jq -er '.mergeCommit.oid')
  [[ "$merge_oid" =~ ^[0-9a-f]{40}$ ]] || die "missing squash merge OID"
  PHASE="wave $wave ($cell): post-merge CI"
  wait_merge_ci "$merge_oid"
  PHASE="wave $wave ($cell): Argo convergence"
  wait_argo "$cell" "$namespace" "$values"
}

cleanup_wave() {
  local remote_head
  PHASE="verified wave cleanup"
  cd "$REPO_ROOT"
  remote_head=$(git ls-remote --heads origin "refs/heads/$CURRENT_BRANCH")
  # GitHub may already have removed the merged branch by repository policy.
  if [ -n "$remote_head" ]; then
    [ "$remote_head" = "$(printf '%s\trefs/heads/%s' "$CURRENT_HEAD" "$CURRENT_BRANCH")" ] ||
      die "remote branch moved after merge; refusing cleanup"
    git push origin --delete "$CURRENT_BRANCH"
  fi
  # Squash merging does not make the original commit an ancestor of main.
  # Detach before deleting only this run's exact local ref. Remove the worktree
  # last, so even a failed ref deletion leaves a checkout for inspection.
  git -C "$CURRENT_WT" switch --detach "$CURRENT_HEAD"
  git update-ref -d "refs/heads/$CURRENT_BRANCH" "$CURRENT_HEAD"
  git worktree remove "$CURRENT_WT"
  CURRENT_WT='' CURRENT_BRANCH='' CURRENT_PR='' CURRENT_HEAD=''
}

main() {
  set -euo pipefail
  export LC_ALL=C
  VERSION='' NO_SCHEMA_CHANGE=false DRY_RUN=false SERVING_URL='' WORKDIR=''
  CI_TIMEOUT=3600 ARGO_TIMEOUT=1200 POLL_INTERVAL=15
  local cells=civo-sandbox-use1-backup,civo-sandbox-usw2-dev option value evidence
  local common release runs host live current tool cell values
  local evidence_dirs=()
  GATE_ARGS=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --help) usage; return ;;
      --dry-run) DRY_RUN=true; shift ;;
      --no-schema-change) NO_SCHEMA_CHANGE=true; shift ;;
      --cells|--serving-url|--workdir|--ci-timeout|--argo-timeout|--poll-interval|--backup-evidence)
        option=$1
        [ "$#" -ge 2 ] || usage_error "$option requires a value"
        value=$2
        case "$value" in ''|-*) usage_error "$option requires a non-option value" ;; esac
        case "$option" in
          --cells) cells=$value ;;
          --serving-url) SERVING_URL=$value ;;
          --workdir) WORKDIR=$value ;;
          --ci-timeout|--argo-timeout|--poll-interval)
            [[ "$value" =~ ^[1-9][0-9]{0,5}$ ]] || usage_error "$option requires positive seconds (at most 999999)"
            case "$option" in
              --ci-timeout) CI_TIMEOUT=$value ;;
              --argo-timeout) ARGO_TIMEOUT=$value ;;
              --poll-interval) POLL_INTERVAL=$value ;;
            esac ;;
          --backup-evidence)
            [ "${#evidence_dirs[@]}" -lt 2 ] || usage_error "--backup-evidence may be specified at most twice"
            evidence_dirs+=("$value") ;;
        esac
        shift 2 ;;
      -*) usage_error "unknown option: $1" ;;
      *) [ -z "$VERSION" ] || usage_error "unexpected argument: $1"; VERSION=$1; shift ;;
    esac
  done
  valid_version "$VERSION" || usage_error "VERSION must be MAJOR.MINOR.PATCH without v or leading zeroes"
  [[ "$cells" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?,[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    usage_error "--cells requires two full cell directory names: BACKUP,SERVING"
  BACKUP_CELL=${cells%,*}; SERVING_CELL=${cells#*,}
  [ "$BACKUP_CELL" != "$SERVING_CELL" ] || usage_error "backup and serving cells must differ"
  if [ "$NO_SCHEMA_CHANGE" = true ]; then
    [ "${#evidence_dirs[@]}" -eq 0 ] || usage_error "--no-schema-change and --backup-evidence are mutually exclusive"
    GATE_ARGS=(--no-schema-change)
    SCHEMA_STATEMENT="Schema attestation: operator explicitly attests that release $VERSION cannot advance the database schema (--no-schema-change)."
  else
    # roll-cell's verifier requires the closed ReviewedCells pair and does not
    # receive the selected cells. Evidence for that pair cannot cover another
    # database, so reject unsupported selections before any operational call.
    if [ "${#evidence_dirs[@]}" -gt 0 ]; then
      [ "$cells" = civo-sandbox-use1-backup,civo-sandbox-usw2-dev ] ||
        usage_error "--backup-evidence requires --cells civo-sandbox-use1-backup,civo-sandbox-usw2-dev; verifier coverage does not support other cell pairs"
    fi
    SCHEMA_STATEMENT="Schema attestation: none; roll-cell.sh must verify backup/restore evidence for both reviewed cells before editing either pin in each wave."
  fi
  if [ -n "$SERVING_URL" ]; then
    SERVING_URL=${SERVING_URL%/}
    [[ "$SERVING_URL" =~ ^https?://[a-zA-Z0-9.-]+(:[0-9]+)?$ ]] || usage_error "--serving-url must be an HTTP(S) origin without credentials, path, query, or fragment"
  fi
  REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && git rev-parse --show-toplevel)
  common=$(git -C "$REPO_ROOT" rev-parse --git-common-dir)
  case "$common" in /*) ;; *) common="$REPO_ROOT/$common" ;; esac
  WORKDIR=${WORKDIR:-$common/../.roll-train}
  case "$WORKDIR" in /*) ;; *) WORKDIR="$PWD/$WORKDIR" ;; esac
  if [ "$DRY_RUN" = true ]; then
    cat <<EOF
Dry run: local two-wave roll to v$VERSION
Workdir: $WORKDIR (unique run directory; primary checkout may be dirty)
Preconditions: gh auth status; both witself-CELL contexts reach argocd (20s request timeout);
published release v$VERSION; latest release.yml run for v$VERSION completed success;
serving /v1/version strictly lower than $VERSION.
Serving origin: ${SERVING_URL:-https://<origin/main .gitops/cells/$SERVING_CELL/values.yaml cell.apiHost>}
$SCHEMA_STATEMENT
Backup evidence directories supplied: ${#evidence_dirs[@]}
Wave 1: $BACKUP_CELL (context witself-$BACKUP_CELL)
Wave 2: $SERVING_CELL (context witself-$SERVING_CELL), only after wave 1 verified.
Each wave: fetch origin/main; isolated worktree at exact base; both desired pins strictly lower;
live Argo and workload versions no newer; roll-cell.sh; signed-off commit;
push branch; create PR; require all required checks to pass (${CI_TIMEOUT}s);
verify head OID/main base; recheck unchanged cell values and live versions;
squash merge --match-head-commit (both pin edits conflict with concurrent upgrades);
verify exact post-merge CI (${CI_TIMEOUT}s); Argo Synced + Healthy + revision $VERSION;
ready, nonterminating Running server pods and ready containers with images ending :$VERSION;
observed deployment generation and all replicas updated/ready/available (${ARGO_TIMEOUT}s);
remove verified worktree/branch. Poll interval: ${POLL_INTERVAL}s.
Finally: print serving /v1/version; witself-infra health --json if available.
Any failure stops the train and retains the current worktree for inspection.
EOF
    return
  fi
  PHASE=preconditions CURRENT_WT='' CURRENT_BRANCH='' CURRENT_PR='' CURRENT_HEAD='' RUN_DIR=''
  trap on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  for tool in git gh jq yq kubectl curl; do command -v "$tool" >/dev/null || die "$tool is required"; done
  if [ "$NO_SCHEMA_CHANGE" = false ]; then
    [ "${#evidence_dirs[@]}" -gt 0 ] || die "rollout gate required: provide --backup-evidence directories or attest --no-schema-change"
    # Resolve relative evidence paths before entering wave worktrees.
    for evidence in "${evidence_dirs[@]}"; do
      evidence=$(cd "$evidence" && pwd -P)
      GATE_ARGS+=(--backup-evidence "$evidence")
    done
  fi
  cd "$REPO_ROOT"
  gh auth status
  release=$(gh release view "v$VERSION" --json tagName,isDraft)
  printf '%s\n' "$release" | jq -e --arg tag "v$VERSION" \
    '.tagName == $tag and .isDraft == false' >/dev/null || die "published release v$VERSION does not exist"
  runs=$(gh run list --workflow release.yml --branch "v$VERSION" --limit 1 --json status,conclusion,headSha,event)
  printf '%s\n' "$runs" | jq -e \
    'length == 1 and .[0].status == "completed" and .[0].conclusion == "success" and .[0].event == "push"' \
    >/dev/null || die "release run is not green for v$VERSION (latest release.yml push run must be completed success)"
  for cell in "$BACKUP_CELL" "$SERVING_CELL"; do
    kubectl --context "witself-$cell" --request-timeout=20s get ns argocd >/dev/null
  done
  git fetch origin main
  for cell in "$BACKUP_CELL" "$SERVING_CELL"; do
    values=$(git show "origin/main:.gitops/cells/$cell/values.yaml")
    if [ "$cell" = "$SERVING_CELL" ] && [ -z "$SERVING_URL" ]; then
      host=$(printf '%s\n' "$values" | yq -er '.cell.apiHost')
      [[ "$host" =~ ^[a-zA-Z0-9.-]+$ ]] || die "invalid cell.apiHost; supply --serving-url"
      SERVING_URL="https://$host"
    fi
  done
  live=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 20 "$SERVING_URL/v1/version")
  current=$(printf '%s\n' "$live" | jq -er '.version | select(type == "string")')
  version_lower "$current" "$VERSION" || die "serving version '$current' must be strictly lower than $VERSION"
  log "Preconditions verified; serving version $current -> $VERSION"
  umask 077
  mkdir -p "$WORKDIR"
  RUN_DIR=$(mktemp -d "$WORKDIR/train-$VERSION.XXXXXX")
  RUN_DIR=$(cd "$RUN_DIR" && pwd -P)
  RUN_ID=${RUN_DIR##*/}
  roll_wave "$BACKUP_CELL" 1
  cleanup_wave
  roll_wave "$SERVING_CELL" 2
  PHASE="final serving verification"
  live=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 20 "$SERVING_URL/v1/version")
  log "Serving /v1/version: $live"
  printf '%s\n' "$live" | jq -e --arg version "$VERSION" '.version == $version' >/dev/null ||
    die "serving /v1/version does not report $VERSION"
  if command -v witself-infra >/dev/null 2>&1; then
    witself-infra health --json
  else
    log "witself-infra absent; optional health report skipped"
  fi
  cleanup_wave
  log "Both waves verified at $VERSION. Run record: $RUN_DIR"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
