import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";
import { uptimeProbeMetricsResponse } from "../src/uptime-probes.mjs";
import { streamToR2Multipart } from "../src/account-lifecycle-runtime.mjs";

import {
  ACCOUNT_BACKUP_SCAN_KEY,
  ACCOUNT_BACKUP_SCAN_SCHEMA,
  ACCOUNT_BACKUP_STATE_SCHEMA,
  IDLE_TIMEOUT_MS,
  OVERALL_TIMEOUT_MS,
  RESTORE_DRILL_DEADLINE_MS,
  RESTORE_DRILL_HEARTBEAT_MS,
  RESTORE_DRILL_ALARM_DEADLINE_MS,
  RESTORE_DRILL_TICK_MS,
  RESTORE_DRILL_MAX_DELAY_MS,
  RESTORE_DRILL_CLAIM_WINDOW_MS,
  RESTORE_DRILL_START_WAIT_SECONDS,
  RESTORE_DRILL_START_TIMEOUT_MS,
  RESTORE_DRILL_POLL_TIMEOUT_MS,
  RESTORE_DRILL_PROBE_TIMEOUT_MS,
  RESTORE_DRILL_MAX_STARTS,
  RESTORE_DRILL_FENCE_RETRIES,
  RESTORE_DRILL_WAIT_POLL_MS,
  RESTORE_DRILL_FINAL_GRACE_MS,
  RESTORE_DRILL_DRIVER_PREFIX,
  RESTORE_DRILL_CELL_JOB_BOUND_MS,
  RestoreDrillProtocolError,
  RestoreDrillUnconfirmedError,
  awaitRestoreDrill,
  BackupValidationBusyError,
  accountBackupHealth,
  accountBackupStatus,
  backupJobIdentity,
  beginAccountBackupValidation,
  DurableAccountBackup,
  heartbeatJSONResponse,
  runAccountBackupValidation,
  runManualAccountBackup,
  runScheduledAccountBackups,
} from "../src/account-backup-runtime.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
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

const ACCOUNT = "acct_backup";
const SOURCE = "aws-us-west-2";
const TARGET = "civo-fixture-target";
const SCHEDULED_AT = Date.parse("2026-07-25T12:34:00.000Z");
const NOW = new Date("2026-07-25T12:35:00.000Z");
const TRAILER_SHA256 = "a".repeat(64);

class Storage {
  constructor() {
    this.values = new Map();
    this.alarm = null;
  }

  async get(key) {
    return this.values.get(key);
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map(
      [...this.values].filter(([key]) => key.startsWith(prefix)),
    );
  }

  async setAlarm(value) {
    this.alarm = value;
  }

  async getAlarm() {
    return this.alarm;
  }

  async deleteAlarm() {
    this.alarm = null;
  }
}

class KV {
  constructor(entries = {}) {
    this.values = new Map(
      Object.entries(entries).map(([key, value]) => [
        key,
        JSON.stringify(value),
      ]),
    );
    this.writes = [];
    this.deletes = [];
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value) {
    this.writes.push(key);
    this.values.set(key, value);
  }

  async delete(key) {
    this.deletes.push(key);
    this.values.delete(key);
  }

  async list({ prefix = "", cursor } = {}) {
    assert.equal(cursor, undefined);
    return {
      keys: [...this.values.keys()]
        .filter((key) => key.startsWith(prefix))
        .map((name) => ({ name })),
      list_complete: true,
    };
  }

  value(key) {
    const value = this.values.get(key);
    return value === undefined ? null : JSON.parse(value);
  }
}

class Bucket {
  constructor() {
    this.values = new Map();
    this.deleted = [];
    this.sequence = 0;
    this.conditionedGets = [];
  }

  write(
    key,
    value = "valid-backup-object",
    customMetadata = {},
    etag = undefined,
  ) {
    const bytes = new TextEncoder().encode(value);
    this.sequence += 1;
    this.values.set(key, {
      bytes,
      customMetadata: structuredClone(customMetadata),
      etag: etag ?? `r2-etag-${this.sequence}`,
    });
  }

  async head(key) {
    const value = this.values.get(key);
    return value
      ? {
          size: value.bytes.byteLength,
          customMetadata: structuredClone(value.customMetadata),
          etag: value.etag,
        }
      : null;
  }

  async get(key, options = undefined) {
    const value = this.values.get(key);
    const etagMatches = options?.onlyIf?.etagMatches;
    if (etagMatches !== undefined) {
      this.conditionedGets.push({ key, etagMatches });
    }
    if (etagMatches !== undefined && value?.etag !== etagMatches) {
      return null;
    }
    return value
      ? { body: new Response(value.bytes).body, size: value.bytes.length, etag: value.etag }
      : null;
  }

  async delete(key) {
    this.deleted.push(key);
    this.values.delete(key);
  }
}

function sourceRoute(epoch = 7) {
  return {
    cell: SOURCE,
    endpoint: "https://source.example",
    region: "us-west",
    region_code: "usw2",
    cell_registration_id: "reg-source",
    epoch,
  };
}

function sourceCell() {
  return {
    endpoint: "https://source.example",
    accepting: true,
    provision_token: "must-never-authorize-backups",
    backup_token: "source-backup-token",
    registration_id: "reg-source",
    registered_at: "2026-07-25T00:00:00.000Z",
  };
}

function objectMetadata(job) {
  return {
    account_id: ACCOUNT,
    backup_id: job.backup_id,
    cell: SOURCE,
    cell_registered_at: "2026-07-25T00:00:00.000Z",
    cell_registration_id: "reg-source",
    scheduled_at: job.scheduled_at,
    route_epoch: "7",
  };
}

function directory(entries = {}) {
  return new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: sourceCell(),
    ...entries,
  });
}

function cellCoordinator(cellProvider) {
  return {
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async (request) => {
        const input = await request.json();
        assert.equal(input.cell_name, id.name);
        const activeCell = typeof cellProvider === "function"
          ? cellProvider(id.name)
          : cellProvider;
        const registrationID =
          activeCell?.registration_id ??
          activeCell?.registered_at ??
          null;
        const active = registrationID === input.registration_id;
        return Response.json({
          ok: true,
          cell_name: id.name,
          expected_registration_id: input.registration_id,
          registration_status: active
            ? "active"
            : registrationID
              ? "replaced"
              : "unknown",
          current_registration_id: registrationID,
          tombstone_registration_id: null,
          active_cell: active ? structuredClone(activeCell) : null,
        });
      },
    }),
  };
}

function projectedCellCoordinator(directoryBinding) {
  return cellCoordinator(
    (name) => directoryBinding.value(`cell:${name}`),
  );
}

function validVerification(job, overrides = {}) {
  const { manifest = {}, ...rest } = overrides;
  return {
    manifest: {
      schema_version: 73,
      account_id: ACCOUNT,
      backup_id: job.backup_id,
      purpose: "backup",
      cell: SOURCE,
      status: "active",
      exported_at: "2026-07-25T12:34:30.000Z",
      ...manifest,
    },
    entries: 4,
    chunks: 2,
    trailer_sha256: TRAILER_SHA256,
    ...rest,
  };
}

function exportResponseHeaders(options) {
  const headers = new Headers(options.headers);
  return {
    "X-Witself-Backup-ID": headers.get("X-Witself-Backup-ID"),
    "X-Witself-Backup-Cell": headers.get("X-Witself-Backup-Cell"),
  };
}

