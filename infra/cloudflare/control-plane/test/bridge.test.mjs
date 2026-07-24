import assert from "node:assert/strict";
import test from "node:test";

import {
  ADMIN_HANDLE_HEADER,
  ADMIN_ID_HEADER,
  containerEnvVars,
  forwardAdminPolicyRequest,
  handleInternalBridgeRequest,
  isInternalBridgePath,
  matchAdminPolicyPath,
  PLAN_LIFECYCLE_ACTIVATE_PATH,
  PLAN_LIFECYCLE_CURSOR_KEY,
  PLAN_LIFECYCLE_PAGE_SIZE,
  restartContainerWithEnvironment,
  runScheduledPlanLifecycle,
} from "../src/bridge.mjs";

class KV {
  constructor(entries = {}, listResponse = null) {
    this.entries = new Map(Object.entries(entries));
    this.listResponse = listResponse;
    this.listCalls = [];
  }

  async get(key, options) {
    if (!this.entries.has(key)) return null;
    const value = this.entries.get(key);
    if (options?.type === "json") {
      return typeof value === "string" ? JSON.parse(value) : value;
    }
    return typeof value === "string" ? value : JSON.stringify(value);
  }

  async list(options) {
    this.listCalls.push(options);
    if (typeof this.listResponse === "function") {
      return this.listResponse(options);
    }
    if (this.listResponse) return this.listResponse;
    const keys = [...this.entries.keys()]
      .filter((name) => name.startsWith(options.prefix))
      .sort()
      .slice(0, options.limit)
      .map((name) => ({ name }));
    return { keys, list_complete: true };
  }

  async put(key, value) {
    this.entries.set(key, value);
  }
}

const bridgeEnv = (entries = {}, listResponse = null) => ({
  INTERNAL_BRIDGE_TOKEN: "bridge-secret",
  DIRECTORY: new KV(entries, listResponse),
});

const bridgeRequest = (path, init = {}) =>
  new Request(`https://self.witwave.ai${path}`, {
    ...init,
    headers: {
      Authorization: "Bearer bridge-secret",
      ...(init.headers ?? {}),
    },
  });

async function responseJSON(response) {
  return JSON.parse(await response.text());
}

test("container env projection is explicit, runtime-only, and opt-in", () => {
  const archiveBinding = { bucket: "must-not-cross" };
  assert.deepEqual(containerEnvVars({
    INTERNAL_BRIDGE_TOKEN: "bridge",
    INTERNAL_BRIDGE_URL: "https://worker.example",
    CP_PLAN_LIFECYCLE_ENABLED: "true",
    CP_R2_ENDPOINT: "https://r2.example",
    CP_R2_BUCKET: "registry",
    CP_R2_ACCESS_KEY: "access",
    CP_R2_SECRET_KEY: "secret",
    CP_R2_PREFIX: "registry/",
    CP_BILLING_PROVIDER: "stripe",
    CP_STRIPE_SECRET_KEY: "stripe-secret-placeholder",
    CP_STRIPE_WEBHOOK_SECRET: "webhook-secret-placeholder",
    CP_STRIPE_SUCCESS_URL: "https://app.example/billing/success",
    CP_STRIPE_CANCEL_URL: "https://app.example/billing/cancel",
    ARCHIVES: archiveBinding,
    FLEET_TOKEN: "must-not-cross",
    WITSELF_CP_STRIPE_SECRET_KEY: "must-not-cross-directly",
  }), {
    WITSELF_CP_BRIDGE_TOKEN: "bridge",
    WITSELF_CP_BRIDGE_URL: "https://worker.example",
    WITSELF_CP_PLAN_LIFECYCLE_ENABLED: "true",
    WITSELF_CP_R2_ENDPOINT: "https://r2.example",
    WITSELF_CP_R2_BUCKET: "registry",
    WITSELF_CP_R2_ACCESS_KEY: "access",
    WITSELF_CP_R2_SECRET_KEY: "secret",
    WITSELF_CP_R2_PREFIX: "registry/",
    WITSELF_CP_BILLING_PROVIDER: "stripe",
    WITSELF_CP_STRIPE_SECRET_KEY: "stripe-secret-placeholder",
    WITSELF_CP_STRIPE_WEBHOOK_SECRET: "webhook-secret-placeholder",
    WITSELF_CP_STRIPE_SUCCESS_URL: "https://app.example/billing/success",
    WITSELF_CP_STRIPE_CANCEL_URL: "https://app.example/billing/cancel",
  });

  assert.deepEqual(containerEnvVars({}), {
    WITSELF_CP_BRIDGE_URL: "https://self.witwave.ai",
  });
  assert.deepEqual(containerEnvVars({
    CP_BILLING_PROVIDER: "",
    CP_STRIPE_SECRET_KEY: "",
    CP_STRIPE_WEBHOOK_SECRET: "",
    CP_STRIPE_SUCCESS_URL: "",
    CP_STRIPE_CANCEL_URL: "",
  }), {
    WITSELF_CP_BRIDGE_URL: "https://self.witwave.ai",
  });
});

