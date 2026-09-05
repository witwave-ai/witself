import assert from "node:assert/strict";
import { AsyncLocalStorage } from "node:async_hooks";
import { readFile } from "node:fs/promises";
import { register } from "node:module";
import test from "node:test";

import {
  UPTIME_PROBE_RUN_KEY,
  UPTIME_PROBE_SCHEDULER_KEY,
  UPTIME_PROBES_KEY,
  UPTIME_PROBES_PATH,
  runScheduledUptimeProbes,
  uptimeProbeMetricsResponse,
} from "../src/uptime-probes.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const { default: worker } = await import("../src/index.js");
const { containerCalls, resetContainerCalls } = await import(
  "./fixtures/cloudflare-containers-stub.mjs"
);
const probeEnv = (directory) => ({
  DIRECTORY: directory,
  CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED: "true",
});

const NOW = Date.parse("2026-09-04T12:00:00.000Z");
const MAINTENANCE_CRON = "*/5 * * * *";
const PROBE_CRON = "1-59/5 * * * *";
const CP_URL = "https://self.witwave.ai/v1/version";
const PRIVATE_MARKER = "private-value-must-never-escape";
const flush = () => new Promise((resolve) => setImmediate(resolve));

class DirectoryKV {
  constructor(entries = {}) {
    this.values = new Map(Object.entries(entries).map(([key, value]) => [
      key, JSON.stringify(value),
    ]));
    this.reads = [];
    this.lists = [];
    this.writes = [];
  }

  async get(key, options) {
    this.reads.push(key);
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async list({ prefix = "", cursor } = {}) {
    this.lists.push({ prefix, cursor });
    return {
      keys: [...this.values.keys()].filter((key) => key.startsWith(prefix))
        .map((name) => ({ name })),
      list_complete: true,
    };
  }

  async put(key, value, options) {
    this.writes.push({ key, value, options });
    this.values.set(key, value);
  }

  document() {
    return JSON.parse(this.values.get(UPTIME_PROBE_RUN_KEY));
  }

  scheduler() {
    return this.document().scheduler;
  }
}

function snapshot() {
  return {
    schema_version: 1,
    targets: [
      { target: "cell-a", state: "down", http_status_class: 5,
        latency_ms: 1250, checked_at: "2026-09-01T12:00:00.000Z" },
      { target: "control_plane", state: "ok", http_status_class: 2,
        latency_ms: 25, checked_at: "2026-09-01T12:00:01.000Z" },
    ],
  };
}

function scheduler(targetCount = 1) {
  return { last_run_at: new Date(NOW).toISOString(), duration_ms: 10,
    target_count: targetCount, directory_ok: true };
}

function runDocument(targets = snapshot().targets, schedulerRecord = scheduler()) {
  return { scheduler: schedulerRecord, targets };
}

function assertPersistenceMetrics(body, { targetCount, writeOK }) {
  for (const [name, value] of [
    ["witself_probe_snapshot_target_count", targetCount],
    ["witself_probe_last_write_ok", Number(writeOK)],
  ]) {
    assert.ok(body.includes(`# HELP ${name} `), `missing HELP for ${name}`);
    assert.ok(body.includes(`# TYPE ${name} gauge\n`), `missing TYPE for ${name}`);
    assert.equal(body.split("\n").filter((line) => line.startsWith(`${name} `)).length, 1);
    assert.ok(body.includes(`${name} ${value}\n`), `incorrect persistence sample for ${name}`);
  }
}

function assertMissingRunMetrics(body) {
  assertSchedulerMetrics(body, { timestamp: 0, targetCount: 0, directoryOK: false });
  assertPersistenceMetrics(body, { targetCount: 0, writeOK: false });
  assert.ok(body.includes("witself_probe_document_was_malformed 0\n"));
  assert.equal(body.includes("{target="), false);
  assert.equal(body.includes(PRIVATE_MARKER), false);
}

function metricsRequest(method = "GET") {
  return new Request(`https://self.witwave.ai${UPTIME_PROBES_PATH}`, {
    method,
    headers: { Authorization: `Bearer ${PRIVATE_MARKER}` },
  });
}

function assertSchedulerMetrics(body, { timestamp, targetCount, directoryOK }) {
  for (const [name, value] of [
    ["witself_probe_scheduler_last_run_timestamp_seconds", timestamp],
    ["witself_probe_scheduler_target_count", targetCount],
    ["witself_probe_directory_ok", Number(directoryOK)],
  ]) {
    assert.ok(body.includes(`# HELP ${name} `), `missing HELP for ${name}`);
    assert.ok(body.includes(`# TYPE ${name} gauge\n`), `missing TYPE for ${name}`);
    assert.equal(body.split("\n").filter((line) => line.startsWith(`${name} `)).length, 1);
    assert.ok(body.includes(`${name} ${value}\n`), `incorrect scheduler sample for ${name}`);
  }
}

test("the template cron triggers keep probe and maintenance work exclusive", async (t) => {
  const template = await readFile(new URL("../wrangler.template.jsonc", import.meta.url), "utf8");
  const cronList = template.match(/"crons"\s*:\s*(\[[\s\S]*?\])/);
  assert.ok(cronList, "deployment must configure both cron triggers");
  assert.deepEqual(JSON.parse(cronList[1]), [MAINTENANCE_CRON, PROBE_CRON]);
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url) => {
    calls.push(url);
    return new Response(null, { status: 200 });
  });
  for (const cron of [PROBE_CRON, MAINTENANCE_CRON, "unknown", "toString", undefined]) {
    const directory = new DirectoryKV({ "cell:a": { endpoint: "https://a.test.invalid" } });
    const pending = [];
    calls.length = 0;
    await worker.scheduled({ cron, scheduledTime: NOW }, { DIRECTORY: directory }, {
      waitUntil(promise) { pending.push(promise); },
    });
    await Promise.all(pending);
    if (cron === PROBE_CRON) {
      assert.equal(pending.length, 1, "the probe invocation must register only its scheduler");
      assert.deepEqual(calls, ["https://a.test.invalid/v1/version"]);
      assert.deepEqual(directory.reads.sort(), [
        "cell:a", UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY,
      ].sort(), "the probe cron must not read maintenance configuration");
      assert.deepEqual(directory.lists.map(({ prefix }) => prefix), ["cell:"]);
      assert.deepEqual(directory.writes.map(({ key }) => key), [UPTIME_PROBE_RUN_KEY]);
    } else {
      assert.equal(pending.length, cron === MAINTENANCE_CRON ? 6 : 0);
      assert.deepEqual(calls, [], "maintenance and unknown crons must never probe");
      assert.equal(directory.reads.includes(UPTIME_PROBE_RUN_KEY), false);
      assert.deepEqual(directory.writes, []);
      assert.deepEqual(directory.lists, []);
    }
  }
});

test("scheduled probes run concurrently, time out at 10s, and persist only value-free results", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const directory = new DirectoryKV(Object.fromEntries(
    ["ok", "fail", "timeout", "reject"].map((name) => [`cell:${name}`, {
      endpoint: `https://${name}.test.invalid`,
      name: PRIVATE_MARKER,
      provision_token: PRIVATE_MARKER,
      backup_token: PRIVATE_MARKER,
      account_id: PRIVATE_MARKER,
      region: PRIVATE_MARKER,
    }]),
  ));
  const calls = [];
  let canceledBodies = 0;
  let timeoutAborted = false;
  t.mock.method(globalThis, "fetch", async (url, options) => {
    calls.push({ url, options });
    assert.equal(options.method, "GET");
    assert.equal(options.redirect, "manual");
    assert.equal(options.cache, "no-store");
    assert.equal(new Headers(options.headers).has("Authorization"), false);
    assert.notEqual(options.credentials, "include");
    assert.ok(options.signal instanceof AbortSignal);
    if (url === "https://timeout.test.invalid/v1/version") {
      return new Promise((_, reject) => options.signal.addEventListener("abort", () => {
        timeoutAborted = true;
        reject(new Error(PRIVATE_MARKER));
      }, { once: true }));
    }
    if (url === "https://reject.test.invalid/v1/version") throw new Error(PRIVATE_MARKER);
    return new Response(new ReadableStream({
      cancel() { canceledBodies += 1; },
    }), { status: url === "https://fail.test.invalid/v1/version" ? 503 : 200 });
  });

  const pending = [];
  await worker.scheduled({ cron: PROBE_CRON, scheduledTime: NOW }, probeEnv(directory), {
    waitUntil(promise) { pending.push(promise); },
  });
  assert.equal(pending.length, 1, "the probe cron must not register maintenance");
  let completed = false;
  pending[0].then(() => { completed = true; });
  await flush();
  assert.deepEqual(calls.map(({ url }) => url).sort(), [
    CP_URL,
    "https://ok.test.invalid/v1/version",
    "https://fail.test.invalid/v1/version",
    "https://timeout.test.invalid/v1/version",
    "https://reject.test.invalid/v1/version",
  ].sort());
  assert.equal(completed, false);
  t.mock.timers.tick(9999);
  await flush();
  assert.equal(completed, false);
  t.mock.timers.tick(1);
  await pending[0];
  await Promise.allSettled(pending);
  assert.equal(timeoutAborted, true);
  assert.equal(canceledBodies, 3);

  const writes = directory.writes;
  assert.deepEqual(writes.map(({ key }) => key), [UPTIME_PROBE_RUN_KEY]);
  assert.equal(writes[0].options, undefined, "retained timestamps must not expire");
  assert.equal(writes[0].value.includes(PRIVATE_MARKER), false);
  assert.equal(writes[0].value.includes("https://"), false);
  const document = directory.document();
  assert.deepEqual(Object.keys(document).sort(), ["scheduler", "targets"]);
  assert.deepEqual(document.targets.map(({ target, state, http_status_class }) => [
    target, state, http_status_class,
  ]).sort(), [
    ["control_plane", "ok", 2], ["fail", "down", 5], ["ok", "ok", 2],
    ["reject", "down", 0], ["timeout", "down", 0],
  ].sort());
  for (const row of document.targets) {
    assert.deepEqual(Object.keys(row).sort(), [
      "checked_at", "http_status_class", "latency_ms", "state", "target",
    ]);
    assert.ok(Number.isFinite(row.latency_ms) && row.latency_ms >= 0 && row.latency_ms <= 10_000);
    assert.equal(new Date(row.checked_at).toISOString(), row.checked_at);
  }
  assert.equal(document.targets.find(({ target }) => target === "timeout").latency_ms, 10_000);
  assert.deepEqual(directory.scheduler(), {
    last_run_at: new Date(NOW).toISOString(), duration_ms: 10_000,
    target_count: 4, directory_ok: true,
  });
  assert.equal(writes[0].options, undefined, "scheduler timestamps must not expire");
});

