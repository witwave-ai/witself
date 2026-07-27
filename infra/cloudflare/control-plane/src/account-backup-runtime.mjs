import {
  AccountLifecycleBusyError,
  AccountLifecycleFence,
} from "./account-lifecycle-fence.mjs";
import {
  ArchiveIntegrityError,
  validateCommittedAccountArchive,
} from "./archive-integrity.mjs";
import { streamToR2Multipart } from "./account-lifecycle-runtime.mjs";
import { activeAccountPage } from "./bridge.mjs";

export const ACCOUNT_BACKUP_STATE_SCHEMA = "witself.account-backup.v1";
export const ACCOUNT_BACKUP_SCAN_SCHEMA =
  "witself.account-backup-scan.v1";
export const ACCOUNT_BACKUP_SCAN_KEY = "config:account_backup_scan";

const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const BACKUP_ID = /^backup_[0-9]{8}T[0-9]{6}Z$/;
const BACKUP_OBJECT_MAX_LENGTH = 1024;
const SHA256_HEX = /^[0-9a-f]{64}$/;
const R2_ETAG_MAX_LENGTH = 256;
const STATE_KEY = "account-backups";
const DEFAULT_INTERVAL_MINUTES = 24 * 60;
const DEFAULT_PAGE_SIZE = 4;
const DEFAULT_CONCURRENCY = 2;
const DEFAULT_MAX_ATTEMPTS = 4;
const DEFAULT_CATALOG_LIMIT = 64;
const EXPORT_TIMEOUT_MS = 5 * 60 * 1000;
const BASE_RETRY_MS = 60_000;
const MAX_RETRY_MS = 30 * 60_000;
const SCAN_FAILURE_SAMPLE_LIMIT = 16;
const SCAN_FAILURE_STATUS = /^[a-z0-9][a-z0-9_-]{0,63}$/;
const SCAN_FAILED_RETRY_POLICY = "next_interval";

function isObject(value) {
  return value !== null && typeof value === "object" &&
    !Array.isArray(value);
}

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(message, status) {
  return json({
    schema_version: "witself.v0",
    error: message,
  }, status);
}

function boundedInteger(raw, fallback, minimum, maximum) {
  if (raw === undefined || raw === null || String(raw).trim() === "") {
    return fallback;
  }
  const value = Number(raw);
  return Number.isSafeInteger(value) &&
    value >= minimum && value <= maximum
    ? value
    : fallback;
}

function sanitizedScanFailureStatus(value) {
  if (typeof value !== "string") {
    return "dispatch_failed";
  }
  const sanitized = value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "_")
    .replace(/^_+|_+$/g, "")
    .slice(0, 64);
  return SCAN_FAILURE_STATUS.test(sanitized)
    ? sanitized
    : "dispatch_failed";
}

export function accountBackupConfig(env) {
  return Object.freeze({
    enabled: String(env.CP_ACCOUNT_BACKUPS_ENABLED ?? "")
      .trim().toLowerCase() === "true",
    interval_minutes: boundedInteger(
      env.CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES,
      DEFAULT_INTERVAL_MINUTES,
      5,
      31 * 24 * 60,
    ),
    page_size: boundedInteger(
      env.CP_ACCOUNT_BACKUPS_PAGE_SIZE,
      DEFAULT_PAGE_SIZE,
      1,
      20,
    ),
    concurrency: boundedInteger(
      env.CP_ACCOUNT_BACKUPS_CONCURRENCY,
      DEFAULT_CONCURRENCY,
      1,
      5,
    ),
    max_attempts: boundedInteger(
      env.CP_ACCOUNT_BACKUPS_MAX_ATTEMPTS,
      DEFAULT_MAX_ATTEMPTS,
      1,
      10,
    ),
    catalog_limit: boundedInteger(
      env.CP_ACCOUNT_BACKUPS_CATALOG_LIMIT,
      DEFAULT_CATALOG_LIMIT,
      1,
      256,
    ),
  });
}

function validDate(value) {
  return typeof value === "string" &&
    Number.isFinite(Date.parse(value));
}

function validR2ETag(value) {
  return typeof value === "string" &&
    value.length >= 1 &&
    value.length <= R2_ETAG_MAX_LENGTH &&
    !/[\u0000-\u001f\u007f]/.test(value);
}

function sourceRouteEpoch(authority) {
  if (
    Number.isSafeInteger(authority?.source_route_epoch) &&
    authority.source_route_epoch >= 0
  ) {
    return authority.source_route_epoch;
  }
  if (
    Number.isSafeInteger(authority?.source_route?.epoch) &&
    authority.source_route.epoch >= 0
  ) {
    return authority.source_route.epoch;
  }
  return null;
}

function assertR2ObjectIdentity(
  authority,
  object,
  expectedSize = undefined,
  expectedETag = undefined,
) {
  const metadata = object?.customMetadata;
  const routeEpoch = sourceRouteEpoch(authority);
  if (
    !object ||
    !validR2ETag(object.etag) ||
    !Number.isSafeInteger(object.size) ||
    object.size <= 0 ||
    !isObject(metadata) ||
    metadata.account_id !== authority.account_id ||
    metadata.backup_id !== authority.backup_id ||
    metadata.cell !== authority.source_cell ||
    metadata.cell_registered_at !== authority.source_registered_at ||
    metadata.cell_registration_id !== authority.source_registration_id ||
    metadata.scheduled_at !== authority.scheduled_at ||
    (
      routeEpoch === null
        ? metadata.route_epoch !== undefined &&
          metadata.route_epoch !== null &&
          metadata.route_epoch !== ""
        : metadata.route_epoch !== String(routeEpoch)
    ) ||
    (
      Number.isSafeInteger(expectedSize) &&
      expectedSize > 0 &&
      object.size !== expectedSize
    ) ||
    (
      expectedETag !== undefined &&
      object.etag !== expectedETag
    )
  ) {
    throw new ArchiveIntegrityError(
      "backup R2 object identity does not match its durable authority",
    );
  }
  return object;
}

function compactTimestamp(value) {
  return value.toISOString()
    .replaceAll("-", "")
    .replaceAll(":", "")
    .replace(".000", "");
}

