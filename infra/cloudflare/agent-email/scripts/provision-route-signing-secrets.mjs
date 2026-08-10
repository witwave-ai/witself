#!/usr/bin/env node
import {
  createPrivateKey,
  createPublicKey,
  randomBytes,
  sign,
  timingSafeEqual,
  verify,
} from "node:crypto";
import { spawnSync } from "node:child_process";
import {
  accessSync,
  chmodSync,
  constants as fsConstants,
  lstatSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { CUSTOM_DOMAIN_DELIVERY_SECRET } from
  "./assert-custom-domain-dark.mjs";
import {
  EMAIL_DARK_SECRET_NAMES,
} from "../../control-plane/scripts/assert-custom-domain-dark.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const CONTROL_PLANE_ROOT = resolve(root, "../control-plane");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const ROUTE_PRIVATE_SECRET = "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY";
const FALLBACK_TOKEN_SECRET = "CONTROL_PLANE_EDGE_TOKEN";
const RELAY_PRIVATE_SECRET = "RELAY_ED25519_PRIVATE_KEY";
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const SECRET_ID = /^sec_[a-z2-7]{16}$/;
const FIELD_ID = /^fld_[a-z2-7]{16}$/;
const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const PUBLIC_KEY = /^[A-Za-z0-9+/]{43}=$/;
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");
const MAX_JSON_OUTPUT = 5 * 1024 * 1024;
const WRANGLER_UNSAFE_ENVIRONMENT = Object.freeze([
  "CLOUDFLARE_API_BASE_URL",
  "CF_API_BASE_URL",
  "WRANGLER_API_ENVIRONMENT",
  "WRANGLER_LOG_PATH",
  "WRANGLER_OUTPUT_FILE_DIRECTORY",
  "WRANGLER_OUTPUT_FILE_PATH",
  "WRANGLER_CI_OVERRIDE_NAME",
  "WRANGLER_AUTH_DOMAIN",
  "WRANGLER_AUTH_URL",
  "WRANGLER_REVOKE_URL",
  "WRANGLER_TOKEN_URL",
  "NODE_OPTIONS",
  "NODE_DEBUG",
  "NODE_V8_COVERAGE",
  "SSLKEYLOGFILE",
]);
const WITSELF_UNSAFE_ENVIRONMENT = Object.freeze([
  "NODE_OPTIONS",
  "NODE_DEBUG",
  "NODE_V8_COVERAGE",
  "SSLKEYLOGFILE",
  "WITSELF_ENDPOINT",
  "WITSELF_TOKEN",
  "WITSELF_TOKEN_FILE",
  "WITSELF_CONTROL_PLANE",
  "WITSELF_CONTROL_PLANE_ADDR",
]);

function fail(message) {
  throw new Error(message);
}

function selector(value, label) {
  if (typeof value !== "string" || value.length < 1 || value.length > 256 ||
      value !== value.trim() || value.startsWith("-") ||
      /[\x00-\x1f\x7f]/.test(value)) {
    fail(`${label} is invalid`);
  }
  return value;
}

function identityName(value, label, fallback = "") {
  const candidate = String(value ?? fallback);
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(candidate)) {
    fail(`${label} is missing or invalid`);
  }
  return candidate;
}

function optionValue(argv, index, name) {
  const value = argv[index + 1];
  if (typeof value !== "string" || value === "") {
    fail(`${name} requires a value`);
  }
  return value;
}

