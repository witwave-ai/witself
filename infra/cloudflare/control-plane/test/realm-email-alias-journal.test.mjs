import assert from "node:assert/strict";
import test from "node:test";

import {
  REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT,
  REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_GENESIS_HASH,
  REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_MAX_CHANGES,
  RealmEmailAliasJournalError,
  appendRealmEmailAliasJournalEntry,
  applyRealmEmailAliasAuthorityAfterImage,
  buildRealmEmailAliasBootstrapPage,
  buildRealmEmailAliasJournalEntry,
  canonicalJSONString,
  classifyRealmEmailAliasStorageKey,
  isRealmEmailAliasAuthorityKey,
  isRealmEmailAliasDerivedKey,
  realmEmailAliasAuthorityStateDigest,
  realmEmailAliasJournalEntryKey,
  rebuildRealmEmailAliasDerivedState,
  replayRealmEmailAliasJournalPage,
  sha256Hex,
  validateRealmEmailAliasAuthorityAfterImage,
  validateRealmEmailAliasJournalEntry,
  validateRealmEmailAliasRecoveredState,
} from "../src/realm-email-alias-journal.mjs";
import {
  buildRealmEmailAliasClaimProof,
  realmEmailAliasClaimRouteFingerprint,
} from "../src/agent-email-custom-domain-route-contract.mjs";
import {
  AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
} from "../src/agent-email-operations-lease.mjs";

const STREAM = "reaj_aaaaaaaaaaaaaaaa";
const ACCOUNT = "acct_alias";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const REQUEST = "earq_aaaaaaaaaaaaaaaa";
const CLAIM = "era_aaaaaaaaaaaaaaaa";
const DOMAIN = "agent-mail.witwave.ai";
const NOW = "2026-08-02T20:00:00.000Z";

function fixtureState() {
  const reservation = {
    name: "witself",
    skeleton: "witseif",
    category: "platform_brand",
    reason: "initial protected name",
    version: 1,
    policy_version: 1,
    enabled: true,
    internal_assignable: true,
    created_at: NOW,
    updated_at: NOW,
    created_by: "system:seed",
    updated_by: "system:seed",
    retired_at: null,
    claim_conflict: null,
  };
  const request = {
    id: REQUEST,
    alias: "acme",
    domain: DOMAIN,
    skeleton: "acme",
    account_id: ACCOUNT,
    realm_id: REALM,
    status: "pending_review",
    requested_by: "opr_alias",
    requested_at: NOW,
    updated_at: NOW,
    decision: null,
  };
  const claim = {
    claim_id: CLAIM,
    alias: "acme",
    domain: DOMAIN,
    skeleton: "acme",
    account_id: ACCOUNT,
    realm_id: REALM,
    request_id: REQUEST,
    assignment_kind: null,
    assignment_revision: 0,
    admin_suspended: false,
    plan_suspended: false,
    operational_gate_suspended: false,
    plan_grace_until: null,
    created_at: NOW,
    updated_at: NOW,
    retired_at: null,
  };
  return new Map([
    ["meta", {
      schema_version: "witself.realm-email-alias.v1",
      seeded: true,
      pending_counter_schema_version: 1,
      pending_counter_state: "ready",
      registry_revision: 2,
      reserved_policy_version: 1,
      audit_sequence: 2,
      created_at: NOW,
      updated_at: NOW,
    }],
    ["audit:000000000001", {
      sequence: 1,
      registry_revision: 1,
      occurred_at: NOW,
      actor_kind: "system",
      actor_id: "seed",
      action: "reserved.seeded",
      target: "reserved-policy",
      metadata: { count: 1, phase: "committed" },
    }],
    ["audit:000000000002", {
      sequence: 2,
      registry_revision: 2,
      occurred_at: NOW,
      actor_kind: "account_operator",
      actor_id: "opr_alias",
      action: "alias.requested",
      target: "acme",
      metadata: { phase: "committed" },
    }],
    ["reserved:witself", reservation],
    ["reserved-history:witself:00000001", reservation],
    [`request:${REQUEST}`, request],
    ["claim:acme", claim],
    ["idem:request:acme:create-acme", {
      fingerprint: "request-acme",
      status: 202,
      body: { request },
    }],
  ]);
}

