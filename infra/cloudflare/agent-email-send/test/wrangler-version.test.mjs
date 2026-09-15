import test from "node:test";

import { assertWranglerVersion } from "../../wrangler-version.mjs";

test("installed Wrangler and all four Worker pins match the shared release", async () => {
  await assertWranglerVersion("agent-email-send");
});
