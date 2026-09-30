#!/usr/bin/env node

import { createPrivateKey, createPublicKey, timingSafeEqual } from "node:crypto";
import { realpathSync } from "node:fs";
import { pathToFileURL } from "node:url";

import {
  RELAY_SIGNATURE_VERSION,
  RELAY_SIGNATURE_VERSION_V2,
  base64Standard,
  base64URL,
  decodePKCS8Secret,
  importSigningKey,
  normalizeRelayMetadata,
  sha256Hex,
  signRelay,
} from "../src/relay.mjs";
import { parseRouteAddress } from "../src/directory.mjs";
import { isProductionCellHost } from "./production-cell-endpoint.mjs";

// REACHABILITY_SCHEMA identifies the value-free probe result.
export const REACHABILITY_SCHEMA = "witself.agent-email.cell-reachability.v1";
// REACHABILITY_CONTRACT_SCHEMA identifies the shared server response contract.
export const REACHABILITY_CONTRACT_SCHEMA = "witself.agent-email.cell-reachability-contract.v1";
// CONTROL_AUDIENCE is deliberately distinct from every target audience.
export const CONTROL_AUDIENCE = "witself-reachability-control";
// PROBE_AGENT_SEGMENT is the synthetic recipient's agent segment.
export const PROBE_AGENT_SEGMENT = "reachability-probe";
// PROBE_DOMAIN is the canonical receive domain.
export const PROBE_DOMAIN = "witmail.net";
// PROBE_ENVELOPE_FROM is the non-deliverable synthetic sender.
export const PROBE_ENVELOPE_FROM = "witself-reachability-probe@probe.invalid";
// INGEST_PATH is the existing receive boundary.
export const INGEST_PATH = "/v1/internal/agent-email:ingest";
// PROVIDER_EVENT_PATH is the existing provider-event boundary.
export const PROVIDER_EVENT_PATH = "/v1/internal/agent-email-send:provider-event";
// LARGE_BODY_BYTES is the opt-in transport proof's exact body size.
export const LARGE_BODY_BYTES = 4 * 1024 * 1024;
// CLOCK_SKEW_LIMIT_SECONDS bounds the cell clock's difference from the operator.
export const CLOCK_SKEW_LIMIT_SECONDS = 60;
// PRIVATE_KEY_INPUT_MAXIMUM_BYTES bounds the standard-input key read.
export const PRIVATE_KEY_INPUT_MAXIMUM_BYTES = 4096;

// REACHABILITY_CONTRACT_CASES pins the answers shared with the real server test.
export const REACHABILITY_CONTRACT_CASES = Object.freeze([
  { name: "ingest_unsigned_on", method: "POST", path: INGEST_PATH, route: "on", relay: "none", status: 401, verdict: "invalid_relay", code: null, cache_control: "no-store" },
  { name: "ingest_unsigned_off", method: "POST", path: INGEST_PATH, route: "off", relay: "none", status: 404, verdict: null, code: null, cache_control: "private, no-store" },
  { name: "provider_event_unauthenticated_on", method: "POST", path: PROVIDER_EVENT_PATH, route: "on", relay: "none", status: 401, verdict: null, code: "auth_failed", cache_control: "private, no-store" },
  { name: "provider_event_unauthenticated_off", method: "POST", path: PROVIDER_EVENT_PATH, route: "off", relay: "none", status: 404, verdict: null, code: null, cache_control: "private, no-store" },
  { name: "ingest_control_audience", method: "POST", path: INGEST_PATH, route: "on", relay: "control_audience", status: 401, verdict: "invalid_relay", code: null, cache_control: "no-store" },
  { name: "ingest_unknown_recipient", method: "POST", path: INGEST_PATH, route: "on", relay: "audience", status: 404, verdict: "unknown_recipient", code: null, cache_control: "no-store" },
].map(Object.freeze));

// ProbeUsageError carries only a closed, value-free usage code.
export class ProbeUsageError extends Error {
  constructor(code) {
    super(code);
    this.name = "ProbeUsageError";
    this.code = code;
  }
}

function fail(code) {
  throw new ProbeUsageError(code);
}

