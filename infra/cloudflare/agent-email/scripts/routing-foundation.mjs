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
  applyRoutingFoundationPlan,
  createRoutingFoundationPlan,
  inspectRoutingFoundation,
  verifyRoutingFoundationPlan,
} from "./routing-foundation-lib.mjs";

function usage() {
  return `usage:
  node scripts/routing-foundation.mjs status
  node scripts/routing-foundation.mjs <enable|disable> --output PLAN
  node scripts/routing-foundation.mjs apply --plan PLAN --plan-sha256 SHA256 \\
    --receipt-output RECEIPT

Status and enable/disable planning are read-only. Plans are mode-0600 and
expire after 15 minutes. Only apply can change the zone-wide Email Routing
subaddressing setting; it holds the shared operations lease, preserves every
other Email Routing setting and rule, and requires the exact reviewed plan
SHA-256.

Required environment:
  CLOUDFLARE_API_TOKEN    Zone Settings Read/Write, Email Routing Rules Read,
                          and Workers Script Read
  CLOUDFLARE_ACCOUNT_ID   exact production account id
  CLOUDFLARE_ZONE_ID      exact witmail.net zone id
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

export class RoutingFoundationCloudflareAPI extends CloudflareAPI {
  async editEmailRoutingSettings(contract) {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing`, {
      method: "PATCH",
      body: contract,
    });
  }
}

export function parseRoutingFoundationArgs(argv) {
  if (argv.length === 1 && argv[0] === "status") {
    return { mode: "status" };
  }
  if (argv.length === 3 && ["enable", "disable"].includes(argv[0]) &&
      argv[1] === "--output" && argv[2]) {
    return {
      mode: "plan",
      action: argv[0],
      output: exactPrivatePath(
        argv[2],
        "routing foundation plan output",
      ),
    };
  }
  if (argv.length === 7 && argv[0] === "apply" &&
      argv[1] === "--plan" && argv[2] &&
      argv[3] === "--plan-sha256" && argv[4] &&
      argv[5] === "--receipt-output" && argv[6]) {
    return {
      mode: "apply",
      plan: exactPrivatePath(argv[2], "routing foundation plan"),
      planSHA256: argv[4],
      receiptOutput: exactPrivatePath(
        argv[6],
        "routing foundation receipt output",
      ),
    };
  }
  throw new Error(usage());
}

export async function runRoutingFoundation(
  options,
  env = process.env,
  dependencies = {},
) {
  assertProductionCloudflareIdentity(env);
  const config = cloudflareEnvironment(env);
  if (!config.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
  const api = dependencies.api ?? new RoutingFoundationCloudflareAPI({
    ...config,
    ...(dependencies.fetchAPI ? { fetchAPI: dependencies.fetchAPI } : {}),
  });
  if (options.mode === "status") {
    return inspectRoutingFoundation(api);
  }
  if (options.mode === "plan") {
    const plan = await createRoutingFoundationPlan(api, options.action);
    writeNewJSON(options.output, plan);
    return {
      schema: "witself.agent-email-routing-foundation-plan-created.v1",
      outcome: "created",
      action: options.action,
      plan_sha256: plan.apply_fence.sha256,
      expires_at: plan.expires_at,
    };
  }

  const plan = readJSON(options.plan, "routing foundation plan");
  verifyRoutingFoundationPlan(plan, options.planSHA256);
  const receiptFile = reserveJSONReceipt(options.receiptOutput, {
    schema: "witself.agent-email-routing-foundation-apply-pending.v1",
    action: plan.action,
    plan_sha256: plan.apply_fence?.sha256 ?? "",
    state: "apply_started_receipt_not_committed",
  });
  let receipt;
  try {
    const runtime = dependencies.runtime ??
      primaryRoutingRuntime(env, dependencies);
    receipt = await applyRoutingFoundationPlan(
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
    schema: "witself.agent-email-routing-foundation-apply-result.v1",
    outcome: "verified",
    action: receipt.action,
    plan_sha256: receipt.plan_sha256,
    receipt_sha256: receipt.receipt_fence.sha256,
    subaddressing_enabled: receipt.after_settings.support_subaddress,
  };
}

async function main() {
  const result = await runRoutingFoundation(
    parseRoutingFoundationArgs(process.argv.slice(2)),
  );
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(
      `${error instanceof Error ? error.message :
        "routing foundation operation failed"}\n`,
    );
    process.exitCode = 1;
  });
}
