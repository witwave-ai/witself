import { isUtf8 } from "node:buffer";
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import { constants as fsConstants } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import process from "node:process";

import { BrokerError, isContained, killProcessTree } from "./util.mjs";

const HANDLE_STATE = new WeakMap();
const JOB_NAME_RE = /^implementation-([0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$/i;
const SAFE_PATH_BYTES = 4_096;
const ARTIFACT_CHUNK_BYTES = 48 * 1024;
const PROCESS_STDERR_BYTES = 64 * 1024;
const DEFAULT_OPERATION_DEADLINE_MS = 10 * 60 * 1000;
const HISTORY_ENUMERATION_LINE_BYTES = 80;
const HISTORY_TYPE_BATCH_SIZE = 4_096;
const HISTORY_TREE_BATCH_BYTES = 64 * 1024 * 1024;
const FREE_SPACE_RESERVE_BYTES = 512 * 1024 * 1024;

export const ISOLATED_WORKSPACE_LIMITS = Object.freeze({
  maxFiles: 100_000,
  maxFileBytes: 128 * 1024 * 1024,
  maxTotalBytes: 512 * 1024 * 1024,
  maxChangedFiles: 200,
  maxPatchBytes: 4 * 1024 * 1024,
  maxEvidenceBytes: 256 * 1024,
  maxArtifactChunkBytes: ARTIFACT_CHUNK_BYTES,
  maxHistoryObjects: 1_000_000,
  maxHistoryObjectBytes: 512 * 1024 * 1024,
  maxHistoryTreeBytes: 64 * 1024 * 1024,
  maxHistoryBytes: 8 * 1024 * 1024 * 1024,
  maxBundleBytes: 4 * 1024 * 1024 * 1024,
  maxCloneBytes: 8 * 1024 * 1024 * 1024,
  maxJobBytes: 12 * 1024 * 1024 * 1024,
});

const LIMIT_KEYS = Object.freeze(Object.keys(ISOLATED_WORKSPACE_LIMITS));

function brokerError(code, message) {
  return new BrokerError(code, message);
}

function operationOptions(options = {}) {
  const deadlineMs = options.deadlineMs ?? DEFAULT_OPERATION_DEADLINE_MS;
  if (!Number.isSafeInteger(deadlineMs) || deadlineMs < 1 || deadlineMs > DEFAULT_OPERATION_DEADLINE_MS) {
    throw brokerError("isolated_workspace_invalid", "The isolated workspace deadline was invalid or exceeded its hard ceiling.");
  }
  if (options.signal !== undefined && (!options.signal || typeof options.signal.aborted !== "boolean" || typeof options.signal.addEventListener !== "function")) {
    throw brokerError("isolated_workspace_invalid", "The isolated workspace abort signal was invalid.");
  }
  return { deadlineAt: Date.now() + deadlineMs, signal: options.signal ?? null };
}

function beginOperation(state, options = {}) {
  if (state.operation) throw brokerError("isolated_workspace_busy", "Another isolated workspace lifecycle operation is already active.");
  state.operation = operationOptions(options);
  try {
    checkOperation(state);
  } catch (error) {
    state.operation = null;
    throw error;
  }
}

function endOperation(state) {
  state.operation = null;
}

function checkOperation(state) {
  const operation = state.operation;
  if (!operation) return;
  if (operation.signal?.aborted) throw brokerError("isolated_workspace_aborted", "The isolated workspace operation was aborted.");
  if (Date.now() >= operation.deadlineAt) throw brokerError("isolated_workspace_deadline", "The isolated workspace operation exceeded its bounded deadline.");
}

function remainingOperationMs(state, requested = 60_000) {
  checkOperation(state);
  if (!state.operation) return requested;
  return Math.max(1, Math.min(requested, state.operation.deadlineAt - Date.now()));
}

function normalizeLimits(overrides = {}) {
  if (!overrides || typeof overrides !== "object" || Array.isArray(overrides)) {
    throw brokerError("isolated_workspace_invalid", "Isolated workspace limits must be an object.");
  }
  const limits = { ...ISOLATED_WORKSPACE_LIMITS };
  for (const [key, value] of Object.entries(overrides)) {
    if (!LIMIT_KEYS.includes(key) || !Number.isSafeInteger(value) || value < 1 || value > ISOLATED_WORKSPACE_LIMITS[key]) {
      throw brokerError("isolated_workspace_invalid", "An isolated workspace limit was invalid or exceeded its hard ceiling.");
    }
    limits[key] = value;
  }
  return Object.freeze(limits);
}

function sameStat(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.mode === right.mode &&
    left.size === right.size && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs;
}

function sameObject(left, right) {
  return left.dev === right.dev && left.ino === right.ino && (left.mode & 0o170000n) === (right.mode & 0o170000n);
}

function statIdentity(stat) {
  return Object.freeze({
    dev: stat.dev,
    ino: stat.ino,
    mode: stat.mode,
    size: stat.size,
    mtimeNs: stat.mtimeNs,
    ctimeNs: stat.ctimeNs,
  });
}

function sha256(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

function safeUtf8(buffer, code, message) {
  if (!isUtf8(buffer)) throw brokerError(code, message);
  return buffer.toString("utf8");
}

function safeRelativePath(value) {
  const bytes = Buffer.from(value);
  if (!value || bytes.length > SAFE_PATH_BYTES || value.includes("\\") || /[\u0000-\u001f\u007f]/u.test(value) ||
      path.posix.isAbsolute(value) || path.posix.normalize(value) !== value || value === "." || value === ".." ||
      value.startsWith("../") || value.endsWith("/") || value.split("/").some((part) => !part || part === "." || part === ".." || part.toLowerCase() === ".git")) {
    throw brokerError("isolated_workspace_unsafe_path", "Git returned a path that is unsafe to reproduce or patch.");
  }
  return value;
}

function parseNulPaths(buffer, limits) {
  if (buffer.length === 0) return [];
  if (buffer.at(-1) !== 0) throw brokerError("isolated_workspace_git_failed", "Git returned an incomplete path list.");
  const paths = [];
  let start = 0;
  while (start < buffer.length) {
    const end = buffer.indexOf(0, start);
    if (end < 0) throw brokerError("isolated_workspace_git_failed", "Git returned an incomplete path list.");
    const raw = buffer.subarray(start, end);
    if (!isUtf8(raw)) throw brokerError("isolated_workspace_unsafe_path", "A Git path was not valid UTF-8.");
    paths.push(safeRelativePath(raw.toString("utf8")));
    start = end + 1;
  }
  if (paths.length > limits.maxFiles) {
    throw brokerError("isolated_workspace_limit", "The repository contains too many tracked or nonignored untracked files for an isolated snapshot.");
  }
  const unique = new Set(paths);
  if (unique.size !== paths.length) throw brokerError("isolated_workspace_unsafe_path", "Git returned duplicate workspace paths.");
  return [...unique].sort((left, right) => Buffer.compare(Buffer.from(left), Buffer.from(right)));
}

function parseNulRecords(buffer) {
  if (buffer.length === 0) return [];
  if (buffer.at(-1) !== 0) throw brokerError("isolated_workspace_git_failed", "Git returned an incomplete record list.");
  const records = [];
  let start = 0;
  while (start < buffer.length) {
    const end = buffer.indexOf(0, start);
    records.push(buffer.subarray(start, end));
    start = end + 1;
  }
  return records;
}

function hasGitlink(records) {
  return parseNulRecords(records).some((record) => record.subarray(0, 7).toString("ascii") === "160000 ");
}

function hasSkipWorktree(records) {
  return parseNulRecords(records).some((record) =>
    record.length >= 2 && (record[0] === 0x53 || record[0] === 0x73) && record[1] === 0x20);
}

function gitExecutable(candidate) {
  if (candidate) return candidate;
  return process.platform === "win32" ? "git.exe" : "/usr/bin/git";
}

function privateGitEnv(home, temp) {
  const nullDevice = process.platform === "win32" ? "NUL" : "/dev/null";
  const falseCommand = process.platform === "win32" ? "cmd /c exit 1" : "/usr/bin/false";
  return Object.freeze({
    HOME: home,
    XDG_CONFIG_HOME: path.join(home, ".config"),
    TMPDIR: temp,
    TMP: temp,
    TEMP: temp,
    PATH: process.platform === "win32" ? "C:\\Windows\\System32" : "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
    LANG: "C",
    LC_ALL: "C",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_SYSTEM: nullDevice,
    GIT_CONFIG_GLOBAL: nullDevice,
    GIT_TERMINAL_PROMPT: "0",
    GIT_ASKPASS: falseCommand,
    SSH_ASKPASS: falseCommand,
    GIT_SSH_COMMAND: falseCommand,
    GIT_OPTIONAL_LOCKS: "0",
    GIT_NO_LAZY_FETCH: "1",
    GIT_NO_REPLACE_OBJECTS: "1",
    GIT_LFS_SKIP_SMUDGE: "1",
    GCM_INTERACTIVE: "never",
    PAGER: "cat",
    GIT_PAGER: "cat",
    GIT_EDITOR: falseCommand,
    GIT_SEQUENCE_EDITOR: falseCommand,
    GIT_MERGE_AUTOEDIT: "no",
  });
}

function safeGitConfig(hooksPath) {
  return [
    ["core.hooksPath", hooksPath],
    ["core.fsmonitor", "false"],
    ["core.untrackedCache", "false"],
    ["core.excludesFile", process.platform === "win32" ? "NUL" : "/dev/null"],
    ["protocol.allow", "never"],
    ["submodule.recurse", "false"],
    ["fetch.recurseSubmodules", "false"],
    ["maintenance.auto", "false"],
    ["gc.auto", "0"],
    ["commit.gpgSign", "false"],
    ["tag.gpgSign", "false"],
    ["credential.helper", ""],
    ["filter.lfs.required", "false"],
    ["filter.lfs.smudge", ""],
    ["filter.lfs.clean", ""],
    ["diff.external", ""],
  ];
}

function gitInvocation(repository, hooksPath, args, extraConfig = []) {
  const invocation = ["--no-optional-locks"];
  for (const [key, value] of [...safeGitConfig(hooksPath), ...extraConfig]) invocation.push("-c", `${key}=${value}`);
  if (repository) invocation.push("-C", repository);
  return [...invocation, ...args];
}

function runProcess(command, args, options = {}) {
  const {
    cwd,
    env,
    input,
    timeoutMs = 60_000,
    maxStdoutBytes = 16 * 1024 * 1024,
    maxStderrBytes = PROCESS_STDERR_BYTES,
    discardStdout = false,
    signal: abortSignal = null,
  } = options;
  if (abortSignal?.aborted) return Promise.reject(brokerError("isolated_workspace_aborted", "The isolated workspace operation was aborted."));
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      env,
      detached: process.platform !== "win32",
      stdio: ["pipe", discardStdout ? "ignore" : "pipe", "pipe"],
      windowsHide: true,
    });
    const stdoutChunks = [];
    const stderrChunks = [];
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let exceeded = null;
    let timedOut = false;
    let aborted = false;
    let stdinFailed = false;
    let settled = false;
    let forceKillTimer;

    const stop = () => {
      killProcessTree(child);
      forceKillTimer ??= setTimeout(() => killProcessTree(child, "SIGKILL"), 1_000);
      forceKillTimer.unref?.();
    };
    const onAbort = () => {
      aborted = true;
      stop();
    };
    abortSignal?.addEventListener("abort", onAbort, { once: true });
    child.stdout?.on("data", (chunk) => {
      stdoutBytes += chunk.length;
      if (stdoutBytes > maxStdoutBytes) {
        exceeded = "stdout";
        stop();
      } else stdoutChunks.push(chunk);
    });
    child.stderr.on("data", (chunk) => {
      stderrBytes += chunk.length;
      if (stderrBytes > maxStderrBytes) {
        exceeded = "stderr";
        stop();
      } else stderrChunks.push(chunk);
    });
    child.stdin.on("error", () => {
      if (settled) return;
      stdinFailed = true;
      stop();
    });
    child.on("error", () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(forceKillTimer);
      abortSignal?.removeEventListener("abort", onAbort);
      reject(brokerError("isolated_workspace_process_failed", "A required local Git process could not be started."));
    });
    child.on("close", (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(forceKillTimer);
      abortSignal?.removeEventListener("abort", onAbort);
      if (aborted) return reject(brokerError("isolated_workspace_aborted", "The isolated workspace operation was aborted."));
      if (stdinFailed) return reject(brokerError("isolated_workspace_process_failed", "A required local Git process closed its input before the bounded request completed."));
      if (exceeded) return reject(brokerError("isolated_workspace_process_limit", `A required local Git process exceeded its ${exceeded} limit.`));
      if (timedOut) return reject(brokerError("isolated_workspace_process_timeout", "A required local Git process timed out."));
      resolve({
        code,
        signal,
        stdout: discardStdout ? Buffer.alloc(0) : Buffer.concat(stdoutChunks),
        stderr: Buffer.concat(stderrChunks),
      });
    });
    const timer = setTimeout(() => {
      timedOut = true;
      stop();
    }, timeoutMs);
    timer.unref?.();
    if (input === undefined) child.stdin.end();
    else child.stdin.end(input);
  });
}

