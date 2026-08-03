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
    env: { ...process.env, EMAIL_DIRECTORY_KV_ID: "b".repeat(32) },
  });
  assert.equal(rendered.status, 0, rendered.stderr);

  const config = await readFile(output, "utf8");
  assert.deepEqual(expectedBuildMetadata(config), {
    service: "witself-control-plane",
    version,
    commit,
    date,
  });
  assert.match(
    config,
    /"limits"\s*:\s*\{\s*"cpu_ms"\s*:\s*300000\s*\}/,
    "release config must preserve the CPU ceiling required for archive validation",
  );
  assert.match(
    config,
    /"name"\s*:\s*"ACCOUNT_LIFECYCLE"\s*,\s*"class_name"\s*:\s*"AccountLifecycle"/,
    "release config must bind the per-account lifecycle Durable Object",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v3"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"AccountLifecycle"\s*\]/,
    "release config must preserve the lifecycle Durable Object migration",
  );
  assert.match(
    config,
    /"name"\s*:\s*"CELL_COORDINATOR"\s*,\s*"class_name"\s*:\s*"TargetCellCoordinator"/,
    "release config must bind the per-cell lifecycle serialization authority",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v4"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"TargetCellCoordinator"\s*\]/,
    "release config must preserve the target cell coordinator migration",
  );
  assert.match(
    config,
    /"name"\s*:\s*"ACCOUNT_SIGNUP"\s*,\s*"class_name"\s*:\s*"AccountSignup"/,
    "release config must bind the caller-stable account signup authority",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v5"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"AccountSignup"\s*\]/,
    "release config must preserve the account signup Durable Object migration",
  );
  assert.match(
    config,
    /"name"\s*:\s*"ACCOUNT_BACKUP"\s*,\s*"class_name"\s*:\s*"AccountBackup"/,
    "release config must bind the per-account backup authority",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v6"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"AccountBackup"\s*\]/,
    "release config must preserve the account backup Durable Object migration",
  );
  assert.match(
    config,
    /"name"\s*:\s*"REALM_EMAIL_ALIASES"\s*,\s*"class_name"\s*:\s*"RealmEmailAliasRegistry"/,
    "release config must bind the global managed realm-email alias authority",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v7"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"RealmEmailAliasRegistry"\s*\]/,
    "release config must preserve the realm-email alias registry migration",
  );
  assert.match(
    config,
    /"binding"\s*:\s*"AGENT_EMAIL_DIRECTORY"\s*,\s*"id"\s*:\s*"b{32}"/,
    "control plane must project only into the dedicated agent-email namespace",
  );
  assert.match(
    config,
    /"AGENT_EMAIL_DOMAIN"\s*:\s*"witmail\.net"/,
    "control plane must assign new managed aliases on the permanent domain",
  );
  assert.match(
    config,
    /"AGENT_EMAIL_LEGACY_DOMAINS"\s*:\s*"agent-mail\.witwave\.ai"/,
    "control plane must keep the bounded canonical pilot domain explicit",
  );
  assert.match(
    config,
    /"CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM"\s*:\s*"8"/,
    "release config must preserve the plan-independent per-realm review ceiling",
  );
  assert.match(
    config,
    /"CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT"\s*:\s*"64"/,
    "release config must preserve the plan-independent per-account review ceiling",
  );
  assert.doesNotMatch(
    config,
    /"CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED"\s*:/,
    "realm aliases must remain disabled until catch-all, lifecycle reconciliation, and terminal recovery acceptance pass",
  );
  assert.doesNotMatch(
    config,
    /"CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED"\s*:/,
    "canonical inventory must remain dark until the dual-domain release is accepted",
  );
  assert.doesNotMatch(
    config,
    /"CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED"\s*:/,
    "canonical delivery must remain dark until the dual-domain release is accepted",
  );
  assert.doesNotMatch(
    config,
    /"CP_ACCOUNT_BACKUPS_ENABLED"\s*:/,
    "ordinary deployments must not reset the operator-controlled activation",
  );
  assert.match(
    config,
    /"binding"\s*:\s*"BACKUPS"\s*,\s*"bucket_name"\s*:\s*"witself-backups"/,
    "release config must bind the isolated immutable backup bucket",
  );
  assert.match(
    config,
    /"binding"\s*:\s*"REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL"\s*,\s*"bucket_name"\s*:\s*"witself-realm-email-alias-authority-journal"/,
    "release config must bind a dedicated realm-email alias authority journal",
  );
  assert.doesNotMatch(
    config,
    /"CP_REALM_EMAIL_ALIAS_RECOVERY_TOKEN"\s*:/,
    "the distinct recovery credential must remain a Worker secret",
  );
  assert.doesNotMatch(config, /__WITSELF_[A-Z_]+__/);
  assert.doesNotMatch(config, /__EMAIL_DIRECTORY_KV_ID__/);
  assert.equal((await stat(output)).mode & 0o777, 0o600);
});

test("release renderer rejects the broad control-plane directory for agent email", () => {
  const rejected = spawnSync(process.execPath, [
    renderer.pathname,
    "--version", version,
    "--commit", commit,
    "--date", date,
    "--output", "ignored.jsonc",
  ], {
    cwd: root,
    encoding: "utf8",
    env: {
      ...process.env,
      EMAIL_DIRECTORY_KV_ID: "ec620d5131524e138a9fca6207953cd2",
    },
  });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /must not reuse/);
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