function journalInput(overrides = {}) {
  return {
    stream_id: STREAM,
    kind: "mutation",
    authority_epoch: 1,
    sequence: 1,
    previous_hash: REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_GENESIS_HASH,
    registry_revision: 2,
    audit_sequence: 2,
    occurred_at: NOW,
    operation_id: "request:create-acme",
    operation_fingerprint: "a".repeat(64),
    actor: { kind: "account_operator", id: "opr_alias" },
    action: "alias.requested",
    target: "acme",
    metadata: { account_id: ACCOUNT },
    after_image: {
      puts: [...fixtureState()].map(([key, value]) => ({ key, value })),
      deletes: [],
    },
    ...overrides,
  };
}

class R2Object {
  constructor(key, bytes) {
    this.key = key;
    this.bytes = new Uint8Array(bytes);
    this.etag = `etag-${bytes.byteLength}`;
  }

  async arrayBuffer() {
    return this.bytes.slice().buffer;
  }
}

class ConditionalR2 {
  constructor() {
    this.values = new Map();
    this.puts = [];
    this.throwBeforePut = false;
    this.throwAfterPut = false;
  }

  async put(key, bytes, options = {}) {
    const onlyIf = options.onlyIf;
    const ifNoneMatch = onlyIf instanceof Headers
      ? onlyIf.get("If-None-Match")
      : null;
    this.puts.push({ key, bytes: new Uint8Array(bytes), options, ifNoneMatch });
    if (this.throwBeforePut) {
      this.throwBeforePut = false;
      throw new Error("simulated unavailable R2");
    }
    if (ifNoneMatch !== "*") throw new Error("missing create-only condition");
    if (this.values.has(key)) return null;
    const object = new R2Object(key, bytes);
    this.values.set(key, object);
    if (this.throwAfterPut) {
      this.throwAfterPut = false;
      throw new Error("simulated lost R2 acknowledgement");
    }
    return object;
  }

  async get(key) {
    return this.values.get(key) ?? null;
  }
}

test("restricted canonical JSON is stable and refuses ambiguous values", () => {
  assert.equal(
    canonicalJSONString({ z: 1, a: { d: 4, c: [3, 2] } }),
    '{"a":{"c":[3,2],"d":4},"z":1}',
  );
  assert.equal(canonicalJSONString({ b: 2, a: 1 }),
    canonicalJSONString({ a: 1, b: 2 }));
  assert.throws(() => canonicalJSONString({ value: 1.5 }),
    /safe integers/);
  assert.throws(() => canonicalJSONString({ value: -0 }), /negative zero/);
  assert.throws(() => canonicalJSONString({ value: undefined }),
    /unsupported value/);
  assert.throws(() => canonicalJSONString(new Date()), /plain prototype/);
  const sparse = [];
  sparse[1] = "value";
  assert.throws(() => canonicalJSONString(sparse), /must not contain holes/);
  const cyclic = {};
  cyclic.self = cyclic;
  assert.throws(() => canonicalJSONString(cyclic), /must not contain cycles/);
});

test("SHA-256 and journal object keys are deterministic", async () => {
  assert.equal(
    await sha256Hex("abc"),
    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
  );
  assert.equal(
    realmEmailAliasJournalEntryKey(STREAM, 42),
    "realm-email-alias-authority/v1/streams/" +
      `${STREAM}/entries/00000000000000000042.json`,
  );
  assert.throws(() => realmEmailAliasJournalEntryKey("global", 1),
    /stream_id/);
  assert.throws(() => realmEmailAliasJournalEntryKey(STREAM, 0),
    /positive safe integer/);
});

test("storage key classification separates authority, derived, and local state", () => {
  for (const key of [
    "meta",
    "claim:acme",
    `request:${REQUEST}`,
    "reserved:witself",
    "reserved-history:witself:00000001",
    "audit:000000000001",
    "idem:request:acme:key",
    `plan-fence:${ACCOUNT}`,
    `plan-intent:${ACCOUNT}`,
    "projection-intent:acme",
    `lifecycle-fence:${ACCOUNT}`,
    `lifecycle-intent:${ACCOUNT}`,
    `realm-close-fence:${ACCOUNT}:${REALM}`,
    `realm-close-intent:${ACCOUNT}:${REALM}`,
    `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`,
    `route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`,
  ]) {
    assert.equal(classifyRealmEmailAliasStorageKey(key), "authority", key);
    assert.equal(isRealmEmailAliasAuthorityKey(key), true, key);
  }
  for (const key of [
    "pending-counter-migration",
    "claim-skeleton:acme",
    `account-claim:${ACCOUNT}:${REALM}:${NOW}:acme`,
    `account-request:${ACCOUNT}:${REALM}:${REQUEST}`,
    "claim-usage-member:era_aaaaaaaaaaaaaaaa",
    "approval-due:0000000000000001:acme",
    "reserved-skeleton:witself",
    "canonical-inventory",
    `realm-canonical:${ACCOUNT}:${REALM}:${DOMAIN}:aaaaaaaaaaaaaaaa`,
    `realm-close-due:0000000000000001:${ACCOUNT}:${REALM}`,
  ]) {
    assert.equal(classifyRealmEmailAliasStorageKey(key), "derived", key);
    assert.equal(isRealmEmailAliasDerivedKey(key), true, key);
  }
  assert.equal(
    classifyRealmEmailAliasStorageKey("realm-email-alias-journal-pending"),
    "journal_local",
  );
  assert.equal(
    classifyRealmEmailAliasStorageKey(
      AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
    ),
    "journal_local",
  );
  assert.equal(classifyRealmEmailAliasStorageKey("mystery:key"), "unknown");
});

