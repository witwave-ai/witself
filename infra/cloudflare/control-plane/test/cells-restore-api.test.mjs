import assert from "node:assert/strict";
import { timingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";
register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
if (typeof crypto.subtle.timingSafeEqual !== "function") Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
  configurable: true, value: (a, b) => a.byteLength === b.byteLength && timingSafeEqual(new Uint8Array(a), new Uint8Array(b)),
});
function fixture() {
  const values = new Map();
  const directory = {
    async get(key, options) { const value = values.get(key); return value === undefined ? null : options?.type === "json" ? JSON.parse(value) : value; },
    async put(key, value) { values.set(key, value); },
    async delete(key) { values.delete(key); },
    async list({ prefix = "" } = {}) { return { keys: [...values.keys()].filter((key) => key.startsWith(prefix)).map((name) => ({ name })), list_complete: true }; },
  };
  const cell = { endpoint: "https://cell.invalid", region: "New York", region_code: "nyc1", cloud: "civo", channel: "edge", owner: "witwave", weight: 1, accepting: true, provision_token: "test-provision" };
  values.set("cell:target", JSON.stringify(cell));
  for (const account of ["acc_one", "acc_two"]) values.set(`archived:${account}`, JSON.stringify({ object: `archives/${account}`, status: "suspended", region_code: "nyc1" }));
  const f = { calls: [], values, result: { import_pending: true, retryable: true, import_job: { attempts: 2 } } };
  f.env = { DIRECTORY: directory, ARCHIVES: {}, FLEET_TOKEN: "test-fleet", CP_ACCOUNT_BACKUPS_ENABLED: "false",
    ACCOUNT_LIFECYCLE: { idFromName: (id) => id, get: () => ({ async fetch(request) {
      const input = await request.json(); f.calls.push(input); f.onCall?.(input);
      return f.busy ? Response.json({ error: "account lifecycle operation already in progress" }, { status: 409 }) : Response.json({ ok: true, result: f.result });
    } }) },
  };
  f.run = async (path, body = {}) => {
    const response = await worker.fetch(new Request(`https://cp.invalid${path}`, { method: "POST", headers: { Authorization: "Bearer test-fleet", "Content-Type": "application/json" }, body: JSON.stringify(body) }), f.env, { waitUntil() {} });
    assert.equal(response.status, 200); return response.json();
  };
  return f;
}
for (const placement of [false, true]) {
  test(`restore pending and busy accounting placement=${placement}`, async () => {
    for (const busy of [false, true]) {
      const f = fixture(); f.busy = busy;
      const result = await f.run(placement ? "/v1/placement:restore" : "/v1/cells/target:restore");
      assert.equal(result.remaining, 2); assert.equal(result.restored.length, 2);
      for (const row of result.restored) { assert.equal(row.ok, true); assert.equal(row.pending, true); if (busy) { assert.equal(row.reason, "busy"); assert.equal(row.retryable, true); } else { assert.equal(row.attempts, 2); assert.equal(row.retryable, true); } }
      if (!placement) { const progress = JSON.parse(f.values.get("restore:target")); assert.equal(progress.done, 0); assert.deepEqual(progress.failed, []); }
    }
  });
}
test("restore call deadline skips accounts and clamps inline budget", async (t) => {
  const f = fixture(); let now = 1000000; t.mock.method(Date, "now", () => now);
  f.onCall = () => { now += f.calls.length === 1 ? 7 * 60_000 : 60_000; };
  f.values.set("archived:acc_three", f.values.get("archived:acc_two"));
  const result = await f.run("/v1/cells/target:restore");
  assert.deepEqual(f.calls.map((call) => call.inline_budget_ms), [120_000, 60_000]); assert.equal(result.restored.length, 2); assert.equal(result.remaining, 3);
});
test("placement runner shares deadline across restore and rebalance", async (t) => {
  const f = fixture(); let now = 1000000; t.mock.method(Date, "now", () => now);
  mockPlacementReads(t);
  const source = { ...JSON.parse(f.values.get("cell:target")), accepting: false, region_code: "other-region", endpoint: "https://source.invalid" };
  f.values.set("cell:source", JSON.stringify(source)); f.values.set("acct:acc_move", JSON.stringify({ cell: "source", status: "active" }));
  f.onCall = () => { now += 240_000; };
  const result = await f.run("/v1/placement:run", { restore_archives: true, rebalance: true });
  assert.equal(f.calls.length, 1); assert.equal(f.calls[0].inline_budget_ms, 120_000); assert.equal(result.restore.restored.length, 1); assert.equal(result.rebalance.rebalanced.length, 0);
});
test("scheduled placement uses zero budget and pending is not logged as restored", async (t) => {
  const f = fixture(); f.values.set("config:placement_runner", JSON.stringify({ enabled: true, restore_archives: true, rebalance: false }));
  const logs = []; t.mock.method(console, "log", (line) => logs.push(line));
  const tasks = []; await worker.scheduled({ cron: "*/5 * * * *", scheduledTime: Date.now() }, f.env, { waitUntil(task) { tasks.push(task); } });
  await Promise.allSettled(tasks);
  assert.equal(f.calls.length, 2); assert.ok(f.calls.every((call) => call.inline_budget_ms === 0));
  assert.ok(logs.some((line) => line.includes("pending=2 restored=0 rebalanced=0")));
});

test("rebalance keeps a pending account in remaining after its live route retires", async (t) => {
  mockPlacementReads(t);
  const f = fixture();
  f.values.set("cell:source", JSON.stringify({ ...JSON.parse(f.values.get("cell:target")), accepting: false, region_code: "other-region", endpoint: "https://source.invalid" }));
  f.values.set("acct:acc_move", JSON.stringify({ cell: "source", status: "active" }));
  f.onCall = () => f.values.delete("acct:acc_move");
  const result = await f.run("/v1/placement:rebalance", { account_id: "acc_move" });
  assert.equal(result.remaining, 1);
  assert.equal(result.rebalanced.length, 1);
  assert.equal(result.rebalanced[0].pending, true);
  assert.equal(result.rebalanced[0].attempts, 2);
});

function mockPlacementReads(t) {
  t.mock.method(globalThis, "fetch", async (url) => {
    assert.ok(url.startsWith("https://source.invalid/v1/accounts/acc_move"));
    if (url.endsWith(":contact")) return Response.json({ status: "active" });
    assert.ok(url.endsWith("/placement-policy"));
    return Response.json({ placement_policy: { allowed_regions: ["nyc1"] } });
  });
}
