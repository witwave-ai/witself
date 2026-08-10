#!/usr/bin/env node
import { createHash } from "node:crypto";
import { writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import { verifyDeployment } from "./deployment-identity.mjs";
import { reserveJSONReceipt } from "./receipt-journal.mjs";
import {
  activeBindings,
  activeVersionID,
  assertRemoteDark,
  assertSecretBinding,
  createSpawnRuntime,
  inspectWorker,
  parseJSONC,
  putWorkerSecret,
  revealSecret,
  routeSigningOperationsLeaseOrigin,
  sanitizedWitselfEnvironment,
  sanitizedWranglerEnvironment,
  sanitizedWranglerInspectionEnvironment,
  secretInventory,
  showSecret,
  validateProvisioningConfigs,
  validateRevealEnvelope,
  validateSecretShowEnvelope,
  verifyEd25519Keypair,
} from "./provision-route-signing-secrets.mjs";
import { canonicalJSON, primaryRoutingInternals, sha256 } from
  "./primary-routing-lib.mjs";
import {
  withAgentEmailOperationsLease,
} from "../../control-plane/scripts/agent-email-operations-lease-client.mjs";
import {
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";
import {
  verifyWorkerVersion as verifyControlPlaneWorkerVersion,
} from "../../control-plane/scripts/verify-deployment.mjs";
import {
  createPrivateDeploymentConfig,
} from "../../control-plane/scripts/private-deployment-config.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const CONTROL_PLANE_ROOT = resolve(root, "../control-plane");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const RELAY_PRIVATE_SECRET = "RELAY_ED25519_PRIVATE_KEY";
const OPERATIONS_LEASE_OPERATION = "relay_signing_key_provision";
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const PUBLIC_KEY = /^[A-Za-z0-9+/]{43}=$/;
const VERSION = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const COMMIT = /^[0-9a-f]{40}$/;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const PROVIDER_RULE_PREFIXES = Object.freeze([
  "witself-agent-email-pilot:",
  "witself-agent-email-primary-canary:",
]);
const PROVIDER_ZONE_CONTRACT = Object.freeze({
  "witmail.net": "primary",
  "witwave.ai": "legacy",
});
const PRIMARY_PROVIDER_ZONE_CONTRACT = Object.freeze({
  "witmail.net": "primary",
});
const HEX_32 = /^[0-9a-f]{32}$/;
const SHA256 = /^[0-9a-f]{64}$/;

function fail(message) {
  throw new Error(message);
}

function selector(value, label) {
  if (typeof value !== "string" || value.length < 1 || value.length > 256 ||
      value !== value.trim() || value.startsWith("-") ||
      /[\x00-\x1f\x7f]/.test(value)) {
    fail(`${label} is invalid`);
  }
  return value;
}

function identityName(value, label, fallback = "") {
  const candidate = String(value ?? fallback);
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(candidate)) {
    fail(`${label} is missing or invalid`);
  }
  return candidate;
}

function optionValue(argv, index, name) {
  const value = argv[index + 1];
  if (typeof value !== "string" || value === "") fail(`${name} requires a value`);
  return value;
}

export function parseRelayProvisioningArgs(argv, env = process.env) {
  const out = {
    controlPlaneConfig: join(CONTROL_PLANE_ROOT, "wrangler.generated.jsonc"),
    emailEdgeConfig: join(root, "wrangler.generated.jsonc"),
    account: String(env.WITSELF_ACCOUNT ?? "default"),
    realm: String(env.WITSELF_REALM ?? "default"),
    agent: String(env.WITSELF_AGENT ?? ""),
    endpoint: "",
    tokenFile: "",
    receipt: "",
    relaySecret: "",
    relayKeyIDField: "",
    relayPublicField: "",
    relayPrivateField: "",
    providerZoneName: "witmail.net",
  };
  const names = new Map([
    ["--control-plane-config", "controlPlaneConfig"],
    ["--email-edge-config", "emailEdgeConfig"],
    ["--account", "account"],
    ["--realm", "realm"],
    ["--agent", "agent"],
    ["--endpoint", "endpoint"],
    ["--token-file", "tokenFile"],
    ["--receipt", "receipt"],
    ["--relay-secret", "relaySecret"],
    ["--relay-key-id-field", "relayKeyIDField"],
    ["--relay-public-field", "relayPublicField"],
    ["--relay-private-field", "relayPrivateField"],
    ["--provider-zone-name", "providerZoneName"],
  ]);
  if (argv.length % 2 !== 0) fail("relay signing provisioning arguments are incomplete");
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const property = names.get(name);
    if (property == null) fail(`unknown argument ${name ?? ""}`.trim());
    out[property] = optionValue(argv, index, name);
  }
  for (const [property, label] of [
    ["relaySecret", "--relay-secret"],
    ["relayKeyIDField", "--relay-key-id-field"],
    ["relayPublicField", "--relay-public-field"],
    ["relayPrivateField", "--relay-private-field"],
  ]) {
    out[property] = selector(out[property], label);
  }
  out.account = identityName(out.account, "--account", "default");
  out.realm = identityName(out.realm, "--realm", "default");
  out.agent = identityName(out.agent, "--agent");
  if (!Object.hasOwn(PROVIDER_ZONE_CONTRACT, out.providerZoneName)) {
    fail("--provider-zone-name must be witmail.net or witwave.ai");
  }
  for (const property of ["controlPlaneConfig", "emailEdgeConfig", "tokenFile"]) {
    if (out[property] !== "") {
      out[property] = isAbsolute(out[property])
        ? resolve(out[property])
        : resolve(root, out[property]);
    }
  }
  if (!isAbsolute(out.receipt) || resolve(out.receipt) !== out.receipt) {
    fail("--receipt must be one canonical absolute path");
  }
  if (out.controlPlaneConfig === out.emailEdgeConfig) {
    fail("control-plane and email-edge config paths must be distinct");
  }
  if (out.endpoint !== "") {
    let parsed;
    try {
      parsed = new URL(out.endpoint);
    } catch {
      fail("--endpoint must be a credential-free HTTPS URL");
    }
    if (parsed.protocol !== "https:" || parsed.username || parsed.password ||
        parsed.search || parsed.hash || !parsed.hostname) {
      fail("--endpoint must be a credential-free HTTPS URL");
    }
  }
  return Object.freeze(out);
}

