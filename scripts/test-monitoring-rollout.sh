#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
platform_chart="$repo_root/.gitops/charts/platform"
apps_chart="$repo_root/.gitops/charts/apps"
monitoring_values="$platform_chart/ci/monitoring-values.yaml"
rules="$platform_chart/files/founder-open-plane.rules.yaml"
rule_tests="$platform_chart/testdata/founder-open-plane.rules.test.yaml"
chart_version="87.6.0"
chart_sha256="e8bad88c0ad0231b34314c643730ca5641f84db65d937e99a7df98133cbd9cc5"
chart_repo="https://prometheus-community.github.io/helm-charts"
chart_name="kube-prometheus-stack"
prometheus_version="3.13.2"
alertmanager_version="0.33.0"

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    tool_platform="linux-amd64"
    prometheus_tool_sha256="0e8c4d46101bd025ea8265e377d2caabc57f488fc1be1c367f37db69ea41be6f"
    alertmanager_tool_sha256="8ce11c42e8a6dfbbf93a59c0b193cb1329210b36d0c7ef3df7b745608675a1d1"
    ;;
  Darwin-x86_64)
    tool_platform="darwin-amd64"
    prometheus_tool_sha256="e57095aed0b69e10edaee28b92718d4a65f46d466bf93aeda54075e901d15c2a"
    alertmanager_tool_sha256="eedaa57a6c9f8a67b5bdbc5658c060247685a72364625c8c85e12a7a76845d92"
    ;;
  Darwin-arm64)
    tool_platform="darwin-arm64"
    prometheus_tool_sha256="f68ca4f1dbedd6366bbfdd8ac5d2c0b7ba1f273474acc8d38eb33202fbeec7a4"
    alertmanager_tool_sha256="597fbbb6c22d75755560377381326b52f9aad1f1a1dce746daccf357f502cda5"
    ;;
  *)
    echo "unsupported monitoring-test host: $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

promtool_bin="$(command -v promtool || true)"
if [[ -z "$promtool_bin" ]]; then
  archive="prometheus-${prometheus_version}.${tool_platform}.tar.gz"
  curl -fsSL \
    "https://github.com/prometheus/prometheus/releases/download/v${prometheus_version}/${archive}" \
    -o "$tmp/$archive"
  [[ "$(sha256_file "$tmp/$archive")" == "$prometheus_tool_sha256" ]] || {
    echo "promtool archive checksum mismatch" >&2
    exit 1
  }
  tar -xzf "$tmp/$archive" -C "$tmp"
  promtool_bin="$tmp/prometheus-${prometheus_version}.${tool_platform}/promtool"
fi

amtool_bin="$(command -v amtool || true)"
if [[ -z "$amtool_bin" ]]; then
  archive="alertmanager-${alertmanager_version}.${tool_platform}.tar.gz"
  curl -fsSL \
    "https://github.com/prometheus/alertmanager/releases/download/v${alertmanager_version}/${archive}" \
    -o "$tmp/$archive"
  [[ "$(sha256_file "$tmp/$archive")" == "$alertmanager_tool_sha256" ]] || {
    echo "amtool archive checksum mismatch" >&2
    exit 1
  }
  tar -xzf "$tmp/$archive" -C "$tmp"
  amtool_bin="$tmp/alertmanager-${alertmanager_version}.${tool_platform}/amtool"
fi

helm lint "$platform_chart" --values "$monitoring_values" >/dev/null
helm lint "$apps_chart" >/dev/null

default_render="$tmp/platform-default.yaml"
enabled_render="$tmp/platform-enabled.yaml"
stack_only_platform_render="$tmp/platform-stack-only.yaml"
child_values="$tmp/monitoring-child-values.yaml"
child_render="$tmp/monitoring-child.yaml"
stack_only_values="$tmp/monitoring-stack-only-values.yaml"
stack_only_render="$tmp/monitoring-stack-only.yaml"
alertmanager_config="$tmp/alertmanager.yaml"
stack_only_alertmanager_config="$tmp/alertmanager-stack-only.yaml"
apps_render="$tmp/apps.yaml"
apps_default_render="$tmp/apps-default.yaml"
apps_child_values="$tmp/apps-child-values.yaml"
apps_child_render="$tmp/apps-child.yaml"

