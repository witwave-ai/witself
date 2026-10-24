#!/usr/bin/env node
// The same bounded document gate runs locally and in GitHub Actions.
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {execFileSync, spawnSync} from 'node:child_process';
import {lstatSync, readFileSync} from 'node:fs';
import {resolve} from 'node:path';
import {pathToFileURL} from 'node:url';
import {classifyEvent, classifyLocal} from './ci-product-scope.mjs';
import {captureDocsSnapshot} from './docs-scope.mjs';
import {checkDocs} from './check-docs.mjs';

const BASE = 'origin/main';
const git = (cwd, ...args) => execFileSync('git', ['--no-replace-objects', '-c', 'core.fsmonitor=false', ...args], {
  cwd, maxBuffer: 32 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
});

// A lane name alone is not a proof that the content checked stayed unchanged.
// Integrity checks in check-local additionally reject hidden Git/filter state.
export function captureLocalDocsState(cwd) {
  const hash = createHash('sha256');
  const add = value => {
    const bytes = Buffer.isBuffer(value) ? value : Buffer.from(value);
    hash.update(String(bytes.length)); hash.update(':'); hash.update(bytes);
  };
  for (const args of [
    ['rev-parse', '--verify', 'HEAD^{commit}'],
    ['rev-parse', '--verify', `${BASE}^{commit}`],
    ['ls-files', '--stage', '-z'],
    ['diff', '--no-ext-diff', '--no-textconv', '--binary', 'HEAD', '--'],
  ]) add(git(cwd, ...args));
  const untracked = new TextDecoder('utf-8', {fatal: true}).decode(git(cwd, 'ls-files', '--others', '--exclude-standard', '-z'));
  assert(!untracked || untracked.endsWith('\0'), 'Incomplete untracked path list');
  for (const path of untracked ? untracked.slice(0, -1).split('\0') : []) {
    const full = resolve(cwd, path), info = lstatSync(full);
    assert(info.isFile() && !info.isSymbolicLink(), 'Unsupported untracked file');
    add(path); add(String(info.mode)); add(readFileSync(full));
  }
  add(captureDocsSnapshot(cwd));
  return hash.digest('hex');
}

export function docsScope(cwd, env = process.env) {
  if (env.GITHUB_ACTIONS === 'true') {
    assert(env.GITHUB_EVENT_NAME && env.GITHUB_EVENT_PATH, 'Missing GitHub comparison context');
    const event = JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8'));
    return classifyEvent(env.GITHUB_EVENT_NAME, event, cwd);
  }
  return classifyLocal(BASE, cwd);
}

export function checkDocsWhitespace(cwd, env = process.env) {
  let base, head = 'HEAD';
  if (env.GITHUB_ACTIONS === 'true') {
    const event = JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8'));
    if (env.GITHUB_EVENT_NAME === 'pull_request') {
      head = event?.pull_request?.head?.sha;
      const target = event?.pull_request?.base?.sha;
      assert(/^[a-f0-9]{40}$/i.test(head ?? '') && /^[a-f0-9]{40}$/i.test(target ?? ''), 'Missing PR whitespace comparison');
      base = git(cwd, 'merge-base', target, head).toString().trim();
    } else if (env.GITHUB_EVENT_NAME === 'push') {
      base = event?.before; head = event?.after;
      assert(/^[a-f0-9]{40}$/i.test(base ?? '') && /^[a-f0-9]{40}$/i.test(head ?? ''), 'Missing push whitespace comparison');
    } else throw new Error('Unsupported document whitespace comparison');
  } else base = git(cwd, 'merge-base', BASE, 'HEAD').toString().trim();
  const paths = ['docs/README.md', ':(glob)docs/research/local-cell/*.md'];
  for (const range of [[base, head], ['--cached', 'HEAD'], []]) {
    git(cwd, 'diff', '--no-ext-diff', '--no-textconv', '--check', ...range, '--', ...paths);
  }
  // Git's ordinary diffs omit untracked additions. A clean --no-index diff may
  // return 1 because the files differ; accept it only with no diagnostics.
  const names = new TextDecoder('utf-8', {fatal: true}).decode(git(cwd, 'ls-files', '--others', '--exclude-standard', '-z', '--', ...paths));
  assert(!names || names.endsWith('\0'), 'Incomplete whitespace path list');
  for (const path of names ? names.slice(0, -1).split('\0') : []) {
    try {
      git(cwd, 'diff', '--no-index', '--no-ext-diff', '--no-textconv', '--check', '--', '/dev/null', path);
    } catch (error) {
      if (error.status !== 1 || error.signal || error.stdout?.length || error.stderr?.length) throw error;
    }
  }
}

function execute(file, args, options) {
  const result = spawnSync(file, args, {...options, stdio: 'inherit'});
  if (result.error || result.signal || result.status !== 0) throw new Error('Document index contract failed');
}

export function runDocsChecks({cwd = process.cwd(), env = process.env,
  classify = docsScope, validate = checkDocs, whitespace = checkDocsWhitespace,
  run = execute, log = console.log} = {}) {
  const scope = classify(cwd, env);
  assert.equal(typeof scope?.docs_index, 'boolean', 'Missing document index scope');
  const result = validate({cwd});
  assert.equal(result?.passed, true, 'Document validation did not pass');
  whitespace(cwd, env);
  if (scope.docs_index) {
    run('go', ['test', './featurestatus', '-run', '^TestRunbookDocsIndexes$', '-count=1'], {cwd, env});
  }
  log(`docs check: PASS; index contract ${scope.docs_index ? 'passed' : 'not applicable'}`);
  return {passed: true, index_checked: scope.docs_index};
}

export function main(args = process.argv.slice(2)) {
  assert.equal(args.length, 0, 'usage: node scripts/run-docs-check.mjs');
  return runDocsChecks();
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); }
  catch (error) {
    for (const diagnostic of error.diagnostics ?? []) console.error(`${diagnostic.file}:${diagnostic.line}: ${diagnostic.message}`);
    console.error(`docs check: FAIL: ${error.message}`);
    process.exitCode = 1;
  }
}
