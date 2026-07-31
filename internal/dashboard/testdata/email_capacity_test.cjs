"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

global.window = {
  location: { hash: "#/email" },
  matchMedia: null,
};
global.document = {
  getElementById() { return null; },
};

const app = require("../static/app.js");

test("email storage HTML distinguishes unlimited and finite capacity pressure", () => {
  assert.equal(app.emailStorageStatusHTML(null), "");

  const unlimited = app.emailStorageStatusHTML({
    maximum_raw_bytes: 25 * 1024 * 1024,
    attachment_capacity: {
      used: 3 * 1024 * 1024,
      max: null,
      remaining: null,
      unlimited: true,
    },
  });
  assert.match(unlimited, /maximum raw message size: 25\.0 MiB/);
  assert.match(unlimited, /account-wide attachment capacity: 3\.0 MiB used/);
  assert.match(unlimited, /<span class="badge">unlimited<\/span>/);
  assert.doesNotMatch(unlimited, /<progress/);

  const available = app.emailStorageStatusHTML({
    maximum_raw_bytes: 1024 * 1024,
    attachment_capacity: {
      used: 4 * 1024,
      max: 8 * 1024,
      remaining: 4 * 1024,
      unlimited: false,
    },
  });
  assert.match(available, /maximum raw message size: 1\.0 MiB/);
  assert.match(available, /4\.0 KiB of 8\.0 KiB used/);
  assert.match(available, /4\.0 KiB remaining/);
  assert.match(available, /<span class="badge">available<\/span>/);
  assert.match(available, /max="8192" value="4096"/);
  assert.doesNotMatch(available, /panel capacity warning/);
  assert.doesNotMatch(available, /panel capacity danger/);

  const near = app.emailStorageStatusHTML({
    maximum_raw_bytes: 1024,
    attachment_capacity: {
      used: 900,
      max: 1000,
      remaining: 100,
      near_limit: true,
    },
  });
  assert.match(near, /panel capacity warning/);
  assert.match(near, /<span class="badge">near limit<\/span>/);

  const at = app.emailStorageStatusHTML({
    maximum_raw_bytes: 1024,
    attachment_capacity: {
      used: 1000,
      max: 1000,
      remaining: 0,
      near_limit: true,
      at_limit: true,
    },
  });
  assert.match(at, /panel capacity danger/);
  assert.match(at, /<span class="badge">at limit<\/span>/);
  assert.match(at, /max="1000" value="1000"/);

  const over = app.emailStorageStatusHTML({
    maximum_raw_bytes: 1024,
    attachment_capacity: {
      used: 1001,
      max: 1000,
      remaining: 0,
      near_limit: true,
      over_limit: true,
    },
  });
  assert.match(over, /panel capacity danger/);
  assert.match(over, /<span class="badge">over limit<\/span>/);
  assert.match(over, /1001 B of 1000 B used/);
  assert.match(over, /max="1000" value="1000"/);
});

test("email attachment omission warning requires the exact capacity marker", () => {
  assert.equal(
    app.emailPayloadRetentionWarning({ payload_retention_state: "omitted_capacity" }),
    "attachment payload omitted because account-wide capacity is full",
  );
  assert.equal(app.emailPayloadRetentionWarning({ payload_retention_state: "retained" }), "");
  assert.equal(app.emailPayloadRetentionWarning({ payload_retention_state: "omitted_policy" }), "");
  assert.equal(app.emailPayloadRetentionWarning(null), "");
});

test("email SSE applies policy-only capacity changes without browser polling", async () => {
  let rendered = "";
  let renderCount = 0;
  const nodes = {
    "view": {
      get innerHTML() { return rendered; },
      set innerHTML(value) {
        rendered = value;
        renderCount++;
      },
    },
    "status-upstream": {
      textContent: "",
      removeAttribute() {},
    },
  };
  global.document.getElementById = (id) => nodes[id] || null;
  global.window.location.hash = "#/email";

  const requests = [];
  const listeners = {};
  global.fetch = async (path) => {
    requests.push(path);
    if (path === "/api/email/address") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            address: {
              address: "agent@example.test",
              receive_state: "enabled",
              agent_receive_state: "enabled",
              realm_receive_state: "enabled",
            },
          };
        },
      };
    }
    if (path === "/api/email/status") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            status: {
              maximum_raw_bytes: 1024,
              attachment_capacity: {
                used: 1024,
                max: 4096,
                remaining: 3072,
                unlimited: false,
              },
            },
          };
        },
      };
    }
    if (path === "/api/email?limit=100") {
      return {
        ok: true,
        async json() {
          return { available: true, messages: [] };
        },
      };
    }
    throw new Error("unexpected fetch " + path);
  };
  global.EventSource = class {
    addEventListener(name, handler) { listeners[name] = handler; }
    close() {}
  };

  app.state.eventSource = null;
  app.state.lastEmailData = null;
  await app.probeEmailMailbox();
  assert.equal(app.state.emailStatus.attachment_capacity.max, 4096);
  assert.equal(typeof listeners.email, "function");

  requests.length = 0;
  const policyFrame = JSON.stringify({
    available: true,
    enrolled: true,
    messages: [],
    status: {
      maximum_raw_bytes: 2048,
      attachment_capacity: {
        used: 1024,
        max: 8192,
        remaining: 7168,
        unlimited: false,
      },
    },
  });
  listeners.email({ data: policyFrame });
  assert.equal(app.state.emailStatus.maximum_raw_bytes, 2048);
  assert.equal(app.state.emailStatus.attachment_capacity.max, 8192);
  assert.match(rendered, /1\.0 KiB of 8\.0 KiB used/);
  assert.deepEqual(requests, []);

  const rendersAfterChange = renderCount;
  listeners.email({ data: policyFrame });
  assert.equal(renderCount, rendersAfterChange);
});
