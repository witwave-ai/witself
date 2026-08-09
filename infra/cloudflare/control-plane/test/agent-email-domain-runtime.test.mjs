import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS,
  AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE,
  AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT,
  AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT,
  AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS,
  AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS,
  DurableAgentEmailDomainRegistry,
  agentEmailCustomDomainEntitlement,
  agentEmailCustomDomainOpenRequestLimit,
  agentEmailDomainRegistryStub,
  isProtectedAgentEmailDomain,
  normalizeAgentEmailCustomDomain,
  runAgentEmailDomainManualVerification,
  runScheduledAgentEmailDomainVerification,
} from "../src/agent-email-domain-runtime.mjs";
import {
  AgentEmailDomainVerificationError,
} from "../src/agent-email-domain-verification.mjs";
import {
  isAgentEmailDomainAuthorityKey,
  isAgentEmailDomainDerivedKey,
  replayAgentEmailDomainJournalPage,
  rebuildAgentEmailDomainDerivedState,
  validateAgentEmailDomainRecoveredState,
} from "../src/agent-email-domain-journal.mjs";
import {
  AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
} from "../src/agent-email-domain-journal-runtime.mjs";

const ACCOUNT = "acct_domain";
const OTHER_ACCOUNT = "acct_other";
const OPERATOR = { kind: "account_operator", id: "opr_domain" };
const OTHER_OPERATOR = { kind: "account_operator", id: "opr_other" };
const ADMIN = { kind: "platform_admin", id: "adm_domain" };
const PLAN_HASH = "7".repeat(64);
const VERIFICATION_CLAIM_LEASE_TEST_MS = 61 * 1_000;
const verificationResolvers = new WeakMap();

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

  async setAlarm(value) {
    this.alarm = value;
  }

  async deleteAlarm() {
    this.alarm = undefined;
  }

  async getAlarm() {
    return this.alarm;
  }
}

class Bucket {
  constructor() {
    this.values = new Map();
  }

  async put(key, value) {
    if (this.values.has(key)) return null;
    this.values.set(key, new Uint8Array(value));
    return { key };
  }

  async get(key) {
    const value = this.values.get(key);
    return value
      ? { arrayBuffer: async () => value.slice().buffer }
      : null;
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
      CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED: "true",
      CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST:
        `${ACCOUNT},${OTHER_ACCOUNT}`,
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
      assertRequestActivationReady: async () => {},
      ...dependencyOverrides,
    },
  );
  verificationResolvers.set(
    runtime,
    dependencyOverrides.resolveTXT ?? (async () => {
      throw new Error("verification test resolver is not configured");
    }),
  );
  return {
    runtime,
    storage,
    requestCount: () => requestSequence,
    challengeCount: () => challengeSequence,
    advanceTime: (milliseconds) => {
      currentTime += milliseconds;
    },
  };
}

function runtimeEnvironment(runtime) {
  return {
    ...runtime.env,
    AGENT_EMAIL_DOMAINS: {
      idFromName: (name) => name,
      get: () => ({ fetch: (request, init) => runtime.fetch(
        new Request(request, init),
      ) }),
    },
  };
}

async function call(runtime, path, body, method = "POST") {
  const resolver = verificationResolvers.get(runtime);
  const env = runtimeEnvironment(runtime);
  if (method === "POST" && path === "/request/verify") {
    const response = await runAgentEmailDomainManualVerification(
      env,
      body,
      { resolveTXT: resolver },
    );
    return { response, body: await response.json() };
  }
  if (method === "POST" && path === "/verification/reconcile") {
    if (env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED !== "true") {
      const response = await runtime.fetch(new Request(
        "https://agent-email-domain.internal/verification/claim",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            mode: "scheduled",
            verification_enabled: body?.verification_enabled === true,
          }),
        },
      ));
      return { response, body: await response.json() };
    }
    const result = await runScheduledAgentEmailDomainVerification(
      env,
      { resolveTXT: resolver },
    );
    const response = Response.json({ schema_version: "witself.agent-email-domain.v1", ...result });
    return { response, body: await response.json() };
  }
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

function auditRows(storage) {
  return [...storage.values].filter(([key]) => key.startsWith("audit:"));
}

async function replayBucketJournal(bucket, streamID) {
  const decoder = new TextDecoder();
  const entries = [...bucket.values.values()]
    .map((bytes) => JSON.parse(decoder.decode(bytes)))
    .sort((left, right) => left.sequence - right.sequence);
  return replayAgentEmailDomainJournalPage(entries, {
    stream_id: streamID,
    state: new Map(),
  });
}

async function claimAndObserve(runtime, request, idempotencyKey, fields = {}) {
  const claimed = await call(runtime, "/verification/claim", {
    mode: "manual",
    actor: ADMIN,
    request_id: request.id,
    idempotency_key: idempotencyKey,
    verification_enabled: true,
  });
  assert.equal(claimed.response.status, 200);
  assert.equal(claimed.body.kind, "claim");
  const observation = {
    kind: "resolved",
    matched: true,
    authoritative_absence: false,
    dnssec_authenticated: true,
    minimum_ttl_seconds: 60,
    rrset_sha256: "4".repeat(64),
    ...fields,
  };
  const observed = await call(runtime, "/verification/observe", {
    request_id: request.id,
    claim_id: claimed.body.claim.claim_id,
    generation: claimed.body.claim.generation,
    observation,
    verification_enabled: true,
  });
  assert.equal(observed.response.status, 200);
  return claimed.body.claim;
}

function assertDerivedParity(storage) {
  const authority = new Map(
    [...storage.values].filter(([key]) => isAgentEmailDomainAuthorityKey(key)),
  );
  const actual = [...storage.values]
    .filter(([key]) => isAgentEmailDomainDerivedKey(key))
    .sort(([left], [right]) => left.localeCompare(right));
  const expected = [...rebuildAgentEmailDomainDerivedState(authority)]
    .sort(([left], [right]) => left.localeCompare(right));
  assert.deepEqual(actual, expected);
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

test("empty journal bootstrap accepts its first request and plan completion", async () => {
  const bucket = new Bucket();
  const fixture = registry({
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED: "true",
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_STREAM_ID:
      `aedj_${"a".repeat(24)}`,
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL: bucket,
  });
  const maintenance = {
    actor: ADMIN,
    reason: "initialize empty registry integration test",
    idempotency_key: "initialize-empty-registry-integration",
  };
  let bootstrapped;
  for (let attempt = 0; attempt < 100; attempt++) {
    bootstrapped = await call(
      fixture.runtime,
      "/journal/bootstrap",
      maintenance,
    );
    assert.equal(bootstrapped.response.status, 200);
    if (bootstrapped.body.complete) break;
  }
  assert.equal(bootstrapped.body.complete, true);

  const created = await create(fixture.runtime, "first-journal.example", {
    domain_limit: 1,
  });
  assert.equal(created.response.status, 202);
  const plan = {
    account_id: ACCOUNT,
    activation_enabled: true,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  assert.equal((await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "restrict_only",
  })).response.status, 200);
  assert.equal((await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "complete",
  })).response.status, 200);
  assert.equal(fixture.storage.values.has(`plan-intent:${ACCOUNT}`), false);
  assert.ok(fixture.storage.values.has(`plan-fence:${ACCOUNT}`));

  let checkpoint;
  for (let attempt = 0; attempt < 100; attempt++) {
    checkpoint = await call(fixture.runtime, "/journal/checkpoint", {
      ...maintenance,
      idempotency_key: "checkpoint-after-first-request",
    });
    assert.equal(checkpoint.response.status, 200);
    if (checkpoint.body.complete) break;
  }
  assert.equal(checkpoint.body.complete, true);
  const status = await call(fixture.runtime, "/journal/status", {});
  assert.equal(status.body.healthy, true);
  assert.ok(status.body.head.sequence >= 1);
});

