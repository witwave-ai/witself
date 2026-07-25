"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

test("disabled then enabled checkpoint reprobes and renders the mailbox", async () => {
  const nodes = {
    "view": { innerHTML: "" },
    "status-upstream": {
      textContent: "",
      removeAttribute() {},
    },
  };
  const requests = [];
  const eventSources = [];

  global.window = {
    location: { hash: "#/email" },
    matchMedia: null,
  };
  global.document = {
    getElementById(id) { return nodes[id] || null; },
  };
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
    if (path === "/api/email?limit=100") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            messages: [{
              subject: "available again",
              envelope_sender: "sender@example.test",
              received_at: "2026-07-24T12:00:00Z",
            }],
          };
        },
      };
    }
    throw new Error("unexpected fetch " + path);
  };
  global.EventSource = class {
    constructor(path) {
      this.path = path;
      eventSources.push(this);
    }
    addEventListener() {}
    close() {}
  };

  const app = require("../static/app.js");
  app.state.emailAddress = { address: "old@example.test" };
  app.state.emailMessages = [{ subject: "old" }];
  app.state.emailAvailable = true;

  app.applyEmailCheckpoint({ enabled: false, pending: false });
  assert.equal(app.state.emailAvailable, false);
  assert.equal(app.state.emailAddress, null);
  assert.deepEqual(app.state.emailMessages, []);
  assert.match(nodes.view.innerHTML, /not enabled on this account/);

  await app.applyEmailCheckpoint({ enabled: true, pending: false });
  assert.deepEqual(requests, ["/api/email/address", "/api/email?limit=100"]);
  assert.equal(app.state.emailAvailable, true);
  assert.equal(app.state.emailAddress.address, "agent@example.test");
  assert.equal(app.state.emailMessages[0].subject, "available again");
  assert.match(nodes.view.innerHTML, /agent@example\.test/);
  assert.match(nodes.view.innerHTML, /available again/);
  assert.equal(eventSources.length, 1);
  assert.match(eventSources[0].path, /^\/api\/events\?email=true/);

  await app.applyEmailCheckpoint({ enabled: true, pending: false });
  assert.deepEqual(requests, ["/api/email/address", "/api/email?limit=100"]);

  requests.length = 0;
  app.state.emailCheckpointEnabled = null;
  app.state.emailAddress = null;
  app.state.emailMessages = [];
  app.state.emailAvailable = null;
  app.state.lastSelfData = '{"email_checkpoint":{"enabled":true,"pending":false}}';
  let disabledAddressProbe = true;
  global.fetch = async (path) => {
    requests.push(path);
    if (path === "/api/email/address" && disabledAddressProbe) {
      disabledAddressProbe = false;
      return {
        ok: false,
        status: 403,
        async json() {
          return { error: "inbound email is not enabled on this account (feature_not_enabled)" };
        },
      };
    }
    if (path === "/api/email/address") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            address: {
              address: "first-frame@example.test",
              receive_state: "enabled",
              agent_receive_state: "enabled",
              realm_receive_state: "enabled",
            },
          };
        },
      };
    }
    if (path === "/api/email?limit=100") {
      return {
        ok: true,
        async json() {
          return { available: true, messages: [{ subject: "first enabled frame" }] };
        },
      };
    }
    throw new Error("unexpected fetch " + path);
  };

  await app.probeEmailMailbox();
  assert.equal(app.state.emailCheckpointEnabled, false);
  assert.equal(app.state.lastSelfData, null);
  assert.match(nodes.view.innerHTML, /not enabled on this account/);
  await app.applyEmailCheckpoint({ enabled: true, pending: false });
  assert.deepEqual(requests, [
    "/api/email/address",
    "/api/email/address",
    "/api/email?limit=100",
  ]);
  assert.equal(app.state.emailAddress.address, "first-frame@example.test");
  assert.match(nodes.view.innerHTML, /first enabled frame/);

  requests.length = 0;
  app.state.emailCheckpointEnabled = null;
  app.state.emailAddress = null;
  app.state.emailMessages = [];
  app.state.emailAvailable = null;
  app.state.lastSelfData = '{"email_checkpoint":{"enabled":true,"pending":false}}';
  let disabledListProbe = true;
  global.fetch = async (path) => {
    requests.push(path);
    if (path === "/api/email/address") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            address: {
              address: "list-race@example.test",
              receive_state: "enabled",
              agent_receive_state: "enabled",
              realm_receive_state: "enabled",
            },
          };
        },
      };
    }
    if (path === "/api/email?limit=100" && disabledListProbe) {
      disabledListProbe = false;
      return {
        ok: false,
        status: 403,
        async json() {
          return { error: "inbound email is not enabled on this account (feature_not_enabled)" };
        },
      };
    }
    if (path === "/api/email?limit=100") {
      return {
        ok: true,
        async json() {
          return { available: true, messages: [{ subject: "list race recovered" }] };
        },
      };
    }
    throw new Error("unexpected fetch " + path);
  };

  await app.probeEmailMailbox();
  assert.equal(app.state.emailCheckpointEnabled, false);
  assert.equal(app.state.lastSelfData, null);
  assert.match(nodes.view.innerHTML, /not enabled on this account/);
  await app.applyEmailCheckpoint({ enabled: true, pending: false });
  assert.deepEqual(requests, [
    "/api/email/address",
    "/api/email?limit=100",
    "/api/email/address",
    "/api/email?limit=100",
  ]);
  assert.equal(app.state.emailAddress.address, "list-race@example.test");
  assert.match(nodes.view.innerHTML, /list race recovered/);
});
