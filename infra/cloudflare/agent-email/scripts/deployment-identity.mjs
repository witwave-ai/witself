#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  assertReleaseSource,
  sourceIdentity,
} from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";
import { PRODUCTION_RECEIVE_WORKER } from "../src/worker-names.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";

const WORKER_NAME = PRODUCTION_RECEIVE_WORKER;
const CONTROL_PLANE_URL = "https://self.witwave.ai/";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const OPAQUE_ETAG = /^[0-9A-Za-z._:-]{16,256}$/;

export function releaseMessage(release) {
  return `Witself v${release.version} agent-email edge ${release.commit}`;
}

function requiredBoolean(value, name) {
  if (value !== "true" && value !== "false") {
    throw new Error(`${name} must be explicitly true or false`);
  }
  return value;
}

function bindingsByName(bindings) {
  if (!Array.isArray(bindings)) throw new Error("Worker version bindings were invalid");
  const result = new Map();
  for (const binding of bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || !binding.name || result.has(binding.name)) {
      throw new Error("Worker version bindings were invalid");
    }
    result.set(binding.name, binding);
  }
  return result;
}

function exactNames(actual, expected) {
  const actualNames = [...actual.keys()].sort();
  const expectedNames = [...expected].sort();
  if (JSON.stringify(actualNames) !== JSON.stringify(expectedNames)) {
    throw new Error("Worker version binding inventory did not match the reviewed contract");
  }
}

function plain(bindings, name, expected) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || binding.text !== expected) {
    throw new Error(`Worker version binding ${name} did not match`);
  }
}

function secret(bindings, name) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    throw new Error(`Worker version secret binding ${name} was missing or invalid`);
  }
}

