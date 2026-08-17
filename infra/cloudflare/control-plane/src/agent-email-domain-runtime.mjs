import {
  AgentEmailDomainJournalRuntime,
  AgentEmailDomainJournalRuntimeError,
} from "./agent-email-domain-journal-runtime.mjs";
import {
  agentEmailDomainResolvedObservation,
  agentEmailDomainTemporaryObservation,
  resolveAgentEmailDomainTXT,
} from "./agent-email-domain-verification.mjs";
import {
  AGENT_EMAIL_DOMAIN_VERIFICATION_REFRESH_SCHEMA_VERSION,
  canonicalJSONString,
  isAgentEmailDomainVerificationRefresh,
} from "./agent-email-domain-journal.mjs";
import {
  AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_INTENT_SCHEMA_VERSION,
  agentEmailCustomDomainRouteKey,
  buildAgentEmailCustomDomainCellProjection,
  buildAgentEmailCustomDomainRouteProjection,
  cellAgentEmailCustomDomainRouteURL,
  realmEmailAliasClaimRouteFingerprint,
  validateAgentEmailCustomDomainCellProjection,
  validateRealmEmailAliasClaimProof,
} from "./agent-email-custom-domain-route-contract.mjs";
import {
  signAgentEmailRouteProjection,
} from "./agent-email-route-signature.mjs";

const SCHEMA_VERSION = "witself.agent-email-domain.v1";
const RECOVERY_SCHEMA_VERSION = "witself.agent-email-domain-recovery.v1";
const META_KEY = "meta";
const DEFAULT_REGISTRY_OBJECT_NAME = "global";
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const ACTOR_ID_PATTERN = /^[A-Za-z0-9._:@-]{1,128}$/;
const REQUEST_ID_PATTERN = /^aedr_[a-z2-7]{16}$/;
const CHALLENGE_TOKEN_PATTERN = /^aedv_[a-z2-7]{32}$/;
const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const DOMAIN_LABEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const MAX_REASON_LENGTH = 500;
const DEFAULT_LIST_LIMIT = 50;
const MAX_CHALLENGE_DOMAIN_LENGTH = 231;
const MAX_REQUEST_ID_MINT_ATTEMPTS = 8;
// Keep the established dark/no-binding page size. Once sparse custom-route
// bindings exist, a smaller page leaves journal room for each request's
// durable convergence obligation under the hard 100-authority-change limit.
const RECONCILE_PAGE_LIMIT = 40;
const ROUTE_AWARE_RECONCILE_PAGE_LIMIT = 30;
const VERIFICATION_RECONCILE_LIMIT = 5;
const VERIFICATION_WORK_SCHEMA =
  "witself.agent-email-domain-verification-work.v1";
const VERIFICATION_CLAIM_PATTERN = /^aedvc_[a-z2-7]{32}$/;
const VERIFICATION_CLAIM_LEASE_MS = 60 * 1_000;
const VERIFICATION_OBSERVATION_MAX_AGE_MS = 10 * 60 * 1_000;
const VERIFICATION_CLAIM_SCAN_LIMIT = 32;
const VERIFICATION_WORKER_CONCURRENCY = 2;
const VERIFICATION_INTERVAL_MS = 24 * 60 * 60 * 1_000;
const VERIFICATION_RETRY_MS = 60 * 60 * 1_000;
const VERIFICATION_RESOLVER_RETRY_MS = 15 * 60 * 1_000;
const DOWNGRADE_GRACE_MS = 30 * 24 * 60 * 60 * 1_000;
const PENDING_CHALLENGE_TTL_MS = 7 * 24 * 60 * 60 * 1_000;
const RECONCILE_RETRY_MS = 1_000;
const RECONCILE_MAX_RETRY_MS = 15 * 60 * 1_000;
const ALARM_BATCH_LIMIT = 5;
const ROUTE_PROJECTION_RETRY_MS = 5 * 60 * 1_000;
const ROUTE_PROJECTION_MAX_RETRY_MS = 60 * 60 * 1_000;
const ROUTE_CONVERGENCE_PAGE_LIMIT = 4;
const ROUTE_CONVERGENCE_RETRY_MS = 1_000;
const ROUTE_CONVERGENCE_MAX_RETRY_MS = 60 * 60 * 1_000;
const ROUTE_SOURCE_INTENT_SCHEMA =
  "witself.agent-email-custom-domain-route-source-intent.v1";
const ROUTE_ALIAS_TASK_SCHEMA =
  "witself.agent-email-custom-domain-route-alias-task.v1";
const ROUTE_BINDING_SCHEMA =
  "witself.agent-email-custom-domain-route-binding.v1";
const REALM_ALIAS_CLAIM_PAGE_SCHEMA =
  "witself.realm-email-alias-claim-page.v1";

function reconcileRetryDelay(failureCount) {
  return Math.min(
    RECONCILE_RETRY_MS * 2 ** Math.min(Math.max(failureCount - 1, 0), 10),
    RECONCILE_MAX_RETRY_MS,
  );
}

function routeProjectionRetryDelay(failureCount) {
  return Math.min(
    ROUTE_PROJECTION_RETRY_MS *
      2 ** Math.min(Math.max(failureCount - 1, 0), 8),
    ROUTE_PROJECTION_MAX_RETRY_MS,
  );
}

function routeConvergenceRetryDelay(failureCount) {
  return Math.min(
    ROUTE_CONVERGENCE_RETRY_MS *
      2 ** Math.min(Math.max(failureCount - 1, 0), 12),
    ROUTE_CONVERGENCE_MAX_RETRY_MS,
  );
}

export const AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE =
  "agent_email_custom_domain";
export const AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT =
  "agent_email_custom_domains_per_account";
export const AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT = 8;
export const AGENT_EMAIL_CUSTOM_DOMAIN_MAX_LIST_LIMIT = 100;
export const AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS = 30;
export const AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_INTERVAL_HOURS = 24;
export const AGENT_EMAIL_CUSTOM_DOMAIN_PENDING_CHALLENGE_DAYS = 7;

// These roots are part of Witself's own product and operating surface. A
// customer can never request one of them, or a child of one of them, as a
// custom inbound domain. Deployment configuration can add more roots without
// weakening these compiled protections.
export const INITIAL_PROTECTED_AGENT_EMAIL_DOMAINS = Object.freeze([
  "witmail.net",
  "witmail.ai",
  "witself.cloud",
  "witself.com",
  "witself.dev",
  "witself.io",
  "witwave.ai",
]);

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(
  message,
  status,
  code = "",
  details = {},
  schemaVersion = SCHEMA_VERSION,
) {
  return json({
    schema_version: schemaVersion,
    error: message,
    ...(code ? { code } : {}),
    ...details,
  }, status);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

class DomainRegistryError extends Error {
  constructor(message, status = 500, code = "", details = {}) {
    super(message);
    this.name = "DomainRegistryError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

function fail(message, status = 500, code = "", details = {}) {
  throw new DomainRegistryError(message, status, code, details);
}

function randomBase32(length) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  const bytes = new Uint8Array(length);
  crypto.getRandomValues(bytes);
  return [...bytes].map((byte) => alphabet[byte & 31]).join("");
}

function newRequestID() {
  return `aedr_${randomBase32(16)}`;
}

function newChallengeToken() {
  return `aedv_${randomBase32(32)}`;
}

function newVerificationClaimID() {
  return `aedvc_${randomBase32(32)}`;
}

/**
 * Canonicalize a customer-supplied DNS name without invoking URL, IDNA, or
 * public-suffix machinery. Unicode and punycode are both intentionally out of
 * scope until the product has an explicit homograph policy.
 */
export function normalizeAgentEmailCustomDomain(value) {
  if (typeof value !== "string") {
    fail("domain must be a string", 400);
  }
  const trimmed = value.trim();
  // The public TXT owner prepends `_witself-verification.` (22 bytes). Keep
  // the derived absolute DNS name within the 253-byte presentation limit.
  if (trimmed.length < 3 || trimmed.length > MAX_CHALLENGE_DOMAIN_LENGTH ||
      !/^[\x00-\x7f]+$/.test(trimmed) || trimmed.includes("*")) {
    fail("custom inbound email domain is invalid", 400);
  }
  const domain = trimmed.toLowerCase();
  const labels = domain.split(".");
  if (labels.length < 2 ||
      labels.some((label) => !DOMAIN_LABEL_PATTERN.test(label)) ||
      labels.some((label) => label.startsWith("xn--")) ||
      !/[a-z]/.test(labels.at(-1))) {
    fail("custom inbound email domain is invalid", 400);
  }
  return domain;
}

function configuredProtectedDomains(env = {}) {
  const values = [...INITIAL_PROTECTED_AGENT_EMAIL_DOMAINS];
  const primary = String(env.AGENT_EMAIL_DOMAIN ?? "");
  const legacy = String(env.AGENT_EMAIL_LEGACY_DOMAINS ?? "");
  const additional = String(env.AGENT_EMAIL_PROTECTED_DOMAINS ?? "");
  if (primary) values.push(primary);
  for (const raw of [legacy, additional]) {
    if (!raw) continue;
    if (raw !== raw.trim() || raw.split(",").some((value) => !value)) {
      fail("protected email domain configuration is invalid", 503);
    }
    values.push(...raw.split(","));
  }
  try {
    return Object.freeze([...new Set(
      values.map((value) => normalizeAgentEmailCustomDomain(value)),
    )]);
  } catch (error) {
    if (error instanceof DomainRegistryError && error.status === 400) {
      fail("protected email domain configuration is invalid", 503);
    }
    throw error;
  }
}

export function isProtectedAgentEmailDomain(domain, env = {}) {
  const normalized = normalizeAgentEmailCustomDomain(domain);
  return configuredProtectedDomains(env).some((root) =>
    normalized === root || normalized.endsWith(`.${root}`)
  );
}

export function agentEmailCustomDomainEntitlement(snapshot) {
  const features = Array.isArray(snapshot?.features) ? snapshot.features : [];
  const rawLimit = snapshot?.limits?.[AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT];
  const limit = rawLimit == null
    ? null
    : Number.isSafeInteger(rawLimit) && rawLimit >= 0
    ? rawLimit
    : 0;
  return {
    enabled: features.includes(AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE) &&
      (limit === null || limit > 0),
    limit,
  };
}

function validDomainLimit(value) {
  return value === null || (Number.isSafeInteger(value) && value >= 0);
}

function validPlanRevisionFence(revision, snapshotHash) {
  return Number.isSafeInteger(revision) && revision >= 0 &&
    (revision === 0
      ? snapshotHash === ""
      : typeof snapshotHash === "string" && /^[0-9a-f]{64}$/.test(snapshotHash));
}

function comparePlanRevision(left, right) {
  return left === right ? 0 : left < right ? -1 : 1;
}

export function agentEmailCustomDomainOpenRequestLimit(env = {}) {
  const raw = env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT;
  if (raw === undefined || raw === null || raw === "") {
    return AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT;
  }
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < 1 ||
      value > AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT) {
    fail("custom inbound domain open-request configuration is invalid", 503);
  }
  return value;
}

function validatePlanFence(input) {
  if (!validPlanRevisionFence(
    input?.plan_revision,
    input?.plan_snapshot_hash,
  )) {
    fail("custom inbound domain plan fence is invalid", 400);
  }
  return {
    revision: input.plan_revision,
    snapshot_hash: input.plan_snapshot_hash,
  };
}

function validateActor(actor, expectedKind) {
  if (!isObject(actor) || actor.kind !== expectedKind ||
      !ACTOR_ID_PATTERN.test(actor.id ?? "")) {
    fail("invalid mutation actor", 400);
  }
  return { kind: actor.kind, id: actor.id };
}

function validateAccountID(value) {
  if (!ACCOUNT_ID_PATTERN.test(value ?? "")) {
    fail("invalid account_id", 400);
  }
  return value;
}

function validateRequestID(value) {
  if (!REQUEST_ID_PATTERN.test(value ?? "")) {
    fail("invalid request_id", 400);
  }
  return value;
}

function validateIdempotencyKey(value) {
  if (!IDEMPOTENCY_KEY_PATTERN.test(value ?? "")) {
    fail("idempotency_key is required", 400);
  }
  return value;
}

function validateReason(value) {
  if (typeof value !== "string" || value.trim().length < 1 ||
      value.trim().length > MAX_REASON_LENGTH) {
    fail("reason is required and must be at most 500 characters", 400);
  }
  return value.trim();
}

function validateListLimit(value) {
  if (value == null) return DEFAULT_LIST_LIMIT;
  if (!Number.isSafeInteger(value) || value < 1 ||
      value > AGENT_EMAIL_CUSTOM_DOMAIN_MAX_LIST_LIMIT) {
    fail("list limit is invalid", 400);
  }
  return value;
}

function fingerprint(value) {
  return JSON.stringify(value);
}

function sameVerificationAuditOutcome(previous, desired) {
  if (!previous || !desired) return false;
  // Check clocks, retry counts, and recursive-resolver TTLs naturally move
  // while the authoritative outcome remains identical. Repeated scheduled
  // drift belongs in bounded journal-local refresh state, not authority.
  const evidence = (value) => ({
    state: value.state,
    last_result: value.last_result,
    first_verified_at: value.first_verified_at ?? null,
    rrset_sha256: value.rrset_sha256 ?? null,
    dnssec_authenticated: value.dnssec_authenticated === true,
  });
  return canonicalJSONString(evidence(previous)) ===
    canonicalJSONString(evidence(desired));
}

function validVerificationObservation(value) {
  if (!isObject(value)) return false;
  if (value.kind === "temporary_error") {
    return [
      "dns_lookup_inconclusive",
      "dns_response_too_large",
      "dns_resolver_unavailable",
    ].includes(value.code) && Object.keys(value).length === 2;
  }
  return value.kind === "resolved" &&
    typeof value.matched === "boolean" &&
    typeof value.authoritative_absence === "boolean" &&
    !(value.authoritative_absence && value.matched) &&
    typeof value.dnssec_authenticated === "boolean" &&
    (value.minimum_ttl_seconds === null ||
      (Number.isSafeInteger(value.minimum_ttl_seconds) &&
        value.minimum_ttl_seconds >= 0)) &&
    /^[0-9a-f]{64}$/.test(value.rrset_sha256 ?? "") &&
    Object.keys(value).length === 6;
}

function validVerificationWork(value, requestID = null) {
  if (!isObject(value) || value.schema_version !== VERIFICATION_WORK_SCHEMA ||
      !REQUEST_ID_PATTERN.test(value.request_id ?? "") ||
      (requestID !== null && value.request_id !== requestID) ||
      !["manual", "scheduled"].includes(value.mode) ||
      !VERIFICATION_CLAIM_PATTERN.test(value.claim_id ?? "") ||
      !Number.isSafeInteger(value.generation) || value.generation < 1 ||
      !Number.isSafeInteger(value.request_state_revision) ||
      value.request_state_revision < 1 ||
      typeof value.request_updated_at !== "string" ||
      !Number.isFinite(Date.parse(value.request_updated_at)) ||
      typeof value.verification_due_key !== "string" ||
      !value.verification_due_key.startsWith("verification-due:") ||
      (value.verification_refresh_generation !== undefined &&
        (!Number.isSafeInteger(value.verification_refresh_generation) ||
          value.verification_refresh_generation < 0)) ||
      !isObject(value.ownership_challenge) ||
      value.ownership_challenge.record_type !== "TXT" ||
      typeof value.ownership_challenge.record_name !== "string" ||
      typeof value.ownership_challenge.record_value !== "string" ||
      typeof value.ownership_challenge.expires_at !== "string" ||
      !Number.isFinite(Date.parse(value.ownership_challenge.expires_at)) ||
      !["claimed", "observed"].includes(value.phase) ||
      typeof value.claimed_at !== "string" ||
      !Number.isFinite(Date.parse(value.claimed_at)) ||
      typeof value.lease_expires_at !== "string" ||
      !Number.isFinite(Date.parse(value.lease_expires_at)) ||
      (value.phase === "claimed" && value.observation !== null) ||
      (value.phase === "observed" &&
        (!validVerificationObservation(value.observation) ||
          typeof value.observed_at !== "string" ||
          !Number.isFinite(Date.parse(value.observed_at))))) {
    return false;
  }
  if (value.mode === "manual") {
    return isObject(value.actor) && value.actor.kind === "platform_admin" &&
      ACTOR_ID_PATTERN.test(value.actor.id ?? "") &&
      IDEMPOTENCY_KEY_PATTERN.test(value.idempotency_key ?? "") &&
      value.idempotency_fingerprint ===
        fingerprint(["request.verify", value.request_id]);
  }
  return value.actor?.kind === "system" &&
    value.actor?.id === "ownership-verifier" &&
    value.idempotency_key === null &&
    value.idempotency_fingerprint === null;
}

function publicVerificationClaim(work) {
  return {
    request_id: work.request_id,
    claim_id: work.claim_id,
    generation: work.generation,
    phase: work.phase,
    record_name: work.ownership_challenge.record_name,
    record_value: work.ownership_challenge.record_value,
    lease_expires_at: work.lease_expires_at,
    ...(work.phase === "observed"
      ? { observation: structuredClone(work.observation) }
      : {}),
  };
}

function resolverErrorMessage(code) {
  if (code === "dns_response_too_large") {
    return "DNS ownership resolver response is too large";
  }
  if (code === "dns_lookup_inconclusive") {
    return "DNS ownership lookup did not complete authoritatively";
  }
  return "DNS ownership resolver is temporarily unavailable";
}

function requestStorageKey(requestID) {
  return `request:${requestID}`;
}

function verificationWorkKey(requestID) {
  return `verification-work:${requestID}`;
}

function verificationRefreshKey(requestID) {
  return `verification-refresh:${requestID}`;
}

function routeProjectionIntentKey(requestID, realmLabel) {
  return `route-projection-intent:${requestID}:${realmLabel}`;
}

function routeBindingKey(requestID, claimID) {
  return `route-binding:${requestID}:${claimID}`;
}

function routeBindingPrefix(requestID) {
  return `route-binding:${requestID}:`;
}

function routeBindingAliasPrefix(accountID, realmID, claimID) {
  return `route-binding-alias:${accountID}:${realmID}:${claimID}:`;
}

function routeBindingAliasKey(binding) {
  return `${routeBindingAliasPrefix(
    binding.account_id,
    binding.realm_id,
    binding.realm_alias_claim_id,
  )}${binding.domain_request_id}`;
}

function routeProjectionDueKey(intent) {
  if (!Number.isSafeInteger(intent?.retry_at_ms) || intent.retry_at_ms < 0 ||
      !REQUEST_ID_PATTERN.test(intent?.domain_request_id ?? "") ||
      typeof intent?.realm_label !== "string") return null;
  return `route-projection-due:${String(intent.retry_at_ms).padStart(16, "0")}:` +
    `${intent.domain_request_id}:${intent.realm_label}`;
}

function routeSourceIntentKey(requestID) {
  return `route-source-intent:${requestID}`;
}

function routeSourceAccountPrefix(accountID) {
  return `route-source-account:${accountID}:`;
}

function routeSourceAccountKey(intent) {
  return `${routeSourceAccountPrefix(intent.account_id)}` +
    intent.domain_request_id;
}

function routeSourceDueKey(intent) {
  if (!Number.isSafeInteger(intent?.retry_at_ms) || intent.retry_at_ms < 0 ||
      !REQUEST_ID_PATTERN.test(intent?.domain_request_id ?? "")) return null;
  return `route-source-due:${String(intent.retry_at_ms).padStart(16, "0")}:` +
    intent.domain_request_id;
}

function routeAliasTaskKey(accountID, realmLabel) {
  return `route-alias-task:${accountID}:${realmLabel}`;
}

function routeAliasDueKey(task) {
  if (!Number.isSafeInteger(task?.retry_at_ms) || task.retry_at_ms < 0 ||
      !ACCOUNT_ID_PATTERN.test(task?.account_id ?? "") ||
      typeof task?.realm_label !== "string") return null;
  return `route-alias-due:${String(task.retry_at_ms).padStart(16, "0")}:` +
    `${task.account_id}:${task.realm_label}`;
}

function domainStorageKey(domain) {
  return `domain:${domain}`;
}

function isLegacyDomainMirror(value) {
  return value?.schema_version === SCHEMA_VERSION &&
    REQUEST_ID_PATTERN.test(value?.id ?? "");
}

function assertLegacyDomainMirror(value, request) {
  if (!isLegacyDomainMirror(value)) return false;
  if (canonicalJSONString(value) !== canonicalJSONString(request)) {
    fail("legacy custom domain authority mirror is inconsistent", 503);
  }
  return true;
}

function domainPendingKey(request) {
  if (!request?.domain || !REQUEST_ID_PATTERN.test(request?.id ?? "") ||
      typeof request?.requested_at !== "string") return null;
  return `domain-pending:${request.domain}:${request.requested_at}:${request.id}`;
}

function accountRequestPrefix(accountID) {
  return `account-request:${accountID}:`;
}

function accountRequestKey(accountID, requestID) {
  return `${accountRequestPrefix(accountID)}${requestID}`;
}

function accountDomainPrefix(accountID) {
  return `account-domain:${accountID}:`;
}

function accountDomainKey(request) {
  if (!ACCOUNT_ID_PATTERN.test(request?.account_id ?? "") ||
      !REQUEST_ID_PATTERN.test(request?.id ?? "") ||
      typeof request?.requested_at !== "string" ||
      !Number.isFinite(Date.parse(request.requested_at))) {
    return null;
  }
  return `${accountDomainPrefix(request.account_id)}` +
    `${request.requested_at}:${request.id}`;
}

function usageKey(accountID) {
  return `account-usage:${accountID}`;
}

function planFenceKey(accountID) {
  return `plan-fence:${accountID}`;
}

function planIntentKey(accountID) {
  return `plan-intent:${accountID}`;
}

function lifecycleFenceKey(accountID) {
  return `lifecycle-fence:${accountID}`;
}

function lifecycleIntentKey(accountID) {
  return `lifecycle-intent:${accountID}`;
}

function dueKey(prefix, retryAtMS, accountID) {
  if (!Number.isSafeInteger(retryAtMS) || retryAtMS < 0 ||
      !ACCOUNT_ID_PATTERN.test(accountID ?? "")) {
    return null;
  }
  return `${prefix}:${String(retryAtMS).padStart(16, "0")}:${accountID}`;
}

function planDueKey(intent) {
  return dueKey("plan-due", intent?.retry_at_ms, intent?.account_id);
}

function lifecycleDueKey(intent) {
  return dueKey("lifecycle-due", intent?.retry_at_ms, intent?.account_id);
}

function verificationDueKey(request) {
  const value = request?.ownership_verification?.next_check_at ??
    (request?.state === "pending_verification" ? request?.requested_at : null);
  const timestamp = typeof value === "string" ? Date.parse(value) : NaN;
  if (!Number.isFinite(timestamp) || !REQUEST_ID_PATTERN.test(request?.id ?? "")) {
    return null;
  }
  return `verification-due:${String(timestamp).padStart(16, "0")}:${request.id}`;
}

function storedVerificationRefreshDue(value, requestID) {
  if (!isObject(value) || value.schema_version !==
        AGENT_EMAIL_DOMAIN_VERIFICATION_REFRESH_SCHEMA_VERSION ||
      value.request_id !== requestID ||
      typeof value.verification_due_key !== "string" ||
      !/^verification-due:\d{16}:aedr_[a-z2-7]{16}$/.test(
        value.verification_due_key,
      ) || !value.verification_due_key.endsWith(`:${requestID}`)) {
    return null;
  }
  return value.verification_due_key;
}

function effectiveVerificationRefresh(request, value) {
  return isAgentEmailDomainVerificationRefresh(request, value) ? value : null;
}

function effectiveVerificationDueKey(request, refresh = null) {
  return effectiveVerificationRefresh(request, refresh)?.verification_due_key ??
    verificationDueKey(request);
}

function graceDueKey(request) {
  const timestamp = typeof request?.plan_grace_until === "string"
    ? Date.parse(request.plan_grace_until)
    : NaN;
  if (!Number.isFinite(timestamp) || !REQUEST_ID_PATTERN.test(request?.id ?? "")) {
    return null;
  }
  return `plan-grace-due:${String(timestamp).padStart(16, "0")}:${request.id}`;
}

function challengeExpiresAt(request) {
  const explicit = request?.ownership_challenge?.expires_at;
  if (typeof explicit === "string" && Number.isFinite(Date.parse(explicit))) {
    return explicit;
  }
  const requestedAt = Date.parse(request?.requested_at ?? "");
  return Number.isFinite(requestedAt)
    ? new Date(requestedAt + PENDING_CHALLENGE_TTL_MS).toISOString()
    : null;
}

function challengeExpiryDueKey(request) {
  const timestamp = Date.parse(challengeExpiresAt(request) ?? "");
  if (!Number.isFinite(timestamp) || !REQUEST_ID_PATTERN.test(request?.id ?? "")) {
    return null;
  }
  return `challenge-expiry-due:${String(timestamp).padStart(16, "0")}:` +
    request.id;
}

function idempotencyStorageKey(scope, key) {
  return `idem:${scope}:${key}`;
}

function encodeListCursor(key) {
  const bytes = new TextEncoder().encode(key);
  const binary = String.fromCharCode(...bytes);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_")
    .replace(/=+$/, "");
}

function decodeListCursor(value, prefix) {
  if (value == null || value === "") return null;
  if (typeof value !== "string" || value.length > 1_024 ||
      !/^[A-Za-z0-9_-]+$/.test(value)) {
    fail("invalid list cursor", 400);
  }
  try {
    const base64 = value.replaceAll("-", "+").replaceAll("_", "/") +
      "=".repeat((4 - value.length % 4) % 4);
    const binary = atob(base64);
    const key = new TextDecoder("utf-8", { fatal: true }).decode(
      Uint8Array.from(binary, (character) => character.charCodeAt(0)),
    );
    if (!key.startsWith(prefix)) fail("invalid list cursor", 400);
    return key;
  } catch (error) {
    if (error instanceof DomainRegistryError) throw error;
    fail("invalid list cursor", 400);
  }
}

function publicRequest(request, refresh = null) {
  const effectiveRefresh = effectiveVerificationRefresh(request, refresh);
  return {
    id: request.id,
    account_id: request.account_id,
    domain: request.domain,
    state: request.state,
    ownership_challenge: {
      ...request.ownership_challenge,
      ...(challengeExpiresAt(request)
        ? { expires_at: challengeExpiresAt(request) }
        : {}),
    },
    requested_by: request.requested_by,
    requested_at: request.requested_at,
    updated_at: request.updated_at,
    domain_limit_at_request: request.domain_limit_at_request,
    plan_revision: request.plan_revision,
    plan_snapshot_hash: request.plan_snapshot_hash,
    state_revision: request.state_revision ?? 1,
    availability: requestAvailability(request),
    plan_suspended: request.plan_suspended === true,
    lifecycle_suspended: request.lifecycle_suspended === true,
    ...(request.plan_grace_until
      ? { plan_grace_until: request.plan_grace_until }
      : {}),
    ...(request.ownership_verification
      ? {
        ownership_verification: {
          ...(effectiveRefresh?.ownership_verification ??
            request.ownership_verification),
        },
      }
      : {}),
    ...(request.expiration ? { expiration: { ...request.expiration } } : {}),
    ...(request.decision ? { decision: { ...request.decision } } : {}),
    ...(request.retirement ? { retirement: { ...request.retirement } } : {}),
  };
}

function requestAvailability(request) {
  if (request?.state === "retired") return "retired";
  if (request?.state === "rejected") return "rejected";
  if (request?.state === "expired") return "expired";
  if (request?.lifecycle_suspended === true) return "suspended_lifecycle";
  if (request?.plan_suspended === true) return "suspended_plan";
  if (request?.ownership_verification?.state === "stale") {
    return "suspended_verification";
  }
  if (request?.ownership_verification?.state === "conflict") {
    return "unavailable_domain";
  }
  if (request?.plan_grace_until) return "active_grace";
  return request?.state === "verified" ? "verified" : "pending_verification";
}

function publicAudit(event) {
  return {
    sequence: event.sequence,
    registry_revision: event.registry_revision,
    occurred_at: event.occurred_at,
    actor_kind: event.actor_kind,
    actor_id: event.actor_id,
    action: event.action,
    target: event.target,
    metadata: { ...event.metadata },
  };
}

export function agentEmailDomainRegistryStub(env = {}) {
  const namespace = env.AGENT_EMAIL_DOMAINS;
  if (!namespace) return null;
  return namespace.get(namespace.idFromName(DEFAULT_REGISTRY_OBJECT_NAME));
}

// readAgentEmailDomainPlanFit returns only the commercial capacity count. It
// includes pending reservations as well as verified allocations so a domain
// that has not produced a cell route cannot silently pass a downgrade check.
export async function readAgentEmailDomainPlanFit(
  env,
  accountID,
  maximum,
) {
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) throw new Error("custom domain authority is not configured");
  const response = await stub.fetch(
    "https://agent-email-domain.internal/plan/fit",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ account_id: accountID, maximum }),
    },
  );
  const body = await response.json().catch(() => null);
  if (!response.ok || body?.schema_version !== SCHEMA_VERSION ||
      body?.account_id !== accountID || body?.maximum !== maximum ||
      !Number.isSafeInteger(body?.used) || body.used < 0) {
    throw new Error(body?.error ?? "custom domain plan-fit authority is unavailable");
  }
  return body;
}

