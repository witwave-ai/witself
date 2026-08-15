import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { devNull, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
  WRANGLER_PRODUCTION_ENV_FILE,
  assertProductionCloudflareIdentity,
  sanitizedWranglerInspectionEnvironment,
  withReviewedWranglerEnvironmentFile,
} from "../scripts/wrangler-environment.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const wranglerEntrypoint = join(
  root,
  "node_modules",
  "wrangler",
  "bin",
  "wrangler.js",
);

test("production Wrangler identity requires the exact canonical provider account", () => {
  assert.deepEqual(assertProductionCloudflareIdentity({
    CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_API_TOKEN: "canonical-production-token",
    CF_ACCOUNT_ID: "another-account",
    CF_API_TOKEN: "deprecated-token",
    CLOUDFLARE_PROFILE: "another-profile",
  }), {
    account_id: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    api_token_present: true,
  });

  for (const environment of [
    {},
    {
      CF_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      CF_API_TOKEN: "deprecated-token",
    },
    {
      CLOUDFLARE_ACCOUNT_ID: "6236aa0c39cdd8d171deab7f86a12bc5",
      CLOUDFLARE_API_TOKEN: "canonical-production-token",
    },
    {
      CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      CLOUDFLARE_PROFILE: "production",
    },
    {
      CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      CLOUDFLARE_API_TOKEN: " token-with-whitespace ",
    },
  ]) {
    assert.throws(
      () => assertProductionCloudflareIdentity(environment),
      /CLOUDFLARE_(?:ACCOUNT_ID|API_TOKEN)/,
    );
  }
});

test("production Wrangler arguments select only the reviewed empty dotenv file", () => {
  const original = ["deployments", "status", "--name", "worker", "--json"];
  const guarded = withReviewedWranglerEnvironmentFile(original);
  assert.deepEqual(original, [
    "deployments", "status", "--name", "worker", "--json",
  ]);
  assert.deepEqual(guarded.slice(-2), [
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
  assert.equal(WRANGLER_PRODUCTION_ENV_FILE, devNull);
  assert.equal(readFileSync(WRANGLER_PRODUCTION_ENV_FILE, "utf8"), "");
  for (const args of [
    ["--env-file", "/tmp/unsafe", "deployments", "status"],
    ["deployments", "status", "--env-file=/tmp/unsafe"],
  ]) {
    assert.throws(
      () => withReviewedWranglerEnvironmentFile(args),
      /unreviewed environment file/,
    );
  }
});

test("a custom Wrangler env file must contain only the frozen reviewed marker", () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-wrangler-reviewed-env-"));
  const reviewed = join(directory, "reviewed.env");
  try {
    writeFileSync(
      reviewed,
      "# Intentionally empty: production Wrangler commands must not load local dotenv files.\n",
      { flag: "wx", mode: 0o400 },
    );
    assert.deepEqual(
      withReviewedWranglerEnvironmentFile(["--version"], reviewed),
      ["--version", "--env-file", reviewed],
    );
    chmodSync(reviewed, 0o600);
    writeFileSync(reviewed, "CLOUDFLARE_ACCOUNT_ID=wrong\n", { mode: 0o600 });
    assert.throws(
      () => withReviewedWranglerEnvironmentFile(["--version"], reviewed),
      /was not empty/,
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("reviewed env-file blocks Wrangler from reloading a poisoned ignored dotenv", () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-wrangler-dotenv-"));
  const output = join(directory, "poisoned-output.json");
  const environment = sanitizedWranglerInspectionEnvironment(process.env);
  const invoke = (args) => spawnSync(process.execPath, [wranglerEntrypoint, ...args], {
    cwd: directory,
    encoding: "utf8",
    env: environment,
    stdio: ["ignore", "pipe", "pipe"],
    timeout: 30_000,
  });
  try {
    writeFileSync(
      join(directory, ".env"),
      `WRANGLER_OUTPUT_FILE_PATH=${output}\n` +
        "CLOUDFLARE_BASE_URL=https://attacker.invalid\n",
      { flag: "wx", mode: 0o600 },
    );

    const baseline = invoke(["--version"]);
    assert.equal(baseline.status, 0);
    assert.equal(existsSync(output), true);
    rmSync(output);

    const guarded = invoke(withReviewedWranglerEnvironmentFile(["--version"]));
    assert.equal(guarded.status, 0, guarded.stderr);
    assert.equal(existsSync(output), false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
