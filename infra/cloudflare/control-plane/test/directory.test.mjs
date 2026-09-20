import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

import {
  containerCalls,
  resetContainerCalls,
} from "./fixtures/cloudflare-containers-stub.mjs";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

const ACCOUNT = "directory-account";
const CELL = "directory-cell";
const entry = Object.freeze({
  cell: CELL,
  endpoint: "https://old-cell.test.invalid",
  region: "old-region",
  region_code: "usw2",
  cell_registration_id: "account-placement-registration",
  epoch: 7,
});
const registry = Object.freeze({
  endpoint: "https://current-cell.test.invalid",
  region: "nyc1",
  region_code: "use1",
  registration_id: "replacement-registration",
  provision_token: "private-test-provision-token",
  backup_token: "private-test-backup-token",
  accepting: false,
  weight: 3,
  owner: "private-test-owner",
});
const archived = Object.freeze({
  cell: "archived-cell",
  region: "archive-region",
  region_code: "usw2",
  exported_at: "2026-09-19T12:00:00Z",
  object: "private/archive/object",
  size: 42,
});

test.beforeEach(resetContainerCalls);
test.afterEach(() => assert.deepEqual(containerCalls, []));

async function lookup(rows, method = "GET") {
  const calls = [];
  const response = await worker.fetch(new Request(
    `https://cp.test.invalid/v1/directory/${ACCOUNT}`,
    { method },
  ), {
    DIRECTORY: {
      async get(key, options) {
        calls.push({ key, options });
        return structuredClone(rows.get(key) ?? null);
      },
      put() { assert.fail("directory lookup must not write KV"); },
      delete() { assert.fail("directory lookup must not delete KV"); },
    },
    CELL_COORDINATOR: {
      idFromName() { assert.fail("directory lookup must not call a DO"); },
    },
  }, { waitUntil() { assert.fail("directory lookup must not schedule work"); } });
  return { response, calls };
}

function assertActiveReads(calls) {
  assert.deepEqual(calls, [
    { key: `acct:${ACCOUNT}`, options: { type: "json", cacheTtl: 60 } },
    { key: `cell:${CELL}`, options: { type: "json", cacheTtl: 60 } },
  ]);
}

test("directory follows registry routing while preserving the placement fence and public shape", async () => {
  const { response, calls } = await lookup(new Map([
    [`acct:${ACCOUNT}`, entry],
    [`cell:${CELL}`, registry],
  ]));
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Cache-Control"), "max-age=60");
  assert.equal(await response.text(), JSON.stringify({
    schema_version: "witself.v0",
    account_id: ACCOUNT,
    cell: {
      ...entry,
      endpoint: registry.endpoint,
      region: registry.region,
      region_code: registry.region_code,
    },
  }));
  assertActiveReads(calls);
});

test("directory retains stored region hints when the registry omits them", async () => {
  const { response, calls } = await lookup(new Map([
    [`acct:${ACCOUNT}`, entry],
    [`cell:${CELL}`, { endpoint: registry.endpoint }],
  ]));
  assert.deepEqual((await response.json()).cell, {
    ...entry, endpoint: registry.endpoint,
  });
  assertActiveReads(calls);
});

test("directory retains stored region hints when registry fields are null", async () => {
  const { response, calls } = await lookup(new Map([
    [`acct:${ACCOUNT}`, entry],
    [`cell:${CELL}`, { ...registry, region: null, region_code: null }],
  ]));
  assert.deepEqual((await response.json()).cell, {
    ...entry, endpoint: registry.endpoint,
  });
  assertActiveReads(calls);
});

test("directory uses registry region fields even when they are empty strings", async () => {
  const { response, calls } = await lookup(new Map([
    [`acct:${ACCOUNT}`, entry],
    [`cell:${CELL}`, { ...registry, region: "", region_code: "" }],
  ]));
  assert.deepEqual((await response.json()).cell, {
    ...entry, endpoint: registry.endpoint, region: "", region_code: "",
  });
  assertActiveReads(calls);
});

for (const [label, cellRow] of [
  ["missing cell row", null],
  ["missing endpoint", {}],
  ["null endpoint", { endpoint: null }],
  ["non-string endpoint", { endpoint: 123 }],
  ["empty endpoint", { endpoint: "" }],
  ["HTTP endpoint", { endpoint: "http://insecure.test.invalid" }],
  ["malformed HTTPS endpoint", { endpoint: "https://" }],
  ["relative endpoint", { endpoint: "/cell" }],
  ["credential-bearing endpoint", { endpoint: "https://user:password@cell.test.invalid" }],
]) {
  test(`directory preserves fallback response bytes for ${label}`, async () => {
    const { response, calls } = await lookup(new Map([
      [`acct:${ACCOUNT}`, entry],
      [`cell:${CELL}`, cellRow && {
        region: registry.region,
        region_code: registry.region_code,
        ...cellRow,
      }],
    ]));
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("Cache-Control"), "max-age=60");
    assert.equal(await response.text(), JSON.stringify({
      schema_version: "witself.v0", account_id: ACCOUNT, cell: entry,
    }));
    assertActiveReads(calls);
  });
}

test("directory active route still wins over archived and pending rows", async () => {
  const { response, calls } = await lookup(new Map([
    [`acct:${ACCOUNT}`, entry],
    [`cell:${CELL}`, registry],
    [`archived:${ACCOUNT}`, archived],
    [`pending:${ACCOUNT}`, { cell: "pending-cell" }],
  ]));
  const body = await response.json();
  assert.equal(response.status, 200);
  assert.equal(body.cell.endpoint, registry.endpoint);
  assert.equal(body.archived, undefined);
  assertActiveReads(calls);
});

test("directory archived route still wins over pending without reading the registry", async () => {
  const { response, calls } = await lookup(new Map([
    [`cell:${archived.cell}`, registry],
    [`archived:${ACCOUNT}`, archived],
    [`pending:${ACCOUNT}`, { cell: "pending-cell" }],
  ]));
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Cache-Control"), "max-age=30");
  assert.equal(await response.text(), JSON.stringify({
    schema_version: "witself.v0",
    account_id: ACCOUNT,
    archived: {
      cell: archived.cell,
      region: archived.region,
      region_code: archived.region_code,
      exported_at: archived.exported_at,
    },
  }));
  assert.deepEqual(calls, [
    { key: `acct:${ACCOUNT}`, options: { type: "json", cacheTtl: 60 } },
    { key: `archived:${ACCOUNT}`, options: { type: "json", cacheTtl: 30 } },
  ]);
});

for (const pending of [false, true]) {
  test(`directory ${pending ? "pending-only" : "unknown"} account still returns 404`, async () => {
    const { response, calls } = await lookup(new Map(pending ? [
      [`pending:${ACCOUNT}`, { cell: CELL }],
      [`cell:${CELL}`, registry],
    ] : []));
    assert.equal(response.status, 404);
    assert.deepEqual(await response.json(), {
      schema_version: "witself.v0", error: "unknown account",
    });
    assert.deepEqual(calls.map(({ key }) => key), [
      `acct:${ACCOUNT}`, `archived:${ACCOUNT}`,
    ]);
  });
}

test("directory rejects other methods before reading KV", async () => {
  const { response, calls } = await lookup(new Map(), "POST");
  assert.equal(response.status, 405);
  assert.deepEqual(calls, []);
});
