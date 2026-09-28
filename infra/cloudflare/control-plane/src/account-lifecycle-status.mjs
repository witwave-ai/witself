import { nextLifecycleStep } from "./account-lifecycle-state.mjs";

export const LIFECYCLE_STATUS_ERRORS = Object.freeze([
  "archive import above 90 MiB requires target account evacuation protocol 2",
  "target cell schema is older than the archive; upgrade the target cell first",
  "archive import failed; retry required",
  "target cell attests protocol 2 while an import job is in flight; waiting for the roll",
  "import job ended without a receipt; retry required",
  "import job exhausted its attempts; operator attention required",
  "import capacity exhausted; retry scheduled",
  "import job exceeded its age limit; operator attention required",
  "account exists under a different evacuation",
  "archive schema is newer than this cell — upgrade the cell first",
  "invalid or corrupt archive",
]);
export const LIFECYCLE_STATUS_UNRECOGNIZED_ERROR = "unrecognized lifecycle error";
export const LIFECYCLE_STATUS_FINALIZATION_MODES = Object.freeze([
  "same_cell_target", "legacy_unfenced_source", "source_cell_unregistered",
  "source_cell_replaced", "cell_receipt",
]);
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const counter = (v) => Number.isSafeInteger(v) && v >= 0 ? v : null;
const word = (v) => typeof v === "string" && /^[a-z_]{1,64}$/.test(v) ? v : null;
const registration = (v) => typeof v === "string" && /^[A-Za-z0-9._:-]{1,256}$/.test(v) ? v : null;
const status = (v) => ["active", "suspended", "closed"].includes(v) ? v : null;
function timestamp(value) {
  let ms = value;
  if (typeof value === "string") {
    if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return null;
    ms = Date.parse(value);
  }
  return Number.isSafeInteger(ms) && ms >= 0 && ms <= 253402300799999
    ? new Date(ms).toISOString() : null;
}
function location(value) {
  if (!object(value) || !["live", "archived", "closed_archived", "closed"].includes(value.kind)) return null;
  const cell = value.kind === "live" ? value.cell :
    ["archived", "closed_archived"].includes(value.kind) ? value.source_cell : null;
  return { kind: value.kind, cell: typeof cell === "string" && /^[a-z0-9-]{1,64}$/.test(cell) ? cell : null };
}
function projection(value) {
  return value == null ? null : { action: value.action, status: value.status };
}
function nextStep(state) {
  try {
    const step = nextLifecycleStep(state);
    return { type: word(step.type), action: word(step.action ?? step.projection?.action), target: word(step.target) };
  } catch { return null; }
}
function exportJob(job) {
  if (!object(job)) return null;
  const streamed = object(job.streamed) ? job.streamed : null;
  return {
    stream_attempts: counter(job.stream_attempts),
    verify_attempts: counter(job.verify_attempts),
    stream_started_at: timestamp(job.stream_started_at),
    streamed: streamed !== null,
    streamed_at: timestamp(streamed?.streamed_at),
    stream_ms: counter(streamed?.stream_ms),
  };
}
function importJob(job, now) {
  if (!object(job)) return null;
  return {
    attempts: counter(job.attempts),
    first_started_at: timestamp(job.first_started_at),
    started_at: timestamp(job.started_at),
    last_polled_at: timestamp(job.last_polled_at),
    retry_at: timestamp(job.retry_at),
    validated: object(job.validated),
    manifest_schema_version: counter(job.validated?.manifest_schema_version),
    next_alarm_action: typeof job.retry_at === "number" ? (job.retry_at <= now ? "start" : "wait") : "poll",
  };
}
function operation(state, now) {
  const op = state?.operation;
  if (!op) return null;
  return {
    operation_id: op.operation_id,
    evacuation_id: op.kind === "close" ? null : op.evacuation_id,
    kind: op.kind,
    phase: word(op.phase) ?? "unknown",
    epoch: counter(op.epoch),
    request_epoch: counter(op.request_epoch),
    source_cell: op.source_cell,
    target_cell: op.target_cell,
    source_registration_id: registration(op.source_registration_id),
    target_registration_id: registration(op.target_registration_id),
    target_protocol: Number.isSafeInteger(op.target_preflight?.protocol) && op.target_preflight.protocol >= 1 && op.target_preflight.protocol <= 99 ? op.target_preflight.protocol : null,
    has_archive: object(op.archive),
    has_archive_origin: typeof op.archive_origin === "string" && op.archive_origin.length > 0,
    next_step: nextStep(state),
    imported_status: status(op.imported_status),
    restored_status: status(op.restored_status),
    source_finalization: LIFECYCLE_STATUS_FINALIZATION_MODES.includes(op.source_finalization?.mode) ? op.source_finalization.mode : null,
    target_reservation_expires_at: timestamp(op.target_reservation?.expires_at),
    retryable: typeof op.retryable === "boolean" ? op.retryable : null,
    last_error: typeof op.last_error !== "string" || op.last_error === "" ? null : LIFECYCLE_STATUS_ERRORS.includes(op.last_error) ? op.last_error : LIFECYCLE_STATUS_UNRECOGNIZED_ERROR,
    export_job: exportJob(op.export_job),
    import_job: importJob(op.import_job, now),
  };
}
function completed(value) {
  if (!value) return null;
  return {
    operation_id: value.operation_id, evacuation_id: value.evacuation_id,
    kind: value.kind, outcome: value.outcome, epoch: counter(value.epoch),
    source_cell: value.source_cell, target_cell: value.target_cell,
    completed_revision: counter(value.completed_revision), final_location: location(value.final_location),
  };
}
function restoreQuarantine(value, state) {
  if (!value) return null;
  if (value.invalid === true) return { reason: "unreadable", quarantined_at: null, matches_operation: false, matches_location: false };
  const matches = (archive) => Boolean(archive && archive.archive_id === value.archive_id && archive.object === value.object);
  return {
    reason: "archive_integrity", quarantined_at: timestamp(value.quarantined_at),
    matches_operation: matches(state?.operation?.archive),
    matches_location: ["archived", "closed_archived"].includes(state?.location?.kind) && matches(state.location),
  };
}
export function projectLifecycleStatus({ accountID, state, quarantine, alarmAt, driverActive, now }) {
  return {
    account_id: accountID, observed_at: timestamp(now), initialized: state !== null,
    revision: counter(state?.revision), epoch: counter(state?.epoch), location: location(state?.location),
    driver_active: driverActive === true, alarm_at: timestamp(alarmAt),
    operation: operation(state, now),
    projections: state === null ? null : { route: projection(state.projections.route), archive: projection(state.projections.archive), cleanup: projection(state.projections.cleanup) },
    last_completed: completed(state?.last_completed), restore_quarantine: restoreQuarantine(quarantine, state),
  };
}
