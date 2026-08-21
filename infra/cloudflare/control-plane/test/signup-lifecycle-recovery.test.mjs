// Request-boundary contract tests for managed-account credential recovery,
// email change, and the undo window. Every test drives the Worker's real
// default-exported fetch against in-memory fakes: no network, no Cloudflare
// account, no email provider, no cells, no real identifiers.
import assert from "node:assert/strict";
import test from "node:test";
import { register } from "node:module";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
const { containerCalls, resetContainerCalls } = await import(
  "./fixtures/cloudflare-containers-stub.mjs"
);

const ORIGIN = "https://cp.test.invalid";
const CELL_ENDPOINT = "https://cell-a.test.invalid";
const PROVISION_TOKEN = "prov-cell-token";
const OWNER_EMAIL = "owner@example.test";
const NEW_EMAIL = "fresh@example.test";
const UNDO_TOKEN = "ef".repeat(32);

async function sha256hex(s) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

class KVFake {
  constructor() {
    this.map = new Map();
    this.ttls = new Map();
    this.failDeletes = new Set();
    this.failPuts = []; // {prefix, skip}: fail the (skip+1)-th put matching prefix
  }

  failPutMatching(prefix, skip = 0) {
    this.failPuts.push({ prefix, skip });
  }

  async get(key, opts) {
    const raw = this.map.get(key);
    if (raw === undefined) {
      return null;
    }
    return opts?.type === "json" ? JSON.parse(raw) : raw;
  }

  async put(key, value, opts) {
    for (const injected of this.failPuts) {
      if (key.startsWith(injected.prefix)) {
        if (injected.skip > 0) {
          injected.skip -= 1;
          break;
        }
        this.failPuts.splice(this.failPuts.indexOf(injected), 1);
        throw new Error("injected KV put failure");
      }
    }
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

  async list({ prefix = "", cursor } = {}) {
    void cursor;
    const keys = [...this.map.keys()]
      .filter((k) => k.startsWith(prefix))
      .sort()
      .map((name) => ({ name }));
    return { keys, list_complete: true, cursor: undefined };
  }

  json(key) {
    const raw = this.map.get(key);
    return raw === undefined ? null : JSON.parse(raw);
  }

  keysWithPrefix(prefix) {
    return [...this.map.keys()].filter((k) => k.startsWith(prefix)).sort();
  }

  dump() {
    return [...this.map.entries()].flat().join("\n");
  }
}

class EmailFake {
  constructor() {
    this.sent = [];
    this.failSendsTo = new Set();
    this.failNextSends = 0;
    this.failError = "injected email delivery failure";
  }

  async send(message) {
    if (this.failNextSends > 0) {
      this.failNextSends -= 1;
      throw new Error(this.failError);
    }
    if (this.failSendsTo.has(message.to)) {
      throw new Error(this.failError);
    }
    this.sent.push(message);
  }
}

// A provider-error fixture shaped like the worst real rejections: recipient
// addresses, token-shaped hex, newline injection, and structured data. None
// of it may ever reach a log line or a response body.
const MALICIOUS_PROVIDER_ERROR =
  `550 mailbox <victim-leak@example.test> rejected token=${"ab".repeat(32)}\n` +
  `X-INJECTED-LOG-LINE MALICIOUS-MARKER\n` +
  `{"provider":"evil","recipient":"victim-leak@example.test"}`;

function makeEnv(overrides = {}) {
  const kv = new KVFake();
  const email = new EmailFake();
  const env = { DIRECTORY: kv, EMAIL: email, ...overrides };
  return { env, kv, email };
}

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
  return worker.fetch(new Request(`${ORIGIN}${path}`, init), env, { waitUntil: () => {} });
}

function seedCell(kv, name = "cell-a") {
  kv.map.set(
    `cell:${name}`,
    JSON.stringify({ endpoint: CELL_ENDPOINT, provision_token: PROVISION_TOKEN }),
  );
}

function seedAcct(kv, accountId = "acct_1", cell = "cell-a") {
  kv.map.set(`acct:${accountId}`, JSON.stringify({ cell, endpoint: CELL_ENDPOINT }));
}

function extractCode(message) {
  const match = message.text.match(/(\d{3}-\d{3}-\d{3})/);
  assert.ok(match, "email must carry a formatted code");
  return match[1];
}

// ---- Credential recovery: request mode -------------------------------------

function recover(env, accountId = "acct_1", body = {}, headers = {}) {
  return run(env, `/v1/accounts/${accountId}:recover`, {
    method: "POST",
    headers: { "CF-Connecting-IP": "198.51.100.7", ...headers },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });
}

function activeContactHandlers(extra = {}) {
  return {
    ":contact": () => cellJSON({ email: OWNER_EMAIL, status: "active" }),
    ...extra,
  };
}

test("recover accepts only POST and tolerates an unparseable body as request mode", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  const wrongMethod = await run(env, "/v1/accounts/acct_1:recover", { method: "GET" });
  assert.equal(wrongMethod.status, 405);
  const garbage = await recover(env, "acct_1", "not json at all");
  assert.equal(garbage.status, 200);
  assert.match((await garbage.json()).message, /if the account exists/);
});

test("phantom and real accounts answer byte-identically and both pay rate state", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv, "acct_real");
  seedCell(kv);
  const calls = mockCell(t, activeContactHandlers());
  const real = await recover(env, "acct_real");
  const realBody = await real.text();
  const phantom = await recover(env, "acct_ghost");
  const phantomBody = await phantom.text();
  assert.equal(real.status, phantom.status);
  assert.equal(realBody, phantomBody, "enumeration must find no oracle in the body");
  assert.equal(email.sent.length, 1, "only the real account gets a code");
  assert.notEqual(kv.json("recover:acct_real"), null);
  assert.notEqual(kv.json("recover:acct_ghost"), null, "phantom probes are never free");
  const contacts = calls.filter((c) => c.path.endsWith(":contact"));
  assert.equal(contacts.length, 2, "the cell round trip happens for phantoms too");
});

test("the edge limiter refuses before any durable or cell work", async (t) => {
  const { env, kv } = makeEnv({
    RECOVER_LIMITER: { limit: async () => ({ success: false }) },
  });
  seedAcct(kv);
  seedCell(kv);
  const calls = mockCell(t, activeContactHandlers());
  const resp = await recover(env);
  assert.equal(resp.status, 429);
  assert.equal(kv.json("recover:acct_1"), null);
  assert.deepEqual(calls, []);
});

