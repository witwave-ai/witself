#!/usr/bin/env node
// Local-only, read-only approval of one canonical application pin update.
// A failure never authorizes a roll and never changes GitHub's full CI policy.
import {execFileSync} from 'node:child_process';
import {mkdtempSync, mkdirSync, writeFileSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {pathToFileURL} from 'node:url';
import {classifyLocal} from './local-roll-scope.mjs';

const repository = 'witwave-ai/witself';
const workflow = '.github/workflows/release.yml';
const serverImage = 'ghcr.io/witwave-ai/images/witself-server';
const chartRepository = 'ghcr.io/witwave-ai/charts';
const schemaPaths = ['internal/store/migrations', 'internal/store/migrate.go', 'internal/store/export.go'];
export const requiredJobs = Object.freeze([
  'go', 'go-rest', 'go-store', 'go-infra', 'verify', 'provider-contract-evidence',
  'homebrew-verify', 'postgres-mirror-cells', 'goreleaser', 'promote-latest',
  ...['linux-x64', 'linux-arm64', 'macos-intel', 'macos-arm64', 'windows-x64'].map(target => `provider integration release gate / ${target}`),
  ...['ubuntu-latest', 'macos-15', 'windows-latest'].map(target => `dashboard release acceptance / ${target}`),
]);
export const requiredPublishSteps = Object.freeze([
  'Run GoReleaser', 'Verify signed release artifact contract', 'Attest container SBOMs',
  'Attest build provenance', 'Verify published assets and archive provenance',
  'Publish Helm chart (OCI)', 'Attest chart provenance', 'Publish Homebrew formulae',
]);
export class GateFailure extends Error {
  constructor(code) {
    super(`check-local-roll: ${code}; focused approval refused. Use the full application path and normal release validation.`);
    this.code = code;
  }
}
// Confirmed schema differences require the normal full gate. Unavailable or
// malformed evidence is never this type and must remain a hard refusal.
export class RollIneligible extends GateFailure {
  constructor() {
    super('release-may-advance-schema');
    this.name = 'RollIneligible';
  }
}
const requireThat = (condition, code) => { if (!condition) throw new GateFailure(code); };
const validSHA = value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value);
const positive = value => Number.isSafeInteger(value) && value > 0;
const stableVersion = value => typeof value === 'string' && /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(value);

export function validateRun(run, tag, commit) {
  requireThat(positive(run?.id) && positive(run.run_attempt) && positive(run.run_number) &&
    run.path === workflow && run.event === 'push' && run.head_branch === tag &&
    run.head_sha === commit && run.repository?.full_name === repository &&
    run.status === 'completed' && run.conclusion === 'success', 'release-run-not-verified');
  return run;
}

export function validateJobs(pages, run, mirrorCells) {
  requireThat(Array.isArray(mirrorCells) && mirrorCells.length > 0 && mirrorCells.length <= 100 &&
    new Set(mirrorCells).size === mirrorCells.length && mirrorCells.every(cell => /^[a-z0-9][a-z0-9-]*$/.test(cell)), 'release-mirror-contract-invalid');
  requireThat(Array.isArray(pages) && pages.length > 0, 'release-jobs-incomplete');
  const count = pages[0]?.total_count;
  requireThat(positive(count) && pages.every(page => page.total_count === count && Array.isArray(page.jobs)), 'release-jobs-incomplete');
  const jobs = pages.flatMap(page => page.jobs);
  requireThat(jobs.length === count && new Set(jobs.map(job => job.id)).size === count && jobs.every(job =>
    positive(job.id) && job.run_id === run.id && job.run_attempt === run.run_attempt &&
    job.head_sha === run.head_sha && job.status === 'completed' && job.conclusion === 'success'), 'release-jobs-not-settled');
  for (const name of [...requiredJobs, ...mirrorCells.map(cell => `mirror-postgresql (${cell})`)]) {
    const matching = jobs.filter(job => job.name === name);
    requireThat(matching.length === 1 && matching[0].conclusion === 'success', 'required-release-job-not-green');
  }
  const steps = jobs.find(job => job.name === 'goreleaser').steps;
  requireThat(Array.isArray(steps), 'release-publication-steps-missing');
  for (const name of requiredPublishSteps) {
    const matching = steps.filter(step => step.name === name);
    requireThat(matching.length === 1 && matching[0].status === 'completed' && matching[0].conclusion === 'success', 'release-publication-step-not-green');
  }
  return jobs.length;
}

