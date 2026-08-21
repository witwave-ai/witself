import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import test from "node:test";

import { SystemAppServerSession, probeSystemRuntime } from "../lib/system-app-server.mjs";
import { SYSTEM_FEATURES } from "../lib/execution-tooling.mjs";
import { fakeRuntime, makeTemp, readRecord } from "./helpers.mjs";

function systemEnvironment(runtime, secret = "launcher-secret-123456") {
  return Object.freeze({
    ...runtime.env,
    HOME: path.dirname(runtime.projectRoot),
    PATH: process.env.PATH ?? "/usr/bin:/bin",
    FAKE_LAUNCHER_SECRET: secret,
    WITSELF_CODEX_CEILING: "system",
    WITSELF_CODEX_GRANT_KEY_FILE: "/must/not/reach/system-child",
  });
}

test("system probe attests exact danger-full-access policy and host capabilities without a model turn", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl, spawnCalls, record, configSnapshots } = await fakeRuntime(temp);
  const launcherEnvironment = systemEnvironment(runtime);
  const result = await probeSystemRuntime(runtime, { spawnImpl, launcherEnvironment });
  assert.equal(result.modelCalls, 0);
  assert.equal(result.runtime.latestVerificationPolicy, "before-every-new-work");
  assert.equal(result.runtime.latestVerifiedAt, 2_000_000_000_000);
  assert.equal(result.attestation.model, "gpt-5.6-sol");
  assert.equal(result.attestation.effort, "ultra");
  assert.equal(result.attestation.permissionProfile, ":danger-full-access");
  assert.equal(result.attestation.sandbox, "dangerFullAccess");
  assert.deepEqual(result.attestation.executionTooling, {
    environmentId: "local",
    environmentStatus: "ready",
    cwd: runtime.projectRoot,
    features: SYSTEM_FEATURES,
  });
  assert.deepEqual(result.attestation.capabilities, {
    outsideRepositoryRead: true,
    privateScratchWrite: true,
    childProcess: true,
    loopbackNetwork: true,
    effectiveUserMatches: true,
  });
  assert.equal(result.attestation.policy.compatible, true);
  assert.deepEqual(result.attestation.influences.systemSkills.names, [
    "imagegen", "openai-docs", "plugin-creator", "review-agent", "skill-creator", "skill-installer",
  ]);
  assert.equal(result.attestation.influences.machineManagedHooks.count, 0);
  assert.equal(result.attestation.influences.instructionSources.count, 0);
  assert.equal(result.attestation.influences.instructionSources.sterile, true);
  assert.deepEqual(await fs.readdir(runtime.scratchRoot), []);

  const requests = await readRecord(record);
  assert.equal(requests.some(({ method }) => method === "turn/start"), false);
  const profile = requests.find(({ method }) => method === "permissionProfile/list");
  assert.deepEqual(profile.params, { cwd: runtime.projectRoot, limit: 100, cursor: null });
  assert.equal(requests.some(({ method }) => method === "configRequirements/read"), true);
  const thread = requests.find(({ method }) => method === "thread/start");
  assert.equal(thread.params.permissions, ":danger-full-access");
  assert.equal(thread.params.sandbox, undefined);
  assert.equal(thread.params.sandboxPolicy, undefined);
  assert.equal(thread.params.allowProviderModelFallback, false);
  assert.equal(thread.params.approvalPolicy, "never");
  assert.equal(thread.params.ephemeral, true);
  assert.deepEqual(thread.params.dynamicTools, []);
  assert.deepEqual(thread.params.environments, [{
    environmentId: "local", cwd: runtime.projectRoot, runtimeWorkspaceRoots: [runtime.projectRoot],
  }]);
  assert.deepEqual(thread.params.selectedCapabilityRoots, []);
  assert.deepEqual(thread.params.runtimeWorkspaceRoots, [runtime.projectRoot]);
  assert.deepEqual(thread.params.config.shell_environment_policy, { inherit: "all", ignore_default_excludes: true });
  assert.equal(thread.params.config.web_search, "live");
  assert.deepEqual(thread.params.config.features, SYSTEM_FEATURES);
  assert.match(configSnapshots[0], /^shell_tool = true$/m);
  assert.match(configSnapshots[0], /^unified_exec = true$/m);
  assert.match(configSnapshots[0], /^code_mode_host = true$/m);
  assert.doesNotMatch(configSnapshots[0], /^code_mode\s*=/m);
  assert.doesNotMatch(configSnapshots[0], /^code_mode_only\s*=/m);
  assert.deepEqual(requests.find(({ method }) => method === "experimentalFeature/list").params, {
    threadId: result.attestation.threadId, limit: 100, cursor: null,
  });
  assert.deepEqual(requests.find(({ method }) => method === "environment/status").params, { environmentId: "local" });
  assert.deepEqual(requests.find(({ method }) => method === "environment/info").params, { environmentId: "local" });
  const commands = requests.filter(({ method }) => method === "command/exec");
  assert.equal(commands.length, 6);
  assert.equal(commands.every(({ params }) => params.permissionProfile === ":danger-full-access" &&
    params.cwd === runtime.projectRoot && Object.keys(params.env).length === 0 &&
    params.timeoutMs === 5000 && params.outputBytesCap === 4096), true);

  assert.equal(spawnCalls.length, 1);
  const childEnvironment = spawnCalls[0].options.env;
  assert.equal(childEnvironment.FAKE_LAUNCHER_SECRET, launcherEnvironment.FAKE_LAUNCHER_SECRET);
  assert.equal(childEnvironment.HOME, launcherEnvironment.HOME);
  assert.equal(childEnvironment.WITSELF_CODEX_CEILING, undefined);
  assert.equal(childEnvironment.WITSELF_CODEX_GRANT_KEY_FILE, undefined);
  assert.notEqual(childEnvironment.CODEX_HOME, launcherEnvironment.CODEX_HOME);
  assert.notEqual(childEnvironment.TMPDIR, launcherEnvironment.TMPDIR);
  assert.equal(JSON.stringify(requests).includes(launcherEnvironment.FAKE_LAUNCHER_SECRET), false);
});