test("per-ip and per-account caps share one indistinguishable refusal", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  kv.map.set(
    "recover-ip:acct_1:198.51.100.7",
    JSON.stringify({ emails_sent: 3 }),
  );
  const perIp = await recover(env);
  assert.equal(perIp.status, 429);
  const perIpBody = await perIp.text();

  const { env: env2, kv: kv2 } = makeEnv();
  seedAcct(kv2);
  seedCell(kv2);
  kv2.map.set("recover:acct_1", JSON.stringify({ emails_sent: 10 }));
  const perAccount = await recover(env2);
  assert.equal(perAccount.status, 429);
  assert.equal(await perAccount.text(), perIpBody, "cap refusals must be one oracle-free string");
});

test("the recovery cooldown refuses without naming the account", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  kv.map.set(
    "recover:acct_1",
    JSON.stringify({ emails_sent: 1, last_email_at: new Date(Date.now() - 60 * 1000).toISOString() }),
  );
  const resp = await recover(env);
  assert.equal(resp.status, 429);
  const body = await resp.text();
  assert.match(body, /just sent/);
  assert.ok(!body.includes("acct_1"));
});

test("a recovery code send persists only hashes with the documented lifetimes", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  const resp = await recover(env);
  assert.equal(resp.status, 200);
  assert.equal(email.sent.length, 1);
  assert.equal(email.sent[0].to, OWNER_EMAIL);
  const code = extractCode(email.sent[0]);

  const state = kv.json("recover:acct_1");
  assert.equal(state.emails_sent, 1);
  assert.equal(state.attempts, 0);
  assert.match(state.code_hash, /^[0-9a-f]{64}$/);
  assert.equal(state.code_hash, await sha256hex(code.replaceAll("-", "")));
  const msUntilExpiry = Date.parse(state.code_expires_at) - Date.now();
  assert.ok(msUntilExpiry > 13 * 60 * 1000 && msUntilExpiry <= 15 * 60 * 1000);
  const codePersistTtl = kv.ttls.get("recover:acct_1");
  assert.ok(
    codePersistTtl <= 4 * 3600 && codePersistTtl >= 4 * 3600 - 5,
    `code persist ttl ${codePersistTtl} must sit at the window boundary`,
  );
  const stored = kv.dump();
  assert.ok(!stored.includes(code), "the raw dashed code must never be stored");
  assert.ok(!stored.includes(code.replaceAll("-", "")), "the raw stripped code must never be stored");
});

test("a provider outage burns the slot uniformly and stays indistinguishable", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv, "acct_real");
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  email.failNextSends = 1;
  const real = await recover(env, "acct_real");
  assert.equal(real.status, 200);
  const realBody = await real.text();
  const realState = kv.json("recover:acct_real");
  assert.equal(
    realState.emails_sent,
    1,
    "the slot stays spent on a send failure — refunding only observable-real requests would be an existence oracle",
  );
  assert.equal(realState.code_hash, undefined);
  assert.ok(realState.last_email_at, "the cooldown paces retries identically to success");

  // A phantom probe during the same outage is metered exactly the same way
  // and answers byte-identically: quota treatment never depends on whether
  // the account exists.
  const phantom = await recover(env, "acct_ghost");
  assert.equal(await phantom.text(), realBody);
  assert.equal(kv.json("recover:acct_ghost").emails_sent, 1);

  // Once the provider recovers, the owner still has window quota left.
  realState.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  kv.map.set("recover:acct_real", JSON.stringify(realState));
  const retry = await recover(env, "acct_real");
  assert.equal(await retry.text(), realBody, "outage and success answer alike");
  assert.equal(email.sent.length, 1, "the retry sends once mail is back");
  assert.equal(kv.json("recover:acct_real").emails_sent, 2);
});

test("an inactive account gets no code but the same generic answer", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, { ":contact": () => cellJSON({ email: OWNER_EMAIL, status: "pending" }) });
  const resp = await recover(env);
  assert.equal(resp.status, 200);
  assert.match((await resp.json()).message, /if the account exists/);
  assert.equal(email.sent.length, 0);
  assert.equal(kv.json("recover:acct_1").code_hash, undefined);
});

// ---- Credential recovery: redeem mode --------------------------------------

async function issueRecoveryCode(t, { env, kv, email }) {
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  const resp = await recover(env);
  assert.equal(resp.status, 200);
  const code = extractCode(email.sent.at(-1));
  return code;
}

function rotatedAccount(overrides = {}) {
  return {
    account: {
      account_id: "acct_1",
      operator_id: "op_1",
      email: OWNER_EMAIL,
      status: "active",
      bootstrap_token: "fresh-bootstrap-token",
      ...overrides,
    },
  };
}

test("a code cannot be guessed into existence", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, {});
  const resp = await recover(env, "acct_1", { code: "123-456-789" });
  assert.equal(resp.status, 401);
});

test("failed attempts count durably before the comparison and cap at five", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  await issueRecoveryCode(t, harness);
  for (let attempt = 1; attempt <= 5; attempt += 1) {
    const resp = await recover(env, "acct_1", { code: "000-000-000" });
    assert.equal(resp.status, 401);
    assert.equal(kv.json("recover:acct_1").attempts, attempt);
  }
  const sixth = await recover(env, "acct_1", { code: "000-000-000" });
  assert.equal(sixth.status, 429);
});

test("an expired code is refused and the attempt still counts", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  const state = kv.json("recover:acct_1");
  state.code_expires_at = new Date(Date.now() - 60 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(state));
  const resp = await recover(env, "acct_1", { code });
  assert.equal(resp.status, 401);
  assert.equal(kv.json("recover:acct_1").attempts, 1);
});

test("a superseded code dies the moment its replacement is issued", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const first = await issueRecoveryCode(t, harness);
  const aged = kv.json("recover:acct_1");
  aged.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(aged));
  mockCell(t, activeContactHandlers({ ":recover": () => cellJSON(rotatedAccount()) }));
  assert.equal((await recover(env)).status, 200);
  const second = extractCode(email.sent.at(-1));
  assert.notEqual(first, second);

  const stale = await recover(env, "acct_1", { code: first });
  assert.equal(stale.status, 401);
  const fresh = await recover(env, "acct_1", { code: second });
  assert.equal(fresh.status, 200);
});

test("a code redeemed against the wrong account is refused", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  seedAcct(kv, "acct_2");
  const code = await issueRecoveryCode(t, harness);
  const resp = await recover(env, "acct_2", { code });
  assert.equal(resp.status, 401);
});

