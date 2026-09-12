#!/usr/bin/env ruby
# Collector and sealed-plane alerts require separate metrics-first rollouts.
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
sealed_alerts = %w[
  WitselfSealedPlanePostureMetricsUnavailable WitselfSecretMaterialDeliveryErrorRatio
  WitselfVaultRotationFenceConflicts WitselfVaultRotationStuckOpen
  WitselfSecretMaterialDeliveryVolumeAnomaly
].freeze
abort 'sealed-plane alert inventory changed' unless source_rules.count { |rule| sealed_alerts.include?(rule['alert']) } == 5
filtered = lambda do |*excluded|
  source.merge('groups' => source.fetch('groups').reject { |group| excluded.include?(group.fetch('name')) })
end
without_collectors = filtered.call('witself-collectors')
without_sealed = filtered.call('witself-sealed-plane')
without_opt_ins = filtered.call('witself-collectors', 'witself-sealed-plane')

render = lambda do |values, *overrides|
  stdout, stderr, status = Open3.capture3('helm', 'template', 'review-capacity', chart,
    '--values', values, *overrides)
  abort "gated alert render failed: #{stderr}" unless status.success?
  app = YAML.load_stream(stdout).compact.find { |doc| doc.dig('metadata', 'name') == 'witself-monitoring' }
  app ? YAML.safe_load(app.dig('spec', 'source', 'helm', 'values')) : {}
end

# The serving cell values and the PagerDuty fixture omit both opt-ins. Neither
# may start paging for collector or sealed posture absence during chart rollout.
[
  File.join(root, '.gitops/cells/civo-sandbox-usw2-dev/values.yaml'),
  File.join(chart, 'ci/monitoring-pagerduty-values.yaml'),
].each do |values|
  disabled = render.call(values)
  abort "gated alerts rendered without an opt-in: #{values}" unless disabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_opt_ins
  enabled = render.call(values, '--set', 'platform.monitoring.collectorAlerts.enabled=true')
  abort 'collector opt-in did not preserve other rules and gates' unless enabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_sealed
  enabled['additionalPrometheusRulesMap']['founder-open-plane'] = without_opt_ins
  abort 'collector opt-in changed unrelated monitoring behavior' unless enabled == disabled
  sealed = render.call(values, '--set', 'platform.monitoring.sealedPlaneAlerts.enabled=true')
  abort 'sealed-plane opt-in did not preserve other rules and gates' unless sealed.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_collectors
  sealed['additionalPrometheusRulesMap']['founder-open-plane'] = without_opt_ins
  abort 'sealed-plane opt-in changed unrelated monitoring behavior' unless sealed == disabled
  both = render.call(values, '--set', 'platform.monitoring.collectorAlerts.enabled=true',
    '--set', 'platform.monitoring.sealedPlaneAlerts.enabled=true')
  abort 'both opt-ins did not preserve the complete source rules' unless both.dig('additionalPrometheusRulesMap', 'founder-open-plane') == source
end

fixture = File.join(chart, 'ci/monitoring-values.yaml')
enabled = render.call(fixture)
abort 'CI fixture must exercise every collector and sealed-plane alert' unless enabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == source
%w[platform.monitoring.enabled platform.monitoring.alerting.enabled].each do |gate|
  disabled = render.call(fixture, '--set', "#{gate}=false")
  abort "gated alerts bypassed #{gate}" if disabled.key?('additionalPrometheusRulesMap')
end
disabled = render.call(fixture, '--set', 'platform.monitoring.collectorAlerts.enabled=false')
abort 'explicit collector opt-out did not preserve existing alerts' unless disabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_collectors
disabled = render.call(fixture, '--set', 'platform.monitoring.sealedPlaneAlerts.enabled=false')
abort 'explicit sealed-plane opt-out did not preserve existing alerts' unless disabled.dig('additionalPrometheusRulesMap', 'founder-open-plane') == without_sealed
defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')))
abort 'collector alerts must default off' unless defaults.dig('platform', 'monitoring', 'collectorAlerts', 'enabled') == false

abort 'sealed-plane alerts must default off' unless defaults.dig('platform', 'monitoring', 'sealedPlaneAlerts', 'enabled') == false

puts 'collector and sealed-plane alert rollout gate checks passed'
