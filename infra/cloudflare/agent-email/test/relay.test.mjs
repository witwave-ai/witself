import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  canonicalSignatureInput,
  importSigningKey,
  normalizeEnvelopeAddress,
  sha256Hex,
  signRelay,
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

test("envelope normalization matches the Go pilot rules", () => {
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
