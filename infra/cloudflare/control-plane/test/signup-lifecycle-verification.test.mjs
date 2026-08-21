// Request-boundary contract tests for the managed-account onboarding
// lifecycle: signup entry, email verification, verification resend, and
// pending-signup expiration. Every test drives the Worker's real default
// export (fetch / scheduled) — not internal helpers — against in-memory
// fakes: no network, no Cloudflare account, no email provider, no cells.
import assert from "node:assert/strict";
import test from "node:test";
import { register } from "node:module";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
const { containerCalls, resetContainerCalls } = await import(
  "./fixtures/cloudflare-containers-stub.mjs"
);

// Workers exposes crypto.subtle.timingSafeEqual; Node does not. The shim is
// behaviorally identical for the worker's equal-length comparisons.
if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(a, b) {
      const ab = new Uint8Array(a);
      const bb = new Uint8Array(b);
      if (ab.byteLength !== bb.byteLength) {
        return false;
      }
      return nodeTimingSafeEqual(ab, bb);
    },
  });
}

const ORIGIN = "https://cp.test.invalid";
const FLEET_TOKEN = "fleet-test-token";
const CELL_ENDPOINT = "https://cell-a.test.invalid";
const PROVISION_TOKEN = "prov-cell-token";
const TOKEN = "ab".repeat(32); // a well-formed 64-hex verification token

async function sha256hex(s) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

class KVFake {
  constructor() {
    this.map = new Map();
    this.ttls = new Map();
    this.failDeletes = new Set();
  }

  async get(key, opts) {
    const raw = this.map.get(key);
    if (raw === undefined) {
      return null;
    }
    return opts?.type === "json" ? JSON.parse(raw) : raw;
  }

  async put(key, value, opts) {
    this.map.set(key, value);
    this.ttls.set(key, opts?.expirationTtl ?? null);
  }

  async delete(key) {
    if (this.failDeletes.has(key)) {
      this.failDeletes.delete(key);
      throw new Error("injected KV delete failure");
    }
    this.map.delete(key);
  }

  // Pages like real KV: small pages and a resume-after-this-key cursor, so
  // the sweep's do/while pagination loop is a tested contract even while it
  // deletes keys mid-iteration.
  async list({ prefix = "", cursor } = {}) {
    const all = [...this.map.keys()].filter((k) => k.startsWith(prefix)).sort();
    let begin = 0;
    if (cursor) {
      const after = all.findIndex((k) => k > cursor);
      begin = after === -1 ? all.length : after;
    }
    const page = all.slice(begin, begin + 2);
    const complete = begin + page.length >= all.length;
    return {
      keys: page.map((name) => ({ name })),
      list_complete: complete,
      cursor: complete ? undefined : page.at(-1),
    };
  }

  json(key) {
    const raw = this.map.get(key);
    return raw === undefined ? null : JSON.parse(raw);
  }

  keysWithPrefix(prefix) {
    return [...this.map.keys()].filter((k) => k.startsWith(prefix)).sort();
  }
}

class EmailFake {
  constructor() {
    this.sent = [];
    this.failNextSends = 0;
  }

  async send(message) {
    if (this.failNextSends > 0) {
      this.failNextSends -= 1;
      throw new Error("injected email delivery failure");
    }
    this.sent.push(message);
  }
}

function signupNamespace(impl) {
  return {
    idFromName: (name) => ({ name }),
    get: (id) => ({ fetch: async (request) => impl(id, request) }),
  };
}

function makeEnv(overrides = {}) {
  const kv = new KVFake();
  const email = new EmailFake();
  const env = {
    DIRECTORY: kv,
    EMAIL: email,
    FLEET_TOKEN,
    ACCOUNT_SIGNUP: signupNamespace(async () =>
      new Response(JSON.stringify({ error: "unexpected signup DO call" }), { status: 500 })),
    ...overrides,
  };
  return { env, kv, email };
}

