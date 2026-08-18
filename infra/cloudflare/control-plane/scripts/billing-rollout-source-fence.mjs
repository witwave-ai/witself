#!/usr/bin/env node
import { createHash } from "node:crypto";
import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
  realpathSync,
} from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  expectedBuildMetadata,
  privateDeploymentConfigMain,
} from "./verify-deployment.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const WORKER_NAME = "witself-control-plane";
const CONTAINER_APPLICATION_NAME = "witself-control-plane";
const SOURCE_FENCE_SCHEMA = "witself.billing-rollout-source-fence.v1";
// This provider trust boundary is intentionally source-owned. None of the
// authority, operation paths, timeout, or response bounds are configurable by
// the operator environment or CLI.
const CLOUDFLARE_API_ORIGIN = "https://api.cloudflare.com";
const CLOUDFLARE_API_PREFIX = "/client/v4";
const CLOUDFLARE_REQUEST_TIMEOUT_MS = 30 * 1000;
const MAX_CLOUDFLARE_RESPONSE_BYTES = 4 * 1024 * 1024;
const INSTANCE_PAGE_SIZE = 100;
const MAX_INSTANCE_PAGES = 128;
const MAX_CONFIG_BYTES = 1024 * 1024;
const MAX_PRIOR_ATTESTATION_BYTES = 64 * 1024;
const EXPECTED_TARGET_MAX_INSTANCES = 2;

// A lifecycle tick can continue for this long after its secret gate disappears.
// The private operator workflow must retain an earlier, independently captured
// absence attestation and compare the later pre/post observations. Supplying a
// timestamp alone is never sufficient rollout evidence.
export const LIFECYCLE_IN_FLIGHT_BOUND_MS = 4 * 60 * 1000;

// Wrangler 4.120 validates Container application and instance identifiers as
// grouped hexadecimal UUIDs without requiring one RFC version/variant nibble.
// Match that provider contract while retaining canonical lowercase output.
const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const HEX32 = /^[0-9a-f]{32}$/;
const SHA256 = /^[0-9a-f]{64}$/;
const IMAGE_DIGEST = /^sha256:[0-9a-f]{64}$/;
const RELEASE_VERSION = /^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/;
const RELEASE_COMMIT = /^[0-9a-f]{40}$/;
const EXACT_UTC_SECOND =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
const EXACT_RFC3339_SECOND =
  /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(Z|([+-])(\d{2}):(\d{2}))$/;
