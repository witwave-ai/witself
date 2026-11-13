#!/usr/bin/env node
// Run every repository-root Node self-test that a workflow step names, so the
// local gate cannot pass while CI fails one of them (PR 977: CI ran the Go JSON
// checker's self-test in four jobs and `make check` never did).
//
// The workflow steps are the only list. scripts/ci-product-workflow.test.mjs
// pins their exact `run:` text, so CI cannot source a shared list file without
// breaking that contract; this runner reads the steps instead of keeping a
// second hand-maintained copy, and fails closed on any form it cannot reproduce.
import {execFileSync, spawnSync} from 'node:child_process';
import {readdirSync, realpathSync, statSync} from 'node:fs';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';

const repositoryRoot = fileURLToPath(new URL('../', import.meta.url));
export const workflowDirectory = '.github/workflows';

// A package-scoped step runs a package's own suite from its directory after that
// package's npm ci. Each directory names the local route that reaches it; a step
// in any other working directory fails closed until it is routed here.
export const packageRoutes = Object.freeze({
  'infra/cloudflare/agent-email': 'make check-infra runs npm --prefix infra/cloudflare/agent-email test (node --test test/*.test.mjs)',
  'scripts/dashboard-acceptance': 'release workflow only; make dashboard-acceptance installs Playwright and runs it',
});

const mentionsNodeTest = /\bnode\s+--test\b/;
const exactNodeTest = /^node --test(?: (.*))?$/;
// Plain relative paths only: no flags, globs, quotes, variables, or dot segments.
const plainTestFile = /^(?:[A-Za-z0-9_][A-Za-z0-9_.-]*\/)*[A-Za-z0-9_][A-Za-z0-9_.-]*\.(?:mjs|cjs|js)$/;

const readWorkflowsRuby = `
  result = ARGV.to_h do |path|
    [File.basename(path), YAML.safe_load(File.read(path, encoding: 'UTF-8'), aliases: false)]
  end
  puts JSON.generate(result)
`;

export function loadWorkflows(root = repositoryRoot) {
  const directory = join(root, workflowDirectory);
  const names = readdirSync(directory).filter(name => /\.ya?ml$/.test(name)).sort();
  if (names.length === 0) throw new Error(`no workflows found in ${workflowDirectory}`);
  let output;
  try {
    output = execFileSync('ruby', ['-rjson', '-ryaml', '-e', readWorkflowsRuby, ...names.map(name => join(directory, name))], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 64 * 1024 * 1024,
    });
  } catch (error) {
    const reason = error.code === 'ENOENT' ? 'ruby missing' : String(error.stderr || error.message).trim();
    throw new Error(`could not read ${workflowDirectory}: ${reason}`);
  }
  return JSON.parse(output);
}

function workingDirectory(workflow, job, step) {
  const value = step['working-directory'] ?? job.defaults?.run?.['working-directory'] ??
    workflow.defaults?.run?.['working-directory'];
  if (value === undefined || value === null) return '';
  const directory = String(value).trim().replace(/^\.\/+/, '').replace(/\/+$/, '');
  return directory === '.' ? '' : directory;
}

export function selectNodeSelfTests(workflows, {isFile}) {
  const files = new Set();
  const packageSteps = [];
  for (const name of Object.keys(workflows).sort()) {
    const workflow = workflows[name] ?? {};
    for (const [jobId, job] of Object.entries(workflow.jobs ?? {})) {
      const steps = Array.isArray(job?.steps) ? job.steps : [];
      for (const [index, step] of steps.entries()) {
        if (typeof step?.run !== 'string') continue;
        const label = `${name} job ${jobId} step ${index + 1}${step.name ? ` (${step.name})` : ''}`;
        const directory = workingDirectory(workflow, job, step);
        for (const line of step.run.split('\n')) {
          if (!mentionsNodeTest.test(line)) continue;
          // trim() also drops the carriage return of a CRLF line.
          const command = line.trim();
          const match = exactNodeTest.exec(command);
          if (!match) throw new Error(`${label}: unsupported node --test command: ${command}`);
          const args = match[1] === undefined ? [] : match[1].split(' ');
          if (directory !== '') {
            if (!Object.hasOwn(packageRoutes, directory)) {
              throw new Error(`${label}: node --test in working directory ${directory} has no local route; ` +
                'run it from the repository root or add the package to packageRoutes in scripts/check-node-selftests.mjs');
            }
            packageSteps.push({label, directory, files: args, route: packageRoutes[directory]});
            continue;
          }
          if (args.length === 0) throw new Error(`${label}: node --test names no test files: ${command}`);
          for (const file of args) {
            if (!plainTestFile.test(file)) throw new Error(`${label}: unsupported node --test argument ${JSON.stringify(file)}`);
            if (!isFile(file)) throw new Error(`${label}: ${file} is not a regular file`);
            files.add(file);
          }
        }
      }
    }
  }
  if (files.size === 0) throw new Error(`no repository-root node --test files found in ${workflowDirectory}`);
  return {files: [...files].sort(), packageSteps};
}

function isRegularFile(path) {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

export function main({
  argv = process.argv.slice(2), root = repositoryRoot, load = loadWorkflows,
  isFile = file => isRegularFile(join(root, file)), spawn = spawnSync, log = console.log,
} = {}) {
  const list = argv.length === 1 && argv[0] === '--list';
  if (argv.length !== 0 && !list) throw new Error('usage: node scripts/check-node-selftests.mjs [--list]');
  const {files, packageSteps} = selectNodeSelfTests(load(root), {isFile});
  if (list) {
    for (const file of files) log(file);
    return 0;
  }
  for (const step of packageSteps) log(`check-node-selftests: package step ${step.label}: ${step.route}`);
  log(`check-node-selftests: node --test ${files.join(' ')}`);
  const result = spawn(process.execPath, ['--test', ...files], {cwd: root, stdio: 'inherit'});
  if (result.error) throw result.error;
  if (result.status !== 0 || result.signal) {
    log(`check-node-selftests: FAIL node --test ${result.signal ? `signal=${result.signal}` : `status=${result.status}`}`);
    return 1;
  }
  log('check-node-selftests: PASS');
  return 0;
}

function invokedAsMain() {
  try {
    return Boolean(process.argv[1]) && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (invokedAsMain()) {
  try {
    process.exitCode = main();
  } catch (error) {
    console.error(`check-node-selftests: FAIL: ${error.message}`);
    process.exitCode = 1;
  }
}
