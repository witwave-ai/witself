#!/usr/bin/env node
import { randomInt, randomUUID } from "node:crypto";

import { CloudflareAPI } from "./cloudflare.mjs";

const MESSAGE_ID = /^emsg_[a-z2-7]{16}$/;
const CLAIM_ID = /^ecl_[a-z2-7]{16}$/;
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const EMAIL_DISCOVERY_PAGE_SIZE = 100;
const MAX_EMAIL_DISCOVERY_PAGES = 100;
const EMAIL_DISCOVERY_RETRY_MS = 1000;
const MAX_EMAIL_CURSOR_BYTES = 4096;

function required(value, name) {
  const normalized = String(value ?? "").trim();
  if (!normalized || /[\r\n\0]/.test(normalized)) throw new Error(`${name} is missing or invalid`);
  return normalized;
}

function emailAddress(value, name) {
  const normalized = required(value, name).toLowerCase();
  if (normalized.length > 320 || normalized.split("@").length !== 2) {
    throw new Error(`${name} is missing or invalid`);
  }
  return normalized;
}

function endpoint(value) {
  let parsed;
  try {
    parsed = new URL(required(value, "WITSELF_EMAIL_CANARY_ENDPOINT"));
  } catch {
    throw new Error("WITSELF_EMAIL_CANARY_ENDPOINT is missing or invalid");
  }
  if (parsed.protocol !== "https:" || parsed.username || parsed.password || parsed.hash || parsed.search) {
    throw new Error("WITSELF_EMAIL_CANARY_ENDPOINT must be a credential-free HTTPS URL");
  }
  return parsed.toString().replace(/\/$/, "");
}

function withAbsoluteDeadline(fetchAPI, deadlineAt, now) {
  return async function boundedFetch(url, init = {}) {
    const remainingMS = Math.ceil(deadlineAt - now());
    if (!Number.isSafeInteger(remainingMS) || remainingMS <= 0) {
      throw new Error("agent-email canary deadline exceeded");
    }
    const deadlineSignal = AbortSignal.timeout(remainingMS);
    const signal = init.signal
      ? AbortSignal.any([init.signal, deadlineSignal])
      : deadlineSignal;
    return fetchAPI(url, { ...init, signal });
  };
}

export function canaryConfiguration(env = process.env) {
  const accountID = required(env.CLOUDFLARE_ACCOUNT_ID, "CLOUDFLARE_ACCOUNT_ID");
  if (!/^[0-9a-f]{32}$/.test(accountID)) throw new Error("CLOUDFLARE_ACCOUNT_ID is missing or invalid");
  const rawTimeout = String(env.AGENT_EMAIL_CANARY_TIMEOUT_SECONDS ?? "180");
  if (!/^\d+$/.test(rawTimeout)) throw new Error("AGENT_EMAIL_CANARY_TIMEOUT_SECONDS is invalid");
  const timeoutSeconds = Number(rawTimeout);
  if (!Number.isSafeInteger(timeoutSeconds) || timeoutSeconds < 20 || timeoutSeconds > 600) {
    throw new Error("AGENT_EMAIL_CANARY_TIMEOUT_SECONDS must be between 20 and 600");
  }
  return {
    accountID,
    cloudflareToken: required(env.CLOUDFLARE_API_TOKEN, "CLOUDFLARE_API_TOKEN"),
    from: emailAddress(env.AGENT_EMAIL_CANARY_FROM, "AGENT_EMAIL_CANARY_FROM"),
    to: emailAddress(env.AGENT_EMAIL_CANARY_TO, "AGENT_EMAIL_CANARY_TO"),
    endpoint: endpoint(env.WITSELF_EMAIL_CANARY_ENDPOINT),
    witselfToken: required(env.WITSELF_EMAIL_CANARY_TOKEN, "WITSELF_EMAIL_CANARY_TOKEN"),
    timeoutSeconds,
  };
}

async function responseJSON(response, operation) {
  let value;
  try {
    value = await response.json();
  } catch {
    throw new Error(`${operation} returned malformed JSON`);
  }
  if (!response.ok || !value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${operation} failed with status ${response.status}`);
  }
  return value;
}

function witselfClient(config, fetchAPI) {
  return async function request(path, { method = "GET", body, idempotencyKey } = {}) {
    const headers = {
      Authorization: `Bearer ${config.witselfToken}`,
      Accept: "application/json",
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    let response;
    try {
      response = await fetchAPI(`${config.endpoint}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        redirect: "error",
      });
    } catch {
      throw new Error("Witself canary request failed");
    }
    return responseJSON(response, "Witself canary request");
  };
}

function permanentlyBouncedBySendingAPI(result) {
  // This canary submits exactly one envelope recipient, so any explicit
  // permanent bounce rejects the submission even if the provider normalized
  // the returned address differently.
  return Array.isArray(result?.permanent_bounces) && result.permanent_bounces.length > 0;
}

