import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { generateKeyPairSync } from "node:crypto";
import {
  chmod,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  realpath,
  rm,
  stat,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import {
  assertBootstrapPathsOutsideRepository,
  assertBootstrapReleaseUnchanged,
  assertProductionReceiveWorkerAbsent,
  bootstrapProductionReceive,
  createBootstrapDeploymentConfig,
  createPrivateBootstrapSecrets,
  createPrivateReleaseWorkerSource,
  parseBootstrapArgs,
  runGitInspection,
  snapshotReleaseWorkerSource,
  validateBootstrapDeploymentConfig,
  validateBootstrapSecrets,
  verifyLegacyWorkerDark,
} from "../scripts/bootstrap-production-receive.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
  WRANGLER_PRODUCTION_ENV_FILE,
} from "../scripts/wrangler-environment.mjs";

function validSecrets() {
  const { privateKey } = generateKeyPairSync("ed25519");
  return {
    CONTROL_PLANE_EDGE_TOKEN: "production-edge-token-1234567890",
    RELAY_ED25519_PRIVATE_KEY: privateKey.export({
      format: "der",
      type: "pkcs8",
    }).toString("base64"),
  };
}

const release = Object.freeze({
  version: "0.0.245",
  commit: "1".repeat(40),
  date: "2026-08-14T20:00:00Z",
  tag: "v0.0.245",
  clean: true,
});

function validEnvironment(relayPublicKey = Buffer.alloc(32, 9).toString("base64")) {
  return {
    PATH: process.env.PATH,
    CONTROL_PLANE_URL: "https://self.witwave.ai/",
    CONTROL_PLANE_EDGE_TOKEN: "production-edge-token-1234567890",
    CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_ZONE_ID: "b".repeat(32),
    CLOUDFLARE_LEGACY_EMAIL_ZONE_ID: "c".repeat(32),
    EMAIL_DIRECTORY_KV_ID: "d".repeat(32),
    CLOUDFLARE_API_TOKEN: "canonical-cloudflare-api-token",
    RELAY_ED25519_PUBLIC_KEY: relayPublicKey,
    RELAY_KEY_ID: "relay-2026-08",
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: JSON.stringify({
      "route-2026-08": Buffer.alloc(32, 7).toString("base64"),
    }),
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
    CLOUDFLARE_API_BASE_URL: "https://attacker.invalid",
    CF_API_BASE_URL: "https://attacker.invalid",
    CF_ACCOUNT_ID: "e".repeat(32),
    CF_API_TOKEN: "conflicting-token",
    WRANGLER_API_ENVIRONMENT: "staging",
    WRANGLER_LOG_PATH: "/tmp/unsafe-wrangler.log",
    WRANGLER_OUTPUT_FILE_DIRECTORY: "/tmp/unsafe-output",
    WRANGLER_OUTPUT_FILE_PATH: "/tmp/unsafe-output.json",
    WRANGLER_CI_OVERRIDE_NAME: "wrong-worker",
    WRANGLER_AUTH_DOMAIN: "attacker.invalid",
    WRANGLER_AUTH_URL: "https://attacker.invalid/auth",
    WRANGLER_REVOKE_URL: "https://attacker.invalid/revoke",
    WRANGLER_TOKEN_URL: "https://attacker.invalid/token",
    NODE_OPTIONS: "--inspect",
    NODE_DEBUG: "http",
    NODE_V8_COVERAGE: "/tmp/unsafe-coverage",
    SSLKEYLOGFILE: "/tmp/unsafe-ssl-keys",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid",
    WITSELF_CONTROL_PLANE_ADDR: "https://attacker.invalid",
    WITSELF_ENDPOINT: "https://attacker.invalid",
  };
}

function renderedConfig(environment, targetRelease = release) {
  return `${JSON.stringify({
    name: "witself-agent-email-receive",
    main: "src/index.js",
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
      CONTROL_PLANE_URL: environment.CONTROL_PLANE_URL,
      AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS:
        environment.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS,
      AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      WITSELF_EDGE_RELEASE_VERSION: targetRelease.version,
      WITSELF_EDGE_RELEASE_COMMIT: targetRelease.commit,
      WITSELF_EDGE_RELEASE_DATE: targetRelease.date,
      REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
      REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
    },
    observability: { enabled: true },
  }, null, 2)}\n`;
}

