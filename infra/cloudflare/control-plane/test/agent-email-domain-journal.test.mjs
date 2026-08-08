import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
  AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION,
  AgentEmailDomainJournalError,
  agentEmailDomainAuthorityStateDigest,
  agentEmailDomainJournalEntryKey,
  appendAgentEmailDomainJournalEntry,
  applyAgentEmailDomainAuthorityAfterImage,
  buildAgentEmailDomainBootstrapPage,
  buildAgentEmailDomainJournalEntry,
  canonicalJSONString,
  classifyAgentEmailDomainStorageKey,
  rebuildAgentEmailDomainDerivedState,
  replayAgentEmailDomainJournalPage,
  sha256Hex,
  validateAgentEmailDomainAuthorityAfterImage,
  validateAgentEmailDomainJournalEntry,
  validateAgentEmailDomainRecoveredState,
} from "../src/agent-email-domain-journal.mjs";

const streamID = `aedj_${"a".repeat(24)}`;
const requestID = `aedr_${"b".repeat(16)}`;
const domain = "agents.example.com";
const requestedAt = "2026-08-03T12:00:00.000Z";
const rejectedAt = "2026-08-03T13:00:00.000Z";
const retiredAt = "2026-08-03T14:00:00.000Z";

function pendingRequest() {
  return {
    schema_version: "witself.agent-email-domain.v1",
    id: requestID,
    account_id: "acc_1",
    domain,
    state: "pending_verification",
    ownership_challenge: {
      record_type: "TXT",
      record_name: `_witself-verification.${domain}`,
      record_value: `witself-domain-verification=aedv_${"c".repeat(32)}`,
      issued_at: requestedAt,
    },
    requested_by: "operator_1",
    requested_at: requestedAt,
    updated_at: requestedAt,
    domain_limit_at_request: 1,
    plan_revision: 7,
    plan_snapshot_hash: "d".repeat(64),
    decision: null,
    retirement: null,
  };
}

function publicRequest(request) {
  const { schema_version: _schema, ...result } = structuredClone(request);
  return result;
}

function requestedAudit() {
  return {
    sequence: 1,
    registry_revision: 1,
    occurred_at: requestedAt,
    actor_kind: "account_operator",
    actor_id: "operator_1",
    action: "custom_domain.requested",
    target: domain,
    metadata: {
      account_id: "acc_1", request_id: requestID,
      state: "pending_verification",
    },
  };
}

function receipt(action, request, reason = "policy") {
  const scope = action === "requested"
    ? `request-create:acc_1:create-1`
    : `request-${action}:${requestID}:${action}-1`;
  const operation = action === "requested" ? "create" : action;
  const fp = action === "requested"
    ? JSON.stringify(["request.create", "acc_1", domain])
    : JSON.stringify([`request.${action}`, requestID, reason]);
  return [`idem:${scope}`, {
    fingerprint: fp,
    status: action === "requested" ? 202 : 200,
    body: {
      schema_version: "witself.agent-email-domain.v1",
      request: publicRequest(request),
    },
    operation,
  }];
}

function pendingState() {
  const request = pendingRequest();
  const create = receipt("requested", request);
  // Runtime receipts have exactly fingerprint/status/body. Keep the helper's
  // operation label out of canonical storage.
  delete create[1].operation;
  return new Map([
    ["meta", {
      schema_version: "witself.agent-email-domain.v1",
      registry_revision: 1,
      audit_sequence: 1,
      created_at: requestedAt,
      updated_at: requestedAt,
    }],
    ["audit:000000000001", requestedAudit()],
    [`request:${requestID}`, request],
    [`domain:${domain}`, request],
    create,
  ]);
}

