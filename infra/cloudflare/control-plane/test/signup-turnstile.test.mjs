import assert from "node:assert/strict";
import test from "node:test";

import {
  SIGNUP_CHALLENGE_PATH,
  signupChallengeResponse,
} from "../src/signup-challenge-page.mjs";
import {
  TURNSTILE_VERIFY_URL,
  turnstileEnabled,
  verifyTurnstileToken,
} from "../src/signup-turnstile.mjs";

const enabledEnv = {
  CP_SIGNUP_TURNSTILE_ENABLED: "true",
  CP_SIGNUP_TURNSTILE_SECRET_KEY: "secret-key",
  CP_SIGNUP_TURNSTILE_SITE_KEY: "site-key",
};

test("Turnstile requires the exact gate value and a runtime secret", () => {
  assert.equal(turnstileEnabled(enabledEnv), true);
  for (const env of [
    {},
    { CP_SIGNUP_TURNSTILE_ENABLED: "true" },
    { CP_SIGNUP_TURNSTILE_ENABLED: "TRUE", CP_SIGNUP_TURNSTILE_SECRET_KEY: "secret" },
    { CP_SIGNUP_TURNSTILE_ENABLED: true, CP_SIGNUP_TURNSTILE_SECRET_KEY: "secret" },
    { CP_SIGNUP_TURNSTILE_ENABLED: "true", CP_SIGNUP_TURNSTILE_SECRET_KEY: "" },
  ]) {
    assert.equal(turnstileEnabled(env), false);
  }
});

test("missing and oversized tokens are invalid without a provider call", async () => {
  let calls = 0;
  const verify = (token) => verifyTurnstileToken({
    secretKey: "secret",
    token,
    remoteIp: "203.0.113.1",
    fetchImpl: async () => {
      calls++;
      return Response.json({ success: true });
    },
  });
  assert.deepEqual(await verify(""), { ok: false, reason: "invalid" });
  assert.deepEqual(await verify(undefined), { ok: false, reason: "invalid" });
  assert.deepEqual(
    await verify("x".repeat(4097)),
    { ok: false, reason: "invalid" },
  );
  assert.equal(calls, 0);
});

test("verification posts the secret, token, and optional remote IP once", async () => {
  let call = null;
  const result = await verifyTurnstileToken({
    secretKey: "server-secret",
    token: "challenge-token",
    remoteIp: "2001:db8::1",
    fetchImpl: async (url, init) => {
      call = { url, init };
      return Response.json({ success: true, hostname: "self.witwave.ai" });
    },
  });
  assert.deepEqual(result, { ok: true });
  assert.equal(call.url, TURNSTILE_VERIFY_URL);
  assert.equal(call.init.method, "POST");
  assert.ok(call.init.signal instanceof AbortSignal);
  assert.deepEqual(Object.fromEntries(call.init.body), {
    secret: "server-secret",
    response: "challenge-token",
    remoteip: "2001:db8::1",
  });
});

test("negative and duplicate provider verdicts are invalid", async () => {
  for (const body of [
    { success: false, "error-codes": ["missing-input-response"] },
    { success: false, "error-codes": ["invalid-input-response"] },
    { success: false, "error-codes": ["timeout-or-duplicate"] },
  ]) {
    assert.deepEqual(await verifyTurnstileToken({
      secretKey: "secret",
      token: "token",
      fetchImpl: async () => Response.json(body),
    }), { ok: false, reason: "invalid" });
  }
});

test("transport, 5xx, and malformed responses are unavailable", async () => {
  const cases = [
    async () => {
      throw new Error("network down");
    },
    async () => Response.json({ error: "down" }, { status: 503 }),
    async () => Response.json({ error: "bad request" }, { status: 400 }),
    async () => new Response("not-json"),
    async () => Response.json({}),
    async () => Response.json({ success: "true" }),
    async () => Response.json({ success: false }),
    async () => Response.json({ success: false, "error-codes": [] }),
    async () => Response.json({
      success: true,
      "error-codes": ["internal-error"],
    }),
    async () => Response.json({
      success: false,
      "error-codes": ["invalid-input-secret"],
    }),
    async () => Response.json({
      success: false,
      "error-codes": ["internal-error"],
    }),
    async () => Response.json({
      success: false,
      "error-codes": ["unknown-provider-failure"],
    }),
  ];
  for (const fetchImpl of cases) {
    assert.deepEqual(await verifyTurnstileToken({
      secretKey: "secret",
      token: "token",
      fetchImpl,
    }), { ok: false, reason: "unavailable" });
  }
});