function providerEvidence(contract, marker) {
  return Object.freeze({
    schema: "witself.agent-email-provider-dark.v1",
    provider_scope: Object.freeze({
      contract,
      account_id_sha256: "a".repeat(64),
      zone_id_sha256: marker.repeat(64),
      zone_name_sha256: marker.repeat(64),
      zone_identity_sha256: marker.repeat(64),
    }),
    catch_all_sha256: "d".repeat(64),
    rule_inventory_sha256: "e".repeat(64),
    rule_count: 0,
    owned_or_edge_worker_routes_enabled: false,
  });
}

function bootstrapHarness({ failAt = "", sourceSequence = [] } = {}) {
  const events = [];
  const state = {
    pending: null,
    committed: null,
    deployedEnvironment: null,
  };
  const environment = validEnvironment();
  const primaryProvider = { label: "primary" };
  const legacyProvider = { label: "legacy" };
  let sourceCalls = 0;
  const config = {
    path: "/private/frozen/source/config/wrangler.generated.jsonc",
    sha256: "4".repeat(64),
    async readText() {
      events.push("config_read");
      return "reviewed-config";
    },
    async assertUnchanged() {
      events.push("config_assert");
    },
    async cleanup() {
      events.push("config_cleanup");
    },
  };
  const sourceSnapshot = {
    parentDirectory: "/private/frozen/source",
    entrypointTarget: "/private/frozen/source/src/index.js",
    file_count: 7,
    byte_count: 12_345,
    sha256: "5".repeat(64),
    async assertUnchanged() {
      events.push("source_assert");
    },
    async cleanup() {
      events.push("source_cleanup");
    },
  };
  const secrets = {
    path: "/private/frozen/secrets.json",
    relayPublicKeyBase64: environment.RELAY_ED25519_PUBLIC_KEY,
    matchesControlPlaneToken(value) {
      return value === environment.CONTROL_PLANE_EDGE_TOKEN;
    },
    async assertUnchanged() {
      events.push("secrets_assert");
    },
    async cleanup() {
      events.push("secrets_cleanup");
    },
  };
  const dependencies = {
    repositoryRoot: "/workspace/witself",
    resolvePaths(options, checkout) {
      return Object.freeze({
        repositoryRoot: checkout,
        secretsFile: options.secretsFile,
        receipt: options.receipt,
      });
    },
    environment,
    providers: { primary: primaryProvider, legacy: legacyProvider },
    sourceIdentity() {
      events.push("source_identity");
      const value = sourceSequence[sourceCalls] ?? release;
      sourceCalls++;
      return value;
    },
    async createSecrets() {
      events.push("secrets_create");
      return secrets;
    },
    async createConfig(snapshot) {
      events.push("config_create");
      assert.equal(snapshot, sourceSnapshot);
      return config;
    },
    async createSourceSnapshot() {
      events.push("source_snapshot");
      return sourceSnapshot;
    },
    validateConfig(raw, targetRelease, receivedEnvironment) {
      events.push("config_validate");
      assert.equal(raw, "reviewed-config");
      assert.equal(targetRelease, release);
      assert.equal(receivedEnvironment, environment);
    },
    async withLease(operation, work, options) {
      events.push("lease_acquire");
      assert.equal(operation, "email_edge_deploy");
      assert.equal(options.endpoint, "https://self.witwave.ai");
      assert.equal(
        options.env.CONTROL_PLANE_EDGE_TOKEN,
        environment.CONTROL_PLANE_EDGE_TOKEN,
      );
      try {
        return await work({
          signal: new AbortController().signal,
          async renew() {
            events.push("lease_renew");
          },
          evidence() {
            events.push("lease_evidence");
            return {
              schema_version: "witself.agent-email-operations-lease-evidence.v1",
              generation: 7,
              operation: "email_edge_deploy",
            };
          },
        });
      } finally {
        events.push("lease_release");
        if (failAt === "lease_release") {
          throw new Error("simulated lease release failure");
        }
      }
    },
    assertWorkerAbsent() {
      events.push("worker_absent");
    },
    preflightCohort() {
      events.push("cohort_preflight");
    },
    inspectLegacyWorker(directoryID) {
      events.push("legacy_inspect");
      assert.equal(directoryID, environment.EMAIL_DIRECTORY_KV_ID);
      return {
        worker: "witself-agent-email-pilot",
        version_id: "legacy-version",
        managed_delivery_dark: true,
      };
    },
    async captureProvider(provider, contract) {
      events.push(`provider_capture_${provider.label}`);
      return providerEvidence(
        provider.label,
        provider.label === "primary" ? "b" : "c",
      );
    },
    validateProvider(value, expected) {
      events.push(`provider_validate_${value.provider_scope.contract}`);
      if (expected != null) {
        assert.deepEqual(value.provider_scope, expected);
      }
      return value;
    },
    reserveReceipt(path, pending) {
      events.push("receipt_reserve");
      assert.equal(path, "/private/receive-bootstrap-receipt.json");
      if (failAt === "reserve") throw new Error("simulated receipt failure");
      state.pending = pending;
      return {
        commit(receipt) {
          events.push("receipt_commit");
          state.committed = receipt;
        },
        close() {
          events.push("receipt_close");
        },
      };
    },
    async runCommand(command, args, options) {
      events.push("provider_deploy");
      assert.equal(command, "wrangler");
      assert.ok(args.includes("--secrets-file"));
      state.deployedEnvironment = options.env;
      if (failAt === "deploy") throw new Error("simulated deploy failure");
    },
    verifyProduction(options) {
      events.push("production_attest");
      assert.equal(options.release, release);
      assert.equal(options.requireAnnotations, true);
      assert.equal(typeof options.inspect, "function");
      if (failAt === "attest") throw new Error("simulated attestation failure");
      return {
        schema: "witself.agent-email-edge-attestation.v1",
        outcome: "verified",
      };
    },
    validateLeaseEvidence(evidence, operation) {
      events.push("lease_validate");
      assert.equal(evidence.operation, operation);
    },
  };
  return { dependencies, environment, events, state };
}

