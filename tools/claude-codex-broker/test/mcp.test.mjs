import assert from "node:assert/strict";
import crypto from "node:crypto";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import test from "node:test";

import { createGrant } from "../../../.claude/codex-profiles.mjs";
import { Broker, processRpcLine, startServer, TOOLS, toolsForCeiling } from "../lib/broker.mjs";
import { GrantVerifier } from "../lib/grants.mjs";

function request(id, method, params) {
  return JSON.stringify({ jsonrpc: "2.0", id, method, ...(params === undefined ? {} : { params }) });
}

function fakeBroker() {
  return {
    calls: [],
    async probe() { this.calls.push(["probe"]); return { modelCalls: 0 }; },
    start(task) { this.calls.push(["start", task]); return { job_id: "00000000-0000-4000-8000-000000000000", state: "preparing" }; },
    status(id) { this.calls.push(["status", id]); return { job_id: id, state: "running" }; },
    async cancel(id) { this.calls.push(["cancel", id]); return { job_id: id, state: "cancelling" }; },
  };
}

function startTransportHarness(broker, options = {}) {
  const input = new PassThrough();
  const output = new PassThrough();
  const signalEmitter = new EventEmitter();
  const responses = [];
  const waiters = new Set();
  let buffer = "";
  output.setEncoding("utf8");
  output.on("data", (chunk) => {
    buffer += chunk;
    let newline;
    while ((newline = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      if (!line) continue;
      const response = JSON.parse(line);
      responses.push(response);
      for (const waiter of [...waiters]) {
        if (waiter.id !== response.id) continue;
        waiters.delete(waiter);
        clearTimeout(waiter.timer);
        waiter.resolve(response);
      }
    }
  });
  const runtime = options.runtime ?? {
    async prepareForNewWork() { return { version: "test" }; },
    async cleanup() {},
  };
  const running = startServer(["--repository", "/canonical/repo"], {
    authority: { verifier: null, launcherEnvironment: Object.freeze({}) },
    runtime,
    broker,
    input,
    output,
    signalEmitter,
    ...(options.maxInFlightRequests === undefined ? {} : { maxInFlightRequests: options.maxInFlightRequests }),
  });
  return {
    input,
    output,
    signalEmitter,
    responses,
    running,
    send(...messages) { input.write(`${messages.map((message) => JSON.stringify(message)).join("\n")}\n`); },
    waitFor(id, timeoutMs = 1_000) {
      const existing = responses.find((response) => response.id === id);
      if (existing) return Promise.resolve(existing);
      return new Promise((resolve, reject) => {
        const waiter = { id, resolve, reject, timer: null };
        waiter.timer = setTimeout(() => {
          waiters.delete(waiter);
          reject(new Error(`timed out waiting for response ${String(id)}`));
        }, timeoutMs);
        waiters.add(waiter);
      });
    },
    closeWaiters() {
      for (const waiter of waiters) {
        clearTimeout(waiter.timer);
        waiter.reject(new Error("transport harness closed"));
      }
      waiters.clear();
    },
  };
}

async function initializeTransport(harness, extraMessages = []) {
  harness.send(
    { jsonrpc: "2.0", id: 1, method: "initialize", params: {
      protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "transport-test", version: "1" },
    } },
    { jsonrpc: "2.0", method: "notifications/initialized" },
    ...extraMessages,
  );
  return harness.waitFor(1);
}

async function initialize(context, broker) {
  const response = await processRpcLine(request(1, "initialize", {
    protocolVersion: "2025-06-18",
    capabilities: {},
    clientInfo: { name: "test", version: "1" },
  }), context, broker);
  assert.equal(response.result.protocolVersion, "2025-06-18");
  assert.equal(await processRpcLine(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }), context, broker), null);
}

