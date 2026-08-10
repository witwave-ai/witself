import { randomUUID } from "node:crypto";

import {
  AGENT_EMAIL_OPERATIONS_LEASE_MAX_TTL_SECONDS,
  AGENT_EMAIL_OPERATIONS_LEASE_PATHS,
  AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
  agentEmailOperationsLeaseEvidence,
  validateAgentEmailOperationsLeaseRequest,
  validateAgentEmailOperationsLeaseResponse,
} from "../src/agent-email-operations-lease.mjs";

const DEFAULT_CONTROL_PLANE = "https://self.witwave.ai";
const DEFAULT_TTL_SECONDS = AGENT_EMAIL_OPERATIONS_LEASE_MAX_TTL_SECONDS;
const DEFAULT_HEARTBEAT_INTERVAL_MS = 60_000;
const DEFAULT_REQUEST_TIMEOUT_MS = 15_000;

export class AgentEmailOperationsLeaseClientError extends Error {
  constructor(message, status = 0, code = "agent_email_operations_lease_client_failed") {
    super(message);
    this.name = "AgentEmailOperationsLeaseClientError";
    this.status = status;
    this.code = code;
  }
}

export function agentEmailOperationsLeaseEndpoint(env = process.env) {
  const raw = String(
    env.WITSELF_CONTROL_PLANE ?? env.CONTROL_PLANE_URL ?? DEFAULT_CONTROL_PLANE,
  );
  let endpoint;
  try {
    endpoint = new URL(raw);
  } catch {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease control-plane endpoint is invalid",
    );
  }
  if (raw !== raw.trim() || endpoint.protocol !== "https:" ||
      endpoint.username || endpoint.password || endpoint.search || endpoint.hash ||
      !endpoint.hostname ||
      (endpoint.pathname !== "/" && endpoint.pathname !== "")) {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease control-plane endpoint is invalid",
    );
  }
  return endpoint.toString().replace(/\/$/, "");
}

function leaseToken(env) {
  const token = String(env.CONTROL_PLANE_EDGE_TOKEN ?? "");
  if (token !== token.trim() || token.length < 16 || token.length > 8_192 ||
      /[\r\n\0]/.test(token)) {
    throw new AgentEmailOperationsLeaseClientError(
      "CONTROL_PLANE_EDGE_TOKEN is required for serialized agent email operations",
    );
  }
  return token;
}

function linkedTimeoutSignal(signal, timeoutMs) {
  const controller = new AbortController();
  let timedOut = false;
  const onAbort = () => controller.abort(signal?.reason);
  if (signal?.aborted) onAbort();
  else signal?.addEventListener("abort", onAbort, { once: true });
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort(new Error("lease request timed out"));
  }, timeoutMs);
  return {
    signal: controller.signal,
    timedOut: () => timedOut,
    dispose: () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
    },
  };
}

async function leaseRequest(
  action,
  input,
  {
    endpoint,
    token,
    fetchImpl,
    signal,
    requestTimeoutMs,
    allowLegacyNotFound = false,
  },
) {
  const path = AGENT_EMAIL_OPERATIONS_LEASE_PATHS[action];
  validateAgentEmailOperationsLeaseRequest(path, input);
  const timeout = linkedTimeoutSignal(signal, requestTimeoutMs);
  let response;
  try {
    response = await fetchImpl(`${endpoint}${path}`, {
      method: "POST",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(input),
      redirect: "error",
      signal: timeout.signal,
    });
  } catch (error) {
    throw new AgentEmailOperationsLeaseClientError(
      timeout.timedOut()
        ? "agent email operations lease request timed out"
        : signal?.aborted
        ? "agent email operations lease request was cancelled"
        : "agent email operations lease authority is unreachable",
    );
  } finally {
    timeout.dispose();
  }
  if (allowLegacyNotFound && action === "acquire" &&
      response.status === 404) {
    await response.body?.cancel().catch(() => {});
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease endpoint is not installed",
      404,
      "agent_email_operations_lease_legacy_not_found",
    );
  }
  const cacheControl = String(response.headers.get("Cache-Control") ?? "")
    .toLowerCase()
    .split(",")
    .map((value) => value.trim());
  const declaredLength = Number(response.headers.get("Content-Length"));
  if (!cacheControl.includes("private") ||
      !cacheControl.includes("no-store") ||
      (Number.isFinite(declaredLength) && declaredLength > 16 * 1024) ||
      !/^application\/json(?:\s*;|$)/i.test(
        String(response.headers.get("Content-Type") ?? ""),
      )) {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease authority returned unsafe response headers",
      503,
      "agent_email_operations_lease_response_invalid",
    );
  }
  const text = await response.text();
  let body;
  try {
    if (text.length < 2 ||
        new TextEncoder().encode(text).byteLength > 16 * 1024) {
      throw new Error("response size is invalid");
    }
    body = JSON.parse(text);
  } catch {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease authority returned an invalid response",
      503,
      "agent_email_operations_lease_response_invalid",
    );
  }
  let validated;
  try {
    validated = validateAgentEmailOperationsLeaseResponse(
      path,
      response.status,
      body,
      input,
    );
  } catch {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease authority returned an invalid response",
      503,
      "agent_email_operations_lease_response_invalid",
    );
  }
  if (!response.ok) {
    throw new AgentEmailOperationsLeaseClientError(
      validated.error.message,
      response.status,
      validated.error.code,
    );
  }
  return validated.lease;
}

