import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

import { DurableAccountSignup } from "../src/account-signup-runtime.mjs";
import { validLegalState, validReconsent, validRegistration } from "../src/signup-legal.mjs";
import {
  SIGNUP_OBSERVATION_OBJECT, SIGNUP_OBSERVATION_KEY,
  recordSignupObservationCount, signupObservationSnapshot,
  recordSignupObservation, runScheduledSignupObservation, signupObservationLines,
} from "../src/signup-observation.mjs";
import { UPTIME_PROBE_RUN_KEY } from "../src/uptime-probes.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const { default: worker } = await import("../src/index.js");
const NOW = Date.parse("2026-10-10T12:00:00.000Z");
const KEYS = ["started", "provisioned", "provision_failed", "verified", "quota_per_ip", "quota_global",
  "turnstile_invalid", "turnstile_unavailable", "email_signup_sent", "email_signup_failed",
  "email_resend_sent", "email_resend_failed"];
const MARKERS = ["someone@example.invalid", "192.0.2.119", "acct_private_signup"];
const INJECTION = 'x"} 1\nevil{';
const flush = () => new Promise((resolve) => setImmediate(resolve));
const zeroCounts = () => Object.fromEntries(KEYS.map((key) => [key, 0]));
const snapshot = () => ({ schema_version: 1, counts: zeroCounts() });
const document = () => ({ ...snapshot(), observed_at: new Date(NOW).toISOString(),
  pending: { accounts: 3, oldest_created_at: "2026-10-10T11:30:00.000Z", complete: true } });
const probeRun = () => ({ scheduler: { last_run_at: new Date(NOW).toISOString(), duration_ms: 5,
  target_count: 0, directory_ok: true }, targets: [] });
class Storage {
  constructor() { this.values = new Map(); this.writes = []; }
  async get(key) { return structuredClone(this.values.get(key)); }
  async put(key, value) { this.writes.push(key); this.values.set(key, structuredClone(value)); }
  async transaction(fn) { return fn(this); }
}
class Directory {
  constructor(entries = []) {
    this.values = new Map([[UPTIME_PROBE_RUN_KEY, JSON.stringify(probeRun())], ...entries.map(([key, value]) => [key, JSON.stringify(value)])]);
    this.calls = []; this.writes = []; this.listComplete = true;
  }
  async get(key, options) {
    this.calls.push(["get", key]);
    const raw = this.values.get(key) ?? null;
    return options?.type === "json" ? JSON.parse(raw) : raw;
  }
  async list(options) {
    this.calls.push(["list", options]);
    return { keys: [...this.values.keys()].filter((key) => key.startsWith(options.prefix)).map((name) => ({ name })), list_complete: this.listComplete };
  }
  async put(key, raw) { this.calls.push(["put", key]); this.writes.push({ key, raw }); this.values.set(key, raw); }
  observation() { return JSON.parse(this.values.get(SIGNUP_OBSERVATION_KEY)); }
}
const namespace = (fetchImpl = async () => Response.json(snapshot())) => ({
  idFromName(name) { assert.equal(name, SIGNUP_OBSERVATION_OBJECT); return name; },
  get(name) { assert.equal(name, SIGNUP_OBSERVATION_OBJECT); return { fetch: fetchImpl }; },
});
const environment = (directory, fetchImpl) => ({ DIRECTORY: directory, ACCOUNT_SIGNUP: namespace(fetchImpl) });
const samples = (body) => body.split("\n").filter((line) => line.startsWith("witself_signup_"));
async function scrape(directory) {
  const response = await worker.fetch(new Request("https://example.invalid/metrics/probes"),
    new Proxy({ DIRECTORY: directory }, { get(target, key) {
      assert.ok(["DIRECTORY", "CP_PLAN_LIFECYCLE_ENABLED", "CP_ACCOUNT_BACKUPS_ENABLED"].includes(key));
      return target[key];
    } }), {});
  assert.equal(response.status, 200);
  return response.text();
}

