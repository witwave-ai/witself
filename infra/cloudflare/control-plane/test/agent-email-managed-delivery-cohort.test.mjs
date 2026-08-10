import assert from "node:assert/strict";
import test from "node:test";

import {
  managedDeliveryAccountIsAdmitted,
  managedDeliveryCohortSummary,
  parseManagedDeliveryAccountAllowlist,
} from "../src/agent-email-managed-delivery-cohort.mjs";

const ACCOUNT_A = "acc_aaaaaaaaaaaaaaaa";
const ACCOUNT_B = "acc_bbbbbbbbbbbbbbbb";
const COHORT = `${ACCOUNT_A},${ACCOUNT_B}`;

test("control-plane cohort parser matches the exact default-off edge contract", async () => {
  assert.deepEqual(parseManagedDeliveryAccountAllowlist(""), []);
  assert.deepEqual(
    parseManagedDeliveryAccountAllowlist(COHORT),
    [ACCOUNT_A, ACCOUNT_B],
  );
  assert.equal(managedDeliveryAccountIsAdmitted({
    CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: COHORT,
  }, ACCOUNT_A), true);
  assert.equal(managedDeliveryAccountIsAdmitted({}, ACCOUNT_A), false);
  for (const value of [
    "*", "acc_*", ` ${ACCOUNT_A}`, `${ACCOUNT_A} `,
    `${ACCOUNT_A}, ${ACCOUNT_B}`, `${ACCOUNT_B},${ACCOUNT_A}`,
    `${ACCOUNT_A},${ACCOUNT_A}`, `${ACCOUNT_A},,${ACCOUNT_B}`,
    "acct_alpha", "acc_aaaaaaaaaaaaaaa1", "acc_AAAAAAAAAAAAAAAA",
  ]) {
    assert.throws(
      () => parseManagedDeliveryAccountAllowlist(value),
      /allowlist is invalid/,
      value,
    );
  }

  const summary = await managedDeliveryCohortSummary({
    CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST:
      COHORT,
  });
  assert.equal(summary.account_count, 2);
  assert.equal(summary.empty, false);
  assert.match(summary.allowlist_sha256, /^[0-9a-f]{64}$/);
  assert.doesNotMatch(JSON.stringify(summary), /acc_aaaaaaaa|acc_bbbbbbbb/);
});
