"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// The app's route() does not return its fact request chains. Once all
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
    throw new Error("fact navigation fixture: " + message);
  };
  const d = dom(check);
  const window = { location: { hash: "#/overview", host: "localhost" }, matchMedia: null };
  // Register the escape before loading the app. Even a failed semantic
  // assertion drains every held response and clears all inert stream handlers.
  t.after(async () => {
    const errors = [];
    try {
      // Old cold-detail code may launch history only after cleanup releases
      // inventory. Discover those real requests and drain them in later turns.
      // Nothing pre-creates a follow-on that the corrected app must not start.
      for (let turn = 0; turn < 8; turn++) {
        for (const request of requests) {
          if (!request.headers.settled) request.releaseHeaders();
        }
        await nextTurn();
        for (const request of requests) {
          if (!request.body.settled && request.jsonCalls === 1) {
            request.body.resolve(cleanupBody(request));
          }
        }
        await nextTurn();
        const count = requests.length;
        if (requests.every((request) => request.headers.settled && request.body.settled && request.jsonCalls === 1)) {
          await nextTurn();
          if (requests.length === count) break;
        }
      }
      if (requests.some((request) => !request.headers.settled || !request.body.settled || request.jsonCalls !== 1)) {
        errors.push("responses did not drain");
      }
      if (faults.length) errors.push("fixture operation failed");
    } catch (_) {
      errors.push("response drain operation failed");
    } finally {
      for (const source of sources) {
        source.close();
        source.listeners.clear();
        source.onopen = null;
        source.onerror = null;
      }
      d.clearListeners();
    }
    assert.equal(errors.length, 0, "fact navigation fixture cleanup failed: " + errors.join(", "));
    assert.ok(sources.every((source) => source.closed && source.listeners.size === 0), "fact navigation fixture cleanup failed: streams remain");
    assert.equal(d.listenerCount(), 0, "fact navigation fixture cleanup failed: DOM handlers remain");
    t.diagnostic("fact navigation fixture drained all owned responses and streams");
  });
  const sandbox = {
    module: { exports: {} }, window, document: d.document, URLSearchParams,
    fetch(url, options) {
      check(typeof url === "string" && /^(?:\/api\/self|\/api\/facts\?limit=100|\/api\/facts\/fact_(?:alpha|beta|locked)\/history\?subject=synthetic%20(?:alpha|beta|locked)&predicate=sample%2Fvalue)$/.test(url), "unexpected fetch URL");
      check(requests.length < 32, "request count exceeded fixture bound");
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
        check(url === "/api/events" || url === "/api/events?facts=true", "unexpected stream URL");
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
    cached() { return JSON.parse(JSON.stringify(app.state.facts)); },
    healthy() { assert.deepEqual(faults, [], "fact navigation fixture: recorded operation failure"); },
    begin(hash, urls) {
      const start = requests.length;
      window.location.hash = hash;
      app.route();
      h.healthy();
      const added = requests.slice(start);
      assert.deepEqual(added.map((request) => request.url), urls, "fact navigation fixture: route request inventory");
      return added;
    },
    async headers(request, status = 200) {
      request.releaseHeaders(status);
      await nextTurn();
      h.healthy();
      assert.equal(request.jsonCalls, 1, "fact navigation fixture: JSON consumption must begin");
      assert.equal(request.body.settled, false, "fact navigation fixture: JSON must remain controlled");
    },
    async finish(request, body, status = 200) {
      if (!request.headers.settled) await h.headers(request, status);
      assert.equal(request.status, status, "fact navigation fixture: response status mismatch");
      assert.equal(request.jsonCalls, 1, "fact navigation fixture: JSON response was not consumed");
      request.body.resolve(body);
      await nextTurn();
      h.healthy();
    },
    settled(owned) {
      h.healthy();
      assert.ok(owned.every((request) => request.headers.settled && request.body.settled && request.jsonCalls === 1), "fact navigation fixture: incomplete controlled response");
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

function fact(id, value, sensitive = false) {
  const row = {
    id, subject: "synthetic " + id.slice("fact_".length), predicate: "sample/value",
    value_type: "string", cardinality: "single", source_kind: "user", sensitive,
    updated_at: "2026-01-01T00:00:00Z", confidence: 0.75,
  };
  if (!sensitive) row.value = value;
  return row;
}

function historyURL(id) {
  return "/api/facts/" + id + "/history?subject=" + encodeURIComponent("synthetic " + id.slice("fact_".length)) +
    "&predicate=" + encodeURIComponent("sample/value");
}

function history(label) {
  return { assertions: [{ value: label, source_kind: "user", confidence: 0.75, observed_at: "2026-01-01T00:00:00Z" }] };
}

function cleanupBody(request) {
  if (request.status >= 400) return { error: "harmless cleanup error" };
  if (request.url === "/api/facts?limit=100") {
    return { facts: [fact("fact_alpha", "cleanup alpha"), fact("fact_beta", "cleanup beta"), fact("fact_locked", null, true)] };
  }
  if (request.url === "/api/self") return { identity: { agent_name: "Synthetic navigator" } };
  return history("harmless cleanup history");
}

function beginList(h) {
  const [request] = h.begin("#/facts", ["/api/facts?limit=100"]);
  navigation(h, "#/facts", "facts", "facts");
  return request;
}

function beginCold(h, id) {
  assert.equal(h.cached()[id], undefined, "cold detail starts without its selected fact cached");
  const [request] = h.begin("#/facts/" + id, ["/api/facts?limit=100"]);
  navigation(h, "#/facts/" + id, "facts", "facts / " + id);
  return request;
}

async function beginWarm(h, id) {
  assert.ok(h.cached()[id], "warm detail was primed by a completed real inventory route");
  const start = h.requests.length;
  h.begin("#/facts/" + id, []);
  await nextTurn();
  h.healthy();
  const added = h.requests.slice(start);
  assert.deepEqual(added.map((request) => request.url), [historyURL(id)], "fact navigation fixture: warm history request inventory");
  navigation(h, "#/facts/" + id, "facts", "facts / " + id);
  return added[0];
}

async function launchColdHistory(h, inventory, id, rows) {
  const start = h.requests.length;
  await h.finish(inventory, { facts: rows });
  h.settled([inventory]);
  const added = h.requests.slice(start);
  assert.deepEqual(added.map((request) => request.url), [historyURL(id)], "fact navigation fixture: current cold history request inventory");
  return added[0];
}

async function drainDiscoveredHistory(h, start) {
  const added = h.requests.slice(start);
  assert.ok(added.length <= 1 && added.every((request) => request.url === historyURL("fact_alpha")), "fact navigation fixture: unexpected cold follow-on inventory");
  // Old-source follow-ons are allowed to be observed, then actually completed;
  // the dedicated assertion below judges whether starting them was legitimate.
  for (const request of added) await h.finish(request, history("abandoned synthetic history"));
  h.settled(added);
  await nextTurn();
  return added;
}

async function overview(h) {
  const [request] = h.begin("#/overview", ["/api/self"]);
  navigation(h, "#/overview", "overview", "overview");
  await h.finish(request, { identity: { agent_name: "Synthetic navigator" }, salient_memories: [{ id: "mem_overview", snippet: "current overview response" }] });
  h.settled([request]);
  visible(h, "#/overview", "overview", "overview", "inventory", "current overview response");
}

async function inventory(h, rows) {
  const request = beginList(h);
  await h.finish(request, { facts: rows });
  h.settled([request]);
  visible(h, "#/facts", "facts", "facts", "facts", rows[0].value);
  assert.deepEqual(h.cached(), Object.fromEntries(rows.map((row) => [row.id, row])), "real inventory keeps exact redacted rows");
}

async function prime(h) {
  await inventory(h, [fact("fact_alpha", "current alpha value"), fact("fact_beta", "current beta value"), fact("fact_locked", null, true)]);
}

function detailVisible(h, id, value, assertion) {
  visible(h, "#/facts/" + id, "facts", "facts / " + id, "synthetic " + id.slice("fact_".length) + " · sample/value", value);
  assert.deepEqual(h.nodes.view.querySelectorAll("h2").map((node) => node.textContent), ["synthetic " + id.slice("fact_".length) + " · sample/value", "details", "assertion history"], "selected fact and history panels are present");
  assert.ok(h.nodes.view.textContent.includes(assertion), "current addressed assertion history is visible");
}

function currentDestination(t, h, hash, section, crumb) {
  navigation(h, hash, section, crumb);
  assert.equal(h.nodes.view.querySelector(".error"), null, "newer destination completed successfully");
  const before = {
    hash, section, crumb, content: h.nodes.view.textContent, cache: h.cached(),
    headings: h.nodes.view.querySelectorAll("h2").map((node) => node.textContent), writes: h.writes.length,
  };
  assert.ok(before.content && before.headings.length && before.writes, "newer destination is nonempty before old response");
  t.diagnostic("fact navigation current destination established");
  return before;
}

function unchangedAfter(h, before, oracle) {
  h.healthy();
  assert.equal(h.nodes.view.textContent, before.content, oracle);
  navigation(h, before.hash, before.section, before.crumb);
  assert.deepEqual(h.nodes.view.querySelectorAll("h2").map((node) => node.textContent), before.headings, "current headings survive old response");
  assert.equal(h.writes.length, before.writes, "old fact response does not repaint the current view");
  assert.deepEqual(h.cached(), before.cache, "old fact response does not replace the current redacted cache");
}

function currentError(h, hash, crumb, label) {
  h.healthy();
  navigation(h, hash, "facts", crumb);
  assert.equal(h.nodes.view.querySelector(".error")?.textContent, label, "current fact failure remains visible as text");
  assert.equal(h.nodes.view.querySelector("literal"), null, "current fact error stays escaped");
}

const options = { timeout: 5000 };

test("fact navigation renders current inventory and filter", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const rows = [fact("fact_alpha", "first inventory entry"), fact("fact_beta", "second inventory entry"), fact("fact_locked", null, true)];
  await inventory(h, rows);
  assert.ok(h.nodes.view.textContent.includes("second inventory entry"), "current inventory has distinct rows");
  assert.equal(h.nodes.view.querySelector(".lock-chip")?.textContent, "locked", "redacted sensitive row stays a placeholder");
  assert.equal(h.nodes.view.querySelectorAll("svg").length, 2, "locked row creates real inert reveal and copy SVG markup");
  const input = h.document.getElementById("filter-facts");
  assert.ok(input, "real renderer created the facts filter");
  input.value = "second";
  input.focus();
  input.setSelectionRange(1, 4);
  input.dispatch("input");
  assert.deepEqual(h.nodes.view.querySelectorAll(".row").map((row) => row.style.display), ["none", "", "none"], "current inventory keeps its client-side filter");
  assert.equal(h.document.activeElement, input, "filter retains focus");
  assert.deepEqual([input.selectionStart, input.selectionEnd], [1, 4], "filter retains selection");
  assert.equal(h.requests.length, 2, "filter and locked controls make no additional request");
  t.diagnostic("fact navigation current baseline completed");
});

test("fact navigation renders current warm detail and history", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  await prime(h);
  const request = await beginWarm(h, "fact_alpha");
  await h.headers(request);
  await h.finish(request, history("current warm assertion"));
  h.settled([request]);
  detailVisible(h, "fact_alpha", "current alpha value", "current warm assertion");
  assert.equal(h.requests.length, 3, "warm detail does not fetch another inventory");
  t.diagnostic("fact navigation current baseline completed");
});

test("fact navigation renders current cold detail and history", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const request = beginCold(h, "fact_alpha");
  await h.headers(request);
  const assertion = await launchColdHistory(h, request, "fact_alpha", [fact("fact_alpha", "current cold value")]);
  await h.headers(assertion);
  await h.finish(assertion, history("current cold assertion"));
  h.settled([request, assertion]);
  detailVisible(h, "fact_alpha", "current cold value", "current cold assertion");
  assert.equal(h.requests.length, 3, "cold detail fetches exactly inventory then addressed history");
  t.diagnostic("fact navigation current baseline completed");
});

