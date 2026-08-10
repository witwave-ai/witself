import assert from "node:assert/strict";
import { createHash, generateKeyPairSync } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import test from "node:test";

import { releaseMessage } from "../scripts/deployment-identity.mjs";
import {
  captureRelayProviderDarkState,
  parseRelayProvisioningArgs,
  provisionRelaySigningKey,
  validateRelayProvisioningConfigs,
  validateRelaySecretMetadata,
} from "../scripts/provision-relay-signing-key.mjs";
import { reserveJSONReceipt } from "../scripts/receipt-journal.mjs";
import {
  assertReceiptAvailable,
} from "../scripts/provision-route-signing-secrets.mjs";
import {
  workerVersionMessage,
  workerVersionTag,
} from "../../control-plane/scripts/source-identity.mjs";

const CP_CONFIG = "/safe/control-plane.jsonc";
const EDGE_CONFIG = "/safe/email-edge.jsonc";
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const relaySecretID = "sec_aaaaaaaaaaaaaaaa";
const keyIDFieldID = "fld_bbbbbbbbbbbbbbbb";
const publicFieldID = "fld_cccccccccccccccc";
const privateFieldID = "fld_dddddddddddddddd";
const cpDeploymentID = "11111111-2222-4333-8444-555555555555";
const cpVersionID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const edgeDeploymentID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const edgeVersionID = "01234567-89ab-4cde-8f01-23456789abcd";
const successorDeploymentID = "12345678-9abc-4def-8123-456789abcdef";
const successorVersionID = "fedcba98-7654-4321-8fed-cba987654321";
const targetKeyID = "relay-2026-08";
const priorKeyID = "relay-2026-07";
const routeKeyID = "route-2026-08";
const routePublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
const directoryID = "b".repeat(32);
const accountID = "0123456789abcdef0123456789abcdef";
const zoneID = "fedcba9876543210fedcba9876543210";
const release = Object.freeze({
  version: "0.0.241",
  tag: "v0.0.241",
  commit: "a".repeat(40),
  date: "2026-08-10T12:00:00Z",
});
const leaseEvidence = Object.freeze({
  schema_version: "witself.agent-email-operations-lease-evidence.v1",
  generation: 11,
  operation: "relay_signing_key_provision",
});

function keyMaterial() {
  const pair = generateKeyPairSync("ed25519");
  const privateDER = pair.privateKey.export({ format: "der", type: "pkcs8" });
  const publicDER = pair.publicKey.export({ format: "der", type: "spki" });
  return {
    privateKey: privateDER.toString("base64"),
    publicKey: publicDER.subarray(publicDER.length - 32).toString("base64"),
  };
}

function configs() {
  return {
    [CP_CONFIG]: JSON.stringify({
      name: CONTROL_PLANE_WORKER,
      main: "src/index.js",
      secrets: { required: [
        "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY",
        "CONTROL_PLANE_EDGE_TOKEN",
      ] },
      kv_namespaces: [{
        binding: "AGENT_EMAIL_DIRECTORY",
        id: directoryID,
      }],
      vars: {
        AGENT_EMAIL_DOMAIN: "witmail.net",
        AGENT_EMAIL_LEGACY_DOMAINS: "agent-mail.witwave.ai",
        AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: routeKeyID,
        CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
        WITSELF_EDGE_RELEASE_VERSION: release.version,
        WITSELF_EDGE_RELEASE_COMMIT: release.commit,
        WITSELF_EDGE_RELEASE_DATE: release.date,
      },
    }, null, 2),
    [EDGE_CONFIG]: JSON.stringify({
      name: EMAIL_EDGE_WORKER,
      main: "src/index.js",
      secrets: { required: [
        "CONTROL_PLANE_EDGE_TOKEN",
        "RELAY_ED25519_PRIVATE_KEY",
      ] },
      kv_namespaces: [{ binding: "EMAIL_DIRECTORY", id: directoryID }],
      vars: {
        AGENT_EMAIL_DOMAIN: "witmail.net",
        AGENT_EMAIL_LEGACY_DOMAINS: "agent-mail.witwave.ai",
        AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: JSON.stringify({
          [routeKeyID]: routePublicKey,
        }),
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
        CONTROL_PLANE_URL: "https://self.witwave.ai/",
        REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
        REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
        RELAY_KEY_ID: targetKeyID,
        WITSELF_EDGE_RELEASE_VERSION: release.version,
        WITSELF_EDGE_RELEASE_COMMIT: release.commit,
        WITSELF_EDGE_RELEASE_DATE: release.date,
      },
    }, null, 2),
  };
}

