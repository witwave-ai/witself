import fs from "node:fs/promises";
import path from "node:path";

import { startIsolatedWorkspaceMonitor } from "../../lib/workspace-monitor.mjs";

const [scratchRoot, workspaceRoot, mode = "normal"] = process.argv.slice(2);
const monitor = startIsolatedWorkspaceMonitor({
  handle: { workspaceRoot },
  runtime: { scratchRoot },
  intervalMs: 10,
  maxScanMs: 5_000,
  maxLogicalBytes: 16 * 1024,
  maxAllocatedBytes: 1024 * 1024,
  maxEntries: 100,
  minFreeBytes: 1,
});

try {
  await monitor.ready;
  if (mode === "after-remove") {
    const additionalRoot = path.join(scratchRoot, "implementation-session-liveness");
    await fs.mkdir(additionalRoot, { mode: 0o700 });
    await monitor.addRoot(additionalRoot);
    await monitor.sample();
    await monitor.removeRoot(additionalRoot);
  } else if (mode !== "normal") {
    throw new Error("Unknown workspace-monitor liveness fixture mode.");
  }
  await fs.writeFile(path.join(path.dirname(workspaceRoot), "over-quota.bin"), Buffer.alloc(32 * 1024, 0x63));

  try {
    await monitor.failure;
    throw new Error("The workspace monitor unexpectedly resolved its failure promise.");
  } catch (error) {
    if (error?.code !== "implementation_workspace_quota_exceeded") throw error;
  }
} finally {
  await monitor.stop();
}

process.stdout.write("quota failure observed\n");
