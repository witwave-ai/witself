import {
  managedRealmEmailDomains,
  managedRealmEmailPrimaryDomain,
  realmEmailAliasEntitlement,
  realmEmailAliasRegistryStub,
} from "./realm-email-alias-runtime.mjs";

const SCHEMA_VERSION = "witself.realm-email-alias.v1";
const ACCOUNT_ID_PATTERN = "[A-Za-z0-9_-]{1,128}";
const REALM_ID_PATTERN = "realm_[a-z2-7]{16}";
const REQUEST_ID_PATTERN = "earq_[a-z2-7]{16}";
const ALIAS_PATTERN = "[a-z0-9][a-z0-9-]{1,14}[a-z0-9]";

const CUSTOMER_REQUESTS_PATH = new RegExp(
  `^/v1/accounts/(${ACCOUNT_ID_PATTERN})/realms/(${REALM_ID_PATTERN})/email-alias-requests$`,
);
const CUSTOMER_REALM_CLOSE_PATH = new RegExp(
  `^/v1/accounts/(${ACCOUNT_ID_PATTERN})/realms/(${REALM_ID_PATTERN}):close$`,
);
const ADMIN_REALM_CLOSE_PATH = new RegExp(
  `^/v1/admin/accounts/(${ACCOUNT_ID_PATTERN})/realms/(${REALM_ID_PATTERN}):close$`,
);
const ADMIN_REQUESTS_PATH = "/v1/admin/realm-email-alias-requests";
const ADMIN_COUNTER_REBUILD_PATH =
  "/v1/admin/realm-email-alias-counters:rebuild";
const ADMIN_REQUEST_ACTION_PATH = new RegExp(
  `^/v1/admin/realm-email-alias-requests/(${REQUEST_ID_PATTERN}):(approve|reject)$`,
);
const ADMIN_ALIASES_PATH = "/v1/admin/realm-email-aliases";
const ADMIN_ALIAS_ACTION_PATH = new RegExp(
  `^/v1/admin/realm-email-aliases/(${ALIAS_PATTERN}):(suspend|reactivate|retire)$`,
);
const ADMIN_ALIAS_ABORT_PATH = new RegExp(
  `^/v1/admin/realm-email-aliases/(${ALIAS_PATTERN}):abort-provisioning$`,
);
const ADMIN_INTERNAL_ASSIGN_PATH =
  "/v1/admin/realm-email-aliases:assign-internal";
const ADMIN_RESERVED_PATH = "/v1/admin/realm-email-reserved-names";
const ADMIN_RESERVED_ITEM_PATH = new RegExp(
  `^/v1/admin/realm-email-reserved-names/(${ALIAS_PATTERN})$`,
);
const ADMIN_AUDIT_PATH = "/v1/admin/realm-email-alias-audit";
const EDGE_ROUTE_PATH =
  /^\/v1\/email\/realm-routes\/([^/]{1,253})\/([^/]{3,16})$/;
const EDGE_ROUTE_PREFIX = "/v1/email/realm-routes";
const BODY_MAX_BYTES = 16 * 1024;