function verifiedState() {
  const pending = {
    ...pendingRequest(),
    state_revision: 1,
    plan_suspended: false,
    plan_grace_until: null,
    lifecycle_suspended: false,
    lifecycle_fence: null,
    ownership_verification: null,
    expiration: null,
  };
  const verifiedAt = rejectedAt;
  const verified = {
    ...pending,
    state: "verified",
    state_revision: 2,
    updated_at: verifiedAt,
    ownership_verification: {
      state: "verified",
      last_result: "present",
      first_verified_at: verifiedAt,
      last_checked_at: verifiedAt,
      last_verified_at: verifiedAt,
      next_check_at: retiredAt,
      rrset_sha256: "e".repeat(64),
      dnssec_authenticated: true,
      minimum_ttl_seconds: 60,
      consecutive_failures: 0,
    },
  };
  const create = receipt("requested", pending);
  delete create[1].operation;
  return new Map([
    ["meta", {
      schema_version: "witself.agent-email-domain.v1",
      registry_revision: 2,
      audit_sequence: 2,
      created_at: requestedAt,
      updated_at: verifiedAt,
    }],
    ["audit:000000000001", requestedAudit()],
    ["audit:000000000002", {
      sequence: 2,
      registry_revision: 2,
      occurred_at: verifiedAt,
      actor_kind: "platform_admin",
      actor_id: "admin_1",
      action: "custom_domain.verified",
      target: domain,
      metadata: {
        account_id: "acc_1",
        request_id: requestID,
        state: "verified",
        rrset_sha256: "e".repeat(64),
      },
    }],
    [`request:${requestID}`, verified],
    [`domain:${domain}`, {
      schema_version: "witself.agent-email-domain-allocation.v1",
      domain,
      account_id: "acc_1",
      source_request_id: requestID,
      generation: 1,
      allocation_revision: 1,
      state: "allocated",
      allocated_at: verifiedAt,
      updated_at: verifiedAt,
      ownership_proof: {
        verified_at: verifiedAt,
        rrset_sha256: "e".repeat(64),
        dnssec_authenticated: true,
      },
      retirement: null,
    }],
    create,
    [`idem:request-verify:${requestID}:verify-1`, {
      fingerprint: JSON.stringify(["request.verify", requestID]),
      status: 200,
      body: {
        schema_version: "witself.agent-email-domain.v1",
        request: publicRequest(verified),
        matched: true,
      },
    }],
  ]);
}

function rejectedState() {
  const request = {
    ...pendingRequest(),
    state: "rejected",
    updated_at: rejectedAt,
    decision: {
      action: "rejected",
      reason: "policy",
      decided_by: "admin_1",
      decided_at: rejectedAt,
    },
  };
  const create = receipt("requested", pendingRequest());
  const rejected = receipt("rejected", request);
  delete create[1].operation;
  delete rejected[1].operation;
  // Intentionally insert audit 2 before audit 1 to prove validation orders by
  // sequence rather than depending on Map insertion order.
  return new Map([
    [`audit:000000000002`, {
      sequence: 2,
      registry_revision: 2,
      occurred_at: rejectedAt,
      actor_kind: "platform_admin",
      actor_id: "admin_1",
      action: "custom_domain.rejected",
      target: domain,
      metadata: {
        account_id: "acc_1",
        request_id: requestID,
        from_state: "pending_verification",
        reason: "policy",
      },
    }],
    ["meta", {
      schema_version: "witself.agent-email-domain.v1",
      registry_revision: 2,
      audit_sequence: 2,
      created_at: requestedAt,
      updated_at: rejectedAt,
    }],
    ["audit:000000000001", requestedAudit()],
    [`request:${requestID}`, request],
    [`domain:${domain}`, request],
    create,
    rejected,
  ]);
}

function retiredState() {
  const rejected = rejectedState();
  const prior = rejected.get(`request:${requestID}`);
  const request = {
    ...prior,
    state: "retired",
    updated_at: retiredAt,
    retirement: {
      reason: "closed",
      retired_by: "admin_1",
      retired_at: retiredAt,
    },
  };
  rejected.set("meta", {
    ...rejected.get("meta"),
    registry_revision: 3,
    audit_sequence: 3,
    updated_at: retiredAt,
  });
  rejected.set("audit:000000000003", {
    sequence: 3,
    registry_revision: 3,
    occurred_at: retiredAt,
    actor_kind: "platform_admin",
    actor_id: "admin_1",
    action: "custom_domain.retired",
    target: domain,
    metadata: {
      account_id: "acc_1",
      request_id: requestID,
      from_state: "rejected",
      reason: "closed",
    },
  });
  rejected.set(`request:${requestID}`, request);
  rejected.set(`domain:${domain}`, request);
  const retired = receipt("retired", request, "closed");
  delete retired[1].operation;
  rejected.set(...retired);
  return rejected;
}

