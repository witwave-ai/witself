#!/usr/bin/env ruby
# These checks exercise Kubernetes policy semantics without a cluster. Rendered
# manifests stay in memory or temporary files and are never printed: unrelated
# resources contain checksum annotations and Secret references.
require 'yaml'
require 'json'
require 'open3'
require 'tmpdir'

root = File.expand_path(ARGV.fetch(0))
apps_chart = File.join(root, '.gitops/charts/apps')
server_chart = File.join(root, 'charts/witself-server')
cells = %w[civo-sandbox-use1-backup civo-sandbox-use1-serving]
lists = %w[controlPlaneCIDRs r2CIDRs agentEmailCIDRs stripeCIDRs]
destinations = {
  'controlPlaneCIDRs' => ['192.0.2.10/32', '2001:db8::10/128'],
  'r2CIDRs' => ['192.0.2.20/32'],
  'agentEmailCIDRs' => ['192.0.2.30/32'],
  'stripeCIDRs' => ['192.0.2.40/32']
}
enabled = {'enabled' => true, 'postgresNamespace' => 'database'}.merge(destinations)
apps_enabled = enabled.reject { |key, _| key == 'postgresNamespace' }
carrying_version = '0.0.293'

def check(message, condition)
  abort "egress NetworkPolicy: #{message}" unless condition
end

def capture(*args, **options)
  Open3.capture3(*args, **options)
end

def helm(chart, values = nil, overrides = {}, release: 'witself-server', namespace: 'witself')
  args = ['helm', 'template', release, chart, '--namespace', namespace]
  args.concat(['--values', values]) if values
  overrides.each { |key, value| args.concat(['--set-json', "#{key}=#{JSON.generate(value)}"]) }
  capture(*args)
end

def render(chart, values = nil, overrides = {}, **options)
  output, _, status = helm(chart, values, overrides, **options)
  check("Helm render must succeed for #{File.basename(chart)} (overrides: #{overrides.keys.join(', ')})", status.success?)
  output
end

def documents(output)
  YAML.load_stream(output).compact
end

def resource(docs, kind, name)
  matches = docs.select { |doc| doc['kind'] == kind && doc.dig('metadata', 'name') == name }
  check("expected one #{kind}/#{name}", matches.length == 1)
  matches.first
end

def child_values(docs, name)
  YAML.safe_load(resource(docs, 'Application', name).dig('spec', 'source', 'helm', 'values'), aliases: false)
end

def policies(docs)
  docs.select { |doc| doc['kind'] == 'NetworkPolicy' && Array(doc.dig('spec', 'policyTypes')).include?('Egress') }
end

def dns_peer
  {
    'namespaceSelector' => {'matchLabels' => {'kubernetes.io/metadata.name' => 'kube-system'}},
    'podSelector' => {'matchLabels' => {'k8s-app' => 'kube-dns'}}
  }
end

def postgres_peer(namespace)
  {
    'namespaceSelector' => {'matchLabels' => {'kubernetes.io/metadata.name' => namespace}},
    'podSelector' => {'matchLabels' => {
      'app.kubernetes.io/name' => 'postgresql',
      'app.kubernetes.io/instance' => 'witself-postgresql',
      'app.kubernetes.io/component' => 'primary'
    }}
  }
end

def expected_destinations(namespace, cidrs, postgres: true)
  expected = [['dns', 'UDP', 53], ['dns', 'TCP', 53]]
  expected << ["postgres:#{namespace}", 'TCP', 5432] if postgres
  expected.concat(cidrs.uniq.map { |cidr| [cidr, 'TCP', 443] })
end

