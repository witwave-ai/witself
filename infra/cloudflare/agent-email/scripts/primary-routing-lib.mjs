import { createHash, webcrypto } from "node:crypto";

import {
  parseRouteAddress,
  realmRouteKey,
  realmRouteProjectionIsFresh,
  verifyRealmRouteProjection,
} from "../src/directory.mjs";
import {
  parseManagedDeliveryAccountAllowlist as parseEdgeManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";
import {
  parseManagedDeliveryAccountAllowlist as parseControlPlaneManagedDeliveryAccountAllowlist,
} from "../../control-plane/src/agent-email-managed-delivery-cohort.mjs";
import { assertIsolatedEmailDirectory } from "./cloudflare.mjs";
import {
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";
import {
  LEGACY_PILOT_WORKER,
  PRODUCTION_RECEIVE_WORKER,
} from "../src/worker-names.mjs";

export const PRIMARY_CANARY_DOMAIN = "witmail.net";
export const PRIMARY_CANARY_WORKER = PRODUCTION_RECEIVE_WORKER;
export const PRIMARY_RULE_PREFIX = "witself-agent-email-primary-canary:";
export const PRIMARY_PLAN_SCHEMA = "witself.agent-email-primary-routing-plan.v2";
export const PRIMARY_RECEIPT_SCHEMA = "witself.agent-email-primary-routing-receipt.v2";
export const PRIMARY_STATUS_SCHEMA = "witself.agent-email-primary-routing-status.v1";
export const PRIMARY_CONTROL_PLANE_URL = "https://self.witwave.ai/";

const PLAN_LIFETIME_MS = 15 * 60 * 1_000;
const ACTIONS = new Set(["prepare", "activate", "disable", "remove"]);
const ROLE_LOCAL_PARTS = Object.freeze(["abuse", "postmaster"]);
const ID = /^[0-9a-f]{32}(?![\s\S])/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}(?![\s\S])/;
const SHA256 = /^[0-9a-f]{64}(?![\s\S])/;
const AGENT_ID = /^agent_[a-z2-7]{16}(?![\s\S])/;
const ACCOUNT_ID = /^acc_[a-z2-7]{16}(?![\s\S])/;
const REALM_ID = /^realm_([a-z2-7]{16})(?![\s\S])/;
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}(?![\s\S])/;
const PUBLIC_KEY = /^[A-Za-z0-9+/]{43}=(?![\s\S])/;
const RELEASE_VERSION = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?![\s\S])/;
const COMMIT = /^[0-9a-f]{40}(?![\s\S])/;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})(?![\s\S])/;
const RFC3339_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z(?![\s\S])/;
const OPERATIONS_LEASE_MINIMUM_RELEASE = "0.0.241";

export function canonicalJSON(value) {
  const canonicalize = (item) => {
    if (Array.isArray(item)) return item.map(canonicalize);
    if (item && typeof item === "object") {
      return Object.fromEntries(
        Object.keys(item).sort().map((key) => [key, canonicalize(item[key])]),
      );
    }
    return item;
  };
  return JSON.stringify(canonicalize(value));
}

export function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function exactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      canonicalJSON(Object.keys(value).sort()) !== canonicalJSON([...keys].sort())) {
    throw new Error(`${label} was malformed`);
  }
}

export function normalizePrimaryCanaryManifest(input) {
  exactKeys(
    input,
    ["schema_version", "domain", "worker_name", "account_ids", "agents"],
    "primary canary manifest",
  );
  if (input.schema_version !== 2 || input.domain !== PRIMARY_CANARY_DOMAIN ||
      input.worker_name !== PRIMARY_CANARY_WORKER || !Array.isArray(input.agents) ||
      input.agents.length < 5 || input.agents.length > 10 ||
      !Array.isArray(input.account_ids) || input.account_ids.length < 1 ||
      input.account_ids.length > 100 ||
      input.account_ids.some((accountID) => !ACCOUNT_ID.test(String(accountID ?? ""))) ||
      new Set(input.account_ids).size !== input.account_ids.length ||
      canonicalJSON(input.account_ids) !== canonicalJSON([...input.account_ids].sort())) {
    throw new Error("primary canary manifest must contain 5-10 witmail.net agents");
  }
  const addresses = new Set();
  const agentIDs = new Set();
  const agents = input.agents.map((agent) => {
    exactKeys(agent, ["agent_id", "realm_id", "address"], "primary canary agent");
    const agentID = String(agent.agent_id ?? "");
    const realmID = String(agent.realm_id ?? "");
    const realm = REALM_ID.exec(realmID);
    if (!AGENT_ID.test(agentID) || !realm || agentIDs.has(agentID)) {
      throw new Error("primary canary agent identity was invalid or duplicated");
    }
    let address;
    try {
      address = parseRouteAddress(String(agent.address ?? ""), false);
    } catch {
      throw new Error("primary canary address was invalid");
    }
    if (address.domain !== PRIMARY_CANARY_DOMAIN || address.realmLabel !== realm[1] ||
        addresses.has(address.baseAddress)) {
      throw new Error("primary canary address did not match its canonical realm");
    }
    addresses.add(address.baseAddress);
    agentIDs.add(agentID);
    return {
      agent_id: agentID,
      realm_id: realmID,
      address: address.baseAddress,
    };
  });
  agents.sort((left, right) => left.address.localeCompare(right.address));
  return Object.freeze({
    schema_version: 2,
    domain: PRIMARY_CANARY_DOMAIN,
    worker_name: PRIMARY_CANARY_WORKER,
    account_ids: [...input.account_ids],
    agents,
  });
}

