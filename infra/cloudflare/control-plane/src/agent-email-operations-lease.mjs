export const AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA =
  "witself.agent-email-operations-lease.v1";
export const AGENT_EMAIL_OPERATIONS_LEASE_EVIDENCE_SCHEMA =
  "witself.agent-email-operations-lease-evidence.v1";
export const AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY =
  "agent-email:operations-lease:v1";

export const AGENT_EMAIL_OPERATIONS_LEASE_PATHS = Object.freeze({
  acquire: "/v1/email/operations-lease:acquire",
  renew: "/v1/email/operations-lease:renew",
  release: "/v1/email/operations-lease:release",
});

export const AGENT_EMAIL_OPERATIONS_LEASE_INTERNAL_PATHS = Object.freeze({
  acquire: "/operations-lease/acquire",
  renew: "/operations-lease/renew",
  release: "/operations-lease/release",
});

export const AGENT_EMAIL_OPERATIONS = Object.freeze([
  "catch_all_routing_apply",
  "control_plane_deploy",
  "email_edge_deploy",
  "email_edge_rollback",
  "primary_routing_apply",
  "route_signing_secret_provision",
]);

export const AGENT_EMAIL_OPERATIONS_LEASE_MIN_TTL_SECONDS = 30;
export const AGENT_EMAIL_OPERATIONS_LEASE_MAX_TTL_SECONDS = 300;

const UUID_V4 =
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const OPERATIONS = new Set(AGENT_EMAIL_OPERATIONS);

const REQUEST_KEYS = Object.freeze({
  acquire: ["holder_id", "operation", "schema_version", "ttl_seconds"],
  renew: ["generation", "holder_id", "schema_version", "ttl_seconds"],
  release: ["generation", "holder_id", "schema_version"],
});

export class AgentEmailOperationsLeaseError extends Error {
  constructor(message, status = 400, code = "agent_email_operations_lease_invalid") {
    super(message);
    this.name = "AgentEmailOperationsLeaseError";
    this.status = status;
    this.code = code;
  }
}

export function agentEmailOperationsLeaseJSON(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "Cache-Control": "private, no-store",
      "Content-Type": "application/json",
    },
  });
}

export function agentEmailOperationsLeaseErrorResponse(error) {
  const recognized = error instanceof AgentEmailOperationsLeaseError;
  return agentEmailOperationsLeaseJSON({
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    error: {
      code: recognized
        ? error.code
        : "agent_email_operations_lease_unavailable",
      message: recognized
        ? error.message
        : "agent email operations lease authority is unavailable",
    },
  }, recognized ? error.status : 503);
}

function fail(message, status, code) {
  throw new AgentEmailOperationsLeaseError(message, status, code);
}

function exactKeys(value, expected) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify(expected);
}

function validGeneration(value) {
  return Number.isSafeInteger(value) && value >= 1;
}

function validTTL(value) {
  return Number.isSafeInteger(value) &&
    value >= AGENT_EMAIL_OPERATIONS_LEASE_MIN_TTL_SECONDS &&
    value <= AGENT_EMAIL_OPERATIONS_LEASE_MAX_TTL_SECONDS;
}

function canonicalTimestamp(value) {
  if (typeof value !== "string" || value.length < 20 || value.length > 32) {
    return false;
  }
  const milliseconds = Date.parse(value);
  return Number.isSafeInteger(milliseconds) && milliseconds >= 0 &&
    new Date(milliseconds).toISOString() === value;
}

function requestAction(path) {
  for (const [action, candidate] of Object.entries(
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS,
  )) {
    if (path === candidate) return action;
  }
  for (const [action, candidate] of Object.entries(
    AGENT_EMAIL_OPERATIONS_LEASE_INTERNAL_PATHS,
  )) {
    if (path === candidate) return action;
  }
  return null;
}

export function isAgentEmailOperationsLeasePath(path) {
  return requestAction(path) !== null;
}

export function internalAgentEmailOperationsLeasePath(path) {
  const action = requestAction(path);
  return action ? AGENT_EMAIL_OPERATIONS_LEASE_INTERNAL_PATHS[action] : null;
}

export function validateAgentEmailOperationsLeaseRequest(path, input) {
  const action = requestAction(path);
  if (!action) {
    fail(
      "agent email operations lease endpoint not found",
      404,
      "agent_email_operations_lease_not_found",
    );
  }
  if (!exactKeys(input, REQUEST_KEYS[action]) ||
      input.schema_version !== AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA ||
      !UUID_V4.test(String(input.holder_id ?? ""))) {
    fail(
      "agent email operations lease request is invalid",
      400,
      "agent_email_operations_lease_invalid",
    );
  }
  if (action === "acquire") {
    if (!OPERATIONS.has(input.operation) || !validTTL(input.ttl_seconds)) {
      fail(
        "agent email operations lease request is invalid",
        400,
        "agent_email_operations_lease_invalid",
      );
    }
  } else {
    if (!validGeneration(input.generation) ||
        (action === "renew" && !validTTL(input.ttl_seconds))) {
      fail(
        "agent email operations lease request is invalid",
        400,
        "agent_email_operations_lease_invalid",
      );
    }
  }
  return Object.freeze({ action, ...input });
}

