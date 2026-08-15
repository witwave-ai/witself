#!/usr/bin/env node
import { randomUUID } from "node:crypto";

import { CloudflareAPI } from "./cloudflare.mjs";
import {
  assertProductionCloudflareIdentity,
} from "./wrangler-environment.mjs";
import { parseRouteAddress } from "../src/directory.mjs";

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SIMPLE_LOCAL_PART = /^[a-z0-9][a-z0-9._%+-]*$/;
const SIMPLE_DOMAIN = /^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/;
const PRODUCTION_DOMAIN = "witmail.net";
const PRODUCTION_CANARY_FROM = "canary@send.witmail.net";
const CANONICAL_REALM_LABEL = /^[a-z2-7]{16}$/;
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

function productionRecipient(value) {
  const normalized = emailAddress(value, "AGENT_EMAIL_STORAGE_PROBE_TO");
  let parsed;
  try {
    parsed = parseRouteAddress(normalized, false);
  } catch {
    throw new Error(
      `AGENT_EMAIL_STORAGE_PROBE_TO must be one canonical @${PRODUCTION_DOMAIN} address`,
    );
  }
  if (parsed.domain !== PRODUCTION_DOMAIN ||
      !CANONICAL_REALM_LABEL.test(parsed.realmLabel)) {
    throw new Error(
      `AGENT_EMAIL_STORAGE_PROBE_TO must be one canonical @${PRODUCTION_DOMAIN} address`,
    );
  }
  return normalized;
}

function productionSender(value) {
  const normalized = emailAddress(value, "AGENT_EMAIL_STORAGE_PROBE_FROM");
  if (normalized !== PRODUCTION_CANARY_FROM) {
    throw new Error(
      `AGENT_EMAIL_STORAGE_PROBE_FROM must be ${PRODUCTION_CANARY_FROM}`,
    );
  }
  return normalized;
}

function base64Lines(value) {
  return value.toString("base64").match(/.{1,76}/g).join("\r\n");
}

export function storageProbeConfiguration(env = process.env) {
  const identity = assertProductionCloudflareIdentity(env);
  return {
    accountID: identity.account_id,
    cloudflareToken: env.CLOUDFLARE_API_TOKEN,
    from: productionSender(env.AGENT_EMAIL_STORAGE_PROBE_FROM),
    to: productionRecipient(env.AGENT_EMAIL_STORAGE_PROBE_TO),
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
  // Email Sending acknowledges submission before the Email Routing Worker has
  // necessarily produced its final SMTP-facing verdict. Intentionally ignore
  // the immediate result; the exact subject, live database, and value-free edge
  // metrics are the acceptance evidence for retained, omitted, or rejected mail.
  await api.sendRawEmail({
    from: config.from,
    recipients: [config.to],
    mime_message: message.mimeMessage,
  });

  return {
    schema: "witself.agent-email.storage-probe.v2",
    outcome: "submitted",
    subject: message.subject,
    raw_bytes: message.rawBytes,
    attachment_bytes: message.attachmentBytes,
    provider_submission_confirmed: true,
    provider_disposition_returned: false,
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
