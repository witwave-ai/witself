import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";
import {
  ACCOUNT_BILLING_CLOSE_TIMEOUT_MS,
  ACCOUNT_CLOSE_BRIDGE_HEADER,
  BRIDGE_BODY_MAX_BYTES,
  CELL_PROVISION_HEADER,
} from "../src/bridge.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
const { containerCalls } = await import("./fixtures/cloudflare-containers-stub.mjs");

const accountID = "acct_close";
const path = `/v1/accounts/${accountID}/billing:close`;
const ownerAuthorization = "Bearer owner-fixture-token";
const cellToken = "cell-fixture-token";
const bridgeToken = "bridge-fixture-token";

function fixture() {
  const f = {
    reads: [],
    forwarded: [],
    limiterCalls: 0,
    values: new Map([
      [`acct:${accountID}`, { cell: "cell-one" }],
      ["cell:cell-one", { endpoint: "https://cell-one.invalid", provision_token: cellToken }],
      ["cell:cell-two", { endpoint: "https://cell-two.invalid", provision_token: "other-cell-token" }],
    ]),
  };
  containerCalls.length = 0;
  f.env = {
    INTERNAL_BRIDGE_TOKEN: bridgeToken,
    PUBLIC_IP_LIMITER: { limit() { f.limiterCalls++; return { success: false }; } },
    DIRECTORY: {
      async get(key, options) {
        f.reads.push(key);
        assert.deepEqual(options, { type: "json" });
        if (f.directoryThrows) throw new Error("private-directory-detail");
        return f.values.get(key) ?? null;
      },
    },
    CONTROL_PLANE: {
      async fetch(request) {
        f.forwarded.push(request);
        if (f.fetchThrows) throw new Error("private-provider-detail");
        return Response.json({
          schema_version: "witself.v0", account_id: accountID, status: "billing_closed",
        }, { headers: { "Set-Cookie": "private-cookie" } });
      },
    },
  };
  f.run = async ({ route = path, method = "POST", headers = {}, body } = {}) => {
    return worker.fetch(new Request(`https://cp.invalid${route}`, {
      method,
      headers: {
        Authorization: ownerAuthorization,
        [CELL_PROVISION_HEADER]: cellToken,
        "Content-Type": "application/json",
        ...headers,
      },
      body,
    }), f.env, { waitUntil() {} });
  };
  return f;
}

async function refusesWithoutForwarding(f, expectedStatus, init) {
  const response = await f.run(init);
  assert.equal(response.status, expectedStatus);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(f.forwarded.length, 0);
  assert.deepEqual(containerCalls, []);
  const encoded = await response.text();
  for (const secret of [cellToken, bridgeToken, "private-directory-detail", "other-cell-token"]) {
    assert.equal(encoded.includes(secret), false, "credential and backend details stay private");
  }
}

test("account billing close bridges only routed cell proof and owner bearer", async (t) => {
  const f = fixture();
  const timeouts = [];
  const signal = new AbortController().signal;
  t.mock.method(AbortSignal, "timeout", (ms) => { timeouts.push(ms); return signal; });
  const response = await f.run({
    body: "{}",
    headers: {
      [ACCOUNT_CLOSE_BRIDGE_HEADER]: "caller-spoofed-proof",
      "X-Witself-Admin-ID": "caller-spoofed-admin",
      Cookie: "private-browser-cookie",
    },
  });
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0", account_id: accountID, status: "billing_closed",
  });
  assert.equal(response.headers.get("Set-Cookie"), null);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(f.limiterCalls, 0, "machine cancellation has its own authority, not a public IP quota");
  assert.equal(f.forwarded.length, 1);
  assert.deepEqual(f.reads, [`archived:${accountID}`, `acct:${accountID}`, "cell:cell-one"]);
  const forwarded = f.forwarded[0];
  assert.equal(forwarded.url, `http://control-plane.internal${path}`);
  assert.equal(forwarded.method, "POST");
  assert.deepEqual([...forwarded.headers], [
    ["authorization", ownerAuthorization],
    ["content-type", "application/json"],
    [ACCOUNT_CLOSE_BRIDGE_HEADER.toLowerCase(), bridgeToken],
  ]);
  assert.equal(await forwarded.text(), "{}");
  assert.equal(forwarded.signal.aborted, false);
  assert.deepEqual(timeouts, [ACCOUNT_BILLING_CLOSE_TIMEOUT_MS]);
  assert.equal(ACCOUNT_BILLING_CLOSE_TIMEOUT_MS, 270_000);
});

