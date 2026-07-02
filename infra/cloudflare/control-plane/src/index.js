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
  await env.DIRECTORY.put(
    `pending:${provisioned.account_id}`,
    JSON.stringify({ cell: cell.name, created_at: new Date().toISOString() }),
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
  // pattern as invite uses; the DO authority tightens both later), and it
  // FAILS CLOSED: no candidate entry, no resend.
  const pendingKey = `pending:${accountId}`;
  const pending = await env.DIRECTORY.get(pendingKey, { type: "json" });
  if (!pending) {
    return err("verification state unavailable — try again shortly", 503);
  }
  const sent = pending.emails_sent ?? 1; // signup's email is the first
  if (sent >= 5) {
    return err("too many verification emails for this account — it closes unverified at the end of its window", 429);
  }
  if (pending.last_email_at && Date.now() - Date.parse(pending.last_email_at) < 2 * 60 * 1000) {
    return err("a verification email was just sent — wait a couple of minutes", 429);
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

    // Cold path: the Go container.
    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },

  // Cron: the pending-account expiry sweep (see reapExpiredPendings).
  async scheduled(_event, env, ctx) {
    ctx.waitUntil(reapExpiredPendings(env));
  },
};
