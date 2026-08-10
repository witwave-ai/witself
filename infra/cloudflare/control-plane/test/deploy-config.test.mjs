import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

import {
  currentProductionVersionID,
  deploymentMatches,
  expectedBuildMetadata,
  verifyCurrentWorkerDeployment,
  verifyWorkerVersion,
} from "../scripts/verify-deployment.mjs";
import {
  bootstrapReleaseDeploymentArguments,
  exactGeneratedConfigPath,
  GENERATED_CONFIG_PATH,
  isFirstManagedCohortProtocolBootstrap,
  preflightManagedCohortProtocolBootstrapPredecessor,
  preflightManagedCohortProtocolUpgrade,
  releaseDeploymentArguments,
  verifyManagedCohortProtocolBootstrapConvergence,
  verifyManagedCohortProtocolBootstrapPredecessor,
  verifyManagedCohortProtocolBootstrapTarget,
  verifyManagedCohortProtocolUpgrade,
} from "../scripts/deploy-release.mjs";
import {
  sourceIdentity,
  taggedReleaseIdentity,
  workerVersionMessage,
  workerVersionTag,
} from "../scripts/source-identity.mjs";
import {
  assertCustomDomainSecretsDark,
  CANONICAL_EMAIL_DARK_SECRET_NAMES,
  CUSTOM_DOMAIN_DARK_SECRET_NAMES,
} from "../scripts/assert-custom-domain-dark.mjs";

const root = new URL("..", import.meta.url);
const renderer = new URL("../scripts/render-wrangler.mjs", import.meta.url);
const version = "1.2.3";
const commit = "a".repeat(40);
const date = "2026-07-23T01:02:03Z";
const versionID = "01234567-89ab-cdef-0123-456789abcdef";
const routeSigningKeyID = "route-2026-08";
const agentEmailDirectoryID = "b".repeat(32);
const emailEdgeDeploymentID = "11111111-1111-4111-8111-111111111111";
const emailEdgeVersionID = "22222222-2222-4222-8222-222222222222";
const bootstrapControlPlaneDeploymentID = "33333333-3333-4333-8333-333333333333";
const bootstrapPredecessorVersionID = "44444444-4444-4444-8444-444444444444";
const bootstrapTargetVersionID = "55555555-5555-4555-8555-555555555555";
const bootstrapConvergedVersionID = "66666666-6666-4666-8666-666666666666";
const cohortAccount = "acc_abcdefghijkl2345";
const secondCohortAccount = "acc_bcdefghijklm2345";

test("generic control-plane secret mutation is explicitly break-glass", async () => {
  const packageJSON = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"));
  assert.equal(Object.hasOwn(packageJSON.scripts, "secret:put"), false);
  assert.equal(
    packageJSON.scripts["secret:put:break-glass"],
    "npm run config && wrangler secret put --config wrangler.generated.jsonc",
  );
});

function emailEdgeDeployment() {
  return {
    id: emailEdgeDeploymentID,
    strategy: "percentage",
    versions: [{ version_id: emailEdgeVersionID, percentage: 100 }],
  };
}

function emailEdgeVersion({
  release = "0.0.240",
  alias = "false",
  canonical = "false",
  cohort = "",
} = {}) {
  return {
    id: emailEdgeVersionID,
    resources: {
      script: { handlers: ["email"] },
      bindings: [
        { name: "WITSELF_EDGE_RELEASE_VERSION", type: "plain_text", text: release },
        { name: "CONTROL_PLANE_URL", type: "plain_text", text: "https://self.witwave.ai/" },
        { name: "REALM_EMAIL_ALIAS_DELIVERY_ENABLED", type: "plain_text", text: alias },
        { name: "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", type: "plain_text", text: canonical },
        { name: "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", type: "plain_text", text: cohort },
      ],
    },
  };
}

test("v0.0.241 CP-first deployment requires the active v0.0.240 edge to be dark", () => {
  assert.deepEqual(
    verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      emailEdgeVersion(),
    ),
    {
      required: true,
      edge_release: "0.0.240",
      already_current: false,
      target_account_count: 0,
      active_edge_account_count: 0,
      operations_lease_origin: "https://self.witwave.ai",
    },
  );
  assert.throws(
    () => verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      emailEdgeVersion(),
      cohortAccount,
    ),
    /requires an empty managed cohort/,
  );
  for (const overrides of [
    { alias: "true" },
    { canonical: "true" },
  ]) {
    assert.throws(
      () => verifyManagedCohortProtocolUpgrade(
        "0.0.241",
        emailEdgeDeployment(),
        emailEdgeVersion(overrides),
      ),
      /managed delivery gates to be false/,
    );
  }
  assert.throws(
    () => verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      emailEdgeVersion({ release: "0.0.239" }),
    ),
    /requires a v0\.0\.240 or v0\.0\.241 email edge/,
  );
});

