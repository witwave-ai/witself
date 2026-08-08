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
  "request:",
]);
const DERIVED_PREFIXES = Object.freeze([
  "account-request:",
  "account-usage:",
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
    return {
      key: put.key,
      value: JSON.parse(canonicalJSONString(put.value)),
    };
  }).sort((left, right) => left.key.localeCompare(right.key));
  for (const key of afterImage.deletes) {
    if (!isAgentEmailDomainAuthorityKey(key)) {
      journalFail(`after_image key is not canonical authority: ${String(key)}`);
    }
    // Every current authority row is permanent. Requests and domains are
    // tombstones, while audits and idempotency receipts are append-only.
    journalFail(`after_image cannot delete canonical authority key: ${key}`);
  }
  return { puts, deletes: [] };
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
    pending_verification: new Set(["pending_verification", "rejected", "retired"]),
    rejected: new Set(["rejected", "retired"]),
    retired: new Set(["retired"]),
  };
  if (!allowed[previous.state]?.has(desired.state)) {
    journalFail("custom domain tombstone was resurrected",
      "agent_email_domain_recovery_tombstone_resurrection");
  }
  if (previous.state === desired.state) {
    if (canonicalJSONString(previous) !== canonicalJSONString(desired)) {
      journalFail("same-state custom domain authority cannot be overwritten",
        "agent_email_domain_recovery_invariant_failed");
    }
    return;
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
  if (key.startsWith("request:") || key.startsWith("domain:")) {
    assertRequestTransition(previous, desired);
  }
}

function applyNormalizedAfterImage(state, normalized) {
  for (const { key, value } of normalized.puts) {
    assertAuthorityTransition(state, key, value);
    state.set(key, JSON.parse(canonicalJSONString(value)));
  }
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
      !["pending_verification", "rejected", "retired"].includes(value.state) ||
      !ACTOR_ID_PATTERN.test(value.requested_by ?? "") ||
      !Number.isSafeInteger(value.plan_revision) || value.plan_revision < 0 ||
      !SHA256_PATTERN.test(value.plan_snapshot_hash ?? "") ||
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
      ) || challenge.issued_at !== value.requested_at) {
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
  if ((value.state === "pending_verification" &&
        (decision != null || retirement != null)) ||
      (value.state === "rejected" &&
        (decision == null || retirement != null)) ||
      (value.state === "retired" && retirement == null)) {
    recoveryFail("recovered custom domain lifecycle evidence is inconsistent");
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
      !["custom_domain.requested", "custom_domain.rejected", "custom_domain.retired"]
        .includes(value.action) || !validDomain(value.target) ||
      !isPlainObject(value.metadata) ||
      !ACCOUNT_ID_PATTERN.test(value.metadata.account_id ?? "")) {
    recoveryFail(`recovered custom domain audit event is invalid: ${key}`,
      "agent_email_domain_recovery_revision_regression");
  }
  validateISODate(value.occurred_at, "audit occurred_at");
  return value;
}