test("MCP initialization and tool catalog expose only constrained review tools", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = fakeBroker();
  await initialize(context, broker);
  const response = await processRpcLine(request(2, "tools/list", {}), context, broker);
  assert.deepEqual(response.result.tools.map(({ name }) => name), [
    "codex_runtime_probe", "codex_review_start", "codex_review_status", "codex_review_cancel",
  ]);
  assert.deepEqual(response.result.tools, TOOLS);
  assert.equal(response.result.tools.every(({ inputSchema }) => inputSchema.additionalProperties === false), true);
});

test("tool catalogs are frozen by startup ceiling and elevated schemas require the reserved grant", () => {
  assert.deepEqual(toolsForCeiling("repository").map(({ name }) => name), [
    "codex_runtime_probe", "codex_review_start", "codex_review_status", "codex_review_cancel",
  ]);
  assert.deepEqual(toolsForCeiling("isolated-write").map(({ name }) => name), [
    "codex_runtime_probe", "codex_review_start", "codex_review_status", "codex_review_cancel",
    "codex_implementation_start", "codex_implementation_status", "codex_implementation_artifact_read", "codex_implementation_cancel",
  ]);
  assert.deepEqual(toolsForCeiling("system").map(({ name }) => name), [
    "codex_runtime_probe", "codex_review_start", "codex_review_status", "codex_review_cancel",
    "codex_implementation_start", "codex_implementation_status", "codex_implementation_artifact_read", "codex_implementation_cancel",
    "codex_system_start", "codex_system_status", "codex_system_cancel",
  ]);
  const elevated = toolsForCeiling("system").filter(({ name }) => name.startsWith("codex_system_") || name.startsWith("codex_implementation_"));
  assert.equal(elevated.every(({ inputSchema }) => inputSchema.required.includes("_codex_grant") &&
    inputSchema.properties._codex_grant.additionalProperties === false), true);
  assert.equal(elevated.find(({ name }) => name === "codex_system_start").annotations.destructiveHint, true);
  for (const name of ["codex_review_status", "codex_implementation_status", "codex_system_status"]) {
    const schema = toolsForCeiling("system").find((tool) => tool.name === name).inputSchema;
    assert.deepEqual(schema.properties.wait_seconds, { type: "integer", minimum: 0, maximum: 30 });
    assert.equal(schema.required.includes("wait_seconds"), false);
  }
  assert.throws(() => toolsForCeiling("unknown"), (error) => error?.code === "invalid_ceiling");
});

test("MCP status accepts a bounded wait and rejects malformed or extra status inputs", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = fakeBroker();
  const statusCalls = [];
  broker.status = (jobId, waitSeconds) => {
    statusCalls.push([jobId, waitSeconds]);
    return { job_id: jobId, state: "running" };
  };
  await initialize(context, broker);
  const jobId = "00000000-0000-4000-8000-000000000000";
  const valid = await processRpcLine(request(2, "tools/call", {
    name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30 },
  }), context, broker);
  assert.equal(valid.result.isError, false);
  for (const [index, waitSeconds] of [-1, 31, 0.5, "30", null].entries()) {
    const malformed = await processRpcLine(request(index + 3, "tools/call", {
      name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: waitSeconds },
    }), context, broker);
    assert.equal(malformed.result.isError, true, String(waitSeconds));
    assert.equal(JSON.parse(malformed.result.content[0].text).error.code, "invalid_wait_seconds", String(waitSeconds));
  }
  const extra = await processRpcLine(request(8, "tools/call", {
    name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30, timeout: 31 },
  }), context, broker);
  assert.equal(extra.result.isError, true);
  assert.deepEqual(statusCalls, [[jobId, 30]]);
});

