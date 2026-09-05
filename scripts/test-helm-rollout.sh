#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$repo_root/scripts/test-agent-email-cell-operation.sh"
bash "$repo_root/scripts/test-agent-email-cell-smoke.sh"
server_chart="$repo_root/charts/witself-server"
apps_chart="$repo_root/.gitops/charts/apps"
apps_profile="$apps_chart/ci/gcp-rollout-values.yaml"
gcp_profile="$server_chart/ci/gcp-rollout-values.yaml"
email_pilot_profile="$server_chart/ci/agent-email-pilot-values.yaml"
apps_email_pilot_profile="$apps_chart/ci/agent-email-pilot-values.yaml"
email_production_profile="$server_chart/ci/agent-email-production-values.yaml"
apps_email_production_profile="$apps_chart/ci/agent-email-production-values.yaml"
gcp_cell="$repo_root/.gitops/cells/gcp-sandbox-use1-dev/values.yaml"
civo_cell="$repo_root/.gitops/cells/civo-sandbox-usw2-dev/values.yaml"
civo_backup_cell="$repo_root/.gitops/cells/civo-sandbox-use1-backup/values.yaml"
civo_use1_cell="$repo_root/.gitops/cells/civo-sandbox-use1-dev/values.yaml"

render_dir="$(mktemp -d)"
trap 'rm -r "$render_dir"' EXIT

default_render="$render_dir/default.yaml"
gcp_render="$render_dir/gcp.yaml"
billing_empty_render="$render_dir/billing-empty.yaml"
billing_render="$render_dir/billing.yaml"
billing_apps_render="$render_dir/billing-apps.yaml"
portable_worker_render="$render_dir/portable-worker.yaml"
apps_render="$render_dir/apps.yaml"
live_apps_render="$render_dir/live-apps.yaml"
agent_email_rate_cleanup_apps_render="$render_dir/agent-email-rate-cleanup-apps.yaml"
civo_apps_render="$render_dir/civo-apps.yaml"
civo_backup_apps_render="$render_dir/civo-backup-apps.yaml"
civo_use1_apps_render="$render_dir/civo-use1-apps.yaml"
civo_default_preset_apps_render="$render_dir/civo-default-preset-apps.yaml"
backup_validation_render="$render_dir/backup-validation.yaml"
backup_validation_apps_render="$render_dir/backup-validation-apps.yaml"
phase_b_gcp_render="$render_dir/phase-b-gcp.yaml"
phase_b_apps_render="$render_dir/phase-b-apps.yaml"
email_pilot_render="$render_dir/email-pilot.yaml"
email_pilot_apps_render="$render_dir/email-pilot-apps.yaml"
email_pilot_legacy_apps_render="$render_dir/email-pilot-legacy-apps.yaml"
email_pilot_new_chart_old_image_render="$render_dir/email-pilot-new-chart-old-image.yaml"
email_pilot_old_chart_new_image_render="$render_dir/email-pilot-old-chart-new-image.yaml"
email_production_render="$render_dir/email-production.yaml"
email_production_apps_render="$render_dir/email-production-apps.yaml"
email_production_pre245_apps_render="$render_dir/email-production-pre245-apps.yaml"
retention_preview_render="$render_dir/retention-preview.yaml"
retention_enforce_render="$render_dir/retention-enforce.yaml"
retention_preview_apps_render="$render_dir/retention-preview-apps.yaml"
retention_enforce_apps_render="$render_dir/retention-enforce-apps.yaml"
email_retention_preview_render="$render_dir/email-retention-preview.yaml"
email_retention_enforce_render="$render_dir/email-retention-enforce.yaml"
email_outbound_render="$render_dir/email-outbound.yaml"
email_outbound_apps_render="$render_dir/email-outbound-apps.yaml"
style_tuned_render="$render_dir/style-tuned.yaml"
monitor_render="$render_dir/monitors.yaml"
apps_monitor_render="$render_dir/apps-monitors.yaml"
long_name_render="$render_dir/long-name.yaml"
long_fullname="$(printf 'a%.0s' {1..63})"
long_worker_fullname="${long_fullname:0:56}-worker"
long_worker_metrics_fullname="${long_fullname:0:48}-worker-metrics"

helm template witself-server "$server_chart" --namespace witself >"$default_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" >"$gcp_render"
helm template witself-server "$server_chart" --namespace witself \
  --set-string billing.endpoint= >"$billing_empty_render"
helm template witself-server "$server_chart" --namespace witself \
  --set-string billing.endpoint=https://self.witwave.ai \
  >"$billing_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --set apps.witselfServer.chartVersion=0.0.255 \
  --set apps.witselfServer.imageTag=0.0.255 \
  --set-string apps.witselfServer.billing.endpoint=https://self.witwave.ai \
  >"$billing_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --set worker.enabled=true \
  --set database.existingSecret.name=witself-db >"$portable_worker_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.avatarPayloadCompactionEnabled=false \
  --set apps.witselfServer.worker.transcriptRetention.enabled=false \
  --set apps.witselfServer.worker.transcriptRetention.mode=preview >"$apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" >"$live_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.chartVersion=0.0.226 \
  --set apps.witselfServer.imageTag=0.0.226 >"$agent_email_rate_cleanup_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" >"$civo_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_backup_cell" >"$civo_backup_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_use1_cell" >"$civo_use1_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --set-string apps.civoPostgres.resourcesPreset= >"$civo_default_preset_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --set backup.existingSecret.name=witself-backup \
  --set backup.validation.enabled=true >"$backup_validation_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --set apps.witselfServer.backup.enabled=true \
  --set apps.witselfServer.backup.validationEnabled=true \
  --set apps.witselfServer.backup.targetName=witself-backup \
  >"$backup_validation_apps_render"
if helm template witself-server "$server_chart" --namespace witself \
  --set backup.validation.enabled=true \
  >"$render_dir/invalid-backup-validation.yaml" \
  2>"$render_dir/invalid-backup-validation.err"; then
  echo "backup validation rendered without a backup credential Secret" >&2
  exit 1
fi
if ! grep -Fq \
  'backup.validation.enabled requires backup.existingSecret.name' \
  "$render_dir/invalid-backup-validation.err"; then
  echo "backup validation failure did not explain the missing credential Secret" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --set apps.witselfServer.backup.enabled=false \
  --set apps.witselfServer.backup.validationEnabled=true \
  >"$render_dir/invalid-backup-validation-apps.yaml" \
  2>"$render_dir/invalid-backup-validation-apps.err"; then
  echo "app-of-apps backup validation rendered with backup export disabled" >&2
  exit 1
fi
if ! grep -Fq \
  'apps.witselfServer.backup.validationEnabled requires apps.witselfServer.backup.enabled' \
  "$render_dir/invalid-backup-validation-apps.err"; then
  echo "app-of-apps validation failure did not explain the disabled export path" >&2
  exit 1
fi
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
  --values "$apps_email_pilot_profile" >"$email_pilot_legacy_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --values "$apps_email_pilot_profile" \
  --set apps.witselfServer.chartVersion=0.0.232 \
  --set apps.witselfServer.imageTag=0.0.232 >"$email_pilot_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --values "$apps_email_pilot_profile" \
  --set apps.witselfServer.chartVersion=0.0.232 \
  --set apps.witselfServer.imageTag=0.0.231 >"$email_pilot_new_chart_old_image_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --values "$apps_email_pilot_profile" \
  --set apps.witselfServer.chartVersion=0.0.231 \
  --set apps.witselfServer.imageTag=0.0.232 >"$email_pilot_old_chart_new_image_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 >"$email_production_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  >"$email_production_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set apps.witselfServer.chartVersion=0.0.244 \
  --set apps.witselfServer.imageTag=0.0.244 \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=false \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name= \
  --set-string apps.witselfServer.billing.endpoint= \
  >"$email_production_pre245_apps_render"
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
  --set apps.witselfServer.worker.transcriptRetention.enabled=true \
  --set apps.witselfServer.worker.transcriptRetention.mode=preview >"$retention_preview_apps_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.worker.transcriptRetention.enabled=true \
  --set apps.witselfServer.worker.transcriptRetention.mode=enforce >"$retention_enforce_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.enabled=true \
  --set worker.agentEmailRetention.mode=preview >"$email_retention_preview_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.enabled=true \
  --set worker.agentEmailRetention.mode=enforce >"$email_retention_enforce_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.agentEmailOutbound.enabled=true \
  --set worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set worker.agentEmailOutbound.dispatchKeyID=founder-cell \
  --set worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch \
  >"$email_outbound_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.chartVersion=0.0.245 \
  --set apps.witselfServer.imageTag=0.0.245 \
  --set apps.witselfServer.agentEmail.providerEventTokenSecret.name=witself-email-provider-events \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=true \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchKeyID=founder-cell \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch \
  >"$email_outbound_apps_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set worker.avatarStyleRollout.batchSize=101 >"$style_tuned_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.podMonitor.enabled=true \
  --set worker.metrics.serviceMonitor.enabled=true \
  --set worker.metrics.podMonitor.enabled=true >"$monitor_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.metrics.serviceMonitor.enabled=true \
  --set apps.witselfServer.metrics.serviceMonitor.interval=45s \
  --set apps.witselfServer.metrics.serviceMonitor.scrapeTimeout=12s \
  --set-string apps.witselfServer.metrics.serviceMonitor.labels.monitor=server \
  --set-string 'apps.witselfServer.metrics.serviceMonitor.relabelings[0].targetLabel=service_role' \
  --set-string 'apps.witselfServer.metrics.serviceMonitor.relabelings[0].replacement=server' \
  --set-string 'apps.witselfServer.metrics.serviceMonitor.metricRelabelings[0].sourceLabels[0]=__name__' \
  --set-string 'apps.witselfServer.metrics.serviceMonitor.metricRelabelings[0].regex=witself_server_.+' \
  --set-string 'apps.witselfServer.metrics.serviceMonitor.metricRelabelings[0].action=keep' \
  --set apps.witselfServer.worker.metrics.serviceMonitor.enabled=true \
  --set apps.witselfServer.worker.metrics.serviceMonitor.interval=50s \
  --set apps.witselfServer.worker.metrics.serviceMonitor.scrapeTimeout=15s \
  --set-string apps.witselfServer.worker.metrics.serviceMonitor.labels.monitor=worker \
  --set-string 'apps.witselfServer.worker.metrics.serviceMonitor.relabelings[0].targetLabel=service_role' \
  --set-string 'apps.witselfServer.worker.metrics.serviceMonitor.relabelings[0].replacement=worker' \
  --set-string 'apps.witselfServer.worker.metrics.serviceMonitor.metricRelabelings[0].sourceLabels[0]=__name__' \
  --set-string 'apps.witselfServer.worker.metrics.serviceMonitor.metricRelabelings[0].regex=witself_worker_.+' \
  --set-string 'apps.witselfServer.worker.metrics.serviceMonitor.metricRelabelings[0].action=keep' \
  >"$apps_monitor_render"
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

