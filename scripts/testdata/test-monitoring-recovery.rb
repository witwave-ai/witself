#!/usr/bin/env ruby
# Render each recovery patch in an isolated fixture. An optional verified
# kube-prometheus-stack archive also exercises the upstream stack resources.
require "yaml"
require "open3"
require "tmpdir"
require "fileutils"
require "base64"

root, stack_archive = ARGV
abort "usage: #{$PROGRAM_NAME} REPO_ROOT [VERIFIED_STACK_ARCHIVE]" unless root
root = File.expand_path(root)
cell_name = "civo-sandbox-use1-serving"
old_cell_name = "civo-sandbox-usw2-dev"
cell_path = ".gitops/cells/#{cell_name}/values.yaml"
fixture_paths = [".gitops/cells/catalog.yaml", cell_path,
                 "internal/gitopsvalues/overlays/#{cell_name}.yaml.tmpl"]
apps_chart = File.join(root, ".gitops/charts/apps")
platform_chart = File.join(root, ".gitops/charts/platform")
server_chart = File.join(root, "charts/witself-server")
reference_path = File.join(root, ".gitops/cells/#{old_cell_name}/values.yaml")

def check(message, condition)
  abort "monitoring recovery: #{message}" unless condition
end

def command(*args, **options)
  output, errors, status = Open3.capture3(*args, **options)
  abort "monitoring recovery command failed: #{args.first}\n#{errors}" unless status.success?
  output
end

def render(release, chart, values, *options)
  # Backups are orthogonal to the monitoring phases: render every phase with the
  # backup switch off so the reconstructed phase 1 has no exporter regardless of
  # the cell's live postgres_backup setting.
  backup_off = ["--set", "apps.civoPostgres.backup.enabled=false",
                "--set", "platform.monitoring.postgresBackupAlerts.enabled=false"]
  YAML.load_stream(command("helm", "template", release, chart, "--values", values, *backup_off, *options)).compact
end

def resource(documents, kind, name)
  matches = documents.select { |doc| doc["kind"] == kind && doc.dig("metadata", "name") == name }
  check("expected one #{kind}/#{name}", matches.length == 1)
  matches.first
end

def child_values(documents, name)
  YAML.safe_load(resource(documents, "Application", name).dig("spec", "source", "helm", "values"), aliases: false)
end

def selected(documents, kind)
  documents.select { |doc| doc["kind"] == kind }.sort_by { |doc| doc.dig("metadata", "name") }
end

def metrics_ingress(policy)
  Array(policy.dig("spec", "ingress")).select do |ingress|
    Array(ingress["ports"]).any? { |port| ["metrics", 9187].include?(port["port"]) }
  end
end

