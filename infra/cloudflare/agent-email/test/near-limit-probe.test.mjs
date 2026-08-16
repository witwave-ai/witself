import assert from "node:assert/strict";
import test from "node:test";

import {
  buildLargePayloadProbeMIME,
  CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
  LARGE_PAYLOAD_HEADROOM_BYTES,
  LARGE_PAYLOAD_MINIMUM_RECEIVED_BYTES,
  LARGE_PAYLOAD_TARGET_RAW_BYTES,
  largePayloadProbeConfiguration,
  runLargePayloadProbe,
  runLargePayloadProbePreflight,
} from "../scripts/near-limit-probe.mjs";
import { RELAY_MAXIMUM_RAW_BYTES } from "../src/relay.mjs";

const correlationNonce = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const claimKey = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
const completionKey = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const messageID = "emsg_aaaaaaaaaaaaaaaa";
const claimID = "ecl_bbbbbbbbbbbbbbbb";

const baseEnvironment = {
  CLOUDFLARE_ACCOUNT_ID: "8f0bf04a4e7aab3a8cc60f02cc8c8fdb",
  CLOUDFLARE_API_TOKEN: "sending-token",
  AGENT_EMAIL_LARGE_PAYLOAD_FROM: "canary@send.witmail.net",
  AGENT_EMAIL_LARGE_PAYLOAD_TO: "large-payload.abcdefghijklmnop@witmail.net",
  WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT:
    "https://api.civo-sandbox-usw2-dev.cells.witself.witwave.ai",
  WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN: "witself-token",
  AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS: "600",
};

const config = largePayloadProbeConfiguration(baseEnvironment);
const subject = `Witself large-payload receive probe ${correlationNonce}`;
const receivedRawBytes = LARGE_PAYLOAD_TARGET_RAW_BYTES + 4096;

function uuidGenerator() {
  const values = [correlationNonce, claimKey, completionKey];
  return () => values.shift() ?? completionKey;
}

function retainedMessage({
  acknowledged = false,
  completed = false,
  envelopeSender = "bounce+rewritten@notify.cloudflare.com",
  headerFrom = config.from,
} = {}) {
  return {
    id: messageID,
    provider: "cloudflare_email_routing",
    envelope_sender: envelopeSender,
    envelope_recipient: config.to,
    header_from: headerFrom,
    recipient_route_kind: "canonical",
    subaddress_tag: "",
    subject,
    parse_state: "parsed",
    raw_size_bytes: receivedRawBytes,
    attachment_count: 1,
    attachment_storage_bytes: receivedRawBytes,
    retained_attachment_storage_bytes: receivedRawBytes,
    payload_retention_state: "retained",
    read_state: { state: acknowledged ? "acked" : "unread" },
    processing: { state: completed ? "completed" : "available" },
  };
}

function storageStatus({ maximumRawBytes = RELAY_MAXIMUM_RAW_BYTES } = {}) {
  return {
    schema_version: "witself.v0",
    maximum_raw_bytes: maximumRawBytes,
    attachment_capacity: {
      used: 0,
      max: 100 * 1024 * 1024,
      remaining: 100 * 1024 * 1024,
      unlimited: false,
      near_limit: false,
      at_limit: false,
      over_limit: false,
    },
  };
}

test("large-payload probe configuration is fenced to production identities", () => {
  assert.equal(config.accountID, baseEnvironment.CLOUDFLARE_ACCOUNT_ID);
  assert.equal(config.from, "canary@send.witmail.net");
  assert.equal(config.to, baseEnvironment.AGENT_EMAIL_LARGE_PAYLOAD_TO);
  assert.equal(config.timeoutSeconds, 600);
  const compatibilityEnvironment = {
    CLOUDFLARE_ACCOUNT_ID: baseEnvironment.CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_API_TOKEN: baseEnvironment.CLOUDFLARE_API_TOKEN,
    AGENT_EMAIL_NEAR_LIMIT_FROM:
      baseEnvironment.AGENT_EMAIL_LARGE_PAYLOAD_FROM,
    AGENT_EMAIL_NEAR_LIMIT_TO: baseEnvironment.AGENT_EMAIL_LARGE_PAYLOAD_TO,
    WITSELF_EMAIL_NEAR_LIMIT_ENDPOINT:
      baseEnvironment.WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT,
    WITSELF_EMAIL_NEAR_LIMIT_TOKEN:
      baseEnvironment.WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN,
    AGENT_EMAIL_NEAR_LIMIT_TIMEOUT_SECONDS:
      baseEnvironment.AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS,
  };
  assert.deepEqual(
    largePayloadProbeConfiguration(compatibilityEnvironment),
    config,
  );
  assert.equal(largePayloadProbeConfiguration({
    ...baseEnvironment,
    WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT:
      "https://api.c1183b8b-2035-4aae-92b9-15156a8ba1c6.k8s.civo.com",
  }).endpoint, "https://api.c1183b8b-2035-4aae-92b9-15156a8ba1c6.k8s.civo.com");

  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
  }), /must identify production account/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    AGENT_EMAIL_LARGE_PAYLOAD_FROM: "canary@witwave.ai",
  }), /must be canary@send\.witmail\.net/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    AGENT_EMAIL_LARGE_PAYLOAD_TO: "large-payload.readable@witmail.net",
  }), /canonical @witmail\.net/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT: "https://self.witwave.ai",
  }), /root HTTPS URL of one production cell/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT:
      "https://api.civo-sandbox-usw2-dev.cells.witself.witwave.ai/v1",
  }), /root HTTPS URL of one production cell/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    WITSELF_EMAIL_LARGE_PAYLOAD_ENDPOINT:
      "https://api.not-a-cluster.k8s.civo.com",
  }), /root HTTPS URL of one production cell/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    AGENT_EMAIL_LARGE_PAYLOAD_TIMEOUT_SECONDS: "901",
  }), /between 60 and 900/);
  assert.throws(() => largePayloadProbeConfiguration({
    ...baseEnvironment,
    WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN: "bad token",
  }), /WITSELF_EMAIL_LARGE_PAYLOAD_TOKEN is missing or invalid/);
});