// mockCell installs the global-fetch fake the worker's cell round trips hit.
// Handlers are matched by URL-pathname suffix; audit :events posts are
// absorbed; anything unmatched answers 500 and is visible in the transcript.
function mockCell(t, handlers = {}) {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (input, init = {}) => {
    const url = new URL(typeof input === "string" ? input : input.url);
    calls.push({ method: init.method ?? "GET", path: url.pathname, init });
    for (const [suffix, handler] of Object.entries(handlers)) {
      if (url.pathname.endsWith(suffix)) {
        return handler(url, init, calls);
      }
    }
    if (url.pathname.endsWith(":events")) {
      return Response.json({ ok: true });
    }
    return Response.json({ error: "unexpected cell call" }, { status: 500 });
  });
  return calls;
}

function cellJSON(body, status = 200) {
  return Response.json(body, { status });
}

async function run(env, path, init = {}) {
  resetContainerCalls();
  const ctx = { waitUntil: () => {} };
  return worker.fetch(new Request(`${ORIGIN}${path}`, init), env, ctx);
}

async function runScheduled(env) {
  const waits = [];
  await worker.scheduled({ scheduledTime: Date.now() }, env, {
    waitUntil: (p) => waits.push(p),
  });
  return Promise.allSettled(waits);
}

function seedCell(kv, name = "cell-a", extra = {}) {
  kv.map.set(
    `cell:${name}`,
    JSON.stringify({ endpoint: CELL_ENDPOINT, provision_token: PROVISION_TOKEN, ...extra }),
  );
}

async function seedVerifyEntry(kv, accountId = "acct_1", cell = "cell-a") {
  await kv.put(
    `verify:${await sha256hex(TOKEN)}`,
    JSON.stringify({ account_id: accountId, cell, created_at: new Date().toISOString() }),
    { expirationTtl: 7 * 24 * 3600 },
  );
}

function seedPending(kv, accountId = "acct_1", state = {}) {
  kv.map.set(
    `pending:${accountId}`,
    JSON.stringify({
      cell: "cell-a",
      created_at: new Date(Date.now() - 10 * 60 * 1000).toISOString(),
      route_epoch: 0,
      provision_id: "prov-1",
      emails_sent: 1,
      last_email_at: new Date(Date.now() - 10 * 60 * 1000).toISOString(),
      ...state,
    }),
  );
}

function seedAcct(kv, accountId = "acct_1", cell = "cell-a") {
  kv.map.set(`acct:${accountId}`, JSON.stringify({ cell, endpoint: CELL_ENDPOINT }));
}

// ---- Signup request boundary ----------------------------------------------

test("signup accepts only POST at the boundary", async () => {
  const { env } = makeEnv();
  const resp = await run(env, "/v1/accounts", { method: "GET" });
  assert.equal(resp.status, 405);
  assert.equal((await resp.json()).error, "method not allowed");
  assert.deepEqual(containerCalls, []);
});

test("signup rejects an unparseable JSON body with a bounded error", async () => {
  const { env } = makeEnv();
  const resp = await run(env, "/v1/accounts", { method: "POST", body: "{not json" });
  assert.equal(resp.status, 400);
  const body = await resp.json();
  assert.deepEqual(Object.keys(body).sort(), ["error", "schema_version"]);
  assert.equal(body.error, "invalid JSON body");
});

test("signup rejects malformed provision ids before the durable authority", async () => {
  let doCalls = 0;
  const { env } = makeEnv({
    ACCOUNT_SIGNUP: signupNamespace(async () => {
      doCalls += 1;
      return Response.json({});
    }),
  });
  for (const provisionID of ["", "bad id", "a".repeat(129), "sneaky/slash"]) {
    const resp = await run(env, "/v1/accounts", {
      method: "POST",
      body: JSON.stringify({ provision_id: provisionID }),
    });
    assert.equal(resp.status, 400);
  }
  assert.equal(doCalls, 0);
});

test("signup without the durable-object binding is unavailable, never the cold path", async () => {
  const { env } = makeEnv({ ACCOUNT_SIGNUP: undefined });
  const resp = await run(env, "/v1/accounts", {
    method: "POST",
    body: JSON.stringify({ provision_id: "prov-1" }),
  });
  assert.equal(resp.status, 503);
  assert.deepEqual(containerCalls, []);
});

