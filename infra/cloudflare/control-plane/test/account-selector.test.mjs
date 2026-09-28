import assert from "node:assert/strict";
import { timingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    value: (a, b) => timingSafeEqual(Buffer.from(a), Buffer.from(b)),
  });
}

function fixture(t) {
  const values = new Map();
  for (const [name, accepting, cloud] of [["src", false, "civo"], ["dst", true, "gcp"]]) {
    values.set(`cell:${name}`, { name, accepting, cloud, endpoint: `https://${name}.invalid`, provision_token: "fixture", region: "test" });
  }
  for (const id of ["aaa", "chosen", "founder"]) values.set(`acct:${id}`, { cell: "src", epoch: 1 });
  values.set("acct:stays", { cell: "dst" });
  const calls = [];
  const env = {
    FLEET_TOKEN: "fixture-fleet", ARCHIVES: {},
    DIRECTORY: {
      async get(key, opts) {
        const value = values.get(key);
        return value === undefined ? null : opts?.type === "json" ? structuredClone(value) : JSON.stringify(value);
      },
      async put(key, value) { values.set(key, JSON.parse(value)); },
      async list({ prefix, cursor }) {
        // Real selection and remaining loops must traverse more than one page.
        const keys = [...values.keys()].filter(k => k.startsWith(prefix)).sort();
        const offset = Number(cursor || 0);
        return { keys: keys.slice(offset, offset + 2).map(name => ({ name })), list_complete: offset + 2 >= keys.length, cursor: String(offset + 2) };
      },
    },
    ACCOUNT_LIFECYCLE: {
      idFromName: id => id,
      get: id => ({ async fetch(req) {
        const body = await req.json();
        assert.equal(body.account_id, id);
        calls.push(body);
        if (body.action === "evacuate") values.delete(`acct:${id}`);
        else values.set(`acct:${id}`, { cell: body.target_cell, epoch: 2 });
        return Response.json({ ok: true });
      } }),
    },
  };
  t.mock.method(globalThis, "fetch", async (url) => {
    if (url.endsWith(":contact")) return Response.json({ status: "active" });
    assert.ok(url.endsWith("/placement-policy"));
    return Response.json({ placement_policy: { preferred_clouds: ["gcp", "civo"], rebalance_on: ["cloud"] } });
  });
  async function request(path, body, authorized = true) {
    const res = await worker.fetch(new Request(`https://cp.invalid${path}`, {
      method: "POST", headers: { Authorization: authorized ? "Bearer fixture-fleet" : "Bearer wrong", "Content-Type": "application/json" }, body: JSON.stringify(body),
    }), env, { waitUntil() {} });
    return { status: res.status, body: await res.json() };
  }
  return { values, calls, request };
}

for (const batch of [0, 10, "ignored", { toString: null, valueOf: null }]) {
  test(`evacuate selector selects only a later account and ignores batch ${JSON.stringify(batch)}`, async t => {
    const f = fixture(t);
    const res = await f.request("/v1/cells/src:evacuate", { account_id: "chosen", batch });
    assert.equal(res.status, 200);
    assert.deepEqual(res.body.evacuated, [{ account_id: "chosen", ok: true }]);
    assert.equal(res.body.remaining, 2);
    assert.equal(res.body.progress.done, 1);
    assert.equal(f.values.get("evac:src").remaining, 2);
    assert.deepEqual(f.calls.map(c => c.account_id), ["chosen"]);
    assert.equal(f.values.get("acct:founder").cell, "src");
  });
}

test("evacuate selector rejects wrong cell and missing account without lifecycle or progress writes", async t => {
  const f = fixture(t);
  for (const account_id of ["stays", "missing"]) {
    const res = await f.request("/v1/cells/src:evacuate", { account_id });
    assert.equal(res.status, 404);
    assert.equal(res.body.error, "account is not routed to this cell");
  }
  assert.deepEqual(f.calls, []);
  assert.equal(f.values.has("evac:src"), false);
});

test("evacuate selector retains authorization and drain preconditions", async t => {
  const f = fixture(t);
  assert.equal((await f.request("/v1/cells/src:evacuate", { account_id: "chosen" }, false)).status, 401);
  f.values.get("cell:src").accepting = true;
  assert.equal((await f.request("/v1/cells/src:evacuate", { account_id: "chosen" })).status, 409);
  assert.deepEqual(f.calls, []);
});

for (const path of ["/v1/cells/src:evacuate", "/v1/placement:rebalance"]) {
  test(`${path} rejects invalid selectors instead of silently running a batch`, async t => {
    const f = fixture(t);
    for (const account_id of ["", "bad/id", "x".repeat(129), null, 123, ["chosen"], {}]) {
      assert.equal((await f.request(path, { account_id })).status, 400);
    }
    assert.deepEqual(f.calls, []);
  });
}

for (const dry_run of [true, false]) {
  test(`rebalance selector preserves dry_run=${dry_run} and ignores batch`, async t => {
    const f = fixture(t);
    const res = await f.request("/v1/placement:rebalance", { account_id: "chosen", batch: { toString: null, valueOf: null }, dry_run });
    assert.equal(res.status, 200);
    assert.equal(res.body.rebalanced.length, 1);
    assert.equal(res.body.rebalanced[0].account_id, "chosen");
    assert.equal(res.body.rebalanced[0].ok, true);
    assert.equal(res.body.rebalanced[0].from_cell, "src");
    assert.equal(res.body.rebalanced[0].to_cell, "dst");
    assert.equal(res.body.remaining, dry_run ? 3 : 2);
    assert.deepEqual(f.calls.map(c => c.account_id), dry_run ? [] : ["chosen"]);
    assert.equal(f.values.get("acct:chosen").cell, dry_run ? "src" : "dst");
  });
}

test("rebalance selector rejects non-candidates including missing routes and ineligible status", async t => {
  const f = fixture(t);
  for (const account_id of ["stays", "missing"]) {
    assert.equal((await f.request("/v1/placement:rebalance", { account_id, dry_run: true })).status, 409);
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ status: "closed" }));
  assert.equal((await f.request("/v1/placement:rebalance", { account_id: "chosen" })).status, 409);
  assert.deepEqual(f.calls, []);
});

for (const [path, field] of [["/v1/cells/src:evacuate", "evacuated"], ["/v1/placement:rebalance", "rebalanced"]]) {
  test(`${path} without selector retains paged batch order`, async t => {
    const f = fixture(t);
    const res = await f.request(path, { batch: 2 });
    assert.equal(res.status, 200);
    assert.deepEqual(res.body[field].map(row => row.account_id), ["aaa", "chosen"]);
    assert.equal(res.body.remaining, 1);
  });
}
