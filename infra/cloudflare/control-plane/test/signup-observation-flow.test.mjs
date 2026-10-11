// Public Worker boundaries backed by the real signup Durable Object, including
// its observation singleton and quota authorities. No network or live accounts.
import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";
import {
  SIGNUP_OBSERVATION_KEY,
  SIGNUP_OBSERVATION_OBJECT,
} from "../src/signup-observation.mjs";
import { signupIPScope, utcDayBucket } from "../src/signup-counters.mjs";
import { UPTIME_PROBE_RUN_KEY } from "../src/uptime-probes.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const { AccountSignup, default: worker } = await import("../src/index.js");
const { containerCalls, resetContainerCalls } = await import("./fixtures/cloudflare-containers-stub.mjs");

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value: (a, b) => nodeTimingSafeEqual(Buffer.from(a), Buffer.from(b)),
  });
}

const ORIGIN = "https://cp.test.invalid";
const CELL_ENDPOINT = "https://cell-a.test.invalid";
const SOURCE_IP = "203.0.113.42";
const EMAIL = "private-observation@example.test";
const TOKEN = "ab".repeat(32);
const KEYS = [
  "started", "provisioned", "provision_failed", "verified",
  "quota_per_ip", "quota_global", "turnstile_invalid", "turnstile_unavailable",
  "email_signup_sent", "email_signup_failed", "email_resend_sent", "email_resend_failed",
];
const COUNTER_SAMPLES = {
  started: 'witself_signup_events_total{event="started"}',
  provisioned: 'witself_signup_events_total{event="provisioned"}',
  provision_failed: 'witself_signup_events_total{event="provision_failed"}',
  verified: 'witself_signup_events_total{event="verified"}',
  quota_per_ip: 'witself_signup_denials_total{reason="quota_per_ip"}',
  quota_global: 'witself_signup_denials_total{reason="quota_global"}',
  turnstile_invalid: 'witself_signup_denials_total{reason="turnstile_invalid"}',
  turnstile_unavailable: 'witself_signup_denials_total{reason="turnstile_unavailable"}',
  email_signup_sent: 'witself_signup_verification_email_total{context="signup",outcome="sent"}',
  email_signup_failed: 'witself_signup_verification_email_total{context="signup",outcome="failed"}',
  email_resend_sent: 'witself_signup_verification_email_total{context="resend",outcome="sent"}',
  email_resend_failed: 'witself_signup_verification_email_total{context="resend",outcome="failed"}',
};
const allCounts = (patch = {}) => Object.fromEntries(KEYS.map((key) => [key, patch[key] ?? 0]));
const nativeSetTimeout = globalThis.setTimeout;

class Storage {
  constructor() { this.values = new Map(); }
  async get(key) { return structuredClone(this.values.get(key)); }
  async put(key, value) { this.values.set(key, structuredClone(value)); }
  async delete(key) { this.values.delete(key); }
  async list({ prefix = "" } = {}) {
    return new Map([...this.values].filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, structuredClone(value)]));
  }
  async transaction(callback) {
    const staged = new Storage();
    staged.values = structuredClone(this.values);
    const result = await callback(staged);
    this.values = staged.values;
    return result;
  }
}

class Directory {
  constructor() {
    this.values = new Map();
    this.pendingPuts = 0;
    this.failSecondPendingPut = false;
  }
  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }
  async put(key, value) {
    if (key.startsWith("pending:")) {
      this.pendingPuts++;
      if (this.failSecondPendingPut && this.pendingPuts === 2) {
        throw new Error(`provider-private ${EMAIL} ${SOURCE_IP}`);
      }
    }
    this.values.set(key, value);
  }
  async delete(key) { this.values.delete(key); }
  async list({ prefix = "" } = {}) {
    return {
      keys: [...this.values.keys()].filter((key) => key.startsWith(prefix)).map((name) => ({ name })),
      list_complete: true,
    };
  }
  seed(key, value) { this.values.set(key, JSON.stringify(value)); }
}