function normalizedSnapshotContents(snapshot) {
  const directory = basename(dirname(snapshot.path));
  const expectedMain = directory.startsWith("witself-relay-control-plane-")
    ? "../control-plane/src/index.js"
    : directory.startsWith("witself-relay-email-edge-")
      ? "../agent-email/src/index.js"
      : "";
  assert.notEqual(expectedMain, "", "snapshot directory must identify its Worker");
  assert.equal(JSON.parse(snapshot.contents).main, expectedMain);
  return snapshot.contents.replace(
    `"main": "${expectedMain}"`,
    '"main": "src/index.js"',
  );
}

function deployment(id, versionID) {
  return {
    id,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function edgeVersion(id = edgeVersionID, {
  annotations = true,
  relayKeyID = priorKeyID,
  releaseVersion = release.version,
  changedResource = false,
} = {}) {
  return {
    id,
    number: id === edgeVersionID ? 241 : 242,
    ...(annotations ? {
      annotations: {
        "workers/tag": release.tag,
        "workers/message": releaseMessage(release),
      },
    } : {}),
    resources: {
      script: {
        etag: "provider-artifact-etag-1234567890",
        handlers: ["email"],
      },
      script_runtime: {
        compatibility_date: changedResource ? "2026-08-10" : "2026-07-21",
        compatibility_flags: ["global_fetch_strictly_public"],
      },
      bindings: [
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"),
        plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
        plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", JSON.stringify({
          [routeKeyID]: routePublicKey,
        })),
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        plain("CONTROL_PLANE_URL", "https://self.witwave.ai/"),
        { name: "EMAIL_DIRECTORY", namespace_id: directoryID, type: "kv_namespace" },
        {
          name: "EMAIL_EDGE_METRICS",
          dataset: "witself_agent_email_edge",
          type: "analytics_engine",
        },
        plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
        plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false"),
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
        secret("RELAY_ED25519_PRIVATE_KEY"),
        plain("RELAY_KEY_ID", relayKeyID),
        plain("WITSELF_EDGE_RELEASE_COMMIT", release.commit),
        plain("WITSELF_EDGE_RELEASE_DATE", release.date),
        plain("WITSELF_EDGE_RELEASE_VERSION", releaseVersion),
      ],
    },
  };
}

function cpExpected(releaseVersion = release.version) {
  return {
    service: CONTROL_PLANE_WORKER,
    version: releaseVersion,
    commit: release.commit,
    date: release.date,
  };
}

function cpVersion(id = cpVersionID, { releaseVersion = release.version } = {}) {
  const expected = cpExpected(releaseVersion);
  return {
    id,
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
          namespace_id: "d".repeat(32),
          type: "durable_object_namespace",
        })),
        plain("WITSELF_EDGE_RELEASE_VERSION", releaseVersion),
        plain("WITSELF_EDGE_RELEASE_COMMIT", release.commit),
        plain("WITSELF_EDGE_RELEASE_DATE", release.date),
        plain("AGENT_EMAIL_ROUTE_SIGNING_KEY_ID", routeKeyID),
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"),
        plain("CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM", "8"),
        plain("CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT", "64"),
        plain("CP_AGENT_EMAIL_CUSTOM_DOMAIN_MAX_OPEN_PER_ACCOUNT", "8"),
        plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
        secret("AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"),
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        {
          name: "AGENT_EMAIL_DIRECTORY",
          type: "kv_namespace",
          namespace_id: directoryID,
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
      ],
    },
  };
}

function field(id, name, kind, sensitive, publicValue) {
  const result = {
    id,
    name,
    kind,
    sensitive,
    encoding: "utf8",
    redacted: sensitive,
  };
  if (!sensitive) result.public_value = publicValue;
  return result;
}

function showEnvelope(material, mutate = () => {}) {
  const value = {
    secret: {
      id: relaySecretID,
      name: "relay-key-source-secret",
      lifecycle: "active",
      row_version: 1,
      fields: [
        field(keyIDFieldID, "key-id", "text", false, targetKeyID),
        field(publicFieldID, "public-key", "text", false, material.publicKey),
        field(privateFieldID, "private-key", "private_key", true),
      ],
    },
  };
  mutate(value);
  return value;
}

function revealEnvelope(material, mutate = () => {}) {
  const value = {
    secret_id: relaySecretID,
    field_id: privateFieldID,
    field_name: "private-key",
    encoding: "utf8",
    value: material.privateKey,
  };
  mutate(value);
  return value;
}

