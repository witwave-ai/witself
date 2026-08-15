import {
  buildRealmEmailAliasClaimProof,
  realmEmailAliasClaimRouteFingerprint,
  validateRealmEmailAliasClaimProof,
} from "./agent-email-custom-domain-route-contract.mjs";
import {
  AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
} from "./agent-email-operations-lease.mjs";

const ALIAS_PATTERN = /^[a-z0-9](?:[a-z0-9-]{1,14}[a-z0-9])$/;
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const REALM_ID_PATTERN = /^realm_[a-z2-7]{16}$/;
const REQUEST_ID_PATTERN = /^earq_[a-z2-7]{16}$/;
const CLAIM_ID_PATTERN = /^era_[a-z2-7]{16}$/;
const CANONICAL_REALM_LABEL_PATTERN = /^[a-z2-7]{16}$/;
const DOMAIN_LABEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const STREAM_ID_PATTERN = /^reaj_[a-z2-7]{16,52}$/;
const OPERATION_ID_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const SHA256_PATTERN = /^[0-9a-f]{64}$/;
const ISO_DATE_MAX_LENGTH = 64;
const STORAGE_KEY_MAX_LENGTH = 1_024;
const ENTRY_MAX_BYTES = 512 * 1_024;
const CANONICAL_MAX_DEPTH = 32;
const CANONICAL_MAX_ITEMS = 10_000;

export const REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_SCHEMA_VERSION =
  "witself.realm-email-alias-authority-journal.v1";
export const REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_PREFIX =
  "realm-email-alias-authority/v1";
export const REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_GENESIS_HASH = "0".repeat(64);
export const REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_MAX_CHANGES = 100;
export const REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT = 100;

const ENTRY_KINDS = new Set([
  "bootstrap",
  "mutation",
  "checkpoint",
  "takeover",
]);

const ACTOR_KINDS = new Set([
  "account_operator",
  "platform_admin",
  "system",
]);

const AUTHORITY_EXACT_KEYS = new Set(["meta"]);
const AUTHORITY_PREFIXES = Object.freeze([
  "audit:",
  "canonical:",
  "claim:",
  "custom-domain-subscription:",
  "custom-domain-sync:",
  "idem:",
  "lifecycle-fence:",
  "lifecycle-intent:",
  "plan-fence:",
  "plan-intent:",
  "projection-intent:",
  "realm-close-fence:",
  "realm-close-intent:",
  "request:",
  "reserved-history:",
  "reserved:",
  "route-refresh:",
]);

const DELETABLE_AUTHORITY_PREFIXES = Object.freeze([
  "custom-domain-sync:",
  "lifecycle-intent:",
  "plan-intent:",
  "projection-intent:",
  "realm-close-intent:",
  "route-refresh:",
]);

const DERIVED_EXACT_KEYS = new Set([
  "canonical-inventory",
  "pending-counter-migration",
]);
const DERIVED_PREFIXES = Object.freeze([
  "account-canonical:",
  "account-claim:",
  "account-request:",
  "approval-due:",
  "claim-skeleton:",
  "claim-usage-account-member:",
  "claim-usage-account:",
  "claim-usage-member:",
  "claim-usage-realm-member:",
  "claim-usage-realm:",
  "custom-domain-subscription-realm:",
  "custom-domain-sync-account:",
  "custom-domain-sync-due:",
  "grace:",
  "internal-due:",
  "lifecycle-due:",
  "plan-due:",
  "projection-due:",
  "refresh-due:",
  "realm-canonical:",
  "realm-close-due:",
  "reserved-skeleton:",
]);

const JOURNAL_LOCAL_EXACT_KEYS = new Set([
  AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
  "realm-email-alias-journal-meta",
  "realm-email-alias-journal-pending",
  "realm-email-alias-journal-fork",
  "realm-email-alias-recovery",
]);
const JOURNAL_LOCAL_PREFIXES = Object.freeze([
  "realm-email-alias-journal:",
  "realm-email-alias-recovery:",
]);

export class RealmEmailAliasJournalError extends Error {
  constructor(message, code = "invalid_realm_email_alias_journal") {
    super(message);
    this.name = "RealmEmailAliasJournalError";
    this.code = code;
  }
}

