import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";

import { startIsolatedWorkspaceMonitor } from "../lib/workspace-monitor.mjs";
import { makeTemp } from "./helpers.mjs";

const execFileAsync = promisify(execFile);

async function monitorFixture(t, options = {}) {
  const root = await makeTemp("claude-codex-monitor-");
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const scratchRoot = path.join(root, "scratch");
  const jobRoot = path.join(scratchRoot, "implementation-11111111-1111-4111-8111-111111111111");
  const workspaceRoot = path.join(jobRoot, "workspace");
  await fs.mkdir(workspaceRoot, { recursive: true, mode: 0o700 });
  await fs.chmod(scratchRoot, 0o700);
  await fs.chmod(jobRoot, 0o700);
  await fs.chmod(workspaceRoot, 0o700);
  const monitor = startIsolatedWorkspaceMonitor({
    handle: { workspaceRoot },
    runtime: { scratchRoot },
    intervalMs: 10,
    maxScanMs: 5_000,
    maxLogicalBytes: 1024 * 1024,
    maxAllocatedBytes: 1024 * 1024,
    maxEntries: 100,
    minFreeBytes: 1,
    ...options,
  });
  t.after(() => monitor.stop());
  await monitor.ready;
  return { root, scratchRoot, jobRoot, workspaceRoot, monitor };
}

test("periodic monitor counts bounded roots without following outside symlink targets", async (t) => {
  const { root, jobRoot, monitor } = await monitorFixture(t);
  const outside = path.join(root, "outside-large.bin");
  await fs.writeFile(outside, Buffer.alloc(2 * 1024 * 1024, 0x61));
  await fs.symlink(outside, path.join(jobRoot, "outside-link"));
  await monitor.sample();
  assert.equal(monitor.error(), null);
  const evidence = monitor.evidence();
  assert.equal(evidence.enforcement, "periodic-best-effort-no-outer-filesystem-quota");
  assert.equal(evidence.instantDiskFillPrevented, false);
  assert.equal(evidence.specialFilesAllowed, false);
  assert.equal(evidence.peak.logicalBytes < 1024 * 1024, true);
  assert.equal(evidence.peak.minimumFreeBytes > 0, true);
});

test("periodic monitor fails when an ignored file grows past the logical quota", async (t) => {
  const { jobRoot, monitor } = await monitorFixture(t, { maxLogicalBytes: 16 * 1024 });
  await fs.writeFile(path.join(jobRoot, "ignored-cache.bin"), Buffer.alloc(32 * 1024, 0x62));
  await assert.rejects(monitor.failure, (error) => error?.code === "implementation_workspace_quota_exceeded");
  assert.equal(monitor.error()?.code, "implementation_workspace_quota_exceeded");
});

test("periodic monitor fails closed for special files", async (t) => {
  const { jobRoot, monitor } = await monitorFixture(t);
  await execFileAsync("/usr/bin/mkfifo", [path.join(jobRoot, "unexpected.fifo")]);
  await assert.rejects(monitor.failure, (error) => error?.code === "implementation_workspace_special_file");
});

test("free-space reserve is attested before a model turn", async (t) => {
  const lowSpace = async () => ({ bavail: 1n, bsize: 4096n });
  await assert.rejects(monitorFixture(t, {
    maxAllocatedBytes: 1024 * 1024,
    minFreeBytes: 1024 * 1024,
    statfsImpl: lowSpace,
  }), (error) => error?.code === "implementation_workspace_free_space_low");
});
