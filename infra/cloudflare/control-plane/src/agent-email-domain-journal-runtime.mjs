import {
  appendAgentEmailDomainJournalEntry,
  buildAgentEmailDomainJournalEntry,
  canonicalJSONBytes,
  canonicalJSONString,
  classifyAgentEmailDomainStorageKey,
  AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
  agentEmailDomainAuthorityStateDigest,
  agentEmailDomainJournalEntryKey,
  rebuildAgentEmailDomainDerivedState,
  AgentEmailDomainJournalError,
  replayAgentEmailDomainJournalPage,
  sha256Hex,
  validateAgentEmailDomainAuthorityAfterImage,
  validateAgentEmailDomainJournalEntry,
  validateAgentEmailDomainRecoveredState,
} from "./agent-email-domain-journal.mjs";

export const AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY =
  "agent-email-domain-journal-meta";
export const AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY =
  "agent-email-domain-journal-pending";
export const AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY =
  "agent-email-domain-journal-fork";
export const AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY =
  "agent-email-domain-journal:bootstrap";
export const AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY =
  "agent-email-domain-journal:capacity";
export const AGENT_EMAIL_DOMAIN_RECOVERY_KEY = "agent-email-domain-recovery";
const AGENT_EMAIL_DOMAIN_JOURNAL_MAINTENANCE_RECEIPT_PREFIX =
  "agent-email-domain-journal:maintenance-receipt:";

const LOCAL_SCHEMA = "witself.agent-email-domain-journal-local.v1";
const CAPACITY_SCHEMA =
  "witself.agent-email-domain-authority-capacity-local.v1";
const MAINTENANCE_SCHEMA = "witself.agent-email-domain-journal-maintenance.v1";
const CHECKPOINT_SCHEMA = "witself.agent-email-domain-authority-checkpoint.v1";
const LEGACY_RECOVERY_SCHEMA = "witself.agent-email-domain-recovery-local.v1";
const RECOVERY_SCHEMA = "witself.agent-email-domain-recovery-local.v2";
const STREAM_ID_PATTERN = /^aedj_[a-z2-7]{16,52}$/;
const RECOVERY_ID_PATTERN = /^aedrec_[a-z2-7]{16}$/;
const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const OBJECT_NAME_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const SHA256_PATTERN = /^[0-9a-f]{64}$/;
const VERIFICATION_WORK_KEY_PATTERN =
  /^verification-work:aedr_[a-z2-7]{16}$/;
const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";
const SNAPSHOT_STORAGE_PAGE_LIMIT = 100;
export const AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS = 10_000;
const MAX_AUTHORITY_KEYS = AGENT_EMAIL_DOMAIN_MAX_AUTHORITY_KEYS;
const NEAR_AUTHORITY_KEYS = MAX_AUTHORITY_KEYS -
  Math.floor(MAX_AUTHORITY_KEYS / 10);
const MAX_SCANNED_STORAGE_KEYS = 100_000;
const DERIVED_REBUILD_PAGE_LIMIT = 100;
const JOURNAL_ENTRY_MAX_BYTES = 512 * 1_024;
const RECOVERY_ACTION_FENCE_GENERATION_ATTEMPTS = 4;

const AUTHORITY_CAPACITY_PREFIXES = Object.freeze([
  ["audit:", "audit"],
  ["domain:", "domain"],
  ["idem:", "idempotency"],
  ["lifecycle-fence:", "lifecycle_fence"],
  ["lifecycle-intent:", "lifecycle_intent"],
  ["plan-fence:", "plan_fence"],
  ["plan-intent:", "plan_intent"],
  ["request:", "request"],
]);
const AUTHORITY_CAPACITY_CATEGORIES = Object.freeze([
  "meta",
  ...AUTHORITY_CAPACITY_PREFIXES.map(([, category]) => category),
]);

export class AgentEmailDomainJournalRuntimeError extends Error {
  constructor(message, code = "agent_email_domain_journal_unavailable") {
    super(message);
    this.name = "AgentEmailDomainJournalRuntimeError";
    this.code = code;
  }
}

function fail(message, code) {
  throw new AgentEmailDomainJournalRuntimeError(message, code);
}

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

function randomStreamID() {
  const bytes = new Uint8Array(26);
  crypto.getRandomValues(bytes);
  return `aedj_${[...bytes].map((byte) => BASE32[byte & 31]).join("")}`;
}

function randomRecoveryActionFence() {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

function journalRequired(env) {
  return String(env?.CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED ?? "") ===
    "true";
}

function configuredStreamID(env) {
  const value = String(
    env?.CP_AGENT_EMAIL_DOMAIN_AUTHORITY_STREAM_ID ?? "",
  ).trim();
  if (value === "") return null;
  if (!STREAM_ID_PATTERN.test(value)) {
    fail("agent email domain authority stream id is invalid",
      "agent_email_domain_journal_configuration_invalid");
  }
  return value;
}

function validHead(value) {
  return value?.schema_version === LOCAL_SCHEMA &&
    STREAM_ID_PATTERN.test(value.stream_id ?? "") &&
    Number.isSafeInteger(value.sequence) && value.sequence >= 0 &&
    typeof value.hash === "string" && /^[0-9a-f]{64}$/.test(value.hash) &&
    Number.isSafeInteger(value.authority_epoch) && value.authority_epoch >= 1 &&
    Number.isSafeInteger(value.registry_revision) &&
    value.registry_revision >= 0 &&
    Number.isSafeInteger(value.audit_sequence) && value.audit_sequence >= 0;
}

function emptyAuthorityBreakdown() {
  return Object.fromEntries(
    AUTHORITY_CAPACITY_CATEGORIES.map((category) => [category, 0]),
  );
}

function authorityCategory(key) {
  if (key === "meta") return "meta";
  const match = AUTHORITY_CAPACITY_PREFIXES.find(([prefix]) =>
    key.startsWith(prefix));
  if (match) return match[1];
  fail(`unclassified agent email domain authority key: ${key}`,
    "agent_email_domain_journal_unknown_storage_key");
}

function validAuthorityBreakdown(value, authorityKeys) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      Object.keys(value).length !== AUTHORITY_CAPACITY_CATEGORIES.length) {
    return false;
  }
  let total = 0;
  for (const category of AUTHORITY_CAPACITY_CATEGORIES) {
    const count = value[category];
    if (!Number.isSafeInteger(count) || count < 0) return false;
    total += count;
  }
  return Number.isSafeInteger(total) && total === authorityKeys;
}

function assertMaintenanceCapacity(maintenance) {
  if (!Number.isSafeInteger(maintenance?.authority_keys) ||
      maintenance.authority_keys < 0 ||
      !validAuthorityBreakdown(
        maintenance.authority_breakdown,
        maintenance.authority_keys,
      )) {
    fail("agent email domain journal maintenance capacity is invalid",
      "agent_email_domain_journal_maintenance_invalid");
  }
}

function capacityMatchesHead(value, head) {
  return validHead(head) && value?.schema_version === CAPACITY_SCHEMA &&
    value.stream_id === head.stream_id &&
    value.sequence === head.sequence && value.hash === head.hash &&
    value.authority_epoch === head.authority_epoch &&
    value.registry_revision === head.registry_revision &&
    value.audit_sequence === head.audit_sequence &&
    value.max_authority_keys === MAX_AUTHORITY_KEYS &&
    Number.isSafeInteger(value.authority_keys) &&
    value.authority_keys >= 0 && value.authority_keys <= MAX_AUTHORITY_KEYS &&
    validAuthorityBreakdown(value.breakdown, value.authority_keys);
}

function capacityForHead(head, authorityKeys, breakdown, updatedAt) {
  if (!validHead(head) || !Number.isSafeInteger(authorityKeys) ||
      authorityKeys < 0 || authorityKeys > MAX_AUTHORITY_KEYS ||
      !validAuthorityBreakdown(breakdown, authorityKeys)) {
    fail("agent email domain authority capacity invariant failed",
      "agent_email_domain_journal_capacity_invalid");
  }
  return {
    schema_version: CAPACITY_SCHEMA,
    stream_id: head.stream_id,
    sequence: head.sequence,
    hash: head.hash,
    authority_epoch: head.authority_epoch,
    registry_revision: head.registry_revision,
    audit_sequence: head.audit_sequence,
    authority_keys: authorityKeys,
    max_authority_keys: MAX_AUTHORITY_KEYS,
    breakdown: clone(breakdown),
    updated_at: updatedAt,
  };
}

function publicCapacity(value, head) {
  const ready = capacityMatchesHead(value, head);
  const used = ready ? value.authority_keys : null;
  return {
    ready,
    used,
    max: MAX_AUTHORITY_KEYS,
    remaining: ready ? MAX_AUTHORITY_KEYS - used : null,
    near_limit: ready ? used >= NEAR_AUTHORITY_KEYS : null,
    at_limit: ready ? used === MAX_AUTHORITY_KEYS : null,
    breakdown: ready ? clone(value.breakdown) : null,
  };
}

