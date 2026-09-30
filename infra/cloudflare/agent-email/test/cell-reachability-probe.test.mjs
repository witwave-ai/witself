import assert from "node:assert/strict";
import { generateKeyPairSync, webcrypto } from "node:crypto";
import { readFile } from "node:fs/promises";
import { Readable } from "node:stream";
import test from "node:test";

import { handleEmail } from "../src/index.js";
import { parseRouteAddress, realmRouteKey } from "../src/directory.mjs";
import {
  canonicalSignatureInput,
  importSigningKey,
  RELAY_SIGNATURE_VERSION,
  RELAY_SIGNATURE_VERSION_V2,
  sha256Hex,
} from "../src/relay.mjs";
import {
  buildSignedRelay,
  CONTROL_AUDIENCE,
  INGEST_PATH,
  LARGE_BODY_BYTES,
  main,
  PRIVATE_KEY_INPUT_MAXIMUM_BYTES,
  PROBE_AGENT_SEGMENT,
  PROBE_DOMAIN,
  PROBE_ENVELOPE_FROM,
  PROVIDER_EVENT_PATH,
  REACHABILITY_CONTRACT_CASES,
  REACHABILITY_CONTRACT_SCHEMA,
  REACHABILITY_SCHEMA,
  syntheticLabel,
  syntheticMessage,
} from "../scripts/cell-reachability-probe.mjs";
import {
  ROUTE_PUBLIC_KEY_ENV,
  signTestRouteProjection,
} from "./route-signature-fixture.mjs";

const vector = JSON.parse(await readFile(new URL("./golden-vector-v2.json", import.meta.url), "utf8"));
const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));
const contract = JSON.parse(await readFile(new URL("./cell-reachability-contract.json", import.meta.url), "utf8"));
const FIXED_NOW = Date.UTC(2026, 8, 29, 18, 0, 0);
const HOST = "api.civo-prod-use1-serving.cells.witself.witwave.ai";
const ORIGIN = `https://${HOST}`;
const CLUSTER_ORIGIN = "https://api.00000000-0000-4000-8000-000000000000.k8s.civo.com";
const AUDIENCE = "civo-prod-use1-serving";
const KEY_ID = "relay-2026-08";
const RECIPIENT = "reachability-probe.abcdefghijklmnop@witmail.net";
const NONCE = "qrstuvwxyz234567";
const OUTPUT_KEYS = [
  "schema_version", "mode", "host", "cell", "audience", "relay_version",
  "large_body", "cell_version", "store_schema_version", "clock_skew_seconds",
  "result", "checks",
];
const PUBLIC_CHECKS = ["version", "clock", "ingest_unsigned", "provider_event_unauthenticated"];

function publicArguments(receive = "on", providerEvent = "on") {
  return [
    "--mode", "public", "--endpoint", ORIGIN,
    "--expect-receive", receive, "--expect-provider-event", providerEvent,
  ];
}

function signedArguments(extra = []) {
  return [
    "--mode", "signed", "--endpoint", ORIGIN,
    "--expect-receive", "on", "--expect-provider-event", "on",
    "--audience", AUDIENCE, "--key-id", KEY_ID,
    "--relay-public-key", vector.public_key_base64, "--relay-private-key-stdin",
    ...extra,
  ];
}

function replaceArgument(argv, name, value) {
  const next = [...argv];
  const offset = next.indexOf(name);
  assert.ok(offset >= 0, "test argument exists");
  next[offset + 1] = value;
  return next;
}

function unreadableInput(isTTY = false) {
  const state = { reads: 0 };
  return {
    state,
    stdin: {
      isTTY,
      [Symbol.asyncIterator]() {
        state.reads += 1;
        throw new Error("unexpected standard input read");
      },
    },
  };
}

function jsonResponse(value, status, cacheControl = null) {
  return new Response(JSON.stringify(value), {
    status,
    headers: {
      "Content-Type": "application/json",
      ...(cacheControl === null ? {} : { "Cache-Control": cacheControl }),
    },
  });
}

function versionResponse(date = new Date(FIXED_NOW).toUTCString()) {
  const response = jsonResponse({
    schema_version: "witself.v0",
    version: "0.0.317-test",
    store_schema_version: 98,
  }, 200);
  if (date !== null) response.headers.set("Date", date);
  return response;
}

function verdictResponse(verdict, status) {
  return jsonResponse({ verdict }, status, "no-store");
}

function providerResponse() {
  return jsonResponse({
    schema_version: "witself.v0",
    code: "auth_failed",
    error: "invalid agent email provider token",
    retryable: false,
  }, 401, "private, no-store");
}

function darkResponse() {
  return new Response("404 page not found\n", {
    status: 404,
    headers: { "Cache-Control": "private, no-store", "Content-Type": "text/plain; charset=utf-8" },
  });
}

