#!/usr/bin/env node
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { sourceIdentity } from "./source-identity.mjs";

export function main() {
  const identity = sourceIdentity({ requireRelease: true });
  process.stdout.write(
    `verified clean email Worker release source v${identity.version} at ${identity.commit}\n`,
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