def assert_allow_list(policy, namespace, cidrs, postgres: true)
  rules = policy.dig('spec', 'egress')
  check('egress must contain explicit rules', rules.is_a?(Array) && !rules.empty?)
  actual = rules.flat_map do |rule|
    check('egress rule must contain only destinations and ports', rule.keys.sort == %w[ports to])
    peers = rule.fetch('to')
    ports = rule.fetch('ports')
    check('egress rule must never allow arbitrary peers or ports',
          peers.is_a?(Array) && !peers.empty? && ports.is_a?(Array) && !ports.empty?)
    destinations = peers.map do |peer|
      if peer == dns_peer
        'dns'
      elsif peer == postgres_peer(namespace)
        "postgres:#{namespace}"
      else
        check('external peer must be exactly one ipBlock CIDR without exceptions',
              peer.keys == ['ipBlock'] && peer.fetch('ipBlock').keys == ['cidr'])
        peer.dig('ipBlock', 'cidr')
      end
    end
    ports.each do |port|
      check('egress port must be explicit with no range', port.keys.sort == %w[port protocol])
    end
    destinations.product(ports).map { |destination, port| [destination, port['protocol'], port['port']] }
  end
  check('egress must grant exactly DNS, selected PostgreSQL, and selected HTTPS destinations',
        actual.sort == expected_destinations(namespace, cidrs, postgres: postgres).sort)
end

def selects?(policy, labels)
  selector = policy.dig('spec', 'podSelector')
  check('workload policy must use nonempty exact label selectors',
        selector.keys == ['matchLabels'] && !selector.fetch('matchLabels').empty?)
  selector.fetch('matchLabels').all? { |key, value| labels[key] == value }
end

def deployment_labels(docs, component)
  matches = docs.select do |doc|
    doc['kind'] == 'Deployment' && doc.dig('spec', 'template', 'metadata', 'labels', 'app.kubernetes.io/component') == component
  end
  check("expected one #{component} Deployment", matches.length == 1)
  matches.first.dig('spec', 'template', 'metadata', 'labels')
end

def assert_server_policies(docs, config, worker: true)
  selected = policies(docs)
  check('expected exactly one egress policy per enabled workload', selected.length == (worker ? 2 : 1))
  names = selected.map { |policy| policy.dig('metadata', 'name') }
  check('policy names must be distinct DNS labels within the Kubernetes limit',
        names.uniq == names && names.all? { |name| name.length <= 63 && name.match?(/\A[a-z0-9](?:[-a-z0-9]*[a-z0-9])?\z/) })
  components = worker ? %w[server worker] : ['server']
  components.each do |component|
    matching = selected.select { |policy| selects?(policy, deployment_labels(docs, component)) }
    check("#{component} must be selected by exactly one egress policy", matching.length == 1)
    policy = matching.first
    check("#{component} egress must include its role in the selector", policy.dig('spec', 'podSelector', 'matchLabels', 'app.kubernetes.io/component') == component)
    check('egress policy must leave ingress to existing policies',
          policy.dig('spec', 'policyTypes') == ['Egress'] && !policy.fetch('spec').key?('ingress'))
    other_components = components - [component]
    check('server and worker policy selectors must not overlap',
          other_components.none? { |other| selects?(policy, deployment_labels(docs, other)) })
    # All groups are populated in the main fixtures: any reintroduced union
    # gives either role extra permissions and fails this exact comparison.
    cidrs = component == 'worker' ? config.fetch('agentEmailCIDRs', []) : []
    assert_allow_list(policy, config.fetch('postgresNamespace'), cidrs)
  end
end

def assert_postgres_policy(values, namespace)
  check('upstream PostgreSQL policy must be disabled to avoid additive allow-all egress',
        values.dig('primary', 'networkPolicy', 'enabled') == false)
  matches = policies(values.fetch('extraDeploy', []))
  check('PostgreSQL must have exactly one restrictive replacement policy', matches.length == 1)
  policy = matches.first
  check('PostgreSQL policy must target only primary pods in the configured namespace',
        policy.dig('metadata', 'namespace') == namespace &&
        policy.dig('spec', 'podSelector') == postgres_peer(namespace).fetch('podSelector'))
  check('PostgreSQL must retain ingress isolation', policy.dig('spec', 'policyTypes').sort == %w[Egress Ingress])
  assert_allow_list(policy, namespace, [], postgres: false)
end

[apps_chart, server_chart].each do |chart|
  defaults = YAML.safe_load(File.read(File.join(chart, 'values.yaml')), aliases: false)
  check('egress switch must default off in both charts', defaults.dig('egressPolicy', 'enabled') == false)
  lists.each { |key| check("#{key} must default to no external hosts", defaults.dig('egressPolicy', key) == []) }
end