export function parseProvisioningArgs(argv, env = process.env) {
  const out = {
    controlPlaneConfig: join(CONTROL_PLANE_ROOT, "wrangler.generated.jsonc"),
    emailEdgeConfig: join(root, "wrangler.generated.jsonc"),
    account: String(env.WITSELF_ACCOUNT ?? "default"),
    realm: String(env.WITSELF_REALM ?? "default"),
    agent: String(env.WITSELF_AGENT ?? ""),
    endpoint: "",
    tokenFile: "",
    receipt: "",
    routeSecret: "",
    routePrivateField: "",
    routePublicField: "",
    fallbackSecret: "",
    fallbackField: "",
  };
  const names = new Map([
    ["--control-plane-config", "controlPlaneConfig"],
    ["--email-edge-config", "emailEdgeConfig"],
    ["--account", "account"],
    ["--realm", "realm"],
    ["--agent", "agent"],
    ["--endpoint", "endpoint"],
    ["--token-file", "tokenFile"],
    ["--receipt", "receipt"],
    ["--route-secret", "routeSecret"],
    ["--route-private-field", "routePrivateField"],
    ["--route-public-field", "routePublicField"],
    ["--fallback-secret", "fallbackSecret"],
    ["--fallback-field", "fallbackField"],
  ]);
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const property = names.get(name);
    if (property == null) fail(`unknown argument ${name ?? ""}`.trim());
    out[property] = optionValue(argv, index, name);
  }

  for (const [property, label] of [
    ["routeSecret", "--route-secret"],
    ["routePrivateField", "--route-private-field"],
    ["routePublicField", "--route-public-field"],
    ["fallbackSecret", "--fallback-secret"],
    ["fallbackField", "--fallback-field"],
  ]) {
    out[property] = selector(out[property], label);
  }
  out.account = identityName(out.account, "--account", "default");
  out.realm = identityName(out.realm, "--realm", "default");
  out.agent = identityName(out.agent, "--agent");
  for (const property of ["controlPlaneConfig", "emailEdgeConfig", "tokenFile", "receipt"]) {
    if (out[property] !== "") {
      out[property] = isAbsolute(out[property])
        ? resolve(out[property])
        : resolve(root, out[property]);
    }
  }
  if (out.controlPlaneConfig === out.emailEdgeConfig) {
    fail("control-plane and email-edge config paths must be distinct");
  }
  if (out.endpoint !== "") {
    let parsed;
    try {
      parsed = new URL(out.endpoint);
    } catch {
      fail("--endpoint must be a credential-free HTTPS URL");
    }
    if (parsed.protocol !== "https:" || parsed.username || parsed.password ||
        parsed.search || parsed.hash || !parsed.hostname) {
      fail("--endpoint must be a credential-free HTTPS URL");
    }
  }
  return Object.freeze(out);
}

// Wrangler files are JSONC. This conservative scanner removes comments and
// trailing commas without ever interpreting comment markers inside strings.
export function parseJSONC(raw, label = "configuration") {
  if (typeof raw !== "string" || raw.length === 0 || raw.length > MAX_JSON_OUTPUT) {
    fail(`${label} was empty or too large`);
  }
  let withoutComments = "";
  let state = "code";
  let escaped = false;
  for (let index = 0; index < raw.length; index += 1) {
    const character = raw[index];
    const next = raw[index + 1];
    if (state === "string") {
      withoutComments += character;
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') state = "code";
      continue;
    }
    if (state === "line-comment") {
      if (character === "\n" || character === "\r") {
        withoutComments += character;
        state = "code";
      }
      continue;
    }
    if (state === "block-comment") {
      if (character === "*" && next === "/") {
        state = "code";
        index += 1;
      } else if (character === "\n" || character === "\r") {
        withoutComments += character;
      }
      continue;
    }
    if (character === '"') {
      state = "string";
      withoutComments += character;
    } else if (character === "/" && next === "/") {
      state = "line-comment";
      index += 1;
    } else if (character === "/" && next === "*") {
      state = "block-comment";
      index += 1;
    } else {
      withoutComments += character;
    }
  }
  if (state === "string" || state === "block-comment") {
    fail(`${label} was invalid JSONC`);
  }
  let normalized = "";
  state = "code";
  escaped = false;
  for (let index = 0; index < withoutComments.length; index += 1) {
    const character = withoutComments[index];
    if (state === "string") {
      normalized += character;
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') state = "code";
      continue;
    }
    if (character === '"') {
      state = "string";
      normalized += character;
      continue;
    }
    if (character === ",") {
      let lookahead = index + 1;
      while (/\s/.test(withoutComments[lookahead] ?? "")) lookahead += 1;
      if (["}", "]"].includes(withoutComments[lookahead])) continue;
    }
    normalized += character;
  }
  try {
    const parsed = JSON.parse(normalized);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      fail(`${label} must be a JSON object`);
    }
    return parsed;
  } catch (error) {
    if (error?.message === `${label} must be a JSON object`) throw error;
    fail(`${label} was invalid JSONC`);
  }
}