const SIGNED_OPTIONS = new Set([
  "--audience", "--key-id", "--relay-public-key", "--relay-private-key-stdin",
  "--relay-version", "--large-body",
]);
const OPTIONS = new Set([
  "--mode", "--endpoint", "--expect-receive", "--expect-provider-event", ...SIGNED_OPTIONS,
]);
const FLAGS = new Set(["--large-body", "--relay-private-key-stdin"]);

// parseArguments refuses ambiguous syntax and mode-inappropriate options.
export function parseArguments(argv) {
  const values = new Map();
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (!OPTIONS.has(name) || values.has(name)) fail("invalid_arguments");
    if (FLAGS.has(name)) {
      values.set(name, true);
    } else {
      const value = argv[++index];
      if (typeof value !== "string" || value === "" || value.startsWith("--")) fail("invalid_arguments");
      values.set(name, value);
    }
  }
  for (const name of ["--mode", "--endpoint", "--expect-receive", "--expect-provider-event"]) {
    if (!values.has(name)) fail("invalid_arguments");
  }
  if (!["public", "signed"].includes(values.get("--mode")) ||
      !["on", "off"].includes(values.get("--expect-receive")) ||
      !["on", "off"].includes(values.get("--expect-provider-event"))) fail("invalid_arguments");
  if (values.get("--mode") === "public") {
    if ([...SIGNED_OPTIONS].some((name) => values.has(name))) fail("invalid_arguments");
  } else {
    if (values.get("--expect-receive") !== "on" ||
        ["--audience", "--key-id", "--relay-public-key", "--relay-private-key-stdin"].some((name) => !values.has(name))) {
      fail("invalid_arguments");
    }
    if (!values.has("--relay-version")) values.set("--relay-version", RELAY_SIGNATURE_VERSION_V2);
    if (![RELAY_SIGNATURE_VERSION, RELAY_SIGNATURE_VERSION_V2].includes(values.get("--relay-version"))) {
      fail("invalid_relay_version");
    }
  }
  return Object.fromEntries([...values].map(([name, value]) => [name.slice(2).replaceAll("-", "_"), value]));
}

function endpointDetails(endpoint) {
  let url;
  try {
    url = new URL(endpoint);
  } catch {
    fail("invalid_endpoint");
  }
  const origin = `https://${url.hostname}`;
  if (url.protocol !== "https:" || url.username || url.password || url.port || url.search || url.hash ||
      url.pathname !== "/" || ![origin, `${origin}/`].includes(endpoint) || !isProductionCellHost(url.hostname)) {
    fail("invalid_endpoint");
  }
  const cell = /^api\.([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\.cells\.witself\.witwave\.ai$/.exec(url.hostname)?.[1] ?? null;
  return { origin, host: url.hostname, cell };
}

function deriveRawPublicKey(privateKeySecret) {
  let privateKey;
  try {
    privateKey = createPrivateKey({
      key: Buffer.from(decodePKCS8Secret(privateKeySecret)),
      format: "der",
      type: "pkcs8",
    });
  } catch {
    fail("invalid_private_key");
  }
  if (privateKey.asymmetricKeyType !== "ed25519") fail("invalid_private_key");
  const spki = createPublicKey(privateKey).export({ format: "der", type: "spki" });
  // RFC 8410 Ed25519 SubjectPublicKeyInfo has a fixed prefix and a raw key.
  const prefix = Buffer.from("302a300506032b6570032100", "hex");
  if (spki.length !== prefix.length + 32 || !timingSafeEqual(spki.subarray(0, prefix.length), prefix)) {
    fail("invalid_private_key");
  }
  return spki.subarray(prefix.length);
}

async function readPrivateKey(stdin, publicKey, cryptoAPI) {
  if (stdin.isTTY === true) fail("private_key_input_invalid");
  const chunks = [];
  let total = 0;
  let text;
  try {
    for await (const chunk of stdin) {
      const bytes = Buffer.from(chunk);
      total += bytes.byteLength;
      if (total > PRIVATE_KEY_INPUT_MAXIMUM_BYTES) {
        stdin.destroy?.();
        fail("private_key_input_invalid");
      }
      chunks.push(bytes);
    }
    text = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks, total));
  } catch {
    fail("private_key_input_invalid");
  }
  if (text.trim() === "") fail("private_key_input_invalid");
  let derived;
  let privateKey;
  try {
    derived = deriveRawPublicKey(text);
    privateKey = await importSigningKey(text, cryptoAPI);
  } catch {
    fail("invalid_private_key");
  }
  if (!timingSafeEqual(derived, publicKey)) fail("key_mismatch");
  return privateKey;
}

