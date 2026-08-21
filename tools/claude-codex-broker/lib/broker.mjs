import crypto from "node:crypto";
import { once } from "node:events";
import { constants as fsConstants } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import process from "node:process";

import { AppServerSession, ImplementationAppServerSession, probeRuntime } from "./app-server.mjs";
import { GRANT_FIELD, loadGrantAuthority } from "./grants.mjs";
import {
  BROKER_VERSION,
  JOB_TIMEOUT_MS,
  MAX_BROKER_JOBS,
  MAX_CONCURRENCY,
  MAX_RETAINED_ARTIFACT_BYTES,
  MAX_RESULT_BYTES,
  MAX_STATUS_WAIT_SECONDS,
  MAX_TASK_CHARS,
} from "./constants.mjs";
import { CodexRuntime } from "./runtime.mjs";
import { SystemAppServerSession, probeSystemRuntime } from "./system-app-server.mjs";
import {
  ISOLATED_WORKSPACE_LIMITS,
  cleanupIsolatedWorkspace,
  compactIsolatedWorkspace,
  createIsolatedWorkspace,
  finalizeIsolatedWorkspace,
  readIsolatedArtifact,
} from "./isolated-workspace.mjs";
import { BrokerError, boundedText, isContained, newId, publicError, spawnCapture } from "./util.mjs";
import { startIsolatedWorkspaceMonitor } from "./workspace-monitor.mjs";

const JOB_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const ARTIFACT_IDS = Object.freeze(["changes.patch", "evidence.bin"]);
const MAX_ARTIFACT_CHUNK_BYTES = 48 * 1024;
const MAX_SNAPSHOT_FILES = 100_000;
const MAX_SNAPSHOT_FILE_BYTES = 128 * 1024 * 1024;
const MAX_SNAPSHOT_TOTAL_BYTES = 512 * 1024 * 1024;
const MAX_SNAPSHOT_PATH_BYTES = 4096;
const MAX_REQUEST_META_BYTES = 4 * 1024;
const MAX_REQUEST_META_KEYS = 32;
const MAX_IN_FLIGHT_REQUESTS = 32;
const TERMINAL_JOB_STATES = Object.freeze(["succeeded", "failed", "cancelled"]);
const MAX_IMPLEMENTATION_ARTIFACT_BYTES = ISOLATED_WORKSPACE_LIMITS.maxPatchBytes + ISOLATED_WORKSPACE_LIMITS.maxEvidenceBytes;
const MAX_ACTIVE_IMPLEMENTATION_ENTRIES = ISOLATED_WORKSPACE_LIMITS.maxFiles * 3 + 10_000;

function requireCompletedActionReport(report, lane) {
  if (Array.isArray(report?.blockers) && report.blockers.length > 0) {
    const label = lane === "system" ? "system task" : "isolated implementation";
    throw new BrokerError(`${lane}_task_blocked`, `The delegated Codex ${label} reported blockers and did not complete.`);
  }
  if ((!Array.isArray(report?.actions) || report.actions.length === 0) &&
      (!Array.isArray(report?.checks) || report.checks.length === 0)) {
    throw new BrokerError("task_unverified", "The delegated Codex action report contained no completed action or verification check.");
  }
}

function boundedBrokerLimit(value, fallback, label) {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved < 1 || resolved > fallback) {
    throw new BrokerError("broker_limit_invalid", `The trusted ${label} limit was invalid or exceeded its hard ceiling.`);
  }
  return resolved;
}

const GRANT_SCHEMA = Object.freeze({
  type: "object",
  additionalProperties: false,
  required: ["v", "ceiling", "tool", "mode", "tool_use_id", "session_id", "issued_at_ms", "nonce", "input_sha256", "mac"],
  properties: {
    v: { const: 1 },
    ceiling: { type: "string", enum: ["isolated-write", "system"] },
    tool: { type: "string", minLength: 1, maxLength: 256 },
    mode: { type: "string", enum: ["default", "plan", "acceptEdits", "auto", "dontAsk", "bypassPermissions"] },
    tool_use_id: { type: "string", minLength: 1, maxLength: 256 },
    session_id: { type: ["string", "null"], maxLength: 256 },
    issued_at_ms: { type: "integer", minimum: 1 },
    nonce: { type: "string", minLength: 16, maxLength: 128 },
    input_sha256: { type: "string", pattern: "^[0-9a-f]{64}$" },
    mac: { type: "string", pattern: "^[A-Za-z0-9_-]{43}$" },
  },
});

const REVIEW_TOOLS = Object.freeze([
  {
    name: "codex_runtime_probe",
    description: "Reverify @openai/codex@latest against the frozen exact runtime, then attest GPT-5.6 Sol Ultra v2 and a constrained ephemeral review thread without making a model call.",
    inputSchema: { type: "object", additionalProperties: false, properties: {} },
  },
  {
    name: "codex_review_start",
    description: "Start one bounded asynchronous read-only Codex engineering review. Model, effort, cwd, permissions, environment, timeout, and output schema are broker-controlled.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["task"],
      properties: { task: { type: "string", minLength: 1, maxLength: MAX_TASK_CHARS } },
    },
  },
  {
    name: "codex_review_status",
    description: "Get bounded status and, once complete, the structured result for a Codex review job. Optionally wait up to 30 seconds for terminal status.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id"],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        wait_seconds: { type: "integer", minimum: 0, maximum: MAX_STATUS_WAIT_SECONDS },
      },
    },
  },
  {
    name: "codex_review_cancel",
    description: "Cancel a running Codex review job created by this broker process.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id"],
      properties: { job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 } },
    },
  },
]);

