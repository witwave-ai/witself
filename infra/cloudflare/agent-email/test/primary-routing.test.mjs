import assert from "node:assert/strict";
import test from "node:test";

import { EMAIL_DIRECTORY_TITLE } from "../scripts/cloudflare.mjs";
import {
  applyPrimaryRoutingPlan,
  canonicalJSON,
  createPrimaryRoutingPlan,
  inspectPrimaryCanary,
  normalizePrimaryCanaryManifest,
  primaryRuleName,
  sha256,
  verifyPrimaryRoutingPlan,
} from "../scripts/primary-routing-lib.mjs";
import {
  parsePrimaryRouteArgs,
  primaryRoutingRuntime,
} from "../scripts/primary-routes.mjs";
import { realmRouteKey } from "../src/directory.mjs";
import {
  ROUTE_PUBLIC_KEY_ENV,
  ROUTE_SIGNING_KEY_ID,
  signTestRouteProjection,
} from "./route-signature-fixture.mjs";

const NOW = new Date("2026-08-15T12:00:00Z");
const accountID = "a".repeat(32);
const zoneID = "b".repeat(32);
const namespaceID = "c".repeat(32);
const realmID = "realm_abcdefghijkl2345";
const realmLabel = "abcdefghijkl2345";
const cohortAccountID = "acc_abcdefghijkl2345";
const controlPlaneDeploymentID = "11111111-2222-4333-8444-555555555555";
const controlPlaneVersionID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const edgeDeploymentID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const edgeVersionID = "01234567-89ab-4cde-8f01-23456789abcd";

