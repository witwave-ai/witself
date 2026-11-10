#!/usr/bin/env ruby
# Offline app-of-apps units; the upstream chart acceptance test is separate.
require 'yaml'
require 'open3'
require 'tmpdir'
require 'fileutils'
require 'shellwords'

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

  output, errors, status = render(chart, cell, *args, '--set', 'apps.civoPostgres.allowInsecureImages=false')
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

# These are wrapper units: inspect the values actually passed to the child, then
# execute the exact rendered hook with a stub pg_ctl, never a real database.
backup_cell = File.join(root, '.gitops/cells/civo-sandbox-use1-backup/values.yaml')
backup_render, _, status = render(chart, backup_cell)
check('backup shutdown cell must render', status.success?)
backup_primary = postgres_values(backup_render).fetch('primary')
check('backup shutdown must allow 240 seconds', backup_primary['terminationGracePeriodSeconds'] == 240)
hook = backup_primary.dig('lifecycleHooks', 'preStop', 'exec', 'command')
check('backup shutdown must have exactly one wrapper-owned preStop exec hook',
      backup_primary['lifecycleHooks']&.keys == ['preStop'] &&
      backup_primary.dig('lifecycleHooks', 'preStop')&.keys == ['exec'] &&
      hook.is_a?(Array) && hook.length == 3 && hook.first(2) == ['/bin/sh', '-c'])
hook_script = hook.fetch(2)