test("container activation replaces the process with the fresh env projection", async () => {
  const calls = [];
  const container = {
    defaultPort: 8080,
    envVars: { OLD: "value" },
    async destroy() {
      calls.push({ method: "destroy" });
    },
    async startAndWaitForPorts(options) {
      calls.push({ method: "startAndWaitForPorts", options });
    },
  };
  const projected = {
    WITSELF_CP_BRIDGE_TOKEN: "bridge-secret",
    WITSELF_CP_PLAN_LIFECYCLE_ENABLED: "true",
  };

  await restartContainerWithEnvironment(container, projected);

  assert.deepEqual(calls.map((call) => call.method), [
    "destroy",
    "startAndWaitForPorts",
  ]);
  assert.deepEqual(container.envVars, projected);
  assert.notEqual(container.envVars, projected);
  assert.equal(calls[1].options.ports, 8080);
  assert.equal(
    calls[1].options.startOptions.envVars,
    container.envVars,
  );
  assert.equal(
    calls[1].options.cancellationOptions.instanceGetTimeoutMS,
    2 * 60 * 1000,
  );
  assert.equal(
    calls[1].options.cancellationOptions.portReadyTimeoutMS,
    2 * 60 * 1000,
  );
  assert.equal(
    calls[1].options.cancellationOptions.abort instanceof AbortSignal,
    true,
  );
});

test("bridge path classifiers are exact", () => {
  assert.ok(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/transcript-retention",
  ));
  assert.ok(matchAdminPolicyPath("/v1/admin/accounts/acct_1/plan-override"));
  assert.ok(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/limit-overrides/stored_secret",
  ));
  assert.ok(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/limit-overrides/agents_per_realm",
  ));
  assert.equal(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/limit-overrides/not_a_limit",
  ), null);
  assert.equal(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/limit-overrides/stored_secret/extra",
  ), null);
  assert.equal(matchAdminPolicyPath(
    "/v1/admin/accounts/acct_1/transcript-retention/extra",
  ), null);
  assert.equal(
    isInternalBridgePath(PLAN_LIFECYCLE_ACTIVATE_PATH),
    true,
  );
  assert.equal(isInternalBridgePath("/v1/internal/accounts"), true);
  assert.equal(isInternalBridgePath("/v1/internal/accounts/a:resolve"), true);
  assert.equal(isInternalBridgePath("/v1/accounts/a/plan"), false);
});

test("admin proxy rejects missing bridge configuration", async () => {
  const env = bridgeEnv({ "acct:acct_1": { cell: "cell-a" } });
  delete env.INTERNAL_BRIDGE_TOKEN;
  const response = await forwardAdminPolicyRequest(
    new Request(
      "https://self.witwave.ai/v1/admin/accounts/acct_1/transcript-retention",
    ),
    env,
    { handle: "scott" },
    async () => {
      assert.fail("container must not be called");
    },
  );
  assert.equal(response.status, 503);
});

