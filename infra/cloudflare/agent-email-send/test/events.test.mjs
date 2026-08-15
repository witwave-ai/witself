import assert from "node:assert/strict";
import test from "node:test";

import {
  consumeProviderEvents,
  forwardProviderEvent,
  normalizeProviderEvent,
  parseEventTargets,
} from "../src/events.mjs";

const accountId = "acc_aaaaaaaaaaaaaaaa".replaceAll("a", "a");
const token = "t".repeat(32);

function rawEvent(type = "delivered") {
  return {
    type: `cf.email.sending.message.${type}`,
    source: { type: "email.sending", domain: "send.witmail.net" },
    payload: {
      eventId: "0190d0c4-7e9a-7b3c-9f12-1a2b3c4d5e6f",
      messageId: "provider-message-1",
      sender: "private@send.witmail.net",
      recipient: "private@example.com",
      subject: "private subject",
      bounce: type === "bounced" ? { type: "hard", reason: "private reason" } : undefined,
    },
    metadata: {
      eventSchemaVersion: 1,
      eventTimestamp: "2026-08-14T12:34:56.000Z",
    },
  };
}

function targets() {
  return JSON.stringify({
    cells: {
      "founder-cell": {
        url: "https://cell.example.com/v1/internal/agent-email-send:provider-event",
        token,
        accepted_signer_key_ids: ["founder-cell"],
      },
    },
    account_targets: { [accountId]: "founder-cell" },
  });
}

function accountIDAt(index) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let suffix = "";
  let remaining = index;
  do {
    suffix = alphabet[remaining % alphabet.length] + suffix;
    remaining = Math.floor(remaining / alphabet.length);
  } while (remaining > 0);
  return `acc_${suffix.padStart(16, "a")}`;
}

test("normalizes all documented lifecycle events without content", () => {
  for (const type of ["delivered", "deferred", "bounced", "failed", "rejected", "complained"]) {
    const event = normalizeProviderEvent(rawEvent(type));
    assert.equal(event.event_class, type);
    assert.doesNotMatch(JSON.stringify(event), /private/);
    if (type === "bounced") assert.equal(event.bounce_type, "permanent");
  }
  const softBounce = rawEvent("bounced");
  softBounce.payload.bounce.type = "soft";
  const normalizedSoftBounce = normalizeProviderEvent(softBounce);
  assert.equal(normalizedSoftBounce.event_class, "failed");
  assert.equal(normalizedSoftBounce.bounce_type, undefined);
});

test("event targets are exact HTTPS cell routes", () => {
  const parsed = parseEventTargets(targets());
  assert.equal(parsed.accountTargets.get(accountId), "founder-cell");
  assert.equal(
    parsed.cells.get("founder-cell").acceptedSignerKeyIds.has("founder-cell"),
    true,
  );
  for (const url of [
    "http://cell.example.com/v1/internal/agent-email-send:provider-event",
    "https://localhost/v1/internal/agent-email-send:provider-event",
    "https://cell.example.com/other",
  ]) {
    assert.throws(() => parseEventTargets(JSON.stringify({
      cells: {
        "founder-cell": {
          url,
          token,
          accepted_signer_key_ids: ["founder-cell"],
        },
      },
      account_targets: { [accountId]: "founder-cell" },
    })));
  }
});

test("historical events follow an account move with explicit signer provenance", async () => {
  const event = normalizeProviderEvent(rawEvent());
  const route = { account_id: accountId, signer_key_id: "source-key" };
  const movedTargets = JSON.stringify({
    cells: {
      "source-cell": {
        url: "https://source.example.com/v1/internal/agent-email-send:provider-event",
        token: "s".repeat(32),
        accepted_signer_key_ids: ["source-key"],
      },
      "destination-cell": {
        url: "https://destination.example.com/v1/internal/agent-email-send:provider-event",
        token: "d".repeat(32),
        accepted_signer_key_ids: ["destination-key", "source-key"],
      },
    },
    account_targets: { [accountId]: "destination-cell" },
  });
  let forwardedURL;
  const result = await forwardProviderEvent(
    event,
    route,
    { EVENT_TARGETS_JSON: movedTargets },
    async (url) => {
      forwardedURL = url;
      return new Response(null, { status: 204 });
    },
  );
  assert.equal(result, "ack");
  assert.equal(
    forwardedURL,
    "https://destination.example.com/v1/internal/agent-email-send:provider-event",
  );

  const unauthorized = JSON.parse(movedTargets);
  unauthorized.cells["destination-cell"].accepted_signer_key_ids = ["destination-key"];
  await assert.rejects(() => forwardProviderEvent(
    event,
    route,
    { EVENT_TARGETS_JSON: JSON.stringify(unauthorized) },
  ));
});

