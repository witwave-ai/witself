import assert from "node:assert/strict";
import { timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import { register } from "node:module";
import test from "node:test";
import { bootstrapLiveState, bootstrapArchivedState, claimOperation, validateLifecycleState } from "../src/account-lifecycle-state.mjs";
import { projectLifecycleStatus } from "../src/account-lifecycle-status.mjs";
register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;
const { containerCalls } = await import("./fixtures/cloudflare-containers-stub.mjs");
if (typeof crypto.subtle.timingSafeEqual !== "function") Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
  configurable: true, value: (a, b) => a.byteLength === b.byteLength && timingSafeEqual(new Uint8Array(a), new Uint8Array(b)),
});
const accountID = "acct_runtime";
const path = `/v1/placement/accounts/${accountID}/lifecycle`;
const now = Date.parse("2026-09-28T12:00:00Z");
const id = "11111111-1111-4111-8111-111111111111";
// Also used by the one-time, external golden generator; tests only read the golden file.
export function lifecycleGoldenInputs() {
  const live = () => bootstrapLiveState({ account_id: accountID, route: { cell: "source", endpoint: "https://source.invalid" } });
  const moving = claimOperation(live(), { kind: "move", operation_id: id, source_cell: "source", target_cell: "target", archive: { archive_id: id, object: `archives/${accountID}/${id}.tar.gz` } });
  Object.assign(moving.operation, { phase: "route_retired", request_epoch: 0, source_registration_id: "reg-source", target_registration_id: "reg-target", target_preflight: { protocol: 3 }, archive_origin: "https://origin.invalid", retryable: true, last_error: null,
    import_job: { attempts: 1, first_started_at: now, started_at: now, last_polled_at: null, retry_at: null, validated: { etag: "private-etag", manifest_schema_version: 98 } } });
  const exporting = structuredClone(moving);
  Object.assign(exporting.operation, { phase: "source_suspended", import_job: null,
    export_job: { stream_attempts: 2, verify_attempts: 1, stream_started_at: now - 1000,
      placement_policy: { rule: "private-export-policy-marker" },
      streamed: { etag: "private-export-etag-marker", size: 987654321, streamed_at: now, stream_ms: 1000 } } });
  const projecting = structuredClone(moving);
  projecting.projections = {
    route: { action: "put", status: "pending", epoch: projecting.epoch, value: { cell: "target", endpoint: "https://target.invalid" } },
    archive: { action: "delete", status: "pending", epoch: projecting.epoch, ...projecting.operation.archive },
    cleanup: { action: "delete", status: "pending", epoch: projecting.epoch, ...projecting.operation.archive },
  };
  const attention = structuredClone(moving);
  Object.assign(attention.operation, { last_error: "invalid or corrupt archive", retryable: false,
    imported_status: "suspended", restored_status: "active", source_finalization: { mode: "cell_receipt" },
    target_reservation: { expires_at: now + 60000 } });
  const completed = live(); completed.epoch = 1; completed.revision = 10;
  completed.last_completed = { operation_id: "restore-operation", evacuation_id: id, kind: "restore", outcome: "completed", epoch: 1, source_cell: "source", target_cell: "target", completed_revision: 10, archive: moving.operation.archive, final_location: { kind: "live", cell: "target" } };
  const quarantined = structuredClone(completed);
  quarantined.location = bootstrapArchivedState({ account_id: accountID, archived: { archive_id: id, object: moving.operation.archive.object, cell: "source", evacuation_id: id } }).location;
  quarantined.last_completed.outcome = "quarantined"; quarantined.last_completed.final_location = quarantined.location;
  for (const state of [moving, exporting, projecting, attention, completed, quarantined]) validateLifecycleState(state);
  const base = { accountID, now, alarmAt: null, driverActive: false, quarantine: null };
  return {
    import_running: { ...base, state: moving, alarmAt: now + 60_000, driverActive: true },
    export_running: { ...base, state: exporting, driverActive: true },
    projections_pending: { ...base, state: projecting },
    operation_attention: { ...base, state: attention },
    restore_completed: { ...base, state: completed },
    quarantined: { ...base, state: quarantined, quarantine: { archive_id: id, object: moving.operation.archive.object, quarantined_at: "2026-09-28T10:00:00+02:00" } },
    uninitialized_resident: { ...base, state: null },
  };
}
function fixture(input = lifecycleGoldenInputs().import_running) {
  const f = { calls: [], reads: [], writes: 0, input, values: new Map([[`acct:${accountID}`, { cell: "source" }]]) };
  f.env = { FLEET_TOKEN: "fixture-fleet", PUBLIC_IP_LIMITER: { limit: () => ({ success: true }) },
    DIRECTORY: { get: async (key, options) => { f.reads.push(key); assert.deepEqual(options, { type: "json" }); assert.ok(key.startsWith("acct:") || key.startsWith("archived:")); if (f.kvThrows) throw new Error("private KV"); return f.values.get(key) ?? null; }, put: () => { f.writes++; }, delete: () => { f.writes++; } },
    ACCOUNT_LIFECYCLE: { idFromName: (value) => { assert.equal(value, accountID); return value; }, get: () => ({ fetch: async (request) => {
      f.calls.push(request); assert.equal(request.method, "POST"); assert.equal(request.url, "https://account-lifecycle.internal/lifecycle-status"); assert.deepEqual([...request.headers], [["content-type", "application/json"]]); assert.deepEqual(await request.json(), { account_id: accountID });
      if (f.reply) return f.reply();
      return Response.json({ ok: true, account_id: accountID, status: projectLifecycleStatus(f.input) });
    } }) },
  };
  f.run = async ({ route = path, method = "GET", token = "fixture-fleet" } = {}) => {
    const response = await worker.fetch(new Request("https://cp.invalid" + route, { method, headers: { Authorization: `Bearer ${token}`, "CF-Connecting-IP": "192.0.2.53" } }), f.env, { waitUntil() {} });
    assert.equal(response.headers.get("Cache-Control"), "no-store"); assert.equal(f.writes, 0);
    return { status: response.status, body: await response.json() };
  };
  return f;
}
for (const [name, input] of Object.entries(lifecycleGoldenInputs())) {
  test(`lifecycle Worker golden ${name}`, async () => {
    const golden = JSON.parse(readFileSync(new URL("./fixtures/account-lifecycle-status.golden.json", import.meta.url), "utf8"));
    const f = fixture(input); const result = await f.run(); assert.equal(result.status, 200); assert.deepEqual(result.body, golden.cases[name]); assert.equal(f.calls.length, 1); assert.equal(f.reads.length, 2);
  });
}
test("lifecycle Worker authenticates before state and terminates malformed prefix paths", async () => {
  containerCalls.length = 0;
  for (const token of ["", "wrong"]) { const f = fixture(); const r = await f.run({ token }); assert.equal(r.status, 401); assert.equal(r.body.error, "unauthorized"); assert.equal(f.calls.length + f.reads.length, 0); }
  for (const route of ["/v1/placement/accounts/encoded%20space/lifecycle", `/v1/placement/accounts/${"a".repeat(129)}/lifecycle`, "/v1/placement/accounts/x/other"]) { const f = fixture(); const r = await f.run({ route }); assert.equal(r.status, 404); assert.equal(r.body.error, "account lifecycle route not found"); assert.equal(f.calls.length + f.reads.length, 0); }
  assert.deepEqual(containerCalls, []);
});
test("lifecycle Worker rejects methods, queries and shared limiter before reads", async (t) => {
  t.mock.method(console, "log", () => {});
  for (const method of ["POST", "DELETE"]) { const f = fixture(); const r = await f.run({ method }); assert.equal(r.status, 405); assert.equal(r.body.error, "method not allowed"); assert.equal(f.calls.length + f.reads.length, 0); }
  const f = fixture(); assert.equal((await f.run({ route: path + "?x=1" })).status, 400); assert.equal(f.calls.length + f.reads.length, 0);
  f.env.PUBLIC_IP_LIMITER.limit = () => ({ success: false }); assert.equal((await f.run()).status, 429); assert.equal(f.calls.length + f.reads.length, 0);
});
test("lifecycle Worker collapses all backend failures and drops unexpected top-level fields", async () => {
  const valid = () => ({ ok: true, account_id: accountID, status: projectLifecycleStatus(lifecycleGoldenInputs().import_running) });
  for (const reply of [() => { throw new Error("private-marker"); }, () => Response.json({ error: "private-marker" }, { status: 503 }), () => new Response("private-marker"), () => Response.json({ ...valid(), ok: false }), () => Response.json({ ...valid(), account_id: "wrong" }), () => Response.json({ ...valid(), status: null }), () => { const v = valid(); v.status.account_id = "wrong"; return Response.json(v); }, () => { const v = valid(); v.status.initialized = "true"; return Response.json(v); }]) {
    const f = fixture(); f.reply = reply; const r = await f.run(); assert.equal(r.status, 503); assert.equal(r.body.error, "account lifecycle status is unavailable"); assert.equal(JSON.stringify(r).includes("private-marker"), false); assert.equal(f.reads.length, 0);
  }
  const f = fixture(); f.reply = () => { const v = valid(); v.status.future = "private-marker"; return Response.json(v); }; const r = await f.run(); assert.equal(r.status, 200); assert.equal(Object.hasOwn(r.body, "future"), false);
});
test("lifecycle Worker distinguishes absent residents, unavailable directory and invalid cells", async () => {
  const f = fixture(lifecycleGoldenInputs().uninitialized_resident); f.values.clear(); let r = await f.run(); assert.equal(r.status, 404); assert.equal(r.body.error, "unknown account");
  f.values.set(`acct:${accountID}`, { cell: "source" }); r = await f.run(); assert.equal(r.status, 200); assert.equal(r.body.directory.live_cell, "source");
  f.kvThrows = true; assert.equal((await f.run()).status, 503);
  f.input = lifecycleGoldenInputs().import_running; r = await f.run(); assert.equal(r.status, 200); assert.equal(r.body.directory, null);
  f.kvThrows = false; f.values.set(`acct:${accountID}`, { cell: "private/bad" }); f.values.set(`archived:${accountID}`, { source_cell: "archive-source" }); r = await f.run(); assert.deepEqual(r.body.directory, { live_cell: null, archived_cell: "archive-source" });
  f.values.set(`archived:${accountID}`, { cell: "bad space" }); assert.equal((await f.run()).body.directory.archived_cell, null);
});


test("lifecycle Worker omits private export metadata and forbidden keys", async () => {
  const input = lifecycleGoldenInputs().export_running;
  const job = input.state.operation.export_job;
  const markers = [job.streamed.etag, String(job.streamed.size), job.placement_policy.rule];
  const result = await fixture(input).run();
  assert.equal(result.status, 200);
  for (const output of [projectLifecycleStatus(input), result.body]) {
    const encoded = JSON.stringify(output);
    for (const marker of markers) assert.equal(encoded.includes(marker), false, "private export metadata is absent");
    function inspect(value) {
      if (!value || typeof value !== "object") return;
      for (const [key, child] of Object.entries(value)) {
        assert.ok(!["etag", "size", "placement_policy"].includes(key), "private export key is absent");
        inspect(child);
      }
    }
    inspect(output);
  }
});