test("admin proxy rejects archived and unknown accounts before Go", async () => {
  for (const [entries, expected] of [
    [{ "archived:acct_1": { cell: "old" }, "acct:acct_1": { cell: "cell-a" } }, 409],
    [{}, 404],
  ]) {
    let called = false;
    const response = await forwardAdminPolicyRequest(
      new Request(
        "https://self.witwave.ai/v1/admin/accounts/acct_1/plan-override",
      ),
      bridgeEnv(entries),
      { handle: "scott" },
      async () => {
        called = true;
        return new Response();
      },
    );
    assert.equal(response.status, expected);
    assert.equal(called, false);
  }
});

test("admin proxy replaces caller credentials and relays Go response", async () => {
  const request = new Request(
    "https://self.witwave.ai/v1/admin/accounts/acct_1/limit-overrides/agents_per_realm",
    {
      method: "PUT",
      headers: {
        Authorization: "Bearer witself_adm_do-not-forward",
        "Content-Type": "application/json",
        "X-Witself-Admin-ID": "adm_caller_forged",
        "X-Witself-Admin-Handle": "forged",
        "X-Untrusted": "drop-me",
      },
      body: JSON.stringify({ max: 0, reason: "exception" }),
    },
  );
  let forwarded;
  const response = await forwardAdminPolicyRequest(
    request,
    bridgeEnv({ "acct:acct_1": { cell: "cell-a" } }),
    { admin_id: "adm_abcdefghijklmnopqrst", handle: "scott" },
    async (next) => {
      forwarded = next;
      return new Response('{"effective_days":60}', {
        status: 202,
        headers: {
          "Content-Type": "application/json",
          "Retry-After": "2",
          "Set-Cookie": "must-not-relay=1",
        },
      });
    },
  );

  assert.equal(forwarded.method, "PUT");
  assert.equal(forwarded.headers.get("Authorization"), "Bearer bridge-secret");
  assert.equal(
    forwarded.headers.get(ADMIN_ID_HEADER),
    "adm_abcdefghijklmnopqrst",
  );
  assert.equal(forwarded.headers.get(ADMIN_HANDLE_HEADER), "scott");
  assert.equal(forwarded.headers.get("X-Untrusted"), null);
  assert.deepEqual(await forwarded.json(), { max: 0, reason: "exception" });
  assert.equal(response.status, 202);
  assert.equal(response.headers.get("Retry-After"), "2");
  assert.equal(response.headers.get("Set-Cookie"), null);
  assert.deepEqual(await responseJSON(response), { effective_days: 60 });
});

test("admin proxy enforces verbs and bounded bodies", async () => {
  const env = bridgeEnv({ "acct:acct_1": { cell: "cell-a" } });
  const path =
    "https://self.witwave.ai/v1/admin/accounts/acct_1/transcript-retention";
  const wrongMethod = await forwardAdminPolicyRequest(
    new Request(path, { method: "POST" }),
    env,
    { handle: "scott" },
    async () => assert.fail("container must not be called"),
  );
  assert.equal(wrongMethod.status, 405);

  const tooLarge = await forwardAdminPolicyRequest(
    new Request(path, {
      method: "PUT",
      headers: { "Content-Length": String(64 * 1024 + 1) },
      body: "{}",
    }),
    env,
    { handle: "scott" },
    async () => assert.fail("container must not be called"),
  );
  assert.equal(tooLarge.status, 413);
});

test("internal namespace authenticates before disclosing routes", async () => {
  const env = bridgeEnv();
  for (const authorization of [null, "Bearer wrong"]) {
    const headers = authorization ? { Authorization: authorization } : {};
    const response = await handleInternalBridgeRequest(
      new Request("https://self.witwave.ai/v1/internal/not-a-route", { headers }),
      env,
    );
    assert.equal(response.status, 401);
  }
  const unknown = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/not-a-route"),
    env,
  );
  assert.equal(unknown.status, 404);
});

