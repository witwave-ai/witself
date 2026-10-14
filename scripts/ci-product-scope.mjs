#!/usr/bin/env node
// Only these brochure files can replace application gates with website gates.
// Workflow/policy changes, shared prices/dependencies, and screenshot tooling
// deliberately stay on the full application path.
import {appendFileSync, readFileSync} from 'node:fs';
import {execFileSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';

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

const full = reason => ({application: true, website: true, website_only: false, reason});
const websiteInput = path => path.startsWith('web/product/') ||
  path.startsWith('web/plans/') || path.startsWith('infra/cloudflare/') ||
  path.startsWith('.github/workflows/') || path.startsWith('scripts/ci-product-') ||
  ['Makefile', 'CLAUDE.md'].includes(path);

export function classifyPaths(paths) {
  if (!Array.isArray(paths) || paths.length === 0) return full('empty-comparison');
  if (paths.some(path => typeof path !== 'string' || !path || path.startsWith('/') ||
    path.split('/').some(part => part === '..' || part === '.' || !part) ||
    /[\0\ufffd]/u.test(path))) return full('invalid-path');
  const websiteOnly = paths.every(path => brochureFiles.includes(path));
  return {
    application: !websiteOnly,
    website: paths.some(websiteInput),
    website_only: websiteOnly,
    reason: websiteOnly ? 'brochure-only' : 'application-or-shared-change',
  };
}

export function parsePaths(buffer) {
  // Do not lose the second side of a rename or split filenames on whitespace.
  const text = new TextDecoder('utf-8', {fatal: true}).decode(buffer);
  if (text === '') return [];
  if (!text.endsWith('\0')) throw new Error('incomplete-path-list');
  return text.slice(0, -1).split('\0');
}

function git(args, cwd) {
  return execFileSync('git', args, {cwd, maxBuffer: 32 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe']});
}

function commit(ref, cwd) {
  if (!/^[a-f0-9]{40}$/i.test(ref ?? '') || /^0+$/.test(ref)) throw new Error('invalid-ref');
  git(['cat-file', '-e', `${ref}^{commit}`], cwd);
  return ref;
}

export function classifyEvent(eventName, event, cwd = process.cwd()) {
  try {
    let base;
    let head;
    if (eventName === 'pull_request') {
      head = commit(event?.pull_request?.head?.sha, cwd);
      const target = commit(event?.pull_request?.base?.sha, cwd);
      base = commit(git(['merge-base', target, head], cwd).toString().trim(), cwd);
    } else if (eventName === 'push') {
      base = commit(event?.before, cwd);
      head = commit(event?.after, cwd);
      git(['merge-base', '--is-ancestor', base, head], cwd);
    } else {
      return full('unsupported-event');
    }
    return classifyPaths(parsePaths(git(['diff', '--no-renames', '--name-only', '-z', base, head, '--'], cwd)));
  } catch {
    // An unavailable comparison never permits skipping application checks.
    return full('comparison-unavailable');
  }
}

export function classifyLocal(baseRef, cwd = process.cwd()) {
  try {
    if (!baseRef || baseRef.startsWith('-')) return full('invalid-local-base');
    const target = commit(git(['rev-parse', '--verify', '--end-of-options', `${baseRef}^{commit}`], cwd).toString().trim(), cwd);
    const head = commit(git(['rev-parse', 'HEAD'], cwd).toString().trim(), cwd);
    const base = commit(git(['merge-base', target, head], cwd).toString().trim(), cwd);
    // Union each layer: an uncommitted reversal must not hide a committed or
    // staged application change that could still be pushed independently.
    const changed = [
      ['diff', '--no-renames', '--name-only', '-z', base, head, '--'],
      ['diff', '--cached', '--no-renames', '--name-only', '-z', head, '--'],
      ['diff', '--no-renames', '--name-only', '-z', '--'],
      ['ls-files', '--others', '--exclude-standard', '-z'],
    ].flatMap(args => parsePaths(git(args, cwd)));
    return classifyPaths([...new Set(changed)]);
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
    appendFileSync(env.GITHUB_OUTPUT, ['application', 'website', 'website_only']
      .map(key => `${key}=${result[key]}\n`).join(''));
  }
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