function canonicalPublicKeyring(raw) {
  if (typeof raw !== "string" || raw.length > 1024) {
    fail("email-edge route verification keyring is invalid");
  }
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    fail("email-edge route verification keyring is invalid");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    fail("email-edge route verification keyring is invalid");
  }
  const entries = Object.entries(parsed).sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0);
  if (entries.length < 1 || entries.length > 4 ||
      JSON.stringify(Object.fromEntries(entries)) !== raw ||
      entries.some(([keyID, encoded]) => {
        if (!KEY_ID.test(keyID) || typeof encoded !== "string" ||
            !PUBLIC_KEY.test(encoded)) return true;
        const decoded = Buffer.from(encoded, "base64");
        return decoded.byteLength !== 32 || decoded.toString("base64") !== encoded;
      })) {
    fail("email-edge route verification keyring is invalid");
  }
  return Object.freeze(Object.fromEntries(entries));
}

export function validateProvisioningConfigs(controlPlaneRaw, emailEdgeRaw) {
  const controlPlane = parseJSONC(controlPlaneRaw, "control-plane configuration");
  const emailEdge = parseJSONC(emailEdgeRaw, "email-edge configuration");
  if (controlPlane.name !== CONTROL_PLANE_WORKER ||
      emailEdge.name !== EMAIL_EDGE_WORKER) {
    fail("configuration did not identify the exact expected Workers");
  }
  const controlPlaneRequired = controlPlane.secrets?.required;
  const emailEdgeRequired = emailEdge.secrets?.required;
  if (!Array.isArray(controlPlaneRequired) ||
      ![ROUTE_PRIVATE_SECRET, FALLBACK_TOKEN_SECRET].every((name) =>
        controlPlaneRequired.includes(name)) ||
      !Array.isArray(emailEdgeRequired) ||
      ![FALLBACK_TOKEN_SECRET, RELAY_PRIVATE_SECRET].every((name) =>
        emailEdgeRequired.includes(name))) {
    fail("configuration did not declare the exact required Worker secrets");
  }
  const keyID = controlPlane.vars?.AGENT_EMAIL_ROUTE_SIGNING_KEY_ID;
  if (typeof keyID !== "string" || !KEY_ID.test(keyID)) {
    fail("control-plane route signing key id is invalid");
  }
  const keyring = canonicalPublicKeyring(
    emailEdge.vars?.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS,
  );
  if (!Object.hasOwn(keyring, keyID)) {
    fail("active route signing key id is absent from the email-edge keyring");
  }
  if (emailEdge.vars?.REALM_EMAIL_ALIAS_DELIVERY_ENABLED !== "false" ||
      emailEdge.vars?.REALM_EMAIL_CANONICAL_DELIVERY_ENABLED !== "false") {
    fail("email-edge generated configuration must keep managed delivery dark");
  }
  if (controlPlane.vars?.CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST !== "" ||
      emailEdge.vars?.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST !== "") {
    fail("generated managed delivery cohorts must be empty before provisioning");
  }
  return Object.freeze({
    keyID,
    publicKey: keyring[keyID],
    trustedKeyIDs: Object.freeze(Object.keys(keyring)),
  });
}

function activeVersionID(deployment, label) {
  if (!deployment || typeof deployment !== "object" ||
      Array.isArray(deployment) || !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    fail(`${label} must already exist with one production version at 100 percent`);
  }
  return deployment.versions[0].version_id;
}

function activeBindings(version, expectedID, label) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedID || !Array.isArray(version.resources?.bindings)) {
    fail(`${label} active Worker version inventory is invalid`);
  }
  const bindings = new Map();
  for (const item of version.resources.bindings) {
    if (!item || typeof item !== "object" || Array.isArray(item) ||
        typeof item.name !== "string" || item.name === "" ||
        bindings.has(item.name)) {
      fail(`${label} active Worker binding inventory is invalid`);
    }
    bindings.set(item.name, item);
  }
  return bindings;
}

function secretInventory(raw, label) {
  if (!Array.isArray(raw)) fail(`${label} secret inventory is invalid`);
  const names = new Set();
  for (const item of raw) {
    if (!item || typeof item !== "object" || Array.isArray(item) ||
        typeof item.name !== "string" || item.name === "" ||
        item.name !== item.name.trim() || names.has(item.name)) {
      fail(`${label} secret inventory is invalid`);
    }
    names.add(item.name);
  }
  return names;
}

function assertSecretBinding(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    fail(`${label} is missing required ${name} secret binding`);
  }
}

