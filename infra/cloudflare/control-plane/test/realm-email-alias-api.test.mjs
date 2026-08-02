import assert from "node:assert/strict";
import test from "node:test";

import {
  handleRealmEmailAliasAdminRequest,
  handleRealmEmailAliasCustomerRequest,
  handleRealmEmailRouteRequest,
  isRealmEmailRoutePath,
  isRealmEmailAliasAdminPath,
  matchRealmEmailAliasCustomerPath,
  matchRealmEmailRoutePath,
} from "../src/realm-email-alias-api.mjs";
import {
  DurableRealmEmailAliasRegistry,
  REALM_EMAIL_ALIAS_FEATURE,
  REALM_EMAIL_ALIAS_LIMIT,
  realmEmailRouteKey,
} from "../src/realm-email-alias-runtime.mjs";

const ACCOUNT = "acct_api";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const ENDPOINT = "https://cell.example";
const DOMAIN = "agent-mail.witwave.ai";
const EDGE_TOKEN = "edge-token-at-least-16-characters";
const ADMIN = { admin_id: "adm_aaaaaaaaaaaaaaaaaaaa", handle: "ada" };

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
  async list({ prefix = "" } = {}) {
    return new Map([...this.values]
      .filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, structuredClone(value)]));
  }
  async transaction(callback) {
    const staged = new Map([...this.values]
      .map(([key, value]) => [key, structuredClone(value)]));
    const transaction = {
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    };
    const result = await callback(transaction);
    this.values = staged;
    return result;
  }
}

class Directory {
  constructor() {
    this.values = new Map([
      [`acct:${ACCOUNT}`, { cell: "cell-one" }],
      ["cell:cell-one", {
        endpoint: ENDPOINT,
        provision_token: "witself_prv_cell",
      }],
    ]);
  }
  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? null : structuredClone(value);
  }
  async put(key, value) {
    this.values.set(key, JSON.parse(value));
  }
}

function environment() {
  const directory = new Directory();
  const emailDirectory = new Directory();
  emailDirectory.values.clear();
  let requestSequence = 0;
  let claimSequence = 0;
  const projectionFetch = cellFetch();
  const runtime = new DurableRealmEmailAliasRegistry(
    { storage: new Storage(), id: { name: "global" } },
    { DIRECTORY: directory, AGENT_EMAIL_DIRECTORY: emailDirectory },
    {
      now: (() => {
        let tick = 0;
        return () => new Date(Date.UTC(2026, 7, 1, 0, 0, tick++));
      })(),
      newRequestID: () => {
        requestSequence++;
        return `earq_${"a".repeat(15)}${String.fromCharCode(96 + requestSequence)}`;
      },
      newClaimID: () => {
        claimSequence++;
        return `era_${"b".repeat(15)}${String.fromCharCode(96 + claimSequence)}`;
      },
      fetch: projectionFetch,
    },
  );
  const namespace = {
    idFromName: (name) => name,
    get: () => ({ fetch: (request, init) => runtime.fetch(
      typeof request === "string" ? new Request(request, init) : request,
    ) }),
  };
  return {
    env: {
      DIRECTORY: directory,
      AGENT_EMAIL_DIRECTORY: emailDirectory,
      REALM_EMAIL_ALIASES: namespace,
      AGENT_EMAIL_DOMAIN: DOMAIN,
      CONTROL_PLANE_EDGE_TOKEN: EDGE_TOKEN,
      CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
    },
    runtime,
    emailDirectory,
  };
}