test("system turn repeats fixed authority fields and redacts launcher environment values", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl, record } = await fakeRuntime(temp, "system-secret-result");
  const secret = "super-sensitive-launcher-value";
  const session = new SystemAppServerSession(runtime, { spawnImpl, launcherEnvironment: systemEnvironment(runtime, secret) });
  t.after(() => session.shutdown());
  await session.start();
  await session.attest();
  const report = await session.runTurn("Perform the bounded system task.", { timeoutMs: 1000 });
  assert.equal(JSON.stringify(report).includes(secret), false);
  assert.match(report.summary, /\[REDACTED_ENV\]/);
  const requests = await readRecord(record);
  const turn = requests.find(({ method }) => method === "turn/start");
  assert.equal(turn.params.model, "gpt-5.6-sol");
  assert.equal(turn.params.effort, "ultra");
  assert.equal(turn.params.cwd, runtime.projectRoot);
  assert.equal(turn.params.permissions, ":danger-full-access");
  assert.equal(turn.params.approvalPolicy, "never");
  assert.equal(turn.params.sandboxPolicy, undefined);
  assert.deepEqual(turn.params.environments, [{
    environmentId: "local", cwd: runtime.projectRoot, runtimeWorkspaceRoots: [runtime.projectRoot],
  }]);
  assert.equal(turn.params.outputSchema.additionalProperties, false);
  const scratch = session.scratch;
  await session.shutdown();
  await assert.rejects(fs.lstat(scratch), { code: "ENOENT" });
});

test("system probe truthfully attests exact machine-managed hooks as non-sterile influences", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "managed-hooks");
  const result = await probeSystemRuntime(runtime, { spawnImpl, launcherEnvironment: systemEnvironment(runtime) });
  assert.equal(result.attestation.influences.machineManagedHooks.mode, "managed-witself");
  assert.equal(result.attestation.influences.machineManagedHooks.count, 10);
  assert.equal(result.attestation.influences.instructionSources.sterile, false);
});

for (const [scenario, code] of [
  ["system-profile-denied", "system_profile_disallowed"],
  ["system-requirements-denied", "system_requirements_incompatible"],
  ["bad-system-thread", "system_thread_attestation_failed"],
  ["bad-system-sandbox", "system_thread_attestation_failed"],
  ["unexpected-skill", "system_inventory_attestation_failed"],
  ["unexpected-hook", "system_inventory_attestation_failed"],
  ["disabled-shell_tool", "system_execution_tooling_unavailable"],
  ["disabled-unified_exec", "system_execution_tooling_unavailable"],
  ["disabled-code_mode_host", "system_execution_tooling_unavailable"],
  ["enabled-goals", "system_execution_tooling_unavailable"],
  ["enabled-guardian_approval", "system_execution_tooling_unavailable"],
  ["local-environment-unavailable", "system_execution_tooling_unavailable"],
  ["local-environment-wrong-cwd", "system_execution_tooling_unavailable"],
]) {
  test(`system policy fails closed for ${scenario}`, async (t) => {
    const temp = await makeTemp();
    const { runtime, spawnImpl } = await fakeRuntime(temp, scenario);
    const session = new SystemAppServerSession(runtime, { spawnImpl, launcherEnvironment: systemEnvironment(runtime) });
    t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });
    await session.start();
    await assert.rejects(session.attest(), (error) => error?.code === code);
  });
}

test("system timeout and cancellation reap the app-server child", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "hang");
  const session = new SystemAppServerSession(runtime, { spawnImpl, launcherEnvironment: systemEnvironment(runtime), requestTimeoutMs: 500 });
  await session.start();
  await session.attest();
  await assert.rejects(session.runTurn("bounded", { timeoutMs: 30 }), (error) => error?.code === "job_timeout");
  await session.shutdown();
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
});

test("system shutdown fails closed when a full-access child ignores TERM and post-KILL reap", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime } = await fakeRuntime(temp);
  const session = new SystemAppServerSession(runtime, {
    launcherEnvironment: systemEnvironment(runtime),
    shutdownTermMs: 5,
    shutdownKillMs: 5,
  });
  let kills = 0;
  session.child = {
    pid: null,
    exitCode: null,
    signalCode: null,
    stdin: { end() {} },
    kill() { kills += 1; return true; },
  };
  session.exitPromise = new Promise(() => {});
  await assert.rejects(session.shutdown(), (error) => error?.code === "system_cleanup_failed");
  assert.equal(kills >= 2, true);
});

test("pre-turn system cancel and owner shutdown race through one actual cleanup", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const { runtime, spawnImpl } = await fakeRuntime(temp, "hang");
  const session = new SystemAppServerSession(runtime, {
    spawnImpl, launcherEnvironment: systemEnvironment(runtime), requestTimeoutMs: 500,
  });
  await session.start();
  await session.attest();
  const scratch = session.scratch;
  const results = await Promise.allSettled([session.interrupt(), session.shutdown()]);
  assert.equal(results.every(({ status }) => status === "fulfilled"), true);
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
  await assert.rejects(fs.lstat(scratch), { code: "ENOENT" });
});
