import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { readFile } from "node:fs/promises";
import { register } from "node:module";
import test from "node:test";
import {
  ACCOUNT_BACKUP_SCAN_KEY, ACCOUNT_BACKUP_STATE_SCHEMA,
  RESTORE_DRILL_FRESHNESS_KEY, RESTORE_DRILL_FRESHNESS_SCHEMA,
  BackupValidationBusyError, RestoreDrillProtocolError, RestoreDrillUnconfirmedError,
  accountBackupStatus, backupJobIdentity, validCatalogRecord,
} from "../src/account-backup-runtime.mjs";
import {
  RESTORE_DRILL_SCHEDULE_KEY, RESTORE_DRILL_SCHEDULE_SCHEMA,
  chainRestoreDrill, restoreDrillScheduleConfig, runScheduledRestoreDrill,
} from "../src/restore-drill-schedule.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const { default: worker } = await import("../src/index.js");
if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(a, b) {
      const left = new Uint8Array(a);
      const right = new Uint8Array(b);
      return left.byteLength === right.byteLength && nodeTimingSafeEqual(left, right);
    },
  });
}

const NOW = Date.parse("2026-10-14T16:00:00.000Z");
const WEEK = 7 * 24 * 60 * 60 * 1000;
const TARGET = "civo-fixture-backup";
const SOURCE = "civo-fixture-serving";
const DRILL = "11111111-2222-3333-4444-555555555555";
const PRIVATE = "private-schedule-error-must-not-escape";
const flush = () => new Promise((resolve) => setImmediate(resolve));
const iso = (value = NOW) => new Date(value).toISOString();
const scheduleKeys = ["schema_version", "window_start", "state", "reason", "rotation_after",
  "examined_after", "account_id", "backup_id", "drill_id", "started_at", "error", "history", "updated_at"].sort();

class ListKV {
  constructor(entries = {}, { rateLimited = false } = {}) {
    this.values = new Map(Object.entries(entries).map(([key, value]) => [key, JSON.stringify(value)]));
    this.clock = NOW;
    this.rateLimited = rateLimited;
    this.lastPuts = new Map();
    this.resetCounters();
  }
  resetCounters() {
    this.gets = [];
    this.lists = [];
    this.putAttempts = [];
    this.puts = [];
  }
  async get(key, options) {
    this.gets.push({ key, type: options?.type });
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }
  async list({ prefix = "", limit = 1000, cursor } = {}) {
    assert.ok(limit <= 1000);
    this.lists.push({ prefix, limit, cursor });
    const keys = [...this.values.keys()].filter((key) => key.startsWith(prefix)).sort();
    const offset = cursor === undefined ? 0 : Number(/^opaque-page:(\d+)$/.exec(cursor)?.[1]);
    assert.ok(Number.isInteger(offset), "listing must reuse the opaque cursor");
    const next = offset + limit;
    return { keys: keys.slice(offset, next).map((name) => ({ name })),
      list_complete: next >= keys.length,
      ...(next < keys.length ? { cursor: `opaque-page:${next}` } : {}) };
  }
  async put(key, value) {
    this.putAttempts.push({ key });
    if (this.rejectPut?.(key) || (this.rateLimited && this.clock - (this.lastPuts.get(key) ?? -Infinity) < 1000)) {
      throw new Error(PRIVATE);
    }
    this.values.set(key, value);
    this.lastPuts.set(key, this.clock);
    this.puts.push({ key });
  }
  record() {
    const value = this.values.get(RESTORE_DRILL_SCHEDULE_KEY);
    return value === undefined ? null : JSON.parse(value);
  }
}

function catalog(accountID, at = NOW - 60_000) {
  const job = backupJobIdentity(accountID, at, 1);
  const record = { account_id: accountID, backup_id: job.backup_id, object: job.object,
    source_cell: SOURCE, source_registration_id: "fixture-registration", source_registered_at: iso(NOW - WEEK),
    source_route_epoch: 1, scheduled_at: job.scheduled_at, exported_at: iso(at), verified_at: iso(at),
    committed_at: iso(at), status: "active", size: 19, archive_schema_version: 73,
    entries: 4, chunks: 2, trailer_sha256: "a".repeat(64), r2_etag: "fixture-etag" };
  assert.equal(validCatalogRecord(record, accountID), true, "fixture must satisfy the real catalog validator");
  return record;
}
function backupStatus(accountID) {
  return { schema_version: "witself.v0", account_id: accountID, restore_drill: null,
    backups: { schema_version: ACCOUNT_BACKUP_STATE_SCHEMA, account_id: accountID,
      revision: 1, catalog: [catalog(accountID)], current_job: null, failures: [] } };
}
function storedRecord(overrides = {}) {
  return { schema_version: RESTORE_DRILL_SCHEDULE_SCHEMA, window_start: iso(),
    state: "pending", reason: null, rotation_after: null, examined_after: null,
    account_id: null, backup_id: null, drill_id: null, started_at: null, error: null,
    history: [], updated_at: iso(), ...overrides };
}
function freshness(running = []) {
  return { schema_version: RESTORE_DRILL_FRESHNESS_SCHEMA,
    last_started_at: null, last_finished_at: null, last_validated_at: null,
    last_state: null, running, updated_at: iso() };
}
function completeScan(time = NOW, interval = 1440) {
  const slot = backupJobIdentity("slot", time, interval).scheduled_at;
  return { complete: true, slot, pass_started_slot: slot };
}
function fixture(ids = ["a"], options = {}) {
  const kv = new ListKV(Object.fromEntries(ids.map((id) => [`acct:${id}`, { cell: SOURCE }])), options);
  const calls = [];
  const examined = [];
  const env = { CP_RESTORE_DRILLS_ENABLED: "true", CP_RESTORE_DRILLS_TARGET_CELL: TARGET,
    CP_PUBLIC_ORIGIN: "https://cp.test.invalid", DIRECTORY: kv, ACCOUNT_BACKUP: {},
    ACCOUNT_LIFECYCLE: {}, BACKUPS: {}, CELL_COORDINATOR: {} };
  const dependencies = {
    now: () => NOW,
    sleep: async (ms) => { assert.equal(ms, 1100); kv.clock += ms; },
    lifecycleStatus: async (_env, id) => {
      examined.push(id);
      return { account_id: id, initialized: true, operation: null };
    },
    backupStatus: async (_env, id) => backupStatus(id),
    begin: async (...args) => {
      calls.push(args);
      return { drill_id: DRILL, restore_drill: { drill_id: DRILL, started_at: iso() },
        complete() { assert.fail("a scheduled start must never call complete"); } };
    },
  };
  return { env, kv, calls, examined, dependencies };
}
function captureLogs(t) {
  const logs = [];
  t.mock.method(console, "log", (...args) => logs.push(args.join(" ")));
  return logs;
}
async function tick(f, time = NOW) {
  f.kv.clock += 5000;
  f.kv.resetCounters();
  const result = await runScheduledRestoreDrill(f.env, time, f.dependencies);
  assert.ok(f.kv.puts.length <= 2, "at most two successful schedule writes per tick");
  assert.ok(f.kv.putAttempts.length <= 3, "at most three schedule put attempts per tick");
  assert.ok(f.kv.putAttempts.every(({ key }) => key === RESTORE_DRILL_SCHEDULE_KEY), "only the schedule key is written");
  if (f.kv.record()) assert.deepEqual(Object.keys(f.kv.record()).sort(), scheduleKeys);
  return result;
}
function requestStatus(accountID) {
  const query = accountID === undefined ? "" : `?account_id=${encodeURIComponent(accountID)}`;
  return new Request(`https://cp.test.invalid/v1/backups/status${query}`, {
    headers: { Authorization: "Bearer fixture-fleet-token" },
  });
}

