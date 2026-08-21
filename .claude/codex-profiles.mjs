import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const BROKER_SERVER_NAME = "codex-local";
export const CEILING_ENV = "WITSELF_CODEX_CEILING";
export const GRANT_KEY_FILE_ENV = "WITSELF_CODEX_GRANT_KEY_FILE";
export const GRANT_FIELD = "_codex_grant";
export const GRANT_TTL_MS = 30_000;

export const REVIEW_TOOLS = Object.freeze([
  "codex_runtime_probe",
  "codex_review_start",
  "codex_review_status",
  "codex_review_cancel",
]);

export const IMPLEMENTATION_TOOLS = Object.freeze([
  "codex_implementation_start",
  "codex_implementation_status",
  "codex_implementation_artifact_read",
  "codex_implementation_cancel",
]);

export const SYSTEM_TOOLS = Object.freeze([
  "codex_system_start",
  "codex_system_status",
  "codex_system_cancel",
]);

const PROFILE_DATA = {
  repository: {
    rank: 0,
    defaultMode: "plan",
    allowedModes: ["plan", "manual"],
    tools: REVIEW_TOOLS,
  },
  "isolated-write": {
    rank: 1,
    defaultMode: "acceptEdits",
    allowedModes: ["acceptEdits", "auto", "bypassPermissions"],
    tools: [...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS],
  },
  system: {
    rank: 2,
    defaultMode: "bypassPermissions",
    allowedModes: ["bypassPermissions"],
    tools: [...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS, ...SYSTEM_TOOLS],
  },
};

export const PROFILES = Object.freeze(Object.fromEntries(
  Object.entries(PROFILE_DATA).map(([name, profile]) => [name, Object.freeze({
    ...profile,
    allowedModes: Object.freeze(profile.allowedModes),
    tools: Object.freeze(profile.tools),
  })]),
));

export const CLAUDE_HOOK_MODES = Object.freeze([
  "default",
  "plan",
  "acceptEdits",
  "auto",
  "dontAsk",
  "bypassPermissions",
]);

export function requireProfile(ceiling) {
  const profile = PROFILES[ceiling];
  if (!profile) {
    throw new Error(`Unknown Codex ceiling ${JSON.stringify(ceiling)}; expected repository, isolated-write, or system.`);
  }
  return profile;
}

export function normalizeLauncherMode(ceiling, requestedMode) {
  const profile = requireProfile(ceiling);
  const mode = requestedMode ?? profile.defaultMode;
  if (!profile.allowedModes.includes(mode)) {
    throw new Error(
      `Claude permission mode ${JSON.stringify(mode)} is not valid for the ${ceiling} ceiling; expected ${profile.allowedModes.join(", ")}.`,
    );
  }
  return mode;
}

export function normalizeHookMode(mode) {
  if (mode === "manual") return "default";
  if (!CLAUDE_HOOK_MODES.includes(mode)) {
    throw new Error("The Claude hook input has a missing or invalid permission_mode.");
  }
  return mode;
}

export function toolKind(toolName) {
  if (REVIEW_TOOLS.includes(toolName)) return "review";
  if (IMPLEMENTATION_TOOLS.includes(toolName)) return "implementation";
  if (SYSTEM_TOOLS.includes(toolName)) return "system";
  return null;
}

export function toolOperation(toolName) {
  if (toolName === "codex_implementation_start" || toolName === "codex_system_start") return "start";
  if (REVIEW_TOOLS.includes(toolName)) return "review";
  if (IMPLEMENTATION_TOOLS.includes(toolName) || SYSTEM_TOOLS.includes(toolName)) return "control";
  return null;
}

export function visibleToolsForCeiling(ceiling) {
  return [...requireProfile(ceiling).tools];
}

export function isModeAllowedForTool(toolName, mode) {
  const normalized = normalizeHookMode(mode);
  const kind = toolKind(toolName);
  const operation = toolOperation(toolName);
  if (operation === "review" || operation === "control") return true;
  if (kind === "implementation") {
    return normalized === "acceptEdits" || normalized === "auto" || normalized === "bypassPermissions";
  }
  if (kind === "system") return normalized === "bypassPermissions";
  return false;
}

export function ceilingAllowsTool(ceiling, toolName) {
  return requireProfile(ceiling).tools.includes(toolName);
}

function canonicalNumber(value) {
  if (!Number.isFinite(value)) throw new Error("Grant input contains a non-finite number.");
  return JSON.stringify(value);
}

