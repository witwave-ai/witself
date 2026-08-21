import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";

import { Broker, defaultWorkspaceSnapshot } from "../lib/broker.mjs";
import { createIsolatedWorkspace } from "../lib/isolated-workspace.mjs";
import { BrokerError } from "../lib/util.mjs";
import { startIsolatedWorkspaceMonitor } from "../lib/workspace-monitor.mjs";
import { makeTemp } from "./helpers.mjs";

const execFileAsync = promisify(execFile);

const REPORT = { summary: "done", findings: [], checks: [], blockers: [] };
const SYSTEM_REPORT = {
  summary: "system done", actions: ["completed bounded action"], checks: ["verified outcome"],
  changes: [], blockers: [], warnings: [],
};

function runtimeStub() {
  const info = {
    version: "1.2.3",
    integrity: "sha512-base",
    platformVersion: "1.2.3-fake",
    platformIntegrity: "sha512-platform",
    projectRoot: "/canonical/repo",
    scratchRoot: "/private/runtime/scratch",
    gitCommand: "/usr/bin/git",
    env: { HOME: "/private/empty" },
  };
  return {
    info,
    cleaned: false,
    async prepareForNewWork() { return info; },
    describe() { return info; },
    async cleanup() { this.cleaned = true; },
  };
}

function sessionStub(mode = "success") {
  return {
    shutdownCalled: false,
    interruptCalled: false,
    async start() {},
    async attest() { return { model: "gpt-5.6-sol", effort: "ultra", permissionProfile: "claude-review" }; },
    async runTurn(_task, { signal }) {
      if (mode === "failure") throw new Error("raw secret should not escape");
      if (mode === "hang") {
        return new Promise((resolve, reject) => {
          if (signal.aborted) reject(Object.assign(new Error("cancelled"), { code: "job_cancelled", publicMessage: "The delegated Codex review was cancelled." }));
          else signal.addEventListener("abort", () => reject(Object.assign(new Error("cancelled"), { code: "job_cancelled", publicMessage: "The delegated Codex review was cancelled." })), { once: true });
        });
      }
      if (mode === "blocked") return { ...REPORT, blockers: ["review could not inspect one optional path"] };
      return REPORT;
    },
    async verifyAuthUnchanged() {},
    async shutdown(options = {}) { await options.beforeScratchCleanup?.(); this.shutdownCalled = true; },
    async interrupt() { this.interruptCalled = true; this.shutdownCalled = true; },
  };
}

function systemSessionStub(mode = "success") {
  return {
    shutdownCalled: false,
    interruptCalled: false,
    async start() {},
    async attest() { return { model: "gpt-5.6-sol", effort: "ultra", permissionProfile: ":danger-full-access", sandbox: "dangerFullAccess" }; },
    async runTurn(_task, { signal }) {
      if (mode === "hang") {
        return new Promise((resolve, reject) => {
          if (signal.aborted) reject(Object.assign(new Error("cancelled"), { code: "job_cancelled", publicMessage: "The delegated Codex system task was cancelled." }));
          else signal.addEventListener("abort", () => reject(Object.assign(new Error("cancelled"), { code: "job_cancelled", publicMessage: "The delegated Codex system task was cancelled." })), { once: true });
        });
      }
      if (mode === "blocked") return { ...SYSTEM_REPORT, blockers: ["required action was not completed"] };
      if (mode === "empty") return { ...SYSTEM_REPORT, actions: [], checks: [], warnings: ["warning only"] };
      return SYSTEM_REPORT;
    },
    async verifyAuthUnchanged() {},
    async shutdown(options = {}) { await options.beforeScratchCleanup?.(); this.shutdownCalled = true; },
    async interrupt() { this.interruptCalled = true; this.shutdownCalled = true; },
  };
}

function implementationSessionStub(mode = "success") {
  return {
    scratch: "/private/runtime/scratch/implementation-session-test",
    shutdownCalled: false,
    interruptCalled: false,
    async start() {},
    async attest() {
      return {
        model: "gpt-5.6-sol", effort: "ultra", multiAgentVersion: "v2",
        permissionProfile: "claude-implementation", approvalPolicy: "never", networkAccess: false,
        confinement: { isolatedWorkspaceWrite: true, sourceWorktreeDenied: true },
      };
    },
    async runTurn(_task, { signal }) {
      if (mode === "failure") throw new Error("untrusted implementation detail");
      if (mode === "hang") {
        return new Promise((resolve, reject) => {
          const cancelled = () => reject(new BrokerError("job_cancelled", "The delegated Codex implementation was cancelled."));
          if (signal.aborted) cancelled();
          else signal.addEventListener("abort", cancelled, { once: true });
        });
      }
      if (mode === "blocked") {
        return { summary: "incomplete", actions: [], checks: [], blockers: ["cannot edit target"], warnings: [] };
      }
      if (mode === "empty") {
        return { summary: "unverified", actions: [], checks: [], blockers: [], warnings: ["warning only"] };
      }
      return { summary: "implemented", actions: ["edited clone"], checks: ["tested"], blockers: [], warnings: [] };
    },
    async verifyAuthUnchanged() {},
    async shutdown(options = {}) { await options.beforeScratchCleanup?.(); this.shutdownCalled = true; },
    async interrupt() { this.interruptCalled = true; this.shutdownCalled = true; },
  };
}

