import assert from "node:assert/strict";
import test from "node:test";

import {
  AgentEmailDomainJournalRuntime,
  AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
  AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY,
  AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
  AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY,
  AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
  AGENT_EMAIL_DOMAIN_RECOVERY_KEY,
} from "../src/agent-email-domain-journal-runtime.mjs";
import {
  agentEmailDomainJournalEntryKey,
  buildAgentEmailDomainJournalEntry,
} from "../src/agent-email-domain-journal.mjs";

const STREAM = "aedj_aaaaaaaaaaaaaaaa";
const RECOVERY = "aedrec_aaaaaaaaaaaaaaaa";
const REQUEST_ID = "aedr_aaaaaaaaaaaaaaaa";
const DOMAIN = "agents.customer.example";
const ACCOUNT = "acc_customer";
const T0 = "2026-08-07T00:00:00.000Z";
const T1 = "2026-08-07T00:01:00.000Z";

class Storage {
  constructor(entries = [], alarm = null) {
    this.values = new Map(entries.map(([key, value]) =>
      [key, structuredClone(value)]));
    this.alarm = alarm;
    this.failLists = 0;
    this.lists = 0;
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

  async getAlarm() {
    return this.alarm;
  }

  async list(options = {}) {
    this.lists++;
    if (this.failLists > 0) {
      this.failLists--;
      throw new Error("durable storage unavailable");
    }
    let rows = [...this.values]
      .filter(([key]) => options.prefix === undefined ||
        key.startsWith(options.prefix))
      .filter(([key]) => options.startAfter === undefined ||
        key > options.startAfter)
      .sort(([left], [right]) => left.localeCompare(right));
    if (Number.isSafeInteger(options.limit)) rows = rows.slice(0, options.limit);
    return new Map(rows.map(([key, value]) =>
      [key, structuredClone(value)]));
  }

  async transaction(callback) {
    const staged = new Map([...this.values].map(([key, value]) =>
      [key, structuredClone(value)]));
    await callback({
      get: async (key) => {
        const value = staged.get(key);
        return value === undefined ? undefined : structuredClone(value);
      },
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    });
    this.values = staged;
  }
}

class Bucket {
  constructor() {
    this.values = new Map();
    this.failPuts = 0;
    this.failGets = 0;
    this.puts = 0;
    this.gets = 0;
  }

  async put(key, value) {
    this.puts++;
    if (this.failPuts > 0) {
      this.failPuts--;
      throw new Error("R2 unavailable");
    }
    if (this.values.has(key)) return null;
    this.values.set(key, new Uint8Array(value));
    return { key };
  }