// syntheticLabel draws a uniformly mapped, canonical 16-character realm label.
export function syntheticLabel(randomBytes) {
  const bytes = randomBytes(16);
  if (bytes.byteLength !== 16) throw new Error("invalid randomness");
  return Array.from(bytes, (byte) => "abcdefghijklmnopqrstuvwxyz234567"[byte & 31]).join("");
}

// syntheticMessage builds the exact ASCII message and optional SMTP-safe filler.
export function syntheticMessage({ recipient, nonce, nowMS, size }) {
  const small = new TextEncoder().encode([
    `From: Witself reachability probe <${PROBE_ENVELOPE_FROM}>`,
    `To: <${recipient}>`,
    "Subject: Witself cell reachability probe",
    `Message-ID: <witself-reachability-probe-${nonce}@probe.invalid>`,
    `Date: ${new Date(nowMS).toUTCString()}`,
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=us-ascii",
    "",
    "Synthetic reachability probe for an address that no agent owns.",
    "",
  ].join("\r\n"));
  if (size === "small") return small;
  if (size !== LARGE_BODY_BYTES || small.byteLength > size - 3) throw new Error("invalid message size");
  const raw = new Uint8Array(size);
  raw.set(small);
  let offset = small.byteLength;
  while (offset < size) {
    const remaining = size - offset;
    const length = remaining > 1000 ? Math.min(1000, remaining - 3) : remaining;
    raw.fill(120, offset, offset + length - 2);
    raw[offset + length - 2] = 13;
    raw[offset + length - 1] = 10;
    offset += length;
  }
  return raw;
}

// relayRequestHeaders reproduces the receive Worker's exact relay header set.
export function relayRequestHeaders(metadata, signature) {
  const headers = new Headers({
    "Content-Type": "message/rfc822",
    "X-Witself-Email-Version": metadata.version,
    "X-Witself-Email-Timestamp": String(metadata.timestamp),
    "X-Witself-Email-Key-Id": metadata.keyId,
    "X-Witself-Email-Audience": metadata.audience,
    "X-Witself-Email-Envelope-From": base64URL(new TextEncoder().encode(metadata.envelopeFrom)),
    "X-Witself-Email-Envelope-To": base64URL(new TextEncoder().encode(metadata.envelopeTo)),
    "X-Witself-Email-Raw-Size": String(metadata.rawSize),
    "X-Witself-Email-Raw-SHA256": `sha256:${metadata.rawSHA256}`,
    "X-Witself-Email-Signature": base64Standard(signature),
  });
  if (metadata.version === RELAY_SIGNATURE_VERSION_V2) {
    headers.set("X-Witself-Email-SPF-Result", metadata.spfResult);
    headers.set("X-Witself-Email-DKIM-Result", metadata.dkimResult);
    headers.set("X-Witself-Email-DMARC-Result", metadata.dmarcResult);
  }
  return headers;
}

// buildSignedRelay signs metadata and binds the exact raw message bytes.
export async function buildSignedRelay(
  { version, timestamp, keyId, envelopeFrom, envelopeTo, audience, raw },
  privateKey,
  cryptoAPI = crypto,
) {
  const metadata = normalizeRelayMetadata({
    version,
    ...(version === RELAY_SIGNATURE_VERSION_V2
      ? { spfResult: "unknown", dkimResult: "unknown", dmarcResult: "unknown" }
      : {}),
    timestamp,
    keyId,
    envelopeFrom,
    envelopeTo,
    audience,
    rawSize: raw.byteLength,
    rawSHA256: await sha256Hex(raw, cryptoAPI),
  });
  const { signature } = await signRelay(metadata, privateKey, cryptoAPI);
  return { headers: relayRequestHeaders(metadata, signature), body: raw };
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value, keys) {
  return isObject(value) && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function parseJSON(bytes) {
  try {
    return JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    return null;
  }
}

class ResponseTooLarge extends Error {}

async function readBoundedResponse(response) {
  if (!response.body) return new Uint8Array();
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > 4096) {
        // Cancellation cannot turn an already oversized response into a transport failure.
        try { await reader.cancel(); } catch { /* The size violation remains authoritative. */ }
        throw new ResponseTooLarge();
      }
      chunks.push(Buffer.from(value));
    }
  } finally {
    reader.releaseLock();
  }
  return Buffer.concat(chunks, total);
}

