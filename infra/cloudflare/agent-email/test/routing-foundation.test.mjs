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

import { CloudflareAPI } from "../scripts/cloudflare.mjs";
import {
  applyRoutingFoundationPlan,
  createRoutingFoundationPlan,
  inspectRoutingFoundation,
  routingFoundationInternals,
  verifyRoutingFoundationPlan,
  verifyRoutingFoundationReceipt,
} from "../scripts/routing-foundation-lib.mjs";
import {
  parseRoutingFoundationArgs,
  RoutingFoundationCloudflareAPI,
  runRoutingFoundation,
} from "../scripts/routing-foundation.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
} from "../scripts/wrangler-environment.mjs";

const NOW = new Date("2026-08-15T12:00:00Z");
const accountID = PRODUCTION_CLOUDFLARE_ACCOUNT_ID;
const zoneID = "b".repeat(32);
const now = () => new Date(NOW);

function roleRule(localPart, index) {
  return {
    id: String(index).padStart(32, "0"),
    name: `${localPart} operator route`,
    enabled: true,
    matchers: [{
      type: "literal",
      field: "to",
      value: `${localPart}@witmail.net`,
    }],
    actions: [{ type: "forward", value: ["operator@example.com"] }],
    priority: index,
  };
}

class FakeCloudflare {
  constructor({ supportSubaddress = false } = {}) {
    this.accountID = accountID;
    this.zoneID = zoneID;
    this.zone = {
      id: zoneID,
      name: "witmail.net",
      status: "active",
      account: { id: accountID, name: "Witwave" },
    };
    this.settings = {
      id: zoneID,
      name: "witmail.net",
      enabled: true,
      status: "ready",
      skip_wizard: true,
      support_subaddress: supportSubaddress,
      modified: "2026-08-15T11:00:00.000Z",
    };
    this.catchAll = {
      id: "f".repeat(32),
      name: "Catch-all",
      enabled: false,
      matchers: [{ type: "all" }],
      actions: [{ type: "drop", value: [] }],
      source: "api",
    };
    this.rules = [roleRule("abuse", 1), roleRule("postmaster", 2)];
    this.calls = [];
    this.modification = 0;
  }

  async getZone() {
    this.calls.push(["getZone"]);
    return structuredClone(this.zone);
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

  async editEmailRoutingSettings(contract) {
    this.calls.push(["editEmailRoutingSettings", structuredClone(contract)]);
    this.modification += 1;
    this.settings = {
      ...this.settings,
      ...structuredClone(contract),
      modified: `2026-08-15T11:00:0${this.modification}.000Z`,
    };
    return structuredClone(this.settings);
  }
}

function runtime(calls = []) {
  return {
    operationsLease: {
      run: async (operation, work) => {
        calls.push(["leaseAcquire", operation]);
        assert.equal(operation, "email_routing_settings_apply");
        const evidence = () => ({
          schema_version:
            "witself.agent-email-operations-lease-evidence.v1",
          generation: 61,
          operation,
        });
        const guard = {
          signal: new AbortController().signal,
          fence: { generation: 61, operation },
          renew: async () => {
            calls.push(["leaseRenew"]);
            return evidence();
          },
          evidence,
        };
        try {
          const result = await work(guard);
          await guard.renew();
          return result;
        } finally {
          calls.push(["leaseRelease"]);
        }
      },
    },
  };
}

test("routing foundation status is read-only and proves the exact enable boundary", async () => {
  const api = new FakeCloudflare();
  const status = await inspectRoutingFoundation(api);
  assert.equal(status.domain, "witmail.net");
  assert.equal(status.routing.zone.active, true);
  assert.equal(status.routing.email_routing.contract.support_subaddress, false);
  assert.equal(status.routing.catch_all.enabled, false);
  assert.equal(
    status.routing.catch_all.witself_worker_targeted_enabled,
    false,
  );
  assert.equal(status.routing.role_routes.ready, true);
  assert.equal(
    status.routing.routing_rules.witself_worker_targeted,
    0,
  );
  assert.equal(status.ready_to_enable_subaddressing, true);
  assert.equal(status.ready_to_disable_subaddressing, false);
  assert.equal(
    api.calls.some(([operation]) => operation === "editEmailRoutingSettings"),
    false,
  );
  assert.doesNotMatch(JSON.stringify(status), /operator@example\.com/);
});

test("routing foundation plans are exact, short-lived, and tamper evident", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  const { apply_fence: ignored, ...body } = plan;
  assert.equal(
    plan.apply_fence.sha256,
    routingFoundationInternals.sha256(
      routingFoundationInternals.canonicalJSON(body),
    ),
  );
  assert.equal(
    verifyRoutingFoundationPlan(plan, plan.apply_fence.sha256, { now }),
    plan.apply_fence.sha256,
  );
  const tampered = structuredClone(plan);
  tampered.desired_settings.enabled = false;
  assert.throws(
    () => verifyRoutingFoundationPlan(
      tampered,
      plan.apply_fence.sha256,
      { now },
    ),
    /fence/,
  );
  assert.throws(
    () => verifyRoutingFoundationPlan(plan, plan.apply_fence.sha256, {
      now: () => new Date(NOW.valueOf() + 16 * 60_000),
    }),
    /expired/,
  );
  assert.throws(
    () => verifyRoutingFoundationPlan(
      plan,
      `${plan.apply_fence.sha256}\n`,
      { now },
    ),
    /plan-sha256/,
  );
});

