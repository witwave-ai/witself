#!/usr/bin/env node
import assert from 'node:assert/strict';
import {execFileSync, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import test from 'node:test';
import {assertGate} from './ci-product-gate.mjs';
import {classifyChanges} from './ci-product-scope.mjs';

const root = fileURLToPath(new URL('../', import.meta.url));
const files = ['ci.yml', 'product-site-checks.yml', 'product-site.yml'];
// Ruby/Psych is already a repository gate dependency. Normalize YAML 1.1's
// interpretation of the top-level `on` key before inspecting workflow data.
const workflows = JSON.parse(execFileSync('ruby', ['-rjson', '-ryaml', '-e', `
  result = ARGV.to_h do |path|
    data = YAML.safe_load(File.read(path), aliases: false)
    data['on'] = data.delete(true) if data.key?(true)
    [File.basename(path), data]
  end
  puts JSON.generate(result)
`, ...files.map(file => `${root}.github/workflows/${file}`)], {encoding: 'utf8'}));

const wrapperDependencies = {
  go: ['go-rest', 'go-store', 'go-infra', 'localcell-windows', 'postgres-backup-image', 'postgres-image-mirror', 'provider-contract-evidence', 'static-analysis'],
  'release-config': ['release-config-full'],
  'homebrew-formula': ['homebrew-formula-full'],
  helm: ['helm-full'],
  'avatar-renderer-portability': ['avatar-renderer-portability-full'],
};
const fullJobs = [...new Set(Object.values(wrapperDependencies).flat()), 'provider-integration-contract'];
const checkNames = ['ubuntu-latest', 'ubuntu-24.04-arm'];
const testCommand = 'node --test scripts/ci-product-scope.test.mjs scripts/ci-product-gate.test.mjs scripts/ci-product-workflow.test.mjs scripts/check-docs.test.mjs scripts/run-docs-check.test.mjs';
const expression = value => String(value ?? '').replace(/^\s*\$\{\{\s*|\s*\}\}\s*$/g, '').trim();
const list = value => Array.isArray(value) ? value : [value];
const sorted = value => [...value].sort();

function needs(job, expected, label) {
  assert.deepEqual(sorted(list(job.needs)), sorted(expected), `${label}: exact dependency fence`);
}

function action(job, prefix, label) {
  const found = job.steps.filter(step => step.uses?.startsWith(prefix));
  assert.equal(found.length, 1, `${label}: exactly one ${prefix}`);
  assert.match(found[0].uses, /@[a-f0-9]{40}$/, `${label}: immutable action`);
  return found[0];
}

function assertClassifier(job, outputs, label) {
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.with['fetch-depth'], 0, `${label}: full comparison history`);
  assert.equal(checkout.with['persist-credentials'], false, `${label}: checkout has no persisted credential`);
  assert.equal(action(job, 'actions/setup-node@', label).with['node-version'], 22);
  for (const name of outputs) {
    assert.equal(job.outputs[name], `\${{ steps.scope.outputs.${name} }}`, `${label}: scope output ${name}`);
  }
  const scope = job.steps.filter(step => step.id === 'scope');
  assert.equal(scope.length, 1, `${label}: one classifier`);
  assert.equal(scope[0].run, 'node scripts/ci-product-scope.mjs');
  assert.equal(scope[0].if, undefined, `${label}: classifier cannot be skipped`);
  assert.equal(scope[0]['continue-on-error'], undefined, `${label}: classifier cannot ignore failure`);
  assert.equal(job['continue-on-error'], undefined);
  assert.equal(job.if, undefined, `${label}: classifier always runs for workflow events`);
  const check = job.steps.find(step => step.run === testCommand);
  assert(check, `${label}: classifier, gate, and wiring regressions run together`);
  assert.equal(check.if, undefined);
  assert.equal(check['continue-on-error'], undefined);
  assert(job.steps.indexOf(check) < job.steps.indexOf(scope[0]), `${label}: test before classification`);
}

function assertWebsiteCall(job, label) {
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.website == 'true'", `${label}: positive website classification`);
  assert.equal(job.uses, './.github/workflows/product-site-checks.yml');
  assert.equal(job.secrets, undefined, `${label}: no inherited deployment secrets`);
  assert.equal(job['continue-on-error'], undefined);
}

