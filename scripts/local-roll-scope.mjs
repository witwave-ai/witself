#!/usr/bin/env node
// Local-only scope evidence. A positive result is NOT release verification and
// never changes GitHub CI: check-local-roll must verify the release and generation.
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {existsSync, lstatSync, readFileSync, realpathSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {pathToFileURL} from 'node:url';

const catalogPath = '.gitops/cells/catalog.yaml';
const versionPattern = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const reject = reason => { throw new ScopeError(reason); };
class ScopeError extends Error {}
const gitRaw = (cwd, ...args) => execFileSync('git', ['--no-replace-objects', ...args], {
  cwd, maxBuffer: 32 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
});
const git = (cwd, ...args) => gitRaw(cwd, '-c', 'core.fsmonitor=false', ...args);
const decode = buffer => new TextDecoder('utf-8', {fatal: true}).decode(buffer);
function nul(buffer) {
  const value = decode(buffer);
  if (!value) return [];
  if (!value.endsWith('\0')) reject('invalid-git-output');
  return value.slice(0, -1).split('\0');
}
function sha(cwd, ref) {
  const value = decode(git(cwd, 'rev-parse', '--verify', '--end-of-options', `${ref}^{commit}`)).trim();
  if (!/^[a-f0-9]{40}$/.test(value)) reject('invalid-commit');
  return value;
}
function changed(cwd, ...range) {
  const fields = nul(git(cwd, '-c', 'core.filemode=true', 'diff', '--no-ext-diff', '--no-textconv', '--no-renames', '--name-status', '-z', ...range, '--'));
  if (fields.length % 2) reject('invalid-git-output');
  const paths = [];
  for (let i = 0; i < fields.length; i += 2) {
    // Disallow adds/deletes/type changes as well as both sides of a rename.
    if (fields[i] !== 'M') reject('not-existing-file-modification');
    paths.push(fields[i + 1]);
  }
  return paths;
}
function treeFile(cwd, ref, path) {
  const entries = nul(git(cwd, 'ls-tree', '-z', ref, '--', path));
  if (entries.length !== 1) reject('missing-tracked-file');
  const match = /^(100644|100755) blob ([a-f0-9]{40})\t(.+)$/.exec(entries[0]);
  if (!match || match[3] !== path) reject('nonregular-tracked-file');
  return {mode: match[1], body: git(cwd, 'cat-file', 'blob', match[2])};
}
function indexFile(cwd, path) {
  const entries = nul(git(cwd, 'ls-files', '--stage', '-z', '--', path));
  if (entries.length !== 1) reject('unmerged-or-missing-index-file');
  const match = /^(100644|100755) ([a-f0-9]{40}) 0\t(.+)$/.exec(entries[0]);
  if (!match || match[3] !== path) reject('nonregular-index-file');
  return {mode: match[1], body: git(cwd, 'cat-file', 'blob', match[2])};
}
function workingFile(cwd, path, checkedDirectories = new Set()) {
  const parts = path.split('/');
  if (parts.some(part => !part || part === '.' || part === '..') || path.startsWith('/')) reject('invalid-tracked-path');
  for (let i = 1; i <= parts.length; i++) {
    const component = join(cwd, ...parts.slice(0, i));
    if (i < parts.length && checkedDirectories.has(component)) continue;
    const info = lstatSync(component);
    if (info.isSymbolicLink() || (i < parts.length ? !info.isDirectory() : !info.isFile())) {
      reject('nonregular-working-file');
    }
    if (i === parts.length) return {mode: info.mode & 0o111 ? '100755' : '100644', body: readFileSync(component)};
    checkedDirectories.add(component);
  }
}
function linesOf(body) {
  const text = decode(body);
  if (!text.endsWith('\n') || /[\r\t\0]/.test(text)) reject('noncanonical-generated-values');
  return text.slice(0, -1).split('\n');
}
// Deliberately recognize only plain generated mapping keys, not arbitrary YAML.
// Canonical-generation validation is a separate mandatory gate. This rejects
// duplicate/quoted/aliased path keys without adding a second YAML implementation.
function mapping(lines, from, to, indent) {
  const entries = new Map();
  for (let i = from; i < to; i++) {
    if (!lines[i].trim() || lines[i].trimStart().startsWith('#')) continue;
    const spaces = lines[i].length - lines[i].trimStart().length;
    if (spaces < indent) reject('noncanonical-generated-values');
    if (spaces !== indent) continue;
    const match = /^([A-Za-z][A-Za-z0-9_-]*):(?: (.*))?$/.exec(lines[i].slice(indent));
    if (!match || entries.has(match[1])) reject('noncanonical-generated-values');
    entries.set(match[1], {index: i, value: match[2] ?? ''});
  }
  return entries;
}
function block(lines, entries, name, end) {
  const entry = entries.get(name);
  if (!entry || entry.value !== '') reject('noncanonical-generated-values');
  const next = [...entries.values()].find(item => item.index > entry.index);
  return [entry.index + 1, next?.index ?? end];
}
function registered(body, cell) {
  const lines = linesOf(body);
  const root = mapping(lines, 0, lines.length, 0);
  if (root.get('schema')?.value !== 'witself.gitops-cells.v1') reject('unknown-cell-catalog');
  const [from, to] = block(lines, root, 'cells', lines.length);
  const cells = mapping(lines, from, to, 2);
  if (!cells.has(cell) || cells.get(cell).value !== '') reject('unregistered-cell');
}
function pins(body, cell) {
  const lines = linesOf(body);
  const root = mapping(lines, 0, lines.length, 0);
  const [cellFrom, cellTo] = block(lines, root, 'cell', lines.length);
  if (mapping(lines, cellFrom, cellTo, 2).get('name')?.value !== cell) reject('cell-identity-mismatch');
  const [appsFrom, appsTo] = block(lines, root, 'apps', lines.length);
  const apps = mapping(lines, appsFrom, appsTo, 2);
  const [serverFrom, serverTo] = block(lines, apps, 'witselfServer', appsTo);
  const fields = mapping(lines, serverFrom, serverTo, 4);
  const chart = fields.get('chartVersion');
  const tag = fields.get('imageTag');
  const digest = fields.get('imageDigest');
  if (!chart || !tag || !digest || tag.index !== chart.index + 1 || digest.index !== tag.index + 1 ||
      !versionPattern.test(chart.value) || chart.value !== tag.value || !/^sha256:[a-f0-9]{64}$/.test(digest.value)) {
    reject('invalid-or-incomplete-release-pins');
  }
  for (const [name, field] of [['chartVersion', chart], ['imageTag', tag], ['imageDigest', digest]]) {
    // Everything except the three scalar values remains byte-identical.
    lines[field.index] = `    ${name}: <release-pin>`;
  }
  return {version: chart.value, digest: digest.value, normalized: lines.join('\n')};
}
function forward(previous, next) {
  const a = previous.split('.').map(BigInt);
  const b = next.split('.').map(BigInt);
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return b[i] > a[i];
  return false;
}

const redirectedGitEnvironment = /^GIT_(?:DIR|WORK_TREE|COMMON_DIR|INDEX_FILE|OBJECT_DIRECTORY|ALTERNATE_OBJECT_DIRECTORIES|NAMESPACE|REPLACE_REF_BASE|SHALLOW_FILE|CONFIG(?:_.*)?|ATTR_SOURCE|GRAFT_FILE|PREFIX|CEILING_DIRECTORIES|DISCOVERY_ACROSS_FILESYSTEM|EXTERNAL_DIFF|DIFF_OPTS)$/;
function assertLocalContext(cwd) {
  if (Object.keys(process.env).some(key => redirectedGitEnvironment.test(key))) reject('redirected-git-environment');
  const top = decode(git(cwd, 'rev-parse', '--show-toplevel')).trim();
  if (realpathSync(top) !== realpathSync(cwd)) reject('repository-root-mismatch');
}

// Run before fetching: an overridden Git context must never redirect the fetch.
// Dirty files, hidden flags, and history overlays belong to integrity fallback.
export function checkLocalContext(cwd = process.cwd()) {
  try {
    assertLocalContext(resolve(cwd));
    return {safe: true};
  } catch (error) {
    return {safe: false, reason: error instanceof ScopeError ? error.message : 'comparison-unavailable'};
  }
}
function checkGitContext(cwd) {
  assertLocalContext(cwd);
  // The website classifier uses ordinary Git status. Do not silently disable a
  // configured FSMonitor here and then trust a different cached path list there.
  try {
    const monitor = decode(gitRaw(cwd, 'config', '--get', 'core.fsmonitor')).trim();
    if (!/^(?:false|no|off|0)$/i.test(monitor)) reject('fsmonitor-configuration');
  } catch (error) { if (error.status !== 1) throw error; }
  if (decode(git(cwd, 'for-each-ref', '--format=%(refname)', 'refs/replace')).trim()) reject('replacement-history');
  const grafts = resolve(cwd, decode(git(cwd, 'rev-parse', '--git-path', 'info/grafts')).trim());
  if (existsSync(grafts)) {
    const info = lstatSync(grafts);
    if (!info.isFile() || info.size !== 0) reject('grafted-history');
  }
  if (nul(git(cwd, 'ls-files', '-v', '-z')).some(item => !item.startsWith('H '))) reject('hidden-or-unmerged-index-state');
}
function rawWorkingSnapshot(cwd, index) {
  const visible = new Set(nul(git(cwd, 'diff', '--no-ext-diff', '--no-textconv',
    '--no-renames', '--name-only', '-z', '--')));
  const snapshot = [];
  const checkedDirectories = new Set();
  for (const entry of nul(index)) {
    const match = /^(100644|100755) ([a-f0-9]{40}) 0\t([\s\S]+)$/.exec(entry);
    if (!match) reject('nonregular-or-unmerged-index-file');
    const [, mode, oid, path] = match;
    let state;
    try { state = workingFile(cwd, path, checkedDirectories); }
    catch (error) {
      if (error.code !== 'ENOENT') throw error;
      if (!visible.has(path)) reject('hidden-working-tree-change');
      snapshot.push([path, 'missing']);
      continue;
    }
    const actual = createHash('sha1').update(`blob ${state.body.length}\0`).update(state.body).digest('hex');
    if ((state.mode !== mode || actual !== oid) && !visible.has(path)) reject('hidden-working-tree-change');
    snapshot.push([path, state.mode, actual]);
  }
  return JSON.stringify(snapshot);
}

// Shared by the automatic local gate before either fast lane. Raw byte hashes
// catch clean-filter and stale-stat/FSMonitor omissions that Git's path list can
// hide. Visible edits are allowed here; each lane still owns its scope rules.
export function checkLocalIntegrity(cwd = process.cwd()) {
  try {
    cwd = resolve(cwd);
    checkGitContext(cwd);
    const head = sha(cwd, 'HEAD');
    const index = git(cwd, 'ls-files', '--stage', '-z');
    const snapshot = rawWorkingSnapshot(cwd, index);
    if (snapshot !== rawWorkingSnapshot(cwd, index) || !index.equals(git(cwd, 'ls-files', '--stage', '-z')) ||
        head !== sha(cwd, 'HEAD')) reject('repository-changed-during-classification');
    checkGitContext(cwd);
    return {safe: true};
  } catch (error) {
    return {safe: false, reason: error instanceof ScopeError ? error.message : 'comparison-unavailable'};
  }
}

export function classifyLocal(baseRef, cwd = process.cwd()) {
  try {
    if (typeof baseRef !== 'string' || !baseRef || baseRef.startsWith('-')) reject('invalid-local-base');
    cwd = resolve(cwd);
    const integrity = checkLocalIntegrity(cwd);
    if (!integrity.safe) reject(integrity.reason);
    const base = sha(cwd, baseRef);
    const head = sha(cwd, 'HEAD');
    // Stale/divergent local branches take the full path rather than qualifying
    // against an older merge-base whose release pins main has already replaced.
    git(cwd, 'merge-base', '--is-ancestor', base, head);
    if (nul(git(cwd, 'ls-files', '-v', '-z')).some(item => !item.startsWith('H '))) reject('hidden-or-unmerged-index-state');
    if (nul(git(cwd, 'ls-files', '--others', '--exclude-standard', '-z')).length) reject('untracked-files');
    const indexSnapshot = git(cwd, 'ls-files', '--stage', '-z');
    const history = decode(git(cwd, 'rev-list', '--reverse', '--topo-order', '--parents', `${base}..${head}`)).trim();
    const revisions = [base];
    for (const line of history ? history.split('\n') : []) {
      const [commit, ...parents] = line.split(' ');
      if (parents.length !== 1 || parents[0] !== revisions.at(-1)) reject('nonlinear-branch');
      revisions.push(commit);
    }
    // Per-commit inspection also refuses a committed mixed change later reverted.
    const changes = revisions.slice(1).flatMap((revision, i) => changed(cwd, revisions[i], revision));
    const workingChanges = changed(cwd);
    changes.push(...changed(cwd, base, head), ...changed(cwd, '--cached', head), ...workingChanges);
    const paths = [...new Set(changes)];
    if (paths.length !== 1) reject(paths.length ? 'mixed-or-multiple-cell-changes' : 'empty-comparison');
    const path = paths[0];
    const match = /^\.gitops\/cells\/([a-z0-9]+(?:-[a-z0-9]+)*)\/values\.yaml$/.exec(path);
    if (!match) reject('not-cell-values');
    const cell = match[1];
    registered(treeFile(cwd, base, catalogPath).body, cell);
    const states = [...revisions.map(revision => treeFile(cwd, revision, path)), indexFile(cwd, path), workingFile(cwd, path)];
    const original = pins(states[0].body, cell);
    let selected;
    let changedOnce = false;
    for (const state of states.slice(1)) {
      if (state.mode !== states[0].mode) reject('file-mode-changed');
      const current = pins(state.body, cell);
      if (current.normalized !== original.normalized) reject('non-pin-content-changed');
      if (state.body.equals(states[0].body)) {
        if (changedOnce) reject('reversed-release-pins');
        continue;
      }
      if (!forward(original.version, current.version) || original.digest === current.digest) reject('not-complete-forward-roll');
      if (selected && (selected.version !== current.version || selected.digest !== current.digest)) reject('conflicting-release-pins');
      selected = current;
      changedOnce = true;
    }
    if (!selected) reject('empty-comparison');
    // Avoid returning evidence for an index or HEAD changed while examining it.
    const finalWorking = workingFile(cwd, path);
    if (base !== sha(cwd, baseRef) || head !== sha(cwd, 'HEAD') ||
        !indexSnapshot.equals(git(cwd, 'ls-files', '--stage', '-z')) ||
        JSON.stringify(workingChanges) !== JSON.stringify(changed(cwd)) ||
        nul(git(cwd, 'ls-files', '--others', '--exclude-standard', '-z')).length ||
        nul(git(cwd, 'ls-files', '-v', '-z')).some(item => !item.startsWith('H ')) ||
        states.at(-1).mode !== finalWorking.mode || !states.at(-1).body.equals(finalWorking.body)) {
      reject('repository-changed-during-classification');
    }
    const finalIntegrity = checkLocalIntegrity(cwd);
    if (!finalIntegrity.safe) reject(finalIntegrity.reason);
    return {roll_only: true, reason: 'application-release-pins-only', cell, version: selected.version,
      previous_version: original.version, base_sha: base, head_sha: head, values_path: path, image_digest: selected.digest};
  } catch (error) {
    return {roll_only: false, reason: error instanceof ScopeError ? error.message : 'comparison-unavailable'};
  }
}

export function main(args = process.argv.slice(2), cwd = process.cwd()) {
  if (args.length !== 2 || args[0] !== '--base') throw new Error('usage: local-roll-scope.mjs --base origin/main');
  const {image_digest: _privateDigest, ...result} = classifyLocal(args[1], cwd);
  console.log(JSON.stringify(result));
  return result;
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
