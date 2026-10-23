import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {execFileSync} from 'node:child_process';
import {runDocsChecks, captureLocalDocsState, docsScope, checkDocsWhitespace, main} from './run-docs-check.mjs';

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

test('document approval snapshot changes with content, index, untracked files, HEAD and base', () => {
  const cwd = mkdtempSync(join(tmpdir(), 'docs-state-'));
  const git = (...args) => execFileSync('git', args, {cwd, stdio: ['ignore', 'pipe', 'pipe']});
  try {
    git('init', '-q'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid');
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
  } finally { rmSync(cwd, {recursive: true, force: true}); }
});

test('forced local scope checks index changes despite inherited unrelated GitHub event data', () => {
  const cwd = mkdtempSync(join(tmpdir(), 'docs-local-context-'));
  const git = (...args) => execFileSync('git', args, {cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe']}).trim();
  try {
    git('init', '-q'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid');
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
  } finally { rmSync(cwd, {recursive: true, force: true}); }
});