test("a correct redemption rotates exactly once, spends the code, and formats tolerate dashes", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  const calls = mockCell(t, { ":recover": () => cellJSON(rotatedAccount()) });
  const resp = await recover(env, "acct_1", { code: ` ${code} ` });
  assert.equal(resp.status, 200);
  const body = await resp.json();
  assert.equal(body.bootstrap_token, "fresh-bootstrap-token");
  assert.equal(body.account_id, "acct_1");
  assert.equal(body.cell.endpoint, CELL_ENDPOINT);
  assert.equal(kv.json("recover:acct_1"), null, "the code is spent");
  assert.equal(calls.filter((c) => c.path.endsWith(":recover")).length, 1);

  const replay = await recover(env, "acct_1", { code });
  assert.equal(replay.status, 401);
  assert.equal(calls.filter((c) => c.path.endsWith(":recover")).length, 1, "replay never re-rotates");
});

test("a lost redemption response cannot lock the owner out", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const code = await issueRecoveryCode(t, harness);
  mockCell(t, activeContactHandlers({ ":recover": () => cellJSON(rotatedAccount()) }));
  assert.equal((await recover(env, "acct_1", { code })).status, 200);
  assert.equal((await recover(env, "acct_1", { code })).status, 401, "the spent code stays spent");

  const aged = { emails_sent: 1, last_email_at: new Date(Date.now() - 10 * 60 * 1000).toISOString() };
  kv.map.set("recover:acct_1", JSON.stringify(aged));
  assert.equal((await recover(env)).status, 200, "a fresh cycle stays available");
  const newCode = extractCode(email.sent.at(-1));
  assert.equal((await recover(env, "acct_1", { code: newCode })).status, 200);
});

test("an interrupted code-spend recovers by exactly replaying the redemption", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  const calls = mockCell(t, { ":recover": () => cellJSON(rotatedAccount()) });
  kv.failDeletes.add("recover:acct_1");
  await assert.rejects(recover(env, "acct_1", { code }), /injected KV delete failure/);
  assert.equal(calls.filter((c) => c.path.endsWith(":recover")).length, 1);

  const replay = await recover(env, "acct_1", { code });
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).bootstrap_token, "fresh-bootstrap-token");
  assert.equal(kv.json("recover:acct_1"), null);
});

test("a cell that cannot recover keeps the code alive for retry", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  mockCell(t, { ":recover": () => cellJSON({ error: "conflict" }, 409) });
  const conflicted = await recover(env, "acct_1", { code });
  assert.equal(conflicted.status, 409);
  assert.notEqual(kv.json("recover:acct_1").code_hash, undefined);

  t.mock.method(globalThis, "fetch", async () => {
    throw new Error("cell down");
  });
  const outage = await recover(env, "acct_1", { code });
  assert.equal(outage.status, 502);
  assert.notEqual(kv.json("recover:acct_1").code_hash, undefined);
});

test("recovery error paths never leak the code, its hash, or the address", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  const hash = kv.json("recover:acct_1").code_hash;
  mockCell(t, { ":recover": () => cellJSON({ error: "conflict" }, 409) });
  for (const attempt of [
    await recover(env, "acct_1", { code: "999-999-999" }),
    await recover(env, "acct_1", { code }),
  ]) {
    const text = await attempt.text();
    assert.ok(!text.includes(code));
    assert.ok(!text.includes(code.replaceAll("-", "")));
    assert.ok(!text.includes(hash));
    assert.ok(!text.includes(OWNER_EMAIL));
  }
});

// ---- Email change: request mode --------------------------------------------

function changeEmail(env, body, accountId = "acct_1", headers = {}) {
  return run(env, `/v1/accounts/${accountId}:change-email`, {
    method: "POST",
    headers: { Authorization: "Bearer operator-token", ...headers },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });
}

function ownerCellHandlers(extra = {}) {
  return {
    "/v1/account": () =>
      cellJSON({ account: { id: "acct_1", status: "active", email: OWNER_EMAIL } }),
    "/v1/whoami": () => cellJSON({ principal: { operator_id: "op_1" } }),
    "/v1/operators": () => cellJSON({ operators: [{ id: "op_1", is_root: true }] }),
    ...extra,
  };
}

test("change-email is method-, auth-, shape-, and existence-bounded", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  assert.equal((await run(env, "/v1/accounts/acct_1:change-email", { method: "GET" })).status, 405);
  assert.equal(
    (await run(env, "/v1/accounts/acct_1:change-email", { method: "POST", body: "{}" })).status,
    401,
  );
  assert.equal((await changeEmail(env, "{broken")).status, 400);
  assert.equal((await changeEmail(env, {})).status, 400);
  assert.equal((await changeEmail(env, { new_email: "not-an-address" })).status, 400);
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL }, "acct_missing")).status, 404);
});

test("cell refusals, mismatches, and non-active accounts gate the request", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers({
    "/v1/account": () => new Response('{"error":"bad token"}', { status: 401 }),
  }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL })).status, 401);

  mockCell(t, ownerCellHandlers({
    "/v1/account": () => cellJSON({ account: { id: "acct_other", status: "active", email: OWNER_EMAIL } }),
  }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL })).status, 403);

  mockCell(t, ownerCellHandlers({
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "suspended", email: OWNER_EMAIL } }),
  }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL })).status, 409);
});

test("only the account owner may even request a change, and non-owners burn nothing", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers({
    "/v1/operators": () => cellJSON({ operators: [{ id: "op_1", role: "member" }] }),
  }));
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 403);
  assert.equal(email.sent.length, 0);
  assert.equal(kv.json("emailchange:acct_1"), null);
});

test("an account without a prior address routes to support", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers({
    "/v1/account": () => cellJSON({ account: { id: "acct_1", status: "active", email: "" } }),
  }));
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 409);
  assert.match((await resp.json()).error, /support/);
});

test("change-email quota and cooldown are durable state", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  kv.map.set("emailchange:acct_1", JSON.stringify({ emails_sent: 5 }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL })).status, 429);

  kv.map.set(
    "emailchange:acct_1",
    JSON.stringify({ emails_sent: 1, last_email_at: new Date(Date.now() - 60 * 1000).toISOString() }),
  );
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL })).status, 429);
  assert.equal(email.sent.length, 0);
});