class SignupNamespace {
  constructor(env, mode) {
    this.env = env;
    this.mode = mode;
    this.objects = new Map();
    this.storages = new Map();
    this.records = 0;
  }
  idFromName(name) { return { name }; }
  storage(name) {
    if (!this.storages.has(name)) this.storages.set(name, new Storage());
    return this.storages.get(name);
  }
  get(id) {
    return { fetch: async (request) => {
      if (id.name === SIGNUP_OBSERVATION_OBJECT && new URL(request.url).pathname === "/observation/record") {
        this.records++;
        if (this.mode === "noop") return Response.json({ ok: true });
        if (this.mode === "throw") throw new Error("observation unavailable");
        if (this.mode === "hang") return new Promise(() => {});
      }
      if (!this.objects.has(id.name)) {
        this.objects.set(id.name, new AccountSignup({ id, storage: this.storage(id.name) }, this.env));
      }
      return this.objects.get(id.name).fetch(request);
    } };
  }
  async counts() {
    const response = await this.get(this.idFromName(SIGNUP_OBSERVATION_OBJECT)).fetch(
      new Request("https://account-signup.internal/observation/snapshot"),
    );
    assert.equal(response.status, 200);
    return (await response.json()).counts;
  }
}

function fixture(t, mode = "real") {
  const directory = new Directory();
  const logs = [];
  t.mock.method(console, "log", (...args) => logs.push(args.join(" ")));
  const env = {
    DIRECTORY: directory,
    CP_SIGNUP_OPEN: "true",
    CP_SIGNUP_TURNSTILE_ENABLED: "true",
    CP_SIGNUP_TURNSTILE_SECRET_KEY: "synthetic-test-key",
    CP_SIGNUP_DAILY_LIMIT_PER_IP: "10",
    CP_SIGNUP_DAILY_LIMIT_GLOBAL: "500",
    EMAIL: { async send() { if (f.emailFails) throw new Error(`provider-private ${EMAIL}`); } },
    CELL_COORDINATOR: {
      idFromName: (name) => ({ name }),
      get: () => ({ fetch: async (request) => {
        const body = await request.json();
        return Response.json({ ok: true, ...body, attached: true, resident: true });
      } }),
    },
  };
  const namespace = new SignupNamespace(env, mode);
  env.ACCOUNT_SIGNUP = namespace;
  const f = {
    env, namespace, directory, logs,
    turnstile: "valid", cellFails: false, emailFails: false, activation: true,
    cellReceipts: new Set(), responses: [],
  };
  f.seedCell = () => directory.seed("cell:cell-a", {
    endpoint: CELL_ENDPOINT, provision_token: "synthetic-cell-key", registration_id: "registration-a",
  });
  f.seedCell();
  directory.seed(UPTIME_PROBE_RUN_KEY, {
    scheduler: { last_run_at: new Date().toISOString(), duration_ms: 1, target_count: 0, directory_ok: true },
    targets: [],
  });
  t.mock.method(globalThis, "fetch", async (input, init = {}) => {
    const url = new URL(typeof input === "string" ? input : input.url);
    if (url.hostname === "challenges.cloudflare.com") {
      if (f.turnstile === "unavailable") throw new Error("siteverify unavailable");
      return Response.json(f.turnstile === "invalid"
        ? { success: false, "error-codes": ["invalid-input-response"] } : { success: true });
    }
    assert.equal(url.origin, CELL_ENDPOINT, "all cell traffic stays in the offline fake");
    if (url.pathname === "/v1/version") return Response.json({ account_provision_protocol: 1 });
    if (url.pathname === "/v1/accounts:provision-exact") {
      if (f.cellFails) return Response.json({ error: "injected cell failure" }, { status: 503 });
      const body = JSON.parse(init.body);
      const replayed = f.cellReceipts.has(body.provision_id);
      f.cellReceipts.add(body.provision_id);
      return Response.json({
        schema_version: "witself.v0", provision_id: body.provision_id, replayed,
        recorded_consent_terms_version: body.consent_terms_version,
        recorded_consent_privacy_version: body.consent_privacy_version,
        account: {
          account_id: "acct_observation", operator_id: "operator-observation", email: body.email,
          status: "pending", bootstrap_token: "deterministic-bootstrap-token",
        },
      }, { status: 201 });
    }
    if (url.pathname.endsWith(":events")) return Response.json({ ok: true });
    if (url.pathname.endsWith(":activate")) return Response.json({
      status: "active", account_id: "acct_observation",
      ...(f.activation === undefined ? {} : { activated: f.activation }),
    });
    if (url.pathname === "/v1/account") return Response.json({
      account: { id: "acct_observation", status: "pending", email: EMAIL },
    });
    assert.fail("unexpected offline cell route");
  });
  return f;
}

async function responseWithinBudget(promise, milliseconds) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = nativeSetTimeout(() => reject(new Error("recorder outage exceeded response deadline")), milliseconds);
      }),
    ]);
  } finally { clearTimeout(timer); }
}

