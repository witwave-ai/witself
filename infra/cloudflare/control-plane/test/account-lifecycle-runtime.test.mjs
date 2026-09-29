import assert from "node:assert/strict";
import test from "node:test";

import {
  AccountLifecycleRuntimeError,
  DurableAccountLifecycle,
} from "../src/account-lifecycle-runtime.mjs";
import {
  ArchiveIntegrityError,
  validateCommittedAccountArchive,
} from "../src/archive-integrity.mjs";
import {
  DurableTargetCellCoordinator,
} from "../src/target-cell-coordinator.mjs";
import {
  acknowledgeStep,
  bootstrapArchivedState,
  bootstrapLiveState,
  claimOperation,
  commitArchived,
  commitLive,
  markProjectionApplied,
  nextLifecycleStep,
  validateLifecycleState,
} from "../src/account-lifecycle-state.mjs";

const ACCOUNT = "acct_runtime";
const SOURCE = "aws-us-west-2";
const TARGET = "civo-fixture-target";
const OPERATION_ID = "11111111-1111-4111-8111-111111111111";
const SOURCE_EVACUATION_ID =
  "22222222-2222-4222-8222-222222222222";

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

class CommitThenThrowStorage extends Storage {
  constructor() {
    super();
    this.thrown = false;
  }

  async put(key, value) {
    await super.put(key, value);
    if (
      !this.thrown &&
      value?.operation?.phase === "archive_committed"
    ) {
      this.thrown = true;
      throw new Error("lost Durable Object storage acknowledgement");
    }
  }
}

class PhaseCommitThenThrowStorage extends Storage {
  constructor(phase) {
    super();
    this.phase = phase;
    this.thrown = false;
  }

  async put(key, value) {
    await super.put(key, value);
    if (!this.thrown && value?.operation?.phase === this.phase) {
      this.thrown = true;
      throw new Error(`lost ${this.phase} storage acknowledgement`);
    }
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
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value) {
    this.values.set(key, value);
  }

  async delete(key) {
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
    this.failDeleteOnce = false;
  }

  async head(key) {
    if (!this.values.has(key)) return null;
    return { size: new Blob([this.values.get(key)]).size, etag: "fixture-etag" };
  }

  async get(key) {
    if (!this.values.has(key)) return null;
    return {
      ...await this.head(key),
      body: new Response(this.values.get(key)).body,
    };
  }

  async delete(key) {
    if (this.failDeleteOnce) {
      this.failDeleteOnce = false;
      throw new Error("temporary R2 delete failure");
    }
    this.deleted.push(key);
    this.values.delete(key);
  }
}

class ErroringBodyBucket extends Bucket {
  constructor(error) {
    super();
    this.error = error;
  }

  async get(key) {
    if (!this.values.has(key)) return null;
    const error = this.error;
    return {
      ...await this.head(key),
      body: new ReadableStream({
        pull(controller) {
          controller.error(error);
        },
      }),
    };
  }
}

test("account lifecycle orders custom domains around alias identity", async () => {
  const events = [];
  const runtime = new DurableAccountLifecycle(
    { storage: new Storage(), id: { name: ACCOUNT } },
    {},
    {
      reconcileAgentEmailDomains: async (_accountID, fence) => {
        events.push(`custom:${fence.action}`);
        return { complete: true };
      },
      reconcileRealmEmailAliases: async (_accountID, fence) => {
        events.push(`alias:${fence.action}`);
        return { complete: true };
      },
    },
  );
  await runtime.reconcileAgentEmailPolicies(ACCOUNT, {
    operation_id: OPERATION_ID,
    epoch: 1,
    action: "suspend",
  });
  await runtime.reconcileAgentEmailPolicies(ACCOUNT, {
    operation_id: OPERATION_ID,
    epoch: 1,
    action: "republish",
  });
  assert.deepEqual(events, [
    "custom:suspend", "alias:suspend",
    "alias:republish", "custom:republish",
  ]);
});

function sourceRoute(epoch) {
  return {
    cell: SOURCE,
    endpoint: "https://source.example",
    region: "us-west",
    cell_registration_id: `reg-${SOURCE}`,
    ...(epoch === undefined ? {} : { epoch }),
  };
}

function cell(name, endpoint, accepting = true) {
  return {
    name,
    endpoint,
    region: "us-west",
    region_code: name === SOURCE ? "us-west-2" : "nyc1",
    accepting,
    provision_token: `token-${name}`,
    registration_id: `reg-${name}`,
    registered_at: "2026-07-25T00:00:00.000Z",
  };
}

function exactTargetReservation(left, right) {
  return left?.account_id === right?.account_id &&
    left?.operation_id === right?.operation_id &&
    left?.evacuation_id === right?.evacuation_id &&
    left?.kind === right?.kind &&
    left?.target_cell === right?.target_cell &&
    left?.target_registration_id === right?.target_registration_id &&
    left?.epoch === right?.epoch;
}

class TargetAuthority {
  constructor(directory) {
    this.directory = directory;
    this.reservations = new Map();
    this.residents = new Map();
    this.departed = [];
  }

  key(cellName, accountID) {
    return `${cellName}:${accountID}`;
  }

  async request(cellName, path, payload) {
    const reservation = payload?.reservation;
    const key = reservation
      ? this.key(cellName, reservation.account_id)
      : null;
    if (path === "/reserve") {
      const resident = this.residents.get(key);
      if (
        resident?.operation_id === reservation.operation_id &&
        resident?.evacuation_id === reservation.evacuation_id
      ) {
        return { ok: true, resident: true, expires_at: null };
      }
      const current = this.reservations.get(key);
      if (current && !exactTargetReservation(current, reservation)) {
        const error = new Error("conflicting target reservation");
        error.status = 409;
        throw error;
      }
      const target = this.directory.value(`cell:${cellName}`);
      if (
        !current &&
        (
          !target ||
          target.accepting === false ||
          target.registration_id !==
            reservation.target_registration_id
        )
      ) {
        const error = new Error("target cell cannot be reserved");
        error.status = 409;
        throw error;
      }
      this.reservations.set(key, structuredClone(reservation));
      return {
        ok: true,
        account_id: reservation.account_id,
        operation_id: reservation.operation_id,
        expires_at: "2026-07-25T12:05:00.000Z",
      };
    }
    if (path === "/registration-status") {
      const current = this.directory.value(`cell:${cellName}`);
      const currentRegistration =
        current?.registration_id ?? current?.registered_at ?? null;
      return {
        ok: true,
        cell_name: cellName,
        expected_registration_id: payload.registration_id,
        registration_status: currentRegistration
          ? currentRegistration === payload.registration_id
            ? "active"
            : "replaced"
          : "deleted",
        current_registration_id: currentRegistration,
        tombstone_registration_id:
          currentRegistration ? null : payload.registration_id,
        active_cell:
          currentRegistration === payload.registration_id
            ? current
            : null,
      };
    }
    if (path === "/release") {
      const current = this.reservations.get(key);
      if (current && !exactTargetReservation(current, reservation)) {
        const error = new Error(
          "target reservation changed before exact release",
        );
        error.status = 409;
        throw error;
      }
      this.reservations.delete(key);
      return { ok: true, released: true };
    }
    if (path === "/promote") {
      const current = this.reservations.get(key);
      if (!current && !this.residents.has(key)) {
        throw new Error("reservation missing before promotion");
      }
      if (current && !exactTargetReservation(current, reservation)) {
        throw new Error("reservation changed before promotion");
      }
      this.residents.set(key, structuredClone(reservation));
      this.reservations.delete(key);
      return { ok: true, resident: true };
    }
    if (path === "/depart") {
      this.residents.delete(this.key(cellName, payload.account_id));
      this.departed.push({
        cell_name: cellName,
        account_id: payload.account_id,
        operation_id: payload.operation_id,
        source_epoch: payload.source_epoch,
      });
      return { ok: true, departed: true };
    }
    throw new Error(`unexpected target authority path ${path}`);
  }
}

const targetAuthorities = new WeakMap();

function context(storage = new Storage()) {
  return {
    id: { name: ACCOUNT },
    storage,
  };
}

function request(action, fields = {}) {
  return new Request("https://account-lifecycle.internal/run", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      account_id: ACCOUNT,
      action,
      ...fields,
    }),
  });
}

function accountRequest(accountID, action, fields = {}) {
  return new Request("https://account-lifecycle.internal/run", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      account_id: accountID,
      action,
      ...fields,
    }),
  });
}

function protocolResponse(enabled = true) {
  return Response.json({
    schema_version: "witself.v0",
    account_evacuation_protocol: enabled ? 1 : 0,
  });
}

test("default lifecycle fetch preserves the platform receiver", async (t) => {
  let receiver = null;
  t.mock.method(globalThis, "fetch", function () {
    receiver = this;
    return protocolResponse();
  });
  const coordinator = new DurableAccountLifecycle(context(), {});

  await coordinator.requireEvacuationProtocol({
    name: TARGET,
    endpoint: "https://target.example",
  });

  assert.equal(receiver, globalThis);
});

test("route retirement stays in place until alias suspension fence completes", async () => {
  const object = `archives/${ACCOUNT}/alias-fence.tar.gz`;
  const pointer = {
    archive_id: "archive_alias_fence",
    evacuation_id: OPERATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    status: "suspended",
  };
  let state = bootstrapLiveState({
    account_id: ACCOUNT,
    route: sourceRoute(),
  });
  state = claimOperation(state, {
    operation_id: OPERATION_ID,
    evacuation_id: OPERATION_ID,
    kind: "move",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: { archive_id: pointer.archive_id, object },
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      source_registration_id: `reg-${SOURCE}`,
      target_registration_id: `reg-${TARGET}`,
      request_epoch: 0,
    },
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "target_reserved",
    to_phase: "source_suspended",
  });
  state = commitArchived(state, {
    operation_id: OPERATION_ID,
    archived: pointer,
  });
  state = markProjectionApplied(state, {
    operation_id: OPERATION_ID,
    target: "archive",
    action: "put",
    archive_id: pointer.archive_id,
    archive_object: object,
  });
  const step = nextLifecycleStep(state);
  assert.equal(step.target, "route");
  assert.equal(step.projection.action, "delete");

  const directory = new KV({ [`acct:${ACCOUNT}`]: sourceRoute() });
  const storage = new Storage();
  await storage.put("account-lifecycle", state);
  let attempts = 0;
  const coordinator = new DurableAccountLifecycle(
    context(storage),
    { DIRECTORY: directory, ARCHIVES: new Bucket() },
    {
      reconcileRealmEmailAliases: async (_accountID, fence) => {
        attempts++;
        assert.equal(fence.action, "suspend");
        if (attempts === 1) throw new Error("alias suspension still converging");
      },
    },
  );
  await assert.rejects(
    () => coordinator.applyProjection(state, step),
    /still converging/,
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  assert.equal(storage.values.get("account-lifecycle").operation.phase, "archive_projected");

  await coordinator.applyProjection(state, step);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(storage.values.get("account-lifecycle").operation.phase, "route_retired");
});

test("live route projection replays alias republish before advancing", async () => {
  const object = `archives/${ACCOUNT}/alias-republish.tar.gz`;
  const pointer = {
    archive_id: "archive_alias_republish",
    evacuation_id: OPERATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    status: "suspended",
  };
  let state = archivedOperationState("move", pointer, TARGET);
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "route_retired",
    to_phase: "target_imported",
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "target_imported",
    to_phase: "target_resumed",
  });
  state = commitLive(state, {
    operation_id: OPERATION_ID,
    route: {
      cell: TARGET,
      endpoint: "https://target.example",
      region: "us-west",
      epoch: state.operation.epoch,
    },
  });
  const step = nextLifecycleStep(state);
  assert.equal(step.target, "route");
  assert.equal(step.projection.action, "put");

  const directory = new KV();
  const storage = new Storage();
  await storage.put("account-lifecycle", state);
  let attempts = 0;
  const coordinator = new DurableAccountLifecycle(
    context(storage),
    { DIRECTORY: directory, ARCHIVES: new Bucket() },
    {
      reconcileRealmEmailAliases: async (_accountID, fence) => {
        attempts++;
        assert.equal(fence.action, "republish");
        if (attempts === 1) throw new Error("alias republish still converging");
      },
    },
  );
  await assert.rejects(
    () => coordinator.applyProjection(state, step),
    /still converging/,
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(storage.values.get("account-lifecycle").operation.phase, "live_committed");

  await coordinator.applyProjection(state, step);
  assert.equal(attempts, 2);
  assert.equal(storage.values.get("account-lifecycle").operation.phase, "route_projected");
});

async function roleAwareFetch(fetchImpl, url, init) {
  const response = await fetchImpl(url, init);
  const role = url.endsWith(":begin-evacuation") ||
      url.endsWith(":abort-evacuation")
    ? "source"
    : url.endsWith(":import-evacuation") || url.endsWith(":complete-evacuation")
    ? "target"
    : null;
  if (!role) return response;
  const body = await response.clone().json().catch(() => null);
  if (
    !body ||
    typeof body !== "object" ||
    !body.evacuation_id ||
    body.evacuation_role !== undefined
  ) {
    return response;
  }
  return Response.json(
    { ...body, evacuation_role: role },
    { status: response.status, headers: response.headers },
  );
}

function archivedOperationState(kind, pointer, targetCell = null) {
  let state = bootstrapLiveState({
    account_id: ACCOUNT,
    route: sourceRoute(),
  });
  state = claimOperation(state, {
    operation_id: OPERATION_ID,
    evacuation_id: OPERATION_ID,
    kind,
    source_cell: SOURCE,
    target_cell: targetCell,
    archive: {
      archive_id: pointer.archive_id,
      object: pointer.object,
    },
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      source_registration_id: `reg-${SOURCE}`,
      request_epoch: 0,
      ...(targetCell
        ? { target_registration_id: `reg-${targetCell}` }
        : {}),
    },
  });
  if (kind === "move") {
    state = acknowledgeStep(state, {
      operation_id: OPERATION_ID,
      from_phase: "claimed",
      to_phase: "target_reserved",
    });
  }
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: kind === "move" ? "target_reserved" : "claimed",
    to_phase: "source_suspended",
  });
  state = commitArchived(state, {
    operation_id: OPERATION_ID,
    archived: pointer,
  });
  state = markProjectionApplied(state, {
    operation_id: OPERATION_ID,
    target: "archive",
    action: "put",
    archive_id: pointer.archive_id,
    archive_object: pointer.object,
  });
  state = markProjectionApplied(state, {
    operation_id: OPERATION_ID,
    target: "route",
    action: "delete",
  });
  return validateLifecycleState(state);
}

function runtime({
  directory,
  bucket,
  storage = new Storage(),
  fetch,
  validation = {
    manifest: {
      account_id: ACCOUNT,
      schema_version: 70,
      evacuation_id: SOURCE_EVACUATION_ID,
    },
  },
  randomUUID = () => OPERATION_ID,
  targetAuthority =
    targetAuthorities.get(directory) ??
      new TargetAuthority(directory),
  targetCoordinatorRequest =
    (cellName, path, payload) =>
      targetAuthority.request(cellName, path, payload),
  backupsEnabled = false,
  reconcileRealmEmailAliases,
}) {
  targetAuthorities.set(directory, targetAuthority);
  return new DurableAccountLifecycle(
    context(storage),
    {
      DIRECTORY: directory,
      ARCHIVES: bucket,
      CP_ACCOUNT_BACKUPS_ENABLED: backupsEnabled ? "true" : "false",
    },
    {
      fetch:
        (url, init) => roleAwareFetch(fetch, url, init),
      randomUUID,
      now: () => new Date("2026-07-25T12:00:00.000Z"),
      validateArchive: async (...args) =>
        typeof validation === "function"
          ? validation(...args)
          : validation,
      verifyArchive: async (_bucket, _object, _account, options) => {
        assert.match(options.evacuationID, /^[A-Za-z0-9_-]+$/);
        return {
          manifest: {
            account_id: ACCOUNT,
            schema_version: 70,
            evacuation_id: options.evacuationID,
            exported_at: "2026-07-25T11:59:59.000Z",
            status: "suspended",
          },
        };
      },
      streamArchive: async (archiveBucket, key) => {
        archiveBucket.values.set(key, new Uint8Array([1, 2, 3]));
        return 3;
      },
      targetCoordinatorRequest,
      ...(reconcileRealmEmailAliases
        ? { reconcileRealmEmailAliases }
        : {}),
    },
  );
}

