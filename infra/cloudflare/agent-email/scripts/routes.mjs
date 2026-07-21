#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import { activatePilot, disablePilot, inspectPilot, preparePilot, removePilot } from "./routing-lib.mjs";

const operations = new Map([
  ["prepare", preparePilot],
  ["activate", activatePilot],
  ["disable", disablePilot],
  ["remove", removePilot],
  ["status", inspectPilot],
]);

function usage() {
  return `usage: node scripts/routes.mjs <prepare|activate|disable|remove|status> <pilot.json>

Required environment:
  CLOUDFLARE_API_TOKEN    Email Routing Edit plus Workers KV read/write
  CLOUDFLARE_ACCOUNT_ID   32-character Cloudflare account id
  CLOUDFLARE_ZONE_ID      32-character Email Routing zone id
  EMAIL_DIRECTORY_KV_ID   isolated witself-agent-email-pilot-directory id

The manager uses literal-address routes only. It reads the catch-all before and
after every operation but contains no operation capable of updating catch_all.`;
}

async function main(argv = process.argv.slice(2)) {
  if (argv.length !== 2 || !operations.has(argv[0])) throw new Error(usage());
  let manifest;
  try {
    manifest = JSON.parse(await readFile(argv[1], "utf8"));
  } catch {
    throw new Error("pilot manifest is missing or invalid JSON");
  }
  const config = cloudflareEnvironment();
  if (!config.zoneID || !config.namespaceID) throw new Error("CLOUDFLARE_ZONE_ID and EMAIL_DIRECTORY_KV_ID are required");
  const result = await operations.get(argv[0])(new CloudflareAPI(config), manifest);
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