function metadataFromHeaders(headers) {
  return {
    version: headers.get("X-Witself-Email-Version"),
    timestamp: Number(headers.get("X-Witself-Email-Timestamp")),
    keyId: headers.get("X-Witself-Email-Key-Id"),
    audience: headers.get("X-Witself-Email-Audience"),
    envelopeFrom: Buffer.from(headers.get("X-Witself-Email-Envelope-From"), "base64url").toString("utf8"),
    envelopeTo: Buffer.from(headers.get("X-Witself-Email-Envelope-To"), "base64url").toString("utf8"),
    rawSize: Number(headers.get("X-Witself-Email-Raw-Size")),
    rawSHA256: headers.get("X-Witself-Email-Raw-SHA256").replace(/^sha256:/, ""),
    spfResult: headers.get("X-Witself-Email-SPF-Result") ?? "",
    dkimResult: headers.get("X-Witself-Email-DKIM-Result") ?? "",
    dmarcResult: headers.get("X-Witself-Email-DMARC-Result") ?? "",
  };
}

function verifyingFetch(receive = "on", providerEvent = "on") {
  const relays = [];
  return {
    relays,
    async fetch(url, init) {
      const path = new URL(url).pathname;
      if (path === "/v1/version") return versionResponse();
      if (path === PROVIDER_EVENT_PATH) return providerEvent === "on" ? providerResponse() : darkResponse();
      assert.ok(path === INGEST_PATH, "fake route");
      const headers = new Headers(init.headers);
      if (!headers.has("X-Witself-Email-Version")) {
        return receive === "on" ? verdictResponse("invalid_relay", 401) : darkResponse();
      }
      let metadata;
      let verified = false;
      try {
        metadata = metadataFromHeaders(headers);
        const publicKey = await webcrypto.subtle.importKey(
          "raw", Buffer.from(vector.public_key_base64, "base64"),
          { name: "Ed25519" }, false, ["verify"],
        );
        verified = await webcrypto.subtle.verify(
          { name: "Ed25519" }, publicKey,
          Buffer.from(headers.get("X-Witself-Email-Signature"), "base64"),
          canonicalSignatureInput(metadata),
        );
        verified = verified && metadata.rawSize === init.body.byteLength &&
          metadata.rawSHA256 === await sha256Hex(init.body, webcrypto);
      } catch {
        verified = false;
      }
      relays.push({ metadata, verified, headers, body: init.body });
      return verified && metadata.audience === AUDIENCE
        ? verdictResponse("unknown_recipient", 404)
        : verdictResponse("invalid_relay", 401);
    },
  };
}

async function withNetworkBlocked(action) {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = () => { throw new Error("network forbidden in reachability test"); };
  try {
    return await action();
  } finally {
    globalThis.fetch = originalFetch;
  }
}

async function execute(argv, overrides = {}) {
  const requests = [];
  const timeouts = [];
  const state = { stdout: "", stderr: "", randomCalls: 0 };
  let randomOffset = 0;
  const fake = overrides.fetch ?? verifyingFetch().fetch;
  const runtime = {
    now: () => FIXED_NOW,
    crypto: webcrypto,
    stdin: Readable.from([Buffer.from(`${vector.pkcs8_base64}\n`)]),
    randomBytes(n) {
      state.randomCalls += 1;
      return Uint8Array.from({ length: n }, () => randomOffset++ & 255);
    },
    ...overrides,
    async fetch(url, init) {
      requests.push({ url, init });
      return fake(url, init, requests.length);
    },
    stdout: { write(chunk) { state.stdout += String(chunk); } },
    stderr: { write(chunk) { state.stderr += String(chunk); } },
  };
  const originalTimeout = AbortSignal.timeout;
  try {
    AbortSignal.timeout = (ms) => {
      timeouts.push(ms);
      return originalTimeout.call(AbortSignal, ms);
    };
    const code = await withNetworkBlocked(() => main(argv, runtime));
    return { code, requests, timeouts, largeBody: argv.includes("--large-body"), ...state };
  } finally {
    AbortSignal.timeout = originalTimeout;
  }
}

function outputDocument(run, name) {
  let document;
  try {
    document = JSON.parse(run.stdout);
  } catch {
    assert.fail(name);
  }
  assert.ok(run.stdout === `${JSON.stringify(document)}\n`, name);
  assert.ok(run.stderr === "", name);
  assert.ok(JSON.stringify(Object.keys(document)) === JSON.stringify(OUTPUT_KEYS), name);
  for (const check of document.checks) {
    assert.ok(JSON.stringify(Object.keys(check)) === JSON.stringify(["name", "result", "status", "reason"]), name);
  }
  return document;
}

function assertRequests(run, paths, name) {
  assert.equal(run.requests.length, paths.length, name);
  assert.equal(run.timeouts.length, run.requests.length, `${name}: one timeout per request`);
  for (const [index, path] of paths.entries()) {
    const { url, init } = run.requests[index];
    assert.ok(url === ORIGIN + path, name);
    assert.ok(init.method === (index === 0 ? "GET" : "POST"), name);
    assert.ok(init.redirect === "manual", name);
    assert.ok(init.signal instanceof AbortSignal, name);
    // The seventh check is the sixth request: the clock check sends no request.
    assert.equal(run.timeouts[index], run.largeBody && index === 5 ? 90000 : 15000, `${name}: timeout`);
    assert.ok(!new Headers(init.headers).has("Authorization"), name);
  }
}

function routeProjection(realmLabel) {
  return signTestRouteProjection({
    schema_version: 2,
    account_id: "acc_aaaaaaaaaaaaaaaa",
    domain: PROBE_DOMAIN,
    realm_label: realmLabel,
    realm_id: example.realm_id,
    route_kind: "canonical",
    state: "applied",
    controller_revision: 7,
    updated_at: new Date(FIXED_NOW).toISOString(),
    cache_ttl_seconds: 300,
    cell_audience: AUDIENCE,
    ingest_url: ORIGIN + INGEST_PATH,
  });
}

