import {
  CELL_REALM_ROUTE_PAGE_LIMIT,
  cellRealmRouteCommitURL,
  cellRealmRouteGetURL,
  cellRealmRouteListURL,
  cellRealmRoutePrepareURL,
} from "./realm-email-canonical-contract.mjs";
import {
  RealmEmailAliasJournalRuntime,
  RealmEmailAliasJournalRuntimeError,
} from "./realm-email-alias-journal-runtime.mjs";
import {
  buildRealmEmailAliasClaimProof,
  realmEmailAliasClaimRouteFingerprint,
  validateRealmEmailAliasClaimProof,
} from "./agent-email-custom-domain-route-contract.mjs";
import {
  signAgentEmailRouteProjection,
} from "./agent-email-route-signature.mjs";
import {
  managedDeliveryAccountIsAdmitted,
} from "./agent-email-managed-delivery-cohort.mjs";
import {
  AgentEmailOperationsLeaseRuntime,
  agentEmailOperationsLeaseErrorResponse,
  agentEmailOperationsLeaseJSON,
  isAgentEmailOperationsLeasePath,
} from "./agent-email-operations-lease.mjs";

const SCHEMA_VERSION = "witself.realm-email-alias.v1";
const META_KEY = "meta";
const DEFAULT_REGISTRY_OBJECT_NAME = "global";
const ALIAS_PATTERN = /^[a-z0-9](?:[a-z0-9-]{1,14}[a-z0-9])$/;
const CANONICAL_REALM_LABEL_PATTERN = /^[a-z2-7]{16}$/;
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const MANAGED_ROUTE_ACCOUNT_ID_PATTERN = /^acc_[a-z2-7]{16}$/;
const REALM_ID_PATTERN = /^realm_[a-z2-7]{16}$/;
const REQUEST_ID_PATTERN = /^earq_[a-z2-7]{16}$/;
const CLAIM_ID_PATTERN = /^era_[a-z2-7]{16}$/;
const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const CATEGORY_PATTERN = /^[a-z][a-z0-9_]{1,63}$/;
const DOMAIN_LABEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const CELL_NAME_PATTERN = /^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/;

export const REALM_EMAIL_ALIAS_FEATURE = "agent_email_realm_alias";
export const REALM_EMAIL_ALIAS_LIMIT =
  "agent_email_realm_aliases_per_realm";
export const REALM_EMAIL_ROUTE_PREFIX = "email:realm-route:v1:";
export const MANAGED_REALM_EMAIL_ROUTE_SCHEMA_VERSION = 2;
export const REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS = 300;
export const REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_DAYS = 30;
const REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_MS =
  REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_DAYS * 24 * 60 * 60 * 1_000;
const REALM_EMAIL_ALIAS_RETRY_MS = 5 * 60 * 1_000;
const REALM_EMAIL_ALIAS_MAX_RETRY_MS = 60 * 60 * 1_000;
const REGISTRY_LIST_LIMIT = 500;
const ALARM_BATCH_LIMIT = 100;
const PLAN_RECONCILE_CLAIM_LIMIT = 100;
// Account moves and archives are latency-sensitive control-plane operations.
// Each changed claim performs a cell apply, an exact fence read, and an edge
// projection write, so lifecycle pages stay deliberately small.
const LIFECYCLE_RECONCILE_CLAIM_LIMIT = 10;
const PLAN_RECONCILE_PAGE_RETRY_MS = 1_000;
const LIFECYCLE_RECONCILE_PAGE_RETRY_MS = 1_000;
const ROUTE_REFRESH_RETRY_MS = 1_000;
const PENDING_COUNTER_SCHEMA_VERSION = 1;
const PENDING_COUNTER_MIGRATION_KEY = "pending-counter-migration";
const PENDING_COUNTER_MIGRATION_PAGE_LIMIT = 100;
const PENDING_COUNTER_MIGRATION_RETRY_MS = 5_000;
const PLAN_FIT_AUTHORITY_PAGE_LIMIT = 500;
const PLAN_FIT_AUTHORITY_SCAN_LIMIT = 10_000;
const PENDING_COUNTER_DERIVED_PREFIXES = Object.freeze([
  "claim-usage-member:",
  "claim-usage-account-member:",
  "claim-usage-realm-member:",
  "claim-usage-account:",
  "claim-usage-realm:",
]);
const AGENT_EMAIL_RECEIVE_FEATURE = "agent_email_receive";
const CANONICAL_INVENTORY_SCHEMA = "witself.realm-email-canonical-inventory.v1";
const CANONICAL_INVENTORY_KEY = "canonical-inventory";
const CANONICAL_INVENTORY_DIRECTORY_LIMIT = 1;
const REALM_CLOSE_CLAIM_PAGE_LIMIT = 25;
const REALM_CLOSE_ALARM_BATCH_LIMIT = 10;
const CUSTOM_DOMAIN_SYNC_SCHEMA =
  "witself.realm-email-alias-custom-domain-sync.v1";
const CUSTOM_DOMAIN_SUBSCRIPTION_SCHEMA =
  "witself.realm-email-alias-custom-domain-subscription.v1";
const CUSTOM_DOMAIN_SYNC_RETRY_MS = 1_000;
export const REALM_EMAIL_MAX_MANAGED_DOMAINS = 2;

// Commercial alias capacity can be explicitly unlimited. Review work cannot:
// these independent technical ceilings bound both one realm's queue and one
// account's aggregate footprint on the globally serialized registry. Runtime
// configuration may lower, but never raise, the compiled safety ceilings.
export const REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM = 8;
export const REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT = 64;

export function realmEmailCanonicalInventoryEnabled(env = {}) {
  return String(env.CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED ?? "") ===
    "true";
}

export function realmEmailCanonicalDeliveryEnabled(env = {}) {
  return String(env.CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED ?? "") ===
    "true";
}

// These names are seeded into durable, administrator-managed state. They are
// not a hardcoded-only policy: platform administrators can version, disable,
// re-enable, or add reservations through the registry API. Seeded operational
// names are internal-assignable so Witself can deliberately use them through
// the privileged path without making them available to customer requests.
export const INITIAL_RESERVED_REALM_EMAIL_ALIASES = Object.freeze([
  ["witself", "platform_brand"],
  ["witwave", "platform_brand"],
  ["witmail", "platform_brand"],
  ["witpass", "platform_brand"],
  ["email", "operational_role"],
  ["mail", "operational_role"],
  ["agent", "operational_role"],
  ["agents", "operational_role"],
  ["admin", "operational_role"],
  ["administrator", "operational_role"],
  ["root", "operational_role"],
  ["system", "operational_role"],
  ["security", "operational_role"],
  ["trust", "operational_role"],
  ["abuse", "operational_role"],
  ["postmaster", "operational_role"],
  ["hostmaster", "operational_role"],
  ["webmaster", "operational_role"],
  ["noc", "operational_role"],
  ["mailer-daemon", "operational_role"],
  ["noreply", "operational_role"],
  ["smtp", "infrastructure"],
  ["imap", "infrastructure"],
  ["pop", "infrastructure"],
  ["mx-record", "infrastructure"],
  ["support", "operational_role"],
  ["help", "operational_role"],
  ["billing", "operational_role"],
  ["status", "operational_role"],
]);

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(message, status, code = "", details = {}) {
  return json({
    schema_version: SCHEMA_VERSION,
    error: message,
    ...(code ? { code } : {}),
    ...details,
  }, status);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

function validRealmEmailAliasPrepareFit(
  value,
  expectedMaximum,
  requireAuthorityRevision = true,
) {
  if (!isObject(value)) return false;
  const allowed = new Set([
    "complete",
    "dimension",
    "maximum",
    "highest_used",
    "over_limit_count",
    "scanned_subject_count",
    "scanned_allocation_count",
    ...(requireAuthorityRevision ? ["authority_revision"] : []),
  ]);
  if (Object.keys(value).length !== allowed.size ||
      Object.keys(value).some((key) => !allowed.has(key)) ||
      value.complete !== true ||
      value.dimension !== REALM_EMAIL_ALIAS_LIMIT ||
      value.maximum !== expectedMaximum ||
      !(value.maximum === null ||
        (Number.isSafeInteger(value.maximum) && value.maximum >= 0)) ||
      !Number.isSafeInteger(value.highest_used) || value.highest_used < 0 ||
      !Number.isSafeInteger(value.over_limit_count) ||
      value.over_limit_count < 0 ||
      !Number.isSafeInteger(value.scanned_subject_count) ||
      value.scanned_subject_count < 0 ||
      value.scanned_subject_count > PLAN_FIT_AUTHORITY_SCAN_LIMIT ||
      !Number.isSafeInteger(value.scanned_allocation_count) ||
      value.scanned_allocation_count < 0 ||
      value.scanned_allocation_count > PLAN_FIT_AUTHORITY_SCAN_LIMIT ||
      value.over_limit_count > value.scanned_subject_count ||
      value.highest_used > value.scanned_allocation_count ||
      (value.scanned_subject_count === 0 && value.highest_used !== 0) ||
      (value.maximum === null && value.over_limit_count !== 0) ||
      (value.maximum !== null &&
        ((value.highest_used > value.maximum) !==
          (value.over_limit_count > 0))) ||
      (requireAuthorityRevision &&
        (!Number.isSafeInteger(value.authority_revision) ||
          value.authority_revision < 0))) {
    return false;
  }
  return true;
}

class RegistryError extends Error {
  constructor(message, status = 500, code = "", details = {}) {
    super(message);
    this.name = "RegistryError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

function fail(message, status = 500, code = "", details = {}) {
  throw new RegistryError(message, status, code, details);
}

export function normalizeRealmEmailAlias(value) {
  if (typeof value !== "string") {
    fail("alias must be a string", 400);
  }
  const alias = value.trim().toLowerCase();
  if (!ALIAS_PATTERN.test(alias) || alias.includes("--")) {
    fail(
      "alias must be 3-16 lowercase ASCII letters, digits, or single hyphens",
      400,
    );
  }
  if (CANONICAL_REALM_LABEL_PATTERN.test(alias)) {
    fail("alias must not look like a canonical Realm ID label", 400);
  }
  return alias;
}

// A deliberately small, deterministic ASCII skeleton. Unicode never reaches
// this step because the public grammar rejects it. Hyphen elision and common
// lookalike folding protect both platform reservations and customer claims.
export function realmEmailAliasSkeleton(alias) {
  const folded = {
    "0": "o",
    "1": "i",
    "3": "e",
    "4": "a",
    "5": "s",
    "7": "t",
    "8": "b",
    "9": "g",
    l: "i",
    i: "i",
  };
  return [...alias]
    .filter((character) => character !== "-")
    .map((character) => folded[character] ?? character)
    .join("");
}

export function realmEmailAliasEntitlement(snapshot) {
  const features = Array.isArray(snapshot?.features) ? snapshot.features : [];
  const featureEnabled = features.includes(REALM_EMAIL_ALIAS_FEATURE);
  const rawLimit = snapshot?.limits?.[REALM_EMAIL_ALIAS_LIMIT];
  // Plan limit semantics are shared across the product: an absent or null
  // dimension is unlimited, while an explicit zero disables capacity. This is
  // how Founder-style overrides stay unlimited without inventing a sentinel
  // integer that could overflow counters or serialized policy documents.
  const limit = rawLimit == null
    ? null
    : Number.isSafeInteger(rawLimit) && rawLimit >= 0
    ? rawLimit
    : 0;
  return {
    enabled: featureEnabled && (limit === null || limit > 0),
    limit,
  };
}

function validAliasLimit(value) {
  return value === null || (Number.isSafeInteger(value) && value >= 0);
}

function configuredTechnicalLimit(value, fallback, maximum, name) {
  if (value === undefined || value === null || value === "") return fallback;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 1 || parsed > maximum) {
    fail(`${name} configuration is invalid`, 503);
  }
  return parsed;
}

export function realmEmailAliasPendingRequestLimits(env = {}) {
  const perRealm = configuredTechnicalLimit(
    env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
    REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
    REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM,
    "realm email alias per-realm pending limit",
  );
  const perAccount = configuredTechnicalLimit(
    env.CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
    REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
    REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT,
    "realm email alias per-account pending limit",
  );
  if (perAccount < perRealm) {
    fail("realm email alias pending limit configuration is inconsistent", 503);
  }
  return { per_realm: perRealm, per_account: perAccount };
}

function validPlanFence(revision, snapshotHash) {
  return Number.isSafeInteger(revision) && revision >= 0 &&
    (revision === 0
      ? snapshotHash === ""
      : typeof snapshotHash === "string" && /^[0-9a-f]{64}$/.test(snapshotHash));
}

function comparePlanFence(leftRevision, rightRevision) {
  return leftRevision === rightRevision ? 0 : leftRevision < rightRevision ? -1 : 1;
}

function randomBase32(length = 16) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  const bytes = new Uint8Array(length);
  crypto.getRandomValues(bytes);
  return [...bytes]
    .map((byte) => alphabet[byte & 31])
    .join("");
}

function requestID() {
  return `earq_${randomBase32(16)}`;
}

function claimID() {
  return `era_${randomBase32(16)}`;
}

export function validateManagedRealmEmailDomain(value) {
  if (typeof value !== "string" || value !== value.trim().toLowerCase() ||
      value.length < 1 || value.length > 253 ||
      value.split(".").some((label) => !DOMAIN_LABEL_PATTERN.test(label))) {
    fail("managed email domain is invalid", 400);
  }
  return value;
}

// The first domain is the permanent agent-email domain. One explicit
// compatibility domain may retain already-issued canonical addresses during
// a migration, but can never grow into a broad or unbounded mail surface.
// Realm aliases are still assigned only on the primary domain.
export function managedRealmEmailDomains(env = {}) {
  const primary = validateManagedRealmEmailDomain(
    String(env.AGENT_EMAIL_DOMAIN ?? ""),
  );
  const rawLegacy = String(env.AGENT_EMAIL_LEGACY_DOMAINS ?? "");
  if (rawLegacy === "") return Object.freeze([primary]);
  if (rawLegacy !== rawLegacy.trim()) {
    fail("managed legacy email domains are invalid", 400);
  }
  const legacy = rawLegacy.split(",");
  if (legacy.some((domain) => domain.length === 0) ||
      legacy.length >= REALM_EMAIL_MAX_MANAGED_DOMAINS) {
    fail("managed legacy email domains are invalid", 400);
  }
  const domains = [
    primary,
    ...legacy.map((domain) => validateManagedRealmEmailDomain(domain)),
  ];
  if (new Set(domains).size !== domains.length) {
    fail("managed email domains must be unique", 400);
  }
  return Object.freeze(domains);
}

export function managedRealmEmailPrimaryDomain(env = {}) {
  return managedRealmEmailDomains(env)[0];
}

export function realmEmailRouteKey(domain, realmLabel) {
  const canonicalDomain = validateManagedRealmEmailDomain(domain);
  const label = typeof realmLabel === "string" ? realmLabel : "";
  if ((!ALIAS_PATTERN.test(label) && !CANONICAL_REALM_LABEL_PATTERN.test(label)) ||
      label.includes("--")) {
    fail("realm route label is invalid", 400);
  }
  return `${REALM_EMAIL_ROUTE_PREFIX}${canonicalDomain}:${label}`;
}

export function buildRealmEmailRouteProjection({
  account_id: accountID,
  domain,
  realm_id: realmID,
  realm_label: realmLabel,
  route_kind: routeKind,
  state,
  suspension_disposition: suspensionDisposition,
  controller_revision: controllerRevision,
  updated_at: updatedAt,
  cell_audience: cellAudience,
  ingest_url: ingestURL,
  cache_ttl_seconds: cacheTTLSeconds = REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS,
}) {
  const canonicalDomain = validateManagedRealmEmailDomain(domain);
  if (!MANAGED_ROUTE_ACCOUNT_ID_PATTERN.test(accountID ?? "")) {
    fail("realm route account_id is invalid", 400);
  }
  if (!REALM_ID_PATTERN.test(realmID ?? "")) fail("realm route realm_id is invalid", 400);
  realmEmailRouteKey(canonicalDomain, realmLabel);
  const canonicalLabel = realmID.slice("realm_".length);
  if ((routeKind === "canonical" && realmLabel !== canonicalLabel) ||
      (routeKind === "realm_alias" &&
        (realmLabel === canonicalLabel || CANONICAL_REALM_LABEL_PATTERN.test(realmLabel))) ||
      !["canonical", "realm_alias"].includes(routeKind)) {
    fail("realm route kind is inconsistent", 400);
  }
  if (!["applied", "suspended", "retired"].includes(state)) {
    fail("realm route state is invalid", 400);
  }
  if (!Number.isSafeInteger(controllerRevision) || controllerRevision < 1) {
    fail("realm route controller revision is invalid", 400);
  }
  if (!Number.isSafeInteger(cacheTTLSeconds) || cacheTTLSeconds < 1 ||
      cacheTTLSeconds > 3600 || typeof updatedAt !== "string" ||
      !Number.isFinite(Date.parse(updatedAt))) {
    fail("realm route freshness fields are invalid", 400);
  }
  const projection = {
    // Managed route v2 adds account_id as signed rollout authority. Version 1
    // remains readable only by the new edge's migration path and can never be
    // used for delivery because it cannot prove account cohort membership.
    schema_version: MANAGED_REALM_EMAIL_ROUTE_SCHEMA_VERSION,
    account_id: accountID,
    domain: canonicalDomain,
    realm_label: realmLabel,
    realm_id: realmID,
    route_kind: routeKind,
    state,
    controller_revision: controllerRevision,
    updated_at: updatedAt,
    cache_ttl_seconds: cacheTTLSeconds,
  };
  if (state === "suspended") {
    if (!["retry", "inactive"].includes(suspensionDisposition)) {
      fail("realm route suspension disposition is invalid", 400);
    }
    projection.suspension_disposition = suspensionDisposition;
  } else if (suspensionDisposition !== undefined) {
    fail("realm route suspension disposition is only valid for suspended routes", 400);
  }
  if (state === "applied") {
    if (!CELL_NAME_PATTERN.test(cellAudience ?? "")) {
      fail("realm route cell audience is invalid", 500);
    }
    let parsed;
    try {
      parsed = new URL(ingestURL);
    } catch {
      fail("realm route ingestion URL is invalid", 500);
    }
    if (parsed.protocol !== "https:" || parsed.username || parsed.password ||
        parsed.hash || parsed.search || !parsed.hostname ||
        parsed.hostname === "localhost") {
      fail("realm route ingestion URL is invalid", 500);
    }
    projection.cell_audience = cellAudience;
    projection.ingest_url = parsed.toString();
  }
  return projection;
}

function canonicalRouteAuthorityIsFresh(value, nowMS) {
  const updatedAt = Date.parse(value?.updated_at ?? "");
  const freshnessMS = REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000;
  return Number.isFinite(nowMS) && Number.isFinite(updatedAt) &&
    updatedAt <= nowMS + freshnessMS && nowMS <= updatedAt + freshnessMS;
}

function canonicalRouteAuthorityShouldRenew(value, nowMS) {
  const updatedAt = Date.parse(value?.updated_at ?? "");
  const freshnessMS = REALM_EMAIL_ROUTE_CACHE_TTL_SECONDS * 1_000;
  const renewalMS = Math.max(1, Math.floor(freshnessMS / 2));
  return !canonicalRouteAuthorityIsFresh(value, nowMS) ||
    nowMS >= updatedAt + renewalMS;
}

function claimKey(alias) {
  return `claim:${alias}`;
}

function claimSkeletonKey(skeleton) {
  return `claim-skeleton:${skeleton}`;
}

function claimUsageMemberKey(claimID) {
  return `claim-usage-member:${claimID}`;
}

function accountUsageMemberPrefix(accountID) {
  return `claim-usage-account-member:${accountID}:`;
}

function accountUsageMemberKey(accountID, claimID) {
  return `${accountUsageMemberPrefix(accountID)}${claimID}`;
}

function realmUsageMemberPrefix(accountID, realmID) {
  return `claim-usage-realm-member:${accountID}:${realmID}:`;
}

function realmUsageMemberKey(accountID, realmID, claimID) {
  return `${realmUsageMemberPrefix(accountID, realmID)}${claimID}`;
}

function accountUsageKey(accountID) {
  return `claim-usage-account:${accountID}`;
}

function realmUsageKey(accountID, realmID) {
  return `claim-usage-realm:${accountID}:${realmID}`;
}

function reservedKey(alias) {
  return `reserved:${alias}`;
}

function reservedSkeletonKey(skeleton) {
  return `reserved-skeleton:${skeleton}`;
}

function requestKey(id) {
  return `request:${id}`;
}

function accountClaimIndexPrefix(accountID, realmID = "") {
  return `account-claim:${accountID}:${realmID ? `${realmID}:` : ""}`;
}

function accountClaimIndexKey(claim) {
  return `${accountClaimIndexPrefix(claim.account_id, claim.realm_id)}` +
    `${claim.created_at}:${claim.alias}`;
}

function accountRequestIndexPrefix(accountID, realmID = "") {
  return `account-request:${accountID}:${realmID ? `${realmID}:` : ""}`;
}

function accountRequestIndexKey(request) {
  return `${accountRequestIndexPrefix(request.account_id, request.realm_id)}${request.id}`;
}

function accountCanonicalIndexPrefix(accountID) {
  return `account-canonical:${accountID}:`;
}

function accountCanonicalIndexKey(canonical) {
  return `${accountCanonicalIndexPrefix(canonical.account_id)}` +
    `${canonical.domain}:${canonical.realm_label}`;
}

function realmCanonicalIndexPrefix(accountID, realmID) {
  return `realm-canonical:${accountID}:${realmID}:`;
}

function realmCanonicalIndexKey(canonical) {
  return `${realmCanonicalIndexPrefix(canonical.account_id, canonical.realm_id)}` +
    `${canonical.domain}:${canonical.realm_label}`;
}

function canonicalRouteAuthorityKey(domain, realmID) {
  return `canonical:${domain}:${realmID.slice("realm_".length)}`;
}

function canonicalRouteLaneKey(domain, realmID) {
  return `canonical:${domain}:${realmID}`;
}

function realmCloseIntentKey(accountID, realmID) {
  return `realm-close-intent:${accountID}:${realmID}`;
}

function realmCloseFenceKey(accountID, realmID) {
  return `realm-close-fence:${accountID}:${realmID}`;
}

function realmCloseDueKey(intent) {
  return `realm-close-due:${String(intent.retry_at_ms).padStart(16, "0")}:` +
    `${intent.account_id}:${intent.realm_id}`;
}

function customDomainSubscriptionKey(claimID) {
  return `custom-domain-subscription:${claimID}`;
}

function customDomainSyncKey(claimID) {
  return `custom-domain-sync:${claimID}`;
}

function customDomainSyncAccountPrefix(accountID) {
  return `custom-domain-sync-account:${accountID}:`;
}

function customDomainSyncAccountKey(intent) {
  return `${customDomainSyncAccountPrefix(intent.claim_proof.account_id)}` +
    intent.claim_proof.realm_alias_claim_id;
}

function customDomainSubscriptionRealmPrefix(accountID, realmID) {
  return `custom-domain-subscription-realm:${accountID}:${realmID}:`;
}

function customDomainSubscriptionRealmKey(subscription) {
  return `${customDomainSubscriptionRealmPrefix(
    subscription.account_id,
    subscription.realm_id,
  )}${subscription.realm_alias_claim_id}`;
}

function customDomainSyncDueKey(intent) {
  if (!Number.isSafeInteger(intent?.retry_at_ms) || intent.retry_at_ms < 0 ||
      !CLAIM_ID_PATTERN.test(intent?.claim_proof?.realm_alias_claim_id ?? "")) {
    return null;
  }
  return `custom-domain-sync-due:${String(intent.retry_at_ms)
    .padStart(16, "0")}:${intent.claim_proof.realm_alias_claim_id}`;
}

function graceIndexKey(claim) {
  if (!claim?.plan_grace_until) return null;
  const deadline = Number.isSafeInteger(claim.grace_retry_at_ms)
    ? claim.grace_retry_at_ms
    : Date.parse(claim.plan_grace_until);
  return Number.isFinite(deadline)
    ? `grace:${String(deadline).padStart(16, "0")}:${claim.alias}`
    : null;
}

function planFenceKey(accountID) {
  return `plan-fence:${accountID}`;
}

function planIntentKey(accountID) {
  return `plan-intent:${accountID}`;
}

function planDueKey(intent) {
  return `plan-due:${String(intent.retry_at_ms).padStart(16, "0")}:${intent.account_id}`;
}

function approvalDueKey(claim) {
  return `approval-due:${String(claim.approval_retry_at_ms).padStart(16, "0")}:${claim.alias}`;
}

function projectionIntentKey(alias) {
  return `projection-intent:${alias}`;
}

function projectionDueKey(intent) {
  return `projection-due:${String(intent.retry_at_ms).padStart(16, "0")}:${intent.alias}`;
}

function internalDueKey(claim) {
  return `internal-due:${String(claim.internal_retry_at_ms).padStart(16, "0")}:${claim.alias}`;
}

function lifecycleIntentKey(accountID) {
  return `lifecycle-intent:${accountID}`;
}

function lifecycleFenceKey(accountID) {
  return `lifecycle-fence:${accountID}`;
}

function lifecycleDueKey(intent) {
  return `lifecycle-due:${String(intent.retry_at_ms).padStart(16, "0")}:${intent.account_id}`;
}

function routeRefreshKey(domain, realmLabel) {
  return `route-refresh:${domain}:${realmLabel}`;
}

function routeRefreshDueKey(intent) {
  return `refresh-due:${String(intent.retry_at_ms).padStart(16, "0")}:${intent.domain}:${intent.realm_label}`;
}

function retryDelayMs(failureCount) {
  const exponent = Math.max(0, Math.min(failureCount - 1, 8));
  return Math.min(
    REALM_EMAIL_ALIAS_RETRY_MS * (2 ** exponent),
    REALM_EMAIL_ALIAS_MAX_RETRY_MS,
  );
}

function effectiveClaimStatus(claim) {
  if (claim.retired_at) return "retired";
  if (claim.customer_activation_intent) return "provisioning";
  if (claim.internal_intent) {
    return claim.internal_failure_reason ? "provisioning_failed" : "provisioning";
  }
  if (claim.lifecycle_suspended) return "suspended_lifecycle";
  if (claim.operational_gate_suspended) return "suspended_gate";
  if (claim.admin_suspended) return "suspended_admin";
  if (claim.plan_suspended) return "suspended_plan";
  if (claim.plan_grace_until) return "active_grace";
  return claim.assignment_kind ? "active" : "pending_review";
}

function cellAliasState(claim) {
  const status = effectiveClaimStatus(claim);
  if (["active", "active_grace"].includes(status)) return "applied";
  if (status === "retired") return "retired";
  return "suspended";
}

function routeSuspensionDisposition(claim) {
  return claim.plan_suspended === true && !claim.lifecycle_suspended &&
      !claim.operational_gate_suspended && !claim.admin_suspended
    ? "inactive"
    : "retry";
}

function publicClaim(claim) {
  return {
    claim_id: claim.claim_id,
    alias: claim.alias,
    domain: claim.domain,
    account_id: claim.account_id,
    realm_id: claim.realm_id,
    status: effectiveClaimStatus(claim),
    assignment_kind: claim.assignment_kind,
    assignment_revision: claim.assignment_revision ?? 0,
    created_at: claim.created_at,
    updated_at: claim.updated_at,
    ...(claim.plan_grace_until
      ? { plan_grace_until: claim.plan_grace_until }
      : {}),
    ...(claim.retired_at ? { retired_at: claim.retired_at } : {}),
    ...(claim.internal_failure_reason
      ? { provisioning_failure: claim.internal_failure_reason }
      : {}),
    ...(claim.lifecycle_fence
      ? { lifecycle_fence: claim.lifecycle_fence }
      : {}),
  };
}

function publicReserved(entry) {
  return {
    name: entry.name,
    normalized_name: entry.name,
    confusable_skeleton: entry.skeleton,
    category: entry.category,
    reason: entry.reason,
    version: entry.version,
    policy_version: entry.policy_version,
    enabled: entry.enabled,
    internal_assignable: entry.internal_assignable,
    created_at: entry.created_at,
    updated_at: entry.updated_at,
    created_by: entry.created_by,
    updated_by: entry.updated_by,
    ...(entry.retired_at ? { retired_at: entry.retired_at } : {}),
    ...(entry.claim_conflict ? { claim_conflict: entry.claim_conflict } : {}),
  };
}

function validateActor(actor, kind) {
  if (!isObject(actor) || actor.kind !== kind ||
      typeof actor.id !== "string" || actor.id.length === 0 ||
      actor.id.length > 128) {
    fail("invalid mutation actor", 400);
  }
  return { kind: actor.kind, id: actor.id };
}

function validateAccountRealm(input) {
  if (!ACCOUNT_ID_PATTERN.test(input?.account_id ?? "")) {
    fail("invalid account_id", 400);
  }
  if (!REALM_ID_PATTERN.test(input?.realm_id ?? "")) {
    fail("invalid realm_id", 400);
  }
}

function validateCellCanonicalRoute(value, accountID, realmID = null) {
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") || !isObject(value) ||
      value.account_id !== accountID ||
      !REALM_ID_PATTERN.test(value.realm_id ?? "") ||
      (realmID !== null && value.realm_id !== realmID) ||
      !["live", "closing", "retired"].includes(value.state) ||
      !Number.isSafeInteger(value.generation) || value.generation < 1 ||
      ((value.state === "closing" || value.state === "retired") &&
        !IDEMPOTENCY_KEY_PATTERN.test(value.operation_id ?? "")) ||
      (value.state === "live" && value.operation_id != null)) {
    fail("cell returned an invalid canonical realm route", 502);
  }
  return {
    account_id: value.account_id,
    realm_id: value.realm_id,
    state: value.state,
    generation: value.generation,
    operation_id: value.operation_id ?? null,
  };
}

function canonicalRoutingPolicy(cellRoute, emailEnabled, deliveryEnabled) {
  if (cellRoute.state === "retired") {
    return { state: "retired", suspension_disposition: null };
  }
  if (cellRoute.state === "closing" || !deliveryEnabled) {
    return { state: "suspended", suspension_disposition: "retry" };
  }
  if (!emailEnabled) {
    return { state: "suspended", suspension_disposition: "inactive" };
  }
  return { state: "applied", suspension_disposition: null };
}

