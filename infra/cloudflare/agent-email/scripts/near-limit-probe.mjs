#!/usr/bin/env node
import { createCipheriv, createHash, randomUUID } from "node:crypto";

import { parseRouteAddress } from "../src/directory.mjs";
import { RELAY_MAXIMUM_RAW_BYTES } from "../src/relay.mjs";
import {
  assertProductionCloudflareIdentity,
} from "./wrangler-environment.mjs";
import { isProductionCellHost } from "./production-cell-endpoint.mjs";

const CLOUDFLARE_API_ROOT = "https://api.cloudflare.com/client/v4";
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const MESSAGE_ID = /^emsg_[a-z2-7]{16}$/;
const CLAIM_ID = /^ecl_[a-z2-7]{16}$/;
const SIMPLE_LOCAL_PART = /^[a-z0-9][a-z0-9._%+-]*$/;
const SIMPLE_DOMAIN = /^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/;
const CANONICAL_REALM_LABEL = /^[a-z2-7]{16}$/;
const PRODUCTION_DOMAIN = "witmail.net";
const PRODUCTION_CANARY_FROM = "canary@send.witmail.net";
const DISCOVERY_PAGE_SIZE = 100;
const MAXIMUM_DISCOVERY_PAGES = 20;
const MAXIMUM_CURSOR_BYTES = 4096;
const DISCOVERY_RETRY_MS = 2000;
const CLAIM_LEASE_SECONDS = 300;
const MAXIMUM_CLOUDFLARE_ERROR_CODES = 8;

// Cloudflare Email Sending to a Worker-routed destination accepts at most
// 5 MiB, even though Email Routing accepts inbound messages up to 25 MiB. Stay
// a full MiB below the sending boundary so this recurring end-to-end probe
// does not pretend it can exercise the separate inbound provider ceiling.
export const CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES =
  5 * 1024 * 1024;
export const LARGE_PAYLOAD_HEADROOM_BYTES = 1024 * 1024;
export const LARGE_PAYLOAD_TARGET_RAW_BYTES =
  CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES -
  LARGE_PAYLOAD_HEADROOM_BYTES;
export const LARGE_PAYLOAD_MINIMUM_RECEIVED_BYTES = 4 * 1024 * 1024;

// Compatibility exports for existing callers. New code and receipts use the
// truthful large-payload terminology.
export const NEAR_LIMIT_HEADROOM_BYTES = LARGE_PAYLOAD_HEADROOM_BYTES;
export const NEAR_LIMIT_TARGET_RAW_BYTES = LARGE_PAYLOAD_TARGET_RAW_BYTES;
export const NEAR_LIMIT_MINIMUM_RECEIVED_BYTES =
  LARGE_PAYLOAD_MINIMUM_RECEIVED_BYTES;

function required(value, name) {
  const raw = String(value ?? "");
  const normalized = raw.trim();
  if (!normalized || raw !== normalized || normalized.length > 4096 ||
      /[\r\n\0]/.test(normalized)) {
    throw new Error(`${name} is missing or invalid`);
  }
  return normalized;
}

function credential(value, name) {
  const normalized = required(value, name);
  if (/\s/.test(normalized)) throw new Error(`${name} is missing or invalid`);
  return normalized;
}

function emailAddress(value, name) {
  const normalized = required(value, name).toLowerCase();
  if (normalized.length > 320 || normalized.split("@").length !== 2) {
    throw new Error(`${name} is missing or invalid`);
  }
  const [localPart, domain] = normalized.split("@");
  if (localPart.length < 1 || localPart.length > 64 ||
      !SIMPLE_LOCAL_PART.test(localPart) || !SIMPLE_DOMAIN.test(domain) ||
      domain.includes("..")) {
    throw new Error(`${name} is missing or invalid`);
  }
  return normalized;
}