helm template witself-platform "$platform_chart" \
  --set cell.name=monitoring-ci >"$default_render"
if grep -q 'name: witself-monitoring' "$default_render"; then
  echo "monitoring Application rendered from default-off values" >&2
  exit 1
fi

if helm template witself-platform "$platform_chart" \
  --set cell.name=monitoring-ci \
  --set platform.monitoring.enabled=true \
  --set platform.monitoring.alerting.enabled=true \
  --set platform.monitoring.receiver.secretName= \
  >"$tmp/invalid.yaml" 2>"$tmp/invalid.err"; then
  echo "monitoring accepted an empty receiver Secret" >&2
  exit 1
fi
grep -q 'receiver.secretName' "$tmp/invalid.err"

helm template witself-platform "$platform_chart" \
  --values "$monitoring_values" >"$enabled_render"
grep -q '/etc/alertmanager/secrets/witself-alert-receiver-v1/url' "$enabled_render"
if grep -Eqi 'https?://[^[:space:]]*(token|hook|secret)=' "$enabled_render"; then
  echo "rendered monitoring Application appears to contain a receiver value" >&2
  exit 1
fi

ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
  abort "monitoring Application missing" unless app
  abort "unexpected monitoring chart repo" unless app.dig("spec", "source", "repoURL") == ARGV[0]
  abort "unexpected monitoring chart name" unless app.dig("spec", "source", "chart") == ARGV[1]
  abort "unexpected monitoring chart version" unless app.dig("spec", "source", "targetRevision") == ARGV[2]
  abort "monitoring chart package digest is not bound to the Application" unless app.dig("metadata", "annotations", "witself.io/chart-package-sha256") == ARGV[3]
  abort "monitoring Application finalizer missing" unless app.dig("metadata", "finalizers") == ["resources-finalizer.argocd.argoproj.io"]
  sync_options = app.dig("spec", "syncPolicy", "syncOptions") || []
  abort "monitoring CRDs are not applied with the reviewed sync contract" unless sync_options.sort == ["CreateNamespace=true", "ServerSideApply=true"]
  print app.dig("spec", "source", "helm", "values")
' "$chart_repo" "$chart_name" "$chart_version" "$chart_sha256" <"$enabled_render" >"$child_values"

helm pull --repo "$chart_repo" "$chart_name" \
  --version "$chart_version" --destination "$tmp"
chart_archive="$tmp/${chart_name}-${chart_version}.tgz"
[[ "$(sha256_file "$chart_archive")" == "$chart_sha256" ]] || {
  echo "kube-prometheus-stack package checksum mismatch" >&2
  exit 1
}
helm template witself-monitoring "$chart_archive" \
  --namespace monitoring --include-crds --values "$child_values" >"$child_render"

helm template witself-platform "$platform_chart" \
  --set cell.name=monitoring-ci \
  --set platform.monitoring.enabled=true \
  >"$stack_only_platform_render"
ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
  abort "stack-only monitoring Application missing" unless app
  abort "unexpected stack-only monitoring source" unless [app.dig("spec", "source", "repoURL"), app.dig("spec", "source", "chart"), app.dig("spec", "source", "targetRevision")] == ARGV
  print app.dig("spec", "source", "helm", "values")
' "$chart_repo" "$chart_name" "$chart_version" <"$stack_only_platform_render" >"$stack_only_values"
helm template witself-monitoring "$chart_archive" \
  --namespace monitoring --include-crds --values "$stack_only_values" >"$stack_only_render"

if grep -q 'WitselfServerMetricsUnavailable' "$stack_only_render"; then
  echo "stack-only rollout rendered production alert rules before target activation" >&2
  exit 1
fi

for forbidden in 'kind: Ingress' 'kind: Grafana' 'node-exporter'; do
  if grep -q "$forbidden" "$child_render"; then
    echo "trimmed monitoring render contains forbidden surface: $forbidden" >&2
    exit 1
  fi
