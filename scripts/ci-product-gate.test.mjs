import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {assertGate} from './ci-product-gate.mjs';

const lanes = {
  brochure: {application: false, docs: false, docs_index: false, docs_only: false, website: true, website_only: true},
  docs: {application: false, docs: true, docs_index: false, docs_only: true, website: false, website_only: false},
  index: {application: false, docs: true, docs_index: true, docs_only: true, website: false, website_only: false},
  application: {application: true, docs: false, docs_index: false, docs_only: false, website: false, website_only: false},
  mixed: {application: true, docs: false, docs_index: false, docs_only: false, website: true, website_only: false},
  'docs-code': {application: true, docs: true, docs_index: false, docs_only: false, website: false, website_only: false},
  'docs-brochure': {application: true, docs: true, docs_index: false, docs_only: false, website: true, website_only: false},
  unknown: {application: true, docs: true, docs_index: true, docs_only: false, website: true, website_only: false},
};

function needs(lane = 'brochure') {
  const {application, docs, website} = lanes[lane];
  return {
    changes: {result: 'success', outputs: Object.fromEntries(Object.entries(lanes[lane]).map(([key, value]) => [key, String(value)]))},
    website: {result: website ? 'success' : 'skipped'},
    docs: {result: docs ? 'success' : 'skipped'},
    'app-first': {result: application ? 'success' : 'skipped'},
    'app-second': {result: application ? 'success' : 'skipped'},
  };
}

test('only successful applicable gates and deliberately skipped inapplicable gates pass', () => {
  assert.equal(assertGate(needs('brochure')), 'brochure-only');
  assert.equal(assertGate(needs('docs')), 'docs-only');
  assert.equal(assertGate(needs('index')), 'docs-only');
  for (const lane of ['application', 'mixed', 'docs-code', 'docs-brochure', 'unknown']) {
    assert.equal(assertGate(needs(lane)), 'application');
  }
});

test('missing, failed, cancelled, or skipped classification never passes', () => {
  for (const input of [null, undefined, [], 'success', {}]) assert.throws(() => assertGate(input));
  for (const status of [undefined, null, '', 'failure', 'cancelled', 'skipped', 'neutral', 'timed_out']) {
    const value = needs();
    value.changes.result = status;
    assert.throws(() => assertGate(value));
  }
});

test('missing outputs or booleans other than exact strings fail closed', () => {
  for (const lane of Object.keys(lanes)) {
    for (const name of Object.keys(lanes[lane])) {
      for (const invalid of [undefined, null, true, false, 0, 1, '', 'TRUE', 'False', 'unknown']) {
        const value = needs(lane);
        value.changes.outputs[name] = invalid;
        assert.throws(() => assertGate(value), `${lane}: ${name}=${String(invalid)}`);
      }
    }
  }
  const value = needs();
  delete value.changes.outputs;
  assert.throws(() => assertGate(value));
});

test('the complete scope truth table cannot waive application, documentation, or website validation', () => {
  const names = ['application', 'docs', 'docs_index', 'docs_only', 'website', 'website_only'];
  for (let mask = 0; mask < 2 ** names.length; mask++) {
    const scope = Object.fromEntries(names.map((name, index) => [name, Boolean(mask & (1 << index))]));
    const value = needs();
    value.changes.outputs = Object.fromEntries(Object.entries(scope).map(([name, flag]) => [name, String(flag)]));
    value.website.result = scope.website ? 'success' : 'skipped';
    value.docs.result = scope.docs ? 'success' : 'skipped';
    value['app-first'].result = value['app-second'].result = scope.application ? 'success' : 'skipped';
    const validApplication = scope.application && !scope.docs_only && !scope.website_only && (!scope.docs_index || scope.docs);
    const validDocs = !scope.application && scope.docs && scope.docs_only && !scope.website && !scope.website_only;
    const validBrochure = !scope.application && !scope.docs && !scope.docs_index && !scope.docs_only && scope.website && scope.website_only;
    const label = JSON.stringify(scope);
    if (validApplication || validDocs || validBrochure) assert.doesNotThrow(() => assertGate(value), label);
    else assert.throws(() => assertGate(value), label);
  }
});

test('each documentation, website, and application dependency must match its exact applicable result', () => {
  for (const lane of Object.keys(lanes)) {
    for (const key of ['website', 'docs', 'app-first', 'app-second']) {
      const expected = needs(lane)[key].result;
      for (const status of [undefined, null, '', 'success', 'failure', 'cancelled', 'skipped', 'neutral', 'timed_out', 'in_progress']) {
        if (status === expected) continue;
        const value = needs(lane);
        value[key].result = status;
        assert.throws(() => assertGate(value), `${lane}: ${key} unexpectedly ${String(status)}`);
      }
    }
  }
});

test('absent documentation, website, and application result objects fail closed', () => {
  for (const lane of Object.keys(lanes)) {
    for (const replacement of [undefined, null, {}]) {
      for (const key of ['website', 'docs', 'app-first']) {
        const value = needs(lane);
        value[key] = replacement;
        assert.throws(() => assertGate(value), `${lane}: missing ${key}`);
      }
    }
    const value = needs(lane);
    delete value['app-first'];
    delete value['app-second'];
    assert.throws(() => assertGate(value));
  }
});

test('all observed application dependencies are checked, including extra dependencies', () => {
  for (const lane of Object.keys(lanes)) {
    const value = needs(lane);
    value.extra = {result: lanes[lane].application ? 'skipped' : 'success'};
    assert.throws(() => assertGate(value));
    value.extra.result = lanes[lane].application ? 'success' : 'skipped';
    assert.doesNotThrow(() => assertGate(value));
  }
});

test('CLI exits unsuccessfully for malformed or missing dependency input', () => {
  const script = fileURLToPath(new URL('./ci-product-gate.mjs', import.meta.url));
  for (const input of ['', 'not json', 'null', '{}',
    JSON.stringify({...needs(), website: {result: 'failure'}}),
    JSON.stringify({...needs('docs'), docs: {result: 'failure'}})]) {
    assert.throws(() => execFileSync(process.execPath, [script], {
      env: {...process.env, NEEDS_JSON: input}, stdio: ['ignore', 'pipe', 'pipe'],
    }));
  }
  for (const [lane, message] of [
    ['brochure', /PASS: brochure-only change; website validation succeeded/],
    ['docs', /PASS: docs-only change; documentation validation succeeded/],
    ['docs-code', /PASS: all required application gates and any affected documentation and website validation succeeded/],
  ]) {
    const output = execFileSync(process.execPath, [script], {
      env: {...process.env, NEEDS_JSON: JSON.stringify(needs(lane))}, encoding: 'utf8',
    });
    assert.match(output, message);
  }
});
