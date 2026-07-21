export const RELAY_SIGNATURE_VERSION = "witself-email-relay-pilot-v1";
export const PILOT_MAXIMUM_RAW_BYTES = 5 * 1024 * 1024;

const textEncoder = new TextEncoder();
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const AUDIENCE = /^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/;
const LOWER_SHA256 = /^[0-9a-f]{64}$/;

export function normalizeEnvelopeAddress(value, allowEmpty = false) {
  if (typeof value !== "string") throw new Error("invalid envelope address");
  let address = value.trim().toLowerCase();
  if (address.startsWith("<") && address.endsWith(">")) {
    address = address.slice(1, -1).trim();
  }
  if (address === "<>" || (allowEmpty && address === "")) return "";
  if (
    address.length < 3 ||
    new TextEncoder().encode(address).byteLength > 320 ||
    address.split("@").length !== 2 ||
    /[\x00-\x1f\x7f]/.test(address)
  ) {
    throw new Error("invalid envelope address");
  }
  const [local, domain] = address.split("@");
  if (!local || !domain || /[ <>\t]/.test(domain)) throw new Error("invalid envelope address");
  return address;
}

export function normalizeRelayMetadata(metadata) {
  const normalized = {
    timestamp: Number(metadata?.timestamp),
    keyId: String(metadata?.keyId ?? "").trim().toLowerCase(),
    envelopeFrom: normalizeEnvelopeAddress(String(metadata?.envelopeFrom ?? ""), true),
    envelopeTo: normalizeEnvelopeAddress(String(metadata?.envelopeTo ?? ""), false),
    audience: String(metadata?.audience ?? "").trim().toLowerCase(),
    rawSize: Number(metadata?.rawSize),
    rawSHA256: String(metadata?.rawSHA256 ?? "").trim().toLowerCase(),
  };
  if (!Number.isSafeInteger(normalized.timestamp) || normalized.timestamp <= 0) {
    throw new Error("invalid relay timestamp");
  }
  if (!KEY_ID.test(normalized.keyId)) throw new Error("invalid relay key id");
  if (!AUDIENCE.test(normalized.audience) || normalized.audience.length > 128) {
    throw new Error("invalid relay audience");
  }
  if (
    !Number.isSafeInteger(normalized.rawSize) ||
    normalized.rawSize < 1 ||
    normalized.rawSize > PILOT_MAXIMUM_RAW_BYTES
  ) {
    throw new Error("invalid relay raw size");
  }
  if (!LOWER_SHA256.test(normalized.rawSHA256)) throw new Error("invalid relay digest");
  return normalized;
}

export function base64URL(bytes) {
  let binary = "";
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  for (let offset = 0; offset < view.length; offset += 0x8000) {
    binary += String.fromCharCode(...view.subarray(offset, offset + 0x8000));
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

export function base64Standard(bytes) {
  let binary = "";
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  for (let offset = 0; offset < view.length; offset += 0x8000) {
    binary += String.fromCharCode(...view.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

function base64Decode(value) {
  const compact = value.replace(/\s+/g, "");
  const binary = atob(compact);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

export function canonicalSignatureInput(metadata) {
  const value = normalizeRelayMetadata(metadata);
  const fields = [
    RELAY_SIGNATURE_VERSION,
    String(value.timestamp),
    value.keyId,
    base64URL(textEncoder.encode(value.envelopeFrom)),
    base64URL(textEncoder.encode(value.envelopeTo)),
    base64URL(textEncoder.encode(value.audience)),
    String(value.rawSize),
    `sha256:${value.rawSHA256}`,
  ];
  return textEncoder.encode(`${fields.join("\n")}\n`);
}

export function decodePKCS8Secret(value) {
  if (typeof value !== "string" || value.trim() === "") throw new Error("relay key unavailable");
  let encoded = value.trim();
  if (encoded.includes("-----BEGIN PRIVATE KEY-----")) {
    encoded = encoded
      .replace("-----BEGIN PRIVATE KEY-----", "")
      .replace("-----END PRIVATE KEY-----", "")
      .replace(/\s+/g, "");
  }
  const decoded = base64Decode(encoded);
  if (decoded.byteLength < 40 || decoded.byteLength > 128) throw new Error("relay key unavailable");
  return decoded;
}

export async function importSigningKey(pkcs8Secret, cryptoAPI = crypto) {
  return cryptoAPI.subtle.importKey(
    "pkcs8",
    decodePKCS8Secret(pkcs8Secret),
    { name: "Ed25519" },
    false,
    ["sign"],
  );
}

export async function sha256Hex(bytes, cryptoAPI = crypto) {
  const digest = await cryptoAPI.subtle.digest("SHA-256", bytes);
  return Array.from(new Uint8Array(digest), (value) => value.toString(16).padStart(2, "0")).join("");
}

export async function signRelay(metadata, privateKey, cryptoAPI = crypto) {
  const input = canonicalSignatureInput(metadata);
  const signature = await cryptoAPI.subtle.sign({ name: "Ed25519" }, privateKey, input);
  return { input, signature: new Uint8Array(signature) };
}