for (const failure of ["inventory", "cold inventory", "history"]) {
  test("fact navigation surfaces current " + failure + " error", options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const label = "current " + failure + " failure <literal>";
    let request;
    if (failure === "inventory") request = beginList(h);
    else if (failure === "cold inventory") request = beginCold(h, "fact_alpha");
    else {
      await prime(h);
      request = await beginWarm(h, "fact_alpha");
    }
    const count = h.requests.length;
    await h.headers(request, 503);
    await h.finish(request, { error: label }, 503);
    h.settled([request]);
    currentError(h, failure === "inventory" ? "#/facts" : "#/facts/fact_alpha", failure === "inventory" ? "facts" : "facts / fact_alpha", label);
    assert.equal(h.requests.length, count, "current failure launches no follow-on request");
    t.diagnostic("fact navigation current baseline completed");
  });
}

test("fact navigation surfaces missing current fact", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const request = beginCold(h, "fact_alpha");
  await h.finish(request, { facts: [fact("fact_beta", "other bounded inventory entry")] });
  h.settled([request]);
  currentError(h, "#/facts/fact_alpha", "facts / fact_alpha", "fact fact_alpha is not in the redacted inventory");
  assert.equal(h.requests.length, 2, "missing selected fact does not broaden inventory or fetch history");
  t.diagnostic("fact navigation current baseline completed");
});