read_witself_server_semver_scalar() {
  local key="$1"
  local file="$2"
  local value
  value="$(awk -v key="${key}:" '
    /^  witselfServer:[[:space:]]*$/ { in_server = 1; next }
    in_server && /^  [^ ]/ { in_server = 0 }
    in_server && $1 == key { count += 1; value = $2 }
    END { if (count == 1) print value }
  ' "$file")"
  if [[ ! "$value" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "expected exactly one semantic-version value for ${key} in ${file}" >&2
    exit 1
  fi
  printf '%s' "$value"
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
      if ($0 ~ /^kind:/) {
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

extract_application_helm_values() {
  local source="$1"
  local destination="$2"
  awk '
    $0 == "      values: |" {
      in_values = 1
      next
    }
    in_values {
      if ($0 == "") {
        print
        next
      }
      if (substr($0, 1, 8) == "        ") {
        print substr($0, 9)
        next
      }
      exit
    }
  ' "$source" >"$destination"
  if [[ ! -s "$destination" ]]; then
    echo "missing nested Application Helm values" >&2
    return 1
  fi
}

config_checksum() {
  awk '$1 == "checksum/config:" { print $2; exit }' "$1"
}

server_config_checksum() {
  awk '$1 == "witself.io/server-config-checksum:" {
    gsub(/^"|"$/, "", $2)
    print $2
    exit
  }' "$1"
}

expect_server_template_failure() {
  local description="$1"
  shift
  if helm template witself-server "$server_chart" --namespace witself "$@" >/dev/null 2>&1; then
    echo "$description unexpectedly passed Helm validation" >&2
    return 1
  fi
}

expect_server_template_failure_message() {
  local description="$1"
  local expected_message="$2"
  shift 2
  local error_output="$render_dir/expected-server-template-failure.err"
  if helm template witself-server "$server_chart" --namespace witself "$@" \
    >/dev/null 2>"$error_output"; then
    echo "$description unexpectedly passed Helm validation" >&2
    return 1
  fi
  if ! grep -Fq "$expected_message" "$error_output"; then
    echo "$description failed without the expected validation message" >&2
    return 1
  fi
}

expect_apps_template_failure() {
  local description="$1"
  local expected_message="$2"
  shift 2
  local error_output="$render_dir/expected-apps-template-failure.err"
  if helm template witself-apps "$apps_chart" "$@" \
    >/dev/null 2>"$error_output"; then
    echo "$description unexpectedly passed Helm validation" >&2
    return 1
  fi
  if ! grep -Fq "$expected_message" "$error_output"; then
    echo "$description failed without the expected validation message" >&2
    return 1
  fi
}

default_server_config="$render_dir/default-server-config.yaml"
default_server_deployment="$render_dir/default-server-deployment.yaml"
gcp_server_config="$render_dir/gcp-server-config.yaml"
billing_server_config="$render_dir/billing-server-config.yaml"
billing_server_deployment="$render_dir/billing-server-deployment.yaml"
billing_server_application="$render_dir/billing-server-application.yaml"
billing_nested_values="$render_dir/billing-nested-values.yaml"
billing_nested_render="$render_dir/billing-nested-render.yaml"
billing_nested_server_config="$render_dir/billing-nested-server-config.yaml"
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
live_server_application="$render_dir/live-server-application.yaml"
live_nested_values="$render_dir/live-nested-values.yaml"
live_nested_render="$render_dir/live-nested-render.yaml"
live_nested_server_config="$render_dir/live-nested-server-config.yaml"
live_nested_worker_config="$render_dir/live-nested-worker-config.yaml"
live_nested_worker_deployment="$render_dir/live-nested-worker-deployment.yaml"
apps_monitor_application="$render_dir/apps-monitor-application.yaml"
apps_monitor_nested_values="$render_dir/apps-monitor-nested-values.yaml"
apps_monitor_nested_render="$render_dir/apps-monitor-nested-render.yaml"
apps_monitor_server="$render_dir/apps-monitor-server.yaml"
apps_monitor_worker="$render_dir/apps-monitor-worker.yaml"
civo_server_application="$render_dir/civo-server-application.yaml"
civo_backup_server_application="$render_dir/civo-backup-server-application.yaml"
civo_use1_server_application="$render_dir/civo-use1-server-application.yaml"
civo_postgres_application="$render_dir/civo-postgres-application.yaml"
civo_backup_postgres_application="$render_dir/civo-backup-postgres-application.yaml"
civo_use1_postgres_application="$render_dir/civo-use1-postgres-application.yaml"
civo_default_preset_postgres_application="$render_dir/civo-default-preset-postgres-application.yaml"
civo_server_nested_values="$render_dir/civo-server-nested-values.yaml"
civo_server_nested_render="$render_dir/civo-server-nested-render.yaml"
civo_server_config="$render_dir/civo-server-config.yaml"
civo_worker_config="$render_dir/civo-worker-config.yaml"
civo_server_deployment="$render_dir/civo-server-deployment.yaml"
civo_worker_deployment="$render_dir/civo-worker-deployment.yaml"
email_production_server_config="$render_dir/email-production-server-config.yaml"
email_production_server_deployment="$render_dir/email-production-server-deployment.yaml"
email_production_server_application="$render_dir/email-production-server-application.yaml"
email_production_nested_values="$render_dir/email-production-nested-values.yaml"
email_production_nested_render="$render_dir/email-production-nested-render.yaml"
email_production_nested_config="$render_dir/email-production-nested-config.yaml"
email_production_nested_deployment="$render_dir/email-production-nested-deployment.yaml"
email_production_pre245_server_application="$render_dir/email-production-pre245-server-application.yaml"
email_production_pre245_nested_values="$render_dir/email-production-pre245-nested-values.yaml"
email_production_retry_name_render="$render_dir/email-production-retry-name.yaml"
email_production_retry_name_config="$render_dir/email-production-retry-name-config.yaml"
email_production_retry_name_deployment="$render_dir/email-production-retry-name-deployment.yaml"
email_production_retry_key_render="$render_dir/email-production-retry-key.yaml"
email_production_retry_key_config="$render_dir/email-production-retry-key-config.yaml"
email_production_retry_key_deployment="$render_dir/email-production-retry-key-deployment.yaml"
email_outbound_worker_config="$render_dir/email-outbound-worker-config.yaml"
email_outbound_worker_deployment="$render_dir/email-outbound-worker-deployment.yaml"
email_outbound_server_application="$render_dir/email-outbound-server-application.yaml"
email_outbound_nested_values="$render_dir/email-outbound-nested-values.yaml"
email_outbound_nested_render="$render_dir/email-outbound-nested-render.yaml"
email_outbound_nested_worker_config="$render_dir/email-outbound-nested-worker-config.yaml"
email_outbound_nested_server_deployment="$render_dir/email-outbound-nested-server-deployment.yaml"

extract_document ConfigMap witself-server "$default_render" "$default_server_config"
extract_document Deployment witself-server "$default_render" "$default_server_deployment"
extract_document ConfigMap witself-server "$gcp_render" "$gcp_server_config"
extract_document ConfigMap witself-server "$billing_render" "$billing_server_config"
extract_document Deployment witself-server "$billing_render" "$billing_server_deployment"
extract_document Application witself-server "$billing_apps_render" "$billing_server_application"
extract_application_helm_values "$billing_server_application" "$billing_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$billing_nested_values" >"$billing_nested_render"
extract_document ConfigMap witself-server "$billing_nested_render" "$billing_nested_server_config"
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
extract_document ConfigMap witself-worker "$email_outbound_render" "$email_outbound_worker_config"
extract_document Deployment witself-worker "$email_outbound_render" "$email_outbound_worker_deployment"
extract_document Application witself-server "$email_outbound_apps_render" "$email_outbound_server_application"
extract_application_helm_values "$email_outbound_server_application" "$email_outbound_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_outbound_nested_values" >"$email_outbound_nested_render"
extract_document ConfigMap witself-worker "$email_outbound_nested_render" "$email_outbound_nested_worker_config"
extract_document Deployment witself-server "$email_outbound_nested_render" "$email_outbound_nested_server_deployment"
extract_document ConfigMap witself-server "$email_production_render" "$email_production_server_config"
extract_document Deployment witself-server "$email_production_render" "$email_production_server_deployment"
extract_document Application witself-server "$email_production_apps_render" "$email_production_server_application"
extract_application_helm_values "$email_production_server_application" "$email_production_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_production_nested_values" >"$email_production_nested_render"
extract_document ConfigMap witself-server "$email_production_nested_render" "$email_production_nested_config"
extract_document Deployment witself-server "$email_production_nested_render" "$email_production_nested_deployment"
extract_document Application witself-server "$email_production_pre245_apps_render" "$email_production_pre245_server_application"
extract_application_helm_values "$email_production_pre245_server_application" "$email_production_pre245_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_production_nested_values" \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=witself-agent-email-retry-canary-v2 \
  >"$email_production_retry_name_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$email_production_nested_values" \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.key=canary_agent_id \
  >"$email_production_retry_key_render"
extract_document ConfigMap witself-server "$email_production_retry_name_render" "$email_production_retry_name_config"
extract_document Deployment witself-server "$email_production_retry_name_render" "$email_production_retry_name_deployment"
extract_document ConfigMap witself-server "$email_production_retry_key_render" "$email_production_retry_key_config"
extract_document Deployment witself-server "$email_production_retry_key_render" "$email_production_retry_key_deployment"
for checksum_pair in \
  "$default_server_config:$default_server_deployment" \
  "$gcp_server_config:$gcp_server_deployment" \
  "$billing_server_config:$billing_server_deployment" \
  "$email_production_server_config:$email_production_server_deployment" \
  "$email_production_nested_config:$email_production_nested_deployment" \
  "$email_production_retry_name_config:$email_production_retry_name_deployment" \
  "$email_production_retry_key_config:$email_production_retry_key_deployment"; do
  config_document="${checksum_pair%%:*}"
  deployment_document="${checksum_pair#*:}"
  config_digest="$(server_config_checksum "$config_document")"
  deployment_digest="$(server_config_checksum "$deployment_document")"
  if [[ ! "$config_digest" =~ ^[0-9a-f]{64}$ || "$deployment_digest" != "$config_digest" ]]; then
    echo "server ConfigMap and Deployment do not share one exact configuration checksum" >&2
    exit 1
  fi
done
extract_document Deployment "$long_fullname" "$long_name_render" "$render_dir/long-name-server-deployment.yaml"
extract_document Deployment "$long_worker_fullname" "$long_name_render" "$render_dir/long-name-worker-deployment.yaml"
extract_document Service "$long_worker_metrics_fullname" "$long_name_render" "$render_dir/long-name-worker-metrics-service.yaml"
extract_document Application witself-server "$live_apps_render" "$live_server_application"
agent_email_rate_cleanup_server_application="$render_dir/agent-email-rate-cleanup-server-application.yaml"
extract_document Application witself-server "$agent_email_rate_cleanup_apps_render" "$agent_email_rate_cleanup_server_application"
extract_application_helm_values "$live_server_application" "$live_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$live_nested_values" >"$live_nested_render"
extract_document ConfigMap witself-server "$live_nested_render" "$live_nested_server_config"
extract_document ConfigMap witself-worker "$live_nested_render" "$live_nested_worker_config"
extract_document Deployment witself-worker "$live_nested_render" "$live_nested_worker_deployment"
if grep -Fqx 'kind: ServiceMonitor' "$live_nested_render"; then
  echo "default app-of-apps values unexpectedly rendered a ServiceMonitor" >&2
  exit 1
fi
extract_document Application witself-server "$apps_monitor_render" "$apps_monitor_application"
extract_application_helm_values "$apps_monitor_application" "$apps_monitor_nested_values"
helm template witself-server "$server_chart" --namespace witself \
  --values "$apps_monitor_nested_values" >"$apps_monitor_nested_render"
extract_document ServiceMonitor witself-server "$apps_monitor_nested_render" "$apps_monitor_server"
extract_document ServiceMonitor witself-worker "$apps_monitor_nested_render" "$apps_monitor_worker"
require_line "      interval: 45s" "$apps_monitor_server"
require_line "      scrapeTimeout: 12s" "$apps_monitor_server"
require_line "    monitor: server" "$apps_monitor_server"
require_line "        - replacement: server" "$apps_monitor_server"
require_line "          targetLabel: service_role" "$apps_monitor_server"
require_line "        - action: keep" "$apps_monitor_server"
require_line "          regex: witself_server_.+" "$apps_monitor_server"
require_line "          sourceLabels:" "$apps_monitor_server"
require_line "          - __name__" "$apps_monitor_server"
require_line "      interval: 50s" "$apps_monitor_worker"
require_line "      scrapeTimeout: 15s" "$apps_monitor_worker"
require_line "    monitor: worker" "$apps_monitor_worker"
require_line "        - replacement: worker" "$apps_monitor_worker"
require_line "          targetLabel: service_role" "$apps_monitor_worker"
require_line "        - action: keep" "$apps_monitor_worker"
require_line "          regex: witself_worker_.+" "$apps_monitor_worker"
require_line "          sourceLabels:" "$apps_monitor_worker"
require_line "          - __name__" "$apps_monitor_worker"
extract_document Application witself-server "$civo_apps_render" "$civo_server_application"
extract_document Application witself-server "$civo_backup_apps_render" "$civo_backup_server_application"
extract_document Application witself-server "$civo_use1_apps_render" "$civo_use1_server_application"
extract_document Application witself-postgresql "$civo_apps_render" "$civo_postgres_application"
extract_document Application witself-postgresql "$civo_backup_apps_render" "$civo_backup_postgres_application"
extract_document Application witself-postgresql "$civo_use1_apps_render" "$civo_use1_postgres_application"
extract_document Application witself-postgresql "$civo_default_preset_apps_render" "$civo_default_preset_postgres_application"
require_line "          resourcesPreset: micro" "$civo_postgres_application"
reject_line "          resourcesPreset: nano" "$civo_postgres_application"
require_line "          resourcesPreset: nano" "$civo_backup_postgres_application"
reject_line "          resourcesPreset: micro" "$civo_backup_postgres_application"
require_line "          resourcesPreset: nano" "$civo_use1_postgres_application"
reject_line "          resourcesPreset: micro" "$civo_use1_postgres_application"
require_line "          resourcesPreset: nano" "$civo_default_preset_postgres_application"
extract_application_helm_values "$civo_server_application" "$civo_server_nested_values"
civo_server_chart_version="$(read_witself_server_semver_scalar chartVersion "$civo_cell")"
civo_server_image_tag="$(read_witself_server_semver_scalar imageTag "$civo_cell")"
require_line "    targetRevision: \"${civo_server_chart_version}\"" "$civo_server_application"
require_line "          tag: ${civo_server_image_tag}" "$civo_server_application"
require_sequence "$civo_server_application" \
  "          providerEventTokenSecret:" \
  "            key: token" \
  "            name: witself-agent-email-provider-event-v2"
require_sequence "$civo_server_application" \
  "          agentEmailOutbound:" \
  "            batchSize: 10" \
  "            batchTimeout: 30s" \
  "            dispatchAudience: witself-agent-email-send" \
  "            dispatchEndpoint: https://witself-agent-email-send.witwave.workers.dev/v1/dispatch" \
  "            dispatchKeyID: civo-sandbox-usw2-dev-2026-08" \
  "            dispatchPrivateKeySecret:" \
  "              key: private-key" \
  "              name: witself-agent-email-outbound-dispatch-v1" \
  "            enabled: true"
# The app-of-apps chart is reconciled before each child chart pin advances.
# Forward the value only to cells whose child chart accepts it. The two live
# v0.0.224 cells include it, while the configured-but-unprovisioned v0.0.223
# cell remains a compatibility check for omission.
require_line "          messageRateBucketCleanup:" "$civo_server_application"
require_line "          messageRateBucketCleanup:" "$civo_backup_server_application"
reject_line "          messageRateBucketCleanup:" "$civo_use1_server_application"
# The email-specific cleanup contract first belongs to the v0.0.226 chart.
# Both provisioned Civo cells have advanced to v0.0.226. The older GCP and
# configured-only Civo cells remain compatibility checks for omission.
require_line "          agentEmailRateBucketCleanup:" "$agent_email_rate_cleanup_server_application"
reject_line "          agentEmailRateBucketCleanup:" "$live_server_application"
require_line "          agentEmailRateBucketCleanup:" "$civo_server_application"
require_line "          agentEmailRateBucketCleanup:" "$civo_backup_server_application"
reject_line "          agentEmailRateBucketCleanup:" "$civo_use1_server_application"
helm template witself-server "$server_chart" --namespace witself \
  --values "$civo_server_nested_values" >"$civo_server_nested_render"
extract_document ConfigMap witself-server "$civo_server_nested_render" "$civo_server_config"
extract_document ConfigMap witself-worker "$civo_server_nested_render" "$civo_worker_config"
extract_document Deployment witself-server "$civo_server_nested_render" "$civo_server_deployment"
extract_document Deployment witself-worker "$civo_server_nested_render" "$civo_worker_deployment"

require_line "  replicas: 1" "$civo_server_deployment"
require_line "  replicas: 2" "$civo_worker_deployment"
require_line '  WITSELF_CELL_NAME: "civo-sandbox-usw2-dev"' "$civo_server_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED: "true"' "$civo_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT: "https://witself-agent-email-send.witwave.workers.dev/v1/dispatch"' "$civo_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE: "witself-agent-email-send"' "$civo_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID: "civo-sandbox-usw2-dev-2026-08"' "$civo_worker_config"
require_sequence "$civo_server_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  '                  name: "witself-agent-email-provider-event-v2"' \
  '                  key: "token"'
reject_line "            - name: WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY" "$civo_server_deployment"
reject_line '                  name: "witself-agent-email-outbound-dispatch-v1"' "$civo_server_deployment"
reject_line "            - name: WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN" "$civo_worker_deployment"
reject_line '                  name: "witself-agent-email-provider-event-v2"' "$civo_worker_deployment"
require_sequence "$civo_worker_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  '                  name: "witself-agent-email-outbound-dispatch-v1"' \
  '                  key: "private-key"'

# Billing discovery is absent by default, forwarded only when explicitly set,
# and server-only. An explicit empty value must preserve the exact portable
# render and its stable configuration checksum.
if ! cmp -s "$default_render" "$billing_empty_render"; then
  echo "an explicit empty billing endpoint changed the default render" >&2
  exit 1
fi
for dark_billing_config in \
  "$default_server_config" \
  "$gcp_server_config" \
  "$live_nested_server_config"; do
  reject_line "  WITSELF_BILLING_ENDPOINT:" "$dark_billing_config"
done
reject_line "        billing:" "$live_server_application"
# The Stripe cutover made managed billing part of the committed serving state
# on both civo cells. Guard the live direction: a change that silently drops
# the billing endpoint from a serving cell must fail here, not be discovered
# as a missing capability in production.
require_line '  WITSELF_BILLING_ENDPOINT: "https://self.witwave.ai"' \
  "$civo_server_config"
require_line '        billing:' "$civo_server_application"
require_line '          endpoint: https://self.witwave.ai' \
  "$civo_server_application"
require_line '        billing:' "$civo_backup_server_application"
require_line '          endpoint: https://self.witwave.ai' \
  "$civo_backup_server_application"
require_line '  WITSELF_BILLING_ENDPOINT: "https://self.witwave.ai"' \
  "$billing_server_config"
require_line '        billing:' "$billing_server_application"
require_line '          endpoint: https://self.witwave.ai' \
  "$billing_server_application"
require_line '  WITSELF_BILLING_ENDPOINT: "https://self.witwave.ai"' \
  "$billing_nested_server_config"
if [[ "$(grep -c 'WITSELF_BILLING_ENDPOINT' "$billing_nested_render")" -ne 1 ]]; then
  echo "billing endpoint was not isolated to the server ConfigMap" >&2
  exit 1
fi
if [[ "$(server_config_checksum "$default_server_config")" == \
      "$(server_config_checksum "$billing_server_config")" ]]; then
  echo "a configured billing endpoint did not change the server configuration checksum" >&2
  exit 1
fi

# Portable defaults keep the API rollout-safe and fail closed on a worker that
# has no shared database Secret.
require_line "  minReadySeconds: 10" "$default_server_deployment"
require_line "    type: RollingUpdate" "$default_server_deployment"
require_line "      maxSurge: 1" "$default_server_deployment"
require_line "      maxUnavailable: 0" "$default_server_deployment"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_CELL_NAME: ""' "$default_server_config"
require_line '  WITSELF_BACKUP_VALIDATION_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_ADMISSION_BYTES: "3221225472"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_ADMISSION_ROWS: "25000"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_HARD_BYTES: "4294967296"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_HARD_ROWS: "100000"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$default_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED: "false"' "$default_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$default_server_config")" -ne 6 ]]; then
  echo "default render did not expose exactly four cell bounds and two disabled receive gates" >&2
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
extract_document ConfigMap witself-server "$backup_validation_render" \
  "$render_dir/backup-validation-server-config.yaml"
require_line '  WITSELF_BACKUP_VALIDATION_ENABLED: "true"' \
  "$render_dir/backup-validation-server-config.yaml"
extract_document Deployment witself-server "$backup_validation_render" \
  "$render_dir/backup-validation-server-deployment.yaml"
require_sequence "$render_dir/backup-validation-server-deployment.yaml" \
  "            - name: WITSELF_BACKUP_TOKEN" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  '                  name: "witself-backup"' \
  '                  key: "backup_token"'
extract_document Application witself-server "$backup_validation_apps_render" \
  "$render_dir/backup-validation-server-application.yaml"
extract_application_helm_values \
  "$render_dir/backup-validation-server-application.yaml" \
  "$render_dir/backup-validation-nested-values.yaml"
helm template witself-server "$server_chart" --namespace witself \
  --values "$render_dir/backup-validation-nested-values.yaml" \
  >"$render_dir/backup-validation-nested.yaml"
extract_document ConfigMap witself-server \
  "$render_dir/backup-validation-nested.yaml" \
  "$render_dir/backup-validation-nested-config.yaml"
require_line '  WITSELF_BACKUP_VALIDATION_ENABLED: "true"' \
  "$render_dir/backup-validation-nested-config.yaml"
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
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_ENABLED: "true"' "$gcp_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_SIZE: "10000"' "$gcp_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_INTERVAL: "1m"' "$gcp_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT: "10s"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_ENABLED: "true"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_SIZE: "10000"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_INTERVAL: "1m"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT: "10s"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED: "false"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE: "witself-agent-email-send"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_SIZE: "10"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_INTERVAL: "2s"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_BATCH_TIMEOUT: "30s"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_PROVIDER_TIMEOUT: "20s"' "$gcp_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$gcp_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_BATCH_TIMEOUT: "2m"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_ENABLED: "false"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_MODE: "preview"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_BATCH_SIZE: "25"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_INTERVAL: "5m"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_BATCH_TIMEOUT: "2m"' "$gcp_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED: "true"' "$email_outbound_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT: "https://send.example.test/v1/dispatch"' "$email_outbound_worker_config"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID: "founder-cell"' "$email_outbound_worker_config"
require_sequence "$email_outbound_worker_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  '                  name: "witself-email-dispatch"' \
  '                  key: "private-key"'
require_line "          agentEmailOutbound:" "$email_outbound_server_application"
require_line '  WITSELF_AGENT_EMAIL_OUTBOUND_ENABLED: "true"' "$email_outbound_nested_worker_config"
require_sequence "$email_outbound_nested_server_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  '                  name: "witself-email-provider-events"' \
  '                  key: "token"'
if grep -Eq 'WITSELF_(API_ADDR|BOOTSTRAP|PROVISION|AGENT_EMAIL_(RECEIVE|PILOT|RETRY|RELAY)|BACKEND_KIND|FACT_DELETION|AVATAR_PAYLOAD)' \
  "$gcp_worker_config" "$gcp_worker_deployment"; then
  echo "worker received API/bootstrap/provision/email-only configuration" >&2
  exit 1
fi
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "false"' "$gcp_server_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$gcp_server_config"

# The exact live app-of-apps payload must satisfy the released server chart,
# render two worker replicas, keep API loops off, and preserve enforcement.
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "false"' "$live_nested_server_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "false"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED: "false"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_ADMISSION_BYTES: "3221225472"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_ADMISSION_ROWS: "25000"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_HARD_BYTES: "4294967296"' "$live_nested_server_config"
require_line '  WITSELF_AGENT_EMAIL_CELL_STORAGE_HARD_ROWS: "100000"' "$live_nested_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$live_nested_server_config")" -ne 6 ]]; then
  echo "live GCP desired state did not expose exactly four cell bounds and two disabled receive gates" >&2
  exit 1
fi
require_line '  WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_SIZE: "10000"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_INTERVAL: "1m"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT: "10s"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_SIZE: "10000"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_INTERVAL: "1m"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_BATCH_TIMEOUT: "10s"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RETENTION_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RETENTION_MODE: "enforce"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RETENTION_BATCH_SIZE: "25"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RETENTION_INTERVAL: "5m"' "$live_nested_worker_config"
require_line '  WITSELF_MESSAGE_RETENTION_BATCH_TIMEOUT: "2m"' "$live_nested_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_MODE: "enforce"' "$live_nested_worker_config"
require_line '  WITSELF_TRANSCRIPT_RETENTION_BATCH_TIMEOUT: "2m"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_ENABLED: "true"' "$live_nested_worker_config"
require_line '  WITSELF_AGENT_EMAIL_RETENTION_MODE: "enforce"' "$live_nested_worker_config"
require_line "  replicas: 2" "$live_nested_worker_deployment"

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
  "HTTP billing endpoint" \
  --set-string billing.endpoint=http://self.witwave.ai
expect_server_template_failure_message \
  "HTTP billing endpoint with schema validation bypassed" \
  "billing.endpoint must be empty or a canonical HTTPS URL" \
  --skip-schema-validation \
  --set-string billing.endpoint=http://self.witwave.ai
expect_server_template_failure \
  "credential-bearing billing endpoint" \
  --set-string billing.endpoint=https://operator:secret@self.witwave.ai
expect_server_template_failure \
  "billing endpoint containing a control character" \
  --set-string $'billing.endpoint=https://self.witwave.ai/control\nplane'
expect_server_template_failure \
  "billing endpoint containing an encoded control character" \
  --set-string billing.endpoint=https://self.witwave.ai/%0aescape
expect_server_template_failure \
  "billing endpoint containing a backslash" \
  --set-string 'billing.endpoint=https://self.witwave.ai\\@forged.example'
expect_server_template_failure \
  "billing endpoint containing a query" \
  --set-string 'billing.endpoint=https://self.witwave.ai?tenant=founder'
expect_apps_template_failure \
  "managed billing endpoint with an old child image" \
  "apps.witselfServer.billing.endpoint requires chart and image v0.0.255 or newer" \
  --values "$civo_cell" \
  --set apps.witselfServer.chartVersion=0.0.255 \
  --set apps.witselfServer.imageTag=0.0.254 \
  --set-string apps.witselfServer.billing.endpoint=https://self.witwave.ai
expect_apps_template_failure \
  "unsafe managed billing endpoint" \
  "apps.witselfServer.billing.endpoint must be empty or a canonical HTTPS URL" \
  --values "$civo_cell" \
  --set apps.witselfServer.chartVersion=0.0.255 \
  --set apps.witselfServer.imageTag=0.0.255 \
  --set-string apps.witselfServer.billing.endpoint=https://self.witwave.ai/%0aescape
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
  "legacy top-level agent-email retention values" \
  --set agentEmailRetention.enabled=true
expect_server_template_failure \
  "agent-email cell byte admission at hard boundary" \
  --set agentEmail.cellStorage.admissionBytes=4294967296
expect_server_template_failure \
  "agent-email cell row admission at hard boundary" \
  --set agentEmail.cellStorage.admissionRows=100000
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
  "zero message-rate bucket cleanup batch" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.batchSize=0
expect_server_template_failure \
  "oversized message-rate bucket cleanup batch" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.batchSize=10001
expect_server_template_failure \
  "undersized message-rate bucket cleanup interval" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.interval=59s
expect_server_template_failure \
  "oversized message-rate bucket cleanup interval" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.interval=25h
expect_server_template_failure \
  "undersized message-rate bucket cleanup timeout" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.batchTimeout=999ms
expect_server_template_failure \
  "oversized message-rate bucket cleanup timeout" \
  --values "$gcp_profile" \
  --set worker.messageRateBucketCleanup.batchTimeout=6m
expect_server_template_failure \
  "zero agent-email-rate bucket cleanup batch" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.batchSize=0
expect_server_template_failure \
  "oversized agent-email-rate bucket cleanup batch" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.batchSize=10001
expect_server_template_failure \
  "undersized agent-email-rate bucket cleanup interval" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.interval=59s
expect_server_template_failure \
  "oversized agent-email-rate bucket cleanup interval" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.interval=25h
expect_server_template_failure \
  "undersized agent-email-rate bucket cleanup timeout" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.batchTimeout=999ms
expect_server_template_failure \
  "oversized agent-email-rate bucket cleanup timeout" \
  --values "$gcp_profile" \
  --set worker.agentEmailRateBucketCleanup.batchTimeout=6m
expect_server_template_failure \
  "agent-email outbound without dispatch key Secret" \
  --values "$gcp_profile" \
  --set worker.agentEmailOutbound.enabled=true \
  --set worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set worker.agentEmailOutbound.dispatchKeyID=founder-cell
expect_server_template_failure \
  "agent-email outbound without HTTPS endpoint" \
  --values "$gcp_profile" \
  --set worker.agentEmailOutbound.enabled=true \
  --set worker.agentEmailOutbound.dispatchEndpoint=http://send.example.test/v1/dispatch \
  --set worker.agentEmailOutbound.dispatchKeyID=founder-cell \
  --set worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch
expect_server_template_failure \
  "agent-email outbound without dispatch key id" \
  --values "$gcp_profile" \
  --set worker.agentEmailOutbound.enabled=true \
  --set worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch
expect_apps_template_failure \
  "agent-email outbound with an old app image" \
  "apps.witselfServer.worker.agentEmailOutbound requires chart and image v0.0.245 or newer" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.chartVersion=0.0.245 \
  --set apps.witselfServer.imageTag=0.0.244 \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=true \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchKeyID=founder-cell \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch
expect_apps_template_failure \
  "agent-email outbound with an old app chart" \
  "apps.witselfServer.worker.agentEmailOutbound requires chart and image v0.0.245 or newer" \
  --values "$gcp_cell" \
  --values "$apps_profile" \
  --set apps.witselfServer.chartVersion=0.0.244 \
  --set apps.witselfServer.imageTag=0.0.245 \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=true \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchEndpoint=https://send.example.test/v1/dispatch \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchKeyID=founder-cell \
  --set apps.witselfServer.worker.agentEmailOutbound.dispatchPrivateKeySecret.name=witself-email-dispatch
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
  "unknown agent-email retention mode" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.mode=delete
expect_server_template_failure \
  "oversized agent-email retention batch" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.batchSize=101
expect_server_template_failure \
  "undersized agent-email retention interval" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.interval=59s
expect_server_template_failure \
  "oversized agent-email retention interval" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.interval=25h
expect_server_template_failure \
  "undersized agent-email retention batch timeout" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.batchTimeout=9s
expect_server_template_failure \
  "oversized agent-email retention batch timeout" \
  --values "$gcp_profile" \
  --set worker.agentEmailRetention.batchTimeout=6m
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
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED: "false"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_DOMAIN: "witmail.net"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS: "agent-mail.witwave.ai"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AUDIENCE: "gcp-sandbox-use1-dev"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_REALM_ID: "realm_aaaaaaaaaaaaaaaa"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS: "agent_aaaaaaaaaaaaaaaa,agent_bbbbbbbbbbbbbbbb,agent_cccccccccccccccc,agent_dddddddddddddddd,agent_eeeeeeeeeeeeeeee"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID: "agent_aaaaaaaaaaaaaaaa"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON: "{\"pilot-2026-07\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}"' "$email_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW: "5m"' "$email_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$email_server_config")" -ne 14 ]]; then
  echo "enabled pilot with cell bounds, legacy compatibility, and a retry canary did not render exactly fourteen agent-email variables" >&2
  exit 1