test("a disabled schedule touches nothing, keeps seven maintenance tasks and keeps responses byte-identical", async (t) => {
  const logs = captureLogs(t);
  for (const value of [undefined, "", "false", "1", "yes"]) {
    const reads = [];
    const env = new Proxy({}, { get(_target, key) {
      reads.push(key);
      assert.equal(key, "CP_RESTORE_DRILLS_ENABLED");
      return value;
    } });
    assert.deepEqual(restoreDrillScheduleConfig(env), { enabled: false });
    assert.deepEqual(await runScheduledRestoreDrill(env, NOW), { ran: false, enabled: false });
    assert.deepEqual(reads, ["CP_RESTORE_DRILLS_ENABLED", "CP_RESTORE_DRILLS_ENABLED"]);
  }
  assert.equal(restoreDrillScheduleConfig({ ...fixture().env, CP_RESTORE_DRILLS_ENABLED: " TRUE " }).enabled, true);
  const kv = new ListKV();
  const bindingReads = [];
  const binding = new Proxy({}, { get(_target, key) { bindingReads.push(key); return undefined; } });
  const pending = [];
  await worker.scheduled({ cron: "*/5 * * * *", scheduledTime: NOW }, {
    DIRECTORY: kv, ACCOUNT_BACKUP: binding, ACCOUNT_LIFECYCLE: binding, CELL_COORDINATOR: binding,
  }, { waitUntil(promise) { pending.push(promise); } });
  const settled = await Promise.allSettled(pending);
  assert.equal(pending.length, 7, "preserve the existing signup-observation maintenance task");
  assert.ok(settled.every(({ status }) => status === "fulfilled"));
  assert.deepEqual(bindingReads, []);
  assert.equal(kv.gets.some(({ key }) => [RESTORE_DRILL_SCHEDULE_KEY, RESTORE_DRILL_FRESHNESS_KEY].includes(key)), false);
  assert.deepEqual(kv.putAttempts, []);
  assert.deepEqual(kv.lists, []);
  const sequence = [];
  const value = {};
  assert.equal(await chainRestoreDrill(Promise.resolve(value), async (result) => { assert.equal(result, value); sequence.push("resolve"); }), value);
  const error = new Error("fixture backup failure");
  assert.deepEqual(await chainRestoreDrill(Promise.reject(error), async (result) => { assert.equal(result.succeeded, false); sequence.push("reject"); }), { succeeded: false });
  assert.deepEqual(sequence, ["resolve", "reject"]);
  for (const rejects of [false, true]) {
    let releaseDrill;
    let settled = false;
    const chained = chainRestoreDrill(rejects ? Promise.reject(error) : Promise.resolve(value),
      () => new Promise((resolve) => { releaseDrill = resolve; }));
    const observed = chained.then((actual) => {
      settled = true;
      if (rejects) assert.deepEqual(actual, { succeeded: false });
      else assert.equal(actual, value);
    });
    await flush();
    assert.equal(typeof releaseDrill, "function");
    assert.equal(settled, false, "the backup result waits for the drill tick to settle");
    releaseDrill();
    await observed;
    assert.equal(settled, true);
  }
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const env = { DIRECTORY: kv, FLEET_TOKEN: "fixture-fleet-token" };
  const golden = JSON.stringify(await accountBackupStatus(env));
  const response = await worker.fetch(requestStatus(), env, {});
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Content-Type"), "application/json");
  assert.equal(await response.text(), golden);
  assert.equal(golden.includes("restore_drill_schedule"), false);
  assert.equal(kv.gets.some(({ key }) => key === RESTORE_DRILL_SCHEDULE_KEY), false);
  assert.deepEqual(logs, []);
});

test("enabled maintenance runs the drill after the backup scan and uses scheduledTime", async (t) => {
  captureLogs(t);
  t.mock.timers.enable({ apis: ["Date"], now: NOW + 60_000 });
  const f = fixture();
  f.env.CP_ACCOUNT_BACKUPS_ENABLED = "true";
  f.env.CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES = "5";
  const events = [];
  const get = f.kv.get.bind(f.kv);
  const put = f.kv.put.bind(f.kv);
  f.kv.get = (key, options) => { events.push(`get:${key}`); return get(key, options); };
  f.kv.put = async (key, value) => { await put(key, value); events.push(`put:${key}`); };
  const target = { endpoint: "https://backup.test.invalid", accepting: false,
    backup_validation_target: true, registration_id: "fixture-target-registration", registered_at: iso(NOW - WEEK),
    backup_token: "fixture-backup-value", provision_token: "fixture-provision-value" };
  f.kv.values.set(`cell:${TARGET}`, JSON.stringify(target));
  f.env.CELL_COORDINATOR = { idFromName: (id) => id, get: (id) => ({ fetch: async (request) => {
    assert.equal(id, TARGET);
    assert.equal(new URL(request.url).pathname, "/registration-status");
    return Response.json({ ok: true, cell_name: TARGET, expected_registration_id: target.registration_id,
      current_registration_id: target.registration_id, registration_status: "active", active_cell: target });
  } }) };
  f.env.ACCOUNT_LIFECYCLE = { idFromName: (id) => id, get: (id) => ({ fetch: async () =>
    Response.json({ ok: true, account_id: id, status: { account_id: id, initialized: true, operation: null } }) }) };
  const begins = [];
  f.env.ACCOUNT_BACKUP = { idFromName: (id) => id, get: (id) => ({ fetch: async (request) => {
    const path = new URL(request.url).pathname;
    if (path === "/status") {
      assert.equal(id, "a");
      return Response.json(backupStatus(id));
    }
    if (path === "/start") {
      // Scheduled backups dispatch through the asynchronous /start path (slice 140).
      const input = await request.json();
      events.push("backup:start");
      assert.equal(input.account_id, "a");
      assert.equal(input.scheduled_at, backupJobIdentity("a", NOW, 5).scheduled_at);
      return Response.json({ schema_version: "witself.v0", account_id: "a", backup_id: input.backup_id, status: "accepted" },
        { status: 202 });
    }
    assert.equal(path, "/restore-drill-driver:start");
    const input = await request.json();
    begins.push({ backup_id: input.backup_id, target_cell: input.target_cell });
    events.push("drill:begin");
    return Response.json({ restore_drill: { drill_id: input.drill_id, driver: "alarm", started_at: iso() } });
  } }) };
  const record = catalog("a");
  f.env.BACKUPS = {
    get() { assert.fail("an alarm start must not fetch the archive body"); },
    async head(object) {
      assert.equal(object, record.object);
      return { size: record.size, etag: record.r2_etag, customMetadata: {
        account_id: "a", backup_id: record.backup_id, cell: SOURCE,
        cell_registered_at: record.source_registered_at, cell_registration_id: record.source_registration_id,
        scheduled_at: record.scheduled_at, route_epoch: String(record.source_route_epoch),
      } };
    },
  };
  t.mock.method(globalThis, "fetch", async (url) => {
    assert.equal(String(url), `${target.endpoint}/v1/version`);
    return Response.json({ schema_version: "witself.v0", backup_validation_protocol: 2 });
  });
  const pending = [];
  await worker.scheduled({ cron: "*/5 * * * *", scheduledTime: NOW }, f.env,
    { waitUntil(promise) { pending.push(promise); } });
  const settled = await Promise.allSettled(pending);
  assert.equal(pending.length, 7);
  assert.ok(settled.every(({ status }) => status === "fulfilled"));
  assert.ok(events.indexOf("backup:start") >= 0);
  const scanPut = events.indexOf(`put:${ACCOUNT_BACKUP_SCAN_KEY}`);
  const scheduleRead = events.indexOf(`get:${RESTORE_DRILL_SCHEDULE_KEY}`);
  assert.ok(scanPut > events.indexOf("backup:start"));
  assert.ok(scheduleRead > scanPut, "the drill reads the scan only after the backup tick stores it");
  assert.ok(events.indexOf("drill:begin") > scheduleRead);
  assert.deepEqual(begins, [{ backup_id: record.backup_id, target_cell: TARGET }]);
  assert.equal(f.kv.record().state, "started");
  assert.equal(f.kv.record().window_start, iso(NOW));
  assert.equal(f.kv.record().updated_at, iso(NOW), "the drill receives event.scheduledTime rather than the current clock");
});