test("bootstrap requires one absolute secrets file and one receipt", () => {
  assert.deepEqual(
    parseBootstrapArgs([
      "--receipt", "/private/receive-receipt.json",
      "--secrets-file", "/private/receive-secrets.json",
    ]),
    {
      secretsFile: "/private/receive-secrets.json",
      receipt: "/private/receive-receipt.json",
    },
  );
  for (const argv of [
    [],
    ["--secrets-file", "relative.json", "--receipt", "/private/receipt.json"],
    ["--secrets-file", "/private/../secrets.json", "--receipt", "/private/receipt.json"],
    ["--other", "/private/secrets.json", "--receipt", "/private/receipt.json"],
    ["--secrets-file", "/private/secrets.json", "--secrets-file", "/private/other.json"],
  ]) {
    assert.throws(() => parseBootstrapArgs(argv), /usage:/);
  }
});

test("bootstrap keeps private sources and durable receipts outside the checkout", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-boundary-"));
  const checkout = join(directory, "checkout");
  const outside = join(directory, "outside");
  await Promise.all([
    mkdir(checkout),
    mkdir(outside),
  ]);
  const options = {
    secretsFile: join(outside, "receive-secrets.json"),
    receipt: join(outside, "receive-receipt.json"),
  };
  await writeFile(options.secretsFile, "{}\n", { mode: 0o600 });
  try {
    assert.deepEqual(
      await assertBootstrapPathsOutsideRepository(options, checkout),
      {
        repositoryRoot: await realpath(checkout),
        secretsFile: await realpath(options.secretsFile),
        receipt: join(await realpath(outside), "receive-receipt.json"),
      },
    );
    await assert.rejects(
      assertBootstrapPathsOutsideRepository({
        ...options,
        receipt: join(checkout, "receipt.json"),
      }, checkout),
      /receipt must be outside the repository/,
    );
    await assert.rejects(
      assertBootstrapPathsOutsideRepository({
        ...options,
        secretsFile: join(checkout, "private", "secrets.json"),
      }, checkout),
      /paths could not be resolved physically|secrets file must be outside the repository/,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap rejects an existing secrets path whose symlinked ancestor enters the checkout", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-secret-link-"));
  const checkout = join(directory, "checkout");
  const privateDirectory = join(checkout, "private");
  const outside = join(directory, "outside");
  await Promise.all([
    mkdir(privateDirectory, { recursive: true }),
    mkdir(outside),
  ]);
  const linkedDirectory = join(outside, "linked-private");
  await symlink(privateDirectory, linkedDirectory, "dir");
  await writeFile(join(privateDirectory, "secrets.json"), "{}\n", {
    mode: 0o600,
  });
  try {
    await assert.rejects(
      assertBootstrapPathsOutsideRepository({
        secretsFile: join(linkedDirectory, "secrets.json"),
        receipt: join(outside, "receipt.json"),
      }, checkout),
      /secrets file must be outside the repository/,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap rejects a new receipt whose symlinked parent enters the checkout", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-receipt-link-"));
  const checkout = join(directory, "checkout");
  const receiptDirectory = join(checkout, "receipts");
  const outside = join(directory, "outside");
  await Promise.all([
    mkdir(receiptDirectory, { recursive: true }),
    mkdir(outside),
  ]);
  const linkedDirectory = join(outside, "linked-receipts");
  await symlink(receiptDirectory, linkedDirectory, "dir");
  const secretsFile = join(outside, "secrets.json");
  await writeFile(secretsFile, "{}\n", { mode: 0o600 });
  try {
    await assert.rejects(
      assertBootstrapPathsOutsideRepository({
        secretsFile,
        receipt: join(linkedDirectory, "receipt.json"),
      }, checkout),
      /receipt must be outside the repository/,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap Git inspection ignores redirected repository environment", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-git-env-"));
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
    await mkdir(root);
    const git = (...args) => execFileSync("git", args, {
      cwd: root,
      encoding: "utf8",
      env: gitEnvironment,
    }).trim();
    git("init", "--quiet");
    git("config", "user.name", "Witself test");
    git("config", "user.email", "test@witself.invalid");
    await writeFile(join(root, "tracked.txt"), `${content}\n`);
    git("add", "tracked.txt");
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
  try {
    const trustedCommit = await initialize(trusted, "trusted");
    await initialize(redirected, "redirected");
    const result = runGitInspection(["rev-parse", "HEAD"], {
      checkout: trusted,
      environment: {
        ...gitEnvironment,
        GIT_DIR: join(redirected, ".git"),
        GIT_WORK_TREE: redirected,
        GIT_OBJECT_DIRECTORY: join(redirected, ".git", "objects"),
      },
    });
    assert.equal(result.status, 0);
    assert.equal(result.stdout.toString("utf8").trim(), trustedCommit);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap requires exactly a token and canonical Ed25519 PKCS8 key", () => {
  const values = validSecrets();
  assert.deepEqual(validateBootstrapSecrets(JSON.stringify(values)), values);
  for (const candidate of [
    "not-json",
    JSON.stringify({ CONTROL_PLANE_EDGE_TOKEN: values.CONTROL_PLANE_EDGE_TOKEN }),
    JSON.stringify({ ...values, EXTRA: "unexpected" }),
    JSON.stringify({ ...values, CONTROL_PLANE_EDGE_TOKEN: "too-short" }),
    JSON.stringify({ ...values, CONTROL_PLANE_EDGE_TOKEN: " token-with-spaces " }),
    JSON.stringify({
      ...values,
      RELAY_ED25519_PRIVATE_KEY: Buffer.from("not-an-ed25519-key").toString("base64"),
    }),
  ]) {
    assert.throws(() => validateBootstrapSecrets(candidate), /bootstrap/);
  }
});