export function backupJobIdentity(
  accountID,
  scheduledTime,
  intervalMinutes = DEFAULT_INTERVAL_MINUTES,
) {
  if (!ACCOUNT_ID.test(accountID ?? "")) {
    throw new Error("invalid account id for backup job");
  }
  const milliseconds = scheduledTime instanceof Date
    ? scheduledTime.getTime()
    : Number(scheduledTime);
  if (!Number.isFinite(milliseconds) || milliseconds < 0) {
    throw new Error("invalid scheduled backup time");
  }
  if (
    !Number.isSafeInteger(intervalMinutes) ||
    intervalMinutes < 1
  ) {
    throw new Error("invalid backup interval");
  }
  const intervalMilliseconds = intervalMinutes * 60_000;
  const slotMilliseconds =
    Math.floor(milliseconds / intervalMilliseconds) * intervalMilliseconds;
  const slot = new Date(slotMilliseconds);
  const compact = compactTimestamp(slot);
  const backupID = `backup_${compact}`;
  const year = compact.slice(0, 4);
  const month = compact.slice(4, 6);
  const day = compact.slice(6, 8);
  return Object.freeze({
    account_id: accountID,
    backup_id: backupID,
    scheduled_at: slot.toISOString(),
    object:
      `accounts/${accountID}/${year}/${month}/${day}/${backupID}.tar.gz`,
  });
}

function emptyState(accountID) {
  return {
    schema_version: ACCOUNT_BACKUP_STATE_SCHEMA,
    account_id: accountID,
    revision: 0,
    current_job: null,
    catalog: [],
    failures: [],
  };
}

function validJob(job, accountID) {
  return isObject(job) &&
    job.account_id === accountID &&
    BACKUP_ID.test(job.backup_id ?? "") &&
    validDate(job.scheduled_at) &&
    typeof job.object === "string" &&
    job.object.length >= 1 &&
    job.object.length <= BACKUP_OBJECT_MAX_LENGTH &&
    !/[\u0000-\u001f\u007f]/.test(job.object) &&
    ["pending", "running", "retrying", "failed", "committed"]
      .includes(job.status) &&
    Number.isSafeInteger(job.attempts) &&
    job.attempts >= 0 &&
    Number.isSafeInteger(job.max_attempts) &&
    job.max_attempts >= 1 &&
    job.max_attempts <= 10 &&
    CELL_NAME.test(job.source_cell ?? "") &&
    validCellEndpoint(job.source_endpoint) !== null &&
    typeof job.source_registration_id === "string" &&
    job.source_registration_id.length >= 1 &&
    job.source_registration_id.length <= 128 &&
    validDate(job.source_registered_at) &&
    validRouteFence(job.source_route, job.source_cell);
}

function validCatalogRecord(record, accountID) {
  return isObject(record) &&
    record.account_id === accountID &&
    BACKUP_ID.test(record.backup_id ?? "") &&
    typeof record.object === "string" &&
    record.object.length >= 1 &&
    record.object.length <= BACKUP_OBJECT_MAX_LENGTH &&
    CELL_NAME.test(record.source_cell ?? "") &&
    typeof record.source_registration_id === "string" &&
    record.source_registration_id.length >= 1 &&
    record.source_registration_id.length <= 128 &&
    validDate(record.source_registered_at) &&
    (
      record.source_route_epoch === null ||
      (
        Number.isSafeInteger(record.source_route_epoch) &&
        record.source_route_epoch >= 0
      )
    ) &&
    validDate(record.scheduled_at) &&
    validDate(record.exported_at) &&
    validDate(record.verified_at) &&
    Number.isSafeInteger(record.size) &&
    record.size > 0 &&
    Number.isSafeInteger(record.archive_schema_version) &&
    record.archive_schema_version >= 0 &&
    Number.isSafeInteger(record.entries) &&
    record.entries >= 2 &&
    Number.isSafeInteger(record.chunks) &&
    record.chunks >= 1 &&
    SHA256_HEX.test(record.trailer_sha256 ?? "") &&
    validR2ETag(record.r2_etag) &&
    ["active", "suspended", "closed"].includes(record.status) &&
    (
      record.validations === undefined ||
      (
        Array.isArray(record.validations) &&
        record.validations.length <= 16 &&
        record.validations.every(
          (validation) =>
            isObject(validation) &&
            CELL_NAME.test(validation.target_cell ?? "") &&
            validDate(validation.validated_at) &&
            validation.status === record.status &&
            validation.archive_schema_version ===
              record.archive_schema_version,
        )
      )
    );
}

export function validateAccountBackupState(state, expectedAccountID) {
  if (
    !isObject(state) ||
    state.schema_version !== ACCOUNT_BACKUP_STATE_SCHEMA ||
    state.account_id !== expectedAccountID ||
    !ACCOUNT_ID.test(state.account_id ?? "") ||
    !Number.isSafeInteger(state.revision) ||
    state.revision < 0 ||
    !Array.isArray(state.catalog) ||
    !Array.isArray(state.failures) ||
    (
      state.current_job !== null &&
      !validJob(state.current_job, expectedAccountID)
    ) ||
    state.catalog.some(
      (record) => !validCatalogRecord(record, expectedAccountID),
    ) ||
    state.failures.some(
      (record) =>
        !validJob(record, expectedAccountID) ||
        record.status !== "failed",
    )
  ) {
    throw new Error("durable account backup state is invalid");
  }
  const ids = new Set();
  for (const record of state.catalog) {
    if (ids.has(record.backup_id)) {
      throw new Error("durable account backup catalog contains duplicates");
    }
    ids.add(record.backup_id);
  }
  return state;
}

function validCellEndpoint(value) {
  if (typeof value !== "string") return null;
  try {
    const endpoint = new URL(value);
    if (
      endpoint.protocol !== "https:" ||
      !endpoint.hostname ||
      endpoint.username ||
      endpoint.password
    ) {
      return null;
    }
    return value.replace(/\/+$/, "");
  } catch {
    return null;
  }
}

function routeFence(route) {
  return {
    cell: route.cell,
    endpoint: typeof route.endpoint === "string" ? route.endpoint : null,
    region: typeof route.region === "string" ? route.region : null,
    region_code:
      typeof route.region_code === "string" ? route.region_code : null,
    cell_registration_id:
      typeof route.cell_registration_id === "string"
        ? route.cell_registration_id
        : null,
    epoch: Number.isSafeInteger(route.epoch) && route.epoch >= 0
      ? route.epoch
      : null,
  };
}

