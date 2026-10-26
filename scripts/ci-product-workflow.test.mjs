#!/usr/bin/env node
import assert from 'node:assert/strict';
import {execFileSync, spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {fileURLToPath} from 'node:url';
import {existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {dirname, join} from 'node:path';
import test from 'node:test';
import {assertGate} from './ci-product-gate.mjs';
import {classifyChanges} from './ci-product-scope.mjs';

const root = fileURLToPath(new URL('../', import.meta.url));
const files = ['ci.yml', 'product-site-checks.yml', 'product-site.yml'];
// Ruby/Psych is already a repository gate dependency. Normalize YAML 1.1's
// interpretation of the top-level `on` key before inspecting workflow data.
const workflows = JSON.parse(execFileSync('ruby', ['-rjson', '-ryaml', '-e', `
  result = ARGV.to_h do |path|
    data = YAML.safe_load(File.read(path), aliases: false)
    data['on'] = data.delete(true) if data.key?(true)
    [File.basename(path), data]
  end
  puts JSON.generate(result)
`, ...files.map(file => `${root}.github/workflows/${file}`)], {encoding: 'utf8'}));

const wrapperDependencies = {
  go: ['go-rest', 'go-store', 'go-infra', 'localcell-windows', 'localcell-unix', 'postgres-backup-image', 'postgres-image-mirror', 'provider-contract-evidence', 'static-analysis'],
  'release-config': ['release-config-full'],
  'homebrew-formula': ['homebrew-formula-full'],
  helm: ['helm-full'],
  'avatar-renderer-portability': ['avatar-renderer-portability-full'],
};
const fullJobs = [...new Set(Object.values(wrapperDependencies).flat()), 'provider-integration-contract'];
const checkNames = ['ubuntu-latest', 'ubuntu-24.04-arm'];
const testCommand = 'node --test scripts/ci-product-scope.test.mjs scripts/ci-product-gate.test.mjs scripts/ci-product-workflow.test.mjs scripts/check-docs.test.mjs scripts/run-docs-check.test.mjs';
const expression = value => String(value ?? '').replace(/^\s*\$\{\{\s*|\s*\}\}\s*$/g, '').trim();
const list = value => Array.isArray(value) ? value : [value];
const sorted = value => [...value].sort();

function needs(job, expected, label) {
  assert.deepEqual(sorted(list(job.needs)), sorted(expected), `${label}: exact dependency fence`);
}

function action(job, prefix, label) {
  const found = job.steps.filter(step => step.uses?.startsWith(prefix));
  assert.equal(found.length, 1, `${label}: exactly one ${prefix}`);
  assert.match(found[0].uses, /@[a-f0-9]{40}$/, `${label}: immutable action`);
  return found[0];
}

function assertClassifier(job, outputs, label) {
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.with['fetch-depth'], 0, `${label}: full comparison history`);
  assert.equal(checkout.with['persist-credentials'], false, `${label}: checkout has no persisted credential`);
  assert.equal(action(job, 'actions/setup-node@', label).with['node-version'], 22);
  for (const name of outputs) {
    assert.equal(job.outputs[name], `\${{ steps.scope.outputs.${name} }}`, `${label}: scope output ${name}`);
  }
  const scope = job.steps.filter(step => step.id === 'scope');
  assert.equal(scope.length, 1, `${label}: one classifier`);
  assert.equal(scope[0].run, 'node scripts/ci-product-scope.mjs');
  assert.equal(scope[0].if, undefined, `${label}: classifier cannot be skipped`);
  assert.equal(scope[0]['continue-on-error'], undefined, `${label}: classifier cannot ignore failure`);
  assert.equal(job['continue-on-error'], undefined);
  assert.equal(job.if, undefined, `${label}: classifier always runs for workflow events`);
  const check = job.steps.find(step => step.run === testCommand);
  assert(check, `${label}: classifier, gate, and wiring regressions run together`);
  assert.equal(check.if, undefined);
  assert.equal(check['continue-on-error'], undefined);
  assert(job.steps.indexOf(check) < job.steps.indexOf(scope[0]), `${label}: test before classification`);
}

function assertWebsiteCall(job, label) {
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.website == 'true'", `${label}: positive website classification`);
  assert.equal(job.uses, './.github/workflows/product-site-checks.yml');
  assert.equal(job.secrets, undefined, `${label}: no inherited deployment secrets`);
  assert.equal(job['continue-on-error'], undefined);
}

function assertDocsJob(job) {
  const label = 'documentation validation';
  assert(job, 'Missing documentation validation job');
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.docs == 'true'", 'Positive documentation classification only');
  assert.equal(job['runs-on'], 'ubuntu-24.04');
  assert.equal(job['continue-on-error'], undefined);
  assert.equal(job.services, undefined, 'Documentation validation needs no database or container service');
  assert.equal(job.env, undefined, 'Documentation validation needs no database or deployment configuration');
  assert.equal(job.environment, undefined, 'Documentation validation cannot enter a deployment environment');
  assert(!JSON.stringify(job).includes('secrets.'), 'Documentation validation receives no deployment secrets');
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.with['fetch-depth'], 0, 'Local and CI docs runner needs complete comparison history');
  assert.equal(checkout.with['persist-credentials'], false);
  assert.equal(action(job, 'actions/setup-node@', label).with['node-version'], 22);
  const go = action(job, 'actions/setup-go@', label);
  assert.equal(expression(go.if), "needs.changes.outputs.docs_index == 'true'", 'Only the runbook index guard needs Go');
  assert.equal(go.with['go-version-file'], 'go.mod');
  assert.equal(go.with['cache-dependency-path'], 'go.sum');
  const checks = job.steps.filter(step => step.run !== undefined);
  assert.deepEqual(checks.map(step => step.run), ['make check-docs'], 'Local and CI documentation checks share one bounded command');
  assert.equal(checks[0].if, undefined);
  assert.equal(checks[0]['continue-on-error'], undefined);
}

function assertLocalcellWindowsJob(ci) {
  const label = 'localcell-windows';
  assert.equal(ci.env, undefined, `${label}: no inherited workflow environment`);
  const job = ci.jobs[label];
  assert(job, 'Missing native Windows Local Cell job');
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.application == 'true'");
  assert.equal(job['runs-on'], 'windows-latest');
  assert.deepEqual(job.permissions, {contents: 'read'});
  assert.equal(job['timeout-minutes'], 45);
  for (const key of ['continue-on-error', 'environment', 'env', 'services', 'secrets', 'strategy', 'defaults']) {
    assert.equal(job[key], undefined, `${label}: no ${key}`);
  }
  assert(!JSON.stringify(job).includes('secrets.'), `${label}: no secret access`);
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.uses, action(ci.jobs['go-rest'], 'actions/checkout@', 'go-rest').uses);
  assert.deepEqual(checkout.with, {'persist-credentials': false});
  const go = action(job, 'actions/setup-go@', label);
  assert.equal(go.uses, action(ci.jobs['go-rest'], 'actions/setup-go@', 'go-rest').uses);
  assert.deepEqual(go.with, {'go-version-file': 'go.mod', cache: true, 'cache-dependency-path': 'go.sum'});
  const node = action(job, 'actions/setup-node@', label);
  assert.equal(node.uses, action(ci.jobs.changes, 'actions/setup-node@', 'changes').uses);
  assert.deepEqual(node.with, {'node-version': 22});
  assert.deepEqual(job.steps.slice(0, 3), [checkout, go, node]);
  const checks = job.steps.slice(3);
  const packages = './cmd/witself-server ./cmd/witself-worker ./internal/localcell/... ./internal/tools/localcellharness/... ./internal/runtimeconfig/...';
  assert.deepEqual(checks.map(step => step.run.trim()), [
    `go build ${packages}`,
    `go vet ${packages}`,
    'node --test scripts/ci-go-test-json-check.test.mjs',
    [
      '$goExit = 1',
      '& {',
      '  go test -json ./internal/localcell ./internal/runtimeconfig ./internal/tools/localcellharness -count=1 -timeout=10m',
      '  $script:goExit = $LASTEXITCODE',
      '} | node scripts/ci-go-test-json-check.mjs',
      '$parserExit = $LASTEXITCODE',
      'if ($goExit -ne 0 -or $parserExit -ne 0) { exit 1 }',
    ].join('\n'),
    './scripts/ci-postgres16-windows.ps1',
    [
      "$proofJSON = Join-Path $env:RUNNER_TEMP 'lc-pg16\\proof.json'",
      "go test -json ./internal/localcell -run '^TestRealLifecycle$' -count=1 -timeout=15m > $proofJSON 2>&1",
      '$goExit = $LASTEXITCODE',
      'node scripts/ci-go-test-json-check.mjs --input $proofJSON --diagnose --require TestRealLifecycle,TestRealLifecycle/InstallStartRestartRemove,TestRealLifecycle/TwoCellsAreIndependent,TestRealLifecycle/InterruptedStartRepair,TestRealLifecycle/WaitsForReadiness',
      '$parserExit = $LASTEXITCODE',
      'if ($goExit -ne 0 -or $parserExit -ne 0) { exit 1 }',
    ].join('\n'),
    './scripts/ci-postgres16-windows.ps1 -Cleanup',
  ], `${label}: unit execution stays unchanged and the native proof is narrow, complete, and fail closed`);
  for (const step of checks) assert.equal(step.shell, 'pwsh');
  const proof = checks[5];
  const cleanup = checks[6];
  assert.deepEqual(proof.env, {
    WITSELF_LOCALCELL_PGBIN: '${{ runner.temp }}\\lc-pg16\\pgsql\\bin',
    TEMP: '${{ runner.temp }}\\lc-pg16\\testtmp',
    TMP: '${{ runner.temp }}\\lc-pg16\\testtmp',
  });
  assert.equal(expression(cleanup.if), 'always()', `${label}: cleanup also runs after failure`);
  for (const step of job.steps) {
    for (const key of ['continue-on-error', 'working-directory']) {
      assert.equal(step[key], undefined, `${label}: step cannot override ${key}`);
    }
    if (step !== cleanup) assert.equal(step.if, undefined, `${label}: required steps cannot skip`);
    if (step !== proof) assert.equal(step.env, undefined, `${label}: only native proof receives PostgreSQL`);
  }
}

