import assert from "node:assert/strict";
import test from "node:test";

import {
  handleAgentEmailDomainAdminRequest,
  handleAgentEmailDomainCustomerRequest,
  isAgentEmailDomainAdminPath,
  matchAgentEmailDomainCustomerPath,
} from "../src/agent-email-domain-api.mjs";
import {
  AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE,
  AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT,
} from "../src/agent-email-domain-runtime.mjs";

const ACCOUNT = "acct_domains";
const ENDPOINT = "https://cell.example";
const REQUEST_ID = `aedr_${"a".repeat(16)}`;
const ADMIN = { admin_id: "adm_aaaaaaaaaaaaaaaaaaaa", handle: "ada" };

class Directory {
  constructor() {
    this.values = new Map([
      [`acct:${ACCOUNT}`, { cell: "cell-one" }],
      ["cell:cell-one", { endpoint: ENDPOINT }],
    ]);
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? null : structuredClone(value);
  }
}

function cellFetch({ enabled = true, limit = 1 } = {}) {
  return async (url) => {
    if (String(url) === `${ENDPOINT}/v1/whoami`) {
      return Response.json({
        principal: { account_id: ACCOUNT, operator_id: "opr_domains" },
      });
    }
    if (String(url) === `${ENDPOINT}/v1/account`) {
      return Response.json({
        account: {
          id: ACCOUNT,
          plan_features: enabled ? [AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE] : [],
          plan_limits: { [AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT]: limit },
          plan_snapshot_revision: 9,
          plan_snapshot_hash: "b".repeat(64),
        },
      });
    }
    return Response.json({ error: "not found" }, { status: 404 });
  };
}

function environment() {
  const forwarded = [];
  return {
    forwarded,
    env: {
      DIRECTORY: new Directory(),
      AGENT_EMAIL_DOMAINS: {
        idFromName: (name) => name,
        get: () => ({
          fetch: async (url, init) => {
            forwarded.push({
              path: new URL(url).pathname,
              body: JSON.parse(init.body),
            });
            return Response.json({
              schema_version: "witself.agent-email-domain.v1",
              requests: [],
            });
          },
        }),
      },
    },
  };
}

function customerRequest(method, body, suffix = "") {
  return new Request(
    `https://self.example/v1/accounts/${ACCOUNT}/email-domain-requests${suffix}`,
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

test("customer and admin custom-domain routes stay exact and distinct", () => {
  const customer =
    `/v1/accounts/${ACCOUNT}/email-domain-requests`;
  assert.deepEqual(
    [...matchAgentEmailDomainCustomerPath(customer)].slice(1),
    [ACCOUNT],
  );
  assert.equal(matchAgentEmailDomainCustomerPath(`${customer}/extra`), null);
  assert.equal(matchAgentEmailDomainCustomerPath(
    "/v1/admin/agent-email-domain-requests",
  ), null);
  for (const path of [
    "/v1/admin/agent-email-domain-requests",
    `/v1/admin/agent-email-domain-requests/${REQUEST_ID}`,
    `/v1/admin/agent-email-domain-requests/${REQUEST_ID}:reject`,
    `/v1/admin/agent-email-domain-requests/${REQUEST_ID}:retire`,
    `/v1/admin/agent-email-domain-requests/${REQUEST_ID}:verify`,
    "/v1/admin/agent-email-domain-audit",
  ]) assert.equal(isAgentEmailDomainAdminPath(path), true, path);
  assert.equal(isAgentEmailDomainAdminPath(
    `/v1/admin/agent-email-domain-requests/${REQUEST_ID}:activate`,
  ), false);
});

test("customer request gate is independent, exact-true, and default-off", async () => {
  const { env, forwarded } = environment();
  Object.assign(env, {
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
    CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED: "true",
    CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "true",
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "true",
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "true",
  });
  const match = matchAgentEmailDomainCustomerPath(
    `/v1/accounts/${ACCOUNT}/email-domain-requests`,
  );
  const body = { domain: "mail.example.com", idempotency_key: "request-once" };

  const dark = await handleAgentEmailDomainCustomerRequest(
    customerRequest("POST", body), env, match, cellFetch(),
  );
  assert.equal(dark.status, 409);
  assert.equal((await dark.json()).code, "custom_domain_requests_disabled");
  assert.equal(forwarded.length, 0);

  env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED = "TRUE";
  const wrongCase = await handleAgentEmailDomainCustomerRequest(
    customerRequest("POST", body), env, match, cellFetch(),
  );
  assert.equal(wrongCase.status, 409);
  assert.equal(forwarded.length, 0);

  env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED = "true";
  const notAllowlisted = await handleAgentEmailDomainCustomerRequest(
    customerRequest("POST", body), env, match, cellFetch(),
  );
  assert.equal(notAllowlisted.status, 409);
  assert.equal(forwarded.length, 0);

  env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST = ACCOUNT;
  const enabled = await handleAgentEmailDomainCustomerRequest(
    customerRequest("POST", body), env, match, cellFetch(),
  );
  assert.equal(enabled.status, 200);
  assert.deepEqual(forwarded, [{
    path: "/request/create",
    body: {
      actor: { kind: "account_operator", id: "opr_domains" },
      account_id: ACCOUNT,
      domain: "mail.example.com",
      idempotency_key: "request-once",
      requests_enabled: true,
      feature_enabled: true,
      domain_limit: 1,
      plan_revision: 9,
      plan_snapshot_hash: "b".repeat(64),
    },
  }]);
});

test("customer auth failures use the custom-domain schema", async () => {
  const { env, forwarded } = environment();
  const path = `/v1/accounts/${ACCOUNT}/email-domain-requests`;
  const response = await handleAgentEmailDomainCustomerRequest(
    new Request(`https://self.example${path}`),
    env,
    matchAgentEmailDomainCustomerPath(path),
    cellFetch(),
  );
  assert.equal(response.status, 401);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.agent-email-domain.v1",
    error: "operator token required",
  });
  assert.equal(forwarded.length, 0);
});