async function request(f, path, expectedStatus, init = {}) {
  resetContainerCalls();
  const response = await responseWithinBudget(worker.fetch(
    new Request(`${ORIGIN}${path}`, init), f.env, { waitUntil() {} },
  ), f.namespace.mode === "hang" ? 2000 : 10_000);
  assert.equal(response.status, expectedStatus, "public response status");
  assert.deepEqual(containerCalls, [], "signup observations never wake the Go container");
  const snapshot = { status: response.status, headers: [...response.headers], body: await response.text() };
  f.responses.push(snapshot);
  return snapshot;
}

function signup(f, status = 201) {
  return request(f, "/v1/accounts", status, {
    method: "POST",
    headers: { "Content-Type": "application/json", "CF-Connecting-IP": SOURCE_IP },
    body: JSON.stringify({
      provision_id: "prov-observation", email: EMAIL, display_name: "Observation test",
      turnstile_token: "synthetic-challenge", consent_terms_version: "v1", consent_privacy_version: "v1",
    }),
  });
}

async function seedQuota(f, scope, limit) {
  const day = utcDayBucket();
  const storage = f.namespace.storage(scope);
  await storage.put("active-day", day);
  await storage.put(`count:${day}`, { day, count: limit });
}

async function seedVerification(f) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(TOKEN));
  const key = [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
  f.directory.seed(`verify:${key}`, { account_id: "acct_observation", cell: "cell-a" });
}

function seedResend(f) {
  f.directory.seed("acct:acct_observation", { cell: "cell-a", endpoint: CELL_ENDPOINT });
  f.directory.seed("pending:acct_observation", {
    cell: "cell-a", created_at: new Date(Date.now() - 600_000).toISOString(),
    last_email_at: new Date(Date.now() - 600_000).toISOString(), emails_sent: 1,
  });
}

const cases = [
  {
    name: "open signup records start, completed provisioning and sent email",
    counts: { started: 1, provisioned: 1, email_signup_sent: 1 },
    async exercise(f) {
      const response = await signup(f);
      assert.equal(JSON.parse(response.body).verification_email_sent, true);
    },
  },
  {
    name: "per-IP quota denial counts once and preserves the 429 body",
    counts: { started: 1, quota_per_ip: 1 },
    async exercise(f) {
      await seedQuota(f, await signupIPScope(SOURCE_IP), 10);
      const response = await signup(f, 429);
      assert.equal(response.body, '{"schema_version":"witself.v0","error":"signup rate limit exceeded"}');
    },
  },
  {
    name: "global quota denial counts once and preserves the 429 body",
    counts: { started: 1, quota_global: 1 },
    async exercise(f) {
      await seedQuota(f, "signup-counter:global", 500);
      const response = await signup(f, 429);
      assert.equal(response.body, '{"schema_version":"witself.v0","error":"signup rate limit exceeded"}');
    },
  },
  {
    name: "invalid Turnstile is a 403 denial, not a provisioning failure",
    counts: { started: 1, turnstile_invalid: 1 },
    async exercise(f) { f.turnstile = "invalid"; await signup(f, 403); },
  },
  {
    name: "unavailable Turnstile is a 503 denial, not a provisioning failure",
    counts: { started: 1, turnstile_unavailable: 1 },
    async exercise(f) { f.turnstile = "unavailable"; await signup(f, 503); },
  },
  {
    name: "placement failure counts one failed attempt and its retry completes once",
    counts: { started: 1, provision_failed: 1, provisioned: 1, email_signup_sent: 1 },
    async exercise(f) {
      f.directory.values.delete("cell:cell-a");
      const failed = await signup(f, 503);
      assert.equal(JSON.parse(failed.body).error, "no capacity: no accepting cells");
      f.seedCell();
      await signup(f);
    },
  },
  {
    name: "a thrown provisioning failure is counted before successful retry",
    counts: { started: 1, provision_failed: 1, provisioned: 1, email_signup_sent: 1 },
    async exercise(f) {
      f.cellFails = true;
      await signup(f, 502);
      f.cellFails = false;
      await signup(f);
    },
  },
  {
    name: "missing EMAIL records a failed email but retains successful signup",
    counts: { started: 1, provisioned: 1, email_signup_failed: 1 },
    async exercise(f) {
      delete f.env.EMAIL;
      const response = await signup(f);
      assert.equal(JSON.parse(response.body).verification_email_sent, false);
    },
  },
  {
    name: "successful email followed by a pending-record failure is still sent and logs no provider detail",
    counts: { started: 1, provisioned: 1, email_signup_sent: 1 },
    async exercise(f) {
      f.directory.failSecondPendingPut = true;
      const response = await signup(f);
      assert.equal(JSON.parse(response.body).verification_email_sent, true);
      assert.equal(f.directory.pendingPuts, 2);
      assert.deepEqual(f.logs, [
        "signup: verification email step failed for acct_observation (reason=email_send_or_record_error)",
      ]);
      assert.ok(!f.logs.join("\n").includes(EMAIL));
      assert.ok(!f.logs.join("\n").includes(SOURCE_IP));
    },
  },
  {
    name: "completed replay adds nothing, including a failing replay",
    counts: { started: 1, provisioned: 1, email_signup_sent: 1 },
    async exercise(f) {
      await signup(f);
      const replay = await signup(f);
      assert.equal(JSON.parse(replay.body).replayed, true);
      f.cellFails = true;
      await signup(f, 502);
    },
  },
  {
    name: "verification counts only explicit activation, excluding replay and omitted boolean",
    counts: { verified: 1 },
    async exercise(f) {
      await seedVerification(f);
      await request(f, `/verify/${TOKEN}`, 200);
      f.activation = false;
      const replay = await request(f, `/verify/${TOKEN}`, 200);
      assert.match(replay.body, /Already verified/);
      f.activation = undefined;
      await request(f, `/verify/${TOKEN}`, 200);
    },
  },
  {
    name: "resend records sent and failed outcomes while preserving its 502 error",
    counts: { email_resend_sent: 1, email_resend_failed: 1 },
    async exercise(f) {
      seedResend(f);
      const init = { method: "POST", headers: { Authorization: "Bearer synthetic-operator-key" } };
      await request(f, "/v1/accounts/acct_observation:resend-verification", 200, init);
      seedResend(f);
      f.emailFails = true;
      const failed = await request(f, "/v1/accounts/acct_observation:resend-verification", 502, init);
      assert.equal(failed.body, '{"schema_version":"witself.v0","error":"could not send the verification email — try again shortly"}');
      assert.ok(!f.logs.join("\n").includes(EMAIL));
    },
  },
];

