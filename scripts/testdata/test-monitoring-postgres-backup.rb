#!/usr/bin/env ruby
# Exercise the opt-in through both Helm charts, then run the same Promtool
# timelines against the enabled rules and each disabled render.
require 'yaml'
require 'open3'
require 'json'

root, tmp, child_chart, promtool = ARGV
abort 'usage: test-monitoring-postgres-backup.rb ROOT TMPDIR CHILD_CHART PROMTOOL' unless promtool
chart = File.join(root, '.gitops/charts/platform')
source = YAML.safe_load(File.read(File.join(chart, 'files/postgres-backup.rules.yaml')))
fixture = YAML.safe_load(File.read(File.join(chart, 'testdata/postgres-backup.rules.test.yaml')), aliases: true)
expected_names = %w[WitselfPostgresBackupFailed WitselfPostgresBackupStale]
abort 'backup alert inventory changed' unless source.fetch('groups').flat_map { |group| group.fetch('rules').map { |rule| rule.fetch('alert') } }.sort == expected_names
defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')))
abort 'backup alerts must default off' unless defaults.dig('platform', 'monitoring', 'postgresBackupAlerts', 'enabled') == false
apps_schema = JSON.parse(File.read(File.join(root, '.gitops/charts/apps/values.schema.json')))
deadline_bound = apps_schema.dig('properties', 'apps', 'properties', 'civoPostgres', 'properties', 'backup', 'properties', 'activeDeadlineSeconds', 'maximum')
abort 'unfinished backup alert must cover the maximum permitted job runtime' unless deadline_bound == 7200
_stdout, _stderr, invalid_status = Open3.capture3('helm', 'template', 'backup-alert-test', chart,
  '--values', File.join(chart, 'ci/monitoring-values.yaml'),
  '--set-string', 'platform.monitoring.postgresBackupAlerts.enabled=true')
abort 'backup alert schema accepted a truthy non-boolean switch' if invalid_status.success?

run = lambda do |*command|
  stdout, stderr, status = Open3.capture3(*command)
  abort "backup alert test command failed: #{stderr.gsub(/[a-fA-F0-9]{64}/, '[redacted-hex]')}" unless status.success?
  stdout
end
render_parent = lambda do |values_path, *overrides|
  rendered = run.call('helm', 'template', 'backup-alert-test', chart,
    '--values', values_path, *overrides)
  app = YAML.load_stream(rendered).compact.find { |doc| doc.dig('metadata', 'name') == 'witself-monitoring' }
  app ? YAML.safe_load(app.dig('spec', 'source', 'helm', 'values')) : {}
end
monitoring_values = File.join(chart, 'ci/monitoring-values.yaml')
base = render_parent.call(monitoring_values, '--set', 'apps.civoPostgres.backup.enabled=true')
abort 'backup rules rendered without explicit opt-in' if base.dig('additionalPrometheusRulesMap', 'postgres-backup')

cases = {
  'enabled' => [],
  'backup-disabled' => ['--set', 'apps.civoPostgres.backup.enabled=false'],
  'backup-unconfigured' => ['--set', 'apps=null'],
  'backup-alerts-disabled' => ['--set', 'platform.monitoring.postgresBackupAlerts.enabled=false'],
  'alerting-disabled' => ['--set', 'platform.monitoring.alerting.enabled=false'],
  'monitoring-disabled' => ['--set', 'platform.monitoring.enabled=false'],
}
cases = cases.map do |name, overrides|
  [name, monitoring_values,
   ['--set', 'apps.civoPostgres.backup.enabled=true', '--set', 'platform.monitoring.postgresBackupAlerts.enabled=true', *overrides],
   %w[enabled alerting-disabled].include?(name)]
