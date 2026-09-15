#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-agent-email-receipt-proof-test.XXXXXX")"
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
chmod 700 "$work_dir"
work_dir="$(cd "$work_dir" && pwd -P)"
mkdir -m 700 "$work_dir/bin" "$work_dir/state"

cat >"$work_dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[ "${1:-}" = --request-timeout=30s ] || exit 91
shift
[ "${1:-}" = --kubeconfig ] || exit 92
kubeconfig_snapshot="${2:-}"
[ "$kubeconfig_snapshot" != "$FAKE_ORIGINAL_KUBECONFIG" ] || exit 92
[ -f "$kubeconfig_snapshot" ] && [ ! -L "$kubeconfig_snapshot" ] || exit 92
snapshot_mode="$(stat -f '%Lp' "$kubeconfig_snapshot" 2>/dev/null || true)"
if [[ ! "$snapshot_mode" =~ ^[0-7]{3,4}$ ]]; then
  snapshot_mode="$(stat -c '%a' "$kubeconfig_snapshot" 2>/dev/null || true)"
fi
[ "$snapshot_mode" = 400 ] || exit 92
cmp -s "$kubeconfig_snapshot" "$FAKE_EXPECTED_KUBECONFIG_CONTENT" || exit 92
printf '%s\n' "$kubeconfig_snapshot" >>"$FAKE_KUBE_STATE/kubeconfig-paths.log"
shift 2
[ "${1:-}" = --context ] && [ "${2:-}" = "$FAKE_EXPECTED_CONTEXT" ] || exit 93
shift 2
[ "${1:-}" = -n ] && [ "${2:-}" = "$FAKE_EXPECTED_NAMESPACE" ] || exit 94
shift 2
printf '%s\n' "$*" >>"$FAKE_KUBE_STATE/calls.log"
if [ "${FAKE_REWRITE_KUBECONFIG:-false}" = true ] &&
   [ ! -e "$FAKE_KUBE_STATE/kubeconfig-rewritten" ]; then
  printf '%s\n' rewritten-cross-cluster >"$FAKE_ORIGINAL_KUBECONFIG"
  chmod 600 "$FAKE_ORIGINAL_KUBECONFIG"
  : >"$FAKE_KUBE_STATE/kubeconfig-rewritten"
fi

render_proof_pods() {
  local job_uid=proof-job-uid
  local pod_uid=proof-pod-uid
  local job_file="$FAKE_KUBE_STATE/job.json"
  [ -f "$job_file" ] || job_file="$FAKE_KUBE_STATE/job-created.json"
  if [ "${FAKE_REPLACE_POD:-}" = owner ]; then job_uid=replacement-job-uid; fi
  if [ "${FAKE_REPLACE_POD:-}" = uid ]; then pod_uid=replacement-pod-uid; fi
  if [ "${FAKE_REPLACE_POD:-}" = postlog ] && [ -e "$FAKE_KUBE_STATE/logs-read" ]; then
    pod_uid=replacement-pod-uid
  fi
  if [ "${FAKE_CLEANUP_POD_STATE:-}" = replacement ] &&
     [ -e "$FAKE_KUBE_STATE/job-deleted" ]; then
    pod_uid=replacement-pod-uid
  fi
  jq -n --arg job_uid "$job_uid" --arg pod_uid "$pod_uid" \
    --argjson runner_exit "${FAKE_RUNNER_EXIT:-0}" --slurpfile job "$job_file" '{
    items:[{
      apiVersion:"v1",kind:"Pod",
      metadata:{name:"witself-agent-email-receipt-proof-pod",uid:$pod_uid,
        resourceVersion:"proof-pod-rv",
        annotations:$job[0].spec.template.metadata.annotations,
        labels:$job[0].spec.template.metadata.labels,
        ownerReferences:[{apiVersion:"batch/v1",kind:"Job",
          name:"witself-agent-email-receipt-proof",uid:$job_uid,
          controller:true,blockOwnerDeletion:true}]},
      spec:$job[0].spec.template.spec,
      status:{phase:"Succeeded",containerStatuses:[{name:"runner",ready:false,
        state:{terminated:{exitCode:$runner_exit}}}]}
    }]
  }'
}