function entryInput(afterImage, overrides = {}) {
  return {
    stream_id: streamID,
    kind: "mutation",
    authority_epoch: 1,
    sequence: 1,
    previous_hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
    registry_revision: 1,
    audit_sequence: 1,
    occurred_at: requestedAt,
    operation_id: "journal:1",
    operation_fingerprint: "e".repeat(64),
    actor: { kind: "account_operator", id: "operator_1" },
    action: "custom_domain.requested",
    target: domain,
    metadata: { storage_puts: afterImage.puts.length, storage_deletes: 0 },
    after_image: afterImage,
    ...overrides,
  };
}

test("restricted canonical JSON is stable and refuses ambiguous values", () => {
  assert.equal(canonicalJSONString({ z: 1, a: [true, null] }),
    '{"a":[true,null],"z":1}');
  for (const value of [1.5, -0, Number.MAX_SAFE_INTEGER + 1, undefined]) {
    assert.throws(() => canonicalJSONString(value), AgentEmailDomainJournalError);
  }
  const cyclic = {};
  cyclic.self = cyclic;
  assert.throws(() => canonicalJSONString(cyclic), /cycles/);
});

test("storage classification keeps both uniqueness rows canonical", () => {
  for (const key of [
    "meta", "audit:000000000001", `request:${requestID}`,
    `domain:${domain}`, "idem:request-create:acc_1:create-1",
    "plan-fence:acc_1", "plan-intent:acc_1",
    "lifecycle-fence:acc_1", "lifecycle-intent:acc_1",
  ]) {
    assert.equal(classifyAgentEmailDomainStorageKey(key), "authority", key);
  }
  for (const key of [
    `account-request:acc_1:${requestID}`, "account-usage:acc_1",
    `account-domain:acc_1:${requestedAt}:${requestID}`,
    `domain-pending:${domain}:${requestedAt}:${requestID}`,
    `challenge-expiry-due:0000000000000001:${requestID}`,
    "plan-due:0000000000000001:acc_1",
    "lifecycle-due:0000000000000001:acc_1",
    `plan-grace-due:0000000000000001:${requestID}`,
    `verification-due:0000000000000001:${requestID}`,
  ]) {
    assert.equal(classifyAgentEmailDomainStorageKey(key), "derived", key);
  }
  for (const key of [
    "agent-email-domain-journal-meta", "agent-email-domain-journal:bootstrap",
    "agent-email-domain-recovery", "agent-email-domain-recovery:aedrec_test",
  ]) {
    assert.equal(classifyAgentEmailDomainStorageKey(key), "journal_local", key);
  }
  assert.equal(classifyAgentEmailDomainStorageKey("future-key"), "unknown");
});

test("after-images are bounded and delete only transient authority intents", () => {
  const normalized = validateAgentEmailDomainAuthorityAfterImage({
    puts: [
      { key: `request:${requestID}`, value: pendingRequest() },
      { key: "meta", value: pendingState().get("meta") },
    ],
    deletes: [],
  });
  assert.deepEqual(normalized.puts.map(({ key }) => key), [
    "meta", `request:${requestID}`,
  ]);
  assert.throws(() => validateAgentEmailDomainAuthorityAfterImage({
    puts: [{ key: "account-usage:acc_1", value: {} }], deletes: [],
  }), /not canonical authority/);
  assert.throws(() => validateAgentEmailDomainAuthorityAfterImage({
    puts: [], deletes: [`domain:${domain}`],
  }), /cannot delete canonical authority/);
  assert.deepEqual(validateAgentEmailDomainAuthorityAfterImage({
    puts: [], deletes: ["plan-intent:acc_1", "lifecycle-intent:acc_1"],
  }).deletes, ["lifecycle-intent:acc_1", "plan-intent:acc_1"]);
  assert.throws(() => validateAgentEmailDomainAuthorityAfterImage({
    puts: Array.from({ length: 101 }, (_, index) => ({
      key: `audit:${String(index + 1).padStart(12, "0")}`,
      value: { index },
    })),
    deletes: [],
  }), /bounded change limit/);
});