function runtime({
  storage = new Storage(),
  directory: directoryBinding = directory(),
  bucket = new Bucket(),
  fetch,
  streamArchive,
  validateArchive,
  maxAttempts = 3,
} = {}) {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const defaultFetch = async (_url, options) => {
    const headers = new Headers(options.headers);
    assert.equal(
      headers.get("Authorization"),
      "Bearer source-backup-token",
    );
    assert.equal(
      headers.get("X-Witself-Backup-ID"),
      job.backup_id,
    );
    assert.equal(headers.get("X-Witself-Backup-Cell"), SOURCE);
    return new Response("archive", {
      status: 200,
      headers: exportResponseHeaders(options),
    });
  };
  const defaultStream = async (
    binding,
    object,
    _body,
    options,
  ) => {
    binding.write(object, "valid-backup-object", options.customMetadata);
    return (await binding.head(object)).size;
  };
  const instance = new DurableAccountBackup(
    { id: { name: ACCOUNT }, storage },
    {
      DIRECTORY: directoryBinding,
      BACKUPS: bucket,
      CP_ACCOUNT_BACKUPS_CATALOG_LIMIT: "8",
    },
    {
      fetch: fetch ?? defaultFetch,
      streamArchive: streamArchive ?? defaultStream,
      validateArchive:
        validateArchive ?? (() => validVerification(job)),
      now: () => new Date(NOW),
    },
  );
  const request = (path = "/run", overrides = {}) =>
    new Request(`http://account-backup.internal${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...job,
        max_attempts: maxAttempts,
        catalog_limit: 8,
        ...overrides,
      }),
    });
  return {
    instance,
    storage,
    directory: directoryBinding,
    bucket,
    job,
    request,
  };
}

async function responseBody(response) {
  return response.json();
}

test("export stamps the registered source name instead of the cell's environment name", async () => {
  const configuredCell = "runtime-local-cell";
  let manifestCell;
  const harness = runtime({
    fetch: async (_url, options) => {
      const headers = new Headers(options.headers);
      const requestedCell = headers.get("X-Witself-Backup-Cell");
      assert.equal(requestedCell, SOURCE);
      assert.notEqual(requestedCell, configuredCell);
      // Model a cell that stamps the requested name into its archive. The
      // synthetic payload is stored and read back through verifyObject below.
      const verification = validVerification(harness.job, {
        manifest: { cell: requestedCell ?? configuredCell },
      });
      return new Response(JSON.stringify(verification), {
        headers: exportResponseHeaders(options),
      });
    },
    streamArchive: async (bucket, object, body, options) => {
      bucket.write(object, await new Response(body).text(), options.customMetadata);
      return (await bucket.head(object)).size;
    },
    validateArchive: async (bucket, object) => {
      const verification = await new Response((await bucket.get(object)).body).json();
      manifestCell = verification.manifest.cell;
      return verification;
    },
  });
  const response = await harness.instance.fetch(harness.request());
  assert.equal(response.status, 200);
  assert.equal((await response.json()).status, "committed");
  const state = harness.storage.values.get("account-backups");
  assert.equal(manifestCell, state.current_job.source_cell);
  assert.equal(state.catalog[0].source_cell, manifestCell);
});

for (const echo of [null, "wrong-cell"]) {
  test(`a ${echo === null ? "missing" : "mismatched"} source cell echo cancels export and retries`, async () => {
    let cancelled = false;
    let streams = 0;
    let verified = 0;
    const harness = runtime({
      fetch: async (_url, options) => {
        assert.equal(new Headers(options.headers).get("X-Witself-Backup-Cell"), SOURCE);
        const headers = exportResponseHeaders(options);
        if (echo === null) delete headers["X-Witself-Backup-Cell"];
        else headers["X-Witself-Backup-Cell"] = echo;
        return new Response(new ReadableStream({
          cancel() { cancelled = true; },
        }), { headers });
      },
      streamArchive: async () => { streams += 1; assert.fail("unacknowledged export uploaded"); },
      validateArchive: async () => { verified += 1; assert.fail("unacknowledged export verified"); },
    });
    const response = await harness.instance.fetch(harness.request());
    assert.equal(response.status, 202);
    assert.equal((await response.json()).status, "retrying");
    assert.equal(cancelled, true);
    assert.equal(streams, 0);
    assert.equal(verified, 0);
    const state = harness.storage.values.get("account-backups");
    assert.equal(state.current_job.attempts, 1);
    assert.equal(state.current_job.last_error,
      "backup export did not acknowledge the exact source cell");
    assert.notEqual(harness.storage.alarm, null);
    assert.deepEqual(state.catalog, []);
    assert.equal(harness.bucket.values.size, 0);
  });
}

function manualEnv(h, fetch = (request) => h.instance.fetch(request)) {
  return {
    DIRECTORY: h.directory,
    BACKUPS: h.bucket,
    FLEET_TOKEN: "fleet-backup-test-token",
    ACCOUNT_BACKUP: {
      idFromName: (name) => ({ name }),
      get: (id) => {
        assert.equal(id.name, ACCOUNT);
        return { fetch: (request) => {
          assert.equal(new URL(request.url).pathname, "/start");
          return fetch(request);
        } };
      },
    },
  };
}

function manualRoute(env, body) {
  return worker.fetch(new Request("https://cp.test.invalid/v1/backups:run", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${env.FLEET_TOKEN}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  }), env, { waitUntil() {} });
}

test("start acknowledges a pending job before export and a cold alarm commits it", async () => {
  const h = runtime({ fetch: () => assert.fail("start must not export inline") });
  const response = await h.instance.fetch(h.request("/start"));
  assert.equal(response.status, 202);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0", account_id: ACCOUNT,
    backup_id: h.job.backup_id, status: "accepted", scheduled_at: h.job.scheduled_at,
  });
  const state = h.storage.values.get("account-backups");
  assert.equal(state.revision, 1);
  assert.deepEqual(state.catalog, []);
  assert.deepEqual(state.current_job, {
    ...h.job, status: "pending", attempts: 0, max_attempts: 3,
    created_at: NOW.toISOString(), source_cell: SOURCE,
    source_endpoint: "https://source.example", source_registration_id: "reg-source",
    source_registered_at: sourceCell().registered_at, source_route: sourceRoute(),
  });
  assert.equal(h.storage.alarm, NOW.getTime());
  const cold = runtime({ storage: h.storage, directory: h.directory, bucket: h.bucket });
  h.storage.alarm = null; // The platform consumes the alarm before invoking it.
  assert.equal((await cold.instance.alarm()).status, "committed");
  assert.equal(h.storage.values.get("account-backups").current_job.attempts, 1);
  assert.equal(h.storage.values.get("account-backups").catalog[0].backup_id, h.job.backup_id);
  assert.equal(h.storage.alarm, null);
  const committedState = structuredClone(h.storage.values.get("account-backups"));
  const repeated = await cold.instance.fetch(h.request("/start"));
  assert.equal(repeated.status, 200);
  const committed = await repeated.json();
  assert.equal(committed.status, "committed");
  assert.deepEqual(committed.backup, committedState.catalog[0]);
  assert.deepEqual(h.storage.values.get("account-backups"), committedState);
  assert.equal(h.storage.alarm, null);
});

test("repeated start preserves one job and only replaces a missing or unreadable alarm", async () => {
  const h = runtime();
  await h.instance.fetch(h.request("/start"));
  const original = structuredClone(h.storage.values.get("account-backups"));
  let writes = 0;
  const setAlarm = h.storage.setAlarm.bind(h.storage);
  h.storage.setAlarm = async (at) => { writes++; await setAlarm(at); };
  h.storage.alarm = NOW.getTime() + 120_000;
  for (let n = 0; n < 2; n++) {
    const response = await h.instance.fetch(h.request("/start", { max_attempts: 10 }));
    assert.equal(response.status, 202);
    assert.equal((await response.json()).status, "accepted");
  }
  assert.equal(writes, 0);
  assert.equal(h.storage.alarm, NOW.getTime() + 120_000);
  h.storage.alarm = null;
  assert.equal((await h.instance.fetch(h.request("/start"))).status, 202);
  assert.equal(writes, 1);
  assert.equal(h.storage.alarm, NOW.getTime());
  h.storage.getAlarm = undefined;
  assert.equal((await h.instance.fetch(h.request("/start"))).status, 202);
  assert.equal(writes, 2);
  assert.deepEqual(h.storage.values.get("account-backups"), original);
});

for (const status of ["pending", "running", "retrying"]) {
  test(`start acknowledges another ${status} generation as busy and arms its retry`, async () => {
    const h = runtime();
    await h.instance.fetch(h.request("/start"));
    const state = h.storage.values.get("account-backups");
    state.current_job.status = status;
    state.current_job.attempts = status === "pending" ? 0 : 1;
    if (status === "retrying") state.current_job.retry_at = new Date(NOW.getTime() + 180_000).toISOString();
    await h.instance.saveState(state);
    const before = structuredClone(state);
    h.storage.alarm = null;
    const other = backupJobIdentity(ACCOUNT, SCHEDULED_AT + 60_000, 1);
    const response = await h.instance.fetch(h.request("/start", other));
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), {
      schema_version: "witself.v0", account_id: ACCOUNT,
      backup_id: other.backup_id, status: "busy", current_backup_id: h.job.backup_id,
    });
    assert.equal(h.storage.alarm, NOW.getTime() + (status === "retrying" ? 180_000 : status === "running" ? 60_000 : 0));
    // An already-armed executor alarm is never pushed later by a busy answer.
    h.storage.alarm = NOW.getTime() + 5_000;
    const again = await h.instance.fetch(h.request("/start", other));
    assert.equal(again.status, 200);
    assert.equal((await again.json()).status, "busy");
    assert.equal(h.storage.alarm, NOW.getTime() + 5_000);
    assert.deepEqual(h.storage.values.get("account-backups"), before);
    assert.deepEqual(h.storage.values.get("account-backups"), before);
    const repeated = await h.instance.fetch(h.request("/start"));
    assert.equal(repeated.status, 202);
    assert.equal((await repeated.json()).status, "accepted");
    assert.deepEqual(h.storage.values.get("account-backups"), before);
  });
}

test("start reports busy or accepted while a slow run holds the fence", { timeout: 10_000 }, async () => {
  let release;
  let started;
  const blocked = new Promise((resolve) => { release = resolve; });
  const exporting = new Promise((resolve) => { started = resolve; });
  const h = runtime({ fetch: async (_url, options) => {
    started();
    await blocked;
    return new Response("archive", { headers: exportResponseHeaders(options) });
  } });
  const running = h.instance.fetch(h.request());
  try {
    await exporting;
    const before = structuredClone(h.storage.values.get("account-backups"));
    const alarm = h.storage.alarm;
    const other = backupJobIdentity(ACCOUNT, SCHEDULED_AT + 60_000, 1);
    const busy = await h.instance.fetch(h.request("/start", other));
    assert.equal(busy.status, 200);
    assert.equal((await busy.json()).current_backup_id, h.job.backup_id);
    const accepted = await h.instance.fetch(h.request("/start"));
    assert.equal(accepted.status, 202);
    assert.equal((await accepted.json()).status, "accepted");
    // Public requests must also acknowledge the persisted job under contention.
    for (const [job, status] of [[other, 200], [h.job, 202]]) {
      const response = await manualRoute(manualEnv(h), { account_id: ACCOUNT, scheduled_at: job.scheduled_at });
      assert.equal(response.status, status);
    }
    assert.deepEqual(h.storage.values.get("account-backups"), before);
    assert.equal(h.storage.alarm, alarm);
  } finally {
    release();
    assert.equal((await running).status, 200);
  }
});

test("start fails closed while the fenced operation has no persisted job yet", async () => {
  const h = runtime();
  await h.instance.fence.run(async () => {
    const response = await h.instance.fetch(h.request("/start"));
    assert.equal(response.status, 503);
    assert.equal(h.storage.values.size, 0);
    assert.equal(h.storage.alarm, null);
  });
});

test("start shares run input validation and never mutates on invalid input", async () => {
  const h = runtime();
  for (const overrides of [
    { account_id: "acct_other" }, { backup_id: "backup_invalid" },
    { object: "accounts/other/archive" }, { scheduled_at: "bad-date" },
    { max_attempts: 0 }, { catalog_limit: 0 },
  ]) {
    for (const path of ["/run", "/start"]) {
      assert.equal((await h.instance.fetch(h.request(path, overrides))).status, 400);
    }
  }
  assert.equal((await h.instance.fetch(new Request("http://account-backup.internal/start", {
    method: "POST", body: "{",
  }))).status, 400);
  assert.equal(h.storage.values.size, 0);
  assert.equal(h.storage.alarm, null);
});

test("manual dispatch and public route return accepted, busy, committed and failed acknowledgements", async () => {
  const h = runtime();
  const env = manualEnv(h);
  const accepted = await runManualAccountBackup(env, ACCOUNT, SCHEDULED_AT);
  assert.deepEqual(accepted, {
    schema_version: "witself.v0", account_id: ACCOUNT, backup_id: h.job.backup_id,
    status: "accepted", scheduled_at: h.job.scheduled_at,
  });
  const body = { account_id: ACCOUNT, scheduled_at: h.job.scheduled_at };
  const response = await manualRoute(env, body);
  assert.equal(response.status, 202);
  assert.deepEqual(await response.json(), accepted);
  const otherTime = SCHEDULED_AT + 60_000;
  const busy = await runManualAccountBackup(env, ACCOUNT, otherTime);
  assert.equal(busy.status, "busy");
  assert.equal(busy.current_backup_id, h.job.backup_id);
  const busyResponse = await manualRoute(env, { ...body, scheduled_at: new Date(otherTime).toISOString() });
  assert.equal(busyResponse.status, 200);
  assert.deepEqual(await busyResponse.json(), busy);
  await h.instance.alarm();
  const committed = await runManualAccountBackup(env, ACCOUNT, SCHEDULED_AT);
  assert.equal(committed.status, "committed");
  const committedResponse = await manualRoute(env, body);
  assert.equal(committedResponse.status, 200);
  assert.deepEqual(await committedResponse.json(), committed);

  const failed = runtime({ maxAttempts: 1, fetch: async () => new Response("unavailable", { status: 503 }) });
  await failed.instance.fetch(failed.request("/start"));
  await failed.instance.alarm();
  const failedState = structuredClone(failed.storage.values.get("account-backups"));
  const failedBody = await runManualAccountBackup(manualEnv(failed), ACCOUNT, SCHEDULED_AT);
  assert.equal(failedBody.status, "failed");
  assert.equal(failedBody.attempts, 1);
  const failedResponse = await manualRoute(manualEnv(failed), body);
  assert.equal(failedResponse.status, 200);
  assert.deepEqual(await failedResponse.json(), failedBody);
  assert.deepEqual(failed.storage.values.get("account-backups"), failedState);
  assert.equal(failed.storage.alarm, null);
});

test("manual dispatch rejects malformed, mismatched and unsuccessful acknowledgements", async () => {
  const h = runtime();
  const valid = {
    schema_version: "witself.v0", account_id: ACCOUNT, backup_id: h.job.backup_id,
    status: "accepted", scheduled_at: h.job.scheduled_at,
  };
  for (const body of [null, {}, { ...valid, schema_version: "other" },
    { ...valid, account_id: "other" }, { ...valid, backup_id: "other" },
    { ...valid, status: "retrying" }, { ...valid, status: "unknown" }]) {
    await assert.rejects(runManualAccountBackup(manualEnv(h, () => Response.json(body)), ACCOUNT, SCHEDULED_AT), /not acknowledged/);
  }
  for (const response of [new Response("not-json"), Response.json(valid, { status: 500 })]) {
    await assert.rejects(runManualAccountBackup(manualEnv(h, () => response), ACCOUNT, SCHEDULED_AT), /not acknowledged/);
  }
});

test("manual public route preserves account and scheduled-time validation", async () => {
  const h = runtime();
  const env = manualEnv(h, () => assert.fail("invalid route input must not dispatch"));
  for (const body of [{ account_id: "bad account" }, { account_id: ACCOUNT, scheduled_at: "invalid" }]) {
    assert.equal((await manualRoute(env, body)).status, 400);
  }
});

test("scheduled backups are disabled by default without touching bindings", async () => {
  const env = new Proxy({}, {
    get(_target, property) {
      if (
        typeof property === "string" &&
        property.startsWith("CP_ACCOUNT_BACKUPS_")
      ) {
        return undefined;
      }
      throw new Error(`unexpected binding read: ${String(property)}`);
    },
  });
  assert.deepEqual(
    await runScheduledAccountBackups(env, SCHEDULED_AT),
    { ran: false, configured: true },
  );
});

test("scheduled scan advances one durable cursor page with bounded concurrency", async () => {
  const directoryBinding = new KV();
  const cursors = [];
  const dispatched = [];
  let active = 0;
  let maximumActive = 0;
  const pages = new Map([
    [undefined, {
      account_ids: ["acct_a", "acct_b", "acct_c", "acct_d"],
      next_cursor: "page-2",
    }],
    ["page-2", {
      account_ids: ["acct_e", "acct_f"],
      next_cursor: null,
    }],
  ]);
  const env = {
    CP_ACCOUNT_BACKUPS_ENABLED: "true",
    CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES: "1440",
    CP_ACCOUNT_BACKUPS_PAGE_SIZE: "4",
    CP_ACCOUNT_BACKUPS_CONCURRENCY: "2",
    DIRECTORY: directoryBinding,
    ACCOUNT_BACKUP: {},
    BACKUPS: {},
  };
  const dependencies = {
    activeAccountPage: async (_env, limit, cursor) => {
      assert.equal(limit, 4);
      cursors.push(cursor);
      return pages.get(cursor);
    },
    dispatch: async (_env, job) => {
      active += 1;
      maximumActive = Math.max(maximumActive, active);
      await new Promise((resolve) => setTimeout(resolve, 2));
      dispatched.push(job);
      active -= 1;
      return {
        account_id: job.account_id,
        accepted: true,
        status: "committed",
      };
    },
  };

  const first = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    dependencies,
  );
  const second = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    dependencies,
  );
  const third = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    dependencies,
  );

  assert.deepEqual(cursors, [undefined, "page-2"]);
  assert.equal(first.complete, false);
  assert.equal(first.scanned, 4);
  assert.equal(first.accepted, 4);
  assert.equal(first.failed, 0);
  assert.equal(second.complete, true);
  assert.equal(second.scanned, 6);
  assert.equal(second.accepted, 6);
  assert.equal(second.failed, 0);
  assert.equal(third.ran, false);
  assert.equal(third.complete, true);
  assert.equal(third.scanned, 6);
  assert.equal(third.accepted, 6);
  assert.equal(third.failed, 0);
  assert.equal(maximumActive, 2);
  assert.equal(dispatched.length, 6);
  assert.equal(
    new Set(dispatched.map((job) => job.backup_id)).size,
    1,
  );
  assert.equal(
    directoryBinding.value(ACCOUNT_BACKUP_SCAN_KEY).complete,
    true,
  );
});

test("scheduled scan preserves bounded failures across later successful pages", async () => {
  const directoryBinding = new KV();
  const failedAccounts = Array.from(
    { length: 18 },
    (_, index) => `acct_failed_${String(index).padStart(2, "0")}`,
  );
  const pages = new Map([
    [undefined, {
      account_ids: failedAccounts,
      next_cursor: "success-page",
    }],
    ["success-page", {
      account_ids: ["acct_success"],
      next_cursor: null,
    }],
  ]);
  const env = {
    CP_ACCOUNT_BACKUPS_ENABLED: "true",
    CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES: "1440",
    CP_ACCOUNT_BACKUPS_PAGE_SIZE: "20",
    CP_ACCOUNT_BACKUPS_CONCURRENCY: "5",
    DIRECTORY: directoryBinding,
    ACCOUNT_BACKUP: {},
    BACKUPS: {},
  };
  const firstSlot = backupJobIdentity(
    "slot",
    SCHEDULED_AT,
    1440,
  ).scheduled_at;
  const dependencies = {
    activeAccountPage: async (_env, _limit, cursor) => pages.get(cursor),
    dispatch: async (_env, job) => {
      const failed = job.scheduled_at === firstSlot &&
        job.account_id !== "acct_success";
      return {
        account_id: job.account_id,
        accepted: !failed,
        status: failed
          ? "UPSTREAM 503\r\nuntrusted detail"
          : "committed",
      };
    },
  };

  const first = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    dependencies,
  );
  const second = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    dependencies,
  );
  const finalScan = directoryBinding.value(
    ACCOUNT_BACKUP_SCAN_KEY,
  ACCOUNT_BACKUP_SCAN_SCHEMA,
  );

  assert.equal(first.complete, false);
  assert.equal(first.scanned, 18);
  assert.equal(first.accepted, 0);
  assert.equal(first.failed, 18);
  assert.equal(first.failure_sample.length, 16);
  assert.equal(second.complete, true);
  assert.equal(second.scanned, 19);
  assert.equal(second.accepted, 1);
  assert.equal(second.failed, 18);
  assert.equal(second.failed_retry, "next_interval");
  assert.deepEqual(
    second.failure_sample,
    first.failure_sample,
    "the successful final page cannot erase earlier failures",
  );
  assert.equal(finalScan.failed, 18);
  assert.equal(finalScan.failure_sample.length, 16);
  assert.ok(
    finalScan.failure_sample.every(
      (failure) =>
        failedAccounts.includes(failure.account_id) &&
        failure.status === "upstream_503_untrusted_detail",
    ),
  );

  const nextInterval = SCHEDULED_AT + 24 * 60 * 60 * 1000;
  const retried = await runScheduledAccountBackups(
    env,
    nextInterval,
    dependencies,
  );
  assert.equal(retried.scanned, 18);
  assert.equal(retried.accepted, 18);
  assert.equal(retried.failed, 0);
  assert.deepEqual(retried.failure_sample, []);
});

test("scheduled scan rejects terminal and busy acknowledgements as missing generations", async () => {
  const directoryBinding = new KV();
  const statuses = new Map([
    ["acct_committed", "committed"],
    ["acct_retrying", "retrying"],
    ["acct_failed", "failed"],
    ["acct_busy", "busy"],
  ]);
  const env = {
    CP_ACCOUNT_BACKUPS_ENABLED: "true",
    CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES: "1440",
    DIRECTORY: directoryBinding,
    ACCOUNT_BACKUP: {
      idFromName: (name) => ({ name }),
      get: (id) => ({
        fetch: async (request) => {
          const job = await request.json();
          return Response.json({
            schema_version: "witself.v0",
            account_id: id.name,
            backup_id: job.backup_id,
            status: statuses.get(id.name),
          });
        },
      }),
    },
    BACKUPS: {},
  };

  const result = await runScheduledAccountBackups(
    env,
    SCHEDULED_AT,
    {
      activeAccountPage: async () => ({
        account_ids: [...statuses.keys()],
        next_cursor: null,
      }),
    },
  );

  assert.equal(result.complete, true);
  assert.equal(result.scanned, 4);
  assert.equal(result.accepted, 2);
  assert.equal(result.failed, 2);
  assert.deepEqual(result.failure_sample, [
    { account_id: "acct_failed", status: "failed" },
    { account_id: "acct_busy", status: "busy" },
  ]);
});

test("manifest purpose mismatch never enters the catalog and is removed on retry", async () => {
  const harness = runtime({
    validateArchive: (binding, object, accountID) => {
      assert.ok(binding.values.has(object));
      assert.equal(accountID, ACCOUNT);
      return validVerification(
        backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1),
        { manifest: { purpose: "evacuation" } },
      );
    },
  });

  const first = await harness.instance.fetch(harness.request());
  assert.equal(first.status, 202);
  assert.equal((await responseBody(first)).status, "retrying");
  assert.deepEqual(
    harness.storage.values.get("account-backups").catalog,
    [],
  );

  await harness.instance.alarm();
  assert.deepEqual(harness.bucket.deleted, [harness.job.object]);
  assert.deepEqual(
    harness.storage.values.get("account-backups").catalog,
    [],
  );
});

test("an existing verified object makes an ambiguous upload retry idempotent", async () => {
  let streams = 0;
  let fetches = 0;
  const harness = runtime({
    fetch: async (_url, options) => {
      fetches += 1;
      const headers = new Headers(options.headers);
      assert.equal(
        headers.get("Authorization"),
        "Bearer source-backup-token",
      );
      return new Response("archive", {
        status: 200,
        headers: exportResponseHeaders(options),
      });
    },
    streamArchive: async (binding, object, _body, options) => {
      streams += 1;
      binding.write(
        object,
        "valid-backup-object",
        options.customMetadata,
      );
      throw new Error("lost multipart completion acknowledgement");
    },
  });

  const first = await harness.instance.fetch(harness.request());
  assert.equal(first.status, 202);
  assert.equal((await responseBody(first)).status, "retrying");

  const second = await harness.instance.fetch(harness.request());
  const body = await responseBody(second);
  assert.equal(second.status, 200);
  assert.equal(body.status, "committed");
  assert.equal(body.recovered_existing_object, true);
  assert.equal(fetches, 1);
  assert.equal(streams, 1);
  assert.equal(
    harness.storage.values.get("account-backups").catalog[0]
      .trailer_sha256,
    TRAILER_SHA256,
  );
});

test("a manual run arms its Durable Object alarm before outbound export", async () => {
  const storage = new Storage();
  let observedArmedAlarm = false;
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const harness = runtime({
    storage,
    fetch: async (_url, options) => {
      observedArmedAlarm = storage.alarm !== null;
      return new Response("archive", {
        status: 200,
        headers: exportResponseHeaders(options),
      });
    },
  });

  const response = await harness.instance.fetch(harness.request());
  assert.equal(response.status, 200);
  assert.equal((await responseBody(response)).status, "committed");
  assert.equal(observedArmedAlarm, true);
  assert.equal(storage.alarm, null);
  assert.equal(
    storage.values.get("account-backups").catalog[0].backup_id,
    job.backup_id,
  );
});

test("an early alarm blocked by the active run rearms crash recovery", async () => {
  let releaseExport;
  let exportStarted;
  const started = new Promise((resolve) => {
    exportStarted = resolve;
  });
  const blocked = new Promise((resolve) => {
    releaseExport = resolve;
  });
  const harness = runtime({
    fetch: async (_url, options) => {
      exportStarted();
      await blocked;
      return new Response("archive", {
        status: 200,
        headers: exportResponseHeaders(options),
      });
    },
  });

  const running = harness.instance.fetch(harness.request());
  await started;
  assert.notEqual(harness.storage.alarm, null);

  // Cloudflare consumes the scheduled alarm before invoking alarm(). Mimic
  // that boundary while the original request still owns the account fence.
  harness.storage.alarm = null;
  await harness.instance.alarm();
  const replacementAlarm = harness.storage.alarm;

  releaseExport();
  const response = await running;
  assert.ok(
    replacementAlarm > NOW.getTime(),
    "the busy alarm invocation must install a later recovery alarm",
  );
  assert.equal(response.status, 200);
  assert.equal((await responseBody(response)).status, "committed");
  assert.equal(
    harness.storage.alarm,
    null,
    "the successful original run clears the replacement alarm",
  );
});

test("an existing object with mismatched R2 identity metadata is replaced", async () => {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const bucket = new Bucket();
  bucket.write(
    job.object,
    "valid-backup-object",
    {
      ...objectMetadata(job),
      cell_registration_id: "wrong-registration",
    },
    "untrusted-etag",
  );
  const harness = runtime({ bucket });

  const response = await harness.instance.fetch(harness.request());
  const body = await responseBody(response);
  assert.equal(response.status, 200);
  assert.equal(body.status, "committed");
  assert.deepEqual(bucket.deleted, [job.object]);
  assert.equal(
    harness.storage.values.get("account-backups").catalog[0]
      .source_registration_id,
    "reg-source",
  );
  assert.notEqual(
    harness.storage.values.get("account-backups").catalog[0].r2_etag,
    "untrusted-etag",
  );
});

test("backup retries are bounded and terminal failure clears its alarm", async () => {
  let fetches = 0;
  const harness = runtime({
    maxAttempts: 2,
    fetch: async () => {
      fetches += 1;
      return new Response("temporarily unavailable", { status: 503 });
    },
  });

  const first = await harness.instance.fetch(harness.request());
  assert.equal(first.status, 202);
  assert.equal((await responseBody(first)).status, "retrying");
  assert.notEqual(harness.storage.alarm, null);

  await harness.instance.alarm();
  const state = harness.storage.values.get("account-backups");
  assert.equal(fetches, 2);
  assert.equal(state.current_job.status, "failed");
  assert.equal(state.current_job.attempts, 2);
  assert.equal(state.failures.length, 1);
  assert.equal(harness.storage.alarm, null);
});

test("a changed source fence cannot authorize a verified object", async () => {
  const directoryBinding = directory();
  const harness = runtime({
    directory: directoryBinding,
    validateArchive: async () => {
      await directoryBinding.put(
        `acct:${ACCOUNT}`,
        JSON.stringify(sourceRoute(8)),
      );
      return validVerification(
        backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1),
      );
    },
  });

  const response = await harness.instance.fetch(harness.request());
  assert.equal(response.status, 202);
  assert.equal((await responseBody(response)).status, "retrying");
  assert.deepEqual(
    harness.storage.values.get("account-backups").catalog,
    [],
  );
});

test("source export fails closed when no distinct backup token exists", async () => {
  const withoutBackupToken = sourceCell();
  delete withoutBackupToken.backup_token;
  let fetches = 0;
  const harness = runtime({
    directory: directory({
      [`cell:${SOURCE}`]: withoutBackupToken,
    }),
    fetch: async () => {
      fetches += 1;
      throw new Error("must not be called");
    },
  });

  const response = await harness.instance.fetch(harness.request());
  assert.equal(response.status, 500);
  assert.match(
    (await responseBody(response)).error,
    /source cell is not configured/,
  );
  assert.equal(fetches, 0);
});

function committedRecord(job) {
  return {
    account_id: ACCOUNT,
    backup_id: job.backup_id,
    object: job.object,
    source_cell: SOURCE,
    source_registration_id: "reg-source",
    source_registered_at: "2026-07-25T00:00:00.000Z",
    source_route_epoch: 7,
    scheduled_at: job.scheduled_at,
    exported_at: "2026-07-25T12:34:30.000Z",
    verified_at: "2026-07-25T12:34:45.000Z",
    status: "active",
    size: 19,
    archive_schema_version: 73,
    entries: 4,
    chunks: 2,
    trailer_sha256: TRAILER_SHA256,
    r2_etag: "catalog-etag",
  };
}

function backupNamespace(record, receipts, options = {}) {
  return {
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async (request) => {
        assert.equal(id.name, ACCOUNT);
        const path = new URL(request.url).pathname;
        options.calls?.push(path);
        if (path === "/status") {
          return Response.json({
            schema_version: "witself.v0",
            account_id: ACCOUNT,
            backups: {
              schema_version: ACCOUNT_BACKUP_STATE_SCHEMA,
              account_id: ACCOUNT,
              revision: 1,
              current_job: null,
              catalog: [record],
              failures: [],
            },
          });
        }
        if (path === "/restore-drill:start") {
          assert.deepEqual(await request.json(), { backup_id: record.backup_id, target_cell: TARGET });
          if (options.busy) return Response.json({ schema_version: "witself.v0", error: "restore drill already running", restore_drill: options.busy }, { status: 409 });
          return Response.json({ schema_version: "witself.v0", account_id: ACCOUNT, restore_drill: drillRecord(record) });
        }
        if (path === "/restore-drill:finish") {
          options.finishes?.push(await request.json());
          return Response.json({ schema_version: "witself.v0" }, { status: options.finishStatus ?? 200 });
        }
        if (path === "/archive-capability") {
          assert.deepEqual(await request.json(), { backup_id: record.backup_id, target_cell: TARGET, ttl_seconds: 1800 });
          return Response.json({ token: "cap_" + "b".repeat(64) });
        }
        assert.equal(path, "/validation-verified");
        receipts.push(await request.json());
        return Response.json({
          schema_version: "witself.v0",
          account_id: ACCOUNT,
          backup: record,
        });
      },
    }),
  };
}

test("rollback-only validation uses the target backup token and never changes routing", async () => {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const record = committedRecord(job);
  const receipts = [];
  const directoryBinding = directory({
    [`cell:${TARGET}`]: {
      endpoint: "https://validation.example",
      accepting: false,
      backup_validation_target: true,
      provision_token: "must-never-authorize-validation",
      backup_token: "target-backup-token",
      registration_id: "reg-target",
      registered_at: "2026-07-25T00:00:00.000Z",
    },
  });
  const bucket = new Bucket();
  bucket.write(
    record.object,
    "valid-backup-object",
    objectMetadata(job),
    record.r2_etag,
  );
  assert.equal((await bucket.head(record.object)).size, record.size);
  let validationCalls = 0;
  const env = {
    DIRECTORY: directoryBinding,
    CELL_COORDINATOR: projectedCellCoordinator(directoryBinding),
    ACCOUNT_BACKUP: backupNamespace(record, receipts),
    BACKUPS: bucket,
  };

  const result = await runAccountBackupValidation(
    env,
    {
      account_id: ACCOUNT,
      backup_id: job.backup_id,
      target_cell: TARGET,
    },
    {
      now: () => new Date("2026-07-25T12:36:00.000Z"),
      origin: "https://cp.test.invalid",
      validateArchive: () => validVerification(job),
      fetch: async (url, options) => {
        validationCalls += 1;
        assert.equal(
          url,
          `https://validation.example/v1/accounts/${ACCOUNT}:validate-backup`,
        );
        const headers = new Headers(options.headers);
        assert.equal(
          headers.get("Authorization"),
          "Bearer target-backup-token",
        );
        assert.equal(
          headers.get("X-Witself-Backup-ID"),
          job.backup_id,
        );
        assert.equal(
          headers.get("X-Witself-Backup-Validation"),
          "true",
        );
        assert.equal(options.body, undefined);
        assert.equal(headers.get("X-Witself-Backup-Archive-URL"), `https://cp.test.invalid/v1/backups:archive?account_id=${ACCOUNT}&backup_id=${job.backup_id}`);
        assert.ok(/^cap_[0-9a-f]{64}$/.test(headers.get("X-Witself-Backup-Archive-Token")));
        assert.equal(headers.get("X-Witself-Backup-Archive-Size"), String(record.size));
        return Response.json({
          schema_version: "witself.v0",
          account_id: ACCOUNT,
          backup_id: job.backup_id,
          purpose: "backup",
          status: "active",
          archive_schema_version: 73,
          validated: true,
        });
      },
    },
  );

  assert.equal(validationCalls, 1);
  assert.equal(result.validated, true);
  assert.equal(result.validated_at, "2026-07-25T12:36:00.000Z");
  assert.deepEqual(receipts, [{
    account_id: ACCOUNT,
    backup_id: job.backup_id,
    target_cell: TARGET,
    validated_at: "2026-07-25T12:36:00.000Z",
    status: "active",
    archive_schema_version: 73,
  }]);
  assert.deepEqual(directoryBinding.writes, []);
  assert.deepEqual(directoryBinding.deletes, []);
  assert.deepEqual(bucket.conditionedGets, []);
});

test("backup validation requires the marker, drain, and backup token", async () => {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const record = committedRecord(job);
  const bucket = new Bucket();
  bucket.write(
    record.object,
    "valid-backup-object",
    objectMetadata(job),
    record.r2_etag,
  );

  for (const target of [
    {
      endpoint: "https://validation.example",
      accepting: true,
      backup_validation_target: true,
      backup_token: "target-backup-token",
      registration_id: "reg-target",
      registered_at: "2026-07-25T00:00:00.000Z",
    },
    {
      endpoint: "https://validation.example",
      accepting: false,
      backup_validation_target: true,
      provision_token: "provision-only",
      registration_id: "reg-target",
      registered_at: "2026-07-25T00:00:00.000Z",
    },
    {
      endpoint: "https://validation.example",
      accepting: false,
      backup_validation_target: false,
      backup_token: "target-backup-token",
      registration_id: "reg-target",
      registered_at: "2026-07-25T00:00:00.000Z",
    },
  ]) {
    const directoryBinding = directory({
      [`cell:${TARGET}`]: target,
    });
    const env = {
      DIRECTORY: directoryBinding,
      CELL_COORDINATOR: projectedCellCoordinator(directoryBinding),
      ACCOUNT_BACKUP: backupNamespace(record, []),
      BACKUPS: bucket,
    };
    await assert.rejects(
      runAccountBackupValidation(env, {
        account_id: ACCOUNT,
        backup_id: job.backup_id,
        target_cell: TARGET,
      }),
      /backup_validation_target=true, accepting=false cell with a distinct backup token/,
    );
  }
});

test("backup validation rejects an R2 etag change between reread phases", async () => {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const record = committedRecord(job);
  const receipts = [];
  const bucket = new Bucket();
  bucket.write(
    record.object,
    "valid-backup-object",
    objectMetadata(job),
    record.r2_etag,
  );
  const directoryBinding = directory({
    [`cell:${TARGET}`]: {
      endpoint: "https://validation.example",
      accepting: false,
      backup_validation_target: true,
      provision_token: "provision-token",
      backup_token: "target-backup-token",
      registration_id: "reg-target",
      registered_at: "2026-07-25T00:00:00.000Z",
    },
  });
  const env = {
    DIRECTORY: directoryBinding,
    CELL_COORDINATOR: projectedCellCoordinator(directoryBinding),
    ACCOUNT_BACKUP: backupNamespace(record, receipts),
    BACKUPS: bucket,
  };
  let remoteCalls = 0;

  await assert.rejects(
    runAccountBackupValidation(
      env,
      {
        account_id: ACCOUNT,
        backup_id: job.backup_id,
        target_cell: TARGET,
      },
      {
        origin: "https://cp.test.invalid",
      validateArchive: () => {
          bucket.write(
            record.object,
            "valid-backup-object",
            objectMetadata(job),
            "changed-etag",
          );
          return validVerification(job);
        },
        fetch: async () => {
          remoteCalls += 1;
          return Response.json({});
        },
      },
    ),
    /R2 object identity/,
  );
  assert.equal(remoteCalls, 0);
  assert.deepEqual(receipts, []);
});

