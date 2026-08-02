import assert from "node:assert/strict";
import test from "node:test";

import {
  DurableRealmEmailAliasRegistry,
  realmEmailCanonicalDeliveryEnabled,
  realmEmailCanonicalInventoryEnabled,
  realmEmailRouteKey,
  runScheduledCanonicalRealmRouteInventory,
} from "../src/realm-email-alias-runtime.mjs";

const ACCOUNT = "acct_canonical";
const OTHER_ACCOUNT = "acct_canonical_other";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const DOMAIN = "agent-mail.witwave.ai";

class Storage {
  constructor() {
    this.values = new Map();
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "", limit, startAfter } = {}) {
    let rows = [...this.values]
      .filter(([key]) => key.startsWith(prefix))
      .sort(([left], [right]) => left.localeCompare(right));
    if (startAfter) rows = rows.filter(([key]) => key > startAfter);
    if (Number.isSafeInteger(limit)) rows = rows.slice(0, limit);
    return new Map(rows.map(([key, value]) => [key, structuredClone(value)]));
  }

  async transaction(callback) {
    const staged = new Map([...this.values].map(([key, value]) => [
      key,
      structuredClone(value),
    ]));
    const tx = {
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    };
    const result = await callback(tx);
    this.values = staged;
    return result;
  }

  async setAlarm(value) {
    this.alarmAt = value;
  }

  async deleteAlarm() {
    this.alarmAt = null;
  }
}

class Directory {
  constructor(entries = []) {
    this.values = new Map(entries);
  }

  async get(key, options = {}) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    if (options.type === "json") {
      return typeof value === "string" ? JSON.parse(value) : structuredClone(value);
    }
    return typeof value === "string" ? value : structuredClone(value);
  }

  async list({ prefix = "", limit = 1_000, cursor } = {}) {
    const offset = cursor ? Number(cursor.slice("cursor:".length)) : 0;
    const keys = [...this.values.keys()]
      .filter((key) => key.startsWith(prefix))
      .sort()
      .slice(offset, offset + limit)
      .map((name) => ({ name }));
    const nextOffset = offset + keys.length;
    const total = [...this.values.keys()].filter((key) =>
      key.startsWith(prefix)
    ).length;
    return {
      keys,
      list_complete: nextOffset >= total,
      ...(nextOffset < total ? { cursor: `cursor:${nextOffset}` } : {}),
    };
  }
}

class EmailDirectory {
  constructor() {
    this.values = new Map();
    this.puts = [];
    this.failAfterWrite = 0;
    this.beforePut = null;
  }

  async put(key, value) {
    await this.beforePut?.(key, value);
    this.values.set(key, value);
    this.puts.push({ key, value });
    if (this.failAfterWrite > 0) {
      this.failAfterWrite--;
      throw new Error("simulated lost KV acknowledgement");
    }
  }

  value(key) {
    const value = this.values.get(key);
    return value === undefined ? null : JSON.parse(value);
  }
}

function fixture({ inventory = true, delivery = true } = {}) {
  const storage = new Storage();
  const directory = new Directory([
    [`acct:${ACCOUNT}`, { cell: "cell-one" }],
    ["cell:cell-one", {
      endpoint: "https://cell.example",
      provision_token: "witself_prv_cell",
      agent_email_audience: "cell-one",
    }],
  ]);
  const emailDirectory = new EmailDirectory();
  let currentTime = Date.UTC(2026, 7, 2, 12, 0, 0);
  let cellRoute = {
    schema_version: "witself.v0",
    account_id: ACCOUNT,
    realm_id: REALM,
    state: "live",
    generation: 1,
  };
  let failCommit = 0;
  const transitionBodies = [];
  const fetchImpl = async (url, init = {}) => {
    const parsed = new URL(url);
    assert.equal(init.headers.Authorization, "Bearer witself_prv_cell");
    if (parsed.pathname === `/v1/accounts/${ACCOUNT}:plan` &&
        init.method === "GET") {
      return Response.json({
        account_id: ACCOUNT,
        revision: 1,
        snapshot_hash: "a".repeat(64),
        features: ["agent_email_receive"],
        limits: {},
      });
    }
    if (parsed.pathname === `/v1/accounts/${ACCOUNT}:email-realm-routes` &&
        init.method === "GET") {
      return Response.json({
        schema_version: "witself.v0",
        account_id: ACCOUNT,
        routes: [cellRoute],
        next_cursor: null,
      });
    }
    if (parsed.pathname === `/v1/accounts/${ACCOUNT}:email-realm-route` &&
        init.method === "GET") {
      return parsed.searchParams.get("realm_id") === REALM
        ? Response.json(cellRoute)
        : Response.json({ error: "not found" }, { status: 404 });
    }
    if (parsed.pathname ===
          `/v1/accounts/${ACCOUNT}:prepare-email-realm-route-retirement` &&
        init.method === "POST") {
      const body = JSON.parse(init.body);
      transitionBodies.push({ phase: "prepare", body });
      assert.deepEqual(Object.keys(body).sort(), [
        "expected_generation",
        "operation_id",
        "realm_id",
      ]);
      if (cellRoute.state === "live") {
        assert.equal(body.expected_generation, cellRoute.generation);
        cellRoute = {
          ...cellRoute,
          state: "closing",
          generation: cellRoute.generation + 1,
          operation_id: body.operation_id,
        };
      }
      return Response.json(cellRoute);
    }
    if (parsed.pathname ===
          `/v1/accounts/${ACCOUNT}:commit-email-realm-route-retirement` &&
        init.method === "POST") {
      const body = JSON.parse(init.body);
      transitionBodies.push({ phase: "commit", body });
      assert.deepEqual(Object.keys(body).sort(), [
        "expected_generation",
        "operation_id",
        "realm_id",
      ]);
      assert.equal(body.expected_generation, cellRoute.generation);
      if (failCommit > 0) {
        failCommit--;
        return Response.json({ error: "temporary" }, { status: 502 });
      }
      cellRoute = { ...cellRoute, state: "retired" };
      return Response.json(cellRoute);
    }
    return Response.json({ error: "not found" }, { status: 404 });
  };
  const env = {
    DIRECTORY: directory,
    AGENT_EMAIL_DIRECTORY: emailDirectory,
    AGENT_EMAIL_DOMAIN: DOMAIN,
    CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: inventory ? "true" : "false",
    CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: delivery ? "true" : "false",
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
  };
  const runtime = new DurableRealmEmailAliasRegistry(
    { storage, id: { name: "global" } },
    env,
    {
      now: () => new Date(currentTime++),
      fetch: fetchImpl,
    },
  );
  return {
    env,
    runtime,
    storage,
    directory,
    emailDirectory,
    transitionBodies,
    setCellRoute(value) {
      cellRoute = structuredClone(value);
    },
    failNextCommit() {
      failCommit++;
    },
    advance(milliseconds) {
      currentTime += milliseconds;
    },
  };
}