test("entry construction, keys, hashing, replay, and tamper fences are exact", async () => {
  const state = pendingState();
  const afterImage = {
    puts: [...state].map(([key, value]) => ({ key, value })),
    deletes: [],
  };
  const built = await buildAgentEmailDomainJournalEntry(entryInput(afterImage));
  assert.equal(built.entry.schema_version,
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION);
  assert.equal(built.key, agentEmailDomainJournalEntryKey(streamID, 1));
  assert.equal(built.hash, await sha256Hex(canonicalJSONString(
    Object.fromEntries(Object.entries(built.entry)
      .filter(([key]) => key !== "entry_hash")),
  )));
  const verified = await validateAgentEmailDomainJournalEntry(built.entry, {
    stream_id: streamID,
    sequence: 1,
    previous_hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
  });
  assert.deepEqual(verified.bytes, built.bytes);
  const replayed = await replayAgentEmailDomainJournalPage([built.entry], {
    stream_id: streamID,
  });
  assert.equal(replayed.head.hash, built.hash);
  assert.equal(replayed.state.size, state.size);

  const changed = structuredClone(built.entry);
  changed.target = "changed.example.com";
  await assert.rejects(
    validateAgentEmailDomainJournalEntry(changed),
    (error) => error.code === "agent_email_domain_journal_hash_mismatch",
  );
  await assert.rejects(
    replayAgentEmailDomainJournalPage([built.entry], {
      stream_id: streamID,
      head: { sequence: 0, hash: "f".repeat(64), authority_epoch: null },
    }),
    (error) => error.code === "agent_email_domain_journal_fence_mismatch",
  );

  await assert.rejects(
    buildAgentEmailDomainJournalEntry(entryInput(afterImage, {
      kind: "takeover",
    })),
    /kind is invalid/,
  );
  const changedEpoch = await buildAgentEmailDomainJournalEntry(entryInput(
    { puts: [], deletes: [] },
    {
      kind: "checkpoint",
      sequence: 2,
      previous_hash: built.hash,
      authority_epoch: 2,
      operation_id: "checkpoint:2",
    },
  ));
  await assert.rejects(
    replayAgentEmailDomainJournalPage([built.entry, changedEpoch.entry], {
      stream_id: streamID,
    }),
    (error) => error.code === "agent_email_domain_journal_fence_mismatch",
  );
});

test("R2 append is create-only, exact retry replays, and changed bytes fork", async () => {
  const afterImage = {
    puts: [{ key: "meta", value: pendingState().get("meta") }],
    deletes: [],
  };
  const built = await buildAgentEmailDomainJournalEntry(entryInput(afterImage));
  const objects = new Map();
  const bucket = {
    async put(key, bytes) {
      if (objects.has(key)) return null;
      objects.set(key, new Uint8Array(bytes));
      return { key };
    },
    async get(key) {
      const bytes = objects.get(key);
      return bytes ? { arrayBuffer: async () => bytes.buffer.slice(0) } : null;
    },
  };
  assert.deepEqual(await appendAgentEmailDomainJournalEntry(bucket, built), {
    created: true, replayed: false, object: { key: built.key },
  });
  const replay = await appendAgentEmailDomainJournalEntry(bucket, built);
  assert.equal(replay.created, false);
  assert.equal(replay.replayed, true);
  objects.set(built.key, new TextEncoder().encode("different"));
  await assert.rejects(
    appendAgentEmailDomainJournalEntry(bucket, built),
    (error) => error.code === "agent_email_domain_journal_fork_detected",
  );
});

test("bootstrap pages and authority digest are insertion-order independent", async () => {
  const state = pendingState();
  const page1 = buildAgentEmailDomainBootstrapPage(state, { limit: 2 });
  assert.equal(page1.count, 2);
  assert.equal(page1.complete, false);
  const page2 = buildAgentEmailDomainBootstrapPage(state, {
    limit: 100,
    cursor: page1.next_cursor,
  });
  assert.equal(page2.complete, true);
  assert.equal(page1.count + page2.count, state.size);
  assert.equal(
    await agentEmailDomainAuthorityStateDigest(state),
    await agentEmailDomainAuthorityStateDigest(new Map([...state].reverse())),
  );
});