function journalFail(message, code = "invalid_realm_email_alias_journal") {
  throw new RealmEmailAliasJournalError(message, code);
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

export function realmEmailAliasJournalEntryKey(streamID, sequence) {
  return `${REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_PREFIX}/streams/` +
    `${validateStreamID(streamID)}/entries/${sequenceSegment(sequence)}.json`;
}

export function classifyRealmEmailAliasStorageKey(key) {
  if (typeof key !== "string" || key.length === 0 ||
      key.length > STORAGE_KEY_MAX_LENGTH) {
    return "unknown";
  }
  if (AUTHORITY_EXACT_KEYS.has(key) ||
      AUTHORITY_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "authority";
  }
  if (DERIVED_EXACT_KEYS.has(key) ||
      DERIVED_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "derived";
  }
  if (JOURNAL_LOCAL_EXACT_KEYS.has(key) ||
      JOURNAL_LOCAL_PREFIXES.some((prefix) => key.startsWith(prefix))) {
    return "journal_local";
  }
  return "unknown";
}

export function isRealmEmailAliasAuthorityKey(key) {
  return classifyRealmEmailAliasStorageKey(key) === "authority";
}

export function isRealmEmailAliasDerivedKey(key) {
  return classifyRealmEmailAliasStorageKey(key) === "derived";
}

function authorityKeyCanBeDeleted(key) {
  return DELETABLE_AUTHORITY_PREFIXES.some((prefix) => key.startsWith(prefix));
}

function normalizedAfterImage(afterImage, options = {}) {
  exactKeys(afterImage, new Set(["puts", "deletes"]), "after_image");
  if (!Array.isArray(afterImage.puts) || !Array.isArray(afterImage.deletes)) {
    journalFail("after_image puts and deletes must be arrays");
  }
  const maximum = options.max_changes ??
    REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_MAX_CHANGES;
  if (!Number.isSafeInteger(maximum) || maximum < 1 ||
      maximum > REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_MAX_CHANGES) {
    journalFail("after_image maximum is invalid");
  }
  if (afterImage.puts.length + afterImage.deletes.length > maximum) {
    journalFail("after_image exceeds the bounded change limit");
  }
  const seen = new Set();
  const puts = afterImage.puts.map((put) => {
    exactKeys(put, new Set(["key", "value"]), "after_image put");
    if (!isRealmEmailAliasAuthorityKey(put.key)) {
      journalFail(`after_image key is not canonical authority: ${String(put.key)}`);
    }
    if (seen.has(put.key)) journalFail("after_image contains a duplicate key");
    seen.add(put.key);
    // Canonicalization is the shared value-shape validator. Parse its output
    // to detach caller-owned objects and normalize null-prototype records.
    const value = JSON.parse(canonicalJSONString(put.value));
    return { key: put.key, value };
  }).sort((left, right) => left.key.localeCompare(right.key));
  const deletes = afterImage.deletes.map((key) => {
    if (!isRealmEmailAliasAuthorityKey(key) || !authorityKeyCanBeDeleted(key)) {
      journalFail(`after_image cannot delete canonical authority key: ${String(key)}`);
    }
    if (seen.has(key)) journalFail("after_image contains a duplicate key");
    seen.add(key);
    return key;
  }).sort();
  return { puts, deletes };
}

export function validateRealmEmailAliasAuthorityAfterImage(
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
  const fields = new Set([
    "schema_version",
    "stream_id",
    "kind",
    "authority_epoch",
    "sequence",
    "previous_hash",
    "registry_revision",
    "audit_sequence",
    "occurred_at",
    "operation_id",
    "operation_fingerprint",
    "actor",
    "action",
    "target",
    "metadata",
    "after_image",
    ...(includeHash ? ["entry_hash"] : []),
  ]);
  exactKeys(input, fields, "journal entry");
  if (input.schema_version !== undefined &&
      input.schema_version !==
        REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_SCHEMA_VERSION) {
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
      typeof input.actor.id !== "string" || input.actor.id.length < 1 ||
      input.actor.id.length > 128) {
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
  const afterImage = normalizedAfterImage(input.after_image);
  const normalized = {
    schema_version: REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_SCHEMA_VERSION,
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
    after_image: afterImage,
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
  const fields = [
    ["stream_id", expected.stream_id],
    ["sequence", expected.sequence],
    ["previous_hash", expected.previous_hash],
    ["authority_epoch", expected.authority_epoch],
  ];
  for (const [name, value] of fields) {
    if (value !== undefined && entry[name] !== value) {
      journalFail(`journal ${name} does not match the expected fence`,
        "realm_email_alias_journal_fence_mismatch");
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
    key: realmEmailAliasJournalEntryKey(entry.stream_id, entry.sequence),
  };
}

export async function buildRealmEmailAliasJournalEntry(input) {
  const unsigned = normalizeEntryInput(input, false);
  const hash = await sha256Hex(canonicalJSONBytes(unsigned));
  const entry = { ...unsigned, entry_hash: hash };
  const bytes = canonicalJSONBytes(entry);
  return encodedEntryResult(entry, bytes, hash);
}

export async function validateRealmEmailAliasJournalEntry(entry, expected = {}) {
  const normalized = normalizeEntryInput(entry, true);
  assertExpectedEntry(normalized, expected);
  const { entry_hash: claimedHash, ...unsigned } = normalized;
  const hash = await sha256Hex(canonicalJSONBytes(unsigned));
  if (hash !== claimedHash) {
    journalFail("journal entry hash is invalid", "realm_email_alias_journal_hash_mismatch");
  }
  const canonicalEntry = { ...unsigned, entry_hash: hash };
  const bytes = canonicalJSONBytes(canonicalEntry);
  return encodedEntryResult(canonicalEntry, bytes, hash);
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
    "realm_email_alias_journal_unavailable");
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
      `realm email alias authority journal is unavailable${cause ? `: ${cause.message ?? cause}` : ""}`,
      "realm_email_alias_journal_unavailable",
    );
  }
  if (!existing) {
    journalFail(
      `realm email alias authority journal append failed${cause ? `: ${cause.message ?? cause}` : ""}`,
      "realm_email_alias_journal_unavailable",
    );
  }
  const bytes = await r2ObjectBytes(existing);
  if (!bytesEqual(bytes, built.bytes)) {
    journalFail(
      "realm email alias authority journal fork detected",
      "realm_email_alias_journal_fork_detected",
    );
  }
  return { created: false, replayed: true, object: existing };
}

export async function appendRealmEmailAliasJournalEntry(bucket, built) {
  if (!bucket || typeof bucket.put !== "function" ||
      typeof bucket.get !== "function") {
    journalFail(
      "realm email alias authority journal binding is unavailable",
      "realm_email_alias_journal_unavailable",
    );
  }
  if (!built || !(built.bytes instanceof Uint8Array) ||
      typeof built.key !== "string" || !isPlainObject(built.entry)) {
    journalFail("built journal entry is invalid");
  }
  const verified = await validateRealmEmailAliasJournalEntry(built.entry);
  if (verified.key !== built.key || !bytesEqual(verified.bytes, built.bytes)) {
    journalFail("built journal entry is not canonical");
  }
  let object;
  try {
    object = await bucket.put(built.key, built.bytes, {
      onlyIf: new Headers({ "If-None-Match": "*" }),
      httpMetadata: { contentType: "application/json" },
      customMetadata: {
        schema: "witself.realm-email-alias-authority-journal.v1",
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
    if (!isRealmEmailAliasAuthorityKey(key)) {
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

function isPreparedAuditCompletion(previous, desired) {
  if (!isPlainObject(previous) || !isPlainObject(desired) ||
      previous.action !== `${desired.action}.intent_recorded` ||
      previous.metadata?.phase !== "prepared" ||
      previous.metadata?.requested_action !== desired.action ||
      desired.metadata?.phase !== "committed" ||
      immutableFieldsChanged(previous, desired, [
        "sequence",
        "registry_revision",
        "occurred_at",
        "actor_kind",
        "actor_id",
        "target",
      ])) {
    return false;
  }
  const {
    phase: _previousPhase,
    requested_action: _requestedAction,
    ...previousMetadata
  } = previous.metadata;
  const { phase: _desiredPhase, ...desiredMetadata } = desired.metadata;
  return canonicalJSONString(previousMetadata) ===
    canonicalJSONString(desiredMetadata);
}

function assertAuthorityTransition(state, key, desired) {
  const previous = state.get(key);
  if (!previous) return;
  if (key.startsWith("audit:")) {
    if (canonicalJSONString(previous) !== canonicalJSONString(desired) &&
        !isPreparedAuditCompletion(previous, desired)) {
      journalFail(`append-only authority key cannot be overwritten: ${key}`,
        "realm_email_alias_recovery_invariant_failed");
    }
    return;
  }
  if (key.startsWith("reserved-history:") || key.startsWith("idem:")) {
    if (canonicalJSONString(previous) !== canonicalJSONString(desired)) {
      journalFail(`append-only authority key cannot be overwritten: ${key}`,
        "realm_email_alias_recovery_invariant_failed");
    }
    return;
  }
  if (key === "meta") {
    for (const field of [
      "registry_revision",
      "audit_sequence",
      "reserved_policy_version",
    ]) {
      if (Number.isSafeInteger(previous[field]) &&
          (!Number.isSafeInteger(desired[field]) || desired[field] < previous[field])) {
        journalFail(`meta ${field} regressed`,
          "realm_email_alias_recovery_invariant_failed");
      }
    }
    return;
  }
  if (key.startsWith("custom-domain-subscription:")) {
    if (canonicalJSONString(previous) !== canonicalJSONString(desired)) {
      journalFail("custom-domain alias subscription is immutable",
        "realm_email_alias_recovery_invariant_failed");
    }
    return;
  }
  if (key.startsWith("custom-domain-sync:")) {
    const previousProof = previous.claim_proof;
    const desiredProof = desired.claim_proof;
    const sameRevision = desiredProof?.realm_alias_revision ===
      previousProof?.realm_alias_revision;
    const newerRevision = desiredProof?.realm_alias_revision >
      previousProof?.realm_alias_revision;
    if (previous.created_at !== desired.created_at ||
        previous.claim_proof?.realm_alias_claim_id !==
          desired.claim_proof?.realm_alias_claim_id ||
        previousProof?.account_id !== desiredProof?.account_id ||
        previousProof?.realm_id !== desiredProof?.realm_id ||
        previousProof?.realm_label !== desiredProof?.realm_label ||
        desired.claim_proof?.realm_alias_revision <
          previous.claim_proof?.realm_alias_revision ||
        (sameRevision &&
          (canonicalJSONString(previousProof) !==
              canonicalJSONString(desiredProof) ||
            previous.source_fingerprint !== desired.source_fingerprint ||
            desired.retry_at_ms < previous.retry_at_ms)) ||
        (newerRevision &&
          (desired.phase !== "enqueue" || desired.failure_count !== 0)) ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("custom-domain alias sync regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("plan-intent:")) {
    const order = {
      awaiting_cell: 1,
      cell_committed: 2,
      custom_domain_converging: 3,
    };
    const previousOrder = order[previous.state] ?? 0;
    const desiredOrder = order[desired.state] ?? 0;
    const sameRevision = previous.plan_revision === desired.plan_revision;
    if (previous.account_id !== desired.account_id ||
        desired.plan_revision < previous.plan_revision ||
        previous.created_at !== desired.created_at ||
        (sameRevision &&
          (previous.plan_snapshot_hash !== desired.plan_snapshot_hash ||
            previous.feature_enabled !== desired.feature_enabled ||
            previous.alias_limit !== desired.alias_limit ||
            previous.activation_enabled !== desired.activation_enabled ||
            desiredOrder < previousOrder)) ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("realm email alias plan intent regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("lifecycle-intent:")) {
    const order = { claims: 1, canonical: 2, custom_domain_converging: 3 };
    const previousOrder = order[previous.phase ?? "claims"] ?? 0;
    const desiredOrder = order[desired.phase ?? "claims"] ?? 0;
    if (previous.account_id !== desired.account_id ||
        previous.operation_id !== desired.operation_id ||
        previous.epoch !== desired.epoch ||
        previous.action !== desired.action ||
        previous.created_at !== desired.created_at ||
        desiredOrder < previousOrder ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("realm email alias lifecycle intent regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("claim:")) {
    if (immutableFieldsChanged(previous, desired, [
      "claim_id", "alias", "domain", "skeleton", "account_id", "realm_id",
      "request_id", "created_at",
    ])) {
      journalFail("claim identity changed during replay",
        "realm_email_alias_recovery_invariant_failed");
    }
    if (previous.retired_at && !desired.retired_at) {
      journalFail("retired claim tombstone was resurrected",
        "realm_email_alias_recovery_tombstone_resurrection");
    }
    if (!Number.isSafeInteger(desired.assignment_revision) ||
        desired.assignment_revision < (previous.assignment_revision ?? 0)) {
      journalFail("claim assignment_revision regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("canonical:")) {
    if (immutableFieldsChanged(previous, desired, [
      "domain", "account_id", "realm_id", "realm_label",
    ])) {
      journalFail("canonical route identity changed during replay",
        "realm_email_alias_recovery_invariant_failed");
    }
    if (!Number.isSafeInteger(desired.controller_revision) ||
        desired.controller_revision < (previous.controller_revision ?? 0)) {
      journalFail("canonical controller_revision regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    const desiredUpdatedAt = typeof desired.updated_at === "string"
      ? Date.parse(desired.updated_at)
      : NaN;
    if (!Number.isFinite(desiredUpdatedAt)) {
      journalFail("canonical updated_at is invalid",
        "realm_email_alias_recovery_revision_regression");
    }
    if (desired.controller_revision === previous.controller_revision) {
      const { updated_at: _previousUpdatedAt, ...previousAuthority } = previous;
      const { updated_at: _desiredUpdatedAt, ...desiredAuthority } = desired;
      if (canonicalJSONString(previousAuthority) !==
            canonicalJSONString(desiredAuthority)) {
        journalFail("same-revision canonical refresh changed authority",
          "realm_email_alias_recovery_revision_regression");
      }
    }
    if (previous.state === "retired" && desired.state !== "retired") {
      journalFail("retired canonical route was resurrected",
        "realm_email_alias_recovery_tombstone_resurrection");
    }
    return;
  }
  if (key.startsWith("realm-close-intent:")) {
    const order = {
      scan_aliases: 1,
      custom_domain_converging: 2,
      prepare_cell: 3,
      publish_retired: 4,
      commit_cell: 5,
    };
    if (previous.account_id !== desired.account_id ||
        previous.realm_id !== desired.realm_id ||
        previous.idempotency_key !== desired.idempotency_key ||
        previous.fingerprint !== desired.fingerprint ||
        previous.created_at !== desired.created_at ||
        (order[desired.phase] ?? 0) < (order[previous.phase] ?? 0) ||
        Date.parse(desired.updated_at) < Date.parse(previous.updated_at)) {
      journalFail("realm-close intent regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("realm-close-fence:")) {
    if (immutableFieldsChanged(previous, desired, [
      "account_id", "realm_id", "operation_id", "cell_generation",
      "canonical_revisions",
    ]) || !Number.isSafeInteger(desired.controller_revision) ||
        desired.controller_revision < (previous.controller_revision ?? 0)) {
      journalFail("realm-close controller revision regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("reserved:")) {
    if (immutableFieldsChanged(previous, desired, [
      "name", "skeleton", "created_at", "created_by",
    ]) || !Number.isSafeInteger(desired.version) ||
        desired.version < previous.version ||
        !Number.isSafeInteger(desired.policy_version) ||
        desired.policy_version < previous.policy_version ||
        (previous.retired_at && !desired.retired_at && desired.enabled !== true)) {
      journalFail("reserved-name revision regressed",
        "realm_email_alias_recovery_revision_regression");
    }
    return;
  }
  if (key.startsWith("request:")) {
    if (immutableFieldsChanged(previous, desired, [
      "id", "alias", "domain", "skeleton", "account_id", "realm_id",
      "requested_by", "requested_at",
    ])) {
      journalFail("request identity changed during replay",
        "realm_email_alias_recovery_invariant_failed");
    }
    if (["approved", "rejected"].includes(previous.status) &&
        desired.status !== previous.status) {
      journalFail("terminal request status changed during replay",
        "realm_email_alias_recovery_invariant_failed");
    }
  }
}

export function applyRealmEmailAliasAuthorityAfterImage(state, afterImage) {
  const result = toAuthorityStateMap(state);
  const normalized = normalizedAfterImage(afterImage);
  applyNormalizedAfterImage(result, normalized);
  return result;
}

function applyNormalizedAfterImage(state, normalized) {
  for (const { key, value } of normalized.puts) {
    assertAuthorityTransition(state, key, value);
    state.set(key, JSON.parse(canonicalJSONString(value)));
  }
  for (const key of normalized.deletes) state.delete(key);
}

export function buildRealmEmailAliasBootstrapPage(state, options = {}) {
  const source = toAuthorityStateMap(state);
  const limit = options.limit ?? REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT;
  if (!Number.isSafeInteger(limit) || limit < 1 ||
      limit > REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT) {
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

export async function realmEmailAliasAuthorityStateDigest(state) {
  const source = toAuthorityStateMap(state);
  const rows = [...source]
    .sort(([left], [right]) => left.localeCompare(right));
  let digest = await sha256Hex(canonicalJSONBytes({
    schema_version: "witself.realm-email-alias-authority-state.v1",
    rows: 0,
  }));
  let count = 0;
  for (const [key, value] of rows) {
    count += 1;
    digest = await sha256Hex(canonicalJSONBytes({
      schema_version: "witself.realm-email-alias-authority-state-row.v1",
      previous_hash: digest,
      row_number: count,
      key,
      value,
    }));
  }
  return sha256Hex(canonicalJSONBytes({
    schema_version: "witself.realm-email-alias-authority-state-final.v1",
    rows: count,
    rows_hash: digest,
  }));
}

export async function replayRealmEmailAliasJournalPage(entries, options = {}) {
  if (!Array.isArray(entries)) journalFail("journal replay page must be an array");
  const maximum = options.max_entries ??
    REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT;
  if (!Number.isSafeInteger(maximum) || maximum < 1 ||
      maximum > REALM_EMAIL_ALIAS_AUTHORITY_BOOTSTRAP_PAGE_LIMIT ||
      entries.length > maximum) {
    journalFail("journal replay page exceeds the bounded entry limit");
  }
  const streamID = validateStreamID(options.stream_id);
  let state = toAuthorityStateMap(options.state ?? new Map());
  let head = options.head ?? {
    sequence: 0,
    hash: REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_GENESIS_HASH,
    authority_epoch: null,
  };
  if (!Number.isSafeInteger(head.sequence) || head.sequence < 0 ||
      !SHA256_PATTERN.test(head.hash ?? "") ||
      (head.authority_epoch !== null &&
        (!Number.isSafeInteger(head.authority_epoch) || head.authority_epoch < 1))) {
    journalFail("journal replay head is invalid");
  }
  for (const raw of entries) {
    const expectedSequence = head.sequence + 1;
    const built = await validateRealmEmailAliasJournalEntry(raw, {
      stream_id: streamID,
      sequence: expectedSequence,
      previous_hash: head.hash,
    });
    if (head.authority_epoch !== null) {
      const expectedEpoch = built.entry.kind === "takeover"
        ? head.authority_epoch + 1
        : head.authority_epoch;
      if (built.entry.authority_epoch !== expectedEpoch) {
        journalFail("journal authority_epoch is not contiguous",
          "realm_email_alias_journal_fence_mismatch");
      }
    }
    applyNormalizedAfterImage(
      state,
      normalizedAfterImage(built.entry.after_image),
    );
    const meta = state.get("meta");
    if (meta && (meta.registry_revision !== built.entry.registry_revision ||
        meta.audit_sequence !== built.entry.audit_sequence)) {
      journalFail("journal entry revisions do not match its meta after-image",
        "realm_email_alias_recovery_revision_regression");
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

function aliasSkeleton(alias) {
  const folded = {
    "0": "o", "1": "i", "3": "e", "4": "a", "5": "s",
    "7": "t", "8": "b", "9": "g", l: "i", i: "i",
  };
  return [...alias]
    .filter((character) => character !== "-")
    .map((character) => folded[character] ?? character)
    .join("");
}

function validDomain(domain) {
  return typeof domain === "string" && domain === domain.toLowerCase() &&
    domain.length >= 1 && domain.length <= 253 &&
    domain.split(".").every((label) => DOMAIN_LABEL_PATTERN.test(label));
}

function validRealmCloseDomains(domains, primary) {
  return Array.isArray(domains) && domains.length >= 1 && domains.length <= 2 &&
    domains[0] === primary && new Set(domains).size === domains.length &&
    domains.every(validDomain);
}

function validCanonicalRevisions(revisions, controllerRevision) {
  return Array.isArray(revisions) && revisions.length >= 1 &&
    revisions.length <= 2 &&
    new Set(revisions.map((item) => item?.domain)).size === revisions.length &&
    revisions.every((item) =>
      isPlainObject(item) && validDomain(item.domain) &&
      Number.isSafeInteger(item.controller_revision) &&
      item.controller_revision >= 1
    ) && revisions[0].controller_revision === controllerRevision;
}

function assertObject(value, message) {
  if (!isPlainObject(value)) {
    journalFail(message, "realm_email_alias_recovery_invariant_failed");
  }
}

function recoveredClaimProof(claim) {
  if (!claim?.assignment_kind || claim.customer_activation_intent === true ||
      claim.internal_intent === true) return null;
  const state = claim.retired_at
    ? "retired"
    : claim.lifecycle_suspended === true ||
        claim.operational_gate_suspended === true ||
        claim.admin_suspended === true || claim.plan_suspended === true
    ? "suspended"
    : "applied";
  return buildRealmEmailAliasClaimProof({
    account_id: claim.account_id,
    realm_id: claim.realm_id,
    realm_label: claim.alias,
    realm_alias_claim_id: claim.claim_id,
    realm_alias_revision: claim.assignment_revision,
    state,
    ...(state === "suspended"
      ? {
        suspension_disposition:
          claim.plan_suspended === true &&
              !claim.lifecycle_suspended &&
              !claim.operational_gate_suspended && !claim.admin_suspended
            ? "inactive"
            : "retry",
      }
      : {}),
    updated_at: claim.updated_at,
  });
}

export function validateRealmEmailAliasRecoveredState(state, options = {}) {
  const source = toAuthorityStateMap(state);
  const meta = source.get("meta");
  assertObject(meta, "recovered authority meta is missing");
  for (const field of [
    "registry_revision",
    "audit_sequence",
    "reserved_policy_version",
  ]) {
    if (!Number.isSafeInteger(meta[field]) || meta[field] < 0) {
      journalFail(`recovered meta ${field} is invalid`,
        "realm_email_alias_recovery_revision_regression");
    }
  }
  for (const [field, expected] of [
    ["registry_revision", options.expected_registry_revision],
    ["audit_sequence", options.expected_audit_sequence],
    ["reserved_policy_version", options.expected_reserved_policy_version],
  ]) {
    if (expected !== undefined && meta[field] !== expected) {
      journalFail(`recovered meta ${field} does not match the expected fence`,
        "realm_email_alias_journal_fence_mismatch");
    }
  }

  const claimsByAlias = new Map();
  const claimsByID = new Map();
  const claimsBySkeleton = new Map();
  const requests = new Map();
  const reservations = new Map();
  const reservationHistories = new Map();
  const audits = new Map();
  const canonicals = new Map();
  const realmCloseFences = new Map();
  const customDomainSubscriptions = new Map();
  const customDomainSyncs = new Map();
  for (const [key, value] of source) {
    assertObject(value, `recovered authority value is invalid: ${key}`);
    if (key.startsWith("custom-domain-subscription:")) {
      const claimID = key.slice("custom-domain-subscription:".length);
      exactKeys(value, new Set([
        "schema_version", "account_id", "realm_id", "realm_label",
        "realm_alias_claim_id", "created_at",
      ]), "custom-domain alias subscription");
      if (value.schema_version !==
            "witself.realm-email-alias-custom-domain-subscription.v1" ||
          !CLAIM_ID_PATTERN.test(claimID) ||
          value.realm_alias_claim_id !== claimID ||
          !ACCOUNT_ID_PATTERN.test(value.account_id ?? "") ||
          !REALM_ID_PATTERN.test(value.realm_id ?? "") ||
          !ALIAS_PATTERN.test(value.realm_label ?? "") ||
          value.realm_label.includes("--") ||
          CANONICAL_REALM_LABEL_PATTERN.test(value.realm_label)) {
        journalFail("recovered custom-domain alias subscription is invalid",
          "realm_email_alias_recovery_invariant_failed");
      }
      validateISODate(value.created_at,
        "custom-domain alias subscription created_at");
      customDomainSubscriptions.set(claimID, value);
    } else if (key.startsWith("custom-domain-sync:")) {
      const claimID = key.slice("custom-domain-sync:".length);
      exactKeys(value, new Set([
        "schema_version", "phase", "claim_proof", "source_fingerprint",
        "failure_count", "retry_at_ms", "created_at", "updated_at",
      ]), "custom-domain alias sync");
      let proof;
      try {
        proof = validateRealmEmailAliasClaimProof(
          value.claim_proof,
          value.claim_proof?.account_id,
          value.claim_proof?.realm_label,
        );
      } catch {
        journalFail("recovered custom-domain alias sync proof is invalid",
          "realm_email_alias_recovery_invariant_failed");
      }
      if (value.schema_version !==
            "witself.realm-email-alias-custom-domain-sync.v1" ||
          !CLAIM_ID_PATTERN.test(claimID) ||
          proof.realm_alias_claim_id !== claimID ||
          !["enqueue", "poll"].includes(value.phase) ||
          value.source_fingerprint !==
            realmEmailAliasClaimRouteFingerprint(proof) ||
          !Number.isSafeInteger(value.failure_count) ||
          value.failure_count < 0 ||
          !Number.isSafeInteger(value.retry_at_ms) || value.retry_at_ms < 0) {
        journalFail("recovered custom-domain alias sync is invalid",
          "realm_email_alias_recovery_invariant_failed");
      }
      validateISODate(value.created_at, "custom-domain alias sync created_at");
      validateISODate(value.updated_at, "custom-domain alias sync updated_at");
      if (Date.parse(value.updated_at) < Date.parse(value.created_at)) {
        journalFail("recovered custom-domain alias sync time regressed",
          "realm_email_alias_recovery_revision_regression");
      }
      customDomainSyncs.set(claimID, value);
    } else if (key.startsWith("claim:")) {
      const alias = key.slice("claim:".length);
      if (!ALIAS_PATTERN.test(alias) || alias.includes("--") ||
          CANONICAL_REALM_LABEL_PATTERN.test(alias) ||
          value.alias !== alias || value.skeleton !== aliasSkeleton(alias) ||
          !CLAIM_ID_PATTERN.test(value.claim_id ?? "") ||
          !ACCOUNT_ID_PATTERN.test(value.account_id ?? "") ||
          !REALM_ID_PATTERN.test(value.realm_id ?? "") ||
          !validDomain(value.domain) ||
          !Number.isSafeInteger(value.assignment_revision) ||
          value.assignment_revision < 0) {
        journalFail(`recovered claim is invalid: ${alias}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      if (claimsByID.has(value.claim_id) ||
          claimsBySkeleton.has(value.skeleton)) {
        journalFail("recovered claims collide by id or skeleton",
          "realm_email_alias_recovery_collision");
      }
      claimsByAlias.set(alias, value);
      claimsByID.set(value.claim_id, value);
      claimsBySkeleton.set(value.skeleton, value);
    } else if (key.startsWith("request:")) {
      const id = key.slice("request:".length);
      if (!REQUEST_ID_PATTERN.test(id) || value.id !== id ||
          !["pending_review", "provisioning", "approved", "rejected"]
            .includes(value.status)) {
        journalFail(`recovered request is invalid: ${id}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      requests.set(id, value);
    } else if (key.startsWith("reserved-history:")) {
      const match = key.match(/^reserved-history:([^:]+):(\d{8})$/);
      const version = match ? Number(match[2]) : 0;
      const alias = match?.[1] ?? "";
      if (!ALIAS_PATTERN.test(alias) || alias.includes("--") ||
          CANONICAL_REALM_LABEL_PATTERN.test(alias) ||
          value.name !== alias ||
          value.skeleton !== aliasSkeleton(alias) ||
          !Number.isSafeInteger(version) || version < 1 ||
          value.version !== version ||
          !Number.isSafeInteger(value.policy_version) ||
          value.policy_version < 1 ||
          value.policy_version > meta.reserved_policy_version) {
        journalFail(`recovered reserved-name history is invalid: ${key}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      const history = reservationHistories.get(alias) ?? new Map();
      history.set(version, value);
      reservationHistories.set(alias, history);
    } else if (key.startsWith("reserved:") &&
        !key.startsWith("reserved-history:")) {
      const alias = key.slice("reserved:".length);
      if (!ALIAS_PATTERN.test(alias) || alias.includes("--") ||
          CANONICAL_REALM_LABEL_PATTERN.test(alias) ||
          value.name !== alias ||
          value.skeleton !== aliasSkeleton(alias) ||
          !Number.isSafeInteger(value.version) || value.version < 1 ||
          !Number.isSafeInteger(value.policy_version) ||
          value.policy_version < 1 || value.policy_version > meta.reserved_policy_version) {
        journalFail(`recovered reserved name is invalid: ${alias}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      if (value.enabled === true) {
        const conflict = [...reservations.values()].find((entry) =>
          entry.enabled === true && entry.skeleton === value.skeleton);
        if (conflict) {
          journalFail("enabled recovered reservations collide by skeleton",
            "realm_email_alias_recovery_collision");
        }
      }
      reservations.set(alias, value);
    } else if (key.startsWith("audit:")) {
      const sequence = Number(key.slice("audit:".length));
      if (!Number.isSafeInteger(sequence) || sequence < 1 ||
          value.sequence !== sequence ||
          !Number.isSafeInteger(value.registry_revision) ||
          value.registry_revision < 1 || value.registry_revision > meta.registry_revision) {
        journalFail(`recovered audit event is invalid: ${key}`,
          "realm_email_alias_recovery_revision_regression");
      }
      audits.set(sequence, value);
    } else if (key.startsWith("canonical:")) {
      const rest = key.slice("canonical:".length);
      const split = rest.lastIndexOf(":");
      const domain = split < 0 ? "" : rest.slice(0, split);
      const label = split < 0 ? "" : rest.slice(split + 1);
      if (!validDomain(domain) || value.domain !== domain ||
          !CANONICAL_REALM_LABEL_PATTERN.test(label) ||
          value.realm_label !== label || !REALM_ID_PATTERN.test(value.realm_id ?? "") ||
          value.realm_id.slice("realm_".length) !== label ||
          !ACCOUNT_ID_PATTERN.test(value.account_id ?? "") ||
          !Number.isSafeInteger(value.controller_revision) ||
          value.controller_revision < 1 ||
          typeof value.updated_at !== "string" ||
          !Number.isFinite(Date.parse(value.updated_at)) ||
          !["applied", "suspended", "retired"].includes(value.state)) {
        journalFail(`recovered canonical route is invalid: ${key}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      canonicals.set(key, value);
    } else if (key.startsWith("plan-intent:")) {
      const accountID = key.slice("plan-intent:".length);
      if (!ACCOUNT_ID_PATTERN.test(accountID) || value.account_id !== accountID ||
          !Number.isSafeInteger(value.plan_revision) ||
          value.plan_revision < 0 ||
          !(value.plan_revision === 0
            ? value.plan_snapshot_hash === ""
            : SHA256_PATTERN.test(value.plan_snapshot_hash ?? "")) ||
          typeof value.feature_enabled !== "boolean" ||
          !(value.alias_limit === null ||
            (Number.isSafeInteger(value.alias_limit) &&
              value.alias_limit >= 0)) ||
          typeof value.activation_enabled !== "boolean" ||
          !["awaiting_cell", "cell_committed",
            "custom_domain_converging"].includes(value.state) ||
          !(value.claim_cursor === null ||
            (typeof value.claim_cursor === "string" &&
              value.claim_cursor.startsWith(`account-claim:${accountID}:`))) ||
          (value.state === "custom_domain_converging" &&
            value.claim_cursor !== null) ||
          !Number.isSafeInteger(value.retry_at_ms) || value.retry_at_ms < 0 ||
          (value.failure_count !== undefined &&
            (!Number.isSafeInteger(value.failure_count) ||
              value.failure_count < 0))) {
        journalFail("recovered realm email alias plan intent is invalid",
          "realm_email_alias_recovery_invariant_failed");
      }
      validateISODate(value.created_at, "alias plan intent created_at");
      validateISODate(value.updated_at, "alias plan intent updated_at");
    } else if (key.startsWith("lifecycle-intent:")) {
      const accountID = key.slice("lifecycle-intent:".length);
      const phase = value.phase ?? "claims";
      if (!ACCOUNT_ID_PATTERN.test(accountID) || value.account_id !== accountID ||
          !OPERATION_ID_PATTERN.test(value.operation_id ?? "") ||
          !Number.isSafeInteger(value.epoch) || value.epoch < 0 ||
          !["suspend", "republish", "retire"].includes(value.action) ||
          typeof value.activation_enabled !== "boolean" ||
          !["claims", "canonical", "custom_domain_converging"]
            .includes(phase) ||
          !(value.claim_cursor === null ||
            (typeof value.claim_cursor === "string" &&
              value.claim_cursor.startsWith(`account-claim:${accountID}:`))) ||
          !(value.canonical_cursor === null ||
            (typeof value.canonical_cursor === "string" &&
              value.canonical_cursor.startsWith(
                `account-canonical:${accountID}:`,
              ))) ||
          (phase === "custom_domain_converging" &&
            (value.claim_cursor !== null || value.canonical_cursor !== null)) ||
          !Number.isSafeInteger(value.retry_at_ms) || value.retry_at_ms < 0 ||
          !Number.isSafeInteger(value.failure_count) ||
          value.failure_count < 0) {
        journalFail("recovered realm email alias lifecycle intent is invalid",
          "realm_email_alias_recovery_invariant_failed");
      }
      validateISODate(value.created_at, "alias lifecycle intent created_at");
      validateISODate(value.updated_at, "alias lifecycle intent updated_at");
    } else if (key.startsWith("realm-close-intent:") ||
        key.startsWith("realm-close-fence:")) {
      const prefix = key.startsWith("realm-close-intent:")
        ? "realm-close-intent:"
        : "realm-close-fence:";
      const parts = key.slice(prefix.length).split(":");
      if (parts.length !== 2 || !ACCOUNT_ID_PATTERN.test(parts[0]) ||
          !REALM_ID_PATTERN.test(parts[1]) || value.account_id !== parts[0] ||
          value.realm_id !== parts[1] ||
          (prefix === "realm-close-intent:" &&
            (!["scan_aliases", "custom_domain_converging", "prepare_cell",
              "publish_retired", "commit_cell"]
              .includes(value.phase) || !validDomain(value.domain) ||
              (value.custom_domain_cursor !== undefined &&
                value.custom_domain_cursor !== null &&
                (typeof value.custom_domain_cursor !== "string" ||
                  !value.custom_domain_cursor.startsWith(
                    `custom-domain-subscription-realm:${parts[0]}:${parts[1]}:`,
                  ))) ||
              (value.domains !== undefined &&
                !validRealmCloseDomains(value.domains, value.domain)) ||
              (value.publish_domain_index !== undefined &&
                (!Number.isSafeInteger(value.publish_domain_index) ||
                  value.publish_domain_index < 0 ||
                  value.publish_domain_index > (value.domains?.length ?? 1))))) ||
          (prefix === "realm-close-fence:" &&
            (!Number.isSafeInteger(value.controller_revision) ||
              value.controller_revision < 1 ||
              (value.canonical_revisions !== undefined &&
                !validCanonicalRevisions(
                  value.canonical_revisions,
                  value.controller_revision,
                ))))) {
        journalFail(`recovered realm-close authority is invalid: ${key}`,
          "realm_email_alias_recovery_invariant_failed");
      }
      if (prefix === "realm-close-fence:") {
        realmCloseFences.set(`${parts[0]}:${parts[1]}`, value);
      }
    }
  }

  for (const [claimID, subscription] of customDomainSubscriptions) {
    const claim = claimsByID.get(claimID);
    if (!claim || claim.account_id !== subscription.account_id ||
        claim.realm_id !== subscription.realm_id ||
        claim.alias !== subscription.realm_label || !claim.assignment_kind) {
      journalFail("recovered custom-domain alias subscription is orphaned",
        "realm_email_alias_recovery_collision");
    }
  }
  for (const [claimID, sync] of customDomainSyncs) {
    const subscription = customDomainSubscriptions.get(claimID);
    const claim = claimsByID.get(claimID);
    let proof = null;
    try {
      proof = recoveredClaimProof(claim);
    } catch {
      // The graph-level failure below is intentionally uniform and does not
      // expose claim data through recovery diagnostics.
    }
    if (!subscription || !proof ||
        proof.account_id !== sync.claim_proof.account_id ||
        proof.realm_id !== sync.claim_proof.realm_id ||
        proof.realm_label !== sync.claim_proof.realm_label ||
        proof.realm_alias_claim_id !==
          sync.claim_proof.realm_alias_claim_id ||
        realmEmailAliasClaimRouteFingerprint(proof) !==
          sync.source_fingerprint) {
      journalFail("recovered custom-domain alias sync is orphaned",
        "realm_email_alias_recovery_collision");
    }
  }

  for (const fence of realmCloseFences.values()) {
    const realmCanonicals = [...canonicals.values()].filter((canonical) =>
      canonical.account_id === fence.account_id &&
      canonical.realm_id === fence.realm_id
    );
    if (realmCanonicals.length === 0) {
      journalFail("recovered realm-close fence has no canonical route",
        "realm_email_alias_recovery_invariant_failed");
    }
    if (realmCanonicals.some((canonical) => canonical.state !== "retired")) {
      journalFail("recovered fenced realm has a nonretired canonical route",
        "realm_email_alias_recovery_invariant_failed");
    }
    if (Array.isArray(fence.canonical_revisions)) {
      const realmLabel = fence.realm_id.slice("realm_".length);
      for (const expected of fence.canonical_revisions) {
        const canonical = canonicals.get(
          `canonical:${expected.domain}:${realmLabel}`,
        );
        if (!canonical) {
          journalFail("recovered realm-close canonical route is missing",
            "realm_email_alias_recovery_invariant_failed");
        }
        if (canonical.account_id !== fence.account_id ||
            canonical.realm_id !== fence.realm_id) {
          journalFail("recovered realm-close canonical ownership is inconsistent",
            "realm_email_alias_recovery_invariant_failed");
        }
        if (canonical.state !== "retired" ||
            canonical.controller_revision !== expected.controller_revision) {
          journalFail("recovered realm-close canonical revision is inconsistent",
            "realm_email_alias_recovery_invariant_failed");
        }
      }
    } else if (!realmCanonicals.some((canonical) =>
      canonical.controller_revision === fence.controller_revision
    )) {
      // Legacy fences predate the per-domain revision list. Their one scalar
      // still has to name an exact retired row in the same account and realm.
      journalFail("recovered legacy realm-close revision is inconsistent",
        "realm_email_alias_recovery_invariant_failed");
    }
  }

  for (const claim of claimsByAlias.values()) {
    if (claim.request_id !== null && claim.request_id !== undefined) {
      const request = requests.get(claim.request_id);
      if (!request || request.alias !== claim.alias ||
          request.account_id !== claim.account_id ||
          request.realm_id !== claim.realm_id || request.domain !== claim.domain) {
        journalFail("recovered request and claim graph is inconsistent",
          "realm_email_alias_recovery_invariant_failed");
      }
    }
  }
  for (const request of requests.values()) {
    const claim = claimsByAlias.get(request.alias);
    if (!claim || claim.request_id !== request.id) {
      journalFail("recovered request has no matching claim",
        "realm_email_alias_recovery_invariant_failed");
    }
  }
  if (audits.size !== meta.audit_sequence) {
    journalFail("recovered audit sequence has a gap",
      "realm_email_alias_recovery_revision_regression");
  }
  let previousRegistryRevision = 0;
  for (let sequence = 1; sequence <= meta.audit_sequence; sequence += 1) {
    const audit = audits.get(sequence);
    if (!audit || audit.registry_revision < previousRegistryRevision) {
      journalFail("recovered audit sequence is not monotonic",
        "realm_email_alias_recovery_revision_regression");
    }
    previousRegistryRevision = audit.registry_revision;
  }
  if (meta.audit_sequence > 0 &&
      previousRegistryRevision !== meta.registry_revision) {
    journalFail("recovered audit head does not match the registry revision",
      "realm_email_alias_recovery_revision_regression");
  }
  for (const reservation of reservations.values()) {
    const history = reservationHistories.get(reservation.name);
    if (!history || history.size !== reservation.version) {
      journalFail("recovered reserved-name history is incomplete",
        "realm_email_alias_recovery_invariant_failed");
    }
    let previousPolicyVersion = 0;
    for (const [index, [version, historical]] of
      [...history].sort(([left], [right]) => left - right).entries()) {
      if (version !== index + 1 ||
          historical.policy_version < previousPolicyVersion ||
          immutableFieldsChanged(reservation, historical, [
            "name", "skeleton", "created_at", "created_by",
          ])) {
        journalFail("recovered reserved-name history is not monotonic",
          "realm_email_alias_recovery_revision_regression");
      }
      previousPolicyVersion = historical.policy_version;
    }
    if (canonicalJSONString(history.get(reservation.version)) !==
        canonicalJSONString(reservation)) {
      journalFail("recovered reserved-name current version does not match history",
        "realm_email_alias_recovery_invariant_failed");
    }
  }
  for (const alias of reservationHistories.keys()) {
    if (!reservations.has(alias)) {
      journalFail("recovered reserved-name history is orphaned",
        "realm_email_alias_recovery_invariant_failed");
    }
  }
  return {
    registry_revision: meta.registry_revision,
    audit_sequence: meta.audit_sequence,
    reserved_policy_version: meta.reserved_policy_version,
    claims: claimsByAlias.size,
    requests: requests.size,
    reserved_names: reservations.size,
    canonical_routes: canonicals.size,
    authority_keys: source.size,
  };
}

function usageContribution(claim) {
  if (claim.retired_at || claim.assignment_kind === "internal") return null;
  if (claim.assignment_kind === "customer") {
    return {
      claim_id: claim.claim_id,
      account_id: claim.account_id,
      realm_id: claim.realm_id,
      open_request: claim.customer_activation_intent === true ? 1 : 0,
      pending_review: 0,
      provisioning: claim.customer_activation_intent === true ? 1 : 0,
      customer_allocated: 1,
    };
  }
  if (claim.assignment_kind == null &&
      REQUEST_ID_PATTERN.test(claim.request_id ?? "")) {
    return {
      claim_id: claim.claim_id,
      account_id: claim.account_id,
      realm_id: claim.realm_id,
      open_request: 1,
      pending_review: 1,
      provisioning: 0,
      customer_allocated: 0,
    };
  }
  return null;
}

function usageIntegrity(record) {
  return JSON.stringify({
    schema_version: record.schema_version,
    account_id: record.account_id,
    realm_id: record.realm_id ?? null,
    open_requests: record.open_requests,
    pending_review: record.pending_review ?? null,
    provisioning: record.provisioning ?? null,
    customer_allocated: record.customer_allocated ?? null,
  });
}

function dueSegment(value, fallback) {
  const candidate = Number.isSafeInteger(value) ? value : fallback;
  return String(candidate).padStart(16, "0");
}

export function rebuildRealmEmailAliasDerivedState(state, options = {}) {
  const source = toAuthorityStateMap(state);
  validateRealmEmailAliasRecoveredState(source);
  const retryAt = options.retry_at_ms ?? Date.now();
  if (!Number.isSafeInteger(retryAt) || retryAt < 0) {
    journalFail("derived-state retry_at_ms is invalid");
  }
  const updatedAt = options.updated_at ?? new Date(retryAt).toISOString();
  validateISODate(updatedAt, "derived-state updated_at");
  const derived = new Map();
  const accountUsage = new Map();
  const realmUsage = new Map();

  for (const [key, value] of [...source].sort(([left], [right]) =>
    left.localeCompare(right))) {
    if (key.startsWith("custom-domain-subscription:")) {
      derived.set(
        `custom-domain-subscription-realm:${value.account_id}:` +
          `${value.realm_id}:${value.realm_alias_claim_id}`,
        value.realm_label,
      );
    } else if (key.startsWith("custom-domain-sync:")) {
      derived.set(
        `custom-domain-sync-account:${value.claim_proof.account_id}:` +
          value.claim_proof.realm_alias_claim_id,
        key,
      );
      derived.set(
        `custom-domain-sync-due:${dueSegment(value.retry_at_ms, retryAt)}:` +
          value.claim_proof.realm_alias_claim_id,
        value.claim_proof.realm_alias_claim_id,
      );
    } else if (key.startsWith("claim:")) {
      derived.set(`claim-skeleton:${value.skeleton}`, value.alias);
      derived.set(
        `account-claim:${value.account_id}:${value.realm_id}:` +
          `${value.created_at}:${value.alias}`,
        value.alias,
      );
      if (value.plan_grace_until) {
        const deadline = Number.isSafeInteger(value.grace_retry_at_ms)
          ? value.grace_retry_at_ms
          : Date.parse(value.plan_grace_until);
        derived.set(`grace:${dueSegment(deadline, retryAt)}:${value.alias}`,
          value.alias);
      }
      if (value.customer_activation_intent === true) {
        derived.set(
          `approval-due:${dueSegment(value.approval_retry_at_ms, retryAt)}:${value.alias}`,
          value.alias,
        );
      }
      if (value.internal_intent === true) {
        derived.set(
          `internal-due:${dueSegment(value.internal_retry_at_ms, retryAt)}:${value.alias}`,
          value.alias,
        );
      }
      const contribution = usageContribution(value);
      if (contribution) {
        derived.set(`claim-usage-member:${value.claim_id}`, {
          schema_version: 1,
          ...contribution,
          updated_at: updatedAt,
        });
        derived.set(
          `claim-usage-account-member:${value.account_id}:${value.claim_id}`,
          value.claim_id,
        );
        derived.set(
          `claim-usage-realm-member:${value.account_id}:${value.realm_id}:${value.claim_id}`,
          value.claim_id,
        );
        const account = accountUsage.get(value.account_id) ?? { open_requests: 0 };
        account.open_requests += contribution.open_request;
        accountUsage.set(value.account_id, account);
        const realmKey = `${value.account_id}:${value.realm_id}`;
        const realm = realmUsage.get(realmKey) ?? {
          account_id: value.account_id,
          realm_id: value.realm_id,
          open_requests: 0,
          pending_review: 0,
          provisioning: 0,
          customer_allocated: 0,
        };
        realm.open_requests += contribution.open_request;
        realm.pending_review += contribution.pending_review;
        realm.provisioning += contribution.provisioning;
        realm.customer_allocated += contribution.customer_allocated;
        realmUsage.set(realmKey, realm);
      }
    } else if (key.startsWith("request:")) {
      derived.set(
        `account-request:${value.account_id}:${value.realm_id}:${value.id}`,
        value.id,
      );
    } else if (key.startsWith("reserved:") &&
        !key.startsWith("reserved-history:") && value.enabled === true) {
      derived.set(`reserved-skeleton:${value.skeleton}`, value.name);
    } else if (key.startsWith("canonical:")) {
      derived.set(
        `account-canonical:${value.account_id}:${value.domain}:${value.realm_label}`,
        key,
      );
      derived.set(
        `realm-canonical:${value.account_id}:${value.realm_id}:` +
          `${value.domain}:${value.realm_label}`,
        key,
      );
    } else if (key.startsWith("projection-intent:")) {
      derived.set(
        `projection-due:${dueSegment(value.retry_at_ms, retryAt)}:${value.alias}`,
        value.alias,
      );
    } else if (key.startsWith("plan-intent:")) {
      derived.set(
        `plan-due:${dueSegment(value.retry_at_ms, retryAt)}:${value.account_id}`,
        value.account_id,
      );
    } else if (key.startsWith("lifecycle-intent:")) {
      derived.set(
        `lifecycle-due:${dueSegment(value.retry_at_ms, retryAt)}:${value.account_id}`,
        value.account_id,
      );
    } else if (key.startsWith("realm-close-intent:")) {
      derived.set(
        `realm-close-due:${dueSegment(value.retry_at_ms, retryAt)}:` +
          `${value.account_id}:${value.realm_id}`,
        `${value.account_id}:${value.realm_id}`,
      );
    } else if (key.startsWith("route-refresh:")) {
      derived.set(
        `refresh-due:${dueSegment(value.retry_at_ms, retryAt)}:` +
          `${value.domain}:${value.realm_label}`,
        key,
      );
    }
  }

  for (const [accountID, usage] of accountUsage) {
    const record = {
      schema_version: 1,
      account_id: accountID,
      open_requests: usage.open_requests,
      updated_at: updatedAt,
    };
    derived.set(`claim-usage-account:${accountID}`, {
      ...record,
      integrity: usageIntegrity(record),
    });
  }
  for (const usage of realmUsage.values()) {
    const record = {
      schema_version: 1,
      account_id: usage.account_id,
      realm_id: usage.realm_id,
      open_requests: usage.open_requests,
      pending_review: usage.pending_review,
      provisioning: usage.provisioning,
      customer_allocated: usage.customer_allocated,
      updated_at: updatedAt,
    };
    derived.set(`claim-usage-realm:${usage.account_id}:${usage.realm_id}`, {
      ...record,
      integrity: usageIntegrity(record),
    });
  }
  return derived;
}