const manifest = Object.freeze({
  schema_version: 1,
  domain: "witmail.net",
  worker_name: "witself-agent-email-pilot",
  agents: Object.freeze([
    ["aaaaaaaaaaaaaaa2", "alpha"],
    ["aaaaaaaaaaaaaaa3", "bravo"],
    ["aaaaaaaaaaaaaaa4", "charlie"],
    ["aaaaaaaaaaaaaaa5", "delta"],
    ["aaaaaaaaaaaaaaa6", "echo"],
  ].map(([id, name]) => Object.freeze({
    agent_id: `agent_${id}`,
    realm_id: realmID,
    address: `${name}.${realmLabel}@witmail.net`,
  }))),
});

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function deployment(id, versionID) {
  return {
    id,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function workerFixtures({ canonical = false, alias = false, controlGates = true } = {}) {
  const releaseBindings = [
    plain("WITSELF_EDGE_RELEASE_VERSION", "1.2.3"),
    plain("WITSELF_EDGE_RELEASE_COMMIT", "d".repeat(40)),
    plain("WITSELF_EDGE_RELEASE_DATE", "2026-08-15T11:00:00Z"),
  ];
  const controlBindings = [
    ...releaseBindings,
    plain("AGENT_EMAIL_ROUTE_SIGNING_KEY_ID", ROUTE_SIGNING_KEY_ID),
    plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", cohortAccountID),
    { name: "AGENT_EMAIL_DIRECTORY", namespace_id: namespaceID, type: "kv_namespace" },
    secret("AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"),
    secret("CONTROL_PLANE_EDGE_TOKEN"),
    ...(controlGates ? [
      secret("CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED"),
      secret("CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED"),
    ] : []),
  ];
  const edgeBindings = [
    ...releaseBindings,
    plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
    plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", cohortAccountID),
    plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", ROUTE_PUBLIC_KEY_ENV),
    plain("CONTROL_PLANE_URL", "https://control.example/"),
    { name: "EMAIL_DIRECTORY", namespace_id: namespaceID, type: "kv_namespace" },
    secret("CONTROL_PLANE_EDGE_TOKEN"),
    secret("RELAY_ED25519_PRIVATE_KEY"),
    plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", String(canonical)),
    plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", String(alias)),
  ];
  return {
    control_plane_deployment: deployment(controlPlaneDeploymentID, controlPlaneVersionID),
    control_plane_version: {
      id: controlPlaneVersionID,
      number: 120,
      resources: { bindings: controlBindings },
    },
    email_edge_deployment: deployment(edgeDeploymentID, edgeVersionID),
    email_edge_version: {
      id: edgeVersionID,
      number: 25,
      resources: { bindings: edgeBindings },
    },
  };
}

function signedProjection() {
  return signTestRouteProjection({
    schema_version: 2,
    domain: "witmail.net",
    account_id: cohortAccountID,
    realm_label: realmLabel,
    realm_id: realmID,
    route_kind: "canonical",
    state: "applied",
    controller_revision: 7,
    updated_at: "2026-08-15T11:59:30Z",
    cache_ttl_seconds: 300,
    cell_audience: "gcp-prod-us-central1-core",
    ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
  });
}

class FakeCloudflare {
  constructor() {
    this.accountID = accountID;
    this.zoneID = zoneID;
    this.namespaceID = namespaceID;
    this.settings = { enabled: true, status: "ready", support_subaddress: true };
    this.catchAll = {
      id: "f".repeat(32),
      name: "Catch-all",
      enabled: false,
      matchers: [{ type: "all" }],
      actions: [{ type: "drop", value: [] }],
      source: "api",
    };
    this.rules = ["abuse", "postmaster"].map((name, index) => ({
      id: String(index + 1).padStart(32, "0"),
      name: `${name} operator route`,
      enabled: true,
      matchers: [{ type: "literal", field: "to", value: `${name}@witmail.net` }],
      actions: [{ type: "forward", value: ["operator@example.com"] }],
      priority: index + 1,
    }));
    this.projection = signedProjection();
    this.calls = [];
    this.nextID = 10;
  }
  async getNamespace() {
    this.calls.push(["getNamespace"]);
    return { id: namespaceID, title: EMAIL_DIRECTORY_TITLE };
  }
  async getEmailRoutingSettings() {
    this.calls.push(["getEmailRoutingSettings"]);
    return structuredClone(this.settings);
  }
  async getCatchAll() {
    this.calls.push(["getCatchAll"]);
    return structuredClone(this.catchAll);
  }
  async listRules() {
    this.calls.push(["listRules"]);
    return structuredClone(this.rules);
  }
  async getKVJSON(key) {
    this.calls.push(["getKVJSON", key]);
    assert.equal(key, realmRouteKey("witmail.net", realmLabel));
    return structuredClone(this.projection);
  }
  async createRule(rule) {
    this.calls.push(["createRule"]);
    const created = {
      ...structuredClone(rule),
      id: String(this.nextID++).padStart(32, "0"),
      priority: this.nextID,
    };
    this.rules.push(created);
    return structuredClone(created);
  }
  async updateRule(id, rule) {
    this.calls.push(["updateRule", id, rule.enabled]);
    const index = this.rules.findIndex((item) => item.id === id);
    assert.notEqual(index, -1);
    this.rules[index] = { ...structuredClone(rule), id };
    return structuredClone(this.rules[index]);
  }
  async deleteRule(id) {
    this.calls.push(["deleteRule", id]);
    this.rules = this.rules.filter((rule) => rule.id !== id);
  }
}

function runtime(options = {}) {
  const workers = workerFixtures(options);
  return {
    inspectWorkers: async () => structuredClone(workers),
    getControlPlaneReadiness: async () => ({
      schema_version: "witself.agent-email-managed-delivery-readiness.v1",
      managed_delivery: {
        cohort: {
          schema: "witself.agent-email-managed-delivery-cohort.v1",
          account_count: 1,
          allowlist_sha256: sha256(cohortAccountID),
          empty: false,
        },
        canonical_inventory_enabled: true,
        canonical_delivery_enabled: true,
        alias_authority_activation_enabled: false,
      },
    }),
    getControlPlaneProjection: async () => signedProjection(),
  };
}

const now = () => new Date(NOW);

test("primary manifest accepts only canonical witmail.net address ownership", () => {
  assert.deepEqual(normalizePrimaryCanaryManifest(manifest), manifest);
  for (const mutate of [
    (value) => { value.domain = "agent-mail.witwave.ai"; },
    (value) => { value.agents = value.agents.slice(0, 4); },
    (value) => { value.agents[0].address = `alpha.zzzzzzzzzzzzzzzz@witmail.net`; },
    (value) => { value.agents[0].address = `alpha.${realmLabel}@other.example`; },
    (value) => { value.agents[0].agent_id += "\n"; },
    (value) => { value.agents[0].realm_id += "\n"; },
    (value) => { value.agents[0].unexpected = true; },
  ]) {
    const candidate = structuredClone(manifest);
    mutate(candidate);
    assert.throws(() => normalizePrimaryCanaryManifest(candidate));
  }
});

test("status proves signed primary projection and returns no route destinations", async () => {
  const api = new FakeCloudflare();
  const status = await inspectPrimaryCanary(api, runtime(), manifest, { now });
  assert.equal(status.ready_for_prepare, true);
  assert.equal(status.ready_for_activate, false);
  assert.equal(status.readiness.projection_count, 1);
  assert.equal(status.readiness.activation_ready, false);
  assert.equal(status.routing.role_routes.ready, true);
  const serialized = JSON.stringify(status);
  assert.doesNotMatch(serialized, /operator@example|cell\.example|route_signature/);
  assert.equal(api.calls.some(([name]) => name === "putKV"), false);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("status is not prepare-ready without both operator role routes", async () => {
  const api = new FakeCloudflare();
  api.rules = api.rules.filter((rule) => rule.name !== "abuse operator route");
  const status = await inspectPrimaryCanary(api, runtime(), manifest, { now });
  assert.equal(status.routing.role_routes.ready, false);
  assert.equal(status.ready_for_prepare, false);
  assert.equal(status.ready_for_activate, false);
});

test("prepare is a short-lived fenced rule-only operation", async () => {
  const api = new FakeCloudflare();
  const plan = await createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now });
  const { apply_fence: ignored, ...body } = plan;
  assert.equal(plan.apply_fence.sha256, sha256(canonicalJSON(body)));
  assert.equal(verifyPrimaryRoutingPlan(plan, plan.apply_fence.sha256, { now }), plan.apply_fence.sha256);
  const receipt = await applyPrimaryRoutingPlan(
    plan,
    plan.apply_fence.sha256,
    api,
    runtime(),
    { now: () => new Date(NOW.valueOf() + 60_000) },
  );
  assert.equal(receipt.outcome, "verified");
  const canary = api.rules.filter((rule) => rule.name.startsWith("witself-agent-email-primary-canary:"));
  assert.equal(canary.length, 5);
  assert.equal(canary.every((rule) => rule.enabled === false), true);
  assert.equal(api.calls.some(([name]) => name === "putKV" || name === "deleteKV"), false);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("activation requires all three canonical gates and fails closed on a partial update", async () => {
  const api = new FakeCloudflare();
  const prepare = await createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now });
  await applyPrimaryRoutingPlan(prepare, prepare.apply_fence.sha256, api, runtime(), { now });
  await assert.rejects(
    () => createPrimaryRoutingPlan(api, runtime(), manifest, "activate", { now }),
    /delivery gates were not ready/,
  );

  const activeRuntime = runtime({ canonical: true });
  const activation = await createPrimaryRoutingPlan(api, activeRuntime, manifest, "activate", { now });
  const update = api.updateRule.bind(api);
  let enabled = 0;
  api.updateRule = async (id, rule) => {
    if (rule.enabled === true && ++enabled === 3) throw new Error("injected activation failure");
    return update(id, rule);
  };
  await assert.rejects(
    () => applyPrimaryRoutingPlan(activation, activation.apply_fence.sha256, api, activeRuntime, { now }),
    /injected activation failure/,
  );
  const canary = api.rules.filter((rule) => rule.name.startsWith("witself-agent-email-primary-canary:"));
  assert.equal(canary.every((rule) => rule.enabled === false), true);
});

test("stale plans and guard drift refuse mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now });
  api.catchAll.name = "changed elsewhere";
  await assert.rejects(
    () => applyPrimaryRoutingPlan(plan, plan.apply_fence.sha256, api, runtime(), { now }),
    /preconditions changed/,
  );
  assert.equal(api.calls.some(([name]) => name === "createRule"), false);

  assert.throws(
    () => verifyPrimaryRoutingPlan(plan, plan.apply_fence.sha256, {
      now: () => new Date(NOW.valueOf() + 16 * 60_000),
    }),
    /expired/,
  );
});

test("last-moment routing drift refuses primary mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now });
  const getCatchAll = api.getCatchAll.bind(api);
  let reads = 0;
  api.getCatchAll = async () => {
    const current = await getCatchAll();
    reads += 1;
    return reads === 2 ? { ...current, name: "changed after plan reconstruction" } : current;
  };
  await assert.rejects(
    () => applyPrimaryRoutingPlan(plan, plan.apply_fence.sha256, api, runtime(), { now }),
    /changed immediately before mutation/,
  );
  assert.equal(api.calls.some(([name]) => name === "createRule"), false);
});