function isolatedHarness(overrides = {}) {
  const handle = Object.freeze({
    workspaceRoot: "/private/runtime/scratch/implementation-jobs/implementation-test/workspace",
    executionEnvironment: Object.freeze({ HOME: "/private/runtime/scratch/implementation-jobs/implementation-test/home", TMPDIR: "/private/runtime/scratch/implementation-jobs/implementation-test/tmp", PATH: "/usr/bin:/bin" }),
    originalHead: "1".repeat(40),
    baselineCommit: "2".repeat(40),
    sourceFingerprint: "3".repeat(64),
    sourceFileCount: 4,
    sourceBytes: 128,
  });
  const state = { finalized: 0, compacted: 0, cleaned: 0, cleanupAttempts: 0, artifactReads: 0, monitored: 0, evidence: null };
  const artifacts = {
    patch: { id: "changes.patch", mediaType: "text/x-diff", sizeBytes: 23, sha256: "4".repeat(64), maxChunkBytes: 49152 },
    evidence: { id: "evidence.bin", mediaType: "application/octet-stream", sizeBytes: 42, sha256: "5".repeat(64), maxChunkBytes: 49152 },
  };
  return {
    handle,
    state,
    artifacts,
    async createIsolatedWorkspace() { return handle; },
    async finalizeIsolatedWorkspace(received, { evidence }) {
      assert.equal(received, handle);
      state.finalized += 1;
      state.evidence = evidence;
      if (overrides.finalizeFailure) throw new Error("unsafe finalization detail");
      return {
        schemaVersion: 1, jobId: "11111111-1111-4111-8111-111111111111",
        originalHead: handle.originalHead, baselineCommit: handle.baselineCommit,
        resultTree: "6".repeat(40), changedFiles: ["src/app.mjs"], changedFileCount: 1,
        sourceDiverged: overrides.sourceDiverged ?? true, artifacts,
      };
    },
    async compactIsolatedWorkspace(received) {
      assert.equal(received, handle);
      state.compacted += 1;
      if (overrides.compactFailure) throw new Error("unsafe compaction detail");
      return { compacted: true, alreadyCompacted: false, retainedBytes: 165 };
    },
    async readIsolatedArtifact(received, options) {
      assert.equal(received, handle);
      state.artifactReads += 1;
      return { artifactId: options.artifactId, encoding: "base64", byteOffset: options.offset, nextByteOffset: options.offset, eof: true, data: "" };
    },
    async cleanupIsolatedWorkspace(received) {
      assert.equal(received, handle);
      state.cleanupAttempts += 1;
      if (overrides.cleanupFailureOnce && state.cleanupAttempts === 1) throw new Error("unsafe cleanup detail");
      state.cleaned += 1;
      return { cleaned: true };
    },
    workspaceMonitorFactory(received) {
      assert.equal(received, handle);
      state.monitored += 1;
      const policy = Object.freeze({
        enforcement: "periodic-best-effort-no-outer-filesystem-quota", intervalMs: 1000,
        maxLogicalBytes: 1024, maxAllocatedBytes: 1024, maxEntries: 100,
        specialFilesAllowed: false, instantDiskFillPrevented: false,
      });
      const evidence = () => Object.freeze({ ...policy, samples: 2, peak: { entries: 1, logicalBytes: 10, allocatedBytes: 512, unstableEntries: 0 } });
      return {
        ready: Promise.resolve(),
        failure: new Promise(() => {}),
        policy,
        async addRoot(candidate) { assert.equal(candidate, "/private/runtime/scratch/implementation-session-test"); },
        async removeRoot(candidate) { assert.equal(candidate, "/private/runtime/scratch/implementation-session-test"); },
        async sample() {},
        error() { return null; },
        evidence,
        async stop() { return evidence(); },
      };
    },
  };
}

async function waitForTerminal(broker, id, timeoutMs = 1000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const status = broker.status(id);
    if (["succeeded", "failed", "cancelled"].includes(status.state)) return status;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("job did not finish");
}

async function waitForImplementationTerminal(broker, id, timeoutMs = 1000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const status = broker.implementationStatus(id);
    if (["succeeded", "failed", "cancelled"].includes(status.state)) return status;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("implementation job did not finish");
}

test("successful asynchronous review returns bounded evidence only after postconditions", async () => {
  const runtime = runtimeStub();
  const session = sessionStub();
  let snapshots = 0;
  const broker = new Broker(runtime, {
    sessionFactory: () => session,
    snapshotFn: async () => ({ headCommit: "a".repeat(40), digest: "same", entries: snapshots++ }),
  });
  const started = broker.start("Review one boundary.");
  assert.equal(started.state, "preparing");
  assert.equal(Object.hasOwn(started, "result"), false);
  const done = await waitForTerminal(broker, started.job_id);
  assert.equal(done.state, "succeeded");
  assert.deepEqual(done.result, REPORT);
  assert.equal(done.workspace.unchanged, true);
  assert.equal(session.shutdownCalled, true);
  await broker.close();
  assert.equal(runtime.cleaned, true);
});

test("review blockers remain a successful diagnostic report", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    sessionFactory: () => sessionStub("blocked"),
    snapshotFn: async () => ({ headCommit: "a".repeat(40), digest: "same", entries: 0 }),
  });
  const done = await waitForTerminal(broker, broker.start("Review one boundary.").job_id);
  assert.equal(done.state, "succeeded");
  assert.deepEqual(done.result.blockers, ["review could not inspect one optional path"]);
  await broker.close();
});

test("workspace change overrides a successful model report and never undoes files", async () => {
  const runtime = runtimeStub();
  let count = 0;
  const broker = new Broker(runtime, {
    sessionFactory: () => sessionStub(),
    snapshotFn: async () => ({ headCommit: "b".repeat(40), digest: count++ === 0 ? "before" : "after", entries: count }),
  });
  const started = broker.start("Review stable state.");
  const done = await waitForTerminal(broker, started.job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "workspace_changed");
  assert.equal(Object.hasOwn(done, "result"), false);
  await broker.close();
});

