import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, utimesSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {checkLocalContext, checkLocalIntegrity, classifyLocal} from './local-roll-scope.mjs';

const script = fileURLToPath(new URL('./local-roll-scope.mjs', import.meta.url));
const cell = 'civo-sandbox-use1-serving';
const second = 'civo-sandbox-use1-backup';
const valuesPath = name => `.gitops/cells/${name}/values.yaml`;
const path = valuesPath(cell);
const oldDigest = `sha256:${'a'.repeat(64)}`;
const newDigest = `sha256:${'b'.repeat(64)}`;
const values = (name = cell, version = '1.2.3', digest = oldDigest) => `cell:\n  name: ${name}\n  cloud: civo\napps:\n  civoPostgres:\n    chartVersion: 18.8.0\n  witselfServer:\n    enabled: true\n    # Preserve operator settings and comments exactly.\n    chartVersion: ${version}\n    imageTag: ${version}\n    imageDigest: ${digest}\n    billing:\n      endpoint: https://self.example.invalid\n    worker:\n      enabled: true\nplatform:\n  monitoring:\n    enabled: true\n`;
function fixture(t, {body = values(), catalogCell = cell} = {}) {
  const cwd = mkdtempSync(join(tmpdir(), 'witself-local-roll-scope-'));
  t.after(() => rmSync(cwd, {recursive: true, force: true}));
  const git = (...args) => execFileSync('git', args, {cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    env: {...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null',
      GIT_AUTHOR_NAME: 'Roll Scope', GIT_AUTHOR_EMAIL: 'scope@example.invalid',
      GIT_COMMITTER_NAME: 'Roll Scope', GIT_COMMITTER_EMAIL: 'scope@example.invalid'}}).trim();
  const write = (name, content) => {mkdirSync(dirname(join(cwd, name)), {recursive: true}); writeFileSync(join(cwd, name), content);};
  const commit = message => {git('add', '--all'); git('-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', message); return git('rev-parse', 'HEAD');};
  git('init', '--quiet', '--initial-branch=main');
  write('.gitops/cells/catalog.yaml', `schema: witself.gitops-cells.v1\ncells:\n  ${catalogCell}:\n    cloud: civo\n  ${second}:\n    cloud: civo\n`);
  write(path, body);
  write(valuesPath(second), values(second));
  write('scripts/tool.mjs', 'original tooling\n');
  write('.gitignore', 'node_modules/\n');
  const base = commit('baseline');
  git('update-ref', 'refs/remotes/origin/main', base);
  return {cwd, git, write, commit, base, roll: () => write(path, values(cell, '1.2.4', newDigest)),
    classify: () => classifyLocal('origin/main', cwd)};
}
function denied(f, reason) {
  const result = f.classify();
  assert.equal(result.roll_only, false, JSON.stringify(result));
  if (reason) assert.equal(result.reason, reason);
  assert.equal(Object.hasOwn(result, 'image_digest'), false);
}

test('complete one-cell forward update qualifies in working, staged, or committed layer', t => {
  for (const layer of ['working', 'staged', 'committed']) {
    const f = fixture(t);
    f.roll();
    if (layer === 'staged') f.git('add', path);
    if (layer === 'committed') f.commit('roll');
    assert.deepEqual(f.classify(), {roll_only: true, reason: 'application-release-pins-only', cell,
      version: '1.2.4', previous_version: '1.2.3', base_sha: f.base, head_sha: f.git('rev-parse', 'HEAD'),
      values_path: path, image_digest: newDigest});
  }
});

test('real generated Civo values qualify without touching unrelated pins or settings', t => {
  const body = readFileSync(new URL('../.gitops/cells/civo-sandbox-use1-serving/values.yaml', import.meta.url), 'utf8');
  const f = fixture(t, {body});
  const updated = body.replace(/(  witselfServer:\n[\s\S]*?    chartVersion: )[0-9]+\.[0-9]+\.[0-9]+/, (_, prefix) => `${prefix}999.0.0`)
    .replace(/(^    imageTag: )[0-9]+\.[0-9]+\.[0-9]+$/m, '$1999.0.0')
    .replace(/(^    imageDigest: )sha256:[a-f0-9]{64}$/m, `$1${newDigest}`);
  f.write(path, updated);
  assert.equal(f.classify().roll_only, true);
});

