#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  deploymentMatches,
  expectedBuildMetadata,
} from "./verify-deployment.mjs";
import {
  sourceIdentity,
  workerVersionMessage,
  workerVersionTag,
} from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
export const GENERATED_CONFIG_PATH = join(root, "wrangler.generated.jsonc");

function parseArgs(argv) {
  if (argv.length === 0) return { config: GENERATED_CONFIG_PATH };
  if (argv.length !== 2 || argv[0] !== "--config") {
    throw new Error(`unknown or incomplete argument ${argv[0] ?? ""}`.trim());
  }
  return { config: exactGeneratedConfigPath(argv[1]) };
}

export function exactGeneratedConfigPath(config = GENERATED_CONFIG_PATH) {
  const candidate = resolve(root, config);
  if (candidate !== GENERATED_CONFIG_PATH) {
    throw new Error("release deployment requires the exact generated control-plane config");
  }
  return GENERATED_CONFIG_PATH;
}

export function releaseDeploymentArguments(
  metadata,
  config = GENERATED_CONFIG_PATH,
) {
  const exactConfig = exactGeneratedConfigPath(config);
  return [
    "deploy",
    "--config", exactConfig,
    "--strict",
    "--tag", workerVersionTag(metadata),
    "--message", workerVersionMessage(metadata),
  ];
}

export async function main(argv = process.argv.slice(2)) {
  const { config } = parseArgs(argv);
  const expected = expectedBuildMetadata(await readFile(config, "utf8"));
  const source = sourceIdentity();
  const actual = { service: "witself-control-plane", ...source };
  if (!deploymentMatches(actual, expected)) {
    throw new Error(
      "generated control-plane config does not match the clean tagged release source",
    );
  }

  const deployed = spawnSync(
    "wrangler",
    releaseDeploymentArguments(source, config),
    { cwd: root, stdio: "inherit" },
  );
  if (deployed.error || deployed.status !== 0) {
    throw new Error("control-plane Worker deployment failed");
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
