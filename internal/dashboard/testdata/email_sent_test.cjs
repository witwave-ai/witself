"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

const nodes = {
  "view": { innerHTML: "" },
  "agent-name": { textContent: "" },
  "realm-name": { textContent: "" },
  "agent-id": { textContent: "" },
  "version": { textContent: "" },
  "status-poll": { textContent: "" },
  "status-addr": { textContent: "" },
  "live-dot": { classList: { toggle() {} } },
  "live-label": { textContent: "" },
  "status-sse": { textContent: "" },
  "status-upstream": {
    textContent: "",
    title: "",
    removeAttribute() {},
  },
};

global.window = {
  location: { hash: "#/email" },
  matchMedia: null,
};
global.document = {
  getElementById(id) { return nodes[id] || null; },
};

const listeners = {};
const eventSources = [];
global.EventSource = class {
  constructor(path) {
    this.path = path;
    eventSources.push(this);
  }
  addEventListener(name, handler) { listeners[name] = handler; }
  close() {}
};

const app = require("../static/app.js");

function sentMessage(overrides = {}) {
  return Object.assign({
    from: "agent@witmail.net",
    reply_to: "reply@witmail.net",
    to: "recipient@example.test",
    subject: "release status",
    state: "delivered",
    provider_state: "delivered",
    error_code: "none",
    request_kind: "direct",
    attempt_count: 2,
    queued_at: "2026-08-17T12:00:00Z",
    provider_started_at: "2026-08-17T12:00:01Z",
    accepted_at: "2026-08-17T12:00:02Z",
    delivered_at: "2026-08-17T12:00:03Z",
    updated_at: "2026-08-17T12:00:03Z",
  }, overrides);
}

function resetState() {
  window.location.hash = "#/email";
  app.state.eventSource = null;
  app.state.sseTranscript = null;
  app.state.sseMessages = false;
  app.state.sseMemories = false;
  app.state.sseFacts = false;
  app.state.sseSecrets = false;
  app.state.sseEmail = false;
  app.state.sseEmailSent = false;
  app.state.sseEmailUnread = false;
  app.state.sseEmailUnacked = false;
  app.state.emailAvailable = false;
  app.state.emailAddress = null;
  app.state.emailStatus = null;
  app.state.emailMessages = [];
  app.state.emailReceiveUnavailableReason = "feature_disabled";
  app.state.emailReceiveDegraded = false;
  app.state.emailReceiveLiveRevision = 0;
  app.state.emailReceiveRequestRevision = 0;
  app.state.emailViewGeneration = 0;
  app.state.emailAddressRecoveryPending = false;
  app.state.emailCheckpointEnabled = null;
  app.state.emailSentAvailable = null;
  app.state.emailSentUnavailableReason = null;
  app.state.emailSentDegraded = false;
  app.state.emailSentLiveRevision = 0;
  app.state.emailSentMessages = [];
  app.state.lastEmailData = null;
  app.state.lastEmailSentData = null;
  nodes.view.innerHTML = "";
  eventSources.length = 0;
  for (const key of Object.keys(listeners)) { delete listeners[key]; }
}

test("sent email renders only escaped lifecycle metadata and no actions", async () => {
  resetState();
  const requests = [];
  global.fetch = async (path, options) => {
    requests.push({ path, options });
    assert.equal(path, "/api/email/sent?limit=100");
    return {
      ok: true,
      async json() {
        return {
          available: true,
          messages: [sentMessage({
            from: "<img src=x onerror=alert(1)>",
            to: "person+<script>alert(2)</script>@example.test",
            subject: "<svg onload=alert(3)>",
            request_kind: "reply<script>",
            provider_state: "accepted<img>",
            error_code: "provider_<b>failed</b>",
            attempt_count: 3.9,
            id: "esnd_must_not_render",
            account_id: "acc_must_not_render",
            owner_agent_id: "agt_must_not_render",
            provider: "provider_must_not_render",
            reply_to_inbound_message_id: "emsg_must_not_render",
            thread_key: "thread_must_not_render",
            text: "body_must_not_render",
          })],
        };
      },
    };
  };

  await app.refreshSentEmail();
  assert.deepEqual(requests.map((request) => request.path), ["/api/email/sent?limit=100"]);
  assert.deepEqual(requests[0].options, { credentials: "same-origin" });
  assert.equal(app.state.emailSentAvailable, true);

  const html = nodes.view.innerHTML;
  assert.match(html, /sent email/);
  assert.match(html, /newest 100 sent messages at most/);
  assert.match(html, /kind: reply&lt;script&gt;/);
  assert.match(html, /provider state: accepted&lt;img&gt;/);
  assert.match(html, /error code: provider_&lt;b&gt;failed&lt;\/b&gt;/);
  assert.match(html, /attempts: 3/);
  assert.match(html, /state: delivered/);
  assert.match(html, /&lt;svg onload=alert\(3\)&gt;/);
  assert.doesNotMatch(html, /<script|<svg|<img/i);
  for (const forbidden of [
    "esnd_must_not_render", "acc_must_not_render", "agt_must_not_render",
    "provider_must_not_render", "emsg_must_not_render", "thread_must_not_render",
    "body_must_not_render",
  ]) {
    assert.doesNotMatch(html, new RegExp(forbidden));
  }
  assert.doesNotMatch(html, /<button|<form|href=|data-action=/i);
});