test("stdio transport dispatches initialized requests concurrently so long-poll never blocks ping or cancel", async (t) => {
  const jobId = "00000000-0000-4000-8000-000000000000";
  let settleStatus;
  let markStatusStarted;
  const statusStarted = new Promise((resolve) => { markStatusStarted = resolve; });
  let closeCalls = 0;
  const broker = {
    ceiling: "repository",
    toolCatalog: () => toolsForCeiling("repository"),
    status(id, waitSeconds) {
      assert.equal(id, jobId);
      assert.equal(waitSeconds, 30);
      markStatusStarted();
      return new Promise((resolve) => { settleStatus = resolve; });
    },
    async cancel(id) {
      assert.equal(id, jobId);
      settleStatus?.({ job_id: id, state: "cancelled" });
      return { job_id: id, state: "cancelling" };
    },
    requestShutdown() {
      settleStatus?.({ job_id: jobId, state: "cancelled" });
    },
    async close() {
      closeCalls += 1;
    },
  };
  const harness = startTransportHarness(broker);
  t.after(async () => {
    harness.closeWaiters();
    harness.input.destroy();
    await harness.running;
  });
  const listed = harness.waitFor(2);
  await initializeTransport(harness, [{ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} }]);
  assert.equal((await listed).result.tools.length, 4, "initialize and initialized notification preserve same-batch ordering");

  const began = Date.now();
  harness.send(
    { jsonrpc: "2.0", id: 3, method: "tools/call", params: {
      name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30 },
    } },
    { jsonrpc: "2.0", id: 4, method: "ping" },
    { jsonrpc: "2.0", id: 5, method: "tools/call", params: {
      name: "codex_review_cancel", arguments: { job_id: jobId },
    } },
  );
  await statusStarted;
  const [ping, cancelled, status] = await Promise.all([
    harness.waitFor(4), harness.waitFor(5), harness.waitFor(3),
  ]);
  assert.deepEqual(ping.result, {});
  assert.equal(JSON.parse(cancelled.result.content[0].text).state, "cancelling");
  assert.equal(JSON.parse(status.result.content[0].text).state, "cancelled");
  assert.ok(Date.now() - began < 1_000, "cancel and ping must not queue behind a 30-second status wait");
  harness.input.end();
  await harness.running;
  assert.equal(closeCalls, 1);
});

test("stdio EOF defers destructive cleanup until accepted probe and artifact reads finish", async (t) => {
  for (const operation of ["probe", "artifact"]) {
    await t.test(operation, async (subtest) => {
      const events = [];
      let releaseOperation;
      let markStarted;
      let completed = false;
      const started = new Promise((resolve) => { markStarted = resolve; });
      const pending = new Promise((resolve) => { releaseOperation = resolve; });
      const broker = {
        ceiling: "isolated-write",
        toolCatalog: () => toolsForCeiling("isolated-write"),
        authorizeElevated(_tool, args) {
          const { _codex_grant: _ignored, ...authorized } = args;
          return authorized;
        },
        async probe() {
          events.push("probe-start");
          markStarted();
          const value = await pending;
          completed = true;
          events.push("probe-finish");
          return value;
        },
        async implementationArtifactRead() {
          events.push("artifact-start");
          markStarted();
          const value = await pending;
          completed = true;
          events.push("artifact-finish");
          return value;
        },
        requestShutdown() { events.push("shutdown-requested"); },
        async close() {
          assert.equal(completed, true, "destructive broker cleanup raced an accepted request");
          events.push("cleanup");
        },
      };
      const harness = startTransportHarness(broker);
      subtest.after(async () => {
        harness.closeWaiters();
        harness.input.destroy();
        releaseOperation({});
        await harness.running;
      });
      await initializeTransport(harness);
      if (operation === "probe") {
        harness.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "codex_runtime_probe" } });
      } else {
        harness.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: {
          name: "codex_implementation_artifact_read",
          arguments: {
            job_id: "00000000-0000-4000-8000-000000000000",
            artifact_id: "changes.patch",
            _codex_grant: {},
          },
        } });
      }
      await started;
      harness.input.end();
      await new Promise((resolve) => setImmediate(resolve));
      assert.equal(events.includes("shutdown-requested"), true);
      assert.equal(events.includes("cleanup"), false);
      releaseOperation(operation === "probe" ? { modelCalls: 0 } : {
        artifactId: "changes.patch", encoding: "base64", data: "", eof: true,
      });
      const response = await harness.waitFor(2);
      await harness.running;
      assert.equal(response.result.isError, false);
      assert.deepEqual(events, [
        `${operation}-start`, "shutdown-requested", `${operation}-finish`, "cleanup",
      ]);
    });
  }
});

