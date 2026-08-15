import assert from "node:assert/strict";
import test from "node:test";

import {
  LEGACY_PILOT_WORKER,
  PRODUCTION_RECEIVE_WORKER,
} from "../src/worker-names.mjs";

test("production receive and retired pilot use distinct Worker identities", () => {
  assert.equal(PRODUCTION_RECEIVE_WORKER, "witself-agent-email-receive");
  assert.equal(LEGACY_PILOT_WORKER, "witself-agent-email-pilot");
  assert.notEqual(PRODUCTION_RECEIVE_WORKER, LEGACY_PILOT_WORKER);
});