test("transaction counts start at zero, increment exactly, and reject unknown or corrupt state", async () => {
  const storage = new Storage();
  assert.deepEqual(await signupObservationSnapshot(storage), zeroCounts());
  assert.deepEqual(storage.writes, []);
  for (const key of KEYS) await storage.transaction((tx) => recordSignupObservationCount(tx, key));
  await storage.transaction((tx) => recordSignupObservationCount(tx, "verified"));
  assert.deepEqual(await signupObservationSnapshot(storage), { ...Object.fromEntries(KEYS.map((key) => [key, 1])), verified: 2 });
  assert.deepEqual([...storage.values.keys()], ["observation:counts"]);
  await assert.rejects(recordSignupObservationCount(storage, INJECTION), /unknown signup observation key/);
  assert.equal(storage.writes.length, 13);
  storage.values.set("observation:counts", { ...zeroCounts(), [INJECTION]: 1 });
  await assert.rejects(signupObservationSnapshot(storage), /invalid signup observation counts/);
  await assert.rejects(recordSignupObservationCount(storage, "started"), /invalid signup observation counts/);
  storage.values.set("observation:counts", { ...zeroCounts(), started: Number.MAX_SAFE_INTEGER });
  await assert.rejects(recordSignupObservationCount(storage, "started"), /count exhausted/);
  assert.equal(storage.writes.length, 13);
});

test("singleton routes reject other object names, and legacy routes cannot write singleton state", async (t) => {
  for (const name of ["provision:p1", "invite:early-access", "signup-counter:global"]) {
    const storage = new Storage();
    const instance = new DurableAccountSignup({ storage, id: { name } }, {});
    for (const [method, path] of [["GET", "/observation/snapshot"], ["POST", "/observation/record"]]) {
      const response = await instance.fetch(new Request(`https://account-signup.internal${path}`, {
        method, ...(method === "POST" ? { body: JSON.stringify({ key: "started" }) } : {}),
      }));
      assert.equal(response.status, 404);
      assert.match(await response.text(), /account signup endpoint not found/);
    }
    assert.deepEqual(storage.writes, []);
  }
  const storage = new Storage();
  const recordObservation = t.mock.fn();
  const instance = new DurableAccountSignup({ storage, id: { name: SIGNUP_OBSERVATION_OBJECT } }, {}, { recordObservation });
  const refusal = { schema_version: "witself.signup-legal-refusal.v1", code: "signup_legal_stale",
    error: "signup legal acceptance is out of date", provision_id: "p1", request_fingerprint: "a".repeat(64),
    consent_terms_version: "v1", consent_privacy_version: "v1", refusal_id: "r1", refusal_revision: 1,
    required_terms_version: "v2", required_privacy_version: "v2" };
  const candidate = { provision_id: "p2", consent_terms_version: "v2", consent_privacy_version: "v2" };
  const abuse = { root_provision_id: "p1", signup_ip_scope: null, turnstile: false,
    per_ip_limit: 0, global_limit: 0, per_ip: null, global: null };
  const registration = { refusal, candidate, transition_id: "t1", request_fingerprint: "b".repeat(64),
    core_fingerprint: "c".repeat(64), open_signup: false, abuse, registered: false };
  const reconsent = { schema_version: "witself.signup-reconsent.v1", refusal, candidate, transition_id: "t1",
    email: "user@example.invalid", invite: "early-access", display_name: "" };
  const legalState = { schema_version: "witself.signup.v1", revision: 1, phase: "legal_rejected", provision_id: "p1",
    request_fingerprint: refusal.request_fingerprint, cell: null, account: null, created_at: new Date(NOW).toISOString(),
    email_attempted: false, verification_email_sent: false, legal_core_fingerprint: registration.core_fingerprint,
    legal_terms_version: "v1", legal_privacy_version: "v1", legal_open_signup: false, legal_abuse: abuse,
    legal_refusal: refusal, legal_manifest_sha256: "d".repeat(64) };
  assert.ok(validRegistration(registration), "reservation fixture must reach object identity validation");
  assert.ok(validReconsent(reconsent), "re-consent fixture must pass protocol validation");
  assert.ok(validLegalState(legalState), "re-consent fixture state must reach object identity validation");
  for (const [method, path, input, status, error] of [
    ["POST", "/run", { provision_id: "p1", email: "user@example.invalid", invite: "early-access" }, 400, "signup Durable Object identity mismatch"],
    ["POST", "/legal/reserve", registration, 400, "invalid signup successor reservation"],
    ["POST", "/invite/reserve", { invite: "early-access", provision_id: "p1", request_fingerprint: "a".repeat(64) }, 400, "invalid invite reservation"],
    ["POST", "/counter/consume", { provision_id: "p1", limit: 10 }, 400, "invalid signup counter consumption"],
    ["GET", "/invite/status", null, 400, "invalid invite status request"],
    ["POST", "/legal/reconsent", reconsent, 409, "signup refusal conflicts"],
  ]) {
    // This route checks the persisted refusal before its object-name guard.
    // Seed valid state directly so an unrelated validation cannot mask it.
    if (path === "/legal/reconsent") storage.values.set("account-signup", structuredClone(legalState));
    const response = await instance.fetch(new Request(`https://account-signup.internal${path}`, {
      method, ...(method === "POST" ? { body: JSON.stringify(input) } : {}),
    }));
    assert.equal(response.status, status, `${method} ${path} must refuse singleton`);
    assert.equal((await response.json()).error, error);
    assert.deepEqual(storage.writes, [], `${path} cannot write signup state`);
    assert.equal(recordObservation.mock.callCount(), 0, `${path} cannot record an observation`);
    storage.values.delete("account-signup");
  }
  for (const body of [{ key: "started", extra: true }, { key: INJECTION }, {}, [], null]) {
    const response = await instance.fetch(new Request("https://account-signup.internal/observation/record", {
      method: "POST", body: JSON.stringify(body),
    }));
    assert.equal(response.status, 400);
    assert.deepEqual(storage.writes, []);
  }
  const recorded = await instance.fetch(new Request("https://account-signup.internal/observation/record", {
    method: "POST", body: JSON.stringify({ key: "started" }),
  }));
  assert.deepEqual(await recorded.json(), { ok: true });
  const response = await instance.fetch(new Request("https://account-signup.internal/observation/snapshot"));
  assert.deepEqual(await response.json(), { schema_version: 1, counts: { ...zeroCounts(), started: 1 } });
});