test("stdio shutdown drains startup runtime preparation before destructive cleanup", async (t) => {
  let releasePreparation;
  let markPreparationStarted;
  const preparationStarted = new Promise((resolve) => { markPreparationStarted = resolve; });
  const runtime = {
    async prepareForNewWork() {
      markPreparationStarted();
      return new Promise((resolve) => { releasePreparation = resolve; });
    },
    async cleanup() {},
  };
  const events = [];
  const broker = {
    ceiling: "repository",
    toolCatalog: () => toolsForCeiling("repository"),
    requestShutdown() { events.push("shutdown-requested"); },
    async close() { events.push("cleanup"); },
  };
  const harness = startTransportHarness(broker, { runtime });
  t.after(async () => {
    harness.closeWaiters();
    harness.input.destroy();
    releasePreparation?.({ version: "test" });
    await harness.running;
  });
  await preparationStarted;
  harness.input.end();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(events, ["shutdown-requested"]);
  releasePreparation({ version: "test" });
  await harness.running;
  assert.deepEqual(events, ["shutdown-requested", "cleanup"]);
});

test("stdio signal and EOF close the broker immediately while a long-poll is active", async (t) => {
  for (const reason of ["signal", "eof"]) {
    await t.test(reason, async (subtest) => {
      const jobId = "00000000-0000-4000-8000-000000000000";
      let settleStatus;
      let markStatusStarted;
      const statusStarted = new Promise((resolve) => { markStatusStarted = resolve; });
      let closeCalls = 0;
      let shutdownRequestedAt = null;
      const broker = {
        ceiling: "repository",
        toolCatalog: () => toolsForCeiling("repository"),
        status() {
          markStatusStarted();
          return new Promise((resolve) => { settleStatus = resolve; });
        },
        requestShutdown() {
          shutdownRequestedAt = Date.now();
          settleStatus?.({ job_id: jobId, state: "cancelled" });
        },
        async close() {
          closeCalls += 1;
        },
      };
      const harness = startTransportHarness(broker);
      subtest.after(() => {
        harness.closeWaiters();
        harness.input.destroy();
      });
      await initializeTransport(harness);
      harness.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: {
        name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30 },
      } });
      await statusStarted;
      const began = Date.now();
      if (reason === "signal") harness.signalEmitter.emit("SIGTERM");
      else harness.input.end();
      const response = await harness.waitFor(2);
      await harness.running;
      assert.equal(JSON.parse(response.result.content[0].text).state, "cancelled");
      assert.equal(closeCalls, 1);
      assert.ok(shutdownRequestedAt >= began && shutdownRequestedAt - began < 500,
        `${reason} must request broker shutdown immediately`);
      assert.equal(harness.signalEmitter.listenerCount("SIGTERM"), 0);
    });
  }
});