const yamlToJSON = `require 'yaml'; require 'json'; docs = YAML.parse_stream(STDIN.read).children.map { |d| s = Psych::Nodes::Stream.new; s.children << d; YAML.safe_load(s.to_yaml, aliases: false) }.compact; STDOUT.write(JSON.generate(docs))`;
function defaultRun(file, args, options) {
  return execFileSync(file, args, {...options, stdio: ['pipe', 'pipe', 'pipe'], maxBuffer: 16 * 1024 * 1024});
}
function cleanEnvironment(file, scratch) {
  const env = {};
  for (const key of ['PATH', 'HOME', 'TMPDIR', 'USER', 'LOGNAME', 'GOPATH', 'GOCACHE', 'GOMODCACHE', 'GOPROXY', 'GOTOOLCHAIN']) {
    if (process.env[key]) env[key] = process.env[key];
  }
  if (file === 'gh') {
    for (const key of ['GH_TOKEN', 'GITHUB_TOKEN', 'GH_CONFIG_DIR']) if (process.env[key]) env[key] = process.env[key];
    env.GH_HOST = 'github.com';
    env.GH_PROMPT_DISABLED = '1';
  }
  Object.assign(env, {LC_ALL: 'C', HELM_CONFIG_HOME: join(scratch, 'helm-config'),
    HELM_CACHE_HOME: join(scratch, 'helm-cache'), HELM_DATA_HOME: join(scratch, 'helm-data'),
    HELM_REGISTRY_CONFIG: join(scratch, 'helm-config', 'registry.json'), DOCKER_CONFIG: join(scratch, 'docker')});
  return env;
}

