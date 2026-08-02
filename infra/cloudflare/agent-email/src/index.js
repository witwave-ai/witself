import {
  base64Standard,
  base64URL,
  importSigningKey,
  normalizeEnvelopeAddress,
  normalizeRelayMetadata,
  PILOT_MAXIMUM_RAW_BYTES,
  sha256Hex,
  signRelay,
} from "./relay.mjs";
import {
  CONFIG_KEY,
  parsePilotAddress,
  parseRouteAddress,
  realmRouteKey,
  realmRouteProjectionIsFresh,
  recipientKey,
  validateRealmRouteProjection,
  validateRuntimeConfig,
  validateRuntimeRecipient,
} from "./directory.mjs";
import { recordEdgeVerdict } from "./metrics.mjs";

const PERMANENT_REJECTION = "recipient unavailable";
const OVER_SIZE_REJECTION = "message too large";
const TRANSIENT_ERROR = "agent email relay temporarily unavailable";
const DEFAULT_TIMEOUT_MS = 20_000;
const DEFAULT_DIRECTORY_TIMEOUT_MS = 3_000;
const MAX_VERDICT_BYTES = 4_096;
const MAX_ROUTE_BYTES = 4_096;

let cachedSecret = "";
let cachedSigningKey;

const EDGE_METRIC = Symbol("agent-email-edge-metric");

function transient(outcome = "tempfail_internal", phase = "internal", status = 0) {
  const error = new Error(TRANSIENT_ERROR);
  error[EDGE_METRIC] = { outcome, phase, status };
  return error;
}

function logRelayFailure(fields) {
  // Keep relay diagnostics value-free: never log envelope addresses, raw
  // message content, digests, signatures, or directory values.
  console.warn(JSON.stringify({ event: "agent_email_relay_failure", ...fields }));
}

async function directoryJSON(namespace, key) {
  try {
    return await namespace.get(key, "json");
  } catch {
    throw transient("tempfail_directory", "directory");
  }
}

async function optionalDirectoryJSON(namespace, key) {
  try {
    return { ok: true, value: await namespace.get(key, "json") };
  } catch {
    return { ok: false, value: null };
  }
}

function boundedTimeout(value, defaultValue, minimum, maximum) {
  const number = Number(value ?? defaultValue);
  return Number.isSafeInteger(number) && number >= minimum && number <= maximum
    ? number
    : defaultValue;
}