test("lifecycle targets keep legacy cells usable disabled and exclude them enabled", async () => {
  const directory = new KV({
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  const disabled = runtime({ directory, bucket });
  assert.equal(
    (await disabled.cell(TARGET, { target: true })).name,
    TARGET,
  );

  const enabled = runtime({
    directory,
    bucket,
    backupsEnabled: true,
  });
  await assert.rejects(
    () => enabled.cell(TARGET, { target: true }),
    /lacks distinct provision and backup credentials/,
  );

  await directory.put(
    `cell:${TARGET}`,
    JSON.stringify({
      ...cell(TARGET, "https://target.example"),
      backup_token: "backup-target",
    }),
  );
  assert.equal(
    (await enabled.cell(TARGET, { target: true })).name,
    TARGET,
  );
});

test("one durable move carries the exact cell epoch through every verb", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  const calls = [];
  const fetch = async (url, init = {}) => {
    calls.push({ url, init });
    if (url.endsWith("/v1/version")) return protocolResponse();
    if (url.endsWith(":begin-evacuation")) {
      const body = JSON.parse(init.body);
      await directory.put(
        `cell:${TARGET}`,
        JSON.stringify(
          cell(TARGET, "https://target.example", false),
        ),
      );
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: body.evacuation_id,
        status: "suspended",
      });
    }
    if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy")) && init.method === "GET") {
      return Response.json({
        account_id: ACCOUNT,
        placement_policy: { allowed_clouds: ["civo"] },
      });
    }
    if (url.endsWith(":export-evacuation")) {
      return new Response(new Uint8Array([1, 2, 3]));
    }
    if (url.endsWith(":import-evacuation")) {
      assert.equal(
        directory.value(`cell:${TARGET}`).accepting,
        false,
        "an accepted in-flight target may drain before import",
      );
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id:
          new Headers(init.headers).get("X-Witself-Evacuation-ID"),
        status: "suspended",
        already_imported: false,
      });
    }
    if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy")) && init.method === "PATCH") {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: OPERATION_ID,
        placement_policy: JSON.parse(init.body),
      });
    }
    if (url.endsWith(":complete-evacuation")) {
      const body = JSON.parse(init.body);
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: body.evacuation_id,
        status: "active",
        completed: true,
      });
    }
    if (url.endsWith(":finalize-evacuation")) {
      const body = JSON.parse(init.body);
      assert.equal(
        directory.value(`acct:${ACCOUNT}`).cell,
        TARGET,
        "source finalization follows durable target route projection",
      );
      assert.equal(
        directory.value(`archived:${ACCOUNT}`).object,
        `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`,
        "archive remains recoverable until source finalization",
      );
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: body.evacuation_id,
        source_status: "suspended",
        finalized: true,
        already_finalized: false,
        finalized_at: "2026-07-25T12:00:00.000Z",
      });
    }
    throw new Error(`unexpected fetch ${init.method} ${url}`);
  };
  const storage = new Storage();
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch,
  });

  const response = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      reason: "rebalance",
    }),
  );
  assert.equal(response.status, 200);
  const result = await response.json();
  assert.equal(result.ok, true);
  assert.equal(result.result.operation_id, OPERATION_ID);
  assert.equal(result.result.evacuation_id, OPERATION_ID);

  const persisted = storage.values.get("account-lifecycle");
  assert.equal(persisted.operation, null);
  assert.equal(persisted.location.kind, "live");
  assert.equal(persisted.location.cell, TARGET);
  assert.equal(persisted.last_completed.evacuation_id, OPERATION_ID);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
  assert.deepEqual(
    bucket.deleted,
    [`archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`],
  );
  assert.deepEqual(
    targetAuthorities.get(directory).departed,
    [{
      cell_name: SOURCE,
      account_id: ACCOUNT,
      operation_id: OPERATION_ID,
      source_epoch: 0,
    }],
  );

  const lifecycleCalls = calls.filter(
    ({ url }) => !url.endsWith("/v1/version"),
  );
  const suspend = lifecycleCalls.find(({ url }) =>
    url.endsWith(":begin-evacuation")
  );
  assert.equal(
    JSON.parse(suspend.init.body).evacuation_id,
    OPERATION_ID,
  );
  const exportCall = lifecycleCalls.find(({ url }) =>
    url.endsWith(":export-evacuation")
  );
  assert.equal(
    new Headers(exportCall.init.headers).get(
      "X-Witself-Evacuation-ID",
    ),
    OPERATION_ID,
  );
  const importCall = lifecycleCalls.find(({ url }) =>
    url.endsWith(":import-evacuation")
  );
  assert.equal(
    new Headers(importCall.init.headers).get(
      "X-Witself-Evacuation-ID",
    ),
    OPERATION_ID,
  );
  const patch = lifecycleCalls.find(
    ({ url, init }) =>
      (url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy")) && init.method === "PATCH",
  );
  assert.equal(
    new Headers(patch.init.headers).get(
      "X-Witself-Evacuation-ID",
    ),
    OPERATION_ID,
  );
  assert.equal(
    patch.url,
    `https://target.example/v1/accounts/${ACCOUNT}:restore-placement-policy`,
  );
  assert.equal(
    lifecycleCalls.some(({ url }) =>
      /:(?:export|import|resume)$/.test(url)
    ),
    false,
    "the Worker never falls back to the removed generic mutation routes",
  );
  assert.equal(
    lifecycleCalls.some(({ url, init }) =>
      url.endsWith("/placement-policy") && init.method === "PATCH"
    ),
    false,
    "placement restoration uses only the evacuation-fenced route",
  );

  await directory.delete(`acct:${ACCOUNT}`);
  await directory.put(
    `archived:${ACCOUNT}`,
    JSON.stringify({
      archive_id: OPERATION_ID,
      object: `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`,
      cell: SOURCE,
    }),
  );
  const replay = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object:
        `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`,
      archive_id: OPERATION_ID,
    }),
  );
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).result.already_routed, true);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
});

test("deterministic legacy archive failure quarantines before target work and a replacement may proceed", async () => {
  const corruptObject =
    `archives/${ACCOUNT}/corrupt-founder.tar.gz`;
  const corruptPointer = {
    archive_id: "archive_corrupt_founder",
    object: corruptObject,
    cell: SOURCE,
    region: "us-west",
    status: "suspended",
  };
  const replacementObject =
    `archives/${ACCOUNT}/replacement.tar.gz`;
  const replacementPointer = {
    archive_id: "archive_replacement",
    object: replacementObject,
    cell: SOURCE,
    region: "us-west",
    status: "suspended",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: corruptPointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(corruptObject, new Uint8Array([1]));
  bucket.values.set(replacementObject, new Uint8Array([2]));
  const validations = new Map();
  const validation = async (_bucket, object) => {
    validations.set(object, (validations.get(object) ?? 0) + 1);
    if (object === corruptObject) {
      throw new ArchiveIntegrityError(
        "truncated tar stream at exactly 1 MiB",
      );
    }
    return {
      manifest: {
        account_id: ACCOUNT,
        schema_version: 69,
        status: "suspended",
      },
    };
  };
  const targetAuthority = new TargetAuthority(directory);
  const fetch = async (url, init = {}) => {
    if (url.endsWith("/v1/version")) return protocolResponse();
    if (url.endsWith(":import-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id:
          new Headers(init.headers).get("X-Witself-Evacuation-ID"),
        status: "suspended",
        already_imported: false,
      });
    }
    if (url.endsWith(":complete-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: JSON.parse(init.body).evacuation_id,
        status: "active",
        completed: true,
      });
    }
    throw new Error(`unexpected fetch ${init.method} ${url}`);
  };
  const storage = new Storage();
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch,
    validation,
    targetAuthority,
  });

  const first = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: corruptObject,
      archive_id: corruptPointer.archive_id,
    }),
  );
  assert.equal(first.status, 422);
  assert.match((await first.json()).error, /is quarantined/);
  assert.deepEqual(
    directory.value(`archived:${ACCOUNT}`),
    corruptPointer,
    "quarantine must not rewrite the archived projection",
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(bucket.values.has(corruptObject), true);
  assert.equal(targetAuthority.reservations.size, 0);
  assert.equal(storage.values.has("account-lifecycle"), false);
  assert.equal(
    storage.values.get("restore-quarantine").archive_id,
    corruptPointer.archive_id,
  );
  assert.equal(validations.get(corruptObject), 1);

  const retry = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: corruptObject,
      archive_id: corruptPointer.archive_id,
    }),
  );
  assert.equal(retry.status, 422);
  assert.equal(
    validations.get(corruptObject),
    1,
    "the exact quarantined archive must fail before another R2 read",
  );
  assert.equal(targetAuthority.reservations.size, 0);

  await directory.put(
    `archived:${ACCOUNT}`,
    JSON.stringify(replacementPointer),
  );
  const replaced = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: replacementObject,
      archive_id: replacementPointer.archive_id,
    }),
  );
  assert.equal(replaced.status, 200);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
  assert.equal(storage.values.has("restore-quarantine"), false);
  assert.equal(
    validations.get(replacementObject),
    2,
    "the changed object validates once for authority and once before import",
  );
  assert.equal(
    bucket.values.has(corruptObject),
    true,
    "replacing the pointer never deletes quarantined operator evidence",
  );
});

test("target-reserved integrity failure releases exact authority and becomes terminal", async () => {
  const object = `archives/${ACCOUNT}/reserved-corrupt.tar.gz`;
  const pointer = {
    archive_id: "archive_reserved_corrupt",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    status: "suspended",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let state = bootstrapArchivedState({
    account_id: ACCOUNT,
    archived: pointer,
  });
  state = claimOperation(state, {
    operation_id: OPERATION_ID,
    evacuation_id: SOURCE_EVACUATION_ID,
    kind: "restore",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: {
      archive_id: pointer.archive_id,
      object,
    },
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      request_epoch: 0,
      target_registration_id: `reg-${TARGET}`,
    },
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  const storage = new Storage();
  await storage.put("account-lifecycle", state);
  await storage.setAlarm(Date.now() + 60_000);
  const targetAuthority = new TargetAuthority(directory);
  const reservation = {
    account_id: ACCOUNT,
    operation_id: OPERATION_ID,
    evacuation_id: SOURCE_EVACUATION_ID,
    kind: "restore",
    target_cell: TARGET,
    target_registration_id: `reg-${TARGET}`,
    epoch: state.epoch,
  };
  targetAuthority.reservations.set(
    targetAuthority.key(TARGET, ACCOUNT),
    reservation,
  );
  const authorityCalls = [];
  let validations = 0;
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      throw new Error(`target mutation was attempted: ${init.method} ${url}`);
    },
    validation: async () => {
      validations += 1;
      throw new ArchiveIntegrityError("checksum mismatch");
    },
    targetAuthority,
    targetCoordinatorRequest: async (cellName, path, payload) => {
      authorityCalls.push(path);
      return targetAuthority.request(cellName, path, payload);
    },
  });

  const first = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(first.status, 422);
  assert.deepEqual(authorityCalls, ["/reserve", "/release"]);
  assert.equal(validations, 1);
  assert.equal(targetAuthority.reservations.size, 0);
  assert.equal(storage.alarm, null);
  assert.equal(storage.values.get("account-lifecycle").operation, null);
  assert.equal(
    storage.values.get("account-lifecycle").last_completed.outcome,
    "quarantined",
  );
  assert.deepEqual(directory.value(`archived:${ACCOUNT}`), pointer);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(bucket.values.has(object), true);

  const retry = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(retry.status, 422);
  assert.equal(validations, 1);
  assert.deepEqual(
    authorityCalls,
    ["/reserve", "/release"],
    "terminal retry must not reserve the target again",
  );
  assert.equal(storage.alarm, null);
});

test("ambiguous archive reads remain resumable and are never quarantined", async () => {
  const object = `archives/${ACCOUNT}/temporary-r2-failure.tar.gz`;
  const pointer = {
    archive_id: "archive_temporary_failure",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    status: "suspended",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new ErroringBodyBucket(
    new Error("temporary R2 body stream timeout"),
  );
  bucket.values.set(object, new Uint8Array([1]));
  let state = bootstrapArchivedState({
    account_id: ACCOUNT,
    archived: pointer,
  });
  state = claimOperation(state, {
    operation_id: OPERATION_ID,
    evacuation_id: SOURCE_EVACUATION_ID,
    kind: "restore",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: {
      archive_id: pointer.archive_id,
      object,
    },
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      request_epoch: 0,
      target_registration_id: `reg-${TARGET}`,
    },
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  const storage = new Storage();
  await storage.put("account-lifecycle", state);
  await storage.setAlarm(Date.now() + 60_000);
  const targetAuthority = new TargetAuthority(directory);
  targetAuthority.reservations.set(
    targetAuthority.key(TARGET, ACCOUNT),
    {
      account_id: ACCOUNT,
      operation_id: OPERATION_ID,
      evacuation_id: SOURCE_EVACUATION_ID,
      kind: "restore",
      target_cell: TARGET,
      target_registration_id: `reg-${TARGET}`,
      epoch: state.epoch,
    },
  );
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      throw new Error(`target mutation was attempted: ${init.method} ${url}`);
    },
    validation: validateCommittedAccountArchive,
    targetAuthority,
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(response.status, 500);
  assert.equal(storage.values.has("restore-quarantine"), false);
  assert.equal(
    storage.values.get("account-lifecycle").operation.phase,
    "target_reserved",
  );
  assert.equal(targetAuthority.reservations.size, 1);
  assert.notEqual(storage.alarm, null);
});

test("mixed restore batch completes healthy accounts and leaves corrupt founder isolated", async () => {
  const founder = "acct_founder";
  const healthyA = "acct_healthy_a";
  const healthyB = "acct_healthy_b";
  const accountIDs = [founder, healthyA, healthyB];
  const pointers = new Map(
    accountIDs.map((accountID) => [
      accountID,
      {
        archive_id: `archive_${accountID}`,
        object: `archives/${accountID}/legacy.tar.gz`,
        cell: SOURCE,
        region: "us-west",
        status: "suspended",
      },
    ]),
  );
  const entries = {
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  };
  for (const [accountID, pointer] of pointers) {
    entries[`archived:${accountID}`] = pointer;
  }
  const directory = new KV(entries);
  const bucket = new Bucket();
  for (const pointer of pointers.values()) {
    bucket.values.set(pointer.object, new Uint8Array([1]));
  }
  const targetAuthority = new TargetAuthority(directory);
  const storages = new Map();
  const coordinators = new Map();
  const validationCalls = new Map();
  let operationSequence = 0;
  for (const accountID of accountIDs) {
    const storage = new Storage();
    storages.set(accountID, storage);
    const coordinator = new DurableAccountLifecycle(
      {
        id: { name: accountID },
        storage,
      },
      {
        DIRECTORY: directory,
        ARCHIVES: bucket,
      },
      {
        now: () => new Date("2026-07-25T12:00:00.000Z"),
        randomUUID: () => {
          operationSequence += 1;
          return `00000000-0000-4000-8000-${String(operationSequence).padStart(12, "0")}`;
        },
        validateArchive: async (_archiveBucket, object) => {
          validationCalls.set(
            object,
            (validationCalls.get(object) ?? 0) + 1,
          );
          if (object === pointers.get(founder).object) {
            throw new ArchiveIntegrityError(
              "truncated tar stream at exactly 1 MiB",
            );
          }
          return {
            manifest: {
              account_id: accountID,
              schema_version: 69,
              status: "suspended",
            },
          };
        },
        fetch: async (url, init = {}) => {
          if (url.endsWith("/v1/version")) {
            return protocolResponse();
          }
          const match =
            /\/v1\/accounts\/([^:]+):/.exec(new URL(url).pathname);
          const requestedAccount = match?.[1];
          if (url.endsWith(":import-evacuation")) {
            return Response.json({
              account_id: requestedAccount,
              evacuation_id:
                new Headers(init.headers).get(
                  "X-Witself-Evacuation-ID",
                ),
              evacuation_role: "target",
              status: "suspended",
              already_imported: false,
            });
          }
          if (url.endsWith(":complete-evacuation")) {
            return Response.json({
              account_id: requestedAccount,
              evacuation_id: JSON.parse(init.body).evacuation_id,
              evacuation_role: "target",
              status: "active",
              completed: true,
            });
          }
          throw new Error(`unexpected fetch ${init.method} ${url}`);
        },
        targetCoordinatorRequest:
          (cellName, path, payload) =>
            targetAuthority.request(cellName, path, payload),
      },
    );
    coordinators.set(accountID, coordinator);
  }

  const results = [];
  for (const accountID of accountIDs) {
    const pointer = pointers.get(accountID);
    const response = await coordinators.get(accountID).fetch(
      accountRequest(accountID, "restore", {
        cell_name: TARGET,
        archive_object: pointer.object,
        archive_id: pointer.archive_id,
      }),
    );
    results.push({
      accountID,
      status: response.status,
      body: await response.json(),
    });
  }

  assert.deepEqual(
    results.map(({ accountID, status }) => ({ accountID, status })),
    [
      { accountID: founder, status: 422 },
      { accountID: healthyA, status: 200 },
      { accountID: healthyB, status: 200 },
    ],
  );
  assert.deepEqual(
    directory.value(`archived:${founder}`),
    pointers.get(founder),
  );
  assert.equal(directory.value(`acct:${founder}`), null);
  assert.equal(bucket.values.has(pointers.get(founder).object), true);
  assert.equal(
    storages.get(founder).values.get("restore-quarantine").archive_id,
    pointers.get(founder).archive_id,
  );
  for (const accountID of [healthyA, healthyB]) {
    assert.equal(directory.value(`archived:${accountID}`), null);
    assert.equal(directory.value(`acct:${accountID}`).cell, TARGET);
    assert.equal(bucket.values.has(pointers.get(accountID).object), false);
  }
  assert.equal(targetAuthority.reservations.size, 0);
  assert.equal(targetAuthority.residents.size, 2);

  const founderCalls =
    validationCalls.get(pointers.get(founder).object);
  const founderRetry = await coordinators.get(founder).fetch(
    accountRequest(founder, "restore", {
      cell_name: TARGET,
      archive_object: pointers.get(founder).object,
      archive_id: pointers.get(founder).archive_id,
    }),
  );
  assert.equal(founderRetry.status, 422);
  assert.equal(
    validationCalls.get(pointers.get(founder).object),
    founderCalls,
  );
  assert.equal(targetAuthority.reservations.size, 0);
});

test("mixed-version cells fail exact restore routes retryably", async (t) => {
  for (
    const {
      stage,
      status,
      expectedSuffix,
      placementPolicy,
    } of [
      {
        stage: "import",
        status: 404,
        expectedSuffix: ":import-evacuation",
        placementPolicy: null,
      },
      {
        stage: "placement",
        status: 405,
        expectedSuffix: ":restore-placement-policy",
        placementPolicy: { allowed_clouds: ["civo"] },
      },
      {
        stage: "complete",
        status: 404,
        expectedSuffix: ":complete-evacuation",
        placementPolicy: null,
      },
    ]
  ) {
    await t.test(stage, async () => {
      const object =
        `archives/${ACCOUNT}/mixed-version-${stage}.tar.gz`;
      const pointer = {
        archive_id: `archive_mixed_version_${stage}`,
        evacuation_id: SOURCE_EVACUATION_ID,
        object,
        cell: SOURCE,
        source_cell: SOURCE,
        source_registration_id: `reg-${SOURCE}`,
        source_route_epoch: 0,
        epoch: 3,
        status: "suspended",
        placement_policy: placementPolicy,
      };
      const directory = new KV({
        [`archived:${ACCOUNT}`]: pointer,
        [`cell:${TARGET}`]: cell(
          TARGET,
          "https://target.example",
        ),
      });
      const bucket = new Bucket();
      bucket.values.set(object, new Uint8Array([1]));
      const calls = [];
      const coordinator = runtime({
        directory,
        bucket,
        fetch: async (url, init = {}) => {
          calls.push({ url, init });
          if (url.endsWith("/v1/version")) return protocolResponse();
          if (url.endsWith(":import-evacuation")) {
            if (stage === "import") {
              return Response.json(
                { error: "route not installed on this cell version" },
                { status },
              );
            }
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: SOURCE_EVACUATION_ID,
              status: "suspended",
            });
          }
          if (url.endsWith(":restore-placement-policy")) {
            assert.equal(stage, "placement");
            return Response.json(
              { error: "route not installed on this cell version" },
              { status },
            );
          }
          if (url.endsWith(":complete-evacuation")) {
            assert.equal(stage, "complete");
            return Response.json(
              { error: "route not installed on this cell version" },
              { status },
            );
          }
          throw new Error(`unexpected fetch ${init.method} ${url}`);
        },
      });

      const response = await coordinator.fetch(
        request("restore", {
          cell_name: TARGET,
          archive_object: object,
          archive_id: pointer.archive_id,
          expected_epoch: 3,
        }),
      );
      assert.equal(response.status, 502);
      assert.ok(
        calls.some(({ url }) => url.endsWith(expectedSuffix)),
      );
      assert.equal(
        calls.some(({ url }) => /:(?:import|resume)$/.test(url)),
        false,
      );
      assert.equal(directory.value(`acct:${ACCOUNT}`), null);
      assert.equal(
        directory.value(`archived:${ACCOUNT}`).object,
        object,
      );
      assert.equal(bucket.values.has(object), true);
    });
  }
});