done
grep -q 'kind: Prometheus' "$child_render"
grep -q 'kind: Alertmanager' "$child_render"
grep -q 'kind: PrometheusRule' "$child_render"
grep -q 'witself-alert-receiver-v1' "$child_render"

ruby -ryaml -rbase64 -e '
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  secrets = docs.select do |doc|
    doc["kind"] == "Secret" && doc.dig("metadata", "name").to_s.start_with?("alertmanager-") &&
      (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml"))
  end
  abort "expected one rendered Alertmanager config Secret" unless secrets.length == 1
  raw = secrets[0].dig("stringData", "alertmanager.yaml") || Base64.strict_decode64(secrets[0].dig("data", "alertmanager.yaml"))
  config = YAML.safe_load(raw, aliases: false)
  route = config.fetch("route")
  abort "Alertmanager root route must be matcher-free" if route.key?("matchers") || route.key?("match") || route.key?("match_re")
  receivers = config.fetch("receivers")
  if ARGV[2] == "active"
    abort "unexpected Alertmanager receiver set" unless receivers.map { |item| item.fetch("name") } == ["null", "witself-external"]
    routes = route.fetch("routes")
    abort "unexpected external receiver route" unless routes == [{"matchers" => ["witself_alert = \"true\""], "receiver" => "witself-external"}]
    webhook = receivers[1].fetch("webhook_configs").fetch(0)
    abort "receiver must use only the mounted URL file" unless webhook["url_file"] == "/etc/alertmanager/secrets/witself-alert-receiver-v1/url" && !webhook.key?("url")
    abort "resolved delivery is required" unless webhook["send_resolved"] == true
    abort "receiver redirects must be disabled" unless webhook.dig("http_config", "follow_redirects") == false
  else
    abort "stack-only receiver set is not null-only" unless receivers == [{"name" => "null"}]
    abort "stack-only route must have no children" unless route.fetch("routes") == []
  end
  File.binwrite(ARGV[1], raw)
' "$child_render" "$alertmanager_config" active
ruby -ryaml -rbase64 -e '
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  secret = docs.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name").to_s.start_with?("alertmanager-") && (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml")) }
  abort "stack-only Alertmanager config missing" unless secret
  raw = secret.dig("stringData", "alertmanager.yaml") || Base64.strict_decode64(secret.dig("data", "alertmanager.yaml"))
  config = YAML.safe_load(raw, aliases: false)
  abort "stack-only receiver set is not null-only" unless config.fetch("receivers") == [{"name" => "null"}]
  abort "stack-only route must have no children" unless config.dig("route", "routes") == []
  abort "stack-only config contains an external URL" if raw.include?("url:") || raw.include?("url_file:")
  File.binwrite(ARGV[1], raw)
' "$stack_only_render" "$stack_only_alertmanager_config"
"$amtool_bin" check-config "$alertmanager_config" >/dev/null
"$amtool_bin" check-config "$stack_only_alertmanager_config" >/dev/null