export function validateAgentEmailOperationsLeaseResponse(
  path,
  status,
  body,
  request = null,
) {
  const action = requestAction(path);
  if (!action || !body || typeof body !== "object" || Array.isArray(body) ||
      body.schema_version !== AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA) {
    fail(
      "agent email operations lease response is invalid",
      503,
      "agent_email_operations_lease_response_invalid",
    );
  }
  if (status < 200 || status >= 300) {
    if (!exactKeys(body, ["error", "schema_version"]) ||
        !exactKeys(body.error, ["code", "message"]) ||
        !/^agent_email_operations_lease_[a-z0-9_]{1,80}$/.test(
          String(body.error.code ?? ""),
        ) || typeof body.error.message !== "string" ||
        body.error.message.length < 1 || body.error.message.length > 256) {
      fail(
        "agent email operations lease response is invalid",
        503,
        "agent_email_operations_lease_response_invalid",
      );
    }
    return structuredClone(body);
  }
  if (!exactKeys(body, ["lease", "schema_version"]) ||
      !body.lease || typeof body.lease !== "object" ||
      Array.isArray(body.lease)) {
    fail(
      "agent email operations lease response is invalid",
      503,
      "agent_email_operations_lease_response_invalid",
    );
  }
  const lease = body.lease;
  if (action === "release") {
    if (status !== 200 || !exactKeys(lease, [
      "already_released",
      "generation",
      "operation",
      "released_at",
      "state",
    ]) || lease.state !== "released" || !validGeneration(lease.generation) ||
        !OPERATIONS.has(lease.operation) ||
        !canonicalTimestamp(lease.released_at) ||
        typeof lease.already_released !== "boolean" ||
        (request && lease.generation !== request.generation)) {
      fail(
        "agent email operations lease response is invalid",
        503,
        "agent_email_operations_lease_response_invalid",
      );
    }
  } else {
    const expectedStatus = action === "acquire" ? 201 : 200;
    if (status !== expectedStatus || !exactKeys(lease, [
      "acquired_at",
      "expires_at",
      "generation",
      "holder_id",
      "operation",
      "state",
    ]) || lease.state !== "active" || !validGeneration(lease.generation) ||
        !UUID_V4.test(String(lease.holder_id ?? "")) ||
        !OPERATIONS.has(lease.operation) ||
        !canonicalTimestamp(lease.acquired_at) ||
        !canonicalTimestamp(lease.expires_at) ||
        Date.parse(lease.expires_at) <= Date.parse(lease.acquired_at) ||
        (request && lease.holder_id !== request.holder_id) ||
        (request && action === "acquire" &&
          lease.operation !== request.operation) ||
        (request && action === "renew" &&
          lease.generation !== request.generation)) {
      fail(
        "agent email operations lease response is invalid",
        503,
        "agent_email_operations_lease_response_invalid",
      );
    }
  }
  return structuredClone(body);
}

export function agentEmailOperationsLeaseEvidence(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      !validGeneration(value.generation) || !OPERATIONS.has(value.operation)) {
    fail(
      "agent email operations lease evidence source is invalid",
      400,
      "agent_email_operations_lease_evidence_invalid",
    );
  }
  return Object.freeze({
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_EVIDENCE_SCHEMA,
    generation: value.generation,
    operation: value.operation,
  });
}

export function validateAgentEmailOperationsLeaseEvidence(value, operation) {
  if (!exactKeys(value, ["generation", "operation", "schema_version"]) ||
      value.schema_version !== AGENT_EMAIL_OPERATIONS_LEASE_EVIDENCE_SCHEMA ||
      !validGeneration(value.generation) || !OPERATIONS.has(operation) ||
      value.operation !== operation) {
    fail(
      "agent email operations lease evidence is invalid",
      400,
      "agent_email_operations_lease_evidence_invalid",
    );
  }
  return Object.freeze({ ...value });
}

function emptyState() {
  return {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    generation: 0,
    active: null,
    last_released: null,
  };
}

function validActive(value, generation) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([
      "acquired_at_ms",
      "expires_at_ms",
      "generation",
      "holder_id",
      "operation",
    ]) && value.generation === generation && validGeneration(value.generation) &&
    UUID_V4.test(String(value.holder_id ?? "")) &&
    OPERATIONS.has(value.operation) &&
    Number.isSafeInteger(value.acquired_at_ms) && value.acquired_at_ms >= 0 &&
    Number.isSafeInteger(value.expires_at_ms) &&
    value.expires_at_ms > value.acquired_at_ms;
}