export function expectedDeployment(env, release) {
  const directoryID = String(env.EMAIL_DIRECTORY_KV_ID ?? "").trim();
  if (!/^[0-9a-f]{32}$/.test(directoryID)) {
    throw new Error("EMAIL_DIRECTORY_KV_ID must be a 32-character lowercase hex id");
  }
  const relayKeyID = String(env.RELAY_KEY_ID ?? "").trim().toLowerCase();
  if (!/^[a-z][a-z0-9_-]{0,63}$/.test(relayKeyID)) {
    throw new Error("RELAY_KEY_ID is missing or invalid");
  }
  let routePublicKeys;
  try {
    routePublicKeys = JSON.parse(String(
      env.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS ?? "",
    ));
  } catch {
    throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS is missing or invalid");
  }
  if (!routePublicKeys || typeof routePublicKeys !== "object" ||
      Array.isArray(routePublicKeys)) {
    throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS is missing or invalid");
  }
  const routePublicKeyEntries = Object.entries(routePublicKeys)
    .sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0);
  if (routePublicKeyEntries.length < 1 || routePublicKeyEntries.length > 4 ||
      routePublicKeyEntries.some(([keyID, encoded]) =>
        !/^[a-z][a-z0-9_-]{0,63}$/.test(keyID) ||
        typeof encoded !== "string" ||
        !/^[A-Za-z0-9+/]{43}=$/.test(encoded) ||
        Buffer.from(encoded, "base64").byteLength !== 32 ||
        Buffer.from(encoded, "base64").toString("base64") !== encoded)) {
    throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS is missing or invalid");
  }
  const rawControlPlaneURL = String(env.CONTROL_PLANE_URL ?? "");
  const managedDeliveryAccountAllowlist = String(
    env.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST ?? "",
  );
  const managedDeliveryAccounts = parseManagedDeliveryAccountAllowlist(
    managedDeliveryAccountAllowlist,
  );
  let controlPlaneURL;
  try {
    controlPlaneURL = new URL(rawControlPlaneURL);
  } catch {
    throw new Error("CONTROL_PLANE_URL is missing or invalid");
  }
  if (rawControlPlaneURL !== CONTROL_PLANE_URL ||
      controlPlaneURL.toString() !== CONTROL_PLANE_URL) {
    throw new Error("CONTROL_PLANE_URL is missing or invalid");
  }
  return Object.freeze({
    release,
    directoryID,
    relayKeyID,
    controlPlaneURL: controlPlaneURL.toString(),
    routePublicKeys: JSON.stringify(Object.fromEntries(routePublicKeyEntries)),
    routeSigningKeyIDs: routePublicKeyEntries.map(([keyID]) => keyID),
    managedDeliveryAccountAllowlist,
    managedDeliveryAccountCount: managedDeliveryAccounts.length,
    managedDeliveryAllowlistSHA256: createHash("sha256")
      .update(managedDeliveryAccountAllowlist)
      .digest("hex"),
    aliasDeliveryEnabled: requiredBoolean(
      env.REALM_EMAIL_ALIAS_DELIVERY_ENABLED,
      "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    ),
    canonicalDeliveryEnabled: requiredBoolean(
      env.REALM_EMAIL_CANONICAL_DELIVERY_ENABLED,
      "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    ),
  });
}

export function verifyDeployment(status, version, expected, {
  requireAnnotations = true,
} = {}) {
  if (!status || typeof status !== "object" || Array.isArray(status) ||
      !UUID.test(String(status.id ?? "")) || status.strategy !== "percentage" ||
      !Array.isArray(status.versions) || status.versions.length !== 1 ||
      status.versions[0]?.percentage !== 100 ||
      !UUID.test(String(status.versions[0]?.version_id ?? ""))) {
    throw new Error("production Worker deployment was not one version at 100 percent");
  }
  const activeVersionID = status.versions[0].version_id;
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== activeVersionID || !Number.isSafeInteger(version.number) ||
      version.number < 1) {
    throw new Error("active Worker version identity was invalid");
  }
  if (requireAnnotations &&
      (version.annotations?.["workers/tag"] !== expected.release.tag ||
       version.annotations?.["workers/message"] !== releaseMessage(expected.release))) {
    throw new Error("active Worker version annotations did not match the release");
  }

  const script = version.resources?.script;
  if (!script || !OPAQUE_ETAG.test(String(script.etag ?? "")) ||
      JSON.stringify(script.handlers) !== JSON.stringify(["email"])) {
    throw new Error("active Worker script was not the reviewed email-only artifact");
  }
  const runtime = version.resources?.script_runtime;
  if (!runtime || runtime.compatibility_date !== "2026-07-21" ||
      JSON.stringify(runtime.compatibility_flags) !==
        JSON.stringify(["global_fetch_strictly_public"])) {
    throw new Error("active Worker runtime contract did not match");
  }

  const bindings = bindingsByName(version.resources?.bindings);
  const names = new Set([
    "AGENT_EMAIL_DOMAIN",
    "AGENT_EMAIL_LEGACY_DOMAINS",
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS",
    "CONTROL_PLANE_EDGE_TOKEN",
    "CONTROL_PLANE_URL",
    "EMAIL_DIRECTORY",
    "EMAIL_EDGE_METRICS",
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    "REALM_ROUTE_COLD_MISS_LIMITER",
    "REALM_ROUTE_KNOWN_MISS_LIMITER",
    "RELAY_ED25519_PRIVATE_KEY",
    "RELAY_KEY_ID",
    "WITSELF_EDGE_RELEASE_COMMIT",
    "WITSELF_EDGE_RELEASE_DATE",
    "WITSELF_EDGE_RELEASE_VERSION",
  ]);
  exactNames(bindings, names);
  plain(bindings, "AGENT_EMAIL_DOMAIN", "witmail.net");
  plain(bindings, "AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai");
  plain(
    bindings,
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    expected.managedDeliveryAccountAllowlist,
  );
  plain(bindings, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", expected.routePublicKeys);
  plain(bindings, "CONTROL_PLANE_URL", expected.controlPlaneURL);
  plain(bindings, "RELAY_KEY_ID", expected.relayKeyID);
  plain(bindings, "REALM_EMAIL_ALIAS_DELIVERY_ENABLED", expected.aliasDeliveryEnabled);
  plain(bindings, "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", expected.canonicalDeliveryEnabled);
  plain(bindings, "WITSELF_EDGE_RELEASE_VERSION", expected.release.version);
  plain(bindings, "WITSELF_EDGE_RELEASE_COMMIT", expected.release.commit);
  plain(bindings, "WITSELF_EDGE_RELEASE_DATE", expected.release.date);
  secret(bindings, "CONTROL_PLANE_EDGE_TOKEN");
  secret(bindings, "RELAY_ED25519_PRIVATE_KEY");

  const directory = bindings.get("EMAIL_DIRECTORY");
  if (directory?.type !== "kv_namespace" || directory.namespace_id !== expected.directoryID) {
    throw new Error("active Worker directory binding did not match");
  }
  const metrics = bindings.get("EMAIL_EDGE_METRICS");
  if (metrics?.type !== "analytics_engine" || metrics.dataset !== "witself_agent_email_edge") {
    throw new Error("active Worker metrics binding did not match");
  }
  for (const [name, namespaceID, limit] of [
    ["REALM_ROUTE_COLD_MISS_LIMITER", "2201", 10],
    ["REALM_ROUTE_KNOWN_MISS_LIMITER", "2202", 100],
  ]) {
    const limiter = bindings.get(name);
    if (limiter?.type !== "ratelimit" || limiter.namespace_id !== namespaceID ||
        limiter.simple?.limit !== limit || limiter.simple?.period !== 10) {
      throw new Error(`active Worker rate-limit binding ${name} did not match`);
    }
  }

  return Object.freeze({
    schema: "witself.agent-email-edge-attestation.v1",
    outcome: "verified",
    release: {
      version: expected.release.version,
      commit: expected.release.commit,
      date: expected.release.date,
    },
    cloudflare: {
      deployment_id: status.id,
      version_id: version.id,
      version_number: version.number,
      provider_script_etag: script.etag,
      created_on: version.metadata?.created_on ?? "",
    },
    runtime: {
      handlers: ["email"],
      compatibility_date: runtime.compatibility_date,
      compatibility_flags: runtime.compatibility_flags,
    },
    bindings: {
      directory_namespace_id: expected.directoryID,
      metrics_dataset: metrics.dataset,
      alias_delivery_enabled: expected.aliasDeliveryEnabled === "true",
      canonical_delivery_enabled: expected.canonicalDeliveryEnabled === "true",
      managed_delivery_cohort: {
        account_count: expected.managedDeliveryAccountCount,
        allowlist_sha256: expected.managedDeliveryAllowlistSHA256,
      },
      custom_domain_delivery_enabled: false,
      route_signing_key_ids: expected.routeSigningKeyIDs,
    },
  });
}

function wranglerJSON(args) {
  assertProductionCloudflareIdentity(process.env);
  const result = spawnSync("wrangler", withReviewedWranglerEnvironmentFile(args), {
    env: sanitizedWranglerInspectionEnvironment(process.env),
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error || result.status !== 0) {
    throw new Error("could not inspect the production email Worker deployment");
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error("Wrangler returned malformed deployment JSON");
  }
}

export function verifyProduction({
  env = process.env,
  inspect = wranglerJSON,
  requireAnnotations = false,
  release = null,
} = {}) {
  if (typeof inspect !== "function") {
    throw new Error("production Worker deployment inspector was invalid");
  }
  const targetRelease = release == null
    ? sourceIdentity({ requireRelease: true })
    : assertReleaseSource(release);
  const expected = expectedDeployment(env, targetRelease);
  const status = inspect([
    "deployments", "status", "--name", WORKER_NAME, "--json",
  ]);
  const versionID = status?.versions?.length === 1
    ? String(status.versions[0]?.version_id ?? "")
    : "";
  if (!UUID.test(versionID)) {
    throw new Error("production Worker deployment did not identify one active version");
  }
  const version = inspect([
    "versions", "view", versionID, "--name", WORKER_NAME, "--json",
  ]);
  const attestation = verifyDeployment(
    status,
    version,
    expected,
    { requireAnnotations },
  );
  const finalStatus = inspect([
    "deployments", "status", "--name", WORKER_NAME, "--json",
  ]);
  verifyDeployment(finalStatus, version, expected, { requireAnnotations });
  if (finalStatus.id !== status.id) {
    throw new Error(
      "production Worker deployment changed during exact provider inspection",
    );
  }
  return attestation;
}

function parseArgs(argv) {
  let requireAnnotations = false;
  for (const argument of argv) {
    if (argument === "--require-annotations") requireAnnotations = true;
    else throw new Error(`unknown argument ${argument}`);
  }
  return { requireAnnotations };
}

function main() {
  const options = parseArgs(process.argv.slice(2));
  const attestation = verifyProduction(options);
  process.stdout.write(`${JSON.stringify(attestation, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
