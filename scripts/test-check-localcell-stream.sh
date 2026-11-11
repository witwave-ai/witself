#!/usr/bin/env bash
set -euo pipefail

if ! command -v node >/dev/null 2>&1; then
  printf 'NOT RUN test-check-localcell-stream: node missing\n' >&2
  if [[ ${WITSELF_TEST_REQUIRE_NODE:-} == 1 ]]; then exit 1; fi
  exit 0
fi

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-localcell-stream-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

node - "$source_root" "$work_dir" "$BASH" "$(command -v dirname)" <<'JS'
const assert = require('node:assert/strict');
const {spawnSync} = require('node:child_process');
const {copyFileSync, mkdirSync, readFileSync, symlinkSync, writeFileSync} = require('node:fs');
const {join, resolve} = require('node:path');
const [sourceRoot, workDir, bash, dirname] = process.argv.slice(2);
const source = path => readFileSync(join(sourceRoot, path), 'utf8').replace(/\r\n/g, '\n');
const script = 'scripts/check-localcell-stream.sh';
const checker = 'scripts/ci-go-test-json-check.mjs';

// Read both actual CI step bodies, not a second hand-maintained command literal.
const workflow = source('.github/workflows/ci.yml');
const blocks = workflow.split(/^      - name: /m).slice(1)
  .filter(block => block.startsWith('Test Local Cell packages and require complete results\n'));
assert.equal(blocks.length, 2, 'both Local Cell result-checker CI steps must exist');
const commands = blocks.map(block => {
  const matches = [...block.matchAll(/^\s+(go test -json [^\n|]+)(?:\|[^\n]*)?$/gm)];
  assert.equal(matches.length, 1, 'each CI step must contain one Go JSON command');
  return matches[0][1].trim().split(/\s+/);
});
assert.deepEqual(blocks.map(block => /^        shell: (\w+)$/m.exec(block)?.[1]).sort(), ['bash', 'pwsh']);
assert.deepEqual(commands[0], commands[1], 'CI shell copies disagree');
const scriptCommands = [...source(script).matchAll(/^go test -json ([^\n|]+)(?:\|[^\n]*)?$/gm)]
  .map(match => `go test -json ${match[1]}`.trim().split(/\s+/));
assert.deepEqual(scriptCommands, [commands[0]], 'local command drifted from CI packages or flags');
console.log('check-localcell-stream regression: CI command drift pin passed');

