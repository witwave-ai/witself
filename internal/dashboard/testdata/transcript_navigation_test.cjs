"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// The app's route() does not return its transcript request chains. Once all
// controlled fetch/JSON promises have settled, a full event-loop turn lets
// their real success/catch continuations finish, without elapsed-time sleeps.
const nextTurn = () => new Promise((resolve) => setImmediate(resolve));

// This adapter supplies DOM operations only. The real app owns navigation,
// rendering, filtering and request decisions. Browser layout is not simulated.
function dom(check) {
  const elements = [], writes = [], insertions = [], scrolls = [];
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
      this.scrollPosition = 0;
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
    insertAdjacentHTML(position, value) {
      check(position === "beforeend", "unexpected insertion position");
      check(this.id === "entries" && this.closest("#view"), "insertion outside current entries");
      insertions.push({ node: this, value: String(value) });
      this.appendHTML(String(value));
    }
    get scrollTop() { return this.scrollPosition; }
    set scrollTop(value) {
      check(this.id === "entries" && Number.isFinite(value), "unexpected scroll position");
      this.scrollPosition = value;
      scrolls.push({ kind: "tail", node: this, value });
    }
    // A deterministic DOM metric records the app's scroll assignment. This
    // does not model browser layout, visibility or pixel positioning.
    get scrollHeight() { return this.children.length * 10; }
    scrollIntoView(options) {
      check(this.matches(".entry.anchored") && this.closest("#view"), "unexpected anchor target");
      check(options && options.block === "center" && Object.keys(options).length === 1, "unexpected anchor options");
      scrolls.push({ kind: "anchor", node: this });
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
    nodes, rail, writes, insertions, scrolls,
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
    throw new Error("transcript navigation fixture: " + message);
  };
  const d = dom(check);
  const window = { location: { hash: "#/overview", host: "localhost" }, matchMedia: null };
  const drained = (request) => request.headers.settled && request.body.settled &&
    request.jsonCalls === (request.networkFailure ? 0 : 1);
  // Register cleanup before loading production code. A failed semantic
  // assertion must still settle every controlled promise and retire streams.
  t.after(async () => {
    const errors = [];
    try {
      for (const request of requests) if (!request.headers.settled) request.releaseHeaders();
      await nextTurn();
      for (const request of requests) {
        if (!request.body.settled) request.body.resolve({ transcripts: [], entries: [] });
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
    assert.equal(errors.length, 0, "transcript navigation fixture cleanup failed: " + errors.join(", "));
    assert.ok(sources.every((source) => source.closed && source.listeners.size === 0), "transcript navigation fixture cleanup failed: streams remain");
    assert.equal(d.listenerCount(), 0, "transcript navigation fixture cleanup failed: DOM listeners remain");
    t.diagnostic("transcript navigation fixture drained all owned responses and streams");
  });
  const sandbox = {
    module: { exports: {} }, window, document: d.document, URLSearchParams,
    fetch(url, options) {
      check(typeof url === "string" && /^(?:\/api\/self|\/api\/transcripts|\/api\/transcripts\/tx_(?:alpha|beta)\?(?:tail=true&limit=200|after_sequence=3&limit=500))$/.test(url), "unexpected fetch URL");
      check(options && options.credentials === "same-origin" && Object.keys(options).length === 1, "unexpected fetch options");
      const request = {
        url, headers: deferred(check), body: deferred(check), jsonCalls: 0, status: null, networkFailure: false,
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
        check(/^\/api\/events(?:\?transcript=tx_(?:alpha|beta)(?:&after_sequence=[1-9][0-9]*)?)?$/.test(url), "unexpected stream URL");
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
        check(!this.closed, "event delivery through a closed source");
        check(name === "transcript" && this.listeners.has(name), "unexpected stream delivery");
        this.listeners.get(name)({ data: JSON.stringify(body) });
      }
    },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8"), sandbox, { filename: "app.js" });
  const app = sandbox.module.exports;
  const h = {
    ...d, window, requests, sources, app,
    healthy() { assert.deepEqual(faults, [], "transcript navigation fixture: recorded operation failure"); },
    begin(hash, url) {
      const start = requests.length;
      window.location.hash = hash;
      app.route();
      h.healthy();
      const added = requests.slice(start);
      assert.deepEqual(added.map((request) => request.url), [url], "transcript navigation fixture: route request inventory");
      return added[0];
    },
    async headers(request, status = 200) {
      request.releaseHeaders(status);
      await nextTurn();
      h.healthy();
      assert.equal(request.jsonCalls, 1, "transcript navigation fixture: JSON consumption must begin");
      assert.equal(request.body.settled, false, "transcript navigation fixture: JSON must remain controlled");
    },
    async hold(request, phase, status = 200) {
      if (phase === "JSON") await h.headers(request, status);
      assert.equal(request.headers.settled, phase === "JSON", "transcript navigation fixture: pending headers boundary");
      assert.equal(request.body.settled, false, "transcript navigation fixture: pending JSON boundary");
      assert.equal(request.jsonCalls, phase === "JSON" ? 1 : 0, "transcript navigation fixture: pending JSON consumption");
    },
    async finish(request, body, status = 200) {
      if (!request.headers.settled) await h.headers(request, status);
      assert.equal(request.status, status, "transcript navigation fixture: response status mismatch");
      assert.equal(request.jsonCalls, 1, "transcript navigation fixture: JSON response was not consumed");
      request.body.resolve(body);
      await nextTurn();
      h.healthy();
      h.settled(request);
    },
    async jsonFailure(request, label) {
      if (!request.headers.settled) await h.headers(request);
      assert.equal(request.status, 200, "transcript navigation fixture: JSON failure follows successful headers");
      request.body.reject(new Error(label));
      await nextTurn();
      h.healthy();
      h.settled(request);
    },
    async networkFailure(request, label) {
      assert.equal(request.headers.settled, false, "transcript navigation fixture: network failure precedes headers");
      request.networkFailure = true;
      request.headers.reject(new Error(label));
      request.body.resolve({}); // No Response exists, so its unused body is never read.
      await nextTurn();
      h.healthy();
      h.settled(request);
    },
    settled(request) {
      h.healthy();
      assert.ok(drained(request), "transcript navigation fixture: incomplete controlled response");
    },
  };
  return h;
}

function navigation(h, hash, crumb) {
  const section = hash === "#/overview" ? "overview" : "transcripts";
  assert.equal(h.window.location.hash, hash, "selected route hash");
  const links = h.rail.querySelectorAll("a");
  assert.equal(links.length, 7, "navigation assertions have real links");
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

function entry(sequence, body = "entry " + sequence) { return { sequence, role: "assistant", body }; }
function page(id, title, entries) { return { transcript: { id, title }, entries }; }
function beginList(h) {
  const request = h.begin("#/transcripts", "/api/transcripts");
  navigation(h, "#/transcripts", "transcripts");
  return request;
}
function beginDetail(h, id = "tx_alpha", anchored = false) {
  const hash = "#/transcripts/" + id + (anchored ? "?from=4&to=5" : "");
  const request = h.begin(hash, "/api/transcripts/" + id + (anchored ? "?after_sequence=3&limit=500" : "?tail=true&limit=200"));
  navigation(h, hash, "transcripts / " + id);
  return request;
}
async function overview(h) {
  const request = h.begin("#/overview", "/api/self");
  await h.finish(request, { identity: { agent_name: "Synthetic navigator" }, salient_memories: [{ id: "mem_overview", snippet: "current overview response" }] });
  visible(h, "#/overview", "overview", "inventory", "current overview response");
}
async function currentDetail(h, id = "tx_beta") {
  const request = beginDetail(h, id);
  await h.finish(request, page(id, "Current " + id, [entry(20, "current transcript response")]));
  visible(h, "#/transcripts/" + id, "transcripts / " + id, "Current " + id + " live tail", "current transcript response");
  const source = h.app.state.eventSource;
  assert.equal(source.url, "/api/events?transcript=" + id + "&after_sequence=20", "new destination seeds its own live cursor");
  assert.equal(h.app.state.seenSequences[id], 20, "new destination records its positive rendered cursor");
  assert.equal(source.closed, false, "new destination stream is open");
  return source;
}
function currentDestination(t, h) {
  h.healthy();
  const hash = h.window.location.hash;
  const crumb = hash === "#/overview" ? "overview" : "transcripts / " + hash.split("/")[2];
  navigation(h, hash, crumb);
  assert.equal(h.nodes.view.querySelector(".error"), null, "new destination completed successfully");
  const before = {
    hash, crumb, content: h.nodes.view.textContent, writes: h.writes.length,
    source: h.app.state.eventSource, sources: h.sources.length,
    closes: h.app.state.eventSource.closeCalls, scrolls: h.scrolls.length,
    sequences: JSON.stringify(h.app.state.seenSequences),
  };
  assert.ok(before.content && h.nodes.view.querySelector("h2") && before.writes, "new destination is nonempty before old response");
  assert.equal(before.source.closed, false, "new destination has an open source before old response");
  t.diagnostic("transcript navigation current destination established");
  return before;
}
function unchangedAfter(h, before, oracle) {
  h.healthy();
  assert.equal(h.nodes.view.textContent, before.content, oracle);
  navigation(h, before.hash, before.crumb);
  assert.equal(h.writes.length, before.writes, "old response does not repaint the current view");
  assert.equal(h.sources.length, before.sources, "old response does not create a source");
  assert.equal(h.app.state.eventSource, before.source, "old response preserves source identity");
  assert.equal(before.source.closeCalls, before.closes, "old response does not close the current source");
  assert.equal(before.source.closed, false, "current source remains open");
  assert.equal(JSON.stringify(h.app.state.seenSequences), before.sequences, "old response preserves rendered sequence cursors");
  assert.equal(h.scrolls.length, before.scrolls, "old response does not scroll an obsolete anchor");
}
function baseline(t) { t.diagnostic("transcript navigation current baseline established"); }
const options = { timeout: 5000 };

test("transcript navigation renders and filters current inventory", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const request = beginList(h);
  await h.headers(request);
  await h.finish(request, { transcripts: [
    { id: "tx_alpha", title: "First <em>literal</em>" }, { id: "tx_beta", title: "Second inventory" },
  ] });
  visible(h, "#/transcripts", "transcripts", "transcripts", "First <em>literal</em>");
  assert.equal(h.nodes.view.querySelector("em"), null, "inventory titles stay escaped");
  const input = h.document.getElementById("filter-transcripts");
  assert.ok(input, "real renderer created the transcript filter");
  input.value = "SECOND";
  input.dispatch("input");
  assert.deepEqual(h.nodes.view.querySelectorAll(".row").map((row) => row.style.display), ["none", ""], "inventory filtering is case insensitive");
  input.value = "";
  input.dispatch("input");
  assert.deepEqual(h.nodes.view.querySelectorAll(".row").map((row) => row.style.display), ["", ""], "clearing the filter restores both rows");
  assert.equal(h.requests.length, 2, "filtering makes no additional request");
  baseline(t);
});

test("transcript navigation renders current tail and appends through its open stream", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const request = beginDetail(h);
  const pendingContent = h.nodes.view.textContent;
  await h.headers(request);
  assert.equal(h.nodes.view.textContent, pendingContent, "pending detail keeps the existing view until JSON settles");
  await h.finish(request, page("tx_alpha", "Current tail", [entry(7, "<em>literal</em>\nsecond line"), entry(9, '{"tool_name":"<tool>","ok":true}')]));
  visible(h, "#/transcripts/tx_alpha", "transcripts / tx_alpha", "Current tail live tail", "<em>literal</em>\nsecond line");
  assert.equal(h.nodes.view.querySelector("em"), null, "entry text stays escaped");
  assert.equal(h.nodes.view.querySelector("tool"), null, "structured tool label stays escaped");
  assert.equal(h.nodes.view.querySelector(".tool-badge")?.textContent, "<tool>", "structured entry retains its real disclosure markup");
  const source = h.app.state.eventSource;
  assert.equal(source.url, "/api/events?transcript=tx_alpha&after_sequence=9", "tail seeds the highest rendered sequence");
  assert.equal(h.app.state.seenSequences.tx_alpha, 9, "tail cursor is positive before append");
  const container = h.document.getElementById("entries");
  source.emit("transcript", { entries: [entry(9, "duplicate must not append"), entry(10, "fresh stream entry")] });
  h.healthy();
  assert.deepEqual(container.querySelectorAll(".entry").map((node) => node.getAttribute("data-seq")), ["7", "9", "10"], "open stream appends only the new sequence");
  assert.equal(h.app.state.seenSequences.tx_alpha, 10, "append advances the cursor");
  assert.equal(h.insertions.length, 1, "one actual DOM insertion occurred");
  assert.equal(container.scrollTop, container.scrollHeight, "new live entry requests the current tail position");
  assert.equal(h.scrolls.length, 1, "one live tail scroll occurred");
  source.emit("transcript", { entries: [entry(10, "duplicate again")] });
  assert.equal(h.insertions.length, 1, "duplicate live entry causes no insertion");
  assert.equal(h.scrolls.length, 1, "duplicate live entry causes no scroll");
  baseline(t);
});

test("transcript navigation renders current anchored range and seeds its maximum", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const request = beginDetail(h, "tx_alpha", true);
  await h.finish(request, page("tx_alpha", "Current anchored", [entry(4), entry(6), entry(5)]));
  visible(h, "#/transcripts/tx_alpha?from=4&to=5", "transcripts / tx_alpha", "Current anchored live tail", "entry 4");
  assert.deepEqual(h.nodes.view.querySelectorAll(".entry.anchored").map((node) => node.getAttribute("data-seq")), ["4", "5"], "only the requested inclusive range is anchored");
  assert.equal(h.scrolls.length, 1, "current anchor requests one scroll");
  assert.equal(h.scrolls[0].kind, "anchor", "current range uses anchor scrolling");
  assert.equal(h.scrolls[0].node.getAttribute("data-seq"), "4", "first current anchor is the scroll target");
  assert.equal(h.app.state.seenSequences.tx_alpha, 6, "initial cursor is maximum rather than last entry");
  assert.equal(h.app.state.eventSource.url, "/api/events?transcript=tx_alpha&after_sequence=6", "anchored query seeds maximum sequence");
  baseline(t);
});

test("transcript navigation renders an empty current tail without a positive cursor", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  await h.finish(beginDetail(h), page("tx_alpha", "Empty current tail", []));
  visible(h, "#/transcripts/tx_alpha", "transcripts / tx_alpha", "Empty current tail live tail", "no entries yet");
  assert.equal(h.app.state.seenSequences.tx_alpha, 0, "empty initial response seeds zero");
  assert.equal(h.app.state.eventSource.url, "/api/events?transcript=tx_alpha", "empty tail omits after_sequence");
  assert.equal(h.scrolls.length, 0, "empty tail has no anchor or appended scroll");
  baseline(t);
});