function validReleased(value, generation) {
  return value && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([
      "generation",
      "holder_id",
      "operation",
      "released_at_ms",
    ]) && value.generation === generation && validGeneration(value.generation) &&
    UUID_V4.test(String(value.holder_id ?? "")) &&
    OPERATIONS.has(value.operation) &&
    Number.isSafeInteger(value.released_at_ms) && value.released_at_ms >= 0;
}

function validatedState(value) {
  if (value === undefined) return emptyState();
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([
        "active",
        "generation",
        "last_released",
        "schema_version",
      ]) || value.schema_version !== AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA ||
      !Number.isSafeInteger(value.generation) || value.generation < 0 ||
      (value.active !== null && !validActive(value.active, value.generation)) ||
      (value.last_released !== null &&
        !validReleased(value.last_released, value.generation)) ||
      (value.active !== null && value.last_released !== null)) {
    fail(
      "agent email operations lease authority is unavailable",
      503,
      "agent_email_operations_lease_state_invalid",
    );
  }
  return structuredClone(value);
}

function activeResponse(active) {
  return {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    lease: {
      state: "active",
      generation: active.generation,
      holder_id: active.holder_id,
      operation: active.operation,
      acquired_at: new Date(active.acquired_at_ms).toISOString(),
      expires_at: new Date(active.expires_at_ms).toISOString(),
    },
  };
}

function releasedResponse(released, alreadyReleased) {
  return {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    lease: {
      state: "released",
      generation: released.generation,
      operation: released.operation,
      released_at: new Date(released.released_at_ms).toISOString(),
      already_released: alreadyReleased,
    },
  };
}

function nowMilliseconds(now) {
  const value = now().getTime();
  if (!Number.isSafeInteger(value) || value < 0) {
    fail(
      "agent email operations lease clock is unavailable",
      503,
      "agent_email_operations_lease_clock_invalid",
    );
  }
  return value;
}

export class AgentEmailOperationsLeaseRuntime {
  constructor(storage, { now = () => new Date() } = {}) {
    this.storage = storage;
    this.now = now;
  }

  async execute(path, rawInput) {
    const input = validateAgentEmailOperationsLeaseRequest(path, rawInput);
    return this.storage.transaction(async (transaction) => {
      const state = validatedState(await transaction.get(
        AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
      ));
      const currentTime = nowMilliseconds(this.now);
      if (input.action === "acquire") {
        if (state.active && state.active.expires_at_ms > currentTime) {
          fail(
            "another agent email operation already holds the lease",
            409,
            "agent_email_operations_lease_held",
          );
        }
        if (state.generation >= Number.MAX_SAFE_INTEGER) {
          fail(
            "agent email operations lease generation is exhausted",
            503,
            "agent_email_operations_lease_generation_exhausted",
          );
        }
        const active = {
          generation: state.generation + 1,
          holder_id: input.holder_id,
          operation: input.operation,
          acquired_at_ms: currentTime,
          expires_at_ms: currentTime + input.ttl_seconds * 1000,
        };
        await transaction.put(AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY, {
          schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
          generation: active.generation,
          active,
          last_released: null,
        });
        return { status: 201, body: activeResponse(active) };
      }

      if (input.action === "renew") {
        if (!state.active || state.active.expires_at_ms <= currentTime ||
            state.active.generation !== input.generation ||
            state.active.holder_id !== input.holder_id) {
          fail(
            "agent email operations lease fence does not match the active holder",
            409,
            "agent_email_operations_lease_fence_mismatch",
          );
        }
        if (state.active.expires_at_ms >= Number.MAX_SAFE_INTEGER) {
          fail(
            "agent email operations lease expiry is exhausted",
            503,
            "agent_email_operations_lease_generation_exhausted",
          );
        }
        const active = {
          ...state.active,
          expires_at_ms: Math.max(
            currentTime + input.ttl_seconds * 1000,
            state.active.expires_at_ms + 1,
          ),
        };
        await transaction.put(
          AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY,
          { ...state, active },
        );
        return { status: 200, body: activeResponse(active) };
      }

      if (state.active && state.active.generation === input.generation &&
          state.active.holder_id === input.holder_id) {
        const released = {
          generation: state.active.generation,
          holder_id: state.active.holder_id,
          operation: state.active.operation,
          released_at_ms: currentTime,
        };
        await transaction.put(AGENT_EMAIL_OPERATIONS_LEASE_STORAGE_KEY, {
          ...state,
          active: null,
          last_released: released,
        });
        return { status: 200, body: releasedResponse(released, false) };
      }
      if (!state.active && state.last_released &&
          state.last_released.generation === input.generation &&
          state.last_released.holder_id === input.holder_id) {
        return {
          status: 200,
          body: releasedResponse(state.last_released, true),
        };
      }
      fail(
        "agent email operations lease fence does not match the active holder",
        409,
        "agent_email_operations_lease_fence_mismatch",
      );
    });
  }
}
