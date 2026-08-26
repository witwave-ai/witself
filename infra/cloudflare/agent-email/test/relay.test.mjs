import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  canonicalSignatureInput,
  importSigningKey,
  normalizeEnvelopeAddress,
  RELAY_MAXIMUM_RAW_BYTES,
  sha256Hex,
  signRelay,
  RELAY_SIGNATURE_VERSION,
  RELAY_SIGNATURE_VERSION_V2,
} from "../src/relay.mjs";

const vector = JSON.parse(await readFile(new URL("./golden-vector.json", import.meta.url), "utf8"));
const metadata = {
  timestamp: vector.metadata.timestamp,
  keyId: vector.metadata.key_id,
  envelopeFrom: vector.metadata.envelope_from,
  envelopeTo: vector.metadata.envelope_to,
  audience: vector.metadata.audience,
  rawSize: vector.metadata.raw_size,
  rawSHA256: vector.metadata.raw_sha256,
};

test("Worker canonical bytes and signature match the Go golden vector", async () => {
  const canonical = canonicalSignatureInput(metadata);
  assert.equal(Buffer.from(canonical).toString("base64"), vector.canonical_base64);
  const raw = Buffer.from(vector.raw_base64, "base64");
  assert.equal(await sha256Hex(raw), vector.metadata.raw_sha256);
  const privateKey = await importSigningKey(vector.pkcs8_base64);
  const { signature } = await signRelay(metadata, privateKey);
  assert.equal(Buffer.from(signature).toString("base64"), vector.signature_base64);

  const publicKey = await crypto.subtle.importKey(
    "raw",
    Buffer.from(vector.public_key_base64, "base64"),
    { name: "Ed25519" },
    false,
    ["verify"],
  );
  assert.equal(await crypto.subtle.verify("Ed25519", publicKey, signature, canonical), true);
});

test("envelope normalization matches the Go relay rules", () => {
  assert.equal(normalizeEnvelopeAddress(" <Sender@Example.COM> ", true), "sender@example.com");
  assert.equal(normalizeEnvelopeAddress("<>", true), "");
  assert.throws(() => normalizeEnvelopeAddress("bad\r\n@example.com"));
  assert.throws(() => normalizeEnvelopeAddress("missing-at"));
});

test("every canonical metadata field changes the signed bytes", () => {
  const original = Buffer.from(canonicalSignatureInput(metadata));
  for (const [field, value] of Object.entries({
    timestamp: metadata.timestamp + 1,
    keyId: "pilot-rotated",
    envelopeFrom: "other@example.com",
    envelopeTo: "bravo.abcdefghijkl2345@agent-mail.witwave.ai",
    audience: "cell-other",
    rawSize: metadata.rawSize + 1,
    rawSHA256: "a".repeat(64),
  })) {
    const changed = Buffer.from(canonicalSignatureInput({ ...metadata, [field]: value }));
    assert.notDeepEqual(changed, original, field);
  }
});

test("relay metadata accepts exactly 25 MiB and rejects one byte more", () => {
  assert.equal(RELAY_MAXIMUM_RAW_BYTES, 25 * 1024 * 1024);
  assert.doesNotThrow(() => canonicalSignatureInput({
    ...metadata,
    rawSize: RELAY_MAXIMUM_RAW_BYTES,
  }));
  assert.throws(() => canonicalSignatureInput({
    ...metadata,
    rawSize: RELAY_MAXIMUM_RAW_BYTES + 1,
  }), /invalid relay raw size/);
});


const vectorV2 = JSON.parse(
  await readFile(new URL("./golden-vector-v2.json", import.meta.url), "utf8"),
);

test("v2 canonical bytes, deterministic signature, and verify match the v2 vector", async () => {
  const metadata = {
    version: vectorV2.metadata.version,
    timestamp: vectorV2.metadata.timestamp,
    keyId: vectorV2.metadata.key_id,
    envelopeFrom: vectorV2.metadata.envelope_from,
    envelopeTo: vectorV2.metadata.envelope_to,
    audience: vectorV2.metadata.audience,
    rawSize: vectorV2.metadata.raw_size,
    rawSHA256: vectorV2.metadata.raw_sha256,
    spfResult: vectorV2.metadata.spf_result,
    dkimResult: vectorV2.metadata.dkim_result,
    dmarcResult: vectorV2.metadata.dmarc_result,
  };
  const canonical = canonicalSignatureInput(metadata);
  assert.equal(Buffer.from(canonical).toString("base64"), vectorV2.canonical_base64);
  const privateKey = await importSigningKey(vectorV2.pkcs8_base64);
  const { signature } = await signRelay(metadata, privateKey);
  assert.equal(Buffer.from(signature).toString("base64"), vectorV2.signature_base64);
});

test("every v2 verdict field changes the signed bytes", () => {
  const base = {
    version: RELAY_SIGNATURE_VERSION_V2,
    timestamp: vectorV2.metadata.timestamp,
    keyId: vectorV2.metadata.key_id,
    envelopeFrom: vectorV2.metadata.envelope_from,
    envelopeTo: vectorV2.metadata.envelope_to,
    audience: vectorV2.metadata.audience,
    rawSize: vectorV2.metadata.raw_size,
    rawSHA256: vectorV2.metadata.raw_sha256,
    spfResult: "pass",
    dkimResult: "none",
    dmarcResult: "fail",
  };
  const baseline = Buffer.from(canonicalSignatureInput(base)).toString("base64");
  for (const mutation of [
    { spfResult: "fail" },
    { dkimResult: "pass" },
    { dmarcResult: "none" },
  ]) {
    const mutated = Buffer.from(canonicalSignatureInput({ ...base, ...mutation })).toString("base64");
    assert.notEqual(mutated, baseline);
  }
});

test("version and verdict rules fail closed", () => {
  const base = {
    timestamp: vectorV2.metadata.timestamp,
    keyId: vectorV2.metadata.key_id,
    envelopeFrom: vectorV2.metadata.envelope_from,
    envelopeTo: vectorV2.metadata.envelope_to,
    audience: vectorV2.metadata.audience,
    rawSize: vectorV2.metadata.raw_size,
    rawSHA256: vectorV2.metadata.raw_sha256,
  };
  // A v1 envelope cannot carry verdicts.
  assert.throws(() => canonicalSignatureInput({ ...base, spfResult: "pass" }));
  // Unknown versions and out-of-vocabulary verdicts are rejected.
  assert.throws(() => canonicalSignatureInput({ ...base, version: "witself-email-relay-v3" }));
  assert.throws(() => canonicalSignatureInput({
    ...base,
    version: RELAY_SIGNATURE_VERSION_V2,
    spfResult: "pass",
    dkimResult: "pass",
    dmarcResult: "softfail",
  }));
  // Cross-version relabeling changes the signed bytes.
  const v1Bytes = Buffer.from(canonicalSignatureInput({ ...base, version: RELAY_SIGNATURE_VERSION })).toString("base64");
  const v2Bytes = Buffer.from(canonicalSignatureInput({
    ...base,
    version: RELAY_SIGNATURE_VERSION_V2,
    spfResult: "unknown",
    dkimResult: "unknown",
    dmarcResult: "unknown",
  })).toString("base64");
  assert.notEqual(v1Bytes, v2Bytes);
});
