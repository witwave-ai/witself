import assert from "node:assert/strict";
import test from "node:test";

import {
  DurableTargetCellCoordinator,
} from "../src/target-cell-coordinator.mjs";

const CELL = "civo-phx1";
const ACCOUNT = "acct_cell_guard";
const OPERATION = "11111111-1111-4111-8111-111111111111";
const EVACUATION = "22222222-2222-4222-8222-222222222222";
const REGISTRATION = "registration-a";
const NEXT_OPERATION = "66666666-6666-4666-8666-666666666666";
const REENTRY_OPERATION = "77777777-7777-4777-8777-777777777777";
const REENTRY_EVACUATION = "88888888-8888-4888-8888-888888888888";

class Storage {
  constructor() {
    this.values = new Map();
    this.alarm = null;
    this.pauseReservationPut = null;
  }

  async get(key) {
    return this.values.get(key);
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
    if (
      key.startsWith("reservation:") &&
      this.pauseReservationPut
    ) {
      const pause = this.pauseReservationPut;
      this.pauseReservationPut = null;
      await pause;
    }
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
    this.failCellDeleteOnce = false;
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
    if (key.startsWith("cell:") && this.failCellDeleteOnce) {
      this.failCellDeleteOnce = false;
      throw new Error("simulated ambiguous KV delete failure");
    }
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

function cell(accepting = true, registration = REGISTRATION) {
  return {
    endpoint: "https://target.example",
    cloud: "civo",
    region: "Phoenix",
    region_code: "phx1",
    channel: "experimental",
    owner: "witwave",
    weight: 1,
    accepting,
    provision_token: "private-cell-token",
    registration_id: registration,
    registered_at: "2026-07-25T00:00:00.000Z",
  };
}

function reservation({
  operationID = OPERATION,
  evacuationID = EVACUATION,
  epoch = 1,
  kind = "restore",
} = {}) {
  return {
    account_id: ACCOUNT,
    operation_id: operationID,
    evacuation_id: evacuationID,
    kind,
    target_cell: CELL,
    target_registration_id: REGISTRATION,
    epoch,
  };
}

test("exact release cannot erase a different reservation epoch or kind", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance, storage } = coordinator({ directory });
  const current = reservation({ epoch: 2, kind: "restore" });
  assert.equal(
    (await instance.fetch(
      request("/reserve", { reservation: current }),
    )).status,
    200,
  );
  const stored = structuredClone(
    storage.values.get(`reservation:${ACCOUNT}`),
  );

  for (const conflicting of [
    reservation({ epoch: 3, kind: "restore" }),
    reservation({ epoch: 2, kind: "move" }),
  ]) {
    const response = await instance.fetch(
      request("/reserve", { reservation: conflicting }),
    );
    assert.equal(response.status, 409);
    assert.deepEqual(
      storage.values.get(`reservation:${ACCOUNT}`),
      stored,
    );
  }

  for (const stale of [
    reservation({ epoch: 1, kind: "restore" }),
    reservation({ epoch: 2, kind: "move" }),
  ]) {
    const response = await instance.fetch(
      request("/release", { reservation: stale }),
    );
    assert.equal(response.status, 409);
    assert.deepEqual(
      storage.values.get(`reservation:${ACCOUNT}`),
      stored,
    );
    assert.equal(
      storage.values.get(`reservation:${ACCOUNT}`).epoch,
      current.epoch,
    );
    assert.equal(
      storage.values.get(`reservation:${ACCOUNT}`).kind,
      current.kind,
    );
  }

  const invalidKind = await instance.fetch(
    request("/release", {
      reservation: reservation({ epoch: 2, kind: "close" }),
    }),
  );
  assert.equal(invalidKind.status, 400);
  assert.ok(storage.values.has(`reservation:${ACCOUNT}`));

  assert.equal(
    (await instance.fetch(
      request("/release", { reservation: current }),
    )).status,
    200,
  );
  assert.equal(storage.values.has(`reservation:${ACCOUNT}`), false);
});