export function agentEmailCustomDomainVerificationEnabled(env = {}) {
  return String(
    env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED ?? "",
  ) === "true";
}

export function agentEmailCustomDomainRoutingEnabled(env = {}) {
  return String(
    env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED ?? "",
  ) === "true";
}

export function agentEmailCustomDomainRoutingEnabledForAccount(
  env = {},
  accountID,
) {
  if (!agentEmailCustomDomainRoutingEnabled(env) ||
      !ACCOUNT_ID_PATTERN.test(accountID ?? "")) return false;
  const raw = String(
    env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST ?? "",
  );
  if (!raw || raw !== raw.trim()) return false;
  const accounts = raw.split(",");
  if (accounts.some((value) => !ACCOUNT_ID_PATTERN.test(value))) return false;
  return new Set(accounts).has(accountID);
}

export function agentEmailCustomDomainRequestsEnabledForAccount(
  env = {},
  accountID,
) {
  if (String(env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED ?? "") !==
      "true" || !ACCOUNT_ID_PATTERN.test(accountID ?? "")) return false;
  const raw = String(
    env.CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST ?? "",
  );
  if (!raw || raw !== raw.trim()) return false;
  const accounts = raw.split(",");
  if (accounts.some((value) => !ACCOUNT_ID_PATTERN.test(value))) return false;
  return new Set(accounts).has(accountID);
}

export async function reconcileAgentEmailDomainsForPlan(
  env,
  accountID,
  snapshot,
  mode,
  options = {},
) {
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) return { skipped: true, complete: true };
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !validPlanRevisionFence(snapshot?.revision, snapshot?.snapshot_hash) ||
      !Array.isArray(snapshot?.features) || !isObject(snapshot?.limits) ||
      !["restrict_only", "complete"].includes(mode)) {
    throw new Error("invalid account plan snapshot for domain reconciliation");
  }
  const entitlement = agentEmailCustomDomainEntitlement(snapshot);
  const response = await stub.fetch(
    "https://agent-email-domain.internal/plan/reconcile",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        account_id: accountID,
        activation_enabled:
          agentEmailCustomDomainRequestsEnabledForAccount(env, accountID),
        feature_enabled: entitlement.enabled,
        domain_limit: entitlement.limit,
        mode,
        plan_revision: snapshot.revision,
        plan_snapshot_hash: snapshot.snapshot_hash,
        ...(options.recover_pending_revision === undefined
          ? {}
          : {
            recover_pending_revision: options.recover_pending_revision,
            recover_pending_snapshot_hash:
              options.recover_pending_snapshot_hash,
          }),
      }),
    },
  );
  const body = await response.json().catch(() => null);
  if (!response.ok) {
    throw new Error(
      body?.error ??
        "custom domain plan reconciliation failed",
    );
  }
  return body;
}

export async function reconcileAgentEmailDomainsForAccountLifecycle(
  env,
  accountID,
  { operation_id: operationID, epoch, action },
) {
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) return { skipped: true, complete: true };
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !IDEMPOTENCY_KEY_PATTERN.test(operationID ?? "") ||
      !Number.isSafeInteger(epoch) || epoch < 0 ||
      !["suspend", "republish", "retire"].includes(action)) {
    throw new Error("invalid account lifecycle fence for domain reconciliation");
  }
  const response = await stub.fetch(
    "https://agent-email-domain.internal/account-lifecycle/reconcile",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        account_id: accountID,
        activation_enabled:
          agentEmailCustomDomainRequestsEnabledForAccount(env, accountID),
        operation_id: operationID,
        epoch,
        action,
      }),
    },
  );
  const body = await response.json().catch(() => null);
  if (!response.ok || body?.complete !== true) {
    throw new Error(
      body?.error ??
        (body?.complete === false
          ? "custom domain account lifecycle is still converging"
          : "custom domain account lifecycle reconciliation failed"),
    );
  }
  return body;
}

