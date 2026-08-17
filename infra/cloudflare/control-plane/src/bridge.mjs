// Production trust bridge between the Cloudflare Worker, the Go control-plane
// container, and cells.
//
// There are two deliberately different credentials:
//   * witself_adm_* is presented by a human administrator and is verified by
//     the Worker against DIRECTORY. It never crosses the Worker boundary.
//   * INTERNAL_BRIDGE_TOKEN is shared only by this Worker and the Go
//     control-plane container. The Worker uses it when proxying an
//     authenticated admin request to Go; Go uses it for the internal
//     directory/apply callbacks below.
//
// Go contract:
//   external proxy:
//     GET|PUT|DELETE /v1/admin/accounts/{id}/transcript-retention
//     GET|PUT|DELETE /v1/admin/accounts/{id}/messaging
//     GET|PUT|DELETE /v1/admin/accounts/{id}/message-retention
//     GET|PUT|DELETE /v1/admin/accounts/{id}/email-receive
//     GET|PUT|DELETE /v1/admin/accounts/{id}/email-send
//     GET|PUT|DELETE /v1/admin/accounts/{id}/email-retention
//     GET|PUT|DELETE /v1/admin/accounts/{id}/plan-override
//     GET|PUT|DELETE /v1/admin/accounts/{id}/limit-overrides/{dimension}
//     Authorization: Bearer $INTERNAL_BRIDGE_TOKEN
//     X-Witself-Admin-ID: <Worker-verified immutable admin id>
//     X-Witself-Admin-Handle: <Worker-verified non-secret handle>
//   callbacks:
//     GET  /v1/internal/accounts?cursor=&limit=
//     GET  /v1/internal/accounts/{id}:resolve
//          -> active account id, cell name, and non-secret HTTPS endpoint
//             (never the cell provision token)
//     GET|POST /v1/internal/accounts/{id}:apply-plan
//       GET reads only the current cell fence; POST applies a snapshot.
//     POST /v1/internal/accounts/{id}:plan-fit
//       read-only target comparison; combines cell usage with global email
//       alias/domain allocation authorities.
//     Authorization: Bearer $INTERNAL_BRIDGE_TOKEN
//
// The callback paths terminate at the Worker. They must never fall through to
// the container, or a configuration error could turn a callback into a loop.

import {
  reconcileRealmEmailAliasesForPlan,
  readRealmEmailAliasPlanFit,
} from "./realm-email-alias-runtime.mjs";
import {
  reconcileAgentEmailDomainsForPlan,
  readAgentEmailDomainPlanFit,
} from "./agent-email-domain-runtime.mjs";

const ACCOUNT_ID_PATTERN = "[A-Za-z0-9_-]{1,128}";
const LIMIT_DIMENSION_PATTERN =
  "(?:realms|agents|agents_per_realm|stored_memory|stored_fact|stored_secret|" +
  "agent_email_max_raw_bytes|agent_email_attachment_storage_bytes|" +
  "agent_email_realm_aliases_per_realm|" +
  "agent_email_custom_domains_per_account|" +
  "agent_email_sent_per_agent_minute|" +
  "agent_email_sent_per_realm_minute|" +
  "message_sent_per_agent_minute|message_delivered_per_realm_minute|" +
  "message_delivered_per_recipient_minute|" +
  "agent_email_received_per_sender_minute|" +
  "agent_email_received_per_recipient_minute|" +
  "agent_email_received_per_realm_minute|" +
  "agent_email_received_bytes_per_sender_minute|" +
  "agent_email_received_bytes_per_recipient_minute|" +
  "agent_email_received_bytes_per_realm_minute)";
const ADMIN_POLICY_PATH = new RegExp(
  `^/v1/admin/accounts/(${ACCOUNT_ID_PATTERN})/(?:transcript-retention|messaging|message-retention|email-receive|email-send|email-retention|plan-override|limit-overrides/${LIMIT_DIMENSION_PATTERN})$`,
);
const INTERNAL_RESOLVE_PATH = new RegExp(
  `^/v1/internal/accounts/(${ACCOUNT_ID_PATTERN}):resolve$`,
);
const INTERNAL_APPLY_PATH = new RegExp(
  `^/v1/internal/accounts/(${ACCOUNT_ID_PATTERN}):apply-plan$`,
);
const INTERNAL_FIT_PATH = new RegExp(
  `^/v1/internal/accounts/(${ACCOUNT_ID_PATTERN}):plan-fit$`,
);

