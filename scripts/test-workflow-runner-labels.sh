#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
ruby - "$source_root" <<'RUBY'
require 'json'
require 'yaml'

ROOT = ARGV.fetch(0)
# The only hosted Linux images a workflow may request, spelled exactly like
# this. Moving to a newer image is a reviewed change to this list, never a side
# effect of a floating label.
PINNED_LINUX = %w[ubuntu-24.04 ubuntu-24.04-arm].freeze
# Frozen contract strings: the repository ruleset and the release train both
# match these names byte for byte. They are not runner labels.
REQUIRED_CHECKS = [
  'go', 'release-config', 'homebrew-formula', 'helm',
  'avatar-renderer-portability (ubuntu-latest)',
  'avatar-renderer-portability (ubuntu-24.04-arm)'
].freeze
MATRIX_REFERENCE = /\$\{\{\s*matrix\.([A-Za-z_][\w-]*)\s*\}\}/.freeze
WHOLE_MATRIX_REFERENCE = /\A\s*\$\{\{\s*matrix\.([A-Za-z_][\w-]*)\s*\}\}\s*\z/.freeze

def fail_check(message)
  raise ArgumentError, message
end

def load_jobs(file, text)
  workflow = YAML.safe_load(text, aliases: false, filename: file)
  jobs = workflow.is_a?(Hash) ? workflow['jobs'] : nil
  fail_check("#{file}: jobs must be a mapping of job mappings") unless jobs.is_a?(Hash) && jobs.values.all? { |job| job.is_a?(Hash) }
  jobs
rescue Psych::Exception => error
  fail_check("#{file}: #{error.message}")
end

def matrix_cells(id, job)
  matrix = job.dig('strategy', 'matrix')
  fail_check("#{id}: matrix is not a static mapping") unless matrix.is_a?(Hash)
  fail_check("#{id}: matrix exclude is not supported by this contract") if matrix.key?('exclude')
  lists = matrix.reject { |key, _| key == 'include' }
  if matrix.key?('include')
    fail_check("#{id}: mixed list and include matrix is not supported by this contract") unless lists.empty?
    include = matrix.fetch('include')
    fail_check("#{id}: matrix include must be a list of mappings") unless include.is_a?(Array) && include.all? { |cell| cell.is_a?(Hash) }
    return include
  end
  lists.reduce([{}]) do |cells, (key, values)|
    # A dynamic list (fromJSON) has no static cells. That is an error only when
    # a runner label or a check name depends on it.
    values = [:dynamic] unless values.is_a?(Array)
    cells.flat_map { |cell| values.map { |value| cell.merge(key => value) } }
  end
end

def matrix_value(id, key, cell)
  value = cell[key]
  fail_check("#{id}: matrix.#{key} has no static value") if value.nil? || value == :dynamic
  value
end

def resolve(id, text, cell)
  text.gsub(MATRIX_REFERENCE) do
    key = Regexp.last_match(1)
    value = matrix_value(id, key, cell)
    fail_check("#{id}: matrix.#{key} is not a scalar") if value.is_a?(Array) || value.is_a?(Hash)
    value.to_s
  end
end

def references_matrix?(value)
  case value
  when String then value.match?(MATRIX_REFERENCE)
  when Array then value.any? { |entry| references_matrix?(entry) }
  when Hash then value.values.any? { |entry| references_matrix?(entry) }
  else false
  end
end

# Every label one matrix cell requests, with matrix references substituted in
# each element. A matrix value may itself be a label list or a runner mapping.
def cell_labels(id, runs_on, cell)
  case runs_on
  when String
    whole = runs_on.match(WHOLE_MATRIX_REFERENCE)
    if whole
      value = matrix_value(id, whole[1], cell)
      return cell_labels(id, value, {}) if value.is_a?(Array) || value.is_a?(Hash)
    end
    [resolve(id, runs_on, cell)]
  when Array
    runs_on.flat_map do |entry|
      fail_check("#{id}: unsupported runs-on label") unless entry.is_a?(String)
      cell_labels(id, entry, cell)
    end
  when Hash
    labels = cell_labels(id, runs_on.fetch('labels') { [] }, cell)
    fail_check("#{id}: a runner group must be self-hosted") unless labels.any? { |label| label.casecmp('self-hosted').zero? }
    labels
  else
    fail_check("#{id}: unsupported runs-on shape")
  end
