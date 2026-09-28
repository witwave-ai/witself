import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { bootstrapLiveState, claimOperation, validateLifecycleState } from "../src/account-lifecycle-state.mjs";
import { projectLifecycleStatus, LIFECYCLE_STATUS_ERRORS as errors, LIFECYCLE_STATUS_FINALIZATION_MODES as modes } from "../src/account-lifecycle-status.mjs";

const now = Date.parse("2026-09-28T12:00:00Z");
const id = "11111111-1111-4111-8111-111111111111";
const keys = (v) => Object.keys(v).sort();
const topKeys = "account_id observed_at initialized revision epoch location driver_active alarm_at operation projections last_completed restore_quarantine".split(" ").sort();
const opKeys = "operation_id evacuation_id kind phase epoch request_epoch source_cell target_cell source_registration_id target_registration_id target_protocol has_archive has_archive_origin next_step imported_status restored_status source_finalization target_reservation_expires_at retryable last_error export_job import_job".split(" ").sort();
const jobKeys = "attempts first_started_at started_at last_polled_at retry_at validated manifest_schema_version next_alarm_action".split(" ").sort();
function live() { return bootstrapLiveState({ account_id: "acct_runtime", route: { cell: "source", endpoint: "https://source.invalid" } }); }
function moving(kind = "move") {
  return claimOperation(live(), { kind, operation_id: id, source_cell: "source", target_cell: kind === "close" ? null : "target", archive: kind === "close" ? null : { archive_id: id, object: `archives/acct_runtime/${id}.tar.gz` } });
}
function project(state, extra = {}) { return projectLifecycleStatus({ accountID: "acct_runtime", state, now, ...extra }); }
function jobState() {
  const state = moving(); state.operation.phase = "route_retired";
  state.operation.import_job = { attempts: 1, first_started_at: now, started_at: now, last_polled_at: null, retry_at: null, validated: { etag: "private-etag", manifest_schema_version: 98 } };
  return state;
}

test("lifecycle projection exact closed key sets across uninitialized, idle, move, restore and close", () => {
  for (const state of [null, live(), jobState(), moving("close")]) {
    const result = project(state); assert.deepEqual(keys(result), topKeys);
    if (result.location) assert.deepEqual(keys(result.location), ["cell", "kind"]);
    if (result.operation) { assert.deepEqual(keys(result.operation), opKeys); if (result.operation.import_job) assert.deepEqual(keys(result.operation.import_job), jobKeys); }
    if (result.projections) assert.deepEqual(keys(result.projections), ["archive", "cleanup", "route"]);
  }
  const close = project(moving("close")).operation; assert.equal(close.evacuation_id, null); assert.equal(close.target_cell, null);
  for (const [retry, action] of [[now + 1, "wait"], [now - 1, "start"]]) {
    const state = jobState(); state.operation.kind = "restore"; state.operation.phase = "target_reserved"; state.operation.import_job.retry_at = retry;
    assert.deepEqual(keys(project(state).operation), opKeys); assert.equal(project(state).operation.import_job.next_alarm_action, action);
  }
  for (const outcome of ["completed", "aborted", "reaped", "cancelled", "quarantined"]) {
    const state = live(); state.last_completed = { operation_id: id, evacuation_id: id, kind: "restore", outcome, epoch: 0, source_cell: "source", target_cell: "target", completed_revision: 0, final_location: { kind: "live", cell: "target" } };
    const c = project(state).last_completed; assert.deepEqual(keys(c), "operation_id evacuation_id kind outcome epoch source_cell target_cell completed_revision final_location".split(" ").sort()); assert.equal(c.outcome, outcome);
  }
});

test("lifecycle projection realistic archive omits all private payloads but deliberately retains D3 identifiers", () => {
  const state = jobState(); const markers = [];
  function marker(name) { const value = `private-${name}-marker`; markers.push(value); return value; }
  const op = state.operation;
  op.archive_origin = marker("origin"); op.target_preflight = { protocol: 3, endpoint: marker("target") };
  op.reason = marker("reason"); op.placement_policy = { rule: marker("policy") };
  op.target_reservation = { receipt: marker("reservation") }; op.source_finalization = { mode: "cell_receipt", receipt: marker("source") };
  op.export_job = { streamed: { etag: marker("export-etag"), size: marker("export-size") }, placement_policy: marker("export-policy") };
  op.future = marker("future"); op.import_job.validated.etag = marker("etag");
  op.capability = "capability-" + "a".repeat(64); markers.push(op.capability);
  state.location.route.endpoint = marker("route"); state.location.pointer = { object: marker("pointer") };
  state.projections = {
    route: { action: "put", status: "pending", epoch: state.epoch, value: { cell: "target", endpoint: marker("route-projection") } },
    archive: { action: "put", status: "pending", epoch: state.epoch, ...op.archive, value: { ...op.archive, cell: "source", extra: marker("archive-projection") } },
    cleanup: { action: "delete", status: "pending", epoch: state.epoch, ...op.archive, value: marker("cleanup-projection") },
  };
  state.last_completed = { operation_id: id, evacuation_id: id, kind: "move", outcome: "completed", epoch: 1, source_cell: "source", target_cell: "target", completed_revision: 1, archive: { archive_id: id, object: marker("completed-archive") }, final_location: { kind: "archived", source_cell: "source", object: marker("completed-object") } };
  validateLifecycleState(state);
  const output = project(state); const encoded = JSON.stringify(output);
  for (const m of markers) assert.equal(encoded.includes(m), false);
  assert.equal(encoded.includes("archives/"), false); assert.equal(encoded.includes(".tar.gz"), false);
  assert.ok(encoded.includes(id)); // D3 intentionally discloses the operation/archive identity, never the object key.
  function inspect(v) { if (!v || typeof v !== "object") return; for (const [key, child] of Object.entries(v)) { assert.ok(!["archive_id", "object", "etag", "size", "endpoint", "reason", "placement_policy", "archive_origin"].includes(key)); inspect(child); } }
  inspect(output);
  op.abort_receipt = { secret: marker("abort") }; assert.ok(!JSON.stringify(project(state)).includes(markers.at(-1)));
});

