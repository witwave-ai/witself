import {
  agentEmailCustomDomainEntitlement,
  agentEmailCustomDomainRequestsEnabledForAccount,
  agentEmailDomainRegistryStub,
  runAgentEmailDomainManualVerification,
} from "./agent-email-domain-runtime.mjs";
import {
  authenticateRealmOperator,
  boundedJSON,
} from "./realm-email-alias-api.mjs";

const SCHEMA_VERSION = "witself.agent-email-domain.v1";
const ACCOUNT_ID_PATTERN = "[A-Za-z0-9_-]{1,128}";
const REQUEST_ID_PATTERN = "aedr_[a-z2-7]{16}";

const CUSTOMER_REQUESTS_PATH = new RegExp(
  `^/v1/accounts/(${ACCOUNT_ID_PATTERN})/email-domain-requests$`,
);
const ADMIN_REQUESTS_PATH = "/v1/admin/agent-email-domain-requests";
const ADMIN_REQUEST_PATH = new RegExp(
  `^/v1/admin/agent-email-domain-requests/(${REQUEST_ID_PATTERN})$`,
);
const ADMIN_REQUEST_ACTION_PATH = new RegExp(
  `^/v1/admin/agent-email-domain-requests/(${REQUEST_ID_PATTERN}):(reject|retire|verify)$`,
);
const ADMIN_AUDIT_PATH = "/v1/admin/agent-email-domain-audit";

const json = (value, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const errorResponse = (message, status, code = "") =>
  json({
    schema_version: SCHEMA_VERSION,
    error: message,
    ...(code ? { code } : {}),
  }, status);

function registryUnavailable() {
  return errorResponse("agent email domain registry is unavailable", 503);
}

async function callRegistry(env, path, body) {
  const stub = agentEmailDomainRegistryStub(env);
  if (!stub) return registryUnavailable();
  try {
    return await stub.fetch(`https://agent-email-domain.internal${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    return registryUnavailable();
  }
}

export function matchAgentEmailDomainCustomerPath(pathname) {
  return pathname.match(CUSTOMER_REQUESTS_PATH);
}

export function isAgentEmailDomainAdminPath(pathname) {
  return pathname === ADMIN_REQUESTS_PATH ||
    pathname === ADMIN_AUDIT_PATH ||
    ADMIN_REQUEST_PATH.test(pathname) ||
    ADMIN_REQUEST_ACTION_PATH.test(pathname);
}

export async function handleAgentEmailDomainCustomerRequest(
  request,
  env,
  match,
  fetchImpl = fetch,
) {
  if (!match) return errorResponse("invalid agent email domain route", 400);
  if (!["GET", "POST"].includes(request.method)) {
    return errorResponse("method not allowed", 405);
  }

  const accountID = match[1];
  const authenticated = await authenticateRealmOperator(
    request,
    env,
    accountID,
    null,
    fetchImpl,
    "custom email domains",
    SCHEMA_VERSION,
  );
  if (authenticated.response) return authenticated.response;

  if (request.method === "GET") {
    return callRegistry(env, "/request/list", {
      actor: authenticated.actor,
      account_id: accountID,
      cursor: new URL(request.url).searchParams.get("cursor") || undefined,
    });
  }

  // This is intentionally independent from every managed-domain alias and
  // canonical-delivery gate. Turning on witmail.net cannot accidentally open
  // customer-owned domains, and ordinary code deployments cannot turn this
  // gate on because it is a runtime-only secret.
  if (!agentEmailCustomDomainRequestsEnabledForAccount(env, accountID)) {
    return errorResponse(
      "custom agent email domain requests are not enabled",
      409,
      "custom_domain_requests_disabled",
    );
  }

  let body;
  try {
    body = await boundedJSON(request);
  } catch (error) {
    return errorResponse(error.message, error.status ?? 400);
  }
  const entitlement = agentEmailCustomDomainEntitlement(authenticated.snapshot);
  return callRegistry(env, "/request/create", {
    actor: authenticated.actor,
    account_id: accountID,
    domain: body?.domain,
    idempotency_key: body?.idempotency_key,
    requests_enabled: true,
    feature_enabled: entitlement.enabled,
    domain_limit: entitlement.limit,
    plan_revision: authenticated.snapshot.revision,
    plan_snapshot_hash: authenticated.snapshot.snapshot_hash,
  });
}

export async function handleAgentEmailDomainAdminRequest(
  request,
  env,
  url,
  admin,
  verificationDependencies = {},
) {
  if (typeof admin?.admin_id !== "string" ||
      admin.admin_id.trim().length === 0 || admin.admin_id.length > 128) {
    return errorResponse("unauthorized", 401);
  }
  const actor = { kind: "platform_admin", id: admin.admin_id };

  if (url.pathname === ADMIN_REQUESTS_PATH) {
    if (request.method !== "GET") {
      return errorResponse("method not allowed", 405);
    }
    return callRegistry(env, "/request/admin-list", {
      actor,
      state: url.searchParams.get("state") || undefined,
      account_id: url.searchParams.get("account_id") || undefined,
      domain: url.searchParams.get("domain") || undefined,
      cursor: url.searchParams.get("cursor") || undefined,
    });
  }

  if (url.pathname === ADMIN_AUDIT_PATH) {
    if (request.method !== "GET") {
      return errorResponse("method not allowed", 405);
    }
    const rawLimit = url.searchParams.get("limit");
    if (rawLimit !== null &&
        (!/^(?:[1-9][0-9]{0,2})$/.test(rawLimit) || Number(rawLimit) > 100)) {
      return errorResponse("audit limit must be an integer from 1 to 100", 400);
    }
    const parsedLimit = rawLimit === null ? 100 : Number(rawLimit);
    return callRegistry(env, "/audit/list", {
      actor,
      action: url.searchParams.get("action") || undefined,
      account_id: url.searchParams.get("account_id") || undefined,
      domain: url.searchParams.get("domain") || undefined,
      limit: parsedLimit,
      cursor: url.searchParams.get("cursor") || undefined,
    });
  }

  const item = url.pathname.match(ADMIN_REQUEST_PATH);
  if (item) {
    if (request.method !== "GET") {
      return errorResponse("method not allowed", 405);
    }
    return callRegistry(env, "/request/get", {
      actor,
      request_id: item[1],
    });
  }

  const action = url.pathname.match(ADMIN_REQUEST_ACTION_PATH);
  if (!action) return errorResponse("not found", 404);
  if (request.method !== "POST") {
    return errorResponse("method not allowed", 405);
  }
  let body;
  try {
    body = await boundedJSON(request);
  } catch (error) {
    return errorResponse(error.message, error.status ?? 400);
  }
  const mutation = {
    actor,
    request_id: action[1],
    idempotency_key: body?.idempotency_key,
    reason: body?.reason,
  };
  if (action[2] === "verify") {
    return runAgentEmailDomainManualVerification(
      env,
      mutation,
      verificationDependencies,
    );
  }
  return callRegistry(env, `/request/${action[2]}`, mutation);
}