const unixPlatforms = [
  {platform: 'linux-x64', runner: 'ubuntu-24.04'},
  {platform: 'macos-arm64', runner: 'macos-15'},
  {platform: 'macos-intel', runner: 'macos-15-intel'},
];
const lifecycleRequired = 'TestRealLifecycle,TestRealLifecycle/InstallStartRestartRemove,TestRealLifecycle/TwoCellsAreIndependent,TestRealLifecycle/InterruptedStartRepair,TestRealLifecycle/WaitsForReadiness';

function assertLocalcellUnixJob(ci) {
  const label = 'localcell-unix';
  const job = ci.jobs[label];
  assert(job, 'Missing native Unix Local Cell job');
  assert.equal(ci.env, undefined, `${label}: no inherited workflow environment`);
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.application == 'true'");
  assert.equal(job['runs-on'], '${{ matrix.runner }}');
  assert.deepEqual(job.strategy, {'fail-fast': false, matrix: {include: unixPlatforms}});
  assert.deepEqual(job.permissions, {contents: 'read'});
  assert.equal(job['timeout-minutes'], 45);
  for (const key of ['continue-on-error', 'environment', 'env', 'services', 'secrets', 'defaults']) {
    assert.equal(job[key], undefined, `${label}: no ${key}`);
  }
  assert(!JSON.stringify(job).includes('secrets.'), `${label}: no secret access`);
  const windows = ci.jobs['localcell-windows'];
  assert.deepEqual(job.steps.slice(0, 3), windows.steps.slice(0, 3), `${label}: same pinned checkout and toolchain`);
  const checks = job.steps.slice(3);
  const packages = './cmd/witself-server ./cmd/witself-worker ./internal/localcell/... ./internal/tools/localcellharness/... ./internal/runtimeconfig/...';
  assert.deepEqual(checks.map(step => step.name), windows.steps.slice(3).map(step => step.name), `${label}: same ordered proof stages`);
  assert.deepEqual(checks.map(step => step.run.trim()), [
    `go build ${packages}`,
    `go vet ${packages}`,
    'node --test scripts/ci-go-test-json-check.test.mjs',
    [
      'set +e',
      'go test -json ./internal/localcell ./internal/runtimeconfig ./internal/tools/localcellharness -count=1 -timeout=10m | node scripts/ci-go-test-json-check.mjs',
      'test_exits=("${PIPESTATUS[@]}")',
      'set -e',
      'if (( test_exits[0] != 0 || test_exits[1] != 0 )); then exit 1; fi',
    ].join('\n'),
    "bash scripts/ci-postgres16-unix.sh '${{ matrix.platform }}'",
    [
      'proof_json="$RUNNER_TEMP/lc-pg16/proof.json"',
      'go_exit=0',
      'go test -json ./internal/localcell -run \'^TestRealLifecycle$\' -count=1 -timeout=15m > "$proof_json" 2>&1 || go_exit=$?',
      'parser_exit=0',
      `node scripts/ci-go-test-json-check.mjs --input "$proof_json" --diagnose --require ${lifecycleRequired} || parser_exit=$?`,
      'if (( go_exit != 0 || parser_exit != 0 )); then exit 1; fi',
    ].join('\n'),
    'bash scripts/ci-postgres16-unix.sh --cleanup',
  ], `${label}: narrow proof preserves native and checker failures`);
  for (const step of checks) assert.equal(step.shell, 'bash');
  const proof = checks[5];
  const cleanup = checks[6];
  assert.deepEqual(proof.env, {TMPDIR: '${{ runner.temp }}/lc-pg16/testtmp'});
  assert.equal(expression(cleanup.if), 'always()', `${label}: cleanup also runs after failure`);
  for (const step of job.steps) {
    for (const key of ['continue-on-error', 'working-directory']) {
      assert.equal(step[key], undefined, `${label}: step cannot override ${key}`);
    }
    if (step !== cleanup) assert.equal(step.if, undefined, `${label}: required steps use default success gating`);
    if (step !== proof) assert.equal(step.env, undefined, `${label}: only proof uses the private temporary tree`);
  }
}

function assertUnixPostgresPins(pins) {
  assert.deepEqual(Object.keys(pins).sort(), unixPlatforms.map(row => row.platform).sort(), 'One PostgreSQL pin per Unix matrix row');
  for (const {platform} of unixPlatforms) {
    const pin = pins[platform];
    assert.deepEqual(Object.keys(pin).sort(), ['major', 'sha256', 'url', 'version']);
    assert.equal(pin.major, 16, `${platform}: PostgreSQL sixteen required`);
    // Keep pin values out of diagnostics, including malformed checksum values.
    assert(typeof pin.sha256 === 'string' && /^[a-f0-9]{64}$/.test(pin.sha256), `${platform}: complete lowercase SHA-256 required`);
    assert(pin.version === '16.10', `${platform}: exact PostgreSQL sixteen source version`);
    const url = new URL(pin.url);
    assert(url.protocol === 'https:' && url.hostname === 'apt-archive.postgresql.org', `${platform}: allowlisted HTTPS archive host`);
    assert(url.pathname === `/pub/repos/apt/pool/main/p/postgresql-16/postgresql-16_${pin.version}.orig.tar.bz2`, `${platform}: exact PostgreSQL source archive`);
    assert(!url.username && !url.password && !url.port && !url.search && !url.hash, `${platform}: plain pinned archive URL`);
  }
  assert(new Set(Object.values(pins).map(pin => pin.sha256)).size === 1, 'Every row builds the same verified source bytes');
}

const unixPostgresPins = JSON.parse(readFileSync(new URL('./ci-postgres16-unix.json', import.meta.url), 'utf8'));

// The Unix provisioner must keep bash strict mode including pipefail as its first executable statement; a comment or
// string containing the words must not satisfy the check (review LC-S2-PL1R, finding E1).
const unixProvisionerSource = readFileSync(new URL('./ci-postgres16-unix.sh', import.meta.url), 'utf8');
function firstExecutableStatement(source) {
  const lines = source.split('\n').map((line) => line.trim());
  return lines.find((line, index) => index > 0 && line !== '' && !line.startsWith('#')) ?? '';
}
test('unix provisioner keeps strict mode with pipefail as its first executable statement', () => {
  assert.equal(firstExecutableStatement(unixProvisionerSource), 'set -euo pipefail');
  assert.ok(unixProvisionerSource.startsWith('#!/usr/bin/env bash\n'), 'bash shebang');
  const weakened = unixProvisionerSource.replace('set -euo pipefail', 'set -eu');
  assert.notEqual(firstExecutableStatement(weakened), 'set -euo pipefail', 'dropping pipefail is detected');
  const decoy = unixProvisionerSource.replace('set -euo pipefail', '# set -euo pipefail\nset -eu');
  assert.notEqual(firstExecutableStatement(decoy), 'set -euo pipefail', 'a commented decoy is not strict mode');
});
test('Unix PostgreSQL pins cover every native row with exact versions and verified digests', () => {
  assertUnixPostgresPins(unixPostgresPins);
});

test('Unix PostgreSQL provisioner is enrolled in the repository shell static analysis', () => {
  const source = readFileSync(new URL('./test-ci-static-analysis.sh', import.meta.url), 'utf8');
  assert(source.includes("mapfile -d '' shell_files < <(find scripts -type f -name '*.sh' -print0)"), 'Static analysis discovers every shell script');
  assert(source.includes('"$work_dir/shellcheck-v${shellcheck_version}/shellcheck" install.sh "${shell_files[@]}"'), 'Static analysis checks every discovered shell script');
});

for (const [name, mutate] of [
  ['missing platform', pins => { delete pins['linux-x64']; }],
  ['extra platform', pins => { pins['linux-arm64'] = {...pins['linux-x64']}; }],
  ['missing digest', pins => { delete pins['linux-x64'].sha256; }],
  ['malformed digest', pins => { pins['linux-x64'].sha256 = 'unverified'; }],
  ['wrong major', pins => { pins['linux-x64'].major = 17; }],
  ['unversioned artifact', pins => { pins['linux-x64'].version = 'latest'; }],
  ['HTTP archive', pins => { pins['linux-x64'].url = pins['linux-x64'].url.replace('https:', 'http:'); }],
  ['untrusted host', pins => { pins['linux-x64'].url = pins['linux-x64'].url.replace('apt-archive.postgresql.org', 'example.invalid'); }],
  ['wrong source version', pins => { pins['linux-x64'].url = pins['linux-x64'].url.replace('16.10', '16.9'); }],
  ['different source bytes per platform', pins => { pins['linux-x64'].sha256 = createHash('sha256').update('different source').digest('hex'); }],
  ['embedded URL credentials', pins => { pins['linux-x64'].url = pins['linux-x64'].url.replace('https://', 'https://user:password@'); }],
]) {
  test(`rejects Unix PostgreSQL pin mutation: ${name}`, () => {
    const pins = structuredClone(unixPostgresPins);
    mutate(pins);
    assert.throws(() => assertUnixPostgresPins(pins));
  });
}