test("backup validation rechecks target isolation before recording its receipt", async () => {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const record = committedRecord(job);
  const receipts = [];
  const target = {
    endpoint: "https://validation.example",
    accepting: false,
    backup_validation_target: true,
    provision_token: "provision-token",
    backup_token: "target-backup-token",
    registration_id: "reg-target",
    registered_at: "2026-07-25T00:00:00.000Z",
  };
  const directoryBinding = directory({
    [`cell:${TARGET}`]: target,
  });
  let authoritativeTarget = target;
  const bucket = new Bucket();
  bucket.write(
    record.object,
    "valid-backup-object",
    objectMetadata(job),
    record.r2_etag,
  );
  const env = {
    DIRECTORY: directoryBinding,
    CELL_COORDINATOR: cellCoordinator(
      () => authoritativeTarget,
    ),
    ACCOUNT_BACKUP: backupNamespace(record, receipts),
    BACKUPS: bucket,
  };

  await assert.rejects(
    runAccountBackupValidation(
      env,
      {
        account_id: ACCOUNT,
        backup_id: job.backup_id,
        target_cell: TARGET,
      },
      {
        origin: "https://cp.test.invalid",
      validateArchive: () => validVerification(job),
        fetch: async () => {
          // Leave KV stale and marked while the authoritative cell
          // coordinator observes the same registration reopened/unmarked.
          authoritativeTarget = {
            ...target,
            accepting: true,
            backup_validation_target: false,
          };
          return Response.json({
            schema_version: "witself.v0",
            account_id: ACCOUNT,
            backup_id: job.backup_id,
            purpose: "backup",
            status: "active",
            archive_schema_version: 73,
            validated: true,
          });
        },
      },
    ),
    /backup_validation_target=true, accepting=false cell/,
  );
  assert.equal(
    directoryBinding.value(`cell:${TARGET}`).backup_validation_target,
    true,
  );
  assert.deepEqual(receipts, []);
});

const flush = () => new Promise((resolve) => setImmediate(resolve));

function streamingRuntime() {
  let upstream;
  let signal;
  let cancelled = false;
  let aborted = false;
  const bucket = new Bucket();
  let bytes = 0;
  bucket.createMultipartUpload = async (object, options) => ({
    uploadPart: async (partNumber, body) => {
      bytes += body.byteLength;
      return { partNumber, etag: `part-${partNumber}` };
    },
    complete: async () => bucket.write(object, "x".repeat(bytes), options.customMetadata),
    abort: async () => { aborted = true; },
  });
  const harness = runtime({
    bucket,
    fetch: async (_url, options) => {
      signal = options.signal;
      return new Response(new ReadableStream({
        start(controller) { upstream = controller; },
        cancel() { cancelled = true; },
      }), { headers: exportResponseHeaders(options) });
    },
    // Exercise the real multipart abort/complete path as well as body reads.
    streamArchive: streamToR2Multipart,
  });
  return {
    ...harness,
    get upstream() { return upstream; },
    get signal() { return signal; },
    get cancelled() { return cancelled; },
    get aborted() { return aborted; },
  };
}

test("a cold start alarm retains the export idle watchdog and retry policy", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = streamingRuntime();
  assert.equal((await h.instance.fetch(h.request("/start"))).status, 202);
  assert.equal(h.signal, undefined);
  // Recreate the object with the streaming dependencies but the persisted job.
  const cold = new DurableAccountBackup(
    { id: { name: ACCOUNT }, storage: h.storage }, h.instance.env,
    { fetch: h.instance.fetchImpl, streamArchive: h.instance.streamArchive,
      validateArchive: h.instance.validateArchive, now: h.instance.now },
  );
  h.storage.alarm = null;
  const running = cold.alarm();
  await flush();
  assert.equal(h.storage.values.get("account-backups").current_job.attempts, 1);
  assert.equal(h.signal.aborted, false);
  t.mock.timers.tick(IDLE_TIMEOUT_MS);
  assert.equal((await running).status, "retrying");
  await flush();
  assert.equal(h.signal.aborted, true);
  assert.equal(h.cancelled, true);
  assert.equal(h.aborted, true);
  const state = h.storage.values.get("account-backups");
  assert.equal(state.current_job.last_error, "export_idle_timeout");
  assert.equal(state.current_job.status, "retrying");
  assert.deepEqual(state.catalog, []);
  assert.ok(h.storage.alarm > NOW.getTime());
});

for (const emptyChunks of [false, true]) {
  test(`idle watchdog aborts with no bytes (empty chunks: ${emptyChunks})`, { timeout: 10_000 }, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const h = streamingRuntime();
    const running = h.instance.fetch(h.request());
    await flush();
    t.mock.timers.tick(IDLE_TIMEOUT_MS - 1);
    if (emptyChunks) h.upstream.enqueue(new Uint8Array());
    await flush();
    assert.equal(h.signal.aborted, false);
    t.mock.timers.tick(1);
    assert.equal((await (await running).json()).status, "retrying");
    await flush();
    assert.equal(h.storage.values.get("account-backups").current_job.last_error, "export_idle_timeout");
    assert.equal(h.signal.aborted, true);
    assert.equal(h.cancelled, true);
    assert.equal(h.aborted, true);
    assert.deepEqual(h.storage.values.get("account-backups").catalog, []);
  });
}

test("idle watchdog also bounds a fetch that never returns headers", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let signal;
  const h = runtime({ fetch: (_url, options) => {
    signal = options.signal;
    return new Promise(() => {});
  } });
  const running = h.instance.fetch(h.request());
  await flush();
  t.mock.timers.tick(IDLE_TIMEOUT_MS);
  assert.equal((await (await running).json()).status, "retrying");
  assert.equal(signal.aborted, true);
  assert.equal(h.storage.values.get("account-backups").current_job.last_error, "export_idle_timeout");
});

test("body progress continues beyond five minutes and commits verified multipart", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = streamingRuntime();
  const running = h.instance.fetch(h.request());
  await flush();
  for (let minute = 0; minute < 7; minute++) {
    t.mock.timers.tick(60_000);
    h.upstream.enqueue(new Uint8Array([1]));
    await flush();
    assert.equal(h.signal.aborted, false);
  }
  h.upstream.close();
  assert.equal((await (await running).json()).status, "committed");
  t.mock.timers.tick(OVERALL_TIMEOUT_MS);
  assert.equal(h.signal.aborted, false, "settlement clears both timers");
  assert.equal(h.aborted, false);
  const state = h.storage.values.get("account-backups");
  assert.equal(state.catalog.length, 1);
  assert.equal(state.catalog[0].committed_at, NOW.toISOString());
});

test("overall ceiling aborts even with continuous body progress", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = streamingRuntime();
  const running = h.instance.fetch(h.request());
  await flush();
  for (let elapsed = 60_000; elapsed < OVERALL_TIMEOUT_MS; elapsed += 60_000) {
    t.mock.timers.tick(60_000);
    h.upstream.enqueue(new Uint8Array([1]));
    await flush();
    assert.equal(h.signal.aborted, false);
  }
  t.mock.timers.tick(60_000);
  assert.equal((await (await running).json()).status, "retrying");
  await flush();
  assert.equal(h.storage.values.get("account-backups").current_job.last_error, "export_overall_timeout");
  assert.equal(h.aborted, true);
  assert.equal(h.cancelled, true);
});

for (const phase of ["fetch", "body"]) {
  test(`connection loss during ${phase} has a bounded code and retains retry policy`, async () => {
    const h = phase === "body" ? streamingRuntime() : runtime({
      fetch: async () => { throw new Error("Network connection lost. private upstream detail"); },
    });
    const running = h.instance.fetch(h.request());
    if (phase === "body") {
      await flush();
      h.upstream.error(new Error("Network connection lost. private upstream detail"));
    }
    assert.equal((await (await running).json()).status, "retrying");
    const state = h.storage.values.get("account-backups");
    assert.equal(state.current_job.last_error, "connection_lost");
    assert.notEqual(h.storage.alarm, null);
    assert.deepEqual(state.catalog, []);
  });
}

function healthEnv(records) {
  const entries = {};
  for (const id of Object.keys(records)) entries[`acct:${id}`] = sourceRoute();
  return {
    CP_ACCOUNT_BACKUPS_ENABLED: "true",
    DIRECTORY: new KV(entries),
    BACKUPS: {},
    ACCOUNT_BACKUP: {
      idFromName: (name) => name,
      get: (id) => ({ fetch: async () => {
        const backups = records[id];
        return backups ? Response.json({
          schema_version: "witself.v0", account_id: id, backups,
        }) : new Response("unavailable", { status: 503 });
      } }),
    },
  };
}

function catalogState(id, dates) {
  return {
    schema_version: ACCOUNT_BACKUP_STATE_SCHEMA,
    account_id: id, revision: 1, current_job: null, failures: [],
    catalog: dates.map((at, index) => ({
      ...committedRecord(backupJobIdentity(id, SCHEDULED_AT - index * 60_000, 1)),
      account_id: id, verified_at: at,
    })),
  };
}

test("status uses newest committed evidence, null for none, and excludes nonlive accounts", async () => {
  const now = Date.now();
  const at = (age) => new Date(now - age * 1000).toISOString();
  const records = {
    acct_old: catalogState("acct_old", [at(400_000), at(200_000)]),
    acct_fresh: catalogState("acct_fresh", [at(30)]),
    acct_never: catalogState("acct_never", []),
    acct_archived: catalogState("acct_archived", []),
    acct_pending: catalogState("acct_pending", []),
  };
  records.acct_fresh.catalog[0].committed_at = at(10);
  const env = healthEnv(records);
  await env.DIRECTORY.put("archived:acct_archived", "true");
  await env.DIRECTORY.put("pending:acct_pending", "true");
  const status = await accountBackupStatus(env);
  assert.equal(status.stale_accounts, 1);
  assert.equal(status.never_committed_accounts, 1);
  assert.equal(status.health_available, true);
  assert.ok(status.oldest_committed_age_seconds >= 200_000);
  assert.ok(status.oldest_committed_age_seconds < 200_002);
  assert.equal((await accountBackupStatus(env, "acct_old")).account.last_committed_at, at(200_000));
  assert.equal((await accountBackupStatus(env, "acct_fresh")).account.last_committed_at, at(10));
  assert.equal((await accountBackupStatus(env, "acct_never")).account.last_committed_at, null);
  assert.deepEqual(await accountBackupHealth(healthEnv({})), {
    stale_accounts: 0, oldest_committed_age_seconds: null, never_committed_accounts: 0, health_available: true,
  });
  const h = runtime();
  assert.equal((await (await h.instance.fetch(new Request("http://backup/status"))).json()).last_committed_at, null);
  await h.instance.fetch(h.request());
  assert.equal((await (await h.instance.fetch(new Request("http://backup/status"))).json()).last_committed_at, NOW.toISOString());
});

test("fleet health traverses pages and uses strictly greater than twice interval", async () => {
  const now = Date.now();
  const env = healthEnv({});
  const seen = [];
  const result = await accountBackupHealth(env, {
    now: () => now,
    activeAccountPage: async (_env, _limit, cursor) => {
      seen.push(cursor);
      return cursor ? { account_ids: ["acct_late"], next_cursor: null }
        : { account_ids: ["acct_boundary"], next_cursor: "next" };
    },
    status: async (_env, id) => ({
      last_committed_at: new Date(now - (172_800_000 + (id === "acct_late" ? 1 : 0))).toISOString(),
      backups: { current_job: null, failures: [] },
    }),
  });
  assert.deepEqual(seen, [undefined, "next"]);
  assert.deepEqual(result, { stale_accounts: 1, oldest_committed_age_seconds: 172800, never_committed_accounts: 0, health_available: true });
  await assert.rejects(accountBackupHealth(env, {
    activeAccountPage: async () => ({ account_ids: [], next_cursor: "cycle" }),
  }), /incomplete/);
  const broken = healthEnv({ acct_broken: null });
  await broken.DIRECTORY.put(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify({ retained: true }));
  const degraded = await accountBackupStatus(broken);
  assert.equal(degraded.schema_version, "witself.v0");
  assert.equal(degraded.schedule.enabled, true);
  assert.deepEqual(degraded.scan, { retained: true, computed_at: null, health_available: false,
    stale_accounts: null, never_committed_accounts: null, oldest_committed_age_seconds: null });
  assert.equal(degraded.health_available, false);
  assert.equal(degraded.stale_accounts, null);
  assert.equal(degraded.oldest_committed_age_seconds, null);
  assert.equal(degraded.never_committed_accounts, null);

  let pages = 0;
  broken.DIRECTORY.list = async () => ({ keys: [], list_complete: false, cursor: `page-${++pages}` });
  const capped = await accountBackupStatus(broken);
  assert.equal(pages, 100);
  assert.equal(capped.health_available, false);
  assert.equal(capped.stale_accounts, null);
  assert.deepEqual(capped.scan, degraded.scan);
});

test("scan counts previous-slot terminal failures once before dispatch across pages", async () => {
  const env = healthEnv({});
  const slot = backupJobIdentity("slot", SCHEDULED_AT, 1440).scheduled_at;
  const previous = new Date(Date.parse(slot) - 86400_000).toISOString();
  const events = [];
  const dependencies = {
    activeAccountPage: async (_env, _limit, cursor) => cursor
      ? { account_ids: ["acct_unavailable", "acct_older"], next_cursor: null }
      : { account_ids: ["acct_failed", "acct_retrying"], next_cursor: "next" },
    status: async (_env, id) => {
      events.push(`status:${id}`);
      if (id === "acct_unavailable") throw new Error("unavailable");
      const job = { status: id === "acct_retrying" ? "retrying" : "failed",
        scheduled_at: id === "acct_older" ? "2026-07-01T00:00:00.000Z" : previous };
      return { last_committed_at: null, backups: { current_job: job, failures: [job] } };
    },
    dispatch: async (_env, job) => {
      assert.ok(events.includes(`status:${job.account_id}`));
      return { accepted: true, status: "retrying" };
    },
  };
  await runScheduledAccountBackups(env, SCHEDULED_AT, dependencies);
  const result = await runScheduledAccountBackups(env, SCHEDULED_AT, dependencies);
  const cached = await runScheduledAccountBackups(env, SCHEDULED_AT, dependencies);
  assert.equal(result.accepted, 4);
  assert.equal(result.failed, 0);
  assert.equal(result.previous_slot_terminal_failures, 1);
  assert.equal(result.previous_slot_status_unavailable, 1);
  assert.equal(cached.previous_slot_terminal_failures, 1);
  assert.equal(env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).previous_slot_terminal_failures, 1);
});

test("public scrape reads only the persisted backup snapshot and fails KV reads as scrape failures", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: SCHEDULED_AT });
  const env = healthEnv({});
  const health = {
    stale_accounts: 2, never_committed_accounts: 3,
    oldest_committed_age_seconds: 200_000, health_available: true,
    computed_at: new Date(SCHEDULED_AT - 60_000).toISOString(),
    private_metadata: "acct_private",
  };
  await env.DIRECTORY.put(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify({
    schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA, health,
  }));
  const get = env.DIRECTORY.get.bind(env.DIRECTORY);
  const reads = [];
  env.DIRECTORY.get = async (key, options) => {
    assert.ok(!key.startsWith("acct:"));
    reads.push(key);
    return get(key, options);
  };
  env.DIRECTORY.list = () => assert.fail("scrape must not list accounts");
  env.ACCOUNT_BACKUP.get = () => assert.fail("scrape must not read Durable Objects");
  const scrape = () => uptimeProbeMetricsResponse(new Request("https://example/metrics/probes"), env);
  const response = await scrape();
  assert.equal(response.status, 200);
  const text = await response.text();
  assert.match(text, /witself_control_plane_account_backup_stale_accounts 2\n/);
  assert.match(text, /witself_control_plane_account_backup_never_committed_accounts 3\n/);
  assert.match(text, /witself_control_plane_account_backup_oldest_committed_age_seconds 200000\n/);
  assert.match(text, /witself_control_plane_account_backup_health_available 1\n/);
  assert.match(text, /witself_control_plane_account_backup_health_age_seconds 60\n/);
  assert.match(text, /witself_control_plane_account_backup_interval_seconds 86400\n/);
  assert.equal(reads.filter((key) => key === ACCOUNT_BACKUP_SCAN_KEY).length, 1);
  assert.doesNotMatch(text, /acct_private|account_id|backup_id/);
  env.DIRECTORY.get = (key, options) => key === ACCOUNT_BACKUP_SCAN_KEY
    ? new Promise(() => {}) : get(key, options);
  const running = scrape();
  await flush();
  t.mock.timers.tick(5000);
  const failed = await running;
  assert.equal(failed.status, 503);
  assert.equal(await failed.text(), "probe results unavailable\n");
});

test("missing, legacy, malformed and failed health snapshots cannot publish healthy counts", async () => {
  const env = healthEnv({});
  for (const scan of [null, {}, {
    schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA,
    health: { computed_at: NOW.toISOString(), health_available: true, stale_accounts: "private" },
  }, {
    schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA,
    health: { computed_at: NOW.toISOString(), health_available: false, stale_accounts: 1 },
  }]) {
    await env.DIRECTORY.put(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(scan));
    const response = await uptimeProbeMetricsResponse(new Request("https://example/metrics/probes"), env);
    assert.equal(response.status, 200);
    const text = await response.text();
    assert.match(text, /witself_control_plane_account_backup_health_available 0\n/);
    assert.doesNotMatch(text, /account_backup_stale_accounts|account_backup_never_committed_accounts/);
  }
});

test("never committed accounts get strictly two intervals from their earliest known job", async () => {
  const now = SCHEDULED_AT;
  const at = (age) => new Date(now - age).toISOString();
  const records = {
    new: { current_job: null, failures: [] },
    recent: { current_job: { scheduled_at: at(60_000) }, failures: [] },
    boundary: { current_job: { scheduled_at: at(172_800_000) }, failures: [] },
    late: { current_job: { scheduled_at: at(172_800_001) }, failures: [] },
    history: { current_job: { scheduled_at: at(60_000) }, failures: [
      { scheduled_at: at(120_000) }, { scheduled_at: at(172_800_001) },
    ] },
  };
  const health = await accountBackupHealth(healthEnv({}), {
    now: () => now,
    activeAccountPage: async () => ({ account_ids: Object.keys(records), next_cursor: null }),
    status: async (_env, id) => ({ last_committed_at: null, backups: records[id] }),
  });
  assert.deepEqual(health, { stale_accounts: 2, never_committed_accounts: 5,
    oldest_committed_age_seconds: null, health_available: true });
});

test("scan health accumulates before dispatch and retains a completed snapshot across pages and slots", async () => {
  const env = healthEnv({});
  const staleAt = new Date(SCHEDULED_AT - 172_800_001).toISOString();
  let statusReads = 0;
  const deps = {
    activeAccountPage: async (_env, _limit, cursor) => cursor
      ? { account_ids: ["acct_never"], next_cursor: null }
      : { account_ids: ["acct_old"], next_cursor: "next" },
    status: async (_env, id) => {
      statusReads++;
      return { last_committed_at: id === "acct_old" ? staleAt : null,
        backups: { current_job: null, failures: [] } };
    },
    dispatch: async () => ({ accepted: true, status: "committed" }),
  };
  await runScheduledAccountBackups(env, SCHEDULED_AT, deps);
  assert.equal(env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).health.health_available, false);
  await runScheduledAccountBackups(env, SCHEDULED_AT + 60_000, deps);
  const health = env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).health;
  assert.deepEqual(health, { stale_accounts: 1, never_committed_accounts: 1,
    oldest_committed_age_seconds: 172800, health_available: true,
    computed_at: new Date(SCHEDULED_AT).toISOString() });
  assert.equal(statusReads, 2, "health reuses pre-dispatch reads");
  assert.deepEqual((await accountBackupStatus(env)).scan.health, health);
  await runScheduledAccountBackups(env, SCHEDULED_AT + 86400_000, deps);
  assert.deepEqual(env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).health, health);
  deps.status = async () => { throw new Error("private failure"); };
  await runScheduledAccountBackups(env, SCHEDULED_AT + 86460_000, deps);
  assert.deepEqual(env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).health, {
    stale_accounts: null, never_committed_accounts: null, oldest_committed_age_seconds: null,
    health_available: false, computed_at: new Date(SCHEDULED_AT + 86400_000).toISOString(),
  });
});

test("empty scans are healthy but resumed legacy pages cannot fabricate complete health", async () => {
  const env = healthEnv({});
  const deps = {
    activeAccountPage: async () => ({ account_ids: [], next_cursor: null }),
    dispatch: () => assert.fail("empty scan must not dispatch"),
  };
  await runScheduledAccountBackups(env, SCHEDULED_AT, deps);
  const scan = env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY);
  assert.deepEqual(scan.health, { stale_accounts: 0, never_committed_accounts: 0,
    oldest_committed_age_seconds: null, health_available: true,
    computed_at: new Date(SCHEDULED_AT).toISOString() });
  const text = await (await uptimeProbeMetricsResponse(new Request("https://example/metrics/probes"), env)).text();
  assert.match(text, /witself_control_plane_account_backup_oldest_committed_age_seconds 0\n/);
  assert.match(text, /witself_control_plane_account_backup_health_available 1\n/);
  delete scan.health;
  delete scan.health_progress;
  Object.assign(scan, { complete: false, cursor: "legacy-page", scanned: 1, accepted: 1 });
  await env.DIRECTORY.put(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(scan));
  await runScheduledAccountBackups(env, SCHEDULED_AT, deps);
  const resumed = env.DIRECTORY.value(ACCOUNT_BACKUP_SCAN_KEY).health;
  assert.equal(resumed.health_available, false);
  assert.equal(resumed.stale_accounts, null);
  assert.equal(resumed.never_committed_accounts, null);
});

test("EOF stops idle timeout but overall ceiling still bounds a stuck multipart completion", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = streamingRuntime();
  const original = h.bucket.createMultipartUpload;
  h.bucket.createMultipartUpload = async (...args) => ({
    ...await original(...args), complete: () => new Promise(() => {}),
  });
  const running = h.instance.fetch(h.request());
  await flush();
  h.upstream.enqueue(new Uint8Array([1]));
  h.upstream.close();
  await flush();
  t.mock.timers.tick(IDLE_TIMEOUT_MS);
  assert.equal(h.signal.aborted, false);
  t.mock.timers.tick(OVERALL_TIMEOUT_MS - IDLE_TIMEOUT_MS);
  assert.equal((await (await running).json()).status, "retrying");
  assert.equal(h.storage.values.get("account-backups").current_job.last_error, "export_overall_timeout");
  assert.deepEqual(h.storage.values.get("account-backups").catalog, []);
});

test("watchdog failure code survives the final attempt and clears the alarm", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = runtime({ maxAttempts: 1, fetch: () => new Promise(() => {}) });
  const running = h.instance.fetch(h.request());
  await flush();
  t.mock.timers.tick(IDLE_TIMEOUT_MS);
  assert.equal((await (await running).json()).status, "failed");
  const state = h.storage.values.get("account-backups");
  assert.equal(state.failures[0].last_error, "export_idle_timeout");
  assert.equal(h.storage.alarm, null);
});

