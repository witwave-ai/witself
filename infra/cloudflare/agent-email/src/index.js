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
  recipientKey,
  validateRuntimeConfig,
  validateRuntimeRecipient,
} from "./directory.mjs";

const PERMANENT_REJECTION = "recipient unavailable";
const TRANSIENT_ERROR = "agent email relay temporarily unavailable";
const DEFAULT_TIMEOUT_MS = 20_000;
const MAX_VERDICT_BYTES = 4_096;

let cachedSecret = "";
let cachedSigningKey;

function transient() {
  return new Error(TRANSIENT_ERROR);
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
    throw transient();
  }
}

async function signingKey(env, cryptoAPI) {
  const secret = String(env.RELAY_ED25519_PRIVATE_KEY ?? "");
  if (!cachedSigningKey || secret !== cachedSecret) {
    cachedSecret = secret;
    cachedSigningKey = importSigningKey(secret, cryptoAPI).catch(() => {
      cachedSigningKey = undefined;
      throw transient();
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
      if (received > maximumBytes) throw transient();
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
  return value.verdict;
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

export async function handleEmail(message, env, runtime = {}) {
  const fetchAPI = runtime.fetch ?? fetch;
  const cryptoAPI = runtime.crypto ?? crypto;
  const now = runtime.now ?? (() => Date.now());
  if (!env?.EMAIL_DIRECTORY || typeof env.EMAIL_DIRECTORY.get !== "function") throw transient();

  const configValue = await directoryJSON(env.EMAIL_DIRECTORY, CONFIG_KEY);
  if (configValue == null) throw transient();
  let config;
  try {
    config = validateRuntimeConfig(configValue);
  } catch {
    throw transient();
  }
  if (!config.enabled) throw transient();

  let envelopeTo;
  let parsed;
  try {
    envelopeTo = normalizeEnvelopeAddress(message.to, false);
    parsed = parsePilotAddress(envelopeTo, config.domain, config.realm_label, true);
  } catch {
    message.setReject(PERMANENT_REJECTION);
    return;
  }
  const enrolled = config.agents.some((agent) => agent.address === parsed.baseAddress);
  if (!enrolled) {
    message.setReject(PERMANENT_REJECTION);
    return;
  }
  const recipientValue = await directoryJSON(env.EMAIL_DIRECTORY, recipientKey(parsed.baseAddress));
  // The address is enrolled in the atomic config projection. A missing or
  // inconsistent detail row can be KV propagation lag or operator error, so
  // it must retry rather than permanently bouncing an enrolled mailbox.
  if (recipientValue == null) throw transient();
  try {
    validateRuntimeRecipient(recipientValue, config, parsed.baseAddress);
  } catch {
    throw transient();
  }

  if (
    !Number.isSafeInteger(message.rawSize) ||
    message.rawSize < 1 ||
    message.rawSize > PILOT_MAXIMUM_RAW_BYTES
  ) {
    message.setReject(PERMANENT_REJECTION);
    return;
  }

  let raw;
  try {
    raw = await new Response(message.raw).arrayBuffer();
  } catch {
    throw transient();
  }
  if (raw.byteLength !== message.rawSize || raw.byteLength > PILOT_MAXIMUM_RAW_BYTES) throw transient();

  let metadata;
  try {
    metadata = normalizeRelayMetadata({
      timestamp: Math.floor(now() / 1000),
      keyId: env.RELAY_KEY_ID,
      envelopeFrom: normalizeEnvelopeAddress(message.from ?? "", true),
      envelopeTo,
      audience: recipientValue.cell_audience,
      rawSize: raw.byteLength,
      rawSHA256: await sha256Hex(raw, cryptoAPI),
    });
  } catch {
    throw transient();
  }

  let signature;
  try {
    const key = await signingKey(env, cryptoAPI);
    ({ signature } = await signRelay(metadata, key, cryptoAPI));
  } catch {
    throw transient();
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
    response = await fetchAPI(recipientValue.ingest_url, {
      method: "POST",
      headers: relayHeaders(metadata, signature),
      body: raw,
      redirect: "error",
      signal: controller.signal,
    });
    verdict = exactVerdict(await boundedResponseText(response));
  } catch (error) {
    logRelayFailure({
      phase: "fetch",
      error_name: error instanceof Error ? error.name : "unknown",
    });
    throw transient();
  } finally {
    clearTimeout(timer);
  }
  if (response.ok && verdict === "accepted") return;
  logRelayFailure({
    phase: "response",
    status: response.status,
    verdict: verdict || "invalid",
  });
  if (verdict === "unknown_recipient" || verdict === "permanent") {
    message.setReject(PERMANENT_REJECTION);
    return;
  }
  throw transient();
}

export default {
  async email(message, env) {
    await handleEmail(message, env);
  },
};