function cellFetch({ aliasEnabled = true, realm = REALM } = {}) {
  const claims = new Map();
  return async (url, init = {}) => {
    url = String(url);
    assert.match(init.headers.Authorization, /^Bearer /);
    if (url === `${ENDPOINT}/v1/whoami`) {
      return Response.json({
        principal: { account_id: ACCOUNT, operator_id: "opr_api" },
      });
    }
    if (url === `${ENDPOINT}/v1/account`) {
      return Response.json({
        account: {
          id: ACCOUNT,
          status: "active",
          plan_features: aliasEnabled ? [REALM_EMAIL_ALIAS_FEATURE] : [],
          plan_limits: {
            [REALM_EMAIL_ALIAS_LIMIT]: aliasEnabled ? 1 : 0,
          },
          plan_snapshot_revision: 7,
          plan_snapshot_hash: "a".repeat(64),
        },
      });
    }
    if (url === `${ENDPOINT}/v1/realms`) {
      return Response.json({ realms: [{ id: realm, name: "Default" }] });
    }
    if (url === `${ENDPOINT}/v1/accounts/${ACCOUNT}:plan`) {
      return Response.json({
        account_id: ACCOUNT,
        revision: 7,
        snapshot_hash: "a".repeat(64),
        features: aliasEnabled ? [REALM_EMAIL_ALIAS_FEATURE] : [],
        limits: { [REALM_EMAIL_ALIAS_LIMIT]: aliasEnabled ? 1 : 0 },
      });
    }
    if (url.startsWith(
      `${ENDPOINT}/v1/accounts/${ACCOUNT}:email-realm-alias-target?`,
    ) && init.method === "GET") {
      const realmID = new URL(url).searchParams.get("realm_id");
      return realmID === realm
        ? Response.json({
          schema_version: "witself.v0",
          account_id: ACCOUNT,
          realm_id: realmID,
          exists: true,
        })
        : Response.json({ error: "not found" }, { status: 404 });
    }
    if (url === `${ENDPOINT}/v1/accounts/${ACCOUNT}:email-realm-alias` &&
        init.method === "POST") {
      const payload = JSON.parse(init.body);
      const acknowledgement = { account_id: ACCOUNT, ...payload };
      claims.set(payload.claim_id, acknowledgement);
      return Response.json(acknowledgement);
    }
    if (url.startsWith(`${ENDPOINT}/v1/accounts/${ACCOUNT}:email-realm-alias?`) &&
        init.method === "GET") {
      const claim = claims.get(new URL(url).searchParams.get("claim_id"));
      return claim
        ? Response.json(claim)
        : Response.json({ error: "not found" }, { status: 404 });
    }
    return Response.json({ error: "not found" }, { status: 404 });
  };
}

function customerRequest(method, body) {
  return new Request(
    `https://self.example/v1/accounts/${ACCOUNT}/realms/${REALM}/email-alias-requests`,
    {
      method,
      headers: {
        Authorization: "Bearer witself_opr_test",
        ...(body ? { "Content-Type": "application/json" } : {}),
      },
      ...(body ? { body: JSON.stringify(body) } : {}),
    },
  );
}

test("route matchers keep customer and platform-admin namespaces distinct", () => {
  const customer = `/v1/accounts/${ACCOUNT}/realms/${REALM}/email-alias-requests`;
  assert.deepEqual([...matchRealmEmailAliasCustomerPath(customer)].slice(1), [ACCOUNT, REALM]);
  assert.equal(matchRealmEmailAliasCustomerPath("/v1/admin/realm-email-aliases"), null);
  assert.deepEqual(
    [...matchRealmEmailRoutePath(`/v1/email/realm-routes/${DOMAIN}/acme`)].slice(1),
    [DOMAIN, "acme"],
  );
  assert.equal(isRealmEmailRoutePath("/v1/email/realm-routes/bad"), true);
  assert.equal(isRealmEmailRoutePath("/v1/email/other"), false);
  for (const path of [
    "/v1/admin/realm-email-alias-requests",
    "/v1/admin/realm-email-alias-requests/earq_aaaaaaaaaaaaaaaa:approve",
    "/v1/admin/realm-email-aliases",
    "/v1/admin/realm-email-aliases/acme:suspend",
    "/v1/admin/realm-email-aliases:assign-internal",
    "/v1/admin/realm-email-reserved-names",
    "/v1/admin/realm-email-reserved-names/witself",
    "/v1/admin/realm-email-alias-audit",
  ]) assert.equal(isRealmEmailAliasAdminPath(path), true, path);
});

test("customer request verifies operator, realm, and current plan before claiming", async () => {
  const { env } = environment();
  const request = customerRequest("POST", {
    alias: "acme",
    idempotency_key: "api-acme",
  });
  const match = matchRealmEmailAliasCustomerPath(new URL(request.url).pathname);
  const response = await handleRealmEmailAliasCustomerRequest(
    request,
    env,
    match,
    cellFetch(),
  );
  assert.equal(response.status, 202);
  const body = await response.json();
  assert.equal(body.request.alias, "acme");
  assert.equal(body.request.requested_by, "opr_api");

  const listedRequest = customerRequest("GET");
  const listed = await handleRealmEmailAliasCustomerRequest(
    listedRequest,
    env,
    match,
    cellFetch(),
  );
  assert.equal(listed.status, 200);
  assert.equal((await listed.json()).requests.length, 1);

  const invalidCursorRequest = new Request(`${listedRequest.url}?cursor=bad!`, {
    headers: { Authorization: "Bearer witself_opr_test" },
  });
  const invalidCursor = await handleRealmEmailAliasCustomerRequest(
    invalidCursorRequest,
    env,
    match,
    cellFetch(),
  );
  assert.equal(invalidCursor.status, 400);
  assert.match((await invalidCursor.json()).error, /cursor/);
});