Dir.mktmpdir('witself-egress-policy-') do |temporary|
  # These persistent checks prove default/off equivalence and dark structure,
  # not byte identity to a historical revision. A git archive HEAD comparison
  # becomes tautological after merge; the review separately compares saved
  # pre-change renders for both live cells with their final off renders.
  cells.each do |cell|
    relative = ".gitops/cells/#{cell}/values.yaml"
    values = File.join(root, relative)
    off_output = render(apps_chart, values, {}, release: 'witself-apps')
    explicit_off = render(apps_chart, values, {'egressPolicy.enabled' => false}, release: 'witself-apps')
    check("#{cell} explicit off must be byte-identical to default", explicit_off == off_output)
    off_docs = documents(off_output)
    check('dark apps render must not select any pod for egress isolation', policies(off_docs).empty?)
    off_child = child_values(off_docs, 'witself-server')
    check('dark apps render must not propagate new child values', !off_child.key?('egressPolicy'))
    child_path = File.join(temporary, 'child.yaml')
    File.write(child_path, off_child.to_yaml)
    server_off = render(server_chart, child_path)
    check("#{cell} child explicit off must be byte-identical to default", server_off == render(server_chart, child_path, {'egressPolicy.enabled' => false}))
    check('dark server render must not select any pod for egress isolation', policies(documents(server_off)).empty?)

    # Select an older child explicitly so a normal server chart roll cannot
    # turn this negative case into a supported activation. Disable the
    # orthogonal backup job; its independent policy is tested below.
    _, errors, status = helm(apps_chart, values, {
      'egressPolicy' => apps_enabled,
      'apps.witselfServer.chartVersion' => '0.0.291',
      'apps.civoPostgres.backup.enabled' => false
    }, release: 'witself-apps')
    check("#{cell} pinned child chart must reject partial egress activation",
          !status.success? && errors.include?('egressPolicy.enabled') && errors.include?(carrying_version))
    on_docs = documents(render(apps_chart, values, {
      'egressPolicy' => apps_enabled,
      'apps.witselfServer.chartVersion' => carrying_version,
      'apps.civoPostgres.namespace' => 'fixture-database',
      'apps.civoPostgres.backup.enabled' => false
    }, release: 'witself-apps'))
    config = enabled.merge('postgresNamespace' => 'fixture-database')
    on_child = child_values(on_docs, 'witself-server')
    check('apps must propagate the single egress configuration and actual PostgreSQL namespace', on_child.fetch('egressPolicy') == config)
    File.write(child_path, on_child.to_yaml)
    child_docs = documents(render(server_chart, child_path))
    assert_server_policies(child_docs, config)
    ingress_before = documents(server_off).select { |doc| doc['kind'] == 'NetworkPolicy' }
    ingress_after = child_docs.select { |doc| doc['kind'] == 'NetworkPolicy' && doc.dig('spec', 'policyTypes') == ['Ingress'] }
    check('enabling egress must preserve server and worker ingress including metrics exactly', ingress_before == ingress_after)
    assert_postgres_policy(child_values(on_docs, 'witself-postgresql'), 'fixture-database')
    check('disabled backup job must not gain an unrelated policy', policies(on_docs).empty?)
  end

  worker_settings = {'worker.enabled' => true, 'database.existingSecret.name' => 'fixture-db', 'egressPolicy' => enabled}
  assert_server_policies(documents(render(server_chart, nil, worker_settings)), enabled)
  no_ingress = worker_settings.merge('networkPolicy.enabled' => false, 'worker.networkPolicy.enabled' => false)
  no_ingress_docs = documents(render(server_chart, nil, no_ingress))
  assert_server_policies(no_ingress_docs, enabled)
  check('egress must stay enabled when both ingress switches are off',
        no_ingress_docs.select { |doc| doc['kind'] == 'NetworkPolicy' }.length == 2)
  assert_server_policies(documents(render(server_chart, nil, {'egressPolicy' => enabled})), enabled, worker: false)
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('fullnameOverride' => 'a' * 63))), enabled)
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('fullnameOverride' => ('a' * 49) + '-worker'))), enabled)
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('nameOverride' => 'witself-worker'))), enabled)
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('podLabels' => {'app.kubernetes.io/component' => 'server'}))), enabled)
  _, errors, status = helm(server_chart, nil, worker_settings.merge('podLabels' => {'app.kubernetes.io/component' => 'custom'}))
  check('enabled egress must reject a pod label override that bypasses the server policy',
        !status.success? && errors.include?('app.kubernetes.io/component'))
  empty_config = {'enabled' => true, 'postgresNamespace' => 'witself'}
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('egressPolicy' => empty_config))), empty_config)
  shared_config = enabled.merge('agentEmailCIDRs' => destinations.fetch('controlPlaneCIDRs'))
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('egressPolicy' => shared_config))), shared_config)
  unused_config = enabled.merge('agentEmailCIDRs' => [])
  assert_server_policies(documents(render(server_chart, nil, worker_settings.merge('egressPolicy' => unused_config))), unused_config)

  values = File.join(root, '.gitops/cells/civo-sandbox-use1-serving/values.yaml')
  guarded_settings = {'egressPolicy' => apps_enabled, 'apps.civoPostgres.backup.enabled' => false}
  %w[0.0.289 0.0.291 0.0.293-rc.1].each do |version|
    _, errors, status = helm(apps_chart, values,
      guarded_settings.merge('apps.witselfServer.chartVersion' => version), release: 'witself-apps')
    check("child chart #{version} must reject partial egress activation with the carrying release",
          !status.success? && errors.include?('egressPolicy.enabled') && errors.include?(carrying_version))
    off_docs = documents(render(apps_chart, values, {
      'egressPolicy.enabled' => false, 'apps.witselfServer.chartVersion' => version
    }, release: 'witself-apps'))
    check("old child chart #{version} must remain usable while egress is off",
          !child_values(off_docs, 'witself-server').key?('egressPolicy'))
  end
  [carrying_version, '0.0.293', '0.1.0'].each do |version|
    docs = documents(render(apps_chart, values,
      guarded_settings.merge('apps.witselfServer.chartVersion' => version), release: 'witself-apps'))
    check("child chart #{version} must accept egress activation",
          child_values(docs, 'witself-server').dig('egressPolicy', 'enabled') == true)
    assert_postgres_policy(child_values(docs, 'witself-postgresql'), 'witself')
  end
  no_server_docs = documents(render(apps_chart, values, guarded_settings.merge(
    'apps.witselfServer.enabled' => false, 'apps.witselfServer.chartVersion' => '0.0.289'
  ), release: 'witself-apps'))
  check('disabled server application must not require an egress-capable child release',
        no_server_docs.none? { |doc| doc['kind'] == 'Application' && doc.dig('metadata', 'name') == 'witself-server' })
  assert_postgres_policy(child_values(no_server_docs, 'witself-postgresql'), 'witself')

  [true, false].product([true, false]).each do |network_enabled, allow_external|
    docs = documents(render(apps_chart, values, {
      'egressPolicy' => apps_enabled,
      'apps.witselfServer.chartVersion' => carrying_version,
      'apps.civoPostgres.backup.enabled' => false,
      'apps.civoPostgres.networkPolicy.enabled' => network_enabled,
      'apps.civoPostgres.networkPolicy.allowExternal' => allow_external
    }, release: 'witself-apps'))
    assert_postgres_policy(child_values(docs, 'witself-postgresql'), 'witself')
  end
  [{'apps.civoPostgres.enabled' => false}, {'cell.cloud' => 'gcp'}].each do |missing_postgres|
    _, errors, status = helm(apps_chart, values, {
      'egressPolicy' => apps_enabled, 'apps.civoPostgres.backup.enabled' => false,
      'apps.witselfServer.chartVersion' => carrying_version
    }.merge(missing_postgres), release: 'witself-apps')
    check('apps must reject egress activation without the supported in-cell PostgreSQL topology',
          !status.success? && errors.include?('egressPolicy.enabled requires in-cell Civo PostgreSQL'))
  end

  backup_image = {'repository' => 'ghcr.io/witwave-ai/images/witself-postgres-backup', 'tag' => 'fixture'}
  backup_overrides = {'egressPolicy' => apps_enabled, 'apps.civoPostgres.backup.enabled' => true,
                      'apps.witselfServer.chartVersion' => carrying_version,
                      'apps.civoPostgres.backup.image' => backup_image, 'apps.civoPostgres.namespace' => 'fixture-database'}
  backup_docs = documents(render(apps_chart, values, backup_overrides, release: 'witself-apps'))
  job = resource(backup_docs, 'CronJob', 'witself-postgresql-backup')
  backup_policies = policies(backup_docs)
  check('backup job must have its own egress policy', backup_policies.length == 1)
  backup_policy = backup_policies.first
  check('backup policy must select only the exact job name and cell in its namespace',
        backup_policy.dig('metadata', 'namespace') == job.dig('metadata', 'namespace') &&
        backup_policy.dig('spec', 'podSelector', 'matchLabels') == job.dig('spec', 'jobTemplate', 'spec', 'template', 'metadata', 'labels') &&
        backup_policy.dig('spec', 'policyTypes') == ['Egress'])
  assert_allow_list(backup_policy, 'fixture-database', destinations.fetch('r2CIDRs'))
  backup_without_r2 = documents(render(apps_chart, values,
    backup_overrides.merge('egressPolicy' => apps_enabled.merge('r2CIDRs' => [])), release: 'witself-apps'))
  assert_allow_list(policies(backup_without_r2).fetch(0), 'fixture-database', [])
  container = job.dig('spec', 'jobTemplate', 'spec', 'template', 'spec', 'containers').first
  check('enabled egress backup must use preinstalled tools', container.fetch('env').any? do |entry|
    entry == {'name' => 'WITSELF_POSTGRES_BACKUP_IMAGE_MODE', 'value' => 'preinstalled'}
  end)
  ['', 'postgres:18-alpine', {}, {'repository' => '', 'tag' => '', 'digest' => ''}].each do |legacy|
    _, errors, status = helm(apps_chart, values, backup_overrides.merge('apps.civoPostgres.backup.image' => legacy), release: 'witself-apps')
    check('enabled egress must reject legacy package-download backup images', !status.success?)
    unless legacy == ''
      check('backup refusal must explain the egress/preinstalled image prerequisite', errors.include?('egressPolicy') && errors.match?(/preinstalled|repository|structured/))
    end
  end
