import { turnstileEnabled } from "./signup-turnstile.mjs";

export const SIGNUP_CHALLENGE_PATH = "/signup/challenge";

const baseSecurityHeaders = Object.freeze({
  "Cache-Control": "no-store, max-age=0",
  "Cross-Origin-Opener-Policy": "same-origin",
  "Cross-Origin-Resource-Policy": "same-origin",
  "Permissions-Policy":
    "camera=(), geolocation=(), microphone=(), payment=(), usb=()",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "X-Robots-Tag": "noindex, nofollow, noarchive",
});

function htmlEscape(value) {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function nonce() {
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  return [...bytes]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

function securityHeaders(scriptNonce) {
  return {
    ...baseSecurityHeaders,
    "Content-Security-Policy":
      "default-src 'none'; " +
      `script-src 'nonce-${scriptNonce}' https://challenges.cloudflare.com; ` +
      "frame-src https://challenges.cloudflare.com; " +
      "style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; " +
      "frame-ancestors 'none'",
  };
}

function html(siteKey, scriptNonce) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex,nofollow,noarchive">
  <title>Complete signup check - Witself</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: system-ui, sans-serif; max-width: 38rem; margin: 12vh auto 0; padding: 0 1.5rem; line-height: 1.5; }
    h1 { font-size: 1.5rem; line-height: 1.2; }
    pre { overflow-wrap: anywhere; white-space: pre-wrap; background: color-mix(in srgb, currentColor 10%, transparent); border-radius: .25rem; padding: .75rem; user-select: all; }
  </style>
  <script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>
</head>
<body>
  <main>
    <h1>Complete the signup check</h1>
    <p>Complete the challenge, then copy the token below and return to the terminal or AI that opened this page.</p>
    <div class="cf-turnstile" data-sitekey="${htmlEscape(siteKey)}" data-callback="witselfTurnstileComplete"></div>
    <section id="challenge-result" hidden>
      <p>Re-run account creation with <code>--challenge &lt;token&gt;</code> using this token:</p>
      <pre><code id="challenge-token"></code></pre>
    </section>
  </main>
  <script nonce="${scriptNonce}">function witselfTurnstileComplete(token) {
  document.getElementById("challenge-token").textContent = token;
  document.getElementById("challenge-result").hidden = false;
}</script>
</body>
</html>
`;
}

// signupChallengeResponse mirrors the billing-return-page boundary: null for
// unrelated paths, an owned terminal response for the exact route, and no
// account/session state. The route remains a plain 404 until all runtime-only
// Turnstile material is present.
export function signupChallengeResponse(
  request,
  url = new URL(request.url),
  env = {},
) {
  if (url.pathname !== SIGNUP_CHALLENGE_PATH) return null;

  const siteKey = env.CP_SIGNUP_TURNSTILE_SITE_KEY;
  if (
    !turnstileEnabled(env) ||
    typeof siteKey !== "string" ||
    siteKey.length < 1
  ) {
    return new Response("not found\n", {
      status: 404,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }

  const scriptNonce = nonce();
  const headers = securityHeaders(scriptNonce);
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("method not allowed\n", {
      status: 405,
      headers: {
        ...headers,
        Allow: "GET, HEAD",
        "Content-Type": "text/plain; charset=utf-8",
      },
    });
  }

  return new Response(
    request.method === "HEAD" ? null : html(siteKey, scriptNonce),
    {
      status: 200,
      headers: {
        ...headers,
        "Content-Type": "text/html; charset=utf-8",
      },
    },
  );
}
