"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// The app's route() does not return its Overview request chain. Once all
// controlled fetch/JSON promises have settled, a full event-loop turn lets
// their real success/catch continuations finish, without elapsed-time sleeps.
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
    dispatch(name) {
      for (const listener of this.listeners.get(name) || []) listener({ target: this });
    }
    focus() { activeElement = this; }
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
      this.appendHTML(value);
    }
    appendHTML(value) {
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
          if (!token.endsWith("/>") && !["input", "br", "hr", "img"].includes(element.tagName)) stack.push(element);
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
    "view", "breadcrumb", "agent-name", "realm-name", "agent-id", "version",
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
      get activeElement() { return activeElement; },
      getElementById(id) { return nodes[id] || nodes.view.querySelector("#" + id); },
      querySelector(selector) {
        check(selector === ".entry.anchored", "unexpected document selector");
        return nodes.view.querySelector(selector);
      },
      querySelectorAll(selector) {
        check(selector === ".rail a", "unexpected document selector");
        return rail.querySelectorAll("a");
      },
    },
    clearListeners() { for (const element of elements) element.listeners.clear(); },
    listenerCount() { return elements.reduce((sum, element) => sum + element.listeners.size, 0); },
  };
}

function deferred(check) {
  let resolve, reject;
  const pending = { settled: false, promise: new Promise((yes, no) => { resolve = yes; reject = no; }) };
  for (const [name, settle] of [["resolve", resolve], ["reject", reject]]) {
    pending[name] = (value) => {
      check(!pending.settled, "response settled twice");
      pending.settled = true;
      settle(value);
    };
  }
  return pending;
}

