#!/usr/bin/env node
import { readFileSync, writeFileSync } from "node:fs";
import { isAbsolute, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import {
  applyCatchAllPlan,
  createCatchAllPlan,
  inspectCatchAll,
  verifyCatchAllPlan,
} from "./catch-all-routing-lib.mjs";
import { primaryRoutingRuntime } from "./primary-routes.mjs";
import { reserveJSONReceipt } from "./receipt-journal.mjs";

function usage() {
  return `usage:
  node scripts/catch-all-routes.mjs status MANIFEST
  node scripts/catch-all-routes.mjs enable MANIFEST --change-id ID \\
    --provider-review-sha256 SHA256 --output PLAN
  node scripts/catch-all-routes.mjs disable MANIFEST --output PLAN
  node scripts/catch-all-routes.mjs rollback MANIFEST --receipt RECEIPT --output PLAN
  node scripts/catch-all-routes.mjs apply --plan PLAN --plan-sha256 SHA256 \\
    --receipt-output RECEIPT [--confirm-enable-witmail-net]

Status and all named actions except apply are read-only. Enable planning requires
the SHA-256 of a separately reviewed provider-contract record. Apply re-reads
all state and requires the exact short-lived plan fence. An enable apply also
requires the literal --confirm-enable-witmail-net argument. The protected
mode-0600 receipt is mandatory and is the only input accepted for a rollback
plan. Rollback can restore only a disabled predecessor and can never re-enable
the catch-all.`;
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
  if (typeof path !== "string" || !isAbsolute(path) || resolve(path) !== path) {
    throw new Error(`${label} must be one canonical absolute path`);
  }
  return path;
}

function reserveReceipt(path, plan) {
  return reserveJSONReceipt(path, {
    schema: "witself.agent-email-catch-all-apply-pending.v1",
    action: plan.action,
    plan_sha256: plan.apply_fence?.sha256 ?? "",
    state: "apply_started_receipt_not_committed",
  });
}

export class CatchAllCloudflareAPI extends CloudflareAPI {
  async replaceCatchAll(contract) {
    if (!this.zoneID) throw new Error("CLOUDFLARE_ZONE_ID is required");
    return this.request(`/zones/${this.zoneID}/email/routing/rules/catch_all`, {
      method: "PUT",
      body: contract,
    });
  }
}

function optionMap(argv, start) {
  const values = new Map();
  const flags = new Set();
  for (let index = start; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--confirm-enable-witmail-net") {
      if (flags.has(argument)) throw new Error(usage());
      flags.add(argument);
      continue;
    }
    if (!["--change-id", "--provider-review-sha256", "--output", "--receipt", "--plan", "--plan-sha256", "--receipt-output"].includes(argument) ||
        values.has(argument) || !argv[index + 1] || argv[index + 1].startsWith("--")) {
      throw new Error(usage());
    }
    values.set(argument, argv[index + 1]);
    index += 1;
  }
  return { values, flags };
}

function exactOptions(values, flags, names, allowedFlags = []) {
  return values.size === names.length && names.every((name) => values.has(name)) &&
    flags.size === allowedFlags.length && allowedFlags.every((name) => flags.has(name));
}

