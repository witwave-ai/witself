import assert from "node:assert/strict";
import { webcrypto } from "node:crypto";
import test from "node:test";

import { handleEmail } from "../src/index.js";
import { SUPPORT_EMAIL_MAX_RAW_BYTES } from "../src/intake.mjs";

const trustedAuthservID = "mx.witwave.example";
const intakeToken = "support-intake-token-1234567890";
const allowLimiter = { async limit() { return { success: true }; } };

function rawEmail({
  dmarc = "pass",
  from = "Owner@Example.com",
  messageID = "<support-1@example.com>",
  subject = "Need help",
  body = "plain support body",
  contentType = "text/plain; charset=utf-8",
  extraHeaders = {},
} = {}) {
  const lines = [];
  if (dmarc !== null) {
    lines.push(`Authentication-Results: ${trustedAuthservID}; dmarc=${dmarc}`);
  }
  lines.push(`From: ${from}`, "To: support@witwave.ai");
  if (messageID !== null) lines.push(`Message-ID: ${messageID}`);
  lines.push(`Subject: ${subject}`, `Content-Type: ${contentType}`);
  for (const [name, value] of Object.entries(extraHeaders)) {
    lines.push(`${name}: ${value}`);
  }
  lines.push("", body);
  return new TextEncoder().encode(lines.join("\r\n"));
}

function fakeMessage(options = {}) {
  const raw = options.raw ?? rawEmail(options);
  const headers = new Headers({
    From: options.from ?? "Owner@Example.com",
    Subject: options.subject ?? "Need help",
    ...(options.messageID === null ? {} : {
      "Message-ID": options.messageID ?? "<support-1@example.com>",
    }),
    ...(options.extraHeaders ?? {}),
  });
  const rejected = [];
  return {
    from: options.envelopeFrom ?? "bounce@example.com",
    to: options.to ?? "support@witwave.ai",
    rawSize: options.rawSize ?? raw.byteLength,
    raw: new ReadableStream({
      start(controller) {
        controller.enqueue(raw);
        controller.close();
      },
    }),
    headers,
    rejected,
    setReject(reason) { rejected.push(reason); },
  };
}

function environment(overrides = {}) {
  return {
    SUPPORT_EMAIL_INTAKE_ENABLED: "true",
    SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
    CONTROL_PLANE_URL: "https://self.witwave.ai/",
    CONTROL_PLANE_SUPPORT_INTAKE_TOKEN: intakeToken,
    SUPPORT_EMAIL_SENDER_LIMITER: allowLimiter,
    SUPPORT_EMAIL_GLOBAL_LIMITER: allowLimiter,
    ...overrides,
  };
}

function runtime(fetch = async () => new Response(null, { status: 200 })) {
  return { crypto: webcrypto, fetch };
}

test("gate-off accepts and drops without control-plane egress", async () => {
  let called = false;
  const result = await handleEmail(
    fakeMessage(),
    environment({ SUPPORT_EMAIL_INTAKE_ENABLED: "false" }),
    runtime(async () => { called = true; return new Response(); }),
  );
  assert.deepEqual(result, { action: "drop", reason: "drop_gate" });
  assert.equal(called, false);
});

test("DMARC none and fail both drop without egress", async () => {
  for (const dmarc of [null, "none", "fail"]) {
    let called = false;
    const result = await handleEmail(fakeMessage({ dmarc }), environment(),
      runtime(async () => { called = true; return new Response(); }));
    assert.deepEqual(result, { action: "drop", reason: "drop_dmarc" });
    assert.equal(called, false);
  }
});

test("trusted DMARC pass forwards the exact bounded value payload", async () => {
  let request;
  const mail = fakeMessage({
    from: "Owner@Example.com",
    envelopeFrom: "different-envelope@example.net",
    subject: "Need help",
    body: "plain support body",
  });
  const result = await handleEmail(mail, environment(), runtime(async (value) => {
    request = value;
    return new Response(null, { status: 200 });
  }));
  assert.deepEqual(result, { action: "forward", reason: "forward" });
  assert.equal(request.url, "https://self.witwave.ai/v1/intake/support-email");
  assert.equal(request.method, "POST");
  assert.equal(request.redirect, "manual");
  assert.equal(request.headers.get("Authorization"), `Bearer ${intakeToken}`);
  assert.deepEqual(await request.json(), {
    sender: "owner@example.com",
    subject: "Need help",
    body: "plain support body",
    message_id: "<support-1@example.com>",
  });
  assert.deepEqual(mail.rejected, []);
  assert.doesNotMatch(JSON.stringify(result), /owner|plain support body|Need help/i);
});

