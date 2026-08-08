import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE,
  AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT,
  AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT,
  DurableAgentEmailDomainRegistry,
  agentEmailCustomDomainEntitlement,
  agentEmailCustomDomainOpenRequestLimit,
  agentEmailDomainRegistryStub,
  isProtectedAgentEmailDomain,
  normalizeAgentEmailCustomDomain,
} from "../src/agent-email-domain-runtime.mjs";

const ACCOUNT = "acct_domain";
const OTHER_ACCOUNT = "acct_other";
const OPERATOR = { kind: "account_operator", id: "opr_domain" };
const OTHER_OPERATOR = { kind: "account_operator", id: "opr_other" };
const ADMIN = { kind: "platform_admin", id: "adm_domain" };
const PLAN_HASH = "7".repeat(64);

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

  async list({
    prefix = "",
    limit,
    reverse = false,
    startAfter,
    end,
  } = {}) {
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
      get: async (key) => {
        const value = staged.get(key);
        return value === undefined ? undefined : structuredClone(value);
      },
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    };
    const result = await callback(transaction);
    this.values = staged;
    return result;
  }
}

function base32(value, length) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let encoded = "";
  for (let current = value; current > 0; current = Math.floor(current / 32)) {
    encoded = alphabet[current % 32] + encoded;
  }
  return (encoded || "a").padStart(length, "a");
}

function registry(env = {}, dependencyOverrides = {}) {
  const storage = new Storage();
  let requestSequence = 0;
  let challengeSequence = 0;
  let currentTime = Date.UTC(2026, 7, 3, 12, 0, 0);
  const runtime = new DurableAgentEmailDomainRegistry(
    { storage },
    {
      AGENT_EMAIL_DOMAIN: "witmail.net",
      AGENT_EMAIL_LEGACY_DOMAINS: "agent-mail.witwave.ai",
      ...env,
    },
    {
      now: () => {
        currentTime += 1_000;
        return new Date(currentTime);
      },
      newRequestID: () => {
        requestSequence++;
        return `aedr_${base32(requestSequence, 16)}`;
      },
      newChallengeToken: () => {
        challengeSequence++;
        return `aedv_${base32(challengeSequence, 32)}`;
      },
      ...dependencyOverrides,
    },
  );
  return {
    runtime,
    storage,
    requestCount: () => requestSequence,
    challengeCount: () => challengeSequence,
  };
}

async function call(runtime, path, body, method = "POST") {
  const response = await runtime.fetch(new Request(
    `https://agent-email-domain.internal${path}`,
    {
      method,
      headers: { "Content-Type": "application/json" },
      ...(method === "POST" ? { body: JSON.stringify(body) } : {}),
    },
  ));
  return { response, body: await response.json() };
}

function create(runtime, domain, fields = {}) {
  return call(runtime, "/request/create", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    domain,
    requests_enabled: true,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 7,
    plan_snapshot_hash: PLAN_HASH,
    idempotency_key: `create-${domain.toLowerCase()}`,
    ...fields,
  });
}

test("domain normalization is lowercase ASCII DNS only", () => {
  assert.equal(normalizeAgentEmailCustomDomain(" Example.COM "), "example.com");
  assert.equal(
    normalizeAgentEmailCustomDomain("Mail.Customer.Example"),
    "mail.customer.example",
  );
  const longestChallengeSafeDomain =
    `${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(39)}`;
  assert.equal(longestChallengeSafeDomain.length, 231);
  assert.equal(
    normalizeAgentEmailCustomDomain(longestChallengeSafeDomain),
    longestChallengeSafeDomain,
  );

  for (const value of [
    "example",
    "*.example.com",
    "café.example",
    "xn--caf-dma.example",
    "XN--CAF-DMA.EXAMPLE",
    "127.0.0.1",
    "[::1]",
    ".example.com",
    "example.com.",
    "example..com",
    "-bad.example",
    "bad-.example",
    "bad_domain.example",
    "https://example.com",
    "example.com:25",
    `${"a".repeat(64)}.example`,
    "example.123",
    `${"a".repeat(63)}.${"b".repeat(63)}.${"c".repeat(63)}.${"d".repeat(40)}`,
    "",
  ]) {
    assert.throws(
      () => normalizeAgentEmailCustomDomain(value),
      /custom inbound email domain|domain must be a string/,
      value,
    );
  }
  assert.throws(() => normalizeAgentEmailCustomDomain(null), /string/);
});

