import assert from "node:assert/strict";
import { generateKeyPairSync } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import {
  assertReceiptAvailable,
  parseJSONC,
  parseProvisioningArgs,
  provisionRouteSigningSecrets,
  sanitizedWitselfEnvironment,
  sanitizedWranglerInspectionEnvironment,
  sanitizedWranglerEnvironment,
  validateFallbackToken,
  validateProvisioningConfigs,
  verifyEd25519Keypair,
} from "../scripts/provision-route-signing-secrets.mjs";
import { reserveJSONReceipt } from "../scripts/receipt-journal.mjs";

const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-receive";
const CP_CONFIG = "/safe/control-plane.jsonc";
const EDGE_CONFIG = "/safe/email-edge.jsonc";
const routeSecretID = "sec_aaaaaaaaaaaaaaaa";
const privateFieldID = "fld_bbbbbbbbbbbbbbbb";
const publicFieldID = "fld_cccccccccccccccc";
const tokenSecretID = "sec_dddddddddddddddd";
const tokenFieldID = "fld_eeeeeeeeeeeeeeee";
const cpDeploymentID = "11111111-2222-4333-8444-555555555555";
const cpVersionID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const edgeDeploymentID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const edgeVersionID = "01234567-89ab-4cde-8f01-23456789abcd";
const token = "C3dfjS1m6fM9-7QpL2vNx8ZrK4aW0uHyB5eTgDqU";
const leaseOperation = "route_signing_secret_provision";
const leaseEvidence = Object.freeze({
  schema_version: "witself.agent-email-operations-lease-evidence.v1",
  generation: 7,
  operation: leaseOperation,
});

function productionEnvironment() {
  return {
    CLOUDFLARE_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
    CLOUDFLARE_API_TOKEN: "production-provider-token",
  };
}

function keyMaterial() {
  const pair = generateKeyPairSync("ed25519");
  const privateDER = pair.privateKey.export({ format: "der", type: "pkcs8" });
  const publicDER = pair.publicKey.export({ format: "der", type: "spki" });
  return {
    privateKey: privateDER.toString("base64"),
    publicKey: publicDER.subarray(publicDER.length - 32).toString("base64"),
  };
}

function configs(publicKey) {
  return {
    [CP_CONFIG]: `{
      // The comment and trailing commas prove JSONC handling.
      "name": "${CONTROL_PLANE_WORKER}",
      "secrets": {"required": [
        "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY",
        "CONTROL_PLANE_EDGE_TOKEN",
      ]},
      "vars": {
        "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID": "route-2026-08",
        "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST": ""
      },
    }`,
    [EDGE_CONFIG]: JSON.stringify({
      name: EMAIL_EDGE_WORKER,
      secrets: {
        required: ["CONTROL_PLANE_EDGE_TOKEN", "RELAY_ED25519_PRIVATE_KEY"],
      },
      vars: {
        AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: JSON.stringify({
          "route-2026-08": publicKey,
        }),
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
        REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
        REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
      },
    }),
  };
}

