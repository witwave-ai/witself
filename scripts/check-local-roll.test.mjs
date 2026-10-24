import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {checkLocalRoll, GateFailure, RollIneligible, validateRun, validateJobs, requiredJobs, requiredPublishSteps, main} from './check-local-roll.mjs';
import {cleanupGitFixture, configureFixtureGit, fixtureGitArgs} from './test-support/git-fixture.mjs';

const sha = 'a'.repeat(40);
const oldSHA = 'b'.repeat(40);
const digest = `sha256:${'c'.repeat(64)}`;
const image = 'ghcr.io/witwave-ai/images/witself-server';
const mirrorCells = ['civo-fixture-use1-serving'];
const baseRun = () => ({id: 123, run_attempt: 1, run_number: 20, path: '.github/workflows/release.yml',
  event: 'push', head_branch: 'v1.2.3', head_sha: sha, repository: {full_name: 'witwave-ai/witself'}, status: 'completed', conclusion: 'success'});
const jobPages = () => {
  const jobs = [...requiredJobs, ...mirrorCells.map(cell => `mirror-postgresql (${cell})`)].map((name, i) => ({id: i + 1, name, run_id: 123, run_attempt: 1, head_sha: sha,
    status: 'completed', conclusion: 'success', steps: name === 'goreleaser' ? requiredPublishSteps.map(name => ({name, status: 'completed', conclusion: 'success'})) : []}));
  return [{total_count: jobs.length, jobs}];
};

test('release run requires exact workflow, repository, tag, source, event, and settled success', () => {
  assert.equal(validateRun(baseRun(), 'v1.2.3', sha).id, 123);
  for (const patch of [{status: 'in_progress'}, {conclusion: 'failure'}, {conclusion: 'cancelled'}, {event: 'workflow_dispatch'},
    {path: '.github/workflows/ci.yml'}, {head_branch: 'main'}, {head_sha: oldSHA}, {repository: {full_name: 'another/repository'}}, {run_attempt: 0}]) {
    assert.throws(() => validateRun({...baseRun(), ...patch}, 'v1.2.3', sha), {code: 'release-run-not-verified'});
  }
});

test('jobs must be complete and current; required jobs and publication steps cannot skip', () => {
  const run = baseRun();
  assert.equal(validateJobs(jobPages(), run, mirrorCells), requiredJobs.length + mirrorCells.length);
  const split = jobPages()[0];
  assert.equal(validateJobs([{total_count: split.total_count, jobs: split.jobs.slice(0, 5)}, {total_count: split.total_count, jobs: split.jobs.slice(5)}], run, mirrorCells), requiredJobs.length + mirrorCells.length);
  const mutate = change => { const pages = jobPages(); change(pages); assert.throws(() => validateJobs(pages, run, mirrorCells)); };
  mutate(p => p[0].jobs.pop());
  mutate(p => p[0].jobs[0].id = p[0].jobs[1].id);
  mutate(p => p[0].jobs[0].run_attempt = 2);
  mutate(p => p[0].jobs[0].head_sha = oldSHA);
  mutate(p => p[0].jobs[0].status = 'in_progress');
  mutate(p => p[0].jobs[0].conclusion = 'failure');
  mutate(p => p[0].jobs[0].conclusion = 'skipped');
  mutate(p => p[0].jobs[0].name = 'unrelated');
  mutate(p => p[0].jobs.find(j => j.name === 'goreleaser').steps.pop());
  mutate(p => p[0].jobs.find(j => j.name === 'goreleaser').steps[0].conclusion = 'skipped');
  mutate(p => p[0].jobs.find(j => j.name === 'goreleaser').steps[0].conclusion = 'failure');
  mutate(p => { p[0].jobs.pop(); p[0].total_count--; });
  mutate(p => p[0].jobs.at(-1).conclusion = 'skipped');
  mutate(p => { p[0].jobs.push({...p[0].jobs[0], id: 1000, name: 'unexpected skipped job', conclusion: 'skipped'}); p[0].total_count++; });
  assert.throws(() => validateJobs(jobPages(), run, []), {code: 'release-mirror-contract-invalid'});
});

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'witself-roll-gate-test-'));
  t.after(() => cleanupGitFixture(cwd, t.name));
  const git = (...args) => execFileSync('git', fixtureGitArgs(args), {cwd, stdio: ['ignore', 'pipe', 'pipe']}).toString().trim();
  git('init', '-q');
  configureFixtureGit(git);
  git('config', 'user.email', 'fixture@example.invalid');
  git('config', 'user.name', 'Fixture');
  const cell = 'civo-fixture-use1-serving';
  const values = `.gitops/cells/${cell}/values.yaml`;
  mkdirSync(join(cwd, '.gitops/cells', cell), {recursive: true});
  writeFileSync(join(cwd, '.gitops/cells/catalog.yaml'), `schema: witself.gitops-cells.v1\ncells:\n  ${cell}:\n    cloud: civo\n`);
  const body = (version, hash) => `cell:\n  name: ${cell}\napps:\n  witselfServer:\n    enabled: true\n    chartVersion: ${version}\n    imageTag: ${version}\n    imageDigest: sha256:${hash.repeat(64)}\n`;
  writeFileSync(join(cwd, values), body('1.2.2', 'd'));
  mkdirSync(join(cwd, 'internal/store/migrations'), {recursive: true});
  writeFileSync(join(cwd, 'internal/store/migrations/0001.sql'), '-- baseline\n');
  writeFileSync(join(cwd, 'internal/store/migrate.go'), 'package store\n');
  writeFileSync(join(cwd, 'internal/store/export.go'), 'package store\n');
  git('add', '.'); git('commit', '-qm', 'baseline');
  const baseRef = git('rev-parse', 'HEAD');
  writeFileSync(join(cwd, values), body('1.2.3', 'c'));
  return {cwd, baseRef, values, cell, git};
}