  async get(key) {
    this.gets++;
    if (this.failGets > 0) {
      this.failGets--;
      throw new Error("R2 unavailable");
    }
    const bytes = this.values.get(key);
    if (!bytes) return null;
    return { arrayBuffer: async () => bytes.slice().buffer };
  }
}

function meta(revision = 0, updatedAt = T0) {
  return {
    schema_version: "witself.agent-email-domain.v1",
    registry_revision: revision,
    audit_sequence: revision,
    created_at: T0,
    updated_at: updatedAt,
  };
}

function pendingRequest() {
  return {
    schema_version: "witself.agent-email-domain.v1",
    id: REQUEST_ID,
    account_id: ACCOUNT,
    domain: DOMAIN,
    state: "pending_verification",
    ownership_challenge: {
      record_type: "TXT",
      record_name: `_witself-verification.${DOMAIN}`,
      record_value: `witself-domain-verification=aedv_${"a".repeat(32)}`,
      issued_at: T1,
    },
    requested_by: "operator",
    requested_at: T1,
    updated_at: T1,
    domain_limit_at_request: 1,
    plan_revision: 1,
    plan_snapshot_hash: "b".repeat(64),
    decision: null,
    retirement: null,
  };
}

function requestedAudit() {
  return {
    sequence: 1,
    registry_revision: 1,
    occurred_at: T1,
    actor_kind: "account_operator",
    actor_id: "operator",
    action: "custom_domain.requested",
    target: DOMAIN,
    metadata: {
      account_id: ACCOUNT,
      state: "pending_verification",
    },
  };
}

function pendingMutation() {
  const request = pendingRequest();
  const { schema_version: _schemaVersion, ...publicRequest } = request;
  void _schemaVersion;
  const body = {
    schema_version: "witself.agent-email-domain.v1",
    request: publicRequest,
  };
  return [
    ["meta", meta(1, T1)],
    ["audit:000000000001", requestedAudit()],
    [`request:${REQUEST_ID}`, request],
    [`domain:${DOMAIN}`, request],
    [`idem:request-create:${ACCOUNT}:create-1`, {
      fingerprint: JSON.stringify(["request.create", ACCOUNT, DOMAIN]),
      status: 202,
      body,
    }],
    [`account-request:${ACCOUNT}:${REQUEST_ID}`, REQUEST_ID],
    [`account-usage:${ACCOUNT}`, {
      schema_version: 1,
      account_id: ACCOUNT,
      open_requests: 1,
      updated_at: T1,
    }],
  ];
}

function lifecycleIntent() {
  return {
    account_id: ACCOUNT,
    operation_id: "capacity-test-suspend",
    epoch: 1,
    action: "suspend",
    cursor: null,
    failure_count: 0,
    retry_at_ms: 0,
    created_at: T1,
    updated_at: T1,
  };
}

function maintenanceInput(key = "bootstrap-test") {
  return {
    actor: { kind: "platform_admin", id: "adm_test" },
    reason: "portable custom domain recovery test",
    idempotency_key: key,
  };
}

function recoveryInput(head, key = "recovery-start") {
  return {
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    source_stream_id: STREAM,
    expected_head: {
      sequence: head.sequence,
      hash: head.hash,
    },
    reason: "recover exact custom domain authority",
    idempotency_key: key,
    active_object_name: "global",
    target_object_name: `recovery:${RECOVERY}`,
  };
}

function recoveryAction(status, key) {
  return {
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    idempotency_key: key,
    expected_action_fence: status.action_fence,
  };
}

function fixture({
  required = true,
  entries = [],
  alarm = null,
  bucket = new Bucket(),
  dependencies = {},
} = {}) {
  const storage = new Storage(entries, alarm);
  const env = {
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL: bucket,
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED: String(required),
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_STREAM_ID: STREAM,
  };
  let tick = 0;
  let recoveryFence = 0;
  const runtime = new AgentEmailDomainJournalRuntime(storage, env, {
    now: () => new Date(Date.UTC(2026, 7, 7, 1, 0, tick++)),
    newRecoveryActionFence: () =>
      (++recoveryFence).toString(16).padStart(64, "0"),
    ...dependencies,
  });
  const apply = async (puts, deletes) => storage.transaction(async (tx) => {
    for (const [key, value] of puts) await tx.put(key, value);
    for (const key of deletes) await tx.delete(key);
  });
  return { storage, bucket, runtime, apply, env };
}

async function finishMaintenance(runtime, input, apply, method = "bootstrap") {
  for (let attempt = 0; attempt < 100; attempt++) {
    const result = await runtime[method](input, apply);
    if (result.complete) return result;
  }
  throw new Error(`${method} did not complete`);
}

async function recoverAndSeal(target, head) {
  let status = await target.runtime.startRecovery(
    recoveryInput(head),
    target.apply,
  );
  for (let attempt = 0; attempt < 100 && status.phase === "replay"; attempt++) {
    status = await target.runtime.advanceRecovery(
      recoveryAction(status, `advance-${attempt}`),
      target.apply,
    );
  }
  assert.equal(status.phase, "replayed");
  for (let attempt = 0; attempt < 100 && !status.sealed; attempt++) {
    status = await target.runtime.verifyRecovery(
      recoveryAction(status, `verify-${attempt}`),
      target.apply,
    );
  }
  assert.equal(status.sealed, true);
  return status;
}

test("required journal writes R2 before local authority and resumes an exact pending write", async () => {
  const seen = [];
  const target = fixture({
    dependencies: {
      afterJournalAppend: () => {
        seen.push("r2");
        if (seen.length === 1) throw new Error("lost append acknowledgement");
      },
    },
  });

  await assert.rejects(
    target.runtime.commit([["meta", meta()]], [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_unavailable",
  );
  assert.equal(await target.storage.get("meta"), undefined);
  const pending = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY,
  );
  assert.ok(pending);
  assert.equal(pending.capacity_after.authority_keys, 1);
  const genesisCapacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  assert.equal(genesisCapacity.sequence, 0);
  assert.equal(genesisCapacity.authority_keys, 0);
  assert.equal(target.bucket.values.size, 1);

  const resumed = await target.runtime.resume(target.apply);
  assert.equal(resumed.resumed, true);
  assert.deepEqual(await target.storage.get("meta"), meta());
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY),
    undefined,
  );
  const capacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  assert.equal(capacity.sequence, resumed.head.sequence);
  assert.equal(capacity.hash, resumed.head.hash);
  assert.equal(capacity.authority_keys, 1);
  assert.deepEqual(capacity.breakdown, {
    meta: 1,
    audit: 0,
    domain: 0,
    idempotency: 0,
    lifecycle_fence: 0,
    lifecycle_intent: 0,
    plan_fence: 0,
    plan_intent: 0,
    request: 0,
  });
  assert.equal(target.bucket.values.size, 1);
});

