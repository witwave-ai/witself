export const AGENT_EMAIL_ROUTE_LEGACY_UNSIGNED_SCHEMA_VERSION = 1;
export const AGENT_EMAIL_ROUTE_UNSIGNED_SCHEMA_VERSION = 2;
export const AGENT_EMAIL_ROUTE_LEGACY_SIGNED_SCHEMA_VERSION = 2;
export const AGENT_EMAIL_ROUTE_SIGNED_SCHEMA_VERSION = 3;
export const AGENT_EMAIL_ROUTE_SIGNATURE_VERSION =
  "witself-agent-email-route-projection-v1";

const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const SIGNATURE = /^[A-Za-z0-9+/]{86}==$/;
const PUBLIC_KEY_BYTES = 32;
const MAX_PUBLIC_KEYS = 4;
const SIGNATURE_FIELD = "route_signature";
const KEY_ID_FIELD = "route_signing_key_id";
const textEncoder = new TextEncoder();

function base64Decode(value) {
  let binary;
  try {
    binary = atob(value);
  } catch {
    throw new Error("agent email route signature encoding is invalid");
  }
  const decoded = Uint8Array.from(
    binary,
    (character) => character.charCodeAt(0),
  );
  let canonical = "";
  for (let offset = 0; offset < decoded.length; offset += 0x8000) {
    canonical += String.fromCharCode(...decoded.subarray(offset, offset + 0x8000));
  }
  if (btoa(canonical) !== value) {
    throw new Error("agent email route signature encoding is invalid");
  }
  return decoded;
}

function canonicalScalarObject(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("signed agent email route projection is invalid");
  }
  const entries = Object.entries(value).sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0);
  if (entries.some(([, item]) =>
    !["string", "number", "boolean"].includes(typeof item) ||
    (typeof item === "number" && !Number.isSafeInteger(item)))) {
    throw new Error("signed agent email route projection is invalid");
  }
  return `{${entries.map(([key, item]) =>
    `${JSON.stringify(key)}:${JSON.stringify(item)}`).join(",")}}`;
}

export function agentEmailRouteSignaturePayload(projection, keyID) {
  if (!projection || typeof projection !== "object" ||
      Array.isArray(projection) ||
      ![
        AGENT_EMAIL_ROUTE_LEGACY_UNSIGNED_SCHEMA_VERSION,
        AGENT_EMAIL_ROUTE_UNSIGNED_SCHEMA_VERSION,
      ].includes(projection.schema_version) ||
      Object.hasOwn(projection, KEY_ID_FIELD) ||
      Object.hasOwn(projection, SIGNATURE_FIELD)) {
    throw new Error("agent email route projection is invalid");
  }
  if (typeof keyID !== "string" || !KEY_ID.test(keyID)) {
    throw new Error("agent email route signing key id is invalid");
  }
  canonicalScalarObject(projection);
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

function unsignedProjection(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      ![
        AGENT_EMAIL_ROUTE_LEGACY_SIGNED_SCHEMA_VERSION,
        AGENT_EMAIL_ROUTE_SIGNED_SCHEMA_VERSION,
      ].includes(value.schema_version) ||
      typeof value[KEY_ID_FIELD] !== "string" ||
      !KEY_ID.test(value[KEY_ID_FIELD]) ||
      typeof value[SIGNATURE_FIELD] !== "string" ||
      !SIGNATURE.test(value[SIGNATURE_FIELD])) {
    throw new Error("signed agent email route projection is invalid");
  }
  const projection = structuredClone(value);
  const unsignedSchemaVersion = value.schema_version - 1;
  delete projection[KEY_ID_FIELD];
  delete projection[SIGNATURE_FIELD];
  projection.schema_version = unsignedSchemaVersion;
  canonicalScalarObject(projection);
  return projection;
}

let cachedPublicKeyConfig = null;
let cachedCryptoAPI = null;
let cachedPublicKeys = null;

async function publicKeys(rawConfig, cryptoAPI) {
  if (rawConfig === cachedPublicKeyConfig && cryptoAPI === cachedCryptoAPI &&
      cachedPublicKeys !== null) {
    return cachedPublicKeys;
  }
  cachedPublicKeyConfig = rawConfig;
  cachedCryptoAPI = cryptoAPI;
  cachedPublicKeys = (async () => {
    if (rawConfig.length < 1 || rawConfig.length > 4_096) {
      throw new Error("agent email route verification keyring is invalid");
    }
    let parsed;
    try {
      parsed = JSON.parse(rawConfig);
    } catch {
      throw new Error("agent email route verification keyring is invalid");
    }
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new Error("agent email route verification keyring is invalid");
    }
    // One lexical, compact JSON representation rejects duplicate keys,
    // ambiguous whitespace, and ordering drift before configuration can
    // silently collapse to a different keyring.
    const canonical = Object.fromEntries(
      Object.entries(parsed).sort(([left], [right]) =>
        left < right ? -1 : left > right ? 1 : 0),
    );
    if (JSON.stringify(canonical) !== rawConfig) {
      throw new Error("agent email route verification keyring is invalid");
    }
    const entries = Object.entries(parsed);
    if (entries.length < 1 || entries.length > MAX_PUBLIC_KEYS ||
        entries.some(([keyID, encoded]) =>
          !KEY_ID.test(keyID) || typeof encoded !== "string")) {
      throw new Error("agent email route verification keyring is invalid");
    }
    const keys = new Map();
    for (const [keyID, encoded] of entries) {
      const raw = base64Decode(encoded);
      if (raw.byteLength !== PUBLIC_KEY_BYTES) {
        throw new Error("agent email route verification keyring is invalid");
      }
      let key;
      try {
        key = await cryptoAPI.subtle.importKey(
          "raw",
          raw,
          { name: "Ed25519" },
          false,
          ["verify"],
        );
      } catch {
        throw new Error("agent email route verification keyring is invalid");
      }
      keys.set(keyID, key);
    }
    return keys;
  })();
  return cachedPublicKeys;
}

export async function verifyAgentEmailRouteProjectionSignature(
  value,
  env = {},
  cryptoAPI = crypto,
) {
  const projection = unsignedProjection(value);
  const keyID = value[KEY_ID_FIELD];
  const rawConfig = String(
    env.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS ?? "",
  );
  const keys = await publicKeys(rawConfig, cryptoAPI);
  const key = keys.get(keyID);
  if (!key) {
    throw new Error("agent email route signing key is not trusted");
  }
  const signature = base64Decode(value[SIGNATURE_FIELD]);
  if (signature.byteLength !== 64) {
    throw new Error("agent email route signature encoding is invalid");
  }
  let verified = false;
  try {
    verified = await cryptoAPI.subtle.verify(
      { name: "Ed25519" },
      key,
      signature,
      agentEmailRouteSignatureInput(projection, keyID),
    );
  } catch {
    throw new Error("agent email route signature verification failed");
  }
  if (!verified) {
    throw new Error("agent email route signature verification failed");
  }
  return projection;
}
