import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import test from "node:test";

import { PLATFORM_TARGETS, REGISTRY } from "../lib/constants.mjs";
import { CodexRuntime, validateProjectRoot } from "../lib/runtime.mjs";
import { jwt, makeTemp } from "./helpers.mjs";

const execFileAsync = promisify(execFile);
const BASE_INTEGRITY = "sha512-YmFzZS1pbnRlZ3JpdHk=";
const PLATFORM_INTEGRITY = "sha512-cGxhdGZvcm0taW50ZWdyaXR5";

async function writeAuth(file, expiration = Math.floor(Date.now() / 1000) + 7200) {
  await fs.writeFile(file, `${JSON.stringify({ tokens: { access_token: jwt(expiration), account_id: "acct-test", refresh_token: "never-used" } })}\n`, { mode: 0o600 });
  await fs.chmod(file, 0o600);
}

async function makeHarness(options = {}) {
  const root = await makeTemp("claude-codex-runtime-");
  const repo = path.join(root, "repo");
  await fs.mkdir(repo, { mode: 0o700 });
  const auth = path.join(root, "auth.json");
  await writeAuth(auth, options.expiration);
  const target = PLATFORM_TARGETS[`${process.platform}:${process.arch}`];
  let latestVersion = options.version ?? "9.8.7";
  let installedVersion = latestVersion;
  let latestAvailable = true;
  let corruptLock = options.corruptLock ?? false;
  const calls = [];

  const runCommand = async (command, args, callOptions) => {
    calls.push({ command, args: [...args], env: { ...callOptions.env }, cwd: callOptions.cwd });
    if (args.includes("view")) {
      if (!latestAvailable) return { code: 1, stdout: "", stderr: "registry unavailable" };
      const spec = args.find((arg) => arg.startsWith("@openai/codex@"));
      if (spec === "@openai/codex@latest") {
        return { code: 0, stdout: JSON.stringify({ version: latestVersion, "dist.integrity": BASE_INTEGRITY }), stderr: "" };
      }
      return { code: 0, stdout: JSON.stringify({ version: `${latestVersion}-${target.suffix}`, "dist.integrity": PLATFORM_INTEGRITY }), stderr: "" };
    }
    if (args.includes("install") && args.includes("--package-lock-only")) {
      installedVersion = latestVersion;
      const packages = {
        "": { dependencies: { "@openai/codex": installedVersion } },
        "node_modules/@openai/codex": {
          version: installedVersion,
          resolved: `${REGISTRY}@openai/codex/-/codex-${installedVersion}.tgz`,
          integrity: corruptLock ? "sha512-d3Jvbmc=" : BASE_INTEGRITY,
        },
        [`node_modules/${target.alias}`]: {
          version: `${installedVersion}-${target.suffix}`,
          resolved: `${REGISTRY}@openai/codex/-/codex-${installedVersion}-${target.suffix}.tgz`,
          integrity: PLATFORM_INTEGRITY,
        },
      };
      await fs.writeFile(path.join(callOptions.cwd, "package-lock.json"), JSON.stringify({ lockfileVersion: 3, packages }), { mode: 0o600 });
      return { code: 0, stdout: "", stderr: "" };
    }
    if (args.includes("ci")) {
      const base = path.join(callOptions.cwd, "node_modules", "@openai", "codex");
      const platform = path.join(callOptions.cwd, "node_modules", ...target.alias.split("/"));
      const binary = path.join(platform, "vendor", target.triple, "bin", process.platform === "win32" ? "codex.exe" : "codex");
      await fs.mkdir(path.dirname(binary), { recursive: true, mode: 0o700 });
      await fs.mkdir(base, { recursive: true, mode: 0o700 });
      await fs.writeFile(path.join(base, "package.json"), JSON.stringify({
        name: "@openai/codex",
        version: installedVersion,
        bin: { codex: "bin/codex.js" },
        optionalDependencies: { [target.alias]: `npm:@openai/codex@${installedVersion}-${target.suffix}` },
      }), { mode: 0o600 });
      await fs.writeFile(path.join(platform, "package.json"), JSON.stringify({
        name: "@openai/codex", version: `${installedVersion}-${target.suffix}`, os: [process.platform], cpu: [process.arch],
      }), { mode: 0o600 });
      await fs.writeFile(binary, "fake binary\n", { mode: 0o700 });
      await fs.chmod(binary, 0o700);
      if (options.publishDuringInstall) latestVersion = "9.8.8";
      return { code: 0, stdout: "", stderr: "" };
    }
    if (args.length === 1 && args[0] === "--version") {
      return { code: 0, stdout: `codex-cli ${installedVersion}\n`, stderr: "" };
    }
    throw new Error(`unexpected command: ${command} ${args.join(" ")}`);
  };

  const runtime = new CodexRuntime({
    projectRoot: repo,
    tempRoot: root,
    authSource: auth,
    npmCommand: { command: "/trusted/node", prefix: ["/trusted/npm-cli.js"] },
    runCommand,
    clock: options.clock,
  });
  return {
    root, repo, auth, runtime, calls,
    setLatestVersion(value) { latestVersion = value; },
    setLatestAvailable(value) { latestAvailable = value; },
    setCorruptLock(value) { corruptLock = value; },
  };
}