function assertDocsJob(job) {
  const label = 'documentation validation';
  assert(job, 'Missing documentation validation job');
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.docs == 'true'", 'Positive documentation classification only');
  assert.equal(job['runs-on'], 'ubuntu-24.04');
  assert.equal(job['continue-on-error'], undefined);
  assert.equal(job.services, undefined, 'Documentation validation needs no database or container service');
  assert.equal(job.env, undefined, 'Documentation validation needs no database or deployment configuration');
  assert.equal(job.environment, undefined, 'Documentation validation cannot enter a deployment environment');
  assert(!JSON.stringify(job).includes('secrets.'), 'Documentation validation receives no deployment secrets');
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.with['fetch-depth'], 0, 'Local and CI docs runner needs complete comparison history');
  assert.equal(checkout.with['persist-credentials'], false);
  assert.equal(action(job, 'actions/setup-node@', label).with['node-version'], 22);
  const go = action(job, 'actions/setup-go@', label);
  assert.equal(expression(go.if), "needs.changes.outputs.docs_index == 'true'", 'Only the runbook index guard needs Go');
  assert.equal(go.with['go-version-file'], 'go.mod');
  assert.equal(go.with['cache-dependency-path'], 'go.sum');
  const checks = job.steps.filter(step => step.run !== undefined);
  assert.deepEqual(checks.map(step => step.run), ['make check-docs'], 'Local and CI documentation checks share one bounded command');
  assert.equal(checks[0].if, undefined);
  assert.equal(checks[0]['continue-on-error'], undefined);
}

function assertLocalcellWindowsJob(ci) {
  const label = 'localcell-windows';
  assert.equal(ci.env, undefined, `${label}: no inherited workflow environment`);
  const job = ci.jobs[label];
  assert(job, 'Missing native Windows Local Cell unit job');
  needs(job, ['changes'], label);
  assert.equal(expression(job.if), "needs.changes.outputs.application == 'true'");
  assert.equal(job['runs-on'], 'windows-latest');
  assert.deepEqual(job.permissions, {contents: 'read'});
  assert.equal(job['timeout-minutes'], 25);
  for (const key of ['continue-on-error', 'environment', 'env', 'services', 'secrets', 'strategy', 'defaults']) {
    assert.equal(job[key], undefined, `${label}: no ${key}`);
  }
  assert(!JSON.stringify(job).includes('secrets.'), `${label}: no secret access`);
  assert(!JSON.stringify(job).includes('WITSELF_LOCALCELL_PGBIN'), `${label}: no PostgreSQL lifecycle activation`);
  const checkout = action(job, 'actions/checkout@', label);
  assert.equal(checkout.uses, action(ci.jobs['go-rest'], 'actions/checkout@', 'go-rest').uses);
  assert.deepEqual(checkout.with, {'persist-credentials': false});
  const go = action(job, 'actions/setup-go@', label);
  assert.equal(go.uses, action(ci.jobs['go-rest'], 'actions/setup-go@', 'go-rest').uses);
  assert.deepEqual(go.with, {'go-version-file': 'go.mod', cache: true, 'cache-dependency-path': 'go.sum'});
  const node = action(job, 'actions/setup-node@', label);
  assert.equal(node.uses, action(ci.jobs.changes, 'actions/setup-node@', 'changes').uses);
  assert.deepEqual(node.with, {'node-version': 22});
  assert.deepEqual(job.steps.slice(0, 3), [checkout, go, node]);
  const checks = job.steps.slice(3);
  const packages = './cmd/witself-server ./cmd/witself-worker ./internal/localcell/... ./internal/tools/localcellharness/... ./internal/runtimeconfig/...';
  assert.deepEqual(checks.map(step => step.run.trim()), [
    `go build ${packages}`,
    `go vet ${packages}`,
    'node --test scripts/ci-go-test-json-check.test.mjs',
    [
      '$goExit = 1',
      '& {',
      '  go test -json ./internal/localcell ./internal/runtimeconfig ./internal/tools/localcellharness -count=1 -timeout=10m',
      '  $script:goExit = $LASTEXITCODE',
      '} | node scripts/ci-go-test-json-check.mjs',
      '$parserExit = $LASTEXITCODE',
      'if ($goExit -ne 0 -or $parserExit -ne 0) { exit 1 }',
    ].join('\n'),
  ], `${label}: build, vet and parsed test execution stay separate and fail closed`);
  for (const step of checks) assert.equal(step.shell, 'pwsh');
  for (const step of job.steps) {
    for (const key of ['if', 'continue-on-error', 'env', 'working-directory']) {
      assert.equal(step[key], undefined, `${label}: step cannot override ${key}`);
    }
  }
}

