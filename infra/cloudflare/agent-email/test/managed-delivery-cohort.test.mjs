import assert from "node:assert/strict";
import test from "node:test";

import {
  managedDeliveryAccountIsAdmitted,
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";

test("managed delivery cohort is exact, canonical, bounded, and default-off", () => {
  assert.deepEqual(parseManagedDeliveryAccountAllowlist(""), []);
  assert.deepEqual(
    parseManagedDeliveryAccountAllowlist("acct_alpha,acct_beta"),
    ["acct_alpha", "acct_beta"],
  );
  assert.equal(managedDeliveryAccountIsAdmitted({
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "acct_alpha,acct_beta",
  }, "acct_beta"), true);
  assert.equal(managedDeliveryAccountIsAdmitted({}, "acct_beta"), false);

  for (const value of [
    "*",
    "acct_*",
    " acct_alpha",
    "acct_alpha ",
    "acct_alpha, acct_beta",
    "acct_beta,acct_alpha",
    "acct_alpha,acct_alpha",
    "acct_alpha,,acct_beta",
    "acct.alpha",
    `${Array.from({ length: 101 }, (_, index) =>
      `acct_${String(index).padStart(3, "0")}`).join(",")}`,
  ]) {
    assert.throws(
      () => parseManagedDeliveryAccountAllowlist(value),
      /allowlist is invalid/,
      value,
    );
  }
});
