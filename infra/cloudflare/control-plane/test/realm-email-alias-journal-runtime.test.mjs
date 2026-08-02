import assert from "node:assert/strict";
import test from "node:test";

import {
  RealmEmailAliasJournalRuntime,
  REALM_EMAIL_ALIAS_JOURNAL_FORK_KEY,
  REALM_EMAIL_ALIAS_JOURNAL_BOOTSTRAP_KEY,
  REALM_EMAIL_ALIAS_JOURNAL_META_KEY,
  REALM_EMAIL_ALIAS_JOURNAL_PENDING_KEY,
  REALM_EMAIL_ALIAS_RECOVERY_KEY,
} from "../src/realm-email-alias-journal-runtime.mjs";
import {
  buildRealmEmailAliasJournalEntry,
  realmEmailAliasJournalEntryKey,
} from "../src/realm-email-alias-journal.mjs";

const STREAM = "reaj_aaaaaaaaaaaaaaaa";
const RECOVERY = "rear_aaaaaaaaaaaaaaaa";

class Storage {
  constructor(entries = []) {
    this.values = new Map(entries.map(([key, value]) =>
      [key, structuredClone(value)]));
  }
  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }
  async put(key, value) { this.values.set(key, structuredClone(value)); }
  async delete(key) { this.values.delete(key); }
  async list(options = {}) {
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
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    });
    this.values = staged;
  }
}

class Bucket {
  constructor() {
    this.values = new Map();
    this.fail = false;
    this.puts = 0;
  }
  async put(key, value) {
    this.puts++;
    if (this.fail) throw new Error("R2 unavailable");
    if (this.values.has(key)) return null;
    this.values.set(key, new Uint8Array(value));
    return { key };
  }
  async get(key) {
    const bytes = this.values.get(key);
    if (!bytes) return null;
    return { arrayBuffer: async () => bytes.slice().buffer };
  }
}

function meta(revision = 1, audit = 1) {
  return {
    schema_version: "witself.realm-email-alias.v1",
    seeded: true,
    registry_revision: revision,
    reserved_policy_version: 1,
    audit_sequence: audit,
    created_at: "2026-08-02T00:00:00.000Z",
    updated_at: "2026-08-02T00:00:00.000Z",
  };
}

function authorityFixture(extra = []) {
  return [
    ["meta", meta()],
    ["audit:000000000001", {
      sequence: 1,
      registry_revision: 1,
      occurred_at: "2026-08-02T00:00:00.000Z",
      actor_kind: "system",
      actor_id: "seed",
      action: "registry.seeded",
      target: "registry",
      metadata: { phase: "committed" },
    }],
    ...extra,
  ];
}

function maintenanceInput(idempotencyKey = "bootstrap-test") {
  return {
    actor: { kind: "platform_admin", id: "adm_test" },
    reason: "portable recovery test",
    idempotency_key: idempotencyKey,
  };
}

function recoveryInput(expectedHead, idempotencyKey = "recovery-start") {
  return {
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    source_stream_id: STREAM,
    expected_head: expectedHead,
    reason: "recover exact tested checkpoint",
    idempotency_key: idempotencyKey,
    active_object_name: "global",
    target_object_name: `recovery:${RECOVERY}`,
  };
}

function fixture({
  required = true,
  entries = [],
  bucket = new Bucket(),
  dependencies = {},
  external = {},
} = {}) {
  const storage = new Storage(entries);
  const env = {
    REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL: bucket,
    CP_REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_ENABLED: String(required),
    CP_REALM_EMAIL_ALIAS_AUTHORITY_STREAM_ID:
      STREAM,
    ...external,
  };
  let tick = 0;
  const runtime = new RealmEmailAliasJournalRuntime(storage, env, {
    now: () => new Date(Date.UTC(2026, 7, 2, 0, 0, tick++)),
    ...dependencies,
  });
  const apply = async (puts, deletes) => storage.transaction(async (tx) => {
    for (const [key, value] of puts) await tx.put(key, value);
    for (const key of deletes) await tx.delete(key);
  });
  return { storage, bucket, runtime, apply };
}

