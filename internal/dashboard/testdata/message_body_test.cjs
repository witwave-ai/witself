"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

// A small DOM adapter lets the real app render its HTML, handle delegated
// clicks and move existing nodes. It does not implement preview state or
// networking decisions. The separate browser acceptance covers browser layout
// and execution semantics; these tests control delayed completion ordering.
function dom() {
  const htmlWrites = [];
  const decode = (value) => value.replace(/&(?:amp|lt|gt|quot|#39);/g, (part) => ({
    "&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": '"', "&#39;": "'",
  }[part]));
  const escape = (value) => value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
  class Element {
    constructor(tag = "div") {
      this.tagName = tag;
      this.attrs = {};
      this.children = [];
      this.parentNode = null;
      this.ownText = "";
      this.style = {};
      this.classList = {
        contains: (name) => (this.attrs.class || "").split(/\s+/).includes(name),
        toggle() {},
      };
    }
    get id() { return this.attrs.id; }
    get hidden() { return Object.hasOwn(this.attrs, "hidden"); }
    set hidden(value) { if (value) this.attrs.hidden = ""; else delete this.attrs.hidden; }
    get disabled() { return Object.hasOwn(this.attrs, "disabled"); }
    set disabled(value) { if (value) this.attrs.disabled = ""; else delete this.attrs.disabled; }
    getAttribute(name) { return Object.hasOwn(this.attrs, name) ? this.attrs[name] : null; }
    setAttribute(name, value) { this.attrs[name] = String(value); }
    removeAttribute(name) { delete this.attrs[name]; }
    addEventListener() {}
    get textContent() { return this.ownText + this.children.map((child) => child.textContent).join(""); }
    set textContent(value) {
      this.children.forEach((child) => { child.parentNode = null; });
      this.children = [];
      this.ownText = String(value);
    }
    get innerHTML() {
      return escape(this.ownText) + this.children.map((child) => child.tagName === "#text"
        ? escape(child.textContent) : `<${child.tagName}>${child.innerHTML}</${child.tagName}>`).join("");
    }
    set innerHTML(value) {
      value = String(value);
      htmlWrites.push(value);
      this.textContent = "";
      const stack = [this];
      for (const token of value.match(/<[^>]*>|[^<]+/g) || []) {
        if (token.startsWith("</")) { stack.pop(); continue; }
        if (token.startsWith("<")) {
          const match = /^<([a-z0-9-]+)([^>]*)>$/i.exec(token);
          assert.ok(match, "fixture DOM encountered unsupported markup");
          const element = new Element(match[1]);
          for (const attr of match[2].matchAll(/([a-z0-9_-]+)(?:="([^"]*)")?/gi)) {
            element.setAttribute(attr[1], decode(attr[2] || ""));
          }
          element.parentNode = stack.at(-1);
          stack.at(-1).children.push(element);
          if (!["input", "br", "hr", "img", "meta", "link"].includes(element.tagName)) stack.push(element);
        } else {
          const textNode = new Element("#text");
          textNode.ownText = decode(token);
          textNode.parentNode = stack.at(-1);
          stack.at(-1).children.push(textNode);
        }
      }
      assert.equal(stack.length, 1, "fixture DOM requires balanced HTML");
    }
    matches(selector) {
      if (selector.startsWith("#")) return this.id === selector.slice(1);
      const [tag, ...classes] = selector.split(".");
      return (!tag || this.tagName === tag) && classes.every((name) => this.classList.contains(name));
    }
    querySelectorAll(selector) {
      return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    closest(selector) {
      for (let node = this; node; node = node.parentNode) if (node.matches(selector)) return node;
      return null;
    }
    contains(node) {
      for (let current = node; current; current = current.parentNode) if (current === this) return true;
      return false;
    }
    replaceWith(replacement) {
      const parent = this.parentNode;
      assert.ok(parent, "replacement target must be attached");
      if (replacement.parentNode) {
        const siblings = replacement.parentNode.children;
        siblings.splice(siblings.indexOf(replacement), 1);
      }
      parent.children[parent.children.indexOf(this)] = replacement;
      replacement.parentNode = parent;
      this.parentNode = null;
    }
  }
  const nodes = Object.fromEntries(["view", "breadcrumb", "status-upstream", "status-sse", "live-dot", "live-label"].map((id) => [id, new Element()]));
  return {
    nodes, htmlWrites,
    document: {
      getElementById(id) { return nodes[id] || nodes.view.querySelector("#" + id); },
      querySelectorAll() { return []; },
    },
  };
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function metadata(id, extra = {}) {
  return {
    id, from: { agent_id: "peer", agent_name: "Synthetic peer" },
    to: { kind: "agent", agent_id: "peer" }, subject: "Synthetic subject " + id,
    kind: "note", created_at: "2026-01-01T00:00:00Z",
    read_state: { state: "unread" }, delivery: { state: "pending" }, ...extra,
  };
}

function harness() {
  const viewDOM = dom();
  const requests = [], timers = new Map(), sources = [];
  let timerID = 0;
  let holdLists = false;
  const inbox = [metadata("shared"), metadata("second")];
  const outbox = [metadata("shared"), metadata("sent-only")];
  const window = { location: { hash: "#/conversations", host: "localhost" }, matchMedia: null };
  const sandbox = {
    module: { exports: {} }, window, document: viewDOM.document,
    URLSearchParams, AbortController, TextEncoder,
    setTimeout(callback, delay) { const id = ++timerID; timers.set(id, { callback, delay }); return id; },
    clearTimeout(id) { timers.delete(id); },
    fetch(url, options) {
      const pending = deferred();
      const request = { url, options, ...pending };
      requests.push(request);
      if (url === "/api/messages?direction=inbox&limit=100" && !holdLists) request.resolve(response({ messages: inbox }));
      else if (url === "/api/messages?direction=outbox&limit=100" && !holdLists) request.resolve(response({ messages: outbox }));
      else assert.match(url, /^\/api\/messages(?:\?direction=(?:inbox|outbox)&limit=100|\/[^/]+\/body)$/, "unexpected action or endpoint");
      return pending.promise;
    },
    EventSource: class {
      constructor(url) { this.url = url; this.listeners = {}; sources.push(this); }
      addEventListener(name, handler) { this.listeners[name] = handler; }
      close() { this.closed = true; }
      emit(name, value) { this.listeners[name]({ data: JSON.stringify(value) }); }
    },
  };
  const source = fs.readFileSync(path.join(__dirname, "../static/app.js"), "utf8");
  vm.runInNewContext(source, sandbox, { filename: "app.js" });
  const app = sandbox.module.exports;
  const h = {
    ...viewDOM, app, window, requests, timers, sources, inbox, outbox,
    holdLists(value) { holdLists = value; },
    bodyRequests() { return requests.filter((request) => request.url.endsWith("/body")); },
    preview(id = "shared") { return viewDOM.nodes.view.querySelectorAll(".message-preview").find((node) => node.getAttribute("data-message-id") === id); },
    click(node = h.preview()) { return app.onMessageBodyClick({ target: node.querySelector(".message-body-toggle") }); },
    async navigate(hash) { window.location.hash = hash; return app.route(); },
    frame(changes = {}) {
      sources.at(-1).emit("messages", { inbox: inbox.map((item) => ({ ...item, ...changes })), outbox });
    },
  };
  return h;
}

function response(body, status = 200) {
  return { ok: status >= 200 && status < 300, status, async json() { return body; } };
}

async function openThread(h) {
  await h.navigate("#/conversations");
  await h.navigate("#/conversations/peer");
  assert.ok(h.preview(), "actual rendered thread contains a received preview");
}

test("passive list, thread and SSE render no bodies or sent controls", async () => {
  const h = harness();
  await openThread(h);
  assert.equal(h.bodyRequests().length, 0);
  assert.equal(h.nodes.view.querySelectorAll(".bubble.received").length, 2);
  assert.equal(h.nodes.view.querySelectorAll(".bubble.sent").length, 2);
  for (const sent of h.nodes.view.querySelectorAll(".bubble.sent")) {
    assert.equal(sent.querySelector(".message-body-toggle"), null);
  }
  for (const received of h.nodes.view.querySelectorAll(".message-preview")) {
    assert.equal(received.querySelector(".message-body-toggle").textContent, "Show body");
    assert.equal(received.querySelector(".message-body-content").hidden, true);
    assert.equal(received.querySelector(".message-body-content").textContent, "");
  }
  h.frame();
  h.frame({ read_state: { state: "read" } });
  assert.equal(h.bodyRequests().length, 0);
  assert.equal(Object.keys(h.app.state.messages).length, 4, "same-id inbox/outbox entries remain separate");
});

test("explicit fetch renders hostile text literally, preserves whitespace and survives metadata repaint", async () => {
  const h = harness();
  await openThread(h);
  const hostile = '<script>throw new Error("executed")</script>\n<a href="https://invalid.test/">literal link</a>\n  indented & text';
  const priorMetadata = JSON.stringify(h.app.state.messages);
  const pending = h.click();
  const request = h.bodyRequests()[0];
  assert.equal(request.url, "/api/messages/shared/body");
  assert.equal(request.options.method, "GET");
  assert.equal(request.options.credentials, "same-origin");
  assert.equal(request.options.cache, "no-store");
  assert.ok(request.options.signal instanceof AbortSignal);
  assert.equal(h.preview("second").querySelector(".message-body-toggle").disabled, true);
  h.click(h.preview("second"));
  assert.equal(h.bodyRequests().length, 1, "only one body request may be active");
  request.resolve(response({ body: hostile }));
  await pending;
  const content = h.preview().querySelector(".message-body-content");
  assert.equal(content.textContent, hostile);
  assert.equal(content.hidden, false);
  assert.equal(content.children.length, 0, "body is a text assignment, never parsed HTML");
  assert.equal(h.htmlWrites.some((html) => html.includes(hostile)), false);
  assert.equal(h.preview().querySelector(".message-body-toggle").textContent, "Hide body");
  assert.equal(JSON.stringify(h.app.state.messages), priorMetadata);
  h.frame({ read_state: { state: "read" } });
  assert.equal(h.preview().querySelector(".message-body-content"), content, "same-view metadata repaint retains only the visible DOM node");
  assert.equal(content.textContent, hostile);
  assert.equal(h.bodyRequests().length, 1);
  assert.equal(JSON.stringify(h.app.state).includes(hostile), false);
  h.click();
  assert.equal(content.hidden, true);
  assert.equal(content.textContent, "");
  assert.equal(h.timers.size, 0);
});

test("hide cancels pending work and late completion cannot replace a newer reveal", async () => {
  const h = harness();
  await openThread(h);
  const old = h.click();
  const oldRequest = h.bodyRequests()[0];
  assert.equal(h.preview().querySelector(".message-body-toggle").textContent, "Hide body");
  h.click();
  assert.equal(h.bodyRequests().length, 1, "second click hides instead of duplicating pending fetch");
  assert.equal(oldRequest.options.signal.aborted, true);
  assert.equal(h.preview().querySelector(".message-body-content").textContent, "");
  const latest = h.click();
  h.bodyRequests()[1].resolve(response({ body: "new explicit response" }));
  await latest;
  oldRequest.resolve(response({ body: "stale hidden response" }));
  await old;
  assert.equal(h.preview().querySelector(".message-body-content").textContent, "new explicit response");
  assert.equal(h.timers.size, 0);
});

test("navigation clears immediately and leaving then returning rejects old body and metadata responses", async () => {
  const h = harness();
  await openThread(h);
  const shown = h.click();
  h.bodyRequests()[0].resolve(response({ body: "visible before navigation" }));
  await shown;
  const oldContent = h.preview().querySelector(".message-body-content");
  h.holdLists(true);
  const oldListView = h.navigate("#/conversations");
  assert.equal(oldContent.textContent, "", "navigation clears before destination reads finish");
  assert.equal(oldContent.hidden, true);
  const oldListReads = h.requests.slice(-2);
  h.holdLists(false);
  await h.navigate("#/conversations/peer");
  assert.equal(h.preview().querySelector(".message-body-content").hidden, true);
  const oldReveal = h.click();
  const oldRequest = h.bodyRequests()[1];
  await h.navigate("#/conversations");
  assert.equal(oldRequest.options.signal.aborted, true);
  await h.navigate("#/conversations/peer");
  const newReveal = h.click();
  h.bodyRequests()[2].resolve(response({ body: "current visit response" }));
  await newReveal;
  oldRequest.resolve(response({ body: "old visit response" }));
  await oldReveal;
  oldListReads[0].resolve(response({ messages: h.inbox }));
  oldListReads[1].resolve(response({ messages: h.outbox }));
  await oldListView;
  assert.equal(h.preview().querySelector(".message-body-content").textContent, "current visit response");
  assert.equal(h.bodyRequests().length, 3);
});

test("empty body is valid while failures settle only that message without retry or raw error text", async () => {
  for (const status of [403, 404, 405, 501, 503]) {
    const h = harness();
    await openThread(h);
    const pending = h.click();
    let errorBodyReads = 0;
    h.bodyRequests()[0].resolve({ ok: false, status, async json() { errorBodyReads++; return { error: "private upstream error text" }; } });
    await pending;
    assert.equal(errorBodyReads, 0);
    assert.match(h.preview().textContent, /Body unavailable\./);
    assert.equal(h.nodes.view.textContent.includes("private upstream"), false);
    assert.equal(h.preview().querySelector(".message-body-toggle").disabled, true);
    h.click();
    h.frame({ read_state: { state: "read" } });
    h.click();
    assert.equal(h.bodyRequests().length, 1, "unavailable message stays settled through repaint");
    const empty = h.click(h.preview("second"));
    h.bodyRequests()[1].resolve(response({ body: "" }));
    await empty;
    const second = h.preview("second");
    assert.equal(second.querySelector(".message-body-content").hidden, false);
    assert.equal(second.querySelector(".message-body-content").textContent, "");
    assert.equal(second.querySelector(".message-body-toggle").textContent, "Hide body");
  }
});

test("malformed or oversized successful projections remain unavailable", async () => {
  for (const invalid of [null, {}, { body: null }, { body: "safe", payload: "private" }, { body: "💧".repeat(16385) }]) {
    const h = harness();
    await openThread(h);
    const pending = h.click();
    h.bodyRequests()[0].resolve(response(invalid));
    await pending;
    assert.match(h.preview().textContent, /Body unavailable\./);
    assert.equal(h.preview().querySelector(".message-body-content").hidden, true);
    assert.equal(h.preview().querySelector(".message-body-content").textContent, "");
    assert.equal(h.timers.size, 0);
  }
});

test("timeout aborts once, enables another message, and rejects delayed completion", async () => {
  const h = harness();
  await openThread(h);
  const pending = h.click();
  const request = h.bodyRequests()[0];
  assert.equal(h.timers.size, 1);
  const timer = [...h.timers.values()][0];
  assert.ok(timer.delay > 0 && timer.delay <= 10000, "bounded preview timeout");
  timer.callback();
  assert.equal(request.options.signal.aborted, true);
  assert.match(h.preview().textContent, /Body unavailable\./);
  assert.equal(h.preview("second").querySelector(".message-body-toggle").disabled, false);
  request.resolve(response({ body: "too late" }));
  await pending;
  assert.equal(h.preview().querySelector(".message-body-content").textContent, "");
  assert.equal(h.bodyRequests().length, 1);
  assert.equal(h.timers.size, 0);
});

test("removed preview nodes are cleared and cannot receive delayed body text", async () => {
  const h = harness();
  await openThread(h);
  const node = h.preview();
  const pending = h.click();
  const request = h.bodyRequests()[0];
  delete h.app.state.messages["received shared"];
  h.app.renderConversation("peer");
  assert.equal(h.preview(), undefined);
  assert.equal(request.options.signal.aborted, true);
  request.resolve(response({ body: "removed message" }));
  await pending;
  assert.equal(node.querySelector(".message-body-content").textContent, "");
  assert.equal(h.nodes.view.textContent.includes("removed message"), false);
});