test("recovered authority verifies mirrored tombstones, ordered audit, and idem graph", () => {
  assert.deepEqual(validateAgentEmailDomainRecoveredState(pendingState()), {
    registry_revision: 1,
    audit_sequence: 1,
    requests: 1,
    domains: 1,
    authority_keys: 5,
  });
  assert.equal(validateAgentEmailDomainRecoveredState(rejectedState()).requests, 1);
  assert.equal(validateAgentEmailDomainRecoveredState(retiredState()).domains, 1);

  const mismatched = pendingState();
  mismatched.set(`domain:${domain}`, {
    ...pendingRequest(), account_id: "acc_other",
  });
  assert.throws(
    () => validateAgentEmailDomainRecoveredState(mismatched),
    (error) => error.code === "agent_email_domain_recovery_collision",
  );

  const gap = rejectedState();
  gap.delete("audit:000000000001");
  assert.throws(
    () => validateAgentEmailDomainRecoveredState(gap),
    (error) => error.code === "agent_email_domain_recovery_revision_regression",
  );

  const wrongReceipt = pendingState();
  const idemKey = "idem:request-create:acc_1:create-1";
  wrongReceipt.get(idemKey).fingerprint = "[]";
  assert.throws(() => validateAgentEmailDomainRecoveredState(wrongReceipt),
    /idempotency receipt is inconsistent/);

  for (const mutate of [
    (audit) => { audit.occurred_at = "2099-01-01T00:00:00.000Z"; },
    (audit) => {
      audit.actor_kind = "platform_admin";
      audit.actor_id = "wrong_actor";
    },
    (audit) => { audit.metadata.state = "retired"; },
  ]) {
    const falseAudit = new Map([...pendingState()].map(([key, value]) =>
      [key, structuredClone(value)]));
    mutate(falseAudit.get("audit:000000000001"));
    assert.throws(
      () => validateAgentEmailDomainRecoveredState(falseAudit),
      /audit (?:evidence|time) is inconsistent|audit time is invalid/,
    );
  }

  const ghost = new Map([
    ["meta", pendingState().get("meta")],
    ["audit:000000000001", {
      ...requestedAudit(),
      target: "ghost.example",
      metadata: { account_id: "acc_1", state: "pending_verification" },
    }],
  ]);
  assert.throws(() => validateAgentEmailDomainRecoveredState(ghost),
    /audit graph is inconsistent/);

  const wrongOrigin = new Map([...retiredState()].map(([key, value]) =>
    [key, structuredClone(value)]));
  wrongOrigin.get("audit:000000000003").metadata.from_state =
    "pending_verification";
  assert.throws(() => validateAgentEmailDomainRecoveredState(wrongOrigin),
    /audit evidence is inconsistent/);

  const missingRetireReceipt = retiredState();
  missingRetireReceipt.delete(`idem:request-retired:${requestID}:retired-1`);
  assert.throws(
    () => validateAgentEmailDomainRecoveredState(missingRetireReceipt),
    /idempotency graph is inconsistent/,
  );

  const earlyExpiry = pendingState();
  const expiredRequest = {
    ...pendingRequest(),
    state: "expired",
    updated_at: rejectedAt,
    expiration: {
      expired_at: rejectedAt,
      reason: "ownership challenge expired",
    },
  };
  earlyExpiry.set("meta", {
    ...earlyExpiry.get("meta"),
    registry_revision: 2,
    audit_sequence: 2,
    updated_at: rejectedAt,
  });
  earlyExpiry.set("audit:000000000002", {
    sequence: 2,
    registry_revision: 2,
    occurred_at: rejectedAt,
    actor_kind: "system",
    actor_id: "challenge-expiry",
    action: "custom_domain.expired",
    target: domain,
    metadata: { account_id: "acc_1", request_id: requestID },
  });
  earlyExpiry.set(`request:${requestID}`, expiredRequest);
  earlyExpiry.set(`domain:${domain}`, expiredRequest);
  assert.throws(() => validateAgentEmailDomainRecoveredState(earlyExpiry),
    /expiration preceded its challenge/);

  const impossiblePending = pendingState();
  const impossible = impossiblePending.get(`request:${requestID}`);
  impossible.ownership_verification = {
    state: "verified",
    last_result: "present",
    first_verified_at: requestedAt,
    last_checked_at: requestedAt,
    last_verified_at: requestedAt,
    next_check_at: rejectedAt,
    rrset_sha256: "e".repeat(64),
    dnssec_authenticated: true,
    minimum_ttl_seconds: 60,
    consecutive_failures: 0,
  };
  impossiblePending.set(`domain:${domain}`, structuredClone(impossible));
  assert.throws(() => validateAgentEmailDomainRecoveredState(impossiblePending),
    /verification history is invalid/);

  const missingLifecycleFence = pendingState();
  const lifecycleRequest = missingLifecycleFence.get(`request:${requestID}`);
  lifecycleRequest.lifecycle_suspended = true;
  lifecycleRequest.lifecycle_fence = null;
  missingLifecycleFence.set(
    `domain:${domain}`,
    structuredClone(lifecycleRequest),
  );
  assert.throws(
    () => validateAgentEmailDomainRecoveredState(missingLifecycleFence),
    /lifecycle fence is missing/,
  );
});

