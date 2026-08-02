import assert from "node:assert/strict";
import test from "node:test";

import {
  DurableRealmEmailAliasRegistry,
  INITIAL_RESERVED_REALM_EMAIL_ALIASES,
  normalizeRealmEmailAlias,
  realmEmailAliasEntitlement,
  realmEmailAliasSkeleton,
  REALM_EMAIL_ALIAS_FEATURE,
  REALM_EMAIL_ALIAS_LIMIT,
  REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
  REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
  buildRealmEmailRouteProjection,
  reconcileRealmEmailAliasesForAccountLifecycle,
  reconcileRealmEmailAliasesForPlan,
  realmEmailRouteKey,
  realmEmailAliasPendingRequestLimits,
} from "../src/realm-email-alias-runtime.mjs";

const ACCOUNT = "acct_alias";
const OTHER_ACCOUNT = "acct_other";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const OTHER_REALM = "realm_bbbbbbbbbbbbbbbb";
const DOMAIN = "agent-mail.witwave.ai";
const OPERATOR = { kind: "account_operator", id: "opr_alias" };
const ADMIN = { kind: "platform_admin", id: "adm_alias" };

function testBase32(value, pad = "a") {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let encoded = "";
  for (let current = value; current > 0; current = Math.floor(current / 32)) {
    encoded = alphabet[current % 32] + encoded;
  }
  return (encoded || "a").padStart(16, pad);
}

class Storage {
  constructor() {
    this.values = new Map();
    this.listCalls = [];
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

  async list({ prefix = "", limit, reverse = false, startAfter, end } = {}) {
    this.listCalls.push({ prefix, limit, reverse, startAfter, end });
    let entries = [...this.values]
      .filter(([key]) => key.startsWith(prefix))
      .sort(([left], [right]) => left.localeCompare(right));
    if (startAfter) entries = entries.filter(([key]) => key > startAfter);
    if (end) entries = entries.filter(([key]) => key < end);
    if (reverse) entries.reverse();
    if (Number.isSafeInteger(limit)) entries = entries.slice(0, limit);
    return new Map(
      entries.map(([key, value]) => [key, structuredClone(value)]),
    );
  }

  async transaction(callback) {
    const staged = new Map(
      [...this.values].map(([key, value]) => [key, structuredClone(value)]),
    );
    const transaction = {
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => {
        const blocker = this.transactionPutBlocker;
        if (blocker && blocker.key === key && blocker.matches(value)) {
          this.transactionPutBlocker = null;
          blocker.startedResolve();
          await blocker.wait;
        }
        staged.set(key, structuredClone(value));
      },
      delete: async (key) => staged.delete(key),
    };
    const result = await callback(transaction);
    this.values = staged;
    return result;
  }

  blockNextTransactionPut(key, matches = () => true) {
    let startedResolve;
    let release;
    const started = new Promise((resolve) => {
      startedResolve = resolve;
    });
    const wait = new Promise((resolve) => {
      release = resolve;
    });
    this.transactionPutBlocker = {
      key,
      matches,
      startedResolve,
      wait,
    };
    return { started, release };
  }

  async setAlarm(value) {
    this.setAlarmCallCount = (this.setAlarmCallCount ?? 0) + 1;
    if (this.failAtSetAlarm === this.setAlarmCallCount) {
      throw new Error("simulated setAlarm failure");
    }
    if ((this.failSetAlarms ?? 0) > 0) {
      this.failSetAlarms--;
      throw new Error("simulated setAlarm failure");
    }
    this.alarmAt = value;
  }

  async deleteAlarm() {
    if (this.deleteAlarmBlocker) {
      const blocker = this.deleteAlarmBlocker;
      this.deleteAlarmBlocker = null;
      blocker.startedResolve();
      await blocker.wait;
    }
    this.alarmAt = null;
  }

  blockNextDeleteAlarm() {
    let startedResolve;
    let release;
    const started = new Promise((resolve) => {
      startedResolve = resolve;
    });
    const wait = new Promise((resolve) => {
      release = resolve;
    });
    this.deleteAlarmBlocker = { startedResolve, wait };
    return { started, release };
  }
}

class KV {
  constructor(entries = []) {
    this.values = new Map(entries);
    this.failPuts = 0;
    this.putCount = 0;
    this.failAtPut = null;
  }

  async put(key, value) {
    this.putCount++;
    if (this.failAtPut === this.putCount) {
      throw new Error("simulated scheduled KV write failure");
    }
    if (this.failPuts > 0) {
      this.failPuts--;
      throw new Error("simulated KV write failure");
    }
    this.values.set(key, value);
  }

  async get(key, options = {}) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    if (options?.type === "json") {
      return typeof value === "string" ? JSON.parse(value) : structuredClone(value);
    }
    return typeof value === "string" ? value : structuredClone(value);
  }

  value(key) {
    const value = this.values.get(key);
    return value === undefined ? null : JSON.parse(value);
  }
}

function registry() {
  const storage = new Storage();
  const logs = [];
  const directory = new KV([
    [`acct:${ACCOUNT}`, { cell: "cell-one" }],
    [`acct:${OTHER_ACCOUNT}`, { cell: "cell-one" }],
    ["cell:cell-one", {
      endpoint: "https://cell.example",
      provision_token: "witself_prv_cell",
      agent_email_audience: "cell-one",
    }],
  ]);
  const emailDirectory = new KV();
  const cellClaims = new Map();
  let authoritativePlan = {
    account_id: ACCOUNT,
    revision: 7,
    snapshot_hash: "7".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 3 },
  };
  let requestSequence = 0;
  let claimSequence = 0;
  let currentTime = Date.UTC(2026, 7, 1, 0, 0, 0);
  let projectionBlocker = null;
  const failingClaimIDs = new Set();
  const missingRealmIDs = new Set();
  const fetchImpl = async (url, init = {}) => {
    const parsed = new URL(url);
    assert.equal(init.headers.Authorization, "Bearer witself_prv_cell");
    if (parsed.pathname === `/v1/accounts/${ACCOUNT}:plan` &&
        init.method === "GET") {
      return Response.json(authoritativePlan);
    }
    if (parsed.pathname.endsWith(":email-realm-alias-target") &&
        init.method === "GET") {
      const accountID = parsed.pathname.includes(OTHER_ACCOUNT)
        ? OTHER_ACCOUNT
        : ACCOUNT;
      const realmID = parsed.searchParams.get("realm_id");
      if (![REALM, OTHER_REALM].includes(realmID) ||
          missingRealmIDs.has(realmID)) {
        return Response.json({ error: "not found" }, { status: 404 });
      }
      return Response.json({
        schema_version: "witself.v0",
        account_id: accountID,
        realm_id: realmID,
        exists: true,
      });
    }
    if (parsed.pathname !== `/v1/accounts/${ACCOUNT}:email-realm-alias` &&
        parsed.pathname !== `/v1/accounts/${OTHER_ACCOUNT}:email-realm-alias`) {
      return Response.json({ error: "not found" }, { status: 404 });
    }
    if (init.method === "POST") {
      if (projectionBlocker) {
        const blocker = projectionBlocker;
        projectionBlocker = null;
        blocker.startedResolve();
        await blocker.wait;
      }
      const payload = JSON.parse(init.body);
      if (missingRealmIDs.has(payload.realm_id)) {
        return Response.json({ error: "not found" }, { status: 404 });
      }
      if (failingClaimIDs.has(payload.claim_id)) {
        return Response.json({ error: "simulated poison claim" }, { status: 502 });
      }
      const acknowledgement = {
        account_id: parsed.pathname.includes(ACCOUNT) ? ACCOUNT : OTHER_ACCOUNT,
        ...payload,
      };
      cellClaims.set(payload.claim_id, acknowledgement);
      return Response.json(acknowledgement);
    }
    const acknowledgement = cellClaims.get(parsed.searchParams.get("claim_id"));
    return acknowledgement
      ? Response.json(acknowledgement)
      : Response.json({ error: "not found" }, { status: 404 });
  };
  const env = {
    DIRECTORY: directory,
    AGENT_EMAIL_DIRECTORY: emailDirectory,
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
  };
  const runtime = new DurableRealmEmailAliasRegistry(
    { storage, id: { name: "global" } },
    env,
    {
      now: () => new Date(currentTime++),
      newRequestID: () => {
        requestSequence++;
        return `earq_${testBase32(requestSequence, "a")}`;
      },
      newClaimID: () => {
        claimSequence++;
        return `era_${testBase32(claimSequence, "a")}`;
      },
      fetch: fetchImpl,
      log: (value) => logs.push(String(value)),
    },
  );
  return {
    runtime,
    storage,
    directory,
    emailDirectory,
    cellClaims,
    logs,
    env,
    setAuthoritativePlan(snapshot) {
      authoritativePlan = structuredClone(snapshot);
    },
    blockNextProjection() {
      let startedResolve;
      let release;
      const started = new Promise((resolve) => {
        startedResolve = resolve;
      });
      const wait = new Promise((resolve) => {
        release = resolve;
      });
      projectionBlocker = { startedResolve, wait };
      return { started, release };
    },
    failClaimProjection(claimID) {
      failingClaimIDs.add(claimID);
    },
    removeRealm(realmID) {
      missingRealmIDs.add(realmID);
    },
    advance(milliseconds) {
      currentTime += milliseconds;
    },
  };
}