test("Witself roots, configured roots, and every child remain protected", () => {
  for (const domain of [
    "witmail.net",
    "mail.witmail.ai",
    "self.witwave.ai",
    "x.witself.com",
    "x.witself.cloud",
    "x.witself.dev",
    "x.witself.io",
    "agent-mail.witwave.ai",
  ]) {
    assert.equal(isProtectedAgentEmailDomain(domain), true, domain);
  }
  assert.equal(isProtectedAgentEmailDomain("witwave.ai.evil.example"), false);
  assert.equal(isProtectedAgentEmailDomain("evilwitwave.ai"), false);
  assert.equal(isProtectedAgentEmailDomain("mail.corp.example", {
    AGENT_EMAIL_PROTECTED_DOMAINS: "corp.example,company.example",
  }), true);
  assert.throws(
    () => isProtectedAgentEmailDomain("customer.example", {
      AGENT_EMAIL_PROTECTED_DOMAINS: " corp.example",
    }),
    /configuration is invalid/,
  );
});

test("custom-domain entitlement follows shared zero and null semantics", () => {
  assert.deepEqual(agentEmailCustomDomainEntitlement({}), {
    enabled: false,
    limit: null,
  });
  assert.deepEqual(agentEmailCustomDomainEntitlement({
    features: [AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE],
    limits: { [AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT]: null },
  }), { enabled: true, limit: null });
  assert.deepEqual(agentEmailCustomDomainEntitlement({
    features: [AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE],
    limits: { [AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT]: 0 },
  }), { enabled: false, limit: 0 });
  assert.deepEqual(agentEmailCustomDomainEntitlement({
    features: [AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE],
    limits: { [AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT]: 3 },
  }), { enabled: true, limit: 3 });
  assert.deepEqual(agentEmailCustomDomainEntitlement({
    features: [AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE],
    limits: { [AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT]: -1 },
  }), { enabled: false, limit: 0 });
});

test("registry stub targets the single global authority", () => {
  const calls = [];
  const stub = { fetch() {} };
  const namespace = {
    idFromName(name) {
      calls.push(["id", name]);
      return `id:${name}`;
    },
    get(id) {
      calls.push(["get", id]);
      return stub;
    },
  };
  assert.equal(agentEmailDomainRegistryStub({}), null);
  assert.equal(agentEmailDomainRegistryStub({
    AGENT_EMAIL_CUSTOM_DOMAINS: namespace,
  }), null);
  assert.equal(agentEmailDomainRegistryStub({
    AGENT_EMAIL_DOMAINS: namespace,
  }), stub);
  assert.deepEqual(calls, [["id", "global"], ["get", "id:global"]]);
});

test("journal control routes do not initialize an empty registry", async () => {
  const fixture = registry();
  const status = await call(fixture.runtime, "/journal/status", {});
  assert.equal(status.response.status, 200);
  assert.equal(status.body.schema_version,
    "witself.agent-email-domain-recovery.v1");
  assert.equal(status.body.enabled, false);
  assert.equal(status.body.pending, false);
  assert.equal(status.body.forked, false);
  assert.equal(fixture.storage.values.size, 0);
});

test("journal enforcement freezes an existing unjournaled registry", async () => {
  const fixture = registry();
  const seeded = await call(fixture.runtime, "/request/admin-list", {
    actor: ADMIN,
  });
  assert.equal(seeded.response.status, 200);
  assert.ok(fixture.storage.values.has("meta"));

  fixture.runtime.env.CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED =
    "true";
  const frozen = await call(fixture.runtime, "/request/admin-list", {
    actor: ADMIN,
  });
  assert.equal(frozen.response.status, 503);
  assert.equal(frozen.body.schema_version,
    "witself.agent-email-domain.v1");
  assert.equal(frozen.body.code,
    "agent_email_domain_journal_bootstrap_required");
});