test("recovery validates verified allocations separately from requests", () => {
  const state = verifiedState();
  const validated = validateAgentEmailDomainRecoveredState(state);
  assert.equal(validated.requests, 1);
  assert.equal(validated.domains, 1);
  const derived = rebuildAgentEmailDomainDerivedState(state);
  assert.deepEqual(derived.get("account-usage:acc_1"), {
    schema_version: 1,
    account_id: "acc_1",
    open_requests: 0,
    allocated_domains: 1,
    updated_at: rejectedAt,
  });
  assert.equal(
    derived.get(`verification-due:${String(Date.parse(retiredAt)).padStart(
      16,
      "0",
    )}:${requestID}`),
    requestID,
  );

  for (const mutate of [
    (allocation) => { allocation.allocated_at = requestedAt; },
    (allocation) => { allocation.ownership_proof.rrset_sha256 = "f".repeat(64); },
    (allocation) => { allocation.ownership_proof.verified_at = retiredAt; },
  ]) {
    const mismatched = new Map([...verifiedState()].map(([key, value]) =>
      [key, structuredClone(value)]));
    mutate(mismatched.get(`domain:${domain}`));
    assert.throws(
      () => validateAgentEmailDomainRecoveredState(mismatched),
      /request and allocation graph is inconsistent/,
    );
  }
});

test("after-image replay allows only real lifecycle edges and contiguous revisions", () => {
  const requestKey = `request:${requestID}`;
  const pending = pendingRequest();
  const state = new Map([
    ["meta", pendingState().get("meta")],
    [requestKey, pending],
  ]);
  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(state, {
    puts: [{ key: requestKey, value: { ...pending, updated_at: rejectedAt } }],
    deletes: [],
  }), /same-state/);

  const rejected = rejectedState().get(requestKey);
  const advanced = applyAgentEmailDomainAuthorityAfterImage(state, {
    puts: [{ key: requestKey, value: rejected }],
    deletes: [],
  });
  assert.equal(advanced.get(requestKey).state, "rejected");

  const legacyState = new Map([
    [requestKey, pending],
    [`domain:${domain}`, pending],
  ]);
  const legacyAdvanced = applyAgentEmailDomainAuthorityAfterImage(legacyState, {
    puts: [
      { key: requestKey, value: rejected },
      { key: `domain:${domain}`, value: rejected },
    ],
    deletes: [],
  });
  assert.deepEqual(
    legacyAdvanced.get(`domain:${domain}`),
    legacyAdvanced.get(requestKey),
  );
  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(advanced, {
    puts: [{ key: requestKey, value: pending }], deletes: [],
  }), (error) =>
    error.code === "agent_email_domain_recovery_tombstone_resurrection");

  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(state, {
    puts: [{ key: "meta", value: {
      ...pendingState().get("meta"), registry_revision: 3, audit_sequence: 3,
    } }],
    deletes: [],
  }), (error) => error.code ===
    "agent_email_domain_recovery_revision_regression");
});

test("plan intent replay rejects revision and progress regression", () => {
  const key = "plan-intent:acc_1";
  const awaiting = {
    account_id: "acc_1",
    plan_revision: 7,
    plan_snapshot_hash: "d".repeat(64),
    feature_enabled: true,
    domain_limit: 1,
    state: "awaiting_cell",
    cursor: null,
    position: 0,
    failure_count: 0,
    retry_at_ms: null,
    created_at: requestedAt,
    updated_at: requestedAt,
  };
  const committed = {
    ...awaiting,
    state: "cell_committed",
    retry_at_ms: Date.parse(rejectedAt),
    updated_at: rejectedAt,
  };
  const advanced = applyAgentEmailDomainAuthorityAfterImage(
    new Map([[key, awaiting]]),
    { puts: [{ key, value: committed }], deletes: [] },
  );
  assert.equal(advanced.get(key).state, "cell_committed");

  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(advanced, {
    puts: [{ key, value: {
      ...committed,
      plan_revision: 6,
      plan_snapshot_hash: "e".repeat(64),
      updated_at: retiredAt,
    } }],
    deletes: [],
  }), (error) => error.code ===
    "agent_email_domain_recovery_revision_regression");

  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(
    new Map([[key, awaiting]]),
    { puts: [{ key, value: {
      ...committed,
      created_at: rejectedAt,
    } }], deletes: [] },
  ), /plan intent identity changed/);

  assert.throws(() => applyAgentEmailDomainAuthorityAfterImage(
    new Map([[key, committed]]),
    { puts: [], deletes: [key] },
  ), /intent deletion lost convergence/);
  const completed = applyAgentEmailDomainAuthorityAfterImage(
    new Map([[key, committed]]),
    {
      puts: [{
        key: "plan-fence:acc_1",
        value: {
          account_id: "acc_1",
          committed_revision: 7,
          committed_snapshot_hash: "d".repeat(64),
          feature_enabled: true,
          domain_limit: 1,
          updated_at: retiredAt,
        },
      }],
      deletes: [key],
    },
  );
  assert.equal(completed.has(key), false);
  assert.ok(completed.has("plan-fence:acc_1"));
});

