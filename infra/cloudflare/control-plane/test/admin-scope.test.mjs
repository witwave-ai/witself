import assert from "node:assert/strict";
import test from "node:test";

import {
  ADMIN_SCOPES,
  ADMIN_SCOPE_FULL,
  ADMIN_SCOPE_SUPPORT_AI,
  adminScopeAllows,
} from "../src/admin-handles.mjs";

// The support runner's credential must reach exactly its ticket surface —
// list, read, reply-as-assistant, retriage, and the whoami round-trip — and
// nothing else. Everything the runner must never do (state changes, the
// support-policy kill-switch, fleet/cells administration, every non-ticket
// admin surface) refuses.
test("support_ai admits exactly the runner's surface", () => {
  for (const action of [
    "whoami", "list-tickets", "get-ticket", "reply-ticket", "retriage-ticket",
  ]) {
    assert.ok(adminScopeAllows(ADMIN_SCOPE_SUPPORT_AI, action), action);
  }
  for (const action of [
    "change-ticket-state", "support-policy", "cells", "fleet-admin-surface",
  ]) {
    assert.ok(!adminScopeAllows(ADMIN_SCOPE_SUPPORT_AI, action), action);
  }
});

// Default-deny: an unknown scope admits nothing, and an action nobody
// thought about is refused for every non-full scope — adding a new admin
// route without considering scopes fails closed.
test("unknown scopes and unlisted actions fail closed", () => {
  assert.ok(!adminScopeAllows("weird", "whoami"));
  assert.ok(!adminScopeAllows(ADMIN_SCOPE_SUPPORT_AI, "brand-new-route"));
});

// Pre-scope credentials (no scope stored) and explicit full keep the whole
// surface, including actions minted after them.
test("full and missing scope admit everything", () => {
  for (const scope of [ADMIN_SCOPE_FULL, undefined, null]) {
    for (const action of [
      "whoami", "list-tickets", "change-ticket-state",
      "support-policy", "cells", "fleet-admin-surface", "brand-new-route",
    ]) {
      assert.ok(adminScopeAllows(scope, action), `${scope} ${action}`);
    }
  }
});

// The mint enum is closed: exactly the two scopes exist.
test("the scope vocabulary is exactly full and support_ai", () => {
  assert.deepEqual([...ADMIN_SCOPES].sort(), ["full", "support_ai"]);
});