end

invalid_cidrs = ['0.0.0.0/0', '::/0', '192.0.0.0/15', '2001:db8::/31', '999.1.2.3/32',
                 '192.0.2.10', '2001:db8::10', '192.0.2.10/128', '192.0.2.10/33', '2001:db8::10/129',
                 '2001:::1/128', '1:2:3/128', '::::/128', 'self.witwave.ai/32', "192.0.2.10/32\n", '']
[apps_chart, server_chart].each do |chart|
  values = chart == apps_chart ? File.join(root, '.gitops/cells/civo-sandbox-use1-serving/values.yaml') : nil
  valid_destinations = ['192.0.0.0/16', '192.0.0.0/17', '192.0.2.0/24', '192.0.2.10/31', '192.0.2.10/32',
                        '2001:db8::/32', '2001:db8::/33', '2001:db8::/64', '2001:db8::10/127',
                        '2001:db8::10/128', '2001:db8:0:1:2:3:4:5/128', '2001:db8::/128', '::1/128']
  lists.each do |key|
    _, _, status = helm(chart, values, {"egressPolicy.#{key}" => valid_destinations})
    check("#{File.basename(chart)} schema must accept bounded provider prefixes and exact hosts in #{key}", status.success?)
  end
  invalid_cidrs.each do |cidr|
    lists.each do |key|
      _, _, status = helm(chart, values, {"egressPolicy.#{key}" => [cidr]})
      check("#{File.basename(chart)} schema must reject invalid or broad #{key}: #{cidr.inspect}", !status.success?)
    end
  end
  invalid_settings = [{'allowAll' => true}, {'enabled' => 'true'}, {'controlPlaneCIDRs' => '192.0.2.10/32'}, {'r2CIDRs' => [42]}]
  invalid_settings.concat(if chart == apps_chart
    [{'postgresNamespace' => 'database'}]
  else
    [{'postgresNamespace' => ''}, {'postgresNamespace' => 'bad_namespace'}, {'postgresNamespace' => 'a' * 64}]
  end)
  invalid_settings.each do |invalid|
    _, _, status = helm(chart, values, {'egressPolicy' => invalid})
    check("#{File.basename(chart)} schema must reject malformed or unknown egress settings", !status.success?)
  end
end

puts 'Egress NetworkPolicy Helm unit checks passed'