test("account policy audits bind the system actor and lifecycle fence", () => {
  const planAudit = {
    sequence: 2,
    registry_revision: 2,
    occurred_at: rejectedAt,
    actor_kind: "system",
    actor_id: "plan-lifecycle",
    action: "custom_domain.plan_reconciled",
    target: "acc_1",
    metadata: {
      account_id: "acc_1",
      changed: 1,
      page_size: 1,
      plan_revision: 7,
      plan_snapshot_hash: "d".repeat(64),
      domain_limit: 1,
      downgrade_grace_days: 30,
    },
  };
  const state = pendingState();
  state.set("meta", {
    ...state.get("meta"),
    registry_revision: 2,
    audit_sequence: 2,
    updated_at: rejectedAt,
  });
  state.set("audit:000000000002", planAudit);
  assert.equal(validateAgentEmailDomainRecoveredState(state).requests, 1);

  const forgedPlan = new Map([...state].map(([key, value]) =>
    [key, structuredClone(value)]));
  forgedPlan.get("audit:000000000002").actor_kind = "platform_admin";
  forgedPlan.get("audit:000000000002").actor_id = "wrong_actor";
  assert.throws(() => validateAgentEmailDomainRecoveredState(forgedPlan),
    /plan audit is invalid/);

  const lifecycle = new Map([...state].map(([key, value]) =>
    [key, structuredClone(value)]));
  lifecycle.set("audit:000000000002", {
    ...planAudit,
    actor_id: "account-lifecycle",
    action: "custom_domain.lifecycle_suspend",
    metadata: {
      account_id: "acc_1",
      operation_id: "close-one",
      epoch: 1,
      changed: 1,
      page_size: 1,
    },
  });
  assert.equal(validateAgentEmailDomainRecoveredState(lifecycle).requests, 1);
  lifecycle.get("audit:000000000002").metadata.operation_id = "bad operation";
  assert.throws(() => validateAgentEmailDomainRecoveredState(lifecycle),
    /lifecycle audit is invalid/);
});

test("derived rebuild recreates chronological indexes and split usage", () => {
  const pending = rebuildAgentEmailDomainDerivedState(pendingState());
  assert.deepEqual([...pending.keys()].sort(), [
    `account-domain:acc_1:${requestedAt}:${requestID}`,
    `account-request:acc_1:${requestID}`,
    "account-usage:acc_1",
    `challenge-expiry-due:${String(
      Date.parse(requestedAt) + 7 * 24 * 60 * 60 * 1_000,
    ).padStart(16, "0")}:${requestID}`,
    `domain-pending:${domain}:${requestedAt}:${requestID}`,
    `verification-due:${String(Date.parse(requestedAt)).padStart(
      16, "0")}:${requestID}`,
  ]);
  assert.deepEqual(pending.get("account-usage:acc_1"), {
    schema_version: 1,
    account_id: "acc_1",
    open_requests: 1,
    allocated_domains: 1,
    updated_at: requestedAt,
  });

  const rejected = rebuildAgentEmailDomainDerivedState(rejectedState());
  assert.equal(rejected.get("account-usage:acc_1").open_requests, 0);
  assert.equal(rejected.get("account-usage:acc_1").allocated_domains, 0);
  assert.equal(rejected.get("account-usage:acc_1").updated_at, rejectedAt);

  const retired = rebuildAgentEmailDomainDerivedState(retiredState());
  assert.equal(retired.get("account-usage:acc_1").open_requests, 0);
  assert.equal(retired.get("account-usage:acc_1").allocated_domains, 0);
  // Retiring an already-rejected request does not change either counter.
  assert.equal(retired.get("account-usage:acc_1").updated_at, rejectedAt);
});
