const SCHEMA_VERSION = "witself.agent-email-domain-recovery.v1";
const BODY_MAX_BYTES = 16 * 1024;
const RECOVERY_ID_PATTERN = /^aedrec_[a-z2-7]{16}$/;
const STREAM_ID_PATTERN = /^aedj_[a-z2-7]{16,52}$/;
const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;
const SHA256_PATTERN = /^[0-9a-f]{64}$/;
const OBJECT_NAME_PATTERN = /^[A-Za-z0-9._:-]{1,128}$/;

export const AGENT_EMAIL_DOMAIN_RECOVERY_HEADER =
  "X-Witself-Agent-Email-Domain-Recovery";
export const AGENT_EMAIL_DOMAIN_JOURNAL_ADMIN_PATH =
  "/v1/admin/agent-email-domain-journal";
export const AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_PATH =
  "/v1/admin/agent-email-domain-journal:bootstrap";
export const AGENT_EMAIL_DOMAIN_JOURNAL_CHECKPOINT_PATH =
  "/v1/admin/agent-email-domain-journal:checkpoint";
export const AGENT_EMAIL_DOMAIN_RECOVERIES_PATH =
  "/v1/admin/agent-email-domain-recoveries";

const RECOVERY_ITEM_PATH = new RegExp(
  "^/v1/admin/agent-email-domain-recoveries/(aedrec_[a-z2-7]{16})$",
);
const RECOVERY_ACTION_PATH = new RegExp(
  "^/v1/admin/agent-email-domain-recoveries/(aedrec_[a-z2-7]{16}):(advance|verify)$",
);

