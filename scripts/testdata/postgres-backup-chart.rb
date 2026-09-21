#!/usr/bin/env ruby
require 'yaml'
require 'open3'
require 'json'

root = ARGV.fetch(0)
chart = File.join(root, '.gitops/charts/apps')
cell = File.join(root, '.gitops/cells/civo-sandbox-use1-serving/values.yaml')
backup_cell = File.join(root, '.gitops/cells/civo-sandbox-use1-backup/values.yaml')

def check(message, condition)
  abort message unless condition
end

def render(chart, cell, *overrides)
  # Mode is part of this fixture, not a property of the last committed roll.
  # Select legacy mode explicitly unless the case supplies its own image. Keep
  # --set separate from --set-json: Helm processes those flag groups in a fixed
  # order, so an unconditional reset would overwrite a case's structured image.
  args = ['helm', 'template', 'witself-apps', chart, '--values', cell]
  selected_image = overrides.any? do |value|
    value.is_a?(Hash) ? value.key?('apps.civoPostgres.backup.image') : value.start_with?('apps.civoPostgres.backup.image=')
  end
  unless selected_image
    %w[repository tag digest].each { |field| args.concat(['--set', "apps.civoPostgres.backup.image.#{field}="]) }
  end
  overrides.each do |value|
    if value.is_a?(Hash)
      value.each { |key, entry| args.concat(['--set-json', "#{key}=#{JSON.generate(entry)}"]) }
      next
    end
    # Boolean/integer overrides must retain their actual types.
    flag = value.match?(/=(true|false|[0-9]+)$/) ? '--set' : '--set-string'
    args.concat([flag, value])
  end
  Open3.capture3(*args)
end

def documents(chart, cell, *overrides)
  output, _, status = render(chart, cell, *overrides)
  check('PostgreSQL backup Helm rendering failed', status.success?)
  YAML.load_stream(output).compact
end

def named(docs, kind, name)
  docs.find { |doc| doc['kind'] == kind && doc.dig('metadata', 'name') == name }
end

enabled = ['apps.civoPostgres.backup.enabled=true']
defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')), aliases: false)
check('backup must default off', defaults.dig('apps', 'civoPostgres', 'backup', 'enabled') == false)

[cell, backup_cell].each do |values|
  # Render the dark case explicitly: a cell may have activated backups in its
  # committed values, and this unit proves the off state regardless.
  disabled = documents(chart, values, 'apps.civoPostgres.backup.enabled=false')
  check('dark backup must not render a CronJob or ConfigMap on either cell', !disabled.any? do |doc|
    %w[CronJob ConfigMap].include?(doc['kind']) && doc.dig('metadata', 'name') == 'witself-postgresql-backup'
  end)
  disabled_pg = YAML.safe_load(named(disabled, 'Application', 'witself-postgresql').dig('spec', 'source', 'helm', 'values'), aliases: false)
  check('dark backup must not add exporter queries on either cell', !disabled_pg.fetch('metrics').key?('customMetrics'))
end

docs = documents(chart, cell, *enabled)
job = named(docs, 'CronJob', 'witself-postgresql-backup')
check('enabled backup must render exactly one CronJob', job && docs.count { |doc| doc['kind'] == 'CronJob' } == 1)
check('daily backup schedule must use UTC', job.dig('spec', 'schedule') == '0 3 * * *' && job.dig('spec', 'timeZone') == 'Etc/UTC')
check('backup must forbid concurrent jobs and bound history', job.dig('spec', 'concurrencyPolicy') == 'Forbid' && job.dig('spec', 'successfulJobsHistoryLimit') == 1 && job.dig('spec', 'failedJobsHistoryLimit') == 3)
check('backup must bound job and scheduler deadlines without retries', job.dig('spec', 'startingDeadlineSeconds') == 3600 && job.dig('spec', 'jobTemplate', 'spec', 'activeDeadlineSeconds') == 3600 && job.dig('spec', 'jobTemplate', 'spec', 'backoffLimit') == 0)
pod = job.dig('spec', 'jobTemplate', 'spec', 'template')
check('backup must disable service-account tokens and restart', pod.dig('spec', 'automountServiceAccountToken') == false && pod.dig('spec', 'restartPolicy') == 'Never')
container = pod.dig('spec', 'containers').fetch(0)
check('backup must not gain Linux capabilities or privilege escalation', container.dig('securityContext', 'allowPrivilegeEscalation') == false && container.dig('securityContext', 'capabilities', 'drop') == ['ALL'])
check('backup must retain the default seccomp profile', pod.dig('spec', 'securityContext', 'seccompProfile', 'type') == 'RuntimeDefault')
check('dump container must not receive the PostgreSQL administrator credential', container.fetch('env').none? { |entry| entry.dig('valueFrom', 'secretKeyRef', 'key') == 'postgres-password' || (entry['name'] == 'PGUSER' && entry['value'] == 'postgres') })
check('backup must run the mounted Bash runner with its default PostgreSQL image', container['image'] == defaults.dig('apps', 'civoPostgres', 'backup', 'legacyImage') && container['command'] == ['/bin/bash', '/scripts/backup.sh'])
config = named(docs, 'ConfigMap', 'witself-postgresql-backup')
check('mounted runner must match the tested source exactly', config && config.dig('data', 'backup.sh') == File.read(File.join(chart, 'files/postgres-backup.sh')))
check('backup script must be mounted read-only from the matching ConfigMap', container.fetch('volumeMounts').any? { |mount| mount['mountPath'] == '/scripts' && mount['readOnly'] == true && pod.dig('spec', 'volumes').any? { |volume| volume['name'] == mount['name'] && volume.dig('configMap', 'name') == 'witself-postgresql-backup' } })
check('backup chart must never emit a Secret', docs.none? { |doc| doc['kind'] == 'Secret' })
check('legacy image must not enable preinstalled mode', container.fetch('env').none? { |entry| entry['name'] == 'WITSELF_POSTGRES_BACKUP_IMAGE_MODE' })

