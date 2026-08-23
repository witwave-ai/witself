import assert from "node:assert/strict";
import test from "node:test";

import { extractAuthenticationVerdicts } from "../src/authenticity.mjs";

const TRUSTED_AUTHSERV_ID = "mx.trusted.example";
const SAFE_DEFAULTS = {
  spf: "unknown",
  dkim: "unknown",
  dmarc: "none",
};

function message(headers, body = "hello") {
  return `${headers.join("\r\n")}\r\n\r\n${body}`;
}

function extract(raw, trustedAuthservID = TRUSTED_AUTHSERV_ID) {
  return extractAuthenticationVerdicts(raw, trustedAuthservID);
}

test("trusted authentication results return pass verdicts in a frozen value-free object", () => {
  const result = extract(message([
    "From: sender@example.com",
    "Authentication-Results: mx.trusted.example; spf=pass smtp.mailfrom=example.com; dkim=pass header.d=example.com; dmarc=pass header.from=example.com",
  ]));

  assert.deepEqual(result, { spf: "pass", dkim: "pass", dmarc: "pass" });
  assert.equal(Object.isFrozen(result), true);
  assert.equal(Object.getPrototypeOf(result), Object.prototype);
  assert.deepEqual(Object.keys(result), ["spf", "dkim", "dmarc"]);
});

test("a trusted attester can return the only enforcing dmarc fail verdict", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=neutral; dkim=policy; dmarc=fail",
  ])), {
    spf: "neutral",
    dkim: "policy",
    dmarc: "fail",
  });
});

test("a forged header before the trusted header cannot override it", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: attacker.example; spf=pass; dkim=pass; dmarc=pass",
    "Authentication-Results: mx.trusted.example; spf=fail; dkim=fail; dmarc=fail",
  ])), {
    spf: "fail",
    dkim: "fail",
    dmarc: "fail",
  });
});

test("a forged header without a trusted attester is never honored", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: attacker.example; spf=pass; dkim=pass; dmarc=fail",
  ])), SAFE_DEFAULTS);
});

test("the trusted authentication results field need not be first", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: first.example; dmarc=fail",
    "Subject: individual fields remain ordered",
    "Authentication-Results: second.example; dmarc=fail",
    "Authentication-Results: mx.trusted.example; spf=softfail; dkim=temperror; dmarc=permerror",
  ])), {
    spf: "softfail",
    dkim: "temperror",
    dmarc: "permerror",
  });
});

test("the first matching trusted field wins without merging later fields", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=pass",
    "Authentication-Results: mx.trusted.example; spf=fail; dkim=fail; dmarc=fail",
  ])), {
    spf: "pass",
    dkim: "pass",
    dmarc: "pass",
  });
});

test("folded authentication results lines are unfolded", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example;",
    "\tspf=pass smtp.mailfrom=example.com;",
    " dkim=pass header.d=example.com;",
    "\tdmarc=pass header.from=example.com",
  ])), {
    spf: "pass",
    dkim: "pass",
    dmarc: "pass",
  });
});

test("header names, authserv ids, methods, and results are case insensitive", () => {
  assert.deepEqual(
    extract(message([
      "aUtHeNtIcAtIoN-rEsUlTs: MX.TRUSTED.EXAMPLE; SPF=PASS; DKIM=NEUTRAL; DMARC=PASS",
    ]), "Mx.TrUsTeD.ExAmPlE"),
    { spf: "pass", dkim: "neutral", dmarc: "pass" },
  );
});

test("bare LF headers and an optional authserv version are accepted", () => {
  const raw = [
    "From: sender@example.com",
    "Authentication-Results: mx.trusted.example 1;",
    " spf=none; dkim=permerror; dmarc=temperror",
    "",
    "body",
  ].join("\n");
  assert.deepEqual(extract(raw), {
    spf: "none",
    dkim: "permerror",
    dmarc: "temperror",
  });
});

test("authentication-results text in the body is ignored", () => {
  assert.deepEqual(extract(message([
    "From: sender@example.com",
    "Subject: body text is untrusted",
  ], "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=fail")), SAFE_DEFAULTS);
});

test("empty, absent, header-only, and unterminated header blocks use safe defaults", () => {
  for (const raw of [
    "",
    "\r\n\r\nbody without headers",
    "From: sender@example.com\r\nSubject: no authentication results\r\n\r\n",
    "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=fail",
  ]) {
    assert.deepEqual(extract(raw), SAFE_DEFAULTS, JSON.stringify(raw));
  }
});

test("unrecognized and cross-method result tokens map to per-method defaults", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=policy; dkim=softfail; dmarc=neutral",
  ])), SAFE_DEFAULTS);
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=garbage; dkim=passish; dmarc=failevil",
  ])), SAFE_DEFAULTS);
});

