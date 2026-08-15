#!/usr/bin/env node
import {
  createHash,
  createPrivateKey,
  createPublicKey,
  timingSafeEqual,
} from "node:crypto";
import {
  chmod,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  realpath,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import {
  basename,
  dirname,
  isAbsolute,
  join,
  relative,
  resolve,
  sep,
} from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

import {
  expectedDeployment,
  releaseMessage,
  verifyProduction,
} from "./deployment-identity.mjs";
import {
  canonicalControlPlaneOrigin,
  preflightManagedCohortDeploymentOrder,
} from "./deploy.mjs";
import {
  sanitizedGitEnvironment,
  sourceIdentity,
} from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";
import {
  LEGACY_PILOT_WORKER,
  PRODUCTION_RECEIVE_WORKER,
} from "../src/worker-names.mjs";
import {
  CloudflareAPI,
  cloudflareEnvironment,
} from "./cloudflare.mjs";
import {
  captureRelayProviderDarkState,
  validateRelayProviderDarkState,
} from "./provision-relay-signing-key.mjs";
import {
  activeBindings,
  activeVersionID,
  parseJSONC,
  secretInventory,
  validateFallbackToken,
} from "./provision-route-signing-secrets.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerEnvironment,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";
import { reserveJSONReceipt } from "./receipt-journal.mjs";
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
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const repositoryRoot = resolve(root, "../../..");
const SECRET_NAMES = Object.freeze([
  "CONTROL_PLANE_EDGE_TOKEN",
  "RELAY_ED25519_PRIVATE_KEY",
]);
const MAX_SECRETS_BYTES = 20 * 1024;
const PRIVATE_DIRECTORY_MODE = 0o700;
const IMMUTABLE_FILE_MODE = 0o400;
const ABSENT_WORKER_CODE = "10007";
const OPERATIONS_LEASE_OPERATION = "email_edge_deploy";
const PRIMARY_PROVIDER_ZONE_CONTRACT = Object.freeze({
  "witmail.net": "primary",
});
const LEGACY_PROVIDER_ZONE_CONTRACT = Object.freeze({
  "witwave.ai": "legacy",
});
const CUSTOM_DOMAIN_DELIVERY_SECRET =
  "AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED";
const LEGACY_PILOT_TRUST_BINDINGS = Object.freeze([
  "LEGACY_PILOT_TRUSTED_INGEST_URL",
  "LEGACY_PILOT_TRUSTED_CELL_AUDIENCE",
]);
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");
const SOURCE_PATH_PREFIX = "infra/cloudflare/agent-email/src/";
const MAX_SOURCE_FILES = 64;
const MAX_SOURCE_BYTES = 5 * 1024 * 1024;

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

export function parseBootstrapArgs(argv) {
  if (argv.length !== 4) {
    throw new Error(
      "usage: bootstrap-production-receive.mjs --secrets-file /absolute/private/secrets.json --receipt /absolute/private/receipt.json",
    );
  }
  const values = new Map();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!["--secrets-file", "--receipt"].includes(name) || values.has(name) ||
        typeof value !== "string" || !isAbsolute(value) ||
        resolve(value) !== value) {
      throw new Error(
        "usage: bootstrap-production-receive.mjs --secrets-file /absolute/private/secrets.json --receipt /absolute/private/receipt.json",
      );
    }
    values.set(name, value);
  }
  if (values.size !== 2) {
    throw new Error(
      "usage: bootstrap-production-receive.mjs --secrets-file /absolute/private/secrets.json --receipt /absolute/private/receipt.json",
    );
  }
  return Object.freeze({
    secretsFile: values.get("--secrets-file"),
    receipt: values.get("--receipt"),
  });
}

function insideDirectory(parent, candidate) {
  const path = relative(parent, candidate);
  return path === "" || (!isAbsolute(path) && path !== ".." &&
    !path.startsWith(`..${sep}`));
}

export async function assertBootstrapPathsOutsideRepository(
  options,
  checkout = repositoryRoot,
) {
  if (typeof checkout !== "string" || !isAbsolute(checkout) ||
      resolve(checkout) !== checkout) {
    throw new Error("production receive bootstrap repository root was invalid");
  }
  for (const [name, path] of [
    ["secrets file", options?.secretsFile],
    ["receipt", options?.receipt],
  ]) {
    if (typeof path !== "string" || !isAbsolute(path) ||
        resolve(path) !== path || insideDirectory(checkout, path)) {
      throw new Error(
        `production receive bootstrap ${name} must be outside the repository`,
      );
    }
  }

  let physicalCheckout;
  let physicalSecretsFile;
  let physicalReceiptParent;
  try {
    [physicalCheckout, physicalSecretsFile, physicalReceiptParent] =
      await Promise.all([
        realpath(checkout),
        realpath(options.secretsFile),
        realpath(dirname(options.receipt)),
      ]);
  } catch {
    throw new Error(
      "production receive bootstrap paths could not be resolved physically",
    );
  }
  const checkoutMetadata = await lstat(physicalCheckout);
  if (!checkoutMetadata.isDirectory() || checkoutMetadata.isSymbolicLink()) {
    throw new Error("production receive bootstrap repository root was invalid");
  }
  const receiptName = basename(options.receipt);
  if (receiptName === "" || receiptName === "." || receiptName === "..") {
    throw new Error(
      "production receive bootstrap receipt must be outside the repository",
    );
  }
  let receiptExists = true;
  try {
    await lstat(options.receipt);
  } catch (error) {
    if (error?.code !== "ENOENT") {
      throw new Error(
        "production receive bootstrap receipt path could not be inspected",
      );
    }
    receiptExists = false;
  }
  if (receiptExists) {
    throw new Error("production receive bootstrap receipt must be new");
  }
  const physicalReceipt = join(physicalReceiptParent, receiptName);
  for (const [name, path] of [
    ["secrets file", physicalSecretsFile],
    ["receipt", physicalReceipt],
  ]) {
    if (insideDirectory(physicalCheckout, path)) {
      throw new Error(
        `production receive bootstrap ${name} must be outside the repository`,
      );
    }
  }
  return Object.freeze({
    repositoryRoot: physicalCheckout,
    secretsFile: physicalSecretsFile,
    receipt: physicalReceipt,
  });
}

