#!/usr/bin/env node
import { randomUUID } from "node:crypto";

import { CloudflareAPI } from "./cloudflare.mjs";

const ACCOUNT_ID = /^[0-9a-f]{32}$/;
const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SIMPLE_LOCAL_PART = /^[a-z0-9][a-z0-9._%+-]*$/;
const SIMPLE_DOMAIN = /^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/;
const PILOT_DOMAIN = "agent-mail.witwave.ai";
const EXPECTATIONS = new Set(["accepted", "permanent_bounce"]);
const MAXIMUM_PROBE_BYTES = 16 * 1024;
const MINIMUM_PROBE_BYTES = 2 * 1024;
const SYNTHETIC_ATTACHMENT = Buffer.from(
  "witself-agent-email-storage-probe\n".repeat(16),
  "utf8",
);
const SYNTHETIC_BODY = [
  "Synthetic Witself agent-email storage probe.",
  "This message contains no user or production content.",
  "x".repeat(2048),
].join("\r\n");

function required(value, name) {
  const normalized = String(value ?? "").trim();
  if (!normalized || /[\r\n\0]/.test(normalized)) {
    throw new Error(`${name} is missing or invalid`);
  }
  return normalized;
}

function emailAddress(value, name) {
  const normalized = required(value, name).toLowerCase();
  if (normalized.length > 320 || normalized.split("@").length !== 2) {
    throw new Error(`${name} is missing or invalid`);
  }
  const [localPart, domain] = normalized.split("@");
  if (
    localPart.length < 1 ||
    localPart.length > 64 ||
    !SIMPLE_LOCAL_PART.test(localPart) ||
    !SIMPLE_DOMAIN.test(domain) ||
    domain.includes("..")
  ) {
    throw new Error(`${name} is missing or invalid`);
  }
  return normalized;
}

function pilotRecipient(value) {
  const normalized = emailAddress(value, "AGENT_EMAIL_STORAGE_PROBE_TO");
  if (!normalized.endsWith(`@${PILOT_DOMAIN}`)) {
    throw new Error(`AGENT_EMAIL_STORAGE_PROBE_TO must use @${PILOT_DOMAIN}`);
  }
  return normalized;
}

function base64Lines(value) {
  return value.toString("base64").match(/.{1,76}/g).join("\r\n");
}

function providerArray(result, field) {
  const value = result?.[field];
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) {
    throw new Error("Cloudflare returned an invalid raw email submission result");
  }
  return value;
}

function hasMessageID(result) {
  const value = result?.message_id;
  return (
    typeof value === "string" &&
    value.trim() !== "" &&
    !/[\r\n\0]/.test(value)
  );
}

export function storageProbeConfiguration(env = process.env) {
  const accountID = required(
    env.CLOUDFLARE_ACCOUNT_ID,
    "CLOUDFLARE_ACCOUNT_ID",
  );
  if (!ACCOUNT_ID.test(accountID)) {
    throw new Error("CLOUDFLARE_ACCOUNT_ID is missing or invalid");
  }
  const expectation = required(
    env.AGENT_EMAIL_STORAGE_PROBE_EXPECTATION,
    "AGENT_EMAIL_STORAGE_PROBE_EXPECTATION",
  );
  if (!EXPECTATIONS.has(expectation)) {
    throw new Error(
      "AGENT_EMAIL_STORAGE_PROBE_EXPECTATION must be accepted or permanent_bounce",
    );
  }
  return {
    accountID,
    cloudflareToken: required(
      env.CLOUDFLARE_API_TOKEN,
      "CLOUDFLARE_API_TOKEN",
    ),
    from: emailAddress(
      env.AGENT_EMAIL_STORAGE_PROBE_FROM,
      "AGENT_EMAIL_STORAGE_PROBE_FROM",
    ),
    to: pilotRecipient(env.AGENT_EMAIL_STORAGE_PROBE_TO),
    expectation,
  };
}

