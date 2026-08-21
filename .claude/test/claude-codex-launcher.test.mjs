import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import test from "node:test";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import {
  CEILING_ENV,
  GRANT_FIELD,
  GRANT_KEY_FILE_ENV,
  IMPLEMENTATION_TOOLS,
  REVIEW_TOOLS,
  SYSTEM_TOOLS,
  canonicalJson,
  createGrant,
  visibleToolsForCeiling,
} from "../codex-profiles.mjs";
import { evaluateHookInput } from "../hooks/codex-ceiling-guard.mjs";
import {
  buildClaudeArgs,
  buildHookSettings,
  buildMcpConfig,
  createSessionArtifacts,
  launcherEnvironment,
  parseLauncherArgs,
  resolveClaudeExecutable,
  validateProjectSurface,
  verifyClaudeCli,
} from "../../scripts/claude-codex.mjs";

const TEST_DIR = path.dirname(fileURLToPath(import.meta.url));
const PROJECT_ROOT = fs.realpathSync(path.resolve(TEST_DIR, "../.."));
const LAUNCHER = path.join(PROJECT_ROOT, "scripts", "claude-codex.mjs");
const HOOK = path.join(PROJECT_ROOT, ".claude", "hooks", "codex-ceiling-guard.mjs");
const PROJECT_MCP = path.join(PROJECT_ROOT, ".mcp.json");
const PROJECT_SETTINGS = path.join(PROJECT_ROOT, ".claude", "settings.json");
const DELEGATION_SKILL = path.join(PROJECT_ROOT, ".claude", "skills", "codex-delegation", "SKILL.md");
const FAKE_CLAUDE = path.join(TEST_DIR, "fixtures", "fake-claude.mjs");

function makeTemporaryDirectory(t) {
  const directory = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "witself-launcher-test-"));
  fs.chmodSync(directory, 0o700);
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  return directory;
}

function hookInput(tool, mode, toolInput = { task: "bounded task" }) {
  return {
    hook_event_name: "PreToolUse",
    tool_name: `mcp__codex-local__${tool}`,
    permission_mode: mode,
    tool_use_id: "toolu_test_123",
    session_id: "session-test-123",
    tool_input: toolInput,
  };
}

async function waitForFile(filePath, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (fs.existsSync(filePath)) return;
    await delay(20);
  }
  throw new Error(`Timed out waiting for ${filePath}.`);
}

async function waitForExit(child, timeoutMs = 7_500) {
  return await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      try { child.kill("SIGKILL"); } catch {}
      reject(new Error("Timed out waiting for launcher exit."));
    }, timeoutMs);
    child.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal });
    });
  });
}

async function waitForPidGone(pid, timeoutMs = 2_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      process.kill(pid, 0);
    } catch (error) {
      if (error?.code === "ESRCH") return;
      throw error;
    }
    await delay(20);
  }
  throw new Error(`Process ${pid} remained alive after launcher shutdown.`);
}

test("launcher profiles have exact cumulative tool visibility", () => {
  assert.deepEqual(visibleToolsForCeiling("repository"), [...REVIEW_TOOLS]);
  assert.deepEqual(visibleToolsForCeiling("isolated-write"), [...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS]);
  assert.deepEqual(visibleToolsForCeiling("system"), [...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS, ...SYSTEM_TOOLS]);
  assert.throws(() => visibleToolsForCeiling("unbounded"), /Unknown Codex ceiling/u);
});

test("launcher help states the same-user grant-key limitation", () => {
  const help = execFileSync(process.execPath, [LAUNCHER, "--help"], { encoding: "utf8" });
  assert.match(help, /not an OS security\s+boundary against a deliberately malicious process running as the same user/u);
  assert.match(help, /Windows can only guarantee direct-child\s+termination/u);
});