case "${1:-} ${2:-}" in
  "get deployment")
    case "${3:-}" in
      witself-worker)
        count_file="$FAKE_KUBE_STATE/deployment-get-count"
        count=$(($(cat "$count_file" 2>/dev/null || printf 0) + 1))
        printf '%s\n' "$count" >"$count_file"
        filter='.'
        if [ "${FAKE_NOT_READY:-false}" = true ]; then
          filter+=' | .status.readyReplicas = 1 | .status.unavailableReplicas = 1'
        fi
        if [ "${FAKE_WRONG_REPLICAS:-false}" = true ]; then
          filter+=' | .spec.replicas = 1 | .status.replicas = 1 |
            .status.readyReplicas = 1 | .status.updatedReplicas = 1 |
            .status.availableReplicas = 1 | .status.unavailableReplicas = 0'
        fi
        if { [ "${FAKE_SOURCE_DRIFT:-}" = prelock ] && [ "$count" -ge 2 ]; } ||
           { [ "${FAKE_SOURCE_DRIFT:-}" = prejob ] && [ "$count" -ge 3 ]; }; then
          filter+=' | .metadata.resourceVersion = "deployment-rv-drift"'
        fi
        jq "$filter" "$FAKE_KUBE_STATE/deployment.json"
        ;;
      witself-server)
        cat "$FAKE_KUBE_STATE/cell-deployment.json"
        ;;
      *) exit 1 ;;
    esac
    ;;
  "get configmap")
    case "${3:-}" in
      witself-worker)
        count_file="$FAKE_KUBE_STATE/config-get-count"
        count=$(($(cat "$count_file" 2>/dev/null || printf 0) + 1))
        printf '%s\n' "$count" >"$count_file"
        filter='.'
        if [ "${FAKE_DISABLED:-false}" = true ]; then
          filter+=' | .data.WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED = "false"'
        fi
        if [ "${FAKE_LITERAL_PRIVATE_CONFIG:-false}" = true ]; then
          filter+=' | .data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY = "forbidden"'
        fi
        jq "$filter" "$FAKE_KUBE_STATE/config.json"
        ;;
      witself-server)
        if [ "${FAKE_WRONG_CELL:-false}" = true ]; then
          jq '.data.WITSELF_CELL_NAME = "wrong-cell"' "$FAKE_KUBE_STATE/cell-config.json"
        else
          cat "$FAKE_KUBE_STATE/cell-config.json"
        fi
        ;;
      witself-agent-email-receipt-proof-lock)
        [ -f "$FAKE_KUBE_STATE/lock.json" ] || exit 0
        if [ "${FAKE_REPLACE_LOCK:-false}" = true ] ||
           { [ "${FAKE_REPLACE_LOCK:-}" = postlog ] && [ -e "$FAKE_KUBE_STATE/logs-read" ]; }; then
          jq '.metadata.uid = "replacement-lock-uid" |
            .metadata.resourceVersion = "replacement-lock-rv"' \
            "$FAKE_KUBE_STATE/lock.json" >"$FAKE_KUBE_STATE/lock-replacement.json"
          mv "$FAKE_KUBE_STATE/lock-replacement.json" "$FAKE_KUBE_STATE/lock.json"
        fi
        cat "$FAKE_KUBE_STATE/lock.json"
        ;;
      *) exit 1 ;;
    esac
    ;;
  "get secret")
    secret_name="${3:-}"
    count_file="$FAKE_KUBE_STATE/${secret_name}-get-count"
    count=$(($(cat "$count_file" 2>/dev/null || printf 0) + 1))
    printf '%s\n' "$count" >"$count_file"
    case "$secret_name" in
      witself-db)
        rv=database-rv
        if { [ "${FAKE_SECRET_DRIFT:-}" = database ] && [ "$count" -ge 2 ]; } ||
           { [ "${FAKE_SECRET_DRIFT:-}" = db-poststart ] && [ "$count" -ge 4 ]; } ||
           { [ "${FAKE_SECRET_DRIFT:-}" = db-postflight ] && [ "$count" -ge 5 ]; }; then
          rv=database-rv-drift
        fi
        printf 'database-uid\n%s\nfalse\n' "$rv"
        ;;
      outbound-dispatch-v1)
        rv=dispatch-rv
        if [ "${FAKE_SECRET_DRIFT:-}" = dispatch ] && [ "$count" -ge 2 ]; then
          rv=dispatch-rv-drift
        fi
        printf 'dispatch-uid\n%s\n%s\n' "$rv" "${FAKE_DISPATCH_IMMUTABLE:-true}"
        ;;
      *) exit 1 ;;
    esac
    ;;
  "get job")
    if [ -f "$FAKE_KUBE_STATE/job.json" ]; then
      if [ "${FAKE_REPLACE_JOB:-false}" = true ] ||
         { [ "${FAKE_REPLACE_JOB:-}" = postlog ] && [ -e "$FAKE_KUBE_STATE/logs-read" ]; }; then
        jq '.metadata.uid = "replacement-job-uid"' \
          "$FAKE_KUBE_STATE/job.json" >"$FAKE_KUBE_STATE/job-replacement.json"
        mv "$FAKE_KUBE_STATE/job-replacement.json" "$FAKE_KUBE_STATE/job.json"
      fi
      cat "$FAKE_KUBE_STATE/job.json"
    elif [ "${FAKE_EXISTING_JOB:-false}" = true ]; then
      printf 'job.batch/witself-agent-email-receipt-proof\n'
    elif [ "${FAKE_JOB_LOOKUP_FAILURE:-false}" = true ]; then
      exit 1
    fi
    ;;
  "create -f")
    payload="$(cat)"
    kind="$(jq -r '.kind' <<<"$payload")"
    case "$kind" in
      ConfigMap)
        [ "${FAKE_LOCK_CREATE_FAILURE:-false}" != true ] || exit 1
        created="$(jq '.metadata.uid = "proof-lock-uid" |
          .metadata.resourceVersion = "proof-lock-rv"' <<<"$payload")"
        printf '%s\n' "$created" >"$FAKE_KUBE_STATE/lock-created.json"
        printf '%s\n' "$created" >"$FAKE_KUBE_STATE/lock.json"
        printf '%s\n' "$created"
        ;;
      Job)
        if [ "${FAKE_JOB_CREATE_FAILURE:-false}" = true ]; then
          exit 1
        fi
        created="$(jq '.metadata.uid = "proof-job-uid" |
          .metadata.resourceVersion = "proof-job-rv"' <<<"$payload")"
        printf '%s\n' "$created" >"$FAKE_KUBE_STATE/job-created.json"
        printf '%s\n' "$created" >"$FAKE_KUBE_STATE/job.json"
        printf '%s\n' "$created"
        ;;
      *) exit 1 ;;
    esac
    ;;
  "get pods")
    case "${4:-}" in
      app.kubernetes.io/name=witself-worker,app.kubernetes.io/instance=witself-server,app.kubernetes.io/component=worker)
        if [ "${FAKE_REPLACE_WORKER_POD_OWNER:-false}" = true ]; then
          jq '.items[0].metadata.ownerReferences[0].uid = "foreign-rs-uid"' \
            "$FAKE_KUBE_STATE/worker-pods.json"
        elif [ "${FAKE_EXTRA_WORKER_POD:-false}" = true ]; then
          jq '.items += [(.items[0] |
            .metadata.name = "witself-worker-rogue" |
            .metadata.uid = "worker-pod-rogue-uid" |
            .metadata.resourceVersion = "worker-pod-rogue-rv" |
            .metadata.ownerReferences[0].uid = "foreign-rs-uid")]' \
            "$FAKE_KUBE_STATE/worker-pods.json"
        else
          cat "$FAKE_KUBE_STATE/worker-pods.json"
        fi
        ;;
      batch.kubernetes.io/job-name=witself-agent-email-receipt-proof)
        if [ -f "$FAKE_KUBE_STATE/job-deleted" ] &&
           [ "${FAKE_BACKGROUND_DELETE:-false}" != true ] &&
           [ -z "${FAKE_CLEANUP_POD_STATE:-}" ]; then
          printf '%s\n' pods-absent >>"$FAKE_KUBE_STATE/cleanup-actions.log"
          printf '%s\n' '{"items":[]}'
        elif [ -f "$FAKE_KUBE_STATE/job-deleted" ] &&
             [ "${FAKE_CLEANUP_POD_STATE:-}" = relabeled ]; then
          printf '%s\n' pods-absent >>"$FAKE_KUBE_STATE/cleanup-actions.log"
          printf '%s\n' '{"items":[]}'
        else
          render_proof_pods
        fi
        ;;
      *) exit 1 ;;
    esac
    ;;
  "get replicasets")
    cat "$FAKE_KUBE_STATE/worker-replicasets.json"
    ;;
  "get pod")
    if [ -f "$FAKE_KUBE_STATE/job-deleted" ] &&
       [ "${FAKE_BACKGROUND_DELETE:-false}" != true ] &&
       [ -z "${FAKE_CLEANUP_POD_STATE:-}" ]; then
      exit 0
    fi
    render_proof_pods | jq '.items[0]'
    ;;
  "logs witself-agent-email-receipt-proof-pod")
    [ "$#" -eq 6 ] || exit 1
    [ "$3" = -c ] && [ "$4" = runner ] && [ "$5" = --tail=4 ] &&
      [ "$6" = --limit-bytes=16384 ] || exit 1
    [ "${FAKE_LOG_FAILURE:-false}" != true ] || exit 1
    : >"$FAKE_KUBE_STATE/logs-read"
    printf '%s\n' "${FAKE_RUNNER_LOG:-}"
    ;;
  "delete --raw="*)
    raw_path="${2#--raw=}"
    [ "${3:-}" = -f ] && [ -f "${4:-}" ] || exit 1
    requested_uid="$(jq -er '.preconditions.uid' "$4")"
    [ "$(jq -r '.propagationPolicy' "$4")" = Foreground ] || exit 1
    printf '%s|%s\n' "$raw_path" "$requested_uid" >>"$FAKE_KUBE_STATE/deletes.log"
    case "$raw_path" in
      /apis/batch/v1/namespaces/witself/jobs/witself-agent-email-receipt-proof)
        current_uid="$(jq -r '.metadata.uid' "$FAKE_KUBE_STATE/job.json")"
        if [ "${FAKE_REPLACE_JOB_AT_CLEANUP:-false}" = true ]; then
          current_uid=replacement-job-uid
          jq --arg uid "$current_uid" '.metadata.uid = $uid' "$FAKE_KUBE_STATE/job.json" \
            >"$FAKE_KUBE_STATE/job-replacement.json"
          mv "$FAKE_KUBE_STATE/job-replacement.json" "$FAKE_KUBE_STATE/job.json"
        fi
        [ "$requested_uid" = "$current_uid" ] || exit 1
        printf '%s\n' delete-job >>"$FAKE_KUBE_STATE/cleanup-actions.log"
        : >"$FAKE_KUBE_STATE/job-deleted"
        if [ "${FAKE_BACKGROUND_DELETE:-false}" != true ]; then
          rm -f "$FAKE_KUBE_STATE/job.json"
        fi
        jq -n --arg uid "$current_uid" '{kind:"Status",status:"Success",
          details:{name:"witself-agent-email-receipt-proof",kind:"jobs",uid:$uid}}'
        ;;
      /api/v1/namespaces/witself/configmaps/witself-agent-email-receipt-proof-lock)
        current_uid="$(jq -r '.metadata.uid' "$FAKE_KUBE_STATE/lock.json")"
        if [ "${FAKE_REPLACE_LOCK_AT_CLEANUP:-false}" = true ]; then
          current_uid=replacement-lock-uid
          jq --arg uid "$current_uid" '.metadata.uid = $uid' "$FAKE_KUBE_STATE/lock.json" \
            >"$FAKE_KUBE_STATE/lock-replacement.json"
          mv "$FAKE_KUBE_STATE/lock-replacement.json" "$FAKE_KUBE_STATE/lock.json"
        fi
        [ "$requested_uid" = "$current_uid" ] || exit 1
        printf '%s\n' delete-configmap >>"$FAKE_KUBE_STATE/cleanup-actions.log"
        rm -f "$FAKE_KUBE_STATE/lock.json"
        jq -n --arg uid "$current_uid" '{kind:"Status",status:"Success",
          details:{name:"witself-agent-email-receipt-proof-lock",kind:"configmaps",uid:$uid}}'
        ;;
      *) exit 1 ;;
    esac
    ;;
  "exec "*)
    printf '%s\n' "$*" >"$FAKE_KUBE_STATE/forbidden-exec.log"
    exit 1
    ;;
  *) exit 1 ;;