test("current edge requires the control-plane cohort to contain its active cohort", () => {
  assert.deepEqual(
    verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      emailEdgeVersion({ release: "0.0.241", cohort: cohortAccount }),
      `${cohortAccount},${secondCohortAccount}`,
    ),
    {
      required: true,
      edge_release: "0.0.241",
      already_current: true,
      target_account_count: 2,
      active_edge_account_count: 1,
      operations_lease_origin: "https://self.witwave.ai",
    },
  );
  assert.throws(
    () => verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      emailEdgeVersion({ release: "0.0.241", cohort: cohortAccount }),
      "",
    ),
    /remove from the edge first/,
  );
});

test("managed cohort protocol preflight inspects the exact active email edge before CP mutation", () => {
  const calls = [];
  const result = preflightManagedCohortProtocolUpgrade("0.0.241", "", (args) => {
    calls.push(args);
    return calls.length === 2 ? emailEdgeVersion() : emailEdgeDeployment();
  });
  assert.deepEqual(result, {
    required: true,
    edge_release: "0.0.240",
    already_current: false,
    target_account_count: 0,
    active_edge_account_count: 0,
    operations_lease_origin: "https://self.witwave.ai",
  });
  assert.deepEqual(calls, [
    ["deployments", "status", "--name", "witself-agent-email-pilot", "--json"],
    [
      "versions", "view", emailEdgeVersionID,
      "--name", "witself-agent-email-pilot", "--json",
    ],
    ["deployments", "status", "--name", "witself-agent-email-pilot", "--json"],
  ]);
  let inspected = false;
  assert.deepEqual(
    preflightManagedCohortProtocolUpgrade("0.0.240", "", () => {
      inspected = true;
    }),
    { required: false },
  );
  assert.equal(inspected, false);
});

test("managed cohort protocol preflight rejects origin substitution and active-version drift", () => {
  assert.throws(
    () => verifyManagedCohortProtocolUpgrade(
      "0.0.241",
      emailEdgeDeployment(),
      {
        ...emailEdgeVersion(),
        resources: {
          ...emailEdgeVersion().resources,
          bindings: emailEdgeVersion().resources.bindings.map((binding) =>
            binding.name === "CONTROL_PLANE_URL"
              ? { ...binding, text: "https://attacker.invalid/" }
              : binding),
        },
      },
    ),
    /exact control-plane route/,
  );

  let calls = 0;
  assert.throws(
    () => preflightManagedCohortProtocolUpgrade("0.0.241", "", () => {
      calls += 1;
      if (calls === 2) return emailEdgeVersion();
      if (calls === 3) {
        return {
          ...emailEdgeDeployment(),
          versions: [{
            version_id: "77777777-7777-4777-8777-777777777777",
            percentage: 100,
          }],
        };
      }
      return emailEdgeDeployment();
    }),
    /changed during exact provider inspection/,
  );
});

test("only the exact dark v0.0.241 transition may bootstrap the operations lease", () => {
  const dark = verifyManagedCohortProtocolUpgrade(
    "0.0.241",
    emailEdgeDeployment(),
    emailEdgeVersion(),
  );
  const predecessor = {
    schema: "witself.agent-email-control-plane-bootstrap-predecessor.v1",
    release: { version: "0.0.240" },
  };
  assert.equal(
    isFirstManagedCohortProtocolBootstrap("0.0.241", dark, predecessor),
    true,
  );
  for (const [release, candidate] of [
    ["0.0.242", dark],
    ["0.0.241", { ...dark, already_current: true }],
    ["0.0.241", { ...dark, target_account_count: 1 }],
    ["0.0.241", { ...dark, active_edge_account_count: 1 }],
    ["0.0.241", { ...dark, edge_release: "0.0.241" }],
  ]) {
    assert.equal(
      isFirstManagedCohortProtocolBootstrap(release, candidate, predecessor),
      false,
    );
  }
  assert.equal(isFirstManagedCohortProtocolBootstrap("0.0.241", dark, null), false);
});

