import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_EMAIL_OPERATIONS,
  AGENT_EMAIL_OPERATIONS_LEASE_PATHS,
  AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
  agentEmailOperationsLeaseEvidence,
  validateAgentEmailOperationsLeaseEvidence,
} from "../src/agent-email-operations-lease.mjs";
import {
  handleAgentEmailOperationsLeaseRequest,
} from "../src/realm-email-alias-api.mjs";
import {
  DurableRealmEmailAliasRegistry,
} from "../src/realm-email-alias-runtime.mjs";
import {
  AgentEmailOperationsLeaseClientError,
  withAgentEmailOperationsLease,
} from "../scripts/agent-email-operations-lease-client.mjs";
import {
  runLeaseGuardedCommand,
} from "../scripts/agent-email-lease-guarded-command.mjs";

const TOKEN = "edge-token-at-least-16-characters";
const FIRST_HOLDER = "11111111-1111-4111-8111-111111111111";
const SECOND_HOLDER = "22222222-2222-4222-8222-222222222222";
const THIRD_HOLDER = "33333333-3333-4333-8333-333333333333";

class Storage {
  constructor() {
    this.values = new Map();
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map([...this.values]
      .filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, structuredClone(value)]));
  }

  async transaction(callback) {
    const staged = new Map([...this.values]
      .map(([key, value]) => [key, structuredClone(value)]));
    const transaction = {
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    };
    const result = await callback(transaction);
    this.values = staged;
    return result;
  }
}

function leaseEnvironment() {
  let currentTime = Date.UTC(2026, 7, 9, 12, 0, 0);
  const runtime = new DurableRealmEmailAliasRegistry(
    { storage: new Storage(), id: { name: "global" } },
    {},
    { now: () => new Date(currentTime) },
  );
  const namespace = {
    idFromName: (name) => name,
    get: () => ({
      fetch: (request, init) => runtime.fetch(
        typeof request === "string" ? new Request(request, init) : request,
      ),
    }),
  };
  const env = {
    CONTROL_PLANE_EDGE_TOKEN: TOKEN,
    REALM_EMAIL_ALIASES: namespace,
  };
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const request = new Request(url, init);
    calls.push(new URL(request.url).pathname);
    return handleAgentEmailOperationsLeaseRequest(
      request,
      env,
      new URL(request.url),
    );
  };
  return {
    env,
    fetchImpl,
    calls,
    advance(milliseconds) {
      currentTime += milliseconds;
    },
  };
}

function leaseBody(holderID, operation = "control_plane_deploy", ttl = 30) {
  return {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    holder_id: holderID,
    operation,
    ttl_seconds: ttl,
  };
}

async function apiCall(env, path, body, token = TOKEN) {
  return handleAgentEmailOperationsLeaseRequest(new Request(
    `https://self.example${path}`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(body),
    },
  ), env);
}

test("operations lease API is exact, edge-token authenticated, and no-store", async () => {
  assert.deepEqual(AGENT_EMAIL_OPERATIONS, [
    "catch_all_routing_apply",
    "control_plane_deploy",
    "email_edge_deploy",
    "email_edge_rollback",
    "email_routing_settings_apply",
    "primary_routing_apply",
    "relay_signing_key_provision",
    "route_signing_secret_provision",
  ]);
  const harness = leaseEnvironment();
  const unauthorized = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(FIRST_HOLDER),
    "wrong-token-at-least-16-characters",
  );
  assert.equal(unauthorized.status, 401);
  assert.equal(unauthorized.headers.get("Cache-Control"), "private, no-store");
  assert.deepEqual(await unauthorized.json(), {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    error: {
      code: "agent_email_operations_lease_unauthorized",
      message: "unauthorized",
    },
  });

  const malformed = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    { ...leaseBody(FIRST_HOLDER), unexpected: true },
  );
  assert.equal(malformed.status, 400);

  const acquired = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(FIRST_HOLDER),
  );
  assert.equal(acquired.status, 201);
  assert.equal(acquired.headers.get("Cache-Control"), "private, no-store");
  assert.deepEqual(await acquired.json(), {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    lease: {
      state: "active",
      generation: 1,
      holder_id: FIRST_HOLDER,
      operation: "control_plane_deploy",
      acquired_at: "2026-08-09T12:00:00.000Z",
      expires_at: "2026-08-09T12:00:30.000Z",
    },
  });

  const held = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(SECOND_HOLDER, "email_edge_deploy"),
  );
  assert.equal(held.status, 409);
  const heldText = await held.text();
  assert.doesNotMatch(heldText, /11111111|22222222/);
  assert.deepEqual(JSON.parse(heldText), {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    error: {
      code: "agent_email_operations_lease_held",
      message: "another agent email operation already holds the lease",
    },
  });
});

