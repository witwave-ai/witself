// Shared control-plane contract for one custom-domain + realm-alias route.
// The edge projection deliberately extends the existing schema-v1 realm-route
// union instead of creating another lookup namespace. The cell projection is
// richer: it carries every source revision needed to reject a stale or
// incorrectly bound controller acknowledgement before anything reaches KV.

export const AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_KIND = "custom_domain";
export const AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_PREFIX =
  "email:realm-route:v1:";
export const AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_CACHE_TTL_SECONDS = 300;
export const REALM_EMAIL_ALIAS_CLAIM_PROOF_SCHEMA_VERSION =
  "witself.realm-email-alias-claim-proof.v1";
export const AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_INTENT_SCHEMA_VERSION =
  "witself.agent-email-custom-domain-route-intent.v1";
export const CELL_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_SCHEMA_VERSION =
  "witself.v0";
export const CELL_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_ACTION =
  "email-custom-domain-route";

const ACCOUNT_ID_PATTERN = /^[A-Za-z0-9_-]{1,128}$/;
const REQUEST_ID_PATTERN = /^aedr_[a-z2-7]{16}$/;
const CLAIM_ID_PATTERN = /^era_[a-z2-7]{16}$/;
const REALM_ID_PATTERN = /^realm_[a-z2-7]{16}$/;
const REALM_ALIAS_PATTERN = /^[a-z0-9](?:[a-z0-9-]{1,14}[a-z0-9])$/;
const CANONICAL_REALM_LABEL_PATTERN = /^[a-z2-7]{16}$/;
const DOMAIN_LABEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactKeys(value, allowed) {
  return isObject(value) && Object.keys(value).every((key) => allowed.has(key));
}

function sameFlatObject(left, right) {
  if (!isObject(left) || !isObject(right)) return false;
  const leftKeys = Object.keys(left);
  const rightKeys = Object.keys(right);
  return leftKeys.length === rightKeys.length &&
    leftKeys.every((key) => Object.hasOwn(right, key) &&
      Object.is(left[key], right[key]));
}

function canonicalDomain(value) {
  if (typeof value !== "string" || value !== value.trim().toLowerCase() ||
      value.length < 3 || value.length > 231 || !/^[\x00-\x7f]+$/.test(value) ||
      value.includes("*") || value.includes("xn--")) {
    throw new TypeError("custom-domain route domain is invalid");
  }
  const labels = value.split(".");
  if (labels.length < 2 || labels.some((label) =>
    !DOMAIN_LABEL_PATTERN.test(label)) || !/[a-z]/.test(labels.at(-1))) {
    throw new TypeError("custom-domain route domain is invalid");
  }
  return value;
}

function realmAlias(value) {
  if (typeof value !== "string" || !REALM_ALIAS_PATTERN.test(value) ||
      value.includes("--") || CANONICAL_REALM_LABEL_PATTERN.test(value)) {
    throw new TypeError("custom-domain route realm label is invalid");
  }
  return value;
}

function positiveRevision(value, name) {
  if (!Number.isSafeInteger(value) || value < 1) {
    throw new TypeError(`${name} is invalid`);
  }
  return value;
}

function validState(value) {
  if (!["applied", "suspended", "retired"].includes(value)) {
    throw new TypeError("custom-domain route state is invalid");
  }
  return value;
}

function stateFields(state, suspensionDisposition) {
  if (state === "suspended") {
    if (!["retry", "inactive"].includes(suspensionDisposition)) {
      throw new TypeError("custom-domain route suspension disposition is invalid");
    }
    return { suspension_disposition: suspensionDisposition };
  }
  if (suspensionDisposition !== undefined && suspensionDisposition !== null) {
    throw new TypeError(
      "custom-domain route suspension disposition is only valid when suspended",
    );
  }
  return {};
}

function routeBinding(input) {
  if (!REQUEST_ID_PATTERN.test(input?.domain_request_id ?? "") ||
      !CLAIM_ID_PATTERN.test(input?.realm_alias_claim_id ?? "") ||
      !REALM_ID_PATTERN.test(input?.realm_id ?? "")) {
    throw new TypeError("custom-domain route authority binding is invalid");
  }
  return {
    domain: canonicalDomain(input.domain),
    realm_label: realmAlias(input.realm_label),
    realm_id: input.realm_id,
    domain_request_id: input.domain_request_id,
    domain_allocation_revision: positiveRevision(
      input.domain_allocation_revision,
      "custom-domain allocation revision",
    ),
    realm_alias_claim_id: input.realm_alias_claim_id,
    realm_alias_revision: positiveRevision(
      input.realm_alias_revision,
      "realm-alias revision",
    ),
  };
}

