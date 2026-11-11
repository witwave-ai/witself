import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync, rmSync, symlinkSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join, resolve} from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {execFileSync, spawnSync} from 'node:child_process';
import {runDocsChecks, captureLocalDocsState, docsScope, checkDocsWhitespace, main} from './run-docs-check.mjs';
import {cleanupGitFixture, configureFixtureGit, fixtureGitArgs} from './test-support/git-fixture.mjs';

function fixture(index = false, overrides = {}) {
  const calls = [], logs = [];
  return {calls, logs, options: {
    cwd: '/fixture', env: {MARKER: 'kept'}, classify: () => ({docs_index: index}),
    validate: () => ({passed: true}), whitespace: () => {}, run: (...args) => calls.push(args), log: value => logs.push(value),
    ...overrides,
  }};
}

test('research-only validation requires no Go process', () => {
  const f = fixture();
  assert.deepEqual(runDocsChecks(f.options), {passed: true, index_checked: false});
  assert.deepEqual(f.calls, []);
});

test('index edits invoke the existing authoritative contract with no database setup', () => {
  const f = fixture(true);
  assert.deepEqual(runDocsChecks(f.options), {passed: true, index_checked: true});
  assert.deepEqual(f.calls, [['go', ['test', './featurestatus', '-run', '^TestRunbookDocsIndexes$', '-count=1'], {cwd: '/fixture', env: {MARKER: 'kept'}}]]);
});

test('missing scope, failed content, and index failures never emit a passing verdict', () => {
  for (const overrides of [
    {classify: () => ({})}, {classify: () => ({docs_index: 'false'})},
    {validate: () => ({passed: false})}, {validate: () => { throw new Error('broken link'); }},
    {whitespace: () => { throw new Error('trailing whitespace'); }},
    {run: () => { throw new Error('index missing required link'); }},
  ]) {
    const f = fixture(true, overrides);
    assert.throws(() => runDocsChecks(f.options));
    assert.equal(f.logs.length, 0);
  }
  assert.throws(() => main(['--skip-index']));
  assert.throws(() => docsScope('/fixture', {GITHUB_ACTIONS: 'true'}), /comparison context/);
});

test('document approval snapshot changes with content, index, untracked files, HEAD and base', t => {
  const cwd = mkdtempSync(join(tmpdir(), 'docs-state-'));
  t.after(() => cleanupGitFixture(cwd, t.name));
  const git = (...args) => execFileSync('git', fixtureGitArgs(args), {cwd, stdio: ['ignore', 'pipe', 'pipe']});
  git('init', '-q');
  configureFixtureGit(git);
  git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid');
  mkdirSync(join(cwd, 'docs/research/local-cell'), {recursive: true});
  writeFileSync(join(cwd, 'docs/README.md'), '# Index\n');
  const file = join(cwd, 'docs/research/local-cell/README.md');
  writeFileSync(file, '# Research\n'); git('add', '.'); git('commit', '-qm', 'base'); git('update-ref', 'refs/remotes/origin/main', 'HEAD');
  const original = captureLocalDocsState(cwd);
  assert.doesNotThrow(() => checkDocsWhitespace(cwd, {}));
  assert.equal(captureLocalDocsState(cwd), original);
  writeFileSync(file, '# Changed\n'); assert.notEqual(captureLocalDocsState(cwd), original);
  git('add', '.'); const staged = captureLocalDocsState(cwd); assert.notEqual(staged, original);
  writeFileSync(file, '# Research\n'); assert.notEqual(captureLocalDocsState(cwd), original);
  git('reset', '--hard', 'HEAD'); assert.equal(captureLocalDocsState(cwd), original);
  const extra = join(cwd, 'docs/research/local-cell/new.md');
  writeFileSync(extra, '# New\n'); const untracked = captureLocalDocsState(cwd); assert.notEqual(untracked, original);
  assert.doesNotThrow(() => checkDocsWhitespace(cwd, {}));
  writeFileSync(extra, '# Trailing whitespace \n'); assert.throws(() => checkDocsWhitespace(cwd, {}));
  writeFileSync(extra, '# Different\n'); assert.notEqual(captureLocalDocsState(cwd), untracked);
  git('add', '.'); git('commit', '-qm', 'addition'); const next = captureLocalDocsState(cwd); assert.notEqual(next, original);
  git('update-ref', 'refs/remotes/origin/main', 'HEAD'); assert.notEqual(captureLocalDocsState(cwd), next);
});