test("unexpected implementation errors are value-safe", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    sessionFactory: () => sessionStub("failure"),
    snapshotFn: async () => ({ headCommit: "c".repeat(40), digest: "same", entries: 0 }),
  });
  const done = await waitForTerminal(broker, broker.start("Review.").job_id);
  assert.deepEqual(done.error, { code: "internal_error", message: "The broker encountered an internal error." });
  assert.equal(JSON.stringify(done).includes("raw secret"), false);
  await broker.close();
});

test("enforces two-operation concurrency and cancellation", async () => {
  const runtime = runtimeStub();
  const sessions = [];
  const broker = new Broker(runtime, {
    sessionFactory: () => { const session = sessionStub("hang"); sessions.push(session); return session; },
    snapshotFn: async () => ({ headCommit: "d".repeat(40), digest: "same", entries: 0 }),
  });
  const first = broker.start("Review A.");
  const second = broker.start("Review B.");
  assert.throws(() => broker.start("Review C."), (error) => error?.code === "concurrency_limit");
  await broker.cancel(first.job_id);
  await broker.cancel(second.job_id);
  assert.equal((await waitForTerminal(broker, first.job_id)).state, "cancelled");
  assert.equal((await waitForTerminal(broker, second.job_id)).state, "cancelled");
  assert.equal(sessions.every(({ interruptCalled, shutdownCalled }) => interruptCalled || shutdownCalled), true);
  await broker.close();
});

test("probe shares the concurrency ceiling and never starts a job", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, { probeFn: async (info) => ({ version: info.version, modelCalls: 0 }) });
  assert.deepEqual(await broker.probe(), {
    version: "1.2.3",
    modelCalls: 0,
    broker_ceiling: "repository",
    available_tools: ["codex_runtime_probe", "codex_review_start", "codex_review_status", "codex_review_cancel"],
  });
  assert.throws(() => broker.status("not-a-job"), (error) => error?.code === "invalid_job_id");
  await broker.close();
});

test("probe reports the immutable live ceiling and exact bounded tool catalog", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    ceiling: "system",
    probeFn: async () => ({ broker_ceiling: "untrusted", available_tools: ["raw_shell"], modelCalls: 0 }),
  });
  const result = await broker.probe();
  assert.equal(result.broker_ceiling, "system");
  assert.deepEqual(result.available_tools, broker.toolCatalog().map(({ name }) => name));
  assert.equal(result.available_tools.includes("codex_system_start"), true);
  assert.equal(result.available_tools.includes("raw_shell"), false);
  assert.equal(Object.isFrozen(result.available_tools), true);
  assert.equal(Object.isFrozen(result), true);
  await broker.close();
});

