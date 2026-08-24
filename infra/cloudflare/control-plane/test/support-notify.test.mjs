import assert from "node:assert/strict";
import test from "node:test";

import { renderSupportEmail } from "../src/support-notify.mjs";

const ACCOUNT = "acct_notify";
const TICKET = "tkt_abc123";
const ADMIN_HANDLE = "harper-human-handle";
const BODY = `First line\nsecond   line from ${ADMIN_HANDLE}`;

test("human support replies keep the legacy wording", () => {
  const rendered = renderSupportEmail(
    "reply",
    "fleet_admin",
    ADMIN_HANDLE,
    ACCOUNT,
    TICKET,
    BODY,
  );

  const opening = `${ADMIN_HANDLE} from Witself support replied to your ticket.`;
  assert.equal(rendered.subject, `Support replied to your ticket ${TICKET}`);
  assert.equal(rendered.text.split("\n", 1)[0], opening);
  assert.ok(rendered.html.includes(opening));
  assert.ok(rendered.text.includes(`> First line second line from ${ADMIN_HANDLE}`));
});

test("assistant replies use assistant wording and never expose the admin handle", () => {
  const rendered = renderSupportEmail(
    "reply",
    "assistant",
    ADMIN_HANDLE,
    ACCOUNT,
    TICKET,
    BODY,
  );

  const opening = "The Witself support assistant replied to your ticket.";
  assert.equal(rendered.subject, `Support replied to your ticket ${TICKET}`);
  assert.equal(rendered.text.split("\n", 1)[0], opening);
  assert.ok(rendered.html.includes(opening));
  assert.ok(rendered.text.includes("> First line second line from [support assistant]"));
  for (const [format, value] of Object.entries(rendered)) {
    assert.ok(!value.includes(ADMIN_HANDLE), `${format} exposed admin handle`);
  }

  const markupCollision = renderSupportEmail(
    "reply",
    "assistant",
    "table",
    "acct-table",
    TICKET,
    "A table example.",
  );
  assert.ok(markupCollision.html.includes("<table"));
  assert.ok(markupCollision.html.includes("acct-table"));
  assert.ok(markupCollision.text.includes("acct-table"));
});

test("state-change notifications keep their legacy wording", () => {
  const resolved = renderSupportEmail(
    "resolved",
    undefined,
    ADMIN_HANDLE,
    ACCOUNT,
    TICKET,
    "",
  );
  const closed = renderSupportEmail(
    "closed",
    undefined,
    ADMIN_HANDLE,
    ACCOUNT,
    TICKET,
    "",
  );

  assert.equal(
    resolved.text.split("\n", 1)[0],
    `${ADMIN_HANDLE} from Witself support marked your ticket as resolved.`,
  );
  assert.equal(
    closed.text.split("\n", 1)[0],
    `${ADMIN_HANDLE} from Witself support closed your ticket.`,
  );
});