function options(
  receipt = "/private/relay-receipt.json",
  providerZoneName = "witmail.net",
) {
  return parseRelayProvisioningArgs([
    "--control-plane-config", CP_CONFIG,
    "--email-edge-config", EDGE_CONFIG,
    "--agent", "scott",
    "--relay-secret", "relay-key-source-secret",
    "--relay-key-id-field", "key-id",
    "--relay-public-field", "public-key",
    "--relay-private-field", "private-key",
    "--provider-zone-name", providerZoneName,
    "--receipt", receipt,
  ], {});
}

function providerEvidence(revision = "d", {
  contract = "primary",
  zoneName = "witmail.net",
} = {}) {
  return Object.freeze({
    schema: "witself.agent-email-provider-dark.v1",
    provider_scope: Object.freeze({
      contract,
      account_id_sha256: "1".repeat(64),
      zone_id_sha256: "2".repeat(64),
      zone_name_sha256: createHash("sha256").update(zoneName).digest("hex"),
      zone_identity_sha256: "4".repeat(64),
    }),
    catch_all_sha256: "c".repeat(64),
    rule_inventory_sha256: revision.repeat(64),
    rule_count: 0,
    owned_or_edge_worker_routes_enabled: false,
  });
}

function fixture({
  material = keyMaterial(),
  showMutate = () => {},
  revealMutate = () => {},
  remoteDriftAt = 0,
  providerDriftAt = 0,
  providerInvalid = false,
  providerContract = "primary",
  providerZoneName = "witmail.net",
  leaseCollision = false,
  renewalFailureAt = 0,
  mutationFailure = false,
  receiptCollision = false,
  receiptReservationCollision = false,
  realReceipt = "",
  noSuccessor = false,
  successorResourceDrift = false,
  liveRelayKeyID = priorKeyID,
  liveControlPlaneRelease = release.version,
  liveEdgeRelease = release.version,
  mutateOriginalSourcesAfterSnapshot = false,
  mutateFrozenSnapshot = false,
} = {}) {
  const files = configs();
  const calls = [];
  const puts = [];
  const leaseEvents = [];
  let edgeDeploymentReads = 0;
  let providerReads = 0;
  let mutated = false;
  let originalSourcesMutated = false;
  let frozenSnapshotMutated = false;
  const snapshotPaths = new Set();
  const snapshotMetadata = [];

  const runtime = {
    readText(path) {
      calls.push({ type: "read", path });
      return files[path];
    },
    json(command, args, runOptions) {
      calls.push({ type: "json", command, args: [...args], runOptions });
      if (command === "witself") {
        if (args[1] === "show") return showEnvelope(material, showMutate);
        if (args[1] === "reveal") return revealEnvelope(material, revealMutate);
      }
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
      const worker = args[args.indexOf("--name") + 1];
      if (args[0] === "deployments") {
        if (worker === EMAIL_EDGE_WORKER) edgeDeploymentReads += 1;
        const drift = remoteDriftAt > 0 && edgeDeploymentReads >= remoteDriftAt;
        if (worker === CONTROL_PLANE_WORKER) {
          return deployment(cpDeploymentID, cpVersionID);
        }
        if (mutated && !noSuccessor) {
          return deployment(successorDeploymentID, successorVersionID);
        }
        return deployment(
          drift ? "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" : edgeDeploymentID,
          edgeVersionID,
        );
      }
      if (args[0] === "versions") {
        if (worker === CONTROL_PLANE_WORKER) {
          return cpVersion(cpVersionID, {
            releaseVersion: liveControlPlaneRelease,
          });
        }
        if (mutated && !noSuccessor) {
          return edgeVersion(successorVersionID, {
            annotations: false,
            relayKeyID: liveRelayKeyID,
            releaseVersion: liveEdgeRelease,
            changedResource: successorResourceDrift,
          });
        }
        return edgeVersion(edgeVersionID, {
          relayKeyID: liveRelayKeyID,
          releaseVersion: liveEdgeRelease,
        });
      }
      if (args[0] === "secret" && args[1] === "list") {
        return worker === CONTROL_PLANE_WORKER
          ? [
            { name: "CONTROL_PLANE_EDGE_TOKEN" },
            { name: "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY" },
          ]
          : [
            { name: "CONTROL_PLANE_EDGE_TOKEN" },
            { name: "RELAY_ED25519_PRIVATE_KEY" },
          ];
      }
      throw new Error(`unexpected command ${command} ${args.join(" ")}`);
    },
    secretPut(command, args, value, runOptions) {
      calls.push({ type: "put", command, args: [...args], runOptions });
      if (mutationFailure) throw new Error("provider secret mutation failed");
      puts.push({ args: [...args], value: Buffer.from(value), runOptions });
      mutated = true;
    },
    assertReceiptAvailable(path) {
      calls.push({ type: "receipt-preflight", path });
      if (receiptCollision) {
        throw new Error("receipt path already exists; refusing to overwrite it");
      }
      if (realReceipt) assertReceiptAvailable(path);
    },
  };
  const provider = {
    async capture() {
      providerReads += 1;
      calls.push({ type: "provider-capture", read: providerReads });
      if (providerInvalid) return { unsafe: true };
      return providerEvidence(
        providerDriftAt > 0 && providerReads >= providerDriftAt ? "e" : "d",
        { contract: providerContract, zoneName: providerZoneName },
      );
    },
  };
  const withLease = async (operation, work, leaseOptions) => {
    leaseEvents.push({
      type: "acquire",
      operation,
      endpoint: leaseOptions.endpoint,
      tokenInEnvironment: typeof leaseOptions.env?.CONTROL_PLANE_EDGE_TOKEN ===
        "string",
      explicitToken: Object.hasOwn(leaseOptions, "token"),
    });
    if (leaseCollision) {
      throw new Error("another agent email operation already holds the lease");
    }
    let renewals = 0;
    try {
      return await work({
        renew: async () => {
          renewals += 1;
          leaseEvents.push({ type: "renew", count: renewals });
          if (renewals === renewalFailureAt) {
            throw new Error("agent email operations lease renewal failed");
          }
        },
        evidence: () => leaseEvidence,
      });
    } finally {
      leaseEvents.push({ type: "release" });
    }
  };
  const reserveReceipt = (path, pending) => {
    calls.push({ type: "receipt-reserve", path, pending });
    if (receiptReservationCollision) {
      throw new Error("EEXIST: receipt reservation collision");
    }
    if (realReceipt) return reserveJSONReceipt(path, pending);
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
  };
  return {
    runtime,
    provider,
    withLease,
    reserveReceipt,
    calls,
    puts,
    leaseEvents,
    material,
    files,
    snapshotPaths,
    snapshotMetadata,
  };
}