test("lease expiry advances generation and stale holders cannot renew or release", async () => {
  const harness = leaseEnvironment();
  assert.equal((await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(FIRST_HOLDER),
  )).status, 201);

  const staleRenew = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.renew,
    {
      schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
      holder_id: SECOND_HOLDER,
      generation: 1,
      ttl_seconds: 30,
    },
  );
  assert.equal(staleRenew.status, 409);

  harness.advance(30_000);
  const successor = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(SECOND_HOLDER, "primary_routing_apply"),
  );
  assert.equal(successor.status, 201);
  assert.equal((await successor.json()).lease.generation, 2);

  const staleRelease = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release,
    {
      schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
      holder_id: FIRST_HOLDER,
      generation: 1,
    },
  );
  assert.equal(staleRelease.status, 409);

  const releaseBody = {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    holder_id: SECOND_HOLDER,
    generation: 2,
  };
  const released = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release,
    releaseBody,
  );
  assert.deepEqual(await released.json(), {
    schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
    lease: {
      state: "released",
      generation: 2,
      operation: "primary_routing_apply",
      released_at: "2026-08-09T12:00:30.000Z",
      already_released: false,
    },
  });
  const replayed = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release,
    releaseBody,
  );
  assert.equal((await replayed.json()).lease.already_released, true);

  const next = await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    leaseBody(THIRD_HOLDER, "catch_all_routing_apply"),
  );
  assert.equal((await next.json()).lease.generation, 3);
  assert.equal((await apiCall(
    harness.env,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release,
    releaseBody,
  )).status, 409);
});

test("simultaneous control-plane and edge deploys cannot both enter preflight", async () => {
  const harness = leaseEnvironment();
  let firstEntered;
  let unblockFirst;
  const entered = new Promise((resolve) => {
    firstEntered = resolve;
  });
  const blocked = new Promise((resolve) => {
    unblockFirst = resolve;
  });
  let edgeWorkRan = false;
  const controlPlane = withAgentEmailOperationsLease(
    "control_plane_deploy",
    async () => {
      firstEntered();
      await blocked;
    },
    {
      endpoint: "https://self.example",
      token: TOKEN,
      fetchImpl: harness.fetchImpl,
      randomUUIDImpl: () => FIRST_HOLDER,
      ttlSeconds: 30,
      heartbeatIntervalMs: 1_000,
    },
  );
  await entered;
  await assert.rejects(
    withAgentEmailOperationsLease(
      "email_edge_deploy",
      async () => {
        edgeWorkRan = true;
      },
      {
        endpoint: "https://self.example",
        token: TOKEN,
        fetchImpl: harness.fetchImpl,
        randomUUIDImpl: () => SECOND_HOLDER,
        ttlSeconds: 30,
        heartbeatIntervalMs: 1_000,
      },
    ),
    (error) => error instanceof AgentEmailOperationsLeaseClientError &&
      error.status === 409 &&
      error.code === "agent_email_operations_lease_held",
  );
  assert.equal(edgeWorkRan, false);
  unblockFirst();
  await controlPlane;
});

