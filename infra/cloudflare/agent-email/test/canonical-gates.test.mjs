import assert from "node:assert/strict";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  applyCanonicalGatesPlan,
  CANONICAL_GATE_NAMES,
  canonicalGatesInternals,
  createCanonicalGatesPlan,
  inspectCanonicalGates,
  verifyCanonicalGatesPlan,
  verifyCanonicalGatesReceipt,
} from "../scripts/canonical-gates-lib.mjs";
import {
  CanonicalGatesCloudflareAPI,
  parseCanonicalGatesArgs,
  runCanonicalGates,
} from "../scripts/canonical-gates.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
} from "../scripts/wrangler-environment.mjs";
import {
  CANONICAL_EMAIL_DARK_SECRET_NAMES,
} from "../../control-plane/scripts/assert-custom-domain-dark.mjs";

const NOW = new Date("2026-08-15T12:00:00Z");
const now = () => new Date(NOW);
const accountID = PRODUCTION_CLOUDFLARE_ACCOUNT_ID;
const founderAccount = "acc_abcdefghijkl2345";

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function secret(name) {
  return { name, type: "secret_text" };
}

function deployment(sequence) {
  const digit = String(sequence).padStart(12, "0");
  const deploymentID = `10000000-0000-4000-8000-${digit}`;
  const versionID = `20000000-0000-4000-8000-${digit}`;
  return {
    deployment: {
      id: deploymentID,
      strategy: "percentage",
      versions: [{ version_id: versionID, percentage: 100 }],
    },
    versionID,
  };
}

class FakeCloudflare {
  constructor({ gates = "absent", cohort = founderAccount } = {}) {
    this.accountID = accountID;
    this.sequence = 1;
    this.calls = [];
    this.failNext = "";
    this.wrongReadback = false;
    this.bindings = [
      plain("WITSELF_EDGE_RELEASE_VERSION", "1.2.3"),
      plain("WITSELF_EDGE_RELEASE_COMMIT", "d".repeat(40)),
      plain("WITSELF_EDGE_RELEASE_DATE", "2026-08-15T11:00:00Z"),
      plain("CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", cohort),
      { name: "REALM_EMAIL_ALIASES", type: "durable_object_namespace",
        namespace_id: "a".repeat(32) },
      secret("CONTROL_PLANE_EDGE_TOKEN"),
    ];
    this.secrets = [secret("CONTROL_PLANE_EDGE_TOKEN")];
    if (gates === "present") this.setGates(true, true);
    if (gates === "mixed") this.setGates(true, false);
  }

  setGates(inventory, delivery) {
    const desired = new Set([
      ...(delivery ? [CANONICAL_GATE_NAMES[0]] : []),
      ...(inventory ? [CANONICAL_GATE_NAMES[1]] : []),
    ]);
    this.bindings = this.bindings
      .filter(({ name }) => !CANONICAL_GATE_NAMES.includes(name));
    this.secrets = this.secrets
      .filter(({ name }) => !CANONICAL_GATE_NAMES.includes(name));
    for (const name of CANONICAL_GATE_NAMES) {
      if (desired.has(name)) {
        this.bindings.push(secret(name));
        this.secrets.push(secret(name));
      }
    }
  }

  async listControlPlaneSecrets() {
    this.calls.push(["list"]);
    return structuredClone(this.secrets);
  }

  workers() {
    const current = deployment(this.sequence);
    return {
      control_plane_deployment: current.deployment,
      control_plane_version: {
        id: current.versionID,
        number: 100 + this.sequence,
        resources: { bindings: structuredClone(this.bindings) },
      },
    };
  }

  async patchControlPlaneSecrets(body) {
    this.calls.push(["patch", structuredClone(body)]);
    const failure = this.failNext;
    this.failNext = "";
    if (failure === "before") throw new Error("provider request failed");
    const enable = Object.values(body).every((value) => value !== null);
    if (enable) {
      this.setGates(true, !this.wrongReadback);
    } else {
      this.setGates(false, false);
    }
    this.sequence += 1;
    if (failure === "after") throw new Error("provider response was lost");
    return Object.fromEntries(this.secrets.map((entry) => [entry.name, entry]));
  }
}

