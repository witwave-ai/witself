import {
  ACCOUNT_BACKUP_SCAN_KEY,
  BackupValidationBusyError,
  RESTORE_DRILL_FRESHNESS_KEY,
  RestoreDrillProtocolError,
  RestoreDrillUnconfirmedError,
  accountBackupConfig,
  backupJobIdentity,
  beginAccountBackupValidation,
  readBackupStatus,
  restoreDrillFreshnessSnapshot,
  validCatalogRecord,
} from "./account-backup-runtime.mjs";

export const RESTORE_DRILL_SCHEDULE_KEY = "config:restore_drill_schedule";
export const RESTORE_DRILL_SCHEDULE_SCHEMA = "witself.restore-drill-schedule.v1";
// This operational fence retains rotation and drill identities only for the
// authenticated fleet status route. Logs and public freshness stay value-free.
const WINDOW_HOURS = 4;
const CANDIDATES_PER_TICK = 3;
const MAX_DIRECTORY_LISTS = 10;
const HISTORY_LIMIT = 8;
const OUTCOME_RETRY_MS = 1_100;
const IO_TIMEOUT_MS = 5_000;
const ACCOUNT_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const BACKUP_ID = /^backup_[0-9]{8}T[0-9]{6}Z$/;
const DRILL_ID = /^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/;
const STATES = ["pending", "starting", "started", "start_failed", "start_unconfirmed", "no_eligible_account"];
const FINAL_STATES = ["started", "start_failed", "start_unconfirmed", "no_eligible_account"];
const REASONS = [null, "backup_scan_incomplete", "freshness_unavailable", "drill_running", "target_unavailable", "candidates_exhausted_this_tick", "window_closed"];
// Operational rotation state contains ids and is exposed only by the fleet-
// authenticated status route. Never copy it into logs or the freshness summary.
const RECORD_KEYS = ["schema_version", "window_start", "state", "reason", "rotation_after", "examined_after", "account_id", "backup_id", "drill_id", "started_at", "error", "history", "updated_at"];

const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const exactKeys = (value, keys) => isObject(value) && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
const validDate = (value) => typeof value === "string" && Number.isFinite(Date.parse(value));
const nullable = (value, pattern) => value === null || (typeof value === "string" && pattern.test(value));

function validSchedule(record) {
  return exactKeys(record, RECORD_KEYS) &&
    record.schema_version === RESTORE_DRILL_SCHEDULE_SCHEMA &&
    validDate(record.window_start) && STATES.includes(record.state) &&
    REASONS.includes(record.reason) &&
    [record.rotation_after, record.examined_after, record.account_id].every((id) => nullable(id, ACCOUNT_ID)) &&
    nullable(record.backup_id, BACKUP_ID) && nullable(record.drill_id, DRILL_ID) &&
    (record.started_at === null || validDate(record.started_at)) &&
    (record.error === null || (typeof record.error === "string" && /^[\x20-\x7e]{0,300}$/.test(record.error))) &&
    Array.isArray(record.history) && record.history.length <= HISTORY_LIMIT &&
    record.history.every((entry) => exactKeys(entry, ["window_start", "state", "drill_id"]) &&
      validDate(entry.window_start) && STATES.includes(entry.state) && nullable(entry.drill_id, DRILL_ID)) &&
    validDate(record.updated_at);
}

function parseRecord(text) {
  if (text === null || text === undefined) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

// Bound reads separately so a slow binding cannot hold a guard or status route.
// The callback also captures synchronous binding errors in the returned promise.
async function bounded(operation) {
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(operation),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error("restore drill schedule I/O timed out")), IO_TIMEOUT_MS);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

export async function readRestoreDrillSchedule(env) {
  try {
    return parseRecord(await bounded(() => env.DIRECTORY.get(RESTORE_DRILL_SCHEDULE_KEY, { type: "text" })));
  } catch {
    return null;
  }
}