const IMPLEMENTATION_TOOLS = Object.freeze([
  {
    name: "codex_implementation_start",
    description: "Start one authorized asynchronous Codex implementation in a broker-owned disposable clone. The source worktree and network are denied; patch artifacts are never auto-applied.",
    annotations: { title: "Start isolated Codex implementation", readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["task", GRANT_FIELD],
      properties: {
        task: { type: "string", minLength: 1, maxLength: MAX_TASK_CHARS },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
  {
    name: "codex_implementation_status",
    description: "Read bounded status, attestation, source-divergence evidence, and artifact descriptors for an isolated implementation job, optionally waiting up to 30 seconds. Requires a fresh one-use grant.",
    annotations: { title: "Read isolated implementation status", readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id", GRANT_FIELD],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        wait_seconds: { type: "integer", minimum: 0, maximum: MAX_STATUS_WAIT_SECONDS },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
  {
    name: "codex_implementation_artifact_read",
    description: "Read one bounded base64 chunk from a finalized patch or evidence artifact retained by this broker process. Requires a fresh one-use grant.",
    annotations: { title: "Read isolated implementation artifact", readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id", "artifact_id", GRANT_FIELD],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        artifact_id: { type: "string", enum: ARTIFACT_IDS },
        offset: { type: "integer", minimum: 0 },
        max_bytes: { type: "integer", minimum: 1, maximum: MAX_ARTIFACT_CHUNK_BYTES },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
  {
    name: "codex_implementation_cancel",
    description: "Cancel an isolated implementation job. Any safely bounded partial patch is finalized and retained for broker-lifetime inspection. Requires a fresh one-use grant.",
    annotations: { title: "Cancel isolated implementation", readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id", GRANT_FIELD],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
]);

const SYSTEM_TOOLS = Object.freeze([
  {
    name: "codex_system_start",
    description: "Start one explicitly authorized asynchronous Codex task with the launcher's same-user full filesystem, process, credential, and network access. This may mutate repository and external state; all authority fields are broker-controlled.",
    annotations: { title: "Start full-access Codex system task", readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: true },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["task", GRANT_FIELD],
      properties: {
        task: { type: "string", minLength: 1, maxLength: MAX_TASK_CHARS },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
  {
    name: "codex_system_status",
    description: "Read bounded status and the final structured report for an authorized full-access system job, optionally waiting up to 30 seconds. Requires a fresh one-use launcher grant.",
    annotations: { title: "Read Codex system task status", readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id", GRANT_FIELD],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        wait_seconds: { type: "integer", minimum: 0, maximum: MAX_STATUS_WAIT_SECONDS },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
  {
    name: "codex_system_cancel",
    description: "Cancel an authorized running full-access system job. Requires a fresh one-use launcher grant.",
    annotations: { title: "Cancel Codex system task", readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["job_id", GRANT_FIELD],
      properties: {
        job_id: { type: "string", pattern: JOB_ID_RE.source, maxLength: 36 },
        [GRANT_FIELD]: GRANT_SCHEMA,
      },
    },
  },
]);

export function toolsForCeiling(ceiling) {
  if (ceiling === "repository") return REVIEW_TOOLS;
  if (ceiling === "isolated-write") return Object.freeze([...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS]);
  if (ceiling === "system") return Object.freeze([...REVIEW_TOOLS, ...IMPLEMENTATION_TOOLS, ...SYSTEM_TOOLS]);
  throw new BrokerError("invalid_ceiling", "The broker startup ceiling is invalid.");
}

const TOOLS = toolsForCeiling("repository");

function exactKeys(value, keys) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const expected = [...keys].sort();
  const actual = Object.keys(value).sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
}

function validRequestMeta(value) {
  if (value === undefined) return true;
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).length > MAX_REQUEST_META_KEYS) {
    return false;
  }
  try {
    return Buffer.byteLength(JSON.stringify(value), "utf8") <= MAX_REQUEST_META_BYTES;
  } catch {
    return false;
  }
}

function statusWaitSeconds(value) {
  if (value === undefined) return 0;
  if (!Number.isInteger(value) || value < 0 || value > MAX_STATUS_WAIT_SECONDS) {
    throw new BrokerError("invalid_wait_seconds", `wait_seconds must be an integer from 0 through ${MAX_STATUS_WAIT_SECONDS}.`);
  }
  return value;
}

function iso(time) {
  return time === null ? null : new Date(time).toISOString();
}

function sameStat(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.mode === right.mode &&
    left.size === right.size && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs;
}

function parseSnapshotPaths(raw) {
  if (raw.includes("\ufffd")) throw new BrokerError("workspace_snapshot_unsafe", "A Git path could not be represented safely.");
  const paths = raw.split("\0");
  if (paths.at(-1) === "") paths.pop();
  if (paths.length > MAX_SNAPSHOT_FILES) {
    throw new BrokerError("workspace_snapshot_limit", "The worktree contains too many tracked or nonignored untracked files for a bounded snapshot.");
  }
  const unique = new Set();
  for (const entry of paths) {
    if (!entry || Buffer.byteLength(entry) > MAX_SNAPSHOT_PATH_BYTES || path.posix.isAbsolute(entry) ||
        path.posix.normalize(entry) !== entry || entry === "." || entry === ".." || entry.startsWith("../")) {
      throw new BrokerError("workspace_snapshot_unsafe", "Git returned an unsafe worktree path.");
    }
    unique.add(entry);
  }
  if (unique.size !== paths.length) throw new BrokerError("workspace_snapshot_unsafe", "Git returned duplicate worktree paths.");
  return [...unique].sort((left, right) => Buffer.compare(Buffer.from(left), Buffer.from(right)));
}

async function hashRegularFile(file, expected, budget) {
  if (expected.size > BigInt(MAX_SNAPSHOT_FILE_BYTES) || expected.size > BigInt(budget.remaining)) {
    throw new BrokerError("workspace_snapshot_limit", "The tracked and nonignored worktree content exceeds the bounded snapshot limit.");
  }
  let handle;
  try {
    handle = await fs.open(file, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW ?? 0));
    const opened = await handle.stat({ bigint: true });
    if (!opened.isFile() || !sameStat(expected, opened)) {
      throw new BrokerError("workspace_snapshot_drift", "A worktree file changed while the snapshot was being captured.");
    }
    const digest = crypto.createHash("sha256");
    const buffer = Buffer.allocUnsafe(64 * 1024);
    let position = 0;
    while (position < Number(opened.size)) {
      const length = Math.min(buffer.length, Number(opened.size) - position);
      const { bytesRead } = await handle.read(buffer, 0, length, position);
      if (bytesRead <= 0) throw new BrokerError("workspace_snapshot_drift", "A worktree file changed while it was being read.");
      digest.update(buffer.subarray(0, bytesRead));
      position += bytesRead;
    }
    const afterHandle = await handle.stat({ bigint: true });
    const afterPath = await fs.lstat(file, { bigint: true });
    if (!sameStat(opened, afterHandle) || !sameStat(opened, afterPath)) {
      throw new BrokerError("workspace_snapshot_drift", "A worktree file changed while the snapshot was being captured.");
    }
    budget.remaining -= Number(opened.size);
    budget.total += Number(opened.size);
    return { type: "file", mode: Number(opened.mode & 0o7777n), size: opened.size.toString(), digest: digest.digest("hex") };
  } catch (error) {
    if (error instanceof BrokerError) throw error;
    throw new BrokerError("workspace_snapshot_failed", "A worktree file could not be read safely for the stability postcondition.");
  } finally {
    await handle?.close().catch(() => {});
  }
}

async function validateParentChain(root, relative) {
  let canonicalRoot;
  try { canonicalRoot = await fs.realpath(root); }
  catch { throw new BrokerError("workspace_snapshot_failed", "The repository root could not be canonicalized for a safe snapshot."); }
  if (canonicalRoot !== root) throw new BrokerError("workspace_snapshot_unsafe", "The repository root changed or became an alias during the snapshot.");
  const parents = [];
  let current = root;
  for (const segment of relative.split("/").slice(0, -1)) {
    current = path.join(current, segment);
    try {
      const before = await fs.lstat(current, { bigint: true });
      const canonical = await fs.realpath(current);
      const after = await fs.lstat(current, { bigint: true });
      if (!before.isDirectory() || before.isSymbolicLink() || !sameStat(before, after) ||
          canonical !== current || !isContained(root, canonical)) {
        throw new BrokerError("workspace_snapshot_unsafe", "A worktree path traversed a symlinked or unstable parent directory.");
      }
      parents.push({ path: current, stat: after });
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw new BrokerError("workspace_snapshot_failed", "A worktree parent directory could not be validated safely.");
    }
  }
  return parents;
}

async function recheckParentChain(parents) {
  for (const parent of parents) {
    try {
      const current = await fs.lstat(parent.path, { bigint: true });
      if (!current.isDirectory() || current.isSymbolicLink() || !sameStat(parent.stat, current)) {
        throw new BrokerError("workspace_snapshot_drift", "A worktree parent directory changed while content was being read.");
      }
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw new BrokerError("workspace_snapshot_drift", "A worktree parent directory changed while content was being read.");
    }
  }
}

async function hashSnapshotEntry(root, relative, budget) {
  const candidate = path.resolve(root, ...relative.split("/"));
  if (!isContained(root, candidate) || candidate === root) {
    throw new BrokerError("workspace_snapshot_unsafe", "A worktree path escaped the configured repository root.");
  }
  const parents = await validateParentChain(root, relative);
  let before;
  try { before = await fs.lstat(candidate, { bigint: true }); }
  catch (error) {
    if (error?.code === "ENOENT") {
      await recheckParentChain(parents);
      return { type: "missing", mode: 0, size: "0", digest: "" };
    }
    throw new BrokerError("workspace_snapshot_failed", "A worktree path could not be inspected safely.");
  }
  if (before.isFile()) {
    const result = await hashRegularFile(candidate, before, budget);
    await recheckParentChain(parents);
    return result;
  }
  if (before.isSymbolicLink()) {
    let target;
    try {
      target = await fs.readlink(candidate, { encoding: "buffer" });
      const after = await fs.lstat(candidate, { bigint: true });
      if (!sameStat(before, after) || target.length > MAX_SNAPSHOT_PATH_BYTES) {
        throw new BrokerError("workspace_snapshot_drift", "A worktree symlink changed while the snapshot was being captured.");
      }
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw new BrokerError("workspace_snapshot_failed", "A worktree symlink could not be read safely.");
    }
    budget.remaining -= target.length;
    budget.total += target.length;
    if (budget.remaining < 0) throw new BrokerError("workspace_snapshot_limit", "The tracked and nonignored worktree content exceeds the bounded snapshot limit.");
    await recheckParentChain(parents);
    return { type: "symlink", mode: Number(before.mode & 0o7777n), size: String(target.length), digest: crypto.createHash("sha256").update(target).digest("hex") };
  }
  throw new BrokerError("workspace_snapshot_unsafe", "The tracked or nonignored worktree contains an unsupported special file.");
}

async function contentManifest(root, paths) {
  const budget = { remaining: MAX_SNAPSHOT_TOTAL_BYTES, total: 0 };
  const hash = crypto.createHash("sha256").update("witself-workspace-content-v1\0");
  for (const relative of paths) {
    const entry = await hashSnapshotEntry(root, relative, budget);
    const pathBytes = Buffer.from(relative);
    hash.update(String(pathBytes.length)).update(":").update(pathBytes).update("\0");
    hash.update(entry.type).update("\0").update(String(entry.mode)).update("\0");
    hash.update(entry.size).update("\0").update(entry.digest).update("\0");
  }
  return { digest: hash.digest("hex"), bytes: budget.total };
}

async function defaultWorkspaceSnapshot(runtime) {
  const env = {
    HOME: runtime.env.HOME,
    PATH: "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
    LANG: "C.UTF-8",
    LC_ALL: "C.UTF-8",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_GLOBAL: process.platform === "win32" ? "NUL" : "/dev/null",
    GIT_TERMINAL_PROMPT: "0",
  };
  const git = runtime.gitCommand ?? (process.platform === "win32" ? "git.exe" : "/usr/bin/git");
  const captureGitState = () => Promise.all([
      spawnCapture(git, ["-C", runtime.projectRoot, "rev-parse", "--verify", "HEAD"], {
        cwd: runtime.projectRoot, env, timeoutMs: 10_000, maxStdoutBytes: 256, maxStderrBytes: 4096,
      }),
      spawnCapture(git, ["-C", runtime.projectRoot, "status", "--porcelain=v2", "-z", "--untracked-files=all"], {
        cwd: runtime.projectRoot, env, timeoutMs: 30_000, maxStdoutBytes: 4 * 1024 * 1024, maxStderrBytes: 8192,
      }),
      spawnCapture(git, ["-C", runtime.projectRoot, "ls-files", "-z", "--cached", "--others", "--exclude-standard"], {
        cwd: runtime.projectRoot, env, timeoutMs: 30_000, maxStdoutBytes: 8 * 1024 * 1024, maxStderrBytes: 8192,
      }),
    ]);
  const [head, status, files] = await captureGitState();
  if (head.code !== 0 || status.code !== 0 || files.code !== 0 || !/^[0-9a-f]{40,64}\n?$/.test(head.stdout)) {
    throw new BrokerError("workspace_snapshot_failed", "The broker could not capture the Git worktree postcondition.");
  }
  const headCommit = head.stdout.trim();
  const paths = parseSnapshotPaths(files.stdout);
  const firstManifest = await contentManifest(runtime.projectRoot, paths);
  const secondManifest = await contentManifest(runtime.projectRoot, paths);
  if (firstManifest.digest !== secondManifest.digest || firstManifest.bytes !== secondManifest.bytes) {
    throw new BrokerError("workspace_snapshot_drift", "The worktree changed while its exact content snapshot was being captured.");
  }
  const [headAfter, statusAfter, filesAfter] = await captureGitState();
  if (headAfter.code !== 0 || statusAfter.code !== 0 || filesAfter.code !== 0 ||
      headAfter.stdout !== head.stdout || statusAfter.stdout !== status.stdout || filesAfter.stdout !== files.stdout) {
    throw new BrokerError("workspace_snapshot_drift", "Git state changed while the exact worktree snapshot was being captured.");
  }
  const digest = crypto.createHash("sha256").update(headCommit).update("\0").update(status.stdout).update("\0")
    .update(files.stdout).update("\0").update(firstManifest.digest).digest("hex");
  const entries = status.stdout.length === 0 ? 0 : status.stdout.split("\0").filter(Boolean).length;
  return Object.freeze({ headCommit, digest, entries, files: paths.length, contentBytes: firstManifest.bytes });
}

export class Broker {
  constructor(runtime, options = {}) {
    this.runtime = runtime;
    const ceiling = options.ceiling ?? "repository";
    toolsForCeiling(ceiling);
    Object.defineProperty(this, "ceiling", { value: ceiling, enumerable: true, writable: false });
    this.grantVerifier = options.grantVerifier ?? null;
    this.launcherEnvironment = options.launcherEnvironment ?? Object.freeze({});
    this.clock = options.clock ?? Date.now;
    this.sessionFactory = options.sessionFactory ?? ((runtimeInfo) => new AppServerSession(runtimeInfo));
    this.implementationSessionFactory = options.implementationSessionFactory ?? ((runtimeInfo, workspace) =>
      new ImplementationAppServerSession(runtimeInfo, workspace));
    this.systemSessionFactory = options.systemSessionFactory ?? ((runtimeInfo) => new SystemAppServerSession(runtimeInfo, {
      launcherEnvironment: this.launcherEnvironment,
    }));
    this.probeFn = options.probeFn ?? probeRuntime;
    this.systemProbeFn = options.systemProbeFn ?? probeSystemRuntime;
    this.snapshotFn = options.snapshotFn ?? defaultWorkspaceSnapshot;
    this.createIsolatedWorkspace = options.createIsolatedWorkspace ?? createIsolatedWorkspace;
    this.finalizeIsolatedWorkspace = options.finalizeIsolatedWorkspace ?? finalizeIsolatedWorkspace;
    this.compactIsolatedWorkspace = options.compactIsolatedWorkspace ?? compactIsolatedWorkspace;
    this.readIsolatedArtifact = options.readIsolatedArtifact ?? readIsolatedArtifact;
    this.cleanupIsolatedWorkspace = options.cleanupIsolatedWorkspace ?? cleanupIsolatedWorkspace;
    this.workspaceMonitorFactory = options.workspaceMonitorFactory ?? ((handle, runtimeInfo) => startIsolatedWorkspaceMonitor({
      handle,
      runtime: runtimeInfo,
      maxLogicalBytes: ISOLATED_WORKSPACE_LIMITS.maxJobBytes,
      maxAllocatedBytes: ISOLATED_WORKSPACE_LIMITS.maxJobBytes,
      maxEntries: MAX_ACTIVE_IMPLEMENTATION_ENTRIES,
    }));
    this.maxBrokerJobs = boundedBrokerLimit(options.maxBrokerJobs, MAX_BROKER_JOBS, "broker job-count");
    this.maxRetainedArtifactBytes = boundedBrokerLimit(
      options.maxRetainedArtifactBytes, MAX_RETAINED_ARTIFACT_BYTES, "retained-artifact byte",
    );
    this.maxImplementationArtifactBytes = boundedBrokerLimit(
      options.maxImplementationArtifactBytes, MAX_IMPLEMENTATION_ARTIFACT_BYTES, "per-implementation artifact byte",
    );
    this.retainedArtifactBytes = 0;
    this.reservedArtifactBytes = 0;
    this.jobs = new Map();
    this.nonJobOperations = new Set();
    this.activeOperations = 0;
    this.systemActive = false;
    this.closed = false;
    this.shutdownInterrupts = [];
    this.closePromise = null;
  }

  toolCatalog() {
    return toolsForCeiling(this.ceiling);
  }

  authorizeElevated(tool, args) {
    if (!this.grantVerifier) throw new BrokerError("grant_required", "This broker was not started with elevated one-use grant authority.");
    return this.grantVerifier.verifyAndConsume(tool, args);
  }

  #ensureJobAdmission(lane) {
    if (this.jobs.size >= this.maxBrokerJobs) {
      throw new BrokerError("job_capacity_reached", "This broker reached its fixed lifetime job-record limit; restart Claude Code and the broker.");
    }
    if (lane === "implementation" &&
        this.retainedArtifactBytes + this.reservedArtifactBytes + this.maxImplementationArtifactBytes > this.maxRetainedArtifactBytes) {
      throw new BrokerError("artifact_capacity_reached", "This broker reached its fixed retained implementation-artifact limit; restart Claude Code and the broker.");
    }
  }

  #enterOperation(lane = "review") {
    if (this.closed) throw new BrokerError("broker_closed", "The broker is shutting down.");
    if (lane === "system") {
      if (this.ceiling !== "system") throw new BrokerError("tool_above_ceiling", "The system lane exceeds the immutable startup ceiling.");
      if (this.activeOperations !== 0 || this.systemActive) {
        throw new BrokerError("system_exclusive", "A full-access system task requires exclusive broker execution.");
      }
      this.systemActive = true;
      this.activeOperations = 1;
      return;
    }
    if (this.systemActive) throw new BrokerError("system_exclusive", "No other Codex operation may start while a full-access system task is active.");
    if (this.activeOperations >= MAX_CONCURRENCY) {
      throw new BrokerError("concurrency_limit", `At most ${MAX_CONCURRENCY} Codex operations may run at once.`);
    }
    this.activeOperations += 1;
  }

  #leaveOperation(lane = "review") {
    this.activeOperations = Math.max(0, this.activeOperations - 1);
    if (lane === "system") this.systemActive = false;
  }

  async probe() {
    this.#enterOperation();
    let operation;
    try {
      operation = (async () => {
        const runtime = await this.runtime.prepareForNewWork();
        const attestation = await this.probeFn(runtime);
        return Object.freeze({
          ...attestation,
          broker_ceiling: this.ceiling,
          available_tools: Object.freeze(this.toolCatalog().map(({ name }) => name)),
        });
      })();
      this.nonJobOperations.add(operation);
      return await operation;
    } finally {
      if (operation) this.nonJobOperations.delete(operation);
      this.#leaveOperation();
    }
  }

  start(task) {
    if (typeof task !== "string" || task.trim().length === 0 || task.length > MAX_TASK_CHARS) {
      throw new BrokerError("invalid_task", `The review task must contain 1 to ${MAX_TASK_CHARS} characters.`);
    }
    this.#ensureJobAdmission("review");
    this.#enterOperation();
    const now = this.clock();
    const job = {
      id: newId(),
      lane: "review",
      capabilityCeiling: "repository",
      state: "preparing",
      createdAt: now,
      startedAt: null,
      completedAt: null,
      runtime: null,
      attestation: null,
      workspace: null,
      result: null,
      error: null,
      controller: new AbortController(),
      session: null,
      statusWaiters: new Set(),
    };
    this.jobs.set(job.id, job);
    job.executionPromise = this.#execute(job, task).finally(() => {
      this.#leaveOperation();
      this.#notifyStatusWaiters(job);
    });
    void job.executionPromise.catch(() => {});
    return this.#view(job, false);
  }

  implementationStart(task) {
    if (typeof task !== "string" || task.trim().length === 0 || task.length > MAX_TASK_CHARS) {
      throw new BrokerError("invalid_task", `The implementation task must contain 1 to ${MAX_TASK_CHARS} characters.`);
    }
    if (this.ceiling !== "isolated-write" && this.ceiling !== "system") {
      throw new BrokerError("tool_above_ceiling", "The implementation lane exceeds the immutable startup ceiling.");
    }
    this.#ensureJobAdmission("implementation");
    this.#enterOperation("implementation");
    this.reservedArtifactBytes += this.maxImplementationArtifactBytes;
    const now = this.clock();
    const job = {
      id: newId(),
      lane: "implementation",
      capabilityCeiling: "isolated-write",
      state: "preparing",
      createdAt: now,
      startedAt: null,
      completedAt: null,
      runtime: null,
      attestation: null,
      workspace: null,
      implementation: null,
      artifacts: null,
      result: null,
      error: null,
      controller: new AbortController(),
      session: null,
      isolatedHandle: null,
      artifactReservation: this.maxImplementationArtifactBytes,
      retainedArtifactBytes: 0,
      workspaceMonitor: null,
      statusWaiters: new Set(),
    };
    this.jobs.set(job.id, job);
    job.executionPromise = this.#executeImplementation(job, task).finally(() => {
      this.#leaveOperation("implementation");
      this.#notifyStatusWaiters(job);
    });
    void job.executionPromise.catch(() => {});
    return this.#view(job, false);
  }

  systemStart(task) {
    if (typeof task !== "string" || task.trim().length === 0 || task.length > MAX_TASK_CHARS) {
      throw new BrokerError("invalid_task", `The system task must contain 1 to ${MAX_TASK_CHARS} characters.`);
    }
    this.#ensureJobAdmission("system");
    this.#enterOperation("system");
    const now = this.clock();
    const job = {
      id: newId(),
      lane: "system",
      capabilityCeiling: "system",
      state: "preparing",
      createdAt: now,
      startedAt: null,
      completedAt: null,
      runtime: null,
      attestation: null,
      workspace: null,
      result: null,
      error: null,
      controller: new AbortController(),
      session: null,
      statusWaiters: new Set(),
    };
    this.jobs.set(job.id, job);
    job.executionPromise = this.#executeSystem(job, task).finally(() => {
      this.#leaveOperation("system");
      this.#notifyStatusWaiters(job);
    });
    void job.executionPromise.catch(() => {});
    return this.#view(job, false);
  }

  async #execute(job, task) {
    let before = null;
    try {
      const runtime = await this.runtime.prepareForNewWork();
      job.runtime = {
        version: runtime.version,
        integrity: runtime.integrity,
        platformVersion: runtime.platformVersion,
        platformIntegrity: runtime.platformIntegrity,
        latestVerificationPolicy: runtime.latestVerificationPolicy,
        latestVerifiedAt: runtime.latestVerifiedAt,
      };
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex review was cancelled.");
      before = await this.snapshotFn(runtime);
      job.workspace = { headCommit: before.headCommit, dirtyEntriesBefore: before.entries };
      const session = this.sessionFactory(runtime);
      job.session = session;
      await session.start();
      job.attestation = await session.attest();
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex review was cancelled.");
      job.state = "running";
      job.startedAt = this.clock();
      job.result = await session.runTurn(task, { signal: job.controller.signal, timeoutMs: JOB_TIMEOUT_MS });
      await session.verifyAuthUnchanged();
      job.state = "succeeded";
    } catch (error) {
      const safe = job.controller?.signal.aborted
        ? { code: "job_cancelled", message: "The delegated Codex review was cancelled." }
        : publicError(error);
      job.error = safe;
      job.state = safe.code === "job_cancelled" ? "cancelled" : "failed";
    } finally {
      let terminalState = job.state;
      job.state = "finalizing";
      try { await job.session?.shutdown(); }
      catch {
        job.result = null;
        terminalState = "failed";
        job.error = { code: "review_cleanup_failed", message: "The constrained Codex review session could not be safely stopped." };
      }
      job.session = null;
      if (before) {
        try {
          const runtime = this.runtime.describe();
          const after = await this.snapshotFn(runtime);
          job.workspace = {
            headCommit: before.headCommit,
            dirtyEntriesBefore: before.entries,
            dirtyEntriesAfter: after.entries,
            unchanged: before.digest === after.digest,
          };
          if (before.digest !== after.digest) {
            job.result = null;
            terminalState = "failed";
            job.error = { code: "workspace_changed", message: "The Git worktree changed during the delegated review; no result is trusted and nothing was undone." };
          }
        } catch {
          job.result = null;
          terminalState = "failed";
          job.error = { code: "workspace_postcondition_failed", message: "The broker could not verify that the Git worktree stayed unchanged; no result is trusted." };
        }
      }
      job.state = terminalState;
      job.completedAt = this.clock();
      job.controller = null;
    }
  }

  async #executeImplementation(job, task) {
    let report = null;
    let monitor = null;
    const guard = (operation) => monitor ? Promise.race([Promise.resolve(operation), monitor.failure]) : operation;
    try {
      const runtime = await this.runtime.prepareForNewWork();
      job.runtime = {
        version: runtime.version,
        integrity: runtime.integrity,
        platformVersion: runtime.platformVersion,
        platformIntegrity: runtime.platformIntegrity,
        latestVerificationPolicy: runtime.latestVerificationPolicy,
        latestVerifiedAt: runtime.latestVerifiedAt,
      };
      const handle = await this.createIsolatedWorkspace({
        sourceRoot: runtime.projectRoot,
        jobsRoot: path.join(runtime.scratchRoot, "implementation-jobs"),
        gitCommand: runtime.gitCommand,
        signal: job.controller.signal,
      });
      job.isolatedHandle = handle;
      monitor = this.workspaceMonitorFactory(handle, runtime);
      job.workspaceMonitor = monitor;
      job.workspace = {
        originalHead: handle.originalHead,
        baselineCommit: handle.baselineCommit,
        sourceFingerprint: handle.sourceFingerprint,
        sourceFileCount: handle.sourceFileCount,
        sourceBytes: handle.sourceBytes,
        finalized: false,
        monitor: monitor.policy,
      };
      await guard(monitor.ready);
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex implementation was cancelled.");
      const session = this.implementationSessionFactory(runtime, handle);
      job.session = session;
      await guard(session.start());
      if (typeof session.scratch !== "string" || !path.isAbsolute(session.scratch)) {
        throw new BrokerError("implementation_workspace_monitor_invalid", "The isolated Codex app-server did not expose its broker-owned scratch root for quota monitoring.");
      }
      await guard(monitor.addRoot(session.scratch));
      await guard(monitor.sample());
      const sessionAttestation = await guard(session.attest());
      job.attestation = Object.freeze({ ...sessionAttestation, workspaceMonitor: monitor.policy });
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex implementation was cancelled.");
      job.state = "running";
      job.startedAt = this.clock();
      report = await guard(session.runTurn(task, { signal: job.controller.signal, timeoutMs: JOB_TIMEOUT_MS }));
      await guard(session.verifyAuthUnchanged());
      job.result = report;
      requireCompletedActionReport(report, "implementation");
      job.state = "succeeded";
    } catch (error) {
      const safe = job.controller?.signal.aborted
        ? { code: "job_cancelled", message: "The delegated Codex implementation was cancelled." }
        : publicError(error);
      if (safe.code.startsWith("implementation_workspace_")) await job.session?.interrupt().catch(() => {});
      job.error = safe;
      job.state = safe.code === "job_cancelled" ? "cancelled" : "failed";
    } finally {
      let terminalState = job.state;
      job.state = "finalizing";
      let monitorEvidence = monitor?.evidence() ?? null;
      let sessionStopped = job.session === null;
      try {
        await job.session?.shutdown(monitor && typeof job.session?.scratch === "string" ? {
          beforeScratchCleanup: () => monitor.removeRoot(job.session.scratch),
        } : {});
        sessionStopped = true;
      } catch {
        job.result = null;
        terminalState = "failed";
        job.error = { code: "implementation_session_cleanup_failed", message: "The isolated Codex app-server could not be safely stopped." };
      }
      if (monitor) {
        await monitor.sample().catch(() => {});
        monitorEvidence = await monitor.stop().catch(() => monitor.evidence());
        const monitorError = monitor.error();
        if (monitorError) {
          job.result = null;
          terminalState = "failed";
          job.error = publicError(monitorError);
        }
        job.workspace = { ...(job.workspace ?? {}), monitor: monitorEvidence };
      }
      job.session = null;
      if (job.isolatedHandle) {
        let finalized = null;
        try {
          if (!sessionStopped) {
            throw new BrokerError("implementation_session_cleanup_failed", "The isolated Codex app-server process tree was not safely reaped, so no artifact can be trusted.");
          }
          const evidence = {
            schemaVersion: 1,
            terminalStateBeforeFinalization: terminalState,
            report,
            error: job.error,
            workspaceMonitor: monitorEvidence,
            attestation: job.attestation ? {
              model: job.attestation.model,
              effort: job.attestation.effort,
              multiAgentVersion: job.attestation.multiAgentVersion,
              permissionProfile: job.attestation.permissionProfile,
              approvalPolicy: job.attestation.approvalPolicy,
              networkAccess: job.attestation.networkAccess,
              confinement: job.attestation.confinement,
            } : null,
          };
          finalized = await this.finalizeIsolatedWorkspace(job.isolatedHandle, { evidence });
          const { artifacts, changedFiles, ...implementation } = finalized;
          const compacted = await this.compactIsolatedWorkspace(job.isolatedHandle);
          const artifactBytes = Object.values(artifacts ?? {}).reduce((total, artifact) =>
            total + (Number.isSafeInteger(artifact?.sizeBytes) && artifact.sizeBytes >= 0 ? artifact.sizeBytes : Number.NaN), 0);
          if (compacted?.compacted !== true || typeof compacted.alreadyCompacted !== "boolean" ||
              !Number.isSafeInteger(compacted.retainedBytes) || !Number.isSafeInteger(artifactBytes) ||
              artifactBytes > (job.artifactReservation ?? 0) || compacted.retainedBytes < artifactBytes ||
              compacted.retainedBytes > artifactBytes + 4096) {
            throw new BrokerError("isolated_workspace_compaction_failed", "The compacted implementation workspace returned invalid bounded evidence.");
          }
          const visibleChangedFiles = [];
          let visibleChangedPathBytes = 0;
          for (const changedPath of changedFiles) {
            const bytes = Buffer.byteLength(changedPath);
            if (visibleChangedFiles.length >= 100 || visibleChangedPathBytes + bytes > 32 * 1024) break;
            visibleChangedFiles.push(changedPath);
            visibleChangedPathBytes += bytes;
          }
          job.implementation = {
            ...implementation,
            changedFiles: visibleChangedFiles,
            changedFilesTruncated: visibleChangedFiles.length !== changedFiles.length,
            finalized: true,
            compacted: true,
            retainedBytes: compacted.retainedBytes,
          };
          job.artifacts = artifacts;
          job.retainedArtifactBytes = artifactBytes;
          this.retainedArtifactBytes += artifactBytes;
          job.workspace = {
            ...(job.workspace ?? {}),
            finalized: true,
            compacted: true,
            retainedBytes: compacted.retainedBytes,
            sourceDiverged: finalized.sourceDiverged,
            changedFileCount: finalized.changedFileCount,
          };
        } catch (error) {
          const safe = publicError(error);
          job.result = null;
          job.artifacts = null;
          const compactionFailed = finalized !== null;
          job.implementation = { finalized: compactionFailed, compacted: false, error: safe };
          job.workspace = { ...(job.workspace ?? {}), finalized: compactionFailed, compacted: false };
          if (compactionFailed) {
            terminalState = "failed";
            job.error = { code: "implementation_compaction_failed", message: "The isolated implementation could not be safely compacted for bounded artifact retention." };
          } else if (terminalState === "succeeded") {
            terminalState = "failed";
            job.error = { code: "implementation_finalization_failed", message: "The isolated implementation could not be converted into a safe bounded patch." };
          }
          try {
            await this.cleanupIsolatedWorkspace(job.isolatedHandle);
            job.isolatedHandle = null;
            job.implementation = { ...job.implementation, cleaned: true };
            job.workspace = { ...(job.workspace ?? {}), cleaned: true };
          } catch {
            terminalState = "failed";
            job.error = { code: "implementation_cleanup_failed", message: "The failed isolated implementation workspace could not be safely removed." };
            job.implementation = { ...job.implementation, cleaned: false };
            job.workspace = { ...(job.workspace ?? {}), cleaned: false };
          }
        }
      }
      this.reservedArtifactBytes = Math.max(0, this.reservedArtifactBytes - (job.artifactReservation ?? 0));
      job.artifactReservation = 0;
      job.state = terminalState;
      job.completedAt = this.clock();
      job.controller = null;
    }
  }

  async #executeSystem(job, task) {
    let before = null;
    try {
      const runtime = await this.runtime.prepareForNewWork();
      job.runtime = {
        version: runtime.version,
        integrity: runtime.integrity,
        platformVersion: runtime.platformVersion,
        platformIntegrity: runtime.platformIntegrity,
        latestVerificationPolicy: runtime.latestVerificationPolicy,
        latestVerifiedAt: runtime.latestVerifiedAt,
      };
      try {
        before = await this.snapshotFn(runtime);
        job.workspace = {
          evidenceComplete: false,
          before: { headCommit: before.headCommit, fingerprint: before.digest, dirtyEntries: before.entries },
        };
      } catch {
        job.workspace = { evidenceComplete: false, beforeUnavailable: true };
      }
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex system task was cancelled.");
      const session = this.systemSessionFactory(runtime);
      job.session = session;
      await session.start();
      job.attestation = await session.attest();
      if (job.controller.signal.aborted) throw new BrokerError("job_cancelled", "The delegated Codex system task was cancelled.");
      job.state = "running";
      job.startedAt = this.clock();
      const report = await session.runTurn(task, { signal: job.controller.signal, timeoutMs: JOB_TIMEOUT_MS });
      await session.verifyAuthUnchanged();
      job.result = report;
      requireCompletedActionReport(report, "system");
      job.state = "succeeded";
    } catch (error) {
      const safe = job.controller?.signal.aborted
        ? { code: "job_cancelled", message: "The delegated Codex system task was cancelled." }
        : publicError(error);
      job.error = safe;
      job.state = safe.code === "job_cancelled" ? "cancelled" : "failed";
    } finally {
      let terminalState = job.state;
      job.state = "finalizing";
      try { await job.session?.shutdown(); }
      catch {
        job.result = null;
        terminalState = "failed";
        job.error = { code: "system_cleanup_failed", message: "The private Codex system-operation session could not be safely cleaned up." };
      }
      job.session = null;
      try {
        const runtime = this.runtime.describe();
        const after = await this.snapshotFn(runtime);
        job.workspace = {
          ...(job.workspace ?? {}),
          after: { headCommit: after.headCommit, fingerprint: after.digest, dirtyEntries: after.entries },
          evidenceComplete: before !== null,
          changed: before === null ? null : before.digest !== after.digest,
        };
      } catch {
        job.workspace = { ...(job.workspace ?? {}), evidenceComplete: false, afterUnavailable: true, changed: null };
      }
      job.state = terminalState;
      job.completedAt = this.clock();
      job.controller = null;
    }
  }

  status(jobId, waitSeconds) {
    const job = this.#requireJob(jobId, "review");
    return this.#statusView(job, waitSeconds);
  }

  implementationStatus(jobId, waitSeconds) {
    return this.#statusView(this.#requireJob(jobId, "implementation"), waitSeconds);
  }

  async implementationArtifactRead(jobId, artifactId, offset, maxBytes) {
    this.#enterOperation("implementation");
    let operation;
    try {
      operation = (async () => {
        const job = this.#requireJob(jobId, "implementation");
        if (!job.isolatedHandle || !job.artifacts || job.implementation?.finalized !== true) {
          throw new BrokerError("implementation_artifact_unavailable", "Implementation artifacts are unavailable until safe bounded finalization completes.");
        }
        return this.readIsolatedArtifact(job.isolatedHandle, {
          artifactId,
          offset: offset ?? 0,
          maxBytes: maxBytes ?? MAX_ARTIFACT_CHUNK_BYTES,
        });
      })();
      this.nonJobOperations.add(operation);
      return await operation;
    } finally {
      if (operation) this.nonJobOperations.delete(operation);
      this.#leaveOperation("implementation");
    }
  }

  systemStatus(jobId, waitSeconds) {
    return this.#statusView(this.#requireJob(jobId, "system"), waitSeconds);
  }

  async cancel(jobId) {
    const job = this.#requireJob(jobId, "review");
    return this.#cancelJob(job);
  }

  async implementationCancel(jobId) {
    return this.#cancelJob(this.#requireJob(jobId, "implementation"));
  }

  async systemCancel(jobId) {
    return this.#cancelJob(this.#requireJob(jobId, "system"));
  }

  async #cancelJob(job) {
    if (["succeeded", "failed", "cancelled"].includes(job.state)) return this.#view(job, false);
    if (job.state === "finalizing") return this.#view(job, false);
    job.controller?.abort();
    await job.session?.interrupt().catch(() => {});
    return { job_id: job.id, state: "cancelling" };
  }

  #requireJob(jobId, lane) {
    if (typeof jobId !== "string" || !JOB_ID_RE.test(jobId)) throw new BrokerError("invalid_job_id", "The Codex job ID is invalid.");
    const job = this.jobs.get(jobId);
    if (!job) throw new BrokerError("job_not_found", "No Codex job with that ID exists in this broker process.");
    if (job.lane !== lane) throw new BrokerError("job_not_found", "No Codex job with that ID exists in this broker lane.");
    return job;
  }

  #statusView(job, waitSeconds) {
    const seconds = statusWaitSeconds(waitSeconds);
    if (seconds === 0 || TERMINAL_JOB_STATES.includes(job.state)) return this.#view(job, true);
    if (!(job.statusWaiters instanceof Set)) return this.#view(job, true);
    return new Promise((resolve, reject) => {
      const waiter = {
        timer: null,
        finish: () => {
          if (!job.statusWaiters.delete(waiter)) return;
          if (waiter.timer) clearTimeout(waiter.timer);
          try { resolve(this.#view(job, true)); }
          catch (error) { reject(error); }
        },
      };
      waiter.timer = setTimeout(waiter.finish, seconds * 1000);
      job.statusWaiters.add(waiter);
    });
  }

  #notifyStatusWaiters(job) {
    if (!(job.statusWaiters instanceof Set) || job.statusWaiters.size === 0) return;
    for (const waiter of [...job.statusWaiters]) waiter.finish();
  }

  #view(job, includeResult) {
    const view = {
      job_id: job.id,
      lane: job.lane,
      capability_ceiling: job.capabilityCeiling,
      state: job.state,
      created_at: iso(job.createdAt),
      started_at: iso(job.startedAt),
      completed_at: iso(job.completedAt),
      runtime: job.runtime,
      attestation: job.attestation,
      workspace: job.workspace,
      ...(job.lane === "implementation" ? { implementation: job.implementation, artifacts: job.artifacts } : {}),
      error: job.error,
    };
    const reportableSemanticFailure = job.state === "failed" && [
      "task_unverified", "implementation_task_blocked", "system_task_blocked",
    ].includes(job.error?.code);
    if (includeResult && (job.state === "succeeded" || reportableSemanticFailure)) view.result = job.result;
    const encoded = JSON.stringify(view);
    if (Buffer.byteLength(encoded) > MAX_RESULT_BYTES) {
      throw new BrokerError("result_too_large", "The bounded Codex job result exceeded the broker output limit.");
    }
    return view;
  }

  requestShutdown() {
    if (this.closed) return;
    this.closed = true;
    for (const job of this.jobs.values()) {
      if (!["succeeded", "failed", "cancelled"].includes(job.state)) {
        job.controller?.abort();
        if (job.session) {
          try { this.shutdownInterrupts.push(Promise.resolve(job.session.interrupt()).catch(() => {})); }
          catch {}
        }
      }
    }
  }

  close() {
    this.requestShutdown();
    if (!this.closePromise) this.closePromise = this.#completeClose();
    return this.closePromise;
  }

  async #completeClose() {
    const executions = [...this.jobs.values()].flatMap((job) => job.executionPromise ? [job.executionPromise] : []);
    await Promise.allSettled([...this.shutdownInterrupts, ...executions, ...this.nonJobOperations]);
    this.shutdownInterrupts.length = 0;
    const isolatedCleanups = [];
    for (const job of this.jobs.values()) {
      if (job.lane === "implementation" && job.isolatedHandle) {
        isolatedCleanups.push(this.cleanupIsolatedWorkspace(job.isolatedHandle));
      }
    }
    const cleanupResults = await Promise.allSettled(isolatedCleanups);
    try {
      if (cleanupResults.some((result) => result.status === "rejected")) {
        throw new BrokerError("implementation_cleanup_failed", "A broker-owned isolated implementation workspace could not be safely removed.");
      }
      await this.runtime.cleanup();
    } finally {
      this.grantVerifier?.destroy();
    }
  }
}