function validRouteFence(value, expectedCell) {
  return isObject(value) &&
    value.cell === expectedCell &&
    (value.endpoint === null || typeof value.endpoint === "string") &&
    (value.region === null || typeof value.region === "string") &&
    (value.region_code === null || typeof value.region_code === "string") &&
    (
      value.cell_registration_id === null ||
      (
        typeof value.cell_registration_id === "string" &&
        value.cell_registration_id.length >= 1 &&
        value.cell_registration_id.length <= 128
      )
    ) &&
    (
      value.epoch === null ||
      (
        Number.isSafeInteger(value.epoch) &&
        value.epoch >= 0
      )
    );
}

function sameRouteFence(left, right) {
  return JSON.stringify(left) === JSON.stringify(right);
}

function boundedReason(error) {
  const message = String(error?.message ?? error ?? "backup failed").trim();
  return (message || "backup failed").slice(0, 300);
}

function retryDelay(attempts) {
  return Math.min(
    BASE_RETRY_MS * (2 ** Math.max(0, attempts - 1)),
    MAX_RETRY_MS,
  );
}

function clone(value) {
  return structuredClone(value);
}

/**
 * One Durable Object exists per account. It owns one in-flight backup job and
 * a bounded catalog of committed immutable objects. R2 is not the authority
 * until the completed object has been fetched back and fully validated.
 */
export class DurableAccountBackup {
  constructor(ctx, env, dependencies = {}) {
    this.ctx = ctx;
    this.storage = ctx.storage;
    this.env = env;
    this.accountId = ctx.id?.name ?? null;
    this.fence = new AccountLifecycleFence();
    this.fetchImpl =
      dependencies.fetch ?? ((...args) => globalThis.fetch(...args));
    this.streamArchive =
      dependencies.streamArchive ?? streamToR2Multipart;
    this.validateArchive =
      dependencies.validateArchive ?? validateCommittedAccountArchive;
    this.now = dependencies.now ?? (() => new Date());
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/status") {
      try {
        return json({
          schema_version: "witself.v0",
          account_id: this.accountId,
          backups: await this.loadState(),
        });
      } catch (error) {
        return errorResponse(boundedReason(error), 500);
      }
    }
    if (
      request.method === "POST" &&
      url.pathname === "/validation-verified"
    ) {
      let input;
      try {
        input = await request.json();
      } catch {
        return errorResponse("invalid backup validation receipt", 400);
      }
      try {
        return await this.fence.run(async () =>
          json({
            schema_version: "witself.v0",
            account_id: this.accountId,
            backup: await this.recordValidation(input),
          })
        );
      } catch (error) {
        if (error instanceof AccountLifecycleBusyError) {
          return errorResponse(error.message, 409);
        }
        return errorResponse(boundedReason(error), 400);
      }
    }
    if (request.method !== "POST" || url.pathname !== "/run") {
      return errorResponse("account backup endpoint not found", 404);
    }

    let input;
    try {
      input = await request.json();
    } catch {
      return errorResponse("invalid account backup request", 400);
    }
    if (!this.validInput(input)) {
      return errorResponse("invalid account backup request", 400);
    }