test("a successful request codes the new inbox, alarms the old, and ignores unknown fields", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  const resp = await changeEmail(env, { new_email: NEW_EMAIL, unexpected_field: "ignored" });
  assert.equal(resp.status, 200);
  const body = await resp.json();
  assert.equal(body.confirmation_email_sent, true);
  assert.equal(body.notice_sent, true);

  assert.equal(email.sent.length, 2);
  assert.equal(email.sent[0].to, NEW_EMAIL);
  assert.equal(email.sent[1].to, OWNER_EMAIL);
  assert.match(email.sent[1].subject, /Security alert/);

  const state = kv.json("emailchange:acct_1");
  assert.equal(state.new_email, NEW_EMAIL);
  assert.equal(state.emails_sent, 1);
  assert.match(state.code_hash, /^[0-9a-f]{64}$/);
  assert.equal(kv.ttls.get("emailchange:acct_1"), 24 * 3600);
  const code = extractCode(email.sent[0]);
  const stored = kv.dump();
  assert.ok(!stored.includes(code), "the raw dashed code must never be stored");
  assert.ok(!stored.includes(code.replaceAll("-", "")), "the raw stripped code must never be stored");
});

test("a same-address request skips the alarm honestly", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  const resp = await changeEmail(env, { new_email: OWNER_EMAIL });
  assert.equal(resp.status, 200);
  const body = await resp.json();
  assert.equal(body.notice_sent, false);
  assert.equal(body.notice_status, "same_address");
  assert.equal(email.sent.length, 1);
});

test("a failed confirmation send burns no quota and persists no code", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  email.failSendsTo.add(NEW_EMAIL);
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 502);
  assert.equal(kv.json("emailchange:acct_1"), null);
});

test("a failed counter-move alarm revokes the live code fail-closed", async (t) => {
  const { env, kv, email } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  email.failSendsTo.add(OWNER_EMAIL);
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 502);

  const state = kv.json("emailchange:acct_1");
  assert.equal(state.emails_sent, 1, "the code send genuinely happened, so quota stays spent");
  assert.equal(state.code_hash, undefined, "an unalarmed code must not stay redeemable");
  assert.equal(state.new_email, undefined);

  const code = extractCode(email.sent[0]);
  const redeem = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(redeem.status, 401, "the revoked code cannot commit the change");
});

// ---- Email change: redeem mode ---------------------------------------------

async function issueChangeCode(t, harness) {
  const { env, kv, email } = harness;
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, ownerCellHandlers());
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 200);
  return extractCode(email.sent[0]);
}

function commitHandlers(extra = {}) {
  return ownerCellHandlers({
    ":update-email": () => cellJSON({ email: NEW_EMAIL }),
    ...extra,
  });
}

test("redeem demands the exact requested address and exact live code", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueChangeCode(t, harness);
  mockCell(t, commitHandlers());
  assert.equal(
    (await changeEmail(env, { new_email: "other@example.test", code })).status,
    401,
    "a different address cannot ride a code issued for another",
  );
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code: "111-111-111" })).status, 401);
  assert.equal(kv.json("emailchange:acct_1").attempts, 2);
});

test("redeem attempts cap durably at five", async (t) => {
  const harness = makeEnv();
  const { env } = harness;
  await issueChangeCode(t, harness);
  mockCell(t, commitHandlers());
  for (let i = 0; i < 5; i += 1) {
    assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code: "000-000-000" })).status, 401);
  }
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code: "000-000-000" })).status, 429);
});

test("an expired confirmation code is dead", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueChangeCode(t, harness);
  const state = kv.json("emailchange:acct_1");
  state.code_expires_at = new Date(Date.now() - 1000).toISOString();
  kv.map.set("emailchange:acct_1", JSON.stringify(state));
  mockCell(t, commitHandlers());
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code })).status, 401);
});

test("a committed change spends the code, kills recovery codes, and opens the undo window", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const code = await issueChangeCode(t, harness);
  kv.map.set("recover:acct_1", JSON.stringify({ code_hash: "ab".repeat(32) }));
  const calls = mockCell(t, commitHandlers());
  const resp = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(resp.status, 200);
  assert.equal((await resp.json()).email, NEW_EMAIL);

  const commits = calls.filter((c) => c.path.endsWith(":update-email"));
  assert.equal(commits.length, 1);
  const commitBody = JSON.parse(commits[0].init.body);
  assert.deepEqual(commitBody, { operator_id: "op_1", new_email: NEW_EMAIL });

  assert.equal(kv.json("emailchange:acct_1"), null, "the code is spent");
  assert.equal(kv.json("recover:acct_1"), null, "stale recovery codes die with the old anchor");

  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1);
  assert.equal(kv.ttls.get(undoKeys[0]), 48 * 3600);
  const undoState = kv.json(undoKeys[0]);
  assert.equal(undoState.old_email, OWNER_EMAIL);
  assert.equal(undoState.new_email, NEW_EMAIL);

  const undoMail = email.sent.at(-1);
  assert.equal(undoMail.to, OWNER_EMAIL);
  assert.match(undoMail.text, /revert/);

  const events = calls.filter((c) => c.path.endsWith(":events"));
  assert.ok(events.length >= 1, "the change lifecycle must be audited");
  for (const event of events) {
    const eventBody = String(event.init.body);
    assert.ok(!eventBody.includes(OWNER_EMAIL), "audit metadata must mask the old address");
    assert.ok(!eventBody.includes(NEW_EMAIL), "audit metadata must mask the new address");
    assert.ok(!eventBody.includes(code.replaceAll("-", "")), "audit metadata must never carry the code");
  }

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 401, "a lost-response replay cannot re-commit");
  assert.equal(calls.filter((c) => c.path.endsWith(":update-email")).length, 1);
});

test("a cell conflict or outage keeps the confirmation code alive", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueChangeCode(t, harness);
  mockCell(t, commitHandlers({ ":update-email": () => cellJSON({ error: "conflict" }, 409) }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code })).status, 409);
  assert.notEqual(kv.json("emailchange:acct_1").code_hash, undefined);

  mockCell(t, commitHandlers({ ":update-email": () => { throw new Error("cell down"); } }));
  assert.equal((await changeEmail(env, { new_email: NEW_EMAIL, code })).status, 502);
  assert.notEqual(kv.json("emailchange:acct_1").code_hash, undefined);
});

test("a failed undo notice does not unwind the committed change", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const code = await issueChangeCode(t, harness);
  email.failSendsTo.add(OWNER_EMAIL);
  mockCell(t, commitHandlers());
  const resp = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(resp.status, 200);
  assert.equal(kv.keysWithPrefix("undoemail:").length, 1, "the undo window still exists");
});

// ---- Undo window -----------------------------------------------------------

