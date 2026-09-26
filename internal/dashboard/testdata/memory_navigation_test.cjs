"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const { summaryData, openDetails } = require("./overview_harness.cjs");

// Once all controlled fetch/JSON promises have settled, a full event-loop
// turn lets the route's real success/catch continuations finish without
// elapsed-time sleeps.
const nextTurn = () => new Promise((resolve) => setImmediate(resolve));

// This adapter supplies DOM operations only. The real app owns navigation,
// rendering, filtering and request decisions. Browser layout is not simulated.
function dom(check) {
  const elements = [], writes = [];
  const decode = (text) => text.replace(/&(?:amp|lt|gt|quot|#39|ndash);/g, (entity) => ({
    "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": '"', "&#39;": "'", "&ndash;": "–",
  }[entity]));
  let activeElement = null;
  class Element {
    constructor(tagName = "div") {
      this.tagName = tagName;
      this.attrs = {};
      this.children = [];
      this.parentNode = null;
      this.ownText = "";
      this.style = {};
      this.listeners = new Map();
      this.value = "";
      this.scrollTop = 0;
      this.classList = {
        contains: (name) => (this.attrs.class || "").split(/\s+/).includes(name),
        toggle: (name, force) => {
          const classes = new Set((this.attrs.class || "").split(/\s+/).filter(Boolean));
          const add = force === undefined ? !classes.has(name) : Boolean(force);
          if (add) classes.add(name); else classes.delete(name);
          this.attrs.class = [...classes].join(" ");
          return add;
        },
      };
      elements.push(this);
    }
    get hidden() { return Object.hasOwn(this.attrs, "hidden"); }
    set hidden(value) { if (value) this.attrs.hidden = ""; else delete this.attrs.hidden; }
    get id() { return this.attrs.id; }
    getAttribute(name) { return Object.hasOwn(this.attrs, name) ? this.attrs[name] : null; }
    setAttribute(name, value) {
      this.attrs[name] = String(value);
      if (name === "value") this.value = String(value);
    }
    removeAttribute(name) { delete this.attrs[name]; }
    addEventListener(name, listener) {
      if (!this.listeners.has(name)) this.listeners.set(name, []);
      this.listeners.get(name).push(listener);
    }
    dispatch(name, extra = {}) {
      const event = { target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, ...extra };
      for (const listener of this.listeners.get(name) || []) listener.call(this, event);
      return event;
    }
    focus() { activeElement = this; this.dispatch("focus"); }
    setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; }
    get textContent() { return this.ownText + this.children.map((child) => child.textContent).join(""); }
    set textContent(value) {
      for (const child of this.children) child.parentNode = null;
      this.children = [];
      this.ownText = String(value);
    }
    set innerHTML(value) {
      value = String(value);
      if (this.id === "view") writes.push(value);
      this.textContent = "";
      const stack = [this];
      for (const token of value.match(/<[^>]*>|[^<]+/g) || []) {
        if (token.startsWith("</")) {
          const closing = /^<\/([a-z0-9-]+)>$/i.exec(token);
          check(closing && stack.length > 1 && stack.at(-1).tagName === closing[1], "unbalanced closing tag");
          stack.pop();
        } else if (token.startsWith("<")) {
          const match = /^<([a-z0-9-]+)([^>]*)>$/i.exec(token);
          check(match, "unsupported markup");
          const element = new Element(match[1]);
          for (const attr of match[2].matchAll(/([a-z0-9_-]+)(?:="([^"]*)")?/gi)) {
            element.setAttribute(attr[1], decode(attr[2] || ""));
          }
          element.parentNode = stack.at(-1);
          stack.at(-1).children.push(element);
          if (!["input", "br", "hr", "img"].includes(element.tagName)) stack.push(element);
        } else {
          const element = new Element("#text");
          element.ownText = decode(token);
          element.parentNode = stack.at(-1);
          stack.at(-1).children.push(element);
        }
      }
      check(stack.length === 1, "unclosed markup");
    }
    matches(selector) {
      if (selector.startsWith("#")) return this.id === selector.slice(1);
      const [tag, ...classes] = selector.split(".");
      return (!tag || this.tagName === tag) && classes.every((name) => this.classList.contains(name));
    }
    querySelectorAll(selector) {
      return this.children.flatMap((child) => [
        ...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector),
      ]);
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    closest(selector) {
      for (let node = this; node; node = node.parentNode) if (node.matches(selector)) return node;
      return null;
    }
  }
  const nodes = Object.fromEntries([
    "view", "focus-status", "breadcrumb", "agent-name", "realm-name", "agent-id", "version",
    "status-poll", "status-addr", "status-upstream", "status-sse", "live-dot", "live-label",
  ].map((id) => {
    const element = new Element();
    element.setAttribute("id", id);
    return [id, element];
  }));
  // Read the actual rail markup; an empty collection or inert toggle would
  // make current-navigation assertions vacuous.
  const index = fs.readFileSync(path.join(__dirname, "../static/index.html"), "utf8");
  const railMarkup = /<nav class="rail">([\s\S]*?)<\/nav>/.exec(index);
  check(railMarkup, "missing real rail markup");
  const rail = new Element("nav");
  rail.innerHTML = railMarkup[1];
  check(rail.querySelectorAll("a").length === 7, "expected seven real navigation links");
  return {
    nodes, rail, writes,
    document: {
      scrollingElement: { scrollTop: 0 },
      get activeElement() { return activeElement; },
      getElementById(id) { return nodes[id] || nodes.view.querySelector("#" + id); },
      querySelectorAll(selector) {
        check(selector === ".rail a", "unexpected document selector");
        return rail.querySelectorAll("a");
      },
    },
    clearListeners() { for (const element of elements) element.listeners.clear(); },
  };
}

