import assert from "node:assert/strict";
import test from "node:test";
import { createHash } from "node:crypto";
import { legalCoreByteLength, legalCoreCanonical, parseLegalJSON, readCanonicalLegal, readLegalJSON } from "../src/signup-legal.mjs";

const manifest = '{"terms":{"version":"terms-v2","path":"/legal/terms"},"privacy":{"version":"privacy-v2","path":"/legal/privacy"}}';

test("canonical manifest reader accepts exact 64-KiB complete body and hashes the actual bytes", async () => {
  const text = manifest + " ".repeat(65536 - manifest.length);
  let count = 0;
  const actual = await readCanonicalLegal({ LEGAL_DOCUMENTS: { fetch: async (request) => {
    count++; assert.equal(request.url, "https://legal.internal/legal/versions.json");
    assert.equal(request.headers.has("Authorization"), false); assert.equal(request.method, "GET");
    return new Response(text);
  } } });
  assert.deepEqual(actual, { terms: "terms-v2", privacy: "privacy-v2",
    manifest_sha256: createHash("sha256").update(text).digest("hex") });
  assert.equal(count, 1);
});

test("legal JSON rejects duplicate aliases, lone surrogates, trailing values and deep nesting", () => {
  for (const text of [
    '{"a":1,"a":2}', '{"a":1,"\\u0061":2}', '{"a":{"x":1,"x":2}}',
    '{"a":"\\ud800"}', '{"a":1} {}', '[', '['.repeat(18) + '0' + ']'.repeat(18),
  ]) assert.throws(() => parseLegalJSON(text));
  assert.deepEqual(parseLegalJSON('{"a":"escaped \\" quotation","b":[1,true,null,{"x":"\\ud83d\\ude00"}]}'),
    { a: 'escaped " quotation', b: [1, true, null, { x: "😀" }] });
});

test("legal reader rejects oversized and incomplete streams, even after valid JSON", async () => {
  for (const stream of [
    new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode(manifest)); c.error(new Error("private read error")); } }),
    new ReadableStream({ start(c) { c.enqueue(new Uint8Array(65537)); c.close(); } }),
    new ReadableStream({ start(c) { c.enqueue(new Uint8Array([0xff])); c.close(); } }),
  ]) await assert.rejects(readLegalJSON(new Response(stream)));
});

test("cancellation closes a pending read without processing a late authority response", async () => {
  const controller = new AbortController();
  let resolve;
  const result = readCanonicalLegal({ LEGAL_DOCUMENTS: { fetch: () => new Promise((r) => { resolve = r; }) } }, controller.signal);
  controller.abort();
  await assert.rejects(result, /signup legal authority is unavailable/);
  resolve(new Response(manifest));
  const preAborted = new AbortController(); preAborted.abort();
  await assert.rejects(readCanonicalLegal({ LEGAL_DOCUMENTS: { fetch: () => assert.fail("aborted read") } }, preAborted.signal));
});

test("core fingerprint is explicitly separated and excludes only provision ID and consent", () => {
  const base = { email: "person@example.com", display_name: "Person", invite: "early-access",
    provision_id: "original", consent_terms_version: "v1", consent_privacy_version: "v1" };
  const canonical = legalCoreCanonical(base);
  assert.equal(canonical, '["signup-core/v1","person@example.com","Person","early-access"]');
  assert.equal(legalCoreCanonical({ ...base, provision_id: "next", consent_terms_version: "v2", consent_privacy_version: "v3" }), canonical);
  for (const field of ["email", "display_name", "invite"]) assert.notEqual(legalCoreCanonical({ ...base, [field]: "other" }), canonical);
});

test("canonical deadline includes stalled headers and stalled body, without late continuation", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const phase of ["headers", "body"]) {
    let release;
    const promise = readCanonicalLegal({ LEGAL_DOCUMENTS: { fetch: () => {
      if (phase === "headers") return new Promise((r) => { release = r; });
      return Promise.resolve(new Response(new ReadableStream({ start(c) { release = () => { try { c.enqueue(new TextEncoder().encode(manifest)); c.close(); } catch { /* canceled */ } }; } })));
    } } });
    const rejected = assert.rejects(promise, /signup legal authority is unavailable/);
    for (let i = 0; i < 16; i++) await Promise.resolve();
    t.mock.timers.tick(15000);
    await rejected;
    release(phase === "headers" ? new Response(manifest) : undefined);
    for (let i = 0; i < 4; i++) await Promise.resolve();
  }
});


test("invariant core budget matches Go HTML and line-separator escaping", () => {
  const base = { email: "a@example.com", display_name: "Person", invite: "early-access" };
  const fixed = new TextEncoder().encode(JSON.stringify(base)).length;
  for (const escaped of ["<", ">", "&", "\u2028", "\u2029"]) {
    const core = { ...base, email: "a".repeat(32768 - fixed - 6) + escaped + base.email };
    assert.equal(legalCoreByteLength(core), 32768);
    assert.equal(legalCoreByteLength({ ...core, email: "a" + core.email }), 32769);
    assert.equal(legalCoreByteLength({ ...core, provision_id: "x".repeat(128), consent_terms_version: "t".repeat(64), consent_privacy_version: "p".repeat(64) }), 32768);
  }
  for (const field of ["email", "display_name", "invite"]) {
    assert.equal(legalCoreByteLength({ ...base, [field]: "\ud800" }), Infinity);
  }
});
