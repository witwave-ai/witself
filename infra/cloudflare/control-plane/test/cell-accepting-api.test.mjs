import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

import {
  DurableTargetCellCoordinator,
} from "../src/target-cell-coordinator.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(a, b) {
      const ab = new Uint8Array(a);
      const bb = new Uint8Array(b);
      return ab.byteLength === bb.byteLength && nodeTimingSafeEqual(ab, bb);
    },
  });
}

const CELL = "repair-cell";
const FLEET_TOKEN = "fleet-test-token";
const NOW = new Date("2026-09-04T12:00:00.000Z");

class Storage {
  values = new Map();

  async get(key) {
    return structuredClone(this.values.get(key));
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map([...this.values].filter(([key]) => key.startsWith(prefix)));
  }

  async setAlarm() {}
  async deleteAlarm() {}
}

class KV {
  values = new Map();

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

  async list({ prefix = "" } = {}) {
    return {
      keys: [...this.values.keys()]
        .filter((key) => key.startsWith(prefix))
        .map((name) => ({ name })),
      list_complete: true,
    };
  }
}

function cell(overrides = {}) {
  return {
    endpoint: "https://cell.test.invalid",
    cloud: "civo",
    region: "Phoenix",
    region_code: "phx1",
    channel: "edge",
    owner: "witwave",
    weight: 7,
    accepting: true,
    backup_validation_target: false,
    provision_token: "private-provision-token",
    backup_token: "private-backup-token",
    registration_id: "registration-current",
    registered_at: "2026-09-04T11:00:00.000Z",
    ...overrides,
  };
}

async function setup(authoritative = cell(), projection = authoritative, options = {}) {
  const storage = new Storage();
  const directory = new KV();
  if (authoritative) await storage.put("cell-registration", authoritative);
  if (projection) await directory.put(`cell:${CELL}`, JSON.stringify(projection));
  const env = {
    DIRECTORY: directory,
    FLEET_TOKEN,
    CP_ACCOUNT_BACKUPS_ENABLED: options.backupsEnabled ? "true" : "false",
  };
  const instance = new DurableTargetCellCoordinator(
    { id: { name: CELL }, storage },
    env,
    { now: () => NOW },
  );
  const calls = [];
  env.CELL_COORDINATOR = {
    idFromName: (name) => ({ name }),
    get: ({ name }) => ({
      async fetch(request) {
        assert.equal(name, CELL);
        calls.push({
          path: new URL(request.url).pathname,
          body: await request.clone().json(),
        });
        return instance.fetch(request);
      },
    }),
  };
  return { env, storage, directory, instance, calls };
}

function run(env, method, path, body, token = FLEET_TOKEN) {
  return worker.fetch(new Request(`https://cp.test.invalid${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  }), env, { waitUntil() {} });
}

function patch(env, body, token) {
  return run(env, "PATCH", `/v1/cells/${CELL}`, body, token);
}

test("drain and undrain preserve authoritative metadata despite stale KV", async () => {
  const current = cell({ future_metadata: { retained: true } });
  const stale = cell({
    channel: "experimental",
    weight: 1,
    endpoint: "https://old-cell.test.invalid",
    provision_token: "old-provision-token",
    backup_token: "old-backup-token",
  });
  const { env, storage, directory, calls } = await setup(current, stale);

  for (const accepting of [false, true]) {
    await directory.put(`cell:${CELL}`, JSON.stringify(stale));
    const response = await patch(env, { accepting });
    assert.equal(response.status, 200);
    const result = await response.json();
    const { provision_token, backup_token, ...publicMetadata } = current;
    assert.deepEqual(result, {
      schema_version: "witself.v0",
      cell: {
        name: CELL,
        ...publicMetadata,
        accepting,
        has_provision_token: true,
        has_backup_token: true,
      },
    });
    assert.ok(!JSON.stringify(result).includes(provision_token));
    assert.ok(!JSON.stringify(result).includes(backup_token));
    assert.deepEqual(await storage.get("cell-registration"), {
      ...current,
      accepting,
    });
    assert.deepEqual(await directory.get(`cell:${CELL}`, { type: "json" }), {
      ...current,
      accepting,
    });
  }
  assert.deepEqual(calls, [false, true].map((accepting) => ({
    path: "/set-accepting",
    body: { cell_name: CELL, accepting },
  })));
});

test("a stale false validation marker cannot clear isolation on drain or undrain", async () => {
  const current = cell({ accepting: false, backup_validation_target: true });
  const stale = cell({ channel: "experimental", weight: 1 });
  const { env, storage, directory } = await setup(current, stale);

  const drained = await patch(env, { accepting: false });
  assert.equal(drained.status, 200);
  assert.equal((await drained.json()).cell.backup_validation_target, true);
  assert.deepEqual(await storage.get("cell-registration"), current);
  assert.deepEqual(await directory.get(`cell:${CELL}`, { type: "json" }), current);

  await directory.put(`cell:${CELL}`, JSON.stringify(stale));
  const undrained = await patch(env, { accepting: true });
  assert.equal(undrained.status, 409);
  assert.deepEqual(await undrained.json(), {
    schema_version: "witself.v0",
    error: "cell is reserved for backup validation",
  });
  assert.deepEqual(await storage.get("cell-registration"), current);
  assert.deepEqual(await directory.get(`cell:${CELL}`, { type: "json" }), stale);
});

test("accepting-only route checks fleet auth and rejects metadata or malformed input", async () => {
  const current = cell();
  const { env, storage, calls } = await setup(current);
  for (const token of ["", "wrong-test-token"]) {
    assert.equal((await patch(env, { accepting: false }, token)).status, 401);
  }
  for (const body of [null, [], true, {}, { accepting: "false" },
    { accepting: false, backup_validation_target: false },
    { accepting: false, cell_name: "other-cell" }]) {
    assert.equal((await patch(env, body)).status, 400);
  }
  const malformed = await worker.fetch(new Request(
    `https://cp.test.invalid/v1/cells/${CELL}`,
    { method: "PATCH", headers: { Authorization: `Bearer ${FLEET_TOKEN}` }, body: "{" },
  ), env, { waitUntil() {} });
  assert.equal(malformed.status, 400);
  assert.deepEqual(calls, []);
  assert.deepEqual(await storage.get("cell-registration"), current);
});

