import assert from "node:assert/strict";
import test from "node:test";

import {
  decideIntake,
  extractTicketTag,
  messageIDFromHeaders,
  SUPPORT_EMAIL_MAX_RAW_BYTES,
  visibleSender,
} from "../src/intake.mjs";

function input(overrides = {}) {
  return {
    headers: new Headers({
      From: "Owner@Example.com",
      "Message-ID": "<message@example.com>",
      Subject: "Help",
    }),
    from: "bounce@example.com",
    to: "support@witwave.ai",
    size: 100,
    verdicts: { spf: "unknown", dkim: "unknown", dmarc: "pass" },
    config: {
      SUPPORT_EMAIL_INTAKE_ENABLED: "true",
      SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "mx.witwave.example",
    },
    ...overrides,
  };
}

test("one authenticated plain visible sender forwards", () => {
  assert.deepEqual(decideIntake(input()), { action: "forward", reason: "forward" });
  assert.equal(visibleSender(input().headers), "owner@example.com");
});

test("recipient and 256 KiB size gates fail safely", () => {
  assert.deepEqual(decideIntake(input({ to: "other@witwave.ai" })), {
    action: "drop", reason: "drop_wrong_recipient",
  });
  assert.deepEqual(decideIntake(input({ size: 0 })), {
    action: "drop", reason: "drop_invalid_size",
  });
  assert.equal(
    decideIntake(input({ size: SUPPORT_EMAIL_MAX_RAW_BYTES })).action,
    "forward",
  );
  assert.deepEqual(
    decideIntake(input({ size: SUPPORT_EMAIL_MAX_RAW_BYTES + 1 })),
    { action: "reject_size", reason: "reject_size" },
  );
});

test("automatic, bulk, list, and null-envelope loops drop", () => {
  for (const [headers, from, reason] of [
    [{ From: "owner@example.com", "Auto-Submitted": "auto-generated" }, "bounce@example.com", "drop_auto_submitted"],
    [{ From: "owner@example.com", Precedence: "bulk" }, "bounce@example.com", "drop_precedence"],
    [{ From: "owner@example.com", Precedence: "auto_reply" }, "bounce@example.com", "drop_precedence"],
    [{ From: "owner@example.com", "List-Id": "" }, "bounce@example.com", "drop_list_id"],
    [{ From: "owner@example.com" }, "<>", "drop_empty_envelope_sender"],
    [{ From: "owner@example.com" }, "", "drop_empty_envelope_sender"],
  ]) {
    assert.deepEqual(decideIntake(input({ headers: new Headers(headers), from })), {
      action: "drop", reason,
    });
  }
  assert.equal(decideIntake(input({
    headers: new Headers({ From: "owner@example.com", "Auto-Submitted": "no" }),
  })).action, "forward");
});

test("visible From must be one plain non-loop address", () => {
  for (const [value, reason] of [
    ["Owner <owner@example.com>", "drop_invalid_from"],
    ["first@example.com, second@example.com", "drop_invalid_from"],
    ["Friends: owner@example.com;", "drop_invalid_from"],
    ["support@witwave.ai", "drop_loop_sender"],
    ["NO-REPLY@witwave.ai", "drop_loop_sender"],
  ]) {
    assert.deepEqual(decideIntake(input({ headers: new Headers({ From: value }) })), {
      action: "drop", reason,
    });
  }
});

test("dark gate, trust config, and DMARC all default to drop", () => {
  assert.equal(decideIntake(input({ config: {
    SUPPORT_EMAIL_INTAKE_ENABLED: "false",
    SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "mx.witwave.example",
  } })).reason, "drop_gate");
  assert.equal(decideIntake(input({ config: {
    SUPPORT_EMAIL_INTAKE_ENABLED: "TRUE",
    SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "mx.witwave.example",
  } })).reason, "drop_gate");
  assert.equal(decideIntake(input({ config: {
    SUPPORT_EMAIL_INTAKE_ENABLED: "true",
    SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "",
  } })).reason, "drop_authserv_id");
  for (const dmarc of ["none", "fail", "unknown", "Pass", undefined]) {
    assert.equal(decideIntake(input({ verdicts: { dmarc } })).reason, "drop_dmarc");
  }
});

test("ticket tags and mandatory bounded Message-ID use exact syntax", () => {
  assert.equal(extractTicketTag("Re: [tkt_abcde123abcde123]"), null);
  assert.equal(
    extractTicketTag("Re: tkt_abcde234abcde234 follow-up"),
    "tkt_abcde234abcde234",
  );
  assert.equal(messageIDFromHeaders(new Headers({ "Message-ID": " <id@example> " })), "<id@example>");
  assert.equal(messageIDFromHeaders(new Headers()), null);
  assert.equal(messageIDFromHeaders({ "Message-ID": "x".repeat(999) }), null);
});