async function seedUndo(kv, accountId = "acct_1") {
  await kv.put(
    `undoemail:${await sha256hex(UNDO_TOKEN)}`,
    JSON.stringify({
      account_id: accountId,
      cell: "cell-a",
      old_email: OWNER_EMAIL,
      new_email: NEW_EMAIL,
      expires_at: new Date(Date.now() + 48 * 3600 * 1000).toISOString(),
    }),
    { expirationTtl: 48 * 3600 },
  );
}

test("undo links are method-bounded and unforgeable", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  const calls = mockCell(t, {});
  assert.equal((await run(env, `/undo-email/${UNDO_TOKEN}`, { method: "POST" })).status, 405);
  assert.equal((await run(env, `/undo-email/${"aa".repeat(32)}`)).status, 404);
  assert.deepEqual(calls, []);
  const malformed = await run(env, `/undo-email/${UNDO_TOKEN.slice(0, 10)}`);
  assert.equal(malformed.status, 599, "malformed tokens fall through to the cold path");
});

test("a valid undo re-points the account and burns the link", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedUndo(kv);
  kv.map.set("recover:acct_1", JSON.stringify({ code_hash: "cd".repeat(32) }));
  const calls = mockCell(t, { ":update-email": () => cellJSON({ email: OWNER_EMAIL }) });
  const resp = await run(env, `/undo-email/${UNDO_TOKEN}`);
  assert.equal(resp.status, 200);
  assert.match(await resp.text(), /reverted/);

  const commits = calls.filter((c) => c.path.endsWith(":update-email"));
  assert.equal(commits.length, 1);
  assert.deepEqual(JSON.parse(commits[0].init.body), {
    undo: true,
    expected_current: NEW_EMAIL,
    new_email: OWNER_EMAIL,
  });
  assert.equal(kv.json(`undoemail:${await sha256hex(UNDO_TOKEN)}`), null);
  assert.equal(kv.json("recover:acct_1"), null, "old-inbox recovery codes are revoked");

  const replay = await run(env, `/undo-email/${UNDO_TOKEN}`);
  assert.equal(replay.status, 404, "a spent undo link is inert");
  assert.equal(calls.filter((c) => c.path.endsWith(":update-email")).length, 1);
});

test("undo cannot affect a newer independent change", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedUndo(kv);
  mockCell(t, { ":update-email": () => cellJSON({ error: "email mismatch" }, 409) });
  const resp = await run(env, `/undo-email/${UNDO_TOKEN}`);
  assert.equal(resp.status, 409);
  const page = await resp.text();
  assert.match(page, /changed again/);
  assert.match(page, /recover/);
  assert.notEqual(kv.json(`undoemail:${await sha256hex(UNDO_TOKEN)}`), null);
});

test("an interrupted undo stays retryable until it lands", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedUndo(kv);
  t.mock.method(globalThis, "fetch", async () => {
    throw new Error("cell down");
  });
  assert.equal((await run(env, `/undo-email/${UNDO_TOKEN}`)).status, 503);
  assert.notEqual(kv.json(`undoemail:${await sha256hex(UNDO_TOKEN)}`), null);

  mockCell(t, { ":update-email": () => cellJSON({ email: OWNER_EMAIL }) });
  assert.equal((await run(env, `/undo-email/${UNDO_TOKEN}`)).status, 200);
  assert.equal(kv.json(`undoemail:${await sha256hex(UNDO_TOKEN)}`), null);
});

test("an interrupted undo burn leaves the revert committed and the replay honest about staleness", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedUndo(kv);
  let commits = 0;
  mockCell(t, {
    ":update-email": () => {
      commits += 1;
      // The first click commits the revert; after it, the account's current
      // email is the OLD address, so a replayed undo no longer matches
      // expected_current and the cell answers 409.
      return commits === 1
        ? cellJSON({ email: OWNER_EMAIL })
        : cellJSON({ error: "email mismatch" }, 409);
    },
  });
  kv.failDeletes.add(`undoemail:${await sha256hex(UNDO_TOKEN)}`);
  await assert.rejects(
    run(env, `/undo-email/${UNDO_TOKEN}`),
    /injected KV delete failure/,
    "the crash lands after the cell committed the revert",
  );
  assert.equal(commits, 1);

  // Pinned current behavior: the replay reports the stale-link page because
  // the revert it asked for has already landed; the page routes the user to
  // credential recovery either way.
  const replay = await run(env, `/undo-email/${UNDO_TOKEN}`);
  assert.equal(replay.status, 409);
  assert.match(await replay.text(), /recover/);
});

// ---- Email-change crash boundaries -----------------------------------------

// statefulOwnerCell models the authoritative cell across the commit: the
// account's email actually moves when :update-email lands, so replays see
// the post-commit world exactly as production would.
function statefulOwnerCell(t) {
  const cellState = { email: OWNER_EMAIL, commits: 0 };
  const calls = mockCell(t, {
    "/v1/account": () =>
      cellJSON({ account: { id: "acct_1", status: "active", email: cellState.email } }),
    "/v1/whoami": () => cellJSON({ principal: { operator_id: "op_1" } }),
    "/v1/operators": () => cellJSON({ operators: [{ id: "op_1", is_root: true }] }),
    ":update-email": (url, init) => {
      const body = JSON.parse(init.body);
      if (body.undo) {
        if (cellState.email !== body.expected_current) {
          return cellJSON({ error: "the email has changed since this undo link was issued" }, 409);
        }
        cellState.email = body.new_email;
        return cellJSON({ email: body.new_email });
      }
      cellState.commits += 1;
      cellState.email = body.new_email;
      return cellJSON({ email: body.new_email });
    },
  });
  return { cellState, calls };
}

async function issueChangeCodeStateful(t, harness) {
  const { env, kv, email } = harness;
  seedAcct(kv);
  seedCell(kv);
  const cell = statefulOwnerCell(t);
  const resp = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(resp.status, 200);
  return { code: extractCode(email.sent[0]), ...cell };
}

function undoNotices(email) {
  return email.sent.filter((m) => m.subject.includes("was changed"));
}

test("the arming snapshot records the committed transition's old address", async (t) => {
  const harness = makeEnv();
  await issueChangeCodeStateful(t, harness);
  const state = harness.kv.json("emailchange:acct_1");
  assert.equal(state.old_email, OWNER_EMAIL);
  assert.equal(state.new_email, NEW_EMAIL);
});