function runner(f, patch = {}) {
  const calls = [];
  const state = {
    run: baseRun(), pages: jobPages(), digest,
    release: {tag_name: 'v1.2.3', draft: false, prerelease: false, published_at: '2026-01-01T00:00:00Z'},
    metadata: [{name: 'witself-server', version: '1.2.3', appVersion: '1.2.3'}],
    child: [{image: {tag: '1.2.3', digest}, worker: {enabled: true}}],
    application: [{kind: 'Application', metadata: {name: 'witself-server'}, spec: {
      source: {repoURL: 'ghcr.io/witwave-ai/charts', chart: 'witself-server', targetRevision: '1.2.3', helm: {values: 'child-values'}},
      destination: {namespace: 'witself'}}}],
    workloads: [{kind: 'Deployment', spec: {template: {spec: {containers: [{name: 'witself-server', image: `${image}@${digest}`}]}}}},
      {kind: 'Deployment', spec: {template: {spec: {containers: [{name: 'witself-worker', image: `${image}@${digest}`}]}}}}],
    ...patch,
  };
  let listingReads = 0;
  const run = (file, args, options) => {
    calls.push({file, args, options});
    if (state.fail?.(file, args)) throw new Error(`private provider body ${digest} secret`);
    if (file === 'git') {
      if (args[0] === 'show' && args[1].endsWith(':images/postgresql/mirror.json')) return JSON.stringify({schema_version: 1, cells: {[mirrorCells[0]]: {}}});
      if (args[0] === 'rev-parse') {
        const ref = args.at(-1);
        if (ref === 'refs/tags/v1.2.3^{commit}') return sha;
        if (ref === 'refs/tags/v1.2.2^{commit}') return oldSHA;
        if (ref.includes(':internal/store/')) {
          if (state.schemaInvalid && ref.startsWith(sha)) return 'invalid-object';
          return state.schemaChanged && ref.startsWith(sha) ? 'e'.repeat(40) : 'f'.repeat(40);
        }
      }
      if (args[0] === 'diff' && args[1] === '--check') return '';
    }
    if (file === 'gh') {
      if (args[0] === 'attestation') return 'verified';
      const endpoint = args.at(-1);
      if (endpoint.endsWith('/commits/v1.2.3')) return JSON.stringify({sha: state.remoteChanged ? oldSHA : sha});
      if (endpoint.endsWith('/commits/v1.2.2')) return JSON.stringify({sha: oldSHA});
      if (endpoint.includes('/releases/tags/')) return JSON.stringify(state.release);
      if (endpoint.includes('/actions/workflows/')) {
        listingReads++;
        if (state.changeOnFinal && listingReads === 2) writeFileSync(join(f.cwd, f.values), readFileSync(join(f.cwd, f.values), 'utf8') + '# concurrent edit\n');
        return JSON.stringify({total_count: 1, workflow_runs: [state.newest ?? state.run]});
      }
      if (endpoint.endsWith('/actions/runs/123')) return JSON.stringify(state.run);
      if (endpoint.endsWith('/actions/runs/124')) return JSON.stringify({...state.run, id: 124, status: 'in_progress', conclusion: null});
      if (endpoint.includes('/attempts/')) return JSON.stringify(state.pages);
    }
    if (file === 'cosign') return 'verified';
    if (file === 'bash') return args[0].includes('resolve-server') ? state.digest : '';
    if (file === 'helm') {
      if (args[0] === 'pull' || args[0] === 'lint') return '';
      if (args[0] === 'show') return 'chart-metadata';
      if (args[0] === 'template') return args[1] === 'witself-apps' ? 'apps-render' : 'workloads-render';
    }
    if (file === 'ruby') return JSON.stringify({'chart-metadata': state.metadata, 'child-values': state.child,
      'apps-render': state.application, 'workloads-render': state.workloads}[options.input]);
    throw new Error(`unexpected fixture command ${file}`);
  };
  return {run, calls, state};
}