function releaseAtLeast241(value) {
  if (!VERSION.test(value)) return false;
  const [major, minor, patch] = value.split(".").map(Number);
  return major > 0 || minor > 0 || patch >= 241;
}

function targetRelease(vars, label) {
  const release = Object.freeze({
    version: vars?.WITSELF_EDGE_RELEASE_VERSION,
    commit: vars?.WITSELF_EDGE_RELEASE_COMMIT,
    date: vars?.WITSELF_EDGE_RELEASE_DATE,
  });
  if (typeof release.version !== "string" ||
      !releaseAtLeast241(release.version) ||
      typeof release.commit !== "string" || !COMMIT.test(release.commit) ||
      typeof release.date !== "string" || !RFC3339.test(release.date) ||
      !Number.isFinite(Date.parse(release.date))) {
    fail(`${label} target release identity must be v0.0.241 or newer`);
  }
  return release;
}

function exactKVNamespace(config, binding, label) {
  if (!Array.isArray(config.kv_namespaces)) {
    fail(`${label} target ${binding} namespace is invalid`);
  }
  const matches = config.kv_namespaces.filter((item) =>
    item?.binding === binding);
  if (matches.length !== 1 || !HEX_32.test(String(matches[0]?.id ?? ""))) {
    fail(`${label} target ${binding} namespace is invalid`);
  }
  return matches[0].id;
}

