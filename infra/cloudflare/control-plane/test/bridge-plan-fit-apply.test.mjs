import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { handleInternalBridgeRequest } from "../src/bridge.mjs";

class KV {
  constructor(entries) {
    this.entries = new Map(Object.entries(entries));
  }

  async get(key, options) {
    const value = this.entries.get(key);
    if (value == null) return null;
    if (options?.type === "json") return value;
    return JSON.stringify(value);
  }
}

function snapshotHash(snapshot) {
  const ordered = (values) => Object.fromEntries(
    Object.keys(values).sort().map((key) => [key, values[key]]),
  );
  return createHash("sha256").update(JSON.stringify({
    plan: snapshot.plan,
    limits: ordered(snapshot.limits),
    policies: ordered(snapshot.policies),
    features: [...snapshot.features].sort(),
  })).digest("hex");
}

function withSnapshotHash(snapshot) {
  return { ...snapshot, snapshot_hash: snapshotHash(snapshot) };
}

const target = Object.freeze(withSnapshotHash({
  revision: 7,
  plan: "personal",
  limits: {
    agents: 10,
    agent_email_realm_aliases_per_realm: 1,
    // A limit alone never enables the feature. The authority must prepare the
    // effective disabled maximum of zero, while the cell still persists the
    // exact resolved snapshot.
    agent_email_custom_domains_per_account: 1,
  },
  policies: {},
  features: ["agent_email_realm_alias"],
}));

const appliedSnapshot = Object.freeze({
  account_id: "acct_1",
  revision: target.revision,
  snapshot_hash: target.snapshot_hash,
  plan: target.plan,
  limits: target.limits,
  policies: target.policies,
  features: target.features,
  applied_at: "2026-08-17T20:00:00Z",
});

const currentSnapshot = Object.freeze(withSnapshotHash({
  account_id: "acct_1",
  revision: 6,
  plan: "professional",
  limits: {
    agents: 100,
    agent_email_realm_aliases_per_realm: 1,
    agent_email_custom_domains_per_account: 0,
  },
  policies: {},
  features: [],
  applied_at: "2026-08-16T20:00:00Z",
}));

function environment() {
  return {
    INTERNAL_BRIDGE_TOKEN: "bridge-token",
    DIRECTORY: new KV({
      "acct:acct_1": { cell: "cell-a" },
      "cell:cell-a": {
        endpoint: "https://cell.example/",
        provision_token: "cell-token",
      },
    }),
  };
}

