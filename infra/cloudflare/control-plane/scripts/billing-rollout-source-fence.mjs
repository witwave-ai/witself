#!/usr/bin/env node
import { createHash } from "node:crypto";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { wranglerJSON } from "./verify-deployment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const GENERATED_CONFIG_PATH = join(root, "wrangler.generated.jsonc");
const WORKER_NAME = "witself-control-plane";
const CONTAINER_APPLICATION_NAME = "witself-control-plane";
const SOURCE_FENCE_SCHEMA = "witself.billing-rollout-source-fence.v1";
const INSTANCE_PAGE_SIZE = 100;
const MAX_INSTANCE_PAGES = 128;

// A lifecycle tick can continue for this long after its secret gate disappears.
// The private operator workflow must retain an earlier, independently captured
// absence attestation and compare the later pre/post observations. Supplying a
// timestamp alone is never sufficient rollout evidence.
export const LIFECYCLE_IN_FLIGHT_BOUND_MS = 4 * 60 * 1000;

const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const HEX32 = /^[0-9a-f]{32}$/;
const SHA256 = /^[0-9a-f]{64}$/;
const EXACT_UTC_SECOND =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
const SAFE_NAME = /^[A-Z][A-Z0-9_]{0,127}$/;
const INSTANCE_STATES = new Set([
  "failed",
  "inactive",
  "provisioning",
  "running",
  "stopped",
  "stopping",
  "unhealthy",
]);

function isRecord(value) {
  return value != null && typeof value === "object" && !Array.isArray(value);
}

function exactKeys(value, allowed, label) {
  if (!isRecord(value) ||
      JSON.stringify(Object.keys(value).sort()) !==
        JSON.stringify([...allowed].sort())) {
    throw new Error(`${label} had an invalid strict shape`);
  }
}

function positiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) {
    throw new Error(`${label} was not a positive integer`);
  }
  return value;
}

function nonnegativeInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} was not a nonnegative integer`);
  }
  return value;
}

function canonicalJSON(value) {
  if (value === null || typeof value === "boolean" ||
      typeof value === "string") {
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error("attestation contained a non-finite number");
    }
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map(canonicalJSON).join(",")}]`;
  }
  if (isRecord(value)) {
    return `{${Object.keys(value).sort().map((key) =>
      `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(",")}}`;
  }
  throw new Error("attestation contained a non-JSON value");
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function jsonFence(value) {
  return sha256(canonicalJSON(value));
}

function exactUTCSecond(value, label) {
  if (typeof value !== "string" || !EXACT_UTC_SECOND.test(value)) {
    throw new Error(`${label} must be an exact UTC second`);
  }
  const milliseconds = Date.parse(value);
  if (!Number.isFinite(milliseconds) ||
      new Date(milliseconds).toISOString() !== value.replace("Z", ".000Z")) {
    throw new Error(`${label} must be a real UTC second`);
  }
  return milliseconds;
}

function observedAt(clock) {
  const value = clock();
  const date = value instanceof Date ? value : new Date(value);
  if (!Number.isFinite(date.getTime())) {
    throw new Error("observation clock was invalid");
  }
  return new Date(Math.floor(date.getTime() / 1000) * 1000)
    .toISOString().replace(".000Z", "Z");
}

function activeDeploymentFence(deployment) {
  if (!isRecord(deployment) || !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error(
      "control-plane deployment was not one valid Worker version at 100 percent",
    );
  }
  return Object.freeze({
    deployment_id: deployment.id,
    version_id: deployment.versions[0].version_id,
  });
}

function bindingInventory(version, expectedVersionID) {
  if (!isRecord(version) || version.id !== expectedVersionID ||
      !UUID.test(String(version.id ?? "")) ||
      version.metadata?.source !== "wrangler") {
    throw new Error("active control-plane Worker version identity was invalid");
  }
  const script = version.resources?.script;
  if (!isRecord(script) || !SHA256.test(String(script.etag ?? "")) ||
      JSON.stringify(script.handlers) !== JSON.stringify(["fetch", "scheduled"])) {
    throw new Error("active control-plane Worker handlers were invalid");
  }
  const namedHandlers = script.named_handlers;
  if (!Array.isArray(namedHandlers) ||
      namedHandlers.filter((handler) =>
        isRecord(handler) && handler.name === "Backend" &&
        JSON.stringify(handler.handlers) === JSON.stringify(["class"])
      ).length !== 1) {
    throw new Error("active control-plane Backend handler was ambiguous");
  }
  const runtime = version.resources?.script_runtime;
  if (!isRecord(runtime) ||
      JSON.stringify(runtime.containers) !==
        JSON.stringify([{ class_name: "Backend" }])) {
    throw new Error("active control-plane container runtime was ambiguous");
  }
  if (!Array.isArray(version.resources?.bindings)) {
    throw new Error("active control-plane binding inventory was missing");
  }

  const bindings = new Map();
  const canonicalBindings = [];
  for (const binding of version.resources.bindings) {
    if (!isRecord(binding) || !SAFE_NAME.test(String(binding.name ?? "")) ||
        typeof binding.type !== "string" || binding.type === "" ||
        bindings.has(binding.name)) {
      throw new Error("active control-plane binding inventory was invalid");
    }
    if (binding.type === "secret_text") {
      exactKeys(binding, ["name", "type"],
        `active secret binding ${binding.name}`);
    }
    // Non-secret binding values are private inspection inputs. Only this
    // canonical hash leaves the process; secret bindings are name/type only.
    canonicalBindings.push(binding);
    bindings.set(binding.name, binding);
  }
  canonicalBindings.sort((left, right) => left.name.localeCompare(right.name));

  const backend = bindings.get("CONTROL_PLANE");
  if (backend?.type !== "durable_object_namespace" ||
      backend.class_name !== "Backend" ||
      !HEX32.test(String(backend.namespace_id ?? ""))) {
    throw new Error("active control-plane Backend namespace was invalid");
  }
  for (const name of [
    "CP_BILLING_ACCOUNT_ALLOWLIST",
    "CP_PLAN_LIFECYCLE_ENABLED",
  ]) {
    const binding = bindings.get(name);
    if (binding != null && binding.type !== "secret_text") {
      throw new Error(`${name} was not an operator-managed secret binding`);
    }
  }

  return Object.freeze({
    version_id: version.id,
    script_etag: script.etag,
    backend_namespace_id: backend.namespace_id,
    binding_inventory_sha256: jsonFence(canonicalBindings),
    secret_names: Object.freeze([...bindings.values()]
      .filter((binding) => binding.type === "secret_text")
      .map((binding) => binding.name)
      .sort()),
  });
}

function secretNameInventory(secrets) {
  if (!Array.isArray(secrets)) {
    throw new Error("persistent Worker secret inventory was not an array");
  }
  const names = new Set();
  for (const secret of secrets) {
    exactKeys(secret, ["name", "type"], "persistent Worker secret entry");
    if (!SAFE_NAME.test(String(secret.name ?? "")) ||
        secret.type !== "secret_text" || names.has(secret.name)) {
      throw new Error("persistent Worker secret inventory was invalid");
    }
    names.add(secret.name);
  }
  return Object.freeze([...names].sort());
}

function assertSameSecretInventories(versionNames, persistentNames) {
  if (JSON.stringify(versionNames) !== JSON.stringify(persistentNames)) {
    throw new Error(
      "active-version and persistent Worker secret inventories disagreed",
    );
  }
  if (persistentNames.includes("CP_BILLING_ACCOUNT_ALLOWLIST")) {
    throw new Error(
      "billing cohort secret is present, so a zero-account cohort cannot be proven",
    );
  }
}

function containerApplicationFence(application, expectedID, namespaceID) {
  if (!isRecord(application) || application.id !== expectedID ||
      !UUID.test(String(application.id ?? "")) ||
      application.name !== CONTAINER_APPLICATION_NAME) {
    throw new Error("source container application identity was invalid");
  }
  const version = positiveInteger(
    application.version,
    "source container application version",
  );
  if (!isRecord(application.durable_objects) ||
      application.durable_objects.namespace_id !== namespaceID) {
    throw new Error(
      "source container application did not match the active Backend namespace",
    );
  }
  const maxInstances = nonnegativeInteger(
    application.max_instances,
    "source container application max_instances",
  );
  const image = application.configuration?.image;
  if (typeof image !== "string" || image === "" || image.length > 2048 ||
      /[\x00-\x1f\x7f]/.test(image)) {
    throw new Error("source container application image identity was invalid");
  }

  const privateIdentity = Object.freeze({
    id: application.id,
    name: application.name,
    version,
    backend_namespace_id: namespaceID,
    max_instances: maxInstances,
    image,
  });
  return Object.freeze({
    application_id: application.id,
    version,
    max_instances: maxInstances,
    application_sha256: jsonFence(privateIdentity),
  });
}

function instanceInventoryPage(page, requestedPageToken) {
  if (!isRecord(page)) {
    throw new Error("container instance page was invalid");
  }
  exactKeys(page, ["instances", "result_info"],
    "container instance page");
  if (!Array.isArray(page.instances) || !isRecord(page.result_info)) {
    throw new Error("container instance page was invalid");
  }
  if (page.instances.length > INSTANCE_PAGE_SIZE) {
    throw new Error("container instance page exceeded its requested bound");
  }
  exactKeys(
    page.result_info,
    ["next_page_token", "page_token", "per_page"],
    "container instance page result_info",
  );
  if (page.result_info.per_page !== INSTANCE_PAGE_SIZE ||
      page.result_info.page_token !== requestedPageToken) {
    throw new Error("container instance pagination fence was invalid");
  }
  const next = page.result_info.next_page_token;
  if (next !== null &&
      (typeof next !== "string" || next === "" || next.length > 2048 ||
       /[\x00-\x1f\x7f]/.test(next))) {
    throw new Error("container instance next-page fence was invalid");
  }
  if (next !== null && page.instances.length === 0) {
    throw new Error("empty container instance page had a continuation fence");
  }
  return next;
}

function canonicalInstance(row) {
  if (!isRecord(row)) {
    throw new Error("container instance inventory contained an invalid row");
  }
  const allowed = new Set([
    "created",
    "id",
    "location",
    "name",
    "state",
    "version",
  ]);
  if (!Object.keys(row).every((key) => allowed.has(key)) ||
      ![5, 6].includes(Object.keys(row).length) ||
      !UUID.test(String(row.id ?? "")) ||
      !INSTANCE_STATES.has(row.state)) {
    throw new Error("container instance inventory contained an ambiguous row");
  }
  if (Object.hasOwn(row, "name") && row.name !== null &&
      (typeof row.name !== "string" || row.name === "" ||
       row.name.length > 256 || /[\x00-\x1f\x7f]/.test(row.name))) {
    throw new Error("container instance inventory contained an invalid name");
  }
  if (row.state === "inactive") {
    if (row.version !== null || row.location !== null) {
      throw new Error("inactive container instance inventory was ambiguous");
    }
  } else {
    positiveInteger(row.version, "container instance application version");
    if (typeof row.location !== "string" || row.location === "" ||
        row.location.length > 128 || /[\x00-\x1f\x7f]/.test(row.location)) {
      throw new Error("container instance location was invalid");
    }
  }
  if (row.created !== null &&
      (typeof row.created !== "string" || row.created.length > 64 ||
       !Number.isFinite(Date.parse(row.created)))) {
    throw new Error("container instance creation fence was invalid");
  }
  return Object.freeze({
    id: row.id,
    ...(Object.hasOwn(row, "name") ? { name: row.name } : {}),
    state: row.state,
    location: row.location,
    version: row.version,
    created: row.created,
  });
}

function inspectInstanceInventory(
  inspect,
  config,
  applicationID,
) {
  const rows = [];
  const ids = new Set();
  const pageTokens = new Set();
  let pageToken = null;
  for (let pageNumber = 0; pageNumber < MAX_INSTANCE_PAGES; pageNumber += 1) {
    const args = [
      "containers", "instances", applicationID,
      "--config", config,
      "--per-page", String(INSTANCE_PAGE_SIZE),
      "--json",
    ];
    if (pageToken !== null) args.push("--page-token", pageToken);
    const page = inspect(args, "inspect control-plane container instances");
    const next = instanceInventoryPage(page, pageToken);
    for (const raw of page.instances) {
      const row = canonicalInstance(raw);
      if (ids.has(row.id)) {
        throw new Error("container instance inventory contained a duplicate id");
      }
      ids.add(row.id);
      rows.push(row);
    }
    if (next === null) {
      rows.sort((left, right) => left.id.localeCompare(right.id));
      return Object.freeze(rows);
    }
    if (pageTokens.has(next)) {
      throw new Error("container instance pagination fence repeated");
    }
    pageTokens.add(next);
    pageToken = next;
  }
  throw new Error("container instance inventory exceeded its page bound");
}

function sameJSON(left, right) {
  return canonicalJSON(left) === canonicalJSON(right);
}

function inspectOneState(options, inspect) {
  const deployment = activeDeploymentFence(inspect([
    "deployments", "status",
    "--config", options.config,
    "--name", WORKER_NAME,
    "--json",
  ], "inspect active control-plane deployment"));
  const worker = bindingInventory(inspect([
    "versions", "view", deployment.version_id,
    "--config", options.config,
    "--name", WORKER_NAME,
    "--json",
  ], "inspect active control-plane Worker version"), deployment.version_id);
  const secrets = secretNameInventory(inspect([
    "secret", "list",
    "--config", options.config,
    "--name", WORKER_NAME,
    "--format", "json",
  ], "inspect persistent control-plane secret names"));
  assertSameSecretInventories(worker.secret_names, secrets);
  const application = containerApplicationFence(inspect([
    "containers", "info", options.expectedSourceApplicationID,
    "--config", options.config,
    "--json",
  ], "inspect control-plane container application"),
  options.expectedSourceApplicationID, worker.backend_namespace_id);
  const instances = inspectInstanceInventory(
    inspect,
    options.config,
    options.expectedSourceApplicationID,
  );
  return Object.freeze({ deployment, worker, secrets, application, instances });
}

function publicStateFence(state) {
  return Object.freeze({
    worker_deployment_id: state.deployment.deployment_id,
    worker_version_id: state.worker.version_id,
    worker_script_etag: state.worker.script_etag,
    backend_namespace_id: state.worker.backend_namespace_id,
    binding_inventory_sha256: state.worker.binding_inventory_sha256,
    secret_name_inventory_sha256: jsonFence(state.secrets),
    container_application_id: state.application.application_id,
    container_application_version: state.application.version,
    container_application_sha256: state.application.application_sha256,
    source_instance_inventory_sha256: jsonFence(state.instances),
    container_instance_count: state.instances.length,
  });
}

function samePrivateState(left, right) {
  return sameJSON(publicStateFence(left), publicStateFence(right));
}

/**
 * Produce one private, count-and-fence-only Cloudflare observation.
 *
 * The caller must retain an earlier absence observation, wait through the
 * lifecycle in-flight bound, take this observation around the complete R2
 * scan, and compare pre/post fence fields. This function deliberately cannot
 * turn one operator-supplied timestamp into proof of continuous quiescence.
 */
export function observeBillingRolloutSourceFence(
  options,
  inspect = wranglerJSON,
  clock = () => new Date(),
) {
  if (!isRecord(options) || typeof options.config !== "string" ||
      options.config === "" ||
      !UUID.test(String(options.expectedSourceApplicationID ?? ""))) {
    throw new Error("source fence options were invalid");
  }
  const expectedSourceVersion = positiveInteger(
    options.expectedSourceApplicationVersion,
    "expected source application version",
  );
  const disabledObservedAt = options.lifecycleDisabledObservedAt ?? null;
  let disabledMilliseconds = null;
  if (disabledObservedAt !== null) {
    disabledMilliseconds = exactUTCSecond(
      disabledObservedAt,
      "lifecycle disabled observation",
    );
  }

  const first = inspectOneState(options, inspect);
  const second = inspectOneState(options, inspect);
  if (!samePrivateState(first, second)) {
    throw new Error(
      "control-plane source state changed during exact provider inspection",
    );
  }

  const observed = observedAt(clock);
  const observedMilliseconds = exactUTCSecond(observed, "observed_at");
  if (disabledMilliseconds !== null &&
      disabledMilliseconds > observedMilliseconds) {
    throw new Error("lifecycle disabled observation is after this observation");
  }

  const sourceApplicationSpawnable =
    second.application.version === expectedSourceVersion;
  const sourceInstances = second.instances.filter((instance) =>
    instance.version === expectedSourceVersion);
  const apiReplicas = Math.max(
    sourceInstances.length,
    sourceApplicationSpawnable ? 1 : 0,
  );
  const lifecycleGatePresent = second.secrets.includes(
    "CP_PLAN_LIFECYCLE_ENABLED",
  );
  const outsideInFlightBound = disabledMilliseconds !== null &&
    observedMilliseconds - disabledMilliseconds >=
      LIFECYCLE_IN_FLIGHT_BOUND_MS;
  const reconcilerReplicas = !lifecycleGatePresent && apiReplicas === 0 &&
      outsideInFlightBound
    ? 0
    : Math.max(1, apiReplicas);

  const fence = Object.freeze({
    ...publicStateFence(second),
    expected_source_application_version: expectedSourceVersion,
    lifecycle_gate_present: lifecycleGatePresent,
    lifecycle_disabled_observed_at: disabledObservedAt,
    source_application_spawnable: sourceApplicationSpawnable,
  });
  const unsigned = Object.freeze({
    schema: SOURCE_FENCE_SCHEMA,
    observed_at: observed,
    billing_mutation_cohort_accounts: 0,
    source_fleet: Object.freeze({
      api_replicas: apiReplicas,
      reconciler_replicas: reconcilerReplicas,
    }),
    fence,
  });
  return Object.freeze({
    ...unsigned,
    inspection_sha256: jsonFence(unsigned),
  });
}

export function parseBillingRolloutSourceFenceArgs(argv) {
  const options = {
    config: GENERATED_CONFIG_PATH,
    wranglerCwd: root,
    reviewedEnvironmentFile: undefined,
    expectedSourceApplicationID: "",
    expectedSourceApplicationVersion: 0,
    lifecycleDisabledObservedAt: null,
  };
  const names = new Set([
    "--config",
    "--expected-source-application-id",
    "--expected-source-application-version",
    "--lifecycle-disabled-observed-at",
    "--reviewed-env-file",
    "--wrangler-cwd",
  ]);
  const seen = new Set();
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (!names.has(name)) throw new Error(`unknown argument ${name}`);
    if (seen.has(name)) throw new Error(`duplicate argument ${name}`);
    seen.add(name);
    const value = argv[++index];
    if (typeof value !== "string" || value === "") {
      throw new Error(`${name} requires a value`);
    }
    switch (name) {
    case "--config":
      options.config = resolve(root, value);
      break;
    case "--expected-source-application-id":
      options.expectedSourceApplicationID = value;
      break;
    case "--expected-source-application-version":
      if (!/^[1-9][0-9]*$/.test(value)) {
        throw new Error(
          "--expected-source-application-version must be a canonical positive integer",
        );
      }
      options.expectedSourceApplicationVersion = Number(value);
      break;
    case "--lifecycle-disabled-observed-at":
      options.lifecycleDisabledObservedAt = value;
      break;
    case "--reviewed-env-file":
      options.reviewedEnvironmentFile = resolve(value);
      if (options.reviewedEnvironmentFile !== value) {
        throw new Error("--reviewed-env-file must be a normalized absolute path");
      }
      break;
    case "--wrangler-cwd":
      options.wranglerCwd = resolve(value);
      if (options.wranglerCwd !== value) {
        throw new Error("--wrangler-cwd must be a normalized absolute path");
      }
      break;
    }
  }
  if (!UUID.test(options.expectedSourceApplicationID)) {
    throw new Error("--expected-source-application-id must be a valid UUID");
  }
  positiveInteger(
    options.expectedSourceApplicationVersion,
    "--expected-source-application-version",
  );
  if (options.lifecycleDisabledObservedAt !== null) {
    exactUTCSecond(
      options.lifecycleDisabledObservedAt,
      "--lifecycle-disabled-observed-at",
    );
  }
  return Object.freeze(options);
}

export function main(argv = process.argv.slice(2)) {
  const options = parseBillingRolloutSourceFenceArgs(argv);
  const inspect = (args, operation) => wranglerJSON(
    args,
    operation,
    process.env,
    undefined,
    {
      cwd: options.wranglerCwd,
      reviewedEnvironmentFile: options.reviewedEnvironmentFile,
    },
  );
  const attestation = observeBillingRolloutSourceFence(options, inspect);
  process.stdout.write(`${canonicalJSON(attestation)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (error) {
    process.stderr.write(
      `billing rollout source fence: FAIL: ${String(error?.message ?? error)}\n`,
    );
    process.exitCode = 1;
  }
}