test("lifecycle activation authenticates before restarting the singleton", async () => {
  const env = bridgeEnv();
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  for (const [authorization, expected] of [
    [null, 401],
    ["Bearer wrong", 401],
    ["Bearer bridge-secret", 405],
  ]) {
    const headers = authorization ? { Authorization: authorization } : {};
    const response = await handleInternalBridgeRequest(
      new Request(`https://self.witwave.ai${PLAN_LIFECYCLE_ACTIVATE_PATH}`, {
        method: authorization === "Bearer bridge-secret" ? "GET" : "POST",
        headers,
      }),
      env,
      undefined,
      () => assert.fail("backend must not be resolved"),
    );
    assert.equal(response.status, expected);
  }

  const disabled = await handleInternalBridgeRequest(
    bridgeRequest(PLAN_LIFECYCLE_ACTIVATE_PATH, { method: "POST" }),
    bridgeEnv(),
    undefined,
    () => assert.fail("backend must not be resolved"),
  );
  assert.equal(disabled.status, 409);
});

test("lifecycle activation restarts with fresh env and proves Go is enabled", async () => {
  const env = bridgeEnv();
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  env.CP_R2_BUCKET = "registry";
  let projected;
  let statusRequest;
  const backend = {
    async restartWithEnvironment(envVars) {
      projected = envVars;
      return { restarted: true };
    },
    async fetch(request) {
      statusRequest = request;
      return new Response(JSON.stringify({
        schema_version: "witself.v0",
        plan_lifecycle: {
          enabled: true,
          billing_available: false,
          running: false,
          runs: 0,
        },
      }), {
        headers: { "Content-Type": "application/json" },
      });
    },
  };

  const response = await handleInternalBridgeRequest(
    bridgeRequest(PLAN_LIFECYCLE_ACTIVATE_PATH, { method: "POST" }),
    env,
    undefined,
    () => backend,
  );

  assert.equal(response.status, 200);
  assert.deepEqual(projected, {
    WITSELF_CP_BRIDGE_TOKEN: "bridge-secret",
    WITSELF_CP_BRIDGE_URL: "https://self.witwave.ai",
    WITSELF_CP_PLAN_LIFECYCLE_ENABLED: "true",
    WITSELF_CP_R2_BUCKET: "registry",
  });
  assert.equal(
    statusRequest.url,
    "http://control-plane.internal/v1/plan-lifecycle/status",
  );
  assert.equal(statusRequest.method, "GET");
  assert.equal(
    statusRequest.headers.get("Authorization"),
    "Bearer bridge-secret",
  );
  assert.deepEqual(await responseJSON(response), {
    schema_version: "witself.v0",
    plan_lifecycle: {
      activated: true,
      enabled: true,
    },
  });
});

test("lifecycle activation fails closed without leaking restart or status details", async () => {
  const env = bridgeEnv();
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  const cases = [
    {
      async restartWithEnvironment() {
        throw new Error("bridge-secret must not leak");
      },
      async fetch() {
        assert.fail("status must not be probed");
      },
    },
    {
      async restartWithEnvironment() {
        return { restarted: false, detail: "bridge-secret" };
      },
      async fetch() {
        assert.fail("status must not be probed");
      },
    },
    {
      async restartWithEnvironment() {
        return { restarted: true };
      },
      async fetch() {
        return new Response(JSON.stringify({
          schema_version: "witself.v0",
          plan_lifecycle: { enabled: false, detail: "bridge-secret" },
        }));
      },
    },
  ];

  for (const backend of cases) {
    const response = await handleInternalBridgeRequest(
      bridgeRequest(PLAN_LIFECYCLE_ACTIVATE_PATH, { method: "POST" }),
      env,
      undefined,
      () => backend,
    );
    assert.equal(response.status, 502);
    const body = await response.text();
    assert.equal(body.includes("bridge-secret"), false);
    assert.deepEqual(JSON.parse(body), {
      schema_version: "witself.v0",
      error: "plan lifecycle activation failed",
    });
  }
});