export function runGitInspection(args, {
  checkout = repositoryRoot,
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

export async function snapshotReleaseWorkerSource(
  destinationRoot,
  commit,
  inspect = runGitInspection,
) {
  if (typeof destinationRoot !== "string" || !isAbsolute(destinationRoot) ||
      resolve(destinationRoot) !== destinationRoot ||
      !/^[0-9a-f]{40}$/.test(String(commit ?? "")) ||
      typeof inspect !== "function") {
    throw new Error("production receive bootstrap source snapshot request was invalid");
  }
  const directoryMetadata = await lstat(destinationRoot);
  if (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
      (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE) {
    throw new Error("production receive bootstrap source snapshot directory was unsafe");
  }
  const inventory = gitOutput([
    "ls-tree", "-rz", "--full-tree", commit, "--", SOURCE_PATH_PREFIX,
  ], "Worker source inventory", inspect).toString("utf8");
  const records = inventory.split("\0").filter(Boolean);
  if (records.length < 1 || records.length > MAX_SOURCE_FILES) {
    throw new Error("tagged production receive Worker source inventory was invalid");
  }
  const parsed = records.map((record) => {
    const match = /^(100644|100755) blob ([0-9a-f]{40,64})\t(.+)$/.exec(record);
    if (!match || !match[3].startsWith(SOURCE_PATH_PREFIX)) {
      throw new Error("tagged production receive Worker source inventory was invalid");
    }
    const path = match[3].slice(SOURCE_PATH_PREFIX.length);
    if (path.includes("/") ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(path) ||
        !/\.(?:js|mjs)$/.test(path)) {
      throw new Error("tagged production receive Worker source path was invalid");
    }
    return Object.freeze({ oid: match[2], path });
  }).sort((left, right) => left.path.localeCompare(right.path));
  if (new Set(parsed.map((item) => item.path)).size !== parsed.length ||
      !parsed.some((item) => item.path === "index.js")) {
    throw new Error("tagged production receive Worker source inventory was invalid");
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
      throw new Error("tagged production receive Worker source exceeded its size limit");
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
    file_count: manifest.length,
    byte_count: totalBytes,
    sha256: sha256(JSON.stringify(manifest)),
    async assertUnchanged() {
      const [metadata, names] = await Promise.all([
        lstat(targetSource),
        readdir(targetSource),
      ]);
      if (!metadata.isDirectory() || metadata.isSymbolicLink() ||
          (metadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
          JSON.stringify(names.sort()) !==
            JSON.stringify(manifest.map((item) => item.path).sort())) {
        throw new Error(
          "production receive bootstrap source snapshot changed during deployment",
        );
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
          throw new Error(
            "production receive bootstrap source snapshot changed during deployment",
          );
        }
      }
    },
  });
  await evidence.assertUnchanged();
  return evidence;
}

export async function createPrivateReleaseWorkerSource(
  commit,
  inspect = runGitInspection,
) {
  if (!/^[0-9a-f]{40}$/.test(String(commit ?? "")) ||
      typeof inspect !== "function") {
    throw new Error(
      "production receive bootstrap source snapshot request was invalid",
    );
  }
  const parentDirectory = await mkdtemp(join(
    tmpdir(),
    "witself-agent-email-receive-bootstrap-source-",
  ));
  let cleaned = false;
  try {
    await chmod(parentDirectory, PRIVATE_DIRECTORY_MODE);
    const evidence = await snapshotReleaseWorkerSource(
      parentDirectory,
      commit,
      inspect,
    );
    const entrypointTarget = join(parentDirectory, "src", "index.js");
    return Object.freeze({
      ...evidence,
      parentDirectory,
      entrypointTarget,
      async assertUnchanged() {
        if (cleaned) {
          throw new Error(
            "production receive bootstrap source snapshot was already cleaned up",
          );
        }
        await evidence.assertUnchanged();
      },
      async cleanup() {
        if (cleaned) return;
        await rm(parentDirectory, { recursive: true, force: true });
        cleaned = true;
      },
    });
  } catch (error) {
    cleaned = true;
    let cleanupError;
    try {
      await rm(parentDirectory, { recursive: true, force: true });
    } catch (failure) {
      cleanupError = failure;
    }
    if (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        "production receive source snapshot failed and cleanup was incomplete",
      );
    }
    throw error;
  }
}

export function createBootstrapDeploymentConfig(
  sourceSnapshot,
  renderEnvironment,
  runCommand = run,
) {
  if (!sourceSnapshot ||
      typeof sourceSnapshot.parentDirectory !== "string" ||
      typeof sourceSnapshot.entrypointTarget !== "string" ||
      typeof runCommand !== "function") {
    throw new Error(
      "production receive bootstrap source relocation was invalid",
    );
  }
  return createPrivateDeploymentConfig({
    prefix: "witself-agent-email-receive-bootstrap-config-",
    parentDirectory: sourceSnapshot.parentDirectory,
    entrypointTarget: sourceSnapshot.entrypointTarget,
    render: (path) => runCommand(process.execPath, [
      join(root, "scripts", "render-wrangler.mjs"),
      "--output", path,
    ], { env: renderEnvironment, timeoutMs: 60_000 }),
  });
}

function exactObjectKeys(value, keys, label) {
  if (!value || Array.isArray(value) || typeof value !== "object" ||
      JSON.stringify(Object.keys(value).sort()) !==
        JSON.stringify([...keys].sort())) {
    throw new Error(`production receive bootstrap ${label} was invalid`);
  }
}

export function validateBootstrapDeploymentConfig(
  raw,
  release,
  environment,
  configPath = "",
  entrypointTarget = "",
) {
  const config = parseJSONC(raw, "production receive bootstrap configuration");
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
  const exactRateLimits = [
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
  if ((configPath === "") !== (entrypointTarget === "")) {
    throw new Error(
      "production receive bootstrap configuration relocation was invalid",
    );
  }
  let expectedMain = "src/index.js";
  if (configPath !== "") {
    if (typeof configPath !== "string" ||
        typeof entrypointTarget !== "string" ||
        !isAbsolute(configPath) || !isAbsolute(entrypointTarget) ||
        resolve(configPath) !== configPath ||
        resolve(entrypointTarget) !== entrypointTarget) {
      throw new Error(
        "production receive bootstrap configuration relocation was invalid",
      );
    }
    expectedMain = relative(dirname(configPath), entrypointTarget)
      .split(sep)
      .join("/");
    if (!/^(?:\.\.\/)*[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._-]+)*$/.test(
      expectedMain,
    ) || resolve(dirname(configPath), expectedMain) !== entrypointTarget) {
      throw new Error(
        "production receive bootstrap configuration relocation was invalid",
      );
    }
  }
  if (config.name !== PRODUCTION_RECEIVE_WORKER ||
      config.main !== expectedMain || config.workers_dev !== false ||
      config.preview_urls !== false ||
      config.compatibility_date !== "2026-07-21" ||
      JSON.stringify(config.compatibility_flags) !==
        JSON.stringify(["global_fetch_strictly_public"]) ||
      JSON.stringify(config.secrets.required) !== JSON.stringify(SECRET_NAMES) ||
      JSON.stringify(config.vars) !== JSON.stringify(expectedVars) ||
      JSON.stringify(config.kv_namespaces) !== JSON.stringify([{
        binding: "EMAIL_DIRECTORY",
        id: expected.directoryID,
      }]) ||
      JSON.stringify(config.analytics_engine_datasets) !== JSON.stringify([{
        binding: "EMAIL_EDGE_METRICS",
        dataset: "witself_agent_email_edge",
      }]) ||
      JSON.stringify(config.ratelimits) !== JSON.stringify(exactRateLimits) ||
      JSON.stringify(config.observability) !== JSON.stringify({ enabled: true })) {
    throw new Error(
      "production receive bootstrap configuration did not match the reviewed release contract",
    );
  }
  return Object.freeze({
    worker: config.name,
    release: Object.freeze({
      version: release.version,
      commit: release.commit,
      date: release.date,
    }),
    directory_namespace_id: expected.directoryID,
  });
}

export function assertBootstrapReleaseUnchanged(expected, current) {
  const fields = ["version", "commit", "date", "tag", "clean"];
  if (fields.some((field) => current?.[field] !== expected?.[field]) ||
      expected?.clean !== true) {
    throw new Error(
      "production receive bootstrap release source changed during deployment",
    );
  }
  return current;
}

function exactSecretKeys(value) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify(SECRET_NAMES);
}

