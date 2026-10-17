import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
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
const schema2Fixture = () => JSON.parse(readFileSync(new URL(
  "../../../../internal/cpserver/testdata/stripe-observation-v2.json", import.meta.url)));
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

test("untrusted schema 1 and schema 2 fields cannot create identity labels, metric names or numeric samples", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const attacks = [
    ["private webhook key", (v) => { v.webhook_events[PRIVATE] = 1; }],
    ["private verified count", (v) => { v.webhook_events.verified = PRIVATE; }],
    ["account identity", (v) => { v.account_id = PRIVATE; }],
    ["customer identity", (v) => { v.customer = PRIVATE; }],
    ["subscription identity", (v) => { v.subscription = PRIVATE; }],
    ["event identity", (v) => { v.event_id = PRIVATE; }],
    ["private pending timestamp", (v) => { v.oldest_pending_at = PRIVATE; }],
    ["private observation timestamp", (v) => { v.observed_at = PRIVATE; }],
    ["invalid calendar timestamp", (v) => { v.oldest_pending_at = "2026-02-30T11:40:00Z"; }],
    ["negative rejected count", (v) => { v.webhook_events.rejected = -1; }],
    ["fractional replayed count", (v) => { v.webhook_events.replayed = 1.5; }],
    ["unsafe reconciliation count", (v) => { v.reconciliation_failures = Number.MAX_SAFE_INTEGER + 1; }],
    ["schema version mismatch", (v) => { v.schema_version = 2; }],
    ["unobserved pending timestamp", (v) => { v.reconciliation_observed = false; }],
    ["incomplete empty reconciliation", (v) => {
      v.oldest_pending_at = null; v.reconciliation_complete = false;
    }],
    ["future observation timestamp", (v) => { v.observed_at = "2026-09-20T12:02:00Z"; }],
  ];
  for (const base of [snapshot(), schema2Fixture()]) {
    for (const [attackLabel, attack] of attacks) {
      if (base.schema_version === 2 && attackLabel === "schema version mismatch") continue;
      const label = `schema ${base.schema_version}: ${attackLabel}`;
      const invalid = structuredClone(base); attack(invalid);
      const directory = new Directory();
      assert.equal((await tick(directory, invalid)).succeeded, true, label);
      assert.equal(directory.document().stripe_observation, undefined, label);
      const ingested = await scrape(directory);
      assert.deepEqual(samples(ingested), [], label);
      assert.ok(!ingested.includes(PRIVATE), label);
      // Check direct persisted-data validation as well as ingestion.
      const document = directory.document(); document.stripe_observation = invalid;
      directory.values.set(KEY, JSON.stringify(document));
      const persisted = await scrape(directory);
      assert.deepEqual(samples(persisted), [], label);
      assert.ok(!persisted.includes(PRIVATE), label);
      assert.match(persisted, /^witself_entitlement_delivery_metrics_up 1$/m,
        `${label}: invalid optional Stripe data must not discard a valid lifecycle observation`);
    }
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

test("schema 2 from the Go fixture publishes the processing, batch and blocked series", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const fixture = schema2Fixture();
  const directory = new Directory();
  await tick(directory, fixture);
  assert.deepEqual(directory.document().stripe_observation, fixture);
  assert.equal(JSON.stringify(directory.document().stripe_observation), JSON.stringify(fixture),
    "normalized schema 2 must preserve the Go contract's key order");
  const body = await scrape(directory);
  assert.deepEqual(samples(body), [
    'witself_cp_stripe_webhook_events_total{outcome="verified"} 7',
    'witself_cp_stripe_webhook_events_total{outcome="rejected"} 2',
    'witself_cp_stripe_webhook_events_total{outcome="replayed"} 3',
    'witself_cp_stripe_webhook_events_total{outcome="processing_failed"} 1',
    "witself_cp_billing_reconciliation_lag_seconds 1200",
    "witself_cp_billing_reconciliation_failures_total 4",
    "witself_cp_billing_mutation_batch_failures_total 2",
    "witself_cp_entitlement_apply_blocked_total 6",
  ]);
  assert.match(body, /^# HELP witself_cp_stripe_webhook_events_total .*and verified deliveries whose processing failed;/m);
  for (const [name, help] of [
    ["witself_cp_billing_mutation_batch_failures_total", "Process-lifetime billing-mutation recovery batches that returned an error (list failure, item failure or deadline); sampled by lifecycle cron."],
    ["witself_cp_entitlement_apply_blocked_total", "Process-lifetime account reconciliations whose snapshot the cell's atomic fit check refused; pending, not failed; sampled by lifecycle cron."],
  ]) {
    assert.ok(body.split("\n").includes(`# HELP ${name} ${help}`));
    assert.ok(body.split("\n").includes(`# TYPE ${name} counter`));
  }
});

test("schema 2 mutation batch failures preserve counts distinct from rejected webhooks", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const fixture = schema2Fixture();
  const observation = structuredClone(fixture);
  observation.mutation_batch_failures = 5;
  const directory = new Directory();
  await tick(directory, observation);
  assert.equal(directory.document().stripe_observation.mutation_batch_failures, 5);
  const body = await scrape(directory);
  assert.match(body, /^witself_cp_billing_mutation_batch_failures_total 5$/m);
  assert.match(body, /^witself_cp_stripe_webhook_events_total\{outcome="rejected"\} 2$/m);
});

test("schema 2 accepts processing failures equal to all verified deliveries", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const observation = schema2Fixture();
  observation.webhook_events.processing_failed = observation.webhook_events.verified;
  const directory = new Directory();
  await tick(directory, observation);
  assert.equal(directory.document().stripe_observation.webhook_events.verified, 7);
  assert.equal(directory.document().stripe_observation.webhook_events.processing_failed, 7);
  const body = await scrape(directory);
  assert.match(body, /^witself_cp_stripe_webhook_events_total\{outcome="verified"\} 7$/m);
  assert.match(body, /^witself_cp_stripe_webhook_events_total\{outcome="processing_failed"\} 7$/m);
});