fi
if grep -Eq 'WITSELF_AGENT_EMAIL_.*PRIVATE|RELAY_ED25519_PRIVATE_KEY|relayPrivateKey' \
  "$email_pilot_render" "$email_pilot_apps_render" "$email_pilot_legacy_apps_render" \
  "$email_production_render" "$email_production_apps_render"; then
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
if helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" \
  --set-json 'agentEmail.receivePilot.acceptedLegacyDomains=["one.example","two.example"]' \
  >/dev/null 2>&1; then
  echo "enabled pilot accepted more than one legacy domain" >&2
  exit 1
fi
if helm template witself-server "$server_chart" --namespace witself \
  --values "$email_pilot_profile" \
  --set-json 'agentEmail.receivePilot.acceptedLegacyDomains=["witmail.net"]' \
  >/dev/null 2>&1; then
  echo "enabled pilot accepted its primary domain as a legacy domain" >&2
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
if helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_email_pilot_profile" \
  --set-json 'apps.witselfServer.agentEmail.receivePilot.acceptedLegacyDomains=["one.example","two.example"]' \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted more than one legacy domain" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" \
  --values "$apps_email_pilot_profile" \
  --set-json 'apps.witselfServer.agentEmail.receivePilot.acceptedLegacyDomains=["witmail.net"]' \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted its primary domain as a legacy domain" >&2
  exit 1
