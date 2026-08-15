import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  chmod,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  assertProductionReleaseUnchanged,
  canonicalControlPlaneOrigin,
  createProductionWorkerSourceSnapshot,
  deployProductionReceive,
  preflightManagedCohortDeploymentOrder,
  preflightProductionSecretInventory,
  productionDeploymentArguments,
  productionDeploymentEnvironments,
  runGitInspection,
  snapshotProductionWorkerSource,
  validateProductionDeploymentConfig,
  verifyManagedCohortDeploymentOrder,
  verifyProductionSecretInventory,
} from "../scripts/deploy.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
  WRANGLER_PRODUCTION_ENV_FILE,
} from "../scripts/wrangler-environment.mjs";

const deploymentID = "11111111-1111-4111-8111-111111111111";
const versionID = "22222222-2222-4222-8222-222222222222";
const firstAccount = "acc_abcdefghijkl2345";
const secondAccount = "acc_bcdefghijklm2345";

function deployment() {
  return {
    id: deploymentID,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function version({ release = "0.0.241", cohort = "" } = {}) {
  return {
    id: versionID,
    resources: {
      bindings: [
        { name: "WITSELF_EDGE_RELEASE_VERSION", type: "plain_text", text: release },
        {
          name: "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
          type: "plain_text",
          text: cohort,
        },
      ],
    },
  };
}

test("edge deployment requires a current control plane and a subset cohort", () => {
  assert.deepEqual(
    verifyManagedCohortDeploymentOrder(
      "0.0.241",
      firstAccount,
      deployment(),
      version({ cohort: `${firstAccount},${secondAccount}` }),
    ),
    {
      required: true,
      control_plane_release: "0.0.241",
      target_account_count: 1,
      active_control_plane_account_count: 2,
    },
  );
  assert.throws(
    () => verifyManagedCohortDeploymentOrder(
      "0.0.241", firstAccount, deployment(), version({ release: "0.0.240" }),
    ),
    /requires a v0\.0\.241 or newer control plane/,
  );
  assert.throws(
    () => verifyManagedCohortDeploymentOrder(
      "0.0.241", secondAccount, deployment(), version({ cohort: firstAccount }),
    ),
    /add to the control plane first/,
  );
});

test("edge deployment preflight inspects the exact active control-plane version", () => {
  const calls = [];
  const result = preflightManagedCohortDeploymentOrder(
    "0.0.241",
    firstAccount,
    (args) => {
      calls.push(args);
      return calls.length === 2
        ? version({ cohort: firstAccount })
        : deployment();
    },
  );
  assert.equal(result.target_account_count, 1);
  assert.deepEqual(calls, [
    [
      "deployments", "status", "--name", "witself-control-plane", "--json",
      "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
    ],
    [
      "versions", "view", versionID,
      "--name", "witself-control-plane", "--json",
      "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
    ],
    [
      "deployments", "status", "--name", "witself-control-plane", "--json",
      "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
    ],
  ]);

  let raceCall = 0;
  assert.throws(
    () => preflightManagedCohortDeploymentOrder(
      "0.0.241",
      firstAccount,
      () => {
        raceCall += 1;
        if (raceCall === 1) return deployment();
        if (raceCall === 2) return version({ cohort: firstAccount });
        return { ...deployment(), id: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" };
      },
    ),
    /changed during exact provider inspection/,
  );

  let inspected = false;
  assert.deepEqual(
    preflightManagedCohortDeploymentOrder("0.0.240", "", () => {
      inspected = true;
    }),
    { required: false },
  );
  assert.equal(inspected, false);
});

test("edge deployment pins its operations lease to the production control plane", () => {
  assert.equal(
    canonicalControlPlaneOrigin("https://self.witwave.ai"),
    "https://self.witwave.ai",
  );
  assert.equal(
    canonicalControlPlaneOrigin("https://self.witwave.ai/"),
    "https://self.witwave.ai",
  );
  for (const endpoint of [
    "https://attacker.invalid",
    "https://self.witwave.ai/other",
    "http://self.witwave.ai",
    "https://user@self.witwave.ai",
    " https://self.witwave.ai",
    "",
  ]) {
    assert.throws(
      () => canonicalControlPlaneOrigin(endpoint),
      /canonical control-plane origin/,
    );
  }
});

test("edge deployment holds one private config under the pinned lease", async () => {
  const source = await readFile(
    new URL("../scripts/deploy.mjs", import.meta.url),
    "utf8",
  );
  assert.match(source, /createPrivateDeploymentConfig/);
  assert.match(source, /createProductionWorkerSourceSnapshot/);
  assert.match(source, /\{ endpoint: leaseOrigin, env: environment \}/);
  assert.match(source, /await assertFrozenInputs\(\)/);
  assert.match(source, /sourceSnapshot\?\.cleanup/);
});

test("production deployment environments scrub poisoned Wrangler and nested Node state", () => {
  const source = {
    PATH: "/safe/bin",
    CLOUDFLARE_ACCOUNT_ID: "canonical-account",
    CLOUDFLARE_API_TOKEN: "canonical-token",
    CF_ACCOUNT_ID: "wrong-account",
    CF_API_TOKEN: "wrong-token",
    CONTROL_PLANE_EDGE_TOKEN: "must-not-reach-children",
    CONTROL_PLANE_URL: "https://self.witwave.ai",
    CLOUDFLARE_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_API_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_ENV: "staging",
    CF_API_BASE_URL: "https://attacker.invalid",
    DOTENV_KEY: "dotenv://attacker.invalid",
    WRANGLER_API_ENVIRONMENT: "staging",
    WRANGLER_LOG_PATH: "/tmp/unsafe.log",
    WRANGLER_OUTPUT_FILE_DIRECTORY: "/tmp/unsafe-output",
    WRANGLER_OUTPUT_FILE_PATH: "/tmp/unsafe-output.json",
    WRANGLER_CI_OVERRIDE_NAME: "wrong-worker",
    WRANGLER_AUTH_URL: "https://attacker.invalid",
    NODE_OPTIONS: "--require attacker",
    NODE_DEBUG: "http",
    NODE_V8_COVERAGE: "/tmp/unsafe-coverage",
    SSLKEYLOGFILE: "/tmp/unsafe-tls",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid",
  };
  const environments = productionDeploymentEnvironments(
    source,
    "https://self.witwave.ai",
  );
  for (const environment of Object.values(environments)) {
    assert.equal(environment.CLOUDFLARE_ACCOUNT_ID, "canonical-account");
    assert.equal(environment.CLOUDFLARE_API_TOKEN, "canonical-token");
    for (const unsafe of [
      "CF_ACCOUNT_ID",
      "CF_API_TOKEN",
      "CONTROL_PLANE_EDGE_TOKEN",
      "CLOUDFLARE_BASE_URL",
      "CLOUDFLARE_API_BASE_URL",
      "CLOUDFLARE_ENV",
      "CF_API_BASE_URL",
      "DOTENV_KEY",
      "WRANGLER_API_ENVIRONMENT",
      "WRANGLER_LOG_PATH",
      "WRANGLER_OUTPUT_FILE_DIRECTORY",
      "WRANGLER_OUTPUT_FILE_PATH",
      "WRANGLER_CI_OVERRIDE_NAME",
      "WRANGLER_AUTH_URL",
      "NODE_OPTIONS",
      "NODE_DEBUG",
      "NODE_V8_COVERAGE",
      "SSLKEYLOGFILE",
      "WITSELF_CONTROL_PLANE",
    ]) {
      assert.equal(Object.hasOwn(environment, unsafe), false, unsafe);
    }
  }
  assert.equal(
    environments.nestedRender.CONTROL_PLANE_URL,
    "https://self.witwave.ai/",
  );
  assert.equal(
    environments.nestedAttestation.CONTROL_PLANE_URL,
    "https://self.witwave.ai/",
  );
  assert.equal(
    Object.hasOwn(environments.wranglerMutation, "CONTROL_PLANE_URL"),
    false,
  );
  assert.equal(
    Object.hasOwn(environments.wranglerInspection, "CONTROL_PLANE_URL"),
    false,
  );
});

test("normal deploy requires the exact production secret inventory", () => {
  const required = [
    { name: "CONTROL_PLANE_EDGE_TOKEN" },
    { name: "RELAY_ED25519_PRIVATE_KEY" },
  ];
  const calls = [];
  assert.equal(
    preflightProductionSecretInventory((args, operation) => {
      calls.push({ args, operation });
      return required;
    }).worker,
    "witself-agent-email-receive",
  );
  assert.deepEqual(calls[0].args, [
    "secret", "list", "--name", "witself-agent-email-receive",
    "--format", "json",
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
  assert.throws(
    () => verifyProductionSecretInventory(required.slice(0, 1)),
    /required production contract/,
  );
  for (const forbidden of [
    "AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED",
    "LEGACY_PILOT_TRUSTED_INGEST_URL",
    "LEGACY_PILOT_TRUSTED_CELL_AUDIENCE",
  ]) {
    assert.throws(
      () => verifyProductionSecretInventory([
        ...required,
        { name: forbidden },
      ]),
      new RegExp(`forbidden ${forbidden}`),
    );
  }
});

test("normal deploy targets the production Worker explicitly", () => {
  const release = {
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  };
  const args = productionDeploymentArguments(
    "/private/wrangler.jsonc",
    release,
  );
  assert.deepEqual(
    args.slice(0, 5),
    [
      "deploy", "--name", "witself-agent-email-receive",
      "--config", "/private/wrangler.jsonc",
    ],
  );
  assert.deepEqual(args.slice(-2), [
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
});

test("normal deploy validates the complete relocated production config", () => {
  const release = {
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  };
  const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
  const environment = {
    EMAIL_DIRECTORY_KV_ID: "b".repeat(32),
    RELAY_KEY_ID: "relay-2026-08",
    CONTROL_PLANE_URL: "https://self.witwave.ai/",
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: JSON.stringify({
      "route-2026-08": publicKey,
    }),
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
  };
  const configPath = "/private/config/wrangler.generated.jsonc";
  const entrypoint = "/private/source/src/index.js";
  const config = {
    name: "witself-agent-email-receive",
    main: "../source/src/index.js",
    secrets: {
      required: [
        "CONTROL_PLANE_EDGE_TOKEN",
        "RELAY_ED25519_PRIVATE_KEY",
      ],
    },
    compatibility_date: "2026-07-21",
    compatibility_flags: ["global_fetch_strictly_public"],
    workers_dev: false,
    preview_urls: false,
    kv_namespaces: [{
      binding: "EMAIL_DIRECTORY",
      id: environment.EMAIL_DIRECTORY_KV_ID,
    }],
    analytics_engine_datasets: [{
      binding: "EMAIL_EDGE_METRICS",
      dataset: "witself_agent_email_edge",
    }],
    ratelimits: [
      {
        name: "REALM_ROUTE_COLD_MISS_LIMITER",
        namespace_id: "2201",
        simple: { limit: 10, period: 10 },
      },
      {
        name: "REALM_ROUTE_KNOWN_MISS_LIMITER",
        namespace_id: "2202",
        simple: { limit: 100, period: 10 },
      },
    ],
    vars: {
      AGENT_EMAIL_DOMAIN: "witmail.net",
      AGENT_EMAIL_LEGACY_DOMAINS: "agent-mail.witwave.ai",
      RELAY_KEY_ID: environment.RELAY_KEY_ID,
      CONTROL_PLANE_URL: "https://self.witwave.ai/",
      AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS:
        environment.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS,
      AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      WITSELF_EDGE_RELEASE_VERSION: release.version,
      WITSELF_EDGE_RELEASE_COMMIT: release.commit,
      WITSELF_EDGE_RELEASE_DATE: release.date,
      REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
      REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
    },
    observability: { enabled: true },
  };
  assert.equal(
    validateProductionDeploymentConfig(
      JSON.stringify(config),
      configPath,
      entrypoint,
      release,
      environment,
    ).worker,
    "witself-agent-email-receive",
  );
  const liveSource = structuredClone(config);
  liveSource.main = "../../repo/infra/cloudflare/agent-email/src/index.js";
  assert.throws(
    () => validateProductionDeploymentConfig(
      JSON.stringify(liveSource),
      configPath,
      entrypoint,
      release,
      environment,
    ),
    /reviewed release contract/,
  );
  const extraSecret = structuredClone(config);
  extraSecret.secrets.required.push("LEGACY_PILOT_TRUSTED_INGEST_URL");
  assert.throws(
    () => validateProductionDeploymentConfig(
      JSON.stringify(extraSecret),
      configPath,
      entrypoint,
      release,
      environment,
    ),
    /reviewed release contract/,
  );
});

test("tagged production source snapshot is immutable and content-addressed", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-deploy-source-test-"));
  await chmod(directory, 0o700);
  const commit = "a".repeat(40);
  const indexOID = "b".repeat(40);
  const helperOID = "c".repeat(40);
  const content = new Map([
    [indexOID, Buffer.from("export default { email() {} };\n")],
    [helperOID, Buffer.from("export const worker = 'production';\n")],
  ]);
  const inspect = (args) => {
    if (args[0] === "ls-tree") {
      return {
        status: 0,
        stdout: Buffer.from(
          `100644 blob ${indexOID}\tinfra/cloudflare/agent-email/src/index.js\0` +
          `100644 blob ${helperOID}\tinfra/cloudflare/agent-email/src/worker-names.mjs\0`,
        ),
      };
    }
    return { status: 0, stdout: content.get(args[2]) };
  };
  try {
    const snapshot = await snapshotProductionWorkerSource(
      directory,
      commit,
      inspect,
    );
    assert.equal(snapshot.file_count, 2);
    assert.match(snapshot.sha256, /^[0-9a-f]{64}$/);
    assert.equal(
      await readFile(snapshot.entrypointTarget, "utf8"),
      "export default { email() {} };\n",
    );
    assert.equal((await stat(snapshot.entrypointTarget)).mode & 0o777, 0o400);
    await chmod(snapshot.entrypointTarget, 0o600);
    await writeFile(snapshot.entrypointTarget, "changed\n");
    await assert.rejects(
      snapshot.assertUnchanged(),
      /source snapshot changed during deployment/,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("normal production source snapshot ignores redirected Git repository environment", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-deploy-git-env-"));
  const trusted = join(directory, "trusted");
  const redirected = join(directory, "redirected");
  const gitEnvironment = { ...process.env };
  for (const name of Object.keys(gitEnvironment)) {
    if (name.startsWith("GIT_") || name === "SSH_ASKPASS" ||
        name === "GIT_ASKPASS") {
      delete gitEnvironment[name];
    }
  }
  Object.assign(gitEnvironment, {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_TERMINAL_PROMPT: "0",
  });
  const initialize = async (root, content) => {
    await mkdir(join(root, "infra/cloudflare/agent-email/src"), {
      recursive: true,
    });
    const git = (...args) => execFileSync("git", args, {
      cwd: root,
      encoding: "utf8",
      env: gitEnvironment,
    }).trim();
    git("init", "--quiet");
    git("config", "user.name", "Witself test");
    git("config", "user.email", "test@witself.invalid");
    await writeFile(
      join(root, "infra/cloudflare/agent-email/src/index.js"),
      `export default { source: ${JSON.stringify(content)} };\n`,
    );
    git("add", ".");
    execFileSync("git", ["commit", "--quiet", "-m", content], {
      cwd: root,
      env: {
        ...gitEnvironment,
        GIT_AUTHOR_DATE: "2026-08-14T12:00:00Z",
        GIT_COMMITTER_DATE: "2026-08-14T12:00:00Z",
      },
    });
    return git("rev-parse", "HEAD");
  };
  let snapshot;
  try {
    const trustedCommit = await initialize(trusted, "trusted");
    await initialize(redirected, "redirected");
    const poisonedEnvironment = {
      ...gitEnvironment,
      GIT_DIR: join(redirected, ".git"),
      GIT_WORK_TREE: redirected,
      GIT_OBJECT_DIRECTORY: join(redirected, ".git", "objects"),
    };
    snapshot = await createProductionWorkerSourceSnapshot(
      trustedCommit,
      (args) => runGitInspection(args, {
        checkout: trusted,
        environment: poisonedEnvironment,
      }),
    );
    assert.equal(
      await readFile(snapshot.entrypointTarget, "utf8"),
      'export default { source: "trusted" };\n',
    );
    await snapshot.assertUnchanged();
  } finally {
    await snapshot?.cleanup();
    await rm(directory, { recursive: true, force: true });
  }
});

test("normal production source snapshot ignores repository-local blob replacement refs", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-deploy-replace-ref-"));
  const repository = join(directory, "repository");
  const sourceDirectory = join(
    repository,
    "infra/cloudflare/agent-email/src",
  );
  const original = "export default { source: 'reviewed' };\n";
  const replacement = "export default { source: 'replaced' };\n";
  const gitEnvironment = { ...process.env };
  for (const name of Object.keys(gitEnvironment)) {
    if (name.startsWith("GIT_") || name === "SSH_ASKPASS" ||
        name === "GIT_ASKPASS") {
      delete gitEnvironment[name];
    }
  }
  Object.assign(gitEnvironment, {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_TERMINAL_PROMPT: "0",
  });
  let snapshot;
  try {
    await mkdir(sourceDirectory, { recursive: true });
    const git = (...args) => execFileSync("git", args, {
      cwd: repository,
      encoding: "utf8",
      env: gitEnvironment,
    }).trim();
    git("init", "--quiet");
    git("config", "user.name", "Witself test");
    git("config", "user.email", "test@witself.invalid");
    await writeFile(join(sourceDirectory, "index.js"), original);
    git("add", ".");
    execFileSync("git", ["commit", "--quiet", "-m", "reviewed source"], {
      cwd: repository,
      env: {
        ...gitEnvironment,
        GIT_AUTHOR_DATE: "2026-08-14T12:00:00Z",
        GIT_COMMITTER_DATE: "2026-08-14T12:00:00Z",
      },
    });
    const commit = git("rev-parse", "HEAD");
    const originalBlob = git(
      "rev-parse",
      "HEAD:infra/cloudflare/agent-email/src/index.js",
    );
    const replacementBlob = execFileSync(
      "git",
      ["hash-object", "-w", "--stdin"],
      {
        cwd: repository,
        encoding: "utf8",
        env: gitEnvironment,
        input: replacement,
      },
    ).trim();
    git("replace", originalBlob, replacementBlob);
    assert.equal(git("cat-file", "blob", originalBlob), replacement.trim());

    snapshot = await createProductionWorkerSourceSnapshot(
      commit,
      (args) => runGitInspection(args, {
        checkout: repository,
        environment: {
          ...gitEnvironment,
          GIT_NO_REPLACE_OBJECTS: "0",
        },
      }),
    );
    assert.equal(await readFile(snapshot.entrypointTarget, "utf8"), original);
    await snapshot.assertUnchanged();
  } finally {
    await snapshot?.cleanup();
    await rm(directory, { recursive: true, force: true });
  }
});

test("source drift immediately before mutation fails without calling Wrangler", async () => {
  const release = Object.freeze({
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  });
  let identityCalls = 0;
  let sourceCleaned = false;
  let configCleaned = false;
  const commands = [];
  const originalEnvironment = {
    CONTROL_PLANE_URL: "https://self.witwave.ai",
    CONTROL_PLANE_EDGE_TOKEN: "lease-only-secret",
    CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_API_TOKEN: "canonical-token",
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
  };
  await assert.rejects(
    deployProductionReceive({
      environment: originalEnvironment,
      sourceIdentity: () => {
        identityCalls += 1;
        return identityCalls < 3
          ? release
          : { ...release, clean: false };
      },
      createSourceSnapshot: async () => ({
        parentDirectory: "/private/source",
        entrypointTarget: "/private/source/src/index.js",
        file_count: 2,
        byte_count: 100,
        sha256: "b".repeat(64),
        assertUnchanged: async () => {},
        cleanup: async () => { sourceCleaned = true; },
      }),
      createConfig: async () => ({
        path: "/private/config/wrangler.jsonc",
        sha256: "c".repeat(64),
        assertUnchanged: async () => {},
        readText: async () => "{}",
        cleanup: async () => { configCleaned = true; },
      }),
      validateConfig: () => {},
      preflightCohort: () => {},
      preflightSecrets: () => {},
      runCommand: async (command, args, options) => {
        commands.push({ command, args, options });
      },
      withLease: async (operation, work, options) => {
        assert.equal(operation, "email_edge_deploy");
        assert.equal(options.env, originalEnvironment);
        return work({ signal: new AbortController().signal });
      },
    }),
    /release source changed during deployment/,
  );
  assert.equal(commands.some((call) => call.command === "wrangler"), false);
  assert.equal(commands.length, 1);
  assert.equal(
    Object.hasOwn(commands[0].options.env, "CONTROL_PLANE_EDGE_TOKEN"),
    false,
  );
  assert.equal(sourceCleaned, true);
  assert.equal(configCleaned, true);
});

test("deploy preserves the primary failure when private cleanup also fails", async () => {
  const release = Object.freeze({
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  });
  const primary = new Error("operations lease failed");
  const cleanup = new Error("private config cleanup failed");
  let sourceCleaned = false;
  await assert.rejects(
    deployProductionReceive({
      environment: {
        CONTROL_PLANE_URL: "https://self.witwave.ai",
        CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
        CLOUDFLARE_API_TOKEN: "canonical-token",
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      },
      sourceIdentity: () => release,
      createSourceSnapshot: async () => ({
        parentDirectory: "/private/source",
        entrypointTarget: "/private/source/src/index.js",
        file_count: 2,
        byte_count: 100,
        sha256: "b".repeat(64),
        assertUnchanged: async () => {},
        cleanup: async () => { sourceCleaned = true; },
      }),
      createConfig: async () => ({
        path: "/private/config/wrangler.jsonc",
        sha256: "c".repeat(64),
        assertUnchanged: async () => {},
        readText: async () => "{}",
        cleanup: async () => { throw cleanup; },
      }),
      validateConfig: () => {},
      preflightCohort: () => {},
      preflightSecrets: () => {},
      runCommand: async () => {},
      withLease: async () => { throw primary; },
    }),
    (error) => {
      assert.equal(error instanceof AggregateError, true);
      assert.equal(error.errors[0], primary);
      assert.equal(error.errors[1].cause, cleanup);
      assert.match(error.message, /deployment and private input cleanup failed/);
      return true;
    },
  );
  assert.equal(sourceCleaned, true);
});

test("deploy preserves both cleanup failures alongside the primary failure", async () => {
  const release = Object.freeze({
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  });
  const primary = new Error("operations lease failed");
  const configCleanup = new Error("private config cleanup failed");
  const sourceCleanup = new Error("source snapshot cleanup failed");
  await assert.rejects(
    deployProductionReceive({
      environment: {
        CONTROL_PLANE_URL: "https://self.witwave.ai",
        CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
        CLOUDFLARE_API_TOKEN: "canonical-token",
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      },
      sourceIdentity: () => release,
      createSourceSnapshot: async () => ({
        parentDirectory: "/private/source",
        entrypointTarget: "/private/source/src/index.js",
        file_count: 2,
        byte_count: 100,
        sha256: "b".repeat(64),
        assertUnchanged: async () => {},
        cleanup: async () => { throw sourceCleanup; },
      }),
      createConfig: async () => ({
        path: "/private/config/wrangler.jsonc",
        sha256: "c".repeat(64),
        assertUnchanged: async () => {},
        readText: async () => "{}",
        cleanup: async () => { throw configCleanup; },
      }),
      validateConfig: () => {},
      preflightCohort: () => {},
      preflightSecrets: () => {},
      runCommand: async () => {},
      withLease: async () => { throw primary; },
    }),
    (error) => {
      assert.equal(error instanceof AggregateError, true);
      assert.equal(error.errors[0], primary);
      assert.equal(error.errors[1] instanceof AggregateError, true);
      assert.deepEqual(error.errors[1].errors, [configCleanup, sourceCleanup]);
      return true;
    },
  );
});

test("release identity comparison rejects every drifted field", () => {
  const release = {
    version: "0.0.245",
    commit: "a".repeat(40),
    date: "2026-08-14T12:00:00Z",
    tag: "v0.0.245",
    clean: true,
  };
  assert.equal(assertProductionReleaseUnchanged(release, release), release);
  for (const field of ["version", "commit", "date", "tag", "clean"]) {
    assert.throws(
      () => assertProductionReleaseUnchanged(release, {
        ...release,
        [field]: field === "clean" ? false : "changed",
      }),
      /release source changed/,
    );
  }
});
