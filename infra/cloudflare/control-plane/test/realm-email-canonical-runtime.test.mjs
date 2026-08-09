import assert from "node:assert/strict";
import test from "node:test";

import {
  DurableRealmEmailAliasRegistry,
  realmEmailCanonicalDeliveryEnabled,
  realmEmailCanonicalInventoryEnabled,
  realmEmailRouteKey,
  runScheduledCanonicalRealmRouteInventory,
} from "../src/realm-email-alias-runtime.mjs";
import {
  buildRealmEmailAliasClaimProof,
} from "../src/agent-email-custom-domain-route-contract.mjs";

const ACCOUNT = "acct_canonical";
const OTHER_ACCOUNT = "acct_canonical_other";
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
    AGENT_EMAIL_DOMAIN: domain,
    ...(legacyDomain ? { AGENT_EMAIL_LEGACY_DOMAINS: legacyDomain } : {}),
    CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: inventory ? "true" : "false",
    CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: delivery ? "true" : "false",
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
        schema_version: 2,
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