def alertmanager_config(documents)
  secrets = documents.select do |doc|
    doc["kind"] == "Secret" &&
      (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml"))
  end
  check("expected one rendered Alertmanager configuration", secrets.length == 1)
  raw = secrets[0].dig("stringData", "alertmanager.yaml") ||
        Base64.strict_decode64(secrets[0].dig("data", "alertmanager.yaml"))
  YAML.safe_load(raw, aliases: false)
end

def no_external_alerting(values, label)
  config = values.dig("alertmanager", "config")
  check("#{label}: Alertmanager must be null-routed", config["receivers"] == [{"name" => "null"}] &&
        config.dig("route", "receiver") == "null" && config.dig("route", "routes") == [])
  check("#{label}: no receiver Secret mounts", Array(values.dig("alertmanager", "alertmanagerSpec", "secrets")).empty?)
  check("#{label}: no Witself rules", values.fetch("additionalPrometheusRulesMap", {}).empty?)
  check("#{label}: no upstream alert rules", values.dig("defaultRules", "create") == false)
end

expected_peer = {
  "namespaceSelector" => {"matchLabels" => {"kubernetes.io/metadata.name" => "monitoring"}},
  "podSelector" => {"matchLabels" => {"app.kubernetes.io/name" => "prometheus"}}
}
reference_apps = render("witself-apps", apps_chart, reference_path)
reference_pg = child_values(reference_apps, "witself-postgresql")
# Recovery explicitly opts into collector alerts, whose old-cell catalog
# switch remains false. Compare with the same reviewed old-cell capability on.
reference_monitoring = child_values(render("witself-platform", platform_chart, reference_path,
                                          "--set", "platform.monitoring.collectorAlerts.enabled=true"),
                                   "witself-monitoring")
reference_monitoring["commonLabels"]["witself.io/cell"] = cell_name

Dir.mktmpdir("witself-monitoring-recovery-") do |fixture|
  fixture_paths.each do |relative|
    destination = File.join(fixture, relative)
    FileUtils.mkdir_p(File.dirname(destination))
    FileUtils.cp(File.join(root, relative), destination)
  end
  apply_phase = lambda do |phase, reverse = false|
    args = ["git", "apply"]
    args << "--reverse" if reverse
    fixture_paths.each { |relative| args << "--include=#{relative}" }
    args << File.join(root, "scripts/testdata/monitoring-recovery/phase#{phase}.patch")
    command(*args, chdir: fixture)
  end
  fixture_cell = File.join(fixture, cell_path)
  current = YAML.safe_load(File.read(fixture_cell), aliases: false)
  # The same tests remain valid after either later GitOps change lands.
  current_phase = if current.dig("platform", "monitoring", "alerting", "enabled")
                    3
                  elsif current.dig("apps", "witselfServer", "metrics", "serviceMonitor", "enabled")
                    2
                  else
                    1
                  end
  current_phase.downto(2) { |phase| apply_phase.call(phase, true) }

  reference_server_values = File.join(fixture, "reference-server-values.yaml")
  File.write(reference_server_values, child_values(reference_apps, "witself-server").to_yaml)
  reference_server = render("witself-server", server_chart, reference_server_values, "--namespace", "witself")
  reference_stack = nil
  if stack_archive
    reference_values = File.join(fixture, "reference-monitoring-values.yaml")
    File.write(reference_values, reference_monitoring.to_yaml)
    reference_stack = render("witself-monitoring", stack_archive, reference_values,
                             "--namespace", "monitoring", "--include-crds")
  end

  baseline_monitoring = nil
  (1..3).each do |phase|
    apply_phase.call(phase) if phase > 1
    label = "phase #{phase}"
    values = YAML.safe_load(File.read(fixture_cell), aliases: false)
    check("#{label}: monitoring stack must be enabled", values.dig("platform", "monitoring", "enabled") == true)
    check("#{label}: recovery chart/image pins changed", %w[chartVersion imageTag].all? do |key|
      values.dig("apps", "witselfServer", key) == "0.0.289"
    end)
    apps = render("witself-apps", apps_chart, fixture_cell)
    pg = child_values(apps, "witself-postgresql")
    server = child_values(apps, "witself-server")
    server_values = File.join(fixture, "phase#{phase}-server-values.yaml")
    File.write(server_values, server.to_yaml)
    server_documents = render("witself-server", server_chart, server_values, "--namespace", "witself")
    monitors = selected(server_documents, "ServiceMonitor")
    pg_ingress = metrics_ingress(pg.fetch("extraDeploy").fetch(0))
    if phase == 1
      check("#{label}: no PostgreSQL exporter or ServiceMonitor", pg["metrics"] == {"enabled" => false})
      check("#{label}: no Witself ServiceMonitors", monitors.empty?)
      check("#{label}: no PostgreSQL metrics ingress", pg_ingress.empty?)
      [server, server.fetch("worker")].each do |component|
        check("#{label}: no metricsFrom peers", Array(component.dig("networkPolicy", "metricsFrom")).empty?)
      end
      selected(server_documents, "NetworkPolicy").each do |policy|
        check("#{label}: no server/worker metrics peers", metrics_ingress(policy).all? { |ingress| Array(ingress["from"]).empty? })
      end
    else
      check("#{label}: PostgreSQL monitor must match usw2-dev", pg["metrics"] == reference_pg["metrics"])
      check("#{label}: PostgreSQL exporter ingress must match usw2-dev", pg_ingress == metrics_ingress(reference_pg.fetch("extraDeploy").fetch(0)))
      check("#{label}: server/worker monitors must match usw2-dev", monitors == selected(reference_server, "ServiceMonitor"))
      check("#{label}: expected exactly two selected Witself monitors", monitors.length == 2 && monitors.all? { |doc| doc.dig("metadata", "labels", "release") == "witself-monitoring" })
      [server, server.fetch("worker")].each do |component|
        check("#{label}: metricsFrom must require namespace AND pod labels", component.dig("networkPolicy", "metricsFrom") == [expected_peer])
      end
      %w[witself-server witself-worker].each do |name|
        actual = metrics_ingress(resource(server_documents, "NetworkPolicy", name))
        check("#{label}: #{name} rendered metrics ingress must match usw2-dev", actual == metrics_ingress(resource(reference_server, "NetworkPolicy", name)) &&
              actual.length == 1 && actual[0]["from"] == [expected_peer])
      end
    end

    platform = render("witself-platform", platform_chart, fixture_cell)
    application = resource(platform, "Application", "witself-monitoring")
    check("#{label}: CRD sync contract", application.dig("spec", "syncPolicy", "syncOptions").sort == ["CreateNamespace=true", "ServerSideApply=true"])
    monitoring = child_values(platform, "witself-monitoring")
    if phase < 3
      check("#{label}: alerting must be explicitly disabled", values.dig("platform", "monitoring", "alerting", "enabled") == false)
      check("#{label}: no receiver references in cell values", %w[receiver receiverDeadman].none? { |key| values.fetch("platform").fetch("monitoring").key?(key) })
      no_external_alerting(monitoring, label)
      baseline_monitoring ||= monitoring
      check("phase 2 must preserve phase 1 monitoring stack", monitoring == baseline_monitoring)
    else
      check("#{label}: alerting and both gated rule groups enabled", %w[alerting collectorAlerts sealedPlaneAlerts].all? { |key| values.dig("platform", "monitoring", key, "enabled") == true })
      check("#{label}: monitoring values must match usw2-dev with collector alerts enabled", monitoring == reference_monitoring)
    end

    next unless stack_archive
    monitoring_values = File.join(fixture, "phase#{phase}-monitoring-values.yaml")
    File.write(monitoring_values, monitoring.to_yaml)
    stack = render("witself-monitoring", stack_archive, monitoring_values,
                   "--namespace", "monitoring", "--include-crds")
    required_crds = %w[alertmanagers prometheuses prometheusrules servicemonitors].map { |name| "#{name}.monitoring.coreos.com" }
    crds = selected(stack, "CustomResourceDefinition").map { |doc| doc.dig("metadata", "name") }
    check("#{label}: required stack CRDs missing", (required_crds - crds).empty?)
    check("#{label}: Prometheus and Alertmanager must render", selected(stack, "Prometheus").length == 1 && selected(stack, "Alertmanager").length == 1)
    check("#{label}: monitoring must remain private", selected(stack, "Ingress").empty? &&
          selected(stack, "Service").all? { |service| [nil, "ClusterIP"].include?(service.dig("spec", "type")) && Array(service.dig("spec", "externalIPs")).empty? })
    config = alertmanager_config(stack)
    alertmanager = selected(stack, "Alertmanager").fetch(0)
    if phase < 3
      check("#{label}: rendered null-only receiver", config["receivers"] == [{"name" => "null"}] && config.dig("route", "routes") == [])
      check("#{label}: rendered receiver mounts absent", Array(alertmanager.dig("spec", "secrets")).empty?)
      check("#{label}: rendered rules absent", selected(stack, "PrometheusRule").empty?)
      check("#{label}: Grafana and node exporter absent", stack.none? { |doc| doc.dig("metadata", "name").to_s.match?(/grafana|node-exporter/) })
    else
      check("#{label}: rendered PagerDuty and dead-man configuration must match usw2-dev", config == alertmanager_config(reference_stack))
      check("#{label}: exact immutable receiver mounts", alertmanager.dig("spec", "secrets").sort == %w[witself-monitoring-deadman-v1 witself-monitoring-pagerduty-v1])
      check("#{label}: rendered rules must match usw2-dev with collector alerts enabled", selected(stack, "PrometheusRule") == selected(reference_stack, "PrometheusRule"))
    end
  end
end

puts "replacement-cell monitoring phases 1/2/3 #{stack_archive ? 'stack and application' : 'application'} rendering checks passed"
