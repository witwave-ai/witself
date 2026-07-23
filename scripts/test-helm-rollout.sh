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

render_dir="$(mktemp -d)"
trap 'rm -r "$render_dir"' EXIT

default_render="$render_dir/default.yaml"
gcp_render="$render_dir/gcp.yaml"
portable_worker_render="$render_dir/portable-worker.yaml"
apps_render="$render_dir/apps.yaml"
live_apps_render="$render_dir/live-apps.yaml"
phase_b_gcp_render="$render_dir/phase-b-gcp.yaml"
phase_b_apps_render="$render_dir/phase-b-apps.yaml"
email_pilot_render="$render_dir/email-pilot.yaml"
email_pilot_apps_render="$render_dir/email-pilot-apps.yaml"
retention_preview_render="$render_dir/retention-preview.yaml"
retention_enforce_render="$render_dir/retention-enforce.yaml"
retention_preview_apps_render="$render_dir/retention-preview-apps.yaml"
retention_enforce_apps_render="$render_dir/retention-enforce-apps.yaml"
style_tuned_render="$render_dir/style-tuned.yaml"
monitor_render="$render_dir/monitors.yaml"
long_name_render="$render_dir/long-name.yaml"
long_fullname="$(printf 'a%.0s' {1..63})"
long_worker_fullname="${long_fullname:0:56}-worker"
long_worker_metrics_fullname="${long_fullname:0:48}-worker-metrics"

helm template witself-server "$server_chart" --namespace witself >"$default_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" >"$gcp_render"
helm template witself-server "$server_chart" --namespace witself \
  --set worker.enabled=true \
  --set database.existingSecret.name=witself-db >"$portable_worker_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=false \
  --set apps.witselfServer.transcriptRetention.enabled=false \
  --set apps.witselfServer.transcriptRetention.mode=preview >"$apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" >"$live_apps_render"
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
  --values "$gcp_profile" \
  --set worker.transcriptRetention.enabled=true \
  --set worker.transcriptRetention.mode=preview >"$retention_preview_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.enabled=true \
  --set worker.transcriptRetention.mode=enforce >"$retention_enforce_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.transcriptRetention.enabled=true \
  --set apps.witselfServer.transcriptRetention.mode=preview >"$retention_preview_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.transcriptRetention.enabled=true \
  --set apps.witselfServer.transcriptRetention.mode=enforce >"$retention_enforce_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.batchSize=101 >"$style_tuned_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.podMonitor.enabled=true \
  --set worker.metrics.serviceMonitor.enabled=true \
  --set worker.metrics.podMonitor.enabled=true >"$monitor_render"
helm template witself-server "$server_chart" --namespace witself \
  --set-string fullnameOverride="$long_fullname" \
  --set worker.enabled=true \
  --set database.existingSecret.name=witself-db >"$long_name_render"

require_line() {
  local expected="$1"
  local file="$2"
  if ! grep -Fqx "$expected" "$file"; then
    echo "missing rendered line: $expected" >&2
    return 1
  fi
}

