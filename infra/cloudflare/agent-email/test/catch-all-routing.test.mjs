import assert from "node:assert/strict";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  applyCatchAllPlan,
  createCatchAllPlan,
  inspectCatchAll,
  verifyCatchAllPlan,
  verifyCatchAllReceipt,
} from "../scripts/catch-all-routing-lib.mjs";
import {
  CatchAllCloudflareAPI,
  parseCatchAllArgs,
  runCatchAllRoutes,
} from "../scripts/catch-all-routes.mjs";
import { EMAIL_DIRECTORY_TITLE } from "../scripts/cloudflare.mjs";
import {
  canonicalJSON,
  desiredPrimaryRule,
  sha256,
} from "../scripts/primary-routing-lib.mjs";
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
const manifest = {
  schema_version: 1,
  domain: "witmail.net",
  worker_name: "witself-agent-email-pilot",
  agents: ["alpha", "bravo", "charlie", "delta", "echo"].map((name, index) => ({
    agent_id: `agent_${"a".repeat(15)}${index + 2}`,
    realm_id: realmID,
    address: `${name}.${realmLabel}@witmail.net`,
  })),
};
const review = {
  change_id: "email-provider-review-2026-08-15",
  provider_contract_review_sha256: "e".repeat(64),
};

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function deployment(id, versionID) {
  return { id, strategy: "percentage", versions: [{ version_id: versionID, percentage: 100 }] };
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
    controller_revision: 9,
    updated_at: "2026-08-15T11:59:30Z",
    cache_ttl_seconds: 300,
    cell_audience: "gcp-prod-us-central1-core",
    ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
  });
}

function workerState() {
  const release = [
    plain("WITSELF_EDGE_RELEASE_VERSION", "1.2.3"),
    plain("WITSELF_EDGE_RELEASE_COMMIT", "d".repeat(40)),
    plain("WITSELF_EDGE_RELEASE_DATE", "2026-08-15T11:00:00Z"),
  ];
  const controlID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
  const edgeID = "01234567-89ab-4cde-8f01-23456789abcd";
  return {
    control_plane_deployment: deployment(
      "11111111-2222-4333-8444-555555555555",
      controlID,
    ),
    control_plane_version: {
      id: controlID,
      number: 120,
      resources: { bindings: [
        ...release,
        plain("AGENT_EMAIL_ROUTE_SIGNING_KEY_ID", ROUTE_SIGNING_KEY_ID),
        plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", cohortAccountID),
        { name: "AGENT_EMAIL_DIRECTORY", namespace_id: namespaceID, type: "kv_namespace" },
        secret("AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY"),
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        secret("CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED"),
        secret("CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED"),
      ] },
    },
    email_edge_deployment: deployment(
      "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
      edgeID,
    ),
    email_edge_version: {
      id: edgeID,
      number: 25,
      resources: { bindings: [
        ...release,
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", cohortAccountID),
        plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", ROUTE_PUBLIC_KEY_ENV),
        plain("CONTROL_PLANE_URL", "https://control.example/"),
        { name: "EMAIL_DIRECTORY", namespace_id: namespaceID, type: "kv_namespace" },
        secret("CONTROL_PLANE_EDGE_TOKEN"),
        secret("RELAY_ED25519_PRIVATE_KEY"),
        plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "true"),
        plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
      ] },
    },
  };
}

