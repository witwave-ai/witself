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
//     POST /v1/internal/accounts/{id}:plan-fit-apply
//       prepares exact global-authority fences, then atomically checks and
//       applies the target in the cell. A blocked leg compensates every
//       prepared authority against the cell's still-current snapshot.
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
const INTERNAL_FIT_APPLY_PATH = new RegExp(
  `^/v1/internal/accounts/(${ACCOUNT_ID_PATTERN}):plan-fit-apply$`,
);

const PLAN_FIT_ALIAS_LIMIT = "agent_email_realm_aliases_per_realm";
const PLAN_FIT_DOMAIN_LIMIT = "agent_email_custom_domains_per_account";
const PLAN_FIT_DIMENSION_FEATURE = Object.freeze({
  [PLAN_FIT_ALIAS_LIMIT]: "agent_email_realm_alias",
  [PLAN_FIT_DOMAIN_LIMIT]: "agent_email_custom_domain",
});
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
const PLAN_FEATURE_KEYS = new Set([
  "agent_email_custom_domain",
  "agent_email_realm_alias",
  "agent_email_receive",
  "agent_email_send",
  "collaboration",
  "facts",
  "memory",
  "messaging",
  "secrets",
  "support",
]);
const PLAN_LIMIT_KEYS = new Set([
  "agent_email_attachment_storage_bytes",
  "agent_email_custom_domains_per_account",
  "agent_email_max_raw_bytes",
  "agent_email_realm_aliases_per_realm",
  "agent_email_received_bytes_per_realm_minute",
  "agent_email_received_bytes_per_recipient_minute",
  "agent_email_received_bytes_per_sender_minute",
  "agent_email_received_per_realm_minute",
  "agent_email_received_per_recipient_minute",
  "agent_email_received_per_sender_minute",
  "agent_email_sent_per_agent_minute",
  "agent_email_sent_per_realm_minute",
  "agents",
  "agents_per_realm",
  "message_delivered_per_realm_minute",
  "message_delivered_per_recipient_minute",
  "message_sent_per_agent_minute",
  "realms",
  "stored_fact",
  "stored_memory",
  "stored_secret",
]);
const PLAN_POLICY_KEYS = new Set([
  "agent_email_entitlement_version",
  "agent_email_retention_days",
  "message_retention_days",
  "messaging_entitlement_version",
  "transcript_retention_days",
]);
const PLAN_LIMIT_MAXIMUMS = Object.freeze({
  agent_email_max_raw_bytes: 25 * 1024 * 1024,
  agent_email_received_bytes_per_realm_minute: 4 * 1024 * 1024 * 1024,
  agent_email_received_bytes_per_recipient_minute: 512 * 1024 * 1024,
  agent_email_received_bytes_per_sender_minute: 64 * 1024 * 1024,
  agent_email_received_per_realm_minute: 5_000,
  agent_email_received_per_recipient_minute: 300,
  agent_email_received_per_sender_minute: 30,
  agent_email_sent_per_agent_minute: 30,
  agent_email_sent_per_realm_minute: 300,
  message_delivered_per_realm_minute: 100_000,
  message_delivered_per_recipient_minute: 5_000,
  message_sent_per_agent_minute: 2_000,
});
const RETENTION_POLICY_KEYS = new Set([
  "agent_email_retention_days",
  "message_retention_days",
  "transcript_retention_days",
]);
const ENTITLEMENT_POLICY_KEYS = new Set([
  "agent_email_entitlement_version",
  "messaging_entitlement_version",
]);

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
    ["CP_STRIPE_MODE", "WITSELF_CP_STRIPE_MODE"],
    ["CP_STRIPE_SECRET_KEY", "WITSELF_CP_STRIPE_SECRET_KEY"],
    ["CP_STRIPE_WEBHOOK_SECRET", "WITSELF_CP_STRIPE_WEBHOOK_SECRET"],
    ["CP_STRIPE_SUCCESS_URL", "WITSELF_CP_STRIPE_SUCCESS_URL"],
    ["CP_STRIPE_CANCEL_URL", "WITSELF_CP_STRIPE_CANCEL_URL"],
    ["CP_STRIPE_PORTAL_RETURN_URL", "WITSELF_CP_STRIPE_PORTAL_RETURN_URL"],
    [
      "CP_STRIPE_PORTAL_CONFIGURATION_ID",
      "WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID",
    ],
    ["CP_STRIPE_TEST_CLOCK_ID", "WITSELF_CP_STRIPE_TEST_CLOCK_ID"],
    // Absent means Stripe Tax stays off (explicitBoolEnv treats empty as
    // false); staging "true" is the tax activation flip once registrations
    // exist in the dashboard.
    ["CP_STRIPE_AUTOMATIC_TAX", "WITSELF_CP_STRIPE_AUTOMATIC_TAX"],
    [
      "CP_BILLING_ACCOUNT_ALLOWLIST",
      "WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST",
    ],
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
  if (!request.body) return { body: new ArrayBuffer(0) };

  const reader = request.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > BRIDGE_BODY_MAX_BYTES) {
        await reader.cancel().catch(() => {});
        return { tooLarge: true };
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { body: body.buffer };
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
      !validPlanName(target.plan) ||
      typeof target.snapshot_hash !== "string" ||
      !/^[0-9a-f]{64}$/.test(target.snapshot_hash) ||
      !validPlanLimits(target.limits) ||
      !validPlanPolicies(target.policies) ||
      !validPlanFeatures(target.features)) {
    return null;
  }
  return target;
}

