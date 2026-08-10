import assert from "node:assert/strict";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { reserveJSONReceipt } from "../scripts/receipt-journal.mjs";

test("receipt journal reserves, commits, and never overwrites another operation", () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-receipt-journal-"));
  try {
    const path = join(directory, "receipt.json");
    const pending = { schema: "pending.v1", state: "apply_started" };
    const receipt = { schema: "receipt.v1", outcome: "verified" };
    const journal = reserveJSONReceipt(path, pending);
    assert.deepEqual(JSON.parse(readFileSync(path, "utf8")), pending);
    assert.equal(statSync(path).mode & 0o777, 0o600);
    assert.throws(() => reserveJSONReceipt(path, pending), /EEXIST/);
    journal.commit(receipt);
    assert.deepEqual(JSON.parse(readFileSync(path, "utf8")), receipt);
    assert.equal(statSync(path).mode & 0o777, 0o600);
    assert.throws(() => journal.commit(receipt), /already settled/);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("receipt journal retains a complete pending marker on failed apply", () => {
  const directory = mkdtempSync(join(tmpdir(), "witself-receipt-journal-"));
  try {
    const path = join(directory, "pending.json");
    const pending = { schema: "pending.v1", state: "apply_started" };
    const journal = reserveJSONReceipt(path, pending);
    journal.close();
    assert.deepEqual(JSON.parse(readFileSync(path, "utf8")), pending);
    assert.equal(statSync(path).mode & 0o777, 0o600);
    assert.throws(() => journal.commit({}), /already settled/);
    assert.throws(
      () => reserveJSONReceipt("relative.json", pending),
      /canonical absolute path/,
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