test("delegation skill assigns reserved grant creation only to the trusted hook", () => {
  const skill = fs.readFileSync(DELEGATION_SKILL, "utf8");
  assert.match(skill, /Call the tool without that field: the\s+launcher's trusted `PreToolUse` hook injects a fresh one-use grant/u);
  assert.match(skill, /stop rather than trying to manufacture or recover one/u);
});

test("launcher, broker, hook, Node runtime, and Git root are canonical", () => {
  assert.equal(validateProjectSurface(PROJECT_ROOT).projectRoot, PROJECT_ROOT);
  assert.notEqual(fs.statSync(LAUNCHER).mode & 0o111, 0);
});

test("launcher parsing freezes profile-compatible Claude permission modes", () => {
  assert.deepEqual(parseLauncherArgs([]), {
    ceiling: "repository", permissionMode: "plan", inspect: false, help: false, forwarded: [],
  });
  assert.equal(parseLauncherArgs(["--ceiling", "repository", "--permission-mode", "manual"]).permissionMode, "manual");
  assert.equal(parseLauncherArgs(["--ceiling=isolated-write", "--permission-mode=auto"]).permissionMode, "auto");
  assert.equal(parseLauncherArgs(["--ceiling", "system"]).permissionMode, "bypassPermissions");
  assert.throws(() => parseLauncherArgs(["--ceiling", "system", "--permission-mode", "auto"]), /not valid/u);
  assert.throws(() => parseLauncherArgs(["--ceiling", "isolated-write", "--permission-mode", "plan"]), /not valid/u);
  assert.throws(() => parseLauncherArgs(["--ceiling", "repository", "--permission-mode", "bypassPermissions"]), /not valid/u);
});

test("forwarded arguments cannot replace launcher-owned MCP, mode, or hook settings", () => {
  for (const flag of [
    "--mcp-config", "--strict-mcp-config", "--permission-mode=auto",
    "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
    "--settings", "--setting-sources=user", "--safe-mode", "--bare",
  ]) {
    assert.throws(() => parseLauncherArgs(["--", flag]), /controlled by the trusted Codex launcher/u, flag);
  }
  assert.deepEqual(parseLauncherArgs(["--ceiling", "system", "--", "--continue"]).forwarded, ["--continue"]);
});

test("launcher excludes mutable ordinary settings while preserving its private settings file", () => {
  const args = buildClaudeArgs({
    configFile: "/private/session/mcp.json",
    settingsFile: "/private/session/settings.json",
    permissionMode: "acceptEdits",
  });
  const sources = args.indexOf("--setting-sources");
  const settings = args.indexOf("--settings");
  assert.notEqual(sources, -1);
  assert.equal(args[sources + 1], "");
  assert.notEqual(settings, -1);
  assert.equal(args[settings + 1], "/private/session/settings.json");
  assert.ok(sources < settings);

  for (const hostile of [
    ["--setting-sources", "project"],
    ["--setting-sources=user,project,local"],
    ["--settings", "/tmp/hostile-settings.json"],
    ["--settings=/tmp/hostile-settings.json"],
  ]) {
    assert.throws(
      () => buildClaudeArgs({
        configFile: "/private/session/mcp.json",
        settingsFile: "/private/session/settings.json",
        permissionMode: "acceptEdits",
        forwarded: hostile,
      }),
      /controlled by the trusted Codex launcher/u,
    );
  }
});

test("private MCP config pins repository, ceiling, grant path, and exact Node executable", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const keyFile = path.join(temporary, "grant.key");
  fs.writeFileSync(keyFile, crypto.randomBytes(32), { mode: 0o600 });
  const config = buildMcpConfig({
    projectRoot: PROJECT_ROOT,
    ceiling: "system",
    grantKeyFile: keyFile,
    nodePath: process.execPath,
  });
  const server = config.mcpServers["codex-local"];
  assert.equal(server.command, process.execPath);
  assert.deepEqual(server.args.slice(-6), [
    "--repository", PROJECT_ROOT, "--ceiling", "system", "--grant-key-file", keyFile,
  ]);
  assert.deepEqual(Object.keys(config.mcpServers), ["codex-local"]);
  assert.throws(() => buildMcpConfig({
    projectRoot: PROJECT_ROOT, ceiling: "repository", grantKeyFile: keyFile,
  }), /must not receive/u);
});