function capabilityFixture() {
  const job = backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1);
  const record = committedRecord(job);
  const storage = new Storage();
  storage.values.set("account-backups", {
    schema_version: ACCOUNT_BACKUP_STATE_SCHEMA, account_id: ACCOUNT,
    revision: 1, current_job: null, catalog: [record], failures: [],
  });
  let now = NOW.getTime();
  const durable = new DurableAccountBackup({ storage, id: { name: ACCOUNT } }, {}, { now: () => new Date(now) });
  const call = (path, input) => durable.fetch(new Request(`https://internal${path}`, {
    method: "POST", body: JSON.stringify(input),
  }));
  const mint = (overrides = {}) => call("/archive-capability", { backup_id: job.backup_id, target_cell: TARGET, ttl_seconds: 1800, ...overrides });
  const bucket = new Bucket();
  bucket.write(record.object, "valid-backup-object", objectMetadata(job), record.r2_etag);
  const env = {
    BACKUPS: bucket,
    PUBLIC_IP_LIMITER: { limit: () => ({ success: true }) },
    ACCOUNT_BACKUP: { idFromName: (name) => ({ name }), get: (id) => ({ fetch: (request) => id.name === ACCOUNT ? durable.fetch(request) : new Response(null, { status: 404 }) }) },
  };
  const route = (token, backup = job.backup_id, method = "GET", account = ACCOUNT) => worker.fetch(new Request(
    `https://cp.test.invalid/v1/backups:archive?account_id=${account}&backup_id=${backup}`,
    { method, headers: { Authorization: `Bearer ${token}`, "CF-Connecting-IP": "192.0.2.47" } },
  ), env, {});
  return { env, durable, record, storage, call, mint, bucket, route, expire: () => { now += 1800_000; } };
}

test("archive limiter runs once per IP before storage and preserves unused capabilities", async () => {
  const f = capabilityFixture();
  const { token } = await (await f.mint()).json();
  const counts = new Map();
  f.env.PUBLIC_IP_LIMITER.limit = ({ key }) => {
    assert.equal(key, "192.0.2.47");
    const count = (counts.get(key) ?? 0) + 1;
    counts.set(key, count);
    return { success: count <= 2 };
  };
  assert.equal((await f.route("bad")).status, 404);
  assert.equal((await f.route("bad")).status, 404);
  const originalGet = f.env.ACCOUNT_BACKUP.get;
  f.env.ACCOUNT_BACKUP.get = () => assert.fail("throttled request reached Durable Object");
  const denied = await f.route(token);
  assert.equal(denied.status, 429);
  assert.equal(denied.headers.get("Retry-After"), "60");
  assert.equal(denied.headers.get("Cache-Control"), "no-store");
  assert.equal(f.bucket.conditionedGets.length, 0);
  counts.clear(); // The binding's next window admits the one-request drill.
  f.env.ACCOUNT_BACKUP.get = originalGet;
  assert.equal((await f.route(token)).status, 200);
  assert.equal(counts.get("192.0.2.47"), 1);
});

test("archive limiter failure or missing binding fails closed with the uniform 404", async () => {
  for (const limiter of [undefined, { limit: () => ({}) }, { limit: () => null }, { limit: () => { throw new Error("private provider failure"); } }]) {
    const f = capabilityFixture();
    f.env.PUBLIC_IP_LIMITER = limiter;
    f.env.ACCOUNT_BACKUP.get = () => assert.fail("unavailable limiter reached Durable Object");
    const response = await f.route("bad");
    assert.equal(response.status, 404);
    assert.deepEqual(await response.json(), { error: "backup archive is not available" });
  }
});

test("archive capabilities bind committed identity, hash storage keys, consume once and expire", async () => {
  const f = capabilityFixture();
  const response = await f.mint();
  assert.equal(response.status, 200);
  const { token } = await response.json();
  assert.ok(/^cap_[0-9a-f]{64}$/.test(token));
  const entries = [...(await f.storage.list({ prefix: "archive-capability:" }))];
  assert.equal(entries.length, 1);
  assert.ok(/^archive-capability:[0-9a-f]{64}$/.test(entries[0][0]));
  assert.ok(!JSON.stringify(entries).includes(token));
  const consume = () => f.call("/archive-capability:consume", { token });
  const results = await Promise.all([consume(), consume()]);
  assert.deepEqual(results.map((r) => r.status).sort(), [200, 404]);
  assert.deepEqual(await results.find((r) => r.status === 200).json(), {
    backup_id: f.record.backup_id, object: f.record.object, r2_etag: f.record.r2_etag,
    size: f.record.size, source_cell: SOURCE, target_cell: TARGET,
  });
  assert.equal((await consume()).status, 404);
  const next = await (await f.mint()).json();
  assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 1);
  f.expire();
  assert.equal((await f.call("/archive-capability:consume", next)).status, 404);
  assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
});

test("archive mint rejects missing/uncommitted records and invalid bounds", async () => {
  for (const overrides of [{ ttl_seconds: 59 }, { ttl_seconds: 3601 }, { ttl_seconds: 60.5 }, { target_cell: "Bad" }, { backup_id: "backup_20260725T123500Z" }]) {
    const f = capabilityFixture();
    assert.equal((await f.mint(overrides)).status, 404);
    assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
  }
  const f = capabilityFixture();
  const state = await f.storage.get("account-backups");
  state.catalog = [];
  assert.equal((await f.mint()).status, 404);
  for (const ttl_seconds of [60, 3600]) {
    assert.equal((await capabilityFixture().mint({ ttl_seconds })).status, 200);
  }
});

test("archive public route streams exact conditioned object without fleet auth and rejects replay", async () => {
  const f = capabilityFixture();
  const { token } = await (await f.mint()).json();
  const response = await f.route(token);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Content-Type"), "application/gzip");
  assert.equal(response.headers.get("Content-Length"), String(f.record.size));
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("X-Witself-Backup-ID"), f.record.backup_id);
  assert.equal(response.headers.get("X-Witself-Backup-Cell"), SOURCE);
  assert.equal(await response.text(), "valid-backup-object");
  assert.deepEqual(f.bucket.conditionedGets, [{ key: f.record.object, etagMatches: f.record.r2_etag }]);
  assert.equal((await f.route(token)).status, 404);
});

test("archive public route conceals missing, expired, mismatched and changed sources", async () => {
  for (const mode of ["missing", "bad-token", "expired", "backup", "account", "etag", "size", "no-object", "method", "bad-id"]) {
    const f = capabilityFixture();
    let { token } = await (await f.mint()).json();
    let backup = f.record.backup_id, account = ACCOUNT, method = "GET";
    if (mode === "missing") token = "cap_" + "c".repeat(64);
    if (mode === "bad-token") token = "not-a-capability";
    if (mode === "expired") f.expire();
    if (mode === "backup") backup = "backup_20260725T123500Z";
    if (mode === "bad-id") backup = "bad";
    if (mode === "account") account = "other";
    if (mode === "etag") f.bucket.write(f.record.object);
    if (mode === "size") f.bucket.write(f.record.object, "short", {}, f.record.r2_etag);
    if (mode === "no-object") f.bucket.values.clear();
    if (mode === "method") method = "POST";
    const response = await f.route(token, backup, method, account);
    assert.equal(response.status, 404, mode);
    assert.deepEqual(await response.json(), { error: "backup archive is not available" }, mode);
    assert.equal(response.headers.get("Cache-Control"), "no-store", mode);
    if (mode === "backup") assert.equal((await f.route(token)).status, 404, "mismatch must burn capability");
  }
});

test("pull drill rejects generic and mismatched acknowledgements without receipts", async () => {
  for (const [acknowledgement, expected] of [
    [{}, "missing exact acknowledgement"],
    [{ error: 123, other: "private" }, "missing exact acknowledgement"],
    [{ error: "invalid or mismatched backup archive", other: "private" }, "invalid or mismatched backup archive"],
    [{ error: " \n" + "x".repeat(121) + "\u0000é" }, "x".repeat(120)],
    [{ error: "bad\n\u0000 archive! é" }, "bad archive!"],
    [{ error: "\n\u0000" }, "missing exact acknowledgement"],
    [{ schema_version: "witself.v0", account_id: ACCOUNT, backup_id: "wrong", purpose: "backup", status: "active", archive_schema_version: 73, validated: true }, "missing exact acknowledgement"],
  ]) {
    const f = capabilityFixture();
    const receipts = [];
    const binding = directory({ [`cell:${TARGET}`]: { endpoint: "https://validation.example", accepting: false,
      backup_validation_target: true, provision_token: "provision", backup_token: "backup",
      registration_id: "reg-target", registered_at: "2026-07-25T00:00:00.000Z" } });
    await assert.rejects(runAccountBackupValidation({ DIRECTORY: binding, CELL_COORDINATOR: projectedCellCoordinator(binding),
      ACCOUNT_BACKUP: backupNamespace(f.record, receipts), BACKUPS: f.bucket },
    { account_id: ACCOUNT, backup_id: f.record.backup_id, target_cell: TARGET },
    { origin: "https://cp.test.invalid", validateArchive: () => validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1)),
      fetch: async (_url, options) => { assert.equal(options.body, undefined); return Response.json(acknowledgement); } }), (error) => {
      assert.equal(error.message, `backup validation 200: ${expected}`);
      assert.ok(!error.message.includes("private"));
      return true;
    });
    assert.deepEqual(receipts, []);
  }
});


test("independent capabilities serialize without sharing the long export fence", async () => {
  const f = capabilityFixture();
  f.durable.fence.busy = true;
  const minted = await Promise.all([f.mint(), f.mint()]);
  assert.deepEqual(minted.map((r) => r.status), [200, 200]);
  const tokens = await Promise.all(minted.map((r) => r.json()));
  assert.ok(tokens[0].token !== tokens[1].token);
  const results = await Promise.all(tokens.map((token) => f.call("/archive-capability:consume", token)));
  assert.deepEqual(results.map((r) => r.status), [200, 200]);
});

test("status exposes persisted probe health after a fresh Worker and without live health", async () => {
  const env = healthEnv({});
  env.FLEET_TOKEN = "fleet-test";
  const request = () => new Request("https://cp.test.invalid/v1/backups/status", {
    headers: { Authorization: "Bearer fleet-test" },
  });
  env.DIRECTORY.list = () => { throw new Error("live listing unavailable"); };
  const health = { computed_at: NOW.toISOString(), health_available: true,
    stale_accounts: 2, never_committed_accounts: 1, oldest_committed_age_seconds: 123 };
  for (const snapshot of [null, { schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA, health },
    { schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA, health: { ...health, health_available: false } },
    { schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA, health: { ...health, stale_accounts: "invalid" } }]) {
    if (snapshot) await env.DIRECTORY.put(ACCOUNT_BACKUP_SCAN_KEY, JSON.stringify(snapshot));
    const response = await worker.fetch(request(), env, {});
    assert.equal(response.status, 200);
    const status = await response.json();
    assert.equal(status.health_available, false, "live fleet health stays independent");
    if (snapshot === null) {
      assert.equal(status.scan, null);
    } else {
      const available = snapshot.health.health_available && typeof snapshot.health.stale_accounts === "number";
      assert.equal(status.scan.health_available, available);
      assert.equal(status.scan.stale_accounts, available ? 2 : null);
      const probes = await uptimeProbeMetricsResponse(new Request("https://cp.test.invalid/metrics/probes"), env);
      assert.match(await probes.text(), new RegExp(`witself_control_plane_account_backup_health_available ${Number(available)}\\n`));
    }
  }
});

function drillRecord(record, overrides = {}) {
  return {
    drill_id: "ce8d8d55-ecb4-4a37-8761-85cbd7c632fd",
    account_id: ACCOUNT, backup_id: record.backup_id, target_cell: TARGET,
    state: "running", started_at: NOW.toISOString(),
    deadline_at: new Date(NOW.getTime() + RESTORE_DRILL_DEADLINE_MS).toISOString(),
    finished_at: null, validated_at: null, error: null, ...overrides,
  };
}

function drillEnv() {
  const f = capabilityFixture();
  const binding = directory({ [`cell:${TARGET}`]: {
    endpoint: "https://validation.example", accepting: false,
    backup_validation_target: true, provision_token: "provision",
    backup_token: "target-backup-token", registration_id: "reg-target",
    registered_at: "2026-07-25T00:00:00.000Z",
  } });
  Object.assign(f.env, { DIRECTORY: binding, CELL_COORDINATOR: projectedCellCoordinator(binding), FLEET_TOKEN: "fleet-backup-test-token" });
  f.durable.now = () => new Date(NOW);
  const input = { account_id: ACCOUNT, backup_id: f.record.backup_id, target_cell: TARGET };
  const status = () => f.durable.fetch(new Request("http://account-backup.internal/status"));
  const route = (overrides = {}, ctx = {}) => worker.fetch(new Request("https://cp.test.invalid/v1/backups:restore-drill", {
    method: "POST", headers: { Authorization: `Bearer ${f.env.FLEET_TOKEN}`, "Content-Type": "application/json" },
    body: JSON.stringify({ ...input, ...overrides }),
  }), f.env, ctx);
  const publicStatus = () => worker.fetch(new Request(`https://cp.test.invalid/v1/backups/status?account_id=${ACCOUNT}`, {
    headers: { Authorization: `Bearer ${f.env.FLEET_TOKEN}` },
  }), f.env, {});
  const start = (overrides = {}) => f.call("/restore-drill:start", { backup_id: f.record.backup_id, target_cell: TARGET, ...overrides });
  return { ...f, input, binding, status, route, publicStatus, start };
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function countDrillFetches(t) {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (url) => {
    if (String(url).endsWith("/v1/version")) return Response.json({ schema_version: "witself.v0" });
    calls += 1; return Response.json({});
  });
  return () => calls;
}

function unhandledRejections(t) {
  const errors = [];
  const listener = (error) => errors.push(error);
  process.on("unhandledRejection", listener);
  t.after(() => process.removeListener("unhandledRejection", listener));
  return errors;
}

function fakeDrill(options = {}) {
  const f = drillEnv();
  const calls = [], finishes = [], receipts = [];
  f.env.ACCOUNT_BACKUP = backupNamespace(f.record, receipts, { calls, finishes, ...options });
  const receipt = { ...f.input, validated_at: "2026-07-25T12:36:00.000Z", status: "active", archive_schema_version: 73 };
  const dependencies = {
    origin: "https://cp.test.invalid", now: () => new Date(receipt.validated_at),
    validateArchive: () => validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1)),
    fetch: async () => Response.json({ schema_version: "witself.v0", account_id: ACCOUNT,
      backup_id: f.record.backup_id, purpose: "backup", status: "active", archive_schema_version: 73, validated: true }),
  };
  return { ...f, calls, finishes, receipts, receipt, dependencies };
}

test("restore drill start records one account slot and refuses concurrent starts", async () => {
  const f = drillEnv();
  const other = committedRecord(backupJobIdentity(ACCOUNT, SCHEDULED_AT + 60_000, 1));
  f.storage.values.get("account-backups").catalog.push(other);
  const state = JSON.stringify(f.storage.values.get("account-backups"));
  const responses = await Promise.all([f.start(), f.start()]);
  assert.deepEqual(responses.map((r) => r.status), [200, 409]);
  const body = await responses[0].json();
  assert.match(body.restore_drill.drill_id, /^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/);
  const expected = drillRecord(f.record, { drill_id: body.restore_drill.drill_id });
  assert.deepEqual(body, { schema_version: "witself.v0", account_id: ACCOUNT, restore_drill: expected });
  assert.equal(Date.parse(expected.deadline_at) - Date.parse(expected.started_at), RESTORE_DRILL_DEADLINE_MS);
  assert.equal(RESTORE_DRILL_DEADLINE_MS, 30 * 60_000);
  for (const response of [responses[1], await f.start({ backup_id: other.backup_id }), await f.start({ target_cell: "another-target" })]) {
    assert.equal(response.status, 409);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "restore drill already running", restore_drill: expected });
  }
  assert.deepEqual(f.storage.values.get("restore-drill"), expected);
  assert.equal(JSON.stringify(f.storage.values.get("account-backups")), state);
  assert.equal(f.storage.alarm, null);
});

test("restore drill refusal matrix preserves the record and survives a failed queue operation", async () => {
  const f = drillEnv();
  const expect = async (promise, status, error) => {
    const response = await promise;
    assert.equal(response.status, status);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error });
  };
  await expect(f.start({ backup_id: "backup_20260725T123500Z" }), 400, "backup validation is not in the committed catalog");
  for (const body of [{}, null, { backup_id: "bad", target_cell: TARGET }, { backup_id: f.record.backup_id, target_cell: "Bad" },
    { backup_id: [f.record.backup_id], target_cell: TARGET },
    ...[123, true, [TARGET]].map((target_cell) => ({ backup_id: f.record.backup_id, target_cell }))]) {
    await expect(f.call("/restore-drill:start", body), 400, "invalid restore drill start");
  }
  for (const body of [{}, null, { drill_id: "", state: "validated" }, { drill_id: "x".repeat(65), state: "failed" },
    { drill_id: 1, state: "failed" }, { drill_id: "x", state: "running" },
    { drill_id: "x", state: "validated", validated_at: "bad" }, { drill_id: "x", state: "failed", error: 1 }]) {
    await expect(f.call("/restore-drill:finish", body), 400, "invalid restore drill finish");
  }
  await expect(f.call("/restore-drill:finish", { drill_id: "missing", state: "failed" }), 409, "restore drill record is not this drill");
  assert.equal(f.storage.values.get("restore-drill"), undefined);
  const state = structuredClone(f.storage.values.get("account-backups"));
  f.storage.values.set("account-backups", "invalid");
  await expect(f.start(), 500, "durable account backup state is invalid");
  f.storage.values.set("account-backups", state);
  const response = await f.start();
  assert.equal(response.status, 200);
  const { restore_drill: record } = await response.json();
  await expect(f.call("/restore-drill:finish", { drill_id: "wrong", state: "failed" }), 409, "restore drill record is not this drill");
  assert.deepEqual(f.storage.values.get("restore-drill"), record);
  assert.equal((await f.call("/restore-drill:finish", { drill_id: record.drill_id, state: "failed", error: "reported" })).status, 200);
  const terminal = structuredClone(f.storage.values.get("restore-drill"));
  await expect(f.call("/restore-drill:finish", { drill_id: record.drill_id, state: "validated" }), 409, "restore drill record is not this drill");
  assert.deepEqual(f.storage.values.get("restore-drill"), terminal);
});

test("restore drill lifecycle projects deadlines without writes and accepts a late owner finish", async () => {
  const f = drillEnv();
  const first = (await (await f.start()).json()).restore_drill;
  const finished = "2026-07-25T12:36:00.000Z";
  f.durable.now = () => new Date(finished);
  let response = await f.call("/restore-drill:finish", { drill_id: first.drill_id, state: "validated", validated_at: finished });
  assert.equal(response.status, 200);
  assert.deepEqual((await response.json()).restore_drill, { ...first, state: "validated", finished_at: finished, validated_at: finished });
  const second = (await (await f.start()).json()).restore_drill;
  assert.notEqual(second.drill_id, first.drill_id);
  response = await f.call("/restore-drill:finish", { drill_id: second.drill_id, state: "failed", error: " \n\u0000bad\té" + "x".repeat(400) });
  assert.equal(response.status, 200);
  const failed = (await response.json()).restore_drill;
  assert.equal(failed.state, "failed");
  assert.equal(failed.finished_at, finished);
  assert.equal(failed.validated_at, null);
  assert.match(failed.error, /^[\x20-\x7e]{1,300}$/);
  assert.equal(failed.error, ("\u0000bad\té" + "x".repeat(400)).slice(0, 300).replace(/[^\x20-\x7e]/g, ""));
  const third = (await (await f.start()).json()).restore_drill;
  const stored = JSON.stringify(f.storage.values.get("restore-drill"));
  f.durable.now = () => new Date(Date.parse(third.deadline_at) - 1);
  assert.deepEqual((await (await f.status()).json()).restore_drill, third);
  f.durable.now = () => new Date(third.deadline_at);
  assert.deepEqual((await (await f.status()).json()).restore_drill, { ...third, state: "failed", error: "restore drill did not report before its deadline" });
  assert.equal(JSON.stringify(f.storage.values.get("restore-drill")), stored);
  const fourth = (await (await f.start()).json()).restore_drill;
  assert.notEqual(fourth.drill_id, third.drill_id);
  assert.equal((await f.call("/restore-drill:finish", { drill_id: third.drill_id, state: "validated", validated_at: finished })).status, 409);
  assert.deepEqual(f.storage.values.get("restore-drill"), fourth);
  f.durable.now = () => new Date(fourth.deadline_at);
  assert.equal((await (await f.status()).json()).restore_drill.state, "failed");
  response = await f.call("/restore-drill:finish", { drill_id: fourth.drill_id, state: "validated", validated_at: fourth.deadline_at });
  assert.equal(response.status, 200);
  const late = { ...fourth, state: "validated", finished_at: fourth.deadline_at, validated_at: fourth.deadline_at };
  assert.deepEqual((await response.json()).restore_drill, late);
  f.durable.now = () => new Date(Date.parse(fourth.deadline_at) + RESTORE_DRILL_DEADLINE_MS);
  assert.deepEqual((await (await f.status()).json()).restore_drill, late);
});

test("restore drill operations stay independent of the export fence and leave receipts fenced", async () => {
  const f = drillEnv();
  f.durable.fence.busy = true;
  const response = await f.start();
  assert.equal(response.status, 200);
  const { restore_drill: record } = await response.json();
  assert.equal((await f.call("/restore-drill:finish", { drill_id: record.drill_id, state: "validated", validated_at: NOW.toISOString() })).status, 200);
  const receipt = await f.call("/validation-verified", { ...f.input, validated_at: NOW.toISOString(), status: "active", archive_schema_version: 73 });
  assert.equal(receipt.status, 409);
  assert.equal(f.storage.values.get("account-backups").catalog[0].validations, undefined);
});

test("restore drill status tolerates absent and malformed records without changing backups", async (t) => {
  const f = drillEnv();
  const logs = t.mock.method(console, "log", () => {});
  const state = structuredClone(f.storage.values.get("account-backups"));
  let response = await f.status();
  assert.equal(response.status, 200);
  let body = await response.json();
  assert.equal(body.restore_drill, null);
  assert.deepEqual(body.backups, state);
  assert.equal(logs.mock.callCount(), 0);
  const valid = drillRecord(f.record);
  const malformed = ["invalid", {}, { ...valid, state: "unknown" }, { ...valid, started_at: "bad" }, { ...valid, deadline_at: "bad" }];
  for (const key of Object.keys(valid)) {
    const missing = { ...valid };
    delete missing[key];
    malformed.push(missing, { ...valid, [key]: 7 });
  }
  for (const value of malformed) {
    f.storage.values.set("restore-drill", value);
    const before = JSON.stringify(value);
    response = await f.status();
    assert.equal(response.status, 200);
    body = await response.json();
    assert.equal(body.restore_drill, null);
    assert.deepEqual(body.backups, state);
    assert.equal(JSON.stringify(f.storage.values.get("restore-drill")), before);
  }
  assert.equal(logs.mock.callCount(), malformed.length);
  for (const call of logs.mock.calls) assert.deepEqual(call.arguments, ["account-backup: restore drill record is invalid"]);
});

