#!/usr/bin/env node
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
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
  assertProductionCloudflareIdentity,
  sanitizedWranglerEnvironment,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";
import { PRODUCTION_RECEIVE_WORKER } from "../src/worker-names.mjs";

const WORKER_NAME = PRODUCTION_RECEIVE_WORKER;
const OPERATIONS_LEASE_ENDPOINT = "https://self.witwave.ai";
const PLAN_SCHEMA = "witself.agent-email-edge-rollback-plan.v1";
const RECEIPT_SCHEMA = "witself.agent-email-edge-rollback-receipt.v1";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SHA256 = /^[0-9a-f]{64}$/;
const OPAQUE_ETAG = /^[0-9A-Za-z._:-]{16,256}$/;
const COMMIT = /^[0-9a-f]{40}$/;
const VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const RFC3339_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/;
const EXPECTED_COMPATIBILITY_DATE = "2026-07-21";
const EXPECTED_COMPATIBILITY_FLAGS = Object.freeze(["global_fetch_strictly_public"]);
const MANAGED_DELIVERY_COHORT_BINDING =
  "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST";

const REQUIRED_BINDINGS = Object.freeze([
  "AGENT_EMAIL_DOMAIN",
  "AGENT_EMAIL_LEGACY_DOMAINS",
  MANAGED_DELIVERY_COHORT_BINDING,
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

const CONTRACT_PLAIN_BINDINGS = Object.freeze([
  "AGENT_EMAIL_DOMAIN",
  "AGENT_EMAIL_LEGACY_DOMAINS",
  MANAGED_DELIVERY_COHORT_BINDING,
  "CONTROL_PLANE_URL",
  "RELAY_KEY_ID",
]);

const LIMITERS = Object.freeze({
  REALM_ROUTE_COLD_MISS_LIMITER: Object.freeze({
    namespace_id: "2201",
    limit: 10,
    period: 10,
  }),
  REALM_ROUTE_KNOWN_MISS_LIMITER: Object.freeze({
    namespace_id: "2202",
    limit: 100,
    period: 10,
  }),
});

function exactKeys(value, keys, name) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) {
    throw new Error(`${name} was malformed`);
  }
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value).sort().map((key) => [key, canonicalize(value[key])]),
    );
  }
  return value;
}

export function canonicalJSON(value) {
  return JSON.stringify(canonicalize(value));
}

export function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function bindingMap(bindings, label, {
  allowLegacyManagedDeliveryCohort = false,
} = {}) {
  if (!Array.isArray(bindings)) throw new Error(`${label} binding inventory was invalid`);
  const result = new Map();
  for (const binding of bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || !binding.name || result.has(binding.name)) {
      throw new Error(`${label} binding inventory was invalid`);
    }
    if (binding.name.includes("CUSTOM_DOMAIN")) {
      throw new Error(`${label} contained a custom-domain activation binding in dark mode`);
    }
    result.set(binding.name, binding);
  }
  const actual = JSON.stringify([...result.keys()].sort());
  const current = JSON.stringify([...REQUIRED_BINDINGS].sort());
  const legacy = JSON.stringify(REQUIRED_BINDINGS
    .filter((name) => name !== MANAGED_DELIVERY_COHORT_BINDING)
    .sort());
  const legacyManagedDeliveryCohort =
    allowLegacyManagedDeliveryCohort && actual === legacy;
  if (actual !== current && !legacyManagedDeliveryCohort) {
    throw new Error(`${label} binding inventory did not match the dark Worker contract`);
  }
  return Object.freeze({
    bindings: result,
    legacyManagedDeliveryCohort,
  });
}

function plain(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    throw new Error(`${label} binding ${name} was missing or invalid`);
  }
  return binding.text;
}

function secret(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    throw new Error(`${label} secret binding ${name} was missing or invalid`);
  }
}

function managedBoolean(bindings, name, label) {
  const value = plain(bindings, name, label);
  if (value !== "true" && value !== "false") {
    throw new Error(`${label} managed binding ${name} was not explicitly true or false`);
  }
  return value;
}