function runtime(api, calls = []) {
  return {
    inspectControlPlane: async () => structuredClone(api.workers()),
    inspectWorkers: async () => structuredClone(api.workers()),
    operationsLease: {
      run: async (operation, work) => {
        calls.push(["leaseAcquire", operation]);
        assert.equal(operation, "control_plane_canonical_gates_apply");
        const evidence = () => ({
          schema_version: "witself.agent-email-operations-lease-evidence.v1",
          generation: 71,
          operation,
        });
        const guard = {
          signal: new AbortController().signal,
          fence: { generation: 71, operation },
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

test("canonical gate status is value-free, read-only, and Founder-fenced", async () => {
  assert.deepEqual(
    CANONICAL_GATE_NAMES,
    [...CANONICAL_EMAIL_DARK_SECRET_NAMES].sort(),
  );
  const api = new FakeCloudflare();
  const status = await inspectCanonicalGates(api, runtime(api));
  assert.equal(status.control_plane.gate_state, "absent");
  assert.equal(status.control_plane.founder_cohort.account_count, 1);
  assert.equal(status.ready_to_enable, true);
  assert.equal(status.ready_to_disable, false);
  assert.equal(api.calls.some(([kind]) => kind === "patch"), false);
  assert.doesNotMatch(JSON.stringify(status), new RegExp(founderAccount));
  assert.doesNotMatch(JSON.stringify(status), /CONTROL_PLANE_EDGE_TOKEN.*true/);
});

test("canonical gate plans are mode-independent, 15-minute, and tamper evident", async () => {
  const api = new FakeCloudflare();
  const plan = await createCanonicalGatesPlan(api, runtime(api), "enable", { now });
  assert.equal(Date.parse(plan.expires_at) - Date.parse(plan.created_at), 900_000);
  assert.equal(
    verifyCanonicalGatesPlan(plan, plan.apply_fence.sha256, { now }),
    plan.apply_fence.sha256,
  );
  const tampered = structuredClone(plan);
  tampered.desired_gate_state = "absent";
  assert.throws(
    () => verifyCanonicalGatesPlan(tampered, plan.apply_fence.sha256, { now }),
    /malformed|fence/,
  );
  assert.throws(
    () => verifyCanonicalGatesPlan(plan, plan.apply_fence.sha256, {
      now: () => new Date(NOW.valueOf() + 16 * 60_000),
    }),
    /expired/,
  );
});

test("enable uses one atomic bulk patch and preserves every unrelated binding", async () => {
  const api = new FakeCloudflare();
  const operationCalls = [];
  const workerRuntime = runtime(api, operationCalls);
  const plan = await createCanonicalGatesPlan(api, workerRuntime, "enable", { now });
  api.calls = [];
  const receipt = await applyCanonicalGatesPlan(
    plan,
    plan.apply_fence.sha256,
    api,
    workerRuntime,
    { now },
  );
  const patches = api.calls.filter(([kind]) => kind === "patch");
  assert.equal(patches.length, 1);
  assert.deepEqual(patches[0][1], canonicalGatesInternals.mutationBody("enable"));
  assert.equal(patches[0][1][CANONICAL_GATE_NAMES[0]].text, "true");
  assert.equal(receipt.before.gate_state, "absent");
  assert.equal(receipt.after.gate_state, "present");
  assert.equal(receipt.before.release_sha256, receipt.after.release_sha256);
  assert.equal(receipt.operations_lease.generation, 71);
  assert.equal(
    verifyCanonicalGatesReceipt(receipt).receipt_fence.sha256,
    receipt.receipt_fence.sha256,
  );
  assert.deepEqual(operationCalls[0], [
    "leaseAcquire", "control_plane_canonical_gates_apply",
  ]);
  assert.equal(operationCalls.at(-1)[0], "leaseRelease");
});

test("disable atomically deletes both gates and never re-enables on ambiguity", async () => {
  const api = new FakeCloudflare({ gates: "present" });
  const workerRuntime = runtime(api);
  const plan = await createCanonicalGatesPlan(api, workerRuntime, "disable", { now });
  api.failNext = "after";
  await assert.rejects(
    () => applyCanonicalGatesPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      workerRuntime,
      { now },
    ),
    /provider response was lost/,
  );
  const patches = api.calls.filter(([kind]) => kind === "patch");
  assert.equal(patches.length, 1);
  assert.deepEqual(patches[0][1], canonicalGatesInternals.mutationBody("disable"));
  assert.equal(api.workers().control_plane_version.resources.bindings.some(
    ({ name }) => CANONICAL_GATE_NAMES.includes(name),
  ), false);
});

test("ambiguous enable performs one fail-closed bulk rollback to both absent", async () => {
  const api = new FakeCloudflare();
  const workerRuntime = runtime(api);
  const plan = await createCanonicalGatesPlan(api, workerRuntime, "enable", { now });
  api.failNext = "after";
  await assert.rejects(
    () => applyCanonicalGatesPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      workerRuntime,
      { now },
    ),
    /provider response was lost/,
  );
  const patches = api.calls.filter(([kind]) => kind === "patch");
  assert.equal(patches.length, 2);
  assert.deepEqual(patches[1][1], canonicalGatesInternals.mutationBody("disable"));
  assert.equal((await inspectCanonicalGates(api, workerRuntime)).control_plane.gate_state,
    "absent");
});

test("failed enable readback rolls back and mixed states cannot be planned", async () => {
  const api = new FakeCloudflare();
  const workerRuntime = runtime(api);
  const plan = await createCanonicalGatesPlan(api, workerRuntime, "enable", { now });
  api.wrongReadback = true;
  await assert.rejects(
    () => applyCanonicalGatesPlan(
      plan,
      plan.apply_fence.sha256,
      api,
      workerRuntime,
      { now },
    ),
    /readback/,
  );
  assert.equal((await inspectCanonicalGates(api, workerRuntime)).control_plane.gate_state,
    "absent");

  const mixed = new FakeCloudflare({ gates: "mixed" });
  const status = await inspectCanonicalGates(mixed, runtime(mixed));
  assert.equal(status.control_plane.gate_state, "mixed");
  assert.equal(status.ready_to_enable, false);
  assert.equal(status.ready_to_disable, false);
  await assert.rejects(
    () => createCanonicalGatesPlan(mixed, runtime(mixed), "enable", { now }),
    /not ready/,
  );
});

test("state drift and plan expiry immediately before PATCH cause no mutation", async () => {
  const drifted = new FakeCloudflare();
  const driftRuntime = runtime(drifted);
  const driftPlan = await createCanonicalGatesPlan(
    drifted, driftRuntime, "enable", { now },
  );
  drifted.bindings.push(plain("UNRELATED_AFTER_REVIEW", "changed"));
  await assert.rejects(
    () => applyCanonicalGatesPlan(
      driftPlan,
      driftPlan.apply_fence.sha256,
      drifted,
      driftRuntime,
      { now },
    ),
    /preconditions changed/,
  );
  assert.equal(drifted.calls.some(([kind]) => kind === "patch"), false);

  const expired = new FakeCloudflare();
  const expiredRuntime = runtime(expired);
  const expiredPlan = await createCanonicalGatesPlan(
    expired, expiredRuntime, "enable", { now },
  );
  let calls = 0;
  const advancingClock = () => {
    calls += 1;
    return new Date(NOW.valueOf() + (calls >= 3 ? 16 * 60_000 : 0));
  };
  expired.calls = [];
  await assert.rejects(
    () => applyCanonicalGatesPlan(
      expiredPlan,
      expiredPlan.apply_fence.sha256,
      expired,
      expiredRuntime,
      { now: advancingClock },
    ),
    /expired/,
  );
  assert.equal(expired.calls.some(([kind]) => kind === "patch"), false);
});

test("secret inventories reject duplicates and never accept returned values", async () => {
  for (const secrets of [
    [secret("DUPLICATE"), secret("DUPLICATE")],
    [{ name: "LEAK", type: "secret_text", text: "private" }],
  ]) {
    assert.throws(
      () => canonicalGatesInternals.normalizeSecretInventory(secrets),
      /invalid/,
    );
  }
});

test("Cloudflare API uses the official list and merge-patch bulk endpoints", async () => {
  const calls = [];
  const api = new CanonicalGatesCloudflareAPI({
    accountID,
    apiToken: "test-token",
    fetchAPI: async (url, init) => {
      calls.push({ url: String(url), ...init });
      const result = init.method === "PATCH" ? {} : [secret("UNRELATED")];
      return Response.json({ success: true, result });
    },
  });
  await api.listControlPlaneSecrets();
  const desired = canonicalGatesInternals.mutationBody("enable");
  await api.patchControlPlaneSecrets(desired);
  assert.match(calls[0].url,
    /\/workers\/scripts\/witself-control-plane\/secrets$/);
  assert.equal(calls[0].method, "GET");
  assert.match(calls[1].url,
    /\/workers\/scripts\/witself-control-plane\/secrets-bulk$/);
  assert.equal(calls[1].method, "PATCH");
  assert.deepEqual(JSON.parse(calls[1].body), { secrets: desired });
  assert.equal(Object.hasOwn(JSON.parse(calls[1].body).secrets, "UNRELATED"), false);
});

test("canonical gates CLI creates private plans and durable pending receipts", async () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-canonical-gates-"));
  try {
    const planPath = join(directory, "plan.json");
    const receiptPath = join(directory, "receipt.json");
    const api = new FakeCloudflare({ gates: "present" });
    const workerRuntime = runtime(api);
    const env = {
      CLOUDFLARE_ACCOUNT_ID: accountID,
      CLOUDFLARE_API_TOKEN: "test-token",
    };
    const planned = await runCanonicalGates({
      mode: "plan", action: "disable", output: planPath,
    }, env, { api, runtime: workerRuntime });
    assert.equal(planned.outcome, "created");
    assert.equal(statSync(planPath).mode & 0o777, 0o600);
    const plan = JSON.parse(readFileSync(planPath, "utf8"));
    api.failNext = "before";
    await assert.rejects(
      () => runCanonicalGates({
        mode: "apply",
        plan: planPath,
        planSHA256: plan.apply_fence.sha256,
        receiptOutput: receiptPath,
      }, env, { api, runtime: workerRuntime }),
      /provider request failed/,
    );
    assert.equal(statSync(receiptPath).mode & 0o777, 0o600);
    assert.equal(
      JSON.parse(readFileSync(receiptPath, "utf8")).state,
      "apply_started_receipt_not_committed",
    );
    assert.throws(
      () => parseCanonicalGatesArgs(["enable", "--output", "plan.json"]),
      /canonical absolute path/,
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