test("signup forwards the normalized provision id and the exact request origin", async () => {
  let received = null;
  let doName = null;
  const { env } = makeEnv({
    ACCOUNT_SIGNUP: signupNamespace(async (id, request) => {
      doName = id.name;
      received = await request.json();
      return Response.json({ ok: true }, { status: 201 });
    }),
  });
  const resp = await run(env, "/v1/accounts", {
    method: "POST",
    body: JSON.stringify({ provision_id: "  prov-9  ", email: "o@example.test", invite: "iv" }),
  });
  assert.equal(resp.status, 201);
  assert.equal(doName, "provision:prov-9");
  assert.equal(received.provision_id, "prov-9");
  assert.equal(received.origin, ORIGIN);
  assert.equal(received.email, "o@example.test");
});

test("an ambiguous durable-object failure is a bounded 502", async () => {
  const { env } = makeEnv({
    ACCOUNT_SIGNUP: signupNamespace(async () => {
      throw new Error("socket torn");
    }),
  });
  const resp = await run(env, "/v1/accounts", {
    method: "POST",
    body: JSON.stringify({ provision_id: "prov-1", email: "secret@example.test" }),
  });
  assert.equal(resp.status, 502);
  const body = await resp.json();
  assert.deepEqual(Object.keys(body).sort(), ["error", "schema_version"]);
  assert.match(body.error, /^account signup outcome is ambiguous/);
  assert.ok(!body.error.includes("secret@example.test"));
});

test("unknown verbs fall through to the cold path; matched lifecycle routes never do", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  mockCell(t, {}); // the :recover probe's cell round trip must stay in-process
  const cold = await run(env, "/v1/accounts/acct_1:unknown-verb", { method: "POST" });
  assert.equal(cold.status, 599);
  assert.deepEqual(containerCalls, ["POST /v1/accounts/acct_1:unknown-verb"]);

  for (const [path, init] of [
    ["/v1/accounts/acct_1:resend-verification", { method: "POST" }],
    ["/v1/accounts/acct_1:recover", { method: "POST", body: "{}" }],
    ["/v1/accounts/acct_1:change-email", { method: "POST", body: "{}" }],
    [`/verify/${TOKEN}`, { method: "GET" }],
    [`/undo-email/${TOKEN}`, { method: "GET" }],
  ]) {
    const resp = await run(env, path, init);
    assert.notEqual(resp.status, 599, `${path} must terminate at the Worker`);
    assert.deepEqual(containerCalls, [], `${path} reached the cold path`);
  }
});

// ---- Email verification ----------------------------------------------------

test("a valid verification link activates once and stands the reaper down", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  seedPending(kv);
  const calls = mockCell(t, {
    ":activate": () =>
      cellJSON({ status: "active", account_id: "acct_1", activated: true }),
  });
  const resp = await run(env, `/verify/${TOKEN}`);
  assert.equal(resp.status, 200);
  assert.match(await resp.text(), /Account verified/);
  const activates = calls.filter((c) => c.path.endsWith(":activate"));
  assert.equal(activates.length, 1);
  assert.equal(activates[0].init.headers.Authorization, `Bearer ${PROVISION_TOKEN}`);
  assert.equal(kv.json("pending:acct_1"), null);
  assert.notEqual(kv.json(`verify:${await sha256hex(TOKEN)}`), null);
});

test("a lost success response is recovered by exactly retrying the same link", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  seedPending(kv);
  let activations = 0;
  mockCell(t, {
    ":activate": () => {
      activations += 1;
      return cellJSON({
        status: "active",
        account_id: "acct_1",
        activated: activations === 1,
      });
    },
  });
  const first = await run(env, `/verify/${TOKEN}`);
  assert.equal(first.status, 200);
  const retry = await run(env, `/verify/${TOKEN}`);
  assert.equal(retry.status, 200);
  assert.match(await retry.text(), /Already verified/);
  assert.equal(activations, 2);
});

test("concurrent confirmation clicks both land on the cell's idempotent activation with no extra mutation", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  seedPending(kv);
  let activations = 0;
  const calls = mockCell(t, {
    ":activate": () => {
      activations += 1;
      return cellJSON({
        status: "active",
        account_id: "acct_1",
        activated: activations === 1,
      });
    },
  });
  const [a, b] = await Promise.all([
    run(env, `/verify/${TOKEN}`),
    worker.fetch(new Request(`${ORIGIN}/verify/${TOKEN}`), env, { waitUntil: () => {} }),
  ]);
  assert.equal(a.status, 200);
  assert.equal(b.status, 200);
  assert.equal(activations, 2);
  assert.deepEqual(
    calls.filter((c) => !c.path.endsWith(":activate")),
    [],
    "verification must touch nothing on the cell beyond :activate",
  );
  assert.equal(kv.json("pending:acct_1"), null);
});