test("genesis head and zero capacity survive failure before pending staging", async () => {
  const target = fixture();
  const transaction = target.storage.transaction.bind(target.storage);
  let transactions = 0;
  target.storage.transaction = async (callback) => {
    transactions += 1;
    if (transactions === 2) {
      throw new Error("pending staging unavailable");
    }
    return transaction(callback);
  };

  await assert.rejects(
    target.runtime.commit([["meta", meta()]], [], {}, target.apply),
    /pending staging unavailable/,
  );
  const head = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  const capacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  assert.equal(head.sequence, 0);
  assert.equal(capacity.sequence, 0);
  assert.equal(capacity.hash, head.hash);
  assert.equal(capacity.authority_keys, 0);
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY),
    undefined,
  );
  assert.equal(target.bucket.puts, 0);
  await target.runtime.assertOperationalReady();

  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const healed = await target.runtime.status();
  assert.equal(healed.healthy, true);
  assert.equal(healed.head.sequence, 1);
  assert.equal(healed.capacity.ready, true);
  assert.equal(healed.capacity.used, 1);
  assert.deepEqual(await target.storage.get("meta"), meta());
});

test("a legacy pending record without capacity cannot append or resume", async () => {
  const target = fixture({
    dependencies: {
      afterJournalAppend: () => {
        throw new Error("lost append acknowledgement");
      },
    },
  });
  await assert.rejects(
    target.runtime.commit([["meta", meta()]], [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_unavailable",
  );
  const pending = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY,
  );
  delete pending.capacity_after;
  await target.storage.put(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY, pending);
  const putsBefore = target.bucket.puts;

  await assert.rejects(
    target.runtime.resume(target.apply),
    (error) => error.code === "agent_email_domain_journal_pending_invalid",
  );
  assert.equal(target.bucket.puts, putsBefore);
  assert.equal(await target.storage.get("meta"), undefined);
  const genesisCapacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  assert.equal(genesisCapacity.sequence, 0);
  assert.equal(genesisCapacity.authority_keys, 0);
});

test("a persisted head keeps journaling after the exact-true gate is removed", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  target.env.CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED = "false";

  await target.runtime.commit(pendingMutation(), [], {}, target.apply);
  const after = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  assert.equal(after.sequence, before.sequence + 1);
  assert.equal(target.bucket.values.size, 2);
  assert.deepEqual(await target.storage.get(`domain:${DOMAIN}`), pendingRequest());
});

test("head-bound capacity tracks authority inserts, overwrites, and deletes", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  await target.runtime.commit(pendingMutation(), [], {}, target.apply);

  let status = await target.runtime.status();
  assert.deepEqual(status.capacity, {
    ready: true,
    used: 5,
    max: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
    remaining: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS - 5,
    near_limit: false,
    at_limit: false,
    breakdown: {
      meta: 1,
      audit: 1,
      domain: 1,
      idempotency: 1,
      lifecycle_fence: 0,
      lifecycle_intent: 0,
      plan_fence: 0,
      plan_intent: 0,
      request: 1,
    },
  });

  const intentKey = `lifecycle-intent:${ACCOUNT}`;
  await target.runtime.commit(
    [[intentKey, lifecycleIntent()]],
    [],
    {},
    target.apply,
  );
  assert.equal((await target.runtime.status()).capacity.used, 6);
  await target.runtime.commit(
    [[intentKey, lifecycleIntent()]],
    [],
    {},
    target.apply,
  );
  status = await target.runtime.status();
  assert.equal(status.capacity.used, 6);
  assert.equal(status.capacity.breakdown.lifecycle_intent, 1);

  await target.runtime.commit([], [intentKey], {}, target.apply);
  status = await target.runtime.status();
  assert.equal(status.capacity.used, 5);
  assert.equal(status.capacity.breakdown.lifecycle_intent, 0);
  assert.equal(await target.storage.get(intentKey), undefined);
});

