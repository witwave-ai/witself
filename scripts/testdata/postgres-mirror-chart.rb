#!/usr/bin/env ruby
# Offline app-of-apps units; the upstream chart acceptance test is separate.
require 'yaml'
require 'open3'

root = ARGV.fetch(0)
chart = File.join(root, '.gitops/charts/apps')
def check(message, condition)
  abort message unless condition
end
def render(chart, cell, *args)
  output, errors, status = Open3.capture3('helm', 'template', 'mirror-test', chart, '--values', cell, *args)
  [output, errors, status]
end
def postgres_values(output)
  app = YAML.load_stream(output).compact.find { |doc| doc['kind'] == 'Application' && doc.dig('metadata', 'name') == 'witself-postgresql' }
  check('missing PostgreSQL Application', app)
  YAML.safe_load(app.dig('spec', 'source', 'helm', 'values'), aliases: false)
end

%w[civo-sandbox-use1-backup civo-sandbox-use1-serving].each do |name|
  cell = File.join(root, '.gitops/cells', name, 'values.yaml')
  baseline, errors, status = render(chart, cell)
  check('default cell must render', status.success?)
  upstream = postgres_values(baseline)
  overrides = {
    'image.registry' => 'ghcr.io',
    'image.repository' => 'witwave-ai/images/postgresql',
    'image.tag' => "0.0.999-#{name}",
    'image.digest' => upstream.fetch('image').fetch('digest')
  }
  args = overrides.flat_map { |key, value| ['--set-string', "apps.civoPostgres.#{key}=#{value}"] }
  output, errors, status = render(chart, cell, *args, '--set', 'apps.civoPostgres.allowInsecureImages=true')
  check('GHCR mirror must render', status.success?)
  mirrored = postgres_values(output)
  expected = upstream.merge('global' => {'security' => {'allowInsecureImages' => true}}, 'image' => overrides.transform_keys { |key| key.delete_prefix('image.') })
  check('GHCR selection must forward exactly the image and explicit verification opt-in', mirrored == expected)

  output, errors, status = render(chart, cell, *args)
  check('mirror alone must not implicitly enable substitution', status.success? && !postgres_values(output).key?('global'))
  %w[sha256:short SHA256:invalid].each do |digest|
    _, _, status = render(chart, cell, '--set-string', "apps.civoPostgres.image.digest=#{digest}")
    check('malformed PostgreSQL digest must fail the schema', !status.success?)
  end
  _, _, status = render(chart, cell, '--set-string', 'apps.civoPostgres.image.typo=oops')
  check('unknown PostgreSQL image fields must fail the schema', !status.success?)

  # Removing every override must omit the child image mapping entirely, keeping
  # the upstream defaults available to portable installations.
  args = %w[registry repository tag digest].flat_map { |field| ['--set-string', "apps.civoPostgres.image.#{field}="] }
  output, _, status = render(chart, cell, *args)
  check('unset image must preserve upstream default selection', status.success? && !postgres_values(output).key?('image'))
end
puts 'PostgreSQL mirror chart units: PASS'
