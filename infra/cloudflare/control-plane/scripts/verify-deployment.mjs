#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { lstatSync, readdirSync } from "node:fs";
import { readFile } from "node:fs/promises";
import {
  basename,
  dirname,
  join,
  relative,
  resolve,
  sep,
} from "node:path";
import { fileURLToPath } from "node:url";

import {
  validateBuildMetadata,
  workerVersionMessage,
  workerVersionTag,
} from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/agent-email-managed-delivery-cohort.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const generatedConfigPath = join(root, "wrangler.generated.jsonc");
const DIRECTORY_NAMESPACE_ID = "ec620d5131524e138a9fca6207953cd2";
const COMPATIBILITY_DATE = "2026-06-01";
const MIGRATION_TAG = "v8";
const CPU_LIMIT_MS = 300000;
const NAMED_HANDLER_CLASSES = Object.freeze([
  "AccountBackup",
  "AccountLifecycle",
  "AccountSignup",
  "AgentEmailDomainRegistry",
  "Backend",
  "RealmEmailAliasRegistry",
  "TargetCellCoordinator",
]);
const DURABLE_OBJECT_BINDINGS = Object.freeze({
  ACCOUNT_BACKUP: "AccountBackup",
  ACCOUNT_LIFECYCLE: "AccountLifecycle",
  ACCOUNT_SIGNUP: "AccountSignup",
  AGENT_EMAIL_DOMAINS: "AgentEmailDomainRegistry",
  CELL_COORDINATOR: "TargetCellCoordinator",
  CONTROL_PLANE: "Backend",
  REALM_EMAIL_ALIASES: "RealmEmailAliasRegistry",
});
const R2_BINDINGS = Object.freeze({
  AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL:
    "witself-agent-email-domain-authority-journal",
  ARCHIVES: "witself-archives",
  BACKUPS: "witself-backups",
  REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL:
    "witself-realm-email-alias-authority-journal",
});
const REQUIRED_SECRET_BINDINGS = Object.freeze([
  "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY",
  "CONTROL_PLANE_EDGE_TOKEN",
  "SUPPORT_EMAIL_INTAKE_TOKEN",
]);

function isRecord(value) {
  return value != null && typeof value === "object" && !Array.isArray(value);
}

function sameJSON(actual, expected) {
  return JSON.stringify(actual) === JSON.stringify(expected);
}

function stripJSONComments(input) {
  let output = "";
  let quoted = false;
  let escaped = false;
  let lineComment = false;
  let blockComment = false;
  for (let index = 0; index < input.length; index += 1) {
    const character = input[index];
    const next = input[index + 1];
    if (lineComment) {
      if (character === "\n") {
        lineComment = false;
        output += character;
      }
      continue;
    }
    if (blockComment) {
      if (character === "*" && next === "/") {
        blockComment = false;
        index += 1;
      } else if (character === "\n") {
        output += character;
      }
      continue;
    }
    if (quoted) {
      output += character;
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === "\"") quoted = false;
      continue;
    }
    if (character === "\"") {
      quoted = true;
      output += character;
    } else if (character === "/" && next === "/") {
      lineComment = true;
      index += 1;
    } else if (character === "/" && next === "*") {
      blockComment = true;
      index += 1;
    } else {
      output += character;
    }
  }
  if (quoted || blockComment) {
    throw new Error("generated control-plane config was malformed JSONC");
  }
  return output;
}

function parseGeneratedConfig(source) {
  try {
    const parsed = JSON.parse(stripJSONComments(source));
    if (!isRecord(parsed)) throw new Error("not an object");
    return parsed;
  } catch (error) {
    if (String(error?.message ?? "").includes("malformed JSONC")) throw error;
    throw new Error("generated control-plane config was not valid JSONC");
  }
}

function namedConfigBindings(bindings, property, description) {
  if (!Array.isArray(bindings)) {
    throw new Error(`generated config had an invalid ${description} inventory`);
  }
  const result = new Map();
  for (const binding of bindings) {
    const name = binding?.[property];
    if (!isRecord(binding) || typeof name !== "string" || !name ||
        result.has(name)) {
      throw new Error(`generated config had an invalid ${description} inventory`);
    }
    result.set(name, binding);
  }
  return result;
}

function exactConfigNames(actual, expected, description) {
  if (!sameJSON([...actual.keys()].sort(), [...expected].sort())) {
    throw new Error(`generated config ${description} did not match the reviewed contract`);
  }
}