esac
EOF
chmod 700 "$work_dir/bin/kubectl"

cat >"$work_dir/state/deployment.json" <<'EOF'
{
  "apiVersion":"apps/v1","kind":"Deployment",
  "metadata":{"name":"witself-worker","uid":"deployment-uid","resourceVersion":"deployment-rv","generation":8},
  "status":{"observedGeneration":8,"replicas":2,"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2,"unavailableReplicas":0},
  "spec":{"replicas":2,"selector":{"matchLabels":{
    "app.kubernetes.io/component":"worker","app.kubernetes.io/instance":"witself-server",
    "app.kubernetes.io/name":"witself-worker"}},"template":{
    "metadata":{"labels":{"app.kubernetes.io/component":"worker",
      "app.kubernetes.io/instance":"witself-server","app.kubernetes.io/name":"witself-worker"},
      "annotations":{"checksum/config":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
    "spec":{
      "serviceAccountName":"witself-server","automountServiceAccountToken":false,
      "imagePullSecrets":[{"name":"registry-read"}],
      "nodeSelector":{"pool":"worker"},"tolerations":[],"affinity":{},
      "containers":[{"name":"witself-worker",
        "image":"ghcr.io/witwave-ai/images/witself-server:0.0.249",
        "imagePullPolicy":"IfNotPresent",
        "command":["/usr/local/bin/witself-worker"],"args":["serve"],
        "envFrom":[{"configMapRef":{"name":"witself-worker"}}],
        "env":[
          {"name":"WITSELF_DATABASE_URL","valueFrom":{"secretKeyRef":{"name":"witself-db","key":"dsn"}}},
          {"name":"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY","valueFrom":{"secretKeyRef":{"name":"outbound-dispatch-v1","key":"private-key"}}},
          {"name":"UNRELATED_PRIVATE_VALUE","valueFrom":{"secretKeyRef":{"name":"not-for-proof","key":"value"}}}
        ],
        "resources":{"requests":{"cpu":"50m","memory":"64Mi"},"limits":{"memory":"256Mi"}}
      }]
    }
  }}
}
EOF

jq -n --slurpfile deployment "$work_dir/state/deployment.json" '{items:[{
  apiVersion:"apps/v1",kind:"ReplicaSet",
  metadata:{name:"witself-worker-current",uid:"worker-rs-uid",resourceVersion:"worker-rs-rv",
    ownerReferences:[{apiVersion:"apps/v1",kind:"Deployment",name:"witself-worker",
      uid:"deployment-uid",controller:true,blockOwnerDeletion:true}]},
  spec:{template:$deployment[0].spec.template}
}]}' >"$work_dir/state/worker-replicasets.json"

