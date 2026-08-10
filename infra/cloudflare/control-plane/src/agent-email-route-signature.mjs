export const AGENT_EMAIL_ROUTE_LEGACY_UNSIGNED_SCHEMA_VERSION = 1;
export const AGENT_EMAIL_ROUTE_UNSIGNED_SCHEMA_VERSION = 2;
export const AGENT_EMAIL_ROUTE_LEGACY_SIGNED_SCHEMA_VERSION = 2;
export const AGENT_EMAIL_ROUTE_SIGNED_SCHEMA_VERSION = 3;
export const AGENT_EMAIL_ROUTE_SIGNATURE_VERSION =
  "witself-agent-email-route-projection-v1";

const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const SIGNATURE_FIELD = "route_signature";
const KEY_ID_FIELD = "route_signing_key_id";
const textEncoder = new TextEncoder();

export class AgentEmailRouteSigningError extends Error {
  constructor(message = "agent email route signing is unavailable") {
    super(message);
    this.name = "AgentEmailRouteSigningError";
  }
}

function fail(message) {
  throw new AgentEmailRouteSigningError(message);
}

function base64Decode(value, label) {
  if (typeof value !== "string" || value.trim() === "" ||
      value.length > 4_096) fail(label);
  let encoded = value.trim();
  if (encoded.includes("-----BEGIN PRIVATE KEY-----")) {
    encoded = encoded
      .replace("-----BEGIN PRIVATE KEY-----", "")
      .replace("-----END PRIVATE KEY-----", "")
      .replace(/\s+/g, "");
  }
  let binary;
  try {
    binary = atob(encoded);
  } catch {
    fail(label);
  }
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function base64Standard(bytes) {
  let binary = "";
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  for (let offset = 0; offset < view.length; offset += 0x8000) {
    binary += String.fromCharCode(...view.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

function canonicalScalarObject(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    fail("agent email route projection is invalid");
  }
  const entries = Object.entries(value).sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0);
  if (entries.some(([, item]) =>
    !["string", "number", "boolean"].includes(typeof item) ||
    (typeof item === "number" && !Number.isSafeInteger(item)))) {
    fail("agent email route projection is invalid");
  }
  return `{${entries.map(([key, item]) =>
    `${JSON.stringify(key)}:${JSON.stringify(item)}`).join(",")}}`;
}

function validateUnsignedProjection(projection) {
  if (!projection || typeof projection !== "object" ||
      Array.isArray(projection) ||
      ![
        AGENT_EMAIL_ROUTE_LEGACY_UNSIGNED_SCHEMA_VERSION,
        AGENT_EMAIL_ROUTE_UNSIGNED_SCHEMA_VERSION,
      ].includes(projection.schema_version) ||
      Object.hasOwn(projection, KEY_ID_FIELD) ||
      Object.hasOwn(projection, SIGNATURE_FIELD)) {
    fail("agent email route projection is invalid");
  }
  canonicalScalarObject(projection);
  return projection;
}

export function agentEmailRouteSignaturePayload(projection, keyID) {
  validateUnsignedProjection(projection);
  if (typeof keyID !== "string" || !KEY_ID.test(keyID)) {
    fail("agent email route signing key id is invalid");
  }
  return {
    ...structuredClone(projection),
    schema_version: projection.schema_version + 1,
    [KEY_ID_FIELD]: keyID,
  };
}

export function agentEmailRouteSignatureInput(projection, keyID) {
  const payload = agentEmailRouteSignaturePayload(projection, keyID);
  return textEncoder.encode(
    `${AGENT_EMAIL_ROUTE_SIGNATURE_VERSION}\n${canonicalScalarObject(payload)}\n`,
  );
}

let cachedSecret = null;
let cachedCryptoAPI = null;
let cachedSigningKey = null;

async function signingKey(secret, cryptoAPI) {
  if (secret !== cachedSecret || cryptoAPI !== cachedCryptoAPI ||
      cachedSigningKey === null) {
    cachedSecret = secret;
    cachedCryptoAPI = cryptoAPI;
    cachedSigningKey = (async () => {
      const decoded = base64Decode(
        secret,
        "agent email route signing private key is unavailable",
      );
      if (decoded.byteLength < 40 || decoded.byteLength > 128) {
        fail("agent email route signing private key is unavailable");
      }
      try {
        return await cryptoAPI.subtle.importKey(
          "pkcs8",
          decoded,
          { name: "Ed25519" },
          false,
          ["sign"],
        );
      } catch {
        fail("agent email route signing private key is unavailable");
      }
    })();
  }
  return cachedSigningKey;
}

export async function signAgentEmailRouteProjection(
  projection,
  env = {},
  cryptoAPI = crypto,
) {
  const keyID = String(env.AGENT_EMAIL_ROUTE_SIGNING_KEY_ID ?? "");
  if (!KEY_ID.test(keyID)) {
    fail("agent email route signing key id is unavailable");
  }
  const secret = String(env.AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY ?? "");
  const key = await signingKey(secret, cryptoAPI);
  const payload = agentEmailRouteSignaturePayload(projection, keyID);
  let signature;
  try {
    signature = await cryptoAPI.subtle.sign(
      { name: "Ed25519" },
      key,
      agentEmailRouteSignatureInput(projection, keyID),
    );
  } catch {
    fail("agent email route signing failed");
  }
  return {
    ...payload,
    [SIGNATURE_FIELD]: base64Standard(signature),
  };
}