test("routing foundation rechecks plan expiry immediately before PATCH", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  let calls = 0;
  const advancingClock = () => {
    calls += 1;
    return new Date(NOW.valueOf() + (calls >= 3 ? 16 * 60_000 : 0));
  };
  api.calls = [];
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      runtime(),
      { now: advancingClock },
    ),
    /expired/,
  );
  assert.equal(
    api.calls.some(([operation]) => operation === "editEmailRoutingSettings"),
    false,
  );
});

test("enable apply holds the shared lease and changes only subaddressing", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  const calls = [];
  api.calls = calls;
  const receipt = await applyRoutingFoundationPlan(
    plan,
    plan.apply_fence.sha256,
    api,
    runtime(calls),
    { now },
  );
  assert.deepEqual(calls[0], [
    "leaseAcquire",
    "email_routing_settings_apply",
  ]);
  assert.equal(calls.at(-1)[0], "leaseRelease");
  assert.deepEqual(
    calls.find(([operation]) => operation === "editEmailRoutingSettings")[1],
    { enabled: true, skip_wizard: true, support_subaddress: true },
  );
  assert.equal(api.settings.support_subaddress, true);
  assert.equal(receipt.before_settings.support_subaddress, false);
  assert.equal(receipt.after_settings.support_subaddress, true);
  assert.equal(receipt.operations_lease.generation, 61);
  assert.equal(
    verifyRoutingFoundationReceipt(receipt).receipt_fence.sha256,
    receipt.receipt_fence.sha256,
  );

  const malformed = structuredClone(receipt);
  malformed.after_settings.enabled = "true";
  assert.throws(
    () => verifyRoutingFoundationReceipt(malformed),
    /malformed/,
  );
});

test("provider drift refuses apply before settings mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  api.rules[0].priority = 99;
  api.calls = [];
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      runtime(),
      { now },
    ),
    /preconditions changed/,
  );
  assert.equal(
    api.calls.some(([operation]) => operation === "editEmailRoutingSettings"),
    false,
  );
});

test("last-moment provider drift refuses apply before settings mutation", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  const listRules = api.listRules.bind(api);
  let reads = 0;
  api.listRules = async () => {
    const rules = await listRules();
    reads += 1;
    if (reads === 2) rules[0].priority = 200;
    return rules;
  };
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      runtime(),
      { now },
    ),
    /changed immediately before mutation/,
  );
  assert.equal(
    api.calls.some(([operation]) => operation === "editEmailRoutingSettings"),
    false,
  );
});