reject_line() {
  local unexpected="$1"
  local file="$2"
  if grep -Fqx "$unexpected" "$file"; then
    echo "unexpected rendered line: $unexpected" >&2
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

extract_document() {
  local kind="$1"
  local name="$2"
  local source="$3"
  local destination="$4"
  awk -v wanted_kind="$kind" -v wanted_name="$name" '
    function reset_document() {
      document = ""
      document_kind = ""
      document_name = ""
      in_metadata = 0
    }
    function emit_document() {
      if (document_kind == wanted_kind && document_name == wanted_name) {
        printf "%s", document
      }
    }
    BEGIN { reset_document() }
    /^---$/ {
      emit_document()
      reset_document()
      next
    }
    {
      document = document $0 ORS
      if ($1 == "kind:") {
        document_kind = $2
      }
      if ($0 == "metadata:") {
        in_metadata = 1
        next
      }
      if (in_metadata && $1 == "name:" && document_name == "") {
        document_name = $2
        gsub(/^"|"$/, "", document_name)
      }
      if (in_metadata && $0 !~ /^ / && $0 != "metadata:") {
        in_metadata = 0
      }
    }
    END { emit_document() }
  ' "$source" >"$destination"
  if [[ ! -s "$destination" ]]; then
    echo "missing rendered $kind/$name" >&2
    return 1
  fi
}

config_checksum() {
  awk '$1 == "checksum/config:" { print $2; exit }' "$1"
}

expect_server_template_failure() {
  local description="$1"
  shift
  if helm template witself-server "$server_chart" --namespace witself "$@" >/dev/null 2>&1; then
    echo "$description unexpectedly passed Helm validation" >&2
    return 1
  fi
}

default_server_config="$render_dir/default-server-config.yaml"
default_server_deployment="$render_dir/default-server-deployment.yaml"
gcp_server_config="$render_dir/gcp-server-config.yaml"
gcp_worker_config="$render_dir/gcp-worker-config.yaml"
gcp_server_deployment="$render_dir/gcp-server-deployment.yaml"
gcp_worker_deployment="$render_dir/gcp-worker-deployment.yaml"
gcp_server_service="$render_dir/gcp-server-service.yaml"
gcp_server_metrics_service="$render_dir/gcp-server-metrics-service.yaml"
gcp_worker_metrics_service="$render_dir/gcp-worker-metrics-service.yaml"
gcp_worker_network_policy="$render_dir/gcp-worker-network-policy.yaml"
gcp_server_pdb="$render_dir/gcp-server-pdb.yaml"
gcp_worker_pdb="$render_dir/gcp-worker-pdb.yaml"
portable_worker_deployment="$render_dir/portable-worker-deployment.yaml"

extract_document ConfigMap witself-server "$default_render" "$default_server_config"
extract_document Deployment witself-server "$default_render" "$default_server_deployment"
extract_document ConfigMap witself-server "$gcp_render" "$gcp_server_config"
extract_document ConfigMap witself-worker "$gcp_render" "$gcp_worker_config"
extract_document Deployment witself-server "$gcp_render" "$gcp_server_deployment"
extract_document Deployment witself-worker "$gcp_render" "$gcp_worker_deployment"
extract_document Service witself-server "$gcp_render" "$gcp_server_service"
extract_document Service witself-server-metrics "$gcp_render" "$gcp_server_metrics_service"
extract_document Service witself-worker-metrics "$gcp_render" "$gcp_worker_metrics_service"
extract_document NetworkPolicy witself-worker "$gcp_render" "$gcp_worker_network_policy"
extract_document PodDisruptionBudget witself-server "$gcp_render" "$gcp_server_pdb"
extract_document PodDisruptionBudget witself-worker "$gcp_render" "$gcp_worker_pdb"
extract_document Deployment witself-worker "$portable_worker_render" "$portable_worker_deployment"
extract_document Deployment "$long_fullname" "$long_name_render" "$render_dir/long-name-server-deployment.yaml"
extract_document Deployment "$long_worker_fullname" "$long_name_render" "$render_dir/long-name-worker-deployment.yaml"
extract_document Service "$long_worker_metrics_fullname" "$long_name_render" "$render_dir/long-name-worker-metrics-service.yaml"

# Portable defaults keep the API rollout-safe and fail closed on a worker that
# has no shared database Secret.
require_line "  minReadySeconds: 10" "$default_server_deployment"
require_line "    type: RollingUpdate" "$default_server_deployment"
require_line "      maxSurge: 1" "$default_server_deployment"
require_line "      maxUnavailable: 0" "$default_server_deployment"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$default_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$default_server_config")" -ne 1 ]]; then
  echo "default render exposed agent-email configuration beyond the disabled gate" >&2
  exit 1
fi
if grep -Eq 'WITSELF_AVATAR_STYLE_ROLLOUT_(BATCH_SIZE|INTERVAL|BATCH_TIMEOUT)|WITSELF_TRANSCRIPT_RETENTION_(MODE|BATCH_SIZE|INTERVAL|BATCH_TIMEOUT)' \
  "$default_server_config"; then
  echo "API ConfigMap contains worker-only tuning values" >&2
  exit 1
fi
if grep -Fq "name: witself-worker" "$default_render"; then
  echo "public defaults unexpectedly rendered the database-dependent worker" >&2
  exit 1
fi
require_line "  replicas: 2" "$portable_worker_deployment"
if grep -Fqx "          lifecycle:" "$default_server_deployment"; then
  echo "default render unexpectedly contains a container lifecycle handler" >&2
  exit 1
fi

# The managed profile renders two independently selectable workers with safe
# rolling overlap, health/metrics listeners, bounded resources, and a shared
# database credential. The existing API selector remains byte-compatible with
# the prior chart while its pod gains a non-selector component label.
require_line "  replicas: 2" "$gcp_worker_deployment"
require_line "  minReadySeconds: 10" "$gcp_worker_deployment"
require_line "      maxSurge: 1" "$gcp_worker_deployment"
require_line "      maxUnavailable: 0" "$gcp_worker_deployment"
require_sequence "$gcp_worker_deployment" \
  "  selector:" \
  "    matchLabels:" \
  "      app.kubernetes.io/name: witself-worker" \
  "      app.kubernetes.io/instance: witself-server" \
  "      app.kubernetes.io/component: worker"
