import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = new URL("..", import.meta.url);
const script = new URL("../scripts/render-wrangler.mjs", import.meta.url);
const controlPlaneURL = "https://self.witwave.ai/";

test("deployment config is email-only and cannot reuse the control-plane DIRECTORY id", async () => {
  const controlPlane = await readFile(new URL("../../control-plane/wrangler.template.jsonc", import.meta.url), "utf8");
  const directoryID = /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/.exec(controlPlane)?.[1];
  assert.match(directoryID, /^[0-9a-f]{32}$/);
  const rejected = spawnSync(process.execPath, [script.pathname], {
    cwd: root,
    env: {
      ...process.env,
      EMAIL_DIRECTORY_KV_ID: directoryID,
      RELAY_KEY_ID: "pilot-2026-07",
      CONTROL_PLANE_URL: controlPlaneURL,
    },
    encoding: "utf8",
  });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /must not reuse the control-plane DIRECTORY namespace/);

  const isolatedID = directoryID === "a".repeat(32) ? "b".repeat(32) : "a".repeat(32);
  const rendered = spawnSync(process.execPath, [script.pathname], {
    cwd: root,
    env: {
      ...process.env,
      EMAIL_DIRECTORY_KV_ID: isolatedID,
      RELAY_KEY_ID: "pilot-2026-07",
      CONTROL_PLANE_URL: controlPlaneURL,
    },
    encoding: "utf8",
  });
  assert.equal(rendered.status, 0, rendered.stderr);
  const config = await readFile(new URL("../wrangler.generated.jsonc", import.meta.url), "utf8");
  assert.match(config, /"workers_dev"\s*:\s*false/);
  assert.match(config, /"preview_urls"\s*:\s*false/);
  assert.match(config, /"compatibility_flags"\s*:\s*\["global_fetch_strictly_public"\]/);
  assert.match(config, /"binding"\s*:\s*"EMAIL_DIRECTORY"/);
  assert.match(config, /"binding"\s*:\s*"EMAIL_EDGE_METRICS"/);
  assert.match(config, /"dataset"\s*:\s*"witself_agent_email_edge"/);
  assert.match(config, /"name"\s*:\s*"REALM_ROUTE_COLD_MISS_LIMITER"/);
  assert.match(config, /"namespace_id"\s*:\s*"2201"/);
  assert.match(config, /"limit"\s*:\s*10[\s\S]{0,60}"period"\s*:\s*10/);
  assert.match(config, /"name"\s*:\s*"REALM_ROUTE_KNOWN_MISS_LIMITER"/);
  assert.match(config, /"namespace_id"\s*:\s*"2202"/);
  assert.match(config, /"limit"\s*:\s*100[\s\S]{0,60}"period"\s*:\s*10/);
  assert.match(config, /"CONTROL_PLANE_URL"\s*:\s*"https:\/\/self\.witwave\.ai\/"/);
  assert.match(
    config,
    /"REALM_EMAIL_ALIAS_DELIVERY_ENABLED"\s*:\s*"false"/,
  );
  assert.doesNotMatch(config, /CONTROL_PLANE_EDGE_TOKEN/);
  assert.doesNotMatch(config, /"binding"\s*:\s*"DIRECTORY"/);
  assert.doesNotMatch(config, /"routes"\s*:/);
});

test("deployment config accepts only explicit boolean alias gate values", () => {
  for (const value of ["TRUE", "1", "yes", " false "]) {
    const rendered = spawnSync(process.execPath, [script.pathname], {
      cwd: root,
      env: {
        ...process.env,
        EMAIL_DIRECTORY_KV_ID: "a".repeat(32),
        RELAY_KEY_ID: "pilot-2026-07",
        CONTROL_PLANE_URL: controlPlaneURL,
        REALM_EMAIL_ALIAS_DELIVERY_ENABLED: value,
      },
      encoding: "utf8",
    });
    assert.notEqual(rendered.status, 0, value);
    assert.match(
      rendered.stderr,
      /REALM_EMAIL_ALIAS_DELIVERY_ENABLED must be true or false/,
    );
  }
});

test("deployment config requires one credential-free public HTTPS control-plane origin", () => {
  for (const value of [
    "",
    "http://self.witwave.ai/",
    "https://user:pass@self.witwave.ai/",
    "https://localhost/",
    "https://self.witwave.ai/path",
    "https://self.witwave.ai/?token=secret",
  ]) {
    const rendered = spawnSync(process.execPath, [script.pathname], {
      cwd: root,
      env: {
        ...process.env,
        EMAIL_DIRECTORY_KV_ID: "a".repeat(32),
        RELAY_KEY_ID: "pilot-2026-07",
        CONTROL_PLANE_URL: value,
      },
      encoding: "utf8",
    });
    assert.notEqual(rendered.status, 0, value);
    assert.match(rendered.stderr, /CONTROL_PLANE_URL must be a credential-free public HTTPS origin/);
  }
});