test("enable verification failure restores the exact disabled predecessor", async () => {
  const api = new FakeCloudflare();
  const plan = await createRoutingFoundationPlan(api, "enable", { now });
  const edit = api.editEmailRoutingSettings.bind(api);
  let first = true;
  api.editEmailRoutingSettings = async (contract) => {
    const result = await edit(contract);
    if (first) {
      first = false;
      api.catchAll.name = "provider drift after subaddressing edit";
    }
    return result;
  };
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      runtime(),
      { now },
    ),
    /postcondition was not exact/,
  );
  assert.equal(api.settings.support_subaddress, false);
  assert.equal(
    api.calls.filter(
      ([operation]) => operation === "editEmailRoutingSettings",
    ).length,
    2,
  );
});

test("an ambiguous enable failure restores false while disable never re-enables", async () => {
  const enableAPI = new FakeCloudflare();
  const enablePlan = await createRoutingFoundationPlan(
    enableAPI,
    "enable",
    { now },
  );
  const enableEdit = enableAPI.editEmailRoutingSettings.bind(enableAPI);
  let throwOnce = true;
  enableAPI.editEmailRoutingSettings = async (contract) => {
    const result = await enableEdit(contract);
    if (throwOnce) {
      throwOnce = false;
      throw new Error("ambiguous provider failure");
    }
    return result;
  };
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      enablePlan,
      enablePlan.apply_fence.sha256,
      enableAPI,
      runtime(),
      { now },
    ),
    /ambiguous provider failure/,
  );
  assert.equal(enableAPI.settings.support_subaddress, false);

  const disableAPI = new FakeCloudflare({ supportSubaddress: true });
  const disablePlan = await createRoutingFoundationPlan(
    disableAPI,
    "disable",
    { now },
  );
  const disableEdit = disableAPI.editEmailRoutingSettings.bind(disableAPI);
  disableAPI.editEmailRoutingSettings = async (contract) => {
    await disableEdit(contract);
    throw new Error("ambiguous disable failure");
  };
  await assert.rejects(
    () => applyRoutingFoundationPlan(
      disablePlan,
      disablePlan.apply_fence.sha256,
      disableAPI,
      runtime(),
      { now },
    ),
    /ambiguous disable failure/,
  );
  assert.equal(disableAPI.settings.support_subaddress, false);
  assert.equal(
    disableAPI.calls.filter(
      ([operation]) => operation === "editEmailRoutingSettings",
    ).length,
    1,
  );
});

test("routing foundation refuses unsafe enable and disable boundaries", async () => {
  const enabledWorker = new FakeCloudflare();
  enabledWorker.rules.push({
    id: "e".repeat(32),
    name: "unexpected production route",
    enabled: true,
    matchers: [{
      type: "literal",
      field: "to",
      value: "agent.realm@witmail.net",
    }],
    actions: [{
      type: "worker",
      value: ["witself-agent-email-receive"],
    }],
  });
  await assert.rejects(
    () => createRoutingFoundationPlan(enabledWorker, "enable", { now }),
    /not ready/,
  );

  const disableWithWorker = new FakeCloudflare({ supportSubaddress: true });
  disableWithWorker.rules.push(structuredClone(enabledWorker.rules.at(-1)));
  await assert.rejects(
    () => createRoutingFoundationPlan(
      disableWithWorker,
      "disable",
      { now },
    ),
    /not ready/,
  );

  const disableWithWorkerCatchAll = new FakeCloudflare({
    supportSubaddress: true,
  });
  disableWithWorkerCatchAll.catchAll = {
    ...disableWithWorkerCatchAll.catchAll,
    enabled: true,
    actions: [{
      type: "worker",
      value: ["witself-agent-email-receive"],
    }],
  };
  await assert.rejects(
    () => createRoutingFoundationPlan(
      disableWithWorkerCatchAll,
      "disable",
      { now },
    ),
    /not ready/,
  );

  const malformedWorker = new FakeCloudflare();
  malformedWorker.rules.push({
    ...structuredClone(enabledWorker.rules.at(-1)),
    actions: [{
      type: "worker",
      value: "witself-agent-email-receive",
    }],
  });
  await assert.rejects(
    () => createRoutingFoundationPlan(malformedWorker, "enable", { now }),
    /inventory was invalid/,
  );
});