function validateIdempotency(key, value) {
  assertObject(value, `recovered idempotency receipt is invalid: ${key}`);
  if (typeof value.fingerprint !== "string" || value.fingerprint.length < 2 ||
      value.fingerprint.length > 2_048 ||
      !Number.isSafeInteger(value.status) || value.status < 200 ||
      value.status > 299 || !isPlainObject(value.body) ||
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
      validRequestRecord(value);
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
    }
  }

  if (requests.size !== domains.size) {
    recoveryFail("recovered request and domain tombstone counts differ");
  }
  for (const request of requests.values()) {
    const tombstone = domains.get(request.domain);
    if (!tombstone || canonicalJSONString(tombstone) !== canonicalJSONString(request)) {
      recoveryFail("recovered request and domain tombstone graph is inconsistent",
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

  const auditByDomain = new Map();
  for (const audit of [...audits.values()].sort((left, right) =>
    left.sequence - right.sequence)) {
    const request = domains.get(audit.target);
    if (!request || request.account_id !== audit.metadata.account_id) {
      recoveryFail("recovered custom domain audit graph is inconsistent");
    }
    const evidence = audit.action === "custom_domain.requested"
      ? {
        time: request.requested_at,
        actor_kind: "account_operator",
        actor_id: request.requested_by,
        state: "pending_verification",
      }
      : audit.action === "custom_domain.rejected"
      ? {
        time: request.decision?.decided_at,
        actor_kind: "platform_admin",
        actor_id: request.decision?.decided_by,
        reason: request.decision?.reason,
      }
      : {
        time: request.retirement?.retired_at,
        actor_kind: "platform_admin",
        actor_id: request.retirement?.retired_by,
        reason: request.retirement?.reason,
      };
    if (audit.occurred_at !== evidence.time ||
        audit.actor_kind !== evidence.actor_kind ||
        audit.actor_id !== evidence.actor_id ||
        (evidence.state !== undefined &&
          audit.metadata.state !== evidence.state) ||
        (evidence.reason !== undefined &&
          audit.metadata.reason !== evidence.reason)) {
      recoveryFail("recovered custom domain audit evidence is inconsistent");
    }
    if (audit.action === "custom_domain.rejected" &&
        audit.metadata.from_state !== "pending_verification") {
      recoveryFail("recovered custom domain rejection origin is inconsistent");
    }
    if (audit.action === "custom_domain.retired") {
      const expectedOrigin = request.decision == null
        ? "pending_verification"
        : "rejected";
      if (audit.metadata.from_state !== expectedOrigin) {
        recoveryFail("recovered custom domain retirement origin is inconsistent");
      }
    }
    const actions = auditByDomain.get(audit.target) ?? [];
    actions.push(audit.action);
    auditByDomain.set(audit.target, actions);
  }
  for (const request of requests.values()) {
    const actions = auditByDomain.get(request.domain) ?? [];
    const expected = request.state === "pending_verification"
      ? ["custom_domain.requested"]
      : request.state === "rejected"
      ? ["custom_domain.requested", "custom_domain.rejected"]
      : request.decision == null
      ? ["custom_domain.requested", "custom_domain.retired"]
      : [
        "custom_domain.requested",
        "custom_domain.rejected",
        "custom_domain.retired",
      ];
    if (canonicalJSONString(actions) !== canonicalJSONString(expected)) {
      recoveryFail("recovered custom domain audit lifecycle is inconsistent");
    }
    const requestReceipts = receipts.filter(({ request: receipt }) =>
      receipt.id === request.id);
    const receiptActions = requestReceipts.map(({ action }) => action).sort();
    const expectedReceipts = request.state === "pending_verification"
      ? ["requested"]
      : request.state === "rejected"
      ? ["rejected", "requested"]
      : request.decision == null
      ? ["requested", "retired"]
      : ["rejected", "requested", "retired"];
    if (canonicalJSONString(receiptActions) !==
        canonicalJSONString(expectedReceipts.sort())) {
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
    const count = usage.get(request.account_id) ?? {
      count: 0,
      updated_at: null,
    };
    usage.set(
      request.account_id,
      {
        count: count.count +
          (request.state === "pending_verification" ? 1 : 0),
        updated_at: count.updated_at,
      },
    );
  }
  for (const audit of [...source]
    .filter(([key]) => key.startsWith("audit:"))
    .map(([, value]) => value)
    .sort((left, right) => left.sequence - right.sequence)) {
    const changesPending = audit.action === "custom_domain.requested" ||
      (["custom_domain.rejected", "custom_domain.retired"].includes(audit.action) &&
        audit.metadata.from_state === "pending_verification");
    if (changesPending) {
      const account = usage.get(audit.metadata.account_id);
      if (!account) {
        recoveryFail("recovered custom domain usage audit is orphaned");
      }
      account.updated_at = audit.occurred_at;
    }
  }
  for (const [accountID, account] of usage) {
    if (!account.updated_at) {
      recoveryFail("recovered custom domain usage timestamp is missing");
    }
    derived.set(`account-usage:${accountID}`, {
      schema_version: 1,
      account_id: accountID,
      open_requests: account.count,
      updated_at: account.updated_at,
    });
  }
  return derived;
}