async function call(runtime, path, body) {
  const response = await runtime.fetch(new Request(`https://registry.test${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }));
  return { response, body: await response.json() };
}

async function requestAlias(runtime, alias, fields = {}) {
  return call(runtime, "/request/create", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    realm_id: REALM,
    alias,
    domain: DOMAIN,
    activation_enabled: true,
    feature_enabled: true,
    alias_limit: 3,
    plan_revision: 7,
    plan_snapshot_hash: "7".repeat(64),
    idempotency_key: `request-${alias}`,
    ...fields,
  });
}

async function approve(runtime, request, fields = {}) {
  return call(runtime, "/request/approve", {
    actor: ADMIN,
    request_id: request.id,
    activation_enabled: true,
    feature_enabled: true,
    alias_limit: 3,
    plan_revision: 7,
    plan_snapshot_hash: "7".repeat(64),
    reason: "approved for test",
    idempotency_key: `approve-${request.id}`,
    ...fields,
  });
}

test("alias grammar and confusable skeleton are deterministic", () => {
  assert.equal(normalizeRealmEmailAlias(" Acme-Team "), "acme-team");
  assert.equal(realmEmailAliasSkeleton("witself"), realmEmailAliasSkeleton("w1tself"));
  assert.equal(realmEmailAliasSkeleton("email"), realmEmailAliasSkeleton("e-mail"));
  assert.equal(realmEmailAliasSkeleton("mail"), realmEmailAliasSkeleton("ma1l"));
  assert.equal(realmEmailAliasSkeleton("agent"), realmEmailAliasSkeleton("ag-ent"));
  assert.throws(() => normalizeRealmEmailAlias("abcdefghijkl2345"), /canonical Realm ID/);
  assert.throws(() => normalizeRealmEmailAlias("two--hyphens"), /single hyphens/);
});

test("plan entitlement treats a missing or null enabled limit as unlimited", () => {
  assert.deepEqual(realmEmailAliasEntitlement({
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 1 },
  }), { enabled: true, limit: 1 });
  assert.deepEqual(realmEmailAliasEntitlement({
    features: [],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 1 },
  }), { enabled: false, limit: 1 });
  assert.deepEqual(realmEmailAliasEntitlement({
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: {},
  }), { enabled: true, limit: null });
  assert.deepEqual(realmEmailAliasEntitlement({
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: null },
  }), { enabled: true, limit: null });
  assert.deepEqual(realmEmailAliasEntitlement({
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 0 },
  }), { enabled: false, limit: 0 });
});

test("pending request safety ceilings are plan-independent and only configurable downward", () => {
  assert.deepEqual(realmEmailAliasPendingRequestLimits({}), {
    per_realm: REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
    per_account: REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
  });
  assert.deepEqual(realmEmailAliasPendingRequestLimits({
    CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM: "2",
    CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT: "7",
  }), { per_realm: 2, per_account: 7 });
  assert.throws(() => realmEmailAliasPendingRequestLimits({
    CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM: "9",
  }), /configuration is invalid/);
  assert.throws(() => realmEmailAliasPendingRequestLimits({
    CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM: "8",
    CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT: "7",
  }), /configuration is inconsistent/);
});

test("seed policy blocks exact and confusable Witself names", async () => {
  const { runtime } = registry();
  for (const [name] of INITIAL_RESERVED_REALM_EMAIL_ALIASES) {
    assert.equal(normalizeRealmEmailAlias(name), name);
  }
  for (const alias of [
    "witself", "witwave", "witmail", "witpass", "email", "mail", "agent",
    "w1tself", "wit-wave", "e-mail", "ma1l", "ag-ent",
  ]) {
    const result = await requestAlias(runtime, alias, {
      idempotency_key: `blocked-${alias}`,
    });
    assert.equal(result.response.status, 409, `${alias}: ${JSON.stringify(result.body)}`);
    assert.match(result.body.error, /reserved/);
  }
  const listed = await call(runtime, "/reserved/list", { actor: ADMIN });
  assert.equal(listed.response.status, 200);
  assert.equal(
    listed.body.reserved_names.length,
    INITIAL_RESERVED_REALM_EMAIL_ALIASES.length,
  );
});

test("customer request, approval, projection, idempotency, and tombstone are durable", async () => {
  const { runtime, emailDirectory, storage } = registry();
  const created = await requestAlias(runtime, "acme");
  assert.equal(created.response.status, 202);
  assert.equal(created.body.request.status, "pending_review");

  const replay = await requestAlias(runtime, "acme");
  assert.equal(replay.response.status, 202);
  assert.deepEqual(replay.body, created.body);

  const approved = await approve(runtime, created.body.request);
  assert.equal(approved.response.status, 200);
  assert.equal(approved.body.assignment.status, "active");
  const projection = emailDirectory.value(realmEmailRouteKey(DOMAIN, "acme"));
  assert.deepEqual(
    Object.keys(projection).sort(),
    [
      "cache_ttl_seconds", "cell_audience", "controller_revision", "domain",
      "ingest_url", "realm_id", "realm_label", "route_kind",
      "schema_version", "state", "updated_at",
    ].sort(),
  );
  assert.equal(projection.schema_version, 1);
  assert.equal(projection.domain, DOMAIN);
  assert.equal(projection.realm_label, "acme");
  assert.equal(projection.realm_id, REALM);
  assert.equal(projection.route_kind, "realm_alias");
  assert.equal(projection.state, "applied");
  assert.equal(projection.controller_revision, 1);
  assert.equal(projection.cell_audience, "cell-one");
  assert.equal(
    projection.ingest_url,
    "https://cell.example/v1/internal/agent-email:ingest",
  );
  const canonical = emailDirectory.value(
    realmEmailRouteKey(DOMAIN, REALM.slice("realm_".length)),
  );
  assert.equal(canonical.route_kind, "canonical");
  assert.equal(canonical.state, "applied");

  const confusable = await requestAlias(runtime, "ac-me", {
    account_id: OTHER_ACCOUNT,
    realm_id: OTHER_REALM,
    idempotency_key: "confusable-acme",
  });
  assert.equal(confusable.response.status, 409);
  assert.match(confusable.body.error, /confusable/);

  const retired = await call(runtime, "/alias/mutate", {
    actor: ADMIN,
    alias: "acme",
    action: "retire",
    reason: "customer rebrand",
    idempotency_key: "retire-acme",
  });
  assert.equal(retired.response.status, 200);
  const retiredProjection = emailDirectory.value(
    realmEmailRouteKey(DOMAIN, "acme"),
  );
  assert.equal(retiredProjection.state, "retired");
  assert.equal(retiredProjection.account_id, undefined);
  assert.equal(retiredProjection.claim_id, undefined);
  assert.equal(retiredProjection.cell_audience, undefined);
  assert.equal(retiredProjection.ingest_url, undefined);
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`))
      .customer_allocated,
    0,
  );

  const reused = await requestAlias(runtime, "acme", {
    account_id: OTHER_ACCOUNT,
    realm_id: OTHER_REALM,
    idempotency_key: "reuse-acme",
  });
  assert.equal(reused.response.status, 409);
  assert.match(reused.body.error, /claimed or tombstoned/);
});

test("realm and account pending ceilings bound unlimited plans with replay-safe capacity", async () => {
  const { runtime, storage, env, logs } = registry();
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM = "2";
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT = "2";

  const first = await requestAlias(runtime, "queue-one", { alias_limit: null });
  const second = await requestAlias(runtime, "queue-two", {
    alias_limit: null,
    realm_id: OTHER_REALM,
  });
  assert.equal(first.response.status, 202);
  assert.equal(second.response.status, 202);

  const auditSequenceBeforeRefusal = (await storage.get("meta")).audit_sequence;
  const accountBlocked = await requestAlias(runtime, "queue-three", {
    alias_limit: null,
    realm_id: OTHER_REALM,
  });
  assert.equal(accountBlocked.response.status, 409);
  assert.match(accountBlocked.body.error, /account.*ceiling/);
  assert.equal(accountBlocked.body.code, "technical_pending_limit_reached");
  assert.equal(accountBlocked.body.scope, "account");
  assert.equal(accountBlocked.body.limit, 2);
  assert.equal(
    (await storage.get("meta")).audit_sequence,
    auditSequenceBeforeRefusal,
    "technical admission refusals must not grow durable audit history",
  );
  assert.deepEqual(JSON.parse(logs.at(-1)), {
    event: "realm_email_alias_pending_limit_refused",
    scope: "account",
    limit: 2,
  });
  assert.equal(logs.at(-1).includes(ACCOUNT), false);
  assert.equal(logs.at(-1).includes(OTHER_REALM), false);
  assert.equal(logs.at(-1).includes("queue-three"), false);

  const replay = await requestAlias(runtime, "queue-one", { alias_limit: null });
  assert.equal(replay.response.status, 202);
  assert.deepEqual(replay.body, first.body);
  assert.equal(
    (await storage.get(`claim-usage-account:${ACCOUNT}`)).open_requests,
    2,
  );
  const visibleCapacity = await call(runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    realm_id: REALM,
  });
  assert.equal(visibleCapacity.body.pending_counter_state, "ready");
  assert.deepEqual(visibleCapacity.body.technical_pending_limits, {
    per_realm: 2,
    per_account: 2,
  });
  assert.deepEqual(visibleCapacity.body.pending_capacity.realm, {
    used: 1,
    max: 2,
    remaining: 1,
    at_limit: false,
  });

  await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: second.body.request.id,
    reason: "clear one technical slot",
    idempotency_key: "reject-queue-two",
  });
  const replacement = await requestAlias(runtime, "queue-three", {
    alias_limit: null,
    realm_id: OTHER_REALM,
  });
  assert.equal(replacement.response.status, 202);
});