test("disable and remove do not depend on projection or gate readiness", async () => {
  const api = new FakeCloudflare();
  const goodRuntime = runtime({ canonical: true });
  const prepare = await createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now });
  await applyPrimaryRoutingPlan(prepare, prepare.apply_fence.sha256, api, runtime(), { now });
  const activate = await createPrimaryRoutingPlan(api, goodRuntime, manifest, "activate", { now });
  await applyPrimaryRoutingPlan(activate, activate.apply_fence.sha256, api, goodRuntime, { now });
  api.rules = api.rules.filter((rule) => !rule.name.startsWith("postmaster operator"));
  const unavailable = {
    inspectWorkers: async () => { throw new Error("unavailable"); },
    getControlPlaneProjection: async () => { throw new Error("unavailable"); },
  };
  const disable = await createPrimaryRoutingPlan(api, unavailable, manifest, "disable", { now });
  await applyPrimaryRoutingPlan(disable, disable.apply_fence.sha256, api, unavailable, { now });
  assert.equal(
    api.rules.filter((rule) => rule.name.startsWith("witself-agent-email-primary-canary:"))
      .every((rule) => rule.enabled === false),
    true,
  );
  const remove = await createPrimaryRoutingPlan(api, unavailable, manifest, "remove", { now });
  await applyPrimaryRoutingPlan(remove, remove.apply_fence.sha256, api, unavailable, { now });
  assert.equal(
    api.rules.some((rule) => rule.name.startsWith("witself-agent-email-primary-canary:")),
    false,
  );
});

