import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

import { runScheduledPlanLifecycle } from "../src/bridge.mjs";
import { PLAN_LIFECYCLE_CURSOR_KEY as KEY } from "../src/entitlement-delivery-metrics.mjs";
import { stripeObservationLines } from "../src/stripe-observation.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const { default: worker } = await import("../src/index.js");
const { containerCalls, resetContainerCalls } = await import("./fixtures/cloudflare-containers-stub.mjs");
const NOW = Date.parse("2026-09-20T12:00:00.000Z");
const PRIVATE = 'acct_private_cus_private_sub_private_evt_private"} injected_metric{identity="private';
const flush = () => new Promise((resolve) => setImmediate(resolve));
const snapshot = (overrides = {}) => ({
  schema_version: 1,
  webhook_events: { verified: 7, rejected: 2, replayed: 3 },
  reconciliation_failures: 4,
  oldest_pending_at: "2026-09-20T11:40:00Z",
  reconciliation_observed: true,
  reconciliation_complete: false,
  observed_at: new Date(NOW).toISOString(),
  ...overrides,
});
class Directory {
  constructor() { this.values = new Map(); this.writes = []; this.reads = []; }
  async get(key, options) {
    this.reads.push(key);
    if (key.startsWith("acct:")) return { cell: "cell-fixture" };
    const raw = this.values.get(key) ?? null;
    return options?.type === "json" ? JSON.parse(raw) : raw;
  }
  async list() { return { keys: [{ name: "acct:fixture" }], list_complete: true }; }
  async put(key, raw) { this.writes.push({ key, raw }); this.values.set(key, raw); }
  document() { return JSON.parse(this.values.get(KEY)); }
}
const environment = (directory) => ({ DIRECTORY: directory,
  CP_PLAN_LIFECYCLE_ENABLED: "true", INTERNAL_BRIDGE_TOKEN: "fixture-bridge" });
const acknowledgement = (observation) => ({
  schema_version: "witself.v0",
  plan_lifecycle: { scanned: 1, seeded: 0, apply_pending: 0, failed: 0, succeeded: true },
  ...(observation === undefined ? {} : { stripe_observation: observation }),
  event: { account: PRIVATE, customer: PRIVATE, subscription: PRIVATE, id: PRIVATE },
});
async function tick(directory, observation, requests = []) {
  return runScheduledPlanLifecycle(environment(directory), async (request) => {
    requests.push({ path: new URL(request.url).pathname, body: await request.json() });
    return Response.json(acknowledgement(observation));
  });
}
async function scrape(directory, extra = {}) {
  const response = await worker.fetch(new Request("https://example.test/metrics/probes"),
    { ...environment(directory), ...extra }, {});
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  return response.text();
}
const samples = (body) => body.split("\n").filter((line) => line.startsWith("witself_cp_"));

test("authenticated tick publishes exactly the three Stripe series through the public Worker probe route", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new Directory(), requests = [];
  const original = await tick(new Directory(), undefined);
  assert.deepEqual(await tick(directory, snapshot(), requests), original,
    "observation must not alter the scheduler's return values");
  assert.deepEqual(requests, [{ path: "/v1/plan-lifecycle:tick", body: { account_ids: ["fixture"] } }]);
  assert.equal(directory.writes.length, 1, "use the existing atomic checkpoint write");
  assert.equal(directory.writes[0].key, KEY);
  assert.deepEqual(directory.document().stripe_observation, snapshot());
  assert.ok(!directory.writes[0].raw.includes(PRIVATE));
  resetContainerCalls();
  const body = await scrape(directory, { PUBLIC_IP_LIMITER: { limit() { assert.fail("scrape reached limiter"); } } });
  assert.deepEqual(containerCalls, [], "scraping must not wake the Go container");
  assert.deepEqual(samples(body), [
    'witself_cp_stripe_webhook_events_total{outcome="verified"} 7',
    'witself_cp_stripe_webhook_events_total{outcome="rejected"} 2',
    'witself_cp_stripe_webhook_events_total{outcome="replayed"} 3',
    "witself_cp_billing_reconciliation_lag_seconds 1200",
    "witself_cp_billing_reconciliation_failures_total 4",
  ]);
  for (const [name, type] of [["stripe_webhook_events_total", "counter"],
    ["billing_reconciliation_lag_seconds", "gauge"], ["billing_reconciliation_failures_total", "counter"]]) {
    assert.match(body, new RegExp(`^# HELP witself_cp_${name} .+$`, "m"));
    assert.match(body, new RegExp(`^# TYPE witself_cp_${name} ${type}$`, "m"));
  }
  assert.ok(!body.includes(PRIVATE));
  t.mock.timers.tick(60_000);
  assert.match(await scrape(directory), /^witself_cp_billing_reconciliation_lag_seconds 1260$/m,
    "lag ages at scrape time rather than freezing at the last cron tick");
});