end
# Exercise the replacement serving cell and the unmonitored backup cell using
# their actual values, without inheriting the CI fixture's enabled stack.
%w[civo-sandbox-use1-serving civo-sandbox-use1-backup].each do |cell|
  values_path = File.join(root, '.gitops/cells', cell, 'values.yaml')
  values = YAML.safe_load(File.read(values_path))
  abort "#{cell}: backup must remain default off" unless values.dig('apps', 'civoPostgres', 'backup', 'enabled') == false &&
    values.dig('platform', 'monitoring', 'postgresBackupAlerts', 'enabled') == false
  cases << ["#{cell}-disabled", values_path, [], false]
  cases << ["#{cell}-enabled", values_path,
            ['--set', 'apps.civoPostgres.backup.enabled=true', '--set', 'platform.monitoring.postgresBackupAlerts.enabled=true'],
            cell == 'civo-sandbox-use1-serving']
end
cases.each do |name, values_path, overrides, enabled|
  values = render_parent.call(values_path, *overrides)
  group = values.dig('additionalPrometheusRulesMap', 'postgres-backup')
  abort "#{name}: backup source/gate mismatch" unless group == (enabled ? source : nil)
  if name == 'enabled'
    unrelated = Marshal.load(Marshal.dump(values))
    unrelated.fetch('additionalPrometheusRulesMap').delete('postgres-backup')
    abort 'backup opt-in changed unrelated monitoring values' unless unrelated == base
  end
  if name == 'alerting-disabled'
    config = values.dig('alertmanager', 'config')
    abort "#{name}: backup activation changed null routing" unless config['receivers'] == [{'name' => 'null'}] &&
      config.dig('route', 'receiver') == 'null' && config.dig('route', 'routes') == [] &&
      Array(values.dig('alertmanager', 'alertmanagerSpec', 'secrets')).empty?
    abort "#{name}: backup activation enabled unrelated alerts" unless values.fetch('additionalPrometheusRulesMap').keys == ['postgres-backup']
  end
  if name == 'civo-sandbox-use1-serving-enabled'
    # The serving cell routes alerts (monitoring phase 3). Backup activation must not
    # touch Alertmanager routing, receiver Secrets, or any other rule group.
    without = render_parent.call(values_path)
    abort "#{name}: backup activation changed alert routing or receiver mounts" unless values['alertmanager'] == without['alertmanager']
    abort "#{name}: backup activation enabled unrelated alerts" unless
      (values.fetch('additionalPrometheusRulesMap').keys - ['postgres-backup']).sort == without.fetch('additionalPrometheusRulesMap', {}).keys.sort
  end

  values_path = File.join(tmp, "backup-#{name}-child-values.yaml")
  File.write(values_path, YAML.dump(values))
  child = values.empty? ? [] : YAML.load_stream(run.call('helm', 'template', 'witself-monitoring', child_chart,
    '--namespace', 'monitoring', '--values', values_path)).compact
  rules = child.select do |doc|
    doc['kind'] == 'PrometheusRule' && Array(doc.dig('spec', 'groups')).any? { |entry| entry['name'] == 'witself-postgres-backup' }
  end
  if enabled
    abort 'backup PrometheusRule must be unique and selected by Prometheus' unless rules.length == 1 &&
      rules[0].dig('metadata', 'namespace') == 'monitoring' && rules[0].dig('metadata', 'labels', 'release') == 'witself-monitoring'
    abort 'rendered backup rules differ from tested source' unless rules[0].fetch('spec') == source
  else
    abort "#{name}: disabled backup PrometheusRule rendered" unless rules.empty?
  end
  rule_path = File.join(tmp, "backup-#{name}.rules.yaml")
  fixture_path = File.join(tmp, "backup-#{name}.rules.test.yaml")
  File.write(rule_path, YAML.dump('groups' => rules.flat_map { |rule| rule.fetch('spec').fetch('groups') }))
  tests = Marshal.load(Marshal.dump(fixture))
  tests['rule_files'] = [rule_path]
  unless enabled
    tests.fetch('tests').each do |test|
      test.fetch('alert_rule_test').each { |assertion| assertion['exp_alerts'] = [] }
    end
  end
  File.write(fixture_path, YAML.dump(tests))
  run.call(promtool, 'check', 'rules', rule_path)
  run.call(promtool, 'test', 'rules', fixture_path)
end
puts 'PostgreSQL backup rendered alert, default-off, and gate-off Promtool checks passed'