function authorityCapacityFromState(state) {
  const breakdown = emptyAuthorityBreakdown();
  let authorityKeys = 0;
  for (const key of state.keys()) {
    breakdown[authorityCategory(key)] += 1;
    authorityKeys += 1;
  }
  if (authorityKeys > MAX_AUTHORITY_KEYS) {
    fail("agent email domain authority exceeds 10000 keys",
      "agent_email_domain_journal_authority_limit_exceeded");
  }
  return { authority_keys: authorityKeys, breakdown };
}

function bytesEqual(left, right) {
  if (left.byteLength !== right.byteLength) return false;
  let different = 0;
  for (let index = 0; index < left.byteLength; index += 1) {
    different |= left[index] ^ right[index];
  }
  return different === 0;
}

function journalError(error) {
  if (error instanceof AgentEmailDomainJournalRuntimeError) return error;
  if (error instanceof AgentEmailDomainJournalError) {
    return new AgentEmailDomainJournalRuntimeError(error.message, error.code);
  }
  return new AgentEmailDomainJournalRuntimeError(
    "agent email domain authority journal is unavailable",
  );
}

function authorityAfterImage(entries, deletes) {
  const puts = [];
  const authorityDeletes = [];
  for (const [key, value] of entries) {
    const classification = classifyAgentEmailDomainStorageKey(key);
    if (classification === "authority") {
      puts.push({ key, value: clone(value) });
    } else if (classification === "journal_local") {
      fail(`journal-local agent email domain storage key cannot be committed: ${key}`,
        "agent_email_domain_journal_local_storage_key");
    } else if (classification === "unknown") {
      fail(`unclassified agent email domain storage key: ${key}`,
        "agent_email_domain_journal_unknown_storage_key");
    }
  }
  for (const key of deletes) {
    const classification = classifyAgentEmailDomainStorageKey(key);
    if (classification === "authority") {
      authorityDeletes.push(key);
    } else if (classification === "journal_local" &&
        VERIFICATION_WORK_KEY_PATTERN.test(key)) {
      // A verification observation is consumed by the authority transition
      // it fences. Keep that cleanup in the same Durable Object transaction,
      // but never place the operational work record in the R2 after-image.
      continue;
    } else if (classification === "journal_local") {
      fail(`journal-local agent email domain storage key cannot be committed: ${key}`,
        "agent_email_domain_journal_local_storage_key");
    } else if (classification === "unknown") {
      fail(`unclassified agent email domain storage key: ${key}`,
        "agent_email_domain_journal_unknown_storage_key");
    }
  }
  try {
    return validateAgentEmailDomainAuthorityAfterImage({
      puts,
      deletes: authorityDeletes,
    });
  } catch (error) {
    throw journalError(error);
  }
}

function maintenanceReceiptKey(idempotencyKey) {
  return `${AGENT_EMAIL_DOMAIN_JOURNAL_MAINTENANCE_RECEIPT_PREFIX}` +
    idempotencyKey;
}

function highestAudit(afterImage) {
  return afterImage.puts
    .filter(({ key, value }) => key.startsWith("audit:") && value)
    .sort((left, right) =>
      Number(right.key.slice("audit:".length)) -
      Number(left.key.slice("audit:".length)))[0]?.value ?? null;
}

function metaPut(afterImage) {
  return afterImage.puts.find(({ key }) => key === "meta")?.value ?? null;
}

function cleanOptions(_options) {
  // The custom-domain registry has no non-storage side channel. Persisting
  // caller-owned options would only create a second, unjournaled mutation
  // contract for recovery to reconstruct.
  return {};
}

function validActor(actor) {
  return actor?.kind === "platform_admin" &&
    typeof actor.id === "string" && actor.id.length >= 1 &&
    actor.id.length <= 128;
}

function maintenanceRequest(input) {
  if (!validActor(input?.actor) ||
      typeof input?.reason !== "string" || input.reason.trim().length < 3 ||
      input.reason.trim().length > 1_024 ||
      !IDEMPOTENCY_KEY_PATTERN.test(input?.idempotency_key ?? "")) {
    fail("agent email domain journal maintenance request is invalid",
      "agent_email_domain_journal_maintenance_invalid");
  }
  return {
    actor: { kind: input.actor.kind, id: input.actor.id },
    reason: input.reason.trim(),
    idempotency_key: input.idempotency_key,
  };
}

function exactExpectedHead(head) {
  return head && Number.isSafeInteger(head.sequence) && head.sequence >= 1 &&
    SHA256_PATTERN.test(head.hash ?? "");
}

function recoveryRequest(input) {
  if (!validActor(input?.actor) ||
      !RECOVERY_ID_PATTERN.test(input?.recovery_id ?? "") ||
      !STREAM_ID_PATTERN.test(input?.source_stream_id ?? "") ||
      !exactExpectedHead(input?.expected_head) ||
      !OBJECT_NAME_PATTERN.test(input?.active_object_name ?? "") ||
      !OBJECT_NAME_PATTERN.test(input?.target_object_name ?? "") ||
      input.target_object_name !== `recovery:${input.recovery_id}` ||
      input.target_object_name === input.active_object_name ||
      typeof input?.reason !== "string" || input.reason.trim().length < 3 ||
      input.reason.trim().length > 1_024 ||
      !IDEMPOTENCY_KEY_PATTERN.test(input?.idempotency_key ?? "")) {
    fail("agent email domain recovery request is invalid",
      "agent_email_domain_recovery_request_invalid");
  }
  return {
    actor: { kind: input.actor.kind, id: input.actor.id },
    recovery_id: input.recovery_id,
    source_stream_id: input.source_stream_id,
    expected_head: {
      sequence: input.expected_head.sequence,
      hash: input.expected_head.hash,
    },
    active_object_name: input.active_object_name,
    target_object_name: input.target_object_name,
    reason: input.reason.trim(),
    idempotency_key: input.idempotency_key,
  };
}

function mapRows(value) {
  if (value instanceof Map) return [...value];
  if (value && typeof value[Symbol.iterator] === "function") return [...value];
  fail("agent email domain durable storage returned an invalid page",
    "agent_email_domain_journal_storage_invalid");
}

async function storagePage(storage, cursor = null, limit = SNAPSHOT_STORAGE_PAGE_LIMIT) {
  if (typeof storage?.list !== "function") {
    fail("agent email domain durable storage cannot be enumerated",
      "agent_email_domain_journal_storage_invalid");
  }
  const listed = await storage.list({
    ...(cursor === null ? {} : { startAfter: cursor }),
    limit: limit + 1,
  });
  const rows = mapRows(listed)
    .filter(([key]) => cursor === null || key > cursor)
    .sort(([left], [right]) => left.localeCompare(right));
  const selected = rows.slice(0, limit);
  return {
    rows: selected,
    complete: rows.length <= limit,
    next_cursor: selected.length === 0 ? cursor : selected.at(-1)[0],
  };
}

async function initialRowsHash() {
  return sha256Hex(canonicalJSONBytes({
    schema_version: "witself.agent-email-domain-authority-state.v1",
    rows: 0,
  }));
}

async function appendDigestRow(rowsHash, rowNumber, key, value) {
  return sha256Hex(canonicalJSONBytes({
    schema_version: "witself.agent-email-domain-authority-state-row.v1",
    previous_hash: rowsHash,
    row_number: rowNumber,
    key,
    value,
  }));
}

async function finishRowsDigest(rowsHash, rows) {
  return sha256Hex(canonicalJSONBytes({
    schema_version: "witself.agent-email-domain-authority-state-final.v1",
    rows,
    rows_hash: rowsHash,
  }));
}

async function scanSnapshotPage(storage, maintenance) {
  const page = await storagePage(storage, maintenance.cursor);
  let authorityKeys = maintenance.authority_keys;
  const breakdown = clone(maintenance.authority_breakdown);
  assertMaintenanceCapacity(maintenance);
  let rowsHash = maintenance.rows_hash;
  let scannedKeys = maintenance.scanned_keys;
  const puts = [];
  for (const [key, value] of page.rows) {
    scannedKeys += 1;
    if (scannedKeys > MAX_SCANNED_STORAGE_KEYS) {
      fail("agent email domain registry exceeds the bounded storage scan limit",
        "agent_email_domain_journal_storage_limit_exceeded");
    }
    const classification = classifyAgentEmailDomainStorageKey(key);
    if (classification === "unknown") {
      fail(`unclassified agent email domain storage key: ${key}`,
        "agent_email_domain_journal_unknown_storage_key");
    }
    if (classification !== "authority") continue;
    authorityKeys += 1;
    breakdown[authorityCategory(key)] += 1;
    if (authorityKeys > MAX_AUTHORITY_KEYS) {
      fail("agent email domain authority exceeds 10000 keys",
        "agent_email_domain_journal_authority_limit_exceeded");
    }
    rowsHash = await appendDigestRow(
      rowsHash,
      authorityKeys,
      key,
      value,
    );
    puts.push({ key, value: clone(value) });
  }
  return {
    puts,
    cursor: page.next_cursor,
    complete: page.complete,
    authority_keys: authorityKeys,
    authority_breakdown: breakdown,
    scanned_keys: scannedKeys,
    rows_hash: rowsHash,
  };
}

