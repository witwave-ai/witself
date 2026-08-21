import crypto from "node:crypto";
import path from "node:path";

import {
  CEILING_ENV,
  CLAUDE_HOOK_MODES,
  GRANT_FIELD,
  GRANT_KEY_FILE_ENV,
  GRANT_TTL_MS,
  canonicalJson,
  readGrantKey,
  sha256Hex,
} from "../../../.claude/codex-profiles.mjs";

import { BrokerError } from "./util.mjs";
import { MAX_STATUS_WAIT_SECONDS, MAX_TASK_CHARS } from "./constants.mjs";

const GRANT_KEYS = Object.freeze([
  "v", "ceiling", "tool", "mode", "tool_use_id", "session_id",
  "issued_at_ms", "nonce", "input_sha256", "mac",
].sort());
const MAX_FUTURE_SKEW_MS = 5_000;
const MAX_REPLAY_ENTRIES = 4096;
const HEX_256_RE = /^[0-9a-f]{64}$/;
const BASE64URL_MAC_RE = /^[A-Za-z0-9_-]{43}$/;
const TOKEN_RE = /^[A-Za-z0-9._:-]{1,256}$/;
const NONCE_RE = /^[A-Za-z0-9_-]{16,128}$/;
const JOB_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const ARTIFACT_IDS = new Set(["changes.patch", "evidence.bin"]);

function exactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) return false;
  const keys = Object.keys(value).sort();
  return keys.length === expected.length && keys.every((key, index) => key === expected[index]);
}

function safeString(value, pattern = TOKEN_RE) {
  return typeof value === "string" && pattern.test(value) && !/[\0\r\n]/u.test(value);
}

function safeSessionId(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 256 && !/[\0\r\n]/u.test(value);
}

function allowedModeForTool(tool, mode) {
  if (!CLAUDE_HOOK_MODES.includes(mode)) return false;
  if (tool === "codex_system_start") return mode === "bypassPermissions";
  if (tool === "codex_system_status" || tool === "codex_system_cancel") return true;
  if (tool === "codex_implementation_start") {
    return mode === "acceptEdits" || mode === "auto" || mode === "bypassPermissions";
  }
  if (tool === "codex_implementation_status" || tool === "codex_implementation_cancel" ||
      tool === "codex_implementation_artifact_read") return true;
  return false;
}

function requiredCeilingForTool(tool) {
  if (tool.startsWith("codex_system_")) return "system";
  if (tool.startsWith("codex_implementation_")) return "isolated-write";
  return null;
}

function ceilingRank(ceiling) {
  return { repository: 0, "isolated-write": 1, system: 2 }[ceiling] ?? -1;
}

function validateOriginalInput(tool, input) {
  if (tool === "codex_system_start") {
    return exactKeys(input, ["task"]) && typeof input.task === "string" && input.task.trim().length > 0 && input.task.length <= MAX_TASK_CHARS;
  }
  if (tool === "codex_system_status") {
    return (exactKeys(input, ["job_id"]) || exactKeys(input, ["job_id", "wait_seconds"])) &&
      typeof input.job_id === "string" && JOB_ID_RE.test(input.job_id) &&
      (!Object.hasOwn(input, "wait_seconds") ||
        (Number.isInteger(input.wait_seconds) && input.wait_seconds >= 0 && input.wait_seconds <= MAX_STATUS_WAIT_SECONDS));
  }
  if (tool === "codex_system_cancel") {
    return exactKeys(input, ["job_id"]) && typeof input.job_id === "string" && JOB_ID_RE.test(input.job_id);
  }
  if (tool === "codex_implementation_start") {
    return exactKeys(input, ["task"]) && typeof input.task === "string" && input.task.trim().length > 0 && input.task.length <= MAX_TASK_CHARS;
  }
  if (tool === "codex_implementation_status") {
    return (exactKeys(input, ["job_id"]) || exactKeys(input, ["job_id", "wait_seconds"])) &&
      typeof input.job_id === "string" && JOB_ID_RE.test(input.job_id) &&
      (!Object.hasOwn(input, "wait_seconds") ||
        (Number.isInteger(input.wait_seconds) && input.wait_seconds >= 0 && input.wait_seconds <= MAX_STATUS_WAIT_SECONDS));
  }
  if (tool === "codex_implementation_cancel") {
    return exactKeys(input, ["job_id"]) && typeof input.job_id === "string" && JOB_ID_RE.test(input.job_id);
  }
  if (tool === "codex_implementation_artifact_read") {
    const keys = Object.keys(input).sort();
    const allowed = new Set(["job_id", "artifact_id", "offset", "max_bytes"]);
    return keys.length >= 2 && keys.length <= 4 && keys.every((key) => allowed.has(key)) &&
      Object.hasOwn(input, "job_id") && typeof input.job_id === "string" && JOB_ID_RE.test(input.job_id) &&
      Object.hasOwn(input, "artifact_id") && ARTIFACT_IDS.has(input.artifact_id) &&
      (!Object.hasOwn(input, "offset") || (Number.isSafeInteger(input.offset) && input.offset >= 0)) &&
      (!Object.hasOwn(input, "max_bytes") || (Number.isSafeInteger(input.max_bytes) && input.max_bytes >= 1 && input.max_bytes <= 48 * 1024));
  }
  return false;
}

