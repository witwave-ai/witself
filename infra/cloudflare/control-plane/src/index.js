// Thin Worker front door for witself-control-plane.
//
// HOT PATH: the account->cell directory is answered entirely at the edge from
// the DIRECTORY KV binding — it never touches the container. Per the 2026-07
// scaling research: Workers+KV reads are effectively unbounded, while
// containers are a fixed instance count (~1k rps DO cap each, 1-3s cold
// starts). This split is what makes the control plane "scale from zero".
//
// FLEET REGISTRY: /v1/cells — register (upsert), list, and remove cells.
// Authorized by the FLEET_TOKEN Worker secret (one shared fleet token in v0;
// partner-hosted cells later get per-party credentials, with owner derived
// from the credential — never from the payload). Removal is gated: the cell
// must be drained (accepting=false) AND no directory entries may point at it,
// so a credential alone can never yank a cell out from under live accounts.
//
// COLD PATH: everything else forwards to the Go container (signup, webhooks —
// later slices).
//
// INVITES: /v1/invites — the v0 signup gate (until email-verification and
// billing land, and the permanent early-access/promo lever after). Codes are
// named gates, not secrets; managed with the fleet token. NOTE: uses-counting
// in KV is best-effort (no atomic increment) — fine for invite gating; exact
// counting arrives with the Durable Object authority.
//
// KV schema (v0, one namespace):
//   acct:<account_id>    -> {"cell":"<name>","endpoint":"https://...",
//                            "region","region_code"}
//   pending:<account_id> -> {"cell":"<name>","created_at":"<iso>"}
//                           reap candidate; dropped on activation/close/reap
//   verify:<sha256(token)> -> {"account_id","cell","created_at"}
//                           email-verification token (hash only, KV TTL 7d)
//   recover:<account_id> -> {"code_hash","code_expires_at","attempts",
//                           "emails_sent","last_email_at","window_expires_at"}
//                           recovery code + rate-limit state, phantom ids too.
//                           The 4h quota window is absolute: every write after
//                           the reservation derives its KV TTL from
//                           window_expires_at, so attempts never extend it.
//   emailchange:<account_id> -> {"code_hash","code_expires_at","new_email",
//                           "old_email","undo_key","attempts","emails_sent",
//                           "last_email_at"} (KV TTL 24h)
//                           pending email change + rate-limit state; old_email
//                           is the committed transition's authoritative source
//                           across a post-commit crash, and undo_key fences
//                           undo-authority minting to at most one live token
//   undoemail:<sha256(token)> -> {"account_id","cell","old_email","new_email",
//                           "expires_at"}
//                           (KV TTL 48h) — undo window shipped in the notice
//   archived:<account_id> -> {"cell","region","region_code","object",
//                           "exported_at","size","format_version",
//                           "placement_policy"}
//                           post-evacuation state; the directory answers
//                           "archived — awaiting placement" for these. Only
//                           {cell,region,region_code,exported_at} are returned
//                           to the public directory route; the rest are
//                           fleet-only.
//   evac:<cell>          -> {"started_at","done","failed"[],"remaining",
//                           "finished_at"}
//                           cross-batch progress for a cell-wide evacuation.
//   cell:<name>          -> {"endpoint","cloud","region","region_code",
//                             "channel","owner","weight","accepting",
//                             "backup_validation_target","registered_at"}
//   invite:<code>        -> {"enabled","not_before","expires_at","max_uses",
//                             "uses","note","created_at","cell","region"}
//   config:placement     -> {"strategy":"weighted"|"pinned","pinned_cell"}
//   config:placement_runner -> {"enabled":bool,"restore_archives":bool,
//                               "restore_batch":N,"restore_any_region":bool,
//                               "rebalance":bool,"rebalance_batch":N}
//   config:account_backup_scan -> {"schema_version","slot","cursor",
//                                  "complete","updated_at"}
//                                  opt-in periodic backup directory cursor
//   config:reaper        -> {"enabled":bool,"ttl_minutes":N}
//
// PLACEMENT (which cell gets a new account), precedence top wins:
//   1. invite.cell    — hard pin (dedicated/enterprise cells); 503 if unavailable
//   2. invite.region  — hard constraint (compliance); 503 if no cell in region
//   3. config:placement strategy — "pinned" (soft: falls back if the pinned
//      cell is ineligible) or "weighted" (default; equal weights ≈ round-robin)
//   4. weighted random among what remains
// "geo" (request.cf-based nearest-region) is reserved for when the fleet has
// multiple regions; exact sequential round-robin arrives with the DO authority.
// The registry is small and rarely written; when signup lands, the
// authoritative copy moves to a Durable Object (KV has no transactions) and
// KV stays the read projection. The O(accounts) scan in DELETE moves to DO
// counters at the same time.
import { Container, getContainer } from "@cloudflare/containers";
import {
  containerEnvVars,
  forwardAdminPolicyRequest,
  handleInternalBridgeRequest,
  isInternalBridgePath,
  matchAdminPolicyPath,
  PLAN_LIFECYCLE_ACTIVATE_PATH,
  restartContainerWithEnvironment,
  runScheduledPlanLifecycle,
} from "./bridge.mjs";
import {
  accountBackupSchedulingEnabled,
  bestPlacementCell,
  bestPolicyCell,
  bestRebalanceCell,
  cellHasDestinationCredentials,
  cellIsEligibleDestination,
  cellMatchesArchivedPlacement,
  cellMatchesPolicy,
  rescuePlacementPolicy,
} from "./placement.mjs";
import {
  ADMIN_SCOPES,
  ADMIN_SCOPE_FULL,
  adminScopeAllows,
  validateMintHandle,
} from "./admin-handles.mjs";
import { renderSupportEmail } from "./support-notify.mjs";
import { DurableAccountLifecycle } from "./account-lifecycle-runtime.mjs";
import { DurableAccountSignup } from "./account-signup-runtime.mjs";
import {
  DurableTargetCellCoordinator,
} from "./target-cell-coordinator.mjs";
import {
  accountBackupStatus,
  DurableAccountBackup,
  runAccountBackupValidation,
  runManualAccountBackup,
  runScheduledAccountBackups,
} from "./account-backup-runtime.mjs";
import {
  handleRealmEmailAliasAdminRequest,
  handleRealmEmailAliasCustomerRequest,
  handleRealmEmailCanonicalCloseRequest,
  handleAgentEmailOperationsLeaseRequest,
  handleManagedDeliveryReadinessRequest,
  handleRealmEmailRouteRequest,
  EDGE_MANAGED_DELIVERY_READINESS_PATH,
  isRealmEmailAliasAdminPath,
  isRealmEmailRoutePath,
  matchRealmEmailAliasCustomerPath,
  matchRealmEmailCanonicalClosePath,
  matchRealmEmailRoutePath,
} from "./realm-email-alias-api.mjs";
import {
  isAgentEmailOperationsLeasePath,
} from "./agent-email-operations-lease.mjs";
import {
  DurableRealmEmailAliasRegistry,
  runScheduledCanonicalRealmRouteInventory,
} from "./realm-email-alias-runtime.mjs";
import {
  handleAgentEmailDomainAdminRequest,
  handleAgentEmailDomainCustomerRequest,
  isAgentEmailDomainAdminPath,
  matchAgentEmailDomainCustomerPath,
} from "./agent-email-domain-api.mjs";
import {
  DurableAgentEmailDomainRegistry,
  runScheduledAgentEmailDomainVerification,
} from "./agent-email-domain-runtime.mjs";
import {
  handleAgentEmailDomainRecoveryAdminRequest,
  isAgentEmailDomainRecoveryAdminPath,
} from "./agent-email-domain-recovery-api.mjs";
import {
  handleRealmEmailAliasRecoveryAdminRequest,
  isRealmEmailAliasRecoveryAdminPath,
} from "./realm-email-alias-recovery-api.mjs";
import {
  billingReturnResponse,
} from "./billing-return-pages.mjs";
import {
  decideDisposition,
  dedupKey as supportEmailDedupKey,
  validateIntakePayload,
} from "./support-email-intake.mjs";

export class Backend extends Container {
  defaultPort = 8080;
  sleepAfter = "10m";

  constructor(ctx, env) {
    super(ctx, env);
    // Secrets are injected at runtime from the Worker's bindings, never
    // embedded in wrangler.jsonc. Only bridge configuration crosses into the
    // container; R2 bindings/credentials remain Worker-only.
    this.envVars = containerEnvVars(env);
  }

  // Worker secrets are projected only when a container process starts. This
  // RPC is the explicit activation boundary for a secret-only lifecycle gate
  // change: replace the old process and wait for the new port before returning.
  async restartWithEnvironment(freshEnv) {
    await restartContainerWithEnvironment(this, freshEnv);
    return { restarted: true };
  }
}

// One Durable Object instance exists per account id. Its SQLite-backed
// storage is the lifecycle/location authority; KV is only the edge read
// projection. The runtime resumes the exact persisted operation after a DO
// restart and never treats a generic existing account as import proof.
export class AccountLifecycle extends DurableAccountLifecycle {
  constructor(ctx, env) {
    super(ctx, env);
  }
}

// One Durable Object instance exists per cell name. It is the serialization
// authority for registration, incoming lifecycle reservations, and safe
// deletion; DIRECTORY KV is only the fleet read projection.
export class TargetCellCoordinator extends DurableTargetCellCoordinator {
  constructor(ctx, env) {
    super(ctx, env);
  }
}

// One Durable Object instance exists per caller-stable provision id. It
// serializes signup retries and also hosts per-invite exact use authorities.
export class AccountSignup extends DurableAccountSignup {
  constructor(ctx, env) {
    super(ctx, env, {
      placeAccount: (invite) => placeAccount(env, invite),
      sendVerification: ({
        origin,
        email,
        account_id: accountID,
        cell_name: cellName,
      }) => sendVerificationEmail(
        env,
        origin,
        email,
        accountID,
        cellName,
      ),
      logVerification: ({
        cell,
        account_id: accountID,
        email,
      }) => logCellEvent(
        cell,
        accountID,
        "account.email.verify.sent",
        "control_plane",
        { to_masked: maskEmail(email) },
      ),
    });
  }
}

// One Durable Object instance exists per account id. It serializes periodic
// backup attempts and owns a bounded catalog of fully reread-verified immutable
// objects in the dedicated backup bucket. It never mutates account routing or
// participates in evacuation.
export class AccountBackup extends DurableAccountBackup {
  constructor(ctx, env) {
    super(ctx, env);
  }
}

// One globally named Durable Object is the strong authority for the shared
// managed-domain realm-alias namespace. The isolated AGENT_EMAIL_DIRECTORY KV
// is only its fail-closed routing projection; it never grants ownership or
// permits address reuse.
export class RealmEmailAliasRegistry extends DurableRealmEmailAliasRegistry {
  constructor(ctx, env) {
    super(ctx, env);
  }
}

// One globally named Durable Object is the dark authority for organization-
// owned inbound agent-email domain requests. It does not publish DNS, routes,
// or cell projections; those require a later, independently gated lifecycle.
export class AgentEmailDomainRegistry extends DurableAgentEmailDomainRegistry {
  constructor(ctx, env) {
    super(ctx, env);
  }
}

const json = (obj, status = 200, extra = {}) =>
  new Response(JSON.stringify(obj), {
    status,
    headers: { "Content-Type": "application/json", ...extra },
  });

const err = (msg, status) => json({ schema_version: "witself.v0", error: msg }, status);

const DIRECTORY_PATH = /^\/v1\/directory\/([A-Za-z0-9_-]{1,128})$/;
const VERIFY_PATH = /^\/verify\/([0-9a-f]{64})$/;
// Account ids are splice into URLs and HTML — same charset the directory
// route accepts, enforced at every ingestion point.
const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const ACCOUNT_CLOSE_PATH = /^\/v1\/accounts\/([A-Za-z0-9_-]{1,128}):close$/;
const ACCOUNT_RESEND_PATH = /^\/v1\/accounts\/([A-Za-z0-9_-]{1,128}):resend-verification$/;
const ACCOUNT_RECOVER_PATH = /^\/v1\/accounts\/([A-Za-z0-9_-]{1,128}):recover$/;
const ACCOUNT_CHANGE_EMAIL_PATH = /^\/v1\/accounts\/([A-Za-z0-9_-]{1,128}):change-email$/;
const UNDO_EMAIL_PATH = /^\/undo-email\/([0-9a-f]{64})$/;
const CELL_PATH = /^\/v1\/cells\/([a-z0-9-]{1,64})$/;
const PURGE_PATH = /^\/v1\/cells\/([a-z0-9-]{1,64}):purge$/;
const EVACUATE_PATH = /^\/v1\/cells\/([a-z0-9-]{1,64}):evacuate$/;
const RESTORE_PATH = /^\/v1\/cells\/([a-z0-9-]{1,64}):restore$/;
const PROBE_PATH = /^\/v1\/cells\/([a-z0-9-]{1,64}):probe$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const INVITE_PATH = /^\/v1\/invites\/([a-z0-9][a-z0-9-]{2,63})$/;
const INVITE_CODE = /^[a-z0-9][a-z0-9-]{2,63}$/;
const REGION_NAME = /^[a-z0-9-]{2,32}$/;
const PLACEMENT_STRATEGIES = ["weighted", "pinned"];
const PLACEMENT_CHANNELS = new Set(["stable", "edge", "experimental"]);
const PLACEMENT_RESCUE_PATH = /^\/v1\/placement\/archives\/([A-Za-z0-9_-]{1,128}):rescue$/;
const PLACEMENT_RESCUE_AXES = new Set(["cloud", "region", "channel"]);
const ACCOUNT_BACKUP_STATUS_PATH = "/v1/backups/status";
const ACCOUNT_BACKUP_RUN_PATH = "/v1/backups:run";
const ACCOUNT_BACKUP_RESTORE_DRILL_PATH =
  "/v1/backups:restore-drill";
const ACCOUNT_BACKUP_ID = /^backup_[0-9]{8}T[0-9]{6}Z$/;
const SUPPORT_EMAIL_INTAKE_PATH = "/v1/intake/support-email";
const SUPPORT_EMAIL_INTAKE_DEDUP_TTL_SECONDS = 7 * 24 * 60 * 60;
const SUPPORT_EMAIL_INTAKE_PENDING_TTL_SECONDS = 60;

// Admin identity: the audit trail only ever records a first-name handle
// (author_id on support_ticket_messages when author_kind='fleet_admin',
// admin_handle on account_events metadata). The credential itself lives
// in DIRECTORY KV under three prefixes:
//   admin:{admin_id}                  — canonical record
//   admintok:{sha256_hex(raw_token)}  — O(1) auth-lookup index
//   adminh:{handle}                   — uniqueness index (kept even after
//                                       revoke so the handle stays reserved
//                                       through the audit window)
// Raw admin token format: "witself_adm_" + base32 body — the same
// witself_<kind>_<body> shape as every other Witself credential (see
// internal/token). Only the sha256 is
// persisted; the raw token is shown exactly once at mint time.
const ADMIN_ID = /^adm_[a-z0-9]{20}$/;
const ADMIN_PATH = /^\/v1\/admins\/(adm_[a-z0-9]{20})$/;
const ADMIN_REVOKE_PATH = /^\/v1\/admins\/(adm_[a-z0-9]{20}):revoke$/;
// Admin-side fan-out paths (all admin-token authorized via adminAuthorized).
// /v1/admin/whoami        — cheap round-trip that also verifies the token.
// /v1/admin/tickets       — fleet-wide list; CP fans out to every cell.
// /v1/admin/accounts/{a}/tickets/{t}          — GET single ticket + thread.
// /v1/admin/accounts/{a}/tickets/{t}/messages — POST admin reply.
// /v1/admin/accounts/{a}/tickets/{t}/state    — PATCH state transition.
// /v1/admin/accounts/{a}/tickets/{t}/retriage — PATCH category / priority.
const ADMIN_ACCOUNT_TICKET_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)$/;
const ADMIN_ACCOUNT_TICKET_MSGS_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)\/messages$/;
const ADMIN_ACCOUNT_TICKET_STATE_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)\/state$/;
const ADMIN_ACCOUNT_TICKET_RETRIAGE_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)\/retriage$/;
// Support-policy read/write per account. GET returns current policy,
// PATCH flips it. Both proxy to the cell's admin: routes and inherit
// the archived-account 409 / unknown-account 404 rules.
const ADMIN_ACCOUNT_SUPPORT_POLICY_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/support-policy$/;
// Handle shape and the reserved set live in admin-handles.mjs so plain-node
// tests can pin them (this file imports workerd-only packages).

function timingSafeEqual(a, b) {
  const enc = new TextEncoder();
  const ab = enc.encode(a);
  const bb = enc.encode(b);
  if (ab.byteLength !== bb.byteLength) return false;
  return crypto.subtle.timingSafeEqual(ab, bb);
}

function fleetAuthorized(request, env) {
  if (!env.FLEET_TOKEN) return false; // no secret configured -> registry closed
  const h = request.headers.get("Authorization") || "";
  if (!h.startsWith("Bearer ")) return false;
  return timingSafeEqual(h.slice(7).trim(), env.FLEET_TOKEN);
}

function supportEmailIntakeAuthorized(request, env) {
  if (!env.SUPPORT_EMAIL_INTAKE_TOKEN) return false;
  const header = request.headers.get("Authorization") || "";
  if (!header.startsWith("Bearer ")) return false;
  return timingSafeEqual(
    header.slice(7).trim(),
    env.SUPPORT_EMAIL_INTAKE_TOKEN,
  );
}

// sha256Hex returns the lowercase hex digest of s. Used to index admin
// tokens: only the hash is persisted, so a KV leak can't be traded for
// a working admin token (mirrors the recovery-code and email-verification
// hashing pattern already in this file).
async function sha256Hex(s) {
  const buf = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(buf)]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

// adminAuthorized resolves an "Authorization: Bearer witself_adm_..." header to
// { admin_id, handle } or null (unauth / revoked / unknown). Used by the
// admin-side fan-out routes; landed here so it lives next to the KV
// shape it queries.
//
// Revocation freshness: KV has per-PoP edge caching (min 60s) and
// eventual global consistency (up to 60s). Without the adminrev:{hash}
// tombstone check below, a PoP that had cached `admin:{id}` with no
// disabled_at + `admintok:{hash}` before revoke would keep authenticating
// the revoked token for up to two cache-TTL windows. The tombstone lets
// revoke assert "denied" as a positive fact — a PoP fetching the
// tombstone freshly (or after its own null-cache TTL expires) blocks
// the token even if its cached admintok/admin still say the token is
// live. The residual worst-case tail on a PoP that just cached a null
// tombstone read pre-revoke is one KV cache TTL (~60s); the fleet-token
// holder should treat "revoke effective" as "within ~60s" not "on the
// next request", and rotate the fleet token as well if hard cutoff
// matters (e.g. suspected compromise).
async function adminAuthorized(request, env) {
  const h = request.headers.get("Authorization") || "";
  if (!h.startsWith("Bearer witself_adm_")) return null;
  const raw = h.slice(7).trim();
  const hash = await sha256Hex(raw);
  // Tombstone check FIRST. A revoked token has adminrev:{hash} set;
  // even if this PoP's admintok/admin records are still stale-cached
  // as live, the tombstone denies. Cheap KV get on the hot path.
  const revoked = await env.DIRECTORY.get(`adminrev:${hash}`, {
    type: "json",
  });
  if (revoked) return null;
  const idx = await env.DIRECTORY.get(`admintok:${hash}`, { type: "json" });
  if (!idx?.admin_id) return null;
  const rec = await env.DIRECTORY.get(`admin:${idx.admin_id}`, { type: "json" });
  if (!rec || rec.disabled_at) return null;
  return {
    admin_id: rec.admin_id,
    handle: rec.handle,
    scope: rec.scope ?? ADMIN_SCOPE_FULL,
  };
}

// Revocation-tombstone TTL. Covers the KV worst-case (60s cross-region
// propagation + 60s edge cache) with plenty of margin — after this
// window the deleted admintok:{hash} entry has definitively propagated
// to every PoP, so the tombstone can safely expire. A hard-deleted
// admin's tombstone stays live for the same window; replay of the same
// raw token after that returns null on the admintok lookup either way.
const ADMIN_REVOKE_TOMBSTONE_TTL_SEC = 3600;