test("mixed-version export route failure aborts before archive authority", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(0),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
    ),
    [`cell:${TARGET}`]: cell(
      TARGET,
      "https://target.example",
    ),
  });
  const bucket = new Bucket();
  const calls = [];
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url, init = {}) => {
      calls.push({ url, init });
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          status: "suspended",
        });
      }
      if (
        url.endsWith("/placement-policy") &&
        init.method === "GET"
      ) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["civo"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return Response.json(
          { error: "route not installed on this cell version" },
          { status: 405 },
        );
      }
      if (url.endsWith(":abort-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 0,
    }),
  );
  assert.equal(response.status, 502);
  assert.ok(
    calls.some(({ url }) => url.endsWith(":export-evacuation")),
  );
  assert.equal(
    calls.some(({ url }) => url.endsWith(":export")),
    false,
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
  assert.equal(bucket.values.size, 0);
  assert.equal(
    coordinator.storage.values.get("account-lifecycle").operation,
    null,
  );
});

test("generic import 409 never advances or deletes the archive", async () => {
  const object = "archives/acct_runtime/source.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_source",
      evacuation_id: SOURCE_EVACUATION_ID,
      object,
      cell: SOURCE,
      source_registration_id: `reg-${SOURCE}`,
      region: "us-west",
    },
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let resumeCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json(
          { error: "account already exists" },
          { status: 409 },
        );
      }
      if (url.endsWith(":complete-evacuation")) resumeCalls++;
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_source",
    }),
  );
  assert.equal(response.status, 409);
  assert.match((await response.json()).error, /^import 409:/);
  assert.equal(resumeCalls, 0);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(bucket.deleted.length, 0);
  const persisted = coordinator.storage.values.get("account-lifecycle");
  assert.equal(persisted.operation.phase, "target_reserved");
  assert.equal(
    persisted.operation.evacuation_id,
    SOURCE_EVACUATION_ID,
  );
});

test("closed archive remains retained without ever publishing a live route", async () => {
  const object = "archives/acct_runtime/closed.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_closed",
      evacuation_id: SOURCE_EVACUATION_ID,
      object,
      cell: SOURCE,
      source_registration_id: `reg-${SOURCE}`,
      region: "us-west",
    },
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "closed",
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "closed",
          completed: true,
        });
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_closed",
    }),
  );
  assert.equal(response.status, 200);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(directory.value(`archived:${ACCOUNT}`).status, "closed");
  assert.deepEqual(bucket.deleted, []);
  assert.equal(bucket.values.has(object), true);
  const persisted = coordinator.storage.values.get("account-lifecycle");
  assert.equal(persisted.location.kind, "closed_archived");
  assert.equal(
    persisted.last_completed.final_location.kind,
    "closed_archived",
  );

  // A stale/lost discovery projection is repaired from Durable Object
  // authority even after the source cell has left the registry.
  await directory.delete(`archived:${ACCOUNT}`);
  const replay = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_closed",
    }),
  );
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).result.archive_retained, true);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(bucket.values.has(object), true);
});

test("failed pre-archive export aborts only after exact cell receipt", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const bucket = new Bucket();
  let abortBody = null;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["aws"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return Response.json({ error: "export failed" }, { status: 500 });
      }
      if (url.endsWith(":abort-evacuation")) {
        abortBody = JSON.parse(init.body);
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: abortBody.evacuation_id,
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 500);
  assert.equal(abortBody.evacuation_id, OPERATION_ID);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
  const persisted = coordinator.storage.values.get("account-lifecycle");
  assert.equal(persisted.operation, null);
  assert.equal(persisted.location.kind, "live");
  assert.equal(persisted.last_completed.outcome, "aborted");
});

test("committed storage write with lost acknowledgement never rolls back archive authority", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const bucket = new Bucket();
  const storage = new CommitThenThrowStorage();
  let abortCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["aws"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return new Response(new Uint8Array([1]));
      }
      if (url.endsWith(":abort-evacuation")) {
        abortCalls++;
        throw new Error("abort must not be called past commit boundary");
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 500);
  assert.match(
    (await response.json()).error,
    /lost Durable Object storage acknowledgement/,
  );
  assert.equal(abortCalls, 0);
  const object =
    `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  assert.equal(bucket.values.has(object), true);
  assert.deepEqual(bucket.deleted, []);
  const persisted = storage.values.get("account-lifecycle");
  assert.equal(persisted.operation.phase, "archive_committed");
  assert.equal(persisted.location.kind, "archived");
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
});

test("cleanup failure is resumed by a restarted Durable Object alarm", async () => {
  const object = "archives/acct_runtime/retry.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_retry",
      evacuation_id: SOURCE_EVACUATION_ID,
      object,
      cell: SOURCE,
      source_registration_id: `reg-${SOURCE}`,
      region: "us-west",
    },
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  bucket.failDeleteOnce = true;
  const storage = new Storage();
  const fetch = async (url) => {
    if (url.endsWith("/v1/version")) return protocolResponse();
    if (url.endsWith(":import-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: SOURCE_EVACUATION_ID,
        status: "suspended",
      });
    }
    if (url.endsWith(":complete-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: SOURCE_EVACUATION_ID,
        status: "active",
        completed: true,
      });
    }
    throw new Error(`unexpected fetch ${url}`);
  };
  const first = runtime({
    directory,
    bucket,
    storage,
    fetch,
  });

  const response = await first.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_retry",
    }),
  );
  const body = await response.json();
  assert.equal(response.status, 200);
  assert.equal(body.result.cleanup_pending, true);
  assert.ok(storage.alarm);
  assert.equal(
    storage.values.get("account-lifecycle").operation.phase,
    "archive_retired",
  );

  const restarted = runtime({
    directory,
    bucket,
    storage,
    fetch,
  });
  await restarted.alarm();
  const persisted = storage.values.get("account-lifecycle");
  assert.equal(persisted.operation, null);
  assert.equal(persisted.location.kind, "live");
  assert.equal(storage.alarm, null);
  assert.deepEqual(bucket.deleted, [object]);
});

test("completed import receipt skips placement patch and resume", async () => {
  const object = "archives/acct_runtime/completed.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_completed",
      evacuation_id: SOURCE_EVACUATION_ID,
      object,
      cell: SOURCE,
      source_registration_id: `reg-${SOURCE}`,
      region: "us-west",
      placement_policy: { allowed_clouds: ["civo"] },
    },
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let mutationCallsAfterImport = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "active",
          already_imported: true,
          evacuation_completed: true,
        });
      }
      mutationCallsAfterImport++;
      throw new Error(`unexpected post-import mutation ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_completed",
    }),
  );
  assert.equal(response.status, 200);
  assert.equal(mutationCallsAfterImport, 0);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
});

test("lost resume acknowledgement is recovered by exact completion receipt", async () => {
  const object = "archives/acct_runtime/lost-resume.tar.gz";
  const pointer = {
    archive_id: "archive_lost_resume",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    region: "us-west",
    placement_policy: { allowed_clouds: ["civo"] },
  };
  let state = bootstrapArchivedState({
    account_id: ACCOUNT,
    archived: pointer,
  });
  state = claimOperation(state, {
    operation_id: OPERATION_ID,
    evacuation_id: SOURCE_EVACUATION_ID,
    kind: "restore",
    target_cell: TARGET,
    archive: {
      archive_id: pointer.archive_id,
      object,
    },
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      placement_policy: pointer.placement_policy,
      imported_status: "suspended",
      target_registration_id: `reg-${TARGET}`,
      source_registration_id: `reg-${SOURCE}`,
      request_epoch: 0,
    },
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: OPERATION_ID,
    from_phase: "target_reserved",
    to_phase: "target_imported",
  });

  const storage = new Storage();
  storage.values.set("account-lifecycle", state);
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let resumeCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          placement_policy: pointer.placement_policy,
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        resumeCalls++;
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "active",
          completed: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(response.status, 200);
  assert.equal(resumeCalls, 1);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(
    storage.values.get("account-lifecycle").last_completed.kind,
    "restore",
  );
});

test("route-retired move resumes from a scheduler restore request", async () => {
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  const pointer = {
    archive_id: OPERATION_ID,
    evacuation_id: OPERATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    region: "us-west",
    epoch: 1,
    exported_at: "2026-07-25T11:59:59.000Z",
    status: "suspended",
  };
  const storage = new Storage();
  storage.values.set(
    "account-lifecycle",
    archivedOperationState("move", pointer, TARGET),
  );
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    validation: {
      manifest: {
        account_id: ACCOUNT,
        schema_version: 70,
        evacuation_id: OPERATION_ID,
      },
    },
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "active",
          completed: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: OPERATION_ID,
      expected_epoch: 1,
    }),
  );
  assert.equal(response.status, 200);
  const body = await response.json();
  assert.equal(body.result.operation_id, OPERATION_ID);
  assert.equal(body.result.target_cell, TARGET);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(
    storage.values.get("account-lifecycle").last_completed.kind,
    "move",
  );
});

test("scheduler resumes a runtime move using the archived projection epoch", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(7),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  const storage = new PhaseCommitThenThrowStorage("route_retired");
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy")) && init.method === "GET") {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["civo"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return new Response(new Uint8Array([1]));
      }
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy")) && init.method === "PATCH") {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          placement_policy: JSON.parse(init.body),
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "active",
          completed: true,
        });
      }
      if (url.endsWith(":finalize-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          source_status: "suspended",
          finalized_at: "2026-07-25T12:00:00.000Z",
          finalized: true,
          already_finalized: false,
        });
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const interrupted = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 7,
    }),
  );
  assert.equal(interrupted.status, 500);
  const pointer = directory.value(`archived:${ACCOUNT}`);
  assert.equal(pointer.epoch, 8);
  assert.equal(
    storage.values.get("account-lifecycle").operation.request_epoch,
    7,
  );

  const resumed = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: pointer.object,
      archive_id: pointer.archive_id,
      expected_epoch: 8,
    }),
  );
  assert.equal(resumed.status, 200);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(
    storage.values.get("account-lifecycle").last_completed.kind,
    "move",
  );
});

test("route-retired evacuation completes before scheduler restore", async () => {
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  const pointer = {
    archive_id: OPERATION_ID,
    evacuation_id: OPERATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    region: "us-west",
    epoch: 1,
    exported_at: "2026-07-25T11:59:59.000Z",
    status: "suspended",
  };
  const storage = new Storage();
  storage.values.set(
    "account-lifecycle",
    archivedOperationState("evacuate", pointer),
  );
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const restoreOperationID =
    "33333333-3333-4333-8333-333333333333";
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    randomUUID: () => restoreOperationID,
    validation: {
      manifest: {
        account_id: ACCOUNT,
        schema_version: 70,
        evacuation_id: OPERATION_ID,
      },
    },
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "active",
          completed: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: OPERATION_ID,
      expected_epoch: 1,
    }),
  );
  assert.equal(response.status, 200);
  const persisted = storage.values.get("account-lifecycle");
  assert.equal(persisted.location.cell, TARGET);
  assert.equal(persisted.last_completed.kind, "restore");
  assert.equal(persisted.last_completed.operation_id, restoreOperationID);
  assert.equal(persisted.last_completed.evacuation_id, OPERATION_ID);
});

test("evacuation pointer uses verified manifest status and timestamp", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const bucket = new Bucket();
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["aws"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return new Response(new Uint8Array([1]));
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 200);
  const pointer = directory.value(`archived:${ACCOUNT}`);
  assert.equal(pointer.status, "suspended");
  assert.equal(pointer.exported_at, "2026-07-25T11:59:59.000Z");
  assert.equal(pointer.evacuation_id, OPERATION_ID);

  await directory.delete(`archived:${ACCOUNT}`);
  const replay = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).result.already_archived, true);
  assert.equal(
    directory.value(`archived:${ACCOUNT}`).object,
    pointer.object,
  );
});

test("archived close fails 409 without touching the tombstone", async () => {
  const object = "archives/acct_runtime/archived-close.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_close",
      evacuation_id: SOURCE_EVACUATION_ID,
      object,
      cell: SOURCE,
    },
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async () => {
      throw new Error("close must not reach a cell while archived");
    },
  });
  const response = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
    }),
  );
  assert.equal(response.status, 409);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(bucket.deleted.length, 0);
});

test("modern archive without evacuation id fails before target mutation", async () => {
  const object = "archives/acct_runtime/missing-id.tar.gz";
  const directory = new KV({
    [`archived:${ACCOUNT}`]: {
      archive_id: "archive_missing_id",
      object,
      cell: SOURCE,
    },
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let fetchCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    validation: {
      manifest: {
        account_id: ACCOUNT,
        schema_version: 70,
      },
    },
    fetch: async () => {
      fetchCalls++;
      throw new Error("target must not be mutated");
    },
  });
  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: "archive_missing_id",
    }),
  );
  assert.equal(response.status, 409);
  assert.match(
    (await response.json()).error,
    /missing its evacuation id/,
  );
  assert.equal(fetchCalls, 0);
});

test("placement snapshot refusal aborts before export publication", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const bucket = new Bucket();
  let exportCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({ account_id: ACCOUNT });
      }
      if (url.endsWith(":export-evacuation")) {
        exportCalls++;
        return new Response(new Uint8Array([1]));
      }
      if (url.endsWith(":abort-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });
  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 502);
  assert.equal(exportCalls, 0);
  const state = coordinator.storage.values.get("account-lifecycle");
  assert.equal(state.last_completed.outcome, "aborted");
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
});

test("closed authority removes a stale route on replay", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
  });
  const bucket = new Bucket();
  let closeCalls = 0;
  const aliasActions = [];
  const coordinator = runtime({
    directory,
    bucket,
    reconcileRealmEmailAliases: async (_accountID, fence) => {
      aliasActions.push(fence.action);
      return { complete: true };
    },
    fetch: async (url) => {
      if (url.endsWith("/v1/account:close")) {
        closeCalls++;
        return Response.json({
          account_id: ACCOUNT,
          status: "closed",
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });
  const first = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
    }),
  );
  assert.equal(first.status, 200);
  await directory.put(
    `acct:${ACCOUNT}`,
    JSON.stringify(sourceRoute()),
  );
  const replay = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
    }),
  );
  assert.equal(replay.status, 200);
  assert.equal(closeCalls, 1);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.deepEqual(aliasActions, ["retire", "retire"]);
});

test("unsupported cell protocol prevents account mutation", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const bucket = new Bucket();
  let mutationCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse(false);
      mutationCalls++;
      throw new Error("mutation should not be attempted");
    },
  });

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 409);
  assert.equal(mutationCalls, 0);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
});

test("definitive close refusal is persisted as cancelled, not wedged", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
  });
  const bucket = new Bucket();
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/account:close")) {
        return Response.json({ error: "unauthorized" }, { status: 401 });
      }
      if (url.endsWith(":contact")) {
        return Response.json({
          account_id: ACCOUNT,
          status: "active",
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
    }),
  );
  assert.equal(response.status, 401);
  const persisted = coordinator.storage.values.get("account-lifecycle");
  assert.equal(persisted.operation, null);
  assert.equal(persisted.last_completed.outcome, "cancelled");
  assert.equal(persisted.location.kind, "live");
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
});