test("the window is four hours on the configured UTC weekday and every setting is validated", async (t) => {
  const logs = captureLogs(t);
  for (const [time, inside] of [[NOW - 300_000, false], [NOW, true], [NOW + 235 * 60_000, true],
    [NOW + 240 * 60_000, false], [NOW + 86400_000, false]]) {
    const f = fixture();
    if (!inside) {
      f.env = new Proxy(f.env, { get(target, key) {
        assert.ok(!["DIRECTORY", "ACCOUNT_BACKUP", "ACCOUNT_LIFECYCLE", "BACKUPS", "CELL_COORDINATOR"].includes(key));
        return target[key];
      } });
    }
    const result = await tick(f, time);
    assert.equal(f.calls.length, Number(inside));
    if (!inside) assert.deepEqual(result, { ran: false, reason: "outside_window" });
  }
  const valid = restoreDrillScheduleConfig({ ...fixture().env, CP_RESTORE_DRILLS_WEEKDAY: " 0 ", CP_RESTORE_DRILLS_START_HOUR_UTC: "18" });
  assert.deepEqual(valid, { enabled: true, valid: true, target_cell: TARGET, weekday: 0, start_hour: 18, origin: "https://cp.test.invalid" });
  const defaults = restoreDrillScheduleConfig(fixture().env);
  assert.equal(defaults.weekday, 3);
  assert.equal(defaults.start_hour, 16);
  for (const [key, values] of [
    ["CP_RESTORE_DRILLS_TARGET_CELL", [undefined, "", "UPPER", "a/b", "a".repeat(65)]],
    ["CP_RESTORE_DRILLS_WEEKDAY", ["7", "-1", "3.5"]],
    ["CP_RESTORE_DRILLS_START_HOUR_UTC", ["0", "19", "20", "16h"]],
    ["CP_PUBLIC_ORIGIN", [undefined, "http://self.witwave.ai", "https://self.witwave.ai/"]],
  ]) {
    for (const value of values) {
      const env = new Proxy({ ...fixture().env, [key]: value }, { get(target, property) {
        assert.ok(!["DIRECTORY", "ACCOUNT_BACKUP", "ACCOUNT_LIFECYCLE", "BACKUPS", "CELL_COORDINATOR"].includes(property));
        return target[property];
      } });
      assert.deepEqual(restoreDrillScheduleConfig(env), { enabled: true, valid: false });
      await runScheduledRestoreDrill(env, NOW);
      assert.equal(logs.at(-1), "restore-drill-schedule: skipped reason=invalid_config");
    }
  }
  for (const key of ["DIRECTORY", "ACCOUNT_BACKUP", "ACCOUNT_LIFECYCLE", "BACKUPS", "CELL_COORDINATOR"]) {
    const f = fixture();
    delete f.env[key];
    await runScheduledRestoreDrill(f.env, NOW, f.dependencies);
    assert.equal(f.calls.length, 0);
    assert.equal(logs.at(-1), "restore-drill-schedule: skipped reason=invalid_config");
  }
});

test("weekly windows rotate through live accounts in order and wrap", async (t) => {
  captureLogs(t);
  const f = fixture(["e", "c", "a", "d", "b"]);
  for (let i = 0; i < 6; i++) await tick(f, NOW + i * WEEK);
  assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["a", "b", "c", "d", "e", "a"]);
  const growing = fixture(["a", "b", "c", "d", "e"]);
  await tick(growing);
  await tick(growing, NOW + WEEK);
  growing.kv.values.set("acct:bb", JSON.stringify({ cell: SOURCE }));
  await tick(growing, NOW + 2 * WEEK);
  growing.kv.values.delete("acct:c");
  await tick(growing, NOW + 3 * WEEK);
  assert.deepEqual(growing.calls.map(([, input]) => input.account_id), ["a", "b", "bb", "d"]);
  const one = fixture(["a"]);
  for (let i = 0; i < 3; i++) await tick(one, NOW + i * WEEK);
  assert.deepEqual(one.calls.map(([, input]) => input.account_id), ["a", "a", "a"]);
});

test("a new weekly window resets examined_after after an ineligible candidate", async (t) => {
  captureLogs(t);
  const f = fixture(["a", "b", "c", "d", "e"]);
  f.dependencies.backupStatus = async (_env, id) => {
    const status = backupStatus(id);
    if (id === "a") status.backups.catalog = [];
    return status;
  };
  await tick(f);
  assert.equal(f.kv.record().examined_after, "a");
  assert.equal(f.kv.record().rotation_after, "b");
  for (let week = 1; week <= 3; week++) {
    await tick(f, NOW + week * WEEK);
    assert.equal(f.kv.record().examined_after, null, "candidate progress belongs only to its weekly window");
  }
  assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["b", "c", "d", "e"]);
});