export function loadGrantAuthority({ ceiling, grantKeyFile, environment = process.env, clock = Date.now }) {
  const environmentCeiling = environment[CEILING_ENV];
  const environmentKeyFile = environment[GRANT_KEY_FILE_ENV];
  if (ceiling === "repository") {
    if (grantKeyFile !== null && grantKeyFile !== undefined) {
      throw new BrokerError("invalid_grant_configuration", "The repository ceiling must not receive an elevated grant key.");
    }
    if (environmentCeiling !== undefined && environmentCeiling !== "repository") {
      throw new BrokerError("ceiling_mismatch", "The launcher ceiling did not match the broker startup ceiling.");
    }
    if (environmentKeyFile !== undefined) {
      throw new BrokerError("invalid_grant_configuration", "The repository ceiling must not inherit an elevated grant key path.");
    }
    return Object.freeze({ verifier: null, launcherEnvironment: freezeLauncherEnvironment(environment) });
  }

  if (environmentCeiling !== ceiling) {
    throw new BrokerError("ceiling_mismatch", "The elevated launcher ceiling did not exactly match the broker startup ceiling.");
  }
  if (typeof grantKeyFile !== "string" || !path.isAbsolute(grantKeyFile) || environmentKeyFile !== grantKeyFile) {
    throw new BrokerError("invalid_grant_configuration", "The elevated launcher grant-key paths did not exactly match.");
  }
  let key;
  try { key = readGrantKey(grantKeyFile); }
  catch { throw new BrokerError("invalid_grant_key", "The elevated one-use grant key failed canonical private-file validation."); }
  try {
    return Object.freeze({
      verifier: new GrantVerifier({ ceiling, key, clock }),
      launcherEnvironment: freezeLauncherEnvironment(environment),
    });
  } finally {
    key.fill(0);
  }
}

export function freezeLauncherEnvironment(environment = process.env) {
  const snapshot = {};
  for (const [name, value] of Object.entries(environment)) {
    if (typeof value === "string") snapshot[name] = value;
  }
  delete snapshot[CEILING_ENV];
  delete snapshot[GRANT_KEY_FILE_ENV];
  return Object.freeze(snapshot);
}

export class GrantVerifier {
  constructor({ ceiling, key, clock = Date.now }) {
    if (!Buffer.isBuffer(key) || key.length !== 32 || ceilingRank(ceiling) < 1) {
      throw new BrokerError("invalid_grant_configuration", "The elevated grant verifier was configured incorrectly.");
    }
    Object.defineProperty(this, "ceiling", { value: ceiling, enumerable: true, writable: false });
    this.key = Buffer.from(key);
    this.clock = clock;
    this.consumed = new Map();
  }

