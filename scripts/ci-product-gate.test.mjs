import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {assertGate} from './ci-product-gate.mjs';

function needs(lane = 'brochure') {
  const application = lane !== 'brochure';
  const website = lane !== 'application';
  return {
    changes: {result: 'success', outputs: {
      application: String(application), website: String(website), website_only: String(!application),
    }},
    website: {result: website ? 'success' : 'skipped'},
    'app-first': {result: application ? 'success' : 'skipped'},
    'app-second': {result: application ? 'success' : 'skipped'},
  };
}

test('only successful applicable gates and deliberately skipped inapplicable gates pass', () => {
  assert.equal(assertGate(needs('brochure')), 'brochure-only');
  assert.equal(assertGate(needs('application')), 'application');
  assert.equal(assertGate(needs('mixed')), 'application');
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
  for (const lane of ['brochure', 'application', 'mixed']) {
    for (const name of ['application', 'website', 'website_only']) {
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

test('inconsistent scope cannot waive application or website validation', () => {
  for (const application of ['true', 'false']) {
    for (const website of ['true', 'false']) {
      for (const websiteOnly of ['true', 'false']) {
        if (application !== websiteOnly && (websiteOnly !== 'true' || website === 'true')) continue;
        const value = needs();
        value.changes.outputs = {application, website, website_only: websiteOnly};
        assert.throws(() => assertGate(value));
      }
    }
  }
});

test('each website and application dependency must match its exact applicable result', () => {
  for (const lane of ['brochure', 'application', 'mixed']) {
    for (const key of ['website', 'app-first', 'app-second']) {
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

test('absent website and absent application result objects fail closed', () => {
  for (const lane of ['brochure', 'application', 'mixed']) {
    for (const replacement of [undefined, null, {}]) {
      const value = needs(lane);
      value.website = replacement;
      assert.throws(() => assertGate(value));
      value.website = needs(lane).website;
      value['app-first'] = replacement;
      assert.throws(() => assertGate(value));
    }
    const value = needs(lane);
    delete value['app-first'];
    delete value['app-second'];
    assert.throws(() => assertGate(value));
  }
});

test('all observed application dependencies are checked, including extra dependencies', () => {
  for (const lane of ['brochure', 'application', 'mixed']) {
    const value = needs(lane);
    value.extra = {result: lane === 'brochure' ? 'success' : 'skipped'};
    assert.throws(() => assertGate(value));
    value.extra.result = lane === 'brochure' ? 'skipped' : 'success';
    assert.doesNotThrow(() => assertGate(value));
  }
});

test('CLI exits unsuccessfully for malformed or missing dependency input', () => {
  const script = fileURLToPath(new URL('./ci-product-gate.mjs', import.meta.url));
  for (const input of ['', 'not json', 'null', '{}', JSON.stringify({...needs(), website: {result: 'failure'}})]) {
    assert.throws(() => execFileSync(process.execPath, [script], {
      env: {...process.env, NEEDS_JSON: input}, stdio: ['ignore', 'pipe', 'pipe'],
    }));
  }
  const output = execFileSync(process.execPath, [script], {
    env: {...process.env, NEEDS_JSON: JSON.stringify(needs())}, encoding: 'utf8',
  });
  assert.match(output, /PASS: brochure-only change; website validation succeeded/);
});
