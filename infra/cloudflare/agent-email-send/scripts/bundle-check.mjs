#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = await mkdtemp(join(tmpdir(), "witself-agent-email-send-"));

try {
  const config = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
  for (const requiredDefault of [
    '"RECEIPT_REPLAY_AUDIENCE": "witself-agent-email-send-receipt-replay"',
    '"RECEIPT_REPLAY_ENABLED": "false"',
  ]) {
    if (!config.includes(requiredDefault)) {
      throw new Error(`outbound email adapter config omitted ${requiredDefault}`);
    }
  }
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
  const bundle = await readFile(join(temporary, "index.js"), "utf8");
  for (const marker of [
    "/v1/dispatch:receipt-replay",
    "witself-agent-email-send-receipt-replay",
    "witself.agent-email-dispatch-receipt-proof.v1",
    "RECEIPT_REPLAY_ENABLED",
    "provider_call_started_count",
    "verified_replay_count",
    "witself.agent-email-provider-event-consume-log.v1",
    "target_account_unmapped",
    "target_signer_unauthorized",
  ]) {
    if (!bundle.includes(marker)) {
      throw new Error(`outbound email adapter bundle omitted ${marker}`);
    }
  }
  process.stdout.write("outbound email adapter bundle verified\n");
} finally {
  await rm(temporary, { recursive: true, force: true });
}