test("stdio transport enforces its hard in-flight request cap without an unbounded queue", async (t) => {
  const jobId = "00000000-0000-4000-8000-000000000000";
  const pending = [];
  let closeCalls = 0;
  const broker = {
    ceiling: "repository",
    toolCatalog: () => toolsForCeiling("repository"),
    status() { return new Promise((resolve) => pending.push(resolve)); },
    requestShutdown() {
      for (const resolve of pending.splice(0)) resolve({ job_id: jobId, state: "cancelled" });
    },
    async close() {
      closeCalls += 1;
    },
  };
  const harness = startTransportHarness(broker, { maxInFlightRequests: 2 });
  t.after(() => {
    harness.closeWaiters();
    harness.input.destroy();
  });
  await initializeTransport(harness);
  await new Promise((resolve) => setImmediate(resolve));
  harness.send(
    { jsonrpc: "2.0", id: 2, method: "tools/call", params: {
      name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30 },
    } },
    { jsonrpc: "2.0", id: 3, method: "tools/call", params: {
      name: "codex_review_status", arguments: { job_id: jobId, wait_seconds: 30 },
    } },
    { jsonrpc: "2.0", id: 4, method: "ping" },
  );
  const overloaded = await harness.waitFor(4);
  await harness.running;
  assert.equal(overloaded.error.code, -32000);
  assert.equal(overloaded.error.message, "Too many in-flight MCP requests");
  assert.equal(closeCalls, 1);
});

test("MCP implementation start, status, artifact read, and cancel require exact fresh grants", async () => {
  const now = 2_000_000_000_000;
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "isolated-write", key, clock: () => now });
  const jobId = "00000000-0000-4000-8000-000000000000";
  const calls = [];
  const broker = {
    ceiling: "isolated-write",
    toolCatalog: () => toolsForCeiling("isolated-write"),
    authorizeElevated: (tool, args) => verifier.verifyAndConsume(tool, args),
    implementationStart(task) { calls.push(["start", task]); return { job_id: jobId, state: "preparing" }; },
    implementationStatus(id, waitSeconds) { calls.push(["status", id, waitSeconds]); return { job_id: id, state: "running" }; },
    async implementationArtifactRead(id, artifactId, offset, maxBytes) {
      calls.push(["artifact", id, artifactId, offset, maxBytes]);
      return { artifactId, encoding: "base64", data: "", eof: true };
    },
    async implementationCancel(id) { calls.push(["cancel", id]); return { job_id: id, state: "cancelling" }; },
  };
  const context = { initializeSeen: false, initialized: false };
  await initialize(context, broker);
  const call = async (id, toolName, originalInput, mode, nonce) => processRpcLine(request(id, "tools/call", {
    name: toolName,
    arguments: {
      ...originalInput,
      _codex_grant: createGrant({ key, ceiling: "isolated-write", toolName, mode, toolUseId: `toolu_impl_${id}`, originalInput, now, nonce }),
    },
  }), context, broker);
  assert.equal((await processRpcLine(request(2, "tools/call", {
    name: "codex_implementation_start", arguments: { task: "change" },
  }), context, broker)).result.isError, true);
  assert.equal((await call(3, "codex_implementation_start", { task: "change" }, "acceptEdits", "implstartnonceABCDEFGH12")).result.isError, false);
  assert.equal((await call(4, "codex_implementation_status", {
    job_id: jobId, wait_seconds: 30,
  }, "plan", "implstatusnonceABCDEFGH1")).result.isError, false);
  assert.equal((await call(5, "codex_implementation_artifact_read", {
    job_id: jobId, artifact_id: "changes.patch", offset: 0, max_bytes: 1024,
  }, "default", "implartifactnonceABCDEFG")).result.isError, false);
  assert.equal((await call(6, "codex_implementation_cancel", { job_id: jobId }, "dontAsk", "implcancelnonceABCDEFGH1")).result.isError, false);
  const hostile = { task: "change", cwd: "/source", model: "other", env: {}, timeout: 1 };
  assert.equal((await call(7, "codex_implementation_start", hostile, "acceptEdits", "implhostilenonceABCDEFGH")).result.isError, true);
  assert.deepEqual(calls, [
    ["start", "change"],
    ["status", jobId, 30],
    ["artifact", jobId, "changes.patch", 0, 1024],
    ["cancel", jobId],
  ]);
  verifier.destroy();
});