export function validateRelayProvisioningConfigs(controlPlaneRaw, emailEdgeRaw) {
  const routeTarget = validateProvisioningConfigs(controlPlaneRaw, emailEdgeRaw);
  const controlPlane = parseJSONC(
    controlPlaneRaw,
    "control-plane configuration",
  );
  const emailEdge = parseJSONC(emailEdgeRaw, "email-edge configuration");
  if (Object.hasOwn(controlPlane, "account_id") ||
      Object.hasOwn(emailEdge, "account_id")) {
    fail("generated Worker configs must not override the provider account");
  }
  const keyID = emailEdge.vars?.RELAY_KEY_ID;
  if (typeof keyID !== "string" || !KEY_ID.test(keyID)) {
    fail("email-edge target relay key id is invalid");
  }
  const controlPlaneRelease = targetRelease(
    controlPlane.vars,
    "control-plane",
  );
  const release = targetRelease(emailEdge.vars, "email-edge");
  if (canonicalJSON(controlPlaneRelease) !== canonicalJSON(release)) {
    fail("control-plane and email-edge target release identities differ");
  }
  if (controlPlane.vars?.AGENT_EMAIL_DOMAIN !== "witmail.net" ||
      controlPlane.vars?.AGENT_EMAIL_LEGACY_DOMAINS !==
        "agent-mail.witwave.ai" ||
      emailEdge.vars?.AGENT_EMAIL_DOMAIN !== "witmail.net" ||
      emailEdge.vars?.AGENT_EMAIL_LEGACY_DOMAINS !==
        "agent-mail.witwave.ai") {
    fail("target managed-email domain contract is invalid");
  }
  const controlPlaneDirectoryID = exactKVNamespace(
    controlPlane,
    "AGENT_EMAIL_DIRECTORY",
    "control-plane",
  );
  const emailEdgeDirectoryID = exactKVNamespace(
    emailEdge,
    "EMAIL_DIRECTORY",
    "email-edge",
  );
  if (controlPlaneDirectoryID !== emailEdgeDirectoryID) {
    fail("target route-directory namespaces differ");
  }
  const controlPlaneExpected = Object.freeze({
    service: CONTROL_PLANE_WORKER,
    ...release,
    route_signing_key_id: routeTarget.keyID,
    agent_email_directory_id: controlPlaneDirectoryID,
    managed_delivery_account_allowlist: "",
  });
  const emailEdgeExpected = Object.freeze({
    release: Object.freeze({ ...release, tag: `v${release.version}` }),
    directoryID: emailEdgeDirectoryID,
    controlPlaneURL: emailEdge.vars?.CONTROL_PLANE_URL,
    routePublicKeys: emailEdge.vars?.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS,
    managedDeliveryAccountAllowlist: "",
    managedDeliveryAccountCount: 0,
    managedDeliveryAllowlistSHA256: sha256(""),
    aliasDeliveryEnabled: "false",
    canonicalDeliveryEnabled: "false",
  });
  return Object.freeze({
    keyID,
    release,
    controlPlaneExpected,
    emailEdgeExpected,
    providerZoneContract: PROVIDER_ZONE_CONTRACT,
    configFence: Object.freeze({
      control_plane_sha256: sha256(controlPlaneRaw),
      email_edge_sha256: sha256(emailEdgeRaw),
    }),
  });
}

export function validateRelaySecretMetadata(envelope, secretSelector, fields, targetKeyID) {
  const shown = validateSecretShowEnvelope(envelope, secretSelector, fields);
  const keyID = shown.fields.keyID;
  const publicKey = shown.fields.public;
  const privateKey = shown.fields.private;
  if (new Set([keyID.id, publicKey.id, privateKey.id]).size !== 3 ||
      keyID.kind !== "text" || keyID.sensitive || keyID.redacted ||
      keyID.encoding !== "utf8" || keyID.public_value !== targetKeyID ||
      !KEY_ID.test(String(keyID.public_value ?? "")) ||
      publicKey.kind !== "text" || publicKey.sensitive || publicKey.redacted ||
      publicKey.encoding !== "utf8" ||
      !PUBLIC_KEY.test(String(publicKey.public_value ?? "")) ||
      Buffer.from(publicKey.public_value, "base64").byteLength !== 32 ||
      Buffer.from(publicKey.public_value, "base64").toString("base64") !==
        publicKey.public_value ||
      privateKey.kind !== "private_key" || !privateKey.sensitive ||
      !privateKey.redacted || privateKey.encoding !== "utf8") {
    fail("Witself relay signing field policy does not match the provisioning contract");
  }
  return Object.freeze({
    secret: shown.secret,
    keyID: keyID.public_value,
    publicKey: publicKey.public_value,
    privateField: privateKey,
  });
}

function providerRuleID(rule) {
  return typeof rule?.id === "string" && /^[0-9a-f]{1,32}$/.test(rule.id)
    ? rule.id
    : fail("Cloudflare Email Routing rule inventory was invalid");
}

function providerRuleTargetsEmailEdge(actions) {
  if (!Array.isArray(actions) || actions.length < 1 || actions.length > 16) {
    fail("Cloudflare Email Routing rule inventory was invalid");
  }
  let targetsEmailEdge = false;
  for (const action of actions) {
    if (action == null || Array.isArray(action) || typeof action !== "object" ||
        typeof action.type !== "string" ||
        !/^[a-z][a-z0-9_]{0,63}$/.test(action.type) ||
        !Array.isArray(action.value) || action.value.length > 256 ||
        action.value.some((value) =>
          typeof value !== "string" || value.length < 1 || value.length > 512 ||
          value !== value.trim() || /[\x00-\x1f\x7f]/.test(value))) {
      fail("Cloudflare Email Routing rule inventory was invalid");
    }
    if (action.type === "worker") {
      if (action.value.length !== 1) {
        fail("Cloudflare Email Routing rule inventory was invalid");
      }
      targetsEmailEdge ||= action.value[0] === EMAIL_EDGE_WORKER;
    }
  }
  return targetsEmailEdge;
}