// Evaluate only the small expression grammar used by deployment and concurrency.
// Never eval workflow source: mutation tests must not execute changed code.
function evaluateExpression(source, context) {
  const text = expression(source);
  const tokenPattern = /\s*(&&|\|\||==|!=|[()!]|'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_.-]*)/gy;
  const tokens = [];
  let offset = 0;
  while (offset < text.length) {
    tokenPattern.lastIndex = offset;
    const match = tokenPattern.exec(text);
    assert(match, `Unsupported condition syntax near ${text.slice(offset)}`);
    tokens.push(match[1]);
    offset = tokenPattern.lastIndex;
  }
  let cursor = 0;
  const take = expected => {
    assert.equal(tokens[cursor++], expected, `Expected condition token ${expected}`);
  };
  const value = () => {
    const token = tokens[cursor++];
    assert(token !== undefined, 'Incomplete deployment condition');
    if (token === '(') {
      const result = or();
      take(')');
      return result;
    }
    if (token === '!') return !value();
    if (token.startsWith("'")) return token.slice(1, -1).replaceAll("''", "'");
    if (token === 'true' || token === 'false') return token === 'true';
    assert.match(token, /^[A-Za-z_][A-Za-z0-9_.-]*$/);
    return context[token];
  };
  const equal = () => {
    let left = value();
    if (['==', '!='].includes(tokens[cursor])) {
      const operator = tokens[cursor++];
      const right = value();
      // All equality operands here are strings; GitHub compares them without
      // case. A separate executed shell guard enforces the exact launch value.
      const same = typeof left === 'string' && typeof right === 'string'
        ? left.toLowerCase() === right.toLowerCase() : left === right;
      left = operator === '==' ? same : !same;
    }
    return left;
  };
  const and = () => {
    let left = equal();
    while (tokens[cursor] === '&&') {
      cursor++;
      const right = equal();
      left = left && right;
    }
    return left;
  };
  const or = () => {
    let left = and();
    while (tokens[cursor] === '||') {
      cursor++;
      const right = and();
      left = left || right;
    }
    return left;
  };
  const result = or();
  assert.equal(cursor, tokens.length, 'Unconsumed deployment condition');
  return result;
}

function assertCIConcurrency(ci) {
  assert.equal(ci.concurrency['cancel-in-progress'], true, 'New PR updates must cancel superseded checks');
  const group = context => {
    const template = ci.concurrency.group.match(/^([^$]*)\$\{\{([\s\S]+)\}\}$/);
    assert(template, 'Concurrency group must contain a supported expression');
    return template[1] + evaluateExpression(template[2], context);
  };
  const main = {'github.event_name': 'push', 'github.ref': 'refs/heads/main'};
  const pr = {'github.event_name': 'pull_request', 'github.ref': 'refs/pull/42/merge'};
  const firstMain = group({...main, 'github.run_id': '1001'});
  const nextMain = group({...main, 'github.run_id': '1002'});
  assert.notEqual(firstMain, nextMain, 'A docs or brochure main push must not cancel or queue behind earlier application CI');
  const firstPR = group({...pr, 'github.run_id': '1003'});
  assert.equal(firstPR, group({...pr, 'github.run_id': '1004'}), 'Updates to one PR must retain their cancellation group');
  assert.notEqual(firstPR, group({...pr, 'github.ref': 'refs/pull/43/merge', 'github.run_id': '1005'}), 'Different PRs must run independently');
  assert.notEqual(firstMain, firstPR, 'Main pushes and PRs must run independently');
}

function assertWorkflowContracts(sources) {
  const ci = sources['ci.yml'];
  const reusable = sources['product-site-checks.yml'];
  const publication = sources['product-site.yml'];
  assert.deepEqual(Object.keys(ci.on).sort(), ['pull_request', 'push']);
  assert.deepEqual(ci.on.push, {branches: ['main']}, 'CI main push cannot have a path filter');
  assert.equal(ci.on.pull_request, null, 'CI PR cannot have a path filter');
  assert.deepEqual(ci.permissions, {contents: 'read'});
  assertCIConcurrency(ci);
  assertClassifier(ci.jobs.changes, ['application', 'docs', 'docs_index', 'docs_only', 'website', 'website_only'], 'ci changes');
  assertWebsiteCall(ci.jobs.website, 'ci website');
  assertDocsJob(ci.jobs.docs);
  assertLocalcellWindowsJob(ci);

  for (const [id, dependencies] of Object.entries(wrapperDependencies)) {
    const job = ci.jobs[id];
    assert(job, `Missing required wrapper: ${id}`);
    assert.equal(expression(job.if), 'always()', `${id}: required check must report despite failed or skipped dependencies`);
    needs(job, ['changes', 'website', 'docs', ...dependencies], id);
    assert.equal(job['continue-on-error'], undefined, `${id}: required gate cannot ignore failure`);
    const guard = job.steps.filter(step => step.run === 'node scripts/ci-product-gate.mjs');
    assert.equal(guard.length, 1, `${id}: strict helper runs exactly once`);
    assert.deepEqual(guard[0].env, {NEEDS_JSON: '${{ toJSON(needs) }}'}, `${id}: forward complete dependency results`);
    assert.equal(guard[0].if, undefined, `${id}: strict helper cannot be skipped`);
    assert.equal(guard[0]['continue-on-error'], undefined);
    assert.equal(action(job, 'actions/checkout@', id).with['persist-credentials'], false);
    assert.equal(action(job, 'actions/setup-node@', id).with['node-version'], 22);
    if (id === 'avatar-renderer-portability') {
      assert.equal(job.name, 'avatar-renderer-portability (${{ matrix.check }})');
      assert.equal(job['runs-on'], '${{ matrix.runner }}');
      assert.deepEqual(job.strategy.matrix.include, checkNames.map(check => ({check, runner: 'ubuntu-24.04'})));
    } else {
      assert.equal(job.name ?? id, id, `${id}: frozen required check name`);
      assert.equal(job['runs-on'], 'ubuntu-24.04');
    }
  }
  for (const id of fullJobs) {
    const job = ci.jobs[id];
    assert(job, `Missing application gate: ${id}`);
    assert.equal(job['continue-on-error'], undefined, `${id}: application failure remains blocking`);
    if (id === 'provider-contract-evidence') {
      needs(job, ['changes', 'provider-integration-contract'], id);
      assert.equal(expression(job.if), "always() && needs.changes.outputs.application == 'true'");
      const native = job.steps.find(step => step.name === 'Require every native provider job');
      assert.equal(native?.run, 'test "$NATIVE_RESULT" = success');
      assert.deepEqual(native.env, {NATIVE_RESULT: '${{ needs.provider-integration-contract.result }}'});
    } else {
      needs(job, ['changes'], id);
      assert.equal(expression(job.if), "needs.changes.outputs.application == 'true'", `${id}: positive application classification only`);
    }
  }
  assert.deepEqual(ci.jobs['provider-integration-contract'].strategy.matrix.include, [
    {target: 'linux-x64', runner: 'ubuntu-24.04'},
    {target: 'linux-arm64', runner: 'ubuntu-24.04-arm'},
    {target: 'macos-intel', runner: 'macos-15-intel'},
    {target: 'macos-arm64', runner: 'macos-15'},
    {target: 'windows-x64', runner: 'windows-latest'},
  ], 'Application provider matrix retains all native targets');
  assert.deepEqual(ci.jobs['avatar-renderer-portability-full'].strategy.matrix.include, [
    {check: 'ubuntu-latest', runner: 'ubuntu-24.04'},
    {check: 'ubuntu-24.04-arm', runner: 'ubuntu-24.04-arm'},
  ], 'Full renderer gates retain both architectures');

  assert.deepEqual(Object.keys(reusable.on), ['workflow_call']);
  assert.deepEqual(reusable.permissions, {contents: 'read'});
  assert.deepEqual(Object.keys(reusable.jobs), ['validate']);
  assert(!JSON.stringify(reusable).includes('secrets.'), 'Reusable validation never receives deployment secrets');
  assert.equal(reusable.jobs.validate.environment, undefined, 'Validation does not enter a deployment environment');
  const checks = reusable.jobs.validate.steps.filter(step => step.run === 'make check-product-site');
  assert.equal(checks.length, 1, 'Local and CI website checks share one command');
  assert.equal(checks[0].if, undefined);
  assert.equal(checks[0]['continue-on-error'], undefined);
  assert.equal(reusable.jobs.validate['continue-on-error'], undefined);

  assert.deepEqual(Object.keys(publication.on).sort(), ['push', 'workflow_dispatch']);
  assert.deepEqual(publication.on.push, {branches: ['main']}, 'Publication uses complete comparisons, not truncated workflow path filtering');
  assert.equal(publication.on.workflow_dispatch.inputs.publish.type, 'boolean');
  assert.equal(publication.on.workflow_dispatch.inputs.publish.default, false);
  assert.equal(publication.concurrency['cancel-in-progress'], false, 'Do not interrupt an in-flight deployment');
  assert.deepEqual(publication.permissions, {contents: 'read'});
  assertClassifier(publication.jobs.changes, ['website', 'website_only'], 'publication changes');
  assertWebsiteCall(publication.jobs.validate, 'publication validation');
  const deploy = publication.jobs.deploy;
  needs(deploy, ['changes', 'validate'], 'deployment');
  assert.equal(deploy.environment.name, 'product-site');
  assert.equal(deploy['continue-on-error'], undefined);
  for (const event of ['push', 'workflow_dispatch', 'pull_request', 'unknown']) {
    for (const ref of ['refs/heads/main', 'refs/heads/other']) {
      for (const enabled of ['true', 'false', '']) {
        for (const validation of ['success', 'failure', 'skipped', 'cancelled']) {
          for (const websiteOnly of ['true', 'false', '']) {
            for (const publish of [true, false]) {
              const context = {
                'github.event_name': event, 'github.ref': ref,
                'vars.WITSELF_PRODUCT_PUBLISH_ENABLED': enabled,
                'needs.validate.result': validation,
                'needs.changes.outputs.website_only': websiteOnly,
                'inputs.publish': publish,
              };
              const expected = ref === 'refs/heads/main' && enabled === 'true' && validation === 'success' &&
                ((event === 'push' && websiteOnly === 'true') || (event === 'workflow_dispatch' && publish));
              assert.equal(evaluateExpression(deploy.if, context), expected, `Deployment fence: ${JSON.stringify(context)}`);
            }
          }
        }
      }
    }
  }
  const launch = deploy.steps.find(step => step.name === 'Require the exact launch ref and hold value');
  assert(launch, 'Deployment must enforce exact launch settings in shell');
  assert.deepEqual(launch.env, {LAUNCH_ENABLED: '${{ vars.WITSELF_PRODUCT_PUBLISH_ENABLED }}'});
  assert.equal(launch.run.trim(), 'test "$GITHUB_REF" = refs/heads/main\ntest "$LAUNCH_ENABLED" = true');
  assert.equal(launch.if, undefined);
  const publishIndex = deploy.steps.findIndex(step => step.name === 'Deploy static assets to Cloudflare');
  const currentMainIndex = deploy.steps.findIndex(step => step.name === 'Require the current main commit before publication');
  assert.equal(currentMainIndex, publishIndex - 1, 'Reject superseded queued main commits immediately before deployment');
  const currentMain = deploy.steps[currentMainIndex];
  assert(currentMain.run.includes('gh api "repos/$GITHUB_REPOSITORY/git/ref/heads/main" --jq .object.sha'));
  assert(currentMain.run.includes('test "$current_main" = "$GITHUB_SHA"'));
  assert.equal(currentMain.if, undefined);
  assert.equal(currentMain['continue-on-error'], undefined);
}

test('documentation and website lanes preserve required checks, application gates, and deployment boundaries', () => {
  assertWorkflowContracts(workflows);
});

test('main push runs remain independent while newer updates cancel only their own PR checks', () => {
  assertCIConcurrency(workflows['ci.yml']);
});

test('native Windows failures block go while brochure-only and docs-only changes skip Windows', () => {
  const job = workflows['ci.yml'].jobs['localcell-windows'];
  for (const [path, application] of [
    ['web/product/content.mjs', false],
    ['docs/research/local-cell/implementation-plan.md', false],
    ['.github/workflows/ci.yml', true],
    ['scripts/ci-go-test-json-check.mjs', true],
  ]) {
    const scope = classifyChanges([{
      path, status: 'M', oldMode: '100644', newMode: '100644',
      oldOID: 'a'.repeat(40), newOID: 'b'.repeat(40),
    }]);
    assert.equal(scope.application, application, path);
    assert.equal(evaluateExpression(job.if, {'needs.changes.outputs.application': String(scope.application)}), application, path);
    const outputs = Object.fromEntries(Object.entries(scope).filter(([key]) => key !== 'reason').map(([key, value]) => [key, String(value)]));
    const results = {
      changes: {result: 'success', outputs},
      docs: {result: scope.docs ? 'success' : 'skipped'},
      website: {result: scope.website ? 'success' : 'skipped'},
      ...Object.fromEntries(wrapperDependencies.go.map(id => [id, {result: application ? 'success' : 'skipped'}])),
    };
    assert.doesNotThrow(() => assertGate(results), path);
    for (const result of application ? ['failure', 'cancelled', 'skipped', undefined] : ['success', 'failure']) {
      assert.throws(() => assertGate({...results, 'localcell-windows': {result}}), `${path}: Windows ${result}`);
    }
  }
});

test('classified docs, mixed, and checker changes preserve every required wrapper and publication fence', () => {
  const research = 'docs/research/local-cell/notes.md';
  const cases = [
    {paths: [research], application: false, docs: true, docs_index: false, docs_only: true, website: false, website_only: false},
    {paths: ['docs/README.md'], application: false, docs: true, docs_index: true, docs_only: true, website: false, website_only: false},
    {paths: [research, 'web/product/content.mjs'], application: true, docs: true, docs_index: false, docs_only: false, website: true, website_only: false},
    {paths: [research, 'internal/store/transcripts.go'], application: true, docs: true, docs_index: false, docs_only: false, website: false, website_only: false},
    {paths: ['web/product/content.mjs'], application: false, docs: false, docs_index: false, docs_only: false, website: true, website_only: true},
    ...['scripts/check-docs.mjs', 'scripts/run-docs-check.mjs'].map(path => ({
      paths: [path], application: true, docs: false, docs_index: false, docs_only: false, website: false, website_only: false,
    })),
    {paths: ['docs/README.md', '.github/workflows/ci.yml'], application: true, docs: true, docs_index: true, docs_only: false, website: true, website_only: false},
  ];
  for (const {paths, ...expected} of cases) {
    const {reason, ...scope} = classifyChanges(paths.map(path => ({
      path, status: 'M', oldMode: '100644', newMode: '100644',
      oldOID: 'a'.repeat(40), newOID: 'b'.repeat(40),
    })));
    assert.deepEqual(scope, expected, paths.join(', '));
    const outputs = Object.fromEntries(Object.entries(scope).map(([key, flag]) => [key, String(flag)]));
    const context = Object.fromEntries(Object.entries(outputs).map(([key, flag]) => [`needs.changes.outputs.${key}`, flag]));
    assert.equal(evaluateExpression(workflows['ci.yml'].jobs.docs.if, context), scope.docs);
    assert.equal(evaluateExpression(workflows['ci.yml'].jobs.website.if, context), scope.website);
    for (const id of fullJobs) {
      assert.equal(evaluateExpression(expression(workflows['ci.yml'].jobs[id].if).replace(/^always\(\) && /, ''), context), scope.application, `${paths}: ${id}`);
    }
    for (const [id, dependencies] of Object.entries(wrapperDependencies)) {
      const results = {
        changes: {result: 'success', outputs},
        docs: {result: scope.docs ? 'success' : 'skipped'},
        website: {result: scope.website ? 'success' : 'skipped'},
        ...Object.fromEntries(dependencies.map(name => [name, {result: scope.application ? 'success' : 'skipped'}])),
      };
      assert.equal(assertGate(results), scope.docs_only ? 'docs-only' : scope.website_only ? 'brochure-only' : 'application', `${paths}: ${id}`);
      for (const status of ['failure', 'cancelled', undefined]) {
        assert.throws(() => assertGate({...results, docs: {result: status}}), `${paths}: ${id} cannot hide documentation ${status}`);
      }
      assert.throws(() => {
        const missing = {...results};
        delete missing.docs;
        assertGate(missing);
      }, `${paths}: ${id} cannot omit documentation result`);
    }
    assert.equal(evaluateExpression(workflows['product-site.yml'].jobs.deploy.if, {
      ...context,
      'github.event_name': 'push', 'github.ref': 'refs/heads/main',
      'vars.WITSELF_PRODUCT_PUBLISH_ENABLED': 'true',
      'needs.validate.result': 'success', 'inputs.publish': false,
    }), scope.website_only, `${paths}: automatic publication remains brochure-only`);
  }
});

test('exact launch shell guard rejects disabled, case-changed, and non-main settings', () => {
  const guard = workflows['product-site.yml'].jobs.deploy.steps.find(step => step.name === 'Require the exact launch ref and hold value');
  for (const [ref, enabled, expected] of [
    ['refs/heads/main', 'true', 0],
    ['refs/heads/main', 'TRUE', 1],
    ['refs/heads/main', 'false', 1],
    ['refs/heads/other', 'true', 1],
  ]) {
    const result = spawnSync('bash', ['-e', '-c', guard.run], {env: {...process.env, GITHUB_REF: ref, LAUNCH_ENABLED: enabled}});
    assert.equal(result.status, expected, `${ref}: ${enabled}`);
  }
});

const mutations = [
  ['Windows inherits PostgreSQL lifecycle configuration', data => { data['ci.yml'].env = {WITSELF_LOCALCELL_PGBIN: 'synthetic'}; }],
  ['missing Windows unit job', data => { delete data['ci.yml'].jobs['localcell-windows']; }],
  ['missing Windows dependency from go', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'localcell-windows'); }],
  ['Windows job failure ignored', data => { data['ci.yml'].jobs['localcell-windows']['continue-on-error'] = true; }],
  ['Windows job always runs', data => { data['ci.yml'].jobs['localcell-windows'].if = 'always()'; }],
  ['Windows parser step removed', data => { data['ci.yml'].jobs['localcell-windows'].steps.pop(); }],
  ['Windows parser bypassed', data => { data['ci.yml'].jobs['localcell-windows'].steps.at(-1).run = 'go test ./internal/localcell'; }],
  ['Windows test failure ignored', data => { data['ci.yml'].jobs['localcell-windows'].steps.at(-1)['continue-on-error'] = true; }],
  ['Windows native Go exit masked', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.at(-1); step.run = step.run.replace('$goExit -ne 0 -or ', ''); }],
  ['Windows parser exit masked', data => { const step = data['ci.yml'].jobs['localcell-windows'].steps.at(-1); step.run = step.run.replace(' -or $parserExit -ne 0', ''); }],
  ['Windows parser unit tests removed', data => { const job = data['ci.yml'].jobs['localcell-windows']; job.steps = job.steps.filter(step => step.run !== 'node --test scripts/ci-go-test-json-check.test.mjs'); }],
  ...[
    ['runs-on', 'ubuntu-24.04'], ['needs', []], ['permissions', {contents: 'write'}],
    ['timeout-minutes', 26], ['environment', 'production'], ['env', {WITSELF_LOCALCELL_PGBIN: 'synthetic'}],
  ].map(([key, value]) => [`Windows ${key} boundary changed`, data => { data['ci.yml'].jobs['localcell-windows'][key] = value; }]),
  ['main pushes share a cancellation or queue group', data => { data['ci.yml'].concurrency.group = 'ci-${{ github.ref }}'; }],
  ['concurrency cancellation disabled', data => { data['ci.yml'].concurrency['cancel-in-progress'] = false; }],
  ['missing Go application dependency', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'go-store'); }],
  ['missing required website dependency', data => { data['ci.yml'].jobs.helm.needs = data['ci.yml'].jobs.helm.needs.filter(id => id !== 'website'); }],
  ...Object.keys(wrapperDependencies).map(id => [
    `missing required documentation dependency from ${id}`,
    data => { data['ci.yml'].jobs[id].needs = data['ci.yml'].jobs[id].needs.filter(dependency => dependency !== 'docs'); },
  ]),
  ['missing static analysis dependency', data => { data['ci.yml'].jobs.go.needs = data['ci.yml'].jobs.go.needs.filter(id => id !== 'static-analysis'); }],
  ['wrong docs output forwarding', data => { data['ci.yml'].jobs.changes.outputs.docs = '${{ steps.scope.outputs.website }}'; }],
  ['missing docs-only output', data => { delete data['ci.yml'].jobs.changes.outputs.docs_only; }],
  ['wrong docs index output forwarding', data => { data['ci.yml'].jobs.changes.outputs.docs_index = '${{ steps.scope.outputs.docs }}'; }],
  ['mixed documentation validation skipped', data => { data['ci.yml'].jobs.docs.if = "needs.changes.outputs.docs_only == 'true'"; }],
  ['different local and CI documentation gate', data => { data['ci.yml'].jobs.docs.steps.at(-1).run = 'echo passed'; }],
  ['documentation failure ignored', data => { data['ci.yml'].jobs.docs.steps.at(-1)['continue-on-error'] = true; }],
  ['documentation check skipped', data => { data['ci.yml'].jobs.docs.steps.at(-1).if = 'false'; }],
  ['documentation job failure ignored', data => { data['ci.yml'].jobs.docs['continue-on-error'] = true; }],
  ['research-only docs installs Go', data => { delete data['ci.yml'].jobs.docs.steps.find(step => step.uses?.startsWith('actions/setup-go@')).if; }],
  ['documentation index guard loses Go setup', data => { data['ci.yml'].jobs.docs.steps = data['ci.yml'].jobs.docs.steps.filter(step => !step.uses?.startsWith('actions/setup-go@')); }],
  ['documentation starts a database', data => { data['ci.yml'].jobs.docs.services = {postgres: {image: 'postgres'}}; }],
  ['skipped required wrapper', data => { data['ci.yml'].jobs.go.if = "needs.changes.outputs.application == 'true'"; }],
  ['lost dependency result forwarding', data => { data['ci.yml'].jobs.go.steps.at(-1).env.NEEDS_JSON = '{}'; }],
  ['ignored required gate failure', data => { data['ci.yml'].jobs.helm.steps.at(-1)['continue-on-error'] = true; }],
  ['broad application skip', data => { data['ci.yml'].jobs['go-store'].if = "github.event_name == 'push'"; }],
  ['provider failure hidden by success condition', data => { data['ci.yml'].jobs['provider-contract-evidence'].if = "success() && needs.changes.outputs.application == 'true'"; }],
  ['provider native target removed', data => { data['ci.yml'].jobs['provider-integration-contract'].strategy.matrix.include.pop(); }],
  ['renderer ARM runner lost', data => { data['ci.yml'].jobs['avatar-renderer-portability-full'].strategy.matrix.include[1].runner = 'ubuntu-24.04'; }],
  ['CI workflow path filtering', data => { data['ci.yml'].on.push.paths = ['web/product/**']; }],
  ['shallow classifier history', data => { data['ci.yml'].jobs.changes.steps[0].with['fetch-depth'] = 1; }],
  ['classifier errors ignored', data => { data['ci.yml'].jobs.changes.steps.at(-1)['continue-on-error'] = true; }],
  ['documentation checker regressions omitted', data => { data['ci.yml'].jobs.changes.steps.find(step => step.run === testCommand).run = testCommand.replace(' scripts/check-docs.test.mjs', ''); }],
  ['reusable validation credential', data => { data['product-site-checks.yml'].jobs.validate.env = {TOKEN: '${{ secrets.CLOUDFLARE_API_TOKEN }}'}; }],
  ['different local and CI website gate', data => { data['product-site-checks.yml'].jobs.validate.steps.at(-1).run = 'echo passed'; }],
  ['publication path filtering', data => { data['product-site.yml'].on.push.paths = ['web/product/**']; }],
  ['publication ignores failed validation', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace("needs.validate.result == 'success' &&", ''); }],
  ['publication allows mixed main pushes', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace(" && needs.changes.outputs.website_only == 'true'", ''); }],
  ['publication allows manual validation-only dispatch', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace(' && inputs.publish', ''); }],
  ['publication loses main restriction', data => { data['product-site.yml'].jobs.deploy.if = data['product-site.yml'].jobs.deploy.if.replace("github.ref == 'refs/heads/main' &&", ''); }],
  ['publication loses validation dependency', data => { data['product-site.yml'].jobs.deploy.needs = ['changes']; }],
  ['publication accepts stale queued commit', data => { data['product-site.yml'].jobs.deploy.steps = data['product-site.yml'].jobs.deploy.steps.filter(step => step.name !== 'Require the current main commit before publication'); }],
];

for (const [name, mutate] of mutations) {
  test(`rejects mutation: ${name}`, () => {
    const changed = structuredClone(workflows);
    mutate(changed);
    assert.notDeepEqual(changed, workflows, 'Mutation must change its intended target');
    assert.throws(() => assertWorkflowContracts(changed));
  });
}