test("all cell registry pages are probed without reading account records", async () => {
  const directory = new DirectoryKV({
    "cell:a": { endpoint: "https://a.test.invalid/", name: PRIVATE_MARKER },
    "cell:b": { endpoint: "https://b.test.invalid" },
    "acct:private": { account_id: PRIVATE_MARKER },
  });
  directory.list = async ({ prefix, cursor }) => {
    assert.equal(prefix, "cell:");
    directory.lists.push(cursor);
    return cursor === undefined
      ? { keys: [{ name: "cell:a" }], list_complete: false, cursor: "next" }
      : { keys: [{ name: "cell:b" }], list_complete: true };
  };
  const calls = [];
  await runScheduledUptimeProbes(probeEnv(directory), async (url) => {
    calls.push(url);
    return new Response(null, { status: 204 });
  });
  assert.deepEqual(directory.lists, [undefined, "next"]);
  assert.deepEqual(directory.reads.sort(), ["cell:a", "cell:b", UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY].sort());
  assert.deepEqual(calls.sort(), [CP_URL, "https://a.test.invalid/v1/version", "https://b.test.invalid/v1/version"].sort());
  assert.deepEqual(directory.document().targets.map(({ target }) => target).sort(), ["a", "b", "control_plane"]);
});

test("an empty directory still probes the control plane", async () => {
  const directory = new DirectoryKV();
  const calls = [];
  await runScheduledUptimeProbes(probeEnv(directory), async (url) => {
    calls.push(url);
    return new Response(null, { status: 200 });
  });
  assert.deepEqual(calls, [CP_URL]);
  assert.deepEqual(directory.document().targets.map(({ target }) => target), ["control_plane"]);
  assert.equal(directory.scheduler().target_count, 0, "the scheduler counts directory cells, excluding the optional CP probe");
});

test("a scheduler that never ran exposes zero-valued scheduler gauges", async () => {
  const directory = new DirectoryKV();
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assertMissingRunMetrics(body);
  assert.equal(directory.writes.length, 0);
});

test("an empty directory persists scheduler health and exposes it without target samples", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new DirectoryKV();
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  assert.deepEqual(directory.scheduler(), {
    last_run_at: new Date(NOW).toISOString(), duration_ms: 0,
    target_count: 0, directory_ok: true,
  });
  assert.deepEqual(directory.document().targets, []);
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 0, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 0, writeOK: true });
  assert.equal(body.includes("{target="), false);
});

test("directory failure without previous targets still persists and exposes scheduler health", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new DirectoryKV();
  directory.list = async () => { throw new Error(PRIVATE_MARKER); };
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  assert.deepEqual(directory.scheduler(), {
    last_run_at: new Date(NOW).toISOString(), duration_ms: 0,
    target_count: 0, directory_ok: false,
  });
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 0, directoryOK: false });
  assert.equal(body.includes("{target="), false);
  assert.equal(body.includes(PRIVATE_MARKER), false);
});

test("six unreachable targets cannot falsely fail a healthy seventh behind a connection queue", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const names = ["a", "b", "c", "d", "e", "f", "healthy"];
  const directory = new DirectoryKV(Object.fromEntries(names.map((name) =>
    [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }])));
  let active = 0;
  const queued = [];
  // Model Workers' six open connections. Aborted requests free slots on the
  // next microtask; a fetch queued with an already-running timer can expire
  // before a connection opens, even though its endpoint would answer 200.
  const drain = () => {
    while (active < 6 && queued.length) {
      const start = queued.shift();
      start();
    }
  };
  const fetchImpl = (url, { signal }) => new Promise((resolve, reject) => {
    let started = false;
    let finished = false;
    const finish = (error) => {
      if (finished) return;
      finished = true;
      if (started) active -= 1;
      if (error) reject(error);
      else resolve(new Response(null, { status: 200 }));
      void Promise.resolve().then(drain);
    };
    signal.addEventListener("abort", () => finish(new Error("aborted")), { once: true });
    queued.push(() => {
      if (finished || signal.aborted) return;
      started = true;
      active += 1;
      if (url === "https://healthy.test.invalid/v1/version") finish();
    });
    drain();
  });
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, fetchImpl);
  await flush();
  t.mock.timers.tick(10_000);
  await flush();
  t.mock.timers.tick(10_000);
  await pending;
  const healthy = directory.document().targets.find(({ target }) => target === "healthy");
  assert.ok(healthy, "every discovered target must receive a persisted result");
  assert.ok(["ok", "skipped"].includes(healthy.state), "a queued healthy target must never be recorded down");
  assert.equal(active, 0);
});

test("the probe pool keeps at most four fetches in flight and gives queued probes a fresh timeout", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const directory = new DirectoryKV(Object.fromEntries(
    Array.from({ length: 8 }, (_, i) => [`cell:c${i}`, { endpoint: `https://c${i}.test.invalid` }]),
  ));
  let active = 0;
  let peak = 0;
  const starts = [];
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, (_url, { signal }) => {
    active += 1;
    peak = Math.max(peak, active);
    starts.push(Date.now() - NOW);
    return new Promise((_, reject) => signal.addEventListener("abort", () => {
      active -= 1;
      reject(new Error("aborted"));
    }, { once: true }));
  });
  await flush();
  assert.equal(active, 4);
  assert.deepEqual(starts, [0, 0, 0, 0]);
  t.mock.timers.tick(10_000);
  await flush();
  assert.equal(active, 4);
  assert.deepEqual(starts, [0, 0, 0, 0, 10_000, 10_000, 10_000, 10_000]);
  t.mock.timers.tick(9999);
  await flush();
  assert.equal(active, 4, "queued targets must get their timeout after dispatch");
  t.mock.timers.tick(1);
  await pending;
  assert.equal(peak, 4);
  assert.equal(active, 0);
  assert.ok(directory.document().targets.every(({ state, latency_ms }) => state === "down" && latency_ms === 10_000));
});

