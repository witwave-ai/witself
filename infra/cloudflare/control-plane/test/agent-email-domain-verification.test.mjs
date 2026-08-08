import assert from "node:assert/strict";
import test from "node:test";

import {
  AgentEmailDomainVerificationError,
  agentEmailDomainTXTMatches,
  parseAgentEmailDomainTXT,
  resolveAgentEmailDomainTXT,
} from "../src/agent-email-domain-verification.mjs";

const QUESTION = [{
  name: "_witself-verification.customer.example.",
  type: 16,
}];

test("TXT presentation parsing joins chunks and decodes bounded escapes", () => {
  assert.equal(parseAgentEmailDomainTXT('"witself-domain-" "verification=value"'),
    "witself-domain-verification=value");
  assert.equal(parseAgentEmailDomainTXT('"a\\032b\\\"c\\\\d"'), 'a b"c\\d');
  for (const value of ["plain", '"unterminated', '"bad\\999"', "", 7]) {
    assert.throws(() => parseAgentEmailDomainTXT(value),
      AgentEmailDomainVerificationError);
  }
});

test("fixed DNS JSON lookup accepts only the exact TXT owner and value", async () => {
  const calls = [];
  const result = await resolveAgentEmailDomainTXT(
    "_witself-verification.customer.example",
    async (url, init) => {
      calls.push({ url: new URL(url), init });
      return Response.json({
        Status: 0,
        AD: true,
        Question: QUESTION,
        Answer: [
          {
            name: "_witself-verification.customer.example.",
            type: 16,
            TTL: 300,
            data: '"witself-domain-" "verification=aedv_value"',
          },
          {
            name: "other.customer.example.",
            type: 16,
            TTL: 10,
            data: '"witself-domain-verification=wrong-owner"',
          },
          {
            name: "_witself-verification.customer.example.",
            type: 1,
            TTL: 10,
            data: "192.0.2.1",
          },
        ],
      });
    },
  );
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url.origin, "https://cloudflare-dns.com");
  assert.equal(calls[0].url.pathname, "/dns-query");
  assert.equal(calls[0].url.searchParams.get("name"),
    "_witself-verification.customer.example");
  assert.equal(calls[0].url.searchParams.get("type"), "TXT");
  assert.equal(calls[0].init.redirect, "error");
  assert.deepEqual(result.answers,
    ["witself-domain-verification=aedv_value"]);
  assert.equal(result.minimum_ttl_seconds, 300);
  assert.equal(result.dnssec_authenticated, true);
  assert.match(result.rrset_sha256, /^[0-9a-f]{64}$/);
  assert.equal(agentEmailDomainTXTMatches(
    result,
    "witself-domain-verification=aedv_value",
  ), true);
  assert.equal(agentEmailDomainTXTMatches(result, "wrong"), false);
});

test("authoritative absence differs from a temporary resolver failure", async () => {
  const absent = await resolveAgentEmailDomainTXT(
    "_witself-verification.customer.example",
    async () => Response.json({ Status: 3, AD: false, Question: QUESTION }),
  );
  assert.equal(absent.authoritative_absence, true);
  assert.deepEqual(absent.answers, []);

  await assert.rejects(
    resolveAgentEmailDomainTXT(
      "_witself-verification.customer.example",
      async () => Response.json({ Status: 2, Question: QUESTION }),
    ),
    (error) => error instanceof AgentEmailDomainVerificationError &&
      error.temporary === true && error.code === "dns_lookup_inconclusive",
  );
  await assert.rejects(
    resolveAgentEmailDomainTXT(
      "_witself-verification.customer.example",
      async () => Response.json({
        Status: 0,
        TC: true,
        Question: QUESTION,
        Answer: [],
      }),
    ),
    (error) => error instanceof AgentEmailDomainVerificationError &&
      error.temporary === true && error.code === "dns_lookup_inconclusive",
  );
  for (const payload of [
    { Status: 0 },
    {
      Status: 0,
      Question: [{ name: "_witself-verification.other.example.", type: 16 }],
    },
    { Status: 0, Question: QUESTION, Answer: {} },
  ]) {
    await assert.rejects(
      resolveAgentEmailDomainTXT(
        "_witself-verification.customer.example",
        async () => Response.json(payload),
      ),
      (error) => error instanceof AgentEmailDomainVerificationError &&
        error.temporary === true && error.code === "dns_lookup_inconclusive",
    );
  }
  await assert.rejects(
    resolveAgentEmailDomainTXT(
      "_witself-verification.customer.example",
      async () => {
        throw new Error("network down");
      },
    ),
    (error) => error instanceof AgentEmailDomainVerificationError &&
      error.temporary === true && error.code === "dns_resolver_unavailable",
  );
});

test("an NXDOMAIN response cannot smuggle a matching TXT answer", async () => {
  const expected = "witself-domain-verification=aedv_expected";
  const result = await resolveAgentEmailDomainTXT(
    "_witself-verification.customer.example",
    async () => Response.json({
      Status: 3,
      AD: true,
      Question: QUESTION,
      Answer: [{
        name: "_witself-verification.customer.example.",
        type: 16,
        TTL: 300,
        data: `"${expected}"`,
      }],
    }),
  );
  assert.equal(result.authoritative_absence, true);
  assert.deepEqual(result.answers, []);
  assert.equal(result.minimum_ttl_seconds, null);
  assert.equal(agentEmailDomainTXTMatches(result, expected), false);
  assert.equal(agentEmailDomainTXTMatches({
    authoritative_absence: true,
    answers: [expected],
  }, expected), false);
});
