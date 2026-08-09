#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  releaseMessage,
  verifyProduction,
} from "./deployment-identity.mjs";
import { sourceIdentity } from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const generatedConfig = join(root, "wrangler.generated.jsonc");

function run(command, args) {
  const result = spawnSync(command, args, {
    cwd: root,
    encoding: "utf8",
    stdio: "inherit",
  });
  if (result.error || result.status !== 0) {
    throw new Error(`email Worker deployment step failed: ${command}`);
  }
}

export function main() {
  const release = sourceIdentity({ requireRelease: true });
  run(process.execPath, [join(root, "scripts", "render-wrangler.mjs")]);
  run(process.execPath, [
    join(root, "scripts", "assert-custom-domain-dark.mjs"),
    "--config", generatedConfig,
  ]);
  run("wrangler", [
    "deploy",
    "--config", generatedConfig,
    "--strict",
    "--tag", release.tag,
    "--message", releaseMessage(release),
  ]);
  run(process.execPath, [
    join(root, "scripts", "assert-custom-domain-dark.mjs"),
    "--config", generatedConfig,
  ]);
  const attestation = verifyProduction({ requireAnnotations: true });
  process.stdout.write(`${JSON.stringify(attestation, null, 2)}\n`);
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