end

def runner_label_sets(id, job)
  # A reusable workflow call runs on the runners of the called workflow.
  return [] if job.key?('uses') && !job.key?('runs-on')
  runs_on = job.fetch('runs-on') { fail_check("#{id}: runs-on is missing") }
  # The matrix matters only when the runner refers to it: a literal label is
  # static whatever the matrix looks like.
  cells = references_matrix?(runs_on) ? matrix_cells(id, job) : [{}]
  cells.flat_map do |cell|
    labels = cell_labels(id, runs_on, cell)
    labels.each do |label|
      fail_check("#{id}: unresolved runner expression") if label.include?('${{')
    end
    labels.any? { |label| label.casecmp('self-hosted').zero? } ? [] : [labels]
  end
end

def check_names(id, job)
  # Checks of a called workflow are named "<caller> / <job>", never a bare name.
  return [] if job.key?('uses') && !job.key?('runs-on')
  where = "ci.yml:#{id}"
  name = job['name']
  names =
    if job.dig('strategy', 'matrix').nil?
      [name.nil? ? id : name.to_s]
    else
      # Both the number and the names of a matrix job's checks depend on its
      # cells, so a job in ci.yml needs a matrix this contract can expand.
      matrix_cells(where, job).map do |cell|
        next resolve(where, name.to_s, cell) unless name.nil?
        fail_check("#{where}: matrix job with a dynamic cell needs an explicit name") if cell.value?(:dynamic)
        "#{id} (#{cell.values.join(', ')})"
      end
    end
  names.each do |produced|
    fail_check("#{where}: check name is not static") if produced.include?('${{')
  end
end

def check(sources)
  linux = 0
  sources.fetch(:workflows).each do |file, text|
    load_jobs(file, text).each do |id, job|
      runner_label_sets("#{file}:#{id}", job).each do |labels|
        linux_labels = labels.select { |label| label.downcase.start_with?('ubuntu') }
        linux_labels.each do |label|
          fail_check("#{file}:#{id}: hosted Linux runner #{label} is not a pinned image") unless PINNED_LINUX.include?(label)
        end
        linux += 1 unless linux_labels.empty?
      end
    end
  end
  fail_check('no hosted Linux runner was inspected') if linux.zero?

  ci = load_jobs('ci.yml', sources.fetch(:workflows).fetch('ci.yml'))
  produced = ci.flat_map { |id, job| check_names(id, job) }
  REQUIRED_CHECKS.each do |name|
    fail_check("ci.yml must report required check #{name} exactly once") unless produced.count(name) == 1
  end

  floor = sources.fetch(:train)[/\. as \$checks \| \[(.*?)\] \|/m, 1]
  fail_check('release train required-check floor not found') if floor.nil?
  fail_check('release train floor differs from the required checks') unless floor.scan(/"([^"]+)"/).flatten.sort == REQUIRED_CHECKS.sort

  fixture = sources.fetch(:train_test)[/^    checks='(\[.*\])'$/, 1]
  fail_check('release train check fixture not found') if fixture.nil?
  names = JSON.parse(fixture).map { |entry| entry.fetch('name') }
  fail_check('release train fixture differs from the required checks') unless names.sort == REQUIRED_CHECKS.sort
  fail_check('missing_matrix fixture must drop a matrix check') unless names.last.start_with?('avatar-renderer-portability (')
  linux
end