test("authority commit atomically consumes only verification work local state", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const workKey = `verification-work:${REQUEST_ID}`;
  await target.storage.put(workKey, { opaque: "operational" });

  await target.runtime.commit(pendingMutation(), [workKey], {}, target.apply);
  assert.equal(await target.storage.get(workKey), undefined);
  assert.deepEqual(
    await target.storage.get(`request:${REQUEST_ID}`),
    pendingRequest(),
  );

  await assert.rejects(
    target.runtime.commit([], [AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY], {},
      target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_local_storage_key",
  );
  await assert.rejects(
    target.runtime.commit([[workKey, { opaque: "replacement" }]], [], {},
      target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_local_storage_key",
  );
});

test("stale capacity fails operational readiness and exposes unknown status fields", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const capacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  await target.storage.put(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, {
    ...capacity,
    hash: "f".repeat(64),
  });

  const status = await target.runtime.status();
  assert.equal(status.healthy, false);
  assert.equal(
    status.degradation_code,
    "agent_email_domain_journal_capacity_unavailable",
  );
  assert.deepEqual(status.capacity, {
    ready: false,
    used: null,
    max: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
    remaining: null,
    near_limit: null,
    at_limit: null,
    breakdown: null,
  });
  await assert.rejects(
    target.runtime.assertOperationalReady(),
    (error) => error.code ===
      "agent_email_domain_journal_capacity_unavailable",
  );
});

test("authority admission rejects the hard limit before pending or R2 work", async () => {
  const logs = [];
  const target = fixture({
    dependencies: {
      log: (line) => logs.push(JSON.parse(line)),
    },
  });
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const capacity = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
  );
  await target.storage.put(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, {
    ...capacity,
    authority_keys: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
    breakdown: {
      ...capacity.breakdown,
      audit: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS - 1,
    },
  });
  const atLimit = await target.runtime.status();
  assert.equal(atLimit.capacity.ready, true);
  assert.equal(atLimit.capacity.remaining, 0);
  assert.equal(atLimit.capacity.near_limit, true);
  assert.equal(atLimit.capacity.at_limit, true);
  const putsBefore = target.bucket.puts;
  const getsBefore = target.bucket.gets;

  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_authority_limit_exceeded",
  );

  assert.equal(target.bucket.puts, putsBefore);
  assert.equal(target.bucket.gets, getsBefore);
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY),
    undefined,
  );
  assert.equal(await target.storage.get(`domain:${DOMAIN}`), undefined);
  assert.deepEqual(logs, [{
    event: "agent_email_domain_authority_capacity_refused",
    code: "agent_email_domain_journal_authority_limit_exceeded",
    used: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
    attempted: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS + 4,
    max: AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS,
  }]);
});

test("a swapped R2 bucket permanently fences before local authority advances", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  const replacement = new Bucket();
  target.env.AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL = replacement;

  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_gap",
  );

  assert.deepEqual(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    before,
  );
  assert.deepEqual(await target.storage.get("meta"), meta());
  assert.equal(await target.storage.get(`domain:${DOMAIN}`), undefined);
  assert.ok(await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY));
  assert.equal(replacement.puts, 0);
  const fence = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY);
  assert.equal(fence.code, "agent_email_domain_journal_gap");
  assert.equal(fence.stream_id, before.stream_id);
  assert.equal(fence.sequence, before.sequence);
  const status = await target.runtime.status();
  assert.equal(status.enabled, true);
  assert.equal(status.healthy, false);
  assert.equal(status.forked, true);
  assert.equal(status.degradation_code, "agent_email_domain_journal_gap");
});