for (const sameHash of [false, true]) {
  for (const outcome of ["success", "error"]) {
    const name = sameHash
      ? "fact navigation ignores old inventory " + outcome + " after returning to same hash"
      : "fact navigation ignores stale inventory " + outcome + " after leaving";
    test(name, options, async (t) => {
      const h = fixture(t);
      await overview(h);
      const old = beginList(h);
      if (sameHash) await h.headers(old, outcome === "error" ? 503 : 200);
      await overview(h);
      if (sameHash) await inventory(h, [fact("fact_beta", "new inventory visit"), fact("fact_locked", null, true)]);
      const before = currentDestination(t, h, sameHash ? "#/facts" : "#/overview", sameHash ? "facts" : "overview", sameHash ? "facts" : "overview");
      await h.finish(old, outcome === "error" ? { error: "old inventory failure" } : { facts: [fact("fact_alpha", "old inventory response")] }, outcome === "error" ? 503 : 200);
      h.settled([old]);
      unchangedAfter(h, before, (sameHash ? "prior-visit" : "stale") + " fact inventory " + outcome + " replaced the current Console view");
    });
  }
  for (const outcome of ["success", "error"]) {
    const name = sameHash
      ? "fact navigation ignores old warm history " + outcome + " after returning to same hash"
      : "fact navigation ignores stale warm history " + outcome + " after selecting another fact";
    test(name, options, async (t) => {
      const h = fixture(t);
      await overview(h);
      await prime(h);
      const old = await beginWarm(h, "fact_alpha");
      await h.headers(old, outcome === "error" ? 503 : 200);
      if (sameHash) await overview(h);
      const id = sameHash ? "fact_alpha" : "fact_beta";
      const current = await beginWarm(h, id);
      await h.finish(current, history("new warm history visit"));
      h.settled([current]);
      detailVisible(h, id, sameHash ? "current alpha value" : "current beta value", "new warm history visit");
      const before = currentDestination(t, h, "#/facts/" + id, "facts", "facts / " + id);
      await h.finish(old, outcome === "error" ? { error: "old warm history failure" } : history("old warm history response"), outcome === "error" ? 503 : 200);
      h.settled([old]);
      unchangedAfter(h, before, (sameHash ? "prior-visit" : "stale") + " warm fact history " + outcome + " replaced the current Console view");
    });
  }
}

