import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import test from "node:test";

import { ImplementationAppServerSession } from "../lib/app-server.mjs";
import { CONSTRAINED_FEATURES } from "../lib/execution-tooling.mjs";
import { fakeRuntime, makeTemp, readRecord } from "./helpers.mjs";

async function fakeWorkspace(runtime) {
  const root = path.join(runtime.scratchRoot, "implementation-jobs", "implementation-00000000-0000-4000-8000-000000000000");
  const workspaceRoot = path.join(root, "workspace");
  const home = path.join(root, "home");
  const temp = path.join(root, "tmp");
  for (const directory of [workspaceRoot, path.join(workspaceRoot, ".git"), home, path.join(home, ".config"), temp]) {
    await fs.mkdir(directory, { recursive: true, mode: 0o700 });
  }
  return Object.freeze({
    workspaceRoot,
    executionEnvironment: Object.freeze({
      HOME: home,
      XDG_CONFIG_HOME: path.join(home, ".config"),
      TMPDIR: temp,
      TMP: temp,
      TEMP: temp,
      PATH: "/usr/bin:/bin",
      LANG: "C",
      LC_ALL: "C",
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_CONFIG_SYSTEM: "/dev/null",
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_TERMINAL_PROMPT: "0",
      GIT_ASKPASS: "/usr/bin/false",
      SSH_ASKPASS: "/usr/bin/false",
      GIT_SSH_COMMAND: "/usr/bin/false",
    }),
  });
}

test("implementation session attests isolated write, source denial, and no network before its turn", async (t) => {
  const temp = await makeTemp();
  const { runtime, spawnImpl, spawnCalls, record, configSnapshots } = await fakeRuntime(temp);
  const workspace = await fakeWorkspace(runtime);
  const session = new ImplementationAppServerSession(runtime, workspace, { spawnImpl });
  t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });

  await session.start();
  const attestation = await session.attest();
  assert.equal(attestation.model, "gpt-5.6-sol");
  assert.equal(attestation.effort, "ultra");
  assert.equal(attestation.multiAgentVersion, "v2");
  assert.equal(attestation.permissionProfile, "claude-implementation");
  assert.equal(attestation.writableRoots, 3);
  assert.equal(attestation.networkAccess, false);
  assert.equal(attestation.cwd, workspace.workspaceRoot);
  assert.equal(attestation.inventory.hooks.sterile, true);
  assert.deepEqual(attestation.executionTooling, {
    environmentId: "local",
    environmentStatus: "ready",
    cwd: workspace.workspaceRoot,
    features: CONSTRAINED_FEATURES,
  });
  assert.deepEqual(attestation.confinement, {
    isolatedRepositoryRead: true,
    isolatedGitHistoryRead: true,
    isolatedWorkspaceWrite: true,
    isolatedGitMetadataWriteDenied: true,
    privateScratchWrite: true,
    sourceWorktreeDenied: true,
    brokerDeniedSentinelReadDenied: true,
    systemTemporaryDirectoryIsolation: "not-guaranteed-on-macos",
    hostSecretConfinement: "requires-outer-os-isolation",
    loopbackNetworkDenied: true,
    networkAccess: false,
  });

  let requests = await readRecord(record);
  assert.equal(requests.some(({ method }) => method === "turn/start"), false);
  const thread = requests.find(({ method }) => method === "thread/start");
  assert.equal(thread.params.cwd, workspace.workspaceRoot);
  assert.equal(thread.params.permissions, "claude-implementation");
  assert.equal(thread.params.allowProviderModelFallback, false);
  assert.equal(thread.params.ephemeral, true);
  assert.deepEqual(thread.params.runtimeWorkspaceRoots, [workspace.workspaceRoot]);
  assert.deepEqual(thread.params.dynamicTools, []);
  assert.deepEqual(thread.params.environments, [{
    environmentId: "local", cwd: workspace.workspaceRoot, runtimeWorkspaceRoots: [workspace.workspaceRoot],
  }]);
  assert.deepEqual(thread.params.selectedCapabilityRoots, []);
  assert.equal(thread.params.config.web_search, "disabled");
  assert.deepEqual(thread.params.config.features, CONSTRAINED_FEATURES);
  assert.match(configSnapshots[0], /^shell_tool = true$/m);
  assert.match(configSnapshots[0], /^unified_exec = true$/m);
  assert.match(configSnapshots[0], /^code_mode_host = true$/m);
  for (const [name, enabled] of Object.entries(CONSTRAINED_FEATURES)) {
    assert.match(configSnapshots[0], new RegExp(`^${name} = ${enabled}$`, "m"));
  }
  assert.doesNotMatch(configSnapshots[0], /^code_mode\s*=/m);
  assert.doesNotMatch(configSnapshots[0], /^code_mode_only\s*=/m);
  const profile = thread.params.config.permissions["claude-implementation"];
  assert.equal(profile.filesystem[":root"], "deny");
  assert.equal(profile.filesystem[":minimal"], "read");
  assert.equal(profile.filesystem[workspace.workspaceRoot], "write");
  assert.equal(profile.filesystem[path.join(workspace.workspaceRoot, ".git")], "read");
  assert.equal(profile.filesystem[runtime.projectRoot], undefined);
  assert.equal(profile.filesystem[workspace.executionEnvironment.HOME], "write");
  assert.equal(profile.filesystem[workspace.executionEnvironment.TMPDIR], "write");
  assert.deepEqual(profile.network, { enabled: false });
  const probes = requests.filter(({ method }) => method === "command/exec");
  assert.equal(probes.length, 11);
  assert.equal(probes.every(({ params }) => params.cwd === workspace.workspaceRoot &&
    params.permissionProfile === "claude-implementation" && Object.keys(params.env).length === 0 &&
    params.timeoutMs === 5000 && params.outputBytesCap === 4096), true);
  assert.equal(spawnCalls[0].options.cwd, workspace.workspaceRoot);
  assert.equal(spawnCalls[0].options.env.HOME, workspace.executionEnvironment.HOME);
  assert.equal(spawnCalls[0].options.env.CODEX_HOME.includes("implementation-"), true);

  const report = await session.runTurn("Implement the bounded change.", { timeoutMs: 1000 });
  assert.deepEqual(report, {
    summary: "implemented",
    actions: ["updated isolated workspace"],
    checks: ["local checks"],
    blockers: [],
    warnings: [],
  });
  requests = await readRecord(record);
  const turn = requests.find(({ method }) => method === "turn/start");
  assert.equal(turn.params.cwd, workspace.workspaceRoot);
  assert.equal(turn.params.permissions, "claude-implementation");
  assert.deepEqual(turn.params.environments, [{
    environmentId: "local", cwd: workspace.workspaceRoot, runtimeWorkspaceRoots: [workspace.workspaceRoot],
  }]);
  assert.equal(turn.params.outputSchema.additionalProperties, false);
});

