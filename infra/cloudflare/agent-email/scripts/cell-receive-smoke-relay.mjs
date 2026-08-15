#!/usr/bin/env node

import {
  createPrivateKey,
  createPublicKey,
  timingSafeEqual,
} from "node:crypto";
import { closeSync, constants, fsyncSync, openSync, writeSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { isAbsolute } from "node:path";
import { pathToFileURL } from "node:url";

import {
  base64Standard,
  base64URL,
  decodePKCS8Secret,
  importSigningKey,
  RELAY_SIGNATURE_VERSION,
  sha256Hex,
  signRelay,
} from "../src/relay.mjs";

const RESULT_VERDICTS = new Set(["accepted", "feature_disabled"]);
const OWNER_GATES = new Set(["feature_disabled", "address_available"]);
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const ACCOUNT_ID = /^acc_[a-z2-7]{16}$/;
const REALM_ID = /^realm_[a-z2-7]{16}$/;
const AGENT_ID = /^agent_[a-z2-7]{16}$/;
const CANONICAL_ADDRESS = /^[a-z0-9]([a-z0-9-]{0,45}[a-z0-9])?\.[a-z2-7]{16}@witmail\.net$/;
const ENVELOPE_FROM = "witself-email-receive-smoke@smoke.invalid";
const REQUIRED_ARGUMENTS = new Set([
  "--audience",
  "--agent-token-file",
  "--expected-verdict",
  "--expected-owner-gate",
  "--key-id",
  "--private-key-file",
  "--probe-file",
  "--public-keys-file",
  "--raw-file",
  "--result-file",
  "--target-file",
  "--url",
]);

export class SmokeRelayError extends Error {
  constructor(code) {
    super(code);
    this.name = "SmokeRelayError";
    this.code = code;
  }
}

function fail(code) {
  throw new SmokeRelayError(code);
}

function parseArguments(argv) {
  const values = new Map();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!REQUIRED_ARGUMENTS.has(name) || value === undefined || value === "" || values.has(name)) {
      fail("invalid_arguments");
    }
    values.set(name, value);
  }
  if (values.size !== REQUIRED_ARGUMENTS.size) fail("invalid_arguments");
  return Object.fromEntries([...values].map(([name, value]) => [name.slice(2).replaceAll("-", "_"), value]));
}

function validateLoopbackURL(value) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    fail("invalid_url");
  }
  if (
    parsed.protocol !== "http:" ||
    parsed.hostname !== "127.0.0.1" ||
    !/^\d{1,5}$/.test(parsed.port) ||
    Number(parsed.port) < 1 ||
    Number(parsed.port) > 65535 ||
    parsed.pathname !== "/v1/internal/agent-email:ingest" ||
    parsed.username !== "" ||
    parsed.password !== "" ||
    parsed.search !== "" ||
    parsed.hash !== ""
  ) {
    fail("invalid_url");
  }
  return parsed.toString();
}

function decodePublicKey(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9+/]{43}=$/.test(value)) {
    fail("invalid_public_key_set");
  }
  const decoded = Buffer.from(value, "base64");
  if (decoded.length !== 32) fail("invalid_public_key_set");
  return decoded;
}

function isPlainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value, expected) {
  if (!isPlainObject(value)) return false;
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  return actual.length === wanted.length && actual.every((key, index) => key === wanted[index]);
}

function validateTarget(value) {
  if (
    !hasExactKeys(value, [
      "schema_version",
      "account_id",
      "realm_id",
      "agent_id",
      "recipient",
      "disabled_plan",
      "entitled_plan",
    ]) ||
    value.schema_version !== 1 ||
    !ACCOUNT_ID.test(value.account_id) ||
    !REALM_ID.test(value.realm_id) ||
    !AGENT_ID.test(value.agent_id) ||
    !CANONICAL_ADDRESS.test(value.recipient) ||
    value.recipient.split("@")[0].split(".")[1] !== value.realm_id.slice("realm_".length) ||
    value.disabled_plan !== "free" ||
    value.entitled_plan !== "standard"
  ) {
    fail("invalid_target");
  }
  return value;
}