const json = (value, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const errorResponse = (message, status) =>
  json({ schema_version: SCHEMA_VERSION, error: message }, status);

function timingSafeEqual(left, right) {
  const encoder = new TextEncoder();
  const leftBytes = encoder.encode(left);
  const rightBytes = encoder.encode(right);
  if (leftBytes.byteLength !== rightBytes.byteLength) return false;
  let different = 0;
  for (let index = 0; index < leftBytes.byteLength; index += 1) {
    different |= leftBytes[index] ^ rightBytes[index];
  }
  return different === 0;
}

function recoveryAuthorized(request, env) {
  const configured = String(env?.CP_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN ?? "");
  const presented = request.headers.get(AGENT_EMAIL_DOMAIN_RECOVERY_HEADER) ?? "";
  return configured.length >= 32 && configured.length <= 8_192 &&
    configured === configured.trim() && presented === presented.trim() &&
    timingSafeEqual(configured, presented);
}

async function boundedJSON(request) {
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

function activeObjectName() {
  return "global";
}

function registryStub(env, objectName) {
  const namespace = env?.AGENT_EMAIL_DOMAINS;
  if (!namespace || typeof namespace.idFromName !== "function" ||
      typeof namespace.get !== "function" ||
      !OBJECT_NAME_PATTERN.test(objectName ?? "")) {
    return null;
  }
  return namespace.get(namespace.idFromName(objectName));
}

async function callRegistry(env, objectName, path, body) {
  const stub = registryStub(env, objectName);
  if (!stub) {
    return errorResponse("custom inbound domain registry is unavailable", 503);
  }
  try {
    return await stub.fetch(`https://agent-email-domain.internal${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    return errorResponse("custom inbound domain registry is unavailable", 503);
  }
}

function validAdmin(admin) {
  return typeof admin?.admin_id === "string" &&
    admin.admin_id.trim().length > 0 && admin.admin_id.length <= 128;
}

function validReason(value) {
  return typeof value === "string" && value.trim().length >= 3 &&
    value.trim().length <= 1_024;
}

function exactHead(value) {
  return value && Number.isSafeInteger(value.sequence) && value.sequence >= 1 &&
    SHA256_PATTERN.test(value.hash ?? "");
}

function recoveryTargetName(recoveryID) {
  return `recovery:${recoveryID}`;
}

export function isAgentEmailDomainRecoveryAdminPath(pathname) {
  return pathname === AGENT_EMAIL_DOMAIN_JOURNAL_ADMIN_PATH ||
    pathname === AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_PATH ||
    pathname === AGENT_EMAIL_DOMAIN_JOURNAL_CHECKPOINT_PATH ||
    pathname === AGENT_EMAIL_DOMAIN_RECOVERIES_PATH ||
    RECOVERY_ITEM_PATH.test(pathname) ||
    RECOVERY_ACTION_PATH.test(pathname);
}

export async function handleAgentEmailDomainRecoveryAdminRequest(
  request,
  env,
  url,
  admin,
) {
  if (!validAdmin(admin)) return errorResponse("unauthorized", 401);
  if (!recoveryAuthorized(request, env)) {
    return errorResponse(
      "distinct agent email domain recovery credential required",
      401,
    );
  }
  const active = activeObjectName();
  const actor = { kind: "platform_admin", id: admin.admin_id };

  if (url.pathname === AGENT_EMAIL_DOMAIN_JOURNAL_ADMIN_PATH) {
    if (request.method !== "GET") return errorResponse("method not allowed", 405);
    return callRegistry(env, active, "/journal/status", { actor });
  }

  if (url.pathname === AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_PATH ||
      url.pathname === AGENT_EMAIL_DOMAIN_JOURNAL_CHECKPOINT_PATH) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    if (!validReason(body?.reason) ||
        !IDEMPOTENCY_KEY_PATTERN.test(body?.idempotency_key ?? "")) {
      return errorResponse("reason and idempotency_key are required", 400);
    }
    return callRegistry(
      env,
      active,
      url.pathname === AGENT_EMAIL_DOMAIN_JOURNAL_BOOTSTRAP_PATH
        ? "/journal/bootstrap"
        : "/journal/checkpoint",
      {
        actor,
        reason: body.reason.trim(),
        idempotency_key: body.idempotency_key,
      },
    );
  }

  if (url.pathname === AGENT_EMAIL_DOMAIN_RECOVERIES_PATH) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    if (!RECOVERY_ID_PATTERN.test(body?.recovery_id ?? "") ||
        !STREAM_ID_PATTERN.test(body?.source_stream_id ?? "") ||
        !exactHead(body?.expected_head) || !validReason(body?.reason) ||
        !IDEMPOTENCY_KEY_PATTERN.test(body?.idempotency_key ?? "")) {
      return errorResponse(
        "recovery_id, source_stream_id, exact expected_head, reason, and idempotency_key are required",
        400,
      );
    }
    const target = recoveryTargetName(body.recovery_id);
    if (target === active) {
      return errorResponse("recovery target must not be the active registry", 409);
    }
    return callRegistry(env, target, "/recovery/start", {
      actor,
      recovery_id: body.recovery_id,
      source_stream_id: body.source_stream_id,
      expected_head: body.expected_head,
      reason: body.reason.trim(),
      idempotency_key: body.idempotency_key,
      active_object_name: active,
      target_object_name: target,
    });
  }

  const item = url.pathname.match(RECOVERY_ITEM_PATH);
  if (item) {
    if (request.method !== "GET") return errorResponse("method not allowed", 405);
    return callRegistry(env, recoveryTargetName(item[1]), "/recovery/status", {
      actor,
      recovery_id: item[1],
    });
  }

  const action = url.pathname.match(RECOVERY_ACTION_PATH);
  if (action) {
    if (request.method !== "POST") return errorResponse("method not allowed", 405);
    let body;
    try {
      body = await boundedJSON(request);
    } catch (error) {
      return errorResponse(error.message, error.status ?? 400);
    }
    if (!IDEMPOTENCY_KEY_PATTERN.test(body?.idempotency_key ?? "") ||
        !SHA256_PATTERN.test(body?.expected_action_fence ?? "")) {
      return errorResponse(
        "idempotency_key and expected_action_fence are required",
        400,
      );
    }
    return callRegistry(
      env,
      recoveryTargetName(action[1]),
      action[2] === "advance" ? "/recovery/advance" : "/recovery/verify",
      {
        actor,
        recovery_id: action[1],
        idempotency_key: body.idempotency_key,
        expected_action_fence: body.expected_action_fence,
      },
    );
  }

  return errorResponse("not found", 404);
}
