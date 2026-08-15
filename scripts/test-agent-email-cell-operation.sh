#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-agent-email-operation-test.XXXXXX")"
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
chmod 700 "$work_dir"
work_dir="$(cd "$work_dir" && pwd -P)"
mkdir -m 700 "$work_dir/bin" "$work_dir/output" "$work_dir/state"

file_mode() {
  local path="$1"
  local mode
  mode="$(stat -f '%Lp' "$path" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$path" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

cat >"$work_dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$#" -gt 0 ]; do
  case "$1" in
    --request-timeout=*) shift ;;
    --kubeconfig|--context|-n) shift 2 ;;
    *) break ;;
  esac
done
case "${1:-} ${2:-}" in
  "get deployment")
    deployment_count_file="$FAKE_KUBE_STATE/deployment-get-count"
    deployment_count=$(($(cat "$deployment_count_file" 2>/dev/null || printf 0) + 1))
    printf '%s\n' "$deployment_count" >"$deployment_count_file"
    filter='.'
    if [ "${FAKE_STALE_READINESS:-false}" = true ]; then
      filter+=' | .status.readyReplicas = 1 | .status.unavailableReplicas = 1'
    fi
    if [ "${FAKE_SOURCE_DRIFT:-}" = deployment ] && [ "$deployment_count" -ge 2 ]; then
      filter+=' | .metadata.resourceVersion = "deployment-rv-drift" | .spec.template.spec.containers[0].image = "ghcr.io/witwave-ai/images/witself-server:0.0.242"'
    fi
    jq "$filter" "$FAKE_KUBE_STATE/deployment.json"
    ;;
  "get configmap")
    config_count_file="$FAKE_KUBE_STATE/config-get-count"
    config_count=$(($(cat "$config_count_file" 2>/dev/null || printf 0) + 1))
    printf '%s\n' "$config_count" >"$config_count_file"
    if [ "${FAKE_SOURCE_DRIFT:-}" = config ] && [ "$config_count" -ge 2 ]; then
      jq '.metadata.resourceVersion = "config-rv-drift"' "$FAKE_KUBE_STATE/config.json"
    else
      cat "$FAKE_KUBE_STATE/config.json"
    fi
    ;;
  "get secret")
    case "${3:-}" in
      receive-cohort-v1) printf 'cohort-uid\ncohort-rv\n%s\n' "${FAKE_COHORT_IMMUTABLE:-true}" ;;
      retry-canary-v1)
        retry_count_file="$FAKE_KUBE_STATE/retry-secret-get-count"
        retry_count=$(($(cat "$retry_count_file" 2>/dev/null || printf 0) + 1))
        printf '%s\n' "$retry_count" >"$retry_count_file"
        retry_rv=retry-rv
        if [ "${FAKE_SOURCE_DRIFT:-}" = retry-secret ] && [ "$retry_count" -ge 2 ]; then
          retry_rv=retry-rv-drift
        fi
        printf 'retry-uid\n%s\n%s\n' "$retry_rv" "${FAKE_RETRY_CANARY_IMMUTABLE:-true}"
        ;;
      witself-db) printf 'database-uid\ndatabase-rv\n' ;;
      *) exit 1 ;;
    esac
    ;;
  "get pods")
    if [ -f "$FAKE_KUBE_STATE/job-deleted" ]; then
      printf '%s\n' pods-absent >>"$FAKE_KUBE_STATE/cleanup-actions.log"
      printf '%s\n' '{"items":[]}'
    else
      jq --argjson runner_exit "${FAKE_RUNNER_EXIT:-0}" \
        '(.items[0].status.containerStatuses[] | select(.name == "runner") |
         .state.terminated.exitCode) = $runner_exit' "$FAKE_KUBE_STATE/pods.json"
    fi
    ;;
  "get pod")
    jq --argjson runner_exit "${FAKE_RUNNER_EXIT:-0}" \
      '(.items[0].status.containerStatuses[] | select(.name == "runner") |
       .state.terminated.exitCode) = $runner_exit | .items[0]' "$FAKE_KUBE_STATE/pods.json"
    ;;
  "create -f")
    payload="$(cat)"
    kind="$(jq -r '.kind' <<<"$payload")"
    case "$kind" in
      ConfigMap)
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/lock-created.json"
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/lock.json"
        ;;
      Job)
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/job-created.json"
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/job.json"
        ;;
      Secret)
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/secret-created.json"
        printf '%s\n' "$payload" >"$FAKE_KUBE_STATE/secret.json"
        ;;
      *) exit 1 ;;
    esac
    ;;
  "create secret")
    [ "${3:-}" = generic ] || exit 1
    printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"witself-agent-email-operation-overrides"},"data":{"overrides.json":"cmVkYWN0ZWQ="}}'
    ;;
  "exec witself-agent-email-operation-pod")
    printf '%s\n' "$*" >>"$FAKE_KUBE_STATE/exec-actions.log"
    if [[ " $* " == *" artifact-helper ready "* ]]; then
      [ "${FAKE_EXEC_READY_FAILURE:-false}" != true ]
      exit
    fi
    if [[ " $* " == *" artifact-helper complete "* ]]; then
      [ "${FAKE_COMPLETE_ERROR:-false}" != true ]
      exit
    fi
    if [[ " $* " == *" artifact-helper exists --name "* ]]; then
      if [ "${FAKE_ARTIFACT_INSPECTION_ERROR:-false}" = true ]; then
        exit 1
      fi
      [[ " $* " == *" --name ${FAKE_PRIVATE_ARTIFACT_KEY:-primary-canary} "* ]] && exit 0
      exit 3
    fi
    if [[ " $* " == *" artifact-helper export --name ${FAKE_PRIVATE_ARTIFACT_KEY:-primary-canary} "* ]]; then
      cat "$FAKE_PRIVATE_ARTIFACT"
      exit 0
    fi
    exit 1
    ;;
  "logs witself-agent-email-operation-pod")
    [ "$#" -eq 6 ] || exit 1
    [ "$3" = -c ] || exit 1
    [ "$4" = runner ] || exit 1
    [ "$5" = --tail=20 ] || exit 1
    [ "$6" = --limit-bytes=8192 ] || exit 1
    [ "${FAKE_RUNNER_LOG_FAILURE:-false}" != true ] || exit 1
    printf '%s\n' "${FAKE_RUNNER_LOG:-}"
    ;;
  "delete job")
    printf '%s\n' "$*" >>"$FAKE_KUBE_STATE/deletes.log"
    printf '%s\n' delete-job >>"$FAKE_KUBE_STATE/cleanup-actions.log"
    if [ "${FAKE_BACKGROUND_DELETE:-false}" != true ]; then
      rm -f "$FAKE_KUBE_STATE/job.json"
      : >"$FAKE_KUBE_STATE/job-deleted"
    fi
    ;;
  "delete secret")
    printf '%s\n' "$*" >>"$FAKE_KUBE_STATE/deletes.log"
    printf '%s\n' delete-secret >>"$FAKE_KUBE_STATE/cleanup-actions.log"
    rm -f "$FAKE_KUBE_STATE/secret.json"
    ;;
  "delete configmap")
    printf '%s\n' "$*" >>"$FAKE_KUBE_STATE/deletes.log"
    printf '%s\n' delete-configmap >>"$FAKE_KUBE_STATE/cleanup-actions.log"
    rm -f "$FAKE_KUBE_STATE/lock.json"
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod 700 "$work_dir/bin/kubectl"