// genAdminID returns an "adm_" + 20 lowercase-base32 identifier. Uses
// crypto.getRandomValues (12 bytes → 20 base32 chars after padding-strip
// & lowercase). Collision probability at fleet scale is negligible.
function genAdminID() {
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  // RFC 4648 base32 without padding, lowercased. 12 bytes → 20 chars,
  // matching ADMIN_ID's fixed length so the router regex is exact.
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let bits = 0;
  let value = 0;
  let out = "";
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      out += alphabet[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += alphabet[(value << (5 - bits)) & 31];
  return `adm_${out.slice(0, 20)}`;
}

// genAdminToken returns a fresh "witself_adm_" + 40 base32 chars secret. 25
// random bytes → 40 chars keeps the token length fixed for log-friendly
// pattern matching.
function genAdminToken() {
  const bytes = new Uint8Array(25);
  crypto.getRandomValues(bytes);
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let bits = 0;
  let value = 0;
  let out = "";
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      out += alphabet[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += alphabet[(value << (5 - bits)) & 31];
  return `witself_adm_${out.slice(0, 40)}`;
}

// publicAdmin strips the token_hash from a record before it leaves the
// control plane. Everything else in the record is safe for a fleet-token
// holder to read.
function publicAdmin(rec) {
  const { token_hash: _omitted, ...rest } = rec;
  return { ...rest, disabled: Boolean(rec.disabled_at) };
}

// listCells returns raw registry entries INCLUDING per-cell credentials.
// Never serve these to clients — use publicCell() first.
async function listCells(env) {
  const out = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "cell:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry) out.push({ name: k.name.slice(5), ...entry });
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return out;
}

// publicCell strips credentials from a registry entry before it leaves the
// control plane (the per-cell credentials are control-plane secrets — even
// fleet-token holders don't need to read them back).
function publicCell(cell) {
  const {
    provision_token: _provisionToken,
    backup_token: _backupToken,
    ...rest
  } = cell;
  return {
    ...rest,
    backup_validation_target:
      cell.backup_validation_target === true,
    has_provision_token: Boolean(cell.provision_token),
    has_backup_token: Boolean(cell.backup_token),
  };
}

// genInviteCode returns a short, unambiguous code like "k3v9-m2xq-7fjp".
function genInviteCode() {
  const alphabet = "abcdefghjkmnpqrstvwxyz23456789"; // no i/l/o/u/0/1
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  const chars = [...bytes].map((b) => alphabet[b % alphabet.length]);
  return `${chars.slice(0, 4).join("")}-${chars.slice(4, 8).join("")}-${chars.slice(8).join("")}`;
}

// validateInvite is the check the signup path runs. Returns {valid, reason}.
function validateInvite(entry) {
  if (!entry) return { valid: false, reason: "unknown code" };
  if (entry.enabled === false) return { valid: false, reason: "disabled" };
  const now = Date.now();
  if (entry.not_before && now < Date.parse(entry.not_before)) {
    return { valid: false, reason: "not yet valid" };
  }
  if (entry.expires_at && now >= Date.parse(entry.expires_at)) {
    return { valid: false, reason: "expired" };
  }
  if (Number.isFinite(entry.max_uses) && (entry.uses ?? 0) >= entry.max_uses) {
    return { valid: false, reason: "fully used" };
  }
  return { valid: true };
}

async function handleInvites(request, env, url) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }

  if (url.pathname === "/v1/invites" && request.method === "GET") {
    const out = [];
    let cursor;
    do {
      const page = await env.DIRECTORY.list({ prefix: "invite:", cursor });
      for (const k of page.keys) {
        const entry = await env.DIRECTORY.get(k.name, { type: "json" });
        if (entry) {
          out.push({ code: k.name.slice(7), ...entry, ...validateInvite(entry) });
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    return json({ schema_version: "witself.v0", invites: out });
  }

  if (url.pathname === "/v1/invites" && request.method === "POST") {
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid JSON body", 400);
    }
    const code = body.code ?? genInviteCode();
    if (!INVITE_CODE.test(code)) {
      return err("invalid code (lowercase letters/digits/hyphens, 3-64 chars)", 400);
    }
    for (const f of ["not_before", "expires_at"]) {
      if (body[f] != null && !Number.isFinite(Date.parse(body[f]))) {
        return err(`${f} must be an ISO-8601 timestamp`, 400);
      }
    }
    if (body.max_uses != null && (!Number.isInteger(body.max_uses) || body.max_uses < 1)) {
      return err("max_uses must be a positive integer", 400);
    }
    if (body.cell != null && !CELL_NAME.test(body.cell)) {
      return err("cell must be a valid cell name", 400);
    }
    if (body.region != null && !REGION_NAME.test(body.region)) {
      return err("region must be a region name like us-west-2", 400);
    }
    const key = `invite:${code}`;
    const existing = await env.DIRECTORY.get(key, { type: "json" });
    const entry = {
      enabled: body.enabled !== false,
      not_before: body.not_before ?? null,
      expires_at: body.expires_at ?? null,
      max_uses: body.max_uses ?? null,
      // Placement constraints: cell = hard pin, region = hard constraint.
      cell: body.cell ?? existing?.cell ?? null,
      region: body.region ?? existing?.region ?? null,
      // uses/created_at survive upserts; consumption belongs to the signup path.
      uses: existing?.uses ?? 0,
      note: body.note ?? existing?.note ?? "",
      created_at: existing?.created_at ?? new Date().toISOString(),
    };
    await env.DIRECTORY.put(key, JSON.stringify(entry));
    return json(
      { schema_version: "witself.v0", invite: { code, ...entry, ...validateInvite(entry) } },
      existing ? 200 : 201,
    );
  }

  const m = url.pathname.match(INVITE_PATH);
  if (m && request.method === "GET") {
    const entry = await env.DIRECTORY.get(`invite:${m[1]}`, { type: "json" });
    if (!entry) {
      return err("unknown invite", 404);
    }
    return json({
      schema_version: "witself.v0",
      invite: { code: m[1], ...entry, ...validateInvite(entry) },
    });
  }
  if (m && request.method === "DELETE") {
    const key = `invite:${m[1]}`;
    if ((await env.DIRECTORY.get(key)) === null) {
      return err("unknown invite", 404);
    }
    await env.DIRECTORY.delete(key);
    return new Response(null, { status: 204 });
  }

  return err("method not allowed", 405);
}

// handleAdmins is the fleet-token-gated admin credential registry. It
// serves four verbs — mint, list, revoke, delete — that maintain the
// admin: / admintok: / adminh: KV prefixes. The credentials it mints
// are what the witself-admin CLI carries when it hits the CP's admin-
// side fan-out routes; adminAuthorized() is the reader half.
//
// Handle uniqueness (adminh:{handle}) is enforced by get-then-put with
// no CAS — acceptable because mint is fleet-token-authorized (low rate,
// single-writer via the CLI). A concurrent double-mint race would leave
// both admin records in place but with only one owning the adminh index;
// the loser's token still works until manually revoked. Acceptable at
// slice-1b fleet scale.
async function handleAdmins(request, env, url) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }

  if (url.pathname === "/v1/admins" && request.method === "GET") {
    const out = [];
    let cursor;
    do {
      const page = await env.DIRECTORY.list({ prefix: "admin:", cursor });
      for (const k of page.keys) {
        // Only match admin:{id} — not admintok: or adminh: (list is
        // prefix-scoped, so those aren't matched, but this comment
        // documents the intent for a future reader).
        const rec = await env.DIRECTORY.get(k.name, { type: "json" });
        if (rec) out.push(publicAdmin(rec));
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    return json({ schema_version: "witself.v0", admins: out });
  }

  if (url.pathname === "/v1/admins" && request.method === "POST") {
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid JSON body", 400);
    }
    const handle = String(body.handle ?? "").toLowerCase().trim();
    const handleRefusal = validateMintHandle(handle);
    if (handleRefusal !== null) {
      return err(handleRefusal, 400);
    }
    const note = body.note == null ? undefined : String(body.note).trim();
    if (note !== undefined && note.length > 200) {
      return err("note too long (max 200 characters)", 400);
    }
    const scope = body.scope == null
      ? ADMIN_SCOPE_FULL
      : String(body.scope).trim();
    if (!ADMIN_SCOPES.has(scope)) {
      return err(`unknown scope "${scope}"`, 400);
    }
    const existingIdx = await env.DIRECTORY.get(`adminh:${handle}`, {
      type: "json",
    });
    if (existingIdx?.admin_id) {
      // The uniqueness index survives revoke on purpose — see file
      // header. A caller who wants the handle back must delete the
      // revoked record first (DELETE /v1/admins/{id}), which cleans
      // adminh too.
      return err("handle already in use", 409);
    }
    const adminID = genAdminID();
    const rawToken = genAdminToken();
    const tokenHash = await sha256Hex(rawToken);
    const now = new Date().toISOString();
    const rec = {
      admin_id: adminID,
      handle,
      token_hash: tokenHash,
      ...(scope !== ADMIN_SCOPE_FULL ? { scope } : {}),
      ...(note ? { note } : {}),
      created_at: now,
      created_by: "fleet_token",
    };
    // Best-effort three-write commit: canonical record first, then the
    // two indexes. If the last two fail, log and continue — a stale
    // admin:{id} without an adminh index just means the handle can be
    // re-claimed after DELETE. A stale record without admintok is
    // effectively already revoked (adminAuthorized returns null).
    await env.DIRECTORY.put(`admin:${adminID}`, JSON.stringify(rec));
    try {
      await env.DIRECTORY.put(
        `admintok:${tokenHash}`,
        JSON.stringify({ admin_id: adminID })
      );
      await env.DIRECTORY.put(
        `adminh:${handle}`,
        JSON.stringify({ admin_id: adminID })
      );
    } catch (e) {
      console.log(`admin-mint index-write failed for ${adminID}: ${String(e)}`);
    }
    return json(
      {
        schema_version: "witself.v0",
        admin_token: rawToken,
        admin: publicAdmin(rec),
      },
      201
    );
  }

  const revokeMatch = url.pathname.match(ADMIN_REVOKE_PATH);
  if (revokeMatch && request.method === "POST") {
    const adminID = revokeMatch[1];
    const rec = await env.DIRECTORY.get(`admin:${adminID}`, { type: "json" });
    if (!rec) return err("unknown admin", 404);
    if (rec.disabled_at) return err("already revoked", 409);
    const now = new Date().toISOString();
    const updated = { ...rec, disabled_at: now };
    await env.DIRECTORY.put(`admin:${adminID}`, JSON.stringify(updated));
    // Tombstone FIRST — adminAuthorized checks this before its normal
    // KV lookups, so a PoP with stale-cached admintok/admin records
    // still denies the revoked token on the next request (once its
    // own tombstone cache line is fresh). Written BEFORE the admintok
    // delete so there is no window where a PoP sees the deleted
    // admintok as still-present AND no tombstone yet.
    if (rec.token_hash) {
      await env.DIRECTORY.put(
        `adminrev:${rec.token_hash}`,
        JSON.stringify({ admin_id: adminID, revoked_at: now }),
        { expirationTtl: ADMIN_REVOKE_TOMBSTONE_TTL_SEC }
      );
    }
    // Second: kill the auth-lookup index. adminh:{handle} is
    // deliberately left in place — the handle stays reserved through
    // the audit window so historical events retain a distinguishable
    // author.
    if (rec.token_hash) {
      await env.DIRECTORY.delete(`admintok:${rec.token_hash}`);
    }
    return json({ schema_version: "witself.v0", admin: publicAdmin(updated) });
  }

  const idMatch = url.pathname.match(ADMIN_PATH);
  if (idMatch && request.method === "GET") {
    const rec = await env.DIRECTORY.get(`admin:${idMatch[1]}`, {
      type: "json",
    });
    if (!rec) return err("unknown admin", 404);
    return json({ schema_version: "witself.v0", admin: publicAdmin(rec) });
  }
  if (idMatch && request.method === "DELETE") {
    const adminID = idMatch[1];
    const rec = await env.DIRECTORY.get(`admin:${adminID}`, { type: "json" });
    if (!rec) return err("unknown admin", 404);
    if (!rec.disabled_at) {
      // Two-step deletion protects against fat-fingered revoke of an
      // active admin. Matches the "revoke before delete" rhythm.
      return err("revoke before delete", 409);
    }
    await env.DIRECTORY.delete(`admin:${adminID}`);
    if (rec.token_hash) {
      // Refresh the tombstone alongside the admintok delete. Delete
      // is only reachable after revoke (guarded above), so the
      // tombstone should already be present — but a fresh write
      // resets the TTL clock, covering the case where delete follows
      // a long-idle revoke by many minutes.
      await env.DIRECTORY.put(
        `adminrev:${rec.token_hash}`,
        JSON.stringify({ admin_id: adminID, revoked_at: rec.disabled_at }),
        { expirationTtl: ADMIN_REVOKE_TOMBSTONE_TTL_SEC }
      );
      await env.DIRECTORY.delete(`admintok:${rec.token_hash}`);
    }
    if (rec.handle) {
      await env.DIRECTORY.delete(`adminh:${rec.handle}`);
    }
    return new Response(null, { status: 204 });
  }

  return err("method not allowed", 405);
}

// fanoutCells calls the same POST path on every cell that has a
// provision token, in parallel, with a 15-second per-cell timeout. Never
// throws — a broken cell surfaces as an error entry in the returned
// array so the caller can render "3 of 4 cells reported" instead of
// silently dropping half the fleet. Result shape per cell:
//   { cell, status: "ok"|"error"|"timeout", http?, body?, error? }
async function fanoutCells(
  env,
  path,
  jsonBody,
  { requireEveryCell = false } = {},
) {
  const registeredCells = await listCells(env);
  const cells = requireEveryCell
    ? registeredCells
    : registeredCells.filter((c) => c.provision_token && c.endpoint);
  return Promise.all(
    cells.map(async (c) => {
      if (!c.provision_token || !c.endpoint) {
        return {
          cell: c.name,
          status: "error",
          error: "cell endpoint or provision token is missing",
        };
      }
      try {
        const r = await fetch(`${c.endpoint}${path}`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${c.provision_token}`,
          },
          body: JSON.stringify(jsonBody),
          signal: AbortSignal.timeout(15000),
        });
        const text = await r.text();
        let parsed = null;
        try {
          parsed = JSON.parse(text);
        } catch {
          // response wasn't JSON — fall through
        }
        return {
          cell: c.name,
          status: r.ok ? "ok" : "error",
          http: r.status,
          body: parsed,
          error: r.ok
            ? undefined
            : parsed?.error || text.slice(0, 200) || `HTTP ${r.status}`,
        };
      } catch (e) {
        // AbortSignal.timeout throws a DOMException("TimeoutError") on
        // deadline; other network errors throw TypeError. Both surface
        // as "timeout" here because the caller doesn't distinguish.
        return {
          cell: c.name,
          status: "timeout",
          error: String(e?.message ?? e),
        };
      }
    })
  );
}

function supportEmailIntakeDisposition(disposition) {
  // Deliberately value-free: email addresses, message identifiers, subjects,
  // bodies, account ids, and ticket ids never enter the Worker log.
  console.log(`support-email-intake disposition=${disposition}`);
  return json({ schema_version: "witself.v0", disposition });
}

async function finishSupportEmailIntake(env, key, disposition) {
  try {
    await env.DIRECTORY.put(key, "done", {
      expirationTtl: SUPPORT_EMAIL_INTAKE_DEDUP_TTL_SECONDS,
    });
  } catch {
    // A successful cell mutation without a durable terminal marker must be
    // retried. The cell's Message-ID idempotency makes that replay safe.
    console.log("support-email-intake outcome=dedup_error");
    return err("support email intake unavailable", 503);
  }
  return supportEmailIntakeDisposition(disposition);
}

async function postSupportEmailCell(cell, accountID, action, body) {
  return fetch(
    `${cell.endpoint}/v1/accounts/${accountID}/admin:${action}`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(15000),
    },
  );
}

// handleSupportEmailIntake terminates the one authenticated request emitted by
// the dark support-email bridge. Every externally visible successful outcome
// is a value-free disposition; the original email content only crosses the
// provision-token-authenticated cell boundary after an exact active match.
async function handleSupportEmailIntake(request, env) {
  // Keep the gate first: a dark deployment does not reveal that this route or
  // its credential exists.
  if (env.CP_SUPPORT_EMAIL_INTAKE_ENABLED !== "true") {
    return err("not found", 404);
  }
  if (request.method !== "POST") {
    return err("method not allowed", 405);
  }
  if (!supportEmailIntakeAuthorized(request, env)) {
    return err("unauthorized", 401);
  }

  let payload;
  try {
    payload = validateIntakePayload(await request.json());
  } catch {
    return err("invalid support email intake payload", 400);
  }

  let key;
  let pendingKey;
  try {
    key = await supportEmailDedupKey(payload.message_id);
    // Workers KV permits only one write per key per second. Keep the short
    // processing lease on a distinct key so a fast terminal outcome never
    // collides with its own seven-day dedup write.
    pendingKey = `${key}:pending`;
    const [dedupState, pendingState] = await Promise.all([
      env.DIRECTORY.get(key),
      env.DIRECTORY.get(pendingKey),
    ]);
    if (dedupState === "done") {
      return supportEmailIntakeDisposition("duplicate");
    }
    if (dedupState !== null || pendingState !== null) {
      // Do not acknowledge a delivery while another isolate owns the short
      // reservation. The Email Worker will throw on this response so the
      // provider retries after the lease either finishes or expires.
      return err("support email intake in progress", 503);
    }
    // Reserve before fan-out/cell mutation, but only for a short lease. A
    // failed or interrupted attempt therefore becomes retryable instead of
    // turning into a seven-day false duplicate.
    await env.DIRECTORY.put(pendingKey, "pending", {
      expirationTtl: SUPPORT_EMAIL_INTAKE_PENDING_TTL_SECONDS,
    });
  } catch {
    console.log("support-email-intake outcome=dedup_error");
    return err("support email intake unavailable", 503);
  }

  let perCell;
  try {
    perCell = await fanoutCells(
      env,
      "/v1/support/admin:match-contact",
      { email: payload.sender },
      { requireEveryCell: true },
    );
  } catch {
    return finishSupportEmailIntake(env, key, "drop_fanout_error");
  }
  const malformed = perCell.some((cell) =>
    cell.status !== "ok" || !Array.isArray(cell.body?.matches) ||
    cell.body.matches.some((match) =>
      match === null || typeof match !== "object" ||
      !ACCOUNT_ID.test(match.account_id ?? "") ||
      typeof match.status !== "string" || match.status === ""));
  if (malformed) {
    return finishSupportEmailIntake(env, key, "drop_fanout_error");
  }
  const matches = perCell.flatMap((cell) =>
    cell.body.matches.map((match) => ({ ...match, cell: cell.cell })));
  const decision = decideDisposition(matches);
  if (decision !== "proceed") {
    return finishSupportEmailIntake(env, key, decision);
  }

  const match = matches.find((candidate) => candidate.status === "active");
  let archived;
  let cell;
  try {
    archived = await env.DIRECTORY.get(`archived:${match.account_id}`, {
      type: "json",
    });
    if (!archived) {
      cell = await cellForAccount(env, match.account_id);
    }
  } catch {
    return finishSupportEmailIntake(env, key, "drop_fanout_error");
  }
  if (archived) {
    return finishSupportEmailIntake(env, key, "drop_archived");
  }
  if (!cell?.endpoint || !cell?.provision_token) {
    return finishSupportEmailIntake(env, key, "drop_fanout_error");
  }

  const commonBody = {
    email: payload.sender,
    body: payload.body,
    email_message_id: payload.message_id,
  };
  try {
    if (payload.ticket_tag) {
      const reply = await postSupportEmailCell(
        cell,
        match.account_id,
        "reply-email-ticket",
        { ...commonBody, ticket_id: payload.ticket_tag },
      );
      await reply.text();
      if (reply.ok) {
        return finishSupportEmailIntake(env, key, "replied");
      }
      if (reply.status !== 404 && reply.status !== 409) {
        console.log("support-email-intake outcome=cell_error");
        return err("support email intake cell request failed", 502);
      }
    }

    const opened = await postSupportEmailCell(
      cell,
      match.account_id,
      "open-email-ticket",
      {
        ...commonBody,
        subject: payload.subject,
      },
    );
    await opened.text();
    if (!opened.ok) {
      console.log("support-email-intake outcome=cell_error");
      return err("support email intake cell request failed", 502);
    }
    return finishSupportEmailIntake(env, key, "opened");
  } catch {
    console.log("support-email-intake outcome=cell_unreachable");
    return err("support email intake cell unreachable", 502);
  }
}

// Aggregate cap for fleet-wide fan-out result sets. Bounds the Worker
// response size in the pathological "10 cells × 500 tickets each" case.
// Callers get an "aggregate_capped: true" flag when we trim.
const FANOUT_AGG_CAP = 500;

// parseTS turns an RFC3339 timestamp into epoch millis for sorting;
// unparseable/missing values sort oldest.
function parseTS(s) {
  const n = Date.parse(s || "");
  return Number.isNaN(n) ? 0 : n;
}

// Customer-facing support notification. Fired inside handleAdminTickets
// via ctx.waitUntil() after a successful cell proxy for reply /
// state=resolved / state=closed. Idempotency dedups honest CLI retries
// on the same message-id; a throttle window coalesces bursts (e.g. an
// admin firing three replies in ten seconds → one email).
//
// The customer's contact address comes from the cell via the existing
// /v1/accounts/{id}:contact route (same one the recovery flow uses),
// so we honour whatever email the tenant has confirmed with the cell.
// Failure to fetch contact => silent skip; a broken cell is not the
// customer's problem, and the admin's action already committed.
async function fireSupportNotification(env, params) {
  const { action, accountID, cell, ticketID, admin, clientBody, parsed } = params;
  if (!env.EMAIL) return; // no email backend configured — dev / self-host
  // What kind of email is this, if any? For state changes we email
  // only on the two customer-visible transitions.
  let kind = null;
  if (action === "reply-ticket") kind = "reply";
  else if (action === "change-ticket-state") {
    const newState = String(clientBody?.state ?? "");
    if (newState === "resolved") kind = "resolved";
    else if (newState === "closed") kind = "closed";
  }
  if (!kind) return; // e.g. state → awaiting_customer: silent

  // Idempotency key: the message id (reply) or the ticket+state pair
  // (state change). An honest retry produces the same key and
  // short-circuits the send.
  let dedupKey;
  if (kind === "reply") {
    const msgID = parsed?.message?.id;
    if (!msgID) return; // response shape unexpected — silent skip
    dedupKey = `notify_dedup:reply:${msgID}`;
  } else {
    dedupKey = `notify_dedup:${kind}:${ticketID}`;
  }
  // Throttle window: at most one email per (account, ticket) per 5
  // minutes, regardless of kind. Bursts (reply→resolve→close within
  // seconds) coalesce; the customer sees the FIRST admin action,
  // then a quiet window, then whatever admin did next.
  const throttleKey = `notify_throttle:${accountID}:${ticketID}`;

  // Read both gates in parallel — narrows the check-time window
  // against a concurrent burst.
  const [dedupHit, throttleHit] = await Promise.all([
    env.DIRECTORY.get(dedupKey),
    env.DIRECTORY.get(throttleKey),
  ]);
  if (dedupHit || throttleHit) return;

  // RESERVE dedup + throttle BEFORE the slow work (contact fetch +
  // EMAIL.send, jointly hundreds of ms). This is what actually makes
  // the anti-burst guarantee stick: a CLI retry of a failed reply
  // (cell inserts a fresh message id — not idempotent — but the
  // throttleKey is msgID-independent) reads the throttle mid-flight
  // and short-circuits before firing a second email.
  //
  // Fail-safe direction on KV write error: DON'T send. Better to
  // miss one notification than to send two. Promise.all keeps the
  // two puts close together so a mid-put isolate-tear leaves at
  // most one reserved slot instead of a stuck dedup with no
  // throttle (which would weaken burst protection for the next 5m).
  //
  // Residual race: two admin actions arriving within a few ms of
  // each other can both pass the check + both write the reserve +
  // both send. That window is now bounded by KV write latency, not
  // send latency, so it's shrunk from hundreds of ms to a handful.
  // Truly-concurrent-burst elimination would need a Durable Object
  // per (account, ticket) — deferred to a later slice, since the
  // realistic burst pattern (an admin firing sequential CLI
  // commands) is now closed.
  try {
    await Promise.all([
      env.DIRECTORY.put(dedupKey, "1", { expirationTtl: 24 * 3600 }),
      env.DIRECTORY.put(throttleKey, "1", { expirationTtl: 5 * 60 }),
    ]);
  } catch (e) {
    console.log(
      `support-notify KV reserve failed for ${accountID}/${ticketID} (${kind}): ${String(
        e?.message ?? e
      )}`
    );
    return;
  }

  const contact = await fetchAccountContact(env, cell, accountID);
  if (!contact?.email || contact.status !== "active") return;

  const body = kind === "reply" ? String(parsed?.message?.body ?? "") : "";
  const emailArgs = renderSupportEmail(
    kind,
    parsed?.message?.author_kind,
    admin.handle,
    accountID,
    ticketID,
    body,
  );
  await env.EMAIL.send({
    to: contact.email,
    from: "no-reply@witwave.ai",
    ...emailArgs,
  });
}

// fetchAccountContact wraps the tenant :contact route. Provision-token
// authenticated; short 15s timeout matches the other cell proxies.
async function fetchAccountContact(env, cell, accountID) {
  try {
    const resp = await fetch(`${cell.endpoint}/v1/accounts/${accountID}:contact`, {
      method: "POST",
      headers: { Authorization: `Bearer ${cell.provision_token}` },
      signal: AbortSignal.timeout(15000),
    });
    if (!resp.ok) {
      await resp.text().catch(() => "");
      return null;
    }
    return await resp.json();
  } catch {
    return null;
  }
}

// handleAdminTickets serves the admin-token-authorized fan-out routes.
// Every route re-runs adminAuthorized so a revoked admin's live tokens
// stop working on the next request.
async function handleAdminTickets(request, env, ctx, url) {
  const admin = await adminAuthorized(request, env);
  if (!admin) return err("unauthorized", 401);

  // whoami: cheap round-trip that lets the CLI verify a token without
  // any KV list scan.
  if (url.pathname === "/v1/admin/whoami" && request.method === "GET") {
    if (!adminScopeAllows(admin.scope, "whoami")) {
      return err("credential scope does not allow this action", 403);
    }
    return json({
      schema_version: "witself.v0",
      admin_id: admin.admin_id,
      handle: admin.handle,
      scope: admin.scope,
    });
  }

  // Fleet cells with per-cell account counts — the dashboard's cells
  // pane. Counts come from the CP's own directory (acct: pointers),
  // which is authoritative for placement; an O(accounts) KV scan is
  // fine at current fleet scale (same tradeoff cellHasAccounts makes,
  // and the same DO-counter upgrade path applies when it stops being
  // fine). provision tokens never leave the CP (publicCell).
  if (url.pathname === "/v1/admin/cells" && request.method === "GET") {
    if (!adminScopeAllows(admin.scope, "cells")) {
      return err("credential scope does not allow this action", 403);
    }
    const cells = await listCells(env);
    const counts = new Map();
    let cursor;
    do {
      const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
      for (const k of page.keys) {
        const entry = await env.DIRECTORY.get(k.name, { type: "json" });
        if (entry?.cell) {
          counts.set(entry.cell, (counts.get(entry.cell) ?? 0) + 1);
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    // Archived accounts (evacuated to R2, awaiting placement) counted
    // by the cell they came FROM — they're not live anywhere, but the
    // origin cell is where an operator would go looking for them.
    const archivedCounts = new Map();
    cursor = undefined;
    do {
      const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
      for (const k of page.keys) {
        const entry = await env.DIRECTORY.get(k.name, { type: "json" });
        if (entry?.cell) {
          archivedCounts.set(entry.cell, (archivedCounts.get(entry.cell) ?? 0) + 1);
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    // Running software version per cell, straight from each cell's
    // public /v1/version (parallel, short timeout). null = the cell
    // didn't answer — the dashboard renders that as unreachable
    // rather than hiding the row.
    const versions = await Promise.all(
      cells.map(async (c) => {
        if (!c.endpoint) return null;
        try {
          const r = await fetch(`${c.endpoint}/v1/version`, {
            signal: AbortSignal.timeout(8000),
          });
          if (!r.ok) return null;
          const b = await r.json();
          return typeof b?.version === "string" ? b.version : null;
        } catch {
          return null;
        }
      })
    );
    return json({
      schema_version: "witself.v0",
      cells: cells.map((c, i) => ({
        ...publicCell(c),
        account_count: counts.get(c.name) ?? 0,
        archived_count: archivedCounts.get(c.name) ?? 0,
        ...(versions[i] ? { version: versions[i] } : {}),
      })),
    });
  }

  // Fleet-wide audit-event tail — the dashboard's events pane. Fans
  // out to every cell's /v1/events/admin:list, merges newest-first.
  // Same partial-failure honesty as /v1/admin/tickets: a broken cell
  // shows up in cells[], never as silently missing events.
  if (url.pathname === "/v1/admin/events" && request.method === "GET") {
    const since = url.searchParams.get("since");
    const verb = url.searchParams.get("verb");
    const limit = Number.parseInt(url.searchParams.get("limit") || "50", 10);
    const body = {
      admin_handle: admin.handle,
      since: since || undefined,
      verb: verb || undefined,
      limit: Number.isFinite(limit) && limit > 0 ? Math.min(limit, 500) : 50,
    };
    const perCell = await fanoutCells(env, "/v1/events/admin:list", body);
    let events = [];
    for (const c of perCell) {
      if (c.status !== "ok" || !c.body?.events) continue;
      for (const e of c.body.events) {
        events.push({ ...e, cell: c.cell });
      }
    }
    // Numeric compare, NOT lexicographic: Go trims trailing fractional
    // zeros from RFC3339 timestamps, so string order misranks
    // same-second events ('...00Z' > '...00.5Z' as strings).
    events.sort((a, b) => parseTS(b.occurred_at) - parseTS(a.occurred_at));
    const aggregateCapped = events.length > FANOUT_AGG_CAP;
    if (aggregateCapped) events = events.slice(0, FANOUT_AGG_CAP);
    return json({
      schema_version: "witself.v0",
      events,
      cells: perCell.map((c) => ({
        name: c.cell,
        status: c.status,
        count: c.status === "ok" ? (c.body?.events?.length ?? 0) : 0,
        ...(c.error ? { error: c.error } : {}),
      })),
      ...(aggregateCapped ? { aggregate_capped: true } : {}),
    });
  }

  // Fleet-wide list. Query params (all optional):
  //   state=<comma-list>   filter by ticket state
  //   since=<ISO>          last_activity_at >= since
  //   limit=<n>            per-cell limit (defaults to 100, capped at 500)
  if (url.pathname === "/v1/admin/tickets" && request.method === "GET") {
    if (!adminScopeAllows(admin.scope, "list-tickets")) {
      return err("credential scope does not allow this action", 403);
    }
    const states = url.searchParams.get("state");
    const since = url.searchParams.get("since");
    const limit = Number.parseInt(
      url.searchParams.get("limit") || "100",
      10
    );
    const body = {
      admin_handle: admin.handle,
      states: states ? states.split(",").map((s) => s.trim()).filter(Boolean) : undefined,
      since: since || undefined,
      limit: Number.isFinite(limit) && limit > 0 ? Math.min(limit, 500) : 100,
    };
    const perCell = await fanoutCells(
      env,
      "/v1/support/admin:list-tickets",
      body
    );
    // Interleave tickets from every ok cell, tag each with its cell,
    // sort newest-activity first.
    let tickets = [];
    for (const c of perCell) {
      if (c.status !== "ok" || !c.body?.tickets) continue;
      for (const t of c.body.tickets) {
        tickets.push({ ...t, cell: c.cell });
      }
    }
    // Numeric compare — see the events merge for why lexicographic
    // ordering is wrong for Go's variable-precision timestamps.
    tickets.sort(
      (a, b) => parseTS(b.last_activity_at) - parseTS(a.last_activity_at)
    );
    const aggregateCapped = tickets.length > FANOUT_AGG_CAP;
    if (aggregateCapped) tickets = tickets.slice(0, FANOUT_AGG_CAP);
    return json({
      schema_version: "witself.v0",
      tickets,
      cells: perCell.map((c) => ({
        name: c.cell,
        status: c.status,
        count: c.status === "ok" ? c.body?.tickets?.length ?? 0 : 0,
        ...(c.error ? { error: c.error } : {}),
      })),
      ...(aggregateCapped ? { aggregate_capped: true } : {}),
    });
  }

  // Per-account fan-in routes: resolve the account -> cell first, then
  // proxy to that cell's admin endpoint. Return 409 with a "restore
  // first" hint for archived accounts (slice 1b defers cross-cell
  // chase; a follow-up slice can transparently restore-then-retry).
  const routes = [
    { rx: ADMIN_ACCOUNT_TICKET_PATH, method: "GET", action: "get-ticket" },
    { rx: ADMIN_ACCOUNT_TICKET_MSGS_PATH, method: "POST", action: "reply-ticket" },
    { rx: ADMIN_ACCOUNT_TICKET_STATE_PATH, method: "PATCH", action: "change-ticket-state" },
    { rx: ADMIN_ACCOUNT_TICKET_RETRIAGE_PATH, method: "PATCH", action: "retriage-ticket" },
  ];
  for (const route of routes) {
    const m = url.pathname.match(route.rx);
    if (!m) continue;
    if (request.method !== route.method) {
      return err("method not allowed", 405);
    }
    if (!adminScopeAllows(admin.scope, route.action)) {
      return err("credential scope does not allow this action", 403);
    }
    const accountID = m[1];
    const ticketID = m[2];
    // archived: accounts have no live cell to talk to; a later slice
    // can transparently restore-then-retry. For 1b: predictable 409.
    const archived = await env.DIRECTORY.get(`archived:${accountID}`, {
      type: "json",
    });
    if (archived) {
      return err("account is archived — restore before support actions", 409);
    }
    const cell = await cellForAccount(env, accountID);
    if (!cell) {
      return err("unknown account", 404);
    }
    let clientBody = {};
    if (route.method !== "GET") {
      try {
        clientBody = (await request.json()) ?? {};
      } catch {
        return err("invalid JSON body", 400);
      }
    }
    const cellBody = {
      admin_handle: admin.handle,
      ticket_id: ticketID,
    };
    if (route.action === "reply-ticket") {
      cellBody.body = clientBody.body ?? "";
      cellBody.as_assistant = clientBody.as_assistant === true;
    } else if (route.action === "change-ticket-state") {
      cellBody.state = clientBody.state ?? "";
    } else if (route.action === "retriage-ticket") {
      cellBody.category = clientBody.category ?? "";
      cellBody.priority = clientBody.priority ?? "";
    }
    try {
      const cellRes = await fetch(
        `${cell.endpoint}/v1/accounts/${accountID}/admin:${route.action}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
          },
          body: JSON.stringify(cellBody),
          signal: AbortSignal.timeout(15000),
        }
      );
      const text = await cellRes.text();
      // Fire-and-forget customer email on a successful admin action.
      // waitUntil() lets the worker finish the email even after the
      // admin's response has flushed — the notification is a
      // best-effort side channel, never in the admin's critical path.
      if (cellRes.ok && ctx?.waitUntil) {
        let parsed = null;
        try {
          parsed = JSON.parse(text);
        } catch {
          // no notification if the cell body wasn't JSON
        }
        if (parsed) {
          ctx.waitUntil(
            fireSupportNotification(env, {
              action: route.action,
              accountID,
              cell,
              ticketID,
              admin,
              clientBody,
              parsed,
            }).catch((e) =>
              console.log(
                `support-notify ${route.action} ${accountID}/${ticketID}: ${String(e?.message ?? e)}`
              )
            )
          );
        }
      }
      // Pass status + parsed JSON through verbatim. The cell already
      // shapes errors the CLI can render; the Worker just relays.
      return new Response(text, {
        status: cellRes.status,
        headers: { "Content-Type": "application/json" },
      });
    } catch (e) {
      return err(`cell unreachable: ${String(e?.message ?? e)}`, 502);
    }
  }

  // Support-policy read/write. GET returns the current value; PATCH
  // flips it. Both are per-account and inherit the archived-first
  // and unknown-account handling from the ticket routes above.
  const spMatch = url.pathname.match(ADMIN_ACCOUNT_SUPPORT_POLICY_PATH);
  if (spMatch) {
    if (!adminScopeAllows(admin.scope, "support-policy")) {
      return err("credential scope does not allow this action", 403);
    }
    const method = request.method;
    if (method !== "GET" && method !== "PATCH") {
      return err("method not allowed", 405);
    }
    const accountID = spMatch[1];
    const archived = await env.DIRECTORY.get(`archived:${accountID}`, {
      type: "json",
    });
    if (archived) {
      return err("account is archived — restore before support actions", 409);
    }
    const cell = await cellForAccount(env, accountID);
    if (!cell) {
      return err("unknown account", 404);
    }
    let clientBody = {};
    if (method === "PATCH") {
      try {
        clientBody = (await request.json()) ?? {};
      } catch {
        return err("invalid JSON body", 400);
      }
    }
    const action = method === "GET" ? "get-support-policy" : "set-support-policy";
    const cellBody = { admin_handle: admin.handle };
    if (method === "PATCH") {
      cellBody.policy = clientBody.policy ?? "";
    }
    try {
      const cellRes = await fetch(
        `${cell.endpoint}/v1/accounts/${accountID}/admin:${action}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
          },
          body: JSON.stringify(cellBody),
          signal: AbortSignal.timeout(15000),
        }
      );
      const text = await cellRes.text();
      return new Response(text, {
        status: cellRes.status,
        headers: { "Content-Type": "application/json" },
      });
    } catch (e) {
      return err(`cell unreachable: ${String(e?.message ?? e)}`, 502);
    }
  }

  return err("not found", 404);
}

// handlePlacement is the fleet-wide default placement strategy: GET returns it
// (default weighted), POST sets it.
async function handlePlacement(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (request.method === "GET") {
    const cfg = (await env.DIRECTORY.get("config:placement", { type: "json" })) ?? {
      strategy: "weighted",
    };
    return json({ schema_version: "witself.v0", placement: cfg });
  }
  if (request.method === "POST") {
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid JSON body", 400);
    }
    if (!PLACEMENT_STRATEGIES.includes(body.strategy)) {
      return err(`strategy must be one of: ${PLACEMENT_STRATEGIES.join(", ")} (geo arrives with a multi-region fleet)`, 400);
    }
    if (body.strategy === "pinned" && !CELL_NAME.test(body.pinned_cell || "")) {
      return err("pinned strategy requires pinned_cell", 400);
    }
    const cfg = { strategy: body.strategy };
    if (body.strategy === "pinned") {
      cfg.pinned_cell = body.pinned_cell;
    }
    await env.DIRECTORY.put("config:placement", JSON.stringify(cfg));
    return json({ schema_version: "witself.v0", placement: cfg });
  }
  return err("method not allowed", 405);
}

// placeAccount picks the cell for a new account. Precedence: invite pin (hard),
// invite region (hard), fleet pinned strategy (soft), weighted random.
// Returns {cell} or {fail: Response}.
async function placeAccount(env, invite) {
  const backupsEnabled = accountBackupSchedulingEnabled(env);
  let pool = (await listCells(env)).filter(
    (cell) => cellIsEligibleDestination(cell, { backupsEnabled }),
  );
  if (invite.cell) {
    pool = pool.filter((c) => c.name === invite.cell);
    if (pool.length === 0) {
      return { fail: err(`no capacity: invite-pinned cell ${invite.cell} unavailable`, 503) };
    }
  } else if (invite.region) {
    pool = pool.filter((c) => c.region === invite.region);
    if (pool.length === 0) {
      return { fail: err(`no capacity in region ${invite.region}`, 503) };
    }
  }
  if (pool.length === 0) {
    return { fail: err("no capacity: no accepting cells", 503) };
  }

  const cfg = (await env.DIRECTORY.get("config:placement", { type: "json" })) ?? {};
  if (cfg.strategy === "pinned" && cfg.pinned_cell) {
    const pinned = pool.find((c) => c.name === cfg.pinned_cell);
    if (pinned) {
      return { cell: pinned }; // soft pin: absent/ineligible falls through
    }
  }

  const total = pool.reduce((s, c) => s + (c.weight > 0 ? c.weight : 1), 0);
  let r = Math.random() * total;
  let cell = pool[pool.length - 1];
  for (const c of pool) {
    r -= c.weight > 0 ? c.weight : 1;
    if (r <= 0) {
      cell = c;
      break;
    }
  }
  return { cell };
}

async function handleCells(request, env, url) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }

  if (url.pathname === "/v1/cells" && request.method === "GET") {
    const cells = (await listCells(env)).map(publicCell);
    return json({ schema_version: "witself.v0", cells });
  }

  if (url.pathname === "/v1/cells" && request.method === "POST") {
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid JSON body", 400);
    }
    if (!body || !CELL_NAME.test(body.name || "")) {
      return err("missing or invalid name", 400);
    }
    if (typeof body.endpoint !== "string" || !body.endpoint.startsWith("https://")) {
      return err("endpoint must be an https URL", 400);
    }
    if (body.provision_token != null && typeof body.provision_token !== "string") {
      return err("provision_token must be a string", 400);
    }
    if (body.backup_token != null && typeof body.backup_token !== "string") {
      return err("backup_token must be a string", 400);
    }
    if (
      body.backup_validation_target !== undefined &&
      typeof body.backup_validation_target !== "boolean"
    ) {
      return err("backup_validation_target must be a boolean", 400);
    }
    if (
      body.backup_token &&
      body.provision_token &&
      body.backup_token === body.provision_token
    ) {
      return err(
        "backup_token must be distinct from provision_token",
        400,
      );
    }
    if (
      body.registration_id != null &&
      (
        typeof body.registration_id !== "string" ||
        body.registration_id.length < 1 ||
        body.registration_id.length > 128
      )
    ) {
      return err("registration_id must be a nonempty opaque identifier", 400);
    }
    if (body.region_code != null && !REGION_NAME.test(body.region_code)) {
      return err("region_code must be a canonical region code like usw2", 400);
    }
    if (body.channel != null && !PLACEMENT_CHANNELS.has(body.channel)) {
      return err("channel must be stable, edge, or experimental", 400);
    }
    return requestCellCoordinator(
      env,
      body.name,
      "/register",
      {
        cell: {
          endpoint: body.endpoint,
          cloud: body.cloud || "",
          region: body.region || "",
          region_code: body.region_code,
          channel: body.channel,
          owner: "witwave",
          weight: Number.isFinite(body.weight) ? body.weight : 1,
          accepting: body.accepting !== false,
          backup_validation_target: body.backup_validation_target,
          provision_token: body.provision_token,
          backup_token: body.backup_token,
          registration_id: body.registration_id,
        },
      },
    );
  }

  // PURGE: the explicitly destructive removal path, for teardowns where the
  // cell's data is genuinely dying (witself-infra down --destroy-accounts).
  // Deletes every directory entry pointing at the cell, then the cell itself.
  // Idempotent: re-running reports zero. The safe path is DELETE below, which
  // refuses while accounts exist.
  const pm = url.pathname.match(PURGE_PATH);
  if (pm && request.method === "POST") {
    return requestCellCoordinator(env, pm[1], "/purge", {});
  }

  const m = url.pathname.match(CELL_PATH);
  if (m && request.method === "DELETE") {
    return requestCellCoordinator(env, m[1], "/delete", {
      deletion_id: crypto.randomUUID(),
    });
  }

  return err("method not allowed", 405);
}

// handleSignup is POST /v1/accounts — the public, invite-gated front door of
// Witself Cloud. A provision-id Durable Object persists the request
// fingerprint and orchestration checkpoints, but never the bootstrap token.
// Every ambiguous retry stays pinned to the same cell and replays the exact
// cell-side provision receipt before returning fresh credentials.
async function handleSignup(request, env) {
  let body;
  try {
    body = await request.json();
  } catch {
    return err("invalid JSON body", 400);
  }
  const provisionID = typeof body?.provision_id === "string"
    ? body.provision_id.trim()
    : "";
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(provisionID)) {
    return err(
      "provision_id is required and must be a nonempty opaque identifier",
      400,
    );
  }
  if (!env.ACCOUNT_SIGNUP) {
    return err("account signup Durable Object is unavailable", 503);
  }
  const id = env.ACCOUNT_SIGNUP.idFromName(`provision:${provisionID}`);
  try {
    return await env.ACCOUNT_SIGNUP.get(id).fetch(
      new Request("https://account-signup.internal/run", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...body,
          provision_id: provisionID,
          origin: new URL(request.url).origin,
        }),
      }),
    );
  } catch (error) {
    return err(
      `account signup outcome is ambiguous: ${String(error?.message ?? error)}`,
      502,
    );
  }
}

// sha256hex hashes a verification token for storage — KV holds only hashes,
// so a KV read can never recover a clickable link.
async function sha256hex(s) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// sendVerificationEmail mints a single-use verification token, stores its
// hash (verify:<hash> -> {account_id, cell}, self-expiring), and emails the
// link. Returns false when no EMAIL binding is configured — signup proceeds;
// the account simply stays pending until some other activation path exists.
// ---- Email presentation ----------------------------------------------------
// Every account email lands through renderEmail so the visual identity stays
// in one place. The template is table-based with fully inline styles because
// email clients (Gmail, Outlook, Apple Mail, Yahoo) strip <style> blocks and
// external CSS. System fonts only — web fonts don't load in most clients,
// including desktop Outlook. Colors are deliberately restrained: one accent
// (#4338ca) on white on a soft gray backdrop. Human-facing text passes
// through escapeHTML at every interpolation site — the panel wanted that
// discipline everywhere.

const EMAIL_FONT = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Oxygen,Ubuntu,Cantarell,sans-serif";
const EMAIL_ACCENT = "#4338ca";
const EMAIL_TEXT = "#0f172a";
const EMAIL_MUTED = "#64748b";
const EMAIL_BG = "#f4f5f7";
const EMAIL_CARD = "#ffffff";
const EMAIL_BORDER = "#eef0f4";

function escapeHTML(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// codeChip renders a large, mono, centered box for a code the user must type.
function codeChip(code) {
  return `<div style="background:${EMAIL_BG};border:1px solid ${EMAIL_BORDER};border-radius:8px;padding:20px;font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:24px;font-weight:600;letter-spacing:0.08em;color:${EMAIL_TEXT};text-align:center;margin:24px 0;">${escapeHTML(code)}</div>`;
}

// cliBlock renders a copyable command shown as a preformatted line.
function cliBlock(cmd) {
  return `<div style="background:${EMAIL_BG};border:1px solid ${EMAIL_BORDER};border-radius:6px;padding:14px 18px;font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:13px;color:${EMAIL_TEXT};overflow-x:auto;margin:16px 0;">${escapeHTML(cmd)}</div>`;
}

// ctaButton is a bulletproof-ish button (nested table + inline styles) so it
// renders on desktop Outlook too.
function ctaButton({ href, label }) {
  return `<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:24px 0 8px;"><tr><td style="border-radius:6px;background:${EMAIL_ACCENT};"><a href="${escapeHTML(href)}" style="display:inline-block;padding:13px 30px;font-family:${EMAIL_FONT};font-size:15px;font-weight:500;color:#ffffff;text-decoration:none;border-radius:6px;">${escapeHTML(label)}</a></td></tr></table>`;
}

// renderEmail wraps the message body in Witself's shared identity. Callers
// pass pre-formed HTML (already using the helpers above); the wrapper adds
// header, footer, preheader (hidden inbox-preview text), and the page chrome.
function renderEmail({ title, preheader, body }) {
  const preheaderMarkup = preheader
    ? `<div style="display:none;font-size:1px;color:${EMAIL_BG};line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">${escapeHTML(preheader)}</div>`
    : "";
  return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHTML(title)}</title></head><body style="margin:0;padding:0;background:${EMAIL_BG};">${preheaderMarkup}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:${EMAIL_BG};padding:40px 12px;"><tr><td align="center"><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background:${EMAIL_CARD};border-radius:8px;box-shadow:0 1px 3px rgba(0,0,0,0.05);"><tr><td style="padding:28px 40px;border-bottom:1px solid ${EMAIL_BORDER};"><div style="font-family:${EMAIL_FONT};font-size:19px;font-weight:600;color:${EMAIL_TEXT};letter-spacing:-0.02em;">Witself</div></td></tr><tr><td style="padding:32px 40px;font-family:${EMAIL_FONT};font-size:15px;line-height:1.65;color:${EMAIL_TEXT};"><h1 style="margin:0 0 20px;font-size:22px;font-weight:600;letter-spacing:-0.01em;color:${EMAIL_TEXT};">${escapeHTML(title)}</h1>${body}</td></tr><tr><td style="padding:20px 40px 28px;border-top:1px solid ${EMAIL_BORDER};font-family:${EMAIL_FONT};font-size:13px;line-height:1.55;color:${EMAIL_MUTED};">Sent by Witself. If you didn't expect this email, you can safely ignore it.</td></tr></table></td></tr></table></body></html>`;
}

async function sendVerificationEmail(env, origin, email, accountId, cellName, opts = {}) {
  if (!env.EMAIL) {
    return false;
  }
  const raw = new Uint8Array(32);
  crypto.getRandomValues(raw);
  const token = [...raw].map((b) => b.toString(16).padStart(2, "0")).join("");
  await env.DIRECTORY.put(
    `verify:${await sha256hex(token)}`,
    JSON.stringify({
      account_id: accountId,
      cell: cellName,
      created_at: new Date().toISOString(),
    }),
    { expirationTtl: 7 * 24 * 3600 }, // links self-expire; the reaper usually wins long before
  );
  const link = `${origin}/verify/${token}`;
  const deadline = await verificationDeadline(env, opts.windowStartedAt);
  const opening = opts.resend
    ? "Here is a fresh verification link for your Witself account."
    : "Welcome to Witself. One click to activate your account.";
  const title = opts.resend ? "A fresh verification link" : "Verify your account";
  const preheader = opts.resend
    ? "A fresh link — your original verification window still applies."
    : "One click to activate your account.";
  const body = `
    <p style="margin:0 0 16px;">${escapeHTML(opening)}</p>
    <p style="margin:0 0 8px;color:${EMAIL_MUTED};font-size:13px;">Account</p>
    <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 4px;">${escapeHTML(accountId)}</div>
    ${ctaButton({ href: link, label: "Verify my account" })}
    <p style="margin:24px 0 0;color:${EMAIL_MUTED};font-size:14px;">${escapeHTML(deadline)}</p>
  `;
  await env.EMAIL.send({
    to: email,
    from: "no-reply@witwave.ai",
    subject: "Verify your Witself account",
    text: `${opening}\n\nAccount: ${accountId}\n\nVerify by opening this link:\n\n  ${link}\n\n${deadline}\n\nIf you didn't sign up, you can ignore this email.\n`,
    html: renderEmail({ title, preheader, body }),
  });
  return true;
}

// verificationDeadline phrases the REAL reaper window for the email, so the
// stated deadline and the enforcement can't drift apart. For a resend,
// windowStartedAt lets it state the REMAINING time — a fresh link never
// resets the reap clock and must not pretend otherwise.
async function verificationDeadline(env, windowStartedAt) {
  const cfg = (await env.DIRECTORY.get("config:reaper", { type: "json" })) ?? {};
  if (cfg.enabled !== true || !(cfg.ttl_minutes > 0)) {
    return "Unverified accounts may be closed automatically.";
  }
  const phrase = (m) =>
    m < 120 ? `${m} minutes` : m < 2880 ? `${Math.round(m / 60)} hours` : `${Math.round(m / 1440)} days`;
  const started = windowStartedAt ? Date.parse(windowStartedAt) : NaN;
  if (!Number.isNaN(started)) {
    const remaining = Math.floor(cfg.ttl_minutes - (Date.now() - started) / 60000);
    if (remaining <= 1) {
      return "Your verification window is almost over — this account closes very soon if unverified.";
    }
    return `Your original verification window still applies: about ${phrase(remaining)} remain before the account closes unverified.`;
  }
  return `Unverified accounts are closed automatically after ${phrase(cfg.ttl_minutes)}.`;
}

// handleVerify is GET /verify/<token> — the human half of activation. The
// token's hash locates the account, the cell flips it active (only-if-pending
// — the cell is truth), and only on the cell's acknowledgement does the
// control plane retire the verification token and the reap candidate.
async function handleVerify(env, token) {
  const key = `verify:${await sha256hex(token)}`;
  const entry = await env.DIRECTORY.get(key, { type: "json" });
  if (!entry?.account_id || !entry?.cell || !ACCOUNT_ID.test(entry.account_id)) {
    return htmlPage(404, "Link invalid or expired", "This verification link is invalid or has expired. If you already verified, your account is active — <code>witself account status</code> will confirm it. If the account was closed for missing the verification window, simply sign up again.");
  }
  const cell = await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" });
  if (!cell?.provision_token || !cell?.endpoint) {
    return htmlPage(503, "Temporarily unavailable", "We couldn't reach your account's home just now. Please try the link again in a few minutes.");
  }
  let resp;
  try {
    resp = await fetch(`${cell.endpoint}/v1/accounts/${entry.account_id}:activate`, {
      method: "POST",
      headers: { Authorization: `Bearer ${cell.provision_token}` },
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return htmlPage(503, "Temporarily unavailable", "We couldn't reach your account's home just now. Please try the link again in a few minutes.");
  }
  let body = null;
  try {
    body = await resp.json();
  } catch {
    // Not JSON — not an answer from a witself-server handler.
  }
  if (resp.ok && body?.status === "active" && body?.account_id === entry.account_id) {
    // Deliberately KEEP the verify: key: mail scanners and prefetchers GET
    // links before the human ever clicks, so the human's click is often the
    // SECOND request. Replay is harmless — the cell answers already-active
    // idempotently — and the key (a hash) self-expires on its KV TTL.
    await env.DIRECTORY.delete(`pending:${entry.account_id}`); // the reaper stands down
    if (body.activated === false) {
      // Any later link (a resent email, a second click) lands here.
      return htmlPage(200, "Already verified", `Your account <code>${entry.account_id}</code> was already verified — nothing more to do. Back in your terminal, <code>witself account status</code> will show it active.`);
    }
    return htmlPage(200, "Account verified", `Your account <code>${entry.account_id}</code> is active. Back in your terminal, <code>witself account status</code> will confirm it — you're ready to create realms and agents.`);
  }
  // Dead-link arms match the cell handler's EXACT error strings: an old cell
  // whose dispatcher predates :activate answers 404 with a DIFFERENT JSON
  // message, and that must stay retryable — never burn a live account's link
  // over a deploy-ordering gap.
  if (resp.status === 409 && body?.error === "account cannot be activated") {
    await env.DIRECTORY.delete(key); // beyond activating; the link is dead
    return htmlPage(410, "Link expired", "This account was closed before it was verified. Sign up again to get a fresh account and link.");
  }
  if (resp.status === 404 && body?.error === "account not found") {
    await env.DIRECTORY.delete(key);
    return htmlPage(404, "Account not found", "This account no longer exists. Sign up again to get a fresh account and link.");
  }
  return htmlPage(503, "Temporarily unavailable", "Something went wrong verifying your account. Please try the link again in a few minutes.");
}

// htmlPage renders the tiny human-facing pages for /verify links.
function htmlPage(status, title, message) {
  return new Response(
    `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${title} — Witself</title><style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:15vh auto 0;padding:0 1.5rem;color:#1a1a1a}h1{font-size:1.4rem}code{background:#f2f2f2;padding:.1em .3em;border-radius:4px}</style></head><body><h1>${title}</h1><p>${message}</p></body></html>`,
    { status, headers: { "Content-Type": "text/html; charset=utf-8" } },
  );
}

// handleResend is POST /v1/accounts/{id}:resend-verification — a fresh
// verification email for a still-pending account. The control plane holds no
// authority here either: it forwards the caller's operator token to the
// account's cell (GET /v1/account, reachable at any status), and only a
// "live operator, this account, still pending" answer mints and sends. The
// email address comes from the cell's record, used in flight, never stored.
async function handleResend(request, env, accountId) {
  const auth = request.headers.get("Authorization");
  if (!auth) {
    return err("operator token required", 401);
  }
  const entry = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (!entry) {
    return err("unknown account", 404);
  }
  // Rate limit BEFORE spending a cell round trip. An invite plus a victim's
  // email address must not become a spam cannon: sends are capped per
  // account and spaced by a cooldown, tracked on the pending: entry.
  // Best-effort counting (KV has no atomic increment — same accepted
  // pattern as invite uses; the DO authority tightens both later). A
  // MISSING candidate is decided after the cell answers — usually it means
  // the account already activated, and "already active" beats a misleading
  // "try again shortly"; either way, no candidate ever means no send.
  const pendingKey = `pending:${accountId}`;
  const pending = await env.DIRECTORY.get(pendingKey, { type: "json" });
  if (pending) {
    const sent = pending.emails_sent ?? 1; // signup's email is the first
    if (sent >= 5) {
      return err("too many verification emails for this account — it closes unverified at the end of its window", 429);
    }
    if (pending.last_email_at && Date.now() - Date.parse(pending.last_email_at) < 2 * 60 * 1000) {
      return err("a verification email was just sent — wait a couple of minutes", 429);
    }
  }
  let cellResp;
  try {
    cellResp = await fetch(`${entry.endpoint}/v1/account`, {
      headers: { Authorization: auth },
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return err(`cell ${entry.cell} unreachable — try again shortly`, 502);
  }
  if (!cellResp.ok) {
    // Pass the cell's refusal (401/...) through verbatim.
    const text = await cellResp.text();
    return new Response(text, {
      status: cellResp.status,
      headers: { "Content-Type": "application/json" },
    });
  }
  let account = null;
  try {
    account = (await cellResp.json()).account;
  } catch {
    account = null;
  }
  if (!account?.id || account.id !== accountId) {
    return err("token does not belong to this account", 403);
  }
  if (account.status !== "pending") {
    return err(`account is already ${account.status}`, 409);
  }
  if (!pending) {
    // The cell says pending but the candidate is missing (KV lag, or the
    // reaper is mid-flight). Without it there is no rate-limit state, so
    // fail closed rather than send unmetered email.
    return err("verification state unavailable — try again shortly", 503);
  }
  let emailSent = false;
  try {
    emailSent = await sendVerificationEmail(
      env,
      new URL(request.url).origin,
      account.email,
      accountId,
      entry.cell,
      { resend: true, windowStartedAt: pending.created_at },
    );
  } catch {
    // Provider errors routinely embed recipient addresses and message
    // content; only the bounded reason code is loggable.
    console.log(`resend: verification email send failed for ${accountId} (reason=email_send_error)`);
  }
  if (!emailSent) {
    return err("could not send the verification email — try again shortly", 502);
  }
  // created_at is preserved deliberately: resending never resets the reap
  // clock — the email says so. The counter reads from `pending` directly:
  // the earlier `sent` binding lives inside the if(pending) block, and
  // referencing it here threw after the email had already left, which both
  // failed every successful resend and left the send cap un-advanced.
  await env.DIRECTORY.put(
    pendingKey,
    JSON.stringify({
      ...pending,
      emails_sent: (pending.emails_sent ?? 1) + 1,
      last_email_at: new Date().toISOString(),
    }),
  );
  // Look up the cell so we can post the audit event. The KV read is
  // wrapped because THIS handler already emailed the user and burned a
  // rate-limit slot; letting a transient KV error crash the response
  // with a 500 would make the user re-request and hit the cooldown for
  // an email that DID go out. audit is best-effort — the request
  // succeeded even without the event landing.
  let resendCell = null;
  try {
    resendCell = await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" });
  } catch {
    console.log(`resend: audit cell lookup failed for ${accountId} (reason=registry_read_error)`);
  }
  await logCellEvent(resendCell, accountId, "account.email.verify.sent",
    "control_plane", { to_masked: maskEmail(account.email) });
  return json({
    schema_version: "witself.v0",
    account_id: accountId,
    email: account.email,
    verification_email_sent: true,
  });
}

// handleRecover is POST /v1/accounts/{id}:recover — lost-token recovery, the
// one UNAUTHENTICATED account verb (the caller has nothing left to present).
// Two modes by body: {} requests a code, {"code"} redeems it. Inbox control
// is the proof: the code goes to the account's email (read from the cell via
// :contact), and only a correct redeem makes the cell rotate the root
// operator's credentials. Requesting never changes the account. Every answer
// to the request mode is identical whether the account exists or not, and
// rate-limit state is kept per-id in KV — for phantom ids too, so refusals
// leak nothing.
// recoverStateTtlSeconds derives the KV TTL for a recover:<id> write from
// the absolute quota window stamped at reservation time, so no later write
// — a wrong-code attempt above all — can extend the record's lifetime past
// the original 4-hour boundary. KV enforces a 60-second minimum TTL, so a
// code issued in the window's final minute simply dies with the window
// (fail-closed). State written before the window field existed falls back
// to the full 4-hour bound.
function recoverStateTtlSeconds(rl) {
  const windowMs = Date.parse(rl.window_expires_at ?? "");
  if (!Number.isFinite(windowMs)) {
    return 4 * 3600;
  }
  const remaining = Math.ceil((windowMs - Date.now()) / 1000);
  return Math.max(60, Math.min(4 * 3600, remaining));
}

async function handleRecover(request, env, accountId) {
  let body = {};
  try {
    body = await request.json();
  } catch {
    // empty body = request mode
  }
  const sourceIp = request.headers.get("CF-Connecting-IP") || "unknown";
  // Edge rate limit fires FIRST — before any KV read, before the cell
  // round-trip, before anything the Worker does. Cloudflare enforces
  // this atomically at the datacenter level, so a concurrent burst
  // (curl -P N) sees the platform's counter serialize the increments;
  // the request never reaches the Worker's KV-based logic which has
  // no CAS. See #23 for the follow-up context; #14 for what the KV
  // counter still handles (4h quota, fail-open on infra failure).
  // Only applied to request mode — redeem mode carries a code that
  // was already gated by a real email send.
  if (!body.code && env.RECOVER_LIMITER) {
    const { success } = await env.RECOVER_LIMITER.limit({
      key: `${sourceIp}:${accountId}`,
    });
    if (!success) {
      return err("too many recovery requests — try again later", 429);
    }
  }
  const rlKey = `recover:${accountId}`;
  const rl = (await env.DIRECTORY.get(rlKey, { type: "json" })) ?? {};
  // Per-(account, source-IP) 4-hour quota (KV-backed). The edge
  // limiter above handles burst enforcement; this handles the
  // longer-term "one attacker can't send more than 3 emails per 4
  // hours per account from their IP" bound. Missing CF-Connecting-IP
  // falls back to "unknown" (one shared bucket).
  const rlPerIpKey = `recover-ip:${accountId}:${sourceIp}`;
  const rlPerIp = (await env.DIRECTORY.get(rlPerIpKey, { type: "json" })) ?? {};

  if (!body.code) {
    // ---- Request mode: maybe send a code; always answer the same. ----
    // The generic answer must be indistinguishable across (phantom id, real
    // id, cell down, email backend down) — in both response BODY and, as
    // best a Worker can, response LATENCY. That means the cell round trip
    // happens for phantom ids too, and rate-limit state advances even for
    // phantom probes so an attacker can't scan account-ids for free.
    const generic = () =>
      json({
        schema_version: "witself.v0",
        message: "if the account exists and is active, a recovery code was emailed",
      });
    // Both 429 error responses use the SAME string so the difference
    // between "your IP is capped" and "your account is capped" isn't
    // an oracle an attacker can use to fingerprint which accounts are
    // under attack from which networks.
    const rateLimited = () =>
      err("too many recovery requests — try again later", 429);
    // Per-IP cap fires FIRST. Attacker on one IP burns 3 emails, then
    // sees 429 while any DIFFERENT IP still has full quota.
    if ((rlPerIp.emails_sent ?? 0) >= 3) {
      return rateLimited();
    }
    // Per-account cap is the backstop against distributed attacks.
    // Raised from 5 to 10 to accommodate legitimate owners retrying
    // from home/office/mobile; distributed attacks still lock out but
    // the resulting spam is a very visible signal.
    if ((rl.emails_sent ?? 0) >= 10) {
      return rateLimited();
    }
    if (rl.last_email_at && Date.now() - Date.parse(rl.last_email_at) < 2 * 60 * 1000) {
      return err("a recovery code was just sent — wait a couple of minutes", 429);
    }
    // Reserve the slots BEFORE the expensive cell round-trip and email
    // send. This shrinks the KV read-modify-write race window from
    // ~15s (the full cell-fetch + send time) to the microseconds
    // between get() and put(). The reserved slot is deliberately spent
    // whether or not the send later succeeds: quota treatment must never
    // depend on whether the account exists, and phantom ids cannot
    // observe a provider failure, so refunding real accounts on a send
    // error would turn a mail outage into an existence oracle (refunded
    // real ids answer 200 forever while metered phantoms hit the cap).
    // The absent-EMAIL branch below refunds UNIFORMLY instead, because a
    // missing binding is global configuration both sides share. TTL is 4h
    // so an owner who hits the cap during an incident recovers within a
    // working day, and a burst attack's blast radius is bounded to hours
    // not a full day.
    const now = new Date().toISOString();
    rl.emails_sent = (rl.emails_sent ?? 0) + 1;
    rl.last_email_at = now;
    // Each ADMITTED send re-stamps the absolute window (sliding on sends,
    // exactly the TTL refresh this put always performed); every later write
    // in this window derives a bounded remaining TTL from it instead of
    // resetting the clock.
    rl.window_expires_at = new Date(Date.now() + 4 * 3600 * 1000).toISOString();
    rlPerIp.emails_sent = (rlPerIp.emails_sent ?? 0) + 1;
    rlPerIp.last_email_at = now;
    await Promise.all([
      env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: 4 * 3600 }),
      env.DIRECTORY.put(rlPerIpKey, JSON.stringify(rlPerIp), { expirationTtl: 4 * 3600 }),
    ]);

    // Always fetch the cell, even for phantom ids: use the placement pool
    // as a stand-in when there's no acct: pointer, so latency doesn't
    // distinguish real from phantom. If nothing usable exists, skip.
    const entry = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
    let cell = entry
      ? await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" })
      : (await listCells(env)).find((c) => c.provision_token && c.endpoint) || null;
    let contact = null;
    if (cell?.provision_token && cell?.endpoint) {
      try {
        const resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:contact`, {
          method: "POST",
          headers: { Authorization: `Bearer ${cell.provision_token}` },
          signal: AbortSignal.timeout(15000),
        });
        // Only trust the answer when we hit the REAL routing pointer's cell.
        if (entry && resp.ok) {
          contact = await resp.json();
        } else {
          await resp.text().catch(() => "");
        }
      } catch {
        contact = null;
      }
    }
    if (contact?.email && contact.status === "active" && env.EMAIL) {
      // recovery.requested lands regardless of email outcome — someone
      // asked for a recovery, that's audit-worthy even if the email
      // backend blew up moments later. Only fires on REAL routing (the
      // enclosing `if entry && contact` shape prevents phantom-id
      // recovery events from leaking cell state).
      await logCellEvent(cell, accountId, "recovery.requested",
        "control_plane", { email_masked: maskEmail(contact.email) });
      const raw = new Uint32Array(3);
      crypto.getRandomValues(raw);
      const code = [...raw].map((n) => String(n % 1000).padStart(3, "0")).join("-");
      try {
        const cmd = `witself account recover --id ${accountId} --code ${code}`;
        await env.EMAIL.send({
          to: contact.email,
          from: "no-reply@witwave.ai",
          subject: "Your Witself recovery code",
          text: `A recovery was requested for your Witself account.\n\nAccount: ${accountId}\nCode:    ${code}\n\nIt expires in 15 minutes. Redeem it at your terminal:\n\n  ${cmd}\n\nIf you didn't request this, you can ignore this email — nothing changes until the code is used.\n`,
          html: renderEmail({
            title: "Recovery code",
            preheader: "Redeem this code at your terminal within 15 minutes.",
            body: `
              <p style="margin:0 0 8px;">A recovery was requested for your Witself account.</p>
              <p style="margin:0 0 8px;color:${EMAIL_MUTED};font-size:13px;">Account</p>
              <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 20px;">${escapeHTML(accountId)}</div>
              ${codeChip(code)}
              <p style="margin:0 0 8px;">Redeem the code at your terminal:</p>
              ${cliBlock(cmd)}
              <p style="margin:20px 0 0;color:${EMAIL_MUTED};font-size:14px;">The code expires in 15 minutes. If you didn't request this, you can ignore this email — nothing changes until the code is used.</p>
            `,
          }),
        });
        // Persist the fresh code shape — the slot counters were already
        // written above (fail-closed reserve pattern), so this only
        // stores the code_hash + expiration alongside them.
        rl.code_hash = await sha256hex(code.replaceAll("-", ""));
        rl.code_expires_at = new Date(Date.now() + 15 * 60 * 1000).toISOString();
        rl.attempts = 0;
        await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: recoverStateTtlSeconds(rl) });
        await logCellEvent(cell, accountId, "account.email.recovery.sent",
          "control_plane", { to_masked: maskEmail(contact.email) });
      } catch {
        console.log(`recover: recovery email send failed for ${accountId} (reason=email_send_error)`);
        // Deliberately NO refund: a provider failure is only observable on
        // real-account requests, so refunding here would meter phantoms
        // while real ids ride free — a stable existence oracle during any
        // mail outage. The spent slot expires with the 4h window.
      }
    }
    if (!env.EMAIL) {
      // No send can happen for ANYONE while outbound mail is unconfigured:
      // refund the reserved slots uniformly — real and phantom ids alike —
      // so a configuration gap neither burns the owner's quota (no email
      // was attempted or delivered) nor becomes an existence oracle
      // through divergent cap behavior between refunded real accounts and
      // still-metered phantoms. Pacing (last_email_at) survives so retry
      // rhythm stays uniform, and the edge limiter still bounds request
      // volume, so the missing backend opens no abuse bypass.
      rl.emails_sent = Math.max(0, (rl.emails_sent ?? 1) - 1);
      rlPerIp.emails_sent = Math.max(0, (rlPerIp.emails_sent ?? 1) - 1);
      await Promise.all([
        env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: recoverStateTtlSeconds(rl) }),
        env.DIRECTORY.put(rlPerIpKey, JSON.stringify(rlPerIp), { expirationTtl: 4 * 3600 }),
      ]);
    }
    return generic();
  }

  // ---- Redeem mode: verify the code, then have the cell rotate. ----
  if (!rl.code_hash || !rl.code_expires_at) {
    return err("invalid or expired recovery code", 401);
  }
  if ((rl.attempts ?? 0) >= 5) {
    return err("too many attempts — request a new code", 429);
  }
  // Count the attempt BEFORE comparing (fail closed on a crashed write).
  // The TTL is derived from the absolute reservation window: a wrong-code
  // attempt must never extend the recovery or lockout window beyond the
  // boundary the request-mode reservation established.
  rl.attempts = (rl.attempts ?? 0) + 1;
  await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: recoverStateTtlSeconds(rl) });
  const presented = await sha256hex(String(body.code).replace(/[^0-9]/g, ""));
  if (presented !== rl.code_hash || Date.parse(rl.code_expires_at) < Date.now()) {
    return err("invalid or expired recovery code", 401);
  }

  const entry = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (!entry) {
    return err("unknown account", 404);
  }
  const cell = await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" });
  if (!cell?.provision_token || !cell?.endpoint) {
    return err(`cell ${entry.cell} unavailable — try again shortly`, 502);
  }
  let resp;
  try {
    resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:recover`, {
      method: "POST",
      headers: { Authorization: `Bearer ${cell.provision_token}` },
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return err(`cell ${entry.cell} unreachable — try again shortly`, 502);
  }
  let recovered = null;
  try {
    recovered = (await resp.json()).account;
  } catch {
    recovered = null;
  }
  if (!resp.ok || !recovered?.bootstrap_token || recovered.account_id !== accountId) {
    return err("account cannot be recovered", resp.status === 409 ? 409 : 502);
  }
  await env.DIRECTORY.delete(rlKey); // the code is spent
  return json({
    schema_version: "witself.v0",
    account_id: recovered.account_id,
    operator_id: recovered.operator_id,
    email: recovered.email,
    status: recovered.status,
    cell: { name: entry.cell, endpoint: cell.endpoint },
    bootstrap_token: recovered.bootstrap_token,
  });
}

// handleChangeEmail is POST /v1/accounts/{id}:change-email — a routine,
// operator-authenticated move of the account's contact address. Two modes by
// body: {new_email} sends a confirmation code to the NEW address (proving it
// can receive) plus a warning notice to the current one; {new_email, code}
// commits via the cell's owner-only :update-email. Unlike recovery nothing
// rotates, and unlike recovery there is no anti-enumeration theater — the
// caller is already authenticated.
async function handleChangeEmail(request, env, accountId) {
  const auth = request.headers.get("Authorization");
  if (!auth) {
    return err("operator token required", 401);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return err("invalid JSON body", 400);
  }
  const newEmail = String(body.new_email ?? "").trim().toLowerCase();
  if (!newEmail || !newEmail.includes("@")) {
    return err("valid new_email required", 400);
  }
  const entry = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (!entry) {
    return err("unknown account", 404);
  }
  // The caller must hold a live operator token for THIS account. cell.endpoint
  // (live registry) is preferred over entry.endpoint (frozen at signup) so a
  // rebuild/re-endpoint of the cell keeps working.
  const cell = await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" });
  if (!cell?.endpoint || !cell?.provision_token) {
    return err(`cell ${entry.cell} unavailable — try again shortly`, 502);
  }
  let account = null;
  let operatorID = "";
  try {
    const resp = await fetch(`${cell.endpoint}/v1/account`, {
      headers: { Authorization: auth },
      signal: AbortSignal.timeout(15000),
    });
    if (!resp.ok) {
      const text = await resp.text();
      return new Response(text, {
        status: resp.status,
        headers: { "Content-Type": "application/json" },
      });
    }
    account = (await resp.json()).account;
    const who = await fetch(`${cell.endpoint}/v1/whoami`, {
      headers: { Authorization: auth },
      signal: AbortSignal.timeout(15000),
    });
    if (who.ok) {
      operatorID = (await who.json()).principal?.operator_id ?? "";
    }
  } catch {
    return err(`cell ${entry.cell} unreachable — try again shortly`, 502);
  }
  if (!account?.id || account.id !== accountId) {
    return err("token does not belong to this account", 403);
  }
  if (account.status !== "active") {
    return err(`account is ${account.status} — email changes need an active account`, 409);
  }
  if (!operatorID) {
    return err("could not identify the operator — try again shortly", 502);
  }
  // Owner-gate the request too, not just the commit. Non-owner tokens must
  // not be able to burn the 5/24h quota or noise up the owner's inbox.
  let operators = null;
  try {
    const list = await fetch(`${cell.endpoint}/v1/operators`, {
      headers: { Authorization: auth },
      signal: AbortSignal.timeout(15000),
    });
    if (list.ok) {
      operators = (await list.json()).operators;
    }
  } catch {
    operators = null;
  }
  const me = Array.isArray(operators)
    ? operators.find((o) => o?.id === operatorID)
    : null;
  if (!me?.is_root && me?.role !== "account_owner") {
    return err("only the account owner can change the email", 403);
  }

  if (!account.email) {
    // No prior address = no counter-move channel. Refuse rather than
    // silently move the anchor without an alarm path.
    return err("this account has no email on file — recovery must run through support", 409);
  }

  const key = `emailchange:${accountId}`;
  const state = (await env.DIRECTORY.get(key, { type: "json" })) ?? {};

  if (!body.code) {
    // ---- Request mode: code to the new address, notice to the old. ----
    if ((state.emails_sent ?? 0) >= 5) {
      return err("too many email-change requests for this account — try again tomorrow", 429);
    }
    if (state.last_email_at && Date.now() - Date.parse(state.last_email_at) < 2 * 60 * 1000) {
      return err("a confirmation code was just sent — wait a couple of minutes", 429);
    }
    if (!env.EMAIL) {
      return err("email sending is not configured", 502);
    }
    // change.initiated lands the moment we accept the request (owner
    // authenticated, has a prior anchor). The dispatches (change.sent,
    // to both new and old addresses) log after their respective
    // env.EMAIL.send succeeds.
    await logCellEvent(cell, accountId, "account.email.change.initiated",
      "control_plane", { new_masked: maskEmail(newEmail) });
    const raw = new Uint32Array(3);
    crypto.getRandomValues(raw);
    const code = [...raw].map((n) => String(n % 1000).padStart(3, "0")).join("-");
    try {
      const cmd = `witself account change-email --new-email ${newEmail} --code ${code}`;
      await env.EMAIL.send({
        to: newEmail,
        from: "no-reply@witwave.ai",
        subject: "Confirm your new Witself account email",
        text: `A request was made to move a Witself account to this address.\n\nAccount: ${accountId}\nCode:    ${code}\n\nIt expires in 15 minutes. Type this code at your terminal:\n\n  ${cmd}\n\nIf you don't recognize this, you can ignore this email.\n`,
        html: renderEmail({
          title: "Confirm this address",
          preheader: "Type this code at your terminal to move the account here.",
          body: `
            <p style="margin:0 0 8px;">A request was made to move a Witself account to this address.</p>
            <p style="margin:0 0 8px;color:${EMAIL_MUTED};font-size:13px;">Account</p>
            <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 20px;">${escapeHTML(accountId)}</div>
            ${codeChip(code)}
            <p style="margin:0 0 8px;">Confirm at your terminal:</p>
            ${cliBlock(cmd)}
            <p style="margin:20px 0 0;color:${EMAIL_MUTED};font-size:14px;">The code expires in 15 minutes. If you don't recognize this request, you can ignore this email.</p>
          `,
        }),
      });
    } catch {
      console.log(`change-email: confirmation send failed for ${accountId} (phase=confirmation_send reason=email_send_error)`);
      return err("could not send the confirmation email — try again shortly", 502);
    }
    await logCellEvent(cell, accountId, "account.email.change.sent",
      "control_plane", { to_masked: maskEmail(newEmail) });
    // Persist the spent quota the moment the code leaves — a send that
    // happened must meter — but hold the code fields back until the
    // counter-move alarm is delivered. A crash or a failed alarm then
    // leaves an unredeemable code (fail-closed), never a change that could
    // commit without the current address ever having been warned.
    state.emails_sent = (state.emails_sent ?? 0) + 1;
    state.last_email_at = new Date().toISOString();
    await env.DIRECTORY.put(key, JSON.stringify(state), { expirationTtl: 24 * 3600 });
    const armCode = async () => {
      state.code_hash = await sha256hex(code.replaceAll("-", ""));
      state.code_expires_at = new Date(Date.now() + 15 * 60 * 1000).toISOString();
      state.new_email = newEmail;
      // Snapshot the address this operation moves AWAY from — the same one
      // the counter-move alarm goes to. The cell's :update-email response
      // and audit trail cannot return the original transition on a replay
      // (the response carries only the new address; audit is masked), so
      // this record is the authoritative source the redeem path uses to
      // rebuild the undo channel after a post-commit crash. It lives in
      // the same record, custody class, and 24h lifetime that already hold
      // new_email in plaintext.
      state.old_email = account.email;
      state.attempts = 0;
      await env.DIRECTORY.put(key, JSON.stringify(state), { expirationTtl: 24 * 3600 });
    };
    // Alarm to the CURRENT address is REQUIRED — it is the only counter-move
    // channel for the stolen-token threat. A send failure must fail the
    // request with the code left unarmed; the caller can retry for a fresh
    // code once outbound mail recovers.
    if (account.email === newEmail) {
      await armCode();
      return json({
        schema_version: "witself.v0",
        account_id: accountId,
        confirmation_email_sent: true,
        notice_sent: false,
        notice_status: "same_address",
      });
    }
    try {
      await env.EMAIL.send({
        to: account.email,
        from: "no-reply@witwave.ai",
        subject: "Security alert: your Witself account email is being changed",
        text: `A request was made to move a Witself account away from this address.\n\nAccount: ${accountId}\nMoving to: ${newEmail}\n\nIf this was you — nothing to do. Confirm from the new inbox to complete the change.\n\nIf this was NOT you, treat it as a compromise: someone else holds a working operator token for your account. Run this at your terminal right now — it rotates the owner credentials and stops the change:\n\n  witself account recover\n`,
        html: renderEmail({
          title: "Your account email is being changed",
          preheader: "Not you? Someone holds your credentials — run witself account recover now.",
          body: `
            <p style="margin:0 0 20px;">A request was made to move a Witself account away from this address.</p>
            <table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%;margin:0 0 20px;">
              <tr>
                <td style="padding:12px 16px;background:${EMAIL_BG};border:1px solid ${EMAIL_BORDER};border-radius:6px;">
                  <div style="color:${EMAIL_MUTED};font-size:12px;text-transform:uppercase;letter-spacing:0.06em;margin:0 0 4px;">Account</div>
                  <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 12px;">${escapeHTML(accountId)}</div>
                  <div style="color:${EMAIL_MUTED};font-size:12px;text-transform:uppercase;letter-spacing:0.06em;margin:0 0 4px;">Moving to</div>
                  <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};">${escapeHTML(newEmail)}</div>
                </td>
              </tr>
            </table>
            <p style="margin:0 0 16px;"><strong>If this was you</strong> — nothing to do. Confirm from the new inbox to complete the change.</p>
            <p style="margin:0 0 8px;"><strong>If this was <span style="color:#b91c1c;">not</span> you, treat it as a compromise</strong> — someone else holds a working operator token for your account. Run this at your terminal right now; it rotates the owner credentials and stops the change:</p>
            ${cliBlock("witself account recover")}
          `,
        }),
      });
    } catch {
      console.log(`change-email: alarm send failed for ${accountId} (phase=alarm_send reason=email_send_error)`);
      // The code was never armed, so it cannot commit the change; the
      // quota stays spent because the code send genuinely happened.
      return err("could not deliver the alarm to your current address — try again shortly", 502);
    }
    await logCellEvent(cell, accountId, "account.email.change.sent",
      "control_plane", { to_masked: maskEmail(account.email) });
    await armCode();
    return json({
      schema_version: "witself.v0",
      account_id: accountId,
      confirmation_email_sent: true,
      notice_sent: true,
    });
  }

  // ---- Redeem mode: verify the code, then the cell commits. ----
  if (!state.code_hash || !state.code_expires_at) {
    return err("invalid or expired confirmation code", 401);
  }
  if ((state.attempts ?? 0) >= 5) {
    return err("too many attempts — request a new code", 429);
  }
  state.attempts = (state.attempts ?? 0) + 1;
  await env.DIRECTORY.put(key, JSON.stringify(state), { expirationTtl: 24 * 3600 });
  const presented = await sha256hex(String(body.code).replace(/[^0-9]/g, ""));
  if (
    presented !== state.code_hash ||
    Date.parse(state.code_expires_at) < Date.now() ||
    state.new_email !== newEmail
  ) {
    return err("invalid or expired confirmation code", 401);
  }
  // A correct code ends the guessing game the attempt cap exists for.
  // Reset the budget durably so post-commit crash replays — which
  // re-present the same correct code — can never exhaust it and strand a
  // committed change without its undo channel.
  if ((state.attempts ?? 0) !== 0) {
    state.attempts = 0;
    await env.DIRECTORY.put(key, JSON.stringify(state), { expirationTtl: 24 * 3600 });
  }

  // The committed transition's old address comes from this operation's own
  // armed snapshot: the cell's :update-email response carries only the new
  // address and its audit trail is masked, so after a post-commit crash the
  // snapshot is the only authoritative source that can rebuild the undo
  // channel toward the ORIGINAL old inbox. States armed before the snapshot
  // existed fall back to the live read, preserving their old semantics.
  const oldEmail = state.old_email ?? account.email;
  // A replay after a committed-but-lost response sees the account already
  // at the new address; the cell mutation is skipped and only the undo
  // channel is (re)built. Anything else that moved the address is an
  // independent change this stale operation must never clobber or revert.
  const alreadyCommitted =
    state.old_email !== undefined &&
    state.old_email !== newEmail &&
    account.email === newEmail;
  if (!alreadyCommitted) {
    // The cell enforces this armed-snapshot CAS atomically inside the same
    // transaction as the update. This live-read check is only a cheap early-out.
    if (state.old_email !== undefined && account.email !== state.old_email) {
      return err("the account's email changed while this request was pending — request a new code", 409);
    }
    let resp;
    try {
      resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:update-email`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${cell.provision_token}`,
        },
        body: JSON.stringify({
          operator_id: operatorID,
          expected_current: oldEmail,
          new_email: newEmail,
        }),
        signal: AbortSignal.timeout(15000),
      });
    } catch {
      return err(`cell ${entry.cell} unreachable — try again shortly`, 502);
    }
    let committed = null;
    try {
      committed = await resp.json();
    } catch {
      committed = null;
    }
    if (!resp.ok || committed?.email !== newEmail) {
      if (resp.status === 403 && committed?.error) {
        return err("only the account owner can change the email", 403);
      }
      return err("email change failed — try again shortly", resp.status === 409 ? 409 : 502);
    }
  }
  // The anchor moved: any live recovery code was mailed to the OLD address,
  // which may now be compromised. Kill it. Rate-limit counters do NOT need
  // to survive an anchor move.
  await env.DIRECTORY.delete(`recover:${accountId}`);
  if (oldEmail === newEmail) {
    // A same-address operation (or the replay of a legacy pre-snapshot
    // state) has no transition to revert: never mint a degenerate undo
    // record whose old and new addresses are equal.
    await env.DIRECTORY.delete(key); // the code is spent
    return json({
      schema_version: "witself.v0",
      account_id: accountId,
      email: newEmail,
    });
  }
  // Undo window: a link in the OLD inbox re-points the email for 48h,
  // matched by hash — the raw token is only ever known to the recipient.
  // The state's undo_key fences minting: a crashed earlier attempt's
  // authority is garbage-collected before a fresh token is issued, and the
  // fence is durable before the record it names exists. The state is
  // re-read first so a concurrent duplicate redeem that already finished
  // (terminal delete) or a superseding request that replaced the operation
  // stops minting here instead of resurrecting spent state. KV has no
  // compare-and-swap, so this narrows rather than eliminates the duplicate
  // window (the same accepted pattern as the invite counter); tokens are
  // only ever delivered to the rightful old inbox and the cell's
  // expected_current guard bounds any residue.
  const fresh = await env.DIRECTORY.get(key, { type: "json" });
  if (!fresh || fresh.code_hash !== state.code_hash) {
    return json({
      schema_version: "witself.v0",
      account_id: accountId,
      email: newEmail,
    });
  }
  if (fresh.undo_key) {
    await env.DIRECTORY.delete(`undoemail:${fresh.undo_key}`);
  }
  const undoRaw = new Uint8Array(32);
  crypto.getRandomValues(undoRaw);
  const undoTok = [...undoRaw].map((b) => b.toString(16).padStart(2, "0")).join("");
  const undoKey = await sha256hex(undoTok);
  const undoTtl = 48 * 3600;
  fresh.undo_key = undoKey;
  await env.DIRECTORY.put(key, JSON.stringify(fresh), { expirationTtl: 24 * 3600 });
  await env.DIRECTORY.put(
    `undoemail:${undoKey}`,
    JSON.stringify({
      account_id: accountId,
      cell: entry.cell,
      old_email: oldEmail,
      new_email: newEmail,
      expires_at: new Date(Date.now() + undoTtl * 1000).toISOString(),
    }),
    { expirationTtl: undoTtl },
  );
  const undoLink = `${new URL(request.url).origin}/undo-email/${undoTok}`;
  try {
    await env.EMAIL.send({
      to: oldEmail,
      from: "no-reply@witwave.ai",
      subject: "Your Witself account email was changed",
      text: `This is a confirmation: a Witself account has moved away from this address.\n\nAccount: ${accountId}\nNow at:  ${newEmail}\n\nIf the change was valid, no action is needed — this is the last email this address will receive for the account.\n\nIf the change was NOT valid, you can revert it. This link points the account back at this address and stays live for 48 hours:\n\n  ${undoLink}\n\nAfter reverting, run \`witself account recover\` at your terminal to rotate the owner credentials.\n`,
      html: renderEmail({
        title: "Your account email was changed",
        preheader: "If the change wasn't valid, you can revert it within 48 hours.",
        body: `
          <p style="margin:0 0 20px;">This is a confirmation: a Witself account has moved away from this address.</p>
          <table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%;margin:0 0 20px;">
            <tr>
              <td style="padding:12px 16px;background:${EMAIL_BG};border:1px solid ${EMAIL_BORDER};border-radius:6px;">
                <div style="color:${EMAIL_MUTED};font-size:12px;text-transform:uppercase;letter-spacing:0.06em;margin:0 0 4px;">Account</div>
                <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 12px;">${escapeHTML(accountId)}</div>
                <div style="color:${EMAIL_MUTED};font-size:12px;text-transform:uppercase;letter-spacing:0.06em;margin:0 0 4px;">Now at</div>
                <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};">${escapeHTML(newEmail)}</div>
              </td>
            </tr>
          </table>
          <p style="margin:0 0 16px;">If the change was valid, no action is needed — this is the last email this address will receive for the account.</p>
          <p style="margin:0 0 8px;">If the change was <strong>not</strong> valid, you can revert it — this points the account back at this address and stays live for <strong>48 hours</strong>:</p>
          ${ctaButton({ href: undoLink, label: "Revert the change" })}
          <p style="margin:20px 0 0;color:${EMAIL_MUTED};font-size:14px;">After reverting, run <code style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;">witself account recover</code> at your terminal to rotate the owner credentials.</p>
        `,
      }),
    });
    await logCellEvent(cell, accountId, "account.email.undo.sent",
      "control_plane", { to_masked: maskEmail(oldEmail) });
  } catch {
    console.log(`change-email: undo notice send failed for ${accountId} (phase=undo_notice_send reason=email_send_error)`);
  }
  await env.DIRECTORY.delete(key); // the code is spent
  return json({
    schema_version: "witself.v0",
    account_id: accountId,
    email: newEmail,
  });
}