test("ordinary reads are empty and journal enforcement freezes old mutations", async () => {
  const fixture = registry();
  const seeded = await call(fixture.runtime, "/request/admin-list", {
    actor: ADMIN,
  });
  assert.equal(seeded.response.status, 200);
  assert.equal(fixture.storage.values.has("meta"), false);

  const oldMutation = await create(fixture.runtime, "old.example", {
    domain_limit: null,
  });
  assert.equal(oldMutation.response.status, 202);
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

test("authority rechecks a removed request gate before first mutation", async () => {
  const fixture = registry();
  fixture.runtime.env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST =
    OTHER_ACCOUNT;
  const removed = await create(fixture.runtime, "removed-gate.example", {
    domain_limit: null,
  });
  assert.equal(removed.response.status, 409);
  assert.equal(removed.body.code, "custom_domain_requests_disabled");
  assert.equal(fixture.storage.values.size, 0);
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
    expires_at: new Date(
      Date.parse(created.body.request.requested_at) + 7 * 24 * 60 * 60 * 1_000,
    ).toISOString(),
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

  const competing = await create(fixture.runtime, "CUSTOMER.EXAMPLE", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "other-claim",
  });
  assert.equal(competing.response.status, 202);

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
  assert.deepEqual(
    adminList.body.requests.map((request) => request.id),
    [created.body.request.id, competing.body.request.id],
  );

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

test("concurrent unverified challenges do not permanently squat a domain", async () => {
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
    [202, 202],
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

test("reject and retire release quota without tombstoning an unverified name", async () => {
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
    domain_limit: 2,
  });
  assert.equal(replacement.response.status, 202);

  const reusable = await create(runtime, "one.example", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "reuse-tombstone",
  });
  assert.equal(reusable.response.status, 202);

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

test("customer request activation fails closed before any authority mutation", async () => {
  const fixture = registry({}, { assertRequestActivationReady: undefined });
  const response = await create(fixture.runtime, "not-ready.example", {
    domain_limit: null,
  });
  assert.equal(response.response.status, 503);
  assert.equal(response.body.code, "custom_domain_activation_not_ready");
  assert.equal(fixture.storage.values.has("meta"), false);
  assert.equal(fixture.storage.values.has("request:aedr_aaaaaaaaaaaaaaab"), false);
});

test("request activation requires durable plan-lifecycle recovery", async () => {
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY: "true",
    CP_PLAN_LIFECYCLE_ENABLED: "false",
  }, { assertRequestActivationReady: undefined });
  fixture.runtime.authorityJournal.status = async () => ({
    required: true,
    enabled: true,
    healthy: true,
    pending: false,
    forked: false,
    head: { sequence: 1 },
  });
  const disabled = await create(
    fixture.runtime,
    "plan-recovery-disabled.example",
    { domain_limit: null },
  );
  assert.equal(disabled.response.status, 503);
  assert.equal(disabled.body.code, "custom_domain_activation_not_ready");
  assert.equal(fixture.storage.values.size, 0);

  fixture.runtime.env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  const enabled = await create(
    fixture.runtime,
    "plan-recovery-enabled.example",
    { domain_limit: null },
  );
  assert.equal(enabled.response.status, 202);
});

test("exact TXT proof atomically wins the verified allocation", async () => {
  let answers = [];
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      return {
        answers,
        authoritative_absence: answers.length === 0,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "e".repeat(64),
      };
    },
  });
  const first = await create(fixture.runtime, "proof.example", {
    domain_limit: null,
    idempotency_key: "proof-first",
  });
  const second = await create(fixture.runtime, "PROOF.EXAMPLE", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "proof-second",
  });
  assert.equal(first.response.status, 202);
  assert.equal(second.response.status, 202);

  answers = [first.body.request.ownership_challenge.record_value];
  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: first.body.request.id,
    idempotency_key: "verify-first",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);
  assert.equal(verified.body.request.state, "verified");
  assert.equal(verified.body.request.availability, "verified");
  assert.equal(verified.body.request.ownership_verification.state, "verified");
  assert.equal(
    fixture.storage.values.get("domain:proof.example").source_request_id,
    first.body.request.id,
  );

  fixture.runtime.env[
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED"
  ] = "false";
  const replay = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: first.body.request.id,
    idempotency_key: "verify-first",
    verification_enabled: true,
  });
  assert.equal(replay.response.status, 200);
  assert.equal(lookups, 1);

  fixture.runtime.env[
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED"
  ] = "true";
  answers = [second.body.request.ownership_challenge.record_value];
  const loser = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: second.body.request.id,
    idempotency_key: "verify-second",
    verification_enabled: true,
  });
  assert.equal(loser.response.status, 409);
  assert.equal(loser.body.code, "domain_unavailable");
  assert.equal(fixture.storage.values.get(
    `request:${second.body.request.id}`,
  ).state, "pending_verification");
  assert.equal(fixture.storage.values.get(
    `request:${second.body.request.id}`,
  ).ownership_verification.state, "conflict");
  assert.equal(lookups, 2);

  fixture.runtime.env[
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED"
  ] = "false";
  const loserReplay = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: second.body.request.id,
    idempotency_key: "verify-second",
    verification_enabled: true,
  });
  assert.equal(loserReplay.response.status, 409);
  assert.deepEqual(loserReplay.body, loser.body);
  assert.equal(lookups, 2);
  assert.doesNotThrow(() => validateAgentEmailDomainRecoveredState(new Map(
    [...fixture.storage.values].filter(([key]) =>
      isAgentEmailDomainAuthorityKey(key)),
  )));
});

test("manual authoritative absence is durably idempotent", async () => {
  let answers = [];
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      return {
        answers,
        authoritative_absence: answers.length === 0,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "f".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "missing-proof.example", {
    domain_limit: null,
  });
  const input = {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-missing-proof",
    verification_enabled: true,
  };
  const missing = await call(fixture.runtime, "/request/verify", input);
  assert.equal(missing.response.status, 409);
  assert.equal(missing.body.code, "ownership_challenge_not_found");
  const afterMissing = structuredClone([...fixture.storage.values.entries()]);

  answers = [created.body.request.ownership_challenge.record_value];
  const replay = await call(fixture.runtime, "/request/verify", input);
  assert.equal(replay.response.status, 409);
  assert.deepEqual(replay.body, missing.body);
  assert.equal(lookups, 1);
  assert.deepEqual([...fixture.storage.values.entries()], afterMissing);

  const verified = await call(fixture.runtime, "/request/verify", {
    ...input,
    idempotency_key: "verify-missing-proof-again",
  });
  assert.equal(verified.response.status, 200);
  assert.equal(lookups, 2);
  assert.doesNotThrow(() => validateAgentEmailDomainRecoveredState(new Map(
    [...fixture.storage.values].filter(([key]) =>
      isAgentEmailDomainAuthorityKey(key)),
  )));
});

test("new pending challenges enter scheduled verification without a manual kick", async () => {
  let expectedValue = null;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => ({
      answers: [expectedValue],
      authoritative_absence: false,
      dnssec_authenticated: false,
      minimum_ttl_seconds: 60,
      rrset_sha256: "d".repeat(64),
    }),
  });
  const created = await create(fixture.runtime, "automatic-proof.example", {
    domain_limit: 1,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  assert.ok(fixture.storage.alarm);

  const reconciled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(reconciled.response.status, 200);
  assert.deepEqual(
    { checked: reconciled.body.checked, matched: reconciled.body.matched },
    { checked: 1, matched: 1 },
  );
  assert.equal(
    fixture.storage.values.get(`request:${created.body.request.id}`).state,
    "verified",
  );
});

test("an unresolved DNS lookup does not hold the serialized authority lane", async () => {
  let releaseDNS;
  let markDNSStarted;
  const dnsStarted = new Promise((resolve) => {
    markDNSStarted = resolve;
  });
  const dnsReleased = new Promise((resolve) => {
    releaseDNS = resolve;
  });
  let expectedValue;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      markDNSStarted();
      await dnsReleased;
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "1".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "nonblocking-dns.example", {
    domain_limit: null,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const verification = call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "nonblocking-dns",
    verification_enabled: true,
  });
  await dnsStarted;

  const shown = await Promise.race([
    call(fixture.runtime, "/request/get", {
      actor: ADMIN,
      request_id: created.body.request.id,
    }),
    new Promise((_, reject) => setTimeout(
      () => reject(new Error("registry read was blocked by DNS")),
      100,
    )),
  ]);
  assert.equal(shown.response.status, 200);
  assert.equal(shown.body.request.state, "pending_verification");

  releaseDNS();
  const verified = await verification;
  assert.equal(verified.response.status, 200);
  assert.equal(verified.body.request.state, "verified");
});