test("the four-probe pool is isolated from three slow maintenance connections", async (t) => {
  for (const separateInvocations of [false, true]) {
    await t.test(separateInvocations ? "separate cron invocations" : "former shared invocation reproduces a false outage", async (subtest) => {
      subtest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
      subtest.mock.method(console, "log", () => {});
      const invocations = new AsyncLocalStorage();
      const newBudget = () => ({ active: 0, peak: 0, queued: [] });
      const maintenanceBudget = newBudget();
      // Reusing one budget for both dispatch branches is the control case
      // modeling their former execution inside a single scheduled invocation.
      const probeBudget = separateInvocations ? newBudget() : maintenanceBudget;
      const probeStarts = [];
      const maintenanceStarts = [];
      const releaseMaintenance = [];
      const directory = new DirectoryKV({
        ...Object.fromEntries(["a", "b", "c", "healthy"].map((name) =>
          [`cell:${name}`, { endpoint: `https://${name}.test.invalid`, provision_token: PRIVATE_MARKER }])),
        "config:reaper": { enabled: true, ttl_minutes: 1 },
        "pending:expired": { cell: "a", created_at: new Date(NOW - 120_000).toISOString() },
      });
      // Workers enforce six open connections per invocation, including
      // service/DO requests. Each request's timeout starts before any wait
      // for a free slot; this is the queue that used to produce false downs.
      const fetchImpl = (input, options = {}) => {
        const budget = invocations.getStore();
        assert.ok(budget, "every outbound request must belong to its scheduled invocation");
        const url = new URL(input instanceof Request ? input.url : input);
        const signal = input instanceof Request ? input.signal : options.signal;
        const probe = url.pathname === "/v1/version";
        const drain = () => {
          while (budget.active < 6 && budget.queued.length) budget.queued.shift()();
        };
        return new Promise((resolve, reject) => {
          let started = false;
          let finished = false;
          let timer;
          const finish = (error) => {
            if (finished) return;
            finished = true;
            clearTimeout(timer);
            signal?.removeEventListener("abort", abort);
            if (started) budget.active -= 1;
            if (error) reject(error);
            else resolve(new Response(null, { status: probe ? 200 : 503 }));
            void Promise.resolve().then(drain);
          };
          const abort = () => finish(new Error("aborted"));
          signal?.addEventListener("abort", abort, { once: true });
          budget.queued.push(() => {
            if (finished) return;
            if (signal?.aborted) return abort();
            started = true;
            budget.active += 1;
            budget.peak = Math.max(budget.peak, budget.active);
            if (probe) {
              probeStarts.push({ target: url.hostname.split(".")[0], at: Date.now() - NOW });
              timer = setTimeout(() => finish(), 6000);
            } else {
              maintenanceStarts.push(url.pathname);
              releaseMaintenance.push(() => finish());
            }
          });
          drain();
        });
      };
      subtest.mock.method(globalThis, "fetch", fetchImpl);
      const env = {
        DIRECTORY: directory,
        CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: "true",
        AGENT_EMAIL_DIRECTORY: {},
        REALM_EMAIL_ALIASES: {
          idFromName: (name) => name,
          get: () => ({ fetch: fetchImpl }),
        },
        CP_PLAN_LIFECYCLE_ENABLED: "true",
        INTERNAL_BRIDGE_TOKEN: PRIVATE_MARKER,
        CONTROL_PLANE: { fetch: fetchImpl },
      };
      const maintenancePending = [];
      const probePending = [];
      await invocations.run(maintenanceBudget, () => worker.scheduled(
        { cron: MAINTENANCE_CRON, scheduledTime: NOW }, env,
        { waitUntil(promise) { maintenancePending.push(promise); } },
      ));
      await flush();
      assert.equal(maintenanceBudget.active, 3);
      assert.deepEqual(maintenanceStarts.sort(), [
        "/canonical/inventory/reconcile", "/v1/accounts/expired:reap", "/v1/plan-lifecycle:tick",
      ]);
      assert.deepEqual(probeStarts, [], "the maintenance cron must not dispatch probes");
      // The same scheduledTime deliberately models overlapping invocations:
      // isolation comes from the invocation, not the one-minute cron offset.
      await invocations.run(probeBudget, () => worker.scheduled(
        { cron: PROBE_CRON, scheduledTime: NOW }, env,
        { waitUntil(promise) { probePending.push(promise); } },
      ));
      await flush();
      assert.deepEqual(probeStarts, (separateInvocations ? ["a", "b", "c", "healthy"] : ["a", "b", "c"])
        .map((target) => ({ target, at: 0 })));
      assert.equal(probePending.length, 1);
      subtest.mock.timers.tick(6000);
      await flush();
      if (separateInvocations) {
        await Promise.all(probePending);
        assert.equal(probeBudget.peak, 4, "keep two connection slots free for KV work");
        assert.equal(probeBudget.active, 0);
        assert.equal(maintenanceBudget.active, 3, "probes must finish while maintenance remains in flight");
      } else {
        assert.deepEqual(probeStarts.at(-1), { target: "healthy", at: 6000 });
      }
      subtest.mock.timers.tick(4000);
      await Promise.all(probePending);
      const healthy = directory.document().targets.find(({ target }) => target === "healthy");
      assert.equal(healthy.state, separateInvocations ? "ok" : "down");
      assert.equal(healthy.latency_ms, separateInvocations ? 6000 : 10_000);
      subtest.mock.timers.tick(1000);
      assert.equal(maintenanceBudget.active, 3, "hold all maintenance requests beyond the 10s probe deadline");
      for (const release of releaseMaintenance) release();
      await Promise.all(maintenancePending);
      assert.equal(maintenanceBudget.active, 0);
    });
  }
});

test("the shared budget records unstarted targets as skipped and omits their up sample", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const directory = new DirectoryKV(Object.fromEntries(
    Array.from({ length: 9 }, (_, i) => [`cell:c${i}`, { endpoint: `https://c${i}.test.invalid` }]),
  ));
  const calls = [];
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, (url) => {
    calls.push(url);
    return new Promise(() => {});
  });
  await flush();
  t.mock.timers.tick(10_000);
  await flush();
  t.mock.timers.tick(10_000);
  await pending;
  assert.equal(calls.length, 8);
  assert.ok(Date.now() - NOW <= 25_000);
  const skipped = directory.document().targets.find(({ target }) => target === "c8");
  assert.equal(skipped.state, "skipped");
  assert.equal(skipped.http_status_class, 0);
  assert.equal(skipped.latency_ms, 0);
  assert.equal(Object.hasOwn(skipped, "ok"), false);
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.ok(body.includes("# HELP witself_probe_skipped "));
  assert.ok(body.includes("# TYPE witself_probe_skipped gauge\n"));
  assert.ok(body.includes('witself_probe_skipped{target="c8"} 1\n'));
  assert.equal(body.includes('witself_probe_up{target="c8"}'), false);
  assert.equal(body.includes('witself_probe_skipped{target="c0"} 1'), false);
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 9, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 9, writeOK: true });
});

test("malformed and absent run documents persist skipped targets on the first tick and probe them on the second", async (t) => {
  const cases = {
    "unparseable JSON": "{invalid",
    "stored JSON null": "null",
    "invalid scheduler": JSON.stringify({ ...runDocument(), scheduler: { private: PRIVATE_MARKER } }),
    "invalid targets": JSON.stringify(runDocument([{ ...snapshot().targets[0], state: "unknown" }])),
    "absent": null,
  };
  for (const [name, raw] of Object.entries(cases)) {
    await t.test(name, async (subtest) => {
      subtest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
      const names = ["a", "b", "c", "d", "z"];
      const directory = new DirectoryKV(Object.fromEntries(names.map((name) => [
        `cell:${name}`, { endpoint: `https://${name}.test.invalid` },
      ])));
      if (raw !== null) {
        directory.values.set(UPTIME_PROBE_RUN_KEY, raw);
        // A present malformed run must not resurrect valid but stale legacy data.
        directory.values.set(UPTIME_PROBES_KEY, JSON.stringify(snapshot()));
        directory.values.set(UPTIME_PROBE_SCHEDULER_KEY, JSON.stringify(scheduler()));
      }
      const list = directory.list.bind(directory);
      directory.list = async (options) => {
        await new Promise((resolve) => setTimeout(resolve, 1));
        return list(options);
      };
      for (const tick of [0, 1, 2, 3]) {
        subtest.mock.timers.setTime(NOW + tick * 300_000);
        const calls = [];
        const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, (url, { signal }) => {
          calls.push(new URL(url).hostname.split(".")[0]);
          if (url === "https://z.test.invalid/v1/version") return new Response(null, { status: 200 });
          return new Promise((_, reject) => signal.addEventListener("abort", () => {
            reject(new Error("aborted"));
          }, { once: true }));
        });
        await flush();
        subtest.mock.timers.tick(1);
        await flush();
        subtest.mock.timers.tick(10_000);
        await pending;
        assert.equal(directory.writes.length, tick + 1, "every tick must persist, including the first skipped run");
        assert.ok(directory.writes.every(({ key }) => key === UPTIME_PROBE_RUN_KEY));
        const document = directory.document();
        const recovered = raw !== null && tick === 0;
        assert.equal(document.recovered_from_malformed, recovered ? true : undefined);
        assert.equal(JSON.stringify(document).includes(PRIVATE_MARKER), false);
        assert.equal(document.scheduler.duration_ms, 10_001);
        const z = document.targets.find(({ target }) => target === "z");
        const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
        assert.equal(response.status, 200);
        const body = await response.text();
        assert.ok(body.includes(`# TYPE witself_probe_document_was_malformed gauge\n`));
        assert.ok(body.includes(`witself_probe_document_was_malformed ${Number(recovered)}\n`));
        assertPersistenceMetrics(body, { targetCount: 5, writeOK: true });
        if (tick === 0) {
          assert.deepEqual(calls, ["a", "b", "c", "d"]);
          assert.equal(z.state, "skipped");
          assert.equal(Object.hasOwn(z, "last_observation"), false);
          assert.ok(body.includes('witself_probe_skipped{target="z"} 1\n'));
          assert.equal(body.includes('witself_probe_up{target="z"}'), false);
        } else {
          assert.equal(calls[0], "z", "persisted skipped-first ordering must dispatch z by the second tick");
          assert.equal(z.state, "ok");
          assert.ok(body.includes('witself_probe_up{target="z"} 1\n'));
        }
      }
      if (raw !== null) {
        assert.equal(directory.reads.includes(UPTIME_PROBES_KEY), false);
        assert.equal(directory.reads.includes(UPTIME_PROBE_SCHEDULER_KEY), false);
      }
    });
  }
});