fi
require_sequence "$email_pilot_apps_render" \
  "        agentEmail:" \
  "          receivePilot:" \
  "            acceptedLegacyDomains:" \
  "            - agent-mail.witwave.ai" \
  "            agentIDs:" \
  "            - agent_aaaaaaaaaaaaaaaa" \
  "            - agent_bbbbbbbbbbbbbbbb" \
  "            - agent_cccccccccccccccc" \
  "            - agent_dddddddddddddddd" \
  "            - agent_eeeeeeeeeeeeeeee" \
  "            audience: gcp-sandbox-use1-dev" \
  "            domain: witmail.net" \
  "            enabled: true" \
  "            realmID: realm_aaaaaaaaaaaaaaaa" \
  "            relayPublicKeysJSON: '{\"pilot-2026-07\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}'" \
  "            relayReplayWindow: 5m" \
  "            retryCanaryAgentID: agent_aaaaaaaaaaaaaaaa"
require_sequence "$email_pilot_legacy_apps_render" \
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
  "            realmID: realm_aaaaaaaaaaaaaaaa"
if grep -Fq 'acceptedLegacyDomains:' "$email_pilot_legacy_apps_render"; then
  echo "pre-0.0.232 child chart received the unsupported legacy-domain list" >&2
  exit 1
fi
for mixed_email_render in \
  "$email_pilot_new_chart_old_image_render" \
  "$email_pilot_old_chart_new_image_render"; do
  require_line "            domain: agent-mail.witwave.ai" "$mixed_email_render"
  if grep -Fq 'acceptedLegacyDomains:' "$mixed_email_render"; then
    echo "mixed pre/post-0.0.232 chart and image pins activated the domain cutover" >&2
    exit 1
  fi
