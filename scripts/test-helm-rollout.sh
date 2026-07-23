#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
server_chart="$repo_root/charts/witself-server"
apps_chart="$repo_root/.gitops/charts/apps"
apps_profile="$apps_chart/ci/gcp-rollout-values.yaml"
gcp_profile="$server_chart/ci/gcp-rollout-values.yaml"
email_pilot_profile="$server_chart/ci/agent-email-pilot-values.yaml"
apps_email_pilot_profile="$apps_chart/ci/agent-email-pilot-values.yaml"
gcp_cell="$repo_root/.gitops/cells/gcp-sandbox-use1-dev/values.yaml"

default_render="$(mktemp)"
gcp_render="$(mktemp)"
apps_render="$(mktemp)"
phase_b_gcp_render="$(mktemp)"
phase_b_apps_render="$(mktemp)"
email_pilot_render="$(mktemp)"
email_pilot_apps_render="$(mktemp)"
retention_preview_render="$(mktemp)"
retention_enforce_render="$(mktemp)"
retention_preview_apps_render="$(mktemp)"
retention_enforce_apps_render="$(mktemp)"
trap 'rm -f "$default_render" "$gcp_render" "$apps_render" "$phase_b_gcp_render" "$phase_b_apps_render" "$email_pilot_render" "$email_pilot_apps_render" "$retention_preview_render" "$retention_enforce_render" "$retention_preview_apps_render" "$retention_enforce_apps_render"' EXIT

helm template witself-server "$server_chart" --namespace witself >"$default_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" >"$gcp_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=false \
  --set apps.witselfServer.transcriptRetention.enabled=false >"$apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set avatar.payloadCompaction.enabled=true >"$phase_b_gcp_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=true >"$phase_b_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" >"$email_pilot_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --values "$apps_email_pilot_profile" >"$email_pilot_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --set transcriptRetention.enabled=true >"$retention_preview_render"
helm template witself-server "$server_chart" --namespace witself \
  --set transcriptRetention.enabled=true \
  --set transcriptRetention.mode=enforce >"$retention_enforce_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.transcriptRetention.enabled=true >"$retention_preview_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.transcriptRetention.enabled=true \
  --set apps.witselfServer.transcriptRetention.mode=enforce >"$retention_enforce_apps_render"

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
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$default_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$default_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_MODE: "preview"' "$default_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_BATCH_SIZE: "100"' "$default_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_INTERVAL: "5m"' "$default_render"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$default_render")" -ne 1 ]]; then
  echo "default render exposed agent-email configuration beyond the disabled gate" >&2
  exit 1
fi
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
  --set transcriptRetention.mode=delete >/dev/null 2>&1; then
  echo "unknown transcriptRetention.mode unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set transcriptRetention.batchSize=1001 >/dev/null 2>&1; then
  echo "oversized transcriptRetention.batchSize unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set transcriptRetention.interval=59s >/dev/null 2>&1; then
  echo "undersized transcriptRetention.interval unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set transcriptRetention.interval=25h >/dev/null 2>&1; then
  echo "oversized transcriptRetention.interval unexpectedly passed schema validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --set strategy.rollingUpdate.maxUnavailable=0 \
  --set strategy.rollingUpdate.maxSurge=0 >/dev/null 2>&1; then
  echo "zero maxUnavailable and maxSurge unexpectedly passed schema validation" >&2
  exit 1
fi

# The receive-only email pilot exposes its seven base server variables plus the
# optional retry-canary agent when configured, carries public verification
# material only, and fails closed outside the authorized 5-10-agent enrollment.
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "true"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_PILOT_DOMAIN: "agent-mail.witwave.ai"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AUDIENCE: "gcp-sandbox-use1-dev"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_PILOT_REALM_ID: "realm_aaaaaaaaaaaaaaaa"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS: "agent_aaaaaaaaaaaaaaaa,agent_bbbbbbbbbbbbbbbb,agent_cccccccccccccccc,agent_dddddddddddddddd,agent_eeeeeeeeeeeeeeee"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID: "agent_aaaaaaaaaaaaaaaa"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON: "{\"pilot-2026-07\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}"' "$email_pilot_render"
require_line '  WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW: "5m"' "$email_pilot_render"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$email_pilot_render")" -ne 8 ]]; then
  echo "enabled pilot with a retry canary did not render exactly eight agent-email variables" >&2
  exit 1