// handleUndoEmail is the human half of the undo window: GET /undo-email/<tok>
// re-points the account back to the OLD address. Possession of the token IS
// the authorization — it was only ever delivered to the old inbox — so the
// worker calls the cell's undo variant of :update-email under the provision
// token; the cell checks that the current email still matches the snapshot
// so a stale link can't roll back a subsequent legitimate change.
async function handleUndoEmail(env, token) {
  const key = `undoemail:${await sha256hex(token)}`;
  const undo = await env.DIRECTORY.get(key, { type: "json" });
  if (!undo?.account_id || !undo?.old_email || !ACCOUNT_ID.test(undo.account_id)) {
    return htmlPage(404, "Undo link invalid or expired", "This undo link is invalid or has already been used.");
  }
  const cell = await env.DIRECTORY.get(`cell:${undo.cell}`, { type: "json" });
  if (!cell?.provision_token || !cell?.endpoint) {
    return htmlPage(503, "Temporarily unavailable", "We couldn't reach your account's home just now. Please try the link again in a few minutes.");
  }
  let resp;
  try {
    resp = await fetch(`${cell.endpoint}/v1/accounts/${undo.account_id}:update-email`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({
        undo: true,
        expected_current: undo.new_email,
        new_email: undo.old_email,
      }),
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return htmlPage(503, "Temporarily unavailable", "The revert couldn't be applied just now. Please try the link again in a few minutes.");
  }
  const committed = await resp.json().catch(() => null);
  if (resp.status === 409) {
    return htmlPage(409, "Undo link is stale", "The account's email has changed again since this link was issued. If you didn't authorize either change, use <code>witself account recover</code> to rotate the owner credentials.");
  }
  if (!resp.ok || committed?.email !== undo.old_email) {
    return htmlPage(503, "Temporarily unavailable", "The revert couldn't be applied just now. Please try the link again in a few minutes.");
  }
  await env.DIRECTORY.delete(key);
  await env.DIRECTORY.delete(`recover:${undo.account_id}`);
  // Burn any still-armed change operation too: after a post-commit crash
  // the confirmation code stays live, and without this delete a stolen
  // code could pass the redeem path's old-address guard again (the revert
  // restored exactly that address) and silently re-commit the change the
  // owner just reverted.
  await env.DIRECTORY.delete(`emailchange:${undo.account_id}`);
  return htmlPage(200, "Email change reverted", `The account's email is back to <code>${undo.old_email}</code>. Run <code>witself account recover</code> from your terminal now to rotate the owner credentials.`);
}

// handleClose is POST /v1/accounts/{id}:close — the symmetric exit to signup's
// entrance. Close shares the per-account Durable Object with evacuation and
// restore, so an owner close cannot commit while an older snapshot is being
// exported and later resurrect that pre-close state.
async function handleClose(request, env, accountId) {
  const auth = request.headers.get("Authorization");
  if (!auth) {
    return err("operator token required", 401);
  }
  let body = "{}";
  try {
    body = await request.text();
  } catch {
    // keep the empty default
  }
  return accountLifecycleStub(env, accountId).fetch(
    new Request("https://account-lifecycle.internal/run", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        action: "close",
        account_id: accountId,
        authorization: auth,
        body: body || "{}",
      }),
    }),
  );
}