test("only a complete backup scan for the current pass slot clears the guard", async (t) => {
  captureLogs(t);
  for (const interval of [1440, 360]) {
    const at = NOW + 2 * 60 * 60_000;
    const current = completeScan(at, interval);
    const stale = completeScan(at - interval * 60_000, interval);
    for (const [scan, starts] of [
      [current, true],
      [{ complete: true, slot: current.slot }, true],
      [stale, false],
      [{ ...current, pass_started_slot: stale.slot }, false],
      [{ ...current, pass_started_slot: null }, true],
      [{ complete: true }, false],
      [{ ...current, complete: false }, false],
    ]) {
      const f = fixture();
      f.env.CP_ACCOUNT_BACKUPS_ENABLED = "true";
      f.env.CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES = String(interval);
      f.kv.values.set(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(scan));
      await tick(f, at);
      assert.equal(f.calls.length, Number(starts));
      assert.equal(f.kv.record().state, starts ? "started" : "pending");
      assert.equal(f.kv.record().reason, starts ? null : "backup_scan_incomplete");
    }
  }
});

test("failed and rejected backup ticks hold the drill without rejecting the chain", async (t) => {
  captureLogs(t);
  for (const rejected of [false, true]) {
    const f = fixture();
    f.env.CP_ACCOUNT_BACKUPS_ENABLED = "true";
    f.kv.values.set(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(completeScan()));
    const failed = { succeeded: false };
    let drillTicks = 0;
    const result = await chainRestoreDrill(
      rejected ? Promise.reject(new Error(PRIVATE)) : Promise.resolve(failed),
      async (backupTickResult) => {
        drillTicks++;
        assert.equal(backupTickResult.succeeded, false);
        return runScheduledRestoreDrill(f.env, NOW, { ...f.dependencies, backupTickResult });
      },
    );
    assert.equal(drillTicks, 1, "a rejected backup promise still runs the holding drill tick");
    assert.deepEqual(result, failed);
    assert.equal(f.calls.length, 0);
    assert.equal(f.kv.record().state, "pending");
    assert.equal(f.kv.record().reason, "backup_scan_incomplete");
  }
});

test("a delayed start rereads the clock after the starting write and holds a closed window", async (t) => {
  captureLogs(t);
  for (const elapsed of [4 * 60 * 60_000, 4 * 60 * 60_000 + 1]) {
    const f = fixture();
    let now = NOW + 235 * 60_000;
    f.dependencies.now = () => now;
    const put = f.kv.put.bind(f.kv);
    f.kv.put = async (key, value) => {
      await put(key, value);
      if (JSON.parse(value).state === "starting") now = NOW + elapsed;
    };
    await tick(f, NOW + 235 * 60_000);
    assert.equal(f.calls.length, 0);
    assert.equal(f.kv.record().state, "pending");
    assert.equal(f.kv.record().reason, "window_closed");
    assert.equal(f.kv.record().rotation_after, null);
    // The new closed reason is a valid stored record, preserving rotation and history.
    f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify({ ...f.kv.record(), rotation_after: "a" }));
    f.kv.values.set("acct:b", JSON.stringify({ cell: SOURCE }));
    f.dependencies.now = () => NOW + WEEK;
    await tick(f, NOW + WEEK);
    assert.equal(f.calls.at(-1)[1].account_id, "b");
    assert.equal(f.kv.record().history[0].window_start, iso());
  }
});

test("overlapping ticks keep a started window when the later begin returns busy", async (t) => {
  const logs = captureLogs(t);
  const f = fixture(["a", "b"]);
  const get = f.kv.get.bind(f.kv);
  let initialReads = 0;
  let releaseInitialReads;
  const initialBarrier = new Promise((resolve) => { releaseInitialReads = resolve; });
  f.kv.get = async (key, options) => {
    if (key === RESTORE_DRILL_SCHEDULE_KEY && ++initialReads <= 2) {
      const value = await get(key, options);
      if (initialReads === 2) releaseInitialReads();
      await initialBarrier;
      return value;
    }
    return get(key, options);
  };
  const begin = f.dependencies.begin;
  let releaseBusy;
  const busyBarrier = new Promise((resolve) => { releaseBusy = resolve; });
  f.dependencies.begin = async (...args) => {
    if (f.calls.length === 0) return begin(...args);
    f.calls.push(args);
    await busyBarrier;
    throw new BackupValidationBusyError({ state: "running" });
  };
  const first = runScheduledRestoreDrill(f.env, NOW, f.dependencies);
  const second = runScheduledRestoreDrill(f.env, NOW, f.dependencies);
  await flush();
  assert.equal(f.calls.length, 2, "both initial reads precede the starting fence");
  assert.equal(f.kv.record().state, "started");
  const started = f.kv.values.get(RESTORE_DRILL_SCHEDULE_KEY);
  releaseBusy();
  await Promise.all([first, second]);
  assert.equal(f.kv.values.get(RESTORE_DRILL_SCHEDULE_KEY), started, "the late busy outcome cannot downgrade the winner");
  assert.ok(logs.includes("restore-drill-schedule: state=started reason=kept"));
  await tick(f, NOW + 150 * 60_000);
  assert.equal(f.calls.length, 2, "a later tick in the window performs no additional begin");
  assert.equal(f.kv.putAttempts.length, 0);
});

test("a concurrent tick holds on a drilling candidate instead of starting another account", async (t) => {
  captureLogs(t);
  const f = fixture(["a", "b"]);
  const begin = f.dependencies.begin;
  let releaseFirst;
  let drilling = false;
  const firstBarrier = new Promise((resolve) => { releaseFirst = resolve; });
  f.dependencies.begin = async (...args) => {
    const firstBegin = f.calls.length === 0;
    const result = await begin(...args);
    if (firstBegin) {
      drilling = true;
      await firstBarrier;
    }
    return result;
  };
  f.dependencies.backupStatus = async (_env, id) => ({ ...backupStatus(id),
    restore_drill: id === "a" && drilling ? { state: "running" } : null });
  const first = runScheduledRestoreDrill(f.env, NOW, f.dependencies);
  try {
    await flush();
    assert.equal(drilling, true);
    // A stale schedule read and absent freshness leave per-account state as the guard.
    const get = f.kv.get.bind(f.kv);
    let stale = true;
    f.kv.get = (key, options) => {
      if (key === RESTORE_DRILL_SCHEDULE_KEY && stale) { stale = false; return Promise.resolve(null); }
      return get(key, options);
    };
    await runScheduledRestoreDrill(f.env, NOW, f.dependencies);
    assert.equal(f.kv.record().state, "pending");
    assert.equal(f.kv.record().reason, "drill_running");
    assert.equal(f.kv.record().rotation_after, null);
    assert.equal(f.kv.record().examined_after, null);
    assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["a"]);
    assert.deepEqual(f.examined, ["a", "a"], "the second tick never examines b");
  } finally {
    releaseFirst();
    await first;
  }
  assert.equal(f.kv.record().state, "started");
});

