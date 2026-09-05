#!/usr/bin/env bash
# Exercise the apps wrapper against the real, versioned PostgreSQL child chart.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
chart="${WITSELF_TEST_POSTGRESQL_CHART:-}"
if [[ -z "$chart" ]]; then
  HELM_CACHE_HOME="$tmp/helm-cache" HELM_CONFIG_HOME="$tmp/helm-config" \
    helm pull oci://registry-1.docker.io/bitnamicharts/postgresql \
      --version 18.8.0 --untar --untardir "$tmp"
  chart="$tmp/postgresql"
fi

ruby -ryaml -ropen3 - "$repo_root" "$chart" "$tmp" <<'RUBY'
root, chart, tmp = ARGV
def check(message, condition)
  abort message unless condition
end
def command(*args)
  out, err, status = Open3.capture3(*args)
  abort err unless status.success?
  out
end
metadata = YAML.safe_load(command("helm", "show", "chart", chart), aliases: false)
check("expected the PostgreSQL 18.8.0 chart", metadata["name"] == "postgresql" && metadata["version"] == "18.8.0")
apps_chart = File.join(root, ".gitops/charts/apps")
cell = File.join(root, ".gitops/cells/civo-sandbox-use1-backup/values.yaml")
def child_values(chart, cell, overrides)
  args = ["helm", "template", "witself-apps", chart, "--values", cell]
  overrides.each { |key, value| args.concat(["--set", "apps.civoPostgres.#{key}=#{value}"]) }
  docs = YAML.load_stream(command(*args)).compact
  app = docs.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-postgresql" }
  check("missing PostgreSQL Application", app)
  YAML.safe_load(app.dig("spec", "source", "helm", "values"), aliases: false)
end
def render_child(chart, values, tmp)
  file = File.join(tmp, "child-values.yaml")
  File.write(file, YAML.dump(values))
  Open3.capture3("helm", "template", "witself-postgresql", chart, "--namespace", "witself", "--values", file)
end

defaults = child_values(apps_chart, cell, {})
check("image verification opt-in must be omitted by default", !defaults.key?("global"))
_, errors, status = render_child(chart, defaults, tmp)
check("default PostgreSQL child must render: #{errors}", status.success?)

# Regression: forwarding a mirror reference alone used to look valid in the
# wrapper test while Bitnami's child chart rejected Argo manifest generation.
[
  {"image.registry" => "mirror.example.com"},
  {"image.repository" => "reviewed/postgresql"},
  {"image.registry" => "mirror.example.com", "image.repository" => "reviewed/postgresql"}
].each do |overrides|
  rejected = child_values(apps_chart, cell, overrides)
  check("mirror must not implicitly bypass image verification", !rejected.key?("global"))
  _, errors, status = render_child(chart, rejected, tmp)
  check("mirror without opt-in must retain Bitnami rejection", !status.success? && errors.include?("Original containers have been substituted for unrecognized ones"))

  reviewed = child_values(apps_chart, cell, overrides.merge("allowInsecureImages" => true))
  check("reviewed mirror must explicitly opt into child image substitution", reviewed.dig("global", "security", "allowInsecureImages") == true)
  output, errors, status = render_child(chart, reviewed, tmp)
  check("reviewed mirror must render PostgreSQL 18.8.0: #{errors}", status.success?)
  statefulset = YAML.load_stream(output).compact.find { |doc| doc["kind"] == "StatefulSet" }
  container = statefulset.fetch("spec").fetch("template").fetch("spec").fetch("containers").find { |item| item["name"] == "postgresql" }
  registry = overrides.fetch("image.registry", defaults.fetch("image").fetch("registry"))
  repository = overrides.fetch("image.repository", defaults.fetch("image").fetch("repository"))
  check("mirror render must preserve the reviewed digest", container["image"] == "#{registry}/#{repository}@#{defaults.fetch('image').fetch('digest')}")
end
explicit_false = child_values(apps_chart, cell, {"allowInsecureImages" => false})
check("explicit false must preserve the default child values", explicit_false == defaults)
puts "PostgreSQL 18.8.0 mirror regression checks passed"
RUBY