    try {
      return await this.fence.run(async () => {
        const result = await this.run(input);
        return json({
          schema_version: "witself.v0",
          account_id: this.accountId,
          backup_id: input.backup_id,
          ...result,
        }, result.status === "retrying" ? 202 : 200);
      });
    } catch (error) {
      if (error instanceof AccountLifecycleBusyError) {
        return errorResponse(error.message, 409);
      }
      return errorResponse(boundedReason(error), 500);
    }
  }

  validInput(input) {
    if (!(isObject(input) &&
      input.account_id === this.accountId &&
      ACCOUNT_ID.test(input.account_id ?? "") &&
      BACKUP_ID.test(input.backup_id ?? "") &&
      validDate(input.scheduled_at) &&
      typeof input.object === "string" &&
      input.object.length >= 1 &&
      input.object.length <= BACKUP_OBJECT_MAX_LENGTH &&
      !/[\u0000-\u001f\u007f]/.test(input.object) &&
      Number.isSafeInteger(input.max_attempts) &&
      input.max_attempts >= 1 &&
      input.max_attempts <= 10 &&
      Number.isSafeInteger(input.catalog_limit) &&
      input.catalog_limit >= 1 &&
      input.catalog_limit <= 256)) {
      return false;
    }
    const expected = backupJobIdentity(
      input.account_id,
      Date.parse(input.scheduled_at),
      1,
    );
    return input.backup_id === expected.backup_id &&
      input.object === expected.object &&
      input.scheduled_at === expected.scheduled_at;
  }

  async loadState() {
    const stored = await this.storage.get(STATE_KEY);
    if (stored === undefined || stored === null) {
      return emptyState(this.accountId);
    }
    return validateAccountBackupState(stored, this.accountId);
  }

  async saveState(state) {
    validateAccountBackupState(state, this.accountId);
    await this.storage.put(STATE_KEY, state);
    return state;
  }

  async run(input) {
    let state = await this.loadState();
    const committed = state.catalog.find(
      (record) => record.backup_id === input.backup_id,
    );
    if (committed) {
      return { status: "committed", backup: committed };
    }

    const current = state.current_job;
    if (current && current.backup_id !== input.backup_id) {
      if (["pending", "running", "retrying"].includes(current.status)) {
        await this.scheduleRetry(current);
        return {
          status: "busy",
          current_backup_id: current.backup_id,
        };
      }
    }

    if (!current || current.backup_id !== input.backup_id) {
      const source = await this.sourceCellSnapshot();
      state = {
        ...state,
        revision: state.revision + 1,
        current_job: {
          account_id: this.accountId,
          backup_id: input.backup_id,
          object: input.object,
          scheduled_at: input.scheduled_at,
          status: "pending",
          attempts: 0,
          max_attempts: input.max_attempts,
          created_at: this.now().toISOString(),
          source_cell: source.name,
          source_endpoint: source.endpoint,
          source_registration_id: source.registration_id,
          source_registered_at: source.registered_at,
          source_route: source.route,
        },
      };
      await this.saveState(state);
    } else if (current.status === "failed") {
      return { status: "failed", attempts: current.attempts };
    }

    return this.execute(input.catalog_limit);
  }

  async execute(catalogLimit) {
    let state = await this.loadState();
    let job = state.current_job;
    if (!job || job.status === "committed") {
      return { status: job?.status ?? "idle" };
    }
    if (job.status === "failed") {
      return { status: "failed", attempts: job.attempts };
    }
    if (job.attempts >= job.max_attempts) {
      state = await this.failJob(
        state,
        job,
        new Error("backup retry budget exhausted"),
        false,
      );
      return { status: "failed", attempts: state.current_job.attempts };
    }

    const {
      retry_at: _retryAt,
      ...resumingJob
    } = job;
    job = {
      ...resumingJob,
      status: "running",
      attempts: job.attempts + 1,
      started_at: this.now().toISOString(),
    };
    state = {
      ...state,
      revision: state.revision + 1,
      current_job: job,
    };
    await this.saveState(state);

    try {
      // Manual runs must remain resumable even when fleet scheduling is
      // disabled. Arm the Durable Object alarm before any outbound I/O; a
      // successful commit clears it, while retry state moves it forward.
      await this.scheduleRetry(job);
      const existing = await this.existingObject(job);
      if (existing) {
        state = await this.commitJob(
          state,
          job,
          existing,
          catalogLimit,
        );
        return {
          status: "committed",
          backup: state.catalog[0],
          recovered_existing_object: true,
        };
      }

      const source = await this.sourceCell(job);
      const response = await this.fetchImpl(
        `${source.endpoint}/v1/accounts/${this.accountId}:export-backup`,
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${source.backup_token}`,
            "X-Witself-Backup-ID": job.backup_id,
          },
          signal: AbortSignal.timeout(EXPORT_TIMEOUT_MS),
        },
      );
      if (!response.ok || !response.body) {
        const detail = (await response.text().catch(() => "")).slice(0, 200);
        throw new Error(
          `backup export ${response.status}: ${detail}`,
        );
      }
      if (
        response.headers.get("X-Witself-Backup-ID") !== job.backup_id
      ) {
        throw new Error(
          "backup export did not acknowledge the exact backup id",
        );
      }

      const streamedAt = this.now().toISOString();
      const size = await this.streamArchive(
        this.env.BACKUPS,
        job.object,
        response.body,
        {
          httpMetadata: {
            contentType: "application/gzip",
            contentDisposition:
              `attachment; filename="${this.accountId}-${job.backup_id}.tar.gz"`,
          },
          customMetadata: {
            account_id: this.accountId,
            backup_id: job.backup_id,
            cell: source.name,
            cell_registered_at: source.registered_at,
            cell_registration_id: source.registration_id,
            scheduled_at: job.scheduled_at,
            streamed_at: streamedAt,
            ...(job.source_route.epoch === null
              ? {}
              : { route_epoch: String(job.source_route.epoch) }),
          },
        },
      );
      const verification = await this.verifyObject(job, source.name, size);
      state = await this.commitJob(
        state,
        job,
        verification,
        catalogLimit,
      );
      return { status: "committed", backup: state.catalog[0] };
    } catch (error) {
      state = await this.failJob(state, job, error, true);
      return {
        status: state.current_job.status,
        attempts: state.current_job.attempts,
        retry_at: state.current_job.retry_at ?? null,
      };
    }
  }

  async sourceCellSnapshot() {
    const route = await this.env.DIRECTORY.get(
      `acct:${this.accountId}`,
      { type: "json" },
    );
    if (!CELL_NAME.test(route?.cell ?? "")) {
      throw new Error("account has no active backup source route");
    }
    const cell = await this.env.DIRECTORY.get(
      `cell:${route.cell}`,
      { type: "json" },
    );
    const endpoint = validCellEndpoint(cell?.endpoint);
    const registrationID =
      cell?.registration_id ?? cell?.registered_at ?? null;
    if (
      !endpoint ||
      typeof cell?.backup_token !== "string" ||
      cell.backup_token.length === 0 ||
      (
        typeof cell.provision_token === "string" &&
        cell.provision_token.length > 0 &&
        cell.backup_token === cell.provision_token
      ) ||
      typeof registrationID !== "string" ||
      registrationID.length < 1 ||
      registrationID.length > 128 ||
      !validDate(cell?.registered_at) ||
      (
        route.cell_registration_id != null &&
        route.cell_registration_id !== registrationID
      )
    ) {
      throw new Error("account backup source cell is not configured");
    }
    return {
      name: route.cell,
      endpoint,
      backup_token: cell.backup_token,
      registration_id: registrationID,
      registered_at: cell.registered_at,
      route: routeFence(route),
    };
  }

  async sourceCell(job) {
    const source = await this.sourceCellSnapshot();
    if (
      source.name !== job.source_cell ||
      source.endpoint !== job.source_endpoint ||
      source.registration_id !== job.source_registration_id ||
      source.registered_at !== job.source_registered_at ||
      !sameRouteFence(source.route, job.source_route)
    ) {
      throw new Error(
        "account backup source fence changed before commit",
      );
    }
    return source;
  }

  async existingObject(job) {
    if (typeof this.env.BACKUPS?.head !== "function") {
      return null;
    }
    const existing = await this.env.BACKUPS.head(job.object);
    if (!existing) return null;
    try {
      return await this.verifyObject(
        job,
        job.source_cell,
        existing.size,
      );
    } catch (error) {
      if (error instanceof ArchiveIntegrityError) {
        // No catalog entry points at this attempt object, so it has not crossed
        // the immutable authority boundary and may be removed before retry.
        await this.env.BACKUPS.delete(job.object);
        return null;
      }
      throw error;
    }
  }

  async verifyObject(job, sourceCell, sizeHint) {
    if (typeof this.env.BACKUPS?.head !== "function") {
      throw new ArchiveIntegrityError(
        "backup R2 binding cannot verify object identity",
      );
    }
    const before = assertR2ObjectIdentity(
      job,
      await this.env.BACKUPS.head(job.object),
      sizeHint,
    );
    const verification = await this.validateArchive(
      this.env.BACKUPS,
      job.object,
      this.accountId,
    );
    const manifest = verification?.manifest;
    if (
      manifest?.account_id !== this.accountId ||
      manifest?.backup_id !== job.backup_id ||
      manifest?.purpose !== "backup" ||
      manifest?.cell !== sourceCell ||
      (
        manifest.evacuation_id !== undefined &&
        manifest.evacuation_id !== null &&
        manifest.evacuation_id !== ""
      ) ||
      !validDate(manifest?.exported_at) ||
      !["active", "suspended", "closed"].includes(manifest?.status) ||
      !Number.isSafeInteger(manifest?.schema_version) ||
      manifest.schema_version < 0 ||
      !CELL_NAME.test(sourceCell ?? "") ||
      !Number.isSafeInteger(verification?.entries) ||
      verification.entries < 2 ||
      !Number.isSafeInteger(verification?.chunks) ||
      verification.chunks < 1 ||
      !SHA256_HEX.test(verification?.trailer_sha256 ?? "")
    ) {
      throw new ArchiveIntegrityError(
        "committed backup manifest does not match the exact backup job",
      );
    }
    const after = assertR2ObjectIdentity(
      job,
      await this.env.BACKUPS.head(job.object),
      before.size,
      before.etag,
    );
    return {
      account_id: this.accountId,
      backup_id: job.backup_id,
      object: job.object,
      source_cell: sourceCell,
      source_registration_id: job.source_registration_id,
      source_registered_at: job.source_registered_at,
      source_route_epoch: sourceRouteEpoch(job),
      scheduled_at: job.scheduled_at,
      exported_at: manifest.exported_at,
      verified_at: this.now().toISOString(),
      status: manifest.status,
      size: after.size,
      archive_schema_version: manifest.schema_version,
      entries: verification.entries,
      chunks: verification.chunks,
      trailer_sha256: verification.trailer_sha256,
      r2_etag: after.etag,
    };
  }

  async commitJob(state, job, record, catalogLimit) {
    await this.sourceCell(job);
    // Reload before crossing the authority boundary. A lost acknowledgement
    // from an earlier storage put may already have committed this exact record.
    const persisted = await this.loadState();
    const existing = persisted.catalog.find(
      (entry) => entry.backup_id === job.backup_id,
    );
    if (existing) return persisted;
    if (
      !persisted.current_job ||
      persisted.current_job.backup_id !== job.backup_id
    ) {
      throw new Error("backup job lost durable ownership before commit");
    }
    const catalog = [
      record,
      ...persisted.catalog.filter(
        (entry) => entry.backup_id !== job.backup_id,
      ),
    ].slice(0, catalogLimit);
    const next = {
      ...persisted,
      revision: persisted.revision + 1,
      current_job: {
        ...persisted.current_job,
        status: "committed",
        committed_at: this.now().toISOString(),
      },
      catalog,
    };
    await this.saveState(next);
    if (typeof this.storage.deleteAlarm === "function") {
      await this.storage.deleteAlarm().catch(() => {});
    }
    return next;
  }

  async failJob(state, job, error, retryable) {
    const terminal = !retryable || job.attempts >= job.max_attempts;
    const failed = {
      ...job,
      status: terminal ? "failed" : "retrying",
      last_error: boundedReason(error),
      ...(terminal
        ? { failed_at: this.now().toISOString() }
        : {
            retry_at: new Date(
              this.now().getTime() + retryDelay(job.attempts),
            ).toISOString(),
          }),
    };
    const next = {
      ...state,
      revision: state.revision + 1,
      current_job: failed,
      failures: terminal
        ? [failed, ...state.failures].slice(0, 16)
        : state.failures,
    };
    await this.saveState(next);
    if (terminal) {
      if (typeof this.storage.deleteAlarm === "function") {
        await this.storage.deleteAlarm().catch(() => {});
      }
    } else {
      await this.scheduleRetry(failed);
    }
    return next;
  }

  async scheduleRetry(job) {
    if (typeof this.storage.setAlarm !== "function") return;
    const retryAt = validDate(job.retry_at)
      ? Date.parse(job.retry_at)
      : this.now().getTime() + retryDelay(job.attempts);
    await this.storage.setAlarm(retryAt);
  }

  async alarm() {
    try {
      return await this.fence.run(async () => {
        const state = await this.loadState();
        if (
          !state.current_job ||
          !["pending", "running", "retrying"].includes(
            state.current_job.status,
          )
        ) {
          if (typeof this.storage.deleteAlarm === "function") {
            await this.storage.deleteAlarm().catch(() => {});
          }
          return;
        }
        // Rearm before I/O. If the invocation is terminated, the persisted job
        // remains visible and another alarm resumes it within the retry bound.
        await this.scheduleRetry(state.current_job);
        return this.execute(
          boundedInteger(
            this.env.CP_ACCOUNT_BACKUPS_CATALOG_LIMIT,
            DEFAULT_CATALOG_LIMIT,
            1,
            256,
          ),
        );
      });
    } catch (error) {
      if (!(error instanceof AccountLifecycleBusyError)) throw error;
      // A Durable Object alarm is consumed when its handler starts. The
      // alarm armed before outbound backup I/O can therefore fire while the
      // original request still owns the in-memory fence. Explicitly replace
      // that consumed alarm so a later invocation can recover the persisted
      // running job if the original request is terminated.
      const state = await this.loadState();
      if (
        state.current_job &&
        ["pending", "running", "retrying"].includes(
          state.current_job.status,
        ) &&
        typeof this.storage.setAlarm === "function"
      ) {
        const attempts = Math.max(1, state.current_job.attempts);
        await this.storage.setAlarm(
          this.now().getTime() + retryDelay(attempts),
        );
      }
    }
  }

  async recordValidation(input) {
    if (
      !isObject(input) ||
      input.account_id !== this.accountId ||
      !BACKUP_ID.test(input.backup_id ?? "") ||
      !CELL_NAME.test(input.target_cell ?? "") ||
      !validDate(input.validated_at) ||
      !["active", "suspended", "closed"].includes(input.status) ||
      !Number.isSafeInteger(input.archive_schema_version) ||
      input.archive_schema_version < 0
    ) {
      throw new Error("invalid backup validation receipt");
    }
    const state = await this.loadState();
    const index = state.catalog.findIndex(
      (record) => record.backup_id === input.backup_id,
    );
    if (index < 0) {
      throw new Error(
        "backup validation is not in the committed catalog",
      );
    }
    const catalog = clone(state.catalog);
    const record = catalog[index];
    if (
      input.status !== record.status ||
      input.archive_schema_version !== record.archive_schema_version
    ) {
      throw new Error(
        "backup validation receipt does not match the committed catalog",
      );
    }
    const validations = [
      {
        target_cell: input.target_cell,
        validated_at: input.validated_at,
        status: input.status,
        archive_schema_version: input.archive_schema_version,
      },
      ...(record.validations ?? []).filter(
        (validation) =>
          validation.target_cell !== input.target_cell ||
          validation.validated_at !== input.validated_at,
      ),
    ].slice(0, 16);
    catalog[index] = { ...record, validations };
    const next = {
      ...state,
      revision: state.revision + 1,
      catalog,
    };
    await this.saveState(next);
    return catalog[index];
  }
}

async function mapBounded(values, concurrency, operation) {
  const results = new Array(values.length);
  let next = 0;
  const workers = Array.from(
    { length: Math.min(concurrency, values.length) },
    async () => {
      while (true) {
        const index = next++;
        if (index >= values.length) return;
        results[index] = await operation(values[index], index);
      }
    },
  );
  await Promise.all(workers);
  return results;
}

function validScanState(state, slot) {
  return isObject(state) &&
    state.schema_version === ACCOUNT_BACKUP_SCAN_SCHEMA &&
    state.slot === slot &&
    (
      state.cursor === null ||
      (
        typeof state.cursor === "string" &&
        state.cursor.length >= 1 &&
        state.cursor.length <= 2048
      )
    ) &&
    typeof state.complete === "boolean" &&
    Number.isSafeInteger(state.scanned) &&
    state.scanned >= 0 &&
    Number.isSafeInteger(state.accepted) &&
    state.accepted >= 0 &&
    Number.isSafeInteger(state.failed) &&
    state.failed >= 0 &&
    state.scanned === state.accepted + state.failed &&
    state.failed_retry === SCAN_FAILED_RETRY_POLICY &&
    Array.isArray(state.failure_sample) &&
    state.failure_sample.length <= SCAN_FAILURE_SAMPLE_LIMIT &&
    state.failure_sample.every(
      (failure) =>
        isObject(failure) &&
        ACCOUNT_ID.test(failure.account_id ?? "") &&
        SCAN_FAILURE_STATUS.test(failure.status ?? ""),
    );
}

async function dispatchBackup(env, job, config) {
  const id = env.ACCOUNT_BACKUP.idFromName(job.account_id);
  const response = await env.ACCOUNT_BACKUP.get(id).fetch(
    new Request("http://account-backup.internal/run", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...job,
        max_attempts: config.max_attempts,
        catalog_limit: config.catalog_limit,
      }),
    }),
  );
  let body = null;
  try {
    body = await response.json();
  } catch {
    // Invalid acknowledgements are counted and retried on the next full scan.
  }
  const acknowledged = response.ok &&
    body?.schema_version === "witself.v0" &&
    body?.account_id === job.account_id &&
    body?.backup_id === job.backup_id &&
    ["committed", "retrying", "failed", "busy"].includes(body?.status);
  const accepted = acknowledged &&
    ["committed", "retrying"].includes(body?.status);
  return {
    account_id: job.account_id,
    acknowledged,
    accepted,
    status: acknowledged ? body.status : "dispatch_failed",
    body,
  };
}

async function backupStub(env, accountID) {
  if (!ACCOUNT_ID.test(accountID ?? "") || !env.ACCOUNT_BACKUP) {
    throw new Error("account backup binding or account id is invalid");
  }
  const id = env.ACCOUNT_BACKUP.idFromName(accountID);
  return env.ACCOUNT_BACKUP.get(id);
}

export async function accountBackupStatus(env, accountID = undefined) {
  const config = accountBackupConfig(env);
  const scan = env.DIRECTORY
    ? await env.DIRECTORY.get(ACCOUNT_BACKUP_SCAN_KEY, { type: "json" })
    : null;
  if (accountID === undefined || accountID === null || accountID === "") {
    return {
      schema_version: "witself.v0",
      schedule: config,
      scan: isObject(scan) ? scan : null,
    };
  }
  if (!ACCOUNT_ID.test(accountID)) {
    throw new Error("invalid backup status account id");
  }
  const response = await (await backupStub(env, accountID)).fetch(
    new Request("http://account-backup.internal/status"),
  );
  const body = await response.json().catch(() => null);
  if (
    !response.ok ||
    body?.schema_version !== "witself.v0" ||
    body?.account_id !== accountID
  ) {
    throw new Error("account backup status is unavailable");
  }
  return {
    schema_version: "witself.v0",
    schedule: config,
    scan: isObject(scan) ? scan : null,
    account: body,
  };
}

export async function runManualAccountBackup(
  env,
  accountID,
  scheduledTime = Date.now(),
) {
  if (
    !env.DIRECTORY ||
    !env.ACCOUNT_BACKUP ||
    !env.BACKUPS ||
    !ACCOUNT_ID.test(accountID ?? "")
  ) {
    throw new Error("manual account backup configuration is incomplete");
  }
  const route = await env.DIRECTORY.get(
    `acct:${accountID}`,
    { type: "json" },
  );
  if (!CELL_NAME.test(route?.cell ?? "")) {
    throw new Error("manual backup requires an active account route");
  }
  const config = accountBackupConfig(env);
  const job = backupJobIdentity(accountID, scheduledTime, 1);
  const result = await dispatchBackup(env, job, config);
  if (!result.acknowledged) {
    throw new Error("manual account backup was not acknowledged");
  }
  return result.body;
}

async function targetHasLiveProjection(env, targetCell) {
  let cursor;
  do {
    const page = await env.DIRECTORY.list({
      prefix: "acct:",
      limit: 1000,
      ...(cursor ? { cursor } : {}),
    });
    const routes = await Promise.all(
      page.keys.map((key) =>
        env.DIRECTORY.get(key.name, { type: "json" })
      ),
    );
    if (routes.some((route) => route?.cell === targetCell)) {
      return true;
    }
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return false;
}

async function authoritativeValidationCell(
  env,
  targetCell,
  projectedRegistrationID,
) {
  if (!env.CELL_COORDINATOR) {
    throw new Error(
      "backup validation target coordinator is unavailable",
    );
  }
  const id = env.CELL_COORDINATOR.idFromName(targetCell);
  let response;
  try {
    response = await env.CELL_COORDINATOR.get(id).fetch(
      new Request(
        "https://target-cell.internal/registration-status",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            cell_name: targetCell,
            registration_id: projectedRegistrationID,
          }),
        },
      ),
    );
  } catch (error) {
    throw new Error(
      `backup validation target coordinator is unavailable: ${
        boundedReason(error)
      }`,
    );
  }
  const body = await response.json().catch(() => null);
  if (
    !response.ok ||
    body?.ok !== true ||
    body?.cell_name !== targetCell ||
    body?.expected_registration_id !== projectedRegistrationID ||
    body?.registration_status !== "active" ||
    body?.current_registration_id !== projectedRegistrationID ||
    !isObject(body?.active_cell)
  ) {
    throw new Error(
      "backup validation target authoritative registration changed",
    );
  }
  return body.active_cell;
}

async function validationTargetSnapshot(env, input, expected = null) {
  const projected = await env.DIRECTORY.get(
    `cell:${input.target_cell}`,
    { type: "json" },
  );
  const projectedRegistrationID =
    projected?.registration_id ?? projected?.registered_at ?? null;
  if (
    typeof projectedRegistrationID !== "string" ||
    projectedRegistrationID.length < 1 ||
    projectedRegistrationID.length > 128
  ) {
    throw new Error(
      "backup validation target has no projected registration fence",
    );
  }
  // DIRECTORY is only an eventually-consistent projection. Resolve the
  // current registration and marker from the per-cell Durable Object before
  // every validation boundary so stale KV cannot authorize an unmarked or
  // reopened target.
  const target = await authoritativeValidationCell(
    env,
    input.target_cell,
    projectedRegistrationID,
  );
  const endpoint = validCellEndpoint(target?.endpoint);
  const registrationID =
    target?.registration_id ?? target?.registered_at ?? null;
  if (
    !endpoint ||
    target.accepting !== false ||
    target.backup_validation_target !== true ||
    typeof registrationID !== "string" ||
    registrationID.length < 1 ||
    registrationID.length > 128 ||
    !validDate(target?.registered_at) ||
    typeof target.backup_token !== "string" ||
    target.backup_token.length === 0 ||
    (
      typeof target.provision_token === "string" &&
      target.provision_token.length > 0 &&
      target.backup_token === target.provision_token
    )
  ) {
    throw new Error(
      "backup validation target must be a registered backup_validation_target=true, accepting=false cell with a distinct backup token",
    );
  }
  if (
    expected &&
    (
      endpoint !== expected.endpoint ||
      registrationID !== expected.registration_id ||
      target.registered_at !== expected.registered_at ||
      target.backup_token !== expected.backup_token
    )
  ) {
    throw new Error(
      "backup validation target registration changed before receipt",
    );
  }
  const route = await env.DIRECTORY.get(
    `acct:${input.account_id}`,
    { type: "json" },
  );
  if (route?.cell === input.target_cell) {
    throw new Error(
      "backup validation target is the account's live cell",
    );
  }
  if (await targetHasLiveProjection(env, input.target_cell)) {
    throw new Error(
      "backup validation target still has live account projections",
    );
  }
  return {
    endpoint,
    registration_id: registrationID,
    registered_at: target.registered_at,
    backup_token: target.backup_token,
    backup_validation_target: true,
  };
}

function exactValidationAck(body, record) {
  return isObject(body) &&
    body.schema_version === "witself.v0" &&
    body.account_id === record.account_id &&
    body.backup_id === record.backup_id &&
    body.purpose === "backup" &&
    body.validated === true &&
    body.status === record.status &&
    body.archive_schema_version === record.archive_schema_version;
}

export async function runAccountBackupValidation(
  env,
  input,
  dependencies = {},
) {
  if (
    !isObject(input) ||
    !ACCOUNT_ID.test(input.account_id ?? "") ||
    !BACKUP_ID.test(input.backup_id ?? "") ||
    !CELL_NAME.test(input.target_cell ?? "") ||
    !env.DIRECTORY ||
    !env.ACCOUNT_BACKUP ||
    !env.BACKUPS ||
    !env.CELL_COORDINATOR
  ) {
    throw new Error("invalid backup validation request");
  }
  const target = await validationTargetSnapshot(env, input);

  const status = await accountBackupStatus(env, input.account_id);
  const record = status.account?.backups?.catalog?.find(
    (candidate) => candidate.backup_id === input.backup_id,
  );
  if (!record || !validCatalogRecord(record, input.account_id)) {
    throw new Error(
      "backup validation requires a committed catalog backup",
    );
  }

  const validate =
    dependencies.validateArchive ?? validateCommittedAccountArchive;
  if (
    typeof env.BACKUPS.head !== "function" ||
    typeof env.BACKUPS.get !== "function"
  ) {
    throw new ArchiveIntegrityError(
      "backup validation cannot pin the R2 object identity",
    );
  }
  const before = assertR2ObjectIdentity(
    record,
    await env.BACKUPS.head(record.object),
    record.size,
    record.r2_etag,
  );
  const verification = await validate(
    env.BACKUPS,
    record.object,
    input.account_id,
  );
  if (
    verification?.manifest?.account_id !== input.account_id ||
    verification?.manifest?.backup_id !== input.backup_id ||
    verification?.manifest?.purpose !== "backup" ||
    verification?.manifest?.cell !== record.source_cell ||
    verification?.manifest?.status !== record.status ||
    verification?.manifest?.schema_version !==
      record.archive_schema_version ||
    (
      verification?.manifest?.evacuation_id !== undefined &&
      verification.manifest.evacuation_id !== null &&
      verification.manifest.evacuation_id !== ""
    ) ||
    verification?.entries !== record.entries ||
    verification?.chunks !== record.chunks ||
    verification?.trailer_sha256 !== record.trailer_sha256
  ) {
    throw new ArchiveIntegrityError(
      "backup validation reread does not match the committed catalog",
    );
  }
  assertR2ObjectIdentity(
    record,
    await env.BACKUPS.head(record.object),
    before.size,
    before.etag,
  );
  const object = await env.BACKUPS.get(record.object, {
    onlyIf: { etagMatches: record.r2_etag },
  });
  if (!object?.body) {
    throw new ArchiveIntegrityError(
      "backup validation object changed before its conditioned read",
    );
  }
  assertR2ObjectIdentity(
    record,
    await env.BACKUPS.head(record.object),
    before.size,
    before.etag,
  );

  const fetchImpl =
    dependencies.fetch ?? ((...args) => globalThis.fetch(...args));
  const response = await fetchImpl(
    `${target.endpoint}/v1/accounts/${input.account_id}:validate-backup`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${target.backup_token}`,
        "Content-Type": "application/octet-stream",
        "X-Witself-Backup-ID": input.backup_id,
        "X-Witself-Backup-Validation": "true",
      },
      body: object.body,
      signal: AbortSignal.timeout(EXPORT_TIMEOUT_MS),
    },
  );
  const text = await response.text().catch(() => "");
  let acknowledgement = null;
  try {
    acknowledgement = JSON.parse(text);
  } catch {
    // A generic 2xx is never accepted as proof of rollback-only validation.
  }
  if (!response.ok || !exactValidationAck(acknowledgement, record)) {
    throw new Error(
      `backup validation ${response.status}: ${
        text.slice(0, 200) || "missing exact acknowledgement"
      }`,
    );
  }

  // The remote validation may take long enough for the target cell to be
  // re-registered, undrained, or receive a live account. Never attach the
  // receipt to the catalog unless the isolated target fence is still exact.
  await validationTargetSnapshot(env, input, target);

  const receipt = {
    account_id: input.account_id,
    backup_id: input.backup_id,
    target_cell: input.target_cell,
    validated_at: (
      dependencies.now ?? (() => new Date())
    )().toISOString(),
    status: acknowledgement.status,
    archive_schema_version: acknowledgement.archive_schema_version,
  };
  const receiptResponse = await (await backupStub(
    env,
    input.account_id,
  )).fetch(
    new Request("http://account-backup.internal/validation-verified", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(receipt),
    }),
  );
  if (!receiptResponse.ok) {
    throw new Error(
      "backup validation completed but its receipt was not persisted",
    );
  }
  return {
    schema_version: "witself.v0",
    validated: true,
    ...receipt,
  };
}