test("after-images normalize order and reject derived, duplicate, or permanent deletes", () => {
  const normalized = validateRealmEmailAliasAuthorityAfterImage({
    puts: [
      { key: "request:z", value: { b: 2, a: 1 } },
      { key: "claim:a", value: { value: 1 } },
    ],
    deletes: ["projection-intent:z", "plan-intent:acct"],
  });
  assert.deepEqual(normalized.puts.map(({ key }) => key), ["claim:a", "request:z"]);
  assert.deepEqual(normalized.deletes, ["plan-intent:acct", "projection-intent:z"]);
  assert.throws(() => validateRealmEmailAliasAuthorityAfterImage({
    puts: [{ key: "claim-skeleton:a", value: "a" }],
    deletes: [],
  }), /not canonical authority/);
  assert.throws(() => validateRealmEmailAliasAuthorityAfterImage({
    puts: [{ key: "claim:a", value: {} }],
    deletes: ["claim:a"],
  }), /cannot delete canonical authority|duplicate/);
  assert.throws(() => validateRealmEmailAliasAuthorityAfterImage({
    puts: [],
    deletes: ["audit:000000000001"],
  }), /cannot delete/);
  const tooMany = Array.from(
    { length: REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_MAX_CHANGES + 1 },
    (_, index) => ({ key: `idem:test:${index}`, value: { index } }),
  );
  assert.throws(() => validateRealmEmailAliasAuthorityAfterImage({
    puts: tooMany,
    deletes: [],
  }), /bounded change limit/);
});

test("journal entry construction is canonical and validation detects tampering", async () => {
  const built = await buildRealmEmailAliasJournalEntry(journalInput());
  assert.equal(built.hash, built.entry.entry_hash);
  assert.equal(built.key, realmEmailAliasJournalEntryKey(STREAM, 1));
  assert.deepEqual(
    built.entry.after_image.puts.map(({ key }) => key),
    [...fixtureState().keys()].sort(),
  );
  assert.deepEqual(
    await validateRealmEmailAliasJournalEntry(built.entry, {
      stream_id: STREAM,
      sequence: 1,
      previous_hash: REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_GENESIS_HASH,
      authority_epoch: 1,
    }),
    built,
  );
  const tampered = structuredClone(built.entry);
  tampered.target = "other";
  await assert.rejects(
    validateRealmEmailAliasJournalEntry(tampered),
    (error) => error.code === "realm_email_alias_journal_hash_mismatch",
  );
  await assert.rejects(
    validateRealmEmailAliasJournalEntry(built.entry, { sequence: 2 }),
    (error) => error.code === "realm_email_alias_journal_fence_mismatch",
  );
});

test("R2 append is create-only and byte-identical retry is idempotent", async () => {
  const bucket = new ConditionalR2();
  const built = await buildRealmEmailAliasJournalEntry(journalInput());
  const created = await appendRealmEmailAliasJournalEntry(bucket, built);
  assert.equal(created.created, true);
  assert.equal(created.replayed, false);
  assert.equal(bucket.puts[0].ifNoneMatch, "*");
  assert.equal(bucket.puts[0].options.httpMetadata.contentType, "application/json");
  const replayed = await appendRealmEmailAliasJournalEntry(bucket, built);
  assert.equal(replayed.created, false);
  assert.equal(replayed.replayed, true);
  assert.equal(bucket.values.size, 1);
});

