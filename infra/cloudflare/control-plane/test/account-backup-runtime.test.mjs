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
  accountBackupHealth,
  accountBackupStatus,
  backupJobIdentity,
  DurableAccountBackup,
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

function backupNamespace(record, receipts) {
  return {
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async (request) => {
        assert.equal(id.name, ACCOUNT);
        const path = new URL(request.url).pathname;
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
