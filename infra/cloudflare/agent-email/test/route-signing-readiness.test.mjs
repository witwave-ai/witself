import assert from "node:assert/strict";
import test from "node:test";

import {
  inspectRouteSigningReadiness,
  verifyRouteSigningReadiness,
} from "../scripts/route-signing-readiness.mjs";

const release = Object.freeze({
  version: "1.2.3",
  commit: "a".repeat(40),
  date: "2026-08-09T12:00:00-06:00",
});
const keyID = "route-2026-08";
const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
const keyring = JSON.stringify({ [keyID]: publicKey });
const directoryID = "f".repeat(32);
const controlPlaneDeploymentID = "11111111-2222-4333-8444-555555555555";
const controlPlaneVersionID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const emailEdgeDeploymentID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const emailEdgeVersionID = "01234567-89ab-4cde-8f01-23456789abcd";

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function kv(name, namespaceID = directoryID) {
  return { name, namespace_id: namespaceID, type: "kv_namespace" };
}

function deployment(id, versionID) {
  return {
    id,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function releasedVersion({
  id,
  number,
  label,
  bindings,
  identity = release,
}) {
  const message = label === "control-plane"
    ? `witself-control-plane v${identity.version} ${identity.commit}`
    : `Witself v${identity.version} agent-email edge ${identity.commit}`;
  return {
    id,
    number,
    annotations: {
      "workers/tag": `v${identity.version}`,
      "workers/message": message,
    },
    resources: {
      script: { etag: label === "control-plane" ? "b".repeat(64) : "edge-etag-1234567890" },
      bindings: [
        plain("WITSELF_EDGE_RELEASE_VERSION", identity.version),
        plain("WITSELF_EDGE_RELEASE_COMMIT", identity.commit),
        plain("WITSELF_EDGE_RELEASE_DATE", identity.date),
        ...bindings,
      ],
    },
  };
}

function fixtures() {
  return structuredClone({
    controlPlaneDeployment: deployment(
      controlPlaneDeploymentID,
      controlPlaneVersionID,
    ),
    controlPlaneVersion: releasedVersion({
      id: controlPlaneVersionID,
      number: 115,
      label: "control-plane",
      bindings: [
        plain("AGENT_EMAIL_ROUTE_SIGNING_KEY_ID", keyID),
        kv("AGENT_EMAIL_DIRECTORY"),
        secret("AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"),
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        plain("UNRELATED_CONTROL_PLANE_BINDING", "kept"),
      ],
    }),
    emailEdgeDeployment: deployment(emailEdgeDeploymentID, emailEdgeVersionID),
    emailEdgeVersion: releasedVersion({
      id: emailEdgeVersionID,
      number: 22,
      label: "email-edge",
      bindings: [
        plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", keyring),
        kv("EMAIL_DIRECTORY"),
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        secret("RELAY_ED25519_PRIVATE_KEY"),
        plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
        plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false"),
        plain("UNRELATED_EMAIL_EDGE_BINDING", "kept"),
      ],
    }),
  });
}

test("readiness attests one shared dark release without returning key material", () => {
  const result = verifyRouteSigningReadiness(fixtures());
  assert.equal(result.outcome, "verified");
  assert.deepEqual(result.release, release);
  assert.equal(result.workers.control_plane.version_id, controlPlaneVersionID);
  assert.equal(result.workers.email_edge.version_id, emailEdgeVersionID);
  assert.equal(result.dark.control_plane_canonical_controls_absent, true);
  assert.equal(result.dark.control_plane_custom_domain_controls_absent, true);
  assert.equal(result.dark.email_edge_custom_domain_control_absent, true);
  assert.equal(result.route_signing.active_key_id, keyID);
  assert.deepEqual(result.route_signing.trusted_key_ids, [keyID]);
  assert.equal(result.route_signing.trusted_key_count, 1);
  assert.deepEqual(result.route_directory, {
    namespace_id: directoryID,
    shared_binding_verified: true,
  });
  const serialized = JSON.stringify(result);
  assert.equal(serialized.includes(publicKey), false);
  assert.equal(serialized.includes("secret-bound-value"), false);
});

test("readiness rejects split or indeterminate production traffic", () => {
  for (const key of ["controlPlaneDeployment", "emailEdgeDeployment"]) {
    const candidate = fixtures();
    candidate[key].versions = [
      { version_id: candidate[key].versions[0].version_id, percentage: 90 },
      { version_id: controlPlaneDeploymentID, percentage: 10 },
    ];
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /one version at 100 percent/,
    );
  }
});

test("readiness rejects any control-plane or edge activation gate", () => {
  for (const name of [
    "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED",
    "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED",
    "CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED",
  ]) {
    const candidate = fixtures();
    candidate.controlPlaneVersion.resources.bindings.push(secret(name));
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /control plane active Worker was not dark/,
    );
  }
  {
    const candidate = fixtures();
    candidate.emailEdgeVersion.resources.bindings.push(
      secret("AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED"),
    );
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /email edge active Worker was not dark/,
    );
  }
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    const candidate = fixtures();
    candidate.emailEdgeVersion.resources.bindings.find(
      (binding) => binding.name === name,
    ).text = "true";
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /managed delivery was not dark/,
    );
  }
});