test("large-payload MIME is exactly 4 MiB and one MiB below the sending cap", () => {
  const message = buildLargePayloadProbeMIME(config, {
    correlationNonce,
    now: Date.UTC(2026, 7, 15, 12, 0, 0),
  });

  assert.equal(RELAY_MAXIMUM_RAW_BYTES, 25 * 1024 * 1024);
  assert.equal(
    CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
    5 * 1024 * 1024,
  );
  assert.equal(LARGE_PAYLOAD_HEADROOM_BYTES, 1024 * 1024);
  assert.equal(message.rawBytes, LARGE_PAYLOAD_TARGET_RAW_BYTES);
  assert.equal(
    Buffer.byteLength(message.mimeMessage, "utf8"),
    LARGE_PAYLOAD_TARGET_RAW_BYTES,
  );
  assert.equal(message.rawBytes, LARGE_PAYLOAD_MINIMUM_RECEIVED_BYTES);
  assert.ok(
    message.rawBytes < CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
  );
  assert.ok(message.rawBytes < RELAY_MAXIMUM_RAW_BYTES);
  assert.ok(message.attachmentBytes > 3.9 * 1024 * 1024);
  assert.match(message.mimeMessage, /multipart\/mixed/);
  assert.match(message.mimeMessage, /receive-large-payload-v1/);
  assert.match(message.mimeMessage, /large-payload-probe\.bin/);
  assert.match(message.mimeMessage, /No user content/);
  assert.doesNotMatch(message.mimeMessage, /sending-token|witself-token/);
  const attachmentHeader = message.mimeMessage.indexOf(
    'Content-Disposition: attachment; filename="large-payload-probe.bin"',
  );
  const attachmentStart = message.mimeMessage.indexOf("\r\n\r\n", attachmentHeader) + 4;
  const sample = message.mimeMessage.slice(attachmentStart, attachmentStart + 4096);
  assert.match(sample, /^[A-Za-z0-9+/\r\n]+$/);
  assert.ok(new Set(sample.replaceAll("\r", "").replaceAll("\n", "")).size > 48);
  assert.equal(
    buildLargePayloadProbeMIME(config, {
      correlationNonce,
      now: Date.UTC(2026, 7, 15, 12, 0, 0),
    }).mimeMessage.slice(attachmentStart, attachmentStart + 4096),
    sample,
  );
});

test("large-payload preflight is read-only, bounded, and value-free", async () => {
  const calls = [];
  const result = await runLargePayloadProbePreflight(config, {
    fetch: async (url, init) => {
      calls.push({ url, ...init });
      return Response.json(storageStatus());
    },
  });
  assert.deepEqual(calls.map((call) => new URL(call.url).pathname), [
    "/v1/email:status",
  ]);
  assert.equal(calls[0].method, "GET");
  assert.ok(calls[0].signal instanceof AbortSignal);
  assert.equal(result.outcome, "ready");
  assert.equal(result.provider_mutation_performed, false);
  assert.equal(result.payload_allocated, false);
  assert.equal(result.service_inbound_ceiling_bytes, RELAY_MAXIMUM_RAW_BYTES);
  assert.equal(
    result.worker_route_sending_maximum_raw_bytes,
    CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
  );
  assert.equal(result.target_raw_bytes, LARGE_PAYLOAD_TARGET_RAW_BYTES);
  assert.equal(result.live_provider_inbound_ceiling_certified, false);
  const output = JSON.stringify(result);
  assert.equal(output.includes(config.from), false);
  assert.equal(output.includes(config.to), false);
  assert.doesNotMatch(output, /sending-token|witself-token/);
});