function productionSender(value) {
  const normalized = emailAddress(value, "AGENT_EMAIL_LARGE_PAYLOAD_FROM");
  if (normalized !== PRODUCTION_CANARY_FROM) {
    throw new Error(
      `AGENT_EMAIL_LARGE_PAYLOAD_FROM must be ${PRODUCTION_CANARY_FROM}`,
    );
  }
  return normalized;
}

function productionRecipient(value) {
  const normalized = emailAddress(value, "AGENT_EMAIL_LARGE_PAYLOAD_TO");
  let parsed;
  try {
    parsed = parseRouteAddress(normalized, false);
  } catch {
    throw new Error(
      `AGENT_EMAIL_LARGE_PAYLOAD_TO must be one canonical @${PRODUCTION_DOMAIN} address`,
    );
  }
  if (parsed.domain !== PRODUCTION_DOMAIN ||
      !CANONICAL_REALM_LABEL.test(parsed.realmLabel)) {
    throw new Error(
      `AGENT_EMAIL_LARGE_PAYLOAD_TO must be one canonical @${PRODUCTION_DOMAIN} address`,
    );
  }
  return normalized;
}

function productionCellEndpoint(value) {
  let parsed;
  try {
    parsed = new URL(required(value, "WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT"));
  } catch {
    throw new Error("WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT is missing or invalid");
  }
  if (parsed.protocol !== "https:" || parsed.username || parsed.password ||
      parsed.hash || parsed.search || parsed.port || parsed.pathname !== "/" ||
      !isProductionCellHost(parsed.hostname)) {
    throw new Error(
      "WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT must be the root HTTPS URL of one production cell",
    );
  }
  return parsed.toString().replace(/\/$/, "");
}

function boundedTimeout(value) {
  const raw = String(value ?? "600");
  if (!/^\d+$/.test(raw)) {
    throw new Error("AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS is invalid");
  }
  const seconds = Number(raw);
  if (!Number.isSafeInteger(seconds) || seconds < 60 || seconds > 900) {
    throw new Error(
      "AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS must be between 60 and 900",
    );
  }
  return seconds;
}

function preferredEnvironmentValue(env, preferredName, compatibilityName) {
  return env[preferredName] === undefined
    ? env[compatibilityName]
    : env[preferredName];
}

export function largePayloadProbeConfiguration(env = process.env) {
  const identity = assertProductionCloudflareIdentity(env);
  return {
    accountID: identity.account_id,
    cloudflareToken: env.CLOUDFLARE_API_TOKEN,
    from: productionSender(preferredEnvironmentValue(
      env,
      "AGENT_EMAIL_LARGE_PAYLOAD_FROM",
      "AGENT_EMAIL_NEAR_LIMIT_FROM",
    )),
    to: productionRecipient(preferredEnvironmentValue(
      env,
      "AGENT_EMAIL_LARGE_PAYLOAD_TO",
      "AGENT_EMAIL_NEAR_LIMIT_TO",
    )),
    endpoint: productionCellEndpoint(preferredEnvironmentValue(
      env,
      "WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT",
      "WITSELF_EMAIL_NEAR_LIMIT_ENDPOINT",
    )),
    witselfToken: credential(
      preferredEnvironmentValue(
        env,
        "WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN",
        "WITSELF_EMAIL_NEAR_LIMIT_TOKEN",
      ),
      "WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN",
    ),
    timeoutSeconds: boundedTimeout(preferredEnvironmentValue(
      env,
      "AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS",
      "AGENT_EMAIL_NEAR_LIMIT_TIMEOUT_SECONDS",
    )),
  };
}

// Retained so an older operator wrapper can load this module during rollout.
export function nearLimitProbeConfiguration(env = process.env) {
  return largePayloadProbeConfiguration(env);
}

