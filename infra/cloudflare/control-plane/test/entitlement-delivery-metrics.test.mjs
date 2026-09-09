import assert from "node:assert/strict";
import test from "node:test";

import { uptimeProbeMetricsResponse } from "../src/uptime-probes.mjs";
import {
  PLAN_LIFECYCLE_CURSOR_KEY as KEY, entitlementDeliveryLines,
  writeDeliveryCheckpoint,
} from "../src/entitlement-delivery-metrics.mjs";
import { runScheduledPlanLifecycle } from "../src/bridge.mjs";

const NOW = "2026-09-09T00:00:00.000Z";
const PRIVATE = "private-account-cursor-token-must-not-appear";
const counts = (pending = 0, failed = 0) => ({ scanned: 1, seeded: 0, apply_pending: pending, failed });
const flush = () => new Promise((resolve) => setImmediate(resolve));
class Directory {
  constructor() { this.raw = null; this.writes = []; this.reads = []; this.cursor = null; }
  async get(key, options) {
    this.reads.push(key);
    if (key !== KEY) return key.startsWith("acct:") ? { cell: "cell-fixture" } : null;
    return options?.type === "json" ? JSON.parse(this.raw) : this.raw;
  }
  async list(options) { this.listed = options; return { keys: [{ name: "acct:fixture" }], list_complete: this.cursor === null, cursor: this.cursor }; }
  async put(key, raw) { assert.equal(key, KEY); this.writes.push(raw); this.raw = raw; }
  document() { return JSON.parse(this.raw); }
}
const environment = (directory) => ({ DIRECTORY: directory, CP_PLAN_LIFECYCLE_ENABLED: "true", INTERNAL_BRIDGE_TOKEN: "fixture-token" });
const lines = async (directory, flag = "true") => (await entitlementDeliveryLines({ DIRECTORY: directory, CP_PLAN_LIFECYCLE_ENABLED: flag })).join("\n");
const ack = (pending = 0, failed = 0) => new Response(JSON.stringify({ schema_version: "witself.v0", plan_lifecycle: { ...counts(pending, failed), succeeded: failed === 0 } }));
async function checkpoint(directory, cursor, next, result = counts()) {
  await writeDeliveryCheckpoint(directory, directory.document(), cursor, next, result);
  return directory.document();
}

test("enabled entitlement monitoring exposes missing coverage without breaking healthy probes", async () => {
  const env = {
    CP_PLAN_LIFECYCLE_ENABLED: "true",
    DIRECTORY: { async get() { return null; } },
  };
  const response = await uptimeProbeMetricsResponse(
    new Request("https://example.test/metrics/probes"), env,
  );
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.match(body, /^witself_entitlement_delivery_metrics_up 0$/m,
    "missing entitlement observations must not be silent or healthy");
  assert.match(body, /^witself_entitlement_delivery_enabled 1$/m);
  assert.doesNotMatch(body, /^witself_entitlement_delivery_last_cycle_accounts/m);
});

test("complete traversal retains earlier pending and failed observations and updates counts atomically", async () => {
  const d = new Directory();
  await checkpoint(d, undefined, PRIVATE, counts(1, 1));
  let body = await lines(d);
  assert.match(body, /cycle_coverage_complete 0/);
  assert.doesNotMatch(body, /last_cycle_accounts/);
  const first = d.document();
  await checkpoint(d, PRIVATE, null);
  const last = d.document();
  assert.equal(last.cursor, null);
  assert.equal(last.monitoring.last_cycle.pages, 2);
  assert.deepEqual(last.monitoring.last_cycle.counts, { scanned: 2, seeded: 0, apply_pending: 1, failed: 1 });
  assert.equal(last.monitoring.last_ack_at, last.updated_at);
  assert.equal(last.monitoring.last_cycle.started_at, first.monitoring.cycle.started_at);
  body = await lines(d);
  assert.match(body, /last_cycle_accounts\{outcome="apply_pending"\} 1/);
  assert.match(body, /last_cycle_accounts\{outcome="failed"\} 1/);
  assert.ok(!body.includes(PRIVATE));
  await checkpoint(d, undefined, "next-cycle");
  assert.deepEqual(d.document().monitoring.last_cycle, last.monitoring.last_cycle);
});