test("run records nothing before malformed request and provision identity refusals", async (t) => {
  for (const [body, error] of [
    ["{", "invalid JSON body"],
    ["{}", "provision_id is required and must be a nonempty opaque identifier"],
    [JSON.stringify({ provision_id: "p2", email: "user@example.invalid", invite: "early-access" }),
      "signup Durable Object identity mismatch"],
  ]) {
    const storage = new Storage();
    const recordObservation = t.mock.fn();
    const instance = new DurableAccountSignup({ storage, id: { name: "provision:p1" } }, {}, { recordObservation });
    const response = await instance.fetch(new Request("https://account-signup.internal/run", { method: "POST", body }));
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, error);
    assert.equal(recordObservation.mock.callCount(), 0, "refused requests must not record started");
    assert.deepEqual(storage.writes, []);
  }
});

test("recorder is absent-safe, value-free, nonthrowing and rejects unknown keys without fetching", async () => {
  await assert.doesNotReject(recordSignupObservation({}, "started"));
  let calls = 0;
  const env = { ACCOUNT_SIGNUP: namespace(async (request) => {
    calls += 1;
    assert.equal(request.url, "https://account-signup.internal/observation/record");
    assert.equal(request.method, "POST");
    assert.deepEqual(await request.json(), { key: "started" });
    throw new Error(MARKERS.join(" "));
  }) };
  await assert.doesNotReject(recordSignupObservation(env, "started"));
  await assert.doesNotReject(recordSignupObservation(env, INJECTION));
  assert.equal(calls, 1);
  await assert.doesNotReject(recordSignupObservation(new Proxy({}, { get() { throw new Error("binding unavailable"); } }), "started"));
});