test("an unknown or expired token is a 404 page with zero cell traffic", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  const calls = mockCell(t, {});
  const resp = await run(env, `/verify/${"cd".repeat(32)}`);
  assert.equal(resp.status, 404);
  const page = await resp.text();
  assert.ok(!page.includes("cd".repeat(32)));
  assert.deepEqual(calls, []);
});

test("a tampered verify entry cannot splice an invalid account id into a cell URL", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await kv.put(
    `verify:${await sha256hex(TOKEN)}`,
    JSON.stringify({ account_id: "../evil", cell: "cell-a" }),
  );
  const calls = mockCell(t, {});
  const resp = await run(env, `/verify/${TOKEN}`);
  assert.equal(resp.status, 404);
  assert.deepEqual(calls, []);
});

test("a malformed verification token never reaches the verify handler", async () => {
  const { env } = makeEnv();
  for (const bad of [TOKEN.slice(0, 63), TOKEN.toUpperCase(), `${TOKEN}00`]) {
    const resp = await run(env, `/verify/${bad}`);
    assert.equal(resp.status, 599, "malformed tokens fall through to the cold path");
  }
});

test("verification stays retryable while the cell record or cell is unavailable", async (t) => {
  const { env, kv } = makeEnv();
  await seedVerifyEntry(kv); // deliberately no cell: record
  seedPending(kv);
  mockCell(t, {});
  const noCell = await run(env, `/verify/${TOKEN}`);
  assert.equal(noCell.status, 503);

  seedCell(kv);
  t.mock.method(globalThis, "fetch", async () => {
    throw new Error("cell down");
  });
  const outage = await run(env, `/verify/${TOKEN}`);
  assert.equal(outage.status, 503);
  assert.notEqual(kv.json(`verify:${await sha256hex(TOKEN)}`), null);
  assert.notEqual(kv.json("pending:acct_1"), null);
});

test("only the cell's exact dead-link answers burn a verification link", async (t) => {
  const cases = [
    [409, { error: "account cannot be activated" }, 410, true],
    [404, { error: "account not found" }, 404, true],
    [404, { error: "no such route" }, 503, false], // old dispatcher: stays retryable
  ];
  for (const [cellStatus, cellBody, wantStatus, wantBurned] of cases) {
    const { env, kv } = makeEnv();
    seedCell(kv);
    await seedVerifyEntry(kv);
    mockCell(t, { ":activate": () => cellJSON(cellBody, cellStatus) });
    const resp = await run(env, `/verify/${TOKEN}`);
    assert.equal(resp.status, wantStatus);
    const kept = kv.json(`verify:${await sha256hex(TOKEN)}`) !== null;
    assert.equal(!kept, wantBurned, `cell ${cellStatus} ${cellBody.error}`);
  }
});

test("a 200 answer naming a different account is never treated as an activation", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  seedPending(kv);
  mockCell(t, {
    ":activate": () => cellJSON({ status: "active", account_id: "acct_other", activated: true }),
  });
  const resp = await run(env, `/verify/${TOKEN}`);
  assert.equal(resp.status, 503, "a misrouted answer stays retryable");
  assert.notEqual(kv.json(`verify:${await sha256hex(TOKEN)}`), null);
  assert.notEqual(kv.json("pending:acct_1"), null, "the reaper must not stand down");
});

test("a non-JSON cell answer is never treated as an activation", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  mockCell(t, {
    ":activate": () => new Response("<html>load balancer</html>", { status: 200 }),
  });
  const resp = await run(env, `/verify/${TOKEN}`);
  assert.equal(resp.status, 503);
  assert.notEqual(kv.json(`verify:${await sha256hex(TOKEN)}`), null);
});

test("verification error pages never echo the token, account id, or endpoint", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedVerifyEntry(kv);
  mockCell(t, { ":activate": () => cellJSON({ error: "account cannot be activated" }, 409) });
  const resp = await run(env, `/verify/${TOKEN}`);
  const page = await resp.text();
  assert.ok(!page.includes(TOKEN));
  assert.ok(!page.includes("acct_1"));
  assert.ok(!page.includes(CELL_ENDPOINT));
});

