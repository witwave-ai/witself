#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
canary="$repo_root/scripts/run-monitoring-alert-canary.sh"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-monitoring-canary-test.XXXXXX")"
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
chmod 700 "$work_dir"
work_dir="$(cd "$work_dir" && pwd -P)"
mkdir -m 700 "$work_dir/bin" "$work_dir/state" "$work_dir/output"

cat >"$work_dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

state="$FAKE_MONITORING_STATE"
scenario="${FAKE_MONITORING_SCENARIO:-success}"
context="${FAKE_MONITORING_CONTEXT:-founder-context}"
namespace=monitoring

if [ "${1:-}" = --request-timeout=30s ]; then
  shift
  request_bounded=true
else
  request_bounded=false
fi
[ "${1:-}" = --context ] && [ "${2:-}" = "$context" ] || exit 91
shift 2
[ "${1:-}" = -n ] && [ "${2:-}" = "$namespace" ] || exit 92
shift 2
printf '%s|%s\n' "$request_bounded" "$*" >>"$state/calls.log"

case "${1:-} ${2:-}" in
  "get prometheusrule")
    [ "$request_bounded" = true ] || exit 93
    [ "${3:-}" = witself-external-receiver-canary ] || exit 94
    case "$*" in
      *"--ignore-not-found -o name")
        if [ "$scenario" = preexisting ]; then
          printf '%s\n' 'prometheusrule.monitoring.coreos.com/witself-external-receiver-canary'
        elif [ -f "$state/rule.json" ]; then
          printf '%s\n' 'prometheusrule.monitoring.coreos.com/witself-external-receiver-canary'
        fi
        ;;
      *"--ignore-not-found -o json")
        if [ -f "$state/rule.json" ]; then
          if [ "$scenario" = uid_replaced ]; then
            jq '.metadata.uid = "foreign-rule-uid"' "$state/rule.json" >"$state/rule.replaced.json"
            mv "$state/rule.replaced.json" "$state/rule.json"
          fi
          cat "$state/rule.json"
        fi
        ;;
      *) exit 95 ;;
    esac
    ;;
  "get service")
    [ "$request_bounded" = true ] || exit 96
    [ "${3:-}" = -l ] || exit 97
    [ "${4:-}" = app=kube-prometheus-stack-alertmanager,release=witself-monitoring ] || exit 98
    [ "${5:-}" = -o ] && [ "${6:-}" = json ] || exit 99
    if [ "$scenario" = service_missing ]; then
      printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}'
      exit 0
    fi
    service_type=ClusterIP
    cluster_ip=10.96.12.34
    service_cell=civo-sandbox-usw2-dev
    if [ "$scenario" = service_public ]; then
      service_type=LoadBalancer
    fi
    if [ "$scenario" = service_wrong_cell ]; then
      service_cell=wrong-cell
    fi
    jq -n --arg type "$service_type" --arg ip "$cluster_ip" --arg cell "$service_cell" \
      --arg marker "$FAKE_SENSITIVE_MARKER" '{
        apiVersion:"v1",kind:"List",items:[{
          apiVersion:"v1",kind:"Service",
          metadata:{name:"witself-monitoring-kube-pr-alertmanager",
            namespace:"monitoring",
            labels:{app:"kube-prometheus-stack-alertmanager",release:"witself-monitoring",
              "witself.io/cell":$cell},
            annotations:{private_test_marker:$marker}},
          spec:{type:$type,clusterIP:$ip,externalIPs:[],
            ports:[{name:"http-web",port:9093,targetPort:9093}]}
        }]
      }'
    ;;
  "create -f")
    [ "$request_bounded" = true ] || exit 100
    [ "${3:-}" = - ] && [ "${4:-}" = -o ] && [ "${5:-}" = json ] || exit 101
    payload="$(cat)"
    printf '%s\n' "$payload" >"$state/create-input.json"
    jq -e '
      .apiVersion == "monitoring.coreos.com/v1" and
      .kind == "PrometheusRule" and
      .metadata.name == "witself-external-receiver-canary" and
      .metadata.labels.release == "witself-monitoring" and
      .metadata.labels["witself.io/monitoring-canary"] == "true" and
      (.metadata.annotations["witself.io/monitoring-canary-owner"] | length > 10) and
      .spec.groups[0].rules[0].alert == "WitselfExternalReceiverCanary" and
      .spec.groups[0].rules[0].expr == "vector(0) == 1" and
      .spec.groups[0].rules[0].labels.witself_alert == "true"
    ' <<<"$payload" >/dev/null || exit 101
    if [ "$scenario" = create_race ]; then
      jq -n '{apiVersion:"monitoring.coreos.com/v1",kind:"PrometheusRule",
        metadata:{name:"witself-external-receiver-canary",namespace:"monitoring",uid:"foreign-rule-uid"}}' \
        >"$state/rule.json"
      exit 1
    fi
    created="$(jq '.metadata.namespace = "monitoring" |
      .metadata.uid = "canary-rule-uid" |
      .metadata.resourceVersion = "canary-rule-rv-1"' <<<"$payload")"
    printf '%s\n' "$created" >"$state/rule.json"
    if [ "$scenario" = create_response_lost ]; then
      exit 1
    fi
    if [ "$scenario" = create_response_invalid ]; then
      jq '.spec.groups[0].rules[0].expr = "vector(0)"' <<<"$created"
      exit 0
    fi
    printf '%s\n' "$created"
    ;;
  "patch prometheusrule")
    [ "$request_bounded" = true ] || exit 112
    [ "${3:-}" = witself-external-receiver-canary ] || exit 113
    [ "${4:-}" = --type=json ] || exit 114
    [[ "${5:-}" == --patch-file=* ]] || exit 115
    patch_file="${5#--patch-file=}"
    [ -f "$patch_file" ] || exit 116
    [ "${6:-}" = -o ] && [ "${7:-}" = json ] || exit 117
    [ -f "$state/rule.json" ] || exit 1
    if [ "$scenario" = uid_replaced ]; then
      jq '.metadata.uid = "foreign-rule-uid"' "$state/rule.json" >"$state/rule.replaced.json"
      mv "$state/rule.replaced.json" "$state/rule.json"
      exit 1
    fi
    [ "$(jq -r '.metadata.uid' "$state/rule.json")" = canary-rule-uid ] || exit 1
    current_resource_version="$(jq -r '.metadata.resourceVersion' "$state/rule.json")"
    current_expression="$(jq -r '.spec.groups[0].rules[0].expr' "$state/rule.json")"
    if [ "$current_resource_version" = canary-rule-rv-1 ] &&
      [ "$current_expression" = 'vector(0) == 1' ]; then
      replacement_expression='vector(1)'
      next_resource_version=canary-rule-rv-2
    elif [ "$current_resource_version" = canary-rule-rv-2 ] &&
      [ "$current_expression" = 'vector(1)' ]; then
      replacement_expression='vector(0) == 1'
      next_resource_version=canary-rule-rv-3
    else
      exit 118
    fi
    jq -e --arg resource_version "$current_resource_version" \
      --arg expected "$current_expression" --arg replacement "$replacement_expression" '
      length == 4 and
      .[0] == {op:"test",path:"/metadata/uid",value:"canary-rule-uid"} and
      .[1] == {op:"test",path:"/metadata/resourceVersion",value:$resource_version} and
      .[2] == {op:"test",path:"/spec/groups/0/rules/0/expr",value:$expected} and
      .[3] == {op:"replace",path:"/spec/groups/0/rules/0/expr",value:$replacement}
    ' "$patch_file" >/dev/null || exit 118
    jq --arg replacement "$replacement_expression" \
      --arg resource_version "$next_resource_version" \
      '.spec.groups[0].rules[0].expr = $replacement |
       .metadata.resourceVersion = $resource_version' "$state/rule.json" \
      >"$state/rule.patched.json"
    mv "$state/rule.patched.json" "$state/rule.json"
    cat "$state/rule.json"
    ;;
  "delete --raw="*)
    [ "$request_bounded" = true ] || exit 102
    [ "${1:-}" = delete ] || exit 103
    [ "${2:-}" = '--raw=/apis/monitoring.coreos.com/v1/namespaces/monitoring/prometheusrules/witself-external-receiver-canary' ] || exit 104
    [ "${3:-}" = -f ] && [ -f "${4:-}" ] || exit 105
    [ "$(jq -r '.apiVersion' "$4")" = v1 ] || exit 106
    [ "$(jq -r '.kind' "$4")" = DeleteOptions ] || exit 107
    [ "$(jq -r '.propagationPolicy' "$4")" = Foreground ] || exit 108
    requested_uid="$(jq -er '.preconditions.uid' "$4")"
    [ "$requested_uid" = canary-rule-uid ] || exit 109
    [ -f "$state/rule.json" ] || exit 1
    [ "$(jq -r '.metadata.uid' "$state/rule.json")" = canary-rule-uid ] || exit 1
    rm -f "$state/rule.json"
    : >"$state/rule-deleted"
    jq -n --arg uid "$requested_uid" '{kind:"Status",status:"Success",
      details:{name:"witself-external-receiver-canary",kind:"prometheusrules",uid:$uid}}'
    ;;
  "port-forward service/witself-monitoring-kube-pr-alertmanager")
    [ "$request_bounded" = false ] || exit 110
    [ "${3:-}" = 19093:9093 ] || exit 111
    : >"$state/port-forward-started"
    exec /bin/sleep 3600
    ;;
  *)
    exit 109
    ;;
