import test from 'node:test';
import assert from 'node:assert/strict';
import {runLocalChecks, selectLocalGate, main} from './check-local.mjs';
import {RollIneligible} from './check-local-roll.mjs';

function fixture(overrides = {}) {
  const calls = [], logs = [];
  const options = {
    cwd: '/fixture', env: {MARKER: 'retained'},
    run: (file, args, options) => calls.push({file, args, options}),
    productClassifier: () => ({website_only: false}),
    rollClassifier: () => ({roll_only: false}),
    integrityChecker: () => ({safe: true}),
    contextChecker: () => ({safe: true}),
    verifyRoll: async () => { throw new Error('unexpected release verification'); },
    captureDocsState: () => 'unchanged-fixture',
    log: text => logs.push(text), ...overrides,
  };
  return {options, calls, logs};
}

test('ordinary or uncertain changes use the full gate with required database and Node tests', async () => {
  for (const rollClassifier of [() => ({roll_only: false}), () => { throw new Error('missing base'); }]) {
    const f = fixture({rollClassifier});
    assert.deepEqual(await runLocalChecks(f.options), {passed: true, gate: 'application'});
    assert.deepEqual(f.calls.map(c => [c.file, ...c.args]), [['git', 'fetch', 'origin'], ['make', 'check']]);
    assert.deepEqual(f.calls[1].options.env, {MARKER: 'retained', WITSELF_TEST_REQUIRE_DATABASE: '1', WITSELF_TEST_REQUIRE_NODE: '1'});
  }
});

test('brochure-only selection installs locked tooling and runs the existing website gate', async () => {
  const f = fixture({productClassifier: () => ({website_only: true}), rollClassifier: () => { throw new Error('must not inspect roll'); }});
  assert.equal((await runLocalChecks(f.options)).gate, 'website');
  assert.deepEqual(f.calls.map(c => [c.file, ...c.args]), [
    ['git', 'fetch', 'origin'], ['npm', 'ci', '--ignore-scripts', '--prefix', 'infra/cloudflare/control-plane'], ['make', 'check-product-site'],
  ]);
});

test('audited documentation uses its shared gate without database or Worker setup', async () => {
  const f = fixture({productClassifier: () => ({website_only: false, docs_only: true}), rollClassifier: () => assert.fail('must not inspect release roll')});
  assert.deepEqual(await runLocalChecks(f.options), {passed: true, gate: 'documentation'});
  assert.deepEqual(f.calls.map(c => [c.file, ...c.args]), [['git', 'fetch', 'origin'], ['make', 'check-docs']]);
  assert.deepEqual(f.calls[1].options.env, {MARKER: 'retained', GITHUB_ACTIONS: 'false'});
});

test('local documentation approval cannot inherit a narrower GitHub event comparison', async () => {
  const env = {MARKER: 'retained', GITHUB_ACTIONS: 'true', GITHUB_EVENT_NAME: 'push', GITHUB_EVENT_PATH: '/unrelated/event.json'};
  const f = fixture({env, productClassifier: () => ({docs_only: true})});
  assert.deepEqual(await runLocalChecks(f.options), {passed: true, gate: 'documentation'});
  assert.deepEqual(f.calls[1].options.env, {...env, GITHUB_ACTIONS: 'false'});
  assert.equal(env.GITHUB_ACTIONS, 'true', 'Local dispatch must not mutate its caller environment');
});

test('changed content cannot pass merely because it is still in the documentation lane', async () => {
  let snapshots = 0;
  const f = fixture({productClassifier: () => ({docs_only: true}), captureDocsState: () => String(++snapshots)});
  await assert.rejects(runLocalChecks(f.options), /document validation state changed/);
  assert.equal(f.logs.some(l => l.includes('PASS')), false);
});

test('failed documentation validation blocks approval without a fallback pass', async () => {
  const f = fixture({productClassifier: () => ({docs_only: true}), run: file => { if (file === 'make') throw new Error('broken document link'); }});
  await assert.rejects(runLocalChecks(f.options), /broken document link/);
  assert.equal(f.logs.some(l => l.includes('PASS')), false);
});