function fixture(t) {
  const faults = [], requests = [], sources = [];
  const check = (condition, message) => {
    if (condition) return;
    faults.push(message);
    throw new Error("overview navigation fixture: " + message);
  };
  const d = dom(check);
  const window = { location: { hash: "#/overview", host: "localhost" }, matchMedia: null };
  const drained = (request) => request.headers.settled && request.body.settled && request.jsonCalls === 1;
  // Register before loading the app. Even a semantic failure must drain every
  // owned response, close every source and clear attached/detached listeners.
  t.after(async () => {
    const errors = [];
    try {
      for (const request of requests) if (!request.headers.settled) request.releaseHeaders();
      await nextTurn();
      for (const request of requests) {
        if (!request.body.settled) request.body.resolve({ identity: {}, entries: [] });
      }
      await nextTurn();
      if (!requests.every(drained)) errors.push("responses did not drain");
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
    assert.equal(errors.length, 0, "overview navigation fixture cleanup failed: " + errors.join(", "));
    assert.ok(sources.every((source) => source.closed && source.listeners.size === 0), "overview navigation fixture cleanup failed: streams remain");
    assert.equal(d.listenerCount(), 0, "overview navigation fixture cleanup failed: DOM listeners remain");
    t.diagnostic("overview navigation fixture drained all owned responses and streams");
  });
  const sandbox = {
    module: { exports: {} }, window, document: d.document, URLSearchParams,
    fetch(url, options) {
      check(url === "/api/self" || url === "/api/transcripts/tx_current?tail=true&limit=200", "unexpected fetch URL");
      check(options && options.credentials === "same-origin" && Object.keys(options).length === 1, "unexpected fetch options");
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
      return request.headers.promise;
    },
    EventSource: class {
      constructor(url) {
        check(url === "/api/events" || url === "/api/events?transcript=tx_current&after_sequence=20", "unexpected stream URL");
        this.url = url;
        this.closed = false;
        this.closeCalls = 0;
        this.listeners = new Map();
        sources.push(this);
      }
      addEventListener(name, listener) {
        check(!this.listeners.has(name), "duplicate stream listener");
        this.listeners.set(name, listener);
      }
      close() { this.closed = true; this.closeCalls++; }
      emit(name, body) {
        check(!this.closed && sandbox.module.exports.state.eventSource === this, "event delivery requires the open current source");
        check(name === "self" && this.listeners.has(name), "unexpected stream delivery");
        this.listeners.get(name)({ data: JSON.stringify(body) });
      }
    },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8"), sandbox, { filename: "app.js" });
  const h = {
    ...d, window, requests, sources, app: sandbox.module.exports,
    healthy() { assert.deepEqual(faults, [], "overview navigation fixture: recorded operation failure"); },
    begin(hash, url) {
      const start = requests.length;
      window.location.hash = hash;
      h.app.route();
      h.healthy();
      assert.deepEqual(requests.slice(start).map((request) => request.url), [url], "overview navigation fixture: route request inventory");
      return requests[start];
    },
    async headers(request, status = 200) {
      request.releaseHeaders(status);
      await nextTurn();
      h.healthy();
      assert.equal(request.jsonCalls, 1, "overview navigation fixture: JSON consumption must begin");
      assert.equal(request.body.settled, false, "overview navigation fixture: JSON must remain controlled");
    },
    pending(request, phase) {
      assert.equal(request.headers.settled, phase === "JSON", "overview navigation fixture: pending headers boundary");
      assert.equal(request.body.settled, false, "overview navigation fixture: pending JSON boundary");
      assert.equal(request.jsonCalls, phase === "JSON" ? 1 : 0, "overview navigation fixture: pending JSON consumption");
    },
    async finish(request, body, status = 200) {
      if (!request.headers.settled) await h.headers(request, status);
      assert.equal(request.status, status, "overview navigation fixture: response status mismatch");
      request.body.resolve(body);
      await nextTurn();
      h.healthy();
      assert.ok(drained(request), "overview navigation fixture: incomplete controlled response");
    },
    async jsonFailure(request, label) {
      if (!request.headers.settled) await h.headers(request);
      assert.equal(request.status, 200, "overview navigation fixture: JSON rejection follows successful headers");
      request.body.reject(new Error(label));
      await nextTurn();
      h.healthy();
      assert.ok(drained(request), "overview navigation fixture: incomplete controlled rejection");
    },
  };
  return h;
}

// Convert state by value into this realm. A reference to state.self would
// conceal mutations, and comparing VM object prototypes is not a UI oracle.
const valueCopy = (value) => JSON.parse(JSON.stringify(value));
const headerIDs = ["agent-name", "realm-name", "agent-id", "version", "status-poll", "status-addr"];
const headerVector = (h) => headerIDs.map((id) => h.nodes[id].textContent);
function selfData(label, number) {
  return {
    identity: { agent_name: label + " agent <literal>", realm_name: label + " realm", agent_id: "ag_" + label },
    dashboard_version: "0.0." + number,
    poll_interval_ms: number * 1000,
    index: { counts: { transcripts: 4, memories: 7, secrets: 2, facts: 3 } },
    salient_memories: [{ id: "mem_" + label, snippet: label + " salient <literal>", kind: "note", salience: 0.75 }],
    memory_checkpoint: { pending: true }, message_checkpoint: { pending: true },
    email_checkpoint: { pending: true }, avatar_checkpoint: { pending: true },
    observational: true,
  };
}
function headerMatches(h, self) {
  assert.deepEqual(headerVector(h), [
    self.identity.agent_name, self.identity.realm_name, self.identity.agent_id,
    "v" + self.dashboard_version, "poll " + self.poll_interval_ms / 1000 + "s", "localhost",
  ], "current header reflects the complete self response");
  assert.deepEqual(valueCopy(h.app.state.self), self, "current self state reflects the complete response by value");
}
function navigation(h, hash, crumb) {
  const section = hash === "#/overview" ? "overview" : "transcripts";
  assert.equal(h.window.location.hash, hash, "selected route hash");
  const links = h.rail.querySelectorAll("a");
  assert.equal(links.length, 7, "navigation assertions have seven real links");
  assert.deepEqual(links.filter((link) => link.classList.contains("active")).map((link) => link.getAttribute("data-nav")), [section], "only the selected rail link is active");
  assert.equal(links.find((link) => link.getAttribute("data-nav") === section).getAttribute("href"), "#/" + section, "active link represents the selected section");
  assert.equal(h.nodes.breadcrumb.textContent, crumb, "breadcrumb identifies the selected view");
}
function visible(h, hash, crumb, heading, content) {
  h.healthy();
  navigation(h, hash, crumb);
  assert.ok(h.writes.length > 0, "a real view render occurred");
  assert.equal(h.nodes.view.querySelector("h2")?.textContent, heading, "current view heading");
  assert.ok(h.nodes.view.textContent.includes(content), "current view contains its distinct response");
  assert.equal(h.nodes.view.querySelector(".error"), null, "current success is not an error");
}
function beginOverview(h) {
  const request = h.begin("#/overview", "/api/self");
  navigation(h, "#/overview", "overview");
  return request;
}
async function currentOverview(h, self = selfData("initial", 3)) {
  const request = beginOverview(h);
  await h.finish(request, self);
  visible(h, "#/overview", "overview", "inventory", self.salient_memories[0].snippet);
  headerMatches(h, self);
  assert.equal(h.app.state.eventSource.url, "/api/events", "current Overview owns the general stream");
  assert.equal(h.app.state.eventSource.closed, false, "current Overview stream is open");
}
async function currentDetail(h) {
  const request = h.begin("#/transcripts/tx_current", "/api/transcripts/tx_current?tail=true&limit=200");
  navigation(h, "#/transcripts/tx_current", "transcripts / tx_current");
  await h.finish(request, {
    transcript: { id: "tx_current", title: "Current transcript" },
    entries: [{ sequence: 20, role: "assistant", body: "current transcript content <literal>" }],
  });
  visible(h, "#/transcripts/tx_current", "transcripts / tx_current", "Current transcript live tail", "current transcript content <literal>");
  assert.equal(h.nodes.view.querySelector("literal"), null, "current detail content stays escaped");
  assert.equal(h.app.state.seenSequences.tx_current, 20, "current detail has a positive rendered cursor");
  assert.equal(h.app.state.eventSource.url, "/api/events?transcript=tx_current&after_sequence=20", "current detail owns its seeded stream");
  assert.equal(h.app.state.eventSource.closed, false, "current detail source is open");
}
function emitSelf(h, self) {
  const source = h.app.state.eventSource;
  source.emit("self", self);
  h.healthy();
  headerMatches(h, self);
  assert.equal(h.app.state.eventSource, source, "current self frame retains source identity");
}
function destination(t, h) {
  h.healthy();
  const hash = h.window.location.hash;
  const crumb = hash === "#/overview" ? "overview" : "transcripts / tx_current";
  navigation(h, hash, crumb);
  assert.equal(h.nodes.view.querySelector(".error"), null, "new destination completed successfully");
  const before = {
    hash, crumb, content: h.nodes.view.textContent, writes: h.writes.length,
    header: headerVector(h), self: valueCopy(h.app.state.self),
    source: h.app.state.eventSource, sources: h.sources.length,
    closes: h.app.state.eventSource.closeCalls, requests: h.requests.length,
  };
  assert.ok(before.content && h.nodes.view.querySelector("h2") && before.writes, "new destination is nonempty before old response");
  assert.ok(before.self.identity.agent_name && before.header.every(Boolean), "new destination has a nonempty current header and state");
  assert.equal(before.source.closed, false, "new destination has an open current source");
  t.diagnostic("overview navigation current destination established");
  return before;
}
function unchangedAfter(h, before, oracle) {
  h.healthy();
  assert.equal(h.nodes.view.textContent, before.content, oracle);
  navigation(h, before.hash, before.crumb);
  assert.deepEqual(headerVector(h), before.header, "old Overview response preserves the current header");
  assert.deepEqual(valueCopy(h.app.state.self), before.self, "old Overview response preserves current self state");
  assert.equal(h.writes.length, before.writes, "old Overview response does not repaint the current view");
  assert.equal(h.requests.length, before.requests, "old Overview response makes no follow-on request");
  assert.equal(h.sources.length, before.sources, "old Overview response does not create a source");
  assert.equal(h.app.state.eventSource, before.source, "old Overview response preserves source identity");
  assert.equal(before.source.closeCalls, before.closes, "old Overview response does not close the current source");
  assert.equal(before.source.closed, false, "current source remains open");
}
function baseline(t) { t.diagnostic("overview navigation current baseline established"); }
const options = { timeout: 5000 };

test("overview navigation renders current inventory and checkpoints", options, async (t) => {
  const h = fixture(t);
  const self = selfData("current", 7);
  const request = beginOverview(h);
  await h.headers(request);
  assert.equal(h.writes.length, 0, "pending current JSON does not render prematurely");
  await h.finish(request, self);
  visible(h, "#/overview", "overview", "inventory", self.salient_memories[0].snippet);
  headerMatches(h, self);
  assert.deepEqual(h.nodes.view.querySelectorAll(".card").map((card) => [
    card.querySelector(".label").textContent, card.querySelector(".num").textContent,
  ]), [["facts", "3"], ["memories", "7"], ["secrets", "2"], ["transcripts", "4"]], "current inventory renders all sorted counts");
  assert.deepEqual(h.nodes.view.querySelectorAll("a.card-link").map((node) => node.getAttribute("href")), ["#/facts", "#/memories", "#/secrets"], "current inventory links retain their destination sections");
  const panels = h.nodes.view.querySelectorAll(".panel");
  const salient = panels.find((panel) => panel.querySelector("h2")?.textContent === "salient memories");
  assert.equal(salient.querySelector("a").getAttribute("href"), "#/memories/mem_current", "current salient memory links to its detail");
  assert.equal(salient.querySelector("a").textContent, "current salient <literal>", "salient snippet remains literal text");
  assert.equal(h.nodes.view.querySelector("literal"), null, "current Overview values stay escaped");
  const checkpoints = panels.find((panel) => panel.querySelector("h2")?.textContent === "checkpoints");
  assert.deepEqual(checkpoints.querySelectorAll(".row").map((node) => node.textContent), [
    "memory curation pending", "messaging work pending", "email pending", "avatar lifecycle pending",
  ], "current checkpoint labels retain every pending lane");
  assert.equal(checkpoints.querySelector("a").getAttribute("href"), "#/email", "email checkpoint keeps its navigation link");
  assert.ok(h.nodes.view.textContent.includes("observational reads only — viewing never records usage"), "Overview retains the observational read label");
  baseline(t);
});

for (const failure of ["HTTP", "JSON"]) {
  test("overview navigation surfaces current " + failure + " error", options, async (t) => {
    const h = fixture(t);
    await currentOverview(h);
    const beforeHeader = headerVector(h), beforeSelf = valueCopy(h.app.state.self);
    const request = beginOverview(h);
    const label = "current " + failure + " failure <literal>";
    if (failure === "HTTP") await h.finish(request, { error: label }, 503);
    else await h.jsonFailure(request, label);
    navigation(h, "#/overview", "overview");
    assert.equal(h.nodes.view.querySelector(".error")?.textContent, label, "current Overview failure remains visible as text");
    assert.equal(h.nodes.view.querySelector("literal"), null, "current Overview error stays escaped");
    assert.deepEqual(headerVector(h), beforeHeader, "current error preserves the established header");
    assert.deepEqual(valueCopy(h.app.state.self), beforeSelf, "current error preserves the established self state");
    baseline(t);
  });
}

test("overview navigation completes current transcript destination", options, async (t) => {
  const h = fixture(t);
  const initial = selfData("initial", 3);
  await currentOverview(h, initial);
  const oldSource = h.app.state.eventSource;
  await currentDetail(h);
  headerMatches(h, initial);
  assert.equal(oldSource.closed, true, "completed detail retires the general stream");
  assert.notEqual(h.app.state.eventSource, oldSource, "completed detail has its own source");
  assert.equal(h.requests.length, 2, "normal Overview-to-detail navigation uses two observational reads");
  baseline(t);
});

test("overview navigation applies current self frame without repainting detail", options, async (t) => {
  const h = fixture(t);
  await currentOverview(h);
  await currentDetail(h);
  const content = h.nodes.view.textContent, writes = h.writes.length;
  const oldHeader = headerVector(h), oldSelf = valueCopy(h.app.state.self);
  emitSelf(h, selfData("live", 11));
  assert.notDeepEqual(headerVector(h), oldHeader, "current source frame actually updates the header");
  assert.notDeepEqual(valueCopy(h.app.state.self), oldSelf, "current source frame actually updates self state");
  assert.equal(h.nodes.view.textContent, content, "current source self frame preserves the detail content");
  assert.equal(h.writes.length, writes, "current source self frame does not repaint a non-Overview pane");
  navigation(h, "#/transcripts/tx_current", "transcripts / tx_current");
  baseline(t);
});

const staleCases = [];
for (const visit of ["after leaving", "after same URL return"]) {
  for (const response of [
    { name: "headers success", phase: "headers", outcome: "success" },
    { name: "JSON success", phase: "JSON", outcome: "success" },
    { name: "headers HTTP error", phase: "headers", outcome: "HTTP" },
    { name: "JSON rejection", phase: "JSON", outcome: "JSON" },
  ]) staleCases.push({ ...response, name: response.name + " " + visit, sameURL: visit === "after same URL return" });
}
staleCases.push(
  { name: "abandoned success in current header", phase: "headers", outcome: "success", first: "header" },
  { name: "abandoned success in current self state", phase: "JSON", outcome: "success", first: "self" },
);
for (const scenario of staleCases) {
  test("overview navigation ignores " + scenario.name, options, async (t) => {
    const h = fixture(t);
    await currentOverview(h);
    const old = beginOverview(h);
    if (scenario.phase === "JSON") await h.headers(old);
    h.pending(old, scenario.phase);
    // Finish a distinct route even when the final destination will return to
    // the identical Overview URL. The abandoned response stays pending.
    await currentDetail(h);
    if (scenario.sameURL) await currentOverview(h, selfData("returned", 9));
    const current = selfData("live", 11);
    emitSelf(h, current);
    if (scenario.sameURL) visible(h, "#/overview", "overview", "inventory", current.salient_memories[0].snippet);
    else visible(h, "#/transcripts/tx_current", "transcripts / tx_current", "Current transcript live tail", "current transcript content <literal>");
    h.pending(old, scenario.phase);
    const before = destination(t, h);
    const abandoned = selfData("abandoned", 99);
    assert.notDeepEqual(before.header, [
      abandoned.identity.agent_name, abandoned.identity.realm_name, abandoned.identity.agent_id,
      "v" + abandoned.dashboard_version, "poll 99s", "localhost",
    ], "overview navigation fixture: old and current headers are distinct");
    assert.notDeepEqual(before.self, abandoned, "overview navigation fixture: old and current self values are distinct");
    if (scenario.outcome === "HTTP") await h.finish(old, { error: "abandoned Overview failure <literal>" }, 503);
    else if (scenario.outcome === "JSON") await h.jsonFailure(old, "abandoned Overview failure <literal>");
    else await h.finish(old, abandoned);
    t.diagnostic("overview navigation old response continuation settled");

    // Independent first oracles prevent a visible DOM failure from masking
    // a stale header rewrite or replacement of the shared self projection.
    if (scenario.first === "header") {
      assert.deepEqual(headerVector(h), before.header, "stale Overview success must not replace the current header");
    } else if (scenario.first === "self") {
      assert.deepEqual(valueCopy(h.app.state.self), before.self, "stale Overview success must not replace current self state");
    }
    unchangedAfter(h, before, "stale Overview " + scenario.name + " must not change the current view");
  });
}
