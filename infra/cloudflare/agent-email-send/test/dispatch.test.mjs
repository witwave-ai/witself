import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import test from "node:test";
import {
  OutboundReceipt,
  ProviderRoute,
  PROVIDER_ROUTE_TTL_MS,
  RECEIPT_IDEMPOTENCY_TTL_MS,
  validateDispatch,
} from "../src/dispatch.mjs";
import adapter, { failure, MAX_BODY_BYTES } from "../src/index.js";
import {
  DISPATCH_SCHEMA,
  HEADERS,
  sha256Hex,
  signatureInput,
} from "../src/signature.mjs";

function dispatch() {
  return {
    schema_version: DISPATCH_SCHEMA,
    send_id: "esnd_aaaaaaaaaaaaaaaa",
    account_id: "acc_aaaaaaaaaaaaaaaa",
    realm_id: "realm_aaaaaaaaaaaaaaaa",
    agent_id: "agent_aaaaaaaaaaaaaaaa",
    from: "scott.aaaaaaaaaaaaaaaa@send.witmail.net",
    reply_to: "scott.aaaaaaaaaaaaaaaa@witmail.net",
    to: "person@example.com",
    subject: "Hello",
    text: "Plain text",
  };
}

function storage(initial) {
  const values = new Map(initial ? [["receipt", initial]] : []);
  let alarmAt = null;
  return {
    values,
    async get(key) { return values.get(key); },
    async put(key, value) { values.set(key, value); },
    async deleteAll() { values.clear(); alarmAt = null; },
    async setAlarm(value) { alarmAt = value; },
    async deleteAlarm() { alarmAt = null; },
    get alarmAt() { return alarmAt; },
  };
}

function request(value = dispatch(), digest = "a".repeat(64)) {
  return new Request("https://receipt.internal/dispatch", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Witself-Verified-Digest": digest,
      "X-Witself-Verified-Key-Id": "founder-cell",
    },
    body: JSON.stringify(value),
  });
}

function providerRoutes(onFetch = async () => new Response(null, { status: 204 })) {
  return {
    idFromName(value) { return value; },
    get(id) {
      return { fetch(request, init) { return onFetch(id, request, init); } };
    },
  };
}

function base64(value) {
  return Buffer.from(value).toString("base64");
}

async function signedAdapterRequest(value) {
  const keyPair = await webcrypto.subtle.generateKey(
    { name: "Ed25519" },
    true,
    ["sign", "verify"],
  );
  const body = new TextEncoder().encode(JSON.stringify(value));
  const digest = await sha256Hex(body, webcrypto);
  const timestamp = new Date().toISOString();
  const audience = "witself-agent-email-send";
  const keyId = "founder-cell";
  const signature = await webcrypto.subtle.sign(
    { name: "Ed25519" },
    keyPair.privateKey,
    signatureInput({ version: DISPATCH_SCHEMA, timestamp, keyId, audience, digest }),
  );
  const publicKey = await webcrypto.subtle.exportKey("raw", keyPair.publicKey);
  return {
    body,
    request: new Request("https://send.example/v1/dispatch", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Content-Length": String(body.byteLength),
        [HEADERS.version]: DISPATCH_SCHEMA,
        [HEADERS.timestamp]: timestamp,
        [HEADERS.keyId]: keyId,
        [HEADERS.audience]: audience,
        [HEADERS.digest]: digest,
        [HEADERS.signature]: base64(signature),
      },
      body,
    }),
    env: {
      DISPATCH_AUDIENCE: audience,
      DISPATCH_REPLAY_WINDOW_SECONDS: "300",
      DISPATCH_SIGNERS_JSON: JSON.stringify({
        [keyId]: {
          public_key: base64(publicKey),
          account_ids: [value.account_id],
        },
      }),
      DISPATCH_ENABLED: "false",
    },
  };
}

test("validates one plain-text managed sender", () => {
  assert.equal(validateDispatch(dispatch(), {}).send_id, dispatch().send_id);
  for (const mutate of [
    (value) => { value.from = "scott.aaaaaaaaaaaaaaaa@witmail.net"; },
    (value) => { value.reply_to = "other.aaaaaaaaaaaaaaaa@witmail.net"; },
    (value) => { value.to = "Person <person@example.com>"; },
    (value) => { value.subject = "bad\nheader"; },
    (value) => { value.text = ""; },
  ]) {
    const value = dispatch();
    mutate(value);
    assert.throws(() => validateDispatch(value, {}));
  }
  const maximum = dispatch();
  maximum.text = "a".repeat(256 * 1024);
  assert.equal(validateDispatch(maximum, {}).text.length, 256 * 1024);
  maximum.text += "a";
  assert.throws(() => validateDispatch(maximum, {}));
});

