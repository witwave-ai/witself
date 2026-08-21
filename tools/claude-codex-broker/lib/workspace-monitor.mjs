import fs from "node:fs/promises";
import path from "node:path";
import process from "node:process";

import { BrokerError, isContained } from "./util.mjs";

const DEFAULT_INTERVAL_MS = 1_000;
const DEFAULT_SCAN_DEADLINE_MS = 15_000;
const DEFAULT_FREE_SPACE_RESERVE_BYTES = 512 * 1024 * 1024;

function sameDirectory(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.isDirectory() && right.isDirectory();
}

function safeLimit(value, fallback, maximum = Number.MAX_SAFE_INTEGER) {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved < 1 || resolved > maximum) {
    throw new BrokerError("implementation_workspace_monitor_invalid", "The isolated workspace monitor limits were invalid.");
  }
  return resolved;
}

async function canonicalPrivateRoot(candidate, boundary) {
  if (typeof candidate !== "string" || !path.isAbsolute(candidate) || path.normalize(candidate) !== candidate ||
      !isContained(boundary, candidate) || candidate === boundary) {
    throw new BrokerError("implementation_workspace_monitor_invalid", "A monitored isolated-workspace root escaped the broker scratch boundary.");
  }
  const [canonical, info] = await Promise.all([
    fs.realpath(candidate).catch(() => null),
    fs.lstat(candidate, { bigint: true }).catch(() => null),
  ]);
  if (!canonical || canonical !== candidate || !info?.isDirectory() || info.isSymbolicLink() ||
      (info.mode & 0o077n) !== 0n || (typeof process.getuid === "function" && info.uid !== BigInt(process.getuid()))) {
    throw new BrokerError("implementation_workspace_monitor_invalid", "A monitored isolated-workspace root failed canonical ownership or mode validation.");
  }
  return Object.freeze({ path: canonical, identity: Object.freeze({ dev: info.dev, ino: info.ino }) });
}

function addStat(totals, info) {
  totals.logicalBytes += info.size;
  if (typeof info.blocks !== "bigint" || info.blocks < 0n) {
    throw new BrokerError("implementation_workspace_monitor_unsupported", "Allocated filesystem blocks are unavailable for the isolated workspace monitor.");
  }
  totals.allocatedBytes += info.blocks * 512n;
}

function enforce(totals, limits) {
  if (totals.entries > BigInt(limits.maxEntries) || totals.logicalBytes > BigInt(limits.maxLogicalBytes) ||
      totals.allocatedBytes > BigInt(limits.maxAllocatedBytes)) {
    throw new BrokerError("implementation_workspace_quota_exceeded", "The isolated implementation exceeded its active workspace file or byte quota.");
  }
}