async function runGit(state, repository, args, options = {}) {
  checkOperation(state);
  let result;
  try {
    result = await runProcess(state.git, gitInvocation(repository, state.hooksRoot, args, options.extraConfig), {
      cwd: repository ?? state.jobRoot,
      env: state.gitEnv,
      input: options.input,
      timeoutMs: remainingOperationMs(state, options.timeoutMs ?? 60_000),
      maxStdoutBytes: options.maxStdoutBytes,
      maxStderrBytes: options.maxStderrBytes,
      discardStdout: options.discardStdout,
      signal: state.operation?.signal,
    });
  } catch (error) {
    checkOperation(state);
    throw error;
  }
  checkOperation(state);
  if (result.code !== 0 && !options.allowCodes?.includes(result.code)) {
    throw brokerError(options.errorCode ?? "isolated_workspace_git_failed", options.errorMessage ?? "A required isolated Git operation failed.");
  }
  return result;
}

async function runStateProcess(state, command, args, options = {}) {
  checkOperation(state);
  let result;
  try {
    result = await runProcess(command, args, {
      ...options,
      timeoutMs: remainingOperationMs(state, options.timeoutMs ?? 60_000),
      signal: state.operation?.signal,
    });
  } catch (error) {
    checkOperation(state);
    throw error;
  }
  checkOperation(state);
  return result;
}

function streamReachableObjectIds(state, repository, onObjectId) {
  checkOperation(state);
  return new Promise((resolve, reject) => {
    const child = spawn(state.git, gitInvocation(repository, state.hooksRoot, [
      "rev-list", "--objects", "--no-object-names", "--all", "HEAD",
    ]), {
      cwd: repository,
      env: state.gitEnv,
      detached: process.platform !== "win32",
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });
    let remainder = Buffer.alloc(0);
    let stderrBytes = 0;
    let failure = null;
    let timedOut = false;
    let aborted = false;
    let settled = false;
    let forceKillTimer;

    const stop = () => {
      killProcessTree(child);
      forceKillTimer ??= setTimeout(() => killProcessTree(child, "SIGKILL"), 1_000);
      forceKillTimer.unref?.();
    };
    const fail = (error) => {
      failure ??= error;
      stop();
    };
    const onAbort = () => {
      aborted = true;
      stop();
    };
    state.operation?.signal?.addEventListener("abort", onAbort, { once: true });
    child.stdout.on("data", (chunk) => {
      if (failure) return;
      const data = remainder.length === 0 ? chunk : Buffer.concat([remainder, chunk]);
      let start = 0;
      try {
        while (true) {
          const end = data.indexOf(0x0a, start);
          if (end < 0) break;
          const line = data.subarray(start, end);
          if (line.length < 1 || line.length > HISTORY_ENUMERATION_LINE_BYTES) {
            throw brokerError("isolated_workspace_history_limit", "Reachable Git object enumeration returned an invalid or overlong record.");
          }
          onObjectId(line);
          checkOperation(state);
          start = end + 1;
        }
        remainder = data.subarray(start);
        if (remainder.length > HISTORY_ENUMERATION_LINE_BYTES) {
          throw brokerError("isolated_workspace_history_limit", "Reachable Git object enumeration exceeded its record bound.");
        }
      } catch (error) {
        fail(error instanceof BrokerError ? error : brokerError("isolated_workspace_history_failed", "Reachable Git object enumeration failed safely."));
      }
    });
    child.stderr.on("data", (chunk) => {
      stderrBytes += chunk.length;
      if (stderrBytes > PROCESS_STDERR_BYTES) fail(brokerError("isolated_workspace_process_limit", "Reachable Git object enumeration exceeded its stderr limit."));
    });
    child.on("error", () => fail(brokerError("isolated_workspace_process_failed", "Reachable Git object enumeration could not be started.")));
    child.on("close", (code) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(forceKillTimer);
      state.operation?.signal?.removeEventListener("abort", onAbort);
      if (aborted) return reject(brokerError("isolated_workspace_aborted", "The isolated workspace operation was aborted."));
      if (failure) return reject(failure);
      if (timedOut || Date.now() >= state.operation.deadlineAt) return reject(brokerError("isolated_workspace_deadline", "The isolated workspace operation exceeded its bounded deadline."));
      if (code !== 0 || remainder.length !== 0) return reject(brokerError("isolated_workspace_incomplete_history", "Reachable Git object enumeration was incomplete."));
      resolve();
    });
    const timer = setTimeout(() => {
      timedOut = true;
      stop();
    }, remainingOperationMs(state, 120_000));
    timer.unref?.();
  });
}

function parseObjectTypeBatch(state, buffer, expected, limits) {
  const text = safeUtf8(buffer, "isolated_workspace_history_failed", "Git returned invalid object metadata.");
  const lines = text.endsWith("\n") ? text.slice(0, -1).split("\n") : [];
  if (lines.length !== expected.length) throw brokerError("isolated_workspace_incomplete_history", "Git returned incomplete reachable object metadata.");
  const trees = [];
  let totalBytes = 0;
  let largestBytes = 0;
  for (let index = 0; index < lines.length; index += 1) {
    checkOperation(state);
    const match = /^([0-9a-f]{40}|[0-9a-f]{64}) (blob|tree|commit|tag) ([0-9]+)$/.exec(lines[index]);
    if (!match || match[1] !== expected[index]) throw brokerError("isolated_workspace_incomplete_history", "Git returned invalid reachable object metadata.");
    const size = Number(match[3]);
    if (!Number.isSafeInteger(size) || size < 0 || size > limits.maxHistoryObjectBytes) {
      throw brokerError("isolated_workspace_history_limit", "A reachable Git object exceeded the per-object history limit.");
    }
    totalBytes += size;
    if (!Number.isSafeInteger(totalBytes)) throw brokerError("isolated_workspace_history_limit", "Reachable Git object sizes exceeded safe integer bounds.");
    largestBytes = Math.max(largestBytes, size);
    if (match[2] === "tree") {
      if (size > limits.maxHistoryTreeBytes) throw brokerError("isolated_workspace_history_limit", "A reachable Git tree exceeded the bounded tree inspection limit.");
      trees.push({ oid: match[1], size });
    }
  }
  return { trees, totalBytes, largestBytes };
}

function inspectRawTreeBody(state, body, rawObjectIdBytes) {
  let offset = 0;
  while (offset < body.length) {
    checkOperation(state);
    const space = body.indexOf(0x20, offset);
    const nul = space < 0 ? -1 : body.indexOf(0, space + 1);
    if (space < 0 || nul <= space + 1 || nul + 1 + rawObjectIdBytes > body.length) {
      throw brokerError("isolated_workspace_incomplete_history", "A reachable Git tree object was malformed.");
    }
    const mode = body.subarray(offset, space).toString("ascii");
    if (!/^[0-7]{5,6}$/.test(mode)) throw brokerError("isolated_workspace_incomplete_history", "A reachable Git tree mode was malformed.");
    if (mode === "160000") throw brokerError("isolated_workspace_submodule", "Reachable Git history contains a submodule entry.");
    offset = nul + 1 + rawObjectIdBytes;
  }
  if (offset !== body.length) throw brokerError("isolated_workspace_incomplete_history", "A reachable Git tree object was truncated.");
}

function inspectTreeBatch(state, buffer, expected, objectFormat) {
  const rawObjectIdBytes = objectFormat === "sha256" ? 32 : 20;
  let offset = 0;
  for (const tree of expected) {
    checkOperation(state);
    const newline = buffer.indexOf(0x0a, offset);
    if (newline < 0) throw brokerError("isolated_workspace_incomplete_history", "Git returned an incomplete tree batch header.");
    const header = buffer.subarray(offset, newline).toString("ascii");
    const match = /^([0-9a-f]{40}|[0-9a-f]{64}) tree ([0-9]+)$/.exec(header);
    if (!match || match[1] !== tree.oid || Number(match[2]) !== tree.size) {
      throw brokerError("isolated_workspace_incomplete_history", "Git returned mismatched reachable tree content.");
    }
    const bodyStart = newline + 1;
    const bodyEnd = bodyStart + tree.size;
    if (bodyEnd >= buffer.length || buffer[bodyEnd] !== 0x0a) throw brokerError("isolated_workspace_incomplete_history", "Git returned truncated reachable tree content.");
    inspectRawTreeBody(state, buffer.subarray(bodyStart, bodyEnd), rawObjectIdBytes);
    offset = bodyEnd + 1;
  }
  if (offset !== buffer.length) throw brokerError("isolated_workspace_incomplete_history", "Git returned unexpected reachable tree content.");
}

async function preflightReachableHistory(state, repository, objectFormat) {
  const objectIds = new Set();
  let enumeratedLines = 0;
  const oidPattern = objectFormat === "sha256" ? /^[0-9a-f]{64}$/ : /^[0-9a-f]{40}$/;
  await streamReachableObjectIds(state, repository, (line) => {
    enumeratedLines += 1;
    if (enumeratedLines > state.limits.maxHistoryObjects) {
      throw brokerError("isolated_workspace_history_limit", "Reachable Git object enumeration exceeded its bounded record count.");
    }
    const oid = line.toString("ascii");
    if (!oidPattern.test(oid)) throw brokerError("isolated_workspace_incomplete_history", "Reachable Git object enumeration returned an invalid object ID.");
    objectIds.add(oid);
    if (objectIds.size > state.limits.maxHistoryObjects) throw brokerError("isolated_workspace_history_limit", "Reachable Git history contains too many unique objects.");
  });
  if (objectIds.size === 0) throw brokerError("isolated_workspace_incomplete_history", "Reachable Git history did not contain any objects.");

  const ordered = [...objectIds].sort();
  const trees = [];
  let totalBytes = 0;
  let largestObjectBytes = 0;
  for (let offset = 0; offset < ordered.length; offset += HISTORY_TYPE_BATCH_SIZE) {
    checkOperation(state);
    const chunk = ordered.slice(offset, offset + HISTORY_TYPE_BATCH_SIZE);
    const result = await runGit(state, repository, ["cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"], {
      input: Buffer.from(`${chunk.join("\n")}\n`),
      maxStdoutBytes: chunk.length * 112,
      timeoutMs: 120_000,
      errorCode: "isolated_workspace_incomplete_history",
      errorMessage: "Reachable Git object metadata could not be read completely.",
    });
    const parsed = parseObjectTypeBatch(state, result.stdout, chunk, state.limits);
    totalBytes += parsed.totalBytes;
    if (!Number.isSafeInteger(totalBytes) || totalBytes > state.limits.maxHistoryBytes) {
      throw brokerError("isolated_workspace_history_limit", "Reachable Git history exceeded its cumulative byte limit.");
    }
    largestObjectBytes = Math.max(largestObjectBytes, parsed.largestBytes);
    trees.push(...parsed.trees);
  }

  let treeBatch = [];
  let treeBatchBytes = 0;
  let inspectedTreeBytes = 0;
  const inspectBatch = async () => {
    if (treeBatch.length === 0) return;
    const headerBytes = treeBatch.length * 96;
    const result = await runGit(state, repository, ["cat-file", "--batch"], {
      input: Buffer.from(`${treeBatch.map((tree) => tree.oid).join("\n")}\n`),
      maxStdoutBytes: treeBatchBytes + headerBytes,
      timeoutMs: 120_000,
      errorCode: "isolated_workspace_incomplete_history",
      errorMessage: "Reachable Git tree content could not be read completely.",
    });
    inspectTreeBatch(state, result.stdout, treeBatch, objectFormat);
    inspectedTreeBytes += treeBatchBytes;
    treeBatch = [];
    treeBatchBytes = 0;
  };
  for (const tree of trees) {
    checkOperation(state);
    if (treeBatch.length > 0 && treeBatchBytes + tree.size > HISTORY_TREE_BATCH_BYTES) await inspectBatch();
    treeBatch.push(tree);
    treeBatchBytes += tree.size;
  }
  await inspectBatch();
  return Object.freeze({
    objectCount: objectIds.size,
    totalBytes,
    largestObjectBytes,
    treeCount: trees.length,
    treeBytes: inspectedTreeBytes,
  });
}