test("checked-in Claude configuration exposes only repository review and a deny-only hook", () => {
  const mcp = JSON.parse(fs.readFileSync(PROJECT_MCP, "utf8"));
  assert.deepEqual(Object.keys(mcp.mcpServers), ["codex-local"]);
  assert.deepEqual(mcp.mcpServers["codex-local"], {
    type: "stdio",
    command: "node",
    args: [
      "${CLAUDE_PROJECT_DIR:-.}/tools/claude-codex-broker/server.mjs",
      "--repository",
      "${CLAUDE_PROJECT_DIR:-.}",
      "--ceiling",
      "repository",
    ],
    env: {},
  });
  const settings = JSON.parse(fs.readFileSync(PROJECT_SETTINGS, "utf8"));
  const hook = settings.hooks.PreToolUse[0];
  assert.equal(hook.matcher, "^mcp__codex-local__.*$");
  assert.deepEqual(hook.hooks[0].args, [
    "${CLAUDE_PROJECT_DIR}/.claude/hooks/codex-ceiling-guard.mjs",
    "--deny-only",
  ]);
});

test("Claude argv uses strict config and an exact-process grant hook", () => {
  const settings = buildHookSettings({ nodePath: process.execPath });
  const hook = settings.hooks.PreToolUse[0];
  assert.equal(hook.matcher, "^mcp__codex-local__.*$");
  assert.equal(hook.hooks[0].command, process.execPath);
  assert.equal(hook.hooks[0].args.at(-1), "--issue-grant");

  const args = buildClaudeArgs({
    configFile: "/private/session/mcp.json",
    settingsFile: "/private/session/settings.json",
    permissionMode: "bypassPermissions",
    forwarded: ["finish this"],
  });
  assert.deepEqual(args.slice(0, 9), [
    "--strict-mcp-config", "--mcp-config", "/private/session/mcp.json",
    "--setting-sources", "",
    "--settings", "/private/session/settings.json", "--permission-mode", "bypassPermissions",
  ]);
  assert.ok(args.includes("--dangerously-skip-permissions"));
  assert.equal(args.at(-1), "finish this");
});

test("session artifacts are private and removed without recursive cleanup", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const artifacts = createSessionArtifacts({
    projectRoot: PROJECT_ROOT,
    ceiling: "isolated-write",
    permissionMode: "acceptEdits",
    temporaryRoot: temporary,
  });
  assert.equal(fs.lstatSync(artifacts.sessionDir).mode & 0o077, 0);
  for (const artifact of [artifacts.configFile, artifacts.settingsFile, artifacts.grantKeyFile]) {
    assert.equal(fs.lstatSync(artifact).mode & 0o077, 0);
  }
  assert.equal(fs.statSync(artifacts.grantKeyFile).size, 32);
  assert.ok(artifacts.runtimeSnapshot.brokerPath.startsWith(`${artifacts.sessionDir}${path.sep}`));
  assert.ok(artifacts.runtimeSnapshot.hookPath.startsWith(`${artifacts.sessionDir}${path.sep}`));
  assert.notEqual(artifacts.runtimeSnapshot.brokerPath, path.join(PROJECT_ROOT, "tools", "claude-codex-broker", "server.mjs"));
  assert.equal(artifacts.verifySnapshot(), true);
  assert.ok(artifacts.runtimeSnapshot.manifest.files.length >= 10);
  const frozenPaths = new Set(artifacts.runtimeSnapshot.manifest.files.map((file) => file.path));
  assert.ok(frozenPaths.has("tools/claude-codex-broker/server.mjs"));
  assert.ok(frozenPaths.has("tools/claude-codex-broker/lib/grants.mjs"));
  assert.ok(frozenPaths.has(".claude/codex-profiles.mjs"));
  assert.ok(frozenPaths.has(".claude/hooks/codex-ceiling-guard.mjs"));
  assert.equal(fs.lstatSync(artifacts.runtimeSnapshot.brokerPath).mode & 0o222, 0);
  assert.equal(fs.lstatSync(artifacts.runtimeSnapshot.hookPath).mode & 0o222, 0);
  assert.equal(fs.lstatSync(artifacts.configFile).mode & 0o222, 0);
  assert.equal(fs.lstatSync(artifacts.settingsFile).mode & 0o222, 0);
  assert.equal(artifacts.mcpConfig.mcpServers["codex-local"].args[0], artifacts.runtimeSnapshot.brokerPath);
  assert.equal(artifacts.hookSettings.hooks.PreToolUse[0].hooks[0].args[0], artifacts.runtimeSnapshot.hookPath);
  assert.deepEqual(artifacts.cleanup(), []);
  assert.equal(fs.existsSync(artifacts.sessionDir), false);
});

