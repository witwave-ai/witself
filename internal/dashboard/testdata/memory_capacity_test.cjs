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

test("memory capacity HTML distinguishes unavailable, unlimited, and finite pressure", () => {
  assert.equal(app.memoryCapacityHTML(null), "");

  const unavailable = app.memoryCapacityHTML({ unavailable: true });
  assert.match(unavailable, /active memory capacity/);
  assert.match(unavailable, /temporarily unavailable/);
  assert.doesNotMatch(unavailable, /<progress/);

  const unlimited = app.memoryCapacityHTML({
    used: 314,
    max: null,
    remaining: null,
    unlimited: true,
  });
  assert.match(unlimited, /314 active/);
  assert.match(unlimited, /unlimited/);
  assert.match(unlimited, /href="#\/memories"/);
  assert.doesNotMatch(unlimited, /<progress/);

  const available = app.memoryCapacityHTML({
    used: 899,
    max: 1000,
    remaining: 101,
  });
  assert.match(available, /899 of 1000 active/);
  assert.match(available, /101 remaining/);
  assert.match(available, /<span class="badge">available<\/span>/);
  assert.match(available, /max="1000" value="899"/);
  assert.doesNotMatch(available, /panel capacity warning/);
  assert.doesNotMatch(available, /panel capacity danger/);

  const near = app.memoryCapacityHTML({
    used: 900,
    max: 1000,
    remaining: 100,
    near_limit: true,
  });
  assert.match(near, /panel capacity warning/);
  assert.match(near, /<span class="badge">near limit<\/span>/);
  assert.match(near, /safe consolidation and replacement stay available at the limit/);

  const at = app.memoryCapacityHTML({
    used: 1000,
    max: 1000,
    remaining: 0,
    near_limit: true,
    at_limit: true,
  });
  assert.match(at, /panel capacity danger/);
  assert.match(at, /<span class="badge">at limit<\/span>/);
  assert.match(at, /max="1000" value="1000"/);

  const over = app.memoryCapacityHTML({
    used: 1001,
    max: 1000,
    remaining: 0,
    near_limit: true,
    over_limit: true,
  });
  assert.match(over, /panel capacity danger/);
  assert.match(over, /<span class="badge">over limit<\/span>/);
  assert.match(over, /1001 of 1000 active/);
  assert.match(over, /max="1000" value="1000"/);
});