test("concurrent manual replay performs only one DNS lookup", async () => {
  let releaseDNS;
  let markDNSStarted;
  let lookups = 0;
  let expectedValue;
  const dnsStarted = new Promise((resolve) => {
    markDNSStarted = resolve;
  });
  const dnsReleased = new Promise((resolve) => {
    releaseDNS = resolve;
  });
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      markDNSStarted();
      await dnsReleased;
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "9".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "manual-collision.example", {
    domain_limit: null,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const input = {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "manual-collision",
    verification_enabled: true,
  };
  const first = call(fixture.runtime, "/request/verify", input);
  await dnsStarted;

  const concurrent = await call(fixture.runtime, "/request/verify", input);
  assert.equal(concurrent.response.status, 409);
  assert.equal(concurrent.body.code, "verification_in_progress");
  assert.equal(lookups, 1);

  releaseDNS();
  const completed = await first;
  assert.equal(completed.response.status, 200);
  assert.equal(lookups, 1);
  const replay = await call(fixture.runtime, "/request/verify", input);
  assert.equal(replay.response.status, 200);
  assert.deepEqual(replay.body, completed.body);
  assert.equal(lookups, 1);
});

test("overlapping scheduled runners resolve each due request exactly once", async () => {
  const expected = new Map();
  const calls = [];
  let releaseDNS;
  let markAllStarted;
  const allStarted = new Promise((resolve) => {
    markAllStarted = resolve;
  });
  const dnsReleased = new Promise((resolve) => {
    releaseDNS = resolve;
  });
  const resolver = async (owner) => {
    calls.push(owner);
    if (new Set(calls).size === 3) markAllStarted();
    await dnsReleased;
    return {
      answers: [expected.get(owner)],
      authoritative_absence: false,
      dnssec_authenticated: false,
      minimum_ttl_seconds: 60,
      rrset_sha256: "2".repeat(64),
    };
  };
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, { resolveTXT: resolver });
  const created = [];
  for (const domain of [
    "parallel-one.example",
    "parallel-two.example",
    "parallel-three.example",
  ]) {
    const result = await create(fixture.runtime, domain, {
      domain_limit: null,
      idempotency_key: `create-${domain}`,
    });
    created.push(result.body.request);
    expected.set(
      result.body.request.ownership_challenge.record_name,
      result.body.request.ownership_challenge.record_value,
    );
  }
  const env = runtimeEnvironment(fixture.runtime);
  const first = runScheduledAgentEmailDomainVerification(
    env,
    { resolveTXT: resolver },
  );
  const second = runScheduledAgentEmailDomainVerification(
    env,
    { resolveTXT: resolver },
  );
  await allStarted;
  assert.equal(calls.length, 3);
  assert.equal(new Set(calls).size, 3);
  releaseDNS();
  const results = await Promise.all([first, second]);
  assert.equal(results.reduce((sum, item) => sum + item.matched, 0), 3);
  assert.equal(results.reduce((sum, item) => sum + item.failures, 0), 0);
  for (const request of created) {
    assert.equal(
      fixture.storage.values.get(`request:${request.id}`).state,
      "verified",
    );
  }
});

test("identical scheduled proof refreshes are journaled without growing audit authority", async () => {
  const streamID = `aedj_${"b".repeat(24)}`;
  const bucket = new Bucket();
  let expectedValue;
  let minimumTTL = 300;
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED: "true",
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_STREAM_ID: streamID,
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL: bucket,
  }, {
    resolveTXT: async () => {
      lookups += 1;
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: minimumTTL,
        rrset_sha256: "5".repeat(64),
      };
    },
  });
  const created = await create(
    fixture.runtime,
    "coalesced-proof.example",
    { domain_limit: 1 },
  );
  assert.equal(created.response.status, 202);
  expectedValue = created.body.request.ownership_challenge.record_value;
  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "coalesced-proof-initial",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);

  const requestKey = `request:${created.body.request.id}`;
  const domainKey = "domain:coalesced-proof.example";
  const beforeRequest = structuredClone(fixture.storage.values.get(requestKey));
  const beforeAllocation = structuredClone(
    fixture.storage.values.get(domainKey),
  );
  const beforeMeta = structuredClone(fixture.storage.values.get("meta"));
  const beforeHead = structuredClone(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
  );
  const beforeCapacity = structuredClone(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY),
  );
  const beforeAuditCount = auditRows(fixture.storage).length;

  minimumTTL = 240;
  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS + 1) *
      60 * 60 * 1_000,
  );
  const scheduled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(scheduled.response.status, 200);
  assert.equal(scheduled.body.matched, 1);

  const refreshed = fixture.storage.values.get(requestKey);
  const refreshedAllocation = fixture.storage.values.get(domainKey);
  const afterHead = fixture.storage.values.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
  );
  const afterCapacity = fixture.storage.values.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  assert.ok(Date.parse(refreshed.ownership_verification.last_checked_at) >
    Date.parse(beforeRequest.ownership_verification.last_checked_at));
  assert.ok(Date.parse(refreshed.ownership_verification.last_verified_at) >
    Date.parse(beforeRequest.ownership_verification.last_verified_at));
  assert.equal(refreshed.state_revision, beforeRequest.state_revision + 1);
  assert.equal(refreshed.ownership_verification.minimum_ttl_seconds, 240);
  assert.equal(
    refreshedAllocation.allocation_revision,
    beforeAllocation.allocation_revision + 1,
  );
  assert.deepEqual(fixture.storage.values.get("meta"), beforeMeta);
  assert.equal(auditRows(fixture.storage).length, beforeAuditCount);
  assert.equal(afterHead.sequence, beforeHead.sequence + 1);
  assert.equal(afterHead.registry_revision, beforeHead.registry_revision);
  assert.equal(afterHead.audit_sequence, beforeHead.audit_sequence);
  assert.equal(afterCapacity.authority_keys, beforeCapacity.authority_keys);
  assert.deepEqual(afterCapacity.breakdown, beforeCapacity.breakdown);

  const beforeManualAuditCount = auditRows(fixture.storage).length;
  const manual = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "coalesced-proof-manual-refresh",
    verification_enabled: true,
  });
  assert.equal(manual.response.status, 200);
  assert.equal(auditRows(fixture.storage).length, beforeManualAuditCount + 1);
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.reverified",
  );
  const beforeReplayHead = structuredClone(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
  );
  const beforeReplayCapacity = structuredClone(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY),
  );
  const beforeReplayBucketSize = bucket.values.size;
  const replay = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "coalesced-proof-manual-refresh",
    verification_enabled: true,
  });
  assert.equal(replay.response.status, 200);
  assert.deepEqual(replay.body, manual.body);
  assert.deepEqual(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    beforeReplayHead,
  );
  assert.deepEqual(
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY),
    beforeReplayCapacity,
  );
  assert.equal(bucket.values.size, beforeReplayBucketSize);
  assert.equal(lookups, 3);

  const recovered = await replayBucketJournal(bucket, streamID);
  assert.deepEqual(
    recovered.state.get(requestKey),
    fixture.storage.values.get(requestKey),
  );
  assert.deepEqual(
    recovered.state.get(domainKey),
    fixture.storage.values.get(domainKey),
  );
  assert.equal(
    recovered.head.sequence,
    fixture.storage.values.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY).sequence,
  );
  assert.doesNotThrow(() => validateAgentEmailDomainRecoveredState(
    recovered.state,
    {
      expected_registry_revision: recovered.head.registry_revision,
      expected_audit_sequence: recovered.head.audit_sequence,
    },
  ));
});

test("only scheduled verification evidence transitions append audit rows", async () => {
  let mode = "present";
  let expectedValue;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      if (mode === "temporary") {
        throw new AgentEmailDomainVerificationError(
          "DNS resolver is temporarily unavailable",
          "dns_resolver_unavailable",
          true,
        );
      }
      return {
        answers: mode === "present" ? [expectedValue] : [],
        authoritative_absence: mode === "absent",
        dnssec_authenticated: mode === "present",
        minimum_ttl_seconds: mode === "present" ? 300 : null,
        rrset_sha256: (mode === "present" ? "6" : "7").repeat(64),
      };
    },
  });
  const created = await create(
    fixture.runtime,
    "coalesced-transitions.example",
    { domain_limit: 1 },
  );
  expectedValue = created.body.request.ownership_challenge.record_value;
  assert.equal((await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "coalesced-transitions-initial",
    verification_enabled: true,
  })).response.status, 200);
  const requestKey = `request:${created.body.request.id}`;

  mode = "temporary";
  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS + 1) *
      60 * 60 * 1_000,
  );
  await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  const firstTemporaryAuditCount = auditRows(fixture.storage).length;
  const firstTemporary = structuredClone(fixture.storage.values.get(requestKey));
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.verification_deferred",
  );

  fixture.advanceTime(16 * 60 * 1_000);
  await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  const secondTemporary = structuredClone(fixture.storage.values.get(requestKey));
  assert.equal(auditRows(fixture.storage).length, firstTemporaryAuditCount);
  assert.ok(Date.parse(secondTemporary.ownership_verification.last_checked_at) >
    Date.parse(firstTemporary.ownership_verification.last_checked_at));
  assert.equal(
    secondTemporary.ownership_verification.consecutive_failures,
    firstTemporary.ownership_verification.consecutive_failures + 1,
  );

  mode = "absent";
  fixture.advanceTime(16 * 60 * 1_000);
  await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  const firstMissingAuditCount = auditRows(fixture.storage).length;
  const firstMissing = structuredClone(fixture.storage.values.get(requestKey));
  assert.equal(firstMissingAuditCount, firstTemporaryAuditCount + 1);
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.verification_missing",
  );
  assert.equal(firstMissing.ownership_verification.state, "stale");

  fixture.advanceTime(61 * 60 * 1_000);
  await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  const secondMissing = structuredClone(fixture.storage.values.get(requestKey));
  assert.equal(auditRows(fixture.storage).length, firstMissingAuditCount);
  assert.ok(Date.parse(secondMissing.ownership_verification.last_checked_at) >
    Date.parse(firstMissing.ownership_verification.last_checked_at));
  assert.equal(
    secondMissing.ownership_verification.consecutive_failures,
    firstMissing.ownership_verification.consecutive_failures + 1,
  );

  mode = "present";
  fixture.advanceTime(61 * 60 * 1_000);
  await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  const restored = fixture.storage.values.get(requestKey);
  assert.equal(auditRows(fixture.storage).length, firstMissingAuditCount + 1);
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.reverified",
  );
  assert.equal(restored.ownership_verification.state, "verified");
  assert.equal(restored.ownership_verification.consecutive_failures, 0);
  assert.doesNotThrow(() => validateAgentEmailDomainRecoveredState(new Map(
    [...fixture.storage.values].filter(([key]) =>
      isAgentEmailDomainAuthorityKey(key)),
  )));
});

