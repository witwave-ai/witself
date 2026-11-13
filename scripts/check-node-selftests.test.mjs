import assert from 'node:assert/strict';
import {execFileSync, spawnSync} from 'node:child_process';
import {chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';
import {loadWorkflows, main, packageRoutes, selectNodeSelfTests} from './check-node-selftests.mjs';

const root = fileURLToPath(new URL('../', import.meta.url));
const source = path => readFileSync(join(root, path), 'utf8').replace(/\r\n/g, '\n');
const anyFile = {isFile: () => true};
const step = (run, extra = {}) => ({run, ...extra});
const workflow = (steps, extra = {}) => ({...extra, jobs: {job: {steps}}});
const rootWorkflow = {'root.yml': workflow([step('node --test scripts/a.test.mjs')])};

function recipe(makefile, name) {
  const lines = makefile.split('\n');
  const starts = lines.flatMap((line, index) => line.startsWith(`${name}:`) ? [index] : []);
  assert.equal(starts.length, 1, `${name} target must exist exactly once`);
  const result = [];
  for (const line of lines.slice(starts[0] + 1)) {
    if (line && !/^[\t #]/.test(line)) break;
    if (line.startsWith('\t')) result.push(line.slice(1));
  }
  return result;
}

function selectRepository() {
  return selectNodeSelfTests(loadWorkflows(root), {isFile: file => existsSync(join(root, file))});
}

test('the workflows select every repository-root node --test file ci.yml names, including the stream checker self-test', () => {
  const {files} = selectRepository();
  // Independent of the selector's YAML walk: every single-line step in ci.yml.
  const named = [...source('.github/workflows/ci.yml').matchAll(/^ +run: node --test (.+)$/gm)]
    .flatMap(match => match[1].trim().split(' '));
  assert(named.length > 0, 'ci.yml must name Node self-tests');
  for (const file of named) assert(files.includes(file), `${file} is named by ci.yml but not selected`);
  // PR 977: four CI jobs ran this checker self-test and the local gate never did.
  assert(files.includes('scripts/ci-go-test-json-check.test.mjs'));
});

test('workflow YAML is read through block scalars, folded scalars, and inherited working directories', t => {
  const directory = mkdtempSync(join(tmpdir(), 'witself-node-selftests-yaml-'));
  t.after(() => rmSync(directory, {recursive: true, force: true}));
  const workflows = join(directory, '.github', 'workflows');
  mkdirSync(workflows, {recursive: true});
  assert.throws(() => loadWorkflows(directory), /no workflows found/);
  writeFileSync(join(workflows, 'a.yml'), [
    'name: fixture', 'on:', '  pull_request:', 'jobs:', '  root:', '    runs-on: ubuntu-24.04', '    steps:',
    '      - name: single', '        run: node --test scripts/one.test.mjs',
    '      - name: block', '        run: |', '          go test ./x', '          node --test scripts/two.test.mjs scripts/one.test.mjs',
    '      - name: folded', '        run: >-', '          node --test', '          scripts/three.test.mjs',
    '  package:', '    runs-on: ubuntu-24.04', '    defaults:', '      run:', '        working-directory: infra/cloudflare/agent-email',
    '    steps:', '      - run: node --test test/probe.test.mjs', '',
  ].join('\n'));
  writeFileSync(join(workflows, 'b.yaml'), 'on: push\njobs:\n  b:\n    steps:\n      - run: node --test scripts/four.test.mjs\n');
  writeFileSync(join(workflows, 'notes.txt'), 'node --test scripts/ignored.test.mjs\n');
  const {files, packageSteps} = selectNodeSelfTests(loadWorkflows(directory), anyFile);
  assert.deepEqual(files, ['scripts/four.test.mjs', 'scripts/one.test.mjs', 'scripts/three.test.mjs', 'scripts/two.test.mjs']);
  assert.deepEqual(packageSteps.map(({directory: path, files: named}) => [path, named]),
    [['infra/cloudflare/agent-email', ['test/probe.test.mjs']]]);
  writeFileSync(join(workflows, 'c.yml'), 'base: &base\n  runs-on: ubuntu-24.04\njobs:\n  c: *base\n');
  assert.throws(() => loadWorkflows(directory), /could not read \.github\/workflows/);
});

test('repository-root commands are selected once each from single-line and multi-line steps', () => {
  const {files, packageSteps} = selectNodeSelfTests({
    'b.yml': workflow([step('node --test scripts/a.test.mjs'), step('npm test')]),
    'a.yml': workflow([
      step('node --test scripts/b.test.mjs scripts/a.test.mjs', {shell: 'pwsh'}),
      step('go test ./x\r\nnode --test web/c.test.mjs\r\n', {'working-directory': './'}),
      step('node --test infra/d.test.cjs', {'working-directory': '.'}),
      {uses: 'actions/checkout@v7'},
    ]),
    'c.yml': {jobs: {called: {uses: './.github/workflows/b.yml'}}},
  }, anyFile);
  assert.deepEqual(files, ['infra/d.test.cjs', 'scripts/a.test.mjs', 'scripts/b.test.mjs', 'web/c.test.mjs']);
  assert.deepEqual(packageSteps, []);
});

test('package-scoped commands need a named local route and never run from the repository root', () => {
  const agentEmail = 'infra/cloudflare/agent-email';
  for (const [scope, workflows] of [
    ['step', {'w.yml': workflow([step('node --test test/x.test.mjs', {'working-directory': agentEmail})])}],
    ['job', {'w.yml': {jobs: {job: {defaults: {run: {'working-directory': `${agentEmail}/`}}, steps: [step('node --test test/x.test.mjs')]}}}}],
    ['workflow', {'w.yml': workflow([step('node --test test/x.test.mjs')], {defaults: {run: {'working-directory': `./${agentEmail}`}}})}],
  ]) {
    const {files, packageSteps} = selectNodeSelfTests({...rootWorkflow, ...workflows}, anyFile);
    assert.deepEqual(files, ['scripts/a.test.mjs'], scope);
    assert.deepEqual(packageSteps.map(({directory, files: named, route}) => [directory, named, route]),
      [[agentEmail, ['test/x.test.mjs'], packageRoutes[agentEmail]]], scope);
  }
  // A step's own directory wins over the job default, and the discovery form is a package form only.
  const routed = selectNodeSelfTests({...rootWorkflow, 'w.yml': {jobs: {job: {
    defaults: {run: {'working-directory': 'infra/cloudflare/unrouted'}},
    steps: [step('node --test', {'working-directory': 'scripts/dashboard-acceptance'})],
  }}}}, anyFile);
  assert.deepEqual(routed.packageSteps.map(({directory, files}) => [directory, files]), [['scripts/dashboard-acceptance', []]]);
  for (const directory of ['infra/cloudflare/control-plane', 'scripts', '${{ github.workspace }}']) {
    assert.throws(() => selectNodeSelfTests({...rootWorkflow,
      'w.yml': workflow([step('node --test test/x.test.mjs', {'working-directory': directory})])}, anyFile),
    error => error.message.includes(`working directory ${directory} has no local route`), directory);
  }
});

test('every package route names a local target that runs that package suite', () => {
  const makefile = source('Makefile');
  assert.deepEqual(Object.keys(packageRoutes).sort(), ['infra/cloudflare/agent-email', 'scripts/dashboard-acceptance']);
  assert(recipe(makefile, 'check-infra').includes('npm --prefix infra/cloudflare/agent-email test'));
  const agentEmailTest = JSON.parse(source('infra/cloudflare/agent-email/package.json')).scripts.test;
  assert.match(agentEmailTest, /^node --test test\/\*\.test\.mjs(?: |$)/);
  assert(recipe(makefile, 'dashboard-acceptance').some(line => line.includes('(cd scripts/dashboard-acceptance && node --test &&')));
  const {packageSteps} = selectRepository();
  assert(packageSteps.length > 0, 'the repository workflows have package-scoped Node steps');
  for (const {label, directory, files} of packageSteps) {
    // The package glob test/*.test.mjs must cover every file a workflow step names there.
    if (directory === 'infra/cloudflare/agent-email') {
      assert(files.length > 0, label);
      for (const file of files) assert.match(file, /^test\/[A-Za-z0-9_.-]+\.test\.mjs$/, label);
    } else {
      assert.equal(directory, 'scripts/dashboard-acceptance', label);
      assert.deepEqual(files, [], `${label}: make dashboard-acceptance runs the directory's discovery form`);
    }
  }
});

test('commands the runner cannot reproduce exactly fail closed', () => {
  for (const run of [
    'cd scripts && node --test a.test.mjs',
    'node --test scripts/a.test.mjs && echo done',
    'node --test scripts/*.test.mjs',
    "node --test 'scripts/a.test.mjs'",
    'node --test "$TEST_FILE"',
    'node --test $TEST_FILE',
    'node --test --test-name-pattern=fixture scripts/a.test.mjs',
    'node --test-reporter=spec scripts/a.test.mjs',
    'node --test ../a.test.mjs',
    'node --test ./scripts/a.test.mjs',
    'node --test /tmp/a.test.mjs',
    'node --test scripts\\a.test.mjs',
    'node --test scripts/a.test.mjs  scripts/b.test.mjs',
    'node  --test scripts/a.test.mjs',
    'node --test scripts/a.test.mjs \\',
    'node --test scripts/README.md',
    '# node --test scripts/a.test.mjs',
  ]) {
    assert.throws(() => selectNodeSelfTests({'w.yml': workflow([step(run)])}, anyFile), /unsupported node --test/, run);
  }
  assert.throws(() => selectNodeSelfTests({'w.yml': workflow([step('node --test')])}, anyFile), /names no test files/);
});

test('a missing test file or an empty selection fails', () => {
  assert.throws(() => selectNodeSelfTests({'w.yml': workflow([step('node --test scripts/gone.test.mjs')])}, {isFile: () => false}),
    /scripts\/gone\.test\.mjs is not a regular file/);
  for (const workflows of [{}, {'w.yml': workflow([step('npm test'), step('node scripts/run.mjs')])}, {'w.yml': {}}]) {
    assert.throws(() => selectNodeSelfTests(workflows, anyFile), /no repository-root node --test files found/);
  }
});

test('the runner hands the selected files to one node --test process and propagates its result', () => {
  const workflows = {'w.yml': workflow([step('node --test scripts/b.test.mjs scripts/a.test.mjs')])};
  const options = {root: '/repo', load: path => (assert.equal(path, '/repo'), workflows), isFile: () => true};
  for (const [outcome, status, verdict] of [
    [{status: 0, signal: null}, 0, 'check-node-selftests: PASS'],
    [{status: 1, signal: null}, 1, 'check-node-selftests: FAIL node --test status=1'],
    [{status: null, signal: 'SIGTERM'}, 1, 'check-node-selftests: FAIL node --test signal=SIGTERM'],
  ]) {
    const calls = [];
    const lines = [];
    assert.equal(main({...options, argv: [], log: line => lines.push(line), spawn: (...args) => (calls.push(args), outcome)}), status);
    assert.deepEqual(calls.map(([file, args, {cwd, stdio}]) => [file, args, cwd, stdio]),
      [[process.execPath, ['--test', 'scripts/a.test.mjs', 'scripts/b.test.mjs'], '/repo', 'inherit']]);
    assert.equal(lines.at(-1), verdict);
  }
  const failure = new Error('spawn failed');
  assert.throws(() => main({...options, argv: [], log: () => {}, spawn: () => ({error: failure})}), failure);
  const listed = [];
  assert.equal(main({...options, argv: ['--list'], log: line => listed.push(line), spawn: () => assert.fail('--list runs nothing')}), 0);
  assert.deepEqual(listed, ['scripts/a.test.mjs', 'scripts/b.test.mjs']);
  for (const argv of [['--bogus'], ['--list', '--list'], ['scripts/a.test.mjs']]) {
    assert.throws(() => main({...options, argv, spawn: () => assert.fail('usage errors run nothing')}), /usage:/);
  }
});

test('make reaches the workflow Node self-tests only through check-node-selftests', () => {
  const makefile = source('Makefile');
  assert.match(makefile, /^check-node-selftests:.*## .+/m, 'target must have help text');
  assert(makefile.split('\n').some(line => line.startsWith('.PHONY:') && line.split(/\s+/).includes('check-node-selftests')),
    'target must be phony');
  assert.deepEqual(recipe(makefile, 'check-node-selftests'), ['bash scripts/check-node-selftests.sh']);
  assert.equal(recipe(makefile, 'check-infra').filter(line => line === '$(MAKE) check-node-selftests').length, 1);
  assert.equal(recipe(makefile, 'check').filter(line => line === '$(MAKE) check-infra').length, 1);
  // A hand-written copy of the workflow list in either gate recipe would drift again.
  for (const target of ['check', 'check-infra']) {
    assert.deepEqual(recipe(makefile, target).filter(line => /\bnode\s+--test\b/.test(line)), [], target);
  }
});

test('the wrapper reports NOT RUN without node and otherwise proves the selector before running it',
  process.platform === 'win32' ? {skip: 'the local gate wrapper runs on Unix hosts'} : {}, t => {
    const directory = mkdtempSync(join(tmpdir(), 'witself-node-selftests-wrapper-'));
    t.after(() => rmSync(directory, {recursive: true, force: true}));
    const bash = execFileSync('bash', ['-c', 'printf %s "$BASH"'], {encoding: 'utf8'});
    const dirnameTool = execFileSync('bash', ['-c', 'command -v dirname'], {encoding: 'utf8'}).trim();
    const wrapper = join(root, 'scripts', 'check-node-selftests.sh');
    const run = env => {
      const result = spawnSync(bash, [wrapper], {cwd: directory, env, encoding: 'utf8'});
      assert.ifError(result.error);
      return result;
    };
    const noNode = join(directory, 'no-node');
    mkdirSync(noNode);
    symlinkSync(dirnameTool, join(noNode, 'dirname'));
    for (const [requireNode, status] of [[undefined, 0], ['0', 0], ['true', 0], ['1', 1]]) {
      const result = run({PATH: noNode, ...(requireNode === undefined ? {} : {WITSELF_TEST_REQUIRE_NODE: requireNode})});
      assert.deepEqual({status: result.status, stdout: result.stdout, stderr: result.stderr},
        {status, stdout: '', stderr: 'NOT RUN check-node-selftests: node missing\n'}, `WITSELF_TEST_REQUIRE_NODE=${requireNode}`);
    }

    const fakeNode = join(directory, 'fake-node');
    mkdirSync(fakeNode);
    symlinkSync(dirnameTool, join(fakeNode, 'dirname'));
    writeFileSync(join(fakeNode, 'node'),
      `#!${bash}\nprintf '%s|%s\\n' "$(pwd -P)" "$*" >>"$NODE_CALLS"\n[[ $* != "\${NODE_FAIL_ON:-}" ]] || exit 7\n`);
    chmodSync(join(fakeNode, 'node'), 0o755);
    const repository = realpathSync(root);
    const selfTest = `${repository}|--test scripts/check-node-selftests.test.mjs`;
    const runner = `${repository}|scripts/check-node-selftests.mjs`;
    for (const [failOn, status, calls] of [
      [undefined, 0, [selfTest, runner]],
      ['--test scripts/check-node-selftests.test.mjs', 7, [selfTest]],
      ['scripts/check-node-selftests.mjs', 7, [selfTest, runner]],
    ]) {
      const record = join(directory, `calls-${calls.length}-${status}`);
      const result = run({PATH: fakeNode, NODE_CALLS: record, WITSELF_TEST_REQUIRE_NODE: '1',
        ...(failOn === undefined ? {} : {NODE_FAIL_ON: failOn})});
      assert.equal(result.status, status, `fail on ${failOn}: ${result.stderr}`);
      assert.equal(result.stderr, '');
      assert.deepEqual(readFileSync(record, 'utf8').trimEnd().split('\n'), calls, `fail on ${failOn}`);
    }
  });
