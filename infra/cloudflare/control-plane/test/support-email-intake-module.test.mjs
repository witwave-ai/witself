import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  decideDisposition,
  dedupKey,
  validateIntakePayload,
} from "../src/support-email-intake.mjs";

const MESSAGE_ID = "<support-message-1@example.test>";
const TICKET_TAG = "tkt_abcdefghijklmnop";

function payload(overrides = {}) {
  return {
    sender: "owner@example.test",
    subject: "Need help",
    body: "A bounded plain-text support request.",
    message_id: MESSAGE_ID,
    ...overrides,
  };
}

test("support-email payload validation enforces the exact bounded shape", () => {
  assert.deepEqual(validateIntakePayload(payload()), payload());
  assert.deepEqual(
    validateIntakePayload(payload({ ticket_tag: TICKET_TAG })),
    payload({ ticket_tag: TICKET_TAG }),
  );
  for (const invalid of [
    null,
    [],
    { ...payload(), unexpected: true },
    { ...payload(), sender: "" },
    { ...payload(), message_id: "" },
    { ...payload(), subject: " \t " },
    { ...payload(), body: "\r\n\t " },
    { ...payload(), body: "\u0085" },
    { ...payload(), subject: "x".repeat(201) },
    { ...payload(), body: "é".repeat(32_769) },
    { ...payload(), ticket_tag: "tkt_abcdefghijklmno" },
  ]) {
    assert.throws(
      () => validateIntakePayload(invalid),
      /support email intake payload is invalid/,
    );
  }

  // The subject limit is characters, while the body limit is encoded bytes.
  assert.equal(
    validateIntakePayload(payload({ subject: "😀".repeat(200) })).subject,
    "😀".repeat(200),
  );
  assert.equal(
    validateIntakePayload(payload({ body: "x".repeat(65_536) })).body.length,
    65_536,
  );
});

test("support-email dedup keys hash the Message-ID into a bounded KV key", async () => {
  assert.equal(
    await dedupKey(MESSAGE_ID),
    `intake_dedup:${createHash("sha256").update(MESSAGE_ID).digest("hex")}`,
  );
  await assert.rejects(() => dedupKey(""), /message id is invalid/);
});

test("support-email disposition proceeds only for exactly one active match", () => {
  assert.equal(decideDisposition([]), "drop_unmatched");
  assert.equal(
    decideDisposition([{ account_id: "acct_a", status: "suspended" }]),
    "drop_unmatched",
  );
  assert.equal(
    decideDisposition([{ account_id: "acct_a", status: "active" }]),
    "proceed",
  );
  assert.equal(
    decideDisposition([
      { account_id: "acct_a", status: "active" },
      { account_id: "acct_a", status: "active" },
    ]),
    "drop_ambiguous",
  );
  assert.throws(() => decideDisposition(null), /matches are invalid/);
});