function deferred(check) {
  let resolve;
  const pending = { settled: false, promise: new Promise((yes) => { resolve = yes; }) };
  pending.resolve = (value) => {
    check(!pending.settled, "response settled twice");
    pending.settled = true;
    resolve(value);
  };
  return pending;
}

function fixture(t) {
  const faults = [], requests = [], sources = [];
  const check = (condition, message) => {
    if (condition) return;
    faults.push(message);
    throw new Error("memory navigation fixture: " + message);
  };
  const d = dom(check);
  const window = { location: { hash: "#/overview", host: "localhost" }, matchMedia: null };
  // Register the escape before loading the app. Even a failed semantic
  // assertion drains every held response and clears all inert stream handlers.
  t.after(async () => {
    const errors = [];
    try {
      for (const request of requests) {
        try { if (!request.headers.settled) request.releaseHeaders(); }
        catch (_) { errors.push("headers did not release"); }
      }
      await nextTurn();
      for (const request of requests) {
        try { if (!request.body.settled) request.body.resolve({}); }
        catch (_) { errors.push("JSON did not release"); }
      }
      await nextTurn();
      if (requests.some((request) => !request.headers.settled || !request.body.settled || request.jsonCalls !== 1)) {
        errors.push("responses did not drain");
      }
      if (faults.length) errors.push("fixture operation failed");
    } finally {
      for (const source of sources) {
        source.close();
        source.listeners.clear();
        source.onopen = null;
        source.onerror = null;
      }
      d.clearListeners();
    }
    assert.equal(errors.length, 0, "memory navigation fixture cleanup failed: " + errors.join(", "));
    assert.ok(sources.every((source) => source.closed && source.listeners.size === 0), "memory navigation fixture cleanup failed: streams remain");
    t.diagnostic("memory navigation fixture drained all owned responses and streams");
  });
  const summaryTimers = new Map();
  let nextSummaryTimer = 0;
  t.after(() => summaryTimers.clear());
  const sandbox = {
    module: { exports: {} }, window, document: d.document, URLSearchParams, AbortController,
    setTimeout(fn, delay) { const id = ++nextSummaryTimer; summaryTimers.set(id, { fn, delay }); return id; },
    clearTimeout(id) { summaryTimers.delete(id); },
    fetch(url, options) {
      check(url === "/api/summary" || typeof url === "string" && /^(?:\/api\/self|\/api\/memories\?limit=100|\/api\/memories\/mem_[a-z]+(?:\/history\?limit=50)?)$/.test(url), "unexpected fetch URL");
      check(options && options.credentials === "same-origin" && Object.keys(options).length === (url === "/api/summary" ? 2 : 1), "unexpected fetch options");
      const request = {
        url, headers: deferred(check), body: deferred(check), jsonCalls: 0, status: null,
        releaseHeaders(status = 200) {
          request.status = status;
          request.headers.resolve({
            ok: status >= 200 && status < 300, status,
            json() {
              check(request.jsonCalls === 0, "JSON read repeated");
              request.jsonCalls++;
              return request.body.promise;
            },
          });
        },
      };
      requests.push(request);
      if (url === "/api/summary") { request.releaseHeaders(); request.body.resolve(summaryData()); }
      return request.headers.promise;
    },
    EventSource: class {
      constructor(url) {
        check(url === "/api/events" || url === "/api/events?memories=true", "unexpected stream URL");
        this.url = url;
        this.closed = false;
        this.listeners = new Map();
        sources.push(this);
      }
      addEventListener(name, listener) { this.listeners.set(name, listener); }
      close() { this.closed = true; }
    },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8"), sandbox, { filename: "app.js" });
  const app = sandbox.module.exports;
  const h = {
    ...d, window, requests, sources,
    healthy() { assert.deepEqual(faults, [], "memory navigation fixture: recorded operation failure"); },
    begin(hash, urls) {
      const start = requests.length;
      window.location.hash = hash;
      app.route();
      h.healthy();
      const added = requests.slice(start);
      assert.deepEqual(added.map((request) => request.url), hash === "#/overview" ? ["/api/summary", ...urls] : urls, "memory navigation fixture: route request inventory");
      return added.filter((r) => r.url !== "/api/summary");
    },
    async headers(request, status = 200) {
      request.releaseHeaders(status);
      await nextTurn();
      h.healthy();
      assert.equal(request.jsonCalls, 1, "memory navigation fixture: JSON consumption must begin");
      assert.equal(request.body.settled, false, "memory navigation fixture: JSON must remain controlled");
    },
    async finish(request, body, status = 200) {
      if (!request.headers.settled) await h.headers(request, status);
      assert.equal(request.status, status, "memory navigation fixture: response status mismatch");
      assert.equal(request.jsonCalls, 1, "memory navigation fixture: JSON response was not consumed");
      request.body.resolve(body);
      await nextTurn();
      h.healthy();
    },
    settled(owned) {
      h.healthy();
      assert.ok(owned.every((request) => request.headers.settled && request.body.settled && request.jsonCalls === 1), "memory navigation fixture: incomplete controlled response");
    },
  };
  return h;
}

function navigation(h, hash, section, crumb) {
  assert.equal(h.window.location.hash, hash, "selected route hash");
  const links = h.rail.querySelectorAll("a");
  assert.equal(links.length, 7, "navigation assertions have real links");
  assert.deepEqual(links.filter((link) => link.classList.contains("active")).map((link) => link.getAttribute("data-nav")), [section], "only the selected rail link is active");
  assert.equal(links.find((link) => link.getAttribute("data-nav") === section).getAttribute("href"), "#/" + section, "active link represents the selected section");
  assert.equal(h.nodes.breadcrumb.textContent, crumb, "breadcrumb identifies the selected view");
}

function visible(h, hash, section, crumb, heading, content) {
  h.healthy();
  navigation(h, hash, section, crumb);
  assert.ok(h.writes.length > 0, "a real view render occurred");
  assert.equal(h.nodes.view.querySelector("h2")?.textContent, heading, "current view heading");
  assert.ok(h.nodes.view.textContent.includes(content), "current view contains its distinct response");
  assert.equal(h.nodes.view.querySelector(".error"), null, "current success is not an error");
}

function memory(id, content) {
  return { id, content, kind: "note", state: "active", salience: 0.5, version: 1, tags: [], evidence: [] };
}

function beginList(h) {
  const result = h.begin("#/memories", ["/api/memories?limit=100"]);
  navigation(h, "#/memories", "memories", "memories");
  return result;
}

function beginDetail(h, id) {
  const result = h.begin("#/memories/" + id, ["/api/memories/" + id, "/api/memories/" + id + "/history?limit=50"]);
  navigation(h, "#/memories/" + id, "memories", "memories / " + id);
  return result;
}

async function overview(h) {
  const [request] = h.begin("#/overview", ["/api/self"]);
  navigation(h, "#/overview", "overview", "overview");
  await h.finish(request, { identity: { agent_name: "Synthetic navigator" }, salient_memories: [{ id: "mem_overview", snippet: "current overview response" }] });
  openDetails(h);
  h.settled([request]);
  visible(h, "#/overview", "overview", "overview", "Agent summary", "current overview response");
}

async function finishList(h, owned, label) {
  await h.finish(owned[0], { items: [memory("mem_alpha", label)] });
  h.settled(owned);
}

async function finishDetail(h, owned, id, label, failure = null) {
  await h.finish(owned[0], failure === "detail" ? { error: label } : { memory: memory(id, label) }, failure === "detail" ? 503 : 200);
  await h.finish(owned[1], failure === "history" ? { error: label } : { versions: [{ version: 1, operation: label + " history", state: "active" }] }, failure === "history" ? 503 : 200);
  h.settled(owned);
}

function unchangedAfter(h, before, oracle) {
  h.healthy();
  // This is the intended old-source failure: the visible destination changed
  // only when the earlier visit's fully settled response reached the real app.
  assert.equal(h.nodes.view.textContent, before.content, oracle);
  navigation(h, before.hash, before.section, before.crumb);
  assert.deepEqual(h.nodes.view.querySelectorAll("h2").map((node) => node.textContent), before.headings, "current headings survive old response");
  assert.equal(h.writes.length, before.writes, "old response does not repaint the current view");
}

function currentDestination(t, h, hash, section, crumb) {
  navigation(h, hash, section, crumb);
  assert.equal(h.nodes.view.querySelector(".error"), null, "newer destination completed successfully");
  const before = {
    hash, section, crumb, content: h.nodes.view.textContent,
    headings: h.nodes.view.querySelectorAll("h2").map((node) => node.textContent), writes: h.writes.length,
  };
  assert.ok(before.content && before.headings.length && before.writes, "newer destination is nonempty before old response");
  t.diagnostic("memory navigation current destination established");
  return before;
}

const options = { timeout: 5000 };

test("memory navigation renders current inventory", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const owned = beginList(h);
  await h.finish(owned[0], { items: [memory("mem_alpha", "first inventory entry"), memory("mem_beta", "second inventory entry")] });
  h.settled(owned);
  visible(h, "#/memories", "memories", "memories", "Memories", "first inventory entry");
  assert.ok(h.nodes.view.textContent.includes("second inventory entry"), "current inventory has both distinct rows");
  const input = h.document.getElementById("filter-memories");
  assert.ok(input, "real renderer created the memory filter");
  input.value = "second";
  input.dispatch("input");
  assert.deepEqual(h.nodes.view.querySelectorAll(".row").map((row) => row.style.display), ["none", ""], "current inventory keeps its client-side filter");
  assert.equal(h.requests.length, 3, "filtering makes no additional request");
});

