export const ACCOUNT_LIFECYCLE_STATE_SCHEMA =
  "witself.account-lifecycle.v1";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const OPAQUE_ID = /^[A-Za-z0-9._:-]{1,256}$/;
const EVACUATION_ID = /^[A-Za-z0-9_-]{1,128}$/;
const ARCHIVE_OBJECT_MAX_LENGTH = 1024;

const OPERATION_KINDS = new Set([
  "evacuate",
  "restore",
  "move",
  "close",
]);
const LOCATION_KINDS = new Set([
  "live",
  "archived",
  "closed",
  "closed_archived",
]);
const PROJECTION_STATUSES = new Set(["pending", "applied"]);

const ACKNOWLEDGEMENT_TRANSITIONS = Object.freeze({
  evacuate: new Set([
    "claimed->source_suspended",
  ]),
  restore: new Set([
    "claimed->target_reserved",
    "target_reserved->target_imported",
    "target_imported->target_resumed",
    "route_projected->source_finalized",
    "closed_archive_projected->closed_archive_retained",
  ]),
  move: new Set([
    "claimed->target_reserved",
    "target_reserved->source_suspended",
    "route_retired->target_imported",
    "target_imported->target_resumed",
    "route_projected->source_finalized",
    "closed_archive_projected->closed_archive_retained",
  ]),
  close: new Set([
    "claimed->source_closed",
  ]),
});

const COMPLETABLE_PHASE = Object.freeze({
  evacuate: "route_retired",
  restore: "archive_cleaned",
  move: "archive_cleaned",
  close: "route_retired",
});

export class AccountLifecycleStateError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "AccountLifecycleStateError";
    this.code = code;
  }
}

function fail(code, message) {
  throw new AccountLifecycleStateError(code, message);
}

function isObject(value) {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value)
  );
}

function cloneJSON(value, label) {
  if (value === undefined) {
    fail("invalid-json", `${label} must be JSON-serializable`);
  }
  let encoded;
  try {
    encoded = JSON.stringify(value);
  } catch {
    fail("invalid-json", `${label} must be JSON-serializable`);
  }
  if (encoded === undefined) {
    fail("invalid-json", `${label} must be JSON-serializable`);
  }
  const cloned = JSON.parse(encoded);
  if (!isObject(cloned)) {
    fail("invalid-json", `${label} must be a JSON object`);
  }
  return cloned;
}

function assertAccountID(accountID) {
  if (typeof accountID !== "string" || !ACCOUNT_ID.test(accountID)) {
    fail("invalid-account-id", "account_id is invalid");
  }
}

function assertCellName(cell, label) {
  if (typeof cell !== "string" || !CELL_NAME.test(cell)) {
    fail("invalid-cell", `${label} is invalid`);
  }
}

function assertOpaqueID(value, label) {
  if (typeof value !== "string" || !OPAQUE_ID.test(value)) {
    fail("invalid-id", `${label} is invalid`);
  }
}

function assertEvacuationID(value, label = "evacuation_id") {
  if (typeof value !== "string" || !EVACUATION_ID.test(value)) {
    fail("invalid-evacuation-id", `${label} is invalid`);
  }
}

function assertArchiveObject(value) {
  if (
    typeof value !== "string" ||
    value.length < 1 ||
    value.length > ARCHIVE_OBJECT_MAX_LENGTH ||
    /[\u0000-\u001f\u007f]/.test(value)
  ) {
    fail("invalid-archive-object", "archive object is invalid");
  }
}

function assertArchiveID(value, label = "archive_id") {
  if (
    typeof value !== "string" ||
    value.length < 1 ||
    value.length > ARCHIVE_OBJECT_MAX_LENGTH ||
    /[\u0000-\u001f\u007f]/.test(value)
  ) {
    fail("invalid-id", `${label} is invalid`);
  }
}

function assertCounter(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) {
    fail("invalid-state", `${label} must be a non-negative safe integer`);
  }
}

function assertArchiveRef(archive, label = "archive") {
  if (!isObject(archive)) {
    fail("invalid-archive", `${label} is required`);
  }
  assertArchiveID(archive.archive_id, `${label}.archive_id`);
  assertArchiveObject(archive.object);
}

function archiveRef(archive) {
  assertArchiveRef(archive);
  return {
    archive_id: archive.archive_id,
    object: archive.object,
  };
}

function sameArchive(left, right) {
  return (
    left?.archive_id === right?.archive_id &&
    left?.object === right?.object
  );
}

function sameOperationIdentity(operation, claim) {
  return (
    operation.operation_id === claim.operation_id &&
    operation.evacuation_id === claim.evacuation_id &&
    operation.kind === claim.kind &&
    operation.source_cell === claim.source_cell &&
    operation.target_cell === claim.target_cell &&
    (
      operation.archive === null
        ? claim.archive === null
        : sameArchive(operation.archive, claim.archive)
    )
  );
}

function currentOperation(state, operationID) {
  assertOpaqueID(operationID, "operation_id");
  if (!state.operation) {
    if (state.last_completed?.operation_id === operationID) {
      fail(
        "operation-completed",
        `operation ${operationID} is already complete`,
      );
    }
    fail("no-operation", "no lifecycle operation is active");
  }
  if (state.operation.operation_id !== operationID) {
    fail(
      "stale-operation",
      `operation ${operationID} does not own the active lifecycle epoch`,
    );
  }
  return state.operation;
}

function bump(state, changes) {
  return {
    ...state,
    ...changes,
    revision: state.revision + 1,
  };
}

function emptyProjections() {
  return {
    route: null,
    archive: null,
    cleanup: null,
  };
}

function routeProjection(action, epoch, value = undefined) {
  const projection = {
    action,
    status: "pending",
    epoch,
  };
  if (action === "put") {
    projection.value = cloneJSON(value, "route projection");
  }
  return projection;
}