test("recovered targets get dispatched across cron ticks while earlier cells keep timing out", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const names = ["a", "b", "c", "d", "z"];
  const observation = { state: "down", http_status_class: 5, latency_ms: 1250,
    checked_at: new Date(NOW - 3_600_000).toISOString() };
  const directory = new DirectoryKV({
    ...Object.fromEntries(names.map((name) => [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }])),
    [UPTIME_PROBE_RUN_KEY]: runDocument(names.map((target) => ({ target, ...observation })), scheduler(names.length)),
  });
  const list = directory.list.bind(directory);
  directory.list = async (options) => {
    await new Promise((resolve) => setTimeout(resolve, 1));
    return list(options);
  };
  const callsByRun = [];
  const upByRun = [];
  for (const minute of [0, 5, 10, 15]) {
    t.mock.timers.setTime(NOW + minute * 60_000);
    const calls = [];
    const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, (url, { signal }) => {
      calls.push(new URL(url).hostname.split(".")[0]);
      if (url === "https://z.test.invalid/v1/version") return new Response(null, { status: 200 });
      return new Promise((_, reject) => signal.addEventListener("abort", () => {
        reject(new Error("aborted"));
      }, { once: true }));
    });
    await flush();
    t.mock.timers.tick(1);
    await flush();
    t.mock.timers.tick(10_000);
    await pending;
    callsByRun.push(calls);
    const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
    const body = await response.text();
    upByRun.push(body.includes('witself_probe_up{target="z"} 1\n'));
    const row = directory.document().targets.find(({ target }) => target === "z");
    if (minute === 0) {
      assert.equal(row.state, "skipped");
      assert.deepEqual(row.last_observation, observation);
      assert.ok(body.includes('witself_probe_up{target="z"} 0\n'));
    } else if (calls.includes("z")) {
      assert.equal(row.state, "ok");
      assert.equal(Object.hasOwn(row, "last_observation"), false);
      assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z"} ${(NOW + minute * 60_000 + 1) / 1000}\n`));
    }
  }
  assert.deepEqual(callsByRun[0], ["a", "b", "c", "d"]);
  assert.ok(callsByRun.slice(1).some((calls) => calls.includes("z")),
    "a skipped recovering target must eventually be dispatched while other cells keep timing out");
  assert.deepEqual(upByRun, [false, true, true, true], "recovery must replace the stale outage metric");
});

test("repeated skipped runs preserve a prior outage and its last completed observation through discovery failure", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const names = ["a", "b", "c", "d", "e", "f", "g", "h", "z"];
  const observation = { state: "down", http_status_class: 5, latency_ms: 1250,
    checked_at: new Date(NOW - 3_600_000).toISOString() };
  const directory = new DirectoryKV({
    ...Object.fromEntries(names.map((name) => [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }])),
    [UPTIME_PROBES_KEY]: { schema_version: 1, targets: names.map((target) => ({ target, ...observation })) },
  });
  const list = directory.list.bind(directory);
  directory.list = async (options) => {
    const page = await list(options);
    // Model elapsed discovery/processing leaving less than one full timeout.
    // Repeated skips must be forced by budget, independently of probe ordering.
    t.mock.timers.setTime(Date.now() + 10_001);
    return page;
  };
  let skipped;
  for (const minute of [0, 5, 10]) {
    t.mock.timers.setTime(NOW + minute * 60_000);
    let calls = 0;
    await runScheduledUptimeProbes({ DIRECTORY: directory }, () => {
      calls += 1;
      return new Response(null, { status: 200 });
    });
    assert.equal(calls, 0, "insufficient budget must prevent dispatch");
    skipped = directory.document().targets.find(({ target }) => target === "z");
    assert.equal(skipped.state, "skipped");
    const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
    assert.equal(response.status, 200);
    const body = await response.text();
    assert.ok(body.includes('witself_probe_up{target="z"} 0\n'), "skipping must not resolve an existing outage");
    assert.ok(body.includes('witself_probe_skipped{target="z"} 1\n'));
    assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z"} ${Date.parse(observation.checked_at) / 1000}\n`));
    assert.ok(body.includes('witself_probe_latency_seconds{target="z"} 1.25\n'));
    assert.ok(body.includes('witself_probe_http_status{target="z"} 5\n'));
    assert.deepEqual(skipped.last_observation, observation);
  }
  directory.list = async () => { throw new Error(PRIVATE_MARKER); };
  t.mock.timers.setTime(NOW + 15 * 60_000);
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  assert.deepEqual(directory.document().targets.find(({ target }) => target === "z"), skipped);
  let response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  let body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="z"} 0\n'));
  assert.ok(body.includes('witself_probe_skipped{target="z"} 1\n'));
  assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z"} ${Date.parse(observation.checked_at) / 1000}\n`));

  directory.list = list;
  t.mock.timers.setTime(NOW + 20 * 60_000);
  await runScheduledUptimeProbes({ DIRECTORY: directory }, async () => new Response(null, { status: 200 }));
  const recovered = directory.document().targets.find(({ target }) => target === "z");
  assert.equal(recovered.state, "ok");
  assert.equal(Object.hasOwn(recovered, "last_observation"), false);
  response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="z"} 1\n'));
  assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z"} ${Date.now() / 1000}\n`));
  assert.equal(body.includes('witself_probe_skipped{target="z"}'), false);
});

test("a healthy fifth target is skipped when slow discovery leaves less than its full timeout", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const directory = new DirectoryKV(Object.fromEntries(
    ["a", "b", "c", "d", "healthy"].map((name) => [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }]),
  ));
  const list = directory.list.bind(directory);
  directory.list = async (options) => {
    await new Promise((resolve) => setTimeout(resolve, 9500));
    return list(options);
  };
  const calls = [];
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, (url, { signal }) => {
    calls.push(url);
    return new Promise((resolve, reject) => {
      const timer = url === "https://healthy.test.invalid/v1/version"
        ? setTimeout(() => resolve(new Response(null, { status: 200 })), 1000) : undefined;
      signal.addEventListener("abort", () => {
        clearTimeout(timer);
        reject(new Error("aborted"));
      }, { once: true });
    });
  });
  await flush();
  t.mock.timers.tick(9500);
  await flush();
  assert.equal(calls.length, 4);
  t.mock.timers.tick(10_000);
  await flush();
  t.mock.timers.tick(500);
  await pending;
  const healthy = directory.document().targets.find(({ target }) => target === "healthy");
  assert.equal(healthy.state, "skipped", "a healthy endpoint must not fail a shortened 500ms probe");
  assert.equal(calls.length, 4, "a request needs the full 10s timeout available before dispatch");
  assert.equal(healthy.latency_ms, 0);
  assert.equal(Object.hasOwn(healthy, "last_observation"), false);
  assert.ok(Date.now() - NOW <= 25_000);
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.ok(body.includes('witself_probe_skipped{target="healthy"} 1\n'));
  assert.equal(body.includes('witself_probe_up{target="healthy"}'), false);
});

test("an unreadable prior snapshot preserves existing outage evidence when discovered targets are skipped", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const names = ["a", "b", "c", "d", "e", "f", "g", "h", "z"];
  const checkedAt = new Date(NOW - 3_600_000).toISOString();
  const previous = { schema_version: 1, targets: names.map((target) => ({
    target, state: "down", http_status_class: 5, latency_ms: 1250, checked_at: checkedAt,
  })) };
  const directory = new DirectoryKV({
    ...Object.fromEntries(names.map((name) => [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }])),
    [UPTIME_PROBES_KEY]: previous,
  });
  const get = directory.get.bind(directory);
  directory.get = async (key, options) => {
    if (key === UPTIME_PROBES_KEY) throw new Error(PRIVATE_MARKER);
    return get(key, options);
  };
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, () => new Promise(() => {}));
  await flush();
  t.mock.timers.tick(10_000);
  await flush();
  t.mock.timers.tick(10_000);
  await pending;
  directory.get = get;
  assert.deepEqual(JSON.parse(directory.values.get(UPTIME_PROBES_KEY)), previous, "an unreadable prior observation must not be erased by a skipped result");
  assert.deepEqual(directory.writes, [], "the heartbeat cannot advance without its retained observations");
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="z"} 0\n'));
  assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z"} ${Date.parse(checkedAt) / 1000}\n`));
  assertSchedulerMetrics(body, { timestamp: 0, targetCount: 0, directoryOK: false });
  assert.equal(body.includes(PRIVATE_MARKER), false);
});

