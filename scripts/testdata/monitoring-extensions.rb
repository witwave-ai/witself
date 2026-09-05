#!/usr/bin/env ruby
# Render regression checks for the serving-cell monitoring extension. The
# backup-cell proofs are pin-independent: they compare live renders against
# renders of the same live values with the extension forced off, so the
# documented roll-cell.sh version-pin rollout never trips them.
require "yaml"
require "open3"
require "fileutils"

repo_root, scratch, chart_archive = ARGV
abort "usage: monitoring-extensions.rb REPO_ROOT TEMP_DIR CHART_ARCHIVE" unless chart_archive
platform_chart = File.join(repo_root, ".gitops/charts/platform")
apps_chart = File.join(repo_root, ".gitops/charts/apps")
testdata = File.join(platform_chart, "testdata")

def command(*args)
  stdout, stderr, status = Open3.capture3(*args)
  abort "#{args.join(' ')} failed:\n#{stderr}\n#{stdout}" unless status.success?
  stdout
end

def documents(raw)
  YAML.load_stream(raw).compact
end

def monitoring_values(raw)
  app = documents(raw).find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
  abort "monitoring Application missing" unless app
  YAML.safe_load(app.dig("spec", "source", "helm", "values"), aliases: false)
end

def platform_group_names(values)
  Array(values.dig("additionalPrometheusRulesMap", "platform", "groups")).map { |group| group.fetch("name") }
end

def render_platform(chart, values, *overrides)
  command("helm", "template", "witself-platform", chart, "--values", values, *overrides)
end

cell_renders = {}
%w[civo-sandbox-usw2-dev civo-sandbox-use1-backup].each do |cell|
  values = File.join(repo_root, ".gitops/cells", cell, "values.yaml")
  command("helm", "lint", platform_chart, "--values", values)
  command("helm", "lint", apps_chart, "--values", values)
  cell_renders[cell] = {
    "platform" => render_platform(platform_chart, values),
    "apps" => command("helm", "template", "witself-apps", apps_chart, "--values", values),
  }
  issuer = documents(cell_renders[cell]["apps"]).find { |doc| doc["kind"] == "ClusterIssuer" }
  abort "#{cell} ACME support contact missing" unless issuer&.dig("spec", "acme", "email") == "support@witwave.ai"
end

existing_group_names = %w[founder-open-plane postgresql uptime-probes].flat_map do |file|
  YAML.safe_load(File.read(File.join(platform_chart, "files", "#{file}.rules.yaml")), aliases: false).fetch("groups").map { |group| group.fetch("name") }
end

# The extension must be a no-op for the backup cell. Rendering its live values
# with every extension knob forced off must reproduce the live render byte for
# byte, and that render must carry none of the extension's artifacts. Live
# deployment pins (roll-cell.sh) are free to move without touching this check.
backup = cell_renders.fetch("civo-sandbox-use1-backup")
backup_values = File.join(repo_root, ".gitops/cells/civo-sandbox-use1-backup/values.yaml")
extension_off = %w[
  platform.monitoring.nodeExporter.enabled=false
  platform.monitoring.kubelet.cadvisor=false
  platform.monitoring.defaultRules.enabled=false
  platform.monitoring.certManager.enabled=false
  platform.monitoring.argocd.enabled=false
].flat_map { |kv| ["--set", kv] }
abort "backup platform render must equal the extension-off render" unless backup.fetch("platform") == render_platform(platform_chart, backup_values, *extension_off)
backup_docs = documents(backup.fetch("platform"))
abort "backup cell must render no PodMonitor" if backup_docs.any? { |doc| doc["kind"] == "PodMonitor" }
backup_app = backup_docs.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring" }
if backup_app
  # A backup cell that does run the monitoring stack must still see none of the extension.
  backup_monitoring = YAML.safe_load(backup_app.dig("spec", "source", "helm", "values"), aliases: false)
  abort "backup cell must not activate upstream defaultRules" if backup_monitoring.dig("defaultRules", "create") == true
  abort "backup cell must not enable node-exporter" if backup_monitoring.dig("nodeExporter", "enabled") == true
  abort "backup cell must not scrape cAdvisor" if backup_monitoring.dig("kubelet", "serviceMonitor", "cAdvisor") == true
  abort "backup cell must keep only the pre-extension platform rule groups" unless platform_group_names(backup_monitoring).sort == existing_group_names.sort