function productionEnvironment() {
  return {
    CLOUDFLARE_ACCOUNT_ID: accountID,
    CLOUDFLARE_ZONE_ID: zoneID,
    CLOUDFLARE_API_TOKEN: "provider-token",
    CF_ACCOUNT_ID: "f".repeat(32),
    CF_API_TOKEN: "conflicting-provider-token",
    CONTROL_PLANE_EDGE_TOKEN: "lease-token-longer-than-sixteen",
    NODE_OPTIONS: "--require attacker",
    WITSELF_TOKEN: "must-not-reach-child",
  };
}

test("full rotation sequence creates one successor and a value-free receipt", async () => {
  const dir = mkdtempSync(join(tmpdir(), "witself-relay-ceremony-"));
  const receiptPath = join(dir, "receipt.json");
  const value = fixture({ realReceipt: receiptPath });
  const receipt = await provisionRelaySigningKey(options(receiptPath), {
    ...value,
    environment: productionEnvironment(),
  });

  assert.equal(receipt.schema,
    "witself.agent-email-relay-signing-key-provisioning.v2");
  assert.equal(receipt.relay_key.prior_key_id, priorKeyID);
  assert.equal(receipt.relay_key.desired_key_id, targetKeyID);
  assert.deepEqual(receipt.target.release, {
    version: release.version,
    commit: release.commit,
    date: release.date,
  });
  assert.match(receipt.target.config_fence.control_plane_sha256, /^[0-9a-f]{64}$/);
  assert.match(receipt.target.config_fence.email_edge_sha256, /^[0-9a-f]{64}$/);
  const controlPlaneSnapshot = value.snapshotMetadata.find((snapshot) =>
    basename(dirname(snapshot.path)).startsWith("witself-relay-control-plane-"));
  const emailEdgeSnapshot = value.snapshotMetadata.find((snapshot) =>
    basename(dirname(snapshot.path)).startsWith("witself-relay-email-edge-"));
  assert.ok(controlPlaneSnapshot);
  assert.ok(emailEdgeSnapshot);
  assert.equal(
    receipt.target.config_fence.control_plane_sha256,
    createHash("sha256").update(controlPlaneSnapshot.contents).digest("hex"),
  );
  assert.equal(
    receipt.target.config_fence.email_edge_sha256,
    createHash("sha256").update(emailEdgeSnapshot.contents).digest("hex"),
  );
  assert.deepEqual(receipt.target.provider_zone, {
    contract: "primary",
    zone_name_sha256:
      createHash("sha256").update("witmail.net").digest("hex"),
  });
  assert.equal(
    receipt.relay_key.desired_public_key_digest.sha256,
    createHash("sha256")
      .update(Buffer.from(value.material.publicKey, "base64"))
      .digest("hex"),
  );
  assert.equal(receipt.successor.prior_deployment_id, edgeDeploymentID);
  assert.equal(receipt.successor.prior_version_id, edgeVersionID);
  assert.equal(receipt.successor.deployment_id, successorDeploymentID);
  assert.equal(receipt.successor.version_id, successorVersionID);
  assert.equal(value.puts.length, 1);
  assert.equal(value.puts[0].args[2], "RELAY_ED25519_PRIVATE_KEY");
  assert.equal(
    value.puts[0].args[value.puts[0].args.indexOf("--name") + 1],
    EMAIL_EDGE_WORKER,
  );
  assert.equal(value.puts[0].value.toString(), value.material.privateKey);
  assert.equal(value.puts[0].args.includes(value.material.privateKey), false);
  assert.equal(value.puts[0].runOptions.env.CONTROL_PLANE_EDGE_TOKEN, undefined);
  assert.equal(value.puts[0].runOptions.env.NODE_OPTIONS, undefined);
  assert.equal(value.puts[0].runOptions.env.CF_ACCOUNT_ID, undefined);
  assert.equal(value.puts[0].runOptions.env.CF_API_TOKEN, undefined);
  assert.equal(value.puts[0].runOptions.env.CLOUDFLARE_ACCOUNT_ID, accountID);
  assert.equal(value.puts[0].runOptions.env.CLOUDFLARE_API_TOKEN,
    "provider-token");
  assert.equal(value.puts[0].runOptions.env.WRANGLER_WRITE_LOGS, "false");
  assert.equal(value.leaseEvents[0].operation, "relay_signing_key_provision");
  assert.equal(value.leaseEvents[0].tokenInEnvironment, true);
  assert.equal(value.leaseEvents[0].explicitToken, false);
  assert.equal(statSync(receiptPath).mode & 0o777, 0o600);

  const initialProvider = value.calls.findIndex((call) =>
    call.type === "provider-capture");
  const show = value.calls.findIndex((call) =>
    call.type === "json" && call.command === "witself" &&
    call.args[1] === "show");
  const reserve = value.calls.findIndex((call) =>
    call.type === "receipt-reserve");
  const put = value.calls.findIndex((call) => call.type === "put");
  assert.equal(initialProvider < show, true);
  assert.equal(show < reserve && reserve < put, true);
  assert.equal(value.calls.filter((call) =>
    call.type === "provider-capture").length, 3);
  for (const call of value.calls.filter((item) =>
    item.type === "json" && item.command === "witself")) {
    assert.equal(call.runOptions.env.CONTROL_PLANE_EDGE_TOKEN, undefined);
    assert.equal(call.runOptions.env.CLOUDFLARE_API_TOKEN, undefined);
    assert.equal(call.runOptions.env.CLOUDFLARE_ACCOUNT_ID, undefined);
    assert.equal(call.runOptions.env.CLOUDFLARE_ZONE_ID, undefined);
  }
  for (const call of value.calls.filter((item) =>
    item.command === "wrangler")) {
    assert.equal(call.runOptions.env.CF_ACCOUNT_ID, undefined);
    assert.equal(call.runOptions.env.CF_API_TOKEN, undefined);
    assert.equal(call.runOptions.env.CLOUDFLARE_ACCOUNT_ID, accountID);
    assert.equal(call.runOptions.env.CLOUDFLARE_API_TOKEN, "provider-token");
  }
  assert.equal(value.snapshotMetadata.length, 2);
  for (const snapshot of value.snapshotMetadata) {
    assert.equal(snapshot.fileMode, 0o400);
    assert.equal(snapshot.directoryMode, 0o700);
    assert.equal(existsSync(snapshot.path), false);
  }

  const persisted = readFileSync(receiptPath, "utf8");
  for (const forbidden of [
    value.material.privateKey,
    value.material.publicKey,
    relaySecretID,
    keyIDFieldID,
    publicFieldID,
    privateFieldID,
    "relay-key-source-secret",
    "private-key",
    accountID,
    zoneID,
    "witwave.ai",
  ]) {
    assert.equal(JSON.stringify(receipt).includes(forbidden), false);
    assert.equal(persisted.includes(forbidden), false);
  }
});