begin
  workflows = Dir.glob(File.join(ROOT, '.github/workflows/*.{yml,yaml}')).sort.map do |path|
    [File.basename(path), File.read(path)]
  end.to_h
  sources = {
    workflows: workflows,
    train: File.read(File.join(ROOT, 'scripts/roll-train.sh')),
    train_test: File.read(File.join(ROOT, 'scripts/test-roll-train.sh'))
  }
  inspected = check(sources)

  floating = 'hosted Linux runner ubuntu-latest is not a pinned image'
  avatar = 'ci.yml must report required check avatar-renderer-portability (ubuntu-latest) exactly once'
  # name, workflow file or source key, text to find, replacement, expected reason
  canaries = [
    ['floating literal label', 'ci.yml', "runs-on: ubuntu-24.04\n", "runs-on: ubuntu-latest\n", floating],
    ['floating matrix cell', 'ci.yml', "            runner: ubuntu-24.04\n", "            runner: ubuntu-latest\n", floating],
    ['unreviewed new image', 'release.yml', "runs-on: ubuntu-24.04\n", "runs-on: ubuntu-26.04\n",
     'hosted Linux runner ubuntu-26.04 is not a pinned image'],
    ['floating release matrix', 'release.yml',
     "          - target: ubuntu-latest\n            runner: ubuntu-24.04\n",
     "          - target: ubuntu-latest\n            runner: ubuntu-latest\n", floating],
    ['floating probe workflow', 'agent-email-canary.yml', "runs-on: ubuntu-24.04\n", "runs-on: ubuntu-latest\n", floating],
    ['label list expression', 'agent-email-storage-probe.yml', "    runs-on: ubuntu-24.04\n",
     "    strategy:\n      matrix:\n        runner: [ubuntu-latest]\n    runs-on: [\"${{ matrix.runner }}\"]\n", floating],
    ['matrix value that is a label list', 'ci.yml', "            runner: ubuntu-24.04\n", "            runner: [ubuntu-latest]\n", floating],
    ['mixed-case label', 'release-channel-reconcile.yml', "runs-on: ubuntu-24.04\n", "runs-on: Ubuntu-Latest\n",
     'hosted Linux runner Ubuntu-Latest is not a pinned image'],
    ['floating required matrix runner', 'ci.yml',
     "          - check: ubuntu-latest\n            runner: ubuntu-24.04\n",
     "          - check: ubuntu-latest\n            runner: ubuntu-latest\n", floating],
    ['renamed required matrix check', 'ci.yml', "          - check: ubuntu-latest\n", "          - check: ubuntu-24.04\n", avatar],
    ['derived matrix check name', 'ci.yml', "    name: avatar-renderer-portability (${{ matrix.check }})\n", '', avatar],
    ['renamed aggregate check', 'ci.yml', "    name: go\n", "    name: go-gates\n", 'ci.yml must report required check go exactly once'],
    ['renamed required job', 'ci.yml', "  helm:\n", "  helm-charts:\n", 'ci.yml must report required check helm exactly once'],
    ['renamed train floor', :train, '"avatar-renderer-portability (ubuntu-latest)"',
     '"avatar-renderer-portability (ubuntu-24.04)"', 'release train floor differs from the required checks'],
    ['renamed train fixture', :train_test, '{"name":"avatar-renderer-portability (ubuntu-latest)"',
     '{"name":"avatar-renderer-portability (ubuntu-24.04)"', 'release train fixture differs from the required checks']
  ]
  canaries.each do |name, target, before, after, reason|
    source = target.is_a?(Symbol) ? sources.fetch(target) : workflows.fetch(target)
    fail_check("canary has no target: #{name}") unless source.include?(before)
    changed = source.sub(before) { after }
    mutant = target.is_a?(Symbol) ? sources.merge(target => changed) : sources.merge(workflows: workflows.merge(target => changed))
    begin
      check(mutant)
    rescue ArgumentError => error
      next if error.message.include?(reason)
      fail_check("canary rejected for another reason: #{name}: #{error.message}")
    end
    fail_check("canary accepted: #{name} (update the canary table when the allowed list or a frozen name changes)")
  end

  %w[Makefile .github/workflows/ci.yml .github/workflows/release.yml].each do |gate|
    fail_check("contract is not wired into #{gate}") unless File.read(File.join(ROOT, gate)).include?('bash scripts/test-workflow-runner-labels.sh')
  end
  puts "workflow runner labels: PASS (#{inspected} hosted Linux runner cells pinned, #{REQUIRED_CHECKS.size} required check names frozen, #{canaries.size} rejecting canaries)"
rescue ArgumentError, KeyError, JSON::ParserError, Psych::Exception, SystemCallError => error
  warn "workflow runner labels: FAIL: #{error.message}"
  exit 1
end
RUBY
