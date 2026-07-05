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
//   acct:<account_id>    -> {"cell":"<name>","endpoint":"https://..."}
//   pending:<account_id> -> {"cell":"<name>","created_at":"<iso>"}
//                           reap candidate; dropped on activation/close/reap
//   verify:<sha256(token)> -> {"account_id","cell","created_at"}
//                           email-verification token (hash only, KV TTL 7d)
//   recover:<account_id> -> {"code_hash","code_expires_at","attempts",
//                           "emails_sent","last_email_at"} (KV TTL 24h)
//                           recovery code + rate-limit state, phantom ids too
//   emailchange:<account_id> -> {"code_hash","code_expires_at","new_email",
//                           "attempts","emails_sent","last_email_at"} (KV TTL 24h)
//                           pending email change + rate-limit state
//   undoemail:<account_id> -> {"code_hash","old_email","new_email","expires_at"}
//                           (KV TTL 48h) — undo window shipped in the notice
//   archived:<account_id> -> {"cell","region","object","exported_at","size",
//                           "format_version"}
//                           post-evacuation state; the directory answers
//                           "archived — awaiting placement" for these. Only
//                           {cell,region,exported_at} are returned to the
//                           public directory route; the rest are fleet-only.
//   evac:<cell>          -> {"started_at","done","failed"[],"remaining",
//                           "finished_at"}
//                           cross-batch progress for a cell-wide evacuation.
//   cell:<name>          -> {"endpoint","cloud","region","owner","weight",
//                             "accepting","registered_at"}
//   invite:<code>        -> {"enabled","not_before","expires_at","max_uses",
//                             "uses","note","created_at","cell","region"}
//   config:placement     -> {"strategy":"weighted"|"pinned","pinned_cell"}
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

export class Backend extends Container {
  defaultPort = 8080;
  sleepAfter = "10m";
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
const ADMIN_HANDLE = /^[a-z][a-z0-9_-]{1,31}$/;
// Admin-side fan-out paths (all admin-token authorized via adminAuthorized).
// /v1/admin/whoami        — cheap round-trip that also verifies the token.
// /v1/admin/tickets       — fleet-wide list; CP fans out to every cell.
// /v1/admin/accounts/{a}/tickets/{t}          — GET single ticket + thread.
// /v1/admin/accounts/{a}/tickets/{t}/messages — POST admin reply.
// /v1/admin/accounts/{a}/tickets/{t}/state    — PATCH state transition.
const ADMIN_ACCOUNT_TICKET_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)$/;
const ADMIN_ACCOUNT_TICKET_MSGS_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)\/messages$/;
const ADMIN_ACCOUNT_TICKET_STATE_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/tickets\/(tkt_[a-z0-9]+)\/state$/;
// Support-policy read/write per account. GET returns current policy,
// PATCH flips it. Both proxy to the cell's admin: routes and inherit
// the archived-account 409 / unknown-account 404 rules.
const ADMIN_ACCOUNT_SUPPORT_POLICY_PATH =
  /^\/v1\/admin\/accounts\/([A-Za-z0-9_-]{1,128})\/support-policy$/;
// Handles that would collide with an existing actor_kind or role name
// (owner / operator / control_plane / system). Reserved so a rogue mint
// can't forge audit rows that read like a system-emitted event.
const RESERVED_HANDLES = new Set([
  "system",
  "control_plane",
  "root",
  "admin",
  "fleet",
  "owner",
  "operator",
]);

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
  return { admin_id: rec.admin_id, handle: rec.handle };
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

// listCells returns raw registry entries INCLUDING the per-cell provision
// token. Never serve these to clients — use publicCell() first.
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
// control plane (the provision token is the control plane's secret for the
// cell — even fleet-token holders don't need to read it back).
function publicCell(cell) {
  const { provision_token: _omitted, ...rest } = cell;
  return { ...rest, has_provision_token: Boolean(cell.provision_token) };
}

