import assert from "node:assert/strict";
import { createHash, timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";
import { containerCalls, resetContainerCalls } from "./fixtures/cloudflare-containers-stub.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(left, right) {
      return nodeTimingSafeEqual(Buffer.from(left), Buffer.from(right));
    },
  });
}

const ORIGIN = "https://cp.test.invalid";
const PATH = "/v1/signup-legal-readiness";
const FLEET_TOKEN = "fleet-probe-test-token";
const SCHEMA = "witself.signup-legal-readiness.v1";
const MANIFEST = '{"terms":{"version":"terms-probe-v2","path":"/legal/terms"},"privacy":{"version":"privacy-probe-v3","path":"/legal/privacy"}}\n';
const UNAVAILABLE = {
  schema_version: SCHEMA,
  status: "unavailable",
  error: "signup legal authority is unavailable",
};

function request({ method = "GET", query = "", token = FLEET_TOKEN, signal } = {}) {
  return new Request(`${ORIGIN}${PATH}${query}`, {
    method,
    headers: token === null ? {} : { Authorization: `Bearer ${token}` },
    signal,
    ...(method === "POST" ? { body: "private body must not be read" } : {}),
  });
}

function fixture(t, fetchLegal, overrides = {}) {
  const calls = { legal: 0, forbidden: [] };
  const forbidden = (name) => {
    calls.forbidden.push(name);
    throw new Error("unexpected non-legal operation");
  };
  const env = new Proxy({
    FLEET_TOKEN,
    CP_SIGNUP_LEGAL_ENFORCEMENT: "false",
    LEGAL_DOCUMENTS: { async fetch(incoming) { calls.legal++; return fetchLegal(incoming); } },
    ...overrides,
  }, {
    get(target, name) {
      if (Object.hasOwn(target, name)) return target[name];
      return forbidden(`env.${String(name)}`);
    },
  });
  const ctx = new Proxy({}, { get: (_, name) => forbidden(`ctx.${String(name)}`) });
  t.mock.method(globalThis, "fetch", () => forbidden("global fetch"));
  resetContainerCalls();
  t.after(() => {
    assert.deepEqual(calls.forbidden, [], "no other binding, limiter, context or global fetch access");
    assert.deepEqual(containerCalls, [], "no container fallback");
  });
  return { calls, env, ctx };
}