test("compiled pending ceilings reject the exact ninth realm and sixty-fifth account requests", async () => {
  {
    const { runtime, storage } = registry();
    for (let index = 0; index < REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM; index++) {
      assert.equal((await requestAlias(runtime, `realmcap${index}`, {
        alias_limit: null,
      })).response.status, 202);
    }
    const ninth = await requestAlias(runtime, "realmcap8", {
      alias_limit: null,
    });
    assert.equal(ninth.response.status, 409);
    assert.match(ninth.body.error, /realm.*ceiling/);
    assert.equal(ninth.body.code, "technical_pending_limit_reached");
    assert.equal(ninth.body.scope, "realm");
    assert.equal(ninth.body.limit, REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM);
    assert.equal(
      (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`)).open_requests,
      REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
    );
  }

  {
    const { runtime, storage } = registry();
    const realmCount = REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT /
      REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM;
    for (let realmIndex = 0; realmIndex < realmCount; realmIndex++) {
      const realmID = `realm_${testBase32(realmIndex + 10)}`;
      for (let slot = 0; slot < REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM; slot++) {
        const ordinal = realmIndex * REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM + slot;
        const created = await requestAlias(runtime, `accountcap${ordinal}`, {
          alias_limit: null,
          realm_id: realmID,
        });
        assert.equal(
          created.response.status,
          202,
          `ordinal=${ordinal} ${JSON.stringify(created.body)}`,
        );
      }
    }
    const sixtyFifth = await requestAlias(runtime, "accountcap64", {
      alias_limit: null,
      realm_id: `realm_${testBase32(99)}`,
    });
    assert.equal(sixtyFifth.response.status, 409);
    assert.match(sixtyFifth.body.error, /account.*ceiling/);
    assert.equal(sixtyFifth.body.code, "technical_pending_limit_reached");
    assert.equal(sixtyFifth.body.scope, "account");
    assert.equal(
      sixtyFifth.body.limit,
      REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
    );
    assert.equal(
      (await storage.get(`claim-usage-account:${ACCOUNT}`)).open_requests,
      REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
    );
  }
});

test("concurrent creates cannot overshoot the serialized realm ceiling", async () => {
  const { runtime, storage, env } = registry();
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM = "2";
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT = "64";
  const results = await Promise.all([
    requestAlias(runtime, "race-one", { alias_limit: null }),
    requestAlias(runtime, "race-two", { alias_limit: null }),
    requestAlias(runtime, "race-three", { alias_limit: null }),
  ]);
  assert.deepEqual(
    results.map((result) => result.response.status).sort(),
    [202, 202, 409],
  );
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`)).open_requests,
    2,
  );
});