test('focused path verifies one signed image and one attested chart, selected render, and generated values', async t => {
  const f = fixture(t);
  const {run, calls} = runner(f);
  const result = await checkLocalRoll({...f, run});
  assert.equal(result.passed, true);
  assert.equal(result.release_run_id, 123);
  assert.equal(JSON.stringify(result).includes(digest), false);
  assert.equal(JSON.stringify(result).includes(sha), false);
  const pull = calls.filter(c => c.file === 'helm' && c.args[0] === 'pull');
  assert.equal(pull.length, 1);
  assert.equal(existsSync(pull[0].args.at(-1)), false, 'owned scratch removed');
  assert.ok(calls.some(c => c.file === 'cosign' && c.args.includes('--certificate-github-workflow-sha') && c.args.includes(sha) && c.args.at(-1) === `${image}@${digest}`));
  assert.ok(calls.some(c => c.file === 'gh' && c.args[0] === 'attestation' && c.args.includes('--source-digest') && c.args.includes('--deny-self-hosted-runners')));
  assert.equal(calls.some(c => ['docker', 'kubectl', 'make'].includes(c.file)), false);
  assert.ok(calls.some(c => c.file === 'bash' && c.args[0] === 'scripts/gitops-cell-values.sh' && c.args[1] === '--check'));
  assert.ok(calls.every(c => !('WITSELF_TEST_STRIPE_SECRET_KEY' in c.options.env)));
});

