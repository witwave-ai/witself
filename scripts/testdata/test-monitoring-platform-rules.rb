#!/usr/bin/env ruby
# Test the exact pinned child-chart output, including upstream alert expressions.
# Fixtures inspect ALERTS to prove the real `for` periods without copying upstream
# annotation templates into expected results. The rule labels are checked here.
require 'yaml'

render, promtool, tmp = ARGV
abort 'usage: test-monitoring-platform-rules.rb CHILD_RENDER PROMTOOL TMPDIR' unless tmp
root = File.expand_path('../..', __dir__)
testdata = File.join(root, '.gitops/charts/platform/testdata')
upstream_alerts = %w[
  NodeMemoryHighUtilization NodeMemoryMajorPagesFaults NodeClockSkewDetected
  NodeClockNotSynchronising KubePodCrashLooping KubePodNotReady
  KubeDeploymentReplicasMismatch KubeStatefulSetReplicasMismatch KubeContainerWaiting
  KubeCPUOvercommit KubeMemoryOvercommit
].freeze
platform_alerts = %w[
  NodeFilesystemSpaceFillingUp WitselfPrometheusPVCUsageHigh
  WitselfCertificateExpiringSoon WitselfArgoApplicationUnhealthy
].freeze
records = %w[
  namespace_cpu:kube_pod_container_resource_requests:sum
  namespace_memory:kube_pod_container_resource_requests:sum
].freeze
groups = YAML.load_stream(File.read(render)).compact
  .select { |doc| doc['kind'] == 'PrometheusRule' }
  .flat_map { |doc| doc.fetch('spec').fetch('groups') }

[
  ['upstream', upstream_alerts, records],
  ['platform', platform_alerts, []],
].each do |name, alerts, recording_names|
  selected = groups.map do |group|
    rules = group.fetch('rules').select do |rule|
      alerts.include?(rule['alert']) || recording_names.include?(rule['record'])
    end
    group.merge('rules' => rules) unless rules.empty?
  end.compact
  rules = selected.flat_map { |group| group.fetch('rules') }
  actual = rules.map { |rule| rule['alert'] || rule['record'] }.sort
  abort "#{name} rendered rule inventory changed: #{actual.inspect}" unless actual == (alerts + recording_names).sort
  rules.each do |rule|
    abort "#{name} rule missing incident label: #{rule.inspect}" unless rule.dig('labels', 'witself_alert') == 'true'
  end

  fixture = YAML.safe_load(File.read(File.join(testdata, "#{name}.rules.test.yaml")), aliases: false)
  assertions = fixture.fetch('tests').flat_map { |test| test.fetch('promql_expr_test') }
  alerts.each do |alert|
    checks = assertions.select { |check| check.fetch('expr').include?("ALERTS{alertname=\"#{alert}\",alertstate=\"firing\"}") }
    abort "#{alert} lacks a firing test" unless checks.any? { |check| check.fetch('exp_samples').any? { |sample| sample['value'] == 1 } }
    abort "#{alert} lacks a silent test" unless checks.any? { |check| check.fetch('exp_samples').empty? }
  end
  recording_names.each do |record|
    checks = assertions.select { |check| check.fetch('expr') == record }
    abort "#{record} lacks nonempty and empty tests" unless checks.any? { |check| !check.fetch('exp_samples').empty? } && checks.any? { |check| check.fetch('exp_samples').empty? }
    fixture.fetch('tests').each do |test|
      abort "#{record} must be computed from raw metrics, never mocked" if test.fetch('input_series', []).any? { |input| input.fetch('series').start_with?(record) }
    end
  end

  rule_path = File.join(tmp, "#{name}-rendered.rules.yaml")
  fixture_path = File.join(tmp, "#{name}-rendered.rules.test.yaml")
  File.write(rule_path, YAML.dump('groups' => selected))
  fixture['rule_files'] = [rule_path]
  # Resource alerts depend on the two recording rules. Evaluate their group
  # first to make the test timeline deterministic across YAML document order.
  fixture['group_eval_order'] = selected.sort_by do |group|
    group.fetch('rules').any? { |rule| recording_names.include?(rule['record']) } ? 0 : 1
  end.map { |group| group.fetch('name') }
  File.write(fixture_path, YAML.dump(fixture))
  abort "#{name} rendered rules are invalid" unless system(promtool, 'check', 'rules', rule_path)
  abort "#{name} rule unit tests failed" unless system(promtool, 'test', 'rules', fixture_path)
end
