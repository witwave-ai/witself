import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  handleInternalBridgeRequest,
  matchAdminPolicyPath,
} from "../src/bridge.mjs";
import contract from "../src/plan-contract.json" with { type: "json" };
import catalog from "../../../../web/plans/plans.json" with { type: "json" };

function targetSnapshot(fields) {
  const target = {
    revision: 19,
    plan: "free",
    limits: {},
    policies: {},
    features: [],
    ...fields,
  };
  const ordered = (values) => Object.fromEntries(
    Object.keys(values).sort().map((key) => [key, values[key]]),
  );
  target.snapshot_hash = createHash("sha256").update(JSON.stringify({
    plan: target.plan,
    limits: ordered(target.limits),
    policies: ordered(target.policies),
    features: [...target.features].sort(),
  })).digest("hex");
  return target;
}

async function assertTargetValidation(fields, accepted, violations = []) {
  const target = targetSnapshot(fields);
  for (const operation of ["plan-fit", "plan-fit-apply"]) {
    let cellCalls = 0;
    const response = await handleInternalBridgeRequest(
      new Request(`https://bridge.example/v1/internal/accounts/acct_1:${operation}`, {
        method: "POST",
        headers: {
          Authorization: "Bearer test-bridge",
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ schema_version: "witself.v0", target }),
      }),
      {
        INTERNAL_BRIDGE_TOKEN: "test-bridge",
        DIRECTORY: {
          async get(key) {
            if (key === "acct:acct_1") return { cell: "cell-a" };
            if (key === "cell:cell-a") {
              return { endpoint: "https://cell.example", provision_token: "test-cell" };
            }
            return null;
          },
        },
      },
      async () => {
        cellCalls += 1;
        return Response.json({
          schema_version: "witself.v0",
          account_id: "acct_1",
          target_plan: target.plan,
          target_snapshot_hash: target.snapshot_hash,
          violations,
        });
      },
    );
    const body = await response.json();
    if (!accepted) {
      assert.equal(response.status, 400, operation);
      assert.equal(cellCalls, 0, "invalid targets must not reach a cell");
    } else if (operation === "plan-fit") {
      assert.equal(response.status, 200, operation);
      assert.equal(cellCalls, 1);
      for (const violation of violations) {
        assert.ok(body.violations.some((actual) =>
          actual.dimension === violation.dimension &&
          actual.scope === violation.scope && actual.used === violation.used &&
          actual.max === violation.max));
      }
    } else {
      // No authorities are installed in this validation-only fixture. Reaching
      // this specific error proves validation passed before authority access.
      assert.equal(response.status, 502, operation);
      assert.equal(body.error, "agent email plan-fit authority is unavailable");
      assert.equal(cellCalls, 0);
    }
  }
}

test("both Worker plan validators accept every current Go catalog snapshot", async () => {
  assert.equal(contract.schema_version, "witself.plan-contract.v1");
  assert.ok(catalog.plans.find((plan) => plan.id === "free").limits.operator_seats);
  for (const plan of catalog.plans) {
    await assertTargetValidation({
      plan: plan.id,
      limits: plan.limits,
      policies: plan.policies,
      features: plan.features,
    }, true);
  }
});

test("plan-fit accepts the Go cell's account-scoped operator seat refusal", async () => {
  await assertTargetValidation({ limits: { operator_seats: 1 } }, true, [{
    code: "limit_exceeded",
    dimension: "operator_seats",
    scope: "account",
    used: 2,
    max: 1,
    subject_count: 1,
  }]);
});

test("both Worker plan validators enforce every generated limit boundary", async (t) => {
  for (const [key, maximum] of Object.entries(contract.limit_maximums)) {
    await t.test(key, async () => {
      for (const value of [0, maximum]) {
        await assertTargetValidation({ limits: { [key]: value } }, true);
      }
      for (const value of [-1, maximum + 1, 0.5]) {
        await assertTargetValidation({ limits: { [key]: value } }, false);
      }
    });
  }
});

test("both Worker plan validators enforce every generated policy boundary", async (t) => {
  for (const [key, { minimum, maximum }] of Object.entries(contract.policy_bounds)) {
    await t.test(key, async () => {
      for (const value of [minimum, maximum]) {
        await assertTargetValidation({ policies: { [key]: value } }, true);
      }
      for (const value of [minimum - 1, maximum + 1, minimum + 0.5]) {
        await assertTargetValidation({ policies: { [key]: value } }, false);
      }
    });
  }
});

test("both Worker plan validators accept only the generated feature vocabulary", async () => {
  await assertTargetValidation({ features: contract.feature_keys }, true);
  for (const key of contract.feature_keys) {
    await assertTargetValidation({ features: [key] }, true);
    await assertTargetValidation({ features: [key, key] }, false);
  }
  for (const key of [...Object.keys(contract.limit_maximums), ...Object.keys(contract.policy_bounds), "unknown_feature"]) {
    await assertTargetValidation({ features: [key] }, false);
  }
});

test("both Worker plan validators reject unknown and misplaced limit and policy keys", async () => {
  for (const key of [...contract.feature_keys, ...Object.keys(contract.policy_bounds), "agent_email_received_per_source_minute", "unknown_limit", "toString"]) {
    await assertTargetValidation({ limits: { [key]: 1 } }, false);
  }
  for (const key of [...contract.feature_keys, ...Object.keys(contract.limit_maximums), "unknown_policy", "toString"]) {
    await assertTargetValidation({ policies: { [key]: 1 } }, false);
  }
});

test("admin limit override routes use the same generated Go limit vocabulary", () => {
  const path = (key) => `/v1/admin/accounts/acct_1/limit-overrides/${key}`;
  for (const key of Object.keys(contract.limit_maximums)) {
    assert.ok(matchAdminPolicyPath(path(key)), key);
    assert.equal(matchAdminPolicyPath(path(`${key}/extra`)), null, key);
  }
  for (const key of [...contract.feature_keys, ...Object.keys(contract.policy_bounds), "agent_email_received_per_source_minute", "unknown_limit"]) {
    assert.equal(matchAdminPolicyPath(path(key)), null, key);
  }
});