test("move target preflight fails before a claim or source suspension", async (t) => {
  const cases = [
    {
      name: "drained",
      target: cell(TARGET, "https://target.example", false),
      protocol: true,
      status: 409,
    },
    {
      name: "backup validation target",
      target: {
        ...cell(TARGET, "https://target.example"),
        backup_validation_target: true,
      },
      protocol: true,
      status: 409,
    },
    {
      name: "old protocol",
      target: cell(TARGET, "https://target.example"),
      protocol: false,
      status: 409,
    },
    {
      name: "unavailable",
      target: null,
      protocol: true,
      status: 502,
    },
  ];

  for (const fixture of cases) {
    await t.test(fixture.name, async () => {
      const entries = {
        [`acct:${ACCOUNT}`]: sourceRoute(7),
        [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
      };
      if (fixture.target) {
        entries[`cell:${TARGET}`] = fixture.target;
      }
      const directory = new KV(entries);
      const bucket = new Bucket();
      const storage = new Storage();
      let suspendCalls = 0;
      const coordinator = runtime({
        directory,
        bucket,
        storage,
        fetch: async (url) => {
          if (url === "https://target.example/v1/version") {
            return protocolResponse(fixture.protocol);
          }
          if (url.endsWith(":begin-evacuation")) {
            suspendCalls++;
          }
          throw new Error(`unexpected fetch ${url}`);
        },
      });

      const response = await coordinator.fetch(
        request("move", {
          source_cell: SOURCE,
          target_cell: TARGET,
          expected_epoch: 7,
        }),
      );
      assert.equal(response.status, fixture.status);
      assert.equal(suspendCalls, 0);
      assert.equal(
        storage.values.get("account-lifecycle").operation,
        null,
      );
      assert.equal(storage.alarm, null);
      assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
      assert.equal(directory.value(`archived:${ACCOUNT}`), null);
    });
  }
});

test("source cleanup requires an exact finalization receipt", async () => {
  const object = "archives/acct_runtime/finalize-mismatch.tar.gz";
  const pointer = {
    archive_id: "archive_finalize_mismatch",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`,
    region: "us-west",
    epoch: 4,
    status: "suspended",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const storage = new Storage();
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "suspended",
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          status: "active",
          completed: true,
        });
      }
      if (url.endsWith(":finalize-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: "wrong_evacuation",
          source_status: "suspended",
          finalized: true,
          already_finalized: false,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
      expected_epoch: 4,
    }),
  );
  assert.equal(response.status, 502);
  assert.match(
    (await response.json()).error,
    /source finalization returned 2xx without an exact/,
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(bucket.values.has(object), true);
  assert.equal(bucket.deleted.length, 0);
  assert.equal(
    storage.values.get("account-lifecycle").operation.phase,
    "route_projected",
  );
  assert.notEqual(storage.alarm, null);
});

test("durable source registration finalizes through missing or stale KV", async (t) => {
  for (const projection of ["missing", "stale"]) {
    await t.test(projection, async () => {
      const object =
        `archives/${ACCOUNT}/durable-source-${projection}.tar.gz`;
      const pointer = {
        archive_id: `archive_durable_source_${projection}`,
        evacuation_id: SOURCE_EVACUATION_ID,
        object,
        cell: SOURCE,
        source_cell: SOURCE,
        source_registration_id: `reg-${SOURCE}`,
        source_route_epoch: 7,
        region: "us-west",
        epoch: 4,
        status: "suspended",
      };
      const durableSource = cell(
        SOURCE,
        "https://durable-source.example",
      );
      const entries = {
        [`archived:${ACCOUNT}`]: pointer,
        [`cell:${TARGET}`]: cell(
          TARGET,
          "https://target.example",
        ),
      };
      if (projection === "stale") {
        entries[`cell:${SOURCE}`] = {
          ...cell(SOURCE, "https://replacement-source.example"),
          registration_id: "replacement-registration",
        };
      }
      const directory = new KV(entries);
      const bucket = new Bucket();
      bucket.values.set(object, new Uint8Array([1]));
      const targetAuthority = new TargetAuthority(directory);
      const sourceFinalizations = [];
      const coordinator = runtime({
        directory,
        bucket,
        targetAuthority,
        targetCoordinatorRequest: async (cellName, path, payload) => {
          if (
            cellName === SOURCE &&
            path === "/registration-status"
          ) {
            return {
              ok: true,
              cell_name: SOURCE,
              expected_registration_id: payload.registration_id,
              registration_status: "active",
              current_registration_id: `reg-${SOURCE}`,
              tombstone_registration_id: null,
              active_cell: durableSource,
            };
          }
          return targetAuthority.request(cellName, path, payload);
        },
        fetch: async (url, init = {}) => {
          if (url.endsWith("/v1/version")) return protocolResponse();
          if (url.endsWith(":import-evacuation")) {
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: SOURCE_EVACUATION_ID,
              status: "suspended",
            });
          }
          if (url.endsWith(":complete-evacuation")) {
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: SOURCE_EVACUATION_ID,
              status: "active",
              completed: true,
            });
          }
          if (
            url ===
              `https://durable-source.example/v1/accounts/${ACCOUNT}:finalize-evacuation`
          ) {
            sourceFinalizations.push(url);
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: JSON.parse(init.body).evacuation_id,
              source_status: "suspended",
              finalized: true,
              already_finalized: false,
              finalized_at: "2026-07-25T12:00:00.000Z",
            });
          }
          throw new Error(`unexpected fetch ${init.method} ${url}`);
        },
      });

      const response = await coordinator.fetch(
        request("restore", {
          cell_name: TARGET,
          archive_object: object,
          archive_id: pointer.archive_id,
          expected_epoch: 4,
        }),
      );
      assert.equal(response.status, 200);
      assert.equal(sourceFinalizations.length, 1);
      assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
      assert.equal(directory.value(`archived:${ACCOUNT}`), null);
      assert.deepEqual(targetAuthority.departed, [{
        cell_name: SOURCE,
        account_id: ACCOUNT,
        operation_id: OPERATION_ID,
        source_epoch: 7,
      }]);
    });
  }
});

test("route retirement cannot release source occupancy before finalization", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(0),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
    ),
    [`cell:${TARGET}`]: cell(
      TARGET,
      "https://target.example",
    ),
  });
  const sourceStorage = new Storage();
  const targetStorage = new Storage();
  const sourceCoordinator = new DurableTargetCellCoordinator(
    { id: { name: SOURCE }, storage: sourceStorage },
    { DIRECTORY: directory },
    {
      now: () => new Date("2026-07-25T12:00:00.000Z"),
      randomUUID: () => "source-delete-operation",
    },
  );
  const targetCoordinator = new DurableTargetCellCoordinator(
    { id: { name: TARGET }, storage: targetStorage },
    { DIRECTORY: directory },
    {
      now: () => new Date("2026-07-25T12:00:00.000Z"),
      randomUUID: () => "target-delete-operation",
    },
  );
  const coordinatorRequest = async (
    instance,
    cellName,
    path,
    payload = {},
  ) => instance.fetch(
    new Request(`https://target-cell.internal${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        cell_name: cellName,
        ...payload,
      }),
    }),
  );
  assert.equal(
    (await coordinatorRequest(
      sourceCoordinator,
      SOURCE,
      "/register",
      { cell: cell(SOURCE, "https://source.example") },
    )).status,
    200,
  );
  assert.equal(
    (await coordinatorRequest(
      targetCoordinator,
      TARGET,
      "/register",
      { cell: cell(TARGET, "https://target.example") },
    )).status,
    200,
  );
  assert.ok(
    sourceStorage.values.has(`resident:${ACCOUNT}`),
    "registration backfills the source route into durable occupancy",
  );

  const coordinators = new Map([
    [SOURCE, sourceCoordinator],
    [TARGET, targetCoordinator],
  ]);
  const targetCoordinatorRequest =
    async (cellName, path, payload) => {
      const response = await coordinatorRequest(
        coordinators.get(cellName),
        cellName,
        path,
        payload,
      );
      const body = await response.json().catch(() => null);
      if (!response.ok || body?.ok !== true) {
        throw new AccountLifecycleRuntimeError(
          body?.error ?? `coordinator HTTP ${response.status}`,
          response.status,
        );
      }
      return body;
    };

  const bucket = new Bucket();
  const coordinator = runtime({
    directory,
    bucket,
    targetCoordinatorRequest,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          status: "suspended",
        });
      }
      if (
        url.endsWith("/placement-policy") &&
        init.method === "GET"
      ) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["civo"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        return new Response(new Uint8Array([1, 2, 3]));
      }
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id:
            new Headers(init.headers).get("X-Witself-Evacuation-ID"),
          status: "suspended",
        });
      }
      if (url.endsWith(":restore-placement-policy")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id:
            new Headers(init.headers).get("X-Witself-Evacuation-ID"),
          placement_policy: JSON.parse(init.body),
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          status: "active",
          completed: true,
        });
      }
      if (url.endsWith(":finalize-evacuation")) {
        throw new Error("simulated lost source finalization response");
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 0,
    }),
  );
  assert.equal(response.status, 502);
  assert.equal(
    coordinator.storage.values.get("account-lifecycle").operation.phase,
    "route_projected",
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
  assert.ok(
    sourceStorage.values.has(`resident:${ACCOUNT}`),
    "route projection alone cannot release source occupancy",
  );

  assert.equal(
    (await coordinatorRequest(
      sourceCoordinator,
      SOURCE,
      "/register",
      { cell: cell(SOURCE, "https://source.example", false) },
    )).status,
    200,
  );
  const deletion = await coordinatorRequest(
    sourceCoordinator,
    SOURCE,
    "/delete",
  );
  assert.equal(deletion.status, 409);
  assert.match(
    (await deletion.json()).error,
    /reservations or residents/,
  );
});

test("bounded legacy restore does not invent an unfenced source teardown", async () => {
  const object = `archives/${ACCOUNT}/legacy-pre-v1.tar.gz`;
  const pointer = {
    archive_id: "legacy_archive",
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    region: "us-west",
    epoch: 2,
    status: "suspended",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const storage = new PhaseCommitThenThrowStorage("source_finalized");
  const targetAuthority = new TargetAuthority(directory);
  let sourceAuthorityCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    targetAuthority,
    validation: {
      manifest: {
        account_id: ACCOUNT,
        schema_version: 69,
      },
    },
    targetCoordinatorRequest: async (cellName, path, payload) => {
      if (
        cellName === SOURCE &&
        path === "/registration-status"
      ) {
        sourceAuthorityCalls++;
        throw new Error(
          "legacy restore must not consult invented source authority",
        );
      }
      return targetAuthority.request(cellName, path, payload);
    },
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id:
            new Headers(init.headers).get("X-Witself-Evacuation-ID"),
          status: "suspended",
        });
      }
      if (url.endsWith(":complete-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          status: "active",
          completed: true,
        });
      }
      throw new Error(`unexpected fetch ${init.method} ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
      expected_epoch: 2,
    }),
  );
  assert.equal(response.status, 500);
  assert.equal(sourceAuthorityCalls, 0);
  const committed = storage.values.get("account-lifecycle");
  assert.equal(committed.operation.phase, "source_finalized");
  assert.equal(
    committed.operation.source_finalization.mode,
    "legacy_unfenced_source",
  );
});

test("durable alarm resumes crashes after archive retirement and cleanup", async (t) => {
  for (const phase of ["archive_retired", "archive_cleaned"]) {
    await t.test(phase, async () => {
      const object = `archives/${ACCOUNT}/${phase}.tar.gz`;
      const pointer = {
        archive_id: `archive_${phase}`,
        evacuation_id: SOURCE_EVACUATION_ID,
        object,
        cell: SOURCE,
        source_cell: SOURCE,
        source_registration_id: `reg-${SOURCE}`,
        region: "us-west",
        epoch: 3,
        status: "suspended",
      };
      const directory = new KV({
        [`archived:${ACCOUNT}`]: pointer,
        [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
      });
      const bucket = new Bucket();
      bucket.values.set(object, new Uint8Array([1]));
      const storage = new PhaseCommitThenThrowStorage(phase);
      const fetch = async (url) => {
        if (url.endsWith("/v1/version")) return protocolResponse();
        if (url.endsWith(":import-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: SOURCE_EVACUATION_ID,
            status: "suspended",
          });
        }
        if (url.endsWith(":complete-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: SOURCE_EVACUATION_ID,
            status: "active",
            completed: true,
          });
        }
        throw new Error(`unexpected fetch ${url}`);
      };
      const coordinator = runtime({
        directory,
        bucket,
        storage,
        fetch,
      });

      const response = await coordinator.fetch(
        request("restore", {
          cell_name: TARGET,
          archive_object: object,
          archive_id: pointer.archive_id,
          expected_epoch: 3,
        }),
      );
      if (phase === "archive_retired") {
        assert.equal(response.status, 500);
      } else {
        assert.equal(response.status, 200);
        assert.equal(
          (await response.json()).result.cleanup_pending,
          true,
        );
      }
      assert.equal(
        storage.values.get("account-lifecycle").operation.phase,
        phase,
      );
      assert.notEqual(storage.alarm, null);
      if (phase === "archive_retired") {
        assert.equal(bucket.values.has(object), true);
      } else {
        assert.equal(bucket.values.has(object), false);
      }

      const restarted = runtime({
        directory,
        bucket,
        storage,
        fetch,
      });
      await restarted.alarm();
      const completed = storage.values.get("account-lifecycle");
      assert.equal(completed.operation, null);
      assert.equal(completed.location.kind, "live");
      assert.equal(completed.location.cell, TARGET);
      assert.equal(bucket.values.has(object), false);
      assert.equal(storage.alarm, null);
    });
  }
});

test("ambiguous multipart completion is deleted only after exact abort", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(
      SOURCE,
      "https://source.example",
      false,
    ),
  });
  const events = [];
  const bucket = new Bucket();
  const originalDelete = bucket.delete.bind(bucket);
  bucket.delete = async (key) => {
    events.push(`delete:${key}`);
    return originalDelete(key);
  };
  const coordinator = new DurableAccountLifecycle(
    context(),
    {
      DIRECTORY: directory,
      ARCHIVES: bucket,
    },
    {
      fetch: async (url, init = {}) => {
        if (url.endsWith("/v1/version")) return protocolResponse();
        if (url.endsWith(":begin-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: OPERATION_ID,
            evacuation_role: "source",
            status: "suspended",
          });
        }
        if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
          return Response.json({
            account_id: ACCOUNT,
            placement_policy: { allowed_clouds: ["aws"] },
          });
        }
        if (url.endsWith(":export-evacuation")) {
          return new Response(new Uint8Array([1]));
        }
        if (url.endsWith(":abort-evacuation")) {
          events.push("abort");
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: JSON.parse(init.body).evacuation_id,
            evacuation_role: "source",
            status: "active",
            aborted: true,
          });
        }
        throw new Error(`unexpected fetch ${url}`);
      },
      randomUUID: () => OPERATION_ID,
      now: () => new Date("2026-07-25T12:00:00.000Z"),
      streamArchive: async (archiveBucket, key) => {
        archiveBucket.values.set(key, new Uint8Array([1, 2, 3]));
        events.push("complete-committed");
        throw new Error("lost multipart completion acknowledgement");
      },
      verifyArchive: async () => {
        throw new Error("verification must not run");
      },
    },
  );

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 500);
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  assert.deepEqual(events, [
    "complete-committed",
    "abort",
    `delete:${object}`,
  ]);
  assert.equal(bucket.values.has(object), false);
  assert.equal(
    coordinator.storage.values.get("account-lifecycle").last_completed.outcome,
    "aborted",
  );
});

test("failed exact delete after ambiguous completion remains durable across restart", async (t) => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example", false),
  });
  const storage = new Storage();
  const bucket = new Bucket();
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  const unrelatedObject = `archives/${ACCOUNT}/unrelated.tar.gz`;
  bucket.values.set(unrelatedObject, new Uint8Array([9]));
  bucket.failDeleteOnce = true;
  const events = [];
  let exportCalls = 0;
  let abortCalls = 0;
  const originalDelete = bucket.delete.bind(bucket);
  bucket.delete = async (key) => {
    events.push(`delete:${key}`);
    return originalDelete(key);
  };
  const dependencies = {
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          evacuation_role: "source",
          status: "suspended",
        });
      }
      if (url.endsWith("/placement-policy")) {
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["aws"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        exportCalls++;
        return new Response(new Uint8Array([1]));
      }
      if (url.endsWith(":abort-evacuation")) {
        abortCalls++;
        events.push("abort");
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          evacuation_role: "source",
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
    randomUUID: () => OPERATION_ID,
    now: () => new Date("2026-07-25T12:00:00.000Z"),
    streamArchive: async (archiveBucket, key) => {
      archiveBucket.values.set(key, new Uint8Array([1, 2, 3]));
      events.push("complete-committed");
      throw new Error("lost multipart completion acknowledgement");
    },
    verifyArchive: async () => {
      throw new Error("verification must not run");
    },
  };
  const createRuntime = () => new DurableAccountLifecycle(
    { storage, id: { name: ACCOUNT } },
    { DIRECTORY: directory, ARCHIVES: bucket },
    dependencies,
  );
  const response = await createRuntime().fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 500);
  assert.deepEqual(events, [
    "complete-committed", "abort", `delete:${object}`,
  ]);
  assert.equal(bucket.values.has(object), true);
  const first = structuredClone(storage.values.get("account-lifecycle"));
  const pending = {
    exact_operation_retained: first.operation?.operation_id === OPERATION_ID,
    alarm_scheduled: storage.alarm !== null,
  };

  // Invoke the restarted alarm even when the broken implementation cleared
  // it, proving that an external wakeup alone cannot recover the lost intent.
  await createRuntime().alarm();
  const completed = storage.values.get("account-lifecycle");
  const observed = {
    ...pending,
    object_removed_after_restart: !bucket.values.has(object),
    export_calls: exportCalls,
    abort_calls: abortCalls,
    delete_attempts: events.filter((event) => event === `delete:${object}`).length,
  };
  t.diagnostic(JSON.stringify(observed));
  assert.equal(completed.operation, null);
  assert.equal(completed.last_completed.outcome, "aborted");
  assert.equal(completed.location.kind, "live");
  assert.equal(completed.location.cell, SOURCE);
  assert.deepEqual(directory.value(`acct:${ACCOUNT}`), sourceRoute());
  assert.equal(directory.value(`archived:${ACCOUNT}`), null);
  assert.deepEqual([...bucket.values.get(unrelatedObject)], [9]);
  assert.equal(storage.alarm, null);
  assert.deepEqual(observed, {
    exact_operation_retained: true,
    alarm_scheduled: true,
    object_removed_after_restart: true,
    export_calls: 1,
    abort_calls: 1,
    delete_attempts: 2,
  });
});

