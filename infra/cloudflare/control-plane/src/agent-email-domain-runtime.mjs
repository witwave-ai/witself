import {
  AgentEmailDomainJournalRuntime,
  AgentEmailDomainJournalRuntimeError,
} from "./agent-email-domain-journal-runtime.mjs";

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

export const AGENT_EMAIL_CUSTOM_DOMAIN_FEATURE =
  "agent_email_custom_domain";
export const AGENT_EMAIL_CUSTOM_DOMAIN_LIMIT =
  "agent_email_custom_domains_per_account";
export const AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT = 8;
export const AGENT_EMAIL_CUSTOM_DOMAIN_MAX_LIST_LIMIT = 100;

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
  if (!Number.isSafeInteger(input?.plan_revision) ||
      input.plan_revision < 0 ||
      typeof input?.plan_snapshot_hash !== "string" ||
      !/^[0-9a-f]{64}$/.test(input.plan_snapshot_hash)) {
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

function requestStorageKey(requestID) {
  return `request:${requestID}`;
}

function domainStorageKey(domain) {
  return `domain:${domain}`;
}

function accountRequestPrefix(accountID) {
  return `account-request:${accountID}:`;
}

function accountRequestKey(accountID, requestID) {
  return `${accountRequestPrefix(accountID)}${requestID}`;
}

function usageKey(accountID) {
  return `account-usage:${accountID}`;
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

function publicRequest(request) {
  return {
    id: request.id,
    account_id: request.account_id,
    domain: request.domain,
    state: request.state,
    ownership_challenge: { ...request.ownership_challenge },
    requested_by: request.requested_by,
    requested_at: request.requested_at,
    updated_at: request.updated_at,
    domain_limit_at_request: request.domain_limit_at_request,
    plan_revision: request.plan_revision,
    plan_snapshot_hash: request.plan_snapshot_hash,
    ...(request.decision ? { decision: { ...request.decision } } : {}),
    ...(request.retirement ? { retirement: { ...request.retirement } } : {}),
  };
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

/**
 * One globally named Durable Object owns every customer-domain tombstone.
 * This initial runtime deliberately stops at an ownership challenge: it does
 * not query DNS, activate mail, publish edge routes, or project state to a
 * cell. Later lifecycle work can build on the durable request without
 * changing the uniqueness boundary.
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
      await this.ensureMeta();
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
        case "/audit/list":
          return await this.listAudit(input);
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

  async mutation(actor, action, target, metadata) {
    const current = await this.ensureMeta();
    const now = this.now().toISOString();
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
        updated_at: null,
      };
    }
    if (usage.schema_version !== 1 || usage.account_id !== accountID ||
        !Number.isSafeInteger(usage.open_requests) ||
        usage.open_requests < 0 ||
        usage.open_requests >
          AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_REQUESTS_PER_ACCOUNT) {
      fail("custom inbound domain account usage is invalid", 503);
    }
    return usage;
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

    if (input?.requests_enabled !== true) {
      fail("custom inbound domain requests are not enabled", 409,
        "custom_domain_requests_disabled");
    }
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
    // Pending verification requests reserve both commercial quota and the
    // independent technical ceiling. Rejection or retirement releases that
    // account capacity but keeps the global domain tombstone fail-closed.
    if (input.domain_limit !== null &&
        usage.open_requests >= input.domain_limit) {
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
      { account_id: accountID, state: "pending_verification" },
    );
    const created = {
      schema_version: SCHEMA_VERSION,
      id,
      account_id: accountID,
      domain,
      state: "pending_verification",
      ownership_challenge: {
        record_type: "TXT",
        record_name: `_witself-verification.${domain}`,
        record_value: `witself-domain-verification=${challengeToken}`,
        issued_at: mutation.now,
      },
      requested_by: actor.id,
      requested_at: mutation.now,
      updated_at: mutation.now,
      domain_limit_at_request: input.domain_limit,
      plan_revision: planFence.revision,
      plan_snapshot_hash: planFence.snapshot_hash,
      decision: null,
      retirement: null,
    };
    const nextUsage = {
      ...usage,
      open_requests: usage.open_requests + 1,
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
      [domainStorageKey(domain), created],
      [accountRequestKey(accountID, id), id],
      [usageKey(accountID), nextUsage],
      [idempotencyStorageKey(idempotencyScope, key), {
        fingerprint: fp,
        status: 202,
        body,
      }],
    ]);
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
        return publicRequest(request);
      }))
      : listed.entries.map(([, request]) => publicRequest(request));
    if (input?.state != null) {
      if (!["pending_verification", "rejected", "retired"].includes(
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
    });
  }

  async getRequest(input) {
    validateActor(input?.actor, "platform_admin");
    const id = validateRequestID(input?.request_id);
    const request = await this.storage.get(requestStorageKey(id));
    if (!request) fail("custom inbound domain request not found", 404);
    return json({
      schema_version: SCHEMA_VERSION,
      request: publicRequest(request),
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
    if (nextState === "rejected" && current.state !== "pending_verification") {
      fail("only a pending custom inbound domain request can be rejected", 409);
    }
    if (nextState === "retired" && current.state === "retired") {
      fail("custom inbound domain request is already retired", 409);
    }
    if (!["pending_verification", "rejected"].includes(current.state)) {
      fail("custom inbound domain request state is invalid", 503);
    }

    const mutation = await this.mutation(
      actor,
      `custom_domain.${nextState}`,
      current.domain,
      { account_id: current.account_id, from_state: current.state, reason },
    );
    const updated = {
      ...current,
      state: nextState,
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
    };
    const entries = [
      [META_KEY, mutation.meta],
      [mutation.audit_key, mutation.audit],
      [requestStorageKey(id), updated],
      [domainStorageKey(current.domain), updated],
    ];
    if (current.state === "pending_verification") {
      const usage = await this.accountUsage(current.account_id);
      if (usage.open_requests < 1) {
        fail("custom inbound domain account usage is invalid", 503);
      }
      entries.push([usageKey(current.account_id), {
        ...usage,
        open_requests: usage.open_requests - 1,
        updated_at: mutation.now,
      }]);
    }
    const body = {
      schema_version: SCHEMA_VERSION,
      request: publicRequest(updated),
    };
    entries.push([idempotencyStorageKey(idempotencyScope, key), {
      fingerprint: fp,
      status: 200,
      body,
    }]);
    await this.atomic(entries);
    return json(body);
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
      if (typeof input.action !== "string" ||
          !/^custom_domain\.(?:requested|rejected|retired)$/.test(input.action)) {
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