test('CLI exposes value-free scope metadata and never prints image digest', t => {
  const f = fixture(t); f.roll();
  const output = execFileSync(process.execPath, [script, '--base', 'origin/main'], {cwd: f.cwd, encoding: 'utf8'});
  const result = JSON.parse(output);
  assert.equal(result.roll_only, true);
  assert.equal(result.version, '1.2.4');
  assert.equal(Object.hasOwn(result, 'image_digest'), false);
  assert.equal(output.includes(newDigest), false);
});

test('empty or unavailable comparisons and stale base fail closed', t => {
  const f = fixture(t);
  denied(f, 'empty-comparison');
  for (const base of [undefined, '', '--help', 'does-not-exist', '0'.repeat(40)]) assert.equal(classifyLocal(base, f.cwd).roll_only, false);
  f.git('switch', '--quiet', '-c', 'roll'); f.roll(); f.commit('roll');
  f.git('switch', '--quiet', 'main'); f.write('scripts/tool.mjs', 'main advanced\n');
  const advanced = f.commit('advance'); f.git('update-ref', 'refs/remotes/origin/main', advanced);
  f.git('switch', '--quiet', 'roll');
  denied(f, 'comparison-unavailable');
});

test('any other changed file in every layer prevents the shortcut', t => {
  for (const layer of ['working', 'staged', 'committed', 'untracked']) {
    const f = fixture(t); f.roll();
    f.write(layer === 'untracked' ? 'unknown\nfile.txt' : 'scripts/tool.mjs', 'dirty tooling\n');
    if (layer === 'staged') f.git('add', '--all');
    if (layer === 'committed') f.commit('mixed');
    denied(f);
  }
});

test('reversals cannot hide committed or staged unrelated changes', t => {
  for (const layer of ['staged', 'committed', 'committed-revert']) {
    const f = fixture(t); f.roll(); f.commit('roll');
    f.write('scripts/tool.mjs', 'changed tool\n');
    if (layer === 'staged') f.git('add', 'scripts/tool.mjs');
    else f.commit('tool change');
    f.write('scripts/tool.mjs', 'original tooling\n');
    if (layer === 'committed-revert') f.commit('tool reversal');
    denied(f, 'mixed-or-multiple-cell-changes');
  }
});

test('pin reversals and multiple intermediate release targets fail closed', t => {
  for (const variant of ['staged-reverse', 'committed-reverse', 'later-target', 'different-digest']) {
    const f = fixture(t); f.roll();
    if (variant === 'staged-reverse') f.git('add', path);
    else f.commit('first roll');
    if (variant.endsWith('reverse')) f.write(path, values());
    else f.write(path, values(cell, variant === 'later-target' ? '1.2.5' : '1.2.4', `sha256:${'c'.repeat(64)}`));
    denied(f, variant.endsWith('reverse') ? 'reversed-release-pins' : 'conflicting-release-pins');
  }
});

test('partial versions, retained digest, downgrades, and malformed pins cannot qualify', t => {
  const mutations = [
    body => body.replace('imageTag: 1.2.4', 'imageTag: 1.2.3'),
    body => body.replace(newDigest, oldDigest),
    () => values(cell, '1.2.2', newDigest),
    () => values(cell, '1.2.3', newDigest),
    () => values(cell, '01.2.4', newDigest),
    () => values(cell, '1.2.4-rc.1', newDigest),
    body => body.replace(newDigest, 'sha256:short'),
    body => body.replace(newDigest, `sha256:${'A'.repeat(64)}`),
    body => body.replace(newDigest, `"${newDigest}"`),
    body => body.replace(`    imageDigest: ${newDigest}\n`, ''),
    body => body.replace('    imageTag: 1.2.4\n', '    imageTag: 1.2.4\n    # gap\n'),
  ];
  for (const mutate of mutations) {
    const f = fixture(t); f.write(path, mutate(values(cell, '1.2.4', newDigest))); denied(f);
  }
});

