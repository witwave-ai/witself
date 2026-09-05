import {
  accountBackupSchedulingEnabled,
  cellHasDestinationCredentials,
} from "./placement.mjs";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const OPERATION_ID = /^[A-Za-z0-9_-]{1,128}$/;
const RESERVATION_KINDS = new Set(["move", "restore"]);
const RESERVATION_PREFIX = "reservation:";
const RESIDENT_PREFIX = "resident:";
const PROVISION_PREFIX = "provision:";
const CELL_STATE_KEY = "cell-registration";
const DELETE_FENCE_KEY = "delete-fence";
const LAST_DELETE_KEY = "last-delete";
const RESERVATION_LEASE_MS = 5 * 60 * 1000;
const DELETE_LEASE_MS = 60 * 1000;
const AMBIGUOUS_LIVENESS_RETRY_MS = 60 * 1000;

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(message, status) {
  return json({ schema_version: "witself.v0", error: message }, status);
}

class TargetCellCoordinatorError extends Error {
  constructor(message, status = 500) {
    super(message);
    this.name = "TargetCellCoordinatorError";
    this.status = status;
  }
}

function fail(message, status = 500) {
  throw new TargetCellCoordinatorError(message, status);
}

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

function publicCell(cell) {
  const {
    provision_token: _provisionToken,
    backup_token: _backupToken,
    ...rest
  } = cell;
  return {
    ...rest,
    backup_validation_target:
      cell.backup_validation_target === true,
    has_provision_token: Boolean(cell.provision_token),
    has_backup_token: Boolean(cell.backup_token),
  };
}

function exactReservation(left, right) {
  return left?.account_id === right?.account_id &&
    left?.operation_id === right?.operation_id &&
    left?.evacuation_id === right?.evacuation_id &&
    left?.kind === right?.kind &&
    left?.target_cell === right?.target_cell &&
    left?.target_registration_id === right?.target_registration_id &&
    left?.epoch === right?.epoch;
}

function validReservation(value, cellName) {
  return isObject(value) &&
    ACCOUNT_ID.test(value.account_id ?? "") &&
    OPERATION_ID.test(value.operation_id ?? "") &&
    OPERATION_ID.test(value.evacuation_id ?? "") &&
    RESERVATION_KINDS.has(value.kind) &&
    value.target_cell === cellName &&
    typeof value.target_registration_id === "string" &&
    value.target_registration_id.length >= 1 &&
    value.target_registration_id.length <= 128 &&
    Number.isSafeInteger(value.epoch) &&
    value.epoch >= 0;
}

function reservationKey(accountID) {
  return `${RESERVATION_PREFIX}${accountID}`;
}

function residentKey(accountID) {
  return `${RESIDENT_PREFIX}${accountID}`;
}

function provisionKey(provisionID) {
  return `${PROVISION_PREFIX}${provisionID}`;
}

/**
 * Per-cell lifecycle serialization authority.
 *
 * Registration, incoming-account reservation and safe deletion all enter this
 * one Durable Object. DIRECTORY KV remains only their read projection; no
 * correctness decision relies on cross-key KV visibility or compare-and-swap.
 */
