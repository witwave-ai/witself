#!/usr/bin/env ruby
# Collector alerts require a separate rollout after compatible metrics exist.
require 'yaml'
require 'open3'

root = ARGV.fetch(0)
chart = File.join(root, '.gitops/charts/platform')
collector_alerts = %w[
  WitselfIdentityCapacityMetricsUnavailable WitselfIdentityCapacityAtLimit
  WitselfAuditAppendMetricsUnavailable WitselfAuditAppendFailures
].freeze
source = YAML.safe_load(File.read(File.join(chart, 'files/founder-open-plane.rules.yaml')))
source_rules = source.fetch('groups').flat_map { |group| group.fetch('rules') }
abort 'collector alert inventory changed' unless source_rules.count { |rule| collector_alerts.include?(rule['alert']) } == 4
without_collectors = source.merge('groups' => source.fetch('groups').map do |group|
  rules = group.fetch('rules').reject { |rule| collector_alerts.include?(rule['alert']) }
  group.merge('rules' => rules) unless rules.empty?
end.compact)

render = lambda do |values, *overrides|
  stdout, stderr, status = Open3.capture3('helm', 'template', 'review-capacity', chart,
    '--values', values, *overrides)
  abort "collector alert render failed: #{stderr}" unless status.success?
  app = YAML.load_stream(stdout).compact.find { |doc| doc.dig('metadata', 'name') == 'witself-monitoring' }
  app ? YAML.safe_load(app.dig('spec', 'source', 'helm', 'values')) : {}
end

# The live serving cell and the PagerDuty fixture deliberately omit the new
# opt-in. Neither may start paging for collector absence during a chart rollout.
[
  File.join(root, '.gitops/cells/civo-sandbox-usw2-dev/values.yaml'),
  File.join(chart, 'ci/monitoring-pagerduty-values.yaml'),
].each do |values|
  disabled = render.call(values)
  abort "collector alerts rendered without an opt-in: #{values}" unless disabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_collectors
  enabled = render.call(values, '--set', 'platform.monitoring.collectorAlerts.enabled=true')
  abort 'explicit opt-in did not preserve the complete source rules' unless enabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == source
  enabled['additionalPrometheusRulesMap']['founder-open-plane'] = without_collectors
  abort 'collector opt-in changed unrelated monitoring behavior' unless enabled == disabled
end

fixture = File.join(chart, 'ci/monitoring-values.yaml')
enabled = render.call(fixture)
abort 'CI fixture must exercise every collector alert' unless enabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == source
%w[platform.monitoring.enabled platform.monitoring.alerting.enabled].each do |gate|
  disabled = render.call(fixture, '--set', "#{gate}=false")
  abort "collector alerts bypassed #{gate}" if disabled.key?('additionalPrometheusRulesMap')
end
disabled = render.call(fixture, '--set', 'platform.monitoring.collectorAlerts.enabled=false')
abort 'explicit collector opt-out did not preserve existing alerts' unless disabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_collectors
defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')))
abort 'collector alerts must default off' unless defaults.dig('platform', 'monitoring', 'collectorAlerts', 'enabled') == false

puts 'collector alert rollout gate checks passed'