function exactProviderZone(zone, api, contract) {
  if (!zone || Array.isArray(zone) || typeof zone !== "object" ||
      !HEX_32.test(String(api.accountID ?? "")) ||
      !HEX_32.test(String(api.zoneID ?? "")) ||
      zone.id !== api.zoneID || zone.account?.id !== api.accountID ||
      zone.status !== "active" || typeof zone.name !== "string" ||
      !Object.hasOwn(contract, zone.name)) {
    fail("Cloudflare provider zone/account did not match the managed-email contract");
  }
  const identity = Object.freeze({
    account_id: api.accountID,
    zone_id: api.zoneID,
    zone_name: zone.name,
    status: zone.status,
    contract: contract[zone.name],
  });
  return Object.freeze({
    contract: identity.contract,
    account_id_sha256: sha256(identity.account_id),
    zone_id_sha256: sha256(identity.zone_id),
    zone_name_sha256: sha256(identity.zone_name),
    zone_identity_sha256: sha256(canonicalJSON(identity)),
  });
}

function exactKeys(value, expected, label) {
  if (!value || Array.isArray(value) || typeof value !== "object" ||
      canonicalJSON(Object.keys(value).sort()) !==
        canonicalJSON([...expected].sort())) {
    fail(`${label} was invalid`);
  }
}

export function validateRelayProviderDarkState(value, expected = null) {
  exactKeys(value, [
    "schema",
    "provider_scope",
    "catch_all_sha256",
    "rule_inventory_sha256",
    "rule_count",
    "owned_or_edge_worker_routes_enabled",
  ], "relay provider-route evidence");
  exactKeys(value.provider_scope, [
    "contract",
    "account_id_sha256",
    "zone_id_sha256",
    "zone_name_sha256",
    "zone_identity_sha256",
  ], "relay provider-zone evidence");
  if (value.schema !== "witself.agent-email-provider-dark.v1" ||
      !["primary", "legacy"].includes(value.provider_scope.contract) ||
      !SHA256.test(value.provider_scope.account_id_sha256) ||
      !SHA256.test(value.provider_scope.zone_id_sha256) ||
      !SHA256.test(value.provider_scope.zone_name_sha256) ||
      !SHA256.test(value.provider_scope.zone_identity_sha256) ||
      !SHA256.test(value.catch_all_sha256) ||
      !SHA256.test(value.rule_inventory_sha256) ||
      !Number.isSafeInteger(value.rule_count) || value.rule_count < 0 ||
      value.rule_count > 10_000 ||
      value.owned_or_edge_worker_routes_enabled !== false) {
    fail("relay provider-route evidence was invalid");
  }
  if (expected != null &&
      (value.provider_scope.contract !== expected.contract ||
       value.provider_scope.zone_name_sha256 !== expected.zone_name_sha256)) {
    fail("relay provider zone did not match the explicit ceremony target");
  }
  return value;
}

export async function captureRelayProviderDarkState(
  api,
  contract = PRIMARY_PROVIDER_ZONE_CONTRACT,
) {
  if (!api || typeof api.getZone !== "function" ||
      typeof api.getCatchAll !== "function" ||
      typeof api.listRules !== "function" || !contract ||
      Array.isArray(contract) || typeof contract !== "object") {
    fail("relay provider-route inspection runtime is invalid");
  }
  const [zone, catchAll, rules] = await Promise.all([
    api.getZone(),
    api.getCatchAll(),
    api.listRules(),
  ]);
  const providerScope = exactProviderZone(zone, api, contract);
  const catchAllState = primaryRoutingInternals.catchAllState(catchAll);
  if (catchAllState.enabled !== false || !Array.isArray(rules)) {
    fail("provider email routes must remain dark during relay key provisioning");
  }
  const normalized = [];
  for (const rule of rules) {
    const id = providerRuleID(rule);
    if (typeof rule.name !== "string" || rule.name !== rule.name.trim() ||
        rule.name.length < 1 || rule.name.length > 256 ||
        typeof rule.enabled !== "boolean") {
      fail("Cloudflare Email Routing rule inventory was invalid");
    }
    const owned = PROVIDER_RULE_PREFIXES.some((prefix) =>
      rule.name.startsWith(prefix));
    const targetsEmailEdge = providerRuleTargetsEmailEdge(rule.actions);
    if (rule.enabled && (owned || targetsEmailEdge)) {
      fail("provider email routes must remain dark during relay key provisioning");
    }
    normalized.push(rule);
  }
  normalized.sort((left, right) => providerRuleID(left).localeCompare(providerRuleID(right)));
  return validateRelayProviderDarkState(Object.freeze({
    schema: "witself.agent-email-provider-dark.v1",
    provider_scope: providerScope,
    catch_all_sha256: catchAllState.sha256,
    rule_inventory_sha256: sha256(canonicalJSON(normalized)),
    rule_count: normalized.length,
    owned_or_edge_worker_routes_enabled: false,
  }));
}