export function validateBootstrapSecrets(raw) {
  let value;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error("production receive bootstrap secrets were invalid JSON");
  }
  if (!exactSecretKeys(value)) {
    throw new Error(
      "production receive bootstrap requires exactly both reviewed Worker secrets",
    );
  }
  const token = value.CONTROL_PLANE_EDGE_TOKEN;
  try {
    validateFallbackToken(token);
  } catch {
    throw new Error("production receive bootstrap edge token was invalid");
  }
  const encoded = value.RELAY_ED25519_PRIVATE_KEY;
  if (typeof encoded !== "string" || encoded.length < 48 || encoded.length > 512 ||
      !/^[A-Za-z0-9+/]+={0,2}$/.test(encoded)) {
    throw new Error("production receive bootstrap relay key was invalid");
  }
  const decoded = Buffer.from(encoded, "base64");
  if (decoded.toString("base64") !== encoded) {
    throw new Error("production receive bootstrap relay key was invalid");
  }
  try {
    const key = createPrivateKey({ key: decoded, format: "der", type: "pkcs8" });
    if (key.asymmetricKeyType !== "ed25519") throw new Error("wrong key type");
  } catch {
    throw new Error("production receive bootstrap relay key was invalid");
  }
  return Object.freeze({
    CONTROL_PLANE_EDGE_TOKEN: token,
    RELAY_ED25519_PRIVATE_KEY: encoded,
  });
}