test("the control plane and previously healthy targets start before deterministic remaining targets", async () => {
  const previous = snapshot();
  previous.targets[0] = { ...previous.targets[0], target: "z-healthy", state: "ok", http_status_class: 2 };
  const directory = new DirectoryKV({
    "cell:c": { endpoint: "https://c.test.invalid" },
    "cell:z-healthy": { endpoint: "https://z-healthy.test.invalid" },
    "cell:b": { endpoint: "https://b.test.invalid" },
    "cell:a": { endpoint: "https://a.test.invalid" },
    [UPTIME_PROBES_KEY]: previous,
  });
  const calls = [];
  await runScheduledUptimeProbes(probeEnv(directory), async (url) => {
    calls.push(url);
    return new Response(null, { status: 200 });
  });
  assert.deepEqual(calls, [CP_URL, "https://z-healthy.test.invalid/v1/version",
    "https://a.test.invalid/v1/version", "https://b.test.invalid/v1/version", "https://c.test.invalid/v1/version"]);
});

test("a skipped target retains its prior healthy metric and probe priority", async () => {
  const observation = { state: "ok", http_status_class: 2, latency_ms: 25,
    checked_at: new Date(NOW - 60_000).toISOString() };
  const directory = new DirectoryKV({
    "cell:a": { endpoint: "https://a.test.invalid" },
    "cell:z-healthy": { endpoint: "https://z-healthy.test.invalid" },
    [UPTIME_PROBES_KEY]: { schema_version: 1, targets: [{
      target: "z-healthy", state: "skipped", http_status_class: 0, latency_ms: 0,
      checked_at: new Date(NOW).toISOString(), last_observation: observation,
    }] },
  });
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="z-healthy"} 1\n'));
  assert.ok(body.includes('witself_probe_skipped{target="z-healthy"} 1\n'));
  assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="z-healthy"} ${Date.parse(observation.checked_at) / 1000}\n`));
  const calls = [];
  await runScheduledUptimeProbes(probeEnv(directory), async (url) => {
    calls.push(url);
    return new Response(null, { status: 200 });
  });
  assert.deepEqual(calls, [CP_URL, "https://z-healthy.test.invalid/v1/version", "https://a.test.invalid/v1/version"]);
  const completed = directory.document().targets.find(({ target }) => target === "z-healthy");
  assert.equal(completed.state, "ok");
  assert.equal(Object.hasOwn(completed, "last_observation"), false);
});

test("skipping projects legacy observations without carrying private metadata", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const names = ["a", "b", "c", "d", "e", "f", "g", "h", "z"];
  const observation = { state: "down", http_status_class: 5, latency_ms: 1250,
    checked_at: new Date(NOW - 60_000).toISOString() };
  const directory = new DirectoryKV({
    ...Object.fromEntries(names.map((name) => [`cell:${name}`, { endpoint: `https://${name}.test.invalid` }])),
    [UPTIME_PROBES_KEY]: { schema_version: 1, targets: names.map((target) => ({
      target, ok: false, http_status_class: observation.http_status_class,
      latency_ms: observation.latency_ms, checked_at: observation.checked_at,
      token: PRIVATE_MARKER, private_metadata: { value: PRIVATE_MARKER },
    })) },
  });
  const pending = runScheduledUptimeProbes({ DIRECTORY: directory }, () => new Promise(() => {}));
  await flush();
  t.mock.timers.tick(10_000);
  await flush();
  t.mock.timers.tick(10_000);
  await pending;
  const skipped = directory.document().targets.find(({ target }) => target === "z");
  assert.deepEqual(skipped.last_observation, observation);
  assert.equal(JSON.stringify(directory.document()).includes(PRIVATE_MARKER), false);
  // Also re-project an already-retained observation when directory discovery
  // fails; unknown stored fields must not acquire a new durable lifetime.
  const stored = directory.document();
  stored.targets.find(({ target }) => target === "z").last_observation.token = PRIVATE_MARKER;
  directory.values.set(UPTIME_PROBE_RUN_KEY, JSON.stringify(stored));
  directory.list = async () => { throw new Error(PRIVATE_MARKER); };
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  assert.deepEqual(directory.document().targets.find(({ target }) => target === "z").last_observation, observation);
  assert.equal(JSON.stringify(directory.document()).includes(PRIVATE_MARKER), false);
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="z"} 0\n'));
  assert.equal(body.includes(PRIVATE_MARKER), false);
});

test("alternating discovery failures preserve a continuous cell outage and its timestamps", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const directory = new DirectoryKV({
    "cell:broken": { endpoint: "https://broken.test.invalid" },
  });
  const env = probeEnv(directory);
  const list = directory.list.bind(directory);
  let discoveryFails = false;
  directory.list = async (options) => {
    if (discoveryFails) throw new Error(PRIVATE_MARKER);
    return list(options);
  };
  t.mock.method(globalThis, "fetch", async (url) =>
    new Response(null, { status: url === CP_URL ? 200 : 503 }));
  let lastCellCheck;
  // At 10m this continuously failed series can fire; failure ticks after that
  // must not resolve it. At 35m onward discovery fails long enough to go stale.
  for (let minute = 0; minute <= 55; minute += 5) {
    t.mock.timers.setTime(NOW + minute * 60_000);
    discoveryFails = minute % 10 === 5 || minute >= 35;
    if (minute === 40) directory.values.delete("cell:broken");
    const pending = [];
    await worker.scheduled({ cron: PROBE_CRON, scheduledTime: Date.now() }, env, {
      waitUntil(promise) { pending.push(promise); },
    });
    await Promise.all(pending);
    const row = directory.document().targets.find(({ target }) => target === "broken");
    assert.ok(row, `cell series disappeared at ${minute}m`);
    assert.equal(row.state, "down");
    assert.equal(row.http_status_class, 5);
    if (discoveryFails) assert.deepEqual(row, lastCellCheck);
    else lastCellCheck = row;
    const response = await worker.fetch(metricsRequest(), env, {});
    assert.equal(response.status, 200);
    const body = await response.text();
    assert.ok(body.includes('witself_probe_up{target="broken"} 0\n'));
    assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="broken"} ${Date.parse(lastCellCheck.checked_at) / 1000}\n`));
    assert.ok(body.includes(`witself_probe_directory_ok ${Number(!discoveryFails)}\n`));
    assert.ok(body.includes("witself_probe_scheduler_target_count 1\n"));
    assert.equal(body.includes('target="directory"'), false);
    if (minute >= 50) assert.ok(Date.now() - Date.parse(row.checked_at) > 900_000);
  }
  // Absence becomes authoritative only after a complete successful discovery.
  discoveryFails = false;
  await runScheduledUptimeProbes(env);
  assert.deepEqual(directory.document().targets.map(({ target }) => target), ["control_plane"]);
});

test("failed discovery never overwrites an unreadable previous snapshot", async (t) => {
  for (const failure of ["throw", "timeout"]) {
    await t.test(failure, async (subtest) => {
      subtest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
      const previous = snapshot();
      const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: previous });
      directory.list = async () => { throw new Error(PRIVATE_MARKER); };
      directory.get = async () => {
        if (failure === "throw") throw new Error(PRIVATE_MARKER);
        return new Promise(() => {});
      };
      const pending = runScheduledUptimeProbes(probeEnv(directory), async () =>
        new Response(null, { status: 200 }));
      await flush();
      subtest.mock.timers.tick(5000);
      await pending;
      assert.deepEqual(directory.writes, [], "an unreadable prior snapshot must also preserve its scheduler timestamp");
      assert.deepEqual(JSON.parse(directory.values.get(UPTIME_PROBES_KEY)), previous);
      assert.ok(Date.now() - NOW <= 25_000);
    });
  }
});

test("failed discovery replaces malformed stored data with a value-free recovery flag", async (t) => {
  for (const key of [UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY]) {
    await t.test(key, async () => {
      const directory = new DirectoryKV({ [key]: { private: PRIVATE_MARKER } });
      directory.list = async () => { throw new Error(PRIVATE_MARKER); };
      await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
      assert.deepEqual(directory.writes.map(({ key }) => key), [UPTIME_PROBE_RUN_KEY]);
      const document = directory.document();
      assert.equal(document.recovered_from_malformed, true);
      assert.equal(document.scheduler.directory_ok, false);
      assert.equal(document.scheduler.target_count, 0);
      assert.deepEqual(document.targets, []);
      assert.equal(JSON.stringify(document).includes(PRIVATE_MARKER), false);
      const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
      assert.equal(response.status, 200);
      const body = await response.text();
      assert.ok(body.includes("witself_probe_document_was_malformed 1\n"));
      assertPersistenceMetrics(body, { targetCount: 0, writeOK: true });
      assert.equal(body.includes(PRIVATE_MARKER), false);
    });
  }
});

test("discovery failure preserves a directory-named cell's check time without duplicate labels", async () => {
  for (const state of ["down", "ok"]) {
    const previous = snapshot();
    previous.targets[0] = { ...previous.targets[0], target: "directory", state,
      http_status_class: state === "ok" ? 2 : 5 };
    const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: previous });
    directory.list = async () => { throw new Error(PRIVATE_MARKER); };
    await runScheduledUptimeProbes(probeEnv(directory), async () =>
      new Response(null, { status: 200 }));
    const rows = directory.document().targets.filter(({ target }) => target === "directory");
    assert.deepEqual(rows, [previous.targets[0]]);
    const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
    assert.equal(response.status, 200);
    const body = await response.text();
    assert.ok(body.includes(`witself_probe_up{target="directory"} ${Number(state === "ok")}\n`));
    assert.ok(body.includes("witself_probe_directory_ok 0\n"));
  }
});

test("scheduled CP probes reach the container only behind the exact opt-in gate", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  let env;
  let cellFetches;
  t.mock.method(globalThis, "fetch", async (url, options) => {
    // Drive the real CP fetch route, including its container dispatch. This
    // reproduces the idle-timer renewal path that must stay dark by default.
    if (url === CP_URL) return worker.fetch(new Request(url, options), env, {});
    assert.equal(url, "https://cell-a.test.invalid/v1/version");
    cellFetches += 1;
    return new Response(null, { status: 200 });
  });
  for (const value of [undefined, "false", false, true, "TRUE", "1", "true"]) {
    resetContainerCalls();
    cellFetches = 0;
    const directory = new DirectoryKV({
      "cell:cell-a": { endpoint: "https://cell-a.test.invalid" },
      [UPTIME_PROBES_KEY]: snapshot(),
    });
    env = {
      DIRECTORY: directory,
      CONTROL_PLANE: { fetch: async () => new Response(null, { status: 200 }) },
    };
    if (value !== undefined) env.CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED = value;
    for (let minute = 0; minute <= 30; minute += 5) {
      t.mock.timers.setTime(NOW + minute * 60_000);
      const pending = [];
      await worker.scheduled({ cron: PROBE_CRON, scheduledTime: Date.now() }, env, {
        waitUntil(promise) { pending.push(promise); },
      });
      await Promise.all(pending);
      const response = await worker.fetch(metricsRequest(), env, {});
      assert.equal(response.status, 200);
      assert.equal((await response.text()).includes('target="control_plane"'), value === "true");
    }
    assert.equal(cellFetches, 7, "the CP gate must leave cell probing active");
    assert.deepEqual(containerCalls, value === "true" ? Array(7).fill("GET /v1/version") : []);
  }
  // An empty directory still publishes scheduler health with the gate absent.
  const directory = new DirectoryKV();
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  assert.equal((await response.text()).includes("{target="), false);
});

test("directory failures publish global directory health and retain the control plane result", async (t) => {
  const cases = {
    "list throws": (kv) => { kv.list = async () => { throw new Error(PRIVATE_MARKER); }; },
    "get throws": (kv) => { kv.get = async (key) => {
      if ([UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY].includes(key)) return null;
      throw new Error(PRIVATE_MARKER);
    }; },
    "record disappeared": (kv) => { kv.get = async () => null; },
    "malformed list": (kv) => { kv.list = async () => ({ keys: null, list_complete: true }); },
    "unsafe cell label": (kv) => { kv.list = async () => ({ keys: [{ name: `cell:${PRIVATE_MARKER}\"` }], list_complete: true }); },
    "repeated cursor": (kv) => { kv.list = async () => ({ keys: [], list_complete: false, cursor: "same" }); },
  };
  for (const [name, configure] of Object.entries(cases)) {
    await t.test(name, async () => {
      const directory = new DirectoryKV({ "cell:a": { endpoint: "https://a.test.invalid" } });
      configure(directory);
      await assert.doesNotReject(runScheduledUptimeProbes(probeEnv(directory), async () => new Response(null, { status: 200 })));
      const document = directory.document();
      assert.deepEqual(document.targets.map(({ target, state, http_status_class }) => [target, state, http_status_class]).sort(), [
        ["control_plane", "ok", 2],
      ]);
      assert.equal(directory.scheduler().directory_ok, false);
      assert.equal(directory.scheduler().target_count, 0);
      assert.equal(JSON.stringify(document).includes(PRIVATE_MARKER), false);
    });
  }
});

