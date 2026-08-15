#!/usr/bin/env bash
# Prove the released cell receive boundary across one Personal -> Professional
# plan transition. The operator supplies one already-existing dedicated target;
# this script never creates accounts, mutates plans, reaches Cloudflare, or
# retries a signed POST whose response may have been lost.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: run-agent-email-cell-smoke.sh \
  --cell CELL \
  --kubeconfig FILE \
  --context CONTEXT \
  --phase disabled|entitled|cleanup \
  --target-file ABSOLUTE_PRIVATE_JSON \
  --state-file ABSOLUTE_PRIVATE_JSON \
  [--agent-token-file ABSOLUTE_PRIVATE_FILE \
   --relay-key-id KEY_ID --relay-private-key-file ABSOLUTE_PRIVATE_FILE] \
  [--namespace NAMESPACE] [--deployment DEPLOYMENT] [--service SERVICE]

disabled requires Personal (`free`) effective policy and creates a new private
state file. entitled consumes that same file after an external, audited plan
change to Professional (`standard`). cleanup performs recovery-only deletion
of an exact untouched synthetic message, if one exists. The relay key options
are required for disabled and entitled and forbidden for cleanup.

The signed phases require the same full agent token file. The token proves the
installed owner surface returns feature_not_enabled on Personal and the exact
mailbox after the Professional transition; its digest is fenced in state so a
different credential cannot masquerade as a no-reinstall proof.

The target JSON has exactly this private schema:
{
  "schema_version": 1,
  "account_id": "acc_...",
  "realm_id": "realm_...",
  "agent_id": "agent_...",
  "recipient": "agent-name.realm-id@witmail.net",
  "disabled_plan": "free",
  "entitled_plan": "standard"
}

Private inputs and the state parent must be mode 0600/0700 as applicable and
must live outside every Git worktree. The state file is crash-recovery evidence;
do not discard it after an indeterminate request. This harness never sends mail
through the public provider path.
EOF
}

die() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is unavailable"
}

