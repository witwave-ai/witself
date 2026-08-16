import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import test from "node:test";
import {
  OutboundReceipt,
  ProviderRoute,
  PROVIDER_ROUTE_TTL_MS,
  RECEIPT_IDEMPOTENCY_TTL_MS,
  durableObjectReceiptReplay,
  validateDispatch,
} from "../src/dispatch.mjs";
import adapter, {
  failure,
  FRONTDOOR_LIMIT_PER_MINUTE,
  MAX_BODY_BYTES,
  RECEIPT_PROOF_SCHEMA,
  RECEIPT_REPLAY_AUDIENCE,
} from "../src/index.js";
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
  const clone = (value) => value === undefined
    ? undefined
    : structuredClone(value);
  const values = new Map(initial ? [["receipt", clone(initial)]] : []);
  let alarmAt = null;
  let activeTransaction = null;
  let deleteAllCalls = 0;
  let tail = Promise.resolve();
  const exclusively = (callback) => {
    const previous = tail;
    let release;
    tail = new Promise((resolve) => { release = resolve; });
    return (async () => {
      await previous;
      try {
        return await callback();
      } finally {
        release();
      }
    })();
  };
  return {
    values,
    async get(key) {
      return exclusively(() => clone(values.get(key)));
    },
    async put(key, value) {
      return exclusively(() => { values.set(key, clone(value)); });
    },
    async deleteAll() {
      if (activeTransaction) {
        activeTransaction.values.clear();
        activeTransaction.alarmAt = null;
        activeTransaction.deleteAllCalls += 1;
        return;
      }
      return exclusively(() => {
        values.clear();
        alarmAt = null;
        deleteAllCalls += 1;
      });
    },
    async setAlarm(value) {
      return exclusively(() => { alarmAt = value; });
    },
    async getAlarm() {
      return exclusively(() => alarmAt);
    },
    async deleteAlarm() {
      return exclusively(() => { alarmAt = null; });
    },
    async transaction(callback) {
      return exclusively(async () => {
        const draft = new Map(
          [...values].map(([key, value]) => [key, clone(value)]),
        );
        const transactionState = {
          values: draft,
          alarmAt,
          deleteAllCalls: 0,
        };
        const transaction = {
          async get(key) { return clone(draft.get(key)); },
          async put(key, value) { draft.set(key, clone(value)); },
          async delete(key) { draft.delete(key); },
        };
        activeTransaction = transactionState;
        try {
          const result = await callback(transaction);
          values.clear();
          for (const [key, value] of draft) values.set(key, clone(value));
          alarmAt = transactionState.alarmAt;
          deleteAllCalls += transactionState.deleteAllCalls;
          return clone(result);
        } finally {
          activeTransaction = null;
        }
      });
    },
    get alarmAt() { return alarmAt; },
    get deleteAllCalls() { return deleteAllCalls; },
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

async function waitForLength(values, length) {
  while (values.length < length) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

function acceptedReceipt(overrides = {}) {
  const route = {
    schema_version: "witself.agent-email-provider-route.v1",
    send_id: dispatch().send_id,
    account_id: dispatch().account_id,
    realm_id: dispatch().realm_id,
    signer_key_id: "founder-cell",
  };
  return {
    digest: "a".repeat(64),
    signer_key_id: "founder-cell",
    state: "accepted",
    provider_call_started_count: 1,
    verified_replay_count: 0,
    started_at: new Date(Date.now() - 2000).toISOString(),
    expires_at: new Date(Date.now() + RECEIPT_IDEMPOTENCY_TTL_MS).toISOString(),
    provider_message_id: "provider-message-1",
    route,
    status: 200,
    response: {
      schema_version: "witself.agent-email-dispatch-response.v1",
      send_id: dispatch().send_id,
      state: "accepted",
      provider: "cloudflare_email_sending",
      provider_message_id: "provider-message-1",
    },
    route_pending: true,
    route_attempts: 1,
    completed_at: new Date(Date.now() - 1000).toISOString(),
    unrelated_marker: "preserve-me",
    ...overrides,
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

function receiptReplayRequest(
  value = dispatch(),
  digest = "a".repeat(64),
  signerKeyId = "founder-cell",
) {
  return new Request("https://receipt.internal/receipt-replay", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Witself-Verified-Digest": digest,
      "X-Witself-Verified-Key-Id": signerKeyId,
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

function allowFrontDoorLimiter(onLimit = () => {}) {
  return {
    async limit(input) {
      onLimit(input);
      return { success: true };
    },
  };
}

async function signedAdapterRequest(
  value,
  {
    audience = "witself-agent-email-send",
    body: bodyOverride,
    connectingIP = "192.0.2.10",
    contentLength = true,
    path = "/v1/dispatch",
    env: envOverride = {},
  } = {},
) {
  const keyPair = await webcrypto.subtle.generateKey(
    { name: "Ed25519" },
    true,
    ["sign", "verify"],
  );
  const body = bodyOverride ??
    new TextEncoder().encode(JSON.stringify(value));
  const digest = await sha256Hex(body, webcrypto);
  const timestamp = new Date().toISOString();
  const keyId = "founder-cell";
  const signature = await webcrypto.subtle.sign(
    { name: "Ed25519" },
    keyPair.privateKey,
    signatureInput({ version: DISPATCH_SCHEMA, timestamp, keyId, audience, digest }),
  );
  const publicKey = await webcrypto.subtle.exportKey("raw", keyPair.publicKey);
  return {
    body,
    request: new Request(`https://send.example${path}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(contentLength ? { "Content-Length": String(body.byteLength) } : {}),
        "CF-Connecting-IP": connectingIP,
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
      DISPATCH_AUDIENCE: "witself-agent-email-send",
      RECEIPT_REPLAY_AUDIENCE,
      DISPATCH_REPLAY_WINDOW_SECONDS: "300",
      DISPATCH_SIGNERS_JSON: JSON.stringify({
        [keyId]: {
          public_key: base64(publicKey),
          account_ids: [value.account_id],
        },
      }),
      DISPATCH_ENABLED: "false",
      RECEIPT_REPLAY_ENABLED: "false",
      DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(),
      ...envOverride,
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

test("rejects untrusted requests without reading their bodies", async () => {
  let pulls = 0;
  const body = new ReadableStream({
    pull(controller) {
      pulls += 1;
      controller.enqueue(new Uint8Array(MAX_BODY_BYTES));
    },
  }, { highWaterMark: 0 });
  const response = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    headers: { "CF-Connecting-IP": "192.0.2.20" },
    body,
    duplex: "half",
  }), {
    DISPATCH_AUDIENCE: "witself-agent-email-send",
    RECEIPT_REPLAY_AUDIENCE,
    DISPATCH_REPLAY_WINDOW_SECONDS: "300",
    DISPATCH_SIGNERS_JSON: "{}",
    DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(),
  });
  assert.equal(response.status, 401);
  assert.equal(pulls, 0);
});

test("streams no more than the authenticated request envelope bound", async () => {
  const raw = new Uint8Array(MAX_BODY_BYTES + 1);
  const signed = await signedAdapterRequest(dispatch(), {
    body: raw,
    contentLength: false,
  });
  let offset = 0;
  let canceled = false;
  const body = new ReadableStream({
    pull(controller) {
      if (offset >= raw.length) {
        controller.close();
        return;
      }
      const end = Math.min(raw.length, offset + MAX_BODY_BYTES);
      controller.enqueue(raw.slice(offset, end));
      offset = end;
    },
    cancel() { canceled = true; },
  }, { highWaterMark: 0 });
  const response = await adapter.fetch(new Request(signed.request.url, {
    method: "POST",
    headers: signed.request.headers,
    body,
    duplex: "half",
  }), signed.env);
  assert.equal(response.status, 413);
  assert.equal(canceled, true);

  const frontDoorEnv = {
    DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(),
  };
  const invalidLength = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    headers: {
      "CF-Connecting-IP": "192.0.2.30",
      "Content-Length": "not-a-number",
    },
    body: "{}",
  }), frontDoorEnv);
  assert.equal(invalidLength.status, 400);

  const declaredOversize = await adapter.fetch(new Request("https://send.example/v1/dispatch", {
    method: "POST",
    headers: {
      "CF-Connecting-IP": "192.0.2.30",
      "Content-Length": String(MAX_BODY_BYTES + 1),
    },
    body: "{}",
  }), frontDoorEnv);
  assert.equal(declaredOversize.status, 413);
});

test("only the source lane precedes body authentication", async () => {
  assert.equal(FRONTDOOR_LIMIT_PER_MINUTE, 1000);
  const events = [];
  const signed = await signedAdapterRequest(dispatch(), {
    env: {
      DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(
        ({ key }) => events.push(key),
      ),
    },
  });
  const body = new ReadableStream({
    pull(controller) {
      events.push("body");
      controller.enqueue(signed.body);
      controller.close();
    },
  }, { highWaterMark: 0 });
  const response = await adapter.fetch(new Request(signed.request.url, {
    method: "POST",
    headers: signed.request.headers,
    body,
    duplex: "half",
  }), signed.env);
  assert.equal(response.status, 503);
  assert.equal(events.length, 4);
  assert.match(
    events[0],
    /^witself-agent-email-send\.frontdoor\.v1:ip:[0-9a-f]{64}$/,
  );
  assert.deepEqual(events.slice(1), [
    "body",
    "witself-agent-email-send.frontdoor.v1:aggregate",
    "witself-agent-email-send.frontdoor.v1:signer:founder-cell",
  ]);
  assert.ok(!events.some((event) => event.includes("192.0.2.10")));
});

test("front-door limiter failures fail closed at their trust boundary", async (t) => {
  const cases = [
    {
      name: "missing binding",
      limiter: null,
      expectedStatus: 503,
      expectedPulls: 0,
    },
    {
      name: "binding error",
      limiter: { async limit() { throw new Error("down"); } },
      expectedStatus: 503,
      expectedPulls: 0,
    },
    {
      name: "malformed result",
      limiter: { async limit() { return {}; } },
      expectedStatus: 503,
      expectedPulls: 0,
    },
    {
      name: "missing edge identity",
      limiter: allowFrontDoorLimiter(),
      connectingIP: "",
      expectedStatus: 503,
      expectedPulls: 0,
    },
    {
      name: "source refusal",
      limiter: { async limit() { return { success: false }; } },
      expectedStatus: 429,
      expectedPulls: 0,
    },
    {
      name: "aggregate refusal",
      limiter: {
        calls: 0,
        async limit() {
          this.calls += 1;
          return { success: this.calls === 1 };
        },
      },
      expectedStatus: 429,
      expectedPulls: 1,
      expectedSendId: dispatch().send_id,
    },
    {
      name: "signer refusal",
      limiter: {
        calls: 0,
        async limit() {
          this.calls += 1;
          return { success: this.calls < 3 };
        },
      },
      expectedStatus: 429,
      expectedPulls: 1,
      expectedSendId: dispatch().send_id,
    },
  ];
  for (const {
    name,
    limiter,
    connectingIP,
    expectedStatus,
    expectedPulls,
    expectedSendId = "esnd_invalid",
  } of cases) {
    await t.test(name, async () => {
      const signed = await signedAdapterRequest(dispatch(), { connectingIP });
      if (limiter === null) delete signed.env.DISPATCH_FRONTDOOR_LIMITER;
      else signed.env.DISPATCH_FRONTDOOR_LIMITER = limiter;
      let pulls = 0;
      let durableCalls = 0;
      signed.env.RECEIPTS = {
        idFromName(value) { return value; },
        get() {
          durableCalls += 1;
          throw new Error("front-door refusal reached receipt storage");
        },
      };
      const stream = new ReadableStream({
        pull(controller) {
          pulls += 1;
          controller.enqueue(signed.body);
          controller.close();
        },
      }, { highWaterMark: 0 });
      const response = await adapter.fetch(new Request(signed.request.url, {
        method: "POST",
        headers: signed.request.headers,
        body: stream,
        duplex: "half",
      }), signed.env);
      assert.equal(response.status, expectedStatus);
      assert.equal(response.headers.get("Retry-After"), "60");
      assert.equal((await response.json()).send_id, expectedSendId);
      assert.equal(pulls, expectedPulls);
      assert.equal(durableCalls, 0);
    });
  }
});

test("invalid signatures never read the request body", async () => {
  const signed = await signedAdapterRequest(dispatch());
  const keys = [];
  signed.env.DISPATCH_FRONTDOOR_LIMITER = allowFrontDoorLimiter(
    ({ key }) => keys.push(key),
  );
  const headers = new Headers(signed.request.headers);
  headers.set(HEADERS.signature, base64(new Uint8Array(64)));
  let pulls = 0;
  const body = new ReadableStream({
    pull(controller) {
      pulls += 1;
      controller.enqueue(signed.body);
      controller.close();
    },
  }, { highWaterMark: 0 });
  const response = await adapter.fetch(new Request(signed.request.url, {
    method: "POST",
    headers,
    body,
    duplex: "half",
  }), signed.env);
  assert.equal(response.status, 401);
  assert.equal(pulls, 0);
  assert.equal(keys.length, 1);
  assert.match(
    keys[0],
    /^witself-agent-email-send\.frontdoor\.v1:ip:[0-9a-f]{64}$/,
  );
});

test("a signed digest mismatch never reaches receipt storage", async () => {
  const keys = [];
  const signed = await signedAdapterRequest(dispatch(), {
    env: {
      DISPATCH_ENABLED: "true",
      DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(
        ({ key }) => keys.push(key),
      ),
    },
  });
  const changed = Uint8Array.from(signed.body);
  changed[changed.length - 2] ^= 1;
  let durableCalls = 0;
  signed.env.RECEIPTS = {
    idFromName(value) { return value; },
    get() {
      durableCalls += 1;
      throw new Error("digest mismatch reached receipt storage");
    },
  };
  const response = await adapter.fetch(new Request(signed.request.url, {
    method: "POST",
    headers: signed.request.headers,
    body: changed,
  }), signed.env);
  assert.equal(response.status, 401);
  assert.equal(durableCalls, 0);
  assert.equal(keys.length, 1);
  assert.match(
    keys[0],
    /^witself-agent-email-send\.frontdoor\.v1:ip:[0-9a-f]{64}$/,
  );
});

test("an unauthorized signed account cannot consume shared lanes", async () => {
  const keys = [];
  const signed = await signedAdapterRequest(dispatch(), {
    env: {
      DISPATCH_FRONTDOOR_LIMITER: allowFrontDoorLimiter(
        ({ key }) => keys.push(key),
      ),
    },
  });
  const signerRing = JSON.parse(signed.env.DISPATCH_SIGNERS_JSON);
  signerRing["founder-cell"].account_ids = ["acc_bbbbbbbbbbbbbbbb"];
  signed.env.DISPATCH_SIGNERS_JSON = JSON.stringify(signerRing);
  const response = await adapter.fetch(signed.request, signed.env);
  assert.equal(response.status, 403);
  assert.equal((await response.json()).error_code, "account_not_allowed");
  assert.equal(keys.length, 1);
  assert.match(
    keys[0],
    /^witself-agent-email-send\.frontdoor\.v1:ip:[0-9a-f]{64}$/,
  );
});

test("normal dispatch and receipt proof audiences are isolated", async () => {
  const normalOnProof = await signedAdapterRequest(dispatch(), {
    path: "/v1/dispatch:receipt-replay",
  });
  let response = await adapter.fetch(normalOnProof.request, normalOnProof.env);
  assert.equal(response.status, 401);
  assert.deepEqual(await response.json(), {
    schema_version: RECEIPT_PROOF_SCHEMA,
    send_id: "esnd_invalid",
    receipt_state: "unresolved",
    error_code: "receipt_unresolved",
  });

  const proofOnNormal = await signedAdapterRequest(dispatch(), {
    audience: RECEIPT_REPLAY_AUDIENCE,
  });
  response = await adapter.fetch(proofOnNormal.request, proofOnNormal.env);
  assert.equal(response.status, 401);
  assert.equal((await response.json()).error_code, "signature_invalid");

  const collapsedAudiences = await signedAdapterRequest(dispatch(), {
    path: "/v1/dispatch:receipt-replay",
    env: { RECEIPT_REPLAY_AUDIENCE: "witself-agent-email-send" },
  });
  response = await adapter.fetch(
    collapsedAudiences.request,
    collapsedAudiences.env,
  );
  assert.equal(response.status, 401);
});

test("receipt replay has an independent default-off public gate", async () => {
  const signed = await signedAdapterRequest(dispatch(), {
    audience: RECEIPT_REPLAY_AUDIENCE,
    path: "/v1/dispatch:receipt-replay",
    env: { DISPATCH_ENABLED: "true" },
  });
  let durableCalls = 0;
  signed.env.RECEIPTS = {
    idFromName(value) { return value; },
    get() {
      return { fetch() { durableCalls += 1; throw new Error("must stay dark"); } };
    },
  };
  const response = await adapter.fetch(signed.request, signed.env);
  assert.equal(response.status, 503);
  assert.equal((await response.json()).error_code, "receipt_unresolved");
  assert.equal(durableCalls, 0);

  const normal = await signedAdapterRequest(dispatch(), {
    env: { DISPATCH_ENABLED: "true", RECEIPT_REPLAY_ENABLED: "false" },
  });
  normal.env.RECEIPTS = {
    idFromName(value) { return value; },
    get() {
      return {
        fetch(url) {
          assert.equal(url, "https://receipt.internal/dispatch");
          return new Response(JSON.stringify({ state: "accepted" }), {
            headers: { "Content-Type": "application/json" },
          });
        },
      };
    },
  };
  const normalResponse = await adapter.fetch(normal.request, normal.env);
  assert.equal(normalResponse.status, 200);
  assert.equal((await normalResponse.json()).state, "accepted");
});

test("public receipt replay forwards only verified identity to the receipt object", async () => {
  const signed = await signedAdapterRequest(dispatch(), {
    audience: RECEIPT_REPLAY_AUDIENCE,
    path: "/v1/dispatch:receipt-replay",
    env: { RECEIPT_REPLAY_ENABLED: "true" },
  });
  const proof = {
    schema_version: RECEIPT_PROOF_SCHEMA,
    send_id: dispatch().send_id,
    receipt_state: "accepted",
    digest_matched: true,
    signer_matched: true,
    provider_call_started_count: 1,
    verified_replay_count: 1,
    route_pending: false,
  };
  let forwarded;
  signed.env.RECEIPTS = {
    idFromName(value) { return `id:${value}`; },
    get(id) {
      return {
        async fetch(url, init) {
          forwarded = { id, url, init };
          return new Response(JSON.stringify(proof), {
            headers: { "Content-Type": "application/json" },
          });
        },
      };
    },
  };
  const response = await adapter.fetch(signed.request, signed.env);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), proof);
  assert.equal(forwarded.id, `id:${dispatch().send_id}`);
  assert.equal(forwarded.url, "https://receipt.internal/receipt-replay");
  assert.equal(
    forwarded.init.headers["X-Witself-Verified-Key-Id"],
    "founder-cell",
  );
  assert.equal(
    forwarded.init.headers["X-Witself-Verified-Digest"],
    await sha256Hex(signed.body, webcrypto),
  );
  assert.deepEqual(JSON.parse(forwarded.init.body), dispatch());
});

test("receipt replay durable helper cannot select the provider dispatch method", async () => {
  let forwarded;
  const env = {
    RECEIPTS: {
      idFromName(value) { return value; },
      get() {
        return { fetch(url, init) { forwarded = { url, init }; return new Response(); } };
      },
    },
  };
  await durableObjectReceiptReplay(
    env,
    dispatch(),
    "a".repeat(64),
    "founder-cell",
  );
  assert.equal(forwarded.url, "https://receipt.internal/receipt-replay");
  assert.notEqual(forwarded.url, "https://receipt.internal/dispatch");
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
        const receipt = state.storage.values.get("receipt");
        assert.equal(receipt.state, "provider_started");
        assert.equal(receipt.provider_call_started_count, 1);
        assert.equal(receipt.verified_replay_count, 0);
        assert.equal(receipt.signer_key_id, "founder-cell");
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
  assert.equal(
    state.storage.values.get("receipt").provider_call_started_count,
    1,
  );
  assert.equal(state.storage.values.get("receipt").verified_replay_count, 0);
});

test("invalid provider message IDs close ambiguously without resending", async (t) => {
  const cases = [
    ["ASCII byte overflow", "x".repeat(513)],
    ["UTF-8 byte overflow", "é".repeat(257)],
    ["control character", "provider\nmessage"],
  ];
  for (const [name, messageId] of cases) {
    await t.test(name, async () => {
      const receiptStorage = storage();
      let sends = 0;
      let routes = 0;
      const durable = new OutboundReceipt({ storage: receiptStorage }, {
        EMAIL: {
          async send() {
            sends += 1;
            return { messageId };
          },
        },
        PROVIDER_ROUTES: providerRoutes(async () => {
          routes += 1;
          return new Response(null, { status: 204 });
        }),
      });
      let result = await durable.fetch(request());
      const firstBody = await result.json();
      assert.equal(result.status, 503);
      assert.equal(firstBody.state, "ambiguous");
      assert.equal(firstBody.error_code, "provider_outcome_ambiguous");
      const receipt = await receiptStorage.get("receipt");
      assert.equal(receipt.state, "ambiguous");
      assert.equal(receipt.provider_call_started_count, 1);
      assert.equal("provider_message_id" in receipt, false);
      assert.equal(routes, 0);

      result = await durable.fetch(request());
      assert.equal(result.status, 503);
      assert.deepEqual(await result.json(), firstBody);
      assert.equal(sends, 1);
      assert.equal(routes, 0);
    });
  }
});

test("receipt replay proves an accepted receipt without an EMAIL binding path", async () => {
  const receiptStorage = storage();
  const state = { storage: receiptStorage };
  const sender = new OutboundReceipt(state, {
    EMAIL: { async send() { return { messageId: "provider-message-1" }; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  assert.equal((await sender.fetch(request())).status, 200);

  const proofOnlyEnv = {};
  Object.defineProperty(proofOnlyEnv, "EMAIL", {
    get() { throw new Error("receipt proof touched EMAIL"); },
  });
  Object.defineProperty(proofOnlyEnv, "PROVIDER_ROUTES", {
    get() { throw new Error("receipt proof touched PROVIDER_ROUTES"); },
  });
  const proofReader = new OutboundReceipt(state, proofOnlyEnv);
  let response = await proofReader.fetch(receiptReplayRequest());
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), {
    schema_version: RECEIPT_PROOF_SCHEMA,
    send_id: dispatch().send_id,
    receipt_state: "accepted",
    digest_matched: true,
    signer_matched: true,
    provider_call_started_count: 1,
    verified_replay_count: 1,
    route_pending: false,
  });
  response = await proofReader.fetch(receiptReplayRequest());
  const second = await response.json();
  assert.equal(second.provider_call_started_count, 1);
  assert.equal(second.verified_replay_count, 2);
  assert.deepEqual(Object.keys(second), [
    "schema_version",
    "send_id",
    "receipt_state",
    "digest_matched",
    "signer_matched",
    "provider_call_started_count",
    "verified_replay_count",
    "route_pending",
  ]);
  assert.doesNotMatch(
    JSON.stringify(second),
    /provider-message-1|person@example|Plain text|scott\./,
  );
});

test("concurrent receipt proofs increment atomically", async () => {
  const receiptStorage = storage(acceptedReceipt({ route_pending: false }));
  const durable = new OutboundReceipt({ storage: receiptStorage }, {});
  const responses = await Promise.all([
    durable.fetch(receiptReplayRequest()),
    durable.fetch(receiptReplayRequest()),
  ]);
  const counts = (await Promise.all(responses.map((item) => item.json())))
    .map((item) => item.verified_replay_count)
    .sort((left, right) => left - right);
  assert.deepEqual(counts, [1, 2]);
  assert.equal(
    (await receiptStorage.get("receipt")).verified_replay_count,
    2,
  );
});

test("each retryable provider boundary durably increments the provider counter", async () => {
  const receiptStorage = storage();
  let calls = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: {
      async send() {
        calls += 1;
        assert.equal(
          receiptStorage.values.get("receipt").provider_call_started_count,
          calls,
        );
        if (calls === 1) {
          throw Object.assign(new Error("rate limited"), {
            code: "E_RATE_LIMIT_EXCEEDED",
          });
        }
        return { messageId: "provider-message-after-retry" };
      },
    },
    PROVIDER_ROUTES: providerRoutes(),
  });
  assert.equal((await durable.fetch(request())).status, 429);
  assert.equal(receiptStorage.values.get("receipt").provider_call_started_count, 1);
  assert.equal((await durable.fetch(request())).status, 200);
  assert.equal(receiptStorage.values.get("receipt").provider_call_started_count, 2);
  assert.equal(receiptStorage.values.get("receipt").verified_replay_count, 0);
});

test("base-format retryable receipt upgrades conservatively before retry", async () => {
  const receiptStorage = storage({
    digest: "a".repeat(64),
    state: "retryable",
    status: 429,
    response: {
      schema_version: "witself.agent-email-dispatch-response.v1",
      send_id: dispatch().send_id,
      state: "retryable",
      provider: "cloudflare_email_sending",
      error_code: "provider_rate_limited",
      retry_after_seconds: 60,
    },
    completed_at: new Date(Date.now() - 1000).toISOString(),
    expires_at: new Date(Date.now() + RECEIPT_IDEMPOTENCY_TTL_MS).toISOString(),
  });
  let calls = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: {
      async send() {
        calls += 1;
        const started = receiptStorage.values.get("receipt");
        assert.equal(started.provider_call_started_count, 2);
        assert.equal(started.verified_replay_count, 0);
        return { messageId: "provider-message-after-upgrade" };
      },
    },
    PROVIDER_ROUTES: providerRoutes(),
  });
  assert.equal((await durable.fetch(request())).status, 200);
  assert.equal(calls, 1);
  assert.equal(receiptStorage.values.get("receipt").provider_call_started_count, 2);
  const proof = await durable.fetch(receiptReplayRequest());
  assert.equal(proof.status, 200);
  const body = await proof.json();
  assert.equal(body.provider_call_started_count, 2);
  assert.notEqual(body.provider_call_started_count, 1);
  assert.equal(body.verified_replay_count, 1);
});

test("a failed provider-boundary receipt write prevents EMAIL.send", async () => {
  const receiptStorage = storage();
  const originalPut = receiptStorage.put;
  receiptStorage.put = async (key, value) => {
    if (value?.state === "provider_started") {
      throw new Error("durable write unavailable");
    }
    return originalPut(key, value);
  };
  let calls = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { calls += 1; return { messageId: "unsafe" }; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  await assert.rejects(
    () => durable.fetch(request()),
    /durable write unavailable/,
  );
  assert.equal(calls, 0);
});

test("receipt replay fails closed on missing, conflict, and unresolved receipts", async () => {
  const missing = new OutboundReceipt({ storage: storage() }, {});
  let response = await missing.fetch(receiptReplayRequest());
  assert.equal(response.status, 404);
  assert.deepEqual(await response.json(), {
    schema_version: RECEIPT_PROOF_SCHEMA,
    send_id: dispatch().send_id,
    receipt_state: "missing",
    error_code: "receipt_missing",
  });

  const receiptStorage = storage();
  const state = { storage: receiptStorage };
  const sender = new OutboundReceipt(state, {
    EMAIL: { async send() { return { messageId: "provider-message-1" }; } },
    PROVIDER_ROUTES: providerRoutes(),
  });
  await sender.fetch(request());
  const accepted = structuredClone(receiptStorage.values.get("receipt"));

  response = await sender.fetch(receiptReplayRequest(dispatch(), "b".repeat(64)));
  assert.equal(response.status, 409);
  assert.equal((await response.json()).error_code, "receipt_conflict");
  response = await sender.fetch(receiptReplayRequest(
    dispatch(),
    "a".repeat(64),
    "other-cell",
  ));
  assert.equal(response.status, 409);
  assert.equal((await response.json()).error_code, "receipt_conflict");

  for (const mutation of [
    (value) => { value.route.account_id = "acc_bbbbbbbbbbbbbbbb"; },
    (value) => { value.response.subject = "must never be accepted"; },
  ]) {
    const value = structuredClone(accepted);
    mutation(value);
    receiptStorage.values.set("receipt", value);
    response = await sender.fetch(receiptReplayRequest());
    assert.equal(response.status, 409);
    const body = await response.json();
    assert.equal(body.receipt_state, "conflict");
    assert.equal(body.error_code, "receipt_conflict");
    assert.deepEqual(receiptStorage.values.get("receipt"), value);
  }

  for (const mutation of [
    (value) => { value.state = "provider_started"; },
    (value) => { delete value.provider_call_started_count; },
    (value) => { delete value.verified_replay_count; },
    (value) => { value.verified_replay_count = 1_000_000; },
  ]) {
    const value = structuredClone(accepted);
    mutation(value);
    receiptStorage.values.set("receipt", value);
    response = await sender.fetch(receiptReplayRequest());
    assert.equal(response.status, 409);
    const body = await response.json();
    assert.equal(body.receipt_state, "unresolved");
    assert.equal(body.error_code, "receipt_unresolved");
    assert.deepEqual(receiptStorage.values.get("receipt"), value);
  }

  const expired = structuredClone(accepted);
  expired.expires_at = new Date(Date.now() - 1000).toISOString();
  receiptStorage.values.set("receipt", expired);
  response = await sender.fetch(receiptReplayRequest());
  assert.equal(response.status, 404);
  assert.equal((await response.json()).error_code, "receipt_missing");
  assert.equal(receiptStorage.values.has("receipt"), false);
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
    storage: storage({
      digest: "a".repeat(64),
      state: "provider_started",
      expires_at: new Date(Date.now() + 60_000).toISOString(),
    }),
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

test("legacy unfinished receipt repairs its original missing expiry alarm", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const receiptStorage = storage({
    digest: "a".repeat(64),
    signer_key_id: "founder-cell",
    state: "provider_started",
    provider_call_started_count: 1,
    verified_replay_count: 0,
    started_at: new Date(Date.now() - 1000).toISOString(),
    expires_at: expiresAt,
  });
  let sends = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { sends += 1; return { messageId: "never" }; } },
  });

  let result = await durable.fetch(request());
  assert.equal(result.status, 503);
  assert.equal((await result.json()).state, "ambiguous");
  assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
  const fixedExpiry = await receiptStorage.getAlarm();
  result = await durable.fetch(request());
  assert.equal(result.status, 503);
  assert.equal(await receiptStorage.getAlarm(), fixedExpiry);
  assert.equal(sends, 0);
});

test("legacy provider-started invalid message ID closes ambiguously", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const invalidId = "x".repeat(513);
  const original = {
    digest: "a".repeat(64),
    signer_key_id: "founder-cell",
    state: "provider_started",
    provider_call_started_count: 1,
    verified_replay_count: 0,
    started_at: new Date(Date.now() - 1000).toISOString(),
    expires_at: expiresAt,
    provider_message_id: invalidId,
    route: acceptedReceipt().route,
  };
  const receiptStorage = storage(original);
  const env = {};
  Object.defineProperties(env, {
    EMAIL: {
      get() { throw new Error("legacy replay touched EMAIL"); },
    },
    PROVIDER_ROUTES: {
      get() { throw new Error("legacy replay touched routes"); },
    },
  });
  const durable = new OutboundReceipt({ storage: receiptStorage }, env);

  const result = await durable.fetch(request());
  assert.equal(result.status, 503);
  assert.deepEqual(await result.json(), {
    schema_version: "witself.agent-email-dispatch-response.v1",
    send_id: dispatch().send_id,
    state: "ambiguous",
    provider: "cloudflare_email_sending",
    error_code: "provider_outcome_ambiguous",
  });
  assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
  assert.deepEqual(await receiptStorage.get("receipt"), original);
});

test("legacy terminal receipt repairs its original missing expiry alarm", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const receiptStorage = storage(acceptedReceipt({
    expires_at: expiresAt,
    route_pending: false,
    route_attempts: 0,
  }));
  const durable = new OutboundReceipt({ storage: receiptStorage }, {});

  let result = await durable.fetch(request());
  assert.equal(result.status, 200);
  assert.equal((await result.json()).state, "accepted");
  assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
  const fixedExpiry = await receiptStorage.getAlarm();
  result = await durable.fetch(request());
  assert.equal(result.status, 200);
  assert.equal(await receiptStorage.getAlarm(), fixedExpiry);
});

test("legacy accepted invalid message lineage closes ambiguously", async (t) => {
  const cases = [
    {
      name: "truncated oversized provider ID",
      storedId: "x".repeat(513),
      responseId: "x".repeat(512),
    },
    {
      name: "control character provider ID",
      storedId: "provider\nmessage",
      responseId: "provider\nmessage",
    },
  ];
  for (const { name, storedId, responseId } of cases) {
    await t.test(name, async () => {
      const expiresAt = new Date(Date.now() + 60_000).toISOString();
      const original = acceptedReceipt({
        expires_at: expiresAt,
        provider_message_id: storedId,
        response: {
          schema_version: "witself.agent-email-dispatch-response.v1",
          send_id: dispatch().send_id,
          state: "accepted",
          provider: "cloudflare_email_sending",
          provider_message_id: responseId,
        },
      });
      const receiptStorage = storage(original);
      const env = {};
      Object.defineProperties(env, {
        EMAIL: {
          get() { throw new Error("legacy replay touched EMAIL"); },
        },
        PROVIDER_ROUTES: {
          get() { throw new Error("legacy replay touched routes"); },
        },
      });
      const durable = new OutboundReceipt({ storage: receiptStorage }, env);

      const result = await durable.fetch(request());
      const body = await result.json();
      assert.equal(result.status, 503);
      assert.equal(body.state, "ambiguous");
      assert.equal(body.error_code, "provider_outcome_ambiguous");
      assert.equal("provider_message_id" in body, false);
      assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
      assert.deepEqual(await receiptStorage.get("receipt"), original);
    });
  }
});

test("receipt proof repairs a legacy accepted receipt's missing alarm", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const receiptStorage = storage(acceptedReceipt({
    expires_at: expiresAt,
    route_pending: false,
  }));
  const durable = new OutboundReceipt({ storage: receiptStorage }, {});

  const result = await durable.fetch(receiptReplayRequest());
  assert.equal(result.status, 200);
  assert.equal((await result.json()).verified_replay_count, 1);
  assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
});

test("existing receipt alarm failure escapes no result and calls no provider", async () => {
  const receiptStorage = storage(acceptedReceipt({ route_pending: false }));
  receiptStorage.setAlarm = async () => {
    throw new Error("receipt alarm unavailable");
  };
  let sends = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { sends += 1; return { messageId: "never" }; } },
  });

  await assert.rejects(durable.fetch(request()), /receipt alarm unavailable/);
  await assert.rejects(
    durable.fetch(receiptReplayRequest()),
    /receipt alarm unavailable/,
  );
  assert.equal(sends, 0);
  assert.equal((await receiptStorage.get("receipt")).verified_replay_count, 0);
});

test("expired legacy receipt is retired without a provider call", async () => {
  const receiptStorage = storage({
    digest: "a".repeat(64),
    signer_key_id: "founder-cell",
    state: "provider_started",
    provider_call_started_count: 1,
    verified_replay_count: 0,
    started_at: new Date(Date.now() - 120_000).toISOString(),
    expires_at: new Date(Date.now() - 60_000).toISOString(),
  });
  let sends = 0;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    EMAIL: { async send() { sends += 1; return { messageId: "never" }; } },
  });

  const result = await durable.fetch(request());
  assert.equal(result.status, 503);
  assert.equal((await result.json()).state, "ambiguous");
  assert.equal(receiptStorage.values.has("receipt"), false);
  assert.equal(sends, 0);
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

test("provider route is not persisted until its expiry alarm is durable", async () => {
  const routeStorage = storage();
  const originalSetAlarm = routeStorage.setAlarm;
  let alarmAttempts = 0;
  routeStorage.setAlarm = async (value) => {
    alarmAttempts += 1;
    if (alarmAttempts === 1) throw new Error("alarm unavailable");
    return originalSetAlarm(value);
  };
  const durable = new ProviderRoute({ storage: routeStorage });
  const route = {
    schema_version: "witself.agent-email-provider-route.v1",
    send_id: dispatch().send_id,
    account_id: dispatch().account_id,
    realm_id: dispatch().realm_id,
    signer_key_id: "founder-cell",
  };
  const post = () => durable.fetch(new Request("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(route),
  }));

  await assert.rejects(post, /alarm unavailable/);
  assert.equal(routeStorage.values.has("route"), false);
  assert.equal(routeStorage.alarmAt, null);

  let response = await post();
  assert.equal(response.status, 204);
  assert.deepEqual(routeStorage.values.get("route"), route);
  const fixedExpiry = routeStorage.alarmAt;
  response = await post();
  assert.equal(response.status, 204);
  assert.equal(routeStorage.alarmAt, fixedExpiry);
  assert.equal(alarmAttempts, 2);
});

test("an exact legacy provider route repairs its missing expiry alarm", async () => {
  const routeStorage = storage();
  const route = {
    schema_version: "witself.agent-email-provider-route.v1",
    send_id: dispatch().send_id,
    account_id: dispatch().account_id,
    realm_id: dispatch().realm_id,
    signer_key_id: "founder-cell",
  };
  routeStorage.values.set("route", structuredClone(route));
  assert.equal(await routeStorage.getAlarm(), null);
  const durable = new ProviderRoute({ storage: routeStorage });
  const post = () => durable.fetch(new Request("https://route.internal/route", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(route),
  }));

  const scheduledAt = Date.now();
  let result = await post();
  assert.equal(result.status, 204);
  const repairedExpiry = await routeStorage.getAlarm();
  assert.ok(repairedExpiry >= scheduledAt + PROVIDER_ROUTE_TTL_MS);
  assert.ok(repairedExpiry <= Date.now() + PROVIDER_ROUTE_TTL_MS);
  assert.deepEqual(routeStorage.values.get("route"), route);

  result = await post();
  assert.equal(result.status, 204);
  assert.equal(await routeStorage.getAlarm(), repairedExpiry);
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

test("successful route finalization preserves a concurrent receipt proof", async () => {
  const receiptStorage = storage(acceptedReceipt());
  const routeStarted = deferred();
  const finishRoute = deferred();
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    PROVIDER_ROUTES: providerRoutes(async () => {
      routeStarted.resolve();
      await finishRoute.promise;
      return new Response(null, { status: 204 });
    }),
  });
  const alarm = durable.alarm();
  await routeStarted.promise;
  const proof = await durable.fetch(receiptReplayRequest());
  assert.equal(proof.status, 200);
  assert.equal((await proof.json()).verified_replay_count, 1);
  finishRoute.resolve();
  await alarm;
  const receipt = await receiptStorage.get("receipt");
  assert.equal(receipt.route_pending, false);
  assert.equal(receipt.route_attempts, 0);
  assert.equal(receipt.verified_replay_count, 1);
  assert.equal(receipt.provider_call_started_count, 1);
  assert.equal(receipt.unrelated_marker, "preserve-me");
});

test("failed route finalization merges fresh attempts and proof state", async () => {
  const receiptStorage = storage(acceptedReceipt());
  const routeStarted = deferred();
  const finishRoute = deferred();
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    PROVIDER_ROUTES: providerRoutes(async () => {
      routeStarted.resolve();
      await finishRoute.promise;
      return new Response(null, { status: 503 });
    }),
  });
  const alarm = durable.alarm();
  await routeStarted.promise;
  const proof = await durable.fetch(receiptReplayRequest());
  assert.equal(proof.status, 200);
  finishRoute.resolve();
  await alarm;
  const receipt = await receiptStorage.get("receipt");
  assert.equal(receipt.route_pending, true);
  assert.equal(receipt.route_attempts, 2);
  assert.equal(receipt.verified_replay_count, 1);
  assert.equal(receipt.provider_call_started_count, 1);
  assert.equal(receipt.unrelated_marker, "preserve-me");
});

test("concurrent route finalizers make success monotonic in either order", async () => {
  for (const order of ["success-first", "failure-first"]) {
    const receiptStorage = storage(acceptedReceipt());
    const routes = [];
    const durable = new OutboundReceipt({ storage: receiptStorage }, {
      PROVIDER_ROUTES: providerRoutes(() => new Promise((resolve) => {
        routes.push(resolve);
      })),
    });
    const first = durable.alarm();
    const second = durable.alarm();
    await waitForLength(routes, 2);
    if (order === "success-first") {
      routes[0](new Response(null, { status: 204 }));
      await first;
      routes[1](new Response(null, { status: 503 }));
      await second;
    } else {
      routes[0](new Response(null, { status: 503 }));
      await first;
      routes[1](new Response(null, { status: 204 }));
      await second;
    }
    const receipt = await receiptStorage.get("receipt");
    assert.equal(receipt.route_pending, false, order);
    assert.equal(receipt.route_attempts, 0, order);
    assert.equal(receipt.verified_replay_count, 0, order);
    assert.equal(receipt.unrelated_marker, "preserve-me", order);
  }
});

test("stale route finalizers never resurrect or overwrite changed lineage", async () => {
  const cases = [
    {
      name: "missing",
      status: 204,
      async mutate(transaction) { await transaction.delete("receipt"); },
    },
    {
      name: "mismatched",
      status: 503,
      async mutate(transaction, receipt) {
        await transaction.put("receipt", { ...receipt, digest: "b".repeat(64) });
      },
    },
    {
      name: "changed-expiry",
      status: 204,
      async mutate(transaction, receipt) {
        await transaction.put("receipt", {
          ...receipt,
          expires_at: new Date(Date.now() - 1000).toISOString(),
        });
      },
    },
  ];
  for (const value of cases) {
    const receiptStorage = storage(acceptedReceipt());
    const routeStarted = deferred();
    const finishRoute = deferred();
    const durable = new OutboundReceipt({ storage: receiptStorage }, {
      PROVIDER_ROUTES: providerRoutes(async () => {
        routeStarted.resolve();
        await finishRoute.promise;
        return new Response(null, { status: value.status });
      }),
    });
    const alarm = durable.alarm();
    await routeStarted.promise;
    await receiptStorage.transaction(async (transaction) => {
      const current = await transaction.get("receipt");
      await value.mutate(transaction, current);
    });
    const beforeFinalize = await receiptStorage.get("receipt");
    finishRoute.resolve();
    await alarm;
    assert.deepEqual(
      await receiptStorage.get("receipt"),
      beforeFinalize,
      value.name,
    );
  }
});

test("route finalizers delete their exact receipt when I/O crosses expiry", async (t) => {
  for (const status of [204, 503]) {
    await t.test(`route status ${status}`, async () => {
      const startedAt = Date.now();
      const expiry = startedAt + 60_000;
      const receiptStorage = storage(acceptedReceipt({
        expires_at: new Date(expiry).toISOString(),
      }));
      await receiptStorage.put("extra-storage", { must: "also disappear" });
      const originalSetAlarm = receiptStorage.setAlarm;
      let alarmSchedules = 0;
      receiptStorage.setAlarm = async (value) => {
        alarmSchedules += 1;
        return originalSetAlarm(value);
      };
      const routeStarted = deferred();
      const finishRoute = deferred();
      const durable = new OutboundReceipt({ storage: receiptStorage }, {
        PROVIDER_ROUTES: providerRoutes(async () => {
          routeStarted.resolve();
          await finishRoute.promise;
          return new Response(null, { status });
        }),
      });
      const alarm = durable.alarm();
      await routeStarted.promise;
      const originalNow = Date.now;
      try {
        Date.now = () => expiry + 1;
        finishRoute.resolve();
        await alarm;
      } finally {
        Date.now = originalNow;
      }
      assert.equal(receiptStorage.values.size, 0);
      assert.equal(receiptStorage.alarmAt, null);
      assert.equal(receiptStorage.deleteAllCalls, 1);
      assert.equal(alarmSchedules, 1);
    });
  }
});

test("expired route finalizer cleanup preserves replacement lineage", async () => {
  const startedAt = Date.now();
  const expiry = startedAt + 60_000;
  const receiptStorage = storage(acceptedReceipt({
    expires_at: new Date(expiry).toISOString(),
  }));
  await receiptStorage.put("extra-storage", { preserve: true });
  const routeStarted = deferred();
  const finishRoute = deferred();
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    PROVIDER_ROUTES: providerRoutes(async () => {
      routeStarted.resolve();
      await finishRoute.promise;
      return new Response(null, { status: 204 });
    }),
  });
  const alarm = durable.alarm();
  await routeStarted.promise;
  await receiptStorage.transaction(async (transaction) => {
    const receipt = await transaction.get("receipt");
    await transaction.put("receipt", { ...receipt, digest: "b".repeat(64) });
  });
  const replacement = await receiptStorage.get("receipt");
  const originalNow = Date.now;
  try {
    Date.now = () => expiry + 1;
    finishRoute.resolve();
    await alarm;
  } finally {
    Date.now = originalNow;
  }
  assert.deepEqual(await receiptStorage.get("receipt"), replacement);
  assert.deepEqual(
    receiptStorage.values.get("extra-storage"),
    { preserve: true },
  );
  assert.equal(receiptStorage.alarmAt, expiry);
  assert.equal(receiptStorage.deleteAllCalls, 0);
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

test("route alarm reinstalls absolute receipt expiry before provider I/O", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const receiptStorage = storage(acceptedReceipt({ expires_at: expiresAt }));
  let observedFallback = null;
  const durable = new OutboundReceipt({ storage: receiptStorage }, {
    PROVIDER_ROUTES: providerRoutes(async () => {
      observedFallback = receiptStorage.alarmAt;
      return new Response(null, { status: 503 });
    }),
  });

  await receiptStorage.deleteAlarm();
  await durable.alarm();
  assert.equal(observedFallback, Date.parse(expiresAt));
  assert.ok(receiptStorage.alarmAt <= Date.parse(expiresAt));
  assert.equal(
    (await receiptStorage.get("receipt")).route_pending,
    true,
  );
});

test("invalid legacy route lineage keeps its absolute cleanup alarm", async () => {
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  const oversizedId = "x".repeat(513);
  const receiptStorage = storage(acceptedReceipt({
    expires_at: expiresAt,
    provider_message_id: oversizedId,
    response: {
      schema_version: "witself.agent-email-dispatch-response.v1",
      send_id: dispatch().send_id,
      state: "accepted",
      provider: "cloudflare_email_sending",
      provider_message_id: oversizedId,
    },
  }));
  const env = { PROVIDER_ROUTES: providerRoutes() };
  Object.defineProperty(env, "EMAIL", {
    get() { throw new Error("alarm touched EMAIL"); },
  });
  const durable = new OutboundReceipt({ storage: receiptStorage }, env);

  await durable.alarm();
  assert.equal(receiptStorage.values.has("receipt"), true);
  assert.equal(await receiptStorage.getAlarm(), Date.parse(expiresAt));
  assert.equal(
    (await receiptStorage.get("receipt")).provider_message_id,
    oversizedId,
  );
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
  receiptStorage.values.set("extra-storage", { must: "also disappear" });
  await durable.alarm();
  assert.equal(receiptStorage.values.size, 0);
  assert.equal(receiptStorage.alarmAt, null);
  assert.equal(receiptStorage.deleteAllCalls, 1);
});