test("internal resolve is value-minimal and rejects archived or unknown accounts", async () => {
  const active = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:resolve"),
    bridgeEnv({
      "acct:acct_1": {
        cell: "cell-a",
        region: "secret-ish",
      },
      "cell:cell-a": {
        endpoint: "https://cell.example",
        provision_token: "must-not-cross",
      },
    }),
  );
  assert.equal(active.status, 200);
  assert.deepEqual(await responseJSON(active), {
    schema_version: "witself.v0",
    account_id: "acct_1",
    state: "active",
    cell: "cell-a",
    endpoint: "https://cell.example",
  });

  const archived = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:resolve"),
    bridgeEnv({ "archived:acct_1": { cell: "cell-a" } }),
  );
  assert.equal(archived.status, 409);

  const missing = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:resolve"),
    bridgeEnv(),
  );
  assert.equal(missing.status, 404);
});

test("internal resolve requires a configured HTTPS cell endpoint", async () => {
  for (const endpoint of [undefined, "http://cell.example", "not a URL"]) {
    const env = bridgeEnv({
      "acct:acct_1": { cell: "cell-a" },
      "cell:cell-a": endpoint == null ? {} : { endpoint },
    });
    const response = await handleInternalBridgeRequest(
      bridgeRequest("/v1/internal/accounts/acct_1:resolve"),
      env,
    );
    assert.equal(response.status, 502);
    assert.equal(
      (await responseJSON(response)).error,
      "account cell has no valid HTTPS endpoint",
    );
  }
});

test("internal account listing is paginated and excludes pending and archived ids", async () => {
  const env = bridgeEnv(
    {
      "acct:active": { cell: "cell-a" },
      "acct:pending": { cell: "cell-a" },
      "pending:pending": { created_at: "now" },
      "acct:archived": { cell: "cell-a" },
      "archived:archived": { cell: "cell-a" },
      // A stale key returned by list but missing on the read is excluded too.
    },
    {
      keys: [
        { name: "acct:active" },
        { name: "acct:pending" },
        { name: "acct:archived" },
        { name: "acct:stale" },
      ],
      list_complete: false,
      cursor: "opaque-next",
    },
  );
  const response = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts?limit=4&cursor=opaque-current"),
    env,
  );
  assert.equal(response.status, 200);
  assert.deepEqual(await responseJSON(response), {
    schema_version: "witself.v0",
    account_ids: ["active"],
    next_cursor: "opaque-next",
  });
  assert.deepEqual(env.DIRECTORY.listCalls[0], {
    prefix: "acct:",
    limit: 4,
    cursor: "opaque-current",
  });
});

test("internal account listing bounds limit and ends with a null cursor", async () => {
  const env = bridgeEnv({ "acct:a": { cell: "cell-a" } });
  const invalid = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts?limit=501"),
    env,
  );
  assert.equal(invalid.status, 400);

  const response = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts?limit=1"),
    env,
  );
  assert.deepEqual(await responseJSON(response), {
    schema_version: "witself.v0",
    account_ids: ["a"],
    next_cursor: null,
  });
});

