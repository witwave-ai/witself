import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {brochureFiles, classifyEvent, classifyLocal, classifyPaths, parsePaths} from './ci-product-scope.mjs';

const script = fileURLToPath(new URL('./ci-product-scope.mjs', import.meta.url));
const site = {application: false, website: true, website_only: true};
const full = {application: true, website: true, website_only: false};
const application = {application: true, website: false, website_only: false};
function scope(actual, expected) {
  assert.deepEqual(Object.fromEntries(Object.keys(expected).map(key => [key, actual[key]])), expected);
}
function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'witself-product-scope-'));
  t.after(() => rmSync(cwd, {recursive: true, force: true}));
  const git = (...args) => execFileSync('git', args, {
    cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    env: {...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null',
      GIT_AUTHOR_NAME: 'Scope Test', GIT_AUTHOR_EMAIL: 'scope@example.invalid',
      GIT_COMMITTER_NAME: 'Scope Test', GIT_COMMITTER_EMAIL: 'scope@example.invalid'},
  }).trim();
  const write = (path, content = 'fixture\n') => {
    mkdirSync(dirname(join(cwd, path)), {recursive: true});
    writeFileSync(join(cwd, path), content);
  };
  const commit = message => {
    git('add', '--all');
    git('-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', message);
    return git('rev-parse', 'HEAD');
  };
  git('init', '--quiet', '--initial-branch=main');
  write('web/product/content.mjs', 'base brochure\n');
  write('web/product/styles.css', 'base styles\n');
  write('cmd/witself/main.go', 'base application\n');
  const base = commit('base');
  return {cwd, git, write, commit, base};
}
const push = (before, after) => ({before, after});
const pull = (base, head) => ({pull_request: {base: {sha: base}, head: {sha: head}}});

test('only the explicit brochure allowlist bypasses application checks', () => {
  assert.deepEqual([...brochureFiles], [
    'web/product/README.md', 'web/product/DISCOVERY.md', 'web/product/content.mjs',
    'web/product/build.mjs', 'web/product/styles.css', 'web/product/app.js',
    'web/product/favicon.svg', 'web/product/assets/screenshots/agent-console.png',
    'web/product/assets/screenshots/agent-tui.png', 'web/product/assets/screenshots/manifest.json',
  ]);
  for (const path of brochureFiles) scope(classifyPaths([path]), site);
  scope(classifyPaths([...brochureFiles]), site);
  scope(classifyPaths(['cmd/witself/main.go']), application);
  scope(classifyPaths(['web/product/content.mjs', 'cmd/witself/main.go']), full);
  for (const path of [
    'web/plans/plans.json', 'web/product/wrangler.toml',
    'web/product/check.mjs', 'web/product/check.test.mjs',
    'web/product/screenshots/capture.mjs', 'web/product/screenshots/demo/main.go',
    'web/product/assets/screenshots/unknown.png', 'web/product/new-file.md',
    '.github/workflows/ci.yml', '.github/workflows/product-site.yml',
    'infra/cloudflare/control-plane/package-lock.json', 'infra/cloudflare/wrangler-version.env',
    'scripts/ci-product-scope.mjs', 'scripts/ci-product-gate.test.mjs', 'CLAUDE.md', 'Makefile',
  ]) scope(classifyPaths([path]), full);
  for (const path of ['go.mod', 'go.sum', 'README.md', 'scripts/dashboard-acceptance/package.json']) {
    scope(classifyPaths([path]), application);
  }
});

test('missing or malformed paths fail closed', () => {
  for (const paths of [undefined, null, {}, 'web/product/content.mjs', [], [''], [null], [1],
    ['/web/product/content.mjs'], ['./web/product/content.mjs'], ['web//product/content.mjs'],
    ['web/product/../product/content.mjs'], ['web/product/content.mjs\0'], ['web/product/\ufffd']]) {
    scope(classifyPaths(paths), full);
  }
});

test('NUL path parsing preserves whitespace and rejects truncation or invalid UTF-8', () => {
  assert.deepEqual(parsePaths(Buffer.from('web/product/content.mjs\0odd file\nname.go\0')), [
    'web/product/content.mjs', 'odd file\nname.go',
  ]);
  assert.deepEqual(parsePaths(Buffer.alloc(0)), []);
  scope(classifyPaths(parsePaths(Buffer.from('web/product/content.mjs\0odd file\nname.go\0'))), full);
  assert.throws(() => parsePaths(Buffer.from('web/product/content.mjs')));
  assert.throws(() => parsePaths(Buffer.from([0xff, 0])));
  scope(classifyPaths(parsePaths(Buffer.from('web/product/content.mjs\0\0'))), full);
});

test('PR comparison uses its merge base when main has moved', t => {
  const f = fixture(t);
  f.git('switch', '--quiet', '-c', 'brochure');
  f.write('web/product/content.mjs', 'new brochure\n');
  const head = f.commit('brochure');
  f.git('switch', '--quiet', 'main');
  f.write('cmd/witself/main.go', 'unrelated application change on main\n');
  const movedBase = f.commit('main advances');
  scope(classifyEvent('pull_request', pull(movedBase, head), f.cwd), site);
});

test('push comparison covers every commit in the range', t => {
  const f = fixture(t);
  f.write('cmd/witself/main.go', 'application changed first\n');
  f.commit('application');
  f.write('web/product/content.mjs', 'brochure changed last\n');
  const head = f.commit('brochure');
  scope(classifyEvent('push', push(f.base, head), f.cwd), full);
});

