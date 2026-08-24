import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { extractAuthenticationVerdicts } from "../src/authenticity.mjs";

test("authenticity parser stays byte-identical to agent-email", async () => {
  const [source, copy] = await Promise.all([
    readFile(new URL("../../agent-email/src/authenticity.mjs", import.meta.url)),
    readFile(new URL("../src/authenticity.mjs", import.meta.url)),
  ]);
  assert.deepEqual(copy, source);
});

test("copied parser selects an exact trusted DMARC pass", () => {
  const raw = [
    "Authentication-Results: mx.witwave.example; dmarc=pass header.from=example.com",
    "From: owner@example.com",
    "",
    "hello",
  ].join("\r\n");
  assert.deepEqual(
    extractAuthenticationVerdicts(raw, "mx.witwave.example"),
    { spf: "unknown", dkim: "unknown", dmarc: "pass" },
  );
  assert.equal(
    extractAuthenticationVerdicts(raw, "forged.example").dmarc,
    "none",
  );
});