test("every recognized result is normalized to its lowercase vocabulary", () => {
  for (const value of [
    "unknown", "none", "neutral", "pass", "fail", "softfail",
    "temperror", "permerror",
  ]) {
    assert.equal(extract(message([
      `Authentication-Results: mx.trusted.example; spf=${value.toUpperCase()}`,
    ])).spf, value);
  }
  for (const value of [
    "unknown", "none", "neutral", "pass", "fail", "policy",
    "temperror", "permerror",
  ]) {
    assert.equal(extract(message([
      `Authentication-Results: mx.trusted.example; dkim=${value.toUpperCase()}`,
    ])).dkim, value);
  }
  for (const value of ["unknown", "none", "pass", "fail", "temperror", "permerror"]) {
    assert.equal(extract(message([
      `Authentication-Results: mx.trusted.example; dmarc=${value.toUpperCase()}`,
    ])).dmarc, value);
  }
});

test("Uint8Array input is parsed without retaining message content", () => {
  const raw = new TextEncoder().encode(message([
    "From: private-sender@example.com",
    "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=pass",
  ], "private body"));
  const result = extract(raw);
  assert.deepEqual(result, { spf: "pass", dkim: "pass", dmarc: "pass" });
  assert.equal(JSON.stringify(result).includes("example.com"), false);
  assert.equal(JSON.stringify(result).includes("private"), false);
});

test("authserv ids match exactly and never as prefixes or joined values", () => {
  for (const authservID of [
    "mx.trusted.example.evil",
    "prefix.mx.trusted.example",
    "mx.trusted.example,attacker.example",
  ]) {
    assert.deepEqual(extract(message([
      `Authentication-Results: ${authservID}; dmarc=fail`,
    ])), SAFE_DEFAULTS, authservID);
  }
});

test("a malformed first trusted field does not fall through to a later one", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example not-a-version; dmarc=fail",
    "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=fail",
  ])), SAFE_DEFAULTS);
});

test("malformed methods and supporting syntax cannot coexist with enforcement", () => {
  for (const value of [
    "garbage; dmarc=fail",
    "spf nonsense; dmarc=fail",
    "spf=pass; dmarc=fail garbage",
    "spf=; dmarc=fail",
  ]) {
    assert.deepEqual(extract(message([
      `Authentication-Results: mx.trusted.example; ${value}`,
    ])), SAFE_DEFAULTS, value);
  }
});

test("quoted strings, comments, properties, and folded fake fields cannot inject dmarc", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=pass reason=\"ok; dmarc=fail\"; dkim=pass (dmarc=fail) header.dmarc=fail",
  ])), {
    spf: "pass",
    dkim: "pass",
    dmarc: "none",
  });
  assert.deepEqual(extract(message([
    "X-Trace: harmless",
    " Authentication-Results: mx.trusted.example; dmarc=fail",
  ])), SAFE_DEFAULTS);
});

test("duplicate method results are ambiguous and use safe defaults", () => {
  assert.deepEqual(extract(message([
    "Authentication-Results: mx.trusted.example; spf=pass; dkim=pass; dmarc=fail; dmarc=pass",
  ])), {
    spf: "pass",
    dkim: "pass",
    dmarc: "none",
  });
});

test("authentication-results consideration and header bytes are bounded", () => {
  const untrusted = Array.from(
    { length: 64 },
    (_, index) => `Authentication-Results: attacker-${index}.example; dmarc=fail`,
  );
  assert.equal(extract(message([
    ...untrusted.slice(0, 63),
    "Authentication-Results: mx.trusted.example; dmarc=fail",
  ])).dmarc, "fail");
  assert.deepEqual(extract(message([
    ...untrusted,
    "Authentication-Results: mx.trusted.example; dmarc=fail",
  ])), SAFE_DEFAULTS);

  const oversizedHeader = [
    "Authentication-Results: mx.trusted.example; dmarc=fail",
    `X-Fill: ${"a".repeat(64 * 1024)}`,
    "",
    "body",
  ].join("\r\n");
  assert.deepEqual(extract(oversizedHeader), SAFE_DEFAULTS);
});

test("malformed inputs are total and never enforce", () => {
  for (const [raw, trustedID] of [
    [null, TRUSTED_AUTHSERV_ID],
    [undefined, TRUSTED_AUTHSERV_ID],
    [{}, TRUSTED_AUTHSERV_ID],
    [message(["Authentication-Results: mx.trusted.example; dmarc=fail"]), null],
    ["\tAuthentication-Results: mx.trusted.example; dmarc=fail\r\n\r\n", TRUSTED_AUTHSERV_ID],
    ["Malformed header line\r\n\r\n", TRUSTED_AUTHSERV_ID],
    ["From: sender@example.com\rbare\r\n\r\n", TRUSTED_AUTHSERV_ID],
  ]) {
    assert.doesNotThrow(() => extract(raw, trustedID));
    assert.deepEqual(extract(raw, trustedID), SAFE_DEFAULTS);
  }
});