test("unsafe endpoints fail their cell without forwarding URL secrets or following redirects", async () => {
  const directory = new DirectoryKV({
    "cell:credential": { endpoint: `https://user:${PRIVATE_MARKER}@private.test.invalid` },
    "cell:query": { endpoint: `https://private.test.invalid?token=${PRIVATE_MARKER}` },
    "cell:fragment": { endpoint: `https://private.test.invalid#${PRIVATE_MARKER}` },
    "cell:insecure": { endpoint: "http://private.test.invalid" },
    "cell:missing": {},
    "cell:redirect": { endpoint: "https://redirect.test.invalid" },
  });
  const calls = [];
  await runScheduledUptimeProbes(probeEnv(directory), async (url, options) => {
    calls.push(url);
    assert.equal(options.redirect, "manual");
    return new Response(null, { status: url === CP_URL ? 200 : 302,
      headers: { Location: `https://private.test.invalid/${PRIVATE_MARKER}` } });
  });
  assert.deepEqual(calls.sort(), [CP_URL, "https://redirect.test.invalid/v1/version"].sort());
  const document = directory.document();
  assert.equal(document.targets.length, 7);
  for (const row of document.targets.filter(({ target }) => target !== "control_plane")) {
    assert.equal(row.state, "down");
    assert.equal(row.http_status_class, row.target === "redirect" ? 3 : 0);
  }
  assert.equal(JSON.stringify(document).includes(PRIVATE_MARKER), false);
});

test("stalled KV reads and noncooperative fetches cannot exceed the scheduled budget", async (t) => {
  for (const phase of ["list", "get"]) {
    await t.test(phase, async (timeoutTest) => {
      timeoutTest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
      const directory = new DirectoryKV({ "cell:a": { endpoint: "https://a.test.invalid" } });
      directory[phase] = async (key) => [UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY].includes(key)
        ? null : new Promise(() => {});
      const pending = runScheduledUptimeProbes(probeEnv(directory), async () => new Promise(() => {}));
      await flush();
      timeoutTest.mock.timers.tick(10_000);
      await flush();
      timeoutTest.mock.timers.tick(10_000);
      await pending;
      assert.ok(Date.now() - NOW <= 25_000);
      assert.deepEqual(directory.document().targets.map(({ target, state }) => [target, state]).sort(), [
        ["control_plane", "down"],
      ]);
      assert.equal(directory.scheduler().directory_ok, false);
    });
  }
  await t.test("directory, fetch and write share the 25s budget", async (timeoutTest) => {
    timeoutTest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
    const directory = new DirectoryKV({ "cell:a": { endpoint: "https://a.test.invalid" } });
    directory.list = async () => {
      await new Promise((resolve) => setTimeout(resolve, 9500));
      return { keys: [{ name: "cell:a" }], list_complete: true };
    };
    directory.put = async () => new Promise(() => {});
    let completed = false;
    const pending = runScheduledUptimeProbes(probeEnv(directory), async () => new Promise(() => {}));
    pending.then(() => { completed = true; });
    await flush();
    timeoutTest.mock.timers.tick(9500);
    await flush();
    timeoutTest.mock.timers.tick(10_000);
    await flush();
    timeoutTest.mock.timers.tick(4999);
    await flush();
    assert.equal(completed, false);
    timeoutTest.mock.timers.tick(1);
    await pending;
    assert.ok(Date.now() - NOW <= 25_000);
  });
});

test("missing KV and failed writes never throw or replace the previous snapshot", async () => {
  const fetchImpl = async () => new Response(null, { status: 200 });
  await assert.doesNotReject(runScheduledUptimeProbes({}, fetchImpl));
  const previous = snapshot();
  const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: previous });
  directory.put = async () => { throw new Error(PRIVATE_MARKER); };
  await assert.doesNotReject(runScheduledUptimeProbes(probeEnv(directory), fetchImpl));
  assert.deepEqual(JSON.parse(directory.values.get(UPTIME_PROBES_KEY)), previous);
  assert.equal(directory.values.has(UPTIME_PROBE_RUN_KEY), false);
});

