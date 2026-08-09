#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { CUSTOM_DOMAIN_DELIVERY_SECRET } from
  "./assert-custom-domain-dark.mjs";
import { EMAIL_DARK_SECRET_NAMES } from
  "../../control-plane/scripts/assert-custom-domain-dark.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const CONTROL_PLANE_WORKER = "witself-control-plane";
const EMAIL_EDGE_WORKER = "witself-agent-email-pilot";
const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const RELEASE_VERSION =
  /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const FULL_COMMIT = /^[0-9a-f]{40}$/;
const KEY_ID = /^[a-z][a-z0-9_-]{0,63}$/;
const PUBLIC_KEY = /^[A-Za-z0-9+/]{43}=$/;
const OPAQUE_ETAG = /^[0-9A-Za-z._:-]{16,256}$/;
const RELEASE_DATE =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

function activeVersionID(deployment, label) {
  if (!deployment || typeof deployment !== "object" ||
      Array.isArray(deployment) || !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error(`${label} production deployment was not one version at 100 percent`);
  }
  return deployment.versions[0].version_id;
}

function versionBindings(version, expectedID, label) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedID || !UUID.test(String(version.id ?? "")) ||
      !Number.isSafeInteger(version.number) || version.number < 1 ||
      !OPAQUE_ETAG.test(String(version.resources?.script?.etag ?? "")) ||
      !Array.isArray(version.resources?.bindings)) {
    throw new Error(`${label} active Worker version identity was invalid`);
  }
  const bindings = new Map();
  for (const binding of version.resources.bindings) {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || binding.name === "" ||
        bindings.has(binding.name)) {
      throw new Error(`${label} active Worker binding inventory was invalid`);
    }
    bindings.set(binding.name, binding);
  }
  return bindings;
}

function plain(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    throw new Error(`${label} active Worker was missing exact ${name} binding`);
  }
  return binding.text;
}

function secret(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "secret_text" || Object.hasOwn(binding, "text")) {
    throw new Error(`${label} active Worker was missing exact ${name} secret binding`);
  }
}

function kvNamespace(bindings, name, label) {
  const binding = bindings.get(name);
  if (binding?.type !== "kv_namespace" ||
      !/^[0-9a-f]{32}$/.test(String(binding.namespace_id ?? ""))) {
    throw new Error(`${label} active Worker was missing exact ${name} KV binding`);
  }
  return binding.namespace_id;
}

function releaseIdentity(version, bindings, label, message) {
  const identity = Object.freeze({
    version: plain(bindings, "WITSELF_EDGE_RELEASE_VERSION", label),
    commit: plain(bindings, "WITSELF_EDGE_RELEASE_COMMIT", label),
    date: plain(bindings, "WITSELF_EDGE_RELEASE_DATE", label),
  });
  if (!RELEASE_VERSION.test(identity.version) ||
      !FULL_COMMIT.test(identity.commit) ||
      !RELEASE_DATE.test(identity.date) || Number.isNaN(Date.parse(identity.date))) {
    throw new Error(`${label} active Worker release identity was invalid`);
  }
  if (version.annotations?.["workers/tag"] !== `v${identity.version}` ||
      version.annotations?.["workers/message"] !== message(identity)) {
    throw new Error(`${label} active Worker release annotations were invalid`);
  }
  return identity;
}

function assertAbsent(bindings, names, label) {
  const present = names.filter((name) => bindings.has(name));
  if (present.length !== 0) {
    throw new Error(`${label} active Worker was not dark`);
  }
}

function publicKeyring(raw) {
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error("email edge route verification keyring was invalid");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("email edge route verification keyring was invalid");
  }
  const entries = Object.entries(parsed).sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0);
  if (entries.length < 1 || entries.length > 4 ||
      JSON.stringify(Object.fromEntries(entries)) !== raw ||
      entries.some(([keyID, encoded]) => {
        if (!KEY_ID.test(keyID) || typeof encoded !== "string" ||
            !PUBLIC_KEY.test(encoded)) return true;
        const decoded = Buffer.from(encoded, "base64");
        return decoded.byteLength !== 32 || decoded.toString("base64") !== encoded;
      })) {
    throw new Error("email edge route verification keyring was invalid");
  }
  return entries.map(([keyID]) => keyID);
}

function equalIdentity(left, right) {
  return left.version === right.version && left.commit === right.commit &&
    left.date === right.date;
}