test("bootstrap freezes a mode-0400 secret snapshot and removes it", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-source-"));
  const source = join(directory, "secrets.json");
  const values = validSecrets();
  await writeFile(source, `${JSON.stringify(values)}\n`, { mode: 0o600 });
  await chmod(source, 0o600);
  const snapshot = await createPrivateBootstrapSecrets(source);
  try {
    assert.equal((await stat(snapshot.path)).mode & 0o777, 0o400);
    assert.deepEqual(
      JSON.parse(await readFile(snapshot.path, "utf8")),
      values,
    );
    assert.match(snapshot.relayPublicKeyBase64, /^[A-Za-z0-9+/]{43}=$/);
    assert.equal(
      snapshot.matchesControlPlaneToken(values.CONTROL_PLANE_EDGE_TOKEN),
      true,
    );
    assert.equal(snapshot.matchesControlPlaneToken("not-the-same-token"), false);
    await snapshot.assertUnchanged();
  } finally {
    await snapshot.cleanup();
    await rm(directory, { recursive: true, force: true });
  }
  await assert.rejects(stat(snapshot.path), /ENOENT/);
});

test("bootstrap zeroes staged secret bytes and reports incomplete post-write cleanup", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-secret-failure-"));
  const source = join(directory, "secrets.json");
  const values = validSecrets();
  await writeFile(source, `${JSON.stringify(values)}\n`, { mode: 0o600 });
  let snapshotDirectory = "";
  let stagedBytes;
  try {
    await assert.rejects(
      createPrivateBootstrapSecrets(source, {
        async mkdtemp(prefix) {
          snapshotDirectory = await mkdtemp(prefix);
          return snapshotDirectory;
        },
        async writeFile(path, bytes, options) {
          stagedBytes = bytes;
          await writeFile(path, bytes, options);
          throw new Error("simulated post-write failure");
        },
        async rm() {
          throw new Error("simulated secret cleanup failure");
        },
      }),
      (error) => {
        assert.equal(error instanceof AggregateError, true);
        assert.match(error.message, /creation failed and cleanup was incomplete/);
        assert.deepEqual(
          error.errors.map((item) => item.message),
          ["simulated post-write failure", "simulated secret cleanup failure"],
        );
        return true;
      },
    );
    assert.ok(Buffer.isBuffer(stagedBytes));
    assert.equal(stagedBytes.every((byte) => byte === 0), true);
    await stat(join(snapshotDirectory, "secrets.json"));
  } finally {
    if (snapshotDirectory !== "") {
      await rm(snapshotDirectory, { recursive: true, force: true });
    }
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap requires the retired Worker to remain dark on shared resources", () => {
  const deploymentID = "11111111-1111-4111-8111-111111111111";
  const versionID = "22222222-2222-4222-8222-222222222222";
  const directoryID = "a".repeat(32);
  const plain = (name, text) => ({ name, text, type: "plain_text" });
  const deployment = {
    id: deploymentID,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
  const version = {
    id: versionID,
    resources: {
      script: { etag: "legacy-email-script-etag-123456", handlers: ["email"] },
      bindings: [
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
        plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
        plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false"),
        plain("WITSELF_EDGE_RELEASE_VERSION", "0.0.243"),
        { name: "CONTROL_PLANE_EDGE_TOKEN", type: "secret_text" },
        { name: "RELAY_ED25519_PRIVATE_KEY", type: "secret_text" },
        { name: "EMAIL_DIRECTORY", namespace_id: directoryID, type: "kv_namespace" },
        {
          name: "EMAIL_EDGE_METRICS",
          dataset: "witself_agent_email_edge",
          type: "analytics_engine",
        },
        {
          name: "REALM_ROUTE_COLD_MISS_LIMITER",
          namespace_id: "2201",
          simple: { limit: 10, period: 10 },
          type: "ratelimit",
        },
        {
          name: "REALM_ROUTE_KNOWN_MISS_LIMITER",
          namespace_id: "2202",
          simple: { limit: 100, period: 10 },
          type: "ratelimit",
        },
      ],
    },
  };
  const secrets = [
    { name: "CONTROL_PLANE_EDGE_TOKEN", type: "secret_text" },
    { name: "RELAY_ED25519_PRIVATE_KEY", type: "secret_text" },
  ];
  const result = verifyLegacyWorkerDark(
    deployment,
    version,
    secrets,
    directoryID,
  );
  assert.equal(result.worker, "witself-agent-email-pilot");
  assert.equal(result.managed_delivery_dark, true);

  const active = structuredClone(version);
  active.resources.bindings.find((binding) =>
    binding.name === "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED").text = "true";
  assert.throws(
    () => verifyLegacyWorkerDark(deployment, active, secrets, directoryID),
    /not dark and stable/,
  );

  for (const name of [
    "LEGACY_PILOT_TRUSTED_INGEST_URL",
    "LEGACY_PILOT_TRUSTED_CELL_AUDIENCE",
  ]) {
    const trusted = structuredClone(version);
    trusted.resources.bindings.push(plain(name, "still-live"));
    assert.throws(
      () => verifyLegacyWorkerDark(deployment, trusted, secrets, directoryID),
      /delivery trust was not fully dark/,
    );
  }
});

test("bootstrap distinguishes an absent Worker from provider failures", () => {
  assert.doesNotThrow(() => assertProductionReceiveWorkerAbsent(() => ({
    status: 1,
    stdout: "",
    stderr: "This Worker does not exist on your account. [code: 10007]",
  })));
  assert.throws(
    () => assertProductionReceiveWorkerAbsent(() => ({
      status: 0,
      stdout: "{}",
      stderr: "",
    })),
    /already exists/,
  );
  assert.throws(
    () => assertProductionReceiveWorkerAbsent(() => ({
      status: 1,
      stdout: "",
      stderr: "authentication failed [code: 10000]",
    })),
    /could not prove/,
  );
});

test("bootstrap freezes the exact tagged Worker source with immutable evidence", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-source-tree-"));
  await chmod(directory, 0o700);
  const blobs = new Map([
    ["a".repeat(40), Buffer.from("export default {};\n")],
    ["b".repeat(40), Buffer.from("export const relay = true;\n")],
  ]);
  const inspect = (args) => {
    if (args[0] === "ls-tree") {
      return {
        status: 0,
        stdout: Buffer.from([
          `100644 blob ${"a".repeat(40)}\tinfra/cloudflare/agent-email/src/index.js`,
          `100644 blob ${"b".repeat(40)}\tinfra/cloudflare/agent-email/src/relay.mjs`,
          "",
        ].join("\0")),
        stderr: Buffer.alloc(0),
      };
    }
    return {
      status: 0,
      stdout: blobs.get(args[2]),
      stderr: Buffer.alloc(0),
    };
  };
  try {
    const evidence = await snapshotReleaseWorkerSource(
      directory,
      release.commit,
      inspect,
    );
    assert.equal(evidence.file_count, 2);
    assert.equal(evidence.byte_count, [...blobs.values()]
      .reduce((sum, value) => sum + value.byteLength, 0));
    assert.match(evidence.sha256, /^[0-9a-f]{64}$/);
    assert.equal((await stat(join(directory, "src", "index.js"))).mode & 0o777, 0o400);
    await evidence.assertUnchanged();
    await chmod(join(directory, "src", "index.js"), 0o600);
    await assert.rejects(evidence.assertUnchanged(), /source snapshot changed/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("bootstrap default factories isolate source from the frozen Wrangler config", async () => {
  const environment = validEnvironment();
  const blobs = new Map([
    ["a".repeat(40), Buffer.from("export default {};\n")],
    ["b".repeat(40), Buffer.from("export const relay = true;\n")],
  ]);
  const inspect = (args) => {
    if (args[0] === "ls-tree") {
      return {
        status: 0,
        stdout: Buffer.from([
          `100644 blob ${"a".repeat(40)}\tinfra/cloudflare/agent-email/src/index.js`,
          `100644 blob ${"b".repeat(40)}\tinfra/cloudflare/agent-email/src/relay.mjs`,
          "",
        ].join("\0")),
        stderr: Buffer.alloc(0),
      };
    }
    return {
      status: 0,
      stdout: blobs.get(args[2]),
      stderr: Buffer.alloc(0),
    };
  };
  const source = await createPrivateReleaseWorkerSource(
    release.commit,
    inspect,
  );
  let config;
  try {
    config = await createBootstrapDeploymentConfig(
      source,
      environment,
      async (command, args, options) => {
        assert.equal(command, process.execPath);
        assert.equal(options.env, environment);
        const output = args[args.indexOf("--output") + 1];
        await writeFile(output, renderedConfig(environment), { mode: 0o600 });
      },
    );
    assert.deepEqual(
      (await readdir(dirname(config.path))).sort(),
      [".wrangler", "wrangler.generated.jsonc"],
    );
    const raw = await config.readText();
    assert.equal(JSON.parse(raw).main, "../src/index.js");
    assert.doesNotThrow(() => validateBootstrapDeploymentConfig(
      raw,
      release,
      environment,
      config.path,
      source.entrypointTarget,
    ));
    await Promise.all([
      config.assertUnchanged(),
      source.assertUnchanged(),
    ]);
  } finally {
    await config?.cleanup();
    await source.cleanup();
  }
});

test("bootstrap validates the complete frozen production Worker configuration", () => {
  const environment = validEnvironment();
  assert.deepEqual(
    validateBootstrapDeploymentConfig(
      renderedConfig(environment),
      release,
      environment,
    ),
    {
      worker: "witself-agent-email-receive",
      release: {
        version: release.version,
        commit: release.commit,
        date: release.date,
      },
      directory_namespace_id: environment.EMAIL_DIRECTORY_KV_ID,
    },
  );
  const unsafe = JSON.parse(renderedConfig(environment));
  unsafe.vars.LEGACY_PILOT_TRUSTED_INGEST_URL = "https://legacy.invalid";
  assert.throws(
    () => validateBootstrapDeploymentConfig(
      JSON.stringify(unsafe),
      release,
      environment,
    ),
    /plain binding contract was invalid/,
  );
  assert.throws(
    () => assertBootstrapReleaseUnchanged(release, {
      ...release,
      clean: false,
    }),
    /release source changed/,
  );
});

test("production bootstrap journals, deploys, attests, and cleans up end to end", async () => {
  const harness = bootstrapHarness();
  const receipt = await bootstrapProductionReceive({
    secretsFile: "/private/receive-bootstrap-secrets.json",
    receipt: "/private/receive-bootstrap-receipt.json",
  }, harness.dependencies);
  assert.equal(receipt.outcome, "bootstrapped");
  assert.equal(receipt.worker, "witself-agent-email-receive");
  assert.equal(receipt.source_sha256, "5".repeat(64));
  assert.equal(receipt.source_file_count, 7);
  assert.deepEqual(Object.keys(receipt.provider).sort(), ["legacy", "primary"]);
  assert.equal(harness.state.pending.outcome, "pending");
  assert.equal(harness.state.pending.recovery, "reconcile_dark_live_state_before_retry");
  assert.equal(harness.state.committed, receipt);

  const reserved = harness.events.indexOf("receipt_reserve");
  const deployed = harness.events.indexOf("provider_deploy");
  const attested = harness.events.indexOf("production_attest");
  const leaseReleased = harness.events.indexOf("lease_release");
  const committed = harness.events.indexOf("receipt_commit");
  assert.ok(reserved >= 0 && deployed > reserved && attested > deployed);
  assert.ok(committed > leaseReleased);
  assert.equal(harness.events.filter((event) =>
    event === "legacy_inspect").length, 2);
  assert.equal(harness.events.filter((event) =>
    event === "provider_capture_primary").length, 2);
  assert.equal(harness.events.filter((event) =>
    event === "provider_capture_legacy").length, 2);
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));

  const deployedEnvironment = harness.state.deployedEnvironment;
  assert.equal(
    deployedEnvironment.CLOUDFLARE_API_TOKEN,
    harness.environment.CLOUDFLARE_API_TOKEN,
  );
  for (const name of [
    "CF_ACCOUNT_ID",
    "CF_API_TOKEN",
    "CONTROL_PLANE_EDGE_TOKEN",
    "CONTROL_PLANE_URL",
    "CLOUDFLARE_API_BASE_URL",
    "CF_API_BASE_URL",
    "WRANGLER_API_ENVIRONMENT",
    "WRANGLER_LOG_PATH",
    "WRANGLER_OUTPUT_FILE_DIRECTORY",
    "WRANGLER_OUTPUT_FILE_PATH",
    "WRANGLER_CI_OVERRIDE_NAME",
    "WRANGLER_AUTH_DOMAIN",
    "WRANGLER_AUTH_URL",
    "WRANGLER_REVOKE_URL",
    "WRANGLER_TOKEN_URL",
    "NODE_OPTIONS",
    "NODE_DEBUG",
    "NODE_V8_COVERAGE",
    "SSLKEYLOGFILE",
    "WITSELF_CONTROL_PLANE",
    "WITSELF_CONTROL_PLANE_ADDR",
    "WITSELF_ENDPOINT",
  ]) {
    assert.equal(Object.hasOwn(deployedEnvironment, name), false, name);
  }
  assert.equal(deployedEnvironment.WRANGLER_WRITE_LOGS, "false");
  assert.equal(deployedEnvironment.WRANGLER_LOG_SANITIZE, "true");
});

test("production bootstrap preserves the reviewed cohort preflight argument handoff", async () => {
  const directory = await mkdtemp(join(tmpdir(), "witself-bootstrap-wrangler-"));
  const executable = join(directory, "wrangler");
  const callLog = join(directory, "calls.jsonl");
  const harness = bootstrapHarness();
  delete harness.dependencies.preflightCohort;
  harness.dependencies.inspectLegacyWorker = () => {
    throw new Error("stop after cohort preflight");
  };
  harness.environment.PATH = `${directory}:${process.env.PATH}`;
  const deployment = {
    id: "11111111-1111-4111-8111-111111111111",
    strategy: "percentage",
    versions: [{
      version_id: "22222222-2222-4222-8222-222222222222",
      percentage: 100,
    }],
  };
  const version = {
    id: "22222222-2222-4222-8222-222222222222",
    resources: {
      bindings: [
        {
          name: "WITSELF_EDGE_RELEASE_VERSION",
          type: "plain_text",
          text: release.version,
        },
        {
          name: "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
          type: "plain_text",
          text: "",
        },
      ],
    },
  };
  const fakeWrangler = `#!/usr/bin/env node
const { appendFileSync } = require("node:fs");
const args = process.argv.slice(2);
appendFileSync(${JSON.stringify(callLog)}, JSON.stringify(args) + "\\n");
const output = args[0] === "versions"
  ? ${JSON.stringify(JSON.stringify(version))}
  : ${JSON.stringify(JSON.stringify(deployment))};
process.stdout.write(output);
`;
  try {
    await writeFile(executable, fakeWrangler, { flag: "wx", mode: 0o700 });
    await assert.rejects(
      bootstrapProductionReceive({
        secretsFile: "/private/receive-bootstrap-secrets.json",
        receipt: "/private/receive-bootstrap-receipt.json",
      }, harness.dependencies),
      /stop after cohort preflight/,
    );
    const calls = (await readFile(callLog, "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    assert.equal(calls.length, 3);
    for (const args of calls) {
      assert.equal(args.filter((arg) => arg === "--env-file").length, 1);
      assert.deepEqual(args.slice(-2), [
        "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
      ]);
    }
    assert.deepEqual(calls.map((args) => args[0]), [
      "deployments", "versions", "deployments",
    ]);
    assert.equal(harness.events.includes("provider_deploy"), false);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("production bootstrap performs no provider mutation when receipt reservation fails", async () => {
  const harness = bootstrapHarness({ failAt: "reserve" });
  await assert.rejects(
    bootstrapProductionReceive({
      secretsFile: "/private/receive-bootstrap-secrets.json",
      receipt: "/private/receive-bootstrap-receipt.json",
    }, harness.dependencies),
    /simulated receipt failure/,
  );
  assert.equal(harness.events.includes("provider_deploy"), false);
  assert.equal(harness.events.includes("production_attest"), false);
  assert.equal(harness.events.includes("receipt_commit"), false);
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));
});

test("production bootstrap refuses a temporary config that could dirty the release checkout", async () => {
  const harness = bootstrapHarness();
  const createConfig = harness.dependencies.createConfig;
  harness.dependencies.createConfig = async (snapshot) => ({
    ...await createConfig(snapshot),
    path: "/workspace/witself/infra/cloudflare/private-config.jsonc",
  });
  await assert.rejects(
    bootstrapProductionReceive({
      secretsFile: "/private/receive-bootstrap-secrets.json",
      receipt: "/private/receive-bootstrap-receipt.json",
    }, harness.dependencies),
    /configuration snapshot must be outside the repository/,
  );
  assert.equal(harness.events.includes("lease_acquire"), false);
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));
});

test("production bootstrap retains pending evidence on post-mutation failure", async () => {
  const harness = bootstrapHarness({ failAt: "attest" });
  await assert.rejects(
    bootstrapProductionReceive({
      secretsFile: "/private/receive-bootstrap-secrets.json",
      receipt: "/private/receive-bootstrap-receipt.json",
    }, harness.dependencies),
    /simulated attestation failure/,
  );
  assert.ok(harness.events.includes("provider_deploy"));
  assert.ok(harness.events.includes("receipt_close"));
  assert.equal(harness.events.includes("receipt_commit"), false);
  assert.equal(harness.state.pending.outcome, "pending");
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));
});

test("production bootstrap does not finalize before lease release succeeds", async () => {
  const harness = bootstrapHarness({ failAt: "lease_release" });
  await assert.rejects(
    bootstrapProductionReceive({
      secretsFile: "/private/receive-bootstrap-secrets.json",
      receipt: "/private/receive-bootstrap-receipt.json",
    }, harness.dependencies),
    /simulated lease release failure/,
  );
  assert.ok(harness.events.includes("provider_deploy"));
  assert.ok(harness.events.includes("receipt_close"));
  assert.equal(harness.events.includes("receipt_commit"), false);
  assert.equal(harness.state.pending.outcome, "pending");
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));
});

test("production bootstrap rejects release drift before journaling or mutation", async () => {
  const harness = bootstrapHarness({
    sourceSequence: [
      release,
      release,
      release,
      { ...release, clean: false },
    ],
  });
  await assert.rejects(
    bootstrapProductionReceive({
      secretsFile: "/private/receive-bootstrap-secrets.json",
      receipt: "/private/receive-bootstrap-receipt.json",
    }, harness.dependencies),
    /release source changed/,
  );
  assert.equal(harness.events.includes("receipt_reserve"), false);
  assert.equal(harness.events.includes("provider_deploy"), false);
  assert.ok(harness.events.includes("config_cleanup"));
  assert.ok(harness.events.includes("secrets_cleanup"));
});