done

# Production receive replaces the fixed realm/agent pilot allowlist with one
# exact, bounded account cohort. Portable installs retain the literal list;
# managed app-of-apps values pass only Kubernetes Secret references. Production
# receive starts at v0.0.241; the private retry-canary Secret starts at v0.0.245.
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED: "true"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN: "witmail.net"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS: "agent-mail.witwave.ai"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE: "civo-sandbox-usw2-dev"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS: "acc_aaaaaaaaaaaaaaaa,acc_bbbbbbbbbbbbbbbb"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID: "agent_aaaaaaaaaaaaaaaa"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON: "{\"route-2026-08\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}"' "$email_production_server_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW: "5m"' "$email_production_server_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$email_production_server_config")" -ne 13 ]]; then
  echo "production receive with cell bounds did not render exactly thirteen agent-email variables" >&2
  exit 1
fi
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED: "false"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED: "true"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN: "witmail.net"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS: "agent-mail.witwave.ai"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE: "civo-sandbox-usw2-dev"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON: "{\"route-2026-08\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}"' "$email_production_nested_config"
require_line '  WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW: "5m"' "$email_production_nested_config"
reject_line 'WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS' "$email_production_nested_config"
reject_line 'WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID' "$email_production_nested_config"
if [[ "$(grep -c '^  WITSELF_AGENT_EMAIL_' "$email_production_nested_config")" -ne 11 ]]; then
  echo "Secret-backed production receive with cell bounds did not render exactly eleven non-secret agent-email variables" >&2
  exit 1