test("email edge rollback collides with an active edge deployment", async () => {
  const harness = leaseEnvironment();
  let deploymentEntered;
  let finishDeployment;
  const entered = new Promise((resolve) => {
    deploymentEntered = resolve;
  });
  const blocked = new Promise((resolve) => {
    finishDeployment = resolve;
  });
  let rollbackWorkRan = false;
  const deployment = withAgentEmailOperationsLease(
    "email_edge_deploy",
    async () => {
      deploymentEntered();
      await blocked;
    },
    {
      endpoint: "https://self.example",
      token: TOKEN,
      fetchImpl: harness.fetchImpl,
      randomUUIDImpl: () => FIRST_HOLDER,
      ttlSeconds: 30,
      heartbeatIntervalMs: 1_000,
    },
  );
  await entered;
  await assert.rejects(
    withAgentEmailOperationsLease(
      "email_edge_rollback",
      async () => {
        rollbackWorkRan = true;
      },
      {
        endpoint: "https://self.example",
        token: TOKEN,
        fetchImpl: harness.fetchImpl,
        randomUUIDImpl: () => SECOND_HOLDER,
        ttlSeconds: 30,
        heartbeatIntervalMs: 1_000,
      },
    ),
    (error) => error instanceof AgentEmailOperationsLeaseClientError &&
      error.status === 409 &&
      error.code === "agent_email_operations_lease_held",
  );
  assert.equal(rollbackWorkRan, false);
  finishDeployment();
  await deployment;
});

test("email edge rollback proves a final renewal and exact release", async () => {
  const harness = leaseEnvironment();
  await withAgentEmailOperationsLease(
    "email_edge_rollback",
    async () => {},
    {
      endpoint: "https://self.example",
      token: TOKEN,
      fetchImpl: harness.fetchImpl,
      randomUUIDImpl: () => FIRST_HOLDER,
      ttlSeconds: 30,
      heartbeatIntervalMs: 1_000,
    },
  );
  assert.deepEqual(harness.calls, [
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.renew,
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release,
  ]);
});

test("guarded command uses exactly the supplied environment", async () => {
  const sentinel = "WITSELF_GUARDED_PARENT_SENTINEL";
  const included = "WITSELF_GUARDED_INCLUDED";
  const previous = process.env[sentinel];
  process.env[sentinel] = "must-not-inherit";
  try {
    await runLeaseGuardedCommand(
      process.execPath,
      [
        "-e",
        `if (process.env[${JSON.stringify(included)}] !== "supplied-only" ||
            process.env.WITSELF_GUARDED_SECOND !== "second-supplied" ||
            Object.hasOwn(process.env, ${JSON.stringify(sentinel)})) {
          process.exit(23);
        }`,
      ],
      {
        env: {
          [included]: "supplied-only",
          WITSELF_GUARDED_SECOND: "second-supplied",
        },
        timeoutMs: 2_000,
      },
    );
  } finally {
    if (previous === undefined) delete process.env[sentinel];
    else process.env[sentinel] = previous;
  }
});

test("a blocked deployment renews its lease and releases it in finally", async () => {
  const harness = leaseEnvironment();
  let callbackEvidence;
  await withAgentEmailOperationsLease(
    "control_plane_deploy",
    async ({ signal, renew, evidence, fence }) => {
      assert.deepEqual(await renew(), evidence());
      assert.deepEqual(
        validateAgentEmailOperationsLeaseEvidence(
          evidence(),
          "control_plane_deploy",
        ),
        agentEmailOperationsLeaseEvidence(fence),
      );
      callbackEvidence = evidence();
      await runLeaseGuardedCommand(
        process.execPath,
        ["-e", "setTimeout(() => process.exit(0), 90)"],
        { signal, timeoutMs: 2_000 },
      );
    },
    {
      endpoint: "https://self.example",
      token: TOKEN,
      fetchImpl: harness.fetchImpl,
      randomUUIDImpl: () => FIRST_HOLDER,
      ttlSeconds: 30,
      heartbeatIntervalMs: 10,
      requestTimeoutMs: 1_000,
    },
  );
  assert.ok(harness.calls.filter((path) =>
    path === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.renew
  ).length >= 2);
  assert.deepEqual(callbackEvidence, {
    schema_version: "witself.agent-email-operations-lease-evidence.v1",
    generation: 1,
    operation: "control_plane_deploy",
  });
  assert.equal(harness.calls.filter((path) =>
    path === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release
  ).length, 1);
});