export function agentEmailCustomDomainRouteKey(domain, realmLabel) {
  return `${AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_PREFIX}${canonicalDomain(domain)}:` +
    realmAlias(realmLabel);
}

export function buildRealmEmailAliasClaimProof({
  account_id: accountID,
  realm_id: realmID,
  realm_label: realmLabel,
  realm_alias_claim_id: claimID,
  realm_alias_revision: revision,
  state,
  suspension_disposition: suspensionDisposition,
  updated_at: updatedAt,
}) {
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "") ||
      !REALM_ID_PATTERN.test(realmID ?? "") ||
      !CLAIM_ID_PATTERN.test(claimID ?? "") ||
      typeof updatedAt !== "string" || !Number.isFinite(Date.parse(updatedAt))) {
    throw new TypeError("realm-alias claim proof binding is invalid");
  }
  const normalizedState = validState(state);
  return {
    schema_version: REALM_EMAIL_ALIAS_CLAIM_PROOF_SCHEMA_VERSION,
    account_id: accountID,
    realm_id: realmID,
    realm_label: realmAlias(realmLabel),
    realm_alias_claim_id: claimID,
    realm_alias_revision: positiveRevision(revision, "realm-alias revision"),
    state: normalizedState,
    ...stateFields(normalizedState, suspensionDisposition),
    updated_at: updatedAt,
  };
}

export function validateRealmEmailAliasClaimProof(
  value,
  accountID,
  realmLabel,
) {
  const allowed = new Set([
    "schema_version", "account_id", "realm_id", "realm_label",
    "realm_alias_claim_id", "realm_alias_revision", "state",
    "suspension_disposition", "updated_at",
  ]);
  if (!exactKeys(value, allowed) || value.schema_version !==
      REALM_EMAIL_ALIAS_CLAIM_PROOF_SCHEMA_VERSION) {
    throw new TypeError("realm-alias claim proof is invalid");
  }
  const normalized = buildRealmEmailAliasClaimProof(value);
  if (normalized.account_id !== accountID ||
      normalized.realm_label !== realmLabel ||
      JSON.stringify(normalized) !== JSON.stringify(value)) {
    throw new TypeError("realm-alias claim proof is inconsistent");
  }
  return normalized;
}

// `updated_at` is freshness metadata, not route authority. Alias mutations
// that leave the exact claim revision and effective route policy unchanged do
// not need to fan out another custom-domain projection merely because their
// audit timestamp changed.
export function realmEmailAliasClaimRouteFingerprint(value) {
  const proof = validateRealmEmailAliasClaimProof(
    value,
    value?.account_id,
    value?.realm_label,
  );
  return JSON.stringify({
    account_id: proof.account_id,
    realm_id: proof.realm_id,
    realm_label: proof.realm_label,
    realm_alias_claim_id: proof.realm_alias_claim_id,
    realm_alias_revision: proof.realm_alias_revision,
    state: proof.state,
    suspension_disposition: proof.suspension_disposition ?? null,
  });
}

export function buildAgentEmailCustomDomainCellProjection({
  account_id: accountID,
  domain_state_revision: domainStateRevision,
  controller_revision: controllerRevision,
  state,
  suspension_disposition: suspensionDisposition,
  ...bindingInput
}) {
  if (!ACCOUNT_ID_PATTERN.test(accountID ?? "")) {
    throw new TypeError("custom-domain route account binding is invalid");
  }
  const binding = routeBinding(bindingInput);
  const normalizedState = validState(state);
  return {
    schema_version: CELL_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_SCHEMA_VERSION,
    account_id: accountID,
    ...binding,
    domain_state_revision: positiveRevision(
      domainStateRevision,
      "custom-domain request revision",
    ),
    controller_revision: positiveRevision(
      controllerRevision,
      "custom-domain controller revision",
    ),
    state: normalizedState,
    ...stateFields(normalizedState, suspensionDisposition),
  };
}