test("sent live state is independent and recovers after a policy change", () => {
  resetState();
  app.openEmailEvents();
  assert.equal(eventSources.length, 1);
  assert.equal(eventSources[0].path, "/api/events?email_sent=true");
  assert.equal(typeof listeners.email_sent, "function");
  assert.equal(typeof listeners.upstream, "function");

  const first = sentMessage({ subject: "first lifecycle" });
  listeners.email_sent({ data: JSON.stringify({ available: true, messages: [first] }) });
  assert.equal(app.state.emailSentAvailable, true);
  assert.equal(app.state.emailSentMessages[0].subject, "first lifecycle");
  assert.match(nodes.view.innerHTML, /first lifecycle/);
  assert.match(nodes.view.innerHTML, /inbound email is not enabled on this account/);

  listeners.upstream({
    data: JSON.stringify({ source: "email_sent", ok: false, message: "fixed safe failure" }),
  });
  assert.equal(app.state.emailSentDegraded, true);
  assert.equal(app.state.emailSentMessages[0].subject, "first lifecycle");
  assert.match(nodes.view.innerHTML, /live sent-email refresh is temporarily unavailable/);

  listeners.upstream({ data: JSON.stringify({ source: "email_sent", ok: true, message: "" }) });
  assert.equal(app.state.emailSentDegraded, false);

  listeners.email_sent({
    data: JSON.stringify({ available: false, reason: "feature_disabled", messages: [] }),
  });
  assert.equal(app.state.emailSentAvailable, false);
  assert.deepEqual(app.state.emailSentMessages, []);
  assert.match(nodes.view.innerHTML, /outbound email is not enabled on this account/);
  assert.match(nodes.view.innerHTML, /no reinstall is required/);

  listeners.email_sent({
    data: JSON.stringify({
      available: true,
      messages: [sentMessage({ state: "accepted", subject: "re-enabled lifecycle" })],
    }),
  });
  assert.equal(app.state.emailSentAvailable, true);
  assert.equal(app.state.emailSentUnavailableReason, null);
  assert.match(nodes.view.innerHTML, /re-enabled lifecycle/);
  assert.match(nodes.view.innerHTML, /state: accepted/);
});

test("an EventSource reconnect reapplies unchanged healthy sent state", () => {
  resetState();
  app.openEmailEvents();
  const source = app.state.eventSource;
  const frame = JSON.stringify({
    available: true,
    messages: [sentMessage({ subject: "unchanged after reconnect" })],
  });

  listeners.email_sent({ data: frame });
  listeners.upstream({
    data: JSON.stringify({ source: "email_sent", ok: false, message: "fixed safe failure" }),
  });
  assert.equal(app.state.emailSentDegraded, true);

  source.onopen();
  listeners.email_sent({ data: frame });

  assert.equal(app.state.emailSentDegraded, false);
  assert.equal(app.state.emailSentMessages[0].subject, "unchanged after reconnect");
  assert.doesNotMatch(nodes.view.innerHTML, /temporarily unavailable/);
});