for (const failure of ["inventory HTTP", "detail JSON", "detail network"]) {
  test("transcript navigation surfaces current " + failure + " error", options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const request = failure === "inventory HTTP" ? beginList(h) : beginDetail(h);
    const label = "current " + failure + " failure <literal>";
    if (failure === "inventory HTTP") await h.finish(request, { error: label }, 503);
    else if (failure === "detail JSON") await h.jsonFailure(request, label);
    else await h.networkFailure(request, label);
    navigation(h, failure === "inventory HTTP" ? "#/transcripts" : "#/transcripts/tx_alpha", failure === "inventory HTTP" ? "transcripts" : "transcripts / tx_alpha");
    assert.equal(h.nodes.view.querySelector(".error")?.textContent, label, "current transcript failure remains visible as text");
    assert.equal(h.nodes.view.querySelector("literal"), null, "current transcript error stays escaped");
    baseline(t);
  });
}

const staleCases = [
  { name: "inventory success after leaving", kind: "list", destination: "overview", phase: "headers", outcome: "success" },
  { name: "inventory success after opening detail", kind: "list", destination: "beta", phase: "JSON", outcome: "success" },
  { name: "inventory HTTP error after opening detail", kind: "list", destination: "beta", phase: "headers", outcome: "HTTP" },
  { name: "inventory JSON error after leaving", kind: "list", destination: "overview", phase: "JSON", outcome: "JSON" },
  { name: "detail success after leaving", kind: "detail", destination: "overview", phase: "headers", outcome: "success" },
  { name: "detail HTTP error after leaving", kind: "detail", destination: "overview", phase: "JSON", outcome: "HTTP" },
  { name: "detail JSON error after opening another detail", kind: "detail", destination: "beta", phase: "JSON", outcome: "JSON" },
  { name: "detail network error after opening another detail", kind: "detail", destination: "beta", phase: "headers", outcome: "network" },
  { name: "detail success cannot replace another detail stream", kind: "detail", destination: "beta", phase: "headers", outcome: "success", oracle: "stream" },
  { name: "same URL detail success preserves current content", kind: "detail", destination: "alpha", phase: "JSON", outcome: "success" },
  { name: "same URL detail success preserves current cursor", kind: "detail", destination: "alpha", phase: "JSON", outcome: "success", oracle: "cursor" },
  { name: "same URL detail HTTP error preserves current content", kind: "detail", destination: "alpha", phase: "headers", outcome: "HTTP" },
  { name: "abandoned anchored detail success cannot scroll", kind: "anchor", destination: "overview", phase: "headers", outcome: "success", oracle: "anchor" },
];

