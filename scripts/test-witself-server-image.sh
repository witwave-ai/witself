#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
server_chart="$repo_root/charts/witself-server"
apps_chart="$repo_root/.gitops/charts/apps"
render_dir="$(mktemp -d)"
trap 'rm -rf "$render_dir"' EXIT

# Keep fixture digest values out of diagnostics, including Helm schema errors.
digest="sha256:$(printf 'a%.0s' {1..64})"
common_args=(--namespace witself --set worker.enabled=true
  --set database.existingSecret.name=witself-db)

helm template witself-server "$server_chart" "${common_args[@]}" \
  >"$render_dir/default.yaml"
helm template witself-server "$server_chart" "${common_args[@]}" \
  --set-string image.tag=unit-test-tag >"$render_dir/tag.yaml"
helm template witself-server "$server_chart" "${common_args[@]}" \
  --set-string image.tag=unit-test-tag --set-string "image.digest=$digest" \
  >"$render_dir/digest.yaml"
helm template witself-server "$server_chart" "${common_args[@]}" \
  --set-string "image.digest=$digest" >"$render_dir/digest-default-tag.yaml"
helm template witself-apps "$apps_chart" --set cell.name=image-test \
  --set apps.witselfServer.worker.enabled=true \
  --set-string apps.witselfServer.imageTag=0.0.999 \
  --set-string "apps.witselfServer.imageDigest=$digest" \
  >"$render_dir/apps-digest.yaml"
ruby -ryaml -e '
  app = YAML.load_stream(STDIN.read).compact.find { |doc| doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-server" }
  abort "digest-pinned server Application missing" unless app
  child_values = app.dig("spec", "source", "helm", "values")
  values = YAML.safe_load(child_values, aliases: false)
  abort "apps chart did not forward image digest" unless values.dig("image", "digest") == "sha256:" + "a" * 64
  abort "apps chart did not retain the release image tag" unless values.dig("image", "tag") == "0.0.999"
  print child_values
' <"$render_dir/apps-digest.yaml" >"$render_dir/apps-child-values.yaml"
helm template witself-server "$server_chart" --namespace witself \
  --values "$render_dir/apps-child-values.yaml" >"$render_dir/apps-child.yaml"

ruby -ryaml -e '
  chart = YAML.safe_load(File.read(ARGV[0]))
  values = YAML.safe_load(File.read(ARGV[1]))
  abort "image.digest must be empty by default" unless values.dig("image", "digest") == ""
  repository = values.dig("image", "repository")
  digest = "sha256:" + "a" * 64
  expectations = {
    "default" => "#{repository}:#{chart.fetch("appVersion")}",
    "tag" => "#{repository}:unit-test-tag",
    "digest" => "#{repository}@#{digest}",
    "digest-default-tag" => "#{repository}@#{digest}",
    "apps-child" => "#{repository}@#{digest}",
  }
  expectations.each do |name, expected|
    docs = YAML.load_stream(File.read(File.join(ARGV[2], "#{name}.yaml"))).compact
    deployments = docs.select { |doc| doc["kind"] == "Deployment" }
    abort "#{name}: expected both server and worker deployments" unless deployments.length == 2
    containers = deployments.flat_map { |doc| doc.dig("spec", "template", "spec", "containers") }
    abort "#{name}: server or worker image reference mismatch" unless containers.length == 2 && containers.all? { |container| container["image"] == expected }
  end
' "$server_chart/Chart.yaml" "$server_chart/values.yaml" "$render_dir"

invalid_digests=(not-a-digest sha256:short "sha512:${digest#sha256:}"
  "${digest}a" "sha256:$(printf 'A%.0s' {1..64})")
for invalid_digest in "${invalid_digests[@]}"; do
  if helm template witself-server "$server_chart" \
    --set-string "image.digest=$invalid_digest" \
    >"$render_dir/invalid.yaml" 2>"$render_dir/invalid.err"; then
    echo "invalid image digest passed schema validation" >&2
    exit 1
  fi
  if ! grep -Eq 'image[./]digest' "$render_dir/invalid.err" || \
    ! grep -Fq 'pattern' "$render_dir/invalid.err"; then
    echo "invalid image digest was not rejected by the schema pattern" >&2
    exit 1
  fi
done

if helm template witself-apps "$apps_chart" --set cell.name=image-test \
  --set-string apps.witselfServer.imageDigest=not-a-digest \
  >"$render_dir/apps-invalid.yaml" 2>"$render_dir/apps-invalid.err"; then
  echo "invalid apps image digest passed schema validation" >&2
  exit 1
fi
if ! grep -Fq 'imageDigest' "$render_dir/apps-invalid.err" || \
  ! grep -Fq 'pattern' "$render_dir/apps-invalid.err"; then
  echo "invalid apps image digest was not rejected by the schema pattern" >&2
  exit 1
fi

echo "witself-server image Helm tests: PASS"