// handleReaper is the fleet-wide pending-account expiry policy: GET returns
// it, POST sets it ({enabled, ttl_minutes}). Accounts pending longer than the
// TTL are closed by the scheduled sweep. Disabled until configured — which
// also makes rollout ordering safe (cells must serve :reap before the sweep
// may run).
async function handleReaper(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (request.method === "GET") {
    const cfg = (await env.DIRECTORY.get("config:reaper", { type: "json" })) ?? {
      enabled: false,
    };
    return json({ schema_version: "witself.v0", reaper: cfg });
  }
  if (request.method === "POST") {
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid JSON body", 400);
    }
    if (typeof body.enabled !== "boolean") {
      return err("enabled must be a boolean", 400);
    }
    if (body.enabled && (typeof body.ttl_minutes !== "number" || !Number.isFinite(body.ttl_minutes) || body.ttl_minutes < 1)) {
      return err("ttl_minutes must be a number >= 1 when enabled", 400);
    }
    const cfg = { enabled: body.enabled };
    if (cfg.enabled) {
      cfg.ttl_minutes = body.ttl_minutes;
    }
    await env.DIRECTORY.put("config:reaper", JSON.stringify(cfg));
    return json({ schema_version: "witself.v0", reaper: cfg });
  }
  return err("method not allowed", 405);
}