test("journal status probes remote continuity without fencing a transient outage", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  target.bucket.failGets = 1;

  const unavailable = await target.runtime.status();
  assert.equal(unavailable.enabled, true);
  assert.equal(unavailable.healthy, false);
  assert.equal(unavailable.forked, false);
  assert.equal(unavailable.remote_head_checked, true);
  assert.equal(unavailable.remote_head_healthy, false);
  assert.equal(
    unavailable.degradation_code,
    "agent_email_domain_journal_unavailable",
  );
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY),
    undefined,
  );

  const recovered = await target.runtime.status();
  assert.equal(recovered.healthy, true);
  assert.equal(recovered.forked, false);
  assert.equal(recovered.remote_head_healthy, true);
  assert.equal(recovered.degradation_code, null);
});

test("a transient continuity read leaves the exact pending append retryable", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  target.bucket.failGets = 1;

  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_unavailable",
  );
  assert.deepEqual(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    before,
  );
  assert.equal(await target.storage.get(`domain:${DOMAIN}`), undefined);
  assert.ok(await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY));
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY),
    undefined,
  );

  const resumed = await target.runtime.resume(target.apply);
  assert.equal(resumed.resumed, true);
  assert.equal(resumed.head.sequence, before.sequence + 1);
  assert.deepEqual(await target.storage.get(`domain:${DOMAIN}`), pendingRequest());
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY),
    undefined,
  );
});

test("a deleted remote head permanently fences before the next append", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  target.bucket.values.delete(agentEmailDomainJournalEntryKey(
    before.stream_id,
    before.sequence,
  ));

  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_gap",
  );

  assert.deepEqual(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    before,
  );
  assert.deepEqual(await target.storage.get("meta"), meta());
  assert.equal(await target.storage.get(`request:${REQUEST_ID}`), undefined);
  assert.equal(target.bucket.puts, 1);
  assert.equal(
    (await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)).code,
    "agent_email_domain_journal_gap",
  );
});

test("a canonical remote head mismatch installs a durable fork fence", async () => {
  const target = fixture();
  await target.runtime.commit([["meta", meta()]], [], {}, target.apply);
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  const key = agentEmailDomainJournalEntryKey(
    before.stream_id,
    before.sequence,
  );
  const original = JSON.parse(
    new TextDecoder().decode(target.bucket.values.get(key)),
  );
  const { entry_hash: _entryHash, ...unsigned } = original;
  void _entryHash;
  const conflicting = await buildAgentEmailDomainJournalEntry({
    ...unsigned,
    actor: { kind: "system", id: "conflicting-authority" },
  });
  target.bucket.values.set(key, conflicting.bytes);

  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_fork_detected",
  );

  assert.deepEqual(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    before,
  );
  assert.deepEqual(await target.storage.get("meta"), meta());
  assert.equal(await target.storage.get(`domain:${DOMAIN}`), undefined);
  const fence = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY);
  assert.equal(fence.code, "agent_email_domain_journal_fork_detected");
  assert.equal(fence.sequence, before.sequence);
});

test("checkpoint verifies its non-genesis source head before appending", async () => {
  const target = fixture({ entries: [["meta", meta()]] });
  await finishMaintenance(
    target.runtime,
    maintenanceInput("checkpoint-source-bootstrap"),
    target.apply,
  );
  const before = await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  const replacement = new Bucket();
  target.env.AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL = replacement;

  await assert.rejects(
    finishMaintenance(
      target.runtime,
      maintenanceInput("checkpoint-after-bucket-swap"),
      target.apply,
      "checkpoint",
    ),
    (error) => error.code === "agent_email_domain_journal_gap",
  );

  assert.deepEqual(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
    before,
  );
  assert.deepEqual(await target.storage.get("meta"), meta());
  assert.equal(replacement.puts, 0);
  assert.ok(await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY));
  assert.equal(
    (await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)).code,
    "agent_email_domain_journal_gap",
  );
});

test("a checkpoint upgrades a legacy head with no capacity record", async () => {
  const target = fixture({ entries: [["meta", meta()]] });
  const bootstrapped = await finishMaintenance(
    target.runtime,
    maintenanceInput("legacy-capacity-bootstrap"),
    target.apply,
  );
  await target.storage.delete(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY);

  const legacy = await target.runtime.status();
  assert.equal(legacy.head.sequence, bootstrapped.head.sequence);
  assert.equal(legacy.healthy, false);
  assert.equal(legacy.capacity.ready, false);
  await assert.rejects(
    target.runtime.assertOperationalReady(),
    (error) => error.code ===
      "agent_email_domain_journal_capacity_unavailable",
  );

  const checkpoint = await finishMaintenance(
    target.runtime,
    maintenanceInput("legacy-capacity-checkpoint"),
    target.apply,
    "checkpoint",
  );
  const upgraded = await target.runtime.status();
  assert.equal(checkpoint.head.sequence, bootstrapped.head.sequence + 1);
  assert.equal(upgraded.head.sequence, checkpoint.head.sequence);
  assert.equal(upgraded.healthy, true);
  assert.equal(upgraded.capacity.ready, true);
  assert.equal(upgraded.capacity.used, 1);
  assert.equal(upgraded.capacity.breakdown.meta, 1);
});