const makefile = source('Makefile');
function recipe(name) {
  const lines = makefile.split('\n');
  const starts = lines.flatMap((line, index) => line.startsWith(`${name}:`) ? [index] : []);
  assert.equal(starts.length, 1, `${name} target must exist exactly once`);
  const result = [];
  for (const line of lines.slice(starts[0] + 1)) {
    if (line && !/^[\t #]/.test(line)) break;
    if (line.startsWith('\t')) result.push(line.slice(1));
  }
  return result;
}
assert.match(makefile, /^check-localcell-stream:.*## .+/m, 'target must have help text');
assert(makefile.split('\n').some(line => line.startsWith('.PHONY:') &&
  line.split(/\s+/).includes('check-localcell-stream')), 'target must be phony');
assert.deepEqual(recipe('check-localcell-stream'), [
  'bash scripts/check-localcell-stream.sh', 'bash scripts/test-check-localcell-stream.sh',
]);
const checkRecipe = recipe('check');
const calls = checkRecipe.flatMap((line, index) => line === '$(MAKE) check-localcell-stream' ? [index] : []);
assert.equal(calls.length, 1, 'check must invoke the stream target once');
assert.match(checkRecipe[calls[0] + 1], /^go test \.\/\.\.\. -race -shuffle=on(?: |$)/,
  'stream check must immediately precede the full race suite');
console.log('check-localcell-stream regression: Makefile wiring passed');

const repo = join(workDir, 'repo');
const withNode = join(workDir, 'with-node');
const noNode = join(workDir, 'no-node');
const noGo = join(workDir, 'no-go');
const neither = join(workDir, 'neither');
for (const path of [join(repo, 'scripts'), withNode, noNode, noGo, neither]) mkdirSync(path, {recursive: true});
for (const path of [script, checker]) copyFileSync(join(sourceRoot, path), join(repo, path));
for (const path of [withNode, noNode, noGo, neither]) symlinkSync(resolve(dirname), join(path, 'dirname'));
for (const path of [withNode, noGo]) symlinkSync(process.execPath, join(path, 'node'));
const fakeGo = `#!/bin/bash
if [[ \${WITSELF_LOCALCELL_PGBIN+x} ]]; then
  printf 'fake go saw native PostgreSQL configuration\\n' >&2
  exit 86
fi
printf '%s\\n' "$@" >"$STREAM_TEST_ARGS"
while IFS= read -r line; do printf '%s\\n' "$line"; done <"$STREAM_TEST_INPUT"
exit "$STREAM_TEST_GO_EXIT"
`;
for (const path of [withNode, noNode]) writeFileSync(join(path, 'go'), fakeGo, {mode: 0o755});

// Match the checker's fixture vocabulary: start/run/pass/package-pass events.
// Package identities come from CI, so changing all copies still exercises reality.
const packages = commands[0].filter(arg => arg.startsWith('./'))
  .map(path => `github.com/witwave-ai/witself/${path.slice(2)}`);
function event(Action, Package, Test) {
  return {Action, Package, ...(Test === undefined ? {} : {Test})};
}
function packageEvents(Package, quoted = false) {
  const events = [event('start', Package), event('run', Package, 'TestFixture')];
  if (quoted) {
    const Test = 'TestFixture/has"quote';
    events.push(event('run', Package, Test), event('pass', Package, Test));
  }
  return [...events, event('pass', Package, 'TestFixture'), event('pass', Package)];
}
const passing = packages.flatMap(name => packageEvents(name));
const quoted = packages.flatMap((name, index) => packageEvents(name, index === 1));
const missing = packages.slice(0, -1).flatMap(name => packageEvents(name));
const encode = events => `${events.map(value => JSON.stringify(value)).join('\n')}\n`;
const input = join(workDir, 'stream.json');
const argsFile = join(workDir, 'go.args');
function run(events, {goExit = 0, path = withNode, requireNode, nativeSentinel = false} = {}) {
  writeFileSync(input, encode(events));
  const childEnv = {
    PATH: path, STREAM_TEST_INPUT: input, STREAM_TEST_ARGS: argsFile,
    STREAM_TEST_GO_EXIT: String(goExit),
  };
  if (requireNode !== undefined) childEnv.WITSELF_TEST_REQUIRE_NODE = requireNode;
  // Only the fake Go is reachable; this sentinel can never start native Postgres.
  if (nativeSentinel) childEnv.WITSELF_LOCALCELL_PGBIN = 'not-a-real-postgres-directory';
  const result = spawnSync(bash, [join(repo, script)], {cwd: repo, env: childEnv, encoding: 'utf8'});
  assert.ifError(result.error);
  assert.equal(result.signal, null);
  return result;
}
function checkerResult(events) {
  // Invoke the checker by the same relative path as the script: its main-module
  // guard compares real paths, and macOS temporary directories sit behind a symlink.
  const result = spawnSync(process.execPath, [checker], {
    cwd: repo, input: encode(events), encoding: 'utf8', env: {},
  });
  assert.ifError(result.error);
  return result;
}
function assertPass(result, events) {
  const expected = checkerResult(events);
  assert.equal(expected.status, 0);
  assert.equal(expected.stdout.split('\n').filter(line => line.startsWith('package ')).length, packages.length,
    'the expected checker output must report every CI package');
  assert.equal(result.status, 0);
  assert.equal(result.stdout, `${expected.stdout}check-localcell-stream: PASS\n`, 'checker stdout must pass through unchanged');
  assert.equal(result.stderr, expected.stderr, 'checker stderr must pass through unchanged');
  assert.deepEqual(readFileSync(argsFile, 'utf8').trimEnd().split('\n'), commands[0].slice(1), 'executed Go arguments drifted from CI');
}
function assertFailure(result, events, goExit) {
  const expected = checkerResult(events);
  assert.notEqual(result.status, 0);
  assert(result.stdout.startsWith(expected.stdout), 'checker stdout must pass through unchanged');
  assert(result.stderr.startsWith(expected.stderr), 'checker stderr must pass through unchanged');
  assert(result.stdout.endsWith(`check-localcell-stream: FAIL go=${goExit} checker=${expected.status}\n`), 'FAIL must expose both pipeline statuses on its final line');
  const plainCommand = commands[0].filter(arg => arg !== '-json').join(' ');
  assert((result.stdout + result.stderr).includes(plainCommand), 'failure must name the plain Go rerun command');
}

assertPass(run(passing), passing);
console.log('check-localcell-stream regression: all-pass stream passed');
const quoteResult = run(quoted);
assertFailure(quoteResult, quoted, 0);
assert.match(quoteResult.stdout + quoteResult.stderr, /FAIL: invalid or incomplete Go test JSON stream/);
assert.notEqual(checkerResult(quoted).status, 0);
console.log('check-localcell-stream regression: quoted passing subtest rejected');
assertFailure(run(passing, {goExit: 1}), passing, 1);
console.log('check-localcell-stream regression: Go failure with complete stream rejected');
assertFailure(run(missing), missing, 0);
assert.notEqual(checkerResult(missing).status, 0);
console.log('check-localcell-stream regression: missing package rejected');
for (const [requireNode, status] of [[undefined, 0], ['1', 1]]) {
  const result = run(passing, {path: noNode, requireNode});
  assert.equal(result.status, status);
  assert.equal(result.stdout, '');
  assert.equal(result.stderr, 'NOT RUN check-localcell-stream: node missing\n');
}
console.log('check-localcell-stream regression: optional and required missing Node passed');
for (const path of [noGo, neither]) {
  assert.notEqual(run(passing, {path}).status, 0, 'missing Go must always be fatal, even without Node');
}
console.log('check-localcell-stream regression: missing Go rejected');
assertPass(run(passing, {nativeSentinel: true}), passing);
console.log('check-localcell-stream regression: native PostgreSQL configuration removed');
console.log('test-check-localcell-stream: PASS');
JS
