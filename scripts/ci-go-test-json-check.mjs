#!/usr/bin/env node
// A successful native command and a complete JSON stream are both required by
// the workflow. Only source-approved diagnostic text may leave the JSON stream.
import {createReadStream, readFileSync, readdirSync, realpathSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

export const expectedPackages = Object.freeze([
  'github.com/witwave-ai/witself/internal/localcell',
  'github.com/witwave-ai/witself/internal/runtimeconfig',
  'github.com/witwave-ai/witself/internal/tools/localcellharness',
]);

const storePackage = 'github.com/witwave-ai/witself/internal/store';
const selectablePackages = Object.freeze([...expectedPackages, storePackage]);

const terminalActions = new Set(['pass', 'skip', 'fail']);
// The three-package unit step has one -timeout for a whole package; show
// where its wall clock goes before growth turns into a timed-out stream.
const slowTestSeconds = 5;
const slowTestLimit = 10;
const serviceProofTest = 'TestNativeServiceLifecycle';
// Every manager stage has both a passing child assertion and one ordered,
// value-free receipt. Capability or foreground success cannot satisfy this mode.
export const serviceManagerStages = Object.freeze([
  'query_typed', 'registration', 'on_demand', 'readiness_write', 'same_boot_restart',
  'persisted_read', 'durable_stop', 'unregister', 'resources_closed', 'bystander_retained',
]);
const managerReceipt = /^PASS service_manager_stage=([a-z_]+)$/;
const managerMissing = new Set([
  'NOT_RUN service_manager_receipt_missing',
  'NOT_RUN windows_task_query_result_untyped',
  'NOT_RUN windows_task_interactive_token_unavailable',
  'NOT_RUN windows_task_manager_unavailable',
]);
const managerEvidence = /(?:service_manager_|service_foreground_persistence|service_adapter_unsupported|native_postgres_unconfigured|service_shutdown_channel|windows_task_)/;
function serviceEvidenceMessage(line) {
  return line.replace(/^[ \t]*(?:[A-Za-z0-9_.-]+_test\.go:[0-9]+: )?/, '').replace(/\r$/, '');
}
const serviceShutdownReceipt = /^service_shutdown phase=(startup|tick|control|idle) operation=(none|reconcile|repair|stop|start|health) journal_pending=(true|false) journal_read_ok=(true|false)$/;
const serviceOutcomes = new Set([
  'PASS service_manager_available', 'NOT_RUN service_manager_unavailable',
  'NOT_RUN service_adapter_unsupported', 'NOT_RUN native_postgres_unconfigured',
  'PASS service_manager_persistence', 'PASS service_foreground_persistence', 'PASS service_shutdown_channel',
]);
// Go replaces whitespace in subtest names with underscores. Accept the names
// used by these suites, including empty subtest segments, addresses, and #NN.
const testNamePattern = /^(?:Test|Example|Fuzz)[A-Za-z0-9_]*(?:\/[A-Za-z0-9_.,:=+\-[\]#]*)*$/;

function sourceLiterals(source, assertions, fixedCauses = false) {
  // Tokenize comments and strings as whole units, so examples inside comments,
  // string contents, and concatenated expressions cannot enroll themselves as
  // diagnostics. Unsupported Go string escapes fail closed. Formatted assertions
  // enroll only their fixed prefix, never their arguments or formatted output.
  const tokens = (source.match(/\/\/[^\n]*|\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|`[^`]*`|'(?:\\.|[^'\\])*'|[A-Za-z_][A-Za-z0-9_]*|[^\s]/g) ?? [])
    .filter(token => !token.startsWith('//') && !token.startsWith('/*'));
  const literals = [];
  const prefixes = [];
  const classPrefixes = [];
  const vocabulary = new Set();
  if (assertions) {
    for (const token of tokens) {
      if (!token.startsWith('"')) continue;
      let value;
      try { value = JSON.parse(token); } catch { continue; }
      if (!/^[a-z _=,-]{1,80}$/.test(value) || /[\r\n]/.test(value)) continue;
      for (const word of value.split(/[ ,=]/)) {
        if (/^[a-z][a-z_-]{0,39}$/.test(word)) vocabulary.add(word);
      }
    }
  }
  if (fixedCauses) {
    // Enroll only complete direct var initializers in fixed_cause.go. Token
    // boundaries exclude comments, quoted examples, calls and expressions.
    // A declaration/semicolon/end boundary after the call excludes suffixes.
    for (let index = 0; index + 6 < tokens.length; index++) {
      const [declaration, name, assign, conversion, open, literal, close, next] = tokens.slice(index, index + 8);
      if (declaration !== 'var' || !/^[A-Za-z_]\w*$/.test(name) || assign !== '=' || conversion !== 'fixedCause' ||
        open !== '(' || close !== ')' || !literal.startsWith('"') ||
        (next !== undefined && !['var', 'const', 'func', 'type', ';'].includes(next))) continue;
      let value;
      try { value = JSON.parse(literal); } catch { continue; }
      if (/^[A-Za-z][A-Za-z0-9 _.,;:!?()'"-]*$/.test(value) && !/\d{3}|[a-f0-9]{64}/i.test(value)) literals.push(value);
    }
  }
  for (let index = 0; index + 5 < tokens.length; index++) {
    const [receiver, dot, method, open, literal, close] = tokens.slice(index, index + 6);
    if (dot !== '.' || open !== '(' || !literal.startsWith('"')) continue;
    if (assertions && /^[A-Za-z_]\w*$/.test(receiver) && method === 'Logf' && close === ',') {
      let value;
      try { value = JSON.parse(literal); } catch { continue; }
      const prefix = value.slice(0, value.indexOf('%'));
      if (value.endsWith('%s') && value.indexOf('%') === value.length - 2 &&
        /(?:class: |class=)$/.test(prefix) && prefix.length >= 12 &&
        /^[A-Za-z][A-Za-z0-9 _.,;:!?()'"=-]*$/.test(prefix) && !/\d{3}|[a-f0-9]{64}/i.test(prefix)) {
        classPrefixes.push(prefix);
      }
      continue;
    }
    if (assertions ? !/^(?:Fatal|Error|Fatalf|Errorf)$/.test(method) || !/^[A-Za-z_]\w*$/.test(receiver)
      : receiver !== 'errors' || method !== 'New') continue;
    let value;
    try { value = JSON.parse(literal); } catch { continue; }
    const format = value.indexOf('%');
    if (assertions && /^(?:Fatalf|Errorf)$/.test(method) && format !== -1 && (close === ',' || close === ')')) {
      const match = value.slice(0, format);
      // Keep the full source prefix (including its value separator) for matching,
      // but omit a final colon from the value-free label shown to the operator.
      const message = match.trimEnd().replace(/:$/, '').trimEnd();
      if (message.length < 12 || !/^[A-Za-z][A-Za-z0-9 _.,;:!?()'"-]*$/.test(message) ||
        !/^[A-Za-z][A-Za-z0-9 _.,;:!?()'"-]*$/.test(match) || /\d{3}|[a-f0-9]{64}/i.test(message)) continue;
      prefixes.push({match, message});
      continue;
    }
    if (close !== ')') continue;
    // Production errors may contain their fixed package prefix. Exact assertions
    // are stricter: no paths, formatting directives, long numbers, =, : or \\.
    if (!/^[A-Za-z][A-Za-z0-9 _.,;:!?()'"-]*$/.test(value) || /\d{3}|[a-f0-9]{64}/i.test(value)) continue;
    if (assertions && /[:=\\/]/.test(value)) continue;
    literals.push(value);
  }
  return {literals, prefixes, classPrefixes, vocabulary};
}

function diagnosticAllowlist() {
  const errors = new Set();
  const storeErrors = new Set();
  const assertions = new Map(selectablePackages.map(name => [name, new Set()]));
  const prefixes = new Map(selectablePackages.map(name => [name, new Map()]));
  const classes = new Map(selectablePackages.map(name => [name, {prefixes: new Set(), vocabulary: new Set()}]));
  const testFiles = new Map(selectablePackages.map(name => [name, new Set()]));
  for (const name of selectablePackages) {
    const directory = new URL(`../${name.slice('github.com/witwave-ai/witself/'.length)}/`, import.meta.url);
    let files;
    try { files = readdirSync(directory).filter(file => file.endsWith('.go')); } catch { continue; }
    for (const file of files) {
      let source;
      try { source = readFileSync(new URL(file, directory), 'utf8'); } catch { continue; }
      const isTest = file.endsWith('_test.go');
      if (isTest) testFiles.get(name).add(file);
      if (!isTest && name === expectedPackages[2]) continue;
      const sourceDiagnostics = sourceLiterals(source, isTest,
        !isTest && name === expectedPackages[0] && file === 'fixed_cause.go');
      for (const literal of sourceDiagnostics.literals) {
        (isTest ? assertions.get(name) : name === storePackage ? storeErrors : errors).add(literal);
      }
      for (const {match, message} of sourceDiagnostics.prefixes) prefixes.get(name).set(match, message);
      if (isTest) {
        for (const prefix of sourceDiagnostics.classPrefixes) classes.get(name).prefixes.add(prefix);
        for (const word of sourceDiagnostics.vocabulary) classes.get(name).vocabulary.add(word);
      }
    }
  }
  // The store proof's cluster fixture reports setup/cleanup failures through
  // testing.TB from one reviewed helper. Enroll only literal assertions and the
  // same safe fixed prefixes; never enable raw helper output or native labels.
  try {
    const helper = readFileSync(new URL('../internal/testenv/disposable_cluster.go', import.meta.url), 'utf8');
    const diagnostics = sourceLiterals(helper, true);
    for (const literal of diagnostics.literals) assertions.get(storePackage).add(literal);
    for (const {match, message} of diagnostics.prefixes) prefixes.get(storePackage).set(match, message);
  } catch { /* Missing fixture sources never authorize arbitrary diagnostics. */ }
  // The helpers' fixed errors propagate across package boundaries. Assertions
  // remain local to their package. A copied script with no sources emits only
  // unclassified reasons; a missing source never licenses arbitrary output.
  return {messages: new Map([...assertions].map(([name, messages]) => [name, new Set([...(name === storePackage ? storeErrors : errors), ...messages])])), prefixes, classes, testFiles};
}

const {messages: allowlist, prefixes: prefixAllowlist, classes: classAllowlist, testFiles} = diagnosticAllowlist();

function classPrefix(message, packageName) {
  // Prefer the most specific enrolled format when fixed prefixes overlap.
  let match;
  for (const prefix of classAllowlist.get(packageName)?.prefixes ?? []) {
    if (message.startsWith(prefix) && (match === undefined || prefix.length > match.length)) match = prefix;
  }
  return match;
}

function classDiagnostic(message, prefix, packageName) {
  const value = message.slice(prefix.length);
  const vocabulary = classAllowlist.get(packageName).vocabulary;
  const accepted = value.length <= 160 && /^[a-z][a-z_-]*(?:[ ,=][a-z][a-z_-]*)*$/.test(value) &&
    !/[\r\n]/.test(value) && value.split(/[ ,=]/).every(word => vocabulary.has(word));
  return `${prefix}${accepted ? value : '(rejected)'}`;
}

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
    .replace(/S-1-(?:\d+-)*\d+/gi, '[MASKED]')
    .replace(/\\\\\.\\pipe\\[^\r\n]*/gi, '[MASKED]')
    // Paths can contain spaces: discard the remaining line rather than leave
    // part of an unquoted path. Include drive-rooted and UNC/device paths.
    .replace(/(?:[a-z]:(?:\\|\/(?!\/))|\\\\)[^\r\n]*/gi, '[MASKED]')
    .replace(/\\?Witself-Cell-[a-z0-9_-]+/gi, '[MASKED]')
    .replace(/[a-z0-9+/]{32,}={0,2}/gi, '[MASKED]')
    .replace(/[\x00-\x1f\x7f]/g, ' ').slice(0, limit);
}

function proofDiagnostic(line, packageName) {
  const assertion = /^[ \t]*([A-Za-z0-9_.-]+_test\.go):[1-9][0-9]*: /.exec(line);
  if (!/^[ \t]*--- FAIL: /.test(line) && !(assertion && testFiles.get(packageName)?.has(assertion[1]))) return null;
  return maskedDiagnostic(line.replace(/\r$/, ''), 200);
}

// --require defaults to the native localcell proof. An explicit --package
// selects one reviewed package and requires named tests; default unit coverage
// and its skip policy remain unchanged.
export async function checkGoTestJSON(input, requiredTests = [], {diagnose = false, packageName, managerRequired = false} = {}) {
  const channelRequirements = requiredTests.filter(name => name === 'service_shutdown_channel').length;
  const requireShutdownChannel = channelRequirements === 1;
  requiredTests = requiredTests.filter(name => name !== 'service_shutdown_channel');
  const channelValid = channelRequirements <= 1 && (!requireShutdownChannel ||
    (requiredTests.includes(serviceProofTest) && (packageName === undefined || packageName === expectedPackages[0])));
  const managerValid = typeof managerRequired === 'boolean' && (!managerRequired ||
    (requiredTests.includes(serviceProofTest) && (packageName === undefined || packageName === expectedPackages[0])));
  const packageValid = managerValid && channelValid && (packageName === undefined || (selectablePackages.includes(packageName) && requiredTests.length !== 0));
  const packageNames = packageName === undefined
    ? requiredTests.length === 0 ? expectedPackages : expectedPackages.slice(0, 1)
    : packageValid ? [packageName] : [];
  const packages = new Map(packageNames.map(name => [name, {
    started: false, summary: null, summaryLine: null, tests: new Map(), why: new Map(), classes: new Map(), diagnostics: new Map(),
    timedOut: new Set(), serviceOutcomes: new Set(), managerStages: [], managerMissing: new Set(), shutdownReceipts: [], elapsed: new Map(), pass: 0, skip: 0, fail: 0,
  }]));
  let invalid = !packageValid || requiredTests.some(name => typeof name !== 'string' || !testNamePattern.test(name) || /[a-f0-9]{64}/i.test(name)) ||
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
    // Receipt-like output from a package, sibling or child cannot become
    // unobserved mixed evidence beside an otherwise valid parent result.
    if (managerRequired && action === 'output' && name !== serviceProofTest &&
      typeof event.Output === 'string' && event.Output.split('\n').some(outputLine =>
        managerEvidence.test(serviceEvidenceMessage(outputLine)))) invalid = true;
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
            const message = serviceEvidenceMessage(line);
            if (serviceOutcomes.has(message)) {
              if (managerRequired && state.serviceOutcomes.has(message)) invalid = true;
              state.serviceOutcomes.add(message);
            }
            if (managerRequired) {
              const match = managerReceipt.exec(message);
              if (match) state.managerStages.push(match[1]);
              else if (message.includes('service_manager_stage')) invalid = true;
              if (managerMissing.has(message)) {
                if (state.managerMissing.has(message)) invalid = true;
                state.managerMissing.add(message);
              } else if (!serviceOutcomes.has(message) && !message.includes('service_manager_stage') &&
                managerEvidence.test(message)) invalid = true;
            }
          }
        }
        if (diagnoseProof && required.has(name)) {
          if (name === serviceProofTest && event.Package === expectedPackages[0]) {
            for (const outputLine of event.Output.split('\n')) {
              if (state.shutdownReceipts.length === 4) break;
              const message = outputLine.replace(/^[ \t]*(?:[A-Za-z0-9_.-]+\.go:[0-9]+: )?/, '').replace(/\r$/, '');
              if (serviceShutdownReceipt.test(message)) state.shutdownReceipts.push(maskedDiagnostic(message, 200));
            }
          }
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
      if (typeof event.Output === 'string') {
        const classes = state.classes.get(name) ?? new Set();
        for (const line of event.Output.split('\n')) {
          if (classes.size === 3) break;
          const message = line.replace(/^[ \t]*(?:[A-Za-z0-9_.-]+\.go:[0-9]+: )?/, '').replace(/\r$/, '');
          const prefix = classPrefix(message, event.Package);
          if (prefix !== undefined) classes.add(classDiagnostic(message, prefix, event.Package));
        }
        if (classes.size !== 0) state.classes.set(name, classes);
      }
      if (!state.why.has(name) && typeof event.Output === 'string') {
        for (const line of event.Output.split('\n')) {
          const message = line.replace(/^[ \t]*(?:[A-Za-z0-9_.-]+\.go:[0-9]+: )?/, '').replace(/\r$/, '');
          if (classPrefix(message, event.Package) !== undefined) continue;
          if (allowlist.get(event.Package)?.has(message)) {
            state.why.set(name, message);
            break;
          }
          const prefix = [...(prefixAllowlist.get(event.Package) ?? [])].find(([match]) => message.startsWith(match));
          if (prefix) {
            state.why.set(name, prefix[1]);
            break;
          }
        }
      }
      return;
    }
    else if (terminalActions.has(action) && (previous === 'running' || previous === 'paused')) {
      state.tests.set(name, action);
      state[action]++;
      if (requiredTests.length === 0 && !name.includes('/') && Number.isFinite(event.Elapsed) && event.Elapsed >= slowTestSeconds) {
        state.elapsed.set(name, event.Elapsed);
      }
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
    // An unavailable native premise is a complete, passing test observation,
    // but never a manager receipt. Permit exactly the marker plus one approved
    // reason, with no attempted manager child, stage or success outcome.
    const missingManagerReceipt = managerRequired && state.managerMissing.size === 2 &&
      state.managerMissing.has('NOT_RUN service_manager_receipt_missing') &&
      (state.managerMissing.has('NOT_RUN windows_task_query_result_untyped') ||
        state.managerMissing.has('NOT_RUN windows_task_interactive_token_unavailable')) &&
      state.serviceOutcomes.size === 0 && state.managerStages.length === 0 &&
      ![...state.tests.keys()].some(testName => testName.startsWith(`${serviceProofTest}/manager_`));
    if (required.has(serviceProofTest) && state.tests.get(serviceProofTest) === 'pass' && !missingManagerReceipt) {
      const outcomes = state.serviceOutcomes;
      const supported = outcomes.has('PASS service_manager_available') && outcomes.has('PASS service_manager_persistence');
      const foreground = outcomes.has('NOT_RUN service_manager_unavailable') && outcomes.has('PASS service_foreground_persistence');
      const unsupported = outcomes.has('NOT_RUN service_adapter_unsupported');
      if (outcomes.has('NOT_RUN native_postgres_unconfigured') || Number(supported) + Number(foreground) + Number(unsupported) !== 1 ||
        outcomes.size !== (unsupported ? 1 : 2) + Number(outcomes.has('PASS service_shutdown_channel')) ||
        (outcomes.has('PASS service_shutdown_channel') && !foreground && !(managerRequired && supported)) ||
        (requireShutdownChannel && ((!foreground && !(managerRequired && supported)) || !outcomes.has('PASS service_shutdown_channel')))) invalid = true;
    }
    if (managerRequired && state.tests.get(serviceProofTest) === 'pass' && !missingManagerReceipt) {
      const stages = state.managerStages;
      if (!state.serviceOutcomes.has('PASS service_manager_available') ||
        !state.serviceOutcomes.has('PASS service_manager_persistence') ||
        state.managerMissing.size !== 0 || stages.length !== serviceManagerStages.length ||
        serviceManagerStages.some((stage, index) => stages[index] !== stage ||
          state.tests.get(`${serviceProofTest}/manager_${stage}`) !== 'pass')) invalid = true;
    }
    if (invalid && firstInvalidLine === null && state.summaryLine !== null &&
      (state.tests.size === 0 || [...state.tests.values()].some(action => !terminalActions.has(action)) ||
        incompleteProof)) firstInvalidLine = maskedDiagnostic(state.summaryLine, 120);
    lines.push(`package ${name} pass=${state.pass} skip=${state.skip} fail=${state.fail}`);
    for (const missing of [...state.managerMissing].sort()) lines.push(`capability ${name} ${serviceProofTest} :: ${missing}`);
    for (const stage of state.managerStages) if (serviceManagerStages.includes(stage)) lines.push(`receipt ${name} ${serviceProofTest} :: PASS service_manager_stage=${stage}`);
    for (const outcome of [...state.serviceOutcomes].sort()) lines.push(`capability ${name} ${serviceProofTest} :: ${outcome}`);
    for (const [testName, action] of [...state.tests].sort(([left], [right]) => left.localeCompare(right))) {
      if (action === 'skip') lines.push(`skip ${name} ${testName}`);
      // Successful native stops retain only their bounded, closed-vocabulary
      // receipts. Failed proofs keep the existing full diagnostic path below.
      if (action === 'pass' && testName === serviceProofTest) {
        for (const receipt of state.shutdownReceipts) lines.push(`diagnose ${name} ${testName} :: ${receipt}`);
      }
      if (action === 'fail') {
        lines.push(`fail ${name} ${testName}`);
        let reason = state.timedOut.has(testName) ? 'test timed out' : state.why.get(testName);
        if (reason === undefined) {
          // Map insertion order preserves the first classified output event.
          // Derive only the display label; keep each test's own reason intact.
          const child = [...state.why].find(([childName]) =>
            childName.startsWith(`${testName}/`) && state.tests.get(childName) === 'fail');
          reason = child ? `child failed: ${child[1]}` : '(unclassified)';
        }
        lines.push(`why ${name} ${testName} :: ${reason}`);
        for (const diagnostic of state.classes.get(testName) ?? []) lines.push(`class ${name} ${testName} :: ${diagnostic}`);
        for (const diagnostic of state.diagnostics.get(testName) ?? []) lines.push(`diagnose ${name} ${testName} :: ${diagnostic}`);
      }
    }
    // Top-level tests only: a parent's elapsed time already includes its
    // children, and a parallel test's includes the time it spent paused.
    for (const [testName, seconds] of [...state.elapsed]
      .sort(([left, a], [right, b]) => b - a || left.localeCompare(right)).slice(0, slowTestLimit)) {
      lines.push(`slow ${name} ${testName} :: ${Math.round(seconds)}s`);
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

function invokedAsMain() {
  try {
    return Boolean(process.argv[1]) && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (invokedAsMain()) {
  const args = process.argv.slice(2);
  const options = {};
  let argumentsValid = true;
  for (let index = 0; index < args.length; index++) {
    const key = args[index];
    if (!['--require', '--input', '--diagnose', '--package', '--manager-required'].includes(key) || options[key] !== undefined) {
      argumentsValid = false;
      break;
    }
    options[key] = ['--diagnose', '--manager-required'].includes(key) ? true : args[++index];
    if (!options[key] || (typeof options[key] === 'string' && options[key].startsWith('--'))) argumentsValid = false;
  }
  if ((options['--diagnose'] || options['--package'] || options['--manager-required']) && !options['--require']) argumentsValid = false;
  if (options['--package'] && !selectablePackages.includes(options['--package'])) argumentsValid = false;
  if (!argumentsValid) {
    console.error('FAIL: invalid Go test checker arguments');
    process.exitCode = 1;
  } else {
    const input = options['--input'] ? createReadStream(options['--input'], {encoding: 'utf8'}) : process.stdin.setEncoding('utf8');
    const result = await checkGoTestJSON(input, options['--require']?.split(',') ?? [], {diagnose: options['--diagnose'], packageName: options['--package'], managerRequired: options['--manager-required'] ?? false});
    console.log(result.lines.join('\n'));
    if (!result.valid) console.error(result.reason);
    process.exitCode = result.valid ? 0 : 1;
  }
}