// run is an execFileSync-compatible dependency seam for offline tests, never a CLI option.
export async function checkLocalRoll({baseRef = 'origin/main', cwd = process.cwd(), run = defaultRun} = {}) {
  const scope = classifyLocal(baseRef, cwd);
  requireThat(scope.roll_only === true, 'scope-not-pin-only');
  requireThat(stableVersion(scope.version) && stableVersion(scope.previous_version) &&
    /^[a-z0-9][a-z0-9-]*$/.test(scope.cell) && scope.values_path === `.gitops/cells/${scope.cell}/values.yaml` &&
    /^sha256:[a-f0-9]{64}$/.test(scope.image_digest), 'scope-invalid');
  let scratch;
  try { scratch = mkdtempSync(join(tmpdir(), 'witself-local-roll-')); }
  catch { throw new GateFailure('private-scratch-unavailable'); }
  try {
    const command = (code, file, args, input, timeout = 120000) => {
      try {
        return run(file, args, {cwd, env: cleanEnvironment(file, scratch), input, timeout}).toString();
      } catch {
        throw new GateFailure(code);
      }
    };
    const json = (code, file, args, input) => {
      try { return JSON.parse(command(code, file, args, input)); }
      catch (error) { if (error instanceof GateFailure) throw error; throw new GateFailure(code); }
    };
    const git = args => command('local-release-source-unavailable', 'git', args).trim();
    const api = endpoint => json('github-release-evidence-unavailable', 'gh', ['api', '--hostname', 'github.com', `repos/${repository}/${endpoint}`]);
    const yaml = text => json('rendered-yaml-invalid', 'ruby', ['-e', yamlToJSON], text);
    const tag = `v${scope.version}`;
    const previousTag = `v${scope.previous_version}`;
    const resolveTag = name => {
      const local = git(['rev-parse', '--verify', '--end-of-options', `refs/tags/${name}^{commit}`]);
      const remote = api(`commits/${name}`)?.sha;
      requireThat(validSHA(local) && remote === local, 'release-tag-identity-mismatch');
      return local;
    };
    const commit = resolveTag(tag);
    const previousCommit = resolveTag(previousTag);
    // Equal migration bytes and orchestration/schema-version code are required;
    // comparing only a maximum migration number would miss edited old migrations.
    for (const path of schemaPaths) {
      const before = git(['rev-parse', '--verify', `${previousCommit}:${path}`]);
      const after = git(['rev-parse', '--verify', `${commit}:${path}`]);
      requireThat(validSHA(before) && validSHA(after), 'release-schema-source-invalid');
      if (before !== after) throw new RollIneligible();
    }
    const release = api(`releases/tags/${tag}`);
    requireThat(release?.tag_name === tag && release.draft === false && release.prerelease === false &&
      typeof release.published_at === 'string' && Number.isFinite(Date.parse(release.published_at)), 'stable-release-not-published');
    const latestRun = () => {
      // Do not filter to successful/completed runs: a newer failed or pending
      // attempt must never fall back to an older green workflow.
      const listing = api(`actions/workflows/release.yml/runs?branch=${tag}&event=push&per_page=100`);
      requireThat(Array.isArray(listing?.workflow_runs) && listing.workflow_runs.length > 0 && positive(listing.total_count), 'release-run-missing');
      const candidates = listing.workflow_runs;
      requireThat(candidates.length === Math.min(listing.total_count, 100) && candidates.every(item => positive(item.id) && positive(item.run_number) && item.head_branch === tag && item.path === workflow), 'release-run-list-invalid');
      const newest = [...candidates].sort((a, b) => b.run_number - a.run_number || b.id - a.id)[0];
      const detail = validateRun(api(`actions/runs/${newest.id}`), tag, commit);
      requireThat(detail.id === newest.id && detail.run_number === newest.run_number, 'release-run-detail-mismatch');
      return detail;
    };
    const releaseRun = latestRun();
    const mirrors = json('release-mirror-contract-invalid', 'git', ['show', `${commit}:images/postgresql/mirror.json`]);
    requireThat(mirrors?.schema_version === 1 && mirrors.cells && typeof mirrors.cells === 'object' && !Array.isArray(mirrors.cells), 'release-mirror-contract-invalid');
    const pages = json('release-jobs-unavailable', 'gh', ['api', '--hostname', 'github.com', '--paginate', '--slurp',
      `repos/${repository}/actions/runs/${releaseRun.id}/attempts/${releaseRun.run_attempt}/jobs?per_page=100`]);
    const jobCount = validateJobs(pages, releaseRun, Object.keys(mirrors.cells));
    const digest = command('release-image-unavailable', 'bash', ['scripts/resolve-server-image-digest.sh', scope.version]).trim();
    requireThat(digest === scope.image_digest, 'release-image-pin-mismatch');
    command('release-image-signature-invalid', 'cosign', ['verify',
      '--certificate-identity', `https://github.com/${repository}/${workflow}@refs/tags/${tag}`,
      '--certificate-oidc-issuer', 'https://token.actions.githubusercontent.com',
      '--certificate-github-workflow-repository', repository,
      '--certificate-github-workflow-ref', `refs/tags/${tag}`,
      '--certificate-github-workflow-sha', commit,
      '--certificate-github-workflow-trigger', 'push', `${serverImage}@${digest}`]);
    mkdirSync(join(scratch, 'helm-config'), {mode: 0o700});
    writeFileSync(join(scratch, 'helm-config', 'registry.json'), '{}\n', {mode: 0o600});
    command('release-chart-unavailable', 'helm', ['pull', `oci://${chartRepository}/witself-server`, '--version', scope.version, '--destination', scratch]);
    const chart = join(scratch, `witself-server-${scope.version}.tgz`);
    command('release-chart-provenance-invalid', 'gh', ['attestation', 'verify', chart, '--repo', repository,
      '--signer-workflow', `${repository}/${workflow}`, '--signer-digest', commit,
      '--source-ref', `refs/tags/${tag}`, '--source-digest', commit, '--deny-self-hosted-runners']);
    const metadata = yaml(command('release-chart-metadata-invalid', 'helm', ['show', 'chart', chart]));
    requireThat(metadata.length === 1 && metadata[0].name === 'witself-server' &&
      metadata[0].version === scope.version && metadata[0].appVersion === scope.version, 'release-chart-version-mismatch');
    command('generated-values-drift', 'bash', ['scripts/gitops-cell-values.sh', '--check']);
    command('values-whitespace-invalid', 'git', ['diff', '--check', scope.base_sha, '--']);
    command('apps-chart-invalid', 'helm', ['lint', '.gitops/charts/apps', '--values', scope.values_path]);
    const applications = yaml(command('selected-cell-render-failed', 'helm', ['template', 'witself-apps', '.gitops/charts/apps',
      '--values', scope.values_path, '--show-only', 'templates/witself-server.yaml']));
    requireThat(applications.length === 1 && applications[0]?.kind === 'Application' &&
      applications[0].metadata?.name === 'witself-server', 'selected-cell-application-invalid');
    const application = applications[0];
    requireThat(application.spec?.source?.repoURL === chartRepository && application.spec.source.chart === 'witself-server' &&
      application.spec.source.targetRevision === scope.version && typeof application.spec.source.helm?.values === 'string', 'selected-cell-chart-mismatch');
    const child = yaml(application.spec.source.helm.values);
    // The apps chart normally omits repository so the attested child chart's
    // default supplies it. The final rendered containers must still match it.
    requireThat(child.length === 1 && child[0]?.image &&
      (child[0].image.repository === undefined || child[0].image.repository === serverImage) && child[0].image.tag === scope.version &&
      child[0].image.digest === digest && typeof child[0].worker?.enabled === 'boolean', 'selected-cell-image-mismatch');
    const namespace = application.spec.destination?.namespace;
    requireThat(typeof namespace === 'string' && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(namespace), 'selected-cell-namespace-invalid');
    const values = join(scratch, 'child-values.yaml');
    writeFileSync(values, application.spec.source.helm.values, {mode: 0o600});
    command('released-child-lint-failed', 'helm', ['lint', chart, '--values', values]);
    const workloads = yaml(command('released-child-render-failed', 'helm', ['template', 'witself-server', chart,
      '--namespace', namespace, '--values', values]));
    const containers = workloads.filter(item => item.kind === 'Deployment').flatMap(item => item.spec?.template?.spec?.containers ?? []);
    const expectedContainers = ['witself-server', ...(child[0].worker.enabled ? ['witself-worker'] : [])];
    for (const name of expectedContainers) {
      const matching = containers.filter(container => container.name === name);
      requireThat(matching.length === 1 && matching[0].image === `${serverImage}@${digest}`, 'released-child-image-not-pinned');
    }
    requireThat(containers.filter(container => ['witself-server', 'witself-worker'].includes(container.name)).length === expectedContainers.length,
      'released-child-image-not-pinned');
    // Close the local/remote race window before returning any focused approval.
    requireThat(resolveTag(tag) === commit && resolveTag(previousTag) === previousCommit, 'release-tag-changed-during-check');
    const finalRun = latestRun();
    requireThat(finalRun.id === releaseRun.id && finalRun.run_attempt === releaseRun.run_attempt, 'release-run-changed-during-check');
    const finalScope = classifyLocal(baseRef, cwd);
    requireThat(JSON.stringify(finalScope) === JSON.stringify(scope), 'local-roll-changed-during-check');
    return {passed: true, cell: scope.cell, version: scope.version, release_run_id: releaseRun.id, release_job_count: jobCount,
      checks: ['strict-pin-scope', 'published-release-and-ci', 'unchanged-schema', 'signed-image', 'attested-chart', 'generated-values', 'selected-cell-render']};
  } catch (error) {
    if (error instanceof GateFailure) throw error;
    throw new GateFailure('verification-unavailable');
  } finally {
    try { rmSync(scratch, {recursive: true, force: true}); }
    catch { throw new GateFailure('private-scratch-cleanup-failed'); }
  }
}

export async function main(args = process.argv.slice(2)) {
  try {
    requireThat(args.length === 2 && args[0] === '--base' && args[1] && !args[1].startsWith('-'), 'usage-check-local-roll--base-origin/main');
    const result = await checkLocalRoll({baseRef: args[1]});
    console.log(`check-local-roll: PASS ${JSON.stringify(result)}`);
    return 0;
  } catch (error) {
    console.error(error instanceof GateFailure ? error.message : new GateFailure('verification-unavailable').message);
    return 1;
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) process.exitCode = await main();
