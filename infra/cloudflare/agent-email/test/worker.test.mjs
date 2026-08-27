import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { createRouteLookupState, handleEmail } from "../src/index.js";
import {
  CONFIG_KEY,
  realmRouteKey,
  recipientKey,
  runtimeConfig,
  runtimeRecipient,
} from "../src/directory.mjs";
import { RELAY_MAXIMUM_RAW_BYTES } from "../src/relay.mjs";
import { EDGE_METRICS_SCHEMA, ROUTE_LOOKUP_METRICS_SCHEMA } from "../src/metrics.mjs";
import {
  ROUTE_PUBLIC_KEY_ENV,
  signTestRouteProjection,
} from "./route-signature-fixture.mjs";

const vector = JSON.parse(await readFile(new URL("./golden-vector.json", import.meta.url), "utf8"));
const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));
const raw = Buffer.from(vector.raw_base64, "base64");
const first = example.agents[0];
const primaryDomain = "witmail.net";
const aliasLabel = "acme-west";
const customDomain = "agents.example.com";
const canonicalAddress = `alpha.${example.realm_label}@${primaryDomain}`;
const aliasAddress = `alpha.${aliasLabel}@${primaryDomain}`;
const customAddress = `alpha.${aliasLabel}@${customDomain}`;
const vectorNowMS = vector.metadata.timestamp * 1000;
const rolloutAccount = "acc_aaaaaaaaaaaaaaaa";
const trustedAuthservID = "mx.trusted.example";
const allowLimiter = { async limit() { return { success: true }; } };

function verdictPoints(points) {
  return points.filter((point) => point.blobs[0] === EDGE_METRICS_SCHEMA);
}

function routeLookupPoints(points) {
  return points.filter((point) => point.blobs[0] === ROUTE_LOOKUP_METRICS_SCHEMA);
}

function routeProjection(realmLabel, overrides = {}) {
  const routeKind = overrides.route_kind ??
    (realmLabel === example.realm_label ? "canonical" : "realm_alias");
  const schemaVersion = overrides.schema_version ??
    (routeKind === "custom_domain" ? 1 : 2);
  const projection = {
    schema_version: schemaVersion,
    ...(routeKind === "custom_domain" || schemaVersion === 1
      ? {}
      : { account_id: rolloutAccount }),
    domain: primaryDomain,
    realm_label: realmLabel,
    realm_id: example.realm_id,
    route_kind: routeKind,
    state: "applied",
    controller_revision: 7,
    updated_at: new Date(vectorNowMS).toISOString(),
    cache_ttl_seconds: 300,
    cell_audience: example.cell_audience,
    ingest_url: example.ingest_url,
    ...overrides,
  };
  if (projection.state !== "applied") {
    delete projection.cell_audience;
    delete projection.ingest_url;
  }
  return signTestRouteProjection(projection);
}

function customRouteProjection(overrides = {}) {
  return routeProjection(aliasLabel, {
    domain: customDomain,
    route_kind: "custom_domain",
    domain_request_id: "aedr_aaaaaaaaaaaaaaaa",
    domain_allocation_revision: 11,
    realm_alias_claim_id: "era_bbbbbbbbbbbbbbbb",
    realm_alias_revision: 19,
    ...overrides,
  });
}

function dynamicEnv(routes, metrics = null, extra = {}) {
  const values = new Map(
    Object.entries(routes).map(([realmLabel, value]) => [
      realmRouteKey(value?.domain ?? primaryDomain, realmLabel),
      value,
    ]),
  );
  return {
    AGENT_EMAIL_DOMAIN: primaryDomain,
    AGENT_EMAIL_LEGACY_DOMAINS: example.domain,
    RELAY_KEY_ID: vector.metadata.key_id,
    RELAY_ED25519_PRIVATE_KEY: vector.pkcs8_base64,
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: ROUTE_PUBLIC_KEY_ENV,
    AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: rolloutAccount,
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "true",
    REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "true",
    REALM_ROUTE_COLD_MISS_LIMITER: allowLimiter,
    REALM_ROUTE_KNOWN_MISS_LIMITER: allowLimiter,
    EMAIL_DIRECTORY: {
      async get(key, type) {
        assert.equal(type, "json");
        return values.get(key) ?? null;
      },
    },
    ...(metrics ? { EMAIL_EDGE_METRICS: metrics } : {}),
    ...extra,
  };
}

function coldRouteEnv(metrics = null, extra = {}) {
  return dynamicEnv({}, metrics, {
    CONTROL_PLANE_URL: "https://control.example/",
    CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
    ...extra,
  });
}

function legacyEnv(metrics = null, extra = {}, routeNowMS = Date.now()) {
  const route = routeProjection(example.realm_label, {
    domain: example.domain,
    updated_at: new Date(routeNowMS).toISOString(),
  });
  return dynamicEnv({}, metrics, {
    EMAIL_DIRECTORY: {
      async get(key, type) {
        assert.equal(type, "json");
        return key === realmRouteKey(example.domain, example.realm_label)
          ? route
          : null;
      },
    },
    ...extra,
  });
}

function message(overrides = {}) {
  const rejected = [];
  return {
    from: vector.metadata.envelope_from,
    to: first.address,
    rawSize: raw.byteLength,
    raw: new ReadableStream({ start(controller) { controller.enqueue(raw); controller.close(); } }),
    setReject(reason) { rejected.push(reason); },
    rejected,
    ...overrides,
  };
}

function messageWithAuthenticationResults(authservID, dmarc) {
  const rawMessage = Buffer.concat([
    Buffer.from(`Authentication-Results: ${authservID}; dmarc=${dmarc}\r\n`, "ascii"),
    raw,
  ]);
  return {
    mail: message({
      rawSize: rawMessage.byteLength,
      raw: new ReadableStream({
        start(controller) {
          controller.enqueue(rawMessage);
          controller.close();
        },
      }),
    }),
    rawMessage,
  };
}

async function captureAccepted(overrides = {}) {
  let request;
  const mail = message(overrides);
  await handleEmail(mail, legacyEnv(null, {}, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  return { mail, request };
}

test("email-only handler signs and relays the byte-identical raw message", async () => {
  const { mail, request } = await captureAccepted();
  assert.deepEqual(mail.rejected, []);
  assert.equal(request.url, example.ingest_url);
  assert.equal(request.init.method, "POST");
  assert.equal(request.init.redirect, "manual");
  assert.deepEqual(Buffer.from(request.init.body), raw);
  const headers = request.init.headers;
  assert.equal(headers.get("X-Witself-Email-Version"), "witself-email-relay-pilot-v1");
  assert.equal(headers.get("X-Witself-Email-Timestamp"), String(vector.metadata.timestamp));
  assert.equal(headers.get("X-Witself-Email-Raw-Size"), String(raw.byteLength));
  assert.equal(headers.get("X-Witself-Email-Raw-SHA256"), `sha256:${vector.metadata.raw_sha256}`);
  assert.equal(headers.get("X-Witself-Email-Signature"), vector.signature_base64);
  assert.equal(Buffer.from(headers.get("X-Witself-Email-Envelope-To"), "base64url").toString(), first.address);
});

test("Worker locally accepts and relays the exact 25 MiB inbound boundary", async () => {
  const exactBoundaryRaw = Buffer.alloc(RELAY_MAXIMUM_RAW_BYTES, 0x61);
  const mail = message({
    rawSize: exactBoundaryRaw.byteLength,
    raw: new ReadableStream({
      start(controller) {
        controller.enqueue(exactBoundaryRaw);
        controller.close();
      },
    }),
  });
  let request;
  await handleEmail(mail, legacyEnv(null, {}, vectorNowMS), {
    now: () => vectorNowMS,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });

  assert.deepEqual(mail.rejected, []);
  assert.equal(request.url, example.ingest_url);
  assert.equal(request.init.body.byteLength, RELAY_MAXIMUM_RAW_BYTES);
  assert.equal(
    request.init.headers.get("X-Witself-Email-Raw-Size"),
    String(RELAY_MAXIMUM_RAW_BYTES),
  );
  assert.deepEqual(
    Buffer.from(request.init.body).subarray(0, 64),
    exactBoundaryRaw.subarray(0, 64),
  );
});

test("subaddress tags stay in the signed envelope but use the exact enrolled key", async () => {
  const tagged = first.address.replace("@", "+signup@");
  const { request } = await captureAccepted({ to: tagged });
  assert.equal(Buffer.from(request.init.headers.get("X-Witself-Email-Envelope-To"), "base64url").toString(), tagged);
});

test("only a 2xx body containing exactly accepted is success", async () => {
  const failures = [
    new Response('{"verdict":"accepted"}', { status: 503 }),
    new Response('{"verdict":"feature_disabled"}', { status: 503 }),
    new Response('{"verdict":"rate_limited"}', { status: 503 }),
    new Response('{"verdict":"rate_limited","scope":"sender"}', { status: 429 }),
    new Response('{"verdict":"accepted","extra":true}', { status: 200 }),
    new Response('{"verdict":"transient"}', { status: 200 }),
    new Response("not json", { status: 200 }),
  ];
  for (const response of failures) {
    await assert.rejects(
      () => handleEmail(message(), legacyEnv(), { fetch: async () => response }),
      { message: "agent email relay temporarily unavailable" },
    );
  }
});

test("plan-disabled receipt accepts and drops with a value-free metric", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, legacyEnv(metrics), {
    fetch: async () => new Response('{"verdict":"feature_disabled"}', { status: 200 }),
  });
  const verdicts = verdictPoints(points);
  assert.deepEqual(mail.rejected, []);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "discarded_feature_disabled", "response",
  ]);
  assert.equal(verdicts[0].doubles[3], 200);
  assert.doesNotMatch(JSON.stringify(points), /@|address|account|realm_|agent_/i);
});

test("cell capacity receipt rejects permanently with a value-free metric", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, legacyEnv(metrics), {
    fetch: async () => new Response('{"verdict":"storage_full"}', { status: 507 }),
  });
  const verdicts = verdictPoints(points);
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_cell_capacity", "response",
  ]);
  assert.equal(verdicts[0].doubles[3], 552);
  assert.doesNotMatch(JSON.stringify(points), /@|address|account|realm_|agent_/i);
});