test("status long-poll returns on early terminal completion and clears its 30-second timer", async () => {
  const runtime = runtimeStub();
  let releaseTurn;
  const session = {
    ...sessionStub(),
    async runTurn() {
      return await new Promise((resolve) => { releaseTurn = () => resolve(REPORT); });
    },
  };
  const broker = new Broker(runtime, {
    sessionFactory: () => session,
    snapshotFn: async () => ({ headCommit: "9".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.start("Complete after the status wait begins.");
  const deadline = Date.now() + 1_000;
  while (broker.status(started.job_id).state !== "running" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.equal(typeof releaseTurn, "function");
  const immediate = broker.status(started.job_id, 0);
  assert.equal(immediate.state, "running");
  assert.equal(immediate instanceof Promise, false);
  const began = Date.now();
  const waiting = broker.status(started.job_id, 30);
  assert.equal(waiting instanceof Promise, true);
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 1);
  releaseTurn();
  const done = await waiting;
  assert.equal(done.state, "succeeded");
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 0);
  assert.equal(broker.activeOperations, 0, "terminal status is not observable until its operation slot is released");
  assert.ok(Date.now() - began < 1_000, "long-poll should resolve on completion rather than its 30-second timer");
  const alreadyTerminal = broker.status(started.job_id, 30);
  assert.equal(alreadyTerminal instanceof Promise, false);
  assert.equal(alreadyTerminal.state, "succeeded");
  await broker.close();
});

test("status long-poll returns current state on timeout and rejects malformed wait bounds", async () => {
  const runtime = runtimeStub();
  const hangingSession = sessionStub("hang");
  hangingSession.runTurn = async (_task, { signal }) => await new Promise((_resolve, reject) => {
    const cancelled = () => reject(new BrokerError("job_cancelled", "The delegated Codex review was cancelled."));
    if (signal.aborted) cancelled();
    else signal.addEventListener("abort", cancelled, { once: true });
  });
  const broker = new Broker(runtime, {
    sessionFactory: () => hangingSession,
    snapshotFn: async () => ({ headCommit: "8".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.start("Remain active through one bounded status wait.");
  const deadline = Date.now() + 1_000;
  while (broker.status(started.job_id).state !== "running" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  const began = Date.now();
  const timedWait = broker.status(started.job_id, 1);
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 1);
  const current = await timedWait;
  const elapsed = Date.now() - began;
  assert.equal(current.state, "running");
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 0, "timeout detaches its per-job waiter");
  assert.ok(elapsed >= 900, `one-second long-poll returned too early after ${elapsed}ms`);
  assert.ok(elapsed < 2_500, `one-second long-poll exceeded its bounded timer after ${elapsed}ms`);
  for (const malformed of [-1, 31, 0.5, "1", null, Number.NaN]) {
    assert.throws(() => broker.status(started.job_id, malformed), (error) => error?.code === "invalid_wait_seconds", String(malformed));
  }
  const cancelledWait = broker.status(started.job_id, 30);
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 1);
  await broker.cancel(started.job_id);
  assert.equal((await cancelledWait).state, "cancelled");
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 0, "cancel terminal completion drains waiters");
  await broker.close();
});

test("broker close cancels active work and drains every detachable status waiter", async () => {
  const runtime = runtimeStub();
  const session = sessionStub("hang");
  session.runTurn = async (_task, { signal }) => await new Promise((_resolve, reject) => {
    const cancelled = () => reject(new BrokerError("job_cancelled", "The delegated Codex review was cancelled."));
    if (signal.aborted) cancelled();
    else signal.addEventListener("abort", cancelled, { once: true });
  });
  const broker = new Broker(runtime, {
    sessionFactory: () => session,
    snapshotFn: async () => ({ headCommit: "7".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.start("Wait until broker shutdown.");
  const deadline = Date.now() + 1_000;
  while (broker.status(started.job_id).state !== "running" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  const waiting = broker.status(started.job_id, 30);
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 1);
  const closing = broker.close();
  const done = await waiting;
  await closing;
  assert.equal(done.state, "cancelled");
  assert.equal(broker.jobs.get(started.job_id).statusWaiters.size, 0);
  assert.equal(runtime.cleaned, true);
});

test("artifact reads hold an operation lease and shutdown defers cleanup until the read finishes", async () => {
  const runtime = runtimeStub();
  const jobId = "00000000-0000-4000-8000-000000000000";
  const handle = { marker: "retained-artifact" };
  let releaseRead;
  let markReadStarted;
  const readStarted = new Promise((resolve) => { markReadStarted = resolve; });
  const readPending = new Promise((resolve) => { releaseRead = resolve; });
  const events = [];
  const broker = new Broker(runtime, {
    ceiling: "system",
    readIsolatedArtifact: async (received) => {
      assert.equal(received, handle);
      events.push("read-start");
      markReadStarted();
      const result = await readPending;
      events.push("read-finish");
      return result;
    },
    cleanupIsolatedWorkspace: async (received) => {
      assert.equal(received, handle);
      events.push("cleanup");
    },
  });
  broker.jobs.set(jobId, {
    id: jobId,
    lane: "implementation",
    capabilityCeiling: "isolated-write",
    state: "succeeded",
    isolatedHandle: handle,
    artifacts: { patch: { id: "changes.patch" } },
    implementation: { finalized: true },
    statusWaiters: new Set(),
  });
  const reading = broker.implementationArtifactRead(jobId, "changes.patch", 0, 1024);
  await readStarted;
  assert.equal(broker.activeOperations, 1);
  assert.throws(() => broker.systemStart("Must remain exclusive."), (error) => error?.code === "system_exclusive");
  const closing = broker.close();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(runtime.cleaned, false);
  assert.deepEqual(events, ["read-start"]);
  releaseRead({ artifactId: "changes.patch", encoding: "base64", data: "", eof: true });
  assert.equal((await reading).artifactId, "changes.patch");
  await closing;
  assert.equal(broker.activeOperations, 0);
  assert.equal(runtime.cleaned, true);
  assert.deepEqual(events, ["read-start", "read-finish", "cleanup"]);
});

test("isolated implementation finalizes an explicit non-applied patch and retains bounded artifacts until close", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ sourceDiverged: true });
  const session = implementationSessionStub();
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => session,
    ...isolated,
  });
  const started = broker.implementationStart("Implement the bounded change.");
  assert.equal(started.lane, "implementation");
  assert.equal(started.capability_ceiling, "isolated-write");
  const done = await waitForImplementationTerminal(broker, started.job_id);
  assert.equal(done.state, "succeeded");
  assert.equal(done.workspace.sourceDiverged, true);
  assert.equal(done.workspace.changedFileCount, 1);
  assert.equal(done.workspace.compacted, true);
  assert.equal(done.workspace.retainedBytes, 165);
  assert.equal(done.implementation.finalized, true);
  assert.equal(done.implementation.compacted, true);
  assert.equal(done.implementation.retainedBytes, 165);
  assert.deepEqual(done.implementation.changedFiles, ["src/app.mjs"]);
  assert.equal(done.implementation.changedFilesTruncated, false);
  assert.deepEqual(done.artifacts, isolated.artifacts);
  assert.equal(done.artifacts.patch.id, "changes.patch");
  assert.match(done.artifacts.patch.sha256, /^[0-9a-f]{64}$/);
  assert.deepEqual(done.result, {
    summary: "implemented", actions: ["edited clone"], checks: ["tested"], blockers: [], warnings: [],
  });
  assert.equal(isolated.state.finalized, 1);
  assert.equal(isolated.state.compacted, 1);
  assert.equal(isolated.state.evidence.terminalStateBeforeFinalization, "succeeded");
  assert.throws(() => broker.status(started.job_id), (error) => error?.code === "job_not_found");
  const chunk = await broker.implementationArtifactRead(started.job_id, "changes.patch", 0, 1024);
  assert.equal(chunk.artifactId, "changes.patch");
  assert.equal(isolated.state.cleaned, 0);
  await broker.close();
  assert.equal(isolated.state.cleaned, 1);
  assert.equal(runtime.cleaned, true);
  assert.equal(session.shutdownCalled, true);
});

test("implementation blockers fail the action job while retaining bounded partial evidence", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ sourceDiverged: false });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub("blocked"),
    ...isolated,
  });
  const done = await waitForImplementationTerminal(
    broker, broker.implementationStart("Implement the required change.").job_id,
  );
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "implementation_task_blocked");
  assert.deepEqual(done.result.blockers, ["cannot edit target"]);
  assert.equal(done.implementation.finalized, true);
  assert.equal(isolated.state.evidence.terminalStateBeforeFinalization, "failed");
  assert.deepEqual(isolated.state.evidence.report.blockers, ["cannot edit target"]);
  await broker.close();
});