test("restore drill runtime success preserves the receipt and all existing answer fields in order", async () => {
  const f = fakeDrill();
  const started = await beginAccountBackupValidation(f.env, f.input, f.dependencies);
  assert.equal(started.drill_id, drillRecord(f.record).drill_id);
  assert.deepEqual(started.restore_drill, drillRecord(f.record));
  assert.deepEqual(f.calls, ["/status", "/restore-drill:start"]);
  const result = await started.complete();
  assert.deepEqual(f.calls, ["/status", "/restore-drill:start", "/archive-capability", "/validation-verified", "/restore-drill:finish"]);
  assert.deepEqual(result, { schema_version: "witself.v0", validated: true, ...f.receipt, drill_id: started.drill_id });
  assert.deepEqual(f.receipts, [f.receipt]);
  assert.deepEqual(f.finishes, [{ drill_id: started.drill_id, state: "validated", validated_at: f.receipt.validated_at }]);
});

test("restore drill runtime failure records the original error without a validation receipt", async () => {
  for (const mode of ["ack", "etag"]) {
    const f = fakeDrill();
    if (mode === "ack") f.dependencies.fetch = async () => Response.json({ error: "bad\n\u0000 archive! é", other: "private response body" });
    else f.dependencies.validateArchive = () => {
      f.bucket.write(f.record.object, "valid-backup-object", objectMetadata(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1)), "changed-etag");
      return validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1));
    };
    let message;
    await assert.rejects(runAccountBackupValidation(f.env, f.input, f.dependencies), (error) => {
      message = error.message;
      assert.match(message, mode === "ack" ? /^backup validation 200: bad archive!$/ : /R2 object identity/);
      return true;
    });
    assert.deepEqual(f.finishes, [{ drill_id: drillRecord(f.record).drill_id, state: "failed", error: message }]);
    assert.deepEqual(f.receipts, []);
    assert.ok(!JSON.stringify(f.finishes).includes("private response body"));
    assert.equal(f.calls.at(-1), "/restore-drill:finish");
  }
});

test("restore drill runtime refusals avoid archive work and rejected preflights never claim", async () => {
  for (const mode of ["busy", "target", "catalog"]) {
    const record = committedRecord(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1));
    const busy = drillRecord(record);
    const f = fakeDrill(mode === "busy" ? { busy } : {});
    let validations = 0, fetches = 0;
    f.dependencies.validateArchive = () => { validations += 1; assert.fail("rejected request validated archive"); };
    f.dependencies.fetch = () => { fetches += 1; assert.fail("rejected request reached cell"); };
    if (mode === "target") f.binding.values.set(`cell:${TARGET}`, JSON.stringify({ ...f.binding.value(`cell:${TARGET}`), accepting: true }));
    const input = mode === "catalog" ? { ...f.input, backup_id: "backup_20260725T123500Z" } : f.input;
    await assert.rejects(runAccountBackupValidation(f.env, input, f.dependencies), (error) => {
      if (mode === "busy") {
        assert.ok(error instanceof BackupValidationBusyError);
        assert.equal(error.message, "restore drill already running");
        assert.deepEqual(error.restore_drill, busy);
      } else assert.match(error.message, mode === "target" ? /backup_validation_target=true, accepting=false cell with a distinct backup token/ : /^backup validation requires a committed catalog backup$/);
      return true;
    });
    assert.equal(validations, 0);
    assert.equal(fetches, 0);
    assert.equal(f.calls.filter((p) => p === "/restore-drill:start").length, mode === "busy" ? 1 : 0);
    assert.equal(f.calls.includes("/archive-capability"), false);
    assert.deepEqual(f.finishes, []);
    assert.deepEqual(f.receipts, []);
  }
});

test("restore drill finish failure preserves catalog-authoritative success and logs once", async (t) => {
  const logs = t.mock.method(console, "log", () => {});
  const f = fakeDrill({ finishStatus: 500 });
  const result = await runAccountBackupValidation(f.env, f.input, f.dependencies);
  assert.deepEqual(result, { schema_version: "witself.v0", validated: true, ...f.receipt, drill_id: drillRecord(f.record).drill_id });
  assert.deepEqual(f.receipts, [f.receipt]);
  assert.deepEqual(logs.mock.calls.map((c) => c.arguments), [["account-backup: restore drill record was not finished"]]);
});

test("restore drill heartbeat framing uses bytes and produces one parseable JSON document", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const terminal = deferred();
  const response = heartbeatJSONResponse(terminal.promise, { headers: { "X-Witself-Restore-Drill-ID": "drill-test" } });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Content-Type"), "application/json");
  assert.equal(response.headers.get("Cache-Control"), "no-store, no-transform");
  assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), "drill-test");
  assert.equal(RESTORE_DRILL_HEARTBEAT_MS, 10_000);
  const [readable, textBody] = response.body.tee();
  const text = new Response(textBody).text();
  const reader = readable.getReader();
  const decoder = new TextDecoder();
  let body = "";
  for (let i = 0; i < 2; i += 1) {
    t.mock.timers.tick(10_000);
    const chunk = await reader.read();
    assert.ok(chunk.value instanceof Uint8Array);
    assert.equal(decoder.decode(chunk.value), "\n");
    body += decoder.decode(chunk.value);
  }
  terminal.resolve({ a: 1 });
  const chunk = await reader.read();
  assert.ok(chunk.value instanceof Uint8Array);
  assert.equal(decoder.decode(chunk.value), '{"a":1}\n');
  body += decoder.decode(chunk.value);
  assert.equal((await reader.read()).done, true);
  assert.deepEqual(JSON.parse(body), { a: 1 });
  assert.equal(await text, body);
  assert.equal(await heartbeatJSONResponse(Promise.resolve({ ready: true })).text(), '{"ready":true}\n');
  assert.deepEqual(await heartbeatJSONResponse(Promise.reject(new Error(" rejected "))).json(), { schema_version: "witself.v0", error: "rejected" });
});

test("restore drill heartbeat cancellation clears timers and skips the terminal write", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const clear = t.mock.method(globalThis, "clearInterval");
  const errors = unhandledRejections(t);
  const terminal = deferred();
  const reader = heartbeatJSONResponse(terminal.promise).body.getReader();
  t.mock.timers.tick(10_000);
  assert.equal(new TextDecoder().decode((await reader.read()).value), "\n");
  await reader.cancel();
  assert.ok(clear.mock.callCount() >= 1);
  assert.doesNotThrow(() => t.mock.timers.tick(30_000));
  terminal.resolve({ validated: true });
  await flush();
  await flush();
  assert.deepEqual(errors, []);
  assert.equal((await reader.read()).done, true);
});

test("restore drill Worker grammar and preflight failures keep real statuses in both framings", async (t) => {
  const fetches = countDrillFetches(t);
  for (const heartbeat of [undefined, true]) {
    for (const mode of ["heartbeat", "target-missing", "target-fence", "catalog"]) {
      const f = drillEnv();
      const overrides = { heartbeat };
      let status = 400, error;
      if (mode === "heartbeat") { overrides.heartbeat = "yes"; error = "heartbeat must be a boolean"; }
      if (mode === "target-missing") { overrides.target_cell = undefined; error = "backup_id and target_cell are required"; }
      if (mode === "target-fence") {
        f.binding.values.set(`cell:${TARGET}`, JSON.stringify({ ...f.binding.value(`cell:${TARGET}`), accepting: true }));
        status = 409; error = "backup validation target must be a registered backup_validation_target=true, accepting=false cell with a distinct backup token";
      }
      if (mode === "catalog") { overrides.backup_id = "backup_20260725T123500Z"; status = 502; error = "backup validation requires a committed catalog backup"; }
      const response = await f.route(overrides);
      assert.equal(response.status, status, mode);
      assert.deepEqual(await response.json(), { schema_version: "witself.v0", error }, mode);
      assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), null);
      assert.equal(f.storage.values.get("restore-drill"), undefined);
    }
  }
  assert.equal(fetches(), 0);
});

test("restore drill Worker busy replies carry the existing record in both framings", async (t) => {
  const fetches = countDrillFetches(t);
  const f = drillEnv();
  const { restore_drill } = await (await f.start()).json();
  for (const heartbeat of [undefined, true]) {
    const response = await f.route({ heartbeat });
    assert.equal(response.status, 409);
    assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), null);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "restore drill already running", restore_drill });
  }
  assert.equal(fetches(), 0);
});

test("restore drill Worker default failure keeps its status and exposes the durable record", async (t) => {
  const fetches = countDrillFetches(t);
  // An absent heartbeat field and an explicit false both select the default framing.
  for (const heartbeat of [undefined, false]) {
    const f = drillEnv();
    const reread = deferred();
    f.bucket.get = () => reread.promise;
    const pending = f.route(heartbeat === undefined ? {} : { heartbeat });
    await flush();
    const running = structuredClone(f.storage.values.get("restore-drill"));
    assert.equal(running.state, "running", String(heartbeat));
    reread.resolve(null);
    const response = await pending;
    assert.equal(response.status, 502, String(heartbeat));
    assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), running.drill_id);
    const error = `committed archive ${f.record.object} is not readable from R2`;
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error });
    const record = { ...running, state: "failed", finished_at: NOW.toISOString(), error };
    assert.deepEqual(f.storage.values.get("restore-drill"), record);
    const status = await f.publicStatus();
    assert.equal(status.status, 200);
    assert.deepEqual((await status.json()).account.restore_drill, (await (await f.status()).json()).restore_drill);
  }
  assert.equal(fetches(), 0);
});

test("restore drill Worker heartbeat sends immediate bytes then the terminal failure", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const fetches = countDrillFetches(t);
  const f = drillEnv();
  const reread = deferred();
  f.bucket.get = () => reread.promise;
  const response = await f.route({ heartbeat: true });
  const running = structuredClone(f.storage.values.get("restore-drill"));
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Content-Type"), "application/json");
  assert.equal(response.headers.get("Cache-Control"), "no-store, no-transform");
  assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), running.drill_id);
  assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "request");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  const reader = response.body.getReader();
  t.mock.timers.tick(10_000);
  const heartbeat = await reader.read();
  assert.ok(heartbeat.value instanceof Uint8Array);
  let body = new TextDecoder().decode(heartbeat.value);
  assert.equal(body, "\n");
  reread.resolve(null);
  const terminal = await reader.read();
  assert.ok(terminal.value instanceof Uint8Array);
  const error = `committed archive ${f.record.object} is not readable from R2`;
  assert.equal(new TextDecoder().decode(terminal.value), JSON.stringify({ schema_version: "witself.v0", error }) + "\n");
  body += new TextDecoder().decode(terminal.value);
  assert.equal((await reader.read()).done, true);
  assert.deepEqual(JSON.parse(body), { schema_version: "witself.v0", error });
  assert.deepEqual(f.storage.values.get("restore-drill"), { ...running, state: "failed", finished_at: NOW.toISOString(), error });
  assert.equal(fetches(), 0);
});

// Node does not model Worker cancellation; an active disconnect cancels a request-driven drill in production (proven 2026-10-03). The alarm driver is the fix.
test("restore drill Worker request driver: cancelling the heartbeat body does not abort the completion promise (Node model; production bounds waitUntil to 30 s)", { timeout: 10_000 }, async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const errors = unhandledRejections(t);
  const fetches = countDrillFetches(t);
  for (const ctx of [{}, { promises: [], waitUntil(promise) { this.promises.push(promise); } }]) {
    const f = drillEnv();
    const reread = deferred();
    f.bucket.get = () => reread.promise;
    const response = await f.route({ heartbeat: true }, ctx);
    assert.equal(response.status, 200);
    const running = structuredClone(f.storage.values.get("restore-drill"));
    assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), running.drill_id);
    assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "request");
    const reader = response.body.getReader();
    t.mock.timers.tick(10_000);
    assert.equal(new TextDecoder().decode((await reader.read()).value), "\n");
    await reader.cancel();
    const duplicate = await f.route();
    assert.equal(duplicate.status, 409);
    assert.deepEqual(await duplicate.json(), { schema_version: "witself.v0", error: "restore drill already running", restore_drill: running });
    assert.doesNotThrow(() => t.mock.timers.tick(30_000));
    reread.resolve(null);
    if (ctx.promises) {
      assert.equal(ctx.promises.length, 1);
      await Promise.all(ctx.promises);
    }
    await flush();
    await flush();
    const record = f.storage.values.get("restore-drill");
    assert.equal(record.state, "failed");
    assert.equal(record.drill_id, running.drill_id);
    assert.equal(record.finished_at, NOW.toISOString());
    assert.equal(record.error, `committed archive ${f.record.object} is not readable from R2`);
    assert.deepEqual((await (await f.publicStatus()).json()).account.restore_drill, record);
  }
  assert.deepEqual(errors, []);
  assert.equal(fetches(), 0);
});

// Separate objects and a shared clock model only local ordering, not Worker lifetime.
function alarmDrillEnv(t) {
  const f = drillEnv();
  let clock = NOW.getTime();
  f.now = () => new Date(clock);
  f.setTime = (value) => { clock = Number(value); };
  f.advance = (ms) => { clock += ms; };
  f.durable.now = f.now;
  f.driverStorage = new Storage();
  f.rereads = 0;
  f.validateArchive = () => validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1));
  f.driver = new DurableAccountBackup({ storage: f.driverStorage, id: { name: RESTORE_DRILL_DRIVER_PREFIX + ACCOUNT } }, f.env, {
    now: f.now, validateArchive: (...args) => { f.rereads += 1; return f.validateArchive(...args); },
  });
  f.log = [];
  f.intercept = null;
  f.env.ACCOUNT_BACKUP = {
    idFromName: (name) => ({ name }),
    get: ({ name }) => ({ fetch: async (request) => {
      const path = new URL(request.url).pathname;
      const input = request.method === "POST" ? await request.clone().json().catch(() => null) : null;
      const instance = name === ACCOUNT ? "account"
        : name === RESTORE_DRILL_DRIVER_PREFIX + ACCOUNT ? "driver" : "unknown";
      const entry = { name, instance, path, input };
      f.log.push(entry);
      const forward = () => name === ACCOUNT ? f.durable.fetch(request)
        : name === RESTORE_DRILL_DRIVER_PREFIX + ACCOUNT ? f.driver.fetch(request)
        : new Response(null, { status: 404 });
      return f.intercept ? f.intercept(entry, forward) : forward();
    } }),
  };
  f.key = (id = f.storage.values.get("restore-drill")?.drill_id) => f.driverStorage.values.get("restore-drill-driver:" + id);
  f.prepareBody = async (overrides = {}) => {
    const target = f.binding.value(`cell:${TARGET}`);
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(target.backup_token));
    return {
      drill_id: crypto.randomUUID(), backup_id: f.record.backup_id, target_cell: TARGET,
      archive_origin: "https://cp.test.invalid", target: {
        endpoint: target.endpoint, registration_id: target.registration_id, registered_at: target.registered_at,
        backup_token_sha256: [...new Uint8Array(digest)].map((n) => n.toString(16).padStart(2, "0")).join(""),
      }, catalog: structuredClone(f.record), ...overrides,
    };
  };
  f.prepare = async (overrides = {}) => f.driver.fetch(new Request("http://account-backup.internal/restore-drill-driver:start", {
    method: "POST", body: JSON.stringify(await f.prepareBody(overrides)),
  }));
  f.next = async () => { assert.notEqual(f.driverStorage.alarm, null); f.setTime(f.driverStorage.alarm); await fireAlarm(f.driverStorage, f.driver); };
  f.archiveRoute = (token) => worker.fetch(new Request(
    `https://cp.test.invalid/v1/backups:archive?account_id=${ACCOUNT}&backup_id=${f.record.backup_id}`,
    { headers: { Authorization: `Bearer ${token}`, "CF-Connecting-IP": "192.0.2.47" } },
  ), f.env, {});
  return f;
}

function cellAck(record) {
  return { schema_version: "witself.v0", account_id: ACCOUNT, status: record.status,
    archive_schema_version: record.archive_schema_version, purpose: "backup", backup_id: record.backup_id, validated: true };
}

function cellJob(f, state, started) {
  return { schema_version: "witself.v0", account_id: ACCOUNT, backup_id: f.record.backup_id,
    validation_id: f.storage.values.get("restore-drill")?.drill_id,
    validation_job: { state, ...(started === undefined ? {} : { started }) } };
}

function fakeCell(t, script = {}) {
  const calls = [];
  const actions = { ...script };
  const fetch = t.mock.method(globalThis, "fetch", async (url, options = {}) => {
    const path = String(url);
    const action = path.endsWith("/v1/version") ? "version"
      : path.endsWith(":start-validate-backup") ? "start"
      : path.endsWith(":validate-backup-status") ? "poll"
      : path.endsWith(":validate-backup") ? "sync" : "unknown";
    const call = { action, url: path, options, headers: new Headers(options.headers) };
    calls.push(call);
    let value = actions[action];
    if (Array.isArray(value)) value = value.shift();
    if (typeof value === "function") value = await value(call);
    if (value instanceof Response) return value;
    if (value !== undefined) return Response.json(value);
    if (action === "version") return Response.json({ schema_version: "witself.v0", backup_validation_protocol: 2 });
    if (action === "start" || action === "poll") return Response.json({ schema_version: "witself.v0", account_id: ACCOUNT,
      backup_id: call.headers.get("X-Witself-Backup-ID"), validation_id: call.headers.get("X-Witself-Validation-ID"),
      validation_job: { state: "running", ...(action === "start" ? { started: true } : {}) } });
    if (action === "sync") return Response.json(cellAck(committedRecord(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1))));
    assert.fail("unexpected cell request path");
  });
  return { calls, fetch, set: (action, value) => { actions[action] = value; }, count: (action) => calls.filter((call) => call.action === action).length };
}

const pendingDriverAlarms = new WeakMap();
function fireAlarm(storage, durable) {
  if (pendingDriverAlarms.has(durable)) throw new Error("an alarm invocation is already running");
  storage.alarm = null; // The platform consumes its one alarm before delivery.
  const invocation = {};
  pendingDriverAlarms.set(durable, invocation);
  return Promise.resolve().then(() => durable.alarm()).finally(() => {
    if (pendingDriverAlarms.get(durable) === invocation) pendingDriverAlarms.delete(durable);
  });
}
function abandonAlarm(durable) {
  assert.ok(pendingDriverAlarms.has(durable), "only a pending invocation can be killed");
  pendingDriverAlarms.delete(durable);
}

const alarmRecordFields = ["drill_id", "account_id", "backup_id", "target_cell", "state", "started_at", "deadline_at", "finished_at", "validated_at", "error", "driver"];
const driverKeys = (f) => [...f.driverStorage.values.keys()].filter((name) => name.startsWith("restore-drill-driver:"));
const callsTo = (f, path) => f.log.filter((entry) => entry.path === path);
const samePrivateState = (actual, expected, message) => assert.ok(JSON.stringify(actual) === JSON.stringify(expected), message);
async function startedAlarm(f) {
  const response = await f.route({ wait: false });
  assert.equal(response.status, 202);
  return (await response.json()).restore_drill;
}
async function preparedAlarm(f) {
  // Enter at the driver so the log contains no Worker's preflight /status read.
  const response = await f.prepare();
  assert.equal(response.status, 200);
  return (await response.json()).restore_drill;
}
function assertAccountBackupIsolation(f, originalAlarm) {
  const allowed = new Set(["/restore-drill:start", "/restore-drill:finish", "/archive-capability", "/validation-verified"]);
  const accountCalls = f.log.filter((entry) => entry.instance === "account");
  assert.ok(accountCalls.length > 0, "the driver reached the account instance");
  for (const { path } of accountCalls) assert.ok(allowed.has(path), `unexpected account-bound route: ${path}`);
  assert.notEqual(originalAlarm, null, "the account backup alarm was armed at setup");
  assert.equal(f.storage.alarm, originalAlarm, "the account backup alarm stays unchanged across driver ticks");
}
function setTarget(f, fields) {
  f.binding.values.set(`cell:${TARGET}`, JSON.stringify({ ...f.binding.value(`cell:${TARGET}`), ...fields }));
}

// 1. Account-side claims never manipulate the account's backup schedule.
test("alarm restore drill claim replays before expiry and preserves account backup state", async (t) => {
  const f = alarmDrillEnv(t);
  const put = t.mock.method(f.storage, "put");
  f.storage.alarm = NOW.getTime() + 500_000;
  const originalAlarm = f.storage.alarm;
  const backups = JSON.stringify(f.storage.values.get("account-backups"));
  const claim = { driver: "alarm", drill_id: crypto.randomUUID(), claim_by: new Date(f.now().getTime() + RESTORE_DRILL_CLAIM_WINDOW_MS).toISOString() };
  const response = await f.start(claim);
  assert.equal(response.status, 200);
  const record = (await response.json()).restore_drill;
  assert.deepEqual(Object.keys(record), alarmRecordFields);
  assert.equal(record.driver, "alarm");
  assert.equal(record.drill_id, claim.drill_id);
  assert.equal(Date.parse(record.deadline_at) - Date.parse(record.started_at), RESTORE_DRILL_ALARM_DEADLINE_MS);
  assert.equal(RESTORE_DRILL_ALARM_DEADLINE_MS, 90 * 60_000);
  for (const time of [f.now().getTime(), Date.parse(claim.claim_by) + 1, Date.parse(record.deadline_at) + 1]) {
    f.setTime(time);
    const before = put.mock.callCount();
    assert.deepEqual((await (await f.start(claim)).json()).restore_drill, record);
    assert.equal(put.mock.callCount(), before);
  }
  assert.equal(put.mock.callCount(), 1);
  for (const [overrides, error] of [
    [{ drill_id: crypto.randomUUID() }, "restore drill claim expired"],
    [{ drill_id: crypto.randomUUID(), claim_by: new Date(f.now().getTime() + 60_001).toISOString() }, "invalid restore drill start"],
    [{ drill_id: "not-uuid" }, "invalid restore drill start"], [{ driver: "x" }, "invalid restore drill start"],
    [{ claim_by: "bad" }, "invalid restore drill start"],
  ]) {
    const answer = await f.start({ ...claim, ...overrides });
    assert.equal(answer.status, 400);
    assert.equal((await answer.json()).error, error);
    assert.equal(put.mock.callCount(), 1);
  }
  assert.equal(JSON.stringify(f.storage.values.get("account-backups")), backups);
  assert.ok(put.mock.calls.every((call) => call.arguments[0] === "restore-drill"), "claims only put the restore-drill key");
  assert.equal(f.storage.alarm, originalAlarm);
  assert.deepEqual([...f.storage.values.keys()].sort(), ["account-backups", "restore-drill"]);
  const removed = alarmDrillEnv(t);
  const removedPut = t.mock.method(removed.storage, "put");
  const removedResponse = await removed.start(claim);
  assert.equal(removedResponse.status, 200);
  const removedRecord = (await removedResponse.json()).restore_drill;
  removed.setTime(Date.parse(removedRecord.deadline_at) + 1);
  removed.storage.values.get("account-backups").catalog = [];
  const removedBackups = JSON.stringify(removed.storage.values.get("account-backups"));
  assert.deepEqual((await (await removed.start(claim)).json()).restore_drill, removedRecord);
  assert.equal(removedPut.mock.callCount(), 1);
  assert.ok(removedPut.mock.calls.every((call) => call.arguments[0] === "restore-drill"), "catalog-removal claims only put the restore-drill key");
  assert.equal(JSON.stringify(removed.storage.values.get("account-backups")), removedBackups);
  const g = alarmDrillEnv(t);
  const busyPut = t.mock.method(g.storage, "put");
  const busyBackups = JSON.stringify(g.storage.values.get("account-backups"));
  const fresh = { ...claim, drill_id: crypto.randomUUID(), claim_by: new Date(g.now().getTime() + RESTORE_DRILL_CLAIM_WINDOW_MS).toISOString() };
  assert.equal((await g.start(fresh)).status, 200);
  const busy = await g.start({ ...fresh, drill_id: crypto.randomUUID() });
  assert.equal(busy.status, 409);
  assert.equal((await busy.json()).restore_drill.drill_id, fresh.drill_id);
  const missing = await g.start({ ...fresh, drill_id: crypto.randomUUID(), backup_id: "backup_20260725T123500Z" });
  assert.equal(missing.status, 400);
  assert.equal((await missing.json()).error, "backup validation is not in the committed catalog");
  assert.equal(JSON.stringify(g.storage.values.get("account-backups")), busyBackups);
  assert.ok(busyPut.mock.calls.every((call) => call.arguments[0] === "restore-drill"), "busy and refused claims only put the restore-drill key");
});