test("unknown and permanent cell verdicts use one sanitized permanent rejection", async () => {
  for (const [verdict, status] of [
    ["unknown_recipient", 404],
    ["permanent", 410],
  ]) {
    const mail = message();
    await handleEmail(mail, legacyEnv(), {
      fetch: async () => new Response(JSON.stringify({ verdict }), { status }),
    });
    assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  }
});

test("terminal retry-canary verdict rejects once with no marker leakage", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, legacyEnv(metrics), {
    fetch: async () => new Response('{"verdict":"retry_canary_rejected"}', { status: 410 }),
  });
  const verdicts = verdictPoints(points);
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_retry_canary", "response",
  ]);
  assert.equal(verdicts[0].doubles[3], 410);
  assert.doesNotMatch(JSON.stringify(points), /challenge|retry_canary_rejected|X-Witself/i);
});

test("cell over-size verdict maps to a sanitized SMTP 552 outcome", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, legacyEnv(metrics), {
    fetch: async () => new Response('{"verdict":"over_size"}', { status: 413 }),
  });
  const verdicts = verdictPoints(points);
  assert.deepEqual(mail.rejected, ["message too large"]);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_over_size", "response",
  ]);
  assert.equal(verdicts[0].doubles[3], 552);
  assert.doesNotMatch(
    JSON.stringify(points),
    /@|address|account|realm_|agent_|subject|digest|signature/i,
  );
});

test("exact cell rate-limit verdict maps to a sanitized temporary outcome", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await assert.rejects(
    () => handleEmail(mail, legacyEnv(metrics), {
      fetch: async () => new Response('{"verdict":"rate_limited"}', {
        status: 429,
        headers: { "Retry-After": "17" },
      }),
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  const verdicts = verdictPoints(points);
  assert.deepEqual(mail.rejected, []);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].indexes, ["tempfail_rate_limited"]);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "tempfail_rate_limited", "response",
  ]);
  assert.equal(verdicts[0].doubles[3], 429);
  assert.doesNotMatch(
    JSON.stringify(points),
    /@|address|account|realm_|agent_|sender|recipient|subject|digest|signature/i,
  );
});

test("legacy canonical gate and transport failures use one sanitized transient error", async () => {
  const points = [];
  await assert.rejects(() => handleEmail(
    message(),
    legacyEnv(
      { writeDataPoint(point) { points.push(point); } },
      { REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false" },
    ),
    { now: () => Date.now() },
  ), {
    message: "agent email relay temporarily unavailable",
  });
  const lookups = routeLookupPoints(points);
  assert.equal(lookups.length, 1);
  assert.deepEqual(lookups[0].blobs, [
    ROUTE_LOOKUP_METRICS_SCHEMA, "kv_fresh", "known", "canonical",
  ]);
  await assert.rejects(() => handleEmail(message(), legacyEnv(), {
    fetch: async () => { throw new Error("secret upstream"); },
  }), {
    message: "agent email relay temporarily unavailable",
  });
});

test("legacy managed delivery ignores unsigned pilot rows", async () => {
  const signedRoute = routeProjection(example.realm_label, {
    domain: example.domain,
    updated_at: new Date(vectorNowMS).toISOString(),
  });
  const unsignedConfig = {
    ...runtimeConfig(example, true),
    ingest_url: "https://attacker.example/v1/collect",
  };
  const unsignedRecipient = runtimeRecipient(example, first);
  const keys = [];
  const current = legacyEnv(null, {}, vectorNowMS);
  current.EMAIL_DIRECTORY.get = async (key, type) => {
    assert.equal(type, "json");
    keys.push(key);
    if (key === CONFIG_KEY) return unsignedConfig;
    if (key === recipientKey(first.address)) return unsignedRecipient;
    if (key === realmRouteKey(example.domain, example.realm_label)) return signedRoute;
    return null;
  };
  let relayed;
  const mail = message();
  await handleEmail(mail, current, {
    now: () => vectorNowMS,
    fetch: async (url) => {
      relayed = url;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });

  assert.deepEqual(mail.rejected, []);
  assert.equal(relayed, example.ingest_url);
  assert.deepEqual(keys, [realmRouteKey(example.domain, example.realm_label)]);
});

test("unsigned pilot rows cannot make a missing legacy canonical route reachable", async () => {
  const keys = [];
  let rawReads = 0;
  const current = coldRouteEnv(null, {
    EMAIL_DIRECTORY: {
      async get(key, type) {
        assert.equal(type, "json");
        keys.push(key);
        if (key === CONFIG_KEY) return runtimeConfig(example, true);
        if (key === recipientKey(first.address)) return runtimeRecipient(example, first);
        return null;
      },
    },
  });
  const mail = message();
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("unknown legacy route must not read raw MIME");
    },
  });
  await handleEmail(mail, current, {
    now: () => vectorNowMS,
    routeLookupState: createRouteLookupState(),
    fetch: async () => new Response(null, { status: 404 }),
  });

  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(rawReads, 0);
  assert.deepEqual(keys, [realmRouteKey(example.domain, example.realm_label)]);
});

test("legacy canonical delivery leaves agent authority to the cell and rejects oversized mail at the edge", async () => {
  let fetchCalls = 0;
  const unlistedAddress = `other.${example.realm_label}@${example.domain}`;
  const unlisted = message({ to: unlistedAddress });
  await handleEmail(unlisted, legacyEnv(), {
    fetch: async () => {
      fetchCalls++;
      return new Response('{"verdict":"unknown_recipient"}', { status: 404 });
    },
  });
  assert.deepEqual(unlisted.rejected, ["recipient unavailable"]);
  assert.equal(fetchCalls, 1);

  const oversized = message({
    rawSize: RELAY_MAXIMUM_RAW_BYTES + 1,
    raw: { must_not_be_read: true },
  });
  await handleEmail(oversized, legacyEnv(), {
    fetch: async () => { fetchCalls++; },
  });
  assert.deepEqual(oversized.rejected, ["message too large"]);
  assert.equal(fetchCalls, 1);
});

test("provider raw-size mismatch tempfails rather than accepting partial content", async () => {
  await assert.rejects(
    () => handleEmail(message({ rawSize: raw.byteLength + 1 }), legacyEnv(), { fetch: async () => new Response() }),
    { message: "agent email relay temporarily unavailable" },
  );
});

test("DMARC hard-fail rejection is dark when the exact flag is unset or false", async () => {
  for (const flag of [undefined, "false"]) {
    const { mail, rawMessage } = messageWithAuthenticationResults(trustedAuthservID, "fail");
    const extra = { AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID };
    if (flag !== undefined) extra.AGENT_EMAIL_DMARC_REJECT_ENABLED = flag;
    let relayedBody;
    await handleEmail(mail, legacyEnv(null, extra, vectorNowMS), {
      now: () => vectorNowMS,
      fetch: async (_url, init) => {
        relayedBody = Buffer.from(init.body);
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    });

    assert.deepEqual(mail.rejected, [], String(flag));
    assert.deepEqual(relayedBody, rawMessage, String(flag));
  }
});

test("trusted DMARC hard fail rejects before signing or cell relay", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const { mail } = messageWithAuthenticationResults(trustedAuthservID, "fail");
  let fetchCalls = 0;
  await handleEmail(
    mail,
    legacyEnv(metrics, {
      AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
      AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
    }, vectorNowMS),
    {
      now: () => vectorNowMS,
      fetch: async () => {
        fetchCalls++;
        throw new Error("DMARC rejection must not reach a cell");
      },
    },
  );

  assert.deepEqual(mail.rejected, [
    "message rejected by sender domain authentication policy",
  ]);
  const verdicts = verdictPoints(points);
  assert.equal(verdicts.length, 1);
  assert.deepEqual(verdicts[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_dmarc_fail", "authentication",
  ]);
  assert.equal(verdicts[0].doubles[3], 550);
  // Rejected before signing, so nothing reaches a cell, and the value-free
  // metric carries no authentication detail.
  assert.equal(fetchCalls, 0);
  assert.doesNotMatch(JSON.stringify(points), /@|address|account|realm_|agent_|dmarc=/i);
});

test("trusted DMARC pass relays normally when hard-fail rejection is enabled", async () => {
  const { mail, rawMessage } = messageWithAuthenticationResults(trustedAuthservID, "pass");
  let relayedBody;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vectorNowMS,
    fetch: async (_url, init) => {
      relayedBody = Buffer.from(init.body);
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });

  assert.deepEqual(mail.rejected, []);
  assert.deepEqual(relayedBody, rawMessage);
});

test("DMARC fail relays when no trusted authentication attester is configured", async () => {
  const { mail } = messageWithAuthenticationResults(trustedAuthservID, "fail");
  let fetchCalls = 0;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "",
    AGENT_EMAIL_RELAY_VERSION: "witself-email-relay-pilot-v1",
  }, vectorNowMS), {
    now: () => vectorNowMS,
    fetch: async () => {
      fetchCalls++;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });

  assert.deepEqual(mail.rejected, []);
  assert.equal(fetchCalls, 1);
});

test("forged DMARC fail from an untrusted attester relays normally", async () => {
  const { mail } = messageWithAuthenticationResults("attacker.example", "fail");
  let fetchCalls = 0;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vectorNowMS,
    fetch: async () => {
      fetchCalls++;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });

  assert.deepEqual(mail.rejected, []);
  assert.equal(fetchCalls, 1);
});

