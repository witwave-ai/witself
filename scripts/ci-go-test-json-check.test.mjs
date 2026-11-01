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

test('why diagnostics accept only exact fixed errors and package-local literal assertions', () => {
  withDiagnosticSources(target => {
    for (const message of ['localcell: fixed private error', 'owner-is-admin-group', 'ace-count', 'assertion with 12 safe digits']) {
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
  'InstallStartRestartRemove', 'TwoCellsAreIndependent', 'InterruptedStartRepair', 'WaitsForReadiness',
].map(name => `TestRealLifecycle/${name}`)];
const requiredArgs = ['--require', requiredTests.join(',')];
const proofEvents = [event('start', packages[0]), event('run', packages[0], requiredTests[0]),
  ...requiredTests.slice(1).flatMap(name => [event('run', packages[0], name), event('pass', packages[0], name)]),
  event('pass', packages[0], requiredTests[0]), event('pass', packages[0])];
const proofSummary = count => `package ${packages[0]} pass=${count} skip=0 fail=0\n`;

test('required-test mode accepts the complete native proof without requiring unrelated unit packages', () => {
  assertFixture(script, {args: requiredArgs, input: stream(proofEvents), output: proofSummary(5), status: 0});
});

for (const required of requiredTests) {
  test(`required-test mode rejects missing ${required}`, () => {
    assertFixture(script, {args: requiredArgs,
      input: stream(proofEvents.filter(value => value.Test !== required)), output: proofSummary(4)});
  });
  test(`required-test mode rejects skipped ${required}`, () => {
    assertFixture(script, {args: requiredArgs,
      input: stream(proofEvents.map(value => value.Test === required && value.Action === 'pass' ? {...value, Action: 'skip'} : value)),
      output: `package ${packages[0]} pass=4 skip=1 fail=0\nskip ${packages[0]} ${required}\n`});
  });
}

test('required-test mode rejects truncated proof and package skip', () => {
  assertFixture(script, {args: requiredArgs, input: stream(proofEvents).trimEnd(), output: proofSummary(5)});
  for (const Action of ['skip']) {
    assertFixture(script, {args: requiredArgs,
      input: stream([...proofEvents.slice(0, -1), event(Action, packages[0])]), output: proofSummary(5)});
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
    output: `package ${packages[0]} pass=5 skip=1 fail=0\nskip ${packages[0]} ${extra}\n`});
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
    `${prefix}${'x'.repeat(160)}${'b'.repeat(64)}\n`,
    `${prefix}${'x'.repeat(250)}\n`,
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
  assert.equal(diagnostics[9], `${prefix}${'x'.repeat(160)}[MASKED]`);
  assert(!result.lines.join('\n').includes('private-'));
  assert(!/[a-f0-9]{64}/i.test(result.lines.join('\n')));
});

test('diagnose reports the first invalid line masked and bounded with the observed record count', async () => {
  const invalid = `postgres://private-user:private-secret@127.0.0.1 ${'a'.repeat(64)} ${'x'.repeat(200)}`;
  const result = await checkGoTestJSON([stream(proofEvents) + `${invalid}\nsecond-invalid-line\n`], requiredTests, {diagnose: true});
  const detail = result.lines.find(line => line.startsWith('stream '));
  assert.equal(detail, `stream records=${proofEvents.length + 2} :: first offending line: ${`postgres://[MASKED]@127.0.0.1 [MASKED] ${'x'.repeat(200)}`.slice(0, 120)}`);
  assert(!detail.includes('private-'));
  assert(!detail.includes('second-invalid-line'));
  for (const fragment of ['password="private password with spaces"', 'PGPASSWORD=private-value', '--pwfile="private path"']) {
    const rejected = await checkGoTestJSON([stream(proofEvents) + fragment], requiredTests, {diagnose: true});
    assert(!rejected.lines.join('\n').includes('private'));
    assert(rejected.lines.some(line => line.startsWith(`stream records=${proofEvents.length} :: first offending line: `)));
  }
});

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
    assertFixture(script, {args: [...requiredArgs, '--input', input, '--diagnose'], input: 'invalid stdin\n', output: proofSummary(5), status: 0});
    writeFileSync(input, stream(proofEvents.slice(0, -1)));
    assertFixture(script, {args: [...requiredArgs, '--input', input, '--diagnose'], input: '',
      output: `${proofSummary(5)}stream records=${proofEvents.length - 1} :: stream ended before the package summary\n`});
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
    outputs: [`--- FAIL: TestRealLifecycle ${'x'.repeat(250)}\n`]},
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
    assertFixture(script, {args: ['--require', names.join(',')], input: stream(proofEvents), output: proofSummary(5)});
  }
});

for (const [name, replacement, events, output] of [
  ['missing required children accepted', 'false', proofEvents.filter(value => value.Test !== requiredTests[1]), proofSummary(4)],
  ['skipped required children accepted', "!['pass', 'fail', 'skip'].includes(state.tests.get(testName))",
    proofEvents.map(value => value.Test === requiredTests[1] && value.Action === 'pass' ? {...value, Action: 'skip'} : value),
    `package ${packages[0]} pass=4 skip=1 fail=0\nskip ${packages[0]} ${requiredTests[1]}\n`],
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
