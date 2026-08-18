"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

global.window = {
  location: { hash: "#/overview" },
  matchMedia: null,
};
global.document = {
  getElementById() { return null; },
};

const app = require("../static/app.js");

test("plan entitlement HTML distinguishes old, unmanaged, unavailable, and applied states", () => {
  const old = app.planEntitlementsHTML(null);
  assert.match(old, /Enforced plan &amp; entitlements/);
  assert.match(old, /not available on this cell version/);
  assert.doesNotMatch(old, /applied<\/span>/);

  const unmanaged = app.planEntitlementsHTML({ state: "unmanaged" });
  assert.match(unmanaged, /<span class="badge">unmanaged<\/span>/);
  assert.match(unmanaged, /no applied plan snapshot; no plan is implied/);
  assert.doesNotMatch(unmanaged, /features<\/h3>/);

  const unavailable = app.planEntitlementsHTML({ state: "unavailable" });
  assert.match(unavailable, /<span class="badge">unavailable<\/span>/);
  assert.match(unavailable, /temporarily unavailable/);

  const applied = app.planEntitlementsHTML({
    state: "applied",
    enforced_plan_id: "standard",
    features: {
      memory: true,
      facts: true,
      secrets: false,
      messaging: true,
      collaboration: false,
      agent_email_receive: true,
      agent_email_send: false,
      support: true,
      billing_admin: true,
    },
    retention_days: {
      transcript_retention_days: 90,
      message_retention_days: 30,
      agent_email_retention_days: null,
      billing_receipt_retention_days: 3650,
    },
  });
  assert.match(applied, /standard/);
  assert.match(applied, /<span class="badge">applied<\/span>/);
  assert.equal((applied.match(/<span class="badge">enabled<\/span>/g) || []).length, 4);
  assert.equal((applied.match(/<span class="badge">disabled<\/span>/g) || []).length, 3);
  assert.match(applied, /transcript_retention_days<\/span><span class="dim">90 days/);
  assert.match(applied, /agent_email_retention_days<\/span><span class="dim">indefinite/);
  assert.doesNotMatch(applied, /support/);
  assert.doesNotMatch(applied, /billing_admin/);
  assert.doesNotMatch(applied, /billing_receipt_retention_days/);
  assert.doesNotMatch(applied, /<a\b|<button\b|<form\b|<input\b/i);
});

test("plan entitlement rendering keeps all supplied text inert", () => {
  const html = app.planEntitlementsHTML({
    state: "applied",
    enforced_plan_id: `<img src=x onerror="alert(1)">`,
    features: {},
    retention_days: {
      transcript_retention_days: null,
      message_retention_days: null,
      agent_email_retention_days: null,
    },
  });
  assert.doesNotMatch(html, /<img\b/);
  assert.match(html, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
});