// 2. The private key and recovery alarm exist before the first cross-object claim.
test("alarm restore drill prepare orders storage before claim and validates every pinned field", async (t) => {
  const f = alarmDrillEnv(t);
  const order = [];
  const put = f.driverStorage.put.bind(f.driverStorage);
  f.driverStorage.put = async (...args) => { order.push("put"); return put(...args); };
  const arm = f.driverStorage.setAlarm.bind(f.driverStorage);
  f.driverStorage.setAlarm = async (...args) => { order.push("alarm"); return arm(...args); };
  f.intercept = (entry, forward) => { order.push(entry.path); return forward(); };
  const body = await f.prepareBody();
  const response = await f.prepare(body);
  assert.equal(response.status, 200);
  const answer = await response.json();
  assert.deepEqual(order.slice(0, 3), ["put", "alarm", "/restore-drill:start"]);
  assert.deepEqual(answer, { schema_version: "witself.v0", account_id: ACCOUNT, restore_drill: f.storage.values.get("restore-drill") });
  assert.equal(answer.restore_drill.drill_id, body.drill_id);
  assert.equal(f.key().phase, "start");
  assert.equal(f.key().deadline_at, answer.restore_drill.deadline_at);
  assert.equal(f.key().next_at, f.now().getTime());
  assert.equal(f.key().rev, 1);
  assert.equal(f.driverStorage.alarm, f.now().getTime());
  assert.deepEqual([...f.storage.values.keys()].sort(), ["account-backups", "restore-drill"]);
  const invalid = [
    { drill_id: "bad" }, { drill_id: body.drill_id }, { backup_id: "bad" }, { target_cell: "Bad" },
    { archive_origin: "http://cp.test.invalid" }, { archive_origin: "https://cp.test.invalid/path" }, { archive_origin: null },
    { target: null }, { target: { ...body.target, endpoint: "https://validation.example/" } },
    { target: { ...body.target, registration_id: "" } }, { target: { ...body.target, registration_id: "x".repeat(129) } },
    { target: { ...body.target, registered_at: "bad" } }, { target: { ...body.target, backup_token_sha256: "bad" } },
    { catalog: null }, { catalog: { ...body.catalog, account_id: "different" } },
    { catalog: { ...body.catalog, backup_id: "backup_20260725T123500Z" } },
  ];
  for (const fields of invalid) {
    const before = JSON.stringify([...f.driverStorage.values]);
    const accountBefore = JSON.stringify([...f.storage.values]);
    const answer = await f.prepare(fields);
    assert.equal(answer.status, 400);
    assert.equal((await answer.json()).error, "invalid restore drill start");
    assert.ok(JSON.stringify([...f.driverStorage.values]) === before, "invalid prepare leaves driver keys unchanged");
    assert.ok(JSON.stringify([...f.storage.values]) === accountBefore, "invalid prepare leaves account unchanged");
  }
});

// 3. Every refused prepare releases only its own key.
test("alarm restore drill refused second prepare leaves the running drill and alarm untouched", async (t) => {
  const f = alarmDrillEnv(t);
  const cell = fakeCell(t);
  const first = await startedAlarm(f);
  const key = structuredClone(f.key());
  const duplicate = await f.route({ wait: false });
  assert.equal(duplicate.status, 409);
  assert.deepEqual((await duplicate.json()).restore_drill, first);
  samePrivateState(f.key(), key, "duplicate preserves first driver key");
  assert.equal(driverKeys(f).length, 1);
  assert.equal(f.driverStorage.alarm, key.next_at);
  cell.set("version", { schema_version: "witself.v0" });
  const driverCalls = f.log.filter((entry) => entry.name.startsWith(RESTORE_DRILL_DRIVER_PREFIX)).length;
  const fallback = await f.route();
  assert.equal(fallback.status, 409);
  assert.deepEqual((await fallback.json()).restore_drill, first);
  assert.equal(f.log.filter((entry) => entry.name.startsWith(RESTORE_DRILL_DRIVER_PREFIX)).length, driverCalls);
  const missing = committedRecord(backupJobIdentity(ACCOUNT, SCHEDULED_AT + 60_000, 1));
  const refused = await f.prepare({ backup_id: missing.backup_id, catalog: missing });
  assert.equal(refused.status, 400);
  samePrivateState(f.key(), key, "catalog refusal preserves first key");
  assert.equal(driverKeys(f).length, 1);
  cell.set("version", { schema_version: "witself.v0", backup_validation_protocol: 2 });
  await f.next();
  assert.equal(cell.count("start"), 1);
  assert.equal(f.key().phase, "poll");
});

// 4. Unknown claim outcomes retain the key until the account's deadline makes the answer final.
test("alarm restore drill never strands claims with lost answers, late delivery, or foreign ids", async (t) => {
  await t.test("first claim stored but answer lost is replayed exactly once", async (t) => {
    const f = alarmDrillEnv(t);
    const put = t.mock.method(f.storage, "put");
    let attempts = 0;
    f.intercept = async (entry, forward) => {
      const response = await forward();
      if (entry.path === "/restore-drill:start" && ++attempts === 1) throw new Error("answer lost");
      return response;
    };
    const answer = await f.prepare();
    assert.equal(answer.status, 200);
    assert.equal(attempts, 2);
    assert.equal(f.key().phase, "start");
    assert.equal(put.mock.calls.filter((call) => call.arguments[0] === "restore-drill").length, 1);
  });
  await t.test("both claims dropped yield recoverable headers and a late claim is refused", async (t) => {
    const f = alarmDrillEnv(t);
    fakeCell(t);
    const held = [];
    f.intercept = (entry, forward) => {
      if (entry.path === "/restore-drill:start") { held.push(forward); throw new Error("claim dropped"); }
      return forward();
    };
    const response = await f.route({ wait: false });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "restore drill could not be recorded" });
    const id = response.headers.get("X-Witself-Restore-Drill-ID");
    assert.ok(id);
    assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "alarm");
    const key = f.key(id);
    assert.equal(key.phase, "claim");
    assert.equal(f.driverStorage.alarm, Date.parse(key.claim_by) + RESTORE_DRILL_CLAIM_WINDOW_MS);
    assert.equal(f.storage.values.get("restore-drill"), undefined);
    assert.equal(held.length, 2);
    f.intercept = null;
    await f.next();
    assert.equal(f.key(id), undefined);
    assert.equal(f.driverStorage.alarm, null);
    const late = await held[0]();
    assert.equal(late.status, 400);
    assert.equal((await late.json()).error, "restore drill claim expired");
    assert.equal(f.storage.values.get("restore-drill"), undefined);
  });
  await t.test("direct prepare reports its id when both claims are dropped", async (t) => {
    const f = alarmDrillEnv(t);
    f.intercept = () => { throw new Error("claim dropped"); };
    const body = await f.prepareBody();
    const response = await f.prepare(body);
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "restore drill could not be recorded", drill_id: body.drill_id });
    assert.equal(f.key(body.drill_id).phase, "claim");
  });
  await t.test("a refusal after an unknown first claim stays unconfirmed until reconcile", async (t) => {
    const f = alarmDrillEnv(t);
    fakeCell(t);
    const running = (await (await f.start()).json()).restore_drill;
    let attempts = 0;
    f.intercept = (entry, forward) => {
      if (entry.path === "/restore-drill:start" && ++attempts === 1) throw new Error("first claim dropped");
      return forward();
    };
    const response = await f.route({ wait: false });
    assert.equal(response.status, 502);
    const id = response.headers.get("X-Witself-Restore-Drill-ID");
    assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "alarm");
    assert.equal(f.key(id).phase, "claim");
    assert.equal(f.storage.values.get("restore-drill").drill_id, running.drill_id);
    await f.next();
    assert.equal(f.key(id), undefined);
    assert.equal(f.driverStorage.alarm, null);
    assert.equal(f.storage.values.get("restore-drill").drill_id, running.drill_id);
  });
  await t.test("definitive first-claim catalog refusal has no drill headers", async (t) => {
    const f = alarmDrillEnv(t);
    fakeCell(t);
    f.intercept = (entry, forward) => {
      if (entry.path === "/restore-drill:start") f.storage.values.get("account-backups").catalog = [];
      return forward();
    };
    const response = await f.route({ wait: false });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "restore drill could not be recorded" });
    assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), null);
    assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), null);
    assert.equal(driverKeys(f).length, 0);
    assert.equal(f.storage.values.get("restore-drill"), undefined);
  });
  for (const mode of ["both claim answers", "Worker prepare answer"]) {
    await t.test(`${mode} lost after recording still completes under the returned id`, async (t) => {
      const f = alarmDrillEnv(t);
      fakeCell(t, { start: () => cellAck(f.record) });
      f.intercept = async (entry, forward) => {
        const response = await forward();
        if (entry.path === (mode === "both claim answers" ? "/restore-drill:start" : "/restore-drill-driver:start")) throw new Error("stored answer lost");
        return response;
      };
      const response = await f.route({ wait: false });
      assert.equal(response.status, 502);
      const id = response.headers.get("X-Witself-Restore-Drill-ID");
      assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "alarm");
      assert.equal(f.storage.values.get("restore-drill").drill_id, id);
      assert.equal(f.storage.values.get("restore-drill").state, "running");
      assert.equal(f.key(id).phase, mode === "both claim answers" ? "claim" : "start");
      f.intercept = null;
      if (mode === "both claim answers") {
        await f.next();
        assert.equal(f.key(id).phase, "start");
      }
      await f.next();
      assert.equal(f.storage.values.get("restore-drill").drill_id, id);
      assert.equal(f.storage.values.get("restore-drill").state, "validated");
      assert.equal(f.key(id), undefined);
      assert.equal(f.driverStorage.alarm, null);
    });
  }
  await t.test("unreachable account advances reconcile then releases at the outer claim bound", async (t) => {
    const f = alarmDrillEnv(t);
    let claims = 0;
    f.intercept = () => { claims += 1; throw new Error("account unavailable"); };
    const body = await f.prepareBody();
    assert.equal((await f.prepare(body)).status, 502);
    const claimBy = Date.parse(f.key(body.drill_id).claim_by);
    await f.next();
    assert.equal(f.key(body.drill_id).next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
    assert.equal(f.key(body.drill_id).phase, "claim");
    const before = claims;
    f.setTime(claimBy + RESTORE_DRILL_ALARM_DEADLINE_MS);
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(claims, before);
    assert.equal(f.key(body.drill_id), undefined);
    assert.equal(f.driverStorage.alarm, null);
  });
  await t.test("reconcile replays an existing record after expiry and catalog removal", async (t) => {
    const f = alarmDrillEnv(t);
    f.intercept = async (_entry, forward) => { await forward(); throw new Error("answer lost"); };
    const body = await f.prepareBody();
    assert.equal((await f.prepare(body)).status, 502);
    f.storage.values.get("account-backups").catalog = [];
    f.intercept = null;
    await f.next();
    assert.equal(f.key(body.drill_id).phase, "start");
    assert.equal(f.storage.values.get("restore-drill").drill_id, body.drill_id);
  });
  await t.test("foreign drill ids never bind or release an unknown claim", async (t) => {
    const f = alarmDrillEnv(t);
    const foreign = drillRecord(f.record, { driver: "alarm" });
    f.intercept = () => Response.json({ schema_version: "witself.v0", restore_drill: foreign });
    const body = await f.prepareBody();
    assert.notEqual(body.drill_id, foreign.drill_id);
    const answer = await f.prepare(body);
    assert.equal(answer.status, 502);
    assert.equal((await answer.json()).drill_id, body.drill_id);
    assert.equal(f.key(body.drill_id).phase, "claim");
    assert.equal(callsTo(f, "/restore-drill:start").length, 2);
    await f.next();
    assert.equal(f.key(body.drill_id).phase, "claim");
    assert.equal(f.key(body.drill_id).next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
    assert.equal(f.storage.values.get("restore-drill"), undefined);
  });
});

// 5. A short validation can finish in the first alarm, with the existing receipt contract.
test("alarm restore drill one-tick success mints on the account and records the exact receipt", async (t) => {
  const f = alarmDrillEnv(t);
  const cell = fakeCell(t, { start: () => cellAck(f.record) });
  const record = await startedAlarm(f);
  const baseRevision = f.storage.values.get("account-backups").revision;
  await f.next();
  assert.deepEqual(cell.calls.map((call) => call.action), ["version", "version", "start"]);
  const call = cell.calls.at(-1);
  assert.equal(call.options.method, "POST");
  assert.equal(call.options.body, undefined);
  assert.ok(call.options.signal instanceof AbortSignal);
  assert.equal(call.url, `https://validation.example/v1/accounts/${ACCOUNT}:start-validate-backup`);
  assert.deepEqual([...call.headers.keys()].sort(), ["authorization", "x-witself-backup-id", "x-witself-validation-id",
    "x-witself-backup-archive-url", "x-witself-backup-archive-token", "x-witself-backup-archive-size", "x-witself-validation-wait"].sort());
  assert.ok(call.headers.get("Authorization") === "Bearer target-backup-token", "backup authority is exact");
  assert.equal(call.headers.get("X-Witself-Backup-ID"), record.backup_id);
  assert.equal(call.headers.get("X-Witself-Validation-ID"), record.drill_id);
  assert.equal(call.headers.get("X-Witself-Validation-Wait"), String(RESTORE_DRILL_START_WAIT_SECONDS));
  assert.equal(call.headers.get("X-Witself-Backup-Archive-Size"), String(f.record.size));
  assert.equal(call.headers.get("X-Witself-Backup-Archive-URL"), `https://cp.test.invalid/v1/backups:archive?account_id=${ACCOUNT}&backup_id=${record.backup_id}`);
  const token = call.headers.get("X-Witself-Backup-Archive-Token");
  assert.ok(token?.startsWith("cap_"), "START receives a minted capability");
  assert.equal((await f.archiveRoute(token)).status, 200);
  assert.equal((await f.archiveRoute(token)).status, 404);
  assert.equal(f.rereads, 1);
  assert.equal(callsTo(f, "/archive-capability").length, 1);
  assert.equal(callsTo(f, "/archive-capability")[0].name, ACCOUNT);
  const receipt = { ...f.input, validated_at: f.now().toISOString(), status: "active", archive_schema_version: 73 };
  assert.deepEqual(callsTo(f, "/validation-verified").map((entry) => entry.input), [receipt]);
  assert.deepEqual(callsTo(f, "/restore-drill:finish").map((entry) => entry.input), [{ drill_id: record.drill_id, state: "validated", validated_at: receipt.validated_at }]);
  assert.equal(f.storage.values.get("restore-drill").state, "validated");
  assert.equal(f.storage.values.get("restore-drill").validated_at, receipt.validated_at);
  assert.equal(f.storage.values.get("account-backups").revision, baseRevision + 1);
  assert.equal(driverKeys(f).length, 0);
  assert.equal(f.driverStorage.alarm, null);
});

// 6. POLL is read-only at the cell and never needs an archive capability.
test("alarm restore drill start then poll respects due times and carries only identity headers", async (t) => {
  const f = alarmDrillEnv(t);
  const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
  const cell = fakeCell(t);
  const record = await preparedAlarm(f);
  await f.next();
  assert.equal(f.key().phase, "poll");
  assert.equal(f.key().starts, 1);
  assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
  const due = f.key().next_at;
  const before = cell.calls.length;
  f.setTime(due - 1);
  await fireAlarm(f.driverStorage, f.driver);
  assert.equal(cell.calls.length, before);
  await f.next();
  assert.equal(cell.count("poll"), 1);
  assert.equal(f.key().last_polled_at, f.now().getTime());
  const poll = cell.calls.at(-1);
  assert.equal(poll.url, `https://validation.example/v1/accounts/${ACCOUNT}:validate-backup-status`);
  assert.deepEqual([...poll.headers.keys()].sort(), ["authorization", "x-witself-backup-id", "x-witself-validation-id"]);
  assert.equal(poll.headers.get("X-Witself-Validation-ID"), record.drill_id);
  assert.equal(poll.options.body, undefined);
  assert.equal(poll.options.method, "POST");
  assert.equal(callsTo(f, "/archive-capability").length, 1);
  cell.set("poll", () => cellAck(f.record));
  await f.next();
  assert.equal(f.storage.values.get("restore-drill").state, "validated");
  assert.equal(cell.count("start"), 1);
  assert.equal(callsTo(f, "/archive-capability").length, 1);
  assert.equal(f.driverStorage.alarm, null);
  assertAccountBackupIsolation(f, originalAlarm);
});

// 7. Only a definitive absent answer permits a fresh START.
test("alarm restore drill absent results restart with fresh capabilities within both bounds", async (t) => {
  await t.test("three STARTs consume three capabilities but reread once", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    const cell = fakeCell(t, { poll: () => cellJob(f, "absent") });
    await preparedAlarm(f);
    for (let starts = 1; starts <= RESTORE_DRILL_MAX_STARTS; starts += 1) {
      await f.next();
      assert.equal(f.key().starts, starts);
      assert.equal(f.key().phase, "poll");
      await f.next();
      if (starts < RESTORE_DRILL_MAX_STARTS) assert.equal(f.key().phase, "start");
    }
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.equal(f.storage.values.get("restore-drill").error, "restore drill validation job ended without a result");
    assert.equal(cell.count("start"), RESTORE_DRILL_MAX_STARTS);
    const tokens = cell.calls.filter((call) => call.action === "start").map((call) => call.headers.get("X-Witself-Backup-Archive-Token"));
    assert.equal(new Set(tokens).size, RESTORE_DRILL_MAX_STARTS);
    assert.equal(f.rereads, 1);
    assert.equal(callsTo(f, "/archive-capability").length, RESTORE_DRILL_MAX_STARTS);
    assertAccountBackupIsolation(f, originalAlarm);
  });
  await t.test("absent cannot restart when less than the cell job bound remains", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    const cell = fakeCell(t, { poll: () => cellJob(f, "absent") });
    const record = await preparedAlarm(f);
    await f.next();
    assert.equal(RESTORE_DRILL_CELL_JOB_BOUND_MS, 60 * 60 * 1000);
    f.setTime(Date.parse(record.deadline_at) - RESTORE_DRILL_CELL_JOB_BOUND_MS + 1);
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.equal(f.storage.values.get("restore-drill").error, "restore drill cannot restart: less than the cell validation bound remains before the deadline");
    assert.equal(cell.count("start"), 1);
    assert.equal(driverKeys(f).length, 0);
    assertAccountBackupIsolation(f, originalAlarm);
  });
  await t.test("restart rechecks the bound after the delay following absent", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    const cell = fakeCell(t, { poll: () => cellJob(f, "absent") });
    const record = await preparedAlarm(f);
    await f.next();
    f.setTime(Date.parse(record.deadline_at) - RESTORE_DRILL_CELL_JOB_BOUND_MS - 30_000);
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(f.key().phase, "start");
    assert.equal(f.storage.values.get("restore-drill").state, "running");
    await f.next();
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.equal(f.storage.values.get("restore-drill").error, "restore drill cannot restart: less than the cell validation bound remains before the deadline");
    assert.equal(cell.count("start"), 1);
    assert.equal(callsTo(f, "/archive-capability").length, 1);
    assert.equal(driverKeys(f).length, 0);
    assertAccountBackupIsolation(f, originalAlarm);
  });

});

// 8. A decline does not consume one of the three potentially started jobs.
test("alarm restore drill declines preserve the START budget and clamp Retry-After", async (t) => {
  for (const [label, retryAfter, delay] of [["integer", "120", 120_000], ["cap", "3600", RESTORE_DRILL_MAX_DELAY_MS],
    ["minimum", "0", RESTORE_DRILL_TICK_MS], ["missing", null, RESTORE_DRILL_TICK_MS],
    ["junk", "soon", RESTORE_DRILL_TICK_MS], ["date", "Wed, 21 Oct 2026 07:28:00 GMT", RESTORE_DRILL_TICK_MS],
    ["started false", false, RESTORE_DRILL_TICK_MS]]) {
    await t.test(label, async (t) => {
      const f = alarmDrillEnv(t);
      fakeCell(t, { start: () => retryAfter === false ? cellJob(f, "running", false)
        : new Response("", { status: 503, headers: retryAfter === null ? {} : { "Retry-After": retryAfter } }) });
      await startedAlarm(f);
      await f.next();
      assert.equal(f.key().phase, "start");
      assert.equal(f.key().starts, 0);
      assert.equal(f.key().next_at, f.now().getTime() + delay);
      assert.equal(f.driverStorage.alarm, f.key().next_at);
      assert.equal(f.key().reread, true);
    });
  }
});