function literalAddress(rule) {
  if (!Array.isArray(rule?.matchers) || rule.matchers.length !== 1) return "";
  const matcher = rule.matchers[0];
  return matcher?.type === "literal" && matcher?.field === "to" &&
    typeof matcher.value === "string" ? matcher.value.toLowerCase() : "";
}

function desiredWorkerAction(rule, workerName) {
  return Array.isArray(rule?.actions) && rule.actions.length === 1 &&
    rule.actions[0]?.type === "worker" &&
    Array.isArray(rule.actions[0].value) && rule.actions[0].value.length === 1 &&
    rule.actions[0].value[0] === workerName;
}

function ownedWorkerAction(rule, productionWorker) {
  if (!Array.isArray(rule?.actions) || rule.actions.length !== 1 ||
      rule.actions[0]?.type !== "worker" ||
      !Array.isArray(rule.actions[0].value) ||
      rule.actions[0].value.length !== 1) {
    return "";
  }
  const worker = rule.actions[0].value[0];
  return worker === productionWorker || worker === LEGACY_PILOT_WORKER
    ? worker
    : "";
}

export function primaryRuleName(address) {
  return `${PRIMARY_RULE_PREFIX}${address}`;
}

export function desiredPrimaryRule(manifest, address, enabled, priority) {
  const rule = {
    name: primaryRuleName(address),
    enabled,
    matchers: [{ type: "literal", field: "to", value: address }],
    actions: [{ type: "worker", value: [manifest.worker_name] }],
  };
  if (Number.isSafeInteger(priority) && priority >= 0) rule.priority = priority;
  return rule;
}

function validForwardAction(rule) {
  if (!Array.isArray(rule?.actions) || rule.actions.length !== 1 ||
      rule.actions[0]?.type !== "forward" ||
      !Array.isArray(rule.actions[0].value) || rule.actions[0].value.length < 1) {
    return false;
  }
  return rule.actions[0].value.every((value) =>
    typeof value === "string" && value === value.trim() && value.length <= 320 &&
    /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value));
}

function indexPrimaryRules(rules, manifest) {
  if (!Array.isArray(rules)) throw new Error("Cloudflare routing-rule list was invalid");
  const enrolled = new Set(manifest.agents.map((agent) => agent.address));
  const byAddress = new Map();
  const owned = [];
  const legacy = [];
  const stale = [];
  const conflicts = [];
  for (const rule of rules) {
    const address = literalAddress(rule);
    const named = typeof rule?.name === "string" && rule.name.startsWith(PRIMARY_RULE_PREFIX);
    const worker = ownedWorkerAction(rule, manifest.worker_name);
    const exactOwned = rule?.source === "api" && ID.test(String(rule?.id ?? "")) &&
      typeof rule?.enabled === "boolean" && named && address &&
      rule.name === primaryRuleName(address) && worker !== "";
    if (named && !exactOwned) {
      conflicts.push(rule);
      continue;
    }
    if (exactOwned) {
      owned.push(rule);
      if (worker === LEGACY_PILOT_WORKER) legacy.push(rule);
      if (!enrolled.has(address)) stale.push(rule);
    }
    if (!enrolled.has(address)) continue;
    if (!exactOwned) {
      conflicts.push(rule);
      continue;
    }
    if (byAddress.has(address)) {
      conflicts.push(rule);
      continue;
    }
    byAddress.set(address, rule);
  }
  return { byAddress, owned, legacy, stale, conflicts };
}

function roleRouteState(rules, domain) {
  const selected = [];
  let ready = true;
  for (const localPart of ROLE_LOCAL_PARTS) {
    const address = `${localPart}@${domain}`;
    const matches = rules.filter((rule) => literalAddress(rule) === address);
    if (matches.length !== 1 || matches[0].enabled !== true ||
        !validForwardAction(matches[0])) {
      ready = false;
    }
    selected.push([address, matches]);
  }
  selected.sort(([left], [right]) => left.localeCompare(right));
  return Object.freeze({
    ready,
    required: ROLE_LOCAL_PARTS.length,
    sha256: sha256(canonicalJSON(selected)),
  });
}

function catchAllState(rule) {
  if (!rule || typeof rule !== "object" || Array.isArray(rule) ||
      !Array.isArray(rule.matchers) || rule.matchers.length !== 1 ||
      rule.matchers[0]?.type !== "all" || typeof rule.enabled !== "boolean" ||
      !Array.isArray(rule.actions)) {
    throw new Error("Cloudflare catch-all rule was invalid");
  }
  const actionTypes = rule.actions.map((action) => String(action?.type ?? "")).sort();
  return Object.freeze({
    enabled: rule.enabled,
    action_types: actionTypes,
    sha256: sha256(canonicalJSON(rule)),
  });
}

function primaryRuleDigest(indexed) {
  const rules = [...indexed.owned].sort((left, right) =>
    literalAddress(left).localeCompare(literalAddress(right)));
  return sha256(canonicalJSON(rules));
}