/**
 * One cron tick processes one bounded directory page. The slot and cursor are
 * durable in KV, so a large fleet advances over several ticks without issuing
 * more than one deterministic backup job per account and interval.
 */
export async function runScheduledAccountBackups(
  env,
  scheduledTime = Date.now(),
  dependencies = {},
) {
  const config = accountBackupConfig(env);
  if (!config.enabled) {
    return { ran: false, configured: true };
  }
  if (
    !env.DIRECTORY ||
    !env.ACCOUNT_BACKUP ||
    !env.BACKUPS
  ) {
    console.log("account-backup: scheduled tick configuration is incomplete");
    return { ran: false, configured: false };
  }

  const identity = backupJobIdentity(
    "slot",
    scheduledTime,
    config.interval_minutes,
  );
  const slot = identity.scheduled_at;
  const pageLoader = dependencies.activeAccountPage ?? activeAccountPage;
  const dispatcher = dependencies.dispatch ?? dispatchBackup;

  try {
    const stored = await env.DIRECTORY.get(
      ACCOUNT_BACKUP_SCAN_KEY,
      { type: "json" },
    );
    const scan = validScanState(stored, slot)
      ? stored
      : {
          schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA,
          slot,
          cursor: null,
          complete: false,
          scanned: 0,
          accepted: 0,
          failed: 0,
          failed_retry: SCAN_FAILED_RETRY_POLICY,
          failure_sample: [],
        };
    if (scan.complete) {
      return {
        ran: false,
        configured: true,
        slot,
        complete: true,
        scanned: scan.scanned,
        accepted: scan.accepted,
        failed: scan.failed,
        failed_retry: scan.failed_retry,
        failure_sample: scan.failure_sample,
      };
    }

    const cursor = scan.cursor ?? undefined;
    const page = await pageLoader(env, config.page_size, cursor);
    const jobs = page.account_ids.map(
      (accountID) =>
        backupJobIdentity(
          accountID,
          scheduledTime,
          config.interval_minutes,
        ),
    );
    const results = await mapBounded(
      jobs,
      config.concurrency,
      (job) => dispatcher(env, job, config),
    );
    const pageAccepted = results.filter(
      (result) => result.accepted,
    ).length;
    const pageFailed = results.length - pageAccepted;
    const scanned = scan.scanned + jobs.length;
    const accepted = scan.accepted + pageAccepted;
    const failed = scan.failed + pageFailed;
    const failureSample = [
      ...scan.failure_sample,
      ...results.flatMap((result, index) =>
        result.accepted
          ? []
          : [{
              // Account ids come from the validated active-account page, not
              // from an untrusted acknowledgement body.
              account_id: jobs[index].account_id,
              status: sanitizedScanFailureStatus(result.status),
            }]
      ),
    ].slice(0, SCAN_FAILURE_SAMPLE_LIMIT);
    const complete = page.next_cursor === null;
    const nextScan = {
      schema_version: ACCOUNT_BACKUP_SCAN_SCHEMA,
      slot,
      cursor: page.next_cursor,
      complete,
      updated_at: new Date(
        Number(scheduledTime),
      ).toISOString(),
      scanned,
      accepted,
      failed,
      failed_retry: SCAN_FAILED_RETRY_POLICY,
      failure_sample: failureSample,
    };
    await env.DIRECTORY.put(
      ACCOUNT_BACKUP_SCAN_KEY,
      JSON.stringify(nextScan),
    );
    console.log(
      "account-backup: scheduled tick " +
      `slot=${slot} page_scanned=${jobs.length} ` +
      `page_accepted=${pageAccepted} page_failed=${pageFailed} ` +
      `scanned=${scanned} accepted=${accepted} failed=${failed} ` +
      `complete=${complete}`,
    );
    return {
      ran: true,
      configured: true,
      slot,
      complete,
      scanned,
      accepted,
      failed,
      failed_retry: SCAN_FAILED_RETRY_POLICY,
      failure_sample: failureSample,
    };
  } catch (error) {
    console.log(
      `account-backup: scheduled tick unavailable: ${boundedReason(error)}`,
    );
    return { ran: true, configured: true, succeeded: false };
  }
}