// ---- Verification resend ---------------------------------------------------

function resend(env, accountId = "acct_1", init = {}) {
  return run(env, `/v1/accounts/${accountId}:resend-verification`, {
    method: "POST",
    headers: { Authorization: "Bearer operator-token" },
    ...init,
  });
}

test("resend is method-, auth-, and existence-bounded", async (t) => {
  const { env, kv } = makeEnv();
  const calls = mockCell(t, {});
  const wrongMethod = await run(env, "/v1/accounts/acct_1:resend-verification", { method: "GET" });
  assert.equal(wrongMethod.status, 405);
  const noAuth = await run(env, "/v1/accounts/acct_1:resend-verification", { method: "POST" });
  assert.equal(noAuth.status, 401);
  const unknown = await resend(env, "acct_missing");
  assert.equal(unknown.status, 404);
  assert.deepEqual(calls, []);
  assert.equal(kv.keysWithPrefix("verify:").length, 0);
});

test("the resend cap and cooldown refuse before any cell round trip", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedPending(kv, "acct_1", { emails_sent: 5 });
  const calls = mockCell(t, {});
  const capped = await resend(env);
  assert.equal(capped.status, 429);

  seedPending(kv, "acct_1", {
    emails_sent: 2,
    last_email_at: new Date(Date.now() - 60 * 1000).toISOString(),
  });
  const cooled = await resend(env);
  assert.equal(cooled.status, 429);
  assert.deepEqual(calls, []);
  assert.equal(email.sent.length, 0);
});

test("a cell auth refusal passes through verbatim", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedPending(kv);
  mockCell(t, {
    "/v1/account": () => new Response('{"error":"cell says no"}', { status: 401 }),
  });
  const resp = await resend(env);
  assert.equal(resp.status, 401);
  assert.deepEqual(await resp.json(), { error: "cell says no" });
});

test("a token for a different account or a non-pending account is refused", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedPending(kv);
  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_other", status: "pending" } }),
  });
  assert.equal((await resend(env)).status, 403);

  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "active", email: "o@x.test" } }),
  });
  const active = await resend(env);
  assert.equal(active.status, 409);
  assert.match((await active.json()).error, /already active/);
});

test("a missing pending candidate fails closed with no unmetered email", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "o@x.test" } }),
  });
  const resp = await resend(env);
  assert.equal(resp.status, 503);
  assert.equal(email.sent.length, 0);
});

test("a successful resend advances the durable counter without resetting the reap clock", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  const createdAt = new Date(Date.now() - 30 * 60 * 1000).toISOString();
  seedPending(kv, "acct_1", { emails_sent: 2, created_at: createdAt });
  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "owner@example.test" } }),
  });
  const resp = await resend(env);
  assert.equal(resp.status, 200);
  const body = await resp.json();
  assert.equal(body.verification_email_sent, true);
  assert.equal(body.account_id, "acct_1");

  const pending = kv.json("pending:acct_1");
  assert.equal(pending.emails_sent, 3, "the send cap must advance durably");
  assert.equal(pending.created_at, createdAt, "resend must never reset the reap clock");
  assert.equal(email.sent.length, 1);
  assert.equal(email.sent[0].to, "owner@example.test");
  assert.match(email.sent[0].text, /fresh verification link/);

  const verifyKeys = kv.keysWithPrefix("verify:");
  assert.equal(verifyKeys.length, 1);
  assert.equal(kv.ttls.get(verifyKeys[0]), 7 * 24 * 3600);
});

test("resend audit events carry only masked addresses", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  seedPending(kv, "acct_1", { emails_sent: 1 });
  const calls = mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "owner@example.test" } }),
  });
  assert.equal((await resend(env)).status, 200);
  const events = calls.filter((c) => c.path.endsWith(":events"));
  assert.ok(events.length >= 1, "the lifecycle send must be audited");
  for (const event of events) {
    assert.ok(
      !String(event.init.body).includes("owner@example.test"),
      "audit metadata must never carry the raw address",
    );
  }
});