test("frozen configs ignore original mutation and are always cleaned", async () => {
  const value = fixture({ mutateOriginalSourcesAfterSnapshot: true });
  const original = configs();
  const receipt = await provisionRelaySigningKey(options(), value);
  assert.equal(receipt.outcome, "provisioned");
  assert.equal(value.snapshotMetadata.length, 2);
  assert.deepEqual(
    new Set(value.snapshotMetadata.map(normalizedSnapshotContents)),
    new Set([original[CP_CONFIG], original[EDGE_CONFIG]]),
  );
  for (const call of value.calls.filter((item) => item.command === "wrangler")) {
    const path = call.args[call.args.indexOf("--config") + 1];
    assert.equal(path === CP_CONFIG || path === EDGE_CONFIG, false);
  }
  for (const path of value.snapshotPaths) assert.equal(existsSync(path), false);

  const failed = fixture({ providerInvalid: true });
  await assert.rejects(
    provisionRelaySigningKey(options(), failed),
    /provider-route evidence was invalid/,
  );
  assert.equal(failed.snapshotPaths.size, 2);
  for (const path of failed.snapshotPaths) assert.equal(existsSync(path), false);
});

test("frozen config mutation is detected and both snapshots are cleaned", async () => {
  const value = fixture({ mutateFrozenSnapshot: true });
  await assert.rejects(
    provisionRelaySigningKey(options(), value),
    /private deployment configuration changed during deployment/,
  );
  assert.equal(value.puts.length, 0);
  assert.equal(value.leaseEvents.length, 0);
  assert.equal(value.snapshotPaths.size, 2);
  for (const path of value.snapshotPaths) assert.equal(existsSync(path), false);
});