async function finishMaintenance(runtime, input, apply, method = "bootstrap") {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    const result = await runtime[method](input, apply);
    if (result.complete) return result;
  }
  throw new Error(`${method} did not complete`);
}

test("disabled journal leaves an existing registry unchanged and does no R2 I/O", async () => {
  const { storage, bucket, runtime, apply } = fixture({
    required: false,
    entries: [["meta", meta()]],
  });
  await runtime.commit([["plan-fence:acct_a", { account_id: "acct_a" }]], [], {}, apply);
  assert.deepEqual(await storage.get("plan-fence:acct_a"), {
    account_id: "acct_a",
  });
  assert.equal(bucket.puts, 0);
  assert.equal(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_META_KEY), undefined);
});

test("required journal seeds a new object through R2 before local authority", async () => {
  const { storage, bucket, runtime, apply } = fixture();
  await runtime.commit([["meta", meta()]], [], {}, apply);
  const head = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_META_KEY);
  assert.equal(head.stream_id, "reaj_aaaaaaaaaaaaaaaa");
  assert.equal(head.sequence, 1);
  assert.equal(bucket.values.size, 1);
  assert.deepEqual(await storage.get("meta"), meta());
  assert.equal(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_PENDING_KEY), undefined);
});

test("required journal refuses an existing unbootstrapped authority", async () => {
  const { runtime, apply } = fixture({ entries: [["meta", meta()]] });
  await assert.rejects(
    runtime.commit([["plan-fence:acct_a", { account_id: "acct_a" }]], [], {}, apply),
    (error) => error.code === "realm_email_alias_journal_bootstrap_required",
  );
});

test("R2 failure leaves an exact pending mutation and retry resumes it", async () => {
  const { storage, bucket, runtime, apply } = fixture();
  bucket.fail = true;
  await assert.rejects(
    runtime.commit([["meta", meta()]], [], {}, apply),
    (error) => error.code === "realm_email_alias_journal_unavailable",
  );
  assert.equal(await storage.get("meta"), undefined);
  assert.ok(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_PENDING_KEY));
  bucket.fail = false;
  const resumed = await runtime.resume(apply);
  assert.equal(resumed.resumed, true);
  assert.deepEqual(await storage.get("meta"), meta());
  assert.equal(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_PENDING_KEY), undefined);
});

test("conflicting create-only object records a permanent local fork fence", async () => {
  const { storage, bucket, runtime, apply } = fixture();
  bucket.fail = true;
  await assert.rejects(runtime.commit([["meta", meta()]], [], {}, apply));
  const pending = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_PENDING_KEY);
  const key = `realm-email-alias-authority/v1/streams/` +
    `${pending.entry.stream_id}/entries/00000000000000000001.json`;
  bucket.values.set(key, new TextEncoder().encode("different"));
  bucket.fail = false;
  await assert.rejects(
    runtime.resume(apply),
    (error) => error.code === "realm_email_alias_journal_fork_detected",
  );
  const fork = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_FORK_KEY);
  assert.equal(fork.sequence, 1);
  await assert.rejects(
    runtime.resume(apply),
    (error) => error.code === "realm_email_alias_journal_fork_detected",
  );
});

test("bootstrap freezes an existing authority and survives the R2 crash window exactly", async () => {
  let crash = true;
  const { storage, bucket, runtime, apply } = fixture({
    entries: authorityFixture(),
    dependencies: {
      afterJournalAppend: () => {
        if (crash) {
          crash = false;
          throw new Error("simulated process death after R2 append");
        }
      },
    },
  });
  const input = maintenanceInput();
  await assert.rejects(
    runtime.bootstrap(input, apply),
    (error) => error.code === "realm_email_alias_journal_unavailable",
  );
  const frozen = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_BOOTSTRAP_KEY);
  assert.equal(frozen.kind, "bootstrap");
  assert.ok(frozen.pending?.entry);
  assert.equal(bucket.values.size, 1);
  const pendingBytes = bucket.values.values().next().value;

  const complete = await finishMaintenance(runtime, input, apply);
  assert.equal(complete.complete, true);
  assert.equal(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_BOOTSTRAP_KEY), undefined);
  const head = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_META_KEY);
  assert.equal(head.sequence, 2);
  assert.equal(bucket.values.size, 2);
  assert.deepEqual(bucket.values.values().next().value, pendingBytes);
});

