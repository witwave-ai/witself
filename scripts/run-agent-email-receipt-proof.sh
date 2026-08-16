#!/usr/bin/env bash
# Run one accepted-receipt proof in a transient operator-owned Job. The live
# worker Deployment is read-only source material and is never exec'd.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: run-agent-email-receipt-proof.sh \
  --cell CELL \
  --kubeconfig FILE \
  --context CONTEXT \
  --namespace NAMESPACE \
  --expected-image ghcr.io/witwave-ai/images/witself-server:X.Y.Z \
  --expected-config-checksum SHA256 \
  --expected-replicas 2 \
  --account-id ACCOUNT_ID \
  --send-id SEND_ID \
  --expected-accepted-at RFC3339NANO \
  [--deployment DEPLOYMENT] \
  [--timeout-seconds 60-600]

The helper requires an exact fully ready witself-worker Deployment with
outbound email enabled. It copies only the proof command's non-secret outbound
configuration and the existing database and dispatch-key Secret references;
it never reads Secret values. The expected attempt count is fixed at one.

On success stdout contains only the closed value-free receipt proof. A fixed
Job and immutable ConfigMap lock prevent concurrent proof runs.
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

require_private_kubeconfig() {
  local path="$1"
  local mode size
  [ -f "$path" ] && [ ! -L "$path" ] || die "kubeconfig must be a regular non-symbolic file"
  mode="$(file_mode "$path")"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die "could not determine kubeconfig permissions"
  (( (8#$mode & 8#077) == 0 )) || die "kubeconfig must not be accessible by group or other users"
  size="$(wc -c <"$path" | tr -d '[:space:]')"
  if [[ ! "$size" =~ ^[0-9]+$ ]] || (( size < 1 || size > 1048576 )); then
    die "kubeconfig has an invalid size"
  fi
}

validate_source_deployment() {
  local source_file="$1"
  local deployment_name="$2"
  local expected_image="$3"
  local expected_checksum="$4"
  local expected_replicas="$5"
  jq -e --arg deployment "$deployment_name" --arg image "$expected_image" \
    --arg checksum "$expected_checksum" --argjson replicas "$expected_replicas" '
    .metadata.name == $deployment and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    (.metadata.generation | type == "number") and
    (.status.observedGeneration == .metadata.generation) and
    .spec.replicas == $replicas and
    (.status.replicas == .spec.replicas) and
    (.status.readyReplicas == .spec.replicas) and
    (.status.updatedReplicas == .spec.replicas) and
    (.status.availableReplicas == .spec.replicas) and
    ((.status.unavailableReplicas // 0) == 0) and
    .spec.template.spec.automountServiceAccountToken == false and
    (.spec.template.spec.serviceAccountName | type == "string" and length > 0) and
    .spec.template.metadata.annotations["checksum/config"] == $checksum and
    ([.spec.template.spec.containers[] | select(.name == "witself-worker")] | length == 1) and
    (.spec.template.spec.containers[] | select(.name == "witself-worker") |
      .image == $image and
      .command == ["/usr/local/bin/witself-worker"] and
      .args == ["serve"] and
      ([.envFrom[]? | select(.configMapRef.name != null)] | length == 1))
  ' "$source_file" >/dev/null
}

validate_cell_identity_deployment() {
  local source_file="$1"
  local expected_image="$2"
  jq -e --arg image "$expected_image" '
    .metadata.name == "witself-server" and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    (.metadata.generation | type == "number") and
    (.status.observedGeneration == .metadata.generation) and
    (.spec.replicas | type == "number" and . >= 1) and
    (.status.replicas == .spec.replicas) and
    (.status.readyReplicas == .spec.replicas) and
    (.status.updatedReplicas == .spec.replicas) and
    (.status.availableReplicas == .spec.replicas) and
    ((.status.unavailableReplicas // 0) == 0) and
    ([.spec.template.spec.containers[] | select(.name == "witself-server")] | length == 1) and
    (.spec.template.spec.containers[] | select(.name == "witself-server") |
      .image == $image and
      ([.envFrom[]? | select(.configMapRef.name != null)] | length == 1)) and
    (.spec.template.metadata.annotations["witself.io/server-config-checksum"] |
      type == "string" and test("^[0-9a-f]{64}$"))
  ' "$source_file" >/dev/null
}

extract_cell_identity_config_name() {
  jq -er '
    [.spec.template.spec.containers[] | select(.name == "witself-server") |
     .envFrom[]? | select(.configMapRef.name != null) | .configMapRef.name] |
    if length == 1 and (.[0] | type == "string" and length > 0)
    then .[0] else error("expected one server ConfigMap") end
  ' "$1"
}

validate_cell_identity_config() {
  local source_file="$1"
  local config_name="$2"
  local cell_name="$3"
  local expected_checksum="$4"
  jq -e --arg config "$config_name" --arg cell "$cell_name" \
    --arg checksum "$expected_checksum" '
    .metadata.name == $config and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    .metadata.annotations["witself.io/server-config-checksum"] == $checksum and
    .data.WITSELF_BACKEND_KIND == "managed" and
    .data.WITSELF_CELL_NAME == $cell
  ' "$source_file" >/dev/null
}

write_cell_identity_deployment_fence() {
  jq -S '
    (.spec.template.spec.containers[] | select(.name == "witself-server")) as $server |
    {
      uid: .metadata.uid,
      resourceVersion: .metadata.resourceVersion,
      generation: .metadata.generation,
      observedGeneration: .status.observedGeneration,
      replicas: .spec.replicas,
      readyReplicas: .status.readyReplicas,
      updatedReplicas: .status.updatedReplicas,
      availableReplicas: .status.availableReplicas,
      unavailableReplicas: (.status.unavailableReplicas // 0),
      configChecksum: .spec.template.metadata.annotations["witself.io/server-config-checksum"],
      image: $server.image,
      configMapRefs: [$server.envFrom[]? | select(.configMapRef.name != null) | .configMapRef]
    }
  ' "$1" >"$2"
}

write_cell_identity_config_fence() {
  jq -S '{
    uid: .metadata.uid,
    resourceVersion: .metadata.resourceVersion,
    checksum: .metadata.annotations["witself.io/server-config-checksum"],
    backendKind: .data.WITSELF_BACKEND_KIND,
    cellName: .data.WITSELF_CELL_NAME
  }' "$1" >"$2"
}

extract_source_config_name() {
  jq -er '
    [.spec.template.spec.containers[] | select(.name == "witself-worker") |
     .envFrom[]? | select(.configMapRef.name != null) | .configMapRef.name] |
    if length == 1 and (.[0] | type == "string" and length > 0)
    then .[0] else error("expected one worker ConfigMap") end
  ' "$1"
}

write_private_env_refs() {
  jq -cer '
    [.spec.template.spec.containers[] | select(.name == "witself-worker") | .env[]? |
     select(.name == "WITSELF_DATABASE_URL" or
            .name == "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY")] |
    sort_by(.name) |
    if (map(.name) == ["WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY",
                       "WITSELF_DATABASE_URL"]) and
       all(.[];
         (.value == null) and
         (.valueFrom | keys == ["secretKeyRef"]) and
         (.valueFrom.secretKeyRef.name | type == "string" and length > 0) and
         (.valueFrom.secretKeyRef.key | type == "string" and length > 0) and
         ((.valueFrom.secretKeyRef.optional // false) == false))
    then map({name: .name, valueFrom: {secretKeyRef: {
      name: .valueFrom.secretKeyRef.name,
      key: .valueFrom.secretKeyRef.key
    }}})
    else error("expected exact private Secret references") end
  ' "$1" >"$2"
}

validate_source_config() {
  local source_file="$1"
  local config_name="$2"
  jq -e --arg config "$config_name" '
    .metadata.name == $config and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    .data.WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED == "true" and
    (.data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT |
      type == "string" and test("^https://[^/?#[:space:]]+/(?:[^?#[:space:]]*/)?v1/dispatch$")) and
    .data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE == "witself-agent-email-send" and
    (.data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID |
      type == "string" and test("^[a-z][a-z0-9_.-]{0,63}$")) and
    (.data.WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT |
      type == "string" and test("^[1-9][0-9]*(ms|s)$")) and
    (.data.WITSELF_DATABASE_URL == null) and
    (.data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY == null)
  ' "$source_file" >/dev/null
}

write_selected_config() {
  jq -S '{
    WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT:
      .data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT,
    WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID:
      .data.WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID,
    WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT:
      .data.WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT
  }' "$1" >"$2"
}

write_deployment_fence() {
  local source_file="$1"
  local private_env_file="$2"
  local output_file="$3"
  jq -S --slurpfile private_env "$private_env_file" '
    (.spec.template.spec.containers[] | select(.name == "witself-worker")) as $worker |
    {
      uid: .metadata.uid,
      resourceVersion: .metadata.resourceVersion,
      generation: .metadata.generation,
      observedGeneration: .status.observedGeneration,
      replicas: .spec.replicas,
      readyReplicas: .status.readyReplicas,
      updatedReplicas: .status.updatedReplicas,
      availableReplicas: .status.availableReplicas,
      unavailableReplicas: (.status.unavailableReplicas // 0),
      configChecksum: .spec.template.metadata.annotations["checksum/config"],
      image: $worker.image,
      imagePullPolicy: ($worker.imagePullPolicy // "IfNotPresent"),
      command: $worker.command,
      args: $worker.args,
      configMapRefs: [$worker.envFrom[]? | select(.configMapRef.name != null) | .configMapRef],
      privateEnv: $private_env[0],
      serviceAccountName: .spec.template.spec.serviceAccountName,
      imagePullSecrets: (.spec.template.spec.imagePullSecrets // []),
      nodeSelector: (.spec.template.spec.nodeSelector // {}),
      tolerations: (.spec.template.spec.tolerations // []),
      affinity: (.spec.template.spec.affinity // {}),
      resources: ($worker.resources // {})
    }
  ' "$source_file" >"$output_file"
}

write_config_fence() {
  local source_file="$1"
  local selected_config_file="$2"
  local output_file="$3"
  jq -S --slurpfile selected "$selected_config_file" '{
    uid: .metadata.uid,
    resourceVersion: .metadata.resourceVersion,
    selectedData: $selected[0]
  }' "$source_file" >"$output_file"
}

CELL=""
KUBECONFIG_FILE=""
KUBE_CONTEXT=""
NAMESPACE=""
DEPLOYMENT="witself-worker"
EXPECTED_IMAGE=""
EXPECTED_CONFIG_CHECKSUM=""
EXPECTED_REPLICAS=""
ACCOUNT_ID=""
SEND_ID=""
EXPECTED_ACCEPTED_AT=""
TIMEOUT_SECONDS=120

while [ "$#" -gt 0 ]; do
  case "$1" in
    --cell) [ "$#" -ge 2 ] || die "incomplete arguments"; CELL="$2"; shift 2 ;;
    --kubeconfig) [ "$#" -ge 2 ] || die "incomplete arguments"; KUBECONFIG_FILE="$2"; shift 2 ;;
    --context) [ "$#" -ge 2 ] || die "incomplete arguments"; KUBE_CONTEXT="$2"; shift 2 ;;
    --namespace) [ "$#" -ge 2 ] || die "incomplete arguments"; NAMESPACE="$2"; shift 2 ;;
    --deployment) [ "$#" -ge 2 ] || die "incomplete arguments"; DEPLOYMENT="$2"; shift 2 ;;
    --expected-image) [ "$#" -ge 2 ] || die "incomplete arguments"; EXPECTED_IMAGE="$2"; shift 2 ;;
    --expected-config-checksum) [ "$#" -ge 2 ] || die "incomplete arguments"; EXPECTED_CONFIG_CHECKSUM="$2"; shift 2 ;;
    --expected-replicas) [ "$#" -ge 2 ] || die "incomplete arguments"; EXPECTED_REPLICAS="$2"; shift 2 ;;
    --account-id) [ "$#" -ge 2 ] || die "incomplete arguments"; ACCOUNT_ID="$2"; shift 2 ;;
    --send-id) [ "$#" -ge 2 ] || die "incomplete arguments"; SEND_ID="$2"; shift 2 ;;
    --expected-accepted-at) [ "$#" -ge 2 ] || die "incomplete arguments"; EXPECTED_ACCEPTED_AT="$2"; shift 2 ;;
    --timeout-seconds) [ "$#" -ge 2 ] || die "incomplete arguments"; TIMEOUT_SECONDS="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown or incomplete argument" ;;
  esac
done

for required_value in "$CELL" "$KUBECONFIG_FILE" "$KUBE_CONTEXT" "$NAMESPACE" \
  "$EXPECTED_IMAGE" "$EXPECTED_CONFIG_CHECKSUM" "$EXPECTED_REPLICAS" "$ACCOUNT_ID" "$SEND_ID" \
  "$EXPECTED_ACCEPTED_AT"; do
  [ -n "$required_value" ] || { usage >&2; die "required arguments are missing"; }
done
for value in "$CELL" "$NAMESPACE" "$DEPLOYMENT"; do
  [[ "$value" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "a Kubernetes name is invalid"
done
[[ "$KUBE_CONTEXT" =~ ^[A-Za-z0-9._:@/-]+$ ]] || die "context contains unsupported characters"
[[ "$EXPECTED_IMAGE" =~ ^ghcr\.io/witwave-ai/images/witself-server:([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
  die "expected image must be an exact released witself-server tag"
IMAGE_MAJOR="${BASH_REMATCH[1]}"
IMAGE_MINOR="${BASH_REMATCH[2]}"
IMAGE_PATCH="${BASH_REMATCH[3]}"
if (( 10#$IMAGE_MAJOR == 0 && 10#$IMAGE_MINOR == 0 && 10#$IMAGE_PATCH < 249 )); then
  die "accepted-receipt proof requires image v0.0.249 or newer"
fi
[[ "$EXPECTED_CONFIG_CHECKSUM" =~ ^[0-9a-f]{64}$ ]] || die "expected config checksum is invalid"
[ "$EXPECTED_REPLICAS" = 2 ] || die "expected replicas must be exactly 2 for the production proof"
[[ "$ACCOUNT_ID" =~ ^acc_[a-z2-7]{16}$ ]] || die "account id is invalid"
[[ "$SEND_ID" =~ ^esnd_[a-z2-7]{16}$ ]] || die "send id is invalid"
[[ "$EXPECTED_ACCEPTED_AT" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$ ]] ||
  die "expected accepted time must be an exact UTC RFC3339Nano value"
[[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || die "timeout must be an integer"
(( TIMEOUT_SECONDS >= 60 && TIMEOUT_SECONDS <= 600 )) ||
  die "timeout must be between 60 and 600 seconds"
CLEANUP_TIMEOUT_SECONDS="${WITSELF_AGENT_EMAIL_RECEIPT_PROOF_CLEANUP_TIMEOUT_SECONDS:-30}"
[[ "$CLEANUP_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || die "cleanup timeout must be an integer"
(( CLEANUP_TIMEOUT_SECONDS >= 1 && CLEANUP_TIMEOUT_SECONDS <= 120 )) ||
  die "cleanup timeout must be between 1 and 120 seconds"

for command_name in cmp jq kubectl stat; do require_command "$command_name"; done
KUBECONFIG_FILE="$(canonical_file_path "$KUBECONFIG_FILE")" ||
  die "kubeconfig path must be canonical and absolute"
require_private_kubeconfig "$KUBECONFIG_FILE"

umask 077
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/witself-agent-email-receipt-proof.XXXXXX")"
chmod 700 "$WORK_DIR"
LOCK_CREATED=false
JOB_CREATE_ATTEMPTED=false
JOB_CREATED=false
JOB_NAME="witself-agent-email-receipt-proof"
LOCK_NAME="witself-agent-email-receipt-proof-lock"
KUBE=(kubectl --request-timeout=30s --kubeconfig "$KUBECONFIG_FILE" --context "$KUBE_CONTEXT" -n "$NAMESPACE")

proof_pods_are_absent() {
  local deadline=$((SECONDS + CLEANUP_TIMEOUT_SECONDS))
  local pods_file="$WORK_DIR/cleanup-pods.json"
  while true; do
    if "${KUBE[@]}" get pods -l "batch.kubernetes.io/job-name=$JOB_NAME" -o json \
        >"$pods_file" 2>/dev/null &&
       [ "$(jq -r '.items | length' "$pods_file" 2>/dev/null)" = 0 ]; then
      return 0
    fi
    (( SECONDS < deadline )) || return 1
    sleep 1
  done
}

cleanup() {
  local status=$?
  local safe_to_unlock=false
  trap - EXIT INT TERM
  if [ "$JOB_CREATED" = true ]; then
    if "${KUBE[@]}" delete job "$JOB_NAME" --ignore-not-found=true \
        --cascade=foreground --wait=true --timeout="${CLEANUP_TIMEOUT_SECONDS}s" \
        >/dev/null 2>&1 && proof_pods_are_absent; then
      safe_to_unlock=true
    fi
  elif [ "$JOB_CREATE_ATTEMPTED" != true ]; then
    safe_to_unlock=true
  fi
  if [ "$LOCK_CREATED" = true ] && [ "$safe_to_unlock" = true ]; then
    if ! "${KUBE[@]}" delete configmap "$LOCK_NAME" --ignore-not-found=true \
        --wait=true --timeout="${CLEANUP_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
      safe_to_unlock=false
    fi
  fi
  if [ "$LOCK_CREATED" = true ] && [ "$safe_to_unlock" != true ]; then
    printf '%s\n' \
      'warning: receipt-proof cleanup could not prove the runner absent; the fixed lock was retained' >&2
  fi
  find "$WORK_DIR" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$WORK_DIR" 2>/dev/null || true
  exit "$status"
}
trap cleanup EXIT INT TERM

read_and_validate_sources() {
  local deployment_file="$1"
  local config_file="$2"
  local private_env_file="$3"
  local selected_config_file="$4"
  local deployment_fence_file="$5"
  local config_fence_file="$6"
  local database_secret_fence_file="$7"
  local dispatch_secret_fence_file="$8"

  "${KUBE[@]}" get deployment "$DEPLOYMENT" -o json >"$deployment_file" 2>/dev/null || return 1
  validate_source_deployment "$deployment_file" "$DEPLOYMENT" "$EXPECTED_IMAGE" \
    "$EXPECTED_CONFIG_CHECKSUM" "$EXPECTED_REPLICAS" || return 1
  local current_config_name
  current_config_name="$(extract_source_config_name "$deployment_file" 2>/dev/null)" || return 1
  if [ -n "${CONFIG_NAME:-}" ] && [ "$current_config_name" != "$CONFIG_NAME" ]; then
    return 1
  fi
  "${KUBE[@]}" get configmap "$current_config_name" -o json >"$config_file" 2>/dev/null || return 1
  validate_source_config "$config_file" "$current_config_name" || return 1
  write_private_env_refs "$deployment_file" "$private_env_file" 2>/dev/null || return 1
  write_selected_config "$config_file" "$selected_config_file"
  write_deployment_fence "$deployment_file" "$private_env_file" "$deployment_fence_file"
  write_config_fence "$config_file" "$selected_config_file" "$config_fence_file"

  local database_secret_name dispatch_secret_name
  database_secret_name="$(jq -er '.[] | select(.name == "WITSELF_DATABASE_URL") |
    .valueFrom.secretKeyRef.name' "$private_env_file")" || return 1
  dispatch_secret_name="$(jq -er '.[] |
    select(.name == "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY") |
    .valueFrom.secretKeyRef.name' "$private_env_file")" || return 1
  [ "$database_secret_name" != "$dispatch_secret_name" ] || return 1
  if [ -n "${DATABASE_SECRET_NAME:-}" ] && [ "$database_secret_name" != "$DATABASE_SECRET_NAME" ]; then
    return 1
  fi
  if [ -n "${DISPATCH_SECRET_NAME:-}" ] && [ "$dispatch_secret_name" != "$DISPATCH_SECRET_NAME" ]; then
    return 1
  fi
  "${KUBE[@]}" get secret "$database_secret_name" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}' \
    >"$database_secret_fence_file" 2>/dev/null || return 1
  "${KUBE[@]}" get secret "$dispatch_secret_name" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}{.immutable}{"\n"}' \
    >"$dispatch_secret_fence_file" 2>/dev/null || return 1
  [ -n "$(sed -n '1p' "$database_secret_fence_file")" ] &&
    [ -n "$(sed -n '2p' "$database_secret_fence_file")" ] &&
    [ -n "$(sed -n '1p' "$dispatch_secret_fence_file")" ] &&
    [ -n "$(sed -n '2p' "$dispatch_secret_fence_file")" ] &&
    [ "$(sed -n '3p' "$dispatch_secret_fence_file")" = true ]
}

read_and_validate_cell_identity() {
  local deployment_file="$1"
  local config_file="$2"
  local deployment_fence_file="$3"
  local config_fence_file="$4"
  "${KUBE[@]}" get deployment witself-server -o json >"$deployment_file" 2>/dev/null || return 1
  validate_cell_identity_deployment "$deployment_file" "$EXPECTED_IMAGE" || return 1
  local config_name checksum
  config_name="$(extract_cell_identity_config_name "$deployment_file" 2>/dev/null)" || return 1
  if [ -n "${CELL_IDENTITY_CONFIG_NAME:-}" ] && [ "$config_name" != "$CELL_IDENTITY_CONFIG_NAME" ]; then
    return 1
  fi
  checksum="$(jq -er '.spec.template.metadata.annotations["witself.io/server-config-checksum"]' \
    "$deployment_file")" || return 1
  "${KUBE[@]}" get configmap "$config_name" -o json >"$config_file" 2>/dev/null || return 1
  validate_cell_identity_config "$config_file" "$config_name" "$CELL" "$checksum" || return 1
  write_cell_identity_deployment_fence "$deployment_file" "$deployment_fence_file"
  write_cell_identity_config_fence "$config_file" "$config_fence_file"
}

INITIAL_DEPLOYMENT="$WORK_DIR/deployment-initial.json"
INITIAL_CONFIG="$WORK_DIR/config-initial.json"
INITIAL_PRIVATE_ENV="$WORK_DIR/private-env-initial.json"
INITIAL_SELECTED_CONFIG="$WORK_DIR/selected-config-initial.json"
INITIAL_DEPLOYMENT_FENCE="$WORK_DIR/deployment-initial.fence.json"
INITIAL_CONFIG_FENCE="$WORK_DIR/config-initial.fence.json"
INITIAL_DATABASE_SECRET_FENCE="$WORK_DIR/database-secret-initial.fence"
INITIAL_DISPATCH_SECRET_FENCE="$WORK_DIR/dispatch-secret-initial.fence"
INITIAL_CELL_DEPLOYMENT="$WORK_DIR/cell-deployment-initial.json"
INITIAL_CELL_CONFIG="$WORK_DIR/cell-config-initial.json"
INITIAL_CELL_DEPLOYMENT_FENCE="$WORK_DIR/cell-deployment-initial.fence.json"
INITIAL_CELL_CONFIG_FENCE="$WORK_DIR/cell-config-initial.fence.json"

if ! read_and_validate_sources "$INITIAL_DEPLOYMENT" "$INITIAL_CONFIG" \
    "$INITIAL_PRIVATE_ENV" "$INITIAL_SELECTED_CONFIG" "$INITIAL_DEPLOYMENT_FENCE" \
    "$INITIAL_CONFIG_FENCE" "$INITIAL_DATABASE_SECRET_FENCE" \
    "$INITIAL_DISPATCH_SECRET_FENCE"; then
  die "managed worker source is absent, ambiguous, or not ready for receipt proof"
fi
if ! read_and_validate_cell_identity "$INITIAL_CELL_DEPLOYMENT" "$INITIAL_CELL_CONFIG" \
    "$INITIAL_CELL_DEPLOYMENT_FENCE" "$INITIAL_CELL_CONFIG_FENCE"; then
  die "managed cell identity is absent, ambiguous, or not fully converged"
fi
CONFIG_NAME="$(extract_source_config_name "$INITIAL_DEPLOYMENT")"
CELL_IDENTITY_CONFIG_NAME="$(extract_cell_identity_config_name "$INITIAL_CELL_DEPLOYMENT")"
DATABASE_SECRET_NAME="$(jq -er '.[] | select(.name == "WITSELF_DATABASE_URL") |
  .valueFrom.secretKeyRef.name' "$INITIAL_PRIVATE_ENV")"
DISPATCH_SECRET_NAME="$(jq -er '.[] |
  select(.name == "WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY") |
  .valueFrom.secretKeyRef.name' "$INITIAL_PRIVATE_ENV")"

if ! "${KUBE[@]}" get job "$JOB_NAME" --ignore-not-found -o name \
    >"$WORK_DIR/existing-job.out" 2>/dev/null; then
  die "could not verify receipt-proof Job absence"
fi
if [ -s "$WORK_DIR/existing-job.out" ]; then
  die "an existing receipt-proof Job requires operator cleanup"
fi

compare_sources_to_initial() {
  local suffix="$1"
  local deployment_file="$WORK_DIR/deployment-$suffix.json"
  local config_file="$WORK_DIR/config-$suffix.json"
  local private_env_file="$WORK_DIR/private-env-$suffix.json"
  local selected_config_file="$WORK_DIR/selected-config-$suffix.json"
  local deployment_fence_file="$WORK_DIR/deployment-$suffix.fence.json"
  local config_fence_file="$WORK_DIR/config-$suffix.fence.json"
  local database_secret_fence_file="$WORK_DIR/database-secret-$suffix.fence"
  local dispatch_secret_fence_file="$WORK_DIR/dispatch-secret-$suffix.fence"
  local cell_deployment_file="$WORK_DIR/cell-deployment-$suffix.json"
  local cell_config_file="$WORK_DIR/cell-config-$suffix.json"
  local cell_deployment_fence_file="$WORK_DIR/cell-deployment-$suffix.fence.json"
  local cell_config_fence_file="$WORK_DIR/cell-config-$suffix.fence.json"
  read_and_validate_sources "$deployment_file" "$config_file" "$private_env_file" \
    "$selected_config_file" "$deployment_fence_file" "$config_fence_file" \
    "$database_secret_fence_file" "$dispatch_secret_fence_file" || return 1
  read_and_validate_cell_identity "$cell_deployment_file" "$cell_config_file" \
    "$cell_deployment_fence_file" "$cell_config_fence_file" || return 1
  cmp -s "$INITIAL_DEPLOYMENT_FENCE" "$deployment_fence_file" &&
    cmp -s "$INITIAL_CONFIG_FENCE" "$config_fence_file" &&
    cmp -s "$INITIAL_DATABASE_SECRET_FENCE" "$database_secret_fence_file" &&
    cmp -s "$INITIAL_DISPATCH_SECRET_FENCE" "$dispatch_secret_fence_file" &&
    cmp -s "$INITIAL_CELL_DEPLOYMENT_FENCE" "$cell_deployment_fence_file" &&
    cmp -s "$INITIAL_CELL_CONFIG_FENCE" "$cell_config_fence_file"
}

# This is the final source read immediately before the first mutation.
compare_sources_to_initial prelock || die "managed worker source drifted before lock creation"

if ! jq -n --arg name "$LOCK_NAME" --arg image "$EXPECTED_IMAGE" \
    --arg checksum "$EXPECTED_CONFIG_CHECKSUM" --arg cell "$CELL" \
    --slurpfile data "$INITIAL_SELECTED_CONFIG" '
  {
    apiVersion: "v1", kind: "ConfigMap",
    metadata: {
      name: $name,
      annotations: {
        "witself.io/source-image": $image,
        "witself.io/source-config-checksum": $checksum
      },
      labels: {
        "app.kubernetes.io/name": "witself-agent-email-receipt-proof",
        "app.kubernetes.io/component": "operator-proof",
        "app.kubernetes.io/managed-by": "witself-operator",
        "witself.io/cell": $cell
      }
    },
    immutable: true,
    data: $data[0]
  }
' | "${KUBE[@]}" create -f - >/dev/null 2>"$WORK_DIR/create-lock.err"; then
  die "another receipt-proof operation is active or requires cleanup"
fi
LOCK_CREATED=true

# Recheck once more after acquiring the fixed lock and immediately before Job
# creation. No live source is used if any metadata or selected input drifted.
compare_sources_to_initial prejob || die "managed worker source drifted before Job creation"

JOB_CREATE_ATTEMPTED=true
if ! jq -n \
    --arg name "$JOB_NAME" \
    --arg image "$EXPECTED_IMAGE" \
    --arg config "$LOCK_NAME" \
    --arg checksum "$EXPECTED_CONFIG_CHECKSUM" \
    --arg cell "$CELL" \
    --arg account_id "$ACCOUNT_ID" \
    --arg send_id "$SEND_ID" \
    --arg accepted_at "$EXPECTED_ACCEPTED_AT" \
    --argjson timeout "$TIMEOUT_SECONDS" \
    --slurpfile private_env "$INITIAL_PRIVATE_ENV" \
    --slurpfile deployment "$INITIAL_DEPLOYMENT" '
  ($deployment[0]) as $source |
  ($source.spec.template.spec.containers[] | select(.name == "witself-worker")) as $worker |
  {
    apiVersion: "batch/v1", kind: "Job",
    metadata: {
      name: $name,
      annotations: {"witself.io/source-config-checksum": $checksum},
      labels: {
        "app.kubernetes.io/name": "witself-agent-email-receipt-proof",
        "app.kubernetes.io/component": "operator-proof",
        "app.kubernetes.io/managed-by": "witself-operator",
        "witself.io/cell": $cell
      }
    },
    spec: {
      backoffLimit: 0,
      activeDeadlineSeconds: $timeout,
      ttlSecondsAfterFinished: 3600,
      template: {
        metadata: {
          annotations: {"witself.io/source-config-checksum": $checksum},
          labels: {
            "app.kubernetes.io/name": "witself-agent-email-receipt-proof",
            "app.kubernetes.io/component": "operator-proof",
            "app.kubernetes.io/managed-by": "witself-operator",
            "witself.io/cell": $cell
          }
        },
        spec: {
          restartPolicy: "Never",
          automountServiceAccountToken: false,
          enableServiceLinks: false,
          terminationGracePeriodSeconds: 30,
          serviceAccountName: $source.spec.template.spec.serviceAccountName,
          imagePullSecrets: ($source.spec.template.spec.imagePullSecrets // []),
          nodeSelector: ($source.spec.template.spec.nodeSelector // {}),
          tolerations: ($source.spec.template.spec.tolerations // []),
          affinity: ($source.spec.template.spec.affinity // {}),
          securityContext: {
            runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532,
            seccompProfile: {type: "RuntimeDefault"}
          },
          containers: [{
            name: "runner",
            image: $image,
            imagePullPolicy: ($worker.imagePullPolicy // "IfNotPresent"),
            command: ["/usr/local/bin/witself-worker"],
            args: [
              "agent-email", "receipt-replay",
              "--account-id", $account_id,
              "--send-id", $send_id,
              "--expected-accepted-at", $accepted_at,
              "--expected-attempt-count", "1",
              "--json"
            ],
            envFrom: [{configMapRef: {name: $config}}],
            env: $private_env[0],
            securityContext: {
              allowPrivilegeEscalation: false,
              readOnlyRootFilesystem: true,
              runAsNonRoot: true,
              capabilities: {drop: ["ALL"]}
            },
            resources: ($worker.resources // {})
          }]
        }
      }
    }
  }
' | "${KUBE[@]}" create -f - >/dev/null 2>"$WORK_DIR/create-job.err"; then
  die "receipt-proof Job creation was not confirmed; the fixed lock was retained"
fi
JOB_CREATED=true

DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
POD_NAME=""
RUNNER_EXIT=""
while [ -z "$RUNNER_EXIT" ]; do
  PODS_JSON="$WORK_DIR/pods.json"
  if "${KUBE[@]}" get pods -l "batch.kubernetes.io/job-name=$JOB_NAME" -o json \
      >"$PODS_JSON" 2>/dev/null; then
    POD_COUNT="$(jq -r '.items | length' "$PODS_JSON")"
    [ "$POD_COUNT" -le 1 ] || die "receipt-proof Job created more than one pod"
    if [ "$POD_COUNT" -eq 1 ]; then
      POD_NAME="$(jq -er '.items[0].metadata.name' "$PODS_JSON")" ||
        die "receipt-proof pod name is unavailable"
      RUNNER_EXIT="$(jq -r '[.items[0].status.containerStatuses[]? |
        select(.name == "runner") | .state.terminated.exitCode][0] // empty' "$PODS_JSON")"
    fi
  fi
  if [ -z "$RUNNER_EXIT" ]; then
    (( SECONDS < DEADLINE )) || die "receipt-proof Job timed out"
    sleep 1
  fi
done
[ "$RUNNER_EXIT" -eq 0 ] || die "receipt-proof Job failed"

RUNNER_LOG="$WORK_DIR/runner.log"
if ! "${KUBE[@]}" logs "$POD_NAME" -c runner --tail=4 --limit-bytes=16384 \
    >"$RUNNER_LOG" 2>"$WORK_DIR/runner-log.err"; then
  die "receipt-proof output is unavailable"
fi

PROOF_FILE="$WORK_DIR/proof.json"
if ! jq -ce --arg send_id "$SEND_ID" '
  select(
    (keys | sort == ["digest_matched","provider_call_started_count","receipt_state",
      "route_pending","schema_version","send_id","signer_matched","verified_replay_count"]) and
    .schema_version == "witself.agent-email-dispatch-receipt-proof.v1" and
    .send_id == $send_id and
    .receipt_state == "accepted" and
    .digest_matched == true and
    .signer_matched == true and
    .provider_call_started_count == 1 and
    (.verified_replay_count | type == "number" and . >= 1 and . <= 1000000 and floor == .) and
    .route_pending == false
  ) |
  {
    schema_version, send_id, receipt_state, digest_matched, signer_matched,
    provider_call_started_count, verified_replay_count, route_pending
  }
' "$RUNNER_LOG" >"$PROOF_FILE" 2>/dev/null; then
  die "receipt-proof output failed closed structural validation"
fi
[ "$(wc -l <"$PROOF_FILE" | tr -d '[:space:]')" = 1 ] ||
  die "receipt-proof output was ambiguous"
printf '%s\n' "$(cat "$PROOF_FILE")"