function request(body = { schema_version: "witself.v0", target }) {
  return new Request(
    "https://self.witwave.ai/v1/internal/accounts/acct_1:plan-fit-apply",
    {
      method: "POST",
      headers: {
        Authorization: "Bearer bridge-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify(body),
    },
  );
}

function authorityNamespace(
  label,
  events,
  prepareResult,
  expected = target,
  requests = null,
) {
  return {
    idFromName(name) {
      assert.equal(name, "global");
      return name;
    },
    get() {
      return {
        fetch: async (_url, init) => {
          const body = JSON.parse(init.body);
          events.push(`${label}:${body.mode}`);
          requests?.push({ label, body });
          if (body.mode === "prepare") {
            const maximum = label === "alias"
              ? (body.feature_enabled ? body.alias_limit : 0)
              : (body.feature_enabled ? body.domain_limit : 0);
            const dimension = label === "alias"
              ? "agent_email_realm_aliases_per_realm"
              : "agent_email_custom_domains_per_account";
            const prepared = typeof prepareResult === "function"
              ? prepareResult(body)
              : prepareResult;
            if (prepared) return prepared;
            const fit = {
              complete: true,
              dimension,
              maximum,
              scanned_subject_count: label === "alias" ? 0 : 1,
              scanned_allocation_count: 0,
              authority_revision: 4,
              ...(label === "alias"
                ? { highest_used: 0, over_limit_count: 0 }
                : { used: 0, over_limit_count: 0 }),
            };
            return Response.json({
              schema_version: label === "alias"
                ? "witself.realm-email-alias.v1"
                : "witself.agent-email-domain.v1",
              account_id: "acct_1",
              mode: "prepare",
              plan_revision: expected.revision,
              plan_snapshot_hash: expected.snapshot_hash,
              complete: true,
              prepared: true,
              pending: true,
              stale: false,
              fit,
              registry_revision: fit.authority_revision,
            });
          }
          return Response.json({
            schema_version: label === "alias"
              ? "witself.realm-email-alias.v1"
              : "witself.agent-email-domain.v1",
            account_id: "acct_1",
            mode: "complete",
            changed: 0,
            stale: false,
            complete: true,
            registry_revision: 5,
            ...(body.recover_pending_revision !== undefined &&
                body.plan_revision < body.recover_pending_revision
              ? { recovered: true }
              : {}),
          });
        },
      };
    },
  };
}

function recoveringPreparedAuthorityNamespace(label, events, pendingTarget) {
  let pending = {
    revision: pendingTarget.revision,
    snapshot_hash: pendingTarget.snapshot_hash,
  };
  const schema = label === "alias"
    ? "witself.realm-email-alias.v1"
    : "witself.agent-email-domain.v1";
  const dimension = label === "alias"
    ? "agent_email_realm_aliases_per_realm"
    : "agent_email_custom_domains_per_account";
  return {
    idFromName(name) {
      assert.equal(name, "global");
      return name;
    },
    get() {
      return {
        fetch: async (_url, init) => {
          const body = JSON.parse(init.body);
          events.push(`${label}:${body.mode}`);
          if (body.mode === "prepare") {
            if (pending && body.plan_revision > pending.revision) {
              return Response.json({
                schema_version: schema,
                error: `${label} plan-fit preparation is fenced`,
                code: "plan_fit_prepared_fence_conflict",
                account_id: "acct_1",
                mode: "prepare",
                plan_revision: body.plan_revision,
                plan_snapshot_hash: body.plan_snapshot_hash,
                prepared: false,
                pending: true,
                stale: false,
                complete: false,
                pending_state: "awaiting_cell",
                pending_plan_revision: pending.revision,
                pending_plan_snapshot_hash: pending.snapshot_hash,
              }, { status: 409 });
            }
            pending = {
              revision: body.plan_revision,
              snapshot_hash: body.plan_snapshot_hash,
            };
            const maximum = label === "alias"
              ? (body.feature_enabled ? body.alias_limit : 0)
              : (body.feature_enabled ? body.domain_limit : 0);
            const fit = {
              complete: true,
              dimension,
              maximum,
              scanned_subject_count: label === "alias" ? 0 : 1,
              scanned_allocation_count: 0,
              authority_revision: 4,
              ...(label === "alias"
                ? { highest_used: 0, over_limit_count: 0 }
                : { used: 0, over_limit_count: 0 }),
            };
            return Response.json({
              schema_version: schema,
              account_id: "acct_1",
              mode: "prepare",
              plan_revision: body.plan_revision,
              plan_snapshot_hash: body.plan_snapshot_hash,
              complete: true,
              prepared: true,
              pending: true,
              stale: false,
              fit,
              registry_revision: fit.authority_revision,
            });
          }
          if (body.recover_pending_revision === pending?.revision &&
              body.recover_pending_snapshot_hash === pending?.snapshot_hash) {
            pending = null;
          } else if (body.plan_revision === pending?.revision &&
              body.plan_snapshot_hash === pending?.snapshot_hash) {
            pending = null;
          }
          return Response.json({
            schema_version: schema,
            account_id: "acct_1",
            mode: "complete",
            changed: 0,
            stale: false,
            complete: true,
            registry_revision: 5,
            ...(body.recover_pending_revision !== undefined &&
                body.plan_revision < body.recover_pending_revision
              ? { recovered: true }
              : {}),
          });
        },
      };
    },
  };
}

function appliedResult(expected = target) {
  const snapshot = expected === target
    ? appliedSnapshot
    : {
      account_id: "acct_1",
      revision: expected.revision,
      snapshot_hash: expected.snapshot_hash,
      plan: expected.plan,
      limits: expected.limits,
      policies: expected.policies,
      features: expected.features,
      applied_at: "2026-08-17T20:00:00Z",
    };
  return {
    schema_version: "witself.v0",
    state: "applied",
    account_id: "acct_1",
    target_revision: expected.revision,
    target_plan: expected.plan,
    target_snapshot_hash: expected.snapshot_hash,
    violations: [],
    applied_snapshot: snapshot,
  };
}

function blockedResult() {
  return {
    schema_version: "witself.v0",
    state: "blocked",
    account_id: "acct_1",
    target_revision: target.revision,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: [{
      code: "limit_exceeded",
      dimension: "agents",
      scope: "account",
      used: 12,
      max: 10,
      subject_count: 1,
    }],
    current_snapshot: currentSnapshot,
  };
}

test("atomic plan apply prepares authorities, commits the cell, then completes", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = authorityNamespace("alias", events);
  let cellBody;
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (url, init) => {
      assert.equal(url, "https://cell.example/v1/accounts/acct_1:plan-fit-apply");
      assert.equal(init.headers.Authorization, "Bearer cell-token");
      cellBody = JSON.parse(new TextDecoder().decode(init.body));
      events.push("cell");
      return Response.json(appliedResult());
    },
  );

  assert.equal(response.status, 200, await response.clone().text());
  assert.deepEqual(await response.json(), appliedResult());
  assert.deepEqual(cellBody, { schema_version: "witself.v0", target });
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "cell",
    "alias:complete",
    "domain:complete",
  ]);
});