test("lifecycle errors are a closed fixed set", () => {
  const state = jobState();
  for (const error of errors) { state.operation.last_error = error; assert.equal(project(state).operation.last_error, error); }
  state.operation.last_error = "private failure"; assert.equal(project(state).operation.last_error, "unrecognized lifecycle error");
  for (const value of [null, undefined, "", {}, 1]) { state.operation.last_error = value; assert.equal(project(state).operation.last_error, null); }
  assert.equal(Object.isFrozen(errors), true); assert.equal(Object.isFrozen(modes), true);
});

test("lifecycle timestamps canonicalize and reject unsafe stored text or years", () => {
  const state = jobState();
  for (const value of [NaN, -1, 1e20, 253402300800000, "not-date", "https://private.invalid/x 2026", "marker 1", "2026-09-28", {}]) {
    state.operation.import_job.first_started_at = value; state.operation.target_reservation = { expires_at: value };
    const result = project(state, { now: value, alarmAt: value, quarantine: { quarantined_at: value } });
    assert.equal(result.observed_at, null); assert.equal(result.alarm_at, null); assert.equal(result.operation.import_job.first_started_at, null); assert.equal(result.operation.target_reservation_expires_at, null); assert.equal(result.restore_quarantine.quarantined_at, null);
  }
  for (const value of [253402300799999, "2026-09-28T10:00:00.123+02:00"]) {
    const result = project(null, { now: value }); assert.ok(result.observed_at.endsWith("Z")); assert.notEqual(result.observed_at, value); assert.ok(!result.observed_at.startsWith("+"));
  }
});

test("lifecycle job type rules preserve null retry polling", () => {
  const state = jobState();
  for (const value of [null, undefined, "0", {}]) { state.operation.import_job.retry_at = value; assert.equal(project(state).operation.import_job.next_alarm_action, "poll"); }
  for (const value of ["1", -1, 1.5]) { state.operation.import_job.attempts = value; state.operation.import_job.validated.manifest_schema_version = value; const job = project(state).operation.import_job; assert.equal(job.attempts, null); assert.equal(job.manifest_schema_version, null); }
  for (const value of ["with space", "with/slash"]) { state.operation.source_registration_id = value; state.operation.target_registration_id = value; assert.equal(project(state).operation.source_registration_id, null); assert.equal(project(state).operation.target_registration_id, null); }
  state.operation.phase = "private/phase"; assert.equal(project(state).operation.phase, "unknown"); assert.equal(project(state).operation.next_step, null);
});

test("lifecycle quarantine compares operation and settled location without identifiers", () => {
  const state = jobState(); const q = { archive_id: id, object: state.operation.archive.object, quarantined_at: "2026-09-28T10:00:00Z" };
  assert.equal(project(state, { quarantine: q }).restore_quarantine.matches_operation, true);
  state.location = { kind: "archived", source_cell: "source", archive_id: id, object: q.object }; state.operation = null;
  const result = project(state, { quarantine: q }).restore_quarantine;
  assert.deepEqual(keys(result), ["matches_location", "matches_operation", "quarantined_at", "reason"]);
  assert.equal(result.matches_location, true); assert.equal(result.matches_operation, false);
  assert.deepEqual(project(state, { quarantine: { invalid: true } }).restore_quarantine, { reason: "unreadable", quarantined_at: null, matches_operation: false, matches_location: false });
});

test("lifecycle projection tolerates every state-machine phase", () => {
  const source = readFileSync(new URL("../src/account-lifecycle-state.mjs", import.meta.url), "utf8");
  for (const [, phase] of source.matchAll(/case "([a-z_]+)":/g)) {
    const state = jobState(); state.operation.phase = phase;
    assert.doesNotThrow(() => project(state));
  }
});

