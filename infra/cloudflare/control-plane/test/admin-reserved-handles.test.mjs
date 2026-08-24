import assert from "node:assert/strict";
import test from "node:test";

import {
  ADMIN_HANDLE,
  RESERVED_HANDLES,
  validateMintHandle,
} from "../src/admin-handles.mjs";

// The AI support assistant posts under author_kind 'assistant' with the fixed
// handle 'assistant' (store constants MessageAuthorAssistant /
// AssistantHandle). The published support policy promises assistant replies
// are labeled and never presented as a human; if a person could mint the
// handle, their replies would render as the assistant and the promise breaks.
test("the assistant handle cannot be minted as a human admin", () => {
  assert.ok(RESERVED_HANDLES.has("assistant"));
  const refusal = validateMintHandle("assistant");
  assert.match(refusal ?? "", /reserved/);
});

// The handle passes the shape gate — the reservation is behavioral, not
// shape-based, so the shape regex alone must never be treated as the defense.
test("the assistant handle is shape-valid; only the reservation blocks it", () => {
  assert.ok(ADMIN_HANDLE.test("assistant"));
});

// Every actor_kind-colliding handle stays reserved; a refactor that rebuilds
// the set must not drop one.
test("actor-kind handles stay reserved", () => {
  for (const handle of [
    "system", "control_plane", "root", "admin", "fleet", "owner", "operator",
  ]) {
    assert.match(validateMintHandle(handle) ?? "", /reserved/, handle);
  }
});

// The refusal must be the reservation branch, not a general outage: ordinary
// handles mint, malformed ones get the shape message.
test("ordinary handles pass; malformed handles get the shape refusal", () => {
  assert.equal(validateMintHandle("sarah"), null);
  assert.equal(validateMintHandle("s2"), null);
  assert.match(validateMintHandle("UPPER") ?? "", /invalid handle/);
  assert.match(validateMintHandle("a") ?? "", /invalid handle/);
});