test("wrong provider scope fails before Witself reveal or mutation", async () => {
  for (const [value, pattern] of [
    [fixture({ providerInvalid: true }), /provider-route evidence was invalid/],
    [fixture({
      providerContract: "legacy",
      providerZoneName: "witwave.ai",
    }), /did not match the explicit ceremony target/],
  ]) {
    await assert.rejects(provisionRelaySigningKey(options(), value), pattern);
    assert.equal(value.calls.some((call) =>
      call.type === "json" && call.command === "witself"), false);
    assert.equal(value.puts.length, 0);
    assert.equal(value.leaseEvents.length, 0);
  }
});

test("provider inspection binds exact active zone, account, and dark routes", async () => {
  const disabledCatchAll = {
    id: "a".repeat(32),
    enabled: false,
    name: "catch-all",
    matchers: [{ type: "all" }],
    actions: [{ type: "drop", value: [] }],
  };
  const api = ({
    zoneName = "witmail.net",
    zoneAccount = accountID,
    status = "active",
    rules = [],
    catchAll = disabledCatchAll,
  } = {}) => ({
    accountID,
    zoneID,
    getZone: async () => ({
      id: zoneID,
      name: zoneName,
      status,
      account: { id: zoneAccount },
    }),
    getCatchAll: async () => catchAll,
    listRules: async () => rules,
  });

  const state = await captureRelayProviderDarkState(api({
    rules: [
      {
        id: "b".repeat(32),
        name: "witself-agent-email-primary-canary:test",
        enabled: false,
        actions: [{ type: "worker", value: [EMAIL_EDGE_WORKER] }],
      },
      {
        id: "d".repeat(32),
        name: "another-tenant-route",
        enabled: true,
        actions: [{ type: "worker", value: ["another-tenant-worker"] }],
      },
    ],
  }));
  assert.equal(state.provider_scope.contract, "primary");
  assert.equal(state.owned_or_edge_worker_routes_enabled, false);
  assert.equal(JSON.stringify(state).includes(accountID), false);
  assert.equal(JSON.stringify(state).includes(zoneID), false);

  for (const [candidate, pattern] of [
    [api({ zoneName: "example.com" }), /zone\/account did not match/],
    [api({ zoneAccount: "f".repeat(32) }), /zone\/account did not match/],
    [api({ status: "pending" }), /zone\/account did not match/],
    [api({ catchAll: { ...disabledCatchAll, enabled: true } }), /must remain dark/],
    [api({
      rules: [{
        id: "e".repeat(32),
        name: "unrelated-name",
        enabled: true,
        actions: [{ type: "worker", value: [EMAIL_EDGE_WORKER] }],
      }],
    }), /must remain dark/],
    [api({
      rules: [{
        id: "f".repeat(32),
        name: "another-tenant-route",
        enabled: true,
        actions: [{ type: "worker", value: [] }],
      }],
    }), /inventory was invalid/],
  ]) {
    await assert.rejects(captureRelayProviderDarkState(candidate), pattern);
  }
});

