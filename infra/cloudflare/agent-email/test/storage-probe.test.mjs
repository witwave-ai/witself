import assert from "node:assert/strict";
import test from "node:test";

import {
  buildStorageProbeMIME,
  runStorageProbe,
  storageProbeConfiguration,
} from "../scripts/storage-probe.mjs";

const correlationNonce = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const baseEnvironment = {
  CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
  CLOUDFLARE_API_TOKEN: "sending-token",
  AGENT_EMAIL_STORAGE_PROBE_FROM: "canary@witwave.ai",
  AGENT_EMAIL_STORAGE_PROBE_TO:
    "canary-a.abcdefghijklmnop@agent-mail.witwave.ai",
  AGENT_EMAIL_STORAGE_PROBE_EXPECTATION: "accepted",
};

function cloudflareResponse(result) {
  return Response.json({
    success: true,
    errors: [],
    messages: [],
    result,
  });
}

test("storage probe configuration is bounded to the pilot domain", () => {
  const config = storageProbeConfiguration(baseEnvironment);
  assert.equal(config.to, baseEnvironment.AGENT_EMAIL_STORAGE_PROBE_TO);
  assert.equal(config.expectation, "accepted");

  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO: "canary@example.com",
    }),
    /must use @agent-mail\.witwave\.ai/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO: "canary@agent-mail.witwave.ai.evil.test",
    }),
    /must use @agent-mail\.witwave\.ai/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO:
        "canary@agent-mail.witwave.ai\nBcc: attacker@example.com",
    }),
    /invalid/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_EXPECTATION: "queued",
    }),
    /accepted or permanent_bounce/,
  );
});

test("storage probe builds one small deterministic synthetic attachment", () => {
  const config = storageProbeConfiguration(baseEnvironment);
  const message = buildStorageProbeMIME(config, {
    correlationNonce,
    now: Date.UTC(2026, 6, 30, 12, 0, 0),
  });

  assert.equal(
    message.subject,
    `Witself storage probe ${correlationNonce}`,
  );
  assert.ok(message.rawBytes >= 2 * 1024);
  assert.ok(message.rawBytes <= 16 * 1024);
  assert.ok(message.attachmentBytes > 0);
  assert.match(message.mimeMessage, /multipart\/mixed/);
  assert.match(message.mimeMessage, /storage-probe\.txt/);
  assert.match(message.mimeMessage, /Content-Transfer-Encoding: base64/);
  assert.match(message.mimeMessage, /This message contains no user or production content/);
  assert.doesNotMatch(message.mimeMessage, /sending-token/);
  assert.equal(
    buildStorageProbeMIME(config, {
      correlationNonce,
      now: Date.UTC(2026, 6, 30, 12, 0, 0),
    }).mimeMessage,
    message.mimeMessage,
  );
});

test("accepted storage probe uses send_raw and returns a value-free summary", async () => {
  const config = storageProbeConfiguration(baseEnvironment);
  const calls = [];
  const result = await runStorageProbe(config, {
    randomUUID: () => correlationNonce,
    now: () => Date.UTC(2026, 6, 30, 12, 0, 0),
    fetch: async (url, init) => {
      calls.push({ url, ...init });
      return cloudflareResponse({
        delivered: [config.to],
        queued: [],
        permanent_bounces: [],
        message_id: "<provider-id@cloudflare.test>",
      });
    },
  });

  assert.equal(calls.length, 1);
  assert.match(
    calls[0].url,
    new RegExp(`/accounts/${"a".repeat(32)}/email/sending/send_raw$`),
  );
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].headers.Authorization, "Bearer sending-token");
  const submission = JSON.parse(calls[0].body);
  assert.equal(submission.from, config.from);
  assert.deepEqual(submission.recipients, [config.to]);
  assert.match(submission.mime_message, /storage-probe\.txt/);
  assert.equal(result.outcome, "passed");
  assert.equal(result.delivered, 1);
  assert.equal(result.provider_message_id_returned, true);

  const output = JSON.stringify(result);
  assert.doesNotMatch(output, /sending-token/);
  assert.doesNotMatch(output, /canary@witwave\.ai/);
  assert.doesNotMatch(output, /agent-mail\.witwave\.ai/);
  assert.doesNotMatch(output, /multipart\/mixed/);
});

test("accepted storage probe fails closed on a permanent bounce", async () => {
  const config = storageProbeConfiguration(baseEnvironment);
  await assert.rejects(
    runStorageProbe(config, {
      randomUUID: () => correlationNonce,
      fetch: async () => cloudflareResponse({
        delivered: [],
        queued: [],
        permanent_bounces: [config.to],
      }),
    }),
    /did not confirm an accepted raw email submission/,
  );
});

test("permanent-bounce probe requires exact provider rejection evidence", async () => {
  const config = storageProbeConfiguration({
    ...baseEnvironment,
    AGENT_EMAIL_STORAGE_PROBE_EXPECTATION: "permanent_bounce",
  });
  const passed = await runStorageProbe(config, {
    randomUUID: () => correlationNonce,
    fetch: async () => cloudflareResponse({
      delivered: [],
      queued: [],
      permanent_bounces: [config.to],
      message_id: "<provider-id@cloudflare.test>",
    }),
  });
  assert.equal(passed.outcome, "passed");
  assert.equal(passed.permanent_bounces, 1);

  await assert.rejects(
    runStorageProbe(config, {
      randomUUID: () => correlationNonce,
      fetch: async () => cloudflareResponse({
        delivered: [],
        queued: [config.to],
        permanent_bounces: [],
      }),
    }),
    /did not confirm a permanent raw email rejection/,
  );
});