test("a committed change whose response was lost replays into the original undo channel", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  kv.failDeletes.add("recover:acct_1");
  kv.map.set("recover:acct_1", JSON.stringify({ code_hash: "cd".repeat(32) }));
  await assert.rejects(
    changeEmail(env, { new_email: NEW_EMAIL, code }),
    /injected KV delete failure/,
    "the crash lands after the cell committed",
  );
  assert.equal(cellState.commits, 1);
  assert.equal(cellState.email, NEW_EMAIL, "the transition is durable on the cell");
  assert.equal(kv.keysWithPrefix("undoemail:").length, 0, "no undo authority exists yet");

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).email, NEW_EMAIL);
  assert.equal(cellState.commits, 1, "the replay never re-mutates the cell");

  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1, "exactly one undo authority");
  const undo = kv.json(undoKeys[0]);
  assert.equal(undo.old_email, OWNER_EMAIL, "the ORIGINAL old address is recovered");
  assert.equal(undo.new_email, NEW_EMAIL);
  const notices = undoNotices(email);
  assert.equal(notices.length, 1);
  assert.equal(notices[0].to, OWNER_EMAIL, "the undo notice goes only to the original old inbox");
  assert.equal(kv.json("recover:acct_1"), null, "stale recovery codes still die");
  assert.equal(kv.json("emailchange:acct_1"), null, "the operation is terminal");
});

test("an interrupted attempt-count write fails closed before any cell mutation", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  kv.failPutMatching("emailchange:");
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }), /injected KV put failure/);
  assert.equal(cellState.commits, 0, "the crash precedes the cell mutation");
  assert.equal(kv.json("emailchange:acct_1").attempts, 0, "no durable attempt was recorded");

  const retry = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(retry.status, 200);
  assert.equal(cellState.commits, 1);
});

test("a crash before the undo-authority write recovers to exactly one valid token", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  // skip=2 passes the attempt-count and attempts-reset writes and fails the
  // undo fence write.
  kv.failPutMatching("emailchange:", 2);
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }), /injected KV put failure/);
  assert.equal(cellState.commits, 1, "the cell already committed");
  assert.equal(kv.keysWithPrefix("undoemail:").length, 0, "no authority record landed");
  assert.equal(undoNotices(email).length, 0, "no notice left before the crash");

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 200);
  assert.equal(cellState.commits, 1);
  assert.equal(kv.keysWithPrefix("undoemail:").length, 1, "exactly one undo authority");
  assert.equal(undoNotices(email).length, 1);
  assert.equal(undoNotices(email)[0].to, OWNER_EMAIL);
});

test("a crash after the authority fence but before the record still converges to one token", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  kv.failPutMatching("undoemail:");
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }), /injected KV put failure/);
  assert.equal(cellState.commits, 1);
  const fenced = kv.json("emailchange:acct_1");
  assert.match(fenced.undo_key, /^[0-9a-f]{64}$/, "the fence is durable before the record");
  assert.equal(kv.keysWithPrefix("undoemail:").length, 0);

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 200);
  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1, "exactly one live undo authority");
  assert.equal(kv.json(undoKeys[0]).old_email, OWNER_EMAIL);
});

test("a notice-sent crash replays without leaving two valid undo tokens", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  kv.failDeletes.add("emailchange:acct_1");
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }), /injected KV delete failure/);
  assert.equal(cellState.commits, 1);
  assert.equal(kv.keysWithPrefix("undoemail:").length, 1);
  const firstKey = kv.keysWithPrefix("undoemail:")[0];
  assert.equal(undoNotices(email).length, 1, "the first notice already left");

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 200);
  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1, "the crashed attempt's token was garbage-collected");
  assert.notEqual(undoKeys[0], firstKey, "the delivered replacement is the one live token");
  assert.equal(kv.json(undoKeys[0]).old_email, OWNER_EMAIL);
  const notices = undoNotices(email);
  assert.equal(notices.length, 2, "the replacement token was re-delivered");
  assert.ok(notices.every((n) => n.to === OWNER_EMAIL));
  assert.equal(cellState.commits, 1);
});

test("the undo revert burns the pending change so a stale code cannot re-commit", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  // Crash after the notice left but before the terminal delete: the code
  // stays armed while a live undo link is in the old inbox.
  kv.failDeletes.add("emailchange:acct_1");
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }));
  assert.equal(cellState.email, NEW_EMAIL);
  const undoToken = undoNotices(email).at(-1).text.match(/undo-email\/([0-9a-f]{64})/)[1];

  // The victim reverts through the undo link.
  const revert = await run(env, `/undo-email/${undoToken}`);
  assert.equal(revert.status, 200);
  assert.equal(cellState.email, OWNER_EMAIL, "the revert landed on the cell");
  assert.equal(kv.json("emailchange:acct_1"), null, "the revert burns the armed operation");

  // The stolen still-valid code must not pass the old-address guard again
  // and silently re-commit the change the owner just reverted.
  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 401);
  assert.equal(cellState.email, OWNER_EMAIL, "the reverted address survives the replay");
  assert.equal(cellState.commits, 1, "no second forward commit");
});

test("post-commit crash replays never exhaust the attempt budget", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  for (let round = 0; round < 6; round += 1) {
    kv.failDeletes.add("recover:acct_1");
    kv.map.set("recover:acct_1", JSON.stringify({ code_hash: "cd".repeat(32) }));
    await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }));
    assert.ok(
      (kv.json("emailchange:acct_1").attempts ?? 0) <= 1,
      "a correct code resets the durable attempt budget every round",
    );
  }
  const healed = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(healed.status, 200, "the undo channel is still recoverable after many crashes");
  assert.equal(cellState.commits, 1);
  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1);
  assert.equal(kv.json(undoKeys[0]).old_email, OWNER_EMAIL);
});

test("a concurrent duplicate redeem converges to a single undo authority", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  seedAcct(kv);
  seedCell(kv);
  const cellState = { email: OWNER_EMAIL, commits: 0 };
  let nested = null;
  let redeemBody = null;
  mockCell(t, {
    "/v1/account": () =>
      cellJSON({ account: { id: "acct_1", status: "active", email: cellState.email } }),
    "/v1/whoami": () => cellJSON({ principal: { operator_id: "op_1" } }),
    "/v1/operators": () => cellJSON({ operators: [{ id: "op_1", is_root: true }] }),
    ":update-email": async (url, init) => {
      const body = JSON.parse(init.body);
      cellState.commits += 1;
      cellState.email = body.new_email;
      if (redeemBody && !nested) {
        // A second redeem of the same operation runs to completion while
        // the first is still inside its cell commit.
        nested = worker.fetch(
          new Request(`${ORIGIN}/v1/accounts/acct_1:change-email`, {
            method: "POST",
            headers: { Authorization: "Bearer operator-token" },
            body: redeemBody,
          }),
          env,
          { waitUntil: () => {} },
        );
        await nested;
      }
      return cellJSON({ email: body.new_email });
    },
  });
  const request = await changeEmail(env, { new_email: NEW_EMAIL });
  assert.equal(request.status, 200);
  const code = extractCode(email.sent[0]);
  redeemBody = JSON.stringify({ new_email: NEW_EMAIL, code });
  const outer = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(outer.status, 200);
  assert.equal((await nested).status, 200);
  assert.equal(
    kv.keysWithPrefix("undoemail:").length,
    1,
    "the racing redeems converge to one live undo authority",
  );
  assert.equal(undoNotices(email).length, 1, "only one undo notice leaves");
  assert.equal(kv.json("emailchange:acct_1"), null, "spent state is not resurrected");
});