test("failed provisioning holds open capacity and alarm completion applies an exact delta", async () => {
  const { runtime, storage, env, emailDirectory, advance } = registry();
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM = "2";
  env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT = "64";

  const first = await requestAlias(runtime, "slow-open", { alias_limit: null });
  emailDirectory.failPuts = 1;
  const failed = await approve(runtime, first.body.request, { alias_limit: null });
  assert.equal(failed.response.status, 502);
  let usage = await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`);
  assert.deepEqual({
    open: usage.open_requests,
    pending: usage.pending_review,
    provisioning: usage.provisioning,
    allocated: usage.customer_allocated,
  }, { open: 1, pending: 0, provisioning: 1, allocated: 1 });

  const intervening = await requestAlias(runtime, "other-open", {
    alias_limit: null,
  });
  assert.equal(intervening.response.status, 202);
  const blocked = await requestAlias(runtime, "blocked-open", {
    alias_limit: null,
  });
  assert.equal(blocked.response.status, 409);
  assert.match(blocked.body.error, /realm.*ceiling/);

  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  usage = await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`);
  assert.deepEqual({
    open: usage.open_requests,
    pending: usage.pending_review,
    provisioning: usage.provisioning,
    allocated: usage.customer_allocated,
  }, { open: 1, pending: 1, provisioning: 0, allocated: 1 });

  const healedReplay = await approve(runtime, first.body.request, {
    alias_limit: null,
  });
  assert.equal(healedReplay.response.status, 200);
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`)).open_requests,
    1,
  );
  assert.equal((await requestAlias(runtime, "blocked-open", {
    alias_limit: null,
  })).response.status, 202);
});

test("legacy counter rebuild is bounded and customer creation fails closed until ready", async () => {
  const { runtime, storage, advance } = registry();
  const legacy = await requestAlias(runtime, "legacy-open", {
    alias_limit: null,
  });
  const claim = await storage.get("claim:legacy-open");
  const meta = await storage.get("meta");
  delete meta.pending_counter_schema_version;
  await storage.put("meta", meta);
  for (const key of [
    `claim-usage-member:${claim.claim_id}`,
    `claim-usage-account-member:${ACCOUNT}:${claim.claim_id}`,
    `claim-usage-realm-member:${ACCOUNT}:${REALM}:${claim.claim_id}`,
    `claim-usage-account:${ACCOUNT}`,
    `claim-usage-realm:${ACCOUNT}:${REALM}`,
  ]) {
    await storage.delete(key);
  }
  for (let index = 0; index < 100; index++) {
    const alias = `legacy${index}`;
    await storage.put(`claim:${alias}`, {
      claim_id: `era_${testBase32(1_000 + index)}`,
      alias,
      account_id: ACCOUNT,
      realm_id: REALM,
      assignment_kind: "customer",
      assignment_revision: 1,
      customer_activation_intent: false,
      created_at: "2026-07-31T00:00:00.000Z",
      updated_at: "2026-07-31T00:00:00.000Z",
      retired_at: null,
    });
  }

  storage.listCalls.length = 0;
  const fenced = await requestAlias(runtime, "during-rebuild", {
    alias_limit: null,
  });
  assert.equal(fenced.response.status, 503);
  assert.match(fenced.body.error, /counters are still rebuilding/);
  await runtime.alarm();
  assert.equal(
    (await storage.get("meta")).pending_counter_schema_version,
    undefined,
    "one alarm may advance only one bounded migration page",
  );
  for (let page = 0; page < 3; page++) {
    advance(5_001);
    await runtime.alarm();
    if (page < 2) {
      assert.notEqual(
        (await storage.get("meta")).pending_counter_schema_version,
        1,
        "the write fence stays closed through the full verification pass",
      );
    }
  }
  assert.equal(
    (await storage.get("meta")).pending_counter_schema_version,
    1,
  );
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`)).open_requests,
    1,
  );
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`))
      .customer_allocated,
    100,
  );
  const migrationRead = storage.listCalls.find((entry) =>
    entry.prefix === "claim:" && entry.limit === 101
  );
  assert.ok(migrationRead, "migration did not use its bounded claim page");
  assert.equal((await requestAlias(runtime, "during-rebuild", {
    alias_limit: null,
  })).response.status, 202);
  assert.equal(legacy.response.status, 202);
});

test("missing or corrupt durable usage state fails closed", async () => {
  const { runtime, storage } = registry();
  await requestAlias(runtime, "counter-guard", { alias_limit: null });
  await storage.delete(`claim-usage-realm:${ACCOUNT}:${REALM}`);
  const missing = await requestAlias(runtime, "counter-missing", {
    alias_limit: null,
  });
  assert.equal(missing.response.status, 503);
  assert.match(missing.body.error, /counter is missing/);

  await storage.put(`claim-usage-realm:${ACCOUNT}:${REALM}`, {
    schema_version: 1,
    account_id: ACCOUNT,
    realm_id: REALM,
    open_requests: -1,
    pending_review: 0,
    provisioning: 0,
    customer_allocated: 0,
  });
  const corrupt = await requestAlias(runtime, "counter-corrupt", {
    alias_limit: null,
  });
  assert.equal(corrupt.response.status, 503);
  assert.match(corrupt.body.error, /counter is invalid/);
});

test("ordinary transitions require exact membership and an audited rebuild repairs ready-state drift", async () => {
  const { runtime, storage, advance } = registry();
  const created = await requestAlias(runtime, "counter-repair", {
    alias_limit: null,
  });
  assert.equal(created.response.status, 202);
  const claim = await storage.get("claim:counter-repair");
  const memberKey = `claim-usage-member:${claim.claim_id}`;
  const member = await storage.get(memberKey);

  await storage.delete(memberKey);
  const missingMember = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "exercise the exact membership guard",
    idempotency_key: "reject-missing-membership",
  });
  assert.equal(missingMember.response.status, 503);
  assert.match(missingMember.body.error, /membership drifted/);
  assert.equal(
    (await storage.get(`request:${created.body.request.id}`)).status,
    "pending_review",
  );

  // Restore the exact member, then introduce a structurally plausible count
  // change without its deterministic integrity projection. The next ordinary
  // transition must fail closed instead of normalizing around the drift.
  await storage.put(memberKey, member);
  const realmUsageKey = `claim-usage-realm:${ACCOUNT}:${REALM}`;
  const driftedUsage = await storage.get(realmUsageKey);
  driftedUsage.open_requests = 2;
  driftedUsage.pending_review = 2;
  await storage.put(realmUsageKey, driftedUsage);
  const aggregateDrift = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "exercise the aggregate integrity guard",
    idempotency_key: "reject-aggregate-drift",
  });
  assert.equal(aggregateDrift.response.status, 503);
  assert.match(aggregateDrift.body.error, /counter is invalid/);

  const wrongActor = await call(runtime, "/counter/rebuild", {
    actor: OPERATOR,
    reason: "must require a platform administrator",
    idempotency_key: "counter-rebuild-wrong-actor",
  });
  assert.equal(wrongActor.response.status, 400);

  const rebuildInput = {
    actor: ADMIN,
    reason: "repair detected pending-counter drift",
    idempotency_key: "counter-rebuild-repair",
  };
  const accepted = await call(runtime, "/counter/rebuild", rebuildInput);
  assert.equal(accepted.response.status, 202);
  assert.deepEqual(accepted.body, {
    schema_version: "witself.realm-email-alias.v1",
    accepted: true,
    pending_counter_state: "rebuilding",
  });
  const replay = await call(runtime, "/counter/rebuild", rebuildInput);
  assert.equal(replay.response.status, 202);
  assert.deepEqual(replay.body, accepted.body);
  const collision = await call(runtime, "/counter/rebuild", {
    actor: ADMIN,
    reason: "a second recovery must not replace the active fence",
    idempotency_key: "counter-rebuild-collision",
  });
  assert.equal(collision.response.status, 409);

  assert.equal((await requestAlias(runtime, "rebuild-fenced", {
    alias_limit: null,
  })).response.status, 503);
  assert.equal((await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "writes stay fenced for the entire rebuild",
    idempotency_key: "reject-while-rebuilding",
  })).response.status, 503);

  let rebuilt = false;
  for (let page = 0; page < 12; page++) {
    await runtime.alarm();
    const meta = await storage.get("meta");
    if (meta.pending_counter_state === "ready" &&
        !await storage.get("pending-counter-migration")) {
      rebuilt = true;
      break;
    }
    advance(5_001);
  }
  assert.equal(rebuilt, true, "bounded clear, scan, and verify did not finish");
  const repairedUsage = await storage.get(realmUsageKey);
  assert.equal(repairedUsage.open_requests, 1);
  assert.equal(repairedUsage.pending_review, 1);
  assert.equal(repairedUsage.provisioning, 0);
  assert.equal(repairedUsage.customer_allocated, 0);

  const events = (await call(runtime, "/audit/list", {
    actor: ADMIN,
    limit: 500,
  })).body.events;
  assert.equal(events.filter((event) =>
    event.action === "alias.pending_counters_rebuild_requested"
  ).length, 1);
  const replayAfterCompletion = await call(
    runtime,
    "/counter/rebuild",
    rebuildInput,
  );
  assert.equal(replayAfterCompletion.response.status, 202);
  assert.equal(await storage.get("pending-counter-migration"), undefined);

  const rejected = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "repaired state permits the original transition",
    idempotency_key: "reject-after-counter-repair",
  });
  assert.equal(rejected.response.status, 200);
  assert.equal((await storage.get(realmUsageKey)).open_requests, 0);
  assert.equal((await requestAlias(runtime, "rebuild-fenced", {
    alias_limit: null,
  })).response.status, 202);
});

test("counter recovery alarm cannot run approval work between rebuild pages", async () => {
  const { runtime, storage, emailDirectory, advance } = registry();
  const created = await requestAlias(runtime, "alarm-fenced", {
    alias_limit: null,
  });
  emailDirectory.failPuts = 1;
  const failed = await approve(runtime, created.body.request, {
    alias_limit: null,
  });
  assert.equal(failed.response.status, 502);
  assert.equal(
    (await storage.get(`request:${created.body.request.id}`)).status,
    "provisioning",
  );

  const rebuild = await call(runtime, "/counter/rebuild", {
    actor: ADMIN,
    reason: "verify alarm work remains fenced between recovery pages",
    idempotency_key: "counter-rebuild-alarm-fence",
  });
  assert.equal(rebuild.response.status, 202);
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();

  assert.ok(await storage.get("pending-counter-migration"));
  assert.equal(
    (await storage.get(`request:${created.body.request.id}`)).status,
    "provisioning",
  );
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "alarm-fenced")),
    null,
  );
});

test("failed initial rebuild scheduling is retryable with the same durable key", async () => {
  const { runtime, storage, advance } = registry();
  storage.failSetAlarms = 1;
  const input = {
    actor: ADMIN,
    reason: "prove a lost initial alarm can be re-armed idempotently",
    idempotency_key: "counter-rebuild-initial-alarm-retry",
  };
  const failed = await call(runtime, "/counter/rebuild", input);
  assert.equal(failed.response.status, 503);
  assert.match(failed.body.error, /could not be scheduled/);
  assert.equal((await storage.get("meta")).pending_counter_state, "rebuilding");
  assert.ok(await storage.get("pending-counter-migration"));
  assert.ok(await storage.get(
    `idem:counter-rebuild:${input.idempotency_key}`,
  ));
  const auditSequence = (await storage.get("meta")).audit_sequence;

  const retried = await call(runtime, "/counter/rebuild", input);
  assert.equal(retried.response.status, 202);
  assert.equal(retried.body.pending_counter_state, "rebuilding");
  assert.ok(Number.isFinite(storage.alarmAt));
  assert.equal(
    (await storage.get("meta")).audit_sequence,
    auditSequence,
    "re-arming an idempotent rebuild must not reserve another audit slot",
  );

  let complete = false;
  for (let attempt = 0; attempt < 12; attempt++) {
    await runtime.alarm();
    if (!await storage.get("pending-counter-migration")) {
      complete = true;
      break;
    }
    advance(5_001);
  }
  assert.equal(complete, true, "re-armed rebuild did not complete autonomously");
  assert.equal((await storage.get("meta")).pending_counter_state, "ready");
});

test("mid-rebuild re-arm failure propagates and an alarm retry completes recovery", async () => {
  const { runtime, storage, advance } = registry();
  const accepted = await call(runtime, "/counter/rebuild", {
    actor: ADMIN,
    reason: "prove alarm retry resumes after a durable cursor advance",
    idempotency_key: "counter-rebuild-mid-alarm-retry",
  });
  assert.equal(accepted.response.status, 202);
  const before = await storage.get("pending-counter-migration");
  // alarm() first refreshes the already-present alarm during seed validation;
  // fail the second setAlarm, after reconcile has advanced and persisted one
  // clear page, to exercise the rebuilding branch rather than the entry path.
  storage.failAtSetAlarm = (storage.setAlarmCallCount ?? 0) + 2;
  await assert.rejects(runtime.alarm(), /simulated setAlarm failure/);
  const advanced = await storage.get("pending-counter-migration");
  assert.notDeepEqual(advanced, before);
  assert.equal(advanced.clear_prefix_index, 1);

  let complete = false;
  for (let attempt = 0; attempt < 12; attempt++) {
    advance(5_001);
    await runtime.alarm();
    if (!await storage.get("pending-counter-migration")) {
      complete = true;
      break;
    }
  }
  assert.equal(complete, true, "retried alarm did not complete recovery");
  assert.equal((await storage.get("meta")).pending_counter_state, "ready");
});

test("failed edge publication leaves a fenced provisioning intent and same-key retry heals", async () => {
  const { runtime, emailDirectory, storage } = registry();
  const created = await requestAlias(runtime, "retryable");
  emailDirectory.failPuts = 1;

  const failed = await approve(runtime, created.body.request);
  assert.equal(failed.response.status, 502);
  assert.match(failed.body.error, /routing projection/);
  const pending = await call(runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(pending.body.request.status, "provisioning");
  const intent = await storage.get("claim:retryable");
  assert.equal(intent.customer_activation_intent, true);
  assert.equal(intent.assignment_revision, 1);
  const hidden = await call(runtime, "/alias/list", { actor: ADMIN });
  assert.equal(hidden.body.aliases.length, 0);
  const failedAudit = await call(runtime, "/audit/list", {
    actor: ADMIN,
    limit: 500,
  });
  assert.ok(failedAudit.body.events.some((event) =>
    event.target === "retryable" &&
    event.action === "alias.approved.intent_recorded" &&
    event.metadata.phase === "prepared"
  ));
  assert.equal(failedAudit.body.events.some((event) =>
    event.target === "retryable" && event.action === "alias.approved" &&
    event.metadata.phase === "committed"
  ), false);
  const rejected = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "must not release a provisioned claim",
    idempotency_key: "reject-provisioning",
  });
  assert.equal(rejected.response.status, 409);

  const retried = await approve(runtime, created.body.request);
  assert.equal(retried.response.status, 200);
  assert.equal(retried.body.assignment.status, "active");
  assert.equal(retried.body.assignment.claim_id, intent.claim_id);
  assert.equal(retried.body.assignment.assignment_revision, 1);
  const healedAudit = await call(runtime, "/audit/list", {
    actor: ADMIN,
    limit: 500,
  });
  assert.ok(healedAudit.body.events.some((event) =>
    event.target === "retryable" && event.action === "alias.approved" &&
    event.metadata.phase === "committed"
  ));
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "retryable")).state,
    "applied",
  );
});

test("authoritative route fallback queues bounded asynchronous edge KV repair", async () => {
  const { runtime, emailDirectory, storage } = registry();
  const created = await requestAlias(runtime, "healing");
  await approve(runtime, created.body.request);
  const key = realmEmailRouteKey(DOMAIN, "healing");
  const before = emailDirectory.value(key);
  emailDirectory.values.delete(key);

  const refreshed = await call(runtime, "/route/get", {
    domain: DOMAIN,
    realm_label: "healing",
  });
  assert.equal(refreshed.response.status, 200);
  assert.equal(refreshed.body.state, "applied");
  assert.ok(Date.parse(refreshed.body.updated_at) > Date.parse(before.updated_at));
  assert.equal(emailDirectory.value(key), null);
  assert.ok(await storage.get(`route-refresh:${DOMAIN}:healing`));
  await runtime.alarm();
  assert.equal(emailDirectory.value(key).state, "applied");
  assert.equal(await storage.get(`route-refresh:${DOMAIN}:healing`), undefined);
});

test("idempotent replay is side-effect free behind an account lifecycle fence", async () => {
  const { runtime, emailDirectory, directory } = registry();
  const created = await requestAlias(runtime, "replay-safe");
  const approved = await approve(runtime, created.body.request);
  assert.equal(approved.response.status, 200);

  const lifecycle = await call(runtime, "/account-lifecycle/reconcile", {
    account_id: ACCOUNT,
    operation_id: "move-replay-safe",
    epoch: 1,
    action: "suspend",
    activation_enabled: true,
  });
  assert.equal(lifecycle.response.status, 200);
  assert.equal(lifecycle.body.complete, false);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "replay-safe")).state,
    "suspended",
  );

  directory.values.delete(`acct:${ACCOUNT}`);
  const replay = await approve(runtime, created.body.request);
  assert.equal(replay.response.status, 200);
  assert.deepEqual(replay.body, approved.body);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "replay-safe")).state,
    "suspended",
  );
});

test("plan and lifecycle adapters advance one durable page and refuse partial completion", async () => {
  let planCalls = 0;
  let lifecycleCalls = 0;
  const env = {
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "false",
    REALM_EMAIL_ALIASES: {
      idFromName(name) { return name; },
      get() {
        return {
          async fetch(url) {
            if (String(url).includes("/plan/reconcile")) {
              planCalls++;
              return Response.json({ operational_gate_complete: false });
            }
            lifecycleCalls++;
            return Response.json({ complete: false });
          },
        };
      },
    },
  };
  const snapshot = {
    revision: 9,
    snapshot_hash: "9".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: null },
  };
  await assert.rejects(
    () => reconcileRealmEmailAliasesForPlan(
      env,
      ACCOUNT,
      snapshot,
      "restrict_only",
    ),
    /still converging/,
  );
  await assert.rejects(
    () => reconcileRealmEmailAliasesForAccountLifecycle(env, ACCOUNT, {
      operation_id: "move-one-page",
      epoch: 2,
      action: "suspend",
    }),
    /still converging/,
  );
  assert.equal(planCalls, 1);
  assert.equal(lifecycleCalls, 1);
});

test("empty registry fast-completes gate-off plan and account lifecycle adapters", async () => {
  const { runtime, env, storage } = registry();
  env.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED = "false";
  env.REALM_EMAIL_ALIASES = {
    idFromName(name) { return name; },
    get() {
      return {
        fetch(request, init) {
          return runtime.fetch(
            typeof request === "string"
              ? new Request(request, init)
              : request,
          );
        },
      };
    },
  };
  const snapshot = {
    revision: 60,
    snapshot_hash: "6".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: null },
  };
  const plan = await reconcileRealmEmailAliasesForPlan(
    env,
    ACCOUNT,
    snapshot,
    "restrict_only",
  );
  assert.equal(plan.operational_gate_complete, true);
  assert.equal(plan.changed, 0);
  assert.equal(
    (await storage.get(`plan-intent:${ACCOUNT}`)).operational_gate_complete,
    true,
  );

  const lifecycle = await reconcileRealmEmailAliasesForAccountLifecycle(
    env,
    ACCOUNT,
    {
      operation_id: "empty-registry-move",
      epoch: 1,
      action: "suspend",
    },
  );
  assert.equal(lifecycle.complete, true);
  assert.equal(lifecycle.changed, 0);
  assert.equal(await storage.get(`lifecycle-intent:${ACCOUNT}`), undefined);
  assert.equal(
    (await storage.get(`lifecycle-fence:${ACCOUNT}`)).operation_id,
    "empty-registry-move",
  );
});

test("shared route builder rejects target details on non-applied rows", () => {
  const route = buildRealmEmailRouteProjection({
    domain: DOMAIN,
    realm_id: REALM,
    realm_label: "acme",
    route_kind: "realm_alias",
    state: "suspended",
    suspension_disposition: "retry",
    controller_revision: 2,
    updated_at: "2026-08-01T00:00:00Z",
    cell_audience: "must-not-leak",
    ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
  });
  assert.equal(route.cell_audience, undefined);
  assert.equal(route.ingest_url, undefined);
});

test("new reservations flag active conflicts without revoking and remain versioned", async () => {
  const { runtime, emailDirectory } = registry();
  const created = await requestAlias(runtime, "launch");
  await approve(runtime, created.body.request);

  const reserved = await call(runtime, "/reserved/create", {
    actor: ADMIN,
    name: "launch",
    category: "platform_brand",
    reason: "future Witself service",
    internal_assignable: true,
    idempotency_key: "reserve-launch",
  });
  assert.equal(reserved.response.status, 201);
  assert.equal(reserved.body.reserved_name.version, 1);
  assert.equal(reserved.body.reserved_name.claim_conflict.alias, "launch");
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "launch")).state,
    "applied",
  );

  const updated = await call(runtime, "/reserved/update", {
    actor: ADMIN,
    name: "launch",
    enabled: false,
    reason: "service name released",
    idempotency_key: "disable-launch",
  });
  assert.equal(updated.response.status, 200);
  assert.equal(updated.body.reserved_name.version, 2);
  assert.equal(updated.body.reserved_name.enabled, false);

  const stillClaimed = await requestAlias(runtime, "lau-nch", {
    account_id: OTHER_ACCOUNT,
    realm_id: OTHER_REALM,
    idempotency_key: "claim-launch-lookalike",
  });
  assert.equal(stillClaimed.response.status, 409);
  assert.match(stillClaimed.body.error, /confusable/);
});

test("protected names can only use the privileged internal assignment path", async () => {
  const { runtime, emailDirectory } = registry();
  const assigned = await call(runtime, "/alias/assign-internal", {
    actor: ADMIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    alias: "witwave",
    domain: DOMAIN,
    activation_enabled: true,
    reason: "Witwave founder realm",
    idempotency_key: "internal-witwave",
  });
  assert.equal(assigned.response.status, 201);
  assert.equal(assigned.body.assignment.assignment_kind, "internal");
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "witwave")).state,
    "applied",
  );

  const unreserved = await call(runtime, "/alias/assign-internal", {
    actor: ADMIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    alias: "not-protected",
    domain: DOMAIN,
    activation_enabled: true,
    reason: "should fail",
    idempotency_key: "internal-not-protected",
  });
  assert.equal(unreserved.response.status, 403);
});

test("internal assignment persists an admin-visible stable intent before projection", async () => {
  const { runtime, emailDirectory, storage } = registry();
  const input = {
    actor: ADMIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    alias: "witself",
    domain: DOMAIN,
    activation_enabled: true,
    reason: "founder routing",
    idempotency_key: "internal-witself-retry",
  };
  emailDirectory.failPuts = 1;
  const failed = await call(runtime, "/alias/assign-internal", input);
  assert.equal(failed.response.status, 502);
  const intent = await storage.get("claim:witself");
  assert.equal(intent.internal_intent, true);
  assert.equal(intent.assignment_kind, "internal");
  const visible = await call(runtime, "/alias/list", { actor: ADMIN });
  assert.equal(visible.body.aliases.length, 1);
  assert.equal(visible.body.aliases[0].status, "provisioning");

  const retried = await call(runtime, "/alias/assign-internal", input);
  assert.equal(retried.response.status, 201);
  assert.equal(retried.body.assignment.claim_id, intent.claim_id);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "witself")).state,
    "applied",
  );
});

test("admin can terminally abort customer provisioning so lifecycle work cannot wedge", async () => {
  const { runtime, emailDirectory, storage } = registry();
  const created = await requestAlias(runtime, "abort-customer");
  emailDirectory.failPuts = 1;
  assert.equal((await approve(runtime, created.body.request)).response.status, 502);

  const aborted = await call(runtime, "/alias/abort-internal", {
    actor: ADMIN,
    alias: "abort-customer",
    reason: "cancel stuck provisioning before evacuation",
    idempotency_key: "abort-customer-provisioning",
  });
  assert.equal(aborted.response.status, 200);
  assert.equal(aborted.body.assignment.status, "retired");
  assert.equal((await storage.get("claim:abort-customer")).retired_at !== null, true);
  assert.equal(
    (await storage.get(`request:${created.body.request.id}`)).status,
    "rejected",
  );
  const usage = await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`);
  assert.deepEqual({
    open: usage.open_requests,
    provisioning: usage.provisioning,
    allocated: usage.customer_allocated,
  }, { open: 0, provisioning: 0, allocated: 0 });
});

