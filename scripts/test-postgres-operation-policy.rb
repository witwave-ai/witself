#!/usr/bin/env ruby
require "json"
require "open3"
require "yaml"

# Use the Job actually emitted by the operation script's fake-kubectl fixture,
# so a producer label change cannot silently strand an authorized operation.
job_path, cell, namespace = ARGV
abort "usage: #{$PROGRAM_NAME} JOB_JSON CELL NAMESPACE" unless namespace
repo_root = File.expand_path("..", __dir__)
output, errors, status = Open3.capture3("helm", "template", "witself-apps",
  File.join(repo_root, ".gitops/charts/apps"), "--values",
  File.join(repo_root, ".gitops/cells", cell, "values.yaml"),
  "--set", "apps.civoPostgres.networkPolicy.enabled=true",
  "--set", "apps.civoPostgres.networkPolicy.allowExternal=false",
  "--set", "apps.civoPostgres.metrics.enabled=true")
abort errors unless status.success?
application = YAML.load_stream(output).compact.find do |doc|
  doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-postgresql"
end
abort "missing PostgreSQL Application" unless application
values = YAML.safe_load(application.dig("spec", "source", "helm", "values"), aliases: false)
abort "strict ingress must replace the upstream policy" unless values.dig("primary", "networkPolicy", "enabled") == false
policies = values.fetch("extraDeploy").select { |doc| doc["kind"] == "NetworkPolicy" }
abort "expected exactly one strict PostgreSQL policy" unless policies.length == 1
policy = policies[0]
job = JSON.parse(File.read(job_path))
labels = job.fetch("spec").fetch("template").fetch("metadata").fetch("labels")
name = job.fetch("metadata").fetch("name")

def selector_matches?(selector, labels)
  selector.fetch("matchLabels", {}).all? { |key, value| labels[key] == value } &&
    selector.fetch("matchExpressions", []).all? do |expression|
      key = expression.fetch("key")
      case expression.fetch("operator")
      when "In" then labels.key?(key) && expression.fetch("values").include?(labels[key])
      when "NotIn" then !expression.fetch("values").include?(labels[key])
      when "Exists" then labels.key?(key)
      when "DoesNotExist" then !labels.key?(key)
      else abort "unsupported NetworkPolicy selector operator"
      end
    end
end

def admits?(policy, namespace, labels, port)
  policy.fetch("spec").fetch("ingress", []).any? do |rule|
    ports_match = rule.fetch("ports", []).empty? || rule["ports"].any? do |entry|
      entry.fetch("protocol", "TCP") == "TCP" && (!entry.key?("port") || entry["port"] == port)
    end
    ports_match && (rule.fetch("from", []).empty? || rule["from"].any? do |peer|
      abort "unexpected IP-based ingress peer" if peer.key?("ipBlock")
      namespace_matches = if peer.key?("namespaceSelector")
        selector_matches?(peer["namespaceSelector"], {"kubernetes.io/metadata.name" => namespace})
      elsif peer.key?("podSelector")
        namespace == policy.dig("metadata", "namespace")
      else
        true
      end
      namespace_matches && selector_matches?(peer.fetch("podSelector", {}), labels)
    end)
  end
end

abort "#{name}: generated Job pod cannot reach PostgreSQL" unless admits?(policy, namespace, labels, 5432)
abort "#{name}: Job from another namespace can reach PostgreSQL" if admits?(policy, "unrelated", labels, 5432)
abort "#{name}: Job can reach the exporter port" if admits?(policy, namespace, labels, 9187)
abort "unrelated pod can reach PostgreSQL" if admits?(policy, namespace, {"app.kubernetes.io/name" => "unrelated"}, 5432)
abort "unlabeled pod can reach PostgreSQL" if admits?(policy, namespace, {}, 5432)
labels.each_key do |key|
  missing = labels.reject { |candidate, _| candidate == key }
  abort "#{name}: Job missing #{key} can reach PostgreSQL" if admits?(policy, namespace, missing, 5432)
  wrong = labels.merge(key => "unrelated")
  abort "#{name}: Job with wrong #{key} can reach PostgreSQL" if admits?(policy, namespace, wrong, 5432)
end
puts "#{name}: generated Job PostgreSQL ingress checks passed"
