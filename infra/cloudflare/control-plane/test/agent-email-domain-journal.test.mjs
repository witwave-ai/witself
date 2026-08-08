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
    metadata: { account_id: "acc_1", state: "pending_verification" },
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
  ]) {
    assert.equal(classifyAgentEmailDomainStorageKey(key), "authority", key);
  }
  for (const key of [
    `account-request:acc_1:${requestID}`, "account-usage:acc_1",
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

test("after-images are bounded, sorted, authority-only, and never delete", () => {
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

test("derived rebuild recreates only account indexes and exact usage history", () => {
  const pending = rebuildAgentEmailDomainDerivedState(pendingState());
  assert.deepEqual([...pending.keys()].sort(), [
    `account-request:acc_1:${requestID}`,
    "account-usage:acc_1",
  ]);
  assert.deepEqual(pending.get("account-usage:acc_1"), {
    schema_version: 1,
    account_id: "acc_1",
    open_requests: 1,
    updated_at: requestedAt,
  });

  const rejected = rebuildAgentEmailDomainDerivedState(rejectedState());
  assert.equal(rejected.get("account-usage:acc_1").open_requests, 0);
  assert.equal(rejected.get("account-usage:acc_1").updated_at, rejectedAt);

  // Rejected -> retired does not change pending usage, so the exact historical
  // counter timestamp remains the rejection audit time.
  const retired = rebuildAgentEmailDomainDerivedState(retiredState());
  assert.equal(retired.get("account-usage:acc_1").open_requests, 0);
  assert.equal(retired.get("account-usage:acc_1").updated_at, rejectedAt);
});