function validPlanFitApplyTarget(input) {
  const target = validPlanFitTarget(input);
  if (!target || !Number.isSafeInteger(target.revision) ||
      target.revision < 1) return null;
  return target;
}

function sameIntegerMap(left, right) {
  if (!left || typeof left !== "object" || Array.isArray(left) ||
      !right || typeof right !== "object" || Array.isArray(right)) return false;
  const leftKeys = Object.keys(left).sort();
  const rightKeys = Object.keys(right).sort();
  return leftKeys.length === rightKeys.length &&
    leftKeys.every((key, index) => key === rightKeys[index] &&
      Number.isSafeInteger(left[key]) && left[key] === right[key]);
}

function validPlanName(plan) {
  return typeof plan === "string" && plan !== "" && plan === plan.trim();
}

function validPlanLimits(limits) {
  if (!limits || typeof limits !== "object" || Array.isArray(limits)) {
    return false;
  }
  return Object.entries(limits).every(([key, value]) => {
    const maximum = PLAN_LIMIT_MAXIMUMS[key] ?? Number.MAX_SAFE_INTEGER;
    return PLAN_LIMIT_KEYS.has(key) && Number.isSafeInteger(value) &&
      value >= 0 && value <= maximum;
  });
}

function validPlanPolicies(policies) {
  if (!policies || typeof policies !== "object" || Array.isArray(policies)) {
    return false;
  }
  return Object.entries(policies).every(([key, value]) =>
    PLAN_POLICY_KEYS.has(key) && Number.isSafeInteger(value) &&
    ((RETENTION_POLICY_KEYS.has(key) && value >= 1 && value <= 36_500) ||
      (ENTITLEMENT_POLICY_KEYS.has(key) && value === 1))
  );
}

function validPlanFeatures(features) {
  return sameStringSet(features, features) &&
    features.every((feature) => PLAN_FEATURE_KEYS.has(feature));
}

function orderedIntegerMap(values) {
  return Object.fromEntries(
    Object.keys(values).sort().map((key) => [key, values[key]]),
  );
}

function canonicalPlanSnapshotJSON(snapshot) {
  const raw = JSON.stringify({
    plan: snapshot.plan,
    limits: orderedIntegerMap(snapshot.limits),
    policies: orderedIntegerMap(snapshot.policies),
    features: [...snapshot.features].sort(),
  });
  // encoding/json escapes these five runes by default. Matching that behavior
  // keeps this edge verifier byte-identical to plans.SnapshotHash even for an
  // unusual but valid plan label.
  return raw.replace(/[<>&\u2028\u2029]/gu, (value) => ({
    "<": "\\u003c",
    ">": "\\u003e",
    "&": "\\u0026",
    "\u2028": "\\u2028",
    "\u2029": "\\u2029",
  })[value]);
}

async function planSnapshotHashMatches(snapshot) {
  if (!globalThis.crypto?.subtle) return false;
  try {
    const digest = await globalThis.crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(canonicalPlanSnapshotJSON(snapshot)),
    );
    const actual = Array.from(new Uint8Array(digest), (value) =>
      value.toString(16).padStart(2, "0")
    ).join("");
    return actual === snapshot.snapshot_hash;
  } catch {
    return false;
  }
}