async function call(runtime, path, body = {}) {
  const response = await runtime.fetch(new Request(`https://registry.test${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }));
  return { response, body: await response.json() };
}

test("canonical gates are exact-true and the scheduled inventory is dark by default", async () => {
  assert.equal(realmEmailCanonicalInventoryEnabled({}), false);
  assert.equal(realmEmailCanonicalDeliveryEnabled({}), false);
  assert.equal(realmEmailCanonicalInventoryEnabled({
    CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: "TRUE",
  }), false);
  assert.deepEqual(await runScheduledCanonicalRealmRouteInventory({}), {
    ran: false,
    configured: true,
  });
});

test("bounded inventory commits canonical authority before KV and retries lost acknowledgement exactly", async () => {
  const { runtime, storage, emailDirectory } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  emailDirectory.beforePut = async () => {
    const authority = await storage.get(authorityKey);
    assert.equal(authority?.controller_revision, 1);
    assert.equal(authority?.state, "applied");
  };
  emailDirectory.failAfterWrite = 1;
  const failed = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(failed.response.status, 502);
  const firstAuthority = await storage.get(authorityKey);
  const firstProjection = emailDirectory.values.get(routeKey);
  assert.equal(firstAuthority.controller_revision, 1);

  const retried = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(retried.response.status, 200);
  assert.equal(retried.body.complete, true);
  assert.deepEqual(await storage.get(authorityKey), firstAuthority);
  assert.equal(emailDirectory.values.get(routeKey), firstProjection);
  assert.equal(emailDirectory.value(routeKey).state, "applied");
});

test("canonical ownership is immutable and equal-generation conflicts cannot overwrite authority", async () => {
  const { runtime } = fixture();
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  await assert.rejects(
    runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: {
        account_id: OTHER_ACCOUNT,
        realm_id: REALM,
        state: "live",
        generation: 1,
      },
      emailEnabled: true,
      deliveryEnabled: true,
    }),
    /ownership collision/,
  );
  await assert.rejects(
    runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: {
        account_id: ACCOUNT,
        realm_id: REALM,
        state: "closing",
        generation: 1,
        operation_id: "conflicting-close",
      },
      emailEnabled: true,
      deliveryEnabled: true,
    }),
    /generation conflicts/,
  );
});

test("realm close publishes the durable tombstone before cell commit and resumes safely", async () => {
  const {
    runtime,
    storage,
    emailDirectory,
    transitionBodies,
    failNextCommit,
    advance,
  } = fixture();
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  let verifiedPrecommitTombstone = false;
  emailDirectory.beforePut = async (_key, value) => {
    const projection = JSON.parse(value);
    if (projection.state !== "retired" || verifiedPrecommitTombstone) return;
    const authority = await storage.get(authorityKey);
    assert.equal(authority.state, "retired");
    assert.equal(authority.cell_state, "closing");
    verifiedPrecommitTombstone = true;
  };
  failNextCommit();
  const request = {
    actor: { kind: "account_operator", id: "opr_close" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: DOMAIN,
    idempotency_key: "close-canonical-realm",
  };
  const failed = await call(runtime, "/canonical/realm-close", request);
  assert.equal(failed.response.status, 202);
  assert.equal(failed.body.complete, false);
  assert.equal(failed.body.phase, "commit_cell");
  assert.equal(emailDirectory.value(routeKey).state, "retired");
  assert.equal(verifiedPrecommitTombstone, true);
  assert.equal((await storage.get(authorityKey)).cell_state, "closing");
  assert.equal(
    (await storage.get(`realm-close-intent:${ACCOUNT}:${REALM}`)).phase,
    "commit_cell",
  );

  advance(6 * 60 * 1_000);
  await runtime.alarm();
  assert.ok(await storage.get(`realm-close-fence:${ACCOUNT}:${REALM}`));
  const completed = await call(runtime, "/canonical/realm-close", request);
  assert.equal(completed.response.status, 200);
  assert.equal(completed.body.complete, true);
  assert.equal(completed.body.canonical_route.state, "retired");
  assert.equal((await storage.get(authorityKey)).cell_state, "retired");
  assert.equal(
    (await call(runtime, "/canonical/realm-close", request)).response.status,
    200,
  );
  assert.deepEqual(transitionBodies.map(({ phase }) => phase), [
    "prepare",
    "commit",
    "commit",
  ]);

  const lifecycleFence = {
    account_id: ACCOUNT,
    operation_id: "account-move-after-realm-close",
    epoch: 2,
    activation_enabled: true,
  };
  assert.equal((await call(runtime, "/account-lifecycle/reconcile", {
    ...lifecycleFence,
    action: "suspend",
  })).response.status, 200);
  assert.equal((await storage.get(authorityKey)).state, "retired");
  assert.equal((await call(runtime, "/account-lifecycle/reconcile", {
    ...lifecycleFence,
    action: "republish",
  })).response.status, 200);
  assert.equal((await storage.get(authorityKey)).state, "retired");

  const blocked = await call(runtime, "/request/create", {
    actor: { kind: "account_operator", id: "opr_close" },
    account_id: ACCOUNT,
    realm_id: REALM,
    alias: "closedrealm",
    domain: DOMAIN,
    activation_enabled: true,
    feature_enabled: true,
    alias_limit: 1,
    plan_revision: 0,
    plan_snapshot_hash: "",
    idempotency_key: "alias-after-close",
  });
  assert.equal(blocked.response.status, 409);
  assert.match(blocked.body.error, /realm close/);
});

test("realm close replays the exact closing tombstone after a lost KV acknowledgement", async () => {
  const { runtime, storage, emailDirectory, transitionBodies } = fixture();
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  emailDirectory.failAfterWrite = 1;
  const request = {
    actor: { kind: "account_operator", id: "opr_close_lost_ack" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: DOMAIN,
    idempotency_key: "close-lost-kv-ack",
  };

  const interrupted = await call(runtime, "/canonical/realm-close", request);
  assert.equal(interrupted.response.status, 202);
  assert.equal(interrupted.body.phase, "publish_retired");
  const tombstone = await storage.get(authorityKey);
  assert.equal(tombstone.state, "retired");
  assert.equal(tombstone.cell_state, "closing");
  assert.equal(tombstone.cell_generation, 2);
  assert.equal(tombstone.cell_operation_id, request.idempotency_key);
  assert.equal(emailDirectory.value(routeKey).state, "retired");

  const exactClosingCellRoute = {
    account_id: ACCOUNT,
    realm_id: REALM,
    state: "closing",
    generation: tombstone.cell_generation,
    operation_id: tombstone.cell_operation_id,
  };
  await assert.rejects(
    runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: exactClosingCellRoute,
    }),
    /cannot be resurrected/,
  );
  await assert.rejects(
    runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: exactClosingCellRoute,
      forcedPolicy: { state: "retired" },
    }),
    /cannot be resurrected/,
  );
  await assert.rejects(
    runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: {
        ...exactClosingCellRoute,
        operation_id: "another-close-operation",
      },
      forcedPolicy: { state: "retired", suspension_disposition: null },
    }),
    /generation conflicts|operation conflicts/,
  );

  const completed = await call(runtime, "/canonical/realm-close", request);
  assert.equal(completed.response.status, 200);
  assert.equal(completed.body.complete, true);
  assert.equal(completed.body.canonical_route.state, "retired");
  assert.equal((await storage.get(authorityKey)).cell_state, "retired");
  assert.deepEqual(transitionBodies.map(({ phase }) => phase), [
    "prepare",
    "commit",
  ]);
  const retiredWrites = emailDirectory.puts
    .filter(({ key }) => key === routeKey)
    .map(({ value }) => JSON.parse(value))
    .filter(({ state }) => state === "retired");
  assert.equal(retiredWrites.length, 3);
  assert.deepEqual(retiredWrites[0], retiredWrites[1]);
});
