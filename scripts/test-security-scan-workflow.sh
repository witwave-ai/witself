#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
export http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9
export HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9
export GOPROXY=off npm_config_offline=true

ruby -E UTF-8:UTF-8 - "$SOURCE_ROOT" <<'RUBY'
require 'yaml'

ROOT = ARGV.fetch(0)
ACTION_NAMES = %w[checkout setup-go setup-node upload-artifact download-artifact].map { |name| "actions/#{name}" }.freeze
ISSUE_STEP = 'Open, update or close the tracking issue'.freeze
SCAN_COMMAND = 'bash scripts/security-scan.sh all --release-version "$RELEASE_VERSION" --report "$RUNNER_TEMP/security-scan/report.md"'.freeze
RENDER_COMMAND = 'bash scripts/security-scan-issue.sh render --detail "$SECURITY_SCAN_PUBLIC_DETAIL" --report "$RUNNER_TEMP/security-scan/report.md" --out "$RUNNER_TEMP/security-scan-public/report.md"'.freeze
SYNC_COMMAND = 'bash scripts/security-scan-issue.sh sync --report "$RUNNER_TEMP/security-scan-public/report.md" --scan-result "$SCAN_RESULT" --run-url "$RUN_URL" --repo "$GITHUB_REPOSITORY"'.freeze
GUARD = '${{ !cancelled() }}'.freeze

def read(relative)
  File.read(File.join(ROOT, relative), encoding: 'UTF-8')
end

def require_check(condition, reason)
  raise ArgumentError, reason unless condition
end

def action_step(steps, action)
  matches = steps.select { |step| step.fetch('uses', '').start_with?("actions/#{action}@") }
  require_check(matches.length == 1, "W4: exactly one #{action} step per job")
  matches.first
end

def named_step(steps, name)
  matches = steps.select { |step| step['name'] == name }
  require_check(matches.length == 1, "workflow step missing or duplicated: #{name}")
  matches.first
end