export async function scanIsolatedWorkspaceRoots(roots, options) {
  const startedAt = Date.now();
  const totals = { entries: 0n, logicalBytes: 0n, allocatedBytes: 0n, unstableEntries: 0 };
  const stack = [...roots];
  while (stack.length > 0) {
    if (Date.now() - startedAt > options.maxScanMs) {
      throw new BrokerError("implementation_workspace_monitor_deadline", "The isolated workspace quota scan exceeded its bounded deadline.");
    }
    const current = stack.pop();
    let before;
    try { before = await fs.lstat(current.path, { bigint: true }); }
    catch (error) {
      if (error?.code === "ENOENT" && current.optional) { totals.unstableEntries += 1; continue; }
      throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace directory disappeared or became unreadable.");
    }
    const canonical = await fs.realpath(current.path).catch(() => null);
    if (!canonical || canonical !== current.path || !sameDirectory(before, before) ||
        before.dev !== current.identity.dev || before.ino !== current.identity.ino) {
      throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace directory changed identity or escaped containment.");
    }
    addStat(totals, before);
    enforce(totals, options);
    let names;
    try { names = await fs.readdir(current.path); }
    catch (error) {
      if (error?.code === "ENOENT" && current.optional) { totals.unstableEntries += 1; continue; }
      throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace directory could not be enumerated safely.");
    }
    for (const name of names) {
      if (Date.now() - startedAt > options.maxScanMs) {
        throw new BrokerError("implementation_workspace_monitor_deadline", "The isolated workspace quota scan exceeded its bounded deadline.");
      }
      if (!name || name === "." || name === ".." || name.includes("/") || name.includes("\0")) {
        throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace entry name was unsafe.");
      }
      const child = path.join(current.path, name);
      if (!isContained(current.path, child) || child === current.path) {
        throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace entry escaped containment.");
      }
      let info;
      try { info = await fs.lstat(child, { bigint: true }); }
      catch (error) {
        if (error?.code === "ENOENT") { totals.unstableEntries += 1; continue; }
        throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace entry could not be inspected safely.");
      }
      totals.entries += 1n;
      enforce(totals, options);
      if (info.isDirectory()) {
        const childCanonical = await fs.realpath(child).catch(() => null);
        if (!childCanonical || childCanonical !== child || !isContained(current.root, childCanonical)) {
          throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace directory escaped through a link or rename.");
        }
        stack.push({ path: child, root: current.root, identity: { dev: info.dev, ino: info.ino }, optional: true });
      } else if (info.isFile() || info.isSymbolicLink()) {
        addStat(totals, info);
        enforce(totals, options);
      } else {
        throw new BrokerError("implementation_workspace_special_file", "The isolated implementation created an unsupported special filesystem entry.");
      }
    }
    const [after, canonicalAfter] = await Promise.all([
      fs.lstat(current.path, { bigint: true }).catch(() => null),
      fs.realpath(current.path).catch(() => null),
    ]);
    if (!after || canonicalAfter !== current.path || !sameDirectory(before, after)) {
      throw new BrokerError("implementation_workspace_monitor_changed", "A monitored isolated-workspace directory changed during its quota scan.");
    }
  }
  let minimumFreeBytes = Number.MAX_SAFE_INTEGER;
  const checkedDevices = new Set();
  for (const root of roots) {
    const device = String(root.identity.dev);
    if (checkedDevices.has(device)) continue;
    checkedDevices.add(device);
    let filesystem;
    try { filesystem = await (options.statfsImpl ?? fs.statfs)(root.path, { bigint: true }); }
    catch { throw new BrokerError("implementation_workspace_free_space_unavailable", "The broker could not attest free space for the isolated implementation volume."); }
    if (filesystem.bavail < 0n || filesystem.bsize <= 0n) {
      throw new BrokerError("implementation_workspace_free_space_unavailable", "The isolated implementation volume returned invalid free-space evidence.");
    }
    const freeBytes = filesystem.bavail * filesystem.bsize;
    const requiredFree = BigInt(options.maxAllocatedBytes) - totals.allocatedBytes + BigInt(options.minFreeBytes);
    if (freeBytes < requiredFree) {
      throw new BrokerError("implementation_workspace_free_space_low", "The isolated implementation volume lacks enough free space to preserve the broker reserve through its allocated-byte quota.");
    }
    minimumFreeBytes = Math.min(minimumFreeBytes, Number(freeBytes > BigInt(Number.MAX_SAFE_INTEGER) ? BigInt(Number.MAX_SAFE_INTEGER) : freeBytes));
  }
  for (const [key, value] of Object.entries(totals)) {
    if (typeof value === "bigint" && value > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new BrokerError("implementation_workspace_quota_exceeded", "The isolated implementation exceeded its active workspace quota.");
    }
    if (typeof value === "bigint") totals[key] = Number(value);
  }
  totals.minimumFreeBytes = minimumFreeBytes;
  return Object.freeze(totals);
}