function assertRemoteDark({ controlPlane, emailEdge }) {
  const cpBindings = activeBindings(
    controlPlane.version,
    activeVersionID(controlPlane.deployment, "control plane"),
    "control plane",
  );
  const edgeBindings = activeBindings(
    emailEdge.version,
    activeVersionID(emailEdge.deployment, "email edge"),
    "email edge",
  );
  const cpSecrets = secretInventory(controlPlane.secrets, "control plane");
  const edgeSecrets = secretInventory(emailEdge.secrets, "email edge");
  const activeGate = EMAIL_DARK_SECRET_NAMES.find((name) =>
    cpBindings.has(name) || cpSecrets.has(name));
  if (activeGate != null) {
    fail("control-plane agent-email delivery must be dark before provisioning");
  }
  if (edgeBindings.has(CUSTOM_DOMAIN_DELIVERY_SECRET) ||
      edgeSecrets.has(CUSTOM_DOMAIN_DELIVERY_SECRET)) {
    fail("email-edge custom-domain delivery must be dark before provisioning");
  }
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    const binding = edgeBindings.get(name);
    if (binding?.type !== "plain_text" || binding.text !== "false") {
      fail("email-edge managed delivery must be dark before provisioning");
    }
  }
  const cpCohort = cpBindings.get(
    "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  );
  const edgeCohort = edgeBindings.get(
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  );
  if (cpCohort?.type !== "plain_text" || cpCohort.text !== "" ||
      edgeCohort?.type !== "plain_text" || edgeCohort.text !== "") {
    fail("active managed delivery cohorts must be empty before provisioning");
  }
  // A pre-existing email Worker is mandatory. This avoids Wrangler's first-
  // deploy secret bootstrap path, which needs a complete --secrets-file.
  assertSecretBinding(edgeBindings, RELAY_PRIVATE_SECRET, "email edge");
  if (!edgeSecrets.has(RELAY_PRIVATE_SECRET)) {
    fail("email edge persistent relay secret is missing");
  }
}

function assertProvisionedRemote(remote) {
  assertRemoteDark(remote);
  const cpBindings = activeBindings(
    remote.controlPlane.version,
    activeVersionID(remote.controlPlane.deployment, "control plane"),
    "control plane",
  );
  const edgeBindings = activeBindings(
    remote.emailEdge.version,
    activeVersionID(remote.emailEdge.deployment, "email edge"),
    "email edge",
  );
  const cpSecrets = secretInventory(
    remote.controlPlane.secrets,
    "control plane",
  );
  const edgeSecrets = secretInventory(remote.emailEdge.secrets, "email edge");
  for (const name of [ROUTE_PRIVATE_SECRET, FALLBACK_TOKEN_SECRET]) {
    assertSecretBinding(cpBindings, name, "control plane");
    if (!cpSecrets.has(name)) {
      fail(`control plane persistent ${name} secret is missing`);
    }
  }
  for (const name of [FALLBACK_TOKEN_SECRET, RELAY_PRIVATE_SECRET]) {
    assertSecretBinding(edgeBindings, name, "email edge");
    if (!edgeSecrets.has(name)) {
      fail(`email edge persistent ${name} secret is missing`);
    }
  }
}

function exactObjectKeys(value, expected, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...expected].sort())) {
    fail(`${label} envelope is invalid`);
  }
}

function resolveField(secret, fieldSelector, label) {
  const matches = secret.fields.filter((field) =>
    field.id === fieldSelector || field.name === fieldSelector);
  if (matches.length !== 1) fail(`${label} field selector did not resolve exactly once`);
  return matches[0];
}

export function validateSecretShowEnvelope(envelope, secretSelector, fields) {
  exactObjectKeys(envelope, ["secret"], "Witself secret show");
  const secret = envelope.secret;
  if (!secret || typeof secret !== "object" || Array.isArray(secret) ||
      !SECRET_ID.test(String(secret.id ?? "")) ||
      typeof secret.name !== "string" || secret.name === "" ||
      (secret.id !== secretSelector && secret.name !== secretSelector) ||
      secret.lifecycle !== "active" || !Number.isSafeInteger(secret.row_version) ||
      secret.row_version < 1 || !Array.isArray(secret.fields) ||
      secret.fields.length < 1 || secret.fields.length > 128) {
    fail("Witself secret show envelope is invalid");
  }
  for (const field of secret.fields) {
    if (!field || typeof field !== "object" || Array.isArray(field) ||
        !FIELD_ID.test(String(field.id ?? "")) ||
        typeof field.name !== "string" || field.name === "" ||
        typeof field.sensitive !== "boolean" ||
        !["utf8", "json", "binary"].includes(field.encoding) ||
        typeof field.redacted !== "boolean") {
      fail("Witself secret show field inventory is invalid");
    }
    if (field.sensitive && Object.hasOwn(field, "public_value")) {
      fail("Witself secret show exposed a sensitive field value");
    }
  }
  const resolved = {};
  for (const [key, fieldSelector] of Object.entries(fields)) {
    resolved[key] = resolveField(secret, fieldSelector, "Witself secret show");
  }
  return Object.freeze({ secret, fields: Object.freeze(resolved) });
}