const PLAN_FIT_ALIAS_LIMIT = "agent_email_realm_aliases_per_realm";
const PLAN_FIT_DOMAIN_LIMIT = "agent_email_custom_domains_per_account";
const PLAN_FIT_DIMENSION_ORDER = Object.freeze([
  "realms",
  "agents",
  "agents_per_realm",
  "stored_memory",
  "stored_fact",
  "stored_secret",
  "agent_email_attachment_storage_bytes",
  PLAN_FIT_ALIAS_LIMIT,
  PLAN_FIT_DOMAIN_LIMIT,
]);
const PLAN_FIT_DIMENSION_SCOPE = Object.freeze({
  realms: "account",
  agents: "account",
  agents_per_realm: "realm",
  stored_memory: "agent",
  stored_fact: "agent",
  stored_secret: "agent",
  agent_email_attachment_storage_bytes: "account",
  [PLAN_FIT_ALIAS_LIMIT]: "realm",
  [PLAN_FIT_DOMAIN_LIMIT]: "account",
});

export const ADMIN_ID_HEADER = "X-Witself-Admin-ID";
export const ADMIN_HANDLE_HEADER = "X-Witself-Admin-Handle";
export const BRIDGE_TIMEOUT_MS = 15_000;
export const BRIDGE_BODY_MAX_BYTES = 64 * 1024;
export const INTERNAL_ACCOUNTS_PATH = "/v1/internal/accounts";
export const INTERNAL_PATH_PREFIX = "/v1/internal/";
export const PLAN_LIFECYCLE_ACTIVATE_PATH =
  "/v1/internal/plan-lifecycle:activate";
export const PLAN_LIFECYCLE_CURSOR_KEY = "config:plan_lifecycle_cursor";
export const PLAN_LIFECYCLE_PAGE_SIZE = 100;

const PLAN_LIFECYCLE_TICK_PATH = "/v1/plan-lifecycle:tick";
const PLAN_LIFECYCLE_STATUS_PATH = "/v1/plan-lifecycle/status";
// Stay below the five-minute cron cadence while leaving Go's 3.5-minute page
// deadline enough time to return and have its acknowledgement validated.
const PLAN_LIFECYCLE_TICK_TIMEOUT_MS = 4 * 60 * 1000;
const PLAN_LIFECYCLE_ACTIVATE_TIMEOUT_MS = 2 * 60 * 1000;

// containerEnvVars is the sole Worker->container configuration projection.
// Values come from runtime Worker bindings (normally `wrangler secret put`);
// no secret is committed to wrangler.jsonc. Keep this an explicit allowlist:
// objects such as the ARCHIVES R2 binding and unrelated fleet/admin secrets
// must never be stringified or exposed to the container by accident.
export function containerEnvVars(env) {
  const vars = {
    WITSELF_CP_BRIDGE_URL:
      env.INTERNAL_BRIDGE_URL || "https://self.witwave.ai",
  };
  const mappings = [
    ["INTERNAL_BRIDGE_TOKEN", "WITSELF_CP_BRIDGE_TOKEN"],
    ["CP_PLAN_LIFECYCLE_ENABLED", "WITSELF_CP_PLAN_LIFECYCLE_ENABLED"],
    ["CP_R2_ENDPOINT", "WITSELF_CP_R2_ENDPOINT"],
    ["CP_R2_BUCKET", "WITSELF_CP_R2_BUCKET"],
    ["CP_R2_ACCESS_KEY", "WITSELF_CP_R2_ACCESS_KEY"],
    ["CP_R2_SECRET_KEY", "WITSELF_CP_R2_SECRET_KEY"],
    ["CP_R2_PREFIX", "WITSELF_CP_R2_PREFIX"],
    ["CP_BILLING_PROVIDER", "WITSELF_CP_BILLING_PROVIDER"],
    ["CP_STRIPE_SECRET_KEY", "WITSELF_CP_STRIPE_SECRET_KEY"],
    ["CP_STRIPE_WEBHOOK_SECRET", "WITSELF_CP_STRIPE_WEBHOOK_SECRET"],
    ["CP_STRIPE_SUCCESS_URL", "WITSELF_CP_STRIPE_SUCCESS_URL"],
    ["CP_STRIPE_CANCEL_URL", "WITSELF_CP_STRIPE_CANCEL_URL"],
  ];
  for (const [binding, variable] of mappings) {
    if (typeof env[binding] === "string" && env[binding] !== "") {
      vars[variable] = env[binding];
    }
  }
  return vars;
}

// restartContainerWithEnvironment is the testable core of Backend's activation
// RPC. Destroy is deliberate: Container applies env only on process start, so a
// secret-only Worker deployment cannot update an already-running Go process.
// The fresh allowlisted projection is never returned or logged.
export async function restartContainerWithEnvironment(container, envVars) {
  const freshEnv = { ...envVars };
  container.envVars = freshEnv;
  await container.destroy();
  await container.startAndWaitForPorts({
    ports: container.defaultPort,
    cancellationOptions: {
      abort: AbortSignal.timeout(PLAN_LIFECYCLE_ACTIVATE_TIMEOUT_MS),
      instanceGetTimeoutMS: PLAN_LIFECYCLE_ACTIVATE_TIMEOUT_MS,
      portReadyTimeoutMS: PLAN_LIFECYCLE_ACTIVATE_TIMEOUT_MS,
    },
    startOptions: { envVars: freshEnv },
  });
}

