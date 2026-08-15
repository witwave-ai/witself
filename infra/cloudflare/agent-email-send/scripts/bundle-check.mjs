#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = await mkdtemp(join(tmpdir(), "witself-agent-email-send-"));

try {
  const result = spawnSync("wrangler", [
    "deploy",
    "--dry-run",
    "--config", join(root, "wrangler.template.jsonc"),
    "--outdir", temporary,
  ], {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    timeout: 60_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error("outbound email adapter did not produce a valid Worker bundle");
  }
  const files = await readdir(temporary);
  if (!files.includes("index.js")) {
    throw new Error("outbound email adapter bundle omitted its entrypoint");
  }
  process.stdout.write("outbound email adapter bundle verified\n");
} finally {
  await rm(temporary, { recursive: true, force: true });
}