function deployment(id, versionID) {
  return {
    id,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function version(id, bindings) {
  return { id, resources: { bindings } };
}

function plain(name, text) {
  return { name, type: "plain_text", text };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function showSecret({ id, name, fields }) {
  return {
    secret: {
      id,
      name,
      lifecycle: "active",
      row_version: 1,
      fields,
    },
  };
}

function sensitiveField(id, name) {
  return {
    id,
    name,
    kind: "api_key",
    sensitive: true,
    encoding: "utf8",
    redacted: true,
  };
}

function publicField(id, name, value) {
  return {
    id,
    name,
    kind: "public_key",
    sensitive: false,
    encoding: "utf8",
    redacted: false,
    public_value: value,
  };
}

function reveal(secretID, field, value) {
  return {
    secret_id: secretID,
    field_id: field.id,
    field_name: field.name,
    encoding: "utf8",
    value,
  };
}

function options(receipt = "/private/route-receipt.json") {
  return parseProvisioningArgs([
    "--control-plane-config", CP_CONFIG,
    "--email-edge-config", EDGE_CONFIG,
    "--account", "default",
    "--realm", "default",
    "--agent", "scott",
    "--route-secret", "route-signing",
    "--route-private-field", "private-key",
    "--route-public-field", "public-key",
    "--fallback-secret", "edge-token",
    "--fallback-field", "token",
    "--receipt", receipt,
  ], {});
}

function runtimeFixture({
  material = keyMaterial(),
  fallbackToken = token,
  mutateJSON = () => {},
  leaseCollision = false,
  renewalFailureAt = 0,
  releaseFailure = false,
  realReceipt = "",
  mutateOriginalSourcesAfterSnapshot = false,
  mutateFrozenSnapshot = false,
} = {}) {
  const files = configs(material.publicKey);
  const privateField = sensitiveField(privateFieldID, "private-key");
  const routePublicField = publicField(
    publicFieldID,
    "public-key",
    material.publicKey,
  );
  const fallbackField = sensitiveField(tokenFieldID, "token");
  const calls = [];
  const puts = [];
  const leaseEvents = [];
  const snapshotPaths = new Set();
  const snapshotMetadata = [];
  let originalSourcesMutated = false;
  let frozenSnapshotMutated = false;

  function response(command, args) {
    if (command === "wrangler") {
      const worker = args[args.indexOf("--name") + 1];
      if (args[0] === "deployments") {
        return worker === CONTROL_PLANE_WORKER
          ? deployment(cpDeploymentID, cpVersionID)
          : deployment(edgeDeploymentID, edgeVersionID);
      }
      if (args[0] === "versions") {
        return worker === CONTROL_PLANE_WORKER
          ? version(cpVersionID, puts.length >= 3 ? [
            plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
            secret("AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"),
            secret("CONTROL_PLANE_EDGE_TOKEN"),
          ] : [plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", "")])
          : version(edgeVersionID, [
            plain("CONTROL_PLANE_URL", "https://self.witwave.ai/"),
            plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
            plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
            plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false"),
            secret("RELAY_ED25519_PRIVATE_KEY"),
            ...(puts.length >= 3 ? [secret("CONTROL_PLANE_EDGE_TOKEN")] : []),
          ]);
      }
      if (args[0] === "secret" && args[1] === "list") {
        return worker === CONTROL_PLANE_WORKER
          ? [
            { name: "EXISTING_CP_SECRET" },
            ...(puts.length >= 3 ? [
              { name: "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY" },
              { name: "CONTROL_PLANE_EDGE_TOKEN" },
            ] : []),
          ]
          : [
            { name: "RELAY_ED25519_PRIVATE_KEY" },
            ...(puts.length >= 3
              ? [{ name: "CONTROL_PLANE_EDGE_TOKEN" }]
              : []),
          ];
      }
    }
    if (command === "witself" && args[1] === "show") {
      if (args[2] === "route-signing") {
        return showSecret({
          id: routeSecretID,
          name: "route-signing",
          fields: [privateField, routePublicField],
        });
      }
      return showSecret({
        id: tokenSecretID,
        name: "edge-token",
        fields: [fallbackField],
      });
    }
    if (command === "witself" && args[1] === "reveal") {
      return args[2] === "route-signing"
        ? reveal(routeSecretID, privateField, material.privateKey)
        : reveal(tokenSecretID, fallbackField, fallbackToken);
    }
    throw new Error(`unexpected fake command ${command} ${args.join(" ")}`);
  }

  const runtime = {
    readText(path) {
      calls.push({ type: "read", path });
      if (!(path in files)) throw new Error("unexpected config path");
      return files[path];
    },
    json(command, args, runOptions) {
      calls.push({ type: "json", command, args: [...args], runOptions });
      const configIndex = args.indexOf("--config");
      if (command === "wrangler" && configIndex >= 0) {
        const configPath = args[configIndex + 1];
        if (!snapshotPaths.has(configPath)) {
          snapshotPaths.add(configPath);
          snapshotMetadata.push({
            path: configPath,
            fileMode: statSync(configPath).mode & 0o777,
            directoryMode: statSync(dirname(configPath)).mode & 0o777,
            contents: readFileSync(configPath, "utf8"),
          });
        }
        if (mutateOriginalSourcesAfterSnapshot && !originalSourcesMutated) {
          originalSourcesMutated = true;
          files[CP_CONFIG] = "{broken original control-plane config";
          files[EDGE_CONFIG] = "{broken original email-edge config";
        }
        if (mutateFrozenSnapshot && !frozenSnapshotMutated) {
          frozenSnapshotMutated = true;
          chmodSync(configPath, 0o600);
          writeFileSync(configPath, "{mutated frozen config", { mode: 0o600 });
          chmodSync(configPath, 0o400);
        }
      }
      const value = structuredClone(response(command, args));
      mutateJSON({ command, args, value });
      return value;
    },
    secretPut(command, args, value, runOptions) {
      calls.push({ type: "put", command, args: [...args], runOptions });
      puts.push({
        command,
        args: [...args],
        value: Buffer.from(value),
        runOptions,
      });
    },
    assertReceiptAvailable(path) {
      calls.push({ type: "receipt-preflight", path });
      if (realReceipt) assertReceiptAvailable(path);
    },
    reserveReceipt(path, pending) {
      calls.push({ type: "receipt-reserve", path, pending });
      if (realReceipt) {
        const journal = reserveJSONReceipt(path, pending);
        return {
          commit(receipt) {
            calls.push({ type: "receipt-commit", path, receipt });
            journal.commit(receipt);
          },
          close() {
            calls.push({ type: "receipt-close", path });
            journal.close();
          },
        };
      }
      let settled = false;
      return {
        commit(receipt) {
          if (settled) throw new Error("journal already settled");
          settled = true;
          calls.push({ type: "receipt-commit", path, receipt });
        },
        close() {
          settled = true;
          calls.push({ type: "receipt-close", path });
        },
      };
    },
  };
  const withLease = async (operation, work, leaseOptions) => {
    leaseEvents.push({
      type: "acquire",
      operation,
      endpoint: leaseOptions?.endpoint,
      token_matched: leaseOptions?.token === fallbackToken,
    });
    if (leaseCollision) {
      throw new Error("another agent email operation already holds the lease");
    }
    let renewalCount = 0;
    try {
      return await work({
        renew: async () => {
          renewalCount += 1;
          leaseEvents.push({ type: "renew", count: renewalCount });
          if (renewalCount === renewalFailureAt) {
            throw new Error("agent email operations lease renewal failed");
          }
        },
        evidence: () => leaseEvidence,
      });
    } finally {
      leaseEvents.push({ type: "release" });
      calls.push({ type: "lease-release" });
      if (releaseFailure) {
        throw new Error("agent email operations lease release failed");
      }
    }
  };
  return {
    runtime,
    calls,
    puts,
    material,
    files,
    withLease,
    leaseEvents,
    snapshotPaths,
    snapshotMetadata,
    environment: productionEnvironment(),
  };
}

test("provisions verified values only after complete dark preflight and both reveals", async () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-route-success-"));
  const receiptPath = join(directory, "receipt.json");
  const fixture = runtimeFixture({ realReceipt: receiptPath });
  const environment = {
    CLOUDFLARE_API_TOKEN: "existing-auth",
    CLOUDFLARE_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
    CF_ACCOUNT_ID: "f".repeat(32),
    CF_API_TOKEN: "conflicting-auth",
    CONTROL_PLANE_EDGE_TOKEN: "must-not-reach-wrangler",
    CONTROL_PLANE_URL: "https://attacker.invalid/",
    CLOUDFLARE_API_BASE_URL: "https://attacker.invalid",
    CF_API_BASE_URL: "https://attacker.invalid",
    WRANGLER_API_ENVIRONMENT: "staging",
    WRANGLER_LOG_PATH: "/tmp/unsafe.log",
    WRANGLER_OUTPUT_FILE_DIRECTORY: "/tmp/unsafe-output",
    WRANGLER_OUTPUT_FILE_PATH: "/tmp/unsafe-output.json",
    WRANGLER_CI_OVERRIDE_NAME: "wrong-worker",
    WRANGLER_AUTH_URL: "https://attacker.invalid",
    NODE_OPTIONS: "--require attacker",
    NODE_DEBUG: "http",
    NODE_V8_COVERAGE: "/tmp/unsafe-coverage",
    SSLKEYLOGFILE: "/tmp/unsafe-tls-keys",
    WITSELF_ENDPOINT: "https://attacker.invalid",
    WITSELF_TOKEN: "must-not-reach-child",
    WITSELF_TOKEN_FILE: "/tmp/unsafe-token",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid",
    WITSELF_FAKE_PROVIDER_LOG: "/tmp/unsafe-witself.log",
    WITSELF_HOME: "/safe/witself-home",
  };
  const receipt = await provisionRouteSigningSecrets(options(receiptPath), {
    runtime: fixture.runtime,
    environment,
    withLease: fixture.withLease,
  });

  assert.equal(receipt.outcome, "provisioned");
  assert.equal(receipt.schema, "witself.agent-email-secret-provisioning.v2");
  assert.equal(receipt.safeguards.route_keypair_verified, true);
  assert.equal(
    receipt.safeguards.exact_same_fallback_value_uploaded_to_both_targets,
    true,
  );
  assert.equal(receipt.safeguards.tagged_redeploy_required, true);
  assert.equal(receipt.safeguards.serialized_by_global_operations_lease, true);
  assert.deepEqual(receipt.operations_lease, leaseEvidence);
  assert.equal(fixture.puts.length, 3);
  assert.deepEqual(fixture.puts.map((put) => [
    put.args[2],
    put.args[put.args.indexOf("--name") + 1],
  ]), [
    ["AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY", CONTROL_PLANE_WORKER],
    ["CONTROL_PLANE_EDGE_TOKEN", CONTROL_PLANE_WORKER],
    ["CONTROL_PLANE_EDGE_TOKEN", EMAIL_EDGE_WORKER],
  ]);
  assert.equal(fixture.puts[0].value.toString(), fixture.material.privateKey);
  assert.equal(fixture.puts[1].value.toString(), token);
  assert.deepEqual(fixture.puts[1].value, fixture.puts[2].value);
  assert.equal(
    fixture.puts[0].args.includes(fixture.material.privateKey) ||
      fixture.puts[1].args.includes(token),
    false,
  );

  const firstReveal = fixture.calls.findIndex((call) =>
    call.command === "witself" && call.args?.[1] === "reveal");
  const lastPreflight = fixture.calls
    .slice(0, firstReveal)
    .reduce((latest, call, index) =>
      call.command === "wrangler" && call.type === "json" ? index : latest, -1);
  const firstPut = fixture.calls.findIndex((call) => call.type === "put");
  const lastReveal = fixture.calls.reduce((latest, call, index) =>
    call.command === "witself" && call.args?.[1] === "reveal" ? index : latest,
  -1);
  assert.ok(lastPreflight < firstReveal);
  assert.ok(lastReveal < firstPut);
  assert.equal(
    receipt.safeguards.post_write_bindings_and_inventories_verified,
    true,
  );
  assert.equal(
    receipt.safeguards.delivery_gates_reverified_immediately_before_mutation,
    true,
  );
  assert.equal(
    fixture.calls.filter((call) =>
      call.command === "wrangler" && call.type === "json").length,
    24,
  );
  assert.deepEqual(fixture.leaseEvents, [
    {
      type: "acquire",
      operation: leaseOperation,
      endpoint: "https://self.witwave.ai",
      token_matched: true,
    },
    { type: "renew", count: 1 },
    { type: "renew", count: 2 },
    { type: "renew", count: 3 },
    { type: "renew", count: 4 },
    { type: "renew", count: 5 },
    { type: "renew", count: 6 },
    { type: "renew", count: 7 },
    { type: "release" },
  ]);

  for (const put of fixture.puts) {
    assert.equal(put.runOptions.env.CLOUDFLARE_API_TOKEN, "existing-auth");
    assert.equal(put.runOptions.env.WRANGLER_WRITE_LOGS, "false");
    assert.equal(put.runOptions.env.WRANGLER_LOG_SANITIZE, "true");
    assert.equal(put.runOptions.env.WRANGLER_SEND_METRICS, "false");
    assert.equal(put.runOptions.env.WRANGLER_SEND_ERROR_REPORTS, "false");
    assert.equal(put.runOptions.env.WRANGLER_LOG, "error");
    for (const name of [
      "CF_ACCOUNT_ID",
      "CF_API_TOKEN",
      "CLOUDFLARE_API_BASE_URL",
      "CF_API_BASE_URL",
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
      "CONTROL_PLANE_EDGE_TOKEN",
      "CONTROL_PLANE_URL",
      "WITSELF_CONTROL_PLANE",
      "WITSELF_ENDPOINT",
    ]) assert.equal(Object.hasOwn(put.runOptions.env, name), false);
  }
  for (const call of fixture.calls.filter((item) =>
    item.command === "wrangler" && item.type === "json")) {
    assert.equal(call.runOptions.env.CLOUDFLARE_API_TOKEN, "existing-auth");
    assert.equal(call.runOptions.env.WRANGLER_WRITE_LOGS, "false");
    assert.equal(call.runOptions.env.WRANGLER_LOG_SANITIZE, "true");
    assert.equal(call.runOptions.env.WRANGLER_SEND_METRICS, "false");
    assert.equal(call.runOptions.env.WRANGLER_SEND_ERROR_REPORTS, "false");
    assert.equal(Object.hasOwn(call.runOptions.env, "WRANGLER_LOG"), false);
    for (const name of [
      "CF_ACCOUNT_ID",
      "CF_API_TOKEN",
      "CONTROL_PLANE_EDGE_TOKEN",
      "CONTROL_PLANE_URL",
      "WITSELF_CONTROL_PLANE",
      "WITSELF_ENDPOINT",
    ]) assert.equal(Object.hasOwn(call.runOptions.env, name), false);
  }
  for (const call of fixture.calls.filter((item) => item.command === "witself")) {
    assert.equal(call.runOptions.env.WITSELF_HOME, "/safe/witself-home");
    for (const name of [
      "NODE_OPTIONS",
      "NODE_DEBUG",
      "NODE_V8_COVERAGE",
      "SSLKEYLOGFILE",
      "WITSELF_ENDPOINT",
      "WITSELF_TOKEN",
      "WITSELF_TOKEN_FILE",
      "WITSELF_CONTROL_PLANE",
      "WITSELF_FAKE_PROVIDER_LOG",
    ]) assert.equal(Object.hasOwn(call.runOptions.env, name), false);
  }
  const rendered = JSON.stringify(receipt);
  assert.equal(rendered.includes(token), false);
  assert.equal(rendered.includes(fixture.material.privateKey), false);
  assert.equal(rendered.includes(fixture.material.publicKey), false);
  assert.deepEqual(JSON.parse(readFileSync(receiptPath, "utf8")), receipt);
  assert.equal(statSync(receiptPath).mode & 0o777, 0o600);
  assert.equal(fixture.snapshotMetadata.length, 2);
  for (const snapshot of fixture.snapshotMetadata) {
    assert.equal(snapshot.fileMode, 0o400);
    assert.equal(snapshot.directoryMode, 0o700);
    assert.equal(existsSync(snapshot.path), false);
  }
  for (const call of fixture.calls.filter((item) =>
    item.command === "wrangler")) {
    const path = call.args[call.args.indexOf("--config") + 1];
    assert.equal(path === CP_CONFIG || path === EDGE_CONFIG, false);
  }
  const pending = fixture.calls.find((call) =>
    call.type === "receipt-reserve").pending;
  assert.equal(pending.state, "secret_writes_started");
  assert.equal(JSON.stringify(pending).includes(token), false);
  assert.equal(JSON.stringify(pending).includes(fixture.material.privateKey), false);
  assert.equal(JSON.stringify(pending).includes(fixture.material.publicKey), false);
  assert.ok(
    fixture.calls.findIndex((call) => call.type === "lease-release") <
      fixture.calls.findIndex((call) => call.type === "receipt-commit"),
  );
});

test("frozen route configs ignore original mutation and reject snapshot mutation", async () => {
  const originalMutation = runtimeFixture({
    mutateOriginalSourcesAfterSnapshot: true,
  });
  const receipt = await provisionRouteSigningSecrets(options(), originalMutation);
  assert.equal(receipt.outcome, "provisioned");
  assert.equal(originalMutation.snapshotMetadata.length, 2);
  for (const path of originalMutation.snapshotPaths) {
    assert.equal(existsSync(path), false);
  }

  const snapshotMutation = runtimeFixture({ mutateFrozenSnapshot: true });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), snapshotMutation),
    /private deployment configuration changed during deployment/,
  );
  assert.equal(snapshotMutation.puts.length, 0);
  for (const path of snapshotMutation.snapshotPaths) {
    assert.equal(existsSync(path), false);
  }
});

test("route cleanup failure preserves the provisioning failure", async () => {
  const fixture = runtimeFixture({ leaseCollision: true });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), {
      ...fixture,
      async cleanupSnapshots(snapshots) {
        await Promise.all([
          snapshots.controlPlane.cleanup(),
          snapshots.emailEdge.cleanup(),
        ]);
        throw new Error("synthetic route snapshot cleanup failure");
      },
    }),
    (error) => {
      assert.equal(error instanceof AggregateError, true);
      assert.equal(error.errors.length, 2);
      assert.match(error.errors[0].message, /already holds the lease/);
      assert.match(error.errors[1].message, /snapshot cleanup failure/);
      return true;
    },
  );
  for (const path of fixture.snapshotPaths) assert.equal(existsSync(path), false);
});