export function validateRevealEnvelope(
  envelope,
  expected,
  label,
  { trimOuterWhitespace = false } = {},
) {
  exactObjectKeys(
    envelope,
    ["secret_id", "field_id", "field_name", "encoding", "value"],
    label,
  );
  if (typeof envelope.value !== "string" || envelope.value.length > 4098) {
    fail(`${label} envelope is invalid`);
  }
  const value = trimOuterWhitespace
    ? envelope.value.trim()
    : envelope.value;
  if (envelope.secret_id !== expected.secretID ||
      envelope.field_id !== expected.field.id ||
      envelope.field_name !== expected.field.name ||
      envelope.encoding !== expected.field.encoding ||
      envelope.encoding !== "utf8" ||
      value.length === 0 || value.length > 4096 ||
      (!trimOuterWhitespace && value !== value.trim()) || value.includes("\0")) {
    fail(`${label} envelope is invalid`);
  }
  return value;
}

function rawPublicFromPrivate(privateValue) {
  let privateKey;
  try {
    if (privateValue.includes("-----BEGIN PRIVATE KEY-----")) {
      if (!/^-----BEGIN PRIVATE KEY-----\n(?:[A-Za-z0-9+/=]+\n)+-----END PRIVATE KEY-----$/.test(privateValue)) {
        fail("route signing private key is not canonical PKCS8 PEM");
      }
      privateKey = createPrivateKey(privateValue);
    } else {
      if (!/^[A-Za-z0-9+/]+={0,2}$/.test(privateValue)) {
        fail("route signing private key is not canonical PKCS8 base64");
      }
      const der = Buffer.from(privateValue, "base64");
      try {
        if (der.toString("base64") !== privateValue ||
            der.byteLength < 40 || der.byteLength > 128) {
          fail("route signing private key is not canonical PKCS8 base64");
        }
        privateKey = createPrivateKey({ key: der, format: "der", type: "pkcs8" });
      } finally {
        der.fill(0);
      }
    }
  } catch (error) {
    if (String(error?.message ?? "").startsWith("route signing private key")) {
      throw error;
    }
    fail("route signing private key is not valid Ed25519 PKCS8");
  }
  if (privateKey.asymmetricKeyType !== "ed25519") {
    fail("route signing private key is not Ed25519");
  }
  const spki = createPublicKey(privateKey).export({ format: "der", type: "spki" });
  if (spki.byteLength !== ED25519_SPKI_PREFIX.byteLength + 32 ||
      !spki.subarray(0, ED25519_SPKI_PREFIX.byteLength).equals(ED25519_SPKI_PREFIX)) {
    fail("route signing public key derivation failed");
  }
  const proof = randomBytes(32);
  const signature = sign(null, proof, privateKey);
  const verified = verify(null, proof, createPublicKey(privateKey), signature);
  proof.fill(0);
  signature.fill(0);
  if (!verified) fail("route signing keypair proof failed");
  return Buffer.from(spki.subarray(ED25519_SPKI_PREFIX.byteLength));
}

export function verifyEd25519Keypair(privateValue, expectedPublicBase64) {
  if (typeof expectedPublicBase64 !== "string" ||
      !PUBLIC_KEY.test(expectedPublicBase64)) {
    fail("route signing public key is invalid");
  }
  const expected = Buffer.from(expectedPublicBase64, "base64");
  const actual = rawPublicFromPrivate(privateValue);
  const matches = expected.byteLength === actual.byteLength &&
    timingSafeEqual(expected, actual);
  expected.fill(0);
  actual.fill(0);
  if (!matches) fail("route signing private key does not match the active public key");
  return true;
}