fi
require_sequence "$email_production_nested_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  "                  name: \"witself-agent-email-receive-cohort-v1\"" \
  "                  key: \"account_ids\"" \
  "            - name: WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  "                  name: \"witself-agent-email-retry-canary-v1\"" \
  "                  key: \"agent_id\""
reject_line 'WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS' "$email_production_server_deployment"
if grep -Eq 'acc_aaaaaaaaaaaaaaaa|agent_aaaaaaaaaaaaaaaa' "$email_production_apps_render"; then
  echo "private production receive account or canary IDs leaked into the app-of-apps render" >&2
  exit 1
fi
require_sequence "$email_production_server_application" \
  "        agentEmail:" \
  "          providerEventTokenSecret:" \
  "            key: token" \
  "            name: witself-agent-email-provider-event-v2" \
  "          receivePilot:" \
  "            acceptedLegacyDomains: []" \
  "            agentIDs: []" \
  "            audience: \"\"" \
  "            domain: \"\"" \
  "            enabled: false" \
  "            realmID: \"\"" \
  "            relayPublicKeysJSON: \"\"" \
  "            relayReplayWindow: 5m" \
  "            retryCanaryAgentID: \"\"" \
  "          receiveProduction:" \
  "            acceptedLegacyDomains:" \
  "            - agent-mail.witwave.ai" \
  "            accountIDs: []" \
  "            accountIDsExistingSecret:" \
  "              key: account_ids" \
  "              name: witself-agent-email-receive-cohort-v1" \
  "            audience: civo-sandbox-usw2-dev" \
  "            domain: witmail.net" \
  "            enabled: true" \
  "            relayPublicKeysJSON: '{\"route-2026-08\":\"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=\"}'" \
  "            relayReplayWindow: 5m" \
  "            retryCanaryAgentID: \"\"" \
  "            retryCanaryAgentIDExistingSecret:" \
  "              key: agent_id" \
  "              name: witself-agent-email-retry-canary-v1"

# Strict pre-0.0.245 child schemas never see the new field when it is empty;
# production receive and its existing cohort Secret remain available at 0.0.244.
require_line '  receiveProduction:' "$email_production_pre245_nested_values"
if grep -Fq 'retryCanaryAgentIDExistingSecret:' "$email_production_pre245_nested_values"; then
  echo "pre-0.0.245 child values received the unsupported retry-canary Secret field" >&2
  exit 1
fi

# The Secret reference itself is part of both deployment checksums. Replacing
# either immutable Secret name or key must roll the API pods without exposing
# the selected agent ID in the ConfigMap.
baseline_server_checksum="$(server_config_checksum "$email_production_nested_deployment")"
baseline_pod_checksum="$(config_checksum "$email_production_nested_deployment")"
for retry_variant_deployment in \
  "$email_production_retry_name_deployment" \
  "$email_production_retry_key_deployment"; do
  variant_server_checksum="$(server_config_checksum "$retry_variant_deployment")"
  variant_pod_checksum="$(config_checksum "$retry_variant_deployment")"
  if [ -z "$baseline_server_checksum" ] || [ -z "$baseline_pod_checksum" ] ||
     [ "$variant_server_checksum" = "$baseline_server_checksum" ] ||
     [ "$variant_pod_checksum" = "$baseline_pod_checksum" ]; then
    echo "retry-canary Secret reference change did not roll the API pod checksums" >&2
    exit 1
  fi
done
require_sequence "$email_production_retry_name_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  "                  name: \"witself-agent-email-retry-canary-v2\"" \
  "                  key: \"agent_id\""
require_sequence "$email_production_retry_key_deployment" \
  "            - name: WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID" \
  "              valueFrom:" \
  "                secretKeyRef:" \
  "                  name: \"witself-agent-email-retry-canary-v1\"" \
  "                  key: \"canary_agent_id\""
reject_line 'WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID' "$email_production_retry_name_config"
reject_line 'WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID' "$email_production_retry_key_config"

expect_server_template_failure \
  "simultaneous pilot and production receive" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set agentEmail.receivePilot.enabled=true
expect_server_template_failure \
  "production receive with pre-0.0.241 image" \
  --values "$email_production_profile" \
  --set image.tag=0.0.240
expect_server_template_failure \
  "empty production receive cohort" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]'
expect_server_template_failure \
  "production receive with both literal and Secret cohort sources" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=witself-agent-email-receive-cohort
expect_server_template_failure \
  "production receive with an invalid Secret name" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=Invalid_Secret
expect_server_template_failure \
  "duplicate production receive account" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set-json 'agentEmail.receiveProduction.accountIDs=["acc_aaaaaaaaaaaaaaaa","acc_aaaaaaaaaaaaaaaa"]'
expect_server_template_failure \
  "unsorted production receive cohort" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set-json 'agentEmail.receiveProduction.accountIDs=["acc_bbbbbbbbbbbbbbbb","acc_aaaaaaaaaaaaaaaa"]'
expect_server_template_failure \
  "wildcard production receive account" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set-json 'agentEmail.receiveProduction.accountIDs=["*"]'
expect_server_template_failure \
  "overlarge production receive cohort" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set 'agentEmail.receiveProduction.accountIDs[100]=acc_aaaaaaaaaaaaaaaa'
expect_server_template_failure \
  "invalid production retry canary" \
  --values "$email_production_profile" \
  --set image.tag=0.0.241 \
  --set agentEmail.receiveProduction.retryCanaryAgentID=agent_invalid
expect_server_template_failure \
  "production receive with both literal and Secret retry-canary sources" \
  --values "$email_production_profile" \
  --set image.tag=0.0.245 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=receive-cohort-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=retry-canary-v1
expect_server_template_failure \
  "production retry-canary Secret without a Secret-backed cohort" \
  --values "$email_production_profile" \
  --set image.tag=0.0.245 \
  --set agentEmail.receiveProduction.retryCanaryAgentID= \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=retry-canary-v1
expect_server_template_failure_message \
  "production retry-canary reusing the account-cohort Secret" \
  "agentEmail.receiveProduction retry-canary and account-cohort Secrets must have distinct names" \
  --values "$email_production_profile" \
  --set image.tag=0.0.245 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=shared-email-config-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentID= \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=shared-email-config-v1
expect_server_template_failure \
  "production retry-canary with an invalid Secret name" \
  --values "$email_production_profile" \
  --set image.tag=0.0.245 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=receive-cohort-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentID= \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=Invalid_Secret
expect_server_template_failure \
  "production retry-canary with an invalid Secret key" \
  --values "$email_production_profile" \
  --set image.tag=0.0.245 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=receive-cohort-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentID= \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=retry-canary-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.key=bad/key
expect_server_template_failure \
  "production retry-canary Secret with pre-0.0.245 image" \
  --values "$email_production_profile" \
  --set image.tag=0.0.244 \
  --set-json 'agentEmail.receiveProduction.accountIDs=[]' \
  --set agentEmail.receiveProduction.accountIDsExistingSecret.name=receive-cohort-v1 \
  --set agentEmail.receiveProduction.retryCanaryAgentID= \
  --set agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=retry-canary-v1

