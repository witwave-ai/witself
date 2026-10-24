import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {chmodSync, existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {brochureFiles, classifyEvent, classifyLocal, classifyPaths, classifyChanges, parseChanges, parsePaths} from './ci-product-scope.mjs';
import {captureDocsSnapshot, collectDocsInputs, isDocsPath} from './docs-scope.mjs';
import {cleanupGitFixture, configureFixtureGit, fixtureGitArgs} from './test-support/git-fixture.mjs';

const script = fileURLToPath(new URL('./ci-product-scope.mjs', import.meta.url));
const site = {application: false, website: true, website_only: true, docs: false, docs_only: false, docs_index: false};
const full = {application: true, website: true, website_only: false, docs: true, docs_only: false, docs_index: true};
const application = {application: true, website: false, website_only: false, docs: false, docs_only: false, docs_index: false};
const mixedWebsite = {...application, website: true};
const docs = {...application, application: false, docs: true, docs_only: true};
const docsIndex = {...docs, docs_index: true};
const mixedDocs = {...application, docs: true};
const docsPath = 'docs/research/local-cell/notes.md';
function scope(actual, expected) {
  assert.deepEqual(Object.fromEntries(Object.keys(expected).map(key => [key, actual[key]])), expected);
}
function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'witself-product-scope-'));
  t.after(() => cleanupGitFixture(cwd, t.name));
  const git = (...args) => execFileSync('git', fixtureGitArgs(args), {
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
  configureFixtureGit(git);
  write('web/product/content.mjs', 'base brochure\n');
  write('web/product/styles.css', 'base styles\n');
  write('cmd/witself/main.go', 'base application\n');
  write('docs/README.md', '# Docs\n');
  write(docsPath, '# Research\n');
  const base = commit('base');
  return {cwd, git, write, commit, base};
}
const push = (before, after) => ({before, after});
const pull = (base, head) => ({pull_request: {base: {sha: base}, head: {sha: head}}});

test('Git fixtures disable background maintenance for direct and indirect commands', t => {
  const f = fixture(t);
  assert.deepEqual(fixtureGitArgs(['status']), ['-c', 'gc.auto=0', '-c', 'maintenance.auto=false', 'status']);
  for (const [key, value] of [['gc.auto', '0'], ['maintenance.auto', 'false']]) {
    assert.equal(f.git('config', '--get', key), value);
    // --local checks persisted settings, independent of the command overrides.
    assert.equal(execFileSync('git', fixtureGitArgs(['config', '--local', '--get', key]), {
      cwd: f.cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    }).trim(), value);
  }
});

test('Git fixture cleanup removes a late-created .git file on its bounded retry', t => {
  const f = fixture(t);
  let attempts = 0;
  let pauses = 0;
  cleanupGitFixture(f.cwd, t.name, {
    remove(dir, options) {
      assert.deepEqual(options, {recursive: true, force: true, maxRetries: 10, retryDelay: 100});
      attempts++;
      if (attempts === 1) {
        rmSync(join(dir, '.git'), options);
        throw Object.assign(new Error('fixture removal interrupted'), {code: 'ENOTEMPTY'});
      }
      assert.equal(readFileSync(join(dir, '.git', 'late-file'), 'utf8'), 'late fixture write');
      rmSync(dir, options);
    },
    pause(milliseconds) {
      assert.equal(milliseconds, 100);
      pauses++;
      mkdirSync(join(f.cwd, '.git'));
      writeFileSync(join(f.cwd, '.git', 'late-file'), 'late fixture write');
    },
  });
  assert.equal(attempts, 2);
  assert.equal(pauses, 1);
  assert.equal(existsSync(f.cwd), false);
});

test('Git fixture cleanup retries a residual directory and reports a persistent leak by test name', t => {
  const f = fixture(t);
  for (const throws of [false, true]) {
    let attempts = 0;
    let pauses = 0;
    assert.throws(() => cleanupGitFixture(f.cwd, t.name, {
      remove() {
        attempts++;
        if (throws) throw new Error(`private filesystem details: ${f.cwd}`);
      },
      pause(milliseconds) { assert.equal(milliseconds, 100); pauses++; },
    }), {message: `Temporary Git fixture cleanup failed for test: ${t.name}`});
    assert.equal(attempts, 2);
    assert.equal(pauses, 1);
  }
});

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
  scope(classifyPaths(['web/product/content.mjs', 'cmd/witself/main.go']), mixedWebsite);
  for (const path of [
    'web/plans/plans.json', 'web/product/wrangler.toml',
    'web/product/check.mjs', 'web/product/check.test.mjs',
    'web/product/screenshots/capture.mjs', 'web/product/screenshots/demo/main.go',
    'web/product/assets/screenshots/unknown.png', 'web/product/new-file.md',
    '.github/workflows/ci.yml', '.github/workflows/product-site.yml',
    'infra/cloudflare/control-plane/package-lock.json', 'infra/cloudflare/wrangler-version.env',
    'scripts/ci-product-scope.mjs', 'scripts/ci-product-gate.test.mjs', 'CLAUDE.md', 'Makefile',
  ]) scope(classifyPaths([path]), mixedWebsite);
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
  assert.deepEqual(parsePaths(Buffer.from('\ufeffdocs/research/local-cell/notes.md\0')), ['\ufeffdocs/research/local-cell/notes.md']);
  scope(classifyPaths(parsePaths(Buffer.from('web/product/content.mjs\0odd file\nname.go\0'))), mixedWebsite);
  assert.throws(() => parsePaths(Buffer.from('web/product/content.mjs')));
  assert.throws(() => parsePaths(Buffer.from([0xff, 0])));
  assert.throws(() => parsePaths(Buffer.from('web/product/content.mjs\0\0')));
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
  scope(classifyEvent('push', push(f.base, head), f.cwd), mixedWebsite);
});