test("accepts the worst-case escaped 256 KiB text envelope", async () => {
  const value = dispatch();
  value.text = "\u0001".repeat(256 * 1024);
  const signed = await signedAdapterRequest(value);
  assert.ok(signed.body.byteLength > 300 * 1024);
  assert.ok(signed.body.byteLength <= MAX_BODY_BYTES);
  const response = await adapter.fetch(signed.request, signed.env);
  assert.equal(response.status, 503);
  assert.equal((await response.json()).error_code, "provider_unavailable");
});

test("streams no more than the request envelope bound before authentication", async () => {
  let pulls = 0;
  let canceled = false;
  const body = new ReadableStream({
    pull(controller) {
      pulls += 1;
      if (pulls === 1) controller.enqueue(new Uint8Array(MAX_BODY_BYTES));
      else if (pulls === 2) controller.enqueue(new Uint8Array(1));
      else controller.close();
    },
    cancel() { canceled = true; },
  });
  const response = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    body,
    duplex: "half",
  }), {});
  assert.equal(response.status, 413);
  assert.equal(canceled, true);

  const invalidLength = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    headers: { "Content-Length": "not-a-number" },
    body: "{}",
  }), {});
  assert.equal(invalidLength.status, 400);

  const declaredOversize = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    headers: { "Content-Length": String(MAX_BODY_BYTES + 1) },
    body: "{}",
  }), {});
  assert.equal(declaredOversize.status, 413);
});

test("dark adapter refusal is retryable rather than a terminal rejection", async () => {
  const response = failure(
    503,
    "provider_unavailable",
    dispatch().send_id,
    "retryable",
    60,
  );
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.agent-email-dispatch-response.v1",
    send_id: dispatch().send_id,
    state: "retryable",
    provider: "cloudflare_email_sending",
    error_code: "provider_unavailable",
    retry_after_seconds: 60,
  });
});

test("durable receipt submits once and replays exact result", async () => {
  const state = { storage: storage() };
  let calls = 0;
  const durable = new OutboundReceipt(state, {
    EMAIL: {
      async send(message) {
        calls += 1;
        assert.equal(message.from, dispatch().from);
        return { messageId: "provider-message-1" };
      },
    },
    PROVIDER_ROUTES: providerRoutes(),
  });
  let response = await durable.fetch(request());
  assert.equal(response.status, 200);
  assert.equal((await response.json()).state, "accepted");
  response = await durable.fetch(request());
  assert.equal((await response.json()).provider_message_id, "provider-message-1");
  assert.equal(calls, 1);
});