export function validateFallbackToken(value) {
  if (typeof value !== "string" || value !== value.trim() ||
      Buffer.byteLength(value, "utf8") < 32 ||
      Buffer.byteLength(value, "utf8") > 4096 ||
      /[\x00-\x20\x7f]/.test(value)) {
    fail("fallback token does not meet the minimum high-entropy token policy");
  }
  const counts = new Map();
  for (const character of value) {
    counts.set(character, (counts.get(character) ?? 0) + 1);
  }
  let entropyPerCharacter = 0;
  for (const count of counts.values()) {
    const probability = count / [...value].length;
    entropyPerCharacter -= probability * Math.log2(probability);
  }
  if (counts.size < 10 || entropyPerCharacter < 3 ||
      entropyPerCharacter * [...value].length < 128) {
    fail("fallback token does not meet the minimum high-entropy token policy");
  }
  return true;
}

export function sanitizedWranglerEnvironment(source = process.env) {
  const output = { ...source };
  for (const name of WRANGLER_UNSAFE_ENVIRONMENT) delete output[name];
  Object.assign(output, {
    WRANGLER_WRITE_LOGS: "false",
    WRANGLER_LOG_SANITIZE: "true",
    WRANGLER_SEND_METRICS: "false",
    WRANGLER_SEND_ERROR_REPORTS: "false",
    WRANGLER_LOG: "error",
    NO_COLOR: "1",
    TERM: "dumb",
  });
  return output;
}

export function sanitizedWranglerInspectionEnvironment(source = process.env) {
  const output = sanitizedWranglerEnvironment(source);
  // Wrangler 4.120.0 suppresses JSON stdout from read-only inspection
  // commands when WRANGLER_LOG=error. Keep every redirection, telemetry, and
  // file-logging guard, but let Wrangler use its default level while stdout
  // and stderr remain captured by spawnJSON.
  delete output.WRANGLER_LOG;
  return output;
}

export function sanitizedWitselfEnvironment(source = process.env) {
  const output = { ...source };
  for (const name of WITSELF_UNSAFE_ENVIRONMENT) delete output[name];
  for (const name of Object.keys(output)) {
    if (name.startsWith("WITSELF_") && /(?:LOG|OUTPUT)/.test(name)) {
      delete output[name];
    }
  }
  Object.assign(output, { NO_COLOR: "1", TERM: "dumb" });
  return output;
}

function spawnJSON(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: options.cwd,
    env: options.env,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: MAX_JSON_OUTPUT,
    timeout: 120_000,
  });
  if (result.error || result.status !== 0 ||
      typeof result.stdout !== "string" || result.stdout.length === 0) {
    fail(`could not ${options.operation}`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    fail(`${options.operation} output was not valid JSON`);
  }
}

function spawnSecretPut(command, args, value, options = {}) {
  const input = Buffer.isBuffer(value) ? value : Buffer.from(value, "utf8");
  const result = spawnSync(command, args, {
    cwd: options.cwd,
    env: options.env,
    input,
    stdio: ["pipe", "ignore", "ignore"],
    timeout: 120_000,
  });
  if (!Buffer.isBuffer(value)) input.fill(0);
  if (result.error || result.status !== 0) fail(`could not ${options.operation}`);
}

export function assertReceiptAvailable(path) {
  if (!path) return;
  try {
    lstatSync(path);
  } catch (error) {
    if (error?.code !== "ENOENT") fail("could not inspect receipt path");
    try {
      accessSync(dirname(path), fsConstants.W_OK);
    } catch {
      fail("receipt directory is not writable");
    }
    return;
  }
  fail("receipt path already exists; refusing to overwrite it");
}

export function writeReceiptExclusive(path, receipt) {
  writeFileSync(path, `${JSON.stringify(receipt, null, 2)}\n`, {
    encoding: "utf8",
    flag: "wx",
    mode: 0o600,
  });
  chmodSync(path, 0o600);
}

export function createSpawnRuntime() {
  return Object.freeze({
    readText(path) {
      return readFileSync(path, "utf8");
    },
    json(command, args, options) {
      return spawnJSON(command, args, options);
    },
    secretPut(command, args, value, options) {
      return spawnSecretPut(command, args, value, options);
    },
    assertReceiptAvailable,
    writeReceiptExclusive,
  });
}

