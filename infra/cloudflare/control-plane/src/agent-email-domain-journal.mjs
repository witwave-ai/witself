const DOMAIN_LABEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const ACTOR_ID_PATTERN = /^[A-Za-z0-9._:@-]{1,128}$/;
const REQUEST_ID_PATTERN = /^aedr_[a-z2-7]{16}$/;
const CHALLENGE_TOKEN_PATTERN = /^aedv_[a-z2-7]{32}$/;
const STREAM_ID_PATTERN = /^aedj_[a-z2-7]{16,52}$/;
const OPERATION_ID_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const SHA256_PATTERN = /^[0-9a-f]{64}$/;
const ISO_DATE_MAX_LENGTH = 64;
const STORAGE_KEY_MAX_LENGTH = 1_024;
const ENTRY_MAX_BYTES = 512 * 1_024;
const CANONICAL_MAX_DEPTH = 32;
const CANONICAL_MAX_ITEMS = 10_000;
const PENDING_CHALLENGE_TTL_MS = 7 * 24 * 60 * 60 * 1_000;

export const AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION =
  "witself.agent-email-domain-authority-journal.v1";
export const AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_PREFIX =
  "agent-email-domain-authority/v1";
export const AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH = "0".repeat(64);
export const AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_MAX_CHANGES = 100;
export const AGENT_EMAIL_DOMAIN_AUTHORITY_BOOTSTRAP_PAGE_LIMIT = 100;

const ENTRY_KINDS = new Set(["bootstrap", "mutation", "checkpoint"]);
const ACTOR_KINDS = new Set(["account_operator", "platform_admin", "system"]);

const AUTHORITY_EXACT_KEYS = new Set(["meta"]);
const AUTHORITY_PREFIXES = Object.freeze([
  "audit:",
  "domain:",
  "idem:",
  "lifecycle-fence:",
  "lifecycle-intent:",
  "plan-fence:",
  "plan-intent:",
  "request:",
]);
const DELETABLE_AUTHORITY_PREFIXES = Object.freeze([
  "lifecycle-intent:",
  "plan-intent:",
]);
const DERIVED_PREFIXES = Object.freeze([
  "account-domain:",
  "account-request:",
  "account-usage:",
  "challenge-expiry-due:",
  "domain-pending:",
  "lifecycle-due:",
  "plan-due:",
  "plan-grace-due:",
  "verification-due:",
]);
const JOURNAL_LOCAL_EXACT_KEYS = new Set([
  "agent-email-domain-journal-meta",
  "agent-email-domain-journal-pending",
  "agent-email-domain-journal-fork",
  "agent-email-domain-recovery",
]);
const JOURNAL_LOCAL_PREFIXES = Object.freeze([
  "agent-email-domain-journal:",
  "agent-email-domain-recovery:",
]);

export class AgentEmailDomainJournalError extends Error {
  constructor(message, code = "invalid_agent_email_domain_journal") {
    super(message);
    this.name = "AgentEmailDomainJournalError";
    this.code = code;
  }
}

function journalFail(message, code = "invalid_agent_email_domain_journal") {
  throw new AgentEmailDomainJournalError(message, code);
}

function isPlainObject(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return false;
  }
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function exactKeys(value, allowed, name) {
  if (!isPlainObject(value)) journalFail(`${name} must be an object`);
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) journalFail(`${name} contains unsupported field ${key}`);
  }
}

function canonicalize(value, depth, seen, budget) {
  if (depth > CANONICAL_MAX_DEPTH) {
    journalFail("canonical JSON exceeds the maximum depth");
  }
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || Object.is(value, -0)) {
      journalFail("canonical JSON numbers must be safe integers other than negative zero");
    }
    return String(value);
  }
  if (typeof value !== "object") {
    journalFail("canonical JSON contains an unsupported value");
  }
  if (seen.has(value)) journalFail("canonical JSON must not contain cycles");
  seen.add(value);
  try {
    if (Array.isArray(value)) {
      budget.count += value.length;
      if (budget.count > CANONICAL_MAX_ITEMS) {
        journalFail("canonical JSON exceeds the maximum item count");
      }
      for (let index = 0; index < value.length; index += 1) {
        if (!Object.prototype.hasOwnProperty.call(value, index)) {
          journalFail("canonical JSON arrays must not contain holes");
        }
      }
      return `[${value.map((item) =>
        canonicalize(item, depth + 1, seen, budget)).join(",")}]`;
    }
    if (!isPlainObject(value)) {
      journalFail("canonical JSON objects must have a plain prototype");
    }
    const keys = Object.keys(value).sort();
    budget.count += keys.length;
    if (budget.count > CANONICAL_MAX_ITEMS) {
      journalFail("canonical JSON exceeds the maximum item count");
    }
    return `{${keys.map((key) => {
      const encoded = canonicalize(value[key], depth + 1, seen, budget);
      return `${JSON.stringify(key)}:${encoded}`;
    }).join(",")}}`;
  } finally {
    seen.delete(value);
  }
}

export function canonicalJSONString(value) {
  return canonicalize(value, 0, new Set(), { count: 0 });
}

export function canonicalJSONBytes(value) {
  return new TextEncoder().encode(canonicalJSONString(value));
}

function asBytes(value) {
  if (typeof value === "string") return new TextEncoder().encode(value);
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (ArrayBuffer.isView(value)) {
    return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  }
  journalFail("SHA-256 input must be text or bytes");
}