export function restoreDrillScheduleConfig(env) {
  if (String(env.CP_RESTORE_DRILLS_ENABLED ?? "").trim().toLowerCase() !== "true") {
    return { enabled: false };
  }
  const target_cell = String(env.CP_RESTORE_DRILLS_TARGET_CELL ?? "").trim();
  const weekday = String(env.CP_RESTORE_DRILLS_WEEKDAY ?? "").trim() || "3";
  const start_hour = String(env.CP_RESTORE_DRILLS_START_HOUR_UTC ?? "").trim() || "16";
  const origin = String(env.CP_PUBLIC_ORIGIN ?? "").trim();
  let validOrigin = false;
  try {
    const parsed = new URL(origin);
    validOrigin = parsed.protocol === "https:" && parsed.origin === origin;
  } catch {
    // A cron has no request origin to use as a fallback.
  }
  if (!CELL_NAME.test(target_cell) || !/^[0-6]$/.test(weekday) || !/^(?:[1-9]|1[0-8])$/.test(start_hour) || !validOrigin) {
    return { enabled: true, valid: false };
  }
  return { enabled: true, valid: true, target_cell, weekday: Number(weekday), start_hour: Number(start_hour), origin };
}

async function lifecycleStatus(env, accountID) {
  const response = await env.ACCOUNT_LIFECYCLE.get(env.ACCOUNT_LIFECYCLE.idFromName(accountID)).fetch(
    new Request("https://account-lifecycle.internal/lifecycle-status", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ account_id: accountID }),
    }),
  );
  const body = await response.json();
  if (!response.ok || body?.ok !== true || body.account_id !== accountID ||
      !isObject(body.status) || body.status.account_id !== accountID || typeof body.status.initialized !== "boolean") {
    throw new Error("account lifecycle status is unavailable");
  }
  return body.status;
}

function newWindow(window_start, previous, now) {
  return {
    schema_version: RESTORE_DRILL_SCHEDULE_SCHEMA,
    window_start,
    state: "pending",
    reason: null,
    rotation_after: previous?.rotation_after ?? null,
    examined_after: null,
    account_id: null,
    backup_id: null,
    drill_id: null,
    started_at: null,
    error: null,
    history: previous ? [{ window_start: previous.window_start, state: previous.state, drill_id: previous.drill_id }, ...previous.history].slice(0, HISTORY_LIMIT) : [],
    updated_at: now,
  };
}

function compareAccounts(a, b, anchor) {
  const aGroup = anchor === null || a > anchor ? 0 : 1;
  const bGroup = anchor === null || b > anchor ? 0 : 1;
  return aGroup - bGroup || (a < b ? -1 : a > b ? 1 : 0);
}

async function listCandidates(directory, record) {
  const ids = new Set();
  let cursor;
  for (let page = 0; page < MAX_DIRECTORY_LISTS; page++) {
    const result = await bounded(() => directory.list({ prefix: "acct:", limit: 1000, cursor }));
    for (const key of result.keys) {
      const id = typeof key?.name === "string" && key.name.startsWith("acct:") ? key.name.slice(5) : "";
      if (ACCOUNT_ID.test(id)) ids.add(id);
    }
    if (result.list_complete) break;
    cursor = result.cursor;
    if (page === MAX_DIRECTORY_LISTS - 1) console.log("restore-drill-schedule: directory listing capped");
  }
  return [...ids].sort((a, b) => compareAccounts(a, b, record.rotation_after))
    .filter((id) => record.examined_after === null || compareAccounts(id, record.examined_after, record.rotation_after) > 0);
}

async function eligibleBackup(env, accountID, config, dependencies) {
  try {
    const results = await Promise.allSettled([
      () => env.DIRECTORY.get(`acct:${accountID}`, { type: "json" }),
      () => env.DIRECTORY.get(`pending:${accountID}`, { type: "text" }),
      () => env.DIRECTORY.get(`archived:${accountID}`, { type: "text" }),
      () => (dependencies.lifecycleStatus ?? lifecycleStatus)(env, accountID),
      () => (dependencies.backupStatus ?? readBackupStatus)(env, accountID),
    ].map(bounded));
    // Per-account state can see a running drill before the KV summary does.
    // Hold the window even if this candidate would otherwise be ineligible.
    if (results[4].value?.restore_drill?.state === "running") return { running: true };
    if (results.some((result) => result.status === "rejected")) return null;
    const [route, pending, archived, lifecycle, status] = results.map((result) => result.value);
    if (!isObject(route) || !CELL_NAME.test(route.cell ?? "") || route.cell === config.target_cell ||
        pending !== null && pending !== undefined || archived !== null && archived !== undefined ||
        lifecycle?.operation !== null || !isObject(status?.backups) ||
        ["pending", "running", "retrying"].includes(status.backups.current_job?.status)) return null;
    const catalog = Array.isArray(status.backups.catalog) ? status.backups.catalog.filter((entry) => validCatalogRecord(entry, accountID)) : [];
    const backup_id = catalog.map((entry) => entry.backup_id).sort().at(-1);
    return backup_id ? { backup_id } : null;
  } catch {
    return null;
  }
}