end
abort "backup cell must render no PrometheusRule" if backup_docs.any? { |doc| doc["kind"] == "PrometheusRule" }

# The backup apps change is exactly the ACME contact: rendering the live values
# with the email reverted must differ only by that one line. The same check runs
# against the documented two-pin rollout so a normal version roll stays green.
email_line = "    email: \"support@witwave.ai\"\n"
def assert_email_only_change(apps_chart, backup_values, email_line, *overrides)
  with_email = command("helm", "template", "witself-apps", apps_chart, "--values", backup_values, *overrides)
  abort "backup apps render must add exactly one ACME contact line" unless with_email.scan(email_line).length == 1
  without_email = command("helm", "template", "witself-apps", apps_chart, "--values", backup_values, *overrides, "--set", "apps.witselfServer.civoIngress.acme.email=")
  abort "ACME email override did not take effect" unless without_email.scan(email_line).empty?
  abort "backup apps render changed beyond the ACME email line" unless with_email.sub(email_line, "") == without_email
end
abort "backup apps render must add exactly one ACME contact line" unless backup.fetch("apps").scan(email_line).length == 1
assert_email_only_change(apps_chart, backup_values, email_line)
assert_email_only_change(apps_chart, backup_values, email_line, "--set", "apps.witselfServer.chartVersion=0.0.274", "--set", "apps.witselfServer.imageTag=0.0.274")

serving_render = cell_renders.fetch("civo-sandbox-usw2-dev").fetch("platform")
serving_values = monitoring_values(serving_render)
child_values_path = File.join(scratch, "monitoring-extensions-child-values.yaml")
File.write(child_values_path, YAML.dump(serving_values))
child_raw = command("helm", "template", "witself-monitoring", chart_archive, "--namespace", "monitoring", "--values", child_values_path)
File.write(File.join(scratch, "monitoring-extensions-child.yaml"), child_raw)
child_docs = documents(child_raw)

upstream_values = YAML.safe_load(command("helm", "show", "values", chart_archive), aliases: true)
group_values = serving_values.dig("defaultRules", "rules")
upstream_group_names = upstream_values.dig("defaultRules", "rules").keys
abort "defaultRules groups must explicitly cover every pinned upstream group" unless group_values.keys.sort == upstream_group_names.sort
selected_groups = %w[nodeExporterAlerting kubernetesApps kubernetesResources]
abort "unexpected enabled kube-prometheus defaultRules groups" unless group_values.select { |_, enabled| enabled == true }.keys.sort == selected_groups.sort
abort "unselected upstream rule groups must be explicitly false" unless group_values.reject { |name, _| selected_groups.include?(name) }.values.all? { |value| value == false }
abort "serving upstream rules were not activated" unless serving_values.dig("defaultRules", "create") == true
%w[kubeEtcd kubeControllerManager kubeScheduler kubeProxy].each do |component|
  abort "k3s-inapplicable #{component} scrape must be explicitly disabled" unless serving_values.dig(component, "enabled") == false
end
abort "KubeVersionMismatch must be explicitly disabled for this small cell" unless serving_values.dig("defaultRules", "disabled", "KubeVersionMismatch") == true

expected_upstream = {
  "node-exporter" => %w[NodeClockNotSynchronising NodeClockSkewDetected NodeMemoryHighUtilization NodeMemoryMajorPagesFaults],
  "kubernetes-apps" => %w[KubeContainerWaiting KubeDeploymentReplicasMismatch KubePodCrashLooping KubePodNotReady KubeStatefulSetReplicasMismatch],
  "kubernetes-resources" => %w[KubeCPUOvercommit KubeMemoryOvercommit],
}
rule_resources = child_docs.select { |doc| doc["kind"] == "PrometheusRule" }
rendered_groups = rule_resources.flat_map { |doc| Array(doc.dig("spec", "groups")) }
expected_upstream.each do |name, names|
  group = rendered_groups.find { |item| item["name"] == name }
  abort "selected upstream group #{name} missing" unless group
  actual = group.fetch("rules").map { |rule| rule["alert"] }.compact
  abort "unexpected #{name} alert allowlist: #{actual.inspect}" unless actual.sort == names.sort