// 9. A cell-declared failure is terminal; a gateway response can hide a running job.
test("alarm restore drill distinguishes terminal cell failures from ambiguous START and POLL answers", async (t) => {
  const rows = [
    ["START error", "start", () => Response.json({ schema_version: "witself.v0", error: "invalid or mismatched backup archive" }), "backup validation 200: invalid or mismatched backup archive"],
    ["START cell 500", "start", () => Response.json({ schema_version: "witself.v0", error: "could not validate backup" }, { status: 500 }), "backup validation 500: could not validate backup"],
    ["START 404", "start", () => Response.json({ schema_version: "witself.v0", error: "unknown backup action" }, { status: 404 }), "backup validation 404: unknown backup action"],
    ["START old pod 401", "start", () => Response.json({ schema_version: "witself.v0", error: "invalid provision token" }, { status: 401 }), "backup validation 401: invalid provision token"],
    ["START gateway 502", "start", () => new Response("<html>gateway</html>", { status: 502 }), null],
    ["START empty 504", "start", () => new Response(null, { status: 504 }), null],
    ["START empty 200", "start", () => new Response(null), null],
    ["START throw", "start", () => { throw new Error("connection reset"); }, null],
    ["START cut body", "start", () => { const response = new Response(); response.text = async () => { throw new Error("body cut"); }; return response; }, null],
    ["POLL 404", "poll", () => new Response(null, { status: 404 }), "backup validation status 404"],
    ["POLL 401", "poll", () => new Response(null, { status: 401 }), "backup validation status 401"],
    ["POLL 502", "poll", () => new Response(null, { status: 502 }), null],
    ["POLL throw", "poll", () => { throw new Error("connection reset"); }, null],
    ["POLL cut JSON", "poll", () => new Response('{"schema_version":'), null],
    ...["start", "poll"].flatMap((action) => [
      ["wrong backup id", (f) => ({ ...cellAck(f.record), backup_id: "backup_20260725T123500Z" })],
      ["wrong purpose", (f) => ({ ...cellAck(f.record), purpose: "restore" })],
      ["missing schema version", (f) => ({ ...cellAck(f.record), schema_version: undefined })],
      ["unknown job state", () => ({ schema_version: "witself.v0", validation_job: { state: "unknown" } })],
    ].map(([label, body]) => [
      `${action.toUpperCase()} ${label}`, action, (f) => Response.json(body(f)),
      "backup validation 200: missing exact acknowledgement",
    ])),
  ];
  for (const [label, action, response, error] of rows) {
    await t.test(label, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t, { [action]: () => response(f) });
      await startedAlarm(f);
      await f.next();
      if (action === "poll") await f.next();
      const record = f.storage.values.get("restore-drill");
      if (error) {
        assert.equal(record.state, "failed");
        assert.equal(record.error, error);
        assert.equal(driverKeys(f).length, 0);
      } else {
        assert.equal(record.state, "running");
        assert.equal(f.key().phase, "poll");
        assert.equal(f.key().starts, 1);
        assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
        cell.set("poll", undefined);
        const before = cell.count("poll");
        await f.next();
        assert.equal(cell.count("poll"), before + 1);
      }
      assert.equal(callsTo(f, "/validation-verified").length, 0);
    });
  }
});

// 10. Pin every target field and retry only bounded transient fence failures.
test("alarm restore drill fences every pinned target field and retries transient coordinator failures", async (t) => {
  for (const [field, value] of [["endpoint", "https://replacement.example"], ["registration_id", "reg-replacement"],
    ["registered_at", "2026-07-25T00:00:01.000Z"], ["backup_token", "replacement-backup-token"], ["accepting", true]]) {
    await t.test(field, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t);
      await startedAlarm(f);
      await f.next();
      setTarget(f, { [field]: value });
      const before = cell.calls.length;
      await f.next();
      assert.equal(cell.calls.length, before);
      const record = f.storage.values.get("restore-drill");
      assert.equal(record.state, "failed");
      assert.equal(record.error, field === "accepting"
        ? "backup validation target must be a registered backup_validation_target=true, accepting=false cell with a distinct backup token"
        : "backup validation target registration changed before receipt");
    });
  }
  await t.test("one coordinator failure recovers and resets the consecutive count", async (t) => {
    const f = alarmDrillEnv(t);
    const cell = fakeCell(t);
    await startedAlarm(f);
    await f.next();
    const coordinator = f.env.CELL_COORDINATOR;
    f.env.CELL_COORDINATOR = { idFromName: coordinator.idFromName, get: () => ({ fetch: () => { throw new Error("coordinator down"); } }) };
    const before = cell.calls.length;
    await f.next();
    assert.equal(f.storage.values.get("restore-drill").state, "running");
    assert.equal(f.key().fence_failures, 1);
    assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
    assert.equal(cell.calls.length, before);
    f.env.CELL_COORDINATOR = coordinator;
    await f.next();
    assert.equal(f.key().fence_failures, 0);
    assert.equal(cell.count("poll"), 1);
  });
  await t.test("three consecutive coordinator failures fail with the coordinator reason", async (t) => {
    const f = alarmDrillEnv(t);
    const cell = fakeCell(t);
    await startedAlarm(f);
    await f.next();
    f.env.CELL_COORDINATOR = { idFromName: (name) => ({ name }), get: () => ({ fetch: () => { throw new Error("coordinator down"); } }) };
    for (let attempt = 1; attempt <= RESTORE_DRILL_FENCE_RETRIES; attempt += 1) {
      await f.next();
      if (attempt < RESTORE_DRILL_FENCE_RETRIES) assert.equal(f.key().fence_failures, attempt);
    }
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.match(f.storage.values.get("restore-drill").error, /backup validation target coordinator is unavailable/);
    assert.equal(cell.count("poll"), 0);
    assert.equal(driverKeys(f).length, 0);
  });
});

// 11. Unknown receipt outcomes replay the same validation identity.
test("alarm restore drill receipt retries preserve acknowledgement time and one entry per forwarded post", async (t) => {
  for (const mode of ["busy", "dropped", "lost", "503"]) {
    await t.test(mode, async (t) => {
      const f = alarmDrillEnv(t);
      const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
      const cell = fakeCell(t, { start: () => cellAck(f.record) });
      const record = await preparedAlarm(f);
      const initialRevision = f.storage.values.get("account-backups").revision;
      let failing = true, forwarded = 0;
      f.intercept = async (entry, forward) => {
        if (entry.path !== "/validation-verified") return forward();
        if (failing && mode === "dropped") throw new Error("receipt dropped");
        if (failing && mode === "503") return new Response(null, { status: 503 });
        if (failing && mode === "busy") f.durable.fence.busy = true;
        const answer = await forward();
        if (answer.status === 200) forwarded += 1;
        if (failing && mode === "lost") throw new Error("receipt answer lost");
        return answer;
      };
      await f.next();
      assert.equal(f.key().phase, "receipt");
      const validatedAt = f.key().ack.validated_at;
      assert.equal(f.storage.values.get("restore-drill").state, "running");
      assert.equal(callsTo(f, "/restore-drill:finish").length, 0);
      assert.equal(f.storage.values.get("account-backups").revision, initialRevision + forwarded);
      if (mode === "busy") {
        await f.next();
        assert.equal(f.driverStorage.alarm, f.now().getTime() + RESTORE_DRILL_TICK_MS, "later busy receipt re-arms from commit time");
        assert.equal(f.key().ack.validated_at, validatedAt);
        assert.equal(f.storage.values.get("account-backups").revision, initialRevision);
      }
      const before = cell.calls.length;
      failing = false;
      f.durable.fence.busy = false;
      await f.next();
      assert.equal(cell.calls.length, before);
      assert.equal(f.storage.values.get("restore-drill").state, "validated");
      assert.equal(f.storage.values.get("restore-drill").validated_at, validatedAt);
      assert.ok(callsTo(f, "/validation-verified").every((entry) => entry.input.validated_at === validatedAt), "every receipt uses the first acknowledgement time");
      assert.equal(f.storage.values.get("account-backups").revision, initialRevision + forwarded, "one revision per successful forwarded post, including replay after a lost answer");
      assert.equal(forwarded, mode === "lost" ? 2 : 1);
      const validations = f.storage.values.get("account-backups").catalog[0].validations;
      assert.equal(validations.length, 1);
      assert.equal(validations[0].target_cell, record.target_cell);
      assert.equal(validations[0].validated_at, validatedAt);
      assert.equal(driverKeys(f).length, 0);
      assertAccountBackupIsolation(f, originalAlarm);
    });
  }
  await t.test("a thrown final receipt fails and releases the driver", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    fakeCell(t, { start: () => cellAck(f.record) });
    const record = await preparedAlarm(f);
    f.intercept = (entry, forward) => { if (entry.path === "/validation-verified") throw new Error("receipt dropped"); return forward(); };
    await f.next();
    f.setTime(Date.parse(record.deadline_at));
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.equal(f.storage.values.get("restore-drill").error, "backup validation completed but its receipt was not persisted");
    assert.equal(driverKeys(f).length, 0);
    assertAccountBackupIsolation(f, originalAlarm);
  });
  await t.test("a removed catalog backup refuses the receipt immediately", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    fakeCell(t, { start: () => { f.storage.values.get("account-backups").catalog = []; return cellAck(f.record); } });
    await preparedAlarm(f);
    await f.next();
    assert.equal(f.storage.values.get("restore-drill").state, "failed");
    assert.equal(f.storage.values.get("restore-drill").error, "backup validation completed but its receipt was not persisted");
    assert.equal(callsTo(f, "/validation-verified").length, 1);
    assert.equal(driverKeys(f).length, 0);
    assertAccountBackupIsolation(f, originalAlarm);
  });
});

// 12. Once the receipt is accepted no later fence, receipt, or cell request can change success.
test("alarm restore drill finish retries cannot fail a validated receipt or hold the backup fence again", async (t) => {
  for (const finalFailure of [false, true]) {
    await t.test(finalFailure ? "finish unavailable through deadline" : "only finish after target changes", async (t) => {
      const f = alarmDrillEnv(t);
      const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
      const logs = t.mock.method(console, "log", () => {});
      const cell = fakeCell(t, { start: () => cellAck(f.record) });
      const record = await preparedAlarm(f);
      let dropFinish = true;
      f.intercept = (entry, forward) => { if (dropFinish && entry.path === "/restore-drill:finish") throw new Error("finish dropped"); return forward(); };
      await f.next();
      assert.equal(f.key().finishing.state, "validated");
      const validatedAt = f.key().ack.validated_at;
      assert.equal(f.driverStorage.alarm, f.now().getTime() + RESTORE_DRILL_TICK_MS);
      setTarget(f, { registration_id: "changed-after-receipt" });
      let fenceReads = 0;
      f.env.CELL_COORDINATOR = { idFromName: (name) => ({ name }), get: () => ({ fetch: () => { fenceReads += 1; throw new Error("must not recheck fence"); } }) };
      const cellCalls = cell.calls.length;
      const revision = f.storage.values.get("account-backups").revision;
      if (finalFailure) {
        await f.next();
        assert.equal(f.key().finishing.state, "validated");
        f.setTime(Date.parse(record.deadline_at));
        const attempts = callsTo(f, "/restore-drill:finish").length;
        await fireAlarm(f.driverStorage, f.driver);
        assert.equal(callsTo(f, "/restore-drill:finish").length, attempts + 1);
      } else {
        dropFinish = false;
        await f.next();
        assert.equal(f.storage.values.get("restore-drill").state, "validated");
        assert.equal(f.storage.values.get("restore-drill").validated_at, validatedAt);
      }
      assert.equal(fenceReads, 0);
      assert.equal(cell.calls.length, cellCalls);
      assert.equal(callsTo(f, "/validation-verified").length, 1);
      assert.equal(f.storage.values.get("account-backups").revision, revision);
      assert.equal(f.storage.values.get("account-backups").catalog[0].validations.length, 1);
      assert.equal(driverKeys(f).length, 0);
      assert.equal(f.driverStorage.alarm, null);
      assert.ok(logs.mock.calls.every((call) => call.arguments[0] === "account-backup: restore drill record was not finished"));
      assertAccountBackupIsolation(f, originalAlarm);
    });
  }
  await t.test("failed finishing storage write cannot turn an accepted receipt into failure", async (t) => {
    const f = alarmDrillEnv(t);
    const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
    fakeCell(t, { start: () => cellAck(f.record) });
    await preparedAlarm(f);
    const put = f.driverStorage.put.bind(f.driverStorage);
    let failed = false;
    f.driverStorage.put = async (name, value) => {
      if (!failed && value.finishing?.state === "validated") { failed = true; throw new Error("storage unavailable"); }
      return put(name, value);
    };
    await f.next();
    assert.equal(failed, true);
    assert.equal(f.storage.values.get("restore-drill").state, "running");
    assert.equal(f.storage.values.get("account-backups").catalog[0].validations.length, 1);
    assert.equal(callsTo(f, "/restore-drill:finish").length, 0, "storage failure must never report validation failure");
    assert.equal(f.key().phase, "receipt");
    const validatedAt = f.key().ack.validated_at;
    await f.next();
    assert.equal(f.storage.values.get("restore-drill").state, "validated");
    assert.equal(f.storage.values.get("restore-drill").validated_at, validatedAt);
    assert.equal(f.storage.values.get("account-backups").catalog[0].validations.length, 1);
    assert.equal(callsTo(f, "/validation-verified").length, 2);
    assert.ok(callsTo(f, "/validation-verified").every((entry) => entry.input.validated_at === validatedAt));
    assert.equal(driverKeys(f).length, 0);
    assertAccountBackupIsolation(f, originalAlarm);
  });

});

// 13. Final ticks do not start or poll jobs and make at most one last receipt attempt.
test("alarm restore drill deadlines terminate START and POLL and settle final receipt attempts", async (t) => {
  for (const phase of ["start", "poll", "receipt busy", "receipt success"]) {
    await t.test(phase, async (t) => {
      const f = alarmDrillEnv(t);
      const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
      const cell = fakeCell(t, { start: () => phase.startsWith("receipt") ? cellAck(f.record) : cellJob(f, "running", true) });
      const record = await preparedAlarm(f);
      if (phase.startsWith("receipt")) f.durable.fence.busy = true;
      if (phase !== "start") await f.next();
      if (phase === "receipt success") f.durable.fence.busy = false;
      f.setTime(Date.parse(record.deadline_at));
      const before = cell.calls.length;
      const receipts = callsTo(f, "/validation-verified").length;
      await fireAlarm(f.driverStorage, f.driver);
      assert.equal(cell.calls.length, before);
      const terminal = f.storage.values.get("restore-drill");
      assert.equal(terminal.state, phase === "receipt success" ? "validated" : "failed");
      assert.equal(terminal.finished_at, f.now().toISOString());
      if (phase === "start" || phase === "poll") assert.equal(terminal.error, "restore drill validation did not finish before its deadline");
      if (phase === "receipt busy") assert.equal(terminal.error, "backup validation completed but its receipt was not persisted");
      assert.equal(callsTo(f, "/validation-verified").length, receipts + Number(phase.startsWith("receipt")));
      assert.equal(driverKeys(f).length, 0);
      assert.equal(f.driverStorage.alarm, null);
      assertAccountBackupIsolation(f, originalAlarm);
    });
  }
  for (const reachable of [false, true]) {
    await t.test(reachable ? "protocol downgrade is terminal" : "unreachable probe retries", async (t) => {
      const f = alarmDrillEnv(t);
      const originalAlarm = f.storage.alarm = NOW.getTime() + 500_000;
      const cell = fakeCell(t);
      await preparedAlarm(f);
      cell.set("version", () => reachable ? Response.json({ schema_version: "witself.v0" }) : new Response(null, { status: 503 }));
      await f.next();
      assert.equal(cell.count("start"), 0);
      if (reachable) {
        assert.equal(f.storage.values.get("restore-drill").state, "failed");
        assert.equal(f.storage.values.get("restore-drill").error, "restore drill target cell no longer attests backup validation protocol 2");
      } else {
        assert.equal(f.storage.values.get("restore-drill").state, "running");
        assert.equal(f.key().phase, "start");
        assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
      }
      assertAccountBackupIsolation(f, originalAlarm);
    });
  }
});

// 14. R2 identity is pinned and a transient account reset during mint retries safely.
test("alarm restore drill verifies archive identity once and retries a dropped capability mint", async (t) => {
  for (const mode of ["reread", "etag", "mint dropped", "mint refused"]) {
    await t.test(mode, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t);
      await startedAlarm(f);
      if (mode === "reread") f.validateArchive = () => validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1), { chunks: 99 });
      if (mode === "etag") f.bucket.write(f.record.object, "valid-backup-object", objectMetadata(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1)), "changed-etag");
      let mintAttempts = 0;
      f.intercept = (entry, forward) => {
        if (entry.path === "/archive-capability") {
          mintAttempts += 1;
          if (mode === "mint dropped" && mintAttempts === 1) throw new Error("mint dropped");
          if (mode === "mint refused") return new Response(null, { status: 404 });
        }
        return forward();
      };
      await f.next();
      assert.equal(cell.count("start"), 0);
      if (mode === "mint dropped") {
        assert.equal(f.storage.values.get("restore-drill").state, "running");
        assert.equal(f.key().phase, "start");
        assert.equal(f.key().starts, 0);
        assert.equal(f.key().reread, true);
        assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
        assert.equal(f.rereads, 1);
        await f.next();
        assert.equal(cell.count("start"), 1);
        assert.equal(f.rereads, 1);
        assert.equal(mintAttempts, 2);
      } else {
        assert.equal(f.storage.values.get("restore-drill").state, "failed");
        assert.equal(f.storage.values.get("restore-drill").error, mode === "reread"
          ? "backup validation reread does not match the committed catalog" : mode === "etag"
            ? "backup R2 object identity does not match its durable authority" : "backup archive capability is not available");
        assert.equal(mintAttempts, mode === "mint refused" ? 1 : 0);
      }
    });
  }
});

// 15. A slow old drill and a delayed prepare response cannot rewrite a newer owner.
test("alarm restore drill stale tick and stale prepare bind preserve current driver ownership", async (t) => {
  await t.test("old START failure cannot mutate the drill that superseded it", async (t) => {
    const f = alarmDrillEnv(t);
    const entered = deferred();
    const held = deferred();
    const cell = fakeCell(t, { start: () => { entered.resolve(); return held.promise; } });
    const first = await startedAlarm(f);
    const ticking = f.next();
    await entered.promise;
    f.setTime(Date.parse(first.deadline_at) + 1);
    const second = await startedAlarm(f);
    assert.notEqual(second.drill_id, first.drill_id);
    const secondKey = structuredClone(f.key(second.drill_id));
    const secondRecord = structuredClone(f.storage.values.get("restore-drill"));
    held.resolve({ schema_version: "witself.v0", error: "invalid or mismatched backup archive" });
    await ticking;
    assert.equal(f.key(first.drill_id), undefined);
    samePrivateState(f.key(second.drill_id), secondKey, "old tick leaves newer key byte-identical");
    assert.deepEqual(f.storage.values.get("restore-drill"), secondRecord);
    assert.equal(callsTo(f, "/validation-verified").length, 0);
    assert.deepEqual(callsTo(f, "/restore-drill:finish").map((entry) => entry.input.drill_id), [first.drill_id]);
    assert.equal(cell.count("start"), 1);
  });
  await t.test("a held first claim answer cannot bind over the reconciled START", async (t) => {
    const f = alarmDrillEnv(t);
    const cell = fakeCell(t);
    const entered = deferred();
    const held = deferred();
    let firstClaim = true;
    f.intercept = async (entry, forward) => {
      const response = await forward();
      if (entry.path === "/restore-drill:start" && firstClaim) {
        firstClaim = false;
        entered.resolve();
        await held.promise;
      }
      return response;
    };
    const body = await f.prepareBody();
    const preparing = f.prepare(body);
    await entered.promise;
    assert.equal(f.key(body.drill_id).phase, "claim");
    await f.next();
    assert.equal(f.key(body.drill_id).phase, "start");
    await f.next();
    assert.equal(f.key(body.drill_id).phase, "poll");
    assert.equal(f.key(body.drill_id).starts, 1);
    const expected = structuredClone(f.key(body.drill_id));
    const put = t.mock.method(f.driverStorage, "put");
    const before = put.mock.callCount();
    held.resolve();
    assert.equal((await preparing).status, 200);
    assert.equal(put.mock.callCount(), before);
    samePrivateState(f.key(body.drill_id), expected, "stale bind preserves poll progress byte-identically");
    await f.next();
    assert.equal(cell.count("start"), 1);
    assert.equal(cell.count("poll"), 1);
  });
});

// 16. Recovery is armed before I/O, and retry times use the clock after long work.
test("alarm restore drill arms crash recovery and schedules from commit time", async (t) => {
  await t.test("a killed held probe resumes through the driver's recovery alarm", async (t) => {
    const f = alarmDrillEnv(t);
    const cell = fakeCell(t);
    await startedAlarm(f);
    const entered = deferred();
    const held = deferred();
    cell.set("version", () => { entered.resolve(); return held.promise; });
    const invokedAt = f.now().getTime();
    const abandoned = fireAlarm(f.driverStorage, f.driver);
    await entered.promise;
    assert.equal(f.driverStorage.alarm, invokedAt + RESTORE_DRILL_TICK_MS);
    assert.throws(() => fireAlarm(f.driverStorage, f.driver), /an alarm invocation is already running/);
    abandonAlarm(f.driver);
    // Never resolve this promise: a killed platform invocation executes no catch or tail.
    void abandoned;
    cell.set("version", { schema_version: "witself.v0", backup_validation_protocol: 2 });
    cell.set("start", () => cellAck(f.record));
    f.setTime(invokedAt + RESTORE_DRILL_TICK_MS + 1);
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(f.storage.values.get("restore-drill").state, "validated");
    assert.equal(cell.count("start"), 1);
    assert.equal(f.driverStorage.alarm, null);
  });
  await t.test("a three-minute reread schedules a future tick from its completion", async (t) => {
    const f = alarmDrillEnv(t);
    fakeCell(t);
    f.validateArchive = () => {
      f.advance(3 * 60_000);
      return validVerification(backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1));
    };
    await startedAlarm(f);
    await f.next();
    assert.equal(f.key().phase, "poll");
    assert.equal(f.key().next_at, f.now().getTime() + RESTORE_DRILL_TICK_MS);
    assert.equal(f.driverStorage.alarm, f.key().next_at);
    assert.ok(f.driverStorage.alarm > f.now().getTime());
    assert.equal(f.rereads, 1);
  });
  await t.test("an idle driver deletes its consumed alarm and makes no request", async (t) => {
    const f = alarmDrillEnv(t);
    const cell = fakeCell(t);
    f.driverStorage.alarm = f.now().getTime();
    await fireAlarm(f.driverStorage, f.driver);
    assert.equal(f.driverStorage.alarm, null);
    assert.equal(cell.calls.length, 0);
    assert.equal(f.log.length, 0);
  });
  for (const [label, value] of [["object", { phase: "invalid" }], ["null", null], ["false", false]]) {
    await t.test(`invalid private state ${label} is removed without external requests`, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t);
      const log = t.mock.method(console, "log", () => {});
      f.driverStorage.values.set("restore-drill-driver:" + crypto.randomUUID(), value);
      f.driverStorage.alarm = f.now().getTime();
      await fireAlarm(f.driverStorage, f.driver);
      assert.equal(driverKeys(f).length, 0);
      assert.equal(f.driverStorage.alarm, null);
      assert.equal(cell.calls.length, 0);
      assert.equal(f.log.length, 0);
      assert.deepEqual(log.mock.calls.map((call) => call.arguments), [["account-backup: restore drill driver state is invalid"]]);
    });
  }
});