test("customer list remains readable while creation is dark", async () => {
  const { env, forwarded } = environment();
  const path = `/v1/accounts/${ACCOUNT}/email-domain-requests`;
  const response = await handleAgentEmailDomainCustomerRequest(
    customerRequest("GET", null, "?cursor=page-two"),
    env,
    matchAgentEmailDomainCustomerPath(path),
    cellFetch(),
  );
  assert.equal(response.status, 200);
  assert.deepEqual(forwarded, [{
    path: "/request/list",
    body: {
      actor: { kind: "account_operator", id: "opr_domains" },
      account_id: ACCOUNT,
      cursor: "page-two",
    },
  }]);
});

test("admin custom-domain review routes bind immutable actor and URL target", async () => {
  const { env, forwarded } = environment();
  const listURL = new URL(
    "https://self.example/v1/admin/agent-email-domain-requests" +
    "?account_id=acct_domains&domain=mail.example.com&state=pending_verification",
  );
  const listed = await handleAgentEmailDomainAdminRequest(
    new Request(listURL), env, listURL, ADMIN,
  );
  assert.equal(listed.status, 200);

  const rejectURL = new URL(
    `https://self.example/v1/admin/agent-email-domain-requests/${REQUEST_ID}:reject`,
  );
  const rejected = await handleAgentEmailDomainAdminRequest(
    new Request(rejectURL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        actor: { kind: "account_operator", id: "forged" },
        request_id: `aedr_${"b".repeat(16)}`,
        idempotency_key: "reject-once",
        reason: "ownership request was withdrawn",
      }),
    }),
    env,
    rejectURL,
    ADMIN,
  );
  assert.equal(rejected.status, 200);

  const auditURL = new URL(
    "https://self.example/v1/admin/agent-email-domain-audit" +
    "?account_id=acct_domains&action=custom_domain.requested&limit=25",
  );
  const audit = await handleAgentEmailDomainAdminRequest(
    new Request(auditURL), env, auditURL, ADMIN,
  );
  assert.equal(audit.status, 200);

  for (const limit of ["25junk", "0", "101", "1.5", "001"]) {
    const invalidURL = new URL(
      `https://self.example/v1/admin/agent-email-domain-audit?limit=${limit}`,
    );
    const invalid = await handleAgentEmailDomainAdminRequest(
      new Request(invalidURL), env, invalidURL, ADMIN,
    );
    assert.equal(invalid.status, 400, limit);
  }

  assert.deepEqual(forwarded, [
    {
      path: "/request/admin-list",
      body: {
        actor: { kind: "platform_admin", id: ADMIN.admin_id },
        state: "pending_verification",
        account_id: ACCOUNT,
        domain: "mail.example.com",
      },
    },
    {
      path: "/request/reject",
      body: {
        actor: { kind: "platform_admin", id: ADMIN.admin_id },
        request_id: REQUEST_ID,
        idempotency_key: "reject-once",
        reason: "ownership request was withdrawn",
      },
    },
    {
      path: "/audit/list",
      body: {
        actor: { kind: "platform_admin", id: ADMIN.admin_id },
        action: "custom_domain.requested",
        account_id: ACCOUNT,
        limit: 25,
      },
    },
  ]);

  const unauthenticated = await handleAgentEmailDomainAdminRequest(
    new Request(listURL), env, listURL, null,
  );
  assert.equal(unauthenticated.status, 401);
});

test("admin ownership verification is separately exact-true gated", async () => {
  const { env, forwarded } = environment();
  const verifyURL = new URL(
    `https://self.example/v1/admin/agent-email-domain-requests/${REQUEST_ID}:verify`,
  );
  const request = (fields = {}) => new Request(verifyURL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ idempotency_key: "verify-once", ...fields }),
  });
  const dark = await handleAgentEmailDomainAdminRequest(
    request({ verification_enabled: true }), env, verifyURL, ADMIN,
  );
  assert.equal(dark.status, 200);
  assert.equal(forwarded.at(-1).body.verification_enabled, false);

  env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED = "TRUE";
  await handleAgentEmailDomainAdminRequest(request(), env, verifyURL, ADMIN);
  assert.equal(forwarded.at(-1).body.verification_enabled, false);

  env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED = "true";
  await handleAgentEmailDomainAdminRequest(
    request({ verification_enabled: false }), env, verifyURL, ADMIN,
  );
  assert.deepEqual(forwarded.at(-1), {
    path: "/request/verify",
    body: {
      actor: { kind: "platform_admin", id: ADMIN.admin_id },
      request_id: REQUEST_ID,
      idempotency_key: "verify-once",
      verification_enabled: true,
    },
  });
});
