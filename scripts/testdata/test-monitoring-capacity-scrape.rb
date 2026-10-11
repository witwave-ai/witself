#!/usr/bin/env ruby
# Verify the rendered scrape contracts used by the capacity alerts. No cell
# values or deployment pins are read here; the roll-state matrix supplies renders.
require "yaml"

abort "usage: test-monitoring-capacity-scrape.rb RENDER" unless ARGV.length == 1
docs = YAML.load_stream(File.read(ARGV.fetch(0))).compact

def exactly_one(docs, kind, name, alert)
  matches = docs.select { |doc| doc["kind"] == kind && doc.dig("metadata", "name").to_s.include?(name) }
  abort "#{alert}: expected exactly one #{name} #{kind}" unless matches.length == 1
  matches.first
end

def monitor_scope(monitor, job_label, alert)
  abort "#{alert}: ServiceMonitor must be release-scoped in monitoring" unless monitor.dig("metadata", "namespace") == "monitoring" && monitor.dig("metadata", "labels", "release") == "witself-monitoring"
  abort "#{alert}: unexpected ServiceMonitor jobLabel" unless monitor.dig("spec", "jobLabel") == job_label
end

def protect_labels(endpoint, labels, alert)
  Array(endpoint["relabelings"]).each do |entry|
    abort "#{alert}: relabeling overwrites #{entry['targetLabel']}" if labels.include?(entry["targetLabel"])
  end
end

def survives?(endpoint, sample, alert)
  # Validate every action before emulation, including entries after a drop.
  entries = Array(endpoint["metricRelabelings"])
  entries.each do |entry|
    abort "#{alert}: unsupported metricRelabelings entry:\n#{YAML.dump(entry)}" unless %w[drop keep].include?(entry.fetch("action", "replace"))
  end
  entries.each do |entry|
    value = Array(entry["sourceLabels"]).map { |label| sample.fetch(label, "") }.join(entry.fetch("separator", ";"))
    pattern = Regexp.new("\\A(?:#{entry.fetch('regex', '(.*)')})\\z")
    matched = pattern.match?(value)
    return false if (entry["action"] == "drop" && matched) || (entry["action"] == "keep" && !matched)
  end
  true
end

memory_alert = "WitselfPostgreSQLMemoryNearLimit"
volume_alert = "WitselfPostgreSQLVolumeFillingUp"
node_alert = "WitselfNodeMemoryWorkingSetHigh"
node_rules = docs.select { |doc| doc["kind"] == "PrometheusRule" }
  .flat_map { |doc| Array(doc.dig("spec", "groups")) }
  .flat_map { |group| Array(group["rules"]) }
  .select { |rule| rule["alert"] == node_alert }
abort "#{node_alert}: expected exactly one rendered alert" unless node_rules.length == 1
node_numerator = <<~PROMQL
  last_over_time(node_memory_MemTotal_bytes{job="node-exporter"}[5m])
  - last_over_time(node_memory_MemFree_bytes{job="node-exporter"}[5m])
  - last_over_time(node_memory_Buffers_bytes{job="node-exporter"}[5m])
  - (last_over_time(node_memory_Cached_bytes{job="node-exporter"}[5m]) - last_over_time(node_memory_Shmem_bytes{job="node-exporter"}[5m]))
  - last_over_time(node_memory_SReclaimable_bytes{job="node-exporter"}[5m])
PROMQL
node_prefix = "max by (node) ((#{node_numerator}) * on (namespace, pod) group_left (node)"
abort "#{node_alert}: node working-set numerator changed" unless node_rules.first.fetch("expr").gsub(/\s+/, "").start_with?(node_prefix.gsub(/\s+/, ""))
ksm_alerts = "WitselfPostgreSQLOOMKilled/WitselfPostgreSQLRestarted/#{memory_alert}/#{node_alert}"
kubelet = exactly_one(docs, "ServiceMonitor", "kubelet", "#{memory_alert}/#{volume_alert}")
monitor_scope(kubelet, "k8s-app", "#{memory_alert}/#{volume_alert}")
kubelet_endpoints = kubelet.fetch("spec").fetch("endpoints")
kubelet_endpoints.each { |endpoint| protect_labels(endpoint, %w[namespace pod container node job], "#{memory_alert}/#{volume_alert}") }
cadvisor = kubelet_endpoints.select { |endpoint| endpoint["path"] == "/metrics/cadvisor" }
main = kubelet_endpoints.select { |endpoint| endpoint.fetch("path", "/metrics") == "/metrics" }
abort "#{memory_alert}: expected one cAdvisor endpoint with honorLabels true" unless cadvisor.length == 1 && cadvisor.first["honorLabels"] == true
abort "#{volume_alert}: expected one main kubelet endpoint with honorLabels true" unless main.length == 1 && main.first["honorLabels"] == true
cadvisor = cadvisor.first
main = main.first
abort "#{memory_alert}: cAdvisor metrics_path relabeling missing" unless Array(cadvisor["relabelings"]).any? do |entry|
  entry.fetch("action", "replace") == "replace" && entry["sourceLabels"] == ["__metrics_path__"] && entry["targetLabel"] == "metrics_path" && entry.fetch("regex", "(.*)") == "(.*)" && entry.fetch("replacement", "$1") == "$1"