function parseArgs(args) {
  if (!Array.isArray(args) || args.length < 2 || args.length > 6 || args.length % 2 !== 0) {
    throw new BrokerError("usage", "Usage: server.mjs --repository ROOT [--ceiling repository|isolated-write|system] [--grant-key-file ABS]");
  }
  const parsed = { projectRoot: null, ceiling: "repository", grantKeyFile: null };
  const seen = new Set();
  for (let index = 0; index < args.length; index += 2) {
    const flag = args[index];
    const value = args[index + 1];
    if (!["--repository", "--ceiling", "--grant-key-file"].includes(flag) || seen.has(flag) ||
        typeof value !== "string" || value.length === 0 || value.length > 4096 || /[\0\r\n]/u.test(value)) {
      throw new BrokerError("usage", "Usage: server.mjs --repository ROOT [--ceiling repository|isolated-write|system] [--grant-key-file ABS]");
    }
    seen.add(flag);
    if (flag === "--repository") parsed.projectRoot = value;
    else if (flag === "--ceiling") parsed.ceiling = value;
    else parsed.grantKeyFile = value;
  }
  if (!parsed.projectRoot || !["repository", "isolated-write", "system"].includes(parsed.ceiling) ||
      (parsed.ceiling === "repository" && parsed.grantKeyFile !== null) ||
      (parsed.ceiling !== "repository" && (parsed.grantKeyFile === null || !path.isAbsolute(parsed.grantKeyFile)))) {
    throw new BrokerError("usage", "Usage: server.mjs --repository ROOT [--ceiling repository|isolated-write|system] [--grant-key-file ABS]");
  }
  return Object.freeze(parsed);
}