const json = (obj, status = 200) =>
  new Response(JSON.stringify(obj), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const err = (message, status) =>
  json({ schema_version: "witself.v0", error: message }, status);

function timingSafeEqual(a, b) {
  const enc = new TextEncoder();
  const aa = enc.encode(a);
  const bb = enc.encode(b);
  if (aa.byteLength !== bb.byteLength) return false;
  if (typeof crypto.subtle.timingSafeEqual === "function") {
    return crypto.subtle.timingSafeEqual(aa, bb);
  }
  // Node's WebCrypto lacks Cloudflare's timingSafeEqual extension. Keeping a
  // fixed-length XOR fallback makes this module directly unit-testable and
  // avoids a data-dependent early return in compatible runtimes.
  let different = 0;
  for (let i = 0; i < aa.byteLength; i += 1) {
    different |= aa[i] ^ bb[i];
  }
  return different === 0;
}

export function matchAdminPolicyPath(pathname) {
  return pathname.match(ADMIN_POLICY_PATH);
}

export function isInternalBridgePath(pathname) {
  return pathname === INTERNAL_ACCOUNTS_PATH ||
    pathname.startsWith(INTERNAL_PATH_PREFIX);
}

function internalBridgeAuthorized(request, env) {
  const configured = String(env.INTERNAL_BRIDGE_TOKEN ?? "");
  if (!configured) return false;
  const header = request.headers.get("Authorization") ?? "";
  if (!header.startsWith("Bearer ")) return false;
  return timingSafeEqual(header.slice(7).trim(), configured);
}

function planLifecycleEnabled(env) {
  return String(env.CP_PLAN_LIFECYCLE_ENABLED ?? "")
    .trim().toLowerCase() === "true";
}

async function activatePlanLifecycle(request, env, backendFactory) {
  if (request.method !== "POST") return err("method not allowed", 405);
  if (!planLifecycleEnabled(env)) {
    return err("plan lifecycle is not enabled", 409);
  }
  if (typeof backendFactory !== "function") {
    return err("control-plane container is unavailable", 503);
  }

  const token = String(env.INTERNAL_BRIDGE_TOKEN ?? "");
  const freshEnv = containerEnvVars(env);
  if (!token ||
      freshEnv.WITSELF_CP_BRIDGE_TOKEN !== token ||
      !planLifecycleEnabled({
        CP_PLAN_LIFECYCLE_ENABLED:
          freshEnv.WITSELF_CP_PLAN_LIFECYCLE_ENABLED,
      })) {
    return err("plan lifecycle activation is not configured", 503);
  }

  try {
    const backend = backendFactory();
    if (!backend ||
        typeof backend.restartWithEnvironment !== "function" ||
        typeof backend.fetch !== "function") {
      return err("control-plane container is unavailable", 503);
    }
    const restarted = await backend.restartWithEnvironment(freshEnv);
    if (restarted?.restarted !== true) {
      return err("plan lifecycle activation failed", 502);
    }

    const status = await backend.fetch(new Request(
      `http://control-plane.internal${PLAN_LIFECYCLE_STATUS_PATH}`,
      {
        headers: {
          Accept: "application/json",
          Authorization: `Bearer ${token}`,
        },
        signal: AbortSignal.timeout(PLAN_LIFECYCLE_ACTIVATE_TIMEOUT_MS),
      },
    ));
    if (!status.ok) {
      return err("plan lifecycle activation failed", 502);
    }
    const doc = await status.json();
    if (doc?.schema_version !== "witself.v0" ||
        doc?.plan_lifecycle?.enabled !== true) {
      return err("plan lifecycle activation failed", 502);
    }
  } catch {
    return err("plan lifecycle activation failed", 502);
  }

  return json({
    schema_version: "witself.v0",
    plan_lifecycle: {
      activated: true,
      enabled: true,
    },
  });
}

async function activeAccountRoute(env, accountID) {
  // Archived wins during the short evacuation overlap where both pointers may
  // be visible due to KV propagation. No policy mutation is allowed until the
  // account has one live home again.
  const archived = await env.DIRECTORY.get(`archived:${accountID}`, {
    type: "json",
  });
  if (archived) {
    return { response: err("account is archived — restore before plan actions", 409) };
  }
  const route = await env.DIRECTORY.get(`acct:${accountID}`, { type: "json" });
  if (!route?.cell) {
    return { response: err("unknown account", 404) };
  }
  return { route };
}

function forwardedHeaders(contentType, token, adminID, adminHandle) {
  const headers = new Headers({
    Authorization: `Bearer ${token}`,
    [ADMIN_ID_HEADER]: adminID,
    [ADMIN_HANDLE_HEADER]: adminHandle,
  });
  if (contentType) headers.set("Content-Type", contentType);
  return headers;
}

async function boundedBody(request) {
  if (request.method === "GET" || request.method === "HEAD") return null;
  const declared = Number(request.headers.get("Content-Length"));
  if (Number.isFinite(declared) && declared > BRIDGE_BODY_MAX_BYTES) {
    return { tooLarge: true };
  }
  const body = await request.arrayBuffer();
  if (body.byteLength > BRIDGE_BODY_MAX_BYTES) {
    return { tooLarge: true };
  }
  return { body };
}

function relay(response) {
  const headers = new Headers();
  for (const name of ["Content-Type", "Retry-After", "X-Request-ID"]) {
    const value = response.headers.get(name);
    if (value) headers.set(name, value);
  }
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

// forwardAdminPolicyRequest is called only after index.js has authenticated
// the caller's witself_adm_* token. It re-checks account liveness at the edge,
// strips every caller header, and presents only the internal bridge credential
// plus the verified immutable id and display handle to Go.
export async function forwardAdminPolicyRequest(
  request,
  env,
  admin,
  containerFetch,
) {
  const url = new URL(request.url);
  const match = matchAdminPolicyPath(url.pathname);
  if (!match) return err("not found", 404);
  if (!["GET", "PUT", "DELETE"].includes(request.method)) {
    return err("method not allowed", 405);
  }
  if (!env.INTERNAL_BRIDGE_TOKEN) {
    return err("internal bridge is not configured", 503);
  }
  const resolved = await activeAccountRoute(env, match[1]);
  if (resolved.response) return resolved.response;

  const bounded = await boundedBody(request);
  if (bounded?.tooLarge) return err("request body too large", 413);
  const target = new URL(request.url);
  target.protocol = "http:";
  target.host = "control-plane.internal";
  const init = {
    method: request.method,
    headers: forwardedHeaders(
      request.headers.get("Content-Type"),
      env.INTERNAL_BRIDGE_TOKEN,
      admin.admin_id,
      admin.handle,
    ),
    signal: AbortSignal.timeout(BRIDGE_TIMEOUT_MS),
  };
  if (bounded?.body != null) init.body = bounded.body;

  try {
    return relay(await containerFetch(new Request(target, init)));
  } catch (cause) {
    const detail = cause?.name === "TimeoutError" ? "timed out" : "unreachable";
    return err(`control-plane container ${detail}`, 502);
  }
}

function parseAccountListQuery(url) {
  const rawLimit = url.searchParams.get("limit");
  const limit = rawLimit == null ? 100 : Number(rawLimit);
  if (!Number.isInteger(limit) || limit < 1 || limit > 500) {
    return { error: err("limit must be an integer from 1 through 500", 400) };
  }
  const cursor = url.searchParams.get("cursor") ?? undefined;
  if (cursor && cursor.length > 2048) {
    return { error: err("cursor is too long", 400) };
  }
  return { limit, cursor };
}

export async function activeAccountPage(env, limit, cursor) {
  const page = await env.DIRECTORY.list({
    prefix: "acct:",
    limit,
    ...(cursor ? { cursor } : {}),
  });
  const candidates = page.keys.map((key) => key.name.slice("acct:".length));
  const checks = await Promise.all(
    candidates.map(async (accountID) => {
      const [route, pending, archived] = await Promise.all([
        env.DIRECTORY.get(`acct:${accountID}`, { type: "json" }),
        env.DIRECTORY.get(`pending:${accountID}`),
        env.DIRECTORY.get(`archived:${accountID}`),
      ]);
      return route?.cell && !pending && !archived ? accountID : null;
    }),
  );
  const nextCursor = page.list_complete ? null : page.cursor;
  if (nextCursor != null &&
      (typeof nextCursor !== "string" || nextCursor === "" ||
       nextCursor.length > 2048 || nextCursor === cursor)) {
    throw new Error("directory returned an invalid cursor");
  }
  return {
    account_ids: checks.filter(Boolean),
    next_cursor: nextCursor,
  };
}

async function listActiveAccountIDs(env, url) {
  const parsed = parseAccountListQuery(url);
  if (parsed.error) return parsed.error;

  return json({
    schema_version: "witself.v0",
    ...await activeAccountPage(env, parsed.limit, parsed.cursor),
  });
}

function planLifecycleTickResult(doc, expectedScanned) {
  const result = doc?.plan_lifecycle;
  const count = (value) => Number.isSafeInteger(value) && value >= 0;
  if (doc?.schema_version !== "witself.v0" || !result ||
      !count(result.scanned) || result.scanned !== expectedScanned ||
      !count(result.seeded) || result.seeded > result.scanned ||
      !count(result.apply_pending) ||
      result.apply_pending > result.scanned ||
      !count(result.failed) || result.failed > result.scanned ||
      typeof result.succeeded !== "boolean") {
    return null;
  }
  let billingMutations;
  if (result.billing_mutations !== undefined) {
    billingMutations = billingMutationTickResult(
      result.billing_mutations,
      count,
    );
  }
  if (result.billing_mutations !== undefined && !billingMutations) {
    return null;
  }
  const expectedSucceeded = result.failed === 0 &&
    (billingMutations === undefined || billingMutations.succeeded);
  if (result.succeeded !== expectedSucceeded) {
    return null;
  }
  return result;
}

function billingMutationTickResult(result, count) {
  if (!result || typeof result !== "object" || Array.isArray(result)) {
    return null;
  }
  const fields = [
    "scanned", "attempted", "completed", "superseded", "busy", "failed",
    "terminal_cleaned",
  ];
  if (fields.some((field) => !count(result[field])) ||
      result.attempted > result.scanned ||
      result.completed > result.attempted ||
      result.superseded > result.attempted ||
      result.failed > result.attempted ||
      result.completed + result.superseded + result.failed >
        result.attempted ||
      result.busy > result.scanned ||
      result.terminal_cleaned > result.scanned ||
      result.attempted + result.busy + result.terminal_cleaned >
        result.scanned ||
      typeof result.scan_capped !== "boolean" ||
      typeof result.succeeded !== "boolean" ||
      (result.succeeded && result.failed !== 0)) {
    return null;
  }
  const oldest = result.oldest_observed_pending_at;
  if (oldest !== null &&
      (typeof oldest !== "string" || oldest.length > 64 ||
       !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(oldest) ||
       !Number.isFinite(Date.parse(oldest)))) {
    return null;
  }
  if (oldest !== null && result.scanned <= result.terminal_cleaned) {
    return null;
  }
  return result;
}

// runScheduledPlanLifecycle is the hosted lifecycle clock. The Worker owns the
// directory cursor in KV and sends one bounded active-account page to Go. The
// Go container may sleep or restart between calls without losing fleet
// progress. A page containing individual failures still advances: those
// accounts are retried on the next complete directory cycle, so one broken
// account cannot pin every later account behind it.
export async function runScheduledPlanLifecycle(env, containerFetch) {
  const enabled = String(env.CP_PLAN_LIFECYCLE_ENABLED ?? "")
    .trim().toLowerCase() === "true";
  const token = String(env.INTERNAL_BRIDGE_TOKEN ?? "");
  if (!enabled) {
    return { ran: false, configured: true };
  }
  if (!token || typeof containerFetch !== "function") {
    console.log("plan-lifecycle: scheduled tick configuration is incomplete");
    return { ran: false, configured: false };
  }

  try {
    const stored = await env.DIRECTORY.get(PLAN_LIFECYCLE_CURSOR_KEY, {
      type: "json",
    });
    let cursor;
    if (stored?.cursor != null) {
      if (typeof stored.cursor !== "string" || stored.cursor === "" ||
          stored.cursor.length > 2048) {
        console.log("plan-lifecycle: durable cursor is invalid; scan restarted");
      } else {
        cursor = stored.cursor;
      }
    }
    const page = await activeAccountPage(
      env,
      PLAN_LIFECYCLE_PAGE_SIZE,
      cursor,
    );
    const request = new Request(
      `http://control-plane.internal${PLAN_LIFECYCLE_TICK_PATH}`,
      {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ account_ids: page.account_ids }),
        signal: AbortSignal.timeout(PLAN_LIFECYCLE_TICK_TIMEOUT_MS),
      },
    );
    const response = await containerFetch(request);
    if (!response.ok) {
      console.log(`plan-lifecycle: scheduled tick failed status=${response.status}`);
      return { ran: true, succeeded: false };
    }
    let doc;
    try {
      doc = await response.json();
    } catch {
      console.log("plan-lifecycle: scheduled tick returned invalid JSON");
      return { ran: true, succeeded: false };
    }
    const result = planLifecycleTickResult(doc, page.account_ids.length);
    if (!result) {
      console.log("plan-lifecycle: scheduled tick returned an invalid acknowledgement");
      return { ran: true, succeeded: false };
    }

    await env.DIRECTORY.put(
      PLAN_LIFECYCLE_CURSOR_KEY,
      JSON.stringify({
        cursor: page.next_cursor,
        updated_at: new Date().toISOString(),
      }),
    );
    console.log(
      "plan-lifecycle: scheduled tick " +
      `scanned=${result.scanned} seeded=${result.seeded} ` +
      `apply_pending=${result.apply_pending} failed=${result.failed} ` +
      `succeeded=${result.succeeded}` +
      (result.billing_mutations === undefined ? "" :
        ` mutation_scanned=${result.billing_mutations.scanned}` +
        ` mutation_attempted=${result.billing_mutations.attempted}` +
        ` mutation_completed=${result.billing_mutations.completed}` +
        ` mutation_superseded=${result.billing_mutations.superseded}` +
        ` mutation_busy=${result.billing_mutations.busy}` +
        ` mutation_failed=${result.billing_mutations.failed}` +
        ` mutation_terminal_cleaned=${result.billing_mutations.terminal_cleaned}` +
        ` mutation_scan_capped=${result.billing_mutations.scan_capped}` +
        ` mutation_succeeded=${result.billing_mutations.succeeded}`),
    );
    return {
      ran: true,
      succeeded: result.succeeded,
      scanned: result.scanned,
      seeded: result.seeded,
      apply_pending: result.apply_pending,
      failed: result.failed,
      billing_mutations: result.billing_mutations,
    };
  } catch {
    console.log("plan-lifecycle: scheduled tick unavailable");
    return { ran: true, succeeded: false };
  }
}

