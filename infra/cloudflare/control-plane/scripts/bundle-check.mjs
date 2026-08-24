#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  sanitizedWranglerEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = await mkdtemp(join(tmpdir(), "witself-control-plane-bundle-"));

try {
  const result = spawnSync("wrangler", withReviewedWranglerEnvironmentFile([
    "deploy",
    join(root, "src", "index.js"),
    "--dry-run",
    "--name", "witself-control-plane",
    "--compatibility-date", "2026-06-01",
    "--outdir", temporary,
  ]), {
    cwd: root,
    encoding: "utf8",
    env: sanitizedWranglerEnvironment(process.env),
    stdio: ["ignore", "pipe", "pipe"],
    timeout: 60_000,
  });
  if (result.error || result.status !== 0) {
    const detail = result.error?.message ?? String(result.stderr ?? "")
      .trim()
      .slice(-2000);
    throw new Error(
      `control-plane Worker did not produce a valid bundle${
        detail ? `: ${detail}` : ""
      }`,
    );
  }
  const files = (await readdir(temporary)).sort();
  if (JSON.stringify(files) !==
      JSON.stringify(["README.md", "index.js", "index.js.map"])) {
    throw new Error("control-plane Worker bundle inventory was unexpected");
  }
  const bundle = await readFile(join(temporary, "index.js"), "utf8");
  for (const marker of [
    "/v1/intake/support-email",
    "/v1/support/admin:match-contact",
    "CP_SUPPORT_EMAIL_INTAKE_ENABLED",
    "SUPPORT_EMAIL_INTAKE_TOKEN",
    "intake_dedup:",
    "drop_fanout_error",
    "open-email-ticket",
    "reply-email-ticket",
  ]) {
    if (!bundle.includes(marker)) {
      throw new Error(`control-plane Worker bundle omitted ${marker}`);
    }
  }
  process.stdout.write("control-plane Worker bundle verified\n");
} finally {
  await rm(temporary, { recursive: true, force: true });
}