export async function capturePrimaryRoutingState(api, manifestInput) {
  const manifest = normalizePrimaryCanaryManifest(manifestInput);
  const [settings, catchAll, rules] = await Promise.all([
    api.getEmailRoutingSettings(),
    api.getCatchAll(),
    api.listRules(),
  ]);
  const indexed = indexPrimaryRules(rules, manifest);
  const roleRoutes = roleRouteState(rules, manifest.domain);
  const catchAllGuard = catchAllState(catchAll);
  return {
    manifest,
    settings,
    catchAll,
    rules,
    indexed,
    summary: Object.freeze({
      email_routing: {
        enabled: settings?.enabled === true,
        ready: settings?.status === "ready",
        support_subaddress: settings?.support_subaddress === true,
      },
      catch_all: catchAllGuard,
      role_routes: roleRoutes,
      managed_rules: {
        sha256: primaryRuleDigest(indexed),
        configured: indexed.byAddress.size,
        enabled: [...indexed.byAddress.values()].filter((rule) => rule.enabled === true).length,
        disabled: [...indexed.byAddress.values()].filter((rule) => rule.enabled === false).length,
        missing: manifest.agents.length - indexed.byAddress.size,
        stale: indexed.stale.length,
        conflicts: indexed.conflicts.length,
        legacy_targets: indexed.legacy.length,
        total_owned: indexed.owned.length,
      },
    }),
  };
}

function activeVersionID(deployment, label) {
  if (!deployment || typeof deployment !== "object" || Array.isArray(deployment) ||
      !UUID.test(String(deployment.id ?? "")) || deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error(`${label} deployment was not one version at 100 percent`);
  }
  return deployment.versions[0].version_id;
}

function bindingMap(version, expectedID, label) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedID || !UUID.test(String(version.id ?? "")) ||
      !Number.isSafeInteger(version.number) || version.number < 1 ||
      !Array.isArray(version.resources?.bindings)) {
    throw new Error(`${label} active Worker version was invalid`);
  }
  const bindings = new Map();
  for (const binding of version.resources.bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || binding.name === "" ||
        bindings.has(binding.name)) {
      throw new Error(`${label} active Worker bindings were invalid`);
    }
    bindings.set(binding.name, binding);
  }
  return bindings;
}

function plain(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    throw new Error(`${label} active Worker was missing ${name}`);
  }
  return binding.text;
}

function secretBound(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    throw new Error(`${label} active Worker was missing ${name}`);
  }
}

function namespace(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "kv_namespace" || !ID.test(String(binding.namespace_id ?? ""))) {
    throw new Error(`${label} active Worker was missing ${name}`);
  }
  return binding.namespace_id;
}

function releaseIdentity(bindings, label) {
  const result = {
    version: plain(bindings, "WITSELF_EDGE_RELEASE_VERSION", label),
    commit: plain(bindings, "WITSELF_EDGE_RELEASE_COMMIT", label),
    date: plain(bindings, "WITSELF_EDGE_RELEASE_DATE", label),
  };
  if (!RELEASE_VERSION.test(result.version) || !COMMIT.test(result.commit) ||
      !RFC3339.test(result.date) || !Number.isFinite(Date.parse(result.date))) {
    throw new Error(`${label} active Worker release identity was invalid`);
  }
  return result;
}

function releaseAtLeast(release, minimum) {
  const parse = (value) => {
    const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?$/.exec(value);
    if (!match) throw new Error("agent-email operations lease release was invalid");
    return {
      numbers: match.slice(1, 4).map(Number),
      prerelease: match[4] ?? "",
    };
  };
  const left = parse(release);
  const right = parse(minimum);
  for (let index = 0; index < left.numbers.length; index += 1) {
    if (left.numbers[index] !== right.numbers[index]) {
      return left.numbers[index] > right.numbers[index];
    }
  }
  return right.prerelease !== "" || left.prerelease === "";
}

// operationsLeaseControlPlaneOrigin proves that the active control plane owns
// the durable v0.0.241+ lease API and derives its exact origin from the active
// email edge. This deliberately has narrower requirements than full delivery
// readiness so emergency disable remains available during a partial rollout.
export function operationsLeaseControlPlaneOrigin(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    throw new Error("operations lease Worker inspection was invalid");
  }
  const controlPlaneID = activeVersionID(
    raw.control_plane_deployment,
    "control plane",
  );
  const edgeID = activeVersionID(raw.email_edge_deployment, "email edge");
  const controlBindings = bindingMap(
    raw.control_plane_version,
    controlPlaneID,
    "control plane",
  );
  const edgeBindings = bindingMap(
    raw.email_edge_version,
    edgeID,
    "email edge",
  );
  const controlRelease = releaseIdentity(controlBindings, "control plane");
  releaseIdentity(edgeBindings, "email edge");
  if (!releaseAtLeast(
    controlRelease.version,
    OPERATIONS_LEASE_MINIMUM_RELEASE,
  )) {
    throw new Error(
      "agent-email routing mutation requires the v0.0.241 durable operations lease",
    );
  }
  secretBound(controlBindings, "CONTROL_PLANE_EDGE_TOKEN", "control plane");
  secretBound(edgeBindings, "CONTROL_PLANE_EDGE_TOKEN", "email edge");
  let origin;
  try {
    origin = new URL(plain(edgeBindings, "CONTROL_PLANE_URL", "email edge"));
  } catch {
    throw new Error("email edge operations lease origin was invalid");
  }
  if (origin.toString() !== PRIMARY_CONTROL_PLANE_URL ||
      plain(edgeBindings, "CONTROL_PLANE_URL", "email edge") !==
        PRIMARY_CONTROL_PLANE_URL) {
    throw new Error("email edge operations lease origin was invalid");
  }
  return origin.toString();
}