Dir.mktmpdir('postgres-shutdown-units') do |scratch|
  # Remove only the optional feature from a scratch chart. A branch that leaks
  # output when unset must fail the complete, byte-for-byte apps comparison.
  # Normalize source first so this source mutation also works on Windows CRLF.
  legacy_chart = File.join(scratch, 'apps-without-shutdown')
  Dir.glob(File.join(chart, '**/*'), File::FNM_DOTMATCH).each do |path|
    relative = path.delete_prefix("#{chart}/")
    next if relative.split('/').any? { |part| %w[. .. secrets .git].include?(part) }
    next if path.end_with?('.sops', '.token') || !File.file?(path)

    target = File.join(legacy_chart, relative)
    FileUtils.mkdir_p(File.dirname(target))
    FileUtils.cp(path, target)
  end
  template = File.join(legacy_chart, 'templates/civo-postgres.yaml')
  original_source = File.read(template)
  line_ending = original_source.include?("\r\n") ? "\r\n" : "\n"
  depth = 0
  blocks = 0
  legacy_source = original_source.gsub("\r\n", "\n").lines.reject do |line|
    if depth.zero? && line.include?('if hasKey $pg "shutdown"')
      blocks += 1
      depth = 1
      true
    elsif depth.positive?
      depth += 1 if line.match?(/\{\{-?\s*(if|range|with)\s/)
      depth -= 1 if line.match?(/\{\{-?\s*end\b/)
      true
    else
      false
    end
  end.join
  check('shutdown baseline must remove exactly its validation and emission branches', blocks == 2 && depth.zero?)
  # Retain checkout line endings for the byte-level render comparison.
  File.write(template, legacy_source.gsub("\n", line_ending))
  Dir.glob(File.join(root, '.gitops/cells/*/values.yaml')).sort.each do |cell|
    next if cell == backup_cell

    baseline, _, status = render(chart, cell)
    check("cell must render: #{File.basename(File.dirname(cell))}", status.success?)
    app = YAML.load_stream(baseline).compact.find do |doc|
      doc['kind'] == 'Application' && doc.dig('metadata', 'name') == 'witself-postgresql'
    end
    if app
      primary = postgres_values(baseline).fetch('primary')
      check('shutdown must remain absent outside the backup cell',
            !primary.key?('terminationGracePeriodSeconds') && !primary.key?('lifecycleHooks'))
    end
    without, _, status = render(legacy_chart, cell)
    check('absent shutdown must be byte-identical to the chart without shutdown support',
          status.success? && without == baseline)
  end

  # Use a cell without shutdown so missing fields cannot inherit backup values.
  serving_cell = File.join(root, '.gitops/cells/civo-sandbox-use1-serving/values.yaml')
  override = File.join(scratch, 'shutdown.yaml')
  valid = {'mode' => 'fast', 'timeoutSeconds' => 180, 'gracePeriodSeconds' => 240}
  render_shutdown = lambda do |shutdown|
    File.write(override, {'apps' => {'civoPostgres' => {'shutdown' => shutdown}}}.to_yaml)
    render(chart, serving_cell, '--values', override)
  end

  invalid = {
    'smart mode' => valid.merge('mode' => 'smart'),
    'immediate mode' => valid.merge('mode' => 'immediate'),
    'empty mode' => valid.merge('mode' => ''),
    'non-string mode' => valid.merge('mode' => 1),
    'timeout below minimum' => valid.merge('timeoutSeconds' => 29),
    'timeout above maximum' => valid.merge('timeoutSeconds' => 601, 'gracePeriodSeconds' => 900),
    'fractional timeout' => valid.merge('timeoutSeconds' => 180.5),
    'string timeout' => valid.merge('timeoutSeconds' => '180'),
    'boolean timeout' => valid.merge('timeoutSeconds' => true),
    'grace below minimum' => valid.merge('timeoutSeconds' => 30, 'gracePeriodSeconds' => 59),
    'grace above maximum' => valid.merge('gracePeriodSeconds' => 901),
    'fractional grace' => valid.merge('gracePeriodSeconds' => 240.5),
    'string grace' => valid.merge('gracePeriodSeconds' => '240'),
    'boolean grace' => valid.merge('gracePeriodSeconds' => true),
    'unknown key' => valid.merge('typo' => true),
    'free-form command' => valid.merge('command' => ['true']),
    'free-form script' => valid.merge('script' => 'true'),
    'free-form lifecycle map' => valid.merge('lifecycleHooks' => {'preStop' => {}}),
    'empty object' => {},
    'non-object' => 'fast',
    'null' => nil
  }
  valid.each_key { |key| invalid["missing #{key}"] = valid.reject { |name, _| name == key } }
  invalid.each do |name, shutdown|
    _, errors, status = render_shutdown.call(shutdown)
    check("shutdown schema must reject #{name}", !status.success? && errors.include?('schema'))
  end
  [180, 179, 209].each do |grace|
    _, errors, status = render_shutdown.call(valid.merge('gracePeriodSeconds' => grace))
    check("shutdown template must reject grace #{grace} for timeout 180",
          !status.success? && errors.include?('gracePeriodSeconds') && errors.include?('timeoutSeconds'))
  end
  [[30, 60], [180, 210], [600, 900]].each do |timeout, grace|
    output, _, status = render_shutdown.call(valid.merge('timeoutSeconds' => timeout, 'gracePeriodSeconds' => grace))
    check('valid shutdown boundaries must render', status.success?)
    primary = postgres_values(output).fetch('primary')
    check('shutdown grace must be forwarded without changing it', primary['terminationGracePeriodSeconds'] == grace)
    script = primary.dig('lifecycleHooks', 'preStop', 'exec', 'command', 2)
    check('shutdown timeout must be built from the typed value', script.include?("-t #{timeout} stop"))
  end

  # Enabling shutdown may add only its two primary keys; every other byte in the
  # child values remains stable after removing that exact generated block.
  absent, _, status = render(chart, serving_cell)
  check('serving baseline must render', status.success?)
  enabled, _, status = render_shutdown.call(valid)
  check('valid shutdown must render', status.success?)
  enabled_values = postgres_values(enabled)
  enabled_values.fetch('primary').delete('terminationGracePeriodSeconds')
  enabled_values.fetch('primary').delete('lifecycleHooks')
  check('shutdown must not change any other child value', enabled_values == postgres_values(absent))

  script_path = File.join(scratch, 'pre-stop.sh')
  File.write(script_path, hook_script)
  _, _, status = Open3.capture3('/bin/sh', '-n', script_path)
  check('rendered shutdown hook must pass sh -n', status.success?)
  if system('sh', '-c', 'command -v shellcheck >/dev/null 2>&1')
    _, _, status = Open3.capture3('shellcheck', '--shell=sh', script_path)
    check('rendered shutdown hook must pass ShellCheck', status.success?)
  else
    puts 'NOT RUN: rendered shutdown hook ShellCheck (shellcheck unavailable)'
  end

  bin = File.join(scratch, 'bin')
  FileUtils.mkdir_p(bin)
  stub = File.join(bin, 'pg_ctl')
  File.write(stub, <<~'SH')
    #!/bin/sh
    printf 'call\n' >> "$STUB_LOG"
    for argument do
      printf '%s\n' "$argument" >> "$STUB_LOG"
    done
    case "$*" in
      *' stop') exit "$STOP_STATUS" ;;
      *' status') exit "$STATUS_STATUS" ;;
      *) exit 99 ;;
    esac
  SH
  File.chmod(0o755, stub)
  pgdata = File.join(scratch, 'data with spaces')
  log = File.join(scratch, 'calls')
  fallback = '/opt/bitnami/postgresql/bin/pg_ctl'
  check('shutdown must keep the pinned image pg_ctl fallback',
        hook_script.include?(fallback))
  # Substitute only the absolute executable path in a local script copy. This
  # proves fallback control flow without touching /opt or claiming image proof.
  empty_bin = File.join(scratch, 'empty-bin')
  FileUtils.mkdir_p(empty_bin)
  fallback_script = File.join(scratch, 'pre-stop-fallback.sh')
  File.write(fallback_script, hook_script.sub(fallback, Shellwords.escape(stub)))
  {'PATH' => [bin, script_path], 'fallback' => [empty_bin, fallback_script]}.each do |resolver, (path, executable)|
    [[0, 1, 0], [7, 3, 0], [7, 0, 7], [7, 1, 7], [9, 4, 9]].each do |stop_status, status_status, expected|
      File.write(log, '')
      _, _, status = Open3.capture3(
        {'PATH' => path, 'PGDATA' => pgdata, 'STUB_LOG' => log,
         'STOP_STATUS' => stop_status.to_s, 'STATUS_STATUS' => status_status.to_s},
        '/bin/sh', executable
      )
      check("shutdown #{resolver} stop=#{stop_status}, status=#{status_status} must exit #{expected}",
            status.exitstatus == expected)
      expected_calls = ['call', '-D', pgdata, '-m', 'fast', '-w', '-t', '180', 'stop']
      expected_calls += ['call', '-D', pgdata, 'status'] unless stop_status.zero?
      check("shutdown #{resolver} must use exact quoted arguments, checking status only after failure",
            File.readlines(log, chomp: true) == expected_calls)
    end
  end
  File.write(fallback_script, hook_script.sub(fallback, Shellwords.escape(File.join(scratch, 'missing-pg-ctl'))))
  _, _, status = Open3.capture3({'PATH' => empty_bin, 'PGDATA' => pgdata}, '/bin/sh', fallback_script)
  check('missing pg_ctl must preserve the original command-not-found status', status.exitstatus == 127)
end
puts 'PostgreSQL shutdown chart units: PASS (24 schema rejections, 3 grace rejections, 3 boundaries, 11 hook outcomes)'