function plain(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    fail(`${label} is missing ${name}`);
  }
  return binding.text;
}

function nonSecretResourcesSHA256(version, label) {
  if (!version?.resources || Array.isArray(version.resources) ||
      typeof version.resources !== "object" ||
      !Array.isArray(version.resources.bindings)) {
    fail(`${label} non-secret Worker resources were invalid`);
  }
  const bindings = version.resources.bindings.filter((binding) =>
    binding?.type !== "secret_text");
  return sha256(canonicalJSON({ ...version.resources, bindings }));
}

function exactTargetControlPlane(remote, target) {
  const versionID = activeVersionID(remote.deployment, "control plane");
  const attestation = verifyControlPlaneWorkerVersion(
    remote.version,
    target.controlPlaneExpected,
    versionID,
  );
  return Object.freeze({
    deployment_id: remote.deployment.id,
    version_id: versionID,
    script_etag: attestation.script_etag,
    non_secret_resources_sha256: nonSecretResourcesSHA256(
      remote.version,
      "control plane",
    ),
  });
}

function exactTargetEmailEdge(remote, target, requireAnnotations) {
  const versionID = activeVersionID(remote.deployment, "email edge");
  const bindings = activeBindings(remote.version, versionID, "email edge");
  const relayKeyID = plain(bindings, "RELAY_KEY_ID", "email edge");
  if (!KEY_ID.test(relayKeyID)) fail("live email-edge relay key id is invalid");
  verifyDeployment(remote.deployment, remote.version, {
    ...target.emailEdgeExpected,
    relayKeyID,
  }, { requireAnnotations });
  return Object.freeze({
    deployment_id: remote.deployment.id,
    version_id: versionID,
    relay_key_id: relayKeyID,
    non_secret_resources_sha256: nonSecretResourcesSHA256(
      remote.version,
      "email edge",
    ),
    secret_names: Object.freeze([...secretInventory(remote.secrets, "email edge")].sort()),
  });
}

function remoteIdentity(remote) {
  return Object.freeze({
    control_plane_deployment_id: remote.controlPlane.deployment.id,
    control_plane_version_id: activeVersionID(
      remote.controlPlane.deployment,
      "control plane",
    ),
    email_edge_deployment_id: remote.emailEdge.deployment.id,
    email_edge_version_id: activeVersionID(remote.emailEdge.deployment, "email edge"),
  });
}

function exactRemoteIdentity(left, right, message) {
  if (canonicalJSON(remoteIdentity(left)) !== canonicalJSON(remoteIdentity(right))) {
    fail(message);
  }
}

function witselfIdentityArgs(options) {
  const args = [
    "--account", options.account,
    "--realm", options.realm,
    "--agent", options.agent,
  ];
  if (options.endpoint) args.push("--endpoint", options.endpoint);
  if (options.tokenFile) args.push("--token-file", options.tokenFile);
  return args;
}

function publicKeyDigest(publicKey) {
  const raw = Buffer.from(publicKey, "base64");
  try {
    return createHash("sha256").update(raw).digest("hex");
  } finally {
    raw.fill(0);
  }
}

function createProvider(environment, target) {
  const api = new CloudflareAPI(cloudflareEnvironment(environment));
  return Object.freeze({
    capture: () => captureRelayProviderDarkState(
      api,
      target.providerZoneContract,
    ),
  });
}

function providerExpectation(target, zoneName) {
  const contract = target.providerZoneContract[zoneName];
  if (typeof contract !== "string") {
    fail("explicit provider zone is outside the target config contract");
  }
  return Object.freeze({
    contract,
    zone_name_sha256: sha256(zoneName),
  });
}

function relayWitselfEnvironment(environment) {
  const output = sanitizedWitselfEnvironment(environment);
  for (const name of [
    "CF_ACCOUNT_ID",
    "CF_API_TOKEN",
    "CLOUDFLARE_ACCOUNT_ID",
    "CLOUDFLARE_API_TOKEN",
    "CLOUDFLARE_ZONE_ID",
    "CONTROL_PLANE_EDGE_TOKEN",
    "CONTROL_PLANE_URL",
    "EMAIL_DIRECTORY_KV_ID",
  ]) {
    delete output[name];
  }
  return output;
}

async function freezeRelayConfig(runtime, source, prefix) {
  return createPrivateDeploymentConfig({
    prefix,
    async render(path) {
      const value = await runtime.readText(source);
      if (typeof value !== "string" || value.length < 1) {
        fail("relay signing source configuration was missing or invalid");
      }
      await writeFile(path, value, { flag: "wx", mode: 0o600 });
    },
  });
}