test("creation is dark by default and requires an explicit entitlement fence", async () => {
  const { runtime } = registry();
  const disabled = await create(runtime, "disabled.example", {
    requests_enabled: undefined,
  });
  assert.equal(disabled.response.status, 409);
  assert.equal(disabled.body.code, "custom_domain_requests_disabled");

  const feature = await create(runtime, "feature.example", {
    feature_enabled: false,
  });
  assert.equal(feature.response.status, 403);
  assert.equal(feature.body.code, "feature_not_enabled");

  const zero = await create(runtime, "zero.example", { domain_limit: 0 });
  assert.equal(zero.response.status, 403);
  assert.equal(zero.body.code, "feature_not_enabled");

  const missingLimit = await create(runtime, "missing-limit.example", {
    domain_limit: undefined,
  });
  assert.equal(missingLimit.response.status, 400);

  const badRevision = await create(runtime, "bad-revision.example", {
    plan_revision: -1,
  });
  assert.equal(badRevision.response.status, 400);
  const badHash = await create(runtime, "bad-hash.example", {
    plan_snapshot_hash: "A".repeat(64),
  });
  assert.equal(badHash.response.status, 400);
});

test("request creation, challenge, ownership, and idempotency are durable", async () => {
  const fixture = registry();
  const created = await create(fixture.runtime, "Customer.Example", {
    domain_limit: null,
    idempotency_key: "create-customer",
  });
  assert.equal(created.response.status, 202);
  assert.equal(created.body.schema_version, "witself.agent-email-domain.v1");
  assert.equal(created.body.request.domain, "customer.example");
  assert.equal(created.body.request.state, "pending_verification");
  assert.equal(created.body.request.plan_revision, 7);
  assert.equal(created.body.request.plan_snapshot_hash, PLAN_HASH);
  assert.equal(created.body.request.domain_limit_at_request, null);
  assert.deepEqual(created.body.request.ownership_challenge, {
    record_type: "TXT",
    record_name: "_witself-verification.customer.example",
    record_value:
      `witself-domain-verification=aedv_${base32(1, 32)}`,
    issued_at: created.body.request.requested_at,
  });

  const replay = await create(fixture.runtime, "customer.example", {
    requests_enabled: false,
    domain_limit: 1,
    idempotency_key: "create-customer",
  });
  assert.equal(replay.response.status, 202);
  assert.deepEqual(replay.body, created.body);
  assert.equal(fixture.requestCount(), 1);
  assert.equal(fixture.challengeCount(), 1);

  const conflict = await create(fixture.runtime, "different.example", {
    domain_limit: null,
    idempotency_key: "create-customer",
  });
  assert.equal(conflict.response.status, 409);
  assert.equal(conflict.body.code, "idempotency_conflict");

  const claimed = await create(fixture.runtime, "CUSTOMER.EXAMPLE", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "other-claim",
  });
  assert.equal(claimed.response.status, 409);
  assert.equal(claimed.body.code, "domain_unavailable");

  const customerList = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(customerList.response.status, 200);
  assert.deepEqual(customerList.body.requests, [created.body.request]);
  assert.equal(customerList.body.open_requests, 1);

  const adminList = await call(fixture.runtime, "/request/admin-list", {
    actor: ADMIN,
  });
  assert.equal(adminList.response.status, 200);
  assert.deepEqual(adminList.body.requests, [created.body.request]);

  const shown = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(shown.response.status, 200);
  assert.deepEqual(shown.body.request, created.body.request);
  const customerShow = await call(fixture.runtime, "/request/get", {
    actor: OPERATOR,
    request_id: created.body.request.id,
  });
  assert.equal(customerShow.response.status, 400);
});

test("global serialization permits exactly one owner for a concurrent domain", async () => {
  const { runtime } = registry();
  const [first, second] = await Promise.all([
    create(runtime, "race.example", {
      domain_limit: null,
      idempotency_key: "race-first",
    }),
    create(runtime, "RACE.EXAMPLE", {
      actor: OTHER_OPERATOR,
      account_id: OTHER_ACCOUNT,
      domain_limit: null,
      idempotency_key: "race-second",
    }),
  ]);
  assert.deepEqual(
    [first.response.status, second.response.status].sort(),
    [202, 409],
  );
});

