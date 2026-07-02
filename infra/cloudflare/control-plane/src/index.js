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
// KV schema (v0, one namespace, three prefixes):
//   acct:<account_id> -> {"cell":"<name>","endpoint":"https://..."}
//   cell:<name>       -> {"endpoint","cloud","region","owner","weight",
//                          "accepting","registered_at"}
//   invite:<code>     -> {"enabled","not_before","expires_at","max_uses",
//                          "uses","note","created_at","cell","region"}
//   config:placement  -> {"strategy":"weighted"|"pinned","pinned_cell"}
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
const ACCOUNT_CLOSE_PATH = /^\/v1\/accounts\/([A-Za-z0-9_-]{1,128}):close$/;
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
    await env.DIRECTORY.delete(key);
    return new Response(null, { status: 204 });
  }

  return err("method not allowed", 405);
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
  if (!provisioned?.account_id || !provisioned?.bootstrap_token) {
    return err("cell returned an invalid provisioning response", 502);
  }

  // Routing pointer + best-effort invite consumption (exact counting arrives
  // with the DO authority).
  await env.DIRECTORY.put(
    `acct:${provisioned.account_id}`,
    JSON.stringify({ cell: cell.name, endpoint: cell.endpoint }),
  );
  const fresh = (await env.DIRECTORY.get(inviteKey, { type: "json" })) ?? invite;
  fresh.uses = (fresh.uses ?? 0) + 1;
  await env.DIRECTORY.put(inviteKey, JSON.stringify(fresh));

  return json(
    {
      schema_version: "witself.v0",
      account_id: provisioned.account_id,
      operator_id: provisioned.operator_id,
      email: provisioned.email,
      status: provisioned.status,
      cell: { name: cell.name, endpoint: cell.endpoint },
      bootstrap_token: provisioned.bootstrap_token,
    },
    201,
  );
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
  return json({
    schema_version: "witself.v0",
    account_id: accountId,
    status: "closed",
  });
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

    // Cold path: the Go container.
    return getContainer(env.CONTROL_PLANE, "singleton").fetch(request);
  },
};