test("scheduled lifecycle persists its cursor across container restarts and pages", async () => {
  const pages = {
    "": {
      keys: [{ name: "acct:active-a" }],
      list_complete: false,
      cursor: "opaque-page-two",
    },
    "opaque-page-two": {
      keys: [{ name: "acct:active-b" }],
      list_complete: true,
    },
  };
  const env = bridgeEnv(
    {
      "acct:active-a": { cell: "cell-a" },
      "acct:active-b": { cell: "cell-b" },
    },
    (options) => pages[options.cursor ?? ""],
  );
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  const accountPages = [];
  const containerFetch = async (request) => {
    assert.equal(
      request.url,
      "http://control-plane.internal/v1/plan-lifecycle:tick",
    );
    assert.equal(request.method, "POST");
    assert.equal(request.headers.get("Authorization"), "Bearer bridge-secret");
    const body = await request.json();
    accountPages.push(body.account_ids);
    return new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        scanned: body.account_ids.length,
        seeded: body.account_ids.length,
        apply_pending: 0,
        failed: 0,
        succeeded: true,
      },
    }), {
      headers: { "Content-Type": "application/json" },
    });
  };

  const first = await runScheduledPlanLifecycle(env, containerFetch);
  assert.equal(first.succeeded, true);
  const firstCursor = JSON.parse(
    env.DIRECTORY.entries.get(PLAN_LIFECYCLE_CURSOR_KEY),
  );
  assert.equal(firstCursor.cursor, "opaque-page-two");
  assert.equal(Number.isFinite(Date.parse(firstCursor.updated_at)), true);

  // The helper is intentionally stateless: this second invocation models a
  // freshly started container/Worker isolate and resumes solely from KV.
  const second = await runScheduledPlanLifecycle(env, containerFetch);
  assert.equal(second.succeeded, true);
  assert.deepEqual(accountPages, [["active-a"], ["active-b"]]);
  assert.deepEqual(env.DIRECTORY.listCalls, [
    { prefix: "acct:", limit: PLAN_LIFECYCLE_PAGE_SIZE },
    {
      prefix: "acct:",
      limit: PLAN_LIFECYCLE_PAGE_SIZE,
      cursor: "opaque-page-two",
    },
  ]);
  const completed = JSON.parse(
    env.DIRECTORY.entries.get(PLAN_LIFECYCLE_CURSOR_KEY),
  );
  assert.equal(completed.cursor, null);
});

test("scheduled lifecycle discovers activation without prior cold-path traffic", async () => {
  const env = bridgeEnv({
    "acct:newly-active": { cell: "cell-a" },
  });
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  let ticked = false;
  const result = await runScheduledPlanLifecycle(env, async (request) => {
    ticked = true;
    const body = await request.json();
    assert.deepEqual(body, { account_ids: ["newly-active"] });
    return new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        scanned: 1,
        seeded: 1,
        apply_pending: 0,
        failed: 0,
        succeeded: true,
      },
    }));
  });
  assert.equal(ticked, true);
  assert.equal(result.seeded, 1);
});

test("scheduled lifecycle is gated and advances only after a valid acknowledgement", async () => {
  const env = bridgeEnv({ "acct:a": { cell: "cell-a" } });
  let called = false;
  const fetcher = async () => {
    called = true;
    return new Response("unavailable", { status: 503 });
  };
  const disabled = await runScheduledPlanLifecycle(env, fetcher);
  assert.equal(disabled.ran, false);
  assert.equal(called, false);

  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  const failed = await runScheduledPlanLifecycle(env, fetcher);
  assert.equal(failed.succeeded, false);
  assert.equal(called, true);
  assert.equal(env.DIRECTORY.entries.has(PLAN_LIFECYCLE_CURSOR_KEY), false);

  const malformed = await runScheduledPlanLifecycle(env, async () =>
    new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        scanned: 0,
        seeded: 0,
        apply_pending: 0,
        failed: 0,
        succeeded: true,
      },
    })));
  assert.equal(malformed.succeeded, false);
  assert.equal(env.DIRECTORY.entries.has(PLAN_LIFECYCLE_CURSOR_KEY), false);
});

test("scheduled lifecycle advances past an acknowledged page with account failures", async () => {
  const env = bridgeEnv(
    { "acct:a": { cell: "cell-a" } },
    {
      keys: [{ name: "acct:a" }],
      list_complete: false,
      cursor: "retry-on-next-cycle",
    },
  );
  env.CP_PLAN_LIFECYCLE_ENABLED = "true";
  const result = await runScheduledPlanLifecycle(env, async () =>
    new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        scanned: 1,
        seeded: 1,
        apply_pending: 1,
        failed: 1,
        succeeded: false,
      },
    })));
  assert.equal(result.succeeded, false);
  const cursor = JSON.parse(
    env.DIRECTORY.entries.get(PLAN_LIFECYCLE_CURSOR_KEY),
  );
  assert.equal(cursor.cursor, "retry-on-next-cycle");
});