test("empty complete directory is distinct from an unmeasured partial traversal", async () => {
  const d = new Directory();
  await checkpoint(d, undefined, null, { scanned: 0, seeded: 0, apply_pending: 0, failed: 0 });
  assert.match(await lines(d), /cycle_coverage_complete 1/);
  assert.match(await lines(d), /last_cycle_accounts\{outcome="scanned"\} 0/);
  d.raw = JSON.stringify({ cursor: "legacy-tail", updated_at: NOW });
  await checkpoint(d, "legacy-tail", null);
  assert.match(await lines(d), /cycle_coverage_complete 0/);
  assert.doesNotMatch(await lines(d), /last_cycle_accounts/);
  await checkpoint(d, undefined, null);
  assert.match(await lines(d), /cycle_coverage_complete 1/);
});

test("malformed or cross-cursor monitoring never promotes a tail to a complete cycle", async (t) => {
  for (const mutate of [
    (v) => { v.monitoring.last_page.scanned = Number.MAX_SAFE_INTEGER + 1; },
    (v) => { v.monitoring.cycle.resume = "0".repeat(64); },
    (v) => { v.monitoring.cycle.counts.failed = 2; },
    (v) => { v.monitoring.cycle.pages = 512; },
    (v) => { v.monitoring.last_ack_at = "tomorrow"; },
    (v) => { v.monitoring = null; },
  ]) {
    await t.test(String(mutate), async () => {
      const d = new Directory(); await checkpoint(d, undefined, "tail");
      const v = d.document(); mutate(v); d.raw = JSON.stringify(v);
      await checkpoint(d, "tail", null);
      assert.equal(d.document().cursor, null);
      assert.equal(d.document().monitoring.last_cycle, null);
    });
  }
});

test("repeated cursor chain loses coverage without altering the intended cursor", async () => {
  const d = new Directory();
  await checkpoint(d, undefined, "one");
  await checkpoint(d, "one", "two");
  await checkpoint(d, "two", "one");
  assert.equal(d.document().cursor, "one");
  assert.equal(d.document().monitoring.cycle, null);
  await checkpoint(d, "one", null);
  assert.equal(d.document().monitoring.last_cycle, null);
});

test("unrooted persisted traversal cannot publish a complete cycle", async () => {
  const d = new Directory(); await checkpoint(d, undefined, "tail");
  const v = d.document(); v.monitoring.cycle.seen[0] = "0".repeat(64); d.raw = JSON.stringify(v);
  await checkpoint(d, "tail", null);
  assert.equal(d.document().monitoring.last_cycle, null,
    "a traversal without the root sentinel must not become a complete cycle");
});

test("persisted aggregate cannot erase pending observations from its last page", async () => {
  const d = new Directory(); await checkpoint(d, undefined, "tail", counts(1, 1));
  const v = d.document(); v.monitoring.cycle.counts.apply_pending = 0; d.raw = JSON.stringify(v);
  await checkpoint(d, "tail", null);
  assert.equal(d.document().monitoring.last_cycle, null,
    "an aggregate smaller than its last page must not become a healthy complete cycle");
});

test("every accumulated outcome and terminal completion must include its last page", async (t) => {
  for (const field of ["scanned", "seeded", "apply_pending", "failed"]) {
    for (const terminal of [false, true]) await t.test(`${field} terminal=${terminal}`, async () => {
      const d = new Directory();
      await checkpoint(d, undefined, terminal ? null : "tail", { scanned: 1, seeded: 1, apply_pending: 1, failed: 1 });
      const v = d.document(); const target = terminal ? v.monitoring.last_cycle : v.monitoring.cycle;
      target.counts[field] = 0; d.raw = JSON.stringify(v);
      assert.match(await lines(d), /snapshot_state\{state="invalid"\} 1/);
      assert.doesNotMatch(await lines(d), /last_cycle_accounts/);
    });
  }
});

test("independent branches and stale complete writes never merge partial cycle counts", async () => {
  const start = new Directory(); await checkpoint(start, undefined, "tail", counts(1));
  const a = new Directory(), b = new Directory(); a.raw = b.raw = start.raw;
  await Promise.all([checkpoint(a, "tail", null), checkpoint(b, "tail", null, counts(1, 1))]);
  assert.equal(a.document().monitoring.last_cycle.counts.scanned, 2);
  assert.equal(b.document().monitoring.last_cycle.counts.scanned, 2);
  assert.equal(a.document().monitoring.last_cycle.counts.apply_pending, 1);
  assert.equal(b.document().monitoring.last_cycle.counts.apply_pending, 2);
  // KV can expose either coherent branch; no cross-isolate CAS is claimed.
  a.raw = b.raw;
  assert.match(await lines(a), /last_cycle_accounts\{outcome="apply_pending"\} 2/);
  const stored = a.document(); stored.cursor = "unrelated-tail"; a.raw = JSON.stringify(stored);
  await checkpoint(a, "unrelated-tail", null);
  assert.equal(a.document().monitoring.last_cycle.counts.scanned, 2); // prior completion only
});