function assertGeneratedConfigContract(config, expectedMain) {
  const topLevelKeys = [
    "compatibility_date",
    "containers",
    "durable_objects",
    "kv_namespaces",
    "limits",
    "main",
    "migrations",
    "name",
    "observability",
    "r2_buckets",
    "routes",
    "secrets",
    "send_email",
    "triggers",
    "unsafe",
    "vars",
  ];
  if (!sameJSON(Object.keys(config).sort(), topLevelKeys.sort())) {
    throw new Error("generated config top-level contract did not match");
  }
  if (config.name !== "witself-control-plane" || config.main !== expectedMain) {
    throw new Error("generated config main Worker entrypoint did not match");
  }
  if (config.compatibility_date !== COMPATIBILITY_DATE ||
      !sameJSON(config.limits, { cpu_ms: CPU_LIMIT_MS })) {
    throw new Error("generated config Worker runtime did not match");
  }
  if (!sameJSON(config.secrets, { required: REQUIRED_SECRET_BINDINGS })) {
    throw new Error("generated config required secret contract did not match");
  }

  if (!Array.isArray(config.containers) || config.containers.length !== 1) {
    throw new Error("generated config Backend container contract did not match");
  }
  const [container] = config.containers;
  if (!isRecord(container) || container.name !== "witself-control-plane" ||
      !sameJSON(Object.keys(container).sort(), [
        "class_name",
        "image",
        "image_build_context",
        "image_vars",
        "instance_type",
        "max_instances",
        "name",
      ].sort()) ||
      container.class_name !== "Backend" ||
      container.image !== "../../../images/witself-control-plane/Dockerfile" ||
      container.image_build_context !== "../../.." ||
      container.instance_type !== "lite" || container.max_instances !== 2 ||
      !isRecord(container.image_vars) ||
      !sameJSON(Object.keys(container.image_vars).sort(), [
        "COMMIT",
        "DATE",
        "VERSION",
      ])) {
    throw new Error("generated config Backend container contract did not match");
  }

  if (!isRecord(config.durable_objects) ||
      !sameJSON(Object.keys(config.durable_objects), ["bindings"])) {
    throw new Error("generated config Durable Object contract did not match");
  }

  const durableObjects = namedConfigBindings(
    config.durable_objects?.bindings,
    "name",
    "Durable Object binding",
  );
  exactConfigNames(
    durableObjects,
    Object.keys(DURABLE_OBJECT_BINDINGS),
    "Durable Object bindings",
  );
  for (const [name, className] of Object.entries(DURABLE_OBJECT_BINDINGS)) {
    if (!sameJSON(durableObjects.get(name), { name, class_name: className })) {
      throw new Error(`generated config Durable Object binding ${name} did not match`);
    }
  }

  const kv = namedConfigBindings(config.kv_namespaces, "binding", "KV binding");
  exactConfigNames(kv, ["AGENT_EMAIL_DIRECTORY", "DIRECTORY"], "KV bindings");
  if (!sameJSON(kv.get("DIRECTORY"), {
    binding: "DIRECTORY",
    id: DIRECTORY_NAMESPACE_ID,
  })) {
    throw new Error("generated config DIRECTORY KV binding did not match");
  }
  const agentEmailDirectoryID = kv.get("AGENT_EMAIL_DIRECTORY")?.id;
  if (!/^[0-9a-f]{32}$/.test(String(agentEmailDirectoryID ?? "")) ||
      !sameJSON(kv.get("AGENT_EMAIL_DIRECTORY"), {
        binding: "AGENT_EMAIL_DIRECTORY",
        id: agentEmailDirectoryID,
      })) {
    throw new Error("generated config AGENT_EMAIL_DIRECTORY KV binding did not match");
  }

  const expectedVarNames = [
    "AGENT_EMAIL_DOMAIN",
    "AGENT_EMAIL_LEGACY_DOMAINS",
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT",
    "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "CP_SIGNUP_DAILY_LIMIT_GLOBAL",
    "CP_SIGNUP_DAILY_LIMIT_PER_IP",
    "CP_SIGNUP_OPEN",
    "CP_SUPPORT_EMAIL_INTAKE_ENABLED",
    "CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED",
    "CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT",
    "CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM",
    "WITSELF_EDGE_RELEASE_COMMIT",
    "WITSELF_EDGE_RELEASE_DATE",
    "WITSELF_EDGE_RELEASE_VERSION",
  ];
  if (!isRecord(config.vars) ||
      !sameJSON(Object.keys(config.vars).sort(), [...expectedVarNames].sort()) ||
      config.vars.AGENT_EMAIL_DOMAIN !== "witmail.net" ||
      config.vars.AGENT_EMAIL_LEGACY_DOMAINS !== "agent-mail.witwave.ai" ||
      !/^[a-z][a-z0-9_-]{0,63}$/.test(
        String(config.vars.AGENT_EMAIL_ROUTE_SIGNING_KEY_ID ?? ""),
      ) ||
      config.vars.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM !== "8" ||
      config.vars.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT !== "64" ||
      config.vars.CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT !== "8" ||
      config.vars.CP_SIGNUP_DAILY_LIMIT_PER_IP !== "10" ||
      config.vars.CP_SIGNUP_DAILY_LIMIT_GLOBAL !== "500" ||
      config.vars.CP_SIGNUP_OPEN !== "true" ||
      config.vars.CP_SUPPORT_EMAIL_INTAKE_ENABLED !== "false" ||
      config.vars.CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED !== "false") {
    throw new Error("generated config Worker vars did not match the reviewed contract");
  }
  parseManagedDeliveryAccountAllowlist(
    config.vars.CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST,
  );

  const migrations = [
    { tag: "v1", new_sqlite_classes: ["ControlPlane"] },
    { tag: "v2", renamed_classes: [{ from: "ControlPlane", to: "Backend" }] },
    { tag: "v3", new_sqlite_classes: ["AccountLifecycle"] },
    { tag: "v4", new_sqlite_classes: ["TargetCellCoordinator"] },
    { tag: "v5", new_sqlite_classes: ["AccountSignup"] },
    { tag: "v6", new_sqlite_classes: ["AccountBackup"] },
    { tag: "v7", new_sqlite_classes: ["RealmEmailAliasRegistry"] },
    { tag: MIGRATION_TAG, new_sqlite_classes: ["AgentEmailDomainRegistry"] },
  ];
  if (!sameJSON(config.migrations, migrations)) {
    throw new Error("generated config migration contract did not match");
  }
  if (!sameJSON(config.routes, [{
    pattern: "self.witwave.ai",
    custom_domain: true,
  }]) || !sameJSON(config.triggers, { crons: ["*/5 * * * *", "1-59/5 * * * *"] })) {
    throw new Error("generated config route and schedule contract did not match");
  }
  if (!sameJSON(config.send_email, [{ name: "EMAIL" }])) {
    throw new Error("generated config EMAIL binding did not match");
  }

  const r2 = namedConfigBindings(config.r2_buckets, "binding", "R2 binding");
  exactConfigNames(r2, Object.keys(R2_BINDINGS), "R2 bindings");
  for (const [binding, bucketName] of Object.entries(R2_BINDINGS)) {
    if (!sameJSON(r2.get(binding), { binding, bucket_name: bucketName })) {
      throw new Error(`generated config R2 binding ${binding} did not match`);
    }
  }
  if (!sameJSON(config.unsafe, {
    bindings: [
      {
        name: "RECOVER_LIMITER",
        type: "ratelimit",
        namespace_id: "1001",
        simple: { limit: 1, period: 10 },
      },
      {
        name: "PUBLIC_IP_LIMITER",
        type: "ratelimit",
        namespace_id: "1002",
        simple: { limit: 300, period: 60 },
      },
      {
        name: "SIGNUP_IP_LIMITER",
        type: "ratelimit",
        namespace_id: "1003",
        simple: { limit: 5, period: 60 },
      },
    ],
  }) || !sameJSON(config.observability, { enabled: true })) {
    throw new Error("generated config operational binding contract did not match");
  }
  return { container, agentEmailDirectoryID };
}