function rpcError(id, code, message) {
  return { jsonrpc: "2.0", id: id ?? null, error: { code, message } };
}

function toolResult(value, isError = false) {
  const text = JSON.stringify(value);
  return { content: [{ type: "text", text }], isError };
}

export async function processRpcLine(line, context, broker) {
  let request;
  try { request = JSON.parse(line); }
  catch { return rpcError(null, -32700, "Parse error"); }
  if (!request || request.jsonrpc !== "2.0" || typeof request.method !== "string" || request.method.length > 128 ||
      (!(["string", "number"].includes(typeof request.id)) && request.id !== undefined)) {
    return rpcError(request?.id, -32600, "Invalid Request");
  }
  if (typeof request.id === "string" && request.id.length > 128) return rpcError(null, -32600, "Invalid Request");
  const isNotification = request.id === undefined;
  if (isNotification) {
    if (request.method === "notifications/initialized" && context.initializeSeen) context.initialized = true;
    return null;
  }
  try {
    let result;
    if (request.method === "initialize") {
      if (context.initializeSeen || !request.params || typeof request.params !== "object" || Array.isArray(request.params)) {
        throw new BrokerError("invalid_initialize", "Invalid or repeated initialize request.");
      }
      const allowed = ["protocolVersion", "capabilities", "clientInfo"];
      if (Object.keys(request.params).some((key) => !allowed.includes(key)) ||
          typeof request.params.protocolVersion !== "string" || request.params.protocolVersion.length > 32 ||
          !request.params.capabilities || typeof request.params.capabilities !== "object" || Array.isArray(request.params.capabilities) ||
          !request.params.clientInfo || typeof request.params.clientInfo !== "object" || Array.isArray(request.params.clientInfo)) {
        throw new BrokerError("invalid_initialize", "The initialize request shape is invalid.");
      }
      context.initializeSeen = true;
      result = {
        protocolVersion: "2025-06-18",
        capabilities: { tools: { listChanged: false } },
        serverInfo: { name: "witself-claude-codex-broker", version: BROKER_VERSION },
        instructions: broker.ceiling === "system"
          ? "A static system-ceiling Codex broker. The runtime probe attests only the constrained repository-review lane; isolated implementations and full-access system starts separately attest their exact policy and capabilities. Elevated tools require fresh one-use launcher grants."
          : broker.ceiling === "isolated-write"
            ? "A static isolated-write Codex broker. The runtime probe attests only the constrained repository-review lane; every isolated implementation separately attests clone-only write access and disabled network before its turn. Patch artifacts are never auto-applied. Elevated tools require fresh one-use launcher grants."
            : "A constrained repository-review Codex broker. Probe first; start bounded jobs; poll status; inspect evidence independently.",
      };
    } else if (request.method === "ping") {
      if (request.params !== undefined && !exactKeys(request.params, [])) throw new BrokerError("invalid_ping", "ping accepts no parameters.");
      result = {};
    } else if (!context.initialized) {
      throw new BrokerError("not_initialized", "The MCP client must initialize before calling tools.");
    } else if (request.method === "tools/list") {
      if (request.params !== undefined && !exactKeys(request.params, [])) throw new BrokerError("invalid_arguments", "tools/list accepts no parameters.");
      result = { tools: typeof broker.toolCatalog === "function" ? broker.toolCatalog() : TOOLS };
    } else if (request.method === "tools/call") {
      try { result = toolResult(await dispatchTool(broker, request.params)); }
      catch (error) { result = toolResult({ error: publicError(error) }, true); }
    } else {
      return rpcError(request.id, -32601, "Method not found");
    }
    return { jsonrpc: "2.0", id: request.id, result };
  } catch (error) {
    const safe = publicError(error);
    return rpcError(request.id, -32602, boundedText(safe.message, 500));
  }
}

