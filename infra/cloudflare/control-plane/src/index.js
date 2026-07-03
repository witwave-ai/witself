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
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const INVITE_PATH = /^\/v1\/invites\/([a-z0-9][a-z0-9-]{2,63})$/;
const INVITE_CODE = /^[a-z0-9][a-z0-9-]{2,63}$/;
const REGION_NAME = /^[a-z0-9-]{2,32}$/;
const PLACEMENT_STRATEGIES = ["weighted", "pinned"];

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
  const rlKey = `recover:${accountId}`;
  const rl = (await env.DIRECTORY.get(rlKey, { type: "json" })) ?? {};

  if (!body.code) {
    // ---- Request mode: maybe send a code; always answer the same. ----
    // The generic answer must be indistinguishable across (phantom id, real
    // id, cell down, email backend down) — in both response BODY and, as
    // best a Worker can, response LATENCY. That means the cell round trip
    // happens for phantom ids too, and rate-limit state advances only when
    // an email actually left. The 429 cases are internal signals to a
    // legitimate owner and use plain 429s that phantom ids never trigger.
    const generic = () =>
      json({
        schema_version: "witself.v0",
        message: "if the account exists and is active, a recovery code was emailed",
      });
    if ((rl.emails_sent ?? 0) >= 5) {
      return err("too many recovery requests for this account — try again tomorrow", 429);
    }
    if (rl.last_email_at && Date.now() - Date.parse(rl.last_email_at) < 2 * 60 * 1000) {
      return err("a recovery code was just sent — wait a couple of minutes", 429);
    }

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
        // Persist the fresh code only after successful send — otherwise a
        // failed email would corrupt an already-issued code AND burn a
        // quota slot on a real owner during a mail outage.
        rl.code_hash = await sha256hex(code.replaceAll("-", ""));
        rl.code_expires_at = new Date(Date.now() + 15 * 60 * 1000).toISOString();
        rl.attempts = 0;
        rl.emails_sent = (rl.emails_sent ?? 0) + 1;
        rl.last_email_at = new Date().toISOString();
        sent = true;
      } catch (e) {
        console.log(`recover: email for ${accountId} failed: ${e}`);
      }
    }
    if (sent) {
      await env.DIRECTORY.put(rlKey, JSON.stringify(rl), { expirationTtl: 24 * 3600 });
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
      await evacuateAccount(env, cellName, cell, accountId);
      progress.done += 1;
      results.push({ account_id: accountId, ok: true });
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
    throw new Error(`suspend ${suspendResp.status}`);
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

  // (4) Retire the routing pointer. From this moment the account is
  // "archived — awaiting placement" fleet-wide.
  await env.DIRECTORY.delete(`acct:${accountId}`);
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
  async fetch(request, env) {
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

    // Invite codes (fleet-token authorized).
    if (url.pathname === "/v1/invites" || INVITE_PATH.test(url.pathname)) {
      return handleInvites(request, env, url);
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