test("frozen runtime verification detects in-place broker mutation and cleanup remains inode-safe", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const artifacts = createSessionArtifacts({
    projectRoot: PROJECT_ROOT,
    ceiling: "system",
    permissionMode: "bypassPermissions",
    temporaryRoot: temporary,
  });
  fs.chmodSync(artifacts.runtimeSnapshot.brokerPath, 0o600);
  fs.appendFileSync(artifacts.runtimeSnapshot.brokerPath, "\n// mutation\n");
  assert.throws(() => artifacts.verifySnapshot(), /Frozen runtime file changed/u);
  assert.deepEqual(artifacts.cleanup(), []);
  assert.equal(fs.existsSync(artifacts.sessionDir), false);
});

test("grantless review calls remain usable at every valid Claude mode", () => {
  for (const mode of ["default", "plan", "acceptEdits", "auto", "dontAsk", "bypassPermissions"]) {
    const result = evaluateHookInput(hookInput("codex_review_start", mode), { [CEILING_ENV]: "repository" });
    assert.equal(result.action, "ignore", mode);
  }
});

test("deny-only project hook cannot mint an elevated grant", () => {
  const result = evaluateHookInput(
    hookInput("codex_implementation_start", "acceptEdits"),
    { [CEILING_ENV]: "isolated-write" },
    { issueGrant: false },
  );
  assert.equal(result.action, "ignore");
  assert.equal(result.output, undefined);
});

test("hook CLI requires an exact operating mode and only issue-grant updates input", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const keyFile = path.join(temporary, "grant.key");
  fs.writeFileSync(keyFile, Buffer.alloc(32, 0x44), { mode: 0o600 });
  const input = JSON.stringify(hookInput("codex_implementation_start", "acceptEdits"));
  const environment = {
    ...process.env,
    [CEILING_ENV]: "isolated-write",
    [GRANT_KEY_FILE_ENV]: keyFile,
  };
  const denyOnly = spawnSync(process.execPath, [HOOK, "--deny-only"], {
    input, env: environment, encoding: "utf8",
  });
  assert.equal(denyOnly.status, 0);
  assert.equal(denyOnly.stdout, "");
  const issueGrant = spawnSync(process.execPath, [HOOK, "--issue-grant"], {
    input, env: environment, encoding: "utf8",
  });
  assert.equal(issueGrant.status, 0, issueGrant.stderr);
  assert.ok(JSON.parse(issueGrant.stdout).hookSpecificOutput.updatedInput[GRANT_FIELD]);
  const missingMode = spawnSync(process.execPath, [HOOK], {
    input, env: environment, encoding: "utf8",
  });
  assert.equal(missingMode.status, 2);
  assert.equal(JSON.parse(missingMode.stdout).hookSpecificOutput.permissionDecision, "deny");
});

test("elevated hook mints a one-use proof bound to exact input, mode, tool, and session", () => {
  const key = Buffer.alloc(32, 0x5a);
  const originalInput = { task: "implement", nested: { z: 2, a: true }, paths: ["a", "b"] };
  const input = hookInput("codex_implementation_start", "auto", originalInput);
  const result = evaluateHookInput(input, { [CEILING_ENV]: "isolated-write" }, {
    issueGrant: true,
    key,
    now: () => 1_800_000_000_000,
    nonce: () => "fixed-nonce",
  });
  assert.equal(result.action, "grant");
  assert.deepEqual(result.output.hookSpecificOutput.updatedInput.task, "implement");
  const grant = result.output.hookSpecificOutput.updatedInput[GRANT_FIELD];
  assert.equal(grant.tool, "codex_implementation_start");
  assert.equal(grant.ceiling, "isolated-write");
  assert.equal(grant.mode, "auto");
  assert.equal(grant.tool_use_id, input.tool_use_id);
  assert.equal(grant.session_id, input.session_id);
  assert.equal(grant.issued_at_ms, 1_800_000_000_000);
  assert.equal(grant.input_sha256, crypto.createHash("sha256").update(canonicalJson(originalInput)).digest("hex"));
  const { mac, ...signed } = grant;
  const expected = crypto.createHmac("sha256", key).update(canonicalJson(signed)).digest("base64url");
  assert.equal(mac, expected);
});