test("settings mutation exists only on the dedicated PATCH API", async () => {
  const base = new CloudflareAPI({
    accountID,
    zoneID,
    apiToken: "test-token",
  });
  assert.equal(base.editEmailRoutingSettings, undefined);
  const calls = [];
  const api = new RoutingFoundationCloudflareAPI({
    accountID,
    zoneID,
    apiToken: "test-token",
    fetchAPI: async (url, init) => {
      calls.push({ url, ...init });
      return Response.json({
        success: true,
        result: { support_subaddress: true },
      });
    },
  });
  const contract = {
    enabled: true,
    skip_wizard: true,
    support_subaddress: true,
  };
  await api.editEmailRoutingSettings(contract);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "PATCH");
  assert.match(calls[0].url, /\/zones\/[a-f0-9]+\/email\/routing$/);
  assert.deepEqual(JSON.parse(calls[0].body), contract);
  assert.equal(calls[0].headers.Authorization, "Bearer test-token");
});

test("lease settlement failure retains a dark pending reconciliation marker", async () => {
  const directory = mkdtempSync(join(
    tmpdir(),
    "witself-routing-foundation-settlement-",
  ));
  try {
    const api = new FakeCloudflare();
    const plan = await createRoutingFoundationPlan(
      api,
      "enable",
      { now: () => new Date() },
    );
    const planPath = join(directory, "enable-plan.json");
    const receiptPath = join(directory, "enable-receipt.json");
    writeFileSync(planPath, JSON.stringify(plan), { mode: 0o600 });
    const settlementFailure = {
      operationsLease: {
        run: async (operation, work) => {
          const evidence = () => ({
            schema_version:
              "witself.agent-email-operations-lease-evidence.v1",
            generation: 62,
            operation,
          });
          await work({
            signal: new AbortController().signal,
            fence: { generation: 62, operation },
            renew: async () => evidence(),
            evidence,
          });
          throw new Error("lease settlement failed");
        },
      },
    };
    await assert.rejects(
      () => runRoutingFoundation({
        mode: "apply",
        plan: planPath,
        planSHA256: plan.apply_fence.sha256,
        receiptOutput: receiptPath,
      }, {
        CLOUDFLARE_API_TOKEN: "token",
        CLOUDFLARE_ACCOUNT_ID: accountID,
        CLOUDFLARE_ZONE_ID: zoneID,
      }, { api, runtime: settlementFailure }),
      /lease settlement failed/,
    );
    assert.equal(api.settings.support_subaddress, true);
    assert.equal(api.catchAll.enabled, false);
    assert.equal(
      api.calls.filter(
        ([operation]) => operation === "editEmailRoutingSettings",
      ).length,
      1,
    );
    assert.deepEqual(JSON.parse(readFileSync(receiptPath, "utf8")), {
      schema:
        "witself.agent-email-routing-foundation-apply-pending.v1",
      action: "enable",
      plan_sha256: plan.apply_fence.sha256,
      state: "apply_started_receipt_not_committed",
    });
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("routing foundation CLI separates status, planning, and receipted apply", async () => {
  const planPath = "/private/tmp/witself-routing-foundation-plan.json";
  const receiptPath =
    "/private/tmp/witself-routing-foundation-receipt.json";
  assert.deepEqual(parseRoutingFoundationArgs(["status"]), {
    mode: "status",
  });
  assert.equal(parseRoutingFoundationArgs([
    "enable",
    "--output",
    planPath,
  ]).mode, "plan");
  assert.equal(parseRoutingFoundationArgs([
    "apply",
    "--plan",
    planPath,
    "--plan-sha256",
    "a".repeat(64),
    "--receipt-output",
    receiptPath,
  ]).mode, "apply");
  assert.throws(
    () => parseRoutingFoundationArgs([
      "enable",
      "--output",
      "relative-plan.json",
    ]),
    /canonical absolute path/,
  );
  assert.throws(
    () => parseRoutingFoundationArgs([
      "apply",
      "--plan",
      planPath,
      "--plan-sha256",
      "a".repeat(64),
    ]),
    /usage/,
  );
  await assert.rejects(
    () => runRoutingFoundation({ mode: "status" }, {
      CLOUDFLARE_API_TOKEN: "token",
      CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
      CLOUDFLARE_ZONE_ID: zoneID,
    }, { api: new FakeCloudflare() }),
    /must identify production account/,
  );

  const directory = mkdtempSync(join(
    tmpdir(),
    "witself-routing-foundation-",
  ));
  try {
    const plannedPath = join(directory, "enable-plan.json");
    const plannerAPI = new FakeCloudflare();
    const planned = await runRoutingFoundation({
      mode: "plan",
      action: "enable",
      output: plannedPath,
    }, {
      CLOUDFLARE_API_TOKEN: "token",
      CLOUDFLARE_ACCOUNT_ID: accountID,
      CLOUDFLARE_ZONE_ID: zoneID,
    }, { api: plannerAPI });
    assert.equal(planned.outcome, "created");
    assert.equal(statSync(plannedPath).mode & 0o777, 0o600);
    assert.equal(
      JSON.parse(readFileSync(plannedPath, "utf8")).apply_fence.sha256,
      planned.plan_sha256,
    );
    await assert.rejects(
      () => runRoutingFoundation({
        mode: "plan",
        action: "enable",
        output: plannedPath,
      }, {
        CLOUDFLARE_API_TOKEN: "token",
        CLOUDFLARE_ACCOUNT_ID: accountID,
        CLOUDFLARE_ZONE_ID: zoneID,
      }, { api: plannerAPI }),
      /EEXIST/,
    );

    const api = new FakeCloudflare({ supportSubaddress: true });
    const plan = await createRoutingFoundationPlan(
      api,
      "disable",
      { now: () => new Date() },
    );
    const actualPlanPath = join(directory, "disable-plan.json");
    const actualReceiptPath = join(directory, "disable-receipt.json");
    writeFileSync(actualPlanPath, JSON.stringify(plan), { mode: 0o600 });
    const result = await runRoutingFoundation({
      mode: "apply",
      plan: actualPlanPath,
      planSHA256: plan.apply_fence.sha256,
      receiptOutput: actualReceiptPath,
    }, {
      CLOUDFLARE_API_TOKEN: "token",
      CLOUDFLARE_ACCOUNT_ID: accountID,
      CLOUDFLARE_ZONE_ID: zoneID,
    }, { api, runtime: runtime() });
    const receipt = JSON.parse(readFileSync(actualReceiptPath, "utf8"));
    assert.equal(result.outcome, "verified");
    assert.equal(result.receipt_sha256, receipt.receipt_fence.sha256);
    assert.equal(receipt.after_settings.support_subaddress, false);
    assert.equal(statSync(actualReceiptPath).mode & 0o777, 0o600);

    const existing = join(directory, "existing-receipt.json");
    writeFileSync(existing, "operator-owned\n", { mode: 0o600 });
    const edits = api.calls.filter(
      ([operation]) => operation === "editEmailRoutingSettings",
    ).length;
    await assert.rejects(
      () => runRoutingFoundation({
        mode: "apply",
        plan: actualPlanPath,
        planSHA256: plan.apply_fence.sha256,
        receiptOutput: existing,
      }, {
        CLOUDFLARE_API_TOKEN: "token",
        CLOUDFLARE_ACCOUNT_ID: accountID,
        CLOUDFLARE_ZONE_ID: zoneID,
      }, { api, runtime: runtime() }),
      /EEXIST/,
    );
    assert.equal(
      api.calls.filter(
        ([operation]) => operation === "editEmailRoutingSettings",
      ).length,
      edits,
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