function syntheticAttachmentBody(bytes, correlationNonce) {
  // A public deterministic AES-CTR stream gives the synthetic body realistic,
  // incompressible storage pressure without incorporating user data or a
  // secret. Map it to 7-bit-safe characters and bounded RFC 5322 lines.
  const key = createHash("sha256")
    .update("witself-large-payload-probe-key-v1\0", "utf8")
    .update(correlationNonce, "ascii")
    .digest();
  const iv = createHash("sha256")
    .update("witself-large-payload-probe-iv-v1\0", "utf8")
    .update(correlationNonce, "ascii")
    .digest()
    .subarray(0, 16);
  const cipher = createCipheriv("aes-256-ctr", key, iv);
  const body = Buffer.allocUnsafe(bytes);
  const zeros = Buffer.alloc(64 * 1024);
  let offset = 0;
  while (offset < body.length) {
    const length = Math.min(zeros.length, body.length - offset);
    const encrypted = cipher.update(zeros.subarray(0, length));
    encrypted.copy(body, offset);
    offset += encrypted.length;
  }
  if (cipher.final().length !== 0 || offset !== body.length) {
    throw new Error("large-payload probe synthetic stream generation failed");
  }
  const alphabet = Buffer.from(
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/",
    "ascii",
  );
  offset = 0;
  while (offset + 76 <= body.length) {
    for (let index = 0; index < 74; index += 1) {
      body[offset + index] = alphabet[body[offset + index] & 63];
    }
    body[offset + 74] = 13;
    body[offset + 75] = 10;
    offset += 76;
  }
  while (offset < body.length) {
    body[offset] = alphabet[body[offset] & 63];
    offset += 1;
  }
  return body.toString("ascii");
}

export function buildLargePayloadProbeMIME(
  config,
  { correlationNonce, now = Date.now() },
) {
  if (!UUID_V4.test(String(correlationNonce ?? ""))) {
    throw new Error("large-payload probe correlation nonce is invalid");
  }
  const instant = new Date(now);
  if (Number.isNaN(instant.getTime())) {
    throw new Error("large-payload probe clock is invalid");
  }
  const boundary = `witself-large-payload-${correlationNonce}`;
  const subject = `Witself large-payload receive probe ${correlationNonce}`;
  const prefix = [
    `From: ${config.from}`,
    `To: ${config.to}`,
    `Date: ${instant.toUTCString()}`,
    `Message-ID: <large-payload-${correlationNonce}@send.witmail.net>`,
    `Subject: ${subject}`,
    "MIME-Version: 1.0",
    "X-Witself-Canary: receive-large-payload-v1",
    `Content-Type: multipart/mixed; boundary="${boundary}"`,
    "",
    `--${boundary}`,
    'Content-Type: text/plain; charset="utf-8"',
    "Content-Transfer-Encoding: 7bit",
    "",
    "Synthetic large-payload production receive probe. No user content.",
    `--${boundary}`,
    'Content-Type: application/octet-stream; name="large-payload-probe.bin"',
    "Content-Transfer-Encoding: 7bit",
    'Content-Disposition: attachment; filename="large-payload-probe.bin"',
    "",
  ].join("\r\n") + "\r\n";
  const suffix = `\r\n--${boundary}--\r\n`;
  const attachmentBytes = LARGE_PAYLOAD_TARGET_RAW_BYTES -
    Buffer.byteLength(prefix, "utf8") - Buffer.byteLength(suffix, "utf8");
  if (!Number.isSafeInteger(attachmentBytes) || attachmentBytes < 1) {
    throw new Error("large-payload probe MIME framing exceeded its target");
  }
  const mimeMessage = prefix +
    syntheticAttachmentBody(attachmentBytes, correlationNonce) + suffix;
  const rawBytes = Buffer.byteLength(mimeMessage, "utf8");
  if (rawBytes !== LARGE_PAYLOAD_TARGET_RAW_BYTES ||
      rawBytes >= CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES) {
    throw new Error("large-payload probe MIME size missed its reviewed target");
  }
  return { mimeMessage, subject, rawBytes, attachmentBytes };
}

// Compatibility export for the prior module API.
export function buildNearLimitProbeMIME(config, options) {
  return buildLargePayloadProbeMIME(config, options);
}