test("heartbeat failure terminates a blocked deployment and still attempts release", async () => {
  const harness = leaseEnvironment();
  let releaseCalls = 0;
  const failingFetch = async (url, init) => {
    const path = new URL(url).pathname;
    if (path === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.renew) {
      return new Response(JSON.stringify({
        schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
        error: {
          code: "agent_email_operations_lease_unavailable",
          message: "agent email operations lease authority is unavailable",
        },
      }), {
        status: 503,
        headers: {
          "Cache-Control": "private, no-store",
          "Content-Type": "application/json",
        },
      });
    }
    if (path === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release) releaseCalls++;
    return harness.fetchImpl(url, init);
  };
  await assert.rejects(
    withAgentEmailOperationsLease(
      "email_edge_deploy",
      ({ signal }) => runLeaseGuardedCommand(
        process.execPath,
        ["-e", "setInterval(() => {}, 1000)"],
        { signal, timeoutMs: 5_000 },
      ),
      {
        endpoint: "https://self.example",
        token: TOKEN,
        fetchImpl: failingFetch,
        randomUUIDImpl: () => FIRST_HOLDER,
        ttlSeconds: 30,
        heartbeatIntervalMs: 10,
        requestTimeoutMs: 1_000,
      },
    ),
    (error) => error instanceof AggregateError &&
      error.errors.some((failure) => /lease renewal failed/.test(failure.message)) &&
      error.errors.some((failure) => /stopped after lease loss/.test(failure.message)),
  );
  assert.equal(releaseCalls, 1);
});

test("work failure never skips the exact release attempt", async () => {
  const harness = leaseEnvironment();
  const failure = new Error("simulated guarded deployment failure");
  await assert.rejects(
    withAgentEmailOperationsLease(
      "control_plane_deploy",
      async () => {
        throw failure;
      },
      {
        endpoint: "https://self.example",
        token: TOKEN,
        fetchImpl: harness.fetchImpl,
        randomUUIDImpl: () => FIRST_HOLDER,
        ttlSeconds: 30,
        heartbeatIntervalMs: 1_000,
      },
    ),
    (error) => error === failure,
  );
  assert.equal(harness.calls.at(-1),
    AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release);
});

test("equal renewal expiry is valid exact-fence proof", async () => {
  const harness = leaseEnvironment();
  let acquiredExpiry = "";
  const equalExpiryFetch = async (url, init) => {
    const response = await harness.fetchImpl(url, init);
    const body = await response.json();
    const action = new URL(url).pathname;
    if (action === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.acquire) {
      acquiredExpiry = body.lease.expires_at;
    } else if (action === AGENT_EMAIL_OPERATIONS_LEASE_PATHS.renew) {
      body.lease.expires_at = acquiredExpiry;
    }
    return new Response(JSON.stringify(body), {
      status: response.status,
      headers: response.headers,
    });
  };
  await withAgentEmailOperationsLease(
    "control_plane_deploy",
    async ({ renew }) => {
      await renew();
    },
    {
      endpoint: "https://self.example",
      token: TOKEN,
      fetchImpl: equalExpiryFetch,
      randomUUIDImpl: () => FIRST_HOLDER,
      ttlSeconds: 30,
      heartbeatIntervalMs: 1_000,
    },
  );
});

test("work and release failures are both preserved", async () => {
  const harness = leaseEnvironment();
  const workFailure = new Error("simulated provider failure");
  const releaseFailingFetch = async (url, init) => {
    if (new URL(url).pathname ===
        AGENT_EMAIL_OPERATIONS_LEASE_PATHS.release) {
      return new Response(JSON.stringify({
        schema_version: AGENT_EMAIL_OPERATIONS_LEASE_SCHEMA,
        error: {
          code: "agent_email_operations_lease_unavailable",
          message: "agent email operations lease authority is unavailable",
        },
      }), {
        status: 503,
        headers: {
          "Cache-Control": "private, no-store",
          "Content-Type": "application/json",
        },
      });
    }
    return harness.fetchImpl(url, init);
  };
  await assert.rejects(
    withAgentEmailOperationsLease(
      "catch_all_routing_apply",
      async () => {
        throw workFailure;
      },
      {
        endpoint: "https://self.example",
        token: TOKEN,
        fetchImpl: releaseFailingFetch,
        randomUUIDImpl: () => FIRST_HOLDER,
        ttlSeconds: 30,
        heartbeatIntervalMs: 1_000,
      },
    ),
    (error) => error instanceof AggregateError &&
      error.errors.includes(workFailure) &&
      error.errors.some((failure) => /lease release failed/.test(failure.message)),
  );
});