function wranglerJSON(runtime, args, config, operation, env) {
  return runtime.json("wrangler", [...args, "--config", config], {
    cwd: root,
    env,
    operation,
  });
}

function inspectWorker(runtime, worker, config, label, env) {
  const deployment = wranglerJSON(runtime, [
    "deployments", "status", "--name", worker, "--json",
  ], config, `inspect the ${label} deployment`, env);
  const versionID = activeVersionID(deployment, label);
  const version = wranglerJSON(runtime, [
    "versions", "view", versionID, "--name", worker, "--json",
  ], config, `inspect the ${label} active version`, env);
  const secrets = wranglerJSON(runtime, [
    "secret", "list", "--name", worker, "--format", "json",
  ], config, `inspect the ${label} secret inventory`, env);
  return { deployment, version, secrets };
}

function witselfIdentityArgs(options) {
  const args = [
    "--account", options.account,
    "--realm", options.realm,
    "--agent", options.agent,
  ];
  if (options.endpoint) args.push("--endpoint", options.endpoint);
  if (options.tokenFile) args.push("--token-file", options.tokenFile);
  return args;
}

function showSecret(runtime, secret, options, env) {
  return runtime.json("witself", [
    "secret", "show", secret, "--json", ...witselfIdentityArgs(options),
  ], {
    cwd: root,
    env,
    operation: "inspect Witself secret metadata",
  });
}

function revealSecret(runtime, secret, field, options, env) {
  return runtime.json("witself", [
    "secret", "reveal", secret, field, "--json",
    ...witselfIdentityArgs(options),
  ], {
    cwd: root,
    env,
    operation: "perform audited Witself secret reveal",
  });
}

function putWorkerSecret(runtime, worker, config, name, value, env) {
  runtime.secretPut("wrangler", [
    "secret", "put", name,
    "--name", worker,
    "--config", config,
  ], value, {
    cwd: root,
    env,
    operation: `provision ${name} on ${worker}`,
  });
}