// reapExpiredPendings is the signup-lifecycle sweep: accounts that never
// activated within the configured window are closed on their cell and
// forgotten here. The pending: keys are only a candidate list — the cell's
// only-if-pending :reap is the authority, so a stale candidate (the account
// activated moments ago) bounces with 409 and is simply dropped. Every arm
// is idempotent; anything unreachable is retried on the next cron tick.
async function reapExpiredPendings(env) {
  const cfg = (await env.DIRECTORY.get("config:reaper", { type: "json" })) ?? {};
  if (cfg.enabled !== true || !(cfg.ttl_minutes > 0)) {
    return;
  }
  const cutoff = Date.now() - cfg.ttl_minutes * 60 * 1000;

  const cells = new Map(); // cell name -> registry entry (cached per sweep)
  const deadCells = new Set(); // unreachable this sweep — skip their candidates
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "pending:", cursor });
    for (const k of page.keys) {
      const accountId = k.name.slice("pending:".length);
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!entry) {
        continue;
      }
      const createdAt = Date.parse(entry.created_at);
      if (Number.isNaN(createdAt)) {
        // Never written by this Worker — surface it, don't guess.
        console.log(`reaper: ${k.name} has unparseable created_at ${JSON.stringify(entry.created_at)}; skipping`);
        continue;
      }
      if (createdAt >= cutoff) {
        continue;
      }
      if (deadCells.has(entry.cell)) {
        continue; // one 15s timeout per dead cell per sweep, not per candidate
      }
      if (!cells.has(entry.cell)) {
        cells.set(
          entry.cell,
          await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" }),
        );
      }
      const cell = cells.get(entry.cell);
      if (!cell?.provision_token || !cell?.endpoint) {
        console.log(`reaper: cell ${entry.cell} missing or has no provision token; skipping ${accountId}`);
        continue;
      }
      let resp;
      try {
        resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:reap`, {
          method: "POST",
          headers: { Authorization: `Bearer ${cell.provision_token}` },
          signal: AbortSignal.timeout(15000),
        });
      } catch {
        deadCells.add(entry.cell);
        continue; // cell unreachable — next tick retries
      }
      let body = null;
      try {
        body = await resp.json();
      } catch {
        // Not JSON — not an answer from a witself-server handler.
      }
      if (resp.ok) {
        // Only a genuine reap acknowledgement may make the fleet forget the
        // account: a stray 200 (LB default page, captive portal, wrong
        // service on the endpoint) must never destroy live routing.
        if (body?.status === "closed" && body?.account_id === accountId) {
          const registrationID =
            cell.registration_id ?? cell.registered_at ?? null;
          const sourceEpoch =
            Number.isSafeInteger(entry.route_epoch) &&
                entry.route_epoch >= 0
              ? entry.route_epoch
              : 0;
          const currentRoute = await env.DIRECTORY.get(
            `acct:${accountId}`,
            { type: "json" },
          );
          const currentRouteEpoch =
            Number.isSafeInteger(currentRoute?.epoch) &&
                currentRoute.epoch >= 0
              ? currentRoute.epoch
              : 0;
          if (
            currentRoute &&
            (
              currentRoute.cell !== entry.cell ||
              currentRouteEpoch !== sourceEpoch ||
              (
                currentRoute.cell_registration_id &&
                registrationID &&
                currentRoute.cell_registration_id !== registrationID
              )
            )
          ) {
            await env.DIRECTORY.delete(k.name);
            console.log(
              `reaper: ${accountId} has newer route authority; stale candidate dropped`,
            );
            continue;
          }
          if (registrationID) {
            const departed = await requestCellCoordinator(
              env,
              entry.cell,
              "/depart",
              {
                account_id: accountId,
                operation_id: crypto.randomUUID(),
                registration_id: registrationID,
                source_epoch: sourceEpoch,
              },
            );
            if (!departed.ok) {
              console.log(
                `reaper: ${accountId} closed but occupancy handoff failed; retrying next tick`,
              );
              continue;
            }
          }
          const routeAfterDeparture = await env.DIRECTORY.get(
            `acct:${accountId}`,
            { type: "json" },
          );
          const routeAfterEpoch =
            Number.isSafeInteger(routeAfterDeparture?.epoch) &&
                routeAfterDeparture.epoch >= 0
              ? routeAfterDeparture.epoch
              : 0;
          if (
            routeAfterDeparture &&
            (
              routeAfterDeparture.cell !== entry.cell ||
              routeAfterEpoch !== sourceEpoch ||
              (
                routeAfterDeparture.cell_registration_id &&
                registrationID &&
                routeAfterDeparture.cell_registration_id !== registrationID
              )
            )
          ) {
            await env.DIRECTORY.delete(k.name);
            console.log(
              `reaper: ${accountId} route changed during reap; stale candidate dropped`,
            );
            continue;
          }
          await env.DIRECTORY.delete(`acct:${accountId}`);
          await env.DIRECTORY.delete(k.name);
          console.log(`reaper: closed ${accountId} on ${entry.cell}`);
        } else {
          console.log(`reaper: ${accountId} on ${entry.cell}: 2xx without a reap acknowledgement; retrying next tick`);
        }
      } else if (resp.status === 409 && body?.error) {
        // Activated first — drop the candidate, keep the routing.
        await env.DIRECTORY.delete(k.name);
        console.log(`reaper: ${accountId} activated before expiry; candidate dropped`);
      } else if (resp.status === 404 && body?.error === "account not found") {
        // The cell's handler answered "no such account" — its EXACT string.
        // A bare mux 404 (cell too old for :reap) or the dispatcher's
        // unknown-action 404 stays retryable, so enabling the reaper before
        // a cell rolls loses nothing.
        await env.DIRECTORY.delete(k.name);
        console.log(`reaper: ${accountId} unknown on ${entry.cell} (404); candidate dropped`);
      } else {
        console.log(`reaper: reap ${accountId} on ${entry.cell} failed: ${resp.status}`);
      }
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
}

// handleEvacuate is POST /v1/cells/{name}:evacuate — the polite counterpart
// to :purge. Bounded per-call (a small batch of accounts) so the Worker
// respects request duration limits; witself-infra loops until {remaining:0}.
// Idempotent by construction: each account's four steps (system-suspend,
// stream-to-R2, write archived: entry, delete acct: pointer) are all
// individually re-safe, and a partially-evacuated account resumes on the
// next call. Refuses to evacuate an accepting cell (drain first): the same
// safety rule as :delete.
async function handleEvacuate(request, env, cellName) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (!env.ARCHIVES) {
    return err("R2 bucket not bound — witwave-archives is not configured", 501);
  }
  const cell = await env.DIRECTORY.get(`cell:${cellName}`, { type: "json" });
  if (!cell) {
    return err("unknown cell", 404);
  }
  if (cell.accepting !== false) {
    return err("cell must be drained first (re-register with accepting=false)", 409);
  }
  if (!cell.provision_token || !cell.endpoint) {
    return err(`cell ${cellName} has no provision credential — cannot export`, 502);
  }

  let body = {};
  try {
    body = await request.json();
  } catch {
    // batch size defaults are fine
  }
  // Coerce carefully — Number(undefined) is NaN, and NaN slips past
  // Math.min/Math.max to become the loop limit. Anything non-finite falls
  // back to the default, then we clamp.
  let batch = Number(body.batch);
  if (!Number.isFinite(batch) || batch < 1) {
    batch = 4;
  }
  batch = Math.min(Math.floor(batch), 10);

  // Iterate acct: entries pointing at this cell.
  const targets = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.cell === cellName) {
        targets.push(k.name.slice("acct:".length));
        if (targets.length >= batch) {
          break;
        }
      }
    }
    if (targets.length >= batch) {
      break;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);

  // Track cross-batch progress. This entry is best-effort; the true state of
  // the world is the acct:/archived: pairs.
  const progressKey = `evac:${cellName}`;
  const progress = (await env.DIRECTORY.get(progressKey, { type: "json" })) ?? {
    started_at: new Date().toISOString(),
    done: 0,
    failed: [],
  };

  const results = [];
  for (const accountId of targets) {
    try {
      const outcome = (await evacuateAccount(env, cellName, cell, accountId)) ?? {};
      progress.done += 1;
      results.push({
        account_id: accountId,
        ok: true,
        ...(outcome.reaped ? { reaped: true } : {}),
      });
    } catch (e) {
      const msg = String(e?.message ?? e);
      progress.failed = [
        ...(progress.failed ?? []).filter((f) => f.account_id !== accountId),
        { account_id: accountId, error: msg, at: new Date().toISOString() },
      ];
      results.push({ account_id: accountId, ok: false, error: msg });
      console.log(`evacuate ${cellName}/${accountId} failed: ${msg}`);
    }
  }

  // How many remain? A remaining=0 lets witself-infra move to the
  // deregister step (which still refuses via the existing zero-accounts
  // guard until every acct: pointer is gone).
  let remaining = 0;
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor: cursor2 });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.cell === cellName) {
        remaining += 1;
      }
    }
    cursor2 = page.list_complete ? undefined : page.cursor;
  } while (cursor2);

  progress.remaining = remaining;
  if (remaining === 0) {
    progress.finished_at = new Date().toISOString();
  }
  await env.DIRECTORY.put(progressKey, JSON.stringify(progress));

  return json({
    schema_version: "witself.v0",
    cell: cellName,
    evacuated: results,
    remaining,
    progress,
  });
}

// logCellEvent records a control-plane-originated event on the tenant's
// cell via the provision-token :events endpoint. Best-effort: an audit
// failure NEVER aborts the caller's flow. A dropped event is worse than
// a failed operation only when the operation would otherwise succeed;
// the operation succeeding without an audit entry is what we'd rather
// have than a spurious operation failure because the audit couldn't be
// written. Logs to the Cloudflare Worker console on error so operators
// can spot systemic drift.
async function logCellEvent(cell, accountId, verb, actorKind, metadata) {
  if (!cell?.endpoint || !cell?.provision_token) {
    console.log(`event ${verb} for ${accountId}: no cell endpoint available`);
    return;
  }
  try {
    const resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:events`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({
        verb,
        actor_kind: actorKind,
        metadata: metadata ?? {},
      }),
      signal: AbortSignal.timeout(10000),
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      console.log(`event ${verb} for ${accountId}: HTTP ${resp.status}: ${text.slice(0, 200)}`);
    }
  } catch (e) {
    console.log(`event ${verb} for ${accountId}: ${String(e?.message ?? e)}`);
  }
}