test('pin-only selection must pass release verification at the fixed refreshed base', async () => {
  let request;
  const f = fixture({rollClassifier: () => ({roll_only: true}), verifyRoll: async value => { request = value; return {passed: true}; }});
  assert.equal((await runLocalChecks(f.options)).gate, 'release-roll');
  assert.deepEqual(request, {baseRef: 'origin/main', cwd: '/fixture'});
  assert.equal(f.calls.length, 1);
  assert.equal(f.logs.at(-1), 'local check: PASS release-roll');
});

test('unverified artifacts fail without laundering the refusal through application tests', async () => {
  for (const verifyRoll of [async () => { throw new Error('bad provenance'); }, async () => ({passed: false}), async () => undefined]) {
    const f = fixture({rollClassifier: () => ({roll_only: true}), verifyRoll});
    await assert.rejects(runLocalChecks(f.options));
    assert.deepEqual(f.calls.map(c => c.file), ['git']);
    assert.equal(f.logs.some(l => l.includes('PASS')), false);
  }
});

test('confirmed migration changes select the full local path without focused approval', async () => {
  const f = fixture({rollClassifier: () => ({roll_only: true}), verifyRoll: async () => { throw new RollIneligible(); }});
  assert.deepEqual(await runLocalChecks(f.options), {passed: true, gate: 'application'});
  assert.deepEqual(f.calls.map(c => [c.file, ...c.args]), [['git', 'fetch', 'origin'], ['make', 'check']]);
  assert.equal(f.calls[1].options.env.WITSELF_TEST_REQUIRE_DATABASE, '1');
  assert.equal(f.calls[1].options.env.WITSELF_TEST_REQUIRE_NODE, '1');
  assert.equal(f.logs.at(-1), 'local check: PASS application');
});

test('hidden Git state cannot select either short lane', async () => {
  for (const integrityChecker of [() => ({safe: false}), () => ({safe: 'true'}), () => { throw new Error('unknown'); }]) {
    const f = fixture({integrityChecker, productClassifier: () => assert.fail('hidden comparison'), rollClassifier: () => assert.fail('hidden comparison')});
    assert.equal((await runLocalChecks(f.options)).gate, 'application');
  }
});

test('integrity is rechecked before focused approval', async () => {
  let calls = 0;
  const f = fixture({productClassifier: () => ({website_only: true}), integrityChecker: () => ({safe: ++calls === 1})});
  await assert.rejects(runLocalChecks(f.options), /checkout changed/);
  assert.equal(f.logs.some(l => l.includes('PASS')), false);
});

test('failed fetch stops before classification or checks', async () => {
  const f = fixture({run: () => { throw new Error('fetch failed'); }, productClassifier: () => assert.fail('stale comparison')});
  await assert.rejects(runLocalChecks(f.options));
  assert.deepEqual(f.logs, []);
});

test('redirected or unavailable repository context refuses before fetch can mutate refs', async () => {
  for (const contextChecker of [() => ({safe: false}), () => ({safe: 'true'}), () => { throw new Error('unavailable'); }]) {
    const f = fixture({contextChecker});
    await assert.rejects(runLocalChecks(f.options));
    assert.deepEqual(f.calls, []);
    assert.deepEqual(f.logs, []);
  }
});

test('check failures never emit a passing verdict', async () => {
  const f = fixture({run: file => { if (file === 'make') throw new Error('tests failed'); }});
  await assert.rejects(runLocalChecks(f.options));
  assert.equal(f.logs.some(l => l.includes('PASS')), false);
});

test('truthy eligibility and unsupported base overrides cannot grant the shorter path', async () => {
  assert.equal(selectLocalGate('/fixture', () => ({website_only: 'true'}), () => ({roll_only: 1}), () => ({safe: true})), 'application');
  assert.equal(selectLocalGate('/fixture', () => ({docs_only: 'true'}), () => ({roll_only: false}), () => ({safe: true})), 'application');
  await assert.rejects(main(['--base', 'HEAD']), /usage/);
});