test("primary lifecycle refuses stale managed and same-address unmanaged rules", async () => {
  for (const rule of [
    {
      id: "9".repeat(32),
      name: primaryRuleName(`stale.${realmLabel}@witmail.net`),
      enabled: false,
      matchers: [{ type: "literal", field: "to", value: `stale.${realmLabel}@witmail.net` }],
      actions: [{ type: "worker", value: ["witself-agent-email-pilot"] }],
    },
    {
      id: "8".repeat(32),
      name: "unmanaged",
      enabled: false,
      matchers: [{ type: "literal", field: "to", value: manifest.agents[0].address }],
      actions: [{ type: "forward", value: ["operator@example.com"] }],
    },
  ]) {
    const api = new FakeCloudflare();
    api.rules.push(rule);
    await assert.rejects(
      () => createPrimaryRoutingPlan(api, runtime(), manifest, "prepare", { now }),
      /stale|conflict/,
    );
  }
});

test("primary CLI exposes plan generation separately from apply", () => {
  assert.deepEqual(parsePrimaryRouteArgs(["status", "manifest.json"]), {
    mode: "status", manifest: "manifest.json", output: "", plan: "", planSHA256: "",
  });
  assert.equal(
    parsePrimaryRouteArgs(["activate", "manifest.json", "--output", "plan.json"]).mode,
    "plan",
  );
  assert.equal(
    parsePrimaryRouteArgs(["apply", "--plan", "plan.json", "--plan-sha256", "a".repeat(64)]).mode,
    "apply",
  );
  assert.throws(() => parsePrimaryRouteArgs(["activate", "manifest.json"]), /usage/);
});

test("primary runtime inspects exact active Workers and authenticated control-plane routes", async () => {
  const inspections = [];
  const requests = [];
  const workers = workerFixtures();
  const inspect = (args) => {
    inspections.push(args);
    const name = args[args.indexOf("--name") + 1];
    const deployment = args[0] === "deployments";
    if (name === "witself-control-plane") {
      return structuredClone(deployment
        ? workers.control_plane_deployment
        : workers.control_plane_version);
    }
    return structuredClone(deployment
      ? workers.email_edge_deployment
      : workers.email_edge_version);
  };
  const runtimeClient = primaryRoutingRuntime({
    CONTROL_PLANE_EDGE_TOKEN: "x".repeat(32),
  }, {
    inspect,
    fetchAPI: async (url, init) => {
      requests.push({ url: String(url), ...init });
      return Response.json({ schema_version: "fixture" });
    },
  });
  const result = await runtimeClient.inspectWorkers();
  assert.equal(result.email_edge_version.id, edgeVersionID);
  assert.deepEqual(inspections, [
    ["deployments", "status", "--name", "witself-control-plane", "--json"],
    ["versions", "view", controlPlaneVersionID, "--name", "witself-control-plane", "--json"],
    ["deployments", "status", "--name", "witself-agent-email-pilot", "--json"],
    ["versions", "view", edgeVersionID, "--name", "witself-agent-email-pilot", "--json"],
  ]);
  await runtimeClient.getControlPlaneReadiness("https://control.example/");
  await runtimeClient.getControlPlaneProjection(
    "https://control.example/", "witmail.net", realmLabel,
  );
  assert.deepEqual(requests.map(({ url }) => url), [
    "https://control.example/v1/email/managed-delivery/readiness",
    `https://control.example/v1/email/realm-routes/witmail.net/${realmLabel}`,
  ]);
  assert.equal(
    requests.every(({ headers }) => headers.Authorization === `Bearer ${"x".repeat(32)}`),
    true,
  );
});
