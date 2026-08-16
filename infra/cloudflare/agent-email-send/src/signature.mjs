export const DISPATCH_SCHEMA = "witself.agent-email-dispatch.v1";
export const RESPONSE_SCHEMA = "witself.agent-email-dispatch-response.v1";
export const RECEIPT_PROOF_SCHEMA =
  "witself.agent-email-dispatch-receipt-proof.v1";
export const RECEIPT_REPLAY_AUDIENCE =
  "witself-agent-email-send-receipt-replay";

export const HEADERS = Object.freeze({
  version: "X-Witself-Email-Dispatch-Version",
  timestamp: "X-Witself-Email-Dispatch-Timestamp",
  keyId: "X-Witself-Email-Dispatch-Key-Id",
  audience: "X-Witself-Email-Dispatch-Audience",
  digest: "X-Witself-Email-Dispatch-SHA256",
  signature: "X-Witself-Email-Dispatch-Signature",
});

const KEY_ID = /^[a-z][a-z0-9_.-]{0,63}$/;
const SHA256 = /^[0-9a-f]{64}$/;

export function signatureInput({ version, timestamp, keyId, audience, digest }) {
  if (
    version !== DISPATCH_SCHEMA ||
    !KEY_ID.test(keyId) ||
    typeof audience !== "string" ||
    audience.length < 1 ||
    audience !== audience.trim() ||
    /[\r\n]/.test(audience) ||
    !SHA256.test(digest) ||
    !Number.isFinite(Date.parse(timestamp))
  ) {
    throw new Error("invalid dispatch signature envelope");
  }
  return new TextEncoder().encode(
    [
      "witself.agent-email-dispatch-signature.v1",
      version,
      timestamp,
      keyId,
      audience,
      digest,
    ].join("\n"),
  );
}

export async function sha256Hex(value, cryptoAPI = crypto) {
  const bytes =
    value instanceof Uint8Array ? value : new TextEncoder().encode(value);
  const digest = new Uint8Array(
    await cryptoAPI.subtle.digest("SHA-256", bytes),
  );
  return [...digest].map((item) => item.toString(16).padStart(2, "0")).join("");
}

function decodeBase64(value) {
  if (typeof value !== "string" || value.length < 1) {
    throw new Error("invalid base64 value");
  }
  const binary = atob(value);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

export function parseSignerRing(raw) {
  let parsed;
  try {
    parsed = JSON.parse(String(raw ?? ""));
  } catch {
    throw new Error("dispatch signer ring is invalid");
  }
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error("dispatch signer ring is invalid");
  }
  const entries = Object.entries(parsed);
  if (entries.length < 1 || entries.length > 8) {
    throw new Error("dispatch signer ring must contain 1-8 entries");
  }
  const result = new Map();
  for (const [keyId, value] of entries) {
    if (
      !KEY_ID.test(keyId) ||
      !value ||
      typeof value !== "object" ||
      Array.isArray(value) ||
      typeof value.public_key !== "string" ||
      !Array.isArray(value.account_ids) ||
      value.account_ids.length < 1 ||
      value.account_ids.length > 100 ||
      !value.account_ids.every((id) => /^acc_[A-Za-z0-9_-]{1,128}$/.test(id))
    ) {
      throw new Error("dispatch signer ring entry is invalid");
    }
    const publicKey = decodeBase64(value.public_key);
    if (publicKey.length !== 32) {
      throw new Error("dispatch signer public key is invalid");
    }
    result.set(keyId, {
      publicKey,
      accountIds: new Set(value.account_ids),
    });
  }
  return result;
}

// The signature covers the complete body digest supplied in the headers. Verify
// that authenticated envelope before reading the body so an untrusted caller
// cannot make the Worker buffer or hash up to the full request limit.
export async function verifyDispatchHeaders(
  request,
  env,
  {
    cryptoAPI = crypto,
    now = Date.now,
    expectedAudience = env.DISPATCH_AUDIENCE,
  } = {},
) {
  const version = request.headers.get(HEADERS.version) ?? "";
  const timestamp = request.headers.get(HEADERS.timestamp) ?? "";
  const keyId = request.headers.get(HEADERS.keyId) ?? "";
  const audience = request.headers.get(HEADERS.audience) ?? "";
  const digest = request.headers.get(HEADERS.digest) ?? "";
  const signatureText = request.headers.get(HEADERS.signature) ?? "";
  if (
    typeof expectedAudience !== "string" ||
    expectedAudience.length < 1 ||
    audience !== expectedAudience
  ) {
    throw new Error("dispatch audience is invalid");
  }
  const parsedTime = Date.parse(timestamp);
  const replayWindow = Number(env.DISPATCH_REPLAY_WINDOW_SECONDS ?? 300);
  if (
    !Number.isInteger(replayWindow) ||
    replayWindow < 30 ||
    replayWindow > 900 ||
    !Number.isFinite(parsedTime) ||
    Math.abs(now() - parsedTime) > replayWindow * 1000
  ) {
    throw new Error("dispatch timestamp is outside the replay window");
  }
  const signer = parseSignerRing(env.DISPATCH_SIGNERS_JSON).get(keyId);
  if (!signer) throw new Error("dispatch signing key is not trusted");
  const signature = decodeBase64(signatureText);
  if (signature.length !== 64) throw new Error("dispatch signature is invalid");
  const key = await cryptoAPI.subtle.importKey(
    "raw",
    signer.publicKey,
    { name: "Ed25519" },
    false,
    ["verify"],
  );
  const verified = await cryptoAPI.subtle.verify(
    { name: "Ed25519" },
    key,
    signature,
    signatureInput({ version, timestamp, keyId, audience, digest }),
  );
  if (!verified) throw new Error("dispatch signature is invalid");
  return { keyId, signer, digest };
}

// Header authentication does not make the body trustworthy. The complete
// bounded body must still match the signed digest before JSON parsing,
// account admission, Durable Object lookup, or provider dispatch.
export async function verifyDispatchBodyDigest(
  body,
  expectedDigest,
  cryptoAPI = crypto,
) {
  if (!SHA256.test(expectedDigest)) {
    throw new Error("dispatch body digest is invalid");
  }
  const actualDigest = await sha256Hex(body, cryptoAPI);
  if (expectedDigest !== actualDigest) {
    throw new Error("dispatch body digest does not match");
  }
}