test("atomic plan apply verifies the Go snapshot hash encoding", async () => {
  const encoded = {
    revision: 9,
    plan: "personal<\u2028&\u2029>",
    snapshot_hash:
      "a24f5d7725ddfe4836252dbbde8f9761983c78805bee265b74c20a9be5785a62",
    limits: {
      agent_email_custom_domains_per_account: 1,
      agent_email_realm_aliases_per_realm: 1,
      agents: 10,
    },
    policies: { agent_email_retention_days: 30 },
    features: ["agent_email_realm_alias"],
  };
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(
    "domain",
    events,
    undefined,
    encoded,
  );
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    undefined,
    encoded,
  );
  const response = await handleInternalBridgeRequest(
    request({ schema_version: "witself.v0", target: encoded }),
    env,
    async () => Response.json(appliedResult(encoded)),
  );
  assert.equal(response.status, 200, await response.clone().text());
  assert.deepEqual(await response.json(), appliedResult(encoded));
});

test("atomic plan apply prepares unlimited entitled authorities", async () => {
  const unlimited = withSnapshotHash({
    revision: 8,
    plan: "enterprise",
    limits: { agents: 2500 },
    policies: {},
    features: [
      "agent_email_realm_alias",
      "agent_email_custom_domain",
    ],
  });
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(
    "domain",
    events,
    (body) => {
      assert.equal(body.feature_enabled, true);
      assert.equal(body.domain_limit, null);
      return null;
    },
    unlimited,
  );
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    (body) => {
      assert.equal(body.feature_enabled, true);
      assert.equal(body.alias_limit, null);
      return null;
    },
    unlimited,
  );
  const response = await handleInternalBridgeRequest(
    request({ schema_version: "witself.v0", target: unlimited }),
    env,
    async (url) => {
      assert.equal(url, "https://cell.example/v1/accounts/acct_1:plan-fit-apply");
      events.push("cell");
      return Response.json(appliedResult(unlimited));
    },
  );

  assert.equal(response.status, 200, await response.clone().text());
  assert.deepEqual(await response.json(), appliedResult(unlimited));
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "cell",
    "alias:complete",
    "domain:complete",
  ]);
});