const SAFE_NAME = /^[A-Z][A-Z0-9_]{0,127}$/;
const INSTANCE_STATES = new Set([
  "failed",
  "inactive",
  "provisioning",
  "running",
  "stopped",
  "stopping",
  "unhealthy",
  "unknown",
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

function exactReleaseIdentity(value, label) {
  const keys = ["commit", "date", "version"];
  exactKeys(value, keys, label);
  if (!RELEASE_VERSION.test(String(value.version ?? "")) ||
      !RELEASE_COMMIT.test(String(value.commit ?? ""))) {
    throw new Error(`${label} was invalid`);
  }
  const date = canonicalReleaseUTCSecond(value.date, `${label} date`);
  return Object.freeze({
    version: value.version,
    commit: value.commit,
    date,
  });
}

function exactReviewedConfigIdentity(value) {
  exactKeys(value, [
    "account_id",
    "config_sha256",
    "target_release",
  ], "reviewed config identity");
  if (value.account_id !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID ||
      !SHA256.test(String(value.config_sha256 ?? ""))) {
    throw new Error("reviewed config identity was invalid");
  }
  return Object.freeze({
    account_id: value.account_id,
    config_sha256: value.config_sha256,
    target_release: exactReleaseIdentity(
      value.target_release,
      "reviewed target release identity",
    ),
  });
}

function exactTargetIdentity(options) {
  if (!isRecord(options) ||
      options.expectedAccountID !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID ||
      !UUID.test(String(options.expectedTargetApplicationID ?? "")) ||
      !IMAGE_DIGEST.test(String(options.expectedTargetImageDigest ?? ""))) {
    throw new Error("source fence options were invalid");
  }
  const version = positiveInteger(
    options.expectedTargetApplicationVersion,
    "expected target application version",
  );
  const reviewed = exactReviewedConfigIdentity(options.reviewedConfigIdentity);
  if (reviewed.account_id !== options.expectedAccountID ||
      reviewed.target_release.version !==
        options.expectedTargetReleaseVersion ||
      reviewed.target_release.commit !==
        options.expectedTargetReleaseCommit) {
    throw new Error(
      "target account/release and reviewed config identity disagreed",
    );
  }
  return Object.freeze({
    account_id: options.expectedAccountID,
    application_id: options.expectedTargetApplicationID,
    application_version: version,
    image_digest: options.expectedTargetImageDigest,
    max_instances: EXPECTED_TARGET_MAX_INSTANCES,
    reviewed_config_sha256: reviewed.config_sha256,
    release: reviewed.target_release,
  });
}

function imageDigest(image) {
  if (typeof image !== "string" || image === "" || image.length > 2048 ||
      /[\x00-\x1f\x7f]/.test(image)) {
    throw new Error("source container application image identity was invalid");
  }
  const marker = image.lastIndexOf("@sha256:");
  if (marker < 1) return null;
  const digest = image.slice(marker + 1);
  return IMAGE_DIGEST.test(digest) ? digest : null;
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

function canonicalReleaseUTCSecond(value, label) {
  if (typeof value !== "string") {
    throw new Error(`${label} must be an exact RFC 3339 second`);
  }
  const match = value.match(EXACT_RFC3339_SECOND);
  if (match === null) {
    throw new Error(`${label} must be an exact RFC 3339 second`);
  }
  const milliseconds = Date.parse(value);
  if (!Number.isFinite(milliseconds)) {
    throw new Error(`${label} must be a real RFC 3339 second`);
  }
  let offsetMinutes = 0;
  if (match[2] !== "Z") {
    const hours = Number(match[4]);
    const minutes = Number(match[5]);
    if (hours > 23 || minutes > 59) {
      throw new Error(`${label} must have a real RFC 3339 offset`);
    }
    offsetMinutes = (match[3] === "+" ? 1 : -1) *
      (hours * 60 + minutes);
  }
  const localRoundTrip = new Date(
    milliseconds + offsetMinutes * 60 * 1000,
  ).toISOString().slice(0, 19);
  if (localRoundTrip !== match[1]) {
    throw new Error(`${label} must be a real RFC 3339 second`);
  }
  return new Date(milliseconds).toISOString().replace(".000Z", "Z");
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

function fixedCloudflareURL(pathname, query = undefined) {
  if (typeof pathname !== "string" ||
      !pathname.startsWith("/accounts/") ||
      /[?#\x00-\x1f\x7f]/.test(pathname)) {
    throw new Error("Cloudflare inspection path was invalid");
  }
  const url = new URL(`${CLOUDFLARE_API_ORIGIN}${CLOUDFLARE_API_PREFIX}${pathname}`);
  if (query !== undefined) url.search = query.toString();
  if (url.origin !== CLOUDFLARE_API_ORIGIN ||
      !url.pathname.startsWith(`${CLOUDFLARE_API_PREFIX}/accounts/`) ||
      url.username !== "" || url.password !== "" || url.hash !== "") {
    throw new Error("Cloudflare inspection authority was invalid");
  }
  return url;
}

async function boundedJSONResponse(response, expectedURL, operation) {
  if (!isRecord(response) || response.status !== 200 || response.ok !== true ||
      response.redirected !== false || response.url !== expectedURL.href ||
      !isRecord(response.headers) ||
      typeof response.headers.get !== "function") {
    throw new Error(`${operation} returned an invalid HTTP response`);
  }
  const contentType = response.headers.get("content-type");
  if (typeof contentType !== "string" ||
      !/^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(contentType)) {
    throw new Error(`${operation} did not return exact JSON content`);
  }
  const contentLength = response.headers.get("content-length");
  if (contentLength !== null &&
      (!/^(?:0|[1-9][0-9]*)$/.test(contentLength) ||
       Number(contentLength) > MAX_CLOUDFLARE_RESPONSE_BYTES)) {
    throw new Error(`${operation} response exceeded its byte bound`);
  }
  if (!isRecord(response.body) ||
      typeof response.body.getReader !== "function") {
    throw new Error(`${operation} response body was unavailable`);
  }

  const reader = response.body.getReader();
  const chunks = [];
  let length = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) {
        throw new Error(`${operation} response body was invalid`);
      }
      length += value.byteLength;
      if (length > MAX_CLOUDFLARE_RESPONSE_BYTES) {
        throw new Error(`${operation} response exceeded its byte bound`);
      }
      chunks.push(value);
    }
  } catch (error) {
    await reader.cancel().catch(() => {});
    throw error;
  }
  let source;
  try {
    source = new TextDecoder("utf-8", { fatal: true }).decode(
      Buffer.concat(chunks.map((chunk) => Buffer.from(chunk))),
    );
  } catch {
    throw new Error(`${operation} response was not valid UTF-8`);
  }
  try {
    return JSON.parse(source);
  } catch {
    throw new Error(`${operation} response was not valid JSON`);
  }
}

async function cloudflareGETEnvelope(
  fetchImpl,
  apiToken,
  url,
  operation,
  includeResultInfo = false,
) {
  const controller = new AbortController();
  let timeout;
  const timeoutPromise = new Promise((_, rejectPromise) => {
    timeout = setTimeout(() => {
      controller.abort();
      rejectPromise(new Error(`${operation} timed out`));
    }, CLOUDFLARE_REQUEST_TIMEOUT_MS);
    timeout.unref?.();
  });
  try {
    const response = await Promise.race([
      fetchImpl(url, {
        method: "GET",
        headers: Object.freeze({
          accept: "application/json",
          authorization: `Bearer ${apiToken}`,
        }),
        redirect: "error",
        credentials: "omit",
        cache: "no-store",
        referrerPolicy: "no-referrer",
        signal: controller.signal,
      }),
      timeoutPromise,
    ]);
    const envelope = await Promise.race([
      boundedJSONResponse(response, url, operation),
      timeoutPromise,
    ]);
    exactKeys(
      envelope,
      includeResultInfo
        ? ["errors", "messages", "result", "result_info", "success"]
        : ["errors", "messages", "result", "success"],
      `${operation} response envelope`,
    );
    if (envelope.success !== true || !Array.isArray(envelope.errors) ||
        envelope.errors.length !== 0 || !Array.isArray(envelope.messages)) {
      throw new Error(`${operation} response envelope reported failure`);
    }
    return envelope;
  } finally {
    clearTimeout(timeout);
    controller.abort();
  }
}

function deriveContainerInstanceState(instance) {
  const status = instance?.current_placement?.status;
  if (!isRecord(status)) return "unknown";
  const raw = status.container_status ?? status.health;
  if (raw === "placed") return "provisioning";
  return INSTANCE_STATES.has(raw) && raw !== "inactive" ? raw : "unknown";
}

function normalizedContainerInstancePage(result, resultInfo, pageToken) {
  if (!isRecord(result) ||
      !Object.keys(result).every((key) =>
        key === "instances" || key === "durable_objects") ||
      !Object.hasOwn(result, "instances") ||
      !Array.isArray(result.instances) ||
      (result.durable_objects != null &&
       !Array.isArray(result.durable_objects)) ||
      !isRecord(resultInfo) ||
      !Object.keys(resultInfo).every((key) => [
        "next_page_token",
        "page_token",
        "per_page",
      ].includes(key))) {
    throw new Error("container instance response envelope was invalid");
  }
  const perPage = resultInfo.per_page ?? INSTANCE_PAGE_SIZE;
  const returnedPageToken = resultInfo.page_token ?? pageToken;
  const nextPageToken = resultInfo.next_page_token ?? null;
  if (perPage !== INSTANCE_PAGE_SIZE || returnedPageToken !== pageToken) {
    throw new Error("container instance response pagination was invalid");
  }

  const durableObjects = result.durable_objects ?? [];
  let rows;
  if (durableObjects.length === 0) {
    rows = result.instances.map((instance) => ({
      id: instance?.id ?? null,
      state: deriveContainerInstanceState(instance),
      location: instance?.location ?? null,
      version: instance?.app_version ?? null,
      created: instance?.created_at ?? null,
    }));
  } else {
    const instanceByDeploymentID = new Map();
    for (const instance of result.instances) {
      if (!isRecord(instance) || !UUID.test(String(instance.id ?? ""))) {
        throw new Error("container instance response had an invalid deployment id");
      }
      if (instanceByDeploymentID.has(instance.id)) {
        throw new Error("container instance response had a duplicate deployment id");
      }
      instanceByDeploymentID.set(instance.id, instance);
    }
    const consumedDeploymentIDs = new Set();
    rows = durableObjects.map((durableObject) => {
      const deploymentID = durableObject?.deployment_id;
      let instance;
      if (deploymentID !== null) {
        if (!UUID.test(String(deploymentID ?? ""))) {
          throw new Error(
            "container instance response had an invalid durable-object deployment id",
          );
        }
        instance = instanceByDeploymentID.get(deploymentID);
        if (instance === undefined) {
          throw new Error(
            "container instance response had an unmatched durable-object deployment id",
          );
        }
        if (consumedDeploymentIDs.has(deploymentID)) {
          throw new Error(
            "container instance response had a duplicate durable-object deployment id",
          );
        }
        consumedDeploymentIDs.add(deploymentID);
      }
      return {
        id: durableObject?.id ?? instance?.id ?? null,
        name: durableObject?.name ?? null,
        state: instance === undefined
          ? "inactive"
          : deriveContainerInstanceState(instance),
        location: instance?.location ?? null,
        version: instance?.app_version ?? null,
        created: instance?.created_at ?? durableObject?.assigned_at ?? null,
      };
    });
    if (consumedDeploymentIDs.size !== instanceByDeploymentID.size) {
      throw new Error(
        "container instance response had an unconsumed deployment",
      );
    }
  }
  return Object.freeze({
    instances: rows,
    result_info: Object.freeze({
      per_page: perPage,
      page_token: returnedPageToken,
      next_page_token: nextPageToken,
    }),
  });
}

export function createCloudflareBillingRolloutInspector({
  accountID,
  applicationID,
  apiToken,
  fetchImpl = globalThis.fetch,
}) {
  if (accountID !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID ||
      !UUID.test(String(applicationID ?? "")) ||
      typeof apiToken !== "string" || apiToken === "" ||
      apiToken.length > 4096 || /[\s\x00-\x1f\x7f]/.test(apiToken) ||
      typeof fetchImpl !== "function") {
    throw new Error("Cloudflare source inspector configuration was invalid");
  }
  const workersBase = `/accounts/${accountID}/workers/scripts/${WORKER_NAME}`;
  const containersBase = `/accounts/${accountID}/containers`;
  const get = (pathname, operation, includeResultInfo = false, query) =>
    cloudflareGETEnvelope(
      fetchImpl,
      apiToken,
      fixedCloudflareURL(pathname, query),
      operation,
      includeResultInfo,
    );
  return Object.freeze({
    async deployment() {
      const envelope = await get(
        `${workersBase}/deployments`,
        "inspect active control-plane deployment",
      );
      exactKeys(envelope.result, ["deployments"],
        "control-plane deployment result");
      if (!Array.isArray(envelope.result.deployments) ||
          envelope.result.deployments.length < 1) {
        throw new Error("control-plane deployment result was empty");
      }
      return envelope.result.deployments[0];
    },
    async version(versionID) {
      if (!UUID.test(String(versionID ?? ""))) {
        throw new Error("control-plane Worker version request was invalid");
      }
      return (await get(
        `${workersBase}/versions/${versionID}`,
        "inspect active control-plane Worker version",
      )).result;
    },
    async secrets() {
      const result = (await get(
        `${workersBase}/secrets`,
        "inspect persistent control-plane secret names",
      )).result;
      if (!Array.isArray(result)) {
        throw new Error("persistent control-plane secret result was invalid");
      }
      return result;
    },
    async application() {
      return (await get(
        `${containersBase}/applications/${applicationID}`,
        "inspect control-plane container application",
      )).result;
    },
    async instances(pageToken = null) {
      if (pageToken !== null &&
          (typeof pageToken !== "string" || pageToken === "" ||
           pageToken.length > 2048 || /[\x00-\x1f\x7f]/.test(pageToken))) {
        throw new Error("container instance page request was invalid");
      }
      const query = new URLSearchParams({
        per_page: String(INSTANCE_PAGE_SIZE),
      });
      if (pageToken !== null) query.set("page_token", pageToken);
      const envelope = await get(
        `${containersBase}/dash/applications/${applicationID}/instances`,
        "inspect control-plane container instances",
        true,
        query,
      );
      return normalizedContainerInstancePage(
        envelope.result,
        envelope.result_info,
        pageToken,
      );
    },
  });
}

export function reviewedBillingRolloutConfigIdentity(
  source,
  expectedMain,
  expectedAccountID,
  expectedReleaseVersion,
  expectedReleaseCommit,
) {
  if (typeof source !== "string" || source === "" ||
      source.length > 1024 * 1024 ||
      expectedAccountID !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID ||
      !RELEASE_VERSION.test(String(expectedReleaseVersion ?? "")) ||
      !RELEASE_COMMIT.test(String(expectedReleaseCommit ?? ""))) {
    throw new Error("reviewed billing rollout config input was invalid");
  }
  const release = expectedBuildMetadata(source, expectedMain);
  if (release.version !== expectedReleaseVersion ||
      release.commit !== expectedReleaseCommit) {
    throw new Error(
      "reviewed config did not match the explicit target release identity",
    );
  }
  return Object.freeze({
    account_id: expectedAccountID,
    config_sha256: sha256(source),
    target_release: Object.freeze({
      version: release.version,
      commit: release.commit,
      date: canonicalReleaseUTCSecond(
        release.date,
        "reviewed config target release date",
      ),
    }),
  });
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

function bindingInventory(version, expectedVersionID, expectedRelease) {
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
  const backendHandlers = Array.isArray(namedHandlers)
    ? namedHandlers.filter((handler) => handler?.name === "Backend")
    : [];
  if (!Array.isArray(namedHandlers) || backendHandlers.length !== 1 ||
      !isRecord(backendHandlers[0]) ||
      JSON.stringify(backendHandlers[0].handlers) !==
        JSON.stringify(["class"])) {
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
  for (const [name, expected] of [
    ["WITSELF_EDGE_RELEASE_VERSION", expectedRelease.version],
    ["WITSELF_EDGE_RELEASE_COMMIT", expectedRelease.commit],
  ]) {
    const binding = bindings.get(name);
    if (binding?.type !== "plain_text" || binding.text !== expected) {
      throw new Error(
        `active control-plane Worker ${name} release identity was invalid`,
      );
    }
  }
  const releaseDate = bindings.get("WITSELF_EDGE_RELEASE_DATE");
  if (releaseDate?.type !== "plain_text" ||
      canonicalReleaseUTCSecond(
        releaseDate.text,
        "active control-plane Worker WITSELF_EDGE_RELEASE_DATE",
      ) !== expectedRelease.date) {
    throw new Error(
      "active control-plane Worker WITSELF_EDGE_RELEASE_DATE release identity was invalid",
    );
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

function containerApplicationFence(application, target, namespaceID) {
  if (!isRecord(application) || application.id !== target.application_id ||
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
  const actualImageDigest = imageDigest(image);
  const targetCurrent = version === target.application_version &&
    actualImageDigest === target.image_digest &&
    maxInstances === target.max_instances;

  return Object.freeze({
    application_id: application.id,
    version,
    max_instances: maxInstances,
    // Hash the complete provider object so changes to constraints, scheduling,
    // SSH policy, observability, or future application fields cannot pass as
    // the same source identity. The raw object never leaves this process.
    application_sha256: jsonFence(application),
    target_current: targetCurrent,
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

async function inspectInstanceInventory(inspect) {
  const rows = [];
  const ids = new Set();
  const pageTokens = new Set();
  let pageToken = null;
  for (let pageNumber = 0; pageNumber < MAX_INSTANCE_PAGES; pageNumber += 1) {
    const page = await inspect.instances(pageToken);
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

async function inspectOneState(target, inspect) {
  const deployment = activeDeploymentFence(await inspect.deployment());
  const worker = bindingInventory(
    await inspect.version(deployment.version_id),
    deployment.version_id,
    target.release,
  );
  const secrets = secretNameInventory(await inspect.secrets());
  assertSameSecretInventories(worker.secret_names, secrets);
  const application = containerApplicationFence(
    await inspect.application(),
    target,
    worker.backend_namespace_id,
  );
  const instances = await inspectInstanceInventory(
    inspect,
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

const SOURCE_FENCE_KEYS = Object.freeze([
  "backend_namespace_id",
  "binding_inventory_sha256",
  "cloudflare_account_id",
  "container_application_id",
  "container_application_sha256",
  "container_application_version",
  "container_instance_count",
  "expected_target_application_id",
  "expected_target_application_version",
  "expected_target_image_digest",
  "incompatible_instance_count",
  "lifecycle_disabled_observed_at",
  "lifecycle_gate_present",
  "potential_writer_instance_count",
  "prior_lifecycle_disabled_inspection_sha256",
  "reviewed_config_sha256",
  "secret_name_inventory_sha256",
  "source_instance_inventory_sha256",
  "target_application_current",
  "target_release_commit",
  "target_release_date",
  "target_release_version",
  "target_version_instance_count",
  "worker_deployment_id",
  "worker_script_etag",
  "worker_version_id",
]);

function validatedSourceFenceAttestation(value, label) {
  exactKeys(value, [
    "billing_mutation_cohort_accounts",
    "fence",
    "inspection_sha256",
    "observed_at",
    "schema",
    "source_fleet",
  ], label);
  if (value.schema !== SOURCE_FENCE_SCHEMA ||
      value.billing_mutation_cohort_accounts !== 0 ||
      !SHA256.test(String(value.inspection_sha256 ?? ""))) {
    throw new Error(`${label} was invalid`);
  }
  const observedMilliseconds = exactUTCSecond(
    value.observed_at,
    `${label} observed_at`,
  );
  exactKeys(value.source_fleet, ["api_replicas", "reconciler_replicas"],
    `${label} source_fleet`);
  const apiReplicas = nonnegativeInteger(
    value.source_fleet.api_replicas,
    `${label} source API replicas`,
  );
  const reconcilerReplicas = nonnegativeInteger(
    value.source_fleet.reconciler_replicas,
    `${label} source reconciler replicas`,
  );

  exactKeys(value.fence, SOURCE_FENCE_KEYS, `${label} fence`);
  const fence = value.fence;
  if (fence.cloudflare_account_id !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID ||
      !UUID.test(String(fence.worker_deployment_id ?? "")) ||
      !UUID.test(String(fence.worker_version_id ?? "")) ||
      !SHA256.test(String(fence.worker_script_etag ?? "")) ||
      !HEX32.test(String(fence.backend_namespace_id ?? "")) ||
      !SHA256.test(String(fence.binding_inventory_sha256 ?? "")) ||
      !SHA256.test(String(fence.secret_name_inventory_sha256 ?? "")) ||
      !UUID.test(String(fence.container_application_id ?? "")) ||
      !SHA256.test(String(fence.container_application_sha256 ?? "")) ||
      !SHA256.test(String(fence.source_instance_inventory_sha256 ?? "")) ||
      !SHA256.test(String(fence.reviewed_config_sha256 ?? "")) ||
      !UUID.test(String(fence.expected_target_application_id ?? "")) ||
      !IMAGE_DIGEST.test(String(fence.expected_target_image_digest ?? "")) ||
      !RELEASE_VERSION.test(String(fence.target_release_version ?? "")) ||
      !RELEASE_COMMIT.test(String(fence.target_release_commit ?? "")) ||
      typeof fence.lifecycle_gate_present !== "boolean" ||
      typeof fence.target_application_current !== "boolean") {
    throw new Error(`${label} fence was invalid`);
  }
  exactUTCSecond(fence.target_release_date, `${label} target release date`);
  positiveInteger(
    fence.container_application_version,
    `${label} container application version`,
  );
  positiveInteger(
    fence.expected_target_application_version,
    `${label} expected target application version`,
  );
  const instanceCount = nonnegativeInteger(
    fence.container_instance_count,
    `${label} container instance count`,
  );
  const targetInstances = nonnegativeInteger(
    fence.target_version_instance_count,
    `${label} target-version instance count`,
  );
  const incompatibleInstances = nonnegativeInteger(
    fence.incompatible_instance_count,
    `${label} incompatible instance count`,
  );
  const potentialWriters = nonnegativeInteger(
    fence.potential_writer_instance_count,
    `${label} potential writer instance count`,
  );
  if (potentialWriters !== targetInstances + incompatibleInstances ||
      potentialWriters > instanceCount ||
      fence.container_application_id !==
        fence.expected_target_application_id ||
      (fence.target_application_current &&
       fence.container_application_version !==
         fence.expected_target_application_version) ||
      apiReplicas !== Math.max(
        potentialWriters,
        fence.target_application_current ? 0 : 1,
      )) {
    throw new Error(`${label} instance counts were inconsistent`);
  }
  const priorHash = fence.prior_lifecycle_disabled_inspection_sha256;
  const disabledObservedAt = fence.lifecycle_disabled_observed_at;
  if ((priorHash === null) !== (disabledObservedAt === null) ||
      (priorHash !== null && !SHA256.test(String(priorHash))) ||
      (disabledObservedAt !== null &&
       exactUTCSecond(disabledObservedAt,
         `${label} lifecycle disabled observation`) > observedMilliseconds)) {
    throw new Error(`${label} lifecycle-disabled fence was invalid`);
  }
  const outsideInFlightBound = disabledObservedAt !== null &&
    observedMilliseconds - exactUTCSecond(
      disabledObservedAt,
      `${label} lifecycle disabled observation`,
    ) >= LIFECYCLE_IN_FLIGHT_BOUND_MS;
  const expectedReconcilers = !fence.lifecycle_gate_present &&
      apiReplicas === 0 && outsideInFlightBound
    ? 0
    : Math.max(1, apiReplicas);
  if (reconcilerReplicas !== expectedReconcilers) {
    throw new Error(`${label} reconciler count was inconsistent`);
  }
  const { inspection_sha256: ignored, ...unsigned } = value;
  if (jsonFence(unsigned) !== value.inspection_sha256) {
    throw new Error(`${label} self hash did not match`);
  }
  return Object.freeze({
    value,
    observedMilliseconds,
  });
}

function stableSourceFenceIdentity(fence) {
  const copy = { ...fence };
  // Exact target instances may naturally stop, become inactive, or be freshly
  // listed while the R2 scan runs. They cannot be equality-bound. Both endpoint
  // observations instead have to prove zero non-null-version rows.
  delete copy.container_instance_count;
  delete copy.source_instance_inventory_sha256;
  delete copy.target_version_instance_count;
  delete copy.incompatible_instance_count;
  delete copy.potential_writer_instance_count;
  return copy;
}

function disabledAuthorityIdentity(fence) {
  const copy = stableSourceFenceIdentity(fence);
  // These two fields bind a later observation to its initial attestation.
  // They are necessarily absent from that initial attestation itself.
  delete copy.lifecycle_disabled_observed_at;
  delete copy.prior_lifecycle_disabled_inspection_sha256;
  return copy;
}

function assertInitialDisabledAttestation(prior, target) {
  const checked = validatedSourceFenceAttestation(
    prior,
    "prior lifecycle-disabled attestation",
  );
  const fence = prior.fence;
  if (prior.source_fleet.api_replicas !== 0 ||
      fence.potential_writer_instance_count !== 0 ||
      prior.source_fleet.reconciler_replicas !== 1 ||
      fence.lifecycle_gate_present || !fence.target_application_current ||
      fence.prior_lifecycle_disabled_inspection_sha256 !== null ||
      fence.lifecycle_disabled_observed_at !== null ||
      fence.cloudflare_account_id !== target.account_id ||
      fence.reviewed_config_sha256 !== target.reviewed_config_sha256 ||
      fence.expected_target_application_id !== target.application_id ||
      fence.expected_target_application_version !== target.application_version ||
      fence.expected_target_image_digest !== target.image_digest ||
      fence.target_release_version !== target.release.version ||
      fence.target_release_commit !== target.release.commit ||
      fence.target_release_date !== target.release.date) {
    throw new Error(
      "prior lifecycle-disabled attestation did not prove the exact stopped target",
    );
  }
  return checked;
}

export function assertBillingRolloutSourceFenceBracket(before, after) {
  const left = validatedSourceFenceAttestation(
    before,
    "pre-inventory source fence",
  );
  const right = validatedSourceFenceAttestation(
    after,
    "post-inventory source fence",
  );
  if (left.observedMilliseconds >= right.observedMilliseconds ||
      before.source_fleet.api_replicas !== 0 ||
      before.source_fleet.reconciler_replicas !== 0 ||
      after.source_fleet.api_replicas !== 0 ||
      after.source_fleet.reconciler_replicas !== 0 ||
      before.fence.potential_writer_instance_count !== 0 ||
      after.fence.potential_writer_instance_count !== 0 ||
      before.fence.lifecycle_gate_present ||
      after.fence.lifecycle_gate_present ||
      !before.fence.target_application_current ||
      !after.fence.target_application_current ||
      before.fence.prior_lifecycle_disabled_inspection_sha256 === null ||
      before.fence.prior_lifecycle_disabled_inspection_sha256 !==
        after.fence.prior_lifecycle_disabled_inspection_sha256 ||
      !sameJSON(
        stableSourceFenceIdentity(before.fence),
        stableSourceFenceIdentity(after.fence),
      )) {
    throw new Error(
      "source fleet did not remain exactly stopped around inventory capture",
    );
  }
  return Object.freeze({
    pre_inspection_sha256: before.inspection_sha256,
    post_inspection_sha256: after.inspection_sha256,
    bracket_sha256: jsonFence({
      pre_inspection_sha256: before.inspection_sha256,
      post_inspection_sha256: after.inspection_sha256,
    }),
  });
}

/**
 * Produce one private, count-and-fence-only Cloudflare observation.
 *
 * The caller must retain an earlier absence observation, wait through the
 * lifecycle in-flight bound, take this observation around the complete R2
 * scan, and compare pre/post fence fields. This function deliberately cannot
 * turn one operator-supplied timestamp into proof of continuous quiescence.
 */
export async function observeBillingRolloutSourceFence(
  options,
  inspect,
  clock = () => new Date(),
) {
  if (!isRecord(options) || typeof options.config !== "string" ||
      options.config === "" || !isRecord(inspect)) {
    throw new Error("source fence options were invalid");
  }
  const target = exactTargetIdentity(options);
  const prior = options.priorLifecycleDisabledAttestation ?? null;
  const checkedPrior = prior === null
    ? null
    : assertInitialDisabledAttestation(prior, target);

  const first = await inspectOneState(target, inspect);
  const second = await inspectOneState(target, inspect);
  if (!samePrivateState(first, second)) {
    throw new Error(
      "control-plane source state changed during exact provider inspection",
    );
  }
  const finalDeployment = activeDeploymentFence(await inspect.deployment());
  if (!sameJSON(finalDeployment, second.deployment)) {
    throw new Error(
      "control-plane deployment changed after exact provider inspection",
    );
  }

  const observed = observedAt(clock);
  const observedMilliseconds = exactUTCSecond(observed, "observed_at");
  const targetInstances = second.instances.filter((instance) =>
    instance.version === target.application_version).length;
  const incompatibleInstances = second.instances.filter((instance) =>
    instance.version !== null &&
    instance.version !== target.application_version).length;
  // A stopped row can retain an old environment and resume. Only the
  // provider's inactive/version-null tombstone is a non-writer.
  const potentialWriters = second.instances.filter((instance) =>
    instance.version !== null).length;
  const apiReplicas = Math.max(
    potentialWriters,
    second.application.target_current ? 0 : 1,
  );
  const lifecycleGatePresent = second.secrets.includes(
    "CP_PLAN_LIFECYCLE_ENABLED",
  );
  const disabledObservedAt = checkedPrior?.value.observed_at ?? null;
  const disabledMilliseconds = checkedPrior?.observedMilliseconds ?? null;
  if (disabledMilliseconds !== null &&
      disabledMilliseconds > observedMilliseconds) {
    throw new Error("prior lifecycle-disabled attestation is in the future");
  }
  const outsideInFlightBound = disabledMilliseconds !== null &&
    observedMilliseconds - disabledMilliseconds >=
      LIFECYCLE_IN_FLIGHT_BOUND_MS;

  const fence = Object.freeze({
    ...publicStateFence(second),
    cloudflare_account_id: target.account_id,
    reviewed_config_sha256: target.reviewed_config_sha256,
    expected_target_application_id: target.application_id,
    expected_target_application_version: target.application_version,
    expected_target_image_digest: target.image_digest,
    target_release_version: target.release.version,
    target_release_commit: target.release.commit,
    target_release_date: target.release.date,
    target_application_current: second.application.target_current,
    target_version_instance_count: targetInstances,
    incompatible_instance_count: incompatibleInstances,
    potential_writer_instance_count: potentialWriters,
    lifecycle_gate_present: lifecycleGatePresent,
    lifecycle_disabled_observed_at: disabledObservedAt,
    prior_lifecycle_disabled_inspection_sha256:
      checkedPrior?.value.inspection_sha256 ?? null,
  });
  if (checkedPrior !== null && !sameJSON(
    disabledAuthorityIdentity(checkedPrior.value.fence),
    disabledAuthorityIdentity(fence),
  )) {
    throw new Error(
      "source identity changed since the prior lifecycle-disabled attestation",
    );
  }
  const reconcilerReplicas = !lifecycleGatePresent && apiReplicas === 0 &&
      outsideInFlightBound && checkedPrior !== null
    ? 0
    : Math.max(1, apiReplicas);
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
  const attestation = Object.freeze({
    ...unsigned,
    inspection_sha256: jsonFence(unsigned),
  });
  validatedSourceFenceAttestation(attestation, "generated source fence");
  return attestation;
}

function sameFileIdentity(left, right) {
  return left.dev === right.dev && left.ino === right.ino;
}

function readStablePrivateTextFile(
  path,
  label,
  maxBytes,
  allowedModes,
  requireRealPath = false,
) {
  if (typeof path !== "string" || resolve(path) !== path ||
      !Number.isSafeInteger(maxBytes) || maxBytes < 1 ||
      !(allowedModes instanceof Set) || allowedModes.size === 0) {
    throw new Error(`${label} path policy was invalid`);
  }
  if (requireRealPath && realpathSync(path) !== path) {
    throw new Error(`${label} path contained a symbolic link`);
  }
  const before = lstatSync(path);
  if (!before.isFile() || before.isSymbolicLink() || before.size < 1 ||
      before.size > maxBytes || !allowedModes.has(before.mode & 0o777)) {
    throw new Error(`${label} was not an exact private regular file`);
  }

  const descriptor = openSync(
    path,
    fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0),
  );
  let raw;
  try {
    const opened = fstatSync(descriptor);
    if (!opened.isFile() || !sameFileIdentity(before, opened) ||
        !allowedModes.has(opened.mode & 0o777)) {
      throw new Error(`${label} identity changed while opening`);
    }
    raw = readFileSync(descriptor);
    const finalDescriptor = fstatSync(descriptor);
    const finalPath = lstatSync(path);
    if (!sameFileIdentity(opened, finalDescriptor) ||
        !sameFileIdentity(finalDescriptor, finalPath) ||
        finalDescriptor.size !== raw.length || raw.length < 1 ||
        raw.length > maxBytes ||
        opened.size !== finalDescriptor.size ||
        opened.mtimeMs !== finalDescriptor.mtimeMs ||
        opened.ctimeMs !== finalDescriptor.ctimeMs ||
        !allowedModes.has(finalDescriptor.mode & 0o777) ||
        !allowedModes.has(finalPath.mode & 0o777)) {
      throw new Error(`${label} changed while reading`);
    }
  } finally {
    closeSync(descriptor);
  }
  const source = raw.toString("utf8");
  if (!Buffer.from(source, "utf8").equals(raw)) {
    throw new Error(`${label} was not valid UTF-8`);
  }
  return source;
}

export function readBillingRolloutSourceFenceAttestationFile(path) {
  const source = readStablePrivateTextFile(
    path,
    "prior lifecycle-disabled attestation",
    MAX_PRIOR_ATTESTATION_BYTES,
    new Set([0o600]),
    true,
  );
  let parsed;
  try {
    parsed = JSON.parse(source);
  } catch {
    throw new Error(
      "prior lifecycle-disabled attestation was not valid JSON",
    );
  }
  if (source !== `${canonicalJSON(parsed)}\n`) {
    throw new Error(
      "prior lifecycle-disabled attestation was not one canonical JSON line",
    );
  }
  return validatedSourceFenceAttestation(
    parsed,
    "prior lifecycle-disabled attestation",
  ).value;
}

export function parseBillingRolloutSourceFenceArgs(argv) {
  const options = {
    config: "",
    expectedAccountID: "",
    expectedTargetApplicationID: "",
    expectedTargetApplicationVersion: 0,
    expectedTargetImageDigest: "",
    expectedTargetReleaseVersion: "",
    expectedTargetReleaseCommit: "",
    priorLifecycleDisabledAttestationPath: null,
  };
  const names = new Set([
    "--config",
    "--expected-account-id",
    "--expected-target-application-id",
    "--expected-target-application-version",
    "--expected-target-image-digest",
    "--expected-target-release-version",
    "--expected-target-release-commit",
    "--prior-lifecycle-disabled-attestation",
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
      options.config = resolve(value);
      if (options.config !== value) {
        throw new Error("--config must be a normalized absolute path");
      }
      break;
    case "--expected-account-id":
      options.expectedAccountID = value;
      break;
    case "--expected-target-application-id":
      options.expectedTargetApplicationID = value;
      break;
    case "--expected-target-application-version":
      if (!/^[1-9][0-9]*$/.test(value)) {
        throw new Error(
          "--expected-target-application-version must be a canonical positive integer",
        );
      }
      options.expectedTargetApplicationVersion = Number(value);
      break;
    case "--expected-target-image-digest":
      options.expectedTargetImageDigest = value;
      break;
    case "--expected-target-release-version":
      options.expectedTargetReleaseVersion = value;
      break;
    case "--expected-target-release-commit":
      options.expectedTargetReleaseCommit = value;
      break;
    case "--prior-lifecycle-disabled-attestation":
      options.priorLifecycleDisabledAttestationPath = resolve(value);
      if (options.priorLifecycleDisabledAttestationPath !== value) {
        throw new Error(
          "--prior-lifecycle-disabled-attestation must be a normalized absolute path",
        );
      }
      break;
    }
  }
  for (const name of [
    "--config",
    "--expected-account-id",
    "--expected-target-application-id",
    "--expected-target-application-version",
    "--expected-target-image-digest",
    "--expected-target-release-version",
    "--expected-target-release-commit",
  ]) {
    if (!seen.has(name)) throw new Error(`${name} is required`);
  }
  if (options.expectedAccountID !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID) {
    throw new Error(
      `--expected-account-id must be ${PRODUCTION_CLOUDFLARE_ACCOUNT_ID}`,
    );
  }
  if (!UUID.test(options.expectedTargetApplicationID)) {
    throw new Error(
      "--expected-target-application-id must be a valid lowercase UUID",
    );
  }
  positiveInteger(
    options.expectedTargetApplicationVersion,
    "--expected-target-application-version",
  );
  if (!IMAGE_DIGEST.test(options.expectedTargetImageDigest)) {
    throw new Error(
      "--expected-target-image-digest must be sha256 followed by 64 lowercase hex characters",
    );
  }
  if (!RELEASE_VERSION.test(options.expectedTargetReleaseVersion)) {
    throw new Error(
      "--expected-target-release-version must be a canonical release version",
    );
  }
  if (!RELEASE_COMMIT.test(options.expectedTargetReleaseCommit)) {
    throw new Error(
      "--expected-target-release-commit must be a lowercase 40-hex commit",
    );
  }
  if (options.priorLifecycleDisabledAttestationPath === options.config) {
    throw new Error(
      "--config and --prior-lifecycle-disabled-attestation must differ",
    );
  }
  return Object.freeze(options);
}

export async function main(
  argv = process.argv.slice(2),
  {
    fetchImpl = globalThis.fetch,
    clock = () => new Date(),
    writeOutput = (value) => process.stdout.write(value),
    expectedControlPlaneRoot = undefined,
  } = {},
) {
  if (typeof writeOutput !== "function") {
    throw new Error("source fence output sink was invalid");
  }
  const options = parseBillingRolloutSourceFenceArgs(argv);
  const readReviewedConfig = () => {
    // This production observation must run from the immutable tagged release
    // snapshot and its frozen 0400 private config. A mutable checkout config
    // could otherwise change only while the provider identity is inspected and
    // restore the reviewed bytes before the final hash check.
    const expectedMain = privateDeploymentConfigMain(
      options.config,
      expectedControlPlaneRoot,
    );
    const source = readStablePrivateTextFile(
      options.config,
      "reviewed generated control-plane config",
      MAX_CONFIG_BYTES,
      new Set([0o400, 0o600]),
    );
    return reviewedBillingRolloutConfigIdentity(
      source,
      expectedMain,
      options.expectedAccountID,
      options.expectedTargetReleaseVersion,
      options.expectedTargetReleaseCommit,
    );
  };
  const reviewedConfigIdentity = readReviewedConfig();
  const priorLifecycleDisabledAttestation =
    options.priorLifecycleDisabledAttestationPath === null
      ? null
      : readBillingRolloutSourceFenceAttestationFile(
        options.priorLifecycleDisabledAttestationPath,
      );
  if (process.env.CLOUDFLARE_ACCOUNT_ID !== options.expectedAccountID) {
    throw new Error("Cloudflare source inspector account identity was invalid");
  }
  const inspect = createCloudflareBillingRolloutInspector({
    accountID: options.expectedAccountID,
    applicationID: options.expectedTargetApplicationID,
    apiToken: process.env.CLOUDFLARE_API_TOKEN,
    fetchImpl,
  });
  const attestation = await observeBillingRolloutSourceFence({
    ...options,
    reviewedConfigIdentity,
    priorLifecycleDisabledAttestation,
  }, inspect, clock);
  if (!sameJSON(readReviewedConfig(), reviewedConfigIdentity)) {
    throw new Error(
      "reviewed generated control-plane config changed during inspection",
    );
  }
  writeOutput(`${canonicalJSON(attestation)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(
      `billing rollout source fence: FAIL: ${String(error?.message ?? error)}\n`,
    );
    process.exitCode = 1;
  });
}
