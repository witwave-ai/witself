import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import test from "node:test";

import { AppServerSession, probeRuntime } from "../lib/app-server.mjs";
import { CONSTRAINED_FEATURES } from "../lib/execution-tooling.mjs";
import { fakeRuntime, makeTemp, readRecord } from "./helpers.mjs";

test("probe attests runtime, external auth, Sol Ultra v2, and restricted profile without a model turn", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl, record, configSnapshots } = await fakeRuntime(temp);
  const result = await probeRuntime(runtime, { spawnImpl });
  assert.equal(result.modelCalls, 0);
  assert.equal(result.runtime.latestVerificationPolicy, "before-every-new-work");
  assert.equal(result.runtime.latestVerifiedAt, 2_000_000_000_000);
  assert.deepEqual(result.account, { type: "chatgpt", planType: "pro", externalInMemory: true });
  assert.equal(result.attestation.model, "gpt-5.6-sol");
  assert.equal(result.attestation.effort, "ultra");
  assert.equal(result.attestation.multiAgentVersion, "v2");
  assert.equal(result.attestation.networkAccess, false);
  assert.equal(result.attestation.permissionProfile, "claude-review");
  assert.equal(result.attestation.writableRoots, 1);
  assert.deepEqual(result.attestation.executionTooling, {
    environmentId: "local",
    environmentStatus: "ready",
    cwd: runtime.projectRoot,
    features: CONSTRAINED_FEATURES,
  });
  assert.deepEqual(result.attestation.confinement, {
    repositoryRead: true, gitHistoryRead: true, privateScratchWrite: true,
    brokerDeniedSentinelReadDenied: true,
    systemTemporaryDirectoryIsolation: "not-guaranteed-on-macos",
    hostSecretConfinement: "requires-outer-os-isolation",
    loopbackNetworkDenied: true,
    networkAccess: false,
  });
  assert.deepEqual(result.attestation.inventory.skills.acceptedSystem, [
    "imagegen", "openai-docs", "plugin-creator", "review-agent", "skill-creator", "skill-installer",
  ]);
  assert.equal(result.attestation.inventory.skills.scope, "system");
  assert.equal(result.attestation.inventory.skills.isolatedPaths, true);
  assert.match(result.attestation.inventory.skills.policyDigest, /^[0-9a-f]{64}$/);
  assert.equal(result.attestation.inventory.hooks.count, 0);
  assert.equal(result.attestation.inventory.hooks.acceptedMode, "none");
  assert.equal(result.attestation.inventory.hooks.sterile, true);
  assert.deepEqual(result.attestation.inventory.hooks.acceptedEvents, []);
  assert.match(result.attestation.inventory.hooks.policyDigest, /^[0-9a-f]{64}$/);

  const requests = await readRecord(record);
  assert.equal(requests.some(({ method }) => method === "turn/start"), false);
  const login = requests.find(({ method }) => method === "account/login/start");
  assert.deepEqual(Object.keys(login.params).sort(), ["accessToken", "chatgptAccountId", "chatgptPlanType", "type"]);
  const thread = requests.find(({ method }) => method === "thread/start");
  assert.equal(thread.params.allowProviderModelFallback, false);
  assert.equal(thread.params.sandbox, undefined);
  assert.equal(thread.params.permissions, "claude-review");
  assert.deepEqual(thread.params.dynamicTools, []);
  assert.deepEqual(thread.params.environments, [{
    environmentId: "local", cwd: runtime.projectRoot, runtimeWorkspaceRoots: [runtime.projectRoot],
  }]);
  assert.deepEqual(thread.params.selectedCapabilityRoots, []);
  assert.deepEqual(thread.params.runtimeWorkspaceRoots, [runtime.projectRoot]);
  assert.deepEqual(thread.params.config.features, CONSTRAINED_FEATURES);
  const profile = thread.params.config.permissions["claude-review"];
  assert.equal(profile.filesystem[":root"], "deny");
  assert.equal(profile.filesystem[":minimal"], "read");
  assert.equal(profile.filesystem[runtime.projectRoot], "read");
  assert.equal(profile.filesystem[runtime.gitReadRoots[0]], "read");
  assert.equal(Object.values(profile.filesystem).filter((value) => value === "write").length, 1);
  assert.deepEqual(profile.network, { enabled: false });
  assert.equal(thread.params.config.cli_auth_credentials_store, "file");
  const commandRequests = requests.filter(({ method }) => method === "command/exec");
  assert.equal(commandRequests.length, 6);
  assert.equal(commandRequests.every(({ params }) => path.isAbsolute(params.command[0]) &&
    params.cwd === runtime.projectRoot && params.permissionProfile === "claude-review" &&
    Object.keys(params.env).length === 0 && params.timeoutMs === 5000 && params.outputBytesCap === 4096 &&
    params.tty === false && params.streamStdin === false && params.streamStdoutStderr === false &&
    params.disableOutputCap === false && params.disableTimeout === false), true);
  assert.equal(await fs.readdir(runtime.scratchRoot).then((entries) => entries.length), 0);
  assert.match(configSnapshots[0], /^cli_auth_credentials_store = "file"$/m);
  assert.match(configSnapshots[0], /^shell_tool = true$/m);
  assert.match(configSnapshots[0], /^unified_exec = true$/m);
  assert.match(configSnapshots[0], /^code_mode_host = true$/m);
  assert.match(configSnapshots[0], /^multi_agent = true$/m);
  for (const [name, enabled] of Object.entries(CONSTRAINED_FEATURES)) {
    assert.match(configSnapshots[0], new RegExp(`^${name} = ${enabled}$`, "m"));
  }
  assert.doesNotMatch(configSnapshots[0], /^code_mode\s*=/m);
  assert.doesNotMatch(configSnapshots[0], /^code_mode_only\s*=/m);
  const featureList = requests.find(({ method }) => method === "experimentalFeature/list");
  assert.deepEqual(featureList.params, { threadId: result.attestation.threadId, limit: 100, cursor: null });
  assert.deepEqual(requests.find(({ method }) => method === "environment/status").params, { environmentId: "local" });
  assert.deepEqual(requests.find(({ method }) => method === "environment/info").params, { environmentId: "local" });
});

