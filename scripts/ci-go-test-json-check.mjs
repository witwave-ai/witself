#!/usr/bin/env node
// A successful native command and a complete JSON stream are both required by
// the workflow. Only exact source-approved diagnostics may leave the JSON stream.
import {readFileSync, readdirSync} from 'node:fs';
import {pathToFileURL} from 'node:url';

export const expectedPackages = Object.freeze([
  'github.com/witwave-ai/witself/internal/localcell',
  'github.com/witwave-ai/witself/internal/runtimeconfig',
  'github.com/witwave-ai/witself/internal/tools/localcellharness',
]);

const terminalActions = new Set(['pass', 'skip', 'fail']);
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
  for (const name of expectedPackages) {
    const directory = new URL(`../${name.slice('github.com/witwave-ai/witself/'.length)}/`, import.meta.url);
    let files;
    try { files = readdirSync(directory).filter(file => file.endsWith('.go')); } catch { continue; }
    for (const file of files) {
      let source;
      try { source = readFileSync(new URL(file, directory), 'utf8'); } catch { continue; }
      const isTest = file.endsWith('_test.go');
      if (!isTest && name === expectedPackages[2]) continue;
      for (const literal of sourceLiterals(source, isTest)) {
        (isTest ? assertions.get(name) : errors).add(literal);
      }
    }
  }
  // The helpers' fixed errors propagate across package boundaries. Assertions
  // remain local to their package. A copied script with no sources emits only
  // unclassified reasons; a missing source never licenses arbitrary output.
  return new Map([...assertions].map(([name, messages]) => [name, new Set([...errors, ...messages])]));
}

const allowlist = diagnosticAllowlist();

export async function checkGoTestJSON(input) {
  const packages = new Map(expectedPackages.map(name => [name, {
    started: false, summary: null, tests: new Map(), why: new Map(), pass: 0, skip: 0, fail: 0,
  }]));
  let invalid = false;
  let failed = false;
  let pending = '';

  function consume(line) {
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      invalid = true;
      return;
    }
    if (!event || typeof event !== 'object' || Array.isArray(event)) {
      invalid = true;
      return;
    }
    const action = event.Action;
    if (action === 'fail') failed = true;
    if (action === 'build-fail') invalid = true;
    // Recent Go versions also send build events for dependencies. Their output
    // is deliberately discarded; build failures still latch failure above.
    if (action === 'build-output' || action === 'build-fail') return;
    const state = packages.get(event.Package);
    if (!state || state.summary !== null) {
      invalid = true;
      return;
    }
    const name = event.Test;
    if (name !== undefined && (typeof name !== 'string' || !testNamePattern.test(name) || /[a-f0-9]{64}/i.test(name))) {
      invalid = true;
      return;
    }
    if (action === 'start' && name === undefined && !state.started) {
      state.started = true;
      return;
    }
    if (!state.started) {
      invalid = true;
      return;
    }
    if (name === undefined) {
      if (terminalActions.has(action)) state.summary = action;
      else if (action !== 'output') invalid = true;
      return;
    }
    const previous = state.tests.get(name);
    if (action === 'run' && previous === undefined) state.tests.set(name, 'running');
    else if (action === 'pause' && previous === 'running') state.tests.set(name, 'paused');
    else if (action === 'cont' && previous === 'paused') state.tests.set(name, 'running');
    else if (action === 'output' && previous !== undefined && !terminalActions.has(previous)) {
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
    } else invalid = true;
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
  const lines = [];
  for (const [name, state] of packages) {
    if (!terminalActions.has(state.summary)) invalid = true;
    if (state.tests.size === 0) invalid = true;
    if ([...state.tests.values()].some(action => !terminalActions.has(action))) invalid = true;
    lines.push(`package ${name} pass=${state.pass} skip=${state.skip} fail=${state.fail}`);
    for (const [testName, action] of [...state.tests].sort(([left], [right]) => left.localeCompare(right))) {
      if (action === 'skip') lines.push(`skip ${name} ${testName}`);
      if (action === 'fail') {
        lines.push(`fail ${name} ${testName}`);
        lines.push(`why ${name} ${testName} :: ${state.why.get(testName) ?? '(unclassified)'}`);
      }
    }
  }
  const reason = invalid ? 'FAIL: invalid or incomplete Go test JSON stream'
    : failed ? 'FAIL: Go test failures' : null;
  return {valid: reason === null, lines, reason};
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.stdin.setEncoding('utf8');
  const result = await checkGoTestJSON(process.stdin);
  console.log(result.lines.join('\n'));
  if (!result.valid) console.error(result.reason);
  process.exitCode = result.valid ? 0 : 1;
}
