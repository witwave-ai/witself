"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { dom, selfData, summaryData, nextTurn } = require("./overview_harness.cjs");
const schema = "witself.console.account.v1";
const sections = ["overview", "clients", "plan", "billing", "support", "access"];
const context = (role = "account_owner", extra = {}) => ({ schema_version: schema, available: true,
  account_id: "acct_fixture", operator_id: "op_fixture", role,
  sections: role === "account_operator" ? sections.filter(x => x !== "plan" && x !== "billing") : sections, ...extra });
const payload = (extra = {}) => ({ schema_version: schema, available: true, ...extra });
function fixture(t, hash = "#/overview", productionBoot = false) {
  const d = dom((condition, message) => assert.ok(condition, message));
  const listeners = new Map(), windowListeners = new Map(), timers = new Map(), requests = [], sources = [];
  const storage = [];
  let timerID = 0;
  if (productionBoot) {
    for (const id of ["theme-css", "theme-select"]) {
      const element = new d.nodes.view.constructor();
      element.setAttribute("id", id); d.nodes[id] = element;
    }
  }
  const originalGet = d.document.getElementById;
  d.document.getElementById = id => originalGet(id) || d.rail.querySelector("#" + id);
  d.document.hidden = false;
  const focusListeners = new Set();
  d.document.addEventListener = (event, fn) => event === "focusin" ? focusListeners.add(fn) : listeners.set(event, fn);
  d.document.removeEventListener = (event, fn) => event === "focusin" ? focusListeners.delete(fn) : listeners.delete(event);
  const Element = d.nodes.view.constructor;
  const focus = Element.prototype.focus;
  const body = d.document.body = new Element("body");
  for (const node of [...Object.values(d.nodes), d.rail]) { node.parentNode = body; body.children.push(node); }
  Object.defineProperty(Element.prototype, "isConnected", { get() {
    for (let node = this; node; node = node.parentNode) if (node === body) return true;
    return false;
  } });
  const active = Object.getOwnPropertyDescriptor(d.document, "activeElement").get;
  Object.defineProperty(d.document, "activeElement", { get: () => active()?.isConnected ? active() : body });
  Element.prototype.focus = function(options) {
    assert.ok(this.isConnected, "cannot focus a detached node");
    focus.call(this, options);
    for (const fn of focusListeners) fn({ target: this });
  };
  const window = { location: { hash, host: "fixture.invalid" }, matchMedia: null,
    addEventListener: (event, fn) => windowListeners.set(event, fn) };
  window.history = { replaceState(_state, _title, hash) { window.location.hash = hash; } };
  const sandbox = {
    module: { exports: {} }, window, document: d.document, AbortController, URLSearchParams,
    setTimeout(fn, delay) { const id = ++timerID; timers.set(id, { fn, delay }); return id; },
    clearTimeout(id) { timers.delete(id); },
    localStorage: { setItem(...args) { storage.push(args); }, getItem() { return null; } },
    sessionStorage: { setItem(...args) { storage.push(args); }, getItem() { return null; } },
    console: { log() { throw new Error("Unexpected debug log"); } },
    fetch(url, options) {
      if (!url.startsWith("/api/account/") && !url.startsWith("/api/secrets")) {
        assert.ok(["/api/self", "/api/summary", "/api/themes", "/api/prefs"].includes(url), "unexpected agent request: " + url);
        return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(url === "/api/self" ? selfData("fixture", 1) : url === "/api/themes" ? { themes: ["console"] } : url === "/api/prefs" ? {} : summaryData()) });
      }
      assert.equal(options.credentials, "same-origin");
      if (url.startsWith("/api/account/")) assert.equal(options.cache, "no-store");
      let resolve, reject;
      const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
      const request = { url, options, settled: false,
        async finish(data, status = 200) {
          assert.equal(this.settled, false);
          this.settled = true;
          resolve({ ok: status >= 200 && status < 300, status, json: () => Promise.resolve(data) });
          await nextTurn();
        },
        async fail() { assert.equal(this.settled, false); this.settled = true; reject(new Error("PRIVATE_RAW_ERROR")); await nextTurn(); },
      };
      requests.push(request);
      // Deliberately ignore abort: prove generation fencing independently of
      // cooperative transport cancellation, including headers arriving late.
      return promise;
    },
    EventSource: class {
      constructor(url) { this.url = url; this.listeners = new Map(); sources.push(this); }
      addEventListener(event, fn) { this.listeners.set(event, fn); }
      close() { this.closed = true; }
    },
  };
  if (productionBoot) delete sandbox.module;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8"), sandbox, { filename: "app.js" });
  const app = sandbox.module?.exports || {
    route: () => windowListeners.get("hashchange")(),
    account: { start() {}, stop: () => windowListeners.get("pagehide")() },
  };
  const h = { ...d, app, window, requests, timers, listeners, windowListeners, storage,
    pending(url) {
      const found = requests.filter(r => !r.settled && r.url === url);
      assert.equal(found.length, 1, "one pending " + url);
      return found[0];
    },
    async boot(data = context()) {
      app.account.start(); app.route();
      await h.pending("/api/account/context").finish(data);
      return h;
    },
    async check(data = context()) {
      const check = app.account.check();
      await h.pending("/api/account/context").finish(data);
      await check;
    },
    async navigate(hash) { window.location.hash = hash; app.route(); await nextTurn(); },
    async section(key, data, ticket) {
      await h.navigate("#/account/" + key + (ticket ? "/" + encodeURIComponent(ticket) : ""));
      await h.pending("/api/account/" + key + (ticket ? "/" + encodeURIComponent(ticket) : "")).finish(payload(data));
    },
    async visibility(hidden) { d.document.hidden = hidden; listeners.get("visibilitychange")(); await nextTurn(); },
    text() { return d.nodes.view.textContent; },
    accountLinks() { return d.rail.querySelectorAll("a").filter(a => a.getAttribute("data-nav") === "account"); },
    tick(delay) {
      const found = [...timers].filter(([, timer]) => timer.delay === delay);
      assert.equal(found.length, 1, "one independent timer at " + delay);
      timers.delete(found[0][0]); found[0][1].fn();
    },
  };
  t.after(async () => {
    app.account.stop();
    if (app.summaryState) app.summaryState.active = false;
    for (const request of requests) if (!request.settled) await request.finish(payload());
    for (const source of sources) source.close();
    timers.clear(); d.clearListeners();
    assert.ok(storage.every(([key, value]) => productionBoot && key === "witself-dashboard-theme" && value === "console"), "only existing theme preference may persist");
    assert.ok(requests.every(r => r.settled), "every request drained");
  });
  return h;
}
module.exports = { fixture, context, payload, sections, nextTurn };