for (const scenario of cases) {
  test(scenario.name, async (t) => {
    const f = fixture(t);
    await scenario.exercise(f);
    assert.deepEqual(await f.namespace.counts(), allCounts(scenario.counts));
  });
}

for (const mode of ["throw", "hang"]) {
  test(`recorder ${mode} preserves every public status, header and response byte`, { timeout: 40_000 }, async (t) => {
    // Keep the production timeout path, but shorten only its 1000ms duration.
    // The independent native timer rejects M3 instead of leaving this test hung.
    t.mock.method(globalThis, "setTimeout", (callback, ms, ...args) =>
      nativeSetTimeout(callback, ms === 1000 ? 10 : ms, ...args));
    for (const scenario of cases) {
      await t.test(scenario.name, async (st) => {
        const baseline = fixture(st, "noop");
        await scenario.exercise(baseline);
        const outage = fixture(st, mode);
        await scenario.exercise(outage);
        assert.ok(outage.namespace.records > 0, "outage must actually hit the recorder");
        assert.deepEqual(outage.responses, baseline.responses);
      });
    }
  });
}

test("one maintenance tick exports singleton counts at the public scrape", async (t) => {
  const f = fixture(t);
  await signup(f);
  const waits = [];
  await worker.scheduled({ cron: "*/5 * * * *", scheduledTime: Date.now() }, f.env, {
    waitUntil: (promise) => waits.push(promise),
  });
  assert.equal(waits.length, 7);
  const results = await Promise.allSettled(waits);
  assert.ok(results.every((result) => result.status === "fulfilled"), "maintenance must settle without failure");
  const document = JSON.parse(f.directory.values.get(SIGNUP_OBSERVATION_KEY));
  assert.deepEqual(document.counts, allCounts({ started: 1, provisioned: 1, email_signup_sent: 1 }));
  const response = await request(f, "/metrics/probes", 200);
  assert.ok(response.body.includes("witself_signup_metrics_up 1\n"));
  const counterLines = response.body.split("\n").filter((line) => /^witself_signup_(events|denials|verification_email)_total\{/.test(line));
  assert.equal(counterLines.length, 12);
  for (const [key, value] of Object.entries(document.counts)) {
    assert.ok(counterLines.includes(`${COUNTER_SAMPLES[key]} ${value}`), `missing ${key} count`);
  }
  for (const privateValue of [EMAIL, SOURCE_IP, "acct_observation", "prov-observation", "signup-counter:ip:"]) {
    assert.ok(!response.body.includes(privateValue), "scrape contains a private marker");
    assert.ok(!JSON.stringify(document).includes(privateValue), "observation document contains a private marker");
  }
});
