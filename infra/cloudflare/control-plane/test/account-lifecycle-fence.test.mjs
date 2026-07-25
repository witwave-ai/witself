import assert from "node:assert/strict";
import test from "node:test";

import {
  AccountLifecycleBusyError,
  AccountLifecycleFence,
} from "../src/account-lifecycle-fence.mjs";

test("serializes one account lifecycle operation across await boundaries", async () => {
  const fence = new AccountLifecycleFence();
  let release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  let entered = false;

  const first = fence.run(async () => {
    entered = true;
    await held;
    return "first";
  });
  assert.equal(entered, true);

  await assert.rejects(
    fence.run(async () => "second"),
    (error) => {
      assert.ok(error instanceof AccountLifecycleBusyError);
      return true;
    },
  );

  release();
  assert.equal(await first, "first");
  assert.equal(await fence.run(async () => "retry"), "retry");
});

test("releases the account lifecycle fence after failure", async () => {
  const fence = new AccountLifecycleFence();
  await assert.rejects(
    fence.run(async () => {
      throw new Error("operation failed");
    }),
    /operation failed/,
  );
  assert.equal(await fence.run(async () => "recovered"), "recovered");
});