function dynamicEnv(routes, version) {
  const values = new Map(Object.entries(routes).map(([realmLabel, value]) => [
    realmRouteKey(value?.domain ?? PROBE_DOMAIN, realmLabel), value,
  ]));
  const allowLimiter = { async limit() { return { success: true }; } };
  return {
    AGENT_EMAIL_DOMAIN: PROBE_DOMAIN,
    AGENT_EMAIL_LEGACY_DOMAINS: example.domain,
    RELAY_KEY_ID: KEY_ID,
    RELAY_ED25519_PRIVATE_KEY: vector.pkcs8_base64,
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: ROUTE_PUBLIC_KEY_ENV,
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "acc_aaaaaaaaaaaaaaaa",
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "true",
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "true",
    REALM_ROUTE_COLD_MISS_LIMITER: allowLimiter,
    REALM_ROUTE_KNOWN_MISS_LIMITER: allowLimiter,
    AGENT_EMAIL_RELAY_VERSION: version,
    EMAIL_DIRECTORY: {
      async get(key, type) {
        assert.equal(type, "json", "Worker route lookup");
        return values.get(key) ?? null;
      },
    },
  };
}

async function assertWorkerParity(version, name) {
  await withNetworkBlocked(async () => {
    const recipient = `${PROBE_AGENT_SEGMENT}.${example.realm_label}@${PROBE_DOMAIN}`;
    const raw = syntheticMessage({ recipient, nonce: NONCE, nowMS: FIXED_NOW, size: "small" });
    const requests = [];
    const message = {
      from: PROBE_ENVELOPE_FROM,
      to: recipient,
      rawSize: raw.byteLength,
      raw: new ReadableStream({ start(controller) { controller.enqueue(raw); controller.close(); } }),
      setReject() {},
    };
    await handleEmail(message, dynamicEnv({ [example.realm_label]: routeProjection(example.realm_label) }, version), {
      now: () => FIXED_NOW,
      crypto: webcrypto,
      async fetch(url, init) {
        requests.push({ url, init });
        return verdictResponse("unknown_recipient", 404);
      },
    });
    const relay = await buildSignedRelay({
      version,
      timestamp: Math.floor(FIXED_NOW / 1000),
      keyId: KEY_ID,
      envelopeFrom: PROBE_ENVELOPE_FROM,
      envelopeTo: recipient,
      audience: AUDIENCE,
      raw,
    }, await importSigningKey(vector.pkcs8_base64, webcrypto), webcrypto);
    assert.equal(requests.length, 1, name);
    assert.ok(requests[0].url === ORIGIN + INGEST_PATH, name);
    assert.ok(requests[0].init.method === "POST", name);
    assert.ok(requests[0].init.redirect === "manual", name);
    const sortedHeaders = (headers) => [...new Headers(headers)]
      .map(([key, value]) => [key.toLowerCase(), value])
      .sort(([left], [right]) => left.localeCompare(right));
    assert.ok(JSON.stringify(sortedHeaders(requests[0].init.headers)) === JSON.stringify(sortedHeaders(relay.headers)), name);
    assert.ok(Buffer.from(requests[0].init.body).equals(Buffer.from(relay.body)), name);
    if (version === RELAY_SIGNATURE_VERSION) {
      assert.ok(![...relay.headers.keys()].some((key) => /^x-witself-email-.*-result$/.test(key)), name);
    }
  });
}

test("reachability expectations equal the shared cell contract", () => {
  assert.deepEqual(REACHABILITY_CONTRACT_CASES, contract.cases);
  assert.equal(contract.schema, REACHABILITY_CONTRACT_SCHEMA);
  assert.equal(contract.control_audience, CONTROL_AUDIENCE);
  assert.ok(Object.isFrozen(REACHABILITY_CONTRACT_CASES));
  assert.ok(REACHABILITY_CONTRACT_CASES.every(Object.isFrozen));
  const parsed = parseRouteAddress(contract.example_recipient, false);
  assert.equal(parsed.agentSegment, PROBE_AGENT_SEGMENT);
  assert.equal(parsed.domain, PROBE_DOMAIN);
  assert.ok(/^[a-z2-7]{16}$/.test(parsed.realmLabel));
  let next = 0;
  const random = (count) => Uint8Array.from({ length: count }, () => next++);
  assert.ok(syntheticLabel(random) === "abcdefghijklmnop", "synthetic recipient label");
  assert.ok(syntheticLabel(random) === NONCE, "synthetic nonce label");
});

test("the probe builds exactly the receive Worker's v2 relay request", async () => {
  await assertWorkerParity(RELAY_SIGNATURE_VERSION_V2, "Worker v2 parity");
});

test("the probe builds exactly the receive Worker's v1 relay request", async () => {
  await assertWorkerParity(RELAY_SIGNATURE_VERSION, "Worker v1 parity");
});