test("hanging recorder resolves within the injected deadline", { timeout: 2000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let completed = false;
  const pending = recordSignupObservation({ ACCOUNT_SIGNUP: namespace(() => new Promise(() => {})) }, "started", { timeoutMs: 50 })
    .then(() => { completed = true; });
  await flush(); t.mock.timers.tick(49); await flush();
  assert.equal(completed, false);
  t.mock.timers.tick(1); await flush();
  assert.equal(completed, true, "recorder must settle at timeoutMs even when fetch never settles");
  await pending;
});

test("tick writes fixed fields and canonical pending times without private source values", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new Directory([
    [`pending:${MARKERS[0]}`, { created_at: "2026-10-10T11:20:00Z", email: MARKERS[0] }],
    [`pending:${MARKERS[1]}`, { created_at: "2026-10-10T11:10:00-01:00", ip: MARKERS[1] }],
    [`pending:${MARKERS[2]}`, { created_at: "2026-10-10T10:00:00.000Z", account: MARKERS[2] }],
    ["pending:invalid", { created_at: "not a date" }], ["pending:gone", null],
  ]);
  const counts = { ...zeroCounts(), started: 4, email_signup_sent: 3 };
  const env = { ...environment(directory, async (request) => {
    assert.equal(request.method, "GET");
    assert.equal(new URL(request.url).pathname, "/observation/snapshot");
    return Response.json({ schema_version: 1, counts });
  }), PRIVATE_EMAIL: MARKERS[0], PRIVATE_IP: MARKERS[1], PRIVATE_ACCOUNT: MARKERS[2] };
  await runScheduledSignupObservation(env);
  assert.equal(directory.writes.length, 1);
  assert.equal(directory.writes[0].key, SIGNUP_OBSERVATION_KEY);
  assert.deepEqual(directory.observation(), { schema_version: 1, observed_at: new Date(NOW).toISOString(), counts,
    pending: { accounts: 4, oldest_created_at: "2026-10-10T10:00:00.000Z", complete: true } });
  assert.deepEqual(directory.calls[0], ["list", { prefix: "pending:", limit: 1000 }]);
  const body = await scrape(directory);
  for (const marker of MARKERS) {
    assert.ok(!directory.writes[0].raw.includes(marker));
    assert.ok(!body.includes(marker));
  }
});

test("tick preserves old totals on snapshot failure or invalid counts and avoids all KV without both bindings", async () => {
  for (const fetchImpl of [
    async () => { throw new Error("unavailable"); },
    async () => new Response("bad", { status: 500 }),
    async () => new Response("{"),
    async () => Response.json({ ...snapshot(), extra: true }),
    async () => Response.json({ ...snapshot(), counts: { ...zeroCounts(), started: -1 } }),
    async () => Response.json({ ...snapshot(), counts: { ...zeroCounts(), started: Number.MAX_SAFE_INTEGER + 1 } }),
    async () => Response.json({ ...snapshot(), counts: { ...zeroCounts(), [INJECTION]: 1 } }),
  ]) {
    const directory = new Directory([[SIGNUP_OBSERVATION_KEY, document()]]);
    const old = directory.values.get(SIGNUP_OBSERVATION_KEY);
    await runScheduledSignupObservation(environment(directory, fetchImpl));
    assert.deepEqual(directory.calls, [], "snapshot failure must not access KV or overwrite real totals");
    assert.equal(directory.values.get(SIGNUP_OBSERVATION_KEY), old);
  }
  const directory = new Directory();
  await runScheduledSignupObservation({ DIRECTORY: directory });
  assert.deepEqual(directory.calls, []);
  await runScheduledSignupObservation({ ACCOUNT_SIGNUP: namespace(() => assert.fail("missing directory must return before fetch")) });
});