function abortCleanupFixture({ kind = "evacuate", fault = null, receipt = {} } = {}) {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(7),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example", false),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  let faultRaised = false;
  const trip = (boundary) => {
    if (fault === boundary && !faultRaised) {
      faultRaised = true;
      throw new Error(`injected ${boundary} failure`);
    }
  };
  class AbortStorage extends Storage {
    async put(key, value) {
      const phase = value.operation?.phase ??
        (value.last_completed?.outcome === "aborted" ? "abort_terminal" : "other");
      trip(`${phase}:before`);
      await super.put(key, value);
      this.savedStates ??= [];
      this.savedStates.push(structuredClone(value));
      trip(`${phase}:after`);
    }
  }
  const storage = new AbortStorage();
  const bucket = new Bucket();
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  const unrelated = `archives/${ACCOUNT}/unrelated.tar.gz`;
  bucket.values.set(unrelated, new Uint8Array([9]));
  const calls = { export: 0, abort: 0, delete: 0, release: 0, reserve: 0 };
  let sourceAborted = false;
  let rejectAbort = false;
  const proof = {
    account_id: ACCOUNT, evacuation_id: OPERATION_ID,
    evacuation_role: "source", status: "active", aborted: true,
  };
  const originalDelete = bucket.delete.bind(bucket);
  bucket.delete = async (key) => {
    calls.delete++;
    assert.equal(key, object);
    assert.equal(sourceAborted, true);
    const stored = storage.values.get("account-lifecycle");
    assert.equal(stored.operation.phase, "abort_cleanup_pending");
    assert.deepEqual(stored.operation.abort_receipt, { ...proof, ...receipt });
    trip("delete:before");
    await originalDelete(key);
    trip("delete:after");
  };
  const authority = new TargetAuthority(directory);
  const dependencies = {
    randomUUID: () => OPERATION_ID,
    now: () => new Date("2026-07-25T12:00:00.000Z"),
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        assert.equal(sourceAborted, false);
        return Response.json({ ...proof, status: "suspended", aborted: false });
      }
      if (url.endsWith("/placement-policy")) {
        assert.equal(sourceAborted, false);
        return Response.json({
          account_id: ACCOUNT,
          placement_policy: { allowed_clouds: ["aws", "civo"] },
        });
      }
      if (url.endsWith(":export-evacuation")) {
        assert.equal(sourceAborted, false, "never export after source abort");
        calls.export++;
        return new Response(new Uint8Array([1]));
      }
      if (url.endsWith(":abort-evacuation")) {
        calls.abort++;
        const stored = storage.values.get("account-lifecycle");
        assert.equal(stored.operation.phase, "abort_requested");
        assert.equal(JSON.parse(init.body).evacuation_id, OPERATION_ID);
        assert.notEqual(storage.alarm, null);
        trip("abort_response:before");
        sourceAborted = true;
        trip("abort_response:after");
        return Response.json({
          ...proof, ...receipt,
          ...(rejectAbort ? { evacuation_id: "different" } : {}),
          ignored_provider_field: "must-not-be-persisted",
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
    streamArchive: async (archiveBucket, key) => {
      archiveBucket.values.set(key, new Uint8Array([1, 2, 3]));
      throw new Error("lost multipart completion acknowledgement");
    },
    verifyArchive: async () => { throw new Error("verification must not run"); },
    targetCoordinatorRequest: async (cellName, path, payload) => {
      if (path === "/reserve") {
        calls.reserve++;
        const phase = storage.values.get("account-lifecycle")?.operation?.phase;
        assert.ok(!["abort_requested", "abort_cleanup_pending"].includes(phase));
      }
      if (path === "/release") {
        calls.release++;
        assert.equal(bucket.values.has(object), false);
        trip("release:before");
      }
      const result = await authority.request(cellName, path, payload);
      if (path === "/release") trip("release:after");
      return result;
    },
  };
  const restart = () => new DurableAccountLifecycle(
    { storage, id: { name: ACCOUNT } },
    { DIRECTORY: directory, ARCHIVES: bucket }, dependencies,
  );
  return {
    storage, bucket, directory, object, unrelated, calls, authority, restart,
    setRejectAbort: (value) => { rejectAbort = value; },
    sourceAborted: () => sourceAborted,
    request: () => request(kind, kind === "move"
      ? { source_cell: SOURCE, target_cell: TARGET }
      : { cell_name: SOURCE }),
  };
}

test("pre-archive abort resumes every durable and external failure boundary", async (t) => {
  for (const kind of ["evacuate", "move"]) {
    const faults = [
      "abort_requested:before", "abort_requested:after",
      "abort_cleanup_pending:before", "abort_cleanup_pending:after",
      "abort_response:before", "abort_response:after", "delete:before", "delete:after",
      "abort_terminal:before", "abort_terminal:after",
      ...(kind === "move" ? ["release:before", "release:after"] : []),
    ];
    for (const fault of faults) {
      await t.test(`${kind} ${fault}`, async () => {
        const f = abortCleanupFixture({ kind, fault });
        const response = await f.restart().fetch(f.request());
        assert.equal(response.status, fault.startsWith("abort_response:") ? 502 : 500);
        assert.notEqual(f.storage.alarm, null);
        const first = structuredClone(f.storage.values.get("account-lifecycle"));
        if (fault === "abort_requested:before") {
          assert.equal(f.sourceAborted(), false);
          assert.equal(first.operation.phase, "source_suspended");
        } else if (fault !== "abort_terminal:after") {
          assert.ok(["abort_requested", "abort_cleanup_pending"].includes(first.operation.phase));
        }
        if (fault.startsWith("abort_requested:") ||
          fault.startsWith("abort_cleanup_pending:") || fault.startsWith("abort_response:")) {
          assert.equal(f.calls.delete, 0);
          assert.equal(f.bucket.values.has(f.object), true);
        }
        const reserveCalls = f.calls.reserve;
        await f.restart().alarm();
        const completed = f.storage.values.get("account-lifecycle");
        assert.equal(completed.operation, null);
        assert.equal(completed.last_completed.outcome, "aborted");
        assert.equal(completed.last_completed.epoch, 8);
        assert.deepEqual(completed.location.route, sourceRoute(7));
        assert.deepEqual(f.directory.value(`acct:${ACCOUNT}`), sourceRoute(7));
        assert.equal(f.directory.value(`archived:${ACCOUNT}`), null);
        assert.equal(f.bucket.values.has(f.object), false);
        assert.deepEqual([...f.bucket.values.get(f.unrelated)], [9]);
        if (fault === "abort_requested:before") {
          // No intent was committed on the first attempt. Its alarm may
          // re-export, settle abort, then rearm for the original export error.
          // The harmless terminal wakeup must not repeat any external work.
          assert.notEqual(f.storage.alarm, null);
          const settledCalls = { ...f.calls };
          await f.restart().alarm();
          assert.deepEqual(f.calls, settledCalls);
        }
        assert.equal(f.storage.alarm, null);
        assert.equal(f.calls.export, fault === "abort_requested:before" ? 2 : 1);
        if (fault === "abort_requested:before") {
          assert.equal(f.storage.savedStates.findLast(s => s.operation?.phase === "source_suspended").operation.export_job.stream_attempts, 2);
        }
        assert.equal(f.calls.abort,
          ["abort_cleanup_pending:before", "abort_response:before", "abort_response:after"].includes(fault) ? 2 : 1);
        assert.equal(f.authority.reservations.size, 0);
        if (fault !== "abort_requested:before") assert.equal(f.calls.reserve, reserveCalls);
      });
    }
  }
});

test("proven move abort cleans up after its target reservation expires", async () => {
  const f = abortCleanupFixture({ kind: "move", fault: "delete:before" });
  assert.equal((await f.restart().fetch(f.request())).status, 500);
  const reserveCalls = f.calls.reserve;
  f.authority.reservations.clear();
  f.directory.values.delete(`cell:${TARGET}`);
  await f.restart().alarm();
  assert.equal(f.calls.reserve, reserveCalls);
  assert.equal(f.calls.export, 1);
  assert.equal(f.calls.abort, 1);
  assert.equal(f.calls.delete, 2);
  assert.equal(f.calls.release, 1);
  assert.equal(f.bucket.values.has(f.object), false);
  assert.equal(f.storage.values.get("account-lifecycle").operation, null);
  assert.equal(f.storage.alarm, null);
});

test("foreground abort cleanup never reports a successful evacuation or move", async (t) => {
  for (const kind of ["evacuate", "move"]) {
    await t.test(kind, async () => {
      const f = abortCleanupFixture({ kind, fault: "delete:before" });
      assert.equal((await f.restart().fetch(f.request())).status, 500);
      const response = await f.restart().fetch(f.request());
      assert.equal(f.storage.values.get("account-lifecycle").operation, null);
      assert.deepEqual(f.directory.value(`acct:${ACCOUNT}`), sourceRoute(7));
      assert.equal(f.directory.value(`archived:${ACCOUNT}`), null);
      assert.equal(f.bucket.values.has(f.object), false);
      assert.equal(f.storage.alarm, null);
      assert.equal(f.calls.export, 1);
      assert.equal(f.calls.abort, 1);
      assert.equal(response.status, 502);
      assert.match((await response.json()).error, /aborted.*cleanup completed/);
    });
  }
});

test("abort receipt refusal retains the candidate and retries only the exact source abort", async () => {
  const f = abortCleanupFixture();
  f.setRejectAbort(true);
  assert.equal((await f.restart().fetch(f.request())).status, 502);
  assert.equal(f.storage.values.get("account-lifecycle").operation.phase, "abort_requested");
  assert.equal(f.bucket.values.has(f.object), true);
  assert.equal(f.calls.delete, 0);
  assert.notEqual(f.storage.alarm, null);
  f.setRejectAbort(false);
  await f.restart().alarm();
  assert.equal(f.calls.export, 1);
  assert.equal(f.calls.abort, 2);
  assert.equal(f.calls.delete, 1);
  assert.equal(f.storage.alarm, null);
});

test("abort cleanup preserves all three source-supported receipt statuses", async (t) => {
  for (const status of ["active", "suspended", "closed"]) {
    await t.test(status, async () => {
      const f = abortCleanupFixture({ receipt: { status } });
      assert.equal((await f.restart().fetch(f.request())).status, 500);
      assert.equal(f.storage.values.get("account-lifecycle").last_completed.outcome, "aborted");
      assert.equal(f.calls.delete, 1);
      assert.equal(f.storage.alarm, null);
    });
  }
});

test("unsupported abort receipt status stays abort-requested without deletion", async (t) => {
  for (const [name, status] of [["unexpected", "pending"], ["oversized", "x".repeat(4096)]]) {
    await t.test(name, async (t) => {
      const f = abortCleanupFixture({ receipt: { status } });
      assert.equal((await f.restart().fetch(f.request())).status, 500);
      const stored = f.storage.values.get("account-lifecycle");
      const observed = {
        status_length: status.length,
        durable_cleanup_proof: f.storage.savedStates.some((s) =>
          s.operation?.phase === "abort_cleanup_pending"),
        phase: stored.operation?.phase ?? "terminal",
        delete_calls: f.calls.delete,
        candidate_retained: f.bucket.values.has(f.object),
        alarm_retained: f.storage.alarm !== null,
      };
      t.diagnostic(JSON.stringify(observed));
      assert.deepEqual(observed, {
        status_length: status.length,
        durable_cleanup_proof: false,
        phase: "abort_requested",
        delete_calls: 0,
        candidate_retained: true,
        alarm_retained: true,
      });
    });
  }
});

test("malformed persisted abort proof cannot delete, renew or export", async (t) => {
  const mutations = [
    (s) => { delete s.operation.abort_receipt; },
    (s) => { s.operation.abort_receipt.aborted = false; },
    (s) => { s.operation.abort_receipt.account_id = "different"; },
    (s) => { s.operation.abort_receipt.evacuation_id = "different"; },
    (s) => { s.operation.abort_receipt.evacuation_role = "target"; },
    (s) => { s.operation.abort_receipt.status = ""; },
    (s) => { s.operation.abort_receipt.status = "pending"; },
    (s) => { s.operation.abort_receipt.status = "x".repeat(4096); },
    (s) => { s.operation.abort_receipt.extra = true; },
    (s) => { s.operation.archive.object = "archives/other/candidate.tar.gz"; },
    (s) => { s.operation.archive.archive_id = "different"; },
    (s) => { s.operation.epoch++; },
    (s) => { s.operation.request_epoch++; },
    (s) => { s.operation.source_registration_id = "different"; },
  ];
  for (const [index, mutate] of mutations.entries()) {
    await t.test(String(index), async () => {
      const f = abortCleanupFixture({ kind: "move", fault: "delete:before" });
      assert.equal((await f.restart().fetch(f.request())).status, 500);
      const state = structuredClone(f.storage.values.get("account-lifecycle"));
      mutate(state);
      f.storage.values.set("account-lifecycle", state);
      const calls = { ...f.calls };
      await f.restart().alarm();
      assert.deepEqual(f.calls, calls);
      assert.deepEqual(f.storage.values.get("account-lifecycle"), state);
      assert.equal(f.bucket.values.has(f.object), true);
      assert.notEqual(f.storage.alarm, null);
    });
  }
});

test("contradictory import and resume receipts never retire the archive", async (t) => {
  const fixtures = [
    {
      name: "completed import is also aborted",
      import: {
        status: "active",
        evacuation_completed: true,
        aborted: true,
      },
      resume: null,
      error: /aborted evacuation receipt/,
    },
    {
      name: "resume returns a garbage status",
      import: { status: "suspended" },
      resume: { status: "pending", completed: true },
      error: /did not attest completed evacuation/,
    },
    {
      name: "live import unexpectedly resumes closed",
      import: { status: "suspended" },
      resume: { status: "closed", completed: true },
      error: /unexpectedly converted a live import into a tombstone/,
    },
  ];

  for (const fixture of fixtures) {
    await t.test(fixture.name, async () => {
      const object = `archives/${ACCOUNT}/${fixture.name.replaceAll(" ", "-")}.tar.gz`;
      const pointer = {
        archive_id: `archive_${fixture.name.replaceAll(" ", "_")}`,
        evacuation_id: SOURCE_EVACUATION_ID,
        object,
        cell: SOURCE,
        source_cell: SOURCE,
        region: "us-west",
        status: "suspended",
      };
      const directory = new KV({
        [`archived:${ACCOUNT}`]: pointer,
        [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
      });
      const bucket = new Bucket();
      bucket.values.set(object, new Uint8Array([1]));
      let resumeCalls = 0;
      const coordinator = runtime({
        directory,
        bucket,
        fetch: async (url) => {
          if (url.endsWith("/v1/version")) return protocolResponse();
          if (url.endsWith(":import-evacuation")) {
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: SOURCE_EVACUATION_ID,
              ...fixture.import,
            });
          }
          if (url.endsWith(":complete-evacuation")) {
            resumeCalls++;
            return Response.json({
              account_id: ACCOUNT,
              evacuation_id: SOURCE_EVACUATION_ID,
              ...fixture.resume,
            });
          }
          throw new Error(`unexpected fetch ${url}`);
        },
      });

      const response = await coordinator.fetch(
        request("restore", {
          cell_name: TARGET,
          archive_object: object,
          archive_id: pointer.archive_id,
        }),
      );
      assert.equal(response.status, 502);
      assert.match((await response.json()).error, fixture.error);
      assert.equal(resumeCalls, fixture.resume ? 1 : 0);
      assert.equal(directory.value(`acct:${ACCOUNT}`), null);
      assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
      assert.equal(bucket.values.has(object), true);
      assert.equal(bucket.deleted.length, 0);
    });
  }
});

test("close follows the fresh cell endpoint after endpoint rotation", async () => {
  const oldEndpoint = "https://old-source.example";
  const newEndpoint = "https://new-source.example";
  const directory = new KV({
    [`acct:${ACCOUNT}`]: {
      ...sourceRoute(5),
      endpoint: oldEndpoint,
    },
    [`cell:${SOURCE}`]: cell(SOURCE, newEndpoint),
  });
  const bucket = new Bucket();
  const calls = [];
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      calls.push(url);
      if (url === `${newEndpoint}/v1/account:close`) {
        return Response.json({
          account_id: ACCOUNT,
          status: "closed",
        });
      }
      throw new Error(`stale or unexpected endpoint ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
      expected_epoch: 5,
    }),
  );
  assert.equal(response.status, 200);
  assert.deepEqual(calls, [`${newEndpoint}/v1/account:close`]);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
});

test("cancelled close preserves the live projection epoch for a later move", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(7),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  const operationIDs = [
    "close_epoch_001",
    "33333333-3333-4333-8333-333333333333",
  ];
  let suspendCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    randomUUID: () => operationIDs.shift(),
    fetch: async (url) => {
      if (url.endsWith("/v1/account:close")) {
        return Response.json({ error: "unauthorized" }, { status: 401 });
      }
      if (url.endsWith(":contact")) {
        return Response.json({
          account_id: ACCOUNT,
          status: "active",
        });
      }
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        suspendCalls++;
        throw new Error("stop after accepted retry claim");
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const refused = await coordinator.fetch(
    request("close", {
      authorization: "Bearer owner-token",
      body: "{}",
      expected_epoch: 7,
    }),
  );
  assert.equal(refused.status, 401);
  assert.equal(directory.value(`acct:${ACCOUNT}`).epoch, 7);

  const retried = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 7,
    }),
  );
  const retriedBody = await retried.json();
  assert.match(retriedBody.error, /begin evacuation outcome is ambiguous/);
  assert.equal(retried.status, 502);
  assert.equal(suspendCalls, 1);
  const state = coordinator.storage.values.get("account-lifecycle");
  assert.equal(state.operation.kind, "move");
  assert.equal(state.operation.request_epoch, 7);
  assert.equal(state.operation.epoch, 9);
});

test("prearchive move abort preserves the live projection epoch for retry", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(7),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  const operationIDs = [
    "44444444-4444-4444-8444-444444444444",
    "55555555-5555-4555-8555-555555555555",
  ];
  let suspendCalls = 0;
  let placementCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    randomUUID: () => operationIDs.shift(),
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        suspendCalls++;
        if (suspendCalls === 1) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: "44444444-4444-4444-8444-444444444444",
            status: "suspended",
          });
        }
        throw new Error("stop after accepted retry claim");
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        placementCalls++;
        return Response.json({ account_id: ACCOUNT });
      }
      if (url.endsWith(":abort-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: "44444444-4444-4444-8444-444444444444",
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const first = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 7,
    }),
  );
  const firstBody = await first.json();
  assert.match(firstBody.error, /placement policy read returned/);
  assert.equal(first.status, 502);
  assert.equal(placementCalls, 1);
  assert.equal(
    coordinator.storage.values.get("account-lifecycle").last_completed.outcome,
    "aborted",
  );
  assert.equal(directory.value(`acct:${ACCOUNT}`).epoch, 7);

  const retry = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 7,
    }),
  );
  const retryBody = await retry.json();
  assert.match(retryBody.error, /begin evacuation outcome is ambiguous/);
  assert.equal(retry.status, 502);
  assert.equal(suspendCalls, 2);
  const state = coordinator.storage.values.get("account-lifecycle");
  assert.equal(
    state.operation.operation_id,
    "55555555-5555-4555-8555-555555555555",
  );
  assert.equal(state.operation.request_epoch, 7);
  assert.equal(state.operation.epoch, 9);
});

test("alarm completes an owner close after contact proves the lost acknowledgement", async () => {
  let state = bootstrapLiveState({
    account_id: ACCOUNT,
    route: sourceRoute(2),
  });
  state = claimOperation(state, {
    operation_id: "close_alarm_001",
    kind: "close",
    source_cell: SOURCE,
  });
  const storage = new Storage();
  storage.values.set("account-lifecycle", state);
  storage.alarm = Date.now();
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(2),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
  });
  const bucket = new Bucket();
  let ownerCloseCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    storage,
    fetch: async (url) => {
      if (url.endsWith(":contact")) {
        return Response.json({
          account_id: ACCOUNT,
          status: "closed",
        });
      }
      if (url.endsWith("/v1/account:close")) {
        ownerCloseCalls++;
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  await coordinator.alarm();
  const completed = storage.values.get("account-lifecycle");
  assert.equal(completed.operation, null);
  assert.equal(completed.location.kind, "closed");
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(ownerCloseCalls, 0);
  assert.equal(storage.alarm, null);
});

test("target deletion after preflight cancels before source mutation", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(4),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  let sourceCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.includes("source.example")) sourceCalls++;
      throw new Error(`unexpected fetch ${url}`);
    },
    targetCoordinatorRequest: async () => {
      await directory.delete(`cell:${TARGET}`);
      throw new AccountLifecycleRuntimeError(
        "target cell is not registered",
        409,
      );
    },
  });

  const response = await coordinator.fetch(
    request("move", {
      source_cell: SOURCE,
      target_cell: TARGET,
      expected_epoch: 4,
    }),
  );
  assert.equal(response.status, 409);
  assert.equal(sourceCalls, 0);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, SOURCE);
  const state = coordinator.storage.values.get("account-lifecycle");
  assert.equal(state.operation, null);
  assert.equal(state.last_completed.outcome, "cancelled");
});

test("generic placement-policy 409 cannot stand in for exact application", async () => {
  const object = "archives/acct_runtime/policy-409.tar.gz";
  const pointer = {
    archive_id: "archive_policy_409",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    region: "us-west",
    placement_policy: { allowed_clouds: ["civo"] },
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  let resumeCalls = 0;
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":import-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: SOURCE_EVACUATION_ID,
          evacuation_role: "target",
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json(
          { error: "account evacuation id does not match" },
          { status: 409 },
        );
      }
      if (url.endsWith(":complete-evacuation")) resumeCalls++;
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(response.status, 409);
  assert.match((await response.json()).error, /placement policy 409/);
  assert.equal(resumeCalls, 0);
  assert.equal(directory.value(`acct:${ACCOUNT}`), null);
  assert.equal(directory.value(`archived:${ACCOUNT}`).object, object);
  assert.equal(
    coordinator.storage.values.get("account-lifecycle").operation.phase,
    "target_imported",
  );
});

test("same-name source replacement is not finalized as the archived source", async () => {
  const object = "archives/acct_runtime/replaced-source.tar.gz";
  const pointer = {
    archive_id: "archive_replaced_source",
    evacuation_id: SOURCE_EVACUATION_ID,
    object,
    cell: SOURCE,
    source_cell: SOURCE,
    source_registration_id: "old-source-registration",
    exported_at: "2026-07-24T00:00:00.000Z",
    region: "us-west",
  };
  const replacement = {
    ...cell(SOURCE, "https://replacement-source.example"),
    registration_id: "new-source-registration",
    registered_at: "2026-07-25T01:00:00.000Z",
  };
  const directory = new KV({
    [`archived:${ACCOUNT}`]: pointer,
    [`cell:${SOURCE}`]: replacement,
    [`cell:${TARGET}`]: cell(TARGET, "https://target.example"),
  });
  const bucket = new Bucket();
  bucket.values.set(object, new Uint8Array([1]));
  const storage = new PhaseCommitThenThrowStorage("source_finalized");
  let finalizationCalls = 0;
  const fetch = async (url, init = {}) => {
    if (url.endsWith("/v1/version")) return protocolResponse();
    if (url.endsWith(":import-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: SOURCE_EVACUATION_ID,
        evacuation_role: "target",
        status: "suspended",
      });
    }
    if (url.endsWith(":complete-evacuation")) {
      return Response.json({
        account_id: ACCOUNT,
        evacuation_id: JSON.parse(init.body).evacuation_id,
        evacuation_role: "target",
        status: "active",
        completed: true,
      });
    }
    if (url.endsWith(":finalize-evacuation")) {
      finalizationCalls++;
      throw new Error("replacement cell must never receive source finalize");
    }
    throw new Error(`unexpected fetch ${url}`);
  };
  const first = runtime({
    directory,
    bucket,
    storage,
    fetch,
  });
  const response = await first.fetch(
    request("restore", {
      cell_name: TARGET,
      archive_object: object,
      archive_id: pointer.archive_id,
    }),
  );
  assert.equal(response.status, 500);
  assert.equal(finalizationCalls, 0);
  const committed = storage.values.get("account-lifecycle");
  assert.equal(committed.operation.phase, "source_finalized");
  assert.equal(
    committed.operation.source_finalization.mode,
    "source_cell_replaced",
  );

  const restarted = runtime({
    directory,
    bucket,
    storage,
    fetch,
  });
  await restarted.alarm();
  assert.equal(storage.values.get("account-lifecycle").operation, null);
  assert.equal(directory.value(`acct:${ACCOUNT}`).cell, TARGET);
});

for (
  const {
    name,
    beginRole = "source",
    importRole = "target",
    resumeRole = "target",
    expected,
  } of [
    {
      name: "begin evacuation rejects a target-role receipt",
      beginRole: "target",
      expected: /begin evacuation returned 2xx without an exact/,
    },
    {
      name: "import rejects a source-role receipt",
      importRole: "source",
      expected: /import returned 2xx without an exact/,
    },
    {
      name: "resume rejects a source-role receipt",
      resumeRole: "source",
      expected: /resume returned 2xx without an exact/,
    },
  ]
) {
  test(name, async () => {
    const object = "archives/acct_runtime/wrong-role.tar.gz";
    const isBeginCase = beginRole === "target";
    const directory = new KV(
      isBeginCase
        ? {
            [`acct:${ACCOUNT}`]: sourceRoute(),
            [`cell:${SOURCE}`]: cell(
              SOURCE,
              "https://source.example",
            ),
          }
        : {
            [`archived:${ACCOUNT}`]: {
              archive_id: "archive_wrong_role",
              evacuation_id: SOURCE_EVACUATION_ID,
              object,
              cell: SOURCE,
            },
            [`cell:${TARGET}`]: cell(
              TARGET,
              "https://target.example",
            ),
          },
    );
    const bucket = new Bucket();
    bucket.values.set(object, new Uint8Array([1]));
    const coordinator = runtime({
      directory,
      bucket,
      fetch: async (url, init = {}) => {
        if (url.endsWith("/v1/version")) return protocolResponse();
        if (url.endsWith(":begin-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: OPERATION_ID,
            evacuation_role: beginRole,
            status: "suspended",
          });
        }
        if (url.endsWith(":import-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: SOURCE_EVACUATION_ID,
            evacuation_role: importRole,
            status: "suspended",
          });
        }
        if (url.endsWith(":complete-evacuation")) {
          return Response.json({
            account_id: ACCOUNT,
            evacuation_id: JSON.parse(init.body).evacuation_id,
            evacuation_role: resumeRole,
            status: "active",
            completed: true,
          });
        }
        throw new Error(`unexpected fetch ${url}`);
      },
    });

    const response = await coordinator.fetch(
      isBeginCase
        ? request("evacuate", { cell_name: SOURCE })
        : request("restore", {
            cell_name: TARGET,
            archive_object: object,
            archive_id: "archive_wrong_role",
          }),
    );
    assert.equal(response.status, 502);
    assert.match((await response.json()).error, expected);
    assert.equal(bucket.deleted.length, 0);
  });
}

test("abort evacuation rejects a target-role receipt", async () => {
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example"),
  });
  const bucket = new Bucket();
  const coordinator = runtime({
    directory,
    bucket,
    fetch: async (url, init = {}) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      if (url.endsWith(":begin-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: OPERATION_ID,
          evacuation_role: "source",
          status: "suspended",
        });
      }
      if ((url.endsWith("/placement-policy") || url.endsWith(":restore-placement-policy"))) {
        return Response.json({ account_id: ACCOUNT });
      }
      if (url.endsWith(":abort-evacuation")) {
        return Response.json({
          account_id: ACCOUNT,
          evacuation_id: JSON.parse(init.body).evacuation_id,
          evacuation_role: "target",
          status: "active",
          aborted: true,
        });
      }
      throw new Error(`unexpected fetch ${url}`);
    },
  });

  const response = await coordinator.fetch(
    request("evacuate", { cell_name: SOURCE }),
  );
  assert.equal(response.status, 502);
  assert.match(
    (await response.json()).error,
    /abort evacuation returned 2xx without an exact/,
  );
  const state = coordinator.storage.values.get("account-lifecycle");
  assert.equal(state.operation.phase, "abort_requested");
  assert.equal(state.last_completed, null);
});

function evacuationPullFixture({ protocol = 2, size = 7 } = {}) {
  const pointer = { archive_id: "archive_pull", object: `archives/${ACCOUNT}/pull.tar.gz`,
    evacuation_id: OPERATION_ID, cell: SOURCE, source_cell: SOURCE,
    source_registration_id: `reg-${SOURCE}`, status: "suspended" };
  const state = archivedOperationState("move", pointer, TARGET);
  state.operation.archive_origin = "https://initiator.example";
  const storage = new Storage();
  storage.values.set("account-lifecycle", state);
  const directory = new KV({ [`cell:${TARGET}`]: cell(TARGET, "https://target.example") });
  const bucket = new Bucket();
  bucket.values.set(pointer.object, "archive");
  bucket.head = async () => ({ etag: "bound-etag", size });
  let now = new Date("2026-07-25T12:00:00Z");
  const requests = [];
  const coordinator = runtime({ directory, bucket, storage, fetch: async (url, init) => {
    if (url.endsWith("/v1/version")) return Response.json({ account_evacuation_protocol: protocol });
    requests.push(init);
    return new Response("\n\n" + JSON.stringify({ account_id: ACCOUNT, evacuation_id: OPERATION_ID,
      evacuation_role: "target", status: "suspended" }));
  } });
  coordinator.now = () => now;
  const env = { ARCHIVES: bucket, PUBLIC_IP_LIMITER: { limit: () => ({ success: true }) },
    ACCOUNT_LIFECYCLE: { idFromName: (id) => id, get: (id) => ({ fetch: (r) => id === ACCOUNT ? coordinator.fetch(r) : new Response(null, { status: 404 }) }) } };
  return { coordinator, state, storage, bucket, requests, env, pointer,
    expire: () => { now = new Date(now.getTime() + 1800_000); } };
}

const { accountArchivePull } = await import("../src/account-lifecycle-runtime.mjs");
const { ExportWatchdog } = await import("../src/account-backup-runtime.mjs");

function pullRequest(token, evacuationID = OPERATION_ID) {
  return new Request(`https://initiator.example/v1/archives:pull?account_id=${ACCOUNT}&evacuation_id=${evacuationID}`, {
    headers: { Authorization: `Bearer ${token}`, "CF-Connecting-IP": "192.0.2.48" },
  });
}

test("evacuation capability binds operation and ETag, hashes at rest, and consumes once concurrently", async () => {
  const f = evacuationPullFixture();
  const token = await f.coordinator.mintArchiveCapability(f.state.operation, await f.bucket.head());
  const entries = [...await f.storage.list({ prefix: "archive-capability:" })];
  assert.equal(entries.length, 1);
  assert.ok(/^archive-capability:[0-9a-f]{64}$/.test(entries[0][0]));
  assert.ok(!JSON.stringify(entries).includes(token));
  assert.equal(entries[0][1].target_cell, TARGET);
  f.bucket.get = async (object, options) => {
    assert.equal(object, f.pointer.object);
    assert.deepEqual(options, { onlyIf: { etagMatches: "bound-etag" } });
    return { body: new Response("archive").body, size: 7, etag: "bound-etag" };
  };
  const responses = await Promise.all([accountArchivePull(pullRequest(token), f.env), accountArchivePull(pullRequest(token), f.env)]);
  assert.deepEqual(responses.map((r) => r.status).sort(), [200, 404]);
  const success = responses.find((r) => r.status === 200);
  assert.equal(success.headers.get("Content-Length"), "7");
  assert.equal(success.headers.get("Content-Type"), "application/gzip");
  assert.equal(success.headers.get("Cache-Control"), "no-store");
  assert.equal(success.headers.get("X-Witself-Evacuation-ID"), OPERATION_ID);
  assert.equal(await success.text(), "archive");
  assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
});

for (const fault of ["expired", "wrong-id", "wrong-account", "changed-target", "changed-object", "etag", "size", "r2-failure", "bad-token", "method"]) {
  test(`evacuation archive uniform 404: ${fault}`, async () => {
    const f = evacuationPullFixture();
    const token = await f.coordinator.mintArchiveCapability(f.state.operation, await f.bucket.head());
    let req = pullRequest(token);
    if (fault === "expired") f.expire();
    if (fault === "wrong-id") req = pullRequest(token, "different");
    if (fault === "wrong-account") req = new Request(req.url.replace(ACCOUNT, "other"), req);
    if (fault === "changed-target") f.state.operation.target_cell = "different";
    if (fault === "changed-object") f.state.operation.archive.object = "different";
    if (fault === "bad-token") req = pullRequest("bad");
    if (fault === "method") req = new Request(req.url, { method: "POST", headers: req.headers });
    f.bucket.get = async () => {
      if (fault === "r2-failure") throw new Error("private provider failure");
      return { body: new Response("archive").body, size: fault === "size" ? 8 : 7, etag: fault === "etag" ? "changed" : "bound-etag" };
    };
    const response = await accountArchivePull(req, f.env);
    assert.equal(response.status, 404);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    assert.deepEqual(await response.json(), { error: "archive is not available" });
  });
}

for (const [protocol, size, outcome] of [[2, 327 * 1024 ** 2, "pull"], [1, 90 * 1024 ** 2, "push"], [1, 90 * 1024 ** 2 + 1, "error"]]) {
  test(`evacuation protocol ${protocol}, size ${size}: ${outcome}`, async () => {
    const f = evacuationPullFixture({ protocol, size });
    if (outcome === "error") {
      await assert.rejects(f.coordinator.importTarget(f.state), /requires target account evacuation protocol 2/);
      assert.equal(f.requests.length, 0);
      assert.equal(f.storage.values.get("account-lifecycle").operation.retryable, true);
      return;
    }
    const result = await f.coordinator.importTarget(f.state);
    assert.equal(result.operation.phase, "target_imported");
    const init = f.requests[0];
    if (outcome === "pull") {
      assert.equal(init.body, undefined);
      assert.equal(init.headers["X-Witself-Archive-Size"], String(size));
      assert.ok(/^cap_[0-9a-f]{64}$/.test(init.headers["X-Witself-Archive-Token"]));
      assert.equal(new URL(init.headers["X-Witself-Archive-URL"]).origin, "https://initiator.example");
    } else {
      assert.ok(init.body instanceof ReadableStream);
      assert.equal(init.headers["X-Witself-Archive-Token"], undefined);
    }
  });
}

test("heartbeat error persists retryable import failure and a cold retry mints a fresh capability", async () => {
  const f = evacuationPullFixture();
  const original = f.coordinator.fetchImpl;
  let firstToken;
  f.coordinator.fetchImpl = async (url, init) => {
    if (url.endsWith("/v1/version")) return original(url, init);
    firstToken = init.headers["X-Witself-Archive-Token"];
    return new Response('\n\n{"error":"could not import account"}');
  };
  await assert.rejects(f.coordinator.importTarget(f.state), /without an exact/);
  const failed = f.storage.values.get("account-lifecycle");
  assert.equal(failed.operation.phase, "route_retired");
  assert.equal(failed.operation.last_error, "archive import failed; retry required");
  assert.equal(failed.operation.retryable, true);
  const resumed = new DurableAccountLifecycle(context(f.storage), { ...f.coordinator.env, CP_PUBLIC_ORIGIN: "https://wrong-fallback.example" }, {
    fetch: original, validateArchive: f.coordinator.validateArchive, now: f.coordinator.now,
  });
  const result = await resumed.importTarget(failed);
  assert.equal(result.operation.phase, "target_imported");
  assert.ok(f.requests[0].headers["X-Witself-Archive-Token"] !== firstToken);
  assert.equal(new URL(f.requests[0].headers["X-Witself-Archive-URL"]).origin, "https://initiator.example");
});

test("evacuation export watchdog aborts idle bodies and allows progress beyond five minutes", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const watchdog = new ExportWatchdog();
  let source;
  const reader = watchdog.body(new ReadableStream({ start(controller) { source = controller; } })).getReader();
  for (let i = 0; i < 7; i++) {
    t.mock.timers.tick(60_000);
    source.enqueue(new Uint8Array([i]));
    await reader.read();
    assert.equal(watchdog.signal.aborted, false);
  }
  const pending = reader.read();
  t.mock.timers.tick(120_000);
  await assert.rejects(pending, /export_idle_timeout/);
  watchdog.close();
});