test("schema 1 projection text is unchanged", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  assert.deepEqual(stripeObservationLines(snapshot({ oldest_pending_at: null, reconciliation_complete: true }), new Date(NOW).toISOString()), [
    "# HELP witself_cp_stripe_webhook_events_total Stripe webhook verification attempts and already-processed receipt replays; process-lifetime totals sampled by lifecycle cron.",
    "# TYPE witself_cp_stripe_webhook_events_total counter",
    'witself_cp_stripe_webhook_events_total{outcome="verified"} 7',
    'witself_cp_stripe_webhook_events_total{outcome="rejected"} 2',
    'witself_cp_stripe_webhook_events_total{outcome="replayed"} 3',
    "# HELP witself_cp_billing_reconciliation_lag_seconds Age of the oldest pending mutation receipt observed by bounded reconciliation, or zero after a complete empty scan; not fleet-wide backlog coverage.",
    "# TYPE witself_cp_billing_reconciliation_lag_seconds gauge",
    "witself_cp_billing_reconciliation_lag_seconds 0",
    "# HELP witself_cp_billing_reconciliation_failures_total Process-lifetime account and billing mutation reconciliation failures sampled by lifecycle cron.",
    "# TYPE witself_cp_billing_reconciliation_failures_total counter",
    "witself_cp_billing_reconciliation_failures_total 4",
  ]);
});

test("malformed schema 2 observations are dropped whole", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const fixture = schema2Fixture();
  const attacks = [
    ["missing processing outcome", (v) => { delete v.webhook_events.processing_failed; }],
    ["extra webhook key", (v) => { v.webhook_events.extra = 1; }],
    ["private webhook key", (v) => { v.webhook_events[PRIVATE] = 1; }],
    ["extra top-level key", (v) => { v.extra = 1; }],
    ["processing failures exceed verified", (v) => { v.webhook_events.processing_failed = 8; }],
    ["v1 with v2 keys", (v) => { v.schema_version = 1; }],
  ];
  for (const [name, target] of [
    ["processing_failed", (v) => v.webhook_events],
    ["mutation_batch_failures", (v) => v],
    ["entitlement_apply_blocked", (v) => v],
  ]) {
    for (const [kind, value] of [["private", PRIVATE], ["negative", -1], ["fractional", 1.5],
      ["unsafe integer", Number.MAX_SAFE_INTEGER + 1]]) {
      attacks.push([`${kind} ${name}`, (v) => { target(v)[name] = value; }]);
    }
  }
  for (const [label, attack] of attacks) {
    const invalid = structuredClone(fixture); attack(invalid);
    const directory = new Directory();
    assert.equal((await tick(directory, invalid)).succeeded, true, label);
    assert.equal(directory.document().stripe_observation, undefined, label);
    const ingested = await scrape(directory);
    assert.deepEqual(samples(ingested), [], label);
    assert.ok(!ingested.includes(PRIVATE), label);
    assert.match(ingested, /^witself_entitlement_delivery_metrics_up 1$/m, label);
    const document = directory.document(); document.stripe_observation = invalid;
    directory.values.set(KEY, JSON.stringify(document));
    const persisted = await scrape(directory);
    assert.deepEqual(samples(persisted), [], label);
    assert.ok(!persisted.includes(PRIVATE), label);
    assert.match(persisted, /^witself_entitlement_delivery_metrics_up 1$/m, label);
  }
});
