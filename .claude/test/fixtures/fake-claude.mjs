#!/usr/bin/env node
import fs from "node:fs";
import process from "node:process";
import { spawn } from "node:child_process";

if (process.argv.includes("--test-grandchild")) {
  const sentinelFile = process.env.CLAUDE_CODE_TEST_GRANDCHILD_SENTINEL;
  if (!sentinelFile) throw new Error("CLAUDE_CODE_TEST_GRANDCHILD_SENTINEL is required.");
  fs.writeFileSync(sentinelFile, JSON.stringify({ pid: process.pid, ppid: process.ppid }), { mode: 0o600 });
  if (process.env.CLAUDE_CODE_TEST_GRANDCHILD_IGNORE_SIGNAL === "1") {
    for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(signal, () => {});
  }
  setInterval(() => {}, 1_000);
} else {

if (process.argv.includes("--version")) {
  process.stdout.write("2.1.224 (Claude Code)\n");
  process.exit(0);
}

if (process.argv.includes("--help")) {
  process.stdout.write([
    "--mcp-config",
    "--strict-mcp-config",
    "--permission-mode",
    "--dangerously-skip-permissions",
    "--setting-sources",
    "--settings",
  ].join("\n"));
  process.exit(0);
}

const captureFile = process.env.CLAUDE_CODE_TEST_CAPTURE;
if (!captureFile) throw new Error("CLAUDE_CODE_TEST_CAPTURE is required.");

function valueAfter(flag) {
  const index = process.argv.indexOf(flag);
  if (index < 0 || !process.argv[index + 1]) throw new Error(`Missing ${flag}.`);
  return process.argv[index + 1];
}

const configFile = valueAfter("--mcp-config");
const settingsFile = valueAfter("--settings");
const grantKeyFile = process.env.WITSELF_CODEX_GRANT_KEY_FILE ?? null;
let grandchild = null;
if (process.env.CLAUDE_CODE_TEST_WAIT === "1" && process.env.CLAUDE_CODE_TEST_GRANDCHILD === "1") {
  grandchild = spawn(process.execPath, [process.argv[1], "--test-grandchild"], {
    env: process.env,
    stdio: "ignore",
    shell: false,
  });
}
fs.writeFileSync(captureFile, JSON.stringify({
  pid: process.pid,
  grandchild_pid: grandchild?.pid ?? null,
  argv: process.argv.slice(2),
  cwd: process.cwd(),
  ceiling: process.env.WITSELF_CODEX_CEILING ?? null,
  grant_key_file: grantKeyFile,
  grant_key_bytes: grantKeyFile ? fs.statSync(grantKeyFile).size : null,
  mcp_config: JSON.parse(fs.readFileSync(configFile, "utf8")),
  hook_settings: JSON.parse(fs.readFileSync(settingsFile, "utf8")),
}), { mode: 0o600 });

if (process.env.CLAUDE_CODE_TEST_WAIT === "1") {
  const signalFile = process.env.CLAUDE_CODE_TEST_SIGNAL_FILE;
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(signal, () => {
      if (signalFile) fs.writeFileSync(signalFile, `${signal}\n`, { mode: 0o600 });
      if (process.env.CLAUDE_CODE_TEST_IGNORE_SIGNAL !== "1") setTimeout(() => process.exit(0), 25);
    });
  }
  setInterval(() => {}, 1_000);
}
}