require_sequence "$gcp_server_deployment" \
  "  selector:" \
  "    matchLabels:" \
  "      app.kubernetes.io/name: witself-server" \
  "      app.kubernetes.io/instance: witself-server" \
  "  template:"
require_line "        app.kubernetes.io/component: server" "$gcp_server_deployment"
require_sequence "$gcp_worker_deployment" \
  "          command:" \
  "            - /usr/local/bin/witself-worker" \
  "          args:" \
  "            - serve"
require_line "            - name: WITSELF_DATABASE_URL" "$gcp_worker_deployment"
require_line '                  name: "witself-db"' "$gcp_worker_deployment"
require_line "              containerPort: 8081" "$gcp_worker_deployment"
require_line "              containerPort: 9090" "$gcp_worker_deployment"
require_line "              path: /livez" "$gcp_worker_deployment"
require_line "              path: /readyz" "$gcp_worker_deployment"
require_line "              path: /startupz" "$gcp_worker_deployment"
require_line "              cpu: 100m" "$gcp_worker_deployment"
require_line "              memory: 128Mi" "$gcp_worker_deployment"
require_line "              memory: 512Mi" "$gcp_worker_deployment"
reject_line "            - name: api" "$gcp_worker_deployment"

require_line '  WITSELF_HEALTH_ADDR: ":8081"' "$gcp_worker_config"
require_line '  WITSELF_METRICS_ADDR: ":9090"' "$gcp_worker_config"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "true"' "$gcp_worker_config"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT: "30s"' "$gcp_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$gcp_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_BATCH_TIMEOUT: "2m"' "$gcp_worker_config"
if grep -Eq 'WITSELF_(API_ADDR|BOOTSTRAP|PROVISION|AGENT_EMAIL|BACKEND_KIND|FACT_DELETION|AVATAR_PAYLOAD)' \
  "$gcp_worker_config" "$gcp_worker_deployment"; then
  echo "worker received API/bootstrap/provision/email-only configuration" >&2
  exit 1
fi
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "false"' "$gcp_server_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$gcp_server_config"

require_sequence "$gcp_worker_metrics_service" \
  "  selector:" \
  "    app.kubernetes.io/name: witself-worker" \
  "    app.kubernetes.io/instance: witself-server" \
  "    app.kubernetes.io/component: worker"
require_line "    - name: metrics" "$gcp_worker_metrics_service"
reject_line "    - name: api" "$gcp_worker_metrics_service"
if grep -Fq "witself-worker" "$gcp_server_service" "$gcp_server_metrics_service"; then
  echo "server Service selector crossed into the worker label set" >&2
  exit 1
fi
require_line "  minAvailable: 1" "$gcp_worker_pdb"
require_line "        - port: health" "$gcp_worker_network_policy"
require_line "        - port: metrics" "$gcp_worker_network_policy"
reject_line "        - port: api" "$gcp_worker_network_policy"

# Optional monitor resources retain the same disjoint selector sets.
server_service_monitor="$render_dir/server-service-monitor.yaml"
worker_service_monitor="$render_dir/worker-service-monitor.yaml"
server_pod_monitor="$render_dir/server-pod-monitor.yaml"
worker_pod_monitor="$render_dir/worker-pod-monitor.yaml"
extract_document ServiceMonitor witself-server "$monitor_render" "$server_service_monitor"
extract_document ServiceMonitor witself-worker "$monitor_render" "$worker_service_monitor"
extract_document PodMonitor witself-server "$monitor_render" "$server_pod_monitor"
extract_document PodMonitor witself-worker "$monitor_render" "$worker_pod_monitor"
require_line "      app.kubernetes.io/name: witself-worker" "$worker_service_monitor"
require_line "      app.kubernetes.io/component: worker" "$worker_service_monitor"
require_line "      app.kubernetes.io/name: witself-worker" "$worker_pod_monitor"
require_line "      app.kubernetes.io/component: worker" "$worker_pod_monitor"
if grep -Fq "witself-worker" "$server_service_monitor" "$server_pod_monitor"; then
  echo "server monitor selector crossed into the worker label set" >&2
  exit 1
fi

# Managed API rollout controls remain intact.
require_line "  replicas: 2" "$gcp_server_deployment"
require_line "      terminationGracePeriodSeconds: 210" "$gcp_server_deployment"
require_line "          lifecycle:" "$gcp_server_deployment"
require_line "                seconds: 120" "$gcp_server_deployment"
require_line "  minAvailable: 1" "$gcp_server_pdb"
require_line "          minDomains: 2" "$gcp_server_deployment"
require_line "          topologyKey: topology.kubernetes.io/zone" "$gcp_server_deployment"
require_line "          whenUnsatisfiable: DoNotSchedule" "$gcp_server_deployment"