function validateProbe(value, target) {
  if (
    !hasExactKeys(value, ["nonce", "tag", "recipient", "mime_message_id", "raw_sha256", "raw_size"]) ||
    typeof value.nonce !== "string" ||
    !/^[0-9a-f]{16}$/.test(value.nonce) ||
    value.tag !== `ws-${value.nonce}` ||
    value.mime_message_id !== `<witself-receive-smoke-${value.nonce}@smoke.invalid>` ||
    typeof value.raw_sha256 !== "string" ||
    !/^[0-9a-f]{64}$/.test(value.raw_sha256) ||
    !Number.isSafeInteger(value.raw_size) ||
    value.raw_size < 1 ||
    value.raw_size > 4096
  ) {
    fail("invalid_probe");
  }
  const at = target.recipient.lastIndexOf("@");
  const expectedRecipient = `${target.recipient.slice(0, at)}+${value.tag}${target.recipient.slice(at)}`;
  if (value.recipient !== expectedRecipient) fail("invalid_probe");
  return value;
}

function decodeAgentToken(value) {
  if (typeof value !== "string" || !/^witself_agt_[A-Za-z0-9_-]{43}\n?$/.test(value)) {
    fail("invalid_agent_token");
  }
  return value.endsWith("\n") ? value.slice(0, -1) : value;
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
  // RFC 8410 Ed25519 SubjectPublicKeyInfo is a fixed 12-byte prefix followed
  // by the 32-byte raw key. Check the prefix rather than accepting any tail.
  const prefix = Buffer.from("302a300506032b6570032100", "hex");
  if (spki.length !== prefix.length + 32 || !timingSafeEqual(spki.subarray(0, prefix.length), prefix)) {
    fail("invalid_private_key");
  }
  return spki.subarray(prefix.length);
}

function buildHeaders(metadata, signature) {
  const encoder = new TextEncoder();
  return {
    "Content-Type": "message/rfc822",
    "X-Witself-Email-Version": RELAY_SIGNATURE_VERSION,
    "X-Witself-Email-Timestamp": String(metadata.timestamp),
    "X-Witself-Email-Key-Id": metadata.keyId,
    "X-Witself-Email-Audience": metadata.audience,
    "X-Witself-Email-Envelope-From": base64URL(encoder.encode(metadata.envelopeFrom)),
    "X-Witself-Email-Envelope-To": base64URL(encoder.encode(metadata.envelopeTo)),
    "X-Witself-Email-Raw-Size": String(metadata.rawSize),
    "X-Witself-Email-Raw-SHA256": `sha256:${metadata.rawSHA256}`,
    "X-Witself-Email-Signature": base64Standard(signature),
  };
}

async function writeExclusiveResult(path, result) {
  if (!isAbsolute(path)) fail("invalid_result_path");
  let descriptor;
  try {
    descriptor = openSync(
      path,
      constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY | (constants.O_NOFOLLOW ?? 0),
      0o600,
    );
    writeSync(descriptor, `${JSON.stringify(result)}\n`);
    fsyncSync(descriptor);
    closeSync(descriptor);
    descriptor = undefined;
  } catch {
    if (descriptor !== undefined) closeSync(descriptor);
    fail("result_write_failed");
  }
}

async function readBoundedResponse(response, maximumBytes = 256) {
  if (!response.body || typeof response.body.getReader !== "function") fail("invalid_response");
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maximumBytes) {
        await reader.cancel();
        fail("invalid_response");
      }
      chunks.push(Buffer.from(value));
    }
  } catch (error) {
    if (error instanceof SmokeRelayError) throw error;
    fail("invalid_response");
  }
  return Buffer.concat(chunks, total);
}