test("implementation warnings alone fail as unverified and preserve the structured report", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ sourceDiverged: false });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub("empty"),
    ...isolated,
  });
  const done = await waitForImplementationTerminal(
    broker, broker.implementationStart("Implement and verify the required change.").job_id,
  );
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "task_unverified");
  assert.deepEqual(done.result, {
    summary: "unverified", actions: [], checks: [], blockers: [], warnings: ["warning only"],
  });
  assert.equal(isolated.state.evidence.terminalStateBeforeFinalization, "failed");
  await broker.close();
});

test("cancelling during isolated capture aborts preparation and never starts an app server", async () => {
  const runtime = runtimeStub();
  let captureSignal;
  let sessionStarts = 0;
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    createIsolatedWorkspace: async ({ signal }) => {
      captureSignal = signal;
      return new Promise((resolve, reject) => {
        signal.addEventListener("abort", () => reject(new BrokerError(
          "isolated_workspace_aborted", "The isolated workspace operation was aborted.",
        )), { once: true });
      });
    },
    implementationSessionFactory: () => { sessionStarts += 1; return implementationSessionStub(); },
  });
  const started = broker.implementationStart("Prepare a bounded clone.");
  const deadline = Date.now() + 1000;
  while (!captureSignal && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 5));
  assert.ok(captureSignal);
  assert.equal((await broker.implementationCancel(started.job_id)).state, "cancelling");
  const done = await waitForImplementationTerminal(broker, started.job_id);
  assert.equal(done.state, "cancelled");
  assert.equal(done.error.code, "job_cancelled");
  assert.equal(captureSignal.aborted, true);
  assert.equal(sessionStarts, 0);
  await broker.close();
});

test("cancelled implementation finalizes a bounded partial patch, while unsafe finalization exposes no artifact", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ sourceDiverged: false });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub("hang"),
    ...isolated,
  });
  const started = broker.implementationStart("Begin a bounded change.");
  const deadline = Date.now() + 1000;
  while (broker.implementationStatus(started.job_id).state !== "running" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  await broker.implementationCancel(started.job_id);
  const cancelled = await waitForImplementationTerminal(broker, started.job_id);
  assert.equal(cancelled.state, "cancelled");
  assert.equal(cancelled.implementation.finalized, true);
  assert.equal(cancelled.artifacts.patch.id, "changes.patch");
  assert.equal(isolated.state.evidence.terminalStateBeforeFinalization, "cancelled");
  await broker.close();

  const unsafeRuntime = runtimeStub();
  const unsafe = isolatedHarness({ finalizeFailure: true });
  const unsafeBroker = new Broker(unsafeRuntime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub(),
    ...unsafe,
  });
  const unsafeDone = await waitForImplementationTerminal(unsafeBroker, unsafeBroker.implementationStart("change").job_id);
  assert.equal(unsafeDone.state, "failed");
  assert.equal(unsafeDone.error.code, "implementation_finalization_failed");
  assert.equal(unsafeDone.implementation.finalized, false);
  assert.equal(unsafeDone.artifacts, null);
  await assert.rejects(unsafeBroker.implementationArtifactRead(unsafeDone.job_id, "changes.patch", 0, 1024),
    (error) => error?.code === "implementation_artifact_unavailable");
  await unsafeBroker.close();
});

test("implementation compaction failure overrides success and exposes no retained artifact", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ compactFailure: true });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub(),
    ...isolated,
  });
  const done = await waitForImplementationTerminal(broker, broker.implementationStart("change").job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "implementation_compaction_failed");
  assert.equal(done.implementation.finalized, true);
  assert.equal(done.implementation.compacted, false);
  assert.equal(done.implementation.cleaned, true);
  assert.equal(done.artifacts, null);
  assert.equal(JSON.stringify(done).includes("unsafe compaction detail"), false);
  await assert.rejects(broker.implementationArtifactRead(done.job_id, "changes.patch", 0, 1024),
    (error) => error?.code === "implementation_artifact_unavailable");
  assert.equal(isolated.state.cleaned, 1);
  await broker.close();
  assert.equal(isolated.state.cleanupAttempts, 1);
});

test("failed immediate isolated cleanup is surfaced and retried safely during broker close", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness({ finalizeFailure: true, cleanupFailureOnce: true });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub(),
    ...isolated,
  });
  const done = await waitForImplementationTerminal(broker, broker.implementationStart("change").job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "implementation_cleanup_failed");
  assert.equal(done.implementation.cleaned, false);
  assert.equal(JSON.stringify(done).includes("unsafe cleanup detail"), false);
  assert.equal(isolated.state.cleanupAttempts, 1);
  await broker.close();
  assert.equal(isolated.state.cleanupAttempts, 2);
  assert.equal(isolated.state.cleaned, 1);
});