function keyring(raw, activeKeyID) {
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error("email edge route-verification keyring was invalid");
  }
  const entries = parsed && typeof parsed === "object" && !Array.isArray(parsed)
    ? Object.entries(parsed).sort(([left], [right]) => left.localeCompare(right))
    : [];
  if (entries.length < 1 || entries.length > 4 ||
      JSON.stringify(Object.fromEntries(entries)) !== raw ||
      entries.some(([keyID, value]) => !KEY_ID.test(keyID) ||
        typeof value !== "string" || !PUBLIC_KEY.test(value) ||
        Buffer.from(value, "base64").byteLength !== 32 ||
        Buffer.from(value, "base64").toString("base64") !== value) ||
      !Object.hasOwn(parsed, activeKeyID)) {
    throw new Error("email edge route-verification keyring was invalid");
  }
  return parsed;
}

export function verifyPrimaryWorkerReadiness(raw, expectedNamespaceID, expectedDomain) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    throw new Error("primary routing Worker inspection was invalid");
  }
  const controlPlaneID = activeVersionID(raw.control_plane_deployment, "control plane");
  const edgeID = activeVersionID(raw.email_edge_deployment, "email edge");
  const controlBindings = bindingMap(raw.control_plane_version, controlPlaneID, "control plane");
  const edgeBindings = bindingMap(raw.email_edge_version, edgeID, "email edge");
  const controlRelease = releaseIdentity(controlBindings, "control plane");
  const edgeRelease = releaseIdentity(edgeBindings, "email edge");
  if (canonicalJSON(controlRelease) !== canonicalJSON(edgeRelease)) {
    throw new Error("control-plane and email-edge releases did not match");
  }
  if (plain(edgeBindings, "AGENT_EMAIL_DOMAIN", "email edge") !== expectedDomain) {
    throw new Error("email edge primary managed domain did not match the canary");
  }
  const controlDirectory = namespace(controlBindings, "AGENT_EMAIL_DIRECTORY", "control plane");
  const edgeDirectory = namespace(edgeBindings, "EMAIL_DIRECTORY", "email edge");
  if (controlDirectory !== edgeDirectory || controlDirectory !== expectedNamespaceID) {
    throw new Error("primary routing directory bindings did not match");
  }
  const activeKeyID = plain(
    controlBindings,
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    "control plane",
  );
  if (!KEY_ID.test(activeKeyID)) {
    throw new Error("control-plane route signing key id was invalid");
  }
  const publicKeys = keyring(
    plain(edgeBindings, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", "email edge"),
    activeKeyID,
  );
  const controlPlaneCohort = plain(
    controlBindings,
    "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "control plane",
  );
  const emailEdgeCohort = plain(
    edgeBindings,
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    "email edge",
  );
  let controlPlaneAccounts;
  let emailEdgeAccounts;
  try {
    controlPlaneAccounts = parseControlPlaneManagedDeliveryAccountAllowlist(controlPlaneCohort);
    emailEdgeAccounts = parseEdgeManagedDeliveryAccountAllowlist(emailEdgeCohort);
  } catch {
    throw new Error("managed delivery cohort was invalid");
  }
  if (controlPlaneCohort !== emailEdgeCohort ||
      canonicalJSON(controlPlaneAccounts) !== canonicalJSON(emailEdgeAccounts) ||
      controlPlaneAccounts.length < 1) {
    throw new Error("control-plane and email-edge managed delivery cohorts were not ready");
  }
  for (const [bindings, name, label] of [
    [controlBindings, "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY", "control plane"],
    [controlBindings, "CONTROL_PLANE_EDGE_TOKEN", "control plane"],
    [edgeBindings, "CONTROL_PLANE_EDGE_TOKEN", "email edge"],
    [edgeBindings, "RELAY_ED25519_PRIVATE_KEY", "email edge"],
    [controlBindings, "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED", "control plane"],
    [controlBindings, "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "control plane"],
  ]) {
    secretBound(bindings, name, label);
  }
  const canonicalGate = plain(
    edgeBindings,
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    "email edge",
  );
  const aliasGate = plain(
    edgeBindings,
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "email edge",
  );
  if (![canonicalGate, aliasGate].every((value) => value === "true" || value === "false")) {
    throw new Error("email edge managed delivery gates were invalid");
  }
  let controlPlaneURL;
  try {
    controlPlaneURL = new URL(plain(edgeBindings, "CONTROL_PLANE_URL", "email edge"));
  } catch {
    throw new Error("email edge control-plane origin was invalid");
  }
  if (controlPlaneURL.toString() !== PRIMARY_CONTROL_PLANE_URL ||
      plain(edgeBindings, "CONTROL_PLANE_URL", "email edge") !==
        PRIMARY_CONTROL_PLANE_URL) {
    throw new Error("email edge control-plane origin was invalid");
  }
  return {
    private: {
      keyring: JSON.stringify(publicKeys),
      control_plane_url: controlPlaneURL.toString(),
      cohort_accounts: emailEdgeAccounts,
    },
    summary: Object.freeze({
      release_sha256: sha256(canonicalJSON(controlRelease)),
      control_plane_version_id: controlPlaneID,
      email_edge_version_id: edgeID,
      route_directory_sha256: sha256(controlDirectory),
      route_signing_key_id: activeKeyID,
      managed_delivery_cohort: {
        account_count: emailEdgeAccounts.length,
        allowlist_sha256: sha256(emailEdgeCohort),
      },
      gates: {
        control_plane_canonical_inventory_bound: true,
        control_plane_canonical_delivery_bound: true,
        email_edge_canonical_delivery_enabled: canonicalGate === "true",
        email_edge_alias_delivery_enabled: aliasGate === "true",
      },
    }),
  };
}

export async function inspectPrimaryReadiness(api, runtime, manifestInput, {
  now = () => new Date(),
} = {}) {
  const manifest = normalizePrimaryCanaryManifest(manifestInput);
  if (!runtime || typeof runtime.inspectWorkers !== "function" ||
      typeof runtime.getControlPlaneProjection !== "function" ||
      typeof runtime.getControlPlaneReadiness !== "function") {
    throw new Error("primary routing readiness runtime was invalid");
  }
  await assertIsolatedEmailDirectory(api);
  const checkedAt = now();
  if (!(checkedAt instanceof Date) || !Number.isFinite(checkedAt.valueOf())) {
    throw new Error("primary routing readiness clock was invalid");
  }
  const workers = verifyPrimaryWorkerReadiness(
    await runtime.inspectWorkers(),
    api.namespaceID,
    manifest.domain,
  );
  if (workers.summary.gates.email_edge_alias_delivery_enabled) {
    throw new Error("managed alias delivery must remain disabled during the primary canary");
  }
  if (canonicalJSON(manifest.account_ids) !==
      canonicalJSON(workers.private.cohort_accounts)) {
    throw new Error("private canary manifest did not match the active managed delivery cohort");
  }
  const controlPlaneReadiness = await runtime.getControlPlaneReadiness(
    workers.private.control_plane_url,
  );
  const managed = controlPlaneReadiness?.managed_delivery;
  const cohort = managed?.cohort;
  if (controlPlaneReadiness?.schema_version !==
        "witself.agent-email-managed-delivery-readiness.v1" ||
      cohort?.schema !== "witself.agent-email-managed-delivery-cohort.v1" ||
      !Number.isSafeInteger(cohort.account_count) || cohort.account_count < 1 ||
      !SHA256.test(String(cohort.allowlist_sha256 ?? "")) || cohort.empty !== false ||
      cohort.account_count !== workers.summary.managed_delivery_cohort.account_count ||
      cohort.allowlist_sha256 !== workers.summary.managed_delivery_cohort.allowlist_sha256 ||
      managed.canonical_inventory_enabled !== true ||
      managed.canonical_delivery_enabled !== true ||
      typeof managed.alias_authority_activation_enabled !== "boolean") {
    throw new Error("control-plane managed delivery readiness did not match the active Workers");
  }
  const realms = new Map();
  for (const agent of manifest.agents) realms.set(agent.realm_id, agent.realm_id.slice(6));
  const projections = [];
  const projectionAccounts = new Set();
  for (const [realmID, realmLabel] of [...realms.entries()].sort()) {
    const key = realmRouteKey(manifest.domain, realmLabel);
    const [kvValue, controlValue] = await Promise.all([
      api.getKVJSON(key),
      runtime.getControlPlaneProjection(
        workers.private.control_plane_url,
        manifest.domain,
        realmLabel,
      ),
    ]);
    if (canonicalJSON(kvValue) !== canonicalJSON(controlValue)) {
      throw new Error("canonical route projection had not converged between control plane and KV");
    }
    let route;
    try {
      route = await verifyRealmRouteProjection(
        kvValue,
        manifest.domain,
        realmLabel,
        { AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: workers.private.keyring },
        webcrypto,
      );
    } catch {
      throw new Error("canonical route projection signature or schema was invalid");
    }
    if (route.route_kind !== "canonical" || route.realm_id !== realmID ||
        route.state !== "applied" ||
        !workers.private.cohort_accounts.includes(route.account_id) ||
        !realmRouteProjectionIsFresh(route, checkedAt.valueOf())) {
      throw new Error("canonical route projection was not fresh applied authority");
    }
    projections.push({
      realm_id: realmID,
      realm_label: realmLabel,
      controller_revision: route.controller_revision,
      updated_at: route.updated_at,
      projection_sha256: sha256(canonicalJSON(kvValue)),
    });
    projectionAccounts.add(route.account_id);
  }
  if ([...projectionAccounts].some((accountID) =>
    !manifest.account_ids.includes(accountID))) {
    throw new Error("canary projection contained an account outside the managed delivery cohort");
  }
  return Object.freeze({
    checked_at: checkedAt.toISOString(),
    workers: workers.summary,
    projections,
    projection_count: projections.length,
    represented_account_count: projectionAccounts.size,
    activation_ready:
      workers.summary.gates.control_plane_canonical_inventory_bound &&
      workers.summary.gates.control_plane_canonical_delivery_bound &&
      managed.canonical_inventory_enabled &&
      managed.canonical_delivery_enabled &&
      workers.summary.gates.email_edge_canonical_delivery_enabled &&
      !workers.summary.gates.email_edge_alias_delivery_enabled,
  });
}

export function summarizePrimaryReadiness(readiness) {
  return Object.freeze({
    workers: readiness.workers,
    projection_count: readiness.projection_count,
    represented_account_count: readiness.represented_account_count,
    projection_set_sha256: sha256(canonicalJSON(readiness.projections)),
    activation_ready: readiness.activation_ready,
  });
}

function assertCommonMutationState(state) {
  if (state.indexed.conflicts.length !== 0) {
    throw new Error("primary canary has an unmanaged or malformed rule conflict");
  }
}

function assertFoundation(state) {
  if (state.settings?.enabled !== true || state.settings?.status !== "ready" ||
      state.settings?.support_subaddress !== true) {
    throw new Error("Cloudflare Email Routing or subaddressing was not ready");
  }
  if (state.summary.catch_all.enabled) {
    throw new Error("primary canary requires the public catch-all to remain disabled");
  }
  if (state.summary.role_routes.ready !== true) {
    throw new Error("postmaster and abuse operator routes were not ready");
  }
}

function assertActionPreconditions(action, state, readiness) {
  // Disable is the emergency fail-closed path. An unmanaged or malformed
  // conflict must never prevent us from turning off the exact rules we own;
  // the conflict remains visible in the fenced state and receipt.
  if (action !== "disable") assertCommonMutationState(state);
  if (action === "prepare" || action === "activate") {
    assertFoundation(state);
    if (!readiness || readiness.projection_count < 1) {
      throw new Error("signed canonical projection readiness was missing");
    }
    if (state.indexed.stale.length !== 0) {
      throw new Error("stale primary canary rules exist outside the manifest");
    }
  }
  if (action === "prepare" &&
      [...state.indexed.byAddress.values()].some((rule) => rule.enabled !== false)) {
    throw new Error("prepare requires every existing canary rule to be disabled");
  }
  if (action === "activate") {
    if (state.indexed.byAddress.size !== state.manifest.agents.length ||
        state.indexed.legacy.length !== 0 ||
        [...state.indexed.byAddress.values()].some((rule) => rule.enabled !== false)) {
      throw new Error("activate requires a complete disabled primary canary");
    }
    if (readiness.activation_ready !== true) {
      throw new Error("canonical delivery gates were not ready for activation");
    }
  }
  if (action === "remove" && state.indexed.owned.some((rule) => rule.enabled !== false)) {
    throw new Error("remove requires a separately reviewed disable first");
  }
}

function planBody(api, action, manifest, routing, readiness, createdAt) {
  return {
    schema: PRIMARY_PLAN_SCHEMA,
    action,
    domain: manifest.domain,
    worker: manifest.worker_name,
    created_at: createdAt.toISOString(),
    expires_at: new Date(createdAt.valueOf() + PLAN_LIFETIME_MS).toISOString(),
    target: {
      account_id: api.accountID,
      zone_id: api.zoneID,
      route_directory_id: api.namespaceID,
    },
    manifest,
    precondition: {
      routing: routing.summary,
      ...(readiness ? { readiness } : {}),
    },
    checks: [
      "target_ids_exact",
      "shared_durable_operations_lease",
      "catch_all_fingerprint_preserved",
      "operator_role_fingerprint_preserved",
      "managed_rule_ownership_exact",
      ...(action === "prepare" || action === "activate"
        ? [
          "email_routing_ready",
          "subaddressing_enabled",
          "signed_control_plane_and_kv_projection_equal",
          "canonical_projection_fresh_and_applied",
          "control_plane_canonical_gates_bound",
          "managed_alias_delivery_disabled",
        ]
        : []),
      ...(action === "activate" ? ["email_edge_canonical_delivery_enabled"] : []),
    ],
  };
}

function withFence(body) {
  return Object.freeze({
    ...body,
    apply_fence: {
      algorithm: "sha256",
      sha256: sha256(canonicalJSON(body)),
    },
  });
}

export async function createPrimaryRoutingPlan(api, runtime, manifestInput, action, {
  now = () => new Date(),
  createdAt = null,
} = {}) {
  if (!ACTIONS.has(action)) throw new Error("primary routing action was invalid");
  const clock = now();
  if (!(clock instanceof Date) || !Number.isFinite(clock.valueOf())) {
    throw new Error("primary routing planner clock was invalid");
  }
  const creation = createdAt === null ? clock : new Date(createdAt);
  if (!Number.isFinite(creation.valueOf())) {
    throw new Error("primary routing plan creation time was invalid");
  }
  const manifest = normalizePrimaryCanaryManifest(manifestInput);
  const routing = await capturePrimaryRoutingState(api, manifest);
  const readiness = action === "prepare" || action === "activate"
    ? await inspectPrimaryReadiness(api, runtime, manifest, { now: () => clock })
    : null;
  const fencedReadiness = readiness
    ? Object.freeze({ ...readiness, checked_at: creation.toISOString() })
    : null;
  assertActionPreconditions(action, routing, fencedReadiness);
  return withFence(planBody(api, action, manifest, routing, fencedReadiness, creation));
}

export function verifyPrimaryRoutingPlan(plan, suppliedSHA256, {
  now = () => new Date(),
} = {}) {
  exactKeys(plan, [
    "schema", "action", "domain", "worker", "created_at", "expires_at",
    "target", "manifest", "precondition", "checks", "apply_fence",
  ], "primary routing plan");
  exactKeys(plan.target, ["account_id", "zone_id", "route_directory_id"], "primary routing target");
  exactKeys(plan.apply_fence, ["algorithm", "sha256"], "primary routing apply fence");
  const manifest = normalizePrimaryCanaryManifest(plan.manifest);
  const clock = now();
  if (plan.schema !== PRIMARY_PLAN_SCHEMA || !ACTIONS.has(plan.action) ||
      plan.domain !== manifest.domain || plan.worker !== manifest.worker_name ||
      !RFC3339_UTC.test(plan.created_at) || !RFC3339_UTC.test(plan.expires_at) ||
      !Number.isFinite(Date.parse(plan.created_at)) ||
      Date.parse(plan.expires_at) !== Date.parse(plan.created_at) + PLAN_LIFETIME_MS ||
      !(clock instanceof Date) || !Number.isFinite(clock.valueOf()) ||
      Date.parse(plan.created_at) > clock.valueOf() + 300_000 ||
      clock.valueOf() > Date.parse(plan.expires_at) ||
      !ID.test(plan.target.account_id) || !ID.test(plan.target.zone_id) ||
      !ID.test(plan.target.route_directory_id) ||
      !Array.isArray(plan.checks) || plan.checks.length < 4 ||
      plan.apply_fence.algorithm !== "sha256" ||
      !SHA256.test(plan.apply_fence.sha256)) {
    throw new Error("primary routing plan was malformed or expired");
  }
  const { apply_fence: ignored, ...body } = plan;
  const calculated = sha256(canonicalJSON(body));
  if (calculated !== plan.apply_fence.sha256) {
    throw new Error("primary routing plan fence did not match its content");
  }
  if (!SHA256.test(String(suppliedSHA256 ?? "")) || calculated !== suppliedSHA256) {
    throw new Error("--plan-sha256 did not match the exact primary routing plan");
  }
  return calculated;
}

function exactPlan(actual, expected) {
  if (canonicalJSON(actual) !== canonicalJSON(expected)) {
    throw new Error("primary routing plan preconditions changed; create and review a new plan");
  }
}

async function leasedMutation(leaseGuard, mutation) {
  await leaseGuard.renew();
  const result = await mutation();
  await leaseGuard.renew();
  return result;
}

async function setOwnedRuleState(api, manifest, enabled, leaseGuard) {
  const state = await capturePrimaryRoutingState(api, manifest);
  for (const rule of state.indexed.owned) {
    if (rule.enabled !== enabled) {
      await leasedMutation(
        leaseGuard,
        () => api.updateRule(
          rule.id,
          desiredPrimaryRule(manifest, literalAddress(rule), enabled, rule.priority),
        ),
      );
    }
  }
}

async function activateManifestRules(api, manifest, leaseGuard) {
  const state = await capturePrimaryRoutingState(api, manifest);
  assertCommonMutationState(state);
  if (state.indexed.legacy.length !== 0 || state.indexed.stale.length !== 0 ||
      state.indexed.byAddress.size !== manifest.agents.length) {
    throw new Error("primary canary changed before activation");
  }
  for (const rule of state.indexed.byAddress.values()) {
    if (rule.enabled !== true) {
      await leasedMutation(
        leaseGuard,
        () => api.updateRule(
          rule.id,
          desiredPrimaryRule(manifest, literalAddress(rule), true, rule.priority),
        ),
      );
    }
  }
}

async function failClosed(api, manifest, leaseGuard) {
  await setOwnedRuleState(api, manifest, false, leaseGuard);
}

function guardsEqual(left, right) {
  return left.summary.catch_all.sha256 === right.summary.catch_all.sha256 &&
    left.summary.role_routes.sha256 === right.summary.role_routes.sha256;
}

async function mutatePrimaryRules(
  api,
  manifest,
  action,
  before,
  leaseGuard,
) {
  let operationError;
  try {
    if (action === "prepare") {
      for (const agent of manifest.agents) {
        const existing = before.indexed.byAddress.get(agent.address);
        if (!existing) {
          await leasedMutation(
            leaseGuard,
            () => api.createRule(
              desiredPrimaryRule(manifest, agent.address, false),
            ),
          );
        } else if (!desiredWorkerAction(existing, manifest.worker_name)) {
          await leasedMutation(
            leaseGuard,
            () => api.updateRule(
              existing.id,
              desiredPrimaryRule(
                manifest,
                agent.address,
                false,
                existing.priority,
              ),
            ),
          );
        }
      }
    } else if (action === "activate") {
      await activateManifestRules(api, manifest, leaseGuard);
    } else if (action === "disable") {
      await failClosed(api, manifest, leaseGuard);
    } else if (action === "remove") {
      await failClosed(api, manifest, leaseGuard);
      const disabled = await capturePrimaryRoutingState(api, manifest);
      for (const rule of disabled.indexed.owned) {
        await leasedMutation(
          leaseGuard,
          () => api.deleteRule(rule.id),
        );
      }
    }
  } catch (error) {
    operationError = error;
  }

  let rollbackError;
  if (operationError) {
    try {
      await failClosed(api, manifest, leaseGuard);
    } catch (error) {
      rollbackError = error;
    }
  }
  let after;
  let guardError;
  try {
    after = await capturePrimaryRoutingState(api, manifest);
    if (!guardsEqual(before, after)) {
      throw new Error("catch-all or operator role route changed during primary routing mutation");
    }
  } catch (error) {
    guardError = error;
  }
  if (guardError && !operationError) {
    try {
      await failClosed(api, manifest, leaseGuard);
    } catch (error) {
      rollbackError = rollbackError ?? error;
    }
  }
  const errors = [operationError, guardError, rollbackError].filter(Boolean);
  if (errors.length > 1) {
    throw new AggregateError(errors, "primary routing mutation failed and recovery was incomplete");
  }
  if (errors.length === 1) throw errors[0];
  return after;
}

function assertPostcondition(action, state) {
  if (action !== "disable" && state.indexed.conflicts.length !== 0) {
    throw new Error("primary routing postcondition found a rule conflict");
  }
  if (action === "prepare" &&
      (state.indexed.byAddress.size !== state.manifest.agents.length ||
       [...state.indexed.byAddress.values()].some((rule) => rule.enabled !== false) ||
       state.indexed.legacy.length !== 0 ||
       state.indexed.stale.length !== 0)) {
    throw new Error("primary routing prepare postcondition failed");
  }
  if (action === "activate" &&
      (state.indexed.byAddress.size !== state.manifest.agents.length ||
       [...state.indexed.byAddress.values()].some((rule) => rule.enabled !== true) ||
       state.indexed.legacy.length !== 0 ||
       state.indexed.stale.length !== 0)) {
    throw new Error("primary routing activation postcondition failed");
  }
  if (action === "disable" && state.indexed.owned.some((rule) => rule.enabled !== false)) {
    throw new Error("primary routing disable postcondition failed");
  }
  if (action === "remove" && state.indexed.owned.length !== 0) {
    throw new Error("primary routing remove postcondition failed");
  }
}

function ownedRuleEvidence(state) {
  return state.indexed.owned.map((rule) => Object.freeze({
    id: String(rule.id ?? ""),
    enabled: rule.enabled === true,
    sha256: sha256(canonicalJSON(rule)),
  })).sort((left, right) => left.id.localeCompare(right.id));
}

export async function applyPrimaryRoutingPlan(plan, suppliedSHA256, api, runtime, {
  now = () => new Date(),
} = {}) {
  const fence = verifyPrimaryRoutingPlan(plan, suppliedSHA256, { now });
  if (api.accountID !== plan.target.account_id || api.zoneID !== plan.target.zone_id ||
      api.namespaceID !== plan.target.route_directory_id) {
    throw new Error("primary routing plan targeted another Cloudflare resource");
  }
  if (!runtime?.operationsLease ||
      typeof runtime.operationsLease.run !== "function") {
    throw new Error("primary routing durable operations lease was unavailable");
  }
  return runtime.operationsLease.run(
    "primary_routing_apply",
    async (leaseGuard) => {
      const reviewed = await createPrimaryRoutingPlan(
        api,
        runtime,
        plan.manifest,
        plan.action,
        { now, createdAt: plan.created_at },
      );
      exactPlan(reviewed, plan);
      const before = await capturePrimaryRoutingState(api, plan.manifest);
      if (canonicalJSON(before.summary) !==
          canonicalJSON(plan.precondition.routing)) {
        throw new Error("primary routing state changed immediately before mutation");
      }
      await leaseGuard.renew();
      const after = await mutatePrimaryRules(
        api,
        plan.manifest,
        plan.action,
        before,
        leaseGuard,
      );
      try {
        assertPostcondition(plan.action, after);
      } catch (error) {
        let recoveryError;
        try {
          await failClosed(api, plan.manifest, leaseGuard);
        } catch (recovery) {
          recoveryError = recovery;
        }
        if (recoveryError) {
          throw new AggregateError(
            [error, recoveryError],
            "primary routing postcondition failed and recovery was incomplete",
          );
        }
        throw error;
      }
      await leaseGuard.renew();
      const leaseEvidence = leaseGuard.evidence();
      validateAgentEmailOperationsLeaseEvidence(
        leaseEvidence,
        "primary_routing_apply",
      );
      const body = {
        schema: PRIMARY_RECEIPT_SCHEMA,
        outcome: "verified",
        action: plan.action,
        domain: plan.domain,
        worker: plan.worker,
        plan_sha256: fence,
        target: plan.target,
        operations_lease: leaseEvidence,
        before_rules: ownedRuleEvidence(before),
        after_rules: ownedRuleEvidence(after),
        rules: after.summary.managed_rules,
        guards: {
          catch_all_sha256: after.summary.catch_all.sha256,
          role_routes_sha256: after.summary.role_routes.sha256,
        },
      };
      return Object.freeze({
        ...body,
        receipt_fence: {
          algorithm: "sha256",
          sha256: sha256(canonicalJSON(body)),
        },
      });
    },
  );
}

export async function inspectPrimaryCanary(api, runtime, manifestInput, {
  now = () => new Date(),
} = {}) {
  const routing = await capturePrimaryRoutingState(api, manifestInput);
  const readiness = await inspectPrimaryReadiness(api, runtime, routing.manifest, { now });
  const clean = routing.indexed.conflicts.length === 0 && routing.indexed.stale.length === 0;
  const foundation = routing.settings?.enabled === true &&
    routing.settings?.status === "ready" &&
    routing.settings?.support_subaddress === true &&
    routing.summary.catch_all.enabled === false &&
    routing.summary.role_routes.ready === true;
  return Object.freeze({
    schema: PRIMARY_STATUS_SCHEMA,
    domain: routing.manifest.domain,
    worker: routing.manifest.worker_name,
    addresses: routing.manifest.agents.length,
    routing: routing.summary,
    readiness: summarizePrimaryReadiness(readiness),
    ready_for_prepare: clean && foundation &&
      [...routing.indexed.byAddress.values()].every((rule) => rule.enabled === false),
    ready_for_activate: clean && foundation && readiness.activation_ready &&
      routing.indexed.legacy.length === 0 &&
      routing.indexed.byAddress.size === routing.manifest.agents.length &&
      [...routing.indexed.byAddress.values()].every((rule) => rule.enabled === false),
  });
}

export const primaryRoutingInternals = Object.freeze({
  catchAllState,
  indexPrimaryRules,
  literalAddress,
  roleRouteState,
});