test("a drilling candidate holds the window despite another eligibility read failing", async (t) => {
  captureLogs(t);
  for (const failing of ["lifecycle", "route"]) {
    const f = fixture(["a", "b"]);
    f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({ rotation_after: "z" })));
    f.dependencies.backupStatus = async (_env, id) => ({ ...backupStatus(id),
      restore_drill: id === "a" ? { state: "running" } : null });
    if (failing === "lifecycle") f.dependencies.lifecycleStatus = async () => { throw new Error(PRIVATE); };
    else {
      const get = f.kv.get.bind(f.kv);
      f.kv.get = async (key, options) => {
        if (key === "acct:a") throw new Error(PRIVATE);
        return get(key, options);
      };
    }
    await tick(f);
    assert.equal(f.calls.length, 0);
    assert.equal(f.kv.record().state, "pending");
    assert.equal(f.kv.record().reason, "drill_running");
    assert.equal(f.kv.record().rotation_after, "z");
    assert.equal(f.kv.record().examined_after, null);
  }
});

test("every begin outcome keeps a same-window final record on its reread", async (t) => {
  const logs = captureLogs(t);
  for (const state of ["started", "start_failed", "start_unconfirmed", "no_eligible_account"]) {
    for (const error of [null, new BackupValidationBusyError({ state: "running" }),
      new RestoreDrillProtocolError(), new RestoreDrillUnconfirmedError(DRILL), new Error(PRIVATE)]) {
      const f = fixture();
      const final = JSON.stringify(storedRecord({ state, rotation_after: "z" }));
      const begin = f.dependencies.begin;
      f.dependencies.begin = async (...args) => {
        f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, final);
        if (error) { f.calls.push(args); throw error; }
        return begin(...args);
      };
      const before = logs.length;
      await tick(f);
      assert.equal(f.calls.length, 1);
      assert.equal(f.kv.values.get(RESTORE_DRILL_SCHEDULE_KEY), final);
      assert.deepEqual(logs.slice(before), [`restore-drill-schedule: state=${state} reason=kept`]);
      assert.equal(f.kv.putAttempts.length, 1);
    }
  }
});

test("an outcome retry rereads the final fence and busy preserves a newer stored rotation", async (t) => {
  const logs = captureLogs(t);
  const retried = fixture();
  const final = JSON.stringify(storedRecord({ state: "started", rotation_after: "z" }));
  retried.kv.rejectPut = () => retried.kv.putAttempts.length === 2;
  retried.dependencies.sleep = async (ms) => {
    assert.equal(ms, 1100);
    retried.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, final);
  };
  await tick(retried);
  assert.equal(retried.kv.putAttempts.length, 2, "the retry does not overwrite a newly visible final record");
  assert.equal(retried.kv.values.get(RESTORE_DRILL_SCHEDULE_KEY), final);
  assert.ok(logs.includes("restore-drill-schedule: state=started reason=kept"));
  const busy = fixture(["a", "b", "c", "d"]);
  busy.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({ rotation_after: "a" })));
  busy.dependencies.begin = async (...args) => {
    busy.calls.push(args);
    busy.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({ state: "starting", rotation_after: "c", account_id: "c" })));
    throw new BackupValidationBusyError({ state: "running" });
  };
  await tick(busy);
  assert.equal(busy.kv.record().state, "pending");
  assert.equal(busy.kv.record().reason, "drill_running");
  assert.equal(busy.kv.record().rotation_after, "c", "the late busy result does not move the observed rotation back to a");
});

test("guards hold the start", async (t) => {
  captureLogs(t);
  for (const complete of [false, undefined]) {
    const f = fixture();
    f.env.CP_ACCOUNT_BACKUPS_ENABLED = "true";
    if (complete !== undefined) f.kv.values.set(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify({ complete }));
    await tick(f);
    assert.equal(f.calls.length, 0);
    assert.equal(f.kv.record().reason, "backup_scan_incomplete");
  }
  const disabled = fixture();
  await tick(disabled);
  assert.equal(disabled.kv.gets.some(({ key }) => key === ACCOUNT_BACKUP_SCAN_KEY), false);
  for (const [value, reason] of [
    [JSON.stringify(freshness([{ drill_id: DRILL, deadline_at: iso(NOW + 60_000) }])), "drill_running"],
    [JSON.stringify({ invalid: true }), "freshness_unavailable"],
    ["{not json", "freshness_unavailable"],
    ["throw", "freshness_unavailable"],
  ]) {
    const f = fixture();
    if (value === "throw") {
      const original = f.kv.get.bind(f.kv);
      f.kv.get = async (key, options) => { if (key === RESTORE_DRILL_FRESHNESS_KEY) throw new Error(PRIVATE); return original(key, options); };
    } else f.kv.values.set(RESTORE_DRILL_FRESHNESS_KEY, value);
    await tick(f);
    assert.equal(f.calls.length, 0);
    assert.equal(f.kv.record().reason, reason);
  }
  const expired = fixture();
  expired.kv.values.set(RESTORE_DRILL_FRESHNESS_KEY, JSON.stringify(freshness([{ drill_id: DRILL, deadline_at: iso(NOW - 120_000) }])));
  await tick(expired);
  assert.equal(expired.calls.length, 1, "expired entries plus final grace do not hold the start");
  const states = ["pending", "archived", "unrouted", "target", "moving", "lifecycle-error", "pending-job", "running-job", "retrying-job", "no-catalog", "invalid-catalog", "backup-error"];
  for (const state of states) {
    const f = fixture(["a", "b"]);
    if (["pending", "archived"].includes(state)) f.kv.values.set(`${state}:a`, "true");
    if (state === "unrouted") f.kv.values.set("acct:a", JSON.stringify({ cell: "INVALID" }));
    if (state === "target") f.kv.values.set("acct:a", JSON.stringify({ cell: TARGET }));
    f.dependencies.lifecycleStatus = async (_env, id) => {
      if (id === "a" && state === "lifecycle-error") throw new Error(PRIVATE);
      return { account_id: id, initialized: true, operation: id === "a" && state === "moving" ? { kind: "move" } : null };
    };
    f.dependencies.backupStatus = async (_env, id) => {
      if (id === "a" && state === "backup-error") throw new Error(PRIVATE);
      const status = backupStatus(id);
      if (id === "a") {
        if (state.endsWith("-job")) status.backups.current_job = { status: state.slice(0, -4) };
        if (state === "no-catalog") status.backups.catalog = [];
        if (state === "invalid-catalog") status.backups.catalog[0].size = 0;
      }
      return status;
    };
    await tick(f);
    assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["b"], state);
    assert.equal(f.kv.record().examined_after, "a", state);
  }
  const f = fixture(["a", "b", "c", "d", "e"]);
  f.dependencies.lifecycleStatus = async (_env, id) => {
    f.examined.push(id);
    return { account_id: id, initialized: true, operation: {} };
  };
  await tick(f);
  assert.equal(f.kv.record().state, "pending");
  assert.equal(f.kv.record().reason, "candidates_exhausted_this_tick");
  assert.equal(f.kv.record().examined_after, "c");
  assert.deepEqual(f.examined, ["a", "b", "c"]);
  await tick(f, NOW + 300_000);
  assert.equal(f.kv.record().state, "no_eligible_account");
  assert.equal(f.kv.record().examined_after, "e");
  assert.deepEqual(f.examined, ["a", "b", "c", "d", "e"]);
  assert.equal(f.calls.length, 0);
  await tick(f, NOW + 600_000);
  assert.deepEqual(f.kv.gets.map(({ key }) => key), [RESTORE_DRILL_SCHEDULE_KEY]);
  assert.equal(f.kv.putAttempts.length, 0);
});