test("authority refusal compensates an earlier prepare and never calls cell apply", async () => {
  const events = [];
  const authorityRequests = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(
    "domain",
    events,
    undefined,
    target,
    authorityRequests,
  );
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    () => Response.json({
      schema_version: "witself.realm-email-alias.v1",
      account_id: "acct_1",
      mode: "prepare",
      plan_revision: target.revision,
      plan_snapshot_hash: target.snapshot_hash,
      complete: true,
      prepared: false,
      pending: false,
      stale: false,
      code: "plan_fit_failed",
      fit: {
        complete: true,
        dimension: "agent_email_realm_aliases_per_realm",
        maximum: 1,
        highest_used: 3,
        over_limit_count: 2,
        scanned_subject_count: 2,
        scanned_allocation_count: 3,
      },
    }, { status: 409 }),
    target,
    authorityRequests,
  );
  let calls = 0;
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (url, init) => {
      calls += 1;
      assert.equal(url, "https://cell.example/v1/accounts/acct_1:plan");
      assert.equal(init.method, "GET");
      return Response.json(currentSnapshot);
    },
  );

  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    state: "blocked",
    account_id: "acct_1",
    target_revision: target.revision,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: [{
      code: "limit_exceeded",
      dimension: "agent_email_realm_aliases_per_realm",
      scope: "realm",
      used: 3,
      max: 1,
      subject_count: 2,
    }],
    current_snapshot: currentSnapshot,
  });
  assert.equal(calls, 1);
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "domain:complete",
  ]);
  for (const entry of authorityRequests.filter(({ body }) =>
    body.mode === "complete"
  )) {
    assert.equal(entry.body.plan_revision, currentSnapshot.revision);
    assert.equal(entry.body.plan_snapshot_hash, currentSnapshot.snapshot_hash);
    assert.equal(entry.body.recover_pending_revision, target.revision);
    assert.equal(
      entry.body.recover_pending_snapshot_hash,
      target.snapshot_hash,
    );
  }
});

test("authority refusal compensates before rejecting a newer cell fence", async () => {
  const newer = withSnapshotHash({
    ...currentSnapshot,
    revision: target.revision + 1,
    applied_at: "2026-08-17T21:00:00Z",
  });
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    () => Response.json({
      schema_version: "witself.realm-email-alias.v1",
      account_id: "acct_1",
      mode: "prepare",
      plan_revision: target.revision,
      plan_snapshot_hash: target.snapshot_hash,
      complete: true,
      prepared: false,
      pending: false,
      stale: false,
      code: "plan_fit_failed",
      fit: {
        complete: true,
        dimension: "agent_email_realm_aliases_per_realm",
        maximum: 1,
        highest_used: 3,
        over_limit_count: 2,
        scanned_subject_count: 2,
        scanned_allocation_count: 3,
      },
    }, { status: 409 }),
  );
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (_url, init) => {
      assert.equal(init.method, "GET");
      return Response.json(newer);
    },
  );

  assert.equal(response.status, 409);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "cell plan snapshot supersedes plan-fit target",
  });
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "domain:complete",
  ]);
});

test("authority outage compensates only the authority that prepared", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = {
    idFromName: () => "global",
    get: () => ({
      fetch: async () => {
        events.push("alias:prepare");
        throw new Error("alias authority unavailable");
      },
    }),
  };
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (_url, init) => {
      assert.equal(init.method, "GET");
      return Response.json(currentSnapshot);
    },
  );
  assert.equal(response.status, 502);
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "domain:complete",
  ]);
});

test("cell fit refusal compensates both exact prepared authority fences", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = authorityNamespace("alias", events);
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async () => {
      events.push("cell");
      return Response.json(blockedResult());
    },
  );

  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), blockedResult());
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "cell",
    "alias:complete",
    "domain:complete",
  ]);
});

test("lost cell response recovers exact applied fence without compensating", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = authorityNamespace("alias", events);
  let calls = 0;
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (url, init) => {
      calls += 1;
      if (init.method === "POST") {
        events.push("cell-committed-response-lost");
        throw new TypeError("response lost after commit");
      }
      assert.equal(url, "https://cell.example/v1/accounts/acct_1:plan");
      return Response.json(appliedSnapshot);
    },
  );

  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), appliedResult());
  assert.equal(calls, 2);
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "cell-committed-response-lost",
    "alias:complete",
    "domain:complete",
  ]);
});

