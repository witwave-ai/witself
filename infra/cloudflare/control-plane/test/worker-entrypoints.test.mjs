import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  sanitizedWranglerEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));

test("Worker bundle exports only its default handler and deployed classes", async () => {
  const temporary = await mkdtemp(join(tmpdir(), "witself-worker-entrypoints-"));
  try {
    const metafile = join(temporary, "bundle-meta.json");
    const result = spawnSync(process.execPath, [
      join(root, "node_modules", "wrangler", "bin", "wrangler.js"),
      ...withReviewedWranglerEnvironmentFile([
        "deploy",
        join(root, "src", "index.js"),
        "--dry-run",
        "--name", "witself-control-plane",
        "--compatibility-date", "2026-06-01",
        "--outdir", temporary,
        "--metafile", metafile,
      ]),
    ], {
      cwd: root,
      encoding: "utf8",
      env: sanitizedWranglerEnvironment({ PATH: process.env.PATH }),
      timeout: 60_000,
    });
    assert.equal(
      result.status,
      0,
      `Worker bundle failed: ${result.error?.message ?? result.stderr}`,
    );
    const metadata = JSON.parse(await readFile(metafile, "utf8"));
    const entrypoints = Object.values(metadata.outputs)
      .filter((output) => output.entryPoint !== undefined);
    assert.equal(entrypoints.length, 1, "Worker must have exactly one entry module");
    // These are the real bundle exports consumed by workerd. Exporting even a
    // scheduling utility here adds a named handler and breaks deployment checks.
    assert.deepEqual(entrypoints[0].exports.toSorted(), [
      "AccountBackup",
      "AccountLifecycle",
      "AccountSignup",
      "AgentEmailDomainRegistry",
      "Backend",
      "RealmEmailAliasRegistry",
      "TargetCellCoordinator",
      "default",
    ]);
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
});
