#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

export const CUSTOM_DOMAIN_DARK_SECRET_NAMES = Object.freeze([
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED",
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST",
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY",
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED",
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED",
  "CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST",
]);

export const CANONICAL_EMAIL_DARK_SECRET_NAMES = Object.freeze([
  "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED",
  "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
]);

export const EMAIL_DARK_SECRET_NAMES = Object.freeze([
  ...CANONICAL_EMAIL_DARK_SECRET_NAMES,
  ...CUSTOM_DOMAIN_DARK_SECRET_NAMES,
]);

export function assertCustomDomainSecretsDark(secrets) {
  if (!Array.isArray(secrets)) {
    throw new Error("Worker secret inventory must be a JSON array");
  }
  const names = new Set();
  for (const secret of secrets) {
    if (secret == null || typeof secret !== "object" ||
        typeof secret.name !== "string" || secret.name.trim() !== secret.name ||
        secret.name === "") {
      throw new Error("Worker secret inventory contains an invalid entry");
    }
    names.add(secret.name);
  }
  const present = EMAIL_DARK_SECRET_NAMES.filter((name) => names.has(name));
  if (present.length !== 0) {
    throw new Error(
      `dark agent-email deployment refused: activation secret present (${present.join(", ")})`,
    );
  }
}

export function inspectWorkerSecrets(
  config,
  environment = process.env,
  run = spawnSync,
  {
    cwd = root,
    reviewedEnvironmentFile,
  } = {},
) {
  assertProductionCloudflareIdentity(environment);
  const listed = run("wrangler", withReviewedWranglerEnvironmentFile(
    ["secret", "list", "--config", config, "--format", "json"],
    reviewedEnvironmentFile,
  ), {
    cwd,
    env: sanitizedWranglerInspectionEnvironment(environment),
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (listed.error || listed.status !== 0) {
    throw new Error("could not verify persistent Worker secret names");
  }
  try {
    return JSON.parse(listed.stdout);
  } catch {
    throw new Error("Worker secret inventory was not valid JSON");
  }
}

function parseArgs(argv) {
  let config = join(root, "wrangler.generated.jsonc");
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] !== "--config" || i + 1 >= argv.length) {
      throw new Error(`unknown or incomplete argument ${argv[i] ?? ""}`.trim());
    }
    const value = argv[++i];
    config = resolve(root, value);
  }
  return { config };
}

function main() {
  const { config } = parseArgs(process.argv.slice(2));
  assertCustomDomainSecretsDark(inspectWorkerSecrets(config));
  process.stdout.write(
    "verified canonical and custom-domain activation secrets are absent\n",
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