test("public metrics bypass account/auth paths, preserve stale timestamps and keep security headers", async () => {
  const document = snapshot();
  document.private_metadata = PRIVATE_MARKER;
  document.targets[0].token = PRIVATE_MARKER;
  const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: document });
  const env = new Proxy({ DIRECTORY: directory }, {
    get(target, key) {
      assert.equal(key, "DIRECTORY", "metrics must not access auth, account, limiter or container bindings");
      return target[key];
    },
  });
  const response = await worker.fetch(metricsRequest(), env, { waitUntil() {
    assert.fail("metrics must not schedule account or authorization work");
  } });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal(response.headers.get("Referrer-Policy"), "no-referrer");
  assert.deepEqual(directory.reads.sort(), [UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY].sort());
  assert.deepEqual(directory.lists, []);
  assert.deepEqual(directory.writes, []);
  const body = await response.text();
  assert.equal(body.includes(PRIVATE_MARKER), false);
  assert.ok(body.endsWith("\n"));
  assert.deepEqual(body.trim().split("\n").filter((line) => !line.startsWith("#")).sort(), [
    "witself_probe_scheduler_last_run_timestamp_seconds 0",
    "witself_probe_scheduler_target_count 0",
    "witself_probe_directory_ok 0",
    "witself_probe_snapshot_target_count 2",
    "witself_probe_last_write_ok 0",
    "witself_probe_document_was_malformed 0",
    'witself_probe_up{target="cell-a"} 0',
    'witself_probe_up{target="control_plane"} 1',
    'witself_probe_latency_seconds{target="cell-a"} 1.25',
    'witself_probe_latency_seconds{target="control_plane"} 0.025',
    `witself_probe_last_check_timestamp_seconds{target="cell-a"} ${Date.parse(document.targets[0].checked_at) / 1000}`,
    `witself_probe_last_check_timestamp_seconds{target="control_plane"} ${Date.parse(document.targets[1].checked_at) / 1000}`,
    'witself_probe_http_status{target="cell-a"} 5',
    'witself_probe_http_status{target="control_plane"} 2',
  ].sort());
  for (const name of ["up", "skipped", "latency_seconds", "last_check_timestamp_seconds", "http_status",
    "scheduler_last_run_timestamp_seconds", "scheduler_target_count", "directory_ok",
    "snapshot_target_count", "last_write_ok", "document_was_malformed"]) {
    assert.ok(body.includes(`# HELP witself_probe_${name} `));
    assert.ok(body.includes(`# TYPE witself_probe_${name} gauge\n`));
  }
});

test("legacy boolean snapshots stay readable and normalize without changing retained cell checks", async () => {
  const previous = snapshot();
  previous.targets = previous.targets.map(({ state, ...row }) => ({ ...row, ok: state === "ok" }));
  const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: previous });
  let response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  let body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="cell-a"} 0\n'));
  assert.ok(body.includes('witself_probe_up{target="control_plane"} 1\n'));
  directory.list = async () => { throw new Error(PRIVATE_MARKER); };
  await runScheduledUptimeProbes({ DIRECTORY: directory }, () => assert.fail("unexpected probe"));
  assert.deepEqual(directory.document().targets, [snapshot().targets[0]]);
  response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  body = await response.text();
  assert.ok(body.includes('witself_probe_up{target="cell-a"} 0\n'));
  assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="cell-a"} ${Date.parse(previous.targets[0].checked_at) / 1000}\n`));
});

test("malformed legacy scheduler records expose missing-run gauges without exposing stored values", async (t) => {
  const valid = { last_run_at: new Date(NOW).toISOString(), duration_ms: 10,
    target_count: 1, directory_ok: true };
  for (const [name, record] of Object.entries({
    timestamp: { ...valid, last_run_at: PRIVATE_MARKER },
    duration: { ...valid, duration_ms: -1 },
    count: { ...valid, target_count: -1 },
    directory: { ...valid, directory_ok: PRIVATE_MARKER },
  })) {
    await t.test(name, async () => {
      const directory = new DirectoryKV({
        [UPTIME_PROBES_KEY]: snapshot(), [UPTIME_PROBE_SCHEDULER_KEY]: record,
      });
      const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
      assert.equal(response.status, 200);
      assertMissingRunMetrics(await response.text());
    });
  }
});

test("malformed retained observations expose missing-run gauges without exposing stored values", async (t) => {
  const valid = { state: "down", http_status_class: 5, latency_ms: 1250,
    checked_at: new Date(NOW - 60_000).toISOString() };
  for (const [name, observation] of Object.entries({
    "not an observation": PRIVATE_MARKER,
    "skipped observation": { ...valid, state: "skipped", http_status_class: 0, latency_ms: 0 },
    "inconsistent status": { ...valid, http_status_class: 2 },
    "negative latency": { ...valid, latency_ms: -1 },
    "invalid timestamp": { ...valid, checked_at: PRIVATE_MARKER },
  })) {
    await t.test(name, async () => {
      const directory = new DirectoryKV({ [UPTIME_PROBES_KEY]: {
        schema_version: 1, targets: [{ target: "z", state: "skipped", http_status_class: 0,
          latency_ms: 0, checked_at: new Date(NOW).toISOString(), last_observation: observation }],
      } });
      const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
      assert.equal(response.status, 200);
      assertMissingRunMetrics(await response.text());
    });
  }
});

test("malformed snapshots expose missing-run gauges while read errors fail the scrape without leaking values", async (t) => {
  const cases = {
    "wrong schema": { ...snapshot(), schema_version: 2 },
    "unsafe label": { schema_version: 1, targets: [{ ...snapshot().targets[1], target: `${PRIVATE_MARKER}\"` }] },
    "duplicate label": { schema_version: 1, targets: [snapshot().targets[1], snapshot().targets[1]] },
    "invalid status": { schema_version: 1, targets: [{ ...snapshot().targets[1], http_status_class: 200 }] },
    "inconsistent up": { schema_version: 1, targets: [{ ...snapshot().targets[1], state: "down" }] },
    "invalid state": { schema_version: 1, targets: [{ ...snapshot().targets[1], state: "unknown" }] },
    "inconsistent skipped": { schema_version: 1, targets: [{ ...snapshot().targets[1], state: "skipped" }] },
    "negative latency": { schema_version: 1, targets: [{ ...snapshot().targets[1], latency_ms: -1 }] },
    "invalid timestamp": { schema_version: 1, targets: [{ ...snapshot().targets[1], checked_at: PRIVATE_MARKER }] },
  };
  for (const [name, value] of Object.entries(cases)) {
    await t.test(name, async () => {
      const response = await uptimeProbeMetricsResponse(metricsRequest(), {
        DIRECTORY: { async get(key) { return key === UPTIME_PROBES_KEY ? JSON.stringify(value) : null; } },
      });
      assert.equal(response.status, 200);
      assert.equal(response.headers.get("Cache-Control"), "no-store");
      assertMissingRunMetrics(await response.text());
    });
  }
  const response = await worker.fetch(metricsRequest(), {
    DIRECTORY: { async get() { throw new Error(PRIVATE_MARKER); } },
  }, {});
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal((await response.text()).includes(PRIVATE_MARKER), false);
});

test("non-GET metrics requests are rejected before reading KV", async () => {
  for (const method of ["POST", "HEAD", "OPTIONS"]) {
    const response = await worker.fetch(metricsRequest(method), new Proxy({}, {
      get() { assert.fail("method rejection must not access bindings"); },
    }), {});
    assert.equal(response.status, 405);
    assert.equal(response.headers.get("Allow"), "GET");
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  }
});

test("thirteen rejected run-document writes cannot refresh the heartbeat without outage results", async (t) => {
  for (const layout of ["empty run document", "empty legacy pair", "never written"]) {
    await t.test(layout, async (subtest) => {
      subtest.mock.timers.enable({ apis: ["Date"], now: NOW });
      const previousScheduler = { ...scheduler(layout === "empty legacy pair" ? 1 : 0),
        last_run_at: new Date(NOW - 300_000).toISOString() };
      const initial = layout === "empty run document"
        ? { [UPTIME_PROBE_RUN_KEY]: runDocument([], previousScheduler) }
        : layout === "empty legacy pair"
          ? { [UPTIME_PROBES_KEY]: { schema_version: 1, targets: [] },
            [UPTIME_PROBE_SCHEDULER_KEY]: previousScheduler }
          : {};
      const directory = new DirectoryKV({ ...initial,
        "cell:broken": { endpoint: "https://broken.test.invalid" } });
      const initialValues = new Map(directory.values);
      const put = directory.put.bind(directory);
      const attempts = [];
      let rejectWrites = true;
      directory.put = async (key, value, options) => {
        attempts.push(key);
        // Reproduce the review: only result-document writes fail; the former
        // separate heartbeat key would still accept every attempted write.
        if (rejectWrites && key === UPTIME_PROBE_RUN_KEY) throw new Error(PRIVATE_MARKER);
        return put(key, value, options);
      };
      let probes = 0;
      const fetchImpl = async (url) => {
        assert.equal(url, "https://broken.test.invalid/v1/version");
        probes += 1;
        return new Response(null, { status: 503 });
      };
      const timestamp = layout === "never written" ? 0 : Date.parse(previousScheduler.last_run_at) / 1000;
      for (let tick = 0; tick < 13; tick += 1) {
        subtest.mock.timers.setTime(NOW + tick * 300_000);
        await assert.doesNotReject(runScheduledUptimeProbes({ DIRECTORY: directory }, fetchImpl));
        const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
        assert.equal(response.status, 200);
        const body = await response.text();
        assertSchedulerMetrics(body, { timestamp,
          targetCount: layout === "never written" ? 0 : previousScheduler.target_count,
          directoryOK: layout !== "never written" });
        assertPersistenceMetrics(body, { targetCount: 0, writeOK: layout !== "never written" });
        assert.equal(body.includes("{target="), false);
        assert.equal(body.includes(PRIVATE_MARKER), false);
        assert.deepEqual(directory.values, initialValues, `failed write advanced persisted state at tick ${tick}`);
        if (tick >= 3) {
          // This exposed heartbeat keeps SchedulerStale's >900s condition
          // true through its 10m hold, even though discovery/probes keep running.
          assert.ok(Date.now() / 1000 - timestamp > 900);
        }
      }
      assert.equal(probes, 13);
      assert.deepEqual(attempts, Array(13).fill(UPTIME_PROBE_RUN_KEY));
      assert.deepEqual(directory.writes, []);

      rejectWrites = false;
      subtest.mock.timers.setTime(NOW + 65 * 60_000);
      await runScheduledUptimeProbes({ DIRECTORY: directory }, fetchImpl);
      assert.deepEqual(directory.writes.map(({ key }) => key), [UPTIME_PROBE_RUN_KEY]);
      const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
      const body = await response.text();
      assertSchedulerMetrics(body, { timestamp: Date.now() / 1000, targetCount: 1, directoryOK: true });
      assertPersistenceMetrics(body, { targetCount: 1, writeOK: true });
      assert.ok(body.includes('witself_probe_up{target="broken"} 0\n'));
      assert.ok(body.includes(`witself_probe_last_check_timestamp_seconds{target="broken"} ${Date.now() / 1000}\n`));
    });
  }
});