test("live Workers must match one exact target release while retaining old relay id", async () => {
  for (const [candidate, pattern] of [
    [fixture({ liveControlPlaneRelease: "0.0.242" }), /wrong release annotations/],
    [fixture({ liveEdgeRelease: "0.0.242" }), /binding WITSELF_EDGE_RELEASE_VERSION/],
    [fixture({ liveRelayKeyID: targetKeyID }), /distinct desired key id/],
  ]) {
    await assert.rejects(provisionRelaySigningKey(options(), candidate), pattern);
    assert.equal(candidate.calls.some((call) =>
      call.type === "json" && call.command === "witself"), false);
    assert.equal(candidate.puts.length, 0);
  }
});

test("target configs require one matching v0.0.241 release and provider contract", () => {
  const files = configs();
  const target = validateRelayProvisioningConfigs(
    files[CP_CONFIG],
    files[EDGE_CONFIG],
  );
  assert.equal(target.keyID, targetKeyID);
  assert.deepEqual(target.release, {
    version: release.version,
    commit: release.commit,
    date: release.date,
  });
  assert.equal(options().providerZoneName, "witmail.net");
  assert.equal(options("/private/legacy.json", "witwave.ai").providerZoneName,
    "witwave.ai");
  assert.throws(
    () => options("/private/wrong.json", "example.com"),
    /must be witmail.net or witwave.ai/,
  );

  const old = JSON.parse(files[EDGE_CONFIG]);
  old.vars.WITSELF_EDGE_RELEASE_VERSION = "0.0.240";
  assert.throws(
    () => validateRelayProvisioningConfigs(files[CP_CONFIG], JSON.stringify(old)),
    /v0.0.241 or newer/,
  );
  old.vars.WITSELF_EDGE_RELEASE_VERSION = "0.0.241-rc.1";
  assert.throws(
    () => validateRelayProvisioningConfigs(files[CP_CONFIG], JSON.stringify(old)),
    /v0.0.241 or newer/,
  );
  old.vars.WITSELF_EDGE_RELEASE_VERSION = release.version;
  old.vars.WITSELF_EDGE_RELEASE_COMMIT = "f".repeat(40);
  assert.throws(
    () => validateRelayProvisioningConfigs(files[CP_CONFIG], JSON.stringify(old)),
    /release identities differ/,
  );
  old.vars.WITSELF_EDGE_RELEASE_COMMIT = release.commit;
  old.vars.AGENT_EMAIL_DOMAIN = "example.com";
  assert.throws(
    () => validateRelayProvisioningConfigs(files[CP_CONFIG], JSON.stringify(old)),
    /domain contract is invalid/,
  );
});

test("missing secret-put successor leaves a durable value-free recovery marker", async () => {
  const dir = mkdtempSync(join(tmpdir(), "witself-relay-pending-"));
  const receiptPath = join(dir, "receipt.json");
  const value = fixture({ realReceipt: receiptPath, noSuccessor: true });
  await assert.rejects(
    provisionRelaySigningKey(options(receiptPath), {
      ...value,
      environment: productionEnvironment(),
    }),
    /did not create one new edge Worker successor/,
  );
  assert.equal(value.puts.length, 1);
  const pending = JSON.parse(readFileSync(receiptPath, "utf8"));
  assert.equal(pending.schema,
    "witself.agent-email-relay-signing-key-provisioning-pending.v1");
  assert.equal(pending.state, "secret_write_started");
  assert.equal(pending.predecessor.edge_deployment_id, edgeDeploymentID);
  assert.equal(pending.predecessor.edge_version_id, edgeVersionID);
  assert.equal(statSync(receiptPath).mode & 0o777, 0o600);
  for (const forbidden of [
    value.material.privateKey,
    value.material.publicKey,
    relaySecretID,
    privateFieldID,
    "relay-key-source-secret",
  ]) {
    assert.equal(JSON.stringify(pending).includes(forbidden), false);
  }

  const blocked = fixture({ realReceipt: receiptPath });
  await assert.rejects(
    provisionRelaySigningKey(options(receiptPath), blocked),
    /refusing to overwrite/,
  );
  assert.equal(blocked.calls.length, 1);

  const incidentPath = join(dir, "receipt.pending-reconciled.json");
  renameSync(receiptPath, incidentPath);
  const recovered = fixture({ realReceipt: receiptPath });
  const receipt = await provisionRelaySigningKey(options(receiptPath), {
    ...recovered,
    environment: productionEnvironment(),
  });
  assert.equal(receipt.outcome, "provisioned");
  assert.equal(JSON.parse(readFileSync(incidentPath, "utf8")).state,
    "secret_write_started");
});