test("customer terminal outbox survives missing realm and edge failure without resurrection", async () => {
  const {
    runtime,
    emailDirectory,
    storage,
    removeRealm,
    advance,
  } = registry();
  const created = await requestAlias(runtime, "terminal-cust");
  emailDirectory.failPuts = 1;
  assert.equal((await approve(runtime, created.body.request)).response.status, 502);
  removeRealm(REALM);
  emailDirectory.failPuts = 1;
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();

  assert.equal(
    (await storage.get("claim:terminal-cust")).customer_activation_intent,
    true,
  );
  assert.ok(await storage.get("projection-intent:terminal-cust"));
  assert.equal(
    (await storage.list({ prefix: "approval-due:" })).size,
    0,
    "projection retry must own recovery without a stale approval alarm",
  );

  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  const terminal = await storage.get("claim:terminal-cust");
  assert.equal(terminal.customer_activation_intent, false);
  assert.equal(terminal.retired_at !== null, true);
  assert.equal(await storage.get("projection-intent:terminal-cust"), undefined);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "terminal-cust")).state,
    "retired",
  );
});

test("internal terminal outbox survives missing realm and edge failure without resurrection", async () => {
  const {
    runtime,
    emailDirectory,
    storage,
    removeRealm,
    advance,
  } = registry();
  emailDirectory.failPuts = 1;
  const input = {
    actor: ADMIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    alias: "witself",
    domain: DOMAIN,
    activation_enabled: true,
    reason: "founder routing",
    idempotency_key: "internal-terminal-retry",
  };
  assert.equal(
    (await call(runtime, "/alias/assign-internal", input)).response.status,
    502,
  );
  removeRealm(REALM);
  emailDirectory.failPuts = 1;
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal((await storage.get("claim:witself")).internal_intent, true);
  assert.ok(await storage.get("projection-intent:witself"));
  assert.equal((await storage.list({ prefix: "internal-due:" })).size, 0);

  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  const terminal = await storage.get("claim:witself");
  assert.equal(terminal.internal_intent, false);
  assert.equal(terminal.retired_at !== null, true);
  assert.equal(await storage.get("projection-intent:witself"), undefined);
});