fi
if grep -Eq 'WITSELF_AGENT_EMAIL_.*PRIVATE|RELAY_ED25519_PRIVATE_KEY|relayPrivateKey' \
  "$email_pilot_render" "$email_pilot_apps_render"; then
  echo "relay private-key configuration leaked into the cell render" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" \
  --set-json 'agentEmail.receivePilot.agentIDs=["agent_aaaaaaaaaaaaaaaa","agent_bbbbbbbbbbbbbbbb","agent_cccccccccccccccc","agent_dddddddddddddddd"]' \
  >/dev/null 2>&1; then
  echo "enabled pilot with four agents unexpectedly passed validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" \
  --set-json 'agentEmail.receivePilot.agentIDs=["agent_aaaaaaaaaaaaaaaa","agent_bbbbbbbbbbbbbbbb","agent_cccccccccccccccc","agent_dddddddddddddddd","agent_eeeeeeeeeeeeeeee","agent_ffffffffffffffff","agent_gggggggggggggggg","agent_hhhhhhhhhhhhhhhh","agent_iiiiiiiiiiiiiiii","agent_jjjjjjjjjjjjjjjj","agent_kkkkkkkkkkkkkkkk"]' \
  >/dev/null 2>&1; then
  echo "enabled pilot with eleven agents unexpectedly passed validation" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" \
  --set agentEmail.receivePilot.retryCanaryAgentID=agent_ffffffffffffffff \
  >/dev/null 2>&1; then
  echo "enabled pilot accepted a retry canary outside its enrolled agents" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_email_pilot_profile" \
  --set-json 'apps.witselfServer.agentEmail.receivePilot.agentIDs=["agent_aaaaaaaaaaaaaaaa","agent_bbbbbbbbbbbbbbbb","agent_cccccccccccccccc","agent_dddddddddddddddd"]' \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted an enabled pilot with four agents" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_email_pilot_profile" \
  --set apps.witselfServer.agentEmail.receivePilot.retryCanaryAgentID=agent_ffffffffffffffff \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted a retry canary outside its enrolled agents" >&2
  exit 1
fi
require_sequence "$email_pilot_apps_render" \
  "        agentEmail:" \
  "          receivePilot:" \
  "            agentIDs:" \
  "            - agent_aaaaaaaaaaaaaaaa" \
  "            - agent_bbbbbbbbbbbbbbbb" \
  "            - agent_cccccccccccccccc" \
  "            - agent_dddddddddddddddd" \
  "            - agent_eeeeeeeeeeeeeeee" \
  "            audience: gcp-sandbox-use1-dev" \
  "            domain: agent-mail.witwave.ai" \
  "            enabled: true" \
  "            realmID: realm_aaaaaaaaaaaaaaaa" \
  "            relayPublicKeysJSON: '{\"pilot-2026-07\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}'" \
  "            relayReplayWindow: 5m" \
  "            retryCanaryAgentID: agent_aaaaaaaaaaaaaaaa"
default_checksum="$(config_checksum "$default_render")"
email_pilot_checksum="$(config_checksum "$email_pilot_render")"
if [[ -z "$default_checksum" || -z "$email_pilot_checksum" || "$default_checksum" == "$email_pilot_checksum" ]]; then
  echo "agent-email pilot activation did not change the pod config checksum" >&2
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

# Transcript retention requires distinct disabled, preview, and enforce
# configurations. Enabling without a mode remains non-destructive, and each
# phase changes the ConfigMap checksum so every replica converges.
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "true"' "$retention_preview_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_MODE: "preview"' "$retention_preview_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "true"' "$retention_enforce_render"
require_line '  WITSELF_TRANSCRIPT_RETENTION_MODE: "enforce"' "$retention_enforce_render"
retention_disabled_checksum="$(config_checksum "$default_render")"
retention_preview_checksum="$(config_checksum "$retention_preview_render")"
retention_enforce_checksum="$(config_checksum "$retention_enforce_render")"
if [[ -z "$retention_disabled_checksum" || -z "$retention_preview_checksum" ||
  -z "$retention_enforce_checksum" ||
  "$retention_disabled_checksum" == "$retention_preview_checksum" ||
  "$retention_preview_checksum" == "$retention_enforce_checksum" ]]; then
  echo "transcript-retention rollout states did not produce distinct pod config checksums" >&2
  exit 1
fi
require_sequence "$apps_render" \
  "        transcriptRetention:" \
  "          batchSize: 100" \
  "          enabled: false" \
  "          interval: 5m" \
  "          mode: preview"
require_sequence "$retention_preview_apps_render" \
  "        transcriptRetention:" \
  "          batchSize: 100" \
  "          enabled: true" \
  "          interval: 5m" \
  "          mode: preview"
require_sequence "$retention_enforce_apps_render" \
  "        transcriptRetention:" \
  "          batchSize: 100" \
  "          enabled: true" \
  "          interval: 5m" \
  "          mode: enforce"

echo "Helm rollout rendering checks passed"
