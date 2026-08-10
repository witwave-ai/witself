#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  deploymentMatches,
  expectedBuildMetadata,
} from "./verify-deployment.mjs";
import {
  sourceIdentity,
  workerVersionMessage,
  workerVersionTag,
} from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
export const GENERATED_CONFIG_PATH = join(root, "wrangler.generated.jsonc");
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const MANAGED_COHORT_PROTOCOL_RELEASE = "0.0.241";
const MANAGED_COHORT_PREDECESSOR_RELEASE = "0.0.240";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function activeVersionID(deployment) {
  if (!deployment || typeof deployment !== "object" || Array.isArray(deployment) ||
      !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error("email edge deployment was not one version at 100 percent");
  }
  return deployment.versions[0].version_id;
}

function bindingMap(version, expectedVersionID) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedVersionID ||
      !Array.isArray(version.resources?.bindings) ||
      JSON.stringify(version.resources?.script?.handlers) !== JSON.stringify(["email"])) {
    throw new Error("active email edge version was not the expected email-only Worker");
  }
  const bindings = new Map();
  for (const binding of version.resources.bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || binding.name === "" ||
        bindings.has(binding.name)) {
      throw new Error("active email edge binding inventory was invalid");
    }
    bindings.set(binding.name, binding);
  }
  return bindings;
}

function plain(bindings, name) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    throw new Error(`active email edge was missing ${name}`);
  }
  return binding.text;
}

// v0.0.241 is the one managed-route wire transition whose new control plane is
// intentionally incompatible with a v0.0.240 edge. A CP-first deployment is
// safe only when the old edge cannot consume a fresh legacy signed-v2 KV row.
// This preflight runs before Wrangler mutates the control plane.
export function verifyManagedCohortProtocolUpgrade(
  targetRelease,
  deployment,
  version,
) {
  if (targetRelease !== MANAGED_COHORT_PROTOCOL_RELEASE) {
    return Object.freeze({ required: false });
  }
  const versionID = activeVersionID(deployment);
  const bindings = bindingMap(version, versionID);
  const edgeRelease = plain(bindings, "WITSELF_EDGE_RELEASE_VERSION");
  if (edgeRelease === MANAGED_COHORT_PROTOCOL_RELEASE) {
    return Object.freeze({ required: true, edge_release: edgeRelease, already_current: true });
  }
  if (edgeRelease !== MANAGED_COHORT_PREDECESSOR_RELEASE) {
    throw new Error(
      "v0.0.241 control-plane deployment requires a v0.0.240 or v0.0.241 email edge",
    );
  }
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    if (plain(bindings, name) !== "false") {
      throw new Error(
        "v0.0.241 CP-first deployment requires the active v0.0.240 email edge managed delivery gates to be false",
      );
    }
  }
  return Object.freeze({ required: true, edge_release: edgeRelease, already_current: false });
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
    throw new Error(`Wrangler ${operation} output was not valid JSON`);
  }
}

export function preflightManagedCohortProtocolUpgrade(
  targetRelease,
  inspect = wranglerJSON,
) {
  if (targetRelease !== MANAGED_COHORT_PROTOCOL_RELEASE) {
    return Object.freeze({ required: false });
  }
  const deployment = inspect([
    "deployments", "status", "--name", EMAIL_EDGE_WORKER, "--json",
  ], "inspect the active email edge deployment");
  const versionID = activeVersionID(deployment);
  const version = inspect([
    "versions", "view", versionID, "--name", EMAIL_EDGE_WORKER, "--json",
  ], "inspect the active email edge version");
  return verifyManagedCohortProtocolUpgrade(
    targetRelease,
    deployment,
    version,
  );
}

function parseArgs(argv) {
  if (argv.length === 0) return { config: GENERATED_CONFIG_PATH };
  if (argv.length !== 2 || argv[0] !== "--config") {
    throw new Error(`unknown or incomplete argument ${argv[0] ?? ""}`.trim());
  }
  return { config: exactGeneratedConfigPath(argv[1]) };
}

export function exactGeneratedConfigPath(config = GENERATED_CONFIG_PATH) {
  const candidate = resolve(root, config);
  if (candidate !== GENERATED_CONFIG_PATH) {
    throw new Error("release deployment requires the exact generated control-plane config");
  }
  return GENERATED_CONFIG_PATH;
}

export function releaseDeploymentArguments(
  metadata,
  config = GENERATED_CONFIG_PATH,
) {
  const exactConfig = exactGeneratedConfigPath(config);
  return [
    "deploy",
    "--config", exactConfig,
    "--strict",
    "--tag", workerVersionTag(metadata),
    "--message", workerVersionMessage(metadata),
  ];
}

export async function main(argv = process.argv.slice(2)) {
  const { config } = parseArgs(argv);
  const expected = expectedBuildMetadata(await readFile(config, "utf8"));
  const source = sourceIdentity();
  const actual = { service: "witself-control-plane", ...source };
  if (!deploymentMatches(actual, expected)) {
    throw new Error(
      "generated control-plane config does not match the clean tagged release source",
    );
  }

  preflightManagedCohortProtocolUpgrade(expected.version);

  const deployed = spawnSync(
    "wrangler",
    releaseDeploymentArguments(source, config),
    { cwd: root, stdio: "inherit" },
  );
  if (deployed.error || deployed.status !== 0) {
    throw new Error("control-plane Worker deployment failed");
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
