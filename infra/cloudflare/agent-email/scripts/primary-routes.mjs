#!/usr/bin/env node
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import {
  applyPrimaryRoutingPlan,
  createPrimaryRoutingPlan,
  inspectPrimaryCanary,
  operationsLeaseControlPlaneOrigin,
  verifyPrimaryRoutingPlan,
} from "./primary-routing-lib.mjs";
import { reserveJSONReceipt } from "./receipt-journal.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";
import {
  withAgentEmailOperationsLease,
} from "../../control-plane/scripts/agent-email-operations-lease-client.mjs";
import { PRODUCTION_RECEIVE_WORKER } from "../src/worker-names.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const PLANNED_ACTIONS = new Set(["prepare", "activate", "disable", "remove"]);
const WORKERS = Object.freeze({
  control_plane: "witself-control-plane",
  email_edge: PRODUCTION_RECEIVE_WORKER,
});

function usage() {
  return `usage:
  node scripts/primary-routes.mjs status MANIFEST
  node scripts/primary-routes.mjs <prepare|activate|disable|remove> MANIFEST --output PLAN
  node scripts/primary-routes.mjs apply --plan PLAN --plan-sha256 SHA256 \\
    --receipt-output RECEIPT

The four lifecycle commands only create a short-lived, mode-0600 review plan.
Only apply can mutate exact literal canary rules, and apply requires the exact
plan SHA-256 fence. This tool has no catch-all mutation method and never writes
legacy pilot KV rows.

Required environment:
  CLOUDFLARE_API_TOKEN    Zone Settings Read, Email Routing Rules Write,
                          Workers Script Read, and Workers KV Read
  CLOUDFLARE_ACCOUNT_ID   exact target account id
  CLOUDFLARE_ZONE_ID      exact witmail.net zone id
  EMAIL_DIRECTORY_KV_ID   isolated agent-email route directory id
  CONTROL_PLANE_EDGE_TOKEN
                          required only for status, prepare, and activate
                          readiness; inject without writing it to disk`;
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

function wranglerJSON(args, operation, environment = process.env) {
  const result = spawnSync(
    "wrangler",
    withReviewedWranglerEnvironmentFile(args),
    {
    cwd: root,
    env: sanitizedWranglerInspectionEnvironment(environment),
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 5 * 1024 * 1024,
    timeout: 30_000,
    },
  );
  if (result.error || result.status !== 0) {
    throw new Error(`could not ${operation} with Wrangler`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error(`Wrangler ${operation} output was invalid JSON`);
  }
}

function inspectWorker(name, label, inspect) {
  const deployment = inspect(
    ["deployments", "status", "--name", name, "--json"],
    `inspect the ${label} deployment`,
  );
  const versionID = deployment?.versions?.length === 1
    ? deployment.versions[0]?.version_id
    : "";
  const version = inspect(
    ["versions", "view", String(versionID ?? ""), "--name", name, "--json"],
    `inspect the ${label} Worker version`,
  );
  return { deployment, version };
}

export function primaryRoutingRuntime(env = process.env, {
  fetchAPI = fetch,
  inspect,
  newLeaseHolderID,
} = {}) {
  assertProductionCloudflareIdentity(env);
  const workerInspector = inspect ?? ((args, operation) =>
    wranglerJSON(args, operation, env));
  const edgeToken = String(env.CONTROL_PLANE_EDGE_TOKEN ?? "");
  const controlPlaneJSON = async (url) => {
    if (edgeToken.length < 16 || edgeToken.length > 8_192 ||
        edgeToken !== edgeToken.trim()) {
      throw new Error("CONTROL_PLANE_EDGE_TOKEN is missing or invalid");
    }
    let response;
    try {
      response = await fetchAPI(url, {
        method: "GET",
        headers: { Authorization: `Bearer ${edgeToken}` },
        redirect: "error",
        signal: AbortSignal.timeout(15_000),
      });
    } catch {
      throw new Error("control-plane managed email inspection failed");
    }
    const declared = Number(response.headers.get("Content-Length"));
    if (!response.ok || (Number.isFinite(declared) && declared > 16_384)) {
      throw new Error("control-plane managed email inspection failed");
    }
    const raw = await response.text();
    if (raw.length < 2 || raw.length > 16_384) {
      throw new Error("control-plane managed email inspection failed");
    }
    try {
      return JSON.parse(raw);
    } catch {
      throw new Error("control-plane managed email inspection returned invalid JSON");
    }
  };
  const inspectWorkers = async () => {
      const controlPlane = inspectWorker(
        WORKERS.control_plane,
        "control plane",
        workerInspector,
      );
      const emailEdge = inspectWorker(
        WORKERS.email_edge,
        "email edge",
        workerInspector,
      );
      return {
        control_plane_deployment: controlPlane.deployment,
        control_plane_version: controlPlane.version,
        email_edge_deployment: emailEdge.deployment,
        email_edge_version: emailEdge.version,
      };
  };
  return {
    inspectWorkers,
    getControlPlaneReadiness: async (origin) => controlPlaneJSON(
      new URL("/v1/email/managed-delivery/readiness", origin),
    ),
    getControlPlaneProjection: async (origin, domain, realmLabel) => {
      const url = new URL(
        `/v1/email/realm-routes/${encodeURIComponent(domain)}/${encodeURIComponent(realmLabel)}`,
        origin,
      );
      return controlPlaneJSON(url);
    },
    operationsLease: {
      run: async (operation, work) => {
        const origin = operationsLeaseControlPlaneOrigin(
          await inspectWorkers(),
        );
        return withAgentEmailOperationsLease(operation, work, {
          endpoint: origin.replace(/\/$/, ""),
          token: edgeToken,
          fetchImpl: fetchAPI,
          ...(newLeaseHolderID
            ? { randomUUIDImpl: newLeaseHolderID }
            : {}),
        });
      },
    },
  };
}

export function parsePrimaryRouteArgs(argv) {
  if (argv[0] === "status" && argv.length === 2) {
    return { mode: "status", manifest: argv[1], output: "", plan: "", planSHA256: "" };
  }
  if (PLANNED_ACTIONS.has(argv[0]) && argv.length === 4 && argv[2] === "--output" && argv[3]) {
    return {
      mode: "plan",
      action: argv[0],
      manifest: argv[1],
      output: exactPrivatePath(argv[3], "primary routing plan output"),
      plan: "",
      planSHA256: "",
    };
  }
  if (argv[0] === "apply" && argv.length === 7 && argv[1] === "--plan" &&
      argv[2] && argv[3] === "--plan-sha256" && argv[4] &&
      argv[5] === "--receipt-output" && argv[6]) {
    return {
      mode: "apply",
      manifest: "",
      output: "",
      plan: exactPrivatePath(argv[2], "primary routing plan"),
      planSHA256: argv[4],
      receiptOutput: exactPrivatePath(argv[6], "primary routing receipt output"),
    };
  }
  throw new Error(usage());
}

export async function runPrimaryRoutes(options, env = process.env, dependencies = {}) {
  const config = cloudflareEnvironment(env);
  if (!config.zoneID || !config.namespaceID) {
    throw new Error("CLOUDFLARE_ZONE_ID and EMAIL_DIRECTORY_KV_ID are required");
  }
  const api = dependencies.api ?? new CloudflareAPI({
    ...config,
    ...(dependencies.fetchAPI ? { fetchAPI: dependencies.fetchAPI } : {}),
  });
  const runtime = dependencies.runtime ?? primaryRoutingRuntime(env, dependencies);
  if (options.mode === "status") {
    return inspectPrimaryCanary(api, runtime, readJSON(options.manifest, "primary canary manifest"));
  }
  if (options.mode === "plan") {
    const plan = await createPrimaryRoutingPlan(
      api,
      runtime,
      readJSON(options.manifest, "primary canary manifest"),
      options.action,
    );
    writeNewJSON(options.output, plan);
    return {
      schema: "witself.agent-email-primary-routing-plan-created.v1",
      outcome: "created",
      action: options.action,
      plan_sha256: plan.apply_fence.sha256,
      expires_at: plan.expires_at,
    };
  }
  const plan = readJSON(options.plan, "primary routing plan");
  verifyPrimaryRoutingPlan(plan, options.planSHA256);
  const receiptFile = reserveJSONReceipt(options.receiptOutput, {
    schema: "witself.agent-email-primary-routing-apply-pending.v1",
    action: plan.action,
    plan_sha256: plan.apply_fence?.sha256 ?? "",
    state: "apply_started_receipt_not_committed",
  });
  let receipt;
  try {
    receipt = await applyPrimaryRoutingPlan(plan, options.planSHA256, api, runtime);
    receiptFile.commit(receipt);
  } catch (error) {
    receiptFile.close();
    throw error;
  }
  return {
    schema: "witself.agent-email-primary-routing-apply-result.v1",
    outcome: "verified",
    action: receipt.action,
    plan_sha256: receipt.plan_sha256,
    receipt_sha256: receipt.receipt_fence.sha256,
    enabled_rules: receipt.rules.enabled,
    disabled_rules: receipt.rules.disabled,
    conflicts: receipt.rules.conflicts,
  };
}

async function main() {
  const result = await runPrimaryRoutes(parsePrimaryRouteArgs(process.argv.slice(2)));
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
}

if (process.argv[1] != null && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : "primary routing failed"}\n`);
    process.exitCode = 1;
  });
}
