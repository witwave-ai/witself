import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { handleEmail } from "../src/index.js";
import { CONFIG_KEY, recipientKey, runtimeConfig, runtimeRecipient } from "../src/directory.mjs";
import { PILOT_MAXIMUM_RAW_BYTES } from "../src/relay.mjs";
import { EDGE_METRICS_SCHEMA } from "../src/metrics.mjs";

const vector = JSON.parse(await readFile(new URL("./golden-vector.json", import.meta.url), "utf8"));
const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));
const raw = Buffer.from(vector.raw_base64, "base64");
const first = example.agents[0];

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

test("unknown and permanent cell verdicts use one sanitized permanent rejection", async () => {
  for (const verdict of ["unknown_recipient", "permanent"]) {
    const mail = message();
    await handleEmail(mail, env(), {
      fetch: async () => new Response(JSON.stringify({ verdict }), { status: 404 }),
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
  assert.deepEqual(oversized.rejected, ["recipient unavailable"]);
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