test("customer authorization refuses non-public or credential-bearing cell endpoints", async () => {
  for (const endpoint of [
    "http://cell.example",
    "https://user:pass@cell.example",
    "https://cell.example/path",
    "https://cell.example/?token=secret",
  ]) {
    const { env } = environment();
    env.DIRECTORY.values.get("cell:cell-one").endpoint = endpoint;
    const request = customerRequest("POST", {
      alias: "acme",
      idempotency_key: `invalid-endpoint-${endpoint.length}`,
    });
    const response = await handleRealmEmailAliasCustomerRequest(
      request,
      env,
      matchRealmEmailAliasCustomerPath(new URL(request.url).pathname),
      async () => { throw new Error("invalid endpoint must not be fetched"); },
    );
    assert.equal(response.status, 404, endpoint);
  }
});

test("Personal or Professional-style zero alias entitlement is refused", async () => {
  const { env } = environment();
  const request = customerRequest("POST", {
    alias: "acme",
    idempotency_key: "api-disabled",
  });
  const response = await handleRealmEmailAliasCustomerRequest(
    request,
    env,
    matchRealmEmailAliasCustomerPath(new URL(request.url).pathname),
    cellFetch({ aliasEnabled: false }),
  );
  assert.equal(response.status, 403);
  assert.match((await response.json()).error, /not enabled/);
});

test("default-off operational gate blocks creation without uninstalling APIs", async () => {
  const { env } = environment();
  delete env.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED;
  const request = customerRequest("POST", {
    alias: "acme",
    idempotency_key: "api-not-activated",
  });
  const response = await handleRealmEmailAliasCustomerRequest(
    request,
    env,
    matchRealmEmailAliasCustomerPath(new URL(request.url).pathname),
    cellFetch(),
  );
  assert.equal(response.status, 409);
  assert.match((await response.json()).error, /not activated/);

  const listURL = new URL(
    "https://self.example/v1/admin/realm-email-reserved-names",
  );
  const listed = await handleRealmEmailAliasAdminRequest(
    new Request(listURL),
    env,
    listURL,
    ADMIN,
    cellFetch(),
  );
  assert.equal(listed.status, 200);
});

test("platform admin approves requests and governs versioned reserved names", async () => {
  const { env } = environment();
  const requested = customerRequest("POST", {
    alias: "acme",
    idempotency_key: "request-acme",
  });
  const requestedResponse = await handleRealmEmailAliasCustomerRequest(
    requested,
    env,
    matchRealmEmailAliasCustomerPath(new URL(requested.url).pathname),
    cellFetch(),
  );
  const requestID = (await requestedResponse.json()).request.id;

  const approveURL = new URL(
    `https://self.example/v1/admin/realm-email-alias-requests/${requestID}:approve`,
  );
  const approved = await handleRealmEmailAliasAdminRequest(
    new Request(approveURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        idempotency_key: "approve-acme",
        reason: "approved customer realm alias",
      }),
    }),
    env,
    approveURL,
    ADMIN,
    cellFetch(),
  );
  assert.equal(approved.status, 200);
  assert.equal((await approved.json()).assignment.status, "active");

  const reservedURL = new URL(
    "https://self.example/v1/admin/realm-email-reserved-names",
  );
  const created = await handleRealmEmailAliasAdminRequest(
    new Request(reservedURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        actor: { kind: "platform_admin", id: "spoofed-admin" },
        name: "acme",
        category: "platform_brand",
        reason: "protected after assignment",
        idempotency_key: "reserve-acme",
      }),
    }),
    env,
    reservedURL,
    ADMIN,
    cellFetch(),
  );
  assert.equal(created.status, 201);
  const createdBody = await created.json();
  assert.equal(createdBody.reserved_name.claim_conflict.alias, "acme");
  assert.equal(createdBody.reserved_name.version, 1);
  assert.equal(createdBody.reserved_name.created_by, ADMIN.admin_id);

  const itemURL = new URL(
    "https://self.example/v1/admin/realm-email-reserved-names/acme",
  );
  const retired = await handleRealmEmailAliasAdminRequest(
    new Request(itemURL, {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        actor: { kind: "platform_admin", id: "spoofed-admin" },
        name: "witwave",
        reason: "no longer protected",
        idempotency_key: "retire-reservation-acme",
      }),
    }),
    env,
    itemURL,
    ADMIN,
    cellFetch(),
  );
  assert.equal(retired.status, 200);
  const retiredBody = await retired.json();
  assert.equal(retiredBody.reserved_name.name, "acme");
  assert.equal(retiredBody.reserved_name.enabled, false);
  assert.equal(retiredBody.reserved_name.updated_by, ADMIN.admin_id);
});

