#!/usr/bin/env node
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import {
  chmod,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import {
  dirname,
  isAbsolute,
  join,
  resolve,
} from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

import {
  expectedDeployment,
  releaseMessage,
} from "./deployment-identity.mjs";
import {
  sanitizedGitEnvironment,
  sourceIdentity,
} from "./source-identity.mjs";
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
import {
  parseJSONC,
  secretInventory,
} from "./provision-route-signing-secrets.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerEnvironment,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";
import { CUSTOM_DOMAIN_DELIVERY_SECRET } from
  "./assert-custom-domain-dark.mjs";
import { PRODUCTION_RECEIVE_WORKER } from "../src/worker-names.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const CANONICAL_CONTROL_PLANE_ORIGIN = "https://self.witwave.ai";
const MANAGED_COHORT_PROTOCOL_RELEASE = "0.0.241";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SOURCE_PATH_PREFIX = "infra/cloudflare/agent-email/src/";
const MAX_SOURCE_FILES = 64;
const MAX_SOURCE_BYTES = 5 * 1024 * 1024;
const PRIVATE_DIRECTORY_MODE = 0o700;
const IMMUTABLE_FILE_MODE = 0o400;
const PRODUCTION_SECRET_NAMES = Object.freeze([
  "CONTROL_PLANE_EDGE_TOKEN",
  "RELAY_ED25519_PRIVATE_KEY",
]);
const FORBIDDEN_PRODUCTION_SECRET_NAMES = Object.freeze([
  CUSTOM_DOMAIN_DELIVERY_SECRET,
  "LEGACY_PILOT_TRUSTED_INGEST_URL",
  "LEGACY_PILOT_TRUSTED_CELL_AUDIENCE",
]);

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

export function runGitInspection(args, {
  checkout = resolve(root, "../../.."),
  environment = process.env,
} = {}) {
  return spawnSync("git", args, {
    cwd: checkout,
    env: sanitizedGitEnvironment(environment),
    encoding: null,
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: MAX_SOURCE_BYTES * 2,
    timeout: 30_000,
  });
}

function gitOutput(args, label, inspect) {
  const result = inspect(args);
  if (result?.error || result?.status !== 0 || !Buffer.isBuffer(result?.stdout)) {
    throw new Error(`could not freeze ${label} from the tagged release`);
  }
  return result.stdout;
}

export async function snapshotProductionWorkerSource(
  destinationRoot,
  commit,
  inspect = runGitInspection,
) {
  if (typeof destinationRoot !== "string" || !isAbsolute(destinationRoot) ||
      resolve(destinationRoot) !== destinationRoot ||
      !/^[0-9a-f]{40}$/.test(String(commit ?? "")) ||
      typeof inspect !== "function") {
    throw new Error("production email Worker source snapshot request was invalid");
  }
  const directoryMetadata = await lstat(destinationRoot);
  if (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
      (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE) {
    throw new Error("production email Worker source snapshot directory was unsafe");
  }
  const inventory = gitOutput([
    "ls-tree", "-rz", "--full-tree", commit, "--", SOURCE_PATH_PREFIX,
  ], "Worker source inventory", inspect).toString("utf8");
  const records = inventory.split("\0").filter(Boolean);
  if (records.length < 1 || records.length > MAX_SOURCE_FILES) {
    throw new Error("tagged production email Worker source inventory was invalid");
  }
  const parsed = records.map((record) => {
    const match = /^(100644|100755) blob ([0-9a-f]{40,64})\t(.+)$/.exec(record);
    if (!match || !match[3].startsWith(SOURCE_PATH_PREFIX)) {
      throw new Error("tagged production email Worker source inventory was invalid");
    }
    const path = match[3].slice(SOURCE_PATH_PREFIX.length);
    if (path.includes("/") ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(path) ||
        !/\.(?:js|mjs)$/.test(path)) {
      throw new Error("tagged production email Worker source path was invalid");
    }
    return Object.freeze({ oid: match[2], path });
  }).sort((left, right) => left.path.localeCompare(right.path));
  if (new Set(parsed.map((item) => item.path)).size !== parsed.length ||
      !parsed.some((item) => item.path === "index.js")) {
    throw new Error("tagged production email Worker source inventory was invalid");
  }

  const targetSource = join(destinationRoot, "src");
  await mkdir(targetSource, { mode: PRIVATE_DIRECTORY_MODE });
  let totalBytes = 0;
  const manifest = [];
  for (const item of parsed) {
    const bytes = gitOutput(
      ["cat-file", "blob", item.oid],
      `Worker source ${item.path}`,
      inspect,
    );
    totalBytes += bytes.byteLength;
    if (bytes.byteLength < 1 || totalBytes > MAX_SOURCE_BYTES) {
      throw new Error("tagged production email Worker source exceeded its size limit");
    }
    const target = join(targetSource, item.path);
    await writeFile(target, bytes, { flag: "wx", mode: 0o600 });
    await chmod(target, IMMUTABLE_FILE_MODE);
    manifest.push(Object.freeze({
      path: item.path,
      bytes: bytes.byteLength,
      sha256: sha256(bytes),
    }));
  }
  const evidence = Object.freeze({
    parentDirectory: destinationRoot,
    entrypointTarget: join(targetSource, "index.js"),
    file_count: manifest.length,
    byte_count: totalBytes,
    sha256: sha256(JSON.stringify(manifest)),
    async assertUnchanged() {
      const [rootMetadata, sourceMetadata, names] = await Promise.all([
        lstat(destinationRoot),
        lstat(targetSource),
        readdir(targetSource),
      ]);
      if (!rootMetadata.isDirectory() || rootMetadata.isSymbolicLink() ||
          (rootMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
          !sourceMetadata.isDirectory() || sourceMetadata.isSymbolicLink() ||
          (sourceMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
          JSON.stringify(names.sort()) !==
            JSON.stringify(manifest.map((item) => item.path).sort())) {
        throw new Error("production email Worker source snapshot changed during deployment");
      }
      for (const item of manifest) {
        const path = join(targetSource, item.path);
        const [fileMetadata, bytes] = await Promise.all([
          lstat(path),
          readFile(path),
        ]);
        if (!fileMetadata.isFile() || fileMetadata.isSymbolicLink() ||
            (fileMetadata.mode & 0o777) !== IMMUTABLE_FILE_MODE ||
            bytes.byteLength !== item.bytes || sha256(bytes) !== item.sha256) {
          throw new Error("production email Worker source snapshot changed during deployment");
        }
      }
    },
  });
  await evidence.assertUnchanged();
  return evidence;
}

export async function createProductionWorkerSourceSnapshot(
  commit,
  inspect = runGitInspection,
) {
  const directory = await mkdtemp(join(
    tmpdir(),
    "witself-agent-email-deploy-source-",
  ));
  let cleaned = false;
  try {
    await chmod(directory, PRIVATE_DIRECTORY_MODE);
    const evidence = await snapshotProductionWorkerSource(
      directory,
      commit,
      inspect,
    );
    return Object.freeze({
      ...evidence,
      async assertUnchanged() {
        if (cleaned) {
          throw new Error("production email Worker source snapshot was already cleaned up");
        }
        await evidence.assertUnchanged();
      },
      async cleanup() {
        if (cleaned) return;
        await rm(directory, { recursive: true, force: true });
        cleaned = true;
      },
    });
  } catch (error) {
    cleaned = true;
    await rm(directory, { recursive: true, force: true }).catch(() => {});
    throw error;
  }
}

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

function wranglerJSON(args, operation, environment = process.env) {
  const result = spawnSync("wrangler", args, {
    cwd: root,
    env: sanitizedWranglerInspectionEnvironment(environment),
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
  const deployment = inspect(withReviewedWranglerEnvironmentFile([
    "deployments", "status", "--name", CONTROL_PLANE_WORKER, "--json",
  ]), "inspect the active control-plane deployment");
  const versionID = activeVersionID(deployment);
  const version = inspect(withReviewedWranglerEnvironmentFile([
    "versions", "view", versionID, "--name", CONTROL_PLANE_WORKER, "--json",
  ]), "inspect the active control-plane version");
  const finalDeployment = inspect(withReviewedWranglerEnvironmentFile([
    "deployments", "status", "--name", CONTROL_PLANE_WORKER, "--json",
  ]), "reinspect the active control-plane deployment");
  if (finalDeployment?.id !== deployment.id ||
      activeVersionID(finalDeployment) !== versionID) {
    throw new Error(
      "control-plane deployment changed during exact provider inspection",
    );
  }
  return verifyManagedCohortDeploymentOrder(
    targetRelease, targetAllowlist, deployment, version,
  );
}

export function verifyProductionSecretInventory(secrets) {
  const names = secretInventory(secrets, "production email Worker");
  const forbidden = FORBIDDEN_PRODUCTION_SECRET_NAMES.find((name) =>
    names.has(name));
  if (forbidden != null) {
    throw new Error(
      `production email Worker secret inventory contained forbidden ${forbidden}`,
    );
  }
  const actual = [...names].sort();
  const expected = [...PRODUCTION_SECRET_NAMES].sort();
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(
      "production email Worker secret inventory did not match the required production contract",
    );
  }
  return Object.freeze({
    worker: PRODUCTION_RECEIVE_WORKER,
    secret_names: Object.freeze([...expected]),
  });
}

export function preflightProductionSecretInventory(inspect = wranglerJSON) {
  const secrets = inspect(withReviewedWranglerEnvironmentFile([
    "secret", "list", "--name", PRODUCTION_RECEIVE_WORKER,
    "--format", "json",
  ]), "inspect the production email Worker secret inventory");
  return verifyProductionSecretInventory(secrets);
}

function run(command, args, {
  env,
  signal,
  timeoutMs = 5 * 60_000,
} = {}) {
  return runLeaseGuardedCommand(command, args, {
    cwd: root,
    env,
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

function exactObjectKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      JSON.stringify(Object.keys(value).sort()) !==
        JSON.stringify([...keys].sort())) {
    throw new Error(`production email Worker ${label} was invalid`);
  }
}

export function validateProductionDeploymentConfig(
  raw,
  configPath,
  entrypointTarget,
  release,
  environment,
) {
  if (typeof configPath !== "string" || !isAbsolute(configPath) ||
      resolve(configPath) !== configPath ||
      typeof entrypointTarget !== "string" || !isAbsolute(entrypointTarget) ||
      resolve(entrypointTarget) !== entrypointTarget) {
    throw new Error("production email Worker frozen paths were invalid");
  }
  const config = parseJSONC(raw, "production email Worker configuration");
  const expected = expectedDeployment(environment, release);
  exactObjectKeys(config, [
    "name",
    "main",
    "secrets",
    "compatibility_date",
    "compatibility_flags",
    "workers_dev",
    "preview_urls",
    "kv_namespaces",
    "analytics_engine_datasets",
    "ratelimits",
    "vars",
    "observability",
  ], "configuration contract");
  exactObjectKeys(config.secrets, ["required"], "secret declaration");
  exactObjectKeys(config.vars, [
    "AGENT_EMAIL_DOMAIN",
    "AGENT_EMAIL_LEGACY_DOMAINS",
    "RELAY_KEY_ID",
    "CONTROL_PLANE_URL",
    "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS",
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "WITSELF_EDGE_RELEASE_VERSION",
    "WITSELF_EDGE_RELEASE_COMMIT",
    "WITSELF_EDGE_RELEASE_DATE",
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ], "plain binding contract");
  const expectedVars = {
    AGENT_EMAIL_DOMAIN: "witmail.net",
    AGENT_EMAIL_LEGACY_DOMAINS: "agent-mail.witwave.ai",
    RELAY_KEY_ID: expected.relayKeyID,
    CONTROL_PLANE_URL: expected.controlPlaneURL,
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: expected.routePublicKeys,
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST:
      expected.managedDeliveryAccountAllowlist,
    WITSELF_EDGE_RELEASE_VERSION: release.version,
    WITSELF_EDGE_RELEASE_COMMIT: release.commit,
    WITSELF_EDGE_RELEASE_DATE: release.date,
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: expected.aliasDeliveryEnabled,
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: expected.canonicalDeliveryEnabled,
  };
  const expectedRateLimits = [
    {
      name: "REALM_ROUTE_COLD_MISS_LIMITER",
      namespace_id: "2201",
      simple: { limit: 10, period: 10 },
    },
    {
      name: "REALM_ROUTE_KNOWN_MISS_LIMITER",
      namespace_id: "2202",
      simple: { limit: 100, period: 10 },
    },
  ];
  if (typeof config.main !== "string" || isAbsolute(config.main) ||
      !/^(?:\.\.\/)*[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._-]+)*$/.test(config.main) ||
      resolve(dirname(configPath), config.main) !== entrypointTarget ||
      config.name !== PRODUCTION_RECEIVE_WORKER ||
      config.workers_dev !== false || config.preview_urls !== false ||
      config.compatibility_date !== "2026-07-21" ||
      JSON.stringify(config.compatibility_flags) !==
        JSON.stringify(["global_fetch_strictly_public"]) ||
      JSON.stringify(config.secrets.required) !==
        JSON.stringify(PRODUCTION_SECRET_NAMES) ||
      JSON.stringify(config.vars) !== JSON.stringify(expectedVars) ||
      JSON.stringify(config.kv_namespaces) !== JSON.stringify([{
        binding: "EMAIL_DIRECTORY",
        id: expected.directoryID,
      }]) ||
      JSON.stringify(config.analytics_engine_datasets) !== JSON.stringify([{
        binding: "EMAIL_EDGE_METRICS",
        dataset: "witself_agent_email_edge",
      }]) ||
      JSON.stringify(config.ratelimits) !== JSON.stringify(expectedRateLimits) ||
      JSON.stringify(config.observability) !== JSON.stringify({ enabled: true })) {
    throw new Error(
      "production email Worker configuration did not match the reviewed release contract",
    );
  }
  return Object.freeze({
    worker: config.name,
    entrypoint: entrypointTarget,
    release: Object.freeze({
      version: release.version,
      commit: release.commit,
      date: release.date,
    }),
    directory_namespace_id: expected.directoryID,
  });
}

export function productionDeploymentEnvironments(
  environment = process.env,
  leaseOrigin = canonicalControlPlaneOrigin(environment.CONTROL_PLANE_URL),
) {
  const wranglerMutation = sanitizedWranglerEnvironment(environment);
  const wranglerInspection = sanitizedWranglerInspectionEnvironment(environment);
  const controlPlaneBindingURL = `${leaseOrigin}/`;
  return Object.freeze({
    wranglerMutation: Object.freeze(wranglerMutation),
    wranglerInspection: Object.freeze(wranglerInspection),
    nestedRender: Object.freeze({
      ...wranglerMutation,
      CONTROL_PLANE_URL: controlPlaneBindingURL,
    }),
    nestedInspection: Object.freeze({ ...wranglerInspection }),
    nestedAttestation: Object.freeze({
      ...wranglerInspection,
      CONTROL_PLANE_URL: controlPlaneBindingURL,
    }),
  });
}

export function assertProductionReleaseUnchanged(expected, current) {
  const fields = ["version", "commit", "date", "tag", "clean"];
  if (expected?.clean !== true ||
      fields.some((field) => current?.[field] !== expected?.[field])) {
    throw new Error("production email Worker release source changed during deployment");
  }
  return current;
}

export function productionDeploymentArguments(configPath, release) {
  if (typeof configPath !== "string" || configPath === "" ||
      !release || typeof release !== "object") {
    throw new Error("production email Worker deployment arguments were invalid");
  }
  return withReviewedWranglerEnvironmentFile([
    "deploy",
    "--name", PRODUCTION_RECEIVE_WORKER,
    "--config", configPath,
    "--strict",
    "--tag", release.tag,
    "--message", releaseMessage(release),
  ]);
}

function requiredFunction(value, label) {
  if (typeof value !== "function") {
    throw new Error(`production email Worker ${label} was invalid`);
  }
  return value;
}

async function cleanupProductionDeploymentInputs(config, sourceSnapshot) {
  const cleanups = [];
  if (typeof config?.cleanup === "function") {
    cleanups.push(Promise.resolve().then(() => config.cleanup()));
  }
  if (typeof sourceSnapshot?.cleanup === "function") {
    cleanups.push(Promise.resolve().then(() => sourceSnapshot.cleanup()));
  }
  const results = await Promise.allSettled(cleanups);
  const failures = results
    .filter((result) => result.status === "rejected")
    .map((result) => result.reason);
  if (failures.length === 1) {
    throw new Error("production email Worker private input cleanup failed", {
      cause: failures[0],
    });
  }
  if (failures.length > 1) {
    throw new AggregateError(
      failures,
      "production email Worker private input cleanup failed",
    );
  }
}

export async function deployProductionReceive(dependencies = {}) {
  const environment = dependencies.environment ?? process.env;
  assertProductionCloudflareIdentity(environment);
  const identifySource = requiredFunction(
    dependencies.sourceIdentity ?? sourceIdentity,
    "source identity inspector",
  );
  const runCommand = requiredFunction(
    dependencies.runCommand ?? run,
    "command runner",
  );
  const withLease = requiredFunction(
    dependencies.withLease ?? withAgentEmailOperationsLease,
    "operations lease client",
  );
  const release = identifySource({ requireRelease: true });
  const leaseOrigin = canonicalControlPlaneOrigin(environment.CONTROL_PLANE_URL);
  const commandEnvironments = productionDeploymentEnvironments(
    environment,
    leaseOrigin,
  );
  const targetAllowlist = String(
    environment.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST ?? "",
  );
  const inspectWrangler = requiredFunction(
    dependencies.inspect ?? wranglerJSON,
    "Wrangler inspector",
  );
  const inspect = (args, operation) => inspectWrangler(
    args,
    operation,
    commandEnvironments.wranglerInspection,
  );
  const preflightCohort = requiredFunction(
    dependencies.preflightCohort ?? ((releaseVersion, allowlist) =>
      preflightManagedCohortDeploymentOrder(
        releaseVersion,
        allowlist,
        inspect,
      )),
    "managed cohort preflight",
  );
  const preflightSecrets = requiredFunction(
    dependencies.preflightSecrets ?? (() =>
      preflightProductionSecretInventory(inspect)),
    "secret inventory preflight",
  );
  const createSourceSnapshot = requiredFunction(
    dependencies.createSourceSnapshot ?? createProductionWorkerSourceSnapshot,
    "source snapshot factory",
  );
  const validateConfig = requiredFunction(
    dependencies.validateConfig ?? validateProductionDeploymentConfig,
    "configuration validator",
  );
  let sourceSnapshot;
  let config;
  let inputsCleaned = false;
  try {
    sourceSnapshot = await createSourceSnapshot(release.commit);
    if (!sourceSnapshot ||
        typeof sourceSnapshot.entrypointTarget !== "string" ||
        !isAbsolute(sourceSnapshot.entrypointTarget) ||
        resolve(sourceSnapshot.entrypointTarget) !==
          sourceSnapshot.entrypointTarget ||
        typeof sourceSnapshot.parentDirectory !== "string" ||
        !isAbsolute(sourceSnapshot.parentDirectory) ||
        resolve(sourceSnapshot.parentDirectory) !==
          sourceSnapshot.parentDirectory ||
        typeof sourceSnapshot.assertUnchanged !== "function" ||
        typeof sourceSnapshot.cleanup !== "function" ||
        !Number.isSafeInteger(sourceSnapshot.file_count) ||
        sourceSnapshot.file_count < 1 ||
        !Number.isSafeInteger(sourceSnapshot.byte_count) ||
        sourceSnapshot.byte_count < 1 ||
        !/^[0-9a-f]{64}$/.test(String(sourceSnapshot.sha256 ?? ""))) {
      throw new Error("production email Worker source snapshot was invalid");
    }
    const createConfig = requiredFunction(
      dependencies.createConfig ?? (() => createPrivateDeploymentConfig({
        prefix: "witself-agent-email-deploy-",
        parentDirectory: sourceSnapshot.parentDirectory,
        entrypointTarget: sourceSnapshot.entrypointTarget,
        render: (path) => runCommand(process.execPath, [
          join(root, "scripts", "render-wrangler.mjs"),
          "--output", path,
        ], { env: commandEnvironments.nestedRender }),
        validate: (path) => runCommand(process.execPath, [
          join(root, "scripts", "assert-custom-domain-dark.mjs"),
          "--config", path,
        ], { env: commandEnvironments.nestedInspection }),
      })),
      "private configuration factory",
    );
    config = await createConfig(sourceSnapshot);
    if (!config || typeof config.path !== "string" ||
        !isAbsolute(config.path) || resolve(config.path) !== config.path ||
        typeof config.assertUnchanged !== "function" ||
        typeof config.readText !== "function" ||
        typeof config.cleanup !== "function" ||
        !/^[0-9a-f]{64}$/.test(String(config.sha256 ?? ""))) {
      throw new Error("production email Worker private configuration was invalid");
    }
    const validationEnvironment = Object.freeze({
      ...environment,
      CONTROL_PLANE_URL: `${leaseOrigin}/`,
    });
    const assertFrozenInputs = async () => {
      await Promise.all([
        config.assertUnchanged(),
        sourceSnapshot.assertUnchanged(),
      ]);
      validateConfig(
        await config.readText(),
        config.path,
        sourceSnapshot.entrypointTarget,
        release,
        validationEnvironment,
      );
    };
    await assertFrozenInputs();
    await withLease(
      "email_edge_deploy",
      async ({ signal }) => {
        await assertFrozenInputs();
        assertProductionReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        preflightSecrets();
        preflightCohort(release.version, targetAllowlist);
        await runCommand(process.execPath, [
          join(root, "scripts", "assert-custom-domain-dark.mjs"),
          "--config", config.path,
        ], {
          env: commandEnvironments.nestedInspection,
          signal,
        });
        await assertFrozenInputs();
        assertProductionReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        await runCommand(
          "wrangler",
          productionDeploymentArguments(config.path, release),
          {
            env: commandEnvironments.wranglerMutation,
            signal,
          },
        );
        assertProductionReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        await assertFrozenInputs();
        await runCommand(process.execPath, [
          join(root, "scripts", "assert-custom-domain-dark.mjs"),
          "--config", config.path,
        ], {
          env: commandEnvironments.nestedInspection,
          signal,
        });
        preflightSecrets();
        await runCommand(process.execPath, [
          join(root, "scripts", "deployment-identity.mjs"),
          "--require-annotations",
        ], {
          env: commandEnvironments.nestedAttestation,
          signal,
        });
        // Re-read the active control plane while the same global lease is still
        // held. A concurrent control-plane or provider-route operation cannot
        // invalidate this proof between preflight and deployment.
        preflightCohort(release.version, targetAllowlist);
        assertProductionReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        await assertFrozenInputs();
      },
      { endpoint: leaseOrigin, env: environment },
    );
    await cleanupProductionDeploymentInputs(config, sourceSnapshot);
    inputsCleaned = true;
  } catch (error) {
    let cleanupError = null;
    if (!inputsCleaned) {
      try {
        await cleanupProductionDeploymentInputs(config, sourceSnapshot);
      } catch (current) {
        cleanupError = current;
      }
    }
    if (cleanupError != null) {
      throw new AggregateError(
        [error, cleanupError],
        "production email Worker deployment and private input cleanup failed",
      );
    }
    throw error;
  }
}

export async function main() {
  await deployProductionReceive();
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