test("the synthetic message is exact and the large body keeps SMTP line limits", () => {
  const input = {
    recipient: "reachability-probe.aaaaaaaaaaaaaaaa@witmail.net",
    nonce: "bbbbbbbbbbbbbbbb",
    nowMS: FIXED_NOW,
  };
  const expected = [
    "From: Witself reachability probe <witself-reachability-probe@probe.invalid>",
    `To: <${input.recipient}>`,
    "Subject: Witself cell reachability probe",
    "Message-ID: <witself-reachability-probe-bbbbbbbbbbbbbbbb@probe.invalid>",
    "Date: Tue, 29 Sep 2026 18:00:00 GMT",
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=us-ascii",
    "",
    "Synthetic reachability probe for an address that no agent owns.",
    "",
  ].join("\r\n");
  const small = syntheticMessage({ ...input, size: "small" });
  assert.ok(Buffer.from(small).equals(Buffer.from(expected, "ascii")), "exact small message");
  const large = syntheticMessage({ ...input, size: LARGE_BODY_BYTES });
  assert.ok(large.byteLength === 4194304, "large message byte count");
  const text = Buffer.from(large).toString("latin1");
  assert.ok(text.startsWith(expected) && text.endsWith("\r\n"), "large message boundaries");
  assert.ok(!/[^\x20-\x7e\r\n]/.test(text), "large message ASCII");
  const lines = text.slice(0, -2).split("\r\n");
  assert.ok(lines.every((line) => line.length + 2 <= 1000 && !/[\r\n]/.test(line)), "SMTP line bounds");
  const filler = text.slice(expected.length, -2).split("\r\n");
  assert.ok(filler.every((line) => /^x+$/.test(line) && line.length + 2 >= 3), "filler minimum and content");
});

test("public mode proves an enabled cell with three requests and no credential", async () => {
  const input = unreadableInput();
  const run = await execute(publicArguments(), { stdin: input.stdin });
  assert.equal(run.code, 0);
  const output = outputDocument(run, "public enabled output");
  assert.ok(output.schema_version === REACHABILITY_SCHEMA && output.mode === "public", "public schema");
  assert.ok(output.host === HOST && output.cell === AUDIENCE, "public target");
  assert.ok(output.audience === null && output.relay_version === null && output.large_body === false, "public credentials absent");
  assert.ok(output.cell_version === "0.0.317-test" && output.store_schema_version === 98 && output.clock_skew_seconds === 0, "public version and clock");
  assert.ok(output.result === "pass" && output.checks.length === 4, "public checks");
  assert.ok(JSON.stringify(output.checks.map((check) => check.name)) === JSON.stringify(PUBLIC_CHECKS), "public check order");
  assert.ok(output.checks.every((check) => check.result === "pass" && check.reason === null), "public checks pass");
  assert.ok(JSON.stringify(output.checks.map((check) => check.status)) === JSON.stringify([200, null, 401, 401]), "public statuses");
  assertRequests(run, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH], "public requests");
  assert.ok(new Headers(run.requests[0].init.headers).get("Accept") === "application/json", "version accept header");
  const ingest = run.requests[1].init;
  assert.ok(![...new Headers(ingest.headers).keys()].some((key) => key.startsWith("x-witself-email-")), "unsigned ingest headers");
  assert.ok(new Headers(ingest.headers).get("Content-Type") === "message/rfc822" && (ingest.body ?? "").length === 0, "unsigned ingest body");
  assert.ok(new Headers(run.requests[2].init.headers).get("Content-Type") === "application/json" && run.requests[2].init.body === "{}", "provider event request");
  assert.equal(input.state.reads, 0);
  assert.equal(run.randomCalls, 0);
  const clusterRun = await execute(replaceArgument(publicArguments(), "--endpoint", CLUSTER_ORIGIN));
  const cluster = outputDocument(clusterRun, "cluster public output");
  assert.equal(clusterRun.code, 0);
  assert.ok(cluster.cell === null && cluster.host === new URL(CLUSTER_ORIGIN).hostname, "cluster host accepted");
});

test("public mode proves a dark cell and stops at the first unexpected route", async () => {
  const dark = verifyingFetch("off", "off");
  const run = await execute(publicArguments("off", "off"), { fetch: dark.fetch });
  assert.equal(run.code, 0);
  assert.ok(outputDocument(run, "dark cell").result === "pass", "dark cell");
  assertRequests(run, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH], "dark requests");
  for (const [receive, providerEvent, statuses] of [
    ["on", "off", [200, null, 401, 404]],
    ["off", "on", [200, null, 404, 401]],
  ]) {
    const mixed = await execute(publicArguments(receive, providerEvent), {
      fetch: verifyingFetch(receive, providerEvent).fetch,
    });
    assert.equal(mixed.code, 0, "mixed route expectations");
    const output = outputDocument(mixed, "mixed route expectations");
    assert.ok(output.result === "pass", "mixed route expectations");
    assert.deepEqual(output.checks.map((check) => check.status), statuses, "mixed route statuses");
    assertRequests(mixed, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH], "mixed route requests");
  }
  const unexpectedProvider = await execute(publicArguments("on", "off"), { fetch: verifyingFetch("on", "on").fetch });
  assert.equal(unexpectedProvider.code, 1, "unexpected enabled provider route");
  const providerOutput = outputDocument(unexpectedProvider, "unexpected enabled provider route");
  assert.ok(providerOutput.checks[3].name === "provider_event_unauthenticated" && providerOutput.checks[3].reason === "unexpected_status", "unexpected enabled provider route");
  assertRequests(unexpectedProvider, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH], "unexpected provider requests");
  for (const [expectReceive, missingHeader, reason] of [
    ["off", true, "unexpected_body"],
    ["on", false, "unexpected_status"],
  ]) {
    const failed = await execute(publicArguments(expectReceive, "off"), {
      fetch: async (_url, _init, count) => count === 1 ? versionResponse()
        : missingHeader ? new Response("404 page not found\n", { status: 404 }) : darkResponse(),
    });
    assert.equal(failed.code, 1, reason);
    const output = outputDocument(failed, reason);
    assert.ok(output.result === "fail", reason);
    assert.ok(output.checks[2].name === "ingest_unsigned" && output.checks[2].result === "fail" && output.checks[2].reason === reason, reason);
    assert.ok(output.checks[3].result === "not_run" && output.checks[3].status === null && output.checks[3].reason === null, reason);
    assert.equal(failed.requests.length, 2, reason);
  }
});