test("an expired claim fence cannot alter a newer verification claim", async () => {
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  });
  const created = await create(fixture.runtime, "fenced-worker.example", {
    domain_limit: null,
  });
  const first = await call(fixture.runtime, "/verification/claim", {
    mode: "scheduled",
    verification_enabled: true,
  });
  assert.equal(first.body.kind, "claim");
  fixture.advanceTime(VERIFICATION_CLAIM_LEASE_TEST_MS);
  const second = await call(fixture.runtime, "/verification/claim", {
    mode: "scheduled",
    verification_enabled: true,
  });
  assert.equal(second.body.kind, "claim");
  assert.ok(second.body.claim.generation > first.body.claim.generation);
  assert.notEqual(second.body.claim.claim_id, first.body.claim.claim_id);

  const stale = await call(fixture.runtime, "/verification/observe", {
    request_id: created.body.request.id,
    claim_id: first.body.claim.claim_id,
    generation: first.body.claim.generation,
    verification_enabled: true,
    observation: {
      kind: "resolved",
      matched: false,
      authoritative_absence: true,
      dnssec_authenticated: false,
      minimum_ttl_seconds: null,
      rrset_sha256: "3".repeat(64),
    },
  });
  assert.equal(stale.response.status, 409);
  assert.equal(stale.body.code, "verification_claim_stale");
  assert.equal(
    fixture.storage.values.get(
      `verification-work:${created.body.request.id}`,
    ).claim_id,
    second.body.claim.claim_id,
  );
});

test("verification commit rechecks gate, request revision, and challenge expiry", async () => {
  let gatedValue;
  let gatedLookups = 0;
  const gated = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      gatedLookups += 1;
      return {
        answers: [gatedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "8".repeat(64),
      };
    },
  });
  const gatedRequest = await create(
    gated.runtime,
    "commit-gate.example",
    { domain_limit: null },
  );
  gatedValue = gatedRequest.body.request.ownership_challenge.record_value;
  const gatedClaim = await claimAndObserve(
    gated.runtime,
    gatedRequest.body.request,
    "commit-gate",
  );
  delete gated.runtime.env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED;
  const gateCommit = await call(gated.runtime, "/verification/commit", {
    request_id: gatedRequest.body.request.id,
    claim_id: gatedClaim.claim_id,
    generation: gatedClaim.generation,
    verification_enabled: true,
  });
  assert.equal(gateCommit.response.status, 409);
  assert.equal(gateCommit.body.code, "custom_domain_verification_disabled");
  assert.equal(gated.storage.values.has("domain:commit-gate.example"), false);
  assert.equal(gated.storage.values.has(
    `verification-work:${gatedRequest.body.request.id}`,
  ), false);
  gated.runtime.env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED = "true";
  const freshAfterEnable = await call(gated.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: gatedRequest.body.request.id,
    idempotency_key: "commit-gate",
    verification_enabled: true,
  });
  assert.equal(freshAfterEnable.response.status, 200);
  assert.equal(gatedLookups, 1, "re-enabled verification must resolve afresh");

  const revised = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  });
  const revisedRequest = await create(
    revised.runtime,
    "commit-revision.example",
    { domain_limit: null },
  );
  const revisedClaim = await claimAndObserve(
    revised.runtime,
    revisedRequest.body.request,
    "commit-revision",
  );
  const rejected = await call(revised.runtime, "/request/reject", {
    actor: ADMIN,
    request_id: revisedRequest.body.request.id,
    idempotency_key: "reject-during-verification",
    reason: "request changed while DNS was being observed",
  });
  assert.equal(rejected.response.status, 200);
  const revisionCommit = await call(revised.runtime, "/verification/commit", {
    request_id: revisedRequest.body.request.id,
    claim_id: revisedClaim.claim_id,
    generation: revisedClaim.generation,
    verification_enabled: true,
  });
  assert.equal(revisionCommit.response.status, 409);
  assert.equal(revisionCommit.body.code, "verification_claim_stale");
  assert.equal(
    revised.storage.values.has("domain:commit-revision.example"),
    false,
  );

  const expired = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  });
  const expiredRequest = await create(
    expired.runtime,
    "commit-expiry.example",
    { domain_limit: null },
  );
  const expiredClaim = await claimAndObserve(
    expired.runtime,
    expiredRequest.body.request,
    "commit-expiry",
  );
  expired.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  const expiryCommit = await call(expired.runtime, "/verification/commit", {
    request_id: expiredRequest.body.request.id,
    claim_id: expiredClaim.claim_id,
    generation: expiredClaim.generation,
    verification_enabled: true,
  });
  assert.equal(expiryCommit.response.status, 200);
  assert.equal(expiryCommit.body.kind, "result");
  assert.equal(expiryCommit.body.status, 409);
  assert.equal(expiryCommit.body.body.code, "ownership_challenge_expired");
  assert.equal(expired.storage.values.has("domain:commit-expiry.example"), false);
  assert.equal(
    expired.storage.values.get(
      `request:${expiredRequest.body.request.id}`,
    ).state,
    "expired",
  );
});

test("a losing domain contender cannot block later scheduled verification", async () => {
  const answersByOwner = new Map();
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async (owner) => ({
      answers: answersByOwner.get(owner) ?? [],
      authoritative_absence: !answersByOwner.has(owner),
      dnssec_authenticated: false,
      minimum_ttl_seconds: 60,
      rrset_sha256: "9".repeat(64),
    }),
  });
  const winner = await create(fixture.runtime, "contended.example", {
    account_id: ACCOUNT,
    domain_limit: null,
    idempotency_key: "contender-winner",
  });
  const loser = await create(fixture.runtime, "contended.example", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "contender-loser",
  });
  const later = await create(fixture.runtime, "later-proof.example", {
    domain_limit: null,
    idempotency_key: "contender-later",
  });
  answersByOwner.set(
    winner.body.request.ownership_challenge.record_name,
    [
      winner.body.request.ownership_challenge.record_value,
      loser.body.request.ownership_challenge.record_value,
    ],
  );
  answersByOwner.set(
    later.body.request.ownership_challenge.record_name,
    [later.body.request.ownership_challenge.record_value],
  );
  const won = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: winner.body.request.id,
    idempotency_key: "verify-contender-winner",
    verification_enabled: true,
  });
  assert.equal(won.response.status, 200);

  const reconciled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(reconciled.response.status, 200);
  assert.deepEqual(
    { checked: reconciled.body.checked, matched: reconciled.body.matched },
    { checked: 2, matched: 1 },
  );
  assert.equal(
    fixture.storage.values.get(`request:${loser.body.request.id}`).state,
    "pending_verification",
  );
  assert.equal(
    fixture.storage.values.get(`request:${loser.body.request.id}`)
      .ownership_verification.state,
    "conflict",
  );
  assert.equal(
    fixture.storage.values.get(`request:${later.body.request.id}`).state,
    "verified",
  );
  assert.equal(
    [...fixture.storage.values.keys()].some((key) =>
      key.startsWith("verification-due:") && key.endsWith(loser.body.request.id)
    ),
    true,
  );
  assertDerivedParity(fixture.storage);
});