test("newer sent SSE state wins over a slower initial GET", async () => {
  resetState();
  let resolveGET;
  global.fetch = async (path) => {
    assert.equal(path, "/api/email/sent?limit=100");
    return new Promise((resolve) => { resolveGET = resolve; });
  };

  app.openEmailEvents();
  const pendingGET = app.refreshSentEmail();
  assert.equal(typeof resolveGET, "function");

  const newerFrame = JSON.stringify({
    available: true,
    messages: [sentMessage({ state: "delivered", subject: "newer SSE" })],
  });
  listeners.email_sent({ data: newerFrame });
  assert.equal(app.state.emailSentMessages[0].state, "delivered");

  resolveGET({
    ok: true,
    async json() {
      return {
        available: true,
        messages: [sentMessage({ state: "queued", subject: "older GET" })],
      };
    },
  });
  await pendingGET;

  assert.equal(app.state.emailSentMessages[0].state, "delivered");
  assert.equal(app.state.emailSentMessages[0].subject, "newer SSE");
  assert.match(nodes.view.innerHTML, /newer SSE/);
  assert.doesNotMatch(nodes.view.innerHTML, /older GET/);

  // The identical next live frame is intentionally de-duplicated; the state
  // must still remain current because the slower GET was never applied.
  listeners.email_sent({ data: newerFrame });
  assert.equal(app.state.emailSentMessages[0].state, "delivered");
  assert.equal(app.state.emailSentMessages[0].subject, "newer SSE");
});

test("newer received SSE state wins over a slower address reprobe", async () => {
  resetState();
  app.state.emailAvailable = true;
  app.state.emailAddress = {
    address: "current@witmail.net",
    receive_state: "enabled",
    agent_receive_state: "enabled",
    realm_receive_state: "enabled",
  };
  app.state.emailStatus = { maximum_raw_bytes: 1, attachment_capacity: { unlimited: true } };
  app.state.emailMessages = [{ subject: "previous" }];

  let resolveAddress;
  const requests = [];
  global.fetch = async (path) => {
    requests.push(path);
    assert.equal(path, "/api/email/address");
    return new Promise((resolve) => { resolveAddress = resolve; });
  };

  app.openEmailEvents();
  assert.match(eventSources[0].path, /email=true/);
  assert.equal(typeof listeners.email, "function");
  const pendingProbe = app.probeEmailMailbox();

  listeners.email({
    data: JSON.stringify({
      available: true,
      status: {
        maximum_raw_bytes: 2048,
        attachment_capacity: { used: 32, max: 1024, remaining: 992 },
      },
      messages: [{ subject: "newer received SSE" }],
    }),
  });

  resolveAddress({
    ok: true,
    async json() {
      return {
        available: true,
        address: {
          address: "older-probe@witmail.net",
          receive_state: "enabled",
          agent_receive_state: "enabled",
          realm_receive_state: "enabled",
        },
      };
    },
  });
  await pendingProbe;

  assert.deepEqual(requests, ["/api/email/address"]);
  assert.equal(app.state.emailAddress.address, "current@witmail.net");
  assert.equal(app.state.emailStatus.maximum_raw_bytes, 2048);
  assert.equal(app.state.emailMessages[0].subject, "newer received SSE");
  assert.match(nodes.view.innerHTML, /newer received SSE/);
  assert.doesNotMatch(nodes.view.innerHTML, /older-probe@witmail\.net/);
});

test("a slower initial address can fill an empty address without replacing newer mail", async () => {
  resetState();
  let resolveAddress;
  global.fetch = async (path) => {
    assert.equal(path, "/api/email/address");
    return new Promise((resolve) => { resolveAddress = resolve; });
  };

  app.openEmailEvents();
  const pendingProbe = app.probeEmailMailbox();
  listeners.email({
    data: JSON.stringify({
      available: true,
      status: { maximum_raw_bytes: 4096, attachment_capacity: {} },
      messages: [{ subject: "newer mail frame" }],
    }),
  });
  resolveAddress({
    ok: true,
    async json() {
      return {
        available: true,
        address: {
          address: "filled@witmail.net",
          receive_state: "enabled",
          agent_receive_state: "enabled",
          realm_receive_state: "enabled",
        },
      };
    },
  });
  await pendingProbe;

  assert.equal(app.state.emailAddress.address, "filled@witmail.net");
  assert.equal(app.state.emailStatus.maximum_raw_bytes, 4096);
  assert.equal(app.state.emailMessages[0].subject, "newer mail frame");
  assert.match(nodes.view.innerHTML, /filled@witmail\.net/);
  assert.match(nodes.view.innerHTML, /newer mail frame/);
});