test('previous release must already have complete valid matching pins', t => {
  for (const body of [values().replace(`    imageDigest: ${oldDigest}\n`, ''),
    values().replace(oldDigest, 'sha256:bad'), values().replace('imageTag: 1.2.3', 'imageTag: 1.2.2')]) {
    const f = fixture(t, {body}); f.roll(); denied(f, 'invalid-or-incomplete-release-pins');
  }
});

test('all non-pin bytes including settings, comments, and other image pins stay exact', t => {
  for (const mutation of [
    body => body.replace('enabled: true', 'enabled: false'),
    body => body.replace('18.8.0', '18.9.0'),
    body => body.replace('Preserve operator', 'Change operator'),
    body => body.replace('billing:\n', 'billing: \n'),
    body => body.replace('https://self.example.invalid', 'https://another.example.invalid'),
  ]) {
    const f = fixture(t); f.write(path, mutation(values(cell, '1.2.4', newDigest))); denied(f);
  }
});

test('duplicate, quoted, aliased, and noncanonical mapping structures fail closed', t => {
  for (const mutate of [
    body => `${body}apps:\n  witselfServer:\n    imageTag: 9.9.9\n`,
    body => body.replace('apps:', '"apps":'),
    body => body.replace('  witselfServer:', '  witselfServer: &shared'),
    body => body.replace('  witselfServer:', '  "witselfServer":'),
    body => body.replace('    imageTag: 1.2.4', '    imageTag: 1.2.4\n    imageTag: 1.2.4'),
    body => `${body}---\napps: {}\n`,
    body => body.replaceAll('\n', '\r\n'),
    body => body.slice(0, -1),
  ]) {
    const f = fixture(t); f.write(path, mutate(values(cell, '1.2.4', newDigest))); denied(f);
  }
});

test('one existing catalog-registered cell only', t => {
  const unknown = fixture(t, {catalogCell: 'different-cell'}); unknown.roll(); denied(unknown, 'unregistered-cell');
  const mismatch = fixture(t, {body: values('different-cell')}); mismatch.roll(); denied(mismatch, 'cell-identity-mismatch');
  const both = fixture(t); both.roll(); both.write(valuesPath(second), values(second, '1.2.4', newDigest)); denied(both, 'mixed-or-multiple-cell-changes');
  const added = fixture(t); added.write(valuesPath('new-cell'), values('new-cell', '1.2.4', newDigest)); added.git('add', '--all'); denied(added, 'not-existing-file-modification');
  const removed = fixture(t); removed.git('rm', path); denied(removed, 'not-existing-file-modification');
  const renamed = fixture(t); renamed.git('mv', path, '.gitops/cells/renamed.yaml'); denied(renamed, 'not-existing-file-modification');
});

test('mode changes, symlinks, and hidden index state fail closed', t => {
  const mode = fixture(t); mode.roll(); chmodSync(join(mode.cwd, path), 0o755); mode.git('config', 'core.filemode', 'false'); denied(mode, 'file-mode-changed');
  const toolMode = fixture(t); toolMode.roll(); chmodSync(join(toolMode.cwd, 'scripts/tool.mjs'), 0o755); toolMode.git('config', 'core.filemode', 'false'); denied(toolMode, 'hidden-working-tree-change');
  const link = fixture(t); rmSync(join(link.cwd, path)); symlinkSync('elsewhere', join(link.cwd, path)); denied(link);
  for (const flag of ['--assume-unchanged', '--skip-worktree']) {
    const hidden = fixture(t); hidden.roll(); hidden.git('update-index', flag, 'scripts/tool.mjs'); hidden.write('scripts/tool.mjs', 'hidden unsafe change\n'); denied(hidden, 'hidden-or-unmerged-index-state');
  }
});

test('ignored installed dependencies do not count as source changes', t => {
  const f = fixture(t); f.roll(); f.write('node_modules/installed/metadata.json', '{}\n');
  assert.equal(f.classify().roll_only, true);
});

test('version comparison is numeric and does not lose large integer precision', t => {
  for (const [before, after, allowed] of [['1.2.9', '1.2.10', true], ['1.2.10', '1.2.9', false],
    ['1.999.999', '2.0.0', true], ['1.2.9007199254740992', '1.2.9007199254740993', true]]) {
    const f = fixture(t, {body: values(cell, before)}); f.write(path, values(cell, after, newDigest));
    assert.equal(f.classify().roll_only, allowed);
  }
});

