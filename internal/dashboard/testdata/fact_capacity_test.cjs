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

test("fact capacity HTML distinguishes unavailable, unlimited, and finite pressure", () => {
  assert.equal(app.factCapacityHTML(null), "");

  const unavailable = app.factCapacityHTML({ unavailable: true });
  assert.match(unavailable, /current fact capacity/);
  assert.match(unavailable, /temporarily unavailable/);
  assert.match(unavailable, /existing-fact updates remain available/);
  assert.doesNotMatch(unavailable, /<progress/);

  const unlimited = app.factCapacityHTML({
    used: 72,
    max: null,
    remaining: null,
    unlimited: true,
  });
  assert.match(unlimited, /72 current/);
  assert.match(unlimited, /unlimited/);
  assert.match(unlimited, /href="#\/facts"/);
  assert.doesNotMatch(unlimited, /<progress/);

  const available = app.factCapacityHTML({
    used: 899,
    max: 1000,
    remaining: 101,
  });
  assert.match(available, /899 of 1000 current/);
  assert.match(available, /101 remaining/);
  assert.match(available, /<span class="badge">available<\/span>/);
  assert.match(available, /max="1000" value="899"/);
  assert.doesNotMatch(available, /panel capacity warning/);
  assert.doesNotMatch(available, /panel capacity danger/);

  const near = app.factCapacityHTML({
    used: 900,
    max: 1000,
    remaining: 100,
    near_limit: true,
  });
  assert.match(near, /panel capacity warning/);
  assert.match(near, /<span class="badge">near limit<\/span>/);
  assert.match(near, /existing-fact updates and separately authorized deletion remain available at the limit/);

  const at = app.factCapacityHTML({
    used: 1000,
    max: 1000,
    remaining: 0,
    near_limit: true,
    at_limit: true,
  });
  assert.match(at, /panel capacity danger/);
  assert.match(at, /<span class="badge">at limit<\/span>/);
  assert.match(at, /max="1000" value="1000"/);

  const over = app.factCapacityHTML({
    used: 1001,
    max: 1000,
    remaining: 0,
    near_limit: true,
    over_limit: true,
  });
  assert.match(over, /panel capacity danger/);
  assert.match(over, /<span class="badge">over limit<\/span>/);
  assert.match(over, /1001 of 1000 current/);
  assert.match(over, /max="1000" value="1000"/);
});