ruby -ryaml -e '
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  required_crds = %w[
    alertmanagers.monitoring.coreos.com
    prometheuses.monitoring.coreos.com
    prometheusrules.monitoring.coreos.com
    servicemonitors.monitoring.coreos.com
  ]
  crds = docs.select { |doc| doc["kind"] == "CustomResourceDefinition" }.map { |doc| doc.dig("metadata", "name") }
  abort "required monitoring CRDs missing" unless (required_crds - crds).empty?
  abort "monitoring render contains an Ingress" if docs.any? { |doc| doc["kind"] == "Ingress" }
  abort "monitoring render contains Grafana or node exporter" if docs.any? { |doc| doc.dig("metadata", "name").to_s.match?(/grafana|node-exporter/) }
  abort "monitoring render contains a DaemonSet" if docs.any? { |doc| doc["kind"] == "DaemonSet" }

  workloads = docs.select { |doc| %w[Deployment StatefulSet DaemonSet Job].include?(doc["kind"]) }
  abort "no monitoring workloads rendered" if workloads.empty?
  expected_workload_images = %w[
    ghcr.io/jkroepke/kube-webhook-certgen:1.8.4@sha256:76a2170cd0c9a7758c4ac8ac5bbe9b6f73e869a15ffb77d9f684664f7d7b96b1
    quay.io/prometheus-operator/prometheus-operator:v0.92.1@sha256:7d9247d2351480fc74587e24681578f815f387bafb2ee7b86a852a94c4cd3774
    registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.19.1@sha256:85108987d044b18a098126732f98602df408888c0f7d456241f5abefb9744bc1
  ]
  actual_images = workloads.flat_map do |doc|
    spec = doc.dig("spec", "template", "spec") || {}
    (Array(spec["initContainers"]) + Array(spec["containers"])).map { |container| container["image"] }
  end.compact.uniq
  abort "monitoring workload images are not the exact digest-pinned set" unless actual_images.sort == expected_workload_images.sort
  abort "monitoring render contains a double sha256 prefix" if actual_images.any? { |image| image.include?("@sha256:sha256:") }
  workloads.each do |doc|
    spec = doc.dig("spec", "template", "spec") || {}
    (Array(spec["initContainers"]) + Array(spec["containers"])).each do |container|
      resources = container.fetch("resources", {})
      %w[requests limits].each do |kind|
        %w[cpu memory].each do |dimension|
          abort "unbounded #{doc["kind"]}/#{doc.dig("metadata", "name")}/#{container["name"]}" if resources.dig(kind, dimension).to_s.empty?
        end
      end
    end
  end

  prometheus = docs.find { |doc| doc["kind"] == "Prometheus" }
  alertmanager = docs.find { |doc| doc["kind"] == "Alertmanager" }
  abort "Prometheus or Alertmanager CR missing" unless prometheus && alertmanager
  expected_prometheus_image = "quay.io/prometheus/prometheus:v3.13.0-distroless@sha256:f3b6aae627d96e7ad8256cdf6de5953247735117c6f577383fadb42efeeea7bc"
  expected_alertmanager_image = "quay.io/prometheus/alertmanager:v0.33.0@sha256:af26fbe4dd1886ac0efd7bd55cd9027da262e105b137a376522b7c14c3626e4a"
  abort "Prometheus image is not digest-pinned" unless prometheus.dig("spec", "image") == expected_prometheus_image
  abort "Alertmanager image is not digest-pinned" unless alertmanager.dig("spec", "image") == expected_alertmanager_image
  founder_rules = docs.select do |doc|
    doc["kind"] == "PrometheusRule" && Array(doc.dig("spec", "groups")).any? { |group| group["name"] == "witself-founder-open-plane" }
  end
  abort "expected one selected founder rule resource" unless founder_rules.length == 1
  abort "founder rules are outside the selected namespace/label" unless founder_rules[0].dig("metadata", "namespace") == "monitoring" && founder_rules[0].dig("metadata", "labels", "release") == "witself-monitoring"
  [prometheus, alertmanager].each do |resource|
    %w[requests limits].each do |kind|
      %w[cpu memory].each do |dimension|
        abort "unbounded #{resource["kind"]}" if resource.dig("spec", "resources", kind, dimension).to_s.empty?
      end
    end
  end
  abort "AlertmanagerConfig discovery is not release-scoped" unless alertmanager.dig("spec", "alertmanagerConfigSelector") == {"matchLabels" => {"release" => "witself-monitoring"}}
  abort "AlertmanagerConfig discovery is not namespace-scoped" unless alertmanager.dig("spec", "alertmanagerConfigNamespaceSelector") == {"matchLabels" => {"kubernetes.io/metadata.name" => "monitoring"}}

  alertmanager_policy = docs.find { |doc| doc["kind"] == "NetworkPolicy" && doc.dig("metadata", "name") == "witself-monitoring-kube-pr-alertmanager" }
  abort "Alertmanager ingress NetworkPolicy missing" unless alertmanager_policy
  abort "Alertmanager egress was unexpectedly blocked" unless alertmanager_policy.dig("spec", "policyTypes") == ["Ingress"]
  alertmanager_ingress = alertmanager_policy.dig("spec", "ingress") || []
  abort "Alertmanager ingress is not closed" unless alertmanager_ingress.length == 2 && alertmanager_ingress.all? { |rule| Array(rule["from"]).length == 1 }
  peers = alertmanager_ingress.map { |rule| rule.dig("from", 0, "podSelector", "matchLabels") }
  abort "Alertmanager ingress peers are not exact" unless peers.include?({"app.kubernetes.io/name" => "prometheus"}) && peers.include?({"app.kubernetes.io/name" => "alertmanager", "component" => "config-reloader"})

  expected_monitoring_ns = {"matchLabels" => {"kubernetes.io/metadata.name" => "monitoring"}}
  expected_release = {"matchLabels" => {"release" => "witself-monitoring"}}
  spec = prometheus.fetch("spec")
  %w[podMonitorNamespaceSelector probeNamespaceSelector scrapeConfigNamespaceSelector ruleNamespaceSelector].each do |field|
    abort "#{field} is not monitoring-only" unless spec[field] == expected_monitoring_ns
  end
  %w[podMonitorSelector probeSelector scrapeConfigSelector ruleSelector serviceMonitorSelector].each do |field|
    abort "#{field} is not release-scoped" unless spec[field] == expected_release
  end
  expression = spec.dig("serviceMonitorNamespaceSelector", "matchExpressions", 0)
  abort "ServiceMonitor namespaces are not exact" unless expression == {"key" => "kubernetes.io/metadata.name", "operator" => "In", "values" => ["witself", "monitoring"]}

  kubelet_monitors = docs.select { |doc| doc["kind"] == "ServiceMonitor" && doc.dig("metadata", "name").to_s.include?("kubelet") }
  abort "expected one kubelet ServiceMonitor" unless kubelet_monitors.length == 1
  paths = Array(kubelet_monitors[0].dig("spec", "endpoints")).map { |endpoint| endpoint["path"] }
  abort "cAdvisor scrape was not disabled" if paths.include?("/metrics/cadvisor")

  alertmanager_service = docs.find { |doc| doc["kind"] == "Service" && doc.dig("metadata", "name") == "witself-monitoring-kube-pr-alertmanager" }
  abort "exact private Alertmanager Service missing" unless alertmanager_service
  abort "Alertmanager Service is not bound to the cell" unless alertmanager_service.dig("metadata", "labels", "witself.io/cell") == "monitoring-ci"
  abort "Alertmanager Service is not private" unless alertmanager_service.dig("spec", "type") == "ClusterIP" && alertmanager_service.dig("spec", "externalIPs").to_a.empty?

  operator = workloads.find { |doc| doc["kind"] == "Deployment" && doc.dig("metadata", "name").to_s.end_with?("operator") }
  args = Array(operator&.dig("spec", "template", "spec", "containers", 0, "args"))
  expected_reloader_args = %w[
    --config-reloader-cpu-request=25m
    --config-reloader-cpu-limit=100m
    --config-reloader-memory-request=32Mi
    --config-reloader-memory-limit=64Mi
  ]
  abort "config reloader resources are not bounded" unless (expected_reloader_args - args).empty?
  expected_reloader_image = "--prometheus-config-reloader=quay.io/prometheus-operator/prometheus-config-reloader:v0.92.1@sha256:74550ba3e8bf93f47bc574231090d340ae9c01d25cd11ff74799e65f9fdb9a48"
  abort "config reloader image is not digest-pinned" unless args.include?(expected_reloader_image)