test("lost append acknowledgement heals by exact read and conflicting bytes fence", async () => {
  const bucket = new ConditionalR2();
  bucket.throwAfterPut = true;
  const built = await buildRealmEmailAliasJournalEntry(journalInput());
  const replayed = await appendRealmEmailAliasJournalEntry(bucket, built);
  assert.equal(replayed.created, false);
  assert.equal(replayed.replayed, true);

  const conflicting = await buildRealmEmailAliasJournalEntry(journalInput({
    target: "different",
  }));
  await assert.rejects(
    appendRealmEmailAliasJournalEntry(bucket, conflicting),
    (error) => error instanceof RealmEmailAliasJournalError &&
      error.code === "realm_email_alias_journal_fork_detected",
  );
});

test("an unavailable R2 append fails closed", async () => {
  const bucket = new ConditionalR2();
  bucket.throwBeforePut = true;
  const built = await buildRealmEmailAliasJournalEntry(journalInput());
  await assert.rejects(
    appendRealmEmailAliasJournalEntry(bucket, built),
    (error) => error.code === "realm_email_alias_journal_unavailable",
  );
  assert.equal(bucket.values.size, 0);
  await assert.rejects(
    appendRealmEmailAliasJournalEntry(null, built),
    (error) => error.code === "realm_email_alias_journal_unavailable",
  );
});

test("bounded bootstrap pages are stable and cursor-exclusive", () => {
  const state = new Map();
  for (let index = 0; index < 5; index += 1) {
    state.set(`idem:test:${index}`, { index });
  }
  const first = buildRealmEmailAliasBootstrapPage(state, { limit: 2 });
  assert.equal(first.count, 2);
  assert.equal(first.complete, false);
  assert.equal(first.next_cursor, "idem:test:1");
  const second = buildRealmEmailAliasBootstrapPage(state, {
    limit: 2,
    cursor: first.next_cursor,
  });
  assert.deepEqual(second.after_image.puts.map(({ key }) => key), [
    "idem:test:2",
    "idem:test:3",
  ]);
  const final = buildRealmEmailAliasBootstrapPage(state, {
    limit: 2,
    cursor: second.next_cursor,
  });
  assert.equal(final.complete, true);
  assert.equal(final.next_cursor, null);
  assert.equal(final.count, 1);
  assert.throws(() => buildRealmEmailAliasBootstrapPage(state, {
    limit: REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT + 1,
  }), /limit is invalid/);
});

test("replay requires a contiguous hash chain and bounded pages", async () => {
  const first = await buildRealmEmailAliasJournalEntry(journalInput());
  const nextMeta = {
    ...fixtureState().get("meta"),
    registry_revision: 3,
    audit_sequence: 3,
    updated_at: "2026-08-02T20:01:00.000Z",
  };
  const secondAudit = {
    sequence: 3,
    registry_revision: 3,
    occurred_at: "2026-08-02T20:01:00.000Z",
    actor_kind: "platform_admin",
    actor_id: "adm_alias",
    action: "alias.suspended",
    target: "acme",
    metadata: { phase: "committed" },
  };
  const second = await buildRealmEmailAliasJournalEntry({
    ...journalInput(),
    sequence: 2,
    previous_hash: first.hash,
    registry_revision: 3,
    audit_sequence: 3,
    occurred_at: "2026-08-02T20:01:00.000Z",
    operation_id: "alias:suspend-acme",
    operation_fingerprint: "b".repeat(64),
    actor: { kind: "platform_admin", id: "adm_alias" },
    action: "alias.suspended",
    after_image: {
      puts: [
        { key: "meta", value: nextMeta },
        { key: "audit:000000000003", value: secondAudit },
      ],
      deletes: [],
    },
  });
  const replayed = await replayRealmEmailAliasJournalPage(
    [first.entry, second.entry],
    { stream_id: STREAM },
  );
  assert.equal(replayed.applied, 2);
  assert.equal(replayed.head.sequence, 2);
  assert.equal(replayed.head.hash, second.hash);
  assert.equal(replayed.state.get("meta").registry_revision, 3);

  const broken = structuredClone(second.entry);
  broken.previous_hash = "f".repeat(64);
  broken.entry_hash = (await buildRealmEmailAliasJournalEntry({
    ...broken,
    entry_hash: undefined,
  }).catch(() => null))?.hash ?? broken.entry_hash;
  await assert.rejects(
    replayRealmEmailAliasJournalPage([first.entry, broken], {
      stream_id: STREAM,
    }),
    (error) => [
      "realm_email_alias_journal_fence_mismatch",
      "realm_email_alias_journal_hash_mismatch",
    ].includes(error.code),
  );
  await assert.rejects(
    replayRealmEmailAliasJournalPage(
      Array(REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT + 1)
        .fill(first.entry),
      { stream_id: STREAM },
    ),
    /bounded entry limit/,
  );
});

