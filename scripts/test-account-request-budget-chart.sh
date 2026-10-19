#!/usr/bin/env bash
# Offline Helm render assertions; no cluster, provider, database, or credentials.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
chart="$repo_root/charts/witself-server"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-account-request-budget-chart.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT

render() {
  helm template witself-server "$chart" --namespace witself "$@"
}

enabled_values='requestBudget.enabled=true,requestBudget.readPerMinute=600,requestBudget.readBurst=60,requestBudget.writePerMinute=120,requestBudget.writeBurst=20,requestBudget.maxTrackedAccounts=1000000'

# C1: disabled values must not add keys or change either server checksum.
render >"$work_dir/default.yaml"
if grep -Eq 'WITSELF_ACCOUNT_REQUEST_BUDGET_|requestBudget' "$work_dir/default.yaml"; then
  echo 'C1: default render contains request budget configuration' >&2
  exit 1
fi
render --set 'requestBudget.enabled=false,requestBudget.readPerMinute=5,requestBudget.readBurst=5,requestBudget.writePerMinute=5,requestBudget.writeBurst=5,requestBudget.maxTrackedAccounts=500' >"$work_dir/disabled.yaml"
cmp "$work_dir/default.yaml" "$work_dir/disabled.yaml"

cat >"$work_dir/plans.yaml" <<'YAML'
requestBudget:
  plans:
    free:
      readPerMinute: 60
      readBurst: 1
      writePerMinute: 30
      writeBurst: 2
    team:
      readPerMinute: 600
      readBurst: 5
      writePerMinute: 120
      writeBurst: 10
YAML

# C7: a non-empty plans map remains completely dark while disabled.
render --values "$work_dir/plans.yaml" >"$work_dir/disabled-plans.yaml"
if ! cmp -s "$work_dir/default.yaml" "$work_dir/disabled-plans.yaml"; then
  echo 'C7: disabled plans changed the default render' >&2
  exit 1
fi

# C6: two plans render one canonical JSON string, exclusively on the server.
render --values "$chart/ci/gcp-rollout-values.yaml" --set "$enabled_values" --values "$work_dir/plans.yaml" >"$work_dir/plans-worker.yaml"
# C8: an enabled budget with no plans keeps exactly the original six keys.
render --set "$enabled_values" >"$work_dir/no-plans.yaml"
# C10: changing only one tier value changes the server config checksum.
render --values "$chart/ci/gcp-rollout-values.yaml" --set "$enabled_values" --values "$work_dir/plans.yaml" --set requestBudget.plans.team.readBurst=6 >"$work_dir/changed-plan.yaml"
ruby - "$work_dir" <<'RUBY'
require 'yaml'

root = ARGV.fetch(0)
documents = ->(name) { YAML.load_stream(File.read(File.join(root, name))).compact }
config_map = lambda do |name, component|
  documents.call(name).find { |doc| doc['kind'] == 'ConfigMap' && doc.dig('metadata', 'name') == component } ||
    abort("missing #{component} ConfigMap")
end
prefix = 'WITSELF_ACCOUNT_REQUEST_BUDGET_'
budget = ->(name) { config_map.call(name, 'witself-server').fetch('data').select { |key, _value| key.start_with?(prefix) } }
expected = {
  'ENABLED' => 'true', 'READ_PER_MINUTE' => '600', 'READ_BURST' => '60',
  'WRITE_PER_MINUTE' => '120', 'WRITE_BURST' => '20', 'MAX_ACCOUNTS' => '1000000'
}.to_h { |key, value| [prefix + key, value] }
abort('C8: enabled budget without plans must have exactly the original six keys') unless budget.call('no-plans.yaml') == expected
fixture = '{"free":{"read_burst":1,"read_per_minute":60,"write_burst":2,"write_per_minute":30},"team":{"read_burst":5,"read_per_minute":600,"write_burst":10,"write_per_minute":120}}'
abort('C6: server must have exactly seven budget keys with byte-identical plan JSON') unless budget.call('plans-worker.yaml') == expected.merge(prefix + 'PLANS' => fixture)
worker_data = config_map.call('plans-worker.yaml', 'witself-worker').fetch('data')
abort('C6: budget keys leaked into worker ConfigMap') if worker_data.keys.any? { |key| key.start_with?(prefix) }
checksum = lambda do |name|
  config_map.call(name, 'witself-server').fetch('metadata').fetch('annotations').fetch('witself.io/server-config-checksum')
end
abort('C10: changing a plan value did not change the server config checksum') if checksum.call('plans-worker.yaml') == checksum.call('changed-plan.yaml')
RUBY

# C2: use the worker-enabled profile to prove only the server receives the env.
render --values "$chart/ci/gcp-rollout-values.yaml" --set "$enabled_values" >"$work_dir/enabled-worker.yaml"
# C3 compares the same default profile with only the budget enabled.
render --set "$enabled_values" >"$work_dir/enabled.yaml"
ruby - "$work_dir" <<'RUBY'
require 'yaml'

