#!/usr/bin/env ruby
require 'yaml'
require 'open3'
require 'json'

root = ARGV.fetch(0)
def check(message, condition)
  abort message unless condition
end

config = YAML.safe_load(File.read(File.join(root, '.goreleaser.yaml')), aliases: false)
images = config.fetch('dockers_v2')
backup = images.find { |image| image['id'] == 'witself-postgres-backup' }
server = images.find { |image| image['id'] == 'witself-server' }
check('backup release image is missing or duplicated', backup && images.count { |image| image['id'] == backup['id'] } == 1)
check('backup image must use its purpose-built Dockerfile', backup['dockerfile'] == 'images/witself-postgres-backup/Dockerfile')
check('backup release must publish the documented repository', backup['images'] == ['ghcr.io/witwave-ai/images/witself-postgres-backup'])
check('backup release must publish immutable version tags only', backup['tags'] == ['{{ .Version }}'])
check('backup release must include both supported architectures', backup.fetch('platforms').sort == %w[linux/amd64 linux/arm64])
check('backup release image must not be disabled', !backup.key?('disable'))
check('backup must use release SBOM attestation like the server', backup['sbom'] == false && server['sbom'] == false)
%w[source version revision licenses].each do |label|
  key = "org.opencontainers.image.#{label}"
  check("backup must preserve release #{label} metadata", backup.fetch('labels').fetch(key) == server.fetch('labels').fetch(key))
end
signer = config.fetch('docker_signs').find { |entry| entry['id'] == 'container-manifests' }
check('backup multiarch manifest must be included in keyless signing',
      signer && signer['cmd'] == 'cosign' && signer['args'] == ['sign', '${artifact}@${digest}', '--yes'] &&
      (!signer.key?('ids') || signer.fetch('ids').include?(backup['id'])))

dockerfile = File.read(File.join(root, backup.fetch('dockerfile')))
check('backup base must pin an official PostgreSQL 18 Alpine digest',
      dockerfile.match?(/^FROM postgres:18-alpine@sha256:[a-f0-9]{64}$/))
check('backup must pin both tools during the build',
      dockerfile.match?(/apk add --no-cache age=\d+\.\d+\.\d+-r\d+ aws-cli=\d+\.\d+\.\d+-r\d+;/))
check('backup must verify required tools while building', dockerfile.include?('for tool in bash psql pg_dump gzip date age aws; do command -v "$tool"'))
check('backup must disable the inherited database server entrypoint', dockerfile.match?(/^ENTRYPOINT \[\]$/))

ci = YAML.safe_load(File.read(File.join(root, '.github/workflows/ci.yml')), aliases: false).fetch('jobs')
tripwire = ci.fetch('postgres-backup-image')
build_step = tripwire.fetch('steps').find { |step| step['run'] == 'make check-postgres-backup-image' }
check('pre-tag CI must require a Docker build and smoke test',
      build_step && build_step.dig('env', 'CI') == 'true' &&
      tripwire['needs'] == 'changes' && tripwire['if'] == "needs.changes.outputs.application == 'true'" &&
      !build_step.key?('if') &&
      !tripwire.fetch('continue-on-error', false) && !build_step.fetch('continue-on-error', false))
go_gate = ci.fetch('go')
check('required go gate must depend on the backup image tripwire',
      go_gate.fetch('needs').include?('postgres-backup-image') && go_gate.fetch('if').include?('always()'))
gate_step = go_gate.fetch('steps').find { |step| step['name'] == 'Require scoped CI gates' }
check('required go gate must inspect all dependency results',
      gate_step && gate_step['run'] == 'node scripts/ci-product-gate.mjs' &&
      gate_step.dig('env', 'NEEDS_JSON') == '${{ toJSON(needs) }}')
gate_needs = go_gate.fetch('needs').to_h { |name| [name, {'result' => 'success'}] }
gate_needs['changes']['outputs'] = {'application' => 'true', 'website' => 'true', 'website_only' => 'false'}
%w[success failure skipped cancelled].each do |result|
  gate_needs['postgres-backup-image']['result'] = result
  _, _, status = Open3.capture3({'NEEDS_JSON' => JSON.generate(gate_needs)}, 'bash', '-c', gate_step.fetch('run'), chdir: root)
  check("required go gate must accept only a successful backup build: #{result}", status.success? == (result == 'success'))
end
gate_needs.each { |name, value| value['result'] = 'skipped' unless %w[changes website].include?(name) }
gate_needs['changes']['outputs'] = {'application' => 'false', 'website' => 'true', 'website_only' => 'true'}
_, _, status = Open3.capture3({'NEEDS_JSON' => JSON.generate(gate_needs)}, 'bash', '-c', gate_step.fetch('run'), chdir: root)
check('website-only gate must accept an explicitly scoped backup skip', status.success?)
gate_needs['postgres-backup-image']['result'] = 'failure'
_, _, status = Open3.capture3({'NEEDS_JSON' => JSON.generate(gate_needs)}, 'bash', '-c', gate_step.fetch('run'), chdir: root)
check('website-only gate must not hide a failed backup build', !status.success?)

workflow = YAML.safe_load(File.read(File.join(root, '.github/workflows/release.yml')), aliases: false)
release = workflow.fetch('jobs').fetch('goreleaser')
required_gates = %w[go verify provider-integration-verify provider-contract-evidence dashboard-acceptance homebrew-verify]
check('backup publication must retain every existing release prerequisite', (required_gates - release.fetch('needs')).empty?)
steps = release.fetch('steps')
attest = steps.find { |step| step['name'] == 'Attest container SBOMs' }
check('backup must receive the same signed SBOM as the other release images',
      attest && attest['if'] == "github.event_name == 'push'" &&
      attest.fetch('run').match?(/for image_name in witself witself-server witself-postgres-backup; do/) &&
      attest.fetch('run').include?('syft "${image}:${version}"') &&
      attest.fetch('run').include?('cosign attest --yes --type https://spdx.dev/Document'))
check('multiarch package installation must retain QEMU and Buildx setup',
      %w[docker/setup-qemu-action@ docker/setup-buildx-action@].all? do |action|
        steps.any? { |step| step.fetch('uses', '').start_with?(action) }
      end)
%w[Verify\ signed\ release\ artifact\ contract Verify\ published\ assets\ and\ archive\ provenance Attest\ build\ provenance].each do |name|
  check("backup publication must retain #{name}", steps.any? { |step| step['name'] == name })
end
puts 'PostgreSQL backup image release contract: PASS'