test("authority state digest is independent of map insertion order and exact", async () => {
  const state = fixtureState();
  const reversed = new Map([...state].reverse());
  assert.equal(
    await realmEmailAliasAuthorityStateDigest(state),
    await realmEmailAliasAuthorityStateDigest(reversed),
  );
  const changed = new Map(reversed);
  changed.set("idem:extra:key", { status: 200 });
  assert.notEqual(
    await realmEmailAliasAuthorityStateDigest(state),
    await realmEmailAliasAuthorityStateDigest(changed),
  );
  changed.set("claim-skeleton:acme", "acme");
  await assert.rejects(
    realmEmailAliasAuthorityStateDigest(changed),
    /non-authority key/,
  );
});

test("recovery validates graph, collisions, history, audit gaps, and exact fences", () => {
  const state = fixtureState();
  assert.deepEqual(validateRealmEmailAliasRecoveredState(state, {
    expected_registry_revision: 2,
    expected_audit_sequence: 2,
    expected_reserved_policy_version: 1,
  }), {
    registry_revision: 2,
    audit_sequence: 2,
    reserved_policy_version: 1,
    claims: 1,
    requests: 1,
    reserved_names: 1,
    canonical_routes: 0,
    authority_keys: state.size,
  });

  const collision = new Map(state);
  collision.set("claim:a-cme", {
    ...state.get("claim:acme"),
    alias: "a-cme",
    skeleton: "acme",
    claim_id: "era_bbbbbbbbbbbbbbbb",
    request_id: null,
  });
  assert.throws(() => validateRealmEmailAliasRecoveredState(collision),
    (error) => error.code === "realm_email_alias_recovery_collision");

  const missingHistory = new Map(state);
  missingHistory.delete("reserved-history:witself:00000001");
  assert.throws(() => validateRealmEmailAliasRecoveredState(missingHistory),
    /history is incomplete/);

  const auditGap = new Map(state);
  auditGap.delete("audit:000000000001");
  assert.throws(() => validateRealmEmailAliasRecoveredState(auditGap),
    /audit sequence has a gap/);

  assert.throws(() => validateRealmEmailAliasRecoveredState(state, {
    expected_registry_revision: 3,
  }), (error) => error.code === "realm_email_alias_journal_fence_mismatch");
});