  verifyAndConsume(tool, argumentsWithGrant) {
    const requiredCeiling = requiredCeilingForTool(tool);
    if (!requiredCeiling || ceilingRank(this.ceiling) < ceilingRank(requiredCeiling)) {
      throw new BrokerError("tool_above_ceiling", "The requested elevated Codex tool exceeds the immutable startup ceiling.");
    }
    if (!argumentsWithGrant || typeof argumentsWithGrant !== "object" || Array.isArray(argumentsWithGrant) ||
        !Object.hasOwn(argumentsWithGrant, GRANT_FIELD)) {
      throw new BrokerError("grant_required", "A fresh launcher-issued one-use grant is required for this elevated Codex call.");
    }
    const grant = argumentsWithGrant[GRANT_FIELD];
    if (!exactKeys(grant, GRANT_KEYS) || grant.v !== 1 || grant.ceiling !== this.ceiling || grant.tool !== tool ||
        !allowedModeForTool(tool, grant.mode) || !safeString(grant.tool_use_id) ||
        !(grant.session_id === null || safeSessionId(grant.session_id)) || !safeString(grant.nonce, NONCE_RE) ||
        !Number.isSafeInteger(grant.issued_at_ms) || grant.issued_at_ms <= 0 ||
        !safeString(grant.input_sha256, HEX_256_RE) || !safeString(grant.mac, BASE64URL_MAC_RE)) {
      throw new BrokerError("grant_invalid", "The elevated one-use grant was malformed or did not match the immutable policy.");
    }

    const originalInput = { ...argumentsWithGrant };
    delete originalInput[GRANT_FIELD];
    if (!validateOriginalInput(tool, originalInput)) {
      throw new BrokerError("grant_input_invalid", "The elevated one-use grant did not contain the exact allowed tool input.");
    }
    let inputHash;
    try { inputHash = sha256Hex(canonicalJson(originalInput)); }
    catch { throw new BrokerError("grant_invalid", "The elevated tool input could not be authenticated."); }
    if (inputHash !== grant.input_sha256) {
      throw new BrokerError("grant_input_mismatch", "The elevated one-use grant did not authenticate this exact tool input.");
    }

    const now = this.clock();
    if (!Number.isSafeInteger(now) || now <= 0 || grant.issued_at_ms > now + MAX_FUTURE_SKEW_MS ||
        now - grant.issued_at_ms > GRANT_TTL_MS) {
      throw new BrokerError("grant_expired", "The elevated one-use grant expired or had an invalid timestamp.");
    }
    const signingFields = {
      v: grant.v,
      ceiling: grant.ceiling,
      tool: grant.tool,
      mode: grant.mode,
      tool_use_id: grant.tool_use_id,
      session_id: grant.session_id,
      issued_at_ms: grant.issued_at_ms,
      nonce: grant.nonce,
      input_sha256: grant.input_sha256,
    };
    const expected = crypto.createHmac("sha256", this.key).update(canonicalJson(signingFields), "utf8").digest();
    let received;
    try { received = Buffer.from(grant.mac, "base64url"); } catch { received = Buffer.alloc(0); }
    if (received.length !== expected.length || !crypto.timingSafeEqual(received, expected)) {
      throw new BrokerError("grant_authentication_failed", "The elevated one-use grant failed authentication.");
    }

    for (const [replayKey, expiresAt] of this.consumed) {
      if (expiresAt < now - MAX_FUTURE_SKEW_MS) this.consumed.delete(replayKey);
    }
    const replayKeys = [`tool-use:${grant.tool_use_id}`, `nonce:${grant.nonce}`];
    if (replayKeys.some((key) => this.consumed.has(key))) {
      throw new BrokerError("grant_replayed", "The elevated one-use grant was already consumed.");
    }
    if (this.consumed.size > MAX_REPLAY_ENTRIES - replayKeys.length) {
      throw new BrokerError("grant_cache_full", "The elevated grant replay cache reached its fail-closed bound; restart the launcher.");
    }
    for (const key of replayKeys) this.consumed.set(key, grant.issued_at_ms + GRANT_TTL_MS);
    return Object.freeze(originalInput);
  }

  destroy() {
    this.key.fill(0);
    this.consumed.clear();
  }
}

export { GRANT_FIELD };