test("receive checkpoint upgrades a sent-only stream when initial list is stale", async () => {
  resetState();
  app.state.emailCheckpointEnabled = true;
  let resolveStatus;
  let resolveList;
  global.fetch = async (path) => {
    if (path === "/api/email/address") {
      return {
        ok: true,
        async json() {
          return {
            available: true,
            address: {
              address: "agent@witmail.net",
              receive_state: "enabled",
              agent_receive_state: "enabled",
              realm_receive_state: "enabled",
            },
          };
        },
      };
    }
    if (path === "/api/email/status") {
      return new Promise((resolve) => { resolveStatus = resolve; });
    }
    if (path === "/api/email?limit=100") {
      return new Promise((resolve) => { resolveList = resolve; });
    }
    throw new Error("unexpected fetch " + path);
  };

  app.openEmailEvents();
  assert.equal(eventSources[0].path, "/api/events?email_sent=true");
  const pendingProbe = app.probeEmailMailbox();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(typeof resolveStatus, "function");
  assert.equal(typeof resolveList, "function");

  listeners.self({
    data: JSON.stringify({
      identity: { agent_name: "scott", realm_name: "default", agent_id: "agent_1" },
      email_checkpoint: {
        enabled: true,
        receive_state: "disabled",
        agent_receive_state: "enabled",
        realm_receive_state: "enabled",
      },
    }),
  });
  assert.equal(eventSources.length, 2);
  assert.match(eventSources[1].path, /email=true/);
  assert.match(eventSources[1].path, /email_sent=true/);

  resolveStatus({
    ok: true,
    async json() {
      return { available: true, status: { maximum_raw_bytes: 1, attachment_capacity: {} } };
    },
  });
  resolveList({
    ok: true,
    async json() { return { available: true, messages: [{ subject: "stale direct list" }] }; },
  });
  await pendingProbe;
  assert.deepEqual(app.state.emailMessages, []);
  assert.doesNotMatch(nodes.view.innerHTML, /stale direct list/);
});

test("sent pre-feature state clears previously loaded lifecycle rows", () => {
  resetState();
  app.state.emailSentAvailable = true;
  app.state.emailSentMessages = [sentMessage({ subject: "must be cleared" })];
  app.openEmailEvents();

  listeners.email_sent({
    data: JSON.stringify({ available: false, reason: "pre_feature", messages: [] }),
  });

  assert.equal(app.state.emailSentAvailable, false);
  assert.equal(app.state.emailSentUnavailableReason, "pre_feature");
  assert.deepEqual(app.state.emailSentMessages, []);
  assert.match(nodes.view.innerHTML, /sent email history is not available in this cell build/);
  assert.doesNotMatch(nodes.view.innerHTML, /must be cleared/);
  assert.doesNotMatch(nodes.view.innerHTML, /showing the last loaded sent metadata/);
});

test("a new email visit reapplies an unchanged sent frame after a direct failure", async () => {
  resetState();
  const unchangedFrame = JSON.stringify({
    available: true,
    messages: [sentMessage({ subject: "unchanged live lifecycle" })],
  });
  app.openEmailEvents();
  listeners.email_sent({ data: unchangedFrame });
  assert.equal(app.state.emailSentAvailable, true);

  window.location.hash = "#/overview";
  app.invalidateEmailView();
  const otherSource = new EventSource("/api/events");
  app.state.eventSource = otherSource;
  app.state.sseEmail = false;
  app.state.sseEmailSent = false;

  window.location.hash = "#/email";
  const currentGeneration = app.invalidateEmailView();
  app.openEmailEvents(currentGeneration);
  assert.equal(app.state.lastEmailSentData, null);
  global.fetch = async (path) => {
    assert.equal(path, "/api/email/sent?limit=100");
    return {
      ok: false,
      status: 502,
      async json() { return { error: "sent email upstream unavailable" }; },
    };
  };
  await app.refreshSentEmail(currentGeneration);
  assert.equal(app.state.emailSentAvailable, false);
  assert.equal(app.state.emailSentUnavailableReason, "upstream");

  listeners.email_sent({ data: unchangedFrame });
  assert.equal(app.state.emailSentAvailable, true);
  assert.equal(app.state.emailSentUnavailableReason, null);
  assert.equal(app.state.emailSentMessages[0].subject, "unchanged live lifecycle");
  assert.match(nodes.view.innerHTML, /unchanged live lifecycle/);
});