test("signed mode signs only for the control audience and the cell's own audience", async () => {
  const fake = verifyingFetch();
  const run = await execute(signedArguments(), { fetch: fake.fetch });
  assert.equal(run.code, 0);
  const output = outputDocument(run, "signed happy path");
  assert.ok(output.mode === "signed" && output.audience === AUDIENCE && output.relay_version === RELAY_SIGNATURE_VERSION_V2 && output.large_body === false, "signed mode fields");
  assert.ok(output.result === "pass" && output.checks.length === 6 && output.checks.every((check) => check.result === "pass"), "signed happy path");
  assertRequests(run, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH, INGEST_PATH, INGEST_PATH], "signed request order");
  assert.equal(run.randomCalls, 2, "signed random draws");
  assert.equal(fake.relays.length, 2, "signed relay count");
  for (const [index, relay] of fake.relays.entries()) {
    assert.ok(relay.verified, "signed verification");
    assert.ok(relay.metadata.audience === (index === 0 ? CONTROL_AUDIENCE : AUDIENCE), "signed audiences");
    assert.ok(relay.metadata.version === RELAY_SIGNATURE_VERSION_V2, "signed version");
    assert.ok([relay.metadata.spfResult, relay.metadata.dkimResult, relay.metadata.dmarcResult].every((value) => value === "unknown"), "signed verdicts");
    assert.ok(relay.metadata.envelopeTo === RECIPIENT && relay.metadata.envelopeFrom === PROBE_ENVELOPE_FROM, "signed envelope");
    assert.ok(relay.metadata.timestamp === Math.floor(FIXED_NOW / 1000), "signed timestamp");
  }
  assert.ok(Buffer.from(fake.relays[0].body).equals(Buffer.from(fake.relays[1].body)), "signed body reuse");
  let clockCalls = 0;
  const advancing = verifyingFetch();
  const advancingRun = await execute(signedArguments(), {
    fetch: advancing.fetch,
    now: () => FIXED_NOW + clockCalls++ * 1000,
  });
  assert.equal(advancingRun.code, 0, "fresh per-request timestamps");
  assert.ok(advancing.relays[1].metadata.timestamp > advancing.relays[0].metadata.timestamp, "fresh per-request timestamps");

  const randomRecipients = [];
  for (let index = 0; index < 2; index += 1) {
    const randomFake = verifyingFetch();
    const randomRun = await execute(signedArguments(), { fetch: randomFake.fetch, randomBytes: undefined });
    assert.equal(randomRun.code, 0, "default randomness signed happy path");
    assertRequests(randomRun, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH, INGEST_PATH, INGEST_PATH], "default randomness requests");
    assert.equal(randomFake.relays.length, 2, "default randomness relay count");
    const recipient = randomFake.relays[1].metadata.envelopeTo;
    assert.ok(randomFake.relays[0].metadata.envelopeTo === recipient, "default randomness recipient reuse");
    assert.ok(/^[a-z2-7]{16}$/.test(parseRouteAddress(recipient, false).realmLabel), "default randomness canonical label");
    assert.ok(recipient !== contract.example_recipient, "default randomness avoids example recipient");
    randomRecipients.push(recipient);
  }
  assert.ok(randomRecipients[0] !== randomRecipients[1], "default randomness differs between invocations");
});

test("a cell that accepts the control audience stops the probe before the real relay", async () => {
  const fake = verifyingFetch();
  const run = await execute(signedArguments(), {
    fetch: async (url, init, count) => count === 4 ? verdictResponse("unknown_recipient", 404) : fake.fetch(url, init),
  });
  assert.equal(run.code, 1);
  const output = outputDocument(run, "accepted control audience");
  assert.ok(output.result === "fail" && output.checks[4].name === "ingest_control_audience" && output.checks[4].reason === "unexpected_status", "accepted control audience");
  assert.ok(output.checks[5].result === "not_run" && output.checks[5].status === null && output.checks[5].reason === null, "real relay not run");
  assert.equal(run.requests.length, 4);

  const temporaryFake = verifyingFetch();
  const temporary = await execute(signedArguments(), {
    fetch: (url, init, count) => count === 5
      ? new Response(JSON.stringify({ verdict: "temporary" }), {
        status: 503,
        headers: { "Content-Type": "application/json", "Cache-Control": "no-store", "Retry-After": "1" },
      }) : temporaryFake.fetch(url, init),
  });
  assert.equal(temporary.code, 1, "temporary ingest is not retried");
  const temporaryOutput = outputDocument(temporary, "temporary ingest is not retried");
  assert.ok(temporaryOutput.checks[5].name === "ingest_unknown_recipient" && temporaryOutput.checks[5].reason === "unexpected_status" && temporaryOutput.checks[5].status === 503, "temporary ingest is not retried");
  assert.equal(temporaryFake.relays.length, 1, "only control relay reached verifier");
  assertRequests(temporary, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH, INGEST_PATH, INGEST_PATH], "temporary ingest requests");
});

