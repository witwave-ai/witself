#!/usr/bin/env ruby
# Resolve every rendered alert against the chart's real Alertmanager routes.
require "yaml"
require "base64"
require "open3"
require "fileutils"

abort "usage: test-monitoring-alert-routing.rb REPO_ROOT SCRATCH CHART_ARCHIVE AMTOOL" unless ARGV.length == 4
repo_root, scratch, chart_archive, amtool = ARGV
output_dir = File.join(scratch, "alert-routing")
FileUtils.mkdir_p(output_dir)
platform_chart = File.join(repo_root, ".gitops/charts/platform")

def command(*args)
  stdout, stderr, status = Open3.capture3(*args)
  abort "#{File.basename(args.first)} failed:\n#{stderr.gsub(/[0-9a-fA-F]{64}/, '[digest redacted]')}" unless status.success?
  stdout
end

def label_argument(name, value)
  escaped = value.to_s.gsub(/["\\]/) { |character| "\\#{character}" }
  "#{name}=\"#{escaped}\""
end

def route_test(amtool, config_path, rule, expected)
  labels = rule.fetch("labels", {}).reject { |_, value| value.to_s.include?("{{") }
  Open3.capture3(amtool, "config", "routes", "test", "--config.file=#{config_path}",
    "--verify.receivers=#{expected}", label_argument("alertname", rule.fetch("alert")),
    *labels.map { |name, value| label_argument(name, value) })
end

collector_names = %w[
  WitselfIdentityCapacityMetricsUnavailable WitselfIdentityCapacityAtLimit
  WitselfAuditAppendMetricsUnavailable WitselfAuditAppendFailures
].freeze
render_inputs = [
  ["pagerduty", File.join(platform_chart, "ci/monitoring-pagerduty-values.yaml"),
    ["--set", "platform.monitoring.collectorAlerts.enabled=true",
      "--set", "platform.monitoring.sealedPlaneAlerts.enabled=true"]],
  ["serving", File.join(repo_root, ".gitops/cells/civo-sandbox-use1-serving/values.yaml"),
    ["--set", "platform.monitoring.collectorAlerts.enabled=true"]],
]

alert_count = 0
render_inputs.each do |name, values_path, overrides|
  platform_raw = command("helm", "template", "witself-platform", platform_chart,
    "--values", values_path, *overrides)
  app = YAML.load_stream(platform_raw).compact.find do |doc|
    doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-monitoring"
  end
  abort "#{name}: monitoring Application missing" unless app
  child_values_path = File.join(output_dir, "#{name}-child-values.yaml")
  File.write(child_values_path, app.fetch("spec").fetch("source").fetch("helm").fetch("values"))
  child_raw = command("helm", "template", "witself-monitoring", chart_archive,
    "--namespace", "monitoring", "--values", child_values_path)
  File.write(File.join(output_dir, "#{name}-child.yaml"), child_raw)
  docs = YAML.load_stream(child_raw).compact
  secret = docs.find do |doc|
    doc["kind"] == "Secret" && doc.dig("metadata", "name").to_s.start_with?("alertmanager-") &&
      (doc.dig("data", "alertmanager.yaml") || doc.dig("stringData", "alertmanager.yaml"))
  end
  abort "#{name}: Alertmanager config missing" unless secret
  raw = secret.dig("stringData", "alertmanager.yaml") || Base64.strict_decode64(secret.dig("data", "alertmanager.yaml"))
  config_path = File.join(output_dir, "#{name}-alertmanager.yaml")
  File.binwrite(config_path, raw)

  groups = docs.select { |doc| doc["kind"] == "PrometheusRule" }
    .flat_map { |doc| Array(doc.dig("spec", "groups")) }
  collectors = groups.select { |group| group["name"] == "witself-collectors" }
  abort "#{name}: collector group must contain exactly the four collector alerts" unless collectors.length == 1 &&
    collectors[0].fetch("rules").map { |rule| rule["alert"] }.sort == collector_names.sort
  alerts = groups.flat_map { |group| group.fetch("rules") }.select { |rule| rule["alert"] }
  at_limit = alerts.find { |rule| rule["alert"] == "WitselfIdentityCapacityAtLimit" }
  abort "#{name}: identity saturation must be informational and unrouted" unless
    at_limit.fetch("labels") == {"severity" => "info", "service" => "identity"}
  unrouted = alerts.reject { |rule| rule.dig("labels", "witself_alert") == "true" }.map { |rule| rule.fetch("alert") }
  abort "#{name}: unexpected alerts without the incident label: #{unrouted.inspect}" unless
    unrouted.sort == %w[WitselfIdentityCapacityAtLimit WitselfWatchdog]

  alerts.each do |rule|
    expected = case rule.fetch("alert")
    when "WitselfWatchdog" then "witself-deadman"
    when "WitselfIdentityCapacityAtLimit" then "null"
    else "witself-external"
    end
    stdout, _, status = route_test(amtool, config_path, rule, expected)
    abort "#{name}: #{rule.fetch('alert')} did not resolve to #{expected}" unless
      status.success? && stdout.lines.first.to_s.strip == expected
  end

  {
    "WitselfIdentityCapacityAtLimit" => "witself-external",
    "WitselfIdentityCapacityMetricsUnavailable" => "null",
  }.each do |alert_name, wrong_receiver|
    rule = alerts.find { |candidate| candidate["alert"] == alert_name }
    _, _, status = route_test(amtool, config_path, rule, wrong_receiver)
    abort "#{name}: negative control accepted #{alert_name} at #{wrong_receiver}" if status.success?
  end
  alert_count += alerts.length
  puts "#{name}: alert routing checks passed (#{alerts.length} alerts)"
end
puts "alert routing checks passed (#{alert_count} alerts in 2 renders)"