function withAbsoluteDeadline(fetchAPI, deadlineAt, now) {
  return async function boundedFetch(url, init = {}) {
    const remainingMS = Math.ceil(deadlineAt - now());
    if (!Number.isSafeInteger(remainingMS) || remainingMS <= 0) {
      throw new Error("large-payload receive probe deadline exceeded");
    }
    const deadlineSignal = AbortSignal.timeout(remainingMS);
    const signal = init.signal
      ? AbortSignal.any([init.signal, deadlineSignal])
      : deadlineSignal;
    try {
      return await fetchAPI(url, { ...init, signal });
    } catch {
      throw new Error("large-payload receive probe request failed");
    }
  };
}

function cloudflareErrorCodeSuffix(value) {
  if (!Array.isArray(value?.errors)) return "";
  const codes = [];
  for (const error of value.errors.slice(0, MAXIMUM_CLOUDFLARE_ERROR_CODES)) {
    const code = error?.code;
    if (Number.isSafeInteger(code) && code >= 0 && code <= 999_999_999 &&
        !codes.includes(code)) {
      codes.push(code);
    }
  }
  return codes.length === 0
    ? ""
    : ` (Cloudflare error codes: ${codes.join(",")})`;
}

async function responseJSON(response, operation, { cloudflare = false } = {}) {
  let value;
  try {
    value = await response.json();
  } catch {
    throw new Error(`${operation} returned malformed JSON`);
  }
  if (!response.ok || !value || typeof value !== "object" ||
      Array.isArray(value)) {
    const suffix = cloudflare ? cloudflareErrorCodeSuffix(value) : "";
    throw new Error(`${operation} failed with status ${response.status}${suffix}`);
  }
  return value;
}

function witselfClient(config, fetchAPI) {
  return async function request(path, {
    method = "GET",
    body,
    idempotencyKey,
  } = {}) {
    const headers = {
      Authorization: `Bearer ${config.witselfToken}`,
      Accept: "application/json",
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    const response = await fetchAPI(`${config.endpoint}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: "error",
    });
    return responseJSON(response, "Witself large-payload probe request");
  };
}

function assertStoragePreflight(value) {
  if (value?.maximum_raw_bytes !== RELAY_MAXIMUM_RAW_BYTES) {
    throw new Error(
      "large-payload probe requires the account raw-message limit at the reviewed 25 MiB service ceiling",
    );
  }
  const capacity = value.attachment_capacity;
  if (!capacity || typeof capacity !== "object" || Array.isArray(capacity) ||
      typeof capacity.unlimited !== "boolean" ||
      typeof capacity.over_limit !== "boolean" || capacity.over_limit ||
      (!capacity.unlimited &&
        (!Number.isSafeInteger(capacity.remaining) ||
          capacity.remaining < RELAY_MAXIMUM_RAW_BYTES))) {
    throw new Error(
      "large-payload probe requires enough attachment capacity for one service-ceiling message",
    );
  }
}

function acceptedBySendingAPI(result) {
  if ([result?.delivered, result?.queued].some((items) =>
    Array.isArray(items) && items.length > 0)) return true;
  return typeof result?.message_id === "string" &&
    result.message_id.trim() !== "" && !/[\r\n\0]/.test(result.message_id);
}

function sendingResult(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Cloudflare raw email submission returned an invalid result");
  }
  for (const name of ["delivered", "queued", "permanent_bounces"]) {
    const addresses = value[name];
    if (!Array.isArray(addresses) || addresses.length > 100 ||
        addresses.some((address) => typeof address !== "string" ||
          address.length > 320 || /[\r\n\0]/.test(address))) {
      throw new Error("Cloudflare raw email submission returned an invalid result");
    }
  }
  if (value.message_id !== undefined &&
      (typeof value.message_id !== "string" || value.message_id.length > 4096 ||
        /[\r\n\0]/.test(value.message_id))) {
    throw new Error("Cloudflare raw email submission returned an invalid result");
  }
  return value;
}

async function submitRawEmail(config, message, fetchAPI) {
  const requestBody = JSON.stringify({
    from: config.from,
    recipients: [config.to],
    mime_message: message.mimeMessage,
  });
  // Cloudflare documents the mail-size limit rather than whether JSON escape
  // overhead is counted. Keep the complete REST request body under the same
  // 5 MiB bound as an additional fail-closed safety margin.
  if (Buffer.byteLength(requestBody, "utf8") >=
      CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES) {
    throw new Error(
      "large-payload probe request framing exceeded the sending safety margin",
    );
  }
  const response = await fetchAPI(
    `${CLOUDFLARE_API_ROOT}/accounts/${config.accountID}/email/sending/send_raw`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${config.cloudflareToken}`,
        "Content-Type": "application/json",
      },
      body: requestBody,
      redirect: "error",
    },
  );
  const envelope = await responseJSON(
    response,
    "Cloudflare raw email submission",
    { cloudflare: true },
  );
  if (envelope.success !== true) {
    throw new Error(
      `Cloudflare raw email submission was rejected${cloudflareErrorCodeSuffix(envelope)}`,
    );
  }
  const result = sendingResult(envelope.result);
  if (result.permanent_bounces.length > 0) {
    throw new Error("Cloudflare permanently bounced the large-payload probe");
  }
  if (!acceptedBySendingAPI(result)) {
    throw new Error("Cloudflare did not confirm the large-payload probe submission");
  }
}