export function expectedBuildMetadata(config, expectedMain = "src/index.js") {
  if (typeof expectedMain !== "string" || expectedMain.length < 1 ||
      expectedMain !== expectedMain.trim() ||
      /[\\\x00-\x1f\x7f]/.test(expectedMain)) {
    throw new Error("expected generated config main Worker entrypoint was invalid");
  }
  const parsed = parseGeneratedConfig(config);
  const { container: parsedContainer, agentEmailDirectoryID } =
    assertGeneratedConfigContract(parsed, expectedMain);
  const container = validateBuildMetadata({
    version: parsedContainer.image_vars.VERSION,
    commit: parsedContainer.image_vars.COMMIT,
    date: parsedContainer.image_vars.DATE,
  });
  const edge = validateBuildMetadata({
    version: parsed.vars.WITSELF_EDGE_RELEASE_VERSION,
    commit: parsed.vars.WITSELF_EDGE_RELEASE_COMMIT,
    date: parsed.vars.WITSELF_EDGE_RELEASE_DATE,
  });
  if (container.version !== edge.version || container.commit !== edge.commit ||
      container.date !== edge.date) {
    throw new Error("container and outer Worker release identities differ");
  }
  return Object.freeze({
    service: "witself-control-plane",
    ...edge,
    route_signing_key_id: parsed.vars.AGENT_EMAIL_ROUTE_SIGNING_KEY_ID,
    agent_email_directory_id: agentEmailDirectoryID,
    managed_delivery_account_allowlist:
      parsed.vars.CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST,
  });
}