export function buildStorageProbeMIME(
  config,
  { correlationNonce, now = Date.now() },
) {
  if (!UUID_V4.test(String(correlationNonce ?? ""))) {
    throw new Error("storage probe correlation nonce is invalid");
  }
  const instant = new Date(now);
  if (Number.isNaN(instant.getTime())) {
    throw new Error("storage probe clock is invalid");
  }
  const boundary = `witself-storage-probe-${correlationNonce}`;
  const subject = `Witself storage probe ${correlationNonce}`;
  const lines = [
    `From: ${config.from}`,
    `To: ${config.to}`,
    `Date: ${instant.toUTCString()}`,
    `Message-ID: <storage-probe-${correlationNonce}@witwave.ai>`,
    `Subject: ${subject}`,
    "MIME-Version: 1.0",
    "X-Witself-Canary: storage-probe-v1",
    `Content-Type: multipart/mixed; boundary="${boundary}"`,
    "",
    `--${boundary}`,
    'Content-Type: text/plain; charset="utf-8"',
    "Content-Transfer-Encoding: 7bit",
    "",
    SYNTHETIC_BODY,
    `--${boundary}`,
    'Content-Type: application/octet-stream; name="storage-probe.txt"',
    "Content-Transfer-Encoding: base64",
    'Content-Disposition: attachment; filename="storage-probe.txt"',
    "",
    base64Lines(SYNTHETIC_ATTACHMENT),
    `--${boundary}--`,
    "",
  ];
  const mimeMessage = lines.join("\r\n");
  const rawBytes = Buffer.byteLength(mimeMessage, "utf8");
  if (
    rawBytes < MINIMUM_PROBE_BYTES ||
    rawBytes > MAXIMUM_PROBE_BYTES
  ) {
    throw new Error("storage probe MIME size is outside its safety bound");
  }
  return {
    mimeMessage,
    subject,
    rawBytes,
    attachmentBytes: SYNTHETIC_ATTACHMENT.byteLength,
  };
}

export async function runStorageProbe(config, runtime = {}) {
  const uuid = runtime.randomUUID ?? randomUUID;
  const correlationNonce = uuid();
  const message = buildStorageProbeMIME(config, {
    correlationNonce,
    now: runtime.now?.() ?? Date.now(),
  });
  const api = new CloudflareAPI({
    accountID: config.accountID,
    apiToken: config.cloudflareToken,
    fetchAPI: runtime.fetch ?? fetch,
  });
  const result = await api.sendRawEmail({
    from: config.from,
    recipients: [config.to],
    mime_message: message.mimeMessage,
  });
  const delivered = providerArray(result, "delivered").length;
  const queued = providerArray(result, "queued").length;
  const permanentBounces = providerArray(
    result,
    "permanent_bounces",
  ).length;
  const accepted =
    delivered > 0 || queued > 0 || hasMessageID(result);

  if (
    config.expectation === "accepted" &&
    (!accepted || permanentBounces !== 0)
  ) {
    throw new Error(
      "Cloudflare did not confirm an accepted raw email submission",
    );
  }
  if (
    config.expectation === "permanent_bounce" &&
    (permanentBounces < 1 || delivered > 0 || queued > 0)
  ) {
    throw new Error(
      "Cloudflare did not confirm a permanent raw email rejection",
    );
  }

  return {
    schema: "witself.agent-email.storage-probe.v1",
    outcome: "passed",
    expectation: config.expectation,
    subject: message.subject,
    raw_bytes: message.rawBytes,
    attachment_bytes: message.attachmentBytes,
    delivered,
    queued,
    permanent_bounces: permanentBounces,
    provider_message_id_returned: hasMessageID(result),
    token_returned: false,
    mime_returned: false,
    addresses_returned: false,
  };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  runStorageProbe(storageProbeConfiguration())
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`agent-email storage probe: ${error.message}\n`);
      process.exitCode = 1;
    });
}