function emailPage(value) {
  if (!Array.isArray(value?.messages) ||
      value.messages.length > DISCOVERY_PAGE_SIZE) {
    throw new Error("large-payload probe email list response was invalid");
  }
  for (const message of value.messages) {
    if (!message || typeof message !== "object" || Array.isArray(message) ||
        !MESSAGE_ID.test(String(message.id ?? "")) ||
        (message.subject !== undefined && typeof message.subject !== "string")) {
      throw new Error("large-payload probe email list response was invalid");
    }
  }
  const cursor = value.next_cursor;
  if (cursor === undefined || cursor === null || cursor === "") {
    return { messages: value.messages, nextCursor: "" };
  }
  if (typeof cursor !== "string" || cursor.length > MAXIMUM_CURSOR_BYTES ||
      /[\u0000-\u001f\u007f]/.test(cursor)) {
    throw new Error("large-payload probe email list cursor was invalid");
  }
  return { messages: value.messages, nextCursor: cursor };
}

function emailListPath(cursor = "", unacked = true) {
  const query = new URLSearchParams({
    oldest_first: "false",
    limit: String(DISCOVERY_PAGE_SIZE),
  });
  if (unacked) query.set("unacked", "true");
  if (cursor) query.set("cursor", cursor);
  return `/v1/email?${query.toString()}`;
}

async function findMessageOnce(request, subject, unacked) {
  let cursor = "";
  const seen = new Set();
  for (let pageNumber = 0;
    pageNumber < MAXIMUM_DISCOVERY_PAGES;
    pageNumber += 1) {
    const page = emailPage(await request(emailListPath(cursor, unacked)));
    const message = page.messages.find((candidate) =>
      candidate.subject === subject);
    if (message) return message;
    if (!page.nextCursor) return undefined;
    if (seen.has(page.nextCursor)) {
      throw new Error("large-payload probe email list cursor repeated");
    }
    seen.add(page.nextCursor);
    cursor = page.nextCursor;
  }
  throw new Error("large-payload probe email list exceeded the safe page limit");
}

async function deadlineSleep(milliseconds, deadlineAt, now, sleep) {
  const remaining = deadlineAt - now();
  if (!Number.isSafeInteger(remaining) || remaining <= 0) {
    throw new Error("large-payload receive probe deadline exceeded");
  }
  await sleep(Math.min(milliseconds, remaining));
}