esac
EOF
chmod 700 "$work_dir/bin/kubectl"

cat >"$work_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url="${*: -1}"
printf '%s\n' "$url" >>"$FAKE_MONITORING_STATE/curl.log"
case "$url" in
  http://127.0.0.1:19093/-/ready)
    printf '%s\n' ready
    ;;
  http://127.0.0.1:19093/api/v2/alerts)
    if [ "$FAKE_MONITORING_SCENARIO" = firing_missing ]; then
      printf '%s\n' '[]'
    elif [ -f "$FAKE_MONITORING_STATE/rule.json" ] &&
      [ "$(jq -r '.spec.groups[0].rules[0].expr // ""' "$FAKE_MONITORING_STATE/rule.json")" = 'vector(1)' ]; then
      printf '%s\n' '[{"labels":{"alertname":"WitselfExternalReceiverCanary"},"status":{"state":"active"}}]'
    else
      printf '%s\n' '[]'
    fi
    ;;
  *) exit 1 ;;
esac
EOF
chmod 700 "$work_dir/bin/curl"

cat >"$work_dir/bin/sleep" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${1:-}" >>"$FAKE_MONITORING_STATE/sleep.log"
exit 0
EOF
chmod 700 "$work_dir/bin/sleep"

cat >"$work_dir/bin/ln" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "${FAKE_MONITORING_SCENARIO:-}" = evidence_race ]; then
  destination="${2:-}"
  printf '%s\n' 'racer-owned-evidence' >"$destination"
  exit 1