async function resolveInternalAccount(env, accountID) {
  const resolved = await activeAccountRoute(env, accountID);
  if (resolved.response) return resolved.response;
  const cell = await env.DIRECTORY.get(`cell:${resolved.route.cell}`, {
    type: "json",
  });
  const endpoint = validCellEndpoint(cell?.endpoint);
  if (!endpoint) {
    return err("account cell has no valid HTTPS endpoint", 502);
  }
  // The endpoint is routing metadata, not a credential. Go needs it to
  // authenticate account-owner plan requests through the cell's /v1/whoami.
  // The provision token remains Worker-only and is never included here.
  return json({
    schema_version: "witself.v0",
    account_id: accountID,
    state: "active",
    cell: resolved.route.cell,
    endpoint,
  });
}

function validCellEndpoint(raw) {
  if (typeof raw !== "string") return null;
  try {
    const endpoint = new URL(raw);
    if (endpoint.protocol !== "https:" || !endpoint.hostname ||
        endpoint.username || endpoint.password) {
      return null;
    }
    return raw;
  } catch {
    return null;
  }
}

async function applyPlanSnapshot(request, env, accountID, fetchImpl) {
  const resolved = await activeAccountRoute(env, accountID);
  if (resolved.response) return resolved.response;
  const cell = await env.DIRECTORY.get(`cell:${resolved.route.cell}`, {
    type: "json",
  });
  const endpoint = validCellEndpoint(cell?.endpoint);
  if (!endpoint || !cell?.provision_token) {
    return err("account cell is not configured for plan application", 502);
  }
  const readFence = request.method === "GET";
  const bounded = readFence ? null : await boundedBody(request);
  if (bounded?.tooLarge) return err("request body too large", 413);
  if (!readFence && bounded?.body == null) {
    return err("plan snapshot body is required", 400);
  }

  // Alias routing is a global control-plane projection while plan enforcement
  // is cell-local. The preflight records a durable, non-mutating candidate
  // fence; ordinary plan restrictions and grace begin only after the cell
  // accepts that exact snapshot. The activation kill switch is the sole
  // immediate fail-closed exception inside the registry.
  let submittedSnapshot = null;
  if (!readFence && (env.REALM_EMAIL_ALIASES || env.AGENT_EMAIL_DOMAINS)) {
    try {
      const candidate = JSON.parse(new TextDecoder().decode(bounded.body));
      if (
        Number.isSafeInteger(candidate?.revision) && candidate.revision >= 0 &&
        typeof candidate?.limits === "object" && candidate.limits !== null &&
        !Array.isArray(candidate.limits) && Array.isArray(candidate?.features)
      ) {
        submittedSnapshot = candidate;
        await reconcileAgentEmailDomainsForPlan(
          env,
          accountID,
          submittedSnapshot,
          "restrict_only",
        );
        await reconcileRealmEmailAliasesForPlan(
          env,
          accountID,
          submittedSnapshot,
          "restrict_only",
        );
      }
    } catch (cause) {
      return err(
        `agent email policy reconciliation failed: ${String(cause?.message ?? cause)}`,
        502,
      );
    }
  }

  try {
    const headers = {
      Authorization: `Bearer ${cell.provision_token}`,
    };
    if (!readFence) {
      headers["Content-Type"] =
        request.headers.get("Content-Type") || "application/json";
    }
    const response = await fetchImpl(
      `${endpoint.replace(/\/+$/, "")}/v1/accounts/${accountID}:plan`,
      {
        method: request.method,
        headers,
        ...(readFence ? {} : { body: bounded.body }),
        signal: AbortSignal.timeout(BRIDGE_TIMEOUT_MS),
      },
    );
    if (readFence && response.ok) {
      let snapshot;
      try {
        snapshot = await response.json();
      } catch {
        return err("cell returned an invalid plan fence", 502);
      }
      const validRevision = Number.isSafeInteger(snapshot?.revision) &&
        snapshot.revision >= 0;
      const validHash = snapshot?.revision === 0
        ? snapshot?.snapshot_hash === ""
        : typeof snapshot?.snapshot_hash === "string" &&
          /^[0-9a-f]{64}$/.test(snapshot.snapshot_hash);
      if (snapshot?.account_id !== accountID || !validRevision || !validHash) {
        return err("cell returned an invalid plan fence", 502);
      }
      return json({
        schema_version: "witself.v0",
        account_id: accountID,
        revision: snapshot.revision,
        snapshot_hash: snapshot.snapshot_hash,
      });
    }
    if (!readFence && submittedSnapshot) {
      if (response.ok) {
        try {
          await reconcileRealmEmailAliasesForPlan(
            env,
            accountID,
            submittedSnapshot,
            "complete",
          );
          await reconcileAgentEmailDomainsForPlan(
            env,
            accountID,
            submittedSnapshot,
            "complete",
          );
        } catch (cause) {
          return err(
            `agent email policy reconciliation failed: ${String(cause?.message ?? cause)}`,
            502,
          );
        }
      } else {
        // Retire only the exact rejected candidate intent by reconciling the
        // cell's actual current snapshot. The candidate fence is supplied so
        // a delayed response cannot erase a newer pending plan revision.
        try {
          const current = await fetchImpl(
            `${endpoint.replace(/\/+$/, "")}/v1/accounts/${accountID}:plan`,
            {
              method: "GET",
              headers: { Authorization: `Bearer ${cell.provision_token}` },
              signal: AbortSignal.timeout(BRIDGE_TIMEOUT_MS),
            },
          );
          if (current.ok) {
            const snapshot = await current.json();
            await reconcileRealmEmailAliasesForPlan(
              env,
              accountID,
              snapshot,
              "complete",
              {
                recover_pending_revision: submittedSnapshot.revision,
                recover_pending_snapshot_hash:
                  submittedSnapshot.snapshot_hash,
              },
            );
            await reconcileAgentEmailDomainsForPlan(
              env,
              accountID,
              snapshot,
              "complete",
              {
                recover_pending_revision: submittedSnapshot.revision,
                recover_pending_snapshot_hash:
                  submittedSnapshot.snapshot_hash,
              },
            );
          }
        } catch {
          // Realm-alias authority can recover from its alarm. Custom-domain
          // awaiting-cell intent remains fail closed until the durable plan
          // workflow or an operator repeats this exact completion fence; its
          // alarm processes only cell-committed work.
        }
      }
    }
    return relay(response);
  } catch (cause) {
    const detail = cause?.name === "TimeoutError" ? "timed out" : "unreachable";
    return err(`cell ${detail}`, 502);
  }
}

