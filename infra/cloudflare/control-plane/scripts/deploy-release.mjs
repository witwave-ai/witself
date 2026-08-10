#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  deploymentMatches,
  expectedBuildMetadata,
  verifyWorkerVersion,
} from "./verify-deployment.mjs";
import {
  sourceIdentity,
  taggedReleaseIdentity,
  workerVersionMessage,
  workerVersionTag,
} from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/agent-email-managed-delivery-cohort.mjs";
import {
  AgentEmailOperationsLeaseClientError,
  withAgentEmailOperationsLease,
} from "./agent-email-operations-lease-client.mjs";
import {
  runLeaseGuardedCommand,
} from "./agent-email-lease-guarded-command.mjs";
import {
  createPrivateDeploymentConfig,
} from "./private-deployment-config.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
export const GENERATED_CONFIG_PATH = join(root, "wrangler.generated.jsonc");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const MANAGED_COHORT_PROTOCOL_RELEASE = "0.0.241";
const MANAGED_COHORT_PREDECESSOR_RELEASE = "0.0.240";
const CANONICAL_CONTROL_PLANE_ORIGIN = "https://self.witwave.ai";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SHA256 = /^[0-9a-f]{64}$/;
const CONTROL_PLANE_DARK_BINDINGS = Object.freeze([
  "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED",
]);

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

function activeVersionID(deployment, label = "email edge") {
  if (!deployment || typeof deployment !== "object" || Array.isArray(deployment) ||
      !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error(`${label} deployment was not one version at 100 percent`);
  }
  return deployment.versions[0].version_id;
}

function assertStableActiveDeployment(first, final, label) {
  const firstVersionID = activeVersionID(first, label);
  const finalVersionID = activeVersionID(final, label);
  if (first.id !== final.id || firstVersionID !== finalVersionID) {
    throw new Error(`${label} deployment changed during exact provider inspection`);
  }
  return firstVersionID;
}