export async function sha256Hex(value) {
  const digest = await globalThis.crypto.subtle.digest("SHA-256", asBytes(value));
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

function validateStreamID(streamID) {
  if (!STREAM_ID_PATTERN.test(streamID ?? "")) {
    journalFail("journal stream_id is invalid");
  }
  return streamID;
}

function sequenceSegment(sequence) {
  if (!Number.isSafeInteger(sequence) || sequence < 1) {
    journalFail("journal sequence must be a positive safe integer");
  }
  return String(sequence).padStart(20, "0");
}

export function agentEmailDomainJournalEntryKey(streamID, sequence) {
  return `${AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_PREFIX}/streams/` +
    `${validateStreamID(streamID)}/entries/${sequenceSegment(sequence)}.json`;
}

export function classifyAgentEmailDomainStorageKey(key) {
  if (typeof key !== "string" || key.length === 0 ||
      key.length > STORAGE_KEY_MAX_LENGTH) {
    return "unknown";
  }
  if (AUTHORITY_EXACT_KEYS.has(key) ||
      AUTHORITY_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "authority";
  }
  if (DERIVED_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "derived";
  }
  if (JOURNAL_LOCAL_EXACT_KEYS.has(key) ||
      JOURNAL_LOCAL_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "journal_local";
  }
  return "unknown";
}

export function isAgentEmailDomainAuthorityKey(key) {
  return classifyAgentEmailDomainStorageKey(key) === "authority";
}

export function isAgentEmailDomainDerivedKey(key) {
  return classifyAgentEmailDomainStorageKey(key) === "derived";
}

function normalizedAfterImage(afterImage, options = {}) {
  exactKeys(afterImage, new Set(["puts", "deletes"]), "after_image");
  if (!Array.isArray(afterImage.puts) || !Array.isArray(afterImage.deletes)) {
    journalFail("after_image puts and deletes must be arrays");
  }
  const maximum = options.max_changes ??
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_MAX_CHANGES;
  if (!Number.isSafeInteger(maximum) || maximum < 1 ||
      maximum > AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_MAX_CHANGES) {
    journalFail("after_image maximum is invalid");
  }
  if (afterImage.puts.length + afterImage.deletes.length > maximum) {
    journalFail("after_image exceeds the bounded change limit");
  }
  const seen = new Set();
  const puts = afterImage.puts.map((put) => {
    exactKeys(put, new Set(["key", "value"]), "after_image put");
    if (!isAgentEmailDomainAuthorityKey(put.key)) {
      journalFail(`after_image key is not canonical authority: ${String(put.key)}`);
    }
    if (seen.has(put.key)) journalFail("after_image contains a duplicate key");
    seen.add(put.key);
    const value = JSON.parse(canonicalJSONString(put.value));
    if (put.key.startsWith("request:")) validRequestRecord(value);
    else if (put.key.startsWith("domain:")) validAllocationRecord(value);
    else if (put.key.startsWith("plan-fence:") ||
        put.key.startsWith("plan-intent:") ||
        put.key.startsWith("lifecycle-fence:") ||
        put.key.startsWith("lifecycle-intent:")) {
      validPolicyRecord(put.key, value);
    }
    return {
      key: put.key,
      value,
    };
  }).sort((left, right) => left.key.localeCompare(right.key));
  for (const key of afterImage.deletes) {
    if (!isAgentEmailDomainAuthorityKey(key)) {
      journalFail(`after_image key is not canonical authority: ${String(key)}`);
    }
    if (!DELETABLE_AUTHORITY_PREFIXES.some((prefix) => key.startsWith(prefix))) {
      journalFail(`after_image cannot delete canonical authority key: ${key}`);
    }
    if (seen.has(key)) journalFail("after_image contains a duplicate key");
    seen.add(key);
  }
  return { puts, deletes: [...afterImage.deletes].sort() };
}

export function validateAgentEmailDomainAuthorityAfterImage(
  afterImage,
  options = {},
) {
  return normalizedAfterImage(afterImage, options);
}

function validateISODate(value, name) {
  if (typeof value !== "string" || value.length === 0 ||
      value.length > ISO_DATE_MAX_LENGTH || !Number.isFinite(Date.parse(value))) {
    journalFail(`${name} is invalid`);
  }
  return value;
}

function normalizeEntryInput(input, includeHash) {
  exactKeys(input, new Set([
    "schema_version", "stream_id", "kind", "authority_epoch", "sequence",
    "previous_hash", "registry_revision", "audit_sequence", "occurred_at",
    "operation_id", "operation_fingerprint", "actor", "action", "target",
    "metadata", "after_image", ...(includeHash ? ["entry_hash"] : []),
  ]), "journal entry");
  if (input.schema_version !== undefined &&
      input.schema_version !== AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION) {
    journalFail("journal schema_version is unsupported");
  }
  const streamID = validateStreamID(input.stream_id);
  if (!ENTRY_KINDS.has(input.kind)) journalFail("journal entry kind is invalid");
  if (!Number.isSafeInteger(input.authority_epoch) || input.authority_epoch < 1) {
    journalFail("journal authority_epoch is invalid");
  }
  const sequence = Number(input.sequence);
  sequenceSegment(sequence);
  if (!SHA256_PATTERN.test(input.previous_hash ?? "")) {
    journalFail("journal previous_hash is invalid");
  }
  for (const [name, value] of [
    ["registry_revision", input.registry_revision],
    ["audit_sequence", input.audit_sequence],
  ]) {
    if (!Number.isSafeInteger(value) || value < 0) {
      journalFail(`journal ${name} is invalid`);
    }
  }
  validateISODate(input.occurred_at, "journal occurred_at");
  if (!OPERATION_ID_PATTERN.test(input.operation_id ?? "")) {
    journalFail("journal operation_id is invalid");
  }
  if (!SHA256_PATTERN.test(input.operation_fingerprint ?? "")) {
    journalFail("journal operation_fingerprint is invalid");
  }
  exactKeys(input.actor, new Set(["kind", "id"]), "journal actor");
  if (!ACTOR_KINDS.has(input.actor.kind) ||
      !ACTOR_ID_PATTERN.test(input.actor.id ?? "")) {
    journalFail("journal actor is invalid");
  }
  for (const [name, value, maximum] of [
    ["action", input.action, 128],
    ["target", input.target, 512],
  ]) {
    if (typeof value !== "string" || value.length < 1 || value.length > maximum) {
      journalFail(`journal ${name} is invalid`);
    }
  }
  const metadata = input.metadata === undefined
    ? {}
    : JSON.parse(canonicalJSONString(input.metadata));
  if (!isPlainObject(metadata)) journalFail("journal metadata must be an object");
  const normalized = {
    schema_version: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION,
    stream_id: streamID,
    kind: input.kind,
    authority_epoch: input.authority_epoch,
    sequence,
    previous_hash: input.previous_hash,
    registry_revision: input.registry_revision,
    audit_sequence: input.audit_sequence,
    occurred_at: input.occurred_at,
    operation_id: input.operation_id,
    operation_fingerprint: input.operation_fingerprint,
    actor: { kind: input.actor.kind, id: input.actor.id },
    action: input.action,
    target: input.target,
    metadata,
    after_image: normalizedAfterImage(input.after_image),
  };
  if (includeHash) {
    if (!SHA256_PATTERN.test(input.entry_hash ?? "")) {
      journalFail("journal entry_hash is invalid");
    }
    normalized.entry_hash = input.entry_hash;
  }
  return normalized;
}

function assertExpectedEntry(entry, expected = {}) {
  for (const [name, value] of [
    ["stream_id", expected.stream_id],
    ["sequence", expected.sequence],
    ["previous_hash", expected.previous_hash],
    ["authority_epoch", expected.authority_epoch],
  ]) {
    if (value !== undefined && entry[name] !== value) {
      journalFail(`journal ${name} does not match the expected fence`,
        "agent_email_domain_journal_fence_mismatch");
    }
  }
}

function encodedEntryResult(entry, bytes, hash) {
  if (bytes.byteLength > ENTRY_MAX_BYTES) {
    journalFail("journal entry exceeds the maximum encoded size");
  }
  return {
    entry,
    bytes,
    hash,
    key: agentEmailDomainJournalEntryKey(entry.stream_id, entry.sequence),
  };
}

export async function buildAgentEmailDomainJournalEntry(input) {
  const unsigned = normalizeEntryInput(input, false);
  const hash = await sha256Hex(canonicalJSONBytes(unsigned));
  const entry = { ...unsigned, entry_hash: hash };
  return encodedEntryResult(entry, canonicalJSONBytes(entry), hash);
}

export async function validateAgentEmailDomainJournalEntry(entry, expected = {}) {
  const normalized = normalizeEntryInput(entry, true);
  assertExpectedEntry(normalized, expected);
  const { entry_hash: claimedHash, ...unsigned } = normalized;
  const hash = await sha256Hex(canonicalJSONBytes(unsigned));
  if (hash !== claimedHash) {
    journalFail("journal entry hash is invalid",
      "agent_email_domain_journal_hash_mismatch");
  }
  const canonicalEntry = { ...unsigned, entry_hash: hash };
  return encodedEntryResult(
    canonicalEntry,
    canonicalJSONBytes(canonicalEntry),
    hash,
  );
}

async function r2ObjectBytes(object) {
  if (!object) return null;
  if (typeof object.arrayBuffer === "function") {
    return new Uint8Array(await object.arrayBuffer());
  }
  if (object.body && typeof new Response(object.body).arrayBuffer === "function") {
    return new Uint8Array(await new Response(object.body).arrayBuffer());
  }
  journalFail("existing journal object body is unreadable",
    "agent_email_domain_journal_unavailable");
}

function bytesEqual(left, right) {
  if (left.byteLength !== right.byteLength) return false;
  let different = 0;
  for (let index = 0; index < left.byteLength; index += 1) {
    different |= left[index] ^ right[index];
  }
  return different === 0;
}

async function resolveExistingAppend(bucket, built, cause = null) {
  let existing;
  try {
    existing = await bucket.get(built.key);
  } catch {
    journalFail(
      `agent email domain authority journal is unavailable${cause ? `: ${cause.message ?? cause}` : ""}`,
      "agent_email_domain_journal_unavailable",
    );
  }
  if (!existing) {
    journalFail(
      `agent email domain authority journal append failed${cause ? `: ${cause.message ?? cause}` : ""}`,
      "agent_email_domain_journal_unavailable",
    );
  }
  const bytes = await r2ObjectBytes(existing);
  if (!bytesEqual(bytes, built.bytes)) {
    journalFail("agent email domain authority journal fork detected",
      "agent_email_domain_journal_fork_detected");
  }
  return { created: false, replayed: true, object: existing };
}

export async function appendAgentEmailDomainJournalEntry(bucket, built) {
  if (!bucket || typeof bucket.put !== "function" ||
      typeof bucket.get !== "function") {
    journalFail("agent email domain authority journal binding is unavailable",
      "agent_email_domain_journal_unavailable");
  }
  if (!built || !(built.bytes instanceof Uint8Array) ||
      typeof built.key !== "string" || !isPlainObject(built.entry)) {
    journalFail("built journal entry is invalid");
  }
  const verified = await validateAgentEmailDomainJournalEntry(built.entry);
  if (verified.key !== built.key || !bytesEqual(verified.bytes, built.bytes)) {
    journalFail("built journal entry is not canonical");
  }
  let object;
  try {
    object = await bucket.put(built.key, built.bytes, {
      onlyIf: new Headers({ "If-None-Match": "*" }),
      httpMetadata: { contentType: "application/json" },
      customMetadata: {
        schema: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_SCHEMA_VERSION,
        stream_id: built.entry.stream_id,
        sequence: String(built.entry.sequence),
        entry_hash: built.entry.entry_hash,
      },
    });
  } catch (error) {
    return resolveExistingAppend(bucket, built, error);
  }
  if (object === null) return resolveExistingAppend(bucket, built);
  return { created: true, replayed: false, object };
}

function toAuthorityStateMap(state) {
  const source = state instanceof Map
    ? [...state]
    : isPlainObject(state)
    ? Object.entries(state)
    : null;
  if (!source) journalFail("authority state must be a Map or plain object");
  const result = new Map();
  for (const [key, value] of source) {
    if (!isAgentEmailDomainAuthorityKey(key)) {
      journalFail(`authority state contains non-authority key: ${String(key)}`);
    }
    if (result.has(key)) journalFail("authority state contains a duplicate key");
    result.set(key, JSON.parse(canonicalJSONString(value)));
  }
  return result;
}

function immutableFieldsChanged(previous, desired, fields) {
  return fields.some((field) =>
    canonicalJSONString(previous?.[field] ?? null) !==
      canonicalJSONString(desired?.[field] ?? null));
}

function assertRequestTransition(previous, desired) {
  if (immutableFieldsChanged(previous, desired, [
    "schema_version", "id", "account_id", "domain", "ownership_challenge",
    "requested_by", "requested_at", "domain_limit_at_request",
    "plan_revision", "plan_snapshot_hash",
  ])) {
    journalFail("custom domain request identity changed during replay",
      "agent_email_domain_recovery_invariant_failed");
  }
  const allowed = {
    pending_verification: new Set([
      "pending_verification", "verified", "rejected", "expired", "retired",
    ]),
    verified: new Set(["verified", "retired"]),
    rejected: new Set(["rejected", "retired"]),
    expired: new Set(["expired", "retired"]),
    retired: new Set(["retired"]),
  };
  if (!allowed[previous.state]?.has(desired.state)) {
    journalFail("custom domain tombstone was resurrected",
      "agent_email_domain_recovery_tombstone_resurrection");
  }
  const previousRevision = previous.state_revision ?? 1;
  const desiredRevision = desired.state_revision ?? 1;
  if (canonicalJSONString(previous) === canonicalJSONString(desired)) return;
  const legacyTransition = previous.state_revision === undefined &&
    desired.state_revision === undefined && previous.state !== desired.state;
  if (previous.state === desired.state &&
      previous.state_revision === undefined && desired.state_revision === undefined) {
    journalFail("same-state custom domain authority cannot be overwritten",
      "agent_email_domain_recovery_invariant_failed");
  }
  if (!legacyTransition &&
      (!Number.isSafeInteger(previousRevision) || previousRevision < 1 ||
        desiredRevision !== previousRevision + 1)) {
    journalFail("custom domain request revisions are not contiguous",
      "agent_email_domain_recovery_revision_regression");
  }
  if (Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
    journalFail("custom domain request update time regressed",
      "agent_email_domain_recovery_revision_regression");
  }
  if (previous.decision != null &&
      canonicalJSONString(previous.decision) !== canonicalJSONString(desired.decision)) {
    journalFail("custom domain rejection decision changed during replay",
      "agent_email_domain_recovery_invariant_failed");
  }
  if (previous.retirement != null &&
      canonicalJSONString(previous.retirement) !==
        canonicalJSONString(desired.retirement)) {
    journalFail("custom domain retirement changed during replay",
        "agent_email_domain_recovery_invariant_failed");
  }
  if (previous.expiration != null &&
      canonicalJSONString(previous.expiration) !==
        canonicalJSONString(desired.expiration)) {
    journalFail("custom domain expiration changed during replay",
      "agent_email_domain_recovery_invariant_failed");
  }
  if (previous.ownership_verification?.first_verified_at &&
      previous.ownership_verification.first_verified_at !==
        desired.ownership_verification?.first_verified_at) {
    journalFail("custom domain first verification changed during replay",
      "agent_email_domain_recovery_invariant_failed");
  }
}

function assertAllocationTransition(previous, desired) {
  if (previous?.schema_version === "witself.agent-email-domain.v1") {
    if (desired?.schema_version === "witself.agent-email-domain.v1") {
      assertRequestTransition(previous, desired);
      return;
    }
    if (desired?.schema_version !==
          "witself.agent-email-domain-allocation.v1" ||
        previous.domain !== desired.domain ||
        previous.account_id !== desired.account_id ||
        previous.id !== desired.source_request_id ||
        desired.state !== "allocated" || desired.generation !== 1 ||
        desired.allocation_revision !== 1 ||
        !Number.isFinite(Date.parse(desired.allocated_at)) ||
        Date.parse(desired.allocated_at) < Date.parse(previous.updated_at)) {
      journalFail("legacy custom domain mirror conversion is invalid",
        "agent_email_domain_recovery_invariant_failed");
    }
    return;
  }
  if (immutableFieldsChanged(previous, desired, [
    "schema_version", "domain", "account_id", "source_request_id",
    "generation", "allocated_at",
  ])) {
    journalFail("custom domain allocation identity changed during replay",
      "agent_email_domain_recovery_invariant_failed");
  }
  if (canonicalJSONString(previous) === canonicalJSONString(desired)) return;
  if (!Number.isSafeInteger(previous.allocation_revision) ||
      desired.allocation_revision !== previous.allocation_revision + 1) {
    journalFail("custom domain allocation revisions are not contiguous",
      "agent_email_domain_recovery_revision_regression");
  }
  if (previous.state === "retired" ||
      (previous.state !== "allocated" ||
        !["allocated", "retired"].includes(desired.state))) {
    journalFail("custom domain allocation tombstone was resurrected",
      "agent_email_domain_recovery_tombstone_resurrection");
  }
  if (Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
    journalFail("custom domain allocation update time regressed",
      "agent_email_domain_recovery_revision_regression");
  }
}

function lifecycleActionOrder(action) {
  return action === "suspend" ? 1 : action === "republish" ? 2 :
    action === "retire" ? 3 : 0;
}

function assertAuthorityTransition(state, key, desired) {
  const previous = state.get(key);
  if (!previous) return;
  if (key.startsWith("audit:") || key.startsWith("idem:")) {
    if (canonicalJSONString(previous) !== canonicalJSONString(desired)) {
      journalFail(`append-only authority key cannot be overwritten: ${key}`,
        "agent_email_domain_recovery_invariant_failed");
    }
    return;
  }
  if (key === "meta") {
    if (previous.schema_version !== desired.schema_version ||
        previous.created_at !== desired.created_at) {
      journalFail("custom domain registry identity changed during replay",
        "agent_email_domain_recovery_invariant_failed");
    }
    const unchanged = desired.registry_revision === previous.registry_revision &&
      desired.audit_sequence === previous.audit_sequence;
    const next = desired.registry_revision === previous.registry_revision + 1 &&
      desired.audit_sequence === previous.audit_sequence + 1;
    if (!unchanged && !next) {
      journalFail("custom domain registry revisions are not contiguous",
        "agent_email_domain_recovery_revision_regression");
    }
    if (unchanged && canonicalJSONString(previous) !== canonicalJSONString(desired)) {
      journalFail("same-revision custom domain meta cannot be overwritten",
        "agent_email_domain_recovery_invariant_failed");
    }
    return;
  }
  if (key.startsWith("request:")) {
    assertRequestTransition(previous, desired);
    return;
  }
  if (key.startsWith("domain:")) {
    assertAllocationTransition(previous, desired);
    return;
  }
  if (key.startsWith("plan-intent:")) {
    if (previous.account_id !== desired.account_id ||
        desired.plan_revision < previous.plan_revision ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("custom domain plan intent regressed",
        "agent_email_domain_recovery_revision_regression");
    }
    if (previous.plan_revision === desired.plan_revision) {
      const previousState = previous.state === "awaiting_cell" ? 1 : 2;
      const desiredState = desired.state === "awaiting_cell" ? 1 : 2;
      if (previous.plan_snapshot_hash !== desired.plan_snapshot_hash ||
          previous.feature_enabled !== desired.feature_enabled ||
          previous.domain_limit !== desired.domain_limit ||
          previous.created_at !== desired.created_at ||
          desiredState < previousState ||
          desired.position < previous.position ||
          (previous.cursor !== null &&
            (desired.cursor === null || desired.cursor < previous.cursor)) ||
          desired.retry_at_ms < previous.retry_at_ms ||
          (previous.state === desired.state &&
            previous.cursor === desired.cursor &&
            desired.failure_count < previous.failure_count)) {
        journalFail("custom domain plan intent identity changed",
          "agent_email_domain_recovery_invariant_failed");
      }
    }
    return;
  }
  if (key.startsWith("plan-fence:")) {
    if (previous.account_id !== desired.account_id ||
        desired.committed_revision < previous.committed_revision ||
        (desired.committed_revision === previous.committed_revision &&
          canonicalJSONString(previous) !== canonicalJSONString(desired))) {
      journalFail("custom domain plan fence regressed",
        "agent_email_domain_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("lifecycle-intent:")) {
    if (previous.account_id !== desired.account_id ||
        previous.operation_id !== desired.operation_id ||
        previous.epoch !== desired.epoch || previous.action !== desired.action ||
        previous.created_at !== desired.created_at ||
        (previous.cursor !== null &&
          (desired.cursor === null || desired.cursor < previous.cursor)) ||
        desired.retry_at_ms < previous.retry_at_ms ||
        (previous.cursor === desired.cursor &&
          desired.failure_count < previous.failure_count) ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("custom domain lifecycle intent changed identity",
        "agent_email_domain_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("lifecycle-fence:")) {
    if (previous.account_id !== desired.account_id ||
        desired.epoch < previous.epoch ||
        (desired.epoch === previous.epoch &&
          (previous.operation_id !== desired.operation_id ||
            lifecycleActionOrder(desired.action) <
              lifecycleActionOrder(previous.action) ||
            (desired.action === previous.action &&
              canonicalJSONString(previous) !==
                canonicalJSONString(desired))))) {
      journalFail("custom domain lifecycle fence regressed",
        "agent_email_domain_recovery_revision_regression");
    }
    return;
  }
}

function applyNormalizedAfterImage(state, normalized) {
  const puts = new Map(normalized.puts.map(({ key, value }) => [key, value]));
  for (const key of normalized.deletes) {
    const intent = state.get(key);
    if (key.startsWith("plan-intent:")) {
      const accountID = key.slice("plan-intent:".length);
      const fence = puts.get(`plan-fence:${accountID}`);
      if (!intent || !fence || fence.account_id !== intent.account_id ||
          fence.committed_revision !== intent.plan_revision ||
          fence.committed_snapshot_hash !== intent.plan_snapshot_hash ||
          fence.feature_enabled !== intent.feature_enabled ||
          fence.domain_limit !== intent.domain_limit ||
          Date.parse(fence.updated_at) < Date.parse(intent.updated_at)) {
        journalFail("custom domain plan intent deletion lost convergence",
          "agent_email_domain_recovery_invariant_failed");
      }
    } else if (key.startsWith("lifecycle-intent:")) {
      const accountID = key.slice("lifecycle-intent:".length);
      const fence = puts.get(`lifecycle-fence:${accountID}`);
      if (!intent || !fence || fence.account_id !== intent.account_id ||
          fence.operation_id !== intent.operation_id ||
          fence.epoch !== intent.epoch || fence.action !== intent.action ||
          Date.parse(fence.completed_at) < Date.parse(intent.updated_at)) {
        journalFail("custom domain lifecycle intent deletion lost convergence",
          "agent_email_domain_recovery_invariant_failed");
      }
    }
  }
  for (const { key, value } of normalized.puts) {
    assertAuthorityTransition(state, key, value);
    state.set(key, JSON.parse(canonicalJSONString(value)));
  }
  for (const key of normalized.deletes) state.delete(key);
}

export function applyAgentEmailDomainAuthorityAfterImage(state, afterImage) {
  const result = toAuthorityStateMap(state);
  applyNormalizedAfterImage(result, normalizedAfterImage(afterImage));
  return result;
}

export function buildAgentEmailDomainBootstrapPage(state, options = {}) {
  const source = toAuthorityStateMap(state);
  const limit = options.limit ?? AGENT_EMAIL_DOMAIN_AUTHORITY_BOOTSTRAP_PAGE_LIMIT;
  if (!Number.isSafeInteger(limit) || limit < 1 ||
      limit > AGENT_EMAIL_DOMAIN_AUTHORITY_BOOTSTRAP_PAGE_LIMIT) {
    journalFail("bootstrap page limit is invalid");
  }
  if (options.cursor !== undefined && options.cursor !== null &&
      (typeof options.cursor !== "string" || options.cursor.length === 0 ||
        options.cursor.length > STORAGE_KEY_MAX_LENGTH)) {
    journalFail("bootstrap cursor is invalid");
  }
  const keys = [...source.keys()].sort()
    .filter((key) => options.cursor == null || key > options.cursor);
  const pageKeys = keys.slice(0, limit);
  return {
    after_image: {
      puts: pageKeys.map((key) => ({ key, value: source.get(key) })),
      deletes: [],
    },
    count: pageKeys.length,
    complete: keys.length <= limit,
    next_cursor: keys.length <= limit || pageKeys.length === 0
      ? null
      : pageKeys.at(-1),
  };
}

export async function agentEmailDomainAuthorityStateDigest(state) {
  const rows = [...toAuthorityStateMap(state)]
    .sort(([left], [right]) => left.localeCompare(right));
  let digest = await sha256Hex(canonicalJSONBytes({
    schema_version: "witself.agent-email-domain-authority-state.v1",
    rows: 0,
  }));
  let count = 0;
  for (const [key, value] of rows) {
    count += 1;
    digest = await sha256Hex(canonicalJSONBytes({
      schema_version: "witself.agent-email-domain-authority-state-row.v1",
      previous_hash: digest,
      row_number: count,
      key,
      value,
    }));
  }
  return sha256Hex(canonicalJSONBytes({
    schema_version: "witself.agent-email-domain-authority-state-final.v1",
    rows: count,
    rows_hash: digest,
  }));
}

export async function replayAgentEmailDomainJournalPage(entries, options = {}) {
  if (!Array.isArray(entries)) journalFail("journal replay page must be an array");
  const maximum = options.max_entries ??
    AGENT_EMAIL_DOMAIN_AUTHORITY_BOOTSTRAP_PAGE_LIMIT;
  if (!Number.isSafeInteger(maximum) || maximum < 1 ||
      maximum > AGENT_EMAIL_DOMAIN_AUTHORITY_BOOTSTRAP_PAGE_LIMIT ||
      entries.length > maximum) {
    journalFail("journal replay page exceeds the bounded entry limit");
  }
  const streamID = validateStreamID(options.stream_id);
  const state = toAuthorityStateMap(options.state ?? new Map());
  let head = options.head ?? {
    sequence: 0,
    hash: AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_GENESIS_HASH,
    authority_epoch: null,
  };
  if (!Number.isSafeInteger(head.sequence) || head.sequence < 0 ||
      !SHA256_PATTERN.test(head.hash ?? "") ||
      (head.authority_epoch !== null &&
        (!Number.isSafeInteger(head.authority_epoch) || head.authority_epoch < 1))) {
    journalFail("journal replay head is invalid");
  }
  for (const raw of entries) {
    const built = await validateAgentEmailDomainJournalEntry(raw, {
      stream_id: streamID,
      sequence: head.sequence + 1,
      previous_hash: head.hash,
    });
    if (head.authority_epoch !== null) {
      if (built.entry.authority_epoch !== head.authority_epoch) {
        journalFail("journal authority_epoch is not contiguous",
          "agent_email_domain_journal_fence_mismatch");
      }
    }
    applyNormalizedAfterImage(state, built.entry.after_image);
    const meta = state.get("meta");
    if (meta && (meta.registry_revision !== built.entry.registry_revision ||
        meta.audit_sequence !== built.entry.audit_sequence)) {
      journalFail("journal entry revisions do not match its meta after-image",
        "agent_email_domain_recovery_revision_regression");
    }
    head = {
      sequence: built.entry.sequence,
      hash: built.entry.entry_hash,
      authority_epoch: built.entry.authority_epoch,
      registry_revision: built.entry.registry_revision,
      audit_sequence: built.entry.audit_sequence,
    };
  }
  return { state, head, applied: entries.length };
}

function validDomain(domain) {
  return typeof domain === "string" && domain === domain.toLowerCase() &&
    domain.length >= 3 && domain.length <= 231 &&
    /^[\x00-\x7f]+$/.test(domain) && !domain.includes("*") &&
    domain.split(".").length >= 2 &&
    domain.split(".").every((label) =>
      DOMAIN_LABEL_PATTERN.test(label) && !label.startsWith("xn--")) &&
    /[a-z]/.test(domain.split(".").at(-1));
}

function recoveryFail(message, code = "agent_email_domain_recovery_invariant_failed") {
  journalFail(message, code);
}

function assertObject(value, message) {
  if (!isPlainObject(value)) recoveryFail(message);
}

function validRequestRecord(value, options = {}) {
  assertObject(value, "recovered custom domain request is invalid");
  if ((!options.public_record &&
        value.schema_version !== "witself.agent-email-domain.v1") ||
      (options.public_record && value.schema_version !== undefined) ||
      !REQUEST_ID_PATTERN.test(value.id ?? "") ||
      !ACCOUNT_ID_PATTERN.test(value.account_id ?? "") ||
      !validDomain(value.domain) ||
      !["pending_verification", "verified", "rejected", "expired", "retired"]
        .includes(value.state) ||
      !ACTOR_ID_PATTERN.test(value.requested_by ?? "") ||
      !Number.isSafeInteger(value.plan_revision) || value.plan_revision < 0 ||
      !(value.plan_revision === 0
        ? value.plan_snapshot_hash === ""
        : SHA256_PATTERN.test(value.plan_snapshot_hash ?? "")) ||
      !(value.state_revision === undefined ||
        (Number.isSafeInteger(value.state_revision) &&
          value.state_revision >= 1)) ||
      !(value.domain_limit_at_request === null ||
        (Number.isSafeInteger(value.domain_limit_at_request) &&
          value.domain_limit_at_request >= 0))) {
    recoveryFail("recovered custom domain request is invalid");
  }
  validateISODate(value.requested_at, "request requested_at");
  validateISODate(value.updated_at, "request updated_at");
  if (Date.parse(value.updated_at) < Date.parse(value.requested_at)) {
    recoveryFail("recovered custom domain request time regressed",
      "agent_email_domain_recovery_revision_regression");
  }
  const challenge = value.ownership_challenge;
  if (!isPlainObject(challenge) || challenge.record_type !== "TXT" ||
      challenge.record_name !== `_witself-verification.${value.domain}` ||
      typeof challenge.record_value !== "string" ||
      !challenge.record_value.startsWith("witself-domain-verification=") ||
      !CHALLENGE_TOKEN_PATTERN.test(
        challenge.record_value.slice("witself-domain-verification=".length),
      ) || challenge.issued_at !== value.requested_at ||
      (challenge.expires_at !== undefined &&
        (!Number.isFinite(Date.parse(challenge.expires_at)) ||
          Date.parse(challenge.expires_at) !==
            Date.parse(challenge.issued_at) + PENDING_CHALLENGE_TTL_MS))) {
    recoveryFail("recovered custom domain ownership challenge is invalid");
  }
  const decision = value.decision;
  if (decision !== null && decision !== undefined) {
    if (!isPlainObject(decision) || decision.action !== "rejected" ||
        typeof decision.reason !== "string" || decision.reason.length < 1 ||
        decision.reason.length > 500 ||
        !ACTOR_ID_PATTERN.test(decision.decided_by ?? "")) {
      recoveryFail("recovered custom domain decision is invalid");
    }
    validateISODate(decision.decided_at, "decision decided_at");
    if (Date.parse(decision.decided_at) < Date.parse(value.requested_at) ||
        Date.parse(decision.decided_at) > Date.parse(value.updated_at)) {
      recoveryFail("recovered custom domain decision time is invalid");
    }
  }
  const retirement = value.retirement;
  if (retirement !== null && retirement !== undefined) {
    if (!isPlainObject(retirement) ||
        typeof retirement.reason !== "string" || retirement.reason.length < 1 ||
        retirement.reason.length > 500 ||
        !ACTOR_ID_PATTERN.test(retirement.retired_by ?? "")) {
      recoveryFail("recovered custom domain retirement is invalid");
    }
    validateISODate(retirement.retired_at, "retirement retired_at");
    if (Date.parse(retirement.retired_at) < Date.parse(value.requested_at) ||
        Date.parse(retirement.retired_at) > Date.parse(value.updated_at)) {
      recoveryFail("recovered custom domain retirement time is invalid");
    }
  }
  const expiration = value.expiration;
  if (expiration !== null && expiration !== undefined) {
    if (!isPlainObject(expiration) ||
        expiration.reason !== "ownership challenge expired") {
      recoveryFail("recovered custom domain expiration is invalid");
    }
    validateISODate(expiration.expired_at, "expiration expired_at");
    const challengeDeadline = challenge.expires_at ?? new Date(
      Date.parse(value.requested_at) + PENDING_CHALLENGE_TTL_MS,
    ).toISOString();
    if (Date.parse(expiration.expired_at) < Date.parse(challengeDeadline)) {
      recoveryFail("recovered custom domain expiration preceded its challenge");
    }
  }
  if (value.plan_suspended !== undefined &&
      typeof value.plan_suspended !== "boolean") {
    recoveryFail("recovered custom domain plan suspension is invalid");
  }
  if (value.lifecycle_suspended !== undefined &&
      typeof value.lifecycle_suspended !== "boolean") {
    recoveryFail("recovered custom domain lifecycle suspension is invalid");
  }
  const lifecycleFence = value.lifecycle_fence;
  if (lifecycleFence !== null && lifecycleFence !== undefined) {
    if (!isPlainObject(lifecycleFence)) {
      recoveryFail("recovered custom domain lifecycle fence is invalid");
    }
    exactKeys(lifecycleFence, new Set(["operation_id", "epoch", "action"]),
      "request lifecycle fence");
    if (!OPERATION_ID_PATTERN.test(lifecycleFence.operation_id ?? "") ||
        !Number.isSafeInteger(lifecycleFence.epoch) ||
        lifecycleFence.epoch < 0 ||
        !["suspend", "republish", "retire"].includes(lifecycleFence.action) ||
        (value.lifecycle_suspended === true &&
          !["suspend", "retire"].includes(lifecycleFence.action)) ||
        (value.lifecycle_suspended !== true &&
          lifecycleFence.action !== "republish")) {
      recoveryFail("recovered custom domain lifecycle fence is invalid");
    }
  } else if (value.lifecycle_suspended === true) {
    recoveryFail("recovered custom domain lifecycle fence is missing");
  }
  if (value.plan_grace_until != null) {
    validateISODate(value.plan_grace_until, "plan grace until");
    if (value.state !== "verified" || value.plan_suspended === true) {
      recoveryFail("recovered custom domain plan grace is invalid");
    }
  }
  const verification = value.ownership_verification;
  if (verification !== null && verification !== undefined) {
    if (!isPlainObject(verification) ||
        !["unverified", "missing", "verified", "stale", "conflict"].includes(
          verification.state,
        ) ||
        ![
          "resolver_error", "present", "absent", "domain_unavailable",
          "policy_converging",
        ].includes(
          verification.last_result,
        ) ||
        !Number.isSafeInteger(verification.consecutive_failures) ||
        verification.consecutive_failures < 0) {
      recoveryFail("recovered custom domain ownership verification is invalid");
    }
    validateISODate(verification.last_checked_at, "verification last_checked_at");
    validateISODate(verification.next_check_at, "verification next_check_at");
    for (const field of ["first_verified_at", "last_verified_at"]) {
      if (verification[field] != null) {
        validateISODate(verification[field], `verification ${field}`);
      }
    }
    if (verification.rrset_sha256 != null &&
        !SHA256_PATTERN.test(verification.rrset_sha256) ||
        typeof verification.dnssec_authenticated !== "boolean" ||
        !(verification.minimum_ttl_seconds === null ||
          (Number.isSafeInteger(verification.minimum_ttl_seconds) &&
            verification.minimum_ttl_seconds >= 0))) {
      recoveryFail("recovered custom domain ownership evidence is invalid");
    }
    const firstVerified = verification.first_verified_at;
    const lastVerified = verification.last_verified_at;
    const hasVerifiedHistory = firstVerified != null || lastVerified != null;
    if ((firstVerified == null) !== (lastVerified == null) ||
        Date.parse(verification.last_checked_at) <
          Date.parse(value.requested_at) ||
        Date.parse(verification.last_checked_at) >
          Date.parse(value.updated_at) ||
        Date.parse(verification.next_check_at) <=
          Date.parse(verification.last_checked_at) ||
        (hasVerifiedHistory &&
          (Date.parse(firstVerified) < Date.parse(value.requested_at) ||
            Date.parse(firstVerified) > Date.parse(lastVerified) ||
            Date.parse(lastVerified) > Date.parse(value.updated_at))) ||
        (hasVerifiedHistory &&
          !["verified", "stale"].includes(verification.state)) ||
        (!hasVerifiedHistory &&
          ["verified", "stale"].includes(verification.state)) ||
        (["pending_verification", "rejected", "expired"].includes(
          value.state,
        ) && hasVerifiedHistory) ||
        (value.state === "retired" && value.decision &&
          hasVerifiedHistory)) {
      recoveryFail("recovered custom domain verification history is invalid");
    }
    if (retirement &&
        (Date.parse(retirement.retired_at) <
            Date.parse(verification.last_checked_at) ||
          (lastVerified && Date.parse(retirement.retired_at) <
            Date.parse(lastVerified)))) {
      recoveryFail("recovered custom domain retirement time is invalid");
    }
  }
  if (retirement && decision &&
      Date.parse(retirement.retired_at) < Date.parse(decision.decided_at)) {
    recoveryFail("recovered custom domain retirement time is invalid");
  }
  if ((value.state === "pending_verification" &&
        (decision != null || retirement != null || expiration != null)) ||
      (value.state === "verified" &&
        (decision != null || retirement != null || expiration != null ||
          verification?.first_verified_at == null)) ||
      (value.state === "rejected" &&
        (decision == null || retirement != null || expiration != null)) ||
      (value.state === "expired" &&
        (expiration == null || decision != null || retirement != null)) ||
      (value.state === "retired" && retirement == null)) {
    recoveryFail("recovered custom domain lifecycle evidence is inconsistent");
  }
  return value;
}

function validAllocationRecord(value) {
  assertObject(value, "recovered custom domain allocation is invalid");
  // Compatibility for authority written by v0.0.235 before verified-only
  // allocations were separated from unverified request history.
  if (value.schema_version === "witself.agent-email-domain.v1" && value.id) {
    validRequestRecord(value);
    return value;
  }
  if (value.schema_version !== "witself.agent-email-domain-allocation.v1" ||
      !validDomain(value.domain) ||
      !ACCOUNT_ID_PATTERN.test(value.account_id ?? "") ||
      !REQUEST_ID_PATTERN.test(value.source_request_id ?? "") ||
      !Number.isSafeInteger(value.generation) || value.generation < 1 ||
      !Number.isSafeInteger(value.allocation_revision) ||
      value.allocation_revision < 1 ||
      !["allocated", "retired"].includes(value.state)) {
    recoveryFail("recovered custom domain allocation is invalid");
  }
  validateISODate(value.allocated_at, "allocation allocated_at");
  validateISODate(value.updated_at, "allocation updated_at");
  if (Date.parse(value.updated_at) < Date.parse(value.allocated_at) ||
      !isPlainObject(value.ownership_proof) ||
      !SHA256_PATTERN.test(value.ownership_proof.rrset_sha256 ?? "") ||
      typeof value.ownership_proof.dnssec_authenticated !== "boolean") {
    recoveryFail("recovered custom domain allocation evidence is invalid");
  }
  validateISODate(value.ownership_proof.verified_at,
    "allocation proof verified_at");
  if ((value.state === "allocated" && value.retirement != null) ||
      (value.state === "retired" && !isPlainObject(value.retirement))) {
    recoveryFail("recovered custom domain allocation lifecycle is invalid");
  }
  if (value.retirement) {
    validateISODate(value.retirement.retired_at,
      "allocation retirement retired_at");
  }
  return value;
}

function validPolicyRecord(key, value) {
  assertObject(value, `recovered custom domain policy record is invalid: ${key}`);
  const accountID = key.slice(key.indexOf(":") + 1);
  if (!ACCOUNT_ID_PATTERN.test(accountID) || value.account_id !== accountID) {
    recoveryFail(`recovered custom domain policy key is invalid: ${key}`);
  }
  if (key.startsWith("plan-fence:")) {
    exactKeys(value, new Set([
      "account_id", "committed_revision", "committed_snapshot_hash",
      "feature_enabled", "domain_limit", "updated_at",
    ]), "custom domain plan fence");
    if (!Number.isSafeInteger(value.committed_revision) ||
        value.committed_revision < 0 ||
        !(value.committed_revision === 0
          ? value.committed_snapshot_hash === ""
          : SHA256_PATTERN.test(value.committed_snapshot_hash ?? "")) ||
        typeof value.feature_enabled !== "boolean" ||
        !(value.domain_limit === null ||
          (Number.isSafeInteger(value.domain_limit) && value.domain_limit >= 0))) {
      recoveryFail(`recovered custom domain plan fence is invalid: ${key}`);
    }
    validateISODate(value.updated_at, "plan fence updated_at");
  } else if (key.startsWith("plan-intent:")) {
    exactKeys(value, new Set([
      "account_id", "plan_revision", "plan_snapshot_hash",
      "feature_enabled", "domain_limit", "state", "cursor", "position",
      "failure_count", "retry_at_ms", "created_at", "updated_at",
    ]), "custom domain plan intent");
    if (!Number.isSafeInteger(value.plan_revision) || value.plan_revision < 0 ||
        !(value.plan_revision === 0
          ? value.plan_snapshot_hash === ""
          : SHA256_PATTERN.test(value.plan_snapshot_hash ?? "")) ||
        !["awaiting_cell", "cell_committed"].includes(value.state) ||
        typeof value.feature_enabled !== "boolean" ||
        !(value.domain_limit === null ||
          (Number.isSafeInteger(value.domain_limit) && value.domain_limit >= 0)) ||
        !Number.isSafeInteger(value.position) || value.position < 0 ||
        !Number.isSafeInteger(value.failure_count) || value.failure_count < 0 ||
        !(value.cursor === null ||
          (typeof value.cursor === "string" &&
            value.cursor.startsWith(`account-domain:${accountID}:`))) ||
        !(value.state === "awaiting_cell"
          ? value.retry_at_ms === null && value.cursor === null &&
            value.position === 0
          : Number.isSafeInteger(value.retry_at_ms) &&
            value.retry_at_ms >= 0)) {
      recoveryFail(`recovered custom domain plan intent is invalid: ${key}`);
    }
    validateISODate(value.created_at, "plan intent created_at");
    validateISODate(value.updated_at, "plan intent updated_at");
    if (Date.parse(value.updated_at) < Date.parse(value.created_at)) {
      recoveryFail(`recovered custom domain plan intent time regressed: ${key}`,
        "agent_email_domain_recovery_revision_regression");
    }
  } else if (key.startsWith("lifecycle-fence:") ||
      key.startsWith("lifecycle-intent:")) {
    const intent = key.startsWith("lifecycle-intent:");
    exactKeys(value, new Set(intent
      ? [
        "account_id", "operation_id", "epoch", "action", "cursor",
        "failure_count", "retry_at_ms", "created_at", "updated_at",
      ]
      : [
        "account_id", "operation_id", "epoch", "action", "completed_at",
      ]), intent
      ? "custom domain lifecycle intent"
      : "custom domain lifecycle fence");
    if (!OPERATION_ID_PATTERN.test(value.operation_id ?? "") ||
        !Number.isSafeInteger(value.epoch) || value.epoch < 0 ||
        !["suspend", "republish", "retire"].includes(value.action)) {
      recoveryFail(`recovered custom domain lifecycle record is invalid: ${key}`);
    }
    if (intent) {
      if (!(value.cursor === null ||
            (typeof value.cursor === "string" &&
              value.cursor.startsWith(`account-domain:${accountID}:`))) ||
          !Number.isSafeInteger(value.failure_count) || value.failure_count < 0 ||
          !Number.isSafeInteger(value.retry_at_ms) || value.retry_at_ms < 0) {
        recoveryFail(`recovered custom domain lifecycle intent is invalid: ${key}`);
      }
      validateISODate(value.created_at, "lifecycle intent created_at");
      validateISODate(value.updated_at, "lifecycle intent updated_at");
      if (Date.parse(value.updated_at) < Date.parse(value.created_at)) {
        recoveryFail(`recovered custom domain lifecycle intent time regressed: ${key}`,
          "agent_email_domain_recovery_revision_regression");
      }
    } else {
      validateISODate(value.completed_at, "lifecycle fence completed_at");
    }
  }
  return value;
}

function validateAudit(key, value, meta) {
  const match = key.match(/^audit:(\d{12})$/);
  const sequence = match ? Number(match[1]) : 0;
  if (!match || !Number.isSafeInteger(sequence) || sequence < 1 ||
      !isPlainObject(value) || value.sequence !== sequence ||
      value.registry_revision !== sequence ||
      value.registry_revision > meta.registry_revision ||
      !ACTOR_KINDS.has(value.actor_kind) ||
      !ACTOR_ID_PATTERN.test(value.actor_id ?? "") ||
      ![
        "custom_domain.requested", "custom_domain.rejected",
        "custom_domain.retired", "custom_domain.verified",
        "custom_domain.reverified", "custom_domain.verification_missing",
        "custom_domain.verification_deferred",
        "custom_domain.verification_conflict", "custom_domain.expired",
        "custom_domain.plan_reconciled",
        "custom_domain.plan_grace_expired",
        "custom_domain.lifecycle_suspend",
        "custom_domain.lifecycle_republish",
        "custom_domain.lifecycle_retire",
      ].includes(value.action) ||
      !isPlainObject(value.metadata) ||
      !ACCOUNT_ID_PATTERN.test(value.metadata.account_id ?? "")) {
    recoveryFail(`recovered custom domain audit event is invalid: ${key}`,
      "agent_email_domain_recovery_revision_regression");
  }
  const accountTarget = value.action === "custom_domain.plan_reconciled" ||
    value.action.startsWith("custom_domain.lifecycle_");
  if (accountTarget
    ? value.target !== value.metadata.account_id
    : !validDomain(value.target)) {
    recoveryFail(`recovered custom domain audit target is invalid: ${key}`,
      "agent_email_domain_recovery_revision_regression");
  }
  validateISODate(value.occurred_at, "audit occurred_at");
  if (Date.parse(value.occurred_at) < Date.parse(meta.created_at) ||
      Date.parse(value.occurred_at) > Date.parse(meta.updated_at)) {
    recoveryFail(`recovered custom domain audit time is invalid: ${key}`,
      "agent_email_domain_recovery_revision_regression");
  }
  if (value.action === "custom_domain.plan_reconciled") {
    if (value.actor_kind !== "system" || value.actor_id !== "plan-lifecycle" ||
        !Number.isSafeInteger(value.metadata.changed) ||
        value.metadata.changed < 1 ||
        !Number.isSafeInteger(value.metadata.page_size) ||
        value.metadata.page_size < value.metadata.changed ||
        value.metadata.page_size > 40 ||
        !Number.isSafeInteger(value.metadata.plan_revision) ||
        value.metadata.plan_revision < 0 ||
        !(value.metadata.plan_revision === 0
          ? value.metadata.plan_snapshot_hash === ""
          : SHA256_PATTERN.test(value.metadata.plan_snapshot_hash ?? "")) ||
        !(value.metadata.domain_limit === null ||
          (Number.isSafeInteger(value.metadata.domain_limit) &&
            value.metadata.domain_limit >= 0))) {
      recoveryFail(`recovered custom domain plan audit is invalid: ${key}`);
    }
  }
  if (value.action.startsWith("custom_domain.lifecycle_")) {
    if (value.actor_kind !== "system" ||
        value.actor_id !== "account-lifecycle" ||
        !OPERATION_ID_PATTERN.test(value.metadata.operation_id ?? "") ||
        !Number.isSafeInteger(value.metadata.epoch) ||
        value.metadata.epoch < 0 ||
        !Number.isSafeInteger(value.metadata.changed) ||
        value.metadata.changed < 1 ||
        !Number.isSafeInteger(value.metadata.page_size) ||
        value.metadata.page_size < value.metadata.changed ||
        value.metadata.page_size > 40) {
      recoveryFail(`recovered custom domain lifecycle audit is invalid: ${key}`);
    }
  }
  return value;
}

function requestAuditFail() {
  recoveryFail("recovered custom domain audit evidence is inconsistent",
    "agent_email_domain_recovery_invariant_failed");
}

function validateRequestAuditEvidence(audit, request) {
  if (audit.metadata.account_id !== request.account_id ||
      audit.target !== request.domain ||
      Date.parse(audit.occurred_at) < Date.parse(request.requested_at) ||
      Date.parse(audit.occurred_at) > Date.parse(request.updated_at)) {
    requestAuditFail();
  }
  switch (audit.action) {
    case "custom_domain.requested":
      if (audit.occurred_at !== request.requested_at ||
          audit.actor_kind !== "account_operator" ||
          audit.actor_id !== request.requested_by ||
          audit.metadata.state !== "pending_verification") {
        requestAuditFail();
      }
      break;
    case "custom_domain.rejected":
      if (!request.decision ||
          audit.occurred_at !== request.decision.decided_at ||
          audit.actor_kind !== "platform_admin" ||
          audit.actor_id !== request.decision.decided_by ||
          audit.metadata.from_state !== "pending_verification" ||
          audit.metadata.reason !== request.decision.reason) {
        requestAuditFail();
      }
      break;
    case "custom_domain.retired": {
      const expectedFromState = request.decision
        ? "rejected"
        : request.ownership_verification?.first_verified_at
        ? "verified"
        : "pending_verification";
      if (!request.retirement ||
          request.retirement.retired_by === "account-lifecycle" ||
          audit.occurred_at !== request.retirement.retired_at ||
          audit.actor_kind !== "platform_admin" ||
          audit.actor_id !== request.retirement.retired_by ||
          audit.metadata.reason !== request.retirement.reason ||
          audit.metadata.from_state !== expectedFromState) {
        requestAuditFail();
      }
      break;
    }
    case "custom_domain.expired":
      if (!request.expiration ||
          audit.occurred_at !== request.expiration.expired_at ||
          audit.actor_kind !== "system" ||
          audit.actor_id !== "challenge-expiry") {
        requestAuditFail();
      }
      break;
    case "custom_domain.verified":
      if (!request.ownership_verification?.first_verified_at ||
          audit.occurred_at !==
            request.ownership_verification.first_verified_at ||
          audit.metadata.state !== "verified" ||
          !((audit.actor_kind === "system" &&
              audit.actor_id === "ownership-verifier") ||
            audit.actor_kind === "platform_admin")) {
        requestAuditFail();
      }
      break;
    case "custom_domain.reverified":
    case "custom_domain.verification_missing":
    case "custom_domain.verification_deferred": {
      const reverified = audit.action === "custom_domain.reverified";
      if ((reverified &&
            !request.ownership_verification?.first_verified_at) ||
          (reverified && audit.metadata.state !== "verified") ||
          !((audit.actor_kind === "system" &&
              audit.actor_id === "ownership-verifier") ||
            audit.actor_kind === "platform_admin")) {
        requestAuditFail();
      }
      break;
    }
    case "custom_domain.verification_conflict":
      if (!((audit.actor_kind === "system" &&
              audit.actor_id === "ownership-verifier") ||
            audit.actor_kind === "platform_admin") ||
          audit.metadata.state !== "conflict") {
        requestAuditFail();
      }
      break;
    case "custom_domain.plan_grace_expired":
      if (audit.actor_kind !== "system" ||
          audit.actor_id !== "plan-lifecycle") {
        requestAuditFail();
      }
      break;
    default:
      requestAuditFail();
  }
}

function validateIdempotency(key, value) {
  assertObject(value, `recovered idempotency receipt is invalid: ${key}`);
  if (typeof value.fingerprint !== "string" || value.fingerprint.length < 2 ||
      value.fingerprint.length > 2_048 ||
      !Number.isSafeInteger(value.status) ||
      !((value.status >= 200 && value.status <= 299) ||
        value.status === 409) || !isPlainObject(value.body) ||
      value.body.schema_version !== "witself.agent-email-domain.v1" ||
      !isPlainObject(value.body.request)) {
    recoveryFail(`recovered idempotency receipt is invalid: ${key}`);
  }
  const request = validRequestRecord(value.body.request, { public_record: true });
  const create = key.match(
    /^idem:request-create:([A-Za-z0-9_-]{1,128}):([A-Za-z0-9._:-]{1,128})$/,
  );
  const transition = key.match(
    /^idem:request-(rejected|retired):(aedr_[a-z2-7]{16}):([A-Za-z0-9._:-]{1,128})$/,
  );
  const verification = key.match(
    /^idem:request-verify:(aedr_[a-z2-7]{16}):([A-Za-z0-9._:-]{1,128})$/,
  );
  if (create) {
    if (request.account_id !== create[1] ||
        request.state !== "pending_verification" || value.status !== 202 ||
        value.fingerprint !== JSON.stringify([
          "request.create", request.account_id, request.domain,
        ])) {
      recoveryFail(`recovered create idempotency receipt is inconsistent: ${key}`);
    }
    return { action: "requested", request };
  }
  if (transition) {
    const action = transition[1];
    const evidence = action === "rejected" ? request.decision : request.retirement;
    if (request.id !== transition[2] || request.state !== action ||
        value.status !== 200 || !evidence ||
        value.fingerprint !== JSON.stringify([
          `request.${action}`, request.id, evidence.reason,
        ])) {
      recoveryFail(`recovered transition idempotency receipt is inconsistent: ${key}`);
    }
    return { action, request };
  }
  if (verification) {
    const verified = value.status === 200 &&
      request.state === "verified" && value.body.matched === true;
    const missing = value.status === 409 &&
      ["pending_verification", "verified"].includes(request.state) &&
      request.ownership_verification?.last_result === "absent" &&
      value.body.code === "ownership_challenge_not_found" &&
      value.body.error ===
        "custom domain ownership challenge was not found" &&
      value.body.matched === undefined;
    const expired = value.status === 409 && request.state === "expired" &&
      request.expiration?.reason === "ownership challenge expired" &&
      value.body.code === "ownership_challenge_expired" &&
      value.body.error ===
        "custom inbound domain ownership challenge has expired";
    const conflict = value.status === 409 &&
      request.state === "pending_verification" &&
      request.ownership_verification?.state === "conflict" &&
      request.ownership_verification?.last_result === "domain_unavailable" &&
      value.body.code === "domain_unavailable" &&
      value.body.error ===
        "domain was allocated by another verified request";
    if (request.id !== verification[1] ||
        (!verified && !missing && !expired && !conflict) ||
        value.fingerprint !== JSON.stringify(["request.verify", request.id])) {
      recoveryFail(`recovered verification receipt is inconsistent: ${key}`);
    }
    return {
      action: verified ? "verified" : expired ? "expired" : conflict ?
        "verification_conflict" : "verification_missing",
      request,
    };
  }
  recoveryFail(`recovered idempotency receipt key is invalid: ${key}`);
}

export function validateAgentEmailDomainRecoveredState(state, options = {}) {
  const source = toAuthorityStateMap(state);
  const meta = source.get("meta");
  if (!isPlainObject(meta) ||
      meta.schema_version !== "witself.agent-email-domain.v1" ||
      !Number.isSafeInteger(meta.registry_revision) || meta.registry_revision < 0 ||
      !Number.isSafeInteger(meta.audit_sequence) || meta.audit_sequence < 0 ||
      meta.registry_revision !== meta.audit_sequence) {
    recoveryFail("recovered custom domain authority meta is invalid",
      "agent_email_domain_recovery_revision_regression");
  }
  validateISODate(meta.created_at, "meta created_at");
  validateISODate(meta.updated_at, "meta updated_at");
  if (Date.parse(meta.updated_at) < Date.parse(meta.created_at)) {
    recoveryFail("recovered custom domain meta time regressed",
      "agent_email_domain_recovery_revision_regression");
  }
  for (const [field, expected] of [
    ["registry_revision", options.expected_registry_revision],
    ["audit_sequence", options.expected_audit_sequence],
  ]) {
    if (expected !== undefined && meta[field] !== expected) {
      journalFail(`recovered meta ${field} does not match the expected fence`,
        "agent_email_domain_journal_fence_mismatch");
    }
  }

  const requests = new Map();
  const domains = new Map();
  const audits = new Map();
  const receipts = [];
  for (const [key, value] of source) {
    if (key === "meta") continue;
    if (key.startsWith("request:")) {
      const id = key.slice("request:".length);
      validRequestRecord(value);
      if (value.id !== id || requests.has(id)) {
        recoveryFail(`recovered custom domain request key is invalid: ${key}`);
      }
      requests.set(id, value);
    } else if (key.startsWith("domain:")) {
      const domain = key.slice("domain:".length);
      validAllocationRecord(value);
      if (value.domain !== domain || domains.has(domain)) {
        recoveryFail(`recovered custom domain tombstone key is invalid: ${key}`,
          "agent_email_domain_recovery_collision");
      }
      domains.set(domain, value);
    } else if (key.startsWith("audit:")) {
      const audit = validateAudit(key, value, meta);
      audits.set(audit.sequence, audit);
    } else if (key.startsWith("idem:")) {
      receipts.push(validateIdempotency(key, value));
    } else if (key.startsWith("plan-fence:") ||
        key.startsWith("plan-intent:") ||
        key.startsWith("lifecycle-fence:") ||
        key.startsWith("lifecycle-intent:")) {
      validPolicyRecord(key, value);
    }
  }

  for (const allocation of domains.values()) {
    if (allocation.schema_version === "witself.agent-email-domain.v1") {
      const legacy = requests.get(allocation.id);
      if (!legacy || canonicalJSONString(legacy) !==
          canonicalJSONString(allocation)) {
        recoveryFail("recovered legacy domain tombstone graph is inconsistent",
          "agent_email_domain_recovery_collision");
      }
      continue;
    }
    const request = requests.get(allocation.source_request_id);
    if (!request || request.domain !== allocation.domain ||
        request.account_id !== allocation.account_id ||
        (allocation.state === "allocated" && request.state !== "verified") ||
        (allocation.state === "retired" && request.state !== "retired") ||
        allocation.allocated_at !==
          request.ownership_verification?.first_verified_at ||
        allocation.ownership_proof.verified_at !==
          request.ownership_verification?.last_verified_at ||
        Date.parse(allocation.ownership_proof.verified_at) <
          Date.parse(allocation.allocated_at) ||
        Date.parse(allocation.ownership_proof.verified_at) >
          Date.parse(allocation.updated_at) ||
        (request.ownership_verification?.last_result === "present" &&
          (allocation.ownership_proof.rrset_sha256 !==
              request.ownership_verification.rrset_sha256 ||
            allocation.ownership_proof.dnssec_authenticated !==
              request.ownership_verification.dnssec_authenticated)) ||
        (allocation.state === "retired" &&
          canonicalJSONString(allocation.retirement) !==
            canonicalJSONString(request.retirement))) {
      recoveryFail("recovered request and allocation graph is inconsistent",
        "agent_email_domain_recovery_collision");
    }
  }
  for (const request of requests.values()) {
    const allocation = domains.get(request.domain);
    if (allocation?.schema_version === "witself.agent-email-domain.v1") {
      continue;
    }
    const wasVerified = request.ownership_verification?.first_verified_at != null;
    if ((request.state === "verified" &&
          (!allocation || allocation.source_request_id !== request.id ||
            allocation.state !== "allocated")) ||
        (request.state === "retired" && wasVerified &&
          (!allocation || allocation.source_request_id !== request.id ||
            allocation.state !== "retired")) ||
        (["pending_verification", "rejected", "expired"].includes(request.state) &&
          allocation?.source_request_id === request.id)) {
      recoveryFail("recovered custom domain ownership graph is inconsistent",
        "agent_email_domain_recovery_collision");
    }
  }
  if (audits.size !== meta.audit_sequence) {
    recoveryFail("recovered custom domain audit sequence has a gap",
      "agent_email_domain_recovery_revision_regression");
  }
  for (let sequence = 1; sequence <= meta.audit_sequence; sequence += 1) {
    if (!audits.has(sequence)) {
      recoveryFail("recovered custom domain audit sequence has a gap",
        "agent_email_domain_recovery_revision_regression");
    }
  }

  const auditByRequest = new Map();
  const lastAuditTimeByRequest = new Map();
  const lifecycleRetireAudits = [];
  for (const audit of [...audits.values()].sort((left, right) =>
    left.sequence - right.sequence)) {
    if (audit.action === "custom_domain.lifecycle_retire") {
      lifecycleRetireAudits.push(audit);
    }
    const accountTarget = audit.action === "custom_domain.plan_reconciled" ||
      audit.action.startsWith("custom_domain.lifecycle_");
    if (accountTarget) {
      const accountRequests = [...requests.values()].filter((request) =>
        request.account_id === audit.metadata.account_id);
      if (accountRequests.length < audit.metadata.changed ||
          (audit.action === "custom_domain.lifecycle_retire" &&
            accountRequests.filter((request) =>
              request.state === "retired" &&
              request.lifecycle_fence?.operation_id ===
                audit.metadata.operation_id &&
              request.lifecycle_fence?.epoch === audit.metadata.epoch &&
              request.lifecycle_fence?.action === "retire"
            ).length < audit.metadata.changed)) {
        recoveryFail("recovered custom domain account audit is orphaned");
      }
      continue;
    }
    let requestID = audit.metadata.request_id;
    if (requestID === undefined) {
      const legacyMatches = [...requests.values()].filter((request) =>
        request.account_id === audit.metadata.account_id &&
        request.domain === audit.target);
      if (legacyMatches.length !== 1) {
        recoveryFail("recovered custom domain audit graph is inconsistent");
      }
      requestID = legacyMatches[0].id;
    }
    const request = requests.get(requestID);
    if (!request || request.account_id !== audit.metadata.account_id ||
        request.domain !== audit.target) {
      recoveryFail("recovered custom domain audit graph is inconsistent");
    }
    validateRequestAuditEvidence(audit, request);
    const previousAuditTime = lastAuditTimeByRequest.get(requestID);
    if (previousAuditTime &&
        Date.parse(audit.occurred_at) < Date.parse(previousAuditTime)) {
      recoveryFail("recovered custom domain audit chronology regressed",
        "agent_email_domain_recovery_revision_regression");
    }
    lastAuditTimeByRequest.set(requestID, audit.occurred_at);
    const actions = auditByRequest.get(requestID) ?? [];
    actions.push(audit.action);
    auditByRequest.set(requestID, actions);
  }
  for (const request of requests.values()) {
    let actions = auditByRequest.get(request.id) ?? [];
    if (actions[0] !== "custom_domain.requested") {
      recoveryFail("recovered custom domain audit lifecycle is inconsistent");
    }
    const expiredIndex = actions.indexOf("custom_domain.expired");
    const retiredIndex = actions.indexOf("custom_domain.retired");
    const rejectedIndex = actions.indexOf("custom_domain.rejected");
    if ((expiredIndex >= 0 && expiredIndex !== actions.length - 1) ||
        (retiredIndex >= 0 && retiredIndex !== actions.length - 1) ||
        (rejectedIndex >= 0 && rejectedIndex !== actions.length - 1 &&
          !(rejectedIndex === actions.length - 2 && retiredIndex ===
            actions.length - 1))) {
      recoveryFail("recovered custom domain audit lifecycle is inconsistent");
    }
    const lifecycleRetireRecorded = request.state === "retired" &&
      request.retirement?.retired_by === "account-lifecycle" &&
      request.lifecycle_fence?.action === "retire" &&
      lifecycleRetireAudits.some((audit) =>
        audit.target === request.account_id &&
        audit.metadata.account_id === request.account_id &&
        audit.metadata.operation_id === request.lifecycle_fence.operation_id &&
        audit.metadata.epoch === request.lifecycle_fence.epoch
      );
    const lifecycleFenceRecorded = !request.lifecycle_fence ||
      [...audits.values()].some((audit) =>
        audit.action ===
          `custom_domain.lifecycle_${request.lifecycle_fence.action}` &&
        audit.target === request.account_id &&
        audit.metadata.account_id === request.account_id &&
        audit.metadata.operation_id === request.lifecycle_fence.operation_id &&
        audit.metadata.epoch === request.lifecycle_fence.epoch);
    if (!lifecycleFenceRecorded) {
      recoveryFail("recovered custom domain lifecycle fence is unaudited");
    }
    const wasVerified =
      request.ownership_verification?.first_verified_at != null;
    const manualRetirement = request.retirement &&
      request.retirement.retired_by !== "account-lifecycle";
    if (!actions.includes("custom_domain.requested") ||
        (wasVerified && !actions.includes("custom_domain.verified")) ||
        (request.decision &&
          !actions.includes("custom_domain.rejected")) ||
        (manualRetirement &&
          !actions.includes("custom_domain.retired")) ||
        (request.state === "verified" &&
          !actions.includes("custom_domain.verified")) ||
        (request.state === "rejected" &&
          !actions.includes("custom_domain.rejected")) ||
        (request.state === "expired" &&
          !actions.includes("custom_domain.expired")) ||
        (request.state === "retired" && !lifecycleRetireRecorded &&
          !actions.some((action) => [
            "custom_domain.retired", "custom_domain.lifecycle_retire",
          ].includes(action)))) {
      recoveryFail("recovered custom domain audit lifecycle is inconsistent");
    }
    const requestReceipts = receipts.filter(({ request: receipt }) =>
      receipt.id === request.id);
    const receiptActions = requestReceipts.map(({ action }) => action).sort();
    if (!receiptActions.includes("requested") ||
        (request.decision && !receiptActions.includes("rejected")) ||
        (manualRetirement && !receiptActions.includes("retired"))) {
      recoveryFail("recovered custom domain idempotency graph is inconsistent");
    }
    for (const { action, request: receipt } of requestReceipts) {
      if (immutableFieldsChanged(request, receipt, [
        "id", "account_id", "domain", "ownership_challenge", "requested_by",
        "requested_at", "domain_limit_at_request", "plan_revision",
        "plan_snapshot_hash",
      ])) {
        recoveryFail("recovered custom domain idempotency identity is inconsistent");
      }
      if (action === "requested" &&
          (receipt.state !== "pending_verification" ||
            receipt.updated_at !== request.requested_at ||
            receipt.decision != null || receipt.retirement != null)) {
        recoveryFail("recovered create receipt lifecycle is inconsistent");
      }
      if (action === "rejected" &&
          (canonicalJSONString(receipt.decision ?? null) !==
              canonicalJSONString(request.decision ?? null) ||
            receipt.updated_at !== request.decision?.decided_at ||
            receipt.retirement != null)) {
        recoveryFail("recovered rejection receipt lifecycle is inconsistent");
      }
      if (action === "retired" &&
          (canonicalJSONString(receipt.retirement ?? null) !==
              canonicalJSONString(request.retirement ?? null) ||
            canonicalJSONString(receipt.decision ?? null) !==
              canonicalJSONString(request.decision ?? null) ||
            receipt.updated_at !== request.retirement?.retired_at)) {
        recoveryFail("recovered retirement receipt lifecycle is inconsistent");
      }
      if (action === "verified" &&
          (receipt.state !== "verified" ||
            receipt.ownership_verification?.first_verified_at == null)) {
        recoveryFail("recovered verification receipt lifecycle is inconsistent");
      }
    }
  }
  if (receipts.some(({ request }) => !requests.has(request.id))) {
    recoveryFail("recovered custom domain idempotency receipt is orphaned");
  }
  return {
    registry_revision: meta.registry_revision,
    audit_sequence: meta.audit_sequence,
    requests: requests.size,
    domains: domains.size,
    authority_keys: source.size,
  };
}

export function rebuildAgentEmailDomainDerivedState(state) {
  const source = toAuthorityStateMap(state);
  validateAgentEmailDomainRecoveredState(source);
  const derived = new Map();
  const usage = new Map();
  for (const [key, request] of [...source]
    .filter(([key]) => key.startsWith("request:"))
    .sort(([left], [right]) => left.localeCompare(right))) {
    void key;
    derived.set(`account-request:${request.account_id}:${request.id}`, request.id);
    const active = ["pending_verification", "verified"].includes(request.state);
    if (active) {
      derived.set(
        `account-domain:${request.account_id}:${request.requested_at}:${request.id}`,
        request.id,
      );
    }
    if (request.state === "pending_verification") {
      derived.set(
        `domain-pending:${request.domain}:${request.requested_at}:${request.id}`,
        request.id,
      );
      const challengeExpiresAt = request.ownership_challenge.expires_at ??
        new Date(
          Date.parse(request.requested_at) + PENDING_CHALLENGE_TTL_MS,
        ).toISOString();
      if (challengeExpiresAt) {
        derived.set(
          `challenge-expiry-due:${String(
            Date.parse(challengeExpiresAt),
          ).padStart(16, "0")}:${request.id}`,
          request.id,
        );
      }
    }
    const verificationDueAt =
      request.ownership_verification?.next_check_at ??
      (request.state === "pending_verification" ? request.requested_at : null);
    if (active && verificationDueAt &&
        request.plan_suspended !== true &&
        request.lifecycle_suspended !== true) {
      derived.set(
        `verification-due:${String(
          Date.parse(verificationDueAt),
        ).padStart(16, "0")}:${request.id}`,
        request.id,
      );
    }
    if (request.state === "verified" && request.plan_grace_until) {
      derived.set(
        `plan-grace-due:${String(
          Date.parse(request.plan_grace_until),
        ).padStart(16, "0")}:${request.id}`,
        request.id,
      );
    }
    const usageChangedAt = request.state === "pending_verification"
      ? request.requested_at
      : request.state === "verified"
      ? request.ownership_verification?.first_verified_at ?? request.requested_at
      : request.state === "rejected"
      ? request.decision?.decided_at
      : request.state === "expired"
      ? request.expiration?.expired_at
      : request.state === "retired" && request.decision?.decided_at
      ? request.decision.decided_at
      : request.state === "retired"
      ? request.retirement?.retired_at
      : request.updated_at;
    const count = usage.get(request.account_id) ?? {
      open_requests: 0,
      allocated_domains: 0,
      updated_at: usageChangedAt,
    };
    usage.set(
      request.account_id,
      {
        open_requests: count.open_requests +
          (request.state === "pending_verification" ? 1 : 0),
        allocated_domains: count.allocated_domains + (active ? 1 : 0),
        updated_at: Date.parse(count.updated_at) >= Date.parse(usageChangedAt)
          ? count.updated_at
          : usageChangedAt,
      },
    );
  }
  for (const [accountID, account] of usage) {
    derived.set(`account-usage:${accountID}`, {
      schema_version: 1,
      account_id: accountID,
      open_requests: account.open_requests,
      allocated_domains: account.allocated_domains,
      updated_at: account.updated_at,
    });
  }
  for (const [key, intent] of source) {
    if (key.startsWith("plan-intent:") &&
        Number.isSafeInteger(intent.retry_at_ms)) {
      derived.set(
        `plan-due:${String(intent.retry_at_ms).padStart(16, "0")}:` +
          intent.account_id,
        intent.account_id,
      );
    }
    if (key.startsWith("lifecycle-intent:") &&
        Number.isSafeInteger(intent.retry_at_ms)) {
      derived.set(
        `lifecycle-due:${String(intent.retry_at_ms).padStart(16, "0")}:` +
          intent.account_id,
        intent.account_id,
      );
    }
  }
  return derived;
}