function validPlanFitTarget(input) {
  const target = input?.target;
  if (input?.schema_version !== "witself.v0" ||
      !target || typeof target !== "object" || Array.isArray(target) ||
      typeof target.plan !== "string" || target.plan === "" ||
      target.plan !== target.plan.trim() ||
      typeof target.snapshot_hash !== "string" ||
      !/^[0-9a-f]{64}$/.test(target.snapshot_hash) ||
      !target.limits || typeof target.limits !== "object" ||
      Array.isArray(target.limits) ||
      !target.policies || typeof target.policies !== "object" ||
      Array.isArray(target.policies) || !Array.isArray(target.features)) {
    return null;
  }
  for (const value of Object.values(target.limits)) {
    if (!Number.isSafeInteger(value) || value < 0) return null;
  }
  return target;
}

function validPlanFitCellReport(report, accountID, target) {
  if (report?.schema_version !== "witself.v0" ||
      report?.account_id !== accountID ||
      report?.target_plan !== target.plan ||
      report?.target_snapshot_hash !== target.snapshot_hash ||
      !Array.isArray(report?.violations)) return false;
  const seen = new Set();
  return report.violations.every((violation) => {
    const dimension = violation?.dimension;
    const expectedScope = PLAN_FIT_DIMENSION_SCOPE[dimension];
    const maximum = target.limits[dimension];
    const valid = violation && typeof violation === "object" &&
      violation.code === "limit_exceeded" &&
      typeof expectedScope === "string" && !seen.has(dimension) &&
      Object.hasOwn(target.limits, dimension) &&
      violation.scope === expectedScope &&
      Number.isSafeInteger(violation.used) &&
      Number.isSafeInteger(violation.max) && violation.max === maximum &&
      violation.used > violation.max &&
      Number.isSafeInteger(violation.subject_count) &&
      violation.subject_count >= 1 &&
      (expectedScope !== "account" || violation.subject_count === 1);
    if (valid) seen.add(dimension);
    return valid;
  });
}