// Run the actual provisioner with generated command shims. The shims never
// download or execute PostgreSQL; the script's validation, sequencing, export,
// and cleanup stay unchanged. Trace records contain fixed operation names only.
const unixProvisionMockDriver = String.raw`
const {appendFileSync, mkdirSync, readFileSync, writeFileSync} = require('node:fs');
const {createHash} = require('node:crypto');
const {basename, join} = require('node:path');
const config = JSON.parse(readFileSync(process.env.LC_PROOF_MOCK_CONFIG, 'utf8'));
const command = basename(process.argv[2]);
const args = process.argv.slice(3);
const trace = value => appendFileSync(config.trace, value + '\n');
const executable = (path, name) => writeFileSync(path,
  '#!/bin/bash\nexec "$LC_PROOF_MOCK_NODE" "$LC_PROOF_MOCK_DRIVER" ' + name + ' "$@"\n', {mode: 0o755});
if (command === 'uname') {
  process.stdout.write(args[0] === '-s' ? config.os + '\n' : config.arch + '\n');
} else if (command === 'curl') {
  trace('download');
  writeFileSync(args[args.indexOf('--output') + 1], 'synthetic PostgreSQL source bytes');
} else if (command === 'shasum') {
  trace('hash');
  const digest = config.mode === 'checksum-mismatch' ? '0'.repeat(64)
    : createHash('sha256').update(readFileSync(args.at(-1))).digest('hex');
  process.stdout.write(digest + '  archive\n');
} else if (command === 'tar') {
  trace('extract');
  const source = join(args[args.indexOf('-C') + 1], 'postgresql-' + config.version);
  mkdirSync(source);
  executable(join(source, 'configure'), 'configure');
} else if (command === 'configure') {
  trace('configure');
  if (!args.includes('--prefix=' + config.runtime)) process.exit(42);
  if (config.mode === 'configure-failure') process.exit(1);
} else if (command === 'make') {
  const install = args[0] === 'install';
  trace(install ? 'install' : 'build');
  if (config.mode === 'build-failure' && !install) process.exit(1);
  if (config.mode === 'install-failure' && install) process.exit(1);
  if (install) {
    for (const directory of ['bin', 'lib', 'share']) mkdirSync(join(config.runtime, directory), {recursive: true});
    for (const name of ['postgres', 'initdb', 'pg_ctl', 'psql', 'createdb']) {
      if (config.mode === 'missing-binary' && name === 'psql') continue;
      executable(join(config.runtime, 'bin', name), name);
    }
  }
} else if (['postgres', 'initdb', 'pg_ctl', 'psql', 'createdb'].includes(command)) {
  if (args.length !== 1 || args[0] !== '--version') process.exit(42);
  trace('version:' + command);
  if (config.mode === 'nonzero-version' && command === 'initdb') process.exit(1);
  const version = config.mode === 'wrong-version' && command === 'initdb' ? '17.1' : config.version;
  process.stdout.write(command + ' (PostgreSQL) ' + version + '\n');
} else {
  process.exit(42);
}
`;