export function parseCatchAllArgs(argv) {
  if (argv[0] === "status" && argv.length === 2) {
    return { mode: "status", manifest: argv[1] };
  }
  if (["enable", "disable", "rollback"].includes(argv[0]) && argv[1]) {
    const { values, flags } = optionMap(argv, 2);
    if (argv[0] === "enable" && exactOptions(values, flags, [
      "--change-id", "--provider-review-sha256", "--output",
    ])) {
      return {
        mode: "plan",
        action: "enable",
        manifest: argv[1],
        output: exactPrivatePath(values.get("--output"), "catch-all plan output"),
        review: {
          change_id: values.get("--change-id"),
          provider_contract_review_sha256: values.get("--provider-review-sha256"),
        },
      };
    }
    if (argv[0] === "disable" && exactOptions(values, flags, ["--output"])) {
      return {
        mode: "plan",
        action: "disable",
        manifest: argv[1],
        output: exactPrivatePath(values.get("--output"), "catch-all plan output"),
      };
    }
    if (argv[0] === "rollback" && exactOptions(values, flags, ["--receipt", "--output"])) {
      return {
        mode: "plan",
        action: "rollback",
        manifest: argv[1],
        receipt: exactPrivatePath(values.get("--receipt"), "catch-all receipt"),
        output: exactPrivatePath(values.get("--output"), "catch-all plan output"),
      };
    }
  }
  if (argv[0] === "apply") {
    const { values, flags } = optionMap(argv, 1);
    if (values.size === 3 && ["--plan", "--plan-sha256", "--receipt-output"]
      .every((name) => values.has(name)) &&
      (flags.size === 0 || exactOptions(values, flags,
        ["--plan", "--plan-sha256", "--receipt-output"],
        ["--confirm-enable-witmail-net"]))) {
      return {
        mode: "apply",
        plan: exactPrivatePath(values.get("--plan"), "catch-all plan"),
        planSHA256: values.get("--plan-sha256"),
        receiptOutput: exactPrivatePath(
          values.get("--receipt-output"), "catch-all receipt output",
        ),
        confirmEnable: flags.has("--confirm-enable-witmail-net"),
      };
    }
  }
  throw new Error(usage());
}

export async function runCatchAllRoutes(options, env = process.env, dependencies = {}) {
  const config = cloudflareEnvironment(env);
  if (!config.zoneID || !config.namespaceID) {
    throw new Error("CLOUDFLARE_ZONE_ID and EMAIL_DIRECTORY_KV_ID are required");
  }
  const api = dependencies.api ?? new CatchAllCloudflareAPI({
    ...config,
    ...(dependencies.fetchAPI ? { fetchAPI: dependencies.fetchAPI } : {}),
  });
  const runtime = dependencies.runtime ?? primaryRoutingRuntime(env, dependencies);
  if (options.mode === "status") {
    return inspectCatchAll(api, runtime, readJSON(options.manifest, "primary canary manifest"));
  }
  if (options.mode === "plan") {
    const plan = await createCatchAllPlan(
      api,
      runtime,
      readJSON(options.manifest, "primary canary manifest"),
      options.action,
      {
        review: options.review ?? null,
        rollbackReceipt: options.receipt
          ? readJSON(options.receipt, "catch-all receipt")
          : null,
      },
    );
    writeNewJSON(options.output, plan);
    return {
      schema: "witself.agent-email-catch-all-plan-created.v1",
      outcome: "created",
      action: options.action,
      plan_sha256: plan.apply_fence.sha256,
      expires_at: plan.expires_at,
    };
  }

  const plan = readJSON(options.plan, "catch-all plan");
  if (plan.action === "enable" && options.confirmEnable !== true) {
    throw new Error("catch-all enable apply requires --confirm-enable-witmail-net");
  }
  if (plan.action !== "enable" && options.confirmEnable === true) {
    throw new Error("enable confirmation is invalid for a non-enable catch-all plan");
  }
  verifyCatchAllPlan(plan, options.planSHA256);
  // Reserve and durably mark the protected receipt path before the external
  // mutation. A crash can leave a clearly non-receipt pending journal, but an
  // existing path or unwritable destination can never be discovered only
  // after the catch-all changed.
  const receiptFile = reserveReceipt(options.receiptOutput, plan);
  let receipt;
  try {
    receipt = await applyCatchAllPlan(plan, options.planSHA256, api, runtime);
    receiptFile.commit(receipt);
  } catch (error) {
    receiptFile.close();
    throw error;
  }
  return {
    schema: "witself.agent-email-catch-all-apply-result.v1",
    outcome: "verified",
    action: receipt.action,
    plan_sha256: receipt.plan_sha256,
    receipt_sha256: receipt.receipt_fence.sha256,
    catch_all_enabled: receipt.after_contract.enabled,
  };
}

async function main() {
  const result = await runCatchAllRoutes(parseCatchAllArgs(process.argv.slice(2)));
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
}

if (process.argv[1] != null && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : "catch-all routing failed"}\n`);
    process.exitCode = 1;
  });
}