test("request id collisions regenerate without overwriting authority", async () => {
  const collided = `aedr_${"a".repeat(16)}`;
  const replacement = `aedr_${"b".repeat(16)}`;
  const ids = [collided, collided, replacement];
  const fixture = registry({}, {
    newRequestID: () => ids.shift() ?? replacement,
  });
  const first = await create(fixture.runtime, "id-one.example", {
    domain_limit: null,
  });
  const second = await create(fixture.runtime, "id-two.example", {
    domain_limit: null,
  });
  assert.equal(first.response.status, 202);
  assert.equal(first.body.request.id, collided);
  assert.equal(second.response.status, 202);
  assert.equal(second.body.request.id, replacement);

  const exhausted = await create(fixture.runtime, "id-three.example", {
    domain_limit: null,
  });
  assert.equal(exhausted.response.status, 503);
  assert.equal(exhausted.body.code, "request_id_unavailable");
  assert.equal(fixture.challengeCount(), 2);

  const list = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(list.body.open_requests, 2);
  assert.deepEqual(
    new Set(list.body.requests.map((request) => request.id)),
    new Set([collided, replacement]),
  );
});

test("reject and retire release quota but preserve a permanent tombstone", async () => {
  const { runtime } = registry();
  const first = await create(runtime, "one.example", {
    domain_limit: 2,
  });
  const second = await create(runtime, "two.example", {
    domain_limit: 2,
  });
  assert.equal(first.response.status, 202);
  assert.equal(second.response.status, 202);
  const blocked = await create(runtime, "three.example", {
    domain_limit: 2,
  });
  assert.equal(blocked.response.status, 403);
  assert.equal(blocked.body.code, "account_limit_reached");

  for (const reason of ["", "x".repeat(501)]) {
    const invalid = await call(runtime, "/request/reject", {
      actor: ADMIN,
      request_id: first.body.request.id,
      reason,
      idempotency_key: `bad-reason-${reason.length}`,
    });
    assert.equal(invalid.response.status, 400);
  }

  const rejected = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: first.body.request.id,
    reason: " Domain is not eligible ",
    idempotency_key: "reject-one",
  });
  assert.equal(rejected.response.status, 200);
  assert.equal(rejected.body.request.state, "rejected");
  assert.equal(rejected.body.request.decision.reason, "Domain is not eligible");
  assert.deepEqual(
    rejected.body.request.ownership_challenge,
    first.body.request.ownership_challenge,
  );

  const replay = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: first.body.request.id,
    reason: "Domain is not eligible",
    idempotency_key: "reject-one",
  });
  assert.deepEqual(replay.body, rejected.body);
  const decideAgain = await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: first.body.request.id,
    reason: "another decision",
    idempotency_key: "reject-one-again",
  });
  assert.equal(decideAgain.response.status, 409);

  const retiredRejected = await call(runtime, "/request/retire", {
    actor: ADMIN,
    request_id: first.body.request.id,
    reason: "close the rejected request",
    idempotency_key: "retire-one",
  });
  assert.equal(retiredRejected.response.status, 200);
  assert.equal(retiredRejected.body.request.state, "retired");
  assert.equal(retiredRejected.body.request.decision.action, "rejected");
  assert.equal(retiredRejected.body.request.retirement.reason,
    "close the rejected request");

  const retiredPending = await call(runtime, "/request/retire", {
    actor: ADMIN,
    request_id: second.body.request.id,
    reason: "customer withdrew request",
    idempotency_key: "retire-two",
  });
  assert.equal(retiredPending.response.status, 200);
  assert.equal(retiredPending.body.request.state, "retired");

  const replacement = await create(runtime, "replacement.example", {
    domain_limit: 1,
  });
  assert.equal(replacement.response.status, 202);

  const tombstoned = await create(runtime, "one.example", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "reuse-tombstone",
  });
  assert.equal(tombstoned.response.status, 409);
  assert.equal(tombstoned.body.code, "domain_unavailable");

  const list = await call(runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(list.body.open_requests, 1);
  assert.deepEqual(
    new Set(list.body.requests.map((request) => request.state)),
    new Set(["retired", "pending_verification"]),
  );
});