test("a window starts at most one drill", async (t) => {
  captureLogs(t);
  const f = fixture(["a", "b"]);
  await tick(f);
  assert.equal(f.kv.record().state, "started");
  for (let minute = 5; minute < 240; minute += 5) {
    await tick(f, NOW + minute * 60_000);
    assert.deepEqual(f.kv.gets.map(({ key }) => key), [RESTORE_DRILL_SCHEDULE_KEY]);
    assert.equal(f.kv.putAttempts.length, 0);
    assert.equal(f.kv.lists.length, 0);
  }
  assert.equal(f.calls.length, 1);
  await tick(f, NOW + WEEK);
  assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["a", "b"]);
  const equivalent = fixture();
  equivalent.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({
    window_start: "2026-10-14T17:00:00.000+01:00", state: "started", drill_id: DRILL,
  })));
  await tick(equivalent);
  assert.equal(equivalent.calls.length, 0, "offset-equivalent timestamps retain the same window fence");
  assert.equal(equivalent.kv.putAttempts.length, 0);
});

test("start errors keep or advance the rotation, and a fast refusal is still recorded", async (t) => {
  const logs = captureLogs(t);
  for (const [error, state, reason] of [
    [new BackupValidationBusyError({ state: "running" }), "pending", "drill_running"],
    [new RestoreDrillProtocolError(), "pending", "target_unavailable"],
    [new RestoreDrillUnconfirmedError(DRILL), "start_unconfirmed", null],
    [new Error(PRIVATE + "\n\u0000" + "x".repeat(400)), "start_failed", null],
  ]) {
    const f = fixture(["a", "b", "c"], { rateLimited: true });
    f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({
      window_start: iso(NOW - WEEK), state: "started", rotation_after: "a", drill_id: DRILL,
    })));
    const begin = f.dependencies.begin;
    let fail = true;
    f.dependencies.begin = async (...args) => {
      if (fail) { f.calls.push(args); throw error; }
      return begin(...args);
    };
    await tick(f);
    assert.equal(f.kv.record().state, state);
    assert.equal(f.kv.record().reason, reason);
    assert.equal(f.kv.putAttempts.length, 3, "fast outcome retries once after the rate limit");
    assert.equal(f.kv.puts.length, 2);
    assert.equal(f.kv.record().rotation_after, state === "pending" ? "a" : "b");
    if (state === "start_unconfirmed") assert.equal(f.kv.record().drill_id, DRILL);
    if (state === "start_failed") {
      assert.match(f.kv.record().error, /^[\x20-\x7e]{1,300}$/);
      assert.equal(JSON.stringify(f.kv.record()).includes(PRIVATE), false, "exception details must not enter persisted scheduler diagnostics");
    }
    fail = false;
    await tick(f, state === "pending" ? NOW + 300_000 : NOW + WEEK);
    assert.equal(f.calls.at(-1)[1].account_id, state === "pending" ? "b" : "c");
    assert.equal(f.kv.record().state, "started");
  }
  const f = fixture(["a", "b"]);
  f.kv.rejectPut = () => f.kv.putAttempts.length > 1;
  await tick(f);
  assert.equal(f.kv.putAttempts.length, 3);
  assert.equal(f.kv.puts.length, 1);
  assert.equal(f.kv.record().state, "starting");
  assert.ok(logs.includes("restore-drill-schedule: state=starting reason=outcome_not_recorded"));
  delete f.kv.rejectPut;
  await tick(f, NOW + 300_000);
  assert.equal(f.kv.record().state, "start_unconfirmed");
  assert.equal(f.calls.length, 1);
  assert.equal(f.kv.putAttempts.length, 1);
});

test("a stored starting record never retries", async (t) => {
  captureLogs(t);
  const f = fixture();
  f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({ state: "starting", account_id: "a", backup_id: catalog("a").backup_id, rotation_after: "a" })));
  await tick(f);
  assert.equal(f.kv.record().state, "start_unconfirmed");
  assert.equal(f.kv.puts.length, 1);
  await tick(f, NOW + 300_000);
  assert.equal(f.kv.putAttempts.length, 0);
  assert.equal(f.calls.length, 0);
});

test("the start is alarm-driven on the newest valid committed backup", async (t) => {
  captureLogs(t);
  const f = fixture();
  const newest = catalog("a", NOW - 60_000);
  f.dependencies.backupStatus = async () => ({ ...backupStatus("a"), backups: {
    ...backupStatus("a").backups, catalog: [catalog("a", NOW - 120_000), newest, { ...catalog("a", NOW), size: 0 }],
  } });
  await tick(f);
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0][0], f.env);
  assert.deepEqual(f.calls[0][1], { account_id: "a", backup_id: newest.backup_id, target_cell: TARGET });
  assert.deepEqual(f.calls[0][2], { origin: f.env.CP_PUBLIC_ORIGIN, driver: "auto", requireAlarm: true });
  assert.equal(f.kv.record().backup_id, newest.backup_id);
  assert.equal(f.kv.record().drill_id, DRILL);
  assert.equal(f.kv.record().started_at, iso());
});

test("the schedule record and logs carry no ids outside the record", async (t) => {
  const logs = captureLogs(t);
  const accountID = "private_fixture_account";
  const f = fixture([accountID]);
  for (let i = 0; i < 11; i++) await tick(f, NOW + WEEK * i);
  const record = f.kv.record();
  assert.deepEqual(Object.keys(record).sort(), scheduleKeys);
  assert.equal(record.history.length, 8);
  assert.deepEqual(record.history.map(({ window_start }) => window_start), Array.from({ length: 8 }, (_, i) => iso(NOW + (9 - i) * WEEK)));
  for (const history of record.history) {
    assert.deepEqual(Object.keys(history).sort(), ["drill_id", "state", "window_start"]);
    assert.equal(history.state, "started");
    assert.equal(history.drill_id, DRILL);
  }
  const serializedLogs = JSON.stringify(logs);
  for (const value of [accountID, catalog(accountID).backup_id, DRILL, TARGET, PRIVATE]) assert.equal(serializedLogs.includes(value), false);
  assert.ok(logs.every((line) => /^restore-drill-schedule: state=[a-z_]+ reason=(?:[a-z_]+|none)$/.test(line)));
  assert.equal(f.kv.values.has(RESTORE_DRILL_FRESHNESS_KEY), false);
  const response = await worker.fetch(new Request("https://cp.test.invalid/v1/backups/status"), { ...f.env, FLEET_TOKEN: "fixture-fleet-token" }, {});
  assert.equal(response.status, 401);
  assert.equal((await response.text()).includes(accountID), false);
});