function controlPlaneRouteRequest(env, parsed) {
  const rawURL = String(env?.CONTROL_PLANE_URL ?? "");
  const token = String(env?.CONTROL_PLANE_EDGE_TOKEN ?? "");
  if (!rawURL && !token) return null;
  if (!rawURL || !token || token !== token.trim() || token.length < 16 || token.length > 8_192) {
    throw transient("tempfail_configuration", "configuration");
  }
  let base;
  try {
    base = new URL(rawURL);
  } catch {
    throw transient("tempfail_configuration", "configuration");
  }
  if (
    base.protocol !== "https:" || base.username || base.password || base.hash || base.search ||
    !base.hostname || base.hostname === "localhost" || (base.pathname !== "/" && base.pathname !== "")
  ) {
    throw transient("tempfail_configuration", "configuration");
  }
  const url = new URL(
    `/v1/email/realm-routes/${encodeURIComponent(parsed.domain)}/${encodeURIComponent(parsed.realmLabel)}`,
    base,
  );
  return new Request(url, {
    method: "GET",
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${token}`,
    },
    redirect: "manual",
  });
}

async function controlPlaneRealmRoute(
  env,
  parsed,
  fetchAPI,
  nowMS,
  minimumRevision = 0,
  missingIsTransient = false,
) {
  const request = controlPlaneRouteRequest(env, parsed);
  if (!request) throw transient("tempfail_route_lookup", "route");
  const timeoutMS = boundedTimeout(
    env?.DIRECTORY_TIMEOUT_MS,
    DEFAULT_DIRECTORY_TIMEOUT_MS,
    250,
    5_000,
  );
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMS);
  let response;
  try {
    response = await fetchAPI(new Request(request, { signal: controller.signal }));
  } catch {
    throw transient("tempfail_route_lookup", "route");
  } finally {
    clearTimeout(timer);
  }
  if (response.status === 404) {
    if (minimumRevision > 0 || missingIsTransient) {
      throw transient("tempfail_route_lookup", "route", response.status);
    }
    return { status: "unknown" };
  }
  if (response.status !== 200) throw transient("tempfail_route_lookup", "route", response.status);

  let value;
  try {
    value = JSON.parse(await boundedResponseText(response, MAX_ROUTE_BYTES));
  } catch {
    throw transient("tempfail_route_lookup", "route", response.status);
  }
  let route;
  try {
    route = validateRealmRouteProjection(value, parsed.domain, parsed.realmLabel);
  } catch {
    throw transient("tempfail_route_lookup", "route", response.status);
  }
  if (
    route.controller_revision < minimumRevision ||
    !realmRouteProjectionIsFresh(route, nowMS)
  ) {
    throw transient("tempfail_route_lookup", "route", response.status);
  }
  return { status: "projection", route };
}

function realmRouteDisposition(route) {
  switch (route.state) {
    case "applied":
      return { status: "route", route };
    case "suspended":
      if (route.suspension_disposition === "inactive") {
        return { status: "inactive" };
      }
      throw transient("tempfail_suspended_route", "route");
    case "retired":
      return { status: "inactive" };
    default:
      throw transient("tempfail_route_lookup", "route");
  }
}

async function projectedRealmRoute(
  env,
  parsed,
  fetchAPI,
  nowMS,
  projectedValue,
  uncertainDirectory = false,
) {
  let route;
  try {
    route = validateRealmRouteProjection(projectedValue, parsed.domain, parsed.realmLabel);
  } catch {
    const fallback = await controlPlaneRealmRoute(
      env,
      parsed,
      fetchAPI,
      nowMS,
      0,
      uncertainDirectory || projectedValue !== null,
    );
    return fallback.status === "projection" ? realmRouteDisposition(fallback.route) : fallback;
  }
  if (realmRouteProjectionIsFresh(route, nowMS)) return realmRouteDisposition(route);
  const fallback = await controlPlaneRealmRoute(
    env,
    parsed,
    fetchAPI,
    nowMS,
    route.controller_revision,
  );
  return fallback.status === "projection" ? realmRouteDisposition(fallback.route) : fallback;
}

async function legacyPilotRoute(env, parsed, envelopeTo) {
  const configValue = await directoryJSON(env.EMAIL_DIRECTORY, CONFIG_KEY);
  if (configValue == null) return null;
  // A corrupt legacy row must not poison lookups for unrelated production
  // realm labels. Only enter the compatibility path when its raw lookup
  // binding names this exact address domain and realm label.
  if (configValue?.domain !== parsed.domain || configValue?.realm_label !== parsed.realmLabel) {
    return null;
  }
  let config;
  try {
    config = validateRuntimeConfig(configValue);
  } catch {
    throw transient("tempfail_configuration", "configuration");
  }
  if (!config.enabled) throw transient("tempfail_disabled", "configuration");

  let pilotAddress;
  try {
    pilotAddress = parsePilotAddress(envelopeTo, config.domain, config.realm_label, true);
  } catch {
    return { status: "invalid" };
  }
  const enrolled = config.agents.some((agent) => agent.address === pilotAddress.baseAddress);
  if (!enrolled) return { status: "unknown" };
  const recipientValue = await directoryJSON(env.EMAIL_DIRECTORY, recipientKey(pilotAddress.baseAddress));
  // The address is enrolled in the atomic config projection. A missing or
  // inconsistent detail row can be KV propagation lag or operator error, so
  // it must retry rather than permanently bouncing an enrolled mailbox.
  if (recipientValue == null) throw transient("tempfail_directory", "directory");
  try {
    validateRuntimeRecipient(recipientValue, config, pilotAddress.baseAddress);
  } catch {
    throw transient("tempfail_directory", "directory");
  }
  return {
    status: "route",
    route: {
      route_kind: "pilot",
      state: "applied",
      realm_id: recipientValue.realm_id,
      cell_audience: recipientValue.cell_audience,
      ingest_url: recipientValue.ingest_url,
    },
  };
}

async function resolveRealmRoute(env, parsed, envelopeTo, fetchAPI, nowMS) {
  const projected = await optionalDirectoryJSON(
    env.EMAIL_DIRECTORY,
    realmRouteKey(parsed.domain, parsed.realmLabel),
  );
  if (!projected.ok) {
    return projectedRealmRoute(env, parsed, fetchAPI, nowMS, null, true);
  }
  if (projected.value != null) {
    return projectedRealmRoute(env, parsed, fetchAPI, nowMS, projected.value);
  }

  const legacy = await legacyPilotRoute(env, parsed, envelopeTo);
  if (legacy != null) return legacy;
  const fallback = await controlPlaneRealmRoute(env, parsed, fetchAPI, nowMS);
  return fallback.status === "projection" ? realmRouteDisposition(fallback.route) : fallback;
}

async function signingKey(env, cryptoAPI) {
  const secret = String(env.RELAY_ED25519_PRIVATE_KEY ?? "");
  if (!cachedSigningKey || secret !== cachedSecret) {
    cachedSecret = secret;
    cachedSigningKey = importSigningKey(secret, cryptoAPI).catch(() => {
      cachedSigningKey = undefined;
      throw transient("tempfail_signing", "signing");
    });
  }
  return cachedSigningKey;
}

async function boundedResponseText(response, maximumBytes = MAX_VERDICT_BYTES) {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let received = 0;
  let result = "";
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      received += value.byteLength;
      if (received > maximumBytes) throw transient("tempfail_cell_response", "response");
      result += decoder.decode(value, { stream: true });
    }
    return result + decoder.decode();
  } finally {
    reader.releaseLock();
  }
}

function exactVerdict(text) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    return "";
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) return "";
  const keys = Object.keys(value);
  if (keys.length !== 1 || keys[0] !== "verdict" || typeof value.verdict !== "string") return "";
  switch (value.verdict) {
    case "accepted":
    case "unknown_recipient":
    case "permanent":
    case "receive_disabled":
    case "feature_disabled":
    case "temporary":
    case "invalid_relay":
    case "retry_canary_rejected":
    case "over_size":
    case "rate_limited":
      return value.verdict;
    default:
      return "";
  }
}

function relayHeaders(metadata, signature) {
  return new Headers({
    "Content-Type": "message/rfc822",
    "X-Witself-Email-Version": "witself-email-relay-pilot-v1",
    "X-Witself-Email-Timestamp": String(metadata.timestamp),
    "X-Witself-Email-Key-Id": metadata.keyId,
    "X-Witself-Email-Audience": metadata.audience,
    "X-Witself-Email-Envelope-From": base64URL(new TextEncoder().encode(metadata.envelopeFrom)),
    "X-Witself-Email-Envelope-To": base64URL(new TextEncoder().encode(metadata.envelopeTo)),
    "X-Witself-Email-Raw-Size": String(metadata.rawSize),
    "X-Witself-Email-Raw-SHA256": `sha256:${metadata.rawSHA256}`,
    "X-Witself-Email-Signature": base64Standard(signature),
  });
}

async function handleEmailTransaction(message, env, runtime = {}) {
  const fetchAPI = runtime.fetch ?? fetch;
  const cryptoAPI = runtime.crypto ?? crypto;
  const now = runtime.now ?? (() => Date.now());
  if (!env?.EMAIL_DIRECTORY || typeof env.EMAIL_DIRECTORY.get !== "function") {
    throw transient("tempfail_configuration", "configuration");
  }

  let envelopeTo;
  let parsed;
  try {
    envelopeTo = normalizeEnvelopeAddress(message.to, false);
    parsed = parseRouteAddress(envelopeTo, true);
  } catch {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_invalid_recipient", phase: "recipient", status: 550 };
  }

  const resolved = await resolveRealmRoute(env, parsed, envelopeTo, fetchAPI, now());
  if (resolved.status === "invalid") {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_invalid_recipient", phase: "recipient", status: 550 };
  }
  if (resolved.status === "unknown") {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_unknown_recipient", phase: "recipient", status: 550 };
  }
  if (resolved.status === "inactive") {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_inactive_route", phase: "route", status: 550 };
  }
  const route = resolved.route;
  // This fleet-wide edge gate is independent of per-account plan state and
  // route projection freshness. It is exact-true and default-off so a config
  // omission or emergency flip immediately tempfails only custom realm aliases
  // across every account; canonical Realm ID addresses remain available.
  if (route.route_kind === "realm_alias" &&
      String(env?.REALM_EMAIL_ALIAS_DELIVERY_ENABLED ?? "") !== "true") {
    throw transient("tempfail_alias_gate", "route");
  }

  if (
    !Number.isSafeInteger(message.rawSize) ||
    message.rawSize < 1 ||
    message.rawSize > PILOT_MAXIMUM_RAW_BYTES
  ) {
    message.setReject(OVER_SIZE_REJECTION);
    return { outcome: "rejected_over_size", phase: "content", status: 552 };
  }

  let raw;
  try {
    raw = await new Response(message.raw).arrayBuffer();
  } catch {
    throw transient("tempfail_content", "content");
  }
  if (raw.byteLength !== message.rawSize || raw.byteLength > PILOT_MAXIMUM_RAW_BYTES) {
    throw transient("tempfail_content", "content");
  }

  let metadata;
  try {
    metadata = normalizeRelayMetadata({
      timestamp: Math.floor(now() / 1000),
      keyId: env.RELAY_KEY_ID,
      envelopeFrom: normalizeEnvelopeAddress(message.from ?? "", true),
      envelopeTo,
      audience: route.cell_audience,
      rawSize: raw.byteLength,
      rawSHA256: await sha256Hex(raw, cryptoAPI),
    });
  } catch {
    throw transient("tempfail_signing", "signing");
  }

  let signature;
  try {
    const key = await signingKey(env, cryptoAPI);
    ({ signature } = await signRelay(metadata, key, cryptoAPI));
  } catch {
    throw transient("tempfail_signing", "signing");
  }

  const timeoutValue = Number(env.RELAY_TIMEOUT_MS ?? DEFAULT_TIMEOUT_MS);
  const timeoutMS = Number.isSafeInteger(timeoutValue) && timeoutValue >= 1_000 && timeoutValue <= 30_000
    ? timeoutValue
    : DEFAULT_TIMEOUT_MS;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMS);
  let response;
  let verdict;
  try {
    response = await fetchAPI(route.ingest_url, {
      method: "POST",
      headers: relayHeaders(metadata, signature),
      body: raw,
      // Cloudflare Workers implements follow/manual, not RequestRedirect
      // "error". Manual keeps the signed relay headers on this exact origin;
      // any 3xx falls through to the fail-closed response check below.
      redirect: "manual",
      signal: controller.signal,
    });
    verdict = exactVerdict(await boundedResponseText(response));
  } catch (error) {
    logRelayFailure({
      phase: "fetch",
      error_name: error instanceof Error ? error.name : "unknown",
    });
    throw transient("tempfail_transport", "fetch");
  } finally {
    clearTimeout(timer);
  }
  if (response.ok && verdict === "accepted") {
    return { outcome: "accepted", phase: "response", status: response.status };
  }
  if (response.ok && verdict === "feature_disabled") {
    // Account-plan receipt is an intentional accept-and-drop disposition.
    // Returning normally prevents provider retries and exposes no account
    // policy detail to the external sender.
    return { outcome: "discarded_feature_disabled", phase: "response", status: response.status };
  }
  if (verdict === "over_size") {
    message.setReject(OVER_SIZE_REJECTION);
    return { outcome: "rejected_over_size", phase: "response", status: 552 };
  }
  if (verdict === "rate_limited" && response.status === 429) {
    // The cell is the authoritative cross-replica admission point. Surface a
    // sanitized temporary SMTP failure so the provider can retry without
    // revealing which internal bucket refused the signed attempt.
    throw transient("tempfail_rate_limited", "response", response.status);
  }
  logRelayFailure({
    phase: "response",
    status: response.status,
    verdict: verdict || "invalid",
  });
  if (verdict === "unknown_recipient" || verdict === "permanent") {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_cell_permanent", phase: "response", status: response.status };
  }
  if (verdict === "retry_canary_rejected") {
    message.setReject(PERMANENT_REJECTION);
    return { outcome: "rejected_retry_canary", phase: "response", status: response.status };
  }
  if (verdict === "receive_disabled") {
    throw transient("tempfail_disabled", "response", response.status);
  }
  throw transient("tempfail_cell_response", "response", response.status);
}

export async function handleEmail(message, env, runtime = {}) {
  const now = runtime.now ?? (() => Date.now());
  const startedAt = now();
  const rawSize = Number.isSafeInteger(message?.rawSize) && message.rawSize > 0 ? message.rawSize : 0;
  try {
    const result = await handleEmailTransaction(message, env, { ...runtime, now });
    recordEdgeVerdict(env, {
      ...result,
      durationMS: Math.max(0, now() - startedAt),
      rawSize,
    });
  } catch (error) {
    const metric = error?.[EDGE_METRIC] ?? {
      outcome: "tempfail_internal",
      phase: "internal",
      status: 0,
    };
    recordEdgeVerdict(env, {
      ...metric,
      durationMS: Math.max(0, now() - startedAt),
      rawSize,
    });
    throw error;
  }
}

export default {
  async email(message, env) {
    await handleEmail(message, env);
  },
};