test("ticket tag is extracted from the subject and forwarded", async () => {
  let payload;
  await handleEmail(fakeMessage({
    subject: "Re: tkt_abcde234abcde234 more detail",
  }), environment(), runtime(async (request) => {
    payload = await request.json();
    return new Response(null, { status: 200 });
  }));
  assert.equal(payload.ticket_tag, "tkt_abcde234abcde234");
});

test("over-size input rejects without reading or forwarding", async () => {
  const mail = fakeMessage({ rawSize: SUPPORT_EMAIL_MAX_RAW_BYTES + 1 });
  let called = false;
  const result = await handleEmail(mail, environment(), runtime(async () => {
    called = true;
    return new Response();
  }));
  assert.deepEqual(result, { action: "reject_size", reason: "reject_size" });
  assert.deepEqual(mail.rejected, ["message too large"]);
  assert.equal(called, false);
});

test("loop-shaped messages drop without forwarding", async () => {
  for (const options of [
    { extraHeaders: { "Auto-Submitted": "auto-generated" } },
    { extraHeaders: { Precedence: "bulk" } },
    { extraHeaders: { "List-Id": "list.example" } },
    { envelopeFrom: "" },
    { from: "support@witwave.ai" },
  ]) {
    let called = false;
    const result = await handleEmail(fakeMessage(options), environment(),
      runtime(async () => { called = true; return new Response(); }));
    assert.equal(result.action, "drop");
    assert.equal(called, false);
  }
});

test("missing Message-ID and HTML-only mail fail safely", async () => {
  const missingID = await handleEmail(
    fakeMessage({ messageID: null }), environment(), runtime(),
  );
  assert.deepEqual(missingID, { action: "drop", reason: "drop_message_id" });
  const html = await handleEmail(fakeMessage({
    contentType: "text/html",
    body: "<p>html only</p>",
  }), environment(), runtime());
  assert.deepEqual(html, { action: "drop", reason: "drop_html_only" });
});

test("blank subject or plain-text body drops before control-plane egress", async () => {
  for (const options of [
    { subject: "   " },
    { body: " \t " },
    { body: "\u0085" },
  ]) {
    let called = false;
    const result = await handleEmail(
      fakeMessage(options),
      environment(),
      runtime(async () => {
        called = true;
        return new Response();
      }),
    );
    assert.deepEqual(result, {
      action: "drop",
      reason: "drop_invalid_content",
    });
    assert.equal(called, false);
  }
});

test("per-sender denial drops while global denial requests provider retry", async () => {
  let globalCalls = 0;
  const senderDrop = await handleEmail(fakeMessage(), environment({
    SUPPORT_EMAIL_SENDER_LIMITER: {
      async limit({ key }) {
        assert.match(key, /^[0-9a-f]{64}$/);
        assert.notEqual(key, "owner@example.com");
        return { success: false };
      },
    },
    SUPPORT_EMAIL_GLOBAL_LIMITER: {
      async limit() { globalCalls += 1; return { success: true }; },
    },
  }), runtime());
  assert.deepEqual(senderDrop, { action: "drop", reason: "drop_sender_rate" });
  assert.equal(globalCalls, 0);

  await assert.rejects(() => handleEmail(fakeMessage(), environment({
    SUPPORT_EMAIL_GLOBAL_LIMITER: {
      async limit({ key }) {
        assert.equal(key, "support-email-intake-v1");
        return { success: false };
      },
    },
  }), runtime()), { message: "support email intake temporarily unavailable" });
});

test("control-plane failures throw for replay and never reject the sender", async () => {
  const mail = fakeMessage();
  await assert.rejects(
    () => handleEmail(mail, environment(), runtime(async () =>
      new Response(null, { status: 503 }))),
    { message: "support email intake temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
});
