import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = new URL("..", import.meta.url);
const script = new URL("../scripts/render-wrangler.mjs", import.meta.url);

test("deployment config is email-only and cannot reuse the control-plane DIRECTORY id", async () => {
  const controlPlane = await readFile(new URL("../../control-plane/wrangler.jsonc", import.meta.url), "utf8");
  const directoryID = /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/.exec(controlPlane)?.[1];
  assert.match(directoryID, /^[0-9a-f]{32}$/);
  const rejected = spawnSync(process.execPath, [script.pathname], {
    cwd: root,
    env: { ...process.env, EMAIL_DIRECTORY_KV_ID: directoryID, RELAY_KEY_ID: "pilot-2026-07" },
    encoding: "utf8",
  });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /must not reuse the control-plane DIRECTORY namespace/);

  const isolatedID = directoryID === "a".repeat(32) ? "b".repeat(32) : "a".repeat(32);
  const rendered = spawnSync(process.execPath, [script.pathname], {
    cwd: root,
    env: { ...process.env, EMAIL_DIRECTORY_KV_ID: isolatedID, RELAY_KEY_ID: "pilot-2026-07" },
    encoding: "utf8",
  });
  assert.equal(rendered.status, 0, rendered.stderr);
  const config = await readFile(new URL("../wrangler.generated.jsonc", import.meta.url), "utf8");
  assert.match(config, /"workers_dev"\s*:\s*false/);
  assert.match(config, /"preview_urls"\s*:\s*false/);
  assert.match(config, /"binding"\s*:\s*"EMAIL_DIRECTORY"/);
  assert.doesNotMatch(config, /"binding"\s*:\s*"DIRECTORY"/);
  assert.doesNotMatch(config, /"routes"\s*:/);
});