test("restore drill schedule docs and config stay dark", async () => {
  const root = new URL("../../../../", import.meta.url);
  const read = async (path) => (await readFile(new URL(path, root), "utf8")).replace(/\s+/g, " ");
  const runbook = await read("docs/runbooks.md");
  for (const secret of ["CP_RESTORE_DRILLS_ENABLED", "CP_RESTORE_DRILLS_TARGET_CELL", "CP_RESTORE_DRILLS_WEEKDAY", "CP_RESTORE_DRILLS_START_HOUR_UTC"]) assert.ok(runbook.includes(secret), secret);
  assert.ok(runbook.includes("92 minutes"));
  const backup = await read("docs/backup-and-recovery.md");
  assert.ok(backup.includes("Scheduled restore drills (off by default)"));
  for (const reason of ["backup_scan_incomplete", "freshness_unavailable", "drill_running", "target_unavailable", "candidates_exhausted_this_tick", "window_closed"]) assert.ok(backup.includes(reason), reason);
  assert.ok((await read("docs/api-routes.md")).includes("restore_drill_schedule"));
  assert.equal((await readFile(new URL("../wrangler.template.jsonc", import.meta.url), "utf8")).includes("CP_RESTORE_DRILLS_"), false);
});

test("the default lifecycle and status readers use the exact internal requests", async (t) => {
  captureLogs(t);
  const f = fixture(["a", "b"]);
  const lifecycleRequests = [];
  const statusRequests = [];
  f.env.ACCOUNT_LIFECYCLE = {
    idFromName: (id) => id,
    get: (id) => ({ fetch: async (request) => {
      const body = await request.text();
      lifecycleRequests.push({ id, method: request.method, url: request.url, body });
      if (body !== JSON.stringify({ account_id: id })) return new Response("invalid", { status: 400 });
      return Response.json({ ok: true, account_id: id, status: {
        account_id: id, initialized: true, operation: id === "a" ? { kind: "move" } : null,
      } });
    } }),
  };
  f.env.ACCOUNT_BACKUP = {
    idFromName: (id) => id,
    get: (id) => ({ fetch: async (request) => {
      statusRequests.push({ id, method: request.method, url: request.url });
      return Response.json(backupStatus(id));
    } }),
  };
  await runScheduledRestoreDrill(f.env, NOW, { begin: f.dependencies.begin, now: () => NOW });
  assert.deepEqual(lifecycleRequests, ["a", "b"].map((id) => ({ id, method: "POST", url: "https://account-lifecycle.internal/lifecycle-status", body: JSON.stringify({ account_id: id }) })));
  assert.ok(statusRequests.length >= 1);
  for (const request of statusRequests) assert.deepEqual(request, { id: request.id, method: "GET", url: "http://account-backup.internal/status" });
  assert.deepEqual(f.calls.map(([, input]) => input.account_id), ["b"]);
  for (const invalid of [
    { ok: false, account_id: "a", status: { account_id: "a", initialized: true, operation: null } },
    { ok: true, account_id: "other", status: { account_id: "a", initialized: true, operation: null } },
    { ok: true, account_id: "a", status: [] },
    { ok: true, account_id: "a", status: { account_id: "other", initialized: true, operation: null } },
    { ok: true, account_id: "a", status: { account_id: "a", initialized: "true", operation: null } },
  ]) {
    const g = fixture();
    g.env.ACCOUNT_LIFECYCLE = { idFromName: (id) => id, get: () => ({ fetch: async () => Response.json(invalid) }) };
    g.env.ACCOUNT_BACKUP = f.env.ACCOUNT_BACKUP;
    await runScheduledRestoreDrill(g.env, NOW, { begin: g.dependencies.begin, now: () => NOW });
    assert.equal(g.calls.length, 0);
    assert.equal(g.kv.record().state, "no_eligible_account");
  }
});

test("a tick is bounded and never rejects", async (t) => {
  const logs = captureLogs(t);
  const prior = { rotation_after: "b", history: [{ window_start: iso(NOW - WEEK), state: "started", drill_id: DRILL }] };
  for (const invalid of [JSON.stringify(storedRecord({ ...prior, extra: true })), JSON.stringify(storedRecord({ ...prior, schema_version: "wrong" })), "{not json"]) {
    const f = fixture(["a", "b", "c"]);
    f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, invalid);
    await tick(f);
    assert.deepEqual(f.kv.record().history, []);
    assert.equal(f.calls[0][1].account_id, "a", "invalid prior rotation must be discarded");
  }
  const throwing = fixture();
  throwing.kv.list = async () => { throw new Error(PRIVATE); };
  const before = logs.length;
  assert.deepEqual(await tick(throwing), { ran: true, succeeded: false });
  assert.deepEqual(logs.slice(before), ["restore-drill-schedule: tick failed"]);
  assert.equal(throwing.kv.putAttempts.length, 0);
  const capped = fixture();
  capped.kv.list = async ({ prefix, limit, cursor }) => {
    assert.equal(prefix, "acct:");
    assert.equal(limit, 1000);
    capped.kv.lists.push({ cursor });
    assert.ok(capped.kv.lists.length <= 11, "a removed cap must fail without an infinite test");
    return { keys: Array.from({ length: 1000 }, (_, i) => ({ name: `acct:key_${capped.kv.lists.length}_${i}` })),
      list_complete: false, cursor: `opaque-${capped.kv.lists.length}` };
  };
  await tick(capped);
  assert.equal(capped.kv.lists.length, 10);
  assert.ok(logs.includes("restore-drill-schedule: directory listing capped"));
  assert.equal(capped.kv.gets.filter(({ key }) => key.startsWith("acct:")).length, 3);
  const pages = fixture(Array.from({ length: 1001 }, (_, i) => `account_${String(i).padStart(4, "0")}`));
  await tick(pages);
  assert.equal(pages.kv.lists.length, 2);
  assert.equal(pages.kv.lists[1].cursor, "opaque-page:1000");
});