root = ARGV.fetch(0)
documents = ->(name) { YAML.load_stream(File.read(File.join(root, name))).compact }
config_map = lambda do |docs, name|
  docs.find { |doc| doc['kind'] == 'ConfigMap' && doc.dig('metadata', 'name') == name } ||
    abort("missing #{name} ConfigMap")
end
worker_docs = documents.call('enabled-worker.yaml')
worker = config_map.call(worker_docs, 'witself-worker')
abort('C2: budget keys leaked into worker ConfigMap') if worker.fetch('data').keys.any? { |key| key.start_with?('WITSELF_ACCOUNT_REQUEST_BUDGET_') }
server = config_map.call(worker_docs, 'witself-server')
prefix = 'WITSELF_ACCOUNT_REQUEST_BUDGET_'
expected = {
  'ENABLED' => 'true', 'READ_PER_MINUTE' => '600', 'READ_BURST' => '60',
  'WRITE_PER_MINUTE' => '120', 'WRITE_BURST' => '20', 'MAX_ACCOUNTS' => '1000000'
}.to_h { |key, value| [prefix + key, value] }
actual = server.fetch('data').select { |key, _value| key.start_with?(prefix) }
abort('C2: server budget keys or values differ') unless actual == expected
lines = File.readlines(File.join(root, 'enabled-worker.yaml')).select { |line| line.include?(prefix) }
abort('C2: expected exactly six budget env lines') unless lines.length == 6
expected.each do |key, value|
  abort("C2: #{key} must render as a quoted decimal or boolean string") unless lines.include?("  #{key}: \"#{value}\"\n")
end
checksum = lambda do |name|
  config_map.call(documents.call(name), 'witself-server').fetch('metadata').fetch('annotations').fetch('witself.io/server-config-checksum')
end
abort('C3: enabling the budget did not change the server config checksum') if checksum.call('default.yaml') == checksum.call('enabled.yaml')
RUBY

expect_invalid() {
  local case_name="$1"
  local overrides="$2"
  if render --set "$enabled_values" --set "$overrides" >"$work_dir/invalid.yaml" 2>"$work_dir/invalid.stderr"; then
    echo "$case_name: invalid request budget rendered successfully" >&2
    exit 1
  fi
}

# C4: validate each rate independently at both rejected bounds.
for rate in readPerMinute readBurst writePerMinute writeBurst; do
  expect_invalid "C4 $rate=0" "requestBudget.$rate=0"
  expect_invalid "C4 $rate=1000001" "requestBudget.$rate=1000001"
done
# C5: the capacity floor and closed set of keys are schema-enforced.
expect_invalid 'C5 maxTrackedAccounts=99' 'requestBudget.maxTrackedAccounts=99'
expect_invalid 'C5 unknown key' 'requestBudget.foo=1'

# C9: every tier is a closed object with bounded integer values and a valid id.
ruby - "$work_dir" <<'RUBY'
require 'json'

root = ARGV.fetch(0)
tier = { 'readPerMinute' => 60, 'readBurst' => 1, 'writePerMinute' => 30, 'writeBurst' => 2 }
cases = {
  'invalid-id' => { 'Free' => tier },
  'seventeen-plans' => (1..17).to_h { |i| ["plan_#{i}", tier] },
  'missing-writeBurst' => { 'free' => tier.reject { |key, _value| key == 'writeBurst' } },
  'unknown-key' => { 'free' => tier.merge('unknown' => 1) }
}
tier.each_key do |key|
  [0, 1_000_001, 1.5].each do |value|
    cases["#{key}-#{value}"] = { 'free' => tier.merge(key => value) }
  end
end
cases.each do |name, plans|
  File.write(File.join(root, "invalid-plans-#{name}.json"), JSON.generate('requestBudget' => { 'plans' => plans }))
end
RUBY
shopt -s nullglob
invalid_plan_fixtures=("$work_dir"/invalid-plans-*.json)
shopt -u nullglob
# Four structural cases and three rejected values for each of four fields.
if [[ ${#invalid_plan_fixtures[@]} -ne 16 ]]; then
  echo "C9: expected 16 invalid plans fixtures, found ${#invalid_plan_fixtures[@]}" >&2
  exit 1
fi
for fixture in "${invalid_plan_fixtures[@]}"; do
  if render --set "$enabled_values" --values "$fixture" >"$work_dir/invalid.yaml" 2>"$work_dir/invalid.stderr"; then
    echo "C9: invalid plans fixture $(basename "$fixture") rendered successfully" >&2
    exit 1
  fi
  if ! grep -Fq "don't meet the specifications" "$work_dir/invalid.stderr"; then
    echo "C9: invalid plans fixture $(basename "$fixture") failed without a schema-refusal diagnostic" >&2
    exit 1
  fi
done

echo 'account request budget chart: C1-C10 passed'