test("memory navigation renders current detail and history", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const owned = beginDetail(h, "mem_alpha");
  await finishDetail(h, owned, "mem_alpha", "current detail response");
  visible(h, "#/memories/mem_alpha", "memories", "memories / mem_alpha", "Memory mem_alpha", "current detail response");
  assert.ok(h.nodes.view.textContent.includes("current detail response history"), "current history response rendered");
});

for (const failure of ["inventory", "detail", "history"]) {
  test("memory navigation surfaces current " + failure + " error", options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const label = "current " + failure + " failure <literal>";
    if (failure === "inventory") {
      const owned = beginList(h);
      await h.finish(owned[0], { error: label }, 503);
      h.settled(owned);
      navigation(h, "#/memories", "memories", "memories");
    } else {
      const owned = beginDetail(h, "mem_alpha");
      await finishDetail(h, owned, "mem_alpha", label, failure);
      navigation(h, "#/memories/mem_alpha", "memories", "memories / mem_alpha");
    }
    assert.equal(h.nodes.view.querySelector(".error")?.textContent, label, "current memory failure remains visible as text");
    assert.equal(h.nodes.view.querySelector("literal"), null, "current error stays escaped");
  });
}

for (const sameHash of [false, true]) {
  for (const outcome of ["success", "error"]) {
    const name = sameHash
      ? "memory navigation ignores old inventory " + outcome + " after returning to same hash"
      : "memory navigation ignores stale inventory " + outcome + " after leaving";
    test(name, options, async (t) => {
      const h = fixture(t);
      await overview(h);
      const old = beginList(h);
      // The different-panel cases hold the entire fetch. Same-hash cases
      // instead consume headers now and delay JSON until the later visit.
      if (sameHash) await h.headers(old[0], outcome === "error" ? 503 : 200);
      await overview(h);
      if (sameHash) {
        const current = beginList(h);
        await finishList(h, current, "new inventory visit");
        visible(h, "#/memories", "memories", "memories", "Memories", "new inventory visit");
      }
      const before = currentDestination(t, h, sameHash ? "#/memories" : "#/overview", sameHash ? "memories" : "overview", sameHash ? "memories" : "overview");
      if (outcome === "error") await h.finish(old[0], { error: "old inventory failure" }, 503);
      else await finishList(h, old, "old inventory response");
      h.settled(old);
      unchangedAfter(h, before, (sameHash ? "prior-visit" : "stale") + " inventory " + outcome + " replaced the current Console view");
    });
  }
  for (const outcome of ["success", "detail", "history"]) {
    const label = outcome === "success" ? "detail success" : outcome + " error";
    const name = sameHash
      ? "memory navigation ignores old " + label + " after returning to same hash"
      : "memory navigation ignores stale " + label + " after selecting another memory";
    test(name, options, async (t) => {
      const h = fixture(t);
      await overview(h);
      const old = beginDetail(h, "mem_alpha");
      // Both reads have reached json(); the abandoned pair stays pending
      // while a newer pair completes. Each failure path names its own member.
      await h.headers(old[0], outcome === "detail" ? 503 : 200);
      await h.headers(old[1], outcome === "history" ? 503 : 200);
      if (sameHash) await overview(h);
      const id = sameHash ? "mem_alpha" : "mem_beta";
      const current = beginDetail(h, id);
      await finishDetail(h, current, id, "new detail visit");
      visible(h, "#/memories/" + id, "memories", "memories / " + id, "Memory " + id, "new detail visit");
      assert.ok(h.nodes.view.textContent.includes("new detail visit history"), "newer history completed before old response");
      const before = currentDestination(t, h, "#/memories/" + id, "memories", "memories / " + id);
      await finishDetail(h, old, "mem_alpha", "old detail response", outcome === "success" ? null : outcome);
      unchangedAfter(h, before, (sameHash ? "prior-visit" : "stale") + " " + label + " replaced the current Console view");
    });
  }
}