' "$child_render"

helm template witself-apps "$apps_chart" \
  --set cell.name=monitoring-ci >"$apps_default_render"
ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-server" }
  abort "default witself-server Application missing" unless app
  values = YAML.safe_load(app.dig("spec", "source", "helm", "values"), aliases: false)
  abort "default-off monitoring changed the server NetworkPolicy contract" if values.key?("networkPolicy") || values.dig("worker")&.key?("networkPolicy")
' <"$apps_default_render"

helm template witself-apps "$apps_chart" \
  --set cell.name=monitoring-ci \
  --set apps.witselfServer.worker.enabled=true \
  --set apps.witselfServer.metrics.serviceMonitor.enabled=true \
  --set apps.witselfServer.metrics.serviceMonitor.labels.release=witself-monitoring \
  --set apps.witselfServer.worker.metrics.serviceMonitor.enabled=true \
  --set apps.witselfServer.worker.metrics.serviceMonitor.labels.release=witself-monitoring \
  --set 'apps.witselfServer.networkPolicy.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring' \
  --set 'apps.witselfServer.networkPolicy.metricsFrom[0].podSelector.matchLabels.app\.kubernetes\.io/name=prometheus' \
  --set 'apps.witselfServer.worker.networkPolicy.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring' \
  --set 'apps.witselfServer.worker.networkPolicy.metricsFrom[0].podSelector.matchLabels.app\.kubernetes\.io/name=prometheus' \
  >"$apps_render"
ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-server" }
  abort "activated witself-server Application missing" unless app
  print app.dig("spec", "source", "helm", "values")