test("a recovered older operation cannot clobber or revert a newer independent change", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const { code, cellState } = await issueChangeCodeStateful(t, harness);
  kv.failDeletes.add("recover:acct_1");
  kv.map.set("recover:acct_1", JSON.stringify({ code_hash: "cd".repeat(32) }));
  await assert.rejects(changeEmail(env, { new_email: NEW_EMAIL, code }));
  cellState.email = "independent@example.test"; // a newer independent change lands

  const replay = await changeEmail(env, { new_email: NEW_EMAIL, code });
  assert.equal(replay.status, 409);
  assert.match((await replay.json()).error, /changed while this request was pending/);
  assert.equal(cellState.commits, 1, "the stale operation never touches the cell again");
  assert.equal(cellState.email, "independent@example.test", "the newer change survives");
  assert.equal(kv.keysWithPrefix("undoemail:").length, 0, "no undo authority for the stale operation");
});

test("a same-address redeem never mints a degenerate undo record", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  seedAcct(kv);
  seedCell(kv);
  const { cellState } = statefulOwnerCell(t);
  const request = await changeEmail(env, { new_email: OWNER_EMAIL });
  assert.equal(request.status, 200);
  const code = extractCode(email.sent[0]);
  const redeem = await changeEmail(env, { new_email: OWNER_EMAIL, code });
  assert.equal(redeem.status, 200);
  assert.equal(cellState.commits, 1);
  assert.equal(kv.keysWithPrefix("undoemail:").length, 0, "old == new must never become an undo record");
  assert.equal(kv.json("emailchange:acct_1"), null);
});

test("a pre-snapshot legacy state completes with the live-read fallback", async (t) => {
  const { env, kv } = makeEnv();
  seedAcct(kv);
  seedCell(kv);
  const { cellState } = statefulOwnerCell(t);
  kv.map.set(
    "emailchange:acct_1",
    JSON.stringify({
      code_hash: await sha256hex("123456789"),
      code_expires_at: new Date(Date.now() + 10 * 60 * 1000).toISOString(),
      new_email: NEW_EMAIL,
      attempts: 0,
      emails_sent: 1,
    }),
  );
  const resp = await changeEmail(env, { new_email: NEW_EMAIL, code: "123-456-789" });
  assert.equal(resp.status, 200);
  assert.equal(cellState.commits, 1);
  const undoKeys = kv.keysWithPrefix("undoemail:");
  assert.equal(undoKeys.length, 1);
  assert.equal(kv.json(undoKeys[0]).old_email, OWNER_EMAIL);
});

// ---- Value-free logging -----------------------------------------------------

test("malicious provider errors never reach recovery or change-email logs or responses", async (t) => {
  const logs = [];
  t.mock.method(console, "log", (line) => logs.push(String(line)));

  // Recovery request mode: provider throws mid-send.
  const recoverHarness = makeEnv();
  seedAcct(recoverHarness.kv);
  seedCell(recoverHarness.kv);
  mockCell(t, activeContactHandlers());
  recoverHarness.email.failError = MALICIOUS_PROVIDER_ERROR;
  recoverHarness.email.failNextSends = 1;
  const recoverResp = await recover(recoverHarness.env);
  assert.equal(recoverResp.status, 200);
  const recoverBody = await recoverResp.text();

  // Change-email confirmation send, alarm send, and undo notice send.
  const changeHarness = makeEnv();
  seedAcct(changeHarness.kv);
  seedCell(changeHarness.kv);
  statefulOwnerCell(t);
  changeHarness.email.failError = MALICIOUS_PROVIDER_ERROR;
  changeHarness.email.failSendsTo.add(NEW_EMAIL);
  const codeFail = await changeEmail(changeHarness.env, { new_email: NEW_EMAIL });
  assert.equal(codeFail.status, 502);
  changeHarness.email.failSendsTo.clear();
  changeHarness.email.failSendsTo.add(OWNER_EMAIL);
  const alarmFail = await changeEmail(changeHarness.env, { new_email: NEW_EMAIL });
  assert.equal(alarmFail.status, 502);
  changeHarness.email.failSendsTo.clear();
  // The alarm-fail attempt durably spent quota and the cooldown; age the
  // cooldown so the follow-up request is admitted.
  const cooled = changeHarness.kv.json("emailchange:acct_1");
  cooled.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  changeHarness.kv.map.set("emailchange:acct_1", JSON.stringify(cooled));
  const okReq = await changeEmail(changeHarness.env, { new_email: NEW_EMAIL });
  assert.equal(okReq.status, 200);
  // The armed code is the admitted request's — the alarm-fail attempt's
  // earlier code was deliberately never armed.
  const changeCode = extractCode(changeHarness.email.sent.findLast((m) => m.to === NEW_EMAIL));
  changeHarness.email.failSendsTo.add(OWNER_EMAIL); // undo notice fails
  const commit = await changeEmail(changeHarness.env, { new_email: NEW_EMAIL, code: changeCode });
  assert.equal(commit.status, 200);

  const responses = [recoverBody, await codeFail.text(), await alarmFail.text()];
  for (const marker of ["victim-leak@example.test", "ab".repeat(32), "MALICIOUS-MARKER", "X-INJECTED"]) {
    for (const line of logs) {
      assert.ok(!line.includes(marker), `log leaked ${marker}: ${line}`);
    }
    for (const body of responses) {
      assert.ok(!body.includes(marker), `response leaked ${marker}`);
    }
  }
  for (const line of logs) {
    assert.ok(!line.includes("\n"), "logs must stay single-line against newline injection");
  }
  // Pin that every injected failure path actually fired: the aggregate
  // reason-code check alone cannot tell a skipped injection from a clean one.
  assert.ok(
    logs.some((l) => l.startsWith("recover: recovery email send failed")),
    "the recovery send failure fired and logged its bounded line",
  );
  assert.ok(
    logs.some((l) => l.includes("phase=confirmation_send")),
    "the confirmation send failure fired",
  );
  assert.ok(
    logs.some((l) => l.includes("phase=alarm_send")),
    "the alarm send failure fired",
  );
  assert.ok(
    logs.some((l) => l.includes("phase=undo_notice_send")),
    "the undo notice failure fired",
  );
});