test("lease collision refuses every secret write without exposing the desired token", async () => {
  const fixture = runtimeFixture({ leaseCollision: true });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /another agent email operation already holds the lease/,
  );
  assert.equal(fixture.puts.length, 0);
  assert.equal(
    fixture.calls.some((call) => call.type === "receipt-commit"),
    false,
  );
  assert.deepEqual(fixture.leaseEvents, [{
    type: "acquire",
    operation: leaseOperation,
    endpoint: "https://self.witwave.ai",
    token_matched: true,
  }]);
  assert.equal(JSON.stringify(fixture.leaseEvents).includes(token), false);
});

test("renewal loss after a bounded write stops the ceremony and releases the lease", async () => {
  const fixture = runtimeFixture({ renewalFailureAt: 2 });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /operations lease renewal failed/,
  );
  assert.equal(fixture.puts.length, 1);
  assert.equal(
    fixture.calls.some((call) => call.type === "receipt-commit"),
    false,
  );
  assert.deepEqual(fixture.leaseEvents.slice(-3), [
    { type: "renew", count: 1 },
    { type: "renew", count: 2 },
    { type: "release" },
  ]);
});

test("lease-release failure retains the durable pending route receipt", async () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-route-release-failure-"));
  const receiptPath = join(directory, "receipt.json");
  const fixture = runtimeFixture({
    releaseFailure: true,
    realReceipt: receiptPath,
  });
  await assert.rejects(
    provisionRouteSigningSecrets(options(receiptPath), fixture),
    /operations lease release failed/,
  );
  assert.equal(fixture.puts.length, 3);
  const persisted = JSON.parse(readFileSync(receiptPath, "utf8"));
  assert.equal(
    persisted.schema,
    "witself.agent-email-secret-provisioning-pending.v1",
  );
  assert.equal(persisted.state, "secret_writes_started");
  assert.equal(fixture.calls.some((call) =>
    call.type === "receipt-commit"), false);
  assert.equal(fixture.calls.some((call) =>
    call.type === "receipt-close"), true);
});