test("evacuation public route applies the IP limiter before capability consumption", async () => {
  const { register } = await import("node:module");
  register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
  const worker = (await import("../src/index.js")).default;
  for (const limiter of [undefined, { limit: () => ({}) }, { limit: () => { throw new Error("private"); } }, { limit: () => ({ success: false }) }]) {
    const f = evacuationPullFixture();
    f.env.PUBLIC_IP_LIMITER = limiter;
    f.env.ACCOUNT_LIFECYCLE.get = () => assert.fail("limiter failure reached capability storage");
    const response = await worker.fetch(pullRequest("bad"), f.env, {});
    assert.ok([404, 429].includes(response.status));
    assert.equal(response.headers.get("Cache-Control"), "no-store");
  }
  const f = evacuationPullFixture();
  let limited = 0;
  f.env.PUBLIC_IP_LIMITER.limit = ({ key }) => { assert.equal(key, "192.0.2.48"); limited++; return { success: true }; };
  const response = await worker.fetch(pullRequest("bad"), f.env, {});
  assert.equal(response.status, 404);
  assert.equal(limited, 1);
});

test("claim stores the initiating origin and the observed target protocol", async () => {
  const f = evacuationPullFixture();
  await f.coordinator.env.DIRECTORY.put(`cell:${SOURCE}`, JSON.stringify(cell(SOURCE, "https://source.example")));
  const initial = bootstrapLiveState({ account_id: ACCOUNT, route: sourceRoute() });
  const claimed = await f.coordinator.claim(initial, {
    action: "move", source_cell: SOURCE, target_cell: TARGET, request_origin: "https://request.example",
  });
  assert.equal(claimed.operation.archive_origin, "https://request.example");
  assert.equal(claimed.operation.target_preflight.protocol, 2);
  assert.equal(f.storage.values.get("account-lifecycle").operation.archive_origin, "https://request.example");
});

test("claim pins the configured public origin over the request origin and records null without either", async () => {
  for (const [configured, requested, want] of [
    ["https://self.example", "https://request.example", "https://self.example"],
    ["https://self.example", undefined, "https://self.example"],
    [undefined, undefined, null],
  ]) {
    const f = evacuationPullFixture();
    if (configured !== undefined) f.coordinator.env.CP_PUBLIC_ORIGIN = configured;
    await f.coordinator.env.DIRECTORY.put(`cell:${SOURCE}`, JSON.stringify(cell(SOURCE, "https://source.example")));
    const initial = bootstrapLiveState({ account_id: ACCOUNT, route: sourceRoute() });
    const claimed = await f.coordinator.claim(initial, {
      action: "move", source_cell: SOURCE, target_cell: TARGET,
      ...(requested === undefined ? {} : { request_origin: requested }),
    });
    assert.equal(claimed.operation.archive_origin, want);
  }
});