// ---- Recovery window is absolute --------------------------------------------

test("the recovery quota window is absolute and wrong attempts cannot extend it", async (t) => {
  const harness = makeEnv();
  const { env, kv } = harness;
  const code = await issueRecoveryCode(t, harness);
  const state = kv.json("recover:acct_1");
  assert.ok(state.window_expires_at, "the reservation stamps the absolute window");
  assert.equal(kv.ttls.get("recover:acct_1"), 4 * 3600);

  // Age the window to its final 10 minutes; a wrong attempt must derive a
  // bounded remaining TTL, never reset the clock.
  state.window_expires_at = new Date(Date.now() + 600 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(state));
  assert.equal((await recover(env, "acct_1", { code: "000-000-000" })).status, 401);
  const nearEnd = kv.ttls.get("recover:acct_1");
  assert.ok(nearEnd <= 600 && nearEnd >= 60, `ttl ${nearEnd} must stay inside the window`);

  // At the window's floor the KV minimum applies, still no extension beyond it.
  const floored = kv.json("recover:acct_1");
  floored.window_expires_at = new Date(Date.now() + 30 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(floored));
  assert.equal((await recover(env, "acct_1", { code: "000-000-000" })).status, 401);
  assert.equal(kv.ttls.get("recover:acct_1"), 60);
  assert.equal(kv.json("recover:acct_1").attempts, 2, "attempts still count durably");

  // The correct code still redeems inside the window and spends the state.
  mockCell(t, { ":recover": () => cellJSON(rotatedAccount()) });
  assert.equal((await recover(env, "acct_1", { code })).status, 200);
  assert.equal(kv.json("recover:acct_1"), null);
});

test("a superseding admitted request re-stamps the window; an outage never rewrites it", async (t) => {
  const harness = makeEnv();
  const { env, kv, email } = harness;
  await issueRecoveryCode(t, harness);
  const first = kv.json("recover:acct_1");
  // Age both the cooldown and the window, then admit a second send.
  first.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  first.window_expires_at = new Date(Date.now() + 600 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(first));
  assert.equal((await recover(env)).status, 200);
  const second = kv.json("recover:acct_1");
  assert.ok(
    Date.parse(second.window_expires_at) > Date.now() + 3 * 3600 * 1000,
    "an admitted send slides the window forward, matching the reservation's own TTL refresh",
  );

  // A provider outage burns the reserved slot without touching the window
  // or code state again: the reservation is the only write of that request.
  second.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(second));
  const sentBefore = kv.json("recover:acct_1").emails_sent;
  email.failNextSends = 1;
  assert.equal((await recover(env)).status, 200);
  const afterOutage = kv.json("recover:acct_1");
  assert.equal(afterOutage.emails_sent, sentBefore + 1, "the slot stays spent on a throw");
  assert.equal(afterOutage.code_hash, second.code_hash, "the live code survives the failed supersede");
});

// ---- Missing EMAIL binding --------------------------------------------------

test("an unconfigured email backend burns no quota and stays indistinguishable", async (t) => {
  const { env, kv } = makeEnv({ EMAIL: undefined });
  seedAcct(kv, "acct_real");
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  const real = await recover(env, "acct_real");
  const phantom = await recover(env, "acct_ghost");
  assert.equal(real.status, 200);
  assert.equal(await real.text(), await phantom.text(), "real and phantom stay byte-identical");

  for (const id of ["acct_real", "acct_ghost"]) {
    const state = kv.json(`recover:${id}`);
    assert.equal(state.emails_sent, 0, `${id}: no email attempted means no quota burned`);
    assert.ok(state.last_email_at, `${id}: pacing survives so retry rhythm stays uniform`);
    assert.equal(kv.json(`recover-ip:${id}:198.51.100.7`).emails_sent, 0, `${id}: per-ip refunded`);
  }

  // Concurrency/abuse bounds hold: a capped id stays capped even without a
  // backend, and the refund path never runs for refused requests.
  kv.map.set("recover:acct_capped", JSON.stringify({ emails_sent: 10 }));
  const capped = await recover(env, "acct_capped");
  assert.equal(capped.status, 429);
  assert.equal(kv.json("recover:acct_capped").emails_sent, 10);
});

test("recovery after backend configuration has the intended quota", async (t) => {
  const harness = makeEnv({ EMAIL: undefined });
  const { env, kv } = harness;
  seedAcct(kv);
  seedCell(kv);
  mockCell(t, activeContactHandlers());
  assert.equal((await recover(env)).status, 200);
  assert.equal(kv.json("recover:acct_1").emails_sent, 0);

  // The backend comes online; after the cooldown the owner has full quota
  // and the send goes out. (Absent binding refunds uniformly; a provider
  // THROW deliberately burns uniformly instead — the shared policy is that
  // quota treatment never depends on whether the account exists.)
  const email = new EmailFake();
  env.EMAIL = email;
  const state = kv.json("recover:acct_1");
  state.last_email_at = new Date(Date.now() - 10 * 60 * 1000).toISOString();
  kv.map.set("recover:acct_1", JSON.stringify(state));
  assert.equal((await recover(env)).status, 200);
  assert.equal(email.sent.length, 1);
  assert.equal(kv.json("recover:acct_1").emails_sent, 1);
  assert.match(kv.json("recover:acct_1").code_hash, /^[0-9a-f]{64}$/);
});

test("undo failure pages never leak the token or either address", async (t) => {
  const { env, kv } = makeEnv();
  seedCell(kv);
  await seedUndo(kv);
  mockCell(t, { ":update-email": () => cellJSON({ error: "email mismatch" }, 409) });
  const stale = await run(env, `/undo-email/${UNDO_TOKEN}`);
  const stalePage = await stale.text();
  const unknown = await run(env, `/undo-email/${"bb".repeat(32)}`);
  const unknownPage = await unknown.text();
  for (const page of [stalePage, unknownPage]) {
    assert.ok(!page.includes(UNDO_TOKEN));
    assert.ok(!page.includes(OWNER_EMAIL));
    assert.ok(!page.includes(NEW_EMAIL));
  }
  assert.deepEqual(containerCalls, []);
});
