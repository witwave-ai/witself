import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';
import {checkGoTestJSON} from './ci-go-test-json-check.mjs';

const script = fileURLToPath(new URL('./ci-go-test-json-check.mjs', import.meta.url));
// Independent of the implementation: dropping a required package must fail.
const packages = [
  'github.com/witwave-ai/witself/internal/localcell',
  'github.com/witwave-ai/witself/internal/runtimeconfig',
  'github.com/witwave-ai/witself/internal/tools/localcellharness',
];
const diagnostic = 'FAIL: invalid or incomplete Go test JSON stream\n';
const failureDiagnostic = 'FAIL: Go test failures\n';

function event(Action, Package, Test, extra = {}) {
  return {Action, Package, ...(Test === undefined ? {} : {Test}), ...extra};
}

function packageEvents(Package, action = 'pass') {
  return [event('start', Package), event('run', Package, 'TestFixture'),
    event(action, Package, 'TestFixture'), event(action === 'fail' ? 'fail' : 'pass', Package)];
}

function stream(events) {
  return `${events.map(value => JSON.stringify(value)).join('\n')}\n`;
}

function summary(counts = {}) {
  return `${packages.flatMap(name => {
    const {pass = 1, skip = 0, fail = 0, names = [], failedNames = [], why = {}} = counts[name] ?? {};
    const outcomes = [...names.map(testName => [testName, 'skip']), ...failedNames.map(testName => [testName, 'fail'])]
      .sort(([left], [right]) => left.localeCompare(right));
    return [`package ${name} pass=${pass} skip=${skip} fail=${fail}`, ...outcomes.flatMap(([testName, action]) => [
      `${action} ${name} ${testName}`, ...(action === 'fail' ? [`why ${name} ${testName} :: ${why[testName] ?? '(unclassified)'}`] : []),
    ])];
  }).join('\n')}\n`;
}

const passing = packages.flatMap(name => packageEvents(name));

test('native service capability results stay explicit and require the selected persistence proof', async () => {
  const name = 'TestNativeServiceLifecycle';
  for (const [outcomes, valid] of [
    [['PASS service_manager_available', 'PASS service_manager_persistence'], true],
    [['NOT_RUN service_manager_unavailable', 'PASS service_foreground_persistence'], true],
    [['NOT_RUN service_adapter_unsupported'], true],
    [['NOT_RUN native_postgres_unconfigured'], false],
    [[], false],
    [['NOT_RUN service_manager_unavailable'], false],
    [['PASS service_manager_available'], false],
    [['PASS service_manager_available', 'PASS service_foreground_persistence'], false],
    [['NOT_RUN service_adapter_unsupported', 'PASS service_manager_available', 'PASS service_manager_persistence'], false],
  ]) {
    const events = [event('start', packages[0]), event('run', packages[0], name),
      ...outcomes.map(message => event('output', packages[0], name, {Output: `    service_native_test.go:10: ${message}\n`})),
      event('pass', packages[0], name), event('pass', packages[0])];
    const result = await checkGoTestJSON([stream(events)], [name]);
    assert.equal(result.valid, valid);
    for (const outcome of outcomes) assert(result.lines.includes(`capability ${packages[0]} ${name} :: ${outcome}`));
  }
});

test('native service capability output cannot disclose arbitrary diagnostics', async () => {
  const name = 'TestNativeServiceLifecycle';
  const events = [event('start', packages[0]), event('run', packages[0], name),
    event('output', packages[0], name, {Output: '    service_native_test.go:10: NOT_RUN service_manager_unavailable private-marker\n'}),
    event('pass', packages[0], name), event('pass', packages[0])];
  const result = await checkGoTestJSON([stream(events)], [name]);
  assert.equal(result.valid, false);
  assert(!result.lines.join('\n').includes('private-marker'));
});

const serviceTest = 'TestNativeServiceLifecycle';
const serviceReceiptArgs = ['--diagnose', '--require', serviceTest];
const serviceReceipts = [
  'service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true',
  'service_shutdown phase=control operation=stop journal_pending=true journal_read_ok=false',
];

function serviceReceiptEvents(outputs, action = 'pass', name = serviceTest, packageName = packages[0]) {
  return [event('start', packageName), event('run', packageName, name),
    event('output', packageName, name, {Output: '    service_native_test.go:10: NOT_RUN service_manager_unavailable\n'}),
    event('output', packageName, name, {Output: '    service_native_test.go:11: PASS service_foreground_persistence\n'}),
    ...outputs.map(Output => event('output', packageName, name, {Output})),
    event(action, packageName, name), event(action === 'fail' ? 'fail' : 'pass', packageName)];
}

function serviceReceiptSummary(receipts = []) {
  return [`package ${packages[0]} pass=1 skip=0 fail=0`,
    `capability ${packages[0]} ${serviceTest} :: NOT_RUN service_manager_unavailable`,
    `capability ${packages[0]} ${serviceTest} :: PASS service_foreground_persistence`,
    ...receipts.map(receipt => `diagnose ${packages[0]} ${serviceTest} :: ${receipt}`), ''].join('\n');
}

test('passing native service proof projects two shutdown receipts verbatim after the Go prefix', () => {
  assertFixture(script, {args: serviceReceiptArgs, status: 0,
    input: stream(serviceReceiptEvents([
      `    service_native_test.go:20: ${serviceReceipts[0]}\r\n`,
      `    service_native_diagnostics_test.go:21: ${serviceReceipts[1]}\n`,
      '    service_native_test.go:22: private-fixture-marker\n',
    ])), output: serviceReceiptSummary(serviceReceipts)});
});

test('passing native service receipts reject extra, missing and non-vocabulary fields without rejecting the stream', async () => {
  const receipt = serviceReceipts[0];
  const invalid = [
    `${receipt} private-fixture-marker`, `private-fixture-marker ${receipt}`,
    `${receipt} site=health_ready`, `${receipt} `,
    receipt.replace(' phase=idle', ''), receipt.replace(' operation=none', ''),
    receipt.replace(' journal_pending=false', ''), receipt.replace(' journal_read_ok=true', ''),
    receipt.replace('phase=idle', 'phase=unknown'), receipt.replace('operation=none', 'operation=unknown'),
    receipt.replace('journal_pending=false', 'journal_pending=unknown'), receipt.replace('journal_read_ok=true', 'journal_read_ok=unknown'),
    receipt.replace('journal_pending=false', 'journal_pending=False'), receipt.replace('journal_read_ok=true', 'journal_read_ok=True'),
  ];
  const result = await checkGoTestJSON([stream(serviceReceiptEvents([
    ...invalid.map(line => `    service_native_test.go:20: ${line}\n`),
    `    service_native_test.go:21: ${receipt}\n`,
  ]))], [serviceTest], {diagnose: true});
  assert.equal(result.valid, true);
  assert.equal(`${result.lines.join('\n')}\n`, serviceReceiptSummary([receipt]));
});

test('passing native service receipts accept each closed vocabulary value', async () => {
  for (const phase of ['startup', 'tick', 'control', 'idle']) {
    for (const operation of ['none', 'reconcile', 'repair', 'stop', 'start', 'health']) {
      const receipts = ['true', 'false'].flatMap(pending => ['true', 'false'].map(read =>
        `service_shutdown phase=${phase} operation=${operation} journal_pending=${pending} journal_read_ok=${read}`));
      const result = await checkGoTestJSON([stream(serviceReceiptEvents(receipts.map(line => `${line}\n`)))], [serviceTest], {diagnose: true});
      assert.equal(result.valid, true);
      assert.equal(`${result.lines.join('\n')}\n`, serviceReceiptSummary(receipts));
    }
  }
});

test('passing native service proof caps receipts at four across output events without deduplicating stops', () => {
  const receipts = [...serviceReceipts, ...serviceReceipts, ...serviceReceipts];
  assertFixture(script, {args: serviceReceiptArgs, status: 0,
    input: stream(serviceReceiptEvents([
      `${receipts[0]}\n${receipts[1]}\n`, ...receipts.slice(2).map(line => `${line}\n`),
    ])), output: serviceReceiptSummary(receipts.slice(0, 4))});
});

test('failed native service receipts retain the existing uncapped failure diagnostic path only', async () => {
  const outputs = [...serviceReceipts, ...serviceReceipts, ...serviceReceipts,
    `${serviceReceipts[0]} private-fixture-marker`, 'service_shutdown phase=unknown']
    .map(line => `    service_native_test.go:20: ${line}\n`);
  const result = await checkGoTestJSON([stream(serviceReceiptEvents(outputs, 'fail'))], [serviceTest], {diagnose: true});
  assert.equal(result.valid, false);
  assert.equal(result.reason, failureDiagnostic.trimEnd());
  assert.deepEqual(result.lines.filter(line => line.startsWith('diagnose ')), [
    '    service_native_test.go:10: NOT_RUN service_manager_unavailable',
    '    service_native_test.go:11: PASS service_foreground_persistence',
    ...outputs.map(line => line.trimEnd()),
  ].map(line => `diagnose ${packages[0]} ${serviceTest} :: ${line}`));
});

test('shutdown receipt projection remains exclusive to the selected passing native proof', async () => {
  const output = `    service_native_test.go:20: ${serviceReceipts[0]}\n`;
  for (const name of ['TestFixture', `${serviceTest}/Child`]) {
    const result = await checkGoTestJSON([stream(serviceReceiptEvents([output], 'pass', name))], [name], {diagnose: true});
    assert.equal(result.valid, true);
    assert(!result.lines.some(line => line.startsWith('diagnose ')));
  }
  for (const packageName of packages.slice(1)) {
    const result = await checkGoTestJSON([stream(serviceReceiptEvents([output], 'pass', serviceTest, packageName))],
      [serviceTest], {diagnose: true, packageName});
    assert(!result.lines.some(line => line.startsWith('diagnose ')));
  }
  const skipped = await checkGoTestJSON([stream(serviceReceiptEvents([output], 'skip'))], [serviceTest], {diagnose: true});
  assert(!skipped.lines.some(line => line.startsWith('diagnose ')));
  assertFixture(script, {args: ['--require', serviceTest], status: 0,
    input: stream(serviceReceiptEvents([output])), output: serviceReceiptSummary()});
});

test('three-package unit mode remains byte-identical with native proof receipt noise', async () => {
  const events = [...serviceReceiptEvents([]), ...packages.slice(1).flatMap(name => packageEvents(name))];
  const noisy = events.flatMap(value => value.Action === 'run'
    ? [value, event('output', value.Package, value.Test, {Output: `${serviceReceipts[0]}\n${serviceReceipts[1]} private-fixture-marker\n`})] : [value]);
  for (const diagnose of [false, true]) {
    const expected = await checkGoTestJSON([stream(events)], [], {diagnose});
    const result = await checkGoTestJSON([stream(noisy)], [], {diagnose});
    assert.equal(result.valid, true);
    assert.deepEqual(result, expected);
    assert(!result.lines.some(line => line.startsWith('diagnose ')));
  }
});

for (const mutation of [
  {name: 'shutdown receipt leading anchor removed', from: '= /^service_shutdown phase=', to: '= /service_shutdown phase=',
    outputs: [`private-fixture-marker ${serviceReceipts[0]}\n`]},
  {name: 'shutdown receipt trailing anchor removed', from: 'journal_read_ok=(true|false)$/;', to: 'journal_read_ok=(true|false)/;',
    outputs: [`${serviceReceipts[0]} private-fixture-marker\n`]},
  {name: 'fifth shutdown receipt allowed', from: 'state.shutdownReceipts.length === 4', to: 'state.shutdownReceipts.length === 5',
    outputs: Array.from({length: 5}, () => `${serviceReceipts[0]}\n`)},
  {name: 'failed native receipts also projected as success receipts',
    from: "if (action === 'pass' && testName === serviceProofTest)", to: "if (['pass', 'fail'].includes(action) && testName === serviceProofTest)",
    action: 'fail', outputs: [`    service_native_test.go:20: ${serviceReceipts[0]}\n`]},
]) {
  test(`mutation killed: ${mutation.name}`, () => {
    withDiagnosticSources(target => {
      // Keep the original failure path enrolled in the copied checker, so a
      // missing source allowlist cannot accidentally kill the failure mutant.
      writeFileSync(join(target, '..', '..', 'internal', 'localcell', 'service_native_test.go'), 'package localcell\n');
      const input = stream(serviceReceiptEvents(mutation.outputs, mutation.action));
      const expected = spawnSync(process.execPath, [target, ...serviceReceiptArgs], {input, encoding: 'utf8'});
      assert.equal(expected.status, mutation.action === 'fail' ? 1 : 0);
      const fixture = {args: serviceReceiptArgs, input, output: expected.stdout, stderr: expected.stderr,
        status: expected.status};
      assertFixture(script, fixture);
      const source = readFileSync(target, 'utf8');
      assert.equal(source.split(mutation.from).length, 2, 'mutation must target exactly one guard');
      writeFileSync(target, source.replace(mutation.from, mutation.to));
      assert.throws(() => assertFixture(target, fixture), {code: 'ERR_ASSERTION'});
    });
  });
}