test("the complete legacy pair remains readable and migrates with one new-key write", async (t) => {
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const previous = snapshot();
  const priorScheduler = { ...scheduler(), last_run_at: previous.targets[0].checked_at };
  const directory = new DirectoryKV({
    [UPTIME_PROBES_KEY]: previous,
    [UPTIME_PROBE_SCHEDULER_KEY]: priorScheduler,
    "cell:cell-a": { endpoint: "https://cell-a.test.invalid" },
  });
  const priorTargetsBytes = directory.values.get(UPTIME_PROBES_KEY);
  const priorSchedulerBytes = directory.values.get(UPTIME_PROBE_SCHEDULER_KEY);
  let response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  let body = await response.text();
  assertSchedulerMetrics(body, { timestamp: Date.parse(priorScheduler.last_run_at) / 1000,
    targetCount: 1, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 2, writeOK: true });
  assert.ok(body.includes('witself_probe_up{target="cell-a"} 0\n'));
  assert.ok(body.includes('witself_probe_up{target="control_plane"} 1\n'));
  assert.deepEqual(directory.reads, [UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY]);
  assert.equal(directory.writes.length, 0);

  await runScheduledUptimeProbes({ DIRECTORY: directory }, async () => new Response(null, { status: 200 }));
  assert.deepEqual(directory.writes.map(({ key }) => key), [UPTIME_PROBE_RUN_KEY]);
  assert.equal(directory.writes[0].options, undefined);
  assert.equal(directory.values.get(UPTIME_PROBES_KEY), priorTargetsBytes);
  assert.equal(directory.values.get(UPTIME_PROBE_SCHEDULER_KEY), priorSchedulerBytes);
  assert.deepEqual(Object.keys(directory.document()).sort(), ["scheduler", "targets"]);
  directory.reads = [];
  response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  body = await response.text();
  assert.deepEqual(directory.reads, [UPTIME_PROBE_RUN_KEY]);
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 1, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 1, writeOK: true });
  assert.ok(body.includes('witself_probe_up{target="cell-a"} 1\n'));
});

test("new-document reads take precedence without consulting stale or invalid legacy records", async () => {
  const directory = new DirectoryKV({
    [UPTIME_PROBE_RUN_KEY]: runDocument(),
    [UPTIME_PROBES_KEY]: { private: PRIVATE_MARKER },
    [UPTIME_PROBE_SCHEDULER_KEY]: { private: PRIVATE_MARKER },
  });
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.deepEqual(directory.reads, [UPTIME_PROBE_RUN_KEY]);
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 1, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 2, writeOK: true });
  assert.ok(body.includes('witself_probe_up{target="cell-a"} 0\n'));
  assert.equal(body.includes(PRIVATE_MARKER), false);
});

test("a legacy scheduler without its results exposes the results-missing condition", async () => {
  const directory = new DirectoryKV({ [UPTIME_PROBE_SCHEDULER_KEY]: scheduler() });
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 1, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 0, writeOK: false });
  assert.equal(body.includes("{target="), false);
});

test("persisted target counts include skipped rows and the optional control-plane result", async () => {
  const skipped = { target: "skipped", state: "skipped", http_status_class: 0,
    latency_ms: 0, checked_at: new Date(NOW).toISOString() };
  const directory = new DirectoryKV({
    [UPTIME_PROBE_RUN_KEY]: runDocument([...snapshot().targets, skipped], scheduler(2)),
  });
  const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assertSchedulerMetrics(body, { timestamp: NOW / 1000, targetCount: 2, directoryOK: true });
  assertPersistenceMetrics(body, { targetCount: 3, writeOK: true });
  assert.ok(body.includes('witself_probe_skipped{target="skipped"} 1\n'));
  assert.equal(body.includes('witself_probe_up{target="skipped"}'), false);
});

test("invalid or unparseable run documents expose stale zero gauges and never fall back to legacy", async (t) => {
  const document = runDocument();
  const invalid = {
    "unparseable JSON": `{${PRIVATE_MARKER}`,
    "stored JSON null": "null",
    "non-object": JSON.stringify(PRIVATE_MARKER),
    "missing scheduler": JSON.stringify({ targets: document.targets }),
    "missing targets": JSON.stringify({ scheduler: document.scheduler }),
    "invalid recovery flag": JSON.stringify({ ...document, recovered_from_malformed: PRIVATE_MARKER }),
    "scheduler timestamp": JSON.stringify({ ...document, scheduler: { ...scheduler(), last_run_at: PRIVATE_MARKER } }),
    "scheduler duration": JSON.stringify({ ...document, scheduler: { ...scheduler(), duration_ms: -1 } }),
    "scheduler count": JSON.stringify({ ...document, scheduler: { ...scheduler(), target_count: -1 } }),
    "scheduler directory": JSON.stringify({ ...document, scheduler: { ...scheduler(), directory_ok: PRIVATE_MARKER } }),
    "unsafe target": JSON.stringify(runDocument([{ ...document.targets[0], target: `${PRIVATE_MARKER}\"` }])),
    "duplicate target": JSON.stringify(runDocument([document.targets[0], document.targets[0]])),
    "invalid target state": JSON.stringify(runDocument([{ ...document.targets[0], state: "unknown" }])),
    "invalid target status": JSON.stringify(runDocument([{ ...document.targets[0], http_status_class: 503 }])),
    "inconsistent target state": JSON.stringify(runDocument([{ ...document.targets[0], state: "ok" }])),
    "inconsistent skipped target": JSON.stringify(runDocument([{ ...document.targets[0], state: "skipped" }])),
    "invalid target latency": JSON.stringify(runDocument([{ ...document.targets[0], latency_ms: -1 }])),
    "invalid target timestamp": JSON.stringify(runDocument([{ ...document.targets[0], checked_at: PRIVATE_MARKER }])),
    "invalid retained observation": JSON.stringify(runDocument([{ target: "skipped", state: "skipped",
      http_status_class: 0, latency_ms: 0, checked_at: new Date(NOW).toISOString(), last_observation: PRIVATE_MARKER }])),
  };
  for (const [name, raw] of Object.entries(invalid)) {
    await t.test(name, async () => {
      const directory = new DirectoryKV({
        [UPTIME_PROBES_KEY]: snapshot(), [UPTIME_PROBE_SCHEDULER_KEY]: scheduler(),
      });
      directory.values.set(UPTIME_PROBE_RUN_KEY, raw);
      const response = await worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
      assert.equal(response.status, 200);
      assert.equal(response.headers.get("Cache-Control"), "no-store");
      assertMissingRunMetrics(await response.text());
      assert.deepEqual(directory.reads, [UPTIME_PROBE_RUN_KEY]);
      assert.equal(directory.writes.length, 0);
    });
  }
});

test("transport errors and timeouts fail the scrape for each persisted run key", async (t) => {
  for (const key of [UPTIME_PROBE_RUN_KEY, UPTIME_PROBES_KEY, UPTIME_PROBE_SCHEDULER_KEY]) {
    for (const failure of ["throw", "timeout"]) {
      await t.test(`${key}: ${failure}`, async (subtest) => {
        subtest.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
        const directory = new DirectoryKV({
          [UPTIME_PROBES_KEY]: snapshot(), [UPTIME_PROBE_SCHEDULER_KEY]: scheduler(),
        });
        const get = directory.get.bind(directory);
        directory.get = async (readKey, options) => {
          if (readKey !== key) return get(readKey, options);
          if (failure === "throw") throw new Error(PRIVATE_MARKER);
          return new Promise(() => {});
        };
        const pending = worker.fetch(metricsRequest(), { DIRECTORY: directory }, {});
        await flush();
        subtest.mock.timers.tick(5000);
        const response = await pending;
        assert.equal(response.status, 503);
        assert.equal(response.headers.get("Cache-Control"), "no-store");
        assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
        assert.equal((await response.text()).includes(PRIVATE_MARKER), false);
        assert.equal(directory.writes.length, 0);
      });
    }
  }
});