for (const mode of ["idle", "progress", "overall"]) {
  test(`exportArchive uses the shared watchdog: ${mode}`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let state = claimOperation(bootstrapLiveState({ account_id: ACCOUNT, route: sourceRoute() }), {
      kind: "evacuate", operation_id: OPERATION_ID, evacuation_id: OPERATION_ID, source_cell: SOURCE,
      archive: { archive_id: "archive_watchdog", object: `archives/${ACCOUNT}/watchdog.tar.gz` },
    });
    state = acknowledgeStep(state, { operation_id: OPERATION_ID, from_phase: "claimed", to_phase: "source_suspended" });
    const directory = new KV({ [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example") });
    let source, signal, aborted = false;
    const coordinator = runtime({ directory, bucket: new Bucket(), fetch: async (url, init) => {
      if (url.endsWith("/v1/version")) return protocolResponse();
      signal = init.signal;
      return new Response(new ReadableStream({ start(controller) { source = controller; } }));
    } });
    coordinator.readPlacementPolicy = async () => null;
    coordinator.abortPreArchive = async () => { aborted = true; };
    coordinator.streamArchive = async (_bucket, _key, body) => {
      let size = 0;
      for await (const chunk of body) size += chunk.byteLength;
      _bucket.values.set(_key, new Uint8Array(size));
      return size;
    };
    const pending = coordinator.exportArchive(state);
    const failure = mode !== "progress" ? assert.rejects(pending, mode === "idle" ? /export_idle_timeout/ : /export_overall_timeout/) : null;
    await new Promise(setImmediate);
    if (mode === "idle") {
      t.mock.timers.tick(120_000);
    } else {
      for (let i = 0; i < (mode === "overall" ? 60 : 7); i++) {
        t.mock.timers.tick(60_000);
        if (!signal.aborted) source.enqueue(new Uint8Array([1]));
        await new Promise(setImmediate);
      }
      if (mode === "progress") {
        assert.equal(signal.aborted, false);
        source.close();
        assert.equal((await pending).operation.phase, "archive_committed");
      }
    }
    if (failure) { await failure; assert.equal(aborted, true); assert.equal(signal.aborted, true); }
  });
}

test("pull origin uses configured fallback only when the operation has no origin", async () => {
  for (const origin of [undefined, "http://unsafe.example", "https://bad.example/path"]) {
    const f = evacuationPullFixture();
    f.state.operation.archive_origin = origin;
    f.coordinator.env.CP_PUBLIC_ORIGIN = "https://fallback.example";
    if (origin === undefined) {
      await f.coordinator.importTarget(f.state);
      assert.equal(new URL(f.requests[0].headers["X-Witself-Archive-URL"]).origin, "https://fallback.example");
    } else {
      await assert.rejects(f.coordinator.importTarget(f.state), /origin is not configured/);
      assert.equal(f.requests.length, 0);
      assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
    }
  }
});

function asyncImportFixture() {
  const f = evacuationPullFixture({ protocol: 3 });
  let now = f.coordinator.now().getTime();
  f.coordinator.now = () => new Date(now);
  f.advance = (ms) => { now += ms; };
  f.sleeps = [];
  f.coordinator.sleep = async (ms) => { f.sleeps.push(ms); f.advance(ms); };
  f.calls = []; f.validations = 0; f.renewals = 0; f.protocol = 3; f.schema = 98;
  f.startBody = { import_job: { state: "running", started: true } };
  f.pollBody = { import_job: { state: "running" } };
  f.coordinator.validateArchive = async () => { f.validations++; return { manifest: { schema_version: 98 } }; };
  f.coordinator.renewTargetReservation = async () => { f.renewals++; };
  f.coordinator.fetchImpl = async (url, init) => {
    if (url.endsWith("/v1/version")) return Response.json({ account_evacuation_protocol: f.protocol, store_schema_version: f.schema });
    f.calls.push({ url, init });
    if (url.endsWith(":start-import-evacuation")) {
      if (f.startThrows) throw new Error("private transport URL");
      return Response.json(f.startBody, { status: f.startStatus ?? 200, headers: f.startHeaders });
    }
    assert.ok(url.endsWith(":import-evacuation-status"));
    assert.deepEqual(Object.keys(init.headers).sort(), ["Authorization", "X-Witself-Evacuation-ID"]);
    return Response.json(f.pollBody, { status: f.pollStatus ?? 200 });
  };
  f.latest = () => f.storage.values.get("account-lifecycle");
  f.tick = (budget = 0) => f.coordinator.importTarget(f.latest(), budget);
  f.ack = { account_id: ACCOUNT, evacuation_id: OPERATION_ID, evacuation_role: "target", status: "suspended" };
  return f;
}

for (const protocol of [0, 1, 2, 3, 4]) {
  test(`async import protocol attestation ${protocol}`, async () => {
    const f = asyncImportFixture(); f.protocol = protocol;
    if ([0, 4].includes(protocol)) await assert.rejects(f.coordinator.cell(TARGET, { evacuationProtocol: true }), /does not attest account evacuation protocol 1, 2 or 3/);
    else { const cell = await f.coordinator.cell(TARGET, { evacuationProtocol: true }); assert.equal(cell.account_evacuation_protocol, protocol); assert.equal(cell.store_schema_version, 98); }
  });
}
for (const completed of [false, true]) {
  test(`async import inline acknowledgement completed=${completed}`, async () => {
    const f = asyncImportFixture(); f.startBody = { ...f.ack, ...(completed ? { evacuation_completed: true, already_imported: true, status: "active" } : {}) };
    const result = await f.tick();
    assert.equal(result.operation.phase, completed ? "target_resumed" : "target_imported");
    assert.equal(result.operation.import_job, null); assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0].init.headers["X-Witself-Import-Wait"], "20");
    assert.equal(f.validations, 1);
  });
}
test("async running null retry_at only polls and status receipt clears metadata", async () => {
  const f = asyncImportFixture(); assert.deepEqual(await f.tick(), { pending: true });
  let job = f.latest().operation.import_job; assert.equal(job.attempts, 1); assert.equal(job.retry_at, null);
  const token = f.calls[0].init.headers["X-Witself-Archive-Token"];
  await f.coordinator.alarm();
  assert.equal(f.calls.length, 2); assert.ok(f.calls[1].url.endsWith(":import-evacuation-status")); assert.ok(f.renewals >= 1);
  assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 1);
  f.pollBody = f.ack; const result = await f.tick(); assert.equal(result.operation.import_job, null);
  assert.ok(!JSON.stringify(result).includes(token));
});
test("async absent backoff, operator re-drive, fresh capability and ETag verification cache", async () => {
  const f = asyncImportFixture(); await f.tick(); const token = f.calls[0].init.headers["X-Witself-Archive-Token"];
  f.pollBody = { import_job: { state: "absent" } }; await f.tick();
  const job = f.latest().operation.import_job; assert.equal(job.attempts, 1); assert.equal(job.retry_at, f.coordinator.now().getTime() + 60_000);
  await f.tick(); assert.equal(f.calls.length, 2);
  f.startBody = f.ack; await f.tick(1);
  assert.equal(f.calls.length, 3); assert.ok(f.calls[2].init.headers["X-Witself-Archive-Token"] !== token); assert.equal(f.validations, 1);
  const g = asyncImportFixture(); await g.tick(); g.pollBody = { import_job: { state: "absent" } }; await g.tick();
  g.bucket.head = async () => ({ etag: "changed", size: 7 }); g.advance(60_000); g.startBody = g.ack; await g.tick(); assert.equal(g.validations, 2);
});
test("async 503 capacity leaves attempts unchanged and honors Retry-After", async () => {
  const f = asyncImportFixture(); f.startStatus = 503; f.startHeaders = { "Retry-After": "120" };
  await f.tick(); const job = f.latest().operation.import_job;
  assert.equal(job.attempts, 0); assert.equal(job.started_at, null); assert.equal(job.retry_at, f.coordinator.now().getTime() + 120_000);
  assert.equal(f.latest().operation.last_error, "import capacity exhausted; retry scheduled"); await f.tick(); assert.equal(f.calls.length, 1);
});
for (const message of ["account exists under a different evacuation", "archive schema is newer than this cell — upgrade the cell first", "invalid or corrupt archive", "archive download failed", "could not import account", "private token and URL"]) {
  test(`async start classifies value-free error ${message.split(" ")[0]}`, async () => {
    const f = asyncImportFixture(); f.startBody = { error: message }; await f.tick();
    const operation = f.latest().operation;
    const permanent = ["account exists under a different evacuation", "archive schema is newer than this cell — upgrade the cell first", "invalid or corrupt archive"].includes(message);
    assert.equal(operation.retryable, !permanent); assert.equal(operation.last_error, permanent ? message : "archive import failed; retry required");
    assert.equal(operation.import_job.retry_at, f.coordinator.now().getTime() + (permanent ? 6 * 3600_000 : 60_000));
  });
}
test("async status errors and ambiguous starts preserve polling ownership", async () => {
  for (const timeout of [false, true]) {
    const f = asyncImportFixture(); f.startThrows = timeout;
    if (timeout) await assert.rejects(f.tick()); else await f.tick();
    assert.equal(f.latest().operation.import_job.retry_at, null);
    f.pollStatus = 409; await assert.rejects(f.tick()); assert.equal(f.latest().operation.retryable, false);
    await assert.rejects(f.tick()); assert.equal(f.calls.filter((c) => c.url.endsWith(":start-import-evacuation")).length, 1);
    assert.equal(f.latest().operation.last_error, "archive import failed; retry required");
  }
});
test("async exhaustion and age limit preserve eventual receipt polling", async () => {
  const f = asyncImportFixture(); await f.tick(); let state = f.latest(); state.operation.import_job.attempts = 6;
  await f.storage.put("account-lifecycle", state); f.pollBody = { import_job: { state: "absent" } }; await f.tick();
  assert.equal(f.latest().operation.last_error, "import job exhausted its attempts; operator attention required");
  assert.equal(f.latest().operation.import_job.retry_at, f.coordinator.now().getTime() + 6 * 3600_000);
  const g = asyncImportFixture(); await g.tick(); g.advance(3.5 * 3600_000 + 1); await g.tick();
  assert.equal(g.latest().operation.last_error, "import job exceeded its age limit; operator attention required"); assert.equal(g.latest().operation.retryable, false);
  g.pollBody = g.ack; assert.equal((await g.tick()).operation.phase, "target_imported");
});
test("async job survives mixed protocol roll without streaming fallback", async () => {
  const f = asyncImportFixture(); await f.tick(); const job = structuredClone(f.latest().operation.import_job);
  f.protocol = 2; await f.tick(); assert.equal(f.calls.length, 1); assert.deepEqual(f.latest().operation.import_job, job);
  assert.equal(f.latest().operation.last_error, "target cell attests protocol 2 while an import job is in flight; waiting for the roll");
  f.protocol = 3; await f.tick(); assert.ok(f.calls[1].url.endsWith(":import-evacuation-status"));
});
test("async inline budget uses injected clock and sleep and renews between polls", async () => {
  const f = asyncImportFixture(); await f.tick(25_000);
  assert.equal(f.calls.length, 3); assert.deepEqual(f.sleeps, [10_000, 10_000, 5_000]); assert.equal(f.renewals, 3);
  const g = asyncImportFixture(); const result = await g.coordinator.runMove(g.latest(), { importBudgetMs: 0 });
  assert.equal(result.import_pending, true); assert.equal(result.import_job.attempts, 1); assert.equal(result.retryable, true); assert.ok(g.storage.alarm);
});
test("async schema precheck prevents mint and metadata survives state validation", async () => {
  const f = asyncImportFixture(); f.schema = 97; await assert.rejects(f.tick(), /target cell schema is older/);
  assert.equal(f.calls.length, 0); assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
  const state = validateLifecycleState(f.latest()); assert.equal(state.operation.import_job.attempts, 0); assert.equal(state.operation.retryable, false);
  assert.equal(state.operation.import_job.retry_at, f.coordinator.now().getTime() + 6 * 3600_000);
});
test("async inline budget input is a nonnegative safe integer", () => {
  const f = asyncImportFixture();
  for (const value of [-1, 0.1, "1", null, Number.MAX_SAFE_INTEGER + 1]) assert.equal(f.coordinator.validInput({ account_id: ACCOUNT, action: "restore", cell_name: TARGET, inline_budget_ms: value }), false);
  for (const value of [undefined, 0, 120_000]) assert.equal(f.coordinator.validInput({ account_id: ACCOUNT, action: "restore", cell_name: TARGET, inline_budget_ms: value }), true);
});

test("async start records ownership before transport and validates before minting", async () => {
  const f = asyncImportFixture();
  f.coordinator.validateArchive = async () => {
    assert.equal((await f.storage.list({ prefix: "archive-capability:" })).size, 0);
    return { manifest: { schema_version: 98 } };
  };
  const fetch = f.coordinator.fetchImpl;
  f.coordinator.fetchImpl = async (url, init) => {
    if (url.endsWith(":start-import-evacuation")) {
      assert.equal(f.latest().operation.import_job.attempts, 1);
      assert.equal(f.latest().operation.import_job.retry_at, null);
    }
    return fetch(url, init);
  };
  await f.tick();
});

test("async pending rebalance accounting survives route retirement", async () => {
  const f = asyncImportFixture();
  const result = await f.coordinator.runMove(f.latest(), { importBudgetMs: 0 });
  assert.equal(result.import_pending, true);
  assert.equal(result.epoch, f.latest().epoch);
  assert.equal(result.phase, "route_retired");
  assert.equal(result.evacuation_id, OPERATION_ID);
});

test("async invalid acknowledgement never echoes remote status text", async () => {
  const f = asyncImportFixture();
  f.startBody = { ...f.ack, status: "https://private.invalid/archive?private-marker" };
  await assert.rejects(f.tick(), { message: "archive import failed; retry required" });
  assert.equal(f.latest().operation.last_error, "archive import failed; retry required");
  assert.equal(f.latest().operation.import_job.retry_at, null);
});

// A seeded, write-frozen source drives the real alarm/run state machine.
function splitExportFixture({ job, seeded = true } = {}) {
  const storage = new Storage();
  const bucket = new Bucket();
  const directory = new KV({
    [`acct:${ACCOUNT}`]: sourceRoute(),
    [`cell:${SOURCE}`]: cell(SOURCE, "https://source.example", false),
  });
  const policy = { allowed_clouds: ["aws", "civo"] };
  const object = `archives/${ACCOUNT}/${OPERATION_ID}.tar.gz`;
  let state = claimOperation(bootstrapLiveState({ account_id: ACCOUNT, route: sourceRoute() }), {
    kind: "evacuate", operation_id: OPERATION_ID, evacuation_id: OPERATION_ID, source_cell: SOURCE,
    archive: { archive_id: OPERATION_ID, object },
  });
  state = acknowledgeStep(state, { operation_id: OPERATION_ID, from_phase: "claimed", to_phase: "source_suspended" });
  state.operation.source_registration_id = `reg-${SOURCE}`;
  state.operation.request_epoch = 0;
  if (job !== undefined) state.operation.export_job = structuredClone(job);
  if (seeded) storage.values.set("account-lifecycle", structuredClone(state));
  const calls = { placement: 0, export: 0, verify: 0, head: 0, abort: 0 };
  const saved = () => storage.values.get("account-lifecycle");
  const abortReceipt = { account_id: ACCOUNT, evacuation_id: OPERATION_ID, evacuation_role: "source", status: "active", aborted: true };
  const coordinator = runtime({ storage, bucket, directory, fetch: async (url, init) => {
    if (url.endsWith("/v1/version")) return protocolResponse();
    if (url.endsWith(":begin-evacuation")) return Response.json({ ...abortReceipt, status: "suspended", aborted: false });
    if (url.endsWith("/placement-policy")) {
      calls.placement++;
      assert.equal(saved().operation.export_job.stream_attempts, (job?.stream_attempts ?? 0) + 1);
      assert.equal(saved().operation.export_job.streamed, null);
      return Response.json({ account_id: ACCOUNT, placement_policy: policy });
    }
    if (url.endsWith(":export-evacuation")) {
      calls.export++;
      assert.equal(saved().operation.export_job.stream_attempts, (job?.stream_attempts ?? 0) + 1);
      return new Response(new Uint8Array([1, 2, 3]));
    }
    if (url.endsWith(":abort-evacuation")) {
      calls.abort++;
      assert.equal(saved().operation.phase, "abort_requested");
      assert.equal(JSON.parse(init.body).evacuation_id, OPERATION_ID);
      return Response.json(abortReceipt);
    }
    if (url.endsWith(":finalize-evacuation")) return Response.json({
      account_id: ACCOUNT, evacuation_id: OPERATION_ID, source_status: "suspended",
      finalized: true, already_finalized: false, finalized_at: "2026-07-25T12:00:00.000Z",
    });
    throw new Error("unexpected export fixture request");
  } });
  const verify = coordinator.verifyArchive;
  coordinator.verifyArchive = async (...args) => { calls.verify++; return verify(...args); };
  const head = bucket.head.bind(bucket);
  bucket.head = async (...args) => { calls.head++; return head(...args); };
  const states = [];
  const put = storage.put.bind(storage);
  storage.put = async (key, value) => { states.push(structuredClone(value)); return put(key, value); };
  return { coordinator, storage, bucket, directory, state, object, policy, calls, saved, states, abortReceipt };
}

function streamedExportJob(overrides = {}) {
  return { stream_attempts: 1, verify_attempts: 0, stream_started_at: 1000,
    placement_policy: { allowed_clouds: ["aws", "civo"] },
    streamed: { etag: "fixture-etag", size: 3, streamed_at: 2000, stream_ms: 1000 }, ...overrides };
}

function seedStreamedObject(f) { f.bucket.values.set(f.object, new Uint8Array([1, 2, 3])); }

function assertExportAborted(f) {
  assert.equal(f.saved().operation, null);
  assert.equal(f.saved().last_completed.outcome, "aborted");
  assert.deepEqual(f.saved().location.route, sourceRoute());
  assert.deepEqual(f.states.find(s => s.operation?.phase === "abort_cleanup_pending").operation.abort_receipt, f.abortReceipt);
  assert.equal(f.directory.value(`archived:${ACCOUNT}`), null);
  assert.equal(f.bucket.values.has(f.object), false);
  assert.deepEqual(f.bucket.deleted, [f.object]);
}