test("sequential implementation jobs eagerly remove large clone payloads while retaining readable artifacts", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const repository = path.join(temp, "source");
  const scratchRoot = path.join(temp, "broker-scratch");
  await fs.mkdir(repository, { mode: 0o700 });
  await fs.mkdir(scratchRoot, { mode: 0o700 });
  await execFileAsync("/usr/bin/git", ["init", "--quiet", repository]);
  await fs.writeFile(path.join(repository, "large.bin"), Buffer.alloc(1024 * 1024, 0x61));
  await execFileAsync("/usr/bin/git", ["-C", repository, "add", "large.bin"]);
  await execFileAsync("/usr/bin/git", ["-C", repository, "-c", "user.name=Broker Test", "-c", "user.email=broker@example.invalid", "commit", "--quiet", "-m", "base"]);

  const runtime = runtimeStub();
  runtime.info.projectRoot = repository;
  runtime.info.scratchRoot = scratchRoot;
  const handles = [];
  let sessionIndex = 0;
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => {
      const session = implementationSessionStub();
      session.scratch = path.join(scratchRoot, `implementation-session-${sessionIndex++}`);
      session.start = async () => { await fs.mkdir(session.scratch, { mode: 0o700 }); };
      session.shutdown = async (options = {}) => {
        await options.beforeScratchCleanup?.();
        session.shutdownCalled = true;
        await fs.rm(session.scratch, { recursive: true, force: true });
      };
      return session;
    },
    createIsolatedWorkspace: async (options) => {
      const handle = await createIsolatedWorkspace(options);
      handles.push(handle);
      return handle;
    },
  });

  for (let index = 0; index < 2; index += 1) {
    const done = await waitForImplementationTerminal(broker, broker.implementationStart(`change ${index}`).job_id, 20_000);
    assert.equal(done.state, "succeeded");
    assert.equal(done.implementation.compacted, true);
    assert.equal(done.implementation.retainedBytes <= done.artifacts.patch.sizeBytes + done.artifacts.evidence.sizeBytes + 4096, true);
    const handle = handles[index];
    const jobRoot = path.dirname(handle.workspaceRoot);
    assert.deepEqual((await fs.readdir(jobRoot)).sort(), [".witself-owner", "artifacts"]);
    await assert.rejects(fs.lstat(handle.workspaceRoot), { code: "ENOENT" });
    const chunk = await broker.implementationArtifactRead(done.job_id, "evidence.bin", 0, 4096);
    assert.equal(chunk.encoding, "base64");
    assert.equal(Buffer.from(chunk.data, "base64").length > 0, true);
  }

  const jobRoots = handles.map((handle) => path.dirname(handle.workspaceRoot));
  await broker.close();
  for (const jobRoot of jobRoots) await assert.rejects(fs.lstat(jobRoot), { code: "ENOENT" });
});

test("active workspace quota failure interrupts the app server and fails before artifact finalization succeeds", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness();
  const session = implementationSessionStub("hang");
  let monitorError = null;
  let rejectMonitor;
  const failure = new Promise((_, reject) => { rejectMonitor = reject; });
  const policy = Object.freeze({
    enforcement: "periodic-best-effort-no-outer-filesystem-quota", intervalMs: 10,
    maxLogicalBytes: 1024, maxAllocatedBytes: 1024, maxEntries: 10, minFreeBytes: 1,
    specialFilesAllowed: false, instantDiskFillPrevented: false,
  });
  const evidence = () => Object.freeze({ ...policy, samples: 3, peak: {
    entries: 11, logicalBytes: 2048, allocatedBytes: 4096, unstableEntries: 0, minimumFreeBytes: 1024 * 1024,
  } });
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => session,
    ...isolated,
    workspaceMonitorFactory: () => ({
      ready: Promise.resolve(), failure, policy,
      async addRoot() {}, async removeRoot() {}, async sample() {},
      error: () => monitorError, evidence, async stop() { return evidence(); },
    }),
  });
  const started = broker.implementationStart("grow an ignored build cache");
  const deadline = Date.now() + 1000;
  while (broker.implementationStatus(started.job_id).state !== "running" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  monitorError = new BrokerError("implementation_workspace_quota_exceeded", "The isolated implementation exceeded its active workspace quota.");
  rejectMonitor(monitorError);
  const done = await waitForImplementationTerminal(broker, started.job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "implementation_workspace_quota_exceeded");
  assert.equal(session.interruptCalled, true);
  assert.equal(done.workspace.monitor.enforcement, "periodic-best-effort-no-outer-filesystem-quota");
  assert.equal(done.workspace.monitor.instantDiskFillPrevented, false);
  assert.equal(done.implementation.compacted, true);
  await broker.close();
});

test("workspace monitor remains active until a background-writing app-server is fully stopped", async (t) => {
  const temp = await makeTemp();
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const repository = path.join(temp, "source");
  const scratchRoot = path.join(temp, "broker-scratch");
  await fs.mkdir(repository, { mode: 0o700 });
  await fs.mkdir(scratchRoot, { mode: 0o700 });
  await execFileAsync("/usr/bin/git", ["init", "--quiet", repository]);
  await fs.writeFile(path.join(repository, ".gitignore"), "ignored-during-shutdown.bin\n");
  await fs.writeFile(path.join(repository, "tracked.txt"), "base\n");
  await execFileAsync("/usr/bin/git", ["-C", repository, "add", ".gitignore", "tracked.txt"]);
  await execFileAsync("/usr/bin/git", ["-C", repository, "-c", "user.name=Broker Test", "-c", "user.email=broker@example.invalid", "commit", "--quiet", "-m", "base"]);
  const runtime = runtimeStub();
  runtime.info.projectRoot = repository;
  runtime.info.scratchRoot = scratchRoot;
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    workspaceMonitorFactory: (handle, runtimeInfo) => startIsolatedWorkspaceMonitor({
      handle, runtime: runtimeInfo, intervalMs: 10, maxScanMs: 5_000,
      maxLogicalBytes: 256 * 1024, maxAllocatedBytes: 16 * 1024 * 1024,
      maxEntries: 10_000, minFreeBytes: 1,
    }),
    implementationSessionFactory: (_runtime, handle) => {
      const session = implementationSessionStub();
      session.scratch = path.join(scratchRoot, `implementation-session-${crypto.randomUUID()}`);
      session.start = async () => { await fs.mkdir(session.scratch, { mode: 0o700 }); };
      session.shutdown = async (options = {}) => {
        await fs.writeFile(path.join(handle.workspaceRoot, "ignored-during-shutdown.bin"), Buffer.alloc(512 * 1024, 0x63));
        await new Promise((resolve) => setTimeout(resolve, 40));
        await options.beforeScratchCleanup?.();
        session.shutdownCalled = true;
        await fs.rm(session.scratch, { recursive: true, force: true });
      };
      return session;
    },
  });
  const done = await waitForImplementationTerminal(broker, broker.implementationStart("bounded change").job_id, 20_000);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "implementation_workspace_quota_exceeded");
  assert.equal(done.workspace.monitor.samples > 1, true);
  await broker.close();
});