test("an immediate retry after a successful resend hits the durable cooldown", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  seedPending(kv, "acct_1", { emails_sent: 1 });
  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "o@x.test" } }),
  });
  assert.equal((await resend(env)).status, 200);
  const retry = await resend(env);
  assert.equal(retry.status, 429);
  assert.equal(email.sent.length, 1, "a lost-response retry must not duplicate email");
});

test("a failed or unconfigured email send burns no quota", async (t) => {
  for (const breakEmail of ["throw", "absent"]) {
    const { env, kv, email } = makeEnv(
      breakEmail === "absent" ? { EMAIL: undefined } : {},
    );
    seedAcct(kv);
    seedCell(kv);
    seedPending(kv, "acct_1", { emails_sent: 2 });
    if (breakEmail === "throw") {
      email.failNextSends = 1;
    }
    mockCell(t, {
      "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "o@x.test" } }),
    });
    const resp = await resend(env);
    assert.equal(resp.status, 502, breakEmail);
    assert.equal(kv.json("pending:acct_1").emails_sent, 2, breakEmail);
  }
});

test("concurrent resends at the cap cannot conjure extra email", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  seedPending(kv, "acct_1", { emails_sent: 5 });
  mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "o@x.test" } }),
  });
  const [a, b] = await Promise.all([resend(env), resend(env)]);
  assert.equal(a.status, 429);
  assert.equal(b.status, 429);
  assert.equal(email.sent.length, 0);
});

test("resend performs no durable lifecycle transition", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  seedPending(kv);
  const acctBefore = kv.map.get("acct:acct_1");
  const calls = mockCell(t, {
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "pending", email: "o@x.test" } }),
  });
  assert.equal((await resend(env)).status, 200);
  const mutating = calls.filter(
    (c) => c.path.endsWith(":activate") || c.path.endsWith(":reap") || c.path.endsWith(":update-email"),
  );
  assert.deepEqual(mutating, []);
  assert.equal(kv.map.get("acct:acct_1"), acctBefore);
});

// ---- Pending-signup expiration ---------------------------------------------

test("the reaper policy route is fleet-gated and method-bounded", async () => {
  const { env } = makeEnv();
  const anon = await run(env, "/v1/reaper");
  assert.equal(anon.status, 401);
  const wrong = await run(env, "/v1/reaper", {
    headers: { Authorization: "Bearer wrong-token" },
  });
  assert.equal(wrong.status, 401);
  const del = await run(env, "/v1/reaper", {
    method: "DELETE",
    headers: { Authorization: `Bearer ${FLEET_TOKEN}` },
  });
  assert.equal(del.status, 405);
});

test("the reaper policy round-trips through strict validation", async () => {
  const { env } = makeEnv();
  const auth = { Authorization: `Bearer ${FLEET_TOKEN}` };
  const initial = await run(env, "/v1/reaper", { headers: auth });
  assert.deepEqual((await initial.json()).reaper, { enabled: false });

  for (const bad of [
    { enabled: "yes" },
    { enabled: true },
    { enabled: true, ttl_minutes: 0 },
    { enabled: true, ttl_minutes: "60" },
  ]) {
    const resp = await run(env, "/v1/reaper", {
      method: "POST",
      headers: auth,
      body: JSON.stringify(bad),
    });
    assert.equal(resp.status, 400, JSON.stringify(bad));
  }

  const set = await run(env, "/v1/reaper", {
    method: "POST",
    headers: auth,
    body: JSON.stringify({ enabled: true, ttl_minutes: 60 }),
  });
  assert.equal(set.status, 200);
  const readback = await run(env, "/v1/reaper", { headers: auth });
  assert.deepEqual((await readback.json()).reaper, { enabled: true, ttl_minutes: 60 });
});

function seedReaper(kv, ttlMinutes = 60) {
  kv.map.set("config:reaper", JSON.stringify({ enabled: true, ttl_minutes: ttlMinutes }));
}

function expiredPending(kv, accountId, extra = {}) {
  seedPending(kv, accountId, {
    created_at: new Date(Date.now() - 2 * 3600 * 1000).toISOString(),
    ...extra,
  });
}

