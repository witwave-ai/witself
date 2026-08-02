import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { handleEmail } from "../src/index.js";
import {
  CONFIG_KEY,
  realmRouteKey,
  recipientKey,
  runtimeConfig,
  runtimeRecipient,
} from "../src/directory.mjs";
import { PILOT_MAXIMUM_RAW_BYTES } from "../src/relay.mjs";
import { EDGE_METRICS_SCHEMA } from "../src/metrics.mjs";

const vector = JSON.parse(await readFile(new URL("./golden-vector.json", import.meta.url), "utf8"));
const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));
const raw = Buffer.from(vector.raw_base64, "base64");
const first = example.agents[0];
const aliasLabel = "acme-west";
const aliasAddress = `alpha.${aliasLabel}@${example.domain}`;
const vectorNowMS = vector.metadata.timestamp * 1000;

function routeProjection(realmLabel, overrides = {}) {
  return {
    schema_version: 1,
    domain: example.domain,
    realm_label: realmLabel,
    realm_id: example.realm_id,
    route_kind: realmLabel === example.realm_label ? "canonical" : "realm_alias",
    state: "applied",
    controller_revision: 7,
    updated_at: new Date(vectorNowMS).toISOString(),
    cache_ttl_seconds: 300,
    cell_audience: example.cell_audience,
    ingest_url: example.ingest_url,
    ...overrides,
  };
}

function dynamicEnv(routes, metrics = null, extra = {}) {
  const values = new Map(
    Object.entries(routes).map(([realmLabel, value]) => [realmRouteKey(example.domain, realmLabel), value]),
  );
  return {
    RELAY_KEY_ID: vector.metadata.key_id,
    RELAY_ED25519_PRIVATE_KEY: vector.pkcs8_base64,
    REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "true",
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

function env(enabled = true, includeRecipient = true, metrics = null) {
  const values = new Map([[CONFIG_KEY, runtimeConfig(example, enabled)]]);
  if (includeRecipient) values.set(recipientKey(first.address), runtimeRecipient(example, first));
  return {
    RELAY_KEY_ID: vector.metadata.key_id,
    RELAY_ED25519_PRIVATE_KEY: vector.pkcs8_base64,
    EMAIL_DIRECTORY: {
      async get(key, type) {
        assert.equal(type, "json");
        return values.get(key) ?? null;
      },
    },
    ...(metrics ? { EMAIL_EDGE_METRICS: metrics } : {}),
  };
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

async function captureAccepted(overrides = {}) {
  let request;
  const mail = message(overrides);
  await handleEmail(mail, env(), {
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
      () => handleEmail(message(), env(), { fetch: async () => response }),
      { message: "agent email relay temporarily unavailable" },
    );
  }
});

test("plan-disabled receipt accepts and drops with a value-free metric", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, env(true, true, metrics), {
    fetch: async () => new Response('{"verdict":"feature_disabled"}', { status: 200 }),
  });
  assert.deepEqual(mail.rejected, []);
  assert.equal(points.length, 1);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA, "discarded_feature_disabled", "response",
  ]);
  assert.equal(points[0].doubles[3], 200);
  assert.doesNotMatch(JSON.stringify(points[0]), /@|address|account|realm_|agent_/i);
});

test("unknown and permanent cell verdicts use one sanitized permanent rejection", async () => {
  for (const [verdict, status] of [
    ["unknown_recipient", 404],
    ["permanent", 410],
  ]) {
    const mail = message();
    await handleEmail(mail, env(), {
      fetch: async () => new Response(JSON.stringify({ verdict }), { status }),
    });
    assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  }
});

test("terminal retry-canary verdict rejects once with no marker leakage", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, env(true, true, metrics), {
    fetch: async () => new Response('{"verdict":"retry_canary_rejected"}', { status: 410 }),
  });
  assert.deepEqual(mail.rejected, ["recipient unavailable"]);
  assert.equal(points.length, 1);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_retry_canary", "response",
  ]);
  assert.equal(points[0].doubles[3], 410);
  assert.doesNotMatch(JSON.stringify(points[0]), /challenge|retry_canary_rejected|X-Witself/i);
});