async function discoverMessage(request, subject, deadlineAt, now, sleep) {
  for (;;) {
    const message = await findMessageOnce(request, subject, true);
    if (message) return message;
    await deadlineSleep(DISCOVERY_RETRY_MS, deadlineAt, now, sleep);
  }
}

function assertRetainedLargePayloadMessage(message, config, subject) {
  if (!message || message.subject !== subject ||
      message.provider !== "cloudflare_email_routing" ||
      message.envelope_sender !== config.from ||
      message.envelope_recipient !== config.to ||
      message.recipient_route_kind !== "canonical" ||
      String(message.subaddress_tag ?? "") !== "" ||
      message.parse_state !== "parsed" || message.attachment_count !== 1 ||
      !Number.isSafeInteger(message.raw_size_bytes) ||
      message.raw_size_bytes < LARGE_PAYLOAD_MINIMUM_RECEIVED_BYTES ||
      message.raw_size_bytes > RELAY_MAXIMUM_RAW_BYTES ||
      message.attachment_storage_bytes !== message.raw_size_bytes ||
      message.retained_attachment_storage_bytes !== message.raw_size_bytes ||
      message.payload_retention_state !== "retained") {
    throw new Error(
      "large-payload probe did not produce one canonical durably retained message",
    );
  }
  return message.raw_size_bytes;
}

function processingClaim(value) {
  const processing = value?.processing;
  if (!processing || processing.state !== "claimed" ||
      !CLAIM_ID.test(String(processing.claim_id ?? "")) ||
      !Number.isSafeInteger(processing.generation) || processing.generation < 1) {
    throw new Error("large-payload probe claim response was invalid");
  }
  return processing;
}

function freshUUID(uuid, operation) {
  const value = uuid();
  if (!UUID_V4.test(String(value ?? ""))) {
    throw new Error(`large-payload probe ${operation} UUID was invalid`);
  }
  return value;
}

export async function runLargePayloadProbePreflight(config, runtime = {}) {
  const now = runtime.now ?? Date.now;
  const startedAt = now();
  const deadlineAt = startedAt + config.timeoutSeconds * 1000;
  const fetchAPI = withAbsoluteDeadline(runtime.fetch ?? fetch, deadlineAt, now);
  const request = witselfClient(config, fetchAPI);
  assertStoragePreflight(await request("/v1/email:status"));
  return {
    schema: "witself.agent-email.large-payload-probe-preflight.v1",
    outcome: "ready",
    service_inbound_ceiling_bytes: RELAY_MAXIMUM_RAW_BYTES,
    worker_route_sending_maximum_raw_bytes:
      CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
    target_raw_bytes: LARGE_PAYLOAD_TARGET_RAW_BYTES,
    live_provider_inbound_ceiling_certified: false,
    attachment_capacity_sufficient: true,
    provider_mutation_performed: false,
    payload_allocated: false,
    addresses_returned: false,
    tokens_returned: false,
  };
}

