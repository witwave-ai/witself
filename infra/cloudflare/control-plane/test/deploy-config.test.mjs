import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

import {
  deploymentMatches,
  expectedBuildMetadata,
} from "../scripts/verify-deployment.mjs";

const root = new URL("..", import.meta.url);
const renderer = new URL("../scripts/render-wrangler.mjs", import.meta.url);
const version = "1.2.3";
const commit = "a".repeat(40);
const date = "2026-07-23T01:02:03Z";

test("release renderer injects immutable container build identity", async (t) => {
  const temp = await mkdtemp(join(tmpdir(), "witself-cp-config-"));
  t.after(() => rm(temp, { recursive: true, force: true }));
  const output = join(temp, "wrangler.jsonc");
  const rendered = spawnSync(process.execPath, [
    renderer.pathname,
    "--version", version,
    "--commit", commit,
    "--date", date,
    "--output", output,
  ], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(rendered.status, 0, rendered.stderr);

  const config = await readFile(output, "utf8");
  assert.deepEqual(expectedBuildMetadata(config), {
    service: "witself-control-plane",
    version,
    commit,
    date,
  });
  assert.doesNotMatch(config, /__WITSELF_[A-Z_]+__/);
  assert.equal((await stat(output)).mode & 0o777, 0o600);
});

test("release renderer requires all explicit identity fields", () => {
  const rejected = spawnSync(process.execPath, [
    renderer.pathname,
    "--version", version,
  ], {
    cwd: root,
    encoding: "utf8",
  });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /must be supplied together/);
});

test("deployment verification compares every identity field", () => {
  const expected = {
    service: "witself-control-plane",
    version,
    commit,
    date,
  };
  assert.equal(deploymentMatches({ ...expected }, expected), true);
  assert.equal(deploymentMatches({ ...expected, commit: "b".repeat(40) }, expected), false);
  assert.equal(deploymentMatches({ ...expected, version: "1.2.4" }, expected), false);
  assert.equal(deploymentMatches({ ...expected, date: "2026-07-23T01:02:04Z" }, expected), false);
});