async function assertDirectory(candidate, code, message) {
  let stat;
  try { stat = await fs.lstat(candidate, { bigint: true }); }
  catch { throw brokerError(code, message); }
  if (!stat.isDirectory() || stat.isSymbolicLink()) throw brokerError(code, message);
  return stat;
}

async function assertCanonicalDirectory(candidate, code, message) {
  let canonical;
  try { canonical = await fs.realpath(candidate); }
  catch { throw brokerError(code, message); }
  const stat = await assertDirectory(canonical, code, message);
  return { canonical, stat };
}

async function validateCachedParent(root, candidate, directoryCache) {
  const immediateParent = path.dirname(candidate);
  let verifiedParent = immediateParent;
  const missingParents = [];
  while (!directoryCache.has(verifiedParent)) {
    if (verifiedParent === root || !isContained(root, verifiedParent)) {
      throw brokerError("isolated_workspace_unsafe_path", "A workspace path had no verified repository ancestor.");
    }
    try {
      await fs.lstat(verifiedParent);
      throw brokerError("isolated_workspace_drift", "A previously absent workspace parent reappeared during capture.");
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      if (error?.code !== "ENOENT") throw brokerError("isolated_workspace_drift", "A workspace parent could not be verified as absent.");
    }
    missingParents.push(verifiedParent);
    verifiedParent = path.dirname(verifiedParent);
  }
  const expected = directoryCache.get(verifiedParent);
  let current;
  let canonical;
  try {
    current = await fs.lstat(verifiedParent, { bigint: true });
    canonical = await fs.realpath(verifiedParent);
  } catch { throw brokerError("isolated_workspace_drift", "A workspace parent directory changed during capture."); }
  if (!current.isDirectory() || current.isSymbolicLink() || !sameStat(expected, current) || canonical !== verifiedParent || !isContained(root, canonical)) {
    throw brokerError("isolated_workspace_drift", "A workspace ancestor chain changed or became non-canonical during capture.");
  }
  return { root, immediateParent, verifiedParent, identity: expected, missingParents };
}

async function recheckCachedParent(parent) {
  let current;
  let canonical;
  try {
    current = await fs.lstat(parent.verifiedParent, { bigint: true });
    canonical = await fs.realpath(parent.verifiedParent);
  } catch { throw brokerError("isolated_workspace_drift", "A workspace parent directory changed during capture."); }
  if (!current.isDirectory() || current.isSymbolicLink() || !sameStat(parent.identity, current) || canonical !== parent.verifiedParent || !isContained(parent.root, canonical)) {
    throw brokerError("isolated_workspace_drift", "A workspace ancestor chain changed or became non-canonical during capture.");
  }
  for (const missing of parent.missingParents) {
    try {
      await fs.lstat(missing);
      throw brokerError("isolated_workspace_drift", "A previously absent workspace parent reappeared during capture.");
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      if (error?.code !== "ENOENT") throw brokerError("isolated_workspace_drift", "A workspace parent could not be rechecked as absent.");
    }
  }
}

async function requirePathAbsent(candidate) {
  try {
    await fs.lstat(candidate);
    throw brokerError("isolated_workspace_drift", "A workspace path appeared while its deletion was being captured.");
  } catch (error) {
    if (error instanceof BrokerError) throw error;
    if (error?.code !== "ENOENT") throw brokerError("isolated_workspace_drift", "A missing workspace path could not be rechecked safely.");
  }
}

function validateSymlinkTarget(root, relative, targetBytes) {
  if (!isUtf8(targetBytes) || targetBytes.includes(0) || targetBytes.length < 1 || targetBytes.length > SAFE_PATH_BYTES) {
    throw brokerError("isolated_workspace_unsafe_symlink", "A workspace symlink target was empty or not safe UTF-8.");
  }
  const target = targetBytes.toString("utf8");
  if (target.includes("\\") || path.isAbsolute(target) || /[\u0000-\u001f\u007f]/u.test(target)) {
    throw brokerError("isolated_workspace_unsafe_symlink", "A workspace symlink target was absolute or unsafe.");
  }
  const linkPath = path.join(root, ...relative.split("/"));
  const resolved = path.resolve(path.dirname(linkPath), target);
  if (!isContained(root, resolved)) throw brokerError("isolated_workspace_unsafe_symlink", "A workspace symlink target escaped its repository root.");
  const rawComponents = path.normalize(target).split(path.sep);
  const resolvedRelative = path.relative(root, resolved);
  const resolvedComponents = resolvedRelative.split(path.sep);
  if ([...rawComponents, ...resolvedComponents].some((component) => component.toLowerCase() === ".git")) {
    throw brokerError("isolated_workspace_unsafe_symlink", "A workspace symlink target referenced protected Git metadata.");
  }
  return resolvedRelative.split(path.sep).join("/");
}

function validateRepresentedSymlinkTargets(state, entries) {
  const represented = new Set(entries.filter((entry) => entry.type !== "missing").map((entry) => entry.path));
  const representedDirectories = new Set();
  for (const candidate of represented) {
    checkOperation(state);
    const segments = candidate.split("/");
    for (let index = 1; index < segments.length; index += 1) representedDirectories.add(segments.slice(0, index).join("/"));
  }
  for (const entry of entries) {
    checkOperation(state);
    if (entry.type !== "symlink") continue;
    const target = entry.targetRelative;
    const representedTarget = target === ""
      ? represented.size > 0
      : represented.has(target) || representedDirectories.has(target);
    if (!representedTarget) {
      throw brokerError("isolated_workspace_unsafe_symlink", "A workspace symlink target was not represented in the bounded snapshot.");
    }
  }
}

