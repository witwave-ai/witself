import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

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
const FLEET_TOKEN = "fleet-test-token";
const SECURITY_HEADERS = Object.freeze({
  "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
  "X-Content-Type-Options": "nosniff",
  "Referrer-Policy": "no-referrer",
});

class DirectoryFake {
  constructor() {
    this.values = new Map([
      ["acct:public-account", JSON.stringify({
        cell: "cell-a",
        endpoint: "https://cell-a.test.invalid",
        region: "test",
        region_code: "tst1",
      })],
    ]);
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async list({ prefix = "" } = {}) {
    return {
      keys: [...this.values.keys()]
        .filter((key) => key.startsWith(prefix))
        .map((name) => ({ name })),
      list_complete: true,
    };
  }
}

const env = {
  DIRECTORY: new DirectoryFake(),
  FLEET_TOKEN,
};

function assertSecurityHeaders(response) {
  for (const [name, value] of Object.entries(SECURITY_HEADERS)) {
    assert.equal(response.headers.get(name), value, name);
  }
}

test("fleet-authenticated JSON responses carry all security headers", async () => {
  const response = await worker.fetch(
    new Request(`${ORIGIN}/v1/invites`, {
      headers: { Authorization: `Bearer ${FLEET_TOKEN}` },
    }),
    env,
    { waitUntil() {} },
  );

  assert.equal(response.status, 200);
  assertSecurityHeaders(response);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
});

test("protected pre-auth failures are no-store", async () => {
  const unauthorized = await worker.fetch(
    new Request(`${ORIGIN}/v1/invites`),
    env,
    { waitUntil() {} },
  );
  assert.equal(unauthorized.status, 401);
  assertSecurityHeaders(unauthorized);
  assert.equal(unauthorized.headers.get("Cache-Control"), "no-store");

  const invalidCredential = await worker.fetch(
    new Request(`${ORIGIN}/v1/invites`, {
      headers: { Authorization: "Bearer wrong-fleet-token" },
    }),
    env,
    { waitUntil() {} },
  );
  assert.equal(invalidCredential.status, 401);
  assertSecurityHeaders(invalidCredential);
  assert.equal(invalidCredential.headers.get("Cache-Control"), "no-store");

  const methodRejected = await worker.fetch(
    new Request(`${ORIGIN}/v1/cells/cell-a:evacuate`),
    env,
    { waitUntil() {} },
  );
  assert.equal(methodRejected.status, 405);
  assertSecurityHeaders(methodRejected);
  assert.equal(methodRejected.headers.get("Cache-Control"), "no-store");
});

test("public responses omit no-store and preserve their explicit cache policy", async () => {
  const response = await worker.fetch(
    new Request(`${ORIGIN}/v1/directory/public-account`),
    env,
    { waitUntil() {} },
  );

  assert.equal(response.status, 200);
  assertSecurityHeaders(response);
  assert.equal(response.headers.get("Cache-Control"), "max-age=60");
  assert.notEqual(response.headers.get("Cache-Control"), "no-store");

  const bearerOnCachedPublicRoute = await worker.fetch(
    new Request(`${ORIGIN}/v1/directory/public-account`, {
      headers: { Authorization: "Bearer irrelevant-on-public-route" },
    }),
    env,
    { waitUntil() {} },
  );
  assert.equal(bearerOnCachedPublicRoute.status, 200);
  assertSecurityHeaders(bearerOnCachedPublicRoute);
  assert.equal(
    bearerOnCachedPublicRoute.headers.get("Cache-Control"),
    "max-age=60",
  );

  const irrelevantBearer = await worker.fetch(
    new Request(`${ORIGIN}/v1/directory/unknown-account`, {
      headers: { Authorization: "Bearer irrelevant-on-public-route" },
    }),
    env,
    { waitUntil() {} },
  );
  assert.equal(irrelevantBearer.status, 404);
  assertSecurityHeaders(irrelevantBearer);
  assert.equal(irrelevantBearer.headers.get("Cache-Control"), null);
});

test("explicit cache policy and streamed pass-through bodies are preserved", async () => {
  let upstreamBody;
  const response = await worker.fetch(
    new Request(`${ORIGIN}/healthz`),
    {
      CONTROL_PLANE: {
        fetch() {
          upstreamBody = new ReadableStream({
            start(controller) {
              controller.enqueue(new TextEncoder().encode("streamed health\n"));
              controller.close();
            },
          });
          return new Response(upstreamBody, {
            headers: {
              "Cache-Control": "public, max-age=17",
              "Content-Type": "text/plain; charset=utf-8",
            },
          });
        },
      },
    },
    { waitUntil() {} },
  );

  assertSecurityHeaders(response);
  assert.equal(response.headers.get("Cache-Control"), "public, max-age=17");
  assert.equal(response.body, upstreamBody);
  assert.equal(await response.text(), "streamed health\n");
});

test("an existing protected-route cache directive wins", async () => {
  const response = await worker.fetch(
    new Request(`${ORIGIN}/v1/email/operations-lease:acquire`),
    env,
    { waitUntil() {} },
  );

  assert.equal(response.status, 405);
  assertSecurityHeaders(response);
  assert.equal(response.headers.get("Cache-Control"), "private, no-store");
});