test('complete git comparison sees more than 300 paths and newline filenames', t => {
  const f = fixture(t);
  for (let index = 0; index < 350; index++) f.write(`web/product/generated/file-${index}.md`);
  f.write('zz-application\nwith-space .go');
  f.write('web/product/content.mjs', 'brochure\n');
  const head = f.commit('many files');
  scope(classifyEvent('push', push(f.base, head), f.cwd), mixedWebsite);
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
    scope(classifyEvent('push', push(f.base, head), f.cwd), mixedWebsite);
    scope(classifyEvent('pull_request', pull(f.base, head), f.cwd), mixedWebsite);
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
    scope(classifyLocal(f.base, f.cwd), mixedWebsite);
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
    scope(classifyLocal(f.base, f.cwd), mixedWebsite);
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
      `application=${result.application}\nwebsite=${result.website}\nwebsite_only=${result.website_only}\ndocs=${result.docs}\ndocs_only=${result.docs_only}\ndocs_index=${result.docs_index}\n`);
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

test('docs allowlist is direct-child Markdown and the exact docs index', () => {
  for (const path of [docsPath, 'docs/research/local-cell/README.md', 'docs/README.md']) {
    assert.equal(isDocsPath(path), true);
    // Bare paths cannot establish the regular-file/status contract.
    scope(classifyPaths([path]), {...mixedDocs, docs_index: path === 'docs/README.md'});
  }
  for (const path of ['docs/other.md', 'README.md', 'docs/research/notes.md',
    'docs/research/local-cell/nested/notes.md', 'docs/research/local-cell/.hidden.md',
    'docs/research/local-cell/notes.MD', 'docs/research/local-cell/notes.json',
    'docs/research/local-cell/../notes.md', 'docs/research/local-cell//notes.md',
    'docs/research/local-cell\\notes.md', '/docs/README.md', './docs/README.md',
    'scripts/check-docs.mjs', 'scripts/run-docs-check.mjs', 'scripts/docs-scope.mjs']) {
    assert.equal(isDocsPath(path), false, path);
    assert.equal(classifyPaths([path]).docs_only, false, path);
  }
});

test('raw NUL parser requires complete canonical records and both modes', () => {
  const oid = 'a'.repeat(40);
  const raw = `:100644 100644 ${oid} ${oid} M\0${docsPath}\0`;
  const record = {path: docsPath, oldMode: '100644', newMode: '100644', oldOID: oid, newOID: oid, status: 'M'};
  assert.deepEqual(parseChanges(Buffer.from(raw)), [record]);
  scope(classifyChanges([record]), docs);
  assert.deepEqual(parseChanges(Buffer.alloc(0)), []);
  scope(classifyChanges([]), full);
  for (const malformed of [raw.slice(0, -1), raw + '\0', raw.replace(' M\0', ' R100\0'),
    raw.replace(' M\0', ' X\0'), raw.replace(':100644', ':100600'), raw.replace(oid, 'a'.repeat(7)),
    raw.replace(docsPath, '../notes.md'), raw.replace(' M\0', ' A\0'),
    raw.replace(docsPath, ''), `:100644 100644 ${oid} ${oid} M\0`]) {
    assert.throws(() => parseChanges(Buffer.from(malformed)), malformed);
  }
  assert.throws(() => parseChanges(Buffer.from([0xff, 0])));
  for (const invalid of [undefined, null, {}, ['docs/README.md'], [{...record, path: '../notes.md'}],
    [{...record, newMode: '100600'}], [{...record, status: 'R100'}]]) {
    scope(classifyChanges(invalid), full);
  }
});

test('docs add and modify qualify for PR, push, and every local comparison layer', t => {
  for (const path of [docsPath, 'docs/research/local-cell/new.md', 'docs/README.md']) {
    const expected = path === 'docs/README.md' ? docsIndex : docs;
    const f = fixture(t);
    f.write(path, '# Updated\n');
    scope(classifyLocal(f.base, f.cwd), expected);
    f.git('add', path);
    scope(classifyLocal(f.base, f.cwd), expected);
    const head = f.commit('docs');
    scope(classifyLocal(f.base, f.cwd), expected);
    scope(classifyEvent('push', push(f.base, head), f.cwd), expected);
    scope(classifyEvent('pull_request', pull(f.base, head), f.cwd), expected);
  }
});

test('docs plus brochure or runtime changes always require application checks', t => {
  for (const path of ['web/product/content.mjs', 'cmd/witself/main.go', 'docs/other.md',
    'docs/research/local-cell/nested/new.md', 'docs/research/local-cell/.hidden.md', 'scripts/run-docs-check.mjs']) {
    const f = fixture(t);
    f.write(docsPath, '# Updated\n');
    f.write(path, 'other change\n');
    const expected = {...mixedDocs, website: path.startsWith('web/product/')};
    scope(classifyLocal(f.base, f.cwd), expected);
    const head = f.commit('mixed');
    scope(classifyEvent('push', push(f.base, head), f.cwd), expected);
  }
});

test('docs PR and local comparisons use the merge base when main advances', t => {
  const f = fixture(t);
  f.git('switch', '--quiet', '-c', 'research');
  f.write(docsPath, '# Branch research\n');
  const head = f.commit('research');
  f.git('switch', '--quiet', 'main');
  f.write('cmd/witself/main.go', 'unrelated main update\n');
  f.write('docs/README.md', '# Main index update\n');
  const movedBase = f.commit('main moves');
  f.git('switch', '--quiet', 'research');
  scope(classifyEvent('pull_request', pull(movedBase, head), f.cwd), docs);
  scope(classifyLocal('main', f.cwd), docs);
});

test('docs event grants bind to head checkout or the exact declared PR test merge', t => {
  const f = fixture(t);
  f.git('switch', '--quiet', '-c', 'research');
  f.write(docsPath, '# Branch research\n');
  const head = f.commit('research');
  scope(classifyEvent('push', push(f.base, head), f.cwd), docs);
  f.git('switch', '--quiet', 'main');
  // A comparison may be available while the working checkout is unrelated.
  scope(classifyEvent('push', push(f.base, head), f.cwd), full);
  scope(classifyEvent('pull_request', pull(f.base, head), f.cwd), full);
  f.write('cmd/witself/main.go', 'main advances\n');
  const target = f.commit('new base');
  f.git('-c', 'commit.gpgsign=false', 'merge', '--quiet', '--no-ff', 'research', '-m', 'synthetic test merge');
  const merge = f.git('rev-parse', 'HEAD');
  const event = pull(target, head);
  event.pull_request.merge_commit_sha = merge;
  scope(classifyEvent('pull_request', event, f.cwd), docs);
  scope(classifyEvent('pull_request', pull(target, head), f.cwd), full);
  scope(classifyEvent('pull_request', {...event, pull_request: {...event.pull_request, merge_commit_sha: target}}, f.cwd), full);
  scope(classifyEvent('pull_request', {...event, pull_request: {...event.pull_request, base: {sha: f.base}}}, f.cwd), full);
  // A valid merge object is not sufficient if a different commit is checked out.
  f.write('cmd/witself/main.go', 'later runtime commit\n');
  f.commit('later');
  scope(classifyEvent('pull_request', event, f.cwd), full);
});

test('docs comparison includes more than 300 paths and a final runtime change', t => {
  const f = fixture(t);
  for (let index = 0; index < 350; index++) f.write(`docs/research/local-cell/note-${index}.md`);
  const docsHead = f.commit('many docs');
  scope(classifyEvent('push', push(f.base, docsHead), f.cwd), docs);
  scope(classifyLocal(f.base, f.cwd), docs);
  f.write('zz-runtime.go');
  scope(classifyLocal(f.base, f.cwd), mixedDocs);
  const mixedHead = f.commit('runtime after docs');
  scope(classifyEvent('push', push(f.base, mixedHead), f.cwd), mixedDocs);
});

test('docs local union preserves staged and committed runtime and index reversals', t => {
  for (const source of ['committed', 'staged']) {
    for (const path of ['cmd/witself/main.go', 'docs/README.md']) {
      const f = fixture(t);
      f.write(docsPath, '# Updated\n');
      f.commit('docs');
      f.write(path, 'change that may be pushed independently\n');
      if (source === 'committed') f.commit('independent change');
      else f.git('add', path);
      f.write(path, path === 'docs/README.md' ? '# Docs\n' : 'base application\n');
      scope(classifyLocal(f.base, f.cwd), path === 'docs/README.md' ? docsIndex : mixedDocs);
    }
  }
});

test('repository configuration cannot hide a mixed submodule change', t => {
  const f = fixture(t);
  f.git('update-index', '--add', '--cacheinfo', `160000,${f.base},vendor/module`);
  f.git('-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', 'gitlink baseline');
  const before = f.git('rev-parse', 'HEAD');
  f.write(docsPath, '# Updated\n');
  f.git('add', docsPath);
  f.git('update-index', '--cacheinfo', `160000,${before},vendor/module`);
  f.git('-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', 'mixed gitlink');
  const head = f.git('rev-parse', 'HEAD');
  f.git('config', 'diff.ignoreSubmodules', 'all');
  scope(classifyEvent('push', push(before, head), f.cwd), mixedDocs);
  scope(classifyEvent('pull_request', pull(before, head), f.cwd), mixedDocs);
});

test('docs deletion and renames within or across the allowlist never qualify', t => {
  for (const mutation of ['delete', 'within', 'out', 'in']) {
    const f = fixture(t);
    if (mutation === 'delete') f.git('rm', docsPath);
    else if (mutation === 'within') f.git('mv', docsPath, 'docs/research/local-cell/renamed.md');
    else if (mutation === 'out') f.git('mv', docsPath, 'docs/outside.md');
    else f.git('mv', 'cmd/witself/main.go', 'docs/research/local-cell/moved.md');
    scope(classifyLocal(f.base, f.cwd), mixedDocs);
    const head = f.commit(mutation);
    scope(classifyEvent('push', push(f.base, head), f.cwd), mixedDocs);
    scope(classifyEvent('pull_request', pull(f.base, head), f.cwd), mixedDocs);
  }
});

test('docs executable, symlink, and type changes never qualify', t => {
  for (const mutation of ['executable', 'executable-add', 'symlink', 'symlink-add', 'directory']) {
    const f = fixture(t);
    const path = mutation.endsWith('-add') ? 'docs/research/local-cell/new.md' : docsPath;
    if (mutation.startsWith('executable')) {
      f.write(path, '# Updated\n');
      chmodSync(join(f.cwd, path), 0o755);
      // Filemode must be checked even if repository configuration hides it.
      f.git('config', 'core.filemode', 'false');
    } else {
      rmSync(join(f.cwd, path), {force: true});
      if (mutation === 'directory') f.write(`${path}/file.txt`);
      else symlinkSync('../../README.md', join(f.cwd, path));
    }
    assert.equal(classifyLocal(f.base, f.cwd).docs_only, false, mutation);
    f.git('-c', 'core.filemode=true', 'add', '--all');
    f.git('-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', mutation);
    const head = f.git('rev-parse', 'HEAD');
    for (const result of [classifyEvent('push', push(f.base, head), f.cwd),
      classifyEvent('pull_request', pull(f.base, head), f.cwd)]) {
      assert.equal(result.application, true, mutation);
      assert.equal(result.docs_only, false, mutation);
    }
  }
});

test('mode reversals and executable-to-regular transitions do not qualify', t => {
  const f = fixture(t);
  chmodSync(join(f.cwd, docsPath), 0o755);
  f.git('add', docsPath);
  chmodSync(join(f.cwd, docsPath), 0o644);
  scope(classifyLocal(f.base, f.cwd), mixedDocs);
  chmodSync(join(f.cwd, docsPath), 0o755);
  const executableBase = f.commit('executable docs');
  chmodSync(join(f.cwd, docsPath), 0o644);
  const head = f.commit('regular again');
  scope(classifyEvent('push', push(executableBase, head), f.cwd), mixedDocs);
});

test('local hidden paths, clean filters, and failed comparisons fail closed', t => {
  for (const mode of ['assume-unchanged', 'skip-worktree', 'filter', 'fsmonitor']) {
    const f = fixture(t);
    f.write(docsPath, '# Updated\n');
    if (mode === 'filter') {
      f.write('.gitattributes', 'cmd/witself/main.go filter=conceal\n');
      f.commit('attribute baseline');
      f.git('config', 'filter.conceal.clean', "printf 'base application\\n'");
      f.write(docsPath, '# New update\n');
    } else if (mode === 'fsmonitor') f.git('config', 'core.fsmonitor', 'true');
    else f.git('update-index', `--${mode}`, 'cmd/witself/main.go');
    f.write('cmd/witself/main.go', 'hidden runtime change\n');
    scope(classifyLocal(f.base, f.cwd), full);
  }
  const f = fixture(t);
  f.write(docsPath, '# Updated\n');
  for (const ref of [undefined, 42, '', '--help', 'missing']) scope(classifyLocal(ref, f.cwd), full);
});

test('docs collector reads all direct documents and snapshot binds paths and raw bytes', t => {
  const f = fixture(t);
  f.write('docs/research/local-cell/second.md', '# Another\n');
  f.write('docs/research/local-cell/nested/ignored.md', '# Outside lane\n');
  f.write('docs/research/local-cell/.hidden.md', '# Outside lane\n');
  const inputs = collectDocsInputs(f.cwd);
  assert.deepEqual(inputs.map(input => input.path), ['docs/README.md', docsPath, 'docs/research/local-cell/second.md']);
  assert.ok(inputs.every(input => Buffer.isBuffer(input.body)));
  const before = captureDocsSnapshot(f.cwd);
  f.write(docsPath, '# Changed\n');
  const modified = captureDocsSnapshot(f.cwd);
  assert.notEqual(before, modified);
  assert.equal(captureDocsSnapshot(f.cwd), modified);
  f.write('docs/research/local-cell/third.md', '# Added\n');
  assert.notEqual(captureDocsSnapshot(f.cwd), modified);
});

test('docs collector rejects symlink parents, symlink sources, executable and missing input', t => {
  for (const mutation of ['parent', 'source', 'executable', 'missing-index', 'directory-source', 'missing-directory']) {
    const f = fixture(t);
    if (mutation === 'parent') {
      f.git('mv', 'docs/research/local-cell', 'docs/research/elsewhere');
      symlinkSync('elsewhere', join(f.cwd, 'docs/research/local-cell'));
    } else if (mutation === 'source') {
      rmSync(join(f.cwd, docsPath));
      symlinkSync('../../README.md', join(f.cwd, docsPath));
    } else if (mutation === 'executable') chmodSync(join(f.cwd, docsPath), 0o755);
    else if (mutation === 'missing-index') rmSync(join(f.cwd, 'docs/README.md'));
    else if (mutation === 'missing-directory') rmSync(join(f.cwd, 'docs/research/local-cell'), {recursive: true});
    else {
      rmSync(join(f.cwd, docsPath));
      mkdirSync(join(f.cwd, docsPath));
    }
    assert.throws(() => collectDocsInputs(f.cwd), mutation);
    assert.throws(() => captureDocsSnapshot(f.cwd), mutation);
  }
});