// 17. The separate named object cannot operate the account's backup executor.
test("alarm restore drill leaves backup recovery alarms untouched and rejects backup routes", async (t) => {
  const f = alarmDrillEnv(t);
  const preparingBackup = runtime();
  const nextBackup = backupJobIdentity(ACCOUNT, SCHEDULED_AT + 60_000, 1);
  assert.equal((await preparingBackup.instance.fetch(preparingBackup.request("/start", nextBackup))).status, 202);
  const pending = structuredClone(preparingBackup.storage.values.get("account-backups").current_job);
  pending.status = "retrying";
  pending.attempts = 1;
  pending.retry_at = new Date(f.now().getTime() + 5 * 60_000).toISOString();
  const state = f.storage.values.get("account-backups");
  state.current_job = pending;
  f.storage.alarm = Date.parse(pending.retry_at);
  const originalAlarm = f.storage.alarm;
  const originalJob = structuredClone(pending);
  const originalRevision = state.revision;
  const arm = t.mock.method(f.storage, "setAlarm");
  const disarm = t.mock.method(f.storage, "deleteAlarm");
  const writes = t.mock.method(f.storage, "put");
  fakeCell(t, { start: () => cellAck(f.record) });
  await startedAlarm(f);
  await f.next();
  assert.equal(f.storage.values.get("restore-drill").state, "validated");
  assert.equal(arm.mock.callCount(), 0);
  assert.equal(disarm.mock.callCount(), 0);
  assert.equal(f.storage.alarm, originalAlarm);
  assert.equal(f.storage.values.get("account-backups").revision, originalRevision + 1);
  samePrivateState(f.storage.values.get("account-backups").current_job, originalJob, "receipt leaves retrying backup job untouched");
  assert.equal(writes.mock.calls.filter((call) => call.arguments[0] === "account-backups").length, 1);
  assert.equal(callsTo(f, "/validation-verified").length, 1);
  const driverWrites = t.mock.method(f.driverStorage, "put");
  const driverArm = t.mock.method(f.driverStorage, "setAlarm");
  const driverDisarm = t.mock.method(f.driverStorage, "deleteAlarm");
  for (const path of ["/run", "/start", "/status", "/archive-capability", "/validation-verified", "/restore-drill:start", "/restore-drill:finish"]) {
    const request = path === "/status" ? new Request("http://account-backup.internal/status")
      : preparingBackup.request(path, nextBackup);
    const response = await f.driver.fetch(request);
    assert.equal(response.status, 404, path);
    assert.equal((await response.json()).error, "account backup endpoint not found", path);
  }
  assert.equal(driverWrites.mock.callCount(), 0);
  assert.equal(driverArm.mock.callCount(), 0);
  assert.equal(driverDisarm.mock.callCount(), 0);
  assert.equal(driverKeys(f).length, 0);
  for (const suffix of ["", "invalid:account"]) {
    const invalidStorage = new Storage();
    const invalidDriver = new DurableAccountBackup({ storage: invalidStorage,
      id: { name: RESTORE_DRILL_DRIVER_PREFIX + suffix } }, f.env, { now: f.now });
    const response = await invalidDriver.fetch(new Request("http://account-backup.internal/restore-drill-driver:start", {
      method: "POST", body: "{}",
    }));
    assert.equal(response.status, 404);
    assert.equal((await response.json()).error, "account backup endpoint not found");
    await fireAlarm(invalidStorage, invalidDriver);
    assert.equal(invalidStorage.alarm, null);
    assert.equal(invalidStorage.values.size, 0);
  }
  const backup = runtime();
  const exports = t.mock.method(backup.instance, "fetchImpl");
  assert.equal((await backup.instance.fetch(backup.request("/start"))).status, 202);
  await fireAlarm(backup.storage, backup.instance);
  assert.equal(exports.mock.callCount(), 1);
  assert.equal(backup.storage.values.get("account-backups").current_job.status, "committed");
});

// 18. Status publishes only the existing record and its one additive driver field.
test("alarm restore drill status retains eleven ordered public fields and no driver secrets", async (t) => {
  for (const outcome of ["validated", "failed"]) {
    await t.test(outcome, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t);
      const record = await startedAlarm(f);
      const digest = f.key(record.drill_id).target.backup_token_sha256;
      const check = async (state) => {
        for (const [response, publicRoute] of [[await f.status(), false], [await f.publicStatus(), true]]) {
          assert.equal(response.status, 200);
          const body = await response.json();
          const drill = publicRoute ? body.account.restore_drill : body.restore_drill;
          assert.deepEqual(Object.keys(drill), alarmRecordFields);
          assert.equal(drill.drill_id, record.drill_id);
          assert.equal(drill.driver, "alarm");
          assert.equal(drill.state, state);
          const serialized = JSON.stringify(drill);
          for (const privateField of ["archive_origin", "target", "catalog", "backup_token_sha256", "next_at", "fence_failures", "reread", "ack", "finishing", "rev", "claim_by"]) {
            assert.ok(!Object.hasOwn(drill, privateField), `record excludes ${privateField}`);
          }
          assert.ok(!/[0-9a-f]{64}/.test(serialized), "record contains no hash-shaped value");
          assert.ok(!serialized.includes("validation.example"), "record contains no private endpoint");
          const all = JSON.stringify(body);
          assert.ok(!all.includes(digest), "status contains no pinned token hash");
          assert.ok(!all.includes("validation.example"), "status contains no private endpoint");
        }
      };
      await check("running");
      cell.set("start", outcome === "validated" ? () => cellAck(f.record)
        : { schema_version: "witself.v0", error: "could not validate backup" });
      await f.next();
      await check(outcome);
    });
  }
});


// Waiting clients only observe the record; driver execution is explicit in tests.
const RECORD_WAIT_MS = 5000;
async function waitForAlarmDrillRecord(f) {
  const recordDeadline = performance.now() + RECORD_WAIT_MS;
  while (!f.storage.values.has("restore-drill") && performance.now() < recordDeadline) await flush();
  assert.ok(f.storage.values.has("restore-drill"), "Worker must record a drill");
  // The first /status read is catalog preflight; the second follows the driver's bind.
  const waiterReady = () => f.key()?.phase === "start" && callsTo(f, "/status").length >= 2;
  const waiterDeadline = performance.now() + RECORD_WAIT_MS;
  while (!waiterReady() && performance.now() < waiterDeadline) await flush();
  assert.ok(waiterReady(), "Worker must bind the drill driver and read its status");
  return structuredClone(f.storage.values.get("restore-drill"));
}

function assertAlarmAnswer(response, drill) {
  assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), drill.drill_id);
  assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), "alarm");
}

function assertDrillSuccess(body, f, drill) {
  assert.deepEqual(Object.keys(body), ["schema_version", "validated", "account_id", "backup_id", "target_cell", "validated_at", "status", "archive_schema_version", "drill_id"]);
  assert.deepEqual(body, {
    schema_version: "witself.v0", validated: true, ...f.input,
    validated_at: f.storage.values.get("restore-drill").validated_at,
    status: f.record.status, archive_schema_version: f.record.archive_schema_version,
    drill_id: drill.drill_id,
  });
}

test("restore drill Worker selection, driver headers, and wait grammar", async (t) => {
  for (const mode of ["protocol2", "missing", "unavailable", "thrown"]) {
    await t.test(mode, async (t) => {
      const f = alarmDrillEnv(t);
      const versions = {
        protocol2: () => Response.json({ schema_version: "witself.v0", backup_validation_protocol: 2 }),
        missing: () => Response.json({ schema_version: "witself.v0" }),
        unavailable: () => new Response(null, { status: 503 }),
        thrown: () => { throw new Error("version unavailable"); },
      };
      const cell = fakeCell(t, { version: versions[mode] });
      // The synchronous path's real reread deliberately fails, as in the existing Worker tests.
      f.bucket.get = async () => null;
      const response = await f.route(mode === "protocol2" ? { wait: false } : {});
      const drill = f.storage.values.get("restore-drill");
      assert.equal(response.status, mode === "protocol2" ? 202 : 502);
      assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), drill.drill_id);
      assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), mode === "protocol2" ? "alarm" : "request");
      assert.equal(drill.driver, mode === "protocol2" ? "alarm" : undefined);
      assert.equal(cell.count("start"), 0);
      if (mode !== "protocol2") {
        assert.equal(f.log.some(({ name }) => name.startsWith(RESTORE_DRILL_DRIVER_PREFIX)), false);
        assert.equal(f.driverStorage.values.size, 0);
      }
    });
  }
  for (const [body, error] of [
    [{ wait: "no" }, "wait must be a boolean"],
    [{ wait: false, heartbeat: true }, "heartbeat requires a waiting request"],
  ]) {
    await t.test(error, async (t) => {
      const f = alarmDrillEnv(t);
      const cell = fakeCell(t);
      const response = await f.route(body);
      assert.equal(response.status, 400);
      assert.deepEqual(await response.json(), { schema_version: "witself.v0", error });
      assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), null);
      assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), null);
      assert.equal(f.storage.values.has("restore-drill"), false);
      assert.equal(f.driverStorage.values.size, 0);
      assert.equal(cell.calls.length, 0);
    });
  }
  for (const version of [
    () => Response.json({ schema_version: "witself.v0" }),
    () => { throw new Error("version unavailable"); },
  ]) {
    await t.test("wait false refuses unconfirmed protocol before recording", async (t) => {
      const f = alarmDrillEnv(t);
      fakeCell(t, { version });
      const response = await f.route({ wait: false });
      assert.equal(response.status, 409);
      assert.deepEqual(await response.json(), { schema_version: "witself.v0", error: "asynchronous restore drill requires a target cell with backup validation protocol 2" });
      assert.equal(response.headers.get("X-Witself-Restore-Drill-ID"), null);
      assert.equal(response.headers.get("X-Witself-Restore-Drill-Driver"), null);
      assert.equal(f.storage.values.has("restore-drill"), false);
      assert.equal(f.driverStorage.values.size, 0);
      assert.equal(f.driverStorage.alarm, null);
      assert.equal(f.log.some(({ path }) => path === "/restore-drill:start"), false);
    });
  }
});

test("restore drill Worker wait false returns 202 without starting a waiter", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const f = alarmDrillEnv(t);
  fakeCell(t, { start: () => Response.json(cellAck(f.record)) });
  const response = await f.route({ wait: false });
  const drill = structuredClone(f.storage.values.get("restore-drill"));
  assert.equal(response.status, 202);
  assertAlarmAnswer(response, drill);
  assert.deepEqual(await response.json(), { schema_version: "witself.v0", account_id: ACCOUNT, restore_drill: drill });
  assert.equal(drill.driver, "alarm");
  assert.equal(drill.state, "running");
  assert.equal(f.driverStorage.alarm, f.now().getTime());
  const reads = f.log.filter(({ path }) => path === "/status").length;
  assert.equal(reads, 1, "only the catalog preflight may read status before the 202");
  t.mock.timers.tick(3 * RESTORE_DRILL_WAIT_POLL_MS);
  await flush();
  assert.equal(f.log.filter(({ path }) => path === "/status").length, reads, "202 must not start a waiter");
  await fireAlarm(f.driverStorage, f.driver);
  assert.equal(f.storage.values.get("restore-drill").state, "validated");
  assert.equal(f.storage.values.get("account-backups").catalog[0].validations.length, 1);
});

test("restore drill Worker default framing waits for the durable outcome with existing statuses", async (t) => {
  for (const mode of ["validated", "cell-error", "fence-error"]) {
    await t.test(mode, async (t) => {
      t.mock.timers.enable({ apis: ["setTimeout"] });
      const f = alarmDrillEnv(t);
      fakeCell(t, { start: () => Response.json(mode === "cell-error"
        ? { schema_version: "witself.v0", error: "invalid or mismatched backup archive" }
        : cellAck(f.record)) });
      let settled = false;
      const pending = f.route().then((response) => { settled = true; return response; });
      const drill = await waitForAlarmDrillRecord(f);
      assert.equal(drill.state, "running");
      assert.equal(settled, false);
      if (mode === "fence-error") {
        f.binding.values.set(`cell:${TARGET}`, JSON.stringify({ ...f.binding.value(`cell:${TARGET}`), accepting: true }));
      }
      await fireAlarm(f.driverStorage, f.driver);
      assert.equal(settled, false, "the waiting request resumes on its next status poll");
      t.mock.timers.tick(RESTORE_DRILL_WAIT_POLL_MS);
      await flush();
      const response = await pending;
      assertAlarmAnswer(response, drill);
      assert.equal(response.status, mode === "validated" ? 200 : mode === "fence-error" ? 409 : 502);
      const body = await response.json();
      if (mode === "validated") assertDrillSuccess(body, f, drill);
      else assert.deepEqual(body, { schema_version: "witself.v0", error: f.storage.values.get("restore-drill").error });
    });
  }
});

test("restore drill Worker alarm driver completes with no client and never uses waitUntil", async (t) => {
  for (const heartbeat of [true, false]) {
    for (const withWaitUntil of [false, true]) {
      await t.test(`${heartbeat ? "cancelled heartbeat" : "unawaited default"}, waitUntil ${withWaitUntil}`, async (t) => {
        t.mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
        const errors = unhandledRejections(t);
        const f = alarmDrillEnv(t);
        const cell = fakeCell(t, { start: () => Response.json(cellAck(f.record)) });
        let waitUntilCalls = 0;
        const ctx = withWaitUntil ? { waitUntil() { waitUntilCalls += 1; } } : {};
        const pending = f.route(heartbeat ? { heartbeat: true } : {}, ctx);
        const drill = await waitForAlarmDrillRecord(f);
        assert.equal(drill.state, "running");
        assert.equal(drill.driver, "alarm");
        assert.equal(cell.count("version"), 1);
        assert.equal(cell.count("start"), 0);
        assert.equal(cell.count("poll"), 0);
        assert.equal(cell.count("sync"), 0);
        if (heartbeat) {
          const response = await pending;
          assertAlarmAnswer(response, drill);
          assert.equal(response.status, 200);
          const reader = response.body.getReader();
          t.mock.timers.tick(RESTORE_DRILL_HEARTBEAT_MS);
          assert.equal(new TextDecoder().decode((await reader.read()).value), "\n");
          await reader.cancel();
          await flush();
        }
        const duplicate = await f.route({ wait: false });
        assert.equal(duplicate.status, 409);
        assert.deepEqual(await duplicate.json(), { schema_version: "witself.v0", error: "restore drill already running", restore_drill: drill });
        await fireAlarm(f.driverStorage, f.driver);
        const finished = f.storage.values.get("restore-drill");
        assert.equal(finished.state, "validated");
        assert.equal(finished.drill_id, drill.drill_id);
        assert.equal(finished.driver, "alarm");
        assert.equal(f.storage.values.get("account-backups").catalog[0].validations.length, 1);
        assert.equal(waitUntilCalls, 0);
        assert.equal(cell.count("sync"), 0);
        t.mock.timers.tick(RESTORE_DRILL_WAIT_POLL_MS);
        await flush();
        await flush();
        if (!heartbeat) {
          const response = await pending;
          assert.equal(response.status, 200);
          assertAlarmAnswer(response, drill);
          assertDrillSuccess(await response.json(), f, drill);
        }
        assert.deepEqual(errors, []);
      });
    }
  }
});

function alarmWaiterFixture() {
  const f = drillEnv();
  let clock = NOW.getTime();
  f.durable.now = () => new Date(clock);
  const drill = drillRecord(f.record, {
    driver: "alarm", deadline_at: new Date(clock + RESTORE_DRILL_ALARM_DEADLINE_MS).toISOString(),
  });
  f.storage.values.set("restore-drill", drill);
  const dependencies = { now: () => new Date(clock), sleep: async () => { assert.fail("unexpected waiter sleep"); } };
  const wait = () => awaitRestoreDrill(f.env, f.input, f.record, drill.drill_id, dependencies);
  const succeed = (validatedAt = new Date(clock).toISOString()) => {
    f.storage.values.set("restore-drill", { ...drill, state: "validated", validated_at: validatedAt, finished_at: new Date(clock).toISOString() });
  };
  return { ...f, drill, dependencies, wait, succeed, setTime: (ms) => { clock = ms; } };
}

test("restore drill waiter handles supersession, bounded read failures, deadline grace and catalog precedence", async (t) => {
  await t.test("superseded record", async () => {
    const f = alarmWaiterFixture();
    f.storage.values.set("restore-drill", { ...f.drill, drill_id: crypto.randomUUID() });
    await assert.rejects(f.wait(), { message: "restore drill outcome is no longer readable; read the catalog validations" });
  });
  for (const failureCount of [1, 6]) {
    await t.test(`${failureCount} status read failures`, async () => {
      const f = alarmWaiterFixture();
      const original = f.env.ACCOUNT_BACKUP;
      let reads = 0, sleeps = 0;
      f.env.ACCOUNT_BACKUP = {
        idFromName: original.idFromName,
        get: (id) => ({ fetch: (request) => {
          assert.equal(new URL(request.url).pathname, "/status");
          reads += 1;
          if (reads <= failureCount) throw new Error("status temporarily unavailable");
          return original.get(id).fetch(request);
        } }),
      };
      f.dependencies.sleep = async (ms) => { assert.equal(ms, RESTORE_DRILL_WAIT_POLL_MS); sleeps += 1; };
      f.succeed();
      if (failureCount === 6) {
        await assert.rejects(f.wait(), { message: "restore drill status is unavailable" });
        assert.equal(reads, 6);
        assert.equal(sleeps, 5);
      } else {
        const result = await f.wait();
        assert.equal(result.validated, true);
        assert.equal(reads, 2);
        assert.equal(sleeps, 1);
      }
    });
  }
  await t.test("final report inside grace", async () => {
    const f = alarmWaiterFixture();
    const deadline = Date.parse(f.drill.deadline_at);
    f.setTime(deadline);
    assert.equal((await (await f.status()).json()).restore_drill.state, "failed");
    let sleeps = 0;
    f.dependencies.sleep = async (ms) => {
      assert.equal(ms, RESTORE_DRILL_WAIT_POLL_MS);
      sleeps += 1;
      f.setTime(deadline + RESTORE_DRILL_FINAL_GRACE_MS / 2);
      f.succeed();
    };
    const result = await f.wait();
    assert.equal(result.validated, true);
    assert.equal(result.validated_at, new Date(deadline + RESTORE_DRILL_FINAL_GRACE_MS / 2).toISOString());
    assert.equal(sleeps, 1);
  });
  await t.test("unreported after grace", async () => {
    const f = alarmWaiterFixture();
    f.setTime(Date.parse(f.drill.deadline_at) + RESTORE_DRILL_FINAL_GRACE_MS);
    await assert.rejects(f.wait(), { message: "restore drill did not report before its deadline" });
  });
  for (const offset of [-1, 0, 1]) {
    await t.test(`catalog validation timestamp offset ${offset}`, async () => {
      const f = alarmWaiterFixture();
      f.storage.values.set("restore-drill", { ...f.drill, state: "failed", finished_at: NOW.toISOString(), error: "recorded drill failure" });
      const validatedAt = new Date(Date.parse(f.drill.started_at) + offset).toISOString();
      f.storage.values.get("account-backups").catalog[0].validations = [{
        target_cell: TARGET, validated_at: validatedAt,
        status: f.record.status, archive_schema_version: f.record.archive_schema_version,
      }];
      if (offset < 0) await assert.rejects(f.wait(), { message: "recorded drill failure" });
      else {
        const result = await f.wait();
        assert.deepEqual(Object.keys(result), ["schema_version", "validated", "account_id", "backup_id", "target_cell", "validated_at", "status", "archive_schema_version", "drill_id"]);
        assert.deepEqual(result, { schema_version: "witself.v0", validated: true, ...f.input,
          validated_at: validatedAt, status: f.record.status, archive_schema_version: f.record.archive_schema_version, drill_id: f.drill.drill_id });
      }
    });
  }
  await t.test("another cell's newer catalog validation cannot override a failed drill", async () => {
    const f = alarmWaiterFixture();
    f.storage.values.set("restore-drill", {
      ...f.drill, state: "failed", finished_at: NOW.toISOString(), error: "recorded drill failure",
    });
    f.storage.values.get("account-backups").catalog[0].validations = [{
      target_cell: "civo-fixture-other-target",
      validated_at: new Date(Date.parse(f.drill.started_at) + 1).toISOString(),
      status: f.record.status, archive_schema_version: f.record.archive_schema_version,
    }];
    await assert.rejects(f.wait(), { message: "recorded drill failure" });
  });
});

test("restore drill docs describe the alarm driver and the request-driver fallback", async () => {
  const { readFile } = await import("node:fs/promises");
  const required = [
    "continues to a recorded outcome whatever the client does",
    "A drill whose record has no `driver` field runs inside the request that started it",
    "validated_at >= restore_drill.started_at",
    "asynchronous restore drill requires a target cell with backup validation protocol 2",
    "may still have been recorded",
  ];
  const forbidden = [
    "A fix that takes the client connection out of the drill is tracked in",
    "issues/648", "cannot run under the Durable Object alarm",
    "The drill still depends on one connected request", "never cancels a drill because the client went away",
    "The drill runs inside the request that started it", "The drill runs inside this request",
    "A step-driven cell-side drill remains the follow-up",
  ];
  for (const file of ["api-routes.md", "backup-and-recovery.md", "runbooks.md"]) {
    const text = (await readFile(new URL(`../../../../docs/${file}`, import.meta.url), "utf8")).replace(/\s+/g, " ");
    const needs = [...required];
    const rejects = [...forbidden];
    if (file === "api-routes.md") needs.push("`wait?`", "HTTP 202", "X-Witself-Restore-Drill-Driver");
    if (file === "backup-and-recovery.md") needs.push("`wait: false`", "from a Durable Object alarm of its own");
    if (file === "runbooks.md") {
      needs.push('\\"wait\\":false}', "curl --fail-with-body -i -m 1800 -X POST", "X-Witself-Restore-Drill-Driver", "comes from a control plane older than this release", "If no answer arrives within a minute, leave the command running");
      rejects.push("curl --fail-with-body -i -m 120");
      assert.ok(text.indexOf("If no answer arrives within a minute, leave the command running") < text.indexOf('\\"wait\\":false}'), `${file}: older-control-plane warning must precede the asynchronous curl`);
    }
    for (const fragment of needs) assert.ok(text.includes(fragment), `${file}: missing ${fragment}`);
    for (const fragment of rejects) assert.equal(text.includes(fragment), false, `${file}: forbidden ${fragment}`);
  }
});