# Keep existing scalar overrides compatible. Empty structured pins select the
# same image, environment, permissions, and runner as the legacy chart defaults.
[{}, {'repository' => '', 'tag' => '', 'digest' => ''}].each do |image|
  fallback = documents(chart, cell, *enabled, {'apps.civoPostgres.backup.image' => image})
  check('empty image pins must preserve every rendered resource', fallback == docs)
end
legacy_image = 'postgres:18-alpine'
legacy = documents(chart, cell, *enabled, "apps.civoPostgres.backup.image=#{legacy_image}")
legacy_job = named(legacy, 'CronJob', 'witself-postgresql-backup')
expected_legacy_job = Marshal.load(Marshal.dump(job))
expected_legacy_job.dig('spec', 'jobTemplate', 'spec', 'template', 'spec', 'containers').fetch(0)['image'] = legacy_image
check('custom scalar image must retain all legacy CronJob behavior', legacy_job == expected_legacy_job)

# Changing the chart's legacy image default must also change every empty-pin
# fallback, without opting into preinstalled tools or changing other resources.
[{}, {'repository' => '', 'tag' => '', 'digest' => ''}].each do |image|
  fallback = documents(chart, cell, *enabled,
    "apps.civoPostgres.backup.legacyImage=#{legacy_image}",
    {'apps.civoPostgres.backup.image' => image})
  check('empty image pins must follow the configured legacy image default', fallback == legacy)
end

repository = 'ghcr.io/witwave-ai/images/witself-postgres-backup'
digest = 'sha256:' + ('a' * 64)
# Exercise the operator-facing --set form as well as the typed object cases.
# Together with the explicit empty-field --set baseline above, both runner
# modes are independent of the committed cell's current backup image.
set_args = ['helm', 'template', 'witself-apps', chart, '--values', cell,
            '--set', 'apps.civoPostgres.backup.enabled=true']
{'repository' => repository, 'tag' => '0.0.999', 'digest' => digest}.each do |field, value|
  set_args.concat(['--set', "apps.civoPostgres.backup.image.#{field}=#{value}"])
end
set_output, _, set_status = Open3.capture3(*set_args)
check('explicit preinstalled --set mode must render', set_status.success?)
set_job = named(YAML.load_stream(set_output).compact, 'CronJob', 'witself-postgresql-backup')
set_container = set_job.dig('spec', 'jobTemplate', 'spec', 'template', 'spec', 'containers').fetch(0)
check('explicit --set pins must select the preinstalled runner', set_container['image'] == "#{repository}@#{digest}" &&
  set_container['command'] == ['/bin/bash', '/scripts/backup.sh'] &&
  set_container.fetch('env').include?({'name' => 'WITSELF_POSTGRES_BACKUP_IMAGE_MODE', 'value' => 'preinstalled'}))