test("internal apply forwards exact snapshot with only the cell provision token", async () => {
  const env = bridgeEnv({
    "acct:acct_1": { cell: "cell-a" },
    "cell:cell-a": {
      endpoint: "https://cell.example/",
      provision_token: "witself_prv_cell-secret",
    },
  });
  const snapshot = {
    plan: "free",
    limits: { agents: 25 },
    policies: { transcript_retention_days: 60 },
    features: ["memory"],
  };
  let call;
  const response = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:apply-plan", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(snapshot),
    }),
    env,
    async (url, init) => {
      call = { url, init };
      return new Response('{"applied":true}', {
        status: 207,
        headers: { "Content-Type": "application/json" },
      });
    },
  );
  assert.equal(call.url, "https://cell.example/v1/accounts/acct_1:plan");
  assert.equal(
    call.init.headers.Authorization,
    "Bearer witself_prv_cell-secret",
  );
  assert.equal(new TextDecoder().decode(call.init.body), JSON.stringify(snapshot));
  assert.equal(response.status, 207);
  assert.deepEqual(await responseJSON(response), { applied: true });
});

test("internal plan fence read uses only the cell provision token", async () => {
  const env = bridgeEnv({
    "acct:acct_1": { cell: "cell-a" },
    "cell:cell-a": {
      endpoint: "https://cell.example/",
      provision_token: "witself_prv_cell-secret",
    },
  });
  let call;
  const response = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:apply-plan"),
    env,
    async (url, init) => {
      call = { url, init };
      return new Response(JSON.stringify({
        account_id: "acct_1",
        revision: 41,
        snapshot_hash: "b".repeat(64),
        plan: "standard",
        limits: {},
        policies: { transcript_retention_days: 90 },
        features: [],
      }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    },
  );
  assert.equal(call.url, "https://cell.example/v1/accounts/acct_1:plan");
  assert.equal(call.init.method, "GET");
  assert.equal(call.init.headers.Authorization, "Bearer witself_prv_cell-secret");
  assert.equal(call.init.body, undefined);
  assert.equal(response.status, 200);
  assert.deepEqual(await responseJSON(response), {
    schema_version: "witself.v0",
    account_id: "acct_1",
    revision: 41,
    snapshot_hash: "b".repeat(64),
  });
});

test("internal plan fence read rejects malformed or cross-account acknowledgements", async () => {
  const env = bridgeEnv({
    "acct:acct_1": { cell: "cell-a" },
    "cell:cell-a": {
      endpoint: "https://cell.example/",
      provision_token: "prv",
    },
  });
  for (const snapshot of [
    { account_id: "acct_other", revision: 1, snapshot_hash: "a".repeat(64) },
    { account_id: "acct_1", revision: -1, snapshot_hash: "a".repeat(64) },
    { account_id: "acct_1", revision: 1, snapshot_hash: "not-a-hash" },
  ]) {
    const response = await handleInternalBridgeRequest(
      bridgeRequest("/v1/internal/accounts/acct_1:apply-plan"),
      env,
      async () => new Response(JSON.stringify(snapshot), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    assert.equal(response.status, 502);
    assert.equal(
      (await responseJSON(response)).error,
      "cell returned an invalid plan fence",
    );
  }
});

test("internal apply reports missing cell configuration and cell failures", async () => {
  const body = JSON.stringify({ plan: "free" });
  const missingCell = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:apply-plan", {
      method: "POST",
      body,
    }),
    bridgeEnv({ "acct:acct_1": { cell: "cell-a" } }),
    async () => assert.fail("cell must not be called"),
  );
  assert.equal(missingCell.status, 502);

  const failed = await handleInternalBridgeRequest(
    bridgeRequest("/v1/internal/accounts/acct_1:apply-plan", {
      method: "POST",
      body,
    }),
    bridgeEnv({
      "acct:acct_1": { cell: "cell-a" },
      "cell:cell-a": {
        endpoint: "https://cell.example",
        provision_token: "prv",
      },
    }),
    async () => {
      throw new TypeError("network details should not leak");
    },
  );
  assert.equal(failed.status, 502);
  assert.equal((await responseJSON(failed)).error, "cell unreachable");
});