test("newer cell fence compensates a prepared older target before relaying conflict", async () => {
  const newer = withSnapshotHash({
    ...currentSnapshot,
    revision: target.revision + 1,
    applied_at: "2026-08-17T21:00:00Z",
  });
  const events = [];
  const authorityRequests = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(
    "domain",
    events,
    undefined,
    target,
    authorityRequests,
  );
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    undefined,
    target,
    authorityRequests,
  );
  let calls = 0;
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (_url, init) => {
      calls += 1;
      if (init.method === "POST") {
        return Response.json({
          schema_version: "witself.v0",
          error: "stale or conflicting plan-fit apply",
        }, { status: 409 });
      }
      return Response.json(newer);
    },
  );

  assert.equal(response.status, 409);
  assert.equal(calls, 2);
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "cell",
    "alias:complete",
    "domain:complete",
  ].filter((event) => event !== "cell"));
  for (const entry of authorityRequests.filter(({ body }) =>
    body.mode === "complete"
  )) {
    assert.equal(entry.body.plan_revision, newer.revision);
    assert.equal(entry.body.plan_snapshot_hash, newer.snapshot_hash);
    assert.equal(entry.body.recover_pending_revision, target.revision);
    assert.equal(
      entry.body.recover_pending_snapshot_hash,
      target.snapshot_hash,
    );
  }
});

test("invalid authority completion acknowledgement fails closed", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = {
    idFromName: () => "global",
    get: () => ({
      fetch: async (_url, init) => {
        const body = JSON.parse(init.body);
        events.push(`alias:${body.mode}`);
        if (body.mode === "prepare") {
          return authorityNamespace("alias", [], undefined).get().fetch(
            _url,
            init,
          );
        }
        return Response.json({ complete: true });
      },
    }),
  };
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async () => Response.json(appliedResult()),
  );
  assert.equal(response.status, 502);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "agent email plan-fit authority reconciliation failed",
  });
});

test("exact conditional replay finishes a lost authority completion", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  const alias = authorityNamespace("alias", events);
  let loseCompletion = true;
  env.REALM_EMAIL_ALIASES = {
    idFromName: alias.idFromName,
    get: () => ({
      fetch: async (url, init) => {
        const body = JSON.parse(init.body);
        if (body.mode === "complete" && loseCompletion) {
          loseCompletion = false;
          events.push("alias:complete-lost");
          return Response.json({ complete: true });
        }
        return alias.get().fetch(url, init);
      },
    }),
  };
  let cellCalls = 0;
  const cell = async () => {
    cellCalls += 1;
    return Response.json(appliedResult());
  };

  const first = await handleInternalBridgeRequest(request(), env, cell);
  assert.equal(first.status, 502);
  const second = await handleInternalBridgeRequest(request(), env, cell);
  assert.equal(second.status, 200, await second.clone().text());
  assert.deepEqual(await second.json(), appliedResult());
  assert.equal(cellCalls, 2);
  assert.deepEqual(events, [
    "domain:prepare",
    "alias:prepare",
    "alias:complete-lost",
    "domain:prepare",
    "alias:prepare",
    "alias:complete",
    "domain:complete",
  ]);
});

test("new target recovers older prepared authority fences before cell apply", async () => {
  const next = Object.freeze({ ...target, revision: target.revision + 1 });
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = recoveringPreparedAuthorityNamespace(
    "domain",
    events,
    target,
  );
  env.REALM_EMAIL_ALIASES = recoveringPreparedAuthorityNamespace(
    "alias",
    events,
    target,
  );
  let cellCalls = 0;
  const response = await handleInternalBridgeRequest(
    request({ schema_version: "witself.v0", target: next }),
    env,
    async (_url, init) => {
      cellCalls += 1;
      if (init.method === "GET") return Response.json(currentSnapshot);
      events.push("cell");
      return Response.json(appliedResult(next));
    },
  );

  assert.equal(response.status, 200, await response.clone().text());
  assert.deepEqual(await response.json(), appliedResult(next));
  assert.equal(cellCalls, 3);
  assert.deepEqual(events, [
    "domain:prepare",
    "domain:complete",
    "domain:prepare",
    "alias:prepare",
    "domain:complete",
    "alias:complete",
    "domain:prepare",
    "alias:prepare",
    "cell",
    "alias:complete",
    "domain:complete",
  ]);
});