test("plan downgrade grants 30 days, alarm suspends, and upgrade reactivates", async () => {
  const { runtime, emailDirectory, advance, storage } = registry();
  const first = await requestAlias(runtime, "alpha");
  await approve(runtime, first.body.request);
  const second = await requestAlias(runtime, "bravo", {
    idempotency_key: "request-bravo",
  });
  await approve(runtime, second.body.request);

  const restricted = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 1,
    mode: "restrict_only",
    plan_revision: 10,
    plan_snapshot_hash: "a".repeat(64),
  });
  assert.equal(restricted.response.status, 200);
  assert.equal(restricted.body.changed, 0);
  assert.equal(restricted.body.pending, true);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "bravo")).state,
    "applied",
  );

  const committedRestriction = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 1,
    mode: "complete",
    plan_revision: 10,
    plan_snapshot_hash: "a".repeat(64),
  });
  assert.equal(committedRestriction.body.changed, 1);
  assert.equal(committedRestriction.body.assignments[0].status, "active_grace");
  assert.match(
    committedRestriction.body.assignments[0].plan_grace_until,
    /^2026-08-31T/,
  );
  assert.ok(storage.alarmAt);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "bravo")).state,
    "applied",
  );

  const disabled = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: false,
    activation_enabled: true,
    alias_limit: 0,
    mode: "restrict_only",
    plan_revision: 11,
    plan_snapshot_hash: "b".repeat(64),
  });
  assert.equal(disabled.body.changed, 0);
  assert.equal(disabled.body.pending, true);

  const committedDisable = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: false,
    activation_enabled: true,
    alias_limit: 0,
    mode: "complete",
    plan_revision: 11,
    plan_snapshot_hash: "b".repeat(64),
  });
  assert.equal(committedDisable.body.changed, 1);
  assert.equal(committedDisable.body.assignments[0].status, "active_grace");

  advance(30 * 24 * 60 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  for (const alias of ["alpha", "bravo"]) {
    assert.equal(
      emailDirectory.value(realmEmailRouteKey(DOMAIN, alias)).state,
      "suspended",
    );
  }

  const restored = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 3,
    mode: "complete",
    plan_revision: 12,
    plan_snapshot_hash: "c".repeat(64),
  });
  assert.equal(restored.body.changed, 2);
  for (const alias of ["alpha", "bravo"]) {
    assert.equal(
      emailDirectory.value(realmEmailRouteKey(DOMAIN, alias)).state,
      "applied",
    );
  }
});

test("retiring a counted alias durably promotes the next alias under the same plan fence", async () => {
  const {
    runtime,
    storage,
    emailDirectory,
    setAuthoritativePlan,
    advance,
  } = registry();
  for (const alias of ["promote-a", "promote-b"]) {
    const created = await requestAlias(runtime, alias, {
      idempotency_key: `request-${alias}`,
    });
    await approve(runtime, created.body.request, {
      idempotency_key: `approve-${alias}`,
    });
  }
  const snapshot = {
    account_id: ACCOUNT,
    revision: 40,
    snapshot_hash: "4".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 1 },
  };
  setAuthoritativePlan(snapshot);
  const limited = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 1,
    mode: "complete",
    plan_revision: snapshot.revision,
    plan_snapshot_hash: snapshot.snapshot_hash,
  });
  assert.equal(limited.response.status, 200);
  advance(30 * 24 * 60 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal((await storage.get("claim:promote-b")).plan_suspended, true);

  const retired = await call(runtime, "/alias/mutate", {
    actor: ADMIN,
    alias: "promote-a",
    action: "retire",
    reason: "free capacity",
    idempotency_key: "retire-promote-a",
  });
  assert.equal(retired.response.status, 200);
  assert.ok(await storage.get(`plan-intent:${ACCOUNT}`));
  advance(1_001);
  await runtime.alarm();
  assert.equal((await storage.get("claim:promote-b")).plan_suspended, false);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "promote-b")).state,
    "applied",
  );
});

test("operational gate disables immediately and plan reconciliation cannot bypass it", async () => {
  const { runtime, emailDirectory } = registry();
  const requested = await requestAlias(runtime, "gateplan");
  await approve(runtime, requested.body.request);

  const disabled = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: false,
    alias_limit: 3,
    mode: "restrict_only",
    plan_revision: 20,
    plan_snapshot_hash: "d".repeat(64),
  });
  assert.equal(disabled.response.status, 200);
  assert.equal(disabled.body.assignments[0].status, "suspended_gate");
  assert.equal(disabled.body.assignments[0].plan_grace_until, undefined);
  const disabledRoute = emailDirectory.value(
    realmEmailRouteKey(DOMAIN, "gateplan"),
  );
  assert.equal(disabledRoute.state, "suspended");
  assert.equal(disabledRoute.suspension_disposition, "retry");

  const stillDisabled = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: false,
    alias_limit: 3,
    mode: "complete",
    plan_revision: 20,
    plan_snapshot_hash: "d".repeat(64),
  });
  assert.equal(stillDisabled.body.changed, 0);

  const enabled = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 3,
    mode: "complete",
    plan_revision: 21,
    plan_snapshot_hash: "e".repeat(64),
  });
  assert.equal(enabled.body.assignments[0].status, "active");
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "gateplan")).state,
    "applied",
  );
});

test("operational gate paginates beyond 100 claims before attesting completion", async () => {
  const { runtime, storage, emailDirectory } = registry();
  await call(runtime, "/reserved/list", { actor: ADMIN });
  for (let index = 0; index < 101; index++) {
    const alias = `gate-${String(index).padStart(3, "0")}`;
    const createdAt = new Date(
      Date.UTC(2026, 0, 1, 0, 0, index),
    ).toISOString();
    const claim = {
      claim_id: `era_${"g".repeat(13)}${String(index).padStart(3, "2")}`,
      alias,
      domain: DOMAIN,
      skeleton: realmEmailAliasSkeleton(alias),
      account_id: ACCOUNT,
      realm_id: REALM,
      request_id: null,
      assignment_kind: "customer",
      assignment_revision: 1,
      admin_suspended: false,
      plan_suspended: false,
      operational_gate_suspended: false,
      lifecycle_suspended: false,
      plan_grace_until: null,
      created_at: createdAt,
      updated_at: createdAt,
      retired_at: null,
    };
    await storage.put(`claim:${alias}`, claim);
    await storage.put(
      `account-claim:${ACCOUNT}:${REALM}:${createdAt}:${alias}`,
      alias,
    );
  }
  const input = {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: false,
    alias_limit: null,
    mode: "restrict_only",
    plan_revision: 50,
    plan_snapshot_hash: "5".repeat(64),
  };
  const first = await call(runtime, "/plan/reconcile", input);
  assert.equal(first.body.operational_gate_complete, false);
  assert.equal(first.body.changed, 100);
  const second = await call(runtime, "/plan/reconcile", input);
  assert.equal(second.body.operational_gate_complete, false);
  assert.equal(second.body.changed, 1);
  const third = await call(runtime, "/plan/reconcile", input);
  assert.equal(third.body.operational_gate_complete, true);
  for (const alias of ["gate-000", "gate-100"]) {
    assert.equal((await storage.get(`claim:${alias}`)).operational_gate_suspended, true);
    const route = emailDirectory.value(realmEmailRouteKey(DOMAIN, alias));
    assert.equal(route.state, "suspended");
    assert.equal(route.suspension_disposition, "retry");
  }
});

test("unlimited alias policy supports multiple assignments and survives reconciliation", async () => {
  const { runtime } = registry();
  for (const alias of ["unlimited-a", "unlimited-b", "unlimited-c", "unlimited-d"]) {
    const created = await requestAlias(runtime, alias, {
      alias_limit: null,
      idempotency_key: `request-${alias}`,
    });
    assert.equal(created.response.status, 202);
    const approved = await approve(runtime, created.body.request, {
      alias_limit: null,
      idempotency_key: `approve-${alias}`,
    });
    assert.equal(approved.response.status, 200);
  }
  const reconciled = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: null,
    mode: "complete",
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  });
  assert.equal(reconciled.response.status, 200);
  assert.equal(reconciled.body.changed, 0);
  const listed = await call(runtime, "/alias/list", {
    actor: ADMIN,
    account_id: ACCOUNT,
  });
  assert.equal(listed.body.aliases.length, 4);
  assert.ok(listed.body.aliases.every((alias) => alias.status === "active"));
});

