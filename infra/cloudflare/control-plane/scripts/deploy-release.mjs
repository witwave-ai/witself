#!/usr/bin/env node
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  deploymentMatches,
  expectedBuildMetadata,
  privateDeploymentConfigMain,
  verifyWorkerVersion,
  wranglerJSON,
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
  createControlPlaneReleaseSnapshot,
} from "./control-plane-release-snapshot.mjs";
import {
  assertCustomDomainSecretsDark,
  inspectWorkerSecrets,
} from "./assert-custom-domain-dark.mjs";
import { PRODUCTION_RECEIVE_WORKER } from
  "../../agent-email/src/worker-names.mjs";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerEnvironment,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
export const GENERATED_CONFIG_PATH = join(root, "wrangler.generated.jsonc");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = PRODUCTION_RECEIVE_WORKER;
const MANAGED_COHORT_PROTOCOL_RELEASE = "0.0.241";
const MANAGED_COHORT_PREDECESSOR_RELEASE = "0.0.240";
// v0.0.241 introduced the protocol, but its private Wrangler snapshot was
// outside the package and Wrangler rejected its relative paths before any
// control-plane provider mutation. Only this exact recovery release may use
// the still-dark v0.0.240 legacy-404 bootstrap. Future releases fail closed.
const LEASE_BOOTSTRAP_TARGET_RELEASE = "0.0.242";
const CANONICAL_CONTROL_PLANE_ORIGIN = "https://self.witwave.ai";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SHA256 = /^[0-9a-f]{64}$/;
// Post-activation deploys attest the reviewed live canonical-email state
// explicitly; anything but the exact literal keeps the strict full-dark gate.
const CANONICAL_EMAIL_ACTIVE =
  process.env.CP_DEPLOY_CANONICAL_EMAIL_ACTIVE === "true";

const CONTROL_PLANE_DARK_BINDINGS = Object.freeze([
  "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED",
]);