test("edge-token fallback is authoritative and response-only", async () => {
  const { env, emailDirectory } = environment();
  const requested = customerRequest("POST", {
    alias: "healing",
    idempotency_key: "request-healing",
  });
  const requestedResponse = await handleRealmEmailAliasCustomerRequest(
    requested,
    env,
    matchRealmEmailAliasCustomerPath(new URL(requested.url).pathname),
    cellFetch(),
  );
  const requestID = (await requestedResponse.json()).request.id;
  const approveURL = new URL(
    `https://self.example/v1/admin/realm-email-alias-requests/${requestID}:approve`,
  );
  const approved = await handleRealmEmailAliasAdminRequest(
    new Request(approveURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        idempotency_key: "approve-healing",
        reason: "approved edge healing test",
      }),
    }),
    env,
    approveURL,
    ADMIN,
    cellFetch(),
  );
  assert.equal(approved.status, 200);

  const key = realmEmailRouteKey(DOMAIN, "healing");
  emailDirectory.values.delete(key);
  const routeURL = new URL(
    `https://self.example/v1/email/realm-routes/${DOMAIN}/healing`,
  );
  const match = matchRealmEmailRoutePath(routeURL.pathname);
  const unauthorized = await handleRealmEmailRouteRequest(
    new Request(routeURL, {
      headers: { Authorization: "Bearer wrong-but-long-enough-token" },
    }),
    env,
    match,
  );
  assert.equal(unauthorized.status, 401);

  const refreshed = await handleRealmEmailRouteRequest(
    new Request(routeURL, {
      headers: { Authorization: `Bearer ${EDGE_TOKEN}` },
    }),
    env,
    match,
  );
  assert.equal(refreshed.status, 200);
  const projection = await refreshed.json();
  assert.equal(projection.state, "applied");
  assert.equal(emailDirectory.values.has(key), false);
});

test("gate-off admin can suspend or retire but cannot reactivate", async () => {
  const { env } = environment();
  const requested = customerRequest("POST", {
    alias: "gated",
    idempotency_key: "request-gated",
  });
  const requestedResponse = await handleRealmEmailAliasCustomerRequest(
    requested,
    env,
    matchRealmEmailAliasCustomerPath(new URL(requested.url).pathname),
    cellFetch(),
  );
  const requestID = (await requestedResponse.json()).request.id;
  const approveURL = new URL(
    `https://self.example/v1/admin/realm-email-alias-requests/${requestID}:approve`,
  );
  await handleRealmEmailAliasAdminRequest(
    new Request(approveURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        idempotency_key: "approve-gated",
        reason: "approved gate test",
      }),
    }),
    env,
    approveURL,
    ADMIN,
    cellFetch(),
  );
  delete env.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED;

  const mutate = async (action, key) => {
    const url = new URL(
      `https://self.example/v1/admin/realm-email-aliases/gated:${action}`,
    );
    return handleRealmEmailAliasAdminRequest(
      new Request(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          reason: `${action} for gate test`,
          idempotency_key: key,
        }),
      }),
      env,
      url,
      ADMIN,
      cellFetch(),
    );
  };
  assert.equal((await mutate("suspend", "suspend-gated")).status, 200);
  assert.equal((await mutate("reactivate", "reactivate-gated")).status, 409);
  assert.equal((await mutate("retire", "retire-gated")).status, 200);
});