test("required mode rejects existing unbootstrapped authority", async () => {
  const target = fixture({ entries: [["meta", meta()]] });
  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_bootstrap_required",
  );
  assert.equal(target.bucket.puts, 0);
});

test("bootstrap atomically initializes a truly empty registry", async () => {
  const target = fixture();
  const complete = await finishMaintenance(
    target.runtime,
    maintenanceInput("initialize-empty-registry"),
    target.apply,
  );
  assert.equal(complete.complete, true);
  assert.ok(complete.head.sequence >= 2);
  assert.deepEqual(await target.storage.get("meta"), {
    schema_version: "witself.agent-email-domain.v1",
    registry_revision: 0,
    audit_sequence: 0,
    created_at: "2026-08-07T01:00:00.000Z",
    updated_at: "2026-08-07T01:00:00.000Z",
  });
  const status = await target.runtime.status();
  assert.equal(status.enabled, true);
  assert.equal(status.required, true);
  assert.equal(status.healthy, true);
  assert.equal(status.capacity.ready, true);
  assert.equal(status.capacity.used, 1);
  assert.equal(status.capacity.max, AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS);
  assert.equal(status.capacity.breakdown.meta, 1);
  await target.runtime.assertOperationalReady();
});

test("bootstrap freeze survives an R2 crash and exact retry completes", async () => {
  let crash = true;
  const target = fixture({
    entries: [["meta", meta()]],
    dependencies: {
      afterJournalAppend: () => {
        if (crash) {
          crash = false;
          throw new Error("process died after R2 append");
        }
      },
    },
  });
  const input = maintenanceInput();

  await assert.rejects(
    target.runtime.bootstrap(input, target.apply),
    (error) => error.code === "agent_email_domain_journal_unavailable",
  );
  const frozen = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
  );
  assert.equal(frozen.kind, "bootstrap");
  assert.ok(frozen.pending);
  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code === "agent_email_domain_journal_write_frozen",
  );

  const complete = await finishMaintenance(
    target.runtime,
    input,
    target.apply,
  );
  assert.equal(complete.complete, true);
  assert.equal(
    await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY),
    undefined,
  );
  assert.equal((await target.runtime.status()).pending, false);
});

test("legacy in-progress maintenance cannot advance before capacity validation", async () => {
  for (const missing of ["record", "pending_next"]) {
    const target = fixture({
      entries: [["meta", meta()]],
      dependencies: {
        afterJournalAppend: () => {
          throw new Error("process died after R2 append");
        },
      },
    });
    const input = maintenanceInput(`legacy-maintenance-${missing}`);
    await assert.rejects(
      target.runtime.bootstrap(input, target.apply),
      (error) => error.code === "agent_email_domain_journal_unavailable",
    );
    const legacy = await target.storage.get(
      AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
    );
    assert.ok(legacy.pending);
    if (missing === "record") delete legacy.authority_breakdown;
    else delete legacy.pending.next.authority_breakdown;
    await target.storage.put(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY, legacy);
    const putsBefore = target.bucket.puts;
    const headBefore = await target.storage.get(
      AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
    );

    await assert.rejects(
      target.runtime.bootstrap(input, target.apply),
      (error) => error.code ===
        "agent_email_domain_journal_maintenance_invalid",
    );
    assert.equal(target.bucket.puts, putsBefore);
    assert.deepEqual(
      await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
      headBefore,
    );
    assert.deepEqual(
      await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY),
      legacy,
    );
  }
});