function mergeAuthoritativePlanFit(report, dimension, authoritative) {
  const existing = report.violations.find(
    (violation) => violation.dimension === dimension,
  );
  report.violations = report.violations.filter(
    (violation) => violation.dimension !== dimension,
  );
  if (authoritative.code === "authority_incomplete") {
    report.violations.push(authoritative);
  } else if (existing || authoritative.used > authoritative.max) {
    report.violations.push({
      ...authoritative,
      used: Math.max(authoritative.used, existing?.used ?? 0),
      subject_count: Math.max(
        authoritative.subject_count,
        existing?.subject_count ?? 0,
      ),
    });
  }
  const order = new Map(
    PLAN_FIT_DIMENSION_ORDER.map((value, index) => [value, index]),
  );
  report.violations.sort((left, right) =>
    (order.get(left.dimension) ?? Number.MAX_SAFE_INTEGER) -
      (order.get(right.dimension) ?? Number.MAX_SAFE_INTEGER)
  );
}

async function planFitAuthorityViolation(
  env,
  accountID,
  dimension,
  maximum,
) {
  try {
    if (dimension === PLAN_FIT_ALIAS_LIMIT) {
      const fit = await readRealmEmailAliasPlanFit(env, accountID, maximum);
      return {
        code: "limit_exceeded",
        dimension,
        scope: "realm",
        used: fit.highest_used,
        max: maximum,
        subject_count: fit.over_limit_count,
      };
    }
    const fit = await readAgentEmailDomainPlanFit(env, accountID, maximum);
    return {
      code: "limit_exceeded",
      dimension,
      scope: "account",
      used: fit.used,
      max: maximum,
      subject_count: fit.used > maximum ? 1 : 0,
    };
  } catch {
    return {
      code: "authority_incomplete",
      dimension,
      scope: "authority",
      used: 0,
      max: maximum,
      subject_count: 1,
    };
  }
}