test("recovery preserves a bounded dual-domain realm-close fence exactly", () => {
  const state = fixtureState();
  const domains = ["witmail.net", DOMAIN];
  for (const [index, domain] of domains.entries()) {
    state.set(`canonical:${domain}:aaaaaaaaaaaaaaaa`, {
      domain,
      account_id: ACCOUNT,
      realm_id: REALM,
      realm_label: "aaaaaaaaaaaaaaaa",
      state: "retired",
      controller_revision: index + 3,
      updated_at: NOW,
    });
  }
  state.set(`realm-close-fence:${ACCOUNT}:${REALM}`, {
    account_id: ACCOUNT,
    realm_id: REALM,
    operation_id: "close-dual-domain",
    cell_generation: 2,
    controller_revision: 3,
    canonical_revisions: [
      { domain: domains[0], controller_revision: 3 },
      { domain: domains[1], controller_revision: 4 },
    ],
    completed_at: NOW,
  });
  assert.equal(
    validateRealmEmailAliasRecoveredState(state).canonical_routes,
    2,
  );

  const malformed = new Map(state);
  malformed.set(`realm-close-fence:${ACCOUNT}:${REALM}`, {
    ...state.get(`realm-close-fence:${ACCOUNT}:${REALM}`),
    canonical_revisions: [
      { domain: domains[0], controller_revision: 4 },
      { domain: domains[1], controller_revision: 4 },
    ],
  });
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(malformed),
    /realm-close authority is invalid/,
  );

  const missing = new Map(state);
  missing.delete(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`);
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(missing),
    /canonical route is missing/,
  );

  const unrelated = new Map(state);
  unrelated.set(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`, {
    ...state.get(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`),
    account_id: "acct_unrelated",
  });
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(unrelated),
    /canonical ownership is inconsistent/,
  );

  const revisionMismatch = new Map(state);
  revisionMismatch.set(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`, {
    ...state.get(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`),
    controller_revision: 5,
  });
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(revisionMismatch),
    /canonical revision is inconsistent/,
  );

  const invalidTimestamp = new Map(state);
  invalidTimestamp.set(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`, {
    ...state.get(`canonical:${domains[1]}:aaaaaaaaaaaaaaaa`),
    updated_at: "invalid",
  });
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(invalidTimestamp),
    /canonical route is invalid/,
  );

  const extraLive = new Map(state);
  extraLive.set("canonical:mail-archive.example:aaaaaaaaaaaaaaaa", {
    domain: "mail-archive.example",
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: "aaaaaaaaaaaaaaaa",
    state: "suspended",
    controller_revision: 1,
    updated_at: NOW,
  });
  assert.throws(
    () => validateRealmEmailAliasRecoveredState(extraLive),
    /nonretired canonical route/,
  );
});

test("after-image replay prevents tombstone resurrection and revision regression", () => {
  const state = fixtureState();
  const retired = {
    ...state.get("claim:acme"),
    assignment_kind: "customer",
    assignment_revision: 4,
    retired_at: NOW,
  };
  const withTombstone = applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{ key: "claim:acme", value: retired }],
    deletes: [],
  });
  assert.throws(() => applyRealmEmailAliasAuthorityAfterImage(withTombstone, {
    puts: [{
      key: "claim:acme",
      value: { ...retired, retired_at: null, assignment_revision: 5 },
    }],
    deletes: [],
  }), (error) => error.code ===
    "realm_email_alias_recovery_tombstone_resurrection");
  assert.throws(() => applyRealmEmailAliasAuthorityAfterImage(withTombstone, {
    puts: [{
      key: "claim:acme",
      value: { ...retired, assignment_revision: 3 },
    }],
    deletes: [],
  }), (error) => error.code ===
    "realm_email_alias_recovery_revision_regression");
  assert.throws(() => applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{
      key: "audit:000000000001",
      value: { ...state.get("audit:000000000001"), action: "changed" },
    }],
    deletes: [],
  }), /append-only authority key/);
});

test("same-revision canonical replay permits only freshness metadata changes", () => {
  const key = `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`;
  const canonical = {
    domain: DOMAIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: "aaaaaaaaaaaaaaaa",
    state: "applied",
    cell_state: "live",
    cell_generation: 1,
    cell_operation_id: null,
    cell_audience: "cell-one",
    ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
    controller_revision: 7,
    updated_at: NOW,
  };
  const state = new Map([[key, canonical]]);
  const refreshedAt = "2026-08-02T20:00:01.000Z";
  const refreshed = applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{ key, value: { ...canonical, updated_at: refreshedAt } }],
    deletes: [],
  });
  assert.equal(refreshed.get(key).updated_at, refreshedAt);
  assert.equal(refreshed.get(key).controller_revision, 7);

  for (const desired of [
    { ...canonical, updated_at: "invalid" },
    {
      ...canonical,
      cell_audience: "cell-two",
      updated_at: refreshedAt,
    },
  ]) {
    assert.throws(
      () => applyRealmEmailAliasAuthorityAfterImage(state, {
        puts: [{ key, value: desired }],
        deletes: [],
      }),
      (error) => error.code ===
        "realm_email_alias_recovery_revision_regression",
    );
  }
  for (const updatedAt of [
    NOW,
    "2026-08-02T19:59:59.999Z",
  ]) {
    assert.doesNotThrow(() => applyRealmEmailAliasAuthorityAfterImage(state, {
      puts: [{ key, value: { ...canonical, updated_at: updatedAt } }],
      deletes: [],
    }));
  }
  const repaired = applyRealmEmailAliasAuthorityAfterImage(
    new Map([[key, { ...canonical, updated_at: "invalid" }]]),
    {
      puts: [{ key, value: { ...canonical, updated_at: refreshedAt } }],
      deletes: [],
    },
  );
  assert.equal(repaired.get(key).updated_at, refreshedAt);

  const {
    cell_audience: _cellAudience,
    ingest_url: _ingestURL,
    ...withoutDestination
  } = canonical;
  const semanticChange = {
    ...withoutDestination,
    state: "suspended",
    suspension_disposition: "retry",
    controller_revision: 8,
    updated_at: refreshedAt,
  };
  const changed = applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{ key, value: semanticChange }],
    deletes: [],
  });
  assert.equal(changed.get(key).state, "suspended");
  assert.equal(changed.get(key).controller_revision, 8);
  assert.doesNotThrow(() => applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{
      key,
      value: {
        ...semanticChange,
        updated_at: "2026-08-02T19:59:59.999Z",
      },
    }],
    deletes: [],
  }));
});

