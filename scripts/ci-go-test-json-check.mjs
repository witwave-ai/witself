#!/usr/bin/env node
// A successful native command and a complete JSON stream are both required by
// the workflow. Only exact source-approved diagnostics may leave the JSON stream.
import {createReadStream, readFileSync, readdirSync} from 'node:fs';
import {pathToFileURL} from 'node:url';

export const expectedPackages = Object.freeze([
  'github.com/witwave-ai/witself/internal/localcell',
  'github.com/witwave-ai/witself/internal/runtimeconfig',
  'github.com/witwave-ai/witself/internal/tools/localcellharness',
]);

const terminalActions = new Set(['pass', 'skip', 'fail']);
const serviceProofTest = 'TestNativeServiceLifecycle';
const serviceOutcomes = new Set([
  'PASS service_manager_available', 'NOT_RUN service_manager_unavailable',
  'NOT_RUN service_adapter_unsupported', 'NOT_RUN native_postgres_unconfigured',
  'PASS service_manager_persistence', 'PASS service_foreground_persistence',
]);
// Go replaces whitespace in subtest names with underscores. Accept the names
// used by these suites, including empty subtest segments, addresses, and #NN.
const testNamePattern = /^(?:Test|Example|Fuzz)[A-Za-z0-9_]*(?:\/[A-Za-z0-9_.,:=+\-[\]#]*)*$/;

function sourceLiterals(source, assertions) {
  // Tokenize comments and strings as whole units, so examples inside comments,
  // string contents, and concatenated or formatted expressions cannot enroll
  // themselves as diagnostics. Unsupported Go string escapes fail closed.
  const tokens = (source.match(/\/\/[^\n]*|\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|`[^`]*`|'(?:\\.|[^'\\])*'|[A-Za-z_][A-Za-z0-9_]*|[^\s]/g) ?? [])
    .filter(token => !token.startsWith('//') && !token.startsWith('/*'));
  const literals = [];
  for (let index = 0; index + 5 < tokens.length; index++) {
    const [receiver, dot, method, open, literal, close] = tokens.slice(index, index + 6);
    if (dot !== '.' || open !== '(' || close !== ')' || !literal.startsWith('"')) continue;
    if (assertions ? !/^(?:Fatal|Error|Fatalf|Errorf)$/.test(method) || !/^[A-Za-z_]\w*$/.test(receiver)
      : receiver !== 'errors' || method !== 'New') continue;
    let value;
    try { value = JSON.parse(literal); } catch { continue; }
    // Production errors may contain their fixed package prefix. Assertions are
    // stricter: no paths, formatting directives, long numbers, =, : or \\.
    if (!/^[A-Za-z][A-Za-z0-9 _.,;:!?()'"-]*$/.test(value) || /\d{3}|[a-f0-9]{64}/i.test(value)) continue;
    if (assertions && /[:=\\/]/.test(value)) continue;
    literals.push(value);
  }
  return literals;
}

function diagnosticAllowlist() {
  const errors = new Set();
  const assertions = new Map(expectedPackages.map(name => [name, new Set()]));
  const testFiles = new Map(expectedPackages.map(name => [name, new Set()]));
  for (const name of expectedPackages) {
    const directory = new URL(`../${name.slice('github.com/witwave-ai/witself/'.length)}/`, import.meta.url);
    let files;
    try { files = readdirSync(directory).filter(file => file.endsWith('.go')); } catch { continue; }
    for (const file of files) {
      let source;
      try { source = readFileSync(new URL(file, directory), 'utf8'); } catch { continue; }
      const isTest = file.endsWith('_test.go');
      if (isTest) testFiles.get(name).add(file);
      if (!isTest && name === expectedPackages[2]) continue;
      for (const literal of sourceLiterals(source, isTest)) {
        (isTest ? assertions.get(name) : errors).add(literal);
      }
    }
  }
  // The helpers' fixed errors propagate across package boundaries. Assertions
  // remain local to their package. A copied script with no sources emits only
  // unclassified reasons; a missing source never licenses arbitrary output.
  return {messages: new Map([...assertions].map(([name, messages]) => [name, new Set([...errors, ...messages])])), testFiles};
}

const {messages: allowlist, testFiles} = diagnosticAllowlist();

// Mask before truncating so a secret crossing the display boundary cannot leave
// an unmasked prefix. Password-file/environment fragments are suppressed to the
// end of their line because they can contain quoted paths or escaped whitespace.
function maskedDiagnostic(value, limit) {
  return value.replace(/[a-f0-9]{64}/gi, '[MASKED]')
    .replace(/\b(?:pwfile|PGPASSWORD)\b[^\r\n]*/gi, '[MASKED]')
    .replace(/\bpassword\s*=[^\r\n]*/gi, 'password=[MASKED]')
    // Any user:password@ pair, whatever punctuation the password carries (an
    // apostrophe or quote is a valid URI password character), is masked whole.
    .replace(/[^\s@/]+:[^\s@/]+@/g, '[MASKED]@')
    .replace(/[\x00-\x1f\x7f]/g, ' ').slice(0, limit);
}

function proofDiagnostic(line, packageName) {
  const assertion = /^[ \t]*([A-Za-z0-9_.-]+_test\.go):[1-9][0-9]*: /.exec(line);
  if (!/^[ \t]*--- FAIL: /.test(line) && !(assertion && testFiles.get(packageName)?.has(assertion[1]))) return null;
  return maskedDiagnostic(line.replace(/\r$/, ''), 200);
}

// --require selects the native PostgreSQL proof, which runs only localcell.
// The ordinary three-package unit mode retains its existing skip policy.
export async function checkGoTestJSON(input, requiredTests = [], {diagnose = false} = {}) {
  const packageNames = requiredTests.length === 0 ? expectedPackages : expectedPackages.slice(0, 1);
  const packages = new Map(packageNames.map(name => [name, {
    started: false, summary: null, summaryLine: null, tests: new Map(), why: new Map(), diagnostics: new Map(),
    timedOut: new Set(), serviceOutcomes: new Set(), pass: 0, skip: 0, fail: 0,
  }]));
  let invalid = requiredTests.some(name => typeof name !== 'string' || !testNamePattern.test(name) || /[a-f0-9]{64}/i.test(name)) ||
    new Set(requiredTests).size !== requiredTests.length;
  let failed = false;
  let pending = '';
  let firstInvalidLine = null;
  let records = 0;
  const required = new Set(requiredTests);
  const diagnoseProof = diagnose && requiredTests.length !== 0;

  function reject(line) {
    invalid = true;
    firstInvalidLine ??= maskedDiagnostic(line, 120);
  }

  function classifyTimeout(state, output) {
    if (typeof output !== 'string' || !/^panic: test timed out(?:\s|$)/m.test(output)) return;
    for (const [name, action] of state.tests) if (!terminalActions.has(action)) state.timedOut.add(name);
  }

  function consume(line) {
    records++;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      reject(line);
      return;
    }
    if (!event || typeof event !== 'object' || Array.isArray(event)) {
      reject(line);
      return;
    }
    const action = event.Action;
    if (action === 'fail') failed = true;
    if (action === 'build-fail') reject(line);
    // Recent Go versions also send build events for dependencies. Their output
    // is deliberately discarded; build failures still latch failure above.
    if (action === 'build-output' || action === 'build-fail') return;
    const state = packages.get(event.Package);
    if (!state || state.summary !== null) {
      reject(line);
      return;
    }
    const name = event.Test;
    if (name !== undefined && (typeof name !== 'string' || !testNamePattern.test(name) || /[a-f0-9]{64}/i.test(name))) {
      reject(line);
      return;
    }
    if (action === 'start' && name === undefined && !state.started) {
      state.started = true;
      return;
    }
    if (!state.started) {
      reject(line);
      return;
    }
    if (name === undefined) {
      if (terminalActions.has(action)) {
        state.summary = action;
        state.summaryLine = line;
      } else if (action !== 'output') reject(line);
      if (action === 'output') classifyTimeout(state, event.Output);
      return;
    }
    const previous = state.tests.get(name);
    if (action === 'run' && previous === undefined) state.tests.set(name, 'running');
    else if (action === 'pause' && previous === 'running') state.tests.set(name, 'paused');
    else if (action === 'cont' && previous === 'paused') state.tests.set(name, 'running');
    else if (action === 'output' && previous !== undefined && !terminalActions.has(previous)) {
      if (typeof event.Output === 'string') {
        classifyTimeout(state, event.Output);
        if (name === serviceProofTest && event.Package === expectedPackages[0]) {
          for (const line of event.Output.split('\n')) {
            const message = line.replace(/^[ \t]*(?:service_native_test\.go:[0-9]+: )?/, '').replace(/\r$/, '');
            if (serviceOutcomes.has(message)) state.serviceOutcomes.add(message);
          }
        }
        if (diagnoseProof && required.has(name)) {
          const diagnostics = state.diagnostics.get(name) ?? [];
          for (const outputLine of event.Output.split('\n')) {
            // Three forty-line log tails plus bounded cause/identity details.
            if (diagnostics.length === 128) break;
            const diagnostic = proofDiagnostic(outputLine, event.Package);
            if (diagnostic !== null) diagnostics.push(diagnostic);
          }
          state.diagnostics.set(name, diagnostics);
        }
      }
      if (!state.why.has(name) && typeof event.Output === 'string') {
        for (const line of event.Output.split('\n')) {
          const message = line.replace(/^[ \t]*(?:[A-Za-z0-9_.-]+\.go:[0-9]+: )?/, '').replace(/\r$/, '');
          if (allowlist.get(event.Package)?.has(message)) {
            state.why.set(name, message);
            break;
          }
        }
      }
      return;
    }
    else if (terminalActions.has(action) && (previous === 'running' || previous === 'paused')) {
      state.tests.set(name, action);
      state[action]++;
    } else reject(line);
  }

  try {
    for await (const chunk of input) {
      pending += chunk;
      let newline;
      while ((newline = pending.indexOf('\n')) !== -1) {
        consume(pending.slice(0, newline));
        pending = pending.slice(newline + 1);
      }
    }
  } catch {
    invalid = true;
  }
  // Even a syntactically complete final object needs its record delimiter.
  if (pending.length !== 0) invalid = true;
  if (pending.length !== 0) firstInvalidLine ??= maskedDiagnostic(pending, 120);
  const lines = [];
  for (const [name, state] of packages) {
    if (!terminalActions.has(state.summary)) invalid = true;
    if (state.tests.size === 0) invalid = true;
    if ([...state.tests.values()].some(action => !terminalActions.has(action))) invalid = true;
    // A terminal failure is a complete observation, not a malformed proof.
    // Missing/skipped requirements still cannot establish that the proof ran.
    const incompleteProof = requiredTests.length !== 0 && (state.summary === 'skip' || state.skip !== 0 ||
      requiredTests.some(testName => !['pass', 'fail'].includes(state.tests.get(testName))));
    if (incompleteProof) invalid = true;
    if (required.has(serviceProofTest) && state.tests.get(serviceProofTest) === 'pass') {
      const outcomes = state.serviceOutcomes;
      const supported = outcomes.has('PASS service_manager_available') && outcomes.has('PASS service_manager_persistence');
      const foreground = outcomes.has('NOT_RUN service_manager_unavailable') && outcomes.has('PASS service_foreground_persistence');
      const unsupported = outcomes.has('NOT_RUN service_adapter_unsupported');
      if (outcomes.has('NOT_RUN native_postgres_unconfigured') || Number(supported) + Number(foreground) + Number(unsupported) !== 1 ||
        outcomes.size !== (unsupported ? 1 : 2)) invalid = true;
    }
    if (invalid && firstInvalidLine === null && state.summaryLine !== null &&
      (state.tests.size === 0 || [...state.tests.values()].some(action => !terminalActions.has(action)) ||
        incompleteProof)) firstInvalidLine = maskedDiagnostic(state.summaryLine, 120);
    lines.push(`package ${name} pass=${state.pass} skip=${state.skip} fail=${state.fail}`);
    for (const outcome of [...state.serviceOutcomes].sort()) lines.push(`capability ${name} ${serviceProofTest} :: ${outcome}`);
    for (const [testName, action] of [...state.tests].sort(([left], [right]) => left.localeCompare(right))) {
      if (action === 'skip') lines.push(`skip ${name} ${testName}`);
      if (action === 'fail') {
        lines.push(`fail ${name} ${testName}`);
        lines.push(`why ${name} ${testName} :: ${state.timedOut.has(testName) ? 'test timed out' : state.why.get(testName) ?? '(unclassified)'}`);
        for (const diagnostic of state.diagnostics.get(testName) ?? []) lines.push(`diagnose ${name} ${testName} :: ${diagnostic}`);
      }
    }
    // A timeout may stop the stream before Go emits individual fail records.
    for (const [testName, action] of state.tests) {
      if (state.timedOut.has(testName) && !terminalActions.has(action)) lines.push(`why ${name} ${testName} :: test timed out`);
    }
  }
  const reason = invalid ? 'FAIL: invalid or incomplete Go test JSON stream'
    : failed ? 'FAIL: Go test failures' : null;
  if (invalid && diagnoseProof) lines.push(`stream records=${records} :: ${firstInvalidLine === null
    ? 'stream ended before the package summary' : `first offending line: ${firstInvalidLine}`}`);
  return {valid: reason === null, lines, reason};
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const args = process.argv.slice(2);
  const options = {};
  let argumentsValid = true;
  for (let index = 0; index < args.length; index++) {
    const key = args[index];
    if (!['--require', '--input', '--diagnose'].includes(key) || options[key] !== undefined) {
      argumentsValid = false;
      break;
    }
    options[key] = key === '--diagnose' ? true : args[++index];
    if (!options[key] || (typeof options[key] === 'string' && options[key].startsWith('--'))) argumentsValid = false;
  }
  if (options['--diagnose'] && !options['--require']) argumentsValid = false;
  if (!argumentsValid) {
    console.error('FAIL: invalid Go test checker arguments');
    process.exitCode = 1;
  } else {
    const input = options['--input'] ? createReadStream(options['--input'], {encoding: 'utf8'}) : process.stdin.setEncoding('utf8');
    const result = await checkGoTestJSON(input, options['--require']?.split(',') ?? [], {diagnose: options['--diagnose']});
    console.log(result.lines.join('\n'));
    if (!result.valid) console.error(result.reason);
    process.exitCode = result.valid ? 0 : 1;
  }
}