async function createRelayConfigSnapshots(runtime, options) {
  let controlPlane;
  let emailEdge;
  try {
    controlPlane = await freezeRelayConfig(
      runtime,
      options.controlPlaneConfig,
      "witself-relay-control-plane-",
    );
    emailEdge = await freezeRelayConfig(
      runtime,
      options.emailEdgeConfig,
      "witself-relay-email-edge-",
    );
    if (controlPlane.path === emailEdge.path ||
        controlPlane.path === options.controlPlaneConfig ||
        emailEdge.path === options.emailEdgeConfig) {
      fail("relay signing configuration snapshots were not isolated");
    }
    return Object.freeze({ controlPlane, emailEdge });
  } catch (error) {
    await Promise.allSettled([
      controlPlane?.cleanup(),
      emailEdge?.cleanup(),
    ]);
    throw error;
  }
}

async function assertRelayConfigSnapshotsUnchanged(snapshots) {
  await Promise.all([
    snapshots.controlPlane.assertUnchanged(),
    snapshots.emailEdge.assertUnchanged(),
  ]);
}

async function cleanupRelayConfigSnapshots(snapshots) {
  const results = await Promise.allSettled([
    snapshots.controlPlane.cleanup(),
    snapshots.emailEdge.cleanup(),
  ]);
  const rejected = results.find((result) => result.status === "rejected");
  if (rejected) {
    throw new Error("could not clean up relay signing configuration snapshots", {
      cause: rejected.reason,
    });
  }
}