test('Windows shutdown channel requirement needs its own observed persistence outcome', async () => {
  const name = 'TestNativeServiceLifecycle';
  const foreground = ['NOT_RUN service_manager_unavailable', 'PASS service_foreground_persistence'];
  for (const [outcomes, required, valid] of [
    [[...foreground, 'PASS service_shutdown_channel'], [name, 'service_shutdown_channel'], true],
    [foreground, [name, 'service_shutdown_channel'], false],
    [['NOT_RUN service_adapter_unsupported'], [name, 'service_shutdown_channel'], false],
    [['PASS service_manager_available', 'PASS service_manager_persistence', 'PASS service_shutdown_channel'], [name, 'service_shutdown_channel'], false],
    [[...foreground, 'PASS service_shutdown_channel'], ['service_shutdown_channel'], false],
    [[...foreground, 'PASS service_shutdown_channel'], [name, 'service_shutdown_channel', 'service_shutdown_channel'], false],
    [[...foreground, 'PASS service_shutdown_channel'], [name], true],
  ]) {
    const events = [event('start', packages[0]), event('run', packages[0], name),
      ...outcomes.map(message => event('output', packages[0], name, {Output: `    service_native_test.go:10: ${message}\n`})),
      event('pass', packages[0], name), event('pass', packages[0])];
    const result = await checkGoTestJSON([stream(events)], required);
    assert.equal(result.valid, valid);
  }
});
// Kept independent of the checker export: deleting a stage from the checker
// must not silently narrow the acceptance contract.
const managerStages = [
  'query_typed', 'registration', 'on_demand', 'readiness_write', 'same_boot_restart',
  'persisted_read', 'durable_stop', 'unregister', 'resources_closed', 'bystander_retained',
];
function managerEvents(stages = managerStages) {
  const name = serviceTest;
  return [event('start', packages[0]), event('run', packages[0], name),
    ...stages.flatMap(stage => [
      event('run', packages[0], `${name}/manager_${stage}`),
      event('pass', packages[0], `${name}/manager_${stage}`),
      event('output', packages[0], name, {Output: `    service_native_windows_task_test.go:10: PASS service_manager_stage=${stage}\n`}),
    ]),
    ...['PASS service_manager_available', 'PASS service_manager_persistence', 'PASS service_shutdown_channel']
      .map(Output => event('output', packages[0], name, {Output: `${Output}\n`})),
    event('pass', packages[0], name), event('pass', packages[0])];
}
const managerOptions = {managerRequired: true};
const managerRequirements = [serviceTest, 'service_shutdown_channel'];
const managerArgs = ['--manager-required', '--require', managerRequirements.join(',')];

function missingManagerEvents(outcomes) {
  return [event('start', packages[0]), event('run', packages[0], serviceTest),
    ...outcomes.map(Output => event('output', packages[0], serviceTest, {Output: `${Output}\n`})),
    event('pass', packages[0], serviceTest), event('pass', packages[0])];
}

for (const reason of ['windows_task_query_result_untyped', 'windows_task_interactive_token_unavailable']) {
  test(`manager-required projects approved missing receipt as NOT RUN: ${reason}`, async () => {
    const outcomes = ['NOT_RUN service_manager_receipt_missing', `NOT_RUN ${reason}`];
    const input = stream(missingManagerEvents(outcomes));
    const output = [`package ${packages[0]} pass=1 skip=0 fail=0`,
      ...outcomes.map(outcome => `capability ${packages[0]} ${serviceTest} :: ${outcome}`), ''].join('\n');
    assertFixture(script, {args: managerArgs, input, status: 0, output});
    const result = await checkGoTestJSON([input], managerRequirements, managerOptions);
    assert.equal(result.valid, true);
    assert.equal(result.lines.some(line => line.startsWith('receipt ') || line.includes('PASS service_manager_')), false);
    // Ordinary required-service mode still requires its existing proof outcome.
    assert.equal((await checkGoTestJSON([input], [serviceTest])).valid, false);
  });
}

test('manager-required refuses mixed duplicate malformed or attempted missing-receipt evidence', async () => {
  const missing = ['NOT_RUN service_manager_receipt_missing', 'NOT_RUN windows_task_query_result_untyped'];
  for (const outcomes of [
    missing.slice(0, 1), missing.slice(1),
    [...missing, missing[0]], [...missing, missing[1]],
    [...missing, 'NOT_RUN windows_task_interactive_token_unavailable'],
    [...missing, 'NOT_RUN windows_task_manager_unavailable'],
    ...['PASS service_manager_available', 'PASS service_manager_persistence',
      'PASS service_foreground_persistence', 'NOT_RUN service_manager_unavailable',
      'NOT_RUN service_adapter_unsupported', 'NOT_RUN native_postgres_unconfigured',
      'PASS service_shutdown_channel', 'PASS service_manager_stage=query_typed',
      'NOT_RUN windows_task_unknown', 'NOT_RUN windows_task_query_result_untyped extra',
      'NOT_RUN service_manager_receipt_missing extra', 'extra NOT_RUN service_manager_receipt_missing',
      'PASS service_manager_available extra'].map(extra => [...missing, extra]),
  ]) {
    const result = await checkGoTestJSON([stream(missingManagerEvents(outcomes))], managerRequirements, managerOptions);
    assert.equal(result.valid, false, 'mixed or malformed missing-manager evidence accepted');
  }
  for (const action of ['pass', 'fail', 'skip']) {
    const events = missingManagerEvents(missing);
    events.splice(events.length - 2, 0, event('run', packages[0], `${serviceTest}/manager_query_typed`),
      event(action, packages[0], `${serviceTest}/manager_query_typed`));
    assert.equal((await checkGoTestJSON([stream(events)], managerRequirements, managerOptions)).valid, false,
      'attempted manager proof accepted as unavailable');
  }
  const failed = missingManagerEvents(missing).map(value => value.Action === 'pass' ? {...value, Action: 'fail'} : value);
  assert.equal((await checkGoTestJSON([stream(failed)], managerRequirements, managerOptions)).valid, false);
  const mixedSuccess = managerEvents();
  mixedSuccess.splice(mixedSuccess.length - 2, 0, ...missing.map(Output =>
    event('output', packages[0], serviceTest, {Output: `${Output}\n`})));
  const result = spawnSync(process.execPath, [script, ...managerArgs], {input: stream(mixedSuccess), encoding: 'utf8'});
  assert.equal(result.status, 1, 'missing-manager marker plus manager success was accepted');
  assert.equal(result.stderr, diagnostic);
});

test('manager-required rejects misplaced evidence in package sibling and child output', async () => {
  const missing = ['NOT_RUN service_manager_receipt_missing', 'NOT_RUN windows_task_query_result_untyped'];
  for (const name of [undefined, 'TestFixture', `${serviceTest}/unrelated`]) {
    for (const proof of [missingManagerEvents(missing), managerEvents()]) {
      for (const message of [
        ...missing, 'NOT_RUN windows_task_interactive_token_unavailable',
        'PASS service_manager_available', 'PASS service_manager_persistence',
        'PASS service_manager_stage=query_typed', 'PASS service_foreground_persistence',
        'PASS service_shutdown_channel', 'NOT_RUN service_manager_unavailable',
        'NOT_RUN service_adapter_unsupported', 'NOT_RUN native_postgres_unconfigured',
        'PASS service_manager_available extra', 'NOT_RUN windows_task_unknown',
      ]) {
        for (const prefix of ['', '    service_native_windows_task_test.go:10: ']) {
          const events = [...proof];
          events.splice(events.length - 2, 0,
            ...(name === undefined ? [] : [event('run', packages[0], name)]),
            event('output', packages[0], name, {Output: `${prefix}${message}\n`}),
            ...(name === undefined ? [] : [event('pass', packages[0], name)]));
          assert.equal((await checkGoTestJSON([stream(events)], managerRequirements, managerOptions)).valid, false,
            'misattributed manager evidence accepted');
        }
      }
      // A source filename containing windows_task is not itself evidence.
      const unrelated = [...proof];
      unrelated.splice(unrelated.length - 2, 0,
        ...(name === undefined ? [] : [event('run', packages[0], name)]),
        event('output', packages[0], name, {Output: '    service_native_windows_task_test.go:10: ordinary fixture output\n'}),
        ...(name === undefined ? [] : [event('pass', packages[0], name)]));
      assert.equal((await checkGoTestJSON([stream(unrelated)], managerRequirements, managerOptions)).valid, true);
    }
  }
});

test('manager-required accepts only complete ordered manager stages with passing child assertions', async () => {
  const result = await checkGoTestJSON([stream(managerEvents())], managerRequirements, managerOptions);
  assert.equal(result.valid, true);
  assert.equal(result.lines.filter(line => line.startsWith('receipt ')).length, managerStages.length);
  for (const stage of managerStages) {
    for (const action of ['skip', 'fail', undefined]) {
      const events = managerEvents().flatMap(value => value.Test === `${serviceTest}/manager_${stage}` && value.Action === 'pass'
        ? action ? [{...value, Action: action}] : [] : [value]);
      assert.equal((await checkGoTestJSON([stream(events)], managerRequirements, managerOptions)).valid, false, `${stage} ${action}`);
    }
    const missing = managerEvents().filter(value => !(value.Output ?? '').includes(`stage=${stage}\n`));
    assert.equal((await checkGoTestJSON([stream(missing)], managerRequirements, managerOptions)).valid, false, stage);
  }
});

test('manager-required rejects duplicate, reordered, unknown and malformed stage evidence', async () => {
  const fixture = managerEvents();
  const index = fixture.findIndex(value => (value.Output ?? '').includes('service_manager_stage='));
  for (const replacement of [
    [fixture[index], fixture[index]],
    [{...fixture[index], Output: 'PASS service_manager_stage=query_typed private-fixture-marker\n'}],
    [{...fixture[index], Output: 'PASS service_manager_stage=unapproved\n'}],
    [{...fixture[index], Output: 'PASS service_manager_stage=\n'}],
    [{...fixture[index], Output: 'PASS service_manager_stage query_typed\n'}],
  ]) {
    const events = [...fixture.slice(0, index), ...replacement, ...fixture.slice(index + 1)];
    const result = await checkGoTestJSON([stream(events)], managerRequirements, managerOptions);
    assert.equal(result.valid, false);
    assert(!result.lines.join('\n').includes('private-fixture-marker'));
  }
  const reversed = managerEvents([...managerStages].reverse());
  assert.equal((await checkGoTestJSON([stream(reversed)], managerRequirements, managerOptions)).valid, false);
});

test('manager-required rejects capability-only, foreground, unsupported and a duplicate outcome', async () => {
  for (const outcomes of [
    ['PASS service_manager_available'],
    ['NOT_RUN service_manager_unavailable', 'PASS service_foreground_persistence', 'PASS service_shutdown_channel'],
    ['NOT_RUN service_adapter_unsupported'],
    ['NOT_RUN service_manager_receipt_missing', 'NOT_RUN windows_task_manager_unavailable'],
  ]) {
    const events = [event('start', packages[0]), event('run', packages[0], serviceTest),
      ...outcomes.map(Output => event('output', packages[0], serviceTest, {Output: `${Output}\n`})),
      event('pass', packages[0], serviceTest), event('pass', packages[0])];
    const result = await checkGoTestJSON([stream(events)], managerRequirements, managerOptions);
    assert.equal(result.valid, false);
    for (const missing of outcomes.filter(outcome => outcome.includes('receipt_missing') || outcome.includes('windows_task_'))) {
      assert(result.lines.some(line => line.endsWith(missing)));
    }
  }
  const events = managerEvents();
  events.splice(events.length - 2, 0, event('output', packages[0], serviceTest, {Output: 'PASS service_manager_available\n'}));
  assert.equal((await checkGoTestJSON([stream(events)], managerRequirements, managerOptions)).valid, false);
});

test('manager-required cannot be enabled for unit mode or another package', async () => {
  for (const [required, options] of [
    [[], managerOptions], [[serviceTest], {...managerOptions, packageName: packages[1]}],
    [[serviceTest], {managerRequired: 'yes'}],
  ]) assert.equal((await checkGoTestJSON([stream(managerEvents())], required, options)).valid, false);
  const result = spawnSync(process.execPath, [script, '--manager-required'], {input: '', encoding: 'utf8'});
  assert.equal(result.status, 1);
  assert.equal(result.stderr, 'FAIL: invalid Go test checker arguments\n');
});

for (const mutation of [
  {name: 'manager child completion ignored',
    from: "state.tests.get(`${serviceProofTest}/manager_${stage}`) !== 'pass'", to: 'false',
    events: () => managerEvents().filter(value => value.Test !== `${serviceTest}/manager_registration`)},
  {name: 'manager receipt order ignored',
    from: 'stages[index] !== stage ||', to: '',
    events: () => managerEvents().map(value => value.Output?.includes('stage=query_typed')
      ? {...value, Output: value.Output.replace('query_typed', 'registration')}
      : value.Output?.includes('stage=registration') ? {...value, Output: value.Output.replace('registration', 'query_typed')} : value)},
  {name: 'manager extra receipt accepted',
    from: 'stages.length !== serviceManagerStages.length ||', to: '',
    events: () => { const events = managerEvents(); events.splice(events.length - 2, 0,
      event('output', packages[0], serviceTest, {Output: 'PASS service_manager_stage=query_typed\n'})); return events; }},
  {name: 'manager malformed extra receipt ignored',
    from: "else if (message.includes('service_manager_stage')) invalid = true;", to: '',
    events: () => { const events = managerEvents(); events.splice(events.length - 2, 0,
      event('output', packages[0], serviceTest, {Output: 'PASS service_manager_stage=query_typed extra\n'})); return events; }},
  {name: 'manager duplicate outcome accepted',
    from: 'if (managerRequired && state.serviceOutcomes.has(message)) invalid = true;', to: '',
    events: () => { const events = managerEvents(); events.splice(events.length - 2, 0,
      event('output', packages[0], serviceTest, {Output: 'PASS service_manager_available\n'})); return events; }},
]) {
  test(`mutation killed: ${mutation.name}`, () => {
    withDiagnosticSources(target => {
      const args = ['--manager-required', '--require', managerRequirements.join(',')];
      const input = stream(mutation.events());
      const expected = spawnSync(process.execPath, [target, ...args], {input, encoding: 'utf8'});
      assert.equal(expected.status, 1);
      const fixture = {args, input, status: expected.status, output: expected.stdout, stderr: expected.stderr};
      const source = readFileSync(target, 'utf8');
      assert.equal(source.split(mutation.from).length, 2, 'mutation must target exactly one manager guard');
      writeFileSync(target, source.replace(mutation.from, mutation.to));
      assert.throws(() => assertFixture(target, fixture), {code: 'ERR_ASSERTION'});
    });
  });
}