test("a probe from an earlier email visit cannot replace the current live stream", async () => {
  resetState();
  let resolveAddress;
  const requests = [];
  global.fetch = async (path) => {
    requests.push(path);
    assert.equal(path, "/api/email/address");
    return new Promise((resolve) => { resolveAddress = resolve; });
  };

  app.openEmailEvents();
  const staleProbe = app.probeEmailMailbox();
  assert.equal(typeof resolveAddress, "function");

  // Model a real leave-and-return: another panel owns a differently shaped
  // stream between two email view generations.
  window.location.hash = "#/conversations";
  app.invalidateEmailView();
  const conversationSource = new EventSource("/api/events?messages=true");
  app.state.eventSource = conversationSource;
  app.state.sseMessages = true;
  app.state.sseEmail = false;
  app.state.sseEmailSent = false;

  window.location.hash = "#/email";
  const currentGeneration = app.invalidateEmailView();
  app.openEmailEvents(currentGeneration);
  const currentEmailSource = app.state.eventSource;
  const sourceCount = eventSources.length;

  resolveAddress({
    ok: true,
    async json() {
      return {
        available: true,
        address: {
          address: "stale-visit@witmail.net",
          receive_state: "enabled",
          agent_receive_state: "enabled",
          realm_receive_state: "enabled",
        },
      };
    },
  });
  await staleProbe;

  assert.deepEqual(requests, ["/api/email/address"]);
  assert.equal(app.state.eventSource, currentEmailSource);
  assert.equal(eventSources.length, sourceCount);
  assert.equal(app.state.emailAddress, null);
});

test("a transient address failure keeps receive polling and heals without reload", async () => {
  resetState();
  let addressCalls = 0;
  const requests = [];
  global.fetch = async (path) => {
    requests.push(path);
    if (path === "/api/email/address") {
      addressCalls++;
      if (addressCalls === 1) {
        return {
          ok: false,
          status: 502,
          async json() { return { error: "receive upstream unavailable" }; },
        };
      }
      return {
        ok: true,
        async json() {
          return {
            available: true,
            address: {
              address: "recovered@witmail.net",
              receive_state: "enabled",
              agent_receive_state: "enabled",
              realm_receive_state: "enabled",
            },
          };
        },
      };
    }
    throw new Error("unexpected fetch " + path);
  };

  await app.probeEmailMailbox();
  assert.equal(app.state.emailAvailable, false);
  assert.equal(app.state.emailReceiveUnavailableReason, "upstream");
  assert.equal(app.state.emailAddressRecoveryPending, true);
  assert.match(app.state.eventSource.path, /email=true/);
  assert.match(app.state.eventSource.path, /email_sent=true/);

  listeners.email({
    data: JSON.stringify({
      available: true,
      status: { maximum_raw_bytes: 2048, attachment_capacity: {} },
      messages: [{ subject: "live recovery" }],
    }),
  });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(addressCalls, 2);
  assert.equal(app.state.emailAvailable, true);
  assert.equal(app.state.emailReceiveUnavailableReason, null);
  assert.equal(app.state.emailAddress.address, "recovered@witmail.net");
  assert.equal(app.state.emailAddressRecoveryPending, false);
  assert.equal(app.state.emailMessages[0].subject, "live recovery");
  assert.match(nodes.view.innerHTML, /recovered@witmail\.net/);
  assert.match(nodes.view.innerHTML, /live recovery/);
  assert.deepEqual(requests, ["/api/email/address", "/api/email/address"]);
});

test("a stale transient initial address failure starts one bounded recovery", async () => {
  resetState();
  let resolveInitialAddress;
  let addressCalls = 0;
  const requests = [];
  global.fetch = async (path) => {
    requests.push(path);
    assert.equal(path, "/api/email/address");
    addressCalls++;
    if (addressCalls === 1) {
      return new Promise((resolve) => { resolveInitialAddress = resolve; });
    }
    return {
      ok: true,
      async json() {
        return {
          available: true,
          address: {
            address: "bounded-recovery@witmail.net",
            receive_state: "enabled",
            agent_receive_state: "enabled",
            realm_receive_state: "enabled",
          },
        };
      },
    };
  };

  app.openEmailEvents();
  const pendingProbe = app.probeEmailMailbox();
  listeners.email({
    data: JSON.stringify({
      available: true,
      status: { maximum_raw_bytes: 8192, attachment_capacity: {} },
      messages: [{ subject: "newer live mail" }],
    }),
  });
  resolveInitialAddress({
    ok: false,
    status: 502,
    async json() { return { error: "receive upstream unavailable" }; },
  });
  await pendingProbe;

  assert.deepEqual(requests, ["/api/email/address", "/api/email/address"]);
  assert.equal(app.state.emailAddress.address, "bounded-recovery@witmail.net");
  assert.equal(app.state.emailAddressRecoveryPending, false);
  assert.equal(app.state.emailAvailable, true);
  assert.equal(app.state.emailStatus.maximum_raw_bytes, 8192);
  assert.equal(app.state.emailMessages[0].subject, "newer live mail");
  assert.match(nodes.view.innerHTML, /bounded-recovery@witmail\.net/);
  assert.match(nodes.view.innerHTML, /newer live mail/);
});