end
allowed_group_names = expected_upstream.keys + existing_group_names + %w[witself-watchdog platform-resources platform-node platform-storage platform-certificates platform-argocd]
abort "unexpected upstream group rendered: #{(rendered_groups.map { |group| group['name'] } - allowed_group_names).inspect}" unless (rendered_groups.map { |group| group["name"] } - allowed_group_names).empty?
alerts = rendered_groups.flat_map { |group| group.fetch("rules") }.select { |rule| rule["alert"] }
custom_names = %w[NodeFilesystemSpaceFillingUp WitselfPrometheusPVCUsageHigh WitselfCertificateExpiringSoon WitselfArgoApplicationUnhealthy]
custom_names.each do |name|
  abort "expected exactly one #{name} alert" unless alerts.count { |rule| rule["alert"] == name } == 1
end
alerts.each do |rule|
  next if rule["alert"] == "WitselfWatchdog"
  abort "#{rule['alert']} cannot reach the existing alert route" unless rule.dig("labels", "witself_alert") == "true"
end
rule_resources.each do |doc|
  abort "PrometheusRule is outside discovery scope" unless doc.dig("metadata", "namespace") == "monitoring" && doc.dig("metadata", "labels", "release") == "witself-monitoring"
end

# Every alert omitted from the selected upstream groups must also be explicit
# in disabled, so a future chart edit cannot silently expand incident paging.
upstream_dir = File.join(scratch, "monitoring-extensions-upstream")
FileUtils.mkdir_p(upstream_dir)
command("tar", "-xzf", chart_archive, "-C", upstream_dir)
expected_upstream.each do |group, retained|
  template = File.read(File.join(upstream_dir, "kube-prometheus-stack/templates/prometheus/rules-1.14", "#{group}.yaml"))
  source_alerts = template.scan(/^\s+- alert: (\w+)$/).flatten.uniq
  (source_alerts - retained).each do |name|
    abort "unselected upstream alert #{name} must be explicitly disabled" unless serving_values.dig("defaultRules", "disabled", name) == true
  end
end

exporters = child_docs.select { |doc| doc["kind"] == "DaemonSet" }
abort "only the node-exporter DaemonSet may add pods" unless exporters.length == 1 && exporters[0].dig("metadata", "name").include?("node-exporter")
exporter_spec = exporters[0].dig("spec", "template", "spec")
abort "node-exporter must have exactly one container" unless exporter_spec.fetch("containers").length == 1 && Array(exporter_spec["initContainers"]).empty?
expected_exporter_image = "quay.io/prometheus/node-exporter:v1.11.1-distroless@sha256:0f422f62c15f154af8d8572b23d623aebfb10cec73a5c654d18f911f3f9df241"
abort "node-exporter image is not the exact reviewed digest" unless exporter_spec.dig("containers", 0, "image") == expected_exporter_image
resources = exporter_spec.dig("containers", 0, "resources")
expected_resources = {"requests" => {"cpu" => "20m", "memory" => "32Mi"}, "limits" => {"cpu" => "100m", "memory" => "64Mi"}}
abort "unexpected per-node node-exporter capacity" unless resources == expected_resources
abort "node-exporter scrape not enabled" unless child_docs.any? { |doc| doc["kind"] == "ServiceMonitor" && doc.dig("metadata", "name").include?("node-exporter") }
abort "Prometheus egress policy would block new scrape targets" if child_docs.any? { |doc| doc["kind"] == "NetworkPolicy" && Array(doc.dig("spec", "policyTypes")).include?("Egress") }

kubelet = child_docs.find { |doc| doc["kind"] == "ServiceMonitor" && doc.dig("metadata", "name").include?("kubelet") }
paths = Array(kubelet&.dig("spec", "endpoints")).map { |endpoint| endpoint["path"] }
abort "kubelet cAdvisor scrape missing" unless paths.include?("/metrics/cadvisor")
%w[probes resource].each do |endpoint|
  expected = upstream_values.dig("kubelet", "serviceMonitor", endpoint)
  abort "kubelet #{endpoint} endpoint differs from upstream default" unless paths.include?("/metrics/#{endpoint}") == expected