export class DurableTargetCellCoordinator {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.cellName = ctx.id?.name ?? null;
    this.now = dependencies.now ?? (() => new Date());
    this.randomUUID =
      dependencies.randomUUID ?? (() => globalThis.crypto.randomUUID());
    // Durable Objects can interleave fetch handlers at await points. This
    // explicit queue keeps every mutation in one critical section.
    this.queue = Promise.resolve();
  }

  fetch(request) {
    return this.serial(() => this.handleFetch(request));
  }

  alarm() {
    return this.serial(() => this.handleAlarm());
  }

  serial(work) {
    const result = this.queue.then(work, work);
    this.queue = result.catch(() => {});
    return result;
  }

  async handleFetch(request) {
    const url = new URL(request.url);
    if (request.method !== "POST") {
      return errorResponse("target cell coordinator endpoint not found", 404);
    }
    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid target cell coordinator request", 400);
    }
    if (
      !CELL_NAME.test(this.cellName ?? "") ||
      !isObject(input) ||
      input.cell_name !== this.cellName
    ) {
      return errorResponse("invalid target cell coordinator request", 400);
    }

    try {
      switch (url.pathname) {
        case "/register":
          return await this.register(input);
        case "/set-accepting":
          return await this.setAccepting(input);
        case "/reserve":
          return await this.reserve(input);
        case "/release":
          return await this.release(input);
        case "/promote":
          return await this.promote(input);
        case "/depart":
          return await this.depart(input);
        case "/provision/begin":
          return await this.beginProvision(input);
        case "/provision/attach":
          return await this.attachProvision(input);
        case "/provision/promote":
          return await this.promoteProvision(input);
        case "/provision/abort":
          return await this.abortProvision(input);
        case "/delete":
          return await this.safeDelete(input);
        case "/purge":
          return await this.purge(input);
        case "/status":
          return await this.status();
        case "/registration-status":
          return await this.registrationStatus(input);
        default:
          return errorResponse(
            "target cell coordinator endpoint not found",
            404,
          );
      }
    } catch (error) {
      const status = error instanceof TargetCellCoordinatorError
        ? error.status
        : 500;
      return errorResponse(String(error?.message ?? error), status);
    }
  }

  async register(input) {
    if (
      !isObject(input.cell) ||
      typeof input.cell.endpoint !== "string" ||
      !input.cell.endpoint.startsWith("https://")
    ) {
      fail("invalid cell registration", 400);
    }
    const markerProvided = Object.prototype.hasOwnProperty.call(
      input.cell,
      "backup_validation_target",
    );
    if (
      markerProvided &&
      typeof input.cell.backup_validation_target !== "boolean"
    ) {
      fail("backup_validation_target must be a boolean", 400);
    }
    await this.recoverExpiredDeleteFence();
    const fence = await this.storage.get(DELETE_FENCE_KEY);
    if (fence) {
      fail("cell deletion is in progress", 409);
    }

    const key = `cell:${this.cellName}`;
    const storedCell = await this.storage.get(CELL_STATE_KEY);
    const projectedCell = await this.env.DIRECTORY.get(key, {
      type: "json",
    });
    const lastDelete = await this.storage.get(LAST_DELETE_KEY);
    // After a finalized delete, an explicit registration is the only path
    // that may create new authority. Ignore any lagging KV projection so the
    // replacement receives a fresh registration generation.
    const existing = storedCell ?? (lastDelete ? null : projectedCell);
    const requestedRegistrationID =
      typeof input.cell.registration_id === "string" &&
          input.cell.registration_id.length >= 1 &&
          input.cell.registration_id.length <= 128
        ? input.cell.registration_id
        : null;
    const existingRegistrationID =
      existing?.registration_id ?? existing?.registered_at ?? null;
    const existingBackupValidationTarget =
      existing?.backup_validation_target === true;
    // An older registration client does not know about this field. Omission
    // therefore means "preserve" for an existing cell, not "clear". This is
    // the fail-closed boundary that prevents an old heartbeat from reopening
    // a dedicated validation cell.
    const backupValidationTarget = markerProvided
      ? input.cell.backup_validation_target
      : existingBackupValidationTarget;
    const markerChanged =
      backupValidationTarget !== existingBackupValidationTarget;
    const incomingProvisionToken =
      typeof input.cell.provision_token === "string" &&
          input.cell.provision_token.length > 0
        ? input.cell.provision_token
        : null;
    const incomingBackupToken =
      typeof input.cell.backup_token === "string" &&
          input.cell.backup_token.length > 0
        ? input.cell.backup_token
        : null;
    const sameInstance =
      Boolean(existing) &&
      (
        requestedRegistrationID
          ? requestedRegistrationID === existingRegistrationID
          : input.cell.endpoint === existing.endpoint &&
            (
              !incomingProvisionToken ||
              incomingProvisionToken === existing.provision_token
            )
      );
    if ((existing && !sameInstance) || markerChanged) {
      await this.reapExpiredReservations();
      await this.reconcileProvisions();
      await this.reconcileResidents();
      const [
        reservations,
        provisions,
        residents,
        hasProjectedAccounts,
        hasPendingAccounts,
      ] =
        await Promise.all([
          this.storage.list({ prefix: RESERVATION_PREFIX }),
          this.storage.list({ prefix: PROVISION_PREFIX }),
          this.storage.list({ prefix: RESIDENT_PREFIX }),
          this.cellHasAccounts(),
          markerChanged
            ? this.cellHasPendingAccounts()
            : Promise.resolve(false),
        ]);
      if (
        reservations.size > 0 ||
        provisions.size > 0 ||
        residents.size > 0 ||
        hasProjectedAccounts ||
        hasPendingAccounts
      ) {
        if (markerChanged) {
          fail(
            "backup_validation_target cannot change while account occupancy, reservations, provisions, or routes remain",
            409,
          );
        }
        fail(
          "cell registration cannot be replaced while account occupancy remains",
          409,
        );
      }
    }
    const registrationID = sameInstance
      ? existingRegistrationID
      : requestedRegistrationID ?? this.randomUUID();
    const registeredAt = sameInstance
      ? existing.registered_at
      : this.now().toISOString();
    const cell = {
      endpoint: input.cell.endpoint,
      cloud: input.cell.cloud || "",
      region: input.cell.region || "",
      region_code:
        input.cell.region_code ?? existing?.region_code ?? "",
      channel:
        input.cell.channel ?? existing?.channel ?? "experimental",
      owner: input.cell.owner || "witwave",
      weight: Number.isFinite(input.cell.weight)
        ? input.cell.weight
        : 1,
      // Validation cells are permanently outside ordinary placement while
      // marked. Force the projection closed even when an older client sends
      // its historical accepting=true default.
      accepting: backupValidationTarget
        ? false
        : input.cell.accepting !== false,
      backup_validation_target: backupValidationTarget,
      provision_token:
        incomingProvisionToken ??
        existing?.provision_token ??
        null,
      backup_token:
        incomingBackupToken ??
        existing?.backup_token ??
        null,
      registration_id: registrationID,
      registered_at: registeredAt,
    };
    if (
      cell.provision_token &&
      cell.backup_token &&
      cell.provision_token === cell.backup_token
    ) {
      fail(
        "backup_token must be distinct from provision_token",
        400,
      );
    }
    if (
      cell.accepting !== false &&
      !cellHasDestinationCredentials(cell, {
        backupsEnabled: accountBackupSchedulingEnabled(this.env),
      })
    ) {
      fail(
        accountBackupSchedulingEnabled(this.env)
          ? "accepting cells require distinct nonempty provision_token and backup_token while account backups are enabled"
          : "accepting cells require a nonempty provision_token",
        400,
      );
    }
    // The DO copy is authoritative. KV remains the fleet read projection.
    await this.storage.put(CELL_STATE_KEY, cell);
    await this.env.DIRECTORY.put(key, JSON.stringify(cell));
    await this.backfillResidents(cell);
    return json({
      schema_version: "witself.v0",
      cell: publicCell({ name: this.cellName, ...cell }),
      created: !existing,
    }, existing ? 200 : 201);
  }

  async setAccepting(input) {
    if (
      typeof input.accepting !== "boolean" ||
      Object.keys(input).some((key) =>
        key !== "cell_name" && key !== "accepting"
      )
    ) {
      fail("body must contain only accepting as a boolean", 400);
    }
    await this.recoverExpiredDeleteFence();
    if (await this.storage.get(DELETE_FENCE_KEY)) {
      fail("cell deletion is in progress", 409);
    }
    const existing = await this.authoritativeCell();
    if (!existing) {
      fail("unknown cell", 404);
    }
    if (input.accepting && existing.backup_validation_target === true) {
      fail("cell is reserved for backup validation", 409);
    }
    const backupsEnabled = accountBackupSchedulingEnabled(this.env);
    if (
      input.accepting &&
      !cellHasDestinationCredentials(existing, { backupsEnabled })
    ) {
      fail(
        backupsEnabled
          ? "accepting cells require distinct nonempty provision_token and backup_token while account backups are enabled"
          : "accepting cells require a nonempty provision_token",
        400,
      );
    }
    // Keep the mutation inside the coordinator's serial queue and derive it
    // solely from its authority. Replaying a KV snapshot through /register
    // could clear newer isolation markers, credentials, or placement metadata.
    const cell = { ...existing, accepting: input.accepting };
    await this.storage.put(CELL_STATE_KEY, cell);
    await this.env.DIRECTORY.put(
      `cell:${this.cellName}`,
      JSON.stringify(cell),
    );
    return json({
      schema_version: "witself.v0",
      cell: publicCell({ name: this.cellName, ...cell }),
    });
  }

  async reserve(input) {
    if (!validReservation(input.reservation, this.cellName)) {
      fail("invalid target reservation", 400);
    }
    await this.recoverExpiredDeleteFence();
    if (await this.storage.get(DELETE_FENCE_KEY)) {
      fail("cell deletion is in progress", 409);
    }

    const reservation = input.reservation;
    const key = reservationKey(reservation.account_id);
    const current = await this.storage.get(key);
    const resident = await this.storage.get(
      residentKey(reservation.account_id),
    );
    const cell = await this.authoritativeCell();
    if (!cell) {
      fail("target cell is not registered", 409);
    }
    if (cell?.backup_validation_target === true) {
      fail("target cell is reserved for backup validation", 409);
    }
    if (
      !cellHasDestinationCredentials(cell, {
        backupsEnabled: accountBackupSchedulingEnabled(this.env),
      })
    ) {
      fail(
        "target cell lacks credentials required for account placement",
        409,
      );
    }
    if (
      !current &&
      resident?.operation_id === reservation.operation_id &&
      resident?.evacuation_id === reservation.evacuation_id &&
      resident?.registration_id ===
        reservation.target_registration_id &&
      resident?.route_epoch === reservation.epoch
    ) {
      return json({
        ok: true,
        account_id: reservation.account_id,
        operation_id: reservation.operation_id,
        evacuation_id: reservation.evacuation_id,
        target_cell: reservation.target_cell,
        target_registration_id: reservation.target_registration_id,
        resident: true,
        expires_at: null,
      });
    }
    if (current) {
      if (!exactReservation(current, reservation)) {
        fail(
          `a different lifecycle operation already reserves ${reservation.account_id}`,
          409,
        );
      }
      if (
        !cell ||
        (cell.registration_id ?? cell.registered_at) !==
          reservation.target_registration_id
      ) {
        fail("target cell registration changed", 409);
      }
    } else {
      if (!cell) {
        fail("target cell is not registered", 409);
      }
      if (cell.accepting === false) {
        fail("target cell is drained", 409);
      }
      if (
        (cell.registration_id ?? cell.registered_at) !==
          reservation.target_registration_id
      ) {
        fail("target cell registration changed", 409);
      }
    }

    const now = this.now();
    const stored = {
      ...reservation,
      reserved_at: current?.reserved_at ?? now.toISOString(),
      renewed_at: now.toISOString(),
      expires_at: new Date(now.getTime() + RESERVATION_LEASE_MS)
        .toISOString(),
    };
    await this.storage.put(key, stored);
    await this.scheduleAlarm();
    return json({
      ok: true,
      account_id: stored.account_id,
      operation_id: stored.operation_id,
      evacuation_id: stored.evacuation_id,
      target_cell: stored.target_cell,
      target_registration_id: stored.target_registration_id,
      expires_at: stored.expires_at,
    });
  }

  async release(input) {
    if (!validReservation(input.reservation, this.cellName)) {
      fail("invalid target reservation release", 400);
    }
    const expected = input.reservation;
    const key = reservationKey(expected.account_id);
    const current = await this.storage.get(key);
    if (!current) {
      return json({
        ok: true,
        account_id: expected.account_id,
        operation_id: expected.operation_id,
        released: false,
      });
    }
    if (!exactReservation(current, expected)) {
      fail("target reservation changed before exact release", 409);
    }
    await this.storage.delete(key);
    await this.scheduleAlarm();
    return json({
      ok: true,
      account_id: expected.account_id,
      operation_id: expected.operation_id,
      released: true,
    });
  }

  async promote(input) {
    if (!validReservation(input.reservation, this.cellName)) {
      fail("invalid target reservation promotion", 400);
    }
    const expected = input.reservation;
    const reservation = await this.storage.get(
      reservationKey(expected.account_id),
    );
    const existing = await this.storage.get(
      residentKey(expected.account_id),
    );
    if (existing) {
      if (
        existing.account_id !== expected.account_id ||
        existing.cell_name !== this.cellName ||
        existing.registration_id !== expected.target_registration_id ||
        existing.route_epoch !== expected.epoch
      ) {
        fail("resident account authority conflicts with promotion", 409);
      }
      if (reservation && !exactReservation(reservation, expected)) {
        fail("target reservation changed before promotion", 409);
      }
    } else {
      if (!reservation || !exactReservation(reservation, expected)) {
        fail("exact target reservation is missing before promotion", 409);
      }
      await this.storage.put(residentKey(expected.account_id), {
        account_id: expected.account_id,
        cell_name: this.cellName,
        registration_id: expected.target_registration_id,
        admitted_by: "lifecycle",
        operation_id: expected.operation_id,
        evacuation_id: expected.evacuation_id,
        route_epoch: expected.epoch,
        admitted_at: this.now().toISOString(),
      });
    }
    if (reservation) {
      await this.storage.delete(reservationKey(expected.account_id));
    }
    await this.scheduleAlarm();
    return json({
      ok: true,
      account_id: expected.account_id,
      operation_id: expected.operation_id,
      resident: true,
    });
  }

  async depart(input) {
    if (
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      !OPERATION_ID.test(input.operation_id ?? "") ||
      typeof input.registration_id !== "string" ||
      input.registration_id.length < 1 ||
      input.registration_id.length > 128 ||
      !Number.isSafeInteger(input.source_epoch) ||
      input.source_epoch < 0
    ) {
      fail("invalid resident departure", 400);
    }
    const key = residentKey(input.account_id);
    const resident = await this.storage.get(key);
    if (!resident) {
      return json({
        ok: true,
        account_id: input.account_id,
        operation_id: input.operation_id,
        departed: false,
      });
    }
    if (
      resident.account_id !== input.account_id ||
      resident.cell_name !== this.cellName ||
      resident.registration_id !== input.registration_id ||
      resident.route_epoch !== input.source_epoch
    ) {
      fail("resident account authority changed before departure", 409);
    }
    await this.storage.delete(key);
    return json({
      ok: true,
      account_id: input.account_id,
      operation_id: input.operation_id,
      source_epoch: input.source_epoch,
      departed: true,
    });
  }

  async beginProvision(input) {
    if (
      !OPERATION_ID.test(input.provision_id ?? "") ||
      typeof input.registration_id !== "string" ||
      input.registration_id.length < 1 ||
      input.registration_id.length > 128
    ) {
      fail("invalid provisioning reservation", 400);
    }
    await this.recoverExpiredDeleteFence();
    if (await this.storage.get(DELETE_FENCE_KEY)) {
      fail("cell deletion is in progress", 409);
    }
    const cell = await this.authoritativeCell();
    if (
      !cell ||
      cell.accepting === false ||
      cell.backup_validation_target === true ||
      !cellHasDestinationCredentials(cell, {
        backupsEnabled: accountBackupSchedulingEnabled(this.env),
      }) ||
      (cell.registration_id ?? cell.registered_at) !==
        input.registration_id
    ) {
      fail("target cell cannot accept provisioning", 409);
    }
    const key = provisionKey(input.provision_id);
    const current = await this.storage.get(key);
    if (
      current &&
      (
        current.provision_id !== input.provision_id ||
        current.registration_id !== input.registration_id
      )
    ) {
      fail("provisioning reservation changed", 409);
    }
    const now = this.now();
    const reservation = {
      provision_id: input.provision_id,
      cell_name: this.cellName,
      registration_id: input.registration_id,
      account_id: current?.account_id ?? null,
      reserved_at: current?.reserved_at ?? now.toISOString(),
      renewed_at: now.toISOString(),
      // Unattached expirations remain fail-closed. The alarm only uses this
      // as a wakeup to reconcile an attached account with its route.
      expires_at: new Date(now.getTime() + RESERVATION_LEASE_MS)
        .toISOString(),
    };
    await this.storage.put(key, reservation);
    await this.scheduleAlarm();
    return json({
      ok: true,
      provision_id: input.provision_id,
      registration_id: input.registration_id,
      expires_at: reservation.expires_at,
    });
  }

  async attachProvision(input) {
    if (
      !OPERATION_ID.test(input.provision_id ?? "") ||
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      typeof input.registration_id !== "string" ||
      !Number.isSafeInteger(input.route_epoch) ||
      input.route_epoch < 0
    ) {
      fail("invalid provisioning attachment", 400);
    }
    const key = provisionKey(input.provision_id);
    const current = await this.storage.get(key);
    if (
      !current ||
      current.registration_id !== input.registration_id ||
      (
        current.route_epoch !== undefined &&
        current.route_epoch !== input.route_epoch
      ) ||
      (
        current.account_id !== null &&
        current.account_id !== input.account_id
      )
    ) {
      fail("exact provisioning reservation is missing", 409);
    }
    const attached = {
      ...current,
      account_id: input.account_id,
      route_epoch: input.route_epoch,
      renewed_at: this.now().toISOString(),
      expires_at: new Date(
        this.now().getTime() + RESERVATION_LEASE_MS,
      ).toISOString(),
    };
    await this.storage.put(key, attached);
    await this.scheduleAlarm();
    return json({
      ok: true,
      provision_id: input.provision_id,
      account_id: input.account_id,
      attached: true,
    });
  }

  async promoteProvision(input) {
    if (
      !OPERATION_ID.test(input.provision_id ?? "") ||
      !ACCOUNT_ID.test(input.account_id ?? "") ||
      typeof input.registration_id !== "string" ||
      !Number.isSafeInteger(input.route_epoch) ||
      input.route_epoch < 0
    ) {
      fail("invalid provisioning promotion", 400);
    }
    const provision = await this.storage.get(
      provisionKey(input.provision_id),
    );
    const resident = await this.storage.get(
      residentKey(input.account_id),
    );
    if (resident) {
      if (
        resident.account_id !== input.account_id ||
        resident.cell_name !== this.cellName ||
        resident.registration_id !== input.registration_id ||
        (
          resident.route_epoch !== input.route_epoch &&
          !(
            resident.route_epoch === undefined &&
            resident.admitted_by === "signup" &&
            resident.provision_id === input.provision_id
          )
        )
      ) {
        fail("resident account authority conflicts with provisioning", 409);
      }
      if (resident.route_epoch === undefined) {
        await this.storage.put(residentKey(input.account_id), {
          ...resident,
          route_epoch: input.route_epoch,
        });
      }
    } else {
      if (
        !provision ||
        provision.account_id !== input.account_id ||
        provision.registration_id !== input.registration_id ||
        provision.route_epoch !== input.route_epoch
      ) {
        fail("exact provisioning reservation is missing", 409);
      }
      await this.storage.put(residentKey(input.account_id), {
        account_id: input.account_id,
        cell_name: this.cellName,
        registration_id: input.registration_id,
        admitted_by: "signup",
        provision_id: input.provision_id,
        route_epoch: input.route_epoch,
        admitted_at: this.now().toISOString(),
      });
    }
    if (provision) {
      await this.storage.delete(provisionKey(input.provision_id));
    }
    await this.scheduleAlarm();
    return json({
      ok: true,
      provision_id: input.provision_id,
      account_id: input.account_id,
      resident: true,
    });
  }

  async abortProvision(input) {
    if (
      !OPERATION_ID.test(input.provision_id ?? "") ||
      typeof input.registration_id !== "string"
    ) {
      fail("invalid provisioning abort", 400);
    }
    const key = provisionKey(input.provision_id);
    const current = await this.storage.get(key);
    if (!current) {
      return json({
        ok: true,
        provision_id: input.provision_id,
        aborted: false,
      });
    }
    if (
      current.registration_id !== input.registration_id ||
      (
        input.account_id !== undefined &&
        current.account_id !== input.account_id
      )
    ) {
      fail("provisioning reservation changed before abort", 409);
    }
    await this.storage.delete(key);
    await this.scheduleAlarm();
    return json({
      ok: true,
      provision_id: input.provision_id,
      aborted: true,
    });
  }

  async safeDelete(input) {
    await this.reapExpiredReservations();
    await this.reconcileProvisions();
    const liveFence = await this.storage.get(DELETE_FENCE_KEY);
    if (liveFence) {
      fail("cell deletion is already in progress", 409);
    }
    const cell = await this.authoritativeCell();
    if (!cell) {
      fail("unknown cell", 404);
    }
    if (cell.accepting !== false) {
      fail(
        "cell must be drained first (re-register with accepting=false)",
        409,
      );
    }
    await this.backfillResidents(cell);
    await this.reconcileResidents();
    const reservations = await this.storage.list({
      prefix: RESERVATION_PREFIX,
    });
    const provisions = await this.storage.list({
      prefix: PROVISION_PREFIX,
    });
    const residents = await this.storage.list({
      prefix: RESIDENT_PREFIX,
    });
    if (
      reservations.size > 0 ||
      provisions.size > 0 ||
      residents.size > 0
    ) {
      fail(
        "account reservations or residents still belong to this cell",
        409,
      );
    }
    if (await this.cellHasAccounts()) {
      fail("accounts still live on this cell", 409);
    }

    const deletionID =
      typeof input.deletion_id === "string" &&
          OPERATION_ID.test(input.deletion_id)
        ? input.deletion_id
        : this.randomUUID();
    let fence = {
      cell_name: this.cellName,
      deletion_id: deletionID,
      registration_id: cell.registration_id ?? cell.registered_at,
      phase: "begun",
      started_at: this.now().toISOString(),
      expires_at: new Date(
        this.now().getTime() + DELETE_LEASE_MS,
      ).toISOString(),
    };
    await this.storage.put(DELETE_FENCE_KEY, fence);
    await this.scheduleAlarm();

    try {
      // Renew and reread the exact fence immediately before touching the
      // registry projection. Registration and reservation are queued behind
      // this request, so neither can cross this checked boundary.
      fence = await this.renewExactDeleteFence(fence);
      const currentCell = await this.authoritativeCell();
      if (
        !currentCell ||
        (currentCell.registration_id ?? currentCell.registered_at) !==
          fence.registration_id
      ) {
        fail("cell registration changed during deletion", 409);
      }
      await this.finishExactDelete(fence);
      return new Response(null, { status: 204 });
    } catch (error) {
      // Retain the exact fence after any ambiguous projection failure. Its
      // alarm resumes the same deletion; registration/reservation remain
      // blocked until both DO authority and KV projection are retired.
      await this.scheduleAlarm();
      throw error;
    }
  }

  async purge(_input) {
    const purgedCell = await this.authoritativeCell();
    for (
      const prefix of [
        RESERVATION_PREFIX,
        RESIDENT_PREFIX,
        PROVISION_PREFIX,
      ]
    ) {
      const entries = await this.storage.list({ prefix });
      for (const key of entries.keys()) {
        await this.storage.delete(key);
      }
    }
    await this.storage.delete(DELETE_FENCE_KEY);

    let purged = 0;
    let cursor;
    do {
      const page = await this.env.DIRECTORY.list({
        prefix: "acct:",
        cursor,
      });
      for (const key of page.keys) {
        const entry = await this.env.DIRECTORY.get(
          key.name,
          { type: "json" },
        );
        if (entry?.cell === this.cellName) {
          await this.env.DIRECTORY.delete(key.name);
          purged++;
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    await this.deletePendingForCell();
    const existed = (
      await this.env.DIRECTORY.get(`cell:${this.cellName}`)
    ) !== null;
    if (existed) {
      await this.env.DIRECTORY.delete(`cell:${this.cellName}`);
    }
    await this.storage.delete(CELL_STATE_KEY);
    await this.storage.put(LAST_DELETE_KEY, {
      cell_name: this.cellName,
      deletion_id: this.randomUUID(),
      registration_id:
        purgedCell?.registration_id ?? purgedCell?.registered_at ?? null,
      phase: "purged",
      finalized_at: this.now().toISOString(),
    });
    await this.scheduleAlarm();
    return json({
      schema_version: "witself.v0",
      name: this.cellName,
      purged_accounts: purged,
      cell_deleted: existed,
    });
  }

  async status() {
    const reservations = await this.storage.list({
      prefix: RESERVATION_PREFIX,
    });
    const residents = await this.storage.list({
      prefix: RESIDENT_PREFIX,
    });
    const provisions = await this.storage.list({
      prefix: PROVISION_PREFIX,
    });
    const fence = await this.storage.get(DELETE_FENCE_KEY);
    return json({
      ok: true,
      cell_name: this.cellName,
      reservations: reservations.size,
      residents: residents.size,
      provisions: provisions.size,
      deleting: Boolean(fence),
    });
  }

  async registrationStatus(input) {
    if (
      typeof input.registration_id !== "string" ||
      input.registration_id.length < 1 ||
      input.registration_id.length > 128
    ) {
      fail("invalid cell registration status fence", 400);
    }
    const expected = input.registration_id;
    const current = await this.authoritativeCell();
    const currentRegistration =
      current?.registration_id ?? current?.registered_at ?? null;
    const tombstone = await this.storage.get(LAST_DELETE_KEY);
    const tombstoneRegistration = tombstone?.registration_id ?? null;
    let status = "unknown";
    if (currentRegistration === expected) {
      status = "active";
    } else if (currentRegistration) {
      status = "replaced";
    } else if (
      tombstoneRegistration === expected &&
      ["finalized", "purged"].includes(tombstone?.phase)
    ) {
      status = "deleted";
    }
    return json({
      ok: true,
      cell_name: this.cellName,
      expected_registration_id: expected,
      registration_status: status,
      current_registration_id: currentRegistration,
      tombstone_registration_id: tombstoneRegistration,
      // This endpoint is reachable only over the internal Durable Object
      // binding. Returning the exact active registration lets a lifecycle
      // finish source teardown even when the DIRECTORY KV projection is
      // missing or stale; the value is never exposed by the public API.
      active_cell:
        status === "active" ? current : null,
    });
  }

  async authoritativeCell() {
    const stored = await this.storage.get(CELL_STATE_KEY);
    if (stored) {
      return stored;
    }
    const projected = await this.env.DIRECTORY.get(
      `cell:${this.cellName}`,
      { type: "json" },
    );
    if (!projected) {
      return null;
    }
    // Migrate pre-v4 registry entries once. A finalized delete tombstone means
    // a lagging KV read must never resurrect the retired registration.
    if (await this.storage.get(LAST_DELETE_KEY)) {
      return null;
    }
    await this.storage.put(CELL_STATE_KEY, projected);
    return projected;
  }

  async finishExactDelete(expected) {
    const currentFence = await this.storage.get(DELETE_FENCE_KEY);
    if (
      !currentFence ||
      currentFence.deletion_id !== expected.deletion_id ||
      currentFence.registration_id !== expected.registration_id
    ) {
      fail("cell delete fence changed before finalization", 409);
    }
    const currentCell = await this.storage.get(CELL_STATE_KEY);
    if (
      currentCell &&
      (currentCell.registration_id ?? currentCell.registered_at) !==
        expected.registration_id
    ) {
      fail("cell registration changed before delete finalization", 409);
    }
    await this.deletePendingForCell();
    await this.env.DIRECTORY.delete(`cell:${this.cellName}`);
    await this.storage.delete(CELL_STATE_KEY);
    await this.storage.put(LAST_DELETE_KEY, {
      ...currentFence,
      phase: "finalized",
      finalized_at: this.now().toISOString(),
    });
    await this.storage.delete(DELETE_FENCE_KEY);
    await this.scheduleAlarm();
  }

  async renewExactDeleteFence(expected) {
    const current = await this.storage.get(DELETE_FENCE_KEY);
    if (
      !current ||
      current.deletion_id !== expected.deletion_id ||
      current.registration_id !== expected.registration_id
    ) {
      fail("cell delete fence changed before commit", 409);
    }
    const renewed = {
      ...current,
      expires_at: new Date(
        this.now().getTime() + DELETE_LEASE_MS,
      ).toISOString(),
    };
    await this.storage.put(DELETE_FENCE_KEY, renewed);
    return renewed;
  }

  async recoverExpiredDeleteFence() {
    const fence = await this.storage.get(DELETE_FENCE_KEY);
    if (!fence) return;
    const expiresAt = Date.parse(fence.expires_at ?? "");
    if (!Number.isFinite(expiresAt) || expiresAt > this.now().getTime()) {
      return;
    }
    // A handler that owns this DO cannot still be running here: every handler
    // is in the serial queue. Resume the exact crash-left deletion instead of
    // guessing from an eventually consistent KV read.
    await this.finishExactDelete(fence);
  }

  async reapExpiredReservations() {
    const reservations = await this.storage.list({
      prefix: RESERVATION_PREFIX,
    });
    for (const [key, reservation] of reservations) {
      const expiresAt = Date.parse(reservation.expires_at ?? "");
      if (
        Number.isFinite(expiresAt) &&
        expiresAt > this.now().getTime()
      ) {
        continue;
      }
      const liveness = await this.accountReservationLiveness(reservation);
      const current = await this.storage.get(key);
      if (!current || !exactReservation(current, reservation)) {
        continue;
      }
      if (liveness === false) {
        await this.storage.delete(key);
        continue;
      }
      // Active or ambiguous both fail closed. Ambiguity retries sooner, while
      // an exact active lifecycle receives a full lease renewal.
      const extension = liveness === true
        ? RESERVATION_LEASE_MS
        : AMBIGUOUS_LIVENESS_RETRY_MS;
      await this.storage.put(key, {
        ...current,
        renewed_at: this.now().toISOString(),
        expires_at: new Date(
          this.now().getTime() + extension,
        ).toISOString(),
      });
    }
    await this.scheduleAlarm();
  }

  async accountReservationLiveness(reservation) {
    if (!this.env.ACCOUNT_LIFECYCLE) {
      return null;
    }
    try {
      const id = this.env.ACCOUNT_LIFECYCLE.idFromName(
        reservation.account_id,
      );
      const response = await this.env.ACCOUNT_LIFECYCLE.get(id).fetch(
        new Request(
          "https://account-lifecycle.internal/reservation-status",
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              account_id: reservation.account_id,
              operation_id: reservation.operation_id,
              evacuation_id: reservation.evacuation_id,
              target_cell: reservation.target_cell,
            }),
          },
        ),
      );
      const body = await response.json().catch(() => null);
      if (
        !response.ok ||
        body?.account_id !== reservation.account_id ||
        body?.operation_id !== reservation.operation_id ||
        body?.evacuation_id !== reservation.evacuation_id ||
        body?.target_cell !== reservation.target_cell ||
        typeof body?.active !== "boolean"
      ) {
        return null;
      }
      return body.active;
    } catch {
      return null;
    }
  }

  async reconcileResidents() {
    const residents = await this.storage.list({
      prefix: RESIDENT_PREFIX,
    });
    for (const [key, resident] of residents) {
      const liveness = await this.accountResidencyLiveness(resident);
      if (liveness !== false) {
        continue;
      }
      const current = await this.storage.get(key);
      if (
        current?.account_id === resident.account_id &&
        current?.registration_id === resident.registration_id &&
        current?.route_epoch === resident.route_epoch
      ) {
        await this.storage.delete(key);
      }
    }
  }

  async accountResidencyLiveness(resident) {
    if (!this.env.ACCOUNT_LIFECYCLE) {
      return null;
    }
    try {
      const id = this.env.ACCOUNT_LIFECYCLE.idFromName(
        resident.account_id,
      );
      const response = await this.env.ACCOUNT_LIFECYCLE.get(id).fetch(
        new Request(
          "https://account-lifecycle.internal/residency-status",
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              account_id: resident.account_id,
              cell_name: this.cellName,
              registration_id: resident.registration_id,
              route_epoch: resident.route_epoch,
            }),
          },
        ),
      );
      const body = await response.json().catch(() => null);
      if (
        !response.ok ||
        body?.account_id !== resident.account_id ||
        body?.cell_name !== this.cellName ||
        body?.registration_id !== resident.registration_id ||
        body?.route_epoch !== resident.route_epoch ||
        typeof body?.resident !== "boolean"
      ) {
        return null;
      }
      return body.resident;
    } catch {
      return null;
    }
  }

  async reconcileProvisions() {
    const provisions = await this.storage.list({
      prefix: PROVISION_PREFIX,
    });
    for (const [key, provision] of provisions) {
      const expiresAt = Date.parse(provision.expires_at ?? "");
      if (
        Number.isFinite(expiresAt) &&
        expiresAt > this.now().getTime()
      ) {
        continue;
      }
      if (provision.account_id) {
        const route = await this.env.DIRECTORY.get(
          `acct:${provision.account_id}`,
          { type: "json" },
        );
        if (
          route?.cell === this.cellName &&
          route?.cell_registration_id === provision.registration_id &&
          (
            Number.isSafeInteger(route.epoch) ? route.epoch : 0
          ) === provision.route_epoch
        ) {
          await this.storage.put(residentKey(provision.account_id), {
            account_id: provision.account_id,
            cell_name: this.cellName,
            registration_id: provision.registration_id,
            admitted_by: "signup_recovery",
            provision_id: provision.provision_id,
            route_epoch: Number.isSafeInteger(route.epoch)
              ? route.epoch
              : 0,
            admitted_at: this.now().toISOString(),
          });
          await this.storage.delete(key);
          continue;
        }
      }
      // A provision request can commit at the cell before its HTTP response is
      // received, so absence of a KV route is not proof that no account
      // exists. Keep the lease fail-closed for manual/exact recovery.
      await this.storage.put(key, {
        ...provision,
        renewed_at: this.now().toISOString(),
        expires_at: new Date(
          this.now().getTime() + AMBIGUOUS_LIVENESS_RETRY_MS,
        ).toISOString(),
      });
    }
  }

  async cellHasAccounts() {
    let cursor;
    do {
      const page = await this.env.DIRECTORY.list({
        prefix: "acct:",
        cursor,
      });
      for (const key of page.keys) {
        const entry = await this.env.DIRECTORY.get(
          key.name,
          { type: "json" },
        );
        if (entry?.cell === this.cellName) {
          return true;
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    return false;
  }

  async cellHasPendingAccounts() {
    let cursor;
    do {
      const page = await this.env.DIRECTORY.list({
        prefix: "pending:",
        cursor,
      });
      for (const key of page.keys) {
        const entry = await this.env.DIRECTORY.get(
          key.name,
          { type: "json" },
        );
        if (entry?.cell === this.cellName) {
          return true;
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
    return false;
  }

  async backfillResidents(cell) {
    const registrationID =
      cell.registration_id ?? cell.registered_at ?? null;
    if (!registrationID) {
      return;
    }
    let cursor;
    do {
      const page = await this.env.DIRECTORY.list({
        prefix: "acct:",
        cursor,
      });
      for (const key of page.keys) {
        const route = await this.env.DIRECTORY.get(
          key.name,
          { type: "json" },
        );
        if (route?.cell !== this.cellName) {
          continue;
        }
        const accountID = key.name.slice("acct:".length);
        if (!ACCOUNT_ID.test(accountID)) {
          continue;
        }
        const keyName = residentKey(accountID);
        const existing = await this.storage.get(keyName);
        const routeEpoch = Number.isSafeInteger(route.epoch)
          ? route.epoch
          : 0;
        if (existing) {
          if (
            existing.route_epoch === undefined &&
            existing.registration_id ===
              (route.cell_registration_id ?? registrationID)
          ) {
            await this.storage.put(keyName, {
              ...existing,
              route_epoch: routeEpoch,
            });
          }
          continue;
        }
        await this.storage.put(keyName, {
          account_id: accountID,
          cell_name: this.cellName,
          registration_id:
            route.cell_registration_id ?? registrationID,
          admitted_by: "legacy_route_backfill",
          route_epoch: routeEpoch,
          admitted_at: this.now().toISOString(),
        });
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
  }

  async deletePendingForCell() {
    let cursor;
    do {
      const page = await this.env.DIRECTORY.list({
        prefix: "pending:",
        cursor,
      });
      for (const key of page.keys) {
        const entry = await this.env.DIRECTORY.get(
          key.name,
          { type: "json" },
        );
        if (entry?.cell === this.cellName) {
          await this.env.DIRECTORY.delete(key.name);
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
  }

  async scheduleAlarm() {
    if (typeof this.storage.setAlarm !== "function") return;
    const candidates = [];
    const fence = await this.storage.get(DELETE_FENCE_KEY);
    const fenceExpiry = Date.parse(fence?.expires_at ?? "");
    if (Number.isFinite(fenceExpiry)) candidates.push(fenceExpiry);
    const reservations = await this.storage.list({
      prefix: RESERVATION_PREFIX,
    });
    for (const reservation of reservations.values()) {
      const expiry = Date.parse(reservation.expires_at ?? "");
      if (Number.isFinite(expiry)) candidates.push(expiry);
    }
    const provisions = await this.storage.list({
      prefix: PROVISION_PREFIX,
    });
    for (const provision of provisions.values()) {
      const expiry = Date.parse(provision.expires_at ?? "");
      if (Number.isFinite(expiry)) candidates.push(expiry);
    }
    if (candidates.length === 0) {
      if (typeof this.storage.deleteAlarm === "function") {
        await this.storage.deleteAlarm();
      }
      return;
    }
    await this.storage.setAlarm(
      Math.max(this.now().getTime() + 1000, Math.min(...candidates)),
    );
  }

  async handleAlarm() {
    await this.recoverExpiredDeleteFence();
    await this.reapExpiredReservations();
    await this.reconcileProvisions();
    await this.scheduleAlarm();
  }
}
