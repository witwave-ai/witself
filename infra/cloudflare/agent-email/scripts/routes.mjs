#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import {
  disablePilot,
  inspectPilot,
  LEGACY_ROUTE_ACTIVATION_RETIRED,
  removePilot,
} from "./routing-lib.mjs";

const operations = new Map([
  ["disable", disablePilot],
  ["remove", removePilot],
  ["status", inspectPilot],
]);
const retiredOperations = new Set(["prepare", "activate"]);

function usage() {
  return `usage: node scripts/routes.mjs <disable|remove|status> <pilot.json>

Required environment:
  CLOUDFLARE_API_TOKEN    Zone Settings Read, Email Routing Rules Write,
                          plus Workers KV read/write
  CLOUDFLARE_ACCOUNT_ID   32-character Cloudflare account id
  CLOUDFLARE_ZONE_ID      32-character Email Routing zone id
  EMAIL_DIRECTORY_KV_ID   isolated witself-agent-email-pilot-directory id

This legacy literal-route manager is cleanup-only. Prepare and activate are
retired; use the production primary-route manager. Status reports the live
subaddressing setting. Disable and remove read the catch-all before and after
every operation, and no operation can update catch_all.`;
}

async function main(argv = process.argv.slice(2)) {
  if (retiredOperations.has(argv[0])) {
    throw new Error(LEGACY_ROUTE_ACTIVATION_RETIRED);
  }
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