export function isLeaseBootstrapTargetRelease(release) {
  return release === LEASE_BOOTSTRAP_TARGET_RELEASE;
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
      "v0.0.241-or-newer managed-cohort protocol upgrade requires a v0.0.240 or newer email edge",
    );
  }
  if (targetAccounts.length !== 0) {
    throw new Error(
      "CP-first managed-cohort protocol upgrade over a v0.0.240 email edge requires an empty managed cohort",
    );
  }
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    if (plain(bindings, name) !== "false") {
      throw new Error(
        "CP-first managed-cohort protocol upgrade requires the active v0.0.240 email edge managed delivery gates to be false",
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
  if (!isLeaseBootstrapTargetRelease(target?.version) ||
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
      "v0.0.242 recovery lease bootstrap requires the exact active v0.0.240 control plane with canonical delivery dark and no managed cohort binding",
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
  if (!isLeaseBootstrapTargetRelease(target?.version) ||
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
  return isLeaseBootstrapTargetRelease(targetRelease) &&
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

function privateWranglerOutdir(workDirectory, name) {
  if (typeof workDirectory !== "string" || !workDirectory ||
      !isAbsolute(workDirectory) ||
      resolve(workDirectory) !== workDirectory) {
    throw new Error(
      "private Wrangler output directory requires a normalized absolute work directory",
    );
  }
  return join(workDirectory, name);
}

function privateDeploymentArguments(
  metadata,
  config,
  workDirectory,
  outdirName,
) {
  return [
    "deploy",
    "--config", config,
    "--outdir", privateWranglerOutdir(workDirectory, outdirName),
    "--strict",
    "--tag", workerVersionTag(metadata),
    "--message", workerVersionMessage(metadata),
  ];
}

export function privateReleaseDeploymentArguments(
  metadata,
  config,
  workDirectory,
) {
  return privateDeploymentArguments(
    metadata,
    config,
    workDirectory,
    "wrangler-control-plane-deploy",
  );
}

export function privateBootstrapReleaseDeploymentArguments(
  metadata,
  config,
  workDirectory,
) {
  return [
    ...privateDeploymentArguments(
      metadata,
      config,
      workDirectory,
      "wrangler-control-plane-bootstrap",
    ),
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

export function productionDeploymentEnvironments(environment = process.env) {
  assertProductionCloudflareIdentity(environment);
  const wranglerMutation = sanitizedWranglerEnvironment(environment);
  const wranglerInspection =
    sanitizedWranglerInspectionEnvironment(environment);
  return Object.freeze({
    wranglerMutation: Object.freeze(wranglerMutation),
    wranglerInspection: Object.freeze(wranglerInspection),
    // These Node entrypoints either render from explicit retained inputs or
    // sanitize their own Wrangler child. Removing Node injection and Wrangler
    // redirection variables here protects them before their module loads.
    nestedRender: Object.freeze({ ...wranglerMutation }),
    nestedInspection: Object.freeze({ ...wranglerInspection }),
  });
}

export function runProductionWranglerDeploy(
  args,
  {
    signal,
    environment = process.env,
    runCommand = runLeaseGuardedCommand,
    cwd = root,
    reviewedEnvironmentFile,
  } = {},
) {
  assertProductionCloudflareIdentity(environment);
  return runCommand(
    "wrangler",
    withReviewedWranglerEnvironmentFile(args, reviewedEnvironmentFile),
    {
      cwd,
      env: sanitizedWranglerEnvironment(environment),
      signal,
      timeoutMs: 5 * 60_000,
    },
  );
}

export async function withReleaseInputIntegrity(
  release,
  operation,
  label = "control-plane provider operation",
) {
  await release.assertUnchanged();
  let result;
  let operationError;
  try {
    result = await operation();
  } catch (error) {
    operationError = error;
  }
  let integrityError;
  try {
    await release.assertUnchanged();
  } catch (error) {
    integrityError = error;
  }
  if (operationError && integrityError) {
    throw new AggregateError(
      [operationError, integrityError],
      `${label} failed and its immutable release inputs changed`,
    );
  }
  if (operationError) throw operationError;
  if (integrityError) throw integrityError;
  return result;
}

async function deployPrivateReleaseConfig(release, commandEnvironments) {
  const configSource = await release.readText();
  const expected = expectedBuildMetadata(
    configSource,
    privateDeploymentConfigMain(release.path, release.controlPlaneRoot),
  );
  const source = release.source;
  const actual = { service: "witself-control-plane", ...source };
  if (!deploymentMatches(actual, expected)) {
    throw new Error(
      "generated control-plane config does not match the clean tagged release source",
    );
  }

  const inspect = (args, operation) => wranglerJSON(
    args,
    operation,
    commandEnvironments.wranglerInspection,
    undefined,
    {
      cwd: release.workDirectory,
      reviewedEnvironmentFile: release.reviewedEnvironmentFile,
    },
  );
  const inspectOrder = () => preflightManagedCohortProtocolUpgrade(
    expected.version,
    expected.managed_delivery_account_allowlist,
    inspect,
  );
  const inspectDarkSecrets = () => assertCustomDomainSecretsDark(
    inspectWorkerSecrets(
      release.path,
      commandEnvironments.wranglerInspection,
      undefined,
      {
        cwd: release.workDirectory,
        reviewedEnvironmentFile: release.reviewedEnvironmentFile,
      },
    ),
    { canonicalEmailActive: CANONICAL_EMAIL_ACTIVE },
  );
  const deploy = async (signal) => {
    await withReleaseInputIntegrity(
      release,
      () => runProductionWranglerDeploy(
        privateReleaseDeploymentArguments(
          source,
          release.path,
          release.workDirectory,
        ),
        {
          environment: commandEnvironments.wranglerMutation,
          signal,
          cwd: release.workDirectory,
          reviewedEnvironmentFile: release.reviewedEnvironmentFile,
        },
      ),
      "control-plane deployment mutation",
    );
  };
  const deployBootstrapOuterWorker = async () => {
    await withReleaseInputIntegrity(
      release,
      () => runProductionWranglerDeploy(
        privateBootstrapReleaseDeploymentArguments(
          source,
          release.path,
          release.workDirectory,
        ),
        {
          environment: commandEnvironments.wranglerMutation,
          cwd: release.workDirectory,
          reviewedEnvironmentFile: release.reviewedEnvironmentFile,
        },
      ),
      "control-plane bootstrap deployment mutation",
    );
  };
  const verify = (signal, endpoint) => runLeaseGuardedCommand(
    process.execPath,
    [
      join(release.controlPlaneRoot, "scripts", "verify-deployment.mjs"),
      "--config", release.path,
      "--endpoint", endpoint,
      "--wrangler-cwd", release.workDirectory,
      "--reviewed-env-file", release.reviewedEnvironmentFile,
    ],
    {
      cwd: release.workDirectory,
      env: commandEnvironments.nestedInspection,
      signal,
      timeoutMs: 12 * 60_000,
    },
  );
  const attest = (operation, label) => withReleaseInputIntegrity(
    release,
    operation,
    label,
  );
  const initialOrder = await attest(
    inspectOrder,
    "initial email edge deployment attestation",
  );
  const leaseOrigin = initialOrder.operations_lease_origin;
  exactLeaseOrigin(initialOrder, leaseOrigin);
  const guarded = async () => attest(
    () => withAgentEmailOperationsLease(
      "control_plane_deploy",
      async ({ signal }) => {
        await attest(
          () => exactLeaseOrigin(inspectOrder(), leaseOrigin),
          "leased email edge deployment attestation",
        );
        await attest(
          inspectDarkSecrets,
          "leased dark-secret deployment attestation",
        );
        await deploy(signal);
        await attest(
          () => verify(signal, leaseOrigin),
          "leased control-plane deployment attestation",
        );
        await attest(
          inspectDarkSecrets,
          "leased final dark-secret deployment attestation",
        );
        await attest(
          () => exactLeaseOrigin(inspectOrder(), leaseOrigin),
          "leased final email edge deployment attestation",
        );
      },
      {
        endpoint: leaseOrigin,
        // Only this caller may distinguish an old release's missing endpoint.
        // The v0.0.241 attempt could not reach a provider mutation. Only the
        // exact v0.0.242 recovery release may enter the dark v0.0.240 proof.
        allowLegacyNotFound: isLeaseBootstrapTargetRelease(expected.version),
      },
    ),
    "control-plane operations lease",
  );
  try {
    await guarded();
  } catch (error) {
    if (!legacyLeaseNotFound(error)) throw error;

    const predecessorIdentity = taggedReleaseIdentity(
      MANAGED_COHORT_PREDECESSOR_RELEASE,
    );
    const bootstrap = await attest(
      () => exactLeaseOrigin(inspectOrder(), leaseOrigin),
      "bootstrap email edge deployment attestation",
    );
    const predecessor = await attest(
      () => preflightManagedCohortProtocolBootstrapPredecessor(
        expected,
        predecessorIdentity,
        release.path,
        inspect,
      ),
      "bootstrap predecessor deployment attestation",
    );
    if (!isFirstManagedCohortProtocolBootstrap(
      expected.version,
      bootstrap,
      predecessor,
    )) {
      throw new Error(
        "control-plane deployment cannot bypass the shared operations lease outside the exact dark v0.0.242 recovery bootstrap",
      );
    }
    await attest(
      inspectDarkSecrets,
      "bootstrap dark-secret deployment attestation",
    );

    // Re-attempt the normal path after the exact provider proof. If another
    // bootstrap installed the endpoint meanwhile, this process must join the
    // durable lease rather than entering the exception.
    try {
      await guarded();
      return;
    } catch (retryError) {
      if (!legacyLeaseNotFound(retryError)) throw retryError;
    }

    const finalBootstrap = await attest(
      () => exactLeaseOrigin(inspectOrder(), leaseOrigin),
      "final bootstrap email edge deployment attestation",
    );
    const finalPredecessor = await attest(
      () => preflightManagedCohortProtocolBootstrapPredecessor(
        expected,
        predecessorIdentity,
        release.path,
        inspect,
      ),
      "final bootstrap predecessor deployment attestation",
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
    await attest(
      inspectDarkSecrets,
      "final bootstrap dark-secret deployment attestation",
    );
    // This is the sole unleased provider write. It installs only the exact
    // outer v0.0.242 Worker and explicitly suppresses every Container build or
    // rollout. Concurrent supported bootstraps have identical clean tagged
    // source, generated config, annotations, and arguments, so this step is
    // idempotent; the full deploy remains below the newly installed lease.
    await deployBootstrapOuterWorker();
    // The newly installed endpoint must become the durable serialization
    // authority before the Container rollout or any successful completion.
    await attest(
      () => withAgentEmailOperationsLease(
        "control_plane_deploy",
        async ({ signal }) => {
          const first = await attest(
            () => preflightManagedCohortProtocolBootstrapTarget(
              expected,
              finalPredecessor,
              release.path,
              inspect,
            ),
            "leased bootstrap target deployment attestation",
          );
          await attest(
            inspectDarkSecrets,
            "leased bootstrap dark-secret deployment attestation",
          );
          await deploy(signal);
          await attest(
            () => verify(signal, leaseOrigin),
            "leased bootstrap deployment attestation",
          );
          await attest(
            inspectDarkSecrets,
            "leased final bootstrap dark-secret deployment attestation",
          );
          const converged = await attest(
            () => preflightManagedCohortProtocolBootstrapTarget(
              expected,
              finalPredecessor,
              release.path,
              inspect,
            ),
            "leased converged bootstrap target attestation",
          );
          verifyManagedCohortProtocolBootstrapConvergence(first, converged);
          await attest(
            () => exactLeaseOrigin(inspectOrder(), leaseOrigin),
            "leased bootstrap email edge deployment attestation",
          );
        },
        { endpoint: leaseOrigin },
      ),
      "bootstrap control-plane operations lease",
    );
  }
}

export async function withPrivateDeploymentConfigCleanup(config, operation) {
  let result;
  let operationError;
  try {
    result = await operation();
  } catch (error) {
    operationError = error;
  }
  let cleanupError;
  try {
    await config.cleanup();
  } catch (error) {
    cleanupError = error;
  }
  if (operationError && cleanupError) {
    const cleanupErrors = cleanupError instanceof AggregateError
      ? cleanupError.errors
      : [cleanupError];
    throw new AggregateError(
      [operationError, ...cleanupErrors],
      "control-plane deployment failed and release input cleanup was incomplete",
    );
  }
  if (operationError) throw operationError;
  if (cleanupError) throw cleanupError;
  return result;
}

export async function main(argv = process.argv.slice(2)) {
  if (argv.length !== 0) {
    throw new Error(`unknown or incomplete argument ${argv[0] ?? ""}`.trim());
  }
  const commandEnvironments = productionDeploymentEnvironments();
  const source = sourceIdentity();
  const release = await createControlPlaneReleaseSnapshot({
    identity: source,
    render: (path, layout) => runLeaseGuardedCommand(
      process.execPath,
      [
        join(layout.controlPlaneRoot, "scripts", "render-wrangler.mjs"),
        "--version", source.version,
        "--commit", source.commit,
        "--date", source.date,
        "--output", path,
      ],
      {
        cwd: layout.workDirectory,
        env: commandEnvironments.nestedRender,
        timeoutMs: 60_000,
      },
    ),
    validate: (path, layout) => assertCustomDomainSecretsDark(
      inspectWorkerSecrets(
        path,
        commandEnvironments.nestedInspection,
        undefined,
        {
          cwd: layout.workDirectory,
          reviewedEnvironmentFile: layout.reviewedEnvironmentFile,
        },
      ),
      { canonicalEmailActive: CANONICAL_EMAIL_ACTIVE },
    ),
  });
  return withPrivateDeploymentConfigCleanup(
    release,
    () => deployPrivateReleaseConfig(release, commandEnvironments),
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