function acceptedBySendingAPI(result) {
  if ([result?.delivered, result?.queued].some((values) =>
    Array.isArray(values) && values.length > 0)) return true;
  const messageID = result?.message_id;
  return typeof messageID === "string" && messageID.trim() !== "" && !/[\r\n\0]/.test(messageID);
}

function checkpoint(value, expectedState = "") {
  const candidate = value?.checkpoint;
  if (!candidate || typeof candidate !== "object" || Array.isArray(candidate) ||
      typeof candidate.state !== "string" || typeof candidate.armed !== "boolean" ||
      typeof candidate.tempfailed !== "boolean" || typeof candidate.accepted !== "boolean" ||
      !Number.isSafeInteger(candidate.tempfail_count) || candidate.tempfail_count < 0 ||
      candidate.tempfail_count > 1 || !candidate.armed ||
      candidate.tempfailed !== (candidate.tempfail_count === 1) ||
      (candidate.accepted && !candidate.tempfailed) ||
      (expectedState && candidate.state !== expectedState)) {
    throw new Error("synthetic retry canary checkpoint was invalid");
  }
  return candidate;
}

function emailListPage(value) {
  if (!Array.isArray(value?.messages) || value.messages.length > EMAIL_DISCOVERY_PAGE_SIZE) {
    throw new Error("synthetic canary email list response was invalid");
  }
  for (const message of value.messages) {
    if (!message || typeof message !== "object" || Array.isArray(message) ||
        !MESSAGE_ID.test(String(message.id ?? "")) ||
        (message.subject !== undefined && typeof message.subject !== "string")) {
      throw new Error("synthetic canary email list response was invalid");
    }
  }
  const rawCursor = value.next_cursor;
  if (rawCursor === undefined || rawCursor === null || rawCursor === "") {
    return { messages: value.messages, nextCursor: "" };
  }
  if (typeof rawCursor !== "string" || rawCursor.length > MAX_EMAIL_CURSOR_BYTES ||
      /[\u0000-\u001f\u007f]/.test(rawCursor)) {
    throw new Error("synthetic canary email list cursor was invalid");
  }
  return { messages: value.messages, nextCursor: rawCursor };
}

function emailListPath(cursor = "") {
  const query = new URLSearchParams({
    unacked: "true",
    oldest_first: "false",
    limit: String(EMAIL_DISCOVERY_PAGE_SIZE),
  });
  if (cursor) query.set("cursor", cursor);
  return `/v1/email?${query.toString()}`;
}

async function discoverCanaryMessage(request, subject, deadlineAt, now, sleep, timeoutSeconds) {
  const attempts = Math.max(1, Math.ceil((timeoutSeconds * 1000) / EMAIL_DISCOVERY_RETRY_MS));
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    let cursor = "";
    const seenCursors = new Set();
    let exhaustedPageBudget = true;
    for (let pageNumber = 0; pageNumber < MAX_EMAIL_DISCOVERY_PAGES; pageNumber += 1) {
      const page = emailListPage(await request(emailListPath(cursor)));
      const message = page.messages.find((candidate) => candidate.subject === subject);
      if (message) return message;
      if (!page.nextCursor) {
        exhaustedPageBudget = false;
        break;
      }
      if (seenCursors.has(page.nextCursor)) {
        throw new Error("synthetic canary email list cursor repeated");
      }
      seenCursors.add(page.nextCursor);
      cursor = page.nextCursor;
    }
    if (exhaustedPageBudget) {
      throw new Error("synthetic canary email list exceeded the safe page limit");
    }
    if (attempt + 1 < attempts) {
      await deadlineSleep(EMAIL_DISCOVERY_RETRY_MS, deadlineAt, now, sleep);
    }
  }
  return undefined;
}

async function deadlineSleep(milliseconds, deadlineAt, now, sleep) {
  const remaining = deadlineAt - now();
  if (!Number.isSafeInteger(remaining) || remaining <= 0) {
    throw new Error("agent-email canary deadline exceeded");
  }
  await sleep(Math.min(milliseconds, remaining));
}