test("stale plan work cannot start grace after a newer upgrade fence", async () => {
  const { runtime } = registry();
  const created = await requestAlias(runtime, "monotonic");
  await approve(runtime, created.body.request);
  const upgraded = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: null,
    mode: "complete",
    plan_revision: 12,
    plan_snapshot_hash: "c".repeat(64),
  });
  assert.equal(upgraded.response.status, 200);

  const stale = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: false,
    activation_enabled: true,
    alias_limit: 0,
    mode: "restrict_only",
    plan_revision: 11,
    plan_snapshot_hash: "b".repeat(64),
  });
  assert.equal(stale.response.status, 200);
  assert.equal(stale.body.stale, true);
  assert.equal(stale.body.changed, 0);
  const aliases = await call(runtime, "/alias/list", {
    actor: ADMIN,
    account_id: ACCOUNT,
  });
  assert.equal(aliases.body.aliases[0].status, "active");
  assert.equal(aliases.body.aliases[0].plan_grace_until, undefined);

  const conflict = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: null,
    mode: "restrict_only",
    plan_revision: 12,
    plan_snapshot_hash: "d".repeat(64),
  });
  assert.equal(conflict.response.status, 409);
});

test("customer requests and fresh approvals must match the committed plan fence", async () => {
  const { runtime } = registry();
  await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 3,
    mode: "complete",
    plan_revision: 12,
    plan_snapshot_hash: "c".repeat(64),
  });
  const staleRequest = await requestAlias(runtime, "fenced-request");
  assert.equal(staleRequest.response.status, 409);
  const currentRequest = await requestAlias(runtime, "fenced-request", {
    plan_revision: 12,
    plan_snapshot_hash: "c".repeat(64),
  });
  assert.equal(currentRequest.response.status, 202);
  const staleApproval = await approve(runtime, currentRequest.body.request);
  assert.equal(staleApproval.response.status, 409);
  const currentApproval = await approve(runtime, currentRequest.body.request, {
    plan_revision: 12,
    plan_snapshot_hash: "c".repeat(64),
  });
  assert.equal(currentApproval.response.status, 200);
});

test("durable plan intent converges from the authoritative cell after projection failure", async () => {
  const { runtime, emailDirectory, storage, setAuthoritativePlan, advance } = registry();
  for (const alias of ["converge-a", "converge-b"]) {
    const created = await requestAlias(runtime, alias, {
      idempotency_key: `request-${alias}`,
    });
    await approve(runtime, created.body.request, {
      idempotency_key: `approve-${alias}`,
    });
  }
  const snapshot = {
    account_id: ACCOUNT,
    revision: 15,
    snapshot_hash: "f".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 1 },
  };
  await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 1,
    mode: "restrict_only",
    plan_revision: snapshot.revision,
    plan_snapshot_hash: snapshot.snapshot_hash,
  });
  emailDirectory.failPuts = 1;
  const failed = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    activation_enabled: true,
    alias_limit: 1,
    mode: "complete",
    plan_revision: snapshot.revision,
    plan_snapshot_hash: snapshot.snapshot_hash,
  });
  assert.equal(failed.response.status, 502);
  assert.equal((await storage.get(`plan-intent:${ACCOUNT}`)).state, "cell_committed");

  setAuthoritativePlan(snapshot);
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal(await storage.get(`plan-intent:${ACCOUNT}`), undefined);
  const fence = await storage.get(`plan-fence:${ACCOUNT}`);
  assert.equal(fence.committed_revision, 15);
  const aliases = await call(runtime, "/alias/list", {
    actor: ADMIN,
    account_id: ACCOUNT,
  });
  assert.equal(aliases.body.aliases.filter((alias) => alias.status === "active_grace").length, 1);
});

test("plan reconciliation is bounded and continues from a durable account cursor", async () => {
  const { runtime, storage, setAuthoritativePlan, advance } = registry();
  await call(runtime, "/reserved/list", { actor: ADMIN });
  for (let index = 0; index < 101; index++) {
    const alias = `bulk-${String(index).padStart(3, "0")}`;
    const createdAt = new Date(Date.UTC(2026, 0, 1, 0, 0, index)).toISOString();
    const claim = {
      claim_id: `era_${"c".repeat(13)}${String(index).padStart(3, "2")}`,
      alias,
      domain: DOMAIN,
      skeleton: realmEmailAliasSkeleton(alias),
      account_id: ACCOUNT,
      realm_id: REALM,
      request_id: null,
      assignment_kind: "customer",
      assignment_revision: 1,
      admin_suspended: false,
      plan_suspended: false,
      plan_grace_until: null,
      created_at: createdAt,
      updated_at: createdAt,
      retired_at: null,
    };
    await storage.put(`claim:${alias}`, claim);
    await storage.put(
      `account-claim:${ACCOUNT}:${REALM}:${createdAt}:${alias}`,
      alias,
    );
  }
  const snapshot = {
    account_id: ACCOUNT,
    revision: 30,
    snapshot_hash: "3".repeat(64),
    features: [],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 0 },
  };
  setAuthoritativePlan(snapshot);
  const first = await call(runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: false,
    activation_enabled: true,
    alias_limit: 0,
    mode: "complete",
    plan_revision: snapshot.revision,
    plan_snapshot_hash: snapshot.snapshot_hash,
  });
  assert.equal(first.response.status, 200);
  assert.equal(first.body.changed, 100);
  assert.equal(first.body.complete, false);
  assert.ok((await storage.get(`plan-intent:${ACCOUNT}`)).claim_cursor);
  assert.equal((await storage.get("claim:bulk-100")).plan_grace_until, null);

  advance(1_001);
  await runtime.alarm();
  assert.equal(await storage.get(`plan-intent:${ACCOUNT}`), undefined);
  assert.equal((await storage.get("claim:bulk-100")).plan_grace_until !== null, true);
  assert.equal((await storage.get(`plan-fence:${ACCOUNT}`)).committed_revision, 30);
  const pageScans = storage.listCalls.filter((entry) =>
    entry.prefix === `account-claim:${ACCOUNT}:`
  );
  assert.ok(pageScans.every((entry) => entry.limit === 101));
});

test("approval recovery alarm honors a gate flip after an applied edge write", async () => {
  const { runtime, emailDirectory, storage, env, advance } = registry();
  const created = await requestAlias(runtime, "gate-recovery");
  // Alias publication succeeds, then canonical publication fails. The durable
  // request must remain provisioning even though an applied alias route exists.
  emailDirectory.failAtPut = emailDirectory.putCount + 2;
  const failed = await approve(runtime, created.body.request);
  assert.equal(failed.response.status, 502);
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "gate-recovery")).state,
    "applied",
  );
  env.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED = "false";
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal(
    emailDirectory.value(realmEmailRouteKey(DOMAIN, "gate-recovery")).state,
    "suspended",
  );
  assert.equal(
    (await storage.get(`request:${created.body.request.id}`)).status,
    "approved",
  );
  const claim = await storage.get("claim:gate-recovery");
  assert.equal(claim.customer_activation_intent, false);
  assert.equal(claim.plan_suspended, false);
  assert.equal(claim.operational_gate_suspended, true);
});