file_mode() {
  local mode
  mode="$(stat -f '%Lp' "$1" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

file_identity() {
  local identity
  identity="$(stat -f '%d:%i:%z:%m' "$1" 2>/dev/null || true)"
  if [[ ! "$identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]]; then
    identity="$(stat -c '%d:%i:%s:%Y' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$identity"
}

canonical_file_path() {
  local path="$1"
  local parent base resolved_parent
  case "$path" in /*) ;; *) return 1 ;; esac
  parent="$(dirname "$path")"
  base="$(basename "$path")"
  [ "$base" != . ] && [ "$base" != .. ] || return 1
  [ -d "$parent" ] && [ ! -L "$parent" ] || return 1
  resolved_parent="$(cd "$parent" && pwd -P)" || return 1
  [ "$path" = "$resolved_parent/$base" ] || return 1
  printf '%s\n' "$path"
}

path_is_in_git_worktree() {
  local path="$1"
  local worktree
  while IFS= read -r worktree; do
    case "$path" in "$worktree"|"$worktree"/*) return 0 ;; esac
  done < <(git -C "$REPO_ROOT" worktree list --porcelain | sed -n 's/^worktree //p')
  return 1
}

require_private_parent() {
  local path="$1"
  local parent mode
  parent="$(dirname "$path")"
  mode="$(file_mode "$parent")"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die "could not verify private parent permissions"
  (( (8#$mode & 8#077) == 0 )) || die "private parent must not be accessible by group or other users"
  if path_is_in_git_worktree "$path"; then
    die "private artifacts must be outside every Git worktree"
  fi
  return 0
}

require_private_file() {
  local label="$1"
  local path="$2"
  local maximum_bytes="$3"
  local mode size
  canonical_file_path "$path" >/dev/null || die "$label path must be canonical and absolute"
  [ -f "$path" ] && [ ! -L "$path" ] || die "$label must be a regular non-symbolic file"
  mode="$(file_mode "$path")"
  [ "$mode" = 600 ] || die "$label must have mode 0600"
  size="$(wc -c <"$path" | tr -d '[:space:]')"
  if [[ ! "$size" =~ ^[0-9]+$ ]] || (( size < 1 || size > maximum_bytes )); then
    die "$label has an invalid size"
  fi
}

snapshot_file() {
  local source="$1"
  local destination="$2"
  local before after
  before="$(file_identity "$source")"
  [[ "$before" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]] || die "private input identity is unavailable"
  cp "$source" "$destination"
  chmod 600 "$destination"
  after="$(file_identity "$source")"
  if [ "$before" != "$after" ] || ! cmp -s "$source" "$destination"; then
    die "private input changed while it was read"
  fi
}

sha256_file() {
  node -e '
    const fs=require("node:fs"), crypto=require("node:crypto");
    process.stdout.write(crypto.createHash("sha256").update(fs.readFileSync(process.argv[1])).digest("hex"));
  ' "$1"
}

publish_new_state() {
  local source="$1"
  [ ! -e "$STATE_FILE" ] && [ ! -L "$STATE_FILE" ] || die "state file already exists"
  STATE_PART="$(mktemp "$STATE_PARENT/.witself-agent-email-smoke-state.XXXXXX")"
  chmod 600 "$STATE_PART"
  cp "$source" "$STATE_PART"
  chmod 600 "$STATE_PART"
  ln "$STATE_PART" "$STATE_FILE" 2>/dev/null || die "could not publish new state without overwrite"
  rm -f "$STATE_PART"
  STATE_PART=""
}

replace_state() {
  local source="$1"
  require_private_file "state file" "$STATE_FILE" 262144
  STATE_PART="$(mktemp "$STATE_PARENT/.witself-agent-email-smoke-state.XXXXXX")"
  chmod 600 "$STATE_PART"
  cp "$source" "$STATE_PART"
  chmod 600 "$STATE_PART"
  mv -f "$STATE_PART" "$STATE_FILE"
  STATE_PART=""
}

CELL=""
KUBECONFIG_FILE=""
KUBE_CONTEXT=""
PHASE=""
TARGET_FILE=""
STATE_FILE=""
RELAY_KEY_ID=""
RELAY_PRIVATE_KEY_FILE=""
AGENT_TOKEN_FILE=""
NAMESPACE="witself"
DEPLOYMENT="witself-server"
SERVICE="witself-server"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --cell) [ "$#" -ge 2 ] || die "incomplete arguments"; CELL="$2"; shift 2 ;;
    --kubeconfig) [ "$#" -ge 2 ] || die "incomplete arguments"; KUBECONFIG_FILE="$2"; shift 2 ;;
    --context) [ "$#" -ge 2 ] || die "incomplete arguments"; KUBE_CONTEXT="$2"; shift 2 ;;
    --phase) [ "$#" -ge 2 ] || die "incomplete arguments"; PHASE="$2"; shift 2 ;;
    --target-file) [ "$#" -ge 2 ] || die "incomplete arguments"; TARGET_FILE="$2"; shift 2 ;;
    --state-file) [ "$#" -ge 2 ] || die "incomplete arguments"; STATE_FILE="$2"; shift 2 ;;
    --agent-token-file) [ "$#" -ge 2 ] || die "incomplete arguments"; AGENT_TOKEN_FILE="$2"; shift 2 ;;
    --relay-key-id) [ "$#" -ge 2 ] || die "incomplete arguments"; RELAY_KEY_ID="$2"; shift 2 ;;
    --relay-private-key-file) [ "$#" -ge 2 ] || die "incomplete arguments"; RELAY_PRIVATE_KEY_FILE="$2"; shift 2 ;;
    --namespace) [ "$#" -ge 2 ] || die "incomplete arguments"; NAMESPACE="$2"; shift 2 ;;
    --deployment) [ "$#" -ge 2 ] || die "incomplete arguments"; DEPLOYMENT="$2"; shift 2 ;;
    --service) [ "$#" -ge 2 ] || die "incomplete arguments"; SERVICE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown or incomplete argument" ;;
  esac
done

[ -n "$CELL" ] && [ -n "$KUBECONFIG_FILE" ] && [ -n "$KUBE_CONTEXT" ] &&
  [ -n "$PHASE" ] && [ -n "$TARGET_FILE" ] && [ -n "$STATE_FILE" ] || {
    usage >&2
    die "required arguments are missing"
  }
case "$PHASE" in
  disabled|entitled)
    [ -n "$AGENT_TOKEN_FILE" ] && [ -n "$RELAY_KEY_ID" ] && [ -n "$RELAY_PRIVATE_KEY_FILE" ] ||
      die "agent token and relay key inputs are required for a signed phase"
    ;;
  cleanup)
    [ -z "$AGENT_TOKEN_FILE" ] && [ -z "$RELAY_KEY_ID" ] && [ -z "$RELAY_PRIVATE_KEY_FILE" ] ||
      die "agent token and relay key inputs are forbidden for cleanup"
    ;;
  *) die "phase must be disabled, entitled, or cleanup" ;;
esac

for value in "$CELL" "$NAMESPACE" "$DEPLOYMENT" "$SERVICE"; do
  [[ "$value" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "a Kubernetes name is invalid"
done
[[ "$KUBE_CONTEXT" =~ ^[A-Za-z0-9._:@/-]+$ ]] || die "context contains unsupported characters"
if [ "$PHASE" != cleanup ]; then
  [[ "$RELAY_KEY_ID" =~ ^[a-z][a-z0-9_-]{0,63}$ ]] || die "relay key id is invalid"
fi

for command_name in cmp curl git jq kubectl node sed stat; do require_command "$command_name"; done

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
RELAY_HELPER="$REPO_ROOT/infra/cloudflare/agent-email/scripts/cell-receive-smoke-relay.mjs"
[ -f "$RELAY_HELPER" ] || die "relay helper is unavailable"

TARGET_FILE="$(canonical_file_path "$TARGET_FILE")" || die "target path must be canonical and absolute"
STATE_FILE="$(canonical_file_path "$STATE_FILE")" || die "state path must be canonical and absolute"
KUBECONFIG_FILE="$(canonical_file_path "$KUBECONFIG_FILE")" || die "kubeconfig path must be canonical and absolute"
require_private_file "target file" "$TARGET_FILE" 32768
require_private_parent "$TARGET_FILE"
require_private_parent "$STATE_FILE"
require_private_file "kubeconfig" "$KUBECONFIG_FILE" 1048576
require_private_parent "$KUBECONFIG_FILE"
if [ "$PHASE" = disabled ]; then
  [ ! -e "$STATE_FILE" ] && [ ! -L "$STATE_FILE" ] || die "disabled phase requires an absent state file"
else
  require_private_file "state file" "$STATE_FILE" 262144
fi
if [ "$PHASE" != cleanup ]; then
  AGENT_TOKEN_FILE="$(canonical_file_path "$AGENT_TOKEN_FILE")" ||
    die "agent token path must be canonical and absolute"
  require_private_file "agent token" "$AGENT_TOKEN_FILE" 256
  require_private_parent "$AGENT_TOKEN_FILE"
  RELAY_PRIVATE_KEY_FILE="$(canonical_file_path "$RELAY_PRIVATE_KEY_FILE")" ||
    die "relay private key path must be canonical and absolute"
  require_private_file "relay private key" "$RELAY_PRIVATE_KEY_FILE" 4096
  require_private_parent "$RELAY_PRIVATE_KEY_FILE"
fi

umask 077
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/witself-agent-email-cell-smoke.XXXXXX")"
chmod 700 "$WORK_DIR"
STATE_PARENT="$(dirname "$STATE_FILE")"
STATE_PART=""
PORT_FORWARD_PID=""
LOCK_CREATED=false
LOCK_NAME="witself-agent-email-operation-lock"
LOCK_UID=""
PORT_FORWARD_STOPPED=true

KUBECONFIG_SNAPSHOT="$WORK_DIR/kubeconfig"
TARGET_SNAPSHOT="$WORK_DIR/target.json"
STATE_SNAPSHOT="$WORK_DIR/state.json"
snapshot_file "$KUBECONFIG_FILE" "$KUBECONFIG_SNAPSHOT"
snapshot_file "$TARGET_FILE" "$TARGET_SNAPSHOT"
if [ "$PHASE" != disabled ]; then snapshot_file "$STATE_FILE" "$STATE_SNAPSHOT"; fi

KUBE=(kubectl --request-timeout=30s --kubeconfig "$KUBECONFIG_SNAPSHOT" --context "$KUBE_CONTEXT")

stop_port_forward() {
  if [ -z "$PORT_FORWARD_PID" ]; then
    PORT_FORWARD_STOPPED=true
    return 0
  fi
  kill "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
  wait "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
  if kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
    PORT_FORWARD_STOPPED=false
    return 1
  fi
  PORT_FORWARD_STOPPED=true
  PORT_FORWARD_PID=""
  return 0
}

release_operation_lock() {
  local current_uid
  [ "$LOCK_CREATED" = true ] || return 0
  [ "$PORT_FORWARD_STOPPED" = true ] || return 1
  current_uid="$("${KUBE[@]}" -n "$NAMESPACE" get configmap "$LOCK_NAME" \
    -o 'jsonpath={.metadata.uid}' 2>/dev/null || true)"
  [ -n "$current_uid" ] && [ "$current_uid" = "$LOCK_UID" ] || return 1
  "${KUBE[@]}" -n "$NAMESPACE" delete configmap "$LOCK_NAME" \
    --wait=true --timeout=30s >/dev/null 2>&1 || return 1
  LOCK_CREATED=false
  return 0
}

cleanup_local() {
  local status=$?
  trap - EXIT INT TERM HUP
  stop_port_forward || true
  if ! release_operation_lock; then
    printf '%s\n' 'warning: cell operation lock retained because cleanup could not prove ownership or transport shutdown' >&2
  fi
  if [ -n "$STATE_PART" ]; then rm -f "$STATE_PART"; fi
  find "$WORK_DIR" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$WORK_DIR" 2>/dev/null || true
  exit "$status"
}
trap cleanup_local EXIT INT TERM HUP

if ! jq -e '
  type == "object" and
  (keys | sort) == ["account_id","agent_id","disabled_plan","entitled_plan","realm_id","recipient","schema_version"] and
  .schema_version == 1 and
  (.account_id | type == "string" and test("^acc_[a-z2-7]{16}$")) and
  (.realm_id | type == "string" and test("^realm_[a-z2-7]{16}$")) and
  (.agent_id | type == "string" and test("^agent_[a-z2-7]{16}$")) and
  (.recipient | type == "string" and
    test("^[a-z0-9]([a-z0-9-]{0,45}[a-z0-9])?\\.[a-z2-7]{16}@witmail\\.net$") and
    (split("@")[0] | length <= 64)) and
  (.recipient | split("@")[0] | split(".")[1]) == (.realm_id | ltrimstr("realm_")) and
  .disabled_plan == "free" and .entitled_plan == "standard"
' "$TARGET_SNAPSHOT" >/dev/null; then
  die "target manifest is invalid"
fi
ACCOUNT_ID="$(jq -er '.account_id' "$TARGET_SNAPSHOT")"
REALM_ID="$(jq -er '.realm_id' "$TARGET_SNAPSHOT")"
AGENT_ID="$(jq -er '.agent_id' "$TARGET_SNAPSHOT")"
BASE_RECIPIENT="$(jq -er '.recipient' "$TARGET_SNAPSHOT")"
TARGET_SHA256="$(sha256_file "$TARGET_SNAPSHOT")"
[[ "$TARGET_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "target manifest digest failed"

if [ "$PHASE" != disabled ]; then
  if ! jq -e --arg cell "$CELL" --arg target_sha "$TARGET_SHA256" '
    . as $state | type == "object" and .schema_version == 1 and
    ((keys - ["cell","cleanup","client_fence","cohort_fence","config_fence","deployment_fence","disabled",
      "entitled","schema_version","target","target_sha256"]) | length == 0) and
    .cell == $cell and .target_sha256 == $target_sha and
    (.target | type == "object") and
    (.deployment_fence | type == "object" and
      (keys|sort)==["config_checksum","config_name","generation","image","resource_version","uid"]) and
    (.config_fence | type == "object" and
      (keys|sort)==["audience","cell","checksum","domain","public_keys","resource_version","uid"]) and
    (.cohort_fence | type == "object" and
      (keys|sort)==["immutable","resource_version","uid"]) and
    (.client_fence | type == "object" and
      (keys|sort)==["token_sha256"] and
      (.token_sha256|type=="string" and test("^[0-9a-f]{64}$"))) and
    (.disabled | type == "object" and
      ((keys-["evidence","outcome","owner_gate","plan","probe","verdict"])|length==0)) and
    (.disabled.plan | type == "object" and
      (keys|sort)==["applied_epoch","entitlement_version","feature_enabled","hash","plan","revision"]) and
    (.disabled.probe | type == "object" and
      (keys|sort)==["mime_message_id","nonce","raw_sha256","raw_size","recipient","tag"]) and
    .disabled.probe.tag==("ws-"+.disabled.probe.nonce) and
    .disabled.probe.mime_message_id==("<witself-receive-smoke-"+.disabled.probe.nonce+"@smoke.invalid>") and
    .disabled.probe.recipient==((.target.recipient|split("@")[0])+"+"+.disabled.probe.tag+"@witmail.net") and
    .disabled.plan.plan=="free" and .disabled.plan.feature_enabled==false and
    .disabled.plan.entitlement_version==1 and
    (.disabled.plan.revision|type=="number" and .>=1) and
    (.disabled.plan.applied_epoch|type=="number" and .>0) and
    (.disabled.plan.hash|type=="string" and test("^[0-9a-f]{64}$")) and
    (if .disabled.outcome=="verified" then
       .disabled.verdict=="feature_disabled" and .disabled.owner_gate=="feature_disabled" and
       .disabled.evidence=={"messages":0,"deliveries":0,"events":0}
     else .disabled.outcome=="prepared" and
       ((.disabled.verdict//null)==null) and ((.disabled.owner_gate//null)==null) and
       ((.disabled.evidence//null)==null) end) and
    ((.entitled // null)==null or
      (.entitled | type == "object" and
       ((keys-["evidence","outcome","owner_gate","plan","probe","verdict"])|length==0) and
       (.plan|type=="object" and
        (keys|sort)==["applied_epoch","entitlement_version","feature_enabled","hash","plan","revision"]) and
       (.probe|type=="object" and
        (keys|sort)==["mime_message_id","nonce","raw_sha256","raw_size","recipient","tag"]) and
       .probe.tag==("ws-"+.probe.nonce) and
       .probe.mime_message_id==("<witself-receive-smoke-"+.probe.nonce+"@smoke.invalid>") and
       .probe.recipient==(($state.target.recipient|split("@")[0])+"+"+.probe.tag+"@witmail.net") and
       .plan.plan=="standard" and .plan.feature_enabled==true and .plan.entitlement_version==1 and
       (.plan.revision|type=="number" and .>$state.disabled.plan.revision) and
       (.plan.applied_epoch|type=="number" and .>$state.disabled.plan.applied_epoch) and
       (.plan.hash|type=="string" and test("^[0-9a-f]{64}$") and .!=$state.disabled.plan.hash) and
       (if .outcome=="verified" then
          .verdict=="accepted" and .owner_gate=="address_available" and
          .evidence=={"messages":1,"deliveries":1,"events":1}
        else .outcome=="prepared" and ((.verdict//null)==null) and
          ((.owner_gate//null)==null) and ((.evidence//null)==null) end))) and
    ((.cleanup // null)==null or
      (.cleanup|type=="object" and (keys|sort)==["deleted","events_retained","matched","outcome"] and
       .outcome=="complete" and (.matched|type=="number" and .>=0 and .<=2) and
       .deleted==.matched and .events_retained==.matched)) and
    (.disabled.outcome == "verified" or .disabled.outcome == "prepared")
  ' "$STATE_SNAPSHOT" >/dev/null; then
    die "state file does not match this target and cell"
  fi
  if ! jq -e --slurpfile target "$TARGET_SNAPSHOT" '.target == $target[0]' "$STATE_SNAPSHOT" >/dev/null; then
    die "state target does not match the private manifest"
  fi
  if [ "$PHASE" = entitled ] &&
     ! jq -e '.disabled.outcome == "verified" and (.entitled // null) == null' "$STATE_SNAPSHOT" >/dev/null; then
    die "entitled phase requires one completed disabled proof and no prior entitled attempt"
  fi
  if [ "$PHASE" = cleanup ] && jq -e '(.cleanup // null)!=null' "$STATE_SNAPSHOT" >/dev/null; then
    die "synthetic cleanup is already complete"
  fi
fi

OBSERVED_CELL="$("${KUBE[@]}" -n argocd get applications.argoproj.io witself-postgresql \
  -o 'jsonpath={.metadata.labels.witself\.io/cell}' 2>/dev/null || true)"
[ "$OBSERVED_CELL" = "$CELL" ] || die "context does not identify the requested cell"

LOCK_JSON="$WORK_DIR/lock.json"
if ! jq -n --arg name "$LOCK_NAME" --arg phase "$PHASE" '{
  apiVersion:"v1", kind:"ConfigMap",
  metadata:{name:$name,labels:{
    "app.kubernetes.io/name":"witself-agent-email-operation",
    "app.kubernetes.io/component":"one-shot",
    "witself.io/agent-email-operation":"receive-smoke",
    "witself.io/agent-email-smoke-phase":$phase
  }}, immutable:true, data:{operation:"receive-smoke"}
}' | "${KUBE[@]}" -n "$NAMESPACE" create -f - -o json >"$LOCK_JSON" 2>"$WORK_DIR/lock.err"; then
  die "another agent-email cell operation is active or requires cleanup"
fi
LOCK_UID="$(jq -er '.metadata.uid | select(type=="string" and length>0)' "$LOCK_JSON" 2>/dev/null)" ||
  die "cell operation lock has no ownership fence"
LOCK_CREATED=true

validate_deployment() {
  local file="$1"
  jq -e --arg name "$DEPLOYMENT" '
    .metadata.name == $name and
    (.metadata.uid | type=="string" and length>0) and
    (.metadata.resourceVersion | type=="string" and length>0) and
    (.metadata.generation | type=="number") and
    .status.observedGeneration == .metadata.generation and
    (.spec.replicas | type=="number" and .>=1) and
    .status.readyReplicas == .spec.replicas and
    .status.updatedReplicas == .spec.replicas and
    .status.availableReplicas == .spec.replicas and
    ((.status.unavailableReplicas // 0)==0) and
    ([.spec.template.spec.containers[] | select(.name=="witself-server")] | length)==1 and
    (.spec.template.metadata.annotations["witself.io/server-config-checksum"] |
      type=="string" and test("^[0-9a-f]{64}$"))
  ' "$file" >/dev/null
}

snapshot_cell_source() {
  local suffix="$1"
  local deployment_file="$WORK_DIR/deployment-$suffix.json"
  local config_file="$WORK_DIR/config-$suffix.json"
  local service_file="$WORK_DIR/service-$suffix.json"
  local config_name checksum image version
  "${KUBE[@]}" -n "$NAMESPACE" get deployment "$DEPLOYMENT" -o json >"$deployment_file" 2>/dev/null ||
    die "managed server Deployment is unavailable"
  validate_deployment "$deployment_file" || die "managed server Deployment is not fully converged"
  image="$(jq -er '.spec.template.spec.containers[] | select(.name=="witself-server") | .image' "$deployment_file")" ||
    die "managed server image is unavailable"
  case "$image" in ghcr.io/witwave-ai/images/witself-server:[0-9]*.[0-9]*.[0-9]*) ;; *) die "managed server image is not an exact release" ;; esac
  version="${image##*:}"
  if [[ ! "$version" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    die "managed server release is invalid"
  fi
  if (( 10#${BASH_REMATCH[1]} == 0 && 10#${BASH_REMATCH[2]} == 0 && 10#${BASH_REMATCH[3]} < 245 )); then
    die "managed server release is too old for this smoke"
  fi
  config_name="$(jq -er '[.spec.template.spec.containers[] | select(.name=="witself-server") |
    .envFrom[]? | select(.configMapRef.name!=null) | .configMapRef.name] |
    if length==1 then .[0] else error("ambiguous") end' "$deployment_file")" ||
    die "managed server ConfigMap reference is ambiguous"
  checksum="$(jq -er '.spec.template.metadata.annotations["witself.io/server-config-checksum"]' "$deployment_file")"
  "${KUBE[@]}" -n "$NAMESPACE" get configmap "$config_name" -o json >"$config_file" 2>/dev/null ||
    die "managed server ConfigMap is unavailable"
  jq -e --arg name "$config_name" --arg cell "$CELL" --arg checksum "$checksum" '
    .metadata.name==$name and
    (.metadata.uid | type=="string" and length>0) and
    (.metadata.resourceVersion | type=="string" and length>0) and
    .metadata.annotations["witself.io/server-config-checksum"]==$checksum and
    .data.WITSELF_BACKEND_KIND=="managed" and
    .data.WITSELF_CELL_NAME==$cell and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED=="true" and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED=="false" and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN=="witmail.net" and
    ((.data.WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS // "")=="") and
    (.data.WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE==$cell) and
    (.data.WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON | fromjson | type=="object")
  ' "$config_file" >/dev/null || die "managed production receive configuration is not ready"
  "${KUBE[@]}" -n "$NAMESPACE" get service "$SERVICE" -o json >"$service_file" 2>/dev/null ||
    die "managed server Service is unavailable"
  jq -e --arg name "$SERVICE" '
    .metadata.name==$name and .spec.clusterIP!="None" and
    ([.spec.ports[] | select(.name=="api" and .port==80 and .targetPort=="api" and .protocol=="TCP")] | length)==1
  ' "$service_file" >/dev/null || die "managed server API Service is invalid"
  jq -S '{uid:.metadata.uid,resource_version:.metadata.resourceVersion,generation:.metadata.generation,
    image:(.spec.template.spec.containers[]|select(.name=="witself-server")|.image),
    config_checksum:.spec.template.metadata.annotations["witself.io/server-config-checksum"],
    config_name:([.spec.template.spec.containers[]|select(.name=="witself-server")|.envFrom[]?|
      select(.configMapRef.name!=null)|.configMapRef.name][0])}' "$deployment_file" >"$WORK_DIR/deployment-$suffix.fence.json"
  jq -S '{uid:.metadata.uid,resource_version:.metadata.resourceVersion,
    checksum:.metadata.annotations["witself.io/server-config-checksum"],
    cell:.data.WITSELF_CELL_NAME,audience:.data.WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE,
    domain:.data.WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN,
    public_keys:.data.WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON}' "$config_file" >"$WORK_DIR/config-$suffix.fence.json"
}

snapshot_cell_source initial
SERVER_IMAGE="$(jq -er '.image' "$WORK_DIR/deployment-initial.fence.json")"
SERVER_VERSION="${SERVER_IMAGE##*:}"

if [ "$PHASE" = entitled ]; then
  jq -e --slurpfile state "$STATE_SNAPSHOT" '. == $state[0].deployment_fence' \
    "$WORK_DIR/deployment-initial.fence.json" >/dev/null || die "server Deployment changed between plan phases"
  jq -e --slurpfile state "$STATE_SNAPSHOT" '. == $state[0].config_fence' \
    "$WORK_DIR/config-initial.fence.json" >/dev/null || die "server configuration changed between plan phases"
fi

if [ "$PHASE" != cleanup ]; then
  DEPLOYMENT_JSON="$WORK_DIR/deployment-initial.json"
  CONFIG_JSON="$WORK_DIR/config-initial.json"
  COHORT_REF_JSON="$WORK_DIR/cohort-ref.json"
  if ! jq -e '[.spec.template.spec.containers[] | select(.name=="witself-server") | .env[]? |
      select(.name=="WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS") |
      {name:.valueFrom.secretKeyRef.name,key:.valueFrom.secretKeyRef.key,
       optional:(.valueFrom.secretKeyRef.optional // false),literal:(.value // null)}] |
      if length==1 and .[0].optional==false and .[0].literal==null and
         (. [0].name|type=="string" and length>0) and (. [0].key|type=="string" and length>0)
      then .[0] else error("invalid") end' "$DEPLOYMENT_JSON" >"$COHORT_REF_JSON"; then
    die "managed receive cohort Secret reference is invalid"
  fi
  COHORT_SECRET_NAME="$(jq -er '.name' "$COHORT_REF_JSON")"
  COHORT_SECRET_KEY="$(jq -er '.key' "$COHORT_REF_JSON")"
  COHORT_SECRET_JSON="$WORK_DIR/cohort-secret.json"
  "${KUBE[@]}" -n "$NAMESPACE" get secret "$COHORT_SECRET_NAME" -o json >"$COHORT_SECRET_JSON" 2>/dev/null ||
    die "managed receive cohort Secret is unavailable"
  jq -e --arg key "$COHORT_SECRET_KEY" '
    .immutable==true and (.metadata.uid|type=="string" and length>0) and
    (.metadata.resourceVersion|type=="string" and length>0) and
    (.data[$key]|type=="string" and length>0)
  ' "$COHORT_SECRET_JSON" >/dev/null || die "managed receive cohort Secret is not immutable and complete"
  COHORT_FILE="$WORK_DIR/cohort"
  jq -ejr --arg key "$COHORT_SECRET_KEY" '.data[$key] | @base64d' "$COHORT_SECRET_JSON" >"$COHORT_FILE" 2>/dev/null ||
    die "managed receive cohort is invalid"
  COHORT_VALUE="$(<"$COHORT_FILE")"
  [ "$(wc -c <"$COHORT_FILE" | tr -d '[:space:]')" = "${#COHORT_VALUE}" ] ||
    die "managed receive cohort is not canonical"
  jq -en --arg csv "$COHORT_VALUE" --arg account "$ACCOUNT_ID" '
    ($csv|split(",")) as $ids |
    ($ids|length)>=1 and ($ids|length)<=100 and
    ($ids|unique|length)==($ids|length) and ($ids|sort)==$ids and
    all($ids[]; test("^acc_[a-z2-7]{16}$")) and ($ids|index($account))!=null
  ' >/dev/null || die "target account is not in the exact managed receive cohort"
  jq -S '{uid:.metadata.uid,resource_version:.metadata.resourceVersion,immutable:.immutable}' \
    "$COHORT_SECRET_JSON" >"$WORK_DIR/cohort-secret.fence.json"
  if [ "$PHASE" = entitled ]; then
    jq -e --slurpfile state "$STATE_SNAPSHOT" '. == $state[0].cohort_fence' \
      "$WORK_DIR/cohort-secret.fence.json" >/dev/null ||
      die "receive cohort Secret changed between plan phases"
  fi
  PUBLIC_KEYS_FILE="$WORK_DIR/public-keys.json"
  jq -er '.data.WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON | fromjson' "$CONFIG_JSON" >"$PUBLIC_KEYS_FILE" ||
    die "relay public key set is invalid"
  chmod 600 "$PUBLIC_KEYS_FILE"
fi

snapshot_cohort_fence() {
  local suffix="$1"
  local secret_file="$WORK_DIR/cohort-secret-$suffix.json"
  [ "$PHASE" != cleanup ] || return 0
  "${KUBE[@]}" -n "$NAMESPACE" get secret "$COHORT_SECRET_NAME" -o json >"$secret_file" 2>/dev/null ||
    die "managed receive cohort Secret changed or became unavailable"
  jq -e --arg key "$COHORT_SECRET_KEY" '
    .immutable==true and (.metadata.uid|type=="string" and length>0) and
    (.metadata.resourceVersion|type=="string" and length>0) and
    (.data[$key]|type=="string" and length>0)
  ' "$secret_file" >/dev/null || die "managed receive cohort Secret changed or became incomplete"
  jq -S '{uid:.metadata.uid,resource_version:.metadata.resourceVersion,immutable:.immutable}' \
    "$secret_file" >"$WORK_DIR/cohort-secret-$suffix.fence.json"
  cmp -s "$WORK_DIR/cohort-secret.fence.json" "$WORK_DIR/cohort-secret-$suffix.fence.json" ||
    die "managed receive cohort Secret drifted during the smoke"
}

PODS_JSON="$WORK_DIR/postgres-pods.json"
"${KUBE[@]}" -n "$NAMESPACE" get pods \
  -l 'app.kubernetes.io/instance=witself-postgresql,app.kubernetes.io/component=primary' \
  -o json >"$PODS_JSON" 2>/dev/null || die "PostgreSQL primary discovery failed"
POSTGRES_POD="$(jq -er '[.items[] | select(.status.phase=="Running") |
  select(any(.status.conditions[]?;.type=="Ready" and .status=="True"))] |
  if length==1 then .[0].metadata.name else error("ambiguous") end' "$PODS_JSON")" ||
  die "expected exactly one Ready PostgreSQL primary"
POSTGRES_CONTAINER="$(jq -er --arg pod "$POSTGRES_POD" '.items[]|select(.metadata.name==$pod)|
  [.spec.containers[].name] | if index("postgresql") then "postgresql"
  elif length==1 then .[0] else error("ambiguous") end' "$PODS_JSON")" ||
  die "PostgreSQL container selection failed"

run_sql() {
  local mode="$1" sql_file="$2" output_file="$3"
  local options='-c statement_timeout=10000 -c lock_timeout=2000'
  if [ "$mode" = read ]; then options="-c default_transaction_read_only=on $options"; fi
  # This static shell is evaluated inside the database pod. Tenant values and
  # SQL travel only over stdin, never through kubectl/process arguments.
  # This program is intentionally expanded by the pod shell, not this host.
  # shellcheck disable=SC2016
  if ! "${KUBE[@]}" -n "$NAMESPACE" exec -i "$POSTGRES_POD" -c "$POSTGRES_CONTAINER" -- \
    env PGOPTIONS="$options" sh -eu -c '
      password_file="${POSTGRES_PASSWORD_FILE:-${POSTGRESQL_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/password}}"
      test -r "$password_file"
      PGPASSWORD="$(cat "$password_file")"
      export PGPASSWORD
      db_user="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-witself}}"
      db_name="${POSTGRES_DATABASE:-${POSTGRESQL_DATABASE:-witself}}"
      exec psql --quiet --no-password --host=127.0.0.1 \
        --port="${POSTGRESQL_PORT_NUMBER:-5432}" --username="$db_user" --dbname="$db_name" \
        --tuples-only --no-align --set=ON_ERROR_STOP=1 --file=-
    ' <"$sql_file" >"$output_file" 2>"$WORK_DIR/sql.err"; then
    die "database fence failed"
  fi
  chmod 600 "$output_file"
}

TARGET_SQL="$WORK_DIR/target.sql"
{
  printf '%s\n' 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;'
  printf "WITH target AS (\n"
  printf " SELECT a.status,a.plan,a.plan_policies,a.plan_features,a.plan_applied_at,\n"
  printf "        a.plan_snapshot_revision,a.plan_snapshot_hash,mb.receive_state\n"
  printf " FROM accounts a JOIN realms r ON r.account_id=a.id\n"
  printf " JOIN agents ag ON ag.realm_id=r.id\n"
  printf " JOIN agent_email_mailboxes mb ON mb.account_id=a.id AND mb.realm_id=r.id AND mb.owner_agent_id=ag.id\n"
  printf " JOIN agent_email_addresses ad ON ad.id=mb.address_id AND ad.account_id=a.id AND ad.realm_id=r.id AND ad.provisioned_agent_id=ag.id\n"
  printf " JOIN agent_email_address_domains route ON route.address_id=ad.id AND route.account_id=a.id AND route.realm_id=r.id AND route.provisioned_agent_id=ag.id\n"
  printf " WHERE a.id='%s' AND r.id='%s' AND ag.id='%s'\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
  printf "   AND r.deleted_at IS NULL AND ag.deleted_at IS NULL AND ad.retired_at IS NULL\n"
  printf "   AND route.domain='witmail.net' AND route.local_part||'@'||route.domain='%s'\n" "$BASE_RECIPIENT"
  printf ") SELECT json_build_object(\n"
  printf " 'target_count',(SELECT count(*) FROM target),\n"
  printf " 'account_status',COALESCE((SELECT status FROM target),''),\n"
  printf " 'plan',COALESCE((SELECT plan FROM target),''),\n"
  printf " 'entitlement_version',COALESCE((SELECT CASE WHEN plan_policies ? 'agent_email_entitlement_version' THEN (plan_policies->>'agent_email_entitlement_version')::bigint END FROM target),-1),\n"
  printf " 'feature_enabled',COALESCE((SELECT plan_features ? 'agent_email_receive' FROM target),false),\n"
  printf " 'receive_state',COALESCE((SELECT receive_state FROM target),''),\n"
  printf " 'plan_applied',COALESCE((SELECT plan_applied_at IS NOT NULL FROM target),false),\n"
  printf " 'plan_applied_epoch',COALESCE((SELECT extract(epoch from plan_applied_at)::bigint FROM target),0),\n"
  printf " 'plan_revision',COALESCE((SELECT plan_snapshot_revision FROM target),-1),\n"
  printf " 'plan_hash',COALESCE((SELECT plan_snapshot_hash FROM target),''),\n"
  printf " 'database_epoch',extract(epoch from clock_timestamp())::bigint\n"
  printf ");\nCOMMIT;\n"
} >"$TARGET_SQL"
chmod 600 "$TARGET_SQL"
TARGET_OBSERVATION="$WORK_DIR/target-observation.json"
run_sql read "$TARGET_SQL" "$TARGET_OBSERVATION"
jq -e 'type=="object" and .target_count==1 and .account_status=="active" and
  .receive_state=="enabled" and .plan_applied==true and
  (.plan_revision|type=="number" and .>=1) and
  (.plan_hash|type=="string" and test("^[0-9a-f]{64}$")) and
  .entitlement_version==1' "$TARGET_OBSERVATION" >/dev/null || die "dedicated target or effective plan snapshot is not ready"

LOCAL_EPOCH="$(date +%s)"
DATABASE_EPOCH="$(jq -er '.database_epoch' "$TARGET_OBSERVATION")"
CLOCK_DELTA=$(( LOCAL_EPOCH - DATABASE_EPOCH ))
(( CLOCK_DELTA >= -60 && CLOCK_DELTA <= 60 )) || die "operator and cell clocks are too far apart"

if [ "$PHASE" = disabled ]; then
  jq -e '.plan=="free" and .feature_enabled==false' "$TARGET_OBSERVATION" >/dev/null ||
    die "disabled phase requires authoritative Personal policy"
elif [ "$PHASE" = entitled ]; then
  jq -e '.plan=="standard" and .feature_enabled==true' "$TARGET_OBSERVATION" >/dev/null ||
    die "entitled phase requires authoritative Professional policy"
  DISABLED_REVISION="$(jq -er '.disabled.plan.revision' "$STATE_SNAPSHOT")"
  DISABLED_HASH="$(jq -er '.disabled.plan.hash' "$STATE_SNAPSHOT")"
  DISABLED_APPLIED_EPOCH="$(jq -er '.disabled.plan.applied_epoch' "$STATE_SNAPSHOT")"
  jq -e --argjson revision "$DISABLED_REVISION" --arg hash "$DISABLED_HASH" \
    --argjson applied "$DISABLED_APPLIED_EPOCH" '
      .plan_revision>$revision and .plan_hash!=$hash and .plan_applied_epoch>$applied
    ' "$TARGET_OBSERVATION" >/dev/null || die "Professional snapshot does not prove a newer plan transition"
fi

if [ "$PHASE" = cleanup ]; then
  PROBE_JSON="$WORK_DIR/cleanup-probe.json"
  if ! jq -e '
    [(.disabled.probe // null),(.entitled.probe // null)] | map(select(.!=null)) |
    length>=1 and length<=2 and all(.[];
      (.nonce|type=="string" and test("^[0-9a-f]{16}$")) and
      (.tag|type=="string" and test("^ws-[0-9a-f]{16}$")) and
      (.raw_sha256|type=="string" and test("^[0-9a-f]{64}$")) and
      (.raw_size|type=="number" and .>=1 and .<=4096) and
      (.mime_message_id|type=="string" and test("^<witself-receive-smoke-[0-9a-f]{16}@smoke\\.invalid>$")) and
      (.recipient|type=="string" and test("^[a-z0-9.-]+\\+ws-[0-9a-f]{16}@witmail\\.net$")))
  ' "$STATE_SNAPSHOT" >/dev/null; then
    die "state contains no valid recoverable smoke probe"
  fi
  jq -c '[.disabled.probe // empty,.entitled.probe // empty]' "$STATE_SNAPSHOT" >"$PROBE_JSON"
else
  NONCE="$(node -e 'process.stdout.write(require("node:crypto").randomBytes(8).toString("hex"))')"
  [[ "$NONCE" =~ ^[0-9a-f]{16}$ ]] || die "probe nonce generation failed"
  TAG="ws-$NONCE"
  BASE_LOCAL="${BASE_RECIPIENT%@*}"
  DOMAIN="${BASE_RECIPIENT#*@}"
  (( ${#BASE_LOCAL} + 1 + ${#TAG} <= 64 )) || die "canonical address has no room for a smoke subaddress"
  TAGGED_RECIPIENT="$BASE_LOCAL+$TAG@$DOMAIN"
  MIME_MESSAGE_ID="<witself-receive-smoke-$NONCE@smoke.invalid>"
  RAW_FILE="$WORK_DIR/probe.eml"
  {
    printf 'From: Witself receive smoke <witself-email-receive-smoke@smoke.invalid>\r\n'
    printf 'To: %s\r\n' "$TAGGED_RECIPIENT"
    printf 'Subject: Witself production receive smoke\r\n'
    printf 'Message-ID: %s\r\n' "$MIME_MESSAGE_ID"
    printf 'X-Witself-Production-Receive-Smoke: 1\r\n'
    printf 'Content-Type: text/plain; charset=utf-8\r\n'
    printf '\r\nSynthetic receive path proof. No reply is expected.\r\n'
  } >"$RAW_FILE"
  chmod 600 "$RAW_FILE"
  RAW_SIZE="$(wc -c <"$RAW_FILE" | tr -d '[:space:]')"
  RAW_SHA256="$(sha256_file "$RAW_FILE")"
  [[ "$RAW_SIZE" =~ ^[0-9]+$ ]] && (( RAW_SIZE >= 1 && RAW_SIZE <= 4096 )) &&
    [[ "$RAW_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "probe encoding failed"
  PROBE_JSON="$WORK_DIR/probe.json"
  jq -n --arg nonce "$NONCE" --arg tag "$TAG" --arg recipient "$TAGGED_RECIPIENT" \
    --arg mime_message_id "$MIME_MESSAGE_ID" --arg raw_sha "$RAW_SHA256" --argjson raw_size "$RAW_SIZE" \
    '{nonce:$nonce,tag:$tag,recipient:$recipient,mime_message_id:$mime_message_id,
      raw_sha256:$raw_sha,raw_size:$raw_size}' >"$PROBE_JSON"
  chmod 600 "$PROBE_JSON"
fi

write_evidence_sql() {
  local probe_file="$1" sql_file="$2"
  local tag recipient mime_id raw_sha raw_size
  tag="$(jq -er '.tag' "$probe_file")"
  recipient="$(jq -er '.recipient' "$probe_file")"
  mime_id="$(jq -er '.mime_message_id' "$probe_file")"
  raw_sha="$(jq -er '.raw_sha256' "$probe_file")"
  raw_size="$(jq -er '.raw_size' "$probe_file")"
  {
    printf '%s\n' 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;'
    printf "WITH messages AS (SELECT id FROM agent_email_messages WHERE account_id='%s' AND realm_id='%s' AND owner_agent_id='%s'\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
    printf " AND provider='cloudflare_email_routing' AND envelope_sender='witself-email-receive-smoke@smoke.invalid'\n"
    printf " AND envelope_recipient='%s' AND subaddress_tag='%s' AND raw_sha256='%s' AND raw_size_bytes=%s\n" "$recipient" "$tag" "$raw_sha" "$raw_size"
    printf " AND header_subject='Witself production receive smoke' AND mime_message_id='%s')\n" "$mime_id"
    printf "SELECT json_build_object('messages',(SELECT count(*) FROM messages),\n"
    printf " 'deliveries',(SELECT count(*) FROM agent_email_deliveries d JOIN messages m ON m.id=d.message_id),\n"
    printf " 'events',(SELECT count(*) FROM account_events e JOIN messages m ON e.account_id='%s' AND e.verb='agent_email.received' AND e.metadata->>'message_id'=m.id),\n" "$ACCOUNT_ID"
    printf " 'owner_events',(SELECT count(*) FROM account_events e WHERE e.account_id='%s' AND e.verb='agent_email.received' AND e.metadata->>'owner_agent_id'='%s'));\n" "$ACCOUNT_ID" "$AGENT_ID"
    printf '%s\n' 'COMMIT;'
  } >"$sql_file"
  chmod 600 "$sql_file"
}

if [ "$PHASE" = cleanup ]; then
  # Cleanup accepts up to the two exact probes recorded by this state. It does
  # not touch account events or the shared rate limiter. Any changed, claimed,
  # read, duplicate-linked, canary-linked, or ambiguous row aborts the entire
  # transaction and leaves every message intact.
  CLEANUP_SQL="$WORK_DIR/cleanup.sql"
  {
    printf '%s\n' 'BEGIN;'
    printf '%s\n' 'CREATE TEMP TABLE smoke_expected(tag text PRIMARY KEY,recipient text,mime_id text,raw_sha text,raw_size bigint) ON COMMIT DROP;'
    jq -r '.[] | "INSERT INTO smoke_expected VALUES (\u0027"+.tag+"\u0027,\u0027"+.recipient+"\u0027,\u0027"+.mime_message_id+"\u0027,\u0027"+.raw_sha256+"\u0027,"+(.raw_size|tostring)+");"' "$PROBE_JSON"
    printf '%s\n' 'CREATE TEMP TABLE smoke_candidates(probe_tag text UNIQUE,id text PRIMARY KEY) ON COMMIT DROP;'
    printf "SELECT 1 FROM accounts WHERE id='%s' FOR NO KEY UPDATE;\n" "$ACCOUNT_ID"
    printf "INSERT INTO smoke_candidates SELECT x.tag,m.id FROM agent_email_messages m JOIN smoke_expected x ON\n"
    printf " m.subaddress_tag=x.tag AND m.envelope_recipient=x.recipient AND m.mime_message_id=x.mime_id AND m.raw_sha256=x.raw_sha AND m.raw_size_bytes=x.raw_size\n"
    printf " WHERE m.account_id='%s' AND m.realm_id='%s' AND m.owner_agent_id='%s' FOR UPDATE OF m;\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
    printf '%s\n' 'CREATE TEMP TABLE smoke_result(matched bigint,deleted bigint) ON COMMIT DROP;'
    printf '%s\n' "DO \$smoke\$ DECLARE matched_count bigint; unsafe_count bigint; deleted_count bigint; BEGIN"
    printf '%s\n' ' SELECT count(*) INTO matched_count FROM smoke_candidates;'
    printf '%s\n' " IF matched_count>2 THEN RAISE EXCEPTION 'unsafe smoke cleanup'; END IF;"
    printf " SELECT count(*) INTO unsafe_count FROM agent_email_messages m JOIN smoke_candidates c ON c.id=m.id\n"
    printf " WHERE m.account_id<>'%s' OR m.realm_id<>'%s' OR m.owner_agent_id<>'%s'\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
    printf " OR m.provider<>'cloudflare_email_routing' OR m.envelope_sender<>'witself-email-receive-smoke@smoke.invalid'\n"
    printf " OR m.header_subject IS DISTINCT FROM 'Witself production receive smoke' OR m.recipient_route_kind<>'canonical'\n"
    printf " OR m.parse_state<>'parsed' OR m.attachment_count<>0 OR m.attachment_storage_bytes<>0 OR m.retained_attachment_storage_bytes<>0\n"
    printf " OR m.payload_retention_state<>'retained' OR m.possible_duplicate_of_message_id IS NOT NULL OR m.raw_mime IS NULL\n"
    printf " OR position(E'\\r\\nX-Witself-Production-Receive-Smoke: 1\\r\\n' in convert_from(m.raw_mime,'UTF8'))=0;\n"
    printf '%s\n' " IF unsafe_count<>0 THEN RAISE EXCEPTION 'unsafe smoke cleanup'; END IF;"
    printf " SELECT count(*) INTO unsafe_count FROM smoke_candidates c WHERE\n"
    printf " (SELECT count(*) FROM agent_email_deliveries d WHERE d.message_id=c.id)<>1\n"
    printf " OR (SELECT count(*) FROM agent_email_deliveries d WHERE d.message_id=c.id AND d.account_id='%s' AND d.realm_id='%s' AND d.owner_agent_id='%s'\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
    printf "  AND d.folder='inbox' AND d.read_at IS NULL AND d.acked_at IS NULL AND d.code_consumed_at IS NULL\n"
    printf "  AND d.processing_state='available' AND d.processing_generation=0 AND d.failure_count=0 AND d.claim_id IS NULL AND d.lease_expires_at IS NULL AND d.completed_at IS NULL)<>1\n"
    printf " OR EXISTS(SELECT 1 FROM agent_email_messages child WHERE child.possible_duplicate_of_message_id=c.id)\n"
    printf " OR EXISTS(SELECT 1 FROM agent_email_retry_canary_arms arm WHERE arm.accepted_message_id=c.id)\n"
    printf " OR (SELECT count(*) FROM account_events e WHERE e.account_id='%s' AND e.verb='agent_email.received' AND e.metadata->>'message_id'=c.id)<>1;\n" "$ACCOUNT_ID"
    printf '%s\n' " IF unsafe_count<>0 THEN RAISE EXCEPTION 'unsafe smoke cleanup'; END IF;"
    printf '%s\n' ' DELETE FROM agent_email_messages m USING smoke_candidates c WHERE m.id=c.id; GET DIAGNOSTICS deleted_count=ROW_COUNT;'
    # $smoke$ is a PostgreSQL dollar-quote delimiter, not a shell variable.
    # shellcheck disable=SC2016
    printf '%s\n' ' INSERT INTO smoke_result VALUES(matched_count,deleted_count); END $smoke$;'
    printf "SELECT json_build_object('matched',matched,'deleted',deleted,\n"
    printf " 'remaining',(SELECT count(*) FROM agent_email_messages m JOIN smoke_expected x ON m.raw_sha256=x.raw_sha AND m.subaddress_tag=x.tag WHERE m.account_id='%s' AND m.realm_id='%s' AND m.owner_agent_id='%s'),\n" "$ACCOUNT_ID" "$REALM_ID" "$AGENT_ID"
    printf " 'events_retained',(SELECT count(*) FROM account_events e JOIN smoke_candidates c ON e.account_id='%s' AND e.verb='agent_email.received' AND e.metadata->>'message_id'=c.id)) FROM smoke_result;\n" "$ACCOUNT_ID"
    printf '%s\n' 'COMMIT;'
  } >"$CLEANUP_SQL"
  chmod 600 "$CLEANUP_SQL"
  CLEANUP_RESULT="$WORK_DIR/cleanup-result.json"
  run_sql write "$CLEANUP_SQL" "$CLEANUP_RESULT"
  jq -e 'type=="object" and (.matched|type=="number" and .>=0 and .<=2) and
    .deleted==.matched and .remaining==0 and .events_retained==.matched' "$CLEANUP_RESULT" >/dev/null ||
    die "synthetic cleanup did not reach a safe exact result"
  UPDATED_STATE="$WORK_DIR/state-updated.json"
  jq --argjson result "$(<"$CLEANUP_RESULT")" '.cleanup={outcome:"complete",matched:$result.matched,
    deleted:$result.deleted,events_retained:$result.events_retained}' "$STATE_SNAPSHOT" >"$UPDATED_STATE"
  replace_state "$UPDATED_STATE"
  jq -cn --argjson result "$(<"$CLEANUP_RESULT")" '{schema_version:1,phase:"cleanup",
    messages_matched:$result.matched,messages_deleted:$result.deleted,
    audit_events_retained:$result.events_retained,shared_rate_state_retained:true,
    provider_mutation_performed:false}'
  exit 0
fi

EVIDENCE_BEFORE_SQL="$WORK_DIR/evidence-before.sql"
EVIDENCE_BEFORE="$WORK_DIR/evidence-before.json"
write_evidence_sql "$PROBE_JSON" "$EVIDENCE_BEFORE_SQL"
run_sql read "$EVIDENCE_BEFORE_SQL" "$EVIDENCE_BEFORE"
jq -e 'type=="object" and (keys|sort)==["deliveries","events","messages","owner_events"] and
  .messages==0 and .deliveries==0 and .events==0 and
  (.owner_events|type=="number" and .>=0)' "$EVIDENCE_BEFORE" >/dev/null ||
  die "synthetic probe evidence already exists"

# Read the installed client's credential only after the cell, cohort, target,
# policy, and zero-evidence fences pass. Persist only its SHA-256 fence; the
# plaintext remains in this process-private work directory and is supplied to
# the loopback helper by file, never argv or environment.
AGENT_TOKEN_SNAPSHOT="$WORK_DIR/agent-token"
snapshot_file "$AGENT_TOKEN_FILE" "$AGENT_TOKEN_SNAPSHOT"
AGENT_TOKEN_SHA256="$(node -e '
  const fs=require("node:fs"),crypto=require("node:crypto");
  let value=fs.readFileSync(process.argv[1],"utf8");
  if (!/^witself_agt_[A-Za-z0-9_-]{43}\n?$/.test(value)) process.exit(1);
  if (value.endsWith("\n")) value=value.slice(0,-1);
  process.stdout.write(crypto.createHash("sha256").update(value).digest("hex"));
' "$AGENT_TOKEN_SNAPSHOT")" || die "agent token encoding is invalid"
[[ "$AGENT_TOKEN_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "agent token digest failed"
if [ "$PHASE" = entitled ]; then
  [ "$AGENT_TOKEN_SHA256" = "$(jq -er '.client_fence.token_sha256' "$STATE_SNAPSHOT")" ] ||
    die "entitled phase must use the same installed client credential"
fi

TOKEN_SQL="$WORK_DIR/token.sql"
{
  printf '%s\n' 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;'
  printf "SELECT json_build_object('token_count',count(*)) FROM tokens t\n"
  printf "JOIN agents ag ON ag.id=t.agent_id JOIN realms r ON r.id=ag.realm_id AND r.account_id=t.account_id\n"
  printf "WHERE t.token_hash='%s' AND t.kind='agent' AND t.account_id='%s' AND t.agent_id='%s'\n" \
    "$AGENT_TOKEN_SHA256" "$ACCOUNT_ID" "$AGENT_ID"
  printf "AND r.id='%s' AND t.consumed_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())\n" "$REALM_ID"
  printf '%s\n' "AND t.access_profile='full' AND ag.deleted_at IS NULL AND r.deleted_at IS NULL;"
  printf '%s\n' 'COMMIT;'
} >"$TOKEN_SQL"
chmod 600 "$TOKEN_SQL"
TOKEN_OBSERVATION="$WORK_DIR/token-observation.json"
run_sql read "$TOKEN_SQL" "$TOKEN_OBSERVATION"
jq -e 'type=="object" and .token_count==1' "$TOKEN_OBSERVATION" >/dev/null ||
  die "agent token is not one live full credential for the dedicated target"

PLAN_JSON="$WORK_DIR/plan.json"
jq '{plan:.plan,revision:.plan_revision,hash:.plan_hash,applied_epoch:.plan_applied_epoch,
  entitlement_version:.entitlement_version,feature_enabled:.feature_enabled}' "$TARGET_OBSERVATION" >"$PLAN_JSON"
if [ "$PHASE" = disabled ]; then
  NEW_STATE="$WORK_DIR/state-new.json"
  jq -n --arg cell "$CELL" --arg target_sha "$TARGET_SHA256" --arg token_sha "$AGENT_TOKEN_SHA256" \
    --slurpfile target "$TARGET_SNAPSHOT" --slurpfile deployment "$WORK_DIR/deployment-initial.fence.json" \
    --slurpfile config "$WORK_DIR/config-initial.fence.json" --slurpfile cohort "$WORK_DIR/cohort-secret.fence.json" \
    --slurpfile plan "$PLAN_JSON" --slurpfile probe "$PROBE_JSON" '{schema_version:1,cell:$cell,
      target_sha256:$target_sha,target:$target[0],client_fence:{token_sha256:$token_sha},
      deployment_fence:$deployment[0],config_fence:$config[0],
      cohort_fence:$cohort[0],disabled:{outcome:"prepared",plan:$plan[0],probe:$probe[0]}}' >"$NEW_STATE"
  publish_new_state "$NEW_STATE"
  snapshot_file "$STATE_FILE" "$STATE_SNAPSHOT"
else
  UPDATED_STATE="$WORK_DIR/state-prepared.json"
  jq --slurpfile plan "$PLAN_JSON" --slurpfile probe "$PROBE_JSON" \
    '.entitled={outcome:"prepared",plan:$plan[0],probe:$probe[0]}' "$STATE_SNAPSHOT" >"$UPDATED_STATE"
  replace_state "$UPDATED_STATE"
  snapshot_file "$STATE_FILE" "$STATE_SNAPSHOT"
fi

# The broad relay signing key is not read until the cell, cohort, exact target,
# authoritative effective plan, and zero-evidence preconditions all pass.
PRIVATE_KEY_SNAPSHOT="$WORK_DIR/relay-private-key"
snapshot_file "$RELAY_PRIVATE_KEY_FILE" "$PRIVATE_KEY_SNAPSHOT"
KEY_BYTES="$(wc -c <"$PRIVATE_KEY_SNAPSHOT" | tr -d '[:space:]')"
if [[ ! "$KEY_BYTES" =~ ^[0-9]+$ ]] || (( KEY_BYTES < 48 || KEY_BYTES > 512 )); then
  die "relay private key encoding is invalid"
fi
node -e '
  const fs=require("node:fs");
  const value=fs.readFileSync(process.argv[1],"utf8");
  if (!/^[A-Za-z0-9+/]+={0,2}\n?$/.test(value)) process.exit(1);
' "$PRIVATE_KEY_SNAPSHOT" >/dev/null 2>&1 || die "relay private key must be canonical PKCS8 base64"

snapshot_cell_source presend
snapshot_cohort_fence presend
if ! cmp -s "$WORK_DIR/deployment-initial.fence.json" "$WORK_DIR/deployment-presend.fence.json" ||
   ! cmp -s "$WORK_DIR/config-initial.fence.json" "$WORK_DIR/config-presend.fence.json"; then
  die "managed server source drifted before the signed request"
fi

PORT_FORWARD_LOG="$WORK_DIR/port-forward.log"
"${KUBE[@]}" -n "$NAMESPACE" port-forward --address=127.0.0.1 "service/$SERVICE" :80 \
  >"$PORT_FORWARD_LOG" 2>&1 &
PORT_FORWARD_PID=$!
PORT_FORWARD_STOPPED=false
LOCAL_PORT=""
for _attempt in $(seq 1 50); do
  if ! kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then break; fi
  LOCAL_PORT="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> [0-9][0-9]*$/\1/p' "$PORT_FORWARD_LOG" | head -1)"
  if [[ "$LOCAL_PORT" =~ ^[0-9]+$ ]] && (( LOCAL_PORT >= 1 && LOCAL_PORT <= 65535 )); then break; fi
  sleep 0.1
done
if [[ ! "$LOCAL_PORT" =~ ^[0-9]+$ ]] || ! kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
  die "loopback port-forward did not become ready"
fi

VERSION_RESULT="$WORK_DIR/version.json"
VERSION_READY=false
for _attempt in $(seq 1 50); do
  if curl --silent --show-error --fail --max-time 2 --proto '=http' \
      "http://127.0.0.1:$LOCAL_PORT/v1/version" >"$VERSION_RESULT" 2>"$WORK_DIR/version.err"; then
    VERSION_READY=true
    break
  fi
  if ! kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
[ "$VERSION_READY" = true ] || die "loopback server version probe failed"
jq -e --arg version "$SERVER_VERSION" 'type=="object" and .schema_version=="witself.v0" and .version==$version' \
  "$VERSION_RESULT" >/dev/null || die "loopback server version does not match the fenced release"

EXPECTED_VERDICT=feature_disabled
EXPECTED_OWNER_GATE=feature_disabled
if [ "$PHASE" = entitled ]; then
  EXPECTED_VERDICT=accepted
  EXPECTED_OWNER_GATE=address_available
fi
RELAY_RESULT="$WORK_DIR/relay-result.json"
set +e
node "$RELAY_HELPER" \
  --audience "$CELL" \
  --agent-token-file "$AGENT_TOKEN_SNAPSHOT" \
  --expected-verdict "$EXPECTED_VERDICT" \
  --expected-owner-gate "$EXPECTED_OWNER_GATE" \
  --key-id "$RELAY_KEY_ID" \
  --private-key-file "$PRIVATE_KEY_SNAPSHOT" \
  --probe-file "$PROBE_JSON" \
  --public-keys-file "$PUBLIC_KEYS_FILE" \
  --raw-file "$RAW_FILE" \
  --result-file "$RELAY_RESULT" \
  --target-file "$TARGET_SNAPSHOT" \
  --url "http://127.0.0.1:$LOCAL_PORT/v1/internal/agent-email:ingest" \
  >"$WORK_DIR/relay.stdout" 2>"$WORK_DIR/relay.stderr"
RELAY_STATUS=$?
set -e
stop_port_forward || die "loopback port-forward could not be stopped"

snapshot_cell_source postsend
snapshot_cohort_fence postsend
if ! cmp -s "$WORK_DIR/deployment-initial.fence.json" "$WORK_DIR/deployment-postsend.fence.json" ||
   ! cmp -s "$WORK_DIR/config-initial.fence.json" "$WORK_DIR/config-postsend.fence.json"; then
  die "managed server source drifted during the signed request"
fi
TARGET_AFTER="$WORK_DIR/target-after.json"
run_sql read "$TARGET_SQL" "$TARGET_AFTER"
jq -e --slurpfile before "$TARGET_OBSERVATION" '
  .target_count==1 and .account_status=="active" and .receive_state=="enabled" and
  .plan==$before[0].plan and .plan_revision==$before[0].plan_revision and
  .plan_hash==$before[0].plan_hash and .plan_applied_epoch==$before[0].plan_applied_epoch and
  .entitlement_version==$before[0].entitlement_version and .feature_enabled==$before[0].feature_enabled
' "$TARGET_AFTER" >/dev/null || die "target policy drifted during the signed request"

EVIDENCE_AFTER_SQL="$WORK_DIR/evidence-after.sql"
EVIDENCE_AFTER="$WORK_DIR/evidence-after.json"
write_evidence_sql "$PROBE_JSON" "$EVIDENCE_AFTER_SQL"
run_sql read "$EVIDENCE_AFTER_SQL" "$EVIDENCE_AFTER"

if (( RELAY_STATUS != 0 )); then
  # The request is never replayed. State remains prepared so cleanup can
  # reconcile either zero rows or one accepted row without another POST.
  die "signed request outcome is indeterminate; retain state and run cleanup"
fi
jq -e --arg verdict "$EXPECTED_VERDICT" --arg owner "$EXPECTED_OWNER_GATE" '
  (keys|sort)==["http_status","owner_gate","verdict"] and
  .owner_gate==$owner and .http_status==200 and .verdict==$verdict
' "$RELAY_RESULT" >/dev/null ||
  die "signed relay result is invalid"

if [ "$PHASE" = disabled ]; then
  OWNER_EVENTS_BEFORE="$(jq -er '.owner_events' "$EVIDENCE_BEFORE")"
  jq -e --argjson before "$OWNER_EVENTS_BEFORE" '
    .messages==0 and .deliveries==0 and .events==0 and .owner_events==$before
  ' "$EVIDENCE_AFTER" >/dev/null ||
    die "Personal receive was not discarded without persistence"
  UPDATED_STATE="$WORK_DIR/state-disabled-complete.json"
  jq '.disabled.outcome="verified" | .disabled.verdict="feature_disabled" |
    .disabled.owner_gate="feature_disabled" |
    .disabled.evidence={messages:0,deliveries:0,events:0}' "$STATE_SNAPSHOT" >"$UPDATED_STATE"
  replace_state "$UPDATED_STATE"
  jq -cn '{schema_version:1,phase:"disabled",http_status:200,verdict:"feature_disabled",
    owner_gate:"feature_disabled",same_client_credential_fenced:true,
    messages_before:0,messages_after:0,deliveries_before:0,deliveries_after:0,
    audit_events_before:0,audit_events_after:0,same_deployment:true,
    owner_receive_event_delta:0,
    plan_transition_ready:true,provider_mutation_performed:false}'
  exit 0
fi

OWNER_EVENTS_BEFORE="$(jq -er '.owner_events' "$EVIDENCE_BEFORE")"
jq -e --argjson before "$OWNER_EVENTS_BEFORE" '
  .messages==1 and .deliveries==1 and .events==1 and .owner_events==($before+1)
' "$EVIDENCE_AFTER" >/dev/null ||
  die "Professional receive did not persist exactly one delivery"
UPDATED_STATE="$WORK_DIR/state-entitled-verified.json"
jq '.entitled.outcome="verified" | .entitled.verdict="accepted" |
  .entitled.owner_gate="address_available" |
  .entitled.evidence={messages:1,deliveries:1,events:1}' "$STATE_SNAPSHOT" >"$UPDATED_STATE"
replace_state "$UPDATED_STATE"
snapshot_file "$STATE_FILE" "$STATE_SNAPSHOT"

# Reuse the recovery phase in a fresh process so the accepted synthetic row is
# removed only after this process has released its port-forward and operation
# lock. The state already carries the exact immutable cleanup evidence. We do
# not recurse here; the runbook makes cleanup an explicit, independently
# observable operator step.
jq -cn '{schema_version:1,phase:"entitled",http_status:200,verdict:"accepted",
  owner_gate:"address_available",same_client_credential:true,
  messages_before:0,messages_after:1,deliveries_before:0,deliveries_after:1,
  audit_events_before:0,audit_events_after:1,same_deployment:true,
  owner_receive_event_delta:1,
  plan_flip_verified_without_reinstall:true,cleanup_required:true,
  provider_mutation_performed:false}'