async function dispatchTool(broker, params) {
  if (!params || typeof params !== "object" || Array.isArray(params)) {
    throw new BrokerError("invalid_tool_call", "tools/call parameters must be an object.");
  }
  const keys = Object.keys(params);
  const hasArguments = Object.hasOwn(params, "arguments");
  const hasOnlyProtocolKeys = keys.every((key) => key === "name" || key === "arguments" || key === "_meta");
  if (!hasOnlyProtocolKeys || typeof params.name !== "string" || !validRequestMeta(params._meta)) {
    throw new BrokerError(
      "invalid_tool_call",
      "tools/call requires a tool name, optional arguments object, and optional bounded request metadata.",
    );
  }
  const args = hasArguments ? params.arguments : {};
  if (!args || typeof args !== "object" || Array.isArray(args)) {
    throw new BrokerError("invalid_tool_call", "tools/call arguments must be an object when provided.");
  }
  switch (params.name) {
    case "codex_runtime_probe":
      if (!exactKeys(args, [])) throw new BrokerError("invalid_arguments", "codex_runtime_probe accepts no arguments.");
      return broker.probe();
    case "codex_review_start":
      if (!exactKeys(args, ["task"])) throw new BrokerError("invalid_arguments", "codex_review_start accepts only task.");
      return broker.start(args.task);
    case "codex_review_status":
      if (!exactKeys(args, ["job_id"]) && !exactKeys(args, ["job_id", "wait_seconds"])) {
        throw new BrokerError("invalid_arguments", "codex_review_status accepts only job_id and optional wait_seconds.");
      }
      return broker.status(args.job_id, statusWaitSeconds(args.wait_seconds));
    case "codex_review_cancel":
      if (!exactKeys(args, ["job_id"])) throw new BrokerError("invalid_arguments", "codex_review_cancel accepts only job_id.");
      return broker.cancel(args.job_id);
    case "codex_implementation_start": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["task"])) throw new BrokerError("invalid_arguments", "codex_implementation_start accepts only task plus its trusted grant.");
      return broker.implementationStart(authorized.task);
    }
    case "codex_implementation_status": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["job_id"]) && !exactKeys(authorized, ["job_id", "wait_seconds"])) {
        throw new BrokerError("invalid_arguments", "codex_implementation_status accepts only job_id, optional wait_seconds, and its trusted grant.");
      }
      return broker.implementationStatus(authorized.job_id, statusWaitSeconds(authorized.wait_seconds));
    }
    case "codex_implementation_artifact_read": {
      const authorized = broker.authorizeElevated(params.name, args);
      const keys = Object.keys(authorized);
      if (!keys.includes("job_id") || !keys.includes("artifact_id") || keys.some((key) => !["job_id", "artifact_id", "offset", "max_bytes"].includes(key))) {
        throw new BrokerError("invalid_arguments", "codex_implementation_artifact_read accepts only job_id, artifact_id, and bounded optional offset/max_bytes plus its trusted grant.");
      }
      return broker.implementationArtifactRead(authorized.job_id, authorized.artifact_id, authorized.offset, authorized.max_bytes);
    }
    case "codex_implementation_cancel": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["job_id"])) throw new BrokerError("invalid_arguments", "codex_implementation_cancel accepts only job_id plus its trusted grant.");
      return broker.implementationCancel(authorized.job_id);
    }
    case "codex_system_start": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["task"])) throw new BrokerError("invalid_arguments", "codex_system_start accepts only task plus its trusted grant.");
      return broker.systemStart(authorized.task);
    }
    case "codex_system_status": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["job_id"]) && !exactKeys(authorized, ["job_id", "wait_seconds"])) {
        throw new BrokerError("invalid_arguments", "codex_system_status accepts only job_id, optional wait_seconds, and its trusted grant.");
      }
      return broker.systemStatus(authorized.job_id, statusWaitSeconds(authorized.wait_seconds));
    }
    case "codex_system_cancel": {
      const authorized = broker.authorizeElevated(params.name, args);
      if (!exactKeys(authorized, ["job_id"])) throw new BrokerError("invalid_arguments", "codex_system_cancel accepts only job_id plus its trusted grant.");
      return broker.systemCancel(authorized.job_id);
    }
    default:
      throw new BrokerError("unknown_tool", "The requested tool is not exposed by this constrained broker.");
  }
}

