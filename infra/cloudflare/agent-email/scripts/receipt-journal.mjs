import { randomUUID } from "node:crypto";
import {
  closeSync,
  fsyncSync,
  openSync,
  renameSync,
  unlinkSync,
  writeSync,
} from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

function writeExact(descriptor, value) {
  if (writeSync(descriptor, value, 0, "utf8") !== Buffer.byteLength(value)) {
    throw new Error("routing receipt journal write was incomplete");
  }
}

function fsyncParent(path) {
  const descriptor = openSync(dirname(path), "r");
  try {
    fsyncSync(descriptor);
  } finally {
    closeSync(descriptor);
  }
}

function writeNewSynced(path, value) {
  const descriptor = openSync(path, "wx", 0o600);
  let complete = false;
  try {
    writeExact(descriptor, `${JSON.stringify(value, null, 2)}\n`);
    fsyncSync(descriptor);
    complete = true;
  } finally {
    closeSync(descriptor);
    if (!complete) {
      try {
        unlinkSync(path);
      } catch {
        // Preserve the original write error. No external mutation has started
        // while the initial pending record is being reserved.
      }
    }
  }
}

// reserveJSONReceipt creates and directory-fsyncs a complete pending marker
// before any provider mutation. Commit writes a complete receipt to a new file,
// fsyncs it, atomically renames it over the pending marker, then fsyncs the
// parent directory. A crash therefore leaves either the complete pending
// marker or the complete final receipt, never an in-place truncated hybrid.
export function reserveJSONReceipt(path, pending) {
  if (typeof path !== "string" || !isAbsolute(path) || resolve(path) !== path) {
    throw new Error("routing receipt journal requires one canonical absolute path");
  }
  const absolute = resolve(path);
  writeNewSynced(absolute, pending);
  fsyncParent(absolute);
  let settled = false;
  return {
    commit(receipt) {
      if (settled) throw new Error("routing receipt journal was already settled");
      const temporary = `${absolute}.commit-${randomUUID()}`;
      let renamed = false;
      try {
        writeNewSynced(temporary, receipt);
        renameSync(temporary, absolute);
        renamed = true;
        fsyncParent(absolute);
        settled = true;
      } finally {
        if (!renamed) {
          try {
            unlinkSync(temporary);
          } catch {
            // The durable pending marker remains authoritative. Keep the
            // provider/application error rather than masking it with cleanup.
          }
        }
      }
    },
    close() {
      // Intentionally retain the complete pending marker. It is durable
      // evidence that an operator must reconcile provider state before retry.
      settled = true;
    },
  };
}