async function provisionRelaySigningKeyFromSnapshots(options, snapshots, {
  runtime,
  environment,
  provider,
  withLease,
  reserveReceipt,
}) {
  await assertRelayConfigSnapshotsUnchanged(snapshots);
  const controlPlaneConfig = await snapshots.controlPlane.readText();
  const emailEdgeConfig = await snapshots.emailEdge.readText();
  const target = validateRelayProvisioningConfigs(
    controlPlaneConfig,
    emailEdgeConfig,
  );
  if (target.configFence.control_plane_sha256 !==
        snapshots.controlPlane.sha256 ||
      target.configFence.email_edge_sha256 !== snapshots.emailEdge.sha256) {
    fail("relay signing configuration snapshot fence was invalid");
  }
  await assertRelayConfigSnapshotsUnchanged(snapshots);
  const expectedProvider = providerExpectation(target, options.providerZoneName);
  const wranglerInspectionEnv = sanitizedWranglerInspectionEnvironment(environment);
  const wranglerMutationEnv = sanitizedWranglerEnvironment(environment);
  const witselfEnv = relayWitselfEnvironment(environment);
  const providerRuntime = provider ?? createProvider(environment, {
    ...target,
    providerZoneContract: Object.freeze({
      [options.providerZoneName]: expectedProvider.contract,
    }),
  });
  if (typeof providerRuntime.capture !== "function" ||
      typeof reserveReceipt !== "function") {
    fail("relay provider-route inspection runtime is invalid");
  }
  const inspectRemote = () => ({
    controlPlane: inspectWorker(
      runtime,
      CONTROL_PLANE_WORKER,
      snapshots.controlPlane.path,
      "control plane",
      wranglerInspectionEnv,
    ),
    emailEdge: inspectWorker(
      runtime,
      EMAIL_EDGE_WORKER,
      snapshots.emailEdge.path,
      "email edge",
      wranglerInspectionEnv,
    ),
  });

  await assertRelayConfigSnapshotsUnchanged(snapshots);
  const initialRemote = inspectRemote();
  await assertRelayConfigSnapshotsUnchanged(snapshots);
  assertRemoteDark(initialRemote);
  const initialControlPlane = exactTargetControlPlane(initialRemote.controlPlane, target);
  const initialEdge = exactTargetEmailEdge(initialRemote.emailEdge, target, true);
  if (initialEdge.relay_key_id === target.keyID) {
    fail("relay signing rotation requires a distinct desired key id");
  }
  const initialProvider = validateRelayProviderDarkState(
    await providerRuntime.capture(),
    expectedProvider,
  );
  const leaseOrigin = routeSigningOperationsLeaseOrigin(initialRemote);

  const shown = validateRelaySecretMetadata(
    showSecret(runtime, options.relaySecret, options, witselfEnv),
    options.relaySecret,
    {
      keyID: options.relayKeyIDField,
      public: options.relayPublicField,
      private: options.relayPrivateField,
    },
    target.keyID,
  );
  const privateValue = validateRevealEnvelope(
    revealSecret(
      runtime,
      options.relaySecret,
      options.relayPrivateField,
      options,
      witselfEnv,
    ),
    { secretID: shown.secret.id, field: shown.privateField },
    "relay signing private-key reveal",
    { trimOuterWhitespace: true },
  );
  try {
    verifyEd25519Keypair(privateValue, shown.publicKey);
  } catch {
    fail("relay signing private key does not match the public key metadata");
  }
  const digest = publicKeyDigest(shown.publicKey);

  await assertRelayConfigSnapshotsUnchanged(snapshots);
  return withLease(
    OPERATIONS_LEASE_OPERATION,
    async (leaseGuard) => {
      if (!leaseGuard || typeof leaseGuard.renew !== "function" ||
          typeof leaseGuard.evidence !== "function") {
        fail("relay signing key provisioning lease guard is invalid");
      }
      await assertRelayConfigSnapshotsUnchanged(snapshots);
      const leasedRemote = inspectRemote();
      await assertRelayConfigSnapshotsUnchanged(snapshots);
      assertRemoteDark(leasedRemote);
      const leasedControlPlane = exactTargetControlPlane(
        leasedRemote.controlPlane,
        target,
      );
      const leasedEdge = exactTargetEmailEdge(
        leasedRemote.emailEdge,
        target,
        true,
      );
      exactRemoteIdentity(
        initialRemote,
        leasedRemote,
        "live Workers changed before relay key provisioning",
      );
      if (routeSigningOperationsLeaseOrigin(leasedRemote) !== leaseOrigin) {
        fail("email edge operations lease origin changed before relay key provisioning");
      }
      if (leasedControlPlane.non_secret_resources_sha256 !==
            initialControlPlane.non_secret_resources_sha256 ||
          leasedEdge.non_secret_resources_sha256 !==
            initialEdge.non_secret_resources_sha256 ||
          leasedEdge.relay_key_id !== initialEdge.relay_key_id) {
        fail("live Worker resources changed before relay key provisioning");
      }
      const leasedProvider = validateRelayProviderDarkState(
        await providerRuntime.capture(),
        expectedProvider,
      );
      if (canonicalJSON(leasedProvider) !== canonicalJSON(initialProvider)) {
        fail("provider email routes changed before relay key provisioning");
      }
      await assertRelayConfigSnapshotsUnchanged(snapshots);

      const pending = Object.freeze({
        schema: "witself.agent-email-relay-signing-key-provisioning-pending.v1",
        state: "secret_write_started",
        worker: EMAIL_EDGE_WORKER,
        operation: OPERATIONS_LEASE_OPERATION,
        target: Object.freeze({
          release: target.release,
          config_fence: target.configFence,
          provider_zone: expectedProvider,
          prior_key_id: initialEdge.relay_key_id,
          desired_key_id: shown.keyID,
          desired_public_key_sha256: digest,
        }),
        predecessor: Object.freeze({
          control_plane_deployment_id: leasedControlPlane.deployment_id,
          control_plane_version_id: leasedControlPlane.version_id,
          control_plane_non_secret_resources_sha256:
            leasedControlPlane.non_secret_resources_sha256,
          edge_deployment_id: leasedEdge.deployment_id,
          edge_version_id: leasedEdge.version_id,
          edge_non_secret_resources_sha256:
            leasedEdge.non_secret_resources_sha256,
        }),
        provider: initialProvider,
        recovery: "reconcile_dark_live_state_before_retry",
      });
      let journal;
      try {
        await assertRelayConfigSnapshotsUnchanged(snapshots);
        journal = reserveReceipt(options.receipt, pending);
        if (!journal || typeof journal.commit !== "function" ||
            typeof journal.close !== "function") {
          fail("relay signing receipt journal is invalid");
        }
        await assertRelayConfigSnapshotsUnchanged(snapshots);
        const privateBytes = Buffer.from(privateValue, "utf8");
        try {
          await putWorkerSecret(
            runtime,
            leaseGuard,
            EMAIL_EDGE_WORKER,
            snapshots.emailEdge.path,
            RELAY_PRIVATE_SECRET,
            privateBytes,
            wranglerMutationEnv,
          );
        } finally {
          privateBytes.fill(0);
        }
        await assertRelayConfigSnapshotsUnchanged(snapshots);

        const postRemote = inspectRemote();
        await assertRelayConfigSnapshotsUnchanged(snapshots);
        assertRemoteDark(postRemote);
        const postControlPlane = exactTargetControlPlane(
          postRemote.controlPlane,
          target,
        );
        const postEdge = exactTargetEmailEdge(
          postRemote.emailEdge,
          target,
          false,
        );
        if (postControlPlane.deployment_id !==
              leasedControlPlane.deployment_id ||
            postControlPlane.version_id !== leasedControlPlane.version_id ||
            postControlPlane.non_secret_resources_sha256 !==
              leasedControlPlane.non_secret_resources_sha256 ||
            postEdge.deployment_id === leasedEdge.deployment_id ||
            postEdge.version_id === leasedEdge.version_id) {
          fail("relay secret write did not create one new edge Worker successor");
        }
        if (postEdge.non_secret_resources_sha256 !==
              leasedEdge.non_secret_resources_sha256 ||
            postEdge.relay_key_id !== leasedEdge.relay_key_id ||
            canonicalJSON(postEdge.secret_names) !==
              canonicalJSON(leasedEdge.secret_names)) {
          fail("non-secret edge Worker resources changed during relay key provisioning");
        }
        const edgeBindings = activeBindings(
          postRemote.emailEdge.version,
          postEdge.version_id,
          "email edge",
        );
        assertSecretBinding(edgeBindings, RELAY_PRIVATE_SECRET, "email edge");
        if (!secretInventory(postRemote.emailEdge.secrets, "email edge")
          .has(RELAY_PRIVATE_SECRET)) {
          fail("email edge persistent relay signing secret is missing after provisioning");
        }
        const postProvider = validateRelayProviderDarkState(
          await providerRuntime.capture(),
          expectedProvider,
        );
        if (canonicalJSON(postProvider) !== canonicalJSON(leasedProvider)) {
          fail("provider email routes changed during relay key provisioning");
        }
        await assertRelayConfigSnapshotsUnchanged(snapshots);

        await leaseGuard.renew();
        await assertRelayConfigSnapshotsUnchanged(snapshots);
        const leaseEvidence = leaseGuard.evidence();
        validateAgentEmailOperationsLeaseEvidence(
          leaseEvidence,
          OPERATIONS_LEASE_OPERATION,
        );
        const receipt = Object.freeze({
          schema: "witself.agent-email-relay-signing-key-provisioning.v2",
          outcome: "provisioned",
          worker: EMAIL_EDGE_WORKER,
          target: Object.freeze({
            release: target.release,
            config_fence: target.configFence,
            provider_zone: expectedProvider,
          }),
          relay_key: Object.freeze({
            prior_key_id: initialEdge.relay_key_id,
            desired_key_id: shown.keyID,
            desired_public_key_digest: Object.freeze({
              algorithm: "sha256",
              encoding: "ed25519_raw",
              sha256: digest,
            }),
          }),
          provider: postProvider,
          successor: Object.freeze({
            prior_deployment_id: leasedEdge.deployment_id,
            prior_version_id: leasedEdge.version_id,
            deployment_id: postEdge.deployment_id,
            version_id: postEdge.version_id,
            non_secret_resources_sha256:
              postEdge.non_secret_resources_sha256,
          }),
          operation: Object.freeze({
            secret_binding: RELAY_PRIVATE_SECRET,
            secret_write: "succeeded",
          }),
          operations_lease: leaseEvidence,
          safeguards: Object.freeze({
            exact_target_release_verified_on_both_workers: true,
            empty_managed_cohorts_verified: true,
            all_delivery_gates_verified_dark: true,
            exact_provider_zone_and_account_verified: true,
            provider_routes_verified_dark: true,
            provider_routes_reverified_after_mutation: true,
            relay_keypair_verified: true,
            private_value_written_only_over_stdin: true,
            no_plain_vars_changed: true,
            new_edge_successor_verified: true,
            non_secret_resources_unchanged: true,
            post_write_binding_and_inventory_verified: true,
            serialized_by_global_operations_lease: true,
            frozen_private_config_snapshots_verified: true,
            exact_tagged_redeploy_required: true,
          }),
        });
        await assertRelayConfigSnapshotsUnchanged(snapshots);
        journal.commit(receipt);
        return receipt;
      } catch (error) {
        if (typeof journal?.close === "function") journal.close();
        throw error;
      }
    },
    { endpoint: leaseOrigin, env: environment },
  );
}

export async function provisionRelaySigningKey(options, dependencies = {}) {
  const runtime = dependencies.runtime ?? createSpawnRuntime();
  const environment = dependencies.environment ?? process.env;
  const provider = dependencies.provider ?? null;
  const withLease = dependencies.withLease ?? withAgentEmailOperationsLease;
  const reserveReceipt = dependencies.reserveReceipt ?? reserveJSONReceipt;
  runtime.assertReceiptAvailable(options.receipt);
  const snapshots = await createRelayConfigSnapshots(runtime, options);
  try {
    return await provisionRelaySigningKeyFromSnapshots(options, snapshots, {
      runtime,
      environment,
      provider,
      withLease,
      reserveReceipt,
    });
  } finally {
    await cleanupRelayConfigSnapshots(snapshots);
  }
}

async function main() {
  const options = parseRelayProvisioningArgs(process.argv.slice(2));
  const receipt = await provisionRelaySigningKey(options);
  process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