export function verifyRouteSigningReadiness({
  controlPlaneDeployment,
  controlPlaneVersion,
  emailEdgeDeployment,
  emailEdgeVersion,
}) {
  const controlPlaneVersionID = activeVersionID(
    controlPlaneDeployment,
    "control plane",
  );
  const emailEdgeVersionID = activeVersionID(
    emailEdgeDeployment,
    "email edge",
  );
  const controlPlaneBindings = versionBindings(
    controlPlaneVersion,
    controlPlaneVersionID,
    "control plane",
  );
  const emailEdgeBindings = versionBindings(
    emailEdgeVersion,
    emailEdgeVersionID,
    "email edge",
  );

  const controlPlaneRelease = releaseIdentity(
    controlPlaneVersion,
    controlPlaneBindings,
    "control plane",
    (release) => `witself-control-plane v${release.version} ${release.commit}`,
  );
  const emailEdgeRelease = releaseIdentity(
    emailEdgeVersion,
    emailEdgeBindings,
    "email edge",
    (release) => `Witself v${release.version} agent-email edge ${release.commit}`,
  );
  if (!equalIdentity(controlPlaneRelease, emailEdgeRelease)) {
    throw new Error("control plane and email edge release identities did not match");
  }

  assertAbsent(
    controlPlaneBindings,
    EMAIL_DARK_SECRET_NAMES,
    "control plane",
  );
  assertAbsent(
    emailEdgeBindings,
    [CUSTOM_DOMAIN_DELIVERY_SECRET],
    "email edge",
  );
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    if (plain(emailEdgeBindings, name, "email edge") !== "false") {
      throw new Error("email edge active Worker managed delivery was not dark");
    }
  }

  const routeSigningKeyID = plain(
    controlPlaneBindings,
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    "control plane",
  );
  if (!KEY_ID.test(routeSigningKeyID)) {
    throw new Error("control plane active route signing key id was invalid");
  }
  secret(
    controlPlaneBindings,
    "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY",
    "control plane",
  );
  secret(controlPlaneBindings, "CONTROL_PLANE_EDGE_TOKEN", "control plane");
  secret(emailEdgeBindings, "CONTROL_PLANE_EDGE_TOKEN", "email edge");
  secret(emailEdgeBindings, "RELAY_ED25519_PRIVATE_KEY", "email edge");

  const controlPlaneDirectoryID = kvNamespace(
    controlPlaneBindings,
    "AGENT_EMAIL_DIRECTORY",
    "control plane",
  );
  const emailEdgeDirectoryID = kvNamespace(
    emailEdgeBindings,
    "EMAIL_DIRECTORY",
    "email edge",
  );
  if (controlPlaneDirectoryID !== emailEdgeDirectoryID) {
    throw new Error("control plane and email edge route directory bindings did not match");
  }

  const trustedKeyIDs = publicKeyring(plain(
    emailEdgeBindings,
    "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS",
    "email edge",
  ));
  if (!trustedKeyIDs.includes(routeSigningKeyID)) {
    throw new Error("control plane active route signing key was not trusted by email edge");
  }

  return Object.freeze({
    schema: "witself.agent-email-route-signing-readiness.v1",
    outcome: "verified",
    release: controlPlaneRelease,
    workers: {
      control_plane: {
        deployment_id: controlPlaneDeployment.id,
        version_id: controlPlaneVersion.id,
        version_number: controlPlaneVersion.number,
        provider_script_etag: controlPlaneVersion.resources.script.etag,
      },
      email_edge: {
        deployment_id: emailEdgeDeployment.id,
        version_id: emailEdgeVersion.id,
        version_number: emailEdgeVersion.number,
        provider_script_etag: emailEdgeVersion.resources.script.etag,
      },
    },
    dark: {
      control_plane_canonical_controls_absent: true,
      control_plane_custom_domain_controls_absent: true,
      email_edge_custom_domain_control_absent: true,
      email_edge_managed_alias_delivery_disabled: true,
      email_edge_canonical_delivery_disabled: true,
    },
    route_signing: {
      active_key_id: routeSigningKeyID,
      trusted_key_ids: trustedKeyIDs,
      trusted_key_count: trustedKeyIDs.length,
      control_plane_private_key_secret_bound: true,
      fallback_secret_binding_present_on_both_workers: true,
      email_edge_relay_secret_bound: true,
    },
    route_directory: {
      namespace_id: controlPlaneDirectoryID,
      shared_binding_verified: true,
    },
  });
}

function wranglerJSON(args, operation) {
  const result = spawnSync("wrangler", args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 5 * 1024 * 1024,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`could not ${operation} with Wrangler`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error(`Wrangler ${operation} output was not valid JSON`);
  }
}

function inspectWorker(name, label, inspect) {
  const deployment = inspect([
    "deployments", "status", "--name", name, "--json",
  ], `inspect the ${label} deployment`);
  const versionID = activeVersionID(deployment, label);
  const version = inspect([
    "versions", "view", versionID, "--name", name, "--json",
  ], `inspect the ${label} Worker version`);
  return { deployment, version };
}

export function inspectRouteSigningReadiness(inspect = wranglerJSON) {
  const controlPlane = inspectWorker(
    CONTROL_PLANE_WORKER,
    "control plane",
    inspect,
  );
  const emailEdge = inspectWorker(EMAIL_EDGE_WORKER, "email edge", inspect);
  return verifyRouteSigningReadiness({
    controlPlaneDeployment: controlPlane.deployment,
    controlPlaneVersion: controlPlane.version,
    emailEdgeDeployment: emailEdge.deployment,
    emailEdgeVersion: emailEdge.version,
  });
}

function main() {
  if (process.argv.length !== 2) {
    throw new Error("route-signing readiness verifier does not accept arguments");
  }
  process.stdout.write(
    `${JSON.stringify(inspectRouteSigningReadiness(), null, 2)}\n`,
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