function archiveProjection(action, epoch, archive, value = undefined) {
  const projection = {
    action,
    status: "pending",
    epoch,
    ...archiveRef(archive),
  };
  if (action === "put") {
    projection.value = cloneJSON(value, "archive projection");
  }
  return projection;
}

function cleanupProjection(epoch, archive) {
  return {
    action: "delete",
    status: "pending",
    epoch,
    ...archiveRef(archive),
  };
}

function normalizeLiveLocation(routeInput) {
  const route = cloneJSON(routeInput, "route");
  assertCellName(route.cell, "route.cell");
  return {
    kind: "live",
    cell: route.cell,
    route,
  };
}

function normalizeArchivedLocation(archivedInput, explicitArchiveID) {
  const pointer = cloneJSON(archivedInput, "archived");
  assertArchiveObject(pointer.object);
  const archiveID =
    explicitArchiveID ??
    pointer.archive_id ??
    pointer.object;
  assertArchiveID(archiveID, "archive_id");
  if (
    pointer.archive_id !== undefined &&
    pointer.archive_id !== archiveID
  ) {
    fail(
      "archive-mismatch",
      "archived.archive_id does not match archive_id",
    );
  }
  const sourceCell = pointer.cell ?? pointer.source_cell;
  assertCellName(sourceCell, "archived source cell");
  return {
    kind: "archived",
    archive_id: archiveID,
    object: pointer.object,
    source_cell: sourceCell,
    pointer,
  };
}

function baseState(accountID, location) {
  assertAccountID(accountID);
  const projectedEpoch = location.kind === "live"
    ? location.route?.epoch
    : location.pointer?.epoch;
  const epoch = Number.isSafeInteger(projectedEpoch) && projectedEpoch >= 0
    ? projectedEpoch
    : 0;
  return {
    schema_version: ACCOUNT_LIFECYCLE_STATE_SCHEMA,
    account_id: accountID,
    revision: 0,
    epoch,
    location,
    operation: null,
    projections: emptyProjections(),
    last_completed: null,
  };
}

/**
 * Bootstrap durable authority from a currently routed account. This is the
 * only operation that trusts a KV route without an existing durable state.
 */
export function bootstrapLiveState({ account_id: accountID, route }) {
  return baseState(accountID, normalizeLiveLocation(route));
}

/**
 * Bootstrap durable authority from an existing archive pointer. Legacy
 * pointers without archive_id use their immutable object key as the id.
 */
export function bootstrapArchivedState({
  account_id: accountID,
  archived,
  archive_id: archiveID,
}) {
  return baseState(
    accountID,
    normalizeArchivedLocation(archived, archiveID),
  );
}

/**
 * Adopt a replacement archived projection after its new immutable object has
 * been validated by the runtime. This is deliberately limited to an idle
 * archived account: an in-flight lifecycle epoch or closed tombstone cannot
 * be rewritten by an eventually-consistent KV observation.
 */
export function replaceArchivedLocation(
  stateInput,
  {
    archived,
    archive_id: archiveID,
  },
) {
  const state = validateLifecycleState(stateInput);
  if (state.operation !== null) {
    fail(
      "operation-busy",
      `operation ${state.operation.operation_id} already owns the lifecycle`,
    );
  }
  if (state.location.kind !== "archived") {
    fail(
      "invalid-location",
      "archive replacement requires an authoritative archived location",
    );
  }
  const location = normalizeArchivedLocation(archived, archiveID);
  if (
    sameArchive(location, state.location) &&
    JSON.stringify(location.pointer) ===
      JSON.stringify(state.location.pointer)
  ) {
    return state;
  }
  const projectedEpoch = location.pointer?.epoch;
  return validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    epoch:
      Number.isSafeInteger(projectedEpoch) && projectedEpoch >= 0
        ? Math.max(state.epoch, projectedEpoch)
        : state.epoch,
    location,
  });
}

function assertProjection(projection, target, operation) {
  if (projection === null) {
    return;
  }
  if (!isObject(projection)) {
    fail("invalid-state", `${target} projection must be an object or null`);
  }
  if (
    !["put", "delete"].includes(projection.action) ||
    !PROJECTION_STATUSES.has(projection.status)
  ) {
    fail("invalid-state", `${target} projection has an invalid action/status`);
  }
  if (projection.epoch !== operation.epoch) {
    fail("invalid-state", `${target} projection has a stale epoch`);
  }
  if (target === "route") {
    if (projection.action === "put") {
      normalizeLiveLocation(projection.value);
    } else if (projection.value !== undefined) {
      fail("invalid-state", "route delete projection cannot carry a value");
    }
    return;
  }
  assertArchiveRef(projection, `${target} projection`);
  if (!sameArchive(projection, operation.archive)) {
    fail(
      "invalid-state",
      `${target} projection does not match the active archive`,
    );
  }
  if (target === "archive") {
    if (projection.action === "put") {
      const projected = normalizeArchivedLocation(
        projection.value,
        projection.archive_id,
      );
      if (!sameArchive(projected, projection)) {
        fail(
          "invalid-state",
          "archive put projection value does not match its archive",
        );
      }
    } else if (projection.value !== undefined) {
      fail("invalid-state", "archive delete projection cannot carry a value");
    }
  } else if (projection.action !== "delete") {
    fail("invalid-state", "archive cleanup must be a delete");
  }
}

/**
 * Validate a value read from Durable Object storage before using it.
 * The function returns the same plain object so callers can use it inline.
 */
