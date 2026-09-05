import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { assertExactConfig } from "../scripts/bundle-check.mjs";

const root = new URL("..", import.meta.url);
const renderer = new URL("../scripts/render-wrangler.mjs", import.meta.url);

function parseJSONC(raw) {
  return JSON.parse(raw.replace(/^\s*\/\/.*$/gm, ""));
}

function render(output, extra = {}) {
  const environment = { ...process.env };
  delete environment.SUPPORT_EMAIL_INTAKE_ENABLED;
  delete environment.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID;
  return spawnSync(process.execPath, [renderer.pathname, "--output", output], {
    cwd: root,
    env: {
      ...environment,
      CONTROL_PLANE_URL: "https://self.witwave.ai/",
      ...extra,
    },
    encoding: "utf8",
  });
}

test("rendered deployment stays dark, email-only, and release-identified", async () => {
  const directory = await mkdtemp(join(tmpdir(), "support-email-config-"));
  try {
    const output = join(directory, "wrangler.generated.jsonc");
    const rendered = render(output);
    assert.equal(rendered.status, 0, rendered.stderr);
    const config = parseJSONC(await readFile(output, "utf8"));
    assert.equal(assertExactConfig(config), true);
    assert.equal(config.vars.SUPPORT_EMAIL_INTAKE_ENABLED, "false");
    assert.equal(config.vars.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID, "");
    assert.equal(config.vars.CONTROL_PLANE_URL, "https://self.witwave.ai/");
    assert.match(config.vars.WITSELF_EDGE_RELEASE_VERSION,
      /^(?:\d+\.\d+\.\d+|development-[0-9a-f]{12}(?:-dirty)?)$/);
    assert.match(config.vars.WITSELF_EDGE_RELEASE_COMMIT, /^[0-9a-f]{40}$/);
    assert.match(config.vars.WITSELF_EDGE_RELEASE_DATE,
      /^\d{4}-\d{2}-\d{2}T/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("exact top-level inventory rejects every additional binding family", () => {
  const base = {
    $schema: "node_modules/wrangler/config-schema.json",
    name: "witself-support-email-intake",
    main: "src/index.js",
    secrets: { required: ["CONTROL_PLANE_SUPPORT_INTAKE_TOKEN"] },
    compatibility_date: "2026-07-21",
    compatibility_flags: ["global_fetch_strictly_public"],
    workers_dev: false,
    preview_urls: false,
    ratelimits: [
      { name: "SUPPORT_EMAIL_SENDER_LIMITER", namespace_id: "2401", simple: { limit: 10, period: 60 } },
      { name: "SUPPORT_EMAIL_GLOBAL_LIMITER", namespace_id: "2402", simple: { limit: 100, period: 60 } },
    ],
    vars: {
      SUPPORT_EMAIL_INTAKE_ENABLED: "false",
      SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "",
      CONTROL_PLANE_URL: "https://self.witwave.ai/",
      WITSELF_EDGE_RELEASE_VERSION: "development-aaaaaaaaaaaa-dirty",
      WITSELF_EDGE_RELEASE_COMMIT: "a".repeat(40),
      WITSELF_EDGE_RELEASE_DATE: "2026-08-24T00:00:00Z",
    },
    observability: { enabled: true },
  };
  assert.equal(assertExactConfig(base), true);
  for (const binding of [
    "ai", "analytics_engine_datasets", "browser", "containers",
    "d1_databases", "dispatch_namespaces", "durable_objects", "hyperdrive",
    "kv_namespaces", "mtls_certificates", "queues", "r2_buckets", "route",
    "routes", "send_email", "services", "unsafe", "vectorize",
  ]) {
    assert.throws(
      () => assertExactConfig({ ...base, [binding]: [] }),
      /configuration drifted/,
    );
  }
});

test("renderer accepts only exact gate values, trusted IDs, and public HTTPS origins", async () => {
  const directory = await mkdtemp(join(tmpdir(), "support-email-config-invalid-"));
  try {
    for (const [extra, error] of [
      [{ SUPPORT_EMAIL_INTAKE_ENABLED: "TRUE" }, /must be true or false/],
      [{ SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "bad;id" }, /without semicolons/],
      [{ CONTROL_PLANE_URL: "http://self.witwave.ai/" }, /public HTTPS origin/],
      [{ CONTROL_PLANE_URL: "https://user:pass@self.witwave.ai/" }, /public HTTPS origin/],
      [{ CONTROL_PLANE_URL: "https://self.witwave.ai/path" }, /public HTTPS origin/],
    ]) {
      const output = join(directory, `config-${Math.random()}.jsonc`);
      const rendered = render(output, extra);
      assert.notEqual(rendered.status, 0);
      assert.match(rendered.stderr, error);
    }
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