' <"$apps_render" >"$apps_child_values"
helm template witself-server "$repo_root/charts/witself-server" \
  --namespace witself --values "$apps_child_values" >"$apps_child_render"
ruby -ryaml -e '
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  monitors = docs.select { |doc| doc["kind"] == "ServiceMonitor" }
  abort "expected server and worker ServiceMonitors" unless monitors.length == 2
  abort "ServiceMonitor release labels are not exact" unless monitors.all? { |doc| doc.dig("metadata", "labels", "release") == "witself-monitoring" }
  policies = docs.select { |doc| doc["kind"] == "NetworkPolicy" }
  abort "expected server and worker NetworkPolicies" unless policies.map { |doc| doc.dig("metadata", "name") }.sort == ["witself-server", "witself-worker"]
  expected_peer = {
    "namespaceSelector" => {"matchLabels" => {"kubernetes.io/metadata.name" => "monitoring"}},
    "podSelector" => {"matchLabels" => {"app.kubernetes.io/name" => "prometheus"}},
  }
  policies.each do |policy|
    metrics = Array(policy.dig("spec", "ingress")).find { |ingress| Array(ingress["ports"]).any? { |port| port["port"] == "metrics" } }
    abort "metrics ingress missing for #{policy.dig("metadata", "name")}" unless metrics
    abort "metrics ingress peer is not exact" unless metrics["from"] == [expected_peer]
  end
' "$apps_child_render"

# PagerDuty Events API v2 receiver plus the dead-man heartbeat route. The
# webhook-mode assertions above stay untouched, so they keep proving that the
# default receiver contract is unchanged.
pagerduty_values="$platform_chart/ci/monitoring-pagerduty-values.yaml"
pagerduty_platform_render="$tmp/platform-pagerduty.yaml"
pagerduty_child_values="$tmp/monitoring-pagerduty-child-values.yaml"
pagerduty_child_render="$tmp/monitoring-pagerduty-child.yaml"
pagerduty_alertmanager_config="$tmp/alertmanager-pagerduty.yaml"

helm template witself-platform "$platform_chart" \
  --values "$pagerduty_values" >"$pagerduty_platform_render"
if grep -Eqi 'routing_key:|https?://[^[:space:]]*(token|hook|secret|key)=' "$pagerduty_platform_render"; then
  echo "rendered PagerDuty monitoring Application appears to contain a secret value" >&2
  exit 1
fi
if helm template witself-platform "$platform_chart" \
  --values "$pagerduty_values" \
  --set platform.monitoring.receiver.kind=carrier-pigeon \
  >/dev/null 2>"$tmp/invalid-kind.err"; then
  echo "monitoring accepted an unknown receiver kind" >&2
  exit 1
fi
grep -q 'receiver.kind' "$tmp/invalid-kind.err"

ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
  abort "pagerduty monitoring Application missing" unless app
  print app.dig("spec", "source", "helm", "values")
' <"$pagerduty_platform_render" >"$pagerduty_child_values"
helm template witself-monitoring "$chart_archive" \
  --namespace monitoring --include-crds --values "$pagerduty_child_values" >"$pagerduty_child_render"