test("a disabled reaper sweep touches nothing", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  const calls = mockCell(t, {});
  await runScheduled(env);
  assert.deepEqual(calls.filter((c) => c.path.endsWith(":reap")), []);
  assert.notEqual(kv.json("pending:acct_1"), null);
  assert.notEqual(kv.json("acct:acct_1"), null);
});

test("the sweep pages through all candidates, reaps only expired ones, and never touches invitations", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv, "acct_old_a");
  seedAcct(kv, "acct_old_b");
  seedAcct(kv, "acct_new");
  // Three candidates span two KV list pages (the fake pages at 2 keys), so
  // the sweep's cursor loop is exercised end to end.
  expiredPending(kv, "acct_old_a");
  expiredPending(kv, "acct_old_b");
  seedPending(kv, "acct_new"); // 10 minutes old — inside the window
  kv.map.set("invite:friends-2026", JSON.stringify({ uses: 3 }));
  const calls = mockCell(t, {
    ":reap": (url) => {
      const accountId = url.pathname.split("/").pop().replace(":reap", "");
      return cellJSON({ status: "closed", account_id: accountId });
    },
  });
  await runScheduled(env);
  const reaps = calls.filter((c) => c.path.endsWith(":reap")).map((c) => c.path);
  assert.equal(reaps.length, 2);
  assert.ok(reaps.some((p) => p.endsWith("acct_old_a:reap")));
  assert.ok(reaps.some((p) => p.endsWith("acct_old_b:reap")));
  assert.equal(kv.json("pending:acct_old_a"), null);
  assert.equal(kv.json("acct:acct_old_a"), null);
  assert.equal(kv.json("pending:acct_old_b"), null);
  assert.equal(kv.json("acct:acct_old_b"), null);
  assert.notEqual(kv.json("pending:acct_new"), null);
  assert.notEqual(kv.json("acct:acct_new"), null);
  assert.notEqual(kv.json("invite:friends-2026"), null);
});

test("only the cell's exact unknown-account answer drops a candidate, and routing survives it", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  mockCell(t, { ":reap": () => cellJSON({ error: "account not found" }, 404) });
  await runScheduled(env);
  assert.equal(kv.json("pending:acct_1"), null, "the exact-string 404 drops the candidate");
  assert.notEqual(kv.json("acct:acct_1"), null, "routing is never deleted on the 404 arm");

  // A bare 404 (old cell dispatcher) must stay retryable.
  seedAcct(kv, "acct_2");
  expiredPending(kv, "acct_2");
  mockCell(t, { ":reap": () => cellJSON({ error: "no such route" }, 404) });
  await runScheduled(env);
  assert.notEqual(kv.json("pending:acct_2"), null);
  assert.notEqual(kv.json("acct:acct_2"), null);
});

function coordinatorNamespace(impl) {
  return {
    idFromName: (name) => ({ name }),
    get: (id) => ({ fetch: async (request) => impl(id, request) }),
  };
}

test("a registered cell's reap hands occupancy off before forgetting the account", async (t) => {
  const departs = [];
  const { env, kv } = makeEnv({
    CELL_COORDINATOR: coordinatorNamespace(async (id, request) => {
      departs.push({ cell: id.name, body: await request.json() });
      return Response.json({ ok: true });
    }),
  });
  seedReaper(kv);
  seedCell(kv, "cell-a", { registration_id: "reg-1" });
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  mockCell(t, { ":reap": () => cellJSON({ status: "closed", account_id: "acct_1" }) });
  await runScheduled(env);
  assert.equal(departs.length, 1);
  assert.equal(departs[0].cell, "cell-a");
  assert.equal(departs[0].body.account_id, "acct_1");
  assert.equal(departs[0].body.registration_id, "reg-1");
  assert.equal(departs[0].body.source_epoch, 0);
  assert.equal(kv.json("pending:acct_1"), null);
  assert.equal(kv.json("acct:acct_1"), null);
});

test("a failed occupancy handoff defers the candidate instead of forgetting the account", async (t) => {
  const { env, kv } = makeEnv({
    CELL_COORDINATOR: coordinatorNamespace(async () =>
      Response.json({ error: "coordinator busy" }, { status: 503 })),
  });
  seedReaper(kv);
  seedCell(kv, "cell-a", { registration_id: "reg-1" });
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  mockCell(t, { ":reap": () => cellJSON({ status: "closed", account_id: "acct_1" }) });
  await runScheduled(env);
  assert.notEqual(kv.json("pending:acct_1"), null, "the candidate retries next tick");
  assert.notEqual(kv.json("acct:acct_1"), null, "routing survives a failed handoff");
});