for (const effect of ["view", "cache", "follow-on"]) {
  const name = effect === "follow-on"
    ? "fact navigation does not launch history for abandoned cold inventory"
    : "fact navigation ignores abandoned cold inventory success in the " + effect;
  test(name, options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const old = beginCold(h, "fact_alpha");
    if (effect === "cache") await h.headers(old);
    await inventory(h, [fact("fact_beta", "new inventory after cold visit"), fact("fact_locked", null, true)]);
    const before = currentDestination(t, h, "#/facts", "facts", "facts");
    const followOnStart = h.requests.length;
    await h.finish(old, { facts: [fact("fact_alpha", "abandoned cold inventory value")] });
    h.settled([old]);
    const followOns = await drainDiscoveredHistory(h, followOnStart);
    // These are separate cases: a stale DOM assertion cannot mask either
    // an abandoned cache mutation or an unauthorized follow-on request.
    if (effect === "cache") assert.deepEqual(h.cached(), before.cache, "abandoned cold fact inventory replaced the current redacted cache");
    if (effect === "follow-on") assert.equal(followOns.length, 0, "abandoned cold fact inventory launched a history request");
    unchangedAfter(h, before, "abandoned cold fact inventory replaced the current Console view");
  });
}

test("fact navigation ignores abandoned cold inventory error", options, async (t) => {
  const h = fixture(t);
  await overview(h);
  const old = beginCold(h, "fact_alpha");
  await h.headers(old, 503);
  await inventory(h, [fact("fact_beta", "new inventory after cold error")]);
  const before = currentDestination(t, h, "#/facts", "facts", "facts");
  const count = h.requests.length;
  await h.finish(old, { error: "abandoned cold inventory failure" }, 503);
  h.settled([old]);
  unchangedAfter(h, before, "abandoned cold fact inventory error replaced the current Console view");
  assert.equal(h.requests.length, count, "abandoned cold error starts no history");
});

for (const outcome of ["success", "error"]) {
  test("fact navigation ignores stale cold history " + outcome + " after leaving", options, async (t) => {
    const h = fixture(t);
    await overview(h);
    const old = beginCold(h, "fact_alpha");
    const assertion = await launchColdHistory(h, old, "fact_alpha", [fact("fact_alpha", "earlier cold value")]);
    await h.headers(assertion, outcome === "error" ? 503 : 200);
    await overview(h);
    const before = currentDestination(t, h, "#/overview", "overview", "overview");
    await h.finish(assertion, outcome === "error" ? { error: "old cold history failure" } : history("old cold history response"), outcome === "error" ? 503 : 200);
    h.settled([old, assertion]);
    unchangedAfter(h, before, "stale cold fact history " + outcome + " replaced the current Console view");
  });
}