end

prometheus = child_docs.find { |doc| doc["kind"] == "Prometheus" }
abort "PodMonitor discovery must remain monitoring-only" unless prometheus&.dig("spec", "podMonitorNamespaceSelector") == {"matchLabels" => {"kubernetes.io/metadata.name" => "monitoring"}}
abort "PodMonitor discovery must remain release-scoped" unless prometheus.dig("spec", "podMonitorSelector") == {"matchLabels" => {"release" => "witself-monitoring"}}
expected_monitors = {
  "witself-cert-manager" => ["cert-manager", "cert-manager", "cert-manager", "http-metrics"],
  "witself-argocd-application-controller" => ["argocd", "argocd", "argocd-application-controller", "metrics"],
  "witself-argocd-repo-server" => ["argocd", "argocd", "argocd-repo-server", "metrics"],
  "witself-argocd-server" => ["argocd", "argocd", "argocd-server", "metrics"],
}
monitors = documents(serving_render).select { |doc| doc["kind"] == "PodMonitor" }
abort "unexpected platform PodMonitor set" unless monitors.map { |doc| doc.dig("metadata", "name") }.sort == expected_monitors.keys.sort
monitors.each do |doc|
  namespace, instance, name, port = expected_monitors.fetch(doc.dig("metadata", "name"))
  abort "platform PodMonitor is outside discovery scope" unless doc.dig("metadata", "namespace") == "monitoring" && doc.dig("metadata", "labels", "release") == "witself-monitoring"
  abort "platform PodMonitor targets the wrong namespace" unless doc.dig("spec", "namespaceSelector") == {"matchNames" => [namespace]}
  expected_labels = {"app.kubernetes.io/instance" => instance, "app.kubernetes.io/name" => name}
  expected_labels["app.kubernetes.io/component"] = "controller" if namespace == "cert-manager"
  abort "platform PodMonitor targets the wrong pods" unless doc.dig("spec", "selector", "matchLabels") == expected_labels
  abort "platform PodMonitor endpoint differs" unless doc.dig("spec", "podMetricsEndpoints") == [{"port" => port, "path" => "/metrics", "interval" => "30s"}]
end

serving_cell_values = File.join(repo_root, ".gitops/cells/civo-sandbox-usw2-dev/values.yaml")
gates = %w[nodeExporter.enabled kubelet.cadvisor defaultRules.enabled certManager.enabled argocd.enabled]
defaults = YAML.safe_load(File.read(File.join(platform_chart, "values.yaml")), aliases: false)
gates.each do |gate|
  abort "#{gate} must default off" unless defaults.dig("platform", "monitoring", *gate.split(".")) == false
end
disabled_render = render_platform(platform_chart, serving_cell_values, *gates.flat_map { |gate| ["--set", "platform.monitoring.#{gate}=false"] })
disabled_values = monitoring_values(disabled_render)
abort "disabled node exporter still enabled" unless disabled_values.dig("nodeExporter", "enabled") == false
abort "disabled cAdvisor still enabled" unless disabled_values.dig("kubelet", "serviceMonitor", "cAdvisor") == false
abort "disabled upstream rules still enabled" unless disabled_values.dig("defaultRules", "create") == false
abort "disabled platform monitors still rendered" if documents(disabled_render).any? { |doc| doc["kind"] == "PodMonitor" }
disabled_groups = disabled_values.fetch("additionalPrometheusRulesMap", {}).values.flat_map { |value| Array(value["groups"]) }
abort "disabled custom platform alerts still rendered" if disabled_groups.flat_map { |group| group.fetch("rules") }.any? { |rule| custom_names.include?(rule["alert"]) }

