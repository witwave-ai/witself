import assert from "node:assert/strict";
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

const target = Object.freeze({
  plan: "personal",
  snapshot_hash: "a".repeat(64),
  limits: {
    agents: 10,
    agent_email_realm_aliases_per_realm: 1,
    agent_email_custom_domains_per_account: 0,
  },
  policies: {},
  features: [],
});

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

function request(body = target) {
  return new Request(
    "https://self.witwave.ai/v1/internal/accounts/acct_1:plan-fit",
    {
      method: "POST",
      headers: {
        Authorization: "Bearer bridge-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ schema_version: "witself.v0", target: body }),
    },
  );
}

function authorityNamespace(handler) {
  return {
    idFromName(name) {
      assert.equal(name, "global");
      return name;
    },
    get() {
      return { fetch: handler };
    },
  };
}

test("plan-fit forwards the exact snapshot and merges global authorities", async () => {
  const env = environment();
  env.REALM_EMAIL_ALIASES = authorityNamespace(async (_url, init) => {
    assert.deepEqual(JSON.parse(init.body), {
      account_id: "acct_1",
      maximum: 1,
    });
    return Response.json({
      schema_version: "witself.realm-email-alias.v1",
      account_id: "acct_1",
      maximum: 1,
      over_limit_count: 2,
      highest_used: 3,
    });
  });
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(async (_url, init) => {
    assert.deepEqual(JSON.parse(init.body), {
      account_id: "acct_1",
      maximum: 0,
    });
    return Response.json({
      schema_version: "witself.agent-email-domain.v1",
      account_id: "acct_1",
      maximum: 0,
      used: 1,
    });
  });
  let cellCall;
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async (url, init) => {
      cellCall = { url, init };
      return Response.json({
        schema_version: "witself.v0",
        account_id: "acct_1",
        target_plan: target.plan,
        target_snapshot_hash: target.snapshot_hash,
        violations: [
          {
            code: "limit_exceeded",
            dimension: "agents",
            scope: "account",
            used: 12,
            max: 10,
            subject_count: 1,
          },
          {
            code: "limit_exceeded",
            dimension: "agent_email_realm_aliases_per_realm",
            scope: "realm",
            used: 2,
            max: 1,
            subject_count: 1,
          },
        ],
      });
    },
  );

  assert.equal(response.status, 200);
  assert.equal(
    cellCall.url,
    "https://cell.example/v1/accounts/acct_1:plan-fit",
  );
  assert.equal(cellCall.init.method, "POST");
  assert.equal(cellCall.init.headers.Authorization, "Bearer cell-token");
  assert.deepEqual(
    JSON.parse(new TextDecoder().decode(cellCall.init.body)),
    { schema_version: "witself.v0", target },
  );
  assert.deepEqual((await response.json()).violations, [
    {
      code: "limit_exceeded",
      dimension: "agents",
      scope: "account",
      used: 12,
      max: 10,
      subject_count: 1,
    },
    {
      code: "limit_exceeded",
      dimension: "agent_email_realm_aliases_per_realm",
      scope: "realm",
      used: 3,
      max: 1,
      subject_count: 2,
    },
    {
      code: "limit_exceeded",
      dimension: "agent_email_custom_domains_per_account",
      scope: "account",
      used: 1,
      max: 0,
      subject_count: 1,
    },
  ]);
});

test("finite global limits fail closed when either authority is unavailable", async () => {
  const response = await handleInternalBridgeRequest(
    request(),
    environment(),
    async () => Response.json({
      schema_version: "witself.v0",
      account_id: "acct_1",
      target_plan: target.plan,
      target_snapshot_hash: target.snapshot_hash,
      violations: [],
    }),
  );
  assert.equal(response.status, 200);
  assert.deepEqual((await response.json()).violations, [
    {
      code: "authority_incomplete",
      dimension: "agent_email_realm_aliases_per_realm",
      scope: "authority",
      used: 0,
      max: 1,
      subject_count: 1,
    },
    {
      code: "authority_incomplete",
      dimension: "agent_email_custom_domains_per_account",
      scope: "authority",
      used: 0,
      max: 0,
      subject_count: 1,
    },
  ]);
});

test("plan-fit turns an internally inconsistent alias authority result into an incomplete refusal", async () => {
  const env = environment();
  env.REALM_EMAIL_ALIASES = authorityNamespace(async () => Response.json({
    schema_version: "witself.realm-email-alias.v1",
    account_id: "acct_1",
    maximum: 1,
    over_limit_count: 0,
    highest_used: 2,
  }));
  env.AGENT_EMAIL_DOMAINS = authorityNamespace(async () => Response.json({
    schema_version: "witself.agent-email-domain.v1",
    account_id: "acct_1",
    maximum: 0,
    used: 0,
  }));
  const response = await handleInternalBridgeRequest(
    request(),
    env,
    async () => Response.json({
      schema_version: "witself.v0",
      account_id: "acct_1",
      target_plan: target.plan,
      target_snapshot_hash: target.snapshot_hash,
      violations: [],
    }),
  );
  assert.equal(response.status, 200);
  assert.deepEqual((await response.json()).violations, [{
    code: "authority_incomplete",
    dimension: "agent_email_realm_aliases_per_realm",
    scope: "authority",
    used: 0,
    max: 1,
    subject_count: 1,
  }]);
});

test("plan-fit rejects an ambiguous or mismatched cell report", async () => {
  const response = await handleInternalBridgeRequest(
    request(),
    environment(),
    async () => Response.json({
      schema_version: "witself.v0",
      account_id: "acct_1",
      target_plan: target.plan,
      target_snapshot_hash: target.snapshot_hash,
      violations: [{
        code: "limit_exceeded",
        dimension: "agents",
        scope: "account",
        used: 12,
        max: 9,
        subject_count: 1,
      }],
    }),
  );
  assert.equal(response.status, 502);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "cell returned an invalid plan-fit report",
  });
});
