"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const index = fs.readFileSync(path.join(__dirname, "../static/index.html"), "utf8");
const app = fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8");
const identityIDs = ["agent-name", "realm-name", "agent-id"];

// Run the real browser boot path with the real shell IDs. This small adapter
// models DOM events and focus, not browser layout, tab trapping, or native
// keyboard defaults. Escape's native dialog cancel event is dispatched below.
async function fixture(t, { missing, unsupported } = {}) {
  const nodes = new Map(), elements = [], requests = [], streams = [];
  let activeElement = null;
  class Element {
    constructor(tag, attrs = {}) {
      this.tag = tag;
      this.attrs = attrs;
      this.listeners = new Map();
      this.textContent = "";
      this.disabled = false;
      this.open = false;
      this.classList = { toggle() {}, add() {}, remove() {} };
    }
    getAttribute(name) { return this.attrs[name] ?? null; }
    setAttribute(name, value) { this.attrs[name] = String(value); }
    removeAttribute(name) { delete this.attrs[name]; }
    querySelectorAll(selector) {
      assert.equal(selector, ".message-preview");
      return []; // The fixture starts on an empty transcript list.
    }
    set innerHTML(value) {
      assert.ok(!identityIDs.includes(this.attrs.id), "identity must never enter an HTML sink");
      this.html = value;
    }
    addEventListener(type, listener) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(listener);
    }
    dispatch(type, fields = {}) {
      const event = { target: this, defaultPrevented: false,
        preventDefault() { this.defaultPrevented = true; }, ...fields };
      for (const listener of this.listeners.get(type) || []) listener(event);
      return event;
    }
    focus() { activeElement = this; }
    click() { if (!this.disabled) this.dispatch("click"); }
  }
  // Opening tags suffice for the shell event adapter; no identity content is
  // parsed here. Duplicate IDs fail before app initialization.
  for (const match of index.matchAll(/<([a-z][a-z0-9-]*)\b([^>]*)>/gi)) {
    const attrs = Object.fromEntries([...match[2].matchAll(/([\w-]+)="([^"]*)"/g)].map((m) => [m[1], m[2]]));
    const element = new Element(match[1], attrs);
    elements.push(element);
    if (attrs.id) {
      assert.ok(!nodes.has(attrs.id), "unique shell ID: " + attrs.id);
      nodes.set(attrs.id, element);
    }
  }
  if (missing) nodes.delete(missing);
  const dialog = nodes.get("avatar-dialog");
  if (dialog) {
    dialog.showModal = function () { assert.equal(this.open, false); this.open = true; };
    dialog.close = function () { this.open = false; this.dispatch("close"); };
    if (unsupported) dialog[unsupported] = undefined;
  }
  const window = { location: { hash: "#/transcripts", host: "localhost", search: "" }, addEventListener() {} };
  const document = {
    get activeElement() { return activeElement; },
    getElementById(id) { return nodes.get(id) || null; },
    querySelectorAll(selector) {
      assert.equal(selector, ".rail a");
      return elements.filter((element) => element.attrs["data-nav"]);
    },
    addEventListener() {},
  };
  const responses = {
    "/api/themes": { themes: ["console", "paper", "amber", "high-contrast", "midnight"] },
    "/api/prefs": { preferences: { prefs: { theme: "console" } } },
    "/api/transcripts": { transcripts: [] },
  };
  t.after(() => {
    for (const stream of streams) { stream.close(); stream.listeners.clear(); }
    for (const element of elements) element.listeners.clear();
  });
  vm.runInNewContext(app, {
    window, document, URLSearchParams,
    localStorage: { getItem() { return null; }, setItem() {} },
    async fetch(url, options) {
      requests.push(url);
      assert.ok(Object.hasOwn(responses, url), "unexpected request");
      assert.equal(options.credentials, "same-origin");
      assert.equal(options.method, undefined, "no mutations");
      return { ok: true, async json() { return responses[url]; } };
    },
    EventSource: class extends Element {
      constructor(url) { super("stream"); assert.equal(url, "/api/events"); streams.push(this); }
      close() { this.closed = true; }
    },
  }, { filename: "app.js" });
  // All fixture fetches resolve immediately; drain their app continuations.
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(nodes.get("status-addr").textContent, "localhost", "boot continued");
  assert.match(nodes.get("view").html, /no transcripts/, "existing panel still renders");
  return { nodes, elements, window, document, requests, streams };
}

test("portrait opens once, explicitly closes, and restores trigger focus", async (t) => {
  const h = await fixture(t);
  const trigger = h.nodes.get("avatar-trigger"), dialog = h.nodes.get("avatar-dialog"), close = h.nodes.get("avatar-close");
  assert.equal(trigger.tag, "button");
  assert.equal(trigger.attrs.type, "button");
  assert.ok(trigger.attrs["aria-label"]);
  assert.equal(trigger.attrs["aria-controls"], dialog.attrs.id);
  assert.equal(dialog.tag, "dialog");
  assert.ok(dialog.attrs["aria-label"]);
  assert.equal(close.tag, "button");
  const requests = [...h.requests];
  trigger.focus();
  trigger.click();
  assert.equal(dialog.open, true);
  assert.equal(h.document.activeElement, close);
  trigger.click(); // An already open dialog must not throw or reopen.
  close.click();
  assert.equal(dialog.open, false);
  assert.equal(h.document.activeElement, trigger);
  trigger.click();
  const cancel = dialog.dispatch("cancel"); // Native Escape boundary.
  assert.equal(cancel.defaultPrevented, true, "app owns explicit close");
  assert.equal(dialog.open, false);
  assert.equal(h.document.activeElement, trigger);
  assert.equal(h.window.location.hash, "#/transcripts");
  assert.deepEqual(h.requests, requests, "portrait interaction issues no fetches");
});

test("live malicious and long identity stays literal without changing image sources", async (t) => {
  const h = await fixture(t);
  const images = h.elements.filter((element) => element.tag === "img");
  assert.deepEqual(images.map((element) => element.attrs.src), ["/api/avatar.svg", "/api/avatar.svg"]);
  const values = ["<img src=x onerror=alert(1)>" + "long-name".repeat(200), "</span><script>alert(1)</script>", 'id-" onmouseover="alert(1)'];
  h.streams[0].dispatch("self", { data: JSON.stringify({ identity: {
    agent_name: values[0], realm_name: values[1], agent_id: values[2],
  }, dashboard_version: "test", poll_interval_ms: 2000 }) });
  assert.deepEqual(identityIDs.map((id) => h.nodes.get(id).textContent), values);
  assert.deepEqual(images.map((element) => element.attrs.src), ["/api/avatar.svg", "/api/avatar.svg"]);
  assert.equal(h.nodes.get("version").textContent, "vtest");
  assert.equal(h.nodes.get("status-poll").textContent, "poll 2s");
  h.streams[0].dispatch("self", { data: JSON.stringify({ identity: {} }) });
  assert.deepEqual(identityIDs.map((id) => h.nodes.get(id).textContent), ["(unnamed agent)", "", ""]);
});

for (const missing of ["avatar-trigger", "avatar-dialog", "avatar-close"]) {
  test("boot tolerates missing optional " + missing, async (t) => { await fixture(t, { missing }); });
}
for (const unsupported of ["showModal", "close"]) {
  test("boot disables enlargement without dialog " + unsupported, async (t) => {
    const h = await fixture(t, { unsupported });
    assert.equal(h.nodes.get("avatar-trigger").disabled, true);
    h.nodes.get("avatar-trigger").click();
    assert.equal(h.nodes.get("avatar-dialog").open, false);
  });
}