function validAppliedAt(value) {
  const match = typeof value === "string" && value.length <= 64
    ? value.match(
      /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|[+-](\d{2}):(\d{2}))$/u,
    )
    : null;
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zoneHour, zoneMinute] =
    match.map((part, index) => index === 0 || part === undefined
      ? part
      : Number(part));
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59 ||
      (zoneHour !== undefined && (zoneHour > 23 || zoneMinute > 59))) {
    return false;
  }
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const monthDays = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31,
    30, 31][month - 1];
  return day >= 1 && day <= monthDays;
}

function validPlanSnapshotFields(snapshot) {
  return validPlanName(snapshot?.plan) && validPlanLimits(snapshot?.limits) &&
    validPlanPolicies(snapshot?.policies) &&
    validPlanFeatures(snapshot?.features);
}

function sameStringSet(left, right) {
  if (!Array.isArray(left) || !Array.isArray(right) ||
      left.some((value) => typeof value !== "string" || value === "" ||
        value !== value.trim()) ||
      right.some((value) => typeof value !== "string" || value === "" ||
        value !== value.trim())) return false;
  const sortedLeft = [...left].sort();
  const sortedRight = [...right].sort();
  return new Set(sortedLeft).size === sortedLeft.length &&
    new Set(sortedRight).size === sortedRight.length &&
    sortedLeft.length === sortedRight.length &&
    sortedLeft.every((value, index) => value === sortedRight[index]);
}

function validCurrentPlanSnapshot(snapshot, accountID, targetRevision) {
  if (!snapshot || typeof snapshot !== "object" || Array.isArray(snapshot) ||
      snapshot.account_id !== accountID ||
      !Number.isSafeInteger(snapshot.revision) || snapshot.revision < 0 ||
      snapshot.revision === targetRevision ||
      !validPlanSnapshotFields(snapshot)) return false;
  if (snapshot.revision === 0) return snapshot.snapshot_hash === "";
  return typeof snapshot.snapshot_hash === "string" &&
    /^[0-9a-f]{64}$/.test(snapshot.snapshot_hash) &&
    validAppliedAt(snapshot.applied_at);
}

async function verifiedCurrentPlanSnapshot(snapshot, accountID, targetRevision) {
  return validCurrentPlanSnapshot(snapshot, accountID, targetRevision) &&
    (snapshot.revision === 0 || await planSnapshotHashMatches(snapshot));
}

function snapshotMatchesPlanFitTarget(snapshot, accountID, target) {
  return snapshot?.account_id === accountID &&
    snapshot?.revision === target.revision &&
    snapshot?.snapshot_hash === target.snapshot_hash &&
    snapshot?.plan === target.plan &&
    sameIntegerMap(snapshot?.limits, target.limits) &&
    sameIntegerMap(snapshot?.policies, target.policies) &&
    sameStringSet(snapshot?.features, target.features) &&
    validAppliedAt(snapshot?.applied_at);
}