test("installs one exact official package into a private session and validates lock, platform, binary, and flags", async (t) => {
  const harness = await makeHarness();
  const ready = await harness.runtime.prepareForNewWork();
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  assert.equal(ready.version, "9.8.7");
  assert.equal(ready.integrity, BASE_INTEGRITY);
  assert.equal(ready.platformIntegrity, PLATFORM_INTEGRITY);
  assert.equal(ready.latestVerificationPolicy, "before-every-new-work");
  assert.equal(Number.isSafeInteger(ready.latestVerifiedAt), true);
  assert.equal((await fs.stat(harness.runtime.sessionDir)).mode & 0o077, 0);
  await assert.rejects(fs.lstat(path.join(ready.codexHome, "auth.json")), { code: "ENOENT" });
  const npmCalls = harness.calls.filter(({ args }) => args.includes("view") || args.includes("install") || args.includes("ci"));
  assert.equal(npmCalls.every(({ args }) => args.some((arg) => arg === `--registry=${REGISTRY}`)), true);
  assert.equal(npmCalls.every(({ args, env }) => args.includes("--prefer-online") && env.npm_config_prefer_online === "true"), true);
  assert.equal(npmCalls.filter(({ args }) => args.includes("install") || args.includes("ci")).every(({ args }) => args.includes("--ignore-scripts")), true);
  assert.equal(npmCalls.every(({ env }) => !Object.hasOwn(env, "OPENAI_API_KEY") && !Object.hasOwn(env, "NODE_OPTIONS")), true);
});

test("freezes runtime, reverifies latest before every new work item, and latches restart on change", async (t) => {
  let now = Date.now();
  const harness = await makeHarness({ clock: () => now });
  await harness.runtime.prepareForNewWork();
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  const initialViews = harness.calls.filter(({ args }) => args.includes("view")).length;
  now += 1;
  await harness.runtime.prepareForNewWork();
  assert.equal(harness.calls.filter(({ args }) => args.includes("view")).length, initialViews + 2);
  now += 1;
  await harness.runtime.prepareForNewWork();
  assert.equal(harness.calls.filter(({ args }) => args.includes("view")).length, initialViews + 4);
  now += 1;
  harness.setLatestVersion("9.8.8");
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_update_available");
  harness.setLatestVersion("9.8.7");
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_restart_required");
});

test("latest metadata verification failure refuses and permanently latches all new work", async (t) => {
  const harness = await makeHarness();
  await harness.runtime.prepareForNewWork();
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  harness.setLatestAvailable(false);
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_restart_required");
  const callsAfterFailure = harness.calls.length;
  harness.setLatestAvailable(true);
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_restart_required");
  assert.equal(harness.calls.length, callsAfterFailure);
});

test("a release published during installation fails the first work item before any runtime use", async (t) => {
  const harness = await makeHarness({ publishDuringInstall: true });
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_update_available");
  const callsAfterMismatch = harness.calls.length;
  harness.setLatestVersion("9.8.7");
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "runtime_restart_required");
  assert.equal(harness.calls.length, callsAfterMismatch);
});

test("never falls back when package-lock integrity is wrong", async (t) => {
  const harness = await makeHarness({ corruptLock: true });
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  await assert.rejects(harness.runtime.prepareForNewWork(), (error) => error?.code === "package_lock_invalid");
  assert.equal(harness.calls.some(({ args }) => args.includes("ci")), false);
});

test("external auth uses only a sufficiently-lived access JWT and detects any source mutation", async (t) => {
  const harness = await makeHarness();
  await harness.runtime.prepareForNewWork();
  t.after(async () => { await harness.runtime.cleanup(); await fs.rm(harness.root, { recursive: true, force: true }); });
  const auth = await harness.runtime.loadExternalAuth(35 * 60 * 1000);
  assert.equal(typeof auth.accessToken, "string");
  assert.equal(auth.accountId, "acct-test");
  assert.equal(await harness.runtime.verifyAuthUnchanged(auth.sourceHash), true);
  await writeAuth(harness.auth, Math.floor(Date.now() / 1000) + 100);
  await assert.rejects(harness.runtime.verifyAuthUnchanged(auth.sourceHash), (error) => error?.code === "auth_changed");
  await assert.rejects(harness.runtime.loadExternalAuth(35 * 60 * 1000), (error) => error?.code === "auth_ttl_insufficient");
});

test("project root validation rejects aliases and nested paths", async (t) => {
  const root = await makeTemp("claude-codex-git-root-");
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const repo = path.join(root, "repo");
  const nested = path.join(repo, "nested");
  const alias = path.join(root, "alias");
  await fs.mkdir(nested, { recursive: true });
  await execFileAsync("/usr/bin/git", ["init", "-q", repo]);
  await fs.symlink(repo, alias);
  assert.equal(await validateProjectRoot(repo), repo);
  await assert.rejects(validateProjectRoot(nested), (error) => error?.code === "not_git_root");
  await assert.rejects(validateProjectRoot(alias), (error) => error?.code === "invalid_project_root");
});
