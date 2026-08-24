import assert from "node:assert/strict";
import test from "node:test";

import {
  extractMimeText,
  MIME_BODY_MAX_BYTES,
  MIME_SUBJECT_MAX_CHARS,
} from "../src/mime-text.mjs";

const encoder = new TextEncoder();

test("non-multipart quoted-printable text and encoded subject are decoded", () => {
  const raw = [
    "From: owner@example.com",
    "Subject: =?UTF-8?Q?caf=C3=A9?=",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: quoted-printable",
    "",
    "first=20line=0D=0Asecond=00\tline",
  ].join("\r\n");
  assert.deepEqual(extractMimeText(raw), {
    subject: "café",
    body: "first line\nsecond\tline",
  });
});

test("base64 text falls back from invalid UTF-8 to Latin-1", () => {
  const encoded = Buffer.from([0x6f, 0x6c, 0xe1]).toString("base64");
  const raw = [
    "Subject: Latin-1",
    "Content-Type: text/plain",
    "Content-Transfer-Encoding: base64",
    "",
    encoded,
  ].join("\r\n");
  assert.deepEqual(extractMimeText(raw), { subject: "Latin-1", body: "olá" });
});

test("multipart traversal returns the first text/plain part", () => {
  const raw = [
    "Subject: Alternative",
    "Content-Type: multipart/alternative; boundary=outer",
    "",
    "preamble",
    "--outer",
    "Content-Type: text/html",
    "",
    "<p>ignored</p>",
    "--outer",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: base64",
    "",
    Buffer.from("plain body", "utf8").toString("base64"),
    "--outer",
    "Content-Type: text/plain",
    "",
    "later body",
    "--outer--",
    "epilogue",
  ].join("\r\n");
  assert.deepEqual(extractMimeText(raw), {
    subject: "Alternative",
    body: "plain body",
  });
});

test("multipart extraction preserves an empty text/plain body", () => {
  const raw = [
    "Subject: Empty",
    "Content-Type: multipart/mixed; boundary=empty",
    "",
    "--empty",
    "Content-Type: text/plain",
    "",
    "--empty--",
  ].join("\r\n");
  assert.deepEqual(extractMimeText(raw), { subject: "Empty", body: "" });
});

test("HTML-only and malformed encoded messages return null", () => {
  assert.equal(extractMimeText([
    "Subject: HTML",
    "Content-Type: text/html",
    "",
    "<p>not accepted</p>",
  ].join("\r\n")), null);
  assert.equal(extractMimeText([
    "Subject: Bad",
    "Content-Type: text/plain",
    "Content-Transfer-Encoding: base64",
    "",
    "not!base64",
  ].join("\r\n")), null);
});

test("body is UTF-8 byte-bounded and subject is code-point-bounded", () => {
  const subject = "🙂".repeat(MIME_SUBJECT_MAX_CHARS + 10);
  const body = "é".repeat(40_000);
  const parsed = extractMimeText([
    `Subject: ${subject}`,
    "Content-Type: text/plain; charset=utf-8",
    "",
    body,
  ].join("\r\n"));
  assert.ok(parsed);
  assert.equal(Array.from(parsed.subject).length, MIME_SUBJECT_MAX_CHARS);
  assert.equal(encoder.encode(parsed.body).byteLength, MIME_BODY_MAX_BYTES);
  assert.equal(parsed.body, "é".repeat(MIME_BODY_MAX_BYTES / 2));
});

test("unencoded text normalizes CRLF and strips C0 except newline and tab", () => {
  const raw = Buffer.concat([
    Buffer.from("Subject: clean\r\n\r\nfirst\r\nsec", "ascii"),
    Buffer.from([0x00, 0x01, 0x09]),
    Buffer.from("ond", "ascii"),
  ]);
  assert.deepEqual(extractMimeText(raw), {
    subject: "clean",
    body: "first\nsec\tond",
  });
});