test("checkpoint freezes the exact active head and records a complete digest", async () => {
  const { storage, bucket, runtime, apply } = fixture();
  await runtime.commit(authorityFixture(), [], {}, apply);
  const source = await storage.get(REALM_EMAIL_ALIAS_JOURNAL_META_KEY);
  const complete = await finishMaintenance(
    runtime,
    maintenanceInput("checkpoint-test"),
    apply,
    "checkpoint",
  );
  assert.equal(complete.complete, true);
  assert.equal(complete.head.sequence, source.sequence + 1);
  const object = await bucket.get(
    realmEmailAliasJournalEntryKey(STREAM, complete.head.sequence),
  );
  const entry = JSON.parse(new TextDecoder().decode(
    new Uint8Array(await object.arrayBuffer()),
  ));
  assert.equal(entry.kind, "checkpoint");
  assert.equal(entry.metadata.source_head.sequence, source.sequence);
  assert.match(entry.metadata.authority_digest, /^[0-9a-f]{64}$/);
  assert.equal(entry.metadata.authority_keys, authorityFixture().length);
});

test("lost maintenance completion acknowledgement replays one bounded receipt", async () => {
  let crash = true;
  let completedResult;
  const source = fixture({
    entries: authorityFixture(),
    dependencies: {
      afterMaintenanceFinalize: (result) => {
        if (crash) {
          completedResult = structuredClone(result);
          crash = false;
          throw new Error("simulated lost final maintenance acknowledgement");
        }
      },
    },
  });
  const input = maintenanceInput("bootstrap-final-ack");
  let lost = false;
  for (let attempt = 0; attempt < 20 && !lost; attempt += 1) {
    try {
      await source.runtime.bootstrap(input, source.apply);
    } catch (error) {
      assert.match(error.message, /lost final maintenance acknowledgement/);
      lost = true;
    }
  }
  assert.equal(lost, true);
  const r2Objects = source.bucket.values.size;
  assert.deepEqual(
    await source.runtime.bootstrap(input, source.apply),
    completedResult,
  );
  assert.equal(source.bucket.values.size, r2Objects);

  const checkpoint = await finishMaintenance(
    source.runtime,
    maintenanceInput("later-distinct-checkpoint"),
    source.apply,
    "checkpoint",
  );
  assert.equal(checkpoint.complete, true);
  assert.ok(checkpoint.head.sequence > completedResult.head.sequence);
});

test("bootstrap refuses more than 10000 authority keys and keeps writes frozen", async () => {
  const extra = Array.from({ length: 9_999 }, (_, index) => [
    `idem:test:${String(index).padStart(5, "0")}`,
    { index },
  ]);
  const { storage, runtime, apply } = fixture({
    entries: authorityFixture(extra),
  });
  const input = maintenanceInput("bootstrap-too-large");
  let error = null;
  for (let attempt = 0; attempt < 200 && error === null; attempt += 1) {
    try {
      await runtime.bootstrap(input, apply);
    } catch (caught) {
      error = caught;
    }
  }
  assert.equal(error?.code, "realm_email_alias_journal_authority_limit_exceeded");
  assert.ok(await storage.get(REALM_EMAIL_ALIAS_JOURNAL_BOOTSTRAP_KEY));
});