test("well-formed token 4xx responses are invalid challenges", async () => {
  assert.deepEqual(await verifyTurnstileToken({
    secretKey: "secret",
    token: "token",
    fetchImpl: async () => Response.json({
      success: false,
      "error-codes": ["invalid-input-response"],
    }, { status: 400 }),
  }), { ok: false, reason: "invalid" });
});

test("non-HTTP failure statuses remain unavailable", async () => {
  for (const response of [
    Response.redirect("https://example.test/", 302),
    Response.error(),
  ]) {
    assert.deepEqual(await verifyTurnstileToken({
      secretKey: "secret",
      token: "token",
      fetchImpl: async () => response,
    }), { ok: false, reason: "unavailable" });
  }
});

test("the challenge route stays dark until gate, secret, and site key exist", async () => {
  const request = new Request(`https://self.witwave.ai${SIGNUP_CHALLENGE_PATH}`);
  for (const env of [
    {},
    { CP_SIGNUP_TURNSTILE_ENABLED: "true" },
    {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "secret",
    },
  ]) {
    const response = signupChallengeResponse(request, undefined, env);
    assert.equal(response.status, 404);
    assert.equal(await response.text(), "not found\n");
  }
  assert.equal(
    signupChallengeResponse(
      new Request("https://self.witwave.ai/other"),
      undefined,
      enabledEnv,
    ),
    null,
  );
});

test("the enabled challenge page escapes the key and freezes its boundary", async () => {
  const siteKey = 'site"><img src=x onerror=alert(1)>';
  const response = signupChallengeResponse(
    new Request(
      `https://self.witwave.ai${SIGNUP_CHALLENGE_PATH}?token=must-not-echo`,
    ),
    undefined,
    { ...enabledEnv, CP_SIGNUP_TURNSTILE_SITE_KEY: siteKey },
  );
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("cache-control"), "no-store, max-age=0");
  assert.equal(response.headers.get("x-frame-options"), "DENY");
  assert.equal(response.headers.get("referrer-policy"), "no-referrer");
  const csp = response.headers.get("content-security-policy");
  assert.match(csp, /script-src .*https:\/\/challenges\.cloudflare\.com/);
  assert.match(csp, /frame-src https:\/\/challenges\.cloudflare\.com/);
  assert.match(csp, /frame-ancestors 'none'/);
  const body = await response.text();
  assert.match(body, /turnstile\/v0\/api\.js/);
  assert.match(body, /data-sitekey="site&quot;&gt;&lt;img/);
  assert.doesNotMatch(body, /<img src=x/);
  assert.doesNotMatch(body, /must-not-echo/);
  assert.match(body, /id="challenge-token"/);
  assert.match(body, /--challenge &lt;token&gt;/);
  assert.equal(response.headers.has("set-cookie"), false);
  assert.equal(response.headers.has("location"), false);
});

test("challenge HEAD and method rejection retain the safe page boundary", async () => {
  const url = `https://self.witwave.ai${SIGNUP_CHALLENGE_PATH}`;
  const head = signupChallengeResponse(
    new Request(url, { method: "HEAD" }),
    undefined,
    enabledEnv,
  );
  assert.equal(head.status, 200);
  assert.equal(await head.text(), "");
  assert.match(
    head.headers.get("content-security-policy"),
    /challenges\.cloudflare\.com/,
  );

  const post = signupChallengeResponse(
    new Request(url, { method: "POST" }),
    undefined,
    enabledEnv,
  );
  assert.equal(post.status, 405);
  assert.equal(post.headers.get("allow"), "GET, HEAD");
  assert.equal(await post.text(), "method not allowed\n");
});