export function chainRestoreDrill(backupTick, drillTick) {
  const continueWith = async (value) => {
    await drillTick(value);
    return value;
  };
  // A rejected backup tick must still run the holding drill tick, and must not
  // escape the maintenance waitUntil promise or expose arbitrary error text.
  return backupTick.then(continueWith, () => continueWith({ succeeded: false }));
}

export async function runScheduledRestoreDrill(env, scheduledTime = Date.now(), dependencies = {}) {
  try {
    const config = restoreDrillScheduleConfig(env);
    if (!config.enabled) return { ran: false, enabled: false };
    if (!config.valid) {
      console.log("restore-drill-schedule: skipped reason=invalid_config");
      return { ran: false, reason: "invalid_config" };
    }
    const time = new Date(scheduledTime);
    if (time.getUTCDay() !== config.weekday || time.getUTCHours() < config.start_hour || time.getUTCHours() >= config.start_hour + WINDOW_HOURS) {
      return { ran: false, reason: "outside_window" };
    }
    if (!env.DIRECTORY || !env.ACCOUNT_BACKUP || !env.ACCOUNT_LIFECYCLE || !env.BACKUPS || !env.CELL_COORDINATOR) {
      console.log("restore-drill-schedule: skipped reason=invalid_config");
      return { ran: false, reason: "invalid_config" };
    }
    const now = time.toISOString();
    time.setUTCHours(config.start_hour, 0, 0, 0);
    const window_start = time.toISOString();
    const raw = await bounded(() => env.DIRECTORY.get(RESTORE_DRILL_SCHEDULE_KEY, { type: "text" }));
    const parsed = parseRecord(raw);
    const previous = validSchedule(parsed) ? parsed : null;
    // Refuse a clock rollback rather than replacing a later window's fence.
    if (previous && Date.parse(previous.window_start) > Date.parse(window_start)) throw new Error("schedule window is ahead");
    const record = previous && Date.parse(previous.window_start) === Date.parse(window_start)
      ? previous : newWindow(window_start, previous, now);
    const logState = () => console.log(`restore-drill-schedule: state=${record.state} reason=${record.reason ?? "none"}`);
    const put = () => {
      record.updated_at = now;
      const value = JSON.stringify(record);
      // Await the platform write itself. Timing out a write cannot cancel it;
      // retrying that ambiguous write could add a third successful put.
      return env.DIRECTORY.put(RESTORE_DRILL_SCHEDULE_KEY, value);
    };
    const finish = async (state, reason = null) => {
      record.state = state;
      record.reason = reason;
      await put();
      logState();
      return { ran: true, state, reason };
    };
    if (FINAL_STATES.includes(record.state)) {
      logState();
      return { ran: true, state: record.state, reason: record.reason };
    }
    if (record.state === "starting") return await finish("start_unconfirmed");

    const backupConfig = accountBackupConfig(env);
    const backupsEnabled = backupConfig.enabled;
    const [scan, freshness] = await Promise.all([
      backupsEnabled ? bounded(() => env.DIRECTORY.get(ACCOUNT_BACKUP_SCAN_KEY, { type: "json" })).catch(() => null) : null,
      bounded(() => env.DIRECTORY.get(RESTORE_DRILL_FRESHNESS_KEY, { type: "text" }))
        .then((text) => ({ text, available: true }), () => ({ available: false })),
    ]);
    if (dependencies.backupTickResult?.succeeded === false ||
        backupsEnabled && (!isObject(scan) || scan.complete !== true ||
          (scan.pass_started_slot ?? scan.slot) !== backupJobIdentity("slot", scheduledTime, backupConfig.interval_minutes).scheduled_at)) {
      return await finish("pending", "backup_scan_incomplete");
    }
    const snapshot = restoreDrillFreshnessSnapshot(parseRecord(freshness.text), scheduledTime);
    if (!freshness.available || freshness.text !== null && freshness.text !== undefined && snapshot === null) return await finish("pending", "freshness_unavailable");
    if (snapshot?.running > 0) return await finish("pending", "drill_running");

    const candidates = await listCandidates(env.DIRECTORY, record);
    for (const account_id of candidates.slice(0, CANDIDATES_PER_TICK)) {
      const candidate = await eligibleBackup(env, account_id, config, dependencies);
      if (candidate?.running) return await finish("pending", "drill_running");
      if (candidate === null) {
        record.examined_after = account_id;
        continue;
      }
      const { backup_id } = candidate;
      const priorRotation = record.rotation_after;
      Object.assign(record, { state: "starting", reason: null, rotation_after: account_id, account_id, backup_id, drill_id: null, started_at: null, error: null });
      await put();
      const startingRecord = JSON.stringify(record);
      let outcome;
      try {
        if ((dependencies.now?.() ?? Date.now()) >= Date.parse(window_start) + WINDOW_HOURS * 60 * 60_000) {
          outcome = { state: "pending", reason: "window_closed", rotation_after: priorRotation };
        } else {
          const result = await (dependencies.begin ?? beginAccountBackupValidation)(env,
            { account_id, backup_id, target_cell: config.target_cell },
            { origin: config.origin, driver: "auto", requireAlarm: true });
          if (!DRILL_ID.test(result?.drill_id ?? "") || !validDate(result?.restore_drill?.started_at)) throw new Error("invalid drill start result");
          outcome = { state: "started", drill_id: result.drill_id, started_at: result.restore_drill.started_at };
        }
      } catch (error) {
        if (error instanceof BackupValidationBusyError) {
          outcome = { state: "pending", reason: "drill_running", rotation_after: priorRotation };
        } else if (error instanceof RestoreDrillProtocolError) {
          outcome = { state: "pending", reason: "target_unavailable", rotation_after: priorRotation };
        } else if (error instanceof RestoreDrillUnconfirmedError) {
          outcome = { state: "start_unconfirmed", drill_id: DRILL_ID.test(error.drill_id ?? "") ? error.drill_id : null };
        } else {
          // Never persist arbitrary exception text: it can carry account data.
          outcome = { state: "start_failed", error: "restore drill start failed" };
        }
      }
      const writeOutcome = async () => {
        const stored = parseRecord(await bounded(() => env.DIRECTORY.get(RESTORE_DRILL_SCHEDULE_KEY, { type: "text" })));
        if (validSchedule(stored)) {
          if (Date.parse(stored.window_start) > Date.parse(window_start)) throw new Error("schedule window is ahead");
          if (Date.parse(stored.window_start) === Date.parse(window_start)) {
            if (FINAL_STATES.includes(stored.state)) {
              console.log(`restore-drill-schedule: state=${stored.state} reason=kept`);
              return { ran: true, state: stored.state, reason: stored.reason };
            }
            // Undo only this tick's tentative starting rotation. Another tick's
            // observed progress wins over our stale prior rotation. KV has no
            // CAS: this narrows the race but cannot provide mutual exclusion.
            if (outcome.state === "pending" && JSON.stringify(stored) !== startingRecord) {
              outcome.rotation_after = stored.rotation_after;
            }
          }
        }
        Object.assign(record, outcome);
        await put();
        return null;
      };
      try {
        const kept = await writeOutcome();
        if (kept) return kept;
      } catch {
        // KV allows about one write per second per key. A fast refusal can
        // collide with the starting write, so retry the outcome once.
        await (dependencies.sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms))))(OUTCOME_RETRY_MS);
        try {
          const kept = await writeOutcome();
          if (kept) return kept;
        } catch {
          console.log("restore-drill-schedule: state=starting reason=outcome_not_recorded");
          return { ran: true, succeeded: false };
        }
      }
      logState();
      return { ran: true, state: record.state, reason: record.reason };
    }
    return await finish(candidates.length > CANDIDATES_PER_TICK ? "pending" : "no_eligible_account",
      candidates.length > CANDIDATES_PER_TICK ? "candidates_exhausted_this_tick" : null);
  } catch {
    console.log("restore-drill-schedule: tick failed");
    return { ran: true, succeeded: false };
  }
}