test("completed maintenance idempotency survives intervening checkpoints", async () => {
  const target = fixture({ entries: [["meta", meta()]] });
  const bootstrapInput = maintenanceInput("durable-maintenance-key");
  const bootstrap = await finishMaintenance(
    target.runtime,
    bootstrapInput,
    target.apply,
  );
  const checkpoint = await finishMaintenance(
    target.runtime,
    maintenanceInput("later-checkpoint-key"),
    target.apply,
    "checkpoint",
  );
  assert.ok(checkpoint.head.sequence > bootstrap.head.sequence);

  assert.deepEqual(
    await target.runtime.bootstrap(bootstrapInput, target.apply),
    bootstrap,
  );
  await assert.rejects(
    target.runtime.checkpoint(bootstrapInput, target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_idempotency_conflict",
  );
  await assert.rejects(
    target.runtime.bootstrap({
      ...bootstrapInput,
      reason: "changed historical maintenance meaning",
    }, target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_idempotency_conflict",
  );
});

test("bootstrap detects source account-index or usage drift and keeps the freeze", async () => {
  const rows = pendingMutation();
  const usageIndex = rows.findIndex(([key]) =>
    key === `account-usage:${ACCOUNT}`);
  rows[usageIndex][1] = {
    ...rows[usageIndex][1],
    open_requests: 0,
  };
  const target = fixture({ entries: rows });
  const input = maintenanceInput("drifted-bootstrap");

  await assert.rejects(
    finishMaintenance(target.runtime, input, target.apply),
    (error) => error.code ===
      "agent_email_domain_journal_derived_state_mismatch",
  );
  assert.ok(await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
  ));
  await assert.rejects(
    target.runtime.assertOperationalReady(),
    (error) => error.code === "agent_email_domain_journal_write_frozen",
  );
});

test("recovery start requires a literally empty and alarm-free named target", async () => {
  const expected = { sequence: 1, hash: "a".repeat(64) };

  const alarmed = fixture({ alarm: Date.now() + 60_000 });
  await assert.rejects(
    alarmed.runtime.startRecovery(recoveryInput(expected), alarmed.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_target_not_empty",
  );
  assert.equal(alarmed.bucket.gets, 0);
  assert.equal(alarmed.storage.values.size, 0);

  const nonempty = fixture({ entries: [["meta", meta()]] });
  await assert.rejects(
    nonempty.runtime.startRecovery(recoveryInput(expected), nonempty.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_target_not_empty",
  );
  assert.equal(nonempty.bucket.gets, 0);

  const empty = fixture();
  const started = await empty.runtime.startRecovery(
    recoveryInput(expected),
    empty.apply,
  );
  assert.equal(started.phase, "replay");
  assert.deepEqual([...empty.storage.values.keys()], [
    AGENT_EMAIL_DOMAIN_RECOVERY_KEY,
  ]);
  assert.equal(await empty.storage.get("meta"), undefined);
  await empty.storage.put("account-usage:injected", {
    schema_version: 1,
    account_id: "injected",
    open_requests: 0,
    updated_at: T0,
  });
  await assert.rejects(
    empty.runtime.startRecovery(recoveryInput(expected), empty.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_target_not_empty",
  );

  await assert.rejects(
    fixture().runtime.startRecovery({
      ...recoveryInput(expected),
      target_object_name: "global",
    }),
    (error) => error.code ===
      "agent_email_domain_recovery_request_invalid",
  );
});

test("lost recovery acknowledgements replay the exact receipted action", async () => {
  const source = fixture({ entries: [["meta", meta()]] });
  const complete = await finishMaintenance(
    source.runtime,
    maintenanceInput("lost-ack-source"),
    source.apply,
  );
  let loseAcknowledgement = true;
  const target = fixture({
    bucket: source.bucket,
    dependencies: {
      afterRecoveryAction: (result) => {
        if (loseAcknowledgement && result.phase === "replay") {
          loseAcknowledgement = false;
          throw new Error("lost recovery acknowledgement");
        }
      },
    },
  });
  const started = await target.runtime.startRecovery(
    recoveryInput(complete.head),
    target.apply,
  );
  const action = recoveryAction(started, "lost-advance");

  await assert.rejects(
    target.runtime.advanceRecovery(action, target.apply),
    /lost recovery acknowledgement/,
  );
  const durable = await target.runtime.recoveryStatus();
  assert.equal(durable.replay_head.sequence, 1);
  assert.notEqual(durable.action_fence, started.action_fence);
  assert.deepEqual(
    await target.runtime.advanceRecovery(action, target.apply),
    durable,
  );
});

test("recovery crosses a complete checkpoint through a later mutation and seals permanently", async () => {
  const source = fixture({ entries: [["meta", meta()]] });
  await finishMaintenance(
    source.runtime,
    maintenanceInput("source-bootstrap"),
    source.apply,
  );
  const checkpointHead = await source.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
  );
  await source.runtime.commit(pendingMutation(), [], {}, source.apply);
  const mutationHead = await source.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY,
  );
  assert.equal(mutationHead.sequence, checkpointHead.sequence + 1);

  const target = fixture({ bucket: source.bucket });
  const sealed = await recoverAndSeal(target, mutationHead);
  assert.equal(sealed.replay_head.sequence, mutationHead.sequence);
  assert.equal(sealed.replay_head.hash, mutationHead.hash);
  assert.deepEqual(
    await target.storage.get(`domain:${DOMAIN}`),
    pendingRequest(),
  );
  assert.equal(
    (await target.storage.get(`account-usage:${ACCOUNT}`)).open_requests,
    1,
  );
  const journalStatus = await target.runtime.status();
  assert.equal(journalStatus.capacity.ready, true);
  assert.equal(journalStatus.capacity.used, 5);
  assert.deepEqual(journalStatus.capacity.breakdown, {
    meta: 1,
    audit: 1,
    domain: 1,
    idempotency: 1,
    lifecycle_fence: 0,
    lifecycle_intent: 0,
    plan_fence: 0,
    plan_intent: 0,
    request: 1,
  });
  await assert.rejects(
    target.runtime.commit(pendingMutation(), [], {}, target.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_target_sealed",
  );
  await assert.rejects(
    target.runtime.bootstrap(maintenanceInput("sealed-bootstrap"), target.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_target_sealed",
  );
});

test("recovery refuses an exact mutation head whose chain has no checkpoint", async () => {
  const source = fixture();
  await source.runtime.commit([["meta", meta()]], [], {}, source.apply);
  const head = await source.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
  const target = fixture({ bucket: source.bucket });
  const started = await target.runtime.startRecovery(
    recoveryInput(head),
    target.apply,
  );

  await assert.rejects(
    target.runtime.advanceRecovery(
      recoveryAction(started, "advance-without-checkpoint"),
      target.apply,
    ),
    (error) => error.code ===
      "agent_email_domain_recovery_checkpoint_invalid",
  );
  const failed = await target.runtime.recoveryStatus();
  assert.equal(failed.failed, true);
  assert.equal(
    failed.failure_code,
    "agent_email_domain_recovery_checkpoint_invalid",
  );
});

test("stale recovery action fences cannot advance or read R2", async () => {
  const source = fixture({ entries: [["meta", meta()]] });
  const head = await finishMaintenance(
    source.runtime,
    maintenanceInput("stale-source"),
    source.apply,
  );
  const target = fixture({ bucket: source.bucket });
  const started = await target.runtime.startRecovery(
    recoveryInput(head.head),
    target.apply,
  );
  const first = recoveryAction(started, "advance-first");
  const advanced = await target.runtime.advanceRecovery(first, target.apply);
  const gets = target.bucket.gets;

  await assert.rejects(
    target.runtime.advanceRecovery({
      ...first,
      idempotency_key: "advance-stale-different",
    }, target.apply),
    (error) => error.code ===
      "agent_email_domain_recovery_action_fence_mismatch",
  );
  assert.equal(target.bucket.gets, gets);
  assert.deepEqual(await target.runtime.recoveryStatus(), advanced);
});

test("a conflicting create-only R2 object installs a permanent fork fence", async () => {
  const target = fixture();
  target.bucket.failPuts = 1;
  await assert.rejects(
    target.runtime.commit([["meta", meta()]], [], {}, target.apply),
  );
  const pending = await target.storage.get(
    AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY,
  );
  const key = `agent-email-domain-authority/v1/streams/${STREAM}/entries/` +
    "00000000000000000001.json";
  target.bucket.values.set(key, new TextEncoder().encode("different"));

  await assert.rejects(
    target.runtime.resume(target.apply),
    (error) => error.code === "agent_email_domain_journal_fork_detected",
  );
  assert.equal(
    (await target.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)).sequence,
    pending.entry.sequence,
  );
  await assert.rejects(
    target.runtime.assertOperationalReady(),
    (error) => error.code === "agent_email_domain_journal_fork_detected",
  );
});