test("route ceremony requires the exact production Cloudflare identity", async () => {
  for (const environment of [
    {},
    {
      CF_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
      CF_API_TOKEN: "alias-token",
    },
    {
      CLOUDFLARE_ACCOUNT_ID: "f".repeat(32),
      CLOUDFLARE_API_TOKEN: "wrong-account-token",
    },
    {
      CLOUDFLARE_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
      CLOUDFLARE_API_TOKEN: " ",
    },
  ]) {
    const fixture = runtimeFixture();
    await assert.rejects(
      provisionRouteSigningSecrets(options(), { ...fixture, environment }),
      /CLOUDFLARE_ACCOUNT_ID must identify production account|CLOUDFLARE_API_TOKEN is missing or invalid/,
    );
    assert.equal(fixture.calls.length, 0);
  }
});

test("noncanonical active edge lease origin is rejected before acquire or mutation", async () => {
  const fixture = runtimeFixture({
    mutateJSON({ command, args, value }) {
      if (command === "wrangler" && args[0] === "versions" &&
          args.includes(EMAIL_EDGE_WORKER)) {
        value.resources.bindings.find(
          (binding) => binding.name === "CONTROL_PLANE_URL",
        ).text = "https://attacker.invalid/";
      }
    },
  });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /operations lease origin is missing or invalid/,
  );
  assert.equal(fixture.leaseEvents.length, 0);
  assert.equal(fixture.puts.length, 0);
});