// cellForAccount is a small helper used by handlers that already have an
// accountId but not the cell record. Looks up acct: to find the cell name,
// then reads the cell: entry to get endpoint + provision_token. Returns
// null when the account is unrouted (evacuated / never provisioned).
async function cellForAccount(env, accountId) {
  const entry = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (!entry?.cell) return null;
  const cell = await env.DIRECTORY.get(`cell:${entry.cell}`, { type: "json" });
  return cell ?? null;
}

async function fetchPlacementPolicySnapshot(cell, accountId) {
  try {
    const resp = await fetch(
      `${cell.endpoint}/v1/accounts/${accountId}/placement-policy`,
      {
        method: "GET",
        headers: { Authorization: `Bearer ${cell.provision_token}` },
        signal: AbortSignal.timeout(15000),
      },
    );
    if (!resp.ok) {
      await resp.text().catch(() => "");
      return null;
    }
    const body = await resp.json();
    return body?.placement_policy ?? null;
  } catch {
    return null;
  }
}

async function fetchPlacementAccountContact(cell, accountId) {
  try {
    const resp = await fetch(
      `${cell.endpoint}/v1/accounts/${accountId}:contact`,
      {
        method: "POST",
        headers: { Authorization: `Bearer ${cell.provision_token}` },
        signal: AbortSignal.timeout(15000),
      },
    );
    if (!resp.ok) {
      const text = await resp.text().catch(() => "");
      return { error: `contact ${resp.status}: ${text.slice(0, 200)}` };
    }
    return await resp.json();
  } catch (e) {
    return { error: String(e?.message ?? e) };
  }
}

async function accountCountsByCell(env) {
  const counts = new Map();
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.cell) {
        counts.set(entry.cell, (counts.get(entry.cell) ?? 0) + 1);
      }
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return counts;
}

// maskEmail turns "scott@witwave.ai" into "s***@w***.ai" for audit
// metadata. Mirrors the cell-side MaskEmail exactly (internal/store/
// events.go) so the same shape lands on both sides of the trust link.
function maskEmail(addr) {
  const s = (addr ?? "").trim();
  if (!s) return "";
  const at = s.lastIndexOf("@");
  if (at <= 0 || at === s.length - 1) return "***";
  const local = s.slice(0, at);
  const domain = s.slice(at + 1);
  const dot = domain.lastIndexOf(".");
  const domainMasked = dot > 0
    ? domain[0] + "***" + domain.slice(dot)
    : domain[0] + "***";
  return local[0] + "***@" + domainMasked;
}