async function readStorageState(storage) {
  const authority = new Map();
  const derived = new Map();
  const local = new Map();
  let cursor = null;
  let scanned = 0;
  for (;;) {
    const page = await storagePage(storage, cursor, 500);
    for (const [key, value] of page.rows) {
      scanned += 1;
      if (scanned > MAX_SCANNED_STORAGE_KEYS) {
        fail("agent email domain registry exceeds the bounded storage scan limit",
          "agent_email_domain_journal_storage_limit_exceeded");
      }
      const classification = classifyAgentEmailDomainStorageKey(key);
      if (classification === "authority") authority.set(key, clone(value));
      else if (classification === "derived") derived.set(key, clone(value));
      else if (classification === "journal_local") local.set(key, clone(value));
      else {
        fail(`unclassified agent email domain storage key: ${key}`,
          "agent_email_domain_journal_unknown_storage_key");
      }
    }
    if (authority.size > MAX_AUTHORITY_KEYS) {
      fail("agent email domain authority exceeds 10000 keys",
        "agent_email_domain_journal_authority_limit_exceeded");
    }
    if (page.complete) break;
    cursor = page.next_cursor;
  }
  return { authority, derived, local, scanned };
}

function assertExactDerivedState(actual, expected, message, code) {
  if (actual.size !== expected.size) fail(message, code);
  for (const [key, value] of actual) {
    if (!expected.has(key) ||
        canonicalJSONString(expected.get(key)) !== canonicalJSONString(value)) {
      fail(message, code);
    }
  }
}

function headFromEntry(entry) {
  return {
    schema_version: LOCAL_SCHEMA,
    stream_id: entry.stream_id,
    sequence: entry.sequence,
    hash: entry.entry_hash,
    authority_epoch: entry.authority_epoch,
    registry_revision: entry.registry_revision,
    audit_sequence: entry.audit_sequence,
    updated_at: entry.occurred_at,
  };
}

function publicHead(value) {
  if (!validHead(value)) return null;
  return {
    schema_version: value.schema_version,
    stream_id: value.stream_id,
    sequence: value.sequence,
    hash: value.hash,
    authority_epoch: value.authority_epoch,
    registry_revision: value.registry_revision,
    audit_sequence: value.audit_sequence,
    updated_at: value.updated_at,
  };
}

function recoveryPublic(record) {
  if (!record || ![LEGACY_RECOVERY_SCHEMA, RECOVERY_SCHEMA]
    .includes(record.schema_version)) return null;
  return {
    recovery_id: record.recovery_id,
    source_stream_id: record.source_stream_id,
    expected_head: clone(record.expected_head),
    replay_head: clone(record.replay_head),
    phase: record.phase,
    authority_keys: record.authority_keys,
    derived_keys: record.derived_keys ?? 0,
    state_digest: record.state_digest ?? null,
    action_fence: record.schema_version === RECOVERY_SCHEMA &&
        SHA256_PATTERN.test(record.action_fence ?? "")
      ? record.action_fence
      : null,
    sealed: record.phase === "sealed",
    failed: record.phase === "failed",
    failure_code: record.failure_code ?? null,
    created_at: record.created_at,
    updated_at: record.updated_at,
    sealed_at: record.sealed_at ?? null,
  };
}

/**
 * Serial commit helper for the singleton registry. The caller must hold one
 * global journal lane for every mutating journal or recovery action; this class
 * deliberately does not own request scheduling or call external routing/cell
 * services.
 */
export class AgentEmailDomainJournalRuntime {
  constructor(storage, env, dependencies = {}) {
    this.storage = storage;
    this.env = env;
    this.now = dependencies.now ?? (() => new Date());
    this.newStreamID = dependencies.newStreamID ?? randomStreamID;
    this.newRecoveryActionFence = dependencies.newRecoveryActionFence ??
      randomRecoveryActionFence;
    this.afterJournalAppend = dependencies.afterJournalAppend ?? (() => {});
    this.afterMaintenanceFinalize =
      dependencies.afterMaintenanceFinalize ?? (() => {});
    this.afterRecoveryAction = dependencies.afterRecoveryAction ?? (() => {});
    this.log = dependencies.log ?? ((line) => console.log(line));
  }

  async logCapacityRefusal(used, attempted) {
    try {
      await this.log(JSON.stringify({
        event: "agent_email_domain_authority_capacity_refused",
        code: "agent_email_domain_journal_authority_limit_exceeded",
        used,
        attempted,
        max: MAX_AUTHORITY_KEYS,
      }));
    } catch {
      // Admission must remain fail-closed even when diagnostic output fails.
    }
  }

  async capacityAfterImage(head, afterImage) {
    const current = await this.storage.get(
      AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY,
    );
    if (!capacityMatchesHead(current, head)) {
      fail("agent email domain authority capacity is not bound to the current head",
        "agent_email_domain_journal_capacity_unavailable");
    }
    let authorityKeys = current.authority_keys;
    let breakdown = clone(current.breakdown);
    const usedBefore = authorityKeys;

    const currentValues = new Map();
    for (const { key } of afterImage.puts) {
      currentValues.set(key, await this.storage.get(key));
    }
    for (const key of afterImage.deletes) {
      currentValues.set(key, await this.storage.get(key));
    }
    for (const { key } of afterImage.puts) {
      if (currentValues.get(key) === undefined) {
        authorityKeys += 1;
        breakdown[authorityCategory(key)] += 1;
      }
    }
    for (const key of afterImage.deletes) {
      if (currentValues.get(key) !== undefined) {
        authorityKeys -= 1;
        breakdown[authorityCategory(key)] -= 1;
      }
    }
    if (authorityKeys > MAX_AUTHORITY_KEYS) {
      await this.logCapacityRefusal(usedBefore, authorityKeys);
      fail("agent email domain authority exceeds 10000 keys",
        "agent_email_domain_journal_authority_limit_exceeded");
    }
    if (authorityKeys < 0 ||
        !validAuthorityBreakdown(breakdown, authorityKeys)) {
      fail("agent email domain authority capacity invariant failed",
        "agent_email_domain_journal_capacity_invalid");
    }
    return { authority_keys: authorityKeys, breakdown };
  }

  async nextRecoveryActionFence(currentFence = null) {
    for (let attempt = 0;
      attempt < RECOVERY_ACTION_FENCE_GENERATION_ATTEMPTS;
      attempt += 1) {
      const candidate = await this.newRecoveryActionFence();
      if (SHA256_PATTERN.test(candidate ?? "") && candidate !== currentFence) {
        return candidate;
      }
    }
    fail("agent email domain recovery action fence is unavailable",
      "agent_email_domain_recovery_action_fence_unavailable");
  }

  async raw(entries, deletes = []) {
    const apply = async (storage) => {
      for (const [key, value] of entries) await storage.put(key, value);
      for (const key of deletes) await storage.delete(key);
    };
    if (typeof this.storage.transaction === "function") {
      return this.storage.transaction(apply);
    }
    return apply(this.storage);
  }

  bucket() {
    const bucket = this.env?.AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL;
    if (!bucket || typeof bucket.put !== "function" ||
        typeof bucket.get !== "function") {
      fail("agent email domain authority journal binding is unavailable");
    }
    return bucket;
  }

