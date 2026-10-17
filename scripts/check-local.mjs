#!/usr/bin/env node
// Local pre-push selection only. GitHub CI deliberately keeps its full gates.
import {spawnSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';
import {classifyLocal as classifyProduct} from './ci-product-scope.mjs';
import {classifyLocal as classifyRoll, checkLocalIntegrity, checkLocalContext} from './local-roll-scope.mjs';
import {checkLocalRoll, GateFailure, RollIneligible} from './check-local-roll.mjs';

const BASE = 'origin/main';

export function selectLocalGate(cwd, productClassifier = classifyProduct, rollClassifier = classifyRoll, integrityChecker = checkLocalIntegrity) {
  try {
    if (integrityChecker(cwd).safe !== true) return 'application';
    if (productClassifier(BASE, cwd).website_only === true) return 'website';
    if (rollClassifier(BASE, cwd).roll_only === true) return 'release-roll';
  } catch {
    // A missing comparison or unsupported change never earns the shorter path.
  }
  return 'application';
}

function execute(file, args, options) {
  const result = spawnSync(file, args, {...options, stdio: 'inherit'});
  if (result.error || result.signal || result.status !== 0) {
    throw new Error(`local check: ${file} did not complete successfully`);
  }
}

export async function runLocalChecks({
  cwd = process.cwd(), env = process.env, run = execute,
  productClassifier = classifyProduct, rollClassifier = classifyRoll,
  integrityChecker = checkLocalIntegrity,
  contextChecker = checkLocalContext,
  verifyRoll = checkLocalRoll, log = console.log,
} = {}) {
  // Never classify against a stale remote-tracking ref or accept a caller's
  // substitute base. Tests inject operations directly, not through CLI flags.
  if (contextChecker(cwd).safe !== true) throw new Error('local check: repository context unavailable');
  run('git', ['fetch', 'origin'], {cwd, env});
  let gate = selectLocalGate(cwd, productClassifier, rollClassifier, integrityChecker);
  log(`local check: selected ${gate}`);
  if (gate === 'website') {
    run('npm', ['ci', '--ignore-scripts', '--prefix', 'infra/cloudflare/control-plane'], {cwd, env});
    run('make', ['check-product-site'], {cwd, env});
  } else if (gate === 'release-roll') {
    // Bad or unavailable release evidence is a refusal. Passing application
    // tests cannot make an incorrect released artifact safe to deploy.
    try {
      const proof = await verifyRoll({baseRef: BASE, cwd});
      if (proof?.passed !== true) throw new Error('local check: release verification did not pass');
    } catch (error) {
      if (!(error instanceof RollIneligible)) throw error;
      gate = 'application';
      log('local check: migration sources differ; selected application');
    }
  }
  if (gate === 'application') {
    run('make', ['check'], {cwd, env: {
      ...env, WITSELF_TEST_REQUIRE_DATABASE: '1', WITSELF_TEST_REQUIRE_NODE: '1',
    }});
  } else if (selectLocalGate(cwd, productClassifier, rollClassifier, integrityChecker) !== gate) {
    throw new Error('local check: checkout changed during focused checks');
  }
  log(`local check: PASS ${gate}`);
  return {passed: true, gate};
}

export async function main(args = process.argv.slice(2)) {
  if (args.length) throw new Error('usage: node scripts/check-local.mjs');
  return runLocalChecks();
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => {
    // GateFailure contains only a fixed reason code, never provider output.
    if (error instanceof GateFailure) console.error(error.message);
    console.error('local check: FAIL; no local pre-push approval. Resolve the failed gate; release verification remains required.');
    process.exitCode = 1;
  });
}