test('forced local scope checks index changes despite inherited unrelated GitHub event data', t => {
  const cwd = mkdtempSync(join(tmpdir(), 'docs-local-context-'));
  t.after(() => cleanupGitFixture(cwd, t.name));
  const git = (...args) => execFileSync('git', fixtureGitArgs(args), {cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe']}).trim();
  git('init', '-q');
  configureFixtureGit(git);
  git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid');
  mkdirSync(join(cwd, 'docs/research/local-cell'), {recursive: true});
  writeFileSync(join(cwd, 'docs/README.md'), '# Index\n');
  const research = join(cwd, 'docs/research/local-cell/README.md');
  writeFileSync(research, '# Research\n'); git('add', '.'); git('commit', '-qm', 'base');
  git('update-ref', 'refs/remotes/origin/main', 'HEAD');
  const before = git('rev-parse', 'HEAD');
  writeFileSync(research, '# Changed research\n'); git('add', '.'); git('commit', '-qm', 'research');
  const after = git('rev-parse', 'HEAD');
  writeFileSync(join(cwd, 'docs/README.md'), '# Changed index\n');
  const eventPath = join(cwd, '.git', 'event.json');
  writeFileSync(eventPath, JSON.stringify({before, after}));
  const inherited = {GITHUB_ACTIONS: 'true', GITHUB_EVENT_NAME: 'push', GITHUB_EVENT_PATH: eventPath};
  assert.equal(docsScope(cwd, inherited).docs_index, false, 'Event alone has no index edit');
  const env = {...inherited, GITHUB_ACTIONS: 'false'};
  assert.equal(docsScope(cwd, env).docs_index, true, 'Local comparison includes the uncommitted index edit');
  const f = fixture(false, {cwd, env, classify: docsScope});
  assert.deepEqual(runDocsChecks(f.options), {passed: true, index_checked: true});
  assert.deepEqual(f.calls, [['go', ['test', './featurestatus', '-run', '^TestRunbookDocsIndexes$', '-count=1'], {cwd, env}]]);
});

const script = fileURLToPath(new URL('./run-docs-check.mjs', import.meta.url));
const repository = resolve(dirname(script), '..');

function assertMainProbe(path, cwd) {
  // Arguments are rejected before document checks, Git commands, or child gates.
  const result = spawnSync(process.execPath, [path, '--invalid'], {cwd, encoding: 'utf8', timeout: 10000});
  assert.ifError(result.error);
  assert.equal(result.signal, null);
  return {status: result.status, stdout: result.stdout, stderr: result.stderr};
}

function assertLinkedMain(linked, cwd) {
  const expected = assertMainProbe(script, cwd);
  assert.notDeepEqual(expected, {status: 0, stdout: '', stderr: ''});
  assert.deepEqual(assertMainProbe(linked, cwd), expected);
}

test('main guard runs through a linked directory', t => {
  const directory = mkdtempSync(join(tmpdir(), 'main-guard-directory-'));
  t.after(() => rmSync(directory, {recursive: true, force: true}));
  const linked = join(directory, 'checkout');
  symlinkSync(repository, linked, 'junction');
  assertLinkedMain(join(linked, 'scripts', 'run-docs-check.mjs'), directory);
});

test('main guard runs through a linked file', {
  skip: process.platform === 'win32' ? 'file symbolic links need privilege on Windows' : false,
}, t => {
  const directory = mkdtempSync(join(tmpdir(), 'main-guard-file-'));
  t.after(() => rmSync(directory, {recursive: true, force: true}));
  const linked = join(directory, 'renamed-tool.mjs');
  symlinkSync(script, linked);
  assertLinkedMain(linked, directory);
});

test('main guard keeps imports inert with absent, nonexistent, or another entry file', t => {
  const directory = mkdtempSync(join(tmpdir(), 'main-guard-import-'));
  t.after(() => rmSync(directory, {recursive: true, force: true}));
  for (const entry of [undefined, join(directory, 'missing.mjs'), fileURLToPath(import.meta.url)]) {
    const source = `process.argv[1] = ${JSON.stringify(entry)};
      process.argv[2] = '--invalid';
      await import(${JSON.stringify(pathToFileURL(script).href)});
      console.log('imported');`;
    const result = spawnSync(process.execPath, ['--input-type=module', '--eval', source], {
      cwd: directory, encoding: 'utf8', timeout: 10000,
    });
    assert.ifError(result.error);
    assert.equal(result.signal, null);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, 'imported\n');
    assert.equal(result.stderr, '');
  }
});