test("recovery rejects a nonempty or active target before reading R2", async () => {
  const bucket = new Bucket();
  const nonempty = fixture({
    bucket,
    entries: [["claim-skeleton:occupied", "occupied"]],
  });
  await assert.rejects(
    nonempty.runtime.startRecovery(recoveryInput({
      sequence: 1,
      hash: "a".repeat(64),
    }), nonempty.apply),
    (error) => error.code === "realm_email_alias_recovery_target_not_empty",
  );
  assert.equal(bucket.puts, 0);

  const receiptOnly = fixture({
    bucket,
    entries: [["realm-email-alias-journal:last-maintenance", {
      schema_version: "witself.realm-email-alias-journal-maintenance.v1",
    }]],
  });
  await assert.rejects(
    receiptOnly.runtime.startRecovery(recoveryInput({
      sequence: 1,
      hash: "a".repeat(64),
    }), receiptOnly.apply),
    (error) => error.code === "realm_email_alias_recovery_target_not_empty",
  );

  const empty = fixture({ bucket });
  await assert.rejects(
    empty.runtime.startRecovery({
      ...recoveryInput({ sequence: 1, hash: "a".repeat(64) }),
      target_object_name: "global",
    }, empty.apply),
    (error) => error.code === "realm_email_alias_recovery_request_invalid",
  );
  assert.equal(bucket.puts, 0);
});

async function bootstrapSource() {
  const source = fixture({ entries: authorityFixture() });
  const complete = await finishMaintenance(
    source.runtime,
    maintenanceInput("source-bootstrap"),
    source.apply,
  );
  return { ...source, head: complete.head };
}

test("bounded recovery replays, rebuilds, and permanently seals without external I/O", async () => {
  const source = await bootstrapSource();
  let externalCalls = 0;
  const external = {
    DIRECTORY: { get: () => { externalCalls += 1; throw new Error("forbidden"); } },
    AGENT_EMAIL_DIRECTORY: {
      put: () => { externalCalls += 1; throw new Error("forbidden"); },
    },
  };
  const target = fixture({ bucket: source.bucket, external });
  await target.runtime.startRecovery(recoveryInput({
    sequence: source.head.sequence,
    hash: source.head.hash,
  }), target.apply);
  let status;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    status = await target.runtime.advanceRecovery({
      actor: { kind: "platform_admin", id: "adm_test" },
      recovery_id: RECOVERY,
      idempotency_key: `advance-${attempt}`,
    }, target.apply);
    if (status.phase === "replayed") break;
  }
  assert.equal(status.phase, "replayed");
  for (let attempt = 0; attempt < 20; attempt += 1) {
    status = await target.runtime.verifyRecovery({
      actor: { kind: "platform_admin", id: "adm_test" },
      recovery_id: RECOVERY,
      idempotency_key: `verify-${attempt}`,
    }, target.apply);
    if (status.sealed) break;
  }
  assert.equal(status.sealed, true);
  assert.equal(status.state_digest, (await target.runtime.recoveryStatus()).state_digest);
  assert.equal(externalCalls, 0);
  assert.ok(await target.storage.get(REALM_EMAIL_ALIAS_RECOVERY_KEY));
  await assert.rejects(
    target.runtime.commit([["plan-fence:acct", { value: 1 }]], [], {}, target.apply),
    (error) => error.code === "realm_email_alias_recovery_target_sealed",
  );
});

test("lost recovery action acknowledgements replay exactly and changed reuse conflicts", async () => {
  const source = await bootstrapSource();
  let crashAction = "advance";
  const target = fixture({
    bucket: source.bucket,
    dependencies: {
      afterRecoveryAction: (result) => {
        if (crashAction === "advance" && result.phase === "replay") {
          crashAction = null;
          throw new Error("simulated lost advance acknowledgement");
        }
        if (crashAction === "verify" && result.sealed) {
          crashAction = null;
          throw new Error("simulated lost verify acknowledgement");
        }
      },
    },
  });
  await target.runtime.startRecovery(recoveryInput({
    sequence: source.head.sequence,
    hash: source.head.hash,
  }), target.apply);
  const advance = {
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    idempotency_key: "lost-advance",
  };
  await assert.rejects(
    target.runtime.advanceRecovery(advance, target.apply),
    /lost advance acknowledgement/,
  );
  const afterLostAdvance = await target.runtime.recoveryStatus();
  assert.equal(afterLostAdvance.replay_head.sequence, 1);
  assert.deepEqual(
    await target.runtime.advanceRecovery(advance, target.apply),
    afterLostAdvance,
  );
  await assert.rejects(
    target.runtime.advanceRecovery({
      ...advance,
      actor: { kind: "platform_admin", id: "different_admin" },
    }, target.apply),
    (error) => error.code === "realm_email_alias_recovery_idempotency_conflict",
  );

  const replayed = await target.runtime.advanceRecovery({
    ...advance,
    idempotency_key: "finish-replay",
  }, target.apply);
  assert.equal(replayed.phase, "replayed");
  crashAction = "verify";
  const verify = {
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    idempotency_key: "lost-verify",
  };
  await assert.rejects(
    target.runtime.verifyRecovery(verify, target.apply),
    /lost verify acknowledgement/,
  );
  const afterLostVerify = await target.runtime.recoveryStatus();
  assert.equal(afterLostVerify.sealed, true);
  assert.deepEqual(
    await target.runtime.verifyRecovery(verify, target.apply),
    afterLostVerify,
  );
  await assert.rejects(
    target.runtime.verifyRecovery({
      ...verify,
      actor: { kind: "platform_admin", id: "different_admin" },
    }, target.apply),
    (error) => error.code === "realm_email_alias_recovery_idempotency_conflict",
  );

  const receipt = (await target.storage.get(REALM_EMAIL_ALIAS_RECOVERY_KEY))
    .last_action;
  assert.equal(receipt.action, "verify");
  assert.equal(receipt.before.phase, "replayed");
  assert.equal(receipt.after.phase, "sealed");
});