test("hook rejects ceiling escalation, mode mismatch, unknown tools, and caller grants", () => {
  assert.equal(evaluateHookInput(
    hookInput("codex_implementation_start", "acceptEdits"),
    { [CEILING_ENV]: "repository" }, { issueGrant: true, key: Buffer.alloc(32) },
  ).action, "deny");
  assert.equal(evaluateHookInput(
    hookInput("codex_implementation_start", "plan"),
    { [CEILING_ENV]: "isolated-write" }, { issueGrant: true, key: Buffer.alloc(32) },
  ).action, "deny");
  assert.equal(evaluateHookInput(
    hookInput("codex_system_start", "auto"),
    { [CEILING_ENV]: "system" }, { issueGrant: true, key: Buffer.alloc(32) },
  ).action, "deny");
  assert.equal(evaluateHookInput(
    hookInput("codex_unlisted", "bypassPermissions"),
    { [CEILING_ENV]: "system" }, { issueGrant: true, key: Buffer.alloc(32) },
  ).action, "deny");
  assert.equal(evaluateHookInput(
    hookInput("codex_system_start", "bypassPermissions", { task: "x", [GRANT_FIELD]: {} }),
    { [CEILING_ENV]: "system" }, { issueGrant: true, key: Buffer.alloc(32) },
  ).action, "deny");
});

test("elevated observation and cancellation remain grantable after an in-session mode downgrade", () => {
  const key = Buffer.alloc(32, 0x31);
  for (const [ceiling, tool] of [
    ["isolated-write", "codex_implementation_status"],
    ["isolated-write", "codex_implementation_artifact_read"],
    ["isolated-write", "codex_implementation_cancel"],
    ["system", "codex_system_status"],
    ["system", "codex_system_cancel"],
  ]) {
    const result = evaluateHookInput(
      hookInput(tool, "plan", { job_id: "11111111-1111-4111-8111-111111111111" }),
      { [CEILING_ENV]: ceiling },
      { issueGrant: true, key, now: () => 1_800_000_000_000, nonce: () => `nonce-${tool}` },
    );
    assert.equal(result.action, "grant", tool);
    assert.equal(result.grant.mode, "plan", tool);
  }
  assert.equal(evaluateHookInput(
    hookInput("codex_system_start", "plan"),
    { [CEILING_ENV]: "system" },
    { issueGrant: true, key },
  ).action, "deny");
});

test("grant creation rejects reserved input and canonicalization is key-order stable", () => {
  assert.equal(canonicalJson({ z: 1, a: { y: 2, x: 3 } }), canonicalJson({ a: { x: 3, y: 2 }, z: 1 }));
  assert.throws(() => createGrant({
    key: Buffer.alloc(32),
    ceiling: "system",
    toolName: "codex_system_start",
    mode: "bypassPermissions",
    toolUseId: "toolu_x",
    originalInput: { [GRANT_FIELD]: {} },
  }), /reserved/u);
});

test("launcher environment overwrites stale profile state and clears repository grant paths", () => {
  const elevated = launcherEnvironment({ [CEILING_ENV]: "bad" }, "system", "/private/key");
  assert.equal(elevated[CEILING_ENV], "system");
  assert.equal(elevated[GRANT_KEY_FILE_ENV], "/private/key");
  const repository = launcherEnvironment(elevated, "repository", null);
  assert.equal(repository[CEILING_ENV], "repository");
  assert.equal(Object.hasOwn(repository, GRANT_KEY_FILE_ENV), false);
});