test("the large-body relay carries exactly 4 MiB and still verifies", async () => {
  const fake = verifyingFetch();
  const run = await execute(signedArguments(["--large-body"]), { fetch: fake.fetch });
  assert.equal(run.code, 0);
  const output = outputDocument(run, "large relay");
  assert.ok(output.result === "pass" && output.large_body && output.checks.length === 7 && output.checks.every((check) => check.result === "pass"), "large relay checks");
  assertRequests(run, ["/v1/version", INGEST_PATH, PROVIDER_EVENT_PATH, INGEST_PATH, INGEST_PATH, INGEST_PATH], "large relay requests");
  const relay = fake.relays[2];
  assert.ok(relay.verified && relay.body.byteLength === 4194304 && relay.metadata.rawSize === 4194304, "large relay signature and size");
  assert.ok(relay.metadata.envelopeTo === RECIPIENT && relay.metadata.audience === AUDIENCE, "large relay target");
  assert.ok(Buffer.from(relay.body).subarray(0, fake.relays[1].body.byteLength).equals(Buffer.from(fake.relays[1].body)), "large relay prefix");
});

test("preconditions refuse before any request", async () => {
  const secondKey = generateKeyPairSync("ed25519").privateKey.export({ format: "der", type: "pkcs8" }).toString("base64");
  const cases = [];
  const add = (name, code, argv, input = null) => cases.push({ name, code, argv, input });
  add("key mismatch", "key_mismatch", signedArguments(), secondKey);
  add("terminal", "private_key_input_invalid", signedArguments(), { terminal: true });
  for (const [name, input] of [
    ["empty", ""], ["whitespace", " \t\r\n"],
    ["oversized", Buffer.alloc(PRIVATE_KEY_INPUT_MAXIMUM_BYTES + 1, 0x78)],
    ["invalid UTF-8", Buffer.from([0xff, 0xfe])],
  ]) add(name, "private_key_input_invalid", signedArguments(), input);
  add("read error", "private_key_input_invalid", signedArguments(), { readError: true });
  add("invalid key text", "invalid_private_key", signedArguments(), "not-a-key");
  add("maximum input length", "invalid_private_key", signedArguments(), Buffer.alloc(PRIVATE_KEY_INPUT_MAXIMUM_BYTES, 0x78));
  add("invalid DER", "invalid_private_key", signedArguments(), Buffer.alloc(48).toString("base64"));
  add("audience host mismatch", "audience_host_mismatch", replaceArgument(signedArguments(), "--audience", "another-cell"));
  add("control audience", "invalid_audience", replaceArgument(signedArguments(), "--audience", CONTROL_AUDIENCE));
  add("uppercase audience", "invalid_audience", replaceArgument(signedArguments(), "--audience", "Civo-prod-use1-serving"));
  add("digest-shaped audience", "invalid_audience", replaceArgument(
    replaceArgument(signedArguments(), "--endpoint", CLUSTER_ORIGIN), "--audience", "a".repeat(64),
  ));
  add("key id", "invalid_key_id", replaceArgument(signedArguments(), "--key-id", "Bad.Key"));
  add("public key", "invalid_public_key", replaceArgument(signedArguments(), "--relay-public-key", "not-a-public-key"));
  add("relay version", "invalid_relay_version", signedArguments(["--relay-version", "unknown"]));
  for (const [name, endpoint] of [
    ["HTTP", `http://${HOST}`], ["path", `${ORIGIN}/path`], ["query", `${ORIGIN}?q=1`],
    ["port", `${ORIGIN}:443`], ["credentials", `https://user:password@${HOST}`],
    ["trailing dot", `${ORIGIN}.`], ["uppercase", `https://${HOST.toUpperCase()}`],
    ["wrong prefix", `https://x${HOST}`], ["unrelated host", "https://example.com"],
    ["fragment", `${ORIGIN}#fragment`], ["escaped host", ORIGIN.replace("api.", "%61pi.")],
  ]) add(name, "invalid_endpoint", replaceArgument(signedArguments(), "--endpoint", endpoint));
  for (const [name, argv] of [
    ["unknown option", [...publicArguments(), "--unknown", "value"]],
    ["repeat option", [...publicArguments(), "--mode", "public"]],
    ["missing value", [...publicArguments(), "--audience"]],
    ["option as value", [...publicArguments(), "--audience", "--key-id"]],
    ["empty value", replaceArgument(publicArguments(), "--endpoint", "")],
    ["public audience", [...publicArguments(), "--audience", AUDIENCE]],
    ["public key id", [...publicArguments(), "--key-id", KEY_ID]],
    ["public key", [...publicArguments(), "--relay-public-key", vector.public_key_base64]],
    ["public relay version", [...publicArguments(), "--relay-version", RELAY_SIGNATURE_VERSION_V2]],
    ["public stdin", [...publicArguments(), "--relay-private-key-stdin"]],
    ["public large body", [...publicArguments(), "--large-body"]],
    ["missing stdin flag", signedArguments().filter((arg) => arg !== "--relay-private-key-stdin")],
    ["signed off", replaceArgument(signedArguments(), "--expect-receive", "off")],
    ["positional", [...publicArguments(), "position"]],
    ["invalid mode", replaceArgument(publicArguments(), "--mode", "other")],
    ["invalid receive", replaceArgument(publicArguments(), "--expect-receive", "other")],
    ["invalid provider", replaceArgument(publicArguments(), "--expect-provider-event", "other")],
    ["duplicate boolean", [...signedArguments(), "--relay-private-key-stdin"]],
    ["boolean value", [...signedArguments(), "value"]],
  ]) add(name, "invalid_arguments", argv);

  for (const entry of cases) {
    const unread = unreadableInput(entry.input?.terminal === true);
    let stdin = unread.stdin;
    let destroyed = false;
    if (entry.input !== null && !entry.input.terminal && !entry.input.readError) {
      stdin = Readable.from([Buffer.from(entry.input)]);
      const originalDestroy = stdin.destroy.bind(stdin);
      stdin.destroy = (...args) => { if (!stdin.destroyed) destroyed = true; return originalDestroy(...args); };
    }
    const run = await execute(entry.argv, {
      stdin,
      fetch: () => { throw new Error("precondition made a request"); },
    });
    assert.equal(run.code, 2, entry.name);
    assert.ok(run.stdout === "", entry.name);
    assert.ok(run.stderr === `witself-agent-email-cell-reachability: ${entry.code}\n`, entry.name);
    assert.equal(run.requests.length, 0, entry.name);
    assert.equal(run.randomCalls, 0, entry.name);
    if (entry.input === null || entry.input.terminal) assert.equal(unread.state.reads, 0, entry.name);
    if (entry.input?.readError) assert.equal(unread.state.reads, 1, entry.name);
    if (entry.name === "oversized") assert.ok(destroyed, entry.name);
  }

  let pulled = 0;
  const stdin = {
    async *[Symbol.asyncIterator]() {
      for (const chunk of [Buffer.alloc(3000, 0x78), Buffer.alloc(3000, 0x78), Buffer.alloc(3000, 0x78)]) {
        pulled += 1;
        yield chunk;
      }
    },
  };
  const run = await execute(signedArguments(), {
    stdin,
    fetch: () => { throw new Error("precondition made a request"); },
  });
  assert.equal(run.code, 2, "cumulative input limit");
  assert.ok(run.stdout === "" && run.stderr === "witself-agent-email-cell-reachability: private_key_input_invalid\n", "cumulative input limit");
  assert.equal(run.requests.length, 0, "cumulative input limit");
  assert.equal(run.randomCalls, 0, "cumulative input limit");
  assert.equal(pulled, 2, "stop at the chunk crossing the cumulative input limit");
});