test("broker lifetime job records and retained implementation bytes are hard bounded without eviction", async () => {
  const jobRuntime = runtimeStub();
  const jobBroker = new Broker(jobRuntime, {
    maxBrokerJobs: 2,
    sessionFactory: () => sessionStub(),
    snapshotFn: async () => ({ headCommit: "a".repeat(40), digest: "same", entries: 0 }),
  });
  const first = jobBroker.start("first");
  await waitForTerminal(jobBroker, first.job_id);
  const second = jobBroker.start("second");
  await waitForTerminal(jobBroker, second.job_id);
  assert.throws(() => jobBroker.start("third"), (error) => error?.code === "job_capacity_reached");
  assert.equal(jobBroker.status(first.job_id).state, "succeeded");
  assert.equal(jobBroker.status(second.job_id).state, "succeeded");
  await jobBroker.close();

  const artifactRuntime = runtimeStub();
  const isolated = isolatedHarness();
  const artifactBroker = new Broker(artifactRuntime, {
    ceiling: "isolated-write",
    maxImplementationArtifactBytes: 100,
    maxRetainedArtifactBytes: 200,
    implementationSessionFactory: () => implementationSessionStub(),
    ...isolated,
  });
  const artifactFirst = artifactBroker.implementationStart("first");
  await waitForImplementationTerminal(artifactBroker, artifactFirst.job_id);
  const artifactSecond = artifactBroker.implementationStart("second");
  await waitForImplementationTerminal(artifactBroker, artifactSecond.job_id);
  assert.throws(() => artifactBroker.implementationStart("third"), (error) => error?.code === "artifact_capacity_reached");
  assert.equal(artifactBroker.implementationStatus(artifactFirst.job_id).artifacts.patch.id, "changes.patch");
  assert.equal(artifactBroker.implementationStatus(artifactSecond.job_id).artifacts.patch.id, "changes.patch");
  await artifactBroker.close();
});

test("implementation jobs share concurrency and lane IDs without escaping a static ceiling", async () => {
  const runtime = runtimeStub();
  const isolated = isolatedHarness();
  const broker = new Broker(runtime, {
    ceiling: "isolated-write",
    implementationSessionFactory: () => implementationSessionStub("hang"),
    sessionFactory: () => sessionStub("hang"),
    snapshotFn: async () => ({ headCommit: "7".repeat(40), digest: "same", entries: 0 }),
    ...isolated,
  });
  assert.throws(() => { broker.ceiling = "system"; }, TypeError);
  const implementation = broker.implementationStart("hold implementation");
  const review = broker.start("hold review");
  assert.throws(() => broker.implementationStart("third"), (error) => error?.code === "concurrency_limit");
  assert.throws(() => broker.implementationStatus(review.job_id), (error) => error?.code === "job_not_found");
  assert.throws(() => broker.status(implementation.job_id), (error) => error?.code === "job_not_found");
  assert.throws(() => broker.systemStart("above ceiling"), (error) => error?.code === "tool_above_ceiling");
  await broker.implementationCancel(implementation.job_id);
  await broker.cancel(review.job_id);
  await waitForImplementationTerminal(broker, implementation.job_id);
  await waitForTerminal(broker, review.job_id);
  await broker.close();
});

test("system mutation is reported as evidence and does not invalidate a successful full-access result", async () => {
  const runtime = runtimeStub();
  let snapshots = 0;
  const broker = new Broker(runtime, {
    ceiling: "system",
    launcherEnvironment: Object.freeze({ HOME: "/real/home" }),
    systemSessionFactory: () => systemSessionStub(),
    snapshotFn: async () => ({ headCommit: snapshots === 0 ? "a".repeat(40) : "b".repeat(40), digest: snapshots++ === 0 ? "before" : "after", entries: 1 }),
  });
  assert.throws(() => { broker.ceiling = "repository"; }, TypeError);
  assert.equal(broker.ceiling, "system");
  const started = broker.systemStart("Apply the authorized change.");
  const done = await waitForTerminal({ status: (id) => broker.systemStatus(id) }, started.job_id);
  assert.equal(done.state, "succeeded");
  assert.deepEqual(done.result, SYSTEM_REPORT);
  assert.equal(done.lane, "system");
  assert.equal(done.capability_ceiling, "system");
  assert.equal(done.workspace.changed, true);
  assert.equal(done.workspace.evidenceComplete, true);
  assert.throws(() => broker.status(started.job_id), (error) => error?.code === "job_not_found");
  await broker.close();
});