const rejections = [
  ['remote release tag differs', {remoteChanged: true}, 'release-tag-identity-mismatch'],
  ['migration bytes/orchestration changed', {schemaChanged: true}, 'release-may-advance-schema'],
  ['invalid migration source object', {schemaInvalid: true}, 'release-schema-source-invalid'],
  ['missing migration source', {fail: (file, args) => file === 'git' && args[0] === 'rev-parse' && args.at(-1).includes(':internal/store/')}, 'local-release-source-unavailable'],
  ['draft release', {release: {tag_name: 'v1.2.3', draft: true, prerelease: false, published_at: '2026-01-01'}}, 'stable-release-not-published'],
  ['latest run pending despite older success', {newest: {...baseRun(), id: 124, run_number: 21}}, 'release-run-not-verified'],
  ['failed latest release run', {run: {...baseRun(), conclusion: 'failure'}}, 'release-run-not-verified'],
  ['manifest mismatch', {digest: `sha256:${'e'.repeat(64)}`}, 'release-image-pin-mismatch'],
  ['invalid image signature', {fail: file => file === 'cosign'}, 'release-image-signature-invalid'],
  ['invalid chart provenance', {fail: (file, args) => file === 'gh' && args[0] === 'attestation'}, 'release-chart-provenance-invalid'],
  ['chart version mismatch', {metadata: [{name: 'witself-server', version: '1.2.2', appVersion: '1.2.3'}]}, 'release-chart-version-mismatch'],
  ['generator drift', {fail: (file, args) => file === 'bash' && args[0].includes('gitops-cell')}, 'generated-values-drift'],
  ['wrong selected child image', {child: [{image: {repository: image, tag: '1.2.2', digest}}]}, 'selected-cell-image-mismatch'],
  ['explicit noncanonical image repository', {child: [{image: {repository: 'example.invalid/image', tag: '1.2.3', digest}, worker: {enabled: true}}]}, 'selected-cell-image-mismatch'],
  ['missing worker pin', {workloads: [{kind: 'Deployment', spec: {template: {spec: {containers: [{name: 'witself-server', image: `${image}@${digest}`}]}}}}]}, 'released-child-image-not-pinned'],
  ['released chart render failure', {fail: (file, args) => file === 'helm' && args[0] === 'template' && args[1] === 'witself-server'}, 'released-child-render-failed'],
  ['local values changed during final remote check', {changeOnFinal: true}, 'local-roll-changed-during-check'],
];
for (const [name, patch, code] of rejections) test(`refuses ${name} without leaking provider details`, async t => {
  const f = fixture(t);
  const {run} = runner(f, patch);
  await assert.rejects(checkLocalRoll({...f, run}), error => {
    assert.ok(error instanceof GateFailure);
    assert.equal(error instanceof RollIneligible, code === 'release-may-advance-schema');
    assert.match(error.message, /focused approval refused/);
    return error.code === code && !error.message.includes(digest) && !error.message.includes('private provider body');
  });
});

test('mixed source change refuses before any GitHub/registry calls', async t => {
  const f = fixture(t);
  writeFileSync(join(f.cwd, 'untracked.go'), 'package changed\n');
  const {run, calls} = runner(f);
  await assert.rejects(checkLocalRoll({...f, run}), {code: 'scope-not-pin-only'});
  assert.equal(calls.length, 0);
});

test('CLI accepts no receipt or evidence bypass option', async () => {
  const errors = [];
  const prior = console.error;
  console.error = message => errors.push(message);
  try {
    assert.equal(await main(['--receipt', '/tmp/green.json']), 1);
    assert.match(errors[0], /focused approval refused/);
  } finally { console.error = prior; }
});

test('standalone CLI still refuses confirmed schema differences; only the selector may choose full checks', t => {
  const f = fixture(t);
  const bin = mkdtempSync(join(tmpdir(), 'witself-roll-cli-shim-'));
  try {
    const changedValues = readFileSync(join(f.cwd, f.values), 'utf8');
    f.git('restore', '--worktree', '--', f.values);
    f.git('tag', 'v1.2.2', f.baseRef);
    f.git('checkout', '-q', '--detach', f.baseRef);
    writeFileSync(join(f.cwd, 'internal/store/migrations/0001.sql'), '-- changed release migration\n');
    f.git('commit', '-qam', 'schema release');
    const target = f.git('rev-parse', 'HEAD');
    f.git('tag', 'v1.2.3');
    f.git('checkout', '-q', '--detach', f.baseRef);
    writeFileSync(join(f.cwd, f.values), changedValues);
    writeFileSync(join(bin, 'gh'), `#!/usr/bin/env node\nconst refs=${JSON.stringify({'repos/witwave-ai/witself/commits/v1.2.2': f.baseRef, 'repos/witwave-ai/witself/commits/v1.2.3': target})}; const sha=refs[process.argv.at(-1)]; if(!sha) process.exit(2); process.stdout.write(JSON.stringify({sha}));\n`, {mode: 0o700});
    assert.throws(() => execFileSync(process.execPath, [fileURLToPath(new URL('./check-local-roll.mjs', import.meta.url)), '--base', f.baseRef], {
      cwd: f.cwd, env: {...process.env, PATH: `${bin}:${process.env.PATH}`}, stdio: ['ignore', 'pipe', 'pipe'],
    }), error => error.status === 1 && error.stderr.toString().includes('release-may-advance-schema; focused approval refused') && !error.stdout.toString().includes('PASS'));
  } finally { rmSync(bin, {recursive: true, force: true}); }
});
