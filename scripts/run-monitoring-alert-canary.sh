#!/usr/bin/env bash
set -euo pipefail

umask 077

usage() {
  echo "usage: $0 --context CONTEXT --cell CELL --out ABSENT_ABSOLUTE_PATH --apply" >&2
  exit 2
}

context=""
cell=""
out=""
apply=false
while (($#)); do
  case "$1" in
    --context) [[ $# -ge 2 ]] || usage; context="$2"; shift 2 ;;
    --cell) [[ $# -ge 2 ]] || usage; cell="$2"; shift 2 ;;
    --out) [[ $# -ge 2 ]] || usage; out="$2"; shift 2 ;;
    --apply) apply=true; shift ;;
    *) usage ;;
  esac
done

[[ "$apply" == true ]] || usage
[[ "$context" =~ ^[A-Za-z0-9._-]{1,253}$ ]] || usage
[[ "$cell" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || usage
[[ "$out" == /* && ! -e "$out" && ! -L "$out" ]] || usage
out_parent="$(dirname "$out")"
[[ -d "$out_parent" && ! -L "$out_parent" ]] || usage

namespace=monitoring
rule=witself-external-receiver-canary
expected_service=witself-monitoring-kube-pr-alertmanager
port=19093
firing_dwell_seconds=45
resolved_dwell_seconds=310
port_log="$(mktemp "${TMPDIR:-/tmp}/witself-monitoring-canary.XXXXXX")"
delete_options="$(mktemp "${TMPDIR:-/tmp}/witself-monitoring-canary-delete.XXXXXX")"
delete_result="$(mktemp "${TMPDIR:-/tmp}/witself-monitoring-canary-delete-result.XXXXXX")"
patch_options="$(mktemp "${TMPDIR:-/tmp}/witself-monitoring-canary-patch.XXXXXX")"
patch_result="$(mktemp "${TMPDIR:-/tmp}/witself-monitoring-canary-patch-result.XXXXXX")"
evidence_tmp=""
port_pid=""
created=false
rule_uid=""
rule_resource_version=""
run_marker="canary-$(basename "$port_log")-$$"
kube=(kubectl --request-timeout=30s --context "$context" -n "$namespace")

delete_created_rule() {
  [[ "$created" == true && -n "$rule_uid" ]] || return 0

  local current current_uid
  if ! current="$("${kube[@]}" get prometheusrule "$rule" --ignore-not-found -o json 2>>"$port_log")"; then
    return 1
  fi
  if [[ -z "$current" ]]; then
    created=false
    return 0
  fi
  if ! current_uid="$(jq -er '.metadata.uid | strings | select(length > 0)' <<<"$current" 2>/dev/null)"; then
    return 1
  fi
  [[ "$current_uid" == "$rule_uid" ]] || return 1

  jq -n --arg uid "$rule_uid" '{
    apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Foreground",
    preconditions:{uid:$uid}
  }' >"$delete_options"
  chmod 0600 "$delete_options"
  if ! "${kube[@]}" delete \
    --raw="/apis/monitoring.coreos.com/v1/namespaces/monitoring/prometheusrules/$rule" \
    -f "$delete_options" >"$delete_result" 2>>"$port_log"; then
    return 1
  fi
  if ! jq -e --arg name "$rule" --arg uid "$rule_uid" '
    (.kind == "Status" and .status == "Success" and
      .details.name == $name and .details.kind == "prometheusrules" and
      ((.details.uid // $uid) == $uid)) or
    (.kind == "PrometheusRule" and .metadata.name == $name and .metadata.uid == $uid)
  ' "$delete_result" >/dev/null 2>&1; then
    return 1
  fi

  local deadline=$((SECONDS + 30))
  while true; do
    if ! current="$("${kube[@]}" get prometheusrule "$rule" \
      --ignore-not-found -o json 2>>"$port_log")"; then
      return 1
    fi
    if [[ -z "$current" ]]; then
      created=false
      return 0
    fi
    if ! current_uid="$(jq -er '.metadata.uid | strings | select(length > 0)' \
      <<<"$current" 2>/dev/null)"; then
      return 1
    fi
    [[ "$current_uid" == "$rule_uid" ]] || return 1
    ((SECONDS < deadline)) || return 1
    sleep 1
  done
}

set_created_rule_expression() {
  [[ "$created" == true && -n "$rule_uid" && -n "$rule_resource_version" ]] || return 1
  local expected_expression="$1"
  local replacement_expression="$2"

  jq -n --arg uid "$rule_uid" --arg resource_version "$rule_resource_version" \
    --arg expected "$expected_expression" --arg replacement "$replacement_expression" '[
    {op:"test",path:"/metadata/uid",value:$uid},
    {op:"test",path:"/metadata/resourceVersion",value:$resource_version},
    {op:"test",path:"/spec/groups/0/rules/0/expr",value:$expected},
    {op:"replace",path:"/spec/groups/0/rules/0/expr",value:$replacement}
  ]' >"$patch_options"
  chmod 0600 "$patch_options"
  if ! "${kube[@]}" patch prometheusrule "$rule" --type=json \
    --patch-file="$patch_options" -o json >"$patch_result" 2>>"$port_log"; then
    return 1
  fi
  rule_resource_version="$(jq -er --arg uid "$rule_uid" \
    --arg replacement "$replacement_expression" '
    select(.apiVersion == "monitoring.coreos.com/v1" and .kind == "PrometheusRule")
    | select(.metadata.name == "witself-external-receiver-canary" and .metadata.uid == $uid)
    | select(.spec.groups[0].rules[0].expr == $replacement)
    | .metadata.resourceVersion | strings | select(length > 0)
  ' "$patch_result" 2>/dev/null)" || return 1
}

cleanup() {
  set +e
  delete_created_rule >/dev/null 2>&1 || true
  if [[ -n "$port_pid" ]]; then
    kill "$port_pid" >/dev/null 2>&1 || true
    wait "$port_pid" >/dev/null 2>&1 || true
  fi
  rm -f "$port_log" "$delete_options" "$delete_result" \
    "$patch_options" "$patch_result"
  if [[ -n "$evidence_tmp" ]]; then
    rm -f "$evidence_tmp"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

existing_rule="$("${kube[@]}" get prometheusrule "$rule" \
  --ignore-not-found -o name 2>>"$port_log")" || {
  echo "could not verify monitoring canary rule absence" >&2
  exit 1
}
[[ -z "$existing_rule" ]] || {
  echo "monitoring canary rule already exists; refusing to replace it" >&2
  exit 1
}

services_json="$("${kube[@]}" get service \
  -l 'app=kube-prometheus-stack-alertmanager,release=witself-monitoring' \
  -o json 2>>"$port_log")" || {
  echo "could not inspect the private Alertmanager Service" >&2
  exit 1
}
service="$(jq -er --arg namespace "$namespace" --arg name "$expected_service" \
  --arg cell "$cell" '
  select(.apiVersion == "v1" and .kind == "List")
  | select((.items | length) == 1)
  | .items[0]
  | select(.metadata.namespace == $namespace and .metadata.name == $name)
  | select(.metadata.labels.app == "kube-prometheus-stack-alertmanager")
  | select(.metadata.labels.release == "witself-monitoring")
  | select(.metadata.labels["witself.io/cell"] == $cell)
  | select(.spec.type == "ClusterIP")
  | select((.spec.clusterIP // "") != "" and .spec.clusterIP != "None")
  | select(((.spec.externalIPs // []) | length) == 0)
  | select(any(.spec.ports[]?;
      .name == "http-web" and .port == 9093 and
      ((.targetPort | tostring) == "9093" or .targetPort == "http-web")))
  | "service/" + .metadata.name
' <<<"$services_json" 2>/dev/null)" || {
  echo "expected exactly one bounded private Alertmanager ClusterIP Service" >&2
  exit 1
}

rule_manifest="$(jq -cn --arg marker "$run_marker" '{
  apiVersion:"monitoring.coreos.com/v1",kind:"PrometheusRule",
  metadata:{name:"witself-external-receiver-canary",
    annotations:{"witself.io/monitoring-canary-owner":$marker},
    labels:{release:"witself-monitoring","witself.io/monitoring-canary":"true"}},
  spec:{groups:[{name:"witself-receiver-canary",interval:"15s",rules:[{
    alert:"WitselfExternalReceiverCanary",expr:"vector(0) == 1",
    labels:{severity:"warning",service:"monitoring-canary",witself_alert:"true"},
    annotations:{summary:"Witself external receiver canary is firing.",
      runbook:"docs/runbooks.md#founder-open-plane-monitoring"}
  }]}]}
}')"
if created_json="$(printf '%s\n' "$rule_manifest" | \
  "${kube[@]}" create -f - -o json 2>>"$port_log")"; then
  :
else
  # `kubectl create` can lose its response after the API server commits. Without
  # the returned UID, this process has no deletion authority: leave the fixed
  # name for an operator to inspect rather than risk deleting a racing object.
  echo "monitoring canary rule creation outcome is indeterminate; inspect the fixed rule name before retrying" >&2
  exit 1
fi
created_identity="$(jq -cer --argjson expected "$rule_manifest" --arg marker "$run_marker" '
  select(.apiVersion == "monitoring.coreos.com/v1" and .kind == "PrometheusRule")
  | select(.metadata.name == "witself-external-receiver-canary" and .metadata.namespace == "monitoring")
  | select(.metadata.labels.release == "witself-monitoring")
  | select(.metadata.labels["witself.io/monitoring-canary"] == "true")
  | select(.metadata.annotations["witself.io/monitoring-canary-owner"] == $marker)
  | select(.spec == $expected.spec)
  | select(.metadata.uid | strings | test("^[A-Za-z0-9._:-]{1,128}$"))
  | select(.metadata.resourceVersion | strings | test("^[A-Za-z0-9._:-]{1,128}$"))
  | {uid:.metadata.uid,resource_version:.metadata.resourceVersion}
' <<<"$created_json" 2>/dev/null)" || {
  echo "created monitoring canary rule returned no exact ownership identity; inspect the fixed rule name" >&2
  exit 1
}
rule_uid="$(jq -r '.uid' <<<"$created_identity")"
rule_resource_version="$(jq -r '.resource_version' <<<"$created_identity")"
created=true

set_created_rule_expression "vector(0) == 1" "vector(1)" || {
  echo "could not safely set the owned monitoring canary rule firing" >&2
  exit 1
}

kubectl --context "$context" -n "$namespace" port-forward "$service" \
  "$port:9093" >"$port_log" 2>&1 &
port_pid=$!

for _ in $(seq 1 30); do
  if curl -fsS --connect-timeout 2 --max-time 5 \
    "http://127.0.0.1:$port/-/ready" >/dev/null 2>&1; then
    break
  fi
  kill -0 "$port_pid" >/dev/null 2>&1 || {
    echo "Alertmanager port-forward exited before readiness" >&2
    exit 1
  }
  sleep 1
done
curl -fsS --connect-timeout 2 --max-time 5 \
  "http://127.0.0.1:$port/-/ready" >/dev/null 2>&1 || {
  echo "Alertmanager did not become ready" >&2
  exit 1
}

firing=false
for _ in $(seq 1 24); do
  if curl -fsS --connect-timeout 2 --max-time 5 \
    "http://127.0.0.1:$port/api/v2/alerts" 2>/dev/null |
    jq -e 'any(.[]; .labels.alertname == "WitselfExternalReceiverCanary" and .status.state == "active")' \
    >/dev/null; then
    firing=true
    break
  fi
  sleep 5
done
[[ "$firing" == true ]] || { echo "canary did not reach Alertmanager" >&2; exit 1; }

sleep "$firing_dwell_seconds"
curl -fsS --connect-timeout 2 --max-time 5 \
  "http://127.0.0.1:$port/api/v2/alerts" 2>/dev/null |
  jq -e 'any(.[]; .labels.alertname == "WitselfExternalReceiverCanary" and .status.state == "active")' \
  >/dev/null || {
  echo "canary did not remain firing through the external receiver group wait" >&2
  exit 1
}

set_created_rule_expression "vector(1)" "vector(0) == 1" || {
  echo "could not safely set the owned monitoring canary rule false" >&2
  exit 1
}
resolved=false
for _ in $(seq 1 24); do
  if curl -fsS --connect-timeout 2 --max-time 5 \
    "http://127.0.0.1:$port/api/v2/alerts" 2>/dev/null |
    jq -e 'all(.[]; .labels.alertname != "WitselfExternalReceiverCanary")' \
    >/dev/null; then
    resolved=true
    break
  fi
  sleep 5
done
[[ "$resolved" == true ]] || { echo "canary did not resolve in Alertmanager" >&2; exit 1; }

sleep "$resolved_dwell_seconds"
curl -fsS --connect-timeout 2 --max-time 5 \
  "http://127.0.0.1:$port/api/v2/alerts" 2>/dev/null |
  jq -e 'all(.[]; .labels.alertname != "WitselfExternalReceiverCanary")' \
  >/dev/null || {
  echo "canary reappeared during the external receiver resolution interval" >&2
  exit 1
}

delete_created_rule || {
  echo "could not safely delete the owned monitoring canary rule" >&2
  exit 1
}

observed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
evidence_tmp="$(mktemp "$out_parent/.witself-monitoring-canary.XXXXXX")"
jq -n --arg cell "$cell" --arg observed_at "$observed_at" \
  --argjson firing_dwell_seconds "$firing_dwell_seconds" \
  --argjson resolved_dwell_seconds "$resolved_dwell_seconds" \
  '{schema:"witself.monitoring-alert-canary.v1",cell:$cell,observed_at:$observed_at,
    firing_observed:true,firing_dwell_seconds:$firing_dwell_seconds,
    rule_false_observed:true,resolved_observed:true,
    resolved_dwell_seconds:$resolved_dwell_seconds,
    external_receiver_firing_receipt_retained:false,
    external_receiver_resolved_receipt_retained:false}' \
  >"$evidence_tmp"
chmod 0600 "$evidence_tmp"
jq -e '
  .schema == "witself.monitoring-alert-canary.v1" and
  .firing_observed == true and .rule_false_observed == true and
  .resolved_observed == true and
  .external_receiver_firing_receipt_retained == false and
  .external_receiver_resolved_receipt_retained == false
' "$evidence_tmp" >/dev/null
if ! ln "$evidence_tmp" "$out"; then
  echo "monitoring canary evidence path was created concurrently; refusing overwrite" >&2
  exit 1
fi
rm -f "$evidence_tmp"
evidence_tmp=""

echo "local firing and resolution verified; retain separate external receiver receipts before closing any gate"