function relayPublicKey(encodedPrivateKey) {
  const privateKey = createPrivateKey({
    key: Buffer.from(encodedPrivateKey, "base64"),
    format: "der",
    type: "pkcs8",
  });
  const spki = createPublicKey(privateKey).export({ format: "der", type: "spki" });
  if (spki.byteLength !== ED25519_SPKI_PREFIX.byteLength + 32 ||
      !spki.subarray(0, ED25519_SPKI_PREFIX.byteLength).equals(ED25519_SPKI_PREFIX)) {
    throw new Error("production receive bootstrap relay key was invalid");
  }
  return spki.subarray(ED25519_SPKI_PREFIX.byteLength).toString("base64");
}

async function readPrivateSource(path) {
  const metadata = await lstat(path);
  const mode = metadata.mode & 0o777;
  if (!metadata.isFile() || metadata.isSymbolicLink() ||
      (mode !== 0o400 && mode !== 0o600) ||
      metadata.size < 2 || metadata.size > MAX_SECRETS_BYTES) {
    throw new Error("production receive bootstrap secrets file had unsafe metadata");
  }
  const raw = await readFile(path, "utf8");
  if (Buffer.byteLength(raw) !== metadata.size) {
    throw new Error("production receive bootstrap secrets file changed while reading");
  }
  return validateBootstrapSecrets(raw);
}

export async function createPrivateBootstrapSecrets(sourcePath, runtime = {}) {
  const createDirectory = requiredFunction(
    runtime.mkdtemp ?? mkdtemp,
    "secret snapshot temporary-directory factory",
  );
  const setMode = requiredFunction(
    runtime.chmod ?? chmod,
    "secret snapshot mode setter",
  );
  const writeSnapshot = requiredFunction(
    runtime.writeFile ?? writeFile,
    "secret snapshot writer",
  );
  const removeDirectory = requiredFunction(
    runtime.rm ?? rm,
    "secret snapshot cleanup remover",
  );
  const values = await readPrivateSource(sourcePath);
  const relayPublicKeyBase64 = relayPublicKey(
    values.RELAY_ED25519_PRIVATE_KEY,
  );
  const directory = await createDirectory(join(
    tmpdir(),
    "witself-agent-email-receive-bootstrap-",
  ));
  const path = join(directory, "secrets.json");
  let cleaned = false;
  try {
    await setMode(directory, PRIVATE_DIRECTORY_MODE);
    const bytes = Buffer.from(`${JSON.stringify(values)}\n`);
    const byteLength = bytes.byteLength;
    const digest = sha256(bytes);
    try {
      await writeSnapshot(path, bytes, { mode: 0o600, flag: "wx" });
      await setMode(path, IMMUTABLE_FILE_MODE);
    } finally {
      bytes.fill(0);
    }
    return Object.freeze({
      path,
      relayPublicKeyBase64,
      matchesControlPlaneToken(candidate) {
        try {
          validateFallbackToken(candidate);
        } catch {
          return false;
        }
        const left = Buffer.from(values.CONTROL_PLANE_EDGE_TOKEN);
        const right = Buffer.from(candidate);
        const matches = left.byteLength === right.byteLength &&
          timingSafeEqual(left, right);
        left.fill(0);
        right.fill(0);
        return matches;
      },
      async assertUnchanged() {
        if (cleaned) throw new Error("private bootstrap secrets were already cleaned up");
        const [directoryMetadata, metadata, current] = await Promise.all([
          lstat(directory),
          lstat(path),
          readFile(path),
        ]);
        if (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
            (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
            !metadata.isFile() || metadata.isSymbolicLink() ||
            (metadata.mode & 0o777) !== IMMUTABLE_FILE_MODE ||
            current.byteLength !== byteLength || sha256(current) !== digest) {
          throw new Error("private bootstrap secrets changed during deployment");
        }
      },
      async cleanup() {
        if (cleaned) return;
        await removeDirectory(directory, { recursive: true, force: true });
        cleaned = true;
      },
    });
  } catch (error) {
    cleaned = true;
    let cleanupError;
    try {
      await removeDirectory(directory, { recursive: true, force: true });
    } catch (failure) {
      cleanupError = failure;
    }
    if (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        "private bootstrap secret snapshot creation failed and cleanup was incomplete",
      );
    }
    throw error;
  }
}

function runWranglerInspection(args, environment = process.env) {
  return spawnSync("wrangler", withReviewedWranglerEnvironmentFile(args), {
    cwd: root,
    encoding: "utf8",
    env: sanitizedWranglerInspectionEnvironment(environment),
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 5 * 1024 * 1024,
    timeout: 30_000,
  });
}

function wranglerJSON(args, label, inspect = runWranglerInspection) {
  const result = inspect(args);
  if (result?.error || result?.status !== 0) {
    throw new Error(`could not inspect ${label}`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error(`could not inspect ${label}`);
  }
}

function plain(bindings, name, expected) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || binding.text !== expected) {
    throw new Error(`retired pilot Worker ${name} binding was not dark and stable`);
  }
}

