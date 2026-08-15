import assert from "node:assert/strict";
import test from "node:test";

import {
  buildStorageProbeMIME,
  runStorageProbe,
  storageProbeConfiguration,
} from "../scripts/storage-probe.mjs";

const correlationNonce = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const baseEnvironment = {
  CLOUDFLARE_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
  CLOUDFLARE_API_TOKEN: "sending-token",
  AGENT_EMAIL_STORAGE_PROBE_FROM: "canary@send.witmail.net",
  AGENT_EMAIL_STORAGE_PROBE_TO:
    "canary-a.abcdefghijklmnop@witmail.net",
};

function cloudflareResponse(result) {
  return Response.json({
    success: true,
    errors: [],
    messages: [],
    result,
  });
}

test("storage probe configuration is bounded to a production canonical address", () => {
  const config = storageProbeConfiguration(baseEnvironment);
  assert.equal(config.to, baseEnvironment.AGENT_EMAIL_STORAGE_PROBE_TO);
  assert.equal(Object.hasOwn(config, "expectation"), false);

  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO: "canary@example.com",
    }),
    /canonical @witmail\.net address/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO: "canary.abcdefghijklmnop@witmail.net.evil.test",
    }),
    /canonical @witmail\.net address/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO:
        "canary.abcdefghijklmnop@witmail.net\nBcc: attacker@example.com",
    }),
    /invalid/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
    }),
    /must identify production account/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_FROM: "canary@witwave.ai",
    }),
    /must be canary@send\.witmail\.net/,
  );
  assert.throws(
    () => storageProbeConfiguration({
      ...baseEnvironment,
      AGENT_EMAIL_STORAGE_PROBE_TO: "canary-alias.readable@witmail.net",
    }),
    /canonical @witmail\.net address/,
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

test("storage probe uses send_raw and returns a value-free submission receipt", async () => {
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
    new RegExp(`/accounts/${baseEnvironment.CLOUDFLARE_ACCOUNT_ID}/email/sending/send_raw$`),
  );
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].headers.Authorization, "Bearer sending-token");
  const submission = JSON.parse(calls[0].body);
  assert.equal(submission.from, config.from);
  assert.deepEqual(submission.recipients, [config.to]);
  assert.match(submission.mime_message, /storage-probe\.txt/);
  assert.deepEqual(Object.keys(result).sort(), [
    "addresses_returned",
    "attachment_bytes",
    "mime_returned",
    "outcome",
    "provider_disposition_returned",
    "provider_submission_confirmed",
    "raw_bytes",
    "schema",
    "subject",
    "token_returned",
  ]);
  assert.equal(result.schema, "witself.agent-email.storage-probe.v2");
  assert.equal(result.outcome, "submitted");
  assert.equal(result.subject, `Witself storage probe ${correlationNonce}`);
  assert.ok(result.raw_bytes >= 2 * 1024);
  assert.ok(result.attachment_bytes > 0);
  assert.equal(result.provider_submission_confirmed, true);
  assert.equal(result.provider_disposition_returned, false);

  const output = JSON.stringify(result);
  assert.doesNotMatch(output, /sending-token/);
  assert.doesNotMatch(output, /canary@send\.witmail\.net/);
  assert.doesNotMatch(output, /witmail\.net/);
  assert.doesNotMatch(output, /multipart\/mixed/);
  assert.doesNotMatch(output, /provider-id/);
  assert.doesNotMatch(output, /delivered|queued|permanent_bounces/);
});

test("storage probe does not treat the sending API result as a delivery verdict", async () => {
  const config = storageProbeConfiguration(baseEnvironment);
  const result = await runStorageProbe(config, {
    randomUUID: () => correlationNonce,
    fetch: async () => cloudflareResponse({
      delivered: [],
      queued: [],
      permanent_bounces: [config.to],
      message_id: "<provider-id@cloudflare.test>",
    }),
  });

  assert.equal(result.outcome, "submitted");
  assert.equal(result.provider_submission_confirmed, true);
  assert.equal(result.provider_disposition_returned, false);
  const output = JSON.stringify(result);
  assert.doesNotMatch(output, /provider-id|permanent_bounces/);
  assert.equal(output.includes(config.to), false);
});

test("storage probe still fails closed when Cloudflare rejects the API request", async () => {
  const config = storageProbeConfiguration(baseEnvironment);
  await assert.rejects(
    runStorageProbe(config, {
      randomUUID: () => correlationNonce,
      fetch: async () => Response.json({
        success: false,
        errors: [{ code: 1000 }],
        messages: [],
        result: null,
      }, {
        status: 400,
      }),
    }),
    /Cloudflare API request failed \(1000\)/,
  );
});