for unsafe_pin in chartVersion imageTag; do
  if helm template witself-apps "$apps_chart" \
    --values "$civo_cell" \
    --values "$apps_email_production_profile" \
    --set "apps.witselfServer.${unsafe_pin}=0.0.240" \
    --set apps.witselfServer.worker.agentEmailOutbound.enabled=false \
    --set-string apps.witselfServer.billing.endpoint= \
    >/dev/null 2>&1; then
    echo "app-of-apps enabled a retry-canary Secret with pre-v0.0.245 ${unsafe_pin}" >&2
    exit 1
  fi
done
expect_apps_template_failure \
  "app-of-apps pre-v0.0.241 production receive" \
  "apps.witselfServer.agentEmail.receiveProduction requires chart and image v0.0.241 or newer" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set apps.witselfServer.chartVersion=0.0.240 \
  --set apps.witselfServer.imageTag=0.0.240 \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=false \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=
expect_apps_template_failure \
  "app-of-apps pre-v0.0.245 retry-canary Secret" \
  "apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret requires chart and image v0.0.245 or newer" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set apps.witselfServer.chartVersion=0.0.244 \
  --set apps.witselfServer.imageTag=0.0.244 \
  --set apps.witselfServer.worker.agentEmailOutbound.enabled=false \
  --set-string apps.witselfServer.billing.endpoint=
expect_apps_template_failure \
  "app-of-apps retry-canary with an invalid Secret name" \
  "apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name must be a valid Kubernetes Secret name" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=Invalid_Secret
expect_apps_template_failure \
  "app-of-apps retry-canary with an invalid Secret key" \
  "apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.key must be a valid Kubernetes Secret data key" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.key=bad/key
expect_apps_template_failure \
  "app-of-apps retry-canary Secret without a Secret-backed cohort" \
  "apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret requires accountIDsExistingSecret.name" \
  --values "$civo_cell" \
  --set apps.witselfServer.chartVersion=0.0.245 \
  --set apps.witselfServer.imageTag=0.0.245 \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.accountIDsExistingSecret.name= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=retry-canary-v1
expect_apps_template_failure \
  "app-of-apps retry-canary reusing the account-cohort Secret" \
  "apps.witselfServer.agentEmail.receiveProduction retry-canary and account-cohort Secrets must have distinct names" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentIDExistingSecret.name=witself-agent-email-receive-cohort-v1
expect_apps_template_failure \
  "app-of-apps dark literal retry canary" \
  "apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentID must remain empty; managed cells require retryCanaryAgentIDExistingSecret" \
  --values "$civo_cell" \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentID=agent_aaaaaaaaaaaaaaaa
if helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.accountIDsExistingSecret.name= \
  --set-json 'apps.witselfServer.agentEmail.receiveProduction.accountIDs=["acc_bbbbbbbbbbbbbbbb","acc_aaaaaaaaaaaaaaaa"]' \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted a literal production receive cohort" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set-json 'apps.witselfServer.agentEmail.receiveProduction.accountIDs=["acc_aaaaaaaaaaaaaaaa"]' \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted a literal cohort alongside its Secret reference" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.accountIDsExistingSecret.name= \
  >/dev/null 2>&1; then
  echo "app-of-apps accepted enabled production receive without a cohort source" >&2
  exit 1
fi
if helm template witself-apps "$apps_chart" \
  --values "$civo_cell" \
  --values "$apps_email_production_profile" \
  --set-string apps.witselfServer.billing.endpoint= \
  --set apps.witselfServer.agentEmail.receiveProduction.retryCanaryAgentID=agent_aaaaaaaaaaaaaaaa \
  >/dev/null 2>&1; then
  echo "app-of-apps exposed a retry canary with a Secret-backed production cohort" >&2
  exit 1
fi

# The chart/image pin and the app-of-apps value-shape migration are atomic.
# Prove the API remains API-only while the nested worker contract carries both
# managed jobs and the two-replica availability settings.
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
require_sequence "$apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: false" \
  "        backend:"
require_sequence "$phase_b_apps_render" \
  "        avatar:" \
  "          payloadCompaction:" \
  "            enabled: true" \
  "        backend:"
require_sequence "$apps_render" \
  "        worker:" \
  "          agentEmailRetention:" \
  "            batchSize: 25" \
  "            batchTimeout: 2m" \
  "            enabled: true" \
  "            interval: 5m" \
  "            mode: enforce" \
  "          avatarStyleRollout:" \
  "            batchSize: 100" \
  "            batchTimeout: 30s" \
  "            enabled: true" \
  "            interval: 2s" \
  "          enabled: true" \
  "          messageRateBucketCleanup:" \
  "            batchSize: 10000" \
  "            batchTimeout: 10s" \
  "            enabled: true" \
  "            interval: 1m" \
  "          messageRetention:" \
  "            batchSize: 25" \
  "            batchTimeout: 2m" \
  "            enabled: true" \
  "            interval: 5m" \
  "            mode: enforce" \
  "          metrics:" \
  "            serviceMonitor:" \
  "              enabled: false" \
  "              interval: 30s" \
  "              labels: {}" \
  "              metricRelabelings: []" \
  "              relabelings: []" \
  "              scrapeTimeout: 10s" \
  "          minReadySeconds: 10"
require_sequence "$apps_render" \
  "          replicaCount: 2" \
  "          resources:" \
  "            limits:" \
  "              memory: 512Mi" \
  "            requests:" \
  "              cpu: 100m" \
  "              memory: 128Mi"
require_sequence "$apps_render" \
  "          terminationGracePeriodSeconds: 30" \
  "          topologySpreadConstraints:" \
  "          - labelSelector:" \
  "              matchLabels:" \
  "                app.kubernetes.io/component: worker" \
  "                app.kubernetes.io/instance: witself-server" \
  "                app.kubernetes.io/name: witself-worker"
extract_document ConfigMap witself-server "$phase_b_gcp_render" "$render_dir/phase-b-server-config.yaml"
require_line '  WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED: "true"' "$render_dir/phase-b-server-config.yaml"
require_sequence "$live_apps_render" \
  "          transcriptRetention:" \
  "            batchSize: 100" \
  "            batchTimeout: 2m" \
  "            enabled: true" \
  "            interval: 5m" \
  "            mode: enforce"
require_sequence "$retention_preview_apps_render" \
  "          transcriptRetention:" \
  "            batchSize: 100" \
  "            batchTimeout: 2m" \
  "            enabled: true" \
  "            interval: 5m" \
  "            mode: preview"
require_sequence "$retention_enforce_apps_render" \
  "          transcriptRetention:" \
  "            batchSize: 100" \
  "            batchTimeout: 2m" \
  "            enabled: true" \
  "            interval: 5m" \
  "            mode: enforce"

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

extract_document Deployment witself-server "$email_retention_preview_render" "$render_dir/email-preview-server-deployment.yaml"
extract_document Deployment witself-worker "$email_retention_preview_render" "$render_dir/email-preview-worker-deployment.yaml"
extract_document Deployment witself-server "$email_retention_enforce_render" "$render_dir/email-enforce-server-deployment.yaml"
extract_document Deployment witself-worker "$email_retention_enforce_render" "$render_dir/email-enforce-worker-deployment.yaml"
email_retention_preview_worker_checksum="$(config_checksum "$render_dir/email-preview-worker-deployment.yaml")"
email_retention_enforce_worker_checksum="$(config_checksum "$render_dir/email-enforce-worker-deployment.yaml")"
if [[ "$phase_a_worker_checksum" == "$email_retention_preview_worker_checksum" ||
  "$email_retention_preview_worker_checksum" == "$email_retention_enforce_worker_checksum" ]]; then
  echo "agent-email-retention phases did not produce distinct worker checksums" >&2
  exit 1
fi
if [[ "$phase_a_server_checksum" != "$(config_checksum "$render_dir/email-preview-server-deployment.yaml")" ]]; then
  echo "worker agent-email-retention preview unexpectedly restarted API pods" >&2
  exit 1
fi
if [[ "$phase_a_server_checksum" != "$(config_checksum "$render_dir/email-enforce-server-deployment.yaml")" ]]; then
  echo "worker agent-email-retention enforcement unexpectedly restarted API pods" >&2
  exit 1
fi

default_server_checksum="$(config_checksum "$default_server_deployment")"
extract_document Deployment witself-server "$email_pilot_render" "$render_dir/email-server-deployment.yaml"
email_server_checksum="$(config_checksum "$render_dir/email-server-deployment.yaml")"
if [[ -z "$default_server_checksum" || -z "$email_server_checksum" ||
  "$default_server_checksum" == "$email_server_checksum" ]]; then
  echo "agent-email compatibility activation did not restart API pods" >&2
  exit 1
fi

# PostgreSQL hardening must preserve serving-cell defaults while the backup
# cell exercises the pinned image and restricted policy. Inspect child values
# structurally without fetching the upstream chart during this offline gate.
ruby -ryaml -ropen3 - "$apps_chart" "$civo_cell" "$civo_backup_cell" \
  "$civo_apps_render" "$civo_backup_apps_render" <<'RUBY'
chart, serving_cell, backup_cell, serving_render, backup_render = ARGV
def check(message, condition)
  abort message unless condition