export function verifyLegacyWorkerDark(
  deployment,
  version,
  secrets,
  expectedDirectoryID,
) {
  const versionID = activeVersionID(deployment, "retired pilot Worker");
  const bindings = activeBindings(version, versionID, "retired pilot Worker");
  if (JSON.stringify(version.resources?.script?.handlers) !== JSON.stringify(["email"])) {
    throw new Error("retired pilot Worker was not email-only");
  }
  plain(bindings, "AGENT_EMAIL_DOMAIN", "witmail.net");
  plain(bindings, "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", "");
  plain(bindings, "REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false");
  plain(bindings, "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false");
  if ([CUSTOM_DOMAIN_DELIVERY_SECRET, ...LEGACY_PILOT_TRUST_BINDINGS]
    .some((name) => bindings.has(name))) {
    throw new Error("retired pilot Worker delivery trust was not fully dark");
  }
  const secretNames = secretInventory(secrets, "retired pilot Worker");
  for (const name of SECRET_NAMES) {
    if (bindings.get(name)?.type !== "secret_text" || !secretNames.has(name)) {
      throw new Error("retired pilot Worker required secret inventory was incomplete");
    }
  }
  if ([CUSTOM_DOMAIN_DELIVERY_SECRET, ...LEGACY_PILOT_TRUST_BINDINGS]
    .some((name) => secretNames.has(name))) {
    throw new Error("retired pilot Worker delivery trust was not fully dark");
  }
  const directory = bindings.get("EMAIL_DIRECTORY");
  const metrics = bindings.get("EMAIL_EDGE_METRICS");
  if (directory?.type !== "kv_namespace" ||
      directory.namespace_id !== expectedDirectoryID ||
      metrics?.type !== "analytics_engine" ||
      metrics.dataset !== "witself_agent_email_edge") {
    throw new Error("retired pilot Worker shared receive resources did not match");
  }
  for (const [name, namespaceID, limit] of [
    ["REALM_ROUTE_COLD_MISS_LIMITER", "2201", 10],
    ["REALM_ROUTE_KNOWN_MISS_LIMITER", "2202", 100],
  ]) {
    const limiter = bindings.get(name);
    if (limiter?.type !== "ratelimit" || limiter.namespace_id !== namespaceID ||
        limiter.simple?.limit !== limit || limiter.simple?.period !== 10) {
      throw new Error("retired pilot Worker rate-limit resources did not match");
    }
  }
  const scriptETag = String(version.resources?.script?.etag ?? "");
  const releaseVersion = String(
    bindings.get("WITSELF_EDGE_RELEASE_VERSION")?.text ?? "",
  );
  if (!/^[0-9A-Za-z._:-]{16,256}$/.test(scriptETag) ||
      !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(releaseVersion)) {
    throw new Error("retired pilot Worker release identity was invalid");
  }
  return Object.freeze({
    worker: LEGACY_PILOT_WORKER,
    deployment_id: deployment.id,
    version_id: version.id,
    script_etag: scriptETag,
    release_version: releaseVersion,
    directory_namespace_id: expectedDirectoryID,
    managed_delivery_dark: true,
  });
}

export function inspectLegacyWorkerDark(
  expectedDirectoryID,
  inspect = runWranglerInspection,
) {
  const deployment = wranglerJSON([
    "deployments", "status", "--name", LEGACY_PILOT_WORKER, "--json",
  ], "the retired pilot Worker deployment", inspect);
  const versionID = activeVersionID(deployment, "retired pilot Worker");
  const version = wranglerJSON([
    "versions", "view", versionID, "--name", LEGACY_PILOT_WORKER, "--json",
  ], "the retired pilot Worker version", inspect);
  const secrets = wranglerJSON([
    "secret", "list", "--name", LEGACY_PILOT_WORKER, "--format", "json",
  ], "the retired pilot Worker secret inventory", inspect);
  const finalDeployment = wranglerJSON([
    "deployments", "status", "--name", LEGACY_PILOT_WORKER, "--json",
  ], "the retired pilot Worker deployment", inspect);
  if (finalDeployment?.id !== deployment.id ||
      activeVersionID(finalDeployment, "retired pilot Worker") !== versionID) {
    throw new Error(
      "retired pilot Worker changed during exact provider inspection",
    );
  }
  return verifyLegacyWorkerDark(
    deployment,
    version,
    secrets,
    expectedDirectoryID,
  );
}

export function assertProductionReceiveWorkerAbsent(inspect = runWranglerInspection) {
  const result = inspect([
    "deployments", "status", "--name", PRODUCTION_RECEIVE_WORKER, "--json",
  ]);
  if (result?.error) {
    throw new Error("could not prove the production receive Worker is absent");
  }
  if (result?.status === 0) {
    throw new Error("production receive Worker already exists; use the normal deploy command");
  }
  const output = `${String(result?.stdout ?? "")}\n${String(result?.stderr ?? "")}`;
  if (!output.includes(`code: ${ABSENT_WORKER_CODE}`) ||
      !output.includes("This Worker does not exist on your account")) {
    throw new Error("could not prove the production receive Worker is absent");
  }
}

function run(command, args, {
  signal,
  timeoutMs = 5 * 60_000,
  env,
} = {}) {
  return runLeaseGuardedCommand(command, args, {
    cwd: root,
    signal,
    timeoutMs,
    env,
  });
}

function requiredFunction(value, label) {
  if (typeof value !== "function") {
    throw new Error(`production receive bootstrap ${label} was invalid`);
  }
  return value;
}

async function cleanupBootstrapInputs(config, sourceSnapshot, secrets) {
  const cleanups = [];
  if (typeof config?.cleanup === "function") {
    cleanups.push(() => config.cleanup());
  }
  if (typeof sourceSnapshot?.cleanup === "function") {
    cleanups.push(() => sourceSnapshot.cleanup());
  }
  if (typeof secrets?.cleanup === "function") {
    cleanups.push(() => secrets.cleanup());
  }
  const failures = [];
  for (const cleanup of cleanups) {
    try {
      await cleanup();
    } catch (error) {
      failures.push(error);
    }
  }
  if (failures.length > 0) {
    throw new Error("production receive bootstrap private input cleanup failed", {
      cause: failures.length === 1 ? failures[0] : new AggregateError(failures),
    });
  }
}