test("tampered current snapshot cannot drive authority compensation", async () => {
  const events = [];
  const env = environment();
  env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
  env.REALM_EMAIL_ALIASES = authorityNamespace(
    "alias",
    events,
    () => Response.json({
      schema_version: "witself.realm-email-alias.v1",
      account_id: "acct_1",
      mode: "prepare",
      plan_revision: target.revision,
      plan_snapshot_hash: target.snapshot_hash,
      complete: true,
      prepared: false,
      pending: false,
      stale: false,
      code: "plan_fit_failed",
      fit: {
        complete: true,
        dimension: "agent_email_realm_aliases_per_realm",
        maximum: 1,
        highest_used: 3,
        over_limit_count: 2,
        scanned_subject_count: 2,
        scanned_allocation_count: 3,
      },
    }, { status: 409 }),
  );
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async () => Response.json({
      ...currentSnapshot,
      plan: "tampered",
    }),
  );
  assert.equal(response.status, 502);
  assert.deepEqual(events, ["domain:prepare", "alias:prepare"]);
});

test("atomic plan apply rejects malformed targets before any authority or cell call", async (t) => {
  for (const [name, invalid] of [
    ["zero revision", { ...target, revision: 0 }],
    ["hash does not match payload", {
      ...target,
      snapshot_hash: "0".repeat(64),
    }],
    ["duplicate feature", {
      ...target,
      features: ["agent_email_realm_alias", "agent_email_realm_alias"],
    }],
    ["unknown feature", {
      ...target,
      features: ["agent_email_realm_alias", "typo_feature"],
    }],
    ["unknown limit", {
      ...target,
      limits: { ...target.limits, typo_limit: 1 },
    }],
    ["non-integer policy", {
      ...target,
      policies: { agent_email_retention_days: 30.5 },
    }],
    ["unsupported policy value", {
      ...target,
      policies: { agent_email_entitlement_version: 2 },
    }],
  ]) {
    await t.test(name, async () => {
      const events = [];
      const env = environment();
      env.AGENT_EMAIL_DOMAINS = authorityNamespace("domain", events);
      env.REALM_EMAIL_ALIASES = authorityNamespace("alias", events);
      const response = await handleInternalBridgeRequest(
        request({ schema_version: "witself.v0", target: invalid }),
        env,
        async () => assert.fail("cell must not be called"),
      );
      assert.equal(response.status, 400);
      assert.deepEqual(events, []);
    });
  }
});

test("atomic plan apply requires both global authorities before reading the cell", async () => {
  const response = await handleInternalBridgeRequest(
    request(),
    environment(),
    async () => assert.fail("cell must not be called"),
  );
  assert.equal(response.status, 502);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "agent email plan-fit authority is unavailable",
  });
});

test("atomic plan apply streams and rejects an undeclared oversized body", async () => {
  let pulls = 0;
  const body = new ReadableStream({
    pull(controller) {
      pulls += 1;
      if (pulls === 1) {
        controller.enqueue(new Uint8Array(64 * 1024));
      } else if (pulls === 2) {
        controller.enqueue(new Uint8Array(1));
      } else {
        assert.fail("bridge must stop consuming after the first excess byte");
      }
    },
  }, { highWaterMark: 0 });
  const response = await handleInternalBridgeRequest(
    new Request(
      "https://self.witwave.ai/v1/internal/accounts/acct_1:plan-fit-apply",
      {
        method: "POST",
        headers: {
          Authorization: "Bearer bridge-token",
          "Content-Type": "application/json",
        },
        body,
        duplex: "half",
      },
    ),
    environment(),
    async () => assert.fail("cell must not be called"),
  );
  assert.equal(response.status, 413);
  assert.equal(pulls, 2);
});