test("custom-domain sync replay allows only fenced source and phase transitions", () => {
  const proof = buildRealmEmailAliasClaimProof({
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: "acme",
    realm_alias_claim_id: CLAIM,
    realm_alias_revision: 1,
    state: "applied",
    updated_at: NOW,
  });
  const key = `custom-domain-sync:${CLAIM}`;
  const enqueue = {
    schema_version: "witself.realm-email-alias-custom-domain-sync.v1",
    phase: "enqueue",
    claim_proof: proof,
    source_fingerprint: realmEmailAliasClaimRouteFingerprint(proof),
    failure_count: 0,
    retry_at_ms: 100,
    created_at: NOW,
    updated_at: NOW,
  };
  const state = new Map([[key, enqueue]]);
  const poll = {
    ...enqueue,
    phase: "poll",
    retry_at_ms: 101,
    updated_at: "2026-08-02T20:00:01.000Z",
  };
  const polling = applyRealmEmailAliasAuthorityAfterImage(state, {
    puts: [{ key, value: poll }],
    deletes: [],
  });
  assert.equal(polling.get(key).phase, "poll");

  const reenqueued = applyRealmEmailAliasAuthorityAfterImage(polling, {
    puts: [{
      key,
      value: {
        ...poll,
        phase: "enqueue",
        retry_at_ms: 102,
        updated_at: "2026-08-02T20:00:02.000Z",
      },
    }],
    deletes: [],
  });
  assert.equal(reenqueued.get(key).phase, "enqueue");

  const newerProof = buildRealmEmailAliasClaimProof({
    ...proof,
    realm_alias_revision: 2,
    state: "retired",
    updated_at: "2026-08-02T20:00:03.000Z",
  });
  const newer = {
    ...enqueue,
    claim_proof: newerProof,
    source_fingerprint: realmEmailAliasClaimRouteFingerprint(newerProof),
    retry_at_ms: 103,
    updated_at: "2026-08-02T20:00:03.000Z",
  };
  assert.doesNotThrow(() => applyRealmEmailAliasAuthorityAfterImage(
    reenqueued,
    { puts: [{ key, value: newer }], deletes: [] },
  ));

  for (const invalid of [
    {
      ...poll,
      claim_proof: { ...poll.claim_proof, updated_at: poll.updated_at },
      updated_at: "2026-08-02T20:00:02.000Z",
    },
    {
      ...poll,
      retry_at_ms: 99,
      updated_at: "2026-08-02T20:00:02.000Z",
    },
    {
      ...newer,
      phase: "poll",
    },
    {
      ...newer,
      failure_count: 1,
    },
  ]) {
    assert.throws(() => applyRealmEmailAliasAuthorityAfterImage(state, {
      puts: [{ key, value: invalid }],
      deletes: [],
    }), (error) => error.code ===
      "realm_email_alias_recovery_revision_regression");
  }
});

test("only the exact prepared-to-committed audit overwrite is replayable", () => {
  const state = fixtureState();
  const committed = state.get("audit:000000000002");
  const prepared = {
    ...committed,
    action: `${committed.action}.intent_recorded`,
    metadata: {
      phase: "prepared",
      requested_action: committed.action,
    },
  };
  const withPrepared = new Map(state);
  withPrepared.set("audit:000000000002", prepared);
  const completed = applyRealmEmailAliasAuthorityAfterImage(withPrepared, {
    puts: [{ key: "audit:000000000002", value: committed }],
    deletes: [],
  });
  assert.deepEqual(completed.get("audit:000000000002"), committed);

  for (const changed of [
    { ...committed, actor_id: "different" },
    { ...committed, registry_revision: 3 },
    { ...committed, metadata: { phase: "committed", extra: true } },
  ]) {
    assert.throws(() => applyRealmEmailAliasAuthorityAfterImage(withPrepared, {
      puts: [{ key: "audit:000000000002", value: changed }],
      deletes: [],
    }), /append-only authority key/);
  }
});