// True if any acct: directory entry points at the named cell. O(accounts) —
// acceptable while the fleet is young; replaced by authoritative DO counters
// when signup lands.
async function cellHasAccounts(env, name) {
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry && entry.cell === name) return true;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return false;
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
    if (!ADMIN_HANDLE.test(handle)) {
      return err(
        "invalid handle (2-32 lowercase chars; must start with a letter; letters/digits/underscore/hyphen only)",
        400
      );
    }
    if (RESERVED_HANDLES.has(handle)) {
      return err(`handle "${handle}" is reserved`, 400);
    }
    const note = body.note == null ? undefined : String(body.note).trim();
    if (note !== undefined && note.length > 200) {
      return err("note too long (max 200 characters)", 400);
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
async function fanoutCells(env, path, jsonBody) {
  const cells = (await listCells(env)).filter(
    (c) => c.provision_token && c.endpoint
  );
  return Promise.all(
    cells.map(async (c) => {
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
  const emailArgs = renderSupportEmail(kind, {
    accountID,
    ticketID,
    adminHandle: admin.handle,
    body,
  });
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

// renderSupportEmail returns { subject, text, html } for one of the
// three support notifications. Kept declarative — one object per kind
// so wording drifts are visible in a single place.
function renderSupportEmail(kind, params) {
  const { accountID, ticketID, adminHandle, body } = params;
  const showCmd = `ws account support show --ticket ${ticketID}`;
  const replyCmd = `ws account support reply --ticket ${ticketID} --stdin`;
  const openCmd = `ws account support open`;

  const preview = (() => {
    if (!body) return "";
    const clean = body.replace(/\s+/g, " ").trim();
    if (clean.length <= 400) return clean;
    return clean.slice(0, 400) + "…";
  })();

  const variants = {
    reply: {
      title: "Support replied to your ticket",
      subject: `Support replied to your ticket ${ticketID}`,
      opening: `${adminHandle} from Witself support replied to your ticket.`,
      cta: replyCmd,
      ctaLabel: "Reply",
    },
    resolved: {
      title: "Your support ticket was marked resolved",
      subject: `Ticket ${ticketID} marked resolved`,
      opening: `${adminHandle} from Witself support marked your ticket as resolved.`,
      cta: `ws account support close --ticket ${ticketID}`,
      ctaLabel: "Close it out",
    },
    closed: {
      title: "Your support ticket was closed",
      subject: `Ticket ${ticketID} closed`,
      opening: `${adminHandle} from Witself support closed your ticket.`,
      cta: openCmd,
      ctaLabel: "Open a new ticket",
    },
  };
  const v = variants[kind];
  const previewHTML = preview
    ? `<blockquote style="margin:0 0 20px;padding:12px 16px;border-left:3px solid ${EMAIL_BORDER};color:${EMAIL_MUTED};font-size:14px;">${escapeHTML(preview)}</blockquote>`
    : "";
  const html = renderEmail({
    title: v.title,
    preheader: v.opening,
    body: `
      <p style="margin:0 0 16px;">${escapeHTML(v.opening)}</p>
      <p style="margin:0 0 8px;color:${EMAIL_MUTED};font-size:13px;">Account · Ticket</p>
      <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 20px;">${escapeHTML(accountID)} · ${escapeHTML(ticketID)}</div>
      ${previewHTML}
      <p style="margin:0 0 8px;">View the full thread:</p>
      ${cliBlock(showCmd)}
      <p style="margin:20px 0 8px;">${escapeHTML(v.ctaLabel)}:</p>
      ${cliBlock(v.cta)}
    `,
  });
  const textPreview = preview ? `\n\n> ${preview}\n` : "";
  const text = `${v.opening}\n\nAccount: ${accountID}\nTicket:  ${ticketID}${textPreview}\n\nView the thread:\n\n  ${showCmd}\n\n${v.ctaLabel}:\n\n  ${v.cta}\n`;
  return { subject: v.subject, text, html };
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
    return json({
      schema_version: "witself.v0",
      admin_id: admin.admin_id,
      handle: admin.handle,
    });
  }

  // Fleet cells with per-cell account counts — the dashboard's cells
  // pane. Counts come from the CP's own directory (acct: pointers),
  // which is authoritative for placement; an O(accounts) KV scan is
  // fine at current fleet scale (same tradeoff cellHasAccounts makes,
  // and the same DO-counter upgrade path applies when it stops being
  // fine). provision tokens never leave the CP (publicCell).
  if (url.pathname === "/v1/admin/cells" && request.method === "GET") {
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
    return json({
      schema_version: "witself.v0",
      cells: cells.map((c) => ({
        ...publicCell(c),
        account_count: counts.get(c.name) ?? 0,
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
  ];
  for (const route of routes) {
    const m = url.pathname.match(route.rx);
    if (!m) continue;
    if (request.method !== route.method) {
      return err("method not allowed", 405);
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
    } else if (route.action === "change-ticket-state") {
      cellBody.state = clientBody.state ?? "";
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
  let pool = (await listCells(env)).filter(
    (c) => c.accepting !== false && c.provision_token,
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
    const key = `cell:${body.name}`;
    const existing = await env.DIRECTORY.get(key, { type: "json" });
    const entry = {
      endpoint: body.endpoint,
      cloud: body.cloud || "",
      region: body.region || "",
      // v0: one fleet token, one owner. With per-party credentials the owner
      // comes from the credential, never the payload.
      owner: "witwave",
      weight: Number.isFinite(body.weight) ? body.weight : 1,
      accepting: body.accepting !== false,
      // The cell's account-provisioning credential. Preserved when the payload
      // omits it (drain re-registers from a stripped read and must not wipe it).
      provision_token: body.provision_token ?? existing?.provision_token ?? null,
      registered_at: existing?.registered_at ?? new Date().toISOString(),
    };
    await env.DIRECTORY.put(key, JSON.stringify(entry));
    return json(
      { schema_version: "witself.v0", cell: publicCell({ name: body.name, ...entry }) },
      existing ? 200 : 201,
    );
  }

  // PURGE: the explicitly destructive removal path, for teardowns where the
  // cell's data is genuinely dying (witself-infra down --destroy-accounts).
  // Deletes every directory entry pointing at the cell, then the cell itself.
  // Idempotent: re-running reports zero. The safe path is DELETE below, which
  // refuses while accounts exist.
  const pm = url.pathname.match(PURGE_PATH);
  if (pm && request.method === "POST") {
    const name = pm[1];
    let purged = 0;
    let cursor;
    do {
      const page = await env.DIRECTORY.list({ prefix: "acct:", cursor });
      for (const k of page.keys) {
        const entry = await env.DIRECTORY.get(k.name, { type: "json" });
        if (entry && entry.cell === name) {
          await env.DIRECTORY.delete(k.name);
          purged++;
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    await deletePendingForCell(env, name);
    const key = `cell:${name}`;
    const existed = (await env.DIRECTORY.get(key)) !== null;
    if (existed) await env.DIRECTORY.delete(key);
    return json({
      schema_version: "witself.v0",
      name,
      purged_accounts: purged,
      cell_deleted: existed,
    });
  }

  const m = url.pathname.match(CELL_PATH);
  if (m && request.method === "DELETE") {
    const name = m[1];
    const key = `cell:${name}`;
    const cell = await env.DIRECTORY.get(key, { type: "json" });
    if (!cell) {
      return err("unknown cell", 404);
    }
    if (cell.accepting !== false) {
      return err("cell must be drained first (re-register with accepting=false)", 409);
    }
    if (await cellHasAccounts(env, name)) {
      return err("accounts still live on this cell", 409);
    }
    await deletePendingForCell(env, name); // no accounts -> candidates are stale
    await env.DIRECTORY.delete(key);
    return new Response(null, { status: 204 });
  }

  return err("method not allowed", 405);
}

// deletePendingForCell drops every reap candidate naming the cell. Called when
// a cell leaves the registry (purge/delete): a candidate without a registry
// entry can never be reaped, and a later re-registration under the same name
// must not inherit a dead fleet's candidate list.
async function deletePendingForCell(env, name) {
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "pending:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.cell === name) {
        await env.DIRECTORY.delete(k.name);
      }
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
}

// handleSignup is POST /v1/accounts — the public, invite-gated front door of
// Witself Cloud. It orchestrates but stores no tenant data: validate the
// invite, place onto an accepting cell, have the CELL provision the account,
// record the one routing pointer, and return everything the CLI needs inline
// (never depending on KV propagation for the first request).
async function handleSignup(request, env) {
  let body;
  try {
    body = await request.json();
  } catch {
    return err("invalid JSON body", 400);
  }
  const email = (body.email ?? "").trim().toLowerCase();
  if (!email || !email.includes("@")) {
    return err("valid email required", 400);
  }
  const code = body.invite ?? "";
  if (!INVITE_CODE.test(code)) {
    return err("invite code required", 403);
  }
  const inviteKey = `invite:${code}`;
  const invite = await env.DIRECTORY.get(inviteKey, { type: "json" });
  const verdict = validateInvite(invite);
  if (!verdict.valid) {
    return err(`invalid invite: ${verdict.reason}`, 403);
  }

  // Placement: invite pin -> invite region -> fleet strategy -> weighted.
  const placed = await placeAccount(env, invite);
  if (placed.fail) {
    return placed.fail;
  }
  const cell = placed.cell;

  // The cell provisions the account (its data, its database).
  let cellResp;
  try {
    cellResp = await fetch(`${cell.endpoint}/v1/accounts`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({ email, display_name: body.display_name ?? "" }),
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return err(`cell ${cell.name} unreachable — try again shortly`, 502);
  }
  if (cellResp.status === 409) {
    return err("an account with this email already exists", 409);
  }
  if (cellResp.status !== 201) {
    return err(`cell provisioning failed (${cell.name})`, 502);
  }
  let provisioned;
  try {
    provisioned = (await cellResp.json()).account;
  } catch {
    provisioned = null;
  }
  if (!provisioned?.account_id || !provisioned?.bootstrap_token || !ACCOUNT_ID.test(provisioned.account_id)) {
    return err("cell returned an invalid provisioning response", 502);
  }

  // Expiry candidate FIRST, routing pointer second: if the second write
  // fails, a candidate without routing is self-healing (the sweep reaps the
  // orphaned provision), whereas routing without a candidate would be a
  // pending account the reaper can never see. Only a candidate — the cell's
  // only-if-pending :reap guard is the truth at reap time.
  const pendingEntry = { cell: cell.name, created_at: new Date().toISOString() };
  await env.DIRECTORY.put(
    `pending:${provisioned.account_id}`,
    JSON.stringify(pendingEntry),
  );
  // Routing pointer + best-effort invite consumption (exact counting arrives
  // with the DO authority).
  await env.DIRECTORY.put(
    `acct:${provisioned.account_id}`,
    JSON.stringify({ cell: cell.name, endpoint: cell.endpoint }),
  );
  const fresh = (await env.DIRECTORY.get(inviteKey, { type: "json" })) ?? invite;
  fresh.uses = (fresh.uses ?? 0) + 1;
  await env.DIRECTORY.put(inviteKey, JSON.stringify(fresh));

  // Verification email — best-effort by design: the account exists either
  // way, and an unverified one is the reaper's problem, not signup's. The
  // email address is used in flight and never stored here.
  let emailSent = false;
  try {
    emailSent = await sendVerificationEmail(
      env,
      new URL(request.url).origin,
      provisioned.email,
      provisioned.account_id,
      cell.name,
    );
  } catch (e) {
    console.log(`signup: verification email for ${provisioned.account_id} failed: ${e}`);
  }
  if (emailSent) {
    // Stamp the send on the candidate so the resend cooldown starts at
    // signup, not at the first resend.
    pendingEntry.emails_sent = 1;
    pendingEntry.last_email_at = new Date().toISOString();
    await env.DIRECTORY.put(
      `pending:${provisioned.account_id}`,
      JSON.stringify(pendingEntry),
    );
    await logCellEvent(cell, provisioned.account_id, "account.email.verify.sent",
      "control_plane", { to_masked: maskEmail(provisioned.email) });
  }

  return json(
    {
      schema_version: "witself.v0",
      account_id: provisioned.account_id,
      operator_id: provisioned.operator_id,
      email: provisioned.email,
      status: provisioned.status,
      verification_email_sent: emailSent,
      cell: { name: cell.name, endpoint: cell.endpoint },
      bootstrap_token: provisioned.bootstrap_token,
    },
    201,
  );
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
    return htmlPage(404, "Link invalid or expired", "This verification link is invalid or has expired. If you already verified, your account is active — <code>ws account status</code> will confirm it. If the account was closed for missing the verification window, simply sign up again.");
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
      return htmlPage(200, "Already verified", `Your account <code>${entry.account_id}</code> was already verified — nothing more to do. Back in your terminal, <code>ws account status</code> will show it active.`);
    }
    return htmlPage(200, "Account verified", `Your account <code>${entry.account_id}</code> is active. Back in your terminal, <code>ws account status</code> will confirm it — you're ready to create realms and agents.`);
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
  } catch (e) {
    console.log(`resend: verification email for ${accountId} failed: ${e}`);
  }
  if (!emailSent) {
    return err("could not send the verification email — try again shortly", 502);
  }
  // created_at is preserved deliberately: resending never resets the reap
  // clock — the email says so.
  await env.DIRECTORY.put(
    pendingKey,
    JSON.stringify({
      ...pending,
      emails_sent: sent + 1,
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
  } catch (e) {
    console.log(`resend: audit cell lookup for ${accountId} failed: ${e}`);
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
    // between get() and put(). Fail-closed on infra failures (see the
    // send-failure branch below) restores the slots so the owner isn't
    // punished for a mail outage. TTL is 4h so an owner who hits the
    // cap during an incident recovers within a working day, and a
    // burst attack's blast radius is bounded to hours not a full day.
    const now = new Date().toISOString();
    rl.emails_sent = (rl.emails_sent ?? 0) + 1;
    rl.last_email_at = now;
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
    let sent = false;
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
        const cmd = `ws account recover --id ${accountId} --code ${code}`;
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
        await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: 4 * 3600 });
        sent = true;
        await logCellEvent(cell, accountId, "account.email.recovery.sent",
          "control_plane", { to_masked: maskEmail(contact.email) });
      } catch (e) {
        console.log(`recover: email for ${accountId} failed: ${e}`);
        // Fail-open on infrastructure failures: mail delivery blew up,
        // that's not the owner's fault. Roll the slots back so a
        // subsequent retry has room. Racy against concurrent
        // increments but the direction stays consistent — the
        // decrement compensates for the reservation we did above.
        rl.emails_sent = Math.max(0, (rl.emails_sent ?? 1) - 1);
        rlPerIp.emails_sent = Math.max(0, (rlPerIp.emails_sent ?? 1) - 1);
        await Promise.all([
          env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: 4 * 3600 }),
          env.DIRECTORY.put(rlPerIpKey, JSON.stringify(rlPerIp), { expirationTtl: 4 * 3600 }),
        ]);
      }
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
  rl.attempts = (rl.attempts ?? 0) + 1;
  await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: 24 * 3600 });
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
      const cmd = `ws account change-email --new-email ${newEmail} --code ${code}`;
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
    } catch (e) {
      console.log(`change-email: code to ${accountId}'s new address failed: ${e}`);
      return err("could not send the confirmation email — try again shortly", 502);
    }
    await logCellEvent(cell, accountId, "account.email.change.sent",
      "control_plane", { to_masked: maskEmail(newEmail) });
    // Persist only after the code actually left; a send failure must not
    // burn quota or corrupt an issued code.
    state.code_hash = await sha256hex(code.replaceAll("-", ""));
    state.code_expires_at = new Date(Date.now() + 15 * 60 * 1000).toISOString();
    state.new_email = newEmail;
    state.attempts = 0;
    state.emails_sent = (state.emails_sent ?? 0) + 1;
    state.last_email_at = new Date().toISOString();
    await env.DIRECTORY.put(key, JSON.stringify(state), { expirationTtl: 24 * 3600 });
    // Alarm to the CURRENT address is REQUIRED — it is the only counter-move
    // channel for the stolen-token threat. A send failure must fail the
    // request; nothing has committed yet, and the caller can retry once
    // outbound mail recovers.
    if (account.email === newEmail) {
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
        text: `A request was made to move a Witself account away from this address.\n\nAccount: ${accountId}\nMoving to: ${newEmail}\n\nIf this was you — nothing to do. Confirm from the new inbox to complete the change.\n\nIf this was NOT you, treat it as a compromise: someone else holds a working operator token for your account. Run this at your terminal right now — it rotates the owner credentials and stops the change:\n\n  ws account recover\n`,
        html: renderEmail({
          title: "Your account email is being changed",
          preheader: "Not you? Someone holds your credentials — run ws account recover now.",
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
            ${cliBlock("ws account recover")}
          `,
        }),
      });
    } catch (e) {
      console.log(`change-email: notice to ${accountId}'s old address failed: ${e}`);
      return err("could not deliver the alarm to your current address — try again shortly", 502);
    }
    await logCellEvent(cell, accountId, "account.email.change.sent",
      "control_plane", { to_masked: maskEmail(account.email) });
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

  const oldEmail = account.email;
  let resp;
  try {
    resp = await fetch(`${cell.endpoint}/v1/accounts/${accountId}:update-email`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({ operator_id: operatorID, new_email: newEmail }),
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
  // The anchor moved: any live recovery code was mailed to the OLD address,
  // which may now be compromised. Kill it. Rate-limit counters do NOT need
  // to survive an anchor move.
  await env.DIRECTORY.delete(`recover:${accountId}`);
  // Undo window: a link in the OLD inbox re-points the email for 48h,
  // matched by hash — the raw token is only ever known to the recipient.
  const undoRaw = new Uint8Array(32);
  crypto.getRandomValues(undoRaw);
  const undoTok = [...undoRaw].map((b) => b.toString(16).padStart(2, "0")).join("");
  const undoTtl = 48 * 3600;
  await env.DIRECTORY.put(
    `undoemail:${await sha256hex(undoTok)}`,
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
      text: `This is a confirmation: a Witself account has moved away from this address.\n\nAccount: ${accountId}\nNow at:  ${newEmail}\n\nIf the change was valid, no action is needed — this is the last email this address will receive for the account.\n\nIf the change was NOT valid, you can revert it. This link points the account back at this address and stays live for 48 hours:\n\n  ${undoLink}\n\nAfter reverting, run \`ws account recover\` at your terminal to rotate the owner credentials.\n`,
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
          <p style="margin:20px 0 0;color:${EMAIL_MUTED};font-size:14px;">After reverting, run <code style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;">ws account recover</code> at your terminal to rotate the owner credentials.</p>
        `,
      }),
    });
    await logCellEvent(cell, accountId, "account.email.undo.sent",
      "control_plane", { to_masked: maskEmail(oldEmail) });
  } catch (e) {
    console.log(`change-email: undo link to ${accountId}'s old address failed: ${e}`);
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
    return htmlPage(409, "Undo link is stale", "The account's email has changed again since this link was issued. If you didn't authorize either change, use <code>ws account recover</code> to rotate the owner credentials.");
  }
  if (!resp.ok || committed?.email !== undo.old_email) {
    return htmlPage(503, "Temporarily unavailable", "The revert couldn't be applied just now. Please try the link again in a few minutes.");
  }
  await env.DIRECTORY.delete(key);
  await env.DIRECTORY.delete(`recover:${undo.account_id}`);
  return htmlPage(200, "Email change reverted", `The account's email is back to <code>${undo.old_email}</code>. Run <code>ws account recover</code> from your terminal now to rotate the owner credentials.`);
}

// handleClose is POST /v1/accounts/{id}:close — the symmetric exit to signup's
// entrance: born through the front door, closed through the front door. The
// control plane holds no authority here — it forwards the caller's operator
// token to the account's cell, which decides (owner-only, tombstone, revoke).
// Only on the cell's success does the control plane forget its routing pointer.
async function handleClose(request, env, accountId) {
  const auth = request.headers.get("Authorization");
  if (!auth) {
    return err("operator token required", 401);
  }
  const key = `acct:${accountId}`;
  const entry = await env.DIRECTORY.get(key, { type: "json" });
  if (!entry) {
    return err("unknown account", 404);
  }
  let body = "{}";
  try {
    body = await request.text();
  } catch {
    // keep the empty default
  }
  let cellResp;
  try {
    cellResp = await fetch(`${entry.endpoint}/v1/account:close`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: auth },
      body: body || "{}",
      signal: AbortSignal.timeout(15000),
    });
  } catch {
    return err(`cell ${entry.cell} unreachable — try again shortly`, 502);
  }
  if (!cellResp.ok) {
    // Pass the cell's refusal (401/403/...) through verbatim.
    const text = await cellResp.text();
    return new Response(text, {
      status: cellResp.status,
      headers: { "Content-Type": "application/json" },
    });
  }
  await env.DIRECTORY.delete(key); // the fleet forgets; the cell remembers
  await env.DIRECTORY.delete(`pending:${accountId}`); // no longer a reap candidate
  return json({
    schema_version: "witself.v0",
    account_id: accountId,
    status: "closed",
  });
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

// evacuateAccount runs the four-step evacuation for a single account. The
// R2 object key is DETERMINISTIC per account (not per attempt) — a retry
// after any failure just overwrites, so no orphaned objects can accrete.
// Multipart upload atomically finalizes the object only on complete(), so a
// truncated stream aborts the upload and nothing is committed at the key.
async function evacuateAccount(env, cellName, cell, accountId) {
  // Idempotency short-circuit: if an archived: pointer already exists we
  // just need to ensure acct: is retired (a failure between steps 3 and 4
  // on a prior call is what leaves that gap). No R2 work — the archive is
  // already there.
  const already = await env.DIRECTORY.get(`archived:${accountId}`, {
    type: "json",
  });
  if (already) {
    await env.DIRECTORY.delete(`acct:${accountId}`);
    return;
  }

  // (1) System-suspend under the "evacuation" category. Owner-suspended
  // accounts keep their original category; closed tombstones are idempotent.
  //
  // Pending accounts — a signup landed on this cell moments before drain —
  // refuse suspension with 409 "pending — not suspendable" (the cell's
  // SuspendAccountSystem allows only active/suspended/closed). Without a
  // rescue path, one late signup would wedge every teardown. Mirror the
  // reaper: POST :reap, verify the acknowledged tombstone, retire pending:
  // and acct:, and skip the archive step (incomplete signups die with the
  // cell, same policy as the pending-expiry sweep at reapExpiredPendings).
  const suspendResp = await fetch(
    `${cell.endpoint}/v1/accounts/${accountId}:suspend`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({ for: "evacuation", reason: "cell decommission" }),
      signal: AbortSignal.timeout(15000),
    },
  );
  if (!suspendResp.ok) {
    const suspendBody = await suspendResp.text().catch(() => "");
    if (suspendResp.status === 409 && suspendBody.includes("pending")) {
      await reapPendingDuringEvacuation(env, cell, accountId);
      return { reaped: true };
    }
    throw new Error(`suspend ${suspendResp.status}: ${suspendBody.slice(0, 200)}`);
  }
  await suspendResp.text().catch(() => "");

  // (2) Stream the archive from the cell into R2 as a MULTIPART upload:
  // - Streaming exports have no Content-Length; single-shot put() rejects.
  // - Multipart finalizes atomically only on complete() — a truncated
  //   stream (cell errors mid-response, connection reset) throws when
  //   reading the body, we hit the catch and abort() the upload, and
  //   nothing lands at the key.
  const exportResp = await fetch(
    `${cell.endpoint}/v1/accounts/${accountId}:export`,
    {
      method: "POST",
      headers: { Authorization: `Bearer ${cell.provision_token}` },
      signal: AbortSignal.timeout(300000), // 5 min per-account ceiling
    },
  );
  if (!exportResp.ok || !exportResp.body) {
    const text = await exportResp.text().catch(() => "");
    throw new Error(`export ${exportResp.status}: ${text.slice(0, 200)}`);
  }
  const objectKey = `archives/${accountId}.tar.gz`;
  const nowISO = new Date().toISOString();
  const size = await streamToR2Multipart(env.ARCHIVES, objectKey, exportResp.body, {
    httpMetadata: {
      contentType: "application/gzip",
      contentDisposition: `attachment; filename="${accountId}.tar.gz"`,
    },
    customMetadata: {
      account_id: accountId,
      cell: cellName,
      exported_at: nowISO,
    },
  });

  // (3) Write the archived: pointer BEFORE removing acct:, so a directory
  // read never briefly returns 404 for the archived account.
  await env.DIRECTORY.put(
    `archived:${accountId}`,
    JSON.stringify({
      cell: cellName,
      region: cell.region ?? null,
      object: objectKey,
      exported_at: nowISO,
      size,
      format_version: 1,
    }),
  );

  // (3a) Audit the evacuation ONCE per successful attempt. Firing after
  // archived: is written (step 3) means:
  // - a retry after a mid-export failure short-circuits at the top of
  //   the function (archived: exists → return without re-logging)
  // - the event lands durably on the cell before step 4 retires
  //   routing, and before the cell is eventually torn down
  // The event does NOT survive the arc (the archive was streamed to
  // R2 in step 2 before this row existed) — that's fine, because the
  // matching account.restored event on the new cell is what carries
  // the migration signal for owners viewing history post-restore.
  // actor_kind=system: fleet-level operation, not a principal action.
  await logCellEvent(cell, accountId, "account.evacuated",
    "system", { cell: cellName });

  // (4) Retire the routing pointer. From this moment the account is
  // "archived — awaiting placement" fleet-wide.
  await env.DIRECTORY.delete(`acct:${accountId}`);
}

// handleRestore is POST /v1/cells/{name}:restore — the mirror of :evacuate.
// Iterates archived: pointers whose region matches the target cell's, and
// for a bounded batch streams the R2 object into the cell's :import, calls
// :resume, writes the new acct: pointer, then deletes both the archived:
// KV entry and the R2 object. Each of the four steps is individually
// re-safe (ready-check-then-act, deterministic keys), so a partial restore
// resumes cleanly on the next call. Refuses to restore into a drained cell
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
  if (cell.accepting === false) {
    return err("cell is drained (accepting=false) — cannot restore into it", 409);
  }
  if (!cell.provision_token || !cell.endpoint) {
    return err(`cell ${cellName} has no provision credential — cannot import`, 502);
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

  // Iterate archived: pointers, region-matching the target cell. Region is
  // the user-facing placement axis: an archived account waits in R2 until a
  // cell in its region can host it, so a us-west-2 archive never silently
  // lands in eu-central-1.
  const targets = [];
  let cursor;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.region === cell.region) {
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

  const progressKey = `restore:${cellName}`;
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

  // Count region-matched archived: pointers still awaiting placement.
  // witself-infra loops until this reaches zero.
  let remaining = 0;
  let cursor2;
  do {
    const page = await env.DIRECTORY.list({ prefix: "archived:", cursor: cursor2 });
    for (const k of page.keys) {
      const entry = await env.DIRECTORY.get(k.name, { type: "json" });
      if (entry?.region === cell.region) {
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
    region: cell.region,
    restored: results,
    remaining,
    progress,
  });
}

// restoreAccount runs the four-step restore for a single account. Each step
// is idempotent under retry, and the ordering — import THEN resume THEN
// route THEN clean — guarantees the account is never simultaneously
// discoverable at two cells. Terminology mirrors evacuateAccount.
//
// Cross-cell race: two accepting cells in the same region CAN be targeted
// simultaneously by two :restore callers (a driver plus a manual retry, two
// operators, etc.). Both would list the same archived: pointer, both would
// see acct: null, both would import into DIFFERENT cells — different
// databases return no 409 collision — and last-writer-wins on the acct:
// KV would leave the losing cell holding a ghost copy of the account with
// no directory pointer. To prevent that, we take a `restoring:<accountId>`
// claim up front and re-check acct: immediately before writing it. KV has
// no CAS, so the defense is layered rather than atomic: the claim shrinks
// the race window from indefinite to milliseconds, and the pre-put
// re-check catches the residual case (throwing instead of silently
// overwriting).
async function restoreAccount(env, cellName, cell, accountId, archived) {
  // Idempotency short-circuit: if an acct: pointer already names this cell
  // for this account, a prior restore either finished or died between steps
  // 3 and 4. Either way, ensure R2 + archived: are gone and stop.
  const routed = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (routed?.cell === cellName) {
    try {
      await env.ARCHIVES.delete(archived.object);
    } catch {
      // ignore — R2 delete of a missing key is a 204
    }
    await env.DIRECTORY.delete(`archived:${accountId}`);
    await env.DIRECTORY.delete(`restoring:${accountId}`);
    return;
  }
  if (routed && routed.cell !== cellName) {
    throw new Error(
      `acct:${accountId} already routes to ${routed.cell} — refusing to route to ${cellName}`,
    );
  }

  // Cross-cell claim: if another cell has an active restore in flight for
  // this account, back off. TTL bounds how long a dead Worker can strand
  // the claim (15 min covers the 5-min :import timeout plus retries).
  const claimKey = `restoring:${accountId}`;
  const existingClaim = await env.DIRECTORY.get(claimKey, { type: "json" });
  if (existingClaim && existingClaim.cell !== cellName) {
    throw new Error(
      `${accountId} is already being restored to ${existingClaim.cell} (since ${existingClaim.started_at}) — skipping`,
    );
  }
  await env.DIRECTORY.put(
    claimKey,
    JSON.stringify({ cell: cellName, started_at: new Date().toISOString() }),
    { expirationTtl: 900 },
  );
  // Re-read to catch a same-instant twin claim from another cell. KV is
  // eventually consistent, so this doesn't close the window completely,
  // but a concurrent claim's write is likely to be visible within a
  // handful of ms — small enough to make the residual race rare, and the
  // pre-put acct: re-check below catches whatever slips through.
  const reclaim = await env.DIRECTORY.get(claimKey, { type: "json" });
  if (reclaim && reclaim.cell !== cellName) {
    throw new Error(
      `${accountId} was claimed by ${reclaim.cell} while we were writing our claim — backing off`,
    );
  }

  // (1) Stream the R2 archive into the target cell's :import. The archive
  // is committed in a single transaction on the cell side (see
  // internal/store/import.go) — a mid-stream failure leaves the cell
  // untouched, and the whole import re-runs on the next call.
  const obj = await env.ARCHIVES.get(archived.object);
  if (!obj || !obj.body) {
    throw new Error(`archive ${archived.object} not in R2`);
  }
  const importResp = await fetch(
    `${cell.endpoint}/v1/accounts/${accountId}:import`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${cell.provision_token}`,
        "Content-Type": "application/octet-stream",
      },
      body: obj.body,
      signal: AbortSignal.timeout(300000), // 5 min per-account ceiling
    },
  );
  if (!importResp.ok) {
    // 409 with body "account already exists on this cell" means a prior
    // attempt succeeded but died before step 3 — treat as success and
    // continue to routing/cleanup. Anything else is a real failure.
    const text = await importResp.text().catch(() => "");
    if (importResp.status !== 409 || !text.includes("already exists")) {
      throw new Error(`import ${importResp.status}: ${text.slice(0, 200)}`);
    }
  } else {
    await importResp.text().catch(() => "");
  }

  // (2) Lift the evacuation suspension. Owner-suspended accounts stay
  // owner-suspended — ResumeAccountSystem's category scoping guarantees the
  // authority that suspended is the authority that resumes.
  const resumeResp = await fetch(
    `${cell.endpoint}/v1/accounts/${accountId}:resume`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${cell.provision_token}`,
      },
      body: JSON.stringify({ for: "evacuation" }),
      signal: AbortSignal.timeout(15000),
    },
  );
  if (!resumeResp.ok) {
    // 409 "account is not suspended" means the account came back
    // suspended for something else (owner_request) — legitimate; the
    // owner will lift it. 409 "category does not match" is the same
    // reality with a more specific message. Either way, do not fail.
    const text = await resumeResp.text().catch(() => "");
    const benign =
      resumeResp.status === 409 &&
      (text.includes("not suspended") || text.includes("category"));
    if (!benign) {
      throw new Error(`resume ${resumeResp.status}: ${text.slice(0, 200)}`);
    }
  } else {
    await resumeResp.text().catch(() => "");
  }

  // (3) Write the acct: pointer BEFORE deleting archived:, so a directory
  // lookup during the tiny gap between the two KV writes always finds
  // SOMETHING — never a false 404 for an account we already restored.
  //
  // Cross-cell race defense: re-read acct: immediately before writing. If
  // another cell won the race between our claim and this moment (the
  // claim's re-read isn't a real mutex), we imported successfully but our
  // data on this cell is now a ghost copy. Refuse to overwrite the
  // winner's routing pointer — the account correctly serves from the
  // winning cell; the ghost rows on this cell need manual removal.
  // Recovery is documented at docs/runbooks.md#clean-up-a-ghost-restore.
  const finalCheck = await env.DIRECTORY.get(`acct:${accountId}`, { type: "json" });
  if (finalCheck && finalCheck.cell !== cellName) {
    throw new Error(
      `restore race: ${accountId} routes to ${finalCheck.cell} — imported rows on ${cellName} are a ghost; see docs/runbooks.md#clean-up-a-ghost-restore`,
    );
  }
  await env.DIRECTORY.put(
    `acct:${accountId}`,
    JSON.stringify({
      cell: cellName,
      endpoint: cell.endpoint,
      region: cell.region,
    }),
  );

  // Audit the arrival on the new cell's ledger. account.evacuated was
  // preserved through the archive (streamed in during :import), so an
  // owner reading their trail after restore sees the evacuation and
  // restore as a matched pair. actor_kind=system: fleet-level operation,
  // not a principal action. archived.cell names the source cell.
  await logCellEvent(cell, accountId, "account.restored",
    "system", { from_cell: archived.cell });

  // (4) Retire the archived: pointer, then delete the R2 object. Order
  // matters for observability: a directory reader that sees archived: gone
  // but the R2 key still present would be transiently confusing, but a
  // reader that sees archived: still present after the R2 key is gone
  // would follow a dead pointer — much worse.
  await env.DIRECTORY.delete(`archived:${accountId}`);
  try {
    await env.ARCHIVES.delete(archived.object);
  } catch {
    // ignore — R2 delete of a missing key is a 204; a genuine R2 outage
    // just leaves an orphan we can sweep later, doesn't roll back the
    // restore.
  }
  await env.DIRECTORY.delete(claimKey);
}

// reapPendingDuringEvacuation is the pending-account escape hatch inside an
// evacuation batch. Mirrors reapExpiredPendings' contract exactly: only a
// {status:"closed", account_id:<id>} acknowledgement is accepted as proof
// the cell reaped the account, so a stray 2xx (LB default page, captive
// portal) can't destroy live routing. Deletes pending: AND acct: on
// success, then returns; the caller records reaped=true and does not
// write an archive — incomplete signups die with the cell.
async function reapPendingDuringEvacuation(env, cell, accountId) {
  const reapResp = await fetch(
    `${cell.endpoint}/v1/accounts/${accountId}:reap`,
    {
      method: "POST",
      headers: { Authorization: `Bearer ${cell.provision_token}` },
      signal: AbortSignal.timeout(15000),
    },
  );
  const bodyText = await reapResp.text().catch(() => "");
  if (!reapResp.ok) {
    throw new Error(`reap-pending ${reapResp.status}: ${bodyText.slice(0, 200)}`);
  }
  let body = null;
  try {
    body = JSON.parse(bodyText);
  } catch {
    // ignore
  }
  if (body?.status !== "closed" || body?.account_id !== accountId) {
    throw new Error(
      `reap-pending returned 2xx without a valid acknowledgement for ${accountId}`,
    );
  }
  await env.DIRECTORY.delete(`pending:${accountId}`);
  await env.DIRECTORY.delete(`acct:${accountId}`);
  console.log(`evacuate: reaped pending ${accountId} on ${cell.name || cell.endpoint}`);
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

// streamToR2Multipart pipes an unknown-length ReadableStream into an R2
// object using multipart uploads. It's the workhorse for the "no
// Content-Length on cell exports" reality: R2's single-shot put() rejects
// streams without a length, but createMultipartUpload streams cleanly.
//
// Any failure — read error mid-stream (truncation), R2 uploadPart error,
// complete error — aborts the upload, so no object is committed at the key
// until we know the whole stream landed. Part size 8 MiB (R2 minimum is 5
// MiB except last).
async function streamToR2Multipart(bucket, key, stream, opts) {
  const upload = await bucket.createMultipartUpload(key, opts);
  const parts = [];
  const partSize = 8 * 1024 * 1024;
  let totalBytes = 0;
  try {
    const reader = stream.getReader();
    let buf = new Uint8Array(0);
    let partNo = 1;
    while (true) {
      const { done, value } = await reader.read();
      if (value && value.length > 0) {
        const combined = new Uint8Array(buf.length + value.length);
        combined.set(buf, 0);
        combined.set(value, buf.length);
        buf = combined;
      }
      // Emit parts as we cross the threshold. A final under-sized part is
      // handled after the loop.
      while (buf.length >= partSize) {
        const chunk = buf.slice(0, partSize);
        buf = buf.slice(partSize);
        parts.push(await upload.uploadPart(partNo, chunk));
        totalBytes += chunk.length;
        partNo++;
      }
      if (done) break;
    }
    if (buf.length > 0) {
      parts.push(await upload.uploadPart(partNo, buf));
      totalBytes += buf.length;
    }
    if (parts.length === 0) {
      throw new Error("export stream was empty");
    }
    await upload.complete(parts);
    return totalBytes;
  } catch (e) {
    try {
      await upload.abort();
    } catch {
      // ignore — R2 auto-cleans abandoned multipart uploads
    }
    throw e;
  }
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

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
      ADMIN_ACCOUNT_SUPPORT_POLICY_PATH.test(url.pathname)
    ) {
      return handleAdminTickets(request, env, ctx, url);
    }

    // Fleet-wide placement strategy (fleet-token authorized).
    if (url.pathname === "/v1/placement") {
      return handlePlacement(request, env);
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

    // Cold path: the Go container.
    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },

  // Cron: the pending-account expiry sweep (see reapExpiredPendings).
  async scheduled(_event, env, ctx) {
    ctx.waitUntil(reapExpiredPendings(env));
  },
};