function heartbeatDelay(milliseconds, signal) {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve(false);
      return;
    }
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve(true);
    }, milliseconds);
    const onAbort = () => {
      clearTimeout(timer);
      resolve(false);
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

export async function withAgentEmailOperationsLease(
  operation,
  work,
  {
    env = process.env,
    endpoint = agentEmailOperationsLeaseEndpoint(env),
    token = leaseToken(env),
    fetchImpl = globalThis.fetch,
    randomUUIDImpl = randomUUID,
    ttlSeconds = DEFAULT_TTL_SECONDS,
    heartbeatIntervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS,
    requestTimeoutMs = DEFAULT_REQUEST_TIMEOUT_MS,
    allowLegacyNotFound = false,
  } = {},
) {
  if (typeof work !== "function" ||
      !Number.isSafeInteger(heartbeatIntervalMs) || heartbeatIntervalMs < 1 ||
      heartbeatIntervalMs >= ttlSeconds * 1000 ||
      !Number.isSafeInteger(requestTimeoutMs) || requestTimeoutMs < 1 ||
      requestTimeoutMs > 60_000) {
    throw new AgentEmailOperationsLeaseClientError(
      "agent email operations lease client configuration is invalid",
    );
  }
  const holderID = randomUUIDImpl();
  const acquireInput = {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    holder_id: holderID,
    operation,
    ttl_seconds: ttlSeconds,
  };
  const acquired = await leaseRequest("acquire", acquireInput, {
    endpoint,
    token,
    fetchImpl,
    requestTimeoutMs,
    allowLegacyNotFound,
  });
  const fence = Object.freeze({
    generation: acquired.generation,
    operation: acquired.operation,
  });
  const stopHeartbeat = new AbortController();
  const abortWork = new AbortController();
  let renewalFailure = null;
  let currentLease = acquired;
  let renewalTail = Promise.resolve();
  let renewalsAccepted = true;
  const evidence = () => agentEmailOperationsLeaseEvidence(fence);
  const renew = () => {
    if (!renewalsAccepted) {
      return Promise.reject(new AgentEmailOperationsLeaseClientError(
        "agent email operations lease is no longer active",
        409,
        "agent_email_operations_lease_fence_mismatch",
      ));
    }
    const attempt = renewalTail.catch(() => {}).then(async () => {
      if (!renewalsAccepted || renewalFailure) {
        throw renewalFailure ?? new AgentEmailOperationsLeaseClientError(
          "agent email operations lease is no longer active",
          409,
          "agent_email_operations_lease_fence_mismatch",
        );
      }
      try {
        const renewed = await leaseRequest("renew", {
          schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
          holder_id: holderID,
          generation: acquired.generation,
          ttl_seconds: ttlSeconds,
        }, {
          endpoint,
          token,
          fetchImpl,
          signal: stopHeartbeat.signal,
          requestTimeoutMs,
        });
        if (renewed.operation !== acquired.operation ||
            renewed.acquired_at !== acquired.acquired_at ||
            Date.parse(renewed.expires_at) <
              Date.parse(currentLease.expires_at)) {
          throw new AgentEmailOperationsLeaseClientError(
            "agent email operations lease renewal changed the active fence",
            503,
            "agent_email_operations_lease_response_invalid",
          );
        }
        currentLease = renewed;
        return evidence();
      } catch (error) {
        if (stopHeartbeat.signal.aborted) throw error;
        renewalFailure ??= new AgentEmailOperationsLeaseClientError(
          "agent email operations lease renewal failed",
          error?.status ?? 0,
          error?.code ?? "agent_email_operations_lease_renewal_failed",
        );
        abortWork.abort(renewalFailure);
        throw renewalFailure;
      }
    });
    renewalTail = attempt;
    return attempt;
  };
  const renewals = (async () => {
    while (await heartbeatDelay(heartbeatIntervalMs, stopHeartbeat.signal)) {
      try {
        await renew();
      } catch {
        if (stopHeartbeat.signal.aborted) break;
        break;
      }
    }
  })();

  let value;
  let workFailure = null;
  let releaseFailure = null;
  try {
    value = await work({
      signal: abortWork.signal,
      fence,
      renew,
      evidence,
    });
    await renew();
  } catch (error) {
    if (error !== renewalFailure) workFailure = error;
  } finally {
    stopHeartbeat.abort();
    await renewals;
    await renewalTail.catch(() => {});
    renewalsAccepted = false;
    try {
      const released = await leaseRequest("release", {
        schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
        holder_id: holderID,
        generation: acquired.generation,
      }, {
        endpoint,
        token,
        fetchImpl,
        requestTimeoutMs,
      });
      if (released.operation !== acquired.operation) {
        throw new AgentEmailOperationsLeaseClientError(
          "agent email operations lease release changed the active fence",
          503,
          "agent_email_operations_lease_response_invalid",
        );
      }
    } catch (error) {
      releaseFailure = new AgentEmailOperationsLeaseClientError(
        "agent email operations lease release failed",
        error?.status ?? 0,
        error?.code ?? "agent_email_operations_lease_release_failed",
      );
    }
  }
  const failures = [...new Set([
    workFailure,
    renewalFailure,
    releaseFailure,
  ].filter(Boolean))];
  if (failures.length > 1) {
    throw new AggregateError(
      failures,
      "agent email operation failed and its durable lease did not settle cleanly",
    );
  }
  if (failures.length === 1) throw failures[0];
  return value;
}