  async status() {
    let [head, pending, fork, bootstrap, capacity] = await Promise.all([
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY),
    ]);
    let remoteHeadChecked = false;
    let remoteHeadHealthy = null;
    let remoteHeadError = null;
    if (!fork && validHead(head) && head.sequence > 0) {
      remoteHeadChecked = true;
      try {
        await this.assertRemoteHeadContinuity(head);
        remoteHeadHealthy = true;
      } catch (error) {
        remoteHeadHealthy = false;
        remoteHeadError = error?.code ??
          "agent_email_domain_journal_unavailable";
        fork = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY);
      }
    }
    const capacityStatus = publicCapacity(capacity, head);
    const healthy = validHead(head) && capacityStatus.ready && !pending &&
      !fork && remoteHeadHealthy !== false;
    return {
      enabled: validHead(head),
      required: journalRequired(this.env),
      head: publicHead(head),
      pending: Boolean(pending),
      forked: Boolean(fork),
      healthy,
      remote_head_checked: remoteHeadChecked,
      remote_head_healthy: remoteHeadHealthy,
      degradation_code: fork?.code ?? remoteHeadError ??
        (validHead(head) && !capacityStatus.ready
          ? "agent_email_domain_journal_capacity_unavailable"
          : null),
      bootstrap: bootstrap ? this.maintenanceResult(bootstrap) : null,
      capacity: capacityStatus,
    };
  }

  async assertOperationalReady() {
    const [head, fork, maintenance, recovery, currentMeta, capacity] =
      await Promise.all([
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY),
      this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY),
      this.storage.get("meta"),
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY),
    ]);
    if (recovery) {
      fail("agent email domain recovery targets are permanently write-fenced",
        "agent_email_domain_recovery_target_sealed");
    }
    if (fork) {
      fail("agent email domain authority journal is permanently fenced",
        "agent_email_domain_journal_fork_detected");
    }
    if (maintenance) {
      fail("agent email domain authority work is frozen for journal maintenance",
        "agent_email_domain_journal_write_frozen");
    }
    if (head && !validHead(head)) {
      fail("agent email domain authority journal head is invalid",
        "agent_email_domain_journal_fence_mismatch");
    }
    if (head && !capacityMatchesHead(capacity, head)) {
      fail("agent email domain authority capacity is not bound to the current head",
        "agent_email_domain_journal_capacity_unavailable");
    }
    // A new object may seed itself and create the genesis head in one R2-first
    // commit. Any object with existing authority must be bootstrapped before
    // ordinary work can perform even preliminary external reads or writes.
    if (journalRequired(this.env) && !head && currentMeta) {
      fail("existing agent email domain authority is not journaled",
        "agent_email_domain_journal_bootstrap_required");
    }
    return { ready: true };
  }

  async recordFork(error, pending) {
    if (await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)
      .catch(() => null)) return;
    const now = this.now().toISOString();
    await this.raw([[AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY, {
      schema_version: LOCAL_SCHEMA,
      code: error?.code ?? "agent_email_domain_journal_fork_detected",
      stream_id: pending?.entry?.stream_id ?? null,
      sequence: pending?.entry?.sequence ?? null,
      detected_at: now,
    }]]).catch(() => {});
  }

  async recordHeadContinuityFailure(error, head) {
    if (await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)
      .catch(() => null)) return;
    const now = this.now().toISOString();
    await this.raw([[AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY, {
      schema_version: LOCAL_SCHEMA,
      code: error?.code ?? "agent_email_domain_journal_fork_detected",
      stream_id: head?.stream_id ?? null,
      sequence: head?.sequence ?? null,
      detected_at: now,
    }]]).catch(() => {});
  }

  async assertRemoteHeadContinuity(head) {
    if (!validHead(head)) {
      fail("agent email domain authority journal head is invalid",
        "agent_email_domain_journal_fence_mismatch");
    }
    if (head.sequence === 0) return { checked: false };

    let object;
    try {
      object = await this.bucket().get(
        agentEmailDomainJournalEntryKey(head.stream_id, head.sequence),
      );
    } catch {
      fail("agent email domain authority journal is unavailable",
        "agent_email_domain_journal_unavailable");
    }
    if (!object) {
      const error = new AgentEmailDomainJournalRuntimeError(
        "agent email domain authority journal current head is missing",
        "agent_email_domain_journal_gap",
      );
      await this.recordHeadContinuityFailure(error, head);
      throw error;
    }

    let bytes;
    try {
      bytes = new Uint8Array(await object.arrayBuffer());
    } catch {
      fail("agent email domain authority journal is unavailable",
        "agent_email_domain_journal_unavailable");
    }
    if (bytes.byteLength > JOURNAL_ENTRY_MAX_BYTES) {
      const error = new AgentEmailDomainJournalRuntimeError(
        "agent email domain authority journal current head is too large",
        "agent_email_domain_journal_schema_invalid",
      );
      await this.recordHeadContinuityFailure(error, head);
      throw error;
    }

    let raw;
    try {
      raw = JSON.parse(
        new TextDecoder("utf-8", { fatal: true }).decode(bytes),
      );
    } catch {
      const error = new AgentEmailDomainJournalRuntimeError(
        "agent email domain authority journal current head is unreadable",
        "agent_email_domain_journal_schema_invalid",
      );
      await this.recordHeadContinuityFailure(error, head);
      throw error;
    }

    let built;
    try {
      built = await validateAgentEmailDomainJournalEntry(raw, {
        stream_id: head.stream_id,
        sequence: head.sequence,
        authority_epoch: head.authority_epoch,
      });
    } catch (cause) {
      const converted = journalError(cause);
      await this.recordHeadContinuityFailure(converted, head);
      throw converted;
    }
    if (!bytesEqual(bytes, built.bytes) || built.entry.entry_hash !== head.hash ||
        built.entry.registry_revision !== head.registry_revision ||
        built.entry.audit_sequence !== head.audit_sequence) {
      const error = new AgentEmailDomainJournalRuntimeError(
        "agent email domain authority journal current head does not match " +
          "local authority",
        "agent_email_domain_journal_fork_detected",
      );
      await this.recordHeadContinuityFailure(error, head);
      throw error;
    }
    return { checked: true, entry: built.entry };
  }

  async appendPending(pending) {
    let built;
    try {
      await this.assertRemoteHeadContinuity(pending.head_before);
      built = await validateAgentEmailDomainJournalEntry(pending.entry, {
        stream_id: pending.head_before.stream_id,
        sequence: pending.head_before.sequence + 1,
        previous_hash: pending.head_before.hash,
        authority_epoch: pending.head_before.authority_epoch,
      });
      await appendAgentEmailDomainJournalEntry(this.bucket(), built);
      await this.afterJournalAppend(built.entry);
      return built;
    } catch (error) {
      const converted = journalError(error);
      if (converted.code === "agent_email_domain_journal_fork_detected") {
        await this.recordFork(converted, pending);
      }
      throw converted;
    }
  }

  async resume(apply) {
    const fork = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY);
    if (fork) {
      fail("agent email domain authority journal is permanently fenced",
        "agent_email_domain_journal_fork_detected");
    }
    const pending = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY);
    if (!pending) return { resumed: false };
    if (pending.schema_version !== LOCAL_SCHEMA ||
        !validHead(pending.head_before) || !pending.entry ||
        !Array.isArray(pending.entries) || !Array.isArray(pending.deletes) ||
        !capacityMatchesHead(
          pending.capacity_after,
          headFromEntry(pending.entry),
        )) {
      fail("agent email domain authority journal pending record is invalid",
        "agent_email_domain_journal_pending_invalid");
    }
    const currentHead = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
    if (!validHead(currentHead) ||
        currentHead.stream_id !== pending.head_before.stream_id ||
        currentHead.sequence !== pending.head_before.sequence ||
        currentHead.hash !== pending.head_before.hash ||
        currentHead.authority_epoch !== pending.head_before.authority_epoch) {
      fail("agent email domain authority journal pending head is stale",
        "agent_email_domain_journal_fence_mismatch");
    }
    const built = await this.appendPending(pending);
    const nextHead = {
      schema_version: LOCAL_SCHEMA,
      stream_id: built.entry.stream_id,
      sequence: built.entry.sequence,
      hash: built.entry.entry_hash,
      authority_epoch: built.entry.authority_epoch,
      registry_revision: built.entry.registry_revision,
      audit_sequence: built.entry.audit_sequence,
      updated_at: built.entry.occurred_at,
    };
    if (!capacityMatchesHead(pending.capacity_after, nextHead)) {
      fail("agent email domain authority journal pending capacity is invalid",
        "agent_email_domain_journal_pending_invalid");
    }
    await apply(
      [
        ...pending.entries,
        [AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY, nextHead],
        [AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, pending.capacity_after],
      ],
      [...pending.deletes, AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY],
      pending.options ?? {},
    );
    return { resumed: true, head: nextHead };
  }

  async commit(entries, deletes, options, apply) {
    const recovery = await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY);
    if (recovery) {
      fail("agent email domain recovery targets are permanently write-fenced",
        "agent_email_domain_recovery_target_sealed");
    }
    await this.resume(apply);
    const bootstrap = await this.storage.get(
      AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
    );
    if (bootstrap) {
      fail("agent email domain authority writes are frozen for journal maintenance",
        "agent_email_domain_journal_write_frozen");
    }
    let head = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY);
    const currentMeta = await this.storage.get("meta");
    if (!head) {
      if (!journalRequired(this.env)) return apply(entries, deletes, options);
      // A brand-new object can establish its stream with its seed transaction.
      // An existing authority must be frozen and bootstrapped first so no
      // pre-journal claims, tombstones, or audit rows are omitted.
      if (currentMeta) {
        fail("existing agent email domain authority is not journaled",
          "agent_email_domain_journal_bootstrap_required");
      }
      let first;
      try {
        first = await this.storage.list({ limit: 1 });
      } catch {
        fail("agent email domain journal genesis target cannot be enumerated",
          "agent_email_domain_journal_unavailable");
      }
      if (mapRows(first).length !== 0) {
        fail("agent email domain journal genesis target is not empty",
          "agent_email_domain_journal_fence_mismatch");
      }
      const streamID = configuredStreamID(this.env) ?? this.newStreamID();
      if (!STREAM_ID_PATTERN.test(streamID)) {
        fail("generated agent email domain authority stream id is invalid",
          "agent_email_domain_journal_configuration_invalid");
      }
      head = {
        schema_version: LOCAL_SCHEMA,
        stream_id: streamID,
        sequence: 0,
        hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
        authority_epoch: 1,
        registry_revision: 0,
        audit_sequence: 0,
        updated_at: this.now().toISOString(),
      };
      // Seed the empty-head capacity in the same local transaction as the
      // genesis head. If staging the first pending mutation then fails, a
      // retry still has a complete head/capacity fence from which to proceed.
      await this.raw([
        [AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY, head],
        [AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, capacityForHead(
          head,
          0,
          emptyAuthorityBreakdown(),
          head.updated_at,
        )],
      ]);
    }
    if (!validHead(head)) {
      fail("agent email domain authority journal head is invalid",
        "agent_email_domain_journal_fence_mismatch");
    }
    const afterImage = authorityAfterImage(entries, deletes);
    if (afterImage.puts.length === 0 && afterImage.deletes.length === 0) {
      return apply(entries, deletes, options);
    }
    const nextCapacity = await this.capacityAfterImage(head, afterImage);
    const desiredMeta = metaPut(afterImage) ?? currentMeta;
    if (!desiredMeta || !Number.isSafeInteger(desiredMeta.registry_revision) ||
        !Number.isSafeInteger(desiredMeta.audit_sequence)) {
      fail("agent email domain authority mutation has no exact meta fence",
        "agent_email_domain_journal_fence_mismatch");
    }
    const audit = highestAudit(afterImage);
    const occurredAt = audit?.occurred_at ?? this.now().toISOString();
    const operationFingerprint = await sha256Hex(canonicalJSONBytes(afterImage));
    const sequence = head.sequence + 1;
    let built;
    try {
      built = await buildAgentEmailDomainJournalEntry({
        stream_id: head.stream_id,
        kind: "mutation",
        authority_epoch: head.authority_epoch,
        sequence,
        previous_hash: head.hash,
        registry_revision: desiredMeta.registry_revision,
        audit_sequence: desiredMeta.audit_sequence,
        occurred_at: occurredAt,
        operation_id: `journal:${sequence}`,
        operation_fingerprint: operationFingerprint,
        actor: audit
          ? { kind: audit.actor_kind, id: audit.actor_id }
          : { kind: "system", id: "registry" },
        action: audit?.action ?? "registry.storage_mutation",
        target: String(audit?.target ?? "registry").slice(0, 512),
        metadata: { storage_puts: entries.length, storage_deletes: deletes.length },
        after_image: afterImage,
      });
    } catch (error) {
      throw journalError(error);
    }
    const capacityAfter = capacityForHead(
      headFromEntry(built.entry),
      nextCapacity.authority_keys,
      nextCapacity.breakdown,
      built.entry.occurred_at,
    );
    const pending = {
      schema_version: LOCAL_SCHEMA,
      head_before: head,
      entry: built.entry,
      entries: clone(entries),
      deletes: clone(deletes),
      capacity_after: capacityAfter,
      options: cleanOptions(options),
      created_at: this.now().toISOString(),
    };
    await this.raw([[AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY, pending]]);
    return this.resume(apply);
  }

  async localApply(entries, deletes = [], apply = null) {
    if (apply) return apply(entries, deletes, {});
    return this.raw(entries, deletes);
  }

  async assertMaintenanceCanStart(apply) {
    if (await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY)) {
      fail("agent email domain recovery targets cannot enter journal maintenance",
        "agent_email_domain_recovery_target_sealed");
    }
    if (await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_FORK_KEY)) {
      fail("agent email domain authority journal is permanently fenced",
        "agent_email_domain_journal_fork_detected");
    }
    const pending = await this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_PENDING_KEY);
    if (pending && typeof apply !== "function") {
      fail("caller-provided raw apply is required to resume journal authority",
        "agent_email_domain_journal_apply_required");
    }
    if (pending) await this.resume(apply);
  }

  maintenanceResult(record, complete = false, head = null) {
    return {
      kind: record?.kind ?? null,
      phase: complete ? "complete" : record?.phase ?? null,
      complete,
      frozen: !complete,
      authority_keys: record?.authority_keys ?? 0,
      scanned_keys: record?.scanned_keys ?? 0,
      head: publicHead(head ?? record?.head ?? null),
      pending: Boolean(record?.pending),
    };
  }

  async beginMaintenance(kind, input, apply) {
    const request = maintenanceRequest(input);
    const requestFingerprint = await sha256Hex(canonicalJSONBytes({
      schema_version: MAINTENANCE_SCHEMA,
      kind,
      ...request,
    }));
    await this.assertMaintenanceCanStart(apply);
    const receiptKey = maintenanceReceiptKey(request.idempotency_key);
    const prior = await this.storage.get(receiptKey);
    if (prior?.schema_version === MAINTENANCE_SCHEMA &&
        prior.idempotency_key === request.idempotency_key &&
        (prior.kind !== kind ||
          prior.request_fingerprint !== requestFingerprint)) {
      fail("journal maintenance idempotency key was reused with changed input",
        "agent_email_domain_journal_idempotency_conflict");
    }
    if (prior?.schema_version === MAINTENANCE_SCHEMA &&
        prior.kind === kind &&
        prior.idempotency_key === request.idempotency_key &&
        prior.request_fingerprint === requestFingerprint) {
      return { ...prior, finalized: true };
    }
    const existing = await this.storage.get(
      AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY,
    );
    if (existing) {
      if (existing.schema_version !== MAINTENANCE_SCHEMA ||
          existing.kind !== kind ||
          existing.idempotency_key !== request.idempotency_key ||
          existing.request_fingerprint !== requestFingerprint) {
        fail("another agent email domain journal maintenance operation is active",
          "agent_email_domain_journal_write_frozen");
      }
      return existing;
    }

    let [head, meta] = await Promise.all([
      this.storage.get(AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY),
      this.storage.get("meta"),
    ]);
    if (!meta && !head && kind === "bootstrap") {
      const [first, alarm] = await Promise.all([
        this.storage.list({ limit: 1 }),
        typeof this.storage.getAlarm === "function"
          ? this.storage.getAlarm()
          : null,
      ]);
      if (mapRows(first).length !== 0 || alarm != null) {
        fail("empty custom domain authority initialization target is not empty",
          "agent_email_domain_journal_fence_mismatch");
      }
      const initializedAt = this.now().toISOString();
      meta = {
        schema_version: "witself.agent-email-domain.v1",
        registry_revision: 0,
        audit_sequence: 0,
        created_at: initializedAt,
        updated_at: initializedAt,
      };
      await this.localApply([["meta", meta]], [], apply);
    }
    if (!meta || !Number.isSafeInteger(meta.registry_revision) ||
        !Number.isSafeInteger(meta.audit_sequence)) {
      fail("agent email domain authority meta is unavailable for maintenance",
        "agent_email_domain_journal_fence_mismatch");
    }
    if (kind === "bootstrap" && head) {
      fail("agent email domain authority journal is already bootstrapped",
        "agent_email_domain_journal_already_bootstrapped");
    }
    if (kind === "checkpoint" && !validHead(head)) {
      fail("agent email domain authority journal is not bootstrapped",
        "agent_email_domain_journal_bootstrap_required");
    }
    const streamID = kind === "checkpoint"
      ? head.stream_id
      : configuredStreamID(this.env) ?? this.newStreamID();
    if (!STREAM_ID_PATTERN.test(streamID)) {
      fail("agent email domain authority stream id is invalid",
        "agent_email_domain_journal_configuration_invalid");
    }
    const now = this.now().toISOString();
    const maintenance = {
      schema_version: MAINTENANCE_SCHEMA,
      kind,
      phase: "scan",
      stream_id: streamID,
      actor: request.actor,
      reason: request.reason,
      idempotency_key: request.idempotency_key,
      request_fingerprint: requestFingerprint,
      source_head: kind === "checkpoint" ? clone(head) : null,
      head: kind === "checkpoint"
        ? clone(head)
        : {
          schema_version: LOCAL_SCHEMA,
          stream_id: streamID,
          sequence: 0,
          hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
          authority_epoch: 1,
          registry_revision: meta.registry_revision,
          audit_sequence: meta.audit_sequence,
          updated_at: now,
        },
      registry_revision: meta.registry_revision,
      audit_sequence: meta.audit_sequence,
      cursor: null,
      authority_keys: 0,
      authority_breakdown: emptyAuthorityBreakdown(),
      scanned_keys: 0,
      rows_hash: await initialRowsHash(),
      pending: null,
      created_at: now,
      updated_at: now,
    };
    // This local record is the global authority-write freeze. The registry's
    // single journal lane must serialize this write with commit().
    await this.localApply([
      [AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY, maintenance],
    ], [], apply);
    return maintenance;
  }

  async stageMaintenanceEntry(record, built, next, apply) {
    const staged = {
      ...record,
      pending: {
        entry: built.entry,
        next,
      },
      updated_at: this.now().toISOString(),
    };
    await this.localApply([
      [AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY, staged],
    ], [], apply);
    return staged;
  }

  async flushMaintenanceEntry(record, apply) {
    let built;
    try {
      await this.assertRemoteHeadContinuity(record.head);
      built = await validateAgentEmailDomainJournalEntry(record.pending.entry, {
        stream_id: record.head.stream_id,
        sequence: record.head.sequence + 1,
        previous_hash: record.head.hash,
        authority_epoch: record.head.authority_epoch,
      });
      await appendAgentEmailDomainJournalEntry(this.bucket(), built);
      // This hook models a process death after the create-only R2 write and
      // before durable local progress. A retry must append identical bytes.
      await this.afterJournalAppend(built.entry);
    } catch (error) {
      throw journalError(error);
    }
    const advanced = {
      ...record,
      ...clone(record.pending.next),
      head: headFromEntry(built.entry),
      pending: null,
      updated_at: this.now().toISOString(),
    };
    await this.localApply([
      [AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY, advanced],
    ], [], apply);
    return advanced;
  }

  async buildSnapshotEntry(record, snapshot) {
    const afterImage = validateAgentEmailDomainAuthorityAfterImage({
      puts: snapshot.puts,
      deletes: [],
    });
    const sequence = record.head.sequence + 1;
    return buildAgentEmailDomainJournalEntry({
      stream_id: record.stream_id,
      kind: "bootstrap",
      authority_epoch: record.head.authority_epoch,
      sequence,
      previous_hash: record.head.hash,
      registry_revision: record.registry_revision,
      audit_sequence: record.audit_sequence,
      occurred_at: this.now().toISOString(),
      operation_id: `bootstrap:${sequence}`,
      operation_fingerprint: await sha256Hex(canonicalJSONBytes(afterImage)),
      actor: record.actor,
      action: "registry.authority.bootstrap_page",
      target: "agent-email-domain-registry",
      metadata: {
        schema_version: MAINTENANCE_SCHEMA,
        page_authority_keys: snapshot.puts.length,
        authority_keys_after: snapshot.authority_keys,
      },
      after_image: afterImage,
    });
  }

  async buildCheckpointEntry(record, stateDigest) {
    const sequence = record.head.sequence + 1;
    const metadata = {
      schema_version: CHECKPOINT_SCHEMA,
      maintenance_kind: record.kind,
      authority_digest: stateDigest,
      authority_keys: record.authority_keys,
      source_head: {
        sequence: record.head.sequence,
        hash: record.head.hash,
        authority_epoch: record.head.authority_epoch,
      },
      reason: record.reason,
    };
    return buildAgentEmailDomainJournalEntry({
      stream_id: record.stream_id,
      kind: "checkpoint",
      authority_epoch: record.head.authority_epoch,
      sequence,
      previous_hash: record.head.hash,
      registry_revision: record.registry_revision,
      audit_sequence: record.audit_sequence,
      occurred_at: this.now().toISOString(),
      operation_id: `checkpoint:${sequence}`,
      operation_fingerprint: await sha256Hex(canonicalJSONBytes(metadata)),
      actor: record.actor,
      action: "registry.authority.checkpointed",
      target: "agent-email-domain-registry",
      metadata,
      after_image: { puts: [], deletes: [] },
    });
  }

  async advanceMaintenance(record, apply) {
    if (record.finalized === true) {
      return clone(record.result);
    }
    // Releases predating the exact capacity counter did not persist the
    // category breakdown. Reject such in-progress maintenance before flushing
    // a staged journal object or advancing any local fence; the operator must
    // finish it on the source release before upgrading.
    assertMaintenanceCapacity(record);
    if (record.pending) {
      assertMaintenanceCapacity({
        ...record,
        ...record.pending.next,
      });
      record = await this.flushMaintenanceEntry(record, apply);
      return this.maintenanceResult(record);
    }
    if (record.phase === "complete") {
      const completedHead = clone(record.head);
      const capacity = capacityForHead(
        completedHead,
        record.authority_keys,
        record.authority_breakdown,
        this.now().toISOString(),
      );
      const result = this.maintenanceResult(record, true, completedHead);
      const completion = {
        schema_version: MAINTENANCE_SCHEMA,
        kind: record.kind,
        actor: record.actor,
        reason: record.reason,
        idempotency_key: record.idempotency_key,
        request_fingerprint: record.request_fingerprint,
        authority_keys: record.authority_keys,
        scanned_keys: record.scanned_keys,
        state_digest: record.state_digest,
        head: completedHead,
        result,
        completed_at: this.now().toISOString(),
      };
      await this.localApply([
        [AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY, completedHead],
        [AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, capacity],
        [maintenanceReceiptKey(record.idempotency_key), completion],
      ], [AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY], apply);
      await this.afterMaintenanceFinalize(result);
      return result;
    }
    if (record.phase === "scan") {
      const snapshot = await scanSnapshotPage(this.storage, record);
      const next = {
        cursor: snapshot.cursor,
        authority_keys: snapshot.authority_keys,
        authority_breakdown: snapshot.authority_breakdown,
        scanned_keys: snapshot.scanned_keys,
        rows_hash: snapshot.rows_hash,
        phase: snapshot.complete ? "checkpoint" : "scan",
      };
      if (record.kind === "bootstrap" && snapshot.puts.length > 0) {
        const built = await this.buildSnapshotEntry(record, snapshot);
        const staged = await this.stageMaintenanceEntry(
          record,
          built,
          next,
          apply,
        );
        const advanced = await this.flushMaintenanceEntry(staged, apply);
        return this.maintenanceResult(advanced);
      }
      const advanced = {
        ...record,
        ...next,
        updated_at: this.now().toISOString(),
      };
      await this.localApply([
        [AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_KEY, advanced],
      ], [], apply);
      return this.maintenanceResult(advanced);
    }
    if (record.phase === "checkpoint") {
      const digest = await finishRowsDigest(
        record.rows_hash,
        record.authority_keys,
      );
      const exact = await readStorageState(this.storage);
      const validation = validateAgentEmailDomainRecoveredState(exact.authority, {
        expected_registry_revision: record.registry_revision,
        expected_audit_sequence: record.audit_sequence,
      });
      const expectedDerived = rebuildAgentEmailDomainDerivedState(
        exact.authority,
      );
      assertExactDerivedState(
        exact.derived,
        expectedDerived,
        "agent email domain derived state drifted from canonical authority",
        "agent_email_domain_journal_derived_state_mismatch",
      );
      const exactDigest = await agentEmailDomainAuthorityStateDigest(
        exact.authority,
      );
      if (validation.authority_keys !== record.authority_keys ||
          exactDigest !== digest) {
        fail("agent email domain authority changed while writes were frozen",
          "agent_email_domain_journal_fence_mismatch");
      }
      const exactCapacity = authorityCapacityFromState(exact.authority);
      if (!validAuthorityBreakdown(
        record.authority_breakdown,
        record.authority_keys,
      ) || canonicalJSONString(exactCapacity.breakdown) !==
          canonicalJSONString(record.authority_breakdown)) {
        fail("agent email domain authority capacity changed while writes were frozen",
          "agent_email_domain_journal_fence_mismatch");
      }
      const built = await this.buildCheckpointEntry(record, digest);
      const staged = await this.stageMaintenanceEntry(record, built, {
        phase: "complete",
        state_digest: digest,
      }, apply);
      const advanced = await this.flushMaintenanceEntry(staged, apply);
      return this.maintenanceResult(advanced);
    }
    fail("agent email domain journal maintenance state is invalid",
      "agent_email_domain_journal_maintenance_invalid");
  }

  async bootstrap(input, apply = null) {
    try {
      const record = await this.beginMaintenance("bootstrap", input, apply);
      return await this.advanceMaintenance(record, apply);
    } catch (error) {
      throw journalError(error);
    }
  }

  async checkpoint(input, apply = null) {
    try {
      const record = await this.beginMaintenance("checkpoint", input, apply);
      return await this.advanceMaintenance(record, apply);
    } catch (error) {
      throw journalError(error);
    }
  }

  async startRecovery(input, apply = null) {
    const request = recoveryRequest(input);
    const fingerprint = await sha256Hex(canonicalJSONBytes(request));
    const existing = await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY);
    if (existing) {
      if (existing.schema_version !== RECOVERY_SCHEMA ||
          existing.idempotency_key !== request.idempotency_key ||
          existing.fingerprint !== fingerprint) {
        fail("recovery target is already bound to another recovery",
          "agent_email_domain_recovery_target_not_empty");
      }
      await this.assertRecoveryTargetAlarmFree();
      await this.assertRecoveryStartReplayIsMarkerOnly(existing);
      return recoveryPublic(existing);
    }
    await this.assertRecoveryTargetAlarmFree();
    let first;
    try {
      first = await this.storage.list({ limit: 1 });
    } catch {
      fail("agent email domain recovery target cannot be enumerated",
        "agent_email_domain_journal_unavailable");
    }
    if (mapRows(first).length !== 0) {
      fail("agent email domain recovery target must be empty",
        "agent_email_domain_recovery_target_not_empty");
    }
    const now = this.now().toISOString();
    const actionFence = await this.nextRecoveryActionFence();
    const record = {
      schema_version: RECOVERY_SCHEMA,
      recovery_id: request.recovery_id,
      source_stream_id: request.source_stream_id,
      expected_head: request.expected_head,
      active_object_name: request.active_object_name,
      target_object_name: request.target_object_name,
      actor: request.actor,
      reason: request.reason,
      idempotency_key: request.idempotency_key,
      fingerprint,
      phase: "replay",
      replay_head: {
        sequence: 0,
        hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
        authority_epoch: null,
        registry_revision: 0,
        audit_sequence: 0,
      },
      authority_keys: 0,
      derived_keys: 0,
      state_digest: null,
      checkpoint: null,
      derived_cursor: null,
      action_fence: actionFence,
      created_at: now,
      updated_at: now,
    };
    await this.localApply([[AGENT_EMAIL_DOMAIN_RECOVERY_KEY, record]], [], apply);
    return recoveryPublic(record);
  }

  async assertRecoveryTargetAlarmFree() {
    if (typeof this.storage?.getAlarm !== "function") {
      fail("agent email domain recovery target alarm state is unavailable",
        "agent_email_domain_journal_storage_invalid");
    }
    let alarm;
    try {
      alarm = await this.storage.getAlarm();
    } catch {
      fail("agent email domain recovery target alarm state is unavailable",
        "agent_email_domain_journal_unavailable");
    }
    if (alarm !== null) {
      fail("agent email domain recovery target must not have an alarm",
        "agent_email_domain_recovery_target_not_empty");
    }
  }

  async assertRecoveryStartReplayIsMarkerOnly(record) {
    if (record.phase !== "replay" || record.replay_head?.sequence !== 0) {
      fail("recovery target has already advanced",
        "agent_email_domain_recovery_target_not_empty");
    }
    let rows;
    try {
      rows = mapRows(await this.storage.list({ limit: 2 }));
    } catch {
      fail("agent email domain recovery target cannot be enumerated",
        "agent_email_domain_journal_unavailable");
    }
    if (rows.length !== 1 || rows[0][0] !== AGENT_EMAIL_DOMAIN_RECOVERY_KEY) {
      fail("recovery target contains state beyond its start marker",
        "agent_email_domain_recovery_target_not_empty");
    }
  }

  async recoveryStatus(recoveryID = null) {
    const record = await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY);
    if (!record || (recoveryID && record.recovery_id !== recoveryID)) return null;
    return recoveryPublic(record);
  }

  validateRecoveryAction(input, record) {
    if (!record || !validActor(input?.actor) ||
        input?.recovery_id !== record.recovery_id ||
        !IDEMPOTENCY_KEY_PATTERN.test(input?.idempotency_key ?? "") ||
        !SHA256_PATTERN.test(input?.expected_action_fence ?? "")) {
      fail("agent email domain recovery action is invalid",
        "agent_email_domain_recovery_request_invalid");
    }
    if (record.schema_version === LEGACY_RECOVERY_SCHEMA) {
      fail("legacy agent email domain recovery requires a new empty target",
        "agent_email_domain_recovery_upgrade_required");
    }
    if (record.schema_version !== RECOVERY_SCHEMA ||
        !SHA256_PATTERN.test(record.action_fence ?? "")) {
      fail("agent email domain recovery action fence invariant failed",
        "agent_email_domain_recovery_invariant_failed");
    }
  }

  async recoveryActionContext(action, input, record) {
    this.validateRecoveryAction(input, record);
    const fingerprint = await sha256Hex(canonicalJSONBytes({
      schema_version: RECOVERY_SCHEMA,
      action,
      actor: input.actor,
      recovery_id: input.recovery_id,
      idempotency_key: input.idempotency_key,
      expected_action_fence: input.expected_action_fence,
    }));
    const prior = record.last_action;
    if (prior?.idempotency_key === input.idempotency_key &&
        prior?.request_fence === input.expected_action_fence) {
      if (prior.action !== action || prior.fingerprint !== fingerprint) {
        fail("recovery idempotency key was reused with changed input",
          "agent_email_domain_recovery_idempotency_conflict");
      }
      if (prior.error) {
        fail(prior.error.message, prior.error.code);
      }
      return { replay: clone(prior.result) };
    }
    if (input.expected_action_fence !== record.action_fence) {
      fail("recovery action fence does not match the current recovery state",
        "agent_email_domain_recovery_action_fence_mismatch");
    }
    return {
      action,
      idempotency_key: input.idempotency_key,
      fingerprint,
      request_fence: input.expected_action_fence,
      before: {
        phase: record.phase,
        replay_head: clone(record.replay_head),
        derived_cursor: record.derived_cursor ?? null,
        action_fence: record.action_fence,
      },
    };
  }

  withRecoveryReceipt(record, context, result, error = null) {
    return {
      ...record,
      last_action: {
        action: context.action,
        idempotency_key: context.idempotency_key,
        fingerprint: context.fingerprint,
        request_fence: context.request_fence,
        before: context.before,
        after: {
          phase: record.phase,
          replay_head: clone(record.replay_head),
          derived_cursor: record.derived_cursor ?? null,
          action_fence: record.action_fence,
        },
        result: clone(result),
        error: error
          ? { code: error.code, message: error.message }
          : null,
      },
    };
  }

  async failRecovery(record, error, apply, actionContext = null) {
    let failed = {
      ...record,
      phase: "failed",
      failure_code: error?.code ?? "agent_email_domain_recovery_failed",
      action_fence: await this.nextRecoveryActionFence(record.action_fence),
      updated_at: this.now().toISOString(),
    };
    if (actionContext) {
      const result = recoveryPublic(failed);
      failed = this.withRecoveryReceipt(
        failed,
        actionContext,
        result,
        error,
      );
    }
    await this.localApply([[AGENT_EMAIL_DOMAIN_RECOVERY_KEY, failed]], [], apply);
    return failed;
  }

  async readJournalEntry(streamID, sequence) {
    let object;
    try {
      object = await this.bucket().get(
        agentEmailDomainJournalEntryKey(streamID, sequence),
      );
    } catch {
      fail("agent email domain authority journal is unavailable",
        "agent_email_domain_journal_unavailable");
    }
    if (!object) {
      fail("agent email domain authority journal has a sequence gap",
        "agent_email_domain_journal_gap");
    }
    let body;
    try {
      body = await object.arrayBuffer();
    } catch {
      fail("agent email domain authority journal is unavailable",
        "agent_email_domain_journal_unavailable");
    }
    try {
      const bytes = new Uint8Array(body);
      if (bytes.byteLength > JOURNAL_ENTRY_MAX_BYTES) {
        fail("agent email domain authority journal entry is too large",
          "agent_email_domain_journal_schema_invalid");
      }
      return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    } catch (error) {
      if (error instanceof AgentEmailDomainJournalRuntimeError) throw error;
      fail("agent email domain authority journal entry is unreadable",
        "agent_email_domain_journal_schema_invalid");
    }
  }

  checkpointMetadata(entry) {
    const metadata = entry?.metadata;
    if (entry?.kind !== "checkpoint" ||
        metadata?.schema_version !== CHECKPOINT_SCHEMA ||
        !["bootstrap", "checkpoint"].includes(metadata?.maintenance_kind) ||
        !SHA256_PATTERN.test(metadata?.authority_digest ?? "") ||
        !Number.isSafeInteger(metadata?.authority_keys) ||
        metadata.authority_keys < 1 ||
        metadata.authority_keys > MAX_AUTHORITY_KEYS ||
        !Number.isSafeInteger(metadata?.source_head?.sequence) ||
        metadata.source_head.sequence < 0 ||
        !SHA256_PATTERN.test(metadata?.source_head?.hash ?? "") ||
        !Number.isSafeInteger(metadata?.source_head?.authority_epoch) ||
        metadata.source_head.authority_epoch < 1 ||
        metadata.source_head.sequence !== entry.sequence - 1 ||
        metadata.source_head.hash !== entry.previous_hash ||
        metadata.source_head.authority_epoch !== entry.authority_epoch) {
      fail("recovery journal entry is not a complete authority checkpoint",
        "agent_email_domain_recovery_checkpoint_invalid");
    }
    return clone(metadata);
  }

  async advanceRecovery(input, apply = null) {
    let record = await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY);
    const actionContext = await this.recoveryActionContext(
      "advance",
      input,
      record,
    );
    if (actionContext.replay) return actionContext.replay;
    if (record.phase === "sealed") {
      fail("agent email domain recovery target is permanently sealed",
        "agent_email_domain_recovery_target_sealed");
    }
    if (record.phase !== "replay") {
      fail("agent email domain recovery advance is not allowed in this phase",
        "agent_email_domain_recovery_action_not_allowed");
    }
    const sequence = record.replay_head.sequence + 1;
    if (sequence > record.expected_head.sequence) {
      fail("agent email domain recovery replay advanced past its exact head",
        "agent_email_domain_journal_fence_mismatch");
    }
    let rawEntry;
    let state;
    let replayed;
    try {
      rawEntry = await this.readJournalEntry(record.source_stream_id, sequence);
      state = (await readStorageState(this.storage)).authority;
      replayed = await replayAgentEmailDomainJournalPage([rawEntry], {
        stream_id: record.source_stream_id,
        state,
        head: record.replay_head,
        max_entries: 1,
      });
      if (replayed.state.size > MAX_AUTHORITY_KEYS) {
        fail("recovered agent email domain authority exceeds 10000 keys",
          "agent_email_domain_journal_authority_limit_exceeded");
      }
      if (sequence === record.expected_head.sequence &&
          replayed.head.hash !== record.expected_head.hash) {
        fail("recovery journal head does not match the expected hash",
          "agent_email_domain_journal_fence_mismatch");
      }
    } catch (error) {
      const converted = journalError(error);
      if (converted.code !== "agent_email_domain_journal_unavailable") {
        await this.failRecovery(record, converted, apply, actionContext);
      }
      throw converted;
    }

    const puts = [];
    const deletes = [];
    for (const [key, value] of replayed.state) {
      if (!state.has(key) || canonicalJSONString(state.get(key)) !==
          canonicalJSONString(value)) {
        puts.push([key, value]);
      }
    }
    for (const key of state.keys()) {
      if (!replayed.state.has(key)) deletes.push(key);
    }
    const next = {
      ...record,
      replay_head: replayed.head,
      authority_keys: replayed.state.size,
      updated_at: this.now().toISOString(),
    };
    try {
      if (rawEntry.kind === "checkpoint") {
        const checkpoint = this.checkpointMetadata(rawEntry);
        const validation = validateAgentEmailDomainRecoveredState(
          replayed.state,
          {
            expected_registry_revision: replayed.head.registry_revision,
            expected_audit_sequence: replayed.head.audit_sequence,
          },
        );
        const digest = await agentEmailDomainAuthorityStateDigest(replayed.state);
        if (checkpoint.authority_keys !== validation.authority_keys ||
            checkpoint.authority_digest !== digest) {
          fail("recovered authority digest does not match the checkpoint",
            "agent_email_domain_recovery_digest_mismatch");
        }
        next.checkpoint = {
          ...checkpoint,
          head: clone(replayed.head),
        };
      }
      if (sequence === record.expected_head.sequence) {
        if (!next.checkpoint) {
          fail("recovery journal has no complete authority checkpoint",
            "agent_email_domain_recovery_checkpoint_invalid");
        }
        const validation = validateAgentEmailDomainRecoveredState(
          replayed.state,
          {
            expected_registry_revision: replayed.head.registry_revision,
            expected_audit_sequence: replayed.head.audit_sequence,
          },
        );
        const digest = await agentEmailDomainAuthorityStateDigest(replayed.state);
        if (validation.authority_keys !== replayed.state.size) {
          fail("recovered authority key count is inconsistent",
            "agent_email_domain_recovery_digest_mismatch");
        }
        next.phase = "replayed";
        next.state_digest = digest;
      }
    } catch (error) {
      const converted = journalError(error);
      await this.failRecovery(record, converted, apply, actionContext);
      throw converted;
    }
    const advanced = {
      ...next,
      action_fence: await this.nextRecoveryActionFence(record.action_fence),
    };
    const result = recoveryPublic(advanced);
    const receipted = this.withRecoveryReceipt(
      advanced,
      actionContext,
      result,
    );
    puts.push([AGENT_EMAIL_DOMAIN_RECOVERY_KEY, receipted]);
    await this.localApply(puts, deletes, apply);
    await this.afterRecoveryAction(result);
    return result;
  }

  async verifyRecovery(input, apply = null) {
    let record = await this.storage.get(AGENT_EMAIL_DOMAIN_RECOVERY_KEY);
    const actionContext = await this.recoveryActionContext(
      "verify",
      input,
      record,
    );
    if (actionContext.replay) return actionContext.replay;
    if (record.phase === "sealed") {
      fail("agent email domain recovery target is permanently sealed",
        "agent_email_domain_recovery_target_sealed");
    }
    if (record.phase === "failed") {
      fail("agent email domain recovery verify is not allowed in this phase",
        "agent_email_domain_recovery_action_not_allowed");
    }
    if (!["replayed", "rebuild"].includes(record.phase)) {
      fail("agent email domain recovery replay is incomplete",
        "agent_email_domain_recovery_incomplete");
    }
    let state;
    let expectedDerived;
    try {
      state = await readStorageState(this.storage);
      const validation = validateAgentEmailDomainRecoveredState(
        state.authority,
        {
          expected_registry_revision: record.replay_head.registry_revision,
          expected_audit_sequence: record.replay_head.audit_sequence,
        },
      );
      const digest = await agentEmailDomainAuthorityStateDigest(state.authority);
      if (validation.authority_keys !== record.authority_keys ||
          digest !== record.state_digest) {
        fail("recovered authority changed after replay verification",
          "agent_email_domain_recovery_digest_mismatch");
      }
      expectedDerived = rebuildAgentEmailDomainDerivedState(state.authority);
      for (const [key, value] of state.derived) {
        if (!expectedDerived.has(key) ||
            canonicalJSONString(expectedDerived.get(key)) !==
              canonicalJSONString(value)) {
          fail("recovery target contains unexpected derived state",
            "agent_email_domain_recovery_collision");
        }
      }
      for (const key of state.local.keys()) {
        if (key !== AGENT_EMAIL_DOMAIN_RECOVERY_KEY) {
          fail("recovery target contains unexpected local state",
            "agent_email_domain_recovery_collision");
        }
      }
    } catch (error) {
      const converted = journalError(error);
      if (converted.code !== "agent_email_domain_journal_unavailable") {
        await this.failRecovery(record, converted, apply, actionContext);
      }
      throw converted;
    }

    const remaining = [...expectedDerived]
      .sort(([left], [right]) => left.localeCompare(right))
      .filter(([key]) => record.derived_cursor === null ||
        key > record.derived_cursor);
    if (remaining.length > 0) {
      const page = remaining.slice(0, DERIVED_REBUILD_PAGE_LIMIT);
      const next = {
        ...record,
        phase: "rebuild",
        derived_cursor: page.at(-1)[0],
        derived_keys: Math.min(
          expectedDerived.size,
          (record.derived_keys ?? 0) + page.length,
        ),
        updated_at: this.now().toISOString(),
      };
      const advanced = {
        ...next,
        action_fence: await this.nextRecoveryActionFence(record.action_fence),
      };
      const result = recoveryPublic(advanced);
      const receipted = this.withRecoveryReceipt(
        advanced,
        actionContext,
        result,
      );
      await this.localApply([
        ...page,
        [AGENT_EMAIL_DOMAIN_RECOVERY_KEY, receipted],
      ], [], apply);
      await this.afterRecoveryAction(result);
      return result;
    }

    if (state.derived.size !== expectedDerived.size) {
      const error = new AgentEmailDomainJournalRuntimeError(
        "recovery derived-state rebuild is incomplete",
        "agent_email_domain_recovery_incomplete",
      );
      await this.failRecovery(record, error, apply, actionContext);
      throw error;
    }
    const now = this.now().toISOString();
    const sealed = {
      ...record,
      phase: "sealed",
      derived_keys: expectedDerived.size,
      action_fence: await this.nextRecoveryActionFence(record.action_fence),
      sealed_at: now,
      updated_at: now,
    };
    const journalHead = {
      schema_version: LOCAL_SCHEMA,
      stream_id: record.source_stream_id,
      sequence: record.replay_head.sequence,
      hash: record.replay_head.hash,
      authority_epoch: record.replay_head.authority_epoch,
      registry_revision: record.replay_head.registry_revision,
      audit_sequence: record.replay_head.audit_sequence,
      updated_at: now,
    };
    const recoveredCapacity = authorityCapacityFromState(state.authority);
    if (recoveredCapacity.authority_keys !== record.authority_keys) {
      fail("recovered authority capacity does not match the replay fence",
        "agent_email_domain_recovery_digest_mismatch");
    }
    const capacity = capacityForHead(
      journalHead,
      recoveredCapacity.authority_keys,
      recoveredCapacity.breakdown,
      now,
    );
    const result = recoveryPublic(sealed);
    const receipted = this.withRecoveryReceipt(
      sealed,
      actionContext,
      result,
    );
    await this.localApply([
      [AGENT_EMAIL_DOMAIN_JOURNAL_META_KEY, journalHead],
      [AGENT_EMAIL_DOMAIN_JOURNAL_CAPACITY_KEY, capacity],
      [AGENT_EMAIL_DOMAIN_RECOVERY_KEY, receipted],
    ], [], apply);
    await this.afterRecoveryAction(result);
    return result;
  }
}