test("cell over-size verdict maps to a sanitized SMTP 552 outcome", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(mail, env(true, true, metrics), {
    fetch: async () => new Response('{"verdict":"over_size"}', { status: 413 }),
  });
  assert.deepEqual(mail.rejected, ["message too large"]);
  assert.equal(points.length, 1);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA, "rejected_over_size", "response",
  ]);
  assert.equal(points[0].doubles[3], 552);
  assert.doesNotMatch(
    JSON.stringify(points[0]),
    /@|address|account|realm_|agent_|subject|digest|signature/i,
  );
});

test("exact cell rate-limit verdict maps to a sanitized temporary outcome", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await assert.rejects(
    () => handleEmail(mail, env(true, true, metrics), {
      fetch: async () => new Response('{"verdict":"rate_limited"}', {
        status: 429,
        headers: { "Retry-After": "17" },
      }),
    }),
    { message: "agent email relay temporarily unavailable" },
  );
  assert.deepEqual(mail.rejected, []);
  assert.equal(points.length, 1);
  assert.deepEqual(points[0].indexes, ["tempfail_rate_limited"]);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA, "tempfail_rate_limited", "response",
  ]);
  assert.equal(points[0].doubles[3], 429);
  assert.doesNotMatch(
    JSON.stringify(points[0]),
    /@|address|account|realm_|agent_|sender|recipient|subject|digest|signature/i,
  );
});

test("disabled pilot and transport failures use one sanitized transient error", async () => {
  await assert.rejects(() => handleEmail(message(), env(false), {}), {
    message: "agent email relay temporarily unavailable",
  });
  await assert.rejects(() => handleEmail(message(), env(), { fetch: async () => { throw new Error("secret upstream"); } }), {
    message: "agent email relay temporarily unavailable",
  });
});

test("unenrolled and oversized messages reject before relay", async () => {
  let fetched = false;
  const unlistedAddress = `other.${example.realm_label}@${example.domain}`;
  const unlisted = message({ to: unlistedAddress });
  await handleEmail(unlisted, env(), { fetch: async () => { fetched = true; } });
  assert.deepEqual(unlisted.rejected, ["recipient unavailable"]);

  const oversized = message({
    rawSize: PILOT_MAXIMUM_RAW_BYTES + 1,
    raw: { must_not_be_read: true },
  });
  await handleEmail(oversized, env(), { fetch: async () => { fetched = true; } });
  assert.deepEqual(oversized.rejected, ["message too large"]);
  assert.equal(fetched, false);
});

test("an enrolled recipient missing from the eventually consistent KV detail map tempfails", async () => {
  await assert.rejects(() => handleEmail(message(), env(true, false), {}), {
    message: "agent email relay temporarily unavailable",
  });
});

test("provider raw-size mismatch tempfails rather than accepting partial content", async () => {
  await assert.rejects(
    () => handleEmail(message({ rawSize: raw.byteLength + 1 }), env(), { fetch: async () => new Response() }),
    { message: "agent email relay temporarily unavailable" },
  );
});

test("edge metrics record value-free accepted, rejected, and tempfailed outcomes", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };

  await handleEmail(message(), env(true, true, metrics), {
    now: () => vector.metadata.timestamp * 1000,
    fetch: async () => new Response('{"verdict":"accepted"}', { status: 200 }),
  });

  const unknown = message({ to: `other.${example.realm_label}@${example.domain}` });
  await handleEmail(unknown, env(true, true, metrics), {
    now: () => vector.metadata.timestamp * 1000,
  });

  await assert.rejects(
    () => handleEmail(message(), env(false, true, metrics), {
      now: () => vector.metadata.timestamp * 1000,
    }),
    { message: "agent email relay temporarily unavailable" },
  );

  await assert.rejects(
    () => handleEmail(message(), env(true, true, metrics), {
      now: () => vector.metadata.timestamp * 1000,
      fetch: async () => new Response('{"verdict":"receive_disabled"}', { status: 503 }),
    }),
    { message: "agent email relay temporarily unavailable" },
  );

  assert.equal(points.length, 4);
  assert.deepEqual(points.map((point) => point.blobs[1]), [
    "accepted", "rejected_unknown_recipient", "tempfail_disabled", "tempfail_disabled",
  ]);
  assert.equal(points.at(-1).blobs[2], "response");
  for (const point of points) {
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
  await handleEmail(mail, env(true, true, metrics), {
    fetch: async () => new Response('{"verdict":"accepted"}', { status: 200 }),
  });
  assert.deepEqual(mail.rejected, []);
});

