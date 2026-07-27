import assert from "node:assert/strict";
import test from "node:test";

import {
  ACCOUNT_BACKUP_SCAN_KEY,
  ACCOUNT_BACKUP_STATE_SCHEMA,
  backupJobIdentity,
  DurableAccountBackup,
  runAccountBackupValidation,
  runScheduledAccountBackups,
} from "../src/account-backup-runtime.mjs";

const ACCOUNT = "acct_backup";
const SOURCE = "aws-us-west-2";
const TARGET = "civo-phx1";
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
      ? { body: new Response(value.bytes).body }
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
    return new Response("archive", {
      status: 200,
      headers: { "X-Witself-Backup-ID": job.backup_id },
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
  const request = () =>
    new Request("http://account-backup.internal/run", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...job,
        max_attempts: maxAttempts,
        catalog_limit: 8,
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
        headers: {
          "X-Witself-Backup-ID":
            backupJobIdentity(ACCOUNT, SCHEDULED_AT, 1).backup_id,
        },
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
        headers: {
          "X-Witself-Backup-ID": new Headers(options.headers).get(
            "X-Witself-Backup-ID",
          ),
        },
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
        headers: {
          "X-Witself-Backup-ID": new Headers(options.headers).get(
            "X-Witself-Backup-ID",
          ),
        },
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
        assert.ok(options.body);
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
  assert.deepEqual(bucket.conditionedGets, [{
    key: record.object,
    etagMatches: record.r2_etag,
  }]);
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