test('committed deletion and readdition cannot masquerade as an ordinary roll', t => {
  const f = fixture(t);
  f.git('rm', path); f.commit('delete values');
  f.roll(); f.commit('readd rolled values');
  denied(f, 'not-existing-file-modification');
});

test('committed pin reversal is rejected even when another roll restores final pins', t => {
  const f = fixture(t); f.roll(); f.commit('roll');
  f.write(path, values()); f.commit('reverse pins');
  f.roll(); f.commit('restore roll');
  denied(f, 'reversed-release-pins');
});

test('nonlinear branch history and unmerged index never qualify', t => {
  const f = fixture(t);
  f.git('switch', '--quiet', '-c', 'side');
  f.roll(); f.commit('side roll');
  f.git('switch', '--quiet', 'main');
  f.git('-c', 'commit.gpgsign=false', 'merge', '--quiet', '--no-ff', 'side', '-m', 'merge roll');
  denied(f, 'nonlinear-branch');
  const conflict = fixture(t);
  conflict.git('switch', '--quiet', '-c', 'side'); conflict.roll(); conflict.commit('side roll');
  conflict.git('switch', '--quiet', 'main'); conflict.write(path, values(cell, '1.2.5', `sha256:${'c'.repeat(64)}`)); conflict.commit('other roll');
  assert.throws(() => conflict.git('-c', 'commit.gpgsign=false', 'merge', '--quiet', 'side'));
  denied(conflict, 'hidden-or-unmerged-index-state');
});

test('invalid UTF-8 and duplicate catalog identities fail closed', t => {
  const f = fixture(t); f.roll();
  writeFileSync(join(f.cwd, path), Buffer.concat([Buffer.from(values(cell, '1.2.4', newDigest)), Buffer.from([0xff, 0x0a])]));
  denied(f, 'comparison-unavailable');
  const duplicate = fixture(t);
  duplicate.write('.gitops/cells/catalog.yaml', `schema: witself.gitops-cells.v1\ncells:\n  ${cell}:\n    cloud: civo\n  ${cell}:\n    cloud: other\n`);
  const baseline = duplicate.commit('duplicate baseline'); duplicate.git('update-ref', 'refs/remotes/origin/main', baseline);
  duplicate.roll(); denied(duplicate, 'noncanonical-generated-values');
});


test('integrity allows visible edits for either lane without loosening roll scope', t => {
  const f = fixture(t); f.roll();
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: true});
  f.write('scripts/tool.mjs', 'visible tooling change\n');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: true});
  denied(f, 'mixed-or-multiple-cell-changes');
});

test('integrity rejects Git repository, index, object, and configuration redirection', t => {
  const f = fixture(t); f.roll();
  const cli = `import {checkLocalIntegrity} from ${JSON.stringify(new URL('./local-roll-scope.mjs', import.meta.url).href)}; console.log(JSON.stringify(checkLocalIntegrity()));`;
  for (const name of ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR', 'GIT_OBJECT_DIRECTORY',
    'GIT_ALTERNATE_OBJECT_DIRECTORIES', 'GIT_REPLACE_REF_BASE', 'GIT_GRAFT_FILE', 'GIT_CONFIG_COUNT',
    'GIT_CONFIG_PARAMETERS', 'GIT_CONFIG_GLOBAL', 'GIT_CONFIG_SYSTEM']) {
    const output = execFileSync(process.execPath, ['--input-type=module', '-e', cli], {cwd: f.cwd,
      encoding: 'utf8', env: {...process.env, [name]: name === 'GIT_CONFIG_COUNT' ? '0' : '/redirected'}});
    assert.deepEqual(JSON.parse(output), {safe: false, reason: 'redirected-git-environment'}, name);
  }
});