test('complete git comparison sees more than 300 paths and newline filenames', t => {
  const f = fixture(t);
  for (let index = 0; index < 350; index++) f.write(`web/product/generated/file-${index}.md`);
  f.write('zz-application\nwith-space .go');
  f.write('web/product/content.mjs', 'brochure\n');
  const head = f.commit('many files');
  scope(classifyEvent('push', push(f.base, head), f.cwd), full);
});

test('renaming into or out of the allowlist includes both source and destination', t => {
  for (const intoBrochure of [true, false]) {
    const f = fixture(t);
    if (intoBrochure) {
      f.git('rm', 'web/product/content.mjs');
      f.git('mv', 'cmd/witself/main.go', 'web/product/content.mjs');
    } else {
      f.git('mv', 'web/product/content.mjs', 'cmd/witself/copied.go');
    }
    const head = f.commit('rename across boundary');
    scope(classifyEvent('push', push(f.base, head), f.cwd), full);
    scope(classifyEvent('pull_request', pull(f.base, head), f.cwd), full);
  }
});

test('missing, invalid, unavailable, zero, and nonancestor event SHAs fail closed', t => {
  const f = fixture(t);
  f.write('web/product/content.mjs', 'first branch\n');
  const first = f.commit('first');
  f.git('switch', '--quiet', '-c', 'other', f.base);
  f.write('web/product/styles.css', 'other branch\n');
  const other = f.commit('other');
  scope(classifyEvent('push', push(first, other), f.cwd), full);
  for (const invalid of [undefined, null, '', 'main', '--help', '0'.repeat(40), 'f'.repeat(40), 42, 'a'.repeat(39)]) {
    scope(classifyEvent('push', push(invalid, other), f.cwd), full);
    scope(classifyEvent('push', push(f.base, invalid), f.cwd), full);
    scope(classifyEvent('pull_request', pull(invalid, other), f.cwd), full);
    scope(classifyEvent('pull_request', pull(f.base, invalid), f.cwd), full);
  }
  for (const event of [undefined, null, {}, {pull_request: {}}, {before: f.base}]) {
    scope(classifyEvent('push', event, f.cwd), full);
    scope(classifyEvent('pull_request', event, f.cwd), full);
  }
  for (const eventName of [undefined, 'workflow_dispatch', 'schedule', 'pull_request_target']) {
    scope(classifyEvent(eventName, push(f.base, other), f.cwd), full);
  }
  scope(classifyEvent('push', push(f.base, f.base), f.cwd), full);
});

test('local scope includes committed, staged, unstaged, and untracked changes', t => {
  for (const source of ['committed', 'staged', 'unstaged', 'untracked']) {
    const f = fixture(t);
    f.write('web/product/content.mjs', 'brochure commit\n');
    f.commit('brochure');
    scope(classifyLocal(f.base, f.cwd), site);
    const path = source === 'untracked' ? 'cmd/witself/new.go' : 'cmd/witself/main.go';
    f.write(path, 'application change\n');
    if (source === 'committed') f.commit('application');
    if (source === 'staged') f.git('add', path);
    scope(classifyLocal(f.base, f.cwd), full);
  }
});

test('uncommitted reversals cannot hide committed or staged application changes from a local push check', t => {
  for (const source of ['committed', 'staged']) {
    const f = fixture(t);
    f.write('web/product/content.mjs', 'brochure change\n');
    f.commit('brochure commit');
    f.write('cmd/witself/main.go', 'application change being pushed\n');
    if (source === 'committed') f.commit('mixed commit');
    else f.git('add', 'cmd/witself/main.go');
    f.write('cmd/witself/main.go', 'base application\n');
    scope(classifyLocal(f.base, f.cwd), full);
  }
});

test('local scope uses merge base and rejects unknown comparisons', t => {
  const f = fixture(t);
  f.git('switch', '--quiet', '-c', 'brochure');
  f.write('web/product/content.mjs', 'new brochure\n');
  f.commit('brochure');
  f.git('switch', '--quiet', 'main');
  f.write('cmd/witself/main.go', 'new application on main\n');
  f.commit('main advances');
  f.git('switch', '--quiet', 'brochure');
  scope(classifyLocal('main', f.cwd), site);
  for (const ref of [undefined, '', '--help', 'nonexistent']) scope(classifyLocal(ref, f.cwd), full);
});

test('CLI writes explicit outputs and fails closed for missing or malformed event files', t => {
  const f = fixture(t);
  f.write('web/product/content.mjs', 'updated\n');
  const head = f.commit('brochure');
  const eventPath = join(f.cwd, '.git', 'event.json');
  const outputPath = join(f.cwd, '.git', 'outputs');
  const run = () => {
    writeFileSync(outputPath, '');
    const stdout = execFileSync(process.execPath, [script], {cwd: f.cwd, encoding: 'utf8', env: {
      ...process.env, GITHUB_EVENT_NAME: 'push', GITHUB_EVENT_PATH: eventPath, GITHUB_OUTPUT: outputPath,
    }});
    const result = JSON.parse(stdout);
    assert.equal(readFileSync(outputPath, 'utf8'),
      `application=${result.application}\nwebsite=${result.website}\nwebsite_only=${result.website_only}\n`);
    return result;
  };
  scope(run(), full);
  writeFileSync(eventPath, '{ invalid JSON');
  scope(run(), full);
  writeFileSync(eventPath, JSON.stringify(push(f.base, head)));
  scope(run(), site);
  assert.throws(() => execFileSync(process.execPath, [script, '--unknown'], {
    cwd: f.cwd, stdio: ['ignore', 'pipe', 'pipe'],
  }));
});