function ownerAddressMatches(body, target) {
  if (!hasExactKeys(body, ["schema_version", "address"]) || body.schema_version !== "witself.v0") {
    return false;
  }
  const address = body.address;
  if (
    !isPlainObject(address) ||
    address.account_id !== target.account_id ||
    address.realm_id !== target.realm_id ||
    address.owner_agent_id !== target.agent_id ||
    address.address !== target.recipient ||
    address.domain !== "witmail.net" ||
    address.local_part !== target.recipient.slice(0, -"@witmail.net".length) ||
    address.realm_label !== target.realm_id.slice("realm_".length) ||
    address.receive_state !== "enabled" ||
    address.agent_receive_state !== "enabled" ||
    address.realm_receive_state !== "enabled" ||
    !Array.isArray(address.addresses) ||
    !address.addresses.some(
      (candidate) =>
        hasExactKeys(candidate, ["address", "domain", "role"]) &&
        candidate.address === target.recipient &&
        candidate.domain === "witmail.net" &&
        candidate.role === "primary",
    )
  ) {
    return false;
  }
  return true;
}

async function proveOwnerGate(fetchAPI, ingestURL, agentToken, target, expectedGate) {
  const ownerURL = new URL(ingestURL);
  ownerURL.pathname = "/v1/email/address";
  let response;
  try {
    response = await fetchAPI(ownerURL, {
      method: "GET",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${agentToken}`,
      },
      redirect: "manual",
      signal: AbortSignal.timeout(15_000),
    });
  } catch {
    fail("owner_request_failed");
  }
  let body;
  try {
    const responseBytes = await readBoundedResponse(response, 32 * 1024);
    body = JSON.parse(responseBytes.toString("utf8"));
  } catch (error) {
    if (error instanceof SmokeRelayError) throw error;
    fail("invalid_owner_response");
  }
  if (
    response.headers.get("content-type") !== "application/json" ||
    response.headers.get("cache-control") !== "private, no-store"
  ) {
    fail("unexpected_owner_gate");
  }
  if (expectedGate === "feature_disabled") {
    if (
      response.status !== 403 ||
      !hasExactKeys(body, ["schema_version", "code", "feature", "error", "retryable"]) ||
      body.schema_version !== "witself.v0" ||
      body.code !== "feature_not_enabled" ||
      body.feature !== "agent_email_receive" ||
      body.error !== "Sorry, this feature is not enabled on this account." ||
      body.retryable !== false
    ) {
      fail("unexpected_owner_gate");
    }
    return;
  }
  if (response.status !== 200 || !ownerAddressMatches(body, target)) {
    fail("unexpected_owner_gate");
  }
}

export async function runSignedProbe(options, runtime = {}) {
  const fetchAPI = runtime.fetch ?? fetch;
  const now = runtime.now ?? (() => Date.now());
  const cryptoAPI = runtime.crypto ?? crypto;
  const url = validateLoopbackURL(options.url);
  if (!RESULT_VERDICTS.has(options.expectedVerdict)) fail("invalid_expected_verdict");
  if (!OWNER_GATES.has(options.expectedOwnerGate)) fail("invalid_expected_owner_gate");
  if (
    (options.expectedOwnerGate === "feature_disabled") !== (options.expectedVerdict === "feature_disabled")
  ) {
    fail("inconsistent_expectations");
  }
  if (!/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(options.audience) || !KEY_ID.test(options.keyId)) {
    fail("invalid_metadata");
  }
  if (
    [
      options.agentTokenFile,
      options.privateKeyFile,
      options.probeFile,
      options.publicKeysFile,
      options.rawFile,
      options.resultFile,
      options.targetFile,
    ].some((path) => typeof path !== "string" || !isAbsolute(path))
  ) {
    fail("invalid_input_path");
  }

  let raw;
  let privateKeySecret;
  let publicKeys;
  let agentToken;
  let target;
  let probe;
  try {
    [raw, privateKeySecret, publicKeys, agentToken, target, probe] = await Promise.all([
      readFile(options.rawFile),
      readFile(options.privateKeyFile, "utf8"),
      readFile(options.publicKeysFile, "utf8").then(JSON.parse),
      readFile(options.agentTokenFile, "utf8").then(decodeAgentToken),
      readFile(options.targetFile, "utf8").then(JSON.parse).then(validateTarget),
      readFile(options.probeFile, "utf8").then(JSON.parse),
    ]);
  } catch {
    fail("input_read_failed");
  }
  probe = validateProbe(probe, target);
  if (!publicKeys || typeof publicKeys !== "object" || Array.isArray(publicKeys)) {
    fail("invalid_public_key_set");
  }
  const configuredEntries = Object.entries(publicKeys);
  if (configuredEntries.length < 1 || configuredEntries.length > 2) fail("invalid_public_key_set");
  if (configuredEntries.some(([keyID]) => !KEY_ID.test(keyID))) fail("invalid_public_key_set");
  const configuredPublic = decodePublicKey(publicKeys[options.keyId]);
  const derivedPublic = deriveRawPublicKey(privateKeySecret);
  if (!timingSafeEqual(configuredPublic, derivedPublic)) fail("key_mismatch");
  const matchingKeyIDs = configuredEntries
    .filter(([, value]) => timingSafeEqual(decodePublicKey(value), derivedPublic))
    .map(([keyID]) => keyID);
  if (matchingKeyIDs.length !== 1 || matchingKeyIDs[0] !== options.keyId) fail("ambiguous_key_match");

  if (raw.length !== probe.raw_size || (await sha256Hex(raw, cryptoAPI)) !== probe.raw_sha256) {
    fail("probe_body_mismatch");
  }
  await proveOwnerGate(fetchAPI, url, agentToken, target, options.expectedOwnerGate);

  const metadata = {
    timestamp: Math.floor(now() / 1000),
    keyId: options.keyId,
    envelopeFrom: ENVELOPE_FROM,
    envelopeTo: probe.recipient,
    audience: options.audience,
    rawSize: raw.length,
    rawSHA256: await sha256Hex(raw, cryptoAPI),
  };
  let privateKey;
  let signature;
  try {
    privateKey = await importSigningKey(privateKeySecret, cryptoAPI);
    ({ signature } = await signRelay(metadata, privateKey, cryptoAPI));
  } catch {
    fail("signing_failed");
  }

  let response;
  try {
    response = await fetchAPI(url, {
      method: "POST",
      headers: buildHeaders(metadata, signature),
      body: raw,
      redirect: "manual",
      signal: AbortSignal.timeout(15_000),
    });
  } catch {
    fail("request_failed");
  }
  let body;
  try {
    const responseBytes = await readBoundedResponse(response);
    body = JSON.parse(responseBytes.toString("utf8"));
  } catch (error) {
    if (error instanceof SmokeRelayError) throw error;
    fail("invalid_response");
  }
  if (
    response.status !== 200 ||
    response.headers.get("content-type") !== "application/json" ||
    response.headers.get("cache-control") !== "no-store" ||
    !body ||
    typeof body !== "object" ||
    Array.isArray(body) ||
    Object.keys(body).length !== 1 ||
    body.verdict !== options.expectedVerdict
  ) {
    fail("unexpected_verdict");
  }
  const result = {
    owner_gate: options.expectedOwnerGate,
    http_status: response.status,
    verdict: body.verdict,
  };
  await writeExclusiveResult(options.resultFile, result);
  return result;
}

export async function main(argv = process.argv.slice(2)) {
  const args = parseArguments(argv);
  await runSignedProbe({
    audience: args.audience,
    agentTokenFile: args.agent_token_file,
    expectedVerdict: args.expected_verdict,
    expectedOwnerGate: args.expected_owner_gate,
    keyId: args.key_id,
    privateKeyFile: args.private_key_file,
    probeFile: args.probe_file,
    publicKeysFile: args.public_keys_file,
    rawFile: args.raw_file,
    resultFile: args.result_file,
    targetFile: args.target_file,
    url: args.url,
  });
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    const code = error instanceof SmokeRelayError ? error.code : "unavailable";
    process.stderr.write(`witself-agent-email-smoke-relay: ${code}\n`);
    process.exitCode = 1;
  });
}