cat >"$work_dir/state/deployment.json" <<'EOF'
{
  "apiVersion":"apps/v1","kind":"Deployment",
  "metadata":{"name":"witself-server","uid":"deployment-uid","resourceVersion":"deployment-rv","generation":4},
  "status":{"observedGeneration":4,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2,"unavailableReplicas":0},
  "spec":{"replicas":2,"template":{
    "metadata":{"annotations":{"checksum/config":"legacy-checksum","witself.io/server-config-checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
    "spec":{
    "serviceAccountName":"witself-server","automountServiceAccountToken":false,
    "containers":[{"name":"witself-server",
      "image":"ghcr.io/witwave-ai/images/witself-server:0.0.245",
      "imagePullPolicy":"IfNotPresent",
      "envFrom":[{"configMapRef":{"name":"witself-server"}}],
      "env":[
        {"name":"WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS","valueFrom":{"secretKeyRef":{"name":"receive-cohort-v1","key":"account_ids"}}},
        {"name":"WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID","valueFrom":{"secretKeyRef":{"name":"retry-canary-v1","key":"agent_id"}}},
        {"name":"WITSELF_DATABASE_URL","valueFrom":{"secretKeyRef":{"name":"witself-db","key":"dsn"}}},
        {"name":"WITSELF_PROVISION_TOKEN","valueFrom":{"secretKeyRef":{"name":"not-for-job","key":"token"}}}
      ],
      "resources":{"requests":{"cpu":"50m","memory":"64Mi"},"limits":{"memory":"256Mi"}}
    }]
  }}}
}
EOF
cp "$work_dir/state/deployment.json" "$work_dir/state/deployment-with-retry.json"

cat >"$work_dir/state/config.json" <<'EOF'
{
  "apiVersion":"v1","kind":"ConfigMap","metadata":{
    "name":"witself-server","uid":"config-uid","resourceVersion":"config-rv",
    "annotations":{"witself.io/server-config-checksum":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
  },
  "data":{
    "WITSELF_BACKEND_KIND":"managed",
    "WITSELF_CELL_NAME":"civo-sandbox-usw2-dev",
    "WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED":"true",
    "WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED":"false",
    "WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN":"witmail.net",
    "WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE":"civo-sandbox-usw2-dev",
    "WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON":"{\"key-1\":\"public\"}"
  }
}
EOF
cp "$work_dir/state/config.json" "$work_dir/state/config-dark.json"

cat >"$work_dir/state/pods.json" <<'EOF'
{
  "items":[{"metadata":{"name":"witself-agent-email-operation-pod"},
    "status":{"containerStatuses":[
      {"name":"runner","state":{"terminated":{"exitCode":0}}},
      {"name":"artifact-export","state":{"running":{"startedAt":"2026-08-10T00:00:00Z"}}}
    ]}}
  ]
}
EOF

cat >"$work_dir/canary.json" <<'EOF'
{
  "schema_version":2,"domain":"witmail.net","worker_name":"witself-agent-email-receive",
  "account_ids":["acc_aaaaaaaaaaaaaaaa"],
  "agents":[
    {"agent_id":"agent_aaaaaaaaaaaaaaaa","realm_id":"realm_aaaaaaaaaaaaaaaa","address":"a@witmail.net"},
    {"agent_id":"agent_bbbbbbbbbbbbbbbb","realm_id":"realm_aaaaaaaaaaaaaaaa","address":"b@witmail.net"},
    {"agent_id":"agent_cccccccccccccccc","realm_id":"realm_aaaaaaaaaaaaaaaa","address":"c@witmail.net"},
    {"agent_id":"agent_dddddddddddddddd","realm_id":"realm_aaaaaaaaaaaaaaaa","address":"d@witmail.net"},
    {"agent_id":"agent_eeeeeeeeeeeeeeee","realm_id":"realm_aaaaaaaaaaaaaaaa","address":"e@witmail.net"}
  ]
}
EOF

printf '%s\n' 'test-kubeconfig' >"$work_dir/kubeconfig"
chmod 600 "$work_dir/kubeconfig"
export FAKE_KUBE_STATE="$work_dir/state"
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=0
export WITSELF_AGENT_EMAIL_OPERATION_CLEANUP_TIMEOUT_SECONDS=1
export PATH="$work_dir/bin:$PATH"

reset_fake_run() {
  unset FAKE_SOURCE_DRIFT FAKE_STALE_READINESS FAKE_COMPLETE_ERROR
  unset FAKE_ARTIFACT_INSPECTION_ERROR FAKE_EXEC_READY_FAILURE FAKE_COHORT_IMMUTABLE
  unset FAKE_RETRY_CANARY_IMMUTABLE
  unset FAKE_BACKGROUND_DELETE
  unset FAKE_RUNNER_LOG FAKE_RUNNER_LOG_FAILURE
  rm -f "$work_dir/state/deployment-get-count" "$work_dir/state/config-get-count"
  rm -f "$work_dir/state/retry-secret-get-count"
  rm -f "$work_dir/state/job.json" "$work_dir/state/lock.json" "$work_dir/state/secret.json"
  rm -f "$work_dir/state/job-created.json" "$work_dir/state/lock-created.json"
  rm -f "$work_dir/state/secret-created.json" "$work_dir/state/job-deleted"
  rm -f "$work_dir/state/deletes.log" "$work_dir/state/cleanup-actions.log"
  rm -f "$work_dir/state/exec-actions.log"
}

reset_fake_run
output_path="$work_dir/output/primary-canary.json"
operation_output="$work_dir/operation-output"
if ! "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation canary-manifest \
    --artifact-output "$output_path" \
    --timeout-seconds 60 >"$operation_output" 2>&1; then
  printf 'agent-email cell operation fixture failed:\n' >&2
  sed -n '1,80p' "$operation_output" >&2
  exit 1
fi

test -f "$output_path"
[ "$(file_mode "$output_path")" = 600 ]
jq -e '.schema_version == 2 and (.agents | length == 5)' "$output_path" >/dev/null
grep -Fqx '{"status":"completed","private_artifact_exported":true}' "$operation_output"
if grep -Eq 'acc_[a-z2-7]{16}|agent_[a-z2-7]{16}|realm_[a-z2-7]{16}|@witmail\.net|receive-cohort-v1|retry-canary-v1|witself-db' "$operation_output"; then
  echo "operation output exposed a private identity or Secret reference" >&2
  exit 1
fi

jq -e '
  .kind == "Job" and .metadata.name == "witself-agent-email-operation" and
  .spec.backoffLimit == 0 and .spec.template.spec.automountServiceAccountToken == false and
  (.spec.template.spec.containers | map(.name) == ["runner","artifact-export"]) and
  (.spec.template.spec.containers[0].env | map(.name) ==
    ["WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS","WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID","WITSELF_DATABASE_URL"]) and
  (.spec.template.spec.containers[0].env | all(.valueFrom.secretKeyRef != null)) and
  (.spec.template.spec.containers | all(.image == "ghcr.io/witwave-ai/images/witself-server:0.0.245")) and
  (.spec.template.spec.volumes[] | select(.name == "private") | .emptyDir.medium == "Memory")
' "$work_dir/state/job-created.json" >/dev/null
jq -e '
  .immutable == true and
  (.data.WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS == null) and
  (.data.WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID == null)
' \
  "$work_dir/state/lock-created.json" >/dev/null
grep -Fq 'delete job witself-agent-email-operation --ignore-not-found=true --cascade=foreground --wait=true --timeout=1s' \
  "$work_dir/state/deletes.log"
grep -Fq 'delete configmap witself-agent-email-operation-lock' "$work_dir/state/deletes.log"
test ! -e "$work_dir/state/lock.json"
awk '
  $0 == "delete-job" { job = NR }
  $0 == "pods-absent" { pods = NR }
  $0 == "delete-configmap" { lock = NR }
  END { exit !(job > 0 && pods > job && lock > pods) }
' "$work_dir/state/cleanup-actions.log"

# Phase A deliberately has no selected retry canary. The one-shot runner must
# remain compatible and copy exactly the cohort and database Secret refs.
reset_fake_run
jq '
  .spec.template.spec.containers[0].env |=
    map(select(.name != "WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID"))
' "$work_dir/state/deployment-with-retry.json" >"$work_dir/state/deployment.json"
phase_a_output="$work_dir/phase-a-output"
if ! "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/phase-a-canary.json" \
    --timeout-seconds 60 >"$phase_a_output" 2>&1; then
  cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
  printf 'phase-A no-canary operation fixture failed:\n' >&2
  sed -n '1,80p' "$phase_a_output" >&2
  exit 1
fi
cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
jq -e '
  .spec.template.spec.containers[0].env | map(.name) ==
    ["WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS","WITSELF_DATABASE_URL"]
' "$work_dir/state/job-created.json" >/dev/null
grep -Fqx '{"status":"completed","private_artifact_exported":true}' "$phase_a_output"

# Runtime startup diagnostics share the Kubernetes log stream with the final
# value-free backfill result. Ignore non-JSON lines, but accept exactly one
# structurally valid count object and never echo the surrounding log text.
reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=0
export FAKE_RUNNER_LOG=$'2026/08/15 17:53:44 goose: no migrations to run. current version: 89\n{"account_count":1,"live_agent_count":10,"missing_mailbox_count_after":0,"missing_mailbox_count_before":10,"override_count":0,"processed_agent_count":10,"ready_mailbox_count":10,"retry_canary_ready":false}'
noisy_backfill_output="$work_dir/noisy-backfill-output"
if ! "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation backfill \
    --artifact-output "$work_dir/output/noisy-backfill-must-stay-absent.json" \
    --timeout-seconds 60 >"$noisy_backfill_output" 2>&1; then
  printf 'noisy backfill operation fixture failed:\n' >&2
  sed -n '1,80p' "$noisy_backfill_output" >&2
  exit 1
fi
grep -Fqx \
  '{"account_count":1,"live_agent_count":10,"missing_mailbox_count_after":0,"missing_mailbox_count_before":10,"override_count":0,"processed_agent_count":10,"ready_mailbox_count":10,"retry_canary_ready":false}' \
  "$noisy_backfill_output"
if grep -Fq 'goose:' "$noisy_backfill_output"; then
  echo "backfill output exposed an unrelated runner log line" >&2
  exit 1
fi
test ! -e "$work_dir/output/noisy-backfill-must-stay-absent.json"

# Multiple valid-looking result objects are ambiguous and must stay fail-closed.
reset_fake_run
export FAKE_RUNNER_LOG=$'{"account_count":1,"live_agent_count":10,"missing_mailbox_count_after":0,"missing_mailbox_count_before":0,"override_count":0,"processed_agent_count":10,"ready_mailbox_count":10,"retry_canary_ready":false}\n{"account_count":1,"live_agent_count":10,"missing_mailbox_count_after":0,"missing_mailbox_count_before":0,"override_count":0,"processed_agent_count":10,"ready_mailbox_count":10,"retry_canary_ready":false}'
ambiguous_backfill_output="$work_dir/ambiguous-backfill-output"
if ! "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation backfill \
    --artifact-output "$work_dir/output/ambiguous-backfill-must-stay-absent.json" \
    --timeout-seconds 60 >"$ambiguous_backfill_output" 2>&1; then
  printf 'ambiguous backfill operation fixture failed:\n' >&2
  sed -n '1,80p' "$ambiguous_backfill_output" >&2
  exit 1
fi
grep -Fqx '{"status":"completed","counts":"unavailable"}' "$ambiguous_backfill_output"
test ! -e "$work_dir/output/ambiguous-backfill-must-stay-absent.json"

# The active ConfigMap is a non-secret source snapshot. It must never carry the
# selected canary ID even if an operator manually creates such a key.
reset_fake_run
jq '.data.WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID = "agent_aaaaaaaaaaaaaaaa"' \
  "$work_dir/state/config-dark.json" >"$work_dir/state/config.json"
literal_config_output="$work_dir/literal-config-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/literal-config-must-stay-absent.json" \
    --timeout-seconds 60 >"$literal_config_output" 2>&1; then
  cp "$work_dir/state/config-dark.json" "$work_dir/state/config.json"
  echo "literal retry-canary ConfigMap fixture unexpectedly succeeded" >&2
  exit 1
fi
cp "$work_dir/state/config-dark.json" "$work_dir/state/config.json"
grep -Fqx \
  'error: managed cell identity or secret-backed production receive configuration is not ready' \
  "$literal_config_output"
test ! -e "$work_dir/output/literal-config-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

# A Secret-backed retry canary was introduced in 0.0.245. Refuse to copy that
# env source into a runner using older code even if every other fence is valid.
reset_fake_run
jq '.spec.template.spec.containers[0].image = "ghcr.io/witwave-ai/images/witself-server:0.0.244"' \
  "$work_dir/state/deployment-with-retry.json" >"$work_dir/state/deployment.json"
pre245_retry_output="$work_dir/pre245-retry-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/pre245-retry-must-stay-absent.json" \
    --timeout-seconds 60 >"$pre245_retry_output" 2>&1; then
  cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
  echo "pre-0.0.245 retry-canary Secret fixture unexpectedly succeeded" >&2
  exit 1
fi
cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
grep -Fqx 'error: managed retry-canary Secret operations require image v0.0.245 or newer' \
  "$pre245_retry_output"
test ! -e "$work_dir/output/pre245-retry-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

# The canary has a separate empty-to-selected lifecycle. Sharing the cohort
# Secret would force an unrelated cohort rotation and is therefore rejected.
reset_fake_run
jq '
  (.spec.template.spec.containers[0].env[] |
   select(.name == "WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID") |
   .valueFrom.secretKeyRef.name) = "receive-cohort-v1"
' "$work_dir/state/deployment-with-retry.json" >"$work_dir/state/deployment.json"
shared_secret_output="$work_dir/shared-secret-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/shared-secret-must-stay-absent.json" \
    --timeout-seconds 60 >"$shared_secret_output" 2>&1; then
  cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
  echo "shared cohort/retry-canary Secret fixture unexpectedly succeeded" >&2
  exit 1
fi
cp "$work_dir/state/deployment-with-retry.json" "$work_dir/state/deployment.json"
grep -Fqx \
  'error: receive-cohort and retry-canary Secret references must use distinct Secret names' \
  "$shared_secret_output"
test ! -e "$work_dir/output/shared-secret-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

cat >"$work_dir/overrides.json" <<'EOF'
{"schema_version":1,"overrides":[{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support-agent"}]}
EOF
chmod 600 "$work_dir/overrides.json"

reset_fake_run
export FAKE_BACKGROUND_DELETE=true
lingering_output="$work_dir/lingering-output"
if ! "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation backfill \
    --overrides "$work_dir/overrides.json" \
    --artifact-output "$work_dir/output/lingering-must-stay-absent.json" \
    --timeout-seconds 60 >"$lingering_output" 2>&1; then
  printf 'lingering operation fixture unexpectedly failed:\n' >&2
  sed -n '1,80p' "$lingering_output" >&2
  exit 1
fi
grep -Fqx \
  'warning: operation cleanup could not prove the runner absent; the fixed lock was retained' \
  "$lingering_output"
test -e "$work_dir/state/lock.json"
test -e "$work_dir/state/secret.json"
test ! -e "$work_dir/output/lingering-must-stay-absent.json"
grep -Fq 'delete job witself-agent-email-operation --ignore-not-found=true --cascade=foreground --wait=true --timeout=1s' \
  "$work_dir/state/deletes.log"
if grep -Eq '^delete (secret|configmap) ' "$work_dir/state/deletes.log"; then
  echo "cleanup removed a lock resource while the exact Job pod lingered" >&2
  exit 1
fi

reset_fake_run
export FAKE_STALE_READINESS=true
stale_output="$work_dir/stale-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/stale-must-stay-absent.json" \
    --timeout-seconds 60 >"$stale_output" 2>&1; then
  echo "stale Deployment fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: managed server Deployment is absent, ambiguous, or not fully converged' "$stale_output"
test ! -e "$work_dir/output/stale-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

reset_fake_run
export FAKE_COHORT_IMMUTABLE=false
mutable_cohort_output="$work_dir/mutable-cohort-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/mutable-cohort-must-stay-absent.json" \
    --timeout-seconds 60 >"$mutable_cohort_output" 2>&1; then
  echo "mutable cohort Secret fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: receive-cohort Secret must be live and immutable' "$mutable_cohort_output"
test ! -e "$work_dir/output/mutable-cohort-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

reset_fake_run
export FAKE_RETRY_CANARY_IMMUTABLE=false
mutable_retry_canary_output="$work_dir/mutable-retry-canary-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/mutable-retry-canary-must-stay-absent.json" \
    --timeout-seconds 60 >"$mutable_retry_canary_output" 2>&1; then
  echo "mutable retry-canary Secret fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: retry-canary Secret must be live and immutable' "$mutable_retry_canary_output"
test ! -e "$work_dir/output/mutable-retry-canary-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

reset_fake_run
export FAKE_SOURCE_DRIFT=config
drift_output="$work_dir/drift-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/drift-must-stay-absent.json" \
    --timeout-seconds 60 >"$drift_output" 2>&1; then
  echo "source drift fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: managed server source drifted before Job creation' "$drift_output"
test ! -e "$work_dir/output/drift-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

reset_fake_run
export FAKE_SOURCE_DRIFT=retry-secret
retry_secret_drift_output="$work_dir/retry-secret-drift-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/retry-secret-drift-must-stay-absent.json" \
    --timeout-seconds 60 >"$retry_secret_drift_output" 2>&1; then
  echo "retry-canary Secret drift fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: managed server source drifted before Job creation' "$retry_secret_drift_output"
test ! -e "$work_dir/output/retry-secret-drift-must-stay-absent.json"
test ! -e "$work_dir/state/job.json"

reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=0
export FAKE_COMPLETE_ERROR=true
completion_output="$work_dir/completion-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/completion-must-stay-absent.json" \
    --timeout-seconds 60 >"$completion_output" 2>&1; then
  echo "holder completion failure fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: could not complete the private artifact holder' "$completion_output"
test ! -e "$work_dir/output/completion-must-stay-absent.json"

reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=1
export FAKE_RUNNER_LOG='witself-server: agent-email production canary database open failed (reason=database_unavailable)'
canary_failure_output="$work_dir/canary-failure-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/failed-canary-must-stay-absent.json" \
    --timeout-seconds 60 >"$canary_failure_output" 2>&1; then
  echo "failed canary runner fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx \
  'error: agent-email operation failed (reason=database_unavailable) without an exportable private artifact' \
  "$canary_failure_output"
test ! -e "$work_dir/output/failed-canary-must-stay-absent.json"
if grep -Eq 'acc_[a-z2-7]{16}|agent_[a-z2-7]{16}|realm_[a-z2-7]{16}|@witmail\.net|receive-cohort-v1|retry-canary-v1|witself-db' "$canary_failure_output"; then
  echo "runner failure output exposed a private identity or Secret reference" >&2
  exit 1
fi

reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=1
export FAKE_RUNNER_LOG='witself-server: agent-email production canary database open failed (reason=account_acc_aaaaaaaaaaaaaaaa) secret=witself-db'
untrusted_reason_output="$work_dir/untrusted-reason-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/untrusted-reason-must-stay-absent.json" \
    --timeout-seconds 60 >"$untrusted_reason_output" 2>&1; then
  echo "untrusted runner reason fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx \
  'error: agent-email operation failed (reason=unavailable) without an exportable private artifact' \
  "$untrusted_reason_output"
test ! -e "$work_dir/output/untrusted-reason-must-stay-absent.json"
if grep -Eq 'acc_[a-z2-7]{16}|agent_[a-z2-7]{16}|realm_[a-z2-7]{16}|@witmail\.net|receive-cohort-v1|retry-canary-v1|witself-db' "$untrusted_reason_output"; then
  echo "untrusted runner log crossed the bounded failure-reason boundary" >&2
  exit 1
fi

reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=1
export FAKE_RUNNER_LOG=$'witself-server: agent-email production canary database open failed (reason=database_unavailable)\nwitself-server: agent-email production canary snapshot failed (reason=canary_snapshot_failed)'
conflicting_reasons_output="$work_dir/conflicting-reasons-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/conflicting-reasons-must-stay-absent.json" \
    --timeout-seconds 60 >"$conflicting_reasons_output" 2>&1; then
  echo "conflicting runner reasons fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx \
  'error: agent-email operation failed (reason=unavailable) without an exportable private artifact' \
  "$conflicting_reasons_output"

reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=1
export FAKE_RUNNER_LOG_FAILURE=true
log_failure_output="$work_dir/log-failure-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev --kubeconfig "$work_dir/kubeconfig" --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/log-failure-must-stay-absent.json" \
    --timeout-seconds 60 >"$log_failure_output" 2>&1; then
  echo "runner log-fetch failure fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx \
  'error: agent-email operation failed (reason=unavailable) without an exportable private artifact' \
  "$log_failure_output"

cat >"$work_dir/exception.json" <<'EOF'
{
  "schema_version":1,"state":"requires_operator_override","processed_agent_count":3,
  "agent_id":"agent_aaaaaaaaaaaaaaaa","realm_id":"realm_aaaaaaaaaaaaaaaa",
  "reason_code":"agent_segment_requires_override"
}
EOF
reset_fake_run
export FAKE_PRIVATE_ARTIFACT="$work_dir/exception.json"
export FAKE_PRIVATE_ARTIFACT_KEY=backfill-exception
export FAKE_RUNNER_EXIT=1
exception_output="$work_dir/output/backfill-exception.json"
failure_output="$work_dir/failure-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation backfill \
    --artifact-output "$exception_output" \
    --timeout-seconds 60 >"$failure_output" 2>&1; then
  echo "backfill exception fixture unexpectedly succeeded" >&2
  exit 1
fi
test -f "$exception_output"
[ "$(file_mode "$exception_output")" = 600 ]
jq -e '.state == "requires_operator_override"' "$exception_output" >/dev/null
grep -Fqx 'error: backfill status requires_operator_override; the private exception artifact was exported' "$failure_output"
if grep -Eq 'acc_[a-z2-7]{16}|agent_[a-z2-7]{16}|realm_[a-z2-7]{16}|@witmail\.net|receive-cohort-v1|retry-canary-v1|witself-db' "$failure_output"; then
  echo "failure output exposed a private identity or Secret reference" >&2
  exit 1
fi

export FAKE_PRIVATE_ARTIFACT="$work_dir/canary.json"
export FAKE_PRIVATE_ARTIFACT_KEY=primary-canary
export FAKE_RUNNER_EXIT=0
reset_fake_run
export FAKE_ARTIFACT_INSPECTION_ERROR=true
inspection_output="$work_dir/inspection-output"
if "$repo_root/scripts/run-agent-email-cell-operation.sh" \
    --cell civo-sandbox-usw2-dev \
    --kubeconfig "$work_dir/kubeconfig" \
    --context civo-test \
    --operation canary-manifest \
    --artifact-output "$work_dir/output/inspection-must-stay-absent.json" \
    --timeout-seconds 60 >"$inspection_output" 2>&1; then
  echo "artifact inspection error fixture unexpectedly succeeded" >&2
  exit 1
fi
grep -Fqx 'error: private artifact inspection failed' "$inspection_output"
test ! -e "$work_dir/output/inspection-must-stay-absent.json"

printf 'agent-email cell operation isolation/export tests passed\n'
