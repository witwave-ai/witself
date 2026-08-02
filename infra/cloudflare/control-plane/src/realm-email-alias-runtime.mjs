const SCHEMA_VERSION = "witself.realm-email-alias.v1";
const META_KEY = "meta";
const REGISTRY_OBJECT_NAME = "global";
const ALIAS_PATTERN = /^[a-z0-9](?:[a-z0-9-]{1,14}[a-z0-9])$/;
const CANONICAL_REALM_LABEL_PATTERN = /^[a-z2-7]{16}$/;
const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
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

function errorResponse(message, status) {
  return json({ schema_version: SCHEMA_VERSION, error: message }, status);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

class RegistryError extends Error {
  constructor(message, status = 500) {
    super(message);
    this.name = "RegistryError";
    this.status = status;
  }
}

function fail(message, status = 500) {
  throw new RegistryError(message, status);
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
    schema_version: 1,
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

function claimKey(alias) {
  return `claim:${alias}`;
}

function claimSkeletonKey(skeleton) {
  return `claim-skeleton:${skeleton}`;
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
  return namespace.get(namespace.idFromName(REGISTRY_OBJECT_NAME));
}

export async function reconcileRealmEmailAliasesForPlan(
  env,
  accountID,
  snapshot,
  mode,
  options = {},
) {
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub) return { skipped: true };
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !validPlanFence(snapshot?.revision, snapshot?.snapshot_hash) ||
      !Array.isArray(snapshot?.features) || !isObject(snapshot?.limits)) {
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
  if (!response.ok) {
    const body = await response.json().catch(() => null);
    throw new Error(body?.error ?? "realm email alias plan reconciliation failed");
  }
  const body = await response.json();
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
      !["suspend", "republish"].includes(action)) {
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
    this.lanes = new Map();
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
      case "/account-lifecycle/reconcile":
        return [`account:${input.account_id}`];
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
        case "/account-lifecycle/reconcile":
          return await this.reconcileAccountLifecycle(input);
        default:
          return errorResponse("registry endpoint not found", 404);
        }
      };
      const lanes = await this.operationLanes(path, input);
      if (lanes.length > 0) {
        return await this.withLanes(lanes, execute);
      }
      return await execute();
    } catch (error) {
      return errorResponse(
        String(error?.message ?? error),
        error instanceof RegistryError ? error.status : 500,
      );
    }
  }

  async atomic(entries, deletes = []) {
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

  async ensureSeeded() {
    const current = await this.storage.get(META_KEY);
    if (current?.seeded) return current;
    const now = this.now().toISOString();
    const meta = {
      schema_version: SCHEMA_VERSION,
      seeded: true,
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

  async mutationEntries(meta, actor, action, target, metadata = {}, options = {}) {
    // Only this tiny local section is global. It durably reserves unique
    // registry/audit revisions before any caller performs cell or KV I/O, then
    // releases immediately so a slow cell never head-of-line blocks the DO.
    return this.withLane("registry:metadata", async () => {
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
      await this.atomic([
        [META_KEY, next],
        [auditKey, preparedAudit],
      ]);
      return {
        now,
        meta: next,
        entries: [[auditKey, committedAudit]],
      };
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

  async fetchAuthoritativePlan(accountID) {
    const target = await this.cellTarget(accountID);
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

  async publishRoute(value) {
    try {
      await this.routingDirectory().put(
        realmEmailRouteKey(value.domain, value.realm_label),
        JSON.stringify(value),
      );
    } catch (error) {
      if (error instanceof RegistryError) throw error;
      fail("agent email routing projection failed", 502);
    }
    return value;
  }

  async publishClaimRoute(claim, _meta, target, updatedAt = this.now().toISOString()) {
    if (!claim?.assignment_kind) return null;
    const state = cellAliasState(claim);
    const value = buildRealmEmailRouteProjection({
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

  async ensureCanonicalRoute(claim, meta, target, desiredState = "applied") {
    const realmLabel = claim.realm_id.slice("realm_".length);
    const key = `canonical:${claim.domain}:${realmLabel}`;
    const current = await this.storage.get(key);
    const revision = Math.max(
      current?.controller_revision ?? 0,
      meta.registry_revision,
    );
    const updatedAt = this.now().toISOString();
    const canonical = {
      domain: claim.domain,
      account_id: claim.account_id,
      realm_id: claim.realm_id,
      realm_label: realmLabel,
      state: desiredState,
      controller_revision: revision,
      updated_at: updatedAt,
    };
    const value = buildRealmEmailRouteProjection({
      ...canonical,
      route_kind: "canonical",
      ...(desiredState === "suspended"
        ? { suspension_disposition: "retry" }
        : {}),
      cell_audience: target?.cell_audience,
      ingest_url: target?.ingest_url,
    });
    await this.publishRoute(value);
    await this.atomic([
      [key, canonical],
      [accountCanonicalIndexKey(canonical), key],
    ]);
    return value;
  }

  async syncClaimProjection(
    claim,
    meta,
    includeCanonical = true,
    canonicalState = "applied",
  ) {
    const target = await this.applyAndVerifyCell(claim);
    const route = await this.publishClaimRoute(claim, meta, target);
    if (includeCanonical) {
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
    const realmLabel = typeof input?.realm_label === "string"
      ? input.realm_label
      : "";
    realmEmailRouteKey(domain, realmLabel);
    if (CANONICAL_REALM_LABEL_PATTERN.test(realmLabel)) {
      const canonical = await this.storage.get(
        `canonical:${domain}:${realmLabel}`,
      );
      if (!canonical) fail("realm email route not found", 404);
      const target = canonical.state === "applied"
        ? await this.cellTarget(canonical.account_id)
        : null;
      const refreshed = {
        ...canonical,
        updated_at: this.now().toISOString(),
      };
      const value = buildRealmEmailRouteProjection({
        ...refreshed,
        route_kind: "canonical",
        ...(refreshed.state === "suspended"
          ? { suspension_disposition: "retry" }
          : {}),
        cell_audience: target?.cell_audience,
        ingest_url: target?.ingest_url,
      });
      await this.enqueueRouteRefresh({
        domain,
        realmLabel,
        accountID: canonical.account_id,
        kind: "canonical",
      });
      return json(value);
    }

    const claim = await this.storage.get(claimKey(realmLabel));
    if (!claim?.assignment_kind || claim.domain !== domain) {
      fail("realm email route not found", 404);
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
    return json(value);
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
      const deadlines = [
        firstGrace ? Number(firstGrace.split(":", 3)[1]) : NaN,
        firstPlan ? Number(firstPlan.split(":", 3)[1]) : NaN,
        firstApproval ? Number(firstApproval.split(":", 3)[1]) : NaN,
        firstLifecycle ? Number(firstLifecycle.split(":", 3)[1]) : NaN,
        firstProjection ? Number(firstProjection.split(":", 3)[1]) : NaN,
        firstInternal ? Number(firstInternal.split(":", 3)[1]) : NaN,
        firstRefresh ? Number(firstRefresh.split(":", 3)[1]) : NaN,
      ].filter(Number.isFinite);
      if (deadlines.length > 0) {
        await this.storage.setAlarm(Math.min(...deadlines));
      } else if (typeof this.storage.deleteAlarm === "function") {
        await this.storage.deleteAlarm().catch(() => {});
      }
    });
  }

  alarm() {
    return (async () => {
      await this.withLane("registry:seed", () => this.ensureSeeded());
      // Each lane owns its own retry fence. A poison account or claim must not
      // prevent later due items, grace expiry, or approval recovery from
      // making progress during the same bounded alarm turn.
      await this.reconcileDuePlanIntents();
      await this.reconcileDueLifecycles();
      await this.reconcileDueProjections();
      await this.reconcileDueInternalAssignments();
      await this.reconcileDueRouteRefreshes();
      await this.reconcileDueApprovals();
      await this.reconcileDueGrace();
      await this.scheduleNextAlarm().catch(() => {});
    })();
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
              const canonicalKey =
                `canonical:${current.domain}:${current.realm_label}`;
              const canonical = await this.storage.get(canonicalKey);
              if (!canonical) {
                await this.atomic([], [refreshKey, dueKey]);
                return;
              }
              const target = canonical.state === "applied"
                ? await this.cellTarget(canonical.account_id)
                : null;
              const refreshed = {
                ...canonical,
                updated_at: this.now().toISOString(),
              };
              await this.publishRoute(buildRealmEmailRouteProjection({
                ...refreshed,
                route_kind: "canonical",
                ...(refreshed.state === "suspended"
                  ? { suspension_disposition: "retry" }
                  : {}),
                cell_audience: target?.cell_audience,
                ingest_url: target?.ingest_url,
              }));
              await this.storage.put(canonicalKey, refreshed);
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
    const realmClaims = (await this.claimsForRealm(
      input.account_id,
      input.realm_id,
    )).filter((claim) => !claim.retired_at);
    const assigned = realmClaims.filter((claim) =>
      claim.assignment_kind === "customer"
    ).length;
    const pending = realmClaims.filter((claim) =>
      !claim.assignment_kind && claim.request_id
    ).length;
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
    await this.atomic(mutation.entries);
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
    return json({
      schema_version: SCHEMA_VERSION,
      requests,
      truncated: listed.truncated,
      next_cursor: listed.next_cursor ?? null,
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
    const pendingProjection = await this.matchingProjectionIntent(
      request.alias,
      scope,
      key,
      fp,
    );
    if (pendingProjection) return this.drainProjectionIntent(pendingProjection);
    await this.assertAccountAliasWritesAllowed(request.account_id);
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
    const occupied = (await this.claimsForRealm(
      request.account_id,
      request.realm_id,
    )).filter((candidate) =>
      !candidate.retired_at &&
      (candidate.assignment_kind === "customer" ||
        candidate.customer_activation_intent === true)
    ).length;
    if (!resuming && input.alias_limit !== null &&
        occupied >= input.alias_limit) {
      fail("realm email alias limit reached", 403);
    }

    if (!resuming) {
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
      ]);
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
    const realmClaims = (await this.claimsForRealm(
      claim.account_id,
      claim.realm_id,
    )).filter((candidate) =>
      candidate.assignment_kind === "customer" && !candidate.retired_at
    ).sort((left, right) =>
      left.created_at.localeCompare(right.created_at) ||
      left.alias.localeCompare(right.alias)
    );
    const position = realmClaims.findIndex((candidate) =>
      candidate.claim_id === claim.claim_id
    );
    const overPlan = !policy.feature_enabled ||
      (policy.alias_limit !== null && position >= policy.alias_limit);
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
    ]);
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
    ]);
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
    if (input.action === "retire" &&
        await this.storage.get(planIntentKey(claim.account_id))) {
      fail("account alias plan is still converging; retirement is fenced", 409);
    }
    if (claim.internal_intent || claim.customer_activation_intent) {
      fail("alias provisioning is still converging", 409);
    }
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
    });
    return this.drainProjectionIntent(intent);
  }

  async assignInternal(input) {
    validateAccountRealm(input);
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
      await this.assertAccountAliasWritesAllowed(input.account_id);
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

  planIntent(input, state) {
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
        current.state === intent.state) {
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
      created_at: current?.created_at ?? mutation.now,
      updated_at: mutation.now,
    };
    mutation.entries.push(
      [planIntentKey(intent.account_id), durable],
      [planDueKey(durable), intent.account_id],
    );
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
    for (const canonical of canonicalPage.canonicals) {
      const desired = {
        ...canonical,
        state: "suspended",
        controller_revision: Math.max(
          canonical.controller_revision ?? 0,
          mutation.meta.registry_revision,
        ),
        updated_at: mutation.now,
      };
      await this.publishRoute(buildRealmEmailRouteProjection({
        ...desired,
        route_kind: "canonical",
        suspension_disposition: "retry",
      }));
      const canonicalKey =
        `canonical:${desired.domain}:${desired.realm_label}`;
      mutation.entries.push(
        [canonicalKey, desired],
        [accountCanonicalIndexKey(desired), canonicalKey],
      );
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
    if (page.next_cursor === null) {
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
      complete: page.next_cursor === null,
      registry_revision: mutation.meta.registry_revision,
      assignments: changed.map(({ next }) => publicClaim(next)),
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
    return action === "suspend" ? 1 : action === "republish" ? 2 : 0;
  }

  validateLifecycleReconciliation(input) {
    if (!ACCOUNT_ID_PATTERN.test(input?.account_id ?? "") ||
        !IDEMPOTENCY_KEY_PATTERN.test(input?.operation_id ?? "") ||
        !Number.isSafeInteger(input?.epoch) || input.epoch < 0 ||
        !["suspend", "republish"].includes(input?.action) ||
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
    let changed = 0;
    for (const claim of claimPage.claims) {
      if (!claim?.assignment_kind) {
        continue;
      }
      if (claim.customer_activation_intent === true ||
          claim.internal_intent === true) {
        fail("account has an alias provisioning intent still converging", 409);
      }
      const lifecycleSuspended = intent.action === "suspend" && !claim.retired_at;
      const operationalGateSuspended = !intent.activation_enabled &&
          !claim.retired_at
        ? true
        : false;
      const stateChanged =
        lifecycleSuspended !== (claim.lifecycle_suspended === true) ||
        operationalGateSuspended !==
          (claim.operational_gate_suspended === true);
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
          responseBody: { schema_version: SCHEMA_VERSION, converged: true },
          includeCanonical: false,
        });
        await this.drainProjectionIntent(projection);
        changed += 1;
      } else {
        await this.storage.put(claimKey(desired.alias), desired);
      }
    }

    for (const canonical of canonicalPage.canonicals) {
      const desiredState = intent.action === "suspend" ||
          !intent.activation_enabled
        ? "suspended"
        : "applied";
      const target = desiredState === "applied"
        ? await this.cellTarget(canonical.account_id)
        : null;
      const desired = {
        ...canonical,
        state: desiredState,
        controller_revision: Math.max(
          canonical.controller_revision ?? 0,
          mutation.meta.registry_revision,
        ),
        updated_at: mutation.now,
        lifecycle_fence: {
          operation_id: intent.operation_id,
          epoch: intent.epoch,
          action: intent.action,
        },
      };
      await this.publishRoute(buildRealmEmailRouteProjection({
        ...desired,
        route_kind: "canonical",
        ...(desiredState === "suspended"
          ? { suspension_disposition: "retry" }
          : {}),
        cell_audience: target?.cell_audience,
        ingest_url: target?.ingest_url,
      }));
      const canonicalKey =
        `canonical:${desired.domain}:${desired.realm_label}`;
      mutation.entries.push(
        [canonicalKey, desired],
        [accountCanonicalIndexKey(desired), canonicalKey],
      );
      changed += 1;
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
    if (!["restrict_only", "complete"].includes(input.mode)) {
      fail("invalid reconciliation mode", 400);
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
    const recoversPending = recoveryProvided && pending &&
      pending.plan_revision === input.recover_pending_revision &&
      pending.plan_snapshot_hash === input.recover_pending_snapshot_hash;

    if (input.mode === "restrict_only") {
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
    }
    const committed = await this.persistPlanIntent(
      this.planIntent(input, "cell_committed"),
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