test("MCP tools/list uses the broker's immutable system catalog", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = { ...fakeBroker(), toolCatalog: () => toolsForCeiling("system") };
  await initialize(context, broker);
  const response = await processRpcLine(request(2, "tools/list", {}), context, broker);
  assert.deepEqual(response.result.tools.map(({ name }) => name).slice(-3), [
    "codex_system_start", "codex_system_status", "codex_system_cancel",
  ]);
});

test("system broker accepts a spec-conformant omitted probe argument and reports its live catalog", async () => {
  const runtime = {
    async prepareForNewWork() { return { version: "test" }; },
    async cleanup() {},
  };
  const broker = new Broker(runtime, {
    ceiling: "system",
    probeFn: async () => ({ runtime: { version: "test" }, modelCalls: 0 }),
  });
  const context = { initializeSeen: false, initialized: false };
  try {
    await initialize(context, broker);
    const listed = await processRpcLine(request(2, "tools/list", {}), context, broker);
    const toolNames = listed.result.tools.map(({ name }) => name);
    const response = await processRpcLine(request(3, "tools/call", { name: "codex_runtime_probe" }), context, broker);
    assert.equal(response.result.isError, false);
    const result = JSON.parse(response.result.content[0].text);
    assert.equal(result.broker_ceiling, "system");
    assert.deepEqual(result.available_tools, toolNames);
    assert.equal(result.available_tools.includes("codex_system_start"), true);
    assert.equal(result.modelCalls, 0);
  } finally {
    await broker.close();
  }
});

test("MCP system start, status, and cancel each require and consume a fresh exact HMAC grant", async () => {
  const now = 2_000_000_000_000;
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "system", key, clock: () => now });
  const calls = [];
  const broker = {
    toolCatalog: () => toolsForCeiling("system"),
    authorizeElevated: (tool, args) => verifier.verifyAndConsume(tool, args),
    systemStart(task) { calls.push(["start", task]); return { job_id: "00000000-0000-4000-8000-000000000000", state: "preparing" }; },
    systemStatus(id, waitSeconds) { calls.push(["status", id, waitSeconds]); return { job_id: id, state: "running" }; },
    async systemCancel(id) { calls.push(["cancel", id]); return { job_id: id, state: "cancelling" }; },
  };
  const context = { initializeSeen: false, initialized: false };
  await initialize(context, broker);
  const missing = await processRpcLine(request(2, "tools/call", { name: "codex_system_start", arguments: { task: "authorized" } }), context, broker);
  assert.equal(missing.result.isError, true);
  const jobId = "00000000-0000-4000-8000-000000000000";
  const call = async (id, toolName, originalInput, mode, nonce) => processRpcLine(request(id, "tools/call", {
    name: toolName,
    arguments: {
      ...originalInput,
      _codex_grant: createGrant({ key, ceiling: "system", toolName, mode, toolUseId: `toolu_${id}`, originalInput, now, nonce }),
    },
  }), context, broker);
  assert.equal((await call(3, "codex_system_start", { task: "authorized" }, "bypassPermissions", "startnonceABCDEFGH123456")).result.isError, false);
  assert.equal((await call(4, "codex_system_status", {
    job_id: jobId, wait_seconds: 30,
  }, "plan", "statusnonceABCDEFGH12345")).result.isError, false);
  assert.equal((await call(5, "codex_system_cancel", { job_id: jobId }, "default", "cancelnonceABCDEFGH12345")).result.isError, false);
  assert.deepEqual(calls, [["start", "authorized"], ["status", jobId, 30], ["cancel", jobId]]);
  verifier.destroy();
});

test("MCP calls cannot override cwd, model, effort, permissions, config, environment, or timeout", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = fakeBroker();
  await initialize(context, broker);
  const bad = await processRpcLine(request(2, "tools/call", {
    name: "codex_review_start",
    arguments: { task: "review", cwd: "/tmp", model: "cheap", effort: "low", env: {}, timeout: 1 },
  }), context, broker);
  assert.equal(bad.result.isError, true);
  assert.equal(broker.calls.length, 0);
  const good = await processRpcLine(request(3, "tools/call", {
    name: "codex_review_start", arguments: { task: "review" },
  }), context, broker);
  assert.equal(good.result.isError, false);
  assert.deepEqual(broker.calls, [["start", "review"]]);
});