for (const back of ["button", "Escape", "history"]) {
  test(`memory focus open/back restores filter, selection and both scroll owners via ${back}`, options, async (t) => {
    const h = fixture(t);
    const list = beginList(h);
    await h.finish(list[0], { items: [memory("mem_alpha", "match alpha"), memory("mem_beta", "match beta"), memory("mem_hidden", "unrelated")] });
    const input = h.document.getElementById("filter-memories");
    input.value = "match";
    input.dispatch("input");
    const controls = h.nodes.view.querySelectorAll(".focus-open");
    controls[0].focus();
    assert.equal(controls[0].getAttribute("aria-expanded"), "false");
    for (const [key, index] of [["ArrowDown", 1], ["ArrowDown", 1], ["Home", 0], ["End", 1], ["ArrowUp", 0], ["End", 1]]) {
      assert.equal(h.document.activeElement.dispatch("keydown", { key }).defaultPrevented, true);
      assert.equal(h.document.activeElement, controls[index], key + " skips filtered rows");
    }
    h.nodes.view.scrollTop = 230;
    h.document.scrollingElement.scrollTop = 410;
    const key = back === "button" ? " " : "Enter";
    assert.equal(controls[1].dispatch("keydown", { key }).defaultPrevented, true);
    assert.equal(h.window.location.hash, "#/memories/mem_beta");
    const detail = beginDetail(h, "mem_beta");
    assert.equal(h.document.getElementById("focus-inventory").hidden, true, "collapse starts before requests settle");
    await finishDetail(h, detail, "mem_beta", "expanded memory");
    assert.equal(h.nodes.view.querySelector(".focus-identity").textContent, "mem_beta");
    assert.equal(h.document.activeElement, h.document.getElementById("focus-detail"));
    assert.equal(h.nodes.view.querySelector(".focus-back").getAttribute("aria-expanded"), "true");
    assert.match(h.nodes["focus-status"].textContent, /list collapsed/);
    h.nodes.view.scrollTop = 12;
    h.document.scrollingElement.scrollTop = 15;
    if (back === "button") h.nodes.view.querySelector(".focus-back").dispatch("click");
    else if (back === "Escape") h.nodes.view.querySelector(".focus-layout").dispatch("keydown", { key: "Escape" });
    else h.window.location.hash = "#/memories";
    assert.equal(h.window.location.hash, "#/memories");
    h.begin(h.window.location.hash, []);
    assert.equal(h.document.getElementById("focus-inventory").hidden, false);
    assert.equal(h.document.getElementById("focus-detail").hidden, true);
    assert.equal(h.document.getElementById("filter-memories").value, "match");
    assert.equal(h.document.activeElement.getAttribute("data-focus-id"), "mem_beta");
    assert.equal(h.document.activeElement.getAttribute("aria-current"), "true");
    assert.equal(h.nodes.view.scrollTop, 230);
    assert.equal(h.document.scrollingElement.scrollTop, 410);
    assert.match(h.nodes["focus-status"].textContent, /list expanded/);
    assert.equal(h.requests.length, 3, "only initial inventory plus existing detail/history metadata requests");
    // Browser Forward is another detail route visit, never a cached reveal.
    const forward = beginDetail(h, "mem_beta");
    await finishDetail(h, forward, "mem_beta", "forward metadata");
    assert.equal(h.document.activeElement, h.document.getElementById("focus-detail"));
  });
}