const failures = [
  {name: 'one failing test', input: stream(packages.flatMap((name, index) => packageEvents(name, index === 0 ? 'fail' : 'pass'))),
    output: summary({[packages[0]]: {pass: 0, fail: 1, failedNames: ['TestFixture']}}), stderr: failureDiagnostic},
  {name: 'test failure even with a passing package summary',
    input: stream(passing.map(value => value.Test && value.Action === 'pass' && value.Package === packages[0] ? {...value, Action: 'fail'} : value)),
    output: summary({[packages[0]]: {pass: 0, fail: 1, failedNames: ['TestFixture']}}), stderr: failureDiagnostic},
  {name: 'zero tests in one package',
    input: stream(passing.filter(value => value.Package !== packages[0] || value.Test === undefined)),
    output: summary({[packages[0]]: {pass: 0}})},
  {name: 'truncated final record delimiter', input: stream(passing).trimEnd(), output: summary()},
  {name: 'truncated extra record after passing summaries', input: stream(passing) + '{"Action":', output: summary()},
  {name: 'package without a summary', input: stream(passing.filter(value => value.Package !== packages[0] || value.Action !== 'pass' || value.Test !== undefined)), output: summary()},
];
const completeFailure = {
  input: stream([
    event('start', packages[0]), event('run', packages[0], 'TestParent'),
    event('run', packages[0], 'TestParent/child'),
    event('output', packages[0], 'TestParent/child', {Output: 'private-fixture-marker\n'}),
    event('fail', packages[0], 'TestParent/child'), event('fail', packages[0], 'TestParent'),
    event('run', packages[0], 'TestSkipped'), event('skip', packages[0], 'TestSkipped'),
    event('run', packages[0], 'TestUnrelated'), event('pass', packages[0], 'TestUnrelated'),
    event('output', packages[0], undefined, {Output: 'FAIL\n'}), event('fail', packages[0]),
    ...packageEvents(packages[1], 'fail'), ...packageEvents(packages[2]),
  ]),
  output: summary({
    [packages[0]]: {pass: 1, skip: 1, fail: 2, names: ['TestSkipped'], failedNames: ['TestParent', 'TestParent/child']},
    [packages[1]]: {pass: 0, fail: 1, failedNames: ['TestFixture']},
  }),
  stderr: failureDiagnostic,
};

function assertFixture(target, {input, output, status = 1, stderr = status === 0 ? '' : diagnostic, args = []}) {
  const result = spawnSync(process.execPath, [target, ...args], {input, encoding: 'utf8'});
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
  assert.equal(result.status, status);
  assert.equal(result.stdout, output);
  assert.equal(result.stderr, stderr);
}

test('fixture: all packages pass with exact exit code and summary', () => {
  assertFixture(script, {input: stream(passing), output: summary(), status: 0});
});

test('fixture: a skipped test is allowed and listed by name', () => {
  assertFixture(script, {
    input: stream(packages.flatMap((name, index) => packageEvents(name, index === 0 ? 'skip' : 'pass'))),
    output: summary({[packages[0]]: {pass: 0, skip: 1, names: ['TestFixture']}}), status: 0,
  });
});

for (const fixture of failures) {
  test(`fixture: ${fixture.name} fails with exact exit code and summary`, () => assertFixture(script, fixture));
}

test('fixture: a complete stream with several failures and Go FAIL output lists every failure with the failure reason', () => {
  assertFixture(script, completeFailure);
});

test('fixture: a failing stream still requires complete JSON records and package summaries', () => {
  assertFixture(script, {...completeFailure, input: completeFailure.input.trimEnd(), stderr: diagnostic});
});

test('interleaved packages, nested tests, parallel pauses, CRLF, and chunk boundaries preserve counts', async () => {
  const name = packages[0];
  const events = [event('start', name), event('run', name, 'TestParent'),
    ...packageEvents(packages[1]), event('run', name, 'TestParent/child'),
    event('pause', name, 'TestParent/child'), ...packageEvents(packages[2]),
    event('cont', name, 'TestParent/child'), event('pass', name, 'TestParent/child'),
    event('run', name, 'TestParent/skipped'), event('skip', name, 'TestParent/skipped'),
    event('pass', name, 'TestParent'), event('pass', name)];
  const input = stream(events).replaceAll('\n', '\r\n');
  const expected = summary({[name]: {pass: 2, skip: 1, names: ['TestParent/skipped']}});
  assertFixture(script, {input, output: expected, status: 0});
  const result = await checkGoTestJSON([...input]);
  assert.equal(result.valid, true);
  assert.equal(`${result.lines.join('\n')}\n`, expected);
});

test('output values and dependency build output are discarded', () => {
  const secretMarker = 'private-fixture-marker';
  const events = [{Action: 'build-output', ImportPath: 'dependency', Output: secretMarker},
    ...passing.flatMap(value => value.Action === 'run'
      ? [value, event('output', value.Package, value.Test, {Output: `${secretMarker}\n${'a'.repeat(64)}`})] : [value])];
  assertFixture(script, {input: stream(events), output: summary(), status: 0});
});

