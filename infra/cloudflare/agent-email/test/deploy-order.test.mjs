import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  canonicalControlPlaneOrigin,
  preflightManagedCohortDeploymentOrder,
  verifyManagedCohortDeploymentOrder,
} from "../scripts/deploy.mjs";

const deploymentID = "11111111-1111-4111-8111-111111111111";
const versionID = "22222222-2222-4222-8222-222222222222";
const firstAccount = "acc_abcdefghijkl2345";
const secondAccount = "acc_bcdefghijklm2345";

function deployment() {
  return {
    id: deploymentID,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function version({ release = "0.0.241", cohort = "" } = {}) {
  return {
    id: versionID,
    resources: {
      bindings: [
        { name: "WITSELF_EDGE_RELEASE_VERSION", type: "plain_text", text: release },
        {
          name: "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
          type: "plain_text",
          text: cohort,
        },
      ],
    },
  };
}

test("edge deployment requires a current control plane and a subset cohort", () => {
  assert.deepEqual(
    verifyManagedCohortDeploymentOrder(
      "0.0.241",
      firstAccount,
      deployment(),
      version({ cohort: `${firstAccount},${secondAccount}` }),
    ),
    {
      required: true,
      control_plane_release: "0.0.241",
      target_account_count: 1,
      active_control_plane_account_count: 2,
    },
  );
  assert.throws(
    () => verifyManagedCohortDeploymentOrder(
      "0.0.241", firstAccount, deployment(), version({ release: "0.0.240" }),
    ),
    /requires a v0\.0\.241 or newer control plane/,
  );
  assert.throws(
    () => verifyManagedCohortDeploymentOrder(
      "0.0.241", secondAccount, deployment(), version({ cohort: firstAccount }),
    ),
    /add to the control plane first/,
  );
});

test("edge deployment preflight inspects the exact active control-plane version", () => {
  const calls = [];
  const result = preflightManagedCohortDeploymentOrder(
    "0.0.241",
    firstAccount,
    (args) => {
      calls.push(args);
      return calls.length === 1
        ? deployment()
        : version({ cohort: firstAccount });
    },
  );
  assert.equal(result.target_account_count, 1);
  assert.deepEqual(calls, [
    ["deployments", "status", "--name", "witself-control-plane", "--json"],
    [
      "versions", "view", versionID,
      "--name", "witself-control-plane", "--json",
    ],
  ]);

  let inspected = false;
  assert.deepEqual(
    preflightManagedCohortDeploymentOrder("0.0.240", "", () => {
      inspected = true;
    }),
    { required: false },
  );
  assert.equal(inspected, false);
});

test("edge deployment pins its operations lease to the production control plane", () => {
  assert.equal(
    canonicalControlPlaneOrigin("https://self.witwave.ai"),
    "https://self.witwave.ai",
  );
  assert.equal(
    canonicalControlPlaneOrigin("https://self.witwave.ai/"),
    "https://self.witwave.ai",
  );
  for (const endpoint of [
    "https://attacker.invalid",
    "https://self.witwave.ai/other",
    "http://self.witwave.ai",
    "https://user@self.witwave.ai",
    " https://self.witwave.ai",
    "",
  ]) {
    assert.throws(
      () => canonicalControlPlaneOrigin(endpoint),
      /canonical control-plane origin/,
    );
  }
});

test("edge deployment holds one private config under the pinned lease", async () => {
  const source = await readFile(
    new URL("../scripts/deploy.mjs", import.meta.url),
    "utf8",
  );
  assert.match(source, /createPrivateDeploymentConfig/);
  assert.match(source, /\{ endpoint: leaseOrigin \}/);
  assert.match(source, /await config\.assertUnchanged\(\)/);
  assert.match(source, /finally \{\s*await config\.cleanup\(\)/);
});
