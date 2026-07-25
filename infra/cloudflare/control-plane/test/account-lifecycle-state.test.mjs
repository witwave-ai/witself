import assert from "node:assert/strict";
import test from "node:test";

import {
  AccountLifecycleStateError,
  abortOperation,
  acknowledgeStep,
  bootstrapArchivedState,
  bootstrapLiveState,
  cancelOperation,
  claimOperation,
  commitArchived,
  commitClosed,
  commitLive,
  commitReaped,
  commitRestoredClosed,
  completeOperation,
  markArchiveCleanupApplied,
  markProjectionApplied,
  nextLifecycleStep,
  quarantineRestoreOperation,
  replaceArchivedLocation,
  validateLifecycleState,
} from "../src/account-lifecycle-state.mjs";

const ACCOUNT = "acct_test";
const SOURCE = "aws-us-west-2";
const TARGET = "civo-phx1";
const ARCHIVE = {
  archive_id: "archive.001",
  object: "accounts/acct_test/attempt-001.tar.gz",
};
const SOURCE_ROUTE = {
  cell: SOURCE,
  endpoint: "https://source.example",
  region: "us-west",
};
const TARGET_ROUTE = {
  cell: TARGET,
  endpoint: "https://target.example",
  region: "us-west",
};
const ARCHIVED_POINTER = {
  archive_id: ARCHIVE.archive_id,
  object: ARCHIVE.object,
  cell: SOURCE,
  exported_at: "2026-07-25T00:00:00.000Z",
  size: 42,
};

function expectCode(code) {
  return (error) => {
    assert.ok(error instanceof AccountLifecycleStateError);
    assert.equal(error.code, code);
    return true;
  };
}

function live() {
  return bootstrapLiveState({
    account_id: ACCOUNT,
    route: SOURCE_ROUTE,
  });
}

function archived() {
  return bootstrapArchivedState({
    account_id: ACCOUNT,
    archived: {
      ...ARCHIVED_POINTER,
      evacuation_id: "source_evacuation_001",
    },
  });
}