ruby -ryaml -rbase64 -e '
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  secret = docs.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name").to_s.start_with?("alertmanager-") && (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml")) }
  abort "pagerduty Alertmanager config missing" unless secret
  raw = secret.dig("stringData", "alertmanager.yaml") || Base64.strict_decode64(secret.dig("data", "alertmanager.yaml"))
  config = YAML.safe_load(raw, aliases: false)
  route = config.fetch("route")
  abort "root route must stay matcher-free" if route.key?("matchers") || route.key?("match") || route.key?("match_re")
  receivers = config.fetch("receivers")
  abort "unexpected pagerduty receiver set" unless receivers.map { |item| item.fetch("name") } == ["null", "witself-external", "witself-deadman"]
  abort "unexpected pagerduty route set" unless route.fetch("routes") == [
    {"matchers" => ["witself_alert = \"true\""], "receiver" => "witself-external"},
    {"matchers" => ["witself_watchdog = \"true\""], "receiver" => "witself-deadman", "group_wait" => "0s", "group_interval" => "1m", "repeat_interval" => "5m"},
  ]
  abort "incident receiver must not use a webhook in pagerduty mode" if receivers[1].key?("webhook_configs")
  pd = receivers[1].fetch("pagerduty_configs").fetch(0)
  abort "pagerduty receiver must read the routing key from the mounted file" unless pd["routing_key_file"] == "/etc/alertmanager/secrets/witself-alert-receiver-v1/routing-key" && !pd.key?("routing_key")
  abort "pagerduty receiver must target Events API v2" unless pd["url"] == "https://events.pagerduty.com/v2/enqueue"
  abort "resolved delivery is required" unless pd["send_resolved"] == true
  abort "pagerduty severity must carry the alert severity" unless pd["severity"] == "{{ .CommonLabels.severity }}"
  abort "pagerduty redirects must be disabled" unless pd.dig("http_config", "follow_redirects") == false
  dead = receivers[2].fetch("webhook_configs").fetch(0)
  abort "dead-man receiver must use only the mounted URL file" unless dead["url_file"] == "/etc/alertmanager/secrets/witself-deadman-v1/url" && !dead.key?("url")
  abort "dead-man heartbeat must not request resolved delivery" unless dead["send_resolved"] == false
  File.binwrite(ARGV[1], raw)
' "$pagerduty_child_render" "$pagerduty_alertmanager_config"
"$amtool_bin" check-config "$pagerduty_alertmanager_config" >/dev/null

ruby -ryaml -e '
  alertmanager_route_of = lambda do |name|
    config = YAML.safe_load(File.read(ARGV[1]), aliases: false)
    route = Array(config.dig("route", "routes")).find { |item| item["receiver"] == name }
    abort "route for #{name} missing" unless route
    route
  end
  docs = YAML.load_stream(File.read(ARGV[0])).compact
  alertmanager = docs.find { |doc| doc["kind"] == "Alertmanager" }
  abort "Alertmanager resource missing" unless alertmanager
  abort "both receiver Secrets must be mounted" unless Array(alertmanager.dig("spec", "secrets")).sort == ["witself-alert-receiver-v1", "witself-deadman-v1"]
  rules = docs.select { |doc| doc["kind"] == "PrometheusRule" }
    .flat_map { |doc| Array(doc.dig("spec", "groups")) }
    .flat_map { |group| Array(group["rules"]) }
  watchdog = rules.find { |rule| rule["alert"] == "WitselfWatchdog" }
  abort "watchdog rule missing" unless watchdog
  abort "watchdog must always fire" unless watchdog["expr"] == "vector(1)"
  abort "watchdog must carry the dead-man label" unless watchdog.dig("labels", "witself_watchdog") == "true"
  abort "watchdog must never reach the incident route" if watchdog.dig("labels", "witself_alert")
  unlabelled = rules.reject { |rule| rule.dig("labels", "witself_alert") == "true" }.map { |rule| rule["alert"] }
  abort "every rule except the watchdog must reach the incident route: #{unlabelled.inspect}" unless unlabelled == ["WitselfWatchdog"]
  deadman_route = alertmanager_route_of.call("witself-deadman")
  abort "the dead-man beat must flush faster than it repeats" unless deadman_route["group_interval"] == "1m" && deadman_route["repeat_interval"] == "5m"