test("historical signer authorization remains bounded", () => {
  const value = JSON.parse(targets());
  value.cells["founder-cell"].accepted_signer_key_ids = Array.from(
    { length: 32 },
    (_, index) => `key-${index}`,
  );
  assert.equal(
    parseEventTargets(JSON.stringify(value)).cells
      .get("founder-cell").acceptedSignerKeyIds.size,
    32,
  );
  value.cells["founder-cell"].accepted_signer_key_ids.push("key-overflow");
  assert.throws(() => parseEventTargets(JSON.stringify(value)));
});

test("account-to-cell routing remains bounded", () => {
  const value = JSON.parse(targets());
  value.account_targets = {};
  for (let index = 0; index < 100; index += 1) {
    value.account_targets[accountIDAt(index)] = "founder-cell";
  }
  assert.equal(parseEventTargets(JSON.stringify(value)).accountTargets.size, 100);
  value.account_targets[accountIDAt(100)] = "founder-cell";
  assert.throws(() => parseEventTargets(JSON.stringify(value)));

  const oversized = JSON.parse(targets());
  oversized.cells["founder-cell"].token = "t".repeat(5 * 1024);
  assert.throws(() => parseEventTargets(JSON.stringify(oversized)));
});

test("forwards only normalized content and exact account route", async () => {
  const event = normalizeProviderEvent(rawEvent());
  const route = { account_id: accountId, signer_key_id: "founder-cell" };
  let forwarded;
  const result = await forwardProviderEvent(event, route, { EVENT_TARGETS_JSON: targets() }, async (url, init) => {
    forwarded = { url, init };
    return new Response(null, { status: 204 });
  });
  assert.equal(result, "ack");
  assert.equal(forwarded.init.headers.Authorization, `Bearer ${token}`);
  assert.deepEqual(JSON.parse(forwarded.init.body), event);
});

test("acknowledges only durable cell success and retries every refusal", async () => {
  const event = normalizeProviderEvent(rawEvent());
  const route = { account_id: accountId, signer_key_id: "founder-cell" };
  for (const [status, expected] of [
    [204, "ack"],
    [400, "retry"],
    [404, "retry"],
    [409, "retry"],
  ]) {
    const result = await forwardProviderEvent(
      event,
      route,
      { EVENT_TARGETS_JSON: targets() },
      async () => new Response(null, { status }),
    );
    assert.equal(result, expected, `cell HTTP ${status}`);
  }
});

test("queue retries disabled, malformed, transient, and missing-route delivery to DLQ", async () => {
  const outcomes = [];
  const message = (body, attempts = 1) => ({
    body,
    attempts,
    ack() { outcomes.push("ack"); },
    retry() { outcomes.push("retry"); },
  });
  await consumeProviderEvents({ messages: [message(rawEvent())] }, {});
  await consumeProviderEvents({ messages: [message({ invalid: true })] }, {
    EVENT_DELIVERY_ENABLED: "true",
  });
  await consumeProviderEvents({ messages: [message(rawEvent(), 25)] }, {
    EVENT_DELIVERY_ENABLED: "true",
    PROVIDER_ROUTES: {
      idFromName(value) { return value; },
      get() {
        return { fetch: async () => new Response("not found", { status: 404 }) };
      },
    },
  });
  assert.deepEqual(outcomes, ["retry", "retry", "retry"]);
});