test("recovery permanently fences R2 sequence gaps and hash corruption", async () => {
  for (const mode of ["gap", "hash"]) {
    const source = await bootstrapSource();
    const firstKey = realmEmailAliasJournalEntryKey(STREAM, 1);
    if (mode === "gap") {
      source.bucket.values.delete(firstKey);
    } else {
      const entry = JSON.parse(new TextDecoder().decode(
        source.bucket.values.get(firstKey),
      ));
      entry.target = "tampered";
      source.bucket.values.set(firstKey, new TextEncoder().encode(JSON.stringify(entry)));
    }
    const target = fixture({ bucket: source.bucket });
    await target.runtime.startRecovery(recoveryInput({
      sequence: source.head.sequence,
      hash: source.head.hash,
    }), target.apply);
    await assert.rejects(
      target.runtime.advanceRecovery({
        actor: { kind: "platform_admin", id: "adm_test" },
        recovery_id: RECOVERY,
        idempotency_key: `advance-${mode}`,
      }, target.apply),
      (error) => mode === "gap"
        ? error.code === "realm_email_alias_journal_gap"
        : error.code === "realm_email_alias_journal_hash_mismatch",
    );
    const failed = await target.runtime.recoveryStatus();
    assert.equal(failed.failed, true);
  }
});

test("recovery detects a validly hashed checkpoint with the wrong authority digest", async () => {
  const source = await bootstrapSource();
  const checkpointKey = realmEmailAliasJournalEntryKey(
    STREAM,
    source.head.sequence,
  );
  const checkpoint = JSON.parse(new TextDecoder().decode(
    source.bucket.values.get(checkpointKey),
  ));
  const { entry_hash: _oldHash, ...unsigned } = checkpoint;
  const conflicting = await buildRealmEmailAliasJournalEntry({
    ...unsigned,
    metadata: {
      ...unsigned.metadata,
      authority_digest: "f".repeat(64),
    },
  });
  source.bucket.values.set(checkpointKey, conflicting.bytes);

  const target = fixture({ bucket: source.bucket });
  await target.runtime.startRecovery(recoveryInput({
    sequence: conflicting.entry.sequence,
    hash: conflicting.entry.entry_hash,
  }), target.apply);
  await target.runtime.advanceRecovery({
    actor: { kind: "platform_admin", id: "adm_test" },
    recovery_id: RECOVERY,
    idempotency_key: "advance-digest-1",
  }, target.apply);
  await assert.rejects(
    target.runtime.advanceRecovery({
      actor: { kind: "platform_admin", id: "adm_test" },
      recovery_id: RECOVERY,
      idempotency_key: "advance-digest-2",
    }, target.apply),
    (error) => error.code === "realm_email_alias_recovery_digest_mismatch",
  );
  assert.equal((await target.runtime.recoveryStatus()).failed, true);
});