test("MCP parser fails closed for malformed, pre-init, repeated init, unknown methods, and unknown tools", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = fakeBroker();
  assert.equal((await processRpcLine("{", context, broker)).error.code, -32700);
  assert.equal((await processRpcLine(request(1, "tools/list", {}), context, broker)).error.code, -32602);
  await initialize(context, broker);
  assert.equal((await processRpcLine(request(4, "initialize", { protocolVersion: "x", capabilities: {}, clientInfo: {} }), context, broker)).error.code, -32602);
  assert.equal((await processRpcLine(request(5, "not/allowed", {}), context, broker)).error.code, -32601);
  const unknown = await processRpcLine(request(6, "tools/call", { name: "raw_codex", arguments: {} }), context, broker);
  assert.equal(unknown.result.isError, true);
});

test("probe, status, and cancel require exact argument objects", async () => {
  const context = { initializeSeen: false, initialized: false };
  const broker = fakeBroker();
  await initialize(context, broker);
  const omittedProbe = await processRpcLine(request(2, "tools/call", { name: "codex_runtime_probe" }), context, broker);
  assert.equal(omittedProbe.result.isError, false);
  const explicitProbe = await processRpcLine(request(3, "tools/call", {
    name: "codex_runtime_probe",
    arguments: {},
    _meta: { progressToken: "toolu_probe" },
  }), context, broker);
  assert.equal(explicitProbe.result.isError, false);
  const extra = await processRpcLine(request(4, "tools/call", { name: "codex_runtime_probe", arguments: { model: "gpt" } }), context, broker);
  assert.equal(extra.result.isError, true);
  const id = "00000000-0000-4000-8000-000000000000";
  for (const [requestId, name] of [
    [5, "codex_review_start"],
    [6, "codex_review_status"],
    [7, "codex_review_cancel"],
  ]) {
    const omitted = await processRpcLine(request(requestId, "tools/call", { name }), context, broker);
    assert.equal(omitted.result.isError, true, name);
    assert.equal(JSON.parse(omitted.result.content[0].text).error.code, "invalid_arguments", name);
  }
  const nullArguments = await processRpcLine(request(8, "tools/call", {
    name: "codex_runtime_probe", arguments: null,
  }), context, broker);
  assert.equal(nullArguments.result.isError, true);
  assert.equal(JSON.parse(nullArguments.result.content[0].text).error.code, "invalid_tool_call");
  const extraEnvelope = await processRpcLine(request(9, "tools/call", {
    name: "codex_runtime_probe", extra: true,
  }), context, broker);
  assert.equal(extraEnvelope.result.isError, true);
  assert.equal(JSON.parse(extraEnvelope.result.content[0].text).error.code, "invalid_tool_call");
  for (const [requestId, meta] of [
    [10, null],
    [11, []],
    [12, { oversized: "x".repeat(4 * 1024 + 1) }],
  ]) {
    const invalidMeta = await processRpcLine(request(requestId, "tools/call", {
      name: "codex_runtime_probe", _meta: meta,
    }), context, broker);
    assert.equal(invalidMeta.result.isError, true);
    assert.equal(JSON.parse(invalidMeta.result.content[0].text).error.code, "invalid_tool_call");
  }
  await processRpcLine(request(13, "tools/call", {
    name: "codex_review_status", arguments: { job_id: id }, _meta: { progressToken: 13 },
  }), context, broker);
  await processRpcLine(request(14, "tools/call", { name: "codex_review_cancel", arguments: { job_id: id } }), context, broker);
  assert.deepEqual(broker.calls, [["probe"], ["probe"], ["status", id], ["cancel", id]]);
});