function accountLifecycleStub(env, accountId) {
  if (!env.ACCOUNT_LIFECYCLE) {
    throw new Error("account lifecycle Durable Object is not configured");
  }
  const id = env.ACCOUNT_LIFECYCLE.idFromName(accountId);
  return env.ACCOUNT_LIFECYCLE.get(id);
}

function cellCoordinatorStub(env, cellName) {
  if (!env.CELL_COORDINATOR) {
    throw new Error("target cell coordinator Durable Object is not configured");
  }
  const id = env.CELL_COORDINATOR.idFromName(cellName);
  return env.CELL_COORDINATOR.get(id);
}

async function requestCellCoordinator(env, cellName, path, payload) {
  try {
    return await cellCoordinatorStub(env, cellName).fetch(
      new Request(`https://target-cell.internal${path}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          cell_name: cellName,
          ...payload,
        }),
      }),
    );
  } catch (error) {
    return err(
      `target cell coordinator unavailable: ${String(error?.message ?? error)}`,
      503,
    );
  }
}

async function runAccountLifecycle(env, accountId, input) {
  const response = await accountLifecycleStub(env, accountId).fetch(
    new Request("https://account-lifecycle.internal/run", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ account_id: accountId, ...input }),
    }),
  );
  const body = await response.json().catch(() => null);
  if (!response.ok || body?.ok !== true) {
    throw new Error(
      body?.error ??
        `account lifecycle coordinator returned HTTP ${response.status}`,
    );
  }
  return body.result ?? {};
}

async function evacuateAccount(env, cellName, _cell, accountId, opts = {}) {
  return runAccountLifecycle(env, accountId, {
    action: "evacuate",
    cell_name: cellName,
    allow_pending_reap: opts.allowPendingReap !== false,
    reason: opts.reason,
  });
}

async function restoreAccount(
  env,
  cellName,
  _cell,
  accountId,
  archived,
) {
  return runAccountLifecycle(env, accountId, {
    action: "restore",
    cell_name: cellName,
    archive_object: archived?.object,
    archive_id: archived?.archive_id,
    expected_epoch: archived?.epoch,
  });
}

async function moveAccount(
  env,
  accountId,
  sourceCellName,
  targetCellName,
  route,
  opts = {},
) {
  return runAccountLifecycle(env, accountId, {
    action: "move",
    source_cell: sourceCellName,
    target_cell: targetCellName,
    expected_epoch: route?.epoch,
    allow_pending_reap: false,
    reason: opts.reason ?? "placement rebalance",
  });
}

// A closed archive is durable retention, not placement work. It intentionally
// remains under archived: so account discovery and later cell teardown cannot
// hide the only portable artifact, but automated restore loops must not import
// it repeatedly.
function isRestorableArchive(archived) {
  return Boolean(archived) && archived.status !== "closed";
}

// handleRestore is POST /v1/cells/{name}:restore — the mirror of :evacuate.
// Iterates archived: pointers eligible for the target cell. Policy-aware
// archives must satisfy hard allowed_* pins; older archives fall back to
// region_code/region matching unless all_regions explicitly bypasses that
// legacy guard. For a bounded batch, streams the R2 object into the cell's
// :import, calls :resume, writes the new acct: pointer, then deletes both the
// archived: KV entry and the R2 object. Each of the four steps is individually
// re-safe (ready-check-then-act, pointer-carried immutable object key), so a
// partial restore resumes cleanly on the next call. Refuses to restore into a
// drained cell
// (accepting=false): dumping accounts onto a cell that will not accept them
// would just move the "awaiting placement" state to a place harder to see.
async function handleRestore(request, env, cellName) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (!env.ARCHIVES) {
    return err("R2 bucket not bound — witwave-archives is not configured", 501);
  }
  const cell = await env.DIRECTORY.get(`cell:${cellName}`, { type: "json" });
  if (!cell) {
    return err("unknown cell", 404);
  }
  if (cell.backup_validation_target === true) {
    return err(
      "cell is reserved for backup validation — cannot restore into it",
      409,
    );
  }
  if (cell.accepting === false) {
    return err("cell is drained (accepting=false) — cannot restore into it", 409);
  }
  const backupsEnabled = accountBackupSchedulingEnabled(env);
  if (!cellHasDestinationCredentials(cell, { backupsEnabled }) || !cell.endpoint) {
    return err(
      backupsEnabled
        ? `cell ${cellName} has no distinct provision and backup credentials — cannot import while account backups are enabled`
        : `cell ${cellName} has no provision credential — cannot import`,
      502,
    );
  }
  if (!cell.region) {
    return err(`cell ${cellName} has no region — cannot match archived accounts`, 502);
  }

  let body = {};
  try {
    body = await request.json();
  } catch {
    // batch size defaults are fine
  }
  let batch = Number(body.batch);
  if (!Number.isFinite(batch) || batch < 1) {
    batch = 4;
  }
  batch = Math.min(Math.floor(batch), 10);
  const allRegions = body.all_regions === true;
  const restoreScope = allRegions ? "all" : (cell.region_code || cell.region);

  // Iterate archived: pointers. Policy-aware archives use hard allowed_* pins;
  // pre-policy archives keep the old region guard. all_regions remains the
  // explicit operator escape hatch for legacy cross-region tests.
  const targets = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (
        isRestorableArchive(entry) &&
        cellMatchesArchivedPlacement(cell, entry, allRegions)
      ) {
        targets.push({ accountId: k.name.slice("archived:".length), archived: entry });
        if (targets.length >= batch) {
          break;
        }
      }
    }
    if (targets.length >= batch) {
      break;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);

  const progressKey = allRegions ? `restore:${cellName}:all-regions` : `restore:${cellName}`;
  const progress = (await env.DIRECTORY.get(progressKey, { type: "json" })) ?? {
    started_at: new Date().toISOString(),
    done: 0,
    failed: [],
  };

  const results = [];
  for (const { accountId, archived } of targets) {
    try {
      await restoreAccount(env, cellName, cell, accountId, archived);
      progress.done += 1;
      results.push({ account_id: accountId, ok: true });
    } catch (e) {
      const msg = String(e?.message ?? e);
      progress.failed = [
        ...(progress.failed ?? []).filter((f) => f.account_id !== accountId),
        { account_id: accountId, error: msg, at: new Date().toISOString() },
      ];
      results.push({ account_id: accountId, ok: false, error: msg });
      console.log(`restore ${cellName}/${accountId} failed: ${msg}`);
    }
  }

  // Count archived: pointers still awaiting placement in this restore scope.
  // witself-infra loops until this reaches zero. KV list/get can briefly lag
  // deletes, so do not count accounts this very call restored successfully.
  // Those accounts are already routed to the target cell; treating stale
  // archived: keys as remaining makes the CLI report a false stalled restore.
  const restoredOK = new Set(
    results.filter((r) => r.ok).map((r) => r.account_id),
  );
  let remaining = 0;
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor: cursor2 });
    for (const k of page.keys) {
      const accountId = k.name.slice("archived:".length);
      if (restoredOK.has(accountId)) {
        continue;
      }
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (
        isRestorableArchive(entry) &&
        cellMatchesArchivedPlacement(cell, entry, allRegions)
      ) {
        remaining += 1;
      }
    }
    cursor2 = page.list_complete ? undefined : page.cursor;
  } while (cursor2);

  progress.remaining = remaining;
  if (remaining === 0) {
    progress.finished_at = new Date().toISOString();
  } else {
    delete progress.finished_at;
  }
  await env.DIRECTORY.put(progressKey, JSON.stringify(progress));

  return json({
    schema_version: "witself.v0",
    cell: cellName,
    region: restoreScope,
    restored: results,
    remaining,
    progress,
  });
}

async function handlePlacementRestore(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (!env.ARCHIVES) {
    return err("R2 bucket not bound — witwave-archives is not configured", 501);
  }
  let body = {};
  try {
    body = await request.json();
  } catch {
    // defaults are fine
  }
  let batch = Number(body.batch);
  if (!Number.isFinite(batch) || batch < 1) {
    batch = 4;
  }
  batch = Math.min(Math.floor(batch), 10);
  const allRegions = body.all_regions === true;

  const backupsEnabled = accountBackupSchedulingEnabled(env);
  const destinationOptions = { backupsEnabled };
  const cells = (await listCells(env)).filter(
    (cell) => cellIsEligibleDestination(cell, destinationOptions),
  );
  const counts = await accountCountsByCell(env);
  const selectionCounts = new Map(counts);

  const targets = [];
  const blocked = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
    for (const k of page.keys) {
      const accountId = k.name.slice("archived:".length);
      const archived = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!archived) {
        continue;
      }
      if (!isRestorableArchive(archived)) {
        continue;
      }
      const cell = bestPlacementCell(
        cells,
        archived,
        selectionCounts,
        allRegions,
        destinationOptions,
      );
      if (!cell) {
        blocked.push({ account_id: accountId, reason: "no eligible accepting cell" });
        continue;
      }
      selectionCounts.set(cell.name, (selectionCounts.get(cell.name) ?? 0) + 1);
      targets.push({ accountId, archived, cell });
      if (targets.length >= batch) {
        break;
      }
    }
    if (targets.length >= batch) {
      break;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);

  const results = [];
  for (const { accountId, archived, cell } of targets) {
    try {
      await restoreAccount(env, cell.name, cell, accountId, archived);
      counts.set(cell.name, (counts.get(cell.name) ?? 0) + 1);
      results.push({ account_id: accountId, ok: true, cell: cell.name });
    } catch (e) {
      const msg = String(e?.message ?? e);
      results.push({ account_id: accountId, ok: false, cell: cell.name, error: msg });
      console.log(`placement restore ${cell.name}/${accountId} failed: ${msg}`);
    }
  }

  const restoredOK = new Set(
    results.filter((r) => r.ok).map((r) => r.account_id),
  );
  const blockedAfter = [];
  let remaining = 0;
  let unplaced = 0;
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor: cursor2 });
    for (const k of page.keys) {
      const accountId = k.name.slice("archived:".length);
      if (restoredOK.has(accountId)) {
        continue;
      }
      const archived = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!archived) {
        continue;
      }
      if (!isRestorableArchive(archived)) {
        continue;
      }
      const cell = bestPlacementCell(
        cells,
        archived,
        counts,
        allRegions,
        destinationOptions,
      );
      if (cell) {
        remaining += 1;
      } else {
        unplaced += 1;
        blockedAfter.push({ account_id: accountId, reason: "no eligible accepting cell" });
      }
    }
    cursor2 = page.list_complete ? undefined : page.cursor;
  } while (cursor2);

  return json({
    schema_version: "witself.v0",
    restored: results,
    blocked: blockedAfter.length > 0 ? blockedAfter : blocked,
    remaining,
    unplaced,
  });
}

async function rebalanceTargetForAccount(
  env,
  cellsByName,
  destinationCells,
  counts,
  accountId,
  route,
  destinationOptions,
) {
  const current = cellsByName.get(route?.cell || "");
  if (!current) {
    return { skip: { account_id: accountId, reason: `current cell ${route?.cell || ""} is not registered` } };
  }
  if (!current.endpoint || !current.provision_token) {
    return { skip: { account_id: accountId, cell: current.name, reason: "current cell has no provision endpoint" } };
  }
  const contact = await fetchPlacementAccountContact(current, accountId);
  if (contact.error) {
    return { skip: { account_id: accountId, cell: current.name, reason: contact.error } };
  }
  if (contact.status !== "active" && contact.status !== "suspended") {
    return { skip: { account_id: accountId, cell: current.name, reason: `status ${contact.status || "unknown"} is not eligible` } };
  }
  const policy = await fetchPlacementPolicySnapshot(current, accountId);
  if (!policy) {
    return { skip: { account_id: accountId, cell: current.name, reason: "could not read placement policy" } };
  }
  if (
    !cellMatchesPolicy(current, policy) &&
    !bestPolicyCell(destinationCells, policy, counts, destinationOptions)
  ) {
    return { skip: { account_id: accountId, cell: current.name, reason: "no eligible accepting cell for hard pin" } };
  }
  const target = bestRebalanceCell(
    destinationCells,
    current,
    policy,
    counts,
    destinationOptions,
  );
  if (!target || target.cell.name === current.name) {
    return {};
  }
  return {
    accountId,
    current,
    target: target.cell,
    reason: target.reason,
    route,
  };
}

async function handlePlacementRebalance(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (!env.ARCHIVES) {
    return err("R2 bucket not bound — witwave-archives is not configured", 501);
  }
  let body = {};
  try {
    body = await request.json();
  } catch {
    // defaults are fine
  }
  let batch = Number(body.batch);
  if (!Number.isFinite(batch) || batch < 1) {
    batch = 1;
  }
  batch = Math.min(Math.floor(batch), 5);
  const dryRun = body.dry_run === true;

  const backupsEnabled = accountBackupSchedulingEnabled(env);
  const destinationOptions = { backupsEnabled };
  const cells = await listCells(env);
  const cellsByName = new Map(cells.map((cell) => [cell.name, cell]));
  const destinationCells = cells.filter(
    (cell) => cellIsEligibleDestination(cell, destinationOptions),
  );
  const counts = await accountCountsByCell(env);
  const selectionCounts = new Map(counts);
  const targets = [];
  const skipped = [];

  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
    for (const k of page.keys) {
      const accountId = k.name.slice("acct:".length);
      const route = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!route) {
        continue;
      }
      const candidate = await rebalanceTargetForAccount(
        env,
        cellsByName,
        destinationCells,
        selectionCounts,
        accountId,
        route,
        destinationOptions,
      );
      if (candidate.skip) {
        skipped.push(candidate.skip);
        continue;
      }
      if (!candidate.target) {
        continue;
      }
      selectionCounts.set(
        candidate.current.name,
        Math.max(0, (selectionCounts.get(candidate.current.name) ?? 0) - 1),
      );
      selectionCounts.set(
        candidate.target.name,
        (selectionCounts.get(candidate.target.name) ?? 0) + 1,
      );
      targets.push(candidate);
      if (targets.length >= batch) {
        break;
      }
    }
    if (targets.length >= batch) {
      break;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);

  const results = [];
  for (const target of targets) {
    const accountId = target.accountId;
    if (dryRun) {
      results.push({
        account_id: accountId,
        ok: true,
        from_cell: target.current.name,
        to_cell: target.target.name,
        reason: target.reason,
        dry_run: true,
      });
      continue;
    }

    try {
      await moveAccount(
        env,
        accountId,
        target.current.name,
        target.target.name,
        target.route,
        { reason: "placement rebalance" },
      );
      counts.set(target.current.name, Math.max(0, (counts.get(target.current.name) ?? 0) - 1));
      counts.set(target.target.name, (counts.get(target.target.name) ?? 0) + 1);
      results.push({
        account_id: accountId,
        ok: true,
        from_cell: target.current.name,
        to_cell: target.target.name,
        reason: target.reason,
      });
    } catch (e) {
      const msg = String(e?.message ?? e);
      results.push({
        account_id: accountId,
        ok: false,
        from_cell: target.current.name,
        to_cell: target.target.name,
        error: msg,
      });
      console.log(`rebalance ${target.current.name}->${target.target.name}/${accountId} failed: ${msg}`);
    }
  }

  let remaining = 0;
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor: cursor2 });
    for (const k of page.keys) {
      const accountId = k.name.slice("acct:".length);
      const route = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!route) {
        continue;
      }
      const candidate = await rebalanceTargetForAccount(
        env,
        cellsByName,
        destinationCells,
        counts,
        accountId,
        route,
        destinationOptions,
      );
      if (candidate.target) {
        remaining += 1;
      }
    }
    cursor2 = page.list_complete ? undefined : page.cursor;
  } while (cursor2);

  return json({
    schema_version: "witself.v0",
    dry_run: dryRun,
    rebalanced: results,
    skipped: skipped.slice(0, 20),
    remaining,
  });
}

function clampInt(value, fallback, min, max) {
  const n = Number(value);
  if (!Number.isFinite(n)) {
    return fallback;
  }
  return Math.min(Math.max(Math.floor(n), min), max);
}

function normalizePlacementRunnerConfig(raw = {}) {
  return {
    enabled: raw.enabled === true,
    restore_archives: raw.restore_archives !== false,
    restore_batch: clampInt(raw.restore_batch, 4, 1, 10),
    restore_any_region: raw.restore_any_region === true,
    rebalance: raw.rebalance !== false,
    rebalance_batch: clampInt(raw.rebalance_batch, 1, 1, 5),
  };
}

async function placementRunnerConfig(env) {
  const cfg = (await env.DIRECTORY.get("config:placement_runner", { type: "json" })) ?? {};
  return normalizePlacementRunnerConfig(cfg);
}

async function handlePlacementRunner(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (request.method === "GET") {
    return json({
      schema_version: "witself.v0",
      placement_runner: await placementRunnerConfig(env),
    });
  }
  if (request.method !== "POST") {
    return err("method not allowed", 405);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return err("invalid JSON body", 400);
  }
  const current = await placementRunnerConfig(env);
  const next = normalizePlacementRunnerConfig({ ...current, ...body });
  await env.DIRECTORY.put("config:placement_runner", JSON.stringify(next));
  return json({ schema_version: "witself.v0", placement_runner: next });
}

async function callInternalFleetHandler(env, path, body, handler) {
  if (!env.FLEET_TOKEN) {
    return { ok: false, status: 501, body: { error: "FLEET_TOKEN is not configured" } };
  }
  const req = new Request(`https://internal${path}`, {
    method: "POST",
    headers: {
      "Authorization": `Bearer ${env.FLEET_TOKEN}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body ?? {}),
  });
  const resp = await handler(req, env);
  const text = await resp.text();
  let parsed = {};
  try {
    parsed = text ? JSON.parse(text) : {};
  } catch {
    parsed = { body: text };
  }
  return { ok: resp.ok, status: resp.status, body: parsed };
}

async function runPlacementRunner(env, cfg) {
  const config = normalizePlacementRunnerConfig(cfg);
  const out = {
    schema_version: "witself.v0",
    placement_runner: config,
  };
  if (config.restore_archives) {
    const restore = await callInternalFleetHandler(
      env,
      "/v1/placement:restore",
      {
        batch: config.restore_batch,
        all_regions: config.restore_any_region,
      },
      handlePlacementRestore,
    );
    out.restore = restore.body;
    if (!restore.ok) {
      out.restore_error = { status: restore.status, body: restore.body };
      return out;
    }
  }
  if (config.rebalance) {
    const rebalance = await callInternalFleetHandler(
      env,
      "/v1/placement:rebalance",
      { batch: config.rebalance_batch },
      handlePlacementRebalance,
    );
    out.rebalance = rebalance.body;
    if (!rebalance.ok) {
      out.rebalance_error = { status: rebalance.status, body: rebalance.body };
    }
  }
  return out;
}

async function handlePlacementRun(request, env) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  let body = {};
  try {
    body = await request.json();
  } catch {
    // defaults are fine for manual runs
  }
  const stored = await placementRunnerConfig(env);
  const cfg = normalizePlacementRunnerConfig({
    ...stored,
    ...body,
    enabled: true,
  });
  const result = await runPlacementRunner(env, cfg);
  return json(result);
}

async function runScheduledPlacementRunner(env) {
  try {
    const cfg = await placementRunnerConfig(env);
    if (!cfg.enabled) {
      return;
    }
    const res = await runPlacementRunner(env, cfg);
    const restored = res.restore?.restored?.filter((r) => r.ok).length ?? 0;
    const rebalanced = res.rebalance?.rebalanced?.filter((r) => r.ok).length ?? 0;
    const restoreRemaining = res.restore?.remaining ?? 0;
    const rebalanceRemaining = res.rebalance?.remaining ?? 0;
    console.log(
      `placement-runner: restored=${restored} rebalanced=${rebalanced} restore_remaining=${restoreRemaining} rebalance_remaining=${rebalanceRemaining}`,
    );
  } catch (e) {
    console.log(`placement-runner failed: ${String(e?.message ?? e)}`);
  }
}

async function archivedCountsByCell(env) {
  const counts = new Map();
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.cell) {
        counts.set(entry.cell, (counts.get(entry.cell) ?? 0) + 1);
      }
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return counts;
}

async function handlePlacementStatus(request, env, url) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (request.method !== "GET") {
    return err("method not allowed", 405);
  }
  const sampleLimit = clampInt(url.searchParams.get("limit"), 25, 1, 200);
  const backupsEnabled = accountBackupSchedulingEnabled(env);
  const destinationOptions = { backupsEnabled };
  const cells = await listCells(env);
  const cellsByName = new Map(cells.map((cell) => [cell.name, cell]));
  const destinationCells = cells.filter(
    (cell) => cellIsEligibleDestination(cell, destinationOptions),
  );
  const liveCounts = await accountCountsByCell(env);
  const archivedCounts = await archivedCountsByCell(env);

  let archivedTotal = 0;
  let archivedPlaceable = 0;
  let archivedUnplaced = 0;
  let archivedClosedRetained = 0;
  const archivedBlocked = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
    for (const k of page.keys) {
      const accountId = k.name.slice("archived:".length);
      const archived = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!archived) {
        continue;
      }
      if (!isRestorableArchive(archived)) {
        archivedClosedRetained += 1;
        continue;
      }
      archivedTotal += 1;
      const target = bestPlacementCell(
        destinationCells,
        archived,
        liveCounts,
        false,
        destinationOptions,
      );
      if (target) {
        archivedPlaceable += 1;
        continue;
      }
      archivedUnplaced += 1;
      if (archivedBlocked.length < sampleLimit) {
        archivedBlocked.push({
          account_id: accountId,
          from_cell: archived.cell ?? null,
          region: archived.region ?? null,
          region_code: archived.region_code ?? null,
          reason: "no eligible accepting cell",
        });
      }
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);

  let liveTotal = 0;
  let movable = 0;
  let skipped = 0;
  const movableAccounts = [];
  const skippedAccounts = [];
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor: cursor2 });
    for (const k of page.keys) {
      const accountId = k.name.slice("acct:".length);
      const route = await env.DIRECTORY.get(k.name, { type: "json" });
      if (!route) {
        continue;
      }
      liveTotal += 1;
      const candidate = await rebalanceTargetForAccount(
        env,
        cellsByName,
        destinationCells,
        liveCounts,
        accountId,
        route,
        destinationOptions,
      );
      if (candidate.skip) {
        skipped += 1;
        if (skippedAccounts.length < sampleLimit) {
          skippedAccounts.push(candidate.skip);
        }
        continue;
      }
      if (candidate.target) {
        movable += 1;
        if (movableAccounts.length < sampleLimit) {
          movableAccounts.push({
            account_id: accountId,
            from_cell: candidate.current.name,
            to_cell: candidate.target.name,
            reason: candidate.reason,
          });
        }
      }
    }
    cursor2 = page.list_complete ? undefined : page.cursor;
  } while (cursor2);

  return json({
    schema_version: "witself.v0",
    placement_runner: await placementRunnerConfig(env),
    cells: cells.map((cell) => ({
      ...publicCell(cell),
      account_count: liveCounts.get(cell.name) ?? 0,
      archived_count: archivedCounts.get(cell.name) ?? 0,
    })),
    archived: {
      total: archivedTotal,
      placeable: archivedPlaceable,
      unplaced: archivedUnplaced,
      blocked: archivedBlocked,
      closed_retained: archivedClosedRetained,
    },
    live: {
      total: liveTotal,
      movable,
      skipped,
      movable_accounts: movableAccounts,
      skipped_accounts: skippedAccounts,
    },
  });
}

async function handlePlacementRescue(request, env, accountId) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  if (request.method !== "POST") {
    return err("method not allowed", 405);
  }
  let body = {};
  try {
    body = await request.json();
  } catch {
    // Clearing every hard pin is the safe operator-rescue default.
  }
  const axes = body.axes == null ? ["cloud", "region", "channel"] : body.axes;
  if (!Array.isArray(axes) || axes.length === 0 ||
      axes.some((axis) => typeof axis !== "string" || !PLACEMENT_RESCUE_AXES.has(axis))) {
    return err("axes must be a non-empty subset of cloud, region, channel", 400);
  }
  const uniqueAxes = [...new Set(axes)];
  const key = `archived:${accountId}`;
  const archived = await env.DIRECTORY.get(key, { type: "json" });
  if (!archived) {
    return err("archived account not found", 404);
  }
  if (!isRestorableArchive(archived)) {
    return err("closed archive is retained and not eligible for placement rescue", 409);
  }
  const nextPolicy = rescuePlacementPolicy(archived.placement_policy, uniqueAxes);
  const changed = !archived.placement_policy ||
    JSON.stringify(nextPolicy) !== JSON.stringify(archived.placement_policy);
  if (changed) {
    archived.placement_policy = nextPolicy;
    archived.placement_rescue = {
      at: new Date().toISOString(),
      cleared_axes: uniqueAxes,
    };
    await env.DIRECTORY.put(key, JSON.stringify(archived));
  }
  return json({
    schema_version: "witself.v0",
    account_id: accountId,
    changed,
    cleared_axes: uniqueAxes,
    placement_policy: nextPolicy,
  });
}

// handleProbe is POST /v1/cells/{name}:probe — a fleet-token-authorized
// reachability check. Reads the cell's endpoint from KV, does a bounded
// GET on <endpoint>/v1/version, and reports whether the Worker (which is
// the client that will do the restore) can currently reach the cell.
//
// The probe ALWAYS returns 200 to the caller unless authorization fails
// or the cell is unknown; whether the cell itself is reachable is
// reported in the response body via {ok, reason}. This keeps the wait
// loop simple: any HTTP-level failure is an infrastructure problem the
// caller can't fix by retrying, but ok=false is a "cell not ready yet"
// signal it should keep polling on.
async function handleProbe(request, env, cellName) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  const cell = await env.DIRECTORY.get(`cell:${cellName}`, { type: "json" });
  if (!cell) {
    return err("unknown cell", 404);
  }
  if (!cell.endpoint) {
    return json({ ok: false, reason: "cell has no endpoint" });
  }

  let resp;
  try {
    resp = await fetch(`${cell.endpoint}/v1/version`, {
      method: "GET",
      signal: AbortSignal.timeout(10000),
    });
  } catch (e) {
    // DNS errors, TLS errors, connect refused all land here.
    const msg = String(e?.message ?? e);
    return json({ ok: false, reason: msg.slice(0, 200) });
  }

  if (!resp.ok) {
    // The cell answered but not with success — during warmup this looks
    // like 502 (ALB target draining), 503 (pod not ready), 404 (default
    // backend before ingress reconciles).
    const body = await resp.text().catch(() => "");
    return json({
      ok: false,
      reason: `HTTP ${resp.status}: ${body.slice(0, 120)}`,
      cell_status: resp.status,
    });
  }

  // Success shape: witself-server /v1/version returns
  // {schema_version, version, commit, date}. Extract version so the
  // driver can log which build actually answered.
  let cellVersion = "";
  try {
    const body = await resp.json();
    cellVersion = body?.version ?? "";
  } catch {
    // Answered 200 but not with a witself-server-shaped JSON: something
    // else is on that hostname. Treat as not-ready — a fresh witself
    // pod will answer correctly once it starts.
    return json({
      ok: false,
      reason: "cell /v1/version response was not JSON — wrong service on the endpoint?",
      cell_status: resp.status,
    });
  }
  return json({
    ok: true,
    cell_status: resp.status,
    cell_version: cellVersion,
  });
}

// Fleet-only backup operations. Scheduled backups remain independently gated
// by CP_ACCOUNT_BACKUPS_ENABLED=false, while these explicit calls make the MVP
// observable and testable before that clock is activated.
async function handleAccountBackups(request, env, url) {
  if (!fleetAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  try {
    if (url.pathname === ACCOUNT_BACKUP_STATUS_PATH) {
      if (request.method !== "GET") {
        return err("method not allowed", 405);
      }
      const accountID = url.searchParams.get("account_id") ?? undefined;
      if (accountID !== undefined && !ACCOUNT_ID.test(accountID)) {
        return err("invalid account_id", 400);
      }
      return json(await accountBackupStatus(env, accountID));
    }

    if (request.method !== "POST") {
      return err("method not allowed", 405);
    }
    let body;
    try {
      body = await request.json();
    } catch {
      return err("invalid backup request", 400);
    }
    if (!ACCOUNT_ID.test(body?.account_id ?? "")) {
      return err("invalid account_id", 400);
    }

    if (url.pathname === ACCOUNT_BACKUP_RUN_PATH) {
      const scheduledTime = body.scheduled_at === undefined
        ? Date.now()
        : Date.parse(body.scheduled_at);
      if (!Number.isFinite(scheduledTime)) {
        return err("scheduled_at must be an RFC3339 timestamp", 400);
      }
      return json(await runManualAccountBackup(
        env,
        body.account_id,
        scheduledTime,
      ));
    }

    if (
      !CELL_NAME.test(body?.target_cell ?? "") ||
      !ACCOUNT_BACKUP_ID.test(body?.backup_id ?? "")
    ) {
      return err("backup_id and target_cell are required", 400);
    }
    return json(await runAccountBackupValidation(env, {
      account_id: body.account_id,
      backup_id: body.backup_id,
      target_cell: body.target_cell,
    }));
  } catch (error) {
    const message = String(error?.message ?? error).slice(0, 300);
    const conflict =
      /backup_validation_target|accepting=false|live cell|live account projections/.test(
      message,
    );
    console.log(`account-backup: fleet operation failed: ${message}`);
    return err(message, conflict ? 409 : 502);
  }
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    // Provider-hosted Checkout and portal flows return to value-free public
    // pages at this already-owned edge. They never reach the container, read
    // account state, or gain mutation authority from a browser redirect.
    const billingReturn = billingReturnResponse(request, url);
    if (billingReturn !== null) return billingReturn;

    if (url.pathname === SUPPORT_EMAIL_INTAKE_PATH) {
      return handleSupportEmailIntake(request, env);
    }

    // Hot path: directory lookups from KV, never the container.
    const m = url.pathname.match(DIRECTORY_PATH);
    if (m) {
      if (request.method !== "GET") {
        return err("method not allowed", 405);
      }
      // Shorter cache than the 300s original — evacuation flips acct:
      // to archived: and the cache window is how long stale routing
      // survives. 60s trades some KV read amplification for a much
      // smaller post-evacuation confusion window.
      const entry = await env.DIRECTORY.get(`acct:${m[1]}`, {
        type: "json",
        cacheTtl: 60,
      });
      if (entry) {
        return json(
          { schema_version: "witself.v0", account_id: m[1], cell: entry },
          200,
          { "Cache-Control": "max-age=60" },
        );
      }
      // Second-chance lookup: archived accounts return a 200 with a
      // distinct shape so the CLI can distinguish "gone" from "awaiting
      // placement" — the whole point of not deleting on evacuation. The
      // response deliberately EXCLUDES object key / sha256 / size — those
      // are fleet-internal facts that would let an unauthenticated caller
      // fingerprint archive layouts and per-tenant sizes.
      const archived = await env.DIRECTORY.get(`archived:${m[1]}`, {
        type: "json",
        cacheTtl: 30,
      });
      if (archived) {
        return json(
          {
            schema_version: "witself.v0",
            account_id: m[1],
            archived: {
              cell: archived.cell,
              region: archived.region ?? null,
              region_code: archived.region_code ?? null,
              exported_at: archived.exported_at,
            },
          },
          200,
          { "Cache-Control": "max-age=30" },
        );
      }
      return err("unknown account", 404);
    }

    // Fleet registry (fleet-token authorized).
    if (
      url.pathname === "/v1/cells" ||
      CELL_PATH.test(url.pathname) ||
      PURGE_PATH.test(url.pathname)
    ) {
      return handleCells(request, env, url);
    }

    // Cell evacuation (fleet-token authorized).
    const em = url.pathname.match(EVACUATE_PATH);
    if (em) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleEvacuate(request, env, em[1]);
    }

    // Cell restore: the mirror of :evacuate. Bounded batch of archived:
    // accounts (region-matched to the target cell) get streamed from R2
    // into the cell's :import, then :resume, then the archived: pointer
    // is retired in favor of an acct: pointer at the new cell.
    const rsm = url.pathname.match(RESTORE_PATH);
    if (rsm) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleRestore(request, env, rsm[1]);
    }

    // Cell reachability probe: the driver polls this between registerCell
    // and restoreCell so the wait step reflects the Worker's DNS/routing
    // view — the client that will actually do the restore — rather than
    // the operator's local resolver, which can hold stale NXDOMAIN across
    // destroy+up cycles for hours (see issue #22).
    const pbm = url.pathname.match(PROBE_PATH);
    if (pbm) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleProbe(request, env, pbm[1]);
    }

    // Invite codes (fleet-token authorized).
    if (url.pathname === "/v1/invites" || INVITE_PATH.test(url.pathname)) {
      return handleInvites(request, env, url);
    }

    // Admin credential registry (fleet-token authorized). The credentials
    // this mints are what the witself-admin CLI carries against the
    // admin-side fan-out routes (slice 1b.iii).
    if (
      url.pathname === "/v1/admins" ||
      ADMIN_PATH.test(url.pathname) ||
      ADMIN_REVOKE_PATH.test(url.pathname)
    ) {
      return handleAdmins(request, env, url);
    }

    // Account policy and plan-override administration crosses a strict
    // Worker->Go trust bridge. The caller's witself_adm_* token is verified
    // at the edge and is never forwarded. Go receives only the internal
    // bridge bearer plus the Worker-verified immutable X-Witself-Admin-ID and
    // display X-Witself-Admin-Handle for its audit record.
    if (matchAdminPolicyPath(url.pathname)) {
      const admin = await adminAuthorized(request, env);
      if (!admin) return err("unauthorized", 401);
      if (!adminScopeAllows(admin.scope, "fleet-admin-surface")) {
        return err("credential scope does not allow this action", 403);
      }
      return forwardAdminPolicyRequest(
        request,
        env,
        admin,
        (bridgedRequest) =>
          getContainer(env.CONTROL_PLANE, "singleton").fetch(bridgedRequest),
      );
    }

    // Managed realm-email aliases live in one global namespace, so customer
    // requests and platform-admin governance terminate at the Worker-backed
    // registry rather than being forwarded to one tenant cell. Customer
    // operator tokens are still verified against the owning cell first.
    const realmEmailCanonicalClose = matchRealmEmailCanonicalClosePath(
      url.pathname,
    );
    if (realmEmailCanonicalClose) {
      return handleRealmEmailCanonicalCloseRequest(
        request,
        env,
        realmEmailCanonicalClose,
      );
    }
    const realmEmailAliasCustomer = matchRealmEmailAliasCustomerPath(
      url.pathname,
    );
    if (realmEmailAliasCustomer) {
      return handleRealmEmailAliasCustomerRequest(
        request,
        env,
        realmEmailAliasCustomer,
      );
    }
    const agentEmailDomainCustomer = matchAgentEmailDomainCustomerPath(
      url.pathname,
    );
    if (agentEmailDomainCustomer) {
      return handleAgentEmailDomainCustomerRequest(
        request,
        env,
        agentEmailDomainCustomer,
      );
    }
    if (isAgentEmailDomainRecoveryAdminPath(url.pathname)) {
      const admin = await adminAuthorized(request, env);
      if (!admin) return err("unauthorized", 401);
      if (!adminScopeAllows(admin.scope, "fleet-admin-surface")) {
        return err("credential scope does not allow this action", 403);
      }
      return handleAgentEmailDomainRecoveryAdminRequest(
        request,
        env,
        url,
        admin,
      );
    }
    if (isAgentEmailDomainAdminPath(url.pathname)) {
      const admin = await adminAuthorized(request, env);
      if (!admin) return err("unauthorized", 401);
      if (!adminScopeAllows(admin.scope, "fleet-admin-surface")) {
        return err("credential scope does not allow this action", 403);
      }
      return handleAgentEmailDomainAdminRequest(request, env, url, admin);
    }
    if (isRealmEmailAliasRecoveryAdminPath(url.pathname)) {
      const admin = await adminAuthorized(request, env);
      if (!admin) return err("unauthorized", 401);
      if (!adminScopeAllows(admin.scope, "fleet-admin-surface")) {
        return err("credential scope does not allow this action", 403);
      }
      return handleRealmEmailAliasRecoveryAdminRequest(
        request,
        env,
        url,
        admin,
      );
    }
    if (isRealmEmailAliasAdminPath(url.pathname)) {
      const admin = await adminAuthorized(request, env);
      if (!admin) return err("unauthorized", 401);
      if (!adminScopeAllows(admin.scope, "fleet-admin-surface")) {
        return err("credential scope does not allow this action", 403);
      }
      return handleRealmEmailAliasAdminRequest(request, env, url, admin);
    }
    if (isAgentEmailOperationsLeasePath(url.pathname)) {
      return handleAgentEmailOperationsLeaseRequest(request, env, url);
    }
    if (url.pathname === EDGE_MANAGED_DELIVERY_READINESS_PATH) {
      return handleManagedDeliveryReadinessRequest(request, env);
    }
    if (isRealmEmailRoutePath(url.pathname)) {
      const realmEmailRoute = matchRealmEmailRoutePath(url.pathname);
      // Edge fallback terminates here and reads the authoritative Durable
      // Object. It never falls through to the cold Go container or trusts the
      // same eventually consistent KV entry that triggered the refresh.
      return handleRealmEmailRouteRequest(request, env, realmEmailRoute);
    }

    // Admin-side fan-out routes (admin-token authorized). Fleet-wide
    // ticket list + per-account thread/reply/state. The Worker is the
    // only door — the CLI never touches a cell directly.
    if (
      url.pathname === "/v1/admin/whoami" ||
      url.pathname === "/v1/admin/cells" ||
      url.pathname === "/v1/admin/events" ||
      url.pathname === "/v1/admin/tickets" ||
      ADMIN_ACCOUNT_TICKET_PATH.test(url.pathname) ||
      ADMIN_ACCOUNT_TICKET_MSGS_PATH.test(url.pathname) ||
      ADMIN_ACCOUNT_TICKET_STATE_PATH.test(url.pathname) ||
      ADMIN_ACCOUNT_TICKET_RETRIAGE_PATH.test(url.pathname) ||
      ADMIN_ACCOUNT_SUPPORT_POLICY_PATH.test(url.pathname)
    ) {
      return handleAdminTickets(request, env, ctx, url);
    }

    // Fleet-wide placement strategy (fleet-token authorized).
    if (url.pathname === "/v1/placement") {
      return handlePlacement(request, env);
    }
    if (url.pathname === "/v1/placement-runner") {
      return handlePlacementRunner(request, env);
    }
    if (url.pathname === "/v1/placement-status") {
      return handlePlacementStatus(request, env, url);
    }
    const placementRescue = url.pathname.match(PLACEMENT_RESCUE_PATH);
    if (placementRescue) {
      return handlePlacementRescue(request, env, placementRescue[1]);
    }
    if (url.pathname === "/v1/placement:run") {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handlePlacementRun(request, env);
    }
    if (url.pathname === "/v1/placement:restore") {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handlePlacementRestore(request, env);
    }
    if (url.pathname === "/v1/placement:rebalance") {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handlePlacementRebalance(request, env);
    }

    if (
      url.pathname === ACCOUNT_BACKUP_STATUS_PATH ||
      url.pathname === ACCOUNT_BACKUP_RUN_PATH ||
      url.pathname === ACCOUNT_BACKUP_RESTORE_DRILL_PATH
    ) {
      return handleAccountBackups(request, env, url);
    }

    // Fleet-wide pending-account expiry policy (fleet-token authorized).
    if (url.pathname === "/v1/reaper") {
      return handleReaper(request, env);
    }

    // Email-verification links: the human half of account activation.
    const vm = url.pathname.match(VERIFY_PATH);
    if (vm) {
      if (request.method !== "GET") {
        return err("method not allowed", 405);
      }
      return handleVerify(env, vm[1]);
    }

    // Signup: public, invite-gated. The one door you can knock on with nothing.
    if (url.pathname === "/v1/accounts") {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleSignup(request, env);
    }

    // Account close: operator-token pass-through to the account's cell.
    const cm = url.pathname.match(ACCOUNT_CLOSE_PATH);
    if (cm) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleClose(request, env, cm[1]);
    }

    // Resend verification: operator-token authorized via the account's cell.
    const rm = url.pathname.match(ACCOUNT_RESEND_PATH);
    if (rm) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleResend(request, env, rm[1]);
    }

    // Recovery: the one unauthenticated account verb; inbox control is proof.
    const rcm = url.pathname.match(ACCOUNT_RECOVER_PATH);
    if (rcm) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleRecover(request, env, rcm[1]);
    }

    // Email change: operator-authenticated, new-inbox-confirmed.
    const cem = url.pathname.match(ACCOUNT_CHANGE_EMAIL_PATH);
    if (cem) {
      if (request.method !== "POST") {
        return err("method not allowed", 405);
      }
      return handleChangeEmail(request, env, cem[1]);
    }

    // Undo an email change: the human half of the 48-hour revert window.
    const uem = url.pathname.match(UNDO_EMAIL_PATH);
    if (uem) {
      if (request.method !== "GET") {
        return err("method not allowed", 405);
      }
      return handleUndoEmail(env, uem[1]);
    }

    // Go->Worker callbacks for account discovery and cell plan application.
    // This namespace always terminates at the Worker, including unknown paths,
    // so a callback can never fall through and loop back into Go.
    if (isInternalBridgePath(url.pathname)) {
      return handleInternalBridgeRequest(
        request,
        env,
        fetch,
        url.pathname === PLAN_LIFECYCLE_ACTIVATE_PATH
          ? () => getContainer(env.CONTROL_PLANE, "singleton")
          : undefined,
      );
    }

    // Cold path: the Go container.
    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },

  // Cron: pending-account expiry plus opt-in placement restore/rebalance.
  async scheduled(_event, env, ctx) {
    ctx.waitUntil(reapExpiredPendings(env));
    ctx.waitUntil(runScheduledPlacementRunner(env));
    ctx.waitUntil(runScheduledAccountBackups(env, _event?.scheduledTime));
    ctx.waitUntil(runScheduledCanonicalRealmRouteInventory(env));
    ctx.waitUntil(runScheduledAgentEmailDomainVerification(env));
    ctx.waitUntil(runScheduledPlanLifecycle(
      env,
      (request) => getContainer(env.CONTROL_PLANE, "singleton").fetch(request),
    ));
  },
};