export async function runCanary(config, runtime = {}) {
  const rawFetchAPI = runtime.fetch ?? fetch;
  const uuid = runtime.randomUUID ?? randomUUID;
  const now = runtime.now ?? Date.now;
  const sleep = runtime.sleep ?? ((milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)));
  const code = String((runtime.randomCode ?? (() => randomInt(100000, 1000000)))());
  if (!/^\d{6}$/.test(code)) throw new Error("canary code generator returned an invalid value");
  const retryChallenge = uuid();
  const correlationNonce = uuid();
  if (!UUID_V4.test(retryChallenge) || !UUID_V4.test(correlationNonce) ||
      retryChallenge === correlationNonce) {
    throw new Error("canary UUID generator returned an invalid value");
  }
  const subject = `Witself receive canary ${correlationNonce}`;
  const startedAt = now();
  const fetchAPI = withAbsoluteDeadline(
    rawFetchAPI,
    startedAt + config.timeoutSeconds * 1000,
    now,
  );
  const request = witselfClient(config, fetchAPI);
  const armed = checkpoint(await request("/v1/email/retry-canary:arm", {
    method: "POST",
    body: { challenge: retryChallenge },
  }), "armed");
  if (armed.tempfailed || armed.accepted || armed.tempfail_count !== 0) {
    throw new Error("synthetic retry canary arm was not fresh");
  }
  const cf = new CloudflareAPI({
    accountID: config.accountID,
    apiToken: config.cloudflareToken,
    fetchAPI,
  });
  const submitted = await cf.sendEmail({
    from: config.from,
    to: config.to,
    subject,
    text: `Synthetic monitoring message. Verification code: ${code}.`,
    headers: {
      "X-Witself-Canary": "receive-pilot-v1",
      "X-Witself-Canary-Retry": retryChallenge,
    },
  });
  if (permanentlyBouncedBySendingAPI(submitted)) {
    throw new Error("Cloudflare permanently bounced the synthetic canary");
  }
  if (!acceptedBySendingAPI(submitted)) {
    throw new Error("Cloudflare did not confirm the synthetic canary submission");
  }
  const deadlineAt = startedAt + config.timeoutSeconds * 1000;
  let sawTemporary = false;
  for (;;) {
    const proof = checkpoint(await request("/v1/email/retry-canary:status", {
      method: "POST",
      body: { challenge: retryChallenge },
    }));
    if (proof.tempfailed && proof.tempfail_count === 1) sawTemporary = true;
    if (proof.accepted) {
      if (!sawTemporary || proof.state !== "accepted") {
        throw new Error("synthetic retry canary accepted without temporary proof");
      }
      break;
    }
    if (proof.state !== "armed" && proof.state !== "tempfailed") {
      throw new Error("synthetic retry canary entered an invalid state");
    }
    await deadlineSleep(1000, deadlineAt, now, sleep);
  }

  const message = await discoverCanaryMessage(
    request,
    subject,
    deadlineAt,
    now,
    sleep,
    config.timeoutSeconds,
  );
  if (!message || !MESSAGE_ID.test(String(message.id ?? ""))) {
    throw new Error("synthetic canary did not arrive before the bounded deadline");
  }

  let claim;
  try {
    const claimed = await request(`/v1/email/${encodeURIComponent(message.id)}:claim`, {
      method: "POST",
      body: { lease_seconds: 300 },
      idempotencyKey: uuid(),
    });
    claim = claimed.processing;
    if (!claim || claim.state !== "claimed" || !CLAIM_ID.test(String(claim.claim_id ?? "")) ||
        !Number.isSafeInteger(claim.generation) || claim.generation < 1) {
      throw new Error("synthetic canary claim response was invalid");
    }

    const read = await request(`/v1/email/${encodeURIComponent(message.id)}:read`, {
      method: "POST", body: {},
    });
    if (read?.message?.parse_state !== "parsed" || read.message.subject !== subject ||
        typeof read.message.text !== "string" || !read.message.text.includes(`Verification code: ${code}`)) {
      throw new Error("synthetic canary content did not round-trip through MIME parsing");
    }

    await request(`/v1/email/${encodeURIComponent(message.id)}:code-consumed`, {
      method: "POST", body: {},
    });
    await request(`/v1/email/${encodeURIComponent(message.id)}:complete`, {
      method: "POST",
      body: { claim_id: claim.claim_id, generation: claim.generation },
      idempotencyKey: uuid(),
    });
    await request(`/v1/email/${encodeURIComponent(message.id)}:ack`, {
      method: "POST", body: {},
    });
  } catch (error) {
    if (claim?.claim_id && Number.isSafeInteger(claim.generation)) {
      try {
        await request(`/v1/email/${encodeURIComponent(message.id)}:release`, {
          method: "POST",
          body: { claim_id: claim.claim_id, generation: claim.generation },
        });
      } catch {
        // Preserve the original failure. The exact fence expires server-side.
      }
    }
    throw error;
  }

  const completedAt = now();
  return {
    schema: "witself.agent-email.canary.v1",
    outcome: "passed",
    elapsed_ms: Math.max(0, completedAt - startedAt),
    code_value_returned: false,
    content_returned: false,
    identifiers_returned: false,
    provider_retry_proven: true,
    completed: true,
    acknowledged: true,
  };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  runCanary(canaryConfiguration())
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`agent-email canary: ${error.message}\n`);
      process.exitCode = 1;
    });
}
