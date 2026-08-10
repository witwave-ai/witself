import assert from "node:assert/strict";
import test from "node:test";

import {
  expectedDeployment,
  releaseMessage,
  verifyDeployment,
} from "../scripts/deployment-identity.mjs";

const release = Object.freeze({
  version: "1.2.3",
  tag: "v1.2.3",
  commit: "a".repeat(40),
  date: "2026-08-09T12:00:00Z",
  clean: true,
});
const env = {
  EMAIL_DIRECTORY_KV_ID: "b".repeat(32),
  RELAY_KEY_ID: "pilot-2026-07",
  CONTROL_PLANE_URL: "https://self.witwave.ai/",
  AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS:
    JSON.stringify({ "route-2026-08": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" }),
  AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
  REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
  REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
};
const expected = expectedDeployment(env, release);
const deploymentID = "11111111-2222-4333-8444-555555555555";
const versionID = "66666666-7777-4888-8999-aaaaaaaaaaaa";

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function fixtures() {
  const status = {
    id: deploymentID,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
  const version = {
    id: versionID,
    number: 24,
    metadata: { created_on: "2026-08-09T12:01:00Z" },
    annotations: {
      "workers/tag": release.tag,
      "workers/message": releaseMessage(release),
    },
    resources: {
      script: {
        etag: "provider-artifact-etag-1234567890",
        handlers: ["email"],
      },
      script_runtime: {
        compatibility_date: "2026-07-21",
        compatibility_flags: ["global_fetch_strictly_public"],
      },
      bindings: [
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"),
        plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
        plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", env.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS),
        { name: "CONTROL_PLANE_EDGE_TOKEN", type: "secret_text" },
        plain("CONTROL_PLANE_URL", env.CONTROL_PLANE_URL),
        { name: "EMAIL_DIRECTORY", namespace_id: env.EMAIL_DIRECTORY_KV_ID, type: "kv_namespace" },
        { name: "EMAIL_EDGE_METRICS", dataset: "witself_agent_email_edge", type: "analytics_engine" },
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
        { name: "RELAY_ED25519_PRIVATE_KEY", type: "secret_text" },
        plain("RELAY_KEY_ID", env.RELAY_KEY_ID),
        plain("WITSELF_EDGE_RELEASE_COMMIT", release.commit),
        plain("WITSELF_EDGE_RELEASE_DATE", release.date),
        plain("WITSELF_EDGE_RELEASE_VERSION", release.version),
      ],
    },
  };
  return structuredClone({ status, version });
}

test("deployment attestation accepts one exact email-only release", () => {
  const { status, version } = fixtures();
  const result = verifyDeployment(status, version, expected);
  assert.equal(result.outcome, "verified");
  assert.equal(result.release.commit, release.commit);
  assert.equal(result.cloudflare.deployment_id, deploymentID);
  assert.equal(result.cloudflare.version_id, versionID);
  assert.equal(result.cloudflare.provider_script_etag, "provider-artifact-etag-1234567890");
  assert.deepEqual(result.runtime.handlers, ["email"]);
  assert.equal(result.bindings.custom_domain_delivery_enabled, false);
  assert.deepEqual(result.bindings.managed_delivery_cohort, {
    account_count: 0,
    allowlist_sha256:
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  });
});

test("deployment expectations summarize but never expose the active cohort", () => {
  const active = expectedDeployment({
    ...env,
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST:
      "acc_aaaaaaaaaaaaaaaa,acc_bbbbbbbbbbbbbbbb",
  }, release);
  assert.equal(active.managedDeliveryAccountCount, 2);
  assert.match(active.managedDeliveryAllowlistSHA256, /^[0-9a-f]{64}$/);
  for (const value of [
    "*",
    "acc_bbbbbbbbbbbbbbbb,acc_aaaaaaaaaaaaaaaa",
    "acc_aaaaaaaaaaaaaaaa ",
  ]) {
    assert.throws(() => expectedDeployment({
      ...env,
      AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: value,
    }, release), /allowlist is invalid/);
  }
});

test("deployment attestation rejects split traffic and identity drift", () => {
  {
    const { status, version } = fixtures();
    status.versions = [
      { version_id: versionID, percentage: 90 },
      { version_id: deploymentID, percentage: 10 },
    ];
    assert.throws(
      () => verifyDeployment(status, version, expected),
      /one version at 100 percent/,
    );
  }
  for (const mutate of [
    (value) => { value.annotations["workers/tag"] = "v1.2.2"; },
    (value) => { value.resources.script.handlers = ["email", "fetch"]; },
    (value) => {
      value.resources.bindings.find((item) =>
        item.name === "WITSELF_EDGE_RELEASE_COMMIT").text = "b".repeat(40);
    },
  ]) {
    const { status, version } = fixtures();
    mutate(version);
    assert.throws(() => verifyDeployment(status, version, expected));
  }
});

test("deployment attestation rejects unknown bindings and a custom-domain gate", () => {
  for (const binding of [
    plain("UNKNOWN_BINDING", "surprise"),
    { name: "AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED", type: "secret_text" },
  ]) {
    const { status, version } = fixtures();
    version.resources.bindings.push(binding);
    assert.throws(
      () => verifyDeployment(status, version, expected),
      /binding inventory did not match/,
    );
  }
});

test("deployment attestation can verify an unannotated secret-only successor", () => {
  const { status, version } = fixtures();
  version.annotations = { "workers/triggered_by": "secret_update" };
  assert.doesNotThrow(() => verifyDeployment(
    status,
    version,
    expected,
    { requireAnnotations: false },
  ));
  assert.throws(
    () => verifyDeployment(status, version, expected, { requireAnnotations: true }),
    /annotations did not match/,
  );
});

test("deployment expectations require explicit managed delivery values", () => {
  for (const name of [
    "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  ]) {
    const candidate = { ...env };
    delete candidate[name];
    assert.throws(
      () => expectedDeployment(candidate, release),
      new RegExp(`${name} must be explicitly true or false`),
    );
  }
});