function runtime() {
  return {
    inspectWorkers: async () => structuredClone(workerState()),
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
    this.rules.push(...manifest.agents.map((agent, index) => ({
      ...desiredPrimaryRule(manifest, agent.address, true, index + 10),
      id: String(index + 10).padStart(32, "0"),
    })));
    this.calls = [];
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
  async getKVJSON() {
    this.calls.push(["getKVJSON"]);
    return signedProjection();
  }
  async replaceCatchAll(contract) {
    this.calls.push(["replaceCatchAll", contract.enabled]);
    this.catchAll = {
      id: this.catchAll.id,
      source: "api",
      ...structuredClone(contract),
    };
    return structuredClone(this.catchAll);
  }
}

const now = () => new Date(NOW);

test("catch-all status is read-only and reports a fully active primary canary", async () => {
  const api = new FakeCloudflare();
  const status = await inspectCatchAll(api, runtime(), manifest, { now });
  assert.equal(status.catch_all.enabled, false);
  assert.equal(status.catch_all.action, "drop");
  assert.equal(status.primary_canary.enabled, 5);
  assert.equal(status.ready_to_plan_enable, true);
  assert.equal(api.calls.some(([name]) => name === "replaceCatchAll"), false);
  assert.doesNotMatch(JSON.stringify(status), /operator@example|cell\.example/);
});

test("enable requires external review and an exact expiring plan fence", async () => {
  const api = new FakeCloudflare();
  await assert.rejects(
    () => createCatchAllPlan(api, runtime(), manifest, "enable", { now }),
    /external review/,
  );
  const plan = await createCatchAllPlan(api, runtime(), manifest, "enable", { review, now });
  const { apply_fence: ignored, ...body } = plan;
  assert.equal(plan.apply_fence.sha256, sha256(canonicalJSON(body)));
  assert.equal(verifyCatchAllPlan(plan, plan.apply_fence.sha256, { now }), plan.apply_fence.sha256);
  const receipt = await applyCatchAllPlan(
    plan,
    plan.apply_fence.sha256,
    api,
    runtime(),
    { now: () => new Date(NOW.valueOf() + 30_000) },
  );
  assert.equal(api.catchAll.enabled, true);
  assert.equal(api.catchAll.actions[0].type, "worker");
  assert.equal(verifyCatchAllReceipt(receipt).receipt_fence.sha256, receipt.receipt_fence.sha256);
  assert.equal(api.calls.filter(([name]) => name === "replaceCatchAll").length, 1);
  const forgedReceipt = structuredClone(receipt);
  forgedReceipt.before_contract.unreviewed = true;
  const { receipt_fence: ignoredReceiptFence, ...forgedReceiptBody } = forgedReceipt;
  forgedReceipt.receipt_fence.sha256 = sha256(canonicalJSON(forgedReceiptBody));
  assert.throws(() => verifyCatchAllReceipt(forgedReceipt), /malformed/);

  const rollback = await createCatchAllPlan(api, runtime(), manifest, "rollback", {
    rollbackReceipt: receipt,
    now,
  });
  const rolledBack = await applyCatchAllPlan(
    rollback,
    rollback.apply_fence.sha256,
    api,
    runtime(),
    { now },
  );
  assert.equal(rolledBack.after_contract.enabled, false);
  assert.equal(api.catchAll.enabled, false);
  assert.equal(api.catchAll.actions[0].type, "drop");
});

test("disable is independently fenced and its receipt cannot re-enable via rollback", async () => {
  const api = new FakeCloudflare();
  api.catchAll = {
    ...api.catchAll,
    name: "Witself agent email catch-all",
    enabled: true,
    actions: [{ type: "worker", value: ["witself-agent-email-pilot"] }],
  };
  const unavailable = {
    inspectWorkers: async () => { throw new Error("unavailable"); },
    getControlPlaneProjection: async () => { throw new Error("unavailable"); },
  };
  const plan = await createCatchAllPlan(api, unavailable, manifest, "disable", { now });
  const receipt = await applyCatchAllPlan(plan, plan.apply_fence.sha256, api, unavailable, { now });
  assert.equal(api.catchAll.enabled, false);
  await assert.rejects(
    () => createCatchAllPlan(api, unavailable, manifest, "rollback", {
      rollbackReceipt: receipt,
      now,
    }),
    /rollback receipt/,
  );
});

test("stale guard state refuses apply before mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createCatchAllPlan(api, runtime(), manifest, "enable", { review, now });
  api.rules.find((rule) => rule.name === "abuse operator route").priority = 99;
  await assert.rejects(
    () => applyCatchAllPlan(plan, plan.apply_fence.sha256, api, runtime(), { now }),
    /preconditions changed/,
  );
  assert.equal(api.calls.some(([name]) => name === "replaceCatchAll"), false);
});

test("last-moment catch-all drift refuses apply before mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createCatchAllPlan(api, runtime(), manifest, "enable", { review, now });
  const getCatchAll = api.getCatchAll.bind(api);
  let reads = 0;
  api.getCatchAll = async () => {
    const current = await getCatchAll();
    reads += 1;
    return reads === 2 ? { ...current, name: "changed after plan reconstruction" } : current;
  };
  await assert.rejects(
    () => applyCatchAllPlan(plan, plan.apply_fence.sha256, api, runtime(), { now }),
    /changed immediately before mutation/,
  );
  assert.equal(api.calls.some(([name]) => name === "replaceCatchAll"), false);
});

test("enable post-verification failure restores the exact disabled predecessor", async () => {
  const api = new FakeCloudflare();
  const plan = await createCatchAllPlan(api, runtime(), manifest, "enable", { review, now });
  const replace = api.replaceCatchAll.bind(api);
  let changed = false;
  api.replaceCatchAll = async (contract) => {
    const result = await replace(contract);
    if (contract.enabled && !changed) {
      changed = true;
      api.rules.find((rule) => rule.name.startsWith("witself-agent-email-primary-canary:")).priority = 999;
    }
    return result;
  };
  await assert.rejects(
    () => applyCatchAllPlan(plan, plan.apply_fence.sha256, api, runtime(), { now }),
    /post-mutation verification failed/,
  );
  assert.equal(api.catchAll.enabled, false);
  assert.equal(api.catchAll.actions[0].type, "drop");
});