[
  [{'repository' => repository, 'tag' => '0.0.999'}, "#{repository}:0.0.999"],
  [{'repository' => repository, 'tag' => '0.0.999', 'digest' => ''}, "#{repository}:0.0.999"],
  [{'repository' => repository, 'tag' => '0.0.999', 'digest' => digest}, "#{repository}@#{digest}"],
  [{'repository' => 'postgres', 'tag' => '18'}, 'postgres:18'],
  [{'repository' => 'localhost:5000/team/postgres', 'tag' => '18'}, 'localhost:5000/team/postgres:18'],
  [{'repository' => 'ghcr.io/team_name/postgres--backup', 'tag' => '18'}, 'ghcr.io/team_name/postgres--backup:18'],
].each do |image, expected_image|
  pinned = documents(chart, cell, *enabled, {'apps.civoPostgres.backup.image' => image})
  pinned_job = named(pinned, 'CronJob', 'witself-postgresql-backup')
  expected_job = Marshal.load(Marshal.dump(job))
  expected_container = expected_job.dig('spec', 'jobTemplate', 'spec', 'template', 'spec', 'containers').fetch(0)
  expected_container['image'] = expected_image
  expected_container['env'].unshift({'name' => 'WITSELF_POSTGRES_BACKUP_IMAGE_MODE', 'value' => 'preinstalled'})
  check('structured image must select preinstalled mode and preserve all other CronJob behavior', pinned_job == expected_job)
  check('structured image must mount the same tested runner', named(pinned, 'ConfigMap', 'witself-postgresql-backup') == config)
end

[
  {'repository' => repository},
  {'repository' => repository, 'tag' => ''},
  {'tag' => '0.0.999'},
  {'digest' => digest},
  {'repository' => '', 'tag' => '0.0.999', 'digest' => digest},
  {'repository' => repository, 'tag' => '0.0.999', 'digest' => 'sha256:invalid'},
  {'repository' => repository, 'tag' => '0.0.999', 'digest' => 'sha256:' + ('A' * 64)},
  {'repository' => repository + ':mutable', 'tag' => '0.0.999'},
  {'repository' => 'ghcr.io/team:5000/backup', 'tag' => '0.0.999'},
  {'repository' => repository + '@' + digest, 'tag' => '0.0.999'},
  {'repository' => repository, 'tag' => 'has whitespace'},
  {'repository' => repository, 'tag' => '0.0.999', 'installPackages' => true},
  [], true, 42, '',
].each do |image|
  _, _, status = render(chart, cell, *enabled, {'apps.civoPostgres.backup.image' => image})
  check('backup schema must reject malformed or partial image selection', !status.success?)
end

expected_refs = {
  'PGPASSWORD' => ['witself-postgresql-backup-r2', 'dump-password'],
  'METRICS_PASSWORD' => ['witself-postgresql-backup-r2', 'metrics-password'],
  'AGE_RECIPIENT' => ['witself-postgresql-backup-age', 'recipient'],
  'AWS_ACCESS_KEY_ID' => ['witself-postgresql-backup-r2', 'access-key-id'],
  'AWS_SECRET_ACCESS_KEY' => ['witself-postgresql-backup-r2', 'secret-access-key'],
  'R2_ENDPOINT' => ['witself-postgresql-backup-r2', 'endpoint'],
  'R2_BUCKET' => ['witself-postgresql-backup-r2', 'bucket'],
  'R2_PREFIX' => ['witself-postgresql-backup-r2', 'prefix'],
}
def assert_refs(container, refs)
  env = container.fetch('env').to_h { |entry| [entry.fetch('name'), entry] }
  refs.each do |name, (secret, key)|
    entry = env.fetch(name)
    check("#{name} must only reference the named Secret key", entry == {'name' => name, 'valueFrom' => {'secretKeyRef' => {'name' => secret, 'key' => key}}})
  end
end
assert_refs(container, expected_refs)
env = container.fetch('env').to_h { |entry| [entry.fetch('name'), entry] }
check('dump and telemetry must use distinct dedicated roles', env.dig('PGUSER', 'value') == 'witself_backup_dump' && env.dig('METRICS_USER', 'value') == 'witself_backup_metrics')
check('dump and exporter telemetry must target their separate databases', env.dig('PGDATABASE', 'value') == 'witself' && env.dig('METRICS_DATABASE', 'value') == 'postgres')
check('only the two named backup Secrets may be exposed to the job', container.fetch('env').map { |entry| entry.dig('valueFrom', 'secretKeyRef', 'name') }.compact.uniq.sort == %w[witself-postgresql-backup-age witself-postgresql-backup-r2])
custom = documents(chart, cell, *enabled,
  'apps.civoPostgres.backup.ageRecipientSecretName=fixture-age',
  'apps.civoPostgres.backup.r2SecretName=fixture-r2',
  'apps.civoPostgres.backup.activeDeadlineSeconds=900')
custom_job = named(custom, 'CronJob', 'witself-postgresql-backup')
custom_refs = expected_refs.transform_values do |secret, key|
  [{'witself-postgresql-backup-age' => 'fixture-age', 'witself-postgresql-backup-r2' => 'fixture-r2'}.fetch(secret), key]
