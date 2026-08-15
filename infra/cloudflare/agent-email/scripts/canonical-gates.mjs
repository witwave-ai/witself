#!/usr/bin/env node
import { readFileSync, writeFileSync } from "node:fs";
import { isAbsolute, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import { primaryRoutingRuntime } from "./primary-routes.mjs";
import { reserveJSONReceipt } from "./receipt-journal.mjs";
import {
  assertProductionCloudflareIdentity,
} from "./wrangler-environment.mjs";
import {
  applyCanonicalGatesPlan,
  CANONICAL_GATES_WORKER,
  createCanonicalGatesPlan,
  inspectCanonicalGates,
  verifyCanonicalGatesPlan,
} from "./canonical-gates-lib.mjs";

function usage() {
  return `usage:
  node scripts/canonical-gates.mjs status
  node scripts/canonical-gates.mjs <enable|disable> --output PLAN
  node scripts/canonical-gates.mjs apply --plan PLAN --plan-sha256 SHA256 \\
    --receipt-output RECEIPT

Status and planning are read-only. Plans are mode-0600 and expire after 15
minutes. Apply atomically adds or removes both control-plane canonical email
gate secrets through Cloudflare's bulk secret endpoint while holding the shared
email operations lease. Secret values are never printed.

Required environment:
  CLOUDFLARE_API_TOKEN    Workers Scripts Read and Write
  CLOUDFLARE_ACCOUNT_ID   exact production account id
  CONTROL_PLANE_EDGE_TOKEN
                          required only for apply lease acquisition; inject
                          without writing it to disk`;
}

function readJSON(path, label) {
  try {
    return JSON.parse(readFileSync(resolve(path), "utf8"));
  } catch {
    throw new Error(`${label} was missing or invalid JSON`);
  }
}

function writeNewJSON(path, value) {
  writeFileSync(resolve(path), `${JSON.stringify(value, null, 2)}\n`, {
    flag: "wx",
    mode: 0o600,
  });
}

function exactPrivatePath(path, label) {
  if (typeof path !== "string" || !isAbsolute(path) ||
      resolve(path) !== path) {
    throw new Error(`${label} must be one canonical absolute path`);
  }
  return path;
}

export class CanonicalGatesCloudflareAPI extends CloudflareAPI {
  async listControlPlaneSecrets() {
    return this.request(
      `/accounts/${this.accountID}/workers/scripts/` +
        `${CANONICAL_GATES_WORKER}/secrets`,
    );
  }

  async patchControlPlaneSecrets(secrets) {
    return this.request(
      `/accounts/${this.accountID}/workers/scripts/` +
        `${CANONICAL_GATES_WORKER}/secrets-bulk`,
      { method: "PATCH", body: { secrets } },
    );
  }
}

export function parseCanonicalGatesArgs(argv) {
  if (argv.length === 1 && argv[0] === "status") {
    return { mode: "status" };
  }
  if (argv.length === 3 && ["enable", "disable"].includes(argv[0]) &&
      argv[1] === "--output" && argv[2]) {
    return {
      mode: "plan",
      action: argv[0],
      output: exactPrivatePath(argv[2], "canonical gates plan output"),
    };
  }
  if (argv.length === 7 && argv[0] === "apply" &&
      argv[1] === "--plan" && argv[2] &&
      argv[3] === "--plan-sha256" && argv[4] &&
      argv[5] === "--receipt-output" && argv[6]) {
    return {
      mode: "apply",
      plan: exactPrivatePath(argv[2], "canonical gates plan"),
      planSHA256: argv[4],
      receiptOutput: exactPrivatePath(
        argv[6],
        "canonical gates receipt output",
      ),
    };
  }
  throw new Error(usage());
}

export async function runCanonicalGates(
  options,
  env = process.env,
  dependencies = {},
) {
  assertProductionCloudflareIdentity(env);
  const config = cloudflareEnvironment(env);
  const api = dependencies.api ?? new CanonicalGatesCloudflareAPI({
    ...config,
    ...(dependencies.fetchAPI ? { fetchAPI: dependencies.fetchAPI } : {}),
  });
  const runtime = dependencies.runtime ??
    primaryRoutingRuntime(env, dependencies);

  if (options.mode === "status") {
    return inspectCanonicalGates(api, runtime);
  }
  if (options.mode === "plan") {
    const plan = await createCanonicalGatesPlan(
      api,
      runtime,
      options.action,
    );
    writeNewJSON(options.output, plan);
    return {
      schema: "witself.agent-email-canonical-gates-plan-created.v1",
      outcome: "created",
      action: options.action,
      plan_sha256: plan.apply_fence.sha256,
      expires_at: plan.expires_at,
    };
  }

  const plan = readJSON(options.plan, "canonical gates plan");
  verifyCanonicalGatesPlan(plan, options.planSHA256);
  const receiptFile = reserveJSONReceipt(options.receiptOutput, {
    schema: "witself.agent-email-canonical-gates-apply-pending.v1",
    action: plan.action,
    plan_sha256: plan.apply_fence?.sha256 ?? "",
    state: "apply_started_receipt_not_committed",
  });
  let receipt;
  try {
    receipt = await applyCanonicalGatesPlan(
      plan,
      options.planSHA256,
      api,
      runtime,
    );
    receiptFile.commit(receipt);
  } catch (error) {
    receiptFile.close();
    throw error;
  }
  return {
    schema: "witself.agent-email-canonical-gates-apply-result.v1",
    outcome: "verified",
    action: receipt.action,
    plan_sha256: receipt.plan_sha256,
    receipt_sha256: receipt.receipt_fence.sha256,
    gate_state: receipt.after.gate_state,
  };
}

async function main() {
  const result = await runCanonicalGates(
    parseCanonicalGatesArgs(process.argv.slice(2)),
  );
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(
      `${error instanceof Error ? error.message :
        "canonical gates operation failed"}\n`,
    );
    process.exitCode = 1;
  });
}