test("lifecycle runtime error and finalization tripwire closes every persistence site", () => {
  const source = readFileSync(new URL("../src/account-lifecycle-runtime.mjs", import.meta.url), "utf8");
  const sites = [...source.matchAll(/\blast_error:\s*([^\n]+)/g)].map((m) => m[1]);
  assert.equal(sites.length, 10);
  const counts = { null: 0, literal: 0, message: 0, body: 0, exhausted: 0 };
  const captured = [];
  for (const rhs of sites) {
    if (/^null\s*[,}]/.test(rhs)) counts.null++;
    else if (/^"([^"\n]+)"\s*[,}]/.test(rhs)) { counts.literal++; captured.push(rhs.match(/^"([^"\n]+)"/)[1]); }
    else if (/^message\s*[,}]/.test(rhs)) counts.message++;
    else if (/^body\.error\s*[,}]/.test(rhs)) counts.body++;
    else if (/^exhausted \? "([^"]+)" : message\s*[,}]/.test(rhs)) { counts.exhausted++; captured.push(rhs.match(/^exhausted \? "([^"]+)"/)[1]); }
    else assert.fail("unclassified persistence expression");
  }
  assert.deepEqual(counts, { null: 4, literal: 3, message: 1, body: 1, exhausted: 1 });
  captured.push(...[...source.matchAll(/backoff\("([^"]+)"\)/g)].map((m) => m[1]));
  const defaultMatch = source.match(/const backoff = async \(message = "([^"]+)"\)/); assert.ok(defaultMatch); captured.push(defaultMatch[1]);
  const classification = source.match(/const message = (\[[\s\S]*?\? error\.message : "[^"\n]+")\s*;/); assert.ok(classification);
  const classificationStrings = [...classification[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]); assert.equal(classificationStrings.length, 3); captured.push(...classificationStrings);
  const permanent = source.match(/const permanent = \[([^\]]+)\]/); assert.ok(permanent);
  const permanentStrings = [...permanent[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]); assert.equal(permanentStrings.length, 3); captured.push(...permanentStrings);
  for (const literal of captured) assert.ok(errors.includes(literal));
  for (const literal of errors) assert.ok(source.includes(JSON.stringify(literal)));
  assert.deepEqual([...new Set([...source.matchAll(/mode: "([^"]+)"/g)].map((m) => m[1]))].sort(), [...modes].sort());
});

test("lifecycle completed location independently closes unvalidated metadata", () => {
  const state = live();
  state.last_completed = { operation_id: id, evacuation_id: id, kind: "restore", outcome: "completed", epoch: 0, source_cell: "source", target_cell: "target", completed_revision: 0, archive: null, final_location: { kind: "private/metadata", cell: "private/endpoint" } };
  validateLifecycleState(state);
  assert.equal(project(state).last_completed.final_location, null);
  state.last_completed.final_location.kind = "live";
  assert.deepEqual(project(state).last_completed.final_location, { kind: "live", cell: null });
});


test("lifecycle export job closes keys and degrades malformed metadata", () => {
  const state = moving();
  const empty = { stream_attempts: null, verify_attempts: null, stream_started_at: null, streamed: false, streamed_at: null, stream_ms: null };
  for (const value of [undefined, null, false, 1, "private-job", []]) {
    state.operation.export_job = value;
    assert.equal(project(state).operation.export_job, null);
  }
  const job = state.operation.export_job = {};
  assert.deepEqual(project(state).operation.export_job, empty);
  for (const value of [undefined, null, false, [], {}, "private-metadata", -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
    Object.assign(job, { stream_attempts: value, verify_attempts: value, stream_started_at: value, streamed: { streamed_at: value, stream_ms: value } });
    assert.deepEqual(project(state).operation.export_job, { ...empty, streamed: true });
  }
  for (const value of [undefined, null, true, 1, "private-stream", []]) {
    job.streamed = value;
    assert.deepEqual(project(state).operation.export_job, empty);
  }
  Object.assign(job, { stream_attempts: 0, verify_attempts: 3, stream_started_at: now, streamed: { streamed_at: "2026-09-28T15:00:01+03:00", stream_ms: 1000 } });
  assert.deepEqual(project(state).operation.export_job, {
    stream_attempts: 0, verify_attempts: 3, stream_started_at: "2026-09-28T12:00:00.000Z",
    streamed: true, streamed_at: "2026-09-28T12:00:01.000Z", stream_ms: 1000,
  });
});

test("lifecycle projection entries have exactly action and status", () => {
  const state = moving();
  state.projections = {
    route: { action: "put", status: "pending", epoch: state.epoch, value: { cell: "target", endpoint: "https://target.invalid" } },
    archive: { action: "delete", status: "pending", epoch: state.epoch, ...state.operation.archive },
    cleanup: { action: "delete", status: "pending", epoch: state.epoch, ...state.operation.archive },
  };
  validateLifecycleState(state);
  for (const entry of Object.values(project(state).projections)) assert.deepEqual(keys(entry), ["action", "status"]);
});