end
assert_refs(custom_job.dig('spec', 'jobTemplate', 'spec', 'template', 'spec', 'containers').fetch(0), custom_refs)
check('custom deadline must render', custom_job.dig('spec', 'jobTemplate', 'spec', 'activeDeadlineSeconds') == 900)

pg = YAML.safe_load(named(docs, 'Application', 'witself-postgresql').dig('spec', 'source', 'helm', 'values'), aliases: false)
metrics = pg.dig('metrics', 'customMetrics', 'witself_postgres_backup')
check('backup must reuse the PostgreSQL SQL exporter with run telemetry', metrics && metrics.fetch('query').include?('witself_ops.backup_runs') && metrics.fetch('metrics').map(&:keys).flatten.sort == %w[failures_total last_attempt_succeeded last_attempt_timestamp_seconds last_success_timestamp_seconds])
%w[last_success_timestamp_seconds last_attempt_timestamp_seconds last_attempt_succeeded].each do |gauge|
  check("#{gauge} must be a gauge", metrics.fetch('metrics').any? { |metric| metric.dig(gauge, 'usage') == 'GAUGE' })
end
check('failures must be a counter', metrics.fetch('metrics').any? { |metric| metric.dig('failures_total', 'usage') == 'COUNTER' })
backup_docs = documents(chart, backup_cell, *enabled)
backup_pg = YAML.safe_load(named(backup_docs, 'Application', 'witself-postgresql').dig('spec', 'source', 'helm', 'values'), aliases: false)
check('backup switch alone must render a CronJob on a cell without monitoring', named(backup_docs, 'CronJob', 'witself-postgresql-backup'))
check('backup switch alone must enable SQL exporter and the same run telemetry on a cell without monitoring', backup_pg.dig('metrics', 'enabled') == true && backup_pg.dig('metrics', 'customMetrics', 'witself_postgres_backup') == metrics)
check('backup switch must preserve disabled ServiceMonitor on a cell without monitoring', backup_pg.dig('metrics', 'serviceMonitor', 'enabled') == false)
check('backup switch must not create a second metric mechanism', backup_docs.none? { |doc| %w[Service Deployment DaemonSet].include?(doc['kind']) && doc.to_s.match?(/pushgateway|textfile/i) })
forced_exporter_docs = documents(chart, cell, *enabled, 'apps.civoPostgres.metrics.enabled=false', 'apps.civoPostgres.metrics.serviceMonitor.enabled=false')
forced_exporter = YAML.safe_load(named(forced_exporter_docs, 'Application', 'witself-postgresql').dig('spec', 'source', 'helm', 'values'), aliases: false)
check('backup alone must provide SQL exporter even when ordinary metrics are off', forced_exporter.dig('metrics', 'enabled') == true && forced_exporter.dig('metrics', 'customMetrics', 'witself_postgres_backup') == metrics)
peers = pg.fetch('extraDeploy').find { |doc| doc['kind'] == 'NetworkPolicy' }.dig('spec', 'ingress').find { |rule| rule['ports'].any? { |port| port['port'] == 5432 } }.fetch('from')
check('strict PostgreSQL policy must admit the exact backup pod labels and namespace', peers.any? do |peer|
  peer.dig('namespaceSelector', 'matchLabels') == {'kubernetes.io/metadata.name' => job.dig('metadata', 'namespace')} && peer.dig('podSelector', 'matchLabels') == pod.dig('metadata', 'labels')
end)

{
  'apps.civoPostgres.enabled=false' => 'requires Civo PostgreSQL',
  'cell.cloud=gcp' => 'requires Civo PostgreSQL',
}.each do |override, diagnostic|
  _, errors, status = render(chart, cell, *enabled, override)
  check("backup must reject invalid prerequisite: #{override}", !status.success? && Array(diagnostic).any? { |message| errors.include?(message) })
end
[
  'apps.civoPostgres.backup.legacyImage=',
  'apps.civoPostgres.backup.ageRecipientSecretName=',
  'apps.civoPostgres.backup.r2SecretName=',
  'apps.civoPostgres.backup.ageRecipientSecretName=a..b',
  'apps.civoPostgres.backup.ageRecipientSecretName=-foo',
  'apps.civoPostgres.backup.r2SecretName=foo.-bar',
  'apps.civoPostgres.backup.ageRecipient=fixture-inline-recipient',
  'apps.civoPostgres.backup.r2AccessKey=fixture-inline-key',
  'apps.civoPostgres.backup.activeDeadlineSeconds=0',
  'apps.civoPostgres.backup.activeDeadlineSeconds=86401',
].each do |override|
  _, _, status = render(chart, cell, *enabled, override)
  check("backup schema must reject #{override.split('=').first}", !status.success?)
end
puts 'PostgreSQL backup Helm unit checks passed'