export function startIsolatedWorkspaceMonitor({ handle, runtime, maxLogicalBytes, maxAllocatedBytes, maxEntries, intervalMs, maxScanMs, minFreeBytes, statfsImpl }) {
  if (!handle || typeof handle.workspaceRoot !== "string" || !runtime || typeof runtime.scratchRoot !== "string") {
    throw new BrokerError("implementation_workspace_monitor_invalid", "The isolated workspace monitor was configured without a broker-owned workspace.");
  }
  const limits = Object.freeze({
    maxLogicalBytes: safeLimit(maxLogicalBytes, 12 * 1024 * 1024 * 1024),
    maxAllocatedBytes: safeLimit(maxAllocatedBytes, 12 * 1024 * 1024 * 1024),
    maxEntries: safeLimit(maxEntries, 310_000, 2_000_000),
    intervalMs: safeLimit(intervalMs, DEFAULT_INTERVAL_MS, 60_000),
    maxScanMs: safeLimit(maxScanMs, DEFAULT_SCAN_DEADLINE_MS, 60_000),
    minFreeBytes: safeLimit(minFreeBytes, DEFAULT_FREE_SPACE_RESERVE_BYTES, 4 * 1024 * 1024 * 1024),
  });
  if (statfsImpl !== undefined && typeof statfsImpl !== "function") {
    throw new BrokerError("implementation_workspace_monitor_invalid", "The isolated workspace monitor free-space probe was invalid.");
  }
  const roots = [];
  let stopped = false;
  let timer = null;
  let current = null;
  let failureError = null;
  let readySettled = false;
  let samples = 0;
  let peak = Object.freeze({ entries: 0, logicalBytes: 0, allocatedBytes: 0, unstableEntries: 0, minimumFreeBytes: Number.MAX_SAFE_INTEGER });
  let resolveReady;
  let rejectReady;
  let rejectFailure;
  const ready = new Promise((resolve, reject) => { resolveReady = resolve; rejectReady = reject; });
  const failure = new Promise((_, reject) => { rejectFailure = reject; });
  void ready.catch(() => {});
  void failure.catch(() => {});

  const policy = Object.freeze({
    enforcement: "periodic-best-effort-no-outer-filesystem-quota",
    intervalMs: limits.intervalMs,
    maxLogicalBytes: limits.maxLogicalBytes,
    maxAllocatedBytes: limits.maxAllocatedBytes,
    maxEntries: limits.maxEntries,
    minFreeBytes: limits.minFreeBytes,
    specialFilesAllowed: false,
    instantDiskFillPrevented: false,
  });

  const fail = (error) => {
    if (failureError || stopped) return;
    failureError = error instanceof BrokerError
      ? error
      : new BrokerError("implementation_workspace_monitor_failed", "The active isolated workspace quota monitor failed closed.");
    if (!readySettled) { readySettled = true; rejectReady(failureError); }
    rejectFailure(failureError);
  };

  const tick = async () => {
    if (stopped || failureError) return;
    if (current) return current;
    if (timer) { clearTimeout(timer); timer = null; }
    current = (async () => {
      const sample = await scanIsolatedWorkspaceRoots([...roots], {
        maxLogicalBytes: limits.maxLogicalBytes,
        maxAllocatedBytes: limits.maxAllocatedBytes,
        maxEntries: limits.maxEntries,
        maxScanMs: limits.maxScanMs,
        minFreeBytes: limits.minFreeBytes,
        statfsImpl,
      });
      samples += 1;
      peak = Object.freeze({
        entries: Math.max(peak.entries, sample.entries),
        logicalBytes: Math.max(peak.logicalBytes, sample.logicalBytes),
        allocatedBytes: Math.max(peak.allocatedBytes, sample.allocatedBytes),
        unstableEntries: Math.min(Number.MAX_SAFE_INTEGER, peak.unstableEntries + sample.unstableEntries),
        minimumFreeBytes: Math.min(peak.minimumFreeBytes, sample.minimumFreeBytes),
      });
      samples = Math.min(Number.MAX_SAFE_INTEGER, samples);
      if (!readySettled) { readySettled = true; resolveReady(); }
    })();
    try { await current; }
    catch (error) { fail(error); }
    finally { current = null; }
    if (!stopped && !failureError) {
      timer = setTimeout(() => { void tick(); }, limits.intervalMs);
    }
  };

  const addRoot = async (candidate) => {
    if (stopped || failureError) throw failureError ?? new BrokerError("implementation_workspace_monitor_stopped", "The isolated workspace monitor was stopped.");
    const scratch = await fs.realpath(runtime.scratchRoot).catch(() => null);
    if (!scratch || scratch !== runtime.scratchRoot) {
      throw new BrokerError("implementation_workspace_monitor_invalid", "The broker scratch root was not canonical for workspace monitoring.");
    }
    const root = await canonicalPrivateRoot(candidate, scratch);
    if (roots.some((entry) => entry.path === root.path)) return;
    if (roots.some((entry) => isContained(entry.path, root.path) || isContained(root.path, entry.path))) {
      throw new BrokerError("implementation_workspace_monitor_invalid", "Monitored isolated-workspace roots overlapped.");
    }
    roots.push({ ...root, root: root.path, optional: false });
  };

  const removeRoot = async (candidate) => {
    if (typeof candidate !== "string" || !path.isAbsolute(candidate)) {
      throw new BrokerError("implementation_workspace_monitor_invalid", "A monitored isolated-workspace root removal was invalid.");
    }
    if (timer) { clearTimeout(timer); timer = null; }
    await current?.catch(() => {});
    if (timer) { clearTimeout(timer); timer = null; }
    const index = roots.findIndex((entry) => entry.path === candidate);
    if (index >= 0) roots.splice(index, 1);
    if (!stopped && !failureError) {
      timer = setTimeout(() => { void tick(); }, limits.intervalMs);
    }
  };

  current = (async () => {
    await addRoot(path.dirname(handle.workspaceRoot));
  })();
  void current.then(() => { current = null; void tick(); }, (error) => { current = null; fail(error); });

  return Object.freeze({
    ready,
    failure,
    policy,
    addRoot,
    removeRoot,
    sample: tick,
    error: () => failureError,
    evidence: () => Object.freeze({ ...policy, samples, peak }),
    async stop() {
      stopped = true;
      if (timer) clearTimeout(timer);
      await current?.catch(() => {});
      return Object.freeze({ ...policy, samples, peak });
    },
  });
}