test("Claude capability preflight is model-free and fails closed on missing flags", () => {
  const calls = [];
  const runner = (_executable, args) => {
    calls.push(args);
    if (args[0] === "--version") return { status: 0, stdout: "2.1.224 (Claude Code)\n", stderr: "" };
    return {
      status: 0,
      stdout: "--mcp-config --strict-mcp-config --permission-mode --dangerously-skip-permissions --setting-sources --settings",
      stderr: "",
    };
  };
  assert.equal(verifyClaudeCli("fake", runner).version, "2.1.224");
  assert.deepEqual(calls, [["--version"], ["--help"]]);
  assert.throws(() => verifyClaudeCli("fake", (_executable, args) => (
    args[0] === "--version"
      ? { status: 0, stdout: "2.1.224", stderr: "" }
      : { status: 0, stdout: "--mcp-config", stderr: "" }
  )), /does not advertise/u);
});

test("Claude executable resolution freezes the canonical PATH target and rejects relative overrides", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const bin = path.join(temporary, "bin");
  fs.mkdirSync(bin);
  const link = path.join(bin, "claude");
  fs.symlinkSync(FAKE_CLAUDE, link);
  const resolved = resolveClaudeExecutable(undefined, { PATH: bin });
  assert.equal(resolved.path, fs.realpathSync(FAKE_CLAUDE));
  assert.match(resolved.ino, /^\d+$/u);
  assert.throws(() => resolveClaudeExecutable("bin/claude", { PATH: bin }), /canonical absolute path/u);
  assert.throws(() => resolveClaudeExecutable(undefined, { PATH: "relative" }), /Could not resolve/u);
});