test("recipient and declared-size gates remain ahead of DMARC authentication", async () => {
  const authenticationEnabled = {
    AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  };

  let unknownRawReads = 0;
  let controlPlaneCalls = 0;
  const unknown = message();
  Object.defineProperty(unknown, "raw", {
    get() {
      unknownRawReads++;
      throw new Error("unknown recipient must not reach authentication");
    },
  });
  await handleEmail(unknown, coldRouteEnv(null, authenticationEnabled), {
    now: () => vectorNowMS,
    routeLookupState: createRouteLookupState(),
    fetch: async () => {
      controlPlaneCalls++;
      return new Response(null, { status: 404 });
    },
  });
  assert.deepEqual(unknown.rejected, ["recipient unavailable"]);
  assert.equal(unknownRawReads, 0);
  assert.equal(controlPlaneCalls, 1);

  let oversizedRawReads = 0;
  let relayCalls = 0;
  const oversized = message({ rawSize: RELAY_MAXIMUM_RAW_BYTES + 1 });
  Object.defineProperty(oversized, "raw", {
    get() {
      oversizedRawReads++;
      throw new Error("oversized message must not reach authentication");
    },
  });
  await handleEmail(
    oversized,
    legacyEnv(null, authenticationEnabled, vectorNowMS),
    {
      now: () => vectorNowMS,
      fetch: async () => { relayCalls++; },
    },
  );
  assert.deepEqual(oversized.rejected, ["message too large"]);
  assert.equal(oversizedRawReads, 0);
  assert.equal(relayCalls, 0);
});

test("edge metrics record value-free accepted, rejected, and tempfailed outcomes", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };

  await handleEmail(message(), legacyEnv(metrics, {}, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async () => new Response('{"verdict":"accepted"}', { status: 200 }),
  });

  const unknown = message({ to: `other.${example.realm_label}@${example.domain}` });
  await handleEmail(unknown, legacyEnv(metrics, {}, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async () => new Response('{"verdict":"unknown_recipient"}', { status: 404 }),
  });

  await assert.rejects(
    () => handleEmail(message(), legacyEnv(
      metrics,
      { REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false" },
      vectorNowMS,
    ), {
      now: () => vector.metadata.timestamp * 1000,
    }),
    { message: "agent email relay temporarily unavailable" },
  );

  await assert.rejects(
    () => handleEmail(message(), legacyEnv(metrics, {}, vectorNowMS), {
      now: () => vector.metadata.timestamp * 1000,
      fetch: async () => new Response('{"verdict":"receive_disabled"}', { status: 503 }),
    }),
    { message: "agent email relay temporarily unavailable" },
  );

  const verdicts = verdictPoints(points);
  assert.equal(verdicts.length, 4);
  assert.deepEqual(verdicts.map((point) => point.blobs[1]), [
    "accepted", "rejected_cell_permanent", "tempfail_canonical_gate", "tempfail_disabled",
  ]);
  assert.equal(verdicts.at(-1).blobs[2], "response");
  for (const point of verdicts) {
    assert.deepEqual(point.indexes, [point.blobs[1]]);
    assert.equal(point.blobs[0], EDGE_METRICS_SCHEMA);
    assert.equal(point.doubles[0], 1);
    const serialized = JSON.stringify(point);
    assert.doesNotMatch(serialized, /@|sha256|signature|realm_|agent_/i);
  }
});

test("edge metrics failures never alter the SMTP disposition", async () => {
  const metrics = { writeDataPoint() { throw new Error("analytics unavailable"); } };
  const mail = message();
  await handleEmail(mail, legacyEnv(metrics), {
    fetch: async () => new Response('{"verdict":"accepted"}', { status: 200 }),
  });
  assert.deepEqual(mail.rejected, []);
});

test("canonical and realm-alias projections converge on one cell route", async () => {
  for (const [realmLabel, address] of [
    [example.realm_label, canonicalAddress],
    [aliasLabel, aliasAddress],
  ]) {
    let relayed;
    const mail = message({ to: address });
    await handleEmail(mail, dynamicEnv({ [realmLabel]: routeProjection(realmLabel) }), {
      now: () => vectorNowMS,
      fetch: async (url, init) => {
        relayed = { url, init };
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    });
    assert.deepEqual(mail.rejected, []);
    assert.equal(relayed.url, example.ingest_url);
    assert.equal(relayed.init.headers.get("X-Witself-Email-Audience"), example.cell_audience);
    assert.equal(
      Buffer.from(relayed.init.headers.get("X-Witself-Email-Envelope-To"), "base64url").toString(),
      address,
    );
  }
});

test("fleet alias delivery gate is exact-true and default-off", async () => {
  for (const value of [undefined, "false", "TRUE", "1"]) {
    let fetched = false;
    const points = [];
    const mail = message({ to: aliasAddress });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [aliasLabel]: routeProjection(aliasLabel) },
          { writeDataPoint(point) { points.push(point); } },
          { REALM_EMAIL_ALIAS_DELIVERY_ENABLED: value },
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => { fetched = true; },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    const verdicts = verdictPoints(points);
    assert.equal(fetched, false);
    assert.deepEqual(mail.rejected, []);
    assert.equal(verdicts.length, 1);
    assert.deepEqual(verdicts[0].blobs, [
      EDGE_METRICS_SCHEMA, "tempfail_alias_gate", "route",
    ]);
  }

});

test("fleet canonical delivery gate is exact-true and independent", async () => {
  for (const value of [undefined, "false", "TRUE", "1"]) {
    let fetched = false;
    const points = [];
    const mail = message({ to: canonicalAddress });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [example.realm_label]: routeProjection(example.realm_label) },
          { writeDataPoint(point) { points.push(point); } },
          { REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: value },
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => { fetched = true; },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.equal(fetched, false);
    assert.deepEqual(mail.rejected, []);
    assert.deepEqual(verdictPoints(points)[0].blobs, [
      EDGE_METRICS_SCHEMA, "tempfail_canonical_gate", "route",
    ]);
  }

  let aliasRelayed = false;
  const alias = message({ to: aliasAddress });
  await handleEmail(
    alias,
    dynamicEnv(
      { [aliasLabel]: routeProjection(aliasLabel) },
      null,
      { REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false" },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => {
        aliasRelayed = true;
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    },
  );
  assert.equal(aliasRelayed, true);
  assert.deepEqual(alias.rejected, []);
});

test("legacy canonical routes obey the managed account cohort and canonical gate before content", async () => {
  for (const { extra, outcome } of [
    {
      extra: { AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "" },
      outcome: "tempfail_account_cohort",
    },
    {
      extra: { REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false" },
      outcome: "tempfail_canonical_gate",
    },
  ]) {
    const points = [];
    let rawReads = 0;
    let fetchCalls = 0;
    const mail = message();
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("held-back legacy mail must not read content");
      },
    });
    await assert.rejects(
      () => handleEmail(
        mail,
        legacyEnv(
          { writeDataPoint(point) { points.push(point); } },
          extra,
          vectorNowMS,
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => { fetchCalls++; },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(rawReads, 0);
    assert.equal(fetchCalls, 0);
    assert.deepEqual(verdictPoints(points).at(-1).blobs, [
      EDGE_METRICS_SCHEMA, outcome, "route",
    ]);
  }
});

test("legacy canonical labels cannot be authorized by a signed realm-alias route", async () => {
  let rawReads = 0;
  let fetchCalls = 0;
  const route = routeProjection(example.realm_label, {
    domain: example.domain,
    route_kind: "realm_alias",
  });
  const mail = message();
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("inconsistent legacy authority must not read content");
    },
  });
  await assert.rejects(
    () => handleEmail(
      mail,
      dynamicEnv(
        { [example.realm_label]: route },
        null,
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      ),
      {
        now: () => vectorNowMS,
        fetch: async () => {
          fetchCalls++;
          return new Response(null, { status: 404 });
        },
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
  assert.equal(rawReads, 0);
  assert.equal(fetchCalls, 1);
});

test("managed account cohort tempfails held-back canonical and alias routes before content", async () => {
  for (const [realmLabel, address, state] of [
    [example.realm_label, canonicalAddress, "applied"],
    [aliasLabel, aliasAddress, "applied"],
    [aliasLabel, aliasAddress, "retired"],
  ]) {
    let rawReads = 0;
    let fetchCalls = 0;
    const points = [];
    const mail = message({ to: address });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("held-back mail must not read content");
      },
    });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [realmLabel]: routeProjection(realmLabel, { state }) },
          { writeDataPoint(point) { points.push(point); } },
          { AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "" },
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => {
            fetchCalls++;
            throw new Error("held-back mail must not reach a cell");
          },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, [], `${realmLabel}:${state}`);
    assert.equal(rawReads, 0, `${realmLabel}:${state}`);
    assert.equal(fetchCalls, 0, `${realmLabel}:${state}`);
    assert.deepEqual(verdictPoints(points).at(-1).blobs, [
      EDGE_METRICS_SCHEMA, "tempfail_account_cohort", "route",
    ]);
    assert.doesNotMatch(
      JSON.stringify(points),
      /acc_aaaaaaaaaaaaaaaa|@|realm_|agent_/i,
    );
  }
});