for (const [scenario, code] of [
  ["bad-implementation-sandbox", "thread_attestation_failed"],
  ["unsafe-implementation-source", "confinement_probe_failed"],
  ["unsafe-implementation-git-write", "confinement_probe_failed"],
  ["unsafe-implementation-network", "confinement_probe_failed"],
  ["disabled-code_mode_host", "execution_tooling_unavailable"],
  ["enabled-image_generation", "execution_tooling_unavailable"],
  ["enabled-browser_use_external", "execution_tooling_unavailable"],
  ["enabled-remote_plugin", "execution_tooling_unavailable"],
  ["missing-computer_use", "execution_tooling_unavailable"],
  ["local-environment-unavailable", "execution_tooling_unavailable"],
]) {
  test(`implementation session fails closed for ${scenario}`, async (t) => {
    const temp = await makeTemp();
    const { runtime, spawnImpl } = await fakeRuntime(temp, scenario);
    const workspace = await fakeWorkspace(runtime);
    const session = new ImplementationAppServerSession(runtime, workspace, { spawnImpl });
    t.after(async () => { await session.shutdown(); await fs.rm(temp, { recursive: true, force: true }); });
    await session.start();
    await assert.rejects(session.attest(), (error) => error?.code === code);
  });
}

test("implementation result schema and timeout fail closed and reap the child", async (t) => {
  const badTemp = await makeTemp();
  const bad = await fakeRuntime(badTemp, "bad-implementation-result");
  const badSession = new ImplementationAppServerSession(bad.runtime, await fakeWorkspace(bad.runtime), { spawnImpl: bad.spawnImpl });
  await badSession.start();
  await badSession.attest();
  await assert.rejects(badSession.runTurn("bounded", { timeoutMs: 1000 }), (error) => error?.code === "result_invalid");
  await badSession.shutdown();
  await fs.rm(badTemp, { recursive: true, force: true });

  const hangTemp = await makeTemp();
  const hang = await fakeRuntime(hangTemp, "hang");
  const hangSession = new ImplementationAppServerSession(hang.runtime, await fakeWorkspace(hang.runtime), { spawnImpl: hang.spawnImpl });
  t.after(async () => { await hangSession.shutdown(); await fs.rm(hangTemp, { recursive: true, force: true }); });
  await hangSession.start();
  await hangSession.attest();
  await assert.rejects(hangSession.runTurn("bounded", { timeoutMs: 30 }), (error) => error?.code === "job_timeout");
  await hangSession.shutdown();
  assert.notEqual(hangSession.child.exitCode ?? hangSession.child.signalCode, null);
});

test("implementation invalid turn/start success disposes completion listeners and timer", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const fake = await fakeRuntime(temp, "invalid-turn-start");
  const session = new ImplementationAppServerSession(fake.runtime, await fakeWorkspace(fake.runtime), { spawnImpl: fake.spawnImpl });
  await session.start();
  await session.attest();
  await assert.rejects(session.runTurn("bounded", { timeoutMs: 1000 }), (error) => error?.code === "turn_attestation_failed");
  assert.equal(session.listenerCount("turn/completed"), 0);
  assert.equal(session.listenerCount("turnError"), 0);
  await new Promise((resolve) => setTimeout(resolve, 50));
  await session.shutdown();
});

test("implementation cancel and EOF cleanup race preserves the sole monitor hook", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const fake = await fakeRuntime(temp, "hang");
  const session = new ImplementationAppServerSession(
    fake.runtime, await fakeWorkspace(fake.runtime), { spawnImpl: fake.spawnImpl, requestTimeoutMs: 500 },
  );
  await session.start();
  await session.attest();
  const scratch = session.scratch;
  let hookCalls = 0;
  const results = await Promise.allSettled([
    session.interrupt(),
    session.shutdown({ beforeScratchCleanup: async () => {
      hookCalls += 1;
      assert.equal((await fs.lstat(scratch)).isDirectory(), true);
    } }),
  ]);
  assert.equal(results.every(({ status }) => status === "fulfilled"), true);
  assert.equal(hookCalls, 1);
  assert.notEqual(session.child.exitCode ?? session.child.signalCode, null);
  await assert.rejects(fs.lstat(scratch), { code: "ENOENT" });
});