export function validateLifecycleState(state) {
  if (!isObject(state)) {
    fail("invalid-state", "lifecycle state must be an object");
  }
  if (state.schema_version !== ACCOUNT_LIFECYCLE_STATE_SCHEMA) {
    fail("invalid-state", "unsupported lifecycle state schema");
  }
  assertAccountID(state.account_id);
  assertCounter(state.revision, "revision");
  assertCounter(state.epoch, "epoch");
  if (!isObject(state.location) || !LOCATION_KINDS.has(state.location.kind)) {
    fail("invalid-state", "location is invalid");
  }
  if (state.location.kind === "live") {
    const normalized = normalizeLiveLocation(state.location.route);
    if (
      state.location.cell !== normalized.cell ||
      state.location.cell !== state.location.route.cell
    ) {
      fail("invalid-state", "live location cell does not match its route");
    }
  } else if (
    state.location.kind === "archived" ||
    state.location.kind === "closed_archived"
  ) {
    const normalized = normalizeArchivedLocation(
      state.location.pointer,
      state.location.archive_id,
    );
    if (
      state.location.object !== normalized.object ||
      state.location.source_cell !== normalized.source_cell
    ) {
      fail(
        "invalid-state",
        "archived location does not match its pointer",
      );
    }
    if (
      state.location.kind === "closed_archived" &&
      state.location.pointer.status !== "closed"
    ) {
      fail(
        "invalid-state",
        "closed archived location must carry a closed archive pointer",
      );
    }
  } else if (Object.keys(state.location).length !== 1) {
    fail("invalid-state", "closed location cannot carry live/archive data");
  }

  if (!isObject(state.projections)) {
    fail("invalid-state", "projections must be an object");
  }
  const projectionKeys = Object.keys(state.projections).sort();
  if (
    projectionKeys.length !== 3 ||
    projectionKeys[0] !== "archive" ||
    projectionKeys[1] !== "cleanup" ||
    projectionKeys[2] !== "route"
  ) {
    fail(
      "invalid-state",
      "projections must contain route, archive, and cleanup",
    );
  }

  if (state.operation === null) {
    if (
      state.projections.route !== null ||
      state.projections.archive !== null ||
      state.projections.cleanup !== null
    ) {
      fail("invalid-state", "completed state cannot retain projections");
    }
  } else {
    const operation = state.operation;
    if (!isObject(operation) || !OPERATION_KINDS.has(operation.kind)) {
      fail("invalid-state", "operation is invalid");
    }
    assertOpaqueID(operation.operation_id, "operation.operation_id");
    if (operation.kind === "close") {
      if (operation.evacuation_id !== null) {
        fail("invalid-state", "close operation cannot carry evacuation_id");
      }
    } else {
      assertEvacuationID(
        operation.evacuation_id,
        "operation.evacuation_id",
      );
    }
    assertCounter(operation.epoch, "operation.epoch");
    if (operation.epoch !== state.epoch || operation.epoch < 1) {
      fail("invalid-state", "operation epoch does not match state epoch");
    }
    if (typeof operation.phase !== "string" || operation.phase.length < 1) {
      fail("invalid-state", "operation phase is invalid");
    }
    assertCellName(operation.source_cell, "operation.source_cell");
    if (operation.target_cell !== null) {
      assertCellName(operation.target_cell, "operation.target_cell");
    }
    if (operation.archive !== null) {
      assertArchiveRef(operation.archive, "operation.archive");
    }
    assertProjection(state.projections.route, "route", operation);
    assertProjection(state.projections.archive, "archive", operation);
    assertProjection(state.projections.cleanup, "cleanup", operation);
  }

  if (state.last_completed !== null) {
    const completed = state.last_completed;
    if (!isObject(completed) || !OPERATION_KINDS.has(completed.kind)) {
      fail("invalid-state", "last_completed is invalid");
    }
    if (
      ![
        "completed",
        "aborted",
        "reaped",
        "cancelled",
        "quarantined",
      ].includes(
        completed.outcome,
      )
    ) {
      fail("invalid-state", "last_completed outcome is invalid");
    }
    assertOpaqueID(completed.operation_id, "last_completed.operation_id");
    if (completed.kind === "close") {
      if (completed.evacuation_id !== null) {
        fail("invalid-state", "completed close cannot carry evacuation_id");
      }
    } else {
      assertEvacuationID(
        completed.evacuation_id,
        "last_completed.evacuation_id",
      );
    }
    assertCounter(completed.epoch, "last_completed.epoch");
    assertCounter(
      completed.completed_revision,
      "last_completed.completed_revision",
    );
    if (
      completed.epoch > state.epoch ||
      completed.completed_revision > state.revision
    ) {
      fail("invalid-state", "last_completed is ahead of state");
    }
    assertCellName(completed.source_cell, "last_completed.source_cell");
    if (completed.target_cell !== null) {
      assertCellName(completed.target_cell, "last_completed.target_cell");
    }
    if (completed.archive !== null) {
      assertArchiveRef(completed.archive, "last_completed.archive");
    }
  }
  return state;
}