async function captureRegularFile(state, candidate, before, outputFile, budget, limits) {
  if (before.size > BigInt(limits.maxFileBytes) || before.size > BigInt(budget.remaining)) {
    throw brokerError("isolated_workspace_limit", "The repository content exceeds the isolated snapshot byte limit.");
  }
  let input;
  let output;
  try {
    input = await fs.open(candidate, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
    const opened = await input.stat({ bigint: true });
    if (!opened.isFile() || !sameStat(before, opened) || (opened.mode & 0o7000n) !== 0n) {
      throw brokerError("isolated_workspace_drift", "A workspace file changed or had an unsupported mode during capture.");
    }
    if (outputFile) output = await fs.open(outputFile, fsConstants.O_RDWR | fsConstants.O_CREAT | fsConstants.O_EXCL, 0o600);
    const digest = crypto.createHash("sha256");
    const buffer = Buffer.allocUnsafe(64 * 1024);
    let position = 0;
    while (position < Number(opened.size)) {
      checkOperation(state);
      const length = Math.min(buffer.length, Number(opened.size) - position);
      const { bytesRead } = await input.read(buffer, 0, length, position);
      if (bytesRead <= 0) throw brokerError("isolated_workspace_drift", "A workspace file changed while it was being read.");
      const chunk = buffer.subarray(0, bytesRead);
      digest.update(chunk);
      if (output) {
        let chunkOffset = 0;
        while (chunkOffset < chunk.length) {
          checkOperation(state);
          const { bytesWritten } = await output.write(chunk, chunkOffset, chunk.length - chunkOffset, position + chunkOffset);
          if (!Number.isSafeInteger(bytesWritten) || bytesWritten <= 0 || bytesWritten > chunk.length - chunkOffset) {
            throw brokerError("isolated_workspace_snapshot_failed", "A private capture copy made no safe write progress.");
          }
          chunkOffset += bytesWritten;
        }
      }
      position += bytesRead;
    }
    await output?.sync();
    const [afterHandle, afterPath] = await Promise.all([input.stat({ bigint: true }), fs.lstat(candidate, { bigint: true })]);
    if (!sameStat(opened, afterHandle) || !sameStat(opened, afterPath)) {
      throw brokerError("isolated_workspace_drift", "A workspace file changed during capture.");
    }
    const sourceDigest = digest.digest("hex");
    if (output) {
      const captured = await output.stat({ bigint: true });
      const capturedAtPath = await fs.lstat(outputFile, { bigint: true });
      if (!captured.isFile() || captured.isSymbolicLink() || captured.size !== opened.size || !sameStat(captured, capturedAtPath)) {
        throw brokerError("isolated_workspace_snapshot_failed", "A private capture copy had the wrong size or identity.");
      }
      const capturedDigest = crypto.createHash("sha256");
      let capturedPosition = 0;
      while (capturedPosition < Number(captured.size)) {
        checkOperation(state);
        const length = Math.min(buffer.length, Number(captured.size) - capturedPosition);
        const { bytesRead } = await output.read(buffer, 0, length, capturedPosition);
        if (bytesRead !== length) throw brokerError("isolated_workspace_snapshot_failed", "A private capture copy could not be verified completely.");
        capturedDigest.update(buffer.subarray(0, bytesRead));
        capturedPosition += bytesRead;
      }
      const capturedAfter = await output.stat({ bigint: true });
      if (!sameStat(captured, capturedAfter) || capturedDigest.digest("hex") !== sourceDigest) {
        throw brokerError("isolated_workspace_snapshot_failed", "A private capture copy did not match its source bytes.");
      }
    }
    budget.remaining -= Number(opened.size);
    budget.total += Number(opened.size);
    const executable = (opened.mode & 0o111n) !== 0n;
    return {
      type: "file",
      gitMode: executable ? "100755" : "100644",
      materializedMode: executable ? 0o755 : 0o644,
      sourceMode: Number(opened.mode & 0o7777n),
      size: Number(opened.size),
      digest: sourceDigest,
    };
  } catch (error) {
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_snapshot_failed", "A workspace file could not be captured safely.");
  } finally {
    await input?.close().catch(() => {});
    await output?.close().catch(() => {});
  }
}

async function captureEntry(state, root, relative, storageRoot, index, budget, limits, directoryCache) {
  checkOperation(state);
  const candidate = path.resolve(root, ...relative.split("/"));
  if (!isContained(root, candidate) || candidate === root) throw brokerError("isolated_workspace_unsafe_path", "A workspace path escaped its repository root.");
  const parent = await validateCachedParent(root, candidate, directoryCache);
  let before;
  try { before = await fs.lstat(candidate, { bigint: true }); }
  catch (error) {
    if (error?.code === "ENOENT") {
      await recheckCachedParent(parent);
      await requirePathAbsent(candidate);
      await recheckCachedParent(parent);
      await requirePathAbsent(candidate);
      return { path: relative, type: "missing", gitMode: null, sourceMode: 0, size: 0, digest: "", storageFile: null };
    }
    throw brokerError("isolated_workspace_snapshot_failed", "A workspace path could not be inspected safely.");
  }
  const storageFile = storageRoot ? path.join(storageRoot, String(index).padStart(8, "0")) : null;
  let entry;
  if (before.isFile()) {
    entry = await captureRegularFile(state, candidate, before, storageFile, budget, limits);
  } else if (before.isSymbolicLink()) {
    let target;
    let targetRelative;
    try {
      target = await fs.readlink(candidate, { encoding: "buffer" });
      const after = await fs.lstat(candidate, { bigint: true });
      if (!sameStat(before, after)) {
        throw brokerError("isolated_workspace_drift", "A workspace symlink changed or had an unsafe target during capture.");
      }
      targetRelative = validateSymlinkTarget(root, relative, target);
      if (storageFile) await fs.writeFile(storageFile, target, { flag: "wx", mode: 0o600 });
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw brokerError("isolated_workspace_snapshot_failed", "A workspace symlink could not be captured safely.");
    }
    if (target.length > budget.remaining) throw brokerError("isolated_workspace_limit", "The repository content exceeds the isolated snapshot byte limit.");
    budget.remaining -= target.length;
    budget.total += target.length;
    entry = { type: "symlink", gitMode: "120000", materializedMode: 0o777, sourceMode: Number(before.mode & 0o7777n), size: target.length, digest: sha256(target), targetRelative };
  } else {
    throw brokerError("isolated_workspace_special_file", "The tracked or nonignored workspace contains an unsupported special file.");
  }
  await recheckCachedParent(parent);
  return { path: relative, ...entry, storageFile };
}

function fingerprintState(metadata, entries, totalBytes) {
  const digest = crypto.createHash("sha256").update("witself-isolated-workspace-v1\0");
  for (const field of [metadata.head, metadata.symbolicHead, metadata.objectFormat]) digest.update(field).update("\0");
  for (const raw of [metadata.refs, metadata.status, metadata.indexStages, metadata.headTree]) {
    digest.update(String(raw.length)).update(":").update(raw).update("\0");
  }
  digest.update(JSON.stringify(metadata.history)).update("\0");
  for (const entry of entries) {
    const name = Buffer.from(entry.path);
    digest.update(String(name.length)).update(":").update(name).update("\0");
    digest.update(entry.type).update("\0").update(entry.gitMode ?? "").update("\0");
    digest.update(String(entry.sourceMode)).update("\0").update(String(entry.size)).update("\0").update(entry.digest).update("\0");
  }
  digest.update(String(totalBytes));
  return digest.digest("hex");
}

async function captureMetadata(state, repository) {
  const common = { maxStderrBytes: PROCESS_STDERR_BYTES };
  const top = await runGit(state, repository, ["rev-parse", "--show-toplevel"], { ...common, maxStdoutBytes: SAFE_PATH_BYTES });
  let topPath;
  try { topPath = await fs.realpath(safeUtf8(top.stdout, "isolated_workspace_git_failed", "Git returned an invalid repository root.").trim()); }
  catch (error) {
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_git_failed", "Git returned an invalid repository root.");
  }
  if (topPath !== repository) throw brokerError("isolated_workspace_unsafe_repository", "The configured path was not the exact Git worktree root.");

  const [bare, shallow, headType, objectFormat, partial] = await Promise.all([
    runGit(state, repository, ["rev-parse", "--is-bare-repository"], { ...common, maxStdoutBytes: 16 }),
    runGit(state, repository, ["rev-parse", "--is-shallow-repository"], { ...common, maxStdoutBytes: 16 }),
    runGit(state, repository, ["cat-file", "-t", "HEAD"], { ...common, maxStdoutBytes: 32 }),
    runGit(state, repository, ["rev-parse", "--show-object-format"], { ...common, maxStdoutBytes: 32 }),
    runGit(state, repository, ["config", "--local", "--null", "--get-regexp", "^(extensions\\.partialclone|remote\\..*\\.(promisor|partialclonefilter))$"], {
      ...common, maxStdoutBytes: 64 * 1024, allowCodes: [1],
    }),
  ]);
  if (bare.stdout.toString("ascii").trim() !== "false") throw brokerError("isolated_workspace_unsafe_repository", "Bare repositories cannot be used for isolated implementation jobs.");
  if (shallow.stdout.toString("ascii").trim() !== "false") throw brokerError("isolated_workspace_incomplete_history", "Shallow repositories cannot provide a complete isolated history.");
  if (headType.stdout.toString("ascii").trim() !== "commit") throw brokerError("isolated_workspace_unsafe_repository", "Repository HEAD was not a commit.");
  if (partial.code === 0 && partial.stdout.length > 0) throw brokerError("isolated_workspace_incomplete_history", "Partial or promisor repositories cannot provide a complete isolated history.");
  const format = objectFormat.stdout.toString("ascii").trim();
  if (format !== "sha1" && format !== "sha256") throw brokerError("isolated_workspace_unsafe_repository", "The repository object format was unsupported.");

  const [sparseCheckout, sparseCheckoutCone, indexFlags] = await Promise.all([
    runGit(state, repository, ["config", "--get", "core.sparseCheckout"], { ...common, maxStdoutBytes: 1024, allowCodes: [1] }),
    runGit(state, repository, ["config", "--get", "core.sparseCheckoutCone"], { ...common, maxStdoutBytes: 1024, allowCodes: [1] }),
    runGit(state, repository, ["ls-files", "-v", "-z"], { ...common, maxStdoutBytes: 24 * 1024 * 1024 }),
  ]);
  if (sparseCheckout.code === 0 || sparseCheckoutCone.code === 0 || hasSkipWorktree(indexFlags.stdout)) {
    throw brokerError("isolated_workspace_sparse_checkout", "Sparse checkout and skip-worktree index entries are not supported for exact isolated snapshots.");
  }

  const head = await runGit(state, repository, ["rev-parse", "--verify", "HEAD"], { ...common, maxStdoutBytes: 80 });
  const headValue = head.stdout.toString("ascii").trim();
  if (!/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(headValue)) throw brokerError("isolated_workspace_git_failed", "Git returned an invalid HEAD object ID.");
  const symbolic = await runGit(state, repository, ["symbolic-ref", "-q", "HEAD"], { ...common, maxStdoutBytes: SAFE_PATH_BYTES, allowCodes: [1] });
  const refs = await runGit(state, repository, ["for-each-ref", "--format=%(refname)%00%(objectname)%00%(objecttype)%00"], { ...common, maxStdoutBytes: 16 * 1024 * 1024 });
  const indexStages = await runGit(state, repository, ["ls-files", "--stage", "-z"], { ...common, maxStdoutBytes: 24 * 1024 * 1024 });
  const headTree = await runGit(state, repository, ["ls-tree", "-r", "-z", "--full-tree", "HEAD"], { ...common, maxStdoutBytes: 24 * 1024 * 1024 });
  if (hasGitlink(indexStages.stdout) || hasGitlink(headTree.stdout)) {
    throw brokerError("isolated_workspace_submodule", "Repositories containing submodules are not accepted for isolated implementation jobs.");
  }
  const history = await preflightReachableHistory(state, repository, format);
  return {
    head: headValue,
    symbolicHead: symbolic.code === 0 ? safeUtf8(symbolic.stdout, "isolated_workspace_git_failed", "Git returned an invalid symbolic HEAD.").trim() : "",
    objectFormat: format,
    refs: refs.stdout,
    // Index stages plus the independently hashed worktree capture cover staged,
    // unstaged, and untracked state without invoking configured clean filters.
    status: Buffer.alloc(0),
    indexStages: indexStages.stdout,
    headTree: headTree.stdout,
    history,
  };
}

async function scanRepositoryDirectories(state, root, limits, expectedRootIdentity = null, enforceAllFileBytes = false) {
  const rootStat = await fs.lstat(root, { bigint: true });
  const pending = [{ directory: root, identity: statIdentity(rootStat) }];
  const directoryCache = new Map([[root, statIdentity(rootStat)]]);
  let inspected = 0;
  let regularBytes = 0;
  while (pending.length > 0) {
    checkOperation(state);
    if (expectedRootIdentity) {
      await validateIdentity(root, expectedRootIdentity, "isolated_workspace_drift", "The repository root changed identity during capture.");
    }
    const { directory, identity } = pending.pop();
    let before;
    try {
      before = await fs.lstat(directory, { bigint: true });
      const canonical = await fs.realpath(directory);
      if (!before.isDirectory() || before.isSymbolicLink() || !sameStat(identity, before) || canonical !== directory || !isContained(root, canonical)) {
        throw brokerError("isolated_workspace_drift", "A repository directory changed identity during the safety scan.");
      }
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw brokerError("isolated_workspace_drift", "A repository directory changed identity during the safety scan.");
    }
    let names;
    try { names = await fs.readdir(directory); }
    catch { throw brokerError("isolated_workspace_snapshot_failed", "A repository directory could not be scanned safely."); }
    for (const name of names) {
      checkOperation(state);
      if (directory === root && name === ".git") continue;
      inspected += 1;
      if (inspected > limits.maxFiles * 2) {
        throw brokerError("isolated_workspace_limit", "The repository filesystem exceeded the bounded safety-scan limit.");
      }
      const candidate = path.join(directory, name);
      let stat;
      try { stat = await fs.lstat(candidate, { bigint: true }); }
      catch { throw brokerError("isolated_workspace_drift", "A repository entry changed during the safety scan."); }
      if (stat.isDirectory() && !stat.isSymbolicLink()) {
        const identity = statIdentity(stat);
        directoryCache.set(candidate, identity);
        pending.push({ directory: candidate, identity });
      }
      else if (stat.isFile()) {
        if (enforceAllFileBytes) {
          if (stat.size > BigInt(limits.maxFileBytes)) {
            throw brokerError("isolated_workspace_limit", "A regular workspace file, including ignored content, exceeded the per-file result limit.");
          }
          regularBytes += Number(stat.size);
          if (!Number.isSafeInteger(regularBytes) || regularBytes > limits.maxTotalBytes) {
            throw brokerError("isolated_workspace_limit", "Regular workspace files, including ignored content, exceeded the total result limit.");
          }
        }
      }
      else if (!stat.isSymbolicLink()) {
        throw brokerError("isolated_workspace_special_file", "The repository filesystem contains an unsupported special file.");
      }
    }
    const after = await fs.lstat(directory, { bigint: true });
    if (!sameStat(before, after)) throw brokerError("isolated_workspace_drift", "A repository directory changed during the safety scan.");
  }
  return directoryCache;
}

async function availableFilesystemBytes(state) {
  checkOperation(state);
  let stats;
  try { stats = await fs.statfs(state.jobRoot, { bigint: true }); }
  catch { throw brokerError("isolated_workspace_disk_policy", "Available filesystem capacity could not be verified safely."); }
  const available = stats.bavail * stats.bsize;
  if (available < 0n) throw brokerError("isolated_workspace_disk_policy", "Available filesystem capacity was invalid.");
  return available;
}

async function requireFreeSpace(state, payloadBytes) {
  if (!Number.isSafeInteger(payloadBytes) || payloadBytes < 0) throw brokerError("isolated_workspace_disk_policy", "The isolated workspace disk estimate was invalid.");
  if (payloadBytes > state.limits.maxJobBytes) throw brokerError("isolated_workspace_disk_policy", "The isolated workspace estimate exceeded its bounded job envelope.");
  const required = BigInt(payloadBytes) + BigInt(FREE_SPACE_RESERVE_BYTES);
  if (await availableFilesystemBytes(state) < required) {
    throw brokerError("isolated_workspace_disk_policy", "The filesystem did not have enough verified free space for a bounded isolated clone.");
  }
}

async function inspectRegularFileNoFollow(state, file, maxBytes, code, message) {
  checkOperation(state);
  let handle;
  try {
    handle = await fs.open(file, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
    const before = await handle.stat({ bigint: true });
    if (!before.isFile() || before.size > BigInt(maxBytes)) throw brokerError(code, message);
    const after = await fs.lstat(file, { bigint: true });
    if (!sameStat(before, after)) throw brokerError(code, message);
    return { size: Number(before.size), identity: statIdentity(before) };
  } catch (error) {
    if (error instanceof BrokerError) throw error;
    throw brokerError(code, message);
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function measureDirectoryNoFollow(state, root, { maxBytes, maxEntries, allowSymlinks }) {
  const rootStat = await fs.lstat(root, { bigint: true });
  if (!rootStat.isDirectory() || rootStat.isSymbolicLink()) throw brokerError("isolated_workspace_disk_policy", "A measured isolated directory changed identity.");
  const pending = [{ directory: root, identity: statIdentity(rootStat) }];
  let bytes = 0;
  let entries = 0;
  while (pending.length > 0) {
    checkOperation(state);
    const { directory, identity } = pending.pop();
    const before = await fs.lstat(directory, { bigint: true });
    if (!before.isDirectory() || before.isSymbolicLink() || !sameStat(identity, before)) throw brokerError("isolated_workspace_disk_policy", "A measured isolated directory changed during inspection.");
    const names = await fs.readdir(directory);
    for (const name of names) {
      checkOperation(state);
      entries += 1;
      if (entries > maxEntries) throw brokerError("isolated_workspace_disk_policy", "The isolated clone exceeded its bounded filesystem entry count.");
      const candidate = path.join(directory, name);
      const stat = await fs.lstat(candidate, { bigint: true });
      if (stat.isDirectory() && !stat.isSymbolicLink()) pending.push({ directory: candidate, identity: statIdentity(stat) });
      else if (stat.isFile()) {
        if (stat.size > BigInt(maxBytes)) throw brokerError("isolated_workspace_disk_policy", "The isolated clone contained an oversized file.");
        bytes += Number(stat.size);
      }
      else if (stat.isSymbolicLink() && allowSymlinks) bytes += (await fs.readlink(candidate, { encoding: "buffer" })).length;
      else throw brokerError("isolated_workspace_disk_policy", "The isolated clone contained a disallowed special file or symlink.");
      if (!Number.isSafeInteger(bytes) || bytes > maxBytes) throw brokerError("isolated_workspace_disk_policy", "The isolated clone exceeded its bounded disk envelope.");
    }
    const after = await fs.lstat(directory, { bigint: true });
    if (!sameStat(before, after)) throw brokerError("isolated_workspace_disk_policy", "A measured isolated directory changed during inspection.");
  }
  return { bytes, entries };
}

async function captureRepository(state, repository, storageRoot = null, expectedRootIdentity = null, storageRootIdentity = null) {
  if (expectedRootIdentity) {
    await validateIdentity(repository, expectedRootIdentity, "isolated_workspace_drift", "The repository root changed identity during capture.");
  }
  const metadata = await captureMetadata(state, repository);
  const pathsResult = await runGit(state, repository, ["ls-files", "--cached", "--others", "--exclude-standard", "--deduplicate", "-z"], {
    maxStdoutBytes: 32 * 1024 * 1024,
  });
  const paths = parseNulPaths(pathsResult.stdout, state.limits);
  const directoryCache = await scanRepositoryDirectories(
    state,
    repository,
    state.limits,
    expectedRootIdentity,
    repository === state.workspaceRoot,
  );
  if (storageRoot) {
    if (storageRootIdentity) {
      await validateIdentity(storageRoot, storageRootIdentity, "isolated_workspace_drift", "The private capture directory changed identity.");
    } else await fs.mkdir(storageRoot, { mode: 0o700 });
  }
  const budget = { remaining: state.limits.maxTotalBytes, total: 0 };
  const entries = [];
  for (let index = 0; index < paths.length; index += 1) {
    if (expectedRootIdentity) {
      await validateIdentity(repository, expectedRootIdentity, "isolated_workspace_drift", "The repository root changed identity during capture.");
    }
    entries.push(await captureEntry(state, repository, paths[index], storageRoot, index, budget, state.limits, directoryCache));
  }
  if (expectedRootIdentity) {
    await validateIdentity(repository, expectedRootIdentity, "isolated_workspace_drift", "The repository root changed identity during capture.");
  }
  validateRepresentedSymlinkTargets(state, entries);
  return {
    metadata,
    entries,
    fileCount: entries.filter((entry) => entry.type !== "missing").length,
    totalBytes: budget.total,
    fingerprint: fingerprintState(metadata, entries, budget.total),
  };
}

function insertTreePath(root, entry, oid) {
  const segments = entry.path.split("/");
  let current = root;
  for (let index = 0; index < segments.length - 1; index += 1) {
    const segment = segments[index];
    const existing = current.get(segment);
    if (existing?.kind === "blob") throw brokerError("isolated_workspace_unsafe_path", "Workspace paths had an unsafe file-directory collision.");
    if (!existing) current.set(segment, { kind: "tree", children: new Map() });
    current = current.get(segment).children;
  }
  const leaf = segments.at(-1);
  if (current.has(leaf)) throw brokerError("isolated_workspace_unsafe_path", "Workspace paths had an unsafe collision.");
  current.set(leaf, { kind: "blob", mode: entry.gitMode, oid });
}

async function writeTreeNode(state, repository, children) {
  checkOperation(state);
  const records = [];
  const names = [...children.keys()].sort((left, right) => Buffer.compare(Buffer.from(left), Buffer.from(right)));
  for (const name of names) {
    checkOperation(state);
    const child = children.get(name);
    if (child.kind === "tree") {
      const oid = await writeTreeNode(state, repository, child.children);
      records.push(Buffer.from(`040000 tree ${oid}\t${name}\0`));
    } else records.push(Buffer.from(`${child.mode} blob ${child.oid}\t${name}\0`));
  }
  const result = await runGit(state, repository, ["mktree", "-z"], { input: Buffer.concat(records), maxStdoutBytes: 80 });
  const oid = result.stdout.toString("ascii").trim();
  if (!/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(oid)) throw brokerError("isolated_workspace_git_failed", "Git returned an invalid tree object ID.");
  return oid;
}

async function writeSnapshotTree(state, repository, snapshot) {
  const root = new Map();
  const entries = snapshot.entries.filter((entry) => entry.type !== "missing");
  for (let offset = 0; offset < entries.length; offset += 128) {
    checkOperation(state);
    const chunk = entries.slice(offset, offset + 128);
    const result = await runGit(state, repository, ["hash-object", "-w", "--no-filters", ...chunk.map((entry) => entry.storageFile)], {
      maxStdoutBytes: chunk.length * 80,
    });
    const objectIds = result.stdout.toString("ascii").trim().split("\n").filter(Boolean);
    if (objectIds.length !== chunk.length || objectIds.some((oid) => !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(oid))) {
      throw brokerError("isolated_workspace_git_failed", "Git returned an invalid batch of blob object IDs.");
    }
    for (let index = 0; index < chunk.length; index += 1) insertTreePath(root, chunk[index], objectIds[index]);
  }
  return writeTreeNode(state, repository, root);
}

async function materializeSnapshot(state, workspace, snapshot) {
  for (const entry of snapshot.entries) {
    checkOperation(state);
    if (entry.type === "missing") continue;
    const destination = path.join(workspace, ...entry.path.split("/"));
    if (!isContained(workspace, destination) || destination === workspace) throw brokerError("isolated_workspace_unsafe_path", "A captured path escaped the isolated workspace.");
    await fs.mkdir(path.dirname(destination), { recursive: true, mode: 0o755 });
    if (entry.type === "file") {
      await fs.copyFile(entry.storageFile, destination, fsConstants.COPYFILE_EXCL);
      await fs.chmod(destination, entry.materializedMode);
    } else {
      const target = await fs.readFile(entry.storageFile);
      await fs.symlink(target, destination);
    }
  }
}

function safeConfigText(objectFormat, hooksRoot) {
  const quote = (value) => JSON.stringify(value);
  const format = objectFormat === "sha256" ? "1" : "0";
  const extension = objectFormat === "sha256" ? `\n[extensions]\n\tobjectFormat = sha256` : "";
  return `[core]\n\trepositoryformatversion = ${format}\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n\thooksPath = ${quote(hooksRoot)}\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n[protocol]\n\tallow = never\n[credential]\n\thelper =\n${extension}\n`;
}

async function assertNoRemoteOrAlternates(state) {
  const remotes = await runGit(state, state.workspaceRoot, ["remote"], { maxStdoutBytes: 16 * 1024 });
  if (remotes.stdout.length !== 0) throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone retained a Git remote.");
  for (const name of ["alternates", "http-alternates"]) {
    try {
      const candidate = path.join(state.gitDir, "objects", "info", name);
      await fs.lstat(candidate);
      throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone retained an object alternate.");
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      if (error?.code !== "ENOENT") throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone object store could not be verified.");
    }
  }
  const hooks = await fs.readdir(state.hooksRoot);
  if (hooks.length !== 0) throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone hooks directory was not empty.");
}

async function assertNoAlternateFiles(state) {
  for (const name of ["alternates", "http-alternates"]) {
    const candidate = path.join(state.gitDir, "objects", "info", name);
    try {
      await fs.lstat(candidate);
      throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone object store contained an alternate.");
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      if (error?.code !== "ENOENT") throw brokerError("isolated_workspace_clone_unsafe", "The isolated clone object store could not be verified.");
    }
  }
}

async function neutralizeIsolatedGitConfig(state) {
  await validateIdentity(state.gitDir, state.gitDirIdentity, "isolated_workspace_owner_mismatch", "The isolated Git directory changed identity.");
  await validateIdentity(state.objectsDir, state.objectsDirIdentity, "isolated_workspace_owner_mismatch", "The isolated object directory changed identity.");
  await assertNoAlternateFiles(state);
  const config = path.join(state.gitDir, "config");
  let configStat;
  try { configStat = await fs.lstat(config, { bigint: true }); }
  catch { throw brokerError("isolated_workspace_git_config", "The isolated Git config was missing before finalization."); }
  if (!configStat.isFile() || configStat.isSymbolicLink() || configStat.size > 1024n * 1024n) {
    throw brokerError("isolated_workspace_git_config", "The isolated Git config was not a bounded regular file.");
  }
  const quarantineRoot = path.join(state.jobRoot, "quarantined-git-configs");
  await fs.mkdir(quarantineRoot, { recursive: true, mode: 0o700 });
  const quarantined = path.join(quarantineRoot, `${crypto.randomUUID()}.config`);
  await fs.rename(config, quarantined);
  let handle;
  try {
    handle = await fs.open(config, fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL, 0o600);
    await handle.writeFile(safeConfigText(state.sourceSnapshot.metadata.objectFormat, state.hooksRoot));
    await handle.sync();
  } catch {
    throw brokerError("isolated_workspace_git_config", "The isolated Git config could not be replaced with broker-controlled settings.");
  } finally {
    await handle?.close().catch(() => {});
  }
}

function handlePublic(state) {
  const handle = Object.freeze({
    schemaVersion: 1,
    jobId: state.jobId,
    workspaceRoot: state.workspaceRoot,
    originalHead: state.sourceSnapshot.metadata.head,
    baselineCommit: state.baselineCommit,
    sourceFingerprint: state.sourceSnapshot.fingerprint,
    sourceFileCount: state.sourceSnapshot.fileCount,
    sourceBytes: state.sourceSnapshot.totalBytes,
    executionEnvironment: Object.freeze({ ...state.gitEnv }),
  });
  HANDLE_STATE.set(handle, state);
  return handle;
}

function requireHandle(handle) {
  const state = HANDLE_STATE.get(handle);
  if (!state) throw brokerError("isolated_workspace_handle", "The isolated workspace handle was invalid.");
  if (state.cleaned) throw brokerError("isolated_workspace_cleaned", "The isolated workspace was already cleaned up.");
  return state;
}

async function validateIdentity(candidate, expected, code, message) {
  let current;
  try { current = await fs.lstat(candidate, { bigint: true }); }
  catch { throw brokerError(code, message); }
  if (!current.isDirectory() || current.isSymbolicLink() || !sameObject(expected, current)) throw brokerError(code, message);
  return current;
}

async function validateMarker(state, root = state.jobRoot) {
  const markerPath = path.join(root, ".witself-owner");
  let handle;
  try {
    handle = await fs.open(markerPath, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
    const opened = await handle.stat({ bigint: true });
    const content = await handle.readFile("utf8");
    const after = await handle.stat({ bigint: true });
    if (!opened.isFile() || !sameStat(opened, after) || content !== state.markerToken) throw new Error("marker mismatch");
  } catch {
    throw brokerError("isolated_workspace_owner_mismatch", "The broker-owned isolated job marker no longer matched.");
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function validateOwnedJob(state, includeWorkspace = true) {
  await validateIdentity(state.jobsRoot, state.jobsRootIdentity, "isolated_workspace_owner_mismatch", "The broker job root changed identity.");
  if (path.dirname(state.jobRoot) !== state.jobsRoot || !JOB_NAME_RE.test(path.basename(state.jobRoot))) {
    throw brokerError("isolated_workspace_owner_mismatch", "The isolated job path was no longer an exact broker-owned child.");
  }
  await validateIdentity(state.jobRoot, state.jobRootIdentity, "isolated_workspace_owner_mismatch", "The broker-owned isolated job changed identity.");
  await validateMarker(state);
  if (includeWorkspace) {
    await validateIdentity(state.workspaceRoot, state.workspaceIdentity, "isolated_workspace_owner_mismatch", "The isolated workspace changed identity.");
    await validateIdentity(state.gitDir, state.gitDirIdentity, "isolated_workspace_owner_mismatch", "The isolated Git directory changed identity.");
    await validateIdentity(state.objectsDir, state.objectsDirIdentity, "isolated_workspace_owner_mismatch", "The isolated object directory changed identity.");
  }
}

async function writePrivateFileExclusive(file, data) {
  let handle;
  try {
    handle = await fs.open(file, fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL, 0o600);
    await handle.writeFile(data);
    await handle.sync();
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function removeExactOwnedJob(state, requireMarker) {
  await validateIdentity(state.jobsRoot, state.jobsRootIdentity, "isolated_workspace_owner_mismatch", "The broker job root changed identity.");
  if (path.dirname(state.jobRoot) !== state.jobsRoot || !JOB_NAME_RE.test(path.basename(state.jobRoot))) {
    throw brokerError("isolated_workspace_owner_mismatch", "The isolated job path was no longer an exact broker-owned child.");
  }
  await validateIdentity(state.jobRoot, state.jobRootIdentity, "isolated_workspace_owner_mismatch", "The broker-owned isolated job changed identity.");
  if (requireMarker) await validateMarker(state);
  const quarantine = path.join(state.jobsRoot, `.cleanup-${state.jobId}-${crypto.randomBytes(12).toString("hex")}`);
  if (path.dirname(quarantine) !== state.jobsRoot) throw brokerError("isolated_workspace_owner_mismatch", "The cleanup quarantine path was unsafe.");
  await fs.rename(state.jobRoot, quarantine);
  try {
    await validateIdentity(quarantine, state.jobRootIdentity, "isolated_workspace_owner_mismatch", "The quarantined isolated job changed identity.");
    if (requireMarker) await validateMarker(state, quarantine);
    await validateIdentity(state.jobsRoot, state.jobsRootIdentity, "isolated_workspace_owner_mismatch", "The broker job root changed identity.");
    await fs.rm(quarantine, { recursive: true, force: false, maxRetries: 1 });
  } catch (error) {
    try { await fs.rename(quarantine, state.jobRoot); } catch { /* Leave the uncertain quarantined path untouched. */ }
    throw error;
  }
}

async function removeExactResultCapture(state, captureRoot, expected) {
  if (path.dirname(captureRoot) !== state.jobRoot ||
      !/^result-capture-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(path.basename(captureRoot))) {
    throw brokerError("isolated_workspace_capture_cleanup", "A result capture cleanup path was unsafe.");
  }
  await validateOwnedJob(state, false);
  let current;
  try { current = await fs.lstat(captureRoot, { bigint: true }); }
  catch { throw brokerError("isolated_workspace_capture_cleanup", "A result capture disappeared before cleanup."); }
  if (!current.isDirectory() || current.isSymbolicLink() || current.dev !== state.jobRootIdentity.dev || !sameObject(expected, current)) {
    throw brokerError("isolated_workspace_capture_cleanup", "A result capture changed identity before cleanup.");
  }
  const quarantine = path.join(state.jobRoot, `.discard-${state.jobId}-${crypto.randomBytes(12).toString("hex")}`);
  let renamed = false;
  try {
    await fs.rename(captureRoot, quarantine);
    renamed = true;
    const moved = await fs.lstat(quarantine, { bigint: true });
    if (!sameObject(expected, moved)) throw brokerError("isolated_workspace_capture_cleanup", "A result capture changed identity during cleanup.");
    await fs.rm(quarantine, { recursive: true, force: false, maxRetries: 1 });
  } catch (error) {
    if (renamed) {
      try { await fs.rename(quarantine, captureRoot); } catch { /* Preserve an uncertain quarantine rather than following another path. */ }
    }
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_capture_cleanup", "A result capture could not be removed safely.");
  }
}

export async function createIsolatedWorkspace(options = {}) {
  const limits = normalizeLimits(options.limits);
  const source = await assertCanonicalDirectory(options.sourceRoot, "isolated_workspace_invalid", "The source repository root was unavailable.");
  await fs.mkdir(options.jobsRoot, { recursive: true, mode: 0o700 });
  const jobs = await assertCanonicalDirectory(options.jobsRoot, "isolated_workspace_invalid", "The broker job root was unavailable.");
  if ((jobs.stat.mode & 0o077n) !== 0n || (typeof process.getuid === "function" && jobs.stat.uid !== BigInt(process.getuid()))) {
    throw brokerError("isolated_workspace_owner_mismatch", "The broker job root was not private to the current operating-system user.");
  }
  if (isContained(source.canonical, jobs.canonical) || isContained(jobs.canonical, source.canonical)) {
    throw brokerError("isolated_workspace_invalid", "The broker job root and source repository must be disjoint.");
  }

  const jobId = crypto.randomUUID();
  const jobRoot = path.join(jobs.canonical, `implementation-${jobId}`);
  const home = path.join(jobRoot, "home");
  const temp = path.join(jobRoot, "tmp");
  const hooksRoot = path.join(jobRoot, "empty-hooks");
  const captureRoot = path.join(jobRoot, "source-capture");
  const templateRoot = path.join(jobRoot, "empty-template");
  const workspaceRoot = path.join(jobRoot, "workspace");
  const artifactsRoot = path.join(jobRoot, "artifacts");
  const bundlePath = path.join(jobRoot, "source.bundle");
  const markerToken = `${jobId}:${crypto.randomBytes(32).toString("hex")}\n`;
  let state;
  try {
    await fs.mkdir(jobRoot, { mode: 0o700 });
    const jobStat = await fs.lstat(jobRoot, { bigint: true });
    state = {
      jobId,
      jobRoot,
      jobRootIdentity: statIdentity(jobStat),
      jobsRoot: jobs.canonical,
      jobsRootIdentity: statIdentity(jobs.stat),
      sourceRoot: source.canonical,
      sourceRootIdentity: statIdentity(source.stat),
      workspaceRoot,
      artifactsRoot,
      hooksRoot,
      markerToken,
      markerCreated: false,
      git: gitExecutable(options.gitCommand),
      gitEnv: privateGitEnv(home, temp),
      limits,
      finalized: null,
      compacted: false,
      cleaned: false,
      operation: null,
    };
    if (!jobStat.isDirectory() || jobStat.isSymbolicLink() || (jobStat.mode & 0o077n) !== 0n ||
        (typeof process.getuid === "function" && jobStat.uid !== BigInt(process.getuid()))) {
      throw brokerError("isolated_workspace_owner_mismatch", "The broker-owned isolated job was not a private directory.");
    }
    beginOperation(state, options);
    await requireFreeSpace(state, state.limits.maxTotalBytes + 64 * 1024 * 1024);
    for (const directory of [home, path.join(home, ".config"), temp, hooksRoot, templateRoot, artifactsRoot]) {
      await fs.mkdir(directory, { recursive: true, mode: 0o700 });
    }
    state.artifactsRootIdentity = statIdentity(await fs.lstat(artifactsRoot, { bigint: true }));
    await writePrivateFileExclusive(path.join(jobRoot, ".witself-owner"), markerToken);
    state.markerCreated = true;
    await syncDirectory(jobRoot);

    state.sourceSnapshot = await captureRepository(state, source.canonical, captureRoot, state.sourceRootIdentity);
    const history = state.sourceSnapshot.metadata.history;
    await requireFreeSpace(state, history.totalBytes * 2 + state.sourceSnapshot.totalBytes * 2 + 64 * 1024 * 1024);
    await runGit(state, source.canonical, ["bundle", "create", bundlePath, "--all", "HEAD"], {
      timeoutMs: 120_000,
      maxStdoutBytes: 64 * 1024,
      errorCode: "isolated_workspace_bundle_failed",
      errorMessage: "Git could not create a complete private history bundle.",
    });
    const bundleBefore = await inspectRegularFileNoFollow(
      state, bundlePath, state.limits.maxBundleBytes,
      "isolated_workspace_history_limit", "The private Git bundle exceeded its bounded disk envelope.",
    );
    await runGit(state, source.canonical, ["bundle", "verify", bundlePath], {
      timeoutMs: 120_000,
      maxStdoutBytes: 4 * 1024 * 1024,
      errorCode: "isolated_workspace_bundle_failed",
      errorMessage: "The private Git history bundle did not verify.",
    });
    const bundleAfter = await inspectRegularFileNoFollow(
      state, bundlePath, state.limits.maxBundleBytes,
      "isolated_workspace_bundle_failed", "The private Git bundle changed identity during verification.",
    );
    if (!sameStat(bundleBefore.identity, bundleAfter.identity)) throw brokerError("isolated_workspace_bundle_failed", "The private Git bundle changed identity during verification.");
    await requireFreeSpace(state, bundleAfter.size + Math.min(history.totalBytes, state.limits.maxCloneBytes) + state.sourceSnapshot.totalBytes * 2);
    const cloneArgs = gitInvocation(null, hooksRoot, [
      "clone", "--no-hardlinks", "--no-local", "--no-checkout", `--template=${templateRoot}`,
      bundlePath, workspaceRoot,
    ], [["protocol.file.allow", "always"]]);
    const clone = await runStateProcess(state, state.git, cloneArgs, {
      cwd: jobRoot,
      env: state.gitEnv,
      timeoutMs: 120_000,
      maxStdoutBytes: 4 * 1024 * 1024,
      maxStderrBytes: 4 * 1024 * 1024,
    });
    if (clone.code !== 0) throw brokerError("isolated_workspace_clone_failed", "The private Git bundle could not be cloned safely.");
    const workspace = await assertCanonicalDirectory(workspaceRoot, "isolated_workspace_clone_failed", "The isolated clone root was unavailable.");
    const gitDirPath = path.join(workspaceRoot, ".git");
    const gitDir = await assertCanonicalDirectory(gitDirPath, "isolated_workspace_clone_failed", "The isolated clone Git directory was unavailable.");
    state.workspaceIdentity = statIdentity(workspace.stat);
    state.gitDir = gitDir.canonical;
    state.gitDirIdentity = statIdentity(gitDir.stat);
    const objects = await assertCanonicalDirectory(path.join(state.gitDir, "objects"), "isolated_workspace_clone_failed", "The isolated object directory was unavailable.");
    state.objectsDir = objects.canonical;
    state.objectsDirIdentity = statIdentity(objects.stat);
    await measureDirectoryNoFollow(state, state.gitDir, {
      maxBytes: state.limits.maxCloneBytes,
      maxEntries: state.limits.maxHistoryObjects * 3 + 10_000,
      allowSymlinks: false,
    });
    await fs.writeFile(path.join(state.gitDir, "config"), safeConfigText(state.sourceSnapshot.metadata.objectFormat, hooksRoot), { mode: 0o600 });
    await assertNoRemoteOrAlternates(state);
    await runGit(state, workspaceRoot, ["cat-file", "-e", `${state.sourceSnapshot.metadata.head}^{commit}`], {
      maxStdoutBytes: 32,
      errorCode: "isolated_workspace_clone_failed",
      errorMessage: "The isolated clone did not contain the source HEAD commit.",
    });

    state.baselineTree = await writeSnapshotTree(state, workspaceRoot, state.sourceSnapshot);
    const commitEnv = {
      ...state.gitEnv,
      GIT_AUTHOR_NAME: "Witself Isolated Broker",
      GIT_AUTHOR_EMAIL: "isolated-broker@invalid",
      GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z",
      GIT_COMMITTER_NAME: "Witself Isolated Broker",
      GIT_COMMITTER_EMAIL: "isolated-broker@invalid",
      GIT_COMMITTER_DATE: "2000-01-01T00:00:00Z",
    };
    const commitResult = await runStateProcess(state, state.git, gitInvocation(workspaceRoot, hooksRoot, [
      "commit-tree", state.baselineTree, "-p", state.sourceSnapshot.metadata.head,
    ]), {
      cwd: workspaceRoot,
      env: commitEnv,
      input: Buffer.from("Witself isolated implementation baseline\n"),
      maxStdoutBytes: 80,
      maxStderrBytes: PROCESS_STDERR_BYTES,
    });
    if (commitResult.code !== 0) throw brokerError("isolated_workspace_git_failed", "The synthetic isolated baseline commit could not be created.");
    state.baselineCommit = commitResult.stdout.toString("ascii").trim();
    if (!/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(state.baselineCommit)) throw brokerError("isolated_workspace_git_failed", "Git returned an invalid baseline commit object ID.");
    await runGit(state, workspaceRoot, ["update-ref", "refs/heads/witself-broker-baseline", state.baselineCommit]);
    await runGit(state, workspaceRoot, ["symbolic-ref", "HEAD", "refs/heads/witself-broker-baseline"]);
    await runGit(state, workspaceRoot, ["read-tree", state.baselineTree]);
    await materializeSnapshot(state, workspaceRoot, state.sourceSnapshot);
    const clean = await runGit(state, workspaceRoot, ["status", "--porcelain=v2", "-z", "--untracked-files=all"], { maxStdoutBytes: 4 * 1024 * 1024 });
    if (clean.stdout.length !== 0) throw brokerError("isolated_workspace_materialize_failed", "The exact captured source did not materialize as a clean synthetic baseline.");
    await assertNoRemoteOrAlternates(state);
    await measureDirectoryNoFollow(state, state.jobRoot, {
      maxBytes: state.limits.maxJobBytes,
      maxEntries: state.limits.maxHistoryObjects * 3 + state.limits.maxFiles * 4 + 20_000,
      allowSymlinks: true,
    });
    await fs.rm(bundlePath, { force: false });

    const postCapture = await captureRepository(state, source.canonical, null, state.sourceRootIdentity);
    if (postCapture.fingerprint !== state.sourceSnapshot.fingerprint) {
      throw brokerError("isolated_workspace_drift", "The source repository changed while the isolated workspace was being captured.");
    }
    const handle = handlePublic(state);
    endOperation(state);
    return handle;
  } catch (error) {
    if (state?.operation) endOperation(state);
    if (state?.jobRootIdentity) {
      try {
        await removeExactOwnedJob(state, state.markerCreated);
      } catch {
        // Preserve an ownership-mismatched job for manual inspection instead of deleting an uncertain path.
      }
    }
    if (error instanceof BrokerError) throw error;
    const wrapped = brokerError("isolated_workspace_failed", "The broker could not create an isolated implementation workspace.");
    wrapped.cause = error;
    throw wrapped;
  }
}

function changedEntryMap(snapshot) {
  return new Map(snapshot.entries.filter((entry) => entry.type !== "missing").map((entry) => [entry.path, entry]));
}

async function validateChangedSymlinks(state, changedPaths, resultSnapshot) {
  const result = changedEntryMap(resultSnapshot);
  for (const relative of changedPaths) {
    const entry = result.get(relative);
    if (entry?.type !== "symlink") continue;
    const targetBytes = await fs.readFile(entry.storageFile);
    validateSymlinkTarget(state.workspaceRoot, relative, targetBytes);
  }
}

async function syncDirectory(directory) {
  let handle;
  try {
    handle = await fs.open(directory, fsConstants.O_RDONLY);
    await handle.sync();
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function stageArtifact(state, stageRoot, name, data, mediaType) {
  const file = path.join(stageRoot, name);
  let handle;
  try {
    handle = await fs.open(file, fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_EXCL, 0o600);
    await handle.writeFile(data);
    await handle.sync();
    const stat = await handle.stat({ bigint: true });
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size !== BigInt(data.length)) throw new Error("artifact mismatch");
    return {
      id: name,
      mediaType,
      sizeBytes: data.length,
      sha256: sha256(data),
      maxChunkBytes: state.limits.maxArtifactChunkBytes,
      identity: statIdentity(stat),
    };
  } catch {
    throw brokerError("isolated_workspace_artifact_failed", "A private implementation artifact could not be staged durably.");
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function publishArtifacts(state, definitions) {
  const stageRoot = path.join(state.artifactsRoot, `.stage-${crypto.randomUUID()}`);
  const publishedRoot = path.join(state.artifactsRoot, "published");
  let renamed = false;
  try {
    await validateIdentity(state.artifactsRoot, state.artifactsRootIdentity, "isolated_workspace_artifact_failed", "The private artifact directory changed identity.");
    await fs.mkdir(stageRoot, { mode: 0o700 });
    const artifacts = [];
    for (const definition of definitions) {
      artifacts.push(await stageArtifact(state, stageRoot, definition.name, definition.data, definition.mediaType));
    }
    await syncDirectory(stageRoot);
    await fs.rename(stageRoot, publishedRoot);
    renamed = true;
    await syncDirectory(state.artifactsRoot);
    const published = await fs.lstat(publishedRoot, { bigint: true });
    if (!published.isDirectory() || published.isSymbolicLink()) throw brokerError("isolated_workspace_artifact_failed", "The published artifact directory was not a private directory.");
    state.publishedRootIdentity = statIdentity(published);
    return artifacts.map((artifact) => Object.freeze({ ...artifact, file: path.join(publishedRoot, artifact.id) }));
  } catch (error) {
    state.publishedRootIdentity = null;
    const target = renamed ? publishedRoot : stageRoot;
    try { await fs.rm(target, { recursive: true, force: false, maxRetries: 1 }); } catch { /* Preserve the original publication error. */ }
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_artifact_failed", "The private implementation artifacts could not be published atomically.");
  }
}

function publicArtifact(artifact) {
  return Object.freeze({
    id: artifact.id,
    mediaType: artifact.mediaType,
    sizeBytes: artifact.sizeBytes,
    sha256: artifact.sha256,
    maxChunkBytes: artifact.maxChunkBytes,
  });
}

function encodeEvidence(evidence) {
  if (evidence === undefined || evidence === null) return Buffer.from("{}");
  if (Buffer.isBuffer(evidence)) return evidence;
  if (typeof evidence === "string") return Buffer.from(evidence);
  try { return Buffer.from(`${JSON.stringify(evidence)}\n`); }
  catch { throw brokerError("isolated_workspace_evidence_invalid", "Implementation evidence could not be encoded as JSON."); }
}

async function sourceDiverged(state) {
  try {
    const current = await captureRepository(state, state.sourceRoot, null, state.sourceRootIdentity);
    return current.fingerprint !== state.sourceSnapshot.fingerprint;
  } catch (error) {
    if (error?.code === "isolated_workspace_aborted" || error?.code === "isolated_workspace_deadline") throw error;
    return true;
  }
}

export async function finalizeIsolatedWorkspace(handle, options = {}) {
  const state = requireHandle(handle);
  if (state.finalized) return state.finalized.publicResult;
  beginOperation(state, options);
  let resultCaptureRoot = null;
  let resultCaptureIdentity = null;
  try {
    await validateOwnedJob(state);
    await neutralizeIsolatedGitConfig(state);
    resultCaptureRoot = path.join(state.jobRoot, `result-capture-${crypto.randomUUID()}`);
    await fs.mkdir(resultCaptureRoot, { mode: 0o700 });
    const captureStat = await fs.lstat(resultCaptureRoot, { bigint: true });
    if (!captureStat.isDirectory() || captureStat.isSymbolicLink() || captureStat.dev !== state.jobRootIdentity.dev || (captureStat.mode & 0o077n) !== 0n) {
      throw brokerError("isolated_workspace_capture_cleanup", "The private result capture was not a safe directory.");
    }
    resultCaptureIdentity = statIdentity(captureStat);
    const resultSnapshot = await captureRepository(state, state.workspaceRoot, resultCaptureRoot, state.workspaceIdentity, resultCaptureIdentity);
    const resultTree = await writeSnapshotTree(state, state.workspaceRoot, resultSnapshot);
    const changedResult = await runGit(state, state.workspaceRoot, [
      "diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", state.baselineTree, resultTree, "--",
    ], { maxStdoutBytes: 4 * 1024 * 1024 });
    const changedPaths = parseNulPaths(changedResult.stdout, state.limits);
    if (changedPaths.length > state.limits.maxChangedFiles) {
      throw brokerError("isolated_workspace_change_limit", "The implementation changed too many files for a bounded patch.");
    }
    await validateChangedSymlinks(state, changedPaths, resultSnapshot);
    await removeExactResultCapture(state, resultCaptureRoot, resultCaptureIdentity);
    resultCaptureIdentity = null;

    const check = await runGit(state, state.workspaceRoot, [
      "diff", "--check", "--no-ext-diff", "--no-textconv", state.baselineTree, resultTree, "--",
    ], { allowCodes: [1, 2], maxStdoutBytes: 256 * 1024 });
    if (check.code !== 0) throw brokerError("isolated_workspace_diff_check", "The implementation patch failed git diff --check.");
    let patch;
    try {
      const result = await runGit(state, state.workspaceRoot, [
        "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/",
        state.baselineTree, resultTree, "--",
      ], { maxStdoutBytes: state.limits.maxPatchBytes });
      patch = result.stdout;
    } catch (error) {
      if (error?.code === "isolated_workspace_process_limit") {
        throw brokerError("isolated_workspace_patch_limit", "The binary Git patch exceeded the isolated artifact limit.");
      }
      throw error;
    }
    if (patch.length > state.limits.maxPatchBytes) throw brokerError("isolated_workspace_patch_limit", "The binary Git patch exceeded the isolated artifact limit.");
    const evidence = encodeEvidence(options.evidence);
    if (evidence.length > state.limits.maxEvidenceBytes) throw brokerError("isolated_workspace_evidence_limit", "Implementation evidence exceeded the isolated artifact limit.");
    await measureDirectoryNoFollow(state, state.jobRoot, {
      maxBytes: state.limits.maxJobBytes,
      maxEntries: state.limits.maxHistoryObjects * 3 + state.limits.maxFiles * 4 + 20_000,
      allowSymlinks: true,
    });

    const diverged = await sourceDiverged(state);
    const [patchArtifact, evidenceArtifact] = await publishArtifacts(state, [
      { name: "changes.patch", data: patch, mediaType: "text/x-diff" },
      { name: "evidence.bin", data: evidence, mediaType: "application/octet-stream" },
    ]);
    const publicResult = Object.freeze({
      schemaVersion: 1,
      jobId: state.jobId,
      originalHead: state.sourceSnapshot.metadata.head,
      baselineCommit: state.baselineCommit,
      resultTree,
      changedFiles: Object.freeze([...changedPaths]),
      changedFileCount: changedPaths.length,
      sourceDiverged: diverged,
      artifacts: Object.freeze({ patch: publicArtifact(patchArtifact), evidence: publicArtifact(evidenceArtifact) }),
    });
    state.finalized = { publicResult, artifacts: new Map([[patchArtifact.id, patchArtifact], [evidenceArtifact.id, evidenceArtifact]]) };
    return publicResult;
  } finally {
    let cleanupError = null;
    if (resultCaptureIdentity) {
      try { await removeExactResultCapture(state, resultCaptureRoot, resultCaptureIdentity); }
      catch (error) { cleanupError = error; }
    }
    endOperation(state);
    if (cleanupError) throw cleanupError;
  }
}

async function verifyPublishedArtifacts(state) {
  await validateIdentity(state.artifactsRoot, state.artifactsRootIdentity, "isolated_workspace_artifact_changed", "The private artifact directory changed identity.");
  const artifactsNames = await fs.readdir(state.artifactsRoot);
  if (artifactsNames.length !== 1 || artifactsNames[0] !== "published") {
    throw brokerError("isolated_workspace_artifact_changed", "The private artifact directory contained unexpected entries.");
  }
  const publishedRoot = path.join(state.artifactsRoot, "published");
  await validateIdentity(publishedRoot, state.publishedRootIdentity, "isolated_workspace_artifact_changed", "The published artifact directory changed identity.");
  const expectedNames = [...state.finalized.artifacts.keys()].sort();
  const publishedNames = (await fs.readdir(publishedRoot)).sort();
  if (publishedNames.length !== expectedNames.length || publishedNames.some((name, index) => name !== expectedNames[index])) {
    throw brokerError("isolated_workspace_artifact_changed", "The published artifact directory contained unexpected entries.");
  }
  let retainedBytes = Buffer.byteLength(state.markerToken);
  for (const artifact of state.finalized.artifacts.values()) {
    checkOperation(state);
    let file;
    try {
      file = await fs.open(artifact.file, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
      const opened = await file.stat({ bigint: true });
      const atPath = await fs.lstat(artifact.file, { bigint: true });
      if (!opened.isFile() || !sameStat(artifact.identity, opened) || !sameStat(opened, atPath) || opened.size !== BigInt(artifact.sizeBytes)) {
        throw new Error("artifact identity mismatch");
      }
      const digest = crypto.createHash("sha256");
      const buffer = Buffer.allocUnsafe(64 * 1024);
      let position = 0;
      while (position < artifact.sizeBytes) {
        checkOperation(state);
        const length = Math.min(buffer.length, artifact.sizeBytes - position);
        const { bytesRead } = await file.read(buffer, 0, length, position);
        if (bytesRead !== length) throw new Error("short artifact read");
        digest.update(buffer.subarray(0, bytesRead));
        position += bytesRead;
      }
      const after = await file.stat({ bigint: true });
      if (!sameStat(opened, after) || digest.digest("hex") !== artifact.sha256) throw new Error("artifact content mismatch");
      retainedBytes += artifact.sizeBytes;
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw brokerError("isolated_workspace_artifact_changed", "A published implementation artifact changed identity or content.");
    } finally {
      await file?.close().catch(() => {});
    }
  }
  return retainedBytes;
}

function isCompactionPayloadName(name) {
  return name === "home" || name === "tmp" || name === "empty-hooks" || name === "empty-template" ||
    name === "source-capture" || name === "workspace" || name === "quarantined-git-configs" || name === "source.bundle" ||
    /^result-capture-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(name);
}

async function removeCompactionQuarantine(state, name, expected) {
  const candidate = path.join(state.jobRoot, name);
  if (path.dirname(candidate) !== state.jobRoot) throw brokerError("isolated_workspace_compaction_failed", "A compaction quarantine path was unsafe.");
  let current;
  try { current = await fs.lstat(candidate, { bigint: true }); }
  catch (error) {
    if (error?.code === "ENOENT") {
      state.compactionQuarantines.delete(name);
      return;
    }
    throw brokerError("isolated_workspace_compaction_failed", "A compaction quarantine could not be inspected safely.");
  }
  if (!sameObject(expected, current) || current.dev !== state.jobRootIdentity.dev ||
      (!current.isDirectory() && !current.isFile() && !current.isSymbolicLink())) {
    throw brokerError("isolated_workspace_compaction_failed", "A compaction quarantine changed identity or type.");
  }
  try {
    await fs.rm(candidate, { recursive: current.isDirectory(), force: false, maxRetries: 1 });
  } catch {
    throw brokerError("isolated_workspace_compaction_failed", "A broker-owned compaction quarantine could not be removed.");
  }
  state.compactionQuarantines.delete(name);
}

async function compactPayloadEntry(state, name) {
  const candidate = path.join(state.jobRoot, name);
  if (path.dirname(candidate) !== state.jobRoot || !isCompactionPayloadName(name)) {
    throw brokerError("isolated_workspace_compaction_failed", "An isolated job payload path was unsafe.");
  }
  let before;
  try { before = await fs.lstat(candidate, { bigint: true }); }
  catch (error) {
    if (error?.code === "ENOENT") return;
    throw brokerError("isolated_workspace_compaction_failed", "An isolated job payload could not be inspected safely.");
  }
  if (before.dev !== state.jobRootIdentity.dev || (!before.isDirectory() && !before.isFile() && !before.isSymbolicLink())) {
    throw brokerError("isolated_workspace_compaction_failed", "An isolated job payload had an unsafe filesystem type or device.");
  }
  const quarantineName = `.compact-${state.jobId}-${crypto.randomBytes(12).toString("hex")}`;
  const quarantine = path.join(state.jobRoot, quarantineName);
  state.compactionQuarantines.set(quarantineName, statIdentity(before));
  try {
    await fs.rename(candidate, quarantine);
  } catch {
    state.compactionQuarantines.delete(quarantineName);
    throw brokerError("isolated_workspace_compaction_failed", "An isolated job payload could not be quarantined for compaction.");
  }
  let moved;
  try { moved = await fs.lstat(quarantine, { bigint: true }); }
  catch { throw brokerError("isolated_workspace_compaction_failed", "A compacted payload disappeared before identity verification."); }
  if (!sameObject(before, moved)) throw brokerError("isolated_workspace_compaction_failed", "A compacted payload changed identity during quarantine.");
  await removeCompactionQuarantine(state, quarantineName, statIdentity(before));
}

async function validateCompactedLayout(state) {
  await validateOwnedJob(state, false);
  const names = (await fs.readdir(state.jobRoot)).sort();
  if (names.length !== 2 || names[0] !== ".witself-owner" || names[1] !== "artifacts") {
    throw brokerError("isolated_workspace_compaction_failed", "The compacted isolated job retained unexpected payload entries.");
  }
  return verifyPublishedArtifacts(state);
}

function releaseCompactedPayloadState(state) {
  // Artifact reads, idempotent compaction, and cleanup need only the owner and
  // published-artifact identities. Drop the potentially 100k-entry capture and
  // large Git metadata buffers as soon as their on-disk payload is gone.
  state.sourceSnapshot = null;
  state.sourceRoot = null;
  state.sourceRootIdentity = null;
  state.workspaceIdentity = null;
  state.gitDir = null;
  state.gitDirIdentity = null;
  state.objectsDir = null;
  state.objectsDirIdentity = null;
  state.baselineTree = null;
  state.baselineCommit = null;
  state.gitEnv = null;
  state.hooksRoot = null;
  state.compactionQuarantines?.clear();
  state.compactionQuarantines = null;
}

export async function compactIsolatedWorkspace(handle, options = {}) {
  const state = requireHandle(handle);
  if (!state.finalized) throw brokerError("isolated_workspace_not_finalized", "An isolated workspace cannot be compacted before finalization.");
  beginOperation(state, options);
  try {
    if (state.compacted) {
      const retainedBytes = await validateCompactedLayout(state);
      return Object.freeze({ compacted: true, alreadyCompacted: true, retainedBytes });
    }
    await validateOwnedJob(state, false);
    await verifyPublishedArtifacts(state);
    state.compactionQuarantines ??= new Map();
    const names = await fs.readdir(state.jobRoot);
    const knownQuarantines = new Set(state.compactionQuarantines.keys());
    for (const name of names) {
      if (name === ".witself-owner" || name === "artifacts" || isCompactionPayloadName(name) || knownQuarantines.has(name)) continue;
      throw brokerError("isolated_workspace_compaction_failed", "The isolated job contained an unexpected entry and was not compacted.");
    }
    for (const [name, identity] of [...state.compactionQuarantines]) {
      checkOperation(state);
      await removeCompactionQuarantine(state, name, identity);
    }
    for (const name of names) {
      checkOperation(state);
      if (isCompactionPayloadName(name)) await compactPayloadEntry(state, name);
    }
    const retainedBytes = await validateCompactedLayout(state);
    state.compacted = true;
    state.retainedBytes = retainedBytes;
    releaseCompactedPayloadState(state);
    return Object.freeze({ compacted: true, alreadyCompacted: false, retainedBytes });
  } finally {
    endOperation(state);
  }
}

export async function readIsolatedArtifact(handle, options = {}) {
  const state = requireHandle(handle);
  await validateOwnedJob(state, false);
  if (!state.finalized) throw brokerError("isolated_workspace_not_finalized", "Implementation artifacts are unavailable before finalization.");
  const artifact = state.finalized.artifacts.get(options.artifactId);
  if (!artifact) throw brokerError("isolated_workspace_artifact_missing", "The requested implementation artifact did not exist.");
  const offset = options.offset ?? 0;
  const maxBytes = options.maxBytes ?? artifact.maxChunkBytes;
  if (!Number.isSafeInteger(offset) || offset < 0 || offset > artifact.sizeBytes || !Number.isSafeInteger(maxBytes) || maxBytes < 1 || maxBytes > artifact.maxChunkBytes) {
    throw brokerError("isolated_workspace_artifact_range", "The requested implementation artifact range was invalid.");
  }
  let file;
  try {
    file = await fs.open(artifact.file, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
    const opened = await file.stat({ bigint: true });
    if (!opened.isFile() || !sameStat(artifact.identity, opened)) throw new Error("artifact changed");
    const length = Math.min(maxBytes, artifact.sizeBytes - offset);
    const data = Buffer.alloc(length);
    if (length > 0) {
      const { bytesRead } = await file.read(data, 0, length, offset);
      if (bytesRead !== length) throw new Error("short artifact read");
    }
    const after = await file.stat({ bigint: true });
    if (!sameStat(opened, after)) throw new Error("artifact changed");
    const nextOffset = offset + length;
    return Object.freeze({
      artifactId: artifact.id,
      encoding: "base64",
      byteOffset: offset,
      nextByteOffset: nextOffset,
      eof: nextOffset === artifact.sizeBytes,
      data: data.toString("base64"),
    });
  } catch (error) {
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_artifact_changed", "The private implementation artifact changed identity or content.");
  } finally {
    await file?.close().catch(() => {});
  }
}

export async function cleanupIsolatedWorkspace(handle) {
  const state = HANDLE_STATE.get(handle);
  if (!state) throw brokerError("isolated_workspace_handle", "The isolated workspace handle was invalid.");
  if (state.cleaned) return Object.freeze({ cleaned: true, alreadyCleaned: true });
  await validateOwnedJob(state, false);
  const quarantine = path.join(state.jobsRoot, `.cleanup-${state.jobId}-${crypto.randomBytes(12).toString("hex")}`);
  if (path.dirname(quarantine) !== state.jobsRoot) throw brokerError("isolated_workspace_owner_mismatch", "The cleanup quarantine path was unsafe.");
  try { await fs.rename(state.jobRoot, quarantine); }
  catch { throw brokerError("isolated_workspace_cleanup_failed", "The broker-owned isolated job could not be quarantined for cleanup."); }
  try {
    await validateIdentity(quarantine, state.jobRootIdentity, "isolated_workspace_owner_mismatch", "The quarantined isolated job changed identity.");
    await validateMarker(state, quarantine);
    await validateIdentity(state.jobsRoot, state.jobsRootIdentity, "isolated_workspace_owner_mismatch", "The broker job root changed identity.");
    await fs.rm(quarantine, { recursive: true, force: false, maxRetries: 1 });
    state.cleaned = true;
    return Object.freeze({ cleaned: true, alreadyCleaned: false });
  } catch (error) {
    try { await fs.rename(quarantine, state.jobRoot); } catch { /* Leave the uncertain quarantined path untouched. */ }
    if (error instanceof BrokerError) throw error;
    throw brokerError("isolated_workspace_cleanup_failed", "The broker-owned isolated job could not be removed safely.");
  }
}