async function checkPlanFit(request, env, accountID, fetchImpl) {
  const resolved = await activeAccountRoute(env, accountID);
  if (resolved.response) return resolved.response;
  const cell = await env.DIRECTORY.get(`cell:${resolved.route.cell}`, {
    type: "json",
  });
  const endpoint = validCellEndpoint(cell?.endpoint);
  if (!endpoint || !cell?.provision_token) {
    return err("account cell is not configured for plan-fit reads", 502);
  }
  const bounded = await boundedBody(request);
  if (bounded?.tooLarge) return err("request body too large", 413);
  if (bounded?.body == null) return err("plan-fit target snapshot is required", 400);
  let input;
  try {
    input = JSON.parse(new TextDecoder().decode(bounded.body));
  } catch {
    return err("invalid plan-fit target snapshot", 400);
  }
  const target = validPlanFitTarget(input);
  if (!target) return err("invalid plan-fit target snapshot", 400);

  let response;
  try {
    response = await fetchImpl(
      `${endpoint.replace(/\/+$/, "")}/v1/accounts/${accountID}:plan-fit`,
      {
        method: "POST",
        headers: {
          Authorization: `Bearer ${cell.provision_token}`,
          "Content-Type": "application/json",
        },
        body: bounded.body,
        signal: AbortSignal.timeout(BRIDGE_TIMEOUT_MS),
      },
    );
  } catch (cause) {
    const detail = cause?.name === "TimeoutError" ? "timed out" : "unreachable";
    return err(`cell ${detail}`, 502);
  }
  if (!response.ok) return relay(response);
  let report;
  try {
    report = await response.json();
  } catch {
    return err("cell returned an invalid plan-fit report", 502);
  }
  if (!validPlanFitCellReport(report, accountID, target)) {
    return err("cell returned an invalid plan-fit report", 502);
  }

  for (const dimension of [PLAN_FIT_ALIAS_LIMIT, PLAN_FIT_DOMAIN_LIMIT]) {
    if (!Object.hasOwn(target.limits, dimension)) continue;
    const authoritative = await planFitAuthorityViolation(
      env,
      accountID,
      dimension,
      target.limits[dimension],
    );
    mergeAuthoritativePlanFit(report, dimension, authoritative);
  }
  return json(report);
}