export function provisionRouteSigningSecrets(options, {
  runtime = createSpawnRuntime(),
  environment = process.env,
} = {}) {
  runtime.assertReceiptAvailable(options.receipt);
  const config = validateProvisioningConfigs(
    runtime.readText(options.controlPlaneConfig),
    runtime.readText(options.emailEdgeConfig),
  );
  const wranglerInspectionEnv = sanitizedWranglerInspectionEnvironment(
    environment,
  );
  const wranglerMutationEnv = sanitizedWranglerEnvironment(environment);
  const witselfEnv = sanitizedWitselfEnvironment(environment);
  const remote = {
    controlPlane: inspectWorker(
      runtime,
      CONTROL_PLANE_WORKER,
      options.controlPlaneConfig,
      "control plane",
      wranglerInspectionEnv,
    ),
    emailEdge: inspectWorker(
      runtime,
      EMAIL_EDGE_WORKER,
      options.emailEdgeConfig,
      "email edge",
      wranglerInspectionEnv,
    ),
  };
  assertRemoteDark(remote);

  const routeShow = validateSecretShowEnvelope(
    showSecret(runtime, options.routeSecret, options, witselfEnv),
    options.routeSecret,
    {
      private: options.routePrivateField,
      public: options.routePublicField,
    },
  );
  const tokenShow = validateSecretShowEnvelope(
    showSecret(runtime, options.fallbackSecret, options, witselfEnv),
    options.fallbackSecret,
    { token: options.fallbackField },
  );
  const routePrivateField = routeShow.fields.private;
  const routePublicField = routeShow.fields.public;
  const tokenField = tokenShow.fields.token;
  if (!routePrivateField.sensitive || !routePrivateField.redacted ||
      routePrivateField.encoding !== "utf8" || routePublicField.sensitive ||
      routePublicField.redacted || routePublicField.encoding !== "utf8" ||
      routePublicField.public_value !== config.publicKey ||
      !tokenField.sensitive || !tokenField.redacted ||
      tokenField.encoding !== "utf8") {
    fail("Witself secret field policy does not match the provisioning contract");
  }

  // Both audited reveals finish and every envelope/value is validated before
  // the first Cloudflare mutation. Never pipe an unchecked reveal directly to
  // Wrangler: Wrangler can accept empty stdin as a new secret value.
  const routePrivate = validateRevealEnvelope(
    revealSecret(
      runtime,
      options.routeSecret,
      options.routePrivateField,
      options,
      witselfEnv,
    ),
    { secretID: routeShow.secret.id, field: routePrivateField },
    "route signing private-key reveal",
    { trimOuterWhitespace: true },
  );
  const fallbackToken = validateRevealEnvelope(
    revealSecret(
      runtime,
      options.fallbackSecret,
      options.fallbackField,
      options,
      witselfEnv,
    ),
    { secretID: tokenShow.secret.id, field: tokenField },
    "fallback-token reveal",
  );
  verifyEd25519Keypair(routePrivate, config.publicKey);
  validateFallbackToken(fallbackToken);

  // Reveals and local cryptographic checks can take time. Narrow the race with
  // any independent operator action by re-reading both live Workers and every
  // persistent activation gate immediately before the first secret mutation.
  assertRemoteDark({
    controlPlane: inspectWorker(
      runtime,
      CONTROL_PLANE_WORKER,
      options.controlPlaneConfig,
      "control plane",
      wranglerInspectionEnv,
    ),
    emailEdge: inspectWorker(
      runtime,
      EMAIL_EDGE_WORKER,
      options.emailEdgeConfig,
      "email edge",
      wranglerInspectionEnv,
    ),
  });

  const routePrivateBytes = Buffer.from(routePrivate, "utf8");
  const fallbackTokenBytes = Buffer.from(fallbackToken, "utf8");
  try {
    putWorkerSecret(
      runtime,
      CONTROL_PLANE_WORKER,
      options.controlPlaneConfig,
      ROUTE_PRIVATE_SECRET,
      routePrivateBytes,
      wranglerMutationEnv,
    );
    // The two token puts are intentionally sequential. The live dark-gate
    // preflight above is what makes the temporary mixed-token state safe.
    try {
      putWorkerSecret(
        runtime,
        CONTROL_PLANE_WORKER,
        options.controlPlaneConfig,
        FALLBACK_TOKEN_SECRET,
        fallbackTokenBytes,
        wranglerMutationEnv,
      );
      putWorkerSecret(
        runtime,
        EMAIL_EDGE_WORKER,
        options.emailEdgeConfig,
        FALLBACK_TOKEN_SECRET,
        fallbackTokenBytes,
        wranglerMutationEnv,
      );
    } catch {
      fail(
        "fallback-token provisioning did not complete; all delivery gates were verified dark and must remain dark until this command is rerun successfully and followed by the tagged deploy",
      );
    }
  } finally {
    routePrivateBytes.fill(0);
    fallbackTokenBytes.fill(0);
  }

  try {
    const postWriteRemote = {
      controlPlane: inspectWorker(
        runtime,
        CONTROL_PLANE_WORKER,
        options.controlPlaneConfig,
        "control plane",
        wranglerInspectionEnv,
      ),
      emailEdge: inspectWorker(
        runtime,
        EMAIL_EDGE_WORKER,
        options.emailEdgeConfig,
        "email edge",
        wranglerInspectionEnv,
      ),
    };
    assertProvisionedRemote(postWriteRemote);
  } catch {
    fail(
      "post-write Worker secret verification failed; all delivery gates must remain dark until this command is rerun successfully and followed by the tagged deploy",
    );
  }

  const receipt = Object.freeze({
    schema: "witself.agent-email-secret-provisioning.v1",
    outcome: "provisioned",
    workers: Object.freeze({
      control_plane: CONTROL_PLANE_WORKER,
      email_edge: EMAIL_EDGE_WORKER,
    }),
    operations: Object.freeze({
      control_plane_route_private_key: "succeeded",
      control_plane_fallback_token: "succeeded",
      email_edge_fallback_token: "succeeded",
    }),
    safeguards: Object.freeze({
      existing_workers_verified: true,
      all_delivery_gates_verified_dark: true,
      delivery_gates_reverified_immediately_before_mutation: true,
      route_keypair_verified: true,
      active_key_id_trusted_by_email_edge: true,
      exact_same_fallback_value_uploaded_to_both_targets: true,
      values_written_only_over_stdin: true,
      post_write_bindings_and_inventories_verified: true,
      tagged_redeploy_required: true,
    }),
  });
  if (options.receipt) runtime.writeReceiptExclusive(options.receipt, receipt);
  return receipt;
}

function main() {
  const options = parseProvisioningArgs(process.argv.slice(2));
  const receipt = provisionRouteSigningSecrets(options);
  process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
