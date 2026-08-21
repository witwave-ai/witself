import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const fixture = fileURLToPath(new URL("../fixtures/fake-app-server.mjs", import.meta.url));

export function jwt(expSeconds = Math.floor(Date.now() / 1000) + 7200) {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");
  return `${encode({ alg: "none", typ: "JWT" })}.${encode({ exp: expSeconds })}.signature`;
}

export async function makeTemp(prefix = "claude-codex-test-") {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), prefix));
  await fs.chmod(root, 0o700);
  return fs.realpath(root);
}

export async function fakeRuntime(tempRoot, scenario = "success") {
  const projectRoot = path.join(tempRoot, "repo");
  const codexHome = path.join(tempRoot, "codex-home");
  const scratchRoot = path.join(tempRoot, "jobs");
  const gitRoot = path.join(tempRoot, "git-common");
  const deniedRoot = path.join(tempRoot, "denied");
  const deniedSentinel = path.join(deniedRoot, "broker-sentinel");
  const record = path.join(tempRoot, "record.jsonl");
  for (const directory of [projectRoot, codexHome, scratchRoot, gitRoot, deniedRoot]) await fs.mkdir(directory, { mode: 0o700 });
  await fs.writeFile(deniedSentinel, "broker secret\n", { mode: 0o600 });
  const accessToken = jwt();
  const runtime = {
    version: "9.8.7",
    integrity: "sha512-base",
    platformVersion: "9.8.7-fake",
    platformIntegrity: "sha512-platform",
    registry: "https://registry.npmjs.org/",
    latestVerificationPolicy: "before-every-new-work",
    latestVerifiedAt: 2_000_000_000_000,
    binary: "/never/executed/directly",
    projectRoot,
    codexHome,
    scratchRoot,
    gitReadRoots: [gitRoot],
    platformReadRoots: [],
    gitCommand: "/usr/bin/git",
    deniedSentinel,
    env: {
      HOME: path.join(tempRoot, "empty-home"),
      CODEX_HOME: codexHome,
      TMPDIR: path.join(tempRoot, "tmp"),
      PATH: "/usr/bin:/bin",
      FAKE_CODEX_SCENARIO: scenario,
      FAKE_CODEX_RECORD: record,
      FAKE_CODEX_VERSION: "9.8.7",
      FAKE_DENIED_SENTINEL: deniedSentinel,
    },
    async loadExternalAuth() { return { accessToken, accountId: "account-sensitive", expiresAt: Math.floor(Date.now() / 1000) + 7200, sourceHash: "unchanged" }; },
    async verifyAuthUnchanged(hash) { return hash === "unchanged"; },
    redact(value) { return String(value).split(accessToken).join("[REDACTED]").split("account-sensitive").join("[REDACTED]"); },
  };
  await fs.mkdir(runtime.env.HOME, { mode: 0o700 });
  await fs.mkdir(runtime.env.TMPDIR, { mode: 0o700 });
  const spawnCalls = [];
  const configSnapshots = [];
  const spawnImpl = (_binary, args, options) => {
    if (JSON.stringify(args) !== JSON.stringify(["app-server", "--stdio", "--strict-config"])) throw new Error("unexpected app-server args");
    spawnCalls.push({ args: [...args], options });
    configSnapshots.push(readFileSync(path.join(options.env.CODEX_HOME, "config.toml"), "utf8"));
    return spawn(process.execPath, [fixture], options);
  };
  return { runtime, spawnImpl, record, spawnCalls, configSnapshots };
}

export async function readRecord(file) {
  const text = await fs.readFile(file, "utf8");
  return text.trim().split("\n").filter(Boolean).map(JSON.parse);
}