test("legacy pending mirrors convert to allocations on verification", async () => {
  let expectedValue = null;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => ({
      answers: [expectedValue],
      authoritative_absence: false,
      dnssec_authenticated: true,
      minimum_ttl_seconds: 300,
      rrset_sha256: "f".repeat(64),
    }),
  });
  const created = await create(fixture.runtime, "legacy-proof.example", {
    domain_limit: 1,
  });
  const storedRequest = fixture.storage.values.get(
    `request:${created.body.request.id}`,
  );
  delete storedRequest.ownership_challenge.expires_at;
  for (const field of [
    "state_revision", "plan_suspended", "plan_grace_until",
    "lifecycle_suspended", "lifecycle_fence", "ownership_verification",
    "expiration",
  ]) delete storedRequest[field];
  fixture.storage.values.set(
    `request:${created.body.request.id}`,
    structuredClone(storedRequest),
  );
  fixture.storage.values.set(
    "domain:legacy-proof.example",
    structuredClone(storedRequest),
  );
  expectedValue = created.body.request.ownership_challenge.record_value;

  const legacyShown = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.ok(legacyShown.body.request.ownership_challenge.expires_at);

  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-legacy-pending",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);
  assert.equal(verified.body.request.state, "verified");
  const allocation = fixture.storage.values.get(
    "domain:legacy-proof.example",
  );
  assert.equal(
    allocation.schema_version,
    "witself.agent-email-domain-allocation.v1",
  );
  assert.equal(allocation.source_request_id, created.body.request.id);
  assert.equal(allocation.state, "allocated");
  assert.equal(allocation.ownership_proof.rrset_sha256, "f".repeat(64));
});

test("legacy pending mirrors remain coherent on rejection", async () => {
  const fixture = registry();
  const created = await create(fixture.runtime, "legacy-reject.example", {
    domain_limit: 1,
  });
  const storedRequest = fixture.storage.values.get(
    `request:${created.body.request.id}`,
  );
  delete storedRequest.ownership_challenge.expires_at;
  for (const field of [
    "state_revision", "plan_suspended", "plan_grace_until",
    "lifecycle_suspended", "lifecycle_fence", "ownership_verification",
    "expiration",
  ]) delete storedRequest[field];
  fixture.storage.values.set(
    `request:${created.body.request.id}`,
    structuredClone(storedRequest),
  );
  fixture.storage.values.set(
    "domain:legacy-reject.example",
    structuredClone(storedRequest),
  );

  const rejected = await call(fixture.runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "reject-legacy-pending",
    reason: "legacy request rejected",
  });
  assert.equal(rejected.response.status, 200);
  assert.equal(rejected.body.request.state, "rejected");
  assert.deepEqual(
    fixture.storage.values.get("domain:legacy-reject.example"),
    fixture.storage.values.get(`request:${created.body.request.id}`),
  );
  const usage = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(usage.body.open_requests, 0);
  assert.equal(usage.body.allocated_domains, 0);

  const blockedReuse = await create(fixture.runtime, "legacy-reject.example", {
    actor: OTHER_OPERATOR,
    account_id: OTHER_ACCOUNT,
    domain_limit: null,
    idempotency_key: "reuse-legacy-reject",
  });
  assert.equal(blockedReuse.response.status, 409);
  assert.equal(blockedReuse.body.code, "domain_unavailable");
});

test("verification gate off performs no lookup or authority mutation", async () => {
  let lookups = 0;
  const fixture = registry({}, {
    resolveTXT: async () => {
      lookups += 1;
      throw new Error("disabled verification must not resolve DNS");
    },
  });
  const created = await create(fixture.runtime, "dark-proof.example", {
    domain_limit: null,
  });
  assert.equal(created.response.status, 202);
  const before = structuredClone([...fixture.storage.values.entries()]);

  const manual = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-dark",
    verification_enabled: true,
  });
  assert.equal(manual.response.status, 409);
  assert.equal(manual.body.code, "custom_domain_verification_disabled");

  const scheduled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(scheduled.response.status, 409);
  assert.equal(scheduled.body.code, "custom_domain_verification_disabled");
  assert.equal(lookups, 0);
  assert.deepEqual([...fixture.storage.values.entries()], before);

  let registryLookups = 0;
  const topLevel = await runScheduledAgentEmailDomainVerification({
    AGENT_EMAIL_DOMAINS: {
      idFromName() {
        registryLookups += 1;
        throw new Error("dark scheduler must not resolve the registry");
      },
    },
  });
  assert.deepEqual(topLevel, { ran: false, configured: true });
  assert.equal(registryLookups, 0);
});

test("temporary resolver failures preserve a verified allocation", async () => {
  let mode = "present";
  let expectedValue = null;
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      if (mode === "temporary") {
        throw new AgentEmailDomainVerificationError(
          "DNS resolver is temporarily unavailable",
          "dns_resolver_unavailable",
          true,
        );
      }
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 300,
        rrset_sha256: "a".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "resolver-outage.example", {
    domain_limit: 1,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-before-outage",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);
  const allocationBefore = structuredClone(
    fixture.storage.values.get("domain:resolver-outage.example"),
  );

  mode = "temporary";
  const authorityBeforeManualRetry = structuredClone(
    [...fixture.storage.values.entries()],
  );
  const manualRetry = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-during-outage",
    verification_enabled: true,
  });
  assert.equal(manualRetry.response.status, 503);
  assert.equal(manualRetry.body.code, "dns_resolver_unavailable");
  assert.deepEqual(
    [...fixture.storage.values.entries()],
    authorityBeforeManualRetry,
  );

  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS + 1) *
      60 * 60 * 1_000,
  );
  const reconciled = await call(
    fixture.runtime,
    "/verification/reconcile",
    { verification_enabled: true },
  );
  assert.equal(reconciled.response.status, 200);
  assert.deepEqual(
    { checked: reconciled.body.checked, matched: reconciled.body.matched },
    { checked: 1, matched: 0 },
  );

  const shown = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(shown.body.request.state, "verified");
  assert.equal(shown.body.request.availability, "verified");
  assert.equal(shown.body.request.ownership_verification.state, "verified");
  assert.equal(
    shown.body.request.ownership_verification.last_result,
    "resolver_error",
  );
  assert.deepEqual(
    fixture.storage.values.get("domain:resolver-outage.example"),
    allocationBefore,
  );
  const usage = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(usage.body.open_requests, 0);
  assert.equal(usage.body.allocated_domains, 1);
  assert.equal(lookups, 3);
  assertDerivedParity(fixture.storage);
});

test("authoritative TXT loss suspends and later proof restores availability", async () => {
  let mode = "present";
  let expectedValue = null;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => ({
      answers: mode === "present" ? [expectedValue] : [],
      authoritative_absence: mode !== "present",
      dnssec_authenticated: mode === "present",
      minimum_ttl_seconds: mode === "present" ? 300 : null,
      rrset_sha256: (mode === "present" ? "b" : "c").repeat(64),
    }),
  });
  const created = await create(fixture.runtime, "txt-loss.example", {
    domain_limit: 1,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-txt-loss",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);
  const allocationBefore = structuredClone(
    fixture.storage.values.get("domain:txt-loss.example"),
  );

  mode = "absent";
  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS + 1) *
      60 * 60 * 1_000,
  );
  const missing = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(missing.response.status, 200);
  assert.equal(missing.body.checked, 1);
  assert.equal(missing.body.matched, 0);

  const stale = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(stale.body.request.state, "verified");
  assert.equal(stale.body.request.availability, "suspended_verification");
  assert.equal(stale.body.request.ownership_verification.state, "stale");
  assert.equal(stale.body.request.ownership_verification.last_result, "absent");
  assert.equal(
    fixture.storage.values.get("domain:txt-loss.example").state,
    "allocated",
  );
  assert.deepEqual(
    fixture.storage.values.get("domain:txt-loss.example").ownership_proof,
    allocationBefore.ownership_proof,
  );

  mode = "present";
  fixture.advanceTime(61 * 60 * 1_000);
  const restored = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(restored.body.checked, 1);
  assert.equal(restored.body.matched, 1);
  const active = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(active.body.request.availability, "verified");
  assert.equal(active.body.request.ownership_verification.state, "verified");
  assert.equal(active.body.request.ownership_verification.consecutive_failures, 0);
  assertDerivedParity(fixture.storage);
});

test("verified allocations receive downgrade grace before suspension", async () => {
  let expectedValue = null;
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 300,
        rrset_sha256: "d".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "downgrade-grace.example", {
    domain_limit: 1,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const verified = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-downgrade",
    verification_enabled: true,
  });
  assert.equal(verified.response.status, 200);

  const plan = {
    account_id: ACCOUNT,
    feature_enabled: false,
    domain_limit: 0,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  assert.equal((await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "restrict_only",
  })).response.status, 200);
  const downgraded = await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "complete",
  });
  assert.equal(downgraded.response.status, 200);
  assert.equal(downgraded.body.complete, true);

  const inGrace = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(inGrace.body.request.state, "verified");
  assert.equal(inGrace.body.request.plan_suspended, false);
  assert.equal(inGrace.body.request.availability, "active_grace");
  const graceLength = Date.parse(inGrace.body.request.plan_grace_until) -
    Date.parse(inGrace.body.request.updated_at);
  const expectedGrace = AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS *
    24 * 60 * 60 * 1_000;
  assert.ok(graceLength <= expectedGrace);
  assert.ok(graceLength >= expectedGrace - 5_000);

  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  await fixture.runtime.alarm();
  const suspended = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(suspended.body.request.state, "verified");
  assert.equal(suspended.body.request.plan_suspended, true);
  assert.equal(suspended.body.request.availability, "suspended_plan");
  assert.equal("plan_grace_until" in suspended.body.request, false);
  assert.equal(
    fixture.storage.values.get("domain:downgrade-grace.example").state,
    "allocated",
  );
  const usage = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(usage.body.open_requests, 0);
  assert.equal(usage.body.allocated_domains, 1);
  assert.equal(lookups, 1);
  assertDerivedParity(fixture.storage);
});