for (const scenario of staleCases) {
  test("transcript navigation ignores " + scenario.name, options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const old = scenario.kind === "list" ? beginList(h) : beginDetail(h, "tx_alpha", scenario.kind === "anchor");
    await h.hold(old, scenario.phase, scenario.outcome === "HTTP" ? 503 : 200);
    if (scenario.destination === "overview") await overview(h);
    else {
      if (scenario.destination === "alpha") await overview(h); // A distinct visit with the exact same final URL.
      await currentDetail(h, "tx_" + scenario.destination);
    }
    const before = currentDestination(t, h);
    const label = "abandoned response <literal>";
    if (scenario.outcome === "HTTP") await h.finish(old, { error: label }, 503);
    else if (scenario.outcome === "JSON") await h.jsonFailure(old, label);
    else if (scenario.outcome === "network") await h.networkFailure(old, label);
    else if (scenario.kind === "list") await h.finish(old, { transcripts: [{ id: "tx_alpha", title: label }] });
    else await h.finish(old, page("tx_alpha", label, [entry(scenario.kind === "anchor" ? 4 : 1, "abandoned entry")]));

    // Separate top-level cases put each side effect first: a visible repaint
    // must not hide cursor rollback, stream takeover or obsolete scrolling.
    const oracle = "stale transcript " + scenario.name + " must not change the current view";
    if (scenario.oracle === "stream") {
      assert.equal(h.sources.length, before.sources, "stale transcript success must not create or replace the current stream");
    } else if (scenario.oracle === "cursor") {
      assert.equal(h.app.state.seenSequences.tx_alpha, 20, "stale same URL transcript success must not lower the current cursor");
    } else if (scenario.oracle === "anchor") {
      assert.equal(h.scrolls.length, before.scrolls, "stale anchored transcript success must not scroll an obsolete entry");
    }
    unchangedAfter(h, before, oracle);
    if (scenario.destination !== "overview") {
      before.source.emit("transcript", { entries: [entry(21, "current stream remains healthy")] });
      h.healthy();
      assert.equal(h.app.state.seenSequences["tx_" + scenario.destination], 21, "surviving current stream advances its own cursor");
      assert.ok(h.nodes.view.textContent.includes("current stream remains healthy"), "surviving current stream appends to the current DOM");
      navigation(h, before.hash, before.crumb);
    }
  });
}
