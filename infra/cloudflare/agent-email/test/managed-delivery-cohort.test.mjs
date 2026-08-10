import assert from "node:assert/strict";
import test from "node:test";

import {
  managedDeliveryAccountIsAdmitted,
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";
import {
  parseManagedDeliveryAccountAllowlist as parseControlPlaneAllowlist,
} from "../../control-plane/src/agent-email-managed-delivery-cohort.mjs";

const ACCOUNT_A = "acc_aaaaaaaaaaaaaaaa";
const ACCOUNT_B = "acc_bbbbbbbbbbbbbbbb";
const COHORT = `${ACCOUNT_A},${ACCOUNT_B}`;

function boundedCohort(size) {
  const alphabet = "234567abcdefghijklmnopqrstuvwxyz";
  return Array.from({ length: size }, (_, index) =>
    `acc_${"a".repeat(14)}${alphabet[Math.floor(index / 32)]}${alphabet[index % 32]}`,
  ).sort().join(",");
}

test("managed delivery cohort is exact, canonical, bounded, and default-off", () => {
  assert.deepEqual(parseManagedDeliveryAccountAllowlist(""), []);
  assert.deepEqual(
    parseManagedDeliveryAccountAllowlist(COHORT),
    [ACCOUNT_A, ACCOUNT_B],
  );
  assert.equal(managedDeliveryAccountIsAdmitted({
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: COHORT,
  }, ACCOUNT_B), true);
  assert.equal(managedDeliveryAccountIsAdmitted({}, ACCOUNT_B), false);

  assert.equal(parseManagedDeliveryAccountAllowlist(boundedCohort(100)).length, 100);

  for (const value of [
    "*",
    "acc_*",
    ` ${ACCOUNT_A}`,
    `${ACCOUNT_A} `,
    `${ACCOUNT_A}, ${ACCOUNT_B}`,
    `${ACCOUNT_B},${ACCOUNT_A}`,
    `${ACCOUNT_A},${ACCOUNT_A}`,
    `${ACCOUNT_A},,${ACCOUNT_B}`,
    "acct_alpha",
    "acc_aaaaaaaaaaaaaaa1",
    "acc_AAAAAAAAAAAAAAAA",
    boundedCohort(101),
  ]) {
    assert.throws(
      () => parseManagedDeliveryAccountAllowlist(value),
      /allowlist is invalid/,
      value,
    );
  }
});

test("control-plane and edge cohort parsers share the exact byte contract", () => {
  for (const value of ["", ACCOUNT_A, COHORT, boundedCohort(100)]) {
    assert.deepEqual(
      parseManagedDeliveryAccountAllowlist(value),
      parseControlPlaneAllowlist(value),
    );
  }
  for (const value of [
    "acc_aaaaaaaaaaaaaaa1",
    `${ACCOUNT_B},${ACCOUNT_A}`,
    boundedCohort(101),
  ]) {
    assert.throws(() => parseManagedDeliveryAccountAllowlist(value));
    assert.throws(() => parseControlPlaneAllowlist(value));
  }
});
