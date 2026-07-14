#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
server_chart="$repo_root/charts/witself-server"
apps_chart="$repo_root/.gitops/charts/apps"
gcp_profile="$server_chart/ci/gcp-rollout-values.yaml"
gcp_cell="$repo_root/.gitops/cells/gcp-sandbox-use1-dev/values.yaml"

default_render="$(mktemp)"
gcp_render="$(mktemp)"
apps_render="$(mktemp)"
trap 'rm -f "$default_render" "$gcp_render" "$apps_render"' EXIT

helm template witself-server "$server_chart" --namespace witself >"$default_render"
helm template witself-server "$server_chart" --namespace witself \
  --values "$gcp_profile" >"$gcp_render"
helm template witself-apps "$apps_chart" \
  --values "$gcp_cell" >"$apps_render"

require_line() {
  local expected="$1"
  local file="$2"
  if ! grep -Fqx "$expected" "$file"; then
    echo "missing rendered line: $expected" >&2
    return 1
  fi
}

# Public defaults keep the old pod available while the replacement becomes
# ready, but do not opt portable self-hosted installs into a version-gated
# native lifecycle handler.
require_line "  minReadySeconds: 10" "$default_render"
require_line "    type: RollingUpdate" "$default_render"
require_line "      maxSurge: 1" "$default_render"
require_line "      maxUnavailable: 0" "$default_render"
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
require_line "        terminationGracePeriodSeconds: 210" "$apps_render"
require_line "          enabled: true" "$apps_render"
require_line "          minAvailable: 1" "$apps_render"
require_line "          minDomains: 2" "$apps_render"
require_line "          topologyKey: topology.kubernetes.io/zone" "$apps_render"
require_line "          whenUnsatisfiable: DoNotSchedule" "$apps_render"

echo "Helm rollout rendering checks passed"