# Schema/template validation rejects unsafe rolling strategies, invalid job
# bounds, and an enabled worker without its shared database Secret.
expect_server_template_failure \
  "worker without database Secret" \
  --set worker.enabled=true
expect_server_template_failure \
  "legacy top-level avatar style rollout values" \
  --set avatar.styleRollout.enabled=true
expect_server_template_failure \
  "legacy top-level transcript retention values" \
  --set transcriptRetention.enabled=true
expect_server_template_failure \
  "zero worker replicas" \
  --values "$gcp_profile" \
  --set worker.replicaCount=0
expect_server_template_failure \
  "worker rolling strategy with no surge or availability" \
  --values "$gcp_profile" \
  --set worker.strategy.rollingUpdate.maxUnavailable=0 \
  --set worker.strategy.rollingUpdate.maxSurge=0
expect_server_template_failure \
  "negative API preStop sleep" \
  --set lifecycle.preStopSleepSeconds=-1
expect_server_template_failure \
  "oversized avatar style batch" \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.batchSize=1001
expect_server_template_failure \
  "undersized avatar style interval" \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.interval=99ms
expect_server_template_failure \
  "oversized avatar style interval" \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.interval=2h
expect_server_template_failure \
  "undersized avatar style batch timeout" \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.batchTimeout=99ms
expect_server_template_failure \
  "oversized avatar style batch timeout" \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.batchTimeout=6m
expect_server_template_failure \
  "unknown transcript retention mode" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.mode=delete
expect_server_template_failure \
  "oversized transcript retention batch" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.batchSize=1001
expect_server_template_failure \
  "undersized transcript retention interval" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.interval=59s
expect_server_template_failure \
  "oversized transcript retention interval" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.interval=25h
expect_server_template_failure \
  "undersized transcript retention batch timeout" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.batchTimeout=999ms
expect_server_template_failure \
  "oversized transcript retention batch timeout" \
  --values "$gcp_profile" \
  --set worker.transcriptRetention.batchTimeout=6m
expect_server_template_failure \
  "API rolling strategy with no surge or availability" \
  --set strategy.rollingUpdate.maxUnavailable=0 \
  --set strategy.rollingUpdate.maxSurge=0
expect_server_template_failure \
  "worker ServiceMonitor without metrics Service" \
  --values "$gcp_profile" \
  --set worker.metrics.service.enabled=false \
  --set worker.metrics.serviceMonitor.enabled=true

# The receive-only email pilot remains server-only and retains its fail-closed
# enrollment validation.
extract_document ConfigMap witself-server "$email_pilot_render" "$render_dir/email-server-config.yaml"
email_server_config="$render_dir/email-server-config.yaml"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "true"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_DOMAIN: "agent-mail.witwave.ai"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AUDIENCE: "gcp-sandbox-use1-dev"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_REALM_ID: "realm_aaaaaaaaaaaaaaaa"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS: "agent_aaaaaaaaaaaaaaaa,agent_bbbbbbbbbbbbbbbb,agent_cccccccccccccccc,agent_dddddddddddddddd,agent_eeeeeeeeeeeeeeee"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID: "agent_aaaaaaaaaaaaaaaa"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON: "{\"pilot-2026-07\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW: "5m"' "$email_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$email_server_config")" -ne 8 ]]; then
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

# Keep the currently pinned app-of-apps contract unchanged. Worker activation
# is staged for the later atomic chart/image/app-values pin update.
require_line "    drainingTimeoutSec: 60" "$apps_render"
require_line "        minReadySeconds: 10" "$apps_render"
require_line "        strategy:" "$apps_render"
require_line "            maxSurge: 1" "$apps_render"
require_line "            maxUnavailable: 0" "$apps_render"
require_line "          type: RollingUpdate" "$apps_render"
require_line "          preStopSleepSeconds: 120" "$apps_render"
require_line "        replicaCount: 2" "$apps_render"
require_line "        terminationGracePeriodSeconds: 210" "$apps_render"
require_line "          enabled: true" "$apps_render"
require_line "          minAvailable: 1" "$apps_render"
require_line "          minDomains: 2" "$apps_render"
require_line "          topologyKey: topology.kubernetes.io/zone" "$apps_render"
require_line "          whenUnsatisfiable: DoNotSchedule" "$apps_render"
reject_line "        worker:" "$apps_render"
require_sequence "$apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: false" \
  "          styleRollout:" \
  "            batchSize: 100" \
  "            batchTimeout: 30s" \
  "            enabled: true" \
  "            interval: 2s"