end
container_sample = {"__name__" => "container_memory_working_set_bytes", "namespace" => "witself", "pod" => "witself-postgresql-0", "container" => "postgresql", "id" => "/kubepods/x", "image" => "x"}
abort "#{memory_alert}: PostgreSQL working-set sample dropped" unless survives?(cadvisor, container_sample, memory_alert)
volume_sample = {"__name__" => "kubelet_volume_stats_available_bytes", "namespace" => "witself", "persistentvolumeclaim" => "data-witself-postgresql-0"}
abort "#{volume_alert}: PostgreSQL volume sample dropped" unless survives?(main, volume_sample, volume_alert)
root_sample = {"__name__" => "container_memory_working_set_bytes", "id" => "/", "pod" => ""}
puts "#{node_alert}: cAdvisor root cgroup survives=#{survives?(cadvisor, root_sample, node_alert)}"

node_exporter = exactly_one(docs, "ServiceMonitor", "node-exporter", node_alert)
monitor_scope(node_exporter, "jobLabel", node_alert)
selector = node_exporter.fetch("spec").fetch("selector")
abort "#{node_alert}: unsupported node-exporter Service selector" unless Array(selector["matchExpressions"]).empty?
namespace_selector = node_exporter.dig("spec", "namespaceSelector") || {}
services = docs.select do |doc|
  next false unless doc["kind"] == "Service"
  namespace = doc.dig("metadata", "namespace")
  namespaces = namespace_selector.fetch("matchNames", [node_exporter.dig("metadata", "namespace")])
  (namespace_selector["any"] == true || namespaces.include?(namespace)) && selector.fetch("matchLabels").all? { |key, value| doc.dig("metadata", "labels", key) == value }
end
abort "#{node_alert}: selected Service must carry jobLabel node-exporter" unless services.length == 1 && services.first.dig("metadata", "labels", "jobLabel") == "node-exporter"
node_endpoints = node_exporter.fetch("spec").fetch("endpoints")
abort "#{node_alert}: node-exporter endpoint missing" if node_endpoints.empty?
node_endpoints.each do |endpoint|
  protect_labels(endpoint, %w[namespace pod], node_alert)
  %w[MemTotal MemFree Buffers Cached Shmem SReclaimable].each do |metric|
    sample = {"__name__" => "node_memory_#{metric}_bytes", "namespace" => "monitoring", "pod" => "ne-n1", "instance" => "n1:9100", "job" => "node-exporter"}
    abort "#{node_alert}: #{sample['__name__']} sample dropped" unless survives?(endpoint, sample, node_alert)
  end
end
daemon = exactly_one(docs, "DaemonSet", "node-exporter", node_alert)
args = daemon.fetch("spec").fetch("template").fetch("spec").fetch("containers").flat_map { |container| Array(container["args"]) }
abort "#{node_alert}: meminfo collector disabled" if args.include?("--no-collector.meminfo") || (args.include?("--collector.disable-defaults") && !args.include?("--collector.meminfo"))

ksm = exactly_one(docs, "Deployment", "kube-state-metrics", ksm_alerts)
ksm_args = ksm.fetch("spec").fetch("template").fetch("spec").fetch("containers").flat_map { |container| Array(container["args"]) }
ksm_args.each do |arg|
  flag, values = arg.split("=", 2)
  next unless values
  selected = values.split(",")
  case flag
  when "--resources"
    abort "#{ksm_alerts}: kube-state-metrics resources omit nodes or pods" unless (%w[nodes pods] - selected).empty?
  when "--namespaces"
    abort "#{ksm_alerts}: kube-state-metrics namespaces omit witself or monitoring" unless (%w[witself monitoring] - selected).empty?
  when "--namespaces-denylist"
    abort "#{ksm_alerts}: kube-state-metrics denies witself or monitoring" unless (selected & %w[witself monitoring]).empty?
  end
end
puts "capacity scrape facts passed: PostgreSQL OOM/restart, container memory, node working set, and volume forecast"
