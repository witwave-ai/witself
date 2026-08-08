import {
  AccountLifecycleBusyError,
  AccountLifecycleFence,
} from "./account-lifecycle-fence.mjs";
import {
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
} from "./account-lifecycle-state.mjs";
import {
  ArchiveIntegrityError,
  newArchiveObjectKey,
  validateCommittedAccountArchive,
} from "./archive-integrity.mjs";
import {
  accountBackupSchedulingEnabled,
  cellHasDestinationCredentials,
} from "./placement.mjs";
import {
  reconcileRealmEmailAliasesForAccountLifecycle,
} from "./realm-email-alias-runtime.mjs";
import {
  reconcileAgentEmailDomainsForAccountLifecycle,
} from "./agent-email-domain-runtime.mjs";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const EVACUATION_ID = /^[A-Za-z0-9_-]{1,128}$/;
const STATE_KEY = "account-lifecycle";
const RESTORE_QUARANTINE_KEY = "restore-quarantine";
const RESTORE_QUARANTINE_SCHEMA = "witself.restore-quarantine.v1";
const LIFECYCLE_RETRY_MS = 60_000;
const PENDING_ERROR = "account is pending — not suspendable";

function json(value, status = 200, headers = {}) {
  return new Response(JSON.stringify(value), {
    status,
    headers: {
      "Content-Type": "application/json",
      ...headers,
    },
  });
}

function errorResponse(message, status) {
  return json(
    {
      schema_version: "witself.v0",
      error: message,
    },
    status,
  );
}

export class AccountLifecycleRuntimeError extends Error {
  constructor(message, status = 500, options = {}) {
    super(message, options);
    this.name = "AccountLifecycleRuntimeError";
    this.status = status;
  }
}

class RestoreArchiveQuarantinedError
  extends AccountLifecycleRuntimeError {
  constructor(archive, options = {}) {
    super(
      `archive ${archive.archive_id} failed deterministic integrity validation and is quarantined`,
      422,
      options,
    );
    this.name = "RestoreArchiveQuarantinedError";
    this.terminal = true;
  }
}