test("export split inline commits only after verification and exactly two identity heads", async () => {
  const f = splitExportFixture();
  const state = await f.coordinator.exportArchive(f.state);
  assert.equal(state.operation.phase, "archive_committed");
  assert.equal(state.operation.export_job, null);
  assert.deepEqual(f.calls, { placement: 1, export: 1, verify: 1, head: 2, abort: 0 });
  assert.deepEqual(state.operation.placement_policy, f.policy);
  const revisions = f.states.map(s => s.revision);
  assert.deepEqual(revisions, [f.state.revision + 1, f.state.revision + 2, f.state.revision + 3, f.state.revision + 5]);
});

test("export split run remains inline from claim through completion", async () => {
  const f = splitExportFixture({ seeded: false });
  const response = await f.coordinator.fetch(request("evacuate", { cell_name: SOURCE }));
  assert.equal(response.status, 200);
  assert.equal("export_pending" in (await response.json()).result, false);
  assert.equal(f.saved().operation, null);
  assert.equal(f.calls.export, 1);
  assert.equal(f.calls.verify, 1);
});

test("export split alarms persist stream then verify with identical inline archive authority", async () => {
  const inline = splitExportFixture();
  await inline.coordinator.runMove(inline.state);
  const f = splitExportFixture();
  await f.coordinator.alarm();
  assert.equal(f.saved().operation.phase, "source_suspended");
  assert.equal(f.saved().operation.export_job.streamed.size, 3);
  assert.equal(f.storage.alarm, f.coordinator.now().getTime());
  assert.deepEqual(f.calls, { placement: 1, export: 1, verify: 0, head: 1, abort: 0 });
  await f.coordinator.alarm();
  assert.deepEqual(f.calls, { placement: 1, export: 1, verify: 1, head: 3, abort: 0 });
  assert.equal(f.saved().operation, null);
  assert.deepEqual(f.directory.value(`archived:${ACCOUNT}`), inline.directory.value(`archived:${ACCOUNT}`));
  assert.deepEqual(f.directory.value(`archived:${ACCOUNT}`).placement_policy, f.policy);
});

for (const orphan of [false, true]) {
  test(`export split cut stream counts next attempt and never adopts unrecorded object: ${orphan}`, async () => {
    const f = splitExportFixture({ job: streamedExportJob({ streamed: null }) });
    if (orphan) f.bucket.values.set(f.object, new Uint8Array([9]));
    await f.coordinator.alarm();
    assert.equal(f.saved().operation.export_job.stream_attempts, 2);
    assert.equal(f.calls.export, 1);
    assert.equal(f.calls.verify, 0);
    assert.equal(f.calls.head, 1);
    assert.deepEqual([...f.bucket.values.get(f.object)], [1, 2, 3]);
  });
}

for (const stage of ["stream", "verify"]) {
  test(`export split exhausted ${stage} budget aborts without another attempt`, async () => {
    const f = splitExportFixture({ job: streamedExportJob(stage === "stream"
      ? { stream_attempts: 3, streamed: null } : { verify_attempts: 3 }) });
    seedStreamedObject(f);
    const response = await f.coordinator.fetch(request("evacuate", { cell_name: SOURCE }));
    assert.equal(response.status, 504);
    assert.equal((await response.json()).error, stage === "stream"
      ? "export stream did not complete within its attempt budget"
      : "archive verification did not complete within its attempt budget");
    assert.equal(f.calls.export, 0);
    assert.equal(f.calls.placement, 0);
    assert.equal(f.calls.verify, 0);
    assertExportAborted(f);
  });
}

test("export split cut verification resumes with durable attempt count before verify", async () => {
  const f = splitExportFixture({ job: streamedExportJob({ verify_attempts: 1 }) });
  seedStreamedObject(f);
  const verify = f.coordinator.verifyArchive;
  f.coordinator.verifyArchive = async (...args) => {
    assert.equal(f.saved().operation.export_job.verify_attempts, 2);
    return verify(...args);
  };
  await f.coordinator.alarm();
  assert.equal(f.saved().operation, null);
  assert.deepEqual(f.calls, { placement: 0, export: 0, verify: 1, head: 2, abort: 0 });
});

for (const change of [{ etag: "changed" }, { size: 4 }]) {
  test(`export split rejects changed identity before verification: ${Object.keys(change)[0]}`, async () => {
    const f = splitExportFixture({ job: streamedExportJob() });
    seedStreamedObject(f);
    f.bucket.head = async () => ({ etag: "fixture-etag", size: 3, ...change });
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "exported archive identity changed before verification" });
    assert.equal(f.calls.verify, 0);
    assertExportAborted(f);
  });
}

for (const change of [null, { etag: "changed", size: 3 }, { etag: "fixture-etag", size: 4 }]) {
  test(`export split rejects changed identity during verification: ${JSON.stringify(change)}`, async () => {
    const f = splitExportFixture({ job: streamedExportJob() });
    seedStreamedObject(f);
    let heads = 0;
    f.bucket.head = async () => ++heads === 1 ? { etag: "fixture-etag", size: 3 } : change;
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "exported archive identity changed during verification" });
    assert.equal(f.calls.verify, 1);
    assertExportAborted(f);
  });
}

test("export split missing recorded object re-streams without resetting verify budget", async () => {
  const f = splitExportFixture({ job: streamedExportJob({ verify_attempts: 2 }) });
  await f.coordinator.alarm();
  assert.equal(f.saved().operation.export_job.stream_attempts, 2);
  assert.equal(f.saved().operation.export_job.verify_attempts, 2);
  assert.equal(f.calls.export, 1);
  assert.equal(f.calls.verify, 0);
  assert.equal(f.calls.head, 2);
});

for (const head of [null, { size: 4, etag: "fixture-etag" }, { size: 3, etag: "" }, { size: 3, etag: 12 }]) {
  test(`export split refuses unprovable streamed identity: ${JSON.stringify(head)}`, async () => {
    const f = splitExportFixture();
    f.bucket.head = async () => head;
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "exported archive identity is not provable" });
    assert.equal(f.calls.verify, 0);
    assertExportAborted(f);
  });
}

test("export split run between alarm ticks finishes without re-export", async () => {
  const f = splitExportFixture();
  await f.coordinator.alarm();
  const response = await f.coordinator.fetch(request("evacuate", { cell_name: SOURCE }));
  assert.equal(response.status, 200);
  assert.equal("export_pending" in (await response.json()).result, false);
  assert.equal(f.saved().operation, null);
  assert.equal(f.calls.export, 1);
  assert.equal(f.calls.placement, 1);
  assert.equal(f.calls.verify, 1);
});

test("export split lock wait times out then retries only abort until source releases", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const f = splitExportFixture({ job: streamedExportJob({ streamed: null }) });
  const fetch = f.coordinator.fetchImpl;
  let exports = 0, aborts = 0;
  f.coordinator.fetchImpl = async (url, init) => {
    if (url.endsWith(":export-evacuation")) { exports++; return new Promise(() => {}); }
    if (url.endsWith(":abort-evacuation") && ++aborts === 1) throw new Error("source row still locked");
    return fetch(url, init);
  };
  const pending = f.coordinator.alarm();
  await new Promise(setImmediate);
  t.mock.timers.tick(120_000);
  await pending;
  assert.equal(f.saved().operation.phase, "abort_requested");
  assert.equal(f.storage.alarm, f.coordinator.now().getTime() + 60_000);
  await f.coordinator.alarm();
  assertExportAborted(f);
  assert.equal(exports, 1);
  assert.equal(aborts, 2);
});

for (const timeout of [false, true]) {
  test(`export split verify-only alarm disarms idle but preserves overall timeout: ${timeout}`, async t => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const f = splitExportFixture({ job: streamedExportJob() });
    seedStreamedObject(f);
    const verify = f.coordinator.verifyArchive;
    let finish;
    f.coordinator.verifyArchive = async (...args) => {
      await new Promise(resolve => { finish = resolve; });
      return verify(...args);
    };
    const logs = [];
    t.mock.method(console, "log", line => logs.push(line));
    const pending = f.coordinator.alarm();
    await new Promise(setImmediate);
    assert.equal(typeof finish, "function");
    t.mock.timers.tick(timeout ? 60 * 60_000 : 121_000);
    if (!timeout) finish();
    await pending;
    if (timeout) {
      assertExportAborted(f);
      assert.ok(logs.some(line => line.endsWith("failed: export_overall_timeout")));
    } else {
      assert.equal(f.saved().operation, null);
      assert.equal(f.saved().last_completed.outcome, "completed");
      assert.equal(f.calls.verify, 1);
    }
    assert.equal(f.calls.export, 0);
  });
}

for (const stage of ["stream", "verify"]) {
  test(`export split stale ${stage} loses ownership without writes or abort`, async () => {
    const f = splitExportFixture();
    let staleState;
    const method = stage === "stream" ? "streamArchive" : "verifyArchive";
    const original = f.coordinator[method];
    f.coordinator[method] = async (...args) => {
      const result = await original(...args);
      staleState = structuredClone(f.saved());
      if (stage === "stream") staleState.operation.export_job.stream_attempts++;
      else staleState.revision++;
      await f.storage.put("account-lifecycle", staleState);
      return result;
    };
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "export attempt lost durable ownership", status: 409 });
    assert.deepEqual(f.saved(), staleState);
    assert.equal(f.calls.abort, 0);
    assert.equal(f.bucket.deleted.length, 0);
    assert.equal(f.saved().operation.phase, "source_suspended");
    if (stage === "stream") assert.equal(f.saved().operation.export_job.streamed, null);
  });
}

for (const field of ["operation_id", "phase"]) {
  test(`export split ownership guard compares ${field} at an unchanged revision`, async () => {
    const f = splitExportFixture();
    const original = f.coordinator.streamArchive;
    const load = f.coordinator.loadState.bind(f.coordinator);
    let durable;
    f.coordinator.streamArchive = async (...args) => {
      const result = await original(...args);
      durable = structuredClone(f.saved());
      // Same revision and counters; only the compared field differs.
      f.coordinator.loadState = async () => {
        const foreign = structuredClone(await load());
        if (field === "phase") foreign.operation.phase = "abort_requested";
        else foreign.operation.operation_id = `${foreign.operation.operation_id}-other`;
        return foreign;
      };
      return result;
    };
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "export attempt lost durable ownership", status: 409 });
    assert.deepEqual(f.saved(), durable);
    assert.equal(f.saved().operation.export_job.streamed, null);
    assert.equal(f.calls.abort, 0);
    assert.equal(f.bucket.deleted.length, 0);
  });
}

test("export split abort acts on the reloaded durable state, not the invocation's stale copy", async () => {
  const f = splitExportFixture();
  const failure = new Error("stream failed after a newer save");
  let newer;
  f.coordinator.streamArchive = async () => {
    newer = structuredClone(f.saved());
    newer.revision++;
    await f.storage.put("account-lifecycle", newer);
    throw failure;
  };
  let abortedWith;
  const abort = f.coordinator.abortPreArchive.bind(f.coordinator);
  f.coordinator.abortPreArchive = async (state, error) => {
    abortedWith = structuredClone(state);
    return abort(state, error);
  };
  await assert.rejects(f.coordinator.exportArchive(f.state), error => error === failure);
  assert.equal(abortedWith.revision, newer.revision);
  assert.equal(f.calls.abort, 1);
});

test("export split exhausted verify budget with a missing object aborts without re-streaming", async () => {
  const f = splitExportFixture({ job: streamedExportJob({ verify_attempts: 3 }) });
  await assert.rejects(f.coordinator.exportArchive(f.state), { message: "archive verification did not complete within its attempt budget" });
  assert.equal(f.calls.export, 0);
  assert.equal(f.calls.verify, 0);
  assertExportAborted(f);
});

test("export split stream error cannot abort another durable phase", async () => {
  const f = splitExportFixture();
  const original = new Error("stream failed after another owner advanced");
  f.coordinator.streamArchive = async () => {
    seedStreamedObject(f);
    const newer = structuredClone(f.saved());
    newer.operation.phase = "abort_requested";
    newer.revision++;
    await f.storage.put("account-lifecycle", newer);
    throw original;
  };
  await assert.rejects(f.coordinator.exportArchive(f.state), error => error === original);
  assert.equal(f.saved().operation.phase, "abort_requested");
  assert.equal(f.calls.abort, 0);
  assert.equal(f.bucket.values.has(f.object), true);
});

test("export split reload failure preserves original error without abort", async () => {
  const f = splitExportFixture();
  const original = new Error("stream failed");
  f.coordinator.streamArchive = async () => {
    f.coordinator.loadState = async () => { throw new Error("reload failed"); };
    throw original;
  };
  await assert.rejects(f.coordinator.exportArchive(f.state), error => error === original);
  assert.equal(f.calls.abort, 0);
  assert.equal(f.bucket.deleted.length, 0);
});

test("export split pending projection reloads durable counters and omits identity", async () => {
  const f = splitExportFixture();
  f.coordinator.scheduleWakeupNow = async () => {
    const newer = structuredClone(f.saved());
    newer.operation.export_job.verify_attempts = 2;
    newer.revision++;
    await f.storage.put("account-lifecycle", newer);
  };
  const result = await f.coordinator.runMove(f.state, { splitExport: true });
  assert.deepEqual(result, { export_pending: true, operation_id: OPERATION_ID,
    evacuation_id: OPERATION_ID, epoch: f.saved().epoch, target_cell: null,
    phase: "source_suspended", export_job: { stream_attempts: 1, verify_attempts: 2,
      size: 3, streamed_at: f.coordinator.now().getTime() } });
  assert.equal(JSON.stringify(result).includes("etag"), false);
});

for (const job of [false, [], "bad", {}, streamedExportJob({ stream_attempts: -1 }),
  streamedExportJob({ verify_attempts: 0.5 }), streamedExportJob({ stream_attempts: Number.MAX_SAFE_INTEGER + 1 }),
  streamedExportJob({ streamed: false }), streamedExportJob({ streamed: {} }),
  streamedExportJob({ streamed: { etag: "", size: 3 } }), streamedExportJob({ streamed: { etag: "x", size: 0 } })]) {
  test(`export split malformed metadata aborts with fixed text: ${JSON.stringify(job)}`, async () => {
    const f = splitExportFixture({ job });
    await assert.rejects(f.coordinator.exportArchive(f.state), { message: "export job metadata is invalid", status: 500 });
    assert.equal(f.calls.export, 0);
    assertExportAborted(f);
  });
}

test("export split logs exact bounded timings only after successful saves", async t => {
  const logs = [];
  t.mock.method(console, "log", line => logs.push(line));
  const f = splitExportFixture();
  let now = 1000;
  f.coordinator.now = () => new Date(now += 100);
  await f.coordinator.exportArchive(f.state);
  assert.deepEqual(logs, [
    `account lifecycle export for ${ACCOUNT} streamed attempt=1 bytes=3 ms=200`,
    `account lifecycle export for ${ACCOUNT} verified attempt=1 bytes=3 ms=100`,
  ]);
  for (const stage of ["stream", "verify"]) {
    const exhausted = splitExportFixture({ job: streamedExportJob(stage === "stream"
      ? { stream_attempts: 3, streamed: null } : { verify_attempts: 3 }) });
    seedStreamedObject(exhausted);
    await assert.rejects(exhausted.coordinator.exportArchive(exhausted.state));
  }
  assert.deepEqual(logs.slice(2), [
    `account lifecycle export for ${ACCOUNT} aborted reason=stream_attempts_exhausted`,
    `account lifecycle export for ${ACCOUNT} aborted reason=verify_attempts_exhausted`,
  ]);
  for (const line of logs) {
    assert.equal(line.includes(f.object), false);
    assert.doesNotMatch(line, /fixture-etag|https:|Bearer|token|archives\//);
  }
  const lostCommit = splitExportFixture();
  lostCommit.storage.put = async (key, state) => {
    if (state.operation?.phase === "archive_committed") throw new Error("commit save failed");
    lostCommit.storage.values.set(key, structuredClone(state));
  };
  await assert.rejects(lostCommit.coordinator.exportArchive(lostCommit.state), /commit save failed/);
  assert.equal(logs.filter(line => line.includes(" verified ")).length, 1);
  assert.equal(lostCommit.calls.abort, 0);
});

test("export split metadata remains compatible with existing lifecycle state phases", () => {
  const f = splitExportFixture({ job: streamedExportJob() });
  for (const phase of ["source_suspended", "abort_requested", "archive_committed"]) {
    const state = structuredClone(f.state);
    state.operation.phase = phase;
    assert.doesNotThrow(() => validateLifecycleState(state));
  }
});

for (const error of [new ArchiveIntegrityError("fixture verification failed"), new Error("fixture R2 read failed")]) {
  test(`export split verification error aborts immediately: ${error.constructor.name}`, async () => {
    const f = splitExportFixture({ job: streamedExportJob() });
    seedStreamedObject(f);
    f.coordinator.verifyArchive = async () => { f.calls.verify++; throw error; };
    await assert.rejects(f.coordinator.exportArchive(f.state), caught => caught === error);
    assert.equal(f.calls.verify, 1);
    assert.equal(f.calls.export, 0);
    assertExportAborted(f);
  });
}


test("export split null metadata starts at attempt one", async () => {
  const f = splitExportFixture({ job: null });
  await f.coordinator.alarm();
  assert.equal(f.saved().operation.export_job.stream_attempts, 1);
  assert.equal(f.saved().operation.export_job.verify_attempts, 0);
  assert.equal(f.calls.verify, 0);
});

test("export split resumed stale owner cannot increment verification attempts", async () => {
  const f = splitExportFixture({ job: streamedExportJob() });
  seedStreamedObject(f);
  const newer = structuredClone(f.saved());
  newer.operation.export_job.verify_attempts = 1;
  await f.storage.put("account-lifecycle", newer);
  const writes = f.states.length;
  await assert.rejects(f.coordinator.exportArchive(f.state), { message: "export attempt lost durable ownership" });
  assert.equal(f.states.length, writes);
  assert.deepEqual(f.saved(), newer);
  assert.equal(f.calls.verify, 0);
  assert.equal(f.calls.abort, 0);
});

test("export split disarmed watchdog ignores later progress", t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const watchdog = new ExportWatchdog();
  try {
    watchdog.disarmIdle();
    watchdog.progress();
    t.mock.timers.tick(121_000);
    assert.equal(watchdog.signal.aborted, false);
    t.mock.timers.tick(60 * 60_000 - 121_000);
    assert.equal(watchdog.signal.reason.message, "export_overall_timeout");
  } finally {
    watchdog.close();
  }
});