test("clock, redirect, oversized and failed responses fail closed", async () => {
  const cases = [
    ["clock ahead", "clock_skew", "clock", () => versionResponse(new Date(FIXED_NOW + 61000).toUTCString())],
    ["clock behind", "clock_skew", "clock", () => versionResponse(new Date(FIXED_NOW - 61000).toUTCString())],
    ["date missing", "date_header_missing", "clock", () => versionResponse(null)],
    ["date invalid", "date_header_missing", "clock", () => versionResponse("invalid")],
    ["redirect", "redirect", "version", () => new Response("", { status: 302, headers: { Location: ORIGIN } })],
    ["gateway error", "unexpected_status", "version", () => new Response("bad gateway", { status: 502 })],
    ["oversized response", "invalid_response", "version", () => new Response("x".repeat(4097), { status: 200 })],
    ["rejected fetch", "request_failed", "version", () => { throw new Error("synthetic fetch failure"); }],
    ["failed body", "request_failed", "version", () => new Response(new ReadableStream({ start(controller) { controller.error(new Error("synthetic body failure")); } }), { status: 200 })],
    ["bad version body", "unexpected_body", "version", () => jsonResponse({ schema_version: "witself.v0", version: "bad version", store_schema_version: 98 }, 200)],
    ["digest-shaped version", "unexpected_body", "version", () => jsonResponse({ schema_version: "witself.v0", version: "a".repeat(64), store_schema_version: 98 }, 200)],
  ];
  for (const [name, reason, checkName, response] of cases) {
    const run = await execute(publicArguments(), { fetch: response });
    assert.equal(run.code, 1, name);
    const output = outputDocument(run, name);
    const index = output.checks.findIndex((check) => check.name === checkName);
    assert.ok(output.result === "fail" && output.checks[index].result === "fail" && output.checks[index].reason === reason, name);
    assert.ok(output.checks.slice(index + 1).every((check) => check.result === "not_run" && check.status === null && check.reason === null), name);
    assert.equal(run.requests.length, 1, name);
  }
  for (const seconds of [-60, 60]) {
    const fake = verifyingFetch();
    const run = await execute(publicArguments(), {
      fetch: (url, init, count) => count === 1 ? versionResponse(new Date(FIXED_NOW + seconds * 1000).toUTCString()) : fake.fetch(url, init),
    });
    assert.equal(run.code, 0, "clock boundary accepted");
    assert.ok(outputDocument(run, "clock boundary").clock_skew_seconds === seconds, "clock boundary");
  }
  for (const [elapsed, seconds, expectedSkew, expectedCode] of [
    [2000, 61, 60, 0], [1200, 61, 60, 0], [800, 61, 61, 1],
    [800, -60, -60, 0], [1200, -60, -61, 1],
  ]) {
    let calls = 0;
    const fake = verifyingFetch();
    const run = await execute(publicArguments(), {
      now: () => FIXED_NOW + (calls++ === 0 ? 0 : elapsed),
      fetch: (url, init, count) => count === 1 ? versionResponse(new Date(FIXED_NOW + seconds * 1000).toUTCString()) : fake.fetch(url, init),
    });
    assert.equal(run.code, expectedCode, "rounded midpoint clock");
    assert.ok(outputDocument(run, "rounded midpoint clock").clock_skew_seconds === expectedSkew, "rounded midpoint clock");
    assert.equal(run.requests.length, expectedCode === 0 ? 3 : 1, "rounded midpoint stop");
  }
  for (const [name, response] of [
    ["extra verdict key", jsonResponse({ verdict: "invalid_relay", extra: true }, 401, "no-store")],
    ["verdict content type", new Response('{"verdict":"invalid_relay"}', { status: 401, headers: { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" } })],
    ["verdict array", jsonResponse(["invalid_relay"], 401, "no-store")],
    ["verdict private cache", jsonResponse({ verdict: "invalid_relay" }, 401, "private, no-store")],
    ["verdict missing cache", jsonResponse({ verdict: "invalid_relay" }, 401)],
  ]) {
    const run = await execute(publicArguments(), { fetch: (_url, _init, count) => count === 1 ? versionResponse() : response });
    const output = outputDocument(run, name);
    assert.equal(run.code, 1, name);
    assert.ok(output.checks[2].reason === "unexpected_body" && output.checks[3].result === "not_run", name);
  }
  const validProviderBody = {
    schema_version: "witself.v0", code: "auth_failed", error: "invalid agent email provider token", retryable: false,
  };
  for (const [name, body] of [
    ["provider extra key", { ...validProviderBody, extra: true }],
    ["provider retryable", { ...validProviderBody, retryable: true }],
    ["provider schema", { ...validProviderBody, schema_version: "other" }],
    ["provider empty error", { ...validProviderBody, error: "" }],
    ["provider long error", { ...validProviderBody, error: "x".repeat(257) }],
  ]) {
    const fake = verifyingFetch();
    const run = await execute(publicArguments(), {
      fetch: (url, init, count) => count === 3 ? jsonResponse(body, 401, "private, no-store") : fake.fetch(url, init),
    });
    assert.equal(run.code, 1, name);
    assert.ok(outputDocument(run, name).checks[3].reason === "unexpected_body", name);
  }
  const providerFake = verifyingFetch();
  const providerCache = await execute(publicArguments(), {
    fetch: (url, init, count) => count === 3 ? jsonResponse(validProviderBody, 401, "no-store") : providerFake.fetch(url, init),
  });
  assert.equal(providerCache.code, 1, "provider cache header");
  assert.ok(outputDocument(providerCache, "provider cache header").checks[3].reason === "unexpected_body", "provider cache header");

  const signedFake = verifyingFetch();
  const signedCache = await execute(signedArguments(), {
    fetch: (url, init, count) => count === 5 ? jsonResponse({ verdict: "unknown_recipient" }, 404, "private, no-store") : signedFake.fetch(url, init),
  });
  assert.equal(signedCache.code, 1, "signed ingest cache header");
  assert.ok(outputDocument(signedCache, "signed ingest cache header").checks[5].reason === "unexpected_body", "signed ingest cache header");
});

test("output and errors carry no key, signature, digest, recipient or nonce", async () => {
  const runs = [
    await execute(signedArguments()),
    await execute(signedArguments(), { stdin: Readable.from([Buffer.from("not-a-key")]) }),
    await execute(signedArguments(), { fetch: () => { throw new Error(vector.pkcs8_base64); } }),
    await execute(signedArguments(), { randomBytes: () => { throw new Error(vector.pkcs8_base64); } }),
  ];
  assert.ok(JSON.stringify(runs.map((run) => run.code)) === JSON.stringify([0, 2, 1, 1]), "output cases");
  assert.ok(runs[3].stdout === "" && runs[3].stderr === "witself-agent-email-cell-reachability: unavailable\n", "internal failure output");
  for (const run of runs) {
    const printed = run.stdout + run.stderr;
    const forbidden = [vector.pkcs8_base64, vector.public_key_base64, "reachability-probe.", "abcdefghijklmnop", NONCE];
    for (const { init } of run.requests) {
      const headers = new Headers(init.headers);
      if (headers.has("X-Witself-Email-Signature")) forbidden.push(headers.get("X-Witself-Email-Signature"));
    }
    for (const value of forbidden) assert.ok(!printed.includes(value), "value-free output");
    assert.ok(!/[0-9a-f]{64}/i.test(printed), "value-free output");
  }
});