test("completed plan reconciliation replays without authority mutation", async () => {
  const fixture = registry();
  const created = await create(fixture.runtime, "plan-replay.example", {
    domain_limit: 1,
  });
  assert.equal(created.response.status, 202);
  const plan = {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    mode: "complete",
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  const first = await call(fixture.runtime, "/plan/reconcile", plan);
  assert.equal(first.response.status, 200);
  assert.equal(first.body.complete, true);
  const completedState = structuredClone([...fixture.storage.values.entries()]);

  const replay = await call(fixture.runtime, "/plan/reconcile", plan);
  assert.equal(replay.response.status, 200);
  assert.equal(replay.body.complete, true);
  assert.equal(replay.body.stale, false);
  assert.equal(replay.body.changed, 0);
  assert.equal(replay.body.registry_revision, first.body.registry_revision);
  assert.deepEqual([...fixture.storage.values.entries()], completedState);

  const conflictingEntitlement = await call(
    fixture.runtime,
    "/plan/reconcile",
    { ...plan, domain_limit: 0 },
  );
  assert.equal(conflictingEntitlement.response.status, 409);
  const conflictingHash = await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    plan_snapshot_hash: "9".repeat(64),
  });
  assert.equal(conflictingHash.response.status, 409);
  assert.deepEqual([...fixture.storage.values.entries()], completedState);
});

test("accounts without domain history keep plan and lifecycle truly dark", async () => {
  const fixture = registry();
  const before = structuredClone([...fixture.storage.values.entries()]);
  for (const mode of ["restrict_only", "complete"]) {
    const plan = await call(fixture.runtime, "/plan/reconcile", {
      account_id: ACCOUNT,
      feature_enabled: true,
      domain_limit: 1,
      mode,
      plan_revision: 8,
      plan_snapshot_hash: "8".repeat(64),
    });
    assert.equal(plan.response.status, 200);
    assert.equal(plan.body.complete, true);
    assert.equal(plan.body.no_op, true);
  }
  for (const action of ["suspend", "republish", "retire"]) {
    const lifecycle = await call(
      fixture.runtime,
      "/account-lifecycle/reconcile",
      {
        account_id: ACCOUNT,
        operation_id: "empty-account-lifecycle",
        epoch: 1,
        action,
      },
    );
    assert.equal(lifecycle.response.status, 200);
    assert.equal(lifecycle.body.complete, true);
    assert.equal(lifecycle.body.no_op, true);
  }
  assert.deepEqual([...fixture.storage.values.entries()], before);
  assert.equal(fixture.storage.alarm, undefined);
});

test("terminal-only history does not amplify dark policy writes", async () => {
  const fixture = registry();
  const created = await create(fixture.runtime, "terminal-history.example", {
    domain_limit: null,
  });
  await call(fixture.runtime, "/request/reject", {
    actor: ADMIN,
    request_id: created.body.request.id,
    reason: "finish terminal history",
    idempotency_key: "terminal-history-reject",
  });
  const before = structuredClone([...fixture.storage.values.entries()]);
  const plan = await call(fixture.runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    mode: "complete",
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  });
  assert.equal(plan.body.no_op, true);
  const lifecycle = await call(
    fixture.runtime,
    "/account-lifecycle/reconcile",
    {
      account_id: ACCOUNT,
      operation_id: "terminal-history-suspend",
      epoch: 1,
      action: "suspend",
    },
  );
  assert.equal(lifecycle.body.no_op, true);
  assert.deepEqual([...fixture.storage.values.entries()], before);
});

test("activated empty accounts persist policy fences before creation", async () => {
  const planFixture = registry();
  const plan = {
    account_id: ACCOUNT,
    activation_enabled: true,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  const restricted = await call(planFixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "restrict_only",
  });
  assert.equal(restricted.response.status, 200);
  assert.equal(restricted.body.pending, true);
  assert.ok(planFixture.storage.values.has(`plan-intent:${ACCOUNT}`));
  const completed = await call(planFixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "complete",
  });
  assert.equal(completed.response.status, 200);
  assert.equal(completed.body.complete, true);
  assert.ok(planFixture.storage.values.has(`plan-fence:${ACCOUNT}`));
  const stalePlan = await create(planFixture.runtime, "stale-plan.example", {
    domain_limit: 1,
  });
  assert.equal(stalePlan.response.status, 409);
  assert.equal(stalePlan.body.code, "stale_plan_fence");

  const lifecycleFixture = registry();
  const retired = await call(
    lifecycleFixture.runtime,
    "/account-lifecycle/reconcile",
    {
      account_id: ACCOUNT,
      activation_enabled: true,
      operation_id: "activated-empty-retire",
      epoch: 1,
      action: "retire",
    },
  );
  assert.equal(retired.response.status, 200);
  assert.equal(retired.body.complete, true);
  assert.ok(lifecycleFixture.storage.values.has(`lifecycle-fence:${ACCOUNT}`));
  const staleLifecycle = await create(
    lifecycleFixture.runtime,
    "stale-lifecycle.example",
    { domain_limit: null },
  );
  assert.equal(staleLifecycle.response.status, 409);
  assert.equal(staleLifecycle.body.code, "account_lifecycle_suspended");
});

test("plan and lifecycle suspension keep verification indexes in exact parity", async () => {
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  });
  const created = await create(fixture.runtime, "cross-suspension.example", {
    domain_limit: 1,
  });
  const requestID = created.body.request.id;
  const hasVerificationDue = () =>
    [...fixture.storage.values.keys()].some((key) =>
      key.startsWith("verification-due:") && key.endsWith(requestID)
    );
  assert.equal(hasVerificationDue(), true);

  const downgrade = {
    account_id: ACCOUNT,
    feature_enabled: false,
    domain_limit: 0,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  await call(fixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "restrict_only",
  });
  await call(fixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "complete",
  });
  assert.equal(hasVerificationDue(), false);
  assertDerivedParity(fixture.storage);

  await call(fixture.runtime, "/account-lifecycle/reconcile", {
    account_id: ACCOUNT,
    operation_id: "cross-suspension-move",
    epoch: 1,
    action: "suspend",
  });
  const upgrade = {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 9,
    plan_snapshot_hash: "9".repeat(64),
  };
  await call(fixture.runtime, "/plan/reconcile", {
    ...upgrade,
    mode: "restrict_only",
  });
  await call(fixture.runtime, "/plan/reconcile", {
    ...upgrade,
    mode: "complete",
  });
  assert.equal(hasVerificationDue(), false);
  assertDerivedParity(fixture.storage);

  await call(fixture.runtime, "/account-lifecycle/reconcile", {
    account_id: ACCOUNT,
    operation_id: "cross-suspension-move",
    epoch: 1,
    action: "republish",
  });
  assert.equal(hasVerificationDue(), true);
  assertDerivedParity(fixture.storage);
});