test("leased reinspection rejects endpoint authority drift before mutation", async () => {
  const fixture = runtimeFixture({
    mutateJSON({ command, args, value }) {
      const reveals = fixture.calls.filter((call) =>
        call.command === "witself" && call.args?.[1] === "reveal").length;
      if (reveals === 2 && command === "wrangler" && args[0] === "versions" &&
          args.includes(EMAIL_EDGE_WORKER)) {
        value.resources.bindings.find(
          (binding) => binding.name === "CONTROL_PLANE_URL",
        ).text = "https://attacker.invalid/";
      }
    },
  });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /operations lease origin is missing or invalid/,
  );
  assert.equal(fixture.puts.length, 0);
  assert.deepEqual(fixture.leaseEvents.map((event) => event.type), [
    "acquire",
    "release",
  ]);
});

test("rejects an enabled remote delivery gate before any Witself access", async () => {
  const fixture = runtimeFixture({
    mutateJSON({ command, args, value }) {
      if (command === "wrangler" && args[0] === "secret" &&
          args.includes(CONTROL_PLANE_WORKER)) {
        value.push({ name: "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED" });
      }
    },
  });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /must be dark/,
  );
  assert.equal(fixture.calls.some((call) => call.command === "witself"), false);
  assert.equal(fixture.puts.length, 0);
});