function publicCanonicalRoute(canonical) {
  return {
    account_id: canonical.account_id,
    realm_id: canonical.realm_id,
    domain: canonical.domain,
    realm_label: canonical.realm_label,
    state: canonical.state,
    controller_revision: canonical.controller_revision,
    cell_state: canonical.cell_state,
    cell_generation: canonical.cell_generation,
    ...(canonical.cell_operation_id
      ? { operation_id: canonical.cell_operation_id }
      : {}),
    updated_at: canonical.updated_at,
  };
}

function validateIdempotencyKey(value) {
  if (!IDEMPOTENCY_KEY_PATTERN.test(value ?? "")) {
    fail("idempotency_key is required", 400);
  }
  return value;
}

function validateReason(value, required = false) {
  if (value == null && !required) return "";
  if (typeof value !== "string" || value.trim().length === 0 ||
      value.trim().length > 500) {
    fail(required ? "reason is required" : "reason is invalid", 400);
  }
  return value.trim();
}

function validateCategory(value) {
  if (!CATEGORY_PATTERN.test(value ?? "")) {
    fail("category is invalid", 400);
  }
  return value;
}

function fingerprint(value) {
  return JSON.stringify(value);
}

function usageRecordIntegrity(record) {
  return fingerprint({
    schema_version: record.schema_version,
    account_id: record.account_id,
    realm_id: record.realm_id ?? null,
    open_requests: record.open_requests,
    pending_review: record.pending_review ?? null,
    provisioning: record.provisioning ?? null,
    customer_allocated: record.customer_allocated ?? null,
  });
}

function withUsageRecordIntegrity(record) {
  return { ...record, integrity: usageRecordIntegrity(record) };
}

function sameClaimUsageContribution(left, right) {
  if (!left || !right) return left === right;
  return left.claim_id === right.claim_id &&
    left.account_id === right.account_id &&
    left.realm_id === right.realm_id &&
    left.open_request === right.open_request &&
    left.pending_review === right.pending_review &&
    left.provisioning === right.provisioning &&
    left.customer_allocated === right.customer_allocated;
}

function listValues(listed) {
  return [...listed.values()];
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
    if (error instanceof RegistryError) throw error;
    fail("invalid list cursor", 400);
  }
}

export function realmEmailAliasRegistryStub(env) {
  if (!env?.REALM_EMAIL_ALIASES) return null;
  const namespace = env.REALM_EMAIL_ALIASES;
  return namespace.get(namespace.idFromName(DEFAULT_REGISTRY_OBJECT_NAME));
}

// readRealmEmailAliasPlanFit returns only aggregate commercial allocation
// usage. Suspended/grace allocations continue to consume their reserved slot,
// while internal Witself assignments do not.
export async function readRealmEmailAliasPlanFit(
  env,
  accountID,
  maximum,
) {
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub) throw new Error("realm alias authority is not configured");
  const response = await stub.fetch(
    "https://realm-email-alias.internal/plan/fit",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ account_id: accountID, maximum }),
    },
  );
  const body = await response.json().catch(() => null);
  if (!response.ok || body?.schema_version !== SCHEMA_VERSION ||
      body?.account_id !== accountID || body?.maximum !== maximum ||
      !Number.isSafeInteger(body?.over_limit_count) ||
      body.over_limit_count < 0 ||
      !Number.isSafeInteger(body?.highest_used) || body.highest_used < 0 ||
      (body.over_limit_count === 0 && body.highest_used > maximum) ||
      (body.over_limit_count > 0 && body.highest_used <= maximum)) {
    throw new Error(body?.error ?? "realm alias plan-fit authority is unavailable");
  }
  return body;
}

function agentEmailDomainRegistryStub(env) {
  if (!env?.AGENT_EMAIL_DOMAINS) return null;
  const namespace = env.AGENT_EMAIL_DOMAINS;
  return namespace.get(namespace.idFromName(DEFAULT_REGISTRY_OBJECT_NAME));
}

export async function reconcileRealmEmailAliasesForPlan(
  env,
  accountID,
  snapshot,
  mode,
  options = {},
) {
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub) {
    if (mode === "prepare") {
      throw new Error("realm email alias authority is not configured");
    }
    return { skipped: true };
  }
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !validPlanFence(snapshot?.revision, snapshot?.snapshot_hash) ||
      !Array.isArray(snapshot?.features) || !isObject(snapshot?.limits) ||
      !["prepare", "restrict_only", "complete"].includes(mode)) {
    throw new Error("invalid account plan snapshot for alias reconciliation");
  }
  const entitlement = realmEmailAliasEntitlement(snapshot);
  const response = await stub.fetch("https://realm-email-alias.internal/plan/reconcile", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      account_id: accountID,
      feature_enabled: entitlement.enabled,
      activation_enabled:
        String(env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") === "true",
      alias_limit: entitlement.limit,
      mode,
      plan_revision: snapshot?.revision ?? 0,
      plan_snapshot_hash: snapshot?.snapshot_hash ?? "",
      ...(options.recover_pending_revision === undefined
        ? {}
        : {
          recover_pending_revision: options.recover_pending_revision,
          recover_pending_snapshot_hash: options.recover_pending_snapshot_hash,
        }),
    }),
  });
  const body = await response.json().catch(() => null);
  const expectedMaximum = entitlement.enabled ? entitlement.limit : 0;
  if (mode === "prepare" && response.status === 409 &&
      body?.schema_version === SCHEMA_VERSION &&
      body?.account_id === accountID && body?.mode === "prepare" &&
      body?.plan_revision === snapshot.revision &&
      body?.plan_snapshot_hash === snapshot.snapshot_hash &&
      body?.prepared === false && body?.pending === false &&
      body?.stale === false && body?.complete === true &&
      body?.code === "plan_fit_failed" &&
      validRealmEmailAliasPrepareFit(body.fit, expectedMaximum, false)) {
    return body;
  }
  if (!response.ok) {
    throw new Error(body?.error ?? "realm email alias plan reconciliation failed");
  }
  if (mode === "prepare") {
    const prepared = body?.prepared === true && body?.pending === true &&
      body?.stale === false && body?.complete === true &&
      body?.plan_revision === snapshot.revision &&
      body?.plan_snapshot_hash === snapshot.snapshot_hash &&
      validRealmEmailAliasPrepareFit(body?.fit, expectedMaximum) &&
      body.fit.over_limit_count === 0 &&
      body?.registry_revision === body.fit.authority_revision;
    const stale = body?.prepared === false && body?.pending === false &&
      body?.stale === true && body?.complete === true && body?.fit === undefined &&
      body?.plan_revision === snapshot.revision &&
      body?.plan_snapshot_hash === snapshot.snapshot_hash;
    if (body?.schema_version !== SCHEMA_VERSION ||
        body?.account_id !== accountID || body?.mode !== "prepare" ||
        (!prepared && !stale)) {
      throw new Error("realm email alias prepare response is invalid");
    }
  }
  // The account plan must not advance while an activation-gate suspension is
  // only partially projected. A later bridge retry (and the registry alarm)
  // advances the durable cursor without turning this call into an unbounded
  // fan-out for large or unlimited accounts.
  if (mode === "restrict_only" &&
      String(env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") !== "true" &&
      body?.operational_gate_complete !== true) {
    throw new Error(
      "realm email alias activation-gate reconciliation is still converging",
    );
  }
  return body;
}

export async function reconcileRealmEmailAliasesForAccountLifecycle(
  env,
  accountID,
  {
    operation_id: operationID,
    epoch,
    action,
  },
) {
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub) return { skipped: true, complete: true };
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !IDEMPOTENCY_KEY_PATTERN.test(operationID ?? "") ||
      !Number.isSafeInteger(epoch) || epoch < 0 ||
      !["suspend", "republish", "retire"].includes(action)) {
    throw new Error("invalid account lifecycle fence for alias reconciliation");
  }
  // One registry call advances at most one bounded account-index page. The
  // account lifecycle Durable Object remains on the same durable phase until
  // the registry attests completion, so arbitrarily large accounts converge
  // without an unbounded Worker request.
  const payload = JSON.stringify({
    account_id: accountID,
    operation_id: operationID,
    epoch,
    action,
    activation_enabled:
      String(env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") === "true",
  });
  // Advance exactly one small registry page. The account lifecycle Durable
  // Object remains on the same phase and retries until the exact registry
  // fence attests completion.
  const response = await stub.fetch(
    "https://realm-email-alias.internal/account-lifecycle/reconcile",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: payload,
    },
  );
  const body = await response.json().catch(() => null);
  if (!response?.ok || body?.complete !== true) {
    throw new Error(
      body?.error ??
        (body?.complete === false
          ? "realm email alias lifecycle reconciliation is still converging"
          : "realm email alias lifecycle reconciliation failed"),
    );
  }
  return body;
}

export async function runScheduledCanonicalRealmRouteInventory(env) {
  if (!realmEmailCanonicalInventoryEnabled(env)) {
    return { ran: false, configured: true };
  }
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub || !env?.DIRECTORY || !env?.AGENT_EMAIL_DIRECTORY) {
    console.log("realm-email-canonical: inventory configuration is incomplete");
    return { ran: false, configured: false };
  }
  try {
    const response = await stub.fetch(
      "https://realm-email-alias.internal/canonical/inventory/reconcile",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      },
    );
    const body = await response.json().catch(() => null);
    if (!response.ok || body?.schema_version !== SCHEMA_VERSION) {
      throw new Error("invalid inventory acknowledgement");
    }
    return { ran: true, configured: true, ...body };
  } catch {
    console.log("realm-email-canonical: inventory tick failed");
    return { ran: true, configured: true, succeeded: false };
  }
}

/**
 * One globally named Durable Object owns managed realm-email aliases.
 * Durable storage is authoritative for claims, reservations, tombstones, and
 * audit history. AGENT_EMAIL_DIRECTORY KV is only an isolated routing
 * projection and never decides uniqueness or ownership; the broader DIRECTORY
 * binding is used solely to resolve the account's current cell target.
 */
export class DurableRealmEmailAliasRegistry {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.now = dependencies.now ?? (() => new Date());
    this.newRequestID = dependencies.newRequestID ?? requestID;
    this.newClaimID = dependencies.newClaimID ?? claimID;
    this.fetchImpl = dependencies.fetch ?? ((...args) => globalThis.fetch(...args));
    this.log = dependencies.log ?? ((value) => console.log(value));
    this.signRouteProjection = dependencies.signRouteProjection ??
      ((projection) => signAgentEmailRouteProjection(projection, this.env));
    this.agentEmailOperationsLease = new AgentEmailOperationsLeaseRuntime(
      this.storage,
      { now: this.now },
    );
    this.lanes = new Map();
    this.activeOperationalWork = 0;
    this.authorityJournal = new RealmEmailAliasJournalRuntime(
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
  }

  fetch(request) {
    return this.handleFetch(request);
  }

  withLane(key, work) {
    const prior = this.lanes.get(key) ?? Promise.resolve();
    let release;
    const gate = new Promise((resolve) => {
      release = resolve;
    });
    const tail = prior.catch(() => {}).then(() => gate);
    this.lanes.set(key, tail);
    return prior.catch(() => {}).then(work).finally(() => {
      release();
      if (this.lanes.get(key) === tail) this.lanes.delete(key);
    });
  }

  withAuthorityJournalMaintenance(work) {
    return this.withLane("registry:authority-journal", async () => {
      if (this.activeOperationalWork > 0) {
        throw new RealmEmailAliasJournalRuntimeError(
          "realm email alias authority work is still active; retry maintenance",
          "realm_email_alias_journal_operational_work_active",
        );
      }
      return work();
    });
  }

  async withAuthorityOperationalWork(work) {
    const rawApply = this.atomicRaw.bind(this);
    await this.withLane("registry:authority-journal", async () => {
      await this.authorityJournal.resume(rawApply);
      await this.authorityJournal.assertOperationalReady();
      this.activeOperationalWork++;
    });
    try {
      return await work();
    } finally {
      // Durable Object events share one isolate. Maintenance checks this count
      // while holding the short journal lane, so it can never install a freeze
      // over external work that is already in flight. A process restart
      // cancels that work and naturally resets the in-memory admission count.
      this.activeOperationalWork--;
      if (this.activeOperationalWork < 0) {
        this.activeOperationalWork = 0;
        throw new Error("realm email alias operational-work count underflow");
      }
    }
  }

  withLanes(keys, work) {
    const ordered = [...new Set(keys)].sort();
    const acquire = (index) => index >= ordered.length
      ? work()
      : this.withLane(ordered[index], () => acquire(index + 1));
    return acquire(0);
  }

  async operationLanes(path, input) {
    switch (path) {
      case "/request/create": {
        const alias = normalizeRealmEmailAlias(input.alias);
        return [
          `account:${input.account_id}`,
          `realm:${input.account_id}:${input.realm_id}`,
          `skeleton:${realmEmailAliasSkeleton(alias)}`,
        ];
      }
      case "/request/approve":
      case "/request/reject": {
        const request = await this.storage.get(requestKey(input.request_id));
        return request
          ? [
            `account:${request.account_id}`,
            `realm:${request.account_id}:${request.realm_id}`,
            `skeleton:${request.skeleton}`,
          ]
          : [`request:${input.request_id}`];
      }
      case "/alias/mutate": {
        const alias = normalizeRealmEmailAlias(input.alias);
        const claim = await this.storage.get(claimKey(alias));
        return claim
          ? [
            `account:${claim.account_id}`,
            `realm:${claim.account_id}:${claim.realm_id}`,
            `skeleton:${claim.skeleton}`,
          ]
          : [`skeleton:${realmEmailAliasSkeleton(alias)}`];
      }
      case "/alias/assign-internal": {
        const alias = normalizeRealmEmailAlias(input.alias);
        return [
          `account:${input.account_id}`,
          `realm:${input.account_id}:${input.realm_id}`,
          `skeleton:${realmEmailAliasSkeleton(alias)}`,
        ];
      }
      case "/alias/abort-internal": {
        const alias = normalizeRealmEmailAlias(input.alias);
        const claim = await this.storage.get(claimKey(alias));
        return claim
          ? [
            `account:${claim.account_id}`,
            `realm:${claim.account_id}:${claim.realm_id}`,
            `skeleton:${claim.skeleton}`,
          ]
          : [`skeleton:${realmEmailAliasSkeleton(alias)}`];
      }
      case "/reserved/create":
      case "/reserved/update":
      case "/reserved/retire": {
        const alias = normalizeRealmEmailAlias(input.name);
        return [`skeleton:${realmEmailAliasSkeleton(alias)}`];
      }
      case "/plan/reconcile":
        return [`account:${input.account_id}`];
      case "/plan/fit":
        return ACCOUNT_ID_PATTERN.test(input?.account_id ?? "")
          ? [`account:${input.account_id}`]
          : [];
      case "/account-lifecycle/reconcile":
        return [`account:${input.account_id}`];
      case "/route/get": {
        const domain = validateManagedRealmEmailDomain(input?.domain);
        const realmLabel = typeof input?.realm_label === "string"
          ? input.realm_label
          : "";
        realmEmailRouteKey(domain, realmLabel);
        if (!CANONICAL_REALM_LABEL_PATTERN.test(realmLabel)) return [];
        const canonical = await this.storage.get(
          `canonical:${domain}:${realmLabel}`,
        );
        return [
          ...(ACCOUNT_ID_PATTERN.test(canonical?.account_id ?? "")
            ? [`account:${canonical.account_id}`]
            : []),
          canonicalRouteLaneKey(domain, `realm_${realmLabel}`),
        ];
      }
      case "/counter/rebuild":
        return ["registry:counter-rebuild"];
      case "/canonical/inventory/reconcile":
        return ["registry:canonical-inventory"];
      case "/canonical/realm-close":
      case "/canonical/realm-close/get": {
        let domains = managedRealmEmailDomains(this.env);
        if (ACCOUNT_ID_PATTERN.test(input?.account_id ?? "") &&
            REALM_ID_PATTERN.test(input?.realm_id ?? "")) {
          const persisted = await this.storage.get(
            realmCloseIntentKey(input.account_id, input.realm_id),
          );
          if (persisted) domains = this.realmCloseIntentDomains(persisted);
        }
        return [
          `account:${input.account_id}`,
          `realm:${input.account_id}:${input.realm_id}`,
          ...domains.map((domain) =>
            canonicalRouteLaneKey(domain, String(input.realm_id ?? ""))
          ),
        ];
      }
      default:
        return [];
    }
  }

