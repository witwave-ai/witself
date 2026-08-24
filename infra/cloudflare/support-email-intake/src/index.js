import { extractAuthenticationVerdicts } from "./authenticity.mjs";
import {
  decideIntake,
  extractTicketTag,
  messageIDFromHeaders,
  SUPPORT_EMAIL_MAX_RAW_BYTES,
  visibleSender,
} from "./intake.mjs";
import { extractMimeText } from "./mime-text.mjs";

const TRANSIENT_ERROR = "support email intake temporarily unavailable";
const OVER_SIZE_REJECTION = "message too large";
const GLOBAL_LIMITER_KEY = "support-email-intake-v1";
const MAX_CONTROL_PLANE_TOKEN_BYTES = 8_192;
const NON_WHITESPACE = /\P{White_Space}/u;
const textEncoder = new TextEncoder();

function transient() {
  return new Error(TRANSIENT_ERROR);
}

async function sha256Hex(value, cryptoAPI) {
  if (!cryptoAPI?.subtle || typeof cryptoAPI.subtle.digest !== "function") {
    throw transient();
  }
  let digest;
  try {
    digest = await cryptoAPI.subtle.digest("SHA-256", textEncoder.encode(value));
  } catch {
    throw transient();
  }
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

async function limiterAdmission(binding, key) {
  if (!binding || typeof binding.limit !== "function") throw transient();
  try {
    const admission = await binding.limit({ key });
    if (!admission || typeof admission.success !== "boolean") throw transient();
    return admission.success;
  } catch {
    throw transient();
  }
}

async function applyRateLimits(message, env, cryptoAPI) {
  const sender = visibleSender(message?.headers) ?? "invalid";
  const senderKey = await sha256Hex(sender, cryptoAPI);
  const senderAllowed = await limiterAdmission(
    env?.SUPPORT_EMAIL_SENDER_LIMITER,
    senderKey,
  );
  if (!senderAllowed) return false;
  const globalAllowed = await limiterAdmission(
    env?.SUPPORT_EMAIL_GLOBAL_LIMITER,
    GLOBAL_LIMITER_KEY,
  );
  if (!globalAllowed) throw transient();
  return true;
}

async function readRawMessage(stream, maximumBytes) {
  if (stream instanceof Uint8Array) {
    return stream.byteLength <= maximumBytes ? stream : null;
  }
  if (stream instanceof ArrayBuffer) {
    const bytes = new Uint8Array(stream);
    return bytes.byteLength <= maximumBytes ? bytes : null;
  }
  if (!stream || typeof stream.getReader !== "function") throw transient();
  const reader = stream.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      const bytes = value instanceof Uint8Array ? value : new Uint8Array(value);
      total += bytes.byteLength;
      if (total > maximumBytes) {
        await reader.cancel().catch(() => {});
        return null;
      }
      chunks.push(bytes);
    }
  } catch {
    throw transient();
  } finally {
    reader.releaseLock();
  }
  const output = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    output.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return output;
}

function controlPlaneRequest(env, payload) {
  const rawURL = String(env?.CONTROL_PLANE_URL ?? "");
  const token = String(env?.CONTROL_PLANE_SUPPORT_INTAKE_TOKEN ?? "");
  let base;
  try {
    base = new URL(rawURL);
  } catch {
    throw transient();
  }
  if (
    rawURL !== rawURL.trim() || base.protocol !== "https:" || base.username ||
    base.password || base.search || base.hash || !base.hostname ||
    base.hostname === "localhost" ||
    (base.pathname !== "/" && base.pathname !== "") ||
    token.length < 16 || token.length > MAX_CONTROL_PLANE_TOKEN_BYTES ||
    token !== token.trim() || /[\s\0-\x1f\x7f]/u.test(token)
  ) {
    throw transient();
  }
  return new Request(new URL("/v1/intake/support-email", base), {
    method: "POST",
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
    redirect: "manual",
  });
}

function rejectOverSize(message) {
  if (!message || typeof message.setReject !== "function") throw transient();
  message.setReject(OVER_SIZE_REJECTION);
  return Object.freeze({ action: "reject_size", reason: "reject_size" });
}

// handleEmail accepts an injected fetch/crypto runtime so all egress and
// rate-limit behavior can be tested without a live Cloudflare Worker.
export async function handleEmail(message, env, runtime = {}) {
  // Dark means dark: with the gate off, return before the rate limiters or
  // any other binding can act, so a disabled deployment produces no rejects,
  // no tempfails, and no limiter consumption whatsoever.
  if (env?.SUPPORT_EMAIL_INTAKE_ENABLED !== "true") {
    return Object.freeze({ action: "drop", reason: "drop_gate" });
  }
  const cryptoAPI = runtime.crypto ?? globalThis.crypto;
  if (!await applyRateLimits(message, env, cryptoAPI)) {
    return Object.freeze({ action: "drop", reason: "drop_sender_rate" });
  }

  const preliminary = decideIntake({
    headers: message?.headers,
    from: message?.from,
    to: message?.to,
    size: message?.rawSize,
    verdicts: { dmarc: "pass" },
    config: env,
  });
  if (preliminary.action === "reject_size") return rejectOverSize(message);
  if (preliminary.action !== "forward") return preliminary;

  const raw = await readRawMessage(message?.raw, SUPPORT_EMAIL_MAX_RAW_BYTES);
  if (raw === null) return rejectOverSize(message);
  if (raw.byteLength !== message.rawSize) throw transient();
  const verdicts = extractAuthenticationVerdicts(
    raw,
    env.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID,
  );
  const decision = decideIntake({
    headers: message.headers,
    from: message.from,
    to: message.to,
    size: raw.byteLength,
    verdicts,
    config: env,
  });
  if (decision.action !== "forward") return decision;

  const mime = extractMimeText(raw);
  if (mime === null) {
    return Object.freeze({ action: "drop", reason: "drop_html_only" });
  }
  if (!NON_WHITESPACE.test(mime.subject) || !NON_WHITESPACE.test(mime.body)) {
    return Object.freeze({ action: "drop", reason: "drop_invalid_content" });
  }
  const sender = visibleSender(message.headers);
  const messageID = messageIDFromHeaders(message.headers);
  if (sender === null || messageID === null) {
    return Object.freeze({ action: "drop", reason: "drop_message_id" });
  }
  const ticketTag = extractTicketTag(mime.subject);
  const payload = {
    sender,
    subject: mime.subject,
    body: mime.body,
    message_id: messageID,
    ...(ticketTag === null ? {} : { ticket_tag: ticketTag }),
  };
  const request = controlPlaneRequest(env, payload);
  const fetchAPI = runtime.fetch ?? globalThis.fetch;
  if (typeof fetchAPI !== "function") throw transient();
  let response;
  try {
    response = await fetchAPI(request);
  } catch {
    throw transient();
  }
  if (!response || response.status < 200 || response.status >= 300) {
    await response?.body?.cancel?.().catch(() => {});
    throw transient();
  }
  await response.body?.cancel?.().catch(() => {});
  return decision;
}

export default {
  async email(message, env) {
    await handleEmail(message, env);
  },
};