function fail(message, status = 500, options = {}) {
  throw new AccountLifecycleRuntimeError(message, status, options);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

function exactArchive(left, right) {
  return left?.archive_id === right?.archive_id &&
    left?.object === right?.object;
}

function validExpectedEpoch(value) {
  return value === undefined || value === null ||
    (Number.isSafeInteger(value) && value >= 0);
}

function boundedReason(value, fallback) {
  if (typeof value !== "string" || value.trim() === "") {
    return fallback;
  }
  return value.trim().slice(0, 200);
}

async function responseBody(response) {
  const text = await response.text().catch(() => "");
  let body = null;
  try {
    body = JSON.parse(text);
  } catch {
    // A non-JSON response is never accepted as a lifecycle acknowledgement.
  }
  return { text, body };
}

function requireEvacuationAck(
  body,
  accountID,
  evacuationID,
  expectedRole,
  label,
) {
  if (
    !isObject(body) ||
    body.account_id !== accountID ||
    body.evacuation_id !== evacuationID ||
    body.evacuation_role !== expectedRole ||
    typeof body.status !== "string" ||
    body.status.length === 0
  ) {
    fail(
      `${label} returned 2xx without an exact evacuation acknowledgement`,
      502,
    );
  }
  return body;
}

function requireImportAck(body, accountID, evacuationID) {
  requireEvacuationAck(
    body,
    accountID,
    evacuationID,
    "target",
    "import",
  );
  if (body.aborted === true) {
    fail("import returned an aborted evacuation receipt", 502);
  }
  const allowed = body.evacuation_completed === true
    ? ["active", "suspended", "closed"]
    : ["suspended", "closed"];
  if (!allowed.includes(body.status)) {
    fail(`import acknowledged unexpected status ${body.status}`, 502);
  }
  return body;
}

function requireResumeAck(body, accountID, evacuationID) {
  requireEvacuationAck(
    body,
    accountID,
    evacuationID,
    "target",
    "resume",
  );
  if (
    !["active", "suspended", "closed"].includes(body.status) ||
    body.completed !== true ||
    body.aborted === true
  ) {
    fail("resume did not attest completed evacuation", 502);
  }
  return body;
}

function requireFinalizeAck(body, accountID, evacuationID) {
  if (
    !isObject(body) ||
    body.account_id !== accountID ||
    body.evacuation_id !== evacuationID ||
    body.finalized !== true ||
    !["suspended", "closed"].includes(body.source_status) ||
    typeof body.finalized_at !== "string" ||
    !Number.isFinite(Date.parse(body.finalized_at)) ||
    typeof body.already_finalized !== "boolean"
  ) {
    fail(
      "source finalization returned 2xx without an exact evacuation acknowledgement",
      502,
    );
  }
  return body;
}

function requireAbortAck(body, accountID, evacuationID) {
  requireEvacuationAck(
    body,
    accountID,
    evacuationID,
    "source",
    "abort evacuation",
  );
  if (body.aborted !== true) {
    fail("abort evacuation did not attest an aborted epoch", 502);
  }
  return body;
}

function operationMatchesInput(state, input) {
  const operation = state.operation;
  if (!operation) {
    return false;
  }
  if (
    input.action === "restore" &&
    operation.kind === "move" &&
    [
      "archive_projected",
      "route_retired",
      "target_imported",
      "target_resumed",
      "live_committed",
      "route_projected",
      "source_finalized",
      "archive_retired",
      "archive_cleaned",
      "restored_closed_committed",
      "closed_archive_projected",
      "closed_archive_retained",
    ].includes(operation.phase)
  ) {
    return operation.target_cell === input.cell_name &&
      (
        input.archive_object === undefined ||
        operation.archive.object === input.archive_object
      ) &&
      (
        input.archive_id === undefined ||
        operation.archive.archive_id === input.archive_id
      ) &&
      (
        input.expected_epoch === undefined ||
        input.expected_epoch === null ||
        input.expected_epoch === operation.epoch
      );
  }
  if (
    input.action === "restore" &&
    operation.kind === "evacuate" &&
    ["archive_projected", "route_retired"].includes(operation.phase)
  ) {
    return (
      input.archive_object === undefined ||
      operation.archive.object === input.archive_object
    ) && (
      input.archive_id === undefined ||
      operation.archive.archive_id === input.archive_id
    ) && (
      input.expected_epoch === undefined ||
      input.expected_epoch === null ||
      input.expected_epoch === operation.epoch
    );
  }
  if (operation.kind !== input.action) {
    return false;
  }
  if (
    input.expected_epoch !== undefined &&
    input.expected_epoch !== null &&
    operation.request_epoch !== input.expected_epoch
  ) {
    return false;
  }
  switch (input.action) {
    case "evacuate":
      return operation.source_cell === input.cell_name;
    case "restore":
      return operation.target_cell === input.cell_name &&
        (
          input.archive_object === undefined ||
          operation.archive.object === input.archive_object
        ) &&
        (
          input.archive_id === undefined ||
          operation.archive.archive_id === input.archive_id
        );
    case "move":
      return operation.source_cell === input.source_cell &&
        operation.target_cell === input.target_cell;
    case "close":
      return true;
    default:
      return false;
  }
}

function operationMetadata(state, metadata) {
  const operation = state.operation;
  return validateLifecycleState({
    ...state,
    revision: state.revision + 1,
    operation: {
      ...operation,
      ...metadata,
    },
  });
}

function archiveIdentity(pointer) {
  return {
    archive_id: pointer.archive_id ?? pointer.object,
    object: pointer.object,
  };
}

function validRestoreQuarantine(value, accountID) {
  return isObject(value) &&
    value.schema_version === RESTORE_QUARANTINE_SCHEMA &&
    value.account_id === accountID &&
    typeof value.archive_id === "string" &&
    value.archive_id.length >= 1 &&
    value.archive_id.length <= 1024 &&
    typeof value.object === "string" &&
    value.object.length >= 1 &&
    value.object.length <= 1024 &&
    value.reason === "archive_integrity" &&
    typeof value.quarantined_at === "string" &&
    Number.isFinite(Date.parse(value.quarantined_at));
}

function authoritativeProjectionEpoch(state) {
  if (
    state.location.kind === "live" &&
    Number.isSafeInteger(state.location.route?.epoch)
  ) {
    return state.location.route.epoch;
  }
  if (
    ["archived", "closed_archived"].includes(state.location.kind) &&
    Number.isSafeInteger(state.location.pointer?.epoch)
  ) {
    return state.location.pointer.epoch;
  }
  // Legacy projections did not carry an epoch. Their bootstrap state starts at
  // zero, so state.epoch remains the only available conservative CAS value.
  return state.epoch;
}

function residentRouteEpoch(state) {
  if (
    state.location.kind === "live" &&
    Number.isSafeInteger(state.location.route?.epoch)
  ) {
    return state.location.route.epoch;
  }
  // A claimed operation increments state.epoch before it retires a legacy
  // epoch-less source route. Its request_epoch remains the exact authority
  // that admitted the current source resident.
  if (
    state.location.kind === "live" &&
    state.operation?.source_cell === state.location.cell &&
    Number.isSafeInteger(state.operation.request_epoch)
  ) {
    return state.operation.request_epoch;
  }
  return state.epoch;
}

function routeForCell(cell, epoch) {
  return {
    cell: cell.name,
    endpoint: cell.endpoint,
    region: cell.region ?? null,
    region_code: cell.region_code ?? null,
    cell_registration_id:
      cell.registration_id ?? cell.registered_at ?? null,
    epoch,
  };
}

function pointerForArchive({
  operation,
  sourceCell,
  exportedAt,
  status,
  size,
  placementPolicy,
}) {
  return {
    cell: operation.source_cell,
    source_cell: operation.source_cell,
    region: sourceCell.region ?? null,
    region_code: sourceCell.region_code ?? null,
    object: operation.archive.object,
    archive_id: operation.archive.archive_id,
    evacuation_id: operation.evacuation_id,
    source_registration_id:
      operation.source_registration_id ?? null,
    source_route_epoch:
      Number.isSafeInteger(operation.request_epoch)
        ? operation.request_epoch
        : null,
    epoch: operation.epoch,
    exported_at: exportedAt,
    status,
    size,
    format_version: 1,
    placement_policy: placementPolicy ?? null,
  };
}

function canonicalClosed(accountID) {
  return json({
    schema_version: "witself.v0",
    account_id: accountID,
    status: "closed",
  });
}

/**
 * Cloudflare AccountLifecycle Durable Object implementation without a
 * dependency on @cloudflare/containers. index.js exposes this through the
 * exported Durable Object class; focused Node tests instantiate it directly.
 */
export class DurableAccountLifecycle {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.accountId = ctx.id?.name ?? null;
    this.fence = new AccountLifecycleFence();
    // Cloudflare's native fetch is an illegal-invocation Web API when it is
    // detached from globalThis and later called as an object method. Keep the
    // platform receiver explicit while leaving injected test fetches intact.
    this.fetchImpl =
      dependencies.fetch ?? ((...args) => globalThis.fetch(...args));
    this.randomUUID =
      dependencies.randomUUID ?? (() => globalThis.crypto.randomUUID());
    this.validateArchive =
      dependencies.validateArchive ?? validateCommittedAccountArchive;
    this.verifyArchive =
      dependencies.verifyArchive ?? validateCommittedAccountArchive;
    this.streamArchive =
      dependencies.streamArchive ?? streamToR2Multipart;
    this.targetCoordinatorRequest =
      dependencies.targetCoordinatorRequest ??
      ((cellName, path, payload) =>
        this.requestTargetCoordinator(cellName, path, payload));
    this.now = dependencies.now ?? (() => new Date());
    this.reconcileRealmEmailAliases =
      dependencies.reconcileRealmEmailAliases ??
      ((accountID, fence) =>
        reconcileRealmEmailAliasesForAccountLifecycle(
          this.env,
          accountID,
          fence,
        ));
    this.reconcileAgentEmailDomains =
      dependencies.reconcileAgentEmailDomains ??
      ((accountID, fence) =>
        reconcileAgentEmailDomainsForAccountLifecycle(
          this.env,
          accountID,
          fence,
        ));
  }

  async reconcileAgentEmailPolicies(accountID, fence) {
    if (fence.action === "republish") {
      await this.reconcileRealmEmailAliases(accountID, fence);
      await this.reconcileAgentEmailDomains(accountID, fence);
      return;
    }
    await this.reconcileAgentEmailDomains(accountID, fence);
    await this.reconcileRealmEmailAliases(accountID, fence);
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (
      request.method === "POST" &&
      url.pathname === "/reservation-status"
    ) {
      return this.reservationStatus(request);
    }
    if (
      request.method === "POST" &&
      url.pathname === "/residency-status"
    ) {
      return this.residencyStatus(request);
    }
    if (request.method !== "POST" || url.pathname !== "/run") {
      return errorResponse("account lifecycle endpoint not found", 404);
    }

    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid account lifecycle request", 400);
    }
    if (!this.validInput(input)) {
      return errorResponse("invalid account lifecycle request", 400);
    }

    try {
      return await this.fence.run(async () => {
        const result = await this.run(input);
        if (result instanceof Response) {
          return result;
        }
        return json({ ok: true, result: result ?? {} });
      });
    } catch (error) {
      if (error instanceof AccountLifecycleBusyError) {
        return errorResponse(error.message, 409);
      }
      const status = error instanceof AccountLifecycleRuntimeError
        ? error.status
        : 500;
      return errorResponse(String(error?.message ?? error), status);
    }
  }

  async reservationStatus(request) {
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid reservation status request", 400);
    }
    if (
      !isObject(input) ||
      input.account_id !== this.accountId ||
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      !EVACUATION_ID.test(input.operation_id ?? "") ||
      !EVACUATION_ID.test(input.evacuation_id ?? "") ||
      !CELL_NAME.test(input.target_cell ?? "")
    ) {
      return errorResponse("invalid reservation status request", 400);
    }
    let state;
    try {
      const stored = await this.storage.get(STATE_KEY);
      state = stored == null ? null : validateLifecycleState(stored);
    } catch {
      return errorResponse("account lifecycle state is unavailable", 503);
    }
    const operation = state?.operation;
    const active = Boolean(
      operation &&
      operation.operation_id === input.operation_id &&
      operation.evacuation_id === input.evacuation_id &&
      operation.target_cell === input.target_cell,
    );
    return json({
      ok: true,
      account_id: input.account_id,
      operation_id: input.operation_id,
      evacuation_id: input.evacuation_id,
      target_cell: input.target_cell,
      active,
      terminal: !active,
    });
  }

  async residencyStatus(request) {
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid residency status request", 400);
    }
    if (
      !isObject(input) ||
      input.account_id !== this.accountId ||
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      !CELL_NAME.test(input.cell_name ?? "") ||
      typeof input.registration_id !== "string" ||
      input.registration_id.length < 1 ||
      input.registration_id.length > 128 ||
      !Number.isSafeInteger(input.route_epoch) ||
      input.route_epoch < 0
    ) {
      return errorResponse("invalid residency status request", 400);
    }
    let state;
    try {
      const stored = await this.storage.get(STATE_KEY);
      if (stored == null) {
        // Signup residents predate AccountLifecycle initialization. Absence of
        // state is not proof that the account left this cell.
        return errorResponse(
          "account lifecycle authority is not initialized",
          409,
        );
      }
      state = validateLifecycleState(stored);
    } catch {
      return errorResponse("account lifecycle state is unavailable", 503);
    }
    const resident =
      state.location.kind === "live" &&
      state.location.cell === input.cell_name &&
      (
        !state.location.route?.cell_registration_id ||
        state.location.route.cell_registration_id ===
          input.registration_id
      ) &&
      residentRouteEpoch(state) === input.route_epoch;
    return json({
      ok: true,
      account_id: input.account_id,
      cell_name: input.cell_name,
      registration_id: input.registration_id,
      route_epoch: input.route_epoch,
      resident,
    });
  }

  validInput(input) {
    if (
      !isObject(input) ||
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      input.account_id !== this.accountId ||
      !["close", "evacuate", "restore", "move"].includes(input.action) ||
      !validExpectedEpoch(input.expected_epoch)
    ) {
      return false;
    }
    if (input.action === "close") {
      return typeof input.authorization === "string" &&
        input.authorization.length >= 1 &&
        input.authorization.length <= 8192 &&
        typeof input.body === "string" &&
        input.body.length <= 64 * 1024;
    }
    if (input.action === "move") {
      return CELL_NAME.test(input.source_cell ?? "") &&
        CELL_NAME.test(input.target_cell ?? "") &&
        input.source_cell !== input.target_cell;
    }
    if (!CELL_NAME.test(input.cell_name ?? "")) {
      return false;
    }
    return (
      input.archive_object === undefined ||
      (
        typeof input.archive_object === "string" &&
        input.archive_object.length >= 1 &&
        input.archive_object.length <= 1024
      )
    ) && (
      input.archive_id === undefined ||
      (
        typeof input.archive_id === "string" &&
        input.archive_id.length >= 1 &&
        input.archive_id.length <= 1024
      )
    );
  }

  async loadState() {
    const stored = await this.storage.get(STATE_KEY);
    if (stored !== undefined && stored !== null) {
      const state = validateLifecycleState(stored);
      if (state.account_id !== this.accountId) {
        fail("durable lifecycle state belongs to a different account", 500);
      }
      return state;
    }

    const [route, archived] = await Promise.all([
      this.env.DIRECTORY.get(`acct:${this.accountId}`, { type: "json" }),
      this.env.DIRECTORY.get(`archived:${this.accountId}`, { type: "json" }),
    ]);
    if (route && archived) {
      fail(
        `account ${this.accountId} has both live and archived projections; manual reconciliation is required`,
        409,
      );
    }
    if (!route && !archived) {
      return null;
    }

    let state;
    if (route) {
      state = bootstrapLiveState({
        account_id: this.accountId,
        route,
      });
    } else {
      if (!this.env.ARCHIVES) {
        fail("R2 archive binding is unavailable", 503);
      }
      if (
        archived.evacuation_id !== undefined &&
        (
          typeof archived.evacuation_id !== "string" ||
          !EVACUATION_ID.test(archived.evacuation_id)
        )
      ) {
        fail("archived projection carries an invalid evacuation id", 409);
      }
      if (
        typeof archived.object !== "string" ||
        archived.object.length === 0
      ) {
        fail("archived projection has no immutable object", 409);
      }
      const expectedEvacuationID =
        typeof archived.evacuation_id === "string" &&
          EVACUATION_ID.test(archived.evacuation_id)
          ? archived.evacuation_id
          : undefined;
      const authoritative = await this.validateArchivedProjection(
        archived,
        expectedEvacuationID,
      );
      state = bootstrapArchivedState({
        account_id: this.accountId,
        archived: authoritative,
      });
    }
    await this.saveState(state);
    return state;
  }

  async restoreQuarantine() {
    const quarantine = await this.storage.get(RESTORE_QUARANTINE_KEY);
    if (quarantine === undefined || quarantine === null) {
      return null;
    }
    if (!validRestoreQuarantine(quarantine, this.accountId)) {
      fail("durable restore quarantine state is invalid", 500);
    }
    return quarantine;
  }

  async quarantineArchive(archive) {
    const quarantine = {
      schema_version: RESTORE_QUARANTINE_SCHEMA,
      account_id: this.accountId,
      archive_id: archive.archive_id,
      object: archive.object,
      reason: "archive_integrity",
      quarantined_at: this.now().toISOString(),
    };
    if (!validRestoreQuarantine(quarantine, this.accountId)) {
      fail("refusing to persist invalid restore quarantine state", 500);
    }
    await this.storage.put(RESTORE_QUARANTINE_KEY, quarantine);
    return quarantine;
  }

  async validateArchivedProjection(
    archived,
    expectedEvacuationID = undefined,
  ) {
    const archive = archiveIdentity(archived);
    const quarantine = await this.restoreQuarantine();
    if (quarantine && exactArchive(quarantine, archive)) {
      throw new RestoreArchiveQuarantinedError(archive);
    }

    let verification;
    try {
      verification = await this.validateArchive(
        this.env.ARCHIVES,
        archived.object,
        this.accountId,
        expectedEvacuationID
          ? {
              evacuationID: expectedEvacuationID,
              allowLegacyEvacuationID: true,
            }
          : {},
      );
    } catch (error) {
      if (!(error instanceof ArchiveIntegrityError)) {
        throw error;
      }
      await this.quarantineArchive(archive);
      throw new RestoreArchiveQuarantinedError(
        archive,
        { cause: error },
      );
    }

    const manifestEvacuationID = verification?.manifest?.evacuation_id;
    if (
      manifestEvacuationID !== undefined &&
      manifestEvacuationID !== "" &&
      !EVACUATION_ID.test(manifestEvacuationID)
    ) {
      fail("archive manifest carries an invalid evacuation id", 409);
    }
    if (
      !manifestEvacuationID &&
      Number.isInteger(verification?.manifest?.schema_version) &&
      verification.manifest.schema_version >= 70
    ) {
      fail(
        "current-schema archive is missing its evacuation id",
        409,
      );
    }
    if (quarantine && !exactArchive(quarantine, archive)) {
      await this.storage.delete(RESTORE_QUARANTINE_KEY);
    }
    return {
      ...archived,
      archive_schema_version:
        verification?.manifest?.schema_version ?? null,
      ...(manifestEvacuationID
        ? { evacuation_id: manifestEvacuationID }
        : {}),
    };
  }

  async prepareArchivedRestore(state, input) {
    if (
      input.action !== "restore" ||
      state.operation !== null ||
      state.location.kind !== "archived"
    ) {
      return state;
    }
    const projected = await this.env.DIRECTORY.get(
      `archived:${this.accountId}`,
      { type: "json" },
    );
    if (!projected) {
      return state;
    }
    const projectedArchive = archiveIdentity(projected);
    if (
      (
        input.archive_object !== undefined &&
        input.archive_object !== projectedArchive.object
      ) ||
      (
        input.archive_id !== undefined &&
        input.archive_id !== projectedArchive.archive_id
      )
    ) {
      fail("archive changed before restore", 409);
    }
    if (exactArchive(projectedArchive, archiveIdentity(state.location))) {
      const quarantine = await this.restoreQuarantine();
      if (quarantine && exactArchive(quarantine, projectedArchive)) {
        throw new RestoreArchiveQuarantinedError(projectedArchive);
      }
      return state;
    }

    if (
      projected.evacuation_id !== undefined &&
      (
        typeof projected.evacuation_id !== "string" ||
        !EVACUATION_ID.test(projected.evacuation_id)
      )
    ) {
      fail("archived projection carries an invalid evacuation id", 409);
    }
    if (
      typeof projected.object !== "string" ||
      projected.object.length === 0
    ) {
      fail("archived projection has no immutable object", 409);
    }
    const expectedEvacuationID =
      typeof projected.evacuation_id === "string"
        ? projected.evacuation_id
        : undefined;
    const authoritative = await this.validateArchivedProjection(
      projected,
      expectedEvacuationID,
    );
    state = replaceArchivedLocation(state, {
      archived: authoritative,
      archive_id: projectedArchive.archive_id,
    });
    return this.saveState(state);
  }

  async saveState(state) {
    validateLifecycleState(state);
    await this.storage.put(STATE_KEY, state);
    return state;
  }

  async cell(
    name,
    {
      target = false,
      evacuationProtocol = false,
      expectedRegistrationID = null,
    } = {},
  ) {
    const cell = await this.env.DIRECTORY.get(`cell:${name}`, {
      type: "json",
    });
    if (!cell?.endpoint || !cell?.provision_token) {
      fail(`cell ${name} is unavailable for account lifecycle work`, 502);
    }
    if (
      target &&
      (
        cell.accepting === false ||
        cell.backup_validation_target === true
      )
    ) {
      fail(
        `cell ${name} is drained or reserved for backup validation and cannot accept a restore`,
        409,
      );
    }
    if (
      target &&
      !cellHasDestinationCredentials(cell, {
        backupsEnabled: accountBackupSchedulingEnabled(this.env),
      })
    ) {
      fail(
        accountBackupSchedulingEnabled(this.env)
          ? `cell ${name} lacks distinct provision and backup credentials required while account backups are enabled`
          : `cell ${name} lacks a provision credential`,
        409,
      );
    }
    if (
      expectedRegistrationID &&
      (cell.registration_id ?? cell.registered_at) !==
        expectedRegistrationID
    ) {
      fail(`cell ${name} registration changed`, 409);
    }
    const resolved = { ...cell, name };
    if (evacuationProtocol) {
      await this.requireEvacuationProtocol(resolved);
    }
    return resolved;
  }

  async requestTargetCoordinator(cellName, path, payload) {
    if (!this.env.CELL_COORDINATOR) {
      fail("target cell coordinator Durable Object is unavailable", 503);
    }
    const id = this.env.CELL_COORDINATOR.idFromName(cellName);
    let response;
    try {
      response = await this.env.CELL_COORDINATOR.get(id).fetch(
        new Request(`https://target-cell.internal${path}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            cell_name: cellName,
            ...payload,
          }),
        }),
      );
    } catch (error) {
      fail(
        `target cell coordinator outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
        { cause: error },
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok || body?.ok !== true) {
      fail(
        body?.error ??
          `target cell coordinator ${response.status}: ${text.slice(0, 200)}`,
        response.ok ? 502 : response.status,
      );
    }
    return body;
  }

  async requireEvacuationProtocol(cell) {
    let response;
    try {
      response = await this.fetchImpl(`${cell.endpoint}/v1/version`, {
        method: "GET",
        signal: AbortSignal.timeout(10_000),
      });
    } catch (error) {
      fail(
        `cell ${cell.name} evacuation protocol probe failed: ${String(error?.message ?? error)}`,
        502,
        { cause: error },
      );
    }
    const { text, body } = await responseBody(response);
    if (
      !response.ok ||
      !isObject(body) ||
      body.account_evacuation_protocol !== 1
    ) {
      fail(
        `cell ${cell.name} does not attest account evacuation protocol 1: HTTP ${response.status} ${text.slice(0, 120)}`,
        409,
      );
    }
  }

  async run(input) {
    let state = await this.loadState();
    if (!state) {
      fail(`unknown account ${this.accountId}`, 404);
    }
    state = await this.prepareArchivedRestore(state, input);

    let finishEvacuationBeforeRestore = false;
    if (state.operation) {
      if (!operationMatchesInput(state, input)) {
        fail(
          `operation ${state.operation.operation_id} already owns the account lifecycle`,
          409,
        );
      }
      finishEvacuationBeforeRestore =
        input.action === "restore" &&
        state.operation.kind === "evacuate";
    } else {
      const terminal = await this.terminalReplay(state, input);
      if (terminal !== null) {
        return terminal;
      }
      state = await this.claim(state, input);
    }

    if (input.action === "close") {
      return this.runClose(state, input);
    }
    const result = await this.runMove(state);
    if (finishEvacuationBeforeRestore) {
      return this.run(input);
    }
    return result;
  }

  async terminalReplay(state, input) {
    await this.releaseCompletedTargetReservation(state);
    if (
      state.location.kind === "closed_archived" &&
      ["restore", "close"].includes(input.action)
    ) {
      const pointer = state.location.pointer;
      if (
        input.action === "restore" &&
        (
          (
            input.archive_object !== undefined &&
            input.archive_object !== state.location.object
          ) ||
          (
            input.archive_id !== undefined &&
            input.archive_id !== state.location.archive_id
          )
        )
      ) {
        fail("closed archive changed before terminal replay", 409);
      }
      await this.validateArchive(
        this.env.ARCHIVES,
        state.location.object,
        this.accountId,
        {
          evacuationID: pointer.evacuation_id,
          allowLegacyEvacuationID: true,
        },
      );
      const route = await this.env.DIRECTORY.get(
        `acct:${this.accountId}`,
        { type: "json" },
      );
      if (route) {
        if (
          Number.isSafeInteger(route.epoch) &&
          route.epoch > state.epoch
        ) {
          fail(
            "closed archive authority conflicts with a newer live route",
            409,
          );
        }
      }
      await this.env.DIRECTORY.put(
        `archived:${this.accountId}`,
        JSON.stringify(pointer),
      );
      await this.reconcileAgentEmailPolicies(this.accountId, {
        operation_id:
          state.last_completed?.operation_id ?? "bootstrap",
        epoch: state.epoch,
        action: "retire",
      });
      if (route) {
        await this.env.DIRECTORY.delete(`acct:${this.accountId}`);
      }
      await this.env.DIRECTORY.delete(`pending:${this.accountId}`);
      return input.action === "close"
        ? canonicalClosed(this.accountId)
        : { already_closed: true, archive_retained: true };
    }
    if (
      input.action === "restore" &&
      state.location.kind === "live" &&
      state.location.cell === input.cell_name
    ) {
      await this.env.DIRECTORY.put(
        `acct:${this.accountId}`,
        JSON.stringify(state.location.route),
      );
      await this.reconcileAgentEmailPolicies(this.accountId, {
        operation_id:
          state.last_completed?.operation_id ?? "bootstrap",
        epoch: state.epoch,
        action: "republish",
      });
      const archived = await this.env.DIRECTORY.get(
        `archived:${this.accountId}`,
        { type: "json" },
      );
      if (archived) {
        const completedArchive = state.last_completed?.archive;
        if (
          !completedArchive ||
          !exactArchive(
            archiveIdentity(archived),
            completedArchive,
          )
        ) {
          fail(
            "live authority conflicts with a different archived projection",
            409,
          );
        }
        await this.env.DIRECTORY.delete(`archived:${this.accountId}`);
      }
      return { already_routed: true };
    }
    if (
      input.action === "evacuate" &&
      state.location.kind === "archived" &&
      state.location.source_cell === input.cell_name
    ) {
      const pointer = state.location.pointer;
      await this.validateArchive(
        this.env.ARCHIVES,
        state.location.object,
        this.accountId,
        pointer.evacuation_id
          ? {
              evacuationID: pointer.evacuation_id,
              allowLegacyEvacuationID: true,
            }
          : {},
      );
      await this.env.DIRECTORY.put(
        `archived:${this.accountId}`,
        JSON.stringify(pointer),
      );
      const route = await this.env.DIRECTORY.get(
        `acct:${this.accountId}`,
        { type: "json" },
      );
      if (route) {
        if (
          route.cell !== state.location.source_cell ||
          (
            Number.isSafeInteger(route.epoch) &&
            route.epoch > state.epoch
          )
        ) {
          fail(
            "archived authority conflicts with a different route projection",
            409,
          );
        }
      }
      await this.reconcileAgentEmailPolicies(this.accountId, {
        operation_id:
          state.last_completed?.operation_id ?? "bootstrap",
        epoch: state.epoch,
        action: "suspend",
      });
      if (route) {
        await this.env.DIRECTORY.delete(`acct:${this.accountId}`);
      }
      return { already_archived: true };
    }
    if (input.action === "close" && state.location.kind === "closed") {
      await this.reconcileAgentEmailPolicies(this.accountId, {
        operation_id:
          state.last_completed?.operation_id ?? "bootstrap",
        epoch: state.epoch,
        action: "retire",
      });
      await this.env.DIRECTORY.delete(`acct:${this.accountId}`);
      await this.env.DIRECTORY.delete(`pending:${this.accountId}`);
      return canonicalClosed(this.accountId);
    }
    return null;
  }

  async claim(state, input) {
    await this.releaseCompletedTargetReservation(state);
    const projectionEpoch = authoritativeProjectionEpoch(state);
    if (
      input.expected_epoch !== undefined &&
      input.expected_epoch !== null &&
      input.expected_epoch !== projectionEpoch
    ) {
      fail(
        `lifecycle projection epoch changed from ${input.expected_epoch} to ${projectionEpoch}`,
        409,
      );
    }

    let sourceRegistrationID = null;
    if (
      ["evacuate", "move", "close"].includes(input.action) &&
      state.location.kind === "live"
    ) {
      const sourceName = input.action === "evacuate"
        ? input.cell_name
        : input.action === "move"
        ? input.source_cell
        : state.location.cell;
      const source = await this.cell(sourceName);
      sourceRegistrationID =
        source.registration_id ?? source.registered_at ?? null;
      const routedRegistrationID =
        state.location.kind === "live"
          ? state.location.route?.cell_registration_id
          : null;
      if (
        routedRegistrationID &&
        sourceRegistrationID !== routedRegistrationID
      ) {
        fail(`source cell ${sourceName} registration changed`, 409);
      }
    } else if (state.location.kind === "archived") {
      sourceRegistrationID =
        state.location.pointer?.source_registration_id ?? null;
    }

    let targetPreflight = null;
    if (input.action === "move" || input.action === "restore") {
      const targetName =
        input.action === "move" ? input.target_cell : input.cell_name;
      const target = await this.cell(targetName, {
        target: true,
        evacuationProtocol: true,
      });
      targetPreflight = {
        cell: target.name,
        endpoint: target.endpoint,
        protocol: 1,
        registration_id:
          target.registration_id ?? target.registered_at ?? null,
        accepted_at: this.now().toISOString(),
      };
      if (!targetPreflight.registration_id) {
        fail(`target cell ${targetName} has no registration generation`, 409);
      }
    }

    const operationID = this.randomUUID();
    let claim;
    let metadata = {
      request_epoch: input.expected_epoch ?? projectionEpoch,
      ...(sourceRegistrationID
        ? { source_registration_id: sourceRegistrationID }
        : {}),
      ...(targetPreflight ? { target_preflight: targetPreflight } : {}),
      ...(targetPreflight
        ? { target_registration_id: targetPreflight.registration_id }
        : {}),
    };
    switch (input.action) {
      case "evacuate": {
        const archive = {
          archive_id: operationID,
          object: newArchiveObjectKey(this.accountId, operationID),
        };
        claim = {
          operation_id: operationID,
          evacuation_id: operationID,
          kind: "evacuate",
          source_cell: input.cell_name,
          archive,
        };
        metadata = {
          ...metadata,
          reason: boundedReason(input.reason, "cell decommission"),
          allow_pending_reap: input.allow_pending_reap !== false,
        };
        break;
      }
      case "move": {
        const archive = {
          archive_id: operationID,
          object: newArchiveObjectKey(this.accountId, operationID),
        };
        claim = {
          operation_id: operationID,
          evacuation_id: operationID,
          kind: "move",
          source_cell: input.source_cell,
          target_cell: input.target_cell,
          archive,
        };
        metadata = {
          ...metadata,
          reason: boundedReason(input.reason, "placement rebalance"),
          allow_pending_reap: false,
        };
        break;
      }
      case "restore": {
        if (state.location.kind !== "archived") {
          fail("restore requires an authoritative archive", 409);
        }
        const archive = archiveIdentity(state.location);
        if (
          input.archive_object !== undefined &&
          input.archive_object !== archive.object
        ) {
          fail("archive object changed before restore", 409);
        }
        if (
          input.archive_id !== undefined &&
          input.archive_id !== archive.archive_id
        ) {
          fail("archive id changed before restore", 409);
        }
        const archivedProjection = await this.env.DIRECTORY.get(
          `archived:${this.accountId}`,
          { type: "json" },
        );
        if (
          archivedProjection &&
          exactArchive(archiveIdentity(archivedProjection), archive)
        ) {
          state = validateLifecycleState({
            ...state,
            revision: state.revision + 1,
            location: {
              ...state.location,
              pointer: {
                ...state.location.pointer,
                ...archivedProjection,
              },
            },
          });
          await this.saveState(state);
        }
        const evacuationID =
          state.location.pointer.evacuation_id ?? operationID;
        claim = {
          operation_id: operationID,
          evacuation_id: evacuationID,
          kind: "restore",
          source_cell: state.location.source_cell,
          target_cell: input.cell_name,
          archive,
        };
        metadata = {
          ...metadata,
          ...(state.location.pointer.source_registration_id
            ? {
                source_registration_id:
                  state.location.pointer.source_registration_id,
              }
            : {}),
          ...(Number.isSafeInteger(
            state.location.pointer.source_route_epoch,
          )
            ? {
                source_route_epoch:
                  state.location.pointer.source_route_epoch,
              }
            : {}),
          legacy_evacuation_id:
            state.location.pointer.evacuation_id === undefined,
          placement_policy:
            state.location.pointer.placement_policy ?? null,
          source_exported_at:
            state.location.pointer.exported_at ?? null,
        };
        break;
      }
      case "close":
        if (state.location.kind !== "live") {
          fail("archived account must be restored before close", 409);
        }
        claim = {
          operation_id: operationID,
          kind: "close",
          source_cell: state.location.cell,
        };
        break;
      default:
        fail("unsupported lifecycle action", 400);
    }
    state = claimOperation(state, claim);
    state = operationMetadata(state, metadata);
    // Arm durable resumption before the claim becomes authoritative. An alarm
    // without a claim is harmless; a claim without an alarm can become
    // undiscoverable after its last KV projection is retired.
    await this.scheduleWakeup();
    await this.saveState(state);
    return state;
  }

  async runMove(initialState) {
    let state = initialState;
    while (state.operation) {
      if (
        state.operation.kind === "restore" &&
        ["claimed", "target_reserved"].includes(
          state.operation.phase,
        )
      ) {
        const quarantine = await this.restoreQuarantine();
        if (
          quarantine &&
          exactArchive(quarantine, state.operation.archive)
        ) {
          await this.settleQuarantinedRestore(state);
        }
      }
      if (
        state.operation.target_cell &&
        state.operation.phase !== "claimed"
      ) {
        await this.renewTargetReservation(state.operation);
      }
      const step = nextLifecycleStep(state);
      switch (step.action) {
        case "reserve_target":
          state = await this.reserveTarget(state);
          break;
        case "suspend_source":
          state = await this.suspendSource(state);
          break;
        case "export_validate_and_commit":
          state = await this.exportArchive(state);
          break;
        case "import_target":
          state = await this.importTarget(state);
          break;
        case "resume_target":
          state = await this.resumeTarget(state);
          break;
        case "commit_live":
          state = await this.commitTargetRoute(state);
          break;
        case "commit_restored_closed":
          state = commitRestoredClosed(state, {
            operation_id: state.operation.operation_id,
          });
          state = await this.saveState(state);
          break;
        case "finalize_source":
          state = await this.finalizeSource(state);
          break;
        case "complete":
          state = await this.complete(state);
          break;
        default:
          if (step.type === "projection") {
            state = await this.applyProjection(state, step);
            break;
          }
          if (
            step.type === "cleanup" &&
            step.target === "archive_object"
          ) {
            try {
              state = await this.cleanupArchive(state);
            } catch (error) {
              await this.scheduleWakeup();
              return {
                cleanup_pending: true,
                error: String(error?.message ?? error),
              };
            }
            break;
          }
          fail(`unsupported lifecycle step ${step.action}`, 500);
      }
    }
    return {
      ...(state.last_completed?.outcome === "reaped"
        ? { reaped: true }
        : {}),
      operation_id: state.last_completed?.operation_id,
      evacuation_id: state.last_completed?.evacuation_id,
      epoch: state.last_completed?.epoch,
      target_cell: state.last_completed?.target_cell,
    };
  }

  async reserveTarget(state) {
    const operation = state.operation;
    const reservation = this.targetReservation(operation);
    let receipt;
    try {
      receipt = await this.targetCoordinatorRequest(
        operation.target_cell,
        "/reserve",
        { reservation },
      );
    } catch (error) {
      // A 4xx is a definitive refusal before source mutation, so retire the
      // claim. A transport/5xx outcome is ambiguous: the reservation may have
      // committed, and the exact claim/alarm must retry it.
      if (
        error instanceof AccountLifecycleRuntimeError &&
        error.status >= 400 &&
        error.status < 500
      ) {
        state = cancelOperation(state, {
          operation_id: operation.operation_id,
        });
        await this.saveState(state);
        if (typeof this.storage.deleteAlarm === "function") {
          await this.storage.deleteAlarm().catch(() => {});
        }
      }
      throw error;
    }

    state = operationMetadata(state, {
      target_reservation: {
        ...reservation,
        expires_at: receipt.expires_at,
      },
    });
    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase: "claimed",
      to_phase: "target_reserved",
    });
    return this.saveState(state);
  }

  async releaseTargetReservation(operation) {
    if (
      !operation?.target_cell ||
      !operation?.target_registration_id ||
      !operation?.evacuation_id
    ) {
      return;
    }
    await this.targetCoordinatorRequest(
      operation.target_cell,
      "/release",
      {
        reservation: this.targetReservation(operation),
      },
    );
  }

  async releaseCompletedTargetReservation(state) {
    const completed = state?.last_completed;
    if (!completed?.target_cell) {
      return;
    }
    await this.releaseTargetReservation(completed);
  }

  async renewTargetReservation(operation) {
    if (!operation?.target_cell || !operation?.target_registration_id) {
      fail("active target operation has no registration generation", 500);
    }
    await this.targetCoordinatorRequest(
      operation.target_cell,
      "/reserve",
      {
        reservation: this.targetReservation(operation),
      },
    );
  }

  targetReservation(operation) {
    return {
      account_id: this.accountId,
      operation_id: operation.operation_id,
      evacuation_id: operation.evacuation_id,
      kind: operation.kind,
      target_cell: operation.target_cell,
      target_registration_id: operation.target_registration_id,
      epoch: operation.epoch,
    };
  }

  async promoteTargetResident(operation) {
    await this.targetCoordinatorRequest(
      operation.target_cell,
      "/promote",
      { reservation: this.targetReservation(operation) },
    );
  }

  async departSourceResident(operation) {
    if (!operation?.source_cell || !operation?.source_registration_id) {
      return;
    }
    const sourceRouteEpoch =
      operation.kind === "restore" &&
        Number.isSafeInteger(operation.source_route_epoch)
        ? operation.source_route_epoch
        : operation.request_epoch;
    if (
      !Number.isSafeInteger(sourceRouteEpoch) ||
      sourceRouteEpoch < 0
    ) {
      fail("active source operation has no route authority epoch", 500);
    }
    await this.targetCoordinatorRequest(
      operation.source_cell,
      "/depart",
      {
        account_id: this.accountId,
        operation_id: operation.operation_id,
        registration_id: operation.source_registration_id,
        source_epoch: sourceRouteEpoch,
      },
    );
  }

  async suspendSource(state) {
    const operation = state.operation;
    const cell = await this.cell(operation.source_cell, {
      evacuationProtocol: true,
      expectedRegistrationID: operation.source_registration_id,
    });
    let response;
    try {
      response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${this.accountId}:begin-evacuation`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
          },
          body: JSON.stringify({
            for: "evacuation",
            reason: operation.reason,
            evacuation_id: operation.evacuation_id,
          }),
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch (error) {
      fail(
        `begin evacuation outcome is ambiguous: ${String(error?.message ?? error)}`,
        502,
        { cause: error },
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok) {
      if (
        response.status === 409 &&
        body?.error === PENDING_ERROR
      ) {
        if (
          operation.kind !== "evacuate" ||
          operation.allow_pending_reap !== true
        ) {
          fail("account is pending — not eligible for this operation", 409);
        }
        await this.reapPending(cell);
        state = commitReaped(state, {
          operation_id: operation.operation_id,
        });
        return this.saveState(state);
      }
      fail(
        `begin evacuation ${response.status}: ${text.slice(0, 200)}`,
        response.status,
      );
    }
    requireEvacuationAck(
      body,
      this.accountId,
      operation.evacuation_id,
      "source",
      "begin evacuation",
    );
    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase:
        operation.kind === "move" ? "target_reserved" : "claimed",
      to_phase: "source_suspended",
    });
    return this.saveState(state);
  }

  async reapPending(cell) {
    const response = await this.fetchImpl(
      `${cell.endpoint}/v1/accounts/${this.accountId}:reap`,
      {
        method: "POST",
        headers: {
          Authorization: `Bearer ${cell.provision_token}`,
        },
        signal: AbortSignal.timeout(15_000),
      },
    );
    const { text, body } = await responseBody(response);
    if (
      !response.ok ||
      body?.account_id !== this.accountId ||
      body?.status !== "closed"
    ) {
      fail(
        `reap-pending ${response.status}: ${text.slice(0, 200)}`,
        response.ok ? 502 : response.status,
      );
    }
  }

  async exportArchive(state) {
    const operation = state.operation;
    const cell = await this.cell(operation.source_cell, {
      evacuationProtocol: true,
      expectedRegistrationID: operation.source_registration_id,
    });
    let placementPolicy;
    let pointer;
    try {
      placementPolicy = await this.readPlacementPolicy(
        cell,
        this.accountId,
      );
      const response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${this.accountId}:export-evacuation`,
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${cell.provision_token}`,
            "X-Witself-Evacuation-ID": operation.evacuation_id,
          },
          signal: AbortSignal.timeout(300_000),
        },
      );
      if (!response.ok || !response.body) {
        const { text } = await responseBody(response);
        fail(
          `export ${response.status}: ${text.slice(0, 200)}`,
          [404, 405].includes(response.status) ? 502 : response.status,
        );
      }
      const streamedAt = this.now().toISOString();
      const size = await this.streamArchive(
        this.env.ARCHIVES,
        operation.archive.object,
        response.body,
        {
          httpMetadata: {
            contentType: "application/gzip",
            contentDisposition:
              `attachment; filename="${this.accountId}.tar.gz"`,
          },
          customMetadata: {
            account_id: this.accountId,
            cell: operation.source_cell,
            evacuation_id: operation.evacuation_id,
            exported_at: streamedAt,
          },
        },
      );
      const verification = await this.verifyArchive(
        this.env.ARCHIVES,
        operation.archive.object,
        this.accountId,
        {
          evacuationID: operation.evacuation_id,
          allowLegacyEvacuationID: false,
        },
      );
      const manifest = verification?.manifest;
      const exportedAt = manifest?.exported_at;
      const archiveStatus = manifest?.status;
      if (
        typeof exportedAt !== "string" ||
        !Number.isFinite(Date.parse(exportedAt)) ||
        !["suspended", "closed"].includes(archiveStatus)
      ) {
        fail(
          "verified archive manifest is missing exported_at or frozen status",
          502,
        );
      }
      pointer = pointerForArchive({
        operation,
        sourceCell: cell,
        exportedAt,
        status: archiveStatus,
        size,
        placementPolicy,
      });
    } catch (error) {
      // Before durable archive authority exists, give the source back only
      // after its exact abort receipt is durably recorded. Cleanup follows
      // that receipt. Always delete the attempt-unique full object key: R2
      // multipart complete may have committed the object before its response
      // was lost, in which case the streaming helper never returned success.
      await this.abortPreArchive(state, cell, error);
      throw error;
    }

    // This is the no-rollback boundary. Durable Object storage may commit a
    // put even if the caller loses its acknowledgement. Once commitArchived
    // or saveState begins, never delete the object or abort the source: an
    // exact retry must inspect persisted state and continue projection.
    state = operationMetadata(state, {
      placement_policy: placementPolicy,
      source_exported_at: pointer.exported_at,
    });
    state = commitArchived(state, {
      operation_id: operation.operation_id,
      archived: pointer,
    });
    return this.saveState(state);
  }

  async abortPreArchive(state, cell, originalError) {
    const operation = state.operation;
    if (
      !operation ||
      !["evacuate", "move"].includes(operation.kind) ||
      !["claimed", "source_suspended"].includes(operation.phase)
    ) {
      return;
    }
    let response;
    try {
      response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${this.accountId}:abort-evacuation`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
          },
          body: JSON.stringify({
            evacuation_id: operation.evacuation_id,
          }),
          signal: AbortSignal.timeout(15_000),
        },
      );
    } catch (abortError) {
      fail(
        `${String(originalError?.message ?? originalError)}; abort outcome is ambiguous: ${String(abortError?.message ?? abortError)}`,
        502,
        { cause: originalError },
      );
    }
    const { text, body } = await responseBody(response);
    if (!response.ok) {
      fail(
        `${String(originalError?.message ?? originalError)}; abort failed ${response.status}: ${text.slice(0, 200)}`,
        502,
        { cause: originalError },
      );
    }
    requireAbortAck(
      body,
      this.accountId,
      operation.evacuation_id,
    );
    await this.env.ARCHIVES.delete(operation.archive.object).catch(
      () => {},
    );
    await this.releaseTargetReservation(operation);
    state = abortOperation(state, {
      operation_id: operation.operation_id,
    });
    await this.saveState(state);
    if (typeof this.storage.deleteAlarm === "function") {
      await this.storage.deleteAlarm().catch(() => {});
    }
  }

  async importTarget(state) {
    const operation = state.operation;
    const cell = await this.cell(operation.target_cell, {
      evacuationProtocol: true,
      expectedRegistrationID: operation.target_registration_id,
    });
    try {
      await this.validateArchive(
        this.env.ARCHIVES,
        operation.archive.object,
        this.accountId,
        {
          evacuationID: operation.evacuation_id,
          allowLegacyEvacuationID:
            operation.legacy_evacuation_id === true,
        },
      );
    } catch (error) {
      if (!(error instanceof ArchiveIntegrityError)) {
        throw error;
      }
      await this.quarantineArchive(operation.archive);
      await this.settleQuarantinedRestore(state, error);
    }
    const object = await this.env.ARCHIVES.get(operation.archive.object);
    if (!object?.body) {
      fail(`archive ${operation.archive.object} is not readable from R2`, 502);
    }
    const response = await this.fetchImpl(
      `${cell.endpoint}/v1/accounts/${this.accountId}:import-evacuation`,
      {
        method: "POST",
        headers: {
          Authorization: `Bearer ${cell.provision_token}`,
          "Content-Type": "application/octet-stream",
          "X-Witself-Evacuation-ID": operation.evacuation_id,
        },
        body: object.body,
        signal: AbortSignal.timeout(300_000),
      },
    );
    const { text, body } = await responseBody(response);
    if (!response.ok) {
      fail(
        `import ${response.status}: ${text.slice(0, 200)}`,
        [404, 405].includes(response.status) ? 502 : response.status,
      );
    }
    requireImportAck(
      body,
      this.accountId,
      operation.evacuation_id,
    );
    state = operationMetadata(state, {
      imported_status: body.status,
      ...(body.evacuation_completed === true
        ? { restored_status: body.status }
        : {}),
    });
    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase:
        operation.kind === "move" ? "route_retired" : "target_reserved",
      to_phase: "target_imported",
    });
    if (body.evacuation_completed === true) {
      state = acknowledgeStep(state, {
        operation_id: operation.operation_id,
        from_phase: "target_imported",
        to_phase: "target_resumed",
      });
    }
    return this.saveState(state);
  }

  async settleQuarantinedRestore(state, cause = undefined) {
    const operation = state.operation;
    if (
      operation?.kind !== "restore" ||
      !["claimed", "target_reserved"].includes(operation.phase)
    ) {
      fail(
        "archive quarantine reached a restore after target mutation",
        500,
      );
    }
    await this.releaseTargetReservation(operation);
    state = quarantineRestoreOperation(state, {
      operation_id: operation.operation_id,
    });
    await this.saveState(state);
    if (typeof this.storage.deleteAlarm === "function") {
      await this.storage.deleteAlarm().catch(() => {});
    }
    throw new RestoreArchiveQuarantinedError(
      operation.archive,
      cause === undefined ? {} : { cause },
    );
  }

  async resumeTarget(state) {
    const operation = state.operation;
    const cell = await this.cell(operation.target_cell, {
      evacuationProtocol: true,
      expectedRegistrationID: operation.target_registration_id,
    });
    if (operation.placement_policy) {
      const policyResponse = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${this.accountId}:restore-placement-policy`,
        {
          method: "PATCH",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${cell.provision_token}`,
            "X-Witself-Evacuation-ID": operation.evacuation_id,
          },
          body: JSON.stringify(operation.placement_policy),
          signal: AbortSignal.timeout(15_000),
        },
      );
      const { text, body } = await responseBody(policyResponse);
      if (!policyResponse.ok) {
        fail(
          `placement policy ${policyResponse.status}: ${text.slice(0, 200)}`,
          [404, 405].includes(policyResponse.status)
            ? 502
            : policyResponse.status,
        );
      }
      if (
        policyResponse.ok &&
        (
          body?.account_id !== this.accountId ||
          body?.evacuation_id !== operation.evacuation_id ||
          !isObject(body?.placement_policy)
        )
      ) {
        fail(
          "placement policy returned 2xx without an exact evacuation acknowledgement",
          502,
        );
      }
    }

    const response = await this.fetchImpl(
      `${cell.endpoint}/v1/accounts/${this.accountId}:complete-evacuation`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${cell.provision_token}`,
        },
        body: JSON.stringify({
          for: "evacuation",
          evacuation_id: operation.evacuation_id,
        }),
        signal: AbortSignal.timeout(15_000),
      },
    );
    const { text, body } = await responseBody(response);
    if (!response.ok) {
      fail(
        `resume ${response.status}: ${text.slice(0, 200)}`,
        [404, 405].includes(response.status) ? 502 : response.status,
      );
    }
    requireResumeAck(
      body,
      this.accountId,
      operation.evacuation_id,
    );
    if (
      operation.imported_status === "closed" &&
      body.status !== "closed"
    ) {
      fail("resume did not preserve the imported closed tombstone", 502);
    }
    if (
      operation.imported_status !== "closed" &&
      body.status === "closed"
    ) {
      fail("resume unexpectedly converted a live import into a tombstone", 502);
    }
    state = operationMetadata(state, {
      restored_status: body.status,
    });
    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase: "target_imported",
      to_phase: "target_resumed",
    });
    return this.saveState(state);
  }

  async commitTargetRoute(state) {
    const operation = state.operation;
    const cell = await this.cell(operation.target_cell, {
      expectedRegistrationID: operation.target_registration_id,
    });
    state = commitLive(state, {
      operation_id: operation.operation_id,
      route: routeForCell(cell, operation.epoch),
    });
    return this.saveState(state);
  }

  async finalizeSource(state) {
    const operation = state.operation;
    const fromPhase = operation.phase;
    const toPhase = fromPhase === "closed_archive_projected"
      ? "closed_archive_retained"
      : "source_finalized";

    let receipt;
    if (operation.source_cell === operation.target_cell) {
      // Same-cell restore promotes the one source row to target only after the
      // archive is verified. Purging it would delete the newly restored copy.
      receipt = {
        mode: "same_cell_target",
        finalized: true,
      };
    } else if (
      operation.kind === "restore" &&
      operation.legacy_evacuation_id === true
    ) {
      // The bounded pre-v1 compatibility path has no source generation or
      // evacuation marker to fence. Archive validation is its authority; the
      // old source is intentionally treated as offline without consulting KV.
      receipt = {
        mode: "legacy_unfenced_source",
        finalized: true,
      };
    } else {
      const expectedRegistrationID =
        operation.source_registration_id ?? null;
      if (!expectedRegistrationID) {
        fail(
          "source finalization has no durable registration fence",
          409,
        );
      }
      const registration = await this.targetCoordinatorRequest(
        operation.source_cell,
        "/registration-status",
        { registration_id: expectedRegistrationID },
      );
      if (
        registration.cell_name !== operation.source_cell ||
        registration.expected_registration_id !==
          expectedRegistrationID ||
        !["active", "replaced", "deleted", "unknown"].includes(
          registration.registration_status,
        )
      ) {
        fail("source registration authority returned an invalid receipt", 502);
      }

      if (registration.registration_status === "deleted") {
        receipt = {
          mode: "source_cell_unregistered",
          finalized: true,
          source_registration_id: expectedRegistrationID,
        };
      } else if (registration.registration_status === "replaced") {
        receipt = {
          mode: "source_cell_replaced",
          finalized: true,
          source_registration_id: expectedRegistrationID,
          current_registration_id:
            registration.current_registration_id,
        };
      } else if (registration.registration_status === "active") {
        let cell;
        if (isObject(registration.active_cell)) {
          const activeCell = registration.active_cell;
          if (
            typeof activeCell.endpoint !== "string" ||
            !activeCell.endpoint.startsWith("https://") ||
            typeof activeCell.provision_token !== "string" ||
            activeCell.provision_token.length === 0 ||
            (activeCell.registration_id ?? activeCell.registered_at) !==
              expectedRegistrationID
          ) {
            fail(
              "source registration authority returned an invalid active cell",
              502,
            );
          }
          cell = {
            ...activeCell,
            name: operation.source_cell,
          };
          await this.requireEvacuationProtocol(cell);
        } else {
          // Bounded rolling-version compatibility for a coordinator that
          // already supports registration-status but does not yet return its
          // exact active cell. KV is safe only when it still carries the same
          // registration generation.
          cell = await this.cell(operation.source_cell, {
            evacuationProtocol: true,
            expectedRegistrationID,
          });
        }
        let response;
        try {
          response = await this.fetchImpl(
            `${cell.endpoint}/v1/accounts/${this.accountId}:finalize-evacuation`,
            {
              method: "POST",
              headers: {
                "Content-Type": "application/json",
                Authorization: `Bearer ${cell.provision_token}`,
              },
              body: JSON.stringify({
                evacuation_id: operation.evacuation_id,
              }),
              signal: AbortSignal.timeout(15_000),
            },
          );
        } catch (error) {
          fail(
            `source finalization outcome is ambiguous: ${String(error?.message ?? error)}`,
            502,
            { cause: error },
          );
        }
        const parsed = await responseBody(response);
        if (!response.ok) {
          fail(
            `source finalization ${response.status}: ${parsed.text.slice(0, 200)}`,
            response.status,
          );
        }
        const body = requireFinalizeAck(
          parsed.body,
          this.accountId,
          operation.evacuation_id,
        );
        receipt = {
          mode: "cell_receipt",
          finalized: true,
          source_status: body.source_status,
          already_finalized: body.already_finalized === true,
          finalized_at: body.finalized_at ?? null,
        };
      } else {
        fail(
          "source cell durable registration state is unknown",
          502,
        );
      }
    }

    if (
      operation.source_cell !== operation.target_cell &&
      ["move", "restore"].includes(operation.kind)
    ) {
      await this.departSourceResident(operation);
    }

    state = operationMetadata(state, {
      source_finalization: receipt,
    });
    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase: fromPhase,
      to_phase: toPhase,
    });
    return this.saveState(state);
  }

  async applyProjection(state, step) {
    const operation = state.operation;
    const projection = step.projection;
    if (step.target === "archive") {
      const key = `archived:${this.accountId}`;
      if (projection.action === "put") {
        const current = await this.env.DIRECTORY.get(key, { type: "json" });
        if (
          current &&
          !exactArchive(archiveIdentity(current), projection)
        ) {
          fail("a different archived projection already exists", 409);
        }
        await this.env.DIRECTORY.put(
          key,
          JSON.stringify(projection.value),
        );
      } else {
        const current = await this.env.DIRECTORY.get(key, { type: "json" });
        if (current) {
          if (!exactArchive(archiveIdentity(current), projection)) {
            fail(
              "archived projection changed before exact retirement",
              409,
            );
          }
          await this.env.DIRECTORY.delete(key);
        }
      }
      state = markProjectionApplied(state, {
        operation_id: operation.operation_id,
        target: "archive",
        action: projection.action,
        archive_id: projection.archive_id,
        archive_object: projection.object,
      });
      return this.saveState(state);
    }

    const key = `acct:${this.accountId}`;
    if (projection.action === "put") {
      const current = await this.env.DIRECTORY.get(key, { type: "json" });
      if (
        current &&
        (
          current.cell !== projection.value.cell ||
          (
            Number.isSafeInteger(current.epoch) &&
            current.epoch > projection.epoch
          )
        )
      ) {
        fail("a newer or different route projection already exists", 409);
      }
      await this.env.DIRECTORY.put(
        key,
        JSON.stringify(projection.value),
      );
      // The target route must be authoritative before alias projections can
      // resolve and verify the new cell. Staying in live_committed until this
      // exact fence completes makes a crash replay only this republish.
      await this.reconcileAgentEmailPolicies(this.accountId, {
        operation_id: operation.operation_id,
        epoch: operation.epoch,
        action: "republish",
      });
    } else {
      const current = await this.env.DIRECTORY.get(key, { type: "json" });
      if (current) {
        if (
          current.cell !== operation.source_cell ||
          (
            Number.isSafeInteger(current.epoch) &&
            current.epoch > operation.epoch
          )
        ) {
          fail("route projection changed before exact retirement", 409);
        }
        // Remove destination authority from every canonical/custom realm route
        // before retiring the account route. The registry owns its own durable
        // cursor and exact lifecycle fence, so a partial page or crash cannot
        // let this state machine advance early.
        await this.reconcileAgentEmailPolicies(this.accountId, {
          operation_id: operation.operation_id,
          epoch: operation.epoch,
          action: operation.kind === "close" ? "retire" : "suspend",
        });
        await this.env.DIRECTORY.delete(key);
      } else {
        // A lost/stale KV acknowledgement is not proof that aliases were
        // suspended. Reconcile the exact durable lifecycle fence even when the
        // route delete itself is already externally visible.
        await this.reconcileAgentEmailPolicies(this.accountId, {
          operation_id: operation.operation_id,
          epoch: operation.epoch,
          action: operation.kind === "close" ? "retire" : "suspend",
        });
      }
      if (operation.outcome === "reaped") {
        await this.env.DIRECTORY.delete(`pending:${this.accountId}`);
      }
      if (!["move", "restore"].includes(operation.kind)) {
        await this.departSourceResident(operation);
      }
    }
    state = markProjectionApplied(state, {
      operation_id: operation.operation_id,
      target: "route",
      action: projection.action,
    });
    return this.saveState(state);
  }

  async cleanupArchive(state) {
    const operation = state.operation;
    const cleanup = state.projections.cleanup;
    await this.env.ARCHIVES.delete(cleanup.object);
    state = markArchiveCleanupApplied(state, {
      operation_id: operation.operation_id,
      archive_id: cleanup.archive_id,
      archive_object: cleanup.object,
    });
    return this.saveState(state);
  }

  async complete(state) {
    const operation = state.operation;
    const operationID = operation.operation_id;
    if (
      operation.target_cell &&
      state.location.kind === "live" &&
      state.location.cell === operation.target_cell
    ) {
      await this.promoteTargetResident(operation);
    } else {
      await this.releaseTargetReservation(operation);
    }
    state = completeOperation(state, {
      operation_id: operationID,
    });
    await this.saveState(state);
    if (typeof this.storage.deleteAlarm === "function") {
      await this.storage.deleteAlarm().catch(() => {});
    }
    return state;
  }

  async scheduleWakeup() {
    if (typeof this.storage.setAlarm === "function") {
      await this.storage.setAlarm(
        this.now().getTime() + LIFECYCLE_RETRY_MS,
      );
    }
  }

  async alarm() {
    try {
      return await this.fence.run(async () => {
        // Durable Object alarms are one-shot. Rearm before doing any work so a
        // crash in any nonterminal phase remains scheduler-visible.
        await this.scheduleWakeup();
        let state = await this.loadState();
        if (!state?.operation) {
          if (state) {
            await this.releaseCompletedTargetReservation(state);
          }
          if (typeof this.storage.deleteAlarm === "function") {
            await this.storage.deleteAlarm().catch(() => {});
          }
          return;
        }
        if (state.operation.kind === "close") {
          if (state.operation.phase === "claimed") {
            const cell = await this.cell(
              state.operation.source_cell,
              {
                expectedRegistrationID:
                  state.operation.source_registration_id,
              },
            );
            if (await this.contactStatus(cell) !== "closed") {
              // The owner credential/body are intentionally not persisted.
              // A claimed close can only resume autonomously after contact
              // proves that the earlier close committed.
              return;
            }
            state = acknowledgeStep(state, {
              operation_id: state.operation.operation_id,
              from_phase: "claimed",
              to_phase: "source_closed",
            });
            state = await this.saveState(state);
          }
          await this.runClose(state, {});
          return;
        }
        await this.runMove(state);
      });
    } catch (error) {
      console.log(
        `account lifecycle retry for ${this.accountId} failed: ${String(error?.message ?? error)}`,
      );
      if (
        error instanceof RestoreArchiveQuarantinedError &&
        error.terminal === true
      ) {
        if (typeof this.storage.deleteAlarm === "function") {
          await this.storage.deleteAlarm().catch(() => {});
        }
        return;
      }
      await this.scheduleWakeup();
    }
  }

  async readPlacementPolicy(cell, accountID) {
    try {
      const response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${accountID}/placement-policy`,
        {
          method: "GET",
          headers: {
            Authorization: `Bearer ${cell.provision_token}`,
          },
          signal: AbortSignal.timeout(15_000),
        },
      );
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        fail(
          `placement policy ${response.status}: ${text.slice(0, 200)}`,
          response.status,
        );
      }
      const body = await response.json();
      if (
        body?.account_id !== accountID ||
        !isObject(body?.placement_policy)
      ) {
        fail(
          "placement policy read returned no exact account policy",
          502,
        );
      }
      return body.placement_policy;
    } catch (error) {
      if (error instanceof AccountLifecycleRuntimeError) {
        throw error;
      }
      fail(
        `placement policy read failed: ${String(error?.message ?? error)}`,
        502,
        { cause: error },
      );
    }
  }

  async runClose(initialState, input) {
    let state = initialState;
    while (state.operation) {
      const step = nextLifecycleStep(state);
      if (step.action === "close_source") {
        const result = await this.closeSource(state, input);
        if (result.response) {
          return result.response;
        }
        state = result.state;
        continue;
      }
      if (step.action === "commit_closed") {
        state = commitClosed(state, {
          operation_id: state.operation.operation_id,
        });
        state = await this.saveState(state);
        continue;
      }
      if (step.type === "projection") {
        state = await this.applyProjection(state, step);
        await this.env.DIRECTORY.delete(
          `pending:${this.accountId}`,
        );
        continue;
      }
      if (step.action === "complete") {
        state = await this.complete(state);
        continue;
      }
      fail(`unsupported close step ${step.action}`, 500);
    }
    return canonicalClosed(this.accountId);
  }

  async closeSource(state, input) {
    const operation = state.operation;
    const cell = await this.cell(operation.source_cell, {
      expectedRegistrationID: operation.source_registration_id,
    });
    let response = null;
    let responseText = "";
    let responseBodyValue = null;
    let transportError = null;
    try {
      response = await this.fetchImpl(
        `${cell.endpoint}/v1/account:close`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: input.authorization,
          },
          body: input.body || "{}",
          signal: AbortSignal.timeout(15_000),
        },
      );
      const parsed = await responseBody(response);
      responseText = parsed.text;
      responseBodyValue = parsed.body;
    } catch (error) {
      transportError = error;
    }

    const exactSuccess = response?.ok &&
      responseBodyValue?.account_id === this.accountId &&
      responseBodyValue?.status === "closed";
    if (!exactSuccess) {
      const contact = await this.contactStatus(cell);
      if (contact === "closed") {
        // The owner close committed but its acknowledgement was lost.
      } else if (
        response &&
        response.status >= 400 &&
        response.status < 500 &&
        contact !== null
      ) {
        state = cancelOperation(state, {
          operation_id: operation.operation_id,
        });
        await this.saveState(state);
        if (typeof this.storage.deleteAlarm === "function") {
          await this.storage.deleteAlarm().catch(() => {});
        }
        return {
          state,
          response: new Response(responseText, {
            status: response.status,
            headers: {
              "Content-Type":
                response.headers.get("Content-Type") ??
                "application/json",
            },
          }),
        };
      } else {
        const detail = transportError
          ? String(transportError?.message ?? transportError)
          : `HTTP ${response?.status ?? "unknown"}`;
        fail(`close outcome is ambiguous (${detail}); retry shortly`, 502);
      }
    }

    state = acknowledgeStep(state, {
      operation_id: operation.operation_id,
      from_phase: "claimed",
      to_phase: "source_closed",
    });
    return { state: await this.saveState(state) };
  }

  async contactStatus(cell) {
    try {
      const response = await this.fetchImpl(
        `${cell.endpoint}/v1/accounts/${this.accountId}:contact`,
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${cell.provision_token}`,
          },
          signal: AbortSignal.timeout(15_000),
        },
      );
      if (!response.ok) {
        await response.text().catch(() => "");
        return null;
      }
      const body = await response.json();
      if (
        body?.account_id !== this.accountId ||
        typeof body?.status !== "string"
      ) {
        return null;
      }
      return body.status;
    } catch {
      return null;
    }
  }
}

// R2 requires multipart uploads for unknown-length streaming export bodies.
// A normal read/upload failure aborts the partial upload. If complete() commits
// and its acknowledgement is lost, the caller performs attempt-key deletion
// only after the source returns an exact pre-archive abort receipt. The bucket
// lifecycle rule remains responsible for uploads interrupted before this
// helper can call abort() (production: all prefixes, seven days).
export async function streamToR2Multipart(bucket, key, stream, options) {
  const upload = await bucket.createMultipartUpload(key, options);
  const parts = [];
  const partSize = 8 * 1024 * 1024;
  let totalBytes = 0;
  try {
    const reader = stream.getReader();
    let buffer = new Uint8Array(0);
    let partNumber = 1;
    while (true) {
      const { done, value } = await reader.read();
      if (value && value.length > 0) {
        const combined = new Uint8Array(buffer.length + value.length);
        combined.set(buffer);
        combined.set(value, buffer.length);
        buffer = combined;
        totalBytes += value.length;
      }
      while (buffer.length >= partSize) {
        const uploaded = await upload.uploadPart(
          partNumber++,
          buffer.slice(0, partSize),
        );
        parts.push(uploaded);
        buffer = buffer.slice(partSize);
      }
      if (done) {
        if (buffer.length > 0) {
          parts.push(await upload.uploadPart(partNumber, buffer));
        }
        break;
      }
    }
    if (parts.length === 0) {
      throw new Error("export stream was empty");
    }
    await upload.complete(parts);
    return totalBytes;
  } catch (error) {
    try {
      await upload.abort();
    } catch {
      // Preserve the primary transport/R2 failure.
    }
    throw error;
  }
}