test("over-limit approval recovery uses O(1) allocation state and graces the recovering claim", async () => {
  const {
    runtime,
    emailDirectory,
    storage,
    setAuthoritativePlan,
    advance,
  } = registry();
  const first = await requestAlias(runtime, "recovery-oldest", {
    alias_limit: null,
  });
  assert.equal((await approve(runtime, first.body.request, {
    alias_limit: null,
  })).response.status, 200);

  const recovering = await requestAlias(runtime, "recovery-newest", {
    alias_limit: null,
  });
  emailDirectory.failPuts = 1;
  assert.equal((await approve(runtime, recovering.body.request, {
    alias_limit: null,
  })).response.status, 502);
  assert.equal(
    (await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`))
      .customer_allocated,
    2,
  );
  setAuthoritativePlan({
    account_id: ACCOUNT,
    revision: 8,
    snapshot_hash: "8".repeat(64),
    features: [REALM_EMAIL_ALIAS_FEATURE],
    limits: { [REALM_EMAIL_ALIAS_LIMIT]: 1 },
  });

  storage.listCalls.length = 0;
  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal(storage.listCalls.some((call) =>
    call.prefix === `account-claim:${ACCOUNT}:${REALM}:`
  ), false, "approval recovery must not scan the realm claim index");
  assert.equal(storage.listCalls.some((call) =>
    call.prefix === `account-claim:${ACCOUNT}:`
  ), false, "approval recovery must not scan the account claim index");

  const claim = await storage.get("claim:recovery-newest");
  assert.equal(claim.customer_activation_intent, false);
  assert.equal(claim.plan_suspended, false);
  assert.ok(claim.plan_grace_until);
  assert.ok(Date.parse(claim.plan_grace_until) > Date.parse(claim.updated_at));
  const request = await storage.get(`request:${recovering.body.request.id}`);
  assert.equal(request.status, "approved");
  const listed = await call(runtime, "/alias/list", {
    actor: ADMIN,
    account_id: ACCOUNT,
  });
  assert.equal(
    listed.body.aliases.find((alias) => alias.alias === "recovery-newest")
      .status,
    "active_grace",
  );
});

test("approval reason is required and routine mutations use O(1) counters", async () => {
  const { runtime, storage } = registry();
  const missing = await requestAlias(runtime, "reasonless");
  for (const [reason, key] of [[undefined, "missing-reason"], ["   ", "blank-reason"]]) {
    const rejected = await approve(runtime, missing.body.request, {
      idempotency_key: key,
      reason,
    });
    assert.equal(rejected.response.status, 400);
    assert.match(rejected.body.error, /reason is required/);
  }
  storage.listCalls.length = 0;
  const approved = await approve(runtime, missing.body.request);
  assert.equal(approved.response.status, 200);
  assert.equal(storage.listCalls.some((call) => call.prefix === "claim:"), false);
  assert.equal(storage.listCalls.some((call) =>
    call.prefix === `account-claim:${ACCOUNT}:${REALM}:`
  ), false);
  const usage = await storage.get(`claim-usage-realm:${ACCOUNT}:${REALM}`);
  assert.deepEqual({
    open_requests: usage.open_requests,
    pending_review: usage.pending_review,
    provisioning: usage.provisioning,
    customer_allocated: usage.customer_allocated,
  }, {
    open_requests: 0,
    pending_review: 0,
    provisioning: 0,
    customer_allocated: 1,
  });
});

test("global administrator scans are hard capped", async () => {
  const { runtime, storage } = registry();
  await call(runtime, "/request/admin-list", { actor: ADMIN });
  await call(runtime, "/alias/list", { actor: ADMIN });
  await call(runtime, "/reserved/list", { actor: ADMIN });
  await call(runtime, "/audit/list", { actor: ADMIN, limit: 500 });
  for (const prefix of ["request:", "claim:", "reserved:", "audit:"]) {
    const scan = storage.listCalls.find((entry) => entry.prefix === prefix);
    assert.ok(scan, `missing ${prefix} scan`);
    assert.ok(scan.limit <= 501, `${prefix} scan was not bounded`);
  }
  const auditScan = storage.listCalls.find((entry) => entry.prefix === "audit:");
  assert.equal(auditScan.reverse, true);
});

test("administrator list cursors make rows beyond the cap reachable", async () => {
  const { runtime, storage } = registry();
  await call(runtime, "/reserved/list", { actor: ADMIN });
  for (let index = 0; index < 501; index++) {
    const id = `synthetic-${String(index).padStart(4, "0")}`;
    await storage.put(`request:${id}`, {
      id,
      alias: `alias-${index}`,
      account_id: ACCOUNT,
      realm_id: REALM,
      status: "pending_review",
      requested_at: new Date(Date.UTC(2026, 0, 1, 0, 0, index)).toISOString(),
    });
  }
  const first = await call(runtime, "/request/admin-list", { actor: ADMIN });
  assert.equal(first.body.requests.length, 500);
  assert.equal(first.body.truncated, true);
  assert.ok(first.body.next_cursor);
  const second = await call(runtime, "/request/admin-list", {
    actor: ADMIN,
    cursor: first.body.next_cursor,
  });
  assert.equal(second.body.requests.length, 1);
  assert.equal(second.body.truncated, false);
  assert.equal(second.body.next_cursor, null);
});

test("slow cell projection cannot head-of-line block route fallback", async () => {
  const { runtime, blockNextProjection, cellClaims } = registry();
  const activeRequest = await requestAlias(runtime, "route-ready");
  await approve(runtime, activeRequest.body.request);
  const slowRequest = await requestAlias(runtime, "slow-approval", {
    idempotency_key: "request-slow-approval",
  });
  const blocker = blockNextProjection();
  const pendingApproval = approve(runtime, slowRequest.body.request, {
    idempotency_key: "approve-slow-approval",
  });
  await blocker.started;

  const fallback = await Promise.race([
    call(runtime, "/route/get", {
      domain: DOMAIN,
      realm_label: "route-ready",
    }),
    new Promise((_, reject) => setTimeout(
      () => reject(new Error("route fallback was head-of-line blocked")),
      100,
    )),
  ]);
  assert.equal(fallback.response.status, 200);
  assert.equal(fallback.body.state, "applied");
  assert.equal(cellClaims.size, 1, "fallback must not POST or verify a cell fence");
  blocker.release();
  assert.equal((await pendingApproval).response.status, 200);
});

test("one poison approval is backed off without starving later due work", async () => {
  const {
    runtime,
    emailDirectory,
    storage,
    advance,
    failClaimProjection,
  } = registry();
  const poison = await requestAlias(runtime, "poison-due");
  emailDirectory.failPuts = 1;
  assert.equal((await approve(runtime, poison.body.request)).response.status, 502);
  const healthy = await requestAlias(runtime, "healthy-due", {
    idempotency_key: "request-healthy-due",
  });
  emailDirectory.failPuts = 1;
  assert.equal((await approve(runtime, healthy.body.request, {
    idempotency_key: "approve-healthy-due",
  })).response.status, 502);
  const poisonClaim = await storage.get("claim:poison-due");
  failClaimProjection(poisonClaim.claim_id);

  advance(5 * 60 * 1_000 + 1_000);
  await runtime.alarm();
  assert.equal(
    (await storage.get(`request:${poison.body.request.id}`)).status,
    "provisioning",
  );
  assert.equal((await storage.get("claim:poison-due")).approval_failure_count, 1);
  assert.equal(
    (await storage.get(`request:${healthy.body.request.id}`)).status,
    "approved",
  );
});

test("concurrent scoped mutations reserve unique global audit revisions", async () => {
  const { runtime, storage, blockNextProjection } = registry();
  const requested = await requestAlias(runtime, "slow-meta");
  const blocker = blockNextProjection();
  const pending = approve(runtime, requested.body.request);
  await blocker.started;
  const parallel = await call(runtime, "/reserved/create", {
    actor: ADMIN,
    name: "parallel-name",
    category: "platform_brand",
    reason: "metadata concurrency test",
    internal_assignable: false,
    idempotency_key: "parallel-reservation",
  });
  assert.equal(parallel.response.status, 201);
  blocker.release();
  assert.equal((await pending).response.status, 200);

  const meta = await storage.get("meta");
  const audits = [...(await storage.list({ prefix: "audit:" })).values()];
  const sequences = audits.map((event) => event.sequence);
  assert.equal(new Set(sequences).size, sequences.length);
  assert.equal(meta.audit_sequence, Math.max(...sequences));
  assert.equal(meta.registry_revision, Math.max(
    ...audits.map((event) => event.registry_revision),
  ));
});

test("counter rebuild installs its fence without regressing concurrent metadata", async () => {
  const { runtime, storage } = registry();
  await call(runtime, "/reserved/list", { actor: ADMIN });
  const blocker = storage.blockNextTransactionPut(
    "meta",
    (value) => value?.pending_counter_state === "rebuilding",
  );
  const rebuilding = call(runtime, "/counter/rebuild", {
    actor: ADMIN,
    reason: "force a metadata interleaving at rebuild installation",
    idempotency_key: "counter-rebuild-metadata-race",
  });
  await blocker.started;

  let parallelSettled = false;
  const parallel = call(runtime, "/reserved/create", {
    actor: ADMIN,
    name: "parallel-rebuild",
    category: "platform_brand",
    reason: "must wait behind the rebuild metadata reservation",
    internal_assignable: false,
    idempotency_key: "parallel-rebuild-reservation",
  }).then((result) => {
    parallelSettled = true;
    return result;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(
    parallelSettled,
    false,
    "a normal mutation entered between rebuild reservation and installation",
  );

  blocker.release();
  assert.equal((await rebuilding).response.status, 202);
  assert.equal((await parallel).response.status, 201);
  const later = await call(runtime, "/reserved/create", {
    actor: ADMIN,
    name: "after-rebuild",
    category: "platform_brand",
    reason: "prove the next revision cannot reuse the parallel audit slot",
    internal_assignable: false,
    idempotency_key: "after-rebuild-reservation",
  });
  assert.equal(later.response.status, 201);

  const meta = await storage.get("meta");
  const audits = [...(await storage.list({ prefix: "audit:" })).values()]
    .sort((left, right) => left.sequence - right.sequence);
  assert.deepEqual(audits.map((event) => event.sequence), [1, 2, 3, 4]);
  assert.deepEqual(
    audits.map((event) => event.registry_revision),
    [1, 2, 3, 4],
  );
  assert.equal(audits[1].action, "alias.pending_counters_rebuild_requested");
  assert.equal(audits[2].action, "reserved.created");
  assert.equal(audits[2].target, "parallel-rebuild");
  assert.equal(audits[3].action, "reserved.created");
  assert.equal(audits[3].target, "after-rebuild");
  assert.equal(meta.audit_sequence, 4);
  assert.equal(meta.registry_revision, 4);
  assert.equal(meta.pending_counter_state, "rebuilding");
  assert.ok(await storage.get("pending-counter-migration"));
});

test("alarm scheduling cannot erase a concurrently committed due row", async () => {
  const { runtime, storage } = registry();
  await call(runtime, "/reserved/list", { actor: ADMIN });
  const blocker = storage.blockNextDeleteAlarm();
  const staleSchedule = runtime.scheduleNextAlarm();
  await blocker.started;

  const dueAt = Date.UTC(2026, 7, 1, 1, 0, 0);
  const intent = {
    account_id: ACCOUNT,
    retry_at_ms: dueAt,
  };
  await storage.put(`plan-intent:${ACCOUNT}`, intent);
  await storage.put(
    `plan-due:${String(dueAt).padStart(16, "0")}:${ACCOUNT}`,
    ACCOUNT,
  );
  const freshSchedule = runtime.scheduleNextAlarm();
  blocker.release();
  await Promise.all([staleSchedule, freshSchedule]);
  assert.equal(storage.alarmAt, dueAt);
});
