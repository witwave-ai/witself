#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
server_chart="$repo_root/charts/witself-server"
apps_chart="$repo_root/.gitops/charts/apps"
apps_profile="$apps_chart/ci/gcp-rollout-values.yaml"
gcp_profile="$server_chart/ci/gcp-rollout-values.yaml"
gcp_cell="$repo_root/.gitops/cells/gcp-sandbox-use1-dev/values.yaml"

default_render="$(mktemp)"
gcp_render="$(mktemp)"
apps_render="$(mktemp)"
phase_b_gcp_render="$(mktemp)"
phase_b_apps_render="$(mktemp)"
trap 'rm -f "$default_render" "$gcp_render" "$apps_render" "$phase_b_gcp_render" "$phase_b_apps_render"' EXIT

helm template witself-server "$server_chart" --namespace witself >"$default_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" >"$gcp_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=false >"$apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set avatar.payloadCompaction.enabled=true >"$phase_b_gcp_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=true >"$phase_b_apps_render"

require_line() {
  local expected="$1"
  local file="$2"
  if ! grep -Fqx "$expected" "$file"; then
    echo "missing rendered line: $expected" >&2
    return 1
  fi
}

require_sequence() {
  local file="$1"
  shift
  local -a expected=("$@")
  local matched=0
  local line
  while IFS= read -r line; do
    if [[ "$line" == "${expected[$matched]}" ]]; then
      matched=$((matched + 1))
      if ((matched == ${#expected[@]})); then
        return 0
      fi
    elif [[ "$line" == "${expected[0]}" ]]; then
      matched=1
    else
      matched=0
    fi
  done <"$file"
  echo "missing rendered sequence starting with: ${expected[0]}" >&2
  return 1
}

config_checksum() {
  awk '$1 == "checksum/config:" { print $2; exit }' "$1"
}

# Public defaults keep the old pod available while the replacement becomes
# ready, but do not opt portable self-hosted installs into a version-gated
# native lifecycle handler.
require_line "  minReadySeconds: 10" "$default_render"
require_line "    type: RollingUpdate" "$default_render"
require_line "      maxSurge: 1" "$default_render"
require_line "      maxUnavailable: 0" "$default_render"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "false"' "$default_render"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "true"' "$default_render"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE: "100"' "$default_render"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL: "2s"' "$default_render"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT: "30s"' "$default_render"
if grep -Fqx "          lifecycle:" "$default_render"; then
  echo "default render unexpectedly contains a container lifecycle handler" >&2
  exit 1
fi

# The managed GKE profile renders the drain window, disruption budget, and
# hard zonal spread on the actual Kubernetes workload.
require_line "  replicas: 2" "$gcp_render"
require_line "      terminationGracePeriodSeconds: 210" "$gcp_render"
require_line "          lifecycle:" "$gcp_render"
require_line "                seconds: 120" "$gcp_render"
require_line "  minAvailable: 1" "$gcp_render"
require_line "          minDomains: 2" "$gcp_render"
require_line "          topologyKey: topology.kubernetes.io/zone" "$gcp_render"
require_line "          whenUnsatisfiable: DoNotSchedule" "$gcp_render"

# Schema validation must reject a negative preStop sleep instead of silently
# emitting an invalid or surprising lifecycle.
if helm template witself-server "$server_chart" --namespace witself \
  --set lifecycle.preStopSleepSeconds=-1 >/dev/null 2>&1; then
  echo "negative lifecycle.preStopSleepSeconds unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set avatar.styleRollout.batchSize=1001 >/dev/null 2>&1; then
  echo "oversized avatar.styleRollout.batchSize unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set avatar.styleRollout.interval=99ms >/dev/null 2>&1; then
  echo "undersized avatar.styleRollout.interval unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set avatar.styleRollout.interval=2h >/dev/null 2>&1; then
  echo "oversized avatar.styleRollout.interval unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set avatar.styleRollout.batchTimeout=99ms >/dev/null 2>&1; then
  echo "undersized avatar.styleRollout.batchTimeout unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set avatar.styleRollout.batchTimeout=6m >/dev/null 2>&1; then
  echo "oversized avatar.styleRollout.batchTimeout unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set strategy.rollingUpdate.maxUnavailable=0 \
  --set strategy.rollingUpdate.maxSurge=0 >/dev/null 2>&1; then
  echo "zero maxUnavailable and maxSurge unexpectedly passed schema validation" >&2
  exit 1
fi

# The GitOps app-of-apps layer must carry every availability value into the
# nested witself-server release and configure GCLB connection draining.
require_line "    drainingTimeoutSec: 60" "$apps_render"
require_line "        minReadySeconds: 10" "$apps_render"
require_line "        strategy:" "$apps_render"
require_line "            maxSurge: 1" "$apps_render"
require_line "            maxUnavailable: 0" "$apps_render"
require_line "          type: RollingUpdate" "$apps_render"
require_line "          preStopSleepSeconds: 120" "$apps_render"
require_line "        replicaCount: 2" "$apps_render"
require_sequence "$apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: false" \
  "          styleRollout:" \
  "            batchSize: 100" \
  "            batchTimeout: 30s" \
  "            enabled: true" \
  "            interval: 2s"
require_line "        terminationGracePeriodSeconds: 210" "$apps_render"
require_line "          enabled: true" "$apps_render"
require_line "          minAvailable: 1" "$apps_render"
require_line "          minDomains: 2" "$apps_render"
require_line "          topologyKey: topology.kubernetes.io/zone" "$apps_render"
require_line "          whenUnsatisfiable: DoNotSchedule" "$apps_render"

# Phase B is deliberately a separate config-only rollout. Verify both the
# app-of-apps handoff and the nested chart's pod-restart checksum rather than
# accepting an unrelated enabled scalar elsewhere in either manifest.
require_sequence "$phase_b_apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: true" \
  "          styleRollout:" \
  "            batchSize: 100" \
  "            batchTimeout: 30s" \
  "            enabled: true" \
  "            interval: 2s"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "true"' "$phase_b_gcp_render"
phase_a_checksum="$(config_checksum "$gcp_render")"
phase_b_checksum="$(config_checksum "$phase_b_gcp_render")"
if [[ -z "$phase_a_checksum" || -z "$phase_b_checksum" || "$phase_a_checksum" == "$phase_b_checksum" ]]; then
  echo "avatar compaction phase flip did not change the pod config checksum" >&2
  exit 1
fi

echo "Helm rollout rendering checks passed"