test("owner alone, forged bridge proof and another cell token cannot close billing", async () => {
  for (const headers of [
    { [CELL_PROVISION_HEADER]: "" },
    { [CELL_PROVISION_HEADER]: "", [ACCOUNT_CLOSE_BRIDGE_HEADER]: bridgeToken },
    { [CELL_PROVISION_HEADER]: "wrong-cell-token" },
    { [CELL_PROVISION_HEADER]: "other-cell-token" },
    { Authorization: "" },
    { Authorization: "Bearer " },
    { Authorization: "Basic owner-fixture-token" },
  ]) {
    await refusesWithoutForwarding(fixture(), 401, { headers });
  }
  const f = fixture();
  f.values.set(`acct:${accountID}`, { cell: "cell-two" });
  await refusesWithoutForwarding(f, 401);
});

test("account billing close rejects unavailable or malformed routing and bridge configuration", async () => {
  for (const configure of [
    (f) => { f.values.set(`acct:${accountID}`, { cell: "invalid/route" }); },
    (f) => { f.values.set(`acct:${accountID}`, { cell: ["cell-one"] }); },
    (f) => { f.values.delete("cell:cell-one"); },
    (f) => { f.values.set("cell:cell-one", { provision_token: cellToken }); },
    (f) => { f.values.set("cell:cell-one", { endpoint: "http://cell-one.invalid", provision_token: cellToken }); },
    (f) => { f.values.set("cell:cell-one", { endpoint: "https://user:pass@cell-one.invalid", provision_token: cellToken }); },
    (f) => { f.values.set("cell:cell-one", { endpoint: "https://cell-one.invalid" }); },
    (f) => { f.values.set("cell:cell-one", { endpoint: "https://cell-one.invalid", provision_token: [cellToken] }); },
    (f) => { f.values.set("cell:cell-one", { endpoint: "https://cell-one.invalid", provision_token: " " }); },
    (f) => { f.directoryThrows = true; },
    (f) => { f.env.INTERNAL_BRIDGE_TOKEN = ""; },
    (f) => { f.env.INTERNAL_BRIDGE_TOKEN = " "; },
    (f) => { f.env.INTERNAL_BRIDGE_TOKEN = { secret: bridgeToken }; },
  ]) {
    const f = fixture();
    configure(f);
    await refusesWithoutForwarding(f, 503);
  }
  const unknown = fixture();
  unknown.values.delete(`acct:${accountID}`);
  await refusesWithoutForwarding(unknown, 404);
  const archived = fixture();
  archived.values.set(`archived:${accountID}`, { cell: "cell-one" });
  await refusesWithoutForwarding(archived, 409);
});

test("malformed close paths, methods, query and oversized bodies terminate before Go", async () => {
  for (const route of [
    "/v1/accounts/encoded%20space/billing:close",
    `/v1/accounts/${"a".repeat(129)}/billing:close`,
    "/v1/accounts/acct_close/billing:close/extra",
    "/v1/accounts/%61cct_close/billing:close",
    "/v1/accounts/acct_close/billing%3Aclose",
    "/v1/accounts/acct_close%ZZ/billing:close",
    "/v1/accounts//billing:close",
  ]) {
    const f = fixture();
    await refusesWithoutForwarding(f, 404, { route });
    assert.equal(f.reads.length, 0);
  }
  for (const method of ["GET", "PUT", "DELETE"]) {
    const f = fixture();
    await refusesWithoutForwarding(f, 405, { method });
    assert.equal(f.reads.length, 0);
  }
  await refusesWithoutForwarding(fixture(), 400, { route: `${path}?account_id=other` });
  await refusesWithoutForwarding(fixture(), 413, { body: "x".repeat(BRIDGE_BODY_MAX_BYTES + 1) });
});

test("generic Go proxy never forwards customer-supplied account-close credentials", async () => {
  const f = fixture();
  delete f.env.PUBLIC_IP_LIMITER;
  const response = await f.run({
    route: `/v1/accounts/${accountID}/plan`,
    headers: { [ACCOUNT_CLOSE_BRIDGE_HEADER]: bridgeToken },
  });
  assert.equal(response.status, 200);
  assert.equal(f.forwarded.length, 1);
  assert.equal(f.forwarded[0].headers.get("Authorization"), ownerAuthorization);
  assert.equal(f.forwarded[0].headers.get(ACCOUNT_CLOSE_BRIDGE_HEADER), null);
  assert.equal(f.forwarded[0].headers.get(CELL_PROVISION_HEADER), null);
});

test("unavailable billing container returns a value-free retryable failure", async () => {
  const f = fixture();
  f.fetchThrows = true;
  const response = await f.run();
  assert.equal(response.status, 502);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0", error: "account billing closure is unavailable",
  });
});