export function canonicalJson(value) {
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "boolean") return JSON.stringify(value);
  if (typeof value === "number") return canonicalNumber(value);
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object") {
    const prototype = Object.getPrototypeOf(value);
    if (prototype !== Object.prototype && prototype !== null) {
      throw new Error("Grant input must contain only plain JSON objects.");
    }
    const keys = Object.keys(value).sort();
    return `{${keys.map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`).join(",")}}`;
  }
  throw new Error("Grant input contains a value that JSON cannot represent.");
}

export function sha256Hex(value) {
  return crypto.createHash("sha256").update(value, "utf8").digest("hex");
}

function validateBoundString(value, field, maxLength = 256) {
  if (typeof value !== "string" || value.length === 0 || value.length > maxLength || /[\0\r\n]/u.test(value)) {
    throw new Error(`The Claude hook input has an invalid ${field}.`);
  }
  return value;
}

export function readGrantKey(keyFile) {
  validateBoundString(keyFile, GRANT_KEY_FILE_ENV, 4096);
  if (!path.isAbsolute(keyFile)) throw new Error("The Codex grant key path must be absolute.");

  const parent = path.dirname(keyFile);
  if (fs.realpathSync(parent) !== parent || fs.realpathSync(keyFile) !== keyFile) {
    throw new Error("The Codex grant key path is not canonical.");
  }
  const parentStat = fs.lstatSync(parent);
  if (!parentStat.isDirectory() || parentStat.isSymbolicLink() || (parentStat.mode & 0o077) !== 0) {
    throw new Error("The Codex grant session directory is not a private directory.");
  }
  if (typeof process.getuid === "function" && parentStat.uid !== process.getuid()) {
    throw new Error("The Codex grant session directory is not owned by the current user.");
  }

  const noFollow = fs.constants.O_NOFOLLOW ?? 0;
  const pathStat = fs.lstatSync(keyFile);
  if (pathStat.isSymbolicLink()) throw new Error("The Codex grant key must not be a symbolic link.");
  const descriptor = fs.openSync(keyFile, fs.constants.O_RDONLY | noFollow);
  try {
    const stat = fs.fstatSync(descriptor);
    if (!stat.isFile() || (stat.mode & 0o077) !== 0 || stat.size !== 32) {
      throw new Error("The Codex grant key is not a private 32-byte regular file.");
    }
    if (typeof process.getuid === "function" && stat.uid !== process.getuid()) {
      throw new Error("The Codex grant key is not owned by the current user.");
    }
    const key = fs.readFileSync(descriptor);
    if (key.length !== 32) throw new Error("The Codex grant key has an invalid length.");
    const postStat = fs.lstatSync(keyFile);
    if (pathStat.dev !== stat.dev || pathStat.ino !== stat.ino || postStat.dev !== stat.dev || postStat.ino !== stat.ino) {
      throw new Error("The Codex grant key path changed while it was being read.");
    }
    if (fs.realpathSync(keyFile) !== keyFile) throw new Error("The Codex grant key path changed while it was being read.");
    return key;
  } finally {
    fs.closeSync(descriptor);
  }
}

function grantSigningFields({ ceiling, toolName, mode, toolUseId, sessionId, issuedAtMs, nonce, inputSha256 }) {
  requireProfile(ceiling);
  return {
    v: 1,
    ceiling,
    tool: validateBoundString(toolName, "tool_name", 256),
    mode: normalizeHookMode(mode),
    tool_use_id: validateBoundString(toolUseId, "tool_use_id", 256),
    session_id: sessionId == null ? null : validateBoundString(sessionId, "session_id", 256),
    issued_at_ms: issuedAtMs,
    nonce: validateBoundString(nonce, "grant nonce", 128),
    input_sha256: validateBoundString(inputSha256, "input hash", 64),
  };
}

export function createGrant({
  key,
  ceiling,
  toolName,
  mode,
  toolUseId,
  sessionId = null,
  originalInput,
  now = Date.now(),
  nonce = crypto.randomBytes(18).toString("base64url"),
}) {
  if (!Buffer.isBuffer(key) || key.length !== 32) throw new Error("The Codex grant key must contain exactly 32 bytes.");
  if (!Number.isSafeInteger(now) || now <= 0) throw new Error("The Codex grant timestamp is invalid.");
  if (!originalInput || typeof originalInput !== "object" || Array.isArray(originalInput)) {
    throw new Error("The delegated Codex tool input must be a JSON object.");
  }
  if (Object.hasOwn(originalInput, GRANT_FIELD)) {
    throw new Error(`The reserved ${GRANT_FIELD} field must not be caller supplied.`);
  }

  const fields = grantSigningFields({
    ceiling,
    toolName,
    mode,
    toolUseId,
    sessionId,
    issuedAtMs: now,
    nonce,
    inputSha256: sha256Hex(canonicalJson(originalInput)),
  });
  const mac = crypto.createHmac("sha256", key).update(canonicalJson(fields), "utf8").digest("base64url");
  return Object.freeze({ ...fields, mac });
}