jq -n --slurpfile deployment "$work_dir/state/deployment.json" '{items:[
  {apiVersion:"v1",kind:"Pod",metadata:{name:"witself-worker-current-a",uid:"worker-pod-a-uid",
    resourceVersion:"worker-pod-a-rv",labels:$deployment[0].spec.template.metadata.labels,
    annotations:$deployment[0].spec.template.metadata.annotations,
    ownerReferences:[{apiVersion:"apps/v1",kind:"ReplicaSet",name:"witself-worker-current",
      uid:"worker-rs-uid",controller:true,blockOwnerDeletion:true}]},
    spec:$deployment[0].spec.template.spec,status:{phase:"Running",startTime:"2026-08-15T09:52:23Z",
      conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:"witself-worker",ready:true}]}},
  {apiVersion:"v1",kind:"Pod",metadata:{name:"witself-worker-current-b",uid:"worker-pod-b-uid",
    resourceVersion:"worker-pod-b-rv",labels:$deployment[0].spec.template.metadata.labels,
    annotations:$deployment[0].spec.template.metadata.annotations,
    ownerReferences:[{apiVersion:"apps/v1",kind:"ReplicaSet",name:"witself-worker-current",
      uid:"worker-rs-uid",controller:true,blockOwnerDeletion:true}]},
    spec:$deployment[0].spec.template.spec,status:{phase:"Running",startTime:"2026-08-15T09:52:50Z",
      conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:"witself-worker",ready:true}]}}
]}' >"$work_dir/state/worker-pods.json"

cat >"$work_dir/state/config.json" <<'EOF'
{
  "apiVersion":"v1","kind":"ConfigMap",
  "metadata":{"name":"witself-worker","uid":"config-uid","resourceVersion":"config-rv"},
  "data":{
    "WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED":"true",
    "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT":"https://witself-agent-email-send.example.workers.dev/v1/dispatch",
    "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE":"witself-agent-email-send",
    "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID":"cell-2026-08",
    "WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_SIZE":"10",
    "WITSELF_AGENT_EMAIL_OUTBOUND_INTERVAL":"2s",
    "WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_TIMEOUT":"30s",
    "WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT":"20s",
    "UNRELATED_NON_SECRET":"must-not-be-copied"
  }
}
EOF