function privateWranglerStateIsSafe(directory) {
  const wranglerDirectory = join(directory, ".wrangler");
  const wranglerTempDirectory = join(wranglerDirectory, "tmp");
  try {
    const wranglerMetadata = lstatSync(wranglerDirectory);
    const wranglerTempMetadata = lstatSync(wranglerTempDirectory);
    return wranglerMetadata.isDirectory() &&
      !wranglerMetadata.isSymbolicLink() &&
      (wranglerMetadata.mode & 0o777) === 0o700 &&
      JSON.stringify(readdirSync(wranglerDirectory)) ===
        JSON.stringify(["tmp"]) &&
      wranglerTempMetadata.isDirectory() &&
      !wranglerTempMetadata.isSymbolicLink() &&
      (wranglerTempMetadata.mode & 0o777) === 0o700 &&
      readdirSync(wranglerTempDirectory).length === 0;
  } catch {
    return false;
  }
}

export function privateDeploymentConfigMain(
  configPath,
  expectedControlPlaneRoot = root,
) {
  const path = resolve(configPath);
  const directory = dirname(path);
  const cloudflareRoot = dirname(directory);
  const infraRoot = dirname(cloudflareRoot);
  const repositoryRoot = dirname(infraRoot);
  const snapshotRoot = dirname(repositoryRoot);
  const controlPlane = join(cloudflareRoot, "control-plane");
  const entrypoint = join(controlPlane, "src", "index.js");
  const workDirectory = join(snapshotRoot, "work");
  if (basename(path) !== "wrangler.generated.jsonc" ||
      basename(cloudflareRoot) !== "cloudflare" ||
      basename(infraRoot) !== "infra" ||
      basename(repositoryRoot) !== "repository" ||
      !/^witself-control-plane-release-[A-Za-z0-9]{6}$/.test(
        basename(snapshotRoot),
      ) ||
      resolve(expectedControlPlaneRoot) !== controlPlane ||
      !/^witself-control-plane-deploy-[A-Za-z0-9]{6}$/.test(
        basename(directory),
      )) {
    throw new Error(
      "deployment verification requires an exact private control-plane configuration path",
    );
  }
  const [
    snapshotMetadata,
    repositoryMetadata,
    workMetadata,
    directoryMetadata,
    configMetadata,
    entrypointMetadata,
  ] = [
    lstatSync(snapshotRoot),
    lstatSync(repositoryRoot),
    lstatSync(workDirectory),
    lstatSync(directory),
    lstatSync(path),
    lstatSync(entrypoint),
  ];
  if (!snapshotMetadata.isDirectory() || snapshotMetadata.isSymbolicLink() ||
      (snapshotMetadata.mode & 0o777) !== 0o700 ||
      !repositoryMetadata.isDirectory() || repositoryMetadata.isSymbolicLink() ||
      (repositoryMetadata.mode & 0o777) !== 0o555 ||
      !workMetadata.isDirectory() || workMetadata.isSymbolicLink() ||
      (workMetadata.mode & 0o777) !== 0o700 ||
      !directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
      (directoryMetadata.mode & 0o777) !== 0o700 ||
      JSON.stringify(readdirSync(directory).sort()) !==
        JSON.stringify([".wrangler", "wrangler.generated.jsonc"]) ||
      !privateWranglerStateIsSafe(directory) ||
      !configMetadata.isFile() || configMetadata.isSymbolicLink() ||
      (configMetadata.mode & 0o777) !== 0o400 ||
      !entrypointMetadata.isFile() || entrypointMetadata.isSymbolicLink() ||
      ![0o444, 0o555].includes(entrypointMetadata.mode & 0o777)) {
    throw new Error(
      "deployment verification requires immutable private configuration metadata",
    );
  }
  const expectedMain = relative(directory, entrypoint).split(sep).join("/");
  if (expectedMain !== "../control-plane/src/index.js" ||
      resolve(directory, expectedMain) !== entrypoint) {
    throw new Error(
      "deployment verification could not resolve the private control-plane entrypoint",
    );
  }
  return expectedMain;
}