test("accepts only the exact machine-managed Witself hook exception and reports non-sterile inventory", async (t) => {
  const temp = await makeTemp();
  const { runtime, spawnImpl } = await fakeRuntime(temp, "managed-hooks");
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const result = await probeRuntime(runtime, { spawnImpl });
  assert.equal(result.attestation.inventory.hooks.count, 10);
  assert.equal(result.attestation.inventory.hooks.acceptedMode, "managed-witself");
  assert.equal(result.attestation.inventory.hooks.source, "system:/etc/codex/witself-hooks");
  assert.equal(result.attestation.inventory.hooks.sterile, false);
  assert.deepEqual(result.attestation.inventory.hooks.acceptedEvents, [
    "permissionRequest", "postCompact", "postToolUse", "preCompact", "preToolUse",
    "sessionStart", "stop", "subagentStart", "subagentStop", "userPromptSubmit",
  ]);
  assert.match(result.attestation.inventory.hooks.policyDigest, /^[0-9a-f]{64}$/);
});

test("turn sends only fixed model, permissions, roots, and structured schema", async (t) => {
  const temp = await makeTemp();
  const { runtime, spawnImpl, record } = await fakeRuntime(temp);
  const session = new AppServerSession(runtime, { spawnImpl });
  t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });
  await session.start();
  await session.attest();
  const report = await session.runTurn("Review authentication boundaries.", { timeoutMs: 2000 });
  assert.deepEqual(report, { summary: "reviewed", findings: [], checks: ["read-only inspection"], blockers: [] });
  const requests = await readRecord(record);
  const turn = requests.find(({ method }) => method === "turn/start");
  assert.equal(turn.params.model, "gpt-5.6-sol");
  assert.equal(turn.params.effort, "ultra");
  assert.equal(turn.params.cwd, runtime.projectRoot);
  assert.equal(turn.params.permissions, "claude-review");
  assert.equal(turn.params.sandboxPolicy, undefined);
  assert.deepEqual(turn.params.environments, [{
    environmentId: "local", cwd: runtime.projectRoot, runtimeWorkspaceRoots: [runtime.projectRoot],
  }]);
  assert.equal(turn.params.outputSchema.additionalProperties, false);
});