test("verification defers while account policy is between durable pages", async () => {
  let expectedValue = null;
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      return {
        answers: [expectedValue],
        authoritative_absence: false,
        dnssec_authenticated: false,
        minimum_ttl_seconds: 60,
        rrset_sha256: "6".repeat(64),
      };
    },
  });
  const created = await create(fixture.runtime, "policy-race.example", {
    domain_limit: 1,
  });
  expectedValue = created.body.request.ownership_challenge.record_value;
  const plan = {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "restrict_only",
  });
  const manual = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-policy-race",
    verification_enabled: true,
  });
  assert.equal(manual.response.status, 409);
  assert.equal(manual.body.code, "account_policy_converging");

  const scheduled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(scheduled.response.status, 200);
  assert.equal(scheduled.body.checked, 1);
  assert.equal(lookups, 0);
  assert.equal(
    fixture.storage.values.get(`request:${created.body.request.id}`)
      .ownership_verification.last_result,
    "policy_converging",
  );
  const firstPolicyDeferral = structuredClone(fixture.storage.values.get(
    `request:${created.body.request.id}`,
  ));
  const firstPolicyAuditCount = auditRows(fixture.storage).length;
  const firstPolicyMeta = structuredClone(fixture.storage.values.get("meta"));
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.verification_deferred",
  );

  fixture.advanceTime(16 * 60 * 1_000);
  const repeatedDeferral = await call(
    fixture.runtime,
    "/verification/reconcile",
    { verification_enabled: true },
  );
  assert.equal(repeatedDeferral.response.status, 200);
  assert.equal(repeatedDeferral.body.checked, 1);
  assert.equal(lookups, 0);
  const refreshedPolicyDeferral = fixture.storage.values.get(
    `request:${created.body.request.id}`,
  );
  assert.equal(auditRows(fixture.storage).length, firstPolicyAuditCount);
  assert.deepEqual(fixture.storage.values.get("meta"), firstPolicyMeta);
  assert.ok(Date.parse(
    refreshedPolicyDeferral.ownership_verification.last_checked_at,
  ) > Date.parse(
    firstPolicyDeferral.ownership_verification.last_checked_at,
  ));
  assert.equal(
    refreshedPolicyDeferral.state_revision,
    firstPolicyDeferral.state_revision + 1,
  );
  assertDerivedParity(fixture.storage);

  await call(fixture.runtime, "/plan/reconcile", {
    ...plan,
    mode: "complete",
  });
  fixture.advanceTime(16 * 60 * 1_000);
  const completed = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(completed.body.matched, 1);
  assert.equal(lookups, 1);
  assert.equal(
    fixture.storage.values.get(`request:${created.body.request.id}`).state,
    "verified",
  );
  assert.equal(auditRows(fixture.storage).length, firstPolicyAuditCount + 1);
  assert.equal(
    auditRows(fixture.storage).at(-1)[1].action,
    "custom_domain.verified",
  );
  assertDerivedParity(fixture.storage);
});

test("plan downgrade fences creation and deterministically suspends overflow", async () => {
  const fixture = registry();
  const created = [];
  for (const domain of ["oldest.example", "middle.example", "newest.example"]) {
    const result = await create(fixture.runtime, domain, {
      domain_limit: null,
      idempotency_key: `create-${domain}`,
    });
    assert.equal(result.response.status, 202);
    created.push(result.body.request);
  }
  const nextHash = "8".repeat(64);
  const restricted = await call(fixture.runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    mode: "restrict_only",
    plan_revision: 8,
    plan_snapshot_hash: nextHash,
  });
  assert.equal(restricted.response.status, 200);
  const crossing = await create(fixture.runtime, "crossing.example", {
    domain_limit: null,
  });
  assert.equal(crossing.response.status, 409);
  assert.equal(crossing.body.code, "account_policy_converging");

  const completed = await call(fixture.runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    mode: "complete",
    plan_revision: 8,
    plan_snapshot_hash: nextHash,
  });
  assert.equal(completed.response.status, 200);
  assert.equal(completed.body.complete, true);
  const page = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.deepEqual(
    page.body.requests.map((request) => request.plan_suspended),
    [false, true, true],
  );
  assert.deepEqual(
    page.body.requests.map((request) => request.id),
    created.map((request) => request.id),
  );

  const upgraded = await call(fixture.runtime, "/plan/reconcile", {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: null,
    mode: "complete",
    plan_revision: 9,
    plan_snapshot_hash: "9".repeat(64),
  });
  assert.equal(upgraded.response.status, 200);
  const afterUpgrade = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(afterUpgrade.body.requests.every((request) =>
    request.plan_suspended === false), true);
});

test("capacity release durably promotes the next in-plan domain", async () => {
  const pendingFixture = registry();
  const pendingFirst = await create(
    pendingFixture.runtime,
    "pending-first.example",
    { domain_limit: null },
  );
  const pendingSecond = await create(
    pendingFixture.runtime,
    "pending-second.example",
    { domain_limit: null },
  );
  const downgrade = {
    account_id: ACCOUNT,
    feature_enabled: true,
    domain_limit: 1,
    plan_revision: 8,
    plan_snapshot_hash: "8".repeat(64),
  };
  await call(pendingFixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "restrict_only",
  });
  await call(pendingFixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "complete",
  });
  assert.equal((await call(pendingFixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: pendingSecond.body.request.id,
  })).body.request.plan_suspended, true);
  await call(pendingFixture.runtime, "/request/reject", {
    actor: ADMIN,
    request_id: pendingFirst.body.request.id,
    reason: "release the in-plan slot",
    idempotency_key: "release-pending-slot",
  });
  assert.ok(pendingFixture.storage.values.has(`plan-intent:${ACCOUNT}`));
  pendingFixture.advanceTime(2_000);
  await pendingFixture.runtime.alarm();
  assert.equal((await call(pendingFixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: pendingSecond.body.request.id,
  })).body.request.plan_suspended, false);
  assertDerivedParity(pendingFixture.storage);

  const expectedValues = new Map();
  const verifiedFixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async (recordName) => ({
      answers: [expectedValues.get(recordName)],
      authoritative_absence: false,
      dnssec_authenticated: true,
      minimum_ttl_seconds: 60,
      rrset_sha256: "c".repeat(64),
    }),
  });
  const verified = [];
  for (const domain of ["verified-first.example", "verified-second.example"]) {
    const created = await create(verifiedFixture.runtime, domain, {
      domain_limit: null,
    });
    expectedValues.set(
      created.body.request.ownership_challenge.record_name,
      created.body.request.ownership_challenge.record_value,
    );
    const result = await call(verifiedFixture.runtime, "/request/verify", {
      actor: ADMIN,
      request_id: created.body.request.id,
      idempotency_key: `verify-${domain}`,
      verification_enabled: true,
    });
    assert.equal(result.response.status, 200);
    verified.push(result.body.request);
  }
  await call(verifiedFixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "restrict_only",
  });
  await call(verifiedFixture.runtime, "/plan/reconcile", {
    ...downgrade,
    mode: "complete",
  });
  verifiedFixture.runtime.env[
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED"
  ] = "false";
  verifiedFixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  await verifiedFixture.runtime.alarm();
  assert.equal((await call(verifiedFixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: verified[1].id,
  })).body.request.plan_suspended, true);
  await call(verifiedFixture.runtime, "/request/retire", {
    actor: ADMIN,
    request_id: verified[0].id,
    reason: "release verified slot",
    idempotency_key: "release-verified-slot",
  });
  verifiedFixture.advanceTime(2_000);
  await verifiedFixture.runtime.alarm();
  const promoted = await call(verifiedFixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: verified[1].id,
  });
  assert.equal(promoted.body.request.plan_suspended, false);
  assert.equal(promoted.body.request.plan_grace_until, undefined);
  assertDerivedParity(verifiedFixture.storage);
});

test("account lifecycle suspension, republish, and close are fenced and retry-safe", async () => {
  const fixture = registry();
  const created = await create(fixture.runtime, "lifecycle.example", {
    domain_limit: null,
  });
  assert.equal(created.response.status, 202);
  const suspended = await call(fixture.runtime,
    "/account-lifecycle/reconcile", {
      account_id: ACCOUNT,
      operation_id: "move-one",
      epoch: 1,
      action: "suspend",
    });
  assert.equal(suspended.body.complete, true);
  const blocked = await create(fixture.runtime, "during-move.example", {
    domain_limit: null,
  });
  assert.equal(blocked.response.status, 409);
  assert.equal(blocked.body.code, "account_lifecycle_suspended");

  const republished = await call(fixture.runtime,
    "/account-lifecycle/reconcile", {
      account_id: ACCOUNT,
      operation_id: "move-one",
      epoch: 1,
      action: "republish",
    });
  assert.equal(republished.body.complete, true);
  const active = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(active.body.request.lifecycle_suspended, false);

  const retired = await call(fixture.runtime,
    "/account-lifecycle/reconcile", {
      account_id: ACCOUNT,
      operation_id: "close-two",
      epoch: 2,
      action: "retire",
    });
  assert.equal(retired.body.complete, true);
  const replay = await call(fixture.runtime,
    "/account-lifecycle/reconcile", {
      account_id: ACCOUNT,
      operation_id: "close-two",
      epoch: 2,
      action: "retire",
    });
  assert.equal(replay.body.replayed, true);
  const closed = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(closed.body.request.state, "retired");
});