export function deploymentMatches(actual, expected) {
  return actual?.service === expected.service &&
    actual?.version === expected.version &&
    actual?.commit === expected.commit &&
    actual?.date === expected.date;
}

function validVersionID(value) {
  return typeof value === "string" &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value);
}

export function currentProductionVersionID(deployment) {
  if (deployment == null || typeof deployment !== "object" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1) {
    throw new Error("production must route to exactly one control-plane Worker version");
  }
  const [version] = deployment.versions;
  if (!validVersionID(version?.version_id) || version.percentage !== 100) {
    throw new Error("production must route 100 percent to one valid Worker version");
  }
  return version.version_id;
}

function bindingsByName(bindings) {
  if (!Array.isArray(bindings)) {
    throw new Error("deployed Worker version is missing its binding inventory");
  }
  const result = new Map();
  for (const binding of bindings) {
    if (!isRecord(binding) || typeof binding.name !== "string" ||
        !binding.name || result.has(binding.name)) {
      throw new Error("deployed Worker version has duplicate or invalid bindings");
    }
    if (binding.type === "secret_text" && Object.hasOwn(binding, "text")) {
      throw new Error(`deployed Worker secret binding ${binding.name} was invalid`);
    }
    result.set(binding.name, binding);
  }
  return result;
}

function exactSecretBinding(bindings, name) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    throw new Error(`deployed Worker version is missing exact ${name} secret binding`);
  }
}

function exactPlainBinding(bindings, name, expected) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || binding.text !== expected) {
    throw new Error(`deployed Worker version has the wrong ${name} binding`);
  }
}

function exactRateLimitBinding(bindings, name, namespaceID, simple) {
  const binding = bindings.get(name);
  if (binding?.type !== "ratelimit" ||
      binding.namespace_id !== namespaceID ||
      !sameJSON(binding.simple, simple)) {
    throw new Error(`deployed Worker version has the wrong ${name} binding`);
  }
}

function exactDurableObjectBinding(bindings, name, className) {
  const binding = bindings.get(name);
  if (binding?.type !== "durable_object_namespace" ||
      binding.class_name !== className ||
      !/^[0-9a-f]{32}$/.test(String(binding.namespace_id ?? ""))) {
    throw new Error(`deployed Worker version has the wrong ${name} Durable Object binding`);
  }
}

function exactKVBinding(bindings, name, namespaceID) {
  const binding = bindings.get(name);
  if (binding?.type !== "kv_namespace" ||
      binding.namespace_id !== namespaceID) {
    throw new Error(`deployed Worker version has the wrong ${name} KV binding`);
  }
}

function exactR2Binding(bindings, name, bucketName) {
  const binding = bindings.get(name);
  if (binding?.type !== "r2_bucket" || binding.bucket_name !== bucketName) {
    throw new Error(`deployed Worker version has the wrong ${name} R2 binding`);
  }
}

function exactNamedHandlers(namedHandlers) {
  if (!Array.isArray(namedHandlers)) {
    throw new Error("deployed Worker version is missing its named handlers");
  }
  const actual = new Map();
  for (const handler of namedHandlers) {
    if (!isRecord(handler) || typeof handler.name !== "string" ||
        actual.has(handler.name) || !sameJSON(handler.handlers, ["class"])) {
      throw new Error("deployed Worker version has invalid named handlers");
    }
    actual.set(handler.name, handler);
  }
  if (!sameJSON([...actual.keys()].sort(), [...NAMED_HANDLER_CLASSES].sort())) {
    throw new Error("deployed Worker version named handler classes did not match");
  }
}

// Cloudflare's version resource began returning the Worker's own name inside
// each runtime container entry (observed 2026-09-05). Accept exactly one
// container for the Backend class whose optional name, when present, is this
// Worker's name; any other key, class, or count still fails the contract.
function exactRuntimeContainers(containers, scriptName) {
  if (!Array.isArray(containers) || containers.length !== 1) return false;
  const entry = containers[0];
  if (!isRecord(entry) || entry.class_name !== "Backend") return false;
  const keys = Object.keys(entry).sort();
  if (sameJSON(keys, ["class_name"])) return true;
  return sameJSON(keys, ["class_name", "name"]) && entry.name === scriptName;
}