  async handleFetch(request) {
    if (request.method !== "POST") {
      return errorResponse("registry endpoint not found", 404);
    }
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid JSON body", 400);
    }
    try {
      const path = new URL(request.url).pathname;
      const rawApply = this.atomicRaw.bind(this);
      if (isAgentEmailOperationsLeasePath(path)) {
        try {
          const result = await this.withLane(
            "registry:agent-email-operations-lease",
            () => this.agentEmailOperationsLease.execute(path, input),
          );
          return agentEmailOperationsLeaseJSON(result.body, result.status);
        } catch (error) {
          return agentEmailOperationsLeaseErrorResponse(error);
        }
      }
      if (path === "/journal/status") {
        return json({
          schema_version: SCHEMA_VERSION,
          ...await this.authorityJournal.status(),
        });
      }
      if (path === "/journal/bootstrap" || path === "/journal/checkpoint") {
        const result = await this.withAuthorityJournalMaintenance(
          () => path === "/journal/bootstrap"
            ? this.authorityJournal.bootstrap(input, rawApply)
            : this.authorityJournal.checkpoint(input, rawApply),
        );
        return json({ schema_version: SCHEMA_VERSION, ...result });
      }
      if (path === "/recovery/start") {
        const result = await this.withAuthorityJournalMaintenance(
          () => this.authorityJournal.startRecovery(input, rawApply),
        );
        return json({ schema_version: SCHEMA_VERSION, ...result }, 202);
      }
      if (path === "/recovery/status") {
        const result = await this.authorityJournal.recoveryStatus(
          input?.recovery_id,
        );
        return result
          ? json({ schema_version: SCHEMA_VERSION, ...result })
          : errorResponse("realm email alias recovery not found", 404);
      }
      if (path === "/recovery/advance" || path === "/recovery/verify") {
        const result = await this.withAuthorityJournalMaintenance(
          () => path === "/recovery/advance"
            ? this.authorityJournal.advanceRecovery(input, rawApply)
            : this.authorityJournal.verifyRecovery(input, rawApply),
        );
        return json(
          { schema_version: SCHEMA_VERSION, ...result },
          result.sealed || result.failed ? 200 : 202,
        );
      }
      // Custom-domain routing begins with an exact claim proof and a permanent
      // sparse subscription handshake. Later subscribed-claim changes use a
      // bounded journaled notification back to the customer-domain registry;
      // claims without that marker remain completely dark.
      if (path === "/alias/claim-proof") {
        return await this.withAuthorityOperationalWork(async () => {
          const alias = normalizeRealmEmailAlias(input?.realm_label);
          return this.withLane(
            `skeleton:${realmEmailAliasSkeleton(alias)}`,
            () => this.getAliasClaimProof(input, alias),
          );
        });
      }
      if (path === "/alias/custom-domain-route-subscribe") {
        return await this.withAuthorityOperationalWork(async () => {
          const proof = this.customDomainSubscriptionProof(input);
          // Realm close owns the account and realm lanes. Holding that exact
          // pair through the live-claim proof, close-fence check, and marker
          // commit makes "subscription acknowledged before any route write"
          // a real ordering boundary rather than a check-then-write race.
          return this.withLanes([
            `account:${proof.account_id}`,
            `realm:${proof.account_id}:${proof.realm_id}`,
            `skeleton:${realmEmailAliasSkeleton(proof.realm_label)}`,
          ], () => this.registerCustomDomainSubscription(input, proof));
        });
      }
      return await this.withAuthorityOperationalWork(async () => {
        await this.withLane("registry:seed", () => this.ensureSeeded());
        const execute = async () => {
          switch (path) {
        case "/request/create":
          return await this.createRequest(input);
        case "/request/list":
          return await this.listRequests(input, false);
        case "/request/admin-list":
          return await this.listRequests(input, true);
        case "/request/get":
          return await this.getRequest(input);
        case "/request/approve":
          return await this.approveRequest(input);
        case "/request/reject":
          return await this.rejectRequest(input);
        case "/alias/list":
          return await this.listAliases(input);
        case "/alias/mutate":
          return await this.mutateAlias(input);
        case "/alias/assign-internal":
          return await this.assignInternal(input);
        case "/alias/abort-internal":
          return await this.abortInternal(input);
        case "/reserved/list":
          return await this.listReserved(input);
        case "/reserved/get":
          return await this.getReserved(input);
        case "/reserved/create":
          return await this.createReserved(input);
        case "/reserved/update":
          return await this.updateReserved(input, false);
        case "/reserved/retire":
          return await this.updateReserved(input, true);
        case "/audit/list":
          return await this.listAudit(input);
        case "/route/get":
          return await this.getRoute(input);
        case "/plan/reconcile":
          return await this.reconcilePlan(input);
        case "/plan/fit":
          return await this.planFit(input);
        case "/account-lifecycle/reconcile":
          return await this.reconcileAccountLifecycle(input);
        case "/counter/rebuild":
          return await this.rebuildPendingCounters(input);
        case "/canonical/inventory/reconcile":
          return await this.reconcileCanonicalInventory();
        case "/canonical/realm-close":
          return await this.closeRealm(input);
        case "/canonical/realm-close/get":
          return await this.getRealmClose(input);
        default:
          return errorResponse("registry endpoint not found", 404);
          }
        };
        const lanes = await this.operationLanes(path, input);
        if (lanes.length > 0) {
          return await this.withLanes(lanes, execute);
        }
        return await execute();
      });
    } catch (error) {
      const journalError = error instanceof RealmEmailAliasJournalRuntimeError;
      const journalConflictCodes = new Set([
        "realm_email_alias_journal_already_bootstrapped",
        "realm_email_alias_journal_fence_mismatch",
        "realm_email_alias_journal_fork_detected",
        "realm_email_alias_journal_idempotency_conflict",
        "realm_email_alias_recovery_checkpoint_invalid",
        "realm_email_alias_recovery_collision",
        "realm_email_alias_recovery_digest_mismatch",
        "realm_email_alias_recovery_action_fence_mismatch",
        "realm_email_alias_recovery_action_not_allowed",
        "realm_email_alias_recovery_idempotency_conflict",
        "realm_email_alias_recovery_incomplete",
        "realm_email_alias_recovery_invariant_failed",
        "realm_email_alias_recovery_revision_regression",
        "realm_email_alias_recovery_target_not_empty",
        "realm_email_alias_recovery_target_sealed",
        "realm_email_alias_recovery_tombstone_resurrection",
        "realm_email_alias_recovery_upgrade_required",
      ]);
      const journalBadRequestCodes = new Set([
        "realm_email_alias_journal_maintenance_invalid",
        "realm_email_alias_recovery_request_invalid",
      ]);
      return errorResponse(
        String(error?.message ?? error),
        error instanceof RegistryError
          ? error.status
          : journalBadRequestCodes.has(error?.code)
          ? 400
          : journalConflictCodes.has(error?.code)
          ? 409
          : journalError
          ? 503
          : 500,
        error instanceof RegistryError || journalError ? error.code : "",
        error instanceof RegistryError ? error.details : {},
      );
    }
  }

  async atomic(entries, deletes = [], options = {}) {
    const augmented = await this.withCustomDomainSyncOutboxes(entries, deletes);
    return this.withLane(
      "registry:authority-journal",
      () => this.authorityJournal.commit(
        augmented.entries,
        augmented.deletes,
        options,
        this.atomicRaw.bind(this),
      ),
    );
  }

  customDomainClaimProof(claim) {
    if (!claim?.assignment_kind || claim.customer_activation_intent === true ||
        claim.internal_intent === true) return null;
    try {
      const state = cellAliasState(claim);
      return buildRealmEmailAliasClaimProof({
        account_id: claim.account_id,
        realm_id: claim.realm_id,
        realm_label: claim.alias,
        realm_alias_claim_id: claim.claim_id,
        realm_alias_revision: claim.assignment_revision,
        state,
        ...(state === "suspended"
          ? { suspension_disposition: routeSuspensionDisposition(claim) }
          : {}),
        updated_at: claim.updated_at,
      });
    } catch {
      fail("realm email alias claim is invalid", 503);
    }
  }

  async withCustomDomainSyncOutboxes(entries, deletes) {
    const finalClaims = new Map();
    for (const [key, value] of entries) {
      if (typeof key === "string" && key.startsWith("claim:")) {
        finalClaims.set(key, value);
      }
    }
    if (finalClaims.size === 0) {
      return { entries, deletes };
    }
    const augmentedEntries = [...entries];
    const augmentedDeletes = [...deletes];
    for (const claim of finalClaims.values()) {
      const subscription = await this.storage.get(
        customDomainSubscriptionKey(claim?.claim_id ?? ""),
      );
      if (!subscription) continue;
      const proof = this.customDomainClaimProof(claim);
      if (!proof) {
        fail("custom-domain alias subscription claim is not projectable", 503);
      }
      if (subscription.account_id !== proof.account_id ||
          subscription.realm_id !== proof.realm_id ||
          subscription.realm_label !== proof.realm_label ||
          subscription.realm_alias_claim_id !==
            proof.realm_alias_claim_id) {
        fail("custom-domain alias subscription is inconsistent", 503);
      }
      const key = customDomainSyncKey(proof.realm_alias_claim_id);
      const existing = await this.storage.get(key);
      const sourceFingerprint = realmEmailAliasClaimRouteFingerprint(proof);
      if (existing?.source_fingerprint === sourceFingerprint) {
        const due = customDomainSyncDueKey(existing);
        if (due) augmentedEntries.push([due, proof.realm_alias_claim_id]);
        continue;
      }
      const now = this.now();
      const intent = {
        schema_version: CUSTOM_DOMAIN_SYNC_SCHEMA,
        phase: "enqueue",
        claim_proof: proof,
        source_fingerprint: sourceFingerprint,
        failure_count: 0,
        retry_at_ms: now.getTime(),
        created_at: existing?.created_at ?? now.toISOString(),
        updated_at: now.toISOString(),
      };
      augmentedEntries.push(
        [key, intent],
        [customDomainSyncAccountKey(intent), key],
        [customDomainSyncDueKey(intent), proof.realm_alias_claim_id],
      );
      const oldDue = customDomainSyncDueKey(existing);
      if (oldDue && oldDue !== customDomainSyncDueKey(intent)) {
        augmentedDeletes.push(oldDue);
      }
    }
    return {
      entries: augmentedEntries,
      deletes: [...new Set(augmentedDeletes.filter(Boolean))],
    };
  }

  async atomicRaw(entries, deletes = [], options = {}) {
    const apply = async (storage) => {
      const transitionMode = options.claimUsageTransition?.mode ?? "ordinary";
      if (options.claimUsageTransition &&
          !["migration", "verification"].includes(transitionMode)) {
        // This check lives inside the same storage transaction as the claim
        // and derived-counter writes. A recovery request can therefore run
        // immediately before or after this commit, but can never interleave
        // and let a stale ready meta record overwrite a rebuilding fence.
        await this.assertPendingCountersReady(storage);
      }
      for (const [key, value] of entries) await storage.put(key, value);
      for (const key of deletes) await storage.delete(key);
      if (options.claimUsageTransition) {
        await this.applyClaimUsageTransition(
          storage,
          options.claimUsageTransition.previousClaim,
          options.claimUsageTransition.desiredClaim,
          options.claimUsageTransition.updatedAt,
          options.claimUsageTransition.mode ?? "ordinary",
        );
      }
    };
    if (typeof this.storage.transaction === "function") {
      await this.storage.transaction(apply);
    } else {
      await apply(this.storage);
    }
  }

  async ensureSeeded() {
    const current = await this.storage.get(META_KEY);
    if (current?.seeded) {
      return this.ensurePendingCounterMigration(current);
    }
    const now = this.now().toISOString();
    const meta = {
      schema_version: SCHEMA_VERSION,
      seeded: true,
      pending_counter_schema_version: PENDING_COUNTER_SCHEMA_VERSION,
      pending_counter_state: "ready",
      registry_revision: 1,
      reserved_policy_version: 1,
      audit_sequence: 1,
      created_at: now,
      updated_at: now,
    };
    const entries = [[META_KEY, meta]];
    for (const [name, category] of INITIAL_RESERVED_REALM_EMAIL_ALIASES) {
      const skeleton = realmEmailAliasSkeleton(name);
      const entry = {
        name,
        skeleton,
        category,
        reason: "initial Witself platform protection",
        version: 1,
        policy_version: 1,
        enabled: true,
        internal_assignable: true,
        created_at: now,
        updated_at: now,
        created_by: "system:seed",
        updated_by: "system:seed",
        retired_at: null,
        claim_conflict: null,
      };
      entries.push([reservedKey(name), entry]);
      entries.push([`reserved-history:${name}:00000001`, entry]);
      entries.push([reservedSkeletonKey(skeleton), name]);
    }
    entries.push(["audit:000000000001", {
      sequence: 1,
      registry_revision: 1,
      occurred_at: now,
      actor_kind: "system",
      actor_id: "seed",
      action: "reserved.seeded",
      target: "reserved-policy",
      metadata: { count: INITIAL_RESERVED_REALM_EMAIL_ALIASES.length },
    }]);
    await this.atomic(entries);
    return meta;
  }

  async ensurePendingCounterMigration(meta) {
    const existing = await this.storage.get(PENDING_COUNTER_MIGRATION_KEY);
    if (meta.pending_counter_schema_version === PENDING_COUNTER_SCHEMA_VERSION &&
        (meta.pending_counter_state === undefined ||
          meta.pending_counter_state === "ready") && !existing) {
      return meta;
    }
    if (meta.pending_counter_schema_version !== undefined &&
        meta.pending_counter_schema_version !== PENDING_COUNTER_SCHEMA_VERSION) {
      fail("realm email alias pending counter schema is unsupported", 503);
    }
    if (meta.pending_counter_state !== undefined &&
        !["ready", "rebuilding"].includes(meta.pending_counter_state)) {
      fail("realm email alias pending counter state is invalid", 503);
    }
    if (existing) {
      if (existing.schema_version !== PENDING_COUNTER_SCHEMA_VERSION ||
          !Number.isSafeInteger(existing.retry_at_ms)) {
        fail("realm email alias pending counter migration is invalid", 503);
      }
      if (typeof this.storage.setAlarm === "function") {
        await this.storage.setAlarm(Math.min(
          existing.retry_at_ms,
          this.now().getTime() + PENDING_COUNTER_MIGRATION_RETRY_MS,
        ));
      }
      return meta;
    }
    if (meta.pending_counter_state === "rebuilding") {
      fail("realm email alias pending counter rebuild intent is missing", 503);
    }

    // A freshly deployed dark registry normally has reservations but no
    // claims. Prove that with one bounded read and upgrade it immediately.
    // Any pre-existing claim set is rebuilt by the paginated alarm below;
    // customer creation remains fail-closed until that exact scan completes.
    const firstClaim = await this.storage.list({ prefix: "claim:", limit: 1 });
    if (firstClaim.size === 0) {
      const upgraded = {
        ...meta,
        pending_counter_schema_version: PENDING_COUNTER_SCHEMA_VERSION,
        pending_counter_state: "ready",
        updated_at: this.now().toISOString(),
      };
      await this.atomic([[META_KEY, upgraded]]);
      return upgraded;
    }

    const now = this.now();
    const migration = {
      schema_version: PENDING_COUNTER_SCHEMA_VERSION,
      kind: "upgrade",
      phase: "scan",
      cursor: null,
      scanned: 0,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: now.toISOString(),
      updated_at: now.toISOString(),
    };
    await this.storage.put(PENDING_COUNTER_MIGRATION_KEY, migration);
    if (typeof this.storage.setAlarm === "function") {
      await this.storage.setAlarm(now.getTime());
    }
    return meta;
  }

  async assertPendingCountersReady(storage = this.storage) {
    const meta = await storage.get(META_KEY);
    if (meta?.pending_counter_schema_version !==
        PENDING_COUNTER_SCHEMA_VERSION ||
        (meta.pending_counter_state !== undefined &&
          meta.pending_counter_state !== "ready") ||
        await storage.get(PENDING_COUNTER_MIGRATION_KEY)) {
      fail("realm email alias pending counters are still rebuilding", 503);
    }
  }

  async rebuildPendingCounters(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const reason = validateReason(input.reason, true);
    const fp = fingerprint({ action: "rebuild_pending_counters", reason });
    const scope = "counter-rebuild";
    const body = {
      schema_version: SCHEMA_VERSION,
      accepted: true,
      pending_counter_state: "rebuilding",
    };
    const result = await this.withLane("registry:metadata", async () => {
      const replay = await this.idempotent(scope, key, fp);
      if (replay) return { response: replay, installed: false };
      if (await this.storage.get(PENDING_COUNTER_MIGRATION_KEY)) {
        fail("realm email alias pending counters are already rebuilding", 409);
      }
      const meta = await this.storage.get(META_KEY);
      if (!meta?.seeded) {
        fail("realm email alias registry is not initialized", 503);
      }
      // Unlike mutations that perform external projection work, installing a
      // recovery fence has no reason to release the global metadata lane after
      // reserving its revision. The new meta, migration, idempotency record,
      // and committed audit event land in one transaction while the lane is
      // still held, so no concurrent mutation can advance meta and then be
      // overwritten by a stale recovery snapshot.
      const mutation = await this.prepareMutation(
        meta,
        actor,
        "alias.pending_counters_rebuild_requested",
        "pending-counters",
        { reason },
      );
      const now = new Date(mutation.now);
      const migration = {
        schema_version: PENDING_COUNTER_SCHEMA_VERSION,
        kind: "recovery",
        phase: "clear",
        clear_prefix_index: 0,
        cursor: null,
        scanned: 0,
        verified: 0,
        failure_count: 0,
        retry_at_ms: now.getTime(),
        created_at: mutation.now,
        updated_at: mutation.now,
        requested_by: actor.id,
        reason,
      };
      const rebuildingMeta = {
        ...mutation.meta,
        pending_counter_schema_version: PENDING_COUNTER_SCHEMA_VERSION,
        pending_counter_state: "rebuilding",
        updated_at: mutation.now,
      };
      await this.atomic([
        [META_KEY, rebuildingMeta],
        ...mutation.entries,
        [PENDING_COUNTER_MIGRATION_KEY, migration],
        [`idem:${scope}:${key}`, { fingerprint: fp, status: 202, body }],
      ]);
      return { response: json(body, 202), installed: true };
    });
    const migration = await this.storage.get(PENDING_COUNTER_MIGRATION_KEY);
    if (result.installed || migration) {
      try {
        // The durable fence and idempotency response may already exist when a
        // prior setAlarm acknowledgement was lost. Replaying the same key must
        // therefore re-arm recovery rather than returning the stored 202 while
        // leaving the registry permanently fenced.
        await this.scheduleNextAlarm();
      } catch {
        fail("realm email alias pending counter rebuild could not be scheduled", 503);
      }
    }
    return result.response;
  }

  claimUsageContribution(claim) {
    if (!claim || claim.retired_at || claim.assignment_kind === "internal") {
      return null;
    }
    if (!ACCOUNT_ID_PATTERN.test(claim.account_id ?? "") ||
        !REALM_ID_PATTERN.test(claim.realm_id ?? "") ||
        !CLAIM_ID_PATTERN.test(claim.claim_id ?? "")) {
      fail("realm email alias claim cannot be counted safely", 503);
    }
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

  validateUsageRecord(record, kind, accountID, realmID = null) {
    if (!record) {
      return withUsageRecordIntegrity({
        schema_version: PENDING_COUNTER_SCHEMA_VERSION,
        account_id: accountID,
        ...(realmID ? { realm_id: realmID } : {}),
        open_requests: 0,
        ...(realmID
          ? { pending_review: 0, provisioning: 0, customer_allocated: 0 }
          : {}),
      });
    }
    if (record.schema_version !== PENDING_COUNTER_SCHEMA_VERSION ||
        record.account_id !== accountID ||
        (realmID !== null && record.realm_id !== realmID) ||
        !Number.isSafeInteger(record.open_requests) ||
        record.open_requests < 0 ||
        (kind === "realm" &&
          (!Number.isSafeInteger(record.pending_review) ||
            record.pending_review < 0 ||
            !Number.isSafeInteger(record.provisioning) ||
            record.provisioning < 0 ||
            record.open_requests !==
              record.pending_review + record.provisioning ||
            !Number.isSafeInteger(record.customer_allocated) ||
            record.customer_allocated < 0)) ||
        record.integrity !== usageRecordIntegrity(record)) {
      fail("realm email alias pending counter is invalid", 503);
    }
    return record;
  }

  async usageForRealm(accountID, realmID) {
    const [accountRaw, realmRaw] = await Promise.all([
      this.storage.get(accountUsageKey(accountID)),
      this.storage.get(realmUsageKey(accountID, realmID)),
    ]);
    if (!accountRaw) {
      const members = await this.storage.list({
        prefix: accountUsageMemberPrefix(accountID),
        limit: 1,
      });
      if (members.size > 0) {
        fail("realm email alias account pending counter is missing", 503);
      }
    }
    if (!realmRaw) {
      const members = await this.storage.list({
        prefix: realmUsageMemberPrefix(accountID, realmID),
        limit: 1,
      });
      if (members.size > 0) {
        fail("realm email alias realm pending counter is missing", 503);
      }
    }
    return {
      account: this.validateUsageRecord(accountRaw, "account", accountID),
      realm: this.validateUsageRecord(realmRaw, "realm", accountID, realmID),
    };
  }

  async collectPlanFitEvidence(accountID, maximum) {
    await this.assertPendingCountersReady();

    let cursor = null;
    let scannedRealms = 0;
    let totalOpenRequests = 0;
    let expectedMemberCount = 0;
    let totalAllocated = 0;
    let overLimitCount = 0;
    let highestUsed = 0;
    do {
      const page = await this.boundedValues(
        realmUsageKey(accountID, ""),
        PLAN_FIT_AUTHORITY_PAGE_LIMIT,
        false,
        cursor,
      );
      scannedRealms += page.values.length;
      if (scannedRealms > PLAN_FIT_AUTHORITY_SCAN_LIMIT) {
        fail("realm email alias plan-fit authority scan is capped", 503);
      }
      for (const raw of page.values) {
        if (!REALM_ID_PATTERN.test(raw?.realm_id ?? "")) {
          fail("realm email alias realm counter is invalid", 503);
        }
        const usage = this.validateUsageRecord(
          raw,
          "realm",
          accountID,
          raw.realm_id,
        );
        totalOpenRequests += usage.open_requests;
        expectedMemberCount += usage.pending_review + usage.customer_allocated;
        totalAllocated += usage.customer_allocated;
        if (!Number.isSafeInteger(totalOpenRequests) ||
            !Number.isSafeInteger(expectedMemberCount) ||
            !Number.isSafeInteger(totalAllocated) ||
            totalAllocated > PLAN_FIT_AUTHORITY_SCAN_LIMIT) {
          fail("realm email alias plan-fit authority scan is capped", 503);
        }
        highestUsed = Math.max(highestUsed, usage.customer_allocated);
        if (maximum !== null && usage.customer_allocated > maximum) {
          overLimitCount += 1;
        }
      }
      cursor = page.next_cursor;
    } while (cursor);

    let memberCursor = null;
    let memberCount = 0;
    do {
      const page = await this.boundedValues(
        accountUsageMemberPrefix(accountID),
        PLAN_FIT_AUTHORITY_PAGE_LIMIT,
        false,
        memberCursor,
      );
      memberCount += page.values.length;
      if (memberCount > PLAN_FIT_AUTHORITY_SCAN_LIMIT) {
        fail("realm email alias plan-fit authority scan is capped", 503);
      }
      if (page.values.some((claimID) =>
        !CLAIM_ID_PATTERN.test(claimID ?? "")
      )) {
        fail("realm email alias account counter membership is invalid", 503);
      }
      memberCursor = page.next_cursor;
    } while (memberCursor);

    const account = this.validateUsageRecord(
      await this.storage.get(accountUsageKey(accountID)),
      "account",
      accountID,
    );
    if (account.open_requests !== totalOpenRequests ||
        memberCount !== expectedMemberCount) {
      fail("realm email alias plan-fit counters disagree", 503);
    }
    // A repair can begin between the first readiness check and the final
    // page. Never return a mixed pre/post-rebuild view as a successful fit.
    await this.assertPendingCountersReady();
    return {
      complete: true,
      dimension: REALM_EMAIL_ALIAS_LIMIT,
      maximum,
      highest_used: highestUsed,
      over_limit_count: overLimitCount,
      scanned_subject_count: scannedRealms,
      scanned_allocation_count: totalAllocated,
    };
  }

  async planFit(input) {
    const accountID = input?.account_id;
    const maximum = input?.maximum;
    if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
        !Number.isSafeInteger(maximum) || maximum < 0) {
      fail("realm email alias plan-fit request is invalid", 400);
    }
    await this.assertPendingCountersReady();
    const [planIntent, lifecycleIntent] = await Promise.all([
      this.storage.get(planIntentKey(accountID)),
      this.storage.get(lifecycleIntentKey(accountID)),
    ]);
    if (planIntent || lifecycleIntent) {
      fail("realm email alias policy is still converging", 409);
    }
    const fit = await this.collectPlanFitEvidence(accountID, maximum);
    return json({
      schema_version: SCHEMA_VERSION,
      account_id: accountID,
      maximum,
      over_limit_count: fit.over_limit_count,
      highest_used: fit.highest_used,
    });
  }

  pendingCapacity(usage) {
    const limits = realmEmailAliasPendingRequestLimits(this.env);
    return {
      realm: {
        used: usage.realm.open_requests,
        max: limits.per_realm,
        remaining: Math.max(0, limits.per_realm - usage.realm.open_requests),
        at_limit: usage.realm.open_requests >= limits.per_realm,
      },
      account: {
        used: usage.account.open_requests,
        max: limits.per_account,
        remaining: Math.max(
          0,
          limits.per_account - usage.account.open_requests,
        ),
        at_limit: usage.account.open_requests >= limits.per_account,
      },
    };
  }

  observePendingLimitRefusal(scope, limit) {
    // Cloudflare Worker logs are the low-cardinality operational signal for
    // this admission guard. Never include tenant, realm, alias, request, claim,
    // actor, usage, or free-form error data, and never grow durable audit state
    // for a rejected request.
    this.log(JSON.stringify({
      event: "realm_email_alias_pending_limit_refused",
      scope,
      limit,
    }));
  }

  async applyClaimUsageTransition(
    storage,
    previousClaim,
    desiredClaim,
    updatedAt,
    mode = "ordinary",
  ) {
    const claimID = desiredClaim?.claim_id ?? previousClaim?.claim_id;
    if (!CLAIM_ID_PATTERN.test(claimID ?? "")) {
      fail("realm email alias claim cannot be counted safely", 503);
    }
    const memberKey = claimUsageMemberKey(claimID);
    const stored = await storage.get(memberKey);
    const previous = this.claimUsageContribution(previousClaim);
    const desired = this.claimUsageContribution(desiredClaim);
    if (stored && (stored.schema_version !== PENDING_COUNTER_SCHEMA_VERSION ||
        stored.claim_id !== claimID ||
        ![0, 1].includes(stored.open_request) ||
        ![0, 1].includes(stored.pending_review) ||
        ![0, 1].includes(stored.provisioning) ||
        stored.open_request !== stored.pending_review + stored.provisioning ||
        ![0, 1].includes(stored.customer_allocated) ||
        (stored.open_request === 0 && stored.customer_allocated === 0) ||
        !ACCOUNT_ID_PATTERN.test(stored.account_id ?? "") ||
        !REALM_ID_PATTERN.test(stored.realm_id ?? ""))) {
      fail("realm email alias claim counter membership is invalid", 503);
    }
    if (mode === "ordinary" || mode === "verification") {
      if (!sameClaimUsageContribution(stored ?? null, previous)) {
        fail("realm email alias claim counter membership drifted", 503);
      }
    } else if (mode === "create") {
      if (previous !== null || stored) {
        fail("realm email alias claim counter create is inconsistent", 503);
      }
    } else if (mode === "migration") {
      if (previous !== null ||
          (stored && !sameClaimUsageContribution(stored, desired))) {
        fail("realm email alias claim counter migration is inconsistent", 503);
      }
    } else {
      fail("realm email alias claim counter transition mode is invalid", 503);
    }
    if (stored && desired &&
        (stored.account_id !== desired.account_id ||
          stored.realm_id !== desired.realm_id)) {
      fail("realm email alias claim counter ownership changed", 503);
    }
    if (!stored && !desired) return;

    const accountID = desired?.account_id ?? stored.account_id;
    const realmID = desired?.realm_id ?? stored.realm_id;
    const accountRaw = await storage.get(accountUsageKey(accountID));
    const realmRaw = await storage.get(realmUsageKey(accountID, realmID));
    const account = this.validateUsageRecord(
      accountRaw,
      "account",
      accountID,
    );
    const realm = this.validateUsageRecord(
      realmRaw,
      "realm",
      accountID,
      realmID,
    );
    const openDelta = (desired?.open_request ?? 0) -
      (stored?.open_request ?? 0);
    const allocatedDelta = (desired?.customer_allocated ?? 0) -
      (stored?.customer_allocated ?? 0);
    const pendingReviewDelta = (desired?.pending_review ?? 0) -
      (stored?.pending_review ?? 0);
    const provisioningDelta = (desired?.provisioning ?? 0) -
      (stored?.provisioning ?? 0);
    const accountOpen = account.open_requests + openDelta;
    const realmOpen = realm.open_requests + openDelta;
    const realmAllocated = realm.customer_allocated + allocatedDelta;
    const realmPendingReview = realm.pending_review + pendingReviewDelta;
    const realmProvisioning = realm.provisioning + provisioningDelta;
    if (![accountOpen, realmOpen, realmAllocated, realmPendingReview,
      realmProvisioning].every((value) =>
      Number.isSafeInteger(value) && value >= 0
    ) || realmOpen !== realmPendingReview + realmProvisioning) {
      fail("realm email alias pending counter transition is invalid", 503);
    }
    const nextAccount = withUsageRecordIntegrity({
      ...account,
      open_requests: accountOpen,
      updated_at: updatedAt,
    });
    const nextRealm = withUsageRecordIntegrity({
      ...realm,
      open_requests: realmOpen,
      pending_review: realmPendingReview,
      provisioning: realmProvisioning,
      customer_allocated: realmAllocated,
      updated_at: updatedAt,
    });
    await storage.put(accountUsageKey(accountID), nextAccount);
    await storage.put(realmUsageKey(accountID, realmID), nextRealm);
    if (desired) {
      await storage.put(memberKey, {
        schema_version: PENDING_COUNTER_SCHEMA_VERSION,
        ...desired,
        updated_at: updatedAt,
      });
      await storage.put(accountUsageMemberKey(accountID, claimID), claimID);
      await storage.put(
        realmUsageMemberKey(accountID, realmID, claimID),
        claimID,
      );
    } else {
      await storage.delete(memberKey);
      await storage.delete(accountUsageMemberKey(accountID, claimID));
      await storage.delete(realmUsageMemberKey(accountID, realmID, claimID));
    }
  }

  async prepareMutation(meta, actor, action, target, metadata = {}, options = {}) {
    const latest = await this.storage.get(META_KEY) ?? meta;
    const now = this.now().toISOString();
    const next = {
      ...latest,
      registry_revision: latest.registry_revision + 1,
      reserved_policy_version: options.reservedPolicyChange
        ? latest.reserved_policy_version + 1
        : latest.reserved_policy_version,
      audit_sequence: latest.audit_sequence + 1,
      updated_at: now,
    };
    const auditKey = `audit:${String(next.audit_sequence).padStart(12, "0")}`;
    const committedAudit = {
      sequence: next.audit_sequence,
      registry_revision: next.registry_revision,
      occurred_at: now,
      actor_kind: actor.kind,
      actor_id: actor.id,
      action,
      target,
      metadata: { ...metadata, phase: "committed" },
    };
    const preparedAudit = {
      ...committedAudit,
      action: `${action}.intent_recorded`,
      metadata: {
        ...metadata,
        phase: "prepared",
        requested_action: action,
      },
    };
    return {
      now,
      meta: next,
      entries: [[auditKey, committedAudit]],
      prepared_entries: [
        [META_KEY, next],
        [auditKey, preparedAudit],
      ],
    };
  }

  async mutationEntries(meta, actor, action, target, metadata = {}, options = {}) {
    // Only this tiny local section is global. It durably reserves unique
    // registry/audit revisions before any caller performs cell or KV I/O, then
    // releases immediately so a slow cell never head-of-line blocks the DO.
    return this.withLane("registry:metadata", async () => {
      const mutation = await this.prepareMutation(
        meta,
        actor,
        action,
        target,
        metadata,
        options,
      );
      await this.atomic([
        ...mutation.prepared_entries,
      ]);
      return mutation;
    });
  }

  async idempotent(scope, key, expectedFingerprint) {
    const record = await this.storage.get(`idem:${scope}:${key}`);
    if (!record) return null;
    if (record.fingerprint !== expectedFingerprint) {
      fail("idempotency_key was already used for a different request", 409);
    }
    // An idempotency replay is a read of a previously committed result, never
    // a projection repair. Replaying old approval or mutation keys while an
    // account lifecycle fence is active must not republish an applied route.
    // Projection intents, lifecycle intents, and route-refresh alarms own all
    // external healing with their exact durable fences.
    return json(record.body, record.status);
  }

  async cellTarget(accountID) {
    const route = await this.env?.DIRECTORY?.get(`acct:${accountID}`, {
      type: "json",
    });
    if (!route?.cell) fail("alias target account is not routed", 409);
    const cell = await this.env.DIRECTORY.get(`cell:${route.cell}`, {
      type: "json",
    });
    if (!cell?.endpoint || !cell?.provision_token) {
      fail("alias target cell is not configured", 502);
    }
    let endpoint;
    try {
      endpoint = new URL(cell.endpoint);
    } catch {
      fail("alias target cell endpoint is invalid", 502);
    }
    if (endpoint.protocol !== "https:" || endpoint.username || endpoint.password ||
        endpoint.search || endpoint.hash || !endpoint.hostname) {
      fail("alias target cell endpoint is invalid", 502);
    }
    const audience = typeof cell.agent_email_audience === "string" &&
        CELL_NAME_PATTERN.test(cell.agent_email_audience)
      ? cell.agent_email_audience
      : route.cell;
    if (!CELL_NAME_PATTERN.test(audience)) {
      fail("alias target cell audience is invalid", 502);
    }
    return {
      endpoint: cell.endpoint.replace(/\/+$/, ""),
      provision_token: cell.provision_token,
      cell_audience: audience,
      ingest_url: `${cell.endpoint.replace(/\/+$/, "")}/v1/internal/agent-email:ingest`,
    };
  }

  async fetchAuthoritativePlan(accountID, currentTarget = null) {
    const target = currentTarget ?? await this.cellTarget(accountID);
    let response;
    try {
      response = await this.fetchImpl(
        `${target.endpoint}/v1/accounts/${accountID}:plan`,
        {
          method: "GET",
          headers: { Authorization: `Bearer ${target.provision_token}` },
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch {
      fail("alias target cell plan is unreachable", 502);
    }
    const snapshot = await response.json().catch(() => null);
    if (!response.ok || snapshot?.account_id !== accountID ||
        !validPlanFence(snapshot?.revision, snapshot?.snapshot_hash) ||
        !Array.isArray(snapshot?.features) || !isObject(snapshot?.limits)) {
      fail("cell returned an invalid account plan snapshot", 502);
    }
    return snapshot;
  }

  async assertRealmTarget(accountID, realmID) {
    const target = await this.cellTarget(accountID);
    let response;
    try {
      const url = new URL(
        `${target.endpoint}/v1/accounts/${accountID}:email-realm-alias-target`,
      );
      url.searchParams.set("realm_id", realmID);
      response = await this.fetchImpl(url, {
        method: "GET",
        headers: { Authorization: `Bearer ${target.provision_token}` },
        signal: AbortSignal.timeout(15_000),
      });
    } catch {
      fail("alias target realm preflight is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (response.status === 404) {
      fail("alias target realm does not exist", 404);
    }
    if (!response.ok || body?.account_id !== accountID ||
        body?.realm_id !== realmID || body?.exists !== true) {
      fail("cell returned an invalid realm target preflight", 502);
    }
    return target;
  }

  async assertCurrentPlanFence(accountID, input) {
    if (!validPlanFence(input.plan_revision, input.plan_snapshot_hash)) {
      fail("invalid plan revision fence", 400);
    }
    const pending = await this.storage.get(planIntentKey(accountID));
    if (pending) {
      fail("account alias policy is still converging", 409);
    }
    const fence = await this.storage.get(planFenceKey(accountID));
    if (!fence) return;
    if (input.plan_revision !== fence.committed_revision ||
        input.plan_snapshot_hash !== fence.committed_snapshot_hash) {
      fail("account plan snapshot is stale for alias mutation", 409);
    }
  }

  async assertAccountAliasWritesAllowed(accountID) {
    if (await this.storage.get(lifecycleIntentKey(accountID))) {
      fail("account lifecycle is still converging; alias writes are fenced", 409);
    }
  }

  async assertRealmAliasWritesAllowed(accountID, realmID) {
    if (await this.storage.get(realmCloseIntentKey(accountID, realmID)) ||
        await this.storage.get(realmCloseFenceKey(accountID, realmID))) {
      fail("realm close is converging or complete; alias writes are fenced", 409);
    }
  }

  async getAliasClaimProof(input, normalizedAlias = null) {
    const accountID = typeof input?.account_id === "string"
      ? input.account_id
      : "";
    if (!ACCOUNT_ID_PATTERN.test(accountID)) fail("invalid account_id", 400);
    const alias = normalizedAlias ?? normalizeRealmEmailAlias(
      input?.realm_label,
    );
    const claim = await this.storage.get(claimKey(alias));
    if (!claim?.assignment_kind || claim.account_id !== accountID ||
        claim.customer_activation_intent === true ||
        claim.internal_intent === true) {
      fail("realm email alias claim not found", 404);
    }
    return json(this.customDomainClaimProof(claim));
  }

  customDomainSubscriptionProof(input) {
    try {
      return validateRealmEmailAliasClaimProof(
        input?.claim_proof,
        input?.claim_proof?.account_id,
        input?.claim_proof?.realm_label,
      );
    } catch {
      fail("custom-domain alias subscription proof is invalid", 400);
    }
  }

  async registerCustomDomainSubscription(input, validatedProof = null) {
    const proof = validatedProof ?? this.customDomainSubscriptionProof(input);
    const claim = await this.storage.get(claimKey(proof.realm_label));
    const currentProof = this.customDomainClaimProof(claim);
    if (!currentProof ||
        fingerprint(currentProof) !== fingerprint(proof)) {
      fail("custom-domain alias subscription proof is stale", 409);
    }
    const key = customDomainSubscriptionKey(proof.realm_alias_claim_id);
    const existing = await this.storage.get(key);
    const subscription = {
      schema_version: CUSTOM_DOMAIN_SUBSCRIPTION_SCHEMA,
      account_id: proof.account_id,
      realm_id: proof.realm_id,
      realm_label: proof.realm_label,
      realm_alias_claim_id: proof.realm_alias_claim_id,
      created_at: existing?.created_at ?? this.now().toISOString(),
    };
    if (existing && fingerprint(existing) !== fingerprint(subscription)) {
      fail("custom-domain alias subscription conflicts", 409);
    }
    if (!existing &&
        (await this.storage.get(realmCloseIntentKey(
          proof.account_id,
          proof.realm_id,
        )) || await this.storage.get(realmCloseFenceKey(
          proof.account_id,
          proof.realm_id,
        )))) {
      fail(
        "realm close is converging or complete; subscription is fenced",
        409,
        "custom_domain_subscription_realm_closed",
      );
    }
    if (!existing) {
      await this.atomic([
        [key, subscription],
        [customDomainSubscriptionRealmKey(subscription), proof.realm_label],
      ]);
    } else {
      // Repairable discovery indexes are rebuilt from the permanent marker.
      await this.atomicRaw([
        [customDomainSubscriptionRealmKey(existing), proof.realm_label],
      ]);
    }
    return json({
      schema_version: SCHEMA_VERSION,
      subscribed: true,
      account_id: proof.account_id,
      realm_id: proof.realm_id,
      realm_label: proof.realm_label,
      realm_alias_claim_id: proof.realm_alias_claim_id,
    });
  }

  async stageCustomDomainSyncForClaim(claim) {
    const proof = this.customDomainClaimProof(claim);
    if (!proof) return null;
    const subscription = await this.storage.get(
      customDomainSubscriptionKey(proof.realm_alias_claim_id),
    );
    if (!subscription) return null;
    if (subscription.account_id !== proof.account_id ||
        subscription.realm_id !== proof.realm_id ||
        subscription.realm_label !== proof.realm_label) {
      fail("custom-domain alias subscription is inconsistent", 503);
    }
    const key = customDomainSyncKey(proof.realm_alias_claim_id);
    const existing = await this.storage.get(key);
    const sourceFingerprint = realmEmailAliasClaimRouteFingerprint(proof);
    if (existing?.source_fingerprint === sourceFingerprint) return existing;
    const now = this.now();
    const intent = {
      schema_version: CUSTOM_DOMAIN_SYNC_SCHEMA,
      phase: "enqueue",
      claim_proof: proof,
      source_fingerprint: sourceFingerprint,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: existing?.created_at ?? now.toISOString(),
      updated_at: now.toISOString(),
    };
    await this.atomic([
      [key, intent],
      [customDomainSyncAccountKey(intent), key],
      [customDomainSyncDueKey(intent), proof.realm_alias_claim_id],
    ], [customDomainSyncDueKey(existing)].filter(Boolean));
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  async completeCustomDomainSync(intent) {
    await this.atomic([], [
      customDomainSyncKey(intent.claim_proof.realm_alias_claim_id),
      customDomainSyncAccountKey(intent),
      customDomainSyncDueKey(intent),
    ].filter(Boolean));
    await this.scheduleNextAlarm().catch(() => {});
    return { complete: true };
  }

  async callCustomDomainConvergence(path, intent) {
    const stub = agentEmailDomainRegistryStub(this.env);
    if (!stub) fail("custom-domain registry is unavailable", 503);
    let response;
    try {
      response = await stub.fetch(
        `https://agent-email-domain.internal${path}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            claim_proof: intent.claim_proof,
            source_fingerprint: intent.source_fingerprint,
          }),
        },
      );
    } catch {
      fail("custom-domain alias convergence is unreachable", 502);
    }
    return {
      response,
      body: await response.json().catch(() => null),
    };
  }

  async drainCustomDomainSyncIntent(startingIntent) {
    let intent = startingIntent;
    const key = customDomainSyncKey(
      intent.claim_proof.realm_alias_claim_id,
    );
    try {
      let result;
      if (intent.phase === "enqueue") {
        result = await this.callCustomDomainConvergence(
          "/route/alias-convergence/enqueue",
          intent,
        );
        if (result.response.ok && result.body?.complete === true &&
            result.body?.source_fingerprint === intent.source_fingerprint) {
          return this.completeCustomDomainSync(intent);
        }
        if (result.response.status !== 202 ||
            result.body?.complete !== false ||
            result.body?.source_fingerprint !== intent.source_fingerprint) {
          fail("custom-domain alias convergence enqueue failed", 502);
        }
        const continued = {
          ...intent,
          phase: "poll",
          failure_count: 0,
          retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [key, continued],
          [customDomainSyncDueKey(continued),
            continued.claim_proof.realm_alias_claim_id],
        ], [customDomainSyncDueKey(intent)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return { complete: false };
      }
      if (intent.phase !== "poll") {
        fail("custom-domain alias convergence intent is invalid", 503);
      }
      result = await this.callCustomDomainConvergence(
        "/route/alias-convergence/status",
        intent,
      );
      if (result.response.ok && result.body?.complete === true &&
          result.body?.source_fingerprint === intent.source_fingerprint) {
        return this.completeCustomDomainSync(intent);
      }
      if (result.response.status === 404) {
        const continued = {
          ...intent,
          phase: "enqueue",
          failure_count: 0,
          retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [key, continued],
          [customDomainSyncDueKey(continued),
            continued.claim_proof.realm_alias_claim_id],
        ], [customDomainSyncDueKey(intent)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
        return { complete: false };
      }
      if (result.response.status !== 202 || result.body?.complete !== false ||
          result.body?.source_fingerprint !== intent.source_fingerprint) {
        fail("custom-domain alias convergence status failed", 502);
      }
      const continued = {
        ...intent,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.atomic([
        [key, continued],
        [customDomainSyncDueKey(continued),
          continued.claim_proof.realm_alias_claim_id],
      ], [customDomainSyncDueKey(intent)].filter(Boolean));
      await this.scheduleNextAlarm().catch(() => {});
      return { complete: false };
    } catch (error) {
      const current = await this.storage.get(key);
      if (current?.source_fingerprint === intent.source_fingerprint) {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [key, retry],
          [customDomainSyncDueKey(retry),
            retry.claim_proof.realm_alias_claim_id],
        ], [customDomainSyncDueKey(current)].filter(Boolean));
        await this.scheduleNextAlarm().catch(() => {});
      }
      throw error;
    }
  }

  async reconcileDueCustomDomainSyncs() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "custom-domain-sync-due:",
      limit: REALM_CLOSE_ALARM_BATCH_LIMIT,
    });
    for (const [due, claimID] of listed) {
      const retryAt = Number(due.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || retryAt > now) break;
      const intent = CLAIM_ID_PATTERN.test(claimID ?? "")
        ? await this.storage.get(customDomainSyncKey(claimID))
        : null;
      if (!intent || customDomainSyncDueKey(intent) !== due) {
        await this.storage.delete(due);
        continue;
      }
      const proof = intent.claim_proof;
      await this.withLanes([
        `account:${proof.account_id}`,
        `realm:${proof.account_id}:${proof.realm_id}`,
        `skeleton:${realmEmailAliasSkeleton(proof.realm_label)}`,
      ], () => this.drainCustomDomainSyncIntent(intent).catch(() => {}));
    }
  }

  assertCellAcknowledgement(body, claim, state) {
    if (!isObject(body) || body.claim_id !== claim.claim_id ||
        body.account_id !== claim.account_id || body.realm_id !== claim.realm_id ||
        body.domain !== claim.domain || body.realm_label !== claim.alias ||
        body.state !== state ||
        body.controller_revision !== claim.assignment_revision) {
      fail("cell returned an inconsistent realm alias acknowledgement", 502);
    }
  }

  async applyAndVerifyCell(claim) {
    const target = await this.cellTarget(claim.account_id);
    const state = cellAliasState(claim);
    const payload = {
      claim_id: claim.claim_id,
      realm_id: claim.realm_id,
      domain: claim.domain,
      realm_label: claim.alias,
      state,
      controller_revision: claim.assignment_revision,
    };
    let response;
    try {
      response = await this.fetchImpl(
        `${target.endpoint}/v1/accounts/${claim.account_id}:email-realm-alias`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${target.provision_token}`,
          },
          body: JSON.stringify(payload),
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch {
      fail("alias target cell is unreachable", 502);
    }
    const applied = await response.json().catch(() => null);
    if (!response.ok) {
      fail(
        response.status === 404
          ? "cell realm alias target no longer exists"
          : response.status === 409
          ? "cell rejected a stale or conflicting realm alias projection"
          : "cell rejected realm alias projection",
        response.status === 404 ? 404 : response.status === 409 ? 409 : 502,
      );
    }
    this.assertCellAcknowledgement(applied, claim, state);
    let fence;
    try {
      const verified = await this.fetchImpl(
        `${target.endpoint}/v1/accounts/${claim.account_id}:email-realm-alias?claim_id=${claim.claim_id}`,
        {
          method: "GET",
          headers: { Authorization: `Bearer ${target.provision_token}` },
          signal: AbortSignal.timeout(15_000),
        },
      );
      fence = await verified.json().catch(() => null);
      if (!verified.ok) fail("cell realm alias fence verification failed", 502);
    } catch (error) {
      if (error instanceof RegistryError) throw error;
      fail("cell realm alias fence verification failed", 502);
    }
    this.assertCellAcknowledgement(fence, claim, state);
    return target;
  }

  routingDirectory() {
    if (!this.env?.AGENT_EMAIL_DIRECTORY ||
        typeof this.env.AGENT_EMAIL_DIRECTORY.put !== "function") {
      fail("isolated agent email routing directory is unavailable", 503);
    }
    return this.env.AGENT_EMAIL_DIRECTORY;
  }

  async signedRouteProjection(value) {
    try {
      return await this.signRouteProjection(value);
    } catch (error) {
      if (error instanceof RegistryError) throw error;
      fail(
        "agent email route signing is unavailable",
        503,
        "agent_email_route_signing_unavailable",
      );
    }
  }

  async publishRoute(value) {
    let signed;
    try {
      signed = await this.signedRouteProjection(value);
      await this.routingDirectory().put(
        realmEmailRouteKey(value.domain, value.realm_label),
        JSON.stringify(signed),
      );
    } catch (error) {
      if (error instanceof RegistryError) throw error;
      fail("agent email routing projection failed", 502);
    }
    return signed;
  }

  async publishClaimRoute(claim, _meta, target, updatedAt = this.now().toISOString()) {
    if (!claim?.assignment_kind) return null;
    const state = cellAliasState(claim);
    const value = buildRealmEmailRouteProjection({
      account_id: claim.account_id,
      domain: claim.domain,
      realm_id: claim.realm_id,
      realm_label: claim.alias,
      route_kind: "realm_alias",
      state,
      ...(state === "suspended"
        ? { suspension_disposition: routeSuspensionDisposition(claim) }
        : {}),
      controller_revision: claim.assignment_revision,
      updated_at: updatedAt,
      cell_audience: target?.cell_audience,
      ingest_url: target?.ingest_url,
    });
    return this.publishRoute(value);
  }

  async fetchCellCanonicalRoute(accountID, realmID, target = null) {
    const destination = target ?? await this.cellTarget(accountID);
    let response;
    try {
      response = await this.fetchImpl(
        cellRealmRouteGetURL(destination.endpoint, accountID, realmID),
        {
          method: "GET",
          headers: { Authorization: `Bearer ${destination.provision_token}` },
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch {
      fail("canonical realm route target is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (response.status === 404) fail("canonical realm route not found", 404);
    if (!response.ok) fail("cell rejected canonical realm route lookup", 502);
    return validateCellCanonicalRoute(body, accountID, realmID);
  }

  canonicalProjection(canonical, updatedAt = canonical.updated_at) {
    return buildRealmEmailRouteProjection({
      ...canonical,
      updated_at: updatedAt,
      route_kind: "canonical",
      ...(canonical.state === "suspended"
        ? { suspension_disposition: canonical.suspension_disposition }
        : {}),
      cell_audience: canonical.cell_audience,
      ingest_url: canonical.ingest_url,
    });
  }

  async publishStoredRetiredCanonicalRoute(reference) {
    const domain = validateManagedRealmEmailDomain(reference?.domain);
    const realmID = reference?.realm_id;
    if (!REALM_ID_PATTERN.test(realmID ?? "") ||
        reference?.realm_label !== realmID.slice("realm_".length) ||
        reference?.state !== "retired") {
      fail("retired canonical realm route reference is invalid", 503);
    }
    return this.withLane(
      canonicalRouteLaneKey(domain, realmID),
      () => this.publishStoredRetiredCanonicalRouteWithLaneHeld({
        domain,
        realm_id: realmID,
      }),
    );
  }

  // This helper renews only terminal deny authority. Live destinations must
  // always pass through upsertCanonicalRoute and its cell/entitlement checks.
  async publishStoredRetiredCanonicalRouteWithLaneHeld(reference) {
    const key = canonicalRouteAuthorityKey(
      reference.domain,
      reference.realm_id,
    );
    const current = await this.storage.get(key);
    if (!current || current.domain !== reference.domain ||
        current.realm_id !== reference.realm_id || current.state !== "retired") {
      fail("retired canonical realm route authority is inconsistent", 503);
    }
    const checkedAt = this.now();
    const refreshFreshness = canonicalRouteAuthorityShouldRenew(
      current,
      checkedAt.valueOf(),
    );
    const canonical = refreshFreshness
      ? { ...current, updated_at: checkedAt.toISOString() }
      : current;
    // Validate the exact terminal projection before changing durable state.
    this.canonicalProjection(canonical);
    if (refreshFreshness) {
      await this.atomic([
        [key, canonical],
        [accountCanonicalIndexKey(canonical), key],
        [realmCanonicalIndexKey(canonical), key],
      ]);
    }
    await this.publishRoute(this.canonicalProjection(canonical));
    return canonical;
  }

  async upsertCanonicalRoute(input) {
    const domain = validateManagedRealmEmailDomain(input?.domain);
    const source = validateCellCanonicalRoute(
      input?.cellRoute,
      input?.cellRoute?.account_id,
      input?.cellRoute?.realm_id,
    );
    return this.withLane(
      canonicalRouteLaneKey(domain, source.realm_id),
      () => this.upsertCanonicalRouteWithLaneHeld({
        ...input,
        domain,
        cellRoute: source,
      }),
    );
  }

  // Only a caller already holding canonicalRouteLaneKey(domain, realm_id) may
  // use this core. Ordinary writers must use the lane-owning public wrapper.
  async upsertCanonicalRouteWithLaneHeld({
    domain,
    cellRoute,
    target = null,
    emailEnabled = false,
    deliveryEnabled = realmEmailCanonicalDeliveryEnabled(this.env),
    forcedPolicy = null,
    minimumControllerRevision = 1,
    lifecycleFence = null,
  }) {
    const canonicalDomain = validateManagedRealmEmailDomain(domain);
    const source = validateCellCanonicalRoute(
      cellRoute,
      cellRoute?.account_id,
      cellRoute?.realm_id,
    );
    const realmLabel = source.realm_id.slice("realm_".length);
    const key = canonicalRouteAuthorityKey(canonicalDomain, source.realm_id);
    const current = await this.storage.get(key);
    if (current &&
        (current.domain !== canonicalDomain ||
          current.account_id !== source.account_id ||
          current.realm_id !== source.realm_id ||
          current.realm_label !== realmLabel)) {
      fail("canonical realm route ownership collision", 409);
    }
    const forcedPolicyKeys = isObject(forcedPolicy)
      ? Object.keys(forcedPolicy).sort()
      : [];
    const exactForcedRetiredPolicy =
      forcedPolicyKeys.length === 2 &&
      forcedPolicyKeys[0] === "state" &&
      forcedPolicyKeys[1] === "suspension_disposition" &&
      forcedPolicy.state === "retired" &&
      forcedPolicy.suspension_disposition === null;
    // A lost KV acknowledgement can leave the durable authority tombstoned
    // while the cell is still at the exact prepared `closing` fence. Replaying
    // that same forced-retired projection is repair, not resurrection. Keep
    // every ownership, generation, operation, cell-state, and policy byte
    // constrained so no other retired -> non-retired source transition passes.
    const closingTombstoneReplayCandidate = current?.state === "retired" &&
      current.cell_state === "closing" &&
      source.state === "closing" &&
      Number.isSafeInteger(current.cell_generation) &&
      current.cell_generation === source.generation &&
      typeof current.cell_operation_id === "string" &&
      current.cell_operation_id === source.operation_id &&
      exactForcedRetiredPolicy;
    const currentGeneration = Number.isSafeInteger(current?.cell_generation)
      ? current.cell_generation
      : 0;
    if (current && source.generation < currentGeneration) {
      fail("canonical realm route generation is stale", 409);
    }
    if (current && currentGeneration > 0 &&
        source.generation === currentGeneration &&
        (source.state !== current.cell_state ||
          source.operation_id !== (current.cell_operation_id ?? null))) {
      const terminalCommit = current.cell_state === "closing" &&
        source.state === "retired" &&
        source.operation_id === current.cell_operation_id;
      if (!terminalCommit) {
        fail("canonical realm route generation conflicts", 409);
      }
    }
    if (current?.cell_operation_id && source.operation_id &&
        current.cell_operation_id !== source.operation_id) {
      fail("canonical realm route operation conflicts", 409);
    }

    const policy = forcedPolicy ?? canonicalRoutingPolicy(
      source,
      emailEnabled,
      deliveryEnabled,
    );
    if (!isObject(policy) ||
        !["applied", "suspended", "retired"].includes(policy.state) ||
        (policy.state === "suspended" &&
          !["retry", "inactive"].includes(policy.suspension_disposition)) ||
        (source.state === "retired" && policy.state !== "retired") ||
        (source.state === "closing" && policy.state === "applied") ||
        (policy.state === "retired" && source.state === "live")) {
      fail("canonical realm route policy is invalid", 500);
    }
    const appliedTarget = policy.state === "applied"
      ? target ?? await this.cellTarget(source.account_id)
      : null;
    const authority = {
      domain: canonicalDomain,
      account_id: source.account_id,
      realm_id: source.realm_id,
      realm_label: realmLabel,
      state: policy.state,
      ...(policy.state === "suspended"
        ? { suspension_disposition: policy.suspension_disposition }
        : {}),
      cell_state: source.state,
      cell_generation: source.generation,
      cell_operation_id: source.operation_id,
      ...(lifecycleFence
        ? { lifecycle_fence: lifecycleFence }
        : current?.lifecycle_fence
        ? { lifecycle_fence: current.lifecycle_fence }
        : {}),
      ...(policy.state === "applied"
        ? {
          cell_audience: appliedTarget.cell_audience,
          ingest_url: appliedTarget.ingest_url,
        }
        : {}),
    };
    const comparable = (value) => JSON.stringify({
      domain: value?.domain,
      account_id: value?.account_id,
      realm_id: value?.realm_id,
      realm_label: value?.realm_label,
      state: value?.state,
      suspension_disposition: value?.suspension_disposition ?? null,
      cell_state: value?.cell_state,
      cell_generation: value?.cell_generation,
      cell_operation_id: value?.cell_operation_id ?? null,
      cell_audience: value?.cell_audience ?? null,
      ingest_url: value?.ingest_url ?? null,
      lifecycle_fence: value?.lifecycle_fence ?? null,
    });
    const exactClosingTombstoneReplay = closingTombstoneReplayCandidate &&
      comparable(current) === comparable(authority);
    if (current?.state === "retired" && source.state !== "retired" &&
        !exactClosingTombstoneReplay) {
      fail("retired canonical realm routes cannot be resurrected", 409);
    }
    const changed = !current || comparable(current) !== comparable(authority);
    const currentControllerRevision = Number.isSafeInteger(
        current?.controller_revision,
      ) && current.controller_revision >= 1
      ? current.controller_revision
      : 0;
    const controllerRevision = current
      ? changed ? currentControllerRevision + 1 : currentControllerRevision
      : Math.max(1, minimumControllerRevision);
    if (!Number.isSafeInteger(controllerRevision) || controllerRevision < 1) {
      fail("canonical realm route revision is exhausted", 503);
    }
    const checkedAt = this.now();
    const refreshFreshness = current && !changed &&
      canonicalRouteAuthorityShouldRenew(current, checkedAt.valueOf());
    const authorityChanged = changed || refreshFreshness;
    const canonical = changed
      ? {
        ...authority,
        controller_revision: controllerRevision,
        updated_at: checkedAt.toISOString(),
      }
      : refreshFreshness
      ? { ...current, updated_at: checkedAt.toISOString() }
      : current;
    // The durable authority after-image must exist before its eventually
    // consistent routing projection can become externally visible. If the KV
    // acknowledgement is lost, an immediate retry reuses the exact bytes at
    // the same controller revision. A validated source read may renew only
    // updated_at without minting a synthetic authority revision. Background
    // writers renew at a TTL-derived half-life so a five-minute inventory
    // cadence keeps the five-minute edge authority continuously warm.
    await this.atomic(authorityChanged
      ? [
        [key, canonical],
        [accountCanonicalIndexKey(canonical), key],
        [realmCanonicalIndexKey(canonical), key],
      ]
      : [
        [accountCanonicalIndexKey(canonical), key],
        [realmCanonicalIndexKey(canonical), key],
      ]);
    await this.publishRoute(this.canonicalProjection(canonical));
    return canonical;
  }

  async ensureCanonicalRoute(claim, meta, target, desiredState = "applied") {
    const source = await this.fetchCellCanonicalRoute(
      claim.account_id,
      claim.realm_id,
      target,
    );
    const emailEnabled = await this.canonicalEmailEntitlement(
      claim.account_id,
      target,
    );
    const canonical = await this.upsertCanonicalRoute({
      domain: claim.domain,
      cellRoute: source,
      target,
      emailEnabled,
      ...(desiredState === "suspended" && source.state !== "retired"
        ? {
          forcedPolicy: {
            state: "suspended",
            suspension_disposition: "retry",
          },
        }
        : {}),
      minimumControllerRevision: meta.registry_revision,
    });
    return this.canonicalProjection(canonical);
  }

  async syncClaimProjection(
    claim,
    meta,
    includeCanonical = true,
    canonicalState = "applied",
  ) {
    const target = await this.applyAndVerifyCell(claim);
    const route = await this.publishClaimRoute(claim, meta, target);
    if (includeCanonical && realmEmailCanonicalInventoryEnabled(this.env)) {
      await this.ensureCanonicalRoute(claim, meta, target, canonicalState);
    }
    return route;
  }

  async enqueueRouteRefresh({ domain, realmLabel, accountID, kind, alias = null }) {
    const key = routeRefreshKey(domain, realmLabel);
    const existing = await this.storage.get(key);
    if (existing) {
      await this.scheduleNextAlarm().catch(() => {});
      return existing;
    }
    const now = this.now();
    const intent = {
      domain,
      realm_label: realmLabel,
      account_id: accountID,
      kind,
      alias,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: now.toISOString(),
      updated_at: now.toISOString(),
    };
    await this.atomic([
      [key, intent],
      [routeRefreshDueKey(intent), key],
    ]);
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  async getRoute(input) {
    const domain = validateManagedRealmEmailDomain(input?.domain);
    if (!managedRealmEmailDomains(this.env).includes(domain)) {
      fail("realm email route not found", 404);
    }
    const realmLabel = typeof input?.realm_label === "string"
      ? input.realm_label
      : "";
    realmEmailRouteKey(domain, realmLabel);
    if (CANONICAL_REALM_LABEL_PATTERN.test(realmLabel)) {
      const canonical = await this.storage.get(
        `canonical:${domain}:${realmLabel}`,
      );
      if (!canonical) fail("realm email route not found", 404);
      const [lifecycleIntent, planIntent] = await Promise.all([
        this.storage.get(lifecycleIntentKey(canonical.account_id)),
        this.storage.get(planIntentKey(canonical.account_id)),
      ]);
      if (lifecycleIntent || planIntent) {
        fail("canonical realm route policy is still converging", 409);
      }
      let admitted;
      try {
        admitted = managedDeliveryAccountIsAdmitted(
          this.env,
          canonical.account_id,
        );
      } catch {
        fail(
          "managed email delivery cohort is unavailable",
          503,
          "managed_email_delivery_cohort_invalid",
        );
      }
      if (!admitted) {
        fail(
          "managed email route is held back",
          409,
          "managed_email_delivery_cohort_held_back",
        );
      }
      if (!canonicalRouteAuthorityIsFresh(
        canonical,
        this.now().valueOf(),
      )) {
        let refreshed;
        if (canonical.state === "retired") {
          refreshed = await this.publishStoredRetiredCanonicalRouteWithLaneHeld({
            domain,
            realm_id: canonical.realm_id,
          });
        } else {
          const target = await this.cellTarget(canonical.account_id);
          const [source, emailEnabled] = await Promise.all([
            this.fetchCellCanonicalRoute(
              canonical.account_id,
              canonical.realm_id,
              target,
            ),
            this.canonicalEmailEntitlement(canonical.account_id, target),
          ]);
          refreshed = await this.upsertCanonicalRouteWithLaneHeld({
            domain,
            cellRoute: source,
            target,
            emailEnabled,
          });
        }
        return json(await this.signedRouteProjection(
          this.canonicalProjection(refreshed),
        ));
      }
      const value = this.canonicalProjection(canonical);
      await this.enqueueRouteRefresh({
        domain,
        realmLabel,
        accountID: canonical.account_id,
        kind: "canonical",
      });
      return json(await this.signedRouteProjection(value));
    }

    const claim = await this.storage.get(claimKey(realmLabel));
    if (!claim?.assignment_kind || claim.domain !== domain) {
      fail("realm email route not found", 404);
    }
    let admitted;
    try {
      admitted = managedDeliveryAccountIsAdmitted(this.env, claim.account_id);
    } catch {
      fail(
        "managed email delivery cohort is unavailable",
        503,
        "managed_email_delivery_cohort_invalid",
      );
    }
    if (!admitted) {
      fail(
        "managed email route is held back",
        409,
        "managed_email_delivery_cohort_held_back",
      );
    }
    // The durable claim fence is authoritative; DIRECTORY supplies only the
    // account's current target. A tiny durable refresh intent is queued before
    // returning so expired/missing KV repairs asynchronously without making
    // this latency-sensitive read perform cell I/O or an edge KV write.
    const target = cellAliasState(claim) === "applied"
      ? await this.cellTarget(claim.account_id)
      : null;
    const state = cellAliasState(claim);
    const value = buildRealmEmailRouteProjection({
      account_id: claim.account_id,
      domain: claim.domain,
      realm_id: claim.realm_id,
      realm_label: claim.alias,
      route_kind: "realm_alias",
      state,
      ...(state === "suspended"
        ? { suspension_disposition: routeSuspensionDisposition(claim) }
        : {}),
      controller_revision: claim.assignment_revision,
      updated_at: this.now().toISOString(),
      cell_audience: target?.cell_audience,
      ingest_url: target?.ingest_url,
    });
    await this.enqueueRouteRefresh({
      domain,
      realmLabel,
      accountID: claim.account_id,
      kind: "realm_alias",
      alias: claim.alias,
    });
    return json(await this.signedRouteProjection(value));
  }

  async fetchCellCanonicalPage(accountID, target, cursor = null) {
    let response;
    try {
      response = await this.fetchImpl(
        cellRealmRouteListURL(
          target.endpoint,
          accountID,
          cursor,
          CELL_REALM_ROUTE_PAGE_LIMIT,
        ),
        {
          method: "GET",
          headers: { Authorization: `Bearer ${target.provision_token}` },
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch {
      fail("canonical realm route inventory is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (!response.ok || body?.schema_version !== "witself.v0" ||
        body?.account_id !== accountID || !Array.isArray(body?.routes) ||
        body.routes.length > CELL_REALM_ROUTE_PAGE_LIMIT ||
        !(body.next_cursor === null ||
          (typeof body.next_cursor === "string" &&
            body.next_cursor.length >= 1 && body.next_cursor.length <= 2_048 &&
            body.next_cursor !== cursor))) {
      fail("cell returned an invalid canonical realm route page", 502);
    }
    const routes = body.routes.map((route) =>
      validateCellCanonicalRoute(route, accountID)
    );
    for (let index = 1; index < routes.length; index += 1) {
      if (routes[index - 1].realm_id >= routes[index].realm_id) {
        fail("cell canonical realm route page is not strictly ordered", 502);
      }
    }
    if (body.next_cursor !== null && routes.length === 0) {
      fail("cell canonical realm route page did not advance", 502);
    }
    return { routes, next_cursor: body.next_cursor };
  }

  async canonicalEmailEntitlement(accountID, currentTarget = null) {
    const snapshot = await this.fetchAuthoritativePlan(accountID, currentTarget);
    return snapshot.features.includes(AGENT_EMAIL_RECEIVE_FEATURE);
  }

  async reconcileCanonicalInventory() {
    if (!realmEmailCanonicalInventoryEnabled(this.env)) {
      fail("canonical realm route inventory is not enabled", 409);
    }
    if (!this.env?.DIRECTORY || typeof this.env.DIRECTORY.list !== "function") {
      fail("canonical realm route inventory directory is unavailable", 503);
    }
    let scan = await this.storage.get(CANONICAL_INVENTORY_KEY);
    if (scan == null) {
      scan = {
        schema_version: CANONICAL_INVENTORY_SCHEMA,
        directory_cursor: null,
        next_directory_cursor: null,
        account_id: null,
        cell_cursor: null,
        cycle: 0,
      };
    } else if (!isObject(scan) ||
        scan.schema_version !== CANONICAL_INVENTORY_SCHEMA ||
        !(scan.directory_cursor === null ||
          (typeof scan.directory_cursor === "string" &&
            scan.directory_cursor.length <= 2_048)) ||
        !(scan.account_id === null ||
          ACCOUNT_ID_PATTERN.test(scan.account_id)) ||
        !(scan.cell_cursor === null ||
          (typeof scan.cell_cursor === "string" && scan.cell_cursor.length <= 2_048)) ||
        !Number.isSafeInteger(scan.cycle) || scan.cycle < 0) {
      fail("canonical realm route inventory cursor is invalid", 503);
    }

    if (!scan.account_id) {
      const page = await this.env.DIRECTORY.list({
        prefix: "acct:",
        limit: CANONICAL_INVENTORY_DIRECTORY_LIMIT,
        ...(scan.directory_cursor ? { cursor: scan.directory_cursor } : {}),
      });
      if (!Array.isArray(page?.keys) ||
          page.keys.length > CANONICAL_INVENTORY_DIRECTORY_LIMIT ||
          typeof page.list_complete !== "boolean" ||
          (!page.list_complete &&
            (typeof page.cursor !== "string" || page.cursor.length < 1 ||
              page.cursor.length > 2_048 || page.cursor === scan.directory_cursor))) {
        fail("directory returned an invalid canonical inventory page", 502);
      }
      if (page.keys.length === 0) {
        if (!page.list_complete) {
          fail("directory canonical inventory page did not advance", 502);
        }
        scan = {
          ...scan,
          directory_cursor: null,
          next_directory_cursor: null,
          account_id: null,
          cell_cursor: null,
          cycle: scan.cycle + 1,
          updated_at: this.now().toISOString(),
        };
        await this.storage.put(CANONICAL_INVENTORY_KEY, scan);
        return json({
          schema_version: SCHEMA_VERSION,
          complete: true,
          cycle: scan.cycle,
          accounts_scanned: 0,
          routes_scanned: 0,
        });
      }
      const name = page.keys[0]?.name;
      const accountID = typeof name === "string" && name.startsWith("acct:")
        ? name.slice("acct:".length)
        : "";
      if (!ACCOUNT_ID_PATTERN.test(accountID)) {
        fail("directory returned an invalid account route key", 502);
      }
      scan = {
        ...scan,
        account_id: accountID,
        cell_cursor: null,
        next_directory_cursor: page.list_complete ? null : page.cursor,
      };
    }

    return this.withLane(
      `account:${scan.account_id}`,
      () => this.reconcileCanonicalInventoryAccountWithLaneHeld(scan),
    );
  }

  // The account lane fences directory placement, cell inventory, entitlement,
  // and every nested canonical write against plan/lifecycle convergence.
  async reconcileCanonicalInventoryAccountWithLaneHeld(scan) {
    const accountID = scan.account_id;
    const [lifecycleIntent, planIntent] = await Promise.all([
      this.storage.get(lifecycleIntentKey(accountID)),
      this.storage.get(planIntentKey(accountID)),
    ]);
    if (lifecycleIntent || planIntent) {
      fail("canonical inventory account policy is still converging", 409);
    }
    const [route, pending, archived] = await Promise.all([
      this.env.DIRECTORY.get(`acct:${accountID}`, { type: "json" }),
      this.env.DIRECTORY.get(`pending:${accountID}`),
      this.env.DIRECTORY.get(`archived:${accountID}`),
    ]);
    if (!route?.cell || pending || archived) {
      const complete = scan.next_directory_cursor === null;
      const continued = {
        ...scan,
        directory_cursor: complete ? null : scan.next_directory_cursor,
        next_directory_cursor: null,
        account_id: null,
        cell_cursor: null,
        cycle: scan.cycle + (complete ? 1 : 0),
        updated_at: this.now().toISOString(),
      };
      await this.storage.put(CANONICAL_INVENTORY_KEY, continued);
      return json({
        schema_version: SCHEMA_VERSION,
        complete,
        cycle: continued.cycle,
        accounts_scanned: 1,
        routes_scanned: 0,
      });
    }

    const target = await this.cellTarget(accountID);
    const [page, emailEnabled] = await Promise.all([
      this.fetchCellCanonicalPage(accountID, target, scan.cell_cursor),
      this.canonicalEmailEntitlement(accountID, target),
    ]);
    const managedDomains = managedRealmEmailDomains(this.env);
    for (const cellRoute of page.routes) {
      for (const domain of managedDomains) {
        await this.upsertCanonicalRoute({
          domain,
          cellRoute,
          target,
          emailEnabled,
        });
      }
    }
    const accountComplete = page.next_cursor === null;
    const complete = accountComplete && scan.next_directory_cursor === null;
    const continued = {
      ...scan,
      directory_cursor: accountComplete
        ? complete ? null : scan.next_directory_cursor
        : scan.directory_cursor,
      next_directory_cursor: accountComplete ? null : scan.next_directory_cursor,
      account_id: accountComplete ? null : accountID,
      cell_cursor: accountComplete ? null : page.next_cursor,
      cycle: scan.cycle + (complete ? 1 : 0),
      updated_at: this.now().toISOString(),
    };
    await this.storage.put(CANONICAL_INVENTORY_KEY, continued);
    return json({
      schema_version: SCHEMA_VERSION,
      complete,
      cycle: continued.cycle,
      accounts_scanned: accountComplete ? 1 : 0,
      routes_scanned: page.routes.length,
      projections_published: page.routes.length * managedDomains.length,
      account_complete: accountComplete,
    });
  }

  async assertRealmHasNoLiveAliases(accountID, realmID, cursor = null) {
    const prefix = accountClaimIndexPrefix(accountID, realmID);
    const listed = await this.storage.list({
      prefix,
      limit: REALM_CLOSE_CLAIM_PAGE_LIMIT + 1,
      ...(cursor ? { startAfter: cursor } : {}),
    });
    const entries = [...listed.entries()];
    const page = entries.slice(0, REALM_CLOSE_CLAIM_PAGE_LIMIT);
    for (const [, alias] of page) {
      const claim = await this.storage.get(claimKey(alias));
      if (!claim || claim.account_id !== accountID || claim.realm_id !== realmID) {
        fail("realm alias index is inconsistent", 503);
      }
      if (!claim.retired_at) {
        fail("realm has a live or pending email alias", 409);
      }
    }
    return entries.length > REALM_CLOSE_CLAIM_PAGE_LIMIT
      ? page.at(-1)[0]
      : null;
  }

  async postCellCanonicalTransition(target, url, payload, accountID, realmID) {
    let response;
    try {
      response = await this.fetchImpl(url, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${target.provision_token}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(15_000),
      });
    } catch {
      fail("canonical realm close target is unreachable", 502);
    }
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      fail(
        response.status === 404
          ? "canonical realm close target not found"
          : response.status === 409
          ? "canonical realm close is blocked by live resources or a stale fence"
          : "cell rejected canonical realm close",
        response.status === 404 ? 404 : response.status === 409 ? 409 : 502,
      );
    }
    return validateCellCanonicalRoute(body, accountID, realmID);
  }

  async closeRealm(input) {
    validateAccountRealm(input);
    const actor = isObject(input.actor) && input.actor.kind === "platform_admin"
      ? validateActor(input.actor, "platform_admin")
      : validateActor(input.actor, "account_operator");
    const key = validateIdempotencyKey(input.idempotency_key);
    const domains = managedRealmEmailDomains(this.env);
    const domain = validateManagedRealmEmailDomain(input.domain);
    if (domain !== domains[0]) {
      fail("realm close must target the primary managed email domain", 400);
    }
    const scope = `realm-close:${input.account_id}:${input.realm_id}`;
    const fp = fingerprint({
      action: "realm_close",
      account_id: input.account_id,
      realm_id: input.realm_id,
      domain,
    });
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    const fence = await this.storage.get(
      realmCloseFenceKey(input.account_id, input.realm_id),
    );
    if (fence) {
      fail("realm is already permanently closed", 409);
    }
    let intent = await this.storage.get(
      realmCloseIntentKey(input.account_id, input.realm_id),
    );
    if (intent &&
        (intent.idempotency_key !== key || intent.fingerprint !== fp)) {
      fail("another realm close is already converging", 409);
    }
    if (!intent) {
      const now = this.now();
      intent = {
        schema_version: SCHEMA_VERSION,
        account_id: input.account_id,
        realm_id: input.realm_id,
        domain,
        domains,
        actor,
        idempotency_key: key,
        fingerprint: fp,
        phase: "scan_aliases",
        alias_cursor: null,
        custom_domain_cursor: null,
        failure_count: 0,
        retry_at_ms: now.getTime(),
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      };
      await this.atomic([
        [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
        [realmCloseDueKey(intent), `${intent.account_id}:${intent.realm_id}`],
      ]);
      await this.scheduleNextAlarm().catch(() => {});
    }
    return this.drainRealmCloseIntent(intent);
  }

  async getRealmClose(input) {
    validateAccountRealm(input);
    const intent = await this.storage.get(
      realmCloseIntentKey(input.account_id, input.realm_id),
    );
    const fence = await this.storage.get(
      realmCloseFenceKey(input.account_id, input.realm_id),
    );
    if (!intent && !fence) fail("realm close not found", 404);
    return json({
      schema_version: SCHEMA_VERSION,
      account_id: input.account_id,
      realm_id: input.realm_id,
      complete: Boolean(fence),
      phase: fence ? "complete" : intent.phase,
    }, fence ? 200 : 202);
  }

  realmCloseIntentDomains(intent) {
    const originalDomain = validateManagedRealmEmailDomain(intent?.domain);
    if (Array.isArray(intent?.domains)) {
      const persisted = intent.domains.map((domain) =>
        validateManagedRealmEmailDomain(domain)
      );
      if (persisted.length < 1 ||
          persisted.length > REALM_EMAIL_MAX_MANAGED_DOMAINS ||
          new Set(persisted).size !== persisted.length ||
          persisted[0] !== originalDomain) {
        fail("canonical realm close domain set is invalid", 503);
      }
      return persisted;
    }

    // Old intents stored only their original primary domain. Use the current
    // bounded configured set once, with that original domain first, so both
    // lane acquisition and the durable upgrade cover exactly the domains that
    // drainRealmCloseIntent is allowed to mutate.
    const configured = managedRealmEmailDomains(this.env);
    if (!configured.includes(originalDomain)) {
      fail("legacy canonical realm close domain is no longer managed", 503);
    }
    return [
      originalDomain,
      ...configured.filter((domain) => domain !== originalDomain),
    ];
  }

  async drainRealmCloseIntent(startingIntent) {
    let intent = startingIntent;
    try {
      const domains = this.realmCloseIntentDomains(intent);
      if (!Array.isArray(intent.domains)) {
        // Legacy single-domain intents predate the bounded compatibility set.
        // Freeze the currently configured set into the durable intent before
        // doing more external work so every retry and journal replay sees the
        // same finite retirement target.
        intent = {
          ...intent,
          domains,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([[
          realmCloseIntentKey(intent.account_id, intent.realm_id),
          intent,
        ]]);
      }
      if (intent.phase === "scan_aliases") {
        const nextCursor = await this.assertRealmHasNoLiveAliases(
          intent.account_id,
          intent.realm_id,
          intent.alias_cursor,
        );
        if (nextCursor !== null) {
          const previous = intent;
          const continued = {
            ...intent,
            alias_cursor: nextCursor,
            retry_at_ms: this.now().getTime() + 1_000,
            updated_at: this.now().toISOString(),
          };
          await this.atomic([
            [realmCloseIntentKey(intent.account_id, intent.realm_id), continued],
            [realmCloseDueKey(continued), `${intent.account_id}:${intent.realm_id}`],
          ], realmCloseDueKey(previous) === realmCloseDueKey(continued)
            ? []
            : [realmCloseDueKey(previous)]);
          await this.scheduleNextAlarm().catch(() => {});
          return json({
            schema_version: SCHEMA_VERSION,
            account_id: intent.account_id,
            realm_id: intent.realm_id,
            complete: false,
            phase: "scan_aliases",
          }, 202);
        }
        const previous = intent;
        intent = {
          ...intent,
          phase: "custom_domain_converging",
          alias_cursor: null,
          custom_domain_cursor: null,
          retry_at_ms: this.now().getTime(),
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
          [realmCloseDueKey(intent), `${intent.account_id}:${intent.realm_id}`],
        ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
          ? []
          : [realmCloseDueKey(previous)]);
      }

      if (intent.phase === "custom_domain_converging") {
        const prefix = customDomainSubscriptionRealmPrefix(
          intent.account_id,
          intent.realm_id,
        );
        const listed = await this.storage.list({
          prefix,
          limit: 2,
          ...(intent.custom_domain_cursor
            ? { startAfter: intent.custom_domain_cursor }
            : {}),
        });
        const rows = [...listed.entries()];
        const current = rows[0];
        if (current) {
          const [indexKey, alias] = current;
          const claimID = indexKey.slice(prefix.length);
          const [subscription, claim] = await Promise.all([
            this.storage.get(customDomainSubscriptionKey(claimID)),
            typeof alias === "string"
              ? this.storage.get(claimKey(alias))
              : null,
          ]);
          if (!subscription || !claim || claim.claim_id !== claimID ||
              claim.account_id !== intent.account_id ||
              claim.realm_id !== intent.realm_id || !claim.retired_at) {
            fail("realm custom-domain subscription index is invalid", 503);
          }
          const sync = await this.stageCustomDomainSyncForClaim(claim);
          const result = sync
            ? await this.drainCustomDomainSyncIntent(sync)
            : { complete: true };
          if (!result.complete) {
            const previous = intent;
            intent = {
              ...intent,
              failure_count: 0,
              retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
              updated_at: this.now().toISOString(),
            };
            await this.atomic([
              [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
              [realmCloseDueKey(intent),
                `${intent.account_id}:${intent.realm_id}`],
            ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
              ? []
              : [realmCloseDueKey(previous)]);
            await this.scheduleNextAlarm().catch(() => {});
            return json({
              schema_version: SCHEMA_VERSION,
              account_id: intent.account_id,
              realm_id: intent.realm_id,
              complete: false,
              phase: "custom_domain_converging",
            }, 202);
          }
          if (rows.length > 1) {
            const previous = intent;
            intent = {
              ...intent,
              custom_domain_cursor: indexKey,
              failure_count: 0,
              retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
              updated_at: this.now().toISOString(),
            };
            await this.atomic([
              [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
              [realmCloseDueKey(intent),
                `${intent.account_id}:${intent.realm_id}`],
            ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
              ? []
              : [realmCloseDueKey(previous)]);
            await this.scheduleNextAlarm().catch(() => {});
            return json({
              schema_version: SCHEMA_VERSION,
              account_id: intent.account_id,
              realm_id: intent.realm_id,
              complete: false,
              phase: "custom_domain_converging",
            }, 202);
          }
        }
        const previous = intent;
        intent = {
          ...intent,
          phase: "prepare_cell",
          custom_domain_cursor: null,
          failure_count: 0,
          retry_at_ms: this.now().getTime(),
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
          [realmCloseDueKey(intent), `${intent.account_id}:${intent.realm_id}`],
        ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
          ? []
          : [realmCloseDueKey(previous)]);
      }

      const target = await this.cellTarget(intent.account_id);
      if (intent.phase === "prepare_cell") {
        let source = await this.fetchCellCanonicalRoute(
          intent.account_id,
          intent.realm_id,
          target,
        );
        if (source.state === "live") {
          source = await this.postCellCanonicalTransition(
            target,
            cellRealmRoutePrepareURL(target.endpoint, intent.account_id),
            {
              realm_id: intent.realm_id,
              operation_id: intent.idempotency_key,
              expected_generation: source.generation,
            },
            intent.account_id,
            intent.realm_id,
          );
        }
        if (!["closing", "retired"].includes(source.state) ||
            source.operation_id !== intent.idempotency_key) {
          fail("cell canonical realm close fence conflicts", 409);
        }
        const previous = intent;
        intent = {
          ...intent,
          phase: "publish_retired",
          cell_route: source,
          retry_at_ms: this.now().getTime(),
          failure_count: 0,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
          [realmCloseDueKey(intent), `${intent.account_id}:${intent.realm_id}`],
        ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
          ? []
          : [realmCloseDueKey(previous)]);
      }

      if (intent.phase === "publish_retired") {
        const startIndex = Number.isSafeInteger(intent.publish_domain_index)
          ? intent.publish_domain_index
          : 0;
        if (startIndex < 0 || startIndex > domains.length) {
          fail("canonical realm close domain cursor is invalid", 503);
        }
        for (let index = startIndex; index < domains.length; index += 1) {
          const canonical = await this.upsertCanonicalRouteWithLaneHeld({
            domain: domains[index],
            cellRoute: intent.cell_route,
            forcedPolicy: { state: "retired", suspension_disposition: null },
          });
          intent = {
            ...intent,
            publish_domain_index: index + 1,
            canonical_routes: {
              ...(isObject(intent.canonical_routes)
                ? intent.canonical_routes
                : {}),
              [domains[index]]: publicCanonicalRoute(canonical),
            },
            updated_at: this.now().toISOString(),
          };
          await this.atomic([[
            realmCloseIntentKey(intent.account_id, intent.realm_id),
            intent,
          ]]);
        }
        const previous = intent;
        intent = {
          ...intent,
          phase: "commit_cell",
          publish_domain_index: domains.length,
          retry_at_ms: this.now().getTime(),
          failure_count: 0,
          updated_at: this.now().toISOString(),
        };
        await this.atomic([
          [realmCloseIntentKey(intent.account_id, intent.realm_id), intent],
          [realmCloseDueKey(intent), `${intent.account_id}:${intent.realm_id}`],
        ], realmCloseDueKey(previous) === realmCloseDueKey(intent)
          ? []
          : [realmCloseDueKey(previous)]);
      }

      if (intent.phase !== "commit_cell") {
        fail("canonical realm close intent is invalid", 503);
      }
      const committed = intent.cell_route.state === "retired"
        ? intent.cell_route
        : await this.postCellCanonicalTransition(
          target,
          cellRealmRouteCommitURL(target.endpoint, intent.account_id),
          {
            realm_id: intent.realm_id,
            operation_id: intent.idempotency_key,
            expected_generation: intent.cell_route.generation,
          },
          intent.account_id,
          intent.realm_id,
        );
      if (committed.state !== "retired" ||
          committed.operation_id !== intent.idempotency_key) {
        fail("cell did not commit canonical realm retirement", 502);
      }
      const canonicals = [];
      for (const domain of domains) {
        canonicals.push(await this.upsertCanonicalRouteWithLaneHeld({
          domain,
          cellRoute: committed,
          forcedPolicy: { state: "retired", suspension_disposition: null },
        }));
      }
      const canonical = canonicals[0];
      const body = {
        schema_version: SCHEMA_VERSION,
        account_id: intent.account_id,
        realm_id: intent.realm_id,
        complete: true,
        canonical_route: publicCanonicalRoute(canonical),
        canonical_routes: canonicals.map(publicCanonicalRoute),
      };
      const fence = {
        account_id: intent.account_id,
        realm_id: intent.realm_id,
        operation_id: intent.idempotency_key,
        cell_generation: committed.generation,
        controller_revision: canonical.controller_revision,
        canonical_revisions: canonicals.map((route) => ({
          domain: route.domain,
          controller_revision: route.controller_revision,
        })),
        completed_at: this.now().toISOString(),
      };
      await this.atomic([
        [realmCloseFenceKey(intent.account_id, intent.realm_id), fence],
        [`idem:realm-close:${intent.account_id}:${intent.realm_id}:${intent.idempotency_key}`, {
          fingerprint: intent.fingerprint,
          status: 200,
          body,
        }],
      ], [
        realmCloseIntentKey(intent.account_id, intent.realm_id),
        realmCloseDueKey(intent),
      ]);
      await this.scheduleNextAlarm().catch(() => {});
      return json(body);
    } catch (error) {
      const current = await this.storage.get(
        realmCloseIntentKey(intent.account_id, intent.realm_id),
      );
      if (current) {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
          last_failure_at: this.now().toISOString(),
        };
        await this.atomic([
          [realmCloseIntentKey(intent.account_id, intent.realm_id), retry],
          [realmCloseDueKey(retry), `${intent.account_id}:${intent.realm_id}`],
        ], realmCloseDueKey(current) === realmCloseDueKey(retry)
          ? []
          : [realmCloseDueKey(current)]);
        await this.scheduleNextAlarm().catch(() => {});
        if (error instanceof RegistryError && error.status >= 500) {
          return json({
            schema_version: SCHEMA_VERSION,
            account_id: intent.account_id,
            realm_id: intent.realm_id,
            complete: false,
            phase: retry.phase,
          }, 202);
        }
      }
      throw error;
    }
  }

  async boundedValues(
    prefix,
    limit = REGISTRY_LIST_LIMIT,
    reverse = false,
    cursor = null,
  ) {
    const cursorKey = decodeListCursor(cursor, prefix);
    const listed = await this.storage.list({
      prefix,
      limit: limit + 1,
      reverse,
      ...(cursorKey
        ? reverse ? { end: cursorKey } : { startAfter: cursorKey }
        : {}),
    });
    const entries = [...listed.entries()];
    const values = entries.map(([, value]) => value);
    const truncated = values.length > limit;
    return {
      values: values.slice(0, limit),
      truncated,
      next_cursor: truncated
        ? encodeListCursor(entries[limit - 1][0])
        : null,
    };
  }

  async boundedIndexedClaims(prefix, cursor = null) {
    const listed = await this.boundedValues(
      prefix,
      REGISTRY_LIST_LIMIT,
      false,
      cursor,
    );
    const claims = await Promise.all(
      listed.values.map((alias) => this.storage.get(claimKey(alias))),
    );
    return { ...listed, values: claims.filter(Boolean) };
  }

  async indexedClaims(prefix) {
    const aliases = listValues(await this.storage.list({ prefix }));
    const claims = await Promise.all(
      aliases.map((alias) => this.storage.get(claimKey(alias))),
    );
    return claims.filter(Boolean);
  }

  async claimsForAccount(accountID) {
    return this.indexedClaims(accountClaimIndexPrefix(accountID));
  }

  async claimPageForAccount(
    accountID,
    startAfter = null,
    limit = PLAN_RECONCILE_CLAIM_LIMIT,
  ) {
    const listed = await this.storage.list({
      prefix: accountClaimIndexPrefix(accountID),
      limit: limit + 1,
      ...(startAfter ? { startAfter } : {}),
    });
    const entries = [...listed.entries()];
    const pageEntries = entries.slice(0, limit);
    const claims = await Promise.all(
      pageEntries.map(([, alias]) => this.storage.get(claimKey(alias))),
    );
    return {
      claims: claims.filter(Boolean),
      next_cursor: entries.length > limit
        ? pageEntries.at(-1)[0]
        : null,
    };
  }

  async canonicalPageForAccount(
    accountID,
    startAfter = null,
    limit = PLAN_RECONCILE_CLAIM_LIMIT,
  ) {
    const listed = await this.storage.list({
      prefix: accountCanonicalIndexPrefix(accountID),
      limit: limit + 1,
      ...(startAfter ? { startAfter } : {}),
    });
    const entries = [...listed.entries()];
    const pageEntries = entries.slice(0, limit);
    const canonicals = await Promise.all(
      pageEntries.map(([, key]) => this.storage.get(key)),
    );
    return {
      canonicals: canonicals.filter(Boolean),
      next_cursor: entries.length > limit
        ? pageEntries.at(-1)[0]
        : null,
    };
  }

  async claimsForRealm(accountID, realmID) {
    return this.indexedClaims(accountClaimIndexPrefix(accountID, realmID));
  }

  async requestsForRealm(accountID, realmID, cursor = null) {
    const listed = await this.boundedValues(
      accountRequestIndexPrefix(accountID, realmID),
      REGISTRY_LIST_LIMIT,
      false,
      cursor,
    );
    const requests = await Promise.all(
      listed.values.map((id) => this.storage.get(requestKey(id))),
    );
    return {
      values: requests.filter(Boolean),
      truncated: listed.truncated,
      next_cursor: listed.next_cursor,
    };
  }

  async reconcilePendingCounterMigration() {
    const meta = await this.storage.get(META_KEY);
    const migration = await this.storage.get(PENDING_COUNTER_MIGRATION_KEY);
    const ready = meta?.pending_counter_schema_version ===
        PENDING_COUNTER_SCHEMA_VERSION &&
      (meta.pending_counter_state === undefined ||
        meta.pending_counter_state === "ready");
    if (ready && !migration) {
      return;
    }
    if (!migration || migration.schema_version !==
        PENDING_COUNTER_SCHEMA_VERSION ||
        !["upgrade", "recovery"].includes(migration.kind) ||
        !["clear", "scan", "verify"].includes(migration.phase) ||
        (migration.phase === "clear" && migration.kind !== "recovery") ||
        (migration.phase !== "clear" && migration.cursor !== null &&
          (typeof migration.cursor !== "string" ||
            !migration.cursor.startsWith("claim:"))) ||
        !Number.isSafeInteger(migration.retry_at_ms)) {
      fail("realm email alias pending counter migration is invalid", 503);
    }
    if (migration.retry_at_ms > this.now().getTime()) return;

    try {
      if (migration.phase === "clear") {
        const index = migration.clear_prefix_index;
        if (!Number.isSafeInteger(index) || index < 0 ||
            index >= PENDING_COUNTER_DERIVED_PREFIXES.length) {
          fail("realm email alias pending counter clear cursor is invalid", 503);
        }
        const prefix = PENDING_COUNTER_DERIVED_PREFIXES[index];
        if (migration.cursor !== null &&
            (typeof migration.cursor !== "string" ||
              !migration.cursor.startsWith(prefix))) {
          fail("realm email alias pending counter clear cursor is invalid", 503);
        }
        const listed = await this.storage.list({
          prefix,
          limit: PENDING_COUNTER_MIGRATION_PAGE_LIMIT + 1,
          ...(migration.cursor ? { startAfter: migration.cursor } : {}),
        });
        const entries = [...listed.entries()];
        const page = entries.slice(0, PENDING_COUNTER_MIGRATION_PAGE_LIMIT);
        await this.atomic([], page.map(([key]) => key));
        const more = entries.length > PENDING_COUNTER_MIGRATION_PAGE_LIMIT;
        const lastPrefix = index === PENDING_COUNTER_DERIVED_PREFIXES.length - 1;
        const continued = {
          ...migration,
          phase: more || !lastPrefix ? "clear" : "scan",
          clear_prefix_index: more ? index : Math.min(index + 1,
            PENDING_COUNTER_DERIVED_PREFIXES.length - 1),
          cursor: more ? page.at(-1)[0] : null,
          failure_count: 0,
          retry_at_ms: this.now().getTime() +
            PENDING_COUNTER_MIGRATION_RETRY_MS,
          updated_at: this.now().toISOString(),
        };
        await this.storage.put(PENDING_COUNTER_MIGRATION_KEY, continued);
        return;
      }

      const listed = await this.storage.list({
        prefix: "claim:",
        limit: PENDING_COUNTER_MIGRATION_PAGE_LIMIT + 1,
        ...(migration.cursor ? { startAfter: migration.cursor } : {}),
      });
      const entries = [...listed.entries()];
      const page = entries.slice(0, PENDING_COUNTER_MIGRATION_PAGE_LIMIT);
      for (const [key, listedClaim] of page) {
        const lanes = [
          `account:${listedClaim.account_id}`,
          `realm:${listedClaim.account_id}:${listedClaim.realm_id}`,
          `skeleton:${listedClaim.skeleton}`,
        ];
        await this.withLanes(lanes, async () => {
          const claim = await this.storage.get(key);
          if (!claim) return;
          const contribution = this.claimUsageContribution(claim);
          if (contribution?.open_request === 1) {
            const request = await this.storage.get(requestKey(claim.request_id));
            const expectedStatus = claim.customer_activation_intent === true
              ? "provisioning"
              : "pending_review";
            if (request?.status !== expectedStatus ||
                request.alias !== claim.alias ||
                request.account_id !== claim.account_id ||
                request.realm_id !== claim.realm_id) {
              fail("realm email alias open request cannot be counted safely", 503);
            }
          }
          if (migration.phase === "scan") {
            await this.atomic([], [], {
              claimUsageTransition: {
                previousClaim: null,
                desiredClaim: claim,
                updatedAt: this.now().toISOString(),
                mode: "migration",
              },
            });
          } else {
            await this.atomic([], [], {
              claimUsageTransition: {
                previousClaim: claim,
                desiredClaim: claim,
                updatedAt: this.now().toISOString(),
                mode: "verification",
              },
            });
          }
        });
      }

      if (entries.length <= PENDING_COUNTER_MIGRATION_PAGE_LIMIT) {
        if (migration.phase === "scan") {
          await this.storage.put(PENDING_COUNTER_MIGRATION_KEY, {
            ...migration,
            phase: "verify",
            cursor: null,
            scanned: (migration.scanned ?? 0) + page.length,
            failure_count: 0,
            retry_at_ms: this.now().getTime() +
              PENDING_COUNTER_MIGRATION_RETRY_MS,
            updated_at: this.now().toISOString(),
          });
          return;
        }
        await this.withLane("registry:metadata", async () => {
          const latest = await this.storage.get(META_KEY);
          const upgraded = {
            ...latest,
            pending_counter_schema_version: PENDING_COUNTER_SCHEMA_VERSION,
            pending_counter_state: "ready",
            pending_counter_rebuilt_at: this.now().toISOString(),
            updated_at: this.now().toISOString(),
          };
          await this.atomic(
            [[META_KEY, upgraded]],
            [PENDING_COUNTER_MIGRATION_KEY],
          );
        });
        return;
      }
      const continued = {
        ...migration,
        cursor: page.at(-1)[0],
        ...(migration.phase === "scan"
          ? { scanned: (migration.scanned ?? 0) + page.length }
          : { verified: (migration.verified ?? 0) + page.length }),
        failure_count: 0,
        retry_at_ms: this.now().getTime() +
          PENDING_COUNTER_MIGRATION_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.storage.put(PENDING_COUNTER_MIGRATION_KEY, continued);
    } catch (error) {
      const current = await this.storage.get(PENDING_COUNTER_MIGRATION_KEY);
      if (current) {
        const failureCount = (current.failure_count ?? 0) + 1;
        await this.storage.put(PENDING_COUNTER_MIGRATION_KEY, {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
          last_failure_at: this.now().toISOString(),
        });
      }
      this.log(
        "realm-email-alias: pending counter migration failed; writes remain fenced",
      );
    }
  }

  async scheduleNextAlarm(fallbackDelay = null) {
    if (typeof this.storage.setAlarm !== "function") return;
    return this.withLane("registry:alarm-schedule", async () => {
      if (fallbackDelay !== null) {
        await this.storage.setAlarm(this.now().getTime() + fallbackDelay);
        return;
      }
      const firstGrace = [...(await this.storage.list({
        prefix: "grace:",
        limit: 1,
      })).keys()][0];
      const firstPlan = [...(await this.storage.list({
        prefix: "plan-due:",
        limit: 1,
      })).keys()][0];
      const firstApproval = [...(await this.storage.list({
        prefix: "approval-due:",
        limit: 1,
      })).keys()][0];
      const firstLifecycle = [...(await this.storage.list({
        prefix: "lifecycle-due:",
        limit: 1,
      })).keys()][0];
      const firstProjection = [...(await this.storage.list({
        prefix: "projection-due:",
        limit: 1,
      })).keys()][0];
      const firstInternal = [...(await this.storage.list({
        prefix: "internal-due:",
        limit: 1,
      })).keys()][0];
      const firstRefresh = [...(await this.storage.list({
        prefix: "refresh-due:",
        limit: 1,
      })).keys()][0];
      const firstRealmClose = [...(await this.storage.list({
        prefix: "realm-close-due:",
        limit: 1,
      })).keys()][0];
      const firstCustomDomainSync = [...(await this.storage.list({
        prefix: "custom-domain-sync-due:",
        limit: 1,
      })).keys()][0];
      const counterMigration = await this.storage.get(
        PENDING_COUNTER_MIGRATION_KEY,
      );
      const deadlines = [
        firstGrace ? Number(firstGrace.split(":", 3)[1]) : NaN,
        firstPlan ? Number(firstPlan.split(":", 3)[1]) : NaN,
        firstApproval ? Number(firstApproval.split(":", 3)[1]) : NaN,
        firstLifecycle ? Number(firstLifecycle.split(":", 3)[1]) : NaN,
        firstProjection ? Number(firstProjection.split(":", 3)[1]) : NaN,
        firstInternal ? Number(firstInternal.split(":", 3)[1]) : NaN,
        firstRefresh ? Number(firstRefresh.split(":", 3)[1]) : NaN,
        firstRealmClose ? Number(firstRealmClose.split(":", 3)[1]) : NaN,
        firstCustomDomainSync
          ? Number(firstCustomDomainSync.split(":", 3)[1])
          : NaN,
        Number(counterMigration?.retry_at_ms),
      ].filter(Number.isFinite);
      if (deadlines.length > 0) {
        await this.storage.setAlarm(Math.min(...deadlines));
      } else if (typeof this.storage.deleteAlarm === "function") {
        await this.storage.deleteAlarm().catch(() => {});
      }
    });
  }

  alarm() {
    return this.withAuthorityOperationalWork(async () => {
      await this.withLane("registry:seed", () => this.ensureSeeded());
      await this.reconcilePendingCounterMigration();
      const meta = await this.storage.get(META_KEY);
      if (meta?.pending_counter_schema_version !==
          PENDING_COUNTER_SCHEMA_VERSION ||
          (meta.pending_counter_state !== undefined &&
            meta.pending_counter_state !== "ready") ||
          await this.storage.get(PENDING_COUNTER_MIGRATION_KEY)) {
        // Cursor progress is already durable. Propagate a failed re-arm so the
        // Durable Object alarm event is retried instead of silently stranding
        // a rebuilding registry with no future wakeup.
        await this.scheduleNextAlarm();
        return;
      }
      // Each lane owns its own retry fence. A poison account or claim must not
      // prevent later due items, grace expiry, or approval recovery from
      // making progress during the same bounded alarm turn.
      await this.reconcileDueCustomDomainSyncs();
      await this.reconcileDueRealmCloses();
      await this.reconcileDuePlanIntents();
      await this.reconcileDueLifecycles();
      await this.reconcileDueProjections();
      await this.reconcileDueInternalAssignments();
      await this.reconcileDueRouteRefreshes();
      await this.reconcileDueApprovals();
      await this.reconcileDueGrace();
      await this.scheduleNextAlarm().catch(() => {});
    });
  }

  async matchingProjectionIntent(alias, scope, key, expectedFingerprint) {
    const intent = await this.storage.get(projectionIntentKey(alias));
    if (!intent) return null;
    if (intent.scope !== scope || intent.idempotency_key !== key ||
        intent.fingerprint !== expectedFingerprint) {
      fail("alias has a durable projection still converging", 409);
    }
    return intent;
  }

  async stageProjectionIntent({
    desiredClaim,
    scope,
    key,
    fingerprint: expectedFingerprint,
    entries,
    deletes = [],
    responseBody,
    responseStatus = 200,
    includeCanonical = true,
    canonicalState = "applied",
    allowMissingCellForRetirement = false,
    handoffDeletes = [],
    claimUsageTransition = null,
  }) {
    const existing = await this.matchingProjectionIntent(
      desiredClaim.alias,
      scope,
      key,
      expectedFingerprint,
    );
    if (existing) return existing;
    const now = this.now();
    const intent = {
      alias: desiredClaim.alias,
      account_id: desiredClaim.account_id,
      realm_id: desiredClaim.realm_id,
      skeleton: desiredClaim.skeleton,
      desired_claim: desiredClaim,
      scope,
      idempotency_key: key,
      fingerprint: expectedFingerprint,
      final_entries: entries,
      final_deletes: deletes,
      response_body: responseBody,
      response_status: responseStatus,
      include_canonical: includeCanonical,
      canonical_state: canonicalState,
      allow_missing_cell_for_retirement:
        allowMissingCellForRetirement === true,
      claim_usage_transition: claimUsageTransition,
      failure_count: 0,
      retry_at_ms: now.getTime(),
      created_at: now.toISOString(),
      updated_at: now.toISOString(),
    };
    await this.atomic(
      [
        [projectionIntentKey(intent.alias), intent],
        [projectionDueKey(intent), intent.alias],
      ],
      handoffDeletes,
    );
    await this.scheduleNextAlarm().catch(() => {});
    return intent;
  }

  async drainProjectionIntent(intent) {
    try {
      const meta = await this.storage.get(META_KEY);
      try {
        await this.syncClaimProjection(
          intent.desired_claim,
          meta,
          intent.include_canonical !== false,
          intent.canonical_state ?? "applied",
        );
      } catch (error) {
        const missingTerminalTarget =
          intent.allow_missing_cell_for_retirement === true &&
          intent.desired_claim?.retired_at &&
          error instanceof RegistryError &&
          (error.status === 404 ||
            (error.status === 409 &&
              error.message === "alias target account is not routed"));
        if (!missingTerminalTarget) throw error;
        // A deleted realm or retired account has no cell surface left to
        // accept a tombstone. The global retired claim is still authoritative;
        // publish the value-free edge tombstone and finish the durable outbox.
        await this.publishClaimRoute(intent.desired_claim, meta, null);
      }
      await this.atomic(
        intent.final_entries,
        [
          ...(intent.final_deletes ?? []),
          projectionIntentKey(intent.alias),
          projectionDueKey(intent),
        ],
        intent.claim_usage_transition
          ? { claimUsageTransition: intent.claim_usage_transition }
          : {},
      );
      await this.scheduleNextAlarm().catch(() => {});
      return json(intent.response_body, intent.response_status);
    } catch (error) {
      const current = await this.storage.get(projectionIntentKey(intent.alias));
      if (current && current.fingerprint === intent.fingerprint &&
          current.scope === intent.scope) {
        const failureCount = (current.failure_count ?? 0) + 1;
        const retry = {
          ...current,
          failure_count: failureCount,
          retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
          last_failure_at: this.now().toISOString(),
        };
        await this.atomic(
          [
            [projectionIntentKey(intent.alias), retry],
            [projectionDueKey(retry), intent.alias],
          ],
          [projectionDueKey(current)],
        );
        await this.scheduleNextAlarm().catch(() => {});
      }
      throw error;
    }
  }

  async reconcileDueProjections() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "projection-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, alias] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      const intent = await this.storage.get(projectionIntentKey(alias));
      const lanes = intent
        ? [
          `account:${intent.account_id}`,
          `realm:${intent.account_id}:${intent.realm_id}`,
          `skeleton:${intent.skeleton}`,
        ]
        : [`skeleton:${alias}`];
      await this.withLanes(lanes, async () => {
        const current = await this.storage.get(projectionIntentKey(alias));
        if (!current || projectionDueKey(current) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        await this.drainProjectionIntent(current).catch(() => {});
      });
    }
  }

  async reconcileDueRealmCloses() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "realm-close-due:",
      limit: REALM_CLOSE_ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, reference] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt) || typeof reference !== "string") {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      const separator = reference.indexOf(":");
      const accountID = separator > 0 ? reference.slice(0, separator) : "";
      const realmID = separator > 0 ? reference.slice(separator + 1) : "";
      if (!ACCOUNT_ID_PATTERN.test(accountID) ||
          !REALM_ID_PATTERN.test(realmID)) {
        await this.storage.delete(dueKey);
        continue;
      }
      const key = realmCloseIntentKey(accountID, realmID);
      const intent = await this.storage.get(key);
      let domains = [];
      if (intent) {
        try {
          domains = this.realmCloseIntentDomains(intent);
        } catch {
          // Invalid intent/configuration state cannot mutate a canonical row:
          // acquire the account/realm lanes and let drainRealmCloseIntent
          // record its bounded retry without poisoning unrelated alarm work.
          domains = [];
        }
      }
      const lanes = intent
        ? [
          `account:${accountID}`,
          `realm:${accountID}:${realmID}`,
          ...domains.map((domain) => `canonical:${domain}:${realmID}`),
        ]
        : [`realm:${accountID}:${realmID}`];
      await this.withLanes(lanes, async () => {
        const current = await this.storage.get(key);
        if (!current || realmCloseDueKey(current) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        await this.drainRealmCloseIntent(current).catch(() => {});
      });
    }
  }

  async reconcileDueRouteRefreshes() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "refresh-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, refreshKey] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      const intent = await this.storage.get(refreshKey);
      await this.withLane(
        intent ? `account:${intent.account_id}` : `refresh:${refreshKey}`,
        async () => {
          const current = await this.storage.get(refreshKey);
          if (!current || routeRefreshDueKey(current) !== dueKey) {
            await this.storage.delete(dueKey);
            return;
          }
          try {
            if (await this.storage.get(
              lifecycleIntentKey(current.account_id),
            )) {
              fail("account lifecycle projection is still converging", 409);
            }
            if (await this.storage.get(planIntentKey(current.account_id))) {
              fail("account alias plan projection is still converging", 409);
            }
            if (current.kind === "realm_alias") {
              if (await this.storage.get(projectionIntentKey(current.alias))) {
                fail("alias projection is still converging", 409);
              }
              const claim = await this.storage.get(claimKey(current.alias));
              if (!claim?.assignment_kind || claim.domain !== current.domain) {
                await this.atomic([], [refreshKey, dueKey]);
                return;
              }
              const target = cellAliasState(claim) === "applied"
                ? await this.cellTarget(claim.account_id)
                : null;
              await this.publishClaimRoute(claim, null, target);
            } else if (current.kind === "canonical") {
              const canonicalKey = canonicalRouteAuthorityKey(
                current.domain,
                `realm_${current.realm_label}`,
              );
              const canonical = await this.storage.get(canonicalKey);
              if (!canonical) {
                await this.atomic([], [refreshKey, dueKey]);
                return;
              }
              if (canonical.state === "retired") {
                await this.publishStoredRetiredCanonicalRoute(canonical);
              } else {
                const target = await this.cellTarget(canonical.account_id);
                const [source, emailEnabled] = await Promise.all([
                  this.fetchCellCanonicalRoute(
                    canonical.account_id,
                    canonical.realm_id,
                    target,
                  ),
                  this.canonicalEmailEntitlement(
                    canonical.account_id,
                    target,
                  ),
                ]);
                await this.upsertCanonicalRoute({
                  domain: canonical.domain,
                  cellRoute: source,
                  target,
                  emailEnabled,
                });
              }
            } else {
              await this.atomic([], [refreshKey, dueKey]);
              return;
            }
            await this.atomic([], [refreshKey, dueKey]);
          } catch {
            const failureCount = (current.failure_count ?? 0) + 1;
            const retry = {
              ...current,
              failure_count: failureCount,
              retry_at_ms: this.now().getTime() +
                Math.max(ROUTE_REFRESH_RETRY_MS, retryDelayMs(failureCount)),
              last_failure_at: this.now().toISOString(),
            };
            await this.atomic(
              [
                [refreshKey, retry],
                [routeRefreshDueKey(retry), refreshKey],
              ],
              [dueKey],
            );
          }
        },
      );
    }
  }

  async rescheduleApproval(claim, dueKey) {
    const failureCount = (claim.approval_failure_count ?? 0) + 1;
    const updated = {
      ...claim,
      approval_failure_count: failureCount,
      approval_retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
      approval_last_failure_at: this.now().toISOString(),
    };
    await this.atomic(
      [
        [claimKey(updated.alias), updated],
        [approvalDueKey(updated), updated.alias],
      ],
      [dueKey],
    );
  }

  async reconcileDueApprovals() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "approval-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, alias] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      const claim = await this.storage.get(claimKey(alias));
      const lanes = claim
        ? [
          `account:${claim.account_id}`,
          `realm:${claim.account_id}:${claim.realm_id}`,
          `skeleton:${claim.skeleton}`,
        ]
        : [`skeleton:${alias}`];
      await this.withLanes(lanes, async () => {
        const current = await this.storage.get(claimKey(alias));
        if (!current?.customer_activation_intent ||
            approvalDueKey(current) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        try {
          const request = await this.storage.get(requestKey(current.request_id));
          if (request?.status !== "provisioning") {
            fail("alias provisioning request is inconsistent", 500);
          }
          const snapshot = await this.fetchAuthoritativePlan(current.account_id);
          const entitlement = realmEmailAliasEntitlement(snapshot);
          await this.completeApprovalProvisioning(request, current, {
            actor: { kind: "system", id: "approval-recovery" },
            scope: `request-approve:${request.id}`,
            key: current.approval_idempotency_key,
            fingerprint: current.approval_fingerprint,
            reason: request.decision?.reason ?? "durable approval recovery",
            activation_enabled:
              String(this.env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") === "true",
            feature_enabled: entitlement.enabled,
            alias_limit: entitlement.limit,
          });
        } catch {
          const stored = await this.storage.get(claimKey(alias));
          const projection = await this.storage.get(projectionIntentKey(alias));
          if (stored?.customer_activation_intent === true &&
              stored.claim_id === current.claim_id &&
              stored.assignment_revision === current.assignment_revision &&
              approvalDueKey(stored) === dueKey && !projection) {
            await this.rescheduleApproval(stored, dueKey);
          }
        }
      });
    }
  }

  async reconcileDueGrace() {
    const now = this.now();
    const listed = await this.storage.list({
      prefix: "grace:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [indexKey, alias] of listed) {
      const deadline = Number(indexKey.split(":", 3)[1]);
      if (!Number.isFinite(deadline)) {
        await this.storage.delete(indexKey);
        continue;
      }
      if (deadline > now.getTime()) break;
      const claim = await this.storage.get(claimKey(alias));
      if (!claim || graceIndexKey(claim) !== indexKey || claim.retired_at ||
          claim.plan_suspended || claim.assignment_kind !== "customer") {
        await this.storage.delete(indexKey);
        continue;
      }
      await this.withLanes([
        `account:${claim.account_id}`,
        `realm:${claim.account_id}:${claim.realm_id}`,
        `skeleton:${claim.skeleton}`,
      ], async () => {
        const current = await this.storage.get(claimKey(alias));
        if (!current || graceIndexKey(current) !== indexKey) {
          await this.storage.delete(indexKey);
          return;
        }
        try {
          const meta = await this.storage.get(META_KEY);
          const mutation = await this.mutationEntries(
            meta,
            { kind: "system", id: "plan-grace-alarm" },
            "alias.plan_grace_expired",
            current.alias,
            { changed: 1 },
          );
          const updated = {
            ...current,
            plan_suspended: true,
            plan_grace_until: null,
            grace_retry_at_ms: null,
            grace_failure_count: null,
            assignment_revision: (current.assignment_revision ?? 0) + 1,
            updated_at: mutation.now,
          };
          const projectionFingerprint = fingerprint({
            operation: "plan_grace_expired",
            alias: updated.alias,
            assignment_revision: updated.assignment_revision,
          });
          const projection = await this.stageProjectionIntent({
            desiredClaim: updated,
            scope: `plan-grace:${updated.alias}`,
            key: `grace:${updated.assignment_revision}:${updated.alias}`,
            fingerprint: projectionFingerprint,
            entries: [
              ...mutation.entries,
              [claimKey(updated.alias), updated],
            ],
            deletes: [indexKey],
            responseBody: { schema_version: SCHEMA_VERSION, converged: true },
            includeCanonical: false,
            handoffDeletes: [indexKey],
          });
          await this.drainProjectionIntent(projection);
        } catch {
          if (await this.storage.get(projectionIntentKey(current.alias))) {
            return;
          }
          const failureCount = (current.grace_failure_count ?? 0) + 1;
          const retry = {
            ...current,
            grace_failure_count: failureCount,
            grace_retry_at_ms:
              this.now().getTime() + retryDelayMs(failureCount),
          };
          await this.atomic(
            [
              [claimKey(retry.alias), retry],
              [graceIndexKey(retry), retry.alias],
            ],
            [indexKey],
          );
        }
      });
    }
    await this.scheduleNextAlarm();
  }

  async createRequest(input) {
    validateAccountRealm(input);
    await this.assertAccountAliasWritesAllowed(input.account_id);
    await this.assertRealmAliasWritesAllowed(input.account_id, input.realm_id);
    const actor = validateActor(input.actor, "account_operator");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.alias);
    const domain = validateManagedRealmEmailDomain(input.domain);
    const aliasLimit = input.alias_limit;
    if (input.activation_enabled !== true) {
      fail("realm email aliases are not activated on the managed email edge", 409);
    }
    if (input.feature_enabled !== true || !validAliasLimit(aliasLimit) ||
        (aliasLimit !== null && aliasLimit < 1)) {
      fail("realm email aliases are not enabled for this account", 403);
    }
    const fp = fingerprint({
      alias,
      domain,
      account_id: input.account_id,
      realm_id: input.realm_id,
    });
    const scope = `request-create:${input.account_id}:${input.realm_id}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    await this.assertPendingCountersReady();
    await this.assertCurrentPlanFence(input.account_id, input);

    const skeleton = realmEmailAliasSkeleton(alias);
    const reservedName = await this.storage.get(reservedSkeletonKey(skeleton));
    const reservation = reservedName
      ? await this.storage.get(reservedKey(reservedName))
      : null;
    if (reservation?.enabled) {
      fail("alias is reserved by Witself policy", 409);
    }
    if (await this.storage.get(claimKey(alias))) {
      fail("alias is already claimed or tombstoned", 409);
    }
    if (await this.storage.get(claimSkeletonKey(skeleton))) {
      fail("alias is confusable with an existing claim", 409);
    }
    const usage = await this.usageForRealm(input.account_id, input.realm_id);
    const technicalCapacity = this.pendingCapacity(usage);
    if (technicalCapacity.realm.at_limit) {
      this.observePendingLimitRefusal("realm", technicalCapacity.realm.max);
      fail(
        "realm email alias pending request ceiling reached",
        409,
        "technical_pending_limit_reached",
        { scope: "realm", limit: technicalCapacity.realm.max },
      );
    }
    if (technicalCapacity.account.at_limit) {
      this.observePendingLimitRefusal("account", technicalCapacity.account.max);
      fail(
        "account email alias pending request ceiling reached",
        409,
        "technical_pending_limit_reached",
        { scope: "account", limit: technicalCapacity.account.max },
      );
    }
    const assigned = usage.realm.customer_allocated;
    const pending = usage.realm.pending_review;
    if (aliasLimit !== null &&
        (assigned >= aliasLimit ||
          pending >= Math.max(1, aliasLimit - assigned))) {
      fail("realm email alias limit reached", 403);
    }

    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      actor,
      "alias.requested",
      alias,
      { account_id: input.account_id, realm_id: input.realm_id },
    );
    const id = this.newRequestID();
    if (!REQUEST_ID_PATTERN.test(id)) fail("could not mint request id", 500);
    const mintedClaimID = this.newClaimID();
    if (!CLAIM_ID_PATTERN.test(mintedClaimID)) {
      fail("could not mint claim id", 500);
    }
    const request = {
      id,
      alias,
      domain,
      skeleton,
      account_id: input.account_id,
      realm_id: input.realm_id,
      status: "pending_review",
      requested_by: actor.id,
      requested_at: mutation.now,
      updated_at: mutation.now,
      decision: null,
    };
    const claim = {
      claim_id: mintedClaimID,
      alias,
      domain,
      skeleton,
      account_id: input.account_id,
      realm_id: input.realm_id,
      request_id: id,
      assignment_kind: null,
      assignment_revision: 0,
      admin_suspended: false,
      plan_suspended: false,
      operational_gate_suspended: false,
      plan_grace_until: null,
      created_at: mutation.now,
      updated_at: mutation.now,
      retired_at: null,
    };
    const body = { schema_version: SCHEMA_VERSION, request };
    mutation.entries.push(
      [requestKey(id), request],
      [accountRequestIndexKey(request), id],
      [claimKey(alias), claim],
      [accountClaimIndexKey(claim), alias],
      [claimSkeletonKey(skeleton), alias],
      [`idem:${scope}:${key}`, { fingerprint: fp, status: 202, body }],
    );
    await this.atomic(mutation.entries, [], {
      claimUsageTransition: {
        previousClaim: null,
        desiredClaim: claim,
        updatedAt: mutation.now,
        mode: "create",
      },
    });
    return json(body, 202);
  }

  async listRequests(input, admin) {
    if (!admin) {
      validateAccountRealm(input);
      validateActor(input.actor, "account_operator");
    } else {
      validateActor(input.actor, "platform_admin");
    }
    const listed = admin
      ? await this.boundedValues(
        "request:",
        REGISTRY_LIST_LIMIT,
        false,
        input.cursor,
      )
      : await this.requestsForRealm(
        input.account_id,
        input.realm_id,
        input.cursor,
      );
    let requests = listed.values.filter((request) =>
      (admin || (request.account_id === input.account_id && request.realm_id === input.realm_id)) &&
      (!input.status || request.status === input.status) &&
      (!input.account_id || request.account_id === input.account_id) &&
      (!input.realm_id || request.realm_id === input.realm_id)
    );
    requests.sort((left, right) =>
      left.requested_at.localeCompare(right.requested_at) || left.id.localeCompare(right.id)
    );
    const meta = await this.storage.get(META_KEY);
    const countersReady = meta?.pending_counter_schema_version ===
        PENDING_COUNTER_SCHEMA_VERSION &&
      (meta.pending_counter_state === undefined ||
        meta.pending_counter_state === "ready") &&
      !await this.storage.get(PENDING_COUNTER_MIGRATION_KEY);
    let pendingCapacityValue;
    if (countersReady &&
        ACCOUNT_ID_PATTERN.test(input.account_id ?? "") &&
        REALM_ID_PATTERN.test(input.realm_id ?? "")) {
      pendingCapacityValue = this.pendingCapacity(await this.usageForRealm(
        input.account_id,
        input.realm_id,
      ));
    }
    return json({
      schema_version: SCHEMA_VERSION,
      requests,
      truncated: listed.truncated,
      next_cursor: listed.next_cursor ?? null,
      pending_counter_state: countersReady ? "ready" : "rebuilding",
      technical_pending_limits: realmEmailAliasPendingRequestLimits(this.env),
      ...(pendingCapacityValue
        ? { pending_capacity: pendingCapacityValue }
        : {}),
    });
  }

  async getRequest(input) {
    validateActor(input.actor, "platform_admin");
    if (!REQUEST_ID_PATTERN.test(input.request_id ?? "")) {
      fail("invalid request_id", 400);
    }
    const request = await this.storage.get(requestKey(input.request_id));
    if (!request) fail("alias request not found", 404);
    return json({ schema_version: SCHEMA_VERSION, request });
  }

  async approveRequest(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    if (!REQUEST_ID_PATTERN.test(input.request_id ?? "")) {
      fail("invalid request_id", 400);
    }
    const reason = validateReason(input.reason, true);
    const fp = fingerprint({ request_id: input.request_id, action: "approve", reason });
    const scope = `request-approve:${input.request_id}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    let request = await this.storage.get(requestKey(input.request_id));
    if (!request) fail("alias request not found", 404);
    await this.assertPendingCountersReady();
    const pendingProjection = await this.matchingProjectionIntent(
      request.alias,
      scope,
      key,
      fp,
    );
    if (pendingProjection) return this.drainProjectionIntent(pendingProjection);
    await this.assertAccountAliasWritesAllowed(request.account_id);
    await this.assertRealmAliasWritesAllowed(
      request.account_id,
      request.realm_id,
    );
    const resuming = request.status === "provisioning";
    if (!resuming && request.status !== "pending_review") {
      fail("alias request is no longer pending", 409);
    }
    if (!resuming) {
      if (input.activation_enabled !== true) {
        fail("realm email aliases are not activated on the managed email edge", 409);
      }
      if (input.feature_enabled !== true || !validAliasLimit(input.alias_limit) ||
          (input.alias_limit !== null && input.alias_limit < 1)) {
        fail("realm email aliases are not enabled for this account", 403);
      }
      await this.assertCurrentPlanFence(request.account_id, input);
      await this.assertRealmTarget(request.account_id, request.realm_id);
    } else if (!validAliasLimit(input.alias_limit)) {
      fail("invalid alias_limit", 400);
    }
    let claim = await this.storage.get(claimKey(request.alias));
    if (!claim || claim.request_id !== request.id ||
        (!resuming && claim.assignment_kind) ||
        (resuming && claim.assignment_kind !== "customer")) {
      fail("alias request claim is inconsistent", 409);
    }
    if (resuming) {
      if (!claim.customer_activation_intent ||
          claim.approval_fingerprint !== fp ||
          claim.approval_idempotency_key !== key) {
        fail("alias provisioning intent is inconsistent", 409);
      }
    } else {
      const reservedName = await this.storage.get(
        reservedSkeletonKey(request.skeleton),
      );
      const reservation = reservedName
        ? await this.storage.get(reservedKey(reservedName))
        : null;
      if (reservation?.enabled) {
        fail("alias is reserved by Witself policy", 409);
      }
    }
    const usage = await this.usageForRealm(
      request.account_id,
      request.realm_id,
    );
    const occupied = usage.realm.customer_allocated;
    if (!resuming && input.alias_limit !== null &&
        occupied >= input.alias_limit) {
      fail("realm email alias limit reached", 403);
    }

    if (!resuming) {
      const pendingClaim = claim;
      const meta = await this.storage.get(META_KEY);
      const intentMutation = await this.mutationEntries(
        meta,
        actor,
        "alias.approval_provisioning",
        request.alias,
        { request_id: request.id, reason },
      );
      request = {
        ...request,
        status: "provisioning",
        updated_at: intentMutation.now,
        decision: {
          action: "approved",
          admin_id: actor.id,
          reason,
          decided_at: intentMutation.now,
        },
      };
      claim = {
        ...claim,
        assignment_kind: "customer",
        assignment_revision: 1,
        customer_activation_intent: true,
        approval_fingerprint: fp,
        approval_idempotency_key: key,
        approval_retry_at_ms: this.now().getTime() + REALM_EMAIL_ALIAS_RETRY_MS,
        approved_by: actor.id,
        approved_at: intentMutation.now,
        updated_at: intentMutation.now,
        plan_suspended: false,
        operational_gate_suspended: false,
        plan_grace_until: null,
      };
      await this.atomic([
        ...intentMutation.entries,
        [requestKey(request.id), request],
        [claimKey(request.alias), claim],
        [approvalDueKey(claim), request.alias],
      ], [], {
        claimUsageTransition: {
          previousClaim: pendingClaim,
          desiredClaim: claim,
          updatedAt: intentMutation.now,
        },
      });
      await this.scheduleNextAlarm().catch(() => {});
    }
    return this.completeApprovalProvisioning(request, claim, {
      actor,
      scope,
      key,
      fingerprint: fp,
      reason,
      activation_enabled: input.activation_enabled === true,
      feature_enabled: input.feature_enabled === true,
      alias_limit: input.alias_limit,
    });
  }

  async completeApprovalProvisioning(request, claim, policy) {
    if (await this.storage.get(projectionIntentKey(claim.alias))) {
      fail("alias has a durable projection still converging", 409);
    }
    try {
      await this.assertRealmTarget(claim.account_id, claim.realm_id);
    } catch (error) {
      if (error instanceof RegistryError && error.status === 404) {
        return this.terminalizeCustomerApproval(request, claim, policy);
      }
      throw error;
    }
    let overPlan = !policy.feature_enabled;
    if (!overPlan && policy.alias_limit !== null) {
      const usage = await this.usageForRealm(claim.account_id, claim.realm_id);
      // A recovery can observe a lower plan after this approval already
      // reserved allocation. Conservatively grace this still-provisioning
      // approval when the O(1) allocated counter is now over capacity. The
      // paginated plan reconciler later restores deterministic oldest-first
      // ordering; the approval path never scans an unlimited realm.
      overPlan = usage.realm.customer_allocated > policy.alias_limit;
    }
    let planSuspended = claim.plan_suspended === true;
    const operationalGateSuspended = !policy.activation_enabled;
    let planGraceUntil = claim.plan_grace_until ?? null;
    if (overPlan && !planSuspended && !planGraceUntil) {
      planGraceUntil = new Date(
        this.now().getTime() + REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_MS,
      ).toISOString();
    } else if (!overPlan) {
      planSuspended = false;
      planGraceUntil = null;
    }

    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      policy.actor,
      "alias.approved",
      request.alias,
      { request_id: request.id, reason: policy.reason },
    );
    const updatedRequest = {
      ...request,
      status: "approved",
      updated_at: mutation.now,
      activated_at: mutation.now,
    };
    const planStateChanged = planSuspended !== (claim.plan_suspended === true) ||
      operationalGateSuspended !==
        (claim.operational_gate_suspended === true) ||
      planGraceUntil !== (claim.plan_grace_until ?? null);
    const updatedClaim = {
      ...claim,
      customer_activation_intent: false,
      approval_retry_at_ms: null,
      approval_failure_count: null,
      approval_last_failure_at: null,
      plan_suspended: planSuspended,
      operational_gate_suspended: operationalGateSuspended,
      plan_grace_until: planGraceUntil,
      assignment_revision: (claim.assignment_revision ?? 0) +
        (planStateChanged ? 1 : 0),
      updated_at: mutation.now,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      request: updatedRequest,
      assignment: publicClaim(updatedClaim),
    };
    mutation.entries.push(
      [requestKey(request.id), updatedRequest],
      [claimKey(request.alias), updatedClaim],
      [`idem:${policy.scope}:${policy.key}`, {
        fingerprint: policy.fingerprint,
        status: 200,
        body,
        alias: request.alias,
      }],
    );
    const oldGraceKey = graceIndexKey(claim);
    const newGraceKey = graceIndexKey(updatedClaim);
    if (newGraceKey) mutation.entries.push([newGraceKey, updatedClaim.alias]);
    // The request remains publicly provisioning until both the exact cell
    // fence and isolated edge route have converged. A durable due row lets the
    // alarm finish or safely suspend the same claim after a crash or gate flip.
    await this.syncClaimProjection(updatedClaim, mutation.meta);
    await this.atomic(mutation.entries, [
      approvalDueKey(claim),
      ...(oldGraceKey && oldGraceKey !== newGraceKey ? [oldGraceKey] : []),
    ], {
      claimUsageTransition: {
        previousClaim: claim,
        desiredClaim: updatedClaim,
        updatedAt: mutation.now,
      },
    });
    await this.scheduleNextAlarm().catch(() => {});
    return json(body);
  }

  async terminalizeCustomerApproval(request, claim, policy) {
    const failureCode = policy.failure_code ?? "realm_not_found";
    const retirementReason = policy.terminal_reason ??
      "the authoritative target realm no longer exists";
    const responseStatus = policy.response_status ?? 409;
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      policy.actor ?? { kind: "system", id: "approval-recovery" },
      "alias.approval_aborted",
      claim.alias,
      { request_id: request.id, failure: failureCode },
    );
    const terminal = {
      ...claim,
      customer_activation_intent: false,
      approval_retry_at_ms: null,
      approval_failure_count: null,
      assignment_revision: Math.max(1, claim.assignment_revision ?? 0) + 1,
      retired_at: mutation.now,
      retirement_reason: retirementReason,
      updated_at: mutation.now,
    };
    const failedRequest = {
      ...request,
      status: "rejected",
      updated_at: mutation.now,
      provisioning_failure: failureCode,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      ...(responseStatus >= 400
        ? { error: "alias approval provisioning was terminally aborted" }
        : {}),
      request: failedRequest,
      assignment: publicClaim(terminal),
    };
    const projection = await this.stageProjectionIntent({
      desiredClaim: terminal,
      scope: policy.scope,
      key: policy.key,
      fingerprint: policy.fingerprint,
      entries: [
        ...mutation.entries,
        [requestKey(request.id), failedRequest],
        [claimKey(terminal.alias), terminal],
        [`idem:${policy.scope}:${policy.key}`, {
          fingerprint: policy.fingerprint,
          status: responseStatus,
          body,
          alias: terminal.alias,
        }],
      ],
      deletes: [approvalDueKey(claim)],
      responseBody: body,
      responseStatus,
      includeCanonical: false,
      allowMissingCellForRetirement: true,
      handoffDeletes: [approvalDueKey(claim)],
      claimUsageTransition: {
        previousClaim: claim,
        desiredClaim: terminal,
        updatedAt: mutation.now,
      },
    });
    return this.drainProjectionIntent(projection);
  }

  async rejectRequest(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    if (!REQUEST_ID_PATTERN.test(input.request_id ?? "")) {
      fail("invalid request_id", 400);
    }
    const reason = validateReason(input.reason, true);
    const fp = fingerprint({ request_id: input.request_id, action: "reject", reason });
    const scope = `request-reject:${input.request_id}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    const request = await this.storage.get(requestKey(input.request_id));
    if (!request) fail("alias request not found", 404);
    if (request.status !== "pending_review") {
      fail("alias request is no longer pending", 409);
    }
    await this.assertPendingCountersReady();
    const claim = await this.storage.get(claimKey(request.alias));
    if (!claim || claim.assignment_kind || claim.request_id !== request.id) {
      fail("alias request claim is inconsistent", 409);
    }
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      actor,
      "alias.rejected",
      request.alias,
      { request_id: request.id, reason },
    );
    const updatedRequest = {
      ...request,
      status: "rejected",
      updated_at: mutation.now,
      decision: {
        action: "rejected",
        admin_id: actor.id,
        reason,
        decided_at: mutation.now,
      },
    };
    const body = { schema_version: SCHEMA_VERSION, request: updatedRequest };
    mutation.entries.push(
      [requestKey(request.id), updatedRequest],
      [`idem:${scope}:${key}`, { fingerprint: fp, status: 200, body }],
    );
    await this.atomic(mutation.entries, [
      claimKey(request.alias),
      claimSkeletonKey(request.skeleton),
      accountClaimIndexKey(claim),
    ], {
      claimUsageTransition: {
        previousClaim: claim,
        desiredClaim: null,
        updatedAt: mutation.now,
      },
    });
    return json(body);
  }

  async listAliases(input) {
    validateActor(input.actor, "platform_admin");
    const listed = input.account_id
      ? await this.boundedIndexedClaims(
        accountClaimIndexPrefix(input.account_id),
        input.cursor,
      )
      : await this.boundedValues(
        "claim:",
        REGISTRY_LIST_LIMIT,
        false,
        input.cursor,
      );
    let aliases = listed.values
      .filter((claim) => claim.assignment_kind &&
        claim.customer_activation_intent !== true)
      .map(publicClaim);
    aliases = aliases.filter((alias) =>
      (!input.account_id || alias.account_id === input.account_id) &&
      (!input.realm_id || alias.realm_id === input.realm_id) &&
      (!input.status || alias.status === input.status)
    );
    aliases.sort((left, right) => left.alias.localeCompare(right.alias));
    return json({
      schema_version: SCHEMA_VERSION,
      aliases,
      truncated: listed.truncated,
      next_cursor: listed.next_cursor ?? null,
    });
  }

  async mutateAlias(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.alias);
    if (!["suspend", "reactivate", "retire"].includes(input.action)) {
      fail("invalid alias action", 400);
    }
    if (input.action === "reactivate" && input.activation_enabled !== true) {
      fail("realm email aliases are not activated on the managed email edge", 409);
    }
    const reason = validateReason(input.reason, true);
    const fp = fingerprint({ alias, action: input.action, reason });
    const scope = `alias-${input.action}:${alias}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    const pending = await this.matchingProjectionIntent(alias, scope, key, fp);
    if (pending) return this.drainProjectionIntent(pending);
    const claim = await this.storage.get(claimKey(alias));
    if (!claim?.assignment_kind) fail("alias assignment not found", 404);
    await this.assertAccountAliasWritesAllowed(claim.account_id);
    if (input.action !== "retire") {
      await this.assertRealmAliasWritesAllowed(
        claim.account_id,
        claim.realm_id,
      );
    }
    if (input.action === "retire" &&
        await this.storage.get(planIntentKey(claim.account_id))) {
      fail("account alias plan is still converging; retirement is fenced", 409);
    }
    if (claim.internal_intent || claim.customer_activation_intent) {
      fail("alias provisioning is still converging", 409);
    }
    if (input.action === "retire") await this.assertPendingCountersReady();
    if (claim.retired_at && input.action !== "retire") {
      fail("retired aliases cannot be reactivated", 409);
    }
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      actor,
      `alias.${input.action}`,
      alias,
      { reason },
    );
    const updated = {
      ...claim,
      assignment_revision: (claim.assignment_revision ?? 0) + 1,
      updated_at: mutation.now,
    };
    if (input.action === "suspend") updated.admin_suspended = true;
    if (input.action === "reactivate") updated.admin_suspended = false;
    if (input.action === "retire") {
      updated.retired_at ??= mutation.now;
      updated.retirement_reason = reason;
    }
    const body = {
      schema_version: SCHEMA_VERSION,
      assignment: publicClaim(updated),
    };
    mutation.entries.push(
      [claimKey(alias), updated],
      [`idem:${scope}:${key}`, {
        fingerprint: fp,
        status: 200,
        body,
        alias,
      }],
    );
    if (input.action === "retire") {
      const fence = await this.storage.get(planFenceKey(claim.account_id));
      if (fence) {
        // Retiring one of the counted aliases can make the next deterministic
        // customer assignment eligible again. Schedule a full bounded pass
        // atomically with the tombstone so a crash cannot leave that alias
        // suspended forever.
        const now = this.now();
        const reconcileIntent = {
          ...this.planIntent({
            account_id: claim.account_id,
            plan_revision: fence.committed_revision,
            plan_snapshot_hash: fence.committed_snapshot_hash,
            feature_enabled: fence.feature_enabled,
            alias_limit: fence.alias_limit,
            activation_enabled:
              String(this.env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") ===
                "true",
          }, "cell_committed"),
          retry_at_ms: now.getTime() + PLAN_RECONCILE_PAGE_RETRY_MS,
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        };
        mutation.entries.push(
          [planIntentKey(claim.account_id), reconcileIntent],
          [planDueKey(reconcileIntent), claim.account_id],
        );
      }
    }
    const intent = await this.stageProjectionIntent({
      desiredClaim: updated,
      scope,
      key,
      fingerprint: fp,
      entries: mutation.entries,
      responseBody: body,
      responseStatus: 200,
      ...(input.action === "retire"
        ? {
          claimUsageTransition: {
            previousClaim: claim,
            desiredClaim: updated,
            updatedAt: mutation.now,
          },
        }
        : {}),
    });
    return this.drainProjectionIntent(intent);
  }

  async assignInternal(input) {
    validateAccountRealm(input);
    await this.assertAccountAliasWritesAllowed(input.account_id);
    await this.assertRealmAliasWritesAllowed(input.account_id, input.realm_id);
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.alias);
    const domain = validateManagedRealmEmailDomain(input.domain);
    const reason = validateReason(input.reason, true);
    if (input.activation_enabled !== true) {
      fail("realm email aliases are not activated on the managed email edge", 409);
    }
    const fp = fingerprint({
      alias,
      domain,
      account_id: input.account_id,
      realm_id: input.realm_id,
      reason,
    });
    const scope = `alias-internal:${alias}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    const pendingProjection = await this.matchingProjectionIntent(
      alias,
      scope,
      key,
      fp,
    );
    if (pendingProjection) return this.drainProjectionIntent(pendingProjection);
    const skeleton = realmEmailAliasSkeleton(alias);
    const reservedName = await this.storage.get(reservedSkeletonKey(skeleton));
    const reservation = reservedName
      ? await this.storage.get(reservedKey(reservedName))
      : null;
    if (!reservation?.enabled || !reservation.internal_assignable) {
      fail("internal assignment requires an enabled internal reservation", 403);
    }
    let intent = await this.storage.get(claimKey(alias));
    const skeletonClaim = await this.storage.get(claimSkeletonKey(skeleton));
    if (intent) {
      if (!intent.internal_intent || intent.assignment_kind !== "internal" ||
          intent.alias !== alias || intent.domain !== domain ||
          intent.account_id !== input.account_id ||
          intent.realm_id !== input.realm_id || skeletonClaim !== alias ||
          intent.internal_idempotency_key !== key ||
          intent.internal_fingerprint !== fp) {
        fail("alias is already claimed or tombstoned", 409);
      }
    } else {
      if (skeletonClaim) fail("alias is already claimed or tombstoned", 409);
      // The cell is the authority for account/realm existence. Validate it
      // before reserving a global name so a typo cannot create an immortal
      // hidden provisioning claim.
      await this.assertRealmTarget(input.account_id, input.realm_id);
      const mintedClaimID = this.newClaimID();
      if (!CLAIM_ID_PATTERN.test(mintedClaimID)) {
        fail("could not mint claim id", 500);
      }
      const createdAt = this.now().toISOString();
      intent = {
        claim_id: mintedClaimID,
        alias,
        domain,
        skeleton,
        account_id: input.account_id,
        realm_id: input.realm_id,
        request_id: null,
        assignment_kind: "internal",
        assignment_revision: 1,
        internal_intent: true,
        internal_idempotency_key: key,
        internal_fingerprint: fp,
        internal_reason: reason,
        internal_actor_id: actor.id,
        internal_retry_at_ms:
          this.now().getTime() + REALM_EMAIL_ALIAS_RETRY_MS,
        admin_suspended: false,
        plan_suspended: false,
        operational_gate_suspended: false,
        lifecycle_suspended: false,
        plan_grace_until: null,
        created_at: createdAt,
        updated_at: createdAt,
        retired_at: null,
      };
      // Persist the invisible provisioning intent before external I/O. This
      // makes claim_id stable across a crash after cell or KV projection while
      // keeping list/public APIs from advertising an active assignment.
      await this.atomic([
        [claimKey(alias), intent],
        [accountClaimIndexKey(intent), alias],
        [claimSkeletonKey(skeleton), alias],
        [internalDueKey(intent), alias],
      ]);
      await this.scheduleNextAlarm().catch(() => {});
    }
    return this.completeInternalAssignment(intent);
  }

  async completeInternalAssignment(intent) {
    if (await this.storage.get(projectionIntentKey(intent.alias))) {
      fail("alias has a durable projection still converging", 409);
    }
    try {
      await this.assertRealmTarget(intent.account_id, intent.realm_id);
    } catch (error) {
      if (error instanceof RegistryError && error.status === 404) {
        return this.terminalizeInternalIntent(
          intent,
          "realm_not_found",
          "the authoritative target realm no longer exists",
        );
      }
      throw error;
    }
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "platform_admin", id: intent.internal_actor_id },
      "alias.internal_assigned",
      intent.alias,
      {
        account_id: intent.account_id,
        realm_id: intent.realm_id,
        reason: intent.internal_reason,
      },
    );
    const operationalGateSuspended =
      String(this.env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") !== "true";
    const claim = {
      ...intent,
      internal_intent: false,
      internal_retry_at_ms: null,
      internal_failure_reason: null,
      approved_by: intent.internal_actor_id,
      approved_at: mutation.now,
      operational_gate_suspended: operationalGateSuspended,
      assignment_revision: (intent.assignment_revision ?? 0) +
        (operationalGateSuspended !==
          (intent.operational_gate_suspended === true)
          ? 1
          : 0),
      updated_at: mutation.now,
      retired_at: null,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      assignment: publicClaim(claim),
    };
    mutation.entries.push(
      [claimKey(claim.alias), claim],
      [`idem:alias-internal:${claim.alias}:${intent.internal_idempotency_key}`, {
        fingerprint: intent.internal_fingerprint,
        status: 201,
        body,
        alias: claim.alias,
      }],
    );
    await this.syncClaimProjection(claim, mutation.meta);
    await this.atomic(mutation.entries, [internalDueKey(intent)]);
    await this.scheduleNextAlarm().catch(() => {});
    return json(body, 201);
  }

  async terminalizeInternalIntent(
    intent,
    failureCode,
    reason,
    scope = `alias-internal:${intent.alias}`,
    responseStatus = 409,
  ) {
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "system", id: "internal-provisioning-recovery" },
      "alias.internal_provisioning_aborted",
      intent.alias,
      { failure: failureCode, reason },
    );
    const terminal = {
      ...intent,
      internal_intent: false,
      internal_retry_at_ms: null,
      internal_failure_reason: failureCode,
      assignment_revision: Math.max(1, intent.assignment_revision ?? 0) + 1,
      retired_at: mutation.now,
      retirement_reason: reason,
      updated_at: mutation.now,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      ...(responseStatus >= 400
        ? { error: "internal alias provisioning was terminally aborted" }
        : {}),
      assignment: publicClaim(terminal),
    };
    const projection = await this.stageProjectionIntent({
      desiredClaim: terminal,
      scope,
      key: intent.internal_idempotency_key,
      fingerprint: intent.internal_fingerprint,
      entries: [
        ...mutation.entries,
        [claimKey(terminal.alias), terminal],
        [`idem:${scope}:${intent.internal_idempotency_key}`, {
          fingerprint: intent.internal_fingerprint,
          status: responseStatus,
          body,
          alias: terminal.alias,
        }],
      ],
      deletes: [internalDueKey(intent)],
      responseBody: body,
      responseStatus,
      includeCanonical: false,
      allowMissingCellForRetirement: true,
      handoffDeletes: [internalDueKey(intent)],
    });
    return this.drainProjectionIntent(projection);
  }

  async abortInternal(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.alias);
    const reason = validateReason(input.reason, true);
    const fingerprintValue = fingerprint({ alias, action: "abort", reason });
    const replay = await this.idempotent(`alias-internal-abort:${alias}`, key, fingerprintValue);
    if (replay) return replay;
    const pendingProjection = await this.matchingProjectionIntent(
      alias,
      `alias-internal-abort:${alias}`,
      key,
      fingerprintValue,
    );
    if (pendingProjection) return this.drainProjectionIntent(pendingProjection);
    const claim = await this.storage.get(claimKey(alias));
    if (claim?.customer_activation_intent === true &&
        claim.assignment_kind === "customer") {
      await this.assertPendingCountersReady();
      const request = await this.storage.get(requestKey(claim.request_id));
      if (request?.status !== "provisioning") {
        fail("customer provisioning intent is inconsistent", 409);
      }
      return this.terminalizeCustomerApproval(request, claim, {
        actor,
        scope: `alias-internal-abort:${alias}`,
        key,
        fingerprint: fingerprintValue,
        failure_code: "admin_aborted",
        terminal_reason: reason,
        response_status: 200,
      });
    }
    if (!claim?.internal_intent || claim.assignment_kind !== "internal") {
      fail("alias provisioning intent not found", 404);
    }
    const response = await this.terminalizeInternalIntent(
      {
        ...claim,
        internal_actor_id: actor.id,
        internal_idempotency_key: key,
        internal_fingerprint: fingerprintValue,
      },
      "admin_aborted",
      reason,
      `alias-internal-abort:${alias}`,
      200,
    );
    return response;
  }

  async reconcileDueInternalAssignments() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "internal-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, alias] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      const claim = await this.storage.get(claimKey(alias));
      const lanes = claim
        ? [
          `account:${claim.account_id}`,
          `realm:${claim.account_id}:${claim.realm_id}`,
          `skeleton:${claim.skeleton}`,
        ]
        : [`skeleton:${alias}`];
      await this.withLanes(lanes, async () => {
        const current = await this.storage.get(claimKey(alias));
        if (!current?.internal_intent || internalDueKey(current) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        try {
          await this.completeInternalAssignment(current);
        } catch {
          const stored = await this.storage.get(claimKey(alias));
          const projection = await this.storage.get(projectionIntentKey(alias));
          if (!stored?.internal_intent || stored.claim_id !== current.claim_id ||
              stored.assignment_revision !== current.assignment_revision ||
              internalDueKey(stored) !== dueKey || projection) {
            return;
          }
          const failureCount = (stored.internal_failure_count ?? 0) + 1;
          const retry = {
            ...stored,
            internal_failure_count: failureCount,
            internal_retry_at_ms:
              this.now().getTime() + retryDelayMs(failureCount),
            internal_last_failure_at: this.now().toISOString(),
          };
          await this.atomic(
            [
              [claimKey(alias), retry],
              [internalDueKey(retry), alias],
            ],
            [dueKey],
          );
        }
      });
    }
  }

  async listReserved(input) {
    validateActor(input.actor, "platform_admin");
    const listed = await this.boundedValues(
      "reserved:",
      REGISTRY_LIST_LIMIT,
      false,
      input.cursor,
    );
    let names = listed.values
      .filter((entry) => isObject(entry) && typeof entry.name === "string");
    if (input.enabled !== undefined) {
      names = names.filter((entry) => entry.enabled === input.enabled);
    }
    if (input.category) {
      names = names.filter((entry) => entry.category === input.category);
    }
    names.sort((left, right) => left.name.localeCompare(right.name));
    const meta = await this.storage.get(META_KEY);
    return json({
      schema_version: SCHEMA_VERSION,
      reserved_policy_version: meta.reserved_policy_version,
      reserved_names: names.map(publicReserved),
      truncated: listed.truncated,
      next_cursor: listed.next_cursor ?? null,
    });
  }

  async getReserved(input) {
    validateActor(input.actor, "platform_admin");
    const alias = normalizeRealmEmailAlias(input.name);
    const entry = await this.storage.get(reservedKey(alias));
    if (!entry) fail("reserved name not found", 404);
    return json({ schema_version: SCHEMA_VERSION, reserved_name: publicReserved(entry) });
  }

  async createReserved(input) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.name);
    const category = validateCategory(input.category);
    const reason = validateReason(input.reason, true);
    const internalAssignable = input.internal_assignable === true;
    const fp = fingerprint({ alias, category, reason, internalAssignable });
    const scope = `reserved-create:${alias}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    if (await this.storage.get(reservedKey(alias))) {
      fail("reserved name already exists; update it instead", 409);
    }
    const skeleton = realmEmailAliasSkeleton(alias);
    const existingSkeleton = await this.storage.get(reservedSkeletonKey(skeleton));
    if (existingSkeleton) fail("reserved name is confusable with another reservation", 409);
    const conflictingAlias = await this.storage.get(claimSkeletonKey(skeleton));
    const conflictClaim = conflictingAlias
      ? await this.storage.get(claimKey(conflictingAlias))
      : null;
    const conflict = conflictClaim?.assignment_kind
      ? publicClaim(conflictClaim)
      : conflictClaim
      ? { alias: conflictClaim.alias, status: "pending_review", account_id: conflictClaim.account_id, realm_id: conflictClaim.realm_id }
      : null;
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      actor,
      "reserved.created",
      alias,
      { category, conflict: Boolean(conflict) },
      { reservedPolicyChange: true },
    );
    const entry = {
      name: alias,
      skeleton,
      category,
      reason,
      version: 1,
      policy_version: mutation.meta.reserved_policy_version,
      enabled: true,
      internal_assignable: internalAssignable,
      created_at: mutation.now,
      updated_at: mutation.now,
      created_by: actor.id,
      updated_by: actor.id,
      retired_at: null,
      claim_conflict: conflict,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      reserved_policy_version: mutation.meta.reserved_policy_version,
      reserved_name: publicReserved(entry),
    };
    mutation.entries.push(
      [reservedKey(alias), entry],
      [reservedSkeletonKey(skeleton), alias],
      [`reserved-history:${alias}:00000001`, entry],
      [`idem:${scope}:${key}`, { fingerprint: fp, status: 201, body }],
    );
    await this.atomic(mutation.entries);
    return json(body, 201);
  }

  async updateReserved(input, retire) {
    const actor = validateActor(input.actor, "platform_admin");
    const key = validateIdempotencyKey(input.idempotency_key);
    const alias = normalizeRealmEmailAlias(input.name);
    const current = await this.storage.get(reservedKey(alias));
    if (!current) fail("reserved name not found", 404);
    const category = retire
      ? current.category
      : input.category === undefined
      ? current.category
      : validateCategory(input.category);
    const reason = retire
      ? validateReason(input.reason, true)
      : input.reason === undefined
      ? current.reason
      : validateReason(input.reason, true);
    const enabled = retire ? false : input.enabled === undefined
      ? current.enabled
      : input.enabled === true;
    const internalAssignable = retire
      ? current.internal_assignable
      : input.internal_assignable === undefined
      ? current.internal_assignable
      : input.internal_assignable === true;
    const fp = fingerprint({ alias, category, reason, enabled, internalAssignable, retire });
    const scope = `reserved-${retire ? "retire" : "update"}:${alias}`;
    const replay = await this.idempotent(scope, key, fp);
    if (replay) return replay;
    if (enabled) {
      const existing = await this.storage.get(reservedSkeletonKey(current.skeleton));
      if (existing && existing !== alias) {
        fail("reserved name is confusable with another reservation", 409);
      }
    }
    const conflictingAlias = await this.storage.get(claimSkeletonKey(current.skeleton));
    const conflictClaim = conflictingAlias
      ? await this.storage.get(claimKey(conflictingAlias))
      : null;
    const conflict = conflictClaim?.assignment_kind
      ? publicClaim(conflictClaim)
      : conflictClaim
      ? { alias: conflictClaim.alias, status: "pending_review", account_id: conflictClaim.account_id, realm_id: conflictClaim.realm_id }
      : null;
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      actor,
      retire ? "reserved.retired" : "reserved.updated",
      alias,
      { category, enabled, conflict: Boolean(conflict), reason },
      { reservedPolicyChange: true },
    );
    const updated = {
      ...current,
      category,
      reason,
      enabled,
      internal_assignable: internalAssignable,
      version: current.version + 1,
      policy_version: mutation.meta.reserved_policy_version,
      updated_at: mutation.now,
      updated_by: actor.id,
      retired_at: enabled ? null : (current.retired_at ?? mutation.now),
      claim_conflict: conflict,
    };
    const body = {
      schema_version: SCHEMA_VERSION,
      reserved_policy_version: mutation.meta.reserved_policy_version,
      reserved_name: publicReserved(updated),
    };
    mutation.entries.push(
      [reservedKey(alias), updated],
      [`reserved-history:${alias}:${String(updated.version).padStart(8, "0")}`, updated],
      [`idem:${scope}:${key}`, { fingerprint: fp, status: 200, body }],
    );
    const deletes = [];
    if (enabled) mutation.entries.push([reservedSkeletonKey(current.skeleton), alias]);
    else deletes.push(reservedSkeletonKey(current.skeleton));
    await this.atomic(mutation.entries, deletes);
    return json(body);
  }

  async listAudit(input) {
    validateActor(input.actor, "platform_admin");
    const scanLimit = Number.isSafeInteger(input.limit)
      ? Math.max(1, Math.min(input.limit, REGISTRY_LIST_LIMIT))
      : 100;
    const listed = await this.boundedValues(
      "audit:",
      scanLimit,
      true,
      input.cursor,
    );
    let events = listed.values;
    if (input.action) events = events.filter((event) => event.action === input.action);
    events.sort((left, right) => right.sequence - left.sequence);
    return json({
      schema_version: SCHEMA_VERSION,
      events: events.slice(0, scanLimit),
      truncated: listed.truncated,
      next_cursor: listed.next_cursor ?? null,
    });
  }

  planIntent(input, state, prepareFit = undefined) {
    return {
      account_id: input.account_id,
      plan_revision: input.plan_revision,
      plan_snapshot_hash: input.plan_snapshot_hash,
      feature_enabled: input.feature_enabled === true,
      alias_limit: input.alias_limit,
      activation_enabled: input.activation_enabled === true,
      state,
      claim_cursor: null,
      realm_positions: {},
      gate_phase: "claims",
      gate_claim_cursor: null,
      gate_canonical_cursor: null,
      operational_gate_complete: input.activation_enabled === true,
      ...(prepareFit === undefined
        ? {}
        : { prepare_fit: structuredClone(prepareFit) }),
      retry_at_ms: this.now().getTime() +
        (input.activation_enabled === true
          ? REALM_EMAIL_ALIAS_RETRY_MS
          : PLAN_RECONCILE_PAGE_RETRY_MS),
      updated_at: this.now().toISOString(),
    };
  }

  async persistPlanIntent(intent, current = null) {
    if (current && current.plan_revision === intent.plan_revision &&
        current.plan_snapshot_hash === intent.plan_snapshot_hash &&
        current.feature_enabled === intent.feature_enabled &&
        current.alias_limit === intent.alias_limit &&
        current.activation_enabled === intent.activation_enabled &&
        current.state === intent.state &&
        fingerprint(current.prepare_fit ?? null) ===
          fingerprint(intent.prepare_fit ?? null)) {
      await this.scheduleNextAlarm().catch(() => {});
      return current;
    }
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "system", id: "plan-lifecycle" },
      "alias.plan_intent_recorded",
      intent.account_id,
      {
        plan_revision: intent.plan_revision,
        plan_snapshot_hash: intent.plan_snapshot_hash,
        state: intent.state,
      },
    );
    const durable = {
      ...intent,
      ...(intent.prepare_fit === undefined
        ? {}
        : {
          prepare_fit: {
            ...intent.prepare_fit,
            authority_revision: intent.prepare_fit.authority_revision ??
              mutation.meta.registry_revision,
          },
        }),
      created_at: current?.created_at ?? mutation.now,
      updated_at: mutation.now,
    };
    mutation.entries.push([planIntentKey(intent.account_id), durable]);
    if (!(durable.state === "awaiting_cell" && durable.prepare_fit)) {
      mutation.entries.push([planDueKey(durable), intent.account_id]);
    }
    await this.atomic(
      mutation.entries,
      current && planDueKey(current) !== planDueKey(durable)
        ? [planDueKey(current)]
        : [],
    );
    await this.scheduleNextAlarm().catch(() => {});
    return durable;
  }

  desiredPlanChanges(claims, intent, initialPositions = {}) {
    // Product-plan suspension and the operational activation gate are
    // independent controls. A plan-disabled route is a stable inactive
    // address; a gate-disabled route is a temporary service condition and
    // must remain retryable at SMTP.
    const allowed = intent.feature_enabled
      ? intent.alias_limit
      : 0;
    const candidates = claims
      .filter((claim) => claim.assignment_kind &&
        claim.customer_activation_intent !== true &&
        claim.internal_intent !== true && !claim.retired_at)
      .sort((left, right) =>
        left.realm_id.localeCompare(right.realm_id) ||
        left.created_at.localeCompare(right.created_at) ||
        left.alias.localeCompare(right.alias)
      );
    const positions = new Map(Object.entries(initialPositions));
    const changed = [];
    const observedNow = this.now().getTime();
    for (const claim of candidates) {
      let planSuspended = claim.plan_suspended === true;
      let graceUntil = claim.plan_grace_until ?? null;
      if (claim.assignment_kind === "customer") {
        const position = positions.get(claim.realm_id) ?? 0;
        positions.set(claim.realm_id, position + 1);
        const shouldRestrict = allowed !== null && position >= allowed;
        if (shouldRestrict) {
          if (!planSuspended && graceUntil &&
              Date.parse(graceUntil) <= observedNow) {
            planSuspended = true;
            graceUntil = null;
          } else if (!planSuspended && !graceUntil) {
            graceUntil = new Date(
              observedNow + REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_MS,
            ).toISOString();
          }
        } else {
          planSuspended = false;
          graceUntil = null;
        }
      } else {
        // Internal platform assignments are not customer plan capacity.
        if (planSuspended || graceUntil) {
          planSuspended = false;
          graceUntil = null;
        }
      }
      const operationalGateSuspended = !intent.activation_enabled;
      if (planSuspended !== (claim.plan_suspended === true) ||
          operationalGateSuspended !==
            (claim.operational_gate_suspended === true) ||
          graceUntil !== (claim.plan_grace_until ?? null)) {
        changed.push({
          previous: claim,
          next: {
            ...claim,
            plan_suspended: planSuspended,
            operational_gate_suspended: operationalGateSuspended,
            plan_grace_until: graceUntil,
          },
        });
      }
    }
    return { allowed, changed, positions: Object.fromEntries(positions) };
  }

  async applyOperationalGate(accountID, intent) {
    if (intent.operational_gate_complete === true) {
      return { assignments: [], complete: true, intent };
    }
    const phase = intent.gate_phase ?? "claims";
    const claimPage = phase === "claims"
      ? await this.claimPageForAccount(accountID, intent.gate_claim_cursor)
      : { claims: [], next_cursor: null };
    if (phase === "claims" && claimPage.claims.length === 0 &&
        claimPage.next_cursor === null) {
      // Crossing an empty index boundary performs no external work and is
      // bounded to this single claims->canonical transition. Fast-complete a
      // dark/empty registry without forcing every unrelated plan mutation to
      // fail once merely to discover that there are no alias rows.
      return this.applyOperationalGate(accountID, {
        ...intent,
        gate_phase: "canonical",
        gate_claim_cursor: null,
        gate_canonical_cursor: null,
      });
    }
    const canonicalPage = phase === "canonical"
      ? await this.canonicalPageForAccount(
        accountID,
        intent.gate_canonical_cursor,
      )
      : { canonicals: [], next_cursor: null };
    const changed = claimPage.claims
      .filter((claim) => claim?.assignment_kind && !claim.retired_at &&
        claim.customer_activation_intent !== true &&
        claim.internal_intent !== true &&
        claim.operational_gate_suspended !== true)
      .map((claim) => ({
        previous: claim,
        next: {
          ...claim,
          operational_gate_suspended: true,
        },
      }));
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "system", id: "activation-gate" },
      "alias.operational_gate_suspended",
      accountID,
      {
        phase,
        changed: changed.length,
        page_size: phase === "claims"
          ? claimPage.claims.length
          : canonicalPage.canonicals.length,
      },
    );
    for (const item of changed) {
      item.next.assignment_revision = (item.previous.assignment_revision ?? 0) + 1;
      item.next.updated_at = mutation.now;
      const previousGrace = graceIndexKey(item.previous);
      const projectionFingerprint = fingerprint({
        operation: "operational_gate",
        account_id: accountID,
        alias: item.next.alias,
        revision: item.next.assignment_revision,
      });
      const projection = await this.stageProjectionIntent({
        desiredClaim: item.next,
        scope: `operational-gate:${accountID}`,
        key: `gate:${item.next.alias}:${item.next.assignment_revision}`,
        fingerprint: projectionFingerprint,
        entries: [[claimKey(item.next.alias), item.next]],
        deletes: previousGrace ? [previousGrace] : [],
        responseBody: { schema_version: SCHEMA_VERSION, converged: true },
        includeCanonical: false,
      });
      await this.drainProjectionIntent(projection);
    }
    if (canonicalPage.canonicals.length > 0) {
      const target = await this.cellTarget(accountID);
      const emailEnabled = await this.canonicalEmailEntitlement(
        accountID,
        target,
      );
      for (const canonical of canonicalPage.canonicals) {
        if (canonical.state === "retired") {
          await this.publishStoredRetiredCanonicalRoute(canonical);
          continue;
        }
        const source = await this.fetchCellCanonicalRoute(
          accountID,
          canonical.realm_id,
          target,
        );
        await this.upsertCanonicalRoute({
          domain: canonical.domain,
          cellRoute: source,
          target,
          emailEnabled,
          minimumControllerRevision: mutation.meta.registry_revision,
        });
      }
    }
    let continued;
    if (phase === "claims" && claimPage.next_cursor !== null) {
      continued = {
        ...intent,
        gate_phase: "claims",
        gate_claim_cursor: claimPage.next_cursor,
      };
    } else if (phase === "claims") {
      continued = {
        ...intent,
        gate_phase: "canonical",
        gate_canonical_cursor: null,
      };
    } else if (canonicalPage.next_cursor !== null) {
      continued = {
        ...intent,
        gate_phase: "canonical",
        gate_canonical_cursor: canonicalPage.next_cursor,
      };
    } else {
      continued = {
        ...intent,
        operational_gate_complete: true,
      };
    }
    continued = {
      ...continued,
      failure_count: 0,
      retry_at_ms: this.now().getTime() + PLAN_RECONCILE_PAGE_RETRY_MS,
      updated_at: mutation.now,
    };
    mutation.entries.push(
      [planIntentKey(accountID), continued],
      [planDueKey(continued), accountID],
    );
    await this.atomic(
      mutation.entries,
      planDueKey(intent) !== planDueKey(continued)
        ? [planDueKey(intent)]
        : [],
    );
    await this.scheduleNextAlarm().catch(() => {});
    return {
      assignments: changed.map(({ next }) => next),
      complete: continued.operational_gate_complete === true,
      intent: continued,
    };
  }

  async applyPlanIntent(intent) {
    if (intent.state === "custom_domain_converging") {
      return this.finishPlanCustomDomainConvergence(intent);
    }
    const page = await this.claimPageForAccount(
      intent.account_id,
      intent.claim_cursor,
    );
    const { allowed, changed, positions } = this.desiredPlanChanges(
      page.claims,
      intent,
      intent.realm_positions,
    );
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "system", id: "plan-lifecycle" },
      "alias.plan_reconciled",
      intent.account_id,
      {
        mode: "complete",
        alias_limit: allowed,
        downgrade_grace_days: REALM_EMAIL_ALIAS_DOWNGRADE_GRACE_DAYS,
        plan_revision: intent.plan_revision,
        plan_snapshot_hash: intent.plan_snapshot_hash,
        changed: changed.length,
        page_size: page.claims.length,
        final_page: page.next_cursor === null,
      },
    );
    const deletes = [planDueKey(intent)];
    for (const item of changed) {
      item.next.assignment_revision = (item.previous.assignment_revision ?? 0) + 1;
      item.next.updated_at = mutation.now;
      const previousGrace = graceIndexKey(item.previous);
      const nextGrace = graceIndexKey(item.next);
      const projectionEntries = [[claimKey(item.next.alias), item.next]];
      const projectionDeletes = [];
      if (previousGrace && previousGrace !== nextGrace) {
        projectionDeletes.push(previousGrace);
      }
      if (nextGrace) projectionEntries.push([nextGrace, item.next.alias]);
      const projectionFingerprint = fingerprint({
        operation: "plan",
        account_id: intent.account_id,
        plan_revision: intent.plan_revision,
        plan_snapshot_hash: intent.plan_snapshot_hash,
        alias: item.next.alias,
        assignment_revision: item.next.assignment_revision,
      });
      const projection = await this.stageProjectionIntent({
        desiredClaim: item.next,
        scope: `plan:${intent.account_id}:${intent.plan_revision}`,
        key: `plan:${intent.plan_revision}:${item.next.alias}`,
        fingerprint: projectionFingerprint,
        entries: projectionEntries,
        deletes: projectionDeletes,
        responseBody: { schema_version: SCHEMA_VERSION, converged: true },
        includeCanonical: false,
      });
      await this.drainProjectionIntent(projection);
    }
    const finalPage = page.next_cursor === null;
    const pendingCustomDomains = finalPage &&
      (await this.storage.list({
        prefix: customDomainSyncAccountPrefix(intent.account_id),
        limit: 1,
      })).size > 0;
    if (finalPage && pendingCustomDomains) {
      const continued = {
        ...intent,
        state: "custom_domain_converging",
        claim_cursor: null,
        realm_positions: positions,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
        updated_at: mutation.now,
      };
      mutation.entries.push(
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      );
    } else if (finalPage) {
      deletes.push(planIntentKey(intent.account_id));
      const fence = {
        account_id: intent.account_id,
        committed_revision: intent.plan_revision,
        committed_snapshot_hash: intent.plan_snapshot_hash,
        feature_enabled: intent.feature_enabled,
        alias_limit: intent.alias_limit,
        activation_enabled: intent.activation_enabled,
        updated_at: mutation.now,
      };
      mutation.entries.push([planFenceKey(intent.account_id), fence]);
    } else {
      const continued = {
        ...intent,
        claim_cursor: page.next_cursor,
        realm_positions: positions,
        failure_count: 0,
        retry_at_ms: this.now().getTime() + PLAN_RECONCILE_PAGE_RETRY_MS,
        updated_at: mutation.now,
      };
      mutation.entries.push(
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      );
    }
    // Each changed claim is durably staged and externally converged above.
    // This final transaction advances only the bounded account cursor/fence;
    // a superseding plan can never reuse a controller revision for a different
    // payload after a crash.
    await this.atomic(mutation.entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return {
      changed: changed.length,
      complete: finalPage && !pendingCustomDomains,
      registry_revision: mutation.meta.registry_revision,
      assignments: changed.map(({ next }) => publicClaim(next)),
    };
  }

  async drainOneAccountCustomDomainSync(accountID) {
    const listed = await this.storage.list({
      prefix: customDomainSyncAccountPrefix(accountID),
      limit: 1,
    });
    const first = [...listed.entries()][0];
    if (!first) return { complete: true };
    const [indexKey, intentKey] = first;
    const intent = typeof intentKey === "string"
      ? await this.storage.get(intentKey)
      : null;
    if (!intent || customDomainSyncAccountKey(intent) !== indexKey) {
      fail("custom-domain alias sync account index is invalid", 503);
    }
    await this.drainCustomDomainSyncIntent(intent);
    const remaining = await this.storage.list({
      prefix: customDomainSyncAccountPrefix(accountID),
      limit: 1,
    });
    return { complete: remaining.size === 0 };
  }

  async finishPlanCustomDomainConvergence(intent) {
    const routes = await this.drainOneAccountCustomDomainSync(
      intent.account_id,
    );
    if (!routes.complete) {
      const continued = {
        ...intent,
        state: "custom_domain_converging",
        failure_count: 0,
        retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.atomic([
        [planIntentKey(intent.account_id), continued],
        [planDueKey(continued), intent.account_id],
      ], [planDueKey(intent)]);
      await this.scheduleNextAlarm().catch(() => {});
      return {
        changed: 0,
        complete: false,
        registry_revision: (await this.storage.get(META_KEY)).registry_revision,
        assignments: [],
      };
    }
    await this.atomic([[planFenceKey(intent.account_id), {
      account_id: intent.account_id,
      committed_revision: intent.plan_revision,
      committed_snapshot_hash: intent.plan_snapshot_hash,
      feature_enabled: intent.feature_enabled,
      alias_limit: intent.alias_limit,
      activation_enabled: intent.activation_enabled,
      updated_at: this.now().toISOString(),
    }]], [planIntentKey(intent.account_id), planDueKey(intent)]);
    await this.scheduleNextAlarm().catch(() => {});
    return {
      changed: 0,
      complete: true,
      registry_revision: (await this.storage.get(META_KEY)).registry_revision,
      assignments: [],
    };
  }

  async reconcileDuePlanIntents() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "plan-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, accountID] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      await this.withLane(`account:${accountID}`, async () => {
        const current = await this.storage.get(planIntentKey(accountID));
        if (!current || planDueKey(current) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        try {
          if (current.state === "custom_domain_converging") {
            await this.applyPlanIntent(current);
            return;
          }
          if (current.state === "awaiting_cell" &&
              current.activation_enabled !== true &&
              current.operational_gate_complete !== true) {
            // The bridge has not touched the cell yet: it is waiting for this
            // durable, paginated kill-switch projection to finish. Advance one
            // page and leave the candidate fence intact.
            await this.applyOperationalGate(accountID, current);
            return;
          }
          const snapshot = await this.fetchAuthoritativePlan(accountID);
          const entitlement = realmEmailAliasEntitlement(snapshot);
          const fence = await this.storage.get(planFenceKey(accountID));
          if (fence && snapshot.revision < fence.committed_revision) {
            fail("cell plan fence regressed behind alias policy", 502);
          }
          if (current.state === "awaiting_cell" &&
              (snapshot.revision !== current.plan_revision ||
                snapshot.snapshot_hash !== current.plan_snapshot_hash) &&
              (current.failure_count ?? 0) < 4) {
            // A one-second alarm can race the bridge's in-flight cell write.
            // Preserve the candidate rather than replacing it with the old
            // authoritative fence. Repeated mismatches eventually recover to
            // the cell below, so a rejected/lost request cannot wedge forever.
            fail("candidate plan has not reached the cell yet", 409);
          }
          const recovered = await this.persistPlanIntent({
            ...this.planIntent({
              account_id: accountID,
              plan_revision: snapshot.revision,
              plan_snapshot_hash: snapshot.snapshot_hash,
              feature_enabled: entitlement.enabled,
              alias_limit: entitlement.limit,
              activation_enabled:
                String(this.env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") === "true",
            }, "cell_committed"),
          }, current);
          await this.applyPlanIntent(recovered);
        } catch {
          const failureCount = (current.failure_count ?? 0) + 1;
          const retry = {
            ...current,
            failure_count: failureCount,
            retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
            last_failure_at: this.now().toISOString(),
          };
          await this.atomic(
            [
              [planIntentKey(accountID), retry],
              [planDueKey(retry), accountID],
            ],
            [dueKey],
          );
        }
      });
    }
  }

  lifecycleOrder(action) {
    return action === "suspend"
      ? 1
      : action === "republish"
      ? 2
      : action === "retire"
      ? 3
      : 0;
  }

  validateLifecycleReconciliation(input) {
    if (!ACCOUNT_ID_PATTERN.test(input?.account_id ?? "") ||
        !IDEMPOTENCY_KEY_PATTERN.test(input?.operation_id ?? "") ||
        !Number.isSafeInteger(input?.epoch) || input.epoch < 0 ||
        !["suspend", "republish", "retire"].includes(input?.action) ||
        typeof input?.activation_enabled !== "boolean") {
      fail("invalid account lifecycle alias reconciliation", 400);
    }
  }

  lifecycleFenceRelation(left, right) {
    if (!right) return 1;
    if (left.epoch !== right.epoch) return left.epoch < right.epoch ? -1 : 1;
    if (left.operation_id !== right.operation_id) {
      fail("account lifecycle epoch conflicts with an existing alias fence", 409);
    }
    const leftOrder = this.lifecycleOrder(left.action);
    const rightOrder = this.lifecycleOrder(right.action);
    return leftOrder === rightOrder ? 0 : leftOrder < rightOrder ? -1 : 1;
  }

  async applyLifecycleIntent(intent) {
    const phase = intent.phase ?? "claims";
    if (phase === "custom_domain_converging") {
      return this.finishLifecycleCustomDomainConvergence(intent);
    }
    const claimPage = phase === "claims"
      ? await this.claimPageForAccount(
        intent.account_id,
        intent.claim_cursor,
        LIFECYCLE_RECONCILE_CLAIM_LIMIT,
      )
      : { claims: [], next_cursor: null };
    if (phase === "claims" && claimPage.claims.length === 0 &&
        claimPage.next_cursor === null) {
      // As above, one empty phase transition is bounded and side-effect free.
      // This keeps account archive/move/restore usable before any aliases or
      // canonical projections have ever been created.
      return this.applyLifecycleIntent({
        ...intent,
        phase: "canonical",
        claim_cursor: null,
        canonical_cursor: null,
      });
    }
    const canonicalPage = phase === "canonical"
      ? await this.canonicalPageForAccount(
        intent.account_id,
        intent.canonical_cursor,
        LIFECYCLE_RECONCILE_CLAIM_LIMIT,
      )
      : { canonicals: [], next_cursor: null };
    const meta = await this.storage.get(META_KEY);
    const mutation = await this.mutationEntries(
      meta,
      { kind: "system", id: "account-lifecycle" },
      `alias.lifecycle_${intent.action}`,
      intent.account_id,
      {
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        phase,
        page_size: phase === "claims"
          ? claimPage.claims.length
          : canonicalPage.canonicals.length,
        final_page: phase === "claims"
          ? claimPage.next_cursor === null
          : canonicalPage.next_cursor === null,
      },
    );
    if (intent.action === "retire" && claimPage.claims.length > 0) {
      await this.assertPendingCountersReady();
    }
    let changed = 0;
    for (const claim of claimPage.claims) {
      if (!claim?.assignment_kind) {
        if (intent.action === "retire") {
          // Pending-review claims have no route to retire, but they still own
          // their request, claim/skeleton indexes, and bounded usage-counter
          // membership. Silently skipping one would let an account-close
          // fence complete while leaving permanent unreachable authority.
          // Keep the close intent retryable until an administrator explicitly
          // rejects (or otherwise terminally resolves) the request. Ordinary
          // archive/move suspension remains safe because an unassigned claim
          // is not deliverable and must survive the move for later review.
          fail(
            "account close is blocked by a pending email alias request",
            409,
            "realm_email_alias_pending_request_blocks_account_close",
          );
        }
        continue;
      }
      if (claim.customer_activation_intent === true ||
          claim.internal_intent === true) {
        fail("account has an alias provisioning intent still converging", 409);
      }
      const lifecycleSuspended = intent.action === "retire"
        ? true
        : intent.action === "suspend" && !claim.retired_at;
      const operationalGateSuspended = !intent.activation_enabled &&
          !claim.retired_at
        ? true
        : false;
      const retiring = intent.action === "retire" && !claim.retired_at;
      const stateChanged =
        lifecycleSuspended !== (claim.lifecycle_suspended === true) ||
        operationalGateSuspended !==
          (claim.operational_gate_suspended === true) ||
        retiring;
      const desired = {
        ...claim,
        lifecycle_suspended: lifecycleSuspended,
        operational_gate_suspended: operationalGateSuspended,
        lifecycle_fence: {
          operation_id: intent.operation_id,
          epoch: intent.epoch,
          action: intent.action,
        },
        assignment_revision: (claim.assignment_revision ?? 0) +
          (stateChanged ? 1 : 0),
        updated_at: mutation.now,
        ...(retiring
          ? {
            retired_at: mutation.now,
            retirement_reason: "account closed",
          }
          : {}),
      };
      // Suspend only writes claims whose effective lifecycle state changes.
      // Republish always verifies every exact claim/tombstone at the new cell,
      // even when its controller revision already existed in the archive.
      if (stateChanged || intent.action === "republish") {
        const projectionFingerprint = fingerprint({
          operation: "account_lifecycle",
          account_id: intent.account_id,
          operation_id: intent.operation_id,
          epoch: intent.epoch,
          action: intent.action,
          alias: desired.alias,
          assignment_revision: desired.assignment_revision,
        });
        const projection = await this.stageProjectionIntent({
          desiredClaim: desired,
          scope:
            `lifecycle:${intent.account_id}:${intent.epoch}:${intent.action}`,
          key: `lifecycle:${intent.epoch}:${intent.action}:${desired.alias}`,
          fingerprint: projectionFingerprint,
          entries: [[claimKey(desired.alias), desired]],
          deletes: retiring && graceIndexKey(claim)
            ? [graceIndexKey(claim)]
            : [],
          responseBody: { schema_version: SCHEMA_VERSION, converged: true },
          includeCanonical: false,
          ...(retiring
            ? {
              claimUsageTransition: {
                previousClaim: claim,
                desiredClaim: desired,
                updatedAt: mutation.now,
              },
            }
            : {}),
        });
        await this.drainProjectionIntent(projection);
        changed += 1;
      } else {
        await this.atomic([[claimKey(desired.alias), desired]]);
      }
    }

    if (canonicalPage.canonicals.length > 0) {
      const activeCanonicals = canonicalPage.canonicals.filter((canonical) =>
        canonical.state !== "retired"
      );
      const target = activeCanonicals.length > 0
        ? await this.cellTarget(intent.account_id)
        : null;
      const emailEnabled = intent.action === "republish" &&
          activeCanonicals.length > 0
        ? await this.canonicalEmailEntitlement(intent.account_id, target)
        : false;
      for (const canonical of canonicalPage.canonicals) {
        if (canonical.state === "retired") {
          await this.publishStoredRetiredCanonicalRoute(canonical);
          continue;
        }
        const source = await this.fetchCellCanonicalRoute(
          intent.account_id,
          canonical.realm_id,
          target,
        );
        await this.upsertCanonicalRoute({
          domain: canonical.domain,
          cellRoute: source,
          target,
          emailEnabled,
          ...(intent.action === "suspend"
            ? {
              forcedPolicy: {
                state: "suspended",
                suspension_disposition: "retry",
              },
            }
            : intent.action === "retire"
            ? {
              forcedPolicy: {
                state: "retired",
                suspension_disposition: null,
              },
            }
            : {}),
          minimumControllerRevision: mutation.meta.registry_revision,
          lifecycleFence: {
            operation_id: intent.operation_id,
            epoch: intent.epoch,
            action: intent.action,
          },
        });
        changed += 1;
      }
    }

    const deletes = [lifecycleDueKey(intent)];
    let complete = false;
    let continued = null;
    if (phase === "claims" && claimPage.next_cursor !== null) {
      continued = {
        ...intent,
        phase: "claims",
        claim_cursor: claimPage.next_cursor,
      };
    } else if (phase === "claims") {
      continued = {
        ...intent,
        phase: "canonical",
        canonical_cursor: null,
      };
    } else if (canonicalPage.next_cursor !== null) {
      continued = {
        ...intent,
        phase: "canonical",
        canonical_cursor: canonicalPage.next_cursor,
      };
    } else if ((await this.storage.list({
      prefix: customDomainSyncAccountPrefix(intent.account_id),
      limit: 1,
    })).size > 0) {
      continued = {
        ...intent,
        phase: "custom_domain_converging",
        claim_cursor: null,
        canonical_cursor: null,
      };
    } else {
      complete = true;
      deletes.push(lifecycleIntentKey(intent.account_id));
      mutation.entries.push([lifecycleFenceKey(intent.account_id), {
        account_id: intent.account_id,
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
        completed_at: mutation.now,
      }]);
    }
    if (continued) {
      continued = {
        ...continued,
        failure_count: 0,
        retry_at_ms: this.now().getTime() +
          LIFECYCLE_RECONCILE_PAGE_RETRY_MS,
        updated_at: mutation.now,
      };
      mutation.entries.push(
        [lifecycleIntentKey(intent.account_id), continued],
        [lifecycleDueKey(continued), intent.account_id],
      );
    }
    await this.atomic(mutation.entries, deletes);
    await this.scheduleNextAlarm().catch(() => {});
    return {
      schema_version: SCHEMA_VERSION,
      account_id: intent.account_id,
      operation_id: intent.operation_id,
      epoch: intent.epoch,
      action: intent.action,
      changed,
      complete,
      registry_revision: mutation.meta.registry_revision,
    };
  }

  async finishLifecycleCustomDomainConvergence(intent) {
    const routes = await this.drainOneAccountCustomDomainSync(
      intent.account_id,
    );
    if (!routes.complete) {
      const continued = {
        ...intent,
        phase: "custom_domain_converging",
        failure_count: 0,
        retry_at_ms: this.now().getTime() + CUSTOM_DOMAIN_SYNC_RETRY_MS,
        updated_at: this.now().toISOString(),
      };
      await this.atomic([
        [lifecycleIntentKey(intent.account_id), continued],
        [lifecycleDueKey(continued), intent.account_id],
      ], [lifecycleDueKey(intent)]);
      await this.scheduleNextAlarm().catch(() => {});
      return {
        schema_version: SCHEMA_VERSION,
        account_id: intent.account_id,
        operation_id: intent.operation_id,
        epoch: intent.epoch,
        action: intent.action,
        changed: 0,
        complete: false,
        registry_revision: (await this.storage.get(META_KEY)).registry_revision,
      };
    }
    await this.atomic([[lifecycleFenceKey(intent.account_id), {
      account_id: intent.account_id,
      operation_id: intent.operation_id,
      epoch: intent.epoch,
      action: intent.action,
      completed_at: this.now().toISOString(),
    }]], [lifecycleIntentKey(intent.account_id), lifecycleDueKey(intent)]);
    await this.scheduleNextAlarm().catch(() => {});
    return {
      schema_version: SCHEMA_VERSION,
      account_id: intent.account_id,
      operation_id: intent.operation_id,
      epoch: intent.epoch,
      action: intent.action,
      changed: 0,
      complete: true,
      registry_revision: (await this.storage.get(META_KEY)).registry_revision,
    };
  }

  async reconcileAccountLifecycle(input) {
    this.validateLifecycleReconciliation(input);
    const requested = {
      account_id: input.account_id,
      operation_id: input.operation_id,
      epoch: input.epoch,
      action: input.action,
      activation_enabled: input.activation_enabled,
    };
    const fence = await this.storage.get(lifecycleFenceKey(input.account_id));
    const fenceRelation = this.lifecycleFenceRelation(requested, fence);
    if (fenceRelation < 0) {
      fail("account lifecycle alias fence is stale", 409);
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
    let intent = await this.storage.get(lifecycleIntentKey(input.account_id));
    if (intent) {
      const relation = this.lifecycleFenceRelation(requested, intent);
      if (relation < 0) fail("account lifecycle alias intent is stale", 409);
      if (relation > 0) {
        fail("an earlier account lifecycle alias projection is still converging", 409);
      }
    } else {
      const now = this.now();
      intent = {
        ...requested,
        phase: "claims",
        claim_cursor: null,
        canonical_cursor: null,
        failure_count: 0,
        retry_at_ms: now.getTime(),
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      };
      await this.atomic([
        [lifecycleIntentKey(input.account_id), intent],
        [lifecycleDueKey(intent), input.account_id],
      ]);
      await this.scheduleNextAlarm().catch(() => {});
    }
    return json(await this.applyLifecycleIntent(intent));
  }

  async reconcileDueLifecycles() {
    const now = this.now().getTime();
    const listed = await this.storage.list({
      prefix: "lifecycle-due:",
      limit: ALARM_BATCH_LIMIT,
    });
    for (const [dueKey, accountID] of listed) {
      const retryAt = Number(dueKey.split(":", 3)[1]);
      if (!Number.isFinite(retryAt)) {
        await this.storage.delete(dueKey);
        continue;
      }
      if (retryAt > now) break;
      await this.withLane(`account:${accountID}`, async () => {
        const intent = await this.storage.get(lifecycleIntentKey(accountID));
        if (!intent || lifecycleDueKey(intent) !== dueKey) {
          await this.storage.delete(dueKey);
          return;
        }
        try {
          await this.applyLifecycleIntent(intent);
        } catch {
          const failureCount = (intent.failure_count ?? 0) + 1;
          const retry = {
            ...intent,
            failure_count: failureCount,
            retry_at_ms: this.now().getTime() + retryDelayMs(failureCount),
            last_failure_at: this.now().toISOString(),
          };
          await this.atomic(
            [
              [lifecycleIntentKey(accountID), retry],
              [lifecycleDueKey(retry), accountID],
            ],
            [dueKey],
          );
        }
      });
    }
  }

  async reconcilePlan(input) {
    if (!ACCOUNT_ID_PATTERN.test(input?.account_id ?? "")) {
      fail("invalid account_id", 400);
    }
    if (!["prepare", "restrict_only", "complete"].includes(input.mode)) {
      fail("invalid reconciliation mode", 400);
    }
    if (input.mode === "prepare" &&
        (typeof input.feature_enabled !== "boolean" ||
          typeof input.activation_enabled !== "boolean")) {
      fail("invalid prepare entitlement", 400);
    }
    if (!validAliasLimit(input.alias_limit)) fail("invalid alias_limit", 400);
    if (!validPlanFence(input.plan_revision, input.plan_snapshot_hash)) {
      fail("invalid plan revision fence", 400);
    }
    await this.assertAccountAliasWritesAllowed(input.account_id);
    const recoveryProvided = input.recover_pending_revision !== undefined ||
      input.recover_pending_snapshot_hash !== undefined;
    if (recoveryProvided && !validPlanFence(
      input.recover_pending_revision,
      input.recover_pending_snapshot_hash,
    )) {
      fail("invalid recovery plan fence", 400);
    }

    const fence = await this.storage.get(planFenceKey(input.account_id));
    const pending = await this.storage.get(planIntentKey(input.account_id));
    const relation = fence
      ? comparePlanFence(input.plan_revision, fence.committed_revision)
      : 1;
    if (relation === 0 &&
        input.plan_snapshot_hash !== fence.committed_snapshot_hash) {
      fail("plan revision conflicts with the committed alias policy fence", 409);
    }
    if (relation === 0 &&
        ((input.feature_enabled === true) !== fence.feature_enabled ||
          input.alias_limit !== fence.alias_limit ||
          (input.activation_enabled === true) !== fence.activation_enabled)) {
      fail("plan entitlement conflicts with the committed alias policy fence", 409);
    }
    const recoversPending = recoveryProvided && pending &&
      pending.plan_revision === input.recover_pending_revision &&
      pending.plan_snapshot_hash === input.recover_pending_snapshot_hash;

    if (input.mode === "prepare") {
      const maximum = input.feature_enabled === true ? input.alias_limit : 0;
      if (relation <= 0) {
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: input.account_id,
          mode: input.mode,
          plan_revision: input.plan_revision,
          plan_snapshot_hash: input.plan_snapshot_hash,
          prepared: false,
          pending: false,
          stale: true,
          complete: true,
        });
      }
      if (pending) {
        const pendingRelation = comparePlanFence(
          input.plan_revision,
          pending.plan_revision,
        );
        if (pendingRelation === 0 &&
            input.plan_snapshot_hash !== pending.plan_snapshot_hash) {
          fail("plan revision conflicts with the pending alias policy fence", 409);
        }
        if (pendingRelation === 0 &&
            ((input.feature_enabled === true) !== pending.feature_enabled ||
              input.alias_limit !== pending.alias_limit ||
              (input.activation_enabled === true) !==
                pending.activation_enabled)) {
          fail("plan entitlement conflicts with the pending alias policy fence", 409);
        }
        if (pendingRelation < 0) {
          return json({
            schema_version: SCHEMA_VERSION,
            account_id: input.account_id,
            mode: input.mode,
            plan_revision: input.plan_revision,
            plan_snapshot_hash: input.plan_snapshot_hash,
            prepared: false,
            pending: false,
            stale: true,
            complete: true,
          });
        }
        if (pendingRelation === 0 && pending.state === "awaiting_cell" &&
            pending.prepare_fit !== undefined) {
          if (!validRealmEmailAliasPrepareFit(
            pending.prepare_fit,
            maximum,
          ) || pending.prepare_fit.over_limit_count !== 0) {
            fail("persisted realm email alias prepare evidence is invalid", 503);
          }
          return json({
            schema_version: SCHEMA_VERSION,
            account_id: input.account_id,
            mode: input.mode,
            plan_revision: input.plan_revision,
            plan_snapshot_hash: input.plan_snapshot_hash,
            prepared: true,
            pending: true,
            stale: false,
            complete: true,
            fit: pending.prepare_fit,
            registry_revision: pending.prepare_fit.authority_revision,
          });
        }
        fail("realm email alias policy is still converging", 409);
      }
      const fit = await this.collectPlanFitEvidence(input.account_id, maximum);
      if (!validRealmEmailAliasPrepareFit(fit, maximum, false)) {
        fail("realm email alias prepare evidence is invalid", 503);
      }
      if (fit.over_limit_count > 0) {
        fail(
          "account does not fit the target realm email alias allocation",
          409,
          "plan_fit_failed",
          {
            account_id: input.account_id,
            mode: input.mode,
            plan_revision: input.plan_revision,
            plan_snapshot_hash: input.plan_snapshot_hash,
            prepared: false,
            pending: false,
            stale: false,
            complete: true,
            fit,
          },
        );
      }
      const durable = await this.persistPlanIntent(
        this.planIntent(input, "awaiting_cell", fit),
      );
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: input.account_id,
        mode: input.mode,
        plan_revision: input.plan_revision,
        plan_snapshot_hash: input.plan_snapshot_hash,
        prepared: true,
        pending: true,
        stale: false,
        complete: true,
        fit: durable.prepare_fit,
        registry_revision: durable.prepare_fit.authority_revision,
      });
    }

    if (input.mode === "complete" && recoversPending &&
        pending.prepare_fit !== undefined &&
        comparePlanFence(input.plan_revision, pending.plan_revision) < 0) {
      const maximum = pending.feature_enabled ? pending.alias_limit : 0;
      if (!validRealmEmailAliasPrepareFit(pending.prepare_fit, maximum)) {
        fail("persisted realm email alias prepare evidence is invalid", 503);
      }
      await this.atomic([], [
        planIntentKey(input.account_id),
        planDueKey(pending),
      ]);
      await this.scheduleNextAlarm().catch(() => {});
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: input.account_id,
        mode: input.mode,
        changed: 0,
        stale: false,
        complete: true,
        recovered: true,
        registry_revision: (await this.storage.get(META_KEY)).registry_revision,
      });
    }

    if (input.mode === "restrict_only") {
      if (pending?.prepare_fit !== undefined &&
          pending.plan_revision === input.plan_revision &&
          pending.plan_snapshot_hash === input.plan_snapshot_hash) {
        fail("realm email alias policy is still prepared for the cell", 409);
      }
      let stale = relation <= 0;
      if (pending) {
        const pendingRelation = comparePlanFence(
          input.plan_revision,
          pending.plan_revision,
        );
        if (pendingRelation === 0 &&
            input.plan_snapshot_hash !== pending.plan_snapshot_hash) {
          fail("plan revision conflicts with the pending alias policy fence", 409);
        }
        if (pendingRelation === 0 &&
            ((input.feature_enabled === true) !== pending.feature_enabled ||
              input.alias_limit !== pending.alias_limit ||
              (input.activation_enabled === true) !==
                pending.activation_enabled)) {
          fail("plan entitlement conflicts with the pending alias policy fence", 409);
        }
        stale ||= pendingRelation <= 0;
      }
      let durable = pending;
      if (!stale) {
        durable = await this.persistPlanIntent(
          this.planIntent(input, "awaiting_cell"),
          pending,
        );
      } else if (!input.activation_enabled && !durable) {
        const source = fence
          ? {
            ...input,
            plan_revision: fence.committed_revision,
            plan_snapshot_hash: fence.committed_snapshot_hash,
            feature_enabled: fence.feature_enabled,
            alias_limit: fence.alias_limit,
          }
          : input;
        durable = await this.persistPlanIntent(
          this.planIntent(source, "awaiting_cell"),
          null,
        );
      }
      const gateResult = input.activation_enabled
        ? { assignments: [], complete: true, intent: durable }
        : await this.applyOperationalGate(input.account_id, durable ?? input);
      const meta = await this.storage.get(META_KEY);
      await this.scheduleNextAlarm().catch(() => {});
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: input.account_id,
        mode: input.mode,
        changed: gateResult.assignments.length,
        pending: !stale,
        stale,
        operational_gate_complete: gateResult.complete,
        registry_revision: meta.registry_revision,
        ...(gateResult.assignments.length > 0
          ? { assignments: gateResult.assignments.map(publicClaim) }
          : {}),
      });
    }

    if (pending?.state === "awaiting_cell" &&
        pending.activation_enabled !== true &&
        pending.operational_gate_complete !== true) {
      const gateResult = await this.applyOperationalGate(
        input.account_id,
        pending,
      );
      if (!gateResult.complete) {
        fail("realm email alias activation gate is still converging", 409);
      }
    }

    if (relation < 0) {
      const meta = await this.storage.get(META_KEY);
      return json({
        schema_version: SCHEMA_VERSION,
        account_id: input.account_id,
        mode: input.mode,
        changed: 0,
        stale: true,
        registry_revision: meta.registry_revision,
      });
    }
    if (pending && !recoversPending) {
      const pendingRelation = comparePlanFence(
        input.plan_revision,
        pending.plan_revision,
      );
      if (pendingRelation === 0 &&
          input.plan_snapshot_hash !== pending.plan_snapshot_hash) {
        fail("plan revision conflicts with the pending alias policy fence", 409);
      }
      if (pendingRelation === 0 &&
          ((input.feature_enabled === true) !== pending.feature_enabled ||
            input.alias_limit !== pending.alias_limit ||
            (input.activation_enabled === true) !== pending.activation_enabled)) {
        fail("plan entitlement conflicts with the pending alias policy fence", 409);
      }
      if (pendingRelation < 0 ||
          (relation === 0 && pendingRelation < 0)) {
        const meta = await this.storage.get(META_KEY);
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: input.account_id,
          mode: input.mode,
          changed: 0,
          stale: true,
          registry_revision: meta.registry_revision,
        });
      }
      if (pendingRelation === 0 &&
          pending.state === "custom_domain_converging") {
        const result = await this.applyPlanIntent(pending);
        return json({
          schema_version: SCHEMA_VERSION,
          account_id: input.account_id,
          mode: input.mode,
          stale: false,
          ...result,
        });
      }
    }
    const prepareFit = pending &&
        pending.plan_revision === input.plan_revision &&
        pending.plan_snapshot_hash === input.plan_snapshot_hash
      ? pending.prepare_fit
      : undefined;
    const committed = await this.persistPlanIntent(
      this.planIntent(input, "cell_committed", prepareFit),
      pending,
    );
    const result = await this.applyPlanIntent(committed);
    return json({
      schema_version: SCHEMA_VERSION,
      account_id: input.account_id,
      mode: input.mode,
      stale: false,
      ...result,
    });
  }
}