function evacuateToArchive(state, operationID = "evacuate_001") {
  let next = claimOperation(state, {
    operation_id: operationID,
    kind: "evacuate",
    source_cell: SOURCE,
    archive: ARCHIVE,
  });
  next = acknowledgeStep(next, {
    operation_id: operationID,
    from_phase: "claimed",
    to_phase: "source_suspended",
  });
  next = commitArchived(next, {
    operation_id: operationID,
    archived: ARCHIVED_POINTER,
  });
  next = markProjectionApplied(next, {
    operation_id: operationID,
    target: "archive",
    action: "put",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  next = markProjectionApplied(next, {
    operation_id: operationID,
    target: "route",
    action: "delete",
  });
  return completeOperation(next, { operation_id: operationID });
}

test("bootstraps plain durable live and legacy archived states", () => {
  const route = { ...SOURCE_ROUTE };
  const state = bootstrapLiveState({ account_id: ACCOUNT, route });
  route.cell = TARGET;

  assert.equal(state.location.cell, SOURCE);
  assert.deepEqual(state.projections, {
    route: null,
    archive: null,
    cleanup: null,
  });
  assert.equal(state.revision, 0);
  assert.equal(state.epoch, 0);
  assert.equal(JSON.parse(JSON.stringify(state)).account_id, ACCOUNT);
  assert.equal(validateLifecycleState(state), state);

  const legacy = bootstrapArchivedState({
    account_id: ACCOUNT,
    archived: {
      object: "accounts/legacy.tar.gz",
      cell: SOURCE,
    },
  });
  assert.equal(legacy.location.archive_id, "accounts/legacy.tar.gz");
  assert.equal(legacy.location.object, "accounts/legacy.tar.gz");
});

test("evacuation persists archive authority before ordered KV projections", () => {
  const initial = live();
  let state = claimOperation(initial, {
    operation_id: "evacuate_001",
    kind: "evacuate",
    archive: ARCHIVE,
  });
  assert.equal(state.epoch, 1);
  assert.equal(state.revision, 1);
  assert.equal(nextLifecycleStep(state).action, "suspend_source");

  const claimed = state;
  assert.equal(
    claimOperation(state, {
      operation_id: "evacuate_001",
      kind: "evacuate",
      source_cell: SOURCE,
      archive: ARCHIVE,
    }),
    claimed,
    "exact active claim is idempotent",
  );

  state = acknowledgeStep(state, {
    operation_id: "evacuate_001",
    from_phase: "claimed",
    to_phase: "source_suspended",
  });
  assert.equal(nextLifecycleStep(state).action, "export_validate_and_commit");

  state = commitArchived(state, {
    operation_id: "evacuate_001",
    archived: ARCHIVED_POINTER,
  });
  assert.equal(state.location.kind, "archived");
  assert.equal(state.location.archive_id, ARCHIVE.archive_id);
  assert.equal(state.projections.archive.status, "pending");
  assert.equal(state.projections.route.status, "pending");
  assert.equal(nextLifecycleStep(state).target, "archive");

  assert.throws(
    () =>
      markProjectionApplied(state, {
        operation_id: "evacuate_001",
        target: "route",
        action: "delete",
      }),
    expectCode("phase-mismatch"),
    "route cannot disappear before the archive pointer is published",
  );

  state = markProjectionApplied(state, {
    operation_id: "evacuate_001",
    target: "archive",
    action: "put",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  assert.equal(nextLifecycleStep(state).target, "route");
  state = markProjectionApplied(state, {
    operation_id: "evacuate_001",
    target: "route",
    action: "delete",
  });
  state = completeOperation(state, {
    operation_id: "evacuate_001",
  });

  assert.equal(state.operation, null);
  assert.equal(state.location.kind, "archived");
  assert.equal(state.last_completed.operation_id, "evacuate_001");
  assert.equal(state.last_completed.epoch, 1);
  assert.equal(state.revision, 6);
  assert.equal(nextLifecycleStep(state), null);

  const replay = claimOperation(state, {
    operation_id: "evacuate_001",
    kind: "evacuate",
    source_cell: SOURCE,
    archive: ARCHIVE,
  });
  assert.equal(replay, state, "last_completed makes exact replay a no-op");
});

test("restore keeps stale archive recovery blocked until exact cleanup", () => {
  let state = archived();
  state = claimOperation(state, {
    operation_id: "restore_001",
    evacuation_id: "source_evacuation_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  assert.equal(nextLifecycleStep(state).action, "reserve_target");
  assert.equal(state.operation.operation_id, "restore_001");
  assert.equal(
    state.operation.evacuation_id,
    "source_evacuation_001",
    "the DO operation and cell evacuation receipt remain distinct",
  );

  state = acknowledgeStep(state, {
    operation_id: "restore_001",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_001",
    from_phase: "target_reserved",
    to_phase: "target_imported",
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_001",
    from_phase: "target_imported",
    to_phase: "target_resumed",
  });
  state = commitLive(state, {
    operation_id: "restore_001",
    route: TARGET_ROUTE,
  });

  assert.equal(state.location.kind, "live");
  assert.equal(state.location.cell, TARGET);
  assert.equal(state.projections.route.action, "put");
  assert.equal(state.projections.archive.action, "delete");
  assert.equal(state.projections.cleanup.object, ARCHIVE.object);

  assert.throws(
    () =>
      claimOperation(state, {
        operation_id: "restore_002",
        kind: "restore",
        target_cell: TARGET,
        archive: ARCHIVE,
      }),
    expectCode("operation-busy"),
  );

  state = markProjectionApplied(state, {
    operation_id: "restore_001",
    target: "route",
    action: "put",
  });
  assert.equal(nextLifecycleStep(state).action, "finalize_source");
  assert.throws(
    () =>
      markArchiveCleanupApplied(state, {
        operation_id: "restore_001",
        archive_id: ARCHIVE.archive_id,
        archive_object: ARCHIVE.object,
      }),
    expectCode("phase-mismatch"),
    "R2 cleanup cannot precede archived pointer retirement",
  );
  assert.throws(
    () =>
      markProjectionApplied(state, {
        operation_id: "restore_001",
        target: "archive",
        action: "delete",
        archive_id: "archive.stale",
        archive_object: ARCHIVE.object,
      }),
    expectCode("archive-mismatch"),
  );

  state = acknowledgeStep(state, {
    operation_id: "restore_001",
    from_phase: "route_projected",
    to_phase: "source_finalized",
  });
  state = markProjectionApplied(state, {
    operation_id: "restore_001",
    target: "archive",
    action: "delete",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  const beforeWrongCleanup = state;
  assert.throws(
    () =>
      markArchiveCleanupApplied(state, {
        operation_id: "restore_001",
        archive_id: ARCHIVE.archive_id,
        archive_object: "accounts/acct_test/a-newer-object.tar.gz",
      }),
    expectCode("archive-mismatch"),
  );
  assert.equal(state, beforeWrongCleanup);

  state = markArchiveCleanupApplied(state, {
    operation_id: "restore_001",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  const cleaned = state;
  assert.equal(
    markArchiveCleanupApplied(state, {
      operation_id: "restore_001",
      archive_id: ARCHIVE.archive_id,
      archive_object: ARCHIVE.object,
    }),
    cleaned,
  );
  state = completeOperation(state, { operation_id: "restore_001" });
  assert.equal(state.location.kind, "live");
  assert.equal(state.location.cell, TARGET);
  assert.equal(state.last_completed.final_location.cell, TARGET);
  assert.equal(
    completeOperation(state, { operation_id: "restore_001" }),
    state,
  );
});

test("move is one epoch spanning evacuation, restoration, and cleanup", () => {
  let state = claimOperation(live(), {
    operation_id: "move_001",
    kind: "move",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  state = acknowledgeStep(state, {
    operation_id: "move_001",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: "move_001",
    from_phase: "target_reserved",
    to_phase: "source_suspended",
  });
  state = commitArchived(state, {
    operation_id: "move_001",
    archived: ARCHIVED_POINTER,
  });
  state = markProjectionApplied(state, {
    operation_id: "move_001",
    target: "archive",
    action: "put",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  state = markProjectionApplied(state, {
    operation_id: "move_001",
    target: "route",
    action: "delete",
  });
  assert.equal(nextLifecycleStep(state).action, "import_target");

  state = acknowledgeStep(state, {
    operation_id: "move_001",
    from_phase: "route_retired",
    to_phase: "target_imported",
  });
  state = acknowledgeStep(state, {
    operation_id: "move_001",
    from_phase: "target_imported",
    to_phase: "target_resumed",
  });
  state = commitLive(state, {
    operation_id: "move_001",
    route: TARGET_ROUTE,
  });
  state = markProjectionApplied(state, {
    operation_id: "move_001",
    target: "route",
    action: "put",
  });
  state = acknowledgeStep(state, {
    operation_id: "move_001",
    from_phase: "route_projected",
    to_phase: "source_finalized",
  });
  state = markProjectionApplied(state, {
    operation_id: "move_001",
    target: "archive",
    action: "delete",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  state = markArchiveCleanupApplied(state, {
    operation_id: "move_001",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  state = completeOperation(state, { operation_id: "move_001" });

  assert.equal(state.epoch, 1);
  assert.equal(state.location.cell, TARGET);
  assert.equal(state.last_completed.kind, "move");
});

test("close commits the tombstone before route deletion", () => {
  let state = claimOperation(live(), {
    operation_id: "close_001",
    kind: "close",
    source_cell: SOURCE,
  });
  state = acknowledgeStep(state, {
    operation_id: "close_001",
    from_phase: "claimed",
    to_phase: "source_closed",
  });
  assert.equal(nextLifecycleStep(state).action, "commit_closed");
  state = commitClosed(state, { operation_id: "close_001" });
  assert.equal(state.location.kind, "closed");
  assert.equal(state.projections.route.status, "pending");
  state = markProjectionApplied(state, {
    operation_id: "close_001",
    target: "route",
    action: "delete",
  });
  state = completeOperation(state, { operation_id: "close_001" });
  assert.equal(state.last_completed.final_location.kind, "closed");
});

test("definitively refused close can release only its claimed epoch", () => {
  let state = claimOperation(live(), {
    operation_id: "close_cancel",
    kind: "close",
    source_cell: SOURCE,
  });
  state = cancelOperation(state, {
    operation_id: "close_cancel",
  });
  assert.equal(state.location.kind, "live");
  assert.equal(state.operation, null);
  assert.equal(state.last_completed.outcome, "cancelled");
  assert.throws(
    () =>
      cancelOperation(
        claimOperation(live(), {
          operation_id: "evacuate_cancel",
          kind: "evacuate",
          archive: ARCHIVE,
        }),
        { operation_id: "evacuate_cancel" },
      ),
    expectCode("invalid-transition"),
  );
});

test("restored closed archive remains canonical without publishing a live route", () => {
  let state = claimOperation(archived(), {
    operation_id: "restore_closed",
    evacuation_id: "source_evacuation_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_closed",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_closed",
    from_phase: "target_reserved",
    to_phase: "target_imported",
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_closed",
    from_phase: "target_imported",
    to_phase: "target_resumed",
  });
  state = validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...state.operation,
      restored_status: "closed",
    },
  });
  assert.equal(
    nextLifecycleStep(state).action,
    "commit_restored_closed",
  );
  state = commitRestoredClosed(state, {
    operation_id: "restore_closed",
  });
  assert.equal(state.location.kind, "closed_archived");
  assert.equal(state.projections.route, null);
  assert.equal(nextLifecycleStep(state).target, "archive");
  state = markProjectionApplied(state, {
    operation_id: "restore_closed",
    target: "archive",
    action: "put",
    archive_id: ARCHIVE.archive_id,
    archive_object: ARCHIVE.object,
  });
  assert.equal(nextLifecycleStep(state).action, "finalize_source");
  state = acknowledgeStep(state, {
    operation_id: "restore_closed",
    from_phase: "closed_archive_projected",
    to_phase: "closed_archive_retained",
  });
  state = completeOperation(state, {
    operation_id: "restore_closed",
  });
  assert.equal(state.location.kind, "closed_archived");
  assert.equal(state.location.object, ARCHIVE.object);
  assert.equal(
    state.last_completed.final_location.kind,
    "closed_archived",
  );
});

test("conflicting claims, reused ids, and stale phases fail closed", () => {
  let state = claimOperation(live(), {
    operation_id: "evacuate_001",
    kind: "evacuate",
    archive: ARCHIVE,
  });
  assert.throws(
    () =>
      claimOperation(state, {
        operation_id: "restore_001",
        kind: "restore",
        target_cell: TARGET,
        archive: ARCHIVE,
      }),
    expectCode("operation-busy"),
  );
  assert.throws(
    () =>
      claimOperation(state, {
        operation_id: "evacuate_001",
        kind: "evacuate",
        archive: {
          archive_id: "archive.002",
          object: "accounts/acct_test/attempt-002.tar.gz",
        },
      }),
    expectCode("operation-id-reused"),
  );
  assert.throws(
    () =>
      acknowledgeStep(state, {
        operation_id: "evacuate_stale",
        from_phase: "claimed",
        to_phase: "source_suspended",
      }),
    expectCode("stale-operation"),
  );

  state = acknowledgeStep(state, {
    operation_id: "evacuate_001",
    from_phase: "claimed",
    to_phase: "source_suspended",
  });
  assert.equal(
    acknowledgeStep(state, {
      operation_id: "evacuate_001",
      from_phase: "claimed",
      to_phase: "source_suspended",
    }),
    state,
    "exact transition replay is allowed",
  );
});

test("pre-archive evacuation abort is durable and idempotent", () => {
  const initial = live();
  let state = claimOperation(initial, {
    operation_id: "evacuate_abort",
    kind: "evacuate",
    archive: ARCHIVE,
  });
  state = acknowledgeStep(state, {
    operation_id: "evacuate_abort",
    from_phase: "claimed",
    to_phase: "source_suspended",
  });
  state = abortOperation(state, {
    operation_id: "evacuate_abort",
  });

  assert.equal(state.operation, null);
  assert.equal(state.location.kind, "live");
  assert.equal(state.location.cell, SOURCE);
  assert.equal(state.last_completed.outcome, "aborted");
  assert.equal(
    abortOperation(state, { operation_id: "evacuate_abort" }),
    state,
  );
  assert.equal(
    claimOperation(state, {
      operation_id: "evacuate_abort",
      kind: "evacuate",
      archive: ARCHIVE,
    }),
    state,
    "the exact aborted request remains idempotent",
  );
});

test("pending-account reap commits closed authority without an archive", () => {
  let state = claimOperation(live(), {
    operation_id: "evacuate_reap",
    kind: "evacuate",
    archive: ARCHIVE,
  });
  state = commitReaped(state, {
    operation_id: "evacuate_reap",
  });
  assert.equal(state.location.kind, "closed");
  assert.equal(state.projections.archive, null);
  assert.equal(state.projections.cleanup, null);
  assert.equal(nextLifecycleStep(state).target, "route");

  state = markProjectionApplied(state, {
    operation_id: "evacuate_reap",
    target: "route",
    action: "delete",
  });
  state = completeOperation(state, {
    operation_id: "evacuate_reap",
  });
  assert.equal(state.last_completed.outcome, "reaped");
  assert.equal(state.last_completed.final_location.kind, "closed");
});

test("move cannot turn a pending reap into silent account deletion", () => {
  const state = claimOperation(live(), {
    operation_id: "move_reap",
    kind: "move",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  assert.throws(
    () => commitReaped(state, { operation_id: "move_reap" }),
    expectCode("invalid-transition"),
  );
});

test("abort is forbidden after archive authority is committed", () => {
  let state = claimOperation(live(), {
    operation_id: "move_no_abort",
    kind: "move",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  state = acknowledgeStep(state, {
    operation_id: "move_no_abort",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = acknowledgeStep(state, {
    operation_id: "move_no_abort",
    from_phase: "target_reserved",
    to_phase: "source_suspended",
  });
  state = commitArchived(state, {
    operation_id: "move_no_abort",
    archived: ARCHIVED_POINTER,
  });
  assert.throws(
    () =>
      abortOperation(state, {
        operation_id: "move_no_abort",
      }),
    expectCode("phase-mismatch"),
  );
});

test("restore quarantine is terminal only before target mutation", () => {
  let state = claimOperation(archived(), {
    operation_id: "restore_quarantine",
    evacuation_id: "source_evacuation_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  state = acknowledgeStep(state, {
    operation_id: "restore_quarantine",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  state = quarantineRestoreOperation(state, {
    operation_id: "restore_quarantine",
  });

  assert.equal(state.operation, null);
  assert.equal(state.location.kind, "archived");
  assert.equal(state.location.archive_id, ARCHIVE.archive_id);
  assert.equal(state.last_completed.outcome, "quarantined");
  assert.equal(
    quarantineRestoreOperation(state, {
      operation_id: "restore_quarantine",
    }),
    state,
    "exact quarantine completion is idempotent",
  );

  let imported = claimOperation(archived(), {
    operation_id: "restore_imported",
    evacuation_id: "source_evacuation_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  imported = acknowledgeStep(imported, {
    operation_id: "restore_imported",
    from_phase: "claimed",
    to_phase: "target_reserved",
  });
  imported = acknowledgeStep(imported, {
    operation_id: "restore_imported",
    from_phase: "target_reserved",
    to_phase: "target_imported",
  });
  assert.throws(
    () =>
      quarantineRestoreOperation(imported, {
        operation_id: "restore_imported",
      }),
    expectCode("phase-mismatch"),
    "an integrity label cannot roll back a target that may have been mutated",
  );
});

test("validated replacement archive can supersede an idle quarantine epoch", () => {
  const previous = archived();
  const replacement = {
    archive_id: "archive.002",
    object: "accounts/acct_test/attempt-002.tar.gz",
    cell: SOURCE,
    evacuation_id: "source_evacuation_002",
    epoch: 7,
  };
  const state = replaceArchivedLocation(previous, {
    archived: replacement,
    archive_id: replacement.archive_id,
  });

  assert.equal(state.location.archive_id, replacement.archive_id);
  assert.equal(state.location.object, replacement.object);
  assert.equal(state.epoch, 7);
  assert.equal(state.revision, previous.revision + 1);

  const busy = claimOperation(previous, {
    operation_id: "restore_busy",
    evacuation_id: "source_evacuation_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  assert.throws(
    () =>
      replaceArchivedLocation(busy, {
        archived: replacement,
        archive_id: replacement.archive_id,
      }),
    expectCode("operation-busy"),
  );
});

test("epochs advance only on claims and revisions only on durable changes", () => {
  const first = evacuateToArchive(live(), "evacuate_001");
  assert.equal(first.epoch, 1);
  const claimed = claimOperation(first, {
    operation_id: "restore_001",
    kind: "restore",
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  assert.equal(claimed.epoch, 2);
  assert.equal(claimed.revision, first.revision + 1);

  const retried = claimOperation(claimed, {
    operation_id: "restore_001",
    kind: "restore",
    source_cell: SOURCE,
    target_cell: TARGET,
    archive: ARCHIVE,
  });
  assert.equal(retried, claimed);
  assert.equal(retried.revision, claimed.revision);
});

test("persisted-state validation rejects broken authority and projections", () => {
  const state = live();
  assert.throws(
    () =>
      validateLifecycleState({
        ...state,
        location: {
          ...state.location,
          cell: TARGET,
        },
      }),
    expectCode("invalid-state"),
  );

  const claimed = claimOperation(state, {
    operation_id: "evacuate_001",
    kind: "evacuate",
    archive: ARCHIVE,
  });
  assert.throws(
    () =>
      validateLifecycleState({
        ...claimed,
        revision: -1,
      }),
    expectCode("invalid-state"),
  );
  assert.throws(
    () =>
      validateLifecycleState({
        ...claimed,
        epoch: 2,
      }),
    expectCode("invalid-state"),
  );
});