test('integrity rejects replacement refs, grafted history, and a different repository root', t => {
  const replaced = fixture(t); replaced.roll(); const target = replaced.commit('replacement object');
  replaced.git('replace', replaced.base, target);
  assert.deepEqual(checkLocalIntegrity(replaced.cwd), {safe: false, reason: 'replacement-history'});
  denied(replaced, 'replacement-history');
  const grafted = fixture(t); grafted.write('.git/info/grafts', `${grafted.base}\n`); grafted.roll();
  assert.deepEqual(checkLocalIntegrity(grafted.cwd), {safe: false, reason: 'grafted-history'});
  const root = fixture(t);
  assert.deepEqual(checkLocalIntegrity(join(root.cwd, 'scripts')), {safe: false, reason: 'repository-root-mismatch'});
});

test('a clean filter cannot hide changed application source beside a visible fast-lane edit', t => {
  const f = fixture(t);
  f.write('.git/info/attributes', 'scripts/tool.mjs filter=mask\n');
  f.git('config', 'filter.mask.clean', "printf 'original tooling\\n'");
  f.roll(); f.write('scripts/tool.mjs', 'hidden application mutation\n');
  assert.equal(f.git('diff', '--name-only', '--', 'scripts/tool.mjs'), '', 'fixture must actually hide the source change');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: false, reason: 'hidden-working-tree-change'});
  denied(f, 'hidden-working-tree-change');
});

test('restored file timestamps and ignored ctime cannot hide changed source', t => {
  const f = fixture(t);
  f.git('config', 'core.trustctime', 'false'); f.git('config', 'core.checkStat', 'minimal');
  const source = join(f.cwd, 'scripts/tool.mjs');
  const past = new Date(Date.now() - 120000);
  utimesSync(source, past, past); f.git('update-index', '--refresh');
  const before = statSync(source);
  f.write('scripts/tool.mjs', 'modified tooling\n');
  assert.equal(statSync(source).size, before.size);
  utimesSync(source, before.atime, before.mtime);
  f.roll();
  assert.equal(f.git('diff', '--name-only', '--', 'scripts/tool.mjs'), '', 'fixture must actually hide the source change');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: false, reason: 'hidden-working-tree-change'});
  denied(f, 'hidden-working-tree-change');
});

test('configured FSMonitor falls back instead of trusting another classifier cache', t => {
  const f = fixture(t); f.roll(); f.git('config', 'core.fsmonitor', 'true');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: false, reason: 'fsmonitor-configuration'});
  f.git('config', 'core.fsmonitor', 'false');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: true});
});


test('pre-fetch context guard separates repository identity from ordinary full-path fallback', t => {
  const f = fixture(t); f.roll();
  assert.deepEqual(checkLocalContext(f.cwd), {safe: true});
  f.git('update-index', '--assume-unchanged', 'scripts/tool.mjs');
  assert.deepEqual(checkLocalContext(f.cwd), {safe: true});
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: false, reason: 'hidden-or-unmerged-index-state'});
  assert.deepEqual(checkLocalContext(join(f.cwd, 'scripts')), {safe: false, reason: 'repository-root-mismatch'});
  const cli = `import {checkLocalContext} from ${JSON.stringify(new URL('./local-roll-scope.mjs', import.meta.url).href)}; console.log(JSON.stringify(checkLocalContext()));`;
  const output = execFileSync(process.execPath, ['--input-type=module', '-e', cli], {cwd: f.cwd,
    encoding: 'utf8', env: {...process.env, GIT_INDEX_FILE: '/redirected'}});
  assert.deepEqual(JSON.parse(output), {safe: false, reason: 'redirected-git-environment'});
});

test('brochure-visible edit cannot mask a tool mode change ignored by ordinary Git', t => {
  const f = fixture(t);
  f.write('web/product/content.mjs', 'base brochure\n'); const base = f.commit('brochure baseline');
  f.git('update-ref', 'refs/remotes/origin/main', base);
  f.write('web/product/content.mjs', 'new brochure\n');
  chmodSync(join(f.cwd, 'scripts/tool.mjs'), 0o755); f.git('config', 'core.filemode', 'false');
  assert.equal(f.git('diff', '--name-only'), 'web/product/content.mjs');
  assert.deepEqual(checkLocalIntegrity(f.cwd), {safe: false, reason: 'hidden-working-tree-change'});
});