async function validPlanFitApplyCellResult(result, accountID, target) {
  if (result?.schema_version !== "witself.v0" ||
      result?.account_id !== accountID ||
      result?.target_revision !== target.revision ||
      result?.target_plan !== target.plan ||
      result?.target_snapshot_hash !== target.snapshot_hash ||
      !Array.isArray(result?.violations)) return false;
  if (result.state === "applied") {
    return result.violations.length === 0 &&
      result.current_snapshot === undefined &&
      snapshotMatchesPlanFitTarget(result.applied_snapshot, accountID, target);
  }
  if (result.state !== "blocked" || result.applied_snapshot !== undefined ||
      !await verifiedCurrentPlanSnapshot(
        result.current_snapshot,
        accountID,
        target.revision,
      ) || result.current_snapshot.revision >= target.revision ||
      result.violations.length === 0) return false;
  return validPlanFitCellReport({
    schema_version: "witself.v0",
    account_id: accountID,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: result.violations,
  }, accountID, target);
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

function preparedAuthorityOutcome(body, dimension, maximum, target) {
  if (body?.code === "plan_fit_prepared_fence_conflict") {
    if (body.prepared !== false || body.pending !== true ||
        body.stale !== false || body.complete !== false ||
        body.pending_state !== "awaiting_cell" ||
        !Number.isSafeInteger(body.pending_plan_revision) ||
        body.pending_plan_revision < 1 ||
        body.pending_plan_revision >= target.revision ||
        typeof body.pending_plan_snapshot_hash !== "string" ||
        !/^[0-9a-f]{64}$/.test(body.pending_plan_snapshot_hash) ||
        body.fit !== undefined) {
      throw new Error("plan-fit authority returned an invalid prepared fence");
    }
    return {
      prepared: false,
      violation: null,
      conflict: {
        dimension,
        revision: body.pending_plan_revision,
        snapshot_hash: body.pending_plan_snapshot_hash,
      },
    };
  }
  if (!body || typeof body !== "object" || body.skipped === true ||
      body.complete !== true || !body.fit || body.fit.complete !== true ||
      body.fit.dimension !== dimension || body.fit.maximum !== maximum) {
    throw new Error("plan-fit authority returned an invalid preparation result");
  }
  const isAlias = dimension === PLAN_FIT_ALIAS_LIMIT;
  const used = isAlias ? body.fit.highest_used : body.fit.used;
  const subjectCount = body.fit.over_limit_count;
  if (!Number.isSafeInteger(used) || used < 0 ||
      !Number.isSafeInteger(subjectCount) || subjectCount < 0 ||
      (!isAlias && subjectCount > 1)) {
    throw new Error("plan-fit authority returned invalid bounded evidence");
  }
  if (body.prepared === true) {
    if ((maximum !== null && used > maximum) || subjectCount !== 0) {
      throw new Error("plan-fit authority prepared an over-limit target");
    }
    return { prepared: true, violation: null, conflict: null };
  }
  if (body.prepared !== false || body.code !== "plan_fit_failed" ||
      maximum === null || used <= maximum || subjectCount < 1) {
    throw new Error("plan-fit authority returned an invalid refusal");
  }
  return {
    prepared: false,
    conflict: null,
    violation: {
      code: "limit_exceeded",
      dimension,
      scope: PLAN_FIT_DIMENSION_SCOPE[dimension],
      used,
      max: maximum,
      subject_count: subjectCount,
    },
  };
}

function planFitAuthorityMaximum(target, dimension) {
  const feature = PLAN_FIT_DIMENSION_FEATURE[dimension];
  if (typeof feature !== "string" || !target.features.includes(feature)) {
    return 0;
  }
  return Object.hasOwn(target.limits, dimension)
    ? target.limits[dimension]
    : null;
}

function validPlanAuthorityCompletion(
  body,
  schemaVersion,
  accountID,
  recovery = false,
) {
  const valid = body?.schema_version === schemaVersion &&
    body?.account_id === accountID && body?.mode === "complete" &&
    body?.stale === false && body?.complete === true &&
    Number.isSafeInteger(body?.changed) && body.changed >= 0 &&
    Number.isSafeInteger(body?.registry_revision) &&
    body.registry_revision >= 0;
  return valid && (recovery
    ? body.recovered === true && body.changed === 0
    : body.recovered === undefined);
}

async function preparePlanAuthorities(env, accountID, target) {
  const prepared = [];
  for (const authority of [
    {
      dimension: PLAN_FIT_DOMAIN_LIMIT,
      reconcile: reconcileAgentEmailDomainsForPlan,
    },
    {
      dimension: PLAN_FIT_ALIAS_LIMIT,
      reconcile: reconcileRealmEmailAliasesForPlan,
    },
  ]) {
    const maximum = planFitAuthorityMaximum(target, authority.dimension);
    let outcome;
    try {
      const body = await authority.reconcile(
        env,
        accountID,
        target,
        "prepare",
      );
      outcome = preparedAuthorityOutcome(
        body,
        authority.dimension,
        maximum,
        target,
      );
    } catch {
      return { prepared, violation: null, conflict: null, unavailable: true };
    }
    if (outcome.conflict) {
      return {
        prepared,
        violation: null,
        conflict: outcome.conflict,
        unavailable: false,
      };
    }
    if (outcome.violation) {
      return {
        prepared,
        violation: outcome.violation,
        conflict: null,
        unavailable: false,
      };
    }
    prepared.push(authority.dimension);
  }
  return { prepared, violation: null, conflict: null, unavailable: false };
}

async function completePlanAuthorities(env, accountID, target) {
  const alias = await reconcileRealmEmailAliasesForPlan(
    env,
    accountID,
    target,
    "complete",
  );
  if (!validPlanAuthorityCompletion(
    alias,
    "witself.realm-email-alias.v1",
    accountID,
  )) {
    throw new Error("realm email alias completion acknowledgement is invalid");
  }
  const domain = await reconcileAgentEmailDomainsForPlan(
    env,
    accountID,
    target,
    "complete",
  );
  if (!validPlanAuthorityCompletion(
    domain,
    "witself.agent-email-domain.v1",
    accountID,
  )) {
    throw new Error("custom domain completion acknowledgement is invalid");
  }
}

async function recoverPlanAuthorities(
  env,
  accountID,
  target,
  current,
  prepared,
) {
  const options = {
    recover_pending_revision: target.revision,
    recover_pending_snapshot_hash: target.snapshot_hash,
  };
  const selected = new Set(prepared);
  for (const authority of [
    {
      dimension: PLAN_FIT_ALIAS_LIMIT,
      schema: "witself.realm-email-alias.v1",
      label: "realm email alias",
      reconcile: reconcileRealmEmailAliasesForPlan,
    },
    {
      dimension: PLAN_FIT_DOMAIN_LIMIT,
      schema: "witself.agent-email-domain.v1",
      label: "custom domain",
      reconcile: reconcileAgentEmailDomainsForPlan,
    },
  ]) {
    if (!selected.has(authority.dimension)) continue;
    const body = await authority.reconcile(
      env,
      accountID,
      current,
      "complete",
      options,
    );
    if (!validPlanAuthorityCompletion(
      body,
      authority.schema,
      accountID,
      current.revision < target.revision,
    )) {
      throw new Error(`${authority.label} recovery acknowledgement is invalid`);
    }
  }
}

async function readCellPlanSnapshot(fetchImpl, endpoint, token, accountID, target) {
  const response = await fetchImpl(
    `${endpoint.replace(/\/+$/, "")}/v1/accounts/${accountID}:plan`,
    {
      method: "GET",
      headers: { Authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(BRIDGE_TIMEOUT_MS),
    },
  );
  if (!response.ok) {
    throw new Error("cell current plan is unavailable");
  }
  const snapshot = await response.json().catch(() => null);
  if (!snapshotMatchesPlanFitTarget(snapshot, accountID, target) &&
      !await verifiedCurrentPlanSnapshot(
        snapshot,
        accountID,
        target.revision,
      )) {
    throw new Error("cell returned an invalid current plan snapshot");
  }
  return snapshot;
}

function appliedPlanFitEnvelope(accountID, target, snapshot) {
  return {
    schema_version: "witself.v0",
    state: "applied",
    account_id: accountID,
    target_revision: target.revision,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: [],
    applied_snapshot: snapshot,
  };
}

function blockedPlanFitEnvelope(accountID, target, current, violation) {
  return {
    schema_version: "witself.v0",
    state: "blocked",
    account_id: accountID,
    target_revision: target.revision,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: [violation],
    current_snapshot: current,
  };
}

async function applyPlanSnapshotIfFits(request, env, accountID, fetchImpl) {
  const resolved = await activeAccountRoute(env, accountID);
  if (resolved.response) return resolved.response;
  const cell = await env.DIRECTORY.get(`cell:${resolved.route.cell}`, {
    type: "json",
  });
  const endpoint = validCellEndpoint(cell?.endpoint);
  if (!endpoint || !cell?.provision_token) {
    return err("account cell is not configured for plan-fit application", 502);
  }
  const bounded = await boundedBody(request);
  if (bounded?.tooLarge) return err("request body too large", 413);
  if (bounded?.body == null) {
    return err("plan-fit apply target snapshot is required", 400);
  }
  let input;
  try {
    input = JSON.parse(new TextDecoder().decode(bounded.body));
  } catch {
    return err("invalid plan-fit apply target snapshot", 400);
  }
  const target = validPlanFitApplyTarget(input);
  if (!target || !await planSnapshotHashMatches(target)) {
    return err("invalid plan-fit apply target snapshot", 400);
  }
  if (!env.AGENT_EMAIL_DOMAINS || !env.REALM_EMAIL_ALIASES) {
    return err("agent email plan-fit authority is unavailable", 502);
  }

  let authority;
  for (let attempt = 0; attempt < 3; attempt += 1) {
    authority = await preparePlanAuthorities(env, accountID, target);
    if (!authority.conflict) break;

    let current;
    try {
      current = await readCellPlanSnapshot(
        fetchImpl,
        endpoint,
        cell.provision_token,
        accountID,
        target,
      );
      if (current.revision === authority.conflict.revision &&
          current.snapshot_hash !== authority.conflict.snapshot_hash) {
        return err("prepared plan-fit fence conflicts with the cell", 502);
      }
      if (snapshotMatchesPlanFitTarget(current, accountID, target)) {
        await recoverPlanAuthorities(
          env,
          accountID,
          authority.conflict,
          current,
          [authority.conflict.dimension],
        );
        await completePlanAuthorities(env, accountID, target);
        return json(appliedPlanFitEnvelope(accountID, target, current));
      }
      if (authority.prepared.length > 0) {
        await recoverPlanAuthorities(
          env,
          accountID,
          target,
          current,
          authority.prepared,
        );
      }
      await recoverPlanAuthorities(
        env,
        accountID,
        authority.conflict,
        current,
        [authority.conflict.dimension],
      );
    } catch {
      return err("prepared plan-fit authority recovery failed", 502);
    }
    if (current.revision > target.revision) {
      return err("cell plan snapshot supersedes plan-fit target", 409);
    }
  }
  if (authority?.conflict) {
    return err("prepared plan-fit authority recovery did not converge", 502);
  }
  if (authority.unavailable) {
    try {
      const current = await readCellPlanSnapshot(
        fetchImpl,
        endpoint,
        cell.provision_token,
        accountID,
        target,
      );
      if (snapshotMatchesPlanFitTarget(current, accountID, target)) {
        await completePlanAuthorities(env, accountID, target);
        return json(appliedPlanFitEnvelope(accountID, target, current));
      }
      if (authority.prepared.length > 0) {
        await recoverPlanAuthorities(
          env,
          accountID,
          target,
          current,
          authority.prepared,
        );
      }
    } catch {
      // The exact prepared intent remains fail closed for a retry or operator.
    }
    return err("agent email plan-fit authority is unavailable", 502);
  }

  if (authority.violation) {
    let current;
    try {
      current = await readCellPlanSnapshot(
        fetchImpl,
        endpoint,
        cell.provision_token,
        accountID,
        target,
      );
      if (snapshotMatchesPlanFitTarget(current, accountID, target)) {
        return err("agent email plan-fit authority conflicts with the cell", 502);
      }
      if (authority.prepared.length > 0) {
        await recoverPlanAuthorities(
          env,
          accountID,
          target,
          current,
          authority.prepared,
        );
      }
      if (current.revision > target.revision) {
        return err("cell plan snapshot supersedes plan-fit target", 409);
      }
    } catch {
      return err("agent email plan-fit authority recovery failed", 502);
    }
    return json(blockedPlanFitEnvelope(
      accountID,
      target,
      current,
      authority.violation,
    ));
  }

  let response;
  try {
    response = await fetchImpl(
      `${endpoint.replace(/\/+$/, "")}/v1/accounts/${accountID}:plan-fit-apply`,
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
  } catch {
    response = null;
  }

  let result = null;
  if (response?.ok) {
    result = await response.json().catch(() => null);
  }
  if (!response?.ok ||
      !await validPlanFitApplyCellResult(result, accountID, target)) {
    try {
      const current = await readCellPlanSnapshot(
        fetchImpl,
        endpoint,
        cell.provision_token,
        accountID,
        target,
      );
      if (snapshotMatchesPlanFitTarget(current, accountID, target)) {
        await completePlanAuthorities(env, accountID, target);
        return json(appliedPlanFitEnvelope(accountID, target, current));
      }
      await recoverPlanAuthorities(
        env,
        accountID,
        target,
        current,
        authority.prepared,
      );
    } catch {
      return err("cell plan-fit application outcome is indeterminate", 502);
    }
    if (response && !response.ok) return relay(response);
    return err("cell returned an invalid plan-fit apply result", 502);
  }

  try {
    if (result.state === "applied") {
      await completePlanAuthorities(env, accountID, target);
    } else {
      await recoverPlanAuthorities(
        env,
        accountID,
        target,
        result.current_snapshot,
        authority.prepared,
      );
    }
  } catch {
    return err("agent email plan-fit authority reconciliation failed", 502);
  }
  return json(result);
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
  if (!target || !await planSnapshotHashMatches(target)) {
    return err("invalid plan-fit target snapshot", 400);
  }

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
    const maximum = planFitAuthorityMaximum(target, dimension);
    if (maximum === null) continue;
    const authoritative = await planFitAuthorityViolation(
      env,
      accountID,
      dimension,
      maximum,
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
  const fitApplyMatch = url.pathname.match(INTERNAL_FIT_APPLY_PATH);
  if (fitApplyMatch) {
    if (request.method !== "POST") return err("method not allowed", 405);
    return applyPlanSnapshotIfFits(
      request,
      env,
      fitApplyMatch[1],
      fetchImpl,
    );
  }
  return err("not found", 404);
}