test("derived-state rebuild recreates indexes, counters, and due work only", () => {
  const state = fixtureState();
  const claim = {
    ...state.get("claim:acme"),
    assignment_kind: "customer",
    assignment_revision: 1,
    customer_activation_intent: true,
    approval_retry_at_ms: 42,
  };
  state.set("claim:acme", claim);
  state.set("projection-intent:acme", {
    alias: "acme",
    retry_at_ms: 43,
  });
  state.set(`plan-intent:${ACCOUNT}`, {
    account_id: ACCOUNT,
    plan_revision: 1,
    plan_snapshot_hash: "a".repeat(64),
    feature_enabled: true,
    alias_limit: 1,
    activation_enabled: true,
    state: "cell_committed",
    claim_cursor: null,
    realm_positions: {},
    gate_phase: "claims",
    gate_claim_cursor: null,
    gate_canonical_cursor: null,
    operational_gate_complete: true,
    retry_at_ms: 44,
    created_at: NOW,
    updated_at: NOW,
  });
  state.set(`lifecycle-intent:${ACCOUNT}`, {
    account_id: ACCOUNT,
    operation_id: "archive-account",
    epoch: 1,
    action: "suspend",
    activation_enabled: true,
    phase: "claims",
    claim_cursor: null,
    canonical_cursor: null,
    failure_count: 0,
    retry_at_ms: 45,
    created_at: NOW,
    updated_at: NOW,
  });
  state.set(`route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`, {
    domain: DOMAIN,
    realm_label: "aaaaaaaaaaaaaaaa",
    account_id: ACCOUNT,
    kind: "canonical",
    retry_at_ms: 46,
  });
  state.set(`canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`, {
    domain: DOMAIN,
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: "aaaaaaaaaaaaaaaa",
    state: "suspended",
    controller_revision: 2,
    updated_at: NOW,
  });
  state.set(`realm-close-intent:${ACCOUNT}:${REALM}`, {
    schema_version: "witself.realm-email-alias.v1",
    account_id: ACCOUNT,
    realm_id: REALM,
    domain: DOMAIN,
    actor: { kind: "account_operator", id: "opr_alias" },
    idempotency_key: "close-realm",
    fingerprint: "close-realm",
    phase: "scan_aliases",
    alias_cursor: null,
    failure_count: 0,
    retry_at_ms: 47,
    created_at: NOW,
    updated_at: NOW,
  });
  const derived = rebuildRealmEmailAliasDerivedState(state, {
    retry_at_ms: 50,
    updated_at: NOW,
  });
  for (const key of derived.keys()) {
    assert.equal(isRealmEmailAliasDerivedKey(key), true, key);
  }
  assert.equal(derived.get("claim-skeleton:acme"), "acme");
  assert.equal(
    derived.get(`account-request:${ACCOUNT}:${REALM}:${REQUEST}`),
    REQUEST,
  );
  assert.equal(
    derived.get(`claim-usage-account:${ACCOUNT}`).open_requests,
    1,
  );
  assert.deepEqual(
    {
      open_requests:
        derived.get(`claim-usage-realm:${ACCOUNT}:${REALM}`).open_requests,
      pending_review:
        derived.get(`claim-usage-realm:${ACCOUNT}:${REALM}`).pending_review,
      provisioning:
        derived.get(`claim-usage-realm:${ACCOUNT}:${REALM}`).provisioning,
      customer_allocated:
        derived.get(`claim-usage-realm:${ACCOUNT}:${REALM}`).customer_allocated,
    },
    {
      open_requests: 1,
      pending_review: 0,
      provisioning: 1,
      customer_allocated: 1,
    },
  );
  assert.equal(derived.get("approval-due:0000000000000042:acme"), "acme");
  assert.equal(derived.get("projection-due:0000000000000043:acme"), "acme");
  assert.equal(derived.get(`plan-due:0000000000000044:${ACCOUNT}`), ACCOUNT);
  assert.equal(
    derived.get(`lifecycle-due:0000000000000045:${ACCOUNT}`),
    ACCOUNT,
  );
  assert.equal(
    derived.get(`refresh-due:0000000000000046:${DOMAIN}:aaaaaaaaaaaaaaaa`),
    `route-refresh:${DOMAIN}:aaaaaaaaaaaaaaaa`,
  );
  assert.equal(
    derived.get(
      `realm-canonical:${ACCOUNT}:${REALM}:${DOMAIN}:aaaaaaaaaaaaaaaa`,
    ),
    `canonical:${DOMAIN}:aaaaaaaaaaaaaaaa`,
  );
  assert.equal(
    derived.get(`realm-close-due:0000000000000047:${ACCOUNT}:${REALM}`),
    `${ACCOUNT}:${REALM}`,
  );
  assert.equal(derived.has("pending-counter-migration"), false);
});