test("catch-all plan tampering and expiry fail closed", async () => {
  const api = new FakeCloudflare();
  const plan = await createCatchAllPlan(api, runtime(), manifest, "enable", { review, now });
  const tampered = structuredClone(plan);
  tampered.desired_catch_all.enabled = false;
  assert.throws(
    () => verifyCatchAllPlan(tampered, plan.apply_fence.sha256, { now }),
    /malformed|fence/,
  );
  assert.throws(
    () => verifyCatchAllPlan(plan, plan.apply_fence.sha256, {
      now: () => new Date(NOW.valueOf() + 16 * 60_000),
    }),
    /expired/,
  );
  assert.throws(
    () => verifyCatchAllPlan(plan, `${plan.apply_fence.sha256}\n`, { now }),
    /plan-sha256/,
  );
});

test("catch-all API mutation exists only on the explicit subclass", async () => {
  const calls = [];
  const api = new CatchAllCloudflareAPI({
    accountID,
    zoneID,
    namespaceID,
    apiToken: "test-token",
    fetchAPI: async (url, init) => {
      calls.push({ url, ...init });
      return Response.json({ success: true, result: { enabled: false } });
    },
  });
  await api.replaceCatchAll({
    name: "Catch-all",
    enabled: false,
    matchers: [{ type: "all" }],
    actions: [{ type: "drop", value: [] }],
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "PUT");
  assert.match(calls[0].url, /\/email\/routing\/rules\/catch_all$/);
  assert.equal(calls[0].headers.Authorization, "Bearer test-token");
});

test("catch-all CLI keeps planning separate and requires an apply receipt", () => {
  assert.equal(parseCatchAllArgs(["status", "manifest.json"]).mode, "status");
  assert.equal(parseCatchAllArgs([
    "enable", "manifest.json",
    "--change-id", review.change_id,
    "--provider-review-sha256", review.provider_contract_review_sha256,
    "--output", "plan.json",
  ]).mode, "plan");
  const apply = parseCatchAllArgs([
    "apply", "--plan", "plan.json", "--plan-sha256", "a".repeat(64),
    "--receipt-output", "receipt.json", "--confirm-enable-witmail-net",
  ]);
  assert.equal(apply.confirmEnable, true);
  assert.throws(
    () => parseCatchAllArgs(["apply", "--plan", "plan.json", "--plan-sha256", "a".repeat(64)]),
    /usage/,
  );
});

test("catch-all apply reserves the protected receipt before any mutation", async () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-catch-all-receipt-"));
  try {
    const api = new FakeCloudflare();
    api.catchAll = {
      ...api.catchAll,
      name: "Witself agent email catch-all",
      enabled: true,
      actions: [{ type: "worker", value: ["witself-agent-email-pilot"] }],
    };
    const plan = await createCatchAllPlan(api, {}, manifest, "disable", {
      now: () => new Date(),
    });
    const planPath = join(directory, "disable-plan.json");
    const receiptPath = join(directory, "existing-receipt.json");
    writeFileSync(planPath, JSON.stringify(plan), { mode: 0o600 });
    writeFileSync(receiptPath, "operator-owned\n", { mode: 0o600 });
    await assert.rejects(
      () => runCatchAllRoutes({
        mode: "apply",
        plan: planPath,
        planSHA256: plan.apply_fence.sha256,
        receiptOutput: receiptPath,
        confirmEnable: false,
      }, {
        CLOUDFLARE_API_TOKEN: "token",
        CLOUDFLARE_ACCOUNT_ID: accountID,
        CLOUDFLARE_ZONE_ID: zoneID,
        EMAIL_DIRECTORY_KV_ID: namespaceID,
      }, { api, runtime: {} }),
      /EEXIST/,
    );
    assert.equal(api.calls.some(([name]) => name === "replaceCatchAll"), false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("catch-all apply durably replaces its pending journal with a mode-0600 receipt", async () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-catch-all-receipt-"));
  try {
    const api = new FakeCloudflare();
    api.catchAll = {
      ...api.catchAll,
      name: "Witself agent email catch-all",
      enabled: true,
      actions: [{ type: "worker", value: ["witself-agent-email-pilot"] }],
    };
    const plan = await createCatchAllPlan(api, {}, manifest, "disable", {
      now: () => new Date(),
    });
    const planPath = join(directory, "disable-plan.json");
    const receiptPath = join(directory, "disable-receipt.json");
    writeFileSync(planPath, JSON.stringify(plan), { mode: 0o600 });
    const result = await runCatchAllRoutes({
      mode: "apply",
      plan: planPath,
      planSHA256: plan.apply_fence.sha256,
      receiptOutput: receiptPath,
      confirmEnable: false,
    }, {
      CLOUDFLARE_API_TOKEN: "token",
      CLOUDFLARE_ACCOUNT_ID: accountID,
      CLOUDFLARE_ZONE_ID: zoneID,
      EMAIL_DIRECTORY_KV_ID: namespaceID,
    }, { api, runtime: {} });
    const receipt = JSON.parse(readFileSync(receiptPath, "utf8"));
    assert.equal(result.receipt_sha256, receipt.receipt_fence.sha256);
    assert.equal(receipt.schema, "witself.agent-email-catch-all-receipt.v1");
    assert.equal(statSync(receiptPath).mode & 0o777, 0o600);
    assert.equal(api.catchAll.enabled, false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