// handleInternalBridgeRequest terminates every /v1/internal/* request at the
// Worker. Authentication precedes route disclosure.
export async function handleInternalBridgeRequest(
  request,
  env,
  fetchImpl = fetch,
  backendFactory,
) {
  if (!internalBridgeAuthorized(request, env)) {
    return err("unauthorized", 401);
  }
  const url = new URL(request.url);
  if (url.pathname === PLAN_LIFECYCLE_ACTIVATE_PATH) {
    return activatePlanLifecycle(request, env, backendFactory);
  }
  if (url.pathname === INTERNAL_ACCOUNTS_PATH) {
    if (request.method !== "GET") return err("method not allowed", 405);
    return listActiveAccountIDs(env, url);
  }
  const resolveMatch = url.pathname.match(INTERNAL_RESOLVE_PATH);
  if (resolveMatch) {
    if (request.method !== "GET") return err("method not allowed", 405);
    return resolveInternalAccount(env, resolveMatch[1]);
  }
  const applyMatch = url.pathname.match(INTERNAL_APPLY_PATH);
  if (applyMatch) {
    if (!["GET", "POST"].includes(request.method)) {
      return err("method not allowed", 405);
    }
    return applyPlanSnapshot(request, env, applyMatch[1], fetchImpl);
  }
  const fitMatch = url.pathname.match(INTERNAL_FIT_PATH);
  if (fitMatch) {
    if (request.method !== "POST") return err("method not allowed", 405);
    return checkPlanFit(request, env, fitMatch[1], fetchImpl);
  }
  return err("not found", 404);
}