function withDiagnosticSources(callback) {
  const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-diagnostics-')));
  try {
    mkdirSync(join(directory, 'scripts'));
    const target = join(directory, 'scripts', 'ci-go-test-json-check.mjs');
    writeFileSync(target, readFileSync(script, 'utf8'));
    for (const name of ['localcell', 'runtimeconfig', 'tools/localcellharness']) {
      mkdirSync(join(directory, 'internal', name), {recursive: true});
    }
    writeFileSync(join(directory, 'internal', 'localcell', 'errors.go'), `package localcell
var allowed = errors.New("localcell: fixed private error")
var concatenated = errors.New("concatenated error" + suffix)
// errors.New("comment error")
var quoted = \`errors.New("quoted error")\`
var formatted = fmt.Errorf("formatted error")
var wrongFile = fixedCause("fixed cause from wrong file")
`);
    writeFileSync(join(directory, 'internal', 'localcell', 'fixed_cause.go'), `package localcell
var allowed = fixedCause("local_fixture_fixed_cause")
var concatenated = fixedCause("fixed concatenated cause") + suffix
var dynamic = fixedCause(value)
var nested = helper(fixedCause("fixed nested cause"))
// var comment = fixedCause("fixed comment cause")
var quoted = \`var quoted = fixedCause("fixed quoted cause")\`
`);
    writeFileSync(join(directory, 'internal', 'runtimeconfig', 'fixed_cause.go'), `package runtimeconfig
var wrongPackage = fixedCause("fixed cause from wrong package")
`);
    writeFileSync(join(directory, 'internal', 'runtimeconfig', 'policy_test.go'), `package runtimeconfig
func TestPolicy(t *testing.T) {
  t.Fatal("owner-is-admin-group")
  t.Errorf("ace-count")
  t.Fatal("assertion with 12 safe digits")
  t.Fatal("assertion with 123 unsafe digits")
  t.Fatal("private/path")
  t.Fatal("private\\\\path")
  t.Fatal("private=marker")
  t.Fatal("private: marker")
  t.Fatalf("private %s", value)
  t.Fatal("multiple arguments", value)
  // t.Fatal("comment assertion")
  t.Fatal("concatenated assertion" + value)
  t.Fatalf("abcdefghijkl: %v", value)
  t.Errorf("abcdefghijk: %v", value)
  t.Fatalf("safe formatted assertion: %v", value)
  t.Fatalf("safe formatted: detail: %v", value)
  t.Errorf("safe formatted assertion without colon %s", value)
  t.Fatalf("unsafe formatted/path: %v", value)
  t.Fatalf("unsafe formatted=value: %v", value)
  t.Fatalf("unsafe formatted 123 digits: %v", value)
  t.Fatal("nonformat method assertion: %v", value)
  t.Fatalf("concatenated formatted assertion: %v" + value)
  // t.Fatalf("comment formatted assertion: %v", value)
  _ = \`t.Fatalf("quoted formatted assertion: %v", value)\`
  _ = errors.New("test-only error")
}
`);
    callback(target);
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
}

function outputFailure(output, message = '(unclassified)', packageName = packages[1]) {
  return {input: stream(packages.flatMap(name => name === packageName
    ? [event('start', name), event('run', name, 'TestFixture'),
      ...output.map(Output => event('output', name, 'TestFixture', {Output})),
      event('fail', name, 'TestFixture'), event('fail', name)] : packageEvents(name))),
  output: summary({[packageName]: {pass: 0, fail: 1, failedNames: ['TestFixture'], why: {TestFixture: message}}}),
  stderr: failureDiagnostic};
}

function withClassSources(callback, {testSource = '', productionSource = '', otherSource = ''} = {}) {
  const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-classes-')));
  try {
    mkdirSync(join(directory, 'scripts'));
    const target = join(directory, 'scripts', 'ci-go-test-json-check.mjs');
    writeFileSync(target, readFileSync(script, 'utf8'));
    for (const name of ['localcell', 'runtimeconfig', 'tools/localcellharness']) {
      mkdirSync(join(directory, 'internal', name), {recursive: true});
    }
    // Exercise Windows source newlines as well as CRLF output events.
    writeFileSync(join(directory, 'internal', 'runtimeconfig', 'class_test.go'), `package runtimeconfig
func TestClass(t *testing.T) {
  t.Logf("fixture error class: %s", value)
  t.Logf("fixture other class=%s", value)
  t.Fatal("class fixture assertion failed")
  _ = "ok next final fourth sentinel=deadline_exceeded windows_errno=other path=journal"
  ${testSource}
}
`.replace(/\r?\n/g, '\r\n'));
    writeFileSync(join(directory, 'internal', 'runtimeconfig', 'class.go'), `package runtimeconfig\n${productionSource}\n`.replace(/\r?\n/g, '\r\n'));
    writeFileSync(join(directory, 'internal', 'localcell', 'class_test.go'), `package localcell\n${otherSource}\n`.replace(/\r?\n/g, '\r\n'));
    callback(target);
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
}

function classFailure(outputs, classes, reason = '(unclassified)', packageName = packages[1]) {
  const fixture = outputFailure(outputs, reason, packageName);
  const why = `why ${packageName} TestFixture :: ${reason}\n`;
  return {...fixture, output: fixture.output.replace(why,
    why + classes.map(message => `class ${packageName} TestFixture :: ${message}\n`).join(''))};
}

test('class diagnostics enroll colon and equals suffixes with safe fixed prefixes', () => {
  const prefixes = ['fixture error class: ', 'fixture other class=', 'abcde class=',
    'Class (safe), said "yes"; item_2!? class='];
  const testSource = prefixes.slice(2).map(prefix => `logger.Logf(${JSON.stringify(`${prefix}%s`)}, value)`).join('\n');
  withClassSources(target => {
    for (const prefix of prefixes) {
      assertFixture(target, classFailure([`    class_test.go:12: ${prefix}ok\r\n`], [`${prefix}ok`]));
    }
  }, {testSource});
});

test('class diagnostics reject unenrolled formats and preserve existing why classification', () => {
  const cases = [
    ['two directives class: %s%s', 'two directives class: ', 't.Logf'],
    ['integer format class: %d', 'integer format class: ', 't.Logf'],
    ['generic format class: %v', 'generic format class: ', 't.Logf'],
    ['width format class: %12s', 'width format class: ', 't.Logf'],
    ['escaped percent class: %%s', 'escaped percent class: ', 't.Logf'],
    ['abcd class=%s', 'abcd class=', 't.Logf'],
    ['unsafe/path class: %s', 'unsafe/path class: ', 't.Logf'],
    ['unsafe 123 class: %s', 'unsafe 123 class: ', 't.Logf'],
    [`unsafe ${'a'.repeat(64)} class: %s`, `unsafe ${'a'.repeat(64)} class: `, 't.Logf'],
    ['wrong suffix category: %s', 'wrong suffix category: ', 't.Logf'],
    ['trailing format class: %s tail', 'trailing format class: ', 't.Logf'],
    ['ordinary log class: %s', 'ordinary log class: ', 't.Log'],
    ['error method class: %s', 'error method class: ', 't.Errorf', 'error method class'],
  ];
  const extra = [
    ['concatenated format class: ', 't.Logf("concatenated format class: %s" + suffix, value)'],
    ['dynamic format class: ', 't.Logf(dynamicFormat, value)'],
    ['raw format class: ', 't.Logf(`raw format class: %s`, value)'],
    ['comment format class: ', '// t.Logf("comment format class: %s", value)'],
    ['block comment class: ', '/* t.Logf("block comment class: %s", value) */'],
    ['quoted format class: ', '_ = "t.Logf(\\"quoted format class: %s\\", value)"'],
    ['raw quoted class: ', '_ = `t.Logf("raw quoted class: %s", value)`'],
    ['missing comma class: ', 't.Logf("missing comma class: %s")'],
    ['nonidentifier receiver class: ', 'logger().Logf("nonidentifier receiver class: %s", value)'],
  ];
  const testSource = [...cases.map(([format, , method]) => `${method}(${JSON.stringify(format)}, value)`),
    ...extra.map(([, source]) => source)].join('\n');
  withClassSources(target => {
    for (const [, prefix, , why = '(unclassified)'] of cases) {
      assertFixture(target, outputFailure([`${prefix}ok\n`], why));
    }
    for (const [prefix] of extra) assertFixture(target, outputFailure([`${prefix}ok\n`]));
    assertFixture(target, outputFailure(['another package class: ok\n']));
    assertFixture(target, outputFailure(['production format class: ok\n']));
    assertFixture(target, outputFailure(['fixture error class: ok\n'], '(unclassified)', packages[0]));
  }, {testSource, otherSource: 'func other(t *testing.T) { t.Logf("another package class: %s", value); _ = "ok" }',
    productionSource: 'func other(t *testing.T) { t.Logf("production format class: %s", value) }'});
});

test('class diagnostics vocabulary uses only eligible package test literals and whole words', () => {
  const tooLongWord = 'q'.repeat(41);
  const tooLongLiteral = `longliteralprivate ${'x'.repeat(64)}`;
  const testSource = String.raw`
  _ = "joined-left split_word,equals=pair"
  _ = "concatword" + "concatsecond"
  // _ = "commentprivate"
  /* _ = "blockprivate" */
  _ = ` + '`rawprivate`' + String.raw`
  _ = "quoted \"insideprivate\""
  _ = "uppercaseprivate UPPER"
  _ = "digitprivate 7"
  _ = "slashprivate/path"
  _ = "colonprivate:path"
  _ = "newlineprivate\n"
  _ = "tabprivate\t"
  _ = "_invalidfirst"
  ` + `_ = ${JSON.stringify(tooLongWord)}\n_ = ${JSON.stringify(tooLongLiteral)}`;
  withClassSources(target => {
    for (const value of ['joined-left split_word,equals=pair', 'concatword concatsecond']) {
      assertFixture(target, classFailure([`fixture error class: ${value}\n`], [`fixture error class: ${value}`]));
    }
    for (const value of ['commentprivate', 'blockprivate', 'rawprivate', 'insideprivate', 'uppercaseprivate',
      'digitprivate', 'slashprivate', 'colonprivate', 'newlineprivate', 'tabprivate', 'invalidfirst', tooLongWord,
      'longliteralprivate', 'productionprivate', 'otherpackageprivate']) {
      assertFixture(target, classFailure([`fixture error class: ${value}\n`], ['fixture error class: (rejected)']));
    }
  }, {testSource, productionSource: 'var value = "productionprivate"', otherSource: 'var value = "otherpackageprivate"'});
});

test('class diagnostics print valid values verbatim and reject private values without fragments', () => {
  const value = 'sentinel=deadline_exceeded windows_errno=other path=journal';
  withClassSources(target => {
    assertFixture(target, classFailure([`fixture error class: ${value}\n`], [`fixture error class: ${value}`]));
    for (const rejected of ['outofvocabularyprivate', 'digitprivate7', 'Uppercaseprivate', 'slashprivate/pathprivate',
      'backslashprivate\\pathprivate', 'colonprivate:pathprivate', '', 'lengthprivate'.repeat(14), 'a'.repeat(64),
      'ok  next', 'ok, next', 'ok=', 'ok ', 'ok\tprivate', 'ok\rprivate', 'ok\u0000private']) {
      assertFixture(target, classFailure([`fixture error class: ${rejected}\n`], ['fixture error class: (rejected)']));
    }
  });
});

test('class diagnostics enforce the value length boundary even for enrolled words', () => {
  const forty = 'p'.repeat(40);
  const thirtySeven = 'q'.repeat(37);
  const value = [forty, forty, forty, thirtySeven].join(' ');
  assert.equal(value.length, 160);
  withClassSources(target => {
    assertFixture(target, classFailure([`fixture error class: ${value}\n`], [`fixture error class: ${value}`]));
    assertFixture(target, classFailure([`fixture error class: ${value} ok\n`], ['fixture error class: (rejected)']));
  }, {testSource: `_ = ${JSON.stringify(forty)}\n_ = ${JSON.stringify(thirtySeven)}`});
});

test('class diagnostics are failed-only and leave passing and skipped streams byte-identical', () => {
  withClassSources(target => {
    for (const action of ['pass', 'skip']) {
      const events = packages.flatMap(name => name === packages[1]
        ? [event('start', name), event('run', name, 'TestFixture'),
          event('output', name, 'TestFixture', {Output: 'fixture error class: ok\nfixture other class=outofvocabularyprivate\n'}),
          event(action, name, 'TestFixture'), event('pass', name)] : packageEvents(name));
      assertFixture(target, {input: stream(events), status: 0,
        output: action === 'pass' ? summary() : summary({[packages[1]]: {pass: 0, skip: 1, names: ['TestFixture']}})});
    }
  });
});

test('class diagnostics keep the first three distinct displayed lines across events and CRLF', () => {
  withClassSources(target => {
    assertFixture(target, classFailure([
      '    class_test.go:12: fixture error class: ok\r\nfixture error class: ok\r\n',
      'fixture other class=next\r\nfixture error class: ok\r\n',
      'fixture error class: final\r\nfixture error class: fourth\r\n',
    ], ['fixture error class: ok', 'fixture other class=next', 'fixture error class: final']));
    assertFixture(target, classFailure([
      'fixture error class: outofvocabularyprivate\r\nfixture error class: differentprivate\r\n',
      'fixture other class=next\r\nfixture error class: ok\r\nfixture error class: fourth\r\n',
    ], ['fixture error class: (rejected)', 'fixture other class=next', 'fixture error class: ok']));
  });
});

test('class diagnostics prefer the longest enrolled prefix when formats overlap', () => {
  withClassSources(target => {
    assertFixture(target, classFailure(['fixture error class: extra class: ok\n'],
      ['fixture error class: extra class: ok']));
  }, {testSource: 't.Logf("fixture error class: extra class: %s", value)'});
});

test('class diagnostics stay local to each failed test including nested failures', () => {
  withClassSources(target => {
    const packageName = packages[1];
    const names = ['TestParent', 'TestParent/child'];
    const events = packages.flatMap(name => name === packageName
      ? [event('start', name), event('run', name, names[0]), event('run', name, names[1]),
        event('output', name, names[0], {Output: 'fixture error class: ok\n'}),
        event('output', name, names[1], {Output: 'fixture error class: next\n'}),
        event('fail', name, names[1]), event('fail', name, names[0]), event('fail', name)] : packageEvents(name));
    let output = summary({[packageName]: {pass: 0, fail: 2, failedNames: names}});
    for (const [index, testName] of names.entries()) {
      const why = `why ${packageName} ${testName} :: (unclassified)\n`;
      output = output.replace(why, `${why}class ${packageName} ${testName} :: fixture error class: ${index === 0 ? 'ok' : 'next'}\n`);
    }
    assertFixture(target, {input: stream(events), output, stderr: failureDiagnostic});
  });
});

test('class diagnostics never replace why even when a formatted assertion shares the prefix', () => {
  withClassSources(target => {
    assertFixture(target, classFailure(['fixture error class: ok\n', 'class fixture assertion failed\n'],
      ['fixture error class: ok'], 'class fixture assertion failed'));
    assertFixture(target, classFailure(['fixture error class: ok\n'], ['fixture error class: ok']));
    assertFixture(target, classFailure(['class fixture assertion failed\n', 'fixture error class: ok\n'],
      ['fixture error class: ok'], 'class fixture assertion failed'));
  }, {testSource: 't.Errorf("fixture error class: %s", value)'});
});

test('class diagnostics enroll the real journal repair format and reject an unknown path word', () => {
  const name = 'TestJournalTornTailRepair/operation_done/partial';
  const prefix = 'journal repair error class: ';
  for (const [value, expected] of [
    ['sentinel=deadline_exceeded windows_errno=other path=journal', 'sentinel=deadline_exceeded windows_errno=other path=journal'],
    ['sentinel=deadline_exceeded windows_errno=other path=notasourcewordprivate', '(rejected)'],
  ]) {
    const events = packages.flatMap(packageName => packageName === packages[0]
      ? [event('start', packageName), event('run', packageName, name),
        event('output', packageName, name, {Output: `    journal_recovery_test.go:12: ${prefix}${value}\r\n`}),
        event('fail', packageName, name), event('fail', packageName)] : packageEvents(packageName));
    const output = summary({[packages[0]]: {pass: 0, fail: 1, failedNames: [name]}});
    const why = `why ${packages[0]} ${name} :: (unclassified)\n`;
    assertFixture(script, {input: stream(events),
      output: output.replace(why, `${why}class ${packages[0]} ${name} :: ${prefix}${expected}\n`), stderr: failureDiagnostic});
  }
});

test('why diagnostics accept only exact fixed errors and package-local literal assertions', () => {
  withDiagnosticSources(target => {
    for (const message of ['localcell: fixed private error', 'local_fixture_fixed_cause', 'owner-is-admin-group', 'ace-count', 'assertion with 12 safe digits']) {
      assertFixture(target, outputFailure([`    policy_test.go:123: ${message}\r\n`], message));
    }
    // A fixed helper error may propagate to another package; assertions may not.
    assertFixture(target, outputFailure(['localcell: fixed private error\n'], 'localcell: fixed private error', packages[2]));
    assertFixture(target, outputFailure(['owner-is-admin-group\n'], '(unclassified)', packages[0]));
  });
});

test('why diagnostics suppress unallowlisted, unsafe, interpolated, commented and test-only errors', () => {
  withDiagnosticSources(target => {
    for (const message of ['private-fixture-marker', 'assertion with 123 unsafe digits', 'private/path', 'private\\path',
      'private=marker', 'private: marker', 'private value', 'multiple arguments value', 'concatenated error',
      'comment error', 'quoted error', 'formatted error', 'comment assertion', 'concatenated assertion', 'test-only error',
      'fixed concatenated cause', 'fixed nested cause', 'fixed comment cause', 'fixed quoted cause',
      'fixed cause from wrong file', 'fixed cause from wrong package', 'local_fixture_fixed_cause private-fixture-marker',
      'owner-is-admin-group private-fixture-marker', 'private/path_test.go:12: owner-is-admin-group', 'a'.repeat(64)]) {
      assertFixture(target, outputFailure([`    policy_test.go:12: ${message}\n`]));
    }
  });
});

test('why diagnostics emit at most the first allowlisted output line per failed test', () => {
  withDiagnosticSources(target => {
    assertFixture(target, outputFailure(['private-fixture-marker\n    policy_test.go:12: ace-count\nowner-is-admin-group\n',
      'localcell: fixed private error\n'], 'ace-count'));
  });
});

test('why diagnostics from the actual packages recognize production errors and native assertions', () => {
  assertFixture(script, outputFailure(['    policy_test.go:12: canonical policy was rejected\n'], 'canonical policy was rejected'));
  assertFixture(script, outputFailure(['    fixture_test.go:12: local cell password file is unavailable or not private\n'],
    'local cell password file is unavailable or not private', packages[0]));
  for (const cause of ['local_migration_phase_required', 'local_runtime_login_owns_tables',
    'local_principal_layout_invalid', 'local_principal_provision_refused', 'local_privileged_sessions_present',
    'local_principal_repair_required', 'local_principal_credentials_refused']) {
    assertFixture(script, outputFailure([`    fixture_test.go:12: ${cause}\n`], cause, packages[0]));
    assertFixture(script, outputFailure([`    fixture_test.go:12: ${cause} private-fixture-marker\n`], '(unclassified)', packages[0]));
  }
});

test('Windows shutdown regression assertions classify every fixed failure without private details', async () => {
  // Independent of source extraction: losing a direct assertion literal must
  // fail this fixture instead of silently returning an unclassified CI reason.
  const cases = [
    ['service_failure_receipt_test.go', 'TestServiceFailureReceiptOnceActualControlPath', [
      'cannot create failure receipt fixture',
      'cannot secure failure receipt fixture',
      'cannot configure failure receipt fixture',
      'cannot prepare failure receipt fixture layout',
      'cannot write failure receipt fixture manifest',
      'failure receipt fixture executable unavailable',
      'cannot prepare failure receipt service',
      'cannot capture receipt',
      'foreground fixture cleanup did not finish',
      'foreground host exited before control request',
      'foreground never became ready',
      'foreground did not stop',
      'failure receipt host did not exit cleanly',
      'failed control stop was not rejected',
      'failed control stop did not preserve shutdown policy',
      'cannot read failure receipt service descriptor',
      'failed control stop did not preserve durable fault and stopped intent',
      'cannot read receipt',
      'one failed control operation did not emit exactly one site receipt',
      'failed control operation lost its inner supervisor_stop site',
    ]],
    ['service_native_deadline_test.go', 'TestServiceRunShutdownSharedDeadline', [
      'shutdown did not create one fresh signal-independent platform budget',
      'cannot read supervisor shutdown budget source',
      'supervisor shutdown budget lost platform cap or independent parent',
      'supervisor shutdown budget mutation did not apply',
      'supervisor shutdown budget contract accepted a broken policy',
    ]],
    ['service_shutdown_windows_test.go', 'TestWindowsNoConsoleShutdown', [
      'no-console probe result unavailable',
      'detached probe unexpectedly had a console',
      'no-console probe launch failed',
      'detached probe unexpectedly attached to parent console',
      'detached AttachConsole probe did not prove ERROR_INVALID_HANDLE',
      'no-console probe exited unexpectedly',
    ]],
    ['service_shutdown_console_contract_test.go', 'TestWindowsNoConsoleProbeSourceContract', [
      'no-console probe source unavailable',
      'no-console probe lost detached parent or exact error proof',
      'no-console probe mutation site changed',
      'no-console probe contract accepted a broken oracle',
      'no-console probe contract rejected CRLF source',
    ]],
  ];
  for (const [file, testName, messages] of cases) {
    for (const message of messages) {
      for (const appended of [false, true]) {
        const input = stream(packages.flatMap(name => name === packages[0]
          ? [event('start', name), event('run', name, testName),
            event('output', name, testName, {Output: `    ${file}:12: ${message}${appended ? ' private-fixture-marker' : ''}\n`}),
            event('output', name, testName, {Output: '    private details are not diagnostics\n'}),
            event('fail', name, testName), event('fail', name)] : packageEvents(name)));
        const result = await checkGoTestJSON([input]);
        assert.equal(result.reason, failureDiagnostic.trimEnd());
        assert(result.lines.includes(`why ${packages[0]} ${testName} :: ${appended ? '(unclassified)' : message}`));
        assert(!result.lines.join('\n').includes('private-fixture-marker'));
      }
    }
  }
});

function lostACKFailure(outputs, reason, genericFatal = true) {
  const parent = 'TestWindowsShutdownEscalation';
  const child = `${parent}/lost`;
  const generic = 'lost ACK with observed exit did not complete safely';
  return {
    input: stream(packages.flatMap(name => name === packages[0]
      ? [event('start', name), event('run', name, parent), event('run', name, child),
        ...[...outputs,
          'shutdown kind=private-lost-ack-sentinel detail=private-lost-ack-sentinel budget=private-lost-ack-sentinel startup=private-lost-ack-sentinel',
          ...(genericFatal ? [generic] : [])]
          .map(Output => event('output', name, child, {Output: `    service_shutdown_windows_test.go:12: ${Output}\n`})),
        event('fail', name, child), event('fail', name, parent), event('fail', name)] : packageEvents(name))),
    output: summary({[packages[0]]: {pass: 0, fail: 2, failedNames: [parent, child],
      why: {[parent]: reason === '(unclassified)' ? reason : `child failed: ${reason}`, [child]: reason}}}),
    stderr: failureDiagnostic,
    status: 1,
  };
}

test('lost ACK final outcomes remain fixed value-free failure reasons', async () => {
  // Independent expected literals: source enrollment must come from the real
  // direct Error calls, including when the unchanged generic Fatal follows.
  const messages = [
    'lost ACK final outcome forced',
    'lost ACK final outcome unknown deadline',
    'lost ACK final outcome unknown ack',
    'lost ACK final outcome unknown incarnation',
    'lost ACK final outcome unknown force',
    'lost ACK final outcome refused incarnation',
    'lost ACK final outcome refused descriptor',
    'lost ACK final outcome refused pipe',
    'lost ACK final outcome refused ack',
    'lost ACK final outcome refused migration',
    'lost ACK final outcome refused force_rights',
    'lost ACK final outcome refused force',
    'lost ACK final outcome unclassified',
  ];
  for (const message of messages) {
    const fixture = lostACKFailure(['irrelevant private-lost-ack-sentinel', message], message);
    const result = await checkGoTestJSON([fixture.input]);
    assert.equal(result.valid, false);
    assert.equal(result.reason, failureDiagnostic.trimEnd());
    assert.equal(`${result.lines.join('\n')}\n`, fixture.output);
    assert(!JSON.stringify(result).includes('private-lost-ack-sentinel'));
    assertFixture(script, fixture);
  }
});

test('lost ACK final outcome rejects unregistered output', async () => {
  for (const output of [
    'lost ACK final outcome refused pipe:',
    'lost ACK final outcome refused pipe private-lost-ack-sentinel',
    'private-lost-ack-sentinel lost ACK final outcome refused pipe',
    'lost ACK final outcome refused pipe=private-lost-ack-sentinel',
    'lost ACK final outcome refused pipe/private-lost-ack-sentinel',
    'lost ACK final outcome refused',
    'lost ACK final outcome unregistered',
    'arbitrary private-lost-ack-sentinel value=private-lost-ack-sentinel',
  ]) {
    for (const genericFatal of [false, true]) {
      const reason = genericFatal ? 'lost ACK with observed exit did not complete safely' : '(unclassified)';
      const fixture = lostACKFailure([output], reason, genericFatal);
      const result = await checkGoTestJSON([fixture.input]);
      assert.equal(result.valid, false);
      assert.equal(result.reason, failureDiagnostic.trimEnd());
      assert.equal(`${result.lines.join('\n')}\n`, fixture.output);
      assert(!JSON.stringify(result).includes('private-lost-ack-sentinel'));
      assertFixture(script, fixture);
    }
  }
});

test('child control and native proof retain each closed transport cause as why', async () => {
  for (const step of ['dial', 'deadline', 'eof', 'response']) {
    const message = `localcell: service_control_unavailable: transport:${step}`;
    for (const testName of ['TestServiceControlChildProcessClient', 'TestNativeServiceLifecycle']) {
      const native = testName === 'TestNativeServiceLifecycle';
      const outputs = [
        `    service_native_diagnostics_test.go:12: ${message}\n`,
        '    service_native_diagnostics_test.go:13: service_control accept=rejected step=peer:sid\n',
      ];
      const failed = [event('start', packages[0]), event('run', packages[0], testName),
        ...outputs.map(Output => event('output', packages[0], testName, {Output})),
        event('fail', packages[0], testName), event('fail', packages[0])];
      const input = native ? failed : [...failed, ...packages.slice(1).flatMap(name => packageEvents(name))];
      const result = await checkGoTestJSON([stream(input)], native ? [testName] : [], {diagnose: native});
      assert.equal(result.valid, false);
      assert.equal(result.reason, failureDiagnostic.trimEnd());
      assert(result.lines.includes(`why ${packages[0]} ${testName} :: ${message}`));
      assert.equal(result.lines.some(line => line.startsWith('diagnose ') && line.includes('step=peer:sid')), native);
    }
  }
});

test('transport why rejects appended private text and unknown transport steps', () => {
  for (const message of ['localcell: service_control_unavailable: transport:unknown',
    'localcell: service_control_unavailable: transport:eof private-marker']) {
    assertFixture(script, outputFailure([`    service_native_diagnostics_test.go:12: ${message}\n`], '(unclassified)', packages[0]));
  }
});

test('unchanged checker retains assertion fallback for dynamic pipe refusal and native diagnose detail', async () => {
  for (const native of [false, true]) {
    const testName = native ? 'TestNativeServiceLifecycle' : 'TestServiceControlChildProcessClient';
    const assertion = native ? 'authenticated native service stop failed' : 'child control action failed: stop';
    const reason = native ? assertion : 'child control action failed';
    const failed = [event('start', packages[0]), event('run', packages[0], testName),
      ...['localcell: service_control_refused: pipe:io', 'service_control accept=rejected step=peer:sid', assertion]
        .map(message => event('output', packages[0], testName, {Output: `    service_native_diagnostics_test.go:12: ${message}\n`})),
      event('fail', packages[0], testName), event('fail', packages[0])];
    const input = native ? failed : [...failed, ...packages.slice(1).flatMap(name => packageEvents(name))];
    const result = await checkGoTestJSON([stream(input)], native ? [testName] : [], {diagnose: native});
    assert(result.lines.includes(`why ${packages[0]} ${testName} :: ${reason}`));
    assert.equal(result.lines.some(line => line.startsWith('diagnose ') && line.includes('service_control_refused: pipe:io')), native);
  }
});

function foregroundHostFailure(output, message) {
  const testName = 'TestWindowsServiceForegroundSubprocessCommands';
  return {
    input: stream(packages.flatMap(name => name === packages[2]
      ? [event('start', name), event('run', name, testName),
        ...output.map(Output => event('output', name, testName, {Output})),
        event('fail', name, testName), event('fail', name)] : packageEvents(name))),
    output: summary({[packages[2]]: {pass: 0, fail: 1, failedNames: [testName], why: {[testName]: message}}}),
    stderr: failureDiagnostic,
  };
}

test('foreground harness first lifecycle error wins before a classified exit fatal without diagnose', () => {
  const message = 'localcell: service_lease_unavailable';
  assertFixture(script, foregroundHostFailure([
    `    service_subprocess_windows_test.go:12: ${message}\n`,
    '    service_subprocess_windows_test.go:13: exit_code=1\n',
    '    service_subprocess_windows_test.go:14: foreground host exited with error before authenticated status after restart\n',
  ], message));
});

test('foreground harness first classified exit fatal wins before a lifecycle error without diagnose', () => {
  const message = 'foreground host exited with error before authenticated status after restart';
  assertFixture(script, foregroundHostFailure([
    `    service_subprocess_windows_test.go:12: ${message}\n`,
    '    service_subprocess_windows_test.go:13: localcell: service_lease_unavailable\n',
  ], message));
});

test('foreground harness classifies every iteration and exit cause without diagnose', () => {
  // Independent contract: collapsing the direct Fatal call sites to the old
  // generic assertion removes these source allowlist entries and fails here.
  for (const iteration of ['on first start', 'after restart']) {
    for (const cause of [
      'foreground command budget expired before authenticated status',
      'foreground synthetic budget expired before authenticated status',
      'foreground host exited with error before authenticated status',
      'foreground host exited cleanly before authenticated status',
      'foreground readiness exceeded synthetic budget',
      'foreground host did not expose authenticated status within allowance',
      'foreground command budget expired during shutdown',
      'foreground synthetic budget expired during shutdown',
      'foreground host exited with error during shutdown',
      'foreground shutdown exceeded synthetic budget',
      'foreground console shutdown exceeded allowance',
    ]) {
      const message = `${cause} ${iteration}`;
      assertFixture(script, foregroundHostFailure([
        '    service_subprocess_windows_test.go:12: exit_code=1\n',
        `    service_subprocess_windows_test.go:13: ${message}\n`,
      ], message));
    }
  }
});

test('formatted concurrent Start failure is classified by its value-free source prefix', () => {
  assertFixture(script, outputFailure([
    '    lifecycle_hermetic_test.go:123: independent concurrent Start did not finish: fixture error\n',
  ], 'independent concurrent Start did not finish', packages[0]));
});

test('a failure sharing only the first eleven source-prefix characters stays unclassified', () => {
  assert.equal('independent'.length, 11);
  assertFixture(script, outputFailure([
    '    lifecycle_hermetic_test.go:123: independent private-fixture-marker\n',
  ], '(unclassified)', packages[0]));
});

// Ordinary output and why classification are value-free; the inherited
// --require … --diagnose proof path masks by pattern and is unchanged.
test('ordinary output and why classification omit formatted values', () => {
  assertFixture(script, outputFailure([
    `    lifecycle_hermetic_test.go:123: independent concurrent Start did not finish: private-fixture-marker ${'a'.repeat(64)} password=private-value\n`,
  ], 'independent concurrent Start did not finish', packages[0]));
});

test('formatted assertion prefixes require at least twelve safe display characters', () => {
  withDiagnosticSources(target => {
    assertFixture(target, outputFailure(['    policy_test.go:12: abcdefghijkl: private-fixture-marker\n'], 'abcdefghijkl'));
    assertFixture(target, outputFailure(['    policy_test.go:12: abcdefghijk: private-fixture-marker\n']));
  });
});

test('formatted prefixes require the complete source prefix and remain package-local', () => {
  withDiagnosticSources(target => {
    assertFixture(target, outputFailure(['    policy_test.go:12: safe formatted assertion: private-fixture-marker\n'], 'safe formatted assertion'));
    assertFixture(target, outputFailure(['    policy_test.go:12: safe formatted: detail: private-fixture-marker\n'], 'safe formatted: detail'));
    assertFixture(target, outputFailure(['    policy_test.go:12: safe formatted assertion without colon private-fixture-marker\n'],
      'safe formatted assertion without colon'));
    for (const message of ['safe formatted assertion private-fixture-marker', 'safe formatted assertion:private-fixture-marker',
      'safe formatted assertion without colony private-fixture-marker']) {
      assertFixture(target, outputFailure([`    policy_test.go:12: ${message}\n`]));
    }
    assertFixture(target, outputFailure(['    policy_test.go:12: safe formatted assertion: private-fixture-marker\n'], '(unclassified)', packages[0]));
  });
});

test('unsafe, commented, quoted, concatenated and nonformat prefixes cannot enroll diagnostics', () => {
  withDiagnosticSources(target => {
    for (const prefix of ['unsafe formatted/path', 'unsafe formatted=value',
      'unsafe formatted 123 digits', 'nonformat method assertion', 'concatenated formatted assertion',
      'comment formatted assertion', 'quoted formatted assertion']) {
      assertFixture(target, outputFailure([`    policy_test.go:12: ${prefix}: private-fixture-marker\n`]));
    }
  });
});

test('hermetic install and startup-barrier failures identify their stage without values', () => {
  for (const prefix of ['synthetic install', 'start ended before owning the startup barrier']) {
    assertFixture(script, outputFailure([`    lifecycle_integration_test.go:12: ${prefix}: private-fixture-marker\n`], prefix, packages[0]));
  }
  const fixed = 'start did not reach the startup barrier within its fixture deadline';
  assertFixture(script, outputFailure([`    lifecycle_integration_test.go:12: ${fixed}\n`], fixed, packages[0]));
});

for (const {name, output, leafReason, ancestorReason} of [
  {
    name: 'classified nested lifecycle failure gives both ancestors the safe child reason',
    output: '    lifecycle_hermetic_test.go:174: start did not reach the startup barrier within its fixture deadline\nprivate-fixture-marker\n',
    leafReason: 'start did not reach the startup barrier within its fixture deadline',
    ancestorReason: 'child failed: start did not reach the startup barrier within its fixture deadline',
  },
  {
    name: 'unclassified nested lifecycle failure leaves both ancestors unclassified',
    output: '    lifecycle_hermetic_test.go:174: private-fixture-marker\n',
    leafReason: '(unclassified)',
    ancestorReason: '(unclassified)',
  },
]) {
  test(name, () => {
    const parent = 'TestHermeticLifecycle';
    const intermediate = `${parent}/ConcurrentStartIndependentRoots`;
    const leaf = `${intermediate}/OverlappingStarts`;
    assertFixture(script, {
      input: stream([
        event('start', packages[0]), ...[parent, intermediate, leaf].map(name => event('run', packages[0], name)),
        event('output', packages[0], leaf, {Output: output}),
        ...[leaf, intermediate, parent].map(name => event('fail', packages[0], name)), event('fail', packages[0]),
      ]),
      args: ['--require', parent],
      output: [
        `package ${packages[0]} pass=0 skip=0 fail=3`,
        `fail ${packages[0]} ${parent}`, `why ${packages[0]} ${parent} :: ${ancestorReason}`,
        `fail ${packages[0]} ${intermediate}`, `why ${packages[0]} ${intermediate} :: ${ancestorReason}`,
        `fail ${packages[0]} ${leaf}`, `why ${packages[0]} ${leaf} :: ${leafReason}`, '',
      ].join('\n'),
      stderr: failureDiagnostic,
    });
  });
}

test('ancestor fallback uses classification event order and preserves direct reasons', () => {
  withDiagnosticSources(target => {
    const parent = 'TestFixture';
    const runFirst = `${parent}/ZRunFirst`;
    const reasonFirst = `${parent}/MReasonFirst`;
    const failFirst = `${parent}/AFailFirst`;
    for (const direct of [false, true]) {
      assertFixture(target, {
        input: stream([
          ...packageEvents(packages[0]), event('start', packages[1]),
          ...[parent, runFirst, reasonFirst, failFirst].map(name => event('run', packages[1], name)),
          event('output', packages[1], reasonFirst, {Output: `safe formatted assertion: private-fixture-marker ${'a'.repeat(64)} password=private-value\n`}),
          event('output', packages[1], runFirst, {Output: 'ace-count\n'}),
          event('output', packages[1], failFirst, {Output: 'owner-is-admin-group\n'}),
          ...(direct ? [event('output', packages[1], parent, {Output: 'localcell: fixed private error\n'})] : []),
          ...[failFirst, runFirst, reasonFirst, parent].map(name => event('fail', packages[1], name)),
          event('fail', packages[1]), ...packageEvents(packages[2]),
        ]),
        output: summary({[packages[1]]: {pass: 0, fail: 4, failedNames: [parent, runFirst, reasonFirst, failFirst], why: {
          [parent]: direct ? 'localcell: fixed private error' : 'child failed: safe formatted assertion',
          [runFirst]: 'ace-count', [reasonFirst]: 'safe formatted assertion', [failFirst]: 'owner-is-admin-group',
        }}}),
        stderr: failureDiagnostic,
      });
    }
  });
});

test('ancestor fallback excludes passing or skipped children, prefix siblings and other packages', () => {
  withDiagnosticSources(target => {
    const parent = 'TestFixture';
    const passed = `${parent}/Passed`;
    const skipped = `${parent}/Skipped`;
    const unclassified = `${parent}/Unclassified`;
    const sibling = `${parent}Sibling/Failed`;
    assertFixture(target, {
      input: stream([
        event('start', packages[0]), event('run', packages[0], unclassified),
        event('output', packages[0], unclassified, {Output: 'localcell: fixed private error\n'}),
        event('fail', packages[0], unclassified), event('fail', packages[0]),
        event('start', packages[1]), event('run', packages[1], parent),
        ...[[passed, 'pass'], [skipped, 'skip'], [sibling, 'fail'], [unclassified, 'fail']].flatMap(([name, action]) => [
          event('run', packages[1], name),
          event('output', packages[1], name, {Output: name === unclassified ? 'private-fixture-marker\n' : 'ace-count\n'}),
          event(action, packages[1], name),
        ]),
        event('fail', packages[1], parent), event('fail', packages[1]), ...packageEvents(packages[2]),
      ]),
      output: summary({
        [packages[0]]: {pass: 0, fail: 1, failedNames: [unclassified], why: {[unclassified]: 'localcell: fixed private error'}},
        [packages[1]]: {pass: 1, skip: 1, fail: 3, names: [skipped], failedNames: [parent, sibling, unclassified], why: {[sibling]: 'ace-count'}},
      }),
      stderr: failureDiagnostic,
    });
  });
});

test('autobuild lifecycle failures classify each formatted assertion without operation or component values', () => {
  for (const prefix of ['harness operation failed', 'harness cleanup witness did not observe the running component',
    'harness process survived removal', 'harness purge failed']) {
    assertFixture(script, outputFailure([`    main_test.go:12: ${prefix}: private-fixture-marker\n`], prefix, packages[2]));
  }
  for (const fixed of ['cannot build lifecycle fixture', 'cannot prepare fixture tool', 'cannot repair harness fixture during cleanup',
    'cannot purge harness fixture during cleanup', 'autobuilt component disappeared after command return',
    'non-purge removal retained runtime records', 'purge retained autobuilt components', 'cannot retain harness process cleanup identity',
    'invalid harness process cleanup identity', 'cannot prepare harness cleanup witness', 'cannot create harness cleanup witness',
    'cannot retain harness cleanup witness', 'cannot construct harness cleanup witness', 'cannot verify harness cleanup identity',
    'harness cleanup witness lost its process identity']) {
    assertFixture(script, outputFailure([`    main_test.go:12: ${fixed}\n`], fixed, packages[2]));
  }
});

test('runtime configuration failures retain fixed first-line reasons before formatted details', () => {
  // Independent list: reverting any assertion to a formatted-only failure must
  // lose its source allowlist entry and fail this fixture. Details stay private.
  for (const message of [
    'hosted config changed', 'local not selected', 'unsafe selector diagnostic',
    'loopback refused', 'unsafe listener accepted or disclosed',
    'server listener defaults could not be loaded', 'worker listener defaults could not be loaded',
    'local listener defaults changed', 'dangerous environment baseline could not be loaded',
    'forbidden variable accepted or value disclosed', 'hosted configuration changed',
    'required configuration refusal missing', 'account configuration could not be loaded',
    'account identity must not be normalized',
  ]) {
    assertFixture(script, outputFailure([
      `    config_test.go:12: ${message}\n`,
      '    config_test.go:13: configuration error: private-fixture-marker\n',
    ], message));
  }
});

test('build failures, malformed input, and unexpected package or unsafe test names fail without reflecting values', () => {
  for (const extra of [
    {Action: 'build-fail', ImportPath: 'private-fixture-marker'},
    {Action: 'start', Package: 'private-fixture-marker'},
    {Action: 'private-fixture-marker', Package: packages[0]},
    null, [], 'private-fixture-marker',
  ]) assertFixture(script, {input: stream([passing[0], extra, ...passing.slice(1)]), output: summary()});
  for (const name of ['TestPrivate\nprivate-fixture-marker', `Test${'a'.repeat(64)}`, 42]) {
    for (const action of ['skip', 'fail']) {
      assertFixture(script, {input: stream([passing[0], event('run', packages[0], name),
        event(action, packages[0], name), ...passing.slice(1)]), output: summary()});
    }
  }
  for (const tail of ['{', 'private-fixture-marker\n', '\n']) {
    assertFixture(script, {input: stream(passing) + tail, output: summary()});
  }
});

test('missing packages, package-only skips, duplicate summaries, and unfinished or unordered tests fail closed', () => {
  const name = packages[0];
  const others = packages.slice(1).flatMap(value => packageEvents(value));
  for (const events of [
    [], [event('start', name), event('skip', name)],
    [event('start', name), event('run', name, 'TestFixture'), event('pass', name)],
    [event('start', name), event('pass', name, 'TestFixture'), event('pass', name)],
    [event('run', name, 'TestFixture'), event('pass', name, 'TestFixture'), event('pass', name)],
  ]) assertFixture(script, {input: stream([...events, ...others]), output: summary({[name]: {pass: 0}})});
  assertFixture(script, {input: stream([...passing, event('pass', name)]), output: summary()});
  assertFixture(script, {input: '', output: summary(Object.fromEntries(packages.map(value => [value, {pass: 0}])))});
});

test('an input stream error fails closed without reflecting its message', async () => {
  async function* interrupted() {
    yield stream(passing);
    throw new Error('private-fixture-marker');
  }
  const result = await checkGoTestJSON(interrupted());
  assert.equal(result.valid, false);
  assert.equal(`${result.lines.join('\n')}\n`, summary());
});

const requiredTests = ['TestRealLifecycle', ...[
  'InstallStartRestartRemove', 'TwoCellsAreIndependent', 'InterruptedStartRepair', 'WaitsForReadiness', 'InstallerPrincipals',
].map(name => `TestRealLifecycle/${name}`)];
const requiredArgs = ['--require', requiredTests.join(',')];
const proofEvents = [event('start', packages[0]), event('run', packages[0], requiredTests[0]),
  ...requiredTests.slice(1).flatMap(name => [event('run', packages[0], name), event('pass', packages[0], name)]),
  event('pass', packages[0], requiredTests[0]), event('pass', packages[0])];
const proofSummary = count => `package ${packages[0]} pass=${count} skip=0 fail=0\n`;

test('required-test mode accepts the complete native proof without requiring unrelated unit packages', () => {
  assertFixture(script, {args: requiredArgs, input: stream(proofEvents), output: proofSummary(requiredTests.length), status: 0});
});

for (const required of requiredTests) {
  test(`required-test mode rejects missing ${required}`, () => {
    assertFixture(script, {args: requiredArgs,
      input: stream(proofEvents.filter(value => value.Test !== required)), output: proofSummary(requiredTests.length - 1)});
  });
  test(`required-test mode rejects skipped ${required}`, () => {
    assertFixture(script, {args: requiredArgs,
      input: stream(proofEvents.map(value => value.Test === required && value.Action === 'pass' ? {...value, Action: 'skip'} : value)),
      output: `package ${packages[0]} pass=${requiredTests.length - 1} skip=1 fail=0\nskip ${packages[0]} ${required}\n`});
  });
}

test('required-test mode rejects truncated proof and package skip', () => {
  assertFixture(script, {args: requiredArgs, input: stream(proofEvents).trimEnd(), output: proofSummary(requiredTests.length)});
  for (const Action of ['skip']) {
    assertFixture(script, {args: requiredArgs,
      input: stream([...proofEvents.slice(0, -1), event(Action, packages[0])]), output: proofSummary(requiredTests.length)});
  }
});

test('required-test mode classifies complete failed tests and packages as Go test failures without stream diagnostics', async () => {
  for (const required of [undefined, ...requiredTests]) {
    const events = proofEvents.map(value => value.Action === 'pass' &&
      (value.Test === required || value.Test === undefined) ? {...value, Action: 'fail'} : value);
    const result = await checkGoTestJSON([stream(events)], requiredTests, {diagnose: true});
    assert.equal(result.valid, false);
    assert.equal(result.reason, failureDiagnostic.trimEnd());
    assert.equal(result.lines.some(line => line.startsWith('stream ')), false);
    if (required !== undefined) assert(result.lines.includes(`fail ${packages[0]} ${required}`));
  }
});

test('required-test mode diagnoses missing and skipped requirements as incomplete even with a failed package', async () => {
  for (const action of ['missing', 'skip']) {
    const events = proofEvents.filter(value => action !== 'missing' || value.Test !== requiredTests[1])
      .map(value => value.Action === 'pass' && value.Test === undefined ? {...value, Action: 'fail'}
        : value.Action === 'pass' && value.Test === requiredTests[1] ? {...value, Action: action} : value);
    const result = await checkGoTestJSON([stream(events)], requiredTests, {diagnose: true});
    assert.equal(result.reason, diagnostic.trimEnd());
    assert(result.lines.some(line => line.startsWith('stream ')));
  }
});

test('mutation killed: required failure classified as an incomplete stream', () => {
  const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-failure-')));
  try {
    const source = readFileSync(script, 'utf8');
    const guard = "!['pass', 'fail'].includes(state.tests.get(testName))";
    assert.equal(source.split(guard).length, 2);
    const target = join(directory, 'mutant.mjs');
    writeFileSync(target, source.replace(guard, "state.tests.get(testName) !== 'pass'"));
    const input = stream(proofEvents.map(value => value.Action === 'pass' &&
      (value.Test === requiredTests[1] || value.Test === requiredTests[0] || value.Test === undefined)
      ? {...value, Action: 'fail'} : value));
    const args = [...requiredArgs, '--diagnose'];
    const expected = spawnSync(process.execPath, [script, ...args], {input, encoding: 'utf8'});
    assert.equal(expected.status, 1);
    assert.equal(expected.stderr, failureDiagnostic);
    assert(!expected.stdout.includes('stream records='));
    assert.throws(() => assertFixture(target, {args, input, output: expected.stdout, stderr: expected.stderr}), {code: 'ERR_ASSERTION'});
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
});

test('required-test mode rejects an additional skipped child even when every named scenario passes', () => {
  const extra = 'TestRealLifecycle/AdditionalScenario';
  assertFixture(script, {args: requiredArgs,
    input: stream([...proofEvents.slice(0, -2), event('run', packages[0], extra), event('skip', packages[0], extra), ...proofEvents.slice(-2)]),
    output: `package ${packages[0]} pass=${requiredTests.length} skip=1 fail=0\nskip ${packages[0]} ${extra}\n`});
});

function diagnosedFailure(outputs, {packageOutput = [], finish = true} = {}) {
  const name = requiredTests[0];
  return [event('start', packages[0]), event('run', packages[0], name),
    ...outputs.map(Output => event('output', packages[0], name, {Output})),
    ...packageOutput.map(Output => event('output', packages[0], undefined, {Output})),
    ...(finish ? [event('fail', packages[0], name), event('fail', packages[0])] : [])];
}

const diagnoseArgs = ['--diagnose', '--require', requiredTests[0]];

test('diagnose emits only failed required tests and repository test assertions or Go FAIL lines', async () => {
  const output = '    lifecycle_proof_test.go:42: cannot start native database\n';
  const events = diagnosedFailure([output, 'raw PostgreSQL output is private\n',
    '    foreign_test.go:42: foreign assertion is private\n',
    '    lifecycle.go:42: production log is private\n',
    '    --- FAIL: TestRealLifecycle (0.25s)\n']);
  const result = await checkGoTestJSON([stream(events)], [requiredTests[0]], {diagnose: true});
  assert.deepEqual(result.lines.filter(line => line.startsWith('diagnose ')), [
    `diagnose ${packages[0]} ${requiredTests[0]} :: ${output.trimEnd()}`,
    `diagnose ${packages[0]} ${requiredTests[0]} ::     --- FAIL: TestRealLifecycle (0.25s)`,
  ]);
  const passed = await checkGoTestJSON([stream(events.map(value => value.Action === 'fail' ? {...value, Action: 'pass'} : value))], [requiredTests[0]], {diagnose: true});
  assert.equal(passed.lines.some(line => line.startsWith('diagnose ')), false);
  const unit = await checkGoTestJSON([stream(events)], [], {diagnose: true});
  assert.equal(unit.lines.some(line => line.startsWith('diagnose ') || line.startsWith('stream ')), false);
  const extra = 'TestRealLifecycle/Unrequired';
  const unrequired = await checkGoTestJSON([stream([
    ...proofEvents.slice(0, -2), event('run', packages[0], extra), event('output', packages[0], extra, {Output: output}),
    event('fail', packages[0], extra), ...proofEvents.slice(-2),
  ])], requiredTests, {diagnose: true});
  assert.equal(unrequired.lines.some(line => line.startsWith('diagnose ')), false);
});

test('diagnose masks credentials before truncating and caps each failed test at 128 lines of two hundred characters', async () => {
  const prefix = '    lifecycle_proof_test.go:42: ';
  const outputs = [
    `${prefix}digest ${'a'.repeat(64)}\n`,
    `${prefix}password=private-password\n`,
    `${prefix}password="private password with spaces"\n`,
    `${prefix}--pwfile="private password path" other text\n`,
    `${prefix}PGPASSWORD=private-environment\n`,
    `${prefix}postgres://private-user:private-secret@127.0.0.1/db\n`,
    `${prefix}private-user:private-secret@127.0.0.1/db\n`,
    `${prefix}postgres://private-user:priv'ate"sec\\ret@127.0.0.1/db\n`,
    `${prefix}private-user:it's-private@127.0.0.1/db\n`,
    `${prefix}${'x '.repeat(80)}${'b'.repeat(64)}\n`,
    `${prefix}${'x '.repeat(125)}\n`,
    ...Array.from({length: 140}, (_, i) => `${prefix}assertion ${i}\n`),
  ];
  const result = await checkGoTestJSON([stream(diagnosedFailure(outputs))], [requiredTests[0]], {diagnose: true});
  const diagnostics = result.lines.filter(line => line.startsWith('diagnose ')).map(line => line.split(' :: ')[1]);
  assert.equal(diagnostics.length, 128);
  assert(diagnostics.every(line => line.length <= 200));
  assert.equal(diagnostics[10].length, 200);
  assert.equal(diagnostics[0], `${prefix}digest [MASKED]`);
  assert.equal(diagnostics[1], `${prefix}password=[MASKED]`);
  assert.equal(diagnostics[2], `${prefix}password=[MASKED]`);
  assert.equal(diagnostics[3], `${prefix}--[MASKED]`);
  assert.equal(diagnostics[4], `${prefix}[MASKED]`);
  assert.equal(diagnostics[5], `${prefix}postgres://[MASKED]@127.0.0.1/db`);
  assert.equal(diagnostics[6], `${prefix}[MASKED]@127.0.0.1/db`);
  assert.equal(diagnostics[7], `${prefix}postgres://[MASKED]@127.0.0.1/db`);
  assert.equal(diagnostics[8], `${prefix}[MASKED]@127.0.0.1/db`);
  assert.equal(diagnostics[9], `${prefix}${'x '.repeat(80)}[MASKED]`);
  assert(!result.lines.join('\n').includes('private-'));
  assert(!/[a-f0-9]{64}/i.test(result.lines.join('\n')));
});

test('diagnose reports the first invalid line masked and bounded with the observed record count', async () => {
  const invalid = `postgres://private-user:private-secret@127.0.0.1 ${'a'.repeat(64)} ${'x '.repeat(100)}`;
  const result = await checkGoTestJSON([stream(proofEvents) + `${invalid}\nsecond-invalid-line\n`], requiredTests, {diagnose: true});
  const detail = result.lines.find(line => line.startsWith('stream '));
  assert.equal(detail, `stream records=${proofEvents.length + 2} :: first offending line: ${`postgres://[MASKED]@127.0.0.1 [MASKED] ${'x '.repeat(100)}`.slice(0, 120)}`);
  assert(!detail.includes('private-'));
  assert(!detail.includes('second-invalid-line'));
  for (const fragment of ['password="private password with spaces"', 'PGPASSWORD=private-value', '--pwfile="private path"']) {
    const rejected = await checkGoTestJSON([stream(proofEvents) + fragment], requiredTests, {diagnose: true});
    assert(!rejected.lines.join('\n').includes('private'));
    assert(rejected.lines.some(line => line.startsWith(`stream records=${proofEvents.length} :: first offending line: `)));
  }
});

for (const [kind, canary] of [
  ['SID', ['S', '1', '5', '21', '7139', '2846', '9025', '1007'].join('-')],
  ['Windows drive path', String.raw`C:\private fixture\host.exe`],
  ['Windows UNC path', String.raw`\\fixture-server\private share\host.exe`],
  ['task namespace', String.raw`\Witself-Cell-privatefixture`],
  ['named pipe', String.raw`\\.\pipe\private-fixture-control`],
  ['base64 run', 'cHJpdmF0ZS1maXh0dXJlLW9ubHktY2FuYXJ5'],
]) {
  test(`diagnose masks ${kind} before and across both truncation boundaries`, async () => {
    for (const boundary of [false, true]) {
      const prefix = '    lifecycle_proof_test.go:42: ';
      const padding = boundary ? ' '.repeat(196 - prefix.length) : '';
      const output = `${prefix}${padding}${canary}\n`;
      const result = await checkGoTestJSON([stream(diagnosedFailure([output]))], [requiredTests[0]], {diagnose: true});
      const detail = result.lines.find(line => line.startsWith('diagnose '))?.split(' :: ')[1];
      assert.equal(detail === `${prefix}${padding}[MASKED]`.slice(0, 200), true,
        'private native diagnostic was not masked before its display boundary');

      const invalidPrefix = 'invalid: ';
      const invalidPadding = boundary ? ' '.repeat(116 - invalidPrefix.length) : '';
      const invalidLine = `${invalidPrefix}${invalidPadding}${canary}`;
      const rejected = await checkGoTestJSON([stream(proofEvents) + `${invalidLine}\n`], requiredTests, {diagnose: true});
      const streamDetail = rejected.lines.find(line => line.startsWith('stream '));
      const expected = `stream records=${proofEvents.length + 1} :: first offending line: ${`${invalidPrefix}${invalidPadding}[MASKED]`.slice(0, 120)}`;
      assert.equal(streamDetail === expected, true,
        'private invalid-stream diagnostic was not masked before its display boundary');
    }
  });
}

test('diagnose masks exactly thirty-two base64 characters and preserves shorter ordinary text', async () => {
  const prefix = '    lifecycle_proof_test.go:42: ';
  for (const length of [31, 32, 33]) {
    const result = await checkGoTestJSON([stream(diagnosedFailure([`${prefix}${'Q'.repeat(length)}\n`]))],
      [requiredTests[0]], {diagnose: true});
    const detail = result.lines.find(line => line.startsWith('diagnose '))?.split(' :: ')[1];
    assert.equal(detail === `${prefix}${length >= 32 ? '[MASKED]' : 'Q'.repeat(length)}`, true,
      'base64 diagnostic mask has the wrong minimum length');
  }
});

for (const [kind, canary, suffix] of [
  ['SID', ['S', '1', '5', '21', '7139', '2846', '9025', '1007'].join('-'), 'x'],
  ['Windows drive path', String.raw`C:\private fixture\host.exe`, ''],
]) {
  test(`diagnose masks concatenated ${kind} before and across both truncation boundaries`, async () => {
    for (const boundary of [false, true]) {
      const prefix = '    lifecycle_proof_test.go:42: ';
      const padding = boundary ? ' '.repeat(195 - prefix.length) : '';
      const output = `${prefix}${padding}x${canary}${suffix}\n`;
      const result = await checkGoTestJSON([stream(diagnosedFailure([output]))], [requiredTests[0]], {diagnose: true});
      const detail = result.lines.find(line => line.startsWith('diagnose '))?.split(' :: ')[1];
      assert.equal(detail === `${prefix}${padding}x[MASKED]${suffix}`.slice(0, 200), true,
        'concatenated private native diagnostic escaped masking');

      const invalidPrefix = 'invalid: ';
      const invalidPadding = boundary ? ' '.repeat(115 - invalidPrefix.length) : '';
      const invalidLine = `${invalidPrefix}${invalidPadding}x${canary}${suffix}`;
      const rejected = await checkGoTestJSON([stream(proofEvents) + `${invalidLine}\n`], requiredTests, {diagnose: true});
      const streamDetail = rejected.lines.find(line => line.startsWith('stream '));
      const expected = `stream records=${proofEvents.length + 1} :: first offending line: ${`${invalidPrefix}${invalidPadding}x[MASKED]${suffix}`.slice(0, 120)}`;
      assert.equal(streamDetail === expected, true,
        'concatenated private invalid-stream diagnostic escaped masking');
    }
  });
}

test('diagnose preserves both forty-line readiness log tails after cause and identity details', async () => {
  for (const component of ['server', 'worker']) {
    const details = [`inner cause: local cell ${component} exited before readiness`,
      `${component} identity before rollback: recorded=true alive=false matches=false exit_observed=true observation_failed=false`,
      ...[component, 'postgres'].flatMap(name => Array.from({length: 40}, (_, i) => `${name} log: line ${i}`))];
    const events = diagnosedFailure(details.map(line => `    lifecycle_diagnostics_test.go:99: ${line}\n`));
    const result = await checkGoTestJSON([stream(events)], [requiredTests[0]], {diagnose: true});
    const diagnostics = result.lines.filter(line => line.startsWith('diagnose '));
    assert.equal(diagnostics.length, 82);
    assert(diagnostics.at(-1).endsWith('postgres log: line 39'));
    assert.equal(result.reason, failureDiagnostic.trimEnd());
    assert.equal(result.lines.some(line => line.startsWith('stream ')), false);
  }
});

test('diagnose explains a stream ending before the package summary and preserves unit output', async () => {
  const input = stream(proofEvents.slice(0, -1));
  const result = await checkGoTestJSON([input], requiredTests, {diagnose: true});
  assert.equal(result.lines.at(-1), `stream records=${proofEvents.length - 1} :: stream ended before the package summary`);
  const normal = await checkGoTestJSON([input], requiredTests);
  assert.deepEqual(normal.lines, result.lines.slice(0, -1));
});

test('timeout panic is classified in both test and package output, including unfinished tests', async () => {
  for (const packageLevel of [false, true]) {
    for (const finish of [false, true]) {
      const panic = 'panic: test timed out after 15m0s\n';
      const events = diagnosedFailure(packageLevel ? [] : [panic], {packageOutput: packageLevel ? [panic] : [], finish});
      const result = await checkGoTestJSON([stream(events)], [requiredTests[0]], {diagnose: true});
      assert(result.lines.includes(`why ${packages[0]} ${requiredTests[0]} :: test timed out`));
      assert(!result.lines.some(line => line.endsWith('(unclassified)')));
      assert(!result.lines.some(line => line.startsWith('diagnose ')));
    }
  }
});

test('input mode reads the named UTF-8 proof file instead of stdin and diagnoses invalid file input', () => {
  const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-input-')));
  try {
    const input = join(directory, 'proof.json');
    writeFileSync(input, stream(proofEvents).replaceAll('\n', '\r\n'));
    assertFixture(script, {args: [...requiredArgs, '--input', input, '--diagnose'], input: 'invalid stdin\n', output: proofSummary(requiredTests.length), status: 0});
    writeFileSync(input, stream(proofEvents.slice(0, -1)));
    assertFixture(script, {args: [...requiredArgs, '--input', input, '--diagnose'], input: '',
      output: `${proofSummary(requiredTests.length)}stream records=${proofEvents.length - 1} :: stream ended before the package summary\n`});
    assertFixture(script, {args: [...requiredArgs, '--input', join(directory, 'private-missing-file'), '--diagnose'], input: '',
      output: 'package ' + packages[0] + ' pass=0 skip=0 fail=0\nstream records=0 :: stream ended before the package summary\n'});
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
});

test('diagnose and input argument validation fails without reflecting values or enabling unit diagnostics', () => {
  for (const args of [['--diagnose'], ['--input'], ['--input', ''], ['--input', '--diagnose'],
    [...diagnoseArgs, '--diagnose'], [...requiredArgs, '--require', requiredTests[0]], ['--input', 'private-input', '--input', 'private-input']]) {
    assertFixture(script, {args, input: '', output: '', stderr: 'FAIL: invalid Go test checker arguments\n'});
  }
});

for (const mutation of [
  {name: 'diagnostic limit removed', from: 'if (diagnostics.length === 128) break;', to: '',
    outputs: Array.from({length: 129}, (_, i) => `--- FAIL: TestRealLifecycle (${i}s)\n`)},
  {name: 'diagnostic width limit removed', from: '.slice(0, limit);', to: ';',
    outputs: [`--- FAIL: TestRealLifecycle ${'x '.repeat(125)}\n`]},
  {name: 'diagnostic masking removed', from: "return maskedDiagnostic(line.replace(/\\r$/, ''), 200);", to: "return line.replace(/\\r$/, '').slice(0, 200);",
    outputs: [`--- FAIL: TestRealLifecycle ${'a'.repeat(64)}\n`, '--- FAIL: TestRealLifecycle password=private-value\n']},
  {name: 'foreign test assertions accepted', from: 'testFiles.get(packageName)?.has(assertion[1])', to: 'true',
    outputs: ['    foreign_test.go:42: private-value\n']},
  {name: 'timeout classification removed', from: 'state.timedOut.add(name)', to: 'void name',
    outputs: ['panic: test timed out after 15m0s\n']},
]) {
  test(`mutation killed: ${mutation.name}`, () => {
    const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-proof-mutation-')));
    try {
      const source = readFileSync(script, 'utf8');
      assert.equal(source.split(mutation.from).length, 2, 'mutation must target exactly one guard');
      const target = join(directory, 'mutant.mjs');
      writeFileSync(target, source.replace(mutation.from, mutation.to));
      const input = stream(diagnosedFailure(mutation.outputs));
      const expected = spawnSync(process.execPath, [script, ...diagnoseArgs], {input, encoding: 'utf8'});
      assert.equal(expected.status, 1);
      const fixture = {args: diagnoseArgs, input, output: expected.stdout, stderr: expected.stderr};
      assert.throws(() => assertFixture(target, fixture), {code: 'ERR_ASSERTION'});
    } finally {
      rmSync(directory, {recursive: true, force: true});
    }
  });
}

test('required-test mode rejects malformed arguments without reflecting their values', () => {
  for (const args of [['--require'], ['--require', ''], ['--private-fixture-marker'], ['--require', requiredTests.join(','), 'extra']]) {
    assertFixture(script, {args, input: stream(proofEvents), output: '', stderr: 'FAIL: invalid Go test checker arguments\n'});
  }
  for (const names of [['TestPrivate\nprivate-fixture-marker'], [`Test${'a'.repeat(64)}`], ['TestRealLifecycle', 'TestRealLifecycle']]) {
    assertFixture(script, {args: ['--require', names.join(',')], input: stream(proofEvents), output: proofSummary(requiredTests.length)});
  }
});

for (const [name, replacement, events, output] of [
  ['missing required children accepted', 'false', proofEvents.filter(value => value.Test !== requiredTests[1]), proofSummary(requiredTests.length - 1)],
  ['skipped required children accepted', "!['pass', 'fail', 'skip'].includes(state.tests.get(testName))",
    proofEvents.map(value => value.Test === requiredTests[1] && value.Action === 'pass' ? {...value, Action: 'skip'} : value),
    `package ${packages[0]} pass=${requiredTests.length - 1} skip=1 fail=0\nskip ${packages[0]} ${requiredTests[1]}\n`],
]) {
  test(`mutation killed: ${name}`, () => {
    const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-required-')));
    try {
      const source = readFileSync(script, 'utf8');
      const guard = "!['pass', 'fail'].includes(state.tests.get(testName))";
      assert.equal(source.split(guard).length, 2);
      const target = join(directory, 'mutant.mjs');
      writeFileSync(target, source.replace(guard, replacement).replace('state.skip !== 0 ||', ''));
      const fixture = {args: requiredArgs, input: stream(events), output};
      assertFixture(target, {...fixture, status: 0});
      assert.throws(() => assertFixture(target, fixture), {code: 'ERR_ASSERTION'});
    } finally {
      rmSync(directory, {recursive: true, force: true});
    }
  });
}

for (const mutation of [
  {name: 'ignoring a fail action', from: "if (action === 'fail') failed = true;", to: '', fixture: failures[1], changed: {status: 0, stderr: ''}},
  {name: 'accepting zero tests', from: 'if (state.tests.size === 0) invalid = true;', to: '', fixture: failures[2], changed: {status: 0, stderr: ''}},
  {name: 'accepting a truncated stream', from: 'if (pending.length !== 0) invalid = true;', to: '', fixture: failures[4], changed: {status: 0, stderr: ''}},
  {name: 'suppressing fail-name emission', from: "if (action === 'fail') {", to: 'if (false) {', fixture: failures[0],
    changed: {output: summary({[packages[0]]: {pass: 0, fail: 1}})}},
  {name: 'misclassifying a complete failing package as incomplete', from: 'if (!terminalActions.has(state.summary)) invalid = true;',
    to: "if (!['pass', 'skip'].includes(state.summary)) invalid = true;", fixture: completeFailure, changed: {stderr: diagnostic}},
  {name: 'printing an unallowlisted output line', from: 'if (allowlist.get(event.Package)?.has(message)) {', to: 'if (message) {',
    fixture: outputFailure(['private-fixture-marker\n']), changed: {output: outputFailure(['private-fixture-marker\n'], 'private-fixture-marker').output}},
]) {
  test(`mutation killed: ${mutation.name}`, () => {
    // Node resolves the module URL through symlinks; use the same canonical
    // path in argv so the mutant's CLI entrypoint really executes on macOS.
    const directory = realpathSync(mkdtempSync(join(tmpdir(), 'witself-go-json-mutation-')));
    try {
      const source = readFileSync(script, 'utf8');
      assert.equal(source.split(mutation.from).length, 2, 'mutation must target exactly one guard');
      const target = join(directory, 'mutant.mjs');
      writeFileSync(target, source.replace(mutation.from, mutation.to));
      assertFixture(target, {input: stream(passing), output: summary(), status: 0});
      assertFixture(target, {...mutation.fixture, ...mutation.changed});
      assert.throws(() => assertFixture(target, mutation.fixture), {code: 'ERR_ASSERTION'});
    } finally {
      rmSync(directory, {recursive: true, force: true});
    }
  });
}

const storePackage = 'github.com/witwave-ai/witself/internal/store';
const principalTest = 'TestSyncPrincipalBypassDenied';

test('explicit store package requires a complete non-skipped principal proof', async () => {
  for (const [action, valid] of [['pass', true], ['skip', false], ['fail', false]]) {
    const input = stream([event('start', storePackage), event('run', storePackage, principalTest),
      event(action, storePackage, principalTest), event(action === 'fail' ? 'fail' : 'pass', storePackage)]);
    const result = await checkGoTestJSON([input], [principalTest], {packageName: storePackage});
    assert.equal(result.valid, valid);
    assert.equal(result.lines[0], `package ${storePackage} pass=${Number(action === 'pass')} skip=${Number(action === 'skip')} fail=${Number(action === 'fail')}`);
    const cli = spawnSync(process.execPath, [script, '--package', storePackage, '--require', principalTest], {input, encoding: 'utf8'});
    assert.equal(cli.status, valid ? 0 : 1);
  }
});

test('store package cannot widen default or native package coverage', async () => {
  const input = stream(packageEvents(storePackage));
  assert.equal((await checkGoTestJSON([input])).valid, false);
  assert.equal((await checkGoTestJSON([input], ['TestFixture'])).valid, false);
  assert.equal((await checkGoTestJSON([stream(passing)], ['TestFixture'], {packageName: storePackage})).valid, false);
});

test('explicit package rejects absent tests zero tests skipped children and unreviewed packages', async () => {
  const options = {packageName: storePackage};
  for (const events of [
    [event('start', storePackage), event('pass', storePackage)],
    packageEvents(storePackage),
    [event('start', storePackage), event('run', storePackage, principalTest),
      event('run', storePackage, `${principalTest}/case`), event('skip', storePackage, `${principalTest}/case`),
      event('pass', storePackage, principalTest), event('pass', storePackage)],
  ]) assert.equal((await checkGoTestJSON([stream(events)], [principalTest], options)).valid, false);
  assert.equal((await checkGoTestJSON([stream(packageEvents(storePackage))], [], options)).valid, false);
  assert.equal((await checkGoTestJSON([], [principalTest], {packageName: 'private-marker'})).valid, false);
  for (const args of [
    ['--package', storePackage],
    ['--package', 'private-marker', '--require', principalTest],
    ['--package', storePackage, '--package', storePackage, '--require', principalTest],
    ['--package', '--require', principalTest],
  ]) {
    const result = spawnSync(process.execPath, [script, ...args], {input: '', encoding: 'utf8'});
    assert.equal(result.status, 1);
    assert.equal(result.stderr, 'FAIL: invalid Go test checker arguments\n');
    assert.equal(result.stdout, '');
  }
});


test('store proof classifies only reviewed cluster-helper fixed assertions', () => {
  withDiagnosticSources(target => {
    const directory = join(target, '..', '..', 'internal', 'testenv');
    mkdirSync(directory, {recursive: true});
    writeFileSync(join(directory, 'disposable_cluster.go'), `package testenv
func DisposableCluster(t testing.TB) {
 t.Fatal("disposable cluster create role failed")
 t.Fatalf("disposable cluster setup failure: %s", privateValue)
 t.Fatalf("private %s", privateValue)
 t.Fatal("disposable cluster bad " + privateValue)
 // t.Fatal("disposable cluster commented assertion")
}
`);
    for (const [output, why] of [
      ['disposable cluster create role failed', 'disposable cluster create role failed'],
      ['disposable cluster setup failure: private-marker', 'disposable cluster setup failure'],
      ['disposable cluster create role failed private-marker', '(unclassified)'],
      ['private private-marker', '(unclassified)'],
      ['disposable cluster bad private-marker', '(unclassified)'],
      ['disposable cluster commented assertion', '(unclassified)'],
    ]) {
      const input = stream([event('start', storePackage), event('run', storePackage, principalTest),
        event('output', storePackage, principalTest, {Output: `    disposable_cluster.go:12: ${output}\n`}),
        event('fail', storePackage, principalTest), event('fail', storePackage)]);
      assertFixture(target, {input, status: 1, stderr: failureDiagnostic,
        args: ['--package', storePackage, '--require', principalTest],
        output: `package ${storePackage} pass=0 skip=0 fail=1\nfail ${storePackage} ${principalTest}\nwhy ${storePackage} ${principalTest} :: ${why}\n`});
    }
    const native = stream([event('start', packages[0]), event('run', packages[0], principalTest),
      event('output', packages[0], principalTest, {Output: '    disposable_cluster.go:12: disposable cluster create role failed\n'}),
      event('fail', packages[0], principalTest), event('fail', packages[0])]);
    assertFixture(target, {input: native, status: 1, stderr: failureDiagnostic, args: ['--require', principalTest],
      output: `package ${packages[0]} pass=0 skip=0 fail=1\nfail ${packages[0]} ${principalTest}\nwhy ${packages[0]} ${principalTest} :: (unclassified)\n`});
  });
});