' "$pagerduty_child_render" "$pagerduty_alertmanager_config"

if grep -q 'witself-deadman' "$child_render"; then
  echo "webhook-mode render leaked the dead-man receiver" >&2
  exit 1
fi

# The two receiver Secrets must be distinct, or the mount list duplicates.
if helm template witself-platform "$platform_chart" \
  --values "$pagerduty_values" \
  --set platform.monitoring.receiverDeadman.secretName=witself-alert-receiver-v1 \
  >/dev/null 2>"$tmp/invalid-deadman.err"; then
  echo "monitoring accepted a dead-man Secret identical to the incident Secret" >&2
  exit 1
fi
grep -q 'receiverDeadman.secretName' "$tmp/invalid-deadman.err"
if helm template witself-platform "$platform_chart" \
  --values "$pagerduty_values" \
  --set platform.monitoring.receiverDeadman.secretKey= \
  >/dev/null 2>"$tmp/invalid-deadman-key.err"; then
  echo "monitoring accepted an empty dead-man Secret key" >&2
  exit 1
fi
grep -q 'receiverDeadman.secretKey' "$tmp/invalid-deadman-key.err"

# Every receiver/route combination must render a coherent Alertmanager config:
# a receiver without its route (or a route without its receiver) bricks startup.
assert_receiver_combination() {
  combo_kind="$1"
  combo_deadman="${2:-}"
  combo_tag="$combo_kind${combo_deadman:+-deadman}"
  combo_render="$tmp/platform-combo-$combo_tag.yaml"
  combo_child_values="$tmp/combo-child-values-$combo_tag.yaml"
  combo_child_render="$tmp/combo-child-$combo_tag.yaml"
  combo_config="$tmp/alertmanager-combo-$combo_tag.yaml"
  helm template witself-platform "$platform_chart" \
    --values "$monitoring_values" \
    --set platform.monitoring.receiver.kind="$combo_kind" \
    --set platform.monitoring.receiverDeadman.secretName="$combo_deadman" \
    >"$combo_render"
  ruby -ryaml -e '
    app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
    abort "combination monitoring Application missing" unless app
    print app.dig("spec", "source", "helm", "values")
  ' <"$combo_render" >"$combo_child_values"
  helm template witself-monitoring "$chart_archive" \
    --namespace monitoring --include-crds --values "$combo_child_values" >"$combo_child_render"
  ruby -ryaml -rbase64 -e '
    docs = YAML.load_stream(File.read(ARGV[0])).compact
    secret = docs.find { |doc| doc["kind"] == "Secret" && doc.dig("metadata", "name").to_s.start_with?("alertmanager-") && (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml")) }
    abort "combination Alertmanager config missing" unless secret
    raw = secret.dig("stringData", "alertmanager.yaml") || Base64.strict_decode64(secret.dig("data", "alertmanager.yaml"))
    config = YAML.safe_load(raw, aliases: false)
    names = config.fetch("receivers").map { |item| item.fetch("name") }
    routed = Array(config.dig("route", "routes")).map { |item| item.fetch("receiver") }
    abort "a routed receiver is not defined: #{(routed - names).inspect}" unless (routed - names).empty?
    abort "a defined receiver is unrouted: #{(names - routed - ["null"]).inspect}" unless (names - routed - ["null"]).empty?
    alertmanager = docs.find { |doc| doc["kind"] == "Alertmanager" }
    mounted = Array(alertmanager.dig("spec", "secrets"))
    abort "mounted Secrets must be unique" unless mounted.uniq == mounted
    raw.scan(%r{/etc/alertmanager/secrets/([^/]+)/}).flatten.uniq.each do |name|
      abort "receiver reads an unmounted Secret #{name}" unless mounted.include?(name)
    end
    File.binwrite(ARGV[1], raw)
  ' "$combo_child_render" "$combo_config"
  "$amtool_bin" check-config "$combo_config" >/dev/null
}

assert_receiver_combination webhook witself-deadman-v1
assert_receiver_combination pagerduty ""


"$promtool_bin" check rules "$rules"
"$promtool_bin" test rules "$rule_tests"

echo "monitoring rollout capability checks passed"