test("undrain enforces authoritative destination credentials and drain stays available", async () => {
  for (const options of [
    { provision_token: null, backupsEnabled: false },
    { backup_token: null, backupsEnabled: true },
    { backup_token: "private-provision-token", backupsEnabled: true },
  ]) {
    const { backupsEnabled, ...overrides } = options;
    const current = cell({ ...overrides, accepting: false });
    const { env, storage } = await setup(current, cell(), { backupsEnabled });
    const response = await patch(env, { accepting: true });
    assert.equal(response.status, 400);
    assert.match((await response.json()).error, /accepting cells require/);
    assert.deepEqual(await storage.get("cell-registration"), current);
    assert.equal((await patch(env, { accepting: false })).status, 200);
  }
});

test("accepting-only mutation honors deletion fences and cannot resurrect a deleted cell", async () => {
  for (const expired of [false, true]) {
    const current = cell({ accepting: false });
    const { env, storage, directory } = await setup(current);
    await storage.put("delete-fence", {
      deletion_id: "delete-current",
      registration_id: current.registration_id,
      expires_at: expired ? "2026-09-04T11:59:00.000Z" : "2026-09-04T12:01:00.000Z",
    });
    const response = await patch(env, { accepting: true });
    assert.equal(response.status, expired ? 404 : 409);
    if (!expired) {
      assert.match((await response.json()).error, /deletion is in progress/);
      assert.deepEqual(await storage.get("cell-registration"), current);
      continue;
    }
    assert.equal(await storage.get("cell-registration"), undefined);
    assert.equal((await storage.get("last-delete")).phase, "finalized");
    await directory.put(`cell:${CELL}`, JSON.stringify(current));
    assert.equal((await patch(env, { accepting: false })).status, 404);
    assert.equal(await storage.get("cell-registration"), undefined);
  }
  const { env, storage } = await setup(null, null);
  assert.equal((await patch(env, { accepting: true })).status, 404);
  assert.equal(await storage.get("cell-registration"), undefined);
});

test("legacy fleet registration and safe deletion retain their routes and behavior", async () => {
  const { env, directory, calls } = await setup(null, null);
  const registration = { name: CELL, ...cell({ accepting: false }) };
  const registered = await run(env, "POST", "/v1/cells", registration);
  assert.equal(registered.status, 201);
  assert.equal((await registered.json()).cell.channel, "edge");
  assert.equal(calls.at(-1).path, "/register");
  const listed = await run(env, "GET", "/v1/cells");
  assert.equal(listed.status, 200);
  assert.equal((await listed.json()).cells.length, 1);
  const deleted = await run(env, "DELETE", `/v1/cells/${CELL}`);
  assert.equal(deleted.status, 204);
  assert.equal(calls.at(-1).path, "/delete");
  assert.equal(await directory.get(`cell:${CELL}`), null);
});