test("rechecks dark gates after reveals and still refuses every mutation", async () => {
  const fixture = runtimeFixture();
  const original = fixture.runtime.json;
  fixture.runtime.json = (command, args, runOptions) => {
    const value = original(command, args, runOptions);
    const reveals = fixture.calls.filter((call) =>
      call.command === "witself" && call.args?.[1] === "reveal").length;
    if (reveals === 2 && command === "wrangler" && args[0] === "secret" &&
        args.includes(CONTROL_PLANE_WORKER)) {
      value.push({ name: "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED" });
    }
    return value;
  };
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /must be dark/,
  );
  assert.equal(fixture.puts.length, 0);
  assert.equal(fixture.leaseEvents.at(-1)?.type, "release");
});

test("rejects malformed reveal envelopes before any Cloudflare mutation", async () => {
  const fixture = runtimeFixture({
    mutateJSON({ command, args, value }) {
      if (command === "witself" && args[1] === "reveal" &&
          args[2] === "edge-token") {
        value.unexpected = "field";
      }
    },
  });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /fallback-token reveal envelope is invalid/,
  );
  assert.equal(fixture.puts.length, 0);
});

test("post-write verification failure cannot produce a success receipt", async () => {
  const fixture = runtimeFixture();
  const original = fixture.runtime.json;
  fixture.runtime.json = (command, args, runOptions) => {
    const value = original(command, args, runOptions);
    if (fixture.puts.length === 3 && command === "wrangler" &&
        args[0] === "versions" && args.includes(EMAIL_EDGE_WORKER)) {
      value.resources.bindings = value.resources.bindings.filter(
        (binding) => binding.name !== "CONTROL_PLANE_EDGE_TOKEN",
      );
    }
    return value;
  };
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /post-write Worker secret verification failed.*remain dark/,
  );
  assert.equal(fixture.puts.length, 3);
  assert.equal(
    fixture.calls.some((call) => call.type === "receipt-commit"),
    false,
  );
  assert.equal(fixture.leaseEvents.at(-1)?.type, "release");
});

test("rejects a private key that does not match the configured public key", async () => {
  const expected = keyMaterial();
  const different = keyMaterial();
  const fixture = runtimeFixture({ material: expected });
  fixture.runtime.json = ((original) => (command, args, runOptions) => {
    const result = original(command, args, runOptions);
    if (command === "witself" && args[1] === "reveal" &&
        args[2] === "route-signing") result.value = different.privateKey;
    return result;
  })(fixture.runtime.json);
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /does not match the active public key/,
  );
  assert.equal(fixture.puts.length, 0);
});

test("rejects a weak fallback token before any Cloudflare mutation", async () => {
  const fixture = runtimeFixture({ fallbackToken: "a".repeat(64) });
  await assert.rejects(
    provisionRouteSigningSecrets(options(), fixture),
    /high-entropy token policy/,
  );
  assert.equal(fixture.puts.length, 0);
});