test("a just-activated account is never reclaimed", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  mockCell(t, { ":reap": () => cellJSON({ error: "account is active" }, 409) });
  await runScheduled(env);
  assert.equal(kv.json("pending:acct_1"), null, "the stale candidate is dropped");
  assert.notEqual(kv.json("acct:acct_1"), null, "live routing must survive");
});

test("a stray 200 without the exact acknowledgement destroys nothing", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  mockCell(t, { ":reap": () => cellJSON({ hello: "captive portal" }) });
  await runScheduled(env);
  assert.notEqual(kv.json("pending:acct_1"), null);
  assert.notEqual(kv.json("acct:acct_1"), null);
});

test("a candidate with newer route authority is dropped without touching the route", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  kv.map.set("acct:acct_1", JSON.stringify({ cell: "cell-b", endpoint: "https://b.test.invalid" }));
  expiredPending(kv, "acct_1"); // candidate still points at cell-a
  mockCell(t, { ":reap": () => cellJSON({ status: "closed", account_id: "acct_1" }) });
  await runScheduled(env);
  assert.equal(kv.json("pending:acct_1"), null);
  assert.deepEqual(kv.json("acct:acct_1"), { cell: "cell-b", endpoint: "https://b.test.invalid" });
});

test("an unreachable cell defers all its candidates with a single attempt", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv, "acct_a");
  seedAcct(kv, "acct_b");
  expiredPending(kv, "acct_a");
  expiredPending(kv, "acct_b");
  let attempts = 0;
  t.mock.method(globalThis, "fetch", async () => {
    attempts += 1;
    throw new Error("cell down");
  });
  await runScheduled(env);
  assert.equal(attempts, 1, "one timeout per dead cell per sweep");
  assert.notEqual(kv.json("pending:acct_a"), null);
  assert.notEqual(kv.json("pending:acct_b"), null);
});

test("sweep skips garbage loudly and its logs stay value-free", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv, "acct_ok");
  expiredPending(kv, "acct_ok");
  seedPending(kv, "acct_bad", { created_at: "not-a-date" });
  mockCell(t, { ":reap": () => cellJSON({ status: "closed", account_id: "acct_ok" }) });
  const logs = [];
  t.mock.method(console, "log", (line) => logs.push(String(line)));
  await runScheduled(env);
  assert.notEqual(kv.json("pending:acct_bad"), null);
  assert.ok(logs.some((l) => l.includes("unparseable created_at")));
  for (const line of logs) {
    assert.ok(!line.includes("@"), `log leaked an address shape: ${line}`);
    assert.ok(!/[0-9a-f]{64}/.test(line), `log leaked a token shape: ${line}`);
  }
});

test("a completed sweep is exactly retryable and then quiescent", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  const calls = mockCell(t, {
    ":reap": () => cellJSON({ status: "closed", account_id: "acct_1" }),
  });
  await runScheduled(env);
  const afterFirst = calls.filter((c) => c.path.endsWith(":reap")).length;
  assert.equal(afterFirst, 1);
  await runScheduled(env);
  assert.equal(calls.filter((c) => c.path.endsWith(":reap")).length, 1, "nothing left to reap");
});

test("sweep interruption before the route delete recovers by exact retry", async (t) => {
  const { env, kv } = makeEnv();
  seedReaper(kv);
  seedCell(kv);
  seedAcct(kv);
  expiredPending(kv, "acct_1");
  kv.failDeletes.add("acct:acct_1");
  mockCell(t, {
    ":reap": () => cellJSON({ status: "closed", account_id: "acct_1" }),
  });
  const settled = await runScheduled(env);
  assert.ok(settled.some((s) => s.status === "rejected"), "the interrupted sweep surfaces");
  assert.notEqual(kv.json("pending:acct_1"), null, "the candidate survives the crash");

  await runScheduled(env);
  assert.equal(kv.json("pending:acct_1"), null);
  assert.equal(kv.json("acct:acct_1"), null);
});