test("tick caps gets at 500, distinguishes exact completion and partial pages, and reports list failure", async () => {
  for (const [size, complete] of [[500, true], [501, false]]) {
    const directory = new Directory(Array.from({ length: size }, (_, index) => [`pending:${index}`, {}]));
    await runScheduledSignupObservation(environment(directory));
    assert.equal(directory.calls.filter(([operation]) => operation === "get").length, 500);
    assert.equal(directory.calls.filter(([operation]) => operation === "list").length, 1);
    assert.deepEqual(directory.observation().pending, { accounts: 500, oldest_created_at: null, complete });
  }
  const partial = new Directory([["pending:one", {}]]); partial.listComplete = false;
  await runScheduledSignupObservation(environment(partial));
  assert.deepEqual(partial.observation().pending, { accounts: 1, oldest_created_at: null, complete: false });
  const failed = new Directory(); failed.list = async () => { throw new Error("list unavailable"); };
  await runScheduledSignupObservation(environment(failed));
  assert.deepEqual(failed.observation().pending, { accounts: null, oldest_created_at: null, complete: false });
});

test("tick holds exactly one operation at a time across singleton fetch, KV scan and write", async () => {
  let active = 0, peak = 0;
  const instrument = (operation) => async (...args) => {
    active += 1; peak = Math.max(peak, active);
    try { await flush(); return await operation(...args); } finally { active -= 1; }
  };
  const directory = new Directory([["pending:first", {}], ["pending:second", {}], ["pending:third", {}]]);
  for (const name of ["get", "list", "put"]) directory[name] = instrument(directory[name].bind(directory));
  await runScheduledSignupObservation(environment(directory, instrument(async () => Response.json(snapshot()))));
  assert.equal(peak, 1, "snapshot, scan reads and document write must be strictly sequential");
  assert.equal(active, 0);
  assert.equal(directory.writes.length, 1);
});

async function delayedPendingScan(t, keys, delay) {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: NOW });
  const directory = new Directory(Array.from({ length: keys }, (_, i) => [
    `pending:${i}`, { created_at: new Date(NOW - (i + 1) * 1000).toISOString() },
  ]));
  const counts = Object.fromEntries(KEYS.map((key, i) => [key, i + 1]));
  let active = 0, peak = 0, settled = false;
  const instrument = (operation, milliseconds = 0) => async (...args) => {
    active += 1; peak = Math.max(peak, active);
    try {
      if (milliseconds) await new Promise((resolve) => setTimeout(resolve, milliseconds));
      return await operation(...args);
    } finally { active -= 1; }
  };
  for (const name of ["get", "list", "put"]) {
    directory[name] = instrument(directory[name].bind(directory), name === "get" ? delay : 0);
  }
  const pending = runScheduledSignupObservation(environment(directory,
    instrument(async () => Response.json({ schema_version: 1, counts })),
  )).then(() => { settled = true; });
  await flush();
  for (let i = 0; i < keys && !settled; i++) {
    t.mock.timers.tick(delay);
    await flush();
  }
  assert.equal(settled, true, "scan must settle within its total time budget");
  await pending;
  assert.equal(directory.writes.length, 1, "settling reads must publish exactly one document");
  assert.equal(directory.writes[0].key, SIGNUP_OBSERVATION_KEY);
  assert.deepEqual(directory.observation().counts, counts, "partial scans retain all twelve counters");
  assert.equal(directory.calls.filter(([operation]) => operation === "list").length, 1);
  assert.equal(peak, 1, "snapshot, reads and publication must remain sequential");
  assert.equal(active, 0);
  return directory;
}

test("settling slow scan publishes partial coverage at eight seconds", async (t) => {
  const directory = await delayedPendingScan(t, 450, 25);
  assert.equal(Date.now(), NOW + 8000);
  assert.equal(directory.calls.filter(([operation]) => operation === "get").length, 320);
  assert.deepEqual(directory.observation().pending, {
    accounts: 320, oldest_created_at: new Date(NOW - 320_000).toISOString(), complete: false,
  });
  const published = new Directory([[SIGNUP_OBSERVATION_KEY, directory.observation()]]);
  assert.match((await signupObservationLines({ DIRECTORY: published })).join("\n"),
    /^witself_signup_pending_scan_complete 0$/m);
});