test("readiness permits the existing alias authority workflow while delivery stays dark", () => {
  const candidate = fixtures();
  candidate.controlPlaneVersion.resources.bindings.push(
    secret("CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED"),
  );
  assert.doesNotThrow(() => verifyRouteSigningReadiness(candidate));
});

test("readiness requires a canonical keyring containing the active signer", () => {
  for (const raw of [
    JSON.stringify({ "different-key": publicKey }),
    `{ "${keyID}":"${publicKey}"}`,
    `{"${keyID}":"not-base64"}`,
  ]) {
    const candidate = fixtures();
    candidate.emailEdgeVersion.resources.bindings.find(
      (binding) => binding.name === "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS",
    ).text = raw;
    assert.throws(() => verifyRouteSigningReadiness(candidate));
  }
  {
    const candidate = fixtures();
    candidate.controlPlaneVersion.resources.bindings.find(
      (binding) => binding.name === "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
    ).text = " INVALID ";
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /key id was invalid/,
    );
  }
});

test("readiness requires value-free secret bindings on both Workers", () => {
  for (const [versionName, secretName] of [
    ["controlPlaneVersion", "AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"],
    ["controlPlaneVersion", "CONTROL_PLANE_EDGE_TOKEN"],
    ["emailEdgeVersion", "CONTROL_PLANE_EDGE_TOKEN"],
    ["emailEdgeVersion", "RELAY_ED25519_PRIVATE_KEY"],
  ]) {
    const candidate = fixtures();
    const binding = candidate[versionName].resources.bindings.find(
      (item) => item.name === secretName,
    );
    binding.text = "secret-bound-value";
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      new RegExp(`${secretName} secret binding`),
    );
  }
});

test("readiness requires both Workers to share the exact route directory", () => {
  const candidate = fixtures();
  candidate.emailEdgeVersion.resources.bindings.find(
    (binding) => binding.name === "EMAIL_DIRECTORY",
  ).namespace_id = "e".repeat(32);
  assert.throws(
    () => verifyRouteSigningReadiness(candidate),
    /route directory bindings did not match/,
  );
});

test("readiness rejects release identity and annotation drift", () => {
  {
    const candidate = fixtures();
    candidate.emailEdgeVersion.resources.bindings.find(
      (binding) => binding.name === "WITSELF_EDGE_RELEASE_COMMIT",
    ).text = "c".repeat(40);
    candidate.emailEdgeVersion.annotations["workers/message"] =
      `Witself v${release.version} agent-email edge ${"c".repeat(40)}`;
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /release identities did not match/,
    );
  }
  for (const versionName of ["controlPlaneVersion", "emailEdgeVersion"]) {
    const candidate = fixtures();
    candidate[versionName].annotations["workers/tag"] = "v1.2.2";
    assert.throws(
      () => verifyRouteSigningReadiness(candidate),
      /release annotations were invalid/,
    );
  }
});

test("production inspection follows each exact active version through Wrangler JSON", () => {
  const source = fixtures();
  const calls = [];
  const responses = [
    source.controlPlaneDeployment,
    source.controlPlaneVersion,
    source.emailEdgeDeployment,
    source.emailEdgeVersion,
  ];
  const result = inspectRouteSigningReadiness((args, operation) => {
    calls.push({ args, operation });
    return responses[calls.length - 1];
  });
  assert.equal(result.outcome, "verified");
  assert.deepEqual(calls.map(({ args }) => args), [
    ["deployments", "status", "--name", "witself-control-plane", "--json"],
    [
      "versions", "view", controlPlaneVersionID,
      "--name", "witself-control-plane", "--json",
    ],
    [
      "deployments", "status", "--name", "witself-agent-email-pilot", "--json",
    ],
    [
      "versions", "view", emailEdgeVersionID,
      "--name", "witself-agent-email-pilot", "--json",
    ],
  ]);
});