function withUnixProvisionFixture(options, check) {
  const temporary = mkdtempSync(join(root, '.lc-unix-provision-test-'));
  try {
    const scriptDirectory = join(temporary, 'scripts');
    const mockDirectory = join(temporary, 'commands');
    const runner = join(temporary, 'runner');
    for (const directory of [scriptDirectory, mockDirectory, runner]) mkdirSync(directory);
    const script = join(scriptDirectory, 'ci-postgres16-unix.sh');
    writeFileSync(script, readFileSync(new URL('./ci-postgres16-unix.sh', import.meta.url)));
    const pins = structuredClone(unixPostgresPins);
    const digest = createHash('sha256').update('synthetic PostgreSQL source bytes').digest('hex');
    for (const pin of Object.values(pins)) pin.sha256 = digest;
    options.mutatePin?.(pins);
    writeFileSync(join(scriptDirectory, 'ci-postgres16-unix.json'), JSON.stringify(pins));
    const trace = join(temporary, 'operations');
    const githubEnvironment = join(temporary, 'github-env');
    writeFileSync(trace, '');
    writeFileSync(githubEnvironment, '');
    const driver = join(temporary, 'driver.cjs');
    writeFileSync(driver, unixProvisionMockDriver);
    for (const name of ['uname', 'curl', 'shasum', 'tar', 'make']) {
      writeFileSync(join(mockDirectory, name),
        '#!/bin/bash\nexec "$LC_PROOF_MOCK_NODE" "$LC_PROOF_MOCK_DRIVER" "$0" "$@"\n', {mode: 0o755});
    }
    const runtime = join(runner, 'lc-pg16', 'pgsql');
    const config = join(temporary, 'config.json');
    writeFileSync(config, JSON.stringify({
      mode: options.mode ?? 'success', trace, runtime, version: '16.10',
      os: options.os ?? 'Linux', arch: options.arch ?? 'x86_64',
    }));
    // Pass only fixture values and executable search paths; no user environment
    // is exposed to the mocked provisioner or its generated version shims.
    const environment = {
      PATH: [mockDirectory, dirname(process.execPath), '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(':'),
      RUNNER_TEMP: runner, GITHUB_ENV: githubEnvironment,
      LC_PROOF_MOCK_CONFIG: config, LC_PROOF_MOCK_NODE: process.execPath, LC_PROOF_MOCK_DRIVER: driver,
      PYTHONOPTIMIZE: '1',
    };
    const invoke = argument => spawnSync('/bin/bash', [script, argument], {
      env: environment, encoding: 'utf8', timeout: 20_000,
    });
    check({
      run: () => invoke(options.platform ?? 'linux-x64'), cleanup: () => invoke('--cleanup'),
      runtime, proofRoot: join(runner, 'lc-pg16'),
      operations: () => readFileSync(trace, 'utf8').trim().split('\n').filter(Boolean),
      exported: () => readFileSync(githubEnvironment, 'utf8'),
    });
  } finally {
    rmSync(temporary, {recursive: true, force: true});
  }
}

test('Unix provisioner checksum mismatch terminates before extraction or build without export', () => {
  withUnixProvisionFixture({mode: 'checksum-mismatch'}, fixture => {
    const result = fixture.run();
    assert(result.error === undefined && result.status === 1, 'Checksum mismatch must fail the actual script');
    assert.deepEqual(fixture.operations(), ['download', 'hash']);
    assert(fixture.exported() === '', 'A mismatched archive cannot publish PostgreSQL');
    assert(result.stderr.trim() === 'PostgreSQL proof: archive checksum mismatch', 'Checksum failure stays value-free');
    assert(result.stdout === '', 'Checksum failure cannot print archive values');
  });
});

test('Unix provisioner verifies all five executables before export and cleanup is repeatable', () => {
  withUnixProvisionFixture({}, fixture => {
    const result = fixture.run();
    assert(result.error === undefined && result.status === 0, 'Complete mocked provisioning must pass');
    assert.deepEqual(fixture.operations(), [
      'download', 'hash', 'extract', 'configure', 'build', 'install',
      'version:postgres', 'version:initdb', 'version:pg_ctl', 'version:psql', 'version:createdb',
    ]);
    assert(fixture.exported() === `WITSELF_LOCALCELL_PGBIN=${fixture.runtime}/bin\n`, 'Only the completely verified runtime is published');
    assert(existsSync(join(fixture.proofRoot, 'testtmp')), 'Proof temporary files belong to the cleanup tree');
    assert(result.stdout === 'Pinned PostgreSQL sixteen runtime verified\n' && result.stderr === '', 'Successful provisioning has bounded value-free output');
    for (let attempt = 0; attempt < 2; attempt++) {
      const cleanup = fixture.cleanup();
      assert(cleanup.error === undefined && cleanup.status === 0, 'Cleanup must be repeatable');
      assert(!existsSync(fixture.proofRoot), 'Cleanup removes the complete proof tree');
    }
  });
});

for (const [mode, message] of [
  ['wrong-version', 'runtime executable did not report major sixteen'],
  ['missing-binary', 'runtime executable is missing'],
  ['nonzero-version', 'runtime executable failed'],
  ['configure-failure', 'configure failed (runner build.log)'],
  ['build-failure', 'build failed (runner build.log)'],
  ['install-failure', 'staging failed (runner build.log)'],
]) {
  test(`Unix provisioner rejects ${mode} without exporting a runtime`, () => {
    withUnixProvisionFixture({mode}, fixture => {
      const result = fixture.run();
      assert(result.error === undefined && result.status === 1, 'Invalid runtime must fail provisioning');
      assert(fixture.exported() === '', 'Incomplete runtime cannot be published');
      assert(result.stderr.trim() === `PostgreSQL proof: ${message}`, 'Failure uses the expected bounded diagnostic');
      assert(result.stdout === '', 'Failed provisioning cannot print build values');
    });
  });
}

for (const [name, options, message] of [
  ['invalid pin', {mutatePin: pins => { pins['linux-x64'].major = 17; }}, 'invalid pin'],
  ['untrusted pin URL', {mutatePin: pins => { pins['linux-x64'].url = 'https://example.invalid/archive'; }}, 'invalid pin'],
  ['invalid digest', {mutatePin: pins => { pins['linux-x64'].sha256 = 'unverified'; }}, 'invalid pin'],
  ['native platform mismatch', {os: 'Darwin', arch: 'arm64'}, 'platform does not match the native runner'],
]) {
  test(`Unix provisioner rejects ${name} before download`, () => {
    withUnixProvisionFixture(options, fixture => {
      const result = fixture.run();
      assert(result.error === undefined && result.status !== 0, 'Invalid platform or pin must fail provisioning');
      assert.deepEqual(fixture.operations(), []);
      assert(fixture.exported() === '', 'Rejected inputs cannot publish PostgreSQL');
      assert(result.stderr.trim() === `PostgreSQL proof: ${message}`, 'Input rejection stays value-free');
      assert(result.stdout === '', 'Rejected inputs cannot print pin values');
    });
  });
}

for (const stage of ['Test Local Cell packages and require complete results', 'Prove native PostgreSQL sixteen lifecycle']) {
  test(`Unix bash exit propagation: ${stage}`, () => {
    const script = workflows['ci.yml'].jobs['localcell-unix'].steps.find(step => step.name === stage).run;
    const temporary = mkdtempSync(join(root, '.lc-unix-workflow-test-'));
    try {
      mkdirSync(join(temporary, 'lc-pg16'));
      for (const [goExit, checkerExit] of [[0, 0], [1, 0], [0, 1], [1, 1]]) {
        // Execute the actual workflow shell under Actions' bash flags. Functions
        // replace commands only: no Go tests, PostgreSQL, or downloads run here.
        const mock = [
          'RUNNER_TEMP=$1',
          `go() { printf 'contract:captured-go-output\\n'; return ${goExit}; }`,
          `node() { if [[ "$*" != *'--input'* ]]; then cat >/dev/null; fi; printf 'contract:checker\\n'; return ${checkerExit}; }`,
          script,
        ].join('\n');
        const result = spawnSync('bash', ['--noprofile', '--norc', '-e', '-o', 'pipefail', '-c', mock, 'unix-workflow-contract', temporary], {
          encoding: 'utf8', timeout: 10_000,
        });
        assert(result.error === undefined, `${stage}: mock must execute`);
        assert.equal(result.status, goExit || checkerExit ? 1 : 0, `${stage}: native=${goExit} checker=${checkerExit}`);
        assert(result.stdout === 'contract:checker\n', `${stage}: checker always runs and Go output stays captured`);
      }
    } finally {
      rmSync(temporary, {recursive: true, force: true});
    }
  });
}

// This is a deliberately narrow structural contract, not a PowerShell evaluator.
// Preserve offsets and statement boundaries while removing non-executable text.
function postgresProvisionCode(source) {
  const code = source.split('');
  const erase = (start, end) => {
    for (let i = start; i < end; i++) if (!/[\r\n]/.test(code[i])) code[i] = ' ';
  };
  for (let i = 0; i < source.length;) {
    const start = i;
    if (source.startsWith('<#', i)) {
      let depth = 1;
      i += 2;
      while (i < source.length && depth > 0) {
        if (source.startsWith('<#', i)) { depth++; i += 2; }
        else if (source.startsWith('#>', i)) { depth--; i += 2; }
        else i++;
      }
      assert.equal(depth, 0, 'Provisioning block comment must close');
      erase(start, i);
    } else if (source[i] === '#') {
      while (i < source.length && source[i] !== '\n') i++;
      erase(start, i);
    } else if (source[i] === "'" || source[i] === '"') {
      const quote = source[i++];
      assert(source[start - 1] !== '@', 'Provisioning contract does not accept here strings');
      let closed = false;
      while (i < source.length) {
        if (quote === '"' && source[i] === '`') { i += 2; continue; }
        if (source[i++] !== quote) continue;
        if (quote === "'" && source[i] === "'") { i++; continue; }
        closed = true;
        break;
      }
      assert(closed, 'Provisioning string must close');
      erase(start, i);
    } else {
      // Reject continuation syntax outside literals so it cannot disguise a
      // statement boundary from the contract's restricted grammar.
      assert(source[i] !== '`', 'Provisioning contract does not accept escaped code tokens');
      i++;
    }
  }
  return code.join('');
}

function assertPostgresChecksumTermination(source) {
  const code = postgresProvisionCode(source);
  const mismatch = /\bif\s*\(\s*\$actualDigest\s+-cne\s+\$pin\.sha256\s*\)\s*\{/gi;
  const branches = [...code.matchAll(mismatch)];
  assert.equal(branches.length, 1, 'Exactly one executable hash mismatch branch is required');
  const branch = branches[0];
  const open = branch.index + branch[0].length - 1;
  const stack = [];
  let close;
  for (let i = 0; i < code.length; i++) {
    if (i === branch.index) assert.equal(stack.length, 0, 'Hash mismatch branch must execute at script scope');
    if ('({['.includes(code[i])) stack.push(code[i]);
    else if (')}]'.includes(code[i])) {
      assert.equal(stack.pop(), {')': '(', '}': '{', ']': '['}[code[i]], 'Provisioning delimiters must balance');
      if (code[i] === '}' && i > open && stack.length === 0 && close === undefined) close = i;
    }
  }
  assert.equal(stack.length, 0, 'Provisioning delimiters must close');
  assert(close !== undefined, 'Hash mismatch branch must close');
  // Requiring the first statement to terminate rules out dead nested branches,
  // early returns, and a checksum throw moved after archive access.
  const body = code.slice(open + 1, close).trimStart();
  assert(/^(?:throw(?=\s|;|$)|exit[ \t]+1[ \t]*(?:[;\r\n]|$))/i.test(body),
    'Hash mismatch branch must unconditionally terminate with throw or exit 1');
  const archive = /\[\s*(?:System\.)?IO\.Compression\.ZipFile\s*\]\s*::\s*(?:OpenRead|Open|ExtractToDirectory)\s*\(|\bExpand-Archive\b/gi;
  const firstArchive = archive.exec(code);
  assert(firstArchive !== null, 'Provisioning must open the verified archive');
  assert(close < firstArchive.index, 'Hash mismatch must terminate before the first archive-opening call');
}

function assertPostgresProvisionContract(source) {
  assertPostgresChecksumTermination(source);
  for (const guard of [
    "$pin.major -ne 16", "$pin.sha256 -cnotmatch '^[a-f0-9]{64}$'",
    "Get-FileHash -LiteralPath $archivePath -Algorithm SHA256",
    "if ($actualDigest -cne $pin.sha256)",
    "'^pgsql/(bin|lib|share)/'", "@('bin', 'lib', 'share')",
    "@('initdb', 'pg_ctl', 'postgres', 'psql', 'createdb')",
    "New-Item -ItemType Directory -Path (Join-Path $proofRoot 'testtmp')",
    "$version -notmatch '\\(PostgreSQL\\) 16\\.[0-9]+'",
    "$LASTEXITCODE -ne 0", "$archive.Dispose()",
    "Remove-Item -LiteralPath $proofRoot -Recurse -Force",
    "throw 'PostgreSQL proof cleanup failed'",
  ]) assert(source.includes(guard), `PostgreSQL provisioning guard missing: ${guard}`);
  assert(!/WITSELF_LOCALCELL_PGBIN|GITHUB_ENV|Write-(?:Output|Host).*\$(?:pin|actualDigest)|register|Start-Service/.test(source),
    'Provisioning cannot activate lifecycle, persist environment, disclose digest, or install a service');
  assert(readFileSync(new URL('./test-powershell-static-analysis.ps1', import.meta.url), 'utf8')
    .includes("'scripts\\ci-postgres16-windows.ps1'"), 'Provisioning script must be analyzed');
}

const postgresProvision = readFileSync(new URL('./ci-postgres16-windows.ps1', import.meta.url), 'utf8');
const checksumThrow = "throw 'PostgreSQL proof archive checksum mismatch'";
for (const [name, replacement] of [
  ['comment out mismatch throw (M11)', `# ${checksumThrow}`],
  ['hide mismatch throw in block comment', `<# ${checksumThrow} #>`],
  ['hide mismatch throw in string', '"throw ignored"'],
  ['make mismatch throw unreachable', `if ($false) { ${checksumThrow} }`],
  ['return before mismatch throw', `return; ${checksumThrow}`],
  ['open archive before mismatch throw', `[IO.Compression.ZipFile]::OpenRead($archivePath); ${checksumThrow}`],
]) {
  test(`rejects structural checksum mutation: ${name}`, () => {
    assert(postgresProvision.includes(checksumThrow), 'Checksum mutation must target a real statement');
    assert.throws(() => assertPostgresChecksumTermination(postgresProvision.replace(checksumThrow, replacement)));
  });
}

test('checksum structural contract accepts terminating exit with non-code decoys removed', () => {
  assertPostgresChecksumTermination(postgresProvision.replace(checksumThrow,
    `# throw and [IO.Compression.ZipFile]::OpenRead are comments\n    <# { ignored } #>\n    exit 1`));
});

test('PowerShell mocked checksum mismatch terminates before archive access', t => {
  const archiveCall = '[IO.Compression.ZipFile]::OpenRead($archivePath)';
  assert(postgresProvision.includes(archiveCall), 'Mock must target the actual archive-opening expression');
  // Replace only the static archive call with an observable mock. All mismatch
  // control flow remains the real script; no download, files, or PG tools run.
  const source = postgresProvision.replace(archiveCall, '(Open-ProofArchive)');
  const harness = `
    $env:RUNNER_TEMP = [IO.Path]::GetTempPath()
    function Join-Path { param($Path, $ChildPath) 'proof-fixture' }
    function Get-Content {
      param($LiteralPath, [switch]$Raw)
      @{ major = 16; sha256 = ('a' * 64); url = 'https://get.enterprisedb.com/postgresql/postgresql-16.0-1-windows-x64-binaries.zip' } | ConvertTo-Json
    }
    function Test-Path { param($LiteralPath) $false }
    function New-Item { param($ItemType, $Path) }
    function Invoke-WebRequest {
      param([switch]$UseBasicParsing, $Uri, $OutFile)
      [Console]::Out.WriteLine('contract:download')
    }
    function Get-FileHash {
      param($LiteralPath, $Algorithm)
      [Console]::Out.WriteLine('contract:hash')
      [pscustomobject]@{ Hash = ('b' * 64) }
    }
    function Add-Type { param($AssemblyName) }
    function Open-ProofArchive {
      [Console]::Out.WriteLine('contract:archive')
      throw 'Archive must not open'
    }
    try {
      & ([scriptblock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${Buffer.from(source).toString('base64')}'))))
      exit 42
    } catch {
      if ($_.Exception.Message -eq 'PostgreSQL proof archive checksum mismatch') { exit 1 }
      exit 42
    }
  `;
  const result = spawnSync('pwsh', ['-NoLogo', '-NoProfile', '-NonInteractive', '-Command', harness], {
    encoding: 'utf8', timeout: 30_000,
  });
  if (result.error?.code === 'ENOENT') {
    t.skip('pwsh is unavailable; mocked PowerShell runtime contract requires PowerShell');
    return;
  }
  // Never include child output or values in diagnostics, even on a failing mock.
  assert(result.error === undefined, 'Mocked PowerShell contract must launch');
  assert(result.status === 1, 'Checksum mismatch must terminate the mocked PowerShell script');
  assert(result.stdout.replaceAll('\r\n', '\n').trim() === 'contract:download\ncontract:hash',
    'Mismatch must follow download and hashing without archive access');
});

test('Windows PostgreSQL provisioning verifies a complete exact vendor pin and runtime before use', () => {
  const pin = JSON.parse(readFileSync(new URL('./ci-postgres16-windows.json', import.meta.url), 'utf8'));
  assert.deepEqual(Object.keys(pin).sort(), ['major', 'sha256', 'url']);
  assert.equal(pin.major, 16);
  // Boolean assertions keep the digest out of test diagnostics, even on failure.
  assert(/^[a-f0-9]{64}$/.test(pin.sha256), 'Owner must fill the verified PostgreSQL archive digest before gating');
  assert(/^https:\/\/get\.enterprisedb\.com\/postgresql\/postgresql-16\.[0-9]+-[0-9]+-windows-x64-binaries\.zip$/.test(pin.url),
    'An exact PostgreSQL sixteen x64 EDB binary ZIP must be pinned');
  assertPostgresProvisionContract(postgresProvision);
});

for (const [name, guard] of [
  ['drop the digest check', 'if ($actualDigest -cne $pin.sha256)'],
  ['drop the PostgreSQL major check', "$pin.major -ne 16"],
  ['drop executable major preflight', "$version -notmatch '\\(PostgreSQL\\) 16\\.[0-9]+'"],
  ['ignore executable native exit', '$LASTEXITCODE -ne 0'],
  ['remove extracted tree cleanup', 'Remove-Item -LiteralPath $proofRoot -Recurse -Force'],
]) {
  test(`rejects PostgreSQL provisioning mutation: ${name}`, () => {
    assert(postgresProvision.includes(guard), 'Mutation must target a real guard');
    assert.throws(() => assertPostgresProvisionContract(postgresProvision.replace(guard, 'REMOVED')));
  });
}

// Evaluate only the small expression grammar used by deployment and concurrency.
// Never eval workflow source: mutation tests must not execute changed code.
function evaluateExpression(source, context) {
  const text = expression(source);
  const tokenPattern = /\s*(&&|\|\||==|!=|[()!]|'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_.-]*)/gy;
  const tokens = [];
  let offset = 0;
  while (offset < text.length) {
    tokenPattern.lastIndex = offset;
    const match = tokenPattern.exec(text);
    assert(match, `Unsupported condition syntax near ${text.slice(offset)}`);
    tokens.push(match[1]);
    offset = tokenPattern.lastIndex;
  }
  let cursor = 0;
  const take = expected => {
    assert.equal(tokens[cursor++], expected, `Expected condition token ${expected}`);
  };
  const value = () => {
    const token = tokens[cursor++];
    assert(token !== undefined, 'Incomplete deployment condition');
    if (token === '(') {
      const result = or();
      take(')');
      return result;
    }
    if (token === '!') return !value();
    if (token.startsWith("'")) return token.slice(1, -1).replaceAll("''", "'");
    if (token === 'true' || token === 'false') return token === 'true';
    assert.match(token, /^[A-Za-z_][A-Za-z0-9_.-]*$/);
    return context[token];
  };
  const equal = () => {
    let left = value();
    if (['==', '!='].includes(tokens[cursor])) {
      const operator = tokens[cursor++];
      const right = value();
      // All equality operands here are strings; GitHub compares them without
      // case. A separate executed shell guard enforces the exact launch value.
      const same = typeof left === 'string' && typeof right === 'string'
        ? left.toLowerCase() === right.toLowerCase() : left === right;
      left = operator === '==' ? same : !same;
    }
    return left;
  };
  const and = () => {
    let left = equal();
    while (tokens[cursor] === '&&') {
      cursor++;
      const right = equal();
      left = left && right;
    }
    return left;
  };
  const or = () => {
    let left = and();
    while (tokens[cursor] === '||') {
      cursor++;
      const right = and();
      left = left || right;
    }
    return left;
  };
  const result = or();
  assert.equal(cursor, tokens.length, 'Unconsumed deployment condition');
  return result;
}

function assertCIConcurrency(ci) {
  assert.equal(ci.concurrency['cancel-in-progress'], true, 'New PR updates must cancel superseded checks');
  const group = context => {
    const template = ci.concurrency.group.match(/^([^$]*)\$\{\{([\s\S]+)\}\}$/);
    assert(template, 'Concurrency group must contain a supported expression');
    return template[1] + evaluateExpression(template[2], context);
  };
  const main = {'github.event_name': 'push', 'github.ref': 'refs/heads/main'};
  const pr = {'github.event_name': 'pull_request', 'github.ref': 'refs/pull/42/merge'};
  const firstMain = group({...main, 'github.run_id': '1001'});
  const nextMain = group({...main, 'github.run_id': '1002'});
  assert.notEqual(firstMain, nextMain, 'A docs or brochure main push must not cancel or queue behind earlier application CI');
  const firstPR = group({...pr, 'github.run_id': '1003'});
  assert.equal(firstPR, group({...pr, 'github.run_id': '1004'}), 'Updates to one PR must retain their cancellation group');
  assert.notEqual(firstPR, group({...pr, 'github.ref': 'refs/pull/43/merge', 'github.run_id': '1005'}), 'Different PRs must run independently');
  assert.notEqual(firstMain, firstPR, 'Main pushes and PRs must run independently');
}

function assertWorkflowContracts(sources) {
  const ci = sources['ci.yml'];
  const reusable = sources['product-site-checks.yml'];
  const publication = sources['product-site.yml'];
  assert.deepEqual(Object.keys(ci.on).sort(), ['pull_request', 'push']);
  assert.deepEqual(ci.on.push, {branches: ['main']}, 'CI main push cannot have a path filter');
  assert.equal(ci.on.pull_request, null, 'CI PR cannot have a path filter');
  assert.deepEqual(ci.permissions, {contents: 'read'});
  assertCIConcurrency(ci);
  assertClassifier(ci.jobs.changes, ['application', 'docs', 'docs_index', 'docs_only', 'website', 'website_only'], 'ci changes');
  assertWebsiteCall(ci.jobs.website, 'ci website');
  assertDocsJob(ci.jobs.docs);
  assertLocalcellWindowsJob(ci);
  assertLocalcellUnixJob(ci);

  for (const [id, dependencies] of Object.entries(wrapperDependencies)) {
    const job = ci.jobs[id];
    assert(job, `Missing required wrapper: ${id}`);
    assert.equal(expression(job.if), 'always()', `${id}: required check must report despite failed or skipped dependencies`);
    needs(job, ['changes', 'website', 'docs', ...dependencies], id);
    assert.equal(job['continue-on-error'], undefined, `${id}: required gate cannot ignore failure`);
    const guard = job.steps.filter(step => step.run === 'node scripts/ci-product-gate.mjs');
    assert.equal(guard.length, 1, `${id}: strict helper runs exactly once`);
    assert.deepEqual(guard[0].env, {NEEDS_JSON: '${{ toJSON(needs) }}'}, `${id}: forward complete dependency results`);
    assert.equal(guard[0].if, undefined, `${id}: strict helper cannot be skipped`);
    assert.equal(guard[0]['continue-on-error'], undefined);
    assert.equal(action(job, 'actions/checkout@', id).with['persist-credentials'], false);
    assert.equal(action(job, 'actions/setup-node@', id).with['node-version'], 22);
    if (id === 'avatar-renderer-portability') {
      assert.equal(job.name, 'avatar-renderer-portability (${{ matrix.check }})');
      assert.equal(job['runs-on'], '${{ matrix.runner }}');
      assert.deepEqual(job.strategy.matrix.include, checkNames.map(check => ({check, runner: 'ubuntu-24.04'})));
    } else {
      assert.equal(job.name ?? id, id, `${id}: frozen required check name`);
      assert.equal(job['runs-on'], 'ubuntu-24.04');
    }
  }
  for (const id of fullJobs) {
    const job = ci.jobs[id];
    assert(job, `Missing application gate: ${id}`);
    assert.equal(job['continue-on-error'], undefined, `${id}: application failure remains blocking`);
    if (id === 'provider-contract-evidence') {
      needs(job, ['changes', 'provider-integration-contract'], id);
      assert.equal(expression(job.if), "always() && needs.changes.outputs.application == 'true'");
      const native = job.steps.find(step => step.name === 'Require every native provider job');
      assert.equal(native?.run, 'test "$NATIVE_RESULT" = success');
      assert.deepEqual(native.env, {NATIVE_RESULT: '${{ needs.provider-integration-contract.result }}'});
    } else {
      needs(job, ['changes'], id);
      assert.equal(expression(job.if), "needs.changes.outputs.application == 'true'", `${id}: positive application classification only`);
    }
  }
  assert.deepEqual(ci.jobs['provider-integration-contract'].strategy.matrix.include, [
    {target: 'linux-x64', runner: 'ubuntu-24.04'},
    {target: 'linux-arm64', runner: 'ubuntu-24.04-arm'},
    {target: 'macos-intel', runner: 'macos-15-intel'},
    {target: 'macos-arm64', runner: 'macos-15'},
    {target: 'windows-x64', runner: 'windows-latest'},
  ], 'Application provider matrix retains all native targets');
  assert.deepEqual(ci.jobs['avatar-renderer-portability-full'].strategy.matrix.include, [
    {check: 'ubuntu-latest', runner: 'ubuntu-24.04'},
    {check: 'ubuntu-24.04-arm', runner: 'ubuntu-24.04-arm'},
  ], 'Full renderer gates retain both architectures');

  assert.deepEqual(Object.keys(reusable.on), ['workflow_call']);
  assert.deepEqual(reusable.permissions, {contents: 'read'});
  assert.deepEqual(Object.keys(reusable.jobs), ['validate']);
  assert(!JSON.stringify(reusable).includes('secrets.'), 'Reusable validation never receives deployment secrets');
  assert.equal(reusable.jobs.validate.environment, undefined, 'Validation does not enter a deployment environment');
  const checks = reusable.jobs.validate.steps.filter(step => step.run === 'make check-product-site');
  assert.equal(checks.length, 1, 'Local and CI website checks share one command');
  assert.equal(checks[0].if, undefined);
  assert.equal(checks[0]['continue-on-error'], undefined);
  assert.equal(reusable.jobs.validate['continue-on-error'], undefined);

  assert.deepEqual(Object.keys(publication.on).sort(), ['push', 'workflow_dispatch']);
  assert.deepEqual(publication.on.push, {branches: ['main']}, 'Publication uses complete comparisons, not truncated workflow path filtering');
  assert.equal(publication.on.workflow_dispatch.inputs.publish.type, 'boolean');
  assert.equal(publication.on.workflow_dispatch.inputs.publish.default, false);
  assert.equal(publication.concurrency['cancel-in-progress'], false, 'Do not interrupt an in-flight deployment');
  assert.deepEqual(publication.permissions, {contents: 'read'});
  assertClassifier(publication.jobs.changes, ['website', 'website_only'], 'publication changes');
  assertWebsiteCall(publication.jobs.validate, 'publication validation');
  const deploy = publication.jobs.deploy;
  needs(deploy, ['changes', 'validate'], 'deployment');
  assert.equal(deploy.environment.name, 'product-site');
  assert.equal(deploy['continue-on-error'], undefined);
  for (const event of ['push', 'workflow_dispatch', 'pull_request', 'unknown']) {
    for (const ref of ['refs/heads/main', 'refs/heads/other']) {
      for (const enabled of ['true', 'false', '']) {
        for (const validation of ['success', 'failure', 'skipped', 'cancelled']) {
          for (const websiteOnly of ['true', 'false', '']) {
            for (const publish of [true, false]) {
              const context = {
                'github.event_name': event, 'github.ref': ref,
                'vars.WITSELF_PRODUCT_PUBLISH_ENABLED': enabled,
                'needs.validate.result': validation,
                'needs.changes.outputs.website_only': websiteOnly,
                'inputs.publish': publish,
              };
              const expected = ref === 'refs/heads/main' && enabled === 'true' && validation === 'success' &&
                ((event === 'push' && websiteOnly === 'true') || (event === 'workflow_dispatch' && publish));
              assert.equal(evaluateExpression(deploy.if, context), expected, `Deployment fence: ${JSON.stringify(context)}`);
            }
          }
        }
      }
    }
  }
  const launch = deploy.steps.find(step => step.name === 'Require the exact launch ref and hold value');
  assert(launch, 'Deployment must enforce exact launch settings in shell');
  assert.deepEqual(launch.env, {LAUNCH_ENABLED: '${{ vars.WITSELF_PRODUCT_PUBLISH_ENABLED }}'});
  assert.equal(launch.run.trim(), 'test "$GITHUB_REF" = refs/heads/main\ntest "$LAUNCH_ENABLED" = true');
  assert.equal(launch.if, undefined);
  const publishIndex = deploy.steps.findIndex(step => step.name === 'Deploy static assets to Cloudflare');
  const currentMainIndex = deploy.steps.findIndex(step => step.name === 'Require the current main commit before publication');
  assert.equal(currentMainIndex, publishIndex - 1, 'Reject superseded queued main commits immediately before deployment');
  const currentMain = deploy.steps[currentMainIndex];
  assert(currentMain.run.includes('gh api "repos/$GITHUB_REPOSITORY/git/ref/heads/main" --jq .object.sha'));
  assert(currentMain.run.includes('test "$current_main" = "$GITHUB_SHA"'));
  assert.equal(currentMain.if, undefined);
  assert.equal(currentMain['continue-on-error'], undefined);
}

test('documentation and website lanes preserve required checks, application gates, and deployment boundaries', () => {
  assertWorkflowContracts(workflows);
});

test('main push runs remain independent while newer updates cancel only their own PR checks', () => {
  assertCIConcurrency(workflows['ci.yml']);
});

test('native Windows failures block go while brochure-only and docs-only changes skip Windows', () => {
  const job = workflows['ci.yml'].jobs['localcell-windows'];
  for (const [path, application] of [
    ['web/product/content.mjs', false],
    ['docs/research/local-cell/implementation-plan.md', false],
    ['.github/workflows/ci.yml', true],
    ['scripts/ci-go-test-json-check.mjs', true],
  ]) {
    const scope = classifyChanges([{
      path, status: 'M', oldMode: '100644', newMode: '100644',
      oldOID: 'a'.repeat(40), newOID: 'b'.repeat(40),
    }]);
    assert.equal(scope.application, application, path);
    assert.equal(evaluateExpression(job.if, {'needs.changes.outputs.application': String(scope.application)}), application, path);
    const outputs = Object.fromEntries(Object.entries(scope).filter(([key]) => key !== 'reason').map(([key, value]) => [key, String(value)]));
    const results = {
      changes: {result: 'success', outputs},
      docs: {result: scope.docs ? 'success' : 'skipped'},
      website: {result: scope.website ? 'success' : 'skipped'},
      ...Object.fromEntries(wrapperDependencies.go.map(id => [id, {result: application ? 'success' : 'skipped'}])),
    };
    assert.doesNotThrow(() => assertGate(results), path);
    for (const result of application ? ['failure', 'cancelled', 'skipped', undefined] : ['success', 'failure']) {
      assert.throws(() => assertGate({...results, 'localcell-windows': {result}}), `${path}: Windows ${result}`);
    }
  }
});

test('native Unix failures block go while brochure-only and docs-only changes skip Unix', () => {
  const job = workflows['ci.yml'].jobs['localcell-unix'];
  for (const [path, application] of [
    ['web/product/content.mjs', false],
    ['docs/research/local-cell/implementation-plan.md', false],
    ['.github/workflows/ci.yml', true],
    ['scripts/ci-postgres16-unix.sh', true],
    ['scripts/ci-postgres16-unix.json', true],
  ]) {
    const scope = classifyChanges([{
      path, status: 'M', oldMode: '100644', newMode: '100644',
      oldOID: 'a'.repeat(40), newOID: 'b'.repeat(40),
    }]);
    assert.equal(scope.application, application, path);
    assert.equal(evaluateExpression(job.if, {'needs.changes.outputs.application': String(scope.application)}), application, path);
    const outputs = Object.fromEntries(Object.entries(scope).filter(([key]) => key !== 'reason').map(([key, value]) => [key, String(value)]));
    const results = {
      changes: {result: 'success', outputs},
      docs: {result: scope.docs ? 'success' : 'skipped'},
      website: {result: scope.website ? 'success' : 'skipped'},
      ...Object.fromEntries(wrapperDependencies.go.map(id => [id, {result: application ? 'success' : 'skipped'}])),
    };
    assert.doesNotThrow(() => assertGate(results), path);
    for (const result of application ? ['failure', 'cancelled', 'skipped', undefined] : ['success', 'failure']) {
      assert.throws(() => assertGate({...results, 'localcell-unix': {result}}), `${path}: Unix ${result}`);
    }
  }
});

test('classified docs, mixed, and checker changes preserve every required wrapper and publication fence', () => {
  const research = 'docs/research/local-cell/notes.md';
  const cases = [
    {paths: [research], application: false, docs: true, docs_index: false, docs_only: true, website: false, website_only: false},
    {paths: ['docs/README.md'], application: false, docs: true, docs_index: true, docs_only: true, website: false, website_only: false},
    {paths: [research, 'web/product/content.mjs'], application: true, docs: true, docs_index: false, docs_only: false, website: true, website_only: false},
    {paths: [research, 'internal/store/transcripts.go'], application: true, docs: true, docs_index: false, docs_only: false, website: false, website_only: false},
    {paths: ['web/product/content.mjs'], application: false, docs: false, docs_index: false, docs_only: false, website: true, website_only: true},
    ...['scripts/check-docs.mjs', 'scripts/run-docs-check.mjs'].map(path => ({
      paths: [path], application: true, docs: false, docs_index: false, docs_only: false, website: false, website_only: false,
    })),
    {paths: ['docs/README.md', '.github/workflows/ci.yml'], application: true, docs: true, docs_index: true, docs_only: false, website: true, website_only: false},
  ];
  for (const {paths, ...expected} of cases) {
    const {reason, ...scope} = classifyChanges(paths.map(path => ({
      path, status: 'M', oldMode: '100644', newMode: '100644',
      oldOID: 'a'.repeat(40), newOID: 'b'.repeat(40),
    })));
    assert.deepEqual(scope, expected, paths.join(', '));
    const outputs = Object.fromEntries(Object.entries(scope).map(([key, flag]) => [key, String(flag)]));
    const context = Object.fromEntries(Object.entries(outputs).map(([key, flag]) => [`needs.changes.outputs.${key}`, flag]));
    assert.equal(evaluateExpression(workflows['ci.yml'].jobs.docs.if, context), scope.docs);
    assert.equal(evaluateExpression(workflows['ci.yml'].jobs.website.if, context), scope.website);
    for (const id of fullJobs) {
      assert.equal(evaluateExpression(expression(workflows['ci.yml'].jobs[id].if).replace(/^always\(\) && /, ''), context), scope.application, `${paths}: ${id}`);
    }
    for (const [id, dependencies] of Object.entries(wrapperDependencies)) {
      const results = {
        changes: {result: 'success', outputs},
        docs: {result: scope.docs ? 'success' : 'skipped'},
        website: {result: scope.website ? 'success' : 'skipped'},
        ...Object.fromEntries(dependencies.map(name => [name, {result: scope.application ? 'success' : 'skipped'}])),
      };
      assert.equal(assertGate(results), scope.docs_only ? 'docs-only' : scope.website_only ? 'brochure-only' : 'application', `${paths}: ${id}`);
      for (const status of ['failure', 'cancelled', undefined]) {
        assert.throws(() => assertGate({...results, docs: {result: status}}), `${paths}: ${id} cannot hide documentation ${status}`);
      }
      assert.throws(() => {
        const missing = {...results};
        delete missing.docs;
        assertGate(missing);
      }, `${paths}: ${id} cannot omit documentation result`);
    }
    assert.equal(evaluateExpression(workflows['product-site.yml'].jobs.deploy.if, {
      ...context,
      'github.event_name': 'push', 'github.ref': 'refs/heads/main',
      'vars.WITSELF_PRODUCT_PUBLISH_ENABLED': 'true',
      'needs.validate.result': 'success', 'inputs.publish': false,
    }), scope.website_only, `${paths}: automatic publication remains brochure-only`);
  }
});

test('exact launch shell guard rejects disabled, case-changed, and non-main settings', () => {
  const guard = workflows['product-site.yml'].jobs.deploy.steps.find(step => step.name === 'Require the exact launch ref and hold value');
  for (const [ref, enabled, expected] of [
    ['refs/heads/main', 'true', 0],
    ['refs/heads/main', 'TRUE', 1],
    ['refs/heads/main', 'false', 1],
    ['refs/heads/other', 'true', 1],
  ]) {
    const result = spawnSync('bash', ['-e', '-c', guard.run], {env: {...process.env, GITHUB_REF: ref, LAUNCH_ENABLED: enabled}});
    assert.equal(result.status, expected, `${ref}: ${enabled}`);
  }
});

const mutations = [
  ['missing Unix lifecycle job', data => { delete data['ci.yml'].jobs['localcell-unix']; }],
  ['missing Unix dependency from go', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'localcell-unix'); }],
  ['Unix matrix row removed', data => { data['ci.yml'].jobs['localcell-unix'].strategy.matrix.include.pop(); }],
  ['Unix architecture runs on the wrong runner', data => { data['ci.yml'].jobs['localcell-unix'].strategy.matrix.include[1].runner = 'macos-15-intel'; }],
  ['Unix matrix cancels remaining native rows', data => { data['ci.yml'].jobs['localcell-unix'].strategy['fail-fast'] = true; }],
  ['Unix job always runs', data => { data['ci.yml'].jobs['localcell-unix'].if = 'always()'; }],
  ...[
    ['runs-on', 'ubuntu-24.04'], ['needs', []], ['permissions', {contents: 'write'}],
    ['timeout-minutes', 26], ['continue-on-error', true], ['env', {WITSELF_LOCALCELL_PGBIN: 'synthetic'}],
  ].map(([key, value]) => [`Unix ${key} boundary changed`, data => { data['ci.yml'].jobs['localcell-unix'][key] = value; }]),
  ...[
    'Build Local Cell packages', 'Vet Local Cell packages', 'Test Go JSON result checker',
    'Test Local Cell packages and require complete results', 'Provision pinned PostgreSQL sixteen',
    'Prove native PostgreSQL sixteen lifecycle', 'Clean PostgreSQL proof tree',
  ].map(name => [`Unix stage removed: ${name}`, data => {
    const job = data['ci.yml'].jobs['localcell-unix'];
    job.steps = job.steps.filter(step => step.name !== name);
  }]),
  ['Unix proof runs before provisioning', data => {
    const steps = data['ci.yml'].jobs['localcell-unix'].steps;
    [steps[7], steps[8]] = [steps[8], steps[7]];
  }],
  ['Unix proof runs after failed provisioning', data => { data['ci.yml'].jobs['localcell-unix'].steps[8].if = 'always()'; }],
  ['Unix provisioning ignores failure', data => { data['ci.yml'].jobs['localcell-unix'].steps[7]['continue-on-error'] = true; }],
  ['Unix cleanup only runs after success', data => { delete data['ci.yml'].jobs['localcell-unix'].steps.at(-1).if; }],
  ['Unix unit loses native pipe exit', data => { data['ci.yml'].jobs['localcell-unix'].steps[6].run = data['ci.yml'].jobs['localcell-unix'].steps[6].run.replace('test_exits[0] != 0 || ', ''); }],
  ['Unix unit loses checker pipe exit', data => { data['ci.yml'].jobs['localcell-unix'].steps[6].run = data['ci.yml'].jobs['localcell-unix'].steps[6].run.replace(' || test_exits[1] != 0', ''); }],
  ...[
    ["'^TestRealLifecycle$'", "'TestRealLifecycle'", 'broadens the run filter'],
    [',TestRealLifecycle/WaitsForReadiness', '', 'omits a required child'],
    [' --diagnose', '', 'omits diagnostics'],
    [' --input "$proof_json"', '', 'does not read captured JSON'],
    [' > "$proof_json" 2>&1', '', 'prints raw Go output'],
    ['lc-pg16/proof.json', 'proof.json', 'leaves JSON outside cleanup tree'],
    [' || go_exit=$?', '', 'stops before checking Go failure'],
    [' || parser_exit=$?', '', 'loses checker exit capture'],
    ['go_exit != 0 || ', '', 'masks native exit'],
    [' || parser_exit != 0', '', 'masks checker exit'],
  ].map(([before, after, name]) => [`Unix proof ${name}`, data => {
    const step = data['ci.yml'].jobs['localcell-unix'].steps[8];
    step.run = step.run.replace(before, after);
  }]),
  ['Unix proof temporary files escape cleanup tree', data => { data['ci.yml'].jobs['localcell-unix'].steps[8].env.TMPDIR = '${{ runner.temp }}'; }],
  ['Windows proof step removed', data => { const job = data['ci.yml'].jobs['localcell-windows']; job.steps = job.steps.filter(step => step.name !== 'Prove native PostgreSQL sixteen lifecycle'); }],
  ['Windows proof broadens the run filter', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace("'^TestRealLifecycle$'", "'TestRealLifecycle'"); }],
  ['Windows proof makes a skip acceptable', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(/ --require [^\n]+/, ''); }],
  ['Windows proof omits one required child', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(',TestRealLifecycle/WaitsForReadiness', ''); }],
  ['Windows proof ignores native exit', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace('$goExit -ne 0 -or ', ''); }],
  ['Windows proof ignores parser exit', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(' -or $parserExit -ne 0', ''); }],
  ['Windows proof omits bounded diagnostics', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(' --diagnose', ''); }],
  ['Windows proof reads stdin instead of its file', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(' --input $proofJSON', ''); }],
  ['Windows proof leaks output instead of capturing its file', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace(' > $proofJSON 2>&1', ''); }],
  ['Windows proof leaves its file outside the cleanup tree', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace('lc-pg16\\proof.json', 'proof.json'); }],
  ['Windows proof loses the separate native exit capture', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Prove native PostgreSQL sixteen lifecycle'); step.run = step.run.replace('$goExit = $LASTEXITCODE', '$goExit = 0'); }],
  ['Windows unit step enables proof diagnostics', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results'); step.run += ' --diagnose'; }],
  ['Windows proof cleanup removed', data => { data['ci.yml'].jobs['localcell-windows'].steps.pop(); }],
  ['Windows proof cleanup only on success', data => { delete data['ci.yml'].jobs['localcell-windows'].steps.at(-1).if; }],
  ['Windows PostgreSQL is enabled on the unit step', data => { data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results').env = {WITSELF_LOCALCELL_PGBIN: 'synthetic'}; }],
  ['Windows proof uploads artifacts', data => { data['ci.yml'].jobs['localcell-windows'].steps.push({uses: 'actions/upload-artifact@' + 'a'.repeat(40)}); }],
  ['Windows inherits PostgreSQL lifecycle configuration', data => { data['ci.yml'].env = {WITSELF_LOCALCELL_PGBIN: 'synthetic'}; }],
  ['missing Windows unit job', data => { delete data['ci.yml'].jobs['localcell-windows']; }],
  ['missing Windows dependency from go', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'localcell-windows'); }],
  ['Windows job failure ignored', data => { data['ci.yml'].jobs['localcell-windows']['continue-on-error'] = true; }],
  ['Windows job always runs', data => { data['ci.yml'].jobs['localcell-windows'].if = 'always()'; }],
  ['Windows parser step removed', data => { const job = data['ci.yml'].jobs['localcell-windows']; job.steps = job.steps.filter(step => step.name !== 'Test Local Cell packages and require complete results'); }],
  ['Windows parser bypassed', data => { data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results').run = 'go test ./internal/localcell'; }],
  ['Windows test failure ignored', data => { data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results')['continue-on-error'] = true; }],
  ['Windows native Go exit masked', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results'); step.run = step.run.replace('$goExit -ne 0 -or ', ''); }],
  ['Windows parser exit masked', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.find(step => step.name === 'Test Local Cell packages and require complete results'); step.run = step.run.replace(' -or $parserExit -ne 0', ''); }],
  ['Windows parser unit tests removed', data => { const job = data['ci.yml'].jobs['localcell-windows']; job.steps = job.steps.filter(step => step.run !== 'node --test scripts/ci-go-test-json-check.test.mjs'); }],
  ...[
    ['runs-on', 'ubuntu-24.04'], ['needs', []], ['permissions', {contents: 'write'}],
    ['timeout-minutes', 26], ['environment', 'production'], ['env', {WITSELF_LOCALCELL_PGBIN: 'synthetic'}],
  ].map(([key, value]) => [`Windows ${key} boundary changed`, data => { data['ci.yml'].jobs['localcell-windows'][key] = value; }]),
  ['main pushes share a cancellation or queue group', data => { data['ci.yml'].concurrency.group = 'ci-${{ github.ref }}'; }],
  ['concurrency cancellation disabled', data => { data['ci.yml'].concurrency['cancel-in-progress'] = false; }],
  ['missing Go application dependency', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'go-store'); }],
  ['missing required website dependency', data => { data['ci.yml'].jobs.helm.needs = data['ci.yml'].jobs.helm.needs.filter(id => id !== 'website'); }],
  ...Object.keys(wrapperDependencies).map(id => [
    `missing required documentation dependency from ${id}`,
    data => { data['ci.yml'].jobs[id].needs = data['ci.yml'].jobs[id].needs.filter(dependency => dependency !== 'docs'); },
  ]),
  ['missing static analysis dependency', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'static-analysis'); }],
  ['wrong docs output forwarding', data => { data['ci.yml'].jobs.changes.outputs.docs = '${{ steps.scope.outputs.website }}'; }],
  ['missing docs-only output', data => { delete data['ci.yml'].jobs.changes.outputs.docs_only; }],
  ['wrong docs index output forwarding', data => { data['ci.yml'].jobs.changes.outputs.docs_index = '${{ steps.scope.outputs.docs }}'; }],
  ['mixed documentation validation skipped', data => { data['ci.yml'].jobs.docs.if = "needs.changes.outputs.docs_only == 'true'"; }],
  ['different local and CI documentation gate', data => { data['ci.yml'].jobs.docs.steps.at(-1).run = 'echo passed'; }],
  ['documentation failure ignored', data => { data['ci.yml'].jobs.docs.steps.at(-1)['continue-on-error'] = true; }],
  ['documentation check skipped', data => { data['ci.yml'].jobs.docs.steps.at(-1).if = 'false'; }],
  ['documentation job failure ignored', data => { data['ci.yml'].jobs.docs['continue-on-error'] = true; }],
  ['research-only docs installs Go', data => { delete data['ci.yml'].jobs.docs.steps.find(step => step.uses?.startsWith('actions/setup-go@')).if; }],
  ['documentation index guard loses Go setup', data => { data['ci.yml'].jobs.docs.steps = data['ci.yml'].jobs.docs.steps.filter(step => !step.uses?.startsWith('actions/setup-go@')); }],
  ['documentation starts a database', data => { data['ci.yml'].jobs.docs.services = {postgres: {image: 'postgres'}}; }],
  ['skipped required wrapper', data => { data['ci.yml'].jobs.go.if = "needs.changes.outputs.application == 'true'"; }],
  ['lost dependency result forwarding', data => { data['ci.yml'].jobs.go.steps.at(-1).env.NEEDS_JSON = '{}'; }],
  ['ignored required gate failure', data => { data['ci.yml'].jobs.helm.steps.at(-1)['continue-on-error'] = true; }],
  ['broad application skip', data => { data['ci.yml'].jobs['go-store'].if = "github.event_name == 'push'"; }],
  ['provider failure hidden by success condition', data => { data['ci.yml'].jobs['provider-contract-evidence'].if = "success() && needs.changes.outputs.application == 'true'"; }],
  ['provider native target removed', data => { data['ci.yml'].jobs['provider-integration-contract'].strategy.matrix.include.pop(); }],
  ['renderer ARM runner lost', data => { data['ci.yml'].jobs['avatar-renderer-portability-full'].strategy.matrix.include[1].runner = 'ubuntu-24.04'; }],
  ['CI workflow path filtering', data => { data['ci.yml'].on.push.paths = ['web/product/**']; }],
  ['shallow classifier history', data => { data['ci.yml'].jobs.changes.steps[0].with['fetch-depth'] = 1; }],
  ['classifier errors ignored', data => { data['ci.yml'].jobs.changes.steps.at(-1)['continue-on-error'] = true; }],
  ['documentation checker regressions omitted', data => { data['ci.yml'].jobs.changes.steps.find(step => step.run === testCommand).run = testCommand.replace(' scripts/check-docs.test.mjs', ''); }],
  ['reusable validation credential', data => { data['product-site-checks.yml'].jobs.validate.env = {TOKEN: '${{ secrets.CLOUDFLARE_API_TOKEN }}'}; }],
  ['different local and CI website gate', data => { data['product-site-checks.yml'].jobs.validate.steps.at(-1).run = 'echo passed'; }],
  ['publication path filtering', data => { data['product-site.yml'].on.push.paths = ['web/product/**']; }],
  ['publication ignores failed validation', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace("needs.validate.result == 'success' &&", ''); }],
  ['publication allows mixed main pushes', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace(" && needs.changes.outputs.website_only == 'true'", ''); }],
  ['publication allows manual validation-only dispatch', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace(' && inputs.publish', ''); }],
  ['publication loses main restriction', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace("github.ref == 'refs/heads/main' &&", ''); }],
  ['publication loses validation dependency', data => { data['product-site.yml'].jobs.deploy.needs = ['changes']; }],
  ['publication accepts stale queued commit', data => { data['product-site.yml'].jobs.deploy.steps = data['product-site.yml'].jobs.deploy.steps.filter(step => step.name !== 'Require the current main commit before publication'); }],
];

for (const [name, mutate] of mutations) {
  test(`rejects mutation: ${name}`, () => {
    const changed = structuredClone(workflows);
    mutate(changed);
    assert.notDeepEqual(changed, workflows, 'Mutation must change its intended target');
    assert.throws(() => assertWorkflowContracts(changed));
  });
}
