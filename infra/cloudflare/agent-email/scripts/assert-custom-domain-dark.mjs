#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "./wrangler-environment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
export const CUSTOM_DOMAIN_DELIVERY_SECRET =
  "AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED";
export const LEGACY_PILOT_TRUST_SECRET_NAMES = Object.freeze([
  "LEGACY_PILOT_TRUSTED_INGEST_URL",
  "LEGACY_PILOT_TRUSTED_CELL_AUDIENCE",
]);

export function assertCustomDomainDeliveryDark(secrets) {
  if (!Array.isArray(secrets)) {
    throw new Error("Worker secret inventory must be a JSON array");
  }
  for (const secret of secrets) {
    if (secret == null || typeof secret !== "object" ||
        typeof secret.name !== "string" || secret.name.trim() !== secret.name ||
        secret.name === "") {
      throw new Error("Worker secret inventory contains an invalid entry");
    }
    if (secret.name === CUSTOM_DOMAIN_DELIVERY_SECRET) {
      throw new Error(
        "dark custom-domain delivery deployment refused: activation secret present",
      );
    }
    if (LEGACY_PILOT_TRUST_SECRET_NAMES.includes(secret.name)) {
      throw new Error(
        `production receive deployment refused: retired pilot trust secret ${secret.name} present`,
      );
    }
  }
}

export function customDomainDarkInspectionEnvironment(env = process.env) {
  assertProductionCloudflareIdentity(env);
  return sanitizedWranglerInspectionEnvironment(env);
}

export function customDomainDarkSecretListArguments(config) {
  return withReviewedWranglerEnvironmentFile([
    "secret", "list", "--config", config, "--format", "json",
  ]);
}

function parseArgs(argv) {
  let config = join(root, "wrangler.generated.jsonc");
  for (let index = 0; index < argv.length; index += 1) {
    if (argv[index] !== "--config" || index + 1 >= argv.length) {
      throw new Error(
        `unknown or incomplete argument ${argv[index] ?? ""}`.trim(),
      );
    }
    config = resolve(root, argv[++index]);
  }
  return { config };
}

function main() {
  const { config } = parseArgs(process.argv.slice(2));
  const listed = spawnSync(
    "wrangler",
    customDomainDarkSecretListArguments(config),
    {
      cwd: root,
      encoding: "utf8",
      env: customDomainDarkInspectionEnvironment(process.env),
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  if (listed.error || listed.status !== 0) {
    throw new Error("could not verify persistent Worker secret names");
  }
  let secrets;
  try {
    secrets = JSON.parse(listed.stdout);
  } catch {
    throw new Error("Worker secret inventory was not valid JSON");
  }
  assertCustomDomainDeliveryDark(secrets);
  process.stdout.write(
    "verified custom-domain delivery activation secret is absent\n",
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