function normalizeClaim(state, input) {
  if (!isObject(input)) {
    fail("invalid-claim", "operation claim is required");
  }
  const operationID = input.operation_id;
  const kind = input.kind;
  assertOpaqueID(operationID, "operation_id");
  if (!OPERATION_KINDS.has(kind)) {
    fail("invalid-operation", "operation kind is invalid");
  }

  const location = state.location;
  let sourceCell;
  let targetCell = input.target_cell ?? null;
  let archive = null;
  let evacuationID = input.evacuation_id ?? null;

  if (kind === "evacuate" || kind === "move" || kind === "close") {
    if (location.kind !== "live") {
      fail(
        "invalid-location",
        `${kind} requires an authoritative live location`,
      );
    }
    sourceCell = input.source_cell ?? location.cell;
    assertCellName(sourceCell, "source_cell");
    if (sourceCell !== location.cell) {
      fail(
        "source-mismatch",
        `source cell ${sourceCell} does not own the account`,
      );
    }
  } else {
    if (location.kind !== "archived") {
      fail(
        "invalid-location",
        "restore requires an authoritative archived location",
      );
    }
    sourceCell = input.source_cell ?? location.source_cell;
    assertCellName(sourceCell, "source_cell");
    if (sourceCell !== location.source_cell) {
      fail(
        "source-mismatch",
        `source cell ${sourceCell} does not match the archive`,
      );
    }
  }

  if (kind === "evacuate" || kind === "move") {
    evacuationID ??= operationID;
    assertEvacuationID(evacuationID);
    if (evacuationID !== operationID) {
      fail(
        "invalid-evacuation-id",
        `${kind} evacuation_id must equal operation_id`,
      );
    }
    archive = archiveRef(input.archive);
  } else if (kind === "restore") {
    evacuationID ??= location.pointer.evacuation_id;
    assertEvacuationID(evacuationID);
    const archivedEvacuationID = location.pointer.evacuation_id;
    if (
      archivedEvacuationID !== undefined &&
      archivedEvacuationID !== null &&
      archivedEvacuationID !== "" &&
      archivedEvacuationID !== evacuationID
    ) {
      fail(
        "evacuation-mismatch",
        "restore evacuation_id does not match the authoritative archive",
      );
    }
    archive = archiveRef(input.archive ?? location);
    if (!sameArchive(archive, location)) {
      fail(
        "archive-mismatch",
        "restore claim does not match the authoritative archive",
      );
    }
  } else if (input.archive !== undefined && input.archive !== null) {
    fail("invalid-claim", "close cannot carry an archive");
  } else if (evacuationID !== null) {
    fail("invalid-claim", "close cannot carry evacuation_id");
  }

  if (kind === "restore" || kind === "move") {
    assertCellName(targetCell, "target_cell");
    if (kind === "move" && targetCell === sourceCell) {
      fail(
        "invalid-target",
        "move target_cell must differ from source_cell",
      );
    }
  } else if (targetCell !== null) {
    fail("invalid-claim", `${kind} cannot carry target_cell`);
  }

  return {
    operation_id: operationID,
    evacuation_id: evacuationID,
    kind,
    source_cell: sourceCell,
    target_cell: targetCell,
    archive,
  };
}

function normalizeReplayClaim(completedOrActive, input) {
  if (!isObject(input)) {
    fail("invalid-claim", "operation claim is required");
  }
  assertOpaqueID(input.operation_id, "operation_id");
  if (!OPERATION_KINDS.has(input.kind)) {
    fail("invalid-operation", "operation kind is invalid");
  }
  const sourceCell = input.source_cell ?? completedOrActive.source_cell;
  assertCellName(sourceCell, "source_cell");

  let targetCell = input.target_cell ?? null;
  let archive = null;
  let evacuationID =
    input.evacuation_id ?? completedOrActive.evacuation_id ?? null;
  if (input.kind === "restore" || input.kind === "move") {
    assertCellName(targetCell, "target_cell");
  } else if (targetCell !== null) {
    fail("invalid-claim", `${input.kind} cannot carry target_cell`);
  }
  if (input.kind === "evacuate" || input.kind === "move") {
    assertEvacuationID(evacuationID);
    if (evacuationID !== input.operation_id) {
      fail(
        "invalid-evacuation-id",
        `${input.kind} evacuation_id must equal operation_id`,
      );
    }
    archive = archiveRef(input.archive);
  } else if (input.kind === "restore") {
    assertEvacuationID(evacuationID);
    archive = archiveRef(input.archive ?? completedOrActive.archive);
  } else if (input.archive !== undefined && input.archive !== null) {
    fail("invalid-claim", "close cannot carry an archive");
  } else if (evacuationID !== null) {
    fail("invalid-claim", "close cannot carry evacuation_id");
  }
  return {
    operation_id: input.operation_id,
    evacuation_id: evacuationID,
    kind: input.kind,
    source_cell: sourceCell,
    target_cell: targetCell,
    archive,
  };
}

/**
 * Claim the next durable epoch. Retrying the exact active or most recently
 * completed claim is a no-op; every conflicting claim fails closed.
 */
export function claimOperation(stateInput, input) {
  const state = validateLifecycleState(stateInput);

  if (state.operation) {
    if (state.operation.operation_id === input?.operation_id) {
      const replay = normalizeReplayClaim(state.operation, input);
      if (sameOperationIdentity(state.operation, replay)) {
        return state;
      }
      fail(
        "operation-id-reused",
        `operation_id ${input.operation_id} was reused with different inputs`,
      );
    }
    fail(
      "operation-busy",
      `operation ${state.operation.operation_id} already owns the lifecycle`,
    );
  }

  if (state.last_completed?.operation_id === input?.operation_id) {
    const replay = normalizeReplayClaim(state.last_completed, input);
    if (sameOperationIdentity(state.last_completed, replay)) {
      return state;
    }
    fail(
      "operation-id-reused",
      `operation_id ${input.operation_id} was reused after completion`,
    );
  }

  const claim = normalizeClaim(state, input);
  const epoch = state.epoch + 1;
  return bump(state, {
    epoch,
    operation: {
      ...claim,
      epoch,
      phase: "claimed",
    },
    projections: emptyProjections(),
  });
}

/**
 * Record completion of one external cell-side step. The caller supplies both
 * phases so a delayed acknowledgement cannot skip or advance a newer step.
 */