function request(path, payload = {}) {
  return new Request(`https://target-cell.internal${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cell_name: CELL, ...payload }),
  });
}

function coordinator({
  directory,
  storage = new Storage(),
  now = () => new Date("2026-07-25T12:00:00.000Z"),
  accountLifecycle,
} = {}) {
  return {
    storage,
    instance: new DurableTargetCellCoordinator(
      { id: { name: CELL }, storage },
      {
        DIRECTORY: directory,
        ACCOUNT_LIFECYCLE: accountLifecycle,
      },
      {
        now,
        randomUUID: () => "33333333-3333-4333-8333-333333333333",
      },
    ),
  };
}

function livenessNamespace({ reservationActive = true, resident = true }) {
  return {
    idFromName: (name) => ({ name }),
    get: () => ({
      fetch: async (requestValue) => {
        const input = await requestValue.json();
        if (new URL(requestValue.url).pathname === "/reservation-status") {
          return Response.json({
            ok: true,
            account_id: input.account_id,
            operation_id: input.operation_id,
            evacuation_id: input.evacuation_id,
            target_cell: input.target_cell,
            active: reservationActive,
          });
        }
        return Response.json({
          ok: true,
          account_id: input.account_id,
          cell_name: input.cell_name,
          registration_id: input.registration_id,
          route_epoch: input.route_epoch,
          resident,
        });
      },
    }),
  };
}

test("reservation and safe deletion are serialized when reserve wins", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const storage = new Storage();
  let releasePut;
  storage.pauseReservationPut = new Promise((resolve) => {
    releasePut = resolve;
  });
  const { instance } = coordinator({ directory, storage });

  const reserving = instance.fetch(
    request("/reserve", { reservation: reservation() }),
  );
  await new Promise((resolve) => setTimeout(resolve, 0));
  const draining = instance.fetch(
    request("/register", { cell: cell(false) }),
  );
  const deleting = instance.fetch(request("/delete"));
  let deleteSettled = false;
  deleting.finally(() => {
    deleteSettled = true;
  });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(
    deleteSettled,
    false,
    "delete waits behind the in-flight reservation critical section",
  );

  releasePut();
  assert.equal((await reserving).status, 200);
  assert.equal((await draining).status, 200);
  const deleteResponse = await deleting;
  assert.equal(deleteResponse.status, 409);
  assert.match(
    (await deleteResponse.json()).error,
    /reservations or residents/,
  );
  assert.notEqual(directory.value(`cell:${CELL}`), null);
});

test("safe deletion and reservation are serialized when delete wins", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(false) });
  const { instance } = coordinator({ directory });

  const deleting = instance.fetch(request("/delete"));
  const reserving = instance.fetch(
    request("/reserve", { reservation: reservation() }),
  );
  assert.equal((await deleting).status, 204);
  const reserveResponse = await reserving;
  assert.equal(reserveResponse.status, 409);
  assert.match(
    (await reserveResponse.json()).error,
    /not registered/,
  );
  assert.equal(directory.value(`cell:${CELL}`), null);
});

test("a finalized delete tombstone rejects stale KV resurrection", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(false) });
  const { instance } = coordinator({ directory });

  assert.equal((await instance.fetch(request("/delete"))).status, 204);
  await directory.put(`cell:${CELL}`, JSON.stringify(cell(false)));

  const staleReserve = await instance.fetch(
    request("/reserve", { reservation: reservation() }),
  );
  assert.equal(staleReserve.status, 409);
  assert.match(
    (await staleReserve.json()).error,
    /not registered/,
  );

  const replacement = { ...cell(true) };
  delete replacement.registration_id;
  const registered = await instance.fetch(
    request("/register", { cell: replacement }),
  );
  assert.equal(registered.status, 201);
  assert.equal(
    directory.value(`cell:${CELL}`).registration_id,
    "33333333-3333-4333-8333-333333333333",
    "explicit re-registration creates fresh authority",
  );
});

test("registration status ignores missing or stale KV projections", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance } = coordinator({ directory });

  assert.equal(
    (await instance.fetch(
      request("/register", { cell: cell(true) }),
    )).status,
    200,
  );
  await directory.delete(`cell:${CELL}`);
  let response = await instance.fetch(
    request("/registration-status", {
      registration_id: REGISTRATION,
    }),
  );
  assert.equal(response.status, 200);
  assert.equal((await response.json()).registration_status, "active");

  await directory.put(
    `cell:${CELL}`,
    JSON.stringify(cell(true, "stale-kv-registration")),
  );
  response = await instance.fetch(
    request("/registration-status", {
      registration_id: REGISTRATION,
    }),
  );
  assert.equal(response.status, 200);
  assert.equal(
    (await response.json()).registration_status,
    "active",
    "the Durable Object registration wins over a stale KV replacement",
  );

  assert.equal(
    (await instance.fetch(
      request("/register", { cell: cell(false) }),
    )).status,
    200,
  );
  assert.equal((await instance.fetch(request("/delete"))).status, 204);
  await directory.put(
    `cell:${CELL}`,
    JSON.stringify(cell(false, REGISTRATION)),
  );
  response = await instance.fetch(
    request("/registration-status", {
      registration_id: REGISTRATION,
    }),
  );
  const deleted = await response.json();
  assert.equal(response.status, 200);
  assert.equal(deleted.registration_status, "deleted");
  assert.equal(deleted.tombstone_registration_id, REGISTRATION);
});

test("an ambiguous projection delete resumes from the exact durable fence", async () => {
  let now = new Date("2026-07-25T12:00:00.000Z");
  const directory = new KV({ [`cell:${CELL}`]: cell(false) });
  directory.failCellDeleteOnce = true;
  const { instance, storage } = coordinator({
    directory,
    now: () => now,
  });

  const failed = await instance.fetch(request("/delete"));
  assert.equal(failed.status, 500);
  assert.ok(storage.values.has("delete-fence"));
  assert.ok(storage.values.has("cell-registration"));
  assert.notEqual(directory.value(`cell:${CELL}`), null);

  now = new Date("2026-07-25T12:02:00.000Z");
  await instance.alarm();
  assert.equal(storage.values.has("delete-fence"), false);
  assert.equal(storage.values.has("cell-registration"), false);
  assert.equal(storage.values.get("last-delete").phase, "finalized");
  assert.equal(directory.value(`cell:${CELL}`), null);
});

test("expired reservation is retained until exact lifecycle liveness is false", async () => {
  let now = new Date("2026-07-25T12:00:00.000Z");
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const activeLifecycle = livenessNamespace({
    reservationActive: true,
  });
  const { instance } = coordinator({
    directory,
    now: () => now,
    accountLifecycle: activeLifecycle,
  });
  assert.equal(
    (await instance.fetch(
      request("/reserve", { reservation: reservation() }),
    )).status,
    200,
  );
  await instance.fetch(request("/register", { cell: cell(false) }));
  now = new Date("2026-07-25T12:10:00.000Z");

  const blocked = await instance.fetch(request("/delete"));
  assert.equal(blocked.status, 409);
  activeLifecycle.get = () => ({
    fetch: async (requestValue) => {
      const input = await requestValue.json();
      return Response.json({
        ok: true,
        account_id: input.account_id,
        operation_id: input.operation_id,
        evacuation_id: input.evacuation_id,
        target_cell: input.target_cell,
        active: false,
      });
    },
  });
  now = new Date("2026-07-25T12:20:00.000Z");
  assert.equal((await instance.fetch(request("/delete"))).status, 204);
});

test("resident handoff blocks deletion after route projection visibility is lost", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance } = coordinator({ directory });
  assert.equal(
    (await instance.fetch(
      request("/reserve", { reservation: reservation() }),
    )).status,
    200,
  );
  assert.equal(
    (await instance.fetch(
      request("/promote", { reservation: reservation() }),
    )).status,
    200,
  );
  // A delayed release from the lifecycle cannot erase resident authority.
  assert.equal(
    (await instance.fetch(
      request("/release", { reservation: reservation() }),
    )).status,
    200,
  );
  await instance.fetch(request("/register", { cell: cell(false) }));
  assert.equal(
    directory.value(`acct:${ACCOUNT}`),
    null,
    "the test deliberately models a not-yet-visible route projection",
  );
  const blocked = await instance.fetch(request("/delete"));
  assert.equal(blocked.status, 409);

  assert.equal(
    (await instance.fetch(
      request("/depart", {
        account_id: ACCOUNT,
        operation_id: OPERATION,
        registration_id: REGISTRATION,
        source_epoch: 1,
      }),
    )).status,
    200,
  );
  assert.equal((await instance.fetch(request("/delete"))).status, 204);
});

test("the next lifecycle operation departs the exact source route epoch", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance, storage } = coordinator({ directory });
  const admitted = reservation({ epoch: 1 });
  assert.equal(
    (await instance.fetch(
      request("/reserve", { reservation: admitted }),
    )).status,
    200,
  );
  assert.equal(
    (await instance.fetch(
      request("/promote", { reservation: admitted }),
    )).status,
    200,
  );

  const departed = await instance.fetch(
    request("/depart", {
      account_id: ACCOUNT,
      operation_id: NEXT_OPERATION,
      registration_id: REGISTRATION,
      source_epoch: 1,
    }),
  );
  assert.equal(departed.status, 200);
  assert.equal((await departed.json()).departed, true);
  assert.equal(storage.values.has(`resident:${ACCOUNT}`), false);
});

test("a delayed older departure cannot remove a newer re-admitted resident", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance, storage } = coordinator({ directory });
  const first = reservation({ epoch: 1 });
  await instance.fetch(request("/reserve", { reservation: first }));
  await instance.fetch(request("/promote", { reservation: first }));
  assert.equal(
    (await instance.fetch(
      request("/depart", {
        account_id: ACCOUNT,
        operation_id: NEXT_OPERATION,
        registration_id: REGISTRATION,
        source_epoch: 1,
      }),
    )).status,
    200,
  );

  const readmitted = reservation({
    operationID: REENTRY_OPERATION,
    evacuationID: REENTRY_EVACUATION,
    epoch: 3,
  });
  await instance.fetch(
    request("/reserve", { reservation: readmitted }),
  );
  await instance.fetch(
    request("/promote", { reservation: readmitted }),
  );

  const delayed = await instance.fetch(
    request("/depart", {
      account_id: ACCOUNT,
      operation_id: NEXT_OPERATION,
      registration_id: REGISTRATION,
      source_epoch: 1,
    }),
  );
  assert.equal(delayed.status, 409);
  assert.equal(
    storage.values.get(`resident:${ACCOUNT}`).route_epoch,
    3,
  );

  assert.equal(
    (await instance.fetch(
      request("/depart", {
        account_id: ACCOUNT,
        operation_id: "99999999-9999-4999-8999-999999999999",
        registration_id: REGISTRATION,
        source_epoch: 3,
      }),
    )).status,
    200,
  );
  assert.equal(storage.values.has(`resident:${ACCOUNT}`), false);
});

test("signup provisioning lease survives drain and becomes a resident", async () => {
  const provisionID = "44444444-4444-4444-8444-444444444444";
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance } = coordinator({ directory });
  assert.equal(
    (await instance.fetch(
      request("/provision/begin", {
        provision_id: provisionID,
        registration_id: REGISTRATION,
      }),
    )).status,
    200,
  );
  assert.equal(
    (await instance.fetch(
      request("/register", { cell: cell(false) }),
    )).status,
    200,
  );
  assert.equal((await instance.fetch(request("/delete"))).status, 409);
  assert.equal(
    (await instance.fetch(
      request("/provision/attach", {
        provision_id: provisionID,
        registration_id: REGISTRATION,
        account_id: ACCOUNT,
        route_epoch: 0,
      }),
    )).status,
    200,
  );
  await directory.put(
    `acct:${ACCOUNT}`,
    JSON.stringify({
      cell: CELL,
      endpoint: "https://target.example",
      cell_registration_id: REGISTRATION,
    }),
  );
  assert.equal(
    (await instance.fetch(
      request("/provision/promote", {
        provision_id: provisionID,
        registration_id: REGISTRATION,
        account_id: ACCOUNT,
        route_epoch: 0,
      }),
    )).status,
    200,
  );
  await directory.delete(`acct:${ACCOUNT}`);
  assert.equal(
    (await instance.fetch(request("/delete"))).status,
    409,
    "resident authority, not KV propagation, blocks deletion",
  );
});

test("a drained cell rejects provisioning before any lease is created", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(false) });
  const { instance, storage } = coordinator({ directory });
  const response = await instance.fetch(
    request("/provision/begin", {
      provision_id: "55555555-5555-4555-8555-555555555555",
      registration_id: REGISTRATION,
    }),
  );
  assert.equal(response.status, 409);
  assert.equal(
    (await storage.list({ prefix: "provision:" })).size,
    0,
  );
});

test("heartbeat preserves generation while replacement identity rotates it", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance } = coordinator({ directory });

  const drained = { ...cell(false) };
  delete drained.provision_token;
  assert.equal(
    (await instance.fetch(
      request("/register", { cell: drained }),
    )).status,
    200,
  );
  assert.equal(
    directory.value(`cell:${CELL}`).registration_id,
    REGISTRATION,
    "a token-omitting drain heartbeat preserves the cell generation",
  );

  assert.equal(
    (await instance.fetch(
      request("/register", { cell: cell(true) }),
    )).status,
    200,
  );
  assert.equal(
    directory.value(`cell:${CELL}`).registration_id,
    REGISTRATION,
    "an endpoint- and token-stable heartbeat preserves generation",
  );

  const endpointReplacement = {
    ...cell(true),
    endpoint: "https://replacement.example",
  };
  delete endpointReplacement.registration_id;
  assert.equal(
    (await instance.fetch(
      request("/register", {
        cell: endpointReplacement,
      }),
    )).status,
    200,
  );
  assert.equal(
    directory.value(`cell:${CELL}`).registration_id,
    "33333333-3333-4333-8333-333333333333",
    "an endpoint replacement rotates generation",
  );

  const secondDirectory = new KV({
    [`cell:${CELL}`]: cell(true),
  });
  const { instance: second } = coordinator({
    directory: secondDirectory,
  });
  const tokenReplacement = {
    ...cell(true),
    provision_token: "replacement-token",
  };
  delete tokenReplacement.registration_id;
  assert.equal(
    (await second.fetch(
      request("/register", {
        cell: tokenReplacement,
      }),
    )).status,
    200,
  );
  assert.equal(
    secondDirectory.value(`cell:${CELL}`).registration_id,
    "33333333-3333-4333-8333-333333333333",
    "a same-endpoint replacement token rotates generation",
  );
});

test("same-name replacement is refused while resident authority remains", async () => {
  const directory = new KV({ [`cell:${CELL}`]: cell(true) });
  const { instance } = coordinator({ directory });
  await instance.fetch(
    request("/reserve", { reservation: reservation() }),
  );
  await instance.fetch(
    request("/promote", { reservation: reservation() }),
  );
  const replacement = {
    ...cell(true),
    endpoint: "https://replacement.example",
  };
  delete replacement.registration_id;
  const response = await instance.fetch(
    request("/register", { cell: replacement }),
  );
  assert.equal(response.status, 409);
  assert.match(
    (await response.json()).error,
    /occupancy remains/,
  );
  assert.equal(
    directory.value(`cell:${CELL}`).endpoint,
    "https://target.example",
  );
});

test("legacy routes are backfilled into resident authority on heartbeat", async () => {
  const directory = new KV({
    [`cell:${CELL}`]: cell(true),
    [`acct:${ACCOUNT}`]: {
      cell: CELL,
      endpoint: "https://target.example",
    },
  });
  const { instance, storage } = coordinator({ directory });
  const heartbeat = { ...cell(false) };
  delete heartbeat.provision_token;
  delete heartbeat.registration_id;
  assert.equal(
    (await instance.fetch(
      request("/register", { cell: heartbeat }),
    )).status,
    200,
  );
  assert.ok(storage.values.has(`resident:${ACCOUNT}`));

  // Once backfilled, resident authority survives a temporarily absent KV
  // projection and still blocks safe deletion.
  await directory.delete(`acct:${ACCOUNT}`);
  assert.equal((await instance.fetch(request("/delete"))).status, 409);
});