async function responseBody(response, status) {
  assert.equal(response.status, status);
  assert.equal(response.headers.get("Content-Type"), "application/json");
  assert.equal(response.headers.get("Cache-Control"), "private, no-store");
  assert.equal(response.headers.get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal(response.headers.get("Referrer-Policy"), "no-referrer");
  return response.json();
}

test("legal readiness rejects every other method before authority, body or binding access", async (t) => {
  for (const method of ["POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]) {
    await t.test(method, async (t) => {
      const f = fixture(t, () => assert.fail("legal read"), { FLEET_TOKEN: undefined });
      const incoming = request({ method, token: null, query: "?private=query" });
      const response = await worker.fetch(incoming, f.env, f.ctx);
      assert.equal(response.headers.get("Allow"), "GET");
      assert.deepEqual(await responseBody(response, 405), { schema_version: SCHEMA, error: "method not allowed" });
      assert.equal(incoming.bodyUsed, false);
      assert.equal(f.calls.legal, 0);
    });
  }
});

test("legal readiness requires the configured fleet credential before examining query input", async (t) => {
  for (const [name, token, configured] of [
    ["missing bearer", null, FLEET_TOKEN],
    ["wrong bearer", "wrong-probe-test-token", FLEET_TOKEN],
    ["edge credential", "edge-probe-test-token", FLEET_TOKEN],
    ["admin credential", "admin-probe-test-token", FLEET_TOKEN],
    ["missing configured secret", FLEET_TOKEN, undefined],
    ["empty configured secret", FLEET_TOKEN, ""],
  ]) {
    await t.test(name, async (t) => {
      const f = fixture(t, () => assert.fail("legal read"), { FLEET_TOKEN: configured });
      const incoming = request({ token, query: `?token=${FLEET_TOKEN}` });
      assert.deepEqual(await responseBody(await worker.fetch(incoming, f.env, f.ctx), 401), {
        schema_version: SCHEMA, error: "unauthorized",
      });
      assert.equal(f.calls.legal, 0);
    });
  }
});

test("legal readiness rejects authenticated query arguments without selecting an authority", async (t) => {
  for (const query of ["?url=https://private.invalid", "?version=v2", "?force=true", "?=value"]) {
    await t.test(query, async (t) => {
      const f = fixture(t, () => assert.fail("legal read"));
      assert.deepEqual(await responseBody(await worker.fetch(request({ query }), f.env, f.ctx), 400), {
        schema_version: SCHEMA, error: "query parameters are not allowed",
      });
      assert.equal(f.calls.legal, 0);
    });
  }
});

test("legal readiness reads the fixed service once even with enforcement off and never inherits public caching", async (t) => {
  for (const enforcement of ["false", "true", true, undefined]) {
    await t.test(String(enforcement) + "/" + typeof enforcement, async (t) => {
      const f = fixture(t, (incoming) => {
        assert.equal(incoming.url, "https://legal.internal/legal/versions.json");
        assert.equal(incoming.method, "GET");
        assert.equal(incoming.redirect, "error");
        assert.deepEqual([...incoming.headers], []);
        assert.equal(incoming.body, null);
        assert.equal(incoming.signal.aborted, false);
        return new Response(MANIFEST, { headers: { "Cache-Control": "public, max-age=300", "X-Private": "upstream-sentinel" } });
      }, { CP_SIGNUP_LEGAL_ENFORCEMENT: enforcement });
      const response = await worker.fetch(request(), f.env, f.ctx);
      assert.equal(response.headers.has("X-Private"), false);
      assert.deepEqual(await responseBody(response, 200), {
        schema_version: SCHEMA,
        status: "ready",
        enforcement_enabled: enforcement === "true",
        terms_version: "terms-probe-v2",
        privacy_version: "privacy-probe-v3",
        manifest_sha256: createHash("sha256").update(MANIFEST).digest("hex"),
      });
      assert.equal(f.calls.legal, 1);
    });
  }
});

test("legal readiness maps unavailable authority to one fixed private response without retry", async (t) => {
  for (const [name, fetchLegal, overrides, expectedCalls] of [
    ["missing binding", () => assert.fail("legal read"), { LEGAL_DOCUMENTS: undefined }, 0],
    ["throwing binding", () => { throw new Error("PRIVATE_UPSTREAM_SENTINEL"); }, {}, 1],
    ["non-200 response", () => new Response("PRIVATE_UPSTREAM_SENTINEL", { status: 403 }), {}, 1],
    ["malformed manifest", () => new Response('{"private":"PRIVATE_UPSTREAM_SENTINEL"}'), {}, 1],
  ]) {
    await t.test(name, async (t) => {
      const f = fixture(t, fetchLegal, overrides);
      assert.deepEqual(await responseBody(await worker.fetch(request(), f.env, f.ctx), 503), UNAVAILABLE);
      assert.equal(f.calls.legal, expectedCalls);
    });
  }
});

test("legal readiness passes cancellation to a causally started binding read", { timeout: 2000 }, async (t) => {
  const controller = new AbortController();
  t.after(() => controller.abort());
  let started;
  const entered = new Promise((resolve) => { started = resolve; });
  let release;
  let bindingSignal;
  const f = fixture(t, (incoming) => {
    bindingSignal = incoming.signal;
    started();
    return new Promise((resolve) => { release = resolve; });
  });
  const pending = worker.fetch(request({ signal: controller.signal }), f.env, f.ctx);
  await entered;
  assert.equal(f.calls.legal, 1);
  controller.abort();
  assert.equal(bindingSignal.aborted, true);
  assert.deepEqual(await responseBody(await pending, 503), UNAVAILABLE);
  release(new Response(MANIFEST));
  await Promise.resolve();
  assert.equal(f.calls.legal, 1);
});

test("legal readiness honors an already canceled request without reading the binding", async (t) => {
  const controller = new AbortController();
  controller.abort();
  const f = fixture(t, () => assert.fail("legal read"));
  assert.deepEqual(await responseBody(await worker.fetch(request({ signal: controller.signal }), f.env, f.ctx), 503), UNAVAILABLE);
  assert.equal(f.calls.legal, 0);
});