test("settling fast scan publishes complete coverage inside the time budget", async (t) => {
  const directory = await delayedPendingScan(t, 300, 15);
  assert.equal(Date.now(), NOW + 4500);
  assert.equal(directory.calls.filter(([operation]) => operation === "get").length, 300);
  assert.deepEqual(directory.observation().pending, {
    accounts: 300, oldest_created_at: new Date(NOW - 300_000).toISOString(), complete: true,
  });
});

test("scan deadline never starts a write or another read beside a pending KV call, even after late completion", async (t) => {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: NOW });
  const directory = new Directory([["pending:first", {}], ["pending:second", {}]]);
  let release, gets = 0;
  directory.get = () => { gets += 1; return new Promise((resolve) => { release = resolve; }); };
  const pending = runScheduledSignupObservation(environment(directory));
  await flush(); t.mock.timers.tick(10_000); await pending;
  assert.equal(gets, 1);
  assert.deepEqual(directory.writes, [], "timed-out unabortable KV call must not overlap a write");
  release({}); await flush();
  assert.equal(gets, 1, "late read must not resume scan");
  assert.deepEqual(directory.writes, [], "late scan must not publish");
});

test("valid exposition gives all fixed families and twelve closed-label counters, aging only at scrape time", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const value = document(); value.counts = Object.fromEntries(KEYS.map((key, index) => [key, index + 1]));
  const directory = new Directory([[SIGNUP_OBSERVATION_KEY, value]]);
  const body = await scrape(directory);
  const expected = [
    'witself_signup_events_total{event="started"} 1', 'witself_signup_events_total{event="provisioned"} 2',
    'witself_signup_events_total{event="provision_failed"} 3', 'witself_signup_events_total{event="verified"} 4',
    'witself_signup_denials_total{reason="quota_per_ip"} 5', 'witself_signup_denials_total{reason="quota_global"} 6',
    'witself_signup_denials_total{reason="turnstile_invalid"} 7', 'witself_signup_denials_total{reason="turnstile_unavailable"} 8',
    'witself_signup_verification_email_total{context="signup",outcome="sent"} 9',
    'witself_signup_verification_email_total{context="signup",outcome="failed"} 10',
    'witself_signup_verification_email_total{context="resend",outcome="sent"} 11',
    'witself_signup_verification_email_total{context="resend",outcome="failed"} 12',
  ];
  assert.deepEqual(samples(body).filter((line) => line.includes("_total{")), expected);
  for (const [name, type] of [["metrics_up", "gauge"], ["observed_timestamp_seconds", "gauge"],
    ["events_total", "counter"], ["denials_total", "counter"], ["verification_email_total", "counter"],
    ["pending_scan_complete", "gauge"], ["pending_accounts", "gauge"], ["pending_oldest_age_seconds", "gauge"]]) {
    assert.match(body, new RegExp(`^# HELP witself_signup_${name} .+$`, "m"));
    assert.match(body, new RegExp(`^# TYPE witself_signup_${name} ${type}$`, "m"));
  }
  assert.match(body, /^witself_signup_metrics_up 1$/m);
  assert.match(body, /^witself_signup_pending_oldest_age_seconds 1800$/m);
  t.mock.timers.tick(60_000);
  assert.match(await scrape(directory), /^witself_signup_pending_oldest_age_seconds 1860$/m);
  assert.equal(directory.observation().observed_at, value.observed_at);
});

test("unknown pending coverage omits count and age, while empty or unparseable times emit age zero", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  for (const [accounts, complete] of [[null, false], [0, true], [2, false]]) {
    const value = document(); value.pending = { accounts, oldest_created_at: null, complete };
    const body = await scrape(new Directory([[SIGNUP_OBSERVATION_KEY, value]]));
    assert.match(body, /^witself_signup_metrics_up 1$/m);
    if (accounts === null) {
      assert.doesNotMatch(body, /^witself_signup_pending_accounts /m);
      assert.doesNotMatch(body, /^witself_signup_pending_oldest_age_seconds /m);
    } else assert.match(body, /^witself_signup_pending_oldest_age_seconds 0$/m);
  }
});