cat >"$work_dir/state/cell-deployment.json" <<'EOF'
{
  "apiVersion":"apps/v1","kind":"Deployment",
  "metadata":{"name":"witself-server","uid":"server-deployment-uid","resourceVersion":"server-deployment-rv","generation":9},
  "status":{"observedGeneration":9,"replicas":1,"readyReplicas":1,"updatedReplicas":1,"availableReplicas":1,"unavailableReplicas":0},
  "spec":{"replicas":1,"template":{
    "metadata":{"annotations":{"witself.io/server-config-checksum":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
    "spec":{"containers":[{"name":"witself-server",
      "image":"ghcr.io/witwave-ai/images/witself-server:0.0.249",
      "envFrom":[{"configMapRef":{"name":"witself-server"}}]
    }]}
  }}
}
EOF

cat >"$work_dir/state/cell-config.json" <<'EOF'
{
  "apiVersion":"v1","kind":"ConfigMap",
  "metadata":{"name":"witself-server","uid":"server-config-uid","resourceVersion":"server-config-rv",
    "annotations":{"witself.io/server-config-checksum":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}},
  "data":{"WITSELF_BACKEND_KIND":"managed","WITSELF_CELL_NAME":"civo-sandbox-usw2-dev"}
}
EOF

cat >"$work_dir/state/pods.json" <<'EOF'
{
  "items":[{
    "metadata":{"name":"witself-agent-email-receipt-proof-pod"},
    "status":{"containerStatuses":[{"name":"runner","state":{"terminated":{"exitCode":0}}}]}
  }]
}
EOF

printf '%s\n' test-kubeconfig >"$work_dir/kubeconfig-expected"
chmod 400 "$work_dir/kubeconfig-expected"
cp "$work_dir/kubeconfig-expected" "$work_dir/kubeconfig"
chmod 600 "$work_dir/kubeconfig"
export FAKE_KUBE_STATE="$work_dir/state"
export FAKE_ORIGINAL_KUBECONFIG="$work_dir/kubeconfig"
export FAKE_EXPECTED_KUBECONFIG_CONTENT="$work_dir/kubeconfig-expected"
export FAKE_EXPECTED_CONTEXT=witself-civo-sandbox-usw2-dev
export FAKE_EXPECTED_NAMESPACE=witself
export WITSELF_AGENT_EMAIL_RECEIPT_PROOF_CLEANUP_TIMEOUT_SECONDS=1
export PATH="$work_dir/bin:$PATH"

proof='{"schema_version":"witself.agent-email-dispatch-receipt-proof.v1","send_id":"esnd_aaaaaaaaaaaaaaaa","receipt_state":"accepted","digest_matched":true,"signer_matched":true,"provider_call_started_count":1,"verified_replay_count":1,"route_pending":false}'
export FAKE_RUNNER_LOG="$proof"
export FAKE_RUNNER_EXIT=0

base_args=(
  --cell civo-sandbox-usw2-dev
  --kubeconfig "$work_dir/kubeconfig"
  --context witself-civo-sandbox-usw2-dev
  --namespace witself
  --expected-image ghcr.io/witwave-ai/images/witself-server:0.0.249
  --expected-config-checksum bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  --expected-replicas 2
  --account-id acc_aaaaaaaaaaaaaaaa
  --send-id esnd_aaaaaaaaaaaaaaaa
  --expected-accepted-at 2026-08-16T01:02:03.123456Z
  --timeout-seconds 60
)

reset_fake_run() {
  unset FAKE_NOT_READY FAKE_WRONG_REPLICAS FAKE_WRONG_CELL
  unset FAKE_DISABLED FAKE_LITERAL_PRIVATE_CONFIG
  unset FAKE_SOURCE_DRIFT FAKE_SECRET_DRIFT FAKE_DISPATCH_IMMUTABLE
  unset FAKE_REWRITE_KUBECONFIG FAKE_REPLACE_POD FAKE_REPLACE_JOB FAKE_REPLACE_LOCK
  unset FAKE_REPLACE_WORKER_POD_OWNER FAKE_EXTRA_WORKER_POD
  unset FAKE_EXISTING_JOB FAKE_LOCK_CREATE_FAILURE FAKE_JOB_CREATE_FAILURE
  unset FAKE_JOB_LOOKUP_FAILURE
  unset FAKE_REPLACE_JOB_AT_CLEANUP FAKE_REPLACE_LOCK_AT_CLEANUP
  unset FAKE_BACKGROUND_DELETE FAKE_CLEANUP_POD_STATE FAKE_LOG_FAILURE
  export FAKE_RUNNER_EXIT=0
  export FAKE_RUNNER_LOG="$proof"
  rm -f "$work_dir/state/"*-get-count "$work_dir/state/calls.log"
  rm -f "$work_dir/state/lock.json" "$work_dir/state/lock-created.json"
  rm -f "$work_dir/state/lock-replacement.json"
  rm -f "$work_dir/state/job.json" "$work_dir/state/job-created.json"
  rm -f "$work_dir/state/job-replacement.json"
  rm -f "$work_dir/state/job-deleted" "$work_dir/state/deletes.log"
  rm -f "$work_dir/state/cleanup-actions.log" "$work_dir/state/forbidden-exec.log"
  rm -f "$work_dir/state/kubeconfig-paths.log" "$work_dir/state/kubeconfig-rewritten"
  rm -f "$work_dir/state/logs-read"
  cp "$work_dir/kubeconfig-expected" "$work_dir/kubeconfig"
  chmod 600 "$work_dir/kubeconfig"
}

run_expect_failure() {
  local expected="$1"
  local output_file="$2"
  shift 2
  if "$repo_root/scripts/run-agent-email-receipt-proof.sh" "$@" >"$output_file" 2>&1; then
    printf 'fixture unexpectedly succeeded: %s\n' "$expected" >&2
    exit 1
  fi
  grep -Fqx "$expected" "$output_file"
}

reset_fake_run
success_output="$work_dir/success-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$success_output" 2>&1; then
  printf 'receipt-proof success fixture failed:\n' >&2
  sed -n '1,80p' "$success_output" >&2
  exit 1
fi
grep -Fqx "$proof" "$success_output"
if grep -Eq 'acc_[a-z2-7]{16}|outbound-dispatch-v1|witself-db|cell-2026-08|workers\.dev' "$success_output"; then
  echo "success output exposed a non-proof value" >&2
  exit 1
fi
test ! -e "$work_dir/state/forbidden-exec.log"

jq -e '
  .kind == "ConfigMap" and .metadata.name == "witself-agent-email-receipt-proof-lock" and
  .immutable == true and .metadata.labels["witself.io/cell"] == "civo-sandbox-usw2-dev" and
  (.data | keys | sort == ["WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT",
    "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID",
    "WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT"]) and
  (.data.UNRELATED_NON_SECRET == null) and
  (.data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY == null)
' "$work_dir/state/lock-created.json" >/dev/null
jq -e '
  .kind == "Job" and .metadata.name == "witself-agent-email-receipt-proof" and
  .metadata.labels["app.kubernetes.io/managed-by"] == "witself-operator" and
  .metadata.labels["witself.io/cell"] == "civo-sandbox-usw2-dev" and
  .spec.backoffLimit == 0 and .spec.activeDeadlineSeconds == 60 and
  .spec.ttlSecondsAfterFinished == 3600 and
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.enableServiceLinks == false and
  (.spec.template.spec.containers | length == 1)
' "$work_dir/state/job-created.json" >/dev/null
ruby "$repo_root/scripts/test-postgres-operation-policy.rb" \
  "$work_dir/state/job-created.json" civo-sandbox-usw2-dev "$FAKE_EXPECTED_NAMESPACE"
jq -e '
  .spec.template.spec.containers[0] as $runner |
  $runner.image == "ghcr.io/witwave-ai/images/witself-server:0.0.249" and
  $runner.command == ["/usr/local/bin/witself-worker"] and
  $runner.args == ["agent-email","receipt-replay",
    "--account-id","acc_aaaaaaaaaaaaaaaa",
    "--send-id","esnd_aaaaaaaaaaaaaaaa",
    "--expected-accepted-at","2026-08-16T01:02:03.123456Z",
    "--expected-attempt-count","1","--json"] and
  ($runner.env | map(.name) == ["WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY",
    "WITSELF_DATABASE_URL"]) and
  ($runner.env | all((.value == null) and (.valueFrom | keys == ["secretKeyRef"]))) and
  $runner.envFrom == [{"configMapRef":{"name":"witself-agent-email-receipt-proof-lock"}}] and
  $runner.securityContext.readOnlyRootFilesystem == true and
  $runner.securityContext.allowPrivilegeEscalation == false and
  $runner.securityContext.capabilities.drop == ["ALL"]
' "$work_dir/state/job-created.json" >/dev/null
grep -Fqx \
  '/apis/batch/v1/namespaces/witself/jobs/witself-agent-email-receipt-proof|proof-job-uid' \
  "$work_dir/state/deletes.log"
grep -Fqx \
  '/api/v1/namespaces/witself/configmaps/witself-agent-email-receipt-proof-lock|proof-lock-uid' \
  "$work_dir/state/deletes.log"
awk '
  $0 == "delete-job" { job = NR }
  $0 == "pods-absent" { pods = NR }
  $0 == "delete-configmap" { lock = NR }
  END { exit !(job > 0 && pods > job && lock > pods) }
' "$work_dir/state/cleanup-actions.log"

# Every kubectl call uses one immutable mode-0400 snapshot. Rewriting the
# operator's original kubeconfig after the first call cannot redirect any later
# source read, mutation, log read, or UID-preconditioned cleanup.
reset_fake_run
export FAKE_REWRITE_KUBECONFIG=true
kubeconfig_rewrite_output="$work_dir/kubeconfig-rewrite-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$kubeconfig_rewrite_output" 2>&1; then
  printf 'kubeconfig rewrite fixture unexpectedly failed:\n' >&2
  sed -n '1,80p' "$kubeconfig_rewrite_output" >&2
  exit 1
fi
grep -Fqx "$proof" "$kubeconfig_rewrite_output"
grep -Fqx rewritten-cross-cluster "$work_dir/kubeconfig"
test -e "$work_dir/state/kubeconfig-rewritten"
[ "$(sort -u "$work_dir/state/kubeconfig-paths.log" | wc -l | tr -d '[:space:]')" = 1 ]
if grep -Fqx "$work_dir/kubeconfig" "$work_dir/state/kubeconfig-paths.log"; then
  echo "a kubectl call used the mutable original kubeconfig" >&2
  exit 1
fi

reset_fake_run
export FAKE_REPLACE_WORKER_POD_OWNER=true
worker_owner_output="$work_dir/worker-owner-output"
run_expect_failure 'error: managed worker Pods are not exact, ready Deployment owners' \
  "$worker_owner_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

reset_fake_run
export FAKE_EXTRA_WORKER_POD=true
extra_worker_output="$work_dir/extra-worker-output"
run_expect_failure 'error: managed worker Pods are not exact, ready Deployment owners' \
  "$extra_worker_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

# Mutable ExternalSecret rotation remains supported, but the canonical DB
# Secret UID/RV must be unchanged after the exact proof Pod starts and after
# its logs are read. The value itself is never requested.
reset_fake_run
export FAKE_SECRET_DRIFT=db-poststart
db_poststart_output="$work_dir/db-poststart-output"
run_expect_failure 'error: managed Secret metadata drifted before receipt-proof read' \
  "$db_poststart_output" "${base_args[@]}"
test ! -e "$work_dir/state/logs-read"
if grep -Fq "$proof" "$db_poststart_output"; then
  echo "a proof escaped after pre-read DB Secret drift" >&2
  exit 1
fi

reset_fake_run
export FAKE_SECRET_DRIFT=db-postflight
db_postflight_output="$work_dir/db-postflight-output"
run_expect_failure 'error: managed Secret metadata drifted after receipt-proof read' \
  "$db_postflight_output" "${base_args[@]}"
test -e "$work_dir/state/logs-read"
if grep -Fq "$proof" "$db_postflight_output"; then
  echo "a proof escaped after post-read DB Secret drift" >&2
  exit 1
fi

reset_fake_run
export FAKE_REPLACE_POD=postlog
replacement_pod_output="$work_dir/replacement-pod-output"
run_expect_failure 'error: receipt-proof lock, Job, or owned Pod changed after proof read' \
  "$replacement_pod_output" "${base_args[@]}"
test -e "$work_dir/state/logs-read"
if grep -Fq "$proof" "$replacement_pod_output"; then
  echo "a proof escaped after Pod replacement" >&2
  exit 1
fi

reset_fake_run
export FAKE_REPLACE_JOB=postlog
replacement_job_output="$work_dir/replacement-job-output"
run_expect_failure 'error: receipt-proof lock, Job, or owned Pod changed after proof read' \
  "$replacement_job_output" "${base_args[@]}"
test -e "$work_dir/state/job.json"
[ "$(jq -r '.metadata.uid' "$work_dir/state/job.json")" = replacement-job-uid ]
test -e "$work_dir/state/lock.json"

reset_fake_run
export FAKE_REPLACE_LOCK=postlog
replacement_lock_output="$work_dir/replacement-lock-output"
run_expect_failure 'error: receipt-proof lock, Job, or owned Pod changed after proof read' \
  "$replacement_lock_output" "${base_args[@]}"
test -e "$work_dir/state/lock.json"
[ "$(jq -r '.metadata.uid' "$work_dir/state/lock.json")" = replacement-lock-uid ]

# UID-preconditioned cleanup cannot delete a same-name foreign replacement.
reset_fake_run
export FAKE_REPLACE_JOB_AT_CLEANUP=true
replacement_job_cleanup_output="$work_dir/replacement-job-cleanup-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$replacement_job_cleanup_output" 2>&1; then
  echo "replacement Job cleanup fixture unexpectedly failed" >&2
  exit 1
fi
grep -Fqx "$proof" "$replacement_job_cleanup_output"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$replacement_job_cleanup_output"
[ "$(jq -r '.metadata.uid' "$work_dir/state/job.json")" = replacement-job-uid ]
test -e "$work_dir/state/lock.json"

reset_fake_run
export FAKE_REPLACE_LOCK_AT_CLEANUP=true
replacement_lock_cleanup_output="$work_dir/replacement-lock-cleanup-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$replacement_lock_cleanup_output" 2>&1; then
  echo "replacement lock cleanup fixture unexpectedly failed" >&2
  exit 1
fi
grep -Fqx "$proof" "$replacement_lock_cleanup_output"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$replacement_lock_cleanup_output"
[ "$(jq -r '.metadata.uid' "$work_dir/state/lock.json")" = replacement-lock-uid ]

# The selector alone cannot prove cleanup: the exact Pod may be relabeled out
# of the Job selector or replaced under the same name. Both cases retain the
# fixed lock, and a foreign replacement is never mistaken for the proof Pod.
reset_fake_run
export FAKE_CLEANUP_POD_STATE=relabeled
relabeled_pod_cleanup_output="$work_dir/relabeled-pod-cleanup-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$relabeled_pod_cleanup_output" 2>&1; then
  echo "relabeled Pod cleanup fixture unexpectedly failed" >&2
  exit 1
fi
grep -Fqx "$proof" "$relabeled_pod_cleanup_output"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$relabeled_pod_cleanup_output"
test -e "$work_dir/state/lock.json"
if grep -Fq '/configmaps/witself-agent-email-receipt-proof-lock|' \
    "$work_dir/state/deletes.log"; then
  echo "cleanup removed the lock while the exact relabeled Pod remained" >&2
  exit 1
fi

reset_fake_run
export FAKE_CLEANUP_POD_STATE=replacement
replacement_pod_cleanup_output="$work_dir/replacement-pod-cleanup-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$replacement_pod_cleanup_output" 2>&1; then
  echo "replacement Pod cleanup fixture unexpectedly failed" >&2
  exit 1
fi
grep -Fqx "$proof" "$replacement_pod_cleanup_output"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$replacement_pod_cleanup_output"
test -e "$work_dir/state/lock.json"
if grep -Fq '/configmaps/witself-agent-email-receipt-proof-lock|' \
    "$work_dir/state/deletes.log"; then
  echo "cleanup removed the lock after a same-name Pod replacement" >&2
  exit 1
fi

# Namespace is intentionally mandatory; no implicit production namespace is
# permitted and validation fails before Kubernetes is contacted.
reset_fake_run
missing_namespace_output="$work_dir/missing-namespace-output"
run_expect_failure 'error: required arguments are missing' "$missing_namespace_output" \
  --cell civo-sandbox-usw2-dev \
  --kubeconfig "$work_dir/kubeconfig" \
  --context witself-civo-sandbox-usw2-dev \
  --expected-image ghcr.io/witwave-ai/images/witself-server:0.0.249 \
  --expected-config-checksum bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  --expected-replicas 2 \
  --account-id acc_aaaaaaaaaaaaaaaa \
  --send-id esnd_aaaaaaaaaaaaaaaa \
  --expected-accepted-at 2026-08-16T01:02:03.123456Z \
  --timeout-seconds 60
test ! -e "$work_dir/state/calls.log"

reset_fake_run
export FAKE_WRONG_REPLICAS=true
wrong_replicas_output="$work_dir/wrong-replicas-output"
run_expect_failure \
  'error: managed worker source is absent, ambiguous, or not ready for receipt proof' \
  "$wrong_replicas_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

reset_fake_run
export FAKE_JOB_LOOKUP_FAILURE=true
job_lookup_output="$work_dir/job-lookup-output"
run_expect_failure 'error: could not verify receipt-proof Job absence' \
  "$job_lookup_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

reset_fake_run
export FAKE_WRONG_CELL=true
wrong_cell_output="$work_dir/wrong-cell-output"
run_expect_failure 'error: managed cell identity is absent, ambiguous, or not fully converged' \
  "$wrong_cell_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"
test ! -e "$work_dir/state/job-created.json"

reset_fake_run
export FAKE_DISABLED=true
disabled_output="$work_dir/disabled-output"
run_expect_failure \
  'error: managed worker source is absent, ambiguous, or not ready for receipt proof' \
  "$disabled_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"
test ! -e "$work_dir/state/job-created.json"

reset_fake_run
export FAKE_LITERAL_PRIVATE_CONFIG=true
literal_output="$work_dir/literal-output"
run_expect_failure \
  'error: managed worker source is absent, ambiguous, or not ready for receipt proof' \
  "$literal_output" "${base_args[@]}"
if grep -Fq forbidden "$literal_output"; then
  echo "literal private config crossed the error boundary" >&2
  exit 1
fi

reset_fake_run
export FAKE_DISPATCH_IMMUTABLE=false
mutable_key_output="$work_dir/mutable-key-output"
run_expect_failure \
  'error: managed worker source is absent, ambiguous, or not ready for receipt proof' \
  "$mutable_key_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

reset_fake_run
export FAKE_SOURCE_DRIFT=prelock
prelock_output="$work_dir/prelock-output"
run_expect_failure 'error: managed worker source drifted before lock creation' \
  "$prelock_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"
test ! -e "$work_dir/state/job-created.json"

reset_fake_run
export FAKE_SOURCE_DRIFT=prejob
prejob_output="$work_dir/prejob-output"
run_expect_failure 'error: managed worker source drifted before Job creation' \
  "$prejob_output" "${base_args[@]}"
test -e "$work_dir/state/lock-created.json"
test ! -e "$work_dir/state/job-created.json"
test ! -e "$work_dir/state/lock.json"
grep -Fqx \
  '/api/v1/namespaces/witself/configmaps/witself-agent-email-receipt-proof-lock|proof-lock-uid' \
  "$work_dir/state/deletes.log"

reset_fake_run
export FAKE_EXISTING_JOB=true
existing_job_output="$work_dir/existing-job-output"
run_expect_failure 'error: an existing receipt-proof Job requires operator cleanup' \
  "$existing_job_output" "${base_args[@]}"
test ! -e "$work_dir/state/lock-created.json"

# If Kubernetes does not confirm Job creation, the helper cannot prove whether
# the mutation happened. It deliberately strands the fixed lock and never
# deletes a possibly foreign or still-running Job.
reset_fake_run
export FAKE_JOB_CREATE_FAILURE=true
job_create_output="$work_dir/job-create-output"
run_expect_failure \
  'error: receipt-proof Job creation was not confirmed; the fixed lock was retained' \
  "$job_create_output" "${base_args[@]}"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$job_create_output"
test -e "$work_dir/state/lock.json"
if [ -s "$work_dir/state/deletes.log" ]; then
  echo "ambiguous Job creation attempted to remove a fixed resource" >&2
  exit 1
fi

# Runner failures and malformed logs remain private. Neither the account nor
# arbitrary pod-log content may cross the helper's bounded error vocabulary.
reset_fake_run
export FAKE_RUNNER_EXIT=1
export FAKE_RUNNER_LOG='private-log-marker account=acc_aaaaaaaaaaaaaaaa secret=outbound-dispatch-v1'
runner_failure_output="$work_dir/runner-failure-output"
run_expect_failure 'error: receipt-proof Job failed' "$runner_failure_output" \
  "${base_args[@]}"
if grep -Eq 'private-log-marker|acc_[a-z2-7]{16}|outbound-dispatch-v1' "$runner_failure_output"; then
  echo "runner failure log crossed the operator boundary" >&2
  exit 1
fi

reset_fake_run
export FAKE_RUNNER_LOG="${proof%?},\"provider_message_id\":\"private-provider-marker\"}"
malformed_output="$work_dir/malformed-output"
run_expect_failure 'error: receipt-proof output failed closed structural validation' \
  "$malformed_output" "${base_args[@]}"
if grep -Fq private-provider-marker "$malformed_output"; then
  echo "malformed proof content crossed the operator boundary" >&2
  exit 1
fi

reset_fake_run
export FAKE_RUNNER_LOG=$'startup-noise\n'"$proof"
noisy_output="$work_dir/noisy-output"
run_expect_failure 'error: receipt-proof output failed closed structural validation' \
  "$noisy_output" "${base_args[@]}"

# A successful proof stays successful even if foreground deletion cannot be
# proven, but the warning is explicit and the lock remains as the safety fence.
reset_fake_run
export FAKE_BACKGROUND_DELETE=true
lingering_output="$work_dir/lingering-output"
if ! "$repo_root/scripts/run-agent-email-receipt-proof.sh" "${base_args[@]}" \
    >"$lingering_output" 2>&1; then
  printf 'lingering cleanup fixture unexpectedly failed:\n' >&2
  sed -n '1,80p' "$lingering_output" >&2
  exit 1
fi
grep -Fqx "$proof" "$lingering_output"
grep -Fqx \
  'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' \
  "$lingering_output"
test -e "$work_dir/state/lock.json"
if grep -Fq '/configmaps/witself-agent-email-receipt-proof-lock|' \
    "$work_dir/state/deletes.log"; then
  echo "cleanup removed the lock while the Job pod remained" >&2
  exit 1
fi

printf 'agent-email transient receipt-proof Job tests passed\n'