async function callVerificationRegistry(stub, path, body) {
  try {
    const response = await stub.fetch(
      `https://agent-email-domain.internal${path}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      },
    );
    return {
      status: response.status,
      ok: response.ok,
      body: await response.json().catch(() => null),
    };
  } catch {
    return { status: 503, ok: false, body: null };
  }
}

function verificationRegistryResponse(result) {
  if (result?.body) return json(result.body, result.status);
  return errorResponse("agent email domain registry is unavailable", 503);
}

async function resolveClaimOutsideAuthority(
  stub,
  claim,
  resolveTXT = resolveAgentEmailDomainTXT,
) {
  let observation = claim.observation;
  if (claim.phase !== "observed") {
    try {
      const result = await resolveTXT(claim.record_name);
      observation = agentEmailDomainResolvedObservation(
        result,
        claim.record_value,
      );
    } catch (error) {
      try {
        observation = agentEmailDomainTemporaryObservation(error);
      } catch {
        return {
          status: 503,
          ok: false,
          body: {
            schema_version: SCHEMA_VERSION,
            error: "custom domain ownership resolver failed",
          },
        };
      }
    }
    const observed = await callVerificationRegistry(
      stub,
      "/verification/observe",
      {
        request_id: claim.request_id,
        claim_id: claim.claim_id,
        generation: claim.generation,
        observation,
        verification_enabled: true,
      },
    );
    if (!observed.ok) return observed;
  }
  return callVerificationRegistry(stub, "/verification/commit", {
    request_id: claim.request_id,
    claim_id: claim.claim_id,
    generation: claim.generation,
    verification_enabled: true,
  });
}

/**
 * Manual verification is orchestrated by the stateless Worker. The singleton
 * authority owns only the short claim, observation, and journaled commit
 * sections; the external DNS wait happens between Durable Object calls.
 */
export async function runAgentEmailDomainManualVerification(
  env,
  input,
  dependencies = {},
) {
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) {
    return errorResponse("agent email domain registry is unavailable", 503);
  }
  const claimed = await callVerificationRegistry(stub, "/verification/claim", {
    mode: "manual",
    actor: input?.actor,
    request_id: input?.request_id,
    idempotency_key: input?.idempotency_key,
    verification_enabled: agentEmailCustomDomainVerificationEnabled(env),
  });
  if (!claimed.ok) return verificationRegistryResponse(claimed);
  if (claimed.body?.kind === "result") {
    return json(claimed.body.body, claimed.body.status);
  }
  if (claimed.body?.kind !== "claim") {
    return errorResponse("custom domain verification claim is invalid", 503);
  }
  const committed = await resolveClaimOutsideAuthority(
    stub,
    claimed.body.claim,
    dependencies.resolveTXT,
  );
  if (!committed.ok) return verificationRegistryResponse(committed);
  if (committed.body?.kind !== "result") {
    return errorResponse("custom domain verification commit is invalid", 503);
  }
  return json(committed.body.body, committed.body.status);
}

export async function runScheduledAgentEmailDomainVerification(
  env,
  dependencies = {},
) {
  if (!agentEmailCustomDomainVerificationEnabled(env)) {
    return { ran: false, configured: true };
  }
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) {
    console.log("agent-email-domain: verification registry is unavailable");
    return { ran: false, configured: false };
  }
  let next = 0;
  let exhausted = false;
  const totals = { claimed: 0, checked: 0, matched: 0, failures: 0 };
  const lane = async () => {
    for (;;) {
      const ordinal = next++;
      if (ordinal >= VERIFICATION_RECONCILE_LIMIT || exhausted) return;
      const claimed = await callVerificationRegistry(
        stub,
        "/verification/claim",
        { mode: "scheduled", verification_enabled: true },
      );
      if (!claimed.ok) {
        totals.failures += 1;
        return;
      }
      if (claimed.body?.kind === "empty") {
        exhausted = true;
        return;
      }
      if (claimed.body?.kind === "processed") {
        totals.checked += claimed.body.checked === true ? 1 : 0;
        totals.matched += claimed.body.matched === true ? 1 : 0;
        continue;
      }
      if (claimed.body?.kind !== "claim") {
        totals.failures += 1;
        return;
      }
      totals.claimed += 1;
      const committed = await resolveClaimOutsideAuthority(
        stub,
        claimed.body.claim,
        dependencies.resolveTXT,
      );
      if (!committed.ok || committed.body?.kind !== "result") {
        totals.failures += 1;
        continue;
      }
      totals.checked += 1;
      totals.matched += committed.body.matched === true ? 1 : 0;
    }
  };
  await Promise.all(Array.from(
    { length: VERIFICATION_WORKER_CONCURRENCY },
    () => lane(),
  ));
  if (totals.failures > 0) {
    console.log("agent-email-domain: scheduled verification had failures");
  }
  return {
    ran: true,
    succeeded: totals.failures === 0,
    ...totals,
  };
}

/**
 * One globally named Durable Object owns pending requests and permanently
 * allocated customer domains. Ownership and verification remain the only
 * canonical authority here. The optional dark routing lane derives a fenced,
 * rebuildable cell/KV projection from that authority plus one exact alias
 * claim proof; it never turns projection state into ownership authority.
 */
export class DurableAgentEmailDomainRegistry {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.now = dependencies.now ?? (() => new Date());
    this.newRequestID = dependencies.newRequestID ?? newRequestID;
    this.newChallengeToken =
      dependencies.newChallengeToken ?? newChallengeToken;
    this.newVerificationClaimID =
      dependencies.newVerificationClaimID ?? newVerificationClaimID;
    this.fetchImpl = dependencies.fetch ??
      ((...args) => globalThis.fetch(...args));
    this.signRouteProjection = dependencies.signRouteProjection ??
      ((projection) => signAgentEmailRouteProjection(projection, this.env));
    this.assertRequestActivationReady =
      dependencies.assertRequestActivationReady ??
      (() => this.defaultAssertRequestActivationReady());
    this.authorityJournal = new AgentEmailDomainJournalRuntime(
      this.storage,
      this.env,
      {
        now: this.now,
        ...(dependencies.newJournalStreamID
          ? { newStreamID: dependencies.newJournalStreamID }
          : {}),
        ...(dependencies.afterJournalAppend
          ? { afterJournalAppend: dependencies.afterJournalAppend }
          : {}),
        ...(dependencies.newRecoveryActionFence
          ? { newRecoveryActionFence: dependencies.newRecoveryActionFence }
          : {}),
        ...(dependencies.afterRecoveryAction
          ? { afterRecoveryAction: dependencies.afterRecoveryAction }
          : {}),
      },
    );
    this.queue = Promise.resolve();
  }

  fetch(request) {
    return this.serial(() => this.handleFetch(request));
  }

  alarm() {
    return this.serial(async () => {
      const rawApply = this.atomicRaw.bind(this);
      await this.authorityJournal.resume(rawApply);
      await this.authorityJournal.assertOperationalReady();
      await this.reconcileDueLifecycles();
      await this.reconcileDuePlanIntents();
      await this.reconcileDueGrace();
      await this.reconcileDueChallengeExpiries();
      await this.reconcileDueRouteSources();
      await this.reconcileDueRouteAliases();
      await this.reconcileDueRouteProjections();
      await this.scheduleNextAlarm();
    });
  }

  serial(work) {
    const result = this.queue.then(work, work);
    this.queue = result.catch(() => {});
    return result;
  }

  async handleFetch(request) {
    if (request.method !== "POST") {
      return errorResponse("domain registry endpoint not found", 404);
    }
    const path = new URL(request.url).pathname;
    const recoveryControlPath = path.startsWith("/journal/") ||
      path.startsWith("/recovery/");
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse(
        "invalid JSON body",
        400,
        "",
        {},
        recoveryControlPath ? RECOVERY_SCHEMA_VERSION : SCHEMA_VERSION,
      );
    }
    try {
      const rawApply = this.atomicRaw.bind(this);
      // Recovery targets must still be literally empty when recovery starts.
      // Journal and recovery control routes therefore run before ensureMeta().
      if (path === "/journal/status") {
        return json({
          schema_version: RECOVERY_SCHEMA_VERSION,
          ...await this.authorityJournal.status(),
        });
      }
      if (path === "/journal/bootstrap" || path === "/journal/checkpoint") {
        const result = path === "/journal/bootstrap"
          ? await this.authorityJournal.bootstrap(input, rawApply)
          : await this.authorityJournal.checkpoint(input, rawApply);
        return json({ schema_version: RECOVERY_SCHEMA_VERSION, ...result });
      }
      if (path === "/recovery/start") {
        const result = await this.authorityJournal.startRecovery(input, rawApply);
        return json({ schema_version: RECOVERY_SCHEMA_VERSION, ...result }, 202);
      }
      if (path === "/recovery/status") {
        const result = await this.authorityJournal.recoveryStatus(
          input?.recovery_id,
        );
        return result
          ? json({ schema_version: RECOVERY_SCHEMA_VERSION, ...result })
          : errorResponse(
            "custom inbound domain recovery not found",
            404,
            "",
            {},
            RECOVERY_SCHEMA_VERSION,
          );
      }
      if (path === "/recovery/advance" || path === "/recovery/verify") {
        const result = path === "/recovery/advance"
          ? await this.authorityJournal.advanceRecovery(input, rawApply)
          : await this.authorityJournal.verifyRecovery(input, rawApply);
        return json(
          { schema_version: RECOVERY_SCHEMA_VERSION, ...result },
          result.sealed || result.failed ? 200 : 202,
        );
      }

      await this.authorityJournal.resume(rawApply);
      await this.authorityJournal.assertOperationalReady();
      switch (path) {
        case "/request/create":
          return await this.createRequest(input);
        case "/request/list":
          return await this.listRequests(input, false);
        case "/request/admin-list":
          return await this.listRequests(input, true);
        case "/request/get":
          return await this.getRequest(input);
        case "/request/reject":
          return await this.rejectRequest(input);
        case "/request/retire":
          return await this.retireRequest(input);
        case "/verification/claim":
          return json(await this.claimVerification(input));
        case "/verification/observe":
          return json(await this.observeVerification(input));
        case "/verification/commit":
          return json(await this.commitVerification(input));
        case "/audit/list":
          return await this.listAudit(input);
        case "/plan/reconcile":
          return await this.reconcilePlan(input);
        case "/plan/fit":
          return await this.planFit(input);
        case "/account-lifecycle/reconcile":
          return await this.reconcileAccountLifecycle(input);
        case "/route/get":
          return await this.getCustomDomainRoute(input);
        case "/route/alias-convergence/enqueue":
          return await this.enqueueCustomDomainAliasConvergence(input);
        case "/route/alias-convergence/status":
          return await this.customDomainAliasConvergenceStatus(input);
        default:
          return errorResponse("domain registry endpoint not found", 404);
      }
    } catch (error) {
      const journalError = error instanceof AgentEmailDomainJournalRuntimeError;
      const journalConflictCodes = new Set([
        "agent_email_domain_journal_already_bootstrapped",
        "agent_email_domain_journal_fence_mismatch",
        "agent_email_domain_journal_fork_detected",
        "agent_email_domain_journal_idempotency_conflict",
        "agent_email_domain_recovery_checkpoint_invalid",
        "agent_email_domain_recovery_collision",
        "agent_email_domain_recovery_digest_mismatch",
        "agent_email_domain_recovery_action_fence_mismatch",
        "agent_email_domain_recovery_action_not_allowed",
        "agent_email_domain_recovery_idempotency_conflict",
        "agent_email_domain_recovery_incomplete",
        "agent_email_domain_recovery_invariant_failed",
        "agent_email_domain_recovery_revision_regression",
        "agent_email_domain_recovery_target_not_empty",
        "agent_email_domain_recovery_target_sealed",
        "agent_email_domain_recovery_tombstone_resurrection",
        "agent_email_domain_recovery_upgrade_required",
      ]);
      const journalBadRequestCodes = new Set([
        "agent_email_domain_journal_maintenance_invalid",
        "agent_email_domain_recovery_request_invalid",
      ]);
      return errorResponse(
        error instanceof DomainRegistryError || journalError
          ? String(error.message)
          : "custom inbound domain registry failed",
        error instanceof DomainRegistryError
          ? error.status
          : journalBadRequestCodes.has(error?.code)
          ? 400
          : journalConflictCodes.has(error?.code)
          ? 409
          : journalError
          ? 503
          : 500,
        error instanceof DomainRegistryError || journalError ? error.code : "",
        error instanceof DomainRegistryError ? error.details : {},
        recoveryControlPath ? RECOVERY_SCHEMA_VERSION : SCHEMA_VERSION,
      );
    }
  }

  async ensureMeta() {
    const existing = await this.storage.get(META_KEY);
    if (existing) {
      if (existing.schema_version !== SCHEMA_VERSION ||
          !Number.isSafeInteger(existing.registry_revision) ||
          existing.registry_revision < 0 ||
          !Number.isSafeInteger(existing.audit_sequence) ||
          existing.audit_sequence < 0) {
        fail("custom inbound domain registry metadata is invalid", 503);
      }
      return existing;
    }
    const now = this.now().toISOString();
    const meta = {
      schema_version: SCHEMA_VERSION,
      registry_revision: 0,
      audit_sequence: 0,
      created_at: now,
      updated_at: now,
    };
    await this.atomic([[META_KEY, meta]]);
    return meta;
  }

  async defaultAssertRequestActivationReady() {
    const status = await this.authorityJournal.status();
    if (String(
      this.env?.CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY ?? "",
    ) !== "true" ||
        String(this.env?.CP_PLAN_LIFECYCLE_ENABLED ?? "") !== "true" ||
        status.required !== true || status.enabled !== true ||
        status.healthy !== true || status.pending === true ||
        status.forked === true || !Number.isSafeInteger(status.head?.sequence) ||
        status.head.sequence < 1) {
      fail(
        "custom inbound domain requests are not operationally ready",
        503,
        "custom_domain_activation_not_ready",
      );
    }
  }

  async atomic(entries, deletes = [], options = {}) {
    return this.authorityJournal.commit(
      entries,
      deletes,
      options,
      this.atomicRaw.bind(this),
    );
  }

  async atomicRaw(entries, deletes = []) {
    const apply = async (storage) => {
      for (const [key, value] of entries) await storage.put(key, value);
      for (const key of deletes) await storage.delete(key);
    };
    if (typeof this.storage.transaction === "function") {
      await this.storage.transaction(apply);
    } else {
      await apply(this.storage);
    }
  }

  async verificationRefresh(request) {
    const value = await this.storage.get(verificationRefreshKey(request.id));
    return effectiveVerificationRefresh(request, value);
  }

  async publicRequest(request) {
    return publicRequest(request, await this.verificationRefresh(request));
  }

  async repairVerificationSchedule(request, encounteredDue = null) {
    const refreshKey = verificationRefreshKey(request.id);
    const raw = await this.storage.get(refreshKey);
    const refreshDue = storedVerificationRefreshDue(raw, request.id);
    const canonicalDue = verificationDueKey(request);
    const active = ["pending_verification", "verified"].includes(request.state) &&
      request.plan_suspended !== true &&
      request.lifecycle_suspended !== true;
    const entries = active && canonicalDue
      ? [[canonicalDue, request.id]]
      : [];
    const deletes = new Set([
      refreshKey,
      verificationWorkKey(request.id),
      encounteredDue,
      refreshDue,
    ].filter(Boolean));
    if (canonicalDue) deletes.delete(canonicalDue);
    await this.atomicRaw(entries, [...deletes]);
    return canonicalDue;
  }

  async commitVerificationRefresh(current, verification, options = {}) {
    const key = verificationRefreshKey(current.id);
    const raw = await this.storage.get(key);
    const previous = effectiveVerificationRefresh(current, raw);
    const previousGeneration = previous?.generation ?? 0;
    const previousDue = effectiveVerificationDueKey(current, previous);
    if ((options.expected_generation !== undefined &&
          options.expected_generation !== previousGeneration) ||
        (options.previous_due !== undefined &&
          options.previous_due !== previousDue)) {
      fail("custom domain verification claim is stale", 409,
        "verification_claim_stale");
    }
    const refresh = {
      schema_version: AGENT_EMAIL_DOMAIN_VERIFICATION_REFRESH_SCHEMA_VERSION,
      request_id: current.id,
      generation: previousGeneration + 1,
      request_state_revision: current.state_revision ?? 1,
      request_updated_at: current.updated_at,
      verification_due_key: verificationDueKey({
        ...current,
        ownership_verification: verification,
      }),
      ownership_challenge: {
        ...current.ownership_challenge,
        expires_at: challengeExpiresAt(current),
      },
      ownership_verification: structuredClone(verification),
      updated_at: verification.last_checked_at,
    };
    if (!effectiveVerificationRefresh(current, refresh)) {
      fail("custom domain verification refresh is invalid", 503,
        "verification_refresh_invalid");
    }
    const deletes = new Set([
      previousDue,
      ...(Array.isArray(options.extra_deletes) ? options.extra_deletes : []),
    ].filter(Boolean));
    deletes.delete(refresh.verification_due_key);
    await this.atomicRaw([
      [key, refresh],
      [refresh.verification_due_key, current.id],
    ], [...deletes]);
    await this.scheduleNextAlarm().catch(() => {});
    return refresh;
  }

  async scheduleNextAlarm() {
    if (typeof this.storage.setAlarm !== "function") return;
    const prefixes = [
      "lifecycle-due:",
      "plan-due:",
      "plan-grace-due:",
      "challenge-expiry-due:",
      "route-source-due:",
      "route-alias-due:",
      "route-projection-due:",
    ];
    const deadlines = [];
    for (const prefix of prefixes) {
      const first = [...(await this.storage.list({ prefix, limit: 1 })).keys()][0];
      if (!first) continue;
      const timestamp = Number(first.split(":", 3)[1]);
      if (Number.isFinite(timestamp)) deadlines.push(timestamp);
    }
    if (deadlines.length > 0) {
      await this.storage.setAlarm(Math.min(...deadlines));
    } else if (typeof this.storage.deleteAlarm === "function") {
      await this.storage.deleteAlarm().catch(() => {});
    }
  }

  async mutation(actor, action, target, metadata, occurredAt = null) {
    const current = await this.ensureMeta();
    const now = occurredAt ?? this.now().toISOString();
    const meta = {
      ...current,
      registry_revision: current.registry_revision + 1,
      audit_sequence: current.audit_sequence + 1,
      updated_at: now,
    };
    const audit = {
      sequence: meta.audit_sequence,
      registry_revision: meta.registry_revision,
      occurred_at: now,
      actor_kind: actor.kind,
      actor_id: actor.id,
      action,
      target,
      metadata,
    };
    return {
      now,
      meta,
      audit,
      audit_key: `audit:${String(audit.sequence).padStart(12, "0")}`,
    };
  }

  async idempotentReplay(scope, key, expectedFingerprint) {
    const receipt = await this.storage.get(idempotencyStorageKey(scope, key));
    if (!receipt) return null;
    if (receipt.fingerprint !== expectedFingerprint) {
      fail("idempotency_key was already used for a different request", 409,
        "idempotency_conflict");
    }
    return json(receipt.body, receipt.status);
  }

  async accountUsage(accountID) {
    const usage = await this.storage.get(usageKey(accountID));
    if (!usage) {
      return {
        schema_version: 1,
        account_id: accountID,
        open_requests: 0,
        allocated_domains: 0,
        updated_at: null,
      };
    }
    if (usage.schema_version !== 1 || usage.account_id !== accountID ||
        !Number.isSafeInteger(usage.open_requests) ||
        usage.open_requests < 0 ||
        usage.open_requests >
          AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT ||
        !Number.isSafeInteger(usage.allocated_domains ?? usage.open_requests) ||
        (usage.allocated_domains ?? usage.open_requests) < usage.open_requests) {
      fail("custom inbound domain account usage is invalid", 503);
    }
    return {
      ...usage,
      allocated_domains: usage.allocated_domains ?? usage.open_requests,
    };
  }

  async planFit(input) {
    const accountID = validateAccountID(input?.account_id);
    const maximum = input?.maximum;
    if (!Number.isSafeInteger(maximum) || maximum < 0) {
      fail("custom inbound domain plan-fit maximum is invalid", 400);
    }
    if (await this.accountPolicyConverging(accountID)) {
      fail("custom inbound domain policy is still converging", 409);
    }
    const rawUsage = await this.storage.get(usageKey(accountID));
    if (!rawUsage && await this.accountHasActiveRequest(accountID)) {
      fail("custom inbound domain account usage is missing", 503);
    }
    const usage = await this.accountUsage(accountID);
    return json({
      schema_version: SCHEMA_VERSION,
      account_id: accountID,
      maximum,
      used: usage.allocated_domains,
    });
  }

  async mintUniqueRequestID() {
    for (let attempt = 0; attempt < MAX_REQUEST_ID_MINT_ATTEMPTS; attempt++) {
      const id = this.newRequestID();
      if (!REQUEST_ID_PATTERN.test(id)) {
        fail("could not mint custom domain request id", 500);
      }
      if (!await this.storage.get(requestStorageKey(id))) return id;
    }
    fail("could not mint a unique custom domain request id", 503,
      "request_id_unavailable");
  }

  async assertRequestPolicyFence(accountID, input, planFence) {
    const [committed, planIntent, lifecycleIntent, lifecycleFence] =
      await Promise.all([
        this.storage.get(planFenceKey(accountID)),
        this.storage.get(planIntentKey(accountID)),
        this.storage.get(lifecycleIntentKey(accountID)),
        this.storage.get(lifecycleFenceKey(accountID)),
      ]);
    if (planIntent || lifecycleIntent) {
      fail("account policy is still converging", 409,
        "account_policy_converging");
    }
    if (committed &&
        (committed.committed_revision !== planFence.revision ||
          committed.committed_snapshot_hash !== planFence.snapshot_hash ||
          committed.feature_enabled !== input.feature_enabled ||
          committed.domain_limit !== input.domain_limit)) {
      fail("custom inbound domain plan fence is stale", 409,
        "stale_plan_fence");
    }
    if (lifecycleFence && lifecycleFence.action !== "republish") {
      fail("account lifecycle does not allow custom domain requests", 409,
        "account_lifecycle_suspended");
    }
    return committed ? null : {
      account_id: accountID,
      committed_revision: planFence.revision,
      committed_snapshot_hash: planFence.snapshot_hash,
      feature_enabled: input.feature_enabled,
      domain_limit: input.domain_limit,
      updated_at: this.now().toISOString(),
    };
  }

  async createRequest(input) {
    const actor = validateActor(input?.actor, "account_operator");
    const accountID = validateAccountID(input?.account_id);
    const key = validateIdempotencyKey(input?.idempotency_key);
    const domain = normalizeAgentEmailCustomDomain(input?.domain);
    const planFence = validatePlanFence(input);
    const fp = fingerprint(["request.create", accountID, domain]);
    const idempotencyScope = `request-create:${accountID}`;
    const replay = await this.idempotentReplay(idempotencyScope, key, fp);
    if (replay) return replay;

    // Re-check the gate inside the globally serialized authority. The Worker
    // hint can become stale while a request is in flight during a gate or
    // allowlist removal.
    if (input?.requests_enabled !== true ||
        !agentEmailCustomDomainRequestsEnabledForAccount(this.env, accountID)) {
      fail("custom inbound domain requests are not enabled", 409,
        "custom_domain_requests_disabled");
    }
    await this.assertRequestActivationReady();
    if (typeof input?.feature_enabled !== "boolean") {
      fail("feature_enabled must be provided", 400);
    }
    if (!Object.hasOwn(input, "domain_limit") ||
        !validDomainLimit(input.domain_limit)) {
      fail("domain_limit must be null or a non-negative integer", 400);
    }
    if (input.feature_enabled !== true || input.domain_limit === 0) {
      fail("custom inbound domains are not enabled for this account", 403,
        "feature_not_enabled");
    }
    const seedPlanFence = await this.assertRequestPolicyFence(
      accountID,
      input,
      planFence,
    );
    if (isProtectedAgentEmailDomain(domain, this.env)) {
      fail("domain is protected by Witself policy", 409,
        "protected_domain");
    }
    if (await this.storage.get(domainStorageKey(domain))) {
      fail("domain is already claimed or tombstoned", 409,
        "domain_unavailable");
    }

    const usage = await this.accountUsage(accountID);
    const technicalLimit = agentEmailCustomDomainOpenRequestLimit(this.env);
    if (usage.open_requests >= technicalLimit) {
      fail("custom inbound domain open-request ceiling reached", 409,
        "technical_open_request_limit_reached", {
          limit: technicalLimit,
        });
    }
    // Pending verification requests reserve commercial account quota and the
    // independent technical ceiling, but do not claim the domain globally.
    if (input.domain_limit !== null &&
        usage.allocated_domains >= input.domain_limit) {
      fail("custom inbound domain account limit reached", 403,
        "account_limit_reached", { limit: input.domain_limit });
    }

    const id = await this.mintUniqueRequestID();
    const challengeToken = this.newChallengeToken();
    if (!CHALLENGE_TOKEN_PATTERN.test(challengeToken)) {
      fail("could not mint custom domain ownership challenge", 500);
    }
    const mutation = await this.mutation(
      actor,
      "custom_domain.requested",
      domain,
      { account_id: accountID, request_id: id, state: "pending_verification" },
    );
    const created = {
      schema_version: SCHEMA_VERSION,
      id,
      account_id: accountID,
      domain,
      state: "pending_verification",
      state_revision: 1,
      ownership_challenge: {
        record_type: "TXT",
        record_name: `_witself-verification.${domain}`,
        record_value: `witself-domain-verification=${challengeToken}`,
        issued_at: mutation.now,
        expires_at: new Date(
          Date.parse(mutation.now) + PENDING_CHALLENGE_TTL_MS,
        ).toISOString(),
      },
      requested_by: actor.id,
      requested_at: mutation.now,
      updated_at: mutation.now,
      domain_limit_at_request: input.domain_limit,
      plan_revision: planFence.revision,
      plan_snapshot_hash: planFence.snapshot_hash,
      plan_suspended: false,
      plan_grace_until: null,
      lifecycle_suspended: false,
      lifecycle_fence: null,
      ownership_verification: null,
      expiration: null,
      decision: null,
      retirement: null,
    };
    const nextUsage = {
      ...usage,
      open_requests: usage.open_requests + 1,
      allocated_domains: usage.allocated_domains + 1,
      updated_at: mutation.now,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      request: publicRequest(created),
    };
    await this.atomic([
      [META_KEY, mutation.meta],
      [mutation.audit_key, mutation.audit],
      [requestStorageKey(id), created],
      [accountRequestKey(accountID, id), id],
      [accountDomainKey(created), id],
      [domainPendingKey(created), id],
      [challengeExpiryDueKey(created), id],
      [verificationDueKey(created), id],
      [usageKey(accountID), nextUsage],
      [idempotencyStorageKey(idempotencyScope, key), {
        fingerprint: fp,
        status: 202,
        body,
      }],
      ...(seedPlanFence ? [[planFenceKey(accountID), seedPlanFence]] : []),
    ]);
    await this.scheduleNextAlarm().catch(() => {});
    return json(body, 202);
  }

  async boundedEntries(prefix, input = {}, reverse = false) {
    const limit = validateListLimit(input.limit);
    const startAfter = decodeListCursor(input.cursor, prefix);
    const listed = await this.storage.list({
      prefix,
      limit: limit + 1,
      reverse,
      ...(startAfter
        ? reverse ? { end: startAfter } : { startAfter }
        : {}),
    });
    const entries = [...listed.entries()];
    const page = entries.slice(0, limit);
    return {
      entries: page,
      truncated: entries.length > limit,
      next_cursor: entries.length > limit && page.length > 0
        ? encodeListCursor(page.at(-1)[0])
        : null,
    };
  }

  async listRequests(input, admin) {
    let accountID = null;
    if (admin) {
      validateActor(input?.actor, "platform_admin");
      if (input?.account_id != null) {
        accountID = validateAccountID(input.account_id);
      }
    } else {
      validateActor(input?.actor, "account_operator");
      accountID = validateAccountID(input?.account_id);
    }
    const prefix = accountID ? accountRequestPrefix(accountID) : "request:";
    const listed = await this.boundedEntries(prefix, input);
    let requests = accountID
      ? await Promise.all(listed.entries.map(async ([, id]) => {
        const request = await this.storage.get(requestStorageKey(id));
        if (!request || request.account_id !== accountID) {
          fail("custom inbound domain request index is invalid", 503);
        }
        return this.publicRequest(request);
      }))
      : await Promise.all(listed.entries.map(([, request]) =>
        this.publicRequest(request)));
    if (input?.state != null) {
      if (!["pending_verification", "verified", "rejected", "expired", "retired"].includes(
        input.state,
      )) {
        fail("request state filter is invalid", 400);
      }
      requests = requests.filter((request) => request.state === input.state);
    }
    if (input?.domain != null) {
      const domain = normalizeAgentEmailCustomDomain(input.domain);
      requests = requests.filter((request) => request.domain === domain);
    }
    const usage = accountID ? await this.accountUsage(accountID) : null;
    return json({
      schema_version: SCHEMA_VERSION,
      requests,
      truncated: listed.truncated,
      next_cursor: listed.next_cursor,
      technical_open_request_limit:
        agentEmailCustomDomainOpenRequestLimit(this.env),
      ...(usage ? { open_requests: usage.open_requests } : {}),
      ...(usage ? { allocated_domains: usage.allocated_domains } : {}),
    });
  }

  async getRequest(input) {
    validateActor(input?.actor, "platform_admin");
    const id = validateRequestID(input?.request_id);
    const request = await this.storage.get(requestStorageKey(id));
    if (!request) fail("custom inbound domain request not found", 404);
    return json({
      schema_version: SCHEMA_VERSION,
      request: await this.publicRequest(request),
    });
  }

  async rejectRequest(input) {
    return this.transitionRequest(input, "rejected");
  }

  async retireRequest(input) {
    return this.transitionRequest(input, "retired");
  }

  async transitionRequest(input, nextState) {
    const actor = validateActor(input?.actor, "platform_admin");
    const id = validateRequestID(input?.request_id);
    const reason = validateReason(input?.reason);
    const key = validateIdempotencyKey(input?.idempotency_key);
    const fp = fingerprint([`request.${nextState}`, id, reason]);
    const idempotencyScope = `request-${nextState}:${id}`;
    const replay = await this.idempotentReplay(idempotencyScope, key, fp);
    if (replay) return replay;

    const current = await this.storage.get(requestStorageKey(id));
    if (!current) fail("custom inbound domain request not found", 404);
    const rawRefresh = await this.storage.get(verificationRefreshKey(id));
    const refresh = effectiveVerificationRefresh(current, rawRefresh);
    if (nextState === "rejected" && current.state !== "pending_verification") {
      fail("only a pending custom inbound domain request can be rejected", 409);
    }
    if (nextState === "retired" && current.state === "retired") {
      fail("custom inbound domain request is already retired", 409);
    }
    if (!["pending_verification", "verified", "rejected"].includes(current.state)) {
      fail("custom inbound domain request state is invalid", 503);
    }

    const mutation = await this.mutation(
      actor,
      `custom_domain.${nextState}`,
      current.domain,
      {
        account_id: current.account_id,
        request_id: current.id,
        from_state: current.state,
        reason,
      },
    );
    const updated = {
      ...current,
      state: nextState,
      state_revision: (current.state_revision ?? 1) + 1,
      updated_at: mutation.now,
      ...(nextState === "rejected"
        ? {
          decision: {
            action: "rejected",
            reason,
            decided_by: actor.id,
            decided_at: mutation.now,
          },
        }
        : {
          retirement: {
            reason,
            retired_by: actor.id,
            retired_at: mutation.now,
          },
        }),
      ...(nextState === "retired" ? { plan_grace_until: null } : {}),
    };
    const entries = [
      [META_KEY, mutation.meta],
      [mutation.audit_key, mutation.audit],
      [requestStorageKey(id), updated],
    ];
    const deletes = [verificationWorkKey(id), verificationRefreshKey(id)];
    const allocation = await this.storage.get(domainStorageKey(current.domain));
    let routeAllocation = null;
    if (assertLegacyDomainMirror(allocation, current)) {
      // v0.0.235 mirrored every request at domain:<domain>. Preserve that
      // already-durable non-reuse decision while the historical request moves.
      entries.push([domainStorageKey(current.domain), updated]);
    } else if (current.state === "verified") {
      if (!allocation || allocation.source_request_id !== current.id ||
          allocation.state !== "allocated") {
        fail("custom inbound domain allocation is invalid", 503);
      }
      routeAllocation = {
        ...allocation,
        state: "retired",
        allocation_revision: (allocation.allocation_revision ?? 1) + 1,
        updated_at: mutation.now,
        retirement: {
          reason,
          retired_by: actor.id,
          retired_at: mutation.now,
        },
      };
      entries.push([domainStorageKey(current.domain), routeAllocation]);
    }
    if (["pending_verification", "verified"].includes(current.state)) {
      const usage = await this.accountUsage(current.account_id);
      if (usage.allocated_domains < 1 ||
          (current.state === "pending_verification" && usage.open_requests < 1)) {
        fail("custom inbound domain account usage is invalid", 503);
      }
      entries.push([usageKey(current.account_id), {
        ...usage,
        open_requests: usage.open_requests -
          (current.state === "pending_verification" ? 1 : 0),
        allocated_domains: usage.allocated_domains - 1,
        updated_at: mutation.now,
      }]);
      entries.push(...await this.capacityReleaseRebalanceEntries(
        current,
        usage,
      ));
      const activeKey = accountDomainKey(current);
      if (activeKey) deletes.push(activeKey);
      const pendingKey = domainPendingKey(current);
      if (pendingKey) deletes.push(pendingKey);
      const expiryKey = challengeExpiryDueKey(current);
      if (expiryKey) deletes.push(expiryKey);
      for (const verificationKey of new Set([
        verificationDueKey(current),
        effectiveVerificationDueKey(current, refresh),
        storedVerificationRefreshDue(rawRefresh, id),
      ])) if (verificationKey) deletes.push(verificationKey);
      const graceKey = graceDueKey(current);
      if (graceKey) deletes.push(graceKey);
    }
    const body = {
      schema_version: SCHEMA_VERSION,
      request: publicRequest(updated),
    };
    if (routeAllocation) {
      await this.appendCustomDomainRouteSourceIntent(
        entries,
        deletes,
        updated,
        routeAllocation,
      );
    }
    entries.push([idempotencyStorageKey(idempotencyScope, key), {
      fingerprint: fp,
      status: 200,
      body,
    }]);
    await this.atomic(entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return json(body);
  }

  async activeRequestPage(accountID, cursor = null) {
    const prefix = accountDomainPrefix(accountID);
    const listed = await this.storage.list({
      prefix,
      limit: RECONCILE_PAGE_LIMIT + 1,
      ...(cursor ? { startAfter: cursor } : {}),
    });
    const rows = [...listed.entries()];
    const candidates = rows.slice(0, RECONCILE_PAGE_LIMIT);
    let pageLimit = RECONCILE_PAGE_LIMIT;
    for (const [, requestID] of candidates) {
      const bindings = await this.storage.list({
        prefix: routeBindingPrefix(requestID),
        limit: 1,
      });
      if (bindings.size > 0) {
        pageLimit = ROUTE_AWARE_RECONCILE_PAGE_LIMIT;
        break;
      }
    }
    const page = rows.slice(0, pageLimit);
    const requests = [];
    for (const [indexKey, requestID] of page) {
      const request = await this.storage.get(requestStorageKey(requestID));
      if (!request || request.account_id !== accountID) {
        fail("custom inbound domain account index is invalid", 503);
      }
      requests.push({ index_key: indexKey, request });
    }
    return {
      requests,
      next_cursor: rows.length > pageLimit && page.length > 0
        ? page.at(-1)[0]
        : null,
    };
  }

  async accountHasActiveRequest(accountID) {
    const listed = await this.storage.list({
      prefix: accountDomainPrefix(accountID),
      limit: 1,
    });
    return listed.size > 0;
  }

  async accountPolicyConverging(accountID) {
    const [planIntent, lifecycleIntent] = await Promise.all([
      this.storage.get(planIntentKey(accountID)),
      this.storage.get(lifecycleIntentKey(accountID)),
    ]);
    return Boolean(planIntent || lifecycleIntent);
  }

  planIntent(input, state) {
    const now = this.now();
    return {
      account_id: input.account_id,
      plan_revision: input.plan_revision,
      plan_snapshot_hash: input.plan_snapshot_hash,
      feature_enabled: input.feature_enabled,
      domain_limit: input.domain_limit,
      state,
      cursor: null,
      position: 0,
      failure_count: 0,
      retry_at_ms: state === "cell_committed"
        ? now.getTime() + RECONCILE_RETRY_MS
        : null,
      created_at: now.toISOString(),
      updated_at: now.toISOString(),
    };
  }

  async capacityReleaseRebalanceEntries(current, usage) {
    if (current.plan_suspended === true || usage.allocated_domains < 1) {
      return [];
    }
    const [committed, pending, lifecycleIntent, lifecycleFence] =
      await Promise.all([
        this.storage.get(planFenceKey(current.account_id)),
        this.storage.get(planIntentKey(current.account_id)),
        this.storage.get(lifecycleIntentKey(current.account_id)),
        this.storage.get(lifecycleFenceKey(current.account_id)),
      ]);
    const allowed = committed?.feature_enabled === true
      ? committed.domain_limit
      : 0;
    if (!committed || pending || lifecycleIntent || allowed === null ||
        allowed < 1 || usage.allocated_domains - 1 < allowed ||
        (lifecycleFence && lifecycleFence.action !== "republish")) {
      return [];
    }
    const intent = this.planIntent({
      account_id: current.account_id,
      plan_revision: committed.committed_revision,
      plan_snapshot_hash: committed.committed_snapshot_hash,
      feature_enabled: committed.feature_enabled,
      domain_limit: committed.domain_limit,
    }, "cell_committed");
    return [
      [planIntentKey(current.account_id), intent],
      [planDueKey(intent), current.account_id],
    ];
  }

  async persistPlanIntent(intent, previous = null) {
    await this.ensureMeta();
    const entries = [[planIntentKey(intent.account_id), intent]];
    if (planDueKey(intent)) {
      entries.push([planDueKey(intent), intent.account_id]);
    }
    const deletes = previous && planDueKey(previous) !== planDueKey(intent)
      ? [planDueKey(previous)].filter(Boolean)
      : [];
    await this.atomic(entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  desiredPlanRequest(request, intent, position) {
    const allowed = intent.feature_enabled
      ? intent.domain_limit
      : 0;
    const shouldRestrict = allowed !== null && position >= allowed;
    let planSuspended = request.plan_suspended === true;
    let graceUntil = request.plan_grace_until ?? null;
    if (!shouldRestrict) {
      planSuspended = false;
      graceUntil = null;
    } else if (request.state === "pending_verification") {
      planSuspended = true;
      graceUntil = null;
    } else if (request.state === "verified") {
      if (!planSuspended && graceUntil &&
          Date.parse(graceUntil) <= this.now().getTime()) {
        planSuspended = true;
        graceUntil = null;
      } else if (!planSuspended && !graceUntil) {
        graceUntil = new Date(
          this.now().getTime() + DOWNGRADE_GRACE_MS,
        ).toISOString();
      }
    }
    if (planSuspended === (request.plan_suspended === true) &&
        graceUntil === (request.plan_grace_until ?? null)) {
      return request;
    }
    return {
      ...request,
      plan_suspended: planSuspended,
      plan_grace_until: graceUntil,
      state_revision: (request.state_revision ?? 1) + 1,
      updated_at: this.now().toISOString(),
    };
  }

  async drainOneAccountRouteSource(accountID) {
    const listed = await this.storage.list({
      prefix: routeSourceAccountPrefix(accountID),
      limit: 1,
    });
    const first = [...listed.entries()][0];
    if (!first) return { complete: true, changed: 0 };
    const [indexKey, intentKey] = first;
    const intent = typeof intentKey === "string"
      ? await this.storage.get(intentKey)
      : null;
    if (!intent || routeSourceAccountKey(intent) !== indexKey) {
      fail("custom domain route source account index is invalid", 503);
    }
    const result = await this.drainCustomDomainRouteSourceIntent(intent);
    const remaining = await this.storage.list({
      prefix: routeSourceAccountPrefix(accountID),
      limit: 1,
    });
    return {
      complete: remaining.size === 0,
      changed: result.changed ?? 0,
    };
  }

  async finishPlanRouteConvergence(intent) {
    const routes = await this.drainOneAccountRouteSource(intent.account_id);
    if (!routes.complete) {
      const continued = {
        ...intent,
        state: "route_converging",
        failure_count: 0,
        retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.atomic([
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      ], [planDueKey(intent)].filter(Boolean));
      await this.scheduleNextAlarm().catch(() => {});
      return {
        changed: routes.changed,
        complete: false,
        registry_revision: (await this.ensureMeta()).registry_revision,
      };
    }
    await this.atomic([[planFenceKey(intent.account_id), {
      account_id: intent.account_id,
      committed_revision: intent.plan_revision,
      committed_snapshot_hash: intent.plan_snapshot_hash,
      feature_enabled: intent.feature_enabled,
      domain_limit: intent.domain_limit,
      updated_at: this.now().toISOString(),
    }]], [planIntentKey(intent.account_id), planDueKey(intent)].filter(Boolean));
    await this.scheduleNextAlarm().catch(() => {});
    return {
      changed: routes.changed,
      complete: true,
      registry_revision: (await this.ensureMeta()).registry_revision,
    };
  }

  async applyPlanIntent(intent) {
    if (intent.state === "route_converging") {
      return this.finishPlanRouteConvergence(intent);
    }
    const page = await this.activeRequestPage(intent.account_id, intent.cursor);
    const changed = [];
    const derivedDeletes = [];
    const derivedEntries = [];
    let position = intent.position ?? 0;
    let routeWork = false;
    for (const { index_key: indexKey, request } of page.requests) {
      if (!["pending_verification", "verified"].includes(request.state)) {
        derivedDeletes.push(indexKey);
        continue;
      }
      const desired = this.desiredPlanRequest(request, intent, position);
      position += 1;
      if (desired !== request) {
        changed.push({ previous: request, next: desired });
        const rawRefresh = await this.storage.get(
          verificationRefreshKey(request.id),
        );
        const refresh = effectiveVerificationRefresh(request, rawRefresh);
        const oldGrace = graceDueKey(request);
        const nextGrace = graceDueKey(desired);
        if (oldGrace && oldGrace !== nextGrace) derivedDeletes.push(oldGrace);
        if (nextGrace) derivedEntries.push([nextGrace, desired.id]);
        const verificationDue = verificationDueKey(desired);
        for (const previousVerificationDue of new Set([
          verificationDueKey(request),
          effectiveVerificationDueKey(request, refresh),
          storedVerificationRefreshDue(rawRefresh, request.id),
        ])) {
          if (previousVerificationDue &&
              previousVerificationDue !== verificationDue) {
            derivedDeletes.push(previousVerificationDue);
          }
        }
        if (desired.plan_suspended === true ||
            desired.lifecycle_suspended === true) {
          if (verificationDue) derivedDeletes.push(verificationDue);
        } else if (verificationDue) {
          derivedEntries.push([verificationDue, desired.id]);
        }
      }
    }
    const finalPage = page.next_cursor === null;
    const deletes = [...derivedDeletes, planDueKey(intent)];
    const entries = [...derivedEntries];
    if (changed.length > 0) {
      const mutation = await this.mutation(
        { kind: "system", id: "plan-lifecycle" },
        "custom_domain.plan_reconciled",
        intent.account_id,
        {
          account_id: intent.account_id,
          changed: changed.length,
          page_size: page.requests.length,
          plan_revision: intent.plan_revision,
          plan_snapshot_hash: intent.plan_snapshot_hash,
          domain_limit: intent.feature_enabled ? intent.domain_limit : 0,
          downgrade_grace_days:
            AGENT_EMAIL_CUSTOM_DOMAIN_DOWNGRADE_GRACE_DAYS,
        },
      );
      entries.push([META_KEY, mutation.meta], [mutation.audit_key, mutation.audit]);
      for (const item of changed) {
        entries.push([requestStorageKey(item.next.id), item.next]);
        deletes.push(verificationWorkKey(item.next.id));
        deletes.push(verificationRefreshKey(item.next.id));
        const allocation = await this.storage.get(
          domainStorageKey(item.previous.domain),
        );
        if (assertLegacyDomainMirror(allocation, item.previous)) {
          entries.push([domainStorageKey(item.previous.domain), item.next]);
        } else if (item.next.state === "verified") {
          if (!allocation || allocation.source_request_id !== item.next.id ||
              allocation.state !== "allocated") {
            fail("custom inbound domain allocation is invalid", 503);
          }
          const source = await this.appendCustomDomainRouteSourceIntent(
            entries,
            deletes,
            item.next,
            allocation,
          );
          routeWork ||= Boolean(source);
        }
      }
    }
    const pendingRouteWork = routeWork || (finalPage &&
      (await this.storage.list({
        prefix: routeSourceAccountPrefix(intent.account_id),
        limit: 1,
      })).size > 0);
    if (finalPage && pendingRouteWork) {
      const continued = {
        ...intent,
        state: "route_converging",
        cursor: null,
        position,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      entries.push(
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      );
    } else if (finalPage) {
      deletes.push(planIntentKey(intent.account_id));
      entries.push([planFenceKey(intent.account_id), {
        account_id: intent.account_id,
        committed_revision: intent.plan_revision,
        committed_snapshot_hash: intent.plan_snapshot_hash,
        feature_enabled: intent.feature_enabled,
        domain_limit: intent.domain_limit,
        updated_at: this.now().toISOString(),
      }]);
    } else {
      const continued = {
        ...intent,
        cursor: page.next_cursor,
        position,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + RECONCILE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      entries.push(
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      );
    }
    await this.atomic(entries, deletes.filter(Boolean));
    await this.scheduleNextAlarm().catch(() => {});
    return {
      changed: changed.length,
      complete: finalPage && !pendingRouteWork,
      registry_revision: (await this.ensureMeta()).registry_revision,
    };
  }

  async reconcilePlan(input) {
    const accountID = validateAccountID(input?.account_id);
    if (!["restrict_only", "complete"].includes(input?.mode) ||
        typeof input?.feature_enabled !== "boolean" ||
        !Object.hasOwn(input ?? {}, "domain_limit") ||
        !validDomainLimit(input.domain_limit)) {
      fail("invalid custom domain plan reconciliation", 400);
    }
    const planFence = validatePlanFence(input);
    const recoveryProvided = input.recover_pending_revision !== undefined ||
      input.recover_pending_snapshot_hash !== undefined;
    if (recoveryProvided && !validPlanRevisionFence(
      input.recover_pending_revision,
      input.recover_pending_snapshot_hash,
    )) {
      fail("invalid recovery plan fence", 400);
    }
    const [committed, pending] = await Promise.all([
      this.storage.get(planFenceKey(accountID)),
      this.storage.get(planIntentKey(accountID)),
    ]);
    const committedRelation = committed
      ? comparePlanRevision(planFence.revision, committed.committed_revision)
      : 1;
    if (committedRelation === 0 &&
        planFence.snapshot_hash !== committed.committed_snapshot_hash) {
      fail("plan revision conflicts with custom domain policy fence", 409);
    }
    if (committedRelation === 0 &&
        (input.feature_enabled !== committed.feature_enabled ||
          input.domain_limit !== committed.domain_limit)) {
      fail("plan entitlement conflicts with custom domain policy fence", 409);
    }
    if (input.activation_enabled !== true && !pending &&
        committedRelation !== 0 &&
        !await this.accountHasActiveRequest(accountID)) {
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: accountID,
        mode: input.mode,
        pending: false,
        stale: false,
        complete: true,
        changed: 0,
        no_op: true,
      });
    }
    const recoversPending = recoveryProvided && pending &&
      pending.plan_revision === input.recover_pending_revision &&
      pending.plan_snapshot_hash === input.recover_pending_snapshot_hash;

    if (input.mode === "restrict_only") {
      if (committedRelation <= 0) {
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: accountID,
          mode: input.mode,
          pending: false,
          stale: true,
          complete: true,
        });
      }
      if (pending) {
        const relation = comparePlanRevision(planFence.revision, pending.plan_revision);
        if (relation === 0 &&
            planFence.snapshot_hash !== pending.plan_snapshot_hash) {
          fail("plan revision conflicts with pending custom domain policy", 409);
        }
        if (relation === 0 &&
            (input.feature_enabled !== pending.feature_enabled ||
              input.domain_limit !== pending.domain_limit)) {
          fail("plan entitlement conflicts with pending custom domain policy", 409);
        }
        if (relation <= 0) {
          return json({
            schema_version: SCHEMA_VERSION,
            account_id: accountID,
            mode: input.mode,
            pending: false,
            stale: true,
            complete: true,
          });
        }
      }
      const intent = this.planIntent({
        ...input,
        plan_revision: planFence.revision,
        plan_snapshot_hash: planFence.snapshot_hash,
      }, "awaiting_cell");
      await this.persistPlanIntent(intent, pending);
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: accountID,
        mode: input.mode,
        pending: true,
        stale: false,
        complete: true,
      });
    }

    if (committedRelation < 0 && !recoversPending) {
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: accountID,
        mode: input.mode,
        stale: true,
        complete: true,
        changed: 0,
      });
    }
    if (pending && !recoversPending) {
      const relation = comparePlanRevision(planFence.revision, pending.plan_revision);
      if (relation === 0 &&
          planFence.snapshot_hash !== pending.plan_snapshot_hash) {
        fail("plan revision conflicts with pending custom domain policy", 409);
      }
      if (relation === 0 &&
          (input.feature_enabled !== pending.feature_enabled ||
            input.domain_limit !== pending.domain_limit)) {
        fail("plan entitlement conflicts with pending custom domain policy", 409);
      }
      if (relation < 0) {
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: accountID,
          mode: input.mode,
          stale: true,
          complete: true,
          changed: 0,
        });
      }
      if (relation === 0 &&
          ["cell_committed", "route_converging"].includes(pending.state)) {
        const result = await this.applyPlanIntent(pending);
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: accountID,
          mode: input.mode,
          stale: false,
          ...result,
        });
      }
    }
    if (committedRelation === 0 && !pending && !recoversPending) {
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: accountID,
        mode: input.mode,
        stale: false,
        complete: true,
        changed: 0,
        registry_revision: (await this.ensureMeta()).registry_revision,
      });
    }
    const intent = this.planIntent({
      ...input,
      plan_revision: planFence.revision,
      plan_snapshot_hash: planFence.snapshot_hash,
    }, "cell_committed");
    // Completing the exact restrict-only fence advances one durable intent;
    // it does not create a second intent with a different identity. Keeping
    // created_at stable also makes the journal stream self-recoverable.
    if (pending && pending.plan_revision === intent.plan_revision &&
        pending.plan_snapshot_hash === intent.plan_snapshot_hash) {
      intent.created_at = pending.created_at;
    }
    await this.persistPlanIntent(intent, pending);
    const result = await this.applyPlanIntent(intent);
    return json({
      schema_version: SCHEMA_VERSION,
      account_id: accountID,
      mode: input.mode,
      stale: false,
      ...result,
    });
  }

  async reconcileDuePlanIntents() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "plan-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [due, accountID] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || retryAt > now) break;
      const intent = await this.storage.get(planIntentKey(accountID));
      if (!intent || planDueKey(intent) !== due) {
        await this.storage.delete(due);
        continue;
      }
      if (!["cell_committed", "route_converging"].includes(intent.state)) {
        continue;
      }
      try {
        await this.applyPlanIntent(intent);
      } catch {
        const failureCount = (intent.failure_count ?? 0) + 1;
        const retry = {
          ...intent,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() +
            reconcileRetryDelay(failureCount),
          updated_at: this.now().toISOString(),
        };
        await this.atomic(
          [[planIntentKey(accountID), retry], [planDueKey(retry), accountID]],
          [due],
        );
      }
    }
  }

  lifecycleOrder(action) {
    return action === "suspend" ? 1 : action === "republish" ? 2 :
      action === "retire" ? 3 : 0;
  }

  lifecycleRelation(left, right) {
    if (!right) return 1;
    if (left.epoch !== right.epoch) return left.epoch < right.epoch ? -1 : 1;
    if (left.operation_id !== right.operation_id) {
      fail("account lifecycle epoch conflicts with custom domain fence", 409);
    }
    const leftOrder = this.lifecycleOrder(left.action);
    const rightOrder = this.lifecycleOrder(right.action);
    return leftOrder === rightOrder ? 0 : leftOrder < rightOrder ? -1 : 1;
  }

  async persistLifecycleIntent(intent, previous = null) {
    await this.ensureMeta();
    await this.atomic(
      [
        [lifecycleIntentKey(intent.account_id), intent],
        [lifecycleDueKey(intent), intent.account_id],
      ],
      previous && lifecycleDueKey(previous) !== lifecycleDueKey(intent)
        ? [lifecycleDueKey(previous)].filter(Boolean)
        : [],
    );
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  async applyLifecycleIntent(intent) {
    if (intent.phase === "route_converging") {
      const routes = await this.drainOneAccountRouteSource(intent.account_id);
      if (!routes.complete) {
        const continued = {
          ...intent,
          phase: "route_converging",
          failure_count: 0,
          retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [lifecycleIntentKey(intent.account_id), continued],
          [lifecycleDueKey(continued), intent.account_id],
        ], [lifecycleDueKey(intent)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return {
          schema_version: SCHEMA_VERSION,
          account_id: intent.account_id,
          operation_id: intent.operation_id,
          epoch: intent.epoch,
          action: intent.action,
          changed: routes.changed,
          complete: false,
        };
      }
      await this.atomic([[lifecycleFenceKey(intent.account_id), {
        account_id: intent.account_id,
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
        completed_at: this.now().toISOString(),
      }]], [
        lifecycleIntentKey(intent.account_id),
        lifecycleDueKey(intent),
      ].filter(Boolean));
      await this.scheduleNextAlarm().catch(() => {});
      return {
        schema_version: SCHEMA_VERSION,
        account_id: intent.account_id,
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
        changed: routes.changed,
        complete: true,
      };
    }
    const page = await this.activeRequestPage(intent.account_id, intent.cursor);
    const changedAt = this.now().toISOString();
    const entries = [];
    const deletes = [lifecycleDueKey(intent)].filter(Boolean);
    const changed = [];
    let routeWork = false;
    let pendingRetired = 0;
    let allocatedRetired = 0;
    for (const { index_key: indexKey, request } of page.requests) {
      if (!["pending_verification", "verified"].includes(request.state)) {
        deletes.push(indexKey);
        continue;
      }
      const rawRefresh = await this.storage.get(
        verificationRefreshKey(request.id),
      );
      const refresh = effectiveVerificationRefresh(request, rawRefresh);
      const previousVerificationDues = new Set([
        verificationDueKey(request),
        effectiveVerificationDueKey(request, refresh),
        storedVerificationRefreshDue(rawRefresh, request.id),
      ]);
      const nextFence = {
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
      };
      let desired;
      let routeAllocation = null;
      if (intent.action === "retire") {
        desired = {
          ...request,
          state: "retired",
          state_revision: (request.state_revision ?? 1) + 1,
          lifecycle_suspended: true,
          lifecycle_fence: nextFence,
          plan_grace_until: null,
          updated_at: changedAt,
          retirement: {
            reason: "account closed",
            retired_by: "account-lifecycle",
            retired_at: changedAt,
          },
        };
        allocatedRetired += 1;
        if (request.state === "pending_verification") pendingRetired += 1;
        deletes.push(indexKey);
        for (const key of [
          domainPendingKey(request),
          challengeExpiryDueKey(request),
          ...previousVerificationDues,
          graceDueKey(request),
        ]) if (key) deletes.push(key);
        if (request.state === "verified") {
          const allocation = await this.storage.get(
            domainStorageKey(request.domain),
          );
          if (!allocation || allocation.source_request_id !== request.id ||
              allocation.state !== "allocated") {
            fail("custom inbound domain allocation is invalid", 503);
          }
          routeAllocation = {
            ...allocation,
            state: "retired",
            allocation_revision: (allocation.allocation_revision ?? 1) + 1,
            updated_at: desired.updated_at,
            retirement: { ...desired.retirement },
          };
          entries.push([domainStorageKey(request.domain), routeAllocation]);
        }
      } else {
        const lifecycleSuspended = intent.action === "suspend";
        const sameFence = fingerprint(request.lifecycle_fence ?? null) ===
          fingerprint(nextFence);
        if (lifecycleSuspended === (request.lifecycle_suspended === true) &&
            sameFence) {
          continue;
        }
        desired = {
          ...request,
          lifecycle_suspended: lifecycleSuspended,
          lifecycle_fence: nextFence,
          state_revision: (request.state_revision ?? 1) + 1,
          updated_at: changedAt,
        };
        const verificationDue = verificationDueKey(desired);
        for (const previousVerificationDue of previousVerificationDues) {
          if (previousVerificationDue &&
              previousVerificationDue !== verificationDue) {
            deletes.push(previousVerificationDue);
          }
        }
        if (lifecycleSuspended || desired.plan_suspended === true) {
          if (verificationDue) deletes.push(verificationDue);
        } else if (verificationDue) {
          entries.push([verificationDue, desired.id]);
        }
      }
      changed.push({ previous: request, next: desired });
      entries.push([requestStorageKey(request.id), desired]);
      deletes.push(verificationWorkKey(request.id));
      deletes.push(verificationRefreshKey(request.id));
      const legacyMirror = await this.storage.get(
        domainStorageKey(request.domain),
      );
      if (assertLegacyDomainMirror(legacyMirror, request)) {
        entries.push([domainStorageKey(request.domain), desired]);
      } else if (request.state === "verified") {
        routeAllocation ??= legacyMirror;
        if (!routeAllocation ||
            routeAllocation.source_request_id !== request.id) {
          fail("custom inbound domain allocation is invalid", 503);
        }
        const source = await this.appendCustomDomainRouteSourceIntent(
          entries,
          deletes,
          desired,
          routeAllocation,
        );
        routeWork ||= Boolean(source);
      }
    }
    if (intent.action === "retire" && allocatedRetired > 0) {
      const usage = await this.accountUsage(intent.account_id);
      if (usage.allocated_domains < allocatedRetired ||
          usage.open_requests < pendingRetired) {
        fail("custom inbound domain account usage is invalid", 503);
      }
      entries.push([usageKey(intent.account_id), {
        ...usage,
        allocated_domains: usage.allocated_domains - allocatedRetired,
        open_requests: usage.open_requests - pendingRetired,
        updated_at: changedAt,
      }]);
    }
    if (changed.length > 0) {
      const mutation = await this.mutation(
        { kind: "system", id: "account-lifecycle" },
        `custom_domain.lifecycle_${intent.action}`,
        intent.account_id,
        {
          account_id: intent.account_id,
          operation_id: intent.operation_id,
          epoch: intent.epoch,
          changed: changed.length,
          page_size: page.requests.length,
        },
      );
      entries.push([META_KEY, mutation.meta], [mutation.audit_key, mutation.audit]);
    }
    const finalPage = page.next_cursor === null;
    const pendingRouteWork = finalPage && (routeWork ||
      (await this.storage.list({
        prefix: routeSourceAccountPrefix(intent.account_id),
        limit: 1,
      })).size > 0);
    const complete = finalPage && !pendingRouteWork;
    if (finalPage && pendingRouteWork) {
      const continued = {
        ...intent,
        phase: "route_converging",
        cursor: null,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      entries.push(
        [lifecycleIntentKey(intent.account_id), continued],
        [lifecycleDueKey(continued), intent.account_id],
      );
    } else if (complete) {
      deletes.push(lifecycleIntentKey(intent.account_id));
      entries.push([lifecycleFenceKey(intent.account_id), {
        account_id: intent.account_id,
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
        completed_at: this.now().toISOString(),
      }]);
    } else {
      const continued = {
        ...intent,
        cursor: page.next_cursor,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + RECONCILE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      entries.push(
        [lifecycleIntentKey(intent.account_id), continued],
        [lifecycleDueKey(continued), intent.account_id],
      );
    }
    await this.atomic(entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return {
      schema_version: SCHEMA_VERSION,
      account_id: intent.account_id,
      operation_id: intent.operation_id,
      epoch: intent.epoch,
      action: intent.action,
      changed: changed.length,
      complete,
    };
  }

  async reconcileAccountLifecycle(input) {
    const accountID = validateAccountID(input?.account_id);
    if (!IDEMPOTENCY_KEY_PATTERN.test(input?.operation_id ?? "") ||
        !Number.isSafeInteger(input?.epoch) || input.epoch < 0 ||
        !["suspend", "republish", "retire"].includes(input?.action)) {
      fail("invalid custom domain account lifecycle reconciliation", 400);
    }
    const requested = {
      account_id: accountID,
      operation_id: input.operation_id,
      epoch: input.epoch,
      action: input.action,
    };
    const [fence, existingIntent] = await Promise.all([
      this.storage.get(lifecycleFenceKey(accountID)),
      this.storage.get(lifecycleIntentKey(accountID)),
    ]);
    const fenceRelation = this.lifecycleRelation(requested, fence);
    if (fenceRelation < 0) {
      fail("custom domain account lifecycle fence is stale", 409);
    }
    if (fenceRelation === 0) {
      return json({
        schema_version: SCHEMA_VERSION,
        ...requested,
        changed: 0,
        complete: true,
        replayed: true,
      });
    }
    if (input.activation_enabled !== true && !existingIntent &&
        !await this.accountHasActiveRequest(accountID)) {
      return json({
        schema_version: SCHEMA_VERSION,
        ...requested,
        changed: 0,
        complete: true,
        no_op: true,
      });
    }
    let intent = existingIntent;
    if (intent) {
      const relation = this.lifecycleRelation(requested, intent);
      if (relation < 0) {
        fail("custom domain account lifecycle intent is stale", 409);
      }
      if (relation > 0) {
        fail("earlier custom domain lifecycle work is still converging", 409);
      }
    } else {
      const now = this.now();
      intent = {
        ...requested,
        phase: "requests",
        cursor: null,
        failure_count: 0,
        retry_at_ms: now.getTime(),
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      };
      await this.persistLifecycleIntent(intent);
    }
    return json(await this.applyLifecycleIntent(intent));
  }

  async reconcileDueLifecycles() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "lifecycle-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [due, accountID] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || retryAt > now) break;
      const intent = await this.storage.get(lifecycleIntentKey(accountID));
      if (!intent || lifecycleDueKey(intent) !== due) {
        await this.storage.delete(due);
        continue;
      }
      try {
        await this.applyLifecycleIntent(intent);
      } catch {
        const failureCount = (intent.failure_count ?? 0) + 1;
        const retry = {
          ...intent,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() +
            reconcileRetryDelay(failureCount),
          updated_at: this.now().toISOString(),
        };
        await this.atomic(
          [
            [lifecycleIntentKey(accountID), retry],
            [lifecycleDueKey(retry), accountID],
          ],
          [due],
        );
      }
    }
  }

  assertVerificationEnabled(input) {
    if (!this.verificationEnabled(input)) {
      fail("custom domain ownership verification is not enabled", 409,
        "custom_domain_verification_disabled");
    }
  }

  verificationEnabled(input) {
    return input?.verification_enabled === true &&
      agentEmailCustomDomainVerificationEnabled(this.env);
  }

  async discardExactVerificationWork(requestID, claimID, generation) {
    const key = verificationWorkKey(requestID);
    const work = await this.storage.get(key);
    if (validVerificationWork(work, requestID) &&
        work.claim_id === claimID && work.generation === generation) {
      await this.atomicRaw([], [key]);
      return true;
    }
    return false;
  }

  async nextVerificationClaimID(previous = null) {
    for (let attempt = 0; attempt < 4; attempt += 1) {
      const value = await this.newVerificationClaimID();
      if (VERIFICATION_CLAIM_PATTERN.test(value ?? "") && value !== previous) {
        return value;
      }
    }
    fail("custom domain verification claim is unavailable", 503,
      "verification_claim_unavailable");
  }

  async persistVerificationClaim(current, mode, options = {}) {
    const key = verificationWorkKey(current.id);
    const rawRefresh = await this.storage.get(verificationRefreshKey(current.id));
    let refresh = effectiveVerificationRefresh(current, rawRefresh);
    if (rawRefresh && !refresh) {
      await this.repairVerificationSchedule(
        current,
        options.verification_due_key ?? null,
      );
      refresh = null;
    }
    const effectiveDue = effectiveVerificationDueKey(current, refresh);
    if (options.verification_due_key !== undefined &&
        options.verification_due_key !== effectiveDue) {
      return { claimed: false };
    }
    const existing = await this.storage.get(key);
    if (existing && !validVerificationWork(existing, current.id)) {
      fail("custom domain verification claim is invalid", 503,
        "verification_claim_invalid");
    }
    const now = this.now();
    const active = existing && Date.parse(existing.lease_expires_at) >
      now.getTime();
    const sameManual = mode === "manual" && existing?.mode === "manual" &&
      existing.actor?.id === options.actor?.id &&
      existing.idempotency_key === options.idempotency_key &&
      existing.idempotency_fingerprint === options.idempotency_fingerprint;
    const exactRequestFence = existing &&
      existing.request_state_revision === (current.state_revision ?? 1) &&
      existing.request_updated_at === current.updated_at &&
      existing.verification_due_key === effectiveDue &&
      (existing.verification_refresh_generation ?? 0) ===
        (refresh?.generation ?? 0) &&
      canonicalJSONString(existing.ownership_challenge) ===
        canonicalJSONString({
          ...current.ownership_challenge,
          expires_at: challengeExpiresAt(current),
        });
    if (active) {
      // Only an already-observed exact manual claim may be resumed. Returning
      // an active claimed fence to two callers would let both perform the
      // external lookup and race different observations under one generation.
      if (sameManual && exactRequestFence && existing.phase === "observed") {
        return { claimed: true, work: existing, replayed: true };
      }
      if (mode === "manual") {
        fail("custom domain verification is already in progress", 409,
          "verification_in_progress");
      }
      return { claimed: false };
    }

    const preserveObservation = existing?.phase === "observed" &&
      exactRequestFence && existing.mode === mode &&
      (mode === "scheduled" || sameManual) &&
      now.getTime() - Date.parse(existing.observed_at) <=
        VERIFICATION_OBSERVATION_MAX_AGE_MS;
    const claimedAt = now.toISOString();
    const work = {
      schema_version: VERIFICATION_WORK_SCHEMA,
      request_id: current.id,
      mode,
      claim_id: await this.nextVerificationClaimID(existing?.claim_id),
      generation: (existing?.generation ?? 0) + 1,
      request_state_revision: current.state_revision ?? 1,
      request_updated_at: current.updated_at,
      verification_due_key: effectiveDue,
      verification_refresh_generation: refresh?.generation ?? 0,
      ownership_challenge: {
        ...current.ownership_challenge,
        expires_at: challengeExpiresAt(current),
      },
      actor: mode === "manual"
        ? { ...options.actor }
        : { kind: "system", id: "ownership-verifier" },
      idempotency_key: mode === "manual" ? options.idempotency_key : null,
      idempotency_fingerprint: mode === "manual"
        ? options.idempotency_fingerprint
        : null,
      phase: preserveObservation ? "observed" : "claimed",
      claimed_at: claimedAt,
      lease_expires_at: new Date(
        now.getTime() + VERIFICATION_CLAIM_LEASE_MS,
      ).toISOString(),
      observation: preserveObservation
        ? structuredClone(existing.observation)
        : null,
      ...(preserveObservation ? { observed_at: existing.observed_at } : {}),
    };
    if (!validVerificationWork(work, current.id)) {
      fail("custom domain verification claim is invalid", 503,
        "verification_claim_invalid");
    }
    await this.atomicRaw([[key, work]]);
    return { claimed: true, work, replayed: false };
  }

  async claimVerification(input) {
    if (input?.mode === "manual") {
      const actor = validateActor(input?.actor, "platform_admin");
      const id = validateRequestID(input?.request_id);
      const idempotencyKey = validateIdempotencyKey(input?.idempotency_key);
      const scope = `request-verify:${id}`;
      const fp = fingerprint(["request.verify", id]);
      const replay = await this.idempotentReplay(scope, idempotencyKey, fp);
      if (replay) {
        return {
          kind: "result",
          status: replay.status,
          body: await replay.json(),
          matched: replay.status === 200,
        };
      }
      this.assertVerificationEnabled(input);
      const current = await this.storage.get(requestStorageKey(id));
      if (!current) fail("custom inbound domain request not found", 404);
      if (!["pending_verification", "verified"].includes(current.state)) {
        fail("custom inbound domain request cannot be verified", 409);
      }
      if (current.plan_suspended === true ||
          current.lifecycle_suspended === true) {
        fail("custom inbound domain verification is suspended by account policy",
          409, "domain_verification_suspended");
      }
      if (current.state === "pending_verification" &&
          Date.parse(challengeExpiresAt(current) ?? "") <=
            this.now().getTime()) {
        const expired = await this.expirePendingRequest(current, {
          idempotency_scope: scope,
          idempotency_key: idempotencyKey,
          fingerprint: fp,
          extra_deletes: [verificationWorkKey(id)],
        });
        return {
          kind: "result",
          status: expired.status,
          body: expired.body,
          matched: false,
        };
      }
      if (await this.accountPolicyConverging(current.account_id)) {
        fail("custom inbound domain account policy is still converging", 409,
          "account_policy_converging");
      }
      const claimed = await this.persistVerificationClaim(current, "manual", {
        actor,
        idempotency_key: idempotencyKey,
        idempotency_fingerprint: fp,
      });
      return { kind: "claim", claim: publicVerificationClaim(claimed.work) };
    }

    if (input?.mode !== "scheduled") {
      fail("custom domain verification claim mode is invalid", 400);
    }
    this.assertVerificationEnabled(input);
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "verification-due:",
      limit: VERIFICATION_CLAIM_SCAN_LIMIT,
    });
    for (const [due, requestID] of listed) {
      const dueAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(dueAt) || dueAt > now) break;
      const current = await this.storage.get(requestStorageKey(requestID));
      if (!current ||
          !["pending_verification", "verified"].includes(current.state)) {
        await this.atomicRaw([], [
          due,
          verificationRefreshKey(requestID),
          verificationWorkKey(requestID),
        ]);
        continue;
      }
      const rawRefresh = await this.storage.get(
        verificationRefreshKey(current.id),
      );
      let refresh = effectiveVerificationRefresh(current, rawRefresh);
      if (rawRefresh && !refresh) {
        const repairedDue = await this.repairVerificationSchedule(current, due);
        if (repairedDue !== due) continue;
      }
      refresh = refresh ?? await this.verificationRefresh(current);
      if (effectiveVerificationDueKey(current, refresh) !== due) {
        await this.atomicRaw([], [due]);
        continue;
      }
      if (current.state === "pending_verification" &&
          Date.parse(challengeExpiresAt(current) ?? "") <= now) {
        await this.expirePendingRequest(current, {
          extra_deletes: [verificationWorkKey(current.id)],
        });
        return { kind: "processed", checked: false, matched: false };
      }
      if (current.plan_suspended === true ||
          current.lifecycle_suspended === true) {
        await this.atomicRaw([], [
          due,
          verificationRefreshKey(current.id),
          verificationWorkKey(current.id),
        ]);
        continue;
      }
      if (await this.accountPolicyConverging(current.account_id)) {
        await this.deferVerificationForPolicy(current, due, {
          scheduled: true,
          extra_deletes: [verificationWorkKey(current.id)],
        });
        return { kind: "processed", checked: true, matched: false };
      }
      const claimed = await this.persistVerificationClaim(current, "scheduled", {
        verification_due_key: due,
      });
      if (!claimed.claimed) continue;
      return { kind: "claim", claim: publicVerificationClaim(claimed.work) };
    }
    return { kind: "empty" };
  }

  async observeVerification(input) {
    const id = validateRequestID(input?.request_id);
    if (!VERIFICATION_CLAIM_PATTERN.test(input?.claim_id ?? "") ||
        !Number.isSafeInteger(input?.generation) || input.generation < 1 ||
        !validVerificationObservation(input?.observation)) {
      fail("custom domain verification observation is invalid", 400);
    }
    if (!this.verificationEnabled(input)) {
      await this.discardExactVerificationWork(
        id,
        input.claim_id,
        input.generation,
      );
      this.assertVerificationEnabled(input);
    }
    const key = verificationWorkKey(id);
    const work = await this.storage.get(key);
    if (!validVerificationWork(work, id)) {
      fail("custom domain verification claim is stale", 409,
        "verification_claim_stale");
    }
    if (work.claim_id !== input.claim_id ||
        work.generation !== input.generation) {
      fail("custom domain verification claim is stale", 409,
        "verification_claim_stale");
    }
    if (work.phase === "observed") {
      if (canonicalJSONString(work.observation) !==
          canonicalJSONString(input.observation)) {
        fail("custom domain verification observation conflicts", 409,
          "verification_observation_conflict");
      }
      return { kind: "observed", claim: publicVerificationClaim(work) };
    }
    const now = this.now();
    const observed = {
      ...work,
      phase: "observed",
      observation: structuredClone(input.observation),
      observed_at: now.toISOString(),
      lease_expires_at: new Date(
        now.getTime() + VERIFICATION_CLAIM_LEASE_MS,
      ).toISOString(),
    };
    if (!validVerificationWork(observed, id)) {
      fail("custom domain verification observation is invalid", 400);
    }
    await this.atomicRaw([[key, observed]]);
    return { kind: "observed", claim: publicVerificationClaim(observed) };
  }

  async commitVerification(input) {
    const id = validateRequestID(input?.request_id);
    if (!VERIFICATION_CLAIM_PATTERN.test(input?.claim_id ?? "") ||
        !Number.isSafeInteger(input?.generation) || input.generation < 1) {
      fail("custom domain verification commit is invalid", 400);
    }
    if (!this.verificationEnabled(input)) {
      await this.discardExactVerificationWork(
        id,
        input.claim_id,
        input.generation,
      );
      this.assertVerificationEnabled(input);
    }
    const workKey = verificationWorkKey(id);
    const work = await this.storage.get(workKey);
    if (!validVerificationWork(work, id) ||
        work.claim_id !== input.claim_id ||
        work.generation !== input.generation) {
      fail("custom domain verification claim is stale", 409,
        "verification_claim_stale");
    }
    if (work.phase !== "observed") {
      await this.atomicRaw([], [workKey]);
      fail("custom domain verification observation is stale", 409,
        "verification_observation_stale");
    }
    const current = await this.storage.get(requestStorageKey(id));
    const rawRefresh = current
      ? await this.storage.get(verificationRefreshKey(id))
      : null;
    const refresh = current
      ? effectiveVerificationRefresh(current, rawRefresh)
      : null;
    const exactRequestFence = current &&
      (!rawRefresh || refresh) &&
      (current.state_revision ?? 1) === work.request_state_revision &&
      current.updated_at === work.request_updated_at &&
      effectiveVerificationDueKey(current, refresh) ===
        work.verification_due_key &&
      (refresh?.generation ?? 0) ===
        (work.verification_refresh_generation ?? 0) &&
      canonicalJSONString({
        ...current.ownership_challenge,
        expires_at: challengeExpiresAt(current),
      }) === canonicalJSONString(work.ownership_challenge);
    if (!exactRequestFence ||
        !["pending_verification", "verified"].includes(current.state)) {
      await this.atomicRaw([], [workKey]);
      fail("custom domain verification claim is stale", 409,
        "verification_claim_stale");
    }
    if (current.plan_suspended === true ||
        current.lifecycle_suspended === true) {
      await this.atomicRaw([], [workKey]);
      fail("custom inbound domain verification is suspended by account policy",
        409, "domain_verification_suspended");
    }
    if (current.state === "pending_verification" &&
        Date.parse(challengeExpiresAt(current) ?? "") <=
          this.now().getTime()) {
      const expired = await this.expirePendingRequest(current, {
        ...(work.mode === "manual" ? {
          idempotency_scope: `request-verify:${id}`,
          idempotency_key: work.idempotency_key,
          fingerprint: work.idempotency_fingerprint,
        } : {}),
        extra_deletes: [workKey],
      });
      return {
        kind: "result",
        status: expired.status,
        body: expired.body,
        matched: false,
      };
    }
    if (this.now().getTime() - Date.parse(work.observed_at) >
        VERIFICATION_OBSERVATION_MAX_AGE_MS) {
      await this.atomicRaw([], [workKey]);
      fail("custom domain verification observation is stale", 409,
        "verification_observation_stale");
    }
    if (await this.accountPolicyConverging(current.account_id)) {
      if (work.mode === "scheduled") {
        await this.deferVerificationForPolicy(
          current,
          work.verification_due_key,
          {
            scheduled: true,
            expected_refresh_generation:
              work.verification_refresh_generation ?? 0,
            extra_deletes: [workKey],
          },
        );
        return {
          kind: "result",
          status: 200,
          body: { schema_version: SCHEMA_VERSION, deferred: true },
          matched: false,
        };
      }
      await this.atomicRaw([], [workKey]);
      fail("custom inbound domain account policy is still converging", 409,
        "account_policy_converging");
    }
    if (work.observation.kind === "temporary_error" &&
        work.mode === "manual") {
      await this.atomicRaw([], [workKey]);
      fail(resolverErrorMessage(work.observation.code), 503,
        work.observation.code);
    }
    const options = {
      actor: work.actor,
      scheduled: work.mode === "scheduled",
      temporary: work.observation.kind === "temporary_error",
      matched: work.observation.kind === "resolved"
        ? work.observation.matched
        : false,
      extra_deletes: [workKey],
      previous_due: work.verification_due_key,
      expected_refresh_generation: work.verification_refresh_generation ?? 0,
      ...(work.mode === "manual" ? {
        idempotency_scope: `request-verify:${id}`,
        idempotency_key: work.idempotency_key,
        fingerprint: work.idempotency_fingerprint,
      } : {}),
    };
    const result = work.observation.kind === "resolved"
      ? work.observation
      : {};
    let committed;
    try {
      committed = await this.applyVerificationObservation(
        current,
        result,
        options,
      );
    } catch (error) {
      if (!(error instanceof DomainRegistryError) ||
          error.code !== "domain_unavailable") throw error;
      committed = await this.deferContendedVerification(
        current,
        work.verification_due_key,
        options,
      );
    }
    return {
      kind: "result",
      status: committed.status,
      body: committed.body,
      matched: committed.matched,
    };
  }

  async applyVerificationObservation(current, result, options = {}) {
    const checkedAt = this.now().toISOString();
    if (current.state === "pending_verification" &&
        Date.parse(challengeExpiresAt(current) ?? "") <=
          Date.parse(checkedAt)) {
      const expired = await this.expirePendingRequest(current, options);
      return { ...expired, matched: false, expired: true };
    }
    const previousRefresh = await this.verificationRefresh(current);
    const previousVerification = previousRefresh?.ownership_verification ??
      current.ownership_verification;
    const matched = options.temporary !== true && options.matched === true;
    let verificationState;
    if (options.temporary === true) {
      verificationState = current.state === "verified"
        ? previousVerification?.state ?? "verified"
        : "unverified";
    } else {
      verificationState = matched
        ? "verified"
        : current.state === "verified" ? "stale" : "missing";
    }
    const consecutiveFailures = matched
      ? 0
      : (previousVerification?.consecutive_failures ?? 0) + 1;
    const nextCheckAt = new Date(
      Date.parse(checkedAt) + (options.temporary === true
        ? VERIFICATION_RESOLVER_RETRY_MS
        : matched ? VERIFICATION_INTERVAL_MS : VERIFICATION_RETRY_MS),
    ).toISOString();
    const verification = {
      state: verificationState,
      last_result: options.temporary === true
        ? "resolver_error"
        : matched ? "present" : "absent",
      first_verified_at: matched
        ? previousVerification?.first_verified_at ?? checkedAt
        : previousVerification?.first_verified_at ?? null,
      last_checked_at: checkedAt,
      last_verified_at: matched
        ? checkedAt
        : previousVerification?.last_verified_at ?? null,
      next_check_at: nextCheckAt,
      rrset_sha256: options.temporary === true
        ? previousVerification?.rrset_sha256 ?? null
        : result.rrset_sha256,
      dnssec_authenticated: options.temporary === true
        ? previousVerification?.dnssec_authenticated ?? false
        : result.dnssec_authenticated === true,
      minimum_ttl_seconds: options.temporary === true
        ? previousVerification?.minimum_ttl_seconds ?? null
        : result.minimum_ttl_seconds,
      consecutive_failures: consecutiveFailures,
    };
    const newlyVerified = matched && current.state === "pending_verification";
    const desired = {
      ...current,
      state: newlyVerified ? "verified" : current.state,
      state_revision: (current.state_revision ?? 1) + 1,
      ownership_verification: verification,
      updated_at: checkedAt,
    };
    const unchangedScheduled = options.scheduled === true &&
      !newlyVerified &&
      sameVerificationAuditOutcome(previousVerification, verification);
    if (unchangedScheduled) {
      const refresh = await this.commitVerificationRefresh(
        current,
        verification,
        {
          previous_due: options.previous_due,
          expected_generation: options.expected_refresh_generation,
          extra_deletes: options.extra_deletes,
        },
      );
      const body = matched ? {
        schema_version: SCHEMA_VERSION,
        request: publicRequest(current, refresh),
        matched: true,
      } : {
        schema_version: SCHEMA_VERSION,
        error: "custom domain ownership challenge was not found",
        code: "ownership_challenge_not_found",
        request: publicRequest(current, refresh),
      };
      return { body, matched, status: matched ? 200 : 409 };
    }
    const entries = [];
    const deletes = [];
    let routeAllocation = null;
    const previousDue = options.previous_due ??
      effectiveVerificationDueKey(current, previousRefresh);
    if (previousDue) deletes.push(previousDue);
    deletes.push(verificationRefreshKey(current.id));
    entries.push([verificationDueKey(desired), desired.id]);
    if (newlyVerified) {
      const existing = await this.storage.get(domainStorageKey(current.domain));
      const legacyMirror = assertLegacyDomainMirror(existing, current);
      if (existing && !legacyMirror &&
          existing.source_request_id !== current.id) {
        fail("domain was allocated by another verified request", 409,
          "domain_unavailable");
      }
      const allocation = !existing || legacyMirror ? {
        schema_version: "witself.agent-email-domain-allocation.v1",
        domain: current.domain,
        account_id: current.account_id,
        source_request_id: current.id,
        generation: 1,
        allocation_revision: 1,
        state: "allocated",
        allocated_at: checkedAt,
        updated_at: checkedAt,
        ownership_proof: null,
        retirement: null,
      } : existing;
      routeAllocation = {
        ...allocation,
        allocation_revision: existing && !legacyMirror
          ? (existing.allocation_revision ?? 1) + 1
          : allocation.allocation_revision,
        updated_at: checkedAt,
        ownership_proof: {
          verified_at: checkedAt,
          rrset_sha256: result.rrset_sha256,
          dnssec_authenticated: result.dnssec_authenticated === true,
        },
      };
      entries.push([domainStorageKey(current.domain), routeAllocation]);
      const usage = await this.accountUsage(current.account_id);
      if (usage.open_requests < 1) {
        fail("custom inbound domain account usage is invalid", 503);
      }
      entries.push([usageKey(current.account_id), {
        ...usage,
        open_requests: usage.open_requests - 1,
        updated_at: checkedAt,
      }]);
      for (const key of [
        domainPendingKey(current),
        challengeExpiryDueKey(current),
      ]) if (key) deletes.push(key);
    } else if (current.state === "pending_verification") {
      const legacyMirror = await this.storage.get(
        domainStorageKey(current.domain),
      );
      if (assertLegacyDomainMirror(legacyMirror, current)) {
        entries.push([domainStorageKey(current.domain), desired]);
      }
    } else if (matched && current.state === "verified") {
      const allocation = await this.storage.get(domainStorageKey(current.domain));
      if (!allocation || allocation.source_request_id !== current.id ||
          allocation.state !== "allocated") {
        fail("custom inbound domain allocation is invalid", 503);
      }
      routeAllocation = {
        ...allocation,
        allocation_revision: (allocation.allocation_revision ?? 1) + 1,
        updated_at: checkedAt,
        ownership_proof: {
          verified_at: checkedAt,
          rrset_sha256: result.rrset_sha256,
          dnssec_authenticated: result.dnssec_authenticated === true,
        },
      };
      entries.push([domainStorageKey(current.domain), routeAllocation]);
    }
    const auditRequired = options.scheduled !== true ||
      !sameVerificationAuditOutcome(previousVerification, verification);
    if (auditRequired) {
      const action = options.temporary === true
        ? "custom_domain.verification_deferred"
        : matched
        ? newlyVerified
          ? "custom_domain.verified"
          : "custom_domain.reverified"
        : "custom_domain.verification_missing";
      const mutation = await this.mutation(
        options.actor ?? { kind: "system", id: "ownership-verifier" },
        action,
        current.domain,
        {
          account_id: current.account_id,
          request_id: current.id,
          state: verification.state,
          rrset_sha256: verification.rrset_sha256,
        },
        checkedAt,
      );
      entries.push(
        [META_KEY, mutation.meta],
        [mutation.audit_key, mutation.audit],
      );
    }
    entries.push([requestStorageKey(current.id), desired]);
    const body = matched ? {
      schema_version: SCHEMA_VERSION,
      request: publicRequest(desired),
      matched: true,
    } : {
      schema_version: SCHEMA_VERSION,
      error: "custom domain ownership challenge was not found",
      code: "ownership_challenge_not_found",
      request: publicRequest(desired),
    };
    const status = matched ? 200 : 409;
    if (options.idempotency_scope && options.idempotency_key) {
      entries.push([idempotencyStorageKey(
        options.idempotency_scope,
        options.idempotency_key,
      ), {
        fingerprint: options.fingerprint,
        status,
        body,
      }]);
    }
    if (desired.state === "verified") {
      routeAllocation ??= await this.storage.get(
        domainStorageKey(current.domain),
      );
      if (!routeAllocation ||
          routeAllocation.source_request_id !== current.id ||
          routeAllocation.state !== "allocated") {
        fail("custom inbound domain allocation is invalid", 503);
      }
      await this.appendCustomDomainRouteSourceIntent(
        entries,
        deletes,
        desired,
        routeAllocation,
      );
    }
    await this.atomic(entries, [
      ...deletes,
      ...(Array.isArray(options.extra_deletes) ? options.extra_deletes : []),
    ]);
    await this.scheduleNextAlarm().catch(() => {});
    return { body, matched, status };
  }

  async deferContendedVerification(current, previousDue, options = {}) {
    const checkedAt = this.now().toISOString();
    const expiresAt = challengeExpiresAt(current);
    if (!expiresAt) {
      fail("custom inbound domain ownership challenge is invalid", 503);
    }
    const previousRefresh = await this.verificationRefresh(current);
    const previous = previousRefresh?.ownership_verification ??
      current.ownership_verification;
    const desired = {
      ...current,
      state_revision: (current.state_revision ?? 1) + 1,
      updated_at: checkedAt,
      ownership_verification: {
        state: "conflict",
        last_result: "domain_unavailable",
        first_verified_at: previous?.first_verified_at ?? null,
        last_checked_at: checkedAt,
        last_verified_at: previous?.last_verified_at ?? null,
        next_check_at: expiresAt,
        rrset_sha256: previous?.rrset_sha256 ?? null,
        dnssec_authenticated:
          previous?.dnssec_authenticated === true,
        minimum_ttl_seconds: previous?.minimum_ttl_seconds ?? null,
        consecutive_failures: (previous?.consecutive_failures ?? 0) + 1,
      },
    };
    const mutation = await this.mutation(
      options.actor ?? { kind: "system", id: "ownership-verifier" },
      "custom_domain.verification_conflict",
      current.domain,
      {
        account_id: current.account_id,
        request_id: current.id,
        state: "conflict",
      },
      checkedAt,
    );
    const body = {
      schema_version: SCHEMA_VERSION,
      error: "domain was allocated by another verified request",
      code: "domain_unavailable",
      request: publicRequest(desired),
    };
    const entries = [
      [META_KEY, mutation.meta],
      [mutation.audit_key, mutation.audit],
      [requestStorageKey(current.id), desired],
      [verificationDueKey(desired), desired.id],
    ];
    if (options.idempotency_scope && options.idempotency_key) {
      entries.push([idempotencyStorageKey(
        options.idempotency_scope,
        options.idempotency_key,
      ), {
        fingerprint: options.fingerprint,
        status: 409,
        body,
      }]);
    }
    await this.atomic(entries, [
      ...[previousDue].filter(Boolean),
      verificationRefreshKey(current.id),
      ...(Array.isArray(options.extra_deletes) ? options.extra_deletes : []),
    ]);
    await this.scheduleNextAlarm().catch(() => {});
    return { body, matched: false, status: 409, conflict: true };
  }

  async deferVerificationForPolicy(current, previousDue, options = {}) {
    const checkedAt = this.now().toISOString();
    const previousRefresh = await this.verificationRefresh(current);
    const previous = previousRefresh?.ownership_verification ??
      current.ownership_verification;
    const desired = {
      ...current,
      state_revision: (current.state_revision ?? 1) + 1,
      updated_at: checkedAt,
      ownership_verification: {
        state: previous?.state ?? "unverified",
        last_result: "policy_converging",
        first_verified_at: previous?.first_verified_at ?? null,
        last_checked_at: checkedAt,
        last_verified_at: previous?.last_verified_at ?? null,
        next_check_at: new Date(
          Date.parse(checkedAt) + VERIFICATION_RESOLVER_RETRY_MS,
        ).toISOString(),
        rrset_sha256: previous?.rrset_sha256 ?? null,
        dnssec_authenticated:
          previous?.dnssec_authenticated === true,
        minimum_ttl_seconds: previous?.minimum_ttl_seconds ?? null,
        consecutive_failures: previous?.consecutive_failures ?? 0,
      },
    };
    if (options.scheduled === true &&
        sameVerificationAuditOutcome(
          previous,
          desired.ownership_verification,
        )) {
      await this.commitVerificationRefresh(
        current,
        desired.ownership_verification,
        {
          previous_due: previousDue,
          expected_generation: options.expected_refresh_generation,
          extra_deletes: options.extra_deletes,
        },
      );
      return;
    }
    const entries = [
      [requestStorageKey(current.id), desired],
      [verificationDueKey(desired), desired.id],
    ];
    const deletes = [
      ...[previousDue].filter(Boolean),
      verificationRefreshKey(current.id),
      ...(Array.isArray(options.extra_deletes) ? options.extra_deletes : []),
    ];
    if (options.scheduled !== true ||
        !sameVerificationAuditOutcome(
          previous,
          desired.ownership_verification,
        )) {
      const mutation = await this.mutation(
        { kind: "system", id: "ownership-verifier" },
        "custom_domain.verification_deferred",
        current.domain,
        {
          account_id: current.account_id,
          request_id: current.id,
          state: desired.ownership_verification.state,
          reason: "account_policy_converging",
        },
        checkedAt,
      );
      entries.unshift(
        [META_KEY, mutation.meta],
        [mutation.audit_key, mutation.audit],
      );
    }
    const legacyMirror = await this.storage.get(
      domainStorageKey(current.domain),
    );
    if (assertLegacyDomainMirror(legacyMirror, current)) {
      entries.push([domainStorageKey(current.domain), desired]);
    } else if (desired.state === "verified") {
      if (!legacyMirror || legacyMirror.source_request_id !== current.id ||
          legacyMirror.state !== "allocated") {
        fail("custom inbound domain allocation is invalid", 503);
      }
      await this.appendCustomDomainRouteSourceIntent(
        entries,
        deletes,
        desired,
        legacyMirror,
      );
    }
    await this.atomic(entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
  }

  async reconcileDueGrace() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "plan-grace-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [due, requestID] of listed) {
      const dueAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(dueAt) || dueAt > now) break;
      const current = await this.storage.get(requestStorageKey(requestID));
      if (!current || graceDueKey(current) !== due ||
          current.state !== "verified") {
        await this.storage.delete(due);
        continue;
      }
      const mutation = await this.mutation(
        { kind: "system", id: "plan-lifecycle" },
        "custom_domain.plan_grace_expired",
        current.domain,
        { account_id: current.account_id, request_id: current.id },
      );
      const updated = {
        ...current,
        plan_suspended: true,
        plan_grace_until: null,
        state_revision: (current.state_revision ?? 1) + 1,
        updated_at: mutation.now,
      };
      const rawRefresh = await this.storage.get(
        verificationRefreshKey(current.id),
      );
      const refresh = effectiveVerificationRefresh(current, rawRefresh);
      const entries = [
        [META_KEY, mutation.meta],
        [mutation.audit_key, mutation.audit],
        [requestStorageKey(current.id), updated],
      ];
      const deletes = [
        due,
        verificationDueKey(current),
        effectiveVerificationDueKey(current, refresh),
        storedVerificationRefreshDue(rawRefresh, current.id),
        verificationWorkKey(current.id),
        verificationRefreshKey(current.id),
      ].filter(Boolean);
      const allocation = await this.storage.get(
        domainStorageKey(current.domain),
      );
      if (!allocation || allocation.source_request_id !== current.id ||
          allocation.state !== "allocated") {
        fail("custom inbound domain allocation is invalid", 503);
      }
      await this.appendCustomDomainRouteSourceIntent(
        entries,
        deletes,
        updated,
        allocation,
      );
      await this.atomic(entries, deletes);
    }
  }

  async expirePendingRequest(current, options = {}) {
    if (current.state !== "pending_verification") return false;
    const mutation = await this.mutation(
      { kind: "system", id: "challenge-expiry" },
      "custom_domain.expired",
      current.domain,
      { account_id: current.account_id, request_id: current.id },
    );
    const updated = {
      ...current,
      state: "expired",
      state_revision: (current.state_revision ?? 1) + 1,
      updated_at: mutation.now,
      expiration: {
        expired_at: mutation.now,
        reason: "ownership challenge expired",
      },
    };
    const usage = await this.accountUsage(current.account_id);
    if (usage.open_requests < 1 || usage.allocated_domains < 1) {
      fail("custom inbound domain account usage is invalid", 503);
    }
    const entries = [
      [META_KEY, mutation.meta],
      [mutation.audit_key, mutation.audit],
      [requestStorageKey(current.id), updated],
      [usageKey(current.account_id), {
        ...usage,
        open_requests: usage.open_requests - 1,
        allocated_domains: usage.allocated_domains - 1,
        updated_at: mutation.now,
      }],
    ];
    entries.push(...await this.capacityReleaseRebalanceEntries(current, usage));
    const legacyMirror = await this.storage.get(
      domainStorageKey(current.domain),
    );
    if (assertLegacyDomainMirror(legacyMirror, current)) {
      entries.push([domainStorageKey(current.domain), updated]);
    }
    const body = {
      schema_version: SCHEMA_VERSION,
      error: "custom inbound domain ownership challenge has expired",
      code: "ownership_challenge_expired",
      request: publicRequest(updated),
    };
    if (options.idempotency_scope && options.idempotency_key) {
      entries.push([idempotencyStorageKey(
        options.idempotency_scope,
        options.idempotency_key,
      ), {
        fingerprint: options.fingerprint,
        status: 409,
        body,
      }]);
    }
    const rawRefresh = await this.storage.get(
      verificationRefreshKey(current.id),
    );
    const refresh = effectiveVerificationRefresh(current, rawRefresh);
    await this.atomic(entries, [...new Set([
      accountDomainKey(current),
      domainPendingKey(current),
      challengeExpiryDueKey(current),
      verificationDueKey(current),
      effectiveVerificationDueKey(current, refresh),
      storedVerificationRefreshDue(rawRefresh, current.id),
      graceDueKey(current),
      verificationWorkKey(current.id),
      verificationRefreshKey(current.id),
      ...(Array.isArray(options.extra_deletes) ? options.extra_deletes : []),
    ].filter(Boolean))]);
    await this.scheduleNextAlarm().catch(() => {});
    return { body, status: 409, expired: true };
  }

  async reconcileDueChallengeExpiries() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "challenge-expiry-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [due, requestID] of listed) {
      const dueAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(dueAt) || dueAt > now) break;
      const current = await this.storage.get(requestStorageKey(requestID));
      if (!current || challengeExpiryDueKey(current) !== due ||
          current.state !== "pending_verification") {
        await this.storage.delete(due);
        continue;
      }
      await this.expirePendingRequest(current);
    }
  }

  assertCustomDomainRoutingEnabled(input = {}) {
    if (input.routing_enabled !== true ||
        !agentEmailCustomDomainRoutingEnabled(this.env)) {
      fail("custom domain routing is not enabled", 409,
        "custom_domain_routing_disabled");
    }
  }

  async customDomainRouteCellTarget(accountID) {
    const route = await this.env?.DIRECTORY?.get(`acct:${accountID}`, {
      type: "json",
    });
    if (!route?.cell) fail("custom domain target account is not routed", 409);
    const cell = await this.env.DIRECTORY.get(`cell:${route.cell}`, {
      type: "json",
    });
    if (!cell?.endpoint || !cell?.provision_token) {
      fail("custom domain target cell is not configured", 502);
    }
    let endpoint;
    try {
      endpoint = new URL(cell.endpoint);
    } catch {
      fail("custom domain target cell endpoint is invalid", 502);
    }
    if (endpoint.protocol !== "https:" || endpoint.username ||
        endpoint.password || endpoint.search || endpoint.hash ||
        !endpoint.hostname) {
      fail("custom domain target cell endpoint is invalid", 502);
    }
    const audience = typeof cell.agent_email_audience === "string" &&
        /^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/.test(
          cell.agent_email_audience,
        )
      ? cell.agent_email_audience
      : route.cell;
    if (!/^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/.test(audience ?? "")) {
      fail("custom domain target cell audience is invalid", 502);
    }
    const normalizedEndpoint = endpoint.toString().replace(/\/+$/, "");
    return {
      endpoint: normalizedEndpoint,
      provision_token: cell.provision_token,
      cell_audience: audience,
      ingest_url: `${normalizedEndpoint}/v1/internal/agent-email:ingest`,
    };
  }

  async realmAliasClaimProof(accountID, realmLabel) {
    const namespace = this.env?.REALM_EMAIL_ALIASES;
    if (!namespace || typeof namespace.idFromName !== "function" ||
        typeof namespace.get !== "function") {
      fail("realm email alias claim authority is unavailable", 503);
    }
    let response;
    try {
      const stub = namespace.get(namespace.idFromName(DEFAULT_REGISTRY_OBJECT_NAME));
      response = await stub.fetch(
        "https://realm-email-alias.internal/alias/claim-proof",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            account_id: accountID,
            realm_label: realmLabel,
          }),
        },
      );
    } catch {
      fail("realm email alias claim proof is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (response.status === 404) {
      fail("custom domain realm alias route not found", 404,
        "custom_domain_route_not_found");
    }
    if (!response.ok) {
      fail("realm email alias claim proof failed", 502);
    }
    try {
      return validateRealmEmailAliasClaimProof(body, accountID, realmLabel);
    } catch {
      fail("realm email alias claim proof is inconsistent", 502);
    }
  }

  async ensureCustomDomainRouteSubscriber(desired) {
    const namespace = this.env?.REALM_EMAIL_ALIASES;
    if (!namespace || typeof namespace.idFromName !== "function" ||
        typeof namespace.get !== "function") {
      fail("realm email alias claim authority is unavailable", 503);
    }
    let response;
    try {
      const stub = namespace.get(namespace.idFromName(DEFAULT_REGISTRY_OBJECT_NAME));
      response = await stub.fetch(
        "https://realm-email-alias.internal/alias/custom-domain-route-subscribe",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ claim_proof: desired.alias_proof }),
        },
      );
    } catch {
      fail("custom-domain alias subscription is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (response.status === 409 &&
        body?.code === "custom_domain_subscription_realm_closed" &&
        desired.edge_projection.state === "retired") {
      // A binding may have been journaled immediately before its first
      // subscription attempt while the realm concurrently closed. Since the
      // subscriber acknowledgement precedes every cell/KV write, this exact
      // never-subscribed retired binding has no external route to tombstone.
      return false;
    }
    if (!response.ok || body?.subscribed !== true ||
        body.account_id !== desired.account_id ||
        body.realm_id !== desired.alias_proof.realm_id ||
        body.realm_label !== desired.realm_label ||
        body.realm_alias_claim_id !==
          desired.alias_proof.realm_alias_claim_id) {
      fail("custom-domain alias subscription failed", 502);
    }
    return true;
  }

  customDomainRequestRoutePolicy(request, allocation) {
    if (allocation.state === "retired" || request.state === "retired") {
      if (allocation.state !== "retired" || request.state !== "retired") {
        fail("custom inbound domain retirement authority is inconsistent", 503);
      }
      return { state: "retired", suspension_disposition: null };
    }
    if (allocation.state !== "allocated" || request.state !== "verified") {
      fail("custom inbound domain allocation is invalid", 503);
    }
    if (request.lifecycle_suspended === true) {
      return { state: "suspended", suspension_disposition: "retry" };
    }
    if (request.plan_suspended === true) {
      return { state: "suspended", suspension_disposition: "inactive" };
    }
    if (request.ownership_verification?.state !== "verified") {
      return { state: "suspended", suspension_disposition: "retry" };
    }
    return { state: "applied", suspension_disposition: null };
  }

  combinedCustomDomainRoutePolicy(domainPolicy, aliasProof) {
    if (domainPolicy.state === "retired" || aliasProof.state === "retired") {
      return { state: "retired", suspension_disposition: null };
    }
    const dispositions = [
      domainPolicy.state === "suspended"
        ? domainPolicy.suspension_disposition
        : null,
      aliasProof.state === "suspended"
        ? aliasProof.suspension_disposition
        : null,
    ].filter(Boolean);
    if (dispositions.length > 0) {
      // An operational or lifecycle suspension always wins over a plan-only
      // inactive disposition so mail tempfails during convergence.
      return {
        state: "suspended",
        suspension_disposition: dispositions.includes("retry")
          ? "retry"
          : "inactive",
      };
    }
    return { state: "applied", suspension_disposition: null };
  }

  async desiredCustomDomainRoute(
    domain,
    realmLabel,
    updatedAt = this.now().toISOString(),
    options = {},
  ) {
    const allocation = await this.storage.get(domainStorageKey(domain));
    if (!allocation ||
        allocation.schema_version !==
          "witself.agent-email-domain-allocation.v1" ||
        allocation.domain !== domain ||
        !ACCOUNT_ID_PATTERN.test(allocation.account_id ?? "") ||
        !REQUEST_ID_PATTERN.test(allocation.source_request_id ?? "") ||
        !Number.isSafeInteger(allocation.allocation_revision) ||
        allocation.allocation_revision < 1 ||
        !["allocated", "retired"].includes(allocation.state)) {
      fail("custom domain realm alias route not found", 404,
        "custom_domain_route_not_found");
    }
    const request = await this.storage.get(
      requestStorageKey(allocation.source_request_id),
    );
    if (!request || request.schema_version !== SCHEMA_VERSION ||
        request.id !== allocation.source_request_id ||
        request.account_id !== allocation.account_id ||
        request.domain !== allocation.domain ||
        !Number.isSafeInteger(request.state_revision) ||
        request.state_revision < 1) {
      fail("custom inbound domain route authority is invalid", 503);
    }
    const routingEnabled = agentEmailCustomDomainRoutingEnabledForAccount(
      this.env,
      allocation.account_id,
    );
    if (!routingEnabled && options.allow_restrictive_when_disabled !== true) {
      fail("custom domain routing is not enabled for this account", 409,
        "custom_domain_routing_disabled");
    }
    const aliasProof = await this.realmAliasClaimProof(
      allocation.account_id,
      realmLabel,
    );
    const domainPolicy = this.customDomainRequestRoutePolicy(
      request,
      allocation,
    );
    const policy = this.combinedCustomDomainRoutePolicy(
      domainPolicy,
      aliasProof,
    );
    if (!routingEnabled && policy.state === "applied") {
      fail("custom domain routing is not enabled for this account", 409,
        "custom_domain_routing_disabled");
    }
    const controllerRevision = request.state_revision +
      allocation.allocation_revision + aliasProof.realm_alias_revision;
    if (!Number.isSafeInteger(controllerRevision) || controllerRevision < 1) {
      fail("custom domain route controller revision is exhausted", 503);
    }
    const target = await this.customDomainRouteCellTarget(
      allocation.account_id,
    );
    let cellProjection;
    let edgeProjection;
    try {
      cellProjection = buildAgentEmailCustomDomainCellProjection({
        account_id: allocation.account_id,
        domain: allocation.domain,
        realm_label: aliasProof.realm_label,
        realm_id: aliasProof.realm_id,
        domain_request_id: request.id,
        domain_allocation_revision: allocation.allocation_revision,
        domain_state_revision: request.state_revision,
        realm_alias_claim_id: aliasProof.realm_alias_claim_id,
        realm_alias_revision: aliasProof.realm_alias_revision,
        controller_revision: controllerRevision,
        state: policy.state,
        ...(policy.state === "suspended"
          ? { suspension_disposition: policy.suspension_disposition }
          : {}),
      });
      edgeProjection = buildAgentEmailCustomDomainRouteProjection({
        ...cellProjection,
        updated_at: updatedAt,
        ...(policy.state === "applied"
          ? {
            cell_audience: target.cell_audience,
            ingest_url: target.ingest_url,
          }
          : {}),
      });
    } catch {
      fail("custom inbound domain route projection is invalid", 503);
    }
    // Freshness is payload metadata, not authority. Keeping it out of the
    // source fence lets an explicit edge retry replay the exact pending bytes
    // after a lost acknowledgement instead of replacing a valid intent merely
    // because wall-clock time advanced.
    const edgeProjectionFence = { ...edgeProjection };
    delete edgeProjectionFence.updated_at;
    const sourceFingerprint = canonicalJSONString({
      cell_endpoint: target.endpoint,
      cell_projection: cellProjection,
      edge_projection: edgeProjectionFence,
    });
    return {
      account_id: allocation.account_id,
      domain_request_id: request.id,
      realm_label: realmLabel,
      cell_endpoint: target.endpoint,
      cell_projection: cellProjection,
      edge_projection: edgeProjection,
      alias_proof: aliasProof,
      source_fingerprint: sourceFingerprint,
      target,
    };
  }

  routeSourceFingerprint(request, allocation) {
    return canonicalJSONString({
      account_id: request.account_id,
      domain_request_id: request.id,
      domain: request.domain,
      domain_state_revision: request.state_revision,
      request_state: request.state,
      domain_allocation_revision: allocation.allocation_revision,
      allocation_state: allocation.state,
    });
  }

  async appendCustomDomainRouteSourceIntent(
    entries,
    deletes,
    request,
    allocation,
    options = {},
  ) {
    if (!request || !allocation ||
        request.account_id !== allocation.account_id ||
        request.id !== allocation.source_request_id ||
        request.domain !== allocation.domain ||
        !["verified", "retired"].includes(request.state) ||
        !["allocated", "retired"].includes(allocation.state)) return null;
    const bindings = await this.storage.list({
      prefix: routeBindingPrefix(request.id),
      limit: 1,
    });
    if (bindings.size === 0 && options.force_binding !== true) return null;
    const sourcePolicy = this.customDomainRequestRoutePolicy(request, allocation);
    const routingEnabled = agentEmailCustomDomainRoutingEnabledForAccount(
      this.env,
      request.account_id,
    );
    const now = this.now();
    const key = routeSourceIntentKey(request.id);
    const previous = await this.storage.get(key);
    if (!routingEnabled && sourcePolicy.state === "applied") {
      if (previous) {
        deletes.push(key);
        deletes.push(routeSourceAccountKey(previous));
        const previousDue = routeSourceDueKey(previous);
        if (previousDue) deletes.push(previousDue);
      }
      return null;
    }
    const sourceFingerprint = this.routeSourceFingerprint(request, allocation);
    if (previous?.source_fingerprint === sourceFingerprint &&
        options.force_binding !== true) return previous;
    const intent = {
      schema_version: ROUTE_SOURCE_INTENT_SCHEMA,
      account_id: request.account_id,
      domain_request_id: request.id,
      domain: request.domain,
      domain_state_revision: request.state_revision,
      request_state: request.state,
      domain_allocation_revision: allocation.allocation_revision,
      allocation_state: allocation.state,
      source_fingerprint: sourceFingerprint,
      binding_cursor: null,
      allow_restrictive_when_disabled: sourcePolicy.state !== "applied",
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: previous?.created_at ?? now.toISOString(),
      updated_at: now.toISOString(),
    };
    entries.push(
      [key, intent],
      [routeSourceAccountKey(intent), key],
      [routeSourceDueKey(intent), request.id],
    );
    const previousDue = routeSourceDueKey(previous);
    if (previousDue && previousDue !== routeSourceDueKey(intent)) {
      deletes.push(previousDue);
    }
    return intent;
  }

  async ensureCustomDomainRouteBinding(desired) {
    const binding = {
      schema_version: ROUTE_BINDING_SCHEMA,
      account_id: desired.account_id,
      domain_request_id: desired.domain_request_id,
      domain: desired.edge_projection.domain,
      realm_id: desired.edge_projection.realm_id,
      realm_label: desired.edge_projection.realm_label,
      realm_alias_claim_id: desired.edge_projection.realm_alias_claim_id,
      created_at: this.now().toISOString(),
    };
    const key = routeBindingKey(
      binding.domain_request_id,
      binding.realm_alias_claim_id,
    );
    const existing = await this.storage.get(key);
    if (existing) {
      const comparable = (value) => canonicalJSONString({
        schema_version: value?.schema_version,
        account_id: value?.account_id,
        domain_request_id: value?.domain_request_id,
        domain: value?.domain,
        realm_id: value?.realm_id,
        realm_label: value?.realm_label,
        realm_alias_claim_id: value?.realm_alias_claim_id,
      });
      if (comparable(existing) !== comparable(binding)) {
        fail("custom domain route binding conflicts", 409,
          "custom_domain_route_binding_conflict");
      }
      // Repairable indexes remain derived and may be absent after recovery.
      await this.atomicRaw([
        [routeBindingAliasKey(existing), key],
      ]);
      return existing;
    }
    if (desired.edge_projection.state === "retired") {
      fail("custom domain realm alias route not found", 404,
        "custom_domain_route_not_found");
    }
    // This sparse membership row is the durable fact that a cell route may
    // exist. It is journaled before the first cell/KV write; recovery can then
    // rebuild both bounded discovery indexes without inventing the full
    // domain-by-alias cross-product.
    const request = await this.storage.get(
      requestStorageKey(binding.domain_request_id),
    );
    const allocation = await this.storage.get(domainStorageKey(binding.domain));
    if (!request || !allocation ||
        request.state_revision !== desired.cell_projection.domain_state_revision ||
        allocation.allocation_revision !==
          desired.cell_projection.domain_allocation_revision) {
      fail("custom domain route binding authority changed", 409,
        "custom_domain_route_stale");
    }
    const entries = [
      [key, binding],
      [routeBindingAliasKey(binding), key],
    ];
    const deletes = [];
    await this.appendCustomDomainRouteSourceIntent(
      entries,
      deletes,
      request,
      allocation,
      { force_binding: true },
    );
    await this.atomic(entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return binding;
  }

  async persistCustomDomainRouteIntent(desired, previous = null) {
    const now = this.now();
    const intent = {
      schema_version: AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_INTENT_SCHEMA_VERSION,
      phase: "pending",
      account_id: desired.account_id,
      domain_request_id: desired.domain_request_id,
      domain: desired.edge_projection.domain,
      realm_label: desired.realm_label,
      cell_endpoint: desired.cell_endpoint,
      cell_projection: structuredClone(desired.cell_projection),
      edge_projection: structuredClone(desired.edge_projection),
      source_fingerprint: desired.source_fingerprint,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: previous?.created_at ?? now.toISOString(),
      updated_at: now.toISOString(),
      completed_at: null,
    };
    const key = routeProjectionIntentKey(
      intent.domain_request_id,
      intent.realm_label,
    );
    const due = routeProjectionDueKey(intent);
    await this.atomicRaw([
      [key, intent],
      [due, key],
    ], [routeProjectionDueKey(previous)].filter((value) => value && value !== due));
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  async stageCustomDomainRouteIntent(domain, realmLabel, options = {}) {
    const desired = await this.desiredCustomDomainRoute(
      domain,
      realmLabel,
      this.now().toISOString(),
      options,
    );
    await this.ensureCustomDomainRouteBinding(desired);
    // The permanent sparse subscriber marker closes the cross-authority
    // discovery gap. It is acknowledged before any cell or edge route write,
    // so later alias mutations can enqueue only claims with real bindings.
    const subscribed = await this.ensureCustomDomainRouteSubscriber(desired);
    if (!subscribed) {
      fail("custom-domain alias binding retired before subscription", 409,
        "custom_domain_route_never_subscribed");
    }
    const key = routeProjectionIntentKey(
      desired.domain_request_id,
      realmLabel,
    );
    const existing = await this.storage.get(key);
    if (existing?.phase === "pending" &&
        existing.source_fingerprint === desired.source_fingerprint) {
      return existing;
    }
    return this.persistCustomDomainRouteIntent(desired, existing);
  }

  async applyAndReadBackCustomDomainRoute(intent, target) {
    const url = cellAgentEmailCustomDomainRouteURL(
      target.endpoint,
      intent.account_id,
    );
    let response;
    try {
      response = await this.fetchImpl(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${target.provision_token}`,
        },
        body: JSON.stringify(intent.cell_projection),
        signal: AbortSignal.timeout(15_000),
      });
    } catch {
      fail("custom domain target cell is unreachable", 502);
    }
    const acknowledgement = await response.json().catch(() => null);
    if (!response.ok) {
      fail(
        response.status === 409
          ? "cell rejected a stale or conflicting custom domain route"
          : response.status === 404
          ? "cell custom domain route target no longer exists"
          : "cell rejected custom domain route projection",
        response.status === 409 ? 409 : 502,
      );
    }
    try {
      validateAgentEmailCustomDomainCellProjection(
        acknowledgement,
        intent.cell_projection,
      );
    } catch {
      fail("cell returned an inconsistent custom domain route acknowledgement",
        502);
    }

    let readback;
    try {
      const verified = await this.fetchImpl(
        cellAgentEmailCustomDomainRouteURL(
          target.endpoint,
          intent.account_id,
          intent.cell_projection.domain_request_id,
          intent.cell_projection.realm_alias_claim_id,
        ),
        {
          method: "GET",
          headers: { Authorization: `Bearer ${target.provision_token}` },
          signal: AbortSignal.timeout(15_000),
        },
      );
      readback = await verified.json().catch(() => null);
      if (!verified.ok) {
        fail("cell custom domain route fence verification failed", 502);
      }
    } catch (error) {
      if (error instanceof DomainRegistryError) throw error;
      fail("cell custom domain route fence verification failed", 502);
    }
    try {
      validateAgentEmailCustomDomainCellProjection(
        readback,
        intent.cell_projection,
      );
    } catch {
      fail("cell custom domain route readback is inconsistent", 502);
    }
    return target;
  }

  async publishCustomDomainRoute(projection) {
    if (!this.env?.AGENT_EMAIL_DIRECTORY ||
        typeof this.env.AGENT_EMAIL_DIRECTORY.put !== "function") {
      fail("isolated agent email routing directory is unavailable", 503);
    }
    try {
      const signed = await this.signedRouteProjection(projection);
      await this.env.AGENT_EMAIL_DIRECTORY.put(
        agentEmailCustomDomainRouteKey(
          projection.domain,
          projection.realm_label,
        ),
        JSON.stringify(signed),
      );
      return signed;
    } catch (error) {
      if (error instanceof DomainRegistryError) throw error;
      fail("custom domain routing projection failed", 502);
    }
  }

  async signedRouteProjection(projection) {
    try {
      return await this.signRouteProjection(projection);
    } catch (error) {
      if (error instanceof DomainRegistryError) throw error;
      fail(
        "agent email route signing is unavailable",
        503,
        "agent_email_route_signing_unavailable",
      );
    }
  }

  async drainCustomDomainRouteIntent(startingIntent) {
    let intent = startingIntent;
    const key = routeProjectionIntentKey(
      intent.domain_request_id,
      intent.realm_label,
    );
    try {
      const convergenceOptions = {
        allow_restrictive_when_disabled:
          intent.edge_projection?.state !== "applied",
      };
      if (!convergenceOptions.allow_restrictive_when_disabled) {
        this.assertCustomDomainRoutingEnabled({ routing_enabled: true });
      }
      // A retry may wake after either authority changed. Replace the derived
      // intent before touching the cell; it is never allowed to outvote the
      // domain allocation or alias claim that produced it.
      const before = await this.desiredCustomDomainRoute(
        intent.domain,
        intent.realm_label,
        intent.edge_projection.updated_at,
        convergenceOptions,
      );
      if (before.source_fingerprint !== intent.source_fingerprint) {
        intent = await this.persistCustomDomainRouteIntent(before, intent);
      }
      let target = await this.customDomainRouteCellTarget(intent.account_id);
      if (target.endpoint !== intent.cell_endpoint) {
        const moved = await this.desiredCustomDomainRoute(
          intent.domain,
          intent.realm_label,
          this.now().toISOString(),
          convergenceOptions,
        );
        intent = await this.persistCustomDomainRouteIntent(moved, intent);
        target = moved.target;
      }
      await this.applyAndReadBackCustomDomainRoute(intent, target);

      // Re-prove the one external authority after exact cell readback. A stale
      // alias revision or any changed domain/lifecycle fence blocks KV
      // publication even when the cell accepted an earlier valid projection.
      const after = await this.desiredCustomDomainRoute(
        intent.domain,
        intent.realm_label,
        intent.edge_projection.updated_at,
        convergenceOptions,
      );
      if (after.source_fingerprint !== intent.source_fingerprint) {
        await this.persistCustomDomainRouteIntent(after, intent);
        fail("custom domain route authority changed during projection", 409,
          "custom_domain_route_stale");
      }
      const signedProjection = await this.publishCustomDomainRoute(
        intent.edge_projection,
      );
      const completedAt = this.now().toISOString();
      const completed = {
        ...intent,
        phase: "complete",
        failure_count: 0,
        retry_at_ms: null,
        updated_at: completedAt,
        completed_at: completedAt,
      };
      await this.atomicRaw([[key, completed]], [routeProjectionDueKey(intent)]);
      await this.scheduleNextAlarm().catch(() => {});
      return json(signedProjection);
    } catch (error) {
      const current = await this.storage.get(key);
      if (error instanceof DomainRegistryError &&
          error.code === "custom_domain_routing_disabled" &&
          !agentEmailCustomDomainRoutingEnabledForAccount(
            this.env,
            intent.account_id,
          )) {
        await this.atomicRaw([], [routeProjectionDueKey(current)].filter(Boolean));
        throw error;
      }
      if (current?.phase === "pending" &&
          current.source_fingerprint === intent.source_fingerprint) {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() +
            routeProjectionRetryDelay(failureCount),
          updated_at: this.now().toISOString(),
          completed_at: null,
        };
        await this.atomicRaw([
          [key, retry],
          [routeProjectionDueKey(retry), key],
        ], [routeProjectionDueKey(current)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
      }
      throw error;
    }
  }

  async getCustomDomainRoute(input) {
    this.assertCustomDomainRoutingEnabled(input);
    const domain = normalizeAgentEmailCustomDomain(input?.domain);
    const realmLabel = typeof input?.realm_label === "string"
      ? input.realm_label
      : "";
    try {
      agentEmailCustomDomainRouteKey(domain, realmLabel);
    } catch {
      fail("custom domain realm alias route not found", 404,
        "custom_domain_route_not_found");
    }
    try {
      const intent = await this.stageCustomDomainRouteIntent(domain, realmLabel);
      return this.drainCustomDomainRouteIntent(intent);
    } catch (error) {
      if (error instanceof DomainRegistryError &&
          error.code === "custom_domain_route_never_subscribed") {
        fail("custom domain realm alias route not found", 404,
          "custom_domain_route_not_found");
      }
      throw error;
    }
  }

  async reconcileDueRouteProjections() {
    const listed = await this.storage.list({
      prefix: "route-projection-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    const now = this.now().getTime();
    for (const [due, intentKey] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(due);
        continue;
      }
      if (retryAt > now) break;
      const intent = typeof intentKey === "string"
        ? await this.storage.get(intentKey)
        : null;
      if (!intent || routeProjectionDueKey(intent) !== due) {
        await this.storage.delete(due);
        continue;
      }
      if (!agentEmailCustomDomainRoutingEnabledForAccount(
        this.env,
        intent.account_id,
      ) && intent.edge_projection?.state === "applied") {
        // Removing the runtime gate stops all external I/O immediately. The
        // bounded applied intent remains available for an explicit future
        // route miss. Restrictive/tombstone intents below must still drain for
        // already-materialized bindings so lifecycle cannot strand a cell.
        await this.atomicRaw([], [due]);
        continue;
      }
      await this.drainCustomDomainRouteIntent(intent).catch(() => {});
    }
  }

  async customDomainRouteBindingPage(prefix, cursor = null) {
    const listed = await this.storage.list({
      prefix,
      limit: ROUTE_CONVERGENCE_PAGE_LIMIT + 1,
      ...(cursor ? { startAfter: cursor } : {}),
    });
    const rows = [...listed.entries()];
    const page = rows.slice(0, ROUTE_CONVERGENCE_PAGE_LIMIT);
    const bindings = [];
    for (const [indexKey, indexedValue] of page) {
      const bindingKey = typeof indexedValue === "string"
        ? indexedValue
        : indexKey;
      const binding = typeof indexedValue === "string"
        ? await this.storage.get(bindingKey)
        : indexedValue;
      if (!binding || binding.schema_version !== ROUTE_BINDING_SCHEMA ||
          routeBindingKey(
            binding.domain_request_id,
            binding.realm_alias_claim_id,
          ) !== bindingKey) {
        fail("custom domain route binding index is invalid", 503);
      }
      bindings.push({ index_key: indexKey, binding });
    }
    return {
      bindings,
      next_cursor: rows.length > ROUTE_CONVERGENCE_PAGE_LIMIT && page.length > 0
        ? page.at(-1)[0]
        : null,
    };
  }

  async drainCustomDomainRouteSourceIntent(startingIntent) {
    let intent = startingIntent;
    const key = routeSourceIntentKey(intent.domain_request_id);
    try {
      const request = await this.storage.get(
        requestStorageKey(intent.domain_request_id),
      );
      const allocation = request
        ? await this.storage.get(domainStorageKey(request.domain))
        : null;
      if (!request || !allocation ||
          allocation.source_request_id !== request.id) {
        fail("custom domain route source authority is invalid", 503);
      }
      const currentFingerprint = this.routeSourceFingerprint(request, allocation);
      if (currentFingerprint !== intent.source_fingerprint) {
        const entries = [];
        const deletes = [];
        const replaced = await this.appendCustomDomainRouteSourceIntent(
          entries,
          deletes,
          request,
          allocation,
        );
        if (!replaced) {
          // The source became applied while routing was disabled. The helper
          // deliberately staged deletion of the obsolete restrictive
          // obligation; commit that cleanup instead of resurrecting it in the
          // retry path.
          await this.atomic(entries, [
            routeSourceDueKey(intent),
            ...deletes,
          ].filter(Boolean));
          await this.scheduleNextAlarm().catch(() => {});
          return { complete: true, changed: 0, gated: true };
        }
        if (replaced.source_fingerprint === intent.source_fingerprint) {
          fail("custom domain route source convergence is stale", 409,
            "custom_domain_route_stale");
        }
        await this.atomic(entries, [routeSourceDueKey(intent), ...deletes].filter(Boolean));
        intent = replaced;
      }
      const routingEnabled = agentEmailCustomDomainRoutingEnabledForAccount(
        this.env,
        intent.account_id,
      );
      if (!routingEnabled && intent.allow_restrictive_when_disabled !== true) {
        // Applied work must not keep a parent plan/lifecycle fence open after
        // the runtime gate is removed. No binding is made less restrictive by
        // dropping this obligation, and no external write is attempted.
        await this.atomic([], [
          key,
          routeSourceAccountKey(intent),
          routeSourceDueKey(intent),
        ].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return { complete: true, changed: 0, gated: true };
      }
      const page = await this.customDomainRouteBindingPage(
        routeBindingPrefix(intent.domain_request_id),
        intent.binding_cursor,
      );
      for (const { binding } of page.bindings) {
        if (binding.account_id !== intent.account_id ||
            binding.domain_request_id !== intent.domain_request_id ||
            binding.domain !== intent.domain) {
          fail("custom domain route source binding is inconsistent", 503);
        }
        try {
          const leaf = await this.stageCustomDomainRouteIntent(
            binding.domain,
            binding.realm_label,
            {
              allow_restrictive_when_disabled:
                intent.allow_restrictive_when_disabled === true,
            },
          );
          await this.drainCustomDomainRouteIntent(leaf);
        } catch (error) {
          if (!(error instanceof DomainRegistryError) ||
              error.code !== "custom_domain_route_never_subscribed") {
            throw error;
          }
          // Subscription acknowledgement is ordered before the first cell/KV
          // write. A realm-closed binding that never crossed that boundary has
          // no external projection and is already terminal without I/O.
        }
      }
      if (page.next_cursor === null) {
        await this.atomic([], [
          key,
          routeSourceAccountKey(intent),
          routeSourceDueKey(intent),
        ].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return { complete: true, changed: page.bindings.length };
      }
      const continued = {
        ...intent,
        binding_cursor: page.next_cursor,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.atomic([
        [key, continued],
        [routeSourceDueKey(continued), intent.domain_request_id],
      ], [routeSourceDueKey(intent)].filter((value) =>
        value && value !== routeSourceDueKey(continued)
      ));
      await this.scheduleNextAlarm().catch(() => {});
      return { complete: false, changed: page.bindings.length };
    } catch (error) {
      const current = await this.storage.get(key);
      if (current?.source_fingerprint === intent.source_fingerprint) {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() +
            routeConvergenceRetryDelay(failureCount),
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [key, retry],
          [routeSourceDueKey(retry), retry.domain_request_id],
        ], [routeSourceDueKey(current)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
      }
      throw error;
    }
  }

  async reconcileDueRouteSources() {
    const listed = await this.storage.list({
      prefix: "route-source-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    const now = this.now().getTime();
    for (const [due, requestID] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || retryAt > now) break;
      const intent = typeof requestID === "string"
        ? await this.storage.get(routeSourceIntentKey(requestID))
        : null;
      if (!intent || routeSourceDueKey(intent) !== due) {
        await this.storage.delete(due);
        continue;
      }
      await this.drainCustomDomainRouteSourceIntent(intent).catch(() => {});
    }
  }

  customDomainAliasConvergenceInput(input) {
    let proof;
    try {
      proof = validateRealmEmailAliasClaimProof(
        input?.claim_proof,
        input?.claim_proof?.account_id,
        input?.claim_proof?.realm_label,
      );
    } catch {
      fail("custom-domain alias convergence proof is invalid", 400);
    }
    const sourceFingerprint = realmEmailAliasClaimRouteFingerprint(proof);
    if (input?.source_fingerprint !== sourceFingerprint) {
      fail("custom-domain alias convergence fingerprint is invalid", 400);
    }
    return { proof, source_fingerprint: sourceFingerprint };
  }

  async enqueueCustomDomainAliasConvergence(input) {
    const { proof, source_fingerprint: sourceFingerprint } =
      this.customDomainAliasConvergenceInput(input);
    const bindingPrefix = routeBindingAliasPrefix(
      proof.account_id,
      proof.realm_id,
      proof.realm_alias_claim_id,
    );
    const bindings = await this.storage.list({ prefix: bindingPrefix, limit: 1 });
    if (bindings.size === 0) {
      return json({
        schema_version: SCHEMA_VERSION,
        complete: true,
        source_fingerprint: sourceFingerprint,
      });
    }
    const key = routeAliasTaskKey(proof.account_id, proof.realm_label);
    const previous = await this.storage.get(key);
    if (previous?.source_fingerprint === sourceFingerprint) {
      return json({
        schema_version: SCHEMA_VERSION,
        complete: previous.phase === "complete",
        source_fingerprint: sourceFingerprint,
      }, previous.phase === "complete" ? 200 : 202);
    }
    if (previous &&
        (previous.claim_proof?.realm_alias_claim_id !==
            proof.realm_alias_claim_id ||
          previous.claim_proof?.realm_alias_revision >
            proof.realm_alias_revision)) {
      fail("custom-domain alias convergence request is stale", 409,
        "custom_domain_route_stale");
    }
    const now = this.now();
    const task = {
      schema_version: ROUTE_ALIAS_TASK_SCHEMA,
      phase: "pending",
      account_id: proof.account_id,
      realm_id: proof.realm_id,
      realm_label: proof.realm_label,
      claim_proof: proof,
      source_fingerprint: sourceFingerprint,
      binding_cursor: null,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: previous?.created_at ?? now.toISOString(),
      updated_at: now.toISOString(),
      completed_at: null,
    };
    await this.atomicRaw([
      [key, task],
      [routeAliasDueKey(task), key],
    ], [routeAliasDueKey(previous)].filter(Boolean));
    await this.scheduleNextAlarm().catch(() => {});
    return json({
      schema_version: SCHEMA_VERSION,
      complete: false,
      source_fingerprint: sourceFingerprint,
    }, 202);
  }

  async customDomainAliasConvergenceStatus(input) {
    const { proof, source_fingerprint: sourceFingerprint } =
      this.customDomainAliasConvergenceInput(input);
    const task = await this.storage.get(
      routeAliasTaskKey(proof.account_id, proof.realm_label),
    );
    if (!task) {
      fail("custom-domain alias convergence task not found", 404,
        "custom_domain_route_convergence_not_found");
    }
    if (task.source_fingerprint !== sourceFingerprint) {
      fail("custom-domain alias convergence request is stale", 409,
        "custom_domain_route_stale");
    }
    return json({
      schema_version: SCHEMA_VERSION,
      complete: task.phase === "complete",
      source_fingerprint: sourceFingerprint,
    }, task.phase === "complete" ? 200 : 202);
  }

  async drainCustomDomainRouteAliasTask(startingTask) {
    let task = startingTask;
    const key = routeAliasTaskKey(task.account_id, task.realm_label);
    try {
      const currentProof = await this.realmAliasClaimProof(
        task.account_id,
        task.realm_label,
      );
      const currentFingerprint = realmEmailAliasClaimRouteFingerprint(
        currentProof,
      );
      if (currentFingerprint !== task.source_fingerprint) {
        const now = this.now();
        task = {
          ...task,
          phase: "pending",
          realm_id: currentProof.realm_id,
          claim_proof: currentProof,
          source_fingerprint: currentFingerprint,
          binding_cursor: null,
          failure_count: 0,
          retry_at_ms: now.getTime(),
          updated_at: now.toISOString(),
          completed_at: null,
        };
        await this.atomicRaw([
          [key, task],
          [routeAliasDueKey(task), key],
        ], [routeAliasDueKey(startingTask)].filter(Boolean));
      }
      const prefix = routeBindingAliasPrefix(
        task.account_id,
        task.realm_id,
        task.claim_proof.realm_alias_claim_id,
      );
      const page = await this.customDomainRouteBindingPage(
        prefix,
        task.binding_cursor,
      );
      for (const { binding } of page.bindings) {
        if (binding.account_id !== task.account_id ||
            binding.realm_id !== task.realm_id ||
            binding.realm_label !== task.realm_label ||
            binding.realm_alias_claim_id !==
              task.claim_proof.realm_alias_claim_id) {
          fail("custom-domain alias route binding is inconsistent", 503);
        }
        try {
          const leaf = await this.stageCustomDomainRouteIntent(
            binding.domain,
            binding.realm_label,
            { allow_restrictive_when_disabled: true },
          );
          await this.drainCustomDomainRouteIntent(leaf);
        } catch (error) {
          if (!(error instanceof DomainRegistryError) ||
              error.code !== "custom_domain_routing_disabled" ||
              agentEmailCustomDomainRoutingEnabledForAccount(
                this.env,
                task.account_id,
              )) {
            throw error;
          }
          // With activation removed, an applied leaf requires no external
          // write. Restrictive/retired siblings are still accepted by desired
          // route derivation above and must converge before this task closes.
        }
      }
      if (page.next_cursor !== null) {
        const continued = {
          ...task,
          binding_cursor: page.next_cursor,
          failure_count: 0,
          retry_at_ms: this.now().getTime() + ROUTE_CONVERGENCE_RETRY_MS,
          updated_at: this.now().toISOString(),
        };
        await this.atomicRaw([
          [key, continued],
          [routeAliasDueKey(continued), key],
        ], [routeAliasDueKey(task)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return { complete: false, changed: page.bindings.length };
      }
      const completedAt = this.now().toISOString();
      const completed = {
        ...task,
        phase: "complete",
        binding_cursor: null,
        failure_count: 0,
        retry_at_ms: null,
        updated_at: completedAt,
        completed_at: completedAt,
      };
      await this.atomicRaw([[key, completed]], [routeAliasDueKey(task)]
        .filter(Boolean));
      await this.scheduleNextAlarm().catch(() => {});
      return { complete: true, changed: page.bindings.length };
    } catch (error) {
      const current = await this.storage.get(key);
      if (current?.phase === "pending") {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() +
            routeConvergenceRetryDelay(failureCount),
          updated_at: this.now().toISOString(),
          completed_at: null,
        };
        await this.atomicRaw([
          [key, retry],
          [routeAliasDueKey(retry), key],
        ], [routeAliasDueKey(current)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
      }
      throw error;
    }
  }

  async reconcileDueRouteAliases() {
    const listed = await this.storage.list({
      prefix: "route-alias-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    const now = this.now().getTime();
    for (const [due, taskKey] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || retryAt > now) break;
      const task = typeof taskKey === "string"
        ? await this.storage.get(taskKey)
        : null;
      if (!task || routeAliasDueKey(task) !== due) {
        await this.storage.delete(due);
        continue;
      }
      await this.drainCustomDomainRouteAliasTask(task).catch(() => {});
    }
  }

  async listAudit(input) {
    validateActor(input?.actor, "platform_admin");
    let accountID = null;
    let domain = null;
    let action = null;
    if (input?.account_id != null) {
      accountID = validateAccountID(input.account_id);
    }
    if (input?.domain != null) {
      domain = normalizeAgentEmailCustomDomain(input.domain);
    }
    if (input?.action != null) {
      if (typeof input.action !== "string" || !new Set([
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
      ]).has(input.action)) {
        fail("audit action filter is invalid", 400);
      }
      action = input.action;
    }
    const listed = await this.boundedEntries("audit:", input, true);
    let events = listed.entries.map(([, event]) => publicAudit(event));
    if (accountID) {
      events = events.filter((event) =>
        event.metadata.account_id === accountID
      );
    }
    if (domain) {
      events = events.filter((event) => event.target === domain);
    }
    if (action) {
      events = events.filter((event) => event.action === action);
    }
    return json({
      schema_version: SCHEMA_VERSION,
      events,
      truncated: listed.truncated,
      next_cursor: listed.next_cursor,
    });
  }
}