require_sequence "$phase_b_apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: true" \
  "          styleRollout:" \
  "            batchSize: 100" \
  "            batchTimeout: 30s" \
  "            enabled: true" \
  "            interval: 2s"
extract_document ConfigMap witself-server "$phase_b_gcp_render" "$render_dir/phase-b-server-config.yaml"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "true"' "$render_dir/phase-b-server-config.yaml"
require_sequence "$live_apps_render" \
  "        transcriptRetention:" \
  "          batchSize: 100" \
  "          enabled: true" \
  "          interval: 5m" \
  "          mode: enforce"
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

# API-only changes restart API pods only; worker job tuning and retention phase
# changes restart worker pods only.
extract_document Deployment witself-server "$phase_b_gcp_render" "$render_dir/phase-b-server-deployment.yaml"
extract_document Deployment witself-worker "$phase_b_gcp_render" "$render_dir/phase-b-worker-deployment.yaml"
phase_a_server_checksum="$(config_checksum "$gcp_server_deployment")"
phase_b_server_checksum="$(config_checksum "$render_dir/phase-b-server-deployment.yaml")"
phase_a_worker_checksum="$(config_checksum "$gcp_worker_deployment")"
phase_b_worker_checksum="$(config_checksum "$render_dir/phase-b-worker-deployment.yaml")"
if [[ -z "$phase_a_server_checksum" || -z "$phase_b_server_checksum" ||
  "$phase_a_server_checksum" == "$phase_b_server_checksum" ]]; then
  echo "avatar payload compaction did not restart the API pods" >&2
  exit 1
fi
if [[ "$phase_a_worker_checksum" != "$phase_b_worker_checksum" ]]; then
  echo "API-only avatar payload compaction unexpectedly restarted worker pods" >&2
  exit 1
fi

extract_document Deployment witself-server "$style_tuned_render" "$render_dir/style-server-deployment.yaml"
extract_document Deployment witself-worker "$style_tuned_render" "$render_dir/style-worker-deployment.yaml"
if [[ "$phase_a_server_checksum" != "$(config_checksum "$render_dir/style-server-deployment.yaml")" ]]; then
  echo "worker style tuning unexpectedly restarted API pods" >&2
  exit 1
fi
if [[ "$phase_a_worker_checksum" == "$(config_checksum "$render_dir/style-worker-deployment.yaml")" ]]; then
  echo "worker style tuning did not restart worker pods" >&2
  exit 1
fi

extract_document Deployment witself-server "$retention_preview_render" "$render_dir/preview-server-deployment.yaml"
extract_document Deployment witself-worker "$retention_preview_render" "$render_dir/preview-worker-deployment.yaml"
extract_document Deployment witself-server "$retention_enforce_render" "$render_dir/enforce-server-deployment.yaml"
extract_document Deployment witself-worker "$retention_enforce_render" "$render_dir/enforce-worker-deployment.yaml"
retention_preview_worker_checksum="$(config_checksum "$render_dir/preview-worker-deployment.yaml")"
retention_enforce_worker_checksum="$(config_checksum "$render_dir/enforce-worker-deployment.yaml")"
if [[ "$phase_a_worker_checksum" == "$retention_preview_worker_checksum" ||
  "$retention_preview_worker_checksum" == "$retention_enforce_worker_checksum" ]]; then
  echo "transcript-retention phases did not produce distinct worker checksums" >&2
  exit 1
fi
if [[ "$phase_a_server_checksum" != "$(config_checksum "$render_dir/preview-server-deployment.yaml")" ]]; then
  echo "worker transcript-retention phase unexpectedly restarted API pods" >&2
  exit 1
fi
if [[ "$phase_a_server_checksum" != "$(config_checksum "$render_dir/enforce-server-deployment.yaml")" ]]; then
  echo "worker transcript-retention enforcement unexpectedly restarted API pods" >&2
  exit 1
fi

default_server_checksum="$(config_checksum "$default_server_deployment")"
extract_document Deployment witself-server "$email_pilot_render" "$render_dir/email-server-deployment.yaml"
email_server_checksum="$(config_checksum "$render_dir/email-server-deployment.yaml")"
if [[ -z "$default_server_checksum" || -z "$email_server_checksum" ||
  "$default_server_checksum" == "$email_server_checksum" ]]; then
  echo "agent-email pilot activation did not restart API pods" >&2
  exit 1
fi

echo "Helm rollout rendering checks passed"