test("canonical and realm-alias projections converge on one cell route", async () => {
  for (const [realmLabel, address] of [
    [example.realm_label, first.address],
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

test("fleet delivery gate is exact-true, alias-only, and default-off", async () => {
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
    assert.equal(fetched, false);
    assert.deepEqual(mail.rejected, []);
    assert.equal(points.length, 1);
    assert.deepEqual(points[0].blobs, [
      EDGE_METRICS_SCHEMA, "tempfail_alias_gate", "route",
    ]);
  }

  let canonicalRelayed = false;
  const canonical = message();
  await handleEmail(
    canonical,
    dynamicEnv(
      { [example.realm_label]: routeProjection(example.realm_label) },
      null,
      { REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false" },
    ),
    {
      now: () => vectorNowMS,
      fetch: async () => {
        canonicalRelayed = true;
        return new Response('{"verdict":"accepted"}', { status: 200 });
      },
    },
  );
  assert.equal(canonicalRelayed, true);
  assert.deepEqual(canonical.rejected, []);
});

test("dynamic routing keeps account-plan enforcement in the signed cell verdict", async () => {
  const points = [];
  const metrics = { writeDataPoint(point) { points.push(point); } };
  const mail = message();
  await handleEmail(
    mail,
    dynamicEnv({ [example.realm_label]: routeProjection(example.realm_label) }, metrics),
    {
      now: () => vectorNowMS,
      fetch: async () => new Response('{"verdict":"feature_disabled"}', { status: 200 }),
    },
  );
  assert.deepEqual(mail.rejected, []);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA, "discarded_feature_disabled", "response",
  ]);
});

test("suspended and retired dynamic routes fail closed without reaching a cell", async () => {
  for (const [state, expectedOutcome, rejects] of [
    ["suspended", "tempfail_suspended_route", []],
    ["retired", "rejected_inactive_route", ["recipient unavailable"]],
  ]) {
    const value = routeProjection(aliasLabel, { state });
    if (state === "suspended") value.suspension_disposition = "retry";
    delete value.cell_audience;
    delete value.ingest_url;
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
    assert.equal(fetched, false);
    assert.deepEqual(mail.rejected, rejects);
    assert.equal(points.length, 1);
    assert.equal(points[0].blobs[1], expectedOutcome);
    assert.equal(points[0].blobs[2], "route");
    assert.doesNotMatch(
      JSON.stringify(points[0]),
      /@|address|account|realm_|agent_|subject|digest|signature/i,
    );
  }
});

test("plan-inactive suspended routes reject without an unbounded SMTP retry loop", async () => {
  const value = routeProjection(aliasLabel, {
    state: "suspended",
    suspension_disposition: "inactive",
  });
  delete value.cell_audience;
  delete value.ingest_url;
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
  assert.equal(points[0].blobs[1], "rejected_inactive_route");
});

test("malformed dynamic addresses reject before directory lookup or relay", async () => {
  for (const address of [
    `alpha.bad--alias@${example.domain}`,
    `alpha.xn--acme@${example.domain}`,
    `alpha.one.two@${example.domain}`,
    `alpha.acme_west@${example.domain}`,
  ]) {
    let reads = 0;
    let fetched = false;
    const currentEnv = dynamicEnv({});
    currentEnv.EMAIL_DIRECTORY.get = async () => { reads++; return null; };
    const mail = message({ to: address });
    await handleEmail(mail, currentEnv, { fetch: async () => { fetched = true; } });
    assert.deepEqual(mail.rejected, ["recipient unavailable"]);
    assert.equal(reads, 0);
    assert.equal(fetched, false);
  }
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
    `https://control.example/v1/email/realm-routes/${example.domain}/${aliasLabel}`,
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
    assert.equal(points[0].blobs[1], "tempfail_route_lookup");
    assert.equal(points[0].blobs[2], "route");
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
  const current = env();
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