test("legacy managed v1 evidence cannot deliver even when the new cohort admits the account", async () => {
  let rawReads = 0;
  let controlPlaneCalls = 0;
  const points = [];
  const mail = message({ to: aliasAddress });
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("legacy managed authority must not read content");
    },
  });
  await assert.rejects(
    () => handleEmail(
      mail,
      dynamicEnv(
        { [aliasLabel]: routeProjection(aliasLabel, { schema_version: 1 }) },
        { writeDataPoint(point) { points.push(point); } },
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      ),
      {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async () => {
          controlPlaneCalls++;
          return new Response("old control plane unavailable", { status: 503 });
        },
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(controlPlaneCalls, 1);
  assert.equal(rawReads, 0);
  assert.deepEqual(mail.rejected, []);
  assert.equal(verdictPoints(points).at(-1).blobs[1], "tempfail_route_lookup");
});

test("new edge with a v240 control plane tempfails legacy authority while dark", async () => {
  let rawReads = 0;
  let controlPlaneCalls = 0;
  const mail = message({ to: aliasAddress });
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("mixed-version route must not read content");
    },
  });
  await assert.rejects(
    () => handleEmail(
      mail,
      coldRouteEnv(null, {
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: rolloutAccount,
        REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
      }),
      {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async () => {
          controlPlaneCalls++;
          return Response.json(routeProjection(aliasLabel, { schema_version: 1 }));
        },
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(controlPlaneCalls, 1);
  assert.equal(rawReads, 0);
  assert.deepEqual(mail.rejected, []);
});

test("removed account makes stale managed v2 evidence retry without content or bounce", async () => {
  let rawReads = 0;
  let controlPlaneCalls = 0;
  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const mail = message({ to: aliasAddress });
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("removed account must not read content");
    },
  });
  await assert.rejects(
    () => handleEmail(
      mail,
      dynamicEnv(
        { [aliasLabel]: stale },
        null,
        {
          AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      ),
      {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async () => {
          controlPlaneCalls++;
          return Response.json({
            code: "managed_email_delivery_cohort_held_back",
          }, { status: 409 });
        },
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(controlPlaneCalls, 1);
  assert.equal(rawReads, 0);
  assert.deepEqual(mail.rejected, []);
});

test("invalid managed account cohorts fail closed as configuration errors", async () => {
  for (const value of [
    "*",
    "acc_aaaaaaaaaaaaaaaa ",
    "acc_bbbbbbbbbbbbbbbb,acc_aaaaaaaaaaaaaaaa",
  ]) {
    let rawReads = 0;
    let fetchCalls = 0;
    const points = [];
    const mail = message({ to: aliasAddress });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("invalid cohort must not read content");
      },
    });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [aliasLabel]: routeProjection(aliasLabel) },
          { writeDataPoint(point) { points.push(point); } },
          { AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: value },
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => { fetchCalls++; },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(rawReads, 0);
    assert.equal(fetchCalls, 0);
    assert.equal(verdictPoints(points).at(-1).blobs[1], "tempfail_configuration");
  }
});

test("control-plane held-back response stays temporary and cannot become unknown", async () => {
  let rawReads = 0;
  let controlPlaneCalls = 0;
  const mail = message({ to: aliasAddress });
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("held-back fallback must not read content");
    },
  });
  await assert.rejects(
    () => handleEmail(mail, coldRouteEnv(null, {
      AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: rolloutAccount,
    }), {
      now: () => vectorNowMS,
      routeLookupState: createRouteLookupState(),
      fetch: async () => {
        controlPlaneCalls++;
        return Response.json({
          code: "managed_email_delivery_cohort_held_back",
        }, { status: 409 });
      },
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(controlPlaneCalls, 1);
  assert.equal(rawReads, 0);
  assert.deepEqual(mail.rejected, []);
});

test("managed cohort has no authority over independently gated custom domains", async () => {
  let relayed = false;
  const mail = message({ to: customAddress });
  await handleEmail(
    mail,
    dynamicEnv(
      { [aliasLabel]: customRouteProjection() },
      null,
      {
        AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true",
        AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => {
        relayed = true;
        return Response.json({ verdict: "accepted" });
      },
    },
  );
  assert.equal(relayed, true);
  assert.deepEqual(mail.rejected, []);
});

test("custom-domain delivery gate is exact-true before lookup, limiters, and raw MIME", async () => {
  for (const value of [undefined, "false", "TRUE", "1"]) {
    const points = [];
    let directoryReads = 0;
    let limiterCalls = 0;
    let fetchCalls = 0;
    let rawReads = 0;
    const limiter = {
      async limit() {
        limiterCalls++;
        return { success: true };
      },
    };
    const currentEnv = dynamicEnv(
      { [aliasLabel]: customRouteProjection() },
      { writeDataPoint(point) { points.push(point); } },
      {
        AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: value,
        REALM_ROUTE_COLD_MISS_LIMITER: limiter,
        REALM_ROUTE_KNOWN_MISS_LIMITER: limiter,
        CONTROL_PLANE_URL: "https://control.example/",
        CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
      },
    );
    const get = currentEnv.EMAIL_DIRECTORY.get;
    currentEnv.EMAIL_DIRECTORY.get = async (...args) => {
      directoryReads++;
      return get(...args);
    };
    const mail = message({ to: customAddress });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("raw MIME must stay unread while the custom-domain gate is dark");
      },
    });

    await assert.rejects(
      () => handleEmail(mail, currentEnv, {
        now: () => vectorNowMS,
        fetch: async () => {
          fetchCalls++;
          throw new Error("no lookup or relay is allowed while the gate is dark");
        },
      }),
      { message: "agent email relay temporarily unavailable" },
    );

    assert.deepEqual(mail.rejected, []);
    assert.equal(directoryReads, 0);
    assert.equal(limiterCalls, 0);
    assert.equal(fetchCalls, 0);
    assert.equal(rawReads, 0);
    assert.equal(routeLookupPoints(points).length, 0);
    assert.deepEqual(verdictPoints(points)[0].blobs, [
      EDGE_METRICS_SCHEMA, "tempfail_custom_domain_gate", "route",
    ]);
  }
});

test("tampered KV and control-plane routes cannot read raw MIME or redirect delivery", async () => {
  const poisonedURL = "https://attacker.example/v1/collect";
  const signed = routeProjection(aliasLabel);
  const tampered = { ...signed, ingest_url: poisonedURL };

  for (const source of ["kv", "control_plane"]) {
    let rawReads = 0;
    let controlPlaneCalls = 0;
    let relayCalls = 0;
    const mail = message({ to: aliasAddress });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("untrusted route authority must not reach raw MIME");
      },
    });
    const currentEnv = source === "kv"
      ? dynamicEnv(
        { [aliasLabel]: tampered },
        null,
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      )
      : coldRouteEnv();

    await assert.rejects(
      () => handleEmail(mail, currentEnv, {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async (input) => {
          if (input instanceof Request) {
            controlPlaneCalls++;
            return source === "kv"
              ? new Response(null, { status: 404 })
              : Response.json(tampered);
          }
          relayCalls++;
          assert.notEqual(input, poisonedURL);
          return new Response('{"verdict":"accepted"}', { status: 200 });
        },
      }),
      { message: "agent email relay temporarily unavailable" },
      source,
    );

    assert.equal(controlPlaneCalls, 1, source);
    assert.equal(relayCalls, 0, source);
    assert.equal(rawReads, 0, source);
    assert.deepEqual(mail.rejected, [], source);
  }
});