test("monitoring rejection falls back to the exact old cursor envelope without another CP call", async () => {
  const d = new Directory(); d.cursor = "next";
  d.put = async (key, raw) => {
    d.writes.push(raw);
    if (JSON.parse(raw).monitoring) throw new Error(PRIVATE);
    assert.equal(key, KEY); d.raw = raw;
  };
  let calls = 0;
  const result = await runScheduledPlanLifecycle(environment(d), async (request) => {
    calls++; assert.deepEqual(await request.json(), { account_ids: ["fixture"] }); return ack(1, 1);
  });
  assert.equal(result.succeeded, false);
  assert.equal(calls, 1);
  assert.equal(d.writes.length, 2);
  const first = JSON.parse(d.writes[0]), second = JSON.parse(d.writes[1]);
  assert.deepEqual(second, { cursor: first.cursor, updated_at: first.updated_at });
  assert.equal(second.cursor, "next");
  assert.match(await lines(d), /snapshot_state\{state="invalid"\} 1/);
});

test("both checkpoint writes failing preserve prior progress and do not retry billing", async () => {
  const d = new Directory(); await checkpoint(d, undefined, "before"); const original = d.raw;
  d.cursor = "after"; let writes = 0, calls = 0;
  d.put = async () => { writes++; throw new Error(PRIVATE); };
  const result = await runScheduledPlanLifecycle(environment(d), async () => { calls++; return ack(); });
  assert.deepEqual(result, { ran: true, succeeded: false });
  assert.equal(calls, 1); assert.equal(writes, 2); assert.equal(d.raw, original);
});

test("failed, partial and disabled ticks never write or advance the cursor", async (t) => {
  for (const [name, response] of [
    ["non-ok", () => new Response(PRIVATE, { status: 503 })],
    ["invalid JSON", () => new Response("{")],
    ["partial page", () => new Response(JSON.stringify({ schema_version: "witself.v0", plan_lifecycle: { ...counts(), scanned: 0, succeeded: true } }))],
  ]) await t.test(name, async () => {
    const d = new Directory(); await checkpoint(d, undefined, "held"); const before = d.raw; d.writes = [];
    assert.equal((await runScheduledPlanLifecycle(environment(d), async () => response())).succeeded, false);
    assert.equal(d.raw, before); assert.deepEqual(d.writes, []);
  });
  const d = new Directory(); const env = environment(d); env.CP_PLAN_LIFECYCLE_ENABLED = "false";
  assert.deepEqual(await runScheduledPlanLifecycle(env, () => assert.fail("disabled tick reached CP")), { ran: false, configured: true });
  assert.deepEqual(d.reads, []); assert.deepEqual(d.writes, []);
  env.CP_PLAN_LIFECYCLE_ENABLED = "true"; env.INTERNAL_BRIDGE_TOKEN = "";
  assert.deepEqual(await runScheduledPlanLifecycle(env, () => assert.fail("unconfigured tick reached CP")), { ran: false, configured: false });
});

test("public state is closed and disabled state never emits stale successful counts", async () => {
  const d = new Directory();
  assert.match(await lines(d), /snapshot_state\{state="missing"\} 1/);
  d.raw = "{"; assert.match(await lines(d), /snapshot_state\{state="invalid"\} 1/);
  d.raw = JSON.stringify({ cursor: null, updated_at: NOW, monitoring: PRIVATE });
  assert.ok(!(await lines(d)).includes(PRIVATE));
  await checkpoint(d, undefined, null);
  assert.match(await lines(d), /metrics_up 1/);
  const disabled = await lines(d, "false");
  assert.match(disabled, /enabled 0/); assert.match(disabled, /state="disabled"/);
  assert.doesNotMatch(disabled, /last_cycle_accounts/);
  d.get = async () => { throw new Error(PRIVATE); };
  assert.match(await lines(d), /state="unavailable"/);
});

test("entitlement KV failure and timeout leave healthy probe HTTP200 and no late follow-on reads", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const failure of ["throw", "timeout"]) {
    let release, calls = 0;
    const env = { CP_PLAN_LIFECYCLE_ENABLED: "true", DIRECTORY: { get(key) {
      calls++;
      if (key !== KEY) return Promise.resolve(null);
      if (failure === "throw") throw new Error(PRIVATE);
      return new Promise((resolve) => { release = resolve; });
    } } };
    const pending = uptimeProbeMetricsResponse(new Request("https://example.test/metrics/probes"), env);
    await flush(); t.mock.timers.tick(5000); await flush();
    const response = await pending; assert.equal(response.status, 200);
    assert.match(await response.text(), /snapshot_state\{state="unavailable"\} 1/);
    const before = calls; release?.(JSON.stringify({ private: PRIVATE })); await flush(); assert.equal(calls, before);
  }
});