function bindingMap(
  version,
  expectedVersionID,
  label = "email edge",
  expectedHandlers = ["email"],
) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedVersionID ||
      !Array.isArray(version.resources?.bindings) ||
      JSON.stringify(version.resources?.script?.handlers) !==
        JSON.stringify(expectedHandlers)) {
    throw new Error(`active ${label} version had an invalid Worker contract`);
  }
  const bindings = new Map();
  for (const binding of version.resources.bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || binding.name === "" ||
        bindings.has(binding.name)) {
      throw new Error(`active ${label} binding inventory was invalid`);
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

function operationsLeaseOrigin(bindings) {
  const raw = plain(bindings, "CONTROL_PLANE_URL");
  let origin;
  try {
    origin = new URL(raw);
  } catch {
    throw new Error("active email edge operations lease origin was invalid");
  }
  if (origin.protocol !== "https:" || origin.username || origin.password ||
      origin.search || origin.hash || origin.pathname !== "/" ||
      origin.toString() !== raw || origin.origin !== CANONICAL_CONTROL_PLANE_ORIGIN) {
    throw new Error(
      "active email edge operations lease origin did not match the exact control-plane route",
    );
  }
  return origin.origin;
}

// v0.0.241 is the one managed-route wire transition whose new control plane is
// intentionally incompatible with a v0.0.240 edge. A CP-first deployment is
// safe only when the old edge cannot consume a fresh legacy signed-v2 KV row.
// This preflight runs before Wrangler mutates the control plane.
export function verifyManagedCohortProtocolUpgrade(
  targetRelease,
  deployment,
  version,
  targetAllowlist = "",
) {
  if (!releaseAtLeast(targetRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    return Object.freeze({ required: false });
  }
  const targetAccounts = parseManagedDeliveryAccountAllowlist(targetAllowlist);
  const versionID = activeVersionID(deployment, "email edge");
  const bindings = bindingMap(version, versionID);
  const leaseOrigin = operationsLeaseOrigin(bindings);
  const edgeRelease = plain(bindings, "WITSELF_EDGE_RELEASE_VERSION");
  if (releaseAtLeast(edgeRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    const edgeAccounts = parseManagedDeliveryAccountAllowlist(
      plain(bindings, "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST"),
    );
    const target = new Set(targetAccounts);
    if (edgeAccounts.some((accountID) => !target.has(accountID))) {
      throw new Error(
        "control-plane managed cohort must contain the complete active email edge cohort; remove from the edge first",
      );
    }
    return Object.freeze({
      required: true,
      edge_release: edgeRelease,
      already_current: true,
      target_account_count: targetAccounts.length,
      active_edge_account_count: edgeAccounts.length,
      operations_lease_origin: leaseOrigin,
    });
  }
  if (edgeRelease !== MANAGED_COHORT_PREDECESSOR_RELEASE) {
    throw new Error(
      "v0.0.241 control-plane deployment requires a v0.0.240 or v0.0.241 email edge",
    );
  }
  if (targetAccounts.length !== 0) {
    throw new Error(
      "v0.0.241 CP-first deployment over a v0.0.240 email edge requires an empty managed cohort",
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
  return Object.freeze({
    required: true,
    edge_release: edgeRelease,
    already_current: false,
    target_account_count: targetAccounts.length,
    active_edge_account_count: 0,
    operations_lease_origin: leaseOrigin,
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
    throw new Error(`Wrangler ${operation} output was not valid JSON`);
  }
}

export function preflightManagedCohortProtocolUpgrade(
  targetRelease,
  targetAllowlist = "",
  inspect = wranglerJSON,
) {
  if (!releaseAtLeast(targetRelease, MANAGED_COHORT_PROTOCOL_RELEASE)) {
    return Object.freeze({ required: false });
  }
  const deployment = inspect([
    "deployments", "status", "--name", EMAIL_EDGE_WORKER, "--json",
  ], "inspect the active email edge deployment");
  const versionID = activeVersionID(deployment, "email edge");
  const version = inspect([
    "versions", "view", versionID, "--name", EMAIL_EDGE_WORKER, "--json",
  ], "inspect the active email edge version");
  const finalDeployment = inspect([
    "deployments", "status", "--name", EMAIL_EDGE_WORKER, "--json",
  ], "reinspect the active email edge deployment");
  assertStableActiveDeployment(deployment, finalDeployment, "email edge");
  return verifyManagedCohortProtocolUpgrade(
    targetRelease,
    deployment,
    version,
    targetAllowlist,
  );
}

function controlPlaneDurableObjectNamespaces(bindings) {
  const namespaces = [...bindings.values()]
    .filter((binding) => binding.type === "durable_object_namespace")
    .map((binding) => [binding.name, binding.namespace_id])
    .sort(([left], [right]) => left.localeCompare(right));
  const durableObjectNamespaces = Object.fromEntries(namespaces);
  if (durableObjectNamespaces.REALM_EMAIL_ALIASES == null ||
      !namespaces.every(([name, namespaceID]) =>
        /^[A-Z][A-Z0-9_]*$/.test(name) &&
        /^[0-9a-f]{32}$/.test(String(namespaceID ?? "")))) {
    throw new Error(
      "active control plane had an invalid Durable Object namespace inventory",
    );
  }
  return Object.freeze(durableObjectNamespaces);
}

function controlPlaneBootstrapAttestation(
  expected,
  deployment,
  version,
  schema,
  verificationOptions = {},
) {
  const versionID = activeVersionID(deployment, "control-plane");
  const verified = verifyWorkerVersion(
    version,
    expected,
    versionID,
    verificationOptions,
  );
  const bindings = bindingMap(
    version,
    versionID,
    "control-plane",
    ["fetch", "scheduled"],
  );
  const durableObjectNamespaces =
    controlPlaneDurableObjectNamespaces(bindings);
  return Object.freeze({
    schema,
    version_id: verified.version_id,
    script_etag: verified.script_etag,
    operations_lease_namespace_id:
      durableObjectNamespaces.REALM_EMAIL_ALIASES,
    durable_object_namespaces: durableObjectNamespaces,
    release: Object.freeze({
      version: expected.version,
      commit: expected.commit,
      date: expected.date,
    }),
  });
}

export function verifyManagedCohortProtocolBootstrapPredecessor(
  target,
  predecessor,
  deployment,
  version,
) {
  if (target?.version !== MANAGED_COHORT_PROTOCOL_RELEASE ||
      target.managed_delivery_account_allowlist !== "" ||
      predecessor?.version !== MANAGED_COHORT_PREDECESSOR_RELEASE ||
      predecessor.tag !== `v${MANAGED_COHORT_PREDECESSOR_RELEASE}`) {
    throw new Error(
      "control-plane lease bootstrap did not target the exact predecessor transition",
    );
  }
  const expected = {
    ...target,
    version: predecessor.version,
    commit: predecessor.commit,
    date: predecessor.date,
    managed_delivery_account_allowlist: "",
  };
  const attestation = controlPlaneBootstrapAttestation(
    expected,
    deployment,
    version,
    "witself.agent-email-control-plane-bootstrap-predecessor.v1",
    { allowLegacyEmptyManagedDeliveryCohort: true },
  );
  const bindings = bindingMap(
    version,
    attestation.version_id,
    "control-plane",
    ["fetch", "scheduled"],
  );
  if (bindings.has("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST") ||
      CONTROL_PLANE_DARK_BINDINGS.some((name) => bindings.has(name))) {
    throw new Error(
      "v0.0.241 lease bootstrap requires the exact active v0.0.240 control plane with canonical delivery dark and no managed cohort binding",
    );
  }
  return attestation;
}

export function preflightManagedCohortProtocolBootstrapPredecessor(
  target,
  predecessor,
  config = GENERATED_CONFIG_PATH,
  inspect = wranglerJSON,
) {
  const deployment = inspect([
    "deployments", "status", "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane deployment before lease bootstrap");
  const versionID = activeVersionID(deployment, "control-plane");
  const version = inspect([
    "versions", "view", versionID, "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane version before lease bootstrap");
  const finalDeployment = inspect([
    "deployments", "status", "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "reinspect the active control-plane deployment before lease bootstrap");
  assertStableActiveDeployment(
    deployment,
    finalDeployment,
    "control-plane",
  );
  return verifyManagedCohortProtocolBootstrapPredecessor(
    target,
    predecessor,
    deployment,
    version,
  );
}

export function verifyManagedCohortProtocolBootstrapTarget(
  target,
  predecessorAttestation,
  deployment,
  version,
) {
  if (target?.version !== MANAGED_COHORT_PROTOCOL_RELEASE ||
      predecessorAttestation?.schema !==
        "witself.agent-email-control-plane-bootstrap-predecessor.v1") {
    throw new Error("control-plane lease bootstrap target proof was invalid");
  }
  const attestation = controlPlaneBootstrapAttestation(
    target,
    deployment,
    version,
    "witself.agent-email-control-plane-bootstrap-target.v1",
  );
  if (attestation.operations_lease_namespace_id !==
      predecessorAttestation.operations_lease_namespace_id ||
      JSON.stringify(attestation.durable_object_namespaces) !==
        JSON.stringify(predecessorAttestation.durable_object_namespaces)) {
    throw new Error(
      "control-plane lease bootstrap changed its Durable Object namespace inventory",
    );
  }
  return attestation;
}

export function preflightManagedCohortProtocolBootstrapTarget(
  target,
  predecessorAttestation,
  config = GENERATED_CONFIG_PATH,
  inspect = wranglerJSON,
) {
  const deployment = inspect([
    "deployments", "status", "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane bootstrap target deployment");
  const versionID = activeVersionID(deployment, "control-plane");
  const version = inspect([
    "versions", "view", versionID, "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "inspect the active control-plane bootstrap target version");
  const finalDeployment = inspect([
    "deployments", "status", "--config", config,
    "--name", CONTROL_PLANE_WORKER, "--json",
  ], "reinspect the active control-plane bootstrap target deployment");
  assertStableActiveDeployment(
    deployment,
    finalDeployment,
    "control-plane",
  );
  return verifyManagedCohortProtocolBootstrapTarget(
    target,
    predecessorAttestation,
    deployment,
    version,
  );
}

export function verifyManagedCohortProtocolBootstrapConvergence(first, final) {
  if (first?.schema !== "witself.agent-email-control-plane-bootstrap-target.v1" ||
      final?.schema !== first.schema ||
      !SHA256.test(String(first.script_etag ?? "")) ||
      final.script_etag !== first.script_etag ||
      final.operations_lease_namespace_id !==
        first.operations_lease_namespace_id ||
      JSON.stringify(final.durable_object_namespaces) !==
        JSON.stringify(first.durable_object_namespaces) ||
      JSON.stringify(final.release) !== JSON.stringify(first.release)) {
    throw new Error(
      "control-plane bootstrap did not converge on one byte-identical release artifact",
    );
  }
  return Object.freeze({
    script_etag: final.script_etag,
    operations_lease_namespace_id: final.operations_lease_namespace_id,
    durable_object_namespaces: final.durable_object_namespaces,
  });
}

export function isFirstManagedCohortProtocolBootstrap(
  targetRelease,
  preflight,
  predecessor,
) {
  return targetRelease === MANAGED_COHORT_PROTOCOL_RELEASE &&
    preflight?.required === true &&
    preflight.edge_release === MANAGED_COHORT_PREDECESSOR_RELEASE &&
    preflight.already_current === false &&
    preflight.target_account_count === 0 &&
    preflight.active_edge_account_count === 0 &&
    preflight.operations_lease_origin === CANONICAL_CONTROL_PLANE_ORIGIN &&
    predecessor?.schema ===
      "witself.agent-email-control-plane-bootstrap-predecessor.v1" &&
    predecessor.release?.version === MANAGED_COHORT_PREDECESSOR_RELEASE;
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

export function bootstrapReleaseDeploymentArguments(
  metadata,
  config = GENERATED_CONFIG_PATH,
) {
  return [
    ...releaseDeploymentArguments(metadata, config),
    "--containers-rollout", "none",
  ];
}

function privateReleaseDeploymentArguments(metadata, config) {
  return [
    "deploy",
    "--config", config,
    "--strict",
    "--tag", workerVersionTag(metadata),
    "--message", workerVersionMessage(metadata),
  ];
}

function privateBootstrapReleaseDeploymentArguments(metadata, config) {
  return [
    ...privateReleaseDeploymentArguments(metadata, config),
    "--containers-rollout", "none",
  ];
}

function legacyLeaseNotFound(error) {
  return error instanceof AgentEmailOperationsLeaseClientError &&
    error.status === 404 &&
    error.code === "agent_email_operations_lease_legacy_not_found";
}

function exactLeaseOrigin(preflight, expectedOrigin) {
  if (preflight?.operations_lease_origin !== expectedOrigin ||
      expectedOrigin !== CANONICAL_CONTROL_PLANE_ORIGIN) {
    throw new Error(
      "active email edge changed the control-plane operations lease origin",
    );
  }
  return preflight;
}

async function deployPrivateReleaseConfig(config) {
  const configSource = await config.readText();
  const expected = expectedBuildMetadata(configSource);
  const source = sourceIdentity();
  const actual = { service: "witself-control-plane", ...source };
  if (!deploymentMatches(actual, expected)) {
    throw new Error(
      "generated control-plane config does not match the clean tagged release source",
    );
  }

  const assertReleaseInputsUnchanged = async () => {
    const current = sourceIdentity();
    await config.assertUnchanged();
    if (current.version !== source.version || current.commit !== source.commit ||
        current.date !== source.date || current.tag !== source.tag) {
      throw new Error(
        "control-plane release source or exact generated config changed during deployment",
      );
    }
  };

  const inspectOrder = () => preflightManagedCohortProtocolUpgrade(
    expected.version,
    expected.managed_delivery_account_allowlist,
  );
  const deploy = async (signal) => {
    await assertReleaseInputsUnchanged();
    await runLeaseGuardedCommand(
      "wrangler",
      privateReleaseDeploymentArguments(source, config.path),
      { cwd: root, signal, timeoutMs: 5 * 60_000 },
    );
    await assertReleaseInputsUnchanged();
  };
  const deployBootstrapOuterWorker = async () => {
    await assertReleaseInputsUnchanged();
    await runLeaseGuardedCommand(
      "wrangler",
      privateBootstrapReleaseDeploymentArguments(source, config.path),
      { cwd: root, timeoutMs: 5 * 60_000 },
    );
    await assertReleaseInputsUnchanged();
  };
  const verify = (signal, endpoint) => runLeaseGuardedCommand(
    process.execPath,
    [
      join(root, "scripts", "verify-deployment.mjs"),
      "--config", config.path,
      "--endpoint", endpoint,
    ],
    { cwd: root, signal, timeoutMs: 12 * 60_000 },
  );
  const initialOrder = inspectOrder();
  const leaseOrigin = initialOrder.operations_lease_origin;
  exactLeaseOrigin(initialOrder, leaseOrigin);
  const guarded = async () => withAgentEmailOperationsLease(
    "control_plane_deploy",
    async ({ signal }) => {
      exactLeaseOrigin(inspectOrder(), leaseOrigin);
      await deploy(signal);
      await verify(signal, leaseOrigin);
      exactLeaseOrigin(inspectOrder(), leaseOrigin);
    },
    {
      endpoint: leaseOrigin,
      // Only this caller may distinguish an old release's missing endpoint.
      // The exception is accepted below solely for the exact v0.0.241 dark
      // bootstrap; every later deployment fails closed without the lease.
      allowLegacyNotFound: expected.version === MANAGED_COHORT_PROTOCOL_RELEASE,
    },
  );
  try {
    await guarded();
  } catch (error) {
    if (!legacyLeaseNotFound(error)) throw error;

    const predecessorIdentity = taggedReleaseIdentity(
      MANAGED_COHORT_PREDECESSOR_RELEASE,
    );
    const bootstrap = exactLeaseOrigin(inspectOrder(), leaseOrigin);
    const predecessor = preflightManagedCohortProtocolBootstrapPredecessor(
      expected,
      predecessorIdentity,
      config.path,
    );
    if (!isFirstManagedCohortProtocolBootstrap(
      expected.version,
      bootstrap,
      predecessor,
    )) {
      throw new Error(
        "control-plane deployment cannot bypass the shared operations lease outside the exact dark v0.0.241 bootstrap",
      );
    }

    // Re-attempt the normal path after the exact provider proof. If another
    // bootstrap installed the endpoint meanwhile, this process must join the
    // durable lease rather than entering the exception.
    try {
      await guarded();
      return;
    } catch (retryError) {
      if (!legacyLeaseNotFound(retryError)) throw retryError;
    }

    const finalBootstrap = exactLeaseOrigin(inspectOrder(), leaseOrigin);
    const finalPredecessor = preflightManagedCohortProtocolBootstrapPredecessor(
      expected,
      predecessorIdentity,
      config.path,
    );
    if (!isFirstManagedCohortProtocolBootstrap(
      expected.version,
      finalBootstrap,
      finalPredecessor,
    ) || JSON.stringify(finalPredecessor) !== JSON.stringify(predecessor)) {
      throw new Error(
        "control-plane deployment cannot bypass a changed v0.0.240 predecessor",
      );
    }
    // This is the sole unleased provider write. It installs only the exact
    // outer v0.0.241 Worker and explicitly suppresses every Container build or
    // rollout. Concurrent supported bootstraps have identical clean tagged
    // source, generated config, annotations, and arguments, so this step is
    // idempotent; the full deploy remains below the newly installed lease.
    await deployBootstrapOuterWorker();
    // The newly installed endpoint must become the durable serialization
    // authority before the Container rollout or any successful completion.
    await withAgentEmailOperationsLease(
      "control_plane_deploy",
      async ({ signal }) => {
        const first = preflightManagedCohortProtocolBootstrapTarget(
          expected,
          finalPredecessor,
          config.path,
        );
        await deploy(signal);
        await verify(signal, leaseOrigin);
        const converged = preflightManagedCohortProtocolBootstrapTarget(
          expected,
          finalPredecessor,
          config.path,
        );
        verifyManagedCohortProtocolBootstrapConvergence(first, converged);
        await assertReleaseInputsUnchanged();
        exactLeaseOrigin(inspectOrder(), leaseOrigin);
      },
      { endpoint: leaseOrigin },
    );
  }
}

export async function main(argv = process.argv.slice(2)) {
  if (argv.length !== 0) {
    throw new Error(`unknown or incomplete argument ${argv[0] ?? ""}`.trim());
  }
  const config = await createPrivateDeploymentConfig({
    prefix: "witself-control-plane-deploy-",
    render: (path) => runLeaseGuardedCommand(
      process.execPath,
      [join(root, "scripts", "render-wrangler.mjs"), "--output", path],
      { cwd: root, timeoutMs: 60_000 },
    ),
    validate: (path) => runLeaseGuardedCommand(
      process.execPath,
      [join(root, "scripts", "assert-custom-domain-dark.mjs"), "--config", path],
      { cwd: root, timeoutMs: 60_000 },
    ),
  });
  try {
    return await deployPrivateReleaseConfig(config);
  } finally {
    await config.cleanup();
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