test("requires exact generated targets, active key id, and canonical public keyring", () => {
  const material = keyMaterial();
  const valid = configs(material.publicKey);
  assert.equal(
    validateProvisioningConfigs(valid[CP_CONFIG], valid[EDGE_CONFIG]).keyID,
    "route-2026-08",
  );
  assert.throws(
    () => validateProvisioningConfigs(
      valid[CP_CONFIG].replace(CONTROL_PLANE_WORKER, "wrong-worker"),
      valid[EDGE_CONFIG],
    ),
    /exact expected Workers/,
  );
  const noncanonicalEdge = JSON.parse(valid[EDGE_CONFIG]);
  noncanonicalEdge.vars.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS =
    `{ "route-2026-08":"${material.publicKey}"}`;
  assert.throws(
    () => validateProvisioningConfigs(
      valid[CP_CONFIG],
      JSON.stringify(noncanonicalEdge),
    ),
    /keyring is invalid/,
  );
});

test("JSONC parser preserves comment-shaped strings and rejects malformed input", () => {
  assert.deepEqual(parseJSONC(`{
    // comment
    "url": "https://example.test/a/*not-comment*/", /* comment */
    "items": [1, 2,],
  }`), {
    url: "https://example.test/a/*not-comment*/",
    items: [1, 2],
  });
  assert.throws(() => parseJSONC("{/* unterminated"), /invalid JSONC/);
});

test("keypair and token validators reject wrong algorithms and low entropy", () => {
  const material = keyMaterial();
  assert.equal(verifyEd25519Keypair(material.privateKey, material.publicKey), true);
  const pemPair = generateKeyPairSync("ed25519");
  const pem = pemPair.privateKey.export({ format: "pem", type: "pkcs8" }).trim();
  const pemPublicDER = pemPair.publicKey.export({ format: "der", type: "spki" });
  assert.equal(
    verifyEd25519Keypair(
      pem,
      pemPublicDER.subarray(pemPublicDER.length - 32).toString("base64"),
    ),
    true,
  );
  const rsa = generateKeyPairSync("rsa", { modulusLength: 2048 });
  const rsaPrivate = rsa.privateKey.export({ format: "der", type: "pkcs8" })
    .toString("base64");
  assert.throws(
    () => verifyEd25519Keypair(rsaPrivate, material.publicKey),
    /not canonical PKCS8 base64|not Ed25519/,
  );
  assert.equal(validateFallbackToken(token), true);
  assert.throws(() => validateFallbackToken("a".repeat(64)));
});

test("argument parsing requires public selectors and configurable identity", () => {
  assert.equal(options().agent, "scott");
  assert.throws(
    () => parseProvisioningArgs([
      "--route-secret", "route",
      "--route-private-field", "private",
      "--route-public-field", "public",
      "--fallback-secret", "fallback",
      "--fallback-field", "token",
      "--receipt", "/private/receipt.json",
    ], {}),
    /--agent is missing/,
  );
  assert.throws(
    () => parseProvisioningArgs([
      "--route-secret", "--looks-like-a-flag",
      "--receipt", "/private/receipt.json",
    ], { WITSELF_AGENT: "scott" }),
    /--route-secret is invalid|requires a value/,
  );
  assert.throws(
    () => parseProvisioningArgs([
      "--route-secret", "route",
      "--route-private-field", "private",
      "--route-public-field", "public",
      "--fallback-secret", "fallback",
      "--fallback-field", "token",
      "--agent", "scott",
    ], {}),
    /--receipt must be one canonical absolute path/,
  );
  assert.throws(
    () => parseProvisioningArgs([
      "--route-secret", "route",
      "--route-private-field", "private",
      "--route-public-field", "public",
      "--fallback-secret", "fallback",
      "--fallback-field", "token",
      "--agent", "scott",
      "--receipt", join(process.cwd(), "receipt.json"),
    ], {}),
    /outside the repository checkout/,
  );
});