test("non-OK and stalled CP bodies are cancelled without late cursor writes", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const logged = [];
  t.mock.method(console, "log", (message) => { logged.push(message); });
  for (const status of [503, 200]) {
    logged.length = 0;
    const d = new Directory(); let cancelled = 0;
    const pending = runScheduledPlanLifecycle(environment(d), async () => new Response(new ReadableStream({
      start(c) { if (status === 200) c.enqueue(new TextEncoder().encode("{")); },
      cancel() { cancelled++; },
    }), { status }));
    await flush(); t.mock.timers.tick(240000); await flush();
    assert.equal((await pending).succeeded, false);
    assert.equal(cancelled, 1); assert.deepEqual(d.writes, []);
    assert.deepEqual(logged, [status === 503
      ? "plan-lifecycle: scheduled tick failed status=503"
      : "plan-lifecycle: scheduled tick returned invalid JSON"]);
  }
});

test("slow existing directory reads preserve the original single CP request", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const method of ["get", "list"]) {
    const d = new Directory(); let release, calls = 0, complete = false;
    const original = d[method].bind(d); let first = true;
    d[method] = (...args) => {
      if (!first) return original(...args);
      first = false;
      return new Promise((resolve) => { release = () => resolve(original(...args)); });
    };
    const result = runScheduledPlanLifecycle(environment(d), async (request) => {
      calls++; assert.deepEqual(await request.json(), { account_ids: ["fixture"] }); return ack();
    });
    result.then(() => { complete = true; });
    await flush(); t.mock.timers.tick(30000); await flush();
    assert.equal(complete, false); assert.equal(calls, 0);
    release(); assert.equal((await result).succeeded, true); assert.equal(calls, 1);
    assert.equal(d.document().cursor, null);
  }
});

test("late optional construction only writes the original cursor envelope once", async (t) => {
  const digest = crypto.subtle.digest.bind(crypto.subtle);
  const prepared = new Map(await Promise.all(["null", '"continue"'].map(async (value) =>
    [value, await digest("SHA-256", new TextEncoder().encode(value))])));
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let release;
  const calls = [];
  t.mock.method(crypto.subtle, "digest", (algorithm, bytes) => {
    assert.equal(algorithm, "SHA-256");
    const value = new TextDecoder().decode(bytes);
    assert.ok(prepared.has(value)); calls.push(value);
    const result = prepared.get(value).slice(0);
    if (calls.length > 1) return Promise.resolve(result);
    return new Promise((resolve) => { release = () => resolve(result); });
  });
  const d = new Directory();
  const result = checkpoint(d, undefined, "continue");
  await flush(); t.mock.timers.tick(1000); await flush(); await result;
  assert.deepEqual(Object.keys(d.document()).sort(), ["cursor", "updated_at"]);
  assert.equal(d.document().cursor, "continue"); assert.equal(d.writes.length, 1);
  release(); await flush();
  // Every digest is already computed, so draining this turn settles the full
  // late construction and its validation, not merely the first held promise.
  assert.deepEqual(calls, ["null", '"continue"', "null", '"continue"']);
  assert.equal(d.writes.length, 1);
});

test("observation timestamps stay fixed while the scrape clock advances", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: Date.parse(NOW) });
  const d = new Directory(); await checkpoint(d, undefined, null);
  const before = await lines(d); t.mock.timers.tick(3600000);
  assert.equal(await lines(d), before);
});

test("future acknowledgement metadata cannot indefinitely suppress staleness", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: Date.parse(NOW) });
  const d = new Directory(); await checkpoint(d, undefined, null);
  const v = d.document(); v.updated_at = v.monitoring.last_ack_at = "2099-01-01T00:00:00.000Z";
  d.raw = JSON.stringify(v);
  assert.match(await lines(d), /snapshot_state\{state="invalid"\} 1/,
    "future acknowledgement timestamps must be unavailable rather than healthy forever");
  assert.doesNotMatch(await lines(d), /last_ack_timestamp_seconds/);
  v.cursor = "future-tail"; d.raw = JSON.stringify(v);
  await checkpoint(d, "future-tail", "next-tail");
  assert.equal(d.document().cursor, "next-tail");
  assert.equal(d.document().monitoring.last_cycle, null);
  assert.equal(d.document().monitoring.cycle, null);
});