for (const flags of [{ sensitive: true }, { redacted: true }]) {
  test(`memory deep link stays private with ${Object.keys(flags)[0]} and Back loads inventory`, options, async (t) => {
    const h = fixture(t);
    const detail = beginDetail(h, "mem_alpha");
    await h.finish(detail[0], { memory: { ...memory("mem_alpha", "must never be shown"), ...flags } });
    await h.finish(detail[1], { versions: [] });
    // The exact read is the deliberate reveal: only a server-redacted memory hides
    // its content; a sensitive flag alone shows what the read returned.
    assert.equal(h.nodes.view.textContent.includes("must never be shown"), !flags.redacted);
    assert.equal(/sensitive value redacted/.test(h.nodes.view.textContent), !!flags.redacted);
    assert.equal(h.document.activeElement, h.document.getElementById("focus-detail"));
    h.nodes.view.querySelector(".focus-back").dispatch("click");
    const list = beginList(h);
    await h.finish(list[0], { items: [{ ...memory("mem_alpha", "must never be shown"), ...flags }] });
    assert.equal(h.nodes.view.textContent.includes("must never be shown"), false);
    assert.equal(h.document.activeElement.getAttribute("data-focus-id"), "mem_alpha");
    assert.equal(h.requests.length, 3, "deep-link Back only fetches missing inventory");
  });
}

test("memory Back during detail loading fences late completion and keeps browse position", options, async (t) => {
  const h = fixture(t);
  await finishList(h, beginList(h), "browse metadata");
  h.nodes.view.scrollTop = 190;
  const detail = beginDetail(h, "mem_alpha");
  h.nodes.view.querySelector(".focus-back").dispatch("click");
  h.begin(h.window.location.hash, []);
  const before = currentDestination(t, h, "#/memories", "memories", "memories");
  await finishDetail(h, detail, "mem_alpha", "late detail");
  unchangedAfter(h, before, "late detail must not reopen expanded content");
  assert.equal(h.nodes.view.scrollTop, 190);
});