test("Wrangler environment is fail-closed against redirection and logging overrides", () => {
  const sanitized = sanitizedWranglerEnvironment({
    PATH: "/safe/bin",
    CF_ACCOUNT_ID: "f".repeat(32),
    CF_API_TOKEN: "conflicting-auth",
    CLOUDFLARE_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_API_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_ENV: "attacker",
    DOTENV_KEY: "dotenv://attacker.invalid",
    AWS_SECRET_ACCESS_KEY: "must-not-reach-wrangler",
    WITSELF_TEST_STRIPE_SECRET_KEY: "must-not-reach-wrangler",
    CLOUDFLARE_API_TOKEN: "canonical-provider-token",
    EMAIL_DIRECTORY_KV_ID: "a".repeat(32),
    WRANGLER_LOG_SANITIZE: "false",
    WRANGLER_SEND_METRICS: "true",
    WRANGLER_WRITE_LOGS: "true",
    WRANGLER_OUTPUT_FILE_PATH: "/tmp/leak",
    WRANGLER_CI_OVERRIDE_NAME: "wrong-worker",
    CONTROL_PLANE_EDGE_TOKEN: "must-not-reach-wrangler",
    CONTROL_PLANE_URL: "https://attacker.invalid/",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid/",
    NODE_OPTIONS: "--inspect",
  });
  assert.equal(sanitized.PATH, "/safe/bin");
  assert.equal(sanitized.CLOUDFLARE_API_TOKEN, "canonical-provider-token");
  assert.equal(sanitized.EMAIL_DIRECTORY_KV_ID, "a".repeat(32));
  assert.equal(sanitized.WRANGLER_LOG_SANITIZE, "true");
  assert.equal(sanitized.WRANGLER_SEND_METRICS, "false");
  assert.equal(sanitized.WRANGLER_SEND_ERROR_REPORTS, "false");
  assert.equal(sanitized.WRANGLER_WRITE_LOGS, "false");
  assert.equal(sanitized.WRANGLER_LOG, "error");
  assert.equal(Object.hasOwn(sanitized, "CLOUDFLARE_API_BASE_URL"), false);
  assert.equal(Object.hasOwn(sanitized, "CLOUDFLARE_BASE_URL"), false);
  assert.equal(Object.hasOwn(sanitized, "CLOUDFLARE_ENV"), false);
  assert.equal(Object.hasOwn(sanitized, "DOTENV_KEY"), false);
  assert.equal(Object.hasOwn(sanitized, "AWS_SECRET_ACCESS_KEY"), false);
  assert.equal(
    Object.hasOwn(sanitized, "WITSELF_TEST_STRIPE_SECRET_KEY"),
    false,
  );
  assert.equal(Object.hasOwn(sanitized, "CF_ACCOUNT_ID"), false);
  assert.equal(Object.hasOwn(sanitized, "CF_API_TOKEN"), false);
  assert.equal(Object.hasOwn(sanitized, "WRANGLER_OUTPUT_FILE_PATH"), false);
  assert.equal(Object.hasOwn(sanitized, "WRANGLER_CI_OVERRIDE_NAME"), false);
  assert.equal(Object.hasOwn(sanitized, "CONTROL_PLANE_EDGE_TOKEN"), false);
  assert.equal(Object.hasOwn(sanitized, "CONTROL_PLANE_URL"), false);
  assert.equal(Object.hasOwn(sanitized, "WITSELF_CONTROL_PLANE"), false);
  assert.equal(Object.hasOwn(sanitized, "NODE_OPTIONS"), false);

  const inspection = sanitizedWranglerInspectionEnvironment({
    PATH: "/safe/bin",
    WRANGLER_LOG: "debug",
    WRANGLER_LOG_PATH: "/tmp/unsafe.log",
  });
  assert.equal(inspection.PATH, "/safe/bin");
  assert.equal(Object.hasOwn(inspection, "WRANGLER_LOG"), false);
  assert.equal(Object.hasOwn(inspection, "WRANGLER_LOG_PATH"), false);
  assert.equal(inspection.WRANGLER_WRITE_LOGS, "false");
  assert.equal(inspection.WRANGLER_LOG_SANITIZE, "true");
});

test("Witself reveal environment preserves custody paths but blocks redirection and output", () => {
  const sanitized = sanitizedWitselfEnvironment({
    PATH: "/safe/bin",
    WITSELF_HOME: "/safe/witself-home",
    WITSELF_ENDPOINT: "https://attacker.invalid",
    WITSELF_TOKEN: "unsafe-token",
    WITSELF_TOKEN_FILE: "/tmp/unsafe-token",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid",
    WITSELF_FAKE_PROVIDER_LOG: "/tmp/unsafe-log",
    WITSELF_TEST_WINDOWS_HOOK_ARGV_OUTPUT: "/tmp/unsafe-output",
    NODE_OPTIONS: "--require attacker",
    NODE_DEBUG: "http",
    NODE_V8_COVERAGE: "/tmp/unsafe-coverage",
    SSLKEYLOGFILE: "/tmp/unsafe-tls-keys",
  });
  assert.equal(sanitized.PATH, "/safe/bin");
  assert.equal(sanitized.WITSELF_HOME, "/safe/witself-home");
  for (const name of [
    "WITSELF_ENDPOINT",
    "WITSELF_TOKEN",
    "WITSELF_TOKEN_FILE",
    "WITSELF_CONTROL_PLANE",
    "WITSELF_FAKE_PROVIDER_LOG",
    "WITSELF_TEST_WINDOWS_HOOK_ARGV_OUTPUT",
    "NODE_OPTIONS",
    "NODE_DEBUG",
    "NODE_V8_COVERAGE",
    "SSLKEYLOGFILE",
  ]) assert.equal(Object.hasOwn(sanitized, name), false);
});

test("required receipt preflight never overwrites", () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-secret-receipt-"));
  const path = join(directory, "receipt.json");
  assert.doesNotThrow(() => assertReceiptAvailable(path));
  writeFileSync(path, "existing", { mode: 0o600 });
  assert.equal(statSync(path).mode & 0o777, 0o600);
  assert.throws(
    () => assertReceiptAvailable(path),
    /refusing to overwrite/,
  );
});