def check(text, ci, release, required_checks)
  workflow = YAML.safe_load(text, aliases: false)
  require_check(workflow.is_a?(Hash), 'workflow must be a mapping')
  triggers = workflow['on'] || workflow[true]
  require_check(triggers.is_a?(Hash) && triggers.keys.sort == %w[schedule workflow_dispatch], 'W1: scheduled and dispatch triggers only')
  schedule = triggers['schedule']
  require_check(schedule.is_a?(Array) && schedule.length == 1 && schedule.first.is_a?(Hash), 'W1: one cron entry')
  cron = schedule.first['cron']
  require_check(cron.is_a?(String) && cron.split.length == 5, 'W1: five-field cron')
  minute, hour, month_day, month, week_day = cron.split
  require_check(minute.match?(/\A[0-9]+\z/) && minute.to_i.between?(1, 59) && hour.match?(/\A[0-9]+\z/) && hour.to_i.between?(0, 23) && month_day == '*' && month == '*' && week_day.match?(/\A[0-6]\z/), 'W1: off-hour weekly cron')
  require_check(cron == '17 13 * * 1', 'W1: Monday 13:17 UTC')
  require_check(triggers['workflow_dispatch'] == {}, 'W1: dispatch has no inputs')

  jobs = workflow['jobs']
  require_check(jobs.is_a?(Hash) && jobs.keys.sort == %w[report scan] && jobs.values.all? { |job| job.is_a?(Hash) }, 'W2: exactly scan and report jobs')
  scan, report = jobs.values_at('scan', 'report')
  require_check(workflow['permissions'] == { 'contents' => 'read' }, 'W2: top-level read-only permissions')
  require_check(scan['permissions'] == { 'contents' => 'read' }, 'W2: scan read-only permissions')
  require_check(report['permissions'] == { 'contents' => 'read', 'issues' => 'write' }, 'W2: report issue-write permissions only')
  jobs.each_value do |job|
    require_check(job['runs-on'] == 'ubuntu-24.04', 'W3: pinned hosted runner')
    require_check(job['steps'].is_a?(Array) && job['steps'].all? { |step| step.is_a?(Hash) }, 'workflow steps must be mappings')
  end
  scan_steps, report_steps = scan['steps'], report['steps']
  steps = scan_steps + report_steps

  uses = text.scan(/^\s*(?:-\s+)?uses:\s*(.+)$/).flatten
  require_check(uses.length == 6 && uses.all? { |use| use.match?(/\A[\w\/-]+@[0-9a-f]{40} # v[\w.+-]+\z/) }, 'W4: version-commented SHA-pinned actions')
  require_check(uses.map { |use| use.split('@').first }.uniq.sort == ACTION_NAMES.sort, 'W4: only approved actions')
  uses.each do |use|
    name, pin = use.split(' ').first.split('@')
    ci_pins = ci.scan(/uses:\s*#{Regexp.escape(name)}@([0-9a-f]{40})(?:\s|$)/).flatten.uniq
    require_check(ci_pins == [pin], "W4: #{name} matches CI pin")
  end
  checkouts = steps.select { |step| step.fetch('uses', '').start_with?('actions/checkout@') }
  require_check(checkouts.length == 2 && checkouts.all? { |step| step.dig('with', 'persist-credentials') == false }, 'W5: checkout credentials disabled')
  require_check(!text.match?(/secrets\s*[.\[]|set\s+-x|ACTIONS_STEP_DEBUG/), 'W5: no secrets or credential tracing')
  token_steps = jobs.flat_map do |id, job|
    require_check(!job.fetch('env', {}).key?('GH_TOKEN'), 'W5: no job-level token')
    job['steps'].select { |step| step.fetch('env', {}).key?('GH_TOKEN') }.map { |step| [id, step['name'], step.dig('env', 'GH_TOKEN')] }
  end
  require_check(!workflow.fetch('env', {}).key?('GH_TOKEN') && token_steps == [['scan', 'Resolve latest release', '${{ github.token }}'], ['report', ISSUE_STEP, '${{ github.token }}']] && text.scan(/GH_TOKEN/).length == 2, 'W5: token reaches only release lookup and issue sync')

  require_check(report['needs'] == 'scan', 'W6: report needs scan')
  require_check(report['if'] == "${{ !cancelled() && github.ref == 'refs/heads/main' }}", 'W6: uncancelled main-only report')
  require_check(scan['timeout-minutes'].is_a?(Integer) && scan['timeout-minutes'].between?(30, 90), 'W6: bounded scan timeout')
  require_check(report['timeout-minutes'] == 10, 'W6: bounded report timeout')
  renderer = named_step(scan_steps, 'Render public report')
  summary = named_step(scan_steps, 'Job summary')
  upload = action_step(scan_steps, 'upload-artifact')
  require_check([renderer, summary, upload].all? { |step| step['if'] == GUARD }, 'W6: public reporting continues after scan failure')
  require_check(workflow['concurrency'] == { 'group' => 'security-scan', 'cancel-in-progress' => false }, 'W6: serialized uncancelled runs')

  require_check(%w[counts no-cells full].include?(workflow.dig('env', 'SECURITY_SCAN_PUBLIC_DETAIL')), 'W7: allowed public detail mode')
  require_check(renderer['run'] == RENDER_COMMAND, 'W7: renderer uses public detail switch')
  require_check(!ci.include?('security-scan.yml') && !release.include?('security-scan.yml'), 'W8: CI and release do not reference scheduled workflow')
  names = jobs.map { |id, job| job.fetch('name', id) }
  require_check((names & required_checks).empty?, 'W8: scheduled jobs are not required checks')

  download = action_step(report_steps, 'download-artifact')
  require_check(download['continue-on-error'] == true, 'W10: artifact download fails softly')
  sync = named_step(report_steps, ISSUE_STEP)
  require_check(sync['run'] == SYNC_COMMAND, 'W10: sync uses public report and explicit repository')
  require_check(download['with'] == { 'name' => 'security-scan-report', 'path' => '${{ runner.temp }}/security-scan-public' }, 'W10: download public artifact only')
  require_check(sync['env'] == { 'GH_TOKEN' => '${{ github.token }}', 'SCAN_RESULT' => '${{ needs.scan.result }}', 'RUN_URL' => '${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}' }, 'W10: scan result and run link reach sync')

  scanner = named_step(scan_steps, 'Scan')
  require_check(scanner['run'] == SCAN_COMMAND && text.scan('scripts/security-scan.sh').length == 1, 'W11: only the scan step invokes all scanners')
  require_check(scanner['env'] == { 'RELEASE_VERSION' => '${{ steps.release.outputs.version }}' } && !scanner.key?('continue-on-error'), 'W11: scanner uses resolved release and failures stay red')
  require_check(!scan.key?('continue-on-error'), 'W11: failed scan job stays red')
  require_check(scan_steps.length == 8 && report_steps.length == 3 && report_steps.none? { |step| step.fetch('run', '').include?('scripts/security-scan.sh') }, 'W11: report job runs no scanner or extra steps')
  require_check(summary['run'] == 'cat "$RUNNER_TEMP/security-scan-public/report.md" >> "$GITHUB_STEP_SUMMARY"', 'W11: summary publishes public report only')
  require_check(upload['with'] == { 'name' => 'security-scan-report', 'path' => '${{ runner.temp }}/security-scan-public/report.md', 'if-no-files-found' => 'error', 'retention-days' => 30 }, 'W11: upload publishes public report only')
  setup_go = action_step(scan_steps, 'setup-go')
  setup_node = action_step(scan_steps, 'setup-node')
  require_check(setup_go['with'] == { 'go-version-file' => 'go.mod', 'cache' => false } && setup_node['with'] == { 'node-version' => 22 }, 'W11: toolchains match scan contract')
  resolver = named_step(scan_steps, 'Resolve latest release')
  require_check(scan_steps == [action_step(scan_steps, 'checkout'), setup_go, setup_node, resolver, scanner, renderer, summary, upload], 'W11: scan steps follow required order')
  require_check(report_steps == [action_step(report_steps, 'checkout'), download, sync], 'W11: report steps follow required order')
  require_check(resolver['id'] == 'release' && resolver['run'].include?('gh release view --repo "$GITHUB_REPOSITORY" --json tagName --jq .tagName') && resolver['run'].include?('^v[0-9]+\.[0-9]+\.[0-9]+$') && resolver['run'].include?('echo "version=${tag#v}" >> "$GITHUB_OUTPUT"'), 'W11: validated release lookup')
end

begin
  ci = read('.github/workflows/ci.yml')
  release = read('.github/workflows/release.yml')
  required_source = read('scripts/test-workflow-runner-labels.sh')[/^REQUIRED_CHECKS = \[(.*?)\]\.freeze/m, 1]
  require_check(!required_source.nil?, 'W8: required check list found')
  required_checks = required_source.scan(/'([^']+)'/).flatten
  require_check(!required_checks.empty?, 'W8: required check names found')
  source = read('.github/workflows/security-scan.yml')
  check(source, ci, release, required_checks)
  callers = Dir.glob(File.join(ROOT, '.github/workflows/*.{yml,yaml}')).select do |path|
    File.read(path, encoding: 'UTF-8').include?('scripts/security-scan.sh')
  end.map { |path| File.basename(path) }
  require_check(callers == ['security-scan.yml'], 'W11: scheduled workflow is the only workflow scanner caller')

  checkout = source[/actions\/checkout@[0-9a-f]{40}/]
  detail = YAML.safe_load(source, aliases: false).dig('env', 'SECURITY_SCAN_PUBLIC_DETAIL')
  scan_source = source[/^      - name: Scan\n.*?(?=^      - )/m]
  render_source = source[/^      - name: Render public report\n.*?(?=^      - )/m]
  download_source = source[/^      - uses: actions\/download-artifact@.*?\n.*?(?=^      - )/m]
  sync_source = source[/^      - name: #{Regexp.escape(ISSUE_STEP)}\n.*\z/m]
  require_check([scan_source, render_source, download_source, sync_source].none?(&:nil?), 'W9: step-order canary targets found')
  # Each candidate must hit its intended assertion, not merely fail YAML parsing.
  canaries = [
    ['pull request trigger', '  workflow_dispatch: {}', "  pull_request: {}\n  workflow_dispatch: {}", 'W1: scheduled and dispatch triggers only'],
    ['scan issue-write permission', "    permissions:\n      contents: read\n", "    permissions:\n      contents: read\n      issues: write\n", 'W2: scan read-only permissions'],
    ['floating action', checkout, 'actions/checkout@v7', 'W4: version-commented SHA-pinned actions'],
    ['floating runner', 'runs-on: ubuntu-24.04', 'runs-on: ubuntu-latest', 'W3: pinned hosted runner'],
    ['secret token', "          GH_TOKEN: ${{ github.token }}\n          SCAN_RESULT:", "          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n          SCAN_RESULT:", 'W5: no secrets or credential tracing'],
    ['persisted credentials', "          persist-credentials: false\n", '', 'W5: checkout credentials disabled'],
    ['branch issue writes', " && github.ref == 'refs/heads/main'", '', 'W6: uncancelled main-only report'],
    ['invalid public detail', "SECURITY_SCAN_PUBLIC_DETAIL: #{detail}", 'SECURITY_SCAN_PUBLIC_DETAIL: partial', 'W7: allowed public detail mode'],
    ['hard artifact download', "        continue-on-error: true\n", '', 'W10: artifact download fails softly'],
    ['render before scan', scan_source + render_source, render_source + scan_source, 'W11: scan steps follow required order'],
    ['download after sync', download_source + sync_source, sync_source + download_source, 'W11: report steps follow required order']
  ]
  canaries.each do |name, before, after, reason|
    require_check(!before.nil? && source.include?(before), "W9: canary target missing: #{name}")
    begin
      check(source.sub(before) { after }, ci, release, required_checks)
    rescue ArgumentError => error
      require_check(error.message == reason, "W9: canary rejected for wrong reason: #{name}")
      next
    end
    raise ArgumentError, "W9: canary accepted: #{name}"
  end

  %w[test-security-scan-issue.sh test-security-scan-workflow.sh].each do |test|
    command = "bash scripts/#{test}"
    require_check(read('Makefile').scan(/^\t#{Regexp.escape(command)}$/).length == 1, "C3: #{test} wired once in Makefile")
    # The offline contract checks live in the full application lane (helm-full); 'helm' is its frozen required-name wrapper (#737).
    helm = YAML.safe_load(ci, aliases: false).dig('jobs', 'helm-full', 'steps')
    matches = helm.select { |step| step['run'] == command }
    require_check(matches.length == 1 && matches.first['timeout-minutes'] == 5, "C3: #{test} wired once in helm-full with timeout")
  end
  puts "security scan workflow: PASS (W1-W11, #{canaries.length} rejecting canaries, C3 wiring)"
rescue ArgumentError, KeyError, Psych::Exception, SystemCallError => error
  warn "security scan workflow: FAIL: #{error.message.gsub(/[0-9a-fA-F]{64}/, '[digest-redacted]')}"
  exit 1
end
RUBY

if [[ ${SECURITY_SCAN_WORKFLOW_LOCALE_CHECK:-0} != 1 ]]; then
  env -u LANG -u LC_ALL -u LC_CTYPE SECURITY_SCAN_WORKFLOW_LOCALE_CHECK=1 \
    /bin/bash "$SOURCE_ROOT/scripts/test-security-scan-workflow.sh"
  printf '%s\n' 'security scan workflow: PASS (locale-unset self-check)'
fi