fi
exec /bin/ln "$@"
EOF
chmod 700 "$work_dir/bin/ln"

file_mode() {
  local path="$1" mode
  mode="$(stat -f '%Lp' "$path" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$path" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

reset_case() {
  find "$work_dir/state" -depth -mindepth 1 -delete
  find "$work_dir/output" -depth -mindepth 1 -delete
  : >"$work_dir/stdout"
  : >"$work_dir/stderr"
}

run_canary() {
  local scenario="$1"
  shift
  env \
    PATH="$work_dir/bin:$PATH" \
    FAKE_MONITORING_STATE="$work_dir/state" \
    FAKE_MONITORING_SCENARIO="$scenario" \
    FAKE_MONITORING_CONTEXT=founder-context \
    FAKE_SENSITIVE_MARKER=receiver-private-marker \
    bash "$canary" \
      --context founder-context \
      --cell civo-sandbox-usw2-dev \
      --out "$work_dir/output/evidence.json" \
      "$@" \
      >"$work_dir/stdout" 2>"$work_dir/stderr"
}

reject_call() {
  local pattern="$1"
  if [ -f "$work_dir/state/calls.log" ] && grep -Fq -- "$pattern" "$work_dir/state/calls.log"; then
    echo "unexpected kubectl call containing: $pattern" >&2
    cat "$work_dir/state/calls.log" >&2
    exit 1
  fi
}

require_call() {
  local pattern="$1"
  if ! [ -f "$work_dir/state/calls.log" ] || ! grep -Fq -- "$pattern" "$work_dir/state/calls.log"; then
    echo "missing kubectl call containing: $pattern" >&2
    cat "$work_dir/state/calls.log" 2>/dev/null >&2 || true
    exit 1
  fi
}

assert_private_marker_absent() {
  if grep -Fq receiver-private-marker \
    "$work_dir/stdout" "$work_dir/stderr" "$work_dir/output/evidence.json" 2>/dev/null; then
    echo "canary output exposed a private receiver marker" >&2
    exit 1
  fi
}

# The tool is never mutating by default. Argument validation must finish before
# any cluster process is invoked.
reset_case
if run_canary success; then
  echo "monitoring canary accepted a run without --apply" >&2
  exit 1
fi
reject_call '|'
[ ! -e "$work_dir/output/evidence.json" ]

# A malformed success response does not establish ownership even if the create
# committed. The fixed-name object remains for explicit operator inspection.
reset_case
if run_canary create_response_invalid --apply; then
  echo "monitoring canary accepted a malformed create response" >&2
  exit 1
fi
grep -Fq 'returned no exact ownership identity; inspect the fixed rule name' "$work_dir/stderr"
require_call 'create -f - -o json'
reject_call 'delete --raw='
[ ! -e "$work_dir/state/rule-deleted" ]
[ "$(jq -r '.metadata.uid' "$work_dir/state/rule.json")" = canary-rule-uid ]
[ "$(jq -r '.spec.groups[0].rules[0].expr' "$work_dir/state/rule.json")" = 'vector(0) == 1' ]
[ ! -e "$work_dir/output/evidence.json" ]

# A pre-existing fixed-name rule is somebody else's object. Refuse before the
# Service query and never run cleanup against it.
reset_case
if run_canary preexisting --apply; then
  echo "monitoring canary replaced a pre-existing rule" >&2
  exit 1
fi
grep -Fq 'already exists; refusing to replace it' "$work_dir/stderr"
reject_call 'get service'
reject_call 'create -f'
reject_call 'delete --raw='
[ ! -e "$work_dir/output/evidence.json" ]

# Alertmanager discovery is an exact private-Service preflight. Missing or
# public services cannot create a rule or start a port-forward.
for scenario in service_missing service_public service_wrong_cell; do
  reset_case
  if run_canary "$scenario" --apply; then
    echo "monitoring canary accepted unsafe service scenario $scenario" >&2
    exit 1
  fi
  require_call 'get service -l app=kube-prometheus-stack-alertmanager,release=witself-monitoring -o json'
  grep -Fq 'bounded private Alertmanager ClusterIP Service' "$work_dir/stderr"
  reject_call 'create -f'
  reject_call 'delete --raw='
  reject_call 'port-forward'
  [ ! -e "$work_dir/output/evidence.json" ]
  assert_private_marker_absent
done

# A create collision after the absence preflight remains racer-owned. Because
# created never flips true, EXIT cleanup must not issue any delete.
reset_case
if run_canary create_race --apply; then
  echo "monitoring canary ignored a create race" >&2
  exit 1
fi
require_call 'create -f - -o json'
reject_call 'delete --raw='
[ ! -e "$work_dir/state/rule-deleted" ]
[ "$(jq -r '.metadata.uid' "$work_dir/state/rule.json")" = foreign-rule-uid ]
[ ! -e "$work_dir/output/evidence.json" ]

# A committed create whose response is lost has no returned UID. It remains for
# explicit operator reconciliation; cleanup must not infer ownership from name
# or annotation and must not emit acceptance evidence.
reset_case
if run_canary create_response_lost --apply; then
  echo "monitoring canary accepted an indeterminate create response" >&2
  exit 1
fi
grep -Fq 'creation outcome is indeterminate; inspect the fixed rule name' "$work_dir/stderr"
require_call 'create -f - -o json'
reject_call 'delete --raw='
[ ! -e "$work_dir/state/rule-deleted" ]
[ "$(jq -r '.metadata.uid' "$work_dir/state/rule.json")" = canary-rule-uid ]
[ "$(jq -r '.spec.groups[0].rules[0].expr' "$work_dir/state/rule.json")" = 'vector(0) == 1' ]
[ ! -e "$work_dir/output/evidence.json" ]

# Even after a successful create, a UID replacement before the firing transition
# fails the JSON Patch UID/resourceVersion tests and is never deleted. The raw
# DeleteOptions UID precondition still protects the owned-object path.
reset_case
if run_canary uid_replaced --apply; then
  echo "monitoring canary deleted a replacement rule" >&2
  exit 1
fi
require_call 'patch prometheusrule witself-external-receiver-canary --type=json --patch-file='
reject_call 'delete --raw='
[ ! -e "$work_dir/state/rule-deleted" ]
[ "$(jq -r '.metadata.uid' "$work_dir/state/rule.json")" = foreign-rule-uid ]
[ ! -e "$work_dir/output/evidence.json" ]

# A failure after the owned rule is created exercises EXIT cleanup. It must use
# the exact UID precondition and leave neither the rule nor partial evidence.
reset_case
if run_canary firing_missing --apply; then
  echo "monitoring canary accepted missing firing state" >&2
  exit 1
fi
grep -Fq 'canary did not reach Alertmanager' "$work_dir/stderr"
require_call 'delete --raw=/apis/monitoring.coreos.com/v1/namespaces/monitoring/prometheusrules/witself-external-receiver-canary -f '
[ -e "$work_dir/state/rule-deleted" ]
[ ! -e "$work_dir/state/rule.json" ]
[ ! -e "$work_dir/output/evidence.json" ]

# Publication is create-only and atomic. A racer that claims the final path is
# preserved byte-for-byte, while the already-owned synthetic rule is still
# safely removed and no temporary evidence file remains.
reset_case
if run_canary evidence_race --apply; then
  echo "monitoring canary overwrote a racing evidence path" >&2
  exit 1
fi
grep -Fq 'evidence path was created concurrently; refusing overwrite' "$work_dir/stderr"
[ "$(cat "$work_dir/output/evidence.json")" = racer-owned-evidence ]
[ -e "$work_dir/state/rule-deleted" ]
[ ! -e "$work_dir/state/rule.json" ]
if find "$work_dir/output" -maxdepth 1 -name '.witself-monitoring-canary.*' -print -quit | grep -q .; then
  echo "monitoring canary left private evidence staging behind" >&2
  exit 1
fi

# Happy path: create one synthetic rule, observe firing via the local
# Alertmanager API, delete only the owned UID, observe resolution, and write a
# sanitized 0600 receipt that makes no external-delivery claim.
reset_case
run_canary success --apply
require_call 'get prometheusrule witself-external-receiver-canary --ignore-not-found -o name'
require_call 'get service -l app=kube-prometheus-stack-alertmanager,release=witself-monitoring -o json'
require_call 'create -f - -o json'
require_call 'port-forward service/witself-monitoring-kube-pr-alertmanager 19093:9093'
require_call 'patch prometheusrule witself-external-receiver-canary --type=json --patch-file='
require_call 'delete --raw=/apis/monitoring.coreos.com/v1/namespaces/monitoring/prometheusrules/witself-external-receiver-canary -f '
[ -e "$work_dir/state/rule-deleted" ]
[ ! -e "$work_dir/state/rule.json" ]
[ "$(file_mode "$work_dir/output/evidence.json")" = 600 ]
jq -e '
  .schema == "witself.monitoring-alert-canary.v1" and
  .cell == "civo-sandbox-usw2-dev" and
  .firing_observed == true and
  .firing_dwell_seconds == 45 and
  .rule_false_observed == true and
  .resolved_observed == true and
  .resolved_dwell_seconds == 310 and
  .external_receiver_firing_receipt_retained == false and
  .external_receiver_resolved_receipt_retained == false and
  (keys | sort == ["cell","external_receiver_firing_receipt_retained",
    "external_receiver_resolved_receipt_retained","firing_dwell_seconds",
    "firing_observed","observed_at","resolved_dwell_seconds",
    "resolved_observed","rule_false_observed","schema"])
' "$work_dir/output/evidence.json" >/dev/null
grep -Fq 'local firing and resolution verified' "$work_dir/stdout"
jq -e '.spec.groups[0].rules[0].alert == "WitselfExternalReceiverCanary" and
  .spec.groups[0].rules[0].expr == "vector(0) == 1"' \
  "$work_dir/state/create-input.json" >/dev/null
grep -Fqx 45 "$work_dir/state/sleep.log"
grep -Fqx 310 "$work_dir/state/sleep.log"
if grep -Eiq '(https?://|receiver-private-marker|token|secret)' \
  "$work_dir/output/evidence.json"; then
  echo "monitoring canary evidence included receiver or secret material" >&2
  exit 1
fi
assert_private_marker_absent

echo "monitoring alert canary tests passed"
