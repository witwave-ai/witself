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

function assertFixture(target, {input, output, status = 1, stderr = status === 0 ? '' : diagnostic}) {
  const result = spawnSync(process.execPath, [target], {input, encoding: 'utf8'});
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