export function verifyWorkerVersion(version, expected, expectedVersionID, {
  allowLegacyEmptyManagedDeliveryCohort = false,
} = {}) {
  if (version == null || typeof version !== "object" ||
      version.id !== expectedVersionID || !validVersionID(version.id)) {
    throw new Error("Wrangler returned the wrong control-plane Worker version");
  }
  if (version.metadata?.source !== "wrangler" ||
      version.annotations?.["workers/triggered_by"] !== "upload" ||
      version.annotations?.["workers/tag"] !== workerVersionTag(expected) ||
      version.annotations?.["workers/message"] !== workerVersionMessage(expected)) {
    throw new Error("deployed Worker version has the wrong release annotations");
  }
  const script = version.resources?.script;
  if (!isRecord(script) ||
      !/^[0-9a-f]{64}$/.test(String(script.etag ?? ""))) {
    throw new Error("deployed Worker version is missing its immutable script etag");
  }
  if (!sameJSON(script.handlers, ["fetch", "scheduled"])) {
    throw new Error("deployed Worker version handlers did not match fetch and scheduled");
  }
  exactNamedHandlers(script.named_handlers);

  const runtime = version.resources?.script_runtime;
  const runtimeKeys = isRecord(runtime) ? Object.keys(runtime).sort() : [];
  const allowedRuntimeKeys = [
    "compatibility_date",
    "containers",
    "limits",
    "migration_tag",
    "usage_model",
  ];
  if (Object.hasOwn(runtime ?? {}, "compatibility_flags")) {
    allowedRuntimeKeys.push("compatibility_flags");
  }
  if (!isRecord(runtime) ||
      !sameJSON(runtimeKeys, allowedRuntimeKeys.sort()) ||
      runtime.compatibility_date !== COMPATIBILITY_DATE ||
      runtime.migration_tag !== MIGRATION_TAG ||
      runtime.usage_model !== "standard" ||
      !sameJSON(runtime.limits, { cpu_ms: CPU_LIMIT_MS }) ||
      !exactRuntimeContainers(runtime.containers, "witself-control-plane") ||
      (Object.hasOwn(runtime, "compatibility_flags") &&
       !sameJSON(runtime.compatibility_flags, []))) {
    throw new Error("deployed Worker version runtime contract did not match");
  }

  const bindings = bindingsByName(version.resources?.bindings);
  const nonSecretNames = new Set([
    ...Object.keys(DURABLE_OBJECT_BINDINGS),
    ...Object.keys(R2_BINDINGS),
    "AGENT_EMAIL_DIRECTORY",
    "AGENT_EMAIL_DOMAIN",
    "AGENT_EMAIL_LEGACY_DOMAINS",
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT",
    "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "CP_SIGNUP_DAILY_LIMIT_GLOBAL",
    "CP_SIGNUP_DAILY_LIMIT_PER_IP",
    "CP_SIGNUP_OPEN",
    "CP_SUPPORT_EMAIL_INTAKE_ENABLED",
    "CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED",
    "CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT",
    "CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM",
    "DIRECTORY",
    "EMAIL",
    "PUBLIC_IP_LIMITER",
    "RECOVER_LIMITER",
    "SIGNUP_IP_LIMITER",
    "WITSELF_EDGE_RELEASE_COMMIT",
    "WITSELF_EDGE_RELEASE_DATE",
    "WITSELF_EDGE_RELEASE_VERSION",
  ]);
  for (const binding of bindings.values()) {
    if (binding.type !== "secret_text" && !nonSecretNames.has(binding.name)) {
      throw new Error(
        `deployed Worker version has unexpected non-secret binding ${binding.name}`,
      );
    }
  }

  const actual = {
    service: "witself-control-plane",
    version: bindings.get("WITSELF_EDGE_RELEASE_VERSION")?.text,
    commit: bindings.get("WITSELF_EDGE_RELEASE_COMMIT")?.text,
    date: bindings.get("WITSELF_EDGE_RELEASE_DATE")?.text,
  };
  exactPlainBinding(bindings, "WITSELF_EDGE_RELEASE_VERSION", expected.version);
  exactPlainBinding(bindings, "WITSELF_EDGE_RELEASE_COMMIT", expected.commit);
  exactPlainBinding(bindings, "WITSELF_EDGE_RELEASE_DATE", expected.date);
  if (!deploymentMatches(actual, expected)) {
    throw new Error("deployed Worker version has the wrong release identity");
  }
  if (!/^[a-z][a-z0-9_-]{0,63}$/.test(expected.route_signing_key_id) ||
      bindings.get("AGENT_EMAIL_ROUTE_SIGNING_KEY_ID")?.text !==
        expected.route_signing_key_id) {
    throw new Error("deployed Worker version has the wrong route signing key id");
  }
  exactPlainBinding(
    bindings,
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    expected.route_signing_key_id,
  );
  exactSecretBinding(bindings, "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY");
  exactSecretBinding(bindings, "CONTROL_PLANE_EDGE_TOKEN");
  exactSecretBinding(bindings, "SUPPORT_EMAIL_INTAKE_TOKEN");

  for (const [name, className] of Object.entries(DURABLE_OBJECT_BINDINGS)) {
    exactDurableObjectBinding(bindings, name, className);
  }
  if (!/^[0-9a-f]{32}$/.test(String(expected.agent_email_directory_id ?? ""))) {
    throw new Error("expected agent email directory id was invalid");
  }
  exactKVBinding(bindings, "DIRECTORY", DIRECTORY_NAMESPACE_ID);
  exactKVBinding(
    bindings,
    "AGENT_EMAIL_DIRECTORY",
    expected.agent_email_directory_id,
  );
  for (const [name, value] of [
    ["AGENT_EMAIL_DOMAIN", "witmail.net"],
    ["AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"],
    ["CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM", "8"],
    ["CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT", "64"],
    ["CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT", "8"],
    ["CP_SIGNUP_DAILY_LIMIT_PER_IP", "10"],
    ["CP_SIGNUP_DAILY_LIMIT_GLOBAL", "500"],
    ["CP_SIGNUP_OPEN", "true"],
    ["CP_SUPPORT_EMAIL_INTAKE_ENABLED", "false"],
    ["CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED", "false"],
  ]) {
    exactPlainBinding(bindings, name, value);
  }
  const legacyManagedDeliveryCohort =
    allowLegacyEmptyManagedDeliveryCohort === true &&
    expected.version === "0.0.240" &&
    expected.managed_delivery_account_allowlist === "" &&
    !bindings.has("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST");
  if (!legacyManagedDeliveryCohort) {
    exactPlainBinding(
      bindings,
      "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
      expected.managed_delivery_account_allowlist,
    );
  }
  for (const [name, bucketName] of Object.entries(R2_BINDINGS)) {
    exactR2Binding(bindings, name, bucketName);
  }
  if (bindings.get("EMAIL")?.type !== "send_email") {
    throw new Error("deployed Worker version has the wrong EMAIL send binding");
  }
  exactRateLimitBinding(
    bindings,
    "RECOVER_LIMITER",
    "1001",
    { limit: 1, period: 10 },
  );
  exactRateLimitBinding(
    bindings,
    "PUBLIC_IP_LIMITER",
    "1002",
    { limit: 300, period: 60 },
  );
  exactRateLimitBinding(
    bindings,
    "SIGNUP_IP_LIMITER",
    "1003",
    { limit: 5, period: 60 },
  );
  return Object.freeze({
    version_id: version.id,
    script_etag: script.etag,
    managed_delivery_cohort: {
      account_count: parseManagedDeliveryAccountAllowlist(
        expected.managed_delivery_account_allowlist,
      ).length,
      allowlist_sha256: createHash("sha256")
        .update(expected.managed_delivery_account_allowlist)
        .digest("hex"),
    },
  });
}