test("verified lifecycle retirement completes across the 40-item page boundary", async () => {
  const expectedValues = new Map();
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async (recordName) => ({
      answers: [expectedValues.get(recordName)],
      authoritative_absence: false,
      dnssec_authenticated: true,
      minimum_ttl_seconds: 300,
      rrset_sha256: "1".repeat(64),
    }),
  });
  const created = [];
  for (let index = 0; index < 41; index++) {
    const pending = await create(
      fixture.runtime,
      `lifecycle-page-${String(index).padStart(2, "0")}.example`,
      {
        domain_limit: null,
        idempotency_key: `create-lifecycle-page-${index}`,
      },
    );
    assert.equal(pending.response.status, 202, `create ${index}`);
    expectedValues.set(
      pending.body.request.ownership_challenge.record_name,
      pending.body.request.ownership_challenge.record_value,
    );
    const verified = await call(fixture.runtime, "/request/verify", {
      actor: ADMIN,
      request_id: pending.body.request.id,
      idempotency_key: `verify-lifecycle-page-${index}`,
      verification_enabled: true,
    });
    assert.equal(verified.response.status, 200, `verify ${index}`);
    created.push(verified.body.request);
  }

  const retirement = {
    account_id: ACCOUNT,
    operation_id: "retire-paginated-verified",
    epoch: 1,
    action: "retire",
  };
  const firstPage = await call(
    fixture.runtime,
    "/account-lifecycle/reconcile",
    retirement,
  );
  assert.equal(firstPage.response.status, 200);
  assert.equal(firstPage.body.complete, false);
  assert.equal(firstPage.body.changed, 40);
  const betweenPages = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    limit: 100,
  });
  assert.equal(betweenPages.body.allocated_domains, 1);
  assert.equal(
    betweenPages.body.requests.filter((request) =>
      request.state === "retired").length,
    40,
  );

  const secondPage = await call(
    fixture.runtime,
    "/account-lifecycle/reconcile",
    retirement,
  );
  assert.equal(secondPage.response.status, 200);
  assert.equal(secondPage.body.complete, true);
  assert.equal(secondPage.body.changed, 1);
  const completed = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
    limit: 100,
  });
  assert.equal(completed.body.open_requests, 0);
  assert.equal(completed.body.allocated_domains, 0);
  assert.equal(completed.body.requests.length, 41);
  assert.equal(completed.body.requests.every((request) =>
    request.state === "retired"), true);
  for (const request of created) {
    assert.equal(
      fixture.storage.values.get(`domain:${request.domain}`).state,
      "retired",
    );
  }
  const recoveredAuthority = new Map(
    [...fixture.storage.values].filter(([key]) =>
      isAgentEmailDomainAuthorityKey(key)
    ),
  );
  assert.doesNotThrow(() =>
    validateAgentEmailDomainRecoveredState(recoveredAuthority)
  );
  assertDerivedParity(fixture.storage);
  const replay = await call(
    fixture.runtime,
    "/account-lifecycle/reconcile",
    retirement,
  );
  assert.equal(replay.body.replayed, true);
});

test("unverified ownership challenges expire and release capacity", async () => {
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  });
  const created = await create(fixture.runtime, "expires.example", {
    domain_limit: 1,
  });
  assert.equal(created.response.status, 202);
  const blocked = await create(fixture.runtime, "blocked-by-pending.example", {
    domain_limit: 1,
  });
  assert.equal(blocked.response.status, 403);
  assert.equal(blocked.body.code, "account_limit_reached");
  const beforeExpiry = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(beforeExpiry.body.open_requests, 1);
  assert.equal(beforeExpiry.body.allocated_domains, 1);
  const claimed = await call(fixture.runtime, "/verification/claim", {
    mode: "manual",
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "expiry-orphan-claim",
    verification_enabled: true,
  });
  assert.equal(claimed.body.kind, "claim");
  const workKey = `verification-work:${created.body.request.id}`;
  assert.equal(fixture.storage.values.has(workKey), true);

  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  await fixture.runtime.alarm();
  const expired = await call(fixture.runtime, "/request/get", {
    actor: ADMIN,
    request_id: created.body.request.id,
  });
  assert.equal(expired.body.request.state, "expired");
  assert.equal(expired.body.request.availability, "expired");
  assert.equal(fixture.storage.values.has(workKey), false);
  const afterExpiry = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(afterExpiry.body.open_requests, 0);
  assert.equal(afterExpiry.body.allocated_domains, 0);
  const replacement = await create(fixture.runtime, "expires.example", {
    domain_limit: 1,
    idempotency_key: "replacement-expired",
  });
  assert.equal(replacement.response.status, 202);
  assert.notEqual(replacement.body.request.id, created.body.request.id);
  assert.notEqual(
    replacement.body.request.ownership_challenge.record_value,
    created.body.request.ownership_challenge.record_value,
  );
});

test("an overdue verification attempt releases capacity without DNS", async () => {
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      throw new Error("an expired challenge must not resolve DNS");
    },
  });
  const created = await create(fixture.runtime, "late-expiry.example", {
    domain_limit: 1,
  });
  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  const expired = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-after-expiry",
    verification_enabled: true,
  });
  assert.equal(expired.response.status, 409);
  assert.equal(expired.body.code, "ownership_challenge_expired");
  assert.equal(lookups, 0);
  const afterExpired = structuredClone([...fixture.storage.values.entries()]);
  const replay = await call(fixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: created.body.request.id,
    idempotency_key: "verify-after-expiry",
    verification_enabled: true,
  });
  assert.equal(replay.response.status, 409);
  assert.deepEqual(replay.body, expired.body);
  assert.deepEqual([...fixture.storage.values.entries()], afterExpired);
  assert.doesNotThrow(() => validateAgentEmailDomainRecoveredState(new Map(
    [...fixture.storage.values].filter(([key]) =>
      isAgentEmailDomainAuthorityKey(key)),
  )));

  const usage = await call(fixture.runtime, "/request/list", {
    actor: OPERATOR,
    account_id: ACCOUNT,
  });
  assert.equal(usage.body.open_requests, 0);
  assert.equal(usage.body.allocated_domains, 0);
  const replacement = await create(fixture.runtime, "replacement-after-gap.example", {
    domain_limit: 1,
  });
  assert.equal(replacement.response.status, 202);
});

test("scheduled verification cannot accept proof after challenge expiry", async () => {
  let lookups = 0;
  const fixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      lookups += 1;
      throw new Error("expired scheduled challenge must not resolve DNS");
    },
  });
  const created = await create(fixture.runtime, "scheduled-expiry.example", {
    domain_limit: 1,
  });
  fixture.advanceTime(
    (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
      24 * 60 * 60 * 1_000,
  );
  const reconciled = await call(fixture.runtime, "/verification/reconcile", {
    verification_enabled: true,
  });
  assert.equal(reconciled.response.status, 200);
  assert.equal(reconciled.body.checked, 0);
  assert.equal(lookups, 0);
  assert.equal(
    fixture.storage.values.get(`request:${created.body.request.id}`).state,
    "expired",
  );
  assert.equal(
    fixture.storage.values.get("account-usage:acct_domain").allocated_domains,
    0,
  );
});

test("proof cannot win while DNS resolution crosses challenge expiry", async () => {
  let manualFixture;
  let manualValue = null;
  manualFixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      manualFixture.advanceTime(
        (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
          24 * 60 * 60 * 1_000,
      );
      return {
        answers: [manualValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "a".repeat(64),
      };
    },
  });
  const manualCreated = await create(
    manualFixture.runtime,
    "manual-cross-expiry.example",
    { domain_limit: 1 },
  );
  manualValue = manualCreated.body.request.ownership_challenge.record_value;
  const manual = await call(manualFixture.runtime, "/request/verify", {
    actor: ADMIN,
    request_id: manualCreated.body.request.id,
    idempotency_key: "manual-cross-expiry",
    verification_enabled: true,
  });
  assert.equal(manual.response.status, 409);
  assert.equal(manual.body.code, "ownership_challenge_expired");
  assert.equal(manualFixture.storage.values.has(
    "domain:manual-cross-expiry.example"), false);

  let scheduledFixture;
  let scheduledValue = null;
  scheduledFixture = registry({
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED: "true",
  }, {
    resolveTXT: async () => {
      scheduledFixture.advanceTime(
        (AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS + 1) *
          24 * 60 * 60 * 1_000,
      );
      return {
        answers: [scheduledValue],
        authoritative_absence: false,
        dnssec_authenticated: true,
        minimum_ttl_seconds: 60,
        rrset_sha256: "b".repeat(64),
      };
    },
  });
  const scheduledCreated = await create(
    scheduledFixture.runtime,
    "scheduled-cross-expiry.example",
    { domain_limit: 1 },
  );
  scheduledValue =
    scheduledCreated.body.request.ownership_challenge.record_value;
  const reconciled = await call(
    scheduledFixture.runtime,
    "/verification/reconcile",
    { verification_enabled: true },
  );
  assert.equal(reconciled.response.status, 200);
  assert.equal(reconciled.body.matched, 0);
  assert.equal(
    scheduledFixture.storage.values.get(
      `request:${scheduledCreated.body.request.id}`,
    ).state,
    "expired",
  );
  assert.equal(scheduledFixture.storage.values.has(
    "domain:scheduled-cross-expiry.example"), false);
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