test("large-payload probe proves retained storage and non-destructive acknowledgement", async () => {
  const calls = [];
  let submitted;
  let listCalls = 0;
  let tick = Date.UTC(2026, 7, 15, 12, 0, 0);
  const result = await runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    now: () => { tick += 10; return tick; },
    sleep: async () => {},
    fetch: async (url, init) => {
      calls.push({ url, ...init });
      const parsed = new URL(url);
      if (parsed.pathname.endsWith("/v1/email:status")) {
        return Response.json(storageStatus());
      }
      if (parsed.pathname.endsWith("/email/sending/send_raw")) {
        submitted = JSON.parse(init.body);
        return Response.json({
          success: true,
          errors: [],
          messages: [],
          result: {
            delivered: [config.to],
            queued: [],
            permanent_bounces: [],
            message_id: "<provider-id@cloudflare.test>",
          },
        });
      }
      if (parsed.pathname === "/v1/email") {
        listCalls += 1;
        return Response.json({
          messages: [retainedMessage({
            acknowledged: listCalls > 1,
            completed: listCalls > 1,
          })],
          next_cursor: "",
        });
      }
      if (parsed.pathname.endsWith(`/${messageID}:claim`)) {
        return Response.json({ processing: {
          state: "claimed",
          claim_id: claimID,
          generation: 7,
        } });
      }
      if (parsed.pathname.endsWith(`/${messageID}:complete`)) {
        return Response.json({ processing: {
          state: "completed",
          claim_id: claimID,
          generation: 7,
        } });
      }
      if (parsed.pathname.endsWith(`/${messageID}:ack`)) {
        return Response.json({ message: retainedMessage({
          acknowledged: true,
          completed: true,
        }) });
      }
      throw new Error(`unexpected test path ${parsed.pathname}`);
    },
  });

  assert.equal(submitted.from, config.from);
  assert.deepEqual(submitted.recipients, [config.to]);
  assert.ok(
    Buffer.byteLength(calls[1].body, "utf8") <
      CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
  );
  assert.equal(
    Buffer.byteLength(submitted.mime_message, "utf8"),
    LARGE_PAYLOAD_TARGET_RAW_BYTES,
  );
  assert.match(submitted.mime_message, new RegExp(correlationNonce));
  assert.equal(result.outcome, "passed");
  assert.equal(result.submitted_raw_bytes, LARGE_PAYLOAD_TARGET_RAW_BYTES);
  assert.equal(result.received_raw_bytes, receivedRawBytes);
  assert.equal(result.service_inbound_ceiling_bytes, RELAY_MAXIMUM_RAW_BYTES);
  assert.equal(
    result.worker_route_sending_maximum_raw_bytes,
    CLOUDFLARE_WORKER_ROUTE_SENDING_MAXIMUM_RAW_BYTES,
  );
  assert.equal(result.live_provider_inbound_ceiling_certified, false);
  assert.equal(result.local_inbound_ceiling_tests_required, true);
  assert.equal(result.durable_storage_verified, true);
  assert.equal(result.mailbox_processing_completed, true);
  assert.equal(result.mailbox_acknowledged, true);
  assert.equal(result.durable_after_acknowledgement, true);
  assert.equal(result.time_based_retention_deletion_tested, false);
  assert.equal(result.payload_returned, false);
  assert.equal(result.addresses_returned, false);
  assert.equal(result.identifiers_returned, false);
  assert.equal(result.tokens_returned, false);

  const paths = calls.map((call) => new URL(call.url).pathname);
  assert.deepEqual(paths, [
    "/v1/email:status",
    `/client/v4/accounts/${config.accountID}/email/sending/send_raw`,
    "/v1/email",
    `/v1/email/${messageID}:claim`,
    `/v1/email/${messageID}:complete`,
    `/v1/email/${messageID}:ack`,
    "/v1/email",
  ]);
  assert.equal(calls.every((call) => call.signal instanceof AbortSignal), true);
  const firstList = new URL(calls[2].url);
  const finalList = new URL(calls.at(-1).url);
  assert.equal(firstList.searchParams.get("unacked"), "true");
  assert.equal(finalList.searchParams.has("unacked"), false);
  assert.equal(calls[3].headers["Idempotency-Key"], claimKey);
  assert.equal(calls[4].headers["Idempotency-Key"], completionKey);

  const output = JSON.stringify(result);
  assert.doesNotMatch(output, /sending-token|witself-token|provider-id/);
  assert.doesNotMatch(output, new RegExp(messageID));
  assert.doesNotMatch(output, new RegExp(claimID));
  assert.doesNotMatch(output, new RegExp(correlationNonce));
  assert.equal(output.includes(config.from), false);
  assert.equal(output.includes(config.to), false);
  assert.doesNotMatch(output, /multipart\/mixed|large-payload-probe\.bin/);
});