export function wranglerJSON(
  args,
  operation,
  environment = process.env,
  run = spawnSync,
  {
    cwd = root,
    reviewedEnvironmentFile,
  } = {},
) {
  assertProductionCloudflareIdentity(environment);
  const result = run("wrangler", withReviewedWranglerEnvironmentFile(
    args,
    reviewedEnvironmentFile,
  ), {
    cwd,
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
    throw new Error(`Wrangler ${operation} output was not valid JSON`);
  }
}

export function verifyCurrentWorkerDeployment(
  expected,
  config,
  inspect = wranglerJSON,
) {
  const deployment = inspect([
    "deployments", "status",
    "--config", config,
    "--name", expected.service,
    "--json",
  ], "inspect the current control-plane deployment");
  const versionID = currentProductionVersionID(deployment);
  const version = inspect([
    "versions", "view", versionID,
    "--config", config,
    "--name", expected.service,
    "--json",
  ], "inspect the current control-plane Worker version");
  const finalDeployment = inspect([
    "deployments", "status",
    "--config", config,
    "--name", expected.service,
    "--json",
  ], "reinspect the current control-plane deployment");
  if (currentProductionVersionID(finalDeployment) !== versionID) {
    throw new Error(
      "control-plane deployment changed during exact provider inspection",
    );
  }
  return verifyWorkerVersion(version, expected, versionID);
}

function parseArgs(argv) {
  const out = {
    config: generatedConfigPath,
    expectedMain: "src/index.js",
    wranglerCwd: root,
    reviewedEnvironmentFile: undefined,
    endpoint: process.env.WITSELF_CONTROL_PLANE ?? "https://self.witwave.ai",
    // Container-backed Worker revisions can take several minutes to replace
    // their live instances after the Worker upload has completed.
    attempts: 120,
    delayMs: 5000,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const name = argv[i];
    if (!["--config", "--endpoint", "--attempts", "--delay-ms",
      "--wrangler-cwd", "--reviewed-env-file"].includes(name)) {
      throw new Error(`unknown argument ${name}`);
    }
    const value = argv[++i];
    if (!value) throw new Error(`${name} requires a value`);
    switch (name) {
    case "--config":
      out.config = resolve(root, value);
      if (out.config !== generatedConfigPath) {
        out.expectedMain = privateDeploymentConfigMain(out.config);
      }
      break;
    case "--endpoint":
      out.endpoint = value;
      break;
    case "--wrangler-cwd":
      out.wranglerCwd = resolve(value);
      if (out.wranglerCwd !== value) {
        throw new Error("--wrangler-cwd must be a normalized absolute path");
      }
      break;
    case "--reviewed-env-file":
      out.reviewedEnvironmentFile = resolve(value);
      if (out.reviewedEnvironmentFile !== value) {
        throw new Error("--reviewed-env-file must be a normalized absolute path");
      }
      break;
    case "--attempts":
      out.attempts = Number(value);
      break;
    case "--delay-ms":
      out.delayMs = Number(value);
      break;
    }
  }
  if (!Number.isInteger(out.attempts) || out.attempts < 1 || out.attempts > 120) {
    throw new Error("--attempts must be an integer from 1 through 120");
  }
  if (!Number.isInteger(out.delayMs) || out.delayMs < 0 || out.delayMs > 30000) {
    throw new Error("--delay-ms must be an integer from 0 through 30000");
  }
  return out;
}

async function sleep(delayMs) {
  if (delayMs === 0) return;
  await new Promise((resolvePromise) => setTimeout(resolvePromise, delayMs));
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const expected = expectedBuildMetadata(
    await readFile(args.config, "utf8"),
    args.expectedMain,
  );
  const inspect = (wranglerArgs, operation) => wranglerJSON(
    wranglerArgs,
    operation,
    process.env,
    spawnSync,
    {
      cwd: args.wranglerCwd,
      reviewedEnvironmentFile: args.reviewedEnvironmentFile,
    },
  );
  const attestation = verifyCurrentWorkerDeployment(
    expected,
    args.config,
    inspect,
  );
  process.stdout.write(
    `verified outer Worker ${attestation.version_id} (${attestation.script_etag})\n`,
  );
  const url = `${args.endpoint.replace(/\/+$/, "")}/v1/version`;
  let last = "no response";
  for (let attempt = 1; attempt <= args.attempts; attempt += 1) {
    try {
      const response = await fetch(url, {
        headers: { Accept: "application/json" },
        signal: AbortSignal.timeout(10000),
      });
      if (response.ok) {
        const actual = await response.json();
        if (deploymentMatches(actual, expected)) {
          process.stdout.write(
            `verified ${expected.service} ${expected.version} (${expected.commit})\n`,
          );
          return;
        }
        last = `identity mismatch: ${JSON.stringify(actual)}`;
      } else {
        last = `HTTP ${response.status}`;
      }
    } catch (error) {
      last = error?.name === "TimeoutError" ? "request timed out" : "request failed";
    }
    if (attempt < args.attempts) await sleep(args.delayMs);
  }
  throw new Error(
    `deployment did not report the rendered build identity after ${args.attempts} attempts (${last})`,
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