test("release renderer injects matching immutable container and Worker identity", async (t) => {
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
    env: {
      ...process.env,
      EMAIL_DIRECTORY_KV_ID: "b".repeat(32),
      AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: routeSigningKeyID,
    },
  });
  assert.equal(rendered.status, 0, rendered.stderr);

  const config = await readFile(output, "utf8");
  assert.deepEqual(expectedBuildMetadata(config), {
    service: "witself-control-plane",
    version,
    commit,
    date,
    route_signing_key_id: routeSigningKeyID,
    agent_email_directory_id: agentEmailDirectoryID,
    managed_delivery_account_allowlist: "",
  });
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"main": "src/index.js"',
      '"main": "src/stamped-attacker.js"',
    )),
    /main Worker entrypoint/,
    "release identity stamps must not make a different Worker main deployable",
  );
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"compatibility_date": "2026-06-01"',
      '"compatibility_date": "2026-07-01"',
    )),
    /Worker runtime/,
    "release identity stamps must not make a different runtime deployable",
  );
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"class_name": "AccountSignup"',
      '"class_name": "StampedAttacker"',
    )),
    /Durable Object binding ACCOUNT_SIGNUP/,
    "release identity stamps must not make a different binding deployable",
  );
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"DATE": "2026-07-23T01:02:03Z"',
      '"DATE": "2026-07-23T01:02:03Z", "ATTACKER_BUILD_ARG": "same-stamp"',
    )),
    /Backend container contract/,
    "release identity stamps must not hide an extra container build argument",
  );
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"max_instances": 2',
      '"max_instances": 2, "scheduling_policy": "attacker"',
    )),
    /Backend container contract/,
    "release identity stamps must not hide an extra container field",
  );
  assert.throws(
    () => expectedBuildMetadata(config.replace(
      '"observability": {',
      '"unexpected_top_level": true, "observability": {',
    )),
    /top-level contract/,
    "release identity stamps must not hide an extra top-level setting",
  );
  assert.match(
    config,
    /"WITSELF_EDGE_RELEASE_VERSION"\s*:\s*"1\.2\.3"/,
  );
  assert.match(
    config,
    new RegExp(`"WITSELF_EDGE_RELEASE_COMMIT"\\s*:\\s*"${commit}"`),
  );
  assert.match(
    config,
    /"WITSELF_EDGE_RELEASE_DATE"\s*:\s*"2026-07-23T01:02:03Z"/,
  );
  assert.match(
    config,
    /"AGENT_EMAIL_ROUTE_SIGNING_KEY_ID"\s*:\s*"route-2026-08"/,
  );
  assert.match(
    config,
    /"secrets"\s*:\s*\{[\s\S]*?"AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"[\s\S]*?"CONTROL_PLANE_EDGE_TOKEN"[\s\S]*?\}/,
  );
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
    /"name"\s*:\s*"AGENT_EMAIL_DOMAINS"\s*,\s*"class_name"\s*:\s*"AgentEmailDomainRegistry"/,
    "release config must bind the dark customer-domain authority",
  );
  assert.match(
    config,
    /"tag"\s*:\s*"v8"\s*,\s*"new_sqlite_classes"\s*:\s*\[\s*"AgentEmailDomainRegistry"\s*\]/,
    "release config must preserve the customer-domain registry migration",
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
  assert.match(
    config,
    /"CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT"\s*:\s*"8"/,
    "release config must preserve the plan-independent custom-domain queue ceiling",
  );
  assert.match(
    config,
    /"CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST"\s*:\s*""/,
    "release config must keep the managed account cohort dark by default",
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
    /"CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED"\s*:/,
    "ordinary deployments must leave customer-domain requests dark",
  );
  for (const gate of [
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST",
  ]) {
    assert.doesNotMatch(
      config,
      new RegExp(`"${gate}"\\s*:`),
      `${gate} must remain absent from ordinary deployments`,
    );
  }
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
  assert.match(
    config,
    /"binding"\s*:\s*"AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL"\s*,\s*"bucket_name"\s*:\s*"witself-agent-email-domain-authority-journal"/,
    "release config must bind a dedicated customer-domain authority journal",
  );
  assert.doesNotMatch(
    config,
    /"CP_REALM_EMAIL_ALIAS_RECOVERY_TOKEN"\s*:/,
    "the distinct recovery credential must remain a Worker secret",
  );
  assert.doesNotMatch(
    config,
    /"CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED"\s*:/,
    "ordinary deployments must not reset the customer-domain journal gate",
  );
  assert.doesNotMatch(
    config,
    /"CP_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN"\s*:/,
    "the customer-domain recovery credential must remain a Worker secret",
  );
  assert.doesNotMatch(config, /__WITSELF_[A-Z_]+__/);
  assert.doesNotMatch(config, /__EMAIL_DIRECTORY_KV_ID__/);
  assert.equal((await stat(output)).mode & 0o777, 0o600);
});