const json = (value, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const errorResponse = (message, status) =>
  json({ schema_version: SCHEMA_VERSION, error: message }, status);

export async function boundedJSON(request) {
  const declared = Number(request.headers.get("Content-Length"));
  if (Number.isFinite(declared) && declared > BODY_MAX_BYTES) {
    throw Object.assign(new Error("request body too large"), { status: 413 });
  }
  const body = await request.arrayBuffer();
  if (body.byteLength > BODY_MAX_BYTES) {
    throw Object.assign(new Error("request body too large"), { status: 413 });
  }
  try {
    return JSON.parse(new TextDecoder().decode(body));
  } catch {
    throw Object.assign(new Error("invalid JSON body"), { status: 400 });
  }
}

function registryUnavailable() {
  return errorResponse("realm email alias registry is unavailable", 503);
}

async function callRegistry(env, path, body) {
  const stub = realmEmailAliasRegistryStub(env);
  if (!stub) return registryUnavailable();
  try {
    return await stub.fetch(`https://realm-email-alias.internal${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    return registryUnavailable();
  }
}

function configuredManagedRealmEmailDomains(env) {
  try {
    return managedRealmEmailDomains(env);
  } catch {
    return null;
  }
}

function managedRealmEmailDomain(env) {
  try {
    return managedRealmEmailPrimaryDomain(env);
  } catch {
    return null;
  }
}

function realmEmailAliasActivationEnabled(env) {
  return String(env?.CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED ?? "") === "true";
}

function timingSafeEqual(left, right) {
  const encoder = new TextEncoder();
  const leftBytes = encoder.encode(left);
  const rightBytes = encoder.encode(right);
  if (leftBytes.byteLength !== rightBytes.byteLength) return false;
  if (typeof crypto.subtle.timingSafeEqual === "function") {
    return crypto.subtle.timingSafeEqual(leftBytes, rightBytes);
  }
  let different = 0;
  for (let index = 0; index < leftBytes.byteLength; index += 1) {
    different |= leftBytes[index] ^ rightBytes[index];
  }
  return different === 0;
}

async function liveCellForAccount(env, accountID) {
  const route = await env.DIRECTORY.get(`acct:${accountID}`, { type: "json" });
  if (!route?.cell) return null;
  const cell = await env.DIRECTORY.get(`cell:${route.cell}`, { type: "json" });
  if (!cell?.endpoint) return null;
  let endpoint;
  try {
    endpoint = new URL(String(cell.endpoint));
  } catch {
    return null;
  }
  if (endpoint.protocol !== "https:" || endpoint.username || endpoint.password ||
      endpoint.search || endpoint.hash || !endpoint.hostname ||
      (endpoint.pathname !== "/" && endpoint.pathname !== "")) {
    return null;
  }
  return { ...cell, endpoint: endpoint.toString().replace(/\/$/, "") };
}

async function fetchCellJSON(fetchImpl, url, authorization) {
  const response = await fetchImpl(url, {
    method: "GET",
    headers: { Authorization: authorization },
    signal: AbortSignal.timeout(15_000),
  });
  const text = await response.text();
  let body = null;
  try {
    body = JSON.parse(text);
  } catch {
    // A malformed cell response is never treated as authorization.
  }
  return { response, body, text };
}

export async function authenticateRealmOperator(
  request,
  env,
  accountID,
  realmID,
  fetchImpl,
  resource = "email aliases",
  schemaVersion = SCHEMA_VERSION,
) {
  const operatorError = (message, status) =>
    json({ schema_version: schemaVersion, error: message }, status);
  const authorization = request.headers.get("Authorization") ?? "";
  if (!authorization.startsWith("Bearer ")) {
    return { response: operatorError("operator token required", 401) };
  }
  const cell = await liveCellForAccount(env, accountID);
  if (!cell) {
    const archived = await env.DIRECTORY.get(`archived:${accountID}`, {
      type: "json",
    });
    return {
      response: operatorError(
        archived
          ? `account is archived — restore before managing ${resource}`
          : "unknown account",
        archived ? 409 : 404,
      ),
    };
  }
  let whoami;
  let account;
  let realms = null;
  try {
    [whoami, account, realms] = await Promise.all([
      fetchCellJSON(fetchImpl, `${cell.endpoint}/v1/whoami`, authorization),
      fetchCellJSON(fetchImpl, `${cell.endpoint}/v1/account`, authorization),
      realmID === null
        ? null
        : fetchCellJSON(fetchImpl, `${cell.endpoint}/v1/realms`, authorization),
    ]);
  } catch {
    return { response: operatorError("account cell is unreachable", 502) };
  }
  for (const result of [whoami, account, realms].filter(Boolean)) {
    if (!result.response.ok) {
      const status = [401, 403].includes(result.response.status)
        ? result.response.status
        : 502;
      return {
        response: operatorError(
          status === 401 ? "unauthorized" :
          status === 403 ? "account is not active" :
          `account cell rejected ${resource} authorization`,
          status,
        ),
      };
    }
  }
  if (whoami.body?.principal?.account_id !== accountID ||
      typeof whoami.body?.principal?.operator_id !== "string" ||
      account.body?.account?.id !== accountID) {
    return { response: operatorError("operator account mismatch", 403) };
  }
  if (realmID !== null) {
    const ownsRealm = Array.isArray(realms.body?.realms) &&
      realms.body.realms.some((realm) => realm?.id === realmID);
    if (!ownsRealm) {
      return { response: operatorError("realm not found in account", 404) };
    }
  }
  const snapshot = {
    features: account.body.account.plan_features ?? [],
    limits: account.body.account.plan_limits ?? {},
    revision: account.body.account.plan_snapshot_revision ?? 0,
    snapshot_hash: account.body.account.plan_snapshot_hash ?? "",
  };
  return {
    actor: {
      kind: "account_operator",
      id: whoami.body.principal.operator_id,
    },
    snapshot,
  };
}

async function fetchPlanSnapshot(env, accountID, fetchImpl) {
  const cell = await liveCellForAccount(env, accountID);
  if (!cell?.provision_token) return null;
  try {
    const result = await fetchCellJSON(
      fetchImpl,
      `${cell.endpoint}/v1/accounts/${accountID}:plan`,
      `Bearer ${cell.provision_token}`,
    );
    return result.response.ok && result.body?.account_id === accountID
      ? result.body
      : null;
  } catch {
    return null;
  }
}

export function matchRealmEmailAliasCustomerPath(pathname) {
  return pathname.match(CUSTOMER_REQUESTS_PATH);
}

export function matchRealmEmailCanonicalClosePath(pathname) {
  return pathname.match(CUSTOMER_REALM_CLOSE_PATH);
}

export function matchRealmEmailRoutePath(pathname) {
  return pathname.match(EDGE_ROUTE_PATH);
}

export function isRealmEmailRoutePath(pathname) {
  return pathname === EDGE_ROUTE_PREFIX ||
    pathname.startsWith(`${EDGE_ROUTE_PREFIX}/`);
}

export async function handleRealmEmailRouteRequest(request, env, match) {
  if (request.method !== "GET") return errorResponse("method not allowed", 405);
  const configured = String(env?.CONTROL_PLANE_EDGE_TOKEN ?? "");
  const authorization = request.headers.get("Authorization") ?? "";
  if (!configured || configured !== configured.trim() ||
      configured.length < 16 || configured.length > 8_192 ||
      !authorization.startsWith("Bearer ") ||
      !timingSafeEqual(authorization.slice(7).trim(), configured)) {
    return errorResponse("unauthorized", 401);
  }
  if (!match) return errorResponse("invalid realm email route", 400);
  let domain;
  let realmLabel;
  try {
    domain = decodeURIComponent(match[1]);
    realmLabel = decodeURIComponent(match[2]);
  } catch {
    return errorResponse("invalid realm email route", 400);
  }
  const domains = configuredManagedRealmEmailDomains(env);
  if (!domains) {
    return errorResponse("managed agent email domains are not configured", 503);
  }
  if (!domains.includes(domain)) {
    return errorResponse("realm email route not found", 404);
  }
  return callRegistry(env, "/route/get", {
    domain,
    realm_label: realmLabel,
  });
}

export function isRealmEmailAliasAdminPath(pathname) {
  return pathname === ADMIN_REQUESTS_PATH ||
    pathname === ADMIN_COUNTER_REBUILD_PATH ||
    pathname === ADMIN_ALIASES_PATH ||
    pathname === ADMIN_INTERNAL_ASSIGN_PATH ||
    pathname === ADMIN_RESERVED_PATH ||
    pathname === ADMIN_AUDIT_PATH ||
    ADMIN_REQUEST_ACTION_PATH.test(pathname) ||
    ADMIN_ALIAS_ACTION_PATH.test(pathname) ||
    ADMIN_ALIAS_ABORT_PATH.test(pathname) ||
    ADMIN_REALM_CLOSE_PATH.test(pathname) ||
    ADMIN_RESERVED_ITEM_PATH.test(pathname);
}

export async function handleRealmEmailCanonicalCloseRequest(
  request,
  env,
  match,
  fetchImpl = fetch,
) {
  if (request.method !== "POST") {
    return errorResponse("method not allowed", 405);
  }
  if (!match) return errorResponse("invalid realm close target", 400);
  const accountID = match[1];
  const realmID = match[2];
  const authenticated = await authenticateRealmOperator(
    request,
    env,
    accountID,
    null,
    fetchImpl,
  );
  if (authenticated.response) return authenticated.response;
  let body;
  try {
    body = await boundedJSON(request);
  } catch (error) {
    return errorResponse(error.message, error.status ?? 400);
  }
  const domain = managedRealmEmailDomain(env);
  if (!domain) {
    return errorResponse("managed agent email domain is not configured", 503);
  }
  return callRegistry(env, "/canonical/realm-close", {
    actor: authenticated.actor,
    account_id: accountID,
    realm_id: realmID,
    domain,
    idempotency_key: body?.idempotency_key,
  });
}

export async function handleRealmEmailAliasCustomerRequest(
  request,
  env,
  match,
  fetchImpl = fetch,
) {
  if (!["GET", "POST"].includes(request.method)) {
    return errorResponse("method not allowed", 405);
  }
  const accountID = match[1];
  const realmID = match[2];
  const authenticated = await authenticateRealmOperator(
    request,
    env,
    accountID,
    realmID,
    fetchImpl,
  );
  if (authenticated.response) return authenticated.response;
  if (request.method === "GET") {
    return callRegistry(env, "/request/list", {
      actor: authenticated.actor,
      account_id: accountID,
      realm_id: realmID,
      cursor: new URL(request.url).searchParams.get("cursor") || undefined,
    });
  }
  if (!realmEmailAliasActivationEnabled(env)) {
    return errorResponse(
      "realm email aliases are not activated on the managed email edge",
      409,
    );
  }
  let body;
  try {
    body = await boundedJSON(request);
  } catch (error) {
    return errorResponse(error.message, error.status ?? 400);
  }
  const entitlement = realmEmailAliasEntitlement(authenticated.snapshot);
  const domain = managedRealmEmailDomain(env);
  if (!domain) {
    return errorResponse("managed agent email domain is not configured", 503);
  }
  return callRegistry(env, "/request/create", {
    actor: authenticated.actor,
    account_id: accountID,
    realm_id: realmID,
    alias: body?.alias,
    domain,
    activation_enabled: true,
    idempotency_key: body?.idempotency_key,
    feature_enabled: entitlement.enabled,
    alias_limit: entitlement.limit,
    plan_revision: authenticated.snapshot.revision,
    plan_snapshot_hash: authenticated.snapshot.snapshot_hash,
  });
}

export async function handleRealmEmailAliasAdminRequest(
  request,
  env,
  url,
  admin,
  fetchImpl = fetch,
) {
  // The Worker performs token authentication before entering this handler.
  // Keep a second narrow boundary check so direct invocation can never turn a
  // missing authenticated principal into an unattributed registry mutation.
  if (typeof admin?.admin_id !== "string" ||
      admin.admin_id.trim().length === 0 || admin.admin_id.length > 128) {
    return errorResponse("unauthorized", 401);
  }
  const actor = { kind: "platform_admin", id: admin.admin_id };

  const realmClose = url.pathname.match(ADMIN_REALM_CLOSE_PATH);
  if (realmClose) {
    if (request.method !== "POST") {
      return errorResponse("method not allowed", 405);
    }
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    const domain = managedRealmEmailDomain(env);
    if (!domain) {
      return errorResponse("managed agent email domain is not configured", 503);
    }
    return callRegistry(env, "/canonical/realm-close", {
      actor,
      account_id: realmClose[1],
      realm_id: realmClose[2],
      domain,
      idempotency_key: body?.idempotency_key,
    });
  }

  if (url.pathname === ADMIN_COUNTER_REBUILD_PATH) {
    if (request.method !== "POST") {
      return errorResponse("method not allowed", 405);
    }
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    return callRegistry(env, "/counter/rebuild", {
      actor,
      idempotency_key: body?.idempotency_key,
      reason: body?.reason,
    });
  }

  if (url.pathname === ADMIN_REQUESTS_PATH) {
    if (request.method !== "GET") return errorResponse("method not allowed", 405);
    return callRegistry(env, "/request/admin-list", {
      actor,
      status: url.searchParams.get("status") || undefined,
      account_id: url.searchParams.get("account_id") || undefined,
      realm_id: url.searchParams.get("realm_id") || undefined,
      cursor: url.searchParams.get("cursor") || undefined,
    });
  }

  const requestAction = url.pathname.match(ADMIN_REQUEST_ACTION_PATH);
  if (requestAction) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    if (requestAction[2] === "reject") {
      return callRegistry(env, "/request/reject", {
        actor,
        request_id: requestAction[1],
        idempotency_key: body?.idempotency_key,
        reason: body?.reason,
      });
    }
    if (!realmEmailAliasActivationEnabled(env)) {
      return errorResponse(
        "realm email aliases are not activated on the managed email edge",
        409,
      );
    }
    const shown = await callRegistry(env, "/request/get", {
      actor,
      request_id: requestAction[1],
    });
    if (!shown.ok) return shown;
    const shownBody = await shown.json();
    const snapshot = await fetchPlanSnapshot(
      env,
      shownBody.request.account_id,
      fetchImpl,
    );
    if (!snapshot) {
      return errorResponse("could not verify the account's current alias plan", 502);
    }
    const entitlement = realmEmailAliasEntitlement(snapshot);
    return callRegistry(env, "/request/approve", {
      actor,
      request_id: requestAction[1],
      idempotency_key: body?.idempotency_key,
      reason: body?.reason,
      activation_enabled: true,
      feature_enabled: entitlement.enabled,
      alias_limit: entitlement.limit,
      plan_revision: snapshot.revision ?? 0,
      plan_snapshot_hash: snapshot.snapshot_hash ?? "",
    });
  }

  if (url.pathname === ADMIN_ALIASES_PATH) {
    if (request.method !== "GET") return errorResponse("method not allowed", 405);
    return callRegistry(env, "/alias/list", {
      actor,
      status: url.searchParams.get("status") || undefined,
      account_id: url.searchParams.get("account_id") || undefined,
      realm_id: url.searchParams.get("realm_id") || undefined,
      cursor: url.searchParams.get("cursor") || undefined,
    });
  }

  const aliasAction = url.pathname.match(ADMIN_ALIAS_ACTION_PATH);
  if (aliasAction) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    if (aliasAction[2] === "reactivate" &&
        !realmEmailAliasActivationEnabled(env)) {
      return errorResponse(
        "realm email aliases are not activated on the managed email edge",
        409,
      );
    }
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    return callRegistry(env, "/alias/mutate", {
      actor,
      alias: aliasAction[1],
      action: aliasAction[2],
      activation_enabled: realmEmailAliasActivationEnabled(env),
      idempotency_key: body?.idempotency_key,
      reason: body?.reason,
    });
  }

  const aliasAbort = url.pathname.match(ADMIN_ALIAS_ABORT_PATH);
  if (aliasAbort) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    return callRegistry(env, "/alias/abort-internal", {
      actor,
      alias: aliasAbort[1],
      idempotency_key: body?.idempotency_key,
      reason: body?.reason,
    });
  }

  if (url.pathname === ADMIN_INTERNAL_ASSIGN_PATH) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    if (!realmEmailAliasActivationEnabled(env)) {
      return errorResponse(
        "realm email aliases are not activated on the managed email edge",
        409,
      );
    }
    if (!await liveCellForAccount(env, body?.account_id ?? "")) {
      return errorResponse("unknown or archived account", 404);
    }
    const domain = managedRealmEmailDomain(env);
    if (!domain) {
      return errorResponse("managed agent email domain is not configured", 503);
    }
    return callRegistry(env, "/alias/assign-internal", {
      actor,
      account_id: body?.account_id,
      realm_id: body?.realm_id,
      alias: body?.alias,
      domain,
      activation_enabled: true,
      idempotency_key: body?.idempotency_key,
      reason: body?.reason,
    });
  }

  if (url.pathname === ADMIN_RESERVED_PATH) {
    if (request.method === "GET") {
      const enabled = url.searchParams.get("enabled");
      return callRegistry(env, "/reserved/list", {
        actor,
        category: url.searchParams.get("category") || undefined,
        cursor: url.searchParams.get("cursor") || undefined,
        ...(enabled === null ? {} : { enabled: enabled === "true" }),
      });
    }
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    // Authenticated identity and the URL target always win over caller JSON.
    // This keeps audit attribution immutable at the Worker trust boundary.
    return callRegistry(env, "/reserved/create", { ...body, actor });
  }

  const reservedItem = url.pathname.match(ADMIN_RESERVED_ITEM_PATH);
  if (reservedItem) {
    if (request.method === "GET") {
      return callRegistry(env, "/reserved/get", {
        actor,
        name: reservedItem[1],
      });
    }
    if (!["PATCH", "DELETE"].includes(request.method)) {
      return errorResponse("method not allowed", 405);
    }
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    return callRegistry(
      env,
      request.method === "DELETE" ? "/reserved/retire" : "/reserved/update",
      { ...body, actor, name: reservedItem[1] },
    );
  }

  if (url.pathname === ADMIN_AUDIT_PATH) {
    if (request.method !== "GET") return errorResponse("method not allowed", 405);
    const parsedLimit = Number.parseInt(url.searchParams.get("limit") || "100", 10);
    return callRegistry(env, "/audit/list", {
      actor,
      action: url.searchParams.get("action") || undefined,
      limit: Number.isFinite(parsedLimit) ? parsedLimit : 100,
      cursor: url.searchParams.get("cursor") || undefined,
    });
  }

  return errorResponse("not found", 404);
}
