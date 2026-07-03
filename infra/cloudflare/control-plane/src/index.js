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
  const greeting = opts.resend
    ? "Here is a fresh verification link for your Witself account."
    : "Welcome to Witself.";
  const greetingHTML = opts.resend
    ? "Here is a fresh verification link for your <strong>Witself</strong> account."
    : "Welcome to <strong>Witself</strong>.";
  await env.EMAIL.send({
    to: email,
    from: "no-reply@witwave.ai",
    subject: "Verify your Witself account",
    text: `${greeting}\n\nVerify your account (${accountId}) by opening this link:\n\n${link}\n\n${deadline} If you didn't sign up, ignore this email.\n`,
    html: `<p>${greetingHTML}</p><p>Verify your account (<code>${accountId}</code>) by clicking the link below:</p><p><a href="${link}">Verify my account</a></p><p>${deadline} If you didn't sign up, ignore this email.</p>`,
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
        await env.EMAIL.send({
          to: contact.email,
          from: "no-reply@witwave.ai",
          subject: "Your Witself recovery code",
          text: `A recovery was requested for your Witself account (${accountId}).\n\nRecovery code: ${code}\n\nIt expires in 15 minutes. Redeem it with:\n\n  ws account recover --id ${accountId} --code ${code}\n\nIf you didn't request this, ignore this email — nothing changes until the code is used.\n`,
          html: `<p>A recovery was requested for your <strong>Witself</strong> account (<code>${accountId}</code>).</p><p>Recovery code: <strong>${code}</strong></p><p>It expires in 15 minutes. Redeem it with:</p><pre>ws account recover --id ${accountId} --code ${code}</pre><p>If you didn't request this, ignore this email — nothing changes until the code is used.</p>`,
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
      await env.EMAIL.send({
        to: newEmail,
        from: "no-reply@witwave.ai",
        subject: "Confirm your new Witself account email",
        text: `A request was made to move Witself account ${accountId} to this address.\n\nConfirmation code: ${code}\n\nIt expires in 15 minutes. Confirm with:\n\n  ws account change-email --new-email ${newEmail} --code ${code}\n\nIf you don't recognize this, ignore this email.\n`,
        html: `<p>A request was made to move <strong>Witself</strong> account <code>${accountId}</code> to this address.</p><p>Confirmation code: <strong>${code}</strong></p><p>It expires in 15 minutes. Confirm with:</p><pre>ws account change-email --new-email ${newEmail} --code ${code}</pre><p>If you don't recognize this, ignore this email.</p>`,
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
        subject: "Your Witself account email is being changed",
        text: `A request was made to move Witself account ${accountId} from this address to ${newEmail}.\n\nIf this was you, nothing to do — confirm from the new inbox. If it was NOT you, someone may hold your operator token: run \`ws account recover\` right now to rotate the owner credentials before the change commits. After the change commits, an undo link stays live for 48 hours (delivered separately).\n`,
        html: `<p>A request was made to move <strong>Witself</strong> account <code>${accountId}</code> from this address to <code>${newEmail}</code>.</p><p>If this was you, nothing to do — confirm from the new inbox. If it was <strong>not</strong> you, someone may hold your operator token: run <code>ws account recover</code> right now to rotate the owner credentials before the change commits. After the change commits, an undo link stays live for 48 hours (delivered separately).</p>`,
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
      text: `Witself account ${accountId} was moved from this address to ${newEmail}.\n\nIf this was NOT you: open the link below within 48 hours to revert the change and re-point the account back to this address. After reverting, run \`ws account recover\` from your terminal to rotate the owner credentials.\n\n  ${undoLink}\n`,
      html: `<p><strong>Witself</strong> account <code>${accountId}</code> was moved from this address to <code>${newEmail}</code>.</p><p>If this was <strong>not</strong> you: open the link below within 48 hours to revert the change and re-point the account back to this address. After reverting, run <code>ws account recover</code> from your terminal to rotate the owner credentials.</p><p><a href="${undoLink}">Revert the email change</a></p>`,
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

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // Hot path: directory lookups from KV, never the container.
    const m = url.pathname.match(DIRECTORY_PATH);
    if (m) {
      if (request.method !== "GET") {
        return err("method not allowed", 405);
      }
      const entry = await env.DIRECTORY.get(`acct:${m[1]}`, {
        type: "json",
        cacheTtl: 300,
      });
      if (!entry) {
        return err("unknown account", 404);
      }
      return json(
        { schema_version: "witself.v0", account_id: m[1], cell: entry },
        200,
        { "Cache-Control": "max-age=60" },
      );
    }

    // Fleet registry (fleet-token authorized).
    if (
      url.pathname === "/v1/cells" ||
      CELL_PATH.test(url.pathname) ||
      PURGE_PATH.test(url.pathname)
    ) {
      return handleCells(request, env, url);
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
