export const TURNSTILE_VERIFY_URL =
  "https://challenges.cloudflare.com/turnstile/v0/siteverify";

const TURNSTILE_TIMEOUT_MS = 5_000;
const MAX_TOKEN_LENGTH = 4_096;
const INVALID_TOKEN_CODES = new Set([
  "invalid-input-response",
  "missing-input-response",
  "timeout-or-duplicate",
]);

export function turnstileEnabled(env) {
  return env?.CP_SIGNUP_TURNSTILE_ENABLED === "true" &&
    typeof env?.CP_SIGNUP_TURNSTILE_SECRET_KEY === "string" &&
    env.CP_SIGNUP_TURNSTILE_SECRET_KEY.length > 0;
}

// Siteverify is fail-closed: a provider or transport failure pauses signup,
// while a definitive negative verdict asks the caller to complete a fresh
// challenge. No returned object includes the token, secret, or remote IP.
export async function verifyTurnstileToken({
  secretKey,
  token,
  remoteIp,
  fetchImpl = (...args) => globalThis.fetch(...args),
}) {
  if (
    typeof token !== "string" ||
    token.length < 1 ||
    token.length > MAX_TOKEN_LENGTH
  ) {
    return { ok: false, reason: "invalid" };
  }
  if (
    typeof secretKey !== "string" ||
    secretKey.length < 1 ||
    typeof fetchImpl !== "function"
  ) {
    return { ok: false, reason: "unavailable" };
  }

  const form = new URLSearchParams({
    secret: secretKey,
    response: token,
  });
  if (typeof remoteIp === "string" && remoteIp.length > 0) {
    form.set("remoteip", remoteIp);
  }

  let response;
  try {
    response = await fetchImpl(TURNSTILE_VERIFY_URL, {
      method: "POST",
      body: form,
      signal: AbortSignal.timeout(TURNSTILE_TIMEOUT_MS),
    });
  } catch {
    return { ok: false, reason: "unavailable" };
  }
  if (!response) {
    return { ok: false, reason: "unavailable" };
  }
  if (
    !response.ok &&
    (response.status < 400 || response.status >= 500)
  ) {
    return { ok: false, reason: "unavailable" };
  }

  let result;
  try {
    result = await response.json();
  } catch {
    return { ok: false, reason: "unavailable" };
  }
  if (
    !result ||
    typeof result !== "object" ||
    Array.isArray(result) ||
    typeof result.success !== "boolean" ||
    (
      result["error-codes"] !== undefined &&
      (
        !Array.isArray(result["error-codes"]) ||
        result["error-codes"].some((code) => typeof code !== "string")
      )
    )
  ) {
    return { ok: false, reason: "unavailable" };
  }
  const errorCodes = result["error-codes"];
  if (result.success) {
    return response.ok &&
        (!Array.isArray(errorCodes) || errorCodes.length === 0)
      ? { ok: true }
      : { ok: false, reason: "unavailable" };
  }
  if (
    !Array.isArray(errorCodes) ||
    errorCodes.length === 0 ||
    errorCodes.some((code) => !INVALID_TOKEN_CODES.has(code))
  ) {
    return { ok: false, reason: "unavailable" };
  }
  return { ok: false, reason: "invalid" };
}