test("product quota and the lowerable hard ceiling are independent", async () => {
  assert.equal(agentEmailCustomDomainOpenRequestLimit({}),
    AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT);
  assert.equal(agentEmailCustomDomainOpenRequestLimit({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT: "2",
  }), 2);
  for (const value of [0, 9, "not-a-number"]) {
    assert.throws(
      () => agentEmailCustomDomainOpenRequestLimit({
        CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT: value,
      }),
      /configuration is invalid/,
    );
  }

  const lowered = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT: "2",
  });
  assert.equal((await create(lowered.runtime, "low-one.example", {
    domain_limit: null,
  })).response.status, 202);
  assert.equal((await create(lowered.runtime, "low-two.example", {
    domain_limit: null,
  })).response.status, 202);
  const third = await create(lowered.runtime, "low-three.example", {
    domain_limit: null,
  });
  assert.equal(third.response.status, 409);
  assert.equal(third.body.code, "technical_open_request_limit_reached");
  assert.equal(third.body.limit, 2);

  const compiled = registry();
  for (let index = 0;
    index < AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT;
    index++) {
    assert.equal((await create(compiled.runtime, `cap-${index}.example`, {
      domain_limit: null,
    })).response.status, 202);
  }
  const ninth = await create(compiled.runtime, "cap-overflow.example", {
    domain_limit: null,
  });
  assert.equal(ninth.response.status, 409);
  assert.equal(ninth.body.limit,
    AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT);
});

test("customer, admin, and newest-first audit pagination stay bounded", async () => {
  const { runtime } = registry();
  const created = [];
  for (const domain of ["page-one.example", "page-two.example", "page-three.example"]) {
    created.push((await create(runtime, domain, { domain_limit: null })).body.request);
  }

  const firstPage = await call(runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    limit: 2,
  });
  assert.equal(firstPage.body.requests.length, 2);
  assert.equal(firstPage.body.truncated, true);
  assert.ok(firstPage.body.next_cursor);
  const secondPage = await call(runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    limit: 2,
    cursor: firstPage.body.next_cursor,
  });
  assert.equal(secondPage.body.requests.length, 1);
  assert.equal(secondPage.body.truncated, false);
  assert.equal(secondPage.body.next_cursor, null);

  await call(runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created[1].id,
    reason: "filter fixture",
    idempotency_key: "filter-reject",
  });
  const filtered = await call(runtime, "/request/admin-list", {
    actor: ADMIN,
    account_id: ACCOUNT,
    state: "rejected",
    domain: "PAGE-TWO.EXAMPLE",
  });
  assert.deepEqual(filtered.body.requests.map((request) => request.id), [
    created[1].id,
  ]);
  const badFilter = await call(runtime, "/request/admin-list", {
    actor: ADMIN,
    state: "active",
  });
  assert.equal(badFilter.response.status, 400);
  const tooLarge = await call(runtime, "/request/admin-list", {
    actor: ADMIN,
    limit: 101,
  });
  assert.equal(tooLarge.response.status, 400);
  const wrongCursor = await call(runtime, "/request/admin-list", {
    actor: ADMIN,
    cursor: firstPage.body.next_cursor,
  });
  assert.equal(wrongCursor.response.status, 400);

  const newestAudit = await call(runtime, "/audit/list", {
    actor: ADMIN,
    limit: 2,
  });
  assert.deepEqual(
    newestAudit.body.events.map((event) => event.sequence),
    [4, 3],
  );
  assert.equal(newestAudit.body.truncated, true);
  const olderAudit = await call(runtime, "/audit/list", {
    actor: ADMIN,
    limit: 2,
    cursor: newestAudit.body.next_cursor,
  });
  assert.deepEqual(
    olderAudit.body.events.map((event) => event.sequence),
    [2, 1],
  );
  assert.equal(olderAudit.body.truncated, false);

  const filteredAudit = await call(runtime, "/audit/list", {
    actor: ADMIN,
    account_id: ACCOUNT,
    domain: "PAGE-TWO.EXAMPLE",
    action: "custom_domain.rejected",
  });
  assert.deepEqual(filteredAudit.body.events.map((event) => event.sequence), [4]);
  const badAuditFilter = await call(runtime, "/audit/list", {
    actor: ADMIN,
    action: "custom_domain.activated",
  });
  assert.equal(badAuditFilter.response.status, 400);
});

test("only internal POST routes are accepted", async () => {
  const { runtime } = registry();
  const get = await call(runtime, "/request/list", {}, "GET");
  assert.equal(get.response.status, 404);
  const missing = await call(runtime, "/not-a-route", {});
  assert.equal(missing.response.status, 404);
  const invalid = await runtime.fetch(new Request(
    "https://agent-email-domain.internal/request/create",
    { method: "POST", body: "{" },
  ));
  assert.equal(invalid.status, 400);
});