export async function bootstrapProductionReceive(options, dependencies = {}) {
  const requestedOptions = parseBootstrapArgs([
    "--secrets-file", options?.secretsFile,
    "--receipt", options?.receipt,
  ]);
  const requestedCheckout = dependencies.repositoryRoot ?? repositoryRoot;
  const resolvePaths = requiredFunction(
    dependencies.resolvePaths ?? assertBootstrapPathsOutsideRepository,
    "physical path boundary inspector",
  );
  const physicalPaths = await resolvePaths(requestedOptions, requestedCheckout);
  if (!physicalPaths || typeof physicalPaths !== "object" ||
      typeof physicalPaths.repositoryRoot !== "string" ||
      !isAbsolute(physicalPaths.repositoryRoot) ||
      resolve(physicalPaths.repositoryRoot) !== physicalPaths.repositoryRoot ||
      typeof physicalPaths.secretsFile !== "string" ||
      !isAbsolute(physicalPaths.secretsFile) ||
      resolve(physicalPaths.secretsFile) !== physicalPaths.secretsFile ||
      typeof physicalPaths.receipt !== "string" ||
      !isAbsolute(physicalPaths.receipt) ||
      resolve(physicalPaths.receipt) !== physicalPaths.receipt ||
      insideDirectory(physicalPaths.repositoryRoot, physicalPaths.secretsFile) ||
      insideDirectory(physicalPaths.repositoryRoot, physicalPaths.receipt)) {
    throw new Error(
      "production receive bootstrap physical path boundary was invalid",
    );
  }
  const checkout = physicalPaths.repositoryRoot;
  const parsedOptions = Object.freeze({
    secretsFile: physicalPaths.secretsFile,
    receipt: physicalPaths.receipt,
  });
  const environment = dependencies.environment ?? process.env;
  assertProductionCloudflareIdentity(environment);
  const identifySource = requiredFunction(
    dependencies.sourceIdentity ?? sourceIdentity,
    "source identity inspector",
  );
  const parseProviderEnvironment = requiredFunction(
    dependencies.cloudflareEnvironment ?? cloudflareEnvironment,
    "provider environment inspector",
  );
  const createSecrets = requiredFunction(
    dependencies.createSecrets ?? createPrivateBootstrapSecrets,
    "secret snapshot factory",
  );
  const withLease = requiredFunction(
    dependencies.withLease ?? withAgentEmailOperationsLease,
    "operations lease client",
  );
  const wranglerInspection = dependencies.wranglerInspection ?? ((args) =>
    runWranglerInspection(args, environment));
  const inspectAbsent = requiredFunction(
    dependencies.assertWorkerAbsent ?? (() =>
      assertProductionReceiveWorkerAbsent(wranglerInspection)),
    "new Worker absence inspector",
  );
  const preflightCohort = requiredFunction(
    dependencies.preflightCohort ?? ((releaseVersion, cohort) =>
      preflightManagedCohortDeploymentOrder(
        releaseVersion,
        cohort,
        (args) => wranglerJSON(
          args,
          "the active control-plane deployment",
          wranglerInspection,
        ),
      )),
    "cohort preflight",
  );
  const inspectLegacy = requiredFunction(
    dependencies.inspectLegacyWorker ?? ((directoryID) =>
      inspectLegacyWorkerDark(directoryID, wranglerInspection)),
    "predecessor inspector",
  );
  const captureProvider = requiredFunction(
    dependencies.captureProvider ?? captureRelayProviderDarkState,
    "provider-route inspector",
  );
  const validateProvider = requiredFunction(
    dependencies.validateProvider ?? validateRelayProviderDarkState,
    "provider-route validator",
  );
  const reserveReceipt = requiredFunction(
    dependencies.reserveReceipt ?? reserveJSONReceipt,
    "receipt journal",
  );
  const runCommand = requiredFunction(
    dependencies.runCommand ?? run,
    "command runner",
  );
  const attestProduction = requiredFunction(
    dependencies.verifyProduction ?? verifyProduction,
    "production attestor",
  );
  const validateLeaseEvidence = requiredFunction(
    dependencies.validateLeaseEvidence ?? validateAgentEmailOperationsLeaseEvidence,
    "lease evidence validator",
  );
  const createSourceSnapshot = requiredFunction(
    dependencies.createSourceSnapshot ?? createPrivateReleaseWorkerSource,
    "source snapshot factory",
  );
  const validateConfig = requiredFunction(
    dependencies.validateConfig ?? validateBootstrapDeploymentConfig,
    "configuration validator",
  );
  const release = identifySource({ requireRelease: true });
  const leaseOrigin = canonicalControlPlaneOrigin(environment.CONTROL_PLANE_URL);
  const providerConfig = parseProviderEnvironment(environment);
  const legacyZoneID = String(
    environment.CLOUDFLARE_LEGACY_EMAIL_ZONE_ID ?? "",
  );
  if (!/^[0-9a-f]{32}$/.test(legacyZoneID) ||
      legacyZoneID === providerConfig.zoneID) {
    throw new Error(
      "CLOUDFLARE_LEGACY_EMAIL_ZONE_ID must identify the distinct witwave.ai zone",
    );
  }
  const legacyProviderConfig = parseProviderEnvironment({
    ...environment,
    CLOUDFLARE_ZONE_ID: legacyZoneID,
  });
  const providers = dependencies.providers ?? Object.freeze({
    primary: new CloudflareAPI(providerConfig),
    legacy: new CloudflareAPI(legacyProviderConfig),
  });
  if (!providers || typeof providers !== "object" ||
      providers.primary == null || providers.legacy == null ||
      providers.primary === providers.legacy) {
    throw new Error("production receive bootstrap provider inspectors were invalid");
  }
  const targetAllowlist = String(
    environment.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST ?? "",
  );
  parseManagedDeliveryAccountAllowlist(targetAllowlist);
  if (targetAllowlist !== "" ||
      environment.REALM_EMAIL_ALIAS_DELIVERY_ENABLED !== "false" ||
      environment.REALM_EMAIL_CANONICAL_DELIVERY_ENABLED !== "false") {
    throw new Error("production receive bootstrap requires an empty cohort and dark delivery gates");
  }

  const secrets = await createSecrets(parsedOptions.secretsFile);
  if (!secrets || typeof secrets !== "object" ||
      typeof secrets.path !== "string" || !isAbsolute(secrets.path) ||
      insideDirectory(checkout, secrets.path) ||
      typeof secrets.matchesControlPlaneToken !== "function" ||
      typeof secrets.assertUnchanged !== "function" ||
      typeof secrets.cleanup !== "function") {
    if (typeof secrets?.cleanup === "function") await secrets.cleanup();
    throw new Error("production receive bootstrap secret snapshot was invalid");
  }
  const relayPublicKey = String(
    environment.RELAY_ED25519_PUBLIC_KEY ?? "",
  );
  if (!secrets.matchesControlPlaneToken(
    environment.CONTROL_PLANE_EDGE_TOKEN,
  ) || !/^[A-Za-z0-9+/]{43}=$/.test(relayPublicKey) ||
      relayPublicKey !== secrets.relayPublicKeyBase64) {
    await secrets.cleanup();
    throw new Error(
      "production receive bootstrap secrets did not match the active lease token and reviewed relay public key",
    );
  }
  const relayPublicKeySHA256 = sha256(Buffer.from(relayPublicKey, "base64"));
  const renderEnvironment = sanitizedWranglerEnvironment(environment);
  renderEnvironment.CONTROL_PLANE_URL = leaseOrigin;
  const wranglerMutationEnvironment = sanitizedWranglerEnvironment(environment);
  let config;
  let sourceSnapshot;
  try {
    sourceSnapshot = await createSourceSnapshot(release.commit);
    const createConfig = dependencies.createConfig ?? ((snapshot) =>
      createBootstrapDeploymentConfig(
        snapshot,
        renderEnvironment,
        runCommand,
      ));
    config = await requiredFunction(
      createConfig,
      "configuration snapshot factory",
    )(sourceSnapshot);
    if (typeof config?.path !== "string" || !isAbsolute(config.path) ||
        insideDirectory(checkout, config.path)) {
      throw new Error(
        "production receive bootstrap configuration snapshot must be outside the repository",
      );
    }
    if (!config || typeof config.assertUnchanged !== "function" ||
        typeof config.readText !== "function" ||
        typeof config.cleanup !== "function" ||
        !/^[0-9a-f]{64}$/.test(String(config.sha256 ?? "")) ||
        !sourceSnapshot ||
        !Number.isSafeInteger(sourceSnapshot.file_count) ||
        sourceSnapshot.file_count < 1 ||
        !Number.isSafeInteger(sourceSnapshot.byte_count) ||
        sourceSnapshot.byte_count < 1 ||
        !/^[0-9a-f]{64}$/.test(String(sourceSnapshot.sha256 ?? "")) ||
        typeof sourceSnapshot.assertUnchanged !== "function" ||
        typeof sourceSnapshot.cleanup !== "function" ||
        typeof sourceSnapshot.parentDirectory !== "string" ||
        !isAbsolute(sourceSnapshot.parentDirectory) ||
        resolve(sourceSnapshot.parentDirectory) !==
          sourceSnapshot.parentDirectory ||
        insideDirectory(checkout, sourceSnapshot.parentDirectory) ||
        typeof sourceSnapshot.entrypointTarget !== "string" ||
        !isAbsolute(sourceSnapshot.entrypointTarget) ||
        resolve(sourceSnapshot.entrypointTarget) !==
          sourceSnapshot.entrypointTarget ||
        !insideDirectory(
          sourceSnapshot.parentDirectory,
          sourceSnapshot.entrypointTarget,
        ) ||
        !insideDirectory(sourceSnapshot.parentDirectory, config.path)) {
      throw new Error("production receive bootstrap frozen inputs were invalid");
    }
    validateConfig(
      await config.readText(),
      release,
      environment,
      config.path,
      sourceSnapshot.entrypointTarget,
    );
    assertBootstrapReleaseUnchanged(
      release,
      identifySource({ requireRelease: true }),
    );
  } catch (error) {
    await cleanupBootstrapInputs(config, sourceSnapshot, secrets);
    throw error;
  }

  const captureProviderPair = async (expected = null) => Object.freeze({
    primary: validateProvider(
      await captureProvider(providers.primary, PRIMARY_PROVIDER_ZONE_CONTRACT),
      expected?.primary?.provider_scope,
    ),
    legacy: validateProvider(
      await captureProvider(providers.legacy, LEGACY_PROVIDER_ZONE_CONTRACT),
      expected?.legacy?.provider_scope,
    ),
  });
  const assertFrozenInputs = () => Promise.all([
    config.assertUnchanged(),
    secrets.assertUnchanged(),
    sourceSnapshot.assertUnchanged(),
  ]);
  let journal;
  let committableReceipt;
  let inputsCleaned = false;
  try {
    const result = await withLease(
      OPERATIONS_LEASE_OPERATION,
      async (leaseGuard) => {
        if (!leaseGuard || typeof leaseGuard.renew !== "function" ||
            typeof leaseGuard.evidence !== "function" || !leaseGuard.signal) {
          throw new Error("production receive bootstrap lease guard was invalid");
        }
        await assertFrozenInputs();
        assertBootstrapReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        inspectAbsent();
        preflightCohort(release.version, targetAllowlist);
        const legacyBefore = inspectLegacy(providerConfig.namespaceID);
        const providerBefore = await captureProviderPair();
        assertBootstrapReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        journal = reserveReceipt(parsedOptions.receipt, Object.freeze({
          schema: "witself.agent-email-production-receive-bootstrap.v1",
          outcome: "pending",
          operation: OPERATIONS_LEASE_OPERATION,
          worker: PRODUCTION_RECEIVE_WORKER,
          release: Object.freeze({
            version: release.version,
            commit: release.commit,
            date: release.date,
          }),
          config_sha256: config.sha256,
          source_sha256: sourceSnapshot.sha256,
          source_file_count: sourceSnapshot.file_count,
          relay_public_key_sha256: relayPublicKeySHA256,
          predecessor: legacyBefore,
          provider: providerBefore,
          recovery: "reconcile_dark_live_state_before_retry",
        }));
        if (!journal || typeof journal.commit !== "function" ||
            typeof journal.close !== "function") {
          throw new Error("production receive bootstrap receipt journal was invalid");
        }
        await leaseGuard.renew();
        await assertFrozenInputs();
        assertBootstrapReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        await runCommand("wrangler", withReviewedWranglerEnvironmentFile([
          "deploy",
          "--config", config.path,
          "--secrets-file", secrets.path,
          "--strict",
          "--tag", release.tag,
          "--message", releaseMessage(release),
        ]), {
          signal: leaseGuard.signal,
          env: wranglerMutationEnvironment,
        });
        await assertFrozenInputs();
        assertBootstrapReleaseUnchanged(
          release,
          identifySource({ requireRelease: true }),
        );
        const attestation = attestProduction({
          env: environment,
          inspect: (args) => wranglerJSON(
            args,
            "the production receive Worker deployment",
            wranglerInspection,
          ),
          release,
          requireAnnotations: true,
        });
        const legacyAfter = inspectLegacy(providerConfig.namespaceID);
        const providerAfter = await captureProviderPair(providerBefore);
        if (JSON.stringify(legacyAfter) !== JSON.stringify(legacyBefore) ||
            JSON.stringify(providerAfter) !== JSON.stringify(providerBefore)) {
          throw new Error("production receive bootstrap provider state changed unexpectedly");
        }
        preflightCohort(release.version, targetAllowlist);
        await leaseGuard.renew();
        const leaseEvidence = leaseGuard.evidence();
        validateLeaseEvidence(
          leaseEvidence,
          OPERATIONS_LEASE_OPERATION,
        );
        committableReceipt = Object.freeze({
          schema: "witself.agent-email-production-receive-bootstrap.v1",
          outcome: "bootstrapped",
          worker: PRODUCTION_RECEIVE_WORKER,
          release: Object.freeze({
            version: release.version,
            commit: release.commit,
            date: release.date,
          }),
          config_sha256: config.sha256,
          source_sha256: sourceSnapshot.sha256,
          source_file_count: sourceSnapshot.file_count,
          relay_public_key_sha256: relayPublicKeySHA256,
          predecessor: legacyAfter,
          production: attestation,
          provider: providerAfter,
          operations_lease: leaseEvidence,
          safeguards: Object.freeze({
            prior_worker_unchanged: true,
            production_worker_was_absent: true,
            secrets_uploaded_in_initial_version: true,
            managed_delivery_cohort_empty: true,
            managed_delivery_gates_dark: true,
            primary_and_legacy_provider_routes_unchanged_and_dark: true,
            retired_worker_all_delivery_trust_absent: true,
            tagged_worker_source_snapshot_used: true,
            frozen_private_inputs_verified: true,
          }),
        });
        return committableReceipt;
      },
      { endpoint: leaseOrigin, env: environment },
    );
    if (result !== committableReceipt || committableReceipt == null ||
        journal == null) {
      throw new Error("production receive bootstrap completion fence was invalid");
    }
    await cleanupBootstrapInputs(config, sourceSnapshot, secrets);
    inputsCleaned = true;
    journal.commit(committableReceipt);
    return committableReceipt;
  } catch (error) {
    journal?.close?.();
    let cleanupError = null;
    if (!inputsCleaned) {
      try {
        await cleanupBootstrapInputs(config, sourceSnapshot, secrets);
      } catch (current) {
        cleanupError = current;
      }
    }
    if (cleanupError != null) {
      throw new AggregateError(
        [error, cleanupError],
        "production receive bootstrap and private input cleanup failed",
      );
    }
    throw error;
  }
}

export async function main(argv = process.argv.slice(2)) {
  return bootstrapProductionReceive(parseBootstrapArgs(argv));
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
