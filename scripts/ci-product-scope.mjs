#!/usr/bin/env node
// Only explicit brochure files or verified regular research-doc additions and
// modifications can replace application gates. Mixed scopes take the full lane.
import {appendFileSync, lstatSync, readFileSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';
import {docsIndex, isDocsPath, validPath} from './docs-scope.mjs';
import {checkLocalIntegrity} from './local-roll-scope.mjs';
import {join} from 'node:path';

export const brochureFiles = Object.freeze([
  'web/product/README.md',
  'web/product/DISCOVERY.md',
  'web/product/content.mjs',
  'web/product/build.mjs',
  'web/product/styles.css',
  'web/product/app.js',
  'web/product/favicon.svg',
  'web/product/assets/screenshots/agent-console.png',
  'web/product/assets/screenshots/agent-tui.png',
  'web/product/assets/screenshots/manifest.json',
]);

const full = reason => ({application: true, website: true, website_only: false,
  docs: true, docs_only: false, docs_index: true, reason});
const websiteInput = path => path.startsWith('web/product/') ||
  path.startsWith('web/plans/') || path.startsWith('infra/cloudflare/') ||
  path.startsWith('.github/workflows/') || path.startsWith('scripts/ci-product-') ||
  ['Makefile', 'CLAUDE.md'].includes(path);

function classify(paths, docsOnly = false) {
  if (!Array.isArray(paths) || paths.length === 0) return full('empty-comparison');
  if (paths.some(path => !validPath(path))) return full('invalid-path');
  const websiteOnly = paths.every(path => brochureFiles.includes(path));
  return {
    application: !websiteOnly && !docsOnly,
    website: paths.some(websiteInput),
    website_only: websiteOnly,
    docs: paths.some(isDocsPath),
    docs_only: docsOnly,
    docs_index: paths.includes(docsIndex),
    reason: websiteOnly ? 'brochure-only' : docsOnly ? 'research-docs-only' : 'application-or-shared-change',
  };
}

// Path-only callers cannot establish the file type/status contract for docs.
export function classifyPaths(paths) {
  return classify(paths);
}

export function parsePaths(buffer) {
  // Do not lose the second side of a rename or split filenames on whitespace.
  const text = new TextDecoder('utf-8', {fatal: true, ignoreBOM: true}).decode(buffer);
  if (text === '') return [];
  if (!text.endsWith('\0')) throw new Error('incomplete-path-list');
  const paths = text.slice(0, -1).split('\0');
  if (paths.some(path => !path)) throw new Error('empty-path-record');
  return paths;
}

const modes = new Set(['000000', '100644', '100755', '120000', '160000']);
const objectID = /^[a-f0-9]{40}$/;
function validChange(change) {
  if (!change || typeof change !== 'object' || !validPath(change.path) ||
      !modes.has(change.oldMode) || !modes.has(change.newMode) ||
      !objectID.test(change.oldOID ?? '') || !objectID.test(change.newOID ?? '') ||
      !['A', 'M', 'D', 'T', 'U'].includes(change.status)) return false;
  if (change.status === 'A') return change.oldMode === '000000' && change.newMode !== '000000' && /^0+$/.test(change.oldOID);
  if (change.status === 'D') return change.oldMode !== '000000' && change.newMode === '000000' && /^0+$/.test(change.newOID);
  if (change.status === 'U') return true;
  return change.oldMode !== '000000' && change.newMode !== '000000';
}

// --raw -z preserves status, both modes, and every filename. --no-renames emits
// source deletions separately, so even a rename within the docs allowlist fails
// the add/modify contract. Rename records reaching this parser are malformed.
export function parseChanges(buffer) {
  const fields = parsePaths(buffer);
  if (fields.length % 2) throw new Error('incomplete-change-list');
  const changes = [];
  for (let i = 0; i < fields.length; i += 2) {
    const match = /^:(\d{6}) (\d{6}) ([a-f0-9]{40}) ([a-f0-9]{40}) ([AMDTU])$/.exec(fields[i]);
    if (!match) throw new Error('invalid-change-record');
    const [, oldMode, newMode, oldOID, newOID, status] = match;
    const change = {path: fields[i + 1], oldMode, newMode, oldOID, newOID, status};
    if (!validChange(change)) throw new Error('invalid-change-record');
    changes.push(change);
  }
  return changes;
}

export function classifyChanges(changes) {
  if (!Array.isArray(changes) || changes.length === 0) return full('empty-comparison');
  if (changes.some(change => !validChange(change))) return full('invalid-change-record');
  const docsOnly = changes.every(change => isDocsPath(change.path) && change.newMode === '100644' &&
    (change.status === 'A' && change.oldMode === '000000' ||
     change.status === 'M' && change.oldMode === '100644'));
  return classify(changes.map(change => change.path), docsOnly);
}

function git(args, cwd) {
  return execFileSync('git', ['--no-replace-objects', '-c', 'core.filemode=true', '-c', 'core.fsmonitor=false', ...args],
    {cwd, maxBuffer: 32 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe']});
}

function changed(cwd, ...range) {
  return parseChanges(git(['diff', '--no-ext-diff', '--no-textconv', '--no-renames', '--ignore-submodules=none', '--raw',
    '--abbrev=40', '-z', ...range, '--'], cwd));
}

function untracked(cwd) {
  return parsePaths(git(['ls-files', '--others', '--exclude-standard', '-z'], cwd)).map(path => {
    if (!validPath(path)) throw new Error('invalid-path');
    const parts = path.split('/');
    let mode;
    for (let i = 1; i <= parts.length; i++) {
      const info = lstatSync(join(cwd, ...parts.slice(0, i)));
      if (i < parts.length) {
        if (info.isSymbolicLink() || !info.isDirectory()) throw new Error('nonregular-parent');
      } else if (info.isSymbolicLink()) mode = '120000';
      else if (info.isFile()) mode = info.mode & 0o111 ? '100755' : '100644';
      else throw new Error('nonregular-untracked-file');
    }
    return {path, status: 'A', oldMode: '000000', newMode: mode, oldOID: '0'.repeat(40), newOID: '0'.repeat(40)};
  });
}

function commit(ref, cwd) {
  if (!/^[a-f0-9]{40}$/i.test(ref ?? '') || /^0+$/.test(ref)) throw new Error('invalid-ref');
  git(['cat-file', '-e', `${ref}^{commit}`], cwd);
  return ref;
}

function docsCheckoutMatches(eventName, event, head, target, cwd) {
  const checkout = commit(git(['rev-parse', 'HEAD'], cwd).toString().trim(), cwd);
  if (checkout === head) return true;
  if (eventName !== 'pull_request') return false;
  // Actions normally checks out GitHub's test merge. Bind it to the event's
  // exact merge object and ordered base/head parents, not any two-parent commit.
  const merge = commit(event?.pull_request?.merge_commit_sha, cwd);
  if (checkout !== merge) return false;
  const parents = git(['rev-list', '--parents', '-n', '1', merge], cwd).toString().trim().split(' ');
  return parents.length === 3 && parents[0] === merge && parents[1] === target && parents[2] === head;
}

export function classifyEvent(eventName, event, cwd = process.cwd()) {
  try {
    let base;
    let head;
    let target;
    if (eventName === 'pull_request') {
      head = commit(event?.pull_request?.head?.sha, cwd);
      target = commit(event?.pull_request?.base?.sha, cwd);
      base = commit(git(['merge-base', target, head], cwd).toString().trim(), cwd);
    } else if (eventName === 'push') {
      base = commit(event?.before, cwd);
      head = commit(event?.after, cwd);
      git(['merge-base', '--is-ancestor', base, head], cwd);
    } else {
      return full('unsupported-event');
    }
    const result = classifyChanges(changed(cwd, base, head));
    if (result.docs_only && !docsCheckoutMatches(eventName, event, head, target, cwd)) return full('docs-checkout-mismatch');
    return result;
  } catch {
    // An unavailable comparison never permits skipping application checks.
    return full('comparison-unavailable');
  }
}

export function classifyLocal(baseRef, cwd = process.cwd()) {
  try {
    if (typeof baseRef !== 'string' || !baseRef || baseRef.startsWith('-')) return full('invalid-local-base');
    const integrity = checkLocalIntegrity(cwd);
    if (!integrity.safe) return full(integrity.reason);
    const target = commit(git(['rev-parse', '--verify', '--end-of-options', `${baseRef}^{commit}`], cwd).toString().trim(), cwd);
    const head = commit(git(['rev-parse', 'HEAD'], cwd).toString().trim(), cwd);
    const base = commit(git(['merge-base', target, head], cwd).toString().trim(), cwd);
    // Union each layer: an uncommitted reversal must not hide a committed or
    // staged application change that could still be pushed independently.
    const changes = [...changed(cwd, base, head), ...changed(cwd, '--cached', head),
      ...changed(cwd), ...untracked(cwd)];
    return classifyChanges(changes);
  } catch {
    return full('comparison-unavailable');
  }
}

export function main(args = process.argv.slice(2), env = process.env) {
  let result;
  if (args.length === 2 && args[0] === '--base') {
    result = classifyLocal(args[1]);
  } else if (args.length) {
    throw new Error('usage: ci-product-scope.mjs [--base origin/main]');
  } else {
    try {
      result = classifyEvent(env.GITHUB_EVENT_NAME, JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8')));
    } catch {
      result = full('event-unavailable');
    }
  }
  console.log(JSON.stringify(result));
  if (env.GITHUB_OUTPUT) {
    appendFileSync(env.GITHUB_OUTPUT, ['application', 'website', 'website_only', 'docs', 'docs_only', 'docs_index']
      .map(key => `${key}=${result[key]}\n`).join(''));
  }
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