%w[certManager argocd].each do |target|
  raw = render_platform(platform_chart, serving_cell_values, "--set", "platform.monitoring.#{target}.enabled=false")
  docs = documents(raw)
  disallowed = target == "certManager" ? ["witself-cert-manager"] : expected_monitors.keys.grep(/argocd/)
  abort "#{target} PodMonitor gate ignored" if docs.any? { |doc| doc["kind"] == "PodMonitor" && disallowed.include?(doc.dig("metadata", "name")) }
  required = expected_monitors.keys - disallowed
  abort "#{target} gate suppressed another target" unless docs.select { |doc| doc["kind"] == "PodMonitor" }.map { |doc| doc.dig("metadata", "name") }.sort == required.sort
  removed_group = target == "certManager" ? "platform-certificates" : "platform-argocd"
  abort "#{target} alert gate ignored" if platform_group_names(monitoring_values(raw)).include?(removed_group)
end

# Exercise each rule dependency independently. Existing scrape discovery must
# remain available when alerting is disabled; the alert and recording rules
# must never remain enabled after their own capability is turned off.
{
  "platform.monitoring.defaultRules.enabled" => %w[platform-certificates platform-argocd],
  "platform.monitoring.nodeExporter.enabled" => %w[platform-resources platform-storage platform-certificates platform-argocd],
  "platform.monitoring.alerting.enabled" => [],
  "platform.certManager.enabled" => %w[platform-resources platform-node platform-storage platform-argocd],
}.each do |gate, expected_groups|
  raw = render_platform(platform_chart, serving_cell_values, "--set", "#{gate}=false")
  values = monitoring_values(raw)
  abort "#{gate} left the wrong platform rule groups" unless platform_group_names(values).sort == expected_groups.sort
  if %w[platform.monitoring.defaultRules.enabled platform.monitoring.alerting.enabled].include?(gate)
    abort "#{gate} left upstream rules active" unless values.dig("defaultRules", "create") == false && values.dig("defaultRules", "rules").values.all? { |value| value == false }
  elsif gate == "platform.monitoring.nodeExporter.enabled"
    abort "node-exporter disabled but scrape/alerts remain active" unless values.dig("nodeExporter", "enabled") == false && values.dig("defaultRules", "rules", "nodeExporterAlerting") == false
    abort "node-exporter gate removed app/resource rules" unless values.dig("defaultRules", "rules", "kubernetesApps") == true && values.dig("defaultRules", "rules", "kubernetesResources") == true
  end
  expected_names = expected_monitors.keys
  expected_names -= ["witself-cert-manager"] if gate == "platform.certManager.enabled"
  actual_names = documents(raw).select { |doc| doc["kind"] == "PodMonitor" }.map { |doc| doc.dig("metadata", "name") }
  abort "#{gate} altered independent PodMonitor gates" unless actual_names.sort == expected_names.sort
  if gate == "platform.monitoring.alerting.enabled"
    abort "disabled alerting still emitted custom rules" unless values.fetch("additionalPrometheusRulesMap", {}).empty?
  end
end
stack_off = documents(render_platform(platform_chart, serving_cell_values, "--set", "platform.monitoring.enabled=false"))
abort "disabled stack retained monitoring resources" if stack_off.any? do |doc|
  %w[PodMonitor ServiceMonitor PrometheusRule].include?(doc["kind"]) || (doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring")
end

# Disabling only these capabilities removes the one new DaemonSet and changes
# no other workload. Scraping existing endpoints adds no deployments or pods.
disabled_values_path = File.join(scratch, "monitoring-extensions-disabled-values.yaml")
File.write(disabled_values_path, YAML.dump(disabled_values))
disabled_child = documents(command("helm", "template", "witself-monitoring", chart_archive, "--namespace", "monitoring", "--values", disabled_values_path))
workload_kinds = %w[Deployment StatefulSet DaemonSet Job]
existing_workloads = child_docs.select { |doc| workload_kinds.include?(doc["kind"]) && doc["kind"] != "DaemonSet" }
disabled_workloads = disabled_child.select { |doc| workload_kinds.include?(doc["kind"]) }
abort "monitoring extensions changed more than the node-exporter workload" unless existing_workloads == disabled_workloads

puts "monitoring extension renders passed: exact upstream allowlist, default-off gates, backup bytes, scrapes, +20m/32Mi requests per node"
