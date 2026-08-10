import assert from "node:assert/strict";
import test from "node:test";

import {
  managedDeliveryAccountIsAdmitted,
  managedDeliveryCohortSummary,
  parseManagedDeliveryAccountAllowlist,
} from "../src/agent-email-managed-delivery-cohort.mjs";

test("control-plane cohort parser matches the exact default-off edge contract", async () => {
  assert.deepEqual(parseManagedDeliveryAccountAllowlist(""), []);
  assert.deepEqual(
    parseManagedDeliveryAccountAllowlist("acct_alpha,acct_beta"),
    ["acct_alpha", "acct_beta"],
  );
  assert.equal(managedDeliveryAccountIsAdmitted({
    CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "acct_alpha,acct_beta",
  }, "acct_alpha"), true);
  assert.equal(managedDeliveryAccountIsAdmitted({}, "acct_alpha"), false);
  for (const value of [
    "*", "acct_*", " acct_alpha", "acct_alpha ",
    "acct_alpha, acct_beta", "acct_beta,acct_alpha",
    "acct_alpha,acct_alpha", "acct_alpha,,acct_beta", "acct.alpha",
  ]) {
    assert.throws(
      () => parseManagedDeliveryAccountAllowlist(value),
      /allowlist is invalid/,
      value,
    );
  }

  const summary = await managedDeliveryCohortSummary({
    CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST:
      "acct_alpha,acct_beta",
  });
  assert.equal(summary.account_count, 2);
  assert.equal(summary.empty, false);
  assert.match(summary.allowlist_sha256, /^[0-9a-f]{64}$/);
  assert.doesNotMatch(JSON.stringify(summary), /acct_alpha|acct_beta/);
});