for (const [scenario, code] of [
  ["bad-account", "auth_attestation_failed"],
  ["bad-model", "model_incompatible"],
  ["bad-thread", "thread_attestation_failed"],
  ["unexpected-skill", "inventory_attestation_failed"],
  ["unexpected-hook", "inventory_attestation_failed"],
  ["unsafe-sentinel", "confinement_probe_failed"],
  ["unsafe-review-network", "confinement_probe_failed"],
  ["unexpected-capability", "unexpected_capability"],
  ["disabled-shell_tool", "execution_tooling_unavailable"],
  ["disabled-unified_exec", "execution_tooling_unavailable"],
  ["disabled-code_mode_host", "execution_tooling_unavailable"],
  ["disabled-multi_agent", "execution_tooling_unavailable"],
  ["enabled-image_generation", "execution_tooling_unavailable"],
  ["enabled-browser_use", "execution_tooling_unavailable"],
  ["enabled-computer_use", "execution_tooling_unavailable"],
  ["enabled-skill_mcp_dependency_install", "execution_tooling_unavailable"],
  ["enabled-auth_elicitation", "execution_tooling_unavailable"],
  ["disabled-hooks", "execution_tooling_unavailable"],
  ["enabled-goals", "execution_tooling_unavailable"],
  ["enabled-guardian_approval", "execution_tooling_unavailable"],
  ["missing-image_generation", "execution_tooling_unavailable"],
  ["missing-view_image", "execution_tooling_unavailable"],
  ["unexpected-enabled-feature", "execution_tooling_unavailable"],
  ["mixed-duplicate-view_image", "execution_tooling_unavailable"],
  ["local-environment-unavailable", "execution_tooling_unavailable"],
  ["local-environment-wrong-cwd", "execution_tooling_unavailable"],
]) {
  test(`fails closed for ${scenario}`, async (t) => {
    const temp = await makeTemp();
    const { runtime, spawnImpl } = await fakeRuntime(temp, scenario);
    const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 1000 });
    t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });
    const operation = scenario === "bad-account"
      ? session.start()
      : session.start().then(() => session.attest());
    await assert.rejects(operation, (error) => error?.code === code);
  });
}

for (const [scenario, code] of [
  ["reroute", "model_rerouted"],
  ["refresh-request", "auth_refresh_requested"],
  ["bad-result", "result_invalid"],
]) {
  test(`turn fails closed for ${scenario}`, async (t) => {
    const temp = await makeTemp();
    const { runtime, spawnImpl } = await fakeRuntime(temp, scenario);
    const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 1000 });
    t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });
    await session.start();
    await session.attest();
    await assert.rejects(session.runTurn("bounded review", { timeoutMs: 1000 }), (error) => error?.code === code);
  });
}

test("turn/start rejection disposes pending completion and app-server child", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "turn-reject");
  const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  await assert.rejects(session.runTurn("bounded", { timeoutMs: 500 }), (error) => error?.code === "app_server_request_failed");
  assert.equal(session.listenerCount("turn/completed"), 0);
  await session.shutdown();
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
});

test("invalid turn/start success disposes completion listeners and timer", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "invalid-turn-start");
  const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  await assert.rejects(session.runTurn("bounded", { timeoutMs: 1000 }), (error) => error?.code === "turn_attestation_failed");
  assert.equal(session.listenerCount("turn/completed"), 0);
  assert.equal(session.listenerCount("turnError"), 0);
  await new Promise((resolve) => setTimeout(resolve, 50));
  await session.shutdown();
});

test("timeout interrupts and reaps app-server child", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "hang");
  const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  await assert.rejects(session.runTurn("bounded", { timeoutMs: 30 }), (error) => error?.code === "job_timeout");
  await session.shutdown();
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
});

test("abort cancels and reaps app-server child", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "hang");
  const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  const controller = new AbortController();
  const turn = session.runTurn("bounded", { timeoutMs: 500, signal: controller.signal });
  setTimeout(() => controller.abort(), 20);
  await assert.rejects(turn, (error) => error?.code === "job_cancelled");
  await session.shutdown();
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
});

test("pre-turn cancel and owner shutdown race through one actual constrained cleanup", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl, record } = await fakeRuntime(temp, "hang");
  const session = new AppServerSession(runtime, { spawnImpl, requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  const scratch = session.scratch;
  assert.equal((await readRecord(record)).some(({ method }) => method === "turn/start"), false);
  const results = await Promise.allSettled([session.interrupt(), session.shutdown()]);
  assert.equal(results.every(({ status }) => status === "fulfilled"), true);
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
  await assert.rejects(fs.lstat(scratch), { code: "ENOENT" });
  assert.equal(await fs.readdir(runtime.scratchRoot).then((entries) => entries.length), 0);
});
