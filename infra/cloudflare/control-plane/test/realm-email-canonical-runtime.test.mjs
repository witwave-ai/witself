import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  DurableRealmEmailAliasRegistry,
  REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS,
  realmEmailCanonicalDeliveryEnabled,
  realmEmailCanonicalInventoryEnabled,
  realmEmailRouteKey,
  runScheduledCanonicalRealmRouteInventory,
} from "../src/realm-email-alias-runtime.mjs";
import {
  buildRealmEmailAliasClaimProof,
} from "../src/agent-email-custom-domain-route-contract.mjs";

const ACCOUNT = "acc_aaaaaaaaaaaaaaaa";
const OTHER_ACCOUNT = "acc_bbbbbbbbbbbbbbbb";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const DOMAIN = "agent-mail.witwave.ai";
const PRIMARY_DOMAIN = "witmail.net";

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
    this.gets = [];
    this.lists = [];
  }

  async get(key, options = {}) {
    this.gets.push(key);
    const value = this.values.get(key);
    if (value === undefined) return null;
    if (options.type === "json") {
      return typeof value === "string" ? JSON.parse(value) : structuredClone(value);
    }
    return typeof value === "string" ? value : structuredClone(value);
  }

  async list({ prefix = "", limit = 1_000, cursor } = {}) {
    this.lists.push({ prefix, limit, cursor });
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

function fixture({
  inventory = true,
  delivery = true,
  domain = DOMAIN,
  legacyDomain = null,
  customDomainStub = null,
} = {}) {
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
  let failCanonicalGet = 0;
  let failPlanGet = 0;
  const transitionBodies = [];
  const fetches = [];
  const fetchImpl = async (url, init = {}) => {
    const parsed = new URL(url);
    fetches.push({ method: init.method, pathname: parsed.pathname });
    assert.equal(init.headers.Authorization, "Bearer witself_prv_cell");
    if (parsed.pathname === `/v1/accounts/${ACCOUNT}:plan` &&
        init.method === "GET") {
      if (failPlanGet > 0) {
        failPlanGet--;
        throw new Error("simulated account plan lookup failure");
      }
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
      if (failCanonicalGet > 0) {
        failCanonicalGet--;
        throw new Error("simulated canonical route lookup failure");
      }
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
    AGENT_EMAIL_DOMAIN: domain,
    ...(legacyDomain ? { AGENT_EMAIL_LEGACY_DOMAINS: legacyDomain } : {}),
    CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: inventory ? "true" : "false",
    CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: delivery ? "true" : "false",
    CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: ACCOUNT,
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
    ...(customDomainStub
      ? {
        AGENT_EMAIL_DOMAINS: {
          idFromName: (name) => name,
          get: () => customDomainStub,
        },
      }
      : {}),
  };
  const runtime = new DurableRealmEmailAliasRegistry(
    { storage, id: { name: "global" } },
    env,
    {
      now: () => new Date(currentTime++),
      fetch: fetchImpl,
      signRouteProjection: async (projection) => ({
        ...structuredClone(projection),
        schema_version: projection.schema_version + 1,
        route_signing_key_id: "route-test",
        route_signature: `${"A".repeat(86)}==`,
      }),
    },
  );
  return {
    env,
    runtime,
    storage,
    directory,
    emailDirectory,
    fetches,
    transitionBodies,
    setCellRoute(value) {
      cellRoute = structuredClone(value);
    },
    failNextCommit() {
      failCommit++;
    },
    failNextCanonicalGet() {
      failCanonicalGet++;
    },
    failNextPlanGet() {
      failPlanGet++;
    },
    advance(milliseconds) {
      currentTime += milliseconds;
    },
    setNow(milliseconds) {
      currentTime = milliseconds;
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

test("canonical upsert lane ownership stays centralized with explicit held callers", async () => {
  const source = await readFile(
    new URL("../src/realm-email-alias-runtime.mjs", import.meta.url),
    "utf8",
  );
  const closeStart = source.indexOf("  async drainRealmCloseIntent(");
  const closeEnd = source.indexOf("\n  async boundedValues(", closeStart);
  assert.ok(closeStart >= 0 && closeEnd > closeStart);
  const closeSource = source.slice(closeStart, closeEnd);
  assert.equal(
    [...closeSource.matchAll(/this\.upsertCanonicalRoute\(/g)].length,
    0,
    "realm close already holds every canonical lane and must not re-enter it",
  );
  assert.equal(
    [...closeSource.matchAll(
      /this\.upsertCanonicalRouteWithLaneHeld\(/g,
    )].length,
    2,
  );
  const getStart = source.indexOf("  async getRoute(");
  const getEnd = source.indexOf("\n  async fetchCellCanonicalPage(", getStart);
  assert.ok(getStart >= 0 && getEnd > getStart);
  const getSource = source.slice(getStart, getEnd);
  assert.equal(
    [...getSource.matchAll(/this\.upsertCanonicalRoute\(/g)].length,
    0,
    "canonical route GET already holds account and canonical lanes",
  );
  assert.equal(
    [...getSource.matchAll(
      /this\.upsertCanonicalRouteWithLaneHeld\(/g,
    )].length,
    1,
  );
  const ordinarySource = source.slice(0, getStart) +
    source.slice(getEnd, closeStart) + source.slice(closeEnd);
  assert.equal(
    [...ordinarySource.matchAll(
      /this\.upsertCanonicalRouteWithLaneHeld\(/g,
    )].length,
    1,
    "only the lane-owning wrapper may call the already-held core outside close",
  );
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

test("stale unchanged canonical authority refreshes durably before KV without revision churn", async () => {
  const { runtime, storage, emailDirectory, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const firstAuthority = await storage.get(authorityKey);
  const firstProjection = emailDirectory.value(routeKey);

  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
  let verifiedDurableBeforeKV = false;
  emailDirectory.beforePut = async (key, value) => {
    if (key !== routeKey) return;
    const durable = await storage.get(authorityKey);
    const projection = JSON.parse(value);
    assert.equal(durable.updated_at, projection.updated_at);
    assert.equal(durable.controller_revision, firstAuthority.controller_revision);
    assert.ok(
      Date.parse(durable.updated_at) > Date.parse(firstAuthority.updated_at),
    );
    verifiedDurableBeforeKV = true;
  };

  const refreshed = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(refreshed.response.status, 200);
  const refreshedAuthority = await storage.get(authorityKey);
  const refreshedProjection = emailDirectory.value(routeKey);
  assert.equal(verifiedDurableBeforeKV, true);
  assert.equal(
    refreshedAuthority.controller_revision,
    firstAuthority.controller_revision,
  );
  assert.equal(
    refreshedProjection.controller_revision,
    firstProjection.controller_revision,
  );
  assert.equal(refreshedProjection.updated_at, refreshedAuthority.updated_at);
  assert.ok(
    Date.parse(refreshedAuthority.updated_at) >
      Date.parse(firstAuthority.updated_at),
  );
});

test("validated background renewal starts at the TTL-derived half-life boundary", async () => {
  const { runtime, storage, emailDirectory, setNow } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const original = await storage.get(authorityKey);
  const renewalMS = Math.floor(
    REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 / 2,
  );
  const validatedSource = {
    domain: DOMAIN,
    cellRoute: {
      account_id: ACCOUNT,
      realm_id: REALM,
      state: "live",
      generation: 1,
    },
    target: {
      cell_audience: "cell-one",
      ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
    },
    emailEnabled: true,
    deliveryEnabled: true,
  };

  setNow(Date.parse(original.updated_at) + renewalMS - 1);
  const beforeBoundary = await runtime.upsertCanonicalRoute(validatedSource);
  assert.deepEqual(beforeBoundary, original);
  assert.deepEqual(await storage.get(authorityKey), original);

  setNow(Date.parse(original.updated_at) + renewalMS);
  const atBoundary = await runtime.upsertCanonicalRoute(validatedSource);
  assert.equal(atBoundary.controller_revision, original.controller_revision);
  assert.equal(
    atBoundary.updated_at,
    new Date(Date.parse(original.updated_at) + renewalMS).toISOString(),
  );
  assert.deepEqual(await storage.get(authorityKey), atBoundary);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa"))
      .updated_at,
    atBoundary.updated_at,
  );
});

test("stale freshness renewal retries a lost KV acknowledgement with exact bytes", async () => {
  const { runtime, storage, emailDirectory, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const original = await storage.get(authorityKey);

  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
  emailDirectory.failAfterWrite = 1;
  const interrupted = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(interrupted.response.status, 502);
  const renewed = await storage.get(authorityKey);
  const lostAcknowledgementProjection = emailDirectory.values.get(routeKey);
  assert.equal(renewed.controller_revision, original.controller_revision);
  assert.ok(Date.parse(renewed.updated_at) > Date.parse(original.updated_at));
  assert.equal(
    JSON.parse(lostAcknowledgementProjection).updated_at,
    renewed.updated_at,
  );

  const retried = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(retried.response.status, 200);
  assert.deepEqual(await storage.get(authorityKey), renewed);
  assert.equal(
    emailDirectory.values.get(routeKey),
    lostAcknowledgementProjection,
  );
  assert.deepEqual(
    emailDirectory.puts.slice(-2).map(({ value }) => value),
    [lostAcknowledgementProjection, lostAcknowledgementProjection],
  );
});

test("validated refresh repairs future or invalid timestamps without changing authority", async () => {
  for (const corruptTimestamp of [
    (updatedAt) => new Date(
      Date.parse(updatedAt) +
        REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 2_000 + 1,
    ).toISOString(),
    () => "invalid",
  ]) {
    const { runtime, storage, emailDirectory } = fixture();
    const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
    assert.equal(
      (await call(runtime, "/canonical/inventory/reconcile")).response.status,
      200,
    );
    const original = await storage.get(authorityKey);
    const corrupted = {
      ...original,
      updated_at: corruptTimestamp(original.updated_at),
      legacy_marker: "preserve-this-byte",
    };
    await storage.put(authorityKey, corrupted);
    const writesBefore = emailDirectory.puts.length;
    let verifiedDurableBeforeKV = false;
    emailDirectory.beforePut = async (_key, value) => {
      const durable = await storage.get(authorityKey);
      const projection = JSON.parse(value);
      assert.equal(durable.updated_at, projection.updated_at);
      assert.equal(durable.controller_revision, original.controller_revision);
      assert.equal(durable.legacy_marker, "preserve-this-byte");
      assert.equal(Object.hasOwn(projection, "legacy_marker"), false);
      verifiedDurableBeforeKV = true;
    };

    const repaired = await runtime.upsertCanonicalRoute({
      domain: DOMAIN,
      cellRoute: {
        account_id: ACCOUNT,
        realm_id: REALM,
        state: "live",
        generation: 1,
      },
      target: {
        cell_audience: "cell-one",
        ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
      },
      emailEnabled: true,
      deliveryEnabled: true,
    });
    assert.equal(verifiedDurableBeforeKV, true);
    assert.equal(repaired.controller_revision, original.controller_revision);
    assert.equal(repaired.legacy_marker, "preserve-this-byte");
    assert.equal(Number.isFinite(Date.parse(repaired.updated_at)), true);
    assert.notEqual(repaired.updated_at, corrupted.updated_at);
    const { updated_at: _corruptTime, ...corruptAuthority } = corrupted;
    const { updated_at: _repairedTime, ...repairedAuthority } = repaired;
    assert.deepEqual(repairedAuthority, corruptAuthority);
    assert.deepEqual(await storage.get(authorityKey), repaired);
    assert.equal(emailDirectory.puts.length, writesBefore + 1);
  }
});

test("stale canonical GET synchronously revalidates before returning fresh authority", async () => {
  const { runtime, storage, emailDirectory, fetches, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const firstAuthority = await storage.get(authorityKey);

  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
  const fetchCount = fetches.length;
  let verifiedDurableBeforeKV = false;
  emailDirectory.beforePut = async (key, value) => {
    if (key !== routeKey) return;
    const durable = await storage.get(authorityKey);
    assert.equal(durable.updated_at, JSON.parse(value).updated_at);
    assert.equal(durable.controller_revision, firstAuthority.controller_revision);
    verifiedDurableBeforeKV = true;
  };
  const refreshed = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
  });

  const refreshedAuthority = await storage.get(authorityKey);
  assert.equal(refreshed.response.status, 200);
  assert.equal(verifiedDurableBeforeKV, true);
  assert.equal(
    refreshedAuthority.controller_revision,
    firstAuthority.controller_revision,
  );
  assert.ok(
    Date.parse(refreshedAuthority.updated_at) >
      Date.parse(firstAuthority.updated_at),
  );
  assert.equal(
    emailDirectory.value(routeKey).updated_at,
    refreshedAuthority.updated_at,
  );
  assert.deepEqual(refreshed.body, emailDirectory.value(routeKey));
  assert.deepEqual(
    fetches.slice(fetchCount).map(({ pathname }) => pathname).sort(),
    [
      `/v1/accounts/${ACCOUNT}:email-realm-route`,
      `/v1/accounts/${ACCOUNT}:plan`,
    ],
  );
  assert.equal(
    await storage.get(`route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`),
    undefined,
  );
});

test("stale canonical GET applies a changed cell fence at a new controller revision", async () => {
  const {
    runtime,
    storage,
    emailDirectory,
    setCellRoute,
    advance,
  } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const original = await storage.get(authorityKey);
  setCellRoute({
    schema_version: "witself.v0",
    account_id: ACCOUNT,
    realm_id: REALM,
    state: "closing",
    generation: 2,
    operation_id: "source-fence-closing",
  });
  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);

  const changed = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
  });
  const durable = await storage.get(authorityKey);
  assert.equal(changed.response.status, 200);
  assert.equal(durable.state, "suspended");
  assert.equal(durable.suspension_disposition, "retry");
  assert.equal(durable.cell_state, "closing");
  assert.equal(durable.cell_generation, 2);
  assert.equal(durable.cell_operation_id, "source-fence-closing");
  assert.equal(
    durable.controller_revision,
    original.controller_revision + 1,
  );
  assert.deepEqual(changed.body, emailDirectory.value(routeKey));
  assert.equal(changed.body.state, "suspended");
  assert.equal(
    changed.body.controller_revision,
    durable.controller_revision,
  );
});

test("stale canonical GET fails closed when either source fence is unavailable", async () => {
  for (const failLookup of [
    (context) => context.failNextCanonicalGet(),
    (context) => context.failNextPlanGet(),
  ]) {
    const context = fixture();
    const { runtime, storage, emailDirectory, advance } = context;
    const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
    const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
    assert.equal(
      (await call(runtime, "/canonical/inventory/reconcile")).response.status,
      200,
    );
    const authority = await storage.get(authorityKey);
    const projection = emailDirectory.values.get(routeKey);
    const writesBefore = emailDirectory.puts.length;
    advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
    failLookup(context);

    const unavailable = await call(runtime, "/route/get", {
      domain: DOMAIN,
      realm_label: "aaaaaaaaaaaaaaaa",
    });
    assert.equal(unavailable.response.status, 502);
    assert.deepEqual(await storage.get(authorityKey), authority);
    assert.equal(emailDirectory.values.get(routeKey), projection);
    assert.equal(emailDirectory.puts.length, writesBefore);
    assert.equal(
      await storage.get(`route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`),
      undefined,
    );
  }
});

test("stale canonical GET refuses plan and lifecycle intent windows before renewal I/O", async () => {
  const intentCases = [
    {
      key: `plan-intent:${ACCOUNT}`,
      value: {
        account_id: ACCOUNT,
        plan_revision: 2,
        plan_snapshot_hash: "b".repeat(64),
        state: "cell_committed",
      },
    },
    {
      key: `lifecycle-intent:${ACCOUNT}`,
      value: {
        account_id: ACCOUNT,
        operation_id: "pending-lifecycle",
        epoch: 2,
        action: "suspend",
        phase: "canonical",
      },
    },
  ];
  for (const intentCase of intentCases) {
    const {
      runtime,
      storage,
      emailDirectory,
      fetches,
      advance,
    } = fixture();
    const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
    const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
    assert.equal(
      (await call(runtime, "/canonical/inventory/reconcile")).response.status,
      200,
    );
    advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
    await storage.put(intentCase.key, intentCase.value);
    const authority = await storage.get(authorityKey);
    const projection = emailDirectory.values.get(routeKey);
    const fetchCount = fetches.length;
    const writesBefore = emailDirectory.puts.length;

    const blocked = await call(runtime, "/route/get", {
      domain: DOMAIN,
      realm_label: "aaaaaaaaaaaaaaaa",
    });
    assert.equal(blocked.response.status, 409);
    assert.deepEqual(await storage.get(authorityKey), authority);
    assert.equal(emailDirectory.values.get(routeKey), projection);
    assert.equal(emailDirectory.puts.length, writesBefore);
    assert.equal(fetches.length, fetchCount);
    assert.equal(
      await storage.get(`route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`),
      undefined,
    );
  }
});

test("bounded inventory refuses plan and lifecycle intent windows before account I/O", async () => {
  const intentCases = [
    {
      key: `plan-intent:${ACCOUNT}`,
      value: {
        account_id: ACCOUNT,
        plan_revision: 2,
        plan_snapshot_hash: "b".repeat(64),
        state: "cell_committed",
      },
    },
    {
      key: `lifecycle-intent:${ACCOUNT}`,
      value: {
        account_id: ACCOUNT,
        operation_id: "pending-lifecycle",
        epoch: 2,
        action: "suspend",
        phase: "canonical",
      },
    },
  ];
  for (const intentCase of intentCases) {
    const {
      runtime,
      storage,
      directory,
      emailDirectory,
      fetches,
      advance,
    } = fixture();
    const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
    const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
    assert.equal(
      (await call(runtime, "/canonical/inventory/reconcile")).response.status,
      200,
    );
    advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
    await storage.put("canonical-inventory", {
      schema_version: "witself.realm-email-canonical-inventory.v1",
      directory_cursor: null,
      next_directory_cursor: null,
      account_id: ACCOUNT,
      cell_cursor: null,
      cycle: 1,
    });
    await storage.put(intentCase.key, intentCase.value);
    const authority = await storage.get(authorityKey);
    const projection = emailDirectory.values.get(routeKey);
    const inventory = await storage.get("canonical-inventory");
    const fetchCount = fetches.length;
    const directoryGetCount = directory.gets.length;
    const directoryListCount = directory.lists.length;
    const writesBefore = emailDirectory.puts.length;

    const blocked = await call(runtime, "/canonical/inventory/reconcile");
    assert.equal(blocked.response.status, 409);
    assert.deepEqual(await storage.get(authorityKey), authority);
    assert.equal(emailDirectory.values.get(routeKey), projection);
    assert.deepEqual(await storage.get("canonical-inventory"), inventory);
    assert.equal(emailDirectory.puts.length, writesBefore);
    assert.equal(fetches.length, fetchCount);
    assert.equal(directory.gets.length, directoryGetCount);
    assert.equal(directory.lists.length, directoryListCount);
  }
});

test("stale retired canonical authority renews without cell or entitlement I/O", async () => {
  const { runtime, storage, emailDirectory, fetches, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const retired = await runtime.upsertCanonicalRoute({
    domain: DOMAIN,
    cellRoute: {
      account_id: ACCOUNT,
      realm_id: REALM,
      state: "retired",
      generation: 2,
      operation_id: "retire-for-refresh",
    },
  });
  const fetchCount = fetches.length;
  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
  let verifiedDurableBeforeKV = false;
  emailDirectory.beforePut = async (key, value) => {
    if (key !== routeKey) return;
    const durable = await storage.get(authorityKey);
    const projection = JSON.parse(value);
    assert.equal(durable.state, "retired");
    assert.equal(durable.controller_revision, retired.controller_revision);
    assert.equal(durable.updated_at, projection.updated_at);
    assert.equal(Object.hasOwn(projection, "cell_audience"), false);
    assert.equal(Object.hasOwn(projection, "ingest_url"), false);
    verifiedDurableBeforeKV = true;
  };
  const refreshed = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
  });

  const renewed = await storage.get(authorityKey);
  const projection = emailDirectory.value(routeKey);
  assert.equal(refreshed.response.status, 200);
  assert.equal(verifiedDurableBeforeKV, true);
  assert.equal(renewed.state, "retired");
  assert.equal(renewed.controller_revision, retired.controller_revision);
  assert.ok(Date.parse(renewed.updated_at) > Date.parse(retired.updated_at));
  assert.equal(projection.state, "retired");
  assert.equal(projection.controller_revision, retired.controller_revision);
  assert.equal(projection.updated_at, renewed.updated_at);
  assert.deepEqual(refreshed.body, projection);
  assert.equal(Object.hasOwn(projection, "cell_audience"), false);
  assert.equal(Object.hasOwn(projection, "ingest_url"), false);
  assert.equal(fetches.length, fetchCount);
  assert.equal(
    await storage.get(`route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`),
    undefined,
  );
});

test("retired freshness renewal retries a lost KV acknowledgement with exact bytes", async () => {
  const { runtime, storage, emailDirectory, fetches, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const retired = await runtime.upsertCanonicalRoute({
    domain: DOMAIN,
    cellRoute: {
      account_id: ACCOUNT,
      realm_id: REALM,
      state: "retired",
      generation: 2,
      operation_id: "retire-for-lost-ack",
    },
  });
  const fetchCount = fetches.length;
  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);
  emailDirectory.failAfterWrite = 1;

  await assert.rejects(
    runtime.publishStoredRetiredCanonicalRoute(retired),
    /agent email routing projection failed/,
  );
  const renewed = await storage.get(authorityKey);
  const lostAcknowledgementProjection = emailDirectory.values.get(routeKey);
  assert.equal(renewed.state, "retired");
  assert.equal(renewed.controller_revision, retired.controller_revision);
  assert.ok(Date.parse(renewed.updated_at) > Date.parse(retired.updated_at));
  assert.equal(
    JSON.parse(lostAcknowledgementProjection).updated_at,
    renewed.updated_at,
  );

  const retried = await runtime.publishStoredRetiredCanonicalRoute(renewed);
  assert.equal(retried.state, "retired");
  assert.deepEqual(await storage.get(authorityKey), renewed);
  assert.equal(emailDirectory.values.get(routeKey), lostAcknowledgementProjection);
  assert.deepEqual(
    emailDirectory.puts.slice(-2).map(({ value }) => value),
    [lostAcknowledgementProjection, lostAcknowledgementProjection],
  );
  assert.equal(fetches.length, fetchCount);
});

test("canonical route GET waits for every lane-owning upsert through KV acknowledgement", async () => {
  const { runtime, storage, emailDirectory, advance } = fixture();
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);

  let releaseProjection;
  let markProjectionStarted;
  const projectionStarted = new Promise((resolve) => {
    markProjectionStarted = resolve;
  });
  const projectionReleased = new Promise((resolve) => {
    releaseProjection = resolve;
  });
  let blocked = false;
  emailDirectory.beforePut = async (key) => {
    if (key !== routeKey || blocked) return;
    blocked = true;
    markProjectionStarted();
    await projectionReleased;
  };

  const upsert = runtime.upsertCanonicalRoute({
    domain: DOMAIN,
    cellRoute: {
      account_id: ACCOUNT,
      realm_id: REALM,
      state: "live",
      generation: 1,
    },
    target: {
      cell_audience: "cell-one",
      ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
    },
    emailEnabled: true,
    deliveryEnabled: true,
  });
  await projectionStarted;
  let lookupSettled = false;
  const lookup = call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
  }).then((result) => {
    lookupSettled = true;
    return result;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(lookupSettled, false);

  releaseProjection();
  const [upsertResult, lookupResult] = await Promise.all([
    upsert,
    lookup,
  ]);
  assert.equal(upsertResult.state, "applied");
  assert.equal(lookupResult.response.status, 200);
  assert.deepEqual(lookupResult.body, emailDirectory.value(routeKey));
  assert.equal(
    lookupResult.body.updated_at,
    (await storage.get(`canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`)).updated_at,
  );
});

test("stale canonical GET holds the account lane until lifecycle suspension can win", async () => {
  const { runtime, storage, emailDirectory, advance } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  advance(REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000 + 1);

  const fetchCanonical = runtime.fetchCellCanonicalRoute.bind(runtime);
  let markLookupStarted;
  let releaseLookup;
  const lookupStarted = new Promise((resolve) => {
    markLookupStarted = resolve;
  });
  const lookupReleased = new Promise((resolve) => {
    releaseLookup = resolve;
  });
  runtime.fetchCellCanonicalRoute = async (...args) => {
    markLookupStarted();
    await lookupReleased;
    return fetchCanonical(...args);
  };

  const lookup = call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
  });
  await lookupStarted;
  let lifecycleSettled = false;
  const lifecycle = call(runtime, "/account-lifecycle/reconcile", {
    account_id: ACCOUNT,
    operation_id: "route-get-race-suspend",
    epoch: 1,
    action: "suspend",
    activation_enabled: true,
  }).then((result) => {
    lifecycleSettled = true;
    return result;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(lifecycleSettled, false);

  releaseLookup();
  const [lookupResult, lifecycleResult] = await Promise.all([
    lookup,
    lifecycle,
  ]);
  assert.equal(lookupResult.response.status, 200);
  assert.equal(lifecycleResult.response.status, 200);
  assert.equal(lifecycleResult.body.complete, true);
  assert.equal((await storage.get(authorityKey)).state, "suspended");
  assert.equal(emailDirectory.value(routeKey).state, "suspended");
});

test("bounded inventory holds the account lane until lifecycle suspension can win", async () => {
  const { runtime, storage, emailDirectory } = fixture();
  const authorityKey = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const routeKey = realmEmailRouteKey(DOMAIN, "aaaaaaaaaaaaaaaa");
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );

  const fetchPage = runtime.fetchCellCanonicalPage.bind(runtime);
  let markInventoryStarted;
  let releaseInventory;
  const inventoryStarted = new Promise((resolve) => {
    markInventoryStarted = resolve;
  });
  const inventoryReleased = new Promise((resolve) => {
    releaseInventory = resolve;
  });
  runtime.fetchCellCanonicalPage = async (...args) => {
    markInventoryStarted();
    await inventoryReleased;
    return fetchPage(...args);
  };

  const inventory = call(runtime, "/canonical/inventory/reconcile");
  await inventoryStarted;
  let lifecycleSettled = false;
  const lifecycle = call(runtime, "/account-lifecycle/reconcile", {
    account_id: ACCOUNT,
    operation_id: "inventory-race-suspend",
    epoch: 1,
    action: "suspend",
    activation_enabled: true,
  }).then((result) => {
    lifecycleSettled = true;
    return result;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(lifecycleSettled, false);

  releaseInventory();
  const [inventoryResult, lifecycleResult] = await Promise.all([
    inventory,
    lifecycle,
  ]);
  assert.equal(inventoryResult.response.status, 200);
  assert.equal(lifecycleResult.response.status, 200);
  assert.equal(lifecycleResult.body.complete, true);
  assert.equal((await storage.get(authorityKey)).state, "suspended");
  assert.equal(emailDirectory.value(routeKey).state, "suspended");
});

test("one bounded inventory page publishes primary and legacy canonical routes", async () => {
  const { runtime, storage, emailDirectory } = fixture({
    domain: PRIMARY_DOMAIN,
    legacyDomain: DOMAIN,
  });
  const result = await call(runtime, "/canonical/inventory/reconcile");
  assert.equal(result.response.status, 200);
  assert.equal(result.body.routes_scanned, 1);
  assert.equal(result.body.projections_published, 2);
  for (const domain of [PRIMARY_DOMAIN, DOMAIN]) {
    const authority = await storage.get(
      `canonical:${domain}:aaaaaaaaaaaaaaaa`,
    );
    assert.equal(authority.domain, domain);
    assert.equal(authority.state, "applied");
    const projection = emailDirectory.value(
      realmEmailRouteKey(domain, "aaaaaaaaaaaaaaaa"),
    );
    assert.equal(projection.domain, domain);
    assert.equal(projection.state, "applied");
  }
});

test("known canonical routes are held back retryably by the exact account cohort", async () => {
  const { runtime, env } = fixture();
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );

  env.CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST = "";
  const heldBack = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: REALM.slice("realm_".length),
  });
  assert.equal(heldBack.response.status, 409);
  assert.equal(
    heldBack.body.code,
    "managed_email_delivery_cohort_held_back",
  );

  env.CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST =
    "acc_bbbbbbbbbbbbbbbb,acc_aaaaaaaaaaaaaaaa";
  const invalid = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: REALM.slice("realm_".length),
  });
  assert.equal(invalid.response.status, 503);
  assert.equal(invalid.body.code, "managed_email_delivery_cohort_invalid");
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

test("realm close waits for exact custom-domain completion before cell prepare", async () => {
  const convergenceCalls = [];
  const customDomainStub = {
    async fetch(request, init) {
      const path = new URL(request.url ?? request).pathname;
      const input = JSON.parse(init.body);
      convergenceCalls.push(path);
      if (path === "/route/alias-convergence/enqueue") {
        return Response.json({
          schema_version: "witself.agent-email-domain.v1",
          complete: false,
          source_fingerprint: input.source_fingerprint,
        }, { status: 202 });
      }
      assert.equal(path, "/route/alias-convergence/status");
      return Response.json({
        schema_version: "witself.agent-email-domain.v1",
        complete: true,
        source_fingerprint: input.source_fingerprint,
      });
    },
  };
  const {
    runtime,
    storage,
    transitionBodies,
  } = fixture({ customDomainStub });
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );

  const alias = "custom-route";
  const claimID = "era_aaaaaaaaaaaaaaaa";
  const createdAt = "2026-08-02T11:00:00.000Z";
  const claim = {
    claim_id: claimID,
    alias,
    domain: DOMAIN,
    skeleton: alias,
    account_id: ACCOUNT,
    realm_id: REALM,
    request_id: null,
    assignment_kind: "customer",
    assignment_revision: 2,
    admin_suspended: false,
    plan_suspended: false,
    operational_gate_suspended: false,
    lifecycle_suspended: false,
    plan_grace_until: null,
    created_at: createdAt,
    updated_at: createdAt,
    retired_at: createdAt,
    retirement_reason: "test setup",
  };
  await storage.put(`claim:${alias}`, claim);
  await storage.put(
    `account-claim:${ACCOUNT}:${REALM}:${createdAt}:${alias}`,
    alias,
  );
  await storage.put(`custom-domain-subscription:${claimID}`, {
    schema_version: "witself.realm-email-alias-custom-domain-subscription.v1",
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: alias,
    realm_alias_claim_id: claimID,
    created_at: createdAt,
  });
  await storage.put(
    `custom-domain-subscription-realm:${ACCOUNT}:${REALM}:${claimID}`,
    alias,
  );

  const request = {
    actor: { kind: "account_operator", id: "opr_close_custom" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: DOMAIN,
    idempotency_key: "close-after-custom-convergence",
  };
  const waiting = await call(runtime, "/canonical/realm-close", request);
  assert.equal(waiting.response.status, 202);
  assert.equal(waiting.body.phase, "custom_domain_converging");
  assert.deepEqual(convergenceCalls, ["/route/alias-convergence/enqueue"]);
  assert.deepEqual(transitionBodies, []);
  assert.ok(await storage.get(`custom-domain-sync:${claimID}`));

  const lateAlias = "late-route";
  const lateClaimID = "era_bbbbbbbbbbbbbbbb";
  const lateClaim = {
    ...claim,
    claim_id: lateClaimID,
    alias: lateAlias,
    skeleton: lateAlias,
  };
  await storage.put(`claim:${lateAlias}`, lateClaim);
  const fenced = await call(runtime, "/alias/custom-domain-route-subscribe", {
    claim_proof: buildRealmEmailAliasClaimProof({
      account_id: ACCOUNT,
      realm_id: REALM,
      realm_label: lateAlias,
      realm_alias_claim_id: lateClaimID,
      realm_alias_revision: lateClaim.assignment_revision,
      state: "retired",
      updated_at: lateClaim.updated_at,
    }),
  });
  assert.equal(fenced.response.status, 409);
  assert.equal(fenced.body.code, "custom_domain_subscription_realm_closed");

  const completed = await call(runtime, "/canonical/realm-close", request);
  assert.equal(completed.response.status, 200);
  assert.equal(completed.body.complete, true);
  assert.deepEqual(convergenceCalls, [
    "/route/alias-convergence/enqueue",
    "/route/alias-convergence/status",
  ]);
  assert.deepEqual(transitionBodies.map(({ phase }) => phase), [
    "prepare",
    "commit",
  ]);
  assert.equal(await storage.get(`custom-domain-sync:${claimID}`), undefined);
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

test("realm close retires both configured canonical domains with one cell fence", async () => {
  const {
    runtime,
    storage,
    emailDirectory,
    transitionBodies,
  } = fixture({ domain: PRIMARY_DOMAIN, legacyDomain: DOMAIN });
  assert.equal(
    (await call(runtime, "/canonical/inventory/reconcile")).response.status,
    200,
  );
  const closed = await call(runtime, "/canonical/realm-close", {
    actor: { kind: "account_operator", id: "opr_close_dual" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: PRIMARY_DOMAIN,
    idempotency_key: "close-canonical-dual-domain",
  });
  assert.equal(closed.response.status, 200);
  assert.deepEqual(
    closed.body.canonical_routes.map((route) => route.domain),
    [PRIMARY_DOMAIN, DOMAIN],
  );
  for (const domain of [PRIMARY_DOMAIN, DOMAIN]) {
    assert.equal(
      (await storage.get(`canonical:${domain}:aaaaaaaaaaaaaaaa`)).state,
      "retired",
    );
    assert.equal(
      emailDirectory.value(
        realmEmailRouteKey(domain, "aaaaaaaaaaaaaaaa"),
      ).state,
      "retired",
    );
  }
  const fence = await storage.get(`realm-close-fence:${ACCOUNT}:${REALM}`);
  assert.deepEqual(
    fence.canonical_revisions.map(({ domain }) => domain),
    [PRIMARY_DOMAIN, DOMAIN],
  );
  assert.deepEqual(transitionBodies.map(({ phase }) => phase), [
    "prepare",
    "commit",
  ]);
});

test("a persisted single-domain realm close freezes and retires the new bounded set", async () => {
  const {
    runtime,
    env,
    storage,
    emailDirectory,
  } = fixture({ domain: DOMAIN });
  const drain = runtime.drainRealmCloseIntent.bind(runtime);
  runtime.drainRealmCloseIntent = async () => Response.json({
    schema_version: "witself.realm-email-alias.v1",
    complete: false,
    phase: "scan_aliases",
  }, { status: 202 });
  const request = {
    actor: { kind: "account_operator", id: "opr_close_legacy" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: DOMAIN,
    idempotency_key: "close-before-domain-cutover",
  };
  assert.equal(
    (await call(runtime, "/canonical/realm-close", request)).response.status,
    202,
  );
  const legacyIntent = await storage.get(
    `realm-close-intent:${ACCOUNT}:${REALM}`,
  );
  delete legacyIntent.domains;
  await storage.put(`realm-close-intent:${ACCOUNT}:${REALM}`, legacyIntent);
  assert.equal(
    (await storage.get(`realm-close-intent:${ACCOUNT}:${REALM}`)).domains,
    undefined,
  );

  runtime.drainRealmCloseIntent = drain;
  env.AGENT_EMAIL_DOMAIN = PRIMARY_DOMAIN;
  env.AGENT_EMAIL_LEGACY_DOMAINS = DOMAIN;
  await runtime.alarm();

  assert.ok(await storage.get(`realm-close-fence:${ACCOUNT}:${REALM}`));
  for (const domain of [DOMAIN, PRIMARY_DOMAIN]) {
    assert.equal(
      (await storage.get(`canonical:${domain}:aaaaaaaaaaaaaaaa`)).state,
      "retired",
    );
    assert.equal(
      emailDirectory.value(
        realmEmailRouteKey(domain, "aaaaaaaaaaaaaaaa"),
      ).state,
      "retired",
    );
  }
});

test("realm-close alarm retry locks every persisted canonical domain lane", async () => {
  const { runtime, env, storage } = fixture({
    domain: PRIMARY_DOMAIN,
    legacyDomain: DOMAIN,
  });
  runtime.drainRealmCloseIntent = async () => Response.json({
    schema_version: "witself.realm-email-alias.v1",
    complete: false,
    phase: "scan_aliases",
  }, { status: 202 });
  const request = {
    actor: { kind: "account_operator", id: "opr_close_lanes" },
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: PRIMARY_DOMAIN,
    idempotency_key: "close-with-persisted-domain-lanes",
  };
  assert.equal(
    (await call(runtime, "/canonical/realm-close", request)).response.status,
    202,
  );
  assert.deepEqual(
    (await storage.get(`realm-close-intent:${ACCOUNT}:${REALM}`)).domains,
    [PRIMARY_DOMAIN, DOMAIN],
  );

  // Configuration may advance after an intent starts. Alarm recovery must
  // continue locking the exact persisted domain set, not only today's set.
  delete env.AGENT_EMAIL_LEGACY_DOMAINS;
  let releaseDrain;
  let markDrainStarted;
  const drainStarted = new Promise((resolve) => {
    markDrainStarted = resolve;
  });
  const drainReleased = new Promise((resolve) => {
    releaseDrain = resolve;
  });
  runtime.drainRealmCloseIntent = async () => {
    markDrainStarted();
    await drainReleased;
  };
  const alarm = runtime.alarm();
  await drainStarted;

  let legacyLaneEntered = false;
  const competing = runtime.withLane(
    `canonical:${DOMAIN}:${REALM}`,
    async () => {
      legacyLaneEntered = true;
    },
  );
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(
    legacyLaneEntered,
    false,
    "the alarm must hold the persisted legacy canonical lane while draining",
  );

  releaseDrain();
  await Promise.all([alarm, competing]);
  assert.equal(legacyLaneEntered, true);
});