test("receipt collisions refuse mutation and failure after reservation stays pending", async () => {
  const collision = fixture({ receiptReservationCollision: true });
  await assert.rejects(
    provisionRelaySigningKey(options(), collision),
    /receipt reservation collision/,
  );
  assert.equal(collision.puts.length, 0);

  const dir = mkdtempSync(join(tmpdir(), "witself-relay-mutation-failure-"));
  const receiptPath = join(dir, "receipt.json");
  const failed = fixture({ realReceipt: receiptPath, mutationFailure: true });
  await assert.rejects(
    provisionRelaySigningKey(options(receiptPath), {
      ...failed,
      environment: productionEnvironment(),
    }),
    /provider secret mutation failed/,
  );
  assert.equal(
    JSON.parse(readFileSync(receiptPath, "utf8")).state,
    "secret_write_started",
  );
  assert.equal(failed.leaseEvents.at(-1).type, "release");
});

test("provider, Worker, lease, and non-secret drift fail closed", async () => {
  for (const [candidate, pattern] of [
    [fixture({ remoteDriftAt: 3 }), /changed before relay key provisioning/],
    [fixture({ providerDriftAt: 2 }), /routes changed before relay key provisioning/],
    [fixture({ successorResourceDrift: true }), /runtime contract did not match/],
  ]) {
    await assert.rejects(provisionRelaySigningKey(options(), candidate), pattern);
  }

  const collision = fixture({ leaseCollision: true });
  await assert.rejects(
    provisionRelaySigningKey(options(), collision),
    /already holds the lease/,
  );
  assert.equal(collision.puts.length, 0);
  assert.equal(collision.calls.some((call) =>
    call.type === "receipt-reserve"), false);

  const loss = fixture({ renewalFailureAt: 1 });
  await assert.rejects(
    provisionRelaySigningKey(options(), loss),
    /lease renewal failed/,
  );
  assert.equal(loss.puts.length, 0);
  assert.equal(loss.calls.some((call) => call.type === "receipt-close"), true);
});

test("default lease client requires CONTROL_PLANE_EDGE_TOKEN", async () => {
  const value = fixture();
  await assert.rejects(
    provisionRelaySigningKey(options(), {
      runtime: value.runtime,
      provider: value.provider,
      reserveReceipt: value.reserveReceipt,
      environment: {
        CLOUDFLARE_ACCOUNT_ID: accountID,
        CLOUDFLARE_ZONE_ID: zoneID,
        CLOUDFLARE_API_TOKEN: "provider-token",
      },
    }),
    /CONTROL_PLANE_EDGE_TOKEN is required/,
  );
  assert.equal(value.puts.length, 0);
});

test("keypair, metadata policy, and reveal envelope fail before mutation", async () => {
  const mismatch = fixture({ material: keyMaterial() });
  const other = keyMaterial();
  mismatch.runtime.json = ((original) => (command, args, runOptions) => {
    const result = original(command, args, runOptions);
    if (command === "witself" && args[1] === "show") {
      result.secret.fields.find((item) =>
        item.name === "public-key").public_value = other.publicKey;
    }
    return result;
  })(mismatch.runtime.json);
  await assert.rejects(
    provisionRelaySigningKey(options(), mismatch),
    /does not match the public key metadata/,
  );
  assert.equal(mismatch.puts.length, 0);

  for (const candidate of [
    fixture({ showMutate: (show) => {
      const value = show.secret.fields.find((item) =>
        item.name === "private-key");
      value.sensitive = false;
      value.redacted = false;
      value.public_value = "exposed";
    } }),
    fixture({ showMutate: (show) => {
      show.secret.fields.find((item) =>
        item.name === "key-id").public_value = "wrong-key";
    } }),
    fixture({ revealMutate: (reveal) => { reveal.field_id = publicFieldID; } }),
    fixture({ revealMutate: (reveal) => { reveal.extra = "unexpected"; } }),
  ]) {
    await assert.rejects(provisionRelaySigningKey(options(), candidate));
    assert.equal(candidate.puts.length, 0);
    assert.equal(candidate.leaseEvents.length, 0);
  }

  const material = keyMaterial();
  assert.equal(validateRelaySecretMetadata(
    showEnvelope(material),
    "relay-key-source-secret",
    { keyID: "key-id", public: "public-key", private: "private-key" },
    targetKeyID,
  ).publicKey, material.publicKey);
});

test("existing receipt refuses all inspection, reveal, and mutation", async () => {
  const dir = mkdtempSync(join(tmpdir(), "witself-relay-receipt-"));
  const receiptPath = join(dir, "receipt.json");
  writeFileSync(receiptPath, "existing", { mode: 0o600 });
  const value = fixture({ realReceipt: receiptPath });
  await assert.rejects(
    provisionRelaySigningKey(options(receiptPath), value),
    /refusing to overwrite/,
  );
  assert.equal(value.calls.length, 1);
  assert.equal(value.puts.length, 0);
});