test("schedule reads are bounded and independent guard and candidate reads run in parallel", { timeout: 10_000 }, async (t) => {
  const logs = captureLogs(t);
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  for (const hang of ["schedule", "guards", "freshness", "candidate", "list"]) {
    const f = fixture();
    const get = f.kv.get.bind(f.kv);
    const called = [];
    const never = () => new Promise(() => {});
    f.env.CP_ACCOUNT_BACKUPS_ENABLED = "true";
    f.kv.values.set(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(completeScan()));
    f.kv.get = (key, options) => {
      called.push(key);
      if (hang === "schedule" && key === RESTORE_DRILL_SCHEDULE_KEY ||
          hang === "guards" && [ACCOUNT_BACKUP_SCAN_KEY, RESTORE_DRILL_FRESHNESS_KEY].includes(key) ||
          hang === "freshness" && key === RESTORE_DRILL_FRESHNESS_KEY ||
          hang === "candidate" && ["acct:a", "pending:a", "archived:a"].includes(key)) return never();
      return get(key, options);
    };
    if (hang === "candidate") {
      f.dependencies.lifecycleStatus = () => { called.push("lifecycle"); return never(); };
      f.dependencies.backupStatus = () => { called.push("backup"); return never(); };
    }
    if (hang === "list") f.kv.list = () => never();
    let settled = false;
    const result = tick(f).then((value) => { settled = true; return value; });
    await flush();
    assert.equal(settled, false, hang);
    if (hang === "guards") {
      assert.ok(called.includes(ACCOUNT_BACKUP_SCAN_KEY));
      assert.ok(called.includes(RESTORE_DRILL_FRESHNESS_KEY), "freshness must start while scan is hung");
    }
    if (hang === "candidate") {
      for (const key of ["acct:a", "pending:a", "archived:a", "lifecycle", "backup"]) assert.ok(called.includes(key), `${key} starts before another candidate read resolves`);
    }
    t.mock.timers.tick(4999);
    await flush();
    assert.equal(settled, false, hang);
    t.mock.timers.tick(1);
    await flush();
    assert.equal(settled, true, `${hang} settles at the 5-second I/O bound`);
    await result;
    assert.equal(f.calls.length, 0);
    if (hang === "guards") assert.equal(f.kv.record().reason, "backup_scan_incomplete");
    if (hang === "freshness") assert.equal(f.kv.record().reason, "freshness_unavailable");
    if (hang === "candidate") assert.equal(f.kv.record().state, "no_eligible_account");
  }
  assert.equal(JSON.stringify(logs).includes(PRIVATE), false);
});

test("schedule writes settle before starting a drill or returning its outcome", async (t) => {
  captureLogs(t);
  const f = fixture();
  const put = f.kv.put.bind(f.kv);
  const releases = [];
  f.kv.put = (key, value) => new Promise((resolve, reject) => {
    releases.push(() => put(key, value).then(resolve, reject));
  });
  let settled = false;
  const result = tick(f).then((value) => { settled = true; return value; });
  await flush();
  assert.equal(releases.length, 1);
  assert.equal(f.calls.length, 0, "the starting write must settle before begin");
  assert.equal(settled, false);
  await releases[0]();
  await flush();
  assert.equal(releases.length, 2);
  assert.equal(f.calls.length, 1);
  assert.equal(settled, false, "the outcome write must settle before the tick returns");
  await releases[1]();
  await result;
  assert.equal(f.kv.record().state, "started");
  assert.equal(f.kv.puts.length, 2);
});

test("a hung outcome reread is bounded and cannot overwrite the starting fence", async (t) => {
  captureLogs(t);
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  const f = fixture();
  const get = f.kv.get.bind(f.kv);
  let scheduleReads = 0;
  f.kv.get = (key, options) => {
    if (key === RESTORE_DRILL_SCHEDULE_KEY && ++scheduleReads > 1) return new Promise(() => {});
    return get(key, options);
  };
  let settled = false;
  const pending = tick(f).then((result) => { settled = true; return result; });
  await flush();
  assert.equal(f.calls.length, 1);
  assert.equal(f.kv.record().state, "starting");
  assert.equal(settled, false);
  for (let attempt = 0; attempt < 2; attempt++) {
    t.mock.timers.tick(4999);
    await flush();
    assert.equal(settled, false);
    t.mock.timers.tick(1);
    await flush();
    assert.equal(settled, attempt === 1, "each outcome reread obeys the existing five-second bound");
  }
  assert.deepEqual(await pending, { ran: true, succeeded: false });
  assert.equal(f.kv.record().state, "starting");
  assert.equal(f.kv.putAttempts.length, 1);
});

test("enabled per-account status keeps the 131a response bytes without reading the fleet schedule", async (t) => {
  captureLogs(t);
  t.mock.timers.enable({ apis: ["Date"], now: NOW });
  const f = fixture();
  f.env.FLEET_TOKEN = "fixture-fleet-token";
  f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(storedRecord({
    account_id: "private_other_account", rotation_after: "private_rotation", examined_after: "private_examined",
  })));
  f.env.ACCOUNT_BACKUP = { idFromName: (id) => id, get: (id) => ({ fetch: async (request) => {
    assert.equal(id, "a");
    assert.equal(new URL(request.url).pathname, "/status");
    return Response.json(backupStatus(id));
  } }) };
  const golden = JSON.stringify(await accountBackupStatus(f.env, "a"));
  f.kv.resetCounters();
  const response = await worker.fetch(requestStatus("a"), f.env, {});
  assert.equal(response.status, 200);
  const body = await response.text();
  assert.equal(body, golden);
  assert.equal(Object.hasOwn(JSON.parse(body), "restore_drill_schedule"), false);
  assert.equal(body.includes("private_other_account"), false);
  assert.equal(f.kv.gets.some(({ key }) => key === RESTORE_DRILL_SCHEDULE_KEY), false);
});

test("enabled fleet status isolates a failed or hung schedule read and reads in parallel", { timeout: 10_000 }, async (t) => {
  captureLogs(t);
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
  for (const kind of ["absent", "unparsable", "stored", "rejected", "hung"]) {
    const f = fixture([]);
    f.env.FLEET_TOKEN = "fixture-fleet-token";
    const record = storedRecord();
    if (kind === "stored") f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, JSON.stringify(record));
    if (kind === "unparsable") f.kv.values.set(RESTORE_DRILL_SCHEDULE_KEY, "{not json");
    const original = f.kv.get.bind(f.kv);
    const reads = [];
    f.kv.get = (key, options) => {
      reads.push(key);
      if (key === RESTORE_DRILL_SCHEDULE_KEY) {
        if (kind === "rejected") throw new Error(PRIVATE);
        if (kind === "hung") return new Promise(() => {});
      }
      return original(key, options);
    };
    let settled = false;
    const pending = worker.fetch(requestStatus(), f.env, {}).then((response) => { settled = true; return response; });
    await flush();
    assert.ok(reads.includes(ACCOUNT_BACKUP_SCAN_KEY));
    assert.ok(reads.includes(RESTORE_DRILL_SCHEDULE_KEY));
    if (kind === "hung") {
      assert.equal(settled, false);
      t.mock.timers.tick(4999);
      await flush();
      assert.equal(settled, false);
      t.mock.timers.tick(1);
      await flush();
      assert.equal(settled, true, "schedule timeout must not hold the status response beyond five seconds");
    }
    const response = await pending;
    assert.equal(response.status, 200);
    const body = await response.json();
    assert.deepEqual(body.restore_drill_schedule, kind === "stored" ? record : null);
    assert.equal(JSON.stringify(body).includes(PRIVATE), false);
  }
  const f = fixture([]);
  f.env.FLEET_TOKEN = "fixture-fleet-token";
  const reads = [];
  let releaseScan;
  f.kv.get = (key) => {
    reads.push(key);
    if (key === ACCOUNT_BACKUP_SCAN_KEY) return new Promise((resolve) => { releaseScan = resolve; });
    return Promise.resolve(null);
  };
  const pending = worker.fetch(requestStatus(), f.env, {});
  await flush();
  assert.ok(reads.includes(RESTORE_DRILL_SCHEDULE_KEY), "schedule read starts while the existing status scan is pending");
  releaseScan(null);
  assert.equal((await pending).status, 200);
});