function keyring(bindings, label) {
  const raw = plain(bindings, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", label);
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error(`${label} route-verification keyring was missing or invalid`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${label} route-verification keyring was missing or invalid`);
  }
  const entries = Object.entries(parsed)
    .sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0);
  if (entries.length < 1 || entries.length > 4 || entries.some(([keyID, encoded]) =>
    !/^[a-z][a-z0-9_-]{0,63}$/.test(keyID) ||
    typeof encoded !== "string" ||
    !/^[A-Za-z0-9+/]{43}=$/.test(encoded) ||
    Buffer.from(encoded, "base64").byteLength !== 32 ||
    Buffer.from(encoded, "base64").toString("base64") !== encoded)) {
    throw new Error(`${label} route-verification keyring was missing or invalid`);
  }
  const canonical = Object.fromEntries(entries);
  if (raw !== JSON.stringify(canonical)) {
    throw new Error(`${label} route-verification keyring was not canonical`);
  }
  return canonical;
}

function releaseIdentity(bindings, label) {
  const version = plain(bindings, "WITSELF_EDGE_RELEASE_VERSION", label);
  const commit = plain(bindings, "WITSELF_EDGE_RELEASE_COMMIT", label);
  const date = plain(bindings, "WITSELF_EDGE_RELEASE_DATE", label);
  if (!VERSION.test(version) || !COMMIT.test(commit) || !RFC3339.test(date) ||
      !Number.isFinite(Date.parse(date))) {
    throw new Error(`${label} immutable release identity was missing or invalid`);
  }
  return { version, commit, date };
}

function limiter(bindings, name, label) {
  const binding = bindings.get(name);
  const expected = LIMITERS[name];
  if (binding?.type !== "ratelimit" ||
      binding.namespace_id !== expected.namespace_id ||
      binding.simple?.limit !== expected.limit ||
      binding.simple?.period !== expected.period) {
    throw new Error(`${label} rate-limit binding ${name} drifted`);
  }
  return expected;
}

function inspectVersion(version, label, {
  allowLegacyManagedDeliveryCohort = false,
} = {}) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      !UUID.test(String(version.id ?? "")) ||
      !Number.isSafeInteger(version.number) || version.number < 1) {
    throw new Error(`${label} Worker version identity was invalid`);
  }
  const script = version.resources?.script;
  if (!script || !OPAQUE_ETAG.test(String(script.etag ?? "")) ||
      JSON.stringify(script.handlers) !== JSON.stringify(["email"])) {
    throw new Error(`${label} Worker version was not an email-only artifact`);
  }
  const runtime = version.resources?.script_runtime;
  if (!runtime || runtime.compatibility_date !== EXPECTED_COMPATIBILITY_DATE ||
      JSON.stringify(runtime.compatibility_flags) !==
        JSON.stringify(EXPECTED_COMPATIBILITY_FLAGS)) {
    throw new Error(`${label} Worker compatibility contract drifted`);
  }

  const inventory = bindingMap(version.resources?.bindings, label, {
    allowLegacyManagedDeliveryCohort,
  });
  const { bindings, legacyManagedDeliveryCohort } = inventory;
  const release = releaseIdentity(bindings, label);
  if (plain(bindings, "AGENT_EMAIL_DOMAIN", label) !== "witmail.net" ||
      plain(bindings, "AGENT_EMAIL_LEGACY_DOMAINS", label) !==
        "agent-mail.witwave.ai") {
    throw new Error(`${label} Worker domain contract drifted`);
  }
  const controlPlaneOrigin = plain(bindings, "CONTROL_PLANE_URL", label);
  let controlPlaneURL;
  try {
    controlPlaneURL = new URL(controlPlaneOrigin);
  } catch {
    throw new Error(`${label} control-plane origin was invalid`);
  }
  if (controlPlaneURL.protocol !== "https:" || controlPlaneURL.username ||
      controlPlaneURL.password || controlPlaneURL.search || controlPlaneURL.hash ||
      !controlPlaneURL.hostname || controlPlaneURL.hostname === "localhost" ||
      controlPlaneURL.toString() !== controlPlaneOrigin ||
      controlPlaneOrigin !== `${OPERATIONS_LEASE_ENDPOINT}/`) {
    throw new Error(`${label} control-plane origin was not canonical`);
  }
  if (!/^[a-z][a-z0-9_-]{0,63}$/.test(plain(bindings, "RELAY_KEY_ID", label))) {
    throw new Error(`${label} relay key identifier was invalid`);
  }
  const managedDeliveryAccountAllowlist = legacyManagedDeliveryCohort
    ? ""
    : plain(bindings, MANAGED_DELIVERY_COHORT_BINDING, label);
  parseManagedDeliveryAccountAllowlist(managedDeliveryAccountAllowlist);
  const directory = bindings.get("EMAIL_DIRECTORY");
  if (directory?.type !== "kv_namespace" ||
      !/^[0-9a-f]{32}$/.test(String(directory.namespace_id ?? ""))) {
    throw new Error(`${label} directory KV binding was missing or invalid`);
  }
  const metrics = bindings.get("EMAIL_EDGE_METRICS");
  if (metrics?.type !== "analytics_engine" ||
      metrics.dataset !== "witself_agent_email_edge") {
    throw new Error(`${label} metrics dataset binding drifted`);
  }
  secret(bindings, "CONTROL_PLANE_EDGE_TOKEN", label);
  secret(bindings, "RELAY_ED25519_PRIVATE_KEY", label);

  const contract = {
    compatibility_date: runtime.compatibility_date,
    compatibility_flags: [...runtime.compatibility_flags],
    plain_bindings: Object.fromEntries(
      CONTRACT_PLAIN_BINDINGS.map((name) => [
        name,
        name === MANAGED_DELIVERY_COHORT_BINDING
          ? managedDeliveryAccountAllowlist
          : plain(bindings, name, label),
      ]),
    ),
    directory_namespace_id: directory.namespace_id,
    metrics_dataset: metrics.dataset,
    managed_delivery_flags: {
      alias: managedBoolean(bindings, "REALM_EMAIL_ALIAS_DELIVERY_ENABLED", label),
      canonical: managedBoolean(bindings, "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", label),
    },
    route_verification_keyring: keyring(bindings, label),
    rate_limiters: Object.fromEntries(
      Object.keys(LIMITERS).sort().map((name) => [name, limiter(bindings, name, label)]),
    ),
    secret_bindings: ["CONTROL_PLANE_EDGE_TOKEN", "RELAY_ED25519_PRIVATE_KEY"],
  };
  return Object.freeze({
    id: version.id,
    number: version.number,
    scriptETag: script.etag,
    release,
    contract,
    contractSHA256: sha256(canonicalJSON(contract)),
    legacyManagedDeliveryCohort,
  });
}

function assertLegacyManagedDeliveryIsDark(version, label) {
  if (!version.legacyManagedDeliveryCohort) return;
  if (version.contract.plain_bindings[MANAGED_DELIVERY_COHORT_BINDING] !== "" ||
      version.contract.managed_delivery_flags.alias !== "false" ||
      version.contract.managed_delivery_flags.canonical !== "false") {
    throw new Error(
      `${label} legacy managed-delivery contract is eligible only while fully dark`,
    );
  }
}

function currentVersionID(status) {
  if (!status || typeof status !== "object" || Array.isArray(status) ||
      !UUID.test(String(status.id ?? "")) || status.strategy !== "percentage" ||
      !Array.isArray(status.versions) || status.versions.length !== 1 ||
      status.versions[0]?.percentage !== 100 ||
      !UUID.test(String(status.versions[0]?.version_id ?? ""))) {
    throw new Error("current Worker deployment was not one version at 100 percent");
  }
  return status.versions[0].version_id;
}

function planBody(status, current, candidate, createdAt) {
  return {
    schema: PLAN_SCHEMA,
    action: "deploy_candidate_at_100_percent",
    worker: WORKER_NAME,
    mode: "custom_domain_dark",
    created_at: createdAt,
    current: {
      deployment_id: status.id,
      version_id: current.id,
    },
    candidate: {
      version_id: candidate.id,
    },
    invariant_contract_sha256: current.contractSHA256,
    checks: [
      "single_current_version",
      "email_only_handlers",
      "compatibility_exact",
      "binding_inventory_exact",
      "directory_kv_exact",
      "metrics_dataset_exact",
      "rate_limiters_exact",
      "managed_delivery_flags_exact",
      "legacy_managed_delivery_candidate_dark",
      "route_verification_keyring_exact",
      "custom_domain_activation_absent",
      "immutable_release_identity_present",
    ],
  };
}

function withFence(body) {
  const fence = sha256(canonicalJSON(body));
  return Object.freeze({
    ...body,
    apply_fence: {
      algorithm: "sha256",
      sha256: fence,
    },
  });
}

export function createRollbackPlan(status, currentVersion, candidateVersion, {
  now = () => new Date(),
} = {}) {
  const activeVersionID = currentVersionID(status);
  const current = inspectVersion(currentVersion, "current", {
    allowLegacyManagedDeliveryCohort: true,
  });
  const candidate = inspectVersion(candidateVersion, "candidate", {
    allowLegacyManagedDeliveryCohort: true,
  });
  assertLegacyManagedDeliveryIsDark(current, "current");
  assertLegacyManagedDeliveryIsDark(candidate, "candidate");
  if (candidate.legacyManagedDeliveryCohort &&
      (current.contract.plain_bindings[MANAGED_DELIVERY_COHORT_BINDING] !== "" ||
       current.contract.managed_delivery_flags.alias !== "false" ||
       current.contract.managed_delivery_flags.canonical !== "false")) {
    throw new Error(
      "legacy managed-delivery rollback candidate requires an empty current cohort and both current delivery gates false",
    );
  }
  if (current.id !== activeVersionID) {
    throw new Error("current Worker version did not match the active deployment");
  }
  if (candidate.id === current.id || candidate.number >= current.number) {
    throw new Error("candidate was not an older distinct Worker version");
  }
  if (candidate.contractSHA256 !== current.contractSHA256) {
    throw new Error("candidate operational contract drifted from the current Worker");
  }
  const date = now();
  if (!(date instanceof Date) || !Number.isFinite(date.valueOf())) {
    throw new Error("rollback planner clock was invalid");
  }
  return withFence(planBody(status, current, candidate, date.toISOString()));
}

export function verifyPlan(plan, suppliedSHA256) {
  exactKeys(plan, [
    "schema", "action", "worker", "mode", "created_at", "current", "candidate",
    "invariant_contract_sha256", "checks", "apply_fence",
  ], "rollback plan");
  exactKeys(plan.current, ["deployment_id", "version_id"], "rollback plan current fence");
  exactKeys(plan.candidate, ["version_id"], "rollback plan candidate fence");
  exactKeys(plan.apply_fence, ["algorithm", "sha256"], "rollback plan apply fence");
  const expectedChecks = planBody(
    { id: plan.current.deployment_id },
    { id: plan.current.version_id, contractSHA256: plan.invariant_contract_sha256 },
    { id: plan.candidate.version_id },
    plan.created_at,
  ).checks;
  if (plan.schema !== PLAN_SCHEMA ||
      plan.action !== "deploy_candidate_at_100_percent" ||
      plan.worker !== WORKER_NAME || plan.mode !== "custom_domain_dark" ||
      !RFC3339_UTC.test(plan.created_at) || !Number.isFinite(Date.parse(plan.created_at)) ||
      !UUID.test(plan.current.deployment_id) || !UUID.test(plan.current.version_id) ||
      !UUID.test(plan.candidate.version_id) ||
      plan.current.version_id === plan.candidate.version_id ||
      !SHA256.test(plan.invariant_contract_sha256) ||
      JSON.stringify(plan.checks) !== JSON.stringify(expectedChecks) ||
      plan.apply_fence.algorithm !== "sha256" ||
      !SHA256.test(plan.apply_fence.sha256)) {
    throw new Error("rollback plan was malformed");
  }
  const { apply_fence: ignored, ...body } = plan;
  const calculated = sha256(canonicalJSON(body));
  if (calculated !== plan.apply_fence.sha256) {
    throw new Error("rollback plan apply fence did not match its content");
  }
  if (!SHA256.test(String(suppliedSHA256 ?? "")) || suppliedSHA256 !== calculated) {
    throw new Error("--plan-sha256 did not match the exact rollback plan");
  }
  return calculated;
}

function exactPlan(actual, expected) {
  if (canonicalJSON(actual) !== canonicalJSON(expected)) {
    throw new Error("rollback plan preconditions changed; create and review a new plan");
  }
}

export async function applyRollbackPlan(plan, suppliedSHA256, runtime) {
  const fence = verifyPlan(plan, suppliedSHA256);
  if (!runtime || typeof runtime.loadStatus !== "function" ||
      typeof runtime.loadVersion !== "function" || typeof runtime.deploy !== "function" ||
      !runtime.operationsLease || typeof runtime.operationsLease.run !== "function") {
    throw new Error("rollback apply runtime was invalid");
  }
  return runtime.operationsLease.run(
    "email_edge_rollback",
    async (leaseGuard) => {
      if (!leaseGuard || typeof leaseGuard.renew !== "function" ||
          !leaseGuard.signal || typeof leaseGuard.signal.addEventListener !== "function") {
        throw new Error("rollback operations lease guard was invalid");
      }

      // Reconstruct the complete reviewed plan only after acquiring the global
      // lease. No supported deployment or provider-routing mutation can now
      // move the final precondition between this read and the provider write.
      const status = await runtime.loadStatus();
      const current = await runtime.loadVersion(plan.current.version_id);
      const candidate = await runtime.loadVersion(plan.candidate.version_id);
      const reviewed = createRollbackPlan(status, current, candidate, {
        now: () => new Date(plan.created_at),
      });
      exactPlan(reviewed, plan);

      await leaseGuard.renew();
      await runtime.deploy(plan.candidate.version_id, {
        signal: leaseGuard.signal,
      });
      await leaseGuard.renew();

      const postStatus = await runtime.loadStatus();
      const postVersionID = currentVersionID(postStatus);
      if (postVersionID !== plan.candidate.version_id) {
        throw new Error("rollback mutation completed without the candidate at 100 percent");
      }
      const postVersion = inspectVersion(
        await runtime.loadVersion(plan.candidate.version_id),
        "post-rollback",
        { allowLegacyManagedDeliveryCohort: true },
      );
      const reviewedCandidate = inspectVersion(candidate, "candidate", {
        allowLegacyManagedDeliveryCohort: true,
      });
      if (canonicalJSON(postVersion) !== canonicalJSON(reviewedCandidate)) {
        throw new Error("post-rollback Worker version did not match the reviewed candidate");
      }
      // Prove the exact fence once more after provider readback. The shared
      // client performs its own final renewal and exact release after return.
      await leaseGuard.renew();
      return Object.freeze({
        schema: RECEIPT_SCHEMA,
        outcome: "verified",
        worker: WORKER_NAME,
        plan_sha256: fence,
        prior: {
          deployment_id: plan.current.deployment_id,
          version_id: plan.current.version_id,
        },
        active: {
          deployment_id: postStatus.id,
          version_id: postVersionID,
        },
      });
    },
  );
}

function wranglerJSON(args, environment = process.env) {
  const result = spawnSync("wrangler", args, {
    encoding: "utf8",
    env: sanitizedWranglerInspectionEnvironment(environment),
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

export function rollbackDeploymentArguments(versionID) {
  if (!UUID.test(String(versionID ?? ""))) {
    throw new Error("rollback candidate Worker version id was invalid");
  }
  return withReviewedWranglerEnvironmentFile([
    "versions", "deploy", `${versionID}@100`, "--name", WORKER_NAME,
    "--message", `Guarded rollback to ${versionID}`,
  ]);
}

export function rollbackOperationsLeaseRuntime(
  environment = process.env,
  {
    fetchImpl = globalThis.fetch,
    randomUUIDImpl,
  } = {},
) {
  // Rollback is intrinsically scoped to the production witmail.net Worker.
  // Copy only its authentication input and pin the matching production
  // control-plane authority; inherited endpoint selectors cannot redirect the
  // fencing request to an attacker-controlled lease service.
  const leaseEnvironment = Object.freeze({
    CONTROL_PLANE_EDGE_TOKEN: environment.CONTROL_PLANE_EDGE_TOKEN,
  });
  return Object.freeze({
    run: (operation, work) => withAgentEmailOperationsLease(operation, work, {
      env: leaseEnvironment,
      endpoint: OPERATIONS_LEASE_ENDPOINT,
      fetchImpl,
      ...(randomUUIDImpl ? { randomUUIDImpl } : {}),
    }),
  });
}

export function rollbackLiveRuntime(
  environment = process.env,
  {
    inspect = wranglerJSON,
    interactive = () => process.stdin.isTTY === true && process.stdout.isTTY === true,
    operationsLease = rollbackOperationsLeaseRuntime(environment),
    runCommand = runLeaseGuardedCommand,
  } = {},
) {
  assertProductionCloudflareIdentity(environment);
  if (typeof inspect !== "function" || typeof interactive !== "function" ||
      !operationsLease || typeof operationsLease.run !== "function" ||
      typeof runCommand !== "function") {
    throw new Error("production rollback runtime was invalid");
  }
  const inspectionEnvironment = Object.freeze(
    sanitizedWranglerInspectionEnvironment(environment),
  );
  const mutationEnvironment = Object.freeze(
    sanitizedWranglerEnvironment(environment),
  );
  return {
    loadStatus: async () => inspect(withReviewedWranglerEnvironmentFile([
      "deployments", "status", "--name", WORKER_NAME, "--json",
    ]), inspectionEnvironment),
    loadVersion: async (versionID) => inspect(withReviewedWranglerEnvironmentFile([
      "versions", "view", versionID, "--name", WORKER_NAME, "--json",
    ]), inspectionEnvironment),
    deploy: async (versionID, { signal } = {}) => {
      // Worker versions capture encrypted secret values. Wrangler deliberately
      // asks for an extra confirmation when a candidate would restore an older
      // secret. Never auto-accept that warning: a non-interactive rollback
      // could silently reactivate a compromised relay or fallback credential.
      if (!interactive()) {
        throw new Error(
          "rollback apply requires an interactive terminal for Cloudflare secret continuity review",
        );
      }
      await runCommand(
        "wrangler",
        rollbackDeploymentArguments(versionID),
        {
          env: mutationEnvironment,
          signal,
          timeoutMs: 20 * 60_000,
        },
      );
    },
    operationsLease,
  };
}

export function parseArgs(argv) {
  const result = {
    apply: false,
    candidateVersion: "",
    output: "",
    plan: "",
    planSHA256: "",
  };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--apply") result.apply = true;
    else if (["--candidate-version", "--output", "--plan", "--plan-sha256"].includes(argument)) {
      const value = argv[index + 1];
      if (!value || value.startsWith("--")) throw new Error(`${argument} requires a value`);
      index += 1;
      if (argument === "--candidate-version") result.candidateVersion = value;
      if (argument === "--output") result.output = value;
      if (argument === "--plan") result.plan = value;
      if (argument === "--plan-sha256") result.planSHA256 = value;
    } else {
      throw new Error(`unknown argument ${argument}`);
    }
  }
  if (result.apply) {
    if (!result.plan || !result.planSHA256 || result.candidateVersion || result.output) {
      throw new Error("--apply requires only --plan FILE and --plan-sha256 SHA256");
    }
  } else if (!UUID.test(result.candidateVersion) || result.plan || result.planSHA256) {
    throw new Error("planning requires exactly one --candidate-version VERSION_ID");
  }
  return result;
}

async function main() {
  const options = parseArgs(process.argv.slice(2));
  const runtime = rollbackLiveRuntime();
  if (options.apply) {
    let plan;
    try {
      plan = JSON.parse(readFileSync(resolve(options.plan), "utf8"));
    } catch {
      throw new Error("--plan did not contain valid rollback plan JSON");
    }
    const receipt = await applyRollbackPlan(plan, options.planSHA256, runtime);
    process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
    return;
  }
  const status = await runtime.loadStatus();
  const activeID = currentVersionID(status);
  const [current, candidate] = await Promise.all([
    runtime.loadVersion(activeID),
    runtime.loadVersion(options.candidateVersion),
  ]);
  const plan = createRollbackPlan(status, current, candidate);
  const encoded = `${JSON.stringify(plan, null, 2)}\n`;
  if (options.output) writeFileSync(resolve(options.output), encoded, { flag: "wx", mode: 0o600 });
  else process.stdout.write(encoded);
}

if (process.argv[1] != null && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : "rollback failed"}\n`);
    process.exitCode = 1;
  });
}