test("invalid documents emit only unavailable and leave every other metrics family byte-identical", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const reference = await scrape(new Directory());
  const nonSignup = (body) => body.split("\n").filter((line) => !line.includes("witself_signup_")).join("\n");
  const attacks = [
    () => null, () => "{", (v) => JSON.stringify(v) + " ".repeat(4097),
    (v) => { v[INJECTION] = 1; }, (v) => { v.counts[INJECTION] = 1; }, (v) => { v.pending[INJECTION] = 1; },
    (v) => { v.counts.started = -1; }, (v) => { v.counts.started = Number.MAX_SAFE_INTEGER + 1; },
    (v) => { v.counts.started = 1.5; }, (v) => { delete v.counts.started; },
    (v) => { v.observed_at = new Date(NOW + 60_001).toISOString(); },
    (v) => { v.observed_at = "2026-10-10T12:00:00Z"; },
    (v) => { v.pending.oldest_created_at = "2026-10-10T13:00:00.000Z"; },
    (v) => { v.pending.oldest_created_at = "2026-02-30T00:00:00.000Z"; },
    (v) => { v.pending.accounts = -1; }, (v) => { v.pending.accounts = Number.MAX_SAFE_INTEGER + 1; },
    (v) => { v.pending.accounts = null; }, (v) => { v.pending.accounts = 0; },
    (v) => { v.pending = { accounts: null, oldest_created_at: null, complete: true }; },
    (v) => { v.pending.complete = "true"; }, (v) => { v.schema_version = 2; },
  ];
  for (const attack of attacks) {
    const value = document(); const override = attack(value);
    const raw = override === undefined ? JSON.stringify(value) : override;
    const directory = new Directory();
    if (raw !== null) directory.values.set(SIGNUP_OBSERVATION_KEY, raw);
    const body = await scrape(directory);
    assert.deepEqual(samples(body), ["witself_signup_metrics_up 0"]);
    assert.equal(nonSignup(body), nonSignup(reference));
    assert.ok(!body.includes(INJECTION));
  }
});

test("a valid observation document of exactly 4096 bytes remains available", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const value = document();
  const raw = JSON.stringify(value);
  const atLimit = raw + " ".repeat(4096 - Buffer.byteLength(raw));
  assert.equal(Buffer.byteLength(atLimit), 4096);
  assert.deepEqual(JSON.parse(atLimit), value);
  const directory = new Directory();
  directory.values.set(SIGNUP_OBSERVATION_KEY, atLimit);
  const body = await scrape(directory);
  assert.match(body, /^witself_signup_metrics_up 1$/m);
  assert.deepEqual(samples(body), samples(await scrape(new Directory([[SIGNUP_OBSERVATION_KEY, value]]))));
});

test("failed or hanging observation read remains HTTP 200 and cannot publish late lines", async (t) => {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: NOW });
  const reference = await scrape(new Directory());
  for (const hangs of [false, true]) {
    const directory = new Directory(); const original = directory.get.bind(directory); let release;
    directory.get = async (key, options) => {
      if (key !== SIGNUP_OBSERVATION_KEY) return original(key, options);
      if (!hangs) throw new Error(MARKERS.join(" "));
      return new Promise((resolve) => { release = resolve; });
    };
    const pending = scrape(directory); await flush();
    t.mock.timers.tick(5000); await flush();
    const body = await pending;
    assert.equal(body, reference);
    release?.(JSON.stringify(document())); await flush();
    assert.deepEqual(samples(body), ["witself_signup_metrics_up 0"]);
  }
  const onlyDirectory = new Proxy({ DIRECTORY: new Directory() }, { get(target, key) {
    assert.equal(key, "DIRECTORY", "signup exposition may touch only DIRECTORY"); return target[key];
  } });
  assert.deepEqual((await signupObservationLines(onlyDirectory)).filter((line) => !line.startsWith("#")), ["witself_signup_metrics_up 0"]);
});
