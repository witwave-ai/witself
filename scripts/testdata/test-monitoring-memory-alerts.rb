#!/usr/bin/env ruby
# Verify the independent memory opt-in without changing any receiver, then
# exercise the selected upstream PrometheusRule and the same disabled fixtures.
require 'yaml'
require 'open3'

root, tmp, child_chart, promtool = ARGV
abort 'usage: test-monitoring-memory-alerts.rb ROOT [TMPDIR CHILD_CHART PROMTOOL]' unless root && [1, 4].include?(ARGV.length)
chart = File.join(root, '.gitops/charts/platform')
source_path = File.join(chart, 'files/memory.rules.yaml')
source = YAML.safe_load(File.read(source_path))
fixture = YAML.safe_load(File.read(File.join(chart, 'testdata/memory.rules.test.yaml')), aliases: true)
expected_names = %w[WitselfMemoryCurationBacklogAgeHigh WitselfMemoryCurationLeaseExpiryRatioHigh WitselfMemoryCurationRunFailureRatioHigh]
abort 'memory group must be independently named' unless source.fetch('groups').map { |group| group.fetch('name') } == ['witself-memory']
rules = source.fetch('groups').flat_map { |group| group.fetch('rules') }
abort 'memory alert inventory changed' unless rules.map { |rule| rule.fetch('alert') } == expected_names
abort 'memory alert labels must be fixed and value-free' unless rules.all? do |rule|
  rule.fetch('labels') == {'severity' => 'warning', 'service' => 'memory-curation', 'witself_alert' => 'true'}
end
defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')))
abort 'memory alerts must default off' unless defaults.dig('platform', 'monitoring', 'memoryAlerts', 'enabled') == false
monitoring_values = File.join(chart, 'ci/monitoring-values.yaml')
_stdout, _stderr, invalid_status = Open3.capture3('helm', 'template', 'memory-alert-test', chart,
  '--values', monitoring_values, '--set-string', 'platform.monitoring.memoryAlerts.enabled=true')
abort 'memory alert schema accepted a truthy non-boolean switch' if invalid_status.success?

run = lambda do |*command|
  stdout, stderr, status = Open3.capture3(*command)
  abort "memory alert test failed: #{stderr.gsub(/[a-fA-F0-9]{64}/, '[redacted-hex]')}#{stdout.gsub(/[a-fA-F0-9]{64}/, '[redacted-hex]')}" unless status.success?
  stdout
end
render_parent = lambda do |values_path, *overrides|
  rendered = run.call('helm', 'template', 'memory-alert-test', chart, '--values', values_path, *overrides)
  app = YAML.load_stream(rendered).compact.find { |doc| doc.dig('metadata', 'name') == 'witself-monitoring' }
  app ? YAML.safe_load(app.dig('spec', 'source', 'helm', 'values')) : {}
end
base = render_parent.call(monitoring_values)
abort 'memory rules rendered without explicit opt-in' if base.dig('additionalPrometheusRulesMap', 'memory')
cases = [
  ['enabled', monitoring_values, ['--set', 'platform.monitoring.memoryAlerts.enabled=true'], true],
  ['disabled', monitoring_values, ['--set', 'platform.monitoring.memoryAlerts.enabled=false'], false],
  ['alerting-disabled', monitoring_values, ['--set', 'platform.monitoring.memoryAlerts.enabled=true', '--set', 'platform.monitoring.alerting.enabled=false'], false],
  ['monitoring-disabled', monitoring_values, ['--set', 'platform.monitoring.memoryAlerts.enabled=true', '--set', 'platform.monitoring.enabled=false'], false],
]
%w[civo-sandbox-use1-serving civo-sandbox-use1-backup].each do |cell|
  path = File.join(root, '.gitops/cells', cell, 'values.yaml')
  cell_values = YAML.safe_load(File.read(path))
  abort "#{cell}: memory alerts must be explicitly off" unless cell_values.dig('platform', 'monitoring', 'memoryAlerts', 'enabled') == false
  cases << ["#{cell}-disabled", path, [], false]
  cases << ["#{cell}-enabled", path, ['--set', 'platform.monitoring.memoryAlerts.enabled=true'], cell.end_with?('-serving')]
end

cases.each do |name, path, overrides, enabled|
  values = render_parent.call(path, *overrides)
  group = values.dig('additionalPrometheusRulesMap', 'memory')
  abort "#{name}: memory gate/source mismatch" unless group == (enabled ? source : nil)
  if enabled
    unrelated = Marshal.load(Marshal.dump(values))
    unrelated.fetch('additionalPrometheusRulesMap').delete('memory')
    original = path == monitoring_values ? base : render_parent.call(path)
    abort "#{name}: memory opt-in changed receivers or unrelated monitoring settings" unless unrelated == original
  end
  next unless child_chart

  child = []
  unless values.empty?
    values_path = File.join(tmp, "memory-#{name}-values.yaml")
    File.write(values_path, values.to_yaml)
    child = YAML.load_stream(run.call('helm', 'template', 'witself-monitoring', child_chart,
      '--namespace', 'monitoring', '--values', values_path)).compact
  end
  selected = child.select do |doc|
    doc['kind'] == 'PrometheusRule' && Array(doc.dig('spec', 'groups')).any? { |item| item['name'] == 'witself-memory' }
  end
  abort "#{name}: unexpected selected memory rule count" unless selected.length == (enabled ? 1 : 0)
  if enabled
    rule = selected.fetch(0)
    abort "#{name}: rule outside Prometheus selection" unless rule.dig('metadata', 'namespace') == 'monitoring' && rule.dig('metadata', 'labels', 'release') == 'witself-monitoring'
    abort "#{name}: upstream chart changed memory rules" unless rule.fetch('spec') == source
  end
  rule_path = File.join(tmp, "memory-#{name}.rules.yaml")
  File.write(rule_path, (enabled ? selected.fetch(0).fetch('spec') : {'groups' => []}).to_yaml)
  tests = Marshal.load(Marshal.dump(fixture))
  tests['rule_files'] = [rule_path]
  unless enabled
    tests.fetch('tests').each do |test|
      test.fetch('alert_rule_test').each { |check| check['exp_alerts'] = [] }
    end
  end
  test_path = File.join(tmp, "memory-#{name}.test.yaml")
  File.write(test_path, tests.to_yaml)
  run.call(promtool, 'check', 'rules', rule_path)
  run.call(promtool, 'test', 'rules', test_path)
end
if promtool
  run.call(promtool, 'check', 'rules', source_path)
  run.call(promtool, 'test', 'rules', File.join(chart, 'testdata/memory.rules.test.yaml'))
  puts 'memory alert gates, receivers, upstream selection, and Promtool checks passed'
else
  puts 'memory alert parent-chart gates and receiver preservation checks passed'
end