test("a slower stale filter response cannot replace the newest filter", async () => {
  resetState();
  app.state.emailAvailable = true;
  const pending = new Map();
  global.fetch = async (path) => new Promise((resolve) => {
    pending.set(path + "#" + pending.size, resolve);
  });

  const first = app.refreshEmail();
  app.state.emailFilters.unread = true;
  const second = app.refreshEmail();
  assert.equal(pending.size, 4);
  const entries = [...pending.entries()];

  entries[2][1]({
    ok: true,
    async json() { return { available: true, status: { maximum_raw_bytes: 200, attachment_capacity: {} } }; },
  });
  entries[3][1]({
    ok: true,
    async json() { return { available: true, messages: [{ subject: "new unread result" }] }; },
  });
  await second;

  entries[0][1]({
    ok: true,
    async json() { return { available: true, status: { maximum_raw_bytes: 100, attachment_capacity: {} } }; },
  });
  entries[1][1]({
    ok: true,
    async json() { return { available: true, messages: [{ subject: "older unfiltered result" }] }; },
  });
  await first;

  assert.equal(app.state.emailStatus.maximum_raw_bytes, 200);
  assert.equal(app.state.emailMessages[0].subject, "new unread result");
  assert.match(nodes.view.innerHTML, /new unread result/);
  assert.doesNotMatch(nodes.view.innerHTML, /older unfiltered result/);
});

test("newer receive frames do not discard an in-flight address recovery", async () => {
  resetState();
  let resolveRecoveryAddress;
  let addressCalls = 0;
  const requests = [];
  global.fetch = async (path) => {
    requests.push(path);
    assert.equal(path, "/api/email/address");
    addressCalls++;
    if (addressCalls === 1) {
      return {
        ok: false,
        status: 502,
        async json() { return { error: "receive upstream unavailable" }; },
      };
    }
    return new Promise((resolve) => { resolveRecoveryAddress = resolve; });
  };

  await app.probeEmailMailbox();
  assert.equal(app.state.emailAddressRecoveryPending, true);
  listeners.email({
    data: JSON.stringify({
      available: true,
      status: { maximum_raw_bytes: 2048, attachment_capacity: {} },
      messages: [{ subject: "first recovery frame" }],
    }),
  });
  assert.equal(typeof resolveRecoveryAddress, "function");

  // A second changed frame advances the receive-content revision while the
  // independent managed-address recovery is still pending.
  listeners.email({
    data: JSON.stringify({
      available: true,
      status: { maximum_raw_bytes: 4096, attachment_capacity: {} },
      messages: [{ subject: "newest recovery frame" }],
    }),
  });
  resolveRecoveryAddress({
    ok: true,
    async json() {
      return {
        available: true,
        address: {
          address: "late-recovered@witmail.net",
          receive_state: "enabled",
          agent_receive_state: "enabled",
          realm_receive_state: "enabled",
        },
      };
    },
  });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.deepEqual(requests, ["/api/email/address", "/api/email/address"]);
  assert.equal(app.state.emailAddress.address, "late-recovered@witmail.net");
  assert.equal(app.state.emailAddressRecoveryPending, false);
  assert.equal(app.state.emailStatus.maximum_raw_bytes, 4096);
  assert.equal(app.state.emailMessages[0].subject, "newest recovery frame");
  assert.match(nodes.view.innerHTML, /late-recovered@witmail\.net/);
  assert.match(nodes.view.innerHTML, /newest recovery frame/);
  assert.doesNotMatch(nodes.view.innerHTML, /first recovery frame/);
});