test("system blockers fail the action job and expose no successful result", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    ceiling: "system",
    launcherEnvironment: Object.freeze({ HOME: "/real/home" }),
    systemSessionFactory: () => systemSessionStub("blocked"),
    snapshotFn: async () => ({ headCommit: "a".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.systemStart("Complete the required system action.");
  const done = await waitForTerminal({ status: (id) => broker.systemStatus(id) }, started.job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "system_task_blocked");
  assert.deepEqual(done.result.blockers, ["required action was not completed"]);
  await broker.close();
});

test("system warnings alone fail as unverified and preserve the structured report", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    ceiling: "system",
    launcherEnvironment: Object.freeze({ HOME: "/real/home" }),
    systemSessionFactory: () => systemSessionStub("empty"),
    snapshotFn: async () => ({ headCommit: "a".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.systemStart("Complete and verify the required system action.");
  const done = await waitForTerminal({ status: (id) => broker.systemStatus(id) }, started.job_id);
  assert.equal(done.state, "failed");
  assert.equal(done.error.code, "task_unverified");
  assert.deepEqual(done.result, {
    ...SYSTEM_REPORT, actions: [], checks: [], warnings: ["warning only"],
  });
  await broker.close();
});

test("system task is exclusive and remains cancellable while all new operations are blocked", async () => {
  const runtime = runtimeStub();
  const systemSession = systemSessionStub("hang");
  const broker = new Broker(runtime, {
    ceiling: "system",
    systemSessionFactory: () => systemSession,
    snapshotFn: async () => ({ headCommit: "e".repeat(40), digest: "same", entries: 0 }),
  });
  const started = broker.systemStart("Hold exclusive authority.");
  assert.throws(() => broker.start("review"), (error) => error?.code === "system_exclusive");
  assert.throws(() => broker.implementationStart("implementation"), (error) => error?.code === "system_exclusive");
  await assert.rejects(broker.probe(), (error) => error?.code === "system_exclusive");
  assert.throws(() => broker.systemStart("second"), (error) => error?.code === "system_exclusive");
  await broker.systemCancel(started.job_id);
  assert.equal((await waitForTerminal({ status: (id) => broker.systemStatus(id) }, started.job_id)).state, "cancelled");
  assert.equal(systemSession.interruptCalled || systemSession.shutdownCalled, true);
  await broker.close();
});

test("active review operations prevent a system task from starting", async () => {
  const runtime = runtimeStub();
  const broker = new Broker(runtime, {
    ceiling: "system",
    sessionFactory: () => sessionStub("hang"),
    systemSessionFactory: () => systemSessionStub(),
    snapshotFn: async () => ({ headCommit: "f".repeat(40), digest: "same", entries: 0 }),
  });
  const review = broker.start("hold review");
  assert.throws(() => broker.systemStart("must wait"), (error) => error?.code === "system_exclusive");
  await broker.cancel(review.job_id);
  await waitForTerminal(broker, review.job_id);
  await broker.close();
});

test("exact workspace fingerprint detects content changes to an already-dirty tracked file", async (t) => {
  const root = await makeTemp("claude-codex-snapshot-");
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const repo = path.join(root, "repo");
  await fs.mkdir(repo, { mode: 0o700 });
  await execFileAsync("/usr/bin/git", ["init", "-q", repo]);
  await fs.writeFile(path.join(repo, "tracked.txt"), "committed\n");
  await execFileAsync("/usr/bin/git", ["-C", repo, "add", "tracked.txt"]);
  await execFileAsync("/usr/bin/git", ["-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial"]);
  const runtime = { projectRoot: repo, gitCommand: "/usr/bin/git", env: { HOME: root } };
  await fs.writeFile(path.join(repo, "tracked.txt"), "dirty version one\n");
  const first = await defaultWorkspaceSnapshot(runtime);
  await fs.writeFile(path.join(repo, "tracked.txt"), "dirty version two\n");
  const second = await defaultWorkspaceSnapshot(runtime);
  assert.equal(first.entries, second.entries);
  assert.equal(first.files, second.files);
  assert.notEqual(first.digest, second.digest);
});

test("workspace fingerprint rejects an indexed path traversing a symlinked parent", async (t) => {
  const root = await makeTemp("claude-codex-symlink-parent-");
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const repo = path.join(root, "repo");
  const outside = path.join(root, "outside");
  await fs.mkdir(repo, { mode: 0o700 });
  await fs.mkdir(outside, { mode: 0o700 });
  await execFileAsync("/usr/bin/git", ["init", "-q", repo]);
  await fs.writeFile(path.join(repo, "base.txt"), "base\n");
  await execFileAsync("/usr/bin/git", ["-C", repo, "add", "base.txt"]);
  await execFileAsync("/usr/bin/git", ["-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial"]);
  await fs.writeFile(path.join(outside, "secret"), "outside\n");
  const blobSource = path.join(root, "blob-source");
  await fs.writeFile(blobSource, "tracked\n");
  const { stdout } = await execFileAsync("/usr/bin/git", ["-C", repo, "hash-object", "-w", blobSource]);
  await execFileAsync("/usr/bin/git", ["-C", repo, "update-index", "--add", "--cacheinfo", `100644,${stdout.trim()},escape/secret`]);
  await fs.symlink(outside, path.join(repo, "escape"));
  const runtime = { projectRoot: repo, gitCommand: "/usr/bin/git", env: { HOME: root } };
  await assert.rejects(defaultWorkspaceSnapshot(runtime), (error) => error?.code === "workspace_snapshot_unsafe");
});
