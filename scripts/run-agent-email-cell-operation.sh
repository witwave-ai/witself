#!/usr/bin/env bash
# Run exactly one production agent-email backfill or canary export against a
# managed cell. The active API Deployment is read-only source material: this
# script creates a separate, fixed-name Kubernetes Job and never execs the
# operation in an API pod.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: run-agent-email-cell-operation.sh \
  --cell CELL \
  --kubeconfig FILE \
  --context CONTEXT \
  --operation backfill|canary-manifest \
  --artifact-output NEW_ABSOLUTE_PRIVATE_JSON \
  [--overrides ABSOLUTE_PRIVATE_JSON] \
  [--namespace NAMESPACE] \
  [--deployment DEPLOYMENT] \
  [--timeout-seconds 60-10800]

The output path must be outside the source checkout and its parent directory
must be private. For a successful backfill the path remains absent; it is used
only when an exception requires an operator override. Canary generation always
exports the generated manifest there.

The script snapshots the active non-secret server ConfigMap and copies only the
exact database and receive-cohort Secret references from the active Deployment.
It never reads either Secret value. A fixed Job name prevents concurrent runs.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

file_mode() {
  local path="$1"
  local mode
  mode="$(stat -f '%Lp' "$path" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$path" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

require_private_file() {
  local label="$1"
  local path="$2"
  local required_exact_mode="${3:-false}"
  local mode
  [ -f "$path" ] || die "$label is not a regular file"
  [ ! -L "$path" ] || die "$label must not be a symbolic link"
  mode="$(file_mode "$path")"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die "could not determine permissions for $label"
  if [ "$required_exact_mode" = true ]; then
    [ "$mode" = 600 ] || die "$label must have mode 0600"
  elif (( (8#$mode & 8#077) != 0 )); then
    die "$label must not be accessible by group or other users"
  fi
}

validate_source_deployment() {
  local source_file="$1"
  local deployment_name="$2"
  jq -e --arg deployment "$deployment_name" '
    .metadata.name == $deployment and
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
    (.spec.template.metadata.annotations["witself.io/server-config-checksum"] |
      type == "string" and test("^[0-9a-f]{64}$"))
  ' "$source_file" >/dev/null
}

extract_source_config_name() {
  jq -er '
    [.spec.template.spec.containers[] | select(.name == "witself-server") |
     .envFrom[]? | select(.configMapRef.name != null) | .configMapRef.name] |
    if length == 1 then .[0] else error("expected one server ConfigMap") end
  ' "$1"
}

write_private_env_refs() {
  jq -cer '
    [.spec.template.spec.containers[] | select(.name == "witself-server") | .env[]? |
     select(.name == "WITSELF_DATABASE_URL" or .name == "WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS")] |
    sort_by(.name) |
    if (map(.name) == ["WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS", "WITSELF_DATABASE_URL"]) and
       all(.[]; (.value == null) and
                (.valueFrom.secretKeyRef.name | type == "string" and length > 0) and
                (.valueFrom.secretKeyRef.key | type == "string" and length > 0) and
                ((.valueFrom.secretKeyRef.optional // false) == false))
    then map({name: .name, valueFrom: {secretKeyRef: {
      name: .valueFrom.secretKeyRef.name, key: .valueFrom.secretKeyRef.key
    }}})
    else error("expected exact private env Secret refs") end
  ' "$1" >"$2"
}

validate_source_config() {
  local source_file="$1"
  local config_name="$2"
  local cell_name="$3"
  local expected_checksum="$4"
  jq -e --arg config "$config_name" --arg cell "$cell_name" --arg checksum "$expected_checksum" '
    .metadata.name == $config and
    (.metadata.uid | type == "string" and length > 0) and
    (.metadata.resourceVersion | type == "string" and length > 0) and
    .metadata.annotations["witself.io/server-config-checksum"] == $checksum and
    .data.WITSELF_BACKEND_KIND == "managed" and
    .data.WITSELF_CELL_NAME == $cell and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED == "true" and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED == "false" and
    .data.WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN == "witmail.net" and
    ((.data.WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS // "") == "")
  ' "$source_file" >/dev/null
}

write_deployment_fence() {
  local source_file="$1"
  local private_env_file="$2"
  local output_file="$3"
  jq -S --slurpfile private_env "$private_env_file" '
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
      legacyConfigChecksum: (.spec.template.metadata.annotations["checksum/config"] // null),
      image: $server.image,
      imagePullPolicy: ($server.imagePullPolicy // "IfNotPresent"),
      configMapRefs: [$server.envFrom[]? | select(.configMapRef.name != null) | .configMapRef],
      privateEnv: $private_env[0],
      serviceAccountName: .spec.template.spec.serviceAccountName,
      imagePullSecrets: (.spec.template.spec.imagePullSecrets // []),
      nodeSelector: (.spec.template.spec.nodeSelector // {}),
      tolerations: (.spec.template.spec.tolerations // []),
      affinity: (.spec.template.spec.affinity // {}),
      resources: ($server.resources // {})
    }
  ' "$source_file" >"$output_file"
}

write_config_fence() {
  jq -S '{
    uid: .metadata.uid,
    resourceVersion: .metadata.resourceVersion,
    checksum: .metadata.annotations["witself.io/server-config-checksum"],
    data: (.data // {})
  }' "$1" >"$2"
}

CELL=""
KUBECONFIG_FILE=""
KUBE_CONTEXT=""
OPERATION=""
ARTIFACT_OUTPUT=""
OVERRIDES=""
NAMESPACE="witself"
DEPLOYMENT="witself-server"
TIMEOUT_SECONDS=7200

while [ "$#" -gt 0 ]; do
  case "$1" in
    --cell) [ "$#" -ge 2 ] || die "$1 requires a value"; CELL="$2"; shift 2 ;;
    --kubeconfig) [ "$#" -ge 2 ] || die "$1 requires a value"; KUBECONFIG_FILE="$2"; shift 2 ;;
    --context) [ "$#" -ge 2 ] || die "$1 requires a value"; KUBE_CONTEXT="$2"; shift 2 ;;
    --operation) [ "$#" -ge 2 ] || die "$1 requires a value"; OPERATION="$2"; shift 2 ;;
    --artifact-output) [ "$#" -ge 2 ] || die "$1 requires a value"; ARTIFACT_OUTPUT="$2"; shift 2 ;;
    --overrides) [ "$#" -ge 2 ] || die "$1 requires a value"; OVERRIDES="$2"; shift 2 ;;
    --namespace) [ "$#" -ge 2 ] || die "$1 requires a value"; NAMESPACE="$2"; shift 2 ;;
    --deployment) [ "$#" -ge 2 ] || die "$1 requires a value"; DEPLOYMENT="$2"; shift 2 ;;
    --timeout-seconds) [ "$#" -ge 2 ] || die "$1 requires a value"; TIMEOUT_SECONDS="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown or incomplete argument: $1" ;;
  esac
done

[ -n "$CELL" ] || { usage >&2; die "--cell is required"; }
[ -n "$KUBECONFIG_FILE" ] || { usage >&2; die "--kubeconfig is required"; }
[ -n "$KUBE_CONTEXT" ] || { usage >&2; die "--context is required"; }
[ -n "$OPERATION" ] || { usage >&2; die "--operation is required"; }
[ -n "$ARTIFACT_OUTPUT" ] || { usage >&2; die "--artifact-output is required"; }
case "$OPERATION" in
  backfill) ;;
  canary-manifest)
    [ -z "$OVERRIDES" ] || die "--overrides is valid only with backfill"
    ;;
  *) die "--operation must be backfill or canary-manifest" ;;
esac
[[ "$CELL" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "--cell is invalid"
[[ "$NAMESPACE" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "--namespace is invalid"
[[ "$DEPLOYMENT" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "--deployment is invalid"
[[ "$KUBE_CONTEXT" =~ ^[A-Za-z0-9._:@/-]+$ ]] || die "--context contains unsupported characters"
[[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || die "--timeout-seconds must be an integer"
(( TIMEOUT_SECONDS >= 60 && TIMEOUT_SECONDS <= 10800 )) ||
  die "--timeout-seconds must be between 60 and 10800"
CLEANUP_TIMEOUT_SECONDS="${WITSELF_AGENT_EMAIL_OPERATION_CLEANUP_TIMEOUT_SECONDS:-30}"
[[ "$CLEANUP_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] ||
  die "WITSELF_AGENT_EMAIL_OPERATION_CLEANUP_TIMEOUT_SECONDS must be an integer"
(( CLEANUP_TIMEOUT_SECONDS >= 1 && CLEANUP_TIMEOUT_SECONDS <= 120 )) ||
  die "WITSELF_AGENT_EMAIL_OPERATION_CLEANUP_TIMEOUT_SECONDS must be between 1 and 120"

for command_name in cmp jq kubectl stat; do
  require_command "$command_name"
done
require_private_file "kubeconfig" "$KUBECONFIG_FILE"
if [ -n "$OVERRIDES" ]; then
  require_private_file "override manifest" "$OVERRIDES" true
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
case "$ARTIFACT_OUTPUT" in
  /*) ;;
  *) die "--artifact-output must be an absolute path" ;;
esac
OUTPUT_PARENT_INPUT="$(dirname "$ARTIFACT_OUTPUT")"
OUTPUT_BASENAME="$(basename "$ARTIFACT_OUTPUT")"
[ "$OUTPUT_BASENAME" != . ] && [ "$OUTPUT_BASENAME" != .. ] ||
  die "--artifact-output is invalid"
[[ "$OUTPUT_BASENAME" == *.json ]] || die "--artifact-output must end in .json"
[ -d "$OUTPUT_PARENT_INPUT" ] || die "artifact output parent must be an existing directory"
[ ! -L "$OUTPUT_PARENT_INPUT" ] || die "artifact output parent must not be a symbolic link"
OUTPUT_PARENT="$(cd "$OUTPUT_PARENT_INPUT" && pwd -P)"
[ "$ARTIFACT_OUTPUT" = "$OUTPUT_PARENT/$OUTPUT_BASENAME" ] ||
  die "--artifact-output must be one canonical absolute path"
[ ! -e "$ARTIFACT_OUTPUT" ] && [ ! -L "$ARTIFACT_OUTPUT" ] ||
  die "--artifact-output must not already exist"
OUTPUT_PARENT_MODE="$(file_mode "$OUTPUT_PARENT")"
[[ "$OUTPUT_PARENT_MODE" =~ ^[0-7]{3,4}$ ]] ||
  die "could not determine artifact output parent permissions"
if (( (8#$OUTPUT_PARENT_MODE & 8#077) != 0 )); then
  die "artifact output parent must not be accessible by group or other users"
fi
case "$OUTPUT_PARENT" in
  "$REPO_ROOT"|"$REPO_ROOT"/*) die "private artifacts must be stored outside the source checkout" ;;
esac
if [ -n "$OVERRIDES" ]; then
  OVERRIDES_PARENT="$(cd "$(dirname "$OVERRIDES")" && pwd -P)"
  case "$OVERRIDES_PARENT" in
    "$REPO_ROOT"|"$REPO_ROOT"/*) die "override manifests must be stored outside the source checkout" ;;
  esac
fi

umask 077
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/witself-agent-email-cell-operation.XXXXXX")"
chmod 700 "$WORK_DIR"
LOCAL_PART=""
LOCK_CREATED=false
SECRET_CREATED=false
JOB_CREATED=false
POD_NAME=""
HOLDER_COMPLETED=false
JOB_NAME="witself-agent-email-operation"
LOCK_NAME="witself-agent-email-operation-lock"
OVERRIDE_SECRET_NAME="witself-agent-email-operation-overrides"

KUBE=(kubectl --request-timeout=30s --kubeconfig "$KUBECONFIG_FILE" --context "$KUBE_CONTEXT" -n "$NAMESPACE")

operation_pods_are_absent() {
  local absence_deadline=$((SECONDS + CLEANUP_TIMEOUT_SECONDS))
  local pods_file="$WORK_DIR/cleanup-pods.json"
  while true; do
    if "${KUBE[@]}" get pods \
        -l "batch.kubernetes.io/job-name=$JOB_NAME" -o json \
        >"$pods_file" 2>/dev/null &&
       [ "$(jq -r '.items | length' "$pods_file" 2>/dev/null)" = 0 ]; then
      return 0
    fi
    (( SECONDS < absence_deadline )) || return 1
    sleep 1
  done
}

cleanup() {
  local status=$?
  local safe_to_unlock=false
  trap - EXIT INT TERM
  if [ "$JOB_CREATED" = true ] && [ -n "$POD_NAME" ] && [ "$HOLDER_COMPLETED" != true ]; then
    "${KUBE[@]}" exec "$POD_NAME" -c artifact-export -- \
      /usr/local/bin/witself-server agent-email artifact-helper complete \
      >/dev/null 2>&1 || true
  fi
  if [ "$LOCK_CREATED" = true ]; then
    if "${KUBE[@]}" delete job "$JOB_NAME" --ignore-not-found=true \
        --cascade=foreground --wait=true --timeout="${CLEANUP_TIMEOUT_SECONDS}s" \
        >/dev/null 2>&1 && operation_pods_are_absent; then
      safe_to_unlock=true
    fi
  fi
  if [ "$safe_to_unlock" = true ] && [ "$SECRET_CREATED" = true ]; then
    if ! "${KUBE[@]}" delete secret "$OVERRIDE_SECRET_NAME" \
        --ignore-not-found=true --wait=true --timeout="${CLEANUP_TIMEOUT_SECONDS}s" \
        >/dev/null 2>&1; then
      safe_to_unlock=false
    fi
  fi
  if [ "$safe_to_unlock" = true ] && [ "$LOCK_CREATED" = true ]; then
    if ! "${KUBE[@]}" delete configmap "$LOCK_NAME" \
        --ignore-not-found=true --wait=true --timeout="${CLEANUP_TIMEOUT_SECONDS}s" \
        >/dev/null 2>&1; then
      safe_to_unlock=false
    fi
  fi
  if [ "$LOCK_CREATED" = true ] && [ "$safe_to_unlock" != true ]; then
    printf '%s\n' \
      'warning: operation cleanup could not prove the runner absent; the fixed lock was retained' >&2
  fi
  if [ -n "$LOCAL_PART" ]; then
    rm -f "$LOCAL_PART"
  fi
  find "$WORK_DIR" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$WORK_DIR" 2>/dev/null || true
  exit "$status"
}
trap cleanup EXIT INT TERM

publish_private_artifact() {
  local staged_artifact="$1"
  [ -f "$staged_artifact" ] || die "validated private artifact staging is unavailable"
  [ ! -e "$ARTIFACT_OUTPUT" ] && [ ! -L "$ARTIFACT_OUTPUT" ] ||
    die "private artifact output appeared before publication"
  LOCAL_PART="$(mktemp "$OUTPUT_PARENT/.witself-agent-email-artifact.XXXXXX")"
  chmod 600 "$LOCAL_PART"
  cp "$staged_artifact" "$LOCAL_PART"
  chmod 600 "$LOCAL_PART"
  if ! ln "$LOCAL_PART" "$ARTIFACT_OUTPUT" 2>/dev/null; then
    die "could not publish the private artifact without overwriting a file"
  fi
  rm -f "$LOCAL_PART"
  LOCAL_PART=""
}

complete_artifact_holder() {
  local attempts=0
  while true; do
    if "${KUBE[@]}" exec "$POD_NAME" -c artifact-export -- \
        /usr/local/bin/witself-server agent-email artifact-helper complete \
        >/dev/null 2>"$WORK_DIR/artifact-complete.err"; then
      HOLDER_COMPLETED=true
      return 0
    fi
    attempts=$((attempts + 1))
    (( attempts < 3 && SECONDS < DEADLINE )) || return 1
    sleep 1
  done
}

read_bounded_runner_failure_reason() {
  local attempts=0
  local reason="unavailable"
  local runner_log="$WORK_DIR/runner-failure.log"

  while true; do
    if "${KUBE[@]}" logs "$POD_NAME" -c runner --tail=20 --limit-bytes=8192 \
        >"$runner_log" 2>"$WORK_DIR/runner-failure-log.err"; then
      break
    fi
    attempts=$((attempts + 1))
    if (( attempts >= 3 || SECONDS >= DEADLINE )); then
      printf '%s\n' "$reason"
      return 0
    fi
    sleep 1
  done

  # Pod logs remain private. Only one exact log-safe reason from the server's
  # closed vocabulary may cross the operator boundary; missing, malformed, or
  # conflicting reasons collapse to one bounded fallback.
  if ! reason="$(jq -Rrs '
    [
      split("\n")[] |
      (try capture(
        "^witself-server: agent-email production (backfill|canary) [a-z][a-z -]* failed \\(reason=(?<reason>[a-z_]+)\\)$"
      ).reason catch empty) |
      select(
        . == "canceled" or
        . == "deadline_exceeded" or
        . == "receive_disabled" or
        . == "invalid_configuration" or
        . == "cohort_not_ready" or
        . == "account_not_found" or
        . == "account_not_active" or
        . == "mailbox_missing" or
        . == "conflict" or
        . == "invalid_override_manifest" or
        . == "invalid_exception_output" or
        . == "database_unavailable" or
        . == "migration_failed" or
        . == "preflight_failed" or
        . == "reconciliation_failed" or
        . == "verification_failed" or
        . == "result_encoding_failed" or
        . == "canary_snapshot_failed"
      )
    ] | unique |
    if length == 1 then .[0] else "unavailable" end
  ' "$runner_log" 2>/dev/null)"; then
    reason="unavailable"
  fi
  case "$reason" in
    canceled|deadline_exceeded|receive_disabled|invalid_configuration|cohort_not_ready|\
      account_not_found|account_not_active|mailbox_missing|conflict|\
      invalid_override_manifest|invalid_exception_output|database_unavailable|\
      migration_failed|preflight_failed|reconciliation_failed|verification_failed|\
      result_encoding_failed|canary_snapshot_failed|unavailable)
      printf '%s\n' "$reason"
      ;;
    *)
      printf '%s\n' unavailable
      ;;
  esac
}

DEPLOYMENT_JSON="$WORK_DIR/deployment.json"
CONFIG_JSON="$WORK_DIR/config.json"
if ! "${KUBE[@]}" get deployment "$DEPLOYMENT" -o json >"$DEPLOYMENT_JSON" 2>/dev/null; then
  die "could not read the managed server Deployment"
fi
if ! validate_source_deployment "$DEPLOYMENT_JSON" "$DEPLOYMENT"; then
  die "managed server Deployment is absent, ambiguous, or not fully converged"
fi

SERVER_IMAGE="$(jq -er '.spec.template.spec.containers[] | select(.name == "witself-server") | .image' "$DEPLOYMENT_JSON")" ||
  die "could not resolve the managed server image"
case "$SERVER_IMAGE" in
  ghcr.io/witwave-ai/images/witself-server:[0-9]*.[0-9]*.[0-9]*) ;;
  *) die "managed operation requires an exact released witself-server image tag" ;;
esac
SERVER_VERSION="${SERVER_IMAGE##*:}"
[[ "$SERVER_VERSION" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
  die "managed operation requires a semantic released image tag"
if (( 10#${BASH_REMATCH[1]} == 0 && 10#${BASH_REMATCH[2]} == 0 && 10#${BASH_REMATCH[3]} < 241 )); then
  die "managed agent-email operations require image v0.0.241 or newer"
fi

CONFIG_NAME="$(extract_source_config_name "$DEPLOYMENT_JSON" 2>/dev/null)" ||
  die "could not resolve the managed server ConfigMap"
SERVER_CONFIG_CHECKSUM="$(jq -er '.spec.template.metadata.annotations["witself.io/server-config-checksum"]' "$DEPLOYMENT_JSON")" ||
  die "could not resolve the managed server configuration checksum"
if ! "${KUBE[@]}" get configmap "$CONFIG_NAME" -o json >"$CONFIG_JSON" 2>/dev/null; then
  die "could not read the managed server ConfigMap"
fi
if ! validate_source_config "$CONFIG_JSON" "$CONFIG_NAME" "$CELL" "$SERVER_CONFIG_CHECKSUM"; then
  die "managed cell identity or secret-backed production receive configuration is not ready"
fi

PRIVATE_ENV_FILE="$WORK_DIR/private-env.json"
if ! write_private_env_refs "$DEPLOYMENT_JSON" "$PRIVATE_ENV_FILE" 2>/dev/null; then
  die "managed server must expose exact database and receive-cohort Secret references"
fi
COHORT_SECRET_NAME="$(jq -er '.[] | select(.name == "WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS") | .valueFrom.secretKeyRef.name' "$PRIVATE_ENV_FILE")" ||
  die "could not resolve the receive-cohort Secret reference"
DATABASE_SECRET_NAME="$(jq -er '.[] | select(.name == "WITSELF_DATABASE_URL") | .valueFrom.secretKeyRef.name' "$PRIVATE_ENV_FILE")" ||
  die "could not resolve the database Secret reference"
COHORT_SECRET_FENCE="$WORK_DIR/cohort-secret.fence"
if ! "${KUBE[@]}" get secret "$COHORT_SECRET_NAME" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}{.immutable}{"\n"}' \
    >"$COHORT_SECRET_FENCE" 2>/dev/null; then
  die "could not verify the receive-cohort Secret metadata"
fi
if [ "$(sed -n '3p' "$COHORT_SECRET_FENCE")" != true ] ||
   [ -z "$(sed -n '1p' "$COHORT_SECRET_FENCE")" ] ||
   [ -z "$(sed -n '2p' "$COHORT_SECRET_FENCE")" ]; then
  die "receive-cohort Secret must be live and immutable"
fi
DATABASE_SECRET_FENCE="$WORK_DIR/database-secret.fence"
if ! "${KUBE[@]}" get secret "$DATABASE_SECRET_NAME" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}' \
    >"$DATABASE_SECRET_FENCE" 2>/dev/null; then
  die "could not verify the database Secret metadata"
fi
if [ -z "$(sed -n '1p' "$DATABASE_SECRET_FENCE")" ] ||
   [ -z "$(sed -n '2p' "$DATABASE_SECRET_FENCE")" ]; then
  die "database Secret metadata is incomplete"
fi

DEPLOYMENT_FENCE="$WORK_DIR/deployment.fence.json"
CONFIG_FENCE="$WORK_DIR/config.fence.json"
write_deployment_fence "$DEPLOYMENT_JSON" "$PRIVATE_ENV_FILE" "$DEPLOYMENT_FENCE"
write_config_fence "$CONFIG_JSON" "$CONFIG_FENCE"

# The immutable non-secret snapshot doubles as the namespace-wide operation
# lock. Its fixed name makes concurrent invocations race safely at create.
if ! jq -n --arg name "$LOCK_NAME" --arg operation "$OPERATION" \
    --arg checksum "$SERVER_CONFIG_CHECKSUM" \
    --slurpfile source "$CONFIG_JSON" '
  {
    apiVersion: "v1", kind: "ConfigMap",
    metadata: {
      name: $name,
      annotations: {"witself.io/source-config-checksum": $checksum},
      labels: {
        "app.kubernetes.io/name": "witself-agent-email-operation",
        "app.kubernetes.io/component": "one-shot",
        "witself.io/agent-email-operation": $operation
      }
    },
    immutable: true,
    data: ($source[0].data // {})
  }
' | "${KUBE[@]}" create -f - >/dev/null 2>"$WORK_DIR/create-lock.err"; then
  die "another agent-email cell operation is active or requires cleanup"
fi
LOCK_CREATED=true

if [ -n "$OVERRIDES" ]; then
  if ! "${KUBE[@]}" create secret generic "$OVERRIDE_SECRET_NAME" \
      --from-file="overrides.json=$OVERRIDES" --dry-run=client -o json 2>/dev/null |
    jq --arg operation "$OPERATION" '
      .immutable = true |
      .metadata.labels = {
        "app.kubernetes.io/name": "witself-agent-email-operation",
        "app.kubernetes.io/component": "one-shot",
        "witself.io/agent-email-operation": $operation
      }
    ' | "${KUBE[@]}" create -f - >/dev/null 2>"$WORK_DIR/create-secret.err"; then
    die "could not stage the private override Secret"
  fi
  SECRET_CREATED=true
fi

if [ "$OPERATION" = backfill ]; then
  RUNNER_ARGS='["agent-email","backfill","--exception-output","/private/backfill-exception.json"]'
  ARTIFACT_KEY="backfill-exception"
  ARTIFACT_FILENAME="backfill-exception.json"
  if [ -n "$OVERRIDES" ]; then
    RUNNER_ARGS='["agent-email","backfill","--exception-output","/private/backfill-exception.json","--overrides","/private/overrides.json"]'
  fi
else
  RUNNER_ARGS='["agent-email","canary-manifest","--output","/private/primary-canary.json"]'
  ARTIFACT_KEY="primary-canary"
  ARTIFACT_FILENAME="primary-canary.json"
fi

# Re-read every live source immediately before Job creation. The Job uses the
# initial immutable ConfigMap snapshot and exact Secret refs only if the active
# Deployment, ConfigMap, cohort Secret metadata, rollout status, image, and
# checksums are still byte-for-byte the same coherent source.
CURRENT_DEPLOYMENT_JSON="$WORK_DIR/deployment-current.json"
CURRENT_CONFIG_JSON="$WORK_DIR/config-current.json"
CURRENT_PRIVATE_ENV_FILE="$WORK_DIR/private-env-current.json"
CURRENT_DEPLOYMENT_FENCE="$WORK_DIR/deployment-current.fence.json"
CURRENT_CONFIG_FENCE="$WORK_DIR/config-current.fence.json"
CURRENT_COHORT_SECRET_FENCE="$WORK_DIR/cohort-secret-current.fence"
CURRENT_DATABASE_SECRET_FENCE="$WORK_DIR/database-secret-current.fence"
if ! "${KUBE[@]}" get deployment "$DEPLOYMENT" -o json >"$CURRENT_DEPLOYMENT_JSON" 2>/dev/null ||
   ! validate_source_deployment "$CURRENT_DEPLOYMENT_JSON" "$DEPLOYMENT"; then
  die "managed server Deployment changed or lost readiness before Job creation"
fi
CURRENT_CONFIG_NAME="$(extract_source_config_name "$CURRENT_DEPLOYMENT_JSON" 2>/dev/null)" ||
  die "managed server ConfigMap reference changed before Job creation"
[ "$CURRENT_CONFIG_NAME" = "$CONFIG_NAME" ] ||
  die "managed server source changed before Job creation"
CURRENT_SERVER_CONFIG_CHECKSUM="$(jq -er '.spec.template.metadata.annotations["witself.io/server-config-checksum"]' "$CURRENT_DEPLOYMENT_JSON")" ||
  die "managed server configuration checksum changed before Job creation"
if ! write_private_env_refs "$CURRENT_DEPLOYMENT_JSON" "$CURRENT_PRIVATE_ENV_FILE" 2>/dev/null; then
  die "managed server Secret references changed before Job creation"
fi
if ! "${KUBE[@]}" get configmap "$CURRENT_CONFIG_NAME" -o json >"$CURRENT_CONFIG_JSON" 2>/dev/null ||
   ! validate_source_config "$CURRENT_CONFIG_JSON" "$CURRENT_CONFIG_NAME" "$CELL" "$CURRENT_SERVER_CONFIG_CHECKSUM"; then
  die "managed server ConfigMap changed or became unready before Job creation"
fi
if ! "${KUBE[@]}" get secret "$COHORT_SECRET_NAME" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}{.immutable}{"\n"}' \
    >"$CURRENT_COHORT_SECRET_FENCE" 2>/dev/null; then
  die "receive-cohort Secret changed before Job creation"
fi
if ! "${KUBE[@]}" get secret "$DATABASE_SECRET_NAME" \
    -o 'jsonpath={.metadata.uid}{"\n"}{.metadata.resourceVersion}{"\n"}' \
    >"$CURRENT_DATABASE_SECRET_FENCE" 2>/dev/null; then
  die "database Secret changed before Job creation"
fi
write_deployment_fence "$CURRENT_DEPLOYMENT_JSON" "$CURRENT_PRIVATE_ENV_FILE" "$CURRENT_DEPLOYMENT_FENCE"
write_config_fence "$CURRENT_CONFIG_JSON" "$CURRENT_CONFIG_FENCE"
if ! cmp -s "$DEPLOYMENT_FENCE" "$CURRENT_DEPLOYMENT_FENCE" ||
   ! cmp -s "$CONFIG_FENCE" "$CURRENT_CONFIG_FENCE" ||
   ! cmp -s "$COHORT_SECRET_FENCE" "$CURRENT_COHORT_SECRET_FENCE" ||
   ! cmp -s "$DATABASE_SECRET_FENCE" "$CURRENT_DATABASE_SECRET_FENCE"; then
  die "managed server source drifted before Job creation"
fi

if ! jq -n \
    --arg name "$JOB_NAME" \
    --arg operation "$OPERATION" \
    --arg image "$SERVER_IMAGE" \
    --arg config "$LOCK_NAME" \
    --arg source_checksum "$SERVER_CONFIG_CHECKSUM" \
    --arg override_secret "$OVERRIDE_SECRET_NAME" \
    --argjson timeout "$TIMEOUT_SECONDS" \
    --argjson runner_args "$RUNNER_ARGS" \
    --argjson with_overrides "$([ -n "$OVERRIDES" ] && printf true || printf false)" \
    --slurpfile private_env "$PRIVATE_ENV_FILE" \
    --slurpfile deployment "$DEPLOYMENT_JSON" '
  ($deployment[0]) as $source |
  ($source.spec.template.spec.containers[] | select(.name == "witself-server")) as $server |
  {
    apiVersion: "batch/v1", kind: "Job",
    metadata: {
      name: $name,
      annotations: {"witself.io/source-config-checksum": $source_checksum},
      labels: {
        "app.kubernetes.io/name": "witself-agent-email-operation",
        "app.kubernetes.io/component": "one-shot",
        "witself.io/agent-email-operation": $operation
      }
    },
    spec: {
      backoffLimit: 0,
      activeDeadlineSeconds: $timeout,
      ttlSecondsAfterFinished: 3600,
      template: {
        metadata: {
          annotations: {"witself.io/source-config-checksum": $source_checksum},
          labels: {
            "app.kubernetes.io/name": "witself-agent-email-operation",
            "app.kubernetes.io/component": "one-shot",
            "witself.io/agent-email-operation": $operation
          }
        },
        spec: {
          restartPolicy: "Never",
          automountServiceAccountToken: false,
          serviceAccountName: $source.spec.template.spec.serviceAccountName,
          imagePullSecrets: ($source.spec.template.spec.imagePullSecrets // []),
          nodeSelector: ($source.spec.template.spec.nodeSelector // {}),
          tolerations: ($source.spec.template.spec.tolerations // []),
          affinity: ($source.spec.template.spec.affinity // {}),
          securityContext: {
            runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532,
            fsGroup: 65532, fsGroupChangePolicy: "OnRootMismatch",
            seccompProfile: {type: "RuntimeDefault"}
          },
          initContainers: (if $with_overrides then [{
            name: "override-stage", image: $image,
            imagePullPolicy: ($server.imagePullPolicy // "IfNotPresent"),
            command: ["/usr/local/bin/witself-server"],
            args: ["agent-email", "artifact-helper", "stage-overrides"],
            securityContext: {
              allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
              runAsNonRoot: true, capabilities: {drop: ["ALL"]}
            },
            resources: {requests: {cpu: "5m", memory: "8Mi"}, limits: {memory: "32Mi"}},
            volumeMounts: [
              {name: "private", mountPath: "/private"},
              {name: "overrides", mountPath: "/overrides", readOnly: true}
            ]
          }] else [] end),
          containers: [
            {
              name: "runner", image: $image,
              imagePullPolicy: ($server.imagePullPolicy // "IfNotPresent"),
              command: ["/usr/local/bin/witself-server"], args: $runner_args,
              envFrom: [{configMapRef: {name: $config}}], env: $private_env[0],
              securityContext: {
                allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                runAsNonRoot: true, capabilities: {drop: ["ALL"]}
              },
              resources: ($server.resources // {}),
              volumeMounts: [{name: "private", mountPath: "/private"}]
            },
            {
              name: "artifact-export", image: $image,
              imagePullPolicy: ($server.imagePullPolicy // "IfNotPresent"),
              command: ["/usr/local/bin/witself-server"],
              args: ["agent-email", "artifact-helper", "hold"],
              securityContext: {
                allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                runAsNonRoot: true, capabilities: {drop: ["ALL"]}
              },
              resources: {requests: {cpu: "5m", memory: "8Mi"}, limits: {memory: "32Mi"}},
              volumeMounts: [{name: "private", mountPath: "/private"}]
            }
          ],
          volumes: ([{name: "private", emptyDir: {medium: "Memory", sizeLimit: "1Mi"}}] +
            (if $with_overrides then [{name: "overrides", secret: {
              secretName: $override_secret, defaultMode: 288
            }}] else [] end))
        }
      }
    }
  }
' | "${KUBE[@]}" create -f - >/dev/null 2>"$WORK_DIR/create-job.err"; then
  die "could not create the isolated agent-email operation Job"
fi
JOB_CREATED=true

DEADLINE=$((SECONDS + TIMEOUT_SECONDS))
RUNNER_EXIT=""
while [ -z "$RUNNER_EXIT" ]; do
  PODS_JSON="$WORK_DIR/pods.json"
  if "${KUBE[@]}" get pods -l "job-name=$JOB_NAME" -o json >"$PODS_JSON" 2>/dev/null; then
    POD_COUNT="$(jq -r '.items | length' "$PODS_JSON")"
    if [ "$POD_COUNT" -gt 1 ]; then
      die "operation Job created more than one pod"
    fi
    if [ "$POD_COUNT" -eq 1 ]; then
      POD_NAME="$(jq -er '.items[0].metadata.name' "$PODS_JSON")" ||
        die "operation pod name is unavailable"
      INIT_FAILURE="$(jq -r '[.items[0].status.initContainerStatuses[]? |
        select(.state.terminated.exitCode != null and .state.terminated.exitCode != 0)] | length' "$PODS_JSON")"
      if [ "$INIT_FAILURE" -gt 0 ]; then
        die "private override staging failed"
      fi
      RUNNER_EXIT="$(jq -r '[.items[0].status.containerStatuses[]? |
        select(.name == "runner") | .state.terminated.exitCode][0] // empty' "$PODS_JSON")"
    fi
  fi
  if [ -z "$RUNNER_EXIT" ]; then
    (( SECONDS < DEADLINE )) || die "agent-email operation timed out"
    sleep 1
  fi
done

RUNNER_FAILURE_REASON=""
if [ "$RUNNER_EXIT" -ne 0 ]; then
  RUNNER_FAILURE_REASON="$(read_bounded_runner_failure_reason)"
fi

# The holder may still be starting after a very fast runner. Wait only until it
# can accept an exec; its stdout is never attached to pod logs.
while true; do
  if "${KUBE[@]}" get pod "$POD_NAME" -o json >"$WORK_DIR/pod.json" 2>/dev/null; then
    HOLDER_STATE="$(jq -r '[.status.containerStatuses[]? | select(.name == "artifact-export") |
      if .state.running then "running" elif .state.terminated then "terminated" else "waiting" end][0] // "waiting"' "$WORK_DIR/pod.json")"
    [ "$HOLDER_STATE" != terminated ] || die "private artifact holder stopped before export"
    [ "$HOLDER_STATE" != running ] || break
  fi
  (( SECONDS < DEADLINE )) || die "private artifact holder did not become ready"
  sleep 1
done

EXEC_READY_DEADLINE=$((SECONDS + 30))
while ! "${KUBE[@]}" exec "$POD_NAME" -c artifact-export -- \
    /usr/local/bin/witself-server agent-email artifact-helper ready \
    >/dev/null 2>"$WORK_DIR/artifact-ready.err"; do
  (( SECONDS < EXEC_READY_DEADLINE && SECONDS < DEADLINE )) ||
    die "private artifact holder did not accept exec before its deadline"
  sleep 1
done

ARTIFACT_EXISTS=false
if "${KUBE[@]}" exec "$POD_NAME" -c artifact-export -- \
    /usr/local/bin/witself-server agent-email artifact-helper exists --name "$ARTIFACT_KEY" \
    >/dev/null 2>"$WORK_DIR/artifact-exists.err"; then
  ARTIFACT_EXISTS=true
else
  ARTIFACT_STATUS=$?
  if [ "$ARTIFACT_STATUS" -ne 3 ]; then
    die "private artifact inspection failed"
  fi
fi

if [ "$OPERATION" = canary-manifest ] && [ "$RUNNER_EXIT" -eq 0 ] && [ "$ARTIFACT_EXISTS" != true ]; then
  die "canary operation completed without its private artifact"
fi
if [ "$OPERATION" = backfill ] && [ "$RUNNER_EXIT" -eq 0 ] && [ "$ARTIFACT_EXISTS" = true ]; then
  die "successful backfill unexpectedly created an exception artifact"
fi

if [ "$ARTIFACT_EXISTS" = true ]; then
  EXPORTED_ARTIFACT="$WORK_DIR/$ARTIFACT_FILENAME"
  if ! "${KUBE[@]}" exec "$POD_NAME" -c artifact-export -- \
      /usr/local/bin/witself-server agent-email artifact-helper export --name "$ARTIFACT_KEY" \
      >"$EXPORTED_ARTIFACT" 2>"$WORK_DIR/artifact-export.err"; then
    die "private artifact export failed"
  fi
  chmod 600 "$EXPORTED_ARTIFACT"
  if [ "$OPERATION" = canary-manifest ]; then
    if ! jq -e '
      (keys | sort == ["account_ids","agents","domain","schema_version","worker_name"]) and
      .schema_version == 2 and .domain == "witmail.net" and
      .worker_name == "witself-agent-email-pilot" and
      (.account_ids | type == "array" and length >= 1 and length <= 100 and
        all(.[]; type == "string" and test("^acc_[a-z2-7]{16}$"))) and
      (.agents | type == "array" and length >= 5 and length <= 10 and
        all(.[]; (keys | sort == ["address","agent_id","realm_id"]) and
                  (.agent_id | type == "string" and test("^agent_[a-z2-7]{16}$")) and
                  (.realm_id | type == "string" and test("^realm_[a-z2-7]{16}$")) and
                  (.address | type == "string" and test("^[^@[:space:]]+@witmail\\.net$")))) and
      (.account_ids == (.account_ids | sort)) and
      ((.account_ids | unique | length) == (.account_ids | length)) and
      ([.agents[].address] == ([.agents[].address] | sort)) and
      (([.agents[].address] | unique | length) == (.agents | length)) and
      (([.agents[].agent_id] | unique | length) == (.agents | length)) and
      (([.agents[] | [.agent_id,.realm_id,.address]] | unique | length) == (.agents | length))
    ' "$EXPORTED_ARTIFACT" >/dev/null; then
      die "exported canary artifact failed local structural validation"
    fi
  else
    if ! jq -e '
      (keys | sort == ["agent_id","processed_agent_count","realm_id","reason_code","schema_version","state"]) and
      .schema_version == 1 and .state == "requires_operator_override" and
      (.processed_agent_count | type == "number" and . >= 0 and floor == .) and
      (.agent_id | type == "string" and test("^agent_[a-z2-7]{16}$")) and
      (.realm_id | type == "string" and test("^realm_[a-z2-7]{16}$")) and
      (.reason_code == "agent_segment_requires_override" or
       .reason_code == "address_collision_requires_override")
    ' "$EXPORTED_ARTIFACT" >/dev/null; then
      die "exported backfill exception failed local structural validation"
    fi
  fi
fi

if ! complete_artifact_holder; then
  die "could not complete the private artifact holder"
fi

if [ "$RUNNER_EXIT" -ne 0 ]; then
  if [ "$OPERATION" = backfill ] && [ "$ARTIFACT_EXISTS" = true ]; then
    publish_private_artifact "$EXPORTED_ARTIFACT"
    die "backfill status requires_operator_override; the private exception artifact was exported"
  fi
  die "agent-email operation failed (reason=$RUNNER_FAILURE_REASON) without an exportable private artifact"
fi

if [ "$OPERATION" = canary-manifest ]; then
  publish_private_artifact "$EXPORTED_ARTIFACT"
fi

if [ "$OPERATION" = backfill ]; then
  RUNNER_LOG="$WORK_DIR/runner.log"
  if "${KUBE[@]}" logs "$POD_NAME" -c runner >"$RUNNER_LOG" 2>/dev/null &&
    jq -e '
      type == "object" and
      (keys | sort == ["account_count","live_agent_count","missing_mailbox_count_after",
        "missing_mailbox_count_before","override_count","processed_agent_count",
        "ready_mailbox_count","retry_canary_ready"]) and
      all(.account_count,.live_agent_count,.missing_mailbox_count_after,
          .missing_mailbox_count_before,.override_count,.processed_agent_count,
          .ready_mailbox_count; type == "number" and . >= 0 and floor == .) and
      (.retry_canary_ready | type == "boolean")
    ' "$RUNNER_LOG" >/dev/null; then
    jq -c . "$RUNNER_LOG"
  else
    printf '{"status":"completed","counts":"unavailable"}\n'
  fi
else
  printf '{"status":"completed","private_artifact_exported":true}\n'
fi