function contractMatches(response, body, contract) {
  if (contract.cache_control !== null && response.headers.get("cache-control") !== contract.cache_control) return false;
  if (contract.verdict !== null) {
    return response.headers.get("content-type") === "application/json" &&
      hasExactKeys(body, ["verdict"]) && body.verdict === contract.verdict;
  }
  if (contract.code !== null) {
    return hasExactKeys(body, ["schema_version", "code", "error", "retryable"]) &&
      body.schema_version === "witself.v0" && body.code === contract.code && body.retryable === false &&
      typeof body.error === "string" && body.error.length > 0 && body.error.length <= 256;
  }
  return true;
}

// runReachabilityProbe validates all inputs and executes the ordered, fail-closed checks.
export async function runReachabilityProbe(options, runtime = {}) {
  const { origin, host, cell } = endpointDetails(options.endpoint);
  const signed = options.mode === "signed";
  const fetchAPI = runtime.fetch ?? globalThis.fetch;
  const now = runtime.now ?? Date.now;
  const cryptoAPI = runtime.crypto ?? globalThis.crypto;
  let privateKey;
  if (signed) {
    // Audience is public output, so it must also satisfy the no-digest output rule.
    if (!/^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/.test(options.audience) || options.audience === CONTROL_AUDIENCE ||
        /[0-9a-f]{64}/i.test(options.audience)) {
      fail("invalid_audience");
    }
    if (cell !== null && options.audience !== cell) fail("audience_host_mismatch");
    if (!/^[a-z][a-z0-9_-]{0,63}$/.test(options.key_id)) fail("invalid_key_id");
    if (!/^[A-Za-z0-9+/]{43}=$/.test(options.relay_public_key)) fail("invalid_public_key");
    const publicKey = Buffer.from(options.relay_public_key, "base64");
    if (publicKey.byteLength !== 32) fail("invalid_public_key");
    privateKey = await readPrivateKey(runtime.stdin ?? process.stdin, publicKey, cryptoAPI);
  }
  const names = ["version", "clock", "ingest_unsigned", "provider_event_unauthenticated"];
  if (signed) names.push("ingest_control_audience", "ingest_unknown_recipient");
  if (signed && options.large_body) names.push("ingest_unknown_recipient_large");
  const result = {
    schema_version: REACHABILITY_SCHEMA,
    mode: options.mode,
    host,
    cell,
    audience: signed ? options.audience : null,
    relay_version: signed ? options.relay_version : null,
    large_body: signed && Boolean(options.large_body),
    cell_version: null,
    store_schema_version: null,
    clock_skew_seconds: null,
    result: "pass",
    checks: names.map((name) => ({ name, result: "not_run", status: null, reason: null })),
  };
  let versionDate;
  let midpoint;
  let recipient;
  let nonce;
  let messageTime;
  let small;
  for (const check of result.checks) {
    if (check.name === "clock") {
      const date = versionDate === null ? NaN : Date.parse(versionDate);
      if (!Number.isFinite(date)) {
        check.reason = "date_header_missing";
      } else {
        result.clock_skew_seconds = Math.round((date - midpoint) / 1000);
        if (Math.abs(result.clock_skew_seconds) > CLOCK_SKEW_LIMIT_SECONDS) check.reason = "clock_skew";
      }
    } else {
      let path;
      let init;
      let contract;
      let timeout = 15000;
      if (check.name === "version") {
        path = "/v1/version";
        init = { method: "GET", headers: { Accept: "application/json" } };
      } else if (check.name === "ingest_unsigned") {
        contract = REACHABILITY_CONTRACT_CASES.find((row) => row.name === `ingest_unsigned_${options.expect_receive}`);
        path = INGEST_PATH;
        init = { method: "POST", headers: { "Content-Type": "message/rfc822" }, body: "" };
      } else if (check.name === "provider_event_unauthenticated") {
        contract = REACHABILITY_CONTRACT_CASES.find((row) => row.name === `provider_event_unauthenticated_${options.expect_provider_event}`);
        path = PROVIDER_EVENT_PATH;
        init = { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" };
      } else {
        if (small === undefined) {
          const randomBytes = runtime.randomBytes ?? ((n) => globalThis.crypto.getRandomValues(new Uint8Array(n)));
          recipient = `${PROBE_AGENT_SEGMENT}.${syntheticLabel(randomBytes)}@${PROBE_DOMAIN}`;
          nonce = syntheticLabel(randomBytes);
          if (!/^[a-z2-7]{16}$/.test(parseRouteAddress(recipient, false).realmLabel)) throw new Error("invalid recipient");
          messageTime = now();
          small = syntheticMessage({ recipient, nonce, nowMS: messageTime, size: "small" });
        }
        const control = check.name === "ingest_control_audience";
        const large = check.name === "ingest_unknown_recipient_large";
        contract = REACHABILITY_CONTRACT_CASES.find((row) => row.name === (control ? check.name : "ingest_unknown_recipient"));
        path = INGEST_PATH;
        const raw = large ? syntheticMessage({ recipient, nonce, nowMS: messageTime, size: LARGE_BODY_BYTES }) : small;
        init = { method: "POST", ...await buildSignedRelay({
          version: options.relay_version,
          timestamp: Math.floor(now() / 1000),
          keyId: options.key_id,
          envelopeFrom: PROBE_ENVELOPE_FROM,
          envelopeTo: recipient,
          audience: control ? CONTROL_AUDIENCE : options.audience,
          raw,
        }, privateKey, cryptoAPI) };
        if (large) timeout = 90000;
      }
      try {
        const sent = now();
        const response = await fetchAPI(origin + path, { ...init, redirect: "manual", signal: AbortSignal.timeout(timeout) });
        const received = now();
        check.status = response.status;
        if (response.status >= 300 && response.status < 400) {
          check.reason = "redirect";
        } else {
          const body = parseJSON(await readBoundedResponse(response));
          if (response.status !== (contract?.status ?? 200)) {
            check.reason = "unexpected_status";
          } else if (contract) {
            if (!contractMatches(response, body, contract)) check.reason = "unexpected_body";
          } else if (!response.headers.get("content-type")?.startsWith("application/json") ||
              !isObject(body) || body.schema_version !== "witself.v0" ||
              typeof body.version !== "string" || !/^[0-9A-Za-z.+_-]{1,64}$/.test(body.version) ||
              /[0-9a-f]{64}/i.test(body.version) ||
              !Number.isSafeInteger(body.store_schema_version) || body.store_schema_version < 1) {
            check.reason = "unexpected_body";
          } else {
            result.cell_version = body.version;
            result.store_schema_version = body.store_schema_version;
            versionDate = response.headers.get("date");
            midpoint = (sent + received) / 2;
          }
        }
      } catch (error) {
        check.reason = error instanceof ResponseTooLarge ? "invalid_response" : "request_failed";
      }
    }
    check.result = check.reason === null ? "pass" : "fail";
    if (check.result === "fail") {
      result.result = "fail";
      break;
    }
  }
  return result;
}

// main prints only the closed result projection or a value-free error code.
export async function main(argv, runtime = {}) {
  const stdout = runtime.stdout ?? process.stdout;
  const stderr = runtime.stderr ?? process.stderr;
  try {
    const result = await runReachabilityProbe(parseArguments(argv), runtime);
    stdout.write(`${JSON.stringify(result)}\n`);
    return result.result === "pass" ? 0 : 1;
  } catch (error) {
    stderr.write(`witself-agent-email-cell-reachability: ${error instanceof ProbeUsageError ? error.code : "unavailable"}\n`);
    return error instanceof ProbeUsageError ? 2 : 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