async function writeLine(message, output = process.stdout) {
  const encoded = `${JSON.stringify(message)}\n`;
  if (Buffer.byteLength(encoded) > MAX_RESULT_BYTES + 16 * 1024) {
    throw new BrokerError("response_too_large", "The broker response exceeded its protocol output limit.");
  }
  if (!output.write(encoded)) await once(output, "drain");
}

export async function startServer(args, options = {}) {
  let broker;
  let authority;
  let runtime;
  let startupPreparation = null;
  let closing = false;
  let closePromise = null;
  const signalHandlers = new Map();
  let input;
  let dataHandler;
  let endHandler;
  let closeHandler;
  try {
    if (Number(process.versions.node.split(".")[0]) < 22) throw new BrokerError("node_version", "The constrained Codex broker requires Node.js 22 or newer.");
    const parsed = parseArgs(args);
    authority = options.authority ?? loadGrantAuthority({
      ceiling: parsed.ceiling,
      grantKeyFile: parsed.grantKeyFile,
      environment: options.environment ?? process.env,
      clock: options.clock ?? Date.now,
    });
    runtime = options.runtime ?? await CodexRuntime.create(parsed.projectRoot, options.runtimeOptions);
    broker = options.broker ?? new Broker(runtime, {
      ...options.brokerOptions,
      ceiling: parsed.ceiling,
      grantVerifier: authority.verifier,
      launcherEnvironment: authority.launcherEnvironment,
    });
    if (typeof broker.requestShutdown !== "function" || typeof broker.close !== "function") {
      throw new BrokerError("invalid_broker", "The MCP transport requires a broker with two-phase shutdown support.");
    }
    // Resolve and install once as this broker starts. Tool calls await the same frozen promise,
    // and shutdown drains it before removing the private runtime session.
    startupPreparation = Promise.resolve().then(() => runtime.prepareForNewWork()).catch(() => {});

    input = options.input ?? process.stdin;
    const output = options.output ?? process.stdout;
    const signalEmitter = options.signalEmitter ?? process;
    const maxInFlightRequests = options.maxInFlightRequests ?? MAX_IN_FLIGHT_REQUESTS;
    if (!Number.isInteger(maxInFlightRequests) || maxInFlightRequests < 1 || maxInFlightRequests > MAX_IN_FLIGHT_REQUESTS) {
      throw new BrokerError("invalid_transport_limit", "The trusted MCP in-flight request limit was invalid or exceeded its hard ceiling.");
    }
    const rpcContext = { initializeSeen: false, initialized: false };
    let buffer = "";
    let writeChain = Promise.resolve();
    let transportError = null;
    let transportClosing = false;
    let resolveTermination;
    const termination = new Promise((resolve) => { resolveTermination = resolve; });
    const inFlight = new Set();

    const closeBroker = () => {
      if (!closePromise) {
        closing = true;
        closePromise = Promise.resolve().then(() => broker.close());
      }
      return closePromise;
    };
    const beginShutdown = () => {
      if (transportClosing) return;
      transportClosing = true;
      input.pause?.();
      try { broker.requestShutdown(); }
      catch (error) { transportError ??= error; }
      resolveTermination();
    };
    const enqueueWrite = (message) => {
      const write = writeChain.then(() => writeLine(message, output));
      writeChain = write.catch((error) => {
        transportError ??= error;
        beginShutdown();
      });
      return writeChain;
    };
    const overloadId = (line) => {
      try {
        const parsed = JSON.parse(line);
        if ((typeof parsed?.id === "string" && parsed.id.length <= 128) || typeof parsed?.id === "number") return parsed.id;
      } catch {}
      return null;
    };
    const scheduleLine = (line) => {
      if (transportClosing) return;
      if (inFlight.size >= maxInFlightRequests) {
        void enqueueWrite(rpcError(overloadId(line), -32000, "Too many in-flight MCP requests"));
        beginShutdown();
        input.destroy?.();
        return;
      }
      let task;
      task = (async () => {
        const response = await processRpcLine(line, rpcContext, broker);
        if (response) await enqueueWrite(response);
      })().catch((error) => {
        transportError ??= error;
        beginShutdown();
      }).finally(() => {
        inFlight.delete(task);
      });
      inFlight.add(task);
    };

    input.setEncoding("utf8");
    dataHandler = (chunk) => {
      if (transportClosing) return;
      buffer += chunk;
      if (Buffer.byteLength(buffer) > 256 * 1024 && !buffer.includes("\n")) {
        void enqueueWrite(rpcError(null, -32700, "Protocol line too large"));
        beginShutdown();
        input.destroy?.();
        return;
      }
      let newline;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, newline).replace(/\r$/, "");
        buffer = buffer.slice(newline + 1);
        if (!line) continue;
        if (Buffer.byteLength(line) > 256 * 1024) {
          void enqueueWrite(rpcError(null, -32700, "Protocol line too large"));
          beginShutdown();
          input.destroy?.();
          break;
        }
        scheduleLine(line);
        if (transportClosing) break;
      }
    };
    input.on("data", dataHandler);

    endHandler = () => { beginShutdown(); };
    closeHandler = () => { beginShutdown(); };
    input.once("end", endHandler);
    input.once("close", closeHandler);
    for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
      const handler = () => { beginShutdown(); };
      signalHandlers.set(signal, { emitter: signalEmitter, handler });
      signalEmitter.once(signal, handler);
    }

    await termination;
    await Promise.allSettled([...inFlight, startupPreparation]);
    await writeChain;
    const closeResult = await Promise.allSettled([closeBroker()]);
    if (closeResult[0].status === "rejected") throw closeResult[0].reason;
    if (transportError) throw transportError;
  } catch (error) {
    process.stderr.write(`claude-codex-broker: ${boundedText(publicError(error).message, 500).replace(/[\r\n]+/g, " ")}\n`);
    process.exitCode = 1;
  } finally {
    if (input && dataHandler) input.removeListener("data", dataHandler);
    if (input && endHandler) input.removeListener("end", endHandler);
    if (input && closeHandler) input.removeListener("close", closeHandler);
    for (const [signal, { emitter, handler }] of signalHandlers) emitter.removeListener(signal, handler);
    if (!closing) {
      closing = true;
      await startupPreparation?.catch(() => {});
      if (broker) await broker.close().catch(() => {});
      else {
        await runtime?.cleanup?.().catch(() => {});
        authority?.verifier?.destroy?.();
      }
    }
  }
}

export { TOOLS, defaultWorkspaceSnapshot, dispatchTool, parseArgs };