test("exact custom-domain gate routes only a fresh custom-domain projection", async () => {
  const points = [];
  const keys = [];
  let relayed;
  const mail = message({ to: customAddress });
  const currentEnv = dynamicEnv(
    { [aliasLabel]: customRouteProjection() },
    { writeDataPoint(point) { points.push(point); } },
    { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true" },
  );
  const get = currentEnv.EMAIL_DIRECTORY.get;
  currentEnv.EMAIL_DIRECTORY.get = async (key, type) => {
    keys.push(key);
    return get(key, type);
  };
  await handleEmail(
    mail,
    currentEnv,
    {
      now: () => vectorNowMS,
      fetch: async (url, init) => {
        relayed = { url, init };
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    },
  );

  assert.deepEqual(mail.rejected, []);
  assert.deepEqual(keys, [realmRouteKey(customDomain, aliasLabel)]);
  assert.equal(relayed.url, example.ingest_url);
  assert.deepEqual(Buffer.from(relayed.init.body), raw);
  assert.equal(
    Buffer.from(
      relayed.init.headers.get("X-Witself-Email-Envelope-To"),
      "base64url",
    ).toString(),
    customAddress,
  );
  assert.deepEqual(routeLookupPoints(points)[0].blobs, [
    ROUTE_LOOKUP_METRICS_SCHEMA, "kv_fresh", "known", "custom_domain",
  ]);
  assert.equal(verdictPoints(points)[0].blobs[1], "accepted");
  assert.equal(JSON.stringify(points).includes(customDomain), false);
  assert.equal(JSON.stringify(points).includes(aliasLabel), false);
});

test("custom-domain cold miss accepts one fresh control-plane projection", async () => {
  const points = [];
  let controlPlaneCalls = 0;
  let relayCalls = 0;
  const mail = message({ to: customAddress });
  await handleEmail(
    mail,
    coldRouteEnv(
      { writeDataPoint(point) { points.push(point); } },
      { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true" },
    ),
    {
      now: () => vectorNowMS,
      fetch: async (input) => {
        if (input instanceof Request) {
          controlPlaneCalls++;
          return Response.json(customRouteProjection());
        }
        relayCalls++;
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    },
  );
  assert.deepEqual(mail.rejected, []);
  assert.equal(controlPlaneCalls, 1);
  assert.equal(relayCalls, 1);
  assert.deepEqual(routeLookupPoints(points)[0].blobs, [
    ROUTE_LOOKUP_METRICS_SCHEMA, "cp_found", "none", "custom_domain",
  ]);
});

test("custom-domain lookup revalidates control-plane route kind", async () => {
  const points = [];
  let controlPlaneCalls = 0;
  let rawReads = 0;
  const mail = message({ to: customAddress });
  Object.defineProperty(mail, "raw", {
    get() {
      rawReads++;
      throw new Error("wrong route kind must not read raw MIME");
    },
  });
  await assert.rejects(
    () => handleEmail(
      mail,
      coldRouteEnv(
        { writeDataPoint(point) { points.push(point); } },
        { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true" },
      ),
      {
        now: () => vectorNowMS,
        fetch: async (input) => {
          assert.equal(input instanceof Request, true);
          assert.equal(
            input.url,
            `https://control.example/v1/email/realm-routes/${customDomain}/${aliasLabel}`,
          );
          controlPlaneCalls++;
          return Response.json(routeProjection(aliasLabel, {
            domain: customDomain,
            route_kind: "realm_alias",
          }));
        },
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
  assert.equal(controlPlaneCalls, 1);
  assert.equal(rawReads, 0);
  assert.deepEqual(routeLookupPoints(points)[0].blobs, [
    ROUTE_LOOKUP_METRICS_SCHEMA, "cp_error", "none", "custom_domain",
  ]);
});

test("malformed, stale, suspended, and retired custom-domain projections fail closed", async () => {
  const cases = [
    {
      name: "tampered destination",
      route: (() => {
        const value = customRouteProjection();
        value.ingest_url = "https://attacker.example/ingest";
        return value;
      })(),
      transient: true,
      outcome: "tempfail_route_lookup",
    },
    {
      name: "malformed",
      route: (() => {
        const value = customRouteProjection();
        delete value.domain_request_id;
        return value;
      })(),
      transient: true,
      outcome: "tempfail_route_lookup",
    },
    {
      name: "wrong kind",
      route: routeProjection(aliasLabel, { domain: customDomain }),
      transient: true,
      outcome: "tempfail_route_lookup",
    },
    {
      name: "stale",
      route: customRouteProjection({
        updated_at: new Date(vectorNowMS - 300_001).toISOString(),
      }),
      transient: true,
      outcome: "tempfail_route_lookup",
    },
    {
      name: "suspended",
      route: customRouteProjection({
        state: "suspended",
        suspension_disposition: "retry",
      }),
      transient: true,
      outcome: "tempfail_suspended_route",
    },
    {
      name: "retired",
      route: customRouteProjection({ state: "retired" }),
      transient: false,
      outcome: "rejected_inactive_route",
    },
  ];

  for (const current of cases) {
    const points = [];
    let rawReads = 0;
    let fetchCalls = 0;
    const mail = message({ to: customAddress });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error(`${current.name} projection must not read raw MIME`);
      },
    });
    const operation = handleEmail(
      mail,
      dynamicEnv(
        { [aliasLabel]: current.route },
        { writeDataPoint(point) { points.push(point); } },
        { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true" },
      ),
      {
        now: () => vectorNowMS,
        fetch: async () => {
          fetchCalls++;
          throw new Error("cell must not be reached");
        },
      },
    );
    if (current.transient) {
      await assert.rejects(
        () => operation,
        { message: "agent email relay temporarily unavailable" },
        current.name,
      );
      assert.deepEqual(mail.rejected, [], current.name);
    } else {
      await operation;
      assert.deepEqual(mail.rejected, ["recipient unavailable"], current.name);
    }
    assert.equal(fetchCalls, 0, current.name);
    assert.equal(rawReads, 0, current.name);
    assert.equal(verdictPoints(points)[0].blobs[1], current.outcome, current.name);
  }
});

test("custom-domain feature-disabled cell receipt accepts and drops", async () => {
  const points = [];
  const mail = message({ to: customAddress });
  await handleEmail(
    mail,
    dynamicEnv(
      { [aliasLabel]: customRouteProjection() },
      { writeDataPoint(point) { points.push(point); } },
      { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: "true" },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => new Response('{"verdict":"feature_disabled"}', { status: 200 }),
    },
  );
  assert.deepEqual(mail.rejected, []);
  assert.deepEqual(verdictPoints(points)[0].blobs, [
    EDGE_METRICS_SCHEMA, "discarded_feature_disabled", "response",
  ]);
});

test("custom-domain gate does not alter managed canonical or alias delivery", async () => {
  for (const customGate of [undefined, "false", "TRUE", "true"]) {
    for (const [realmLabel, address] of [
      [example.realm_label, canonicalAddress],
      [aliasLabel, aliasAddress],
    ]) {
      let relays = 0;
      const mail = message({ to: address });
      await handleEmail(
        mail,
        dynamicEnv(
          { [realmLabel]: routeProjection(realmLabel) },
          null,
          { AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED: customGate },
        ),
        {
          now: () => vectorNowMS,
          fetch: async () => {
            relays++;
            return new Response('{"verdict":"accepted"}', { status: 200 });
          },
        },
      );
      assert.deepEqual(mail.rejected, []);
      assert.equal(relays, 1);
    }
  }
});

test("dynamic routing keeps account-plan enforcement in the signed cell verdict", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message({ to: canonicalAddress });
  await handleEmail(
    mail,
    dynamicEnv({ [example.realm_label]: routeProjection(example.realm_label) }, metrics),
    {
      now: () => vectorNowMS,
      fetch: async () => new Response('{"verdict":"feature_disabled"}', { status: 200 }),
    },
  );
  assert.deepEqual(mail.rejected, []);
  assert.deepEqual(verdictPoints(points)[0].blobs, [
    EDGE_METRICS_SCHEMA, "discarded_feature_disabled", "response",
  ]);
});

test("suspended and retired dynamic routes fail closed without reaching a cell", async () => {
  for (const [state, expectedOutcome, rejects] of [
    ["suspended", "tempfail_suspended_route", []],
    ["retired", "rejected_inactive_route", ["recipient unavailable"]],
  ]) {
    const value = routeProjection(aliasLabel, {
      state,
      ...(state === "suspended" ? { suspension_disposition: "retry" } : {}),
    });
    const points = [];
    let fetched = false;
    const mail = message({ to: aliasAddress });
    const operation = handleEmail(
      mail,
      dynamicEnv({ [aliasLabel]: value }, { writeDataPoint(point) { points.push(point); } }),
      { now: () => vectorNowMS, fetch: async () => { fetched = true; } },
    );
    if (state === "suspended") {
      await assert.rejects(operation, { message: "agent email relay temporarily unavailable" });
    } else {
      await operation;
    }
    const verdicts = verdictPoints(points);
    assert.equal(fetched, false);
    assert.deepEqual(mail.rejected, rejects);
    assert.equal(verdicts.length, 1);
    assert.equal(verdicts[0].blobs[1], expectedOutcome);
    assert.equal(verdicts[0].blobs[2], "route");
    assert.doesNotMatch(
      JSON.stringify(points),
      /@|address|account|realm_|agent_|subject|digest|signature/i,
    );
  }
});

test("plan-inactive suspended routes reject without an unbounded SMTP retry loop", async () => {
  const value = routeProjection(aliasLabel, {
    state: "suspended",
    suspension_disposition: "inactive",
  });
  const points = [];
  const mail = message({ to: aliasAddress });
  await handleEmail(
    mail,
    dynamicEnv(
      { [aliasLabel]: value },
      { writeDataPoint(point) { points.push(point); } },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => { throw new Error("must not relay"); },
    },
  );
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(verdictPoints(points)[0].blobs[1], "rejected_inactive_route");
});

test("malformed dynamic addresses reject before directory lookup or relay", async () => {
  for (const address of [
    `alpha.bad--alias@${primaryDomain}`,
    `alpha.xn--acme@${primaryDomain}`,
    `alpha.one.two@${primaryDomain}`,
    `alpha.acme_west@${primaryDomain}`,
  ]) {
    let reads = 0;
    let limiterCalls = 0;
    let fetched = false;
    const limiter = { async limit() { limiterCalls++; return { success: true }; } };
    const currentEnv = dynamicEnv({}, null, {
      REALM_ROUTE_COLD_MISS_LIMITER: limiter,
      REALM_ROUTE_KNOWN_MISS_LIMITER: limiter,
    });
    currentEnv.EMAIL_DIRECTORY.get = async () => { reads++; return null; };
    const mail = message({ to: address });
    await handleEmail(mail, currentEnv, { fetch: async () => { fetched = true; } });
    assert.deepEqual(mail.rejected, ["recipient unavailable"]);
    assert.equal(reads, 0);
    assert.equal(limiterCalls, 0);
    assert.equal(fetched, false);
  }
});

test("unmanaged domain tempfails behind the absent custom gate before lookup", async () => {
  let reads = 0;
  let limiterCalls = 0;
  let fetched = false;
  const limiter = { async limit() { limiterCalls++; return { success: true }; } };
  const currentEnv = dynamicEnv({}, null, {
    REALM_ROUTE_COLD_MISS_LIMITER: limiter,
    REALM_ROUTE_KNOWN_MISS_LIMITER: limiter,
  });
  currentEnv.EMAIL_DIRECTORY.get = async () => { reads++; return null; };
  const mail = message({
    to: `alpha.${example.realm_label}@unmanaged.example`,
  });
  await assert.rejects(
    () => handleEmail(mail, currentEnv, {
      fetch: async () => { fetched = true; },
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
  assert.equal(reads, 0);
  assert.equal(limiterCalls, 0);
  assert.equal(fetched, false);
});

test("permanent and exact legacy routes preserve the signed envelope domain", async () => {
  for (const { domain, address, currentEnv } of [
    {
      domain: primaryDomain,
      address: canonicalAddress,
      currentEnv: (() => {
        const value = dynamicEnv({});
        const route = routeProjection(example.realm_label, {
          domain: primaryDomain,
        });
        value.EMAIL_DIRECTORY.get = async (key, type) => {
          assert.equal(type, "json");
          return key === realmRouteKey(primaryDomain, example.realm_label)
            ? route
            : null;
        };
        return value;
      })(),
    },
    {
      domain: example.domain,
      address: first.address,
      currentEnv: legacyEnv(null, {}, vectorNowMS),
    },
  ]) {
    let relayed;
    const mail = message({ to: address });
    await handleEmail(mail, currentEnv, {
      now: () => vectorNowMS,
      fetch: async (url, init) => {
        relayed = { url, init };
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    });
    assert.deepEqual(mail.rejected, []);
    assert.equal(relayed.url, example.ingest_url);
    assert.equal(
      Buffer.from(
        relayed.init.headers.get("X-Witself-Email-Envelope-To"),
        "base64url",
      ).toString(),
      address,
    );
    assert.equal(address.endsWith(`@${domain}`), true);
  }
});

test("legacy domain rejects realm aliases before KV or control-plane lookup", async () => {
  const currentEnv = legacyEnv(null, {}, vectorNowMS);
  currentEnv.CONTROL_PLANE_URL = "https://control.example/";
  currentEnv.CONTROL_PLANE_EDGE_TOKEN = "edge-token-1234567890";
  const keys = [];
  currentEnv.EMAIL_DIRECTORY.get = async (key) => {
    keys.push(key);
    return null;
  };
  let fetched = false;
  const mail = message({
    to: `alpha.${aliasLabel}@${example.domain}`,
  });
  await handleEmail(mail, currentEnv, {
    now: () => vectorNowMS,
    fetch: async () => { fetched = true; },
  });
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.deepEqual(keys, []);
  assert.equal(fetched, false);
});

test("10,000 rotating valid labels share one fixed cold-miss budget", async () => {
  const state = createRouteLookupState();
  const limiterKeys = [];
  let controlPlaneCalls = 0;
  let rawReads = 0;
  const currentEnv = coldRouteEnv(null, {
    REALM_ROUTE_COLD_MISS_LIMITER: {
      async limit({ key }) {
        limiterKeys.push(key);
        return { success: true };
      },
    },
  });

  for (let index = 0; index < 10_000; index++) {
    const label = `r${index.toString(36).padStart(3, "0")}`;
    const mail = message({ to: `alpha.${label}@${primaryDomain}` });
    Object.defineProperty(mail, "raw", {
      get() {
        rawReads++;
        throw new Error("raw message must not be read during route admission");
      },
    });
    try {
      await handleEmail(mail, currentEnv, {
        now: () => vectorNowMS,
        routeLookupState: state,
        fetch: async (input) => {
          assert.equal(input instanceof Request, true);
          controlPlaneCalls++;
          return new Response(null, { status: 404 });
        },
      });
    } catch (error) {
      assert.equal(error.message, "agent email relay temporarily unavailable");
    }
  }

  assert.equal(controlPlaneCalls, 10);
  assert.equal(limiterKeys.length, 10);
  assert.deepEqual(new Set(limiterKeys), new Set(["cold-miss-v1"]));
  assert.equal(rawReads, 0);
  assert.equal(state.suppressed.size, 10);
  assert.equal(state.inflight.size, 0);
});

test("strict local cold-miss budget rolls over only after ten seconds", async () => {
  const state = createRouteLookupState();
  let nowMS = vectorNowMS;
  let limiterCalls = 0;
  let controlPlaneCalls = 0;
  const currentEnv = coldRouteEnv(null, {
    REALM_ROUTE_COLD_MISS_LIMITER: {
      async limit({ key }) {
        assert.equal(key, "cold-miss-v1");
        limiterCalls++;
        return { success: true };
      },
    },
  });
  const attempt = async (index) => {
    const label = `w${index.toString(36).padStart(2, "0")}`;
    const mail = message({ to: `alpha.${label}@${primaryDomain}` });
    try {
      await handleEmail(mail, currentEnv, {
        now: () => nowMS,
        routeLookupState: state,
        fetch: async () => {
          controlPlaneCalls++;
          return new Response(null, { status: 404 });
        },
      });
    } catch (error) {
      assert.equal(error.message, "agent email relay temporarily unavailable");
    }
    return mail;
  };

  for (let index = 0; index < 10; index++) {
    assert.deepEqual((await attempt(index)).rejected, ["recipient unavailable"]);
  }
  assert.deepEqual((await attempt(10)).rejected, []);
  nowMS += 9_999;
  assert.deepEqual((await attempt(11)).rejected, []);
  assert.equal(controlPlaneCalls, 10);
  assert.equal(limiterCalls, 10);

  nowMS += 1;
  assert.deepEqual((await attempt(12)).rejected, ["recipient unavailable"]);
  assert.equal(controlPlaneCalls, 11);
  assert.equal(limiterCalls, 11);
});

test("same-label cold misses are suppressed and concurrent lookups singleflight", async () => {
  const state = createRouteLookupState();
  const points = [];
  let limiterCalls = 0;
  let controlPlaneCalls = 0;
  let releaseLookup;
  const lookupReleased = new Promise((resolve) => { releaseLookup = resolve; });
  let enterLookup;
  const lookupEntered = new Promise((resolve) => { enterLookup = resolve; });
  const currentEnv = coldRouteEnv(
    { writeDataPoint(point) { points.push(point); } },
    {
      REALM_ROUTE_COLD_MISS_LIMITER: {
        async limit({ key }) {
          assert.equal(key, "cold-miss-v1");
          limiterCalls++;
          return { success: true };
        },
      },
    },
  );
  const runtime = {
    now: () => vectorNowMS,
    routeLookupState: state,
    fetch: async (input) => {
      assert.equal(input instanceof Request, true);
      controlPlaneCalls++;
      enterLookup();
      await lookupReleased;
      return new Response(null, { status: 404 });
    },
  };

  const concurrent = Array.from({ length: 32 }, () => {
    const mail = message({ to: aliasAddress });
    return handleEmail(mail, currentEnv, runtime).then(
      () => ({ mail, error: null }),
      (error) => ({ mail, error }),
    );
  });
  await lookupEntered;
  assert.equal(controlPlaneCalls, 1);
  assert.equal(limiterCalls, 1);
  releaseLookup();
  const completed = await Promise.all(concurrent);
  let permanent = 0;
  let transient = 0;
  for (const { mail, error } of completed) {
    if (error) {
      assert.equal(error.message, "agent email relay temporarily unavailable");
      assert.deepEqual(mail.rejected, []);
      transient++;
    } else {
      assert.deepEqual(mail.rejected, ["recipient unavailable"]);
      permanent++;
    }
  }
  assert.equal(permanent, 1);
  assert.equal(transient, 31);
  assert.equal(state.suppressed.size, 1);
  assert.equal(state.inflight.size, 0);

  const retry = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(retry, currentEnv, runtime),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(retry.rejected, []);
  assert.equal(controlPlaneCalls, 1);
  assert.equal(limiterCalls, 1);
  const results = routeLookupPoints(points).map((point) => point.blobs[1]);
  assert.equal(results.filter((result) => result === "cp_not_found").length, 1);
  assert.equal(results.filter((result) => result === "miss_suppressed").length, 32);
});

test("singleflight followers share a found projection without extra control-plane calls", async () => {
  const state = createRouteLookupState();
  const foundLabel = "found-route";
  const foundAddress = `alpha.${foundLabel}@${primaryDomain}`;
  let limiterCalls = 0;
  let controlPlaneCalls = 0;
  let relayCalls = 0;
  let releaseLookup;
  const lookupReleased = new Promise((resolve) => { releaseLookup = resolve; });
  let allJoined;
  const joined = new Promise((resolve) => { allJoined = resolve; });
  let followerCount = 0;
  const currentEnv = coldRouteEnv(
    {
      writeDataPoint(point) {
        if (
          point.blobs[0] === ROUTE_LOOKUP_METRICS_SCHEMA &&
          point.blobs[1] === "miss_suppressed" &&
          ++followerCount === 15
        ) {
          allJoined();
        }
      },
    },
    {
      REALM_ROUTE_COLD_MISS_LIMITER: {
        async limit() {
          limiterCalls++;
          return { success: true };
        },
      },
    },
  );
  const runtime = {
    now: () => vectorNowMS,
    routeLookupState: state,
    fetch: async (input) => {
      if (input instanceof Request) {
        controlPlaneCalls++;
        await lookupReleased;
        return Response.json(routeProjection(foundLabel));
      }
      relayCalls++;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  };

  const mails = Array.from({ length: 16 }, () => message({ to: foundAddress }));
  const pending = mails.map((mail) => handleEmail(mail, currentEnv, runtime));
  await joined;
  assert.equal(controlPlaneCalls, 1);
  assert.equal(limiterCalls, 1);
  releaseLookup();
  await Promise.all(pending);
  assert.equal(relayCalls, 16);
  assert.equal(mails.every((mail) => mail.rejected.length === 0), true);
});

test("cold-miss suppression expires after its fixed ten-second window", async () => {
  const state = createRouteLookupState();
  let nowMS = vectorNowMS;
  let controlPlaneCalls = 0;
  const currentEnv = coldRouteEnv();
  const runtime = {
    now: () => nowMS,
    routeLookupState: state,
    fetch: async () => {
      controlPlaneCalls++;
      return new Response(null, { status: 404 });
    },
  };

  const first = message({ to: aliasAddress });
  await handleEmail(first, currentEnv, runtime);
  assert.deepEqual(first.rejected, ["recipient unavailable"]);

  nowMS += 9_999;
  const suppressed = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(suppressed, currentEnv, runtime),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(suppressed.rejected, []);
  assert.equal(controlPlaneCalls, 1);

  nowMS += 1;
  const expired = message({ to: aliasAddress });
  await handleEmail(expired, currentEnv, runtime);
  assert.deepEqual(expired.rejected, ["recipient unavailable"]);
  assert.equal(controlPlaneCalls, 2);
});

test("cold-miss suppression stores only bounded SHA-256 markers", async () => {
  const state = createRouteLookupState();
  for (let index = 0; index < 1_024; index++) {
    state.suppressed.set(index.toString(16).padStart(64, "0"), vectorNowMS + 5_000);
  }
  const mail = message({ to: aliasAddress });
  await handleEmail(mail, coldRouteEnv(), {
    now: () => vectorNowMS,
    routeLookupState: state,
    fetch: async () => new Response(null, { status: 404 }),
  });
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(state.suppressed.size, 1_024);
  assert.equal(state.inflight.size, 0);
  for (const [key, expiresAt] of state.suppressed) {
    assert.match(key, /^[0-9a-f]{64}$/);
    assert.equal(typeof expiresAt, "number");
    assert.equal(key.includes(aliasLabel), false);
    assert.equal(key.includes(primaryDomain), false);
  }
});

test("a positive KV projection always beats an existing cold-miss marker", async () => {
  const state = createRouteLookupState();
  const routes = new Map();
  let limiterCalls = 0;
  let controlPlaneCalls = 0;
  let relays = 0;
  const currentEnv = coldRouteEnv(null, {
    REALM_ROUTE_COLD_MISS_LIMITER: {
      async limit() {
        limiterCalls++;
        return { success: true };
      },
    },
  });
  currentEnv.EMAIL_DIRECTORY.get = async (key, type) => {
    assert.equal(type, "json");
    return routes.get(key) ?? null;
  };
  const runtime = {
    now: () => vectorNowMS,
    routeLookupState: state,
    fetch: async (input) => {
      if (input instanceof Request) {
        controlPlaneCalls++;
        return new Response(null, { status: 404 });
      }
      relays++;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  };

  const firstMiss = message({ to: aliasAddress });
  await handleEmail(firstMiss, currentEnv, runtime);
  assert.deepEqual(firstMiss.rejected, ["recipient unavailable"]);
  assert.equal(state.suppressed.size, 1);

  routes.set(realmRouteKey(primaryDomain, aliasLabel), routeProjection(aliasLabel));
  const projected = message({ to: aliasAddress });
  await handleEmail(projected, currentEnv, runtime);
  assert.deepEqual(projected.rejected, []);
  assert.equal(controlPlaneCalls, 1);
  assert.equal(limiterCalls, 1);
  assert.equal(relays, 1);
});

test("cold and known lookup admission fail safely before reading content", async () => {
  const missingBindings = [
    {},
    { REALM_ROUTE_COLD_MISS_LIMITER: { async limit() { throw new Error("down"); } } },
    { REALM_ROUTE_COLD_MISS_LIMITER: { async limit() { return { success: false }; } } },
    { REALM_ROUTE_COLD_MISS_LIMITER: { async limit() { return {}; } } },
  ];
  for (const bindings of missingBindings) {
    let rawReads = 0;
    let fetches = 0;
    const mail = message({ to: aliasAddress });
    Object.defineProperty(mail, "raw", { get() { rawReads++; return null; } });
    const currentEnv = coldRouteEnv(null, {
      REALM_ROUTE_COLD_MISS_LIMITER: undefined,
      ...bindings,
    });
    await assert.rejects(
      () => handleEmail(mail, currentEnv, {
        routeLookupState: createRouteLookupState(),
        fetch: async () => { fetches++; return new Response(null, { status: 404 }); },
      }),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(rawReads, 0);
    assert.equal(fetches, 0);
  }

  for (const limiter of [
    undefined,
    { async limit() { throw new Error("down"); } },
    { async limit() { return { success: false }; } },
  ]) {
    let rawReads = 0;
    let fetches = 0;
    const stale = routeProjection(aliasLabel, {
      updated_at: new Date(vectorNowMS - 301_000).toISOString(),
    });
    const mail = message({ to: aliasAddress });
    Object.defineProperty(mail, "raw", { get() { rawReads++; return null; } });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [aliasLabel]: stale },
          null,
          {
            CONTROL_PLANE_URL: "https://control.example/",
            CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
            REALM_ROUTE_KNOWN_MISS_LIMITER: limiter,
          },
        ),
        {
          now: () => vectorNowMS,
          routeLookupState: createRouteLookupState(),
          fetch: async () => { fetches++; return Response.json(routeProjection(aliasLabel)); },
        },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(rawReads, 0);
    assert.equal(fetches, 0);
  }
});

test("hash and local singleflight-capacity failures also stop before admission", async () => {
  const saturatedState = createRouteLookupState();
  saturatedState.inflight = new Map(
    Array.from({ length: 1_024 }, (_, index) => [`occupied-${index}`, new Promise(() => {})]),
  );
  for (const state of [
    createRouteLookupState(),
    saturatedState,
  ]) {
    let limiterCalls = 0;
    let fetches = 0;
    let rawReads = 0;
    const mail = message({ to: aliasAddress });
    Object.defineProperty(mail, "raw", { get() { rawReads++; return null; } });
    const runtime = {
      routeLookupState: state,
      fetch: async () => { fetches++; return new Response(null, { status: 404 }); },
    };
    if (state.inflight.size === 0) {
      runtime.crypto = { subtle: { async digest() { throw new Error("unavailable"); } } };
    }
    await assert.rejects(
      () => handleEmail(
        mail,
        coldRouteEnv(null, {
          REALM_ROUTE_COLD_MISS_LIMITER: {
            async limit() {
              limiterCalls++;
              return { success: true };
            },
          },
        }),
        runtime,
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(limiterCalls, 0);
    assert.equal(fetches, 0);
    assert.equal(rawReads, 0);
  }
});

test("known-route refreshes use one fixed higher-capacity limiter key", async () => {
  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const keys = [];
  let relayed = false;
  const mail = message({ to: aliasAddress });
  await handleEmail(
    mail,
    dynamicEnv(
      { [aliasLabel]: stale },
      null,
      {
        CONTROL_PLANE_URL: "https://control.example/",
        CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        REALM_ROUTE_KNOWN_MISS_LIMITER: {
          async limit({ key }) {
            keys.push(key);
            return { success: true };
          },
        },
      },
    ),
    {
      now: () => vectorNowMS,
      fetch: async (input) => {
        if (input instanceof Request) {
          return Response.json(routeProjection(aliasLabel, { controller_revision: 8 }));
        }
        relayed = true;
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    },
  );
  assert.deepEqual(keys, ["known-miss-v1"]);
  assert.equal(relayed, true);
  assert.deepEqual(mail.rejected, []);
});

test("strict local known-route budget admits 100 lookups per ten-second window", async () => {
  const state = createRouteLookupState();
  let nowMS = vectorNowMS;
  let limiterCalls = 0;
  let controlPlaneCalls = 0;
  const routes = {};
  for (let index = 0; index < 103; index++) {
    const label = `k${index.toString(36).padStart(2, "0")}`;
    routes[label] = routeProjection(label, {
      updated_at: new Date(vectorNowMS - 301_000).toISOString(),
    });
  }
  const currentEnv = dynamicEnv(routes, null, {
    CONTROL_PLANE_URL: "https://control.example/",
    CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
    REALM_ROUTE_KNOWN_MISS_LIMITER: {
      async limit({ key }) {
        assert.equal(key, "known-miss-v1");
        limiterCalls++;
        return { success: true };
      },
    },
  });
  const attempt = async (index) => {
    const label = `k${index.toString(36).padStart(2, "0")}`;
    const mail = message({ to: `alpha.${label}@${primaryDomain}` });
    await assert.rejects(
      () => handleEmail(mail, currentEnv, {
        now: () => nowMS,
        routeLookupState: state,
        fetch: async () => {
          controlPlaneCalls++;
          return new Response(null, { status: 404 });
        },
      }),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
  };

  for (let index = 0; index < 101; index++) await attempt(index);
  assert.equal(controlPlaneCalls, 100);
  assert.equal(limiterCalls, 100);
  nowMS += 10_000;
  await attempt(101);
  assert.equal(controlPlaneCalls, 101);
  assert.equal(limiterCalls, 101);
});

test("exhausting the local cold lane does not consume the known lane", async () => {
  const state = createRouteLookupState();
  let coldBindingCalls = 0;
  let knownBindingCalls = 0;
  let controlPlaneCalls = 0;
  const shared = {
    CONTROL_PLANE_URL: "https://control.example/",
    CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
    REALM_ROUTE_COLD_MISS_LIMITER: {
      async limit() {
        coldBindingCalls++;
        return { success: true };
      },
    },
    REALM_ROUTE_KNOWN_MISS_LIMITER: {
      async limit() {
        knownBindingCalls++;
        return { success: true };
      },
    },
  };
  const runtime = {
    now: () => vectorNowMS,
    routeLookupState: state,
    fetch: async () => {
      controlPlaneCalls++;
      return new Response(null, { status: 404 });
    },
  };

  for (let index = 0; index < 11; index++) {
    const label = `z${index.toString(36).padStart(2, "0")}`;
    const mail = message({ to: `alpha.${label}@${primaryDomain}` });
    try {
      await handleEmail(mail, coldRouteEnv(null, shared), runtime);
    } catch (error) {
      assert.equal(error.message, "agent email relay temporarily unavailable");
    }
  }
  assert.equal(coldBindingCalls, 10);
  assert.equal(controlPlaneCalls, 10);

  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const knownMail = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(knownMail, dynamicEnv({ [aliasLabel]: stale }, null, shared), runtime),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(knownBindingCalls, 1);
  assert.equal(controlPlaneCalls, 11);
});

test("KV uncertainty and corrupt or stale evidence can never turn a 404 into a bounce", async () => {
  const cases = [
    {
      setup(currentEnv) {
        currentEnv.EMAIL_DIRECTORY.get = async () => { throw new Error("KV unavailable"); };
      },
    },
    {
      routes: {
        [aliasLabel]: routeProjection(aliasLabel, {
          updated_at: new Date(vectorNowMS - 301_000).toISOString(),
        }),
      },
    },
    {
      routes: {
        [aliasLabel]: routeProjection(aliasLabel, { realm_label: "other-realm" }),
      },
    },
  ];
  for (const currentCase of cases) {
    const currentEnv = dynamicEnv(currentCase.routes ?? {}, null, {
      CONTROL_PLANE_URL: "https://control.example/",
      CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
    });
    currentCase.setup?.(currentEnv);
    const mail = message({ to: aliasAddress });
    await assert.rejects(
      () => handleEmail(mail, currentEnv, {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async () => new Response(null, { status: 404 }),
      }),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
  }
});

test("uncertain KV fallbacks emit exactly one terminal route event", async () => {
  for (const source of ["read_error", "corrupt_projection"]) {
    for (const found of [true, false]) {
      const points = [];
      const currentEnv = dynamicEnv(
        source === "corrupt_projection"
          ? { [aliasLabel]: routeProjection(aliasLabel, { realm_label: "other-realm" }) }
          : {},
        { writeDataPoint(point) { points.push(point); } },
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      );
      if (source === "read_error") {
        currentEnv.EMAIL_DIRECTORY.get = async () => { throw new Error("KV unavailable"); };
      }
      const mail = message({ to: aliasAddress });
      const operation = handleEmail(mail, currentEnv, {
        now: () => vectorNowMS,
        routeLookupState: createRouteLookupState(),
        fetch: async (input) => {
          if (input instanceof Request) {
            return found
              ? Response.json(routeProjection(aliasLabel))
              : new Response(null, { status: 404 });
          }
          return new Response('{"verdict":"accepted"}', { status: 200 });
        },
      });
      if (found) {
        await operation;
        assert.deepEqual(mail.rejected, []);
      } else {
        await assert.rejects(operation, {
          message: "agent email relay temporarily unavailable",
        });
        assert.deepEqual(mail.rejected, []);
      }
      const lookups = routeLookupPoints(points);
      assert.equal(lookups.length, 1);
      assert.equal(lookups[0].blobs[1], found ? "cp_found" : "cp_error");
      assert.equal(lookups[0].blobs[2], "uncertain");
      assert.equal(lookups[0].doubles[2], found ? 200 : 404);
    }
  }
});

test("route lookup metrics are low-cardinality, value-free, and best effort", async () => {
  const points = [];
  const state = createRouteLookupState();
  const mail = message({ to: aliasAddress });
  await handleEmail(
    mail,
    coldRouteEnv({ writeDataPoint(point) { points.push(point); } }),
    {
      now: () => vectorNowMS,
      routeLookupState: state,
      fetch: async () => new Response(null, { status: 404 }),
    },
  );
  const lookup = routeLookupPoints(points);
  assert.equal(lookup.length, 1);
  assert.deepEqual(lookup[0], {
    indexes: ["cp_not_found"],
    blobs: [ROUTE_LOOKUP_METRICS_SCHEMA, "cp_not_found", "none", "unknown"],
    doubles: [1, 0, 404],
  });
  const serialized = JSON.stringify(lookup);
  assert.equal(serialized.includes(aliasAddress), false);
  assert.equal(serialized.includes(aliasLabel), false);
  assert.equal(serialized.includes(primaryDomain), false);
  assert.equal(serialized.includes(example.realm_id), false);

  const projected = message({ to: aliasAddress });
  await handleEmail(
    projected,
    dynamicEnv(
      { [aliasLabel]: routeProjection(aliasLabel) },
      { writeDataPoint() { throw new Error("analytics unavailable"); } },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => new Response('{"verdict":"accepted"}', { status: 200 }),
    },
  );
  assert.deepEqual(projected.rejected, []);
});

test("stale KV route refreshes from the control plane before relaying", async () => {
  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const refreshed = routeProjection(aliasLabel, { controller_revision: 8 });
  const calls = [];
  const currentEnv = dynamicEnv(
    { [aliasLabel]: stale },
    null,
    {
      CONTROL_PLANE_URL: "https://control.example/",
      CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
    },
  );
  const mail = message({ to: aliasAddress });
  await handleEmail(mail, currentEnv, {
    now: () => vectorNowMS,
    fetch: async (input, init) => {
      if (input instanceof Request) {
        calls.push({ kind: "control", input });
        return Response.json(refreshed);
      }
      calls.push({ kind: "relay", input, init });
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.equal(calls.length, 2);
  assert.equal(
    calls[0].input.url,
    `https://control.example/v1/email/realm-routes/${primaryDomain}/${aliasLabel}`,
  );
  assert.equal(calls[0].input.headers.get("Authorization"), "Bearer edge-token-1234567890");
  assert.equal(calls[1].input, example.ingest_url);
  assert.deepEqual(mail.rejected, []);
});

test("stale routes never downgrade or turn control-plane failure into a bounce", async () => {
  const stale = routeProjection(aliasLabel, {
    controller_revision: 7,
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  for (const response of [
    () => Response.json(routeProjection(aliasLabel, { controller_revision: 6 })),
    () => new Response("unavailable", { status: 503 }),
  ]) {
    const points = [];
    const mail = message({ to: aliasAddress });
    await assert.rejects(
      () => handleEmail(
        mail,
        dynamicEnv(
          { [aliasLabel]: stale },
          { writeDataPoint(point) { points.push(point); } },
          {
            CONTROL_PLANE_URL: "https://control.example/",
            CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
          },
        ),
        { now: () => vectorNowMS, fetch: async () => response() },
      ),
      { message: "agent email relay temporarily unavailable" },
    );
    assert.deepEqual(mail.rejected, []);
    assert.equal(verdictPoints(points)[0].blobs[1], "tempfail_route_lookup");
    assert.equal(verdictPoints(points)[0].blobs[2], "route");
  }
});

test("a stale known route plus control-plane 404 tempfails instead of bouncing", async () => {
  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const mail = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(
      mail,
      dynamicEnv(
        { [aliasLabel]: stale },
        null,
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      ),
      {
        now: () => vectorNowMS,
        fetch: async () => new Response(null, { status: 404 }),
      },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
});

test("directory read failure plus control-plane 404 remains transient for legacy routes", async () => {
  const current = legacyEnv(null, {}, vectorNowMS);
  current.CONTROL_PLANE_URL = "https://control.example/";
  current.CONTROL_PLANE_EDGE_TOKEN = "edge-token-1234567890";
  current.EMAIL_DIRECTORY.get = async () => {
    throw new Error("temporary KV failure");
  };
  const mail = message({ to: first.address });
  await assert.rejects(
    () => handleEmail(mail, current, {
      now: () => vectorNowMS,
      fetch: async () => new Response(null, { status: 404 }),
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
});

test("authoritative control-plane 404 rejects only when no prior route evidence exists", async () => {
  const stale = routeProjection(aliasLabel, {
    updated_at: new Date(vectorNowMS - 301_000).toISOString(),
  });
  const unknown = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(
      unknown,
      dynamicEnv(
        { [aliasLabel]: stale },
        null,
        {
          CONTROL_PLANE_URL: "https://control.example/",
          CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
        },
      ),
      { now: () => vectorNowMS, fetch: async () => new Response(null, { status: 404 }) },
    ),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(unknown.rejected, []);

  const neverKnown = message({ to: aliasAddress });
  await handleEmail(
    neverKnown,
    dynamicEnv(
      {},
      null,
      {
        CONTROL_PLANE_URL: "https://control.example/",
        CONTROL_PLANE_EDGE_TOKEN: "edge-token-1234567890",
      },
    ),
    { now: () => vectorNowMS, fetch: async () => new Response(null, { status: 404 }) },
  );
  assert.deepEqual(neverKnown.rejected, ["recipient unavailable"]);

  const corrupt = routeProjection(aliasLabel, {
    realm_label: "other-realm",
    ingest_url: "https://wrong-cell.example/v1/internal/agent-email:ingest",
  });
  let fetched = false;
  const collision = message({ to: aliasAddress });
  await assert.rejects(
    () => handleEmail(collision, dynamicEnv({ [aliasLabel]: corrupt }), {
      now: () => vectorNowMS,
      fetch: async () => { fetched = true; },
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.equal(fetched, false);
  assert.deepEqual(collision.rejected, []);
});

test("cell remains final recipient authority after alias routing", async () => {
  const mail = message({ to: aliasAddress });
  await handleEmail(mail, dynamicEnv({ [aliasLabel]: routeProjection(aliasLabel) }), {
    now: () => vectorNowMS,
    fetch: async () => new Response('{"verdict":"unknown_recipient"}', { status: 404 }),
  });
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
});


test("v2 relay version sends verdict headers with all-unknown when nothing is attested", async () => {
  let request;
  const mail = message();
  await handleEmail(mail, legacyEnv(null, { AGENT_EMAIL_RELAY_VERSION: "witself-email-relay-v2" }, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.deepEqual(mail.rejected, []);
  const headers = request.init.headers;
  assert.equal(headers.get("X-Witself-Email-Version"), "witself-email-relay-v2");
  assert.equal(headers.get("X-Witself-Email-SPF-Result"), "unknown");
  assert.equal(headers.get("X-Witself-Email-DKIM-Result"), "unknown");
  assert.equal(headers.get("X-Witself-Email-DMARC-Result"), "unknown");
});

test("v2 relay version records the trusted attester's verdicts in the signed headers", async () => {
  const { mail } = messageWithAuthenticationResults(trustedAuthservID, "pass");
  let request;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_RELAY_VERSION: "witself-email-relay-v2",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.deepEqual(mail.rejected, []);
  const headers = request.init.headers;
  assert.equal(headers.get("X-Witself-Email-Version"), "witself-email-relay-v2");
  assert.equal(headers.get("X-Witself-Email-DMARC-Result"), "pass");
  assert.equal(headers.get("X-Witself-Email-SPF-Result"), "unknown");
  assert.equal(headers.get("X-Witself-Email-DKIM-Result"), "unknown");
});

test("a sender-forged attester never fills v2 verdict headers", async () => {
  const { mail } = messageWithAuthenticationResults("mx.forged.example", "pass");
  let request;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_RELAY_VERSION: "witself-email-relay-v2",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.deepEqual(mail.rejected, []);
  const headers = request.init.headers;
  assert.equal(headers.get("X-Witself-Email-DMARC-Result"), "unknown");
});

test("a v1 relay never sends verdict headers even with an attested result", async () => {
  const { mail } = messageWithAuthenticationResults(trustedAuthservID, "pass");
  let request;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async (url, init) => {
      request = { url, init };
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.deepEqual(mail.rejected, []);
  const headers = request.init.headers;
  assert.equal(headers.get("X-Witself-Email-Version"), "witself-email-relay-pilot-v1");
  assert.equal(headers.get("X-Witself-Email-SPF-Result"), null);
  assert.equal(headers.get("X-Witself-Email-DMARC-Result"), null);
});

test("an invalid relay version fails closed as a transient before any relay", async () => {
  let fetched = false;
  const mail = message();
  await assert.rejects(
    handleEmail(mail, legacyEnv(null, { AGENT_EMAIL_RELAY_VERSION: "v2" }, vectorNowMS), {
      now: () => vector.metadata.timestamp * 1000,
      fetch: async () => {
        fetched = true;
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    }),
    /temporarily unavailable/,
  );
  assert.equal(fetched, false);
  assert.equal(mail.rejected.length, 0);
});

test("dmarc rejection still fires from the shared extraction on v2", async () => {
  const { mail } = messageWithAuthenticationResults(trustedAuthservID, "fail");
  let fetched = false;
  await handleEmail(mail, legacyEnv(null, {
    AGENT_EMAIL_RELAY_VERSION: "witself-email-relay-v2",
    AGENT_EMAIL_DMARC_REJECT_ENABLED: "true",
    AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: trustedAuthservID,
  }, vectorNowMS), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async () => {
      fetched = true;
      return new Response('{"verdict":"accepted"}', { status: 200 });
    },
  });
  assert.equal(fetched, false);
  assert.equal(mail.rejected.length, 1);
});
