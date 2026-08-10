#!/usr/bin/env node
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import {
  applyPrimaryRoutingPlan,
  createPrimaryRoutingPlan,
  inspectPrimaryCanary,
} from "./primary-routing-lib.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const PLANNED_ACTIONS = new Set(["prepare", "activate", "disable", "remove"]);
const WORKERS = Object.freeze({
  control_plane: "witself-control-plane",
  email_edge: "witself-agent-email-pilot",
});

function usage() {
  return `usage:
  node scripts/primary-routes.mjs status MANIFEST
  node scripts/primary-routes.mjs <prepare|activate|disable|remove> MANIFEST --output PLAN
  node scripts/primary-routes.mjs apply --plan PLAN --plan-sha256 SHA256

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

function wranglerJSON(args, operation) {
  const result = spawnSync("wrangler", args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 5 * 1024 * 1024,
  });
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
  inspect = wranglerJSON,
} = {}) {
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
  return {
    inspectWorkers: async () => {
      const controlPlane = inspectWorker(
        WORKERS.control_plane,
        "control plane",
        inspect,
      );
      const emailEdge = inspectWorker(WORKERS.email_edge, "email edge", inspect);
      return {
        control_plane_deployment: controlPlane.deployment,
        control_plane_version: controlPlane.version,
        email_edge_deployment: emailEdge.deployment,
        email_edge_version: emailEdge.version,
      };
    },
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
      output: argv[3],
      plan: "",
      planSHA256: "",
    };
  }
  if (argv[0] === "apply" && argv.length === 5 && argv[1] === "--plan" &&
      argv[2] && argv[3] === "--plan-sha256" && argv[4]) {
    return {
      mode: "apply",
      manifest: "",
      output: "",
      plan: argv[2],
      planSHA256: argv[4],
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
  return applyPrimaryRoutingPlan(plan, options.planSHA256, api, runtime);
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