export function validateAgentEmailCustomDomainCellProjection(
  value,
  expected = null,
) {
  const allowed = new Set([
    "schema_version", "account_id", "domain", "realm_label", "realm_id",
    "domain_request_id", "domain_allocation_revision",
    "domain_state_revision", "realm_alias_claim_id", "realm_alias_revision",
    "controller_revision", "state", "suspension_disposition",
  ]);
  if (!exactKeys(value, allowed) || value.schema_version !==
      CELL_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_SCHEMA_VERSION) {
    throw new TypeError("cell custom-domain route projection is invalid");
  }
  const normalized = buildAgentEmailCustomDomainCellProjection(value);
  if (!sameFlatObject(normalized, value) ||
      (expected !== null && !sameFlatObject(
        normalized,
        buildAgentEmailCustomDomainCellProjection(expected),
      ))) {
    throw new TypeError("cell custom-domain route projection is inconsistent");
  }
  return normalized;
}

export function cellAgentEmailCustomDomainRouteURL(
  endpoint,
  accountID,
  domainRequestID = null,
  realmAliasClaimID = null,
) {
  const url = new URL(
    `${endpoint}/v1/accounts/${encodeURIComponent(accountID)}:` +
      CELL_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_ACTION,
  );
  if (domainRequestID !== null) {
    url.searchParams.set("domain_request_id", domainRequestID);
  }
  if (realmAliasClaimID !== null) {
    url.searchParams.set("realm_alias_claim_id", realmAliasClaimID);
  }
  return url.toString();
}

export function buildAgentEmailCustomDomainRouteProjection({
  state,
  suspension_disposition: suspensionDisposition,
  controller_revision: controllerRevision,
  updated_at: updatedAt,
  cell_audience: cellAudience,
  ingest_url: ingestURL,
  cache_ttl_seconds: cacheTTLSeconds =
    AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_CACHE_TTL_SECONDS,
  ...bindingInput
}) {
  const binding = routeBinding(bindingInput);
  const normalizedState = validState(state);
  if (!Number.isSafeInteger(cacheTTLSeconds) || cacheTTLSeconds < 1 ||
      cacheTTLSeconds > 3_600 || typeof updatedAt !== "string" ||
      !Number.isFinite(Date.parse(updatedAt))) {
    throw new TypeError("custom-domain route freshness fields are invalid");
  }
  const projection = {
    schema_version: 1,
    domain: binding.domain,
    realm_label: binding.realm_label,
    realm_id: binding.realm_id,
    route_kind: AGENT_EMAIL_CUSTOM_DOMAIN_ROUTE_KIND,
    state: normalizedState,
    controller_revision: positiveRevision(
      controllerRevision,
      "custom-domain controller revision",
    ),
    updated_at: updatedAt,
    cache_ttl_seconds: cacheTTLSeconds,
    domain_request_id: binding.domain_request_id,
    domain_allocation_revision: binding.domain_allocation_revision,
    realm_alias_claim_id: binding.realm_alias_claim_id,
    realm_alias_revision: binding.realm_alias_revision,
    ...stateFields(normalizedState, suspensionDisposition),
  };
  if (normalizedState === "applied") {
    if (typeof cellAudience !== "string" || cellAudience.length < 1 ||
        cellAudience.length > 128 ||
        !/^[a-z](?:[a-z0-9-]{0,126}[a-z0-9])?$/.test(cellAudience)) {
      throw new TypeError("custom-domain route cell audience is invalid");
    }
    let parsed;
    try {
      parsed = new URL(ingestURL);
    } catch {
      throw new TypeError("custom-domain route ingestion URL is invalid");
    }
    if (parsed.protocol !== "https:" || parsed.username || parsed.password ||
        parsed.hash || parsed.search || !parsed.hostname ||
        parsed.hostname === "localhost") {
      throw new TypeError("custom-domain route ingestion URL is invalid");
    }
    projection.cell_audience = cellAudience;
    projection.ingest_url = parsed.toString();
  } else if (cellAudience !== undefined || ingestURL !== undefined) {
    throw new TypeError(
      "custom-domain route destination is only valid when applied",
    );
  }
  return projection;
}