export async function runLargePayloadProbe(config, runtime = {}) {
  const now = runtime.now ?? Date.now;
  const sleep = runtime.sleep ?? ((milliseconds) =>
    new Promise((resolve) => setTimeout(resolve, milliseconds)));
  const uuid = runtime.randomUUID ?? randomUUID;
  const startedAt = now();
  const deadlineAt = startedAt + config.timeoutSeconds * 1000;
  const fetchAPI = withAbsoluteDeadline(runtime.fetch ?? fetch, deadlineAt, now);
  const request = witselfClient(config, fetchAPI);

  assertStoragePreflight(await request("/v1/email:status"));
  const correlationNonce = freshUUID(uuid, "correlation");
  const message = buildLargePayloadProbeMIME(config, {
    correlationNonce,
    now: now(),
  });
  await submitRawEmail(config, message, fetchAPI);

  const received = await discoverMessage(
    request,
    message.subject,
    deadlineAt,
    now,
    sleep,
  );
  const receivedRawBytes = assertRetainedLargePayloadMessage(
    received,
    config,
    message.subject,
  );

  let claim;
  let completed = false;
  try {
    claim = processingClaim(await request(
      `/v1/email/${encodeURIComponent(received.id)}:claim`,
      {
        method: "POST",
        body: { lease_seconds: CLAIM_LEASE_SECONDS },
        idempotencyKey: freshUUID(uuid, "claim"),
      },
    ));
    const completion = await request(
      `/v1/email/${encodeURIComponent(received.id)}:complete`,
      {
        method: "POST",
        body: {
          claim_id: claim.claim_id,
          generation: claim.generation,
        },
        idempotencyKey: freshUUID(uuid, "completion"),
      },
    );
    if (completion?.processing?.state !== "completed" ||
        completion.processing.claim_id !== claim.claim_id ||
        completion.processing.generation !== claim.generation) {
      throw new Error("large-payload probe completion response was invalid");
    }
    completed = true;

    const acknowledged = await request(
      `/v1/email/${encodeURIComponent(received.id)}:ack`,
      { method: "POST", body: {} },
    );
    if (acknowledged?.message?.id !== received.id ||
        acknowledged.message.read_state?.state !== "acked" ||
        acknowledged.message.processing?.state !== "completed") {
      throw new Error("large-payload probe acknowledgement response was invalid");
    }
    assertRetainedLargePayloadMessage(
      acknowledged.message,
      config,
      message.subject,
    );
  } catch (error) {
    if (claim?.claim_id && Number.isSafeInteger(claim.generation) && !completed) {
      try {
        await request(`/v1/email/${encodeURIComponent(received.id)}:release`, {
          method: "POST",
          body: {
            claim_id: claim.claim_id,
            generation: claim.generation,
          },
        });
      } catch {
        // Preserve the original error. The exact bounded lease expires.
      }
    }
    throw error;
  }

  const afterCleanup = await findMessageOnce(
    request,
    message.subject,
    false,
  );
  if (!afterCleanup || afterCleanup.id !== received.id ||
      afterCleanup.read_state?.state !== "acked" ||
      afterCleanup.processing?.state !== "completed") {
    throw new Error(
      "large-payload probe was not durable after mailbox acknowledgement",
    );
  }
  assertRetainedLargePayloadMessage(afterCleanup, config, message.subject);

  return {
    schema: "witself.agent-email.large-payload-probe.v1",
    outcome: "passed",
    service_inbound_ceiling_bytes: RELAY_MAXIMUM_RAW_BYTES,
    worker_route_sending_maximum_raw_bytes:
      CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
    live_provider_inbound_ceiling_certified: false,
    local_inbound_ceiling_tests_required: true,
    submitted_raw_bytes: message.rawBytes,
    received_raw_bytes: receivedRawBytes,
    retained_attachment_storage_bytes: receivedRawBytes,
    provider_submission_confirmed: true,
    durable_storage_verified: true,
    mailbox_processing_completed: true,
    mailbox_acknowledged: true,
    durable_after_acknowledgement: true,
    time_based_retention_deletion_tested: false,
    payload_returned: false,
    addresses_returned: false,
    identifiers_returned: false,
    tokens_returned: false,
    elapsed_ms: Math.max(0, now() - startedAt),
  };
}

// Compatibility exports for the prior module API and npm command.
export function runNearLimitProbePreflight(config, runtime = {}) {
  return runLargePayloadProbePreflight(config, runtime);
}

export function runNearLimitProbe(config, runtime = {}) {
  return runLargePayloadProbe(config, runtime);
}

async function main(args = process.argv.slice(2)) {
  if (args.length > 1 || (args.length === 1 && args[0] !== "--preflight")) {
    throw new Error("usage: large-payload-probe [--preflight]");
  }
  const config = largePayloadProbeConfiguration();
  return args[0] === "--preflight"
    ? runLargePayloadProbePreflight(config)
    : runLargePayloadProbe(config);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main()
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`agent-email large-payload probe: ${error.message}\n`);
      process.exitCode = 1;
    });
}