end
def application(documents, name)
  documents.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == name } || abort("missing Application/#{name}")
end
def child_values(documents, name = "witself-postgresql")
  YAML.safe_load(application(documents, name).dig("spec", "source", "helm", "values"), aliases: false)
end
def render(chart, cell, *overrides)
  args = ["helm", "template", "witself-apps", chart, "--values", cell]
  overrides.each { |override| args.concat(["--set", override]) }
  output, errors, status = Open3.capture3(*args)
  abort errors unless status.success?
  YAML.load_stream(output).compact
end

serving = YAML.load_stream(File.read(serving_render)).compact
backup = YAML.load_stream(File.read(backup_render)).compact
defaults = YAML.safe_load(File.read(File.join(chart, "values.yaml")), aliases: false).dig("apps", "civoPostgres")
check("PostgreSQL image defaults must inherit upstream values", defaults["image"].values.all? { |value| value == "" })
check("PostgreSQL mirror opt-in must default false", defaults["allowInsecureImages"] == false)
check("PostgreSQL metrics must default off", defaults.dig("metrics", "enabled") == false && defaults.dig("metrics", "serviceMonitor", "enabled") == false)
check("PostgreSQL policy defaults must retain upstream behavior", defaults["networkPolicy"] == {"enabled" => true, "allowExternal" => true})

[serving, backup].each do |documents|
  ["witself-server", "witself-postgresql"].each do |name|
    sync = application(documents, name).dig("spec", "syncPolicy")
    check("#{name}: retry/backoff contract changed", sync["retry"] == {"limit" => 20, "backoff" => {"duration" => "10s", "factor" => 2, "maxDuration" => "2m"}})
    check("#{name}: automated sync changed", sync["automated"] == {"prune" => true, "selfHeal" => true})
    check("#{name}: namespace/foreground sync options missing", sync["syncOptions"] == ["CreateNamespace=true", "PrunePropagationPolicy=foreground"])
  end
end

pg = child_values(serving)
check("serving PostgreSQL image must remain unpinned", !pg.key?("image"))
check("serving PostgreSQL must retain upstream image validation", !pg.key?("global"))
check("serving PostgreSQL metrics must remain disabled", pg["metrics"] == {"enabled" => false})
check("serving PostgreSQL policy must inherit upstream defaults", !pg.fetch("primary").key?("networkPolicy") && !pg.key?("extraDeploy"))
check("serving compaction must remain disabled", child_values(serving, "witself-server").dig("avatar", "payloadCompaction", "enabled") == false)

pg = child_values(backup)
check("backup PostgreSQL pin changed", pg["image"] == {"registry" => "registry-1.docker.io", "repository" => "bitnami/postgresql", "digest" => "sha256:a727ea9d5ceb64beb404afeb62a4b757fe3c33c2a09af86dc391a3a0dfed6049"})
check("backup PostgreSQL must retain upstream image validation", !pg.key?("global"))
check("backup has no monitoring stack", pg["metrics"] == {"enabled" => false})
check("backup compaction must be enabled", child_values(backup, "witself-server").dig("avatar", "payloadCompaction", "enabled") == true)
backup_config = YAML.safe_load(File.read(backup_cell), aliases: false)
serving_config = YAML.safe_load(File.read(serving_cell), aliases: false)
check("backup-only Metrics Server activation changed", backup_config.dig("platform", "metricsServer", "enabled") == true && serving_config.dig("platform", "metricsServer", "enabled") == false)
server_config = backup_config.dig("apps", "witselfServer")
check("backup replica/PDB/topology must stay unchanged", server_config["replicaCount"] == 1 && server_config.dig("podDisruptionBudget", "enabled") == false && server_config["topologySpreadConstraints"] == [] && server_config.dig("worker", "replicaCount") == 2 && server_config.dig("worker", "podDisruptionBudget", "enabled") == false && server_config.dig("worker", "topologySpreadConstraints") == [])

check("strict policy must disable the broader upstream policy", pg.dig("primary", "networkPolicy", "enabled") == false)
policies = pg.fetch("extraDeploy")
check("strict policy must create exactly one NetworkPolicy", policies.length == 1 && policies[0]["kind"] == "NetworkPolicy")
policy = policies[0]
check("strict policy must replace the upstream policy identity", policy["metadata"] == {"name" => "witself-postgresql", "namespace" => "witself"})
check("strict policy must target only this PostgreSQL primary", policy.dig("spec", "podSelector", "matchLabels") == {"app.kubernetes.io/name" => "postgresql", "app.kubernetes.io/instance" => "witself-postgresql", "app.kubernetes.io/component" => "primary"})
check("strict policy must retain open egress", policy.dig("spec", "policyTypes") == ["Ingress", "Egress"] && policy.dig("spec", "egress") == [{}])
ingress = policy.dig("spec", "ingress")
check("backup must expose only PostgreSQL through its policy", ingress.length == 1 && ingress[0]["ports"] == [{"protocol" => "TCP", "port" => 5432}])
peers = ingress[0].fetch("from")
expected = [
  ["witself", {"app.kubernetes.io/name" => "witself-server", "app.kubernetes.io/instance" => "witself-server", "app.kubernetes.io/component" => "server"}],
  ["witself", {"app.kubernetes.io/name" => "witself-worker", "app.kubernetes.io/instance" => "witself-server", "app.kubernetes.io/component" => "worker"}],
  ["monitoring", {"app.kubernetes.io/name" => "prometheus"}]
].map { |namespace, labels| {"namespaceSelector" => {"matchLabels" => {"kubernetes.io/metadata.name" => namespace}}, "podSelector" => {"matchLabels" => labels}} }
expected.concat([
  {"namespaceSelector" => {"matchLabels" => {"kubernetes.io/metadata.name" => "witself"}},
   "podSelector" => {"matchLabels" => {"app.kubernetes.io/name" => "witself-agent-email-operation", "app.kubernetes.io/component" => "one-shot"},
     "matchExpressions" => [{"key" => "witself.io/agent-email-operation", "operator" => "In", "values" => ["backfill", "canary-manifest"]}]}},
  {"namespaceSelector" => {"matchLabels" => {"kubernetes.io/metadata.name" => "witself"}},
   "podSelector" => {"matchLabels" => {"app.kubernetes.io/name" => "witself-agent-email-receipt-proof", "app.kubernetes.io/component" => "operator-proof",
     "app.kubernetes.io/managed-by" => "witself-operator", "witself.io/cell" => backup_config.dig("cell", "name")}}}
])
check("strict policy must admit only server, worker, monitoring scraper, and authorized email Jobs; generic client peers are forbidden", peers == expected)

pinned = child_values(render(chart, serving_cell,
  "apps.civoPostgres.image.registry=registry.example.com", "apps.civoPostgres.image.repository=mirror/postgresql",
  "apps.civoPostgres.image.tag=18.8.0", "apps.civoPostgres.image.digest=sha256:#{'a' * 64}"))
check("image fields must all forward exactly", pinned["image"] == {"registry" => "registry.example.com", "repository" => "mirror/postgresql", "tag" => "18.8.0", "digest" => "sha256:#{'a' * 64}"})
tagged = child_values(render(chart, serving_cell, "apps.civoPostgres.image.tag=18.8.0"))
check("empty image digest must be omitted", tagged["image"] == {"tag" => "18.8.0"})
monitored = child_values(render(chart, backup_cell, "apps.civoPostgres.metrics.enabled=true",
  "apps.civoPostgres.metrics.serviceMonitor.enabled=true", "apps.civoPostgres.metrics.serviceMonitor.interval=45s",
  "apps.civoPostgres.metrics.serviceMonitor.labels.team=platform"))
check("metrics and ServiceMonitor values must forward exactly", monitored["metrics"] == {"enabled" => true, "serviceMonitor" => {"enabled" => true, "namespace" => "witself", "interval" => "45s", "labels" => {"release" => "witself-monitoring", "team" => "platform"}}})
metrics_ingress = monitored.fetch("extraDeploy")[0].dig("spec", "ingress")
check("strict exporter ingress must admit only the metrics scraper", metrics_ingress.length == 2 && metrics_ingress[1] == {"ports" => [{"protocol" => "TCP", "port" => 9187}], "from" => [expected[2]]})
exporter_only = child_values(render(chart, serving_cell, "apps.civoPostgres.metrics.enabled=true"))
check("ServiceMonitor must stay disabled when only exporter is enabled", exporter_only.dig("metrics", "serviceMonitor", "enabled") == false)
disabled = child_values(render(chart, backup_cell, "apps.civoPostgres.networkPolicy.enabled=false"))
check("disabling networkPolicy must not leave restrictive extraDeploy policy", disabled.dig("primary", "networkPolicy", "enabled") == false && !disabled.key?("extraDeploy"))
_, errors, status = Open3.capture3("helm", "template", "witself-apps", chart, "--values", serving_cell, "--set", "apps.civoPostgres.metrics.serviceMonitor.enabled=true")
check("ServiceMonitor without exporter must be rejected", !status.success? && errors.include?("apps.civoPostgres.metrics.serviceMonitor.enabled requires apps.civoPostgres.metrics.enabled"))
puts "PostgreSQL deployment hardening rendering checks passed"
RUBY

echo "Helm rollout rendering checks passed"