test("large-payload probe refuses a lower plan limit before provider submission", async () => {
  const paths = [];
  await assert.rejects(() => runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    fetch: async (url) => {
      paths.push(new URL(url).pathname);
      return Response.json(storageStatus({ maximumRawBytes: 10 * 1024 * 1024 }));
    },
  }), /reviewed 25 MiB service ceiling/);
  assert.deepEqual(paths, ["/v1/email:status"]);
});

test("large-payload probe rejects a message whose Header From does not match", async () => {
  await assert.rejects(() => runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    sleep: async () => {},
    fetch: async (url) => {
      const path = new URL(url).pathname;
      if (path.endsWith("/v1/email:status")) {
        return Response.json(storageStatus());
      }
      if (path.endsWith("/email/sending/send_raw")) {
        return Response.json({
          success: true,
          errors: [],
          result: {
            delivered: [config.to],
            queued: [],
            permanent_bounces: [],
          },
        });
      }
      if (path === "/v1/email") {
        return Response.json({
          messages: [retainedMessage({ headerFrom: "other@example.com" })],
          next_cursor: "",
        });
      }
      throw new Error(`unexpected test path ${path}`);
    },
  }), /did not produce one canonical durably retained message/);
});

test("Cloudflare submission failures expose only bounded numeric codes", async () => {
  const providerErrors = Array.from({ length: 10 }, (_, index) => ({
    code: 10_000 + index,
    message: `private provider detail ${index}`,
  }));
  await assert.rejects(() => runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    fetch: async (url) => {
      const path = new URL(url).pathname;
      if (path.endsWith("/v1/email:status")) {
        return Response.json(storageStatus());
      }
      if (path.endsWith("/email/sending/send_raw")) {
        return Response.json({
          success: false,
          errors: providerErrors,
          result: null,
        }, { status: 413 });
      }
      throw new Error(`unexpected test path ${path}`);
    },
  }), (error) => {
    assert.match(error.message, /failed with status 413/);
    assert.match(
      error.message,
      /Cloudflare error codes: 10000,10001,10002,10003,10004,10005,10006,10007/,
    );
    assert.doesNotMatch(error.message, /10008|10009|private provider detail/);
    return true;
  });
});

test("large-payload probe keeps service-ceiling capacity headroom before sending", async () => {
  const paths = [];
  await assert.rejects(() => runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    fetch: async (url) => {
      paths.push(new URL(url).pathname);
      const value = storageStatus();
      value.attachment_capacity.remaining = RELAY_MAXIMUM_RAW_BYTES - 1;
      return Response.json(value);
    },
  }), /enough attachment capacity/);
  assert.deepEqual(paths, ["/v1/email:status"]);
});

test("large-payload probe releases the exact claim after a completion failure", async () => {
  const paths = [];
  let releaseBody;
  await assert.rejects(() => runLargePayloadProbe(config, {
    randomUUID: uuidGenerator(),
    sleep: async () => {},
    fetch: async (url, init) => {
      const parsed = new URL(url);
      paths.push(parsed.pathname);
      if (parsed.pathname.endsWith("/v1/email:status")) {
        return Response.json(storageStatus());
      }
      if (parsed.pathname.endsWith("/email/sending/send_raw")) {
        return Response.json({
          success: true,
          result: {
            delivered: [],
            queued: [config.to],
            permanent_bounces: [],
          },
        });
      }
      if (parsed.pathname === "/v1/email") {
        return Response.json({ messages: [retainedMessage()], next_cursor: "" });
      }
      if (parsed.pathname.endsWith(`/${messageID}:claim`)) {
        return Response.json({ processing: {
          state: "claimed",
          claim_id: claimID,
          generation: 7,
        } });
      }
      if (parsed.pathname.endsWith(`/${messageID}:complete`)) {
        return Response.json({ error: "unavailable" }, { status: 503 });
      }
      if (parsed.pathname.endsWith(`/${messageID}:release`)) {
        releaseBody = JSON.parse(init.body);
        return Response.json({ processing: { state: "available", generation: 8 } });
      }
      throw new Error(`unexpected test path ${parsed.pathname}`);
    },
  }), /failed with status 503/);

  assert.deepEqual(releaseBody, { claim_id: claimID, generation: 7 });
  assert.deepEqual(paths.slice(-3), [
    `/v1/email/${messageID}:claim`,
    `/v1/email/${messageID}:complete`,
    `/v1/email/${messageID}:release`,
  ]);
});