test("end-to-end launcher passes only the selected private profile to a fake Claude", (t) => {
  const temporary = makeTemporaryDirectory(t);
  const captureFile = path.join(temporary, "capture.json");
  execFileSync(process.execPath, [LAUNCHER, "--ceiling", "system", "--", "finish production readiness"], {
    cwd: PROJECT_ROOT,
    env: {
      ...process.env,
      CLAUDE_CODE_EXECUTABLE: FAKE_CLAUDE,
      CLAUDE_CODE_TEST_CAPTURE: captureFile,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const capture = JSON.parse(fs.readFileSync(captureFile, "utf8"));
  assert.equal(capture.cwd, PROJECT_ROOT);
  assert.equal(capture.ceiling, "system");
  assert.equal(capture.grant_key_bytes, 32);
  assert.ok(capture.argv.includes("--strict-mcp-config"));
  assert.equal(capture.argv[capture.argv.indexOf("--setting-sources") + 1], "");
  assert.ok(capture.argv.includes("--dangerously-skip-permissions"));
  assert.equal(capture.argv.at(-1), "finish production readiness");
  assert.deepEqual(Object.keys(capture.mcp_config.mcpServers), ["codex-local"]);
  assert.ok(capture.mcp_config.mcpServers["codex-local"].args.includes("system"));
  assert.ok(capture.mcp_config.mcpServers["codex-local"].args.includes("--grant-key-file"));
  assert.ok(capture.mcp_config.mcpServers["codex-local"].args[0].startsWith(`${path.dirname(capture.grant_key_file)}${path.sep}`));
  assert.notEqual(capture.mcp_config.mcpServers["codex-local"].args[0], path.join(PROJECT_ROOT, "tools", "claude-codex-broker", "server.mjs"));
  assert.equal(capture.hook_settings.hooks.PreToolUse[0].hooks[0].command, process.execPath);
  assert.equal(capture.hook_settings.hooks.PreToolUse[0].hooks[0].args.at(-1), "--issue-grant");
  assert.ok(capture.hook_settings.hooks.PreToolUse[0].hooks[0].args[0].startsWith(`${path.dirname(capture.grant_key_file)}${path.sep}`));
  assert.equal(fs.existsSync(path.dirname(capture.grant_key_file)), false);
});

test("launcher forwards SIGINT, SIGTERM, and SIGHUP, reaps Claude, and removes session artifacts", async (t) => {
  for (const [signal, expectedCode] of [["SIGINT", 130], ["SIGTERM", 143], ["SIGHUP", 129]]) {
    const temporary = makeTemporaryDirectory(t);
    const captureFile = path.join(temporary, `capture-${signal}.json`);
    const signalFile = path.join(temporary, `signal-${signal}.txt`);
    const grandchildSentinel = path.join(temporary, `grandchild-${signal}.json`);
    const child = spawn(process.execPath, [LAUNCHER, "--ceiling", "system"], {
      cwd: PROJECT_ROOT,
      env: {
        ...process.env,
        CLAUDE_CODE_EXECUTABLE: FAKE_CLAUDE,
        CLAUDE_CODE_TEST_CAPTURE: captureFile,
        CLAUDE_CODE_TEST_WAIT: "1",
        CLAUDE_CODE_TEST_SIGNAL_FILE: signalFile,
        CLAUDE_CODE_TEST_GRANDCHILD: process.platform === "win32" ? "0" : "1",
        CLAUDE_CODE_TEST_GRANDCHILD_SENTINEL: grandchildSentinel,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    await waitForFile(captureFile);
    const capture = JSON.parse(fs.readFileSync(captureFile, "utf8"));
    if (process.platform !== "win32") {
      await waitForFile(grandchildSentinel);
      assert.equal(JSON.parse(fs.readFileSync(grandchildSentinel, "utf8")).pid, capture.grandchild_pid);
    }
    assert.equal(child.kill(signal), true, signal);
    const result = await waitForExit(child);
    assert.equal(result.code, expectedCode, signal);
    assert.equal(result.signal, null, signal);
    assert.equal(fs.readFileSync(signalFile, "utf8"), `${signal}\n`, signal);
    assert.equal(fs.existsSync(path.dirname(capture.grant_key_file)), false, signal);
    await waitForPidGone(capture.pid);
    if (process.platform !== "win32") await waitForPidGone(capture.grandchild_pid);
  }
});

test("runtime integrity monitor terminates and reaps Claude before cleaning a changed snapshot", async (t) => {
  const temporary = makeTemporaryDirectory(t);
  const captureFile = path.join(temporary, "capture-integrity.json");
  const signalFile = path.join(temporary, "signal-integrity.txt");
  const grandchildSentinel = path.join(temporary, "grandchild-integrity.json");
  const child = spawn(process.execPath, [LAUNCHER, "--ceiling", "system"], {
    cwd: PROJECT_ROOT,
    env: {
      ...process.env,
      CLAUDE_CODE_EXECUTABLE: FAKE_CLAUDE,
      CLAUDE_CODE_TEST_CAPTURE: captureFile,
      CLAUDE_CODE_TEST_WAIT: "1",
      CLAUDE_CODE_TEST_SIGNAL_FILE: signalFile,
      CLAUDE_CODE_TEST_GRANDCHILD: process.platform === "win32" ? "0" : "1",
      CLAUDE_CODE_TEST_GRANDCHILD_SENTINEL: grandchildSentinel,
      CLAUDE_CODE_TEST_IGNORE_SIGNAL: "1",
      CLAUDE_CODE_TEST_GRANDCHILD_IGNORE_SIGNAL: "1",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  await waitForFile(captureFile);
  const capture = JSON.parse(fs.readFileSync(captureFile, "utf8"));
  if (process.platform !== "win32") {
    await waitForFile(grandchildSentinel);
    assert.equal(JSON.parse(fs.readFileSync(grandchildSentinel, "utf8")).pid, capture.grandchild_pid);
  }
  const frozenBroker = capture.mcp_config.mcpServers["codex-local"].args[0];
  fs.chmodSync(frozenBroker, 0o600);
  fs.appendFileSync(frozenBroker, "\n// integrity violation\n");
  const result = await waitForExit(child);
  assert.equal(result.code, 1);
  assert.equal(fs.readFileSync(signalFile, "utf8"), "SIGTERM\n");
  assert.equal(fs.existsSync(path.dirname(capture.grant_key_file)), false);
  await waitForPidGone(capture.pid);
  if (process.platform !== "win32") await waitForPidGone(capture.grandchild_pid);
});