test("digest conflict never resubmits", async () => {
  const state = { storage: storage() };
  let calls = 0;
  const durable = new OutboundReceipt(state, {
    EMAIL: { async send() { calls += 1; return { messageId: "one" }; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  await durable.fetch(request());
  const response = await durable.fetch(request(dispatch(), "b".repeat(64)));
  assert.equal(response.status, 409);
  assert.equal((await response.json()).error_code, "idempotency_conflict");
  assert.equal(calls, 1);
});

test("unfinished provider boundary resolves ambiguous without a second call", async () => {
  const state = {
    storage: storage({ digest: "a".repeat(64), state: "provider_started" }),
  };
  let calls = 0;
  const durable = new OutboundReceipt(state, {
    EMAIL: { async send() { calls += 1; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  const response = await durable.fetch(request());
  assert.equal(response.status, 503);
  assert.equal((await response.json()).state, "ambiguous");
  assert.equal(calls, 0);
});

test("known provider throttling is retryable and content-free", async () => {
  const state = { storage: storage() };
  const durable = new OutboundReceipt(state, {
    EMAIL: {
      async send() {
        throw Object.assign(new Error("private provider text"), {
          code: "E_RATE_LIMIT_EXCEEDED",
        });
      },
    },
    PROVIDER_ROUTES: providerRoutes(),
  });
  const response = await durable.fetch(request());
  const text = await response.text();
  assert.equal(response.status, 429);
  assert.match(text, /provider_rate_limited/);
  assert.doesNotMatch(text, /private provider text/);
});

test("undifferentiated delivery failure does not create a hard-bounce result", async () => {
  const state = { storage: storage() };
  const durable = new OutboundReceipt(state, {
    EMAIL: {
      async send() {
        throw Object.assign(new Error("could be an exhausted soft bounce"), {
          code: "E_DELIVERY_FAILED",
        });
      },
    },
    PROVIDER_ROUTES: providerRoutes(),
  });
  const response = await durable.fetch(request());
  const body = await response.json();
  assert.equal(response.status, 422);
  assert.equal(body.state, "rejected");
  assert.equal(body.error_code, "provider_delivery_failed");
});

test("provider route is immutable and content-free", async () => {
  const routeStorage = storage();
  const state = { storage: routeStorage };
  const durable = new ProviderRoute(state);
  const route = {
    schema_version: "witself.agent-email-provider-route.v1",
    send_id: dispatch().send_id,
    account_id: dispatch().account_id,
    realm_id: dispatch().realm_id,
    signer_key_id: "founder-cell",
  };
  const scheduledAt = Date.now();
  let response = await durable.fetch(new Request("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(route),
  }));
  assert.equal(response.status, 204);
  const firstExpiry = routeStorage.alarmAt;
  response = await durable.fetch(new Request("https://route.internal/route"));
  assert.deepEqual(await response.json(), route);
  response = await durable.fetch(new Request("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(route),
  }));
  assert.equal(response.status, 204);
  assert.equal(routeStorage.alarmAt, firstExpiry);
  response = await durable.fetch(new Request("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ...route, realm_id: "realm_bbbbbbbbbbbbbbbb" }),
  }));
  assert.equal(response.status, 409);
  assert.equal(typeof routeStorage.alarmAt, "number");
  assert.ok(routeStorage.alarmAt >= scheduledAt + PROVIDER_ROUTE_TTL_MS);
  assert.ok(routeStorage.alarmAt <= Date.now() + PROVIDER_ROUTE_TTL_MS);
  await durable.alarm();
  response = await durable.fetch(new Request("https://route.internal/route"));
  assert.equal(response.status, 404);
});

test("accepted provider result repairs a failed route without resending", async () => {
  const receiptStorage = storage();
  const state = { storage: receiptStorage };
  let sends = 0;
  let routes = 0;
  const durable = new OutboundReceipt(state, {
    EMAIL: { async send() { sends += 1; return { messageId: "repair-me" }; } },
    PROVIDER_ROUTES: providerRoutes(async () => {
      routes += 1;
      return new Response(null, { status: routes === 1 ? 503 : 204 });
    }),
  });
  let response = await durable.fetch(request());
  assert.equal(response.status, 200);
  assert.equal((await response.json()).provider_message_id, "repair-me");
  assert.equal(receiptStorage.values.get("receipt").route_pending, true);
  assert.equal(typeof receiptStorage.alarmAt, "number");
  await durable.alarm();
  assert.equal(receiptStorage.values.get("receipt").route_pending, false);
  response = await durable.fetch(request());
  assert.equal(response.status, 200);
  assert.equal((await response.json()).provider_message_id, "repair-me");
  assert.equal(sends, 1);
  assert.equal(routes, 2);
});

test("route-repair alarm backs off durably without resending", async () => {
  const receiptStorage = storage();
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { return { messageId: "repair-later" }; } },
    PROVIDER_ROUTES: providerRoutes(async () => new Response(null, { status: 503 })),
  });
  const response = await durable.fetch(request());
  assert.equal(response.status, 200);
  const firstAlarm = receiptStorage.alarmAt;
  await durable.alarm();
  const receipt = receiptStorage.values.get("receipt");
  assert.equal(receipt.state, "accepted");
  assert.equal(receipt.route_pending, true);
  assert.equal(receipt.route_attempts, 2);
  assert.ok(receiptStorage.alarmAt > firstAlarm);
});

test("receipt idempotency expires separately without retaining message content", async () => {
  const receiptStorage = storage();
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { return { messageId: "expires" }; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  const scheduledAt = Date.now();
  await durable.fetch(request());
  const receipt = receiptStorage.values.get("receipt");
  assert.equal("text" in receipt, false);
  assert.ok(Date.parse(receipt.expires_at) >= scheduledAt + RECEIPT_IDEMPOTENCY_TTL_MS);
  assert.ok(Date.parse(receipt.expires_at) <= Date.now() + RECEIPT_IDEMPOTENCY_TTL_MS);
  assert.ok(PROVIDER_ROUTE_TTL_MS > RECEIPT_IDEMPOTENCY_TTL_MS);
  receiptStorage.values.set("receipt", {
    ...receipt,
    expires_at: new Date(Date.now() - 1000).toISOString(),
  });
  await durable.alarm();
  assert.equal(receiptStorage.values.size, 0);
});