test("empty, unknown and incomplete backlog observations never fabricate a healthy lag", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  for (const [observation, lag] of [
    [snapshot({ oldest_pending_at: null, reconciliation_complete: true }), "0"],
    [snapshot({ oldest_pending_at: null, reconciliation_observed: false }), null],
    [snapshot(), "1200"],
  ]) {
    const directory = new Directory(); await tick(directory, observation);
    const body = await scrape(directory);
    if (lag === null) assert.doesNotMatch(body, /^witself_cp_billing_reconciliation_lag_seconds /m);
    else assert.match(body, new RegExp(`^witself_cp_billing_reconciliation_lag_seconds ${lag}$`, "m"));
    assert.match(body, /^witself_cp_billing_reconciliation_failures_total 4$/m);
  }
});

test("process counter resets are projected as resets without invented cross-process increments", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new Directory(); await tick(directory, snapshot());
  await tick(directory, snapshot({ webhook_events: { verified: 1, rejected: 0, replayed: 0 }, reconciliation_failures: 0 }));
  const body = await scrape(directory);
  assert.match(body, /events_total\{outcome="verified"\} 1\n/);
  assert.match(body, /events_total\{outcome="rejected"\} 0\n/);
  assert.match(body, /events_total\{outcome="replayed"\} 0\n/);
  assert.match(body, /failures_total 0\n/);
});

test("untrusted observation fields cannot create identity labels, metric names or numeric samples", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const attacks = [
    (v) => { v.webhook_events[PRIVATE] = 1; },
    (v) => { v.webhook_events.verified = PRIVATE; },
    (v) => { v.account_id = PRIVATE; },
    (v) => { v.customer = PRIVATE; },
    (v) => { v.subscription = PRIVATE; },
    (v) => { v.event_id = PRIVATE; },
    (v) => { v.oldest_pending_at = PRIVATE; },
    (v) => { v.observed_at = PRIVATE; },
    (v) => { v.oldest_pending_at = "2026-02-30T11:40:00Z"; },
    (v) => { v.webhook_events.rejected = -1; },
    (v) => { v.webhook_events.replayed = 1.5; },
    (v) => { v.reconciliation_failures = Number.MAX_SAFE_INTEGER + 1; },
    (v) => { v.schema_version = 2; },
    (v) => { v.reconciliation_observed = false; },
    (v) => { v.oldest_pending_at = null; },
    (v) => { v.observed_at = "2026-09-20T12:02:00Z"; },
  ];
  for (const attack of attacks) {
    const invalid = snapshot(); attack(invalid);
    const directory = new Directory();
    assert.equal((await tick(directory, invalid)).succeeded, true);
    assert.equal(directory.document().stripe_observation, undefined);
    // Check both ingestion and direct persisted-data validation.
    const document = directory.document(); document.stripe_observation = invalid;
    directory.values.set(KEY, JSON.stringify(document));
    const body = await scrape(directory);
    assert.deepEqual(samples(body), []);
    assert.ok(!body.includes(PRIVATE));
    assert.match(body, /^witself_entitlement_delivery_metrics_up 1$/m,
      "invalid optional Stripe data must not discard a valid lifecycle observation");
  }
});

test("missing, legacy, disabled and malformed checkpoints omit Stripe samples", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new Directory();
  assert.deepEqual(samples(await scrape(directory)), []);
  await tick(directory, snapshot());
  assert.deepEqual(samples(await scrape(directory, { CP_PLAN_LIFECYCLE_ENABLED: "false" })), []);
  await tick(directory, undefined);
  assert.deepEqual(samples(await scrape(directory)), [], "legacy/manual tick must not carry stale Stripe totals");
  directory.values.set(KEY, "{");
  assert.deepEqual(samples(await scrape(directory)), []);
  assert.deepEqual(stripeObservationLines(snapshot(), PRIVATE), []);
});

test("Stripe metadata persistence failure uses the original cursor envelope without retrying billing", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new Directory(), requests = [];
  directory.put = async (key, raw) => {
    directory.writes.push({ key, raw });
    if (JSON.parse(raw).stripe_observation) throw new Error(PRIVATE);
    directory.values.set(key, raw);
  };
  assert.equal((await tick(directory, snapshot(), requests)).succeeded, true);
  assert.equal(requests.length, 1);
  assert.equal(directory.writes.length, 2);
  assert.deepEqual(directory.document(), { cursor: null, updated_at: new Date(NOW).toISOString() });
  assert.deepEqual(samples(await scrape(directory)), []);
});

test("checkpoint read failure or timeout keeps the probe response healthy and prevents late Stripe samples", async (t) => {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: NOW });
  for (const stalled of [false, true]) {
    const directory = new Directory(); await tick(directory, snapshot());
    const raw = directory.values.get(KEY); let release;
    directory.get = async (key) => {
      if (key !== KEY) return null;
      if (stalled) return new Promise((resolve) => { release = resolve; });
      throw new Error(PRIVATE);
    };
    const pending = scrape(directory); await flush();
    t.mock.timers.tick(5000); await flush();
    assert.deepEqual(samples(await pending), []);
    release?.(raw); await flush();
    assert.deepEqual(directory.writes.length, 1);
  }
});
