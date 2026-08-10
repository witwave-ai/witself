#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  releaseMessage,
} from "./deployment-identity.mjs";
import { sourceIdentity } from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";
import {
  withAgentEmailOperationsLease,
} from "../../control-plane/scripts/agent-email-operations-lease-client.mjs";
import {
  runLeaseGuardedCommand,
} from "../../control-plane/scripts/agent-email-lease-guarded-command.mjs";
import {
  createPrivateDeploymentConfig,
} from "../../control-plane/scripts/private-deployment-config.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const CANONICAL_CONTROL_PLANE_ORIGIN = "https://self.witwave.ai";
const MANAGED_COHORT_PROTOCOL_RELEASE = "0.0.241";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function releaseAtLeast(release, minimum) {
  const parse = (value) => {
    const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:[-+].*)?$/.exec(value);
    if (!match) throw new Error("managed cohort release identity was invalid");
    return match.slice(1, 4).map(Number);
  };
  const left = parse(release);
  const right = parse(minimum);
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return left[index] > right[index];
  }
  return true;
}

function activeVersionID(deployment) {
  if (!deployment || typeof deployment !== "object" || Array.isArray(deployment) ||
      !UUID.test(String(deployment.id ?? "")) || deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error("control-plane deployment was not one version at 100 percent");
  }
  return deployment.versions[0].version_id;
}

function plainBindings(version, expectedVersionID) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedVersionID || !Array.isArray(version.resources?.bindings)) {
    throw new Error("active control-plane version was invalid");
  }
  const bindings = new Map();
  for (const binding of version.resources.bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || !binding.name || bindings.has(binding.name)) {
      throw new Error("active control-plane binding inventory was invalid");
    }
    bindings.set(binding.name, binding);
  }
  const plain = (name) => {
    const binding = bindings.get(name);
    if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
      throw new Error(`active control plane was missing ${name}`);
    }
    return binding.text;
  };
  return plain;
}

export function verifyManagedCohortDeploymentOrder(
  targetRelease,
  targetAllowlist,
  deployment,
  version,
) {
  if (!releaseAtLeast(targetRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    return Object.freeze({ required: false });
  }
  const targetAccounts = parseManagedDeliveryAccountAllowlist(targetAllowlist);
  const versionID = activeVersionID(deployment);
  const plain = plainBindings(version, versionID);
  const controlPlaneRelease = plain("WITSELF_EDGE_RELEASE_VERSION");
  if (!releaseAtLeast(controlPlaneRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    throw new Error("managed cohort email edge deployment requires a v0.0.241 or newer control plane");
  }
  const controlPlaneAccounts = parseManagedDeliveryAccountAllowlist(
    plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST"),
  );
  const controlPlane = new Set(controlPlaneAccounts);
  if (targetAccounts.some((accountID) => !controlPlane.has(accountID))) {
    throw new Error(
      "email edge managed cohort must be contained by the active control-plane cohort; add to the control plane first",
    );
  }
  return Object.freeze({
    required: true,
    control_plane_release: controlPlaneRelease,
    target_account_count: targetAccounts.length,
    active_control_plane_account_count: controlPlaneAccounts.length,
  });
}

function wranglerJSON(args, operation) {
  const result = spawnSync("wrangler", args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 5 * 1024 * 1024,
    timeout: 30_000,
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

export function preflightManagedCohortDeploymentOrder(
  targetRelease,
  targetAllowlist,
  inspect = wranglerJSON,
) {
  if (!releaseAtLeast(targetRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    return Object.freeze({ required: false });
  }
  const deployment = inspect([
    "deployments", "status", "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane deployment");
  const versionID = activeVersionID(deployment);
  const version = inspect([
    "versions", "view", versionID, "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane version");
  return verifyManagedCohortDeploymentOrder(
    targetRelease, targetAllowlist, deployment, version,
  );
}

function run(command, args, { signal, timeoutMs = 5 * 60_000 } = {}) {
  return runLeaseGuardedCommand(command, args, {
    cwd: root,
    signal,
    timeoutMs,
  });
}

export function canonicalControlPlaneOrigin(raw) {
  const value = String(raw ?? "");
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error("email edge deployment requires the canonical control-plane origin");
  }
  if (value !== value.trim() || parsed.protocol !== "https:" ||
      parsed.username || parsed.password ||
      parsed.search || parsed.hash || parsed.pathname !== "/" ||
      parsed.origin !== CANONICAL_CONTROL_PLANE_ORIGIN) {
    throw new Error("email edge deployment requires the canonical control-plane origin");
  }
  return parsed.origin;
}

export async function main() {
  const release = sourceIdentity({ requireRelease: true });
  const leaseOrigin = canonicalControlPlaneOrigin(process.env.CONTROL_PLANE_URL);
  const targetAllowlist = String(
    process.env.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST ?? "",
  );
  const config = await createPrivateDeploymentConfig({
    prefix: "witself-agent-email-deploy-",
    parentDirectory: resolve(root, ".."),
    entrypointTarget: join(root, "src", "index.js"),
    render: (path) => run(process.execPath, [
      join(root, "scripts", "render-wrangler.mjs"),
      "--output", path,
    ]),
    validate: (path) => run(process.execPath, [
      join(root, "scripts", "assert-custom-domain-dark.mjs"),
      "--config", path,
    ]),
  });
  try {
    await withAgentEmailOperationsLease(
      "email_edge_deploy",
      async ({ signal }) => {
        await config.assertUnchanged();
        preflightManagedCohortDeploymentOrder(
          release.version,
          targetAllowlist,
        );
        await run(process.execPath, [
          join(root, "scripts", "assert-custom-domain-dark.mjs"),
          "--config", config.path,
        ], { signal });
        await config.assertUnchanged();
        await run("wrangler", [
          "deploy",
          "--config", config.path,
          "--strict",
          "--tag", release.tag,
          "--message", releaseMessage(release),
        ], { signal });
        await config.assertUnchanged();
        await run(process.execPath, [
          join(root, "scripts", "assert-custom-domain-dark.mjs"),
          "--config", config.path,
        ], { signal });
        await run(process.execPath, [
          join(root, "scripts", "deployment-identity.mjs"),
          "--require-annotations",
        ], { signal });
        // Re-read the active control plane while the same global lease is still
        // held. A concurrent control-plane or provider-route operation cannot
        // invalidate this proof between preflight and deployment.
        preflightManagedCohortDeploymentOrder(
          release.version,
          targetAllowlist,
        );
        await config.assertUnchanged();
      },
      { endpoint: leaseOrigin },
    );
  } finally {
    await config.cleanup();
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