test("release renderer accepts only an exact canonical managed account cohort", async (t) => {
  const temp = await mkdtemp(join(tmpdir(), "witself-cp-cohort-config-"));
  t.after(() => rm(temp, { recursive: true, force: true }));
  for (const [index, value] of [
    "*", "acc_*", " acc_aaaaaaaaaaaaaaaa", "acc_aaaaaaaaaaaaaaaa ",
    "acc_bbbbbbbbbbbbbbbb,acc_aaaaaaaaaaaaaaaa",
    "acc_aaaaaaaaaaaaaaaa,acc_aaaaaaaaaaaaaaaa",
  ].entries()) {
    const rendered = spawnSync(process.execPath, [
      renderer.pathname,
      "--version", version,
      "--commit", commit,
      "--date", date,
      "--output", join(temp, `wrangler-${index}.jsonc`),
    ], {
      cwd: root,
      encoding: "utf8",
      env: {
        ...process.env,
        EMAIL_DIRECTORY_KV_ID: agentEmailDirectoryID,
        AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: routeSigningKeyID,
        CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: value,
      },
    });
    assert.notEqual(rendered.status, 0, value);
    assert.match(rendered.stderr, /allowlist is invalid/);
  }
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
      AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: routeSigningKeyID,
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

test("release renderer rejects non-canonical semantic versions", () => {
  const rejected = spawnSync(process.execPath, [
    renderer.pathname,
    "--version", "01.2.3",
    "--commit", commit,
    "--date", date,
  ], {
    cwd: root,
    encoding: "utf8",
  });
  assert.notEqual(rejected.status, 0);
  assert.match(rejected.stderr, /MAJOR\.MINOR\.PATCH/);
});

function runGit(cwd, args) {
  const result = spawnSync("git", args, { cwd, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
}

test("release source requires one clean exact semantic-version tag", async (t) => {
  const repositoryRoot = await mkdtemp(join(tmpdir(), "witself-cp-release-source-"));
  t.after(() => rm(repositoryRoot, { recursive: true, force: true }));
  runGit(repositoryRoot, ["init", "--quiet"]);
  await writeFile(join(repositoryRoot, "release.txt"), "release\n");
  runGit(repositoryRoot, ["add", "release.txt"]);
  runGit(repositoryRoot, [
    "-c", "user.name=Witself Test",
    "-c", "user.email=test@witself.invalid",
    "commit", "--quiet", "-m", "release",
  ]);
  runGit(repositoryRoot, ["tag", "v1.2.3"]);

  const identity = sourceIdentity({ repositoryRoot });
  assert.equal(identity.version, "1.2.3");
  assert.equal(identity.tag, "v1.2.3");
  assert.match(identity.commit, /^[0-9a-f]{40}$/);
  assert.equal(identity.clean, true);
  assert.deepEqual(
    taggedReleaseIdentity("1.2.3", { repositoryRoot }),
    {
      version: "1.2.3",
      commit: identity.commit,
      date: identity.date,
      tag: "v1.2.3",
    },
  );
  assert.throws(
    () => taggedReleaseIdentity("1.2.4", { repositoryRoot }),
    /could not resolve v1\.2\.4 commit from git/,
  );

  await writeFile(join(repositoryRoot, "dirty.txt"), "dirty\n");
  assert.throws(
    () => sourceIdentity({ repositoryRoot }),
    /clean checkout at one exact semantic-version tag/,
  );
  await rm(join(repositoryRoot, "dirty.txt"));
  runGit(repositoryRoot, ["tag", "v1.2.4"]);
  assert.throws(
    () => sourceIdentity({ repositoryRoot }),
    /clean checkout at one exact semantic-version tag/,
  );
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

function expectedIdentity() {
  return {
    service: "witself-control-plane",
    version,
    commit,
    date,
    route_signing_key_id: routeSigningKeyID,
    agent_email_directory_id: agentEmailDirectoryID,
    managed_delivery_account_allowlist: "",
  };
}

function deployedVersion(overrides = {}) {
  const expected = expectedIdentity();
  return {
    id: versionID,
    metadata: { source: "wrangler" },
    annotations: {
      "workers/triggered_by": "upload",
      "workers/tag": workerVersionTag(expected),
      "workers/message": workerVersionMessage(expected),
    },
    resources: {
      script: {
        etag: "b".repeat(64),
        handlers: ["fetch", "scheduled"],
        named_handlers: [
          "AccountBackup",
          "AccountLifecycle",
          "AccountSignup",
          "AgentEmailDomainRegistry",
          "Backend",
          "RealmEmailAliasRegistry",
          "TargetCellCoordinator",
        ].map((name) => ({ name, handlers: ["class"] })),
      },
      script_runtime: {
        migration_tag: "v8",
        compatibility_date: "2026-06-01",
        usage_model: "standard",
        limits: { cpu_ms: 300000 },
        containers: [{ class_name: "Backend" }],
      },
      bindings: [
        ...[
          ["ACCOUNT_BACKUP", "AccountBackup"],
          ["ACCOUNT_LIFECYCLE", "AccountLifecycle"],
          ["ACCOUNT_SIGNUP", "AccountSignup"],
          ["AGENT_EMAIL_DOMAINS", "AgentEmailDomainRegistry"],
          ["CELL_COORDINATOR", "TargetCellCoordinator"],
          ["CONTROL_PLANE", "Backend"],
          ["REALM_EMAIL_ALIASES", "RealmEmailAliasRegistry"],
        ].map(([name, class_name]) => ({
          name,
          class_name,
          namespace_id: "c".repeat(32),
          type: "durable_object_namespace",
        })),
        {
          name: "WITSELF_EDGE_RELEASE_VERSION",
          type: "plain_text",
          text: version,
        },
        {
          name: "WITSELF_EDGE_RELEASE_COMMIT",
          type: "plain_text",
          text: commit,
        },
        {
          name: "WITSELF_EDGE_RELEASE_DATE",
          type: "plain_text",
          text: date,
        },
        {
          name: "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
          type: "plain_text",
          text: routeSigningKeyID,
        },
        ...[
          ["AGENT_EMAIL_DOMAIN", "witmail.net"],
          ["AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"],
          ["CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM", "8"],
          ["CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT", "64"],
          ["CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT", "8"],
          ["CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""],
        ].map(([name, text]) => ({ name, type: "plain_text", text })),
        {
          name: "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY",
          type: "secret_text",
        },
        {
          name: "CONTROL_PLANE_EDGE_TOKEN",
          type: "secret_text",
        },
        {
          name: "AGENT_EMAIL_DIRECTORY",
          type: "kv_namespace",
          namespace_id: agentEmailDirectoryID,
        },
        {
          name: "DIRECTORY",
          type: "kv_namespace",
          namespace_id: "ec620d5131524e138a9fca6207953cd2",
        },
        ...[
          [
            "AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL",
            "witself-agent-email-domain-authority-journal",
          ],
          ["ARCHIVES", "witself-archives"],
          ["BACKUPS", "witself-backups"],
          [
            "REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL",
            "witself-realm-email-alias-authority-journal",
          ],
        ].map(([name, bucket_name]) => ({
          name,
          bucket_name,
          type: "r2_bucket",
        })),
        { name: "EMAIL", type: "send_email" },
        {
          name: "RECOVER_LIMITER",
          type: "ratelimit",
          namespace_id: "1001",
          simple: { limit: 1, period: 10 },
        },
        {
          name: "CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED",
          type: "secret_text",
        },
      ],
    },
    ...overrides,
  };
}

function controlPlaneDeployment(activeVersionID, {
  deploymentID = bootstrapControlPlaneDeploymentID,
} = {}) {
  return {
    id: deploymentID,
    strategy: "percentage",
    versions: [{ version_id: activeVersionID, percentage: 100 }],
  };
}

function controlPlaneReleaseIdentity(release, releaseCommit, releaseDate) {
  return {
    ...expectedIdentity(),
    version: release,
    commit: releaseCommit,
    date: releaseDate,
    managed_delivery_account_allowlist: "",
  };
}

function controlPlaneReleaseVersion(identity, {
  id,
  includeManagedCohort = true,
  includeAliasActivation = false,
  namespaceOverrides = {},
  scriptETag = "b".repeat(64),
} = {}) {
  const candidate = deployedVersion();
  candidate.id = id;
  candidate.annotations = {
    "workers/triggered_by": "upload",
    "workers/tag": workerVersionTag(identity),
    "workers/message": workerVersionMessage(identity),
  };
  candidate.resources.script.etag = scriptETag;
  for (const [name, text] of [
    ["WITSELF_EDGE_RELEASE_VERSION", identity.version],
    ["WITSELF_EDGE_RELEASE_COMMIT", identity.commit],
    ["WITSELF_EDGE_RELEASE_DATE", identity.date],
  ]) {
    candidate.resources.bindings.find((binding) => binding.name === name).text = text;
  }
  candidate.resources.bindings = candidate.resources.bindings.filter((binding) =>
    (includeManagedCohort ||
      binding.name !== "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST") &&
    (includeAliasActivation ||
      binding.name !== "CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED"));
  for (const binding of candidate.resources.bindings) {
    if (binding.type === "durable_object_namespace" &&
        Object.hasOwn(namespaceOverrides, binding.name)) {
      binding.namespace_id = namespaceOverrides[binding.name];
    }
  }
  return candidate;
}

test("lease bootstrap proves only the exact dark v0.0.240 predecessor", () => {
  const target = controlPlaneReleaseIdentity(
    "0.0.241",
    "a".repeat(40),
    "2026-08-09T01:02:03Z",
  );
  const predecessor = {
    version: "0.0.240",
    commit: "d".repeat(40),
    date: "2026-08-08T01:02:03Z",
    tag: "v0.0.240",
  };
  const predecessorIdentity = {
    ...target,
    ...predecessor,
  };
  delete predecessorIdentity.tag;
  const predecessorVersion = controlPlaneReleaseVersion(predecessorIdentity, {
    id: bootstrapPredecessorVersionID,
    includeManagedCohort: false,
    includeAliasActivation: true,
  });
  assert.equal(
    predecessorVersion.resources.bindings.some((binding) =>
      binding.name === "CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED" &&
      binding.type === "secret_text"),
    true,
    "the live v0.0.240 alias-administration gate is legitimate predecessor state",
  );
  const deployment = controlPlaneDeployment(bootstrapPredecessorVersionID);

  const proof = verifyManagedCohortProtocolBootstrapPredecessor(
    target,
    predecessor,
    deployment,
    predecessorVersion,
  );
  assert.equal(
    proof.schema,
    "witself.agent-email-control-plane-bootstrap-predecessor.v1",
  );
  assert.deepEqual(proof.release, {
    version: predecessor.version,
    commit: predecessor.commit,
    date: predecessor.date,
  });
  assert.equal(proof.operations_lease_namespace_id, "c".repeat(32));
  assert.equal(proof.durable_object_namespaces.REALM_EMAIL_ALIASES, "c".repeat(32));
  assert.equal(Object.keys(proof.durable_object_namespaces).length, 7);

  const currentVersion = controlPlaneReleaseVersion(target, {
    id: bootstrapTargetVersionID,
  });
  assert.throws(
    () => verifyManagedCohortProtocolBootstrapPredecessor(
      target,
      predecessor,
      controlPlaneDeployment(bootstrapTargetVersionID),
      currentVersion,
    ),
    /release annotations/,
    "a current control plane returning 404 must never qualify as legacy",
  );

  const presentCohort = controlPlaneReleaseVersion(predecessorIdentity, {
    id: bootstrapPredecessorVersionID,
    includeManagedCohort: true,
  });
  assert.throws(
    () => verifyManagedCohortProtocolBootstrapPredecessor(
      target,
      predecessor,
      deployment,
      presentCohort,
    ),
    /canonical delivery dark and no managed cohort binding/,
  );

  const activeGate = controlPlaneReleaseVersion(predecessorIdentity, {
    id: bootstrapPredecessorVersionID,
    includeManagedCohort: false,
    includeAliasActivation: true,
  });
  activeGate.resources.bindings.push({
    name: "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    type: "secret_text",
  });
  assert.throws(
    () => verifyManagedCohortProtocolBootstrapPredecessor(
      target,
      predecessor,
      deployment,
      activeGate,
    ),
    /canonical delivery dark and no managed cohort binding/,
  );

  assert.throws(
    () => verifyManagedCohortProtocolBootstrapPredecessor(
      target,
      { ...predecessor, commit: "e".repeat(40) },
      deployment,
      predecessorVersion,
    ),
    /release annotations/,
  );
});

test("lease bootstrap rechecks a stable provider predecessor and preserves every Durable Object namespace", () => {
  const target = controlPlaneReleaseIdentity(
    "0.0.241",
    "a".repeat(40),
    "2026-08-09T01:02:03Z",
  );
  const predecessor = {
    version: "0.0.240",
    commit: "d".repeat(40),
    date: "2026-08-08T01:02:03Z",
    tag: "v0.0.240",
  };
  const predecessorIdentity = { ...target, ...predecessor };
  delete predecessorIdentity.tag;
  const predecessorVersion = controlPlaneReleaseVersion(predecessorIdentity, {
    id: bootstrapPredecessorVersionID,
    includeManagedCohort: false,
    includeAliasActivation: true,
  });
  const predecessorDeployment = controlPlaneDeployment(bootstrapPredecessorVersionID);
  const calls = [];
  const proof = preflightManagedCohortProtocolBootstrapPredecessor(
    target,
    predecessor,
    GENERATED_CONFIG_PATH,
    (args) => {
      calls.push(args);
      return calls.length === 2 ? predecessorVersion : predecessorDeployment;
    },
  );
  assert.equal(proof.version_id, bootstrapPredecessorVersionID);
  assert.equal(calls.length, 3);

  let driftCalls = 0;
  assert.throws(
    () => preflightManagedCohortProtocolBootstrapPredecessor(
      target,
      predecessor,
      GENERATED_CONFIG_PATH,
      () => {
        driftCalls += 1;
        if (driftCalls === 2) return predecessorVersion;
        if (driftCalls === 3) {
          return controlPlaneDeployment(bootstrapTargetVersionID);
        }
        return predecessorDeployment;
      },
    ),
    /changed during exact provider inspection/,
  );

  const targetVersion = controlPlaneReleaseVersion(target, {
    id: bootstrapTargetVersionID,
  });
  const staged = verifyManagedCohortProtocolBootstrapTarget(
    target,
    proof,
    controlPlaneDeployment(bootstrapTargetVersionID),
    targetVersion,
  );
  const convergedVersion = controlPlaneReleaseVersion(target, {
    id: bootstrapConvergedVersionID,
  });
  const converged = verifyManagedCohortProtocolBootstrapTarget(
    target,
    proof,
    controlPlaneDeployment(bootstrapConvergedVersionID),
    convergedVersion,
  );
  assert.deepEqual(
    verifyManagedCohortProtocolBootstrapConvergence(staged, converged),
    {
      script_etag: "b".repeat(64),
      operations_lease_namespace_id: "c".repeat(32),
      durable_object_namespaces: staged.durable_object_namespaces,
    },
  );

  const changedNamespace = controlPlaneReleaseVersion(target, {
    id: bootstrapConvergedVersionID,
    namespaceOverrides: { ACCOUNT_BACKUP: "e".repeat(32) },
  });
  assert.throws(
    () => verifyManagedCohortProtocolBootstrapTarget(
      target,
      proof,
      controlPlaneDeployment(bootstrapConvergedVersionID),
      changedNamespace,
    ),
    /changed its Durable Object namespace inventory/,
  );

  assert.throws(
    () => verifyManagedCohortProtocolBootstrapConvergence(
      staged,
      { ...converged, script_etag: "f".repeat(64) },
    ),
    /byte-identical release artifact/,
  );
});

test("Worker deployment verification requires one exact 100 percent version", () => {
  assert.equal(currentProductionVersionID({
    versions: [{ version_id: versionID, percentage: 100 }],
  }), versionID);
  for (const deployment of [
    { versions: [] },
    { versions: [{ version_id: versionID, percentage: 99 }] },
    {
      versions: [
        { version_id: versionID, percentage: 50 },
        { version_id: "fedcba98-7654-3210-fedc-ba9876543210", percentage: 50 },
      ],
    },
  ]) {
    assert.throws(() => currentProductionVersionID(deployment));
  }
});

test("Worker version verification checks annotations, bindings, and script etag", () => {
  const expected = expectedIdentity();
  assert.deepEqual(verifyWorkerVersion(deployedVersion(), expected, versionID), {
    version_id: versionID,
    script_etag: "b".repeat(64),
    managed_delivery_cohort: {
      account_count: 0,
      allowlist_sha256:
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    },
  });

  const wrongAnnotation = deployedVersion();
  wrongAnnotation.annotations["workers/tag"] = "v1.2.4";
  assert.throws(
    () => verifyWorkerVersion(wrongAnnotation, expected, versionID),
    /release annotations/,
  );

  const wrongBinding = deployedVersion();
  wrongBinding.resources.bindings.find(
    (binding) => binding.name === "WITSELF_EDGE_RELEASE_COMMIT",
  ).text = "c".repeat(40);
  assert.throws(
    () => verifyWorkerVersion(wrongBinding, expected, versionID),
    /wrong WITSELF_EDGE_RELEASE_COMMIT binding/,
  );

  const noETag = deployedVersion();
  noETag.resources.script.etag = "";
  assert.throws(
    () => verifyWorkerVersion(noETag, expected, versionID),
    /script etag/,
  );

  const wrongDirectory = deployedVersion();
  wrongDirectory.resources.bindings.find(
    (binding) => binding.name === "AGENT_EMAIL_DIRECTORY",
  ).namespace_id = "c".repeat(32);
  assert.throws(
    () => verifyWorkerVersion(wrongDirectory, expected, versionID),
    /wrong AGENT_EMAIL_DIRECTORY KV binding/,
  );
});

test("Worker version verification rejects stamped script and runtime drift", () => {
  const expected = expectedIdentity();
  for (const [mutate, message] of [
    [
      (value) => { value.resources.script.handlers = ["fetch"]; },
      /handlers did not match/,
    ],
    [
      (value) => {
        value.resources.script.named_handlers[0].name = "StampedAttacker";
      },
      /named handler classes did not match/,
    ],
    [
      (value) => {
        value.resources.script_runtime.compatibility_date = "2026-07-01";
      },
      /runtime contract/,
    ],
    [
      (value) => { value.resources.script_runtime.migration_tag = "v7"; },
      /runtime contract/,
    ],
    [
      (value) => { value.resources.script_runtime.usage_model = "bundled"; },
      /runtime contract/,
    ],
    [
      (value) => { value.resources.script_runtime.limits.cpu_ms = 299999; },
      /runtime contract/,
    ],
    [
      (value) => {
        value.resources.script_runtime.containers[0].class_name = "StampedAttacker";
      },
      /runtime contract/,
    ],
  ]) {
    const candidate = deployedVersion();
    mutate(candidate);
    assert.throws(
      () => verifyWorkerVersion(candidate, expected, versionID),
      message,
    );
  }
});

test("Worker version verification enforces complete non-secret bindings", () => {
  const expected = expectedIdentity();
  for (const [name, mutate, message] of [
    [
      "Durable Object class",
      (bindings) => {
        bindings.find((binding) => binding.name === "ACCOUNT_BACKUP")
          .class_name = "StampedAttacker";
      },
      /wrong ACCOUNT_BACKUP Durable Object binding/,
    ],
    [
      "fixed directory",
      (bindings) => {
        bindings.find((binding) => binding.name === "DIRECTORY")
          .namespace_id = "d".repeat(32);
      },
      /wrong DIRECTORY KV binding/,
    ],
    [
      "fixed variable",
      (bindings) => {
        bindings.find((binding) => binding.name === "AGENT_EMAIL_DOMAIN")
          .text = "attacker.invalid";
      },
      /wrong AGENT_EMAIL_DOMAIN binding/,
    ],
    [
      "R2 bucket",
      (bindings) => {
        bindings.find((binding) => binding.name === "ARCHIVES")
          .bucket_name = "attacker-bucket";
      },
      /wrong ARCHIVES R2 binding/,
    ],
    [
      "email binding",
      (bindings) => {
        bindings.find((binding) => binding.name === "EMAIL").type = "plain_text";
      },
      /wrong EMAIL send binding/,
    ],
    [
      "rate limiter",
      (bindings) => {
        bindings.find((binding) => binding.name === "RECOVER_LIMITER")
          .simple.limit = 2;
      },
      /wrong RECOVER_LIMITER binding/,
    ],
  ]) {
    const candidate = deployedVersion();
    mutate(candidate.resources.bindings);
    assert.throws(
      () => verifyWorkerVersion(candidate, expected, versionID),
      message,
      name,
    );
  }

  const extraBinding = deployedVersion();
  extraBinding.resources.bindings.push({
    name: "STAMPED_ATTACKER_BINDING",
    type: "plain_text",
    text: "same-release-stamp",
  });
  assert.throws(
    () => verifyWorkerVersion(extraBinding, expected, versionID),
    /unexpected non-secret binding STAMPED_ATTACKER_BINDING/,
  );

  const duplicate = deployedVersion();
  duplicate.resources.bindings.push({
    name: "DIRECTORY",
    type: "kv_namespace",
    namespace_id: "ec620d5131524e138a9fca6207953cd2",
  });
  assert.throws(
    () => verifyWorkerVersion(duplicate, expected, versionID),
    /duplicate or invalid bindings/,
  );

  const optionalSecret = deployedVersion();
  optionalSecret.resources.bindings.push({
    name: "CP_ACCOUNT_BACKUPS_ENABLED",
    type: "secret_text",
  });
  assert.doesNotThrow(
    () => verifyWorkerVersion(optionalSecret, expected, versionID),
    "operator-managed secrets must survive an otherwise exact release",
  );
});

test("Worker verifier follows the one production version through Wrangler JSON", () => {
  const calls = [];
  const inspected = verifyCurrentWorkerDeployment(
    expectedIdentity(),
    "/tmp/wrangler.jsonc",
    (args, operation) => {
      calls.push({ args, operation });
      return args[0] === "deployments"
        ? { versions: [{ version_id: versionID, percentage: 100 }] }
        : deployedVersion();
    },
  );
  assert.deepEqual(inspected, {
    version_id: versionID,
    script_etag: "b".repeat(64),
    managed_delivery_cohort: {
      account_count: 0,
      allowlist_sha256:
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    },
  });
  assert.deepEqual(calls, [
    {
      args: [
        "deployments", "status",
        "--config", "/tmp/wrangler.jsonc",
        "--name", "witself-control-plane",
        "--json",
      ],
      operation: "inspect the current control-plane deployment",
    },
    {
      args: [
        "versions", "view", versionID,
        "--config", "/tmp/wrangler.jsonc",
        "--name", "witself-control-plane",
        "--json",
      ],
      operation: "inspect the current control-plane Worker version",
    },
  ]);
});

test("release deployment is pinned to the exact generated config", () => {
  assert.equal(
    exactGeneratedConfigPath("wrangler.generated.jsonc"),
    GENERATED_CONFIG_PATH,
  );
  assert.throws(
    () => exactGeneratedConfigPath("/tmp/stamped-wrangler.jsonc"),
    /exact generated control-plane config/,
  );
  assert.throws(
    () => releaseDeploymentArguments(
      expectedIdentity(),
      "/tmp/stamped-wrangler.jsonc",
    ),
    /exact generated control-plane config/,
  );
  assert.deepEqual(
    releaseDeploymentArguments(expectedIdentity()),
    [
      "deploy",
      "--config", GENERATED_CONFIG_PATH,
      "--strict",
      "--tag", "v1.2.3",
      "--message", `witself-control-plane v1.2.3 ${commit}`,
    ],
  );
  assert.deepEqual(
    bootstrapReleaseDeploymentArguments(expectedIdentity()),
    [
      ...releaseDeploymentArguments(expectedIdentity()),
      "--containers-rollout", "none",
    ],
    "the sole unleased bootstrap write must suppress every Container rollout",
  );
});

test("dark deployment refuses every persistent agent-email activation secret", async () => {
  assert.doesNotThrow(() => assertCustomDomainSecretsDark([
    { name: "CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED", type: "secret_text" },
    { name: "CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED", type: "secret_text" },
  ]));
  for (const name of [
    ...CANONICAL_EMAIL_DARK_SECRET_NAMES,
    ...CUSTOM_DOMAIN_DARK_SECRET_NAMES,
  ]) {
    assert.throws(
      () => assertCustomDomainSecretsDark([{ name, type: "secret_text" }]),
      new RegExp(name),
    );
  }
  for (const malformed of [null, {}, [{ type: "secret_text" }], [{ name: " bad" }]]) {
    assert.throws(() => assertCustomDomainSecretsDark(malformed));
  }
  const packageJSON = JSON.parse(await readFile(new URL("../package.json", import.meta.url)));
  assert.equal(
    packageJSON.scripts.deploy,
    "node scripts/deploy-release.mjs",
    "deployment must create and hold its own private generated configuration",
  );
  const deploySource = await readFile(
    new URL("../scripts/deploy-release.mjs", import.meta.url),
    "utf8",
  );
  assert.match(
    deploySource,
    /createPrivateDeploymentConfig/,
    "deployment must use a per-invocation immutable configuration",
  );
  assert.match(
    deploySource,
    /scripts", "assert-custom-domain-dark\.mjs/,
    "deployment must validate persistent activation secrets before upload",
  );
});