export function acknowledgeStep(
  stateInput,
  {
    operation_id: operationID,
    from_phase: fromPhase,
    to_phase: toPhase,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  const transition = `${fromPhase}->${toPhase}`;
  if (!ACKNOWLEDGEMENT_TRANSITIONS[operation.kind].has(transition)) {
    fail(
      "invalid-transition",
      `${transition} is not valid for ${operation.kind}`,
    );
  }
  if (operation.phase === toPhase) {
    return state;
  }
  if (operation.phase !== fromPhase) {
    fail(
      "phase-mismatch",
      `expected phase ${fromPhase}, found ${operation.phase}`,
    );
  }
  return bump(state, {
    operation: {
      ...operation,
      phase: toPhase,
    },
  });
}

function requirePhase(operation, expected) {
  if (operation.phase !== expected) {
    fail(
      "phase-mismatch",
      `expected phase ${expected}, found ${operation.phase}`,
    );
  }
}

function normalizedArchivePointer(operation, pointerInput) {
  const pointer = cloneJSON(pointerInput, "archive pointer");
  if (pointer.object !== operation.archive.object) {
    fail(
      "archive-mismatch",
      "archive pointer object does not match the claimed archive",
    );
  }
  if (
    pointer.archive_id !== undefined &&
    pointer.archive_id !== operation.archive.archive_id
  ) {
    fail(
      "archive-mismatch",
      "archive pointer id does not match the claimed archive",
    );
  }
  if (
    pointer.evacuation_id !== undefined &&
    pointer.evacuation_id !== operation.evacuation_id
  ) {
    fail(
      "evacuation-mismatch",
      "archive pointer evacuation id does not match the operation",
    );
  }
  const sourceCell = pointer.cell ?? pointer.source_cell;
  if (sourceCell !== operation.source_cell) {
    fail(
      "source-mismatch",
      "archive pointer source does not match the operation",
    );
  }
  pointer.archive_id = operation.archive.archive_id;
  pointer.object = operation.archive.object;
  pointer.evacuation_id = operation.evacuation_id;
  return pointer;
}

/**
 * Commit the durable archived location before publishing archived: or
 * deleting acct:. This is the authoritative recovery point.
 */
export function commitArchived(
  stateInput,
  {
    operation_id: operationID,
    archived,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (!["evacuate", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot commit an archived location`,
    );
  }
  const pointer = normalizedArchivePointer(operation, archived);
  if (operation.phase === "archive_committed") {
    if (
      state.location.kind === "archived" &&
      sameArchive(state.location, operation.archive) &&
      JSON.stringify(state.location.pointer) === JSON.stringify(pointer)
    ) {
      return state;
    }
    fail(
      "state-conflict",
      "archive_committed state does not match the acknowledgement",
    );
  }
  requirePhase(operation, "source_suspended");

  const location = normalizeArchivedLocation(
    pointer,
    operation.archive.archive_id,
  );
  return bump(state, {
    location,
    operation: {
      ...operation,
      phase: "archive_committed",
    },
    projections: {
      route: routeProjection("delete", operation.epoch),
      archive: archiveProjection(
        "put",
        operation.epoch,
        operation.archive,
        pointer,
      ),
      cleanup: null,
    },
  });
}

/**
 * Commit a restored live location before its acct: projection is written.
 * The archived pointer and object remain pending exact deletion until the
 * route projection succeeds.
 */
export function commitLive(
  stateInput,
  {
    operation_id: operationID,
    route,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (!["restore", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot commit a live location`,
    );
  }
  const location = normalizeLiveLocation(route);
  if (location.cell !== operation.target_cell) {
    fail(
      "target-mismatch",
      "live route does not match the operation target",
    );
  }
  if (operation.phase === "live_committed") {
    if (
      state.location.kind === "live" &&
      JSON.stringify(state.location.route) === JSON.stringify(location.route)
    ) {
      return state;
    }
    fail(
      "state-conflict",
      "live_committed state does not match the acknowledgement",
    );
  }
  requirePhase(operation, "target_resumed");

  return bump(state, {
    location,
    operation: {
      ...operation,
      phase: "live_committed",
    },
    projections: {
      route: routeProjection("put", operation.epoch, location.route),
      archive: archiveProjection(
        "delete",
        operation.epoch,
        operation.archive,
      ),
      cleanup: cleanupProjection(operation.epoch, operation.archive),
    },
  });
}

/**
 * Commit an imported closed tombstone without ever creating an acct: route.
 * The exact archived pointer and object remain authoritative retention. A
 * closed import is evidence that the archive is semantically complete, not
 * permission to erase the only portable copy of the closed account.
 */
export function commitRestoredClosed(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (!["restore", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot commit a restored tombstone`,
    );
  }
  if (operation.restored_status !== "closed") {
    fail(
      "state-conflict",
      "restored tombstone requires a closed target acknowledgement",
    );
  }
  if (operation.phase === "restored_closed_committed") {
    if (state.location.kind === "closed_archived") {
      return state;
    }
    fail(
      "state-conflict",
      "restored_closed_committed state does not contain a tombstone",
    );
  }
  requirePhase(operation, "target_resumed");
  if (
    state.location.kind !== "archived" ||
    !sameArchive(state.location, operation.archive)
  ) {
    fail(
      "state-conflict",
      "restored tombstone requires its authoritative archive",
    );
  }
  return bump(state, {
    location: {
      ...state.location,
      kind: "closed_archived",
      pointer: {
        ...state.location.pointer,
        status: "closed",
      },
    },
    operation: {
      ...operation,
      phase: "restored_closed_committed",
    },
    projections: {
      route: null,
      archive: archiveProjection(
        "put",
        operation.epoch,
        operation.archive,
        {
          ...state.location.pointer,
          status: "closed",
        },
      ),
      cleanup: null,
    },
  });
}

/**
 * Commit a cell-side tombstone before deleting the route projection.
 */
export function commitClosed(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (operation.kind !== "close") {
    fail(
      "invalid-transition",
      `${operation.kind} cannot commit a closed location`,
    );
  }
  if (operation.phase === "closed_committed") {
    if (state.location.kind === "closed") {
      return state;
    }
    fail(
      "state-conflict",
      "closed_committed state does not contain a closed location",
    );
  }
  requirePhase(operation, "source_closed");
  return bump(state, {
    location: { kind: "closed" },
    operation: {
      ...operation,
      phase: "closed_committed",
    },
    projections: {
      route: routeProjection("delete", operation.epoch),
      archive: null,
      cleanup: null,
    },
  });
}

/**
 * Commit the acknowledged tombstone for a pending account encountered during
 * evacuation. No archive is published; only the stale route projection is
 * retired. A move must fail instead of silently turning into account loss.
 */
export function commitReaped(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (operation.kind !== "evacuate") {
    fail(
      "invalid-transition",
      `${operation.kind} cannot commit a pending-account reap`,
    );
  }
  if (operation.phase === "closed_committed") {
    if (
      operation.outcome === "reaped" &&
      state.location.kind === "closed"
    ) {
      return state;
    }
    fail(
      "state-conflict",
      "closed_committed state is not the acknowledged reap",
    );
  }
  requirePhase(operation, "claimed");
  return bump(state, {
    location: { kind: "closed" },
    operation: {
      ...operation,
      outcome: "reaped",
      phase: "closed_committed",
    },
    projections: {
      route: routeProjection("delete", operation.epoch),
      archive: null,
      cleanup: null,
    },
  });
}

const PROJECTION_TRANSITIONS = Object.freeze({
  evacuate: Object.freeze({
    "archive:put": {
      from: "archive_committed",
      to: "archive_projected",
    },
    "route:delete": {
      from: "archive_projected",
      to: "route_retired",
    },
    "route:delete:reaped": {
      from: "closed_committed",
      to: "route_retired",
    },
  }),
  restore: Object.freeze({
    "route:put": {
      from: "live_committed",
      to: "route_projected",
    },
    "archive:delete": {
      from: "source_finalized",
      to: "archive_retired",
    },
    "archive:put:closed": {
      from: "restored_closed_committed",
      to: "closed_archive_projected",
    },
  }),
  move: Object.freeze({
    "archive:put": {
      from: "archive_committed",
      to: "archive_projected",
    },
    "route:delete": {
      from: "archive_projected",
      to: "route_retired",
    },
    "route:put": {
      from: "live_committed",
      to: "route_projected",
    },
    "archive:delete": {
      from: "source_finalized",
      to: "archive_retired",
    },
    "archive:put:closed": {
      from: "restored_closed_committed",
      to: "closed_archive_projected",
    },
  }),
  close: Object.freeze({
    "route:delete": {
      from: "closed_committed",
      to: "route_retired",
    },
  }),
});

/**
 * Mark one KV projection complete. Restore/move archive deletion is matched
 * by both archive_id and object; a stale pointer must never be deleted.
 */
export function markProjectionApplied(
  stateInput,
  {
    operation_id: operationID,
    target,
    action,
    archive_id: archiveID,
    archive_object: archiveObject,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (!["route", "archive"].includes(target)) {
    fail("invalid-projection", "projection target is invalid");
  }
  const transitionKey =
    operation.kind === "evacuate" &&
    operation.outcome === "reaped" &&
    target === "route" &&
    action === "delete"
      ? "route:delete:reaped"
      : (
          operation.restored_status === "closed" &&
          target === "archive" &&
          action === "put"
        )
        ? "archive:put:closed"
        : `${target}:${action}`;
  const transition =
    PROJECTION_TRANSITIONS[operation.kind][transitionKey];
  if (!transition) {
    fail(
      "invalid-transition",
      `${target}:${action} is not valid for ${operation.kind}`,
    );
  }
  const projection = state.projections[target];
  if (!projection || projection.action !== action) {
    fail(
      "projection-mismatch",
      `${target} projection does not expect ${action}`,
    );
  }
  if (target === "archive") {
    if (
      archiveID !== projection.archive_id ||
      archiveObject !== projection.object
    ) {
      fail(
        "archive-mismatch",
        "projection acknowledgement does not match the exact archive",
      );
    }
  }
  if (
    operation.phase === transition.to &&
    projection.status === "applied"
  ) {
    return state;
  }
  requirePhase(operation, transition.from);
  if (projection.status !== "pending") {
    fail("projection-mismatch", `${target} projection is not pending`);
  }
  return bump(state, {
    operation: {
      ...operation,
      phase: transition.to,
    },
    projections: {
      ...state.projections,
      [target]: {
        ...projection,
        status: "applied",
      },
    },
  });
}

/**
 * Mark the R2 object cleanup complete only for the archive claimed by this
 * epoch. A failed deletion leaves the operation active and future restores
 * blocked; a retry of the exact deletion is idempotent.
 */
export function markArchiveCleanupApplied(
  stateInput,
  {
    operation_id: operationID,
    archive_id: archiveID,
    archive_object: archiveObject,
  },
) {
  const state = validateLifecycleState(stateInput);
  const operation = currentOperation(state, operationID);
  if (!["restore", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot clean up a restored archive`,
    );
  }
  const cleanup = state.projections.cleanup;
  if (!cleanup) {
    fail("projection-mismatch", "no archive cleanup is pending");
  }
  if (
    archiveID !== cleanup.archive_id ||
    archiveObject !== cleanup.object
  ) {
    fail(
      "archive-mismatch",
      "cleanup acknowledgement does not match the exact archive",
    );
  }
  if (
    operation.phase === "archive_cleaned" &&
    cleanup.status === "applied"
  ) {
    return state;
  }
  requirePhase(operation, "archive_retired");
  if (cleanup.status !== "pending") {
    fail("projection-mismatch", "archive cleanup is not pending");
  }
  return bump(state, {
    operation: {
      ...operation,
      phase: "archive_cleaned",
    },
    projections: {
      ...state.projections,
      cleanup: {
        ...cleanup,
        status: "applied",
      },
    },
  });
}

function completedLocation(location) {
  if (location.kind === "live") {
    return {
      kind: "live",
      cell: location.cell,
    };
  }
  if (location.kind === "archived") {
    return {
      kind: "archived",
      source_cell: location.source_cell,
      archive_id: location.archive_id,
      object: location.object,
    };
  }
  if (location.kind === "closed_archived") {
    return {
      kind: "closed_archived",
      source_cell: location.source_cell,
      archive_id: location.archive_id,
      object: location.object,
    };
  }
  return { kind: "closed" };
}

function completablePhase(operation) {
  if (
    ["restore", "move"].includes(operation.kind) &&
    operation.restored_status === "closed"
  ) {
    return "closed_archive_retained";
  }
  return COMPLETABLE_PHASE[operation.kind];
}

/**
 * Retire an operation only after its last durable projection/cleanup phase.
 * last_completed makes a replay of the exact operation id a stable no-op.
 */
export function completeOperation(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  assertOpaqueID(operationID, "operation_id");
  if (!state.operation) {
    if (state.last_completed?.operation_id === operationID) {
      return state;
    }
    fail("no-operation", "no lifecycle operation is active");
  }
  const operation = currentOperation(state, operationID);
  requirePhase(operation, completablePhase(operation));
  const revision = state.revision + 1;
  return {
    ...state,
    revision,
    operation: null,
    projections: emptyProjections(),
    last_completed: {
      operation_id: operation.operation_id,
      evacuation_id: operation.evacuation_id,
      kind: operation.kind,
      epoch: operation.epoch,
      source_cell: operation.source_cell,
      target_cell: operation.target_cell,
      archive:
        operation.archive === null
          ? null
          : archiveRef(operation.archive),
      outcome: operation.outcome ?? "completed",
      final_location: completedLocation(state.location),
      completed_revision: revision,
    },
  };
}

/**
 * Release an evacuation/move claim before an archive is committed. The caller
 * must first undo any source-side suspension; once archive authority exists,
 * rollback is forbidden and the operation must be resumed to completion.
 */
export function abortOperation(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  assertOpaqueID(operationID, "operation_id");
  if (!state.operation) {
    if (
      state.last_completed?.operation_id === operationID &&
      state.last_completed.outcome === "aborted"
    ) {
      return state;
    }
    if (state.last_completed?.operation_id === operationID) {
      fail(
        "operation-completed",
        `operation ${operationID} already completed successfully`,
      );
    }
    fail("no-operation", "no lifecycle operation is active");
  }
  const operation = currentOperation(state, operationID);
  if (!["evacuate", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot be aborted`,
    );
  }
  if (!["claimed", "source_suspended"].includes(operation.phase)) {
    fail(
      "phase-mismatch",
      `operation cannot be aborted from phase ${operation.phase}`,
    );
  }
  if (state.location.kind !== "live") {
    fail(
      "state-conflict",
      "pre-archive abort requires the original live location",
    );
  }
  const revision = state.revision + 1;
  return {
    ...state,
    revision,
    operation: null,
    projections: emptyProjections(),
    last_completed: {
      operation_id: operation.operation_id,
      evacuation_id: operation.evacuation_id,
      kind: operation.kind,
      epoch: operation.epoch,
      source_cell: operation.source_cell,
      target_cell: operation.target_cell,
      archive: archiveRef(operation.archive),
      outcome: "aborted",
      final_location: completedLocation(state.location),
      completed_revision: revision,
    },
  };
}

/**
 * Release an operation before it mutates authoritative account data. Close
 * requires a definitive owner refusal plus an independent non-closed contact
 * result. Restore/move may be cancelled only while still claimed, when the
 * incoming target cannot be durably reserved.
 */
export function cancelOperation(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  assertOpaqueID(operationID, "operation_id");
  if (!state.operation) {
    if (
      state.last_completed?.operation_id === operationID &&
      state.last_completed.outcome === "cancelled"
    ) {
      return state;
    }
    fail("no-operation", "no lifecycle operation is active");
  }
  const operation = currentOperation(state, operationID);
  if (!["close", "restore", "move"].includes(operation.kind)) {
    fail(
      "invalid-transition",
      `${operation.kind} cannot be cancelled`,
    );
  }
  requirePhase(operation, "claimed");
  const requiredLocation =
    operation.kind === "restore" ? "archived" : "live";
  if (state.location.kind !== requiredLocation) {
    fail(
      "state-conflict",
      `${operation.kind} cancellation requires the original ${requiredLocation} location`,
    );
  }
  const revision = state.revision + 1;
  return {
    ...state,
    revision,
    operation: null,
    projections: emptyProjections(),
    last_completed: {
      operation_id: operation.operation_id,
      evacuation_id: operation.evacuation_id,
      kind: operation.kind,
      epoch: operation.epoch,
      source_cell: operation.source_cell,
      target_cell: operation.target_cell,
      archive:
        operation.archive === null
          ? null
          : archiveRef(operation.archive),
      outcome: "cancelled",
      final_location: completedLocation(state.location),
      completed_revision: revision,
    },
  };
}

/**
 * Retire a restore that deterministically failed archive validation before
 * the target cell was mutated. The runtime must release the exact target
 * reservation before committing this transition. A separate exact-archive
 * quarantine record prevents automatic retries from claiming another epoch.
 */
export function quarantineRestoreOperation(
  stateInput,
  {
    operation_id: operationID,
  },
) {
  const state = validateLifecycleState(stateInput);
  assertOpaqueID(operationID, "operation_id");
  if (!state.operation) {
    if (
      state.last_completed?.operation_id === operationID &&
      state.last_completed.outcome === "quarantined"
    ) {
      return state;
    }
    fail("no-operation", "no lifecycle operation is active");
  }
  const operation = currentOperation(state, operationID);
  if (operation.kind !== "restore") {
    fail(
      "invalid-transition",
      `${operation.kind} cannot be archive-quarantined`,
    );
  }
  if (!["claimed", "target_reserved"].includes(operation.phase)) {
    fail(
      "phase-mismatch",
      `restore cannot be archive-quarantined from phase ${operation.phase}`,
    );
  }
  if (
    state.location.kind !== "archived" ||
    !sameArchive(state.location, operation.archive)
  ) {
    fail(
      "state-conflict",
      "restore quarantine requires the exact original archived location",
    );
  }
  const revision = state.revision + 1;
  return {
    ...state,
    revision,
    operation: null,
    projections: emptyProjections(),
    last_completed: {
      operation_id: operation.operation_id,
      evacuation_id: operation.evacuation_id,
      kind: operation.kind,
      epoch: operation.epoch,
      source_cell: operation.source_cell,
      target_cell: operation.target_cell,
      archive: archiveRef(operation.archive),
      outcome: "quarantined",
      final_location: completedLocation(state.location),
      completed_revision: revision,
    },
  };
}

function stepBase(operation) {
  return {
    operation_id: operation.operation_id,
    epoch: operation.epoch,
    kind: operation.kind,
    phase: operation.phase,
  };
}

/**
 * Describe exactly one resumable next step after a Durable Object restart.
 * The descriptor contains no authority by itself; reducers still require the
 * matching operation id, phase, epoch-backed archive, and ordered projection.
 */
export function nextLifecycleStep(stateInput) {
  const state = validateLifecycleState(stateInput);
  const operation = state.operation;
  if (!operation) {
    return null;
  }
  const base = stepBase(operation);

  switch (operation.phase) {
    case "claimed":
      if (operation.kind === "restore" || operation.kind === "move") {
        return {
          ...base,
          type: "projection",
          target: "incoming_target",
          action: "reserve_target",
          target_cell: operation.target_cell,
        };
      }
      if (operation.kind === "close") {
        return {
          ...base,
          type: "cell",
          action: "close_source",
          source_cell: operation.source_cell,
        };
      }
      return {
        ...base,
        type: "cell",
        action: "suspend_source",
        source_cell: operation.source_cell,
      };
    case "target_reserved":
      if (operation.kind === "restore") {
        return {
          ...base,
          type: "cell",
          action: "import_target",
          target_cell: operation.target_cell,
          archive: archiveRef(operation.archive),
        };
      }
      return {
        ...base,
        type: "cell",
        action: "suspend_source",
        source_cell: operation.source_cell,
      };
    case "source_suspended":
      return {
        ...base,
        type: "archive",
        action: "export_validate_and_commit",
        source_cell: operation.source_cell,
        archive: archiveRef(operation.archive),
      };
    case "archive_committed":
      return {
        ...base,
        type: "projection",
        target: "archive",
        projection: cloneJSON(
          state.projections.archive,
          "archive projection",
        ),
      };
    case "archive_projected":
      return {
        ...base,
        type: "projection",
        target: "route",
        projection: cloneJSON(
          state.projections.route,
          "route projection",
        ),
      };
    case "route_retired":
      if (operation.kind === "move") {
        return {
          ...base,
          type: "cell",
          action: "import_target",
          target_cell: operation.target_cell,
          archive: archiveRef(operation.archive),
        };
      }
      return { ...base, type: "state", action: "complete" };
    case "target_imported":
      return {
        ...base,
        type: "cell",
        action: "resume_target",
        target_cell: operation.target_cell,
      };
    case "target_resumed":
      return {
        ...base,
        type: "state",
        action:
          operation.restored_status === "closed"
            ? "commit_restored_closed"
            : "commit_live",
        target_cell: operation.target_cell,
      };
    case "live_committed":
      return {
        ...base,
        type: "projection",
        target: "route",
        projection: cloneJSON(
          state.projections.route,
          "route projection",
        ),
      };
    case "route_projected":
      return {
        ...base,
        type: "cell",
        action: "finalize_source",
        source_cell: operation.source_cell,
      };
    case "restored_closed_committed":
      return {
        ...base,
        type: "projection",
        target: "archive",
        projection: cloneJSON(
          state.projections.archive,
          "archive projection",
        ),
      };
    case "closed_archive_projected":
      return {
        ...base,
        type: "cell",
        action: "finalize_source",
        source_cell: operation.source_cell,
      };
    case "source_finalized":
      return {
        ...base,
        type: "projection",
        target: "archive",
        projection: cloneJSON(
          state.projections.archive,
          "archive projection",
        ),
      };
    case "archive_retired":
      return {
        ...base,
        type: "cleanup",
        target: "archive_object",
        projection: cloneJSON(
          state.projections.cleanup,
          "cleanup projection",
        ),
      };
    case "archive_cleaned":
      return { ...base, type: "state", action: "complete" };
    case "closed_archive_retained":
      return { ...base, type: "state", action: "complete" };
    case "source_closed":
      return { ...base, type: "state", action: "commit_closed" };
    case "closed_committed":
      return {
        ...base,
        type: "projection",
        target: "route",
        projection: cloneJSON(
          state.projections.route,
          "route projection",
        ),
      };
    default:
      fail(
        "invalid-state",
        `operation phase ${operation.phase} has no resumable next step`,
      );
  }
}
