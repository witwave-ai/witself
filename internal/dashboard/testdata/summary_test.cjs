"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { fixture, selfData, summaryData, currentDetail, emitSelf, headerVector, openDetails } = require("./overview_harness.cjs");
const options = { timeout: 5000 };
const reports = (h) => h.requests.filter((r) => r.url === "/api/summary");
const node = (h, id) => h.document.getElementById(id);
const text = (h) => node(h, "summary-body").textContent;
async function enter(h) {
  const self = h.begin("#/overview", "/api/self");
  await h.finish(self, selfData("current", 1));
  return reports(h).at(-1);
}
async function loaded(t, data = summaryData()) {
  const h = fixture(t, { controlledSummary: true });
  await h.finish(await enter(h), data);
  return h;
}
function select(h, view) { node(h, "summary-view-" + view).dispatch("click"); }

test("summary separates dimensions, closes labels, preserves zero/disabled/unavailable and bounded inventory", options, async (t) => {
  const data = summaryData(), cats = data.summary.categories;
  cats[1].activity.bins.fill(0); cats[1].activity.total = 0;
  cats[2].inventory.count = null;
  cats[3].inventory.status = "disabled";
  cats[4].activity.status = "disabled";
  const h = await loaded(t, data);
  assert.deepEqual(node(h, "summary-body").querySelectorAll(".summary-name").map((n) => n.textContent), [
    "OPS Transactions", "TRN Transcripts", "FCT Facts", "MEM Memories", "SEC Secrets", "EML Email", "MSG Messages",
  ]);
  assert.equal(node(h, "summary-row-transactions").tagName, "div");
  assert.equal(node(h, "summary-row-messages").getAttribute("href"), "#/conversations");
  assert.match(node(h, "summary-row-transcripts").textContent, /0entries recorded/);
  assert.match(node(h, "summary-row-facts").textContent, /unavailable/);
  assert.match(node(h, "summary-row-memories").textContent, /disabled.*unavailable/);
  assert.match(node(h, "summary-row-secrets").textContent, /4recent records · bounded.*disabled/);
  assert.match(text(h), /Bounded recent records are not inventory totals/);
  assert.doesNotMatch(text(h), /UNTRUSTED|grand total|total activity|360/);
  assert.equal(node(h, "summary-body").querySelectorAll(".summary-amount").length, 7, "only per-category quantities");
  assert.match(node(h, "summary-status").textContent, /Generated 2026-09-22T16:23:00.000Z/);
});

test("summary keeps private content out of DOM until explicit workspace disclosure", options, async (t) => {
  const data = summaryData();
  const privateValue = 'PRIVATE <script>canary</script> someone@example.com';
  data.summary.error = privateValue;
  data.summary.categories.forEach((c) => {
    c.label = c.code = c.inventory.label = c.activity.dimension = c.title = c.id = privateValue;
  });
  data.summary.recent = [
    { key: "transcripts", at: "2026-09-22T16:10:00Z", action: "transcript updated", subject: privateValue, id: privateValue },
    { key: privateValue, at: "2026-09-22T16:00:00Z", action: "updated" },
    { key: "facts", at: "2026-09-22T16:00:00Z", action: privateValue },
    { key: "facts", at: privateValue, action: "updated" },
  ];
  const h = await loaded(t, data);
  assert.equal(node(h, "workspace-content").textContent, "");
  for (const view of ["overview", "timeline", "recent"]) {
    select(h, view);
    assert.doesNotMatch(h.nodes.view.textContent, /PRIVATE|canary|@example|current salient|UNTRUSTED/);
  }
  assert.equal(node(h, "summary-body").querySelectorAll(".summary-update").length, 1);
  assert.match(text(h), /Latest 12 observations.*first pages.*100 records.*not a complete audit log/);
  openDetails(h);
  assert.match(node(h, "workspace-content").textContent, /current salient <literal>/);
  assert.equal(node(h, "workspace-content").querySelector("literal"), null);
  node(h, "workspace-details").open = false;
  node(h, "workspace-details").dispatch("toggle");
  assert.equal(node(h, "workspace-content").textContent, "");
  emitSelf(h, selfData("new private", 2));
  assert.doesNotMatch(h.nodes.view.textContent, /new private salient/);
  assert.deepEqual(h.requests.map((r) => r.url), ["/api/summary", "/api/self"], "summary never requests private content or active work");
});

test("summary polls despite identical SSE, coalesces, keeps selected buttons and row focus", options, async (t) => {
  const h = await loaded(t);
  select(h, "timeline");
  const button = node(h, "summary-view-timeline");
  button.focus();
  emitSelf(h, selfData("live", 2));
  emitSelf(h, selfData("live", 2));
  assert.equal(h.document.activeElement, button);
  assert.equal(node(h, "summary-view-timeline"), button, "SSE never replaces summary controls");
  assert.match(text(h), /░ 1–2 · ▒ 3–5 · ▓ 6–9 · █ 10\+/);
  assert.ok(node(h, "summary-body").querySelectorAll(".summary-pattern").every((n) => n.children.length === 24));
  await h.advance(29999);
  assert.equal(reports(h).length, 1);
  await h.advance(1);
  assert.equal(reports(h).length, 2);
  h.app.refreshSummary(); h.app.refreshSummary();
  assert.equal(reports(h).length, 2, "one request in flight");
  node(h, "summary-row-facts").focus();
  const fresh = summaryData(); fresh.summary.categories[2].inventory.count = 42;
  await h.finish(reports(h).at(-1), fresh);
  assert.equal(node(h, "summary-view-timeline").getAttribute("aria-pressed"), "true");
  assert.equal(h.document.activeElement.id, "summary-row-facts");
  assert.equal(h.document.activeElement.focusOptions.preventScroll, true, "refresh preserves scroll while restoring focus");
  select(h, "overview");
  assert.match(node(h, "summary-row-facts").textContent, /42facts/);
});

for (const removed of [false, true]) {
  test(`workspace details refresh with focused link ${removed ? "removed" : "preserved"}`, options, async (t) => {
    const h = await loaded(t);
    openDetails(h);
    const content = node(h, "workspace-content");
    const oldLink = content.querySelectorAll("a").find((link) => link.getAttribute("href").startsWith("#/memories/"));
    oldLink.focus();
    const changed = selfData("fresh", 913);
    changed.index.counts.facts = 913;
    if (!removed) changed.salient_memories[0].id = oldLink.getAttribute("href").slice("#/memories/".length);
    emitSelf(h, changed);
    assert.match(content.textContent, /913/);
    assert.match(content.textContent, /fresh salient/);
    assert.doesNotMatch(content.textContent, /current salient/);
    const focused = h.document.activeElement;
    assert.notEqual(focused, oldLink);
    if (removed) assert.equal(focused, node(h, "workspace-details").querySelector("summary"));
    else assert.equal(focused.getAttribute("href"), oldLink.getAttribute("href"));
    assert.equal(focused.focusOptions.preventScroll, true);
    node(h, "summary-view-overview").focus();
    emitSelf(h, changed);
    await h.advance(30000);
    assert.match(content.textContent, /fresh salient/);
  });
}

for (const returned of [false, true]) for (const phase of ["headers", "JSON"]) for (const failure of [false, true]) {
  test(`summary fences stale ${phase} ${failure ? "failure" : "success"} after ${returned ? "return" : "leave"}`, options, async (t) => {
    const h = fixture(t, { controlledSummary: true });
    const old = await enter(h);
    if (phase === "JSON") await h.headers(old);
    await currentDetail(h);
    assert.equal(old.options.signal.aborted, true);
    assert.equal(h.timers.size, 0, "leaving removes summary timers");
    if (returned) {
      const fresh = summaryData(); fresh.summary.categories[1].inventory.count = 73;
      await h.finish(await enter(h), fresh);
    }
    const before = h.nodes.view.textContent, count = h.requests.length, header = headerVector(h);
    if (failure) await h.jsonFailure(old, "PRIVATE old failure");
    else await h.finish(old, summaryData());
    assert.equal(h.nodes.view.textContent, before);
    assert.deepEqual(headerVector(h), header);
    assert.equal(h.requests.length, count);
    if (returned) assert.match(text(h), /73recent records/);
    else { await h.advance(120000); assert.equal(h.requests.length, count); }
  });
}

test("summary retains last success as stale on failure then recovers without erasing self", options, async (t) => {
  const h = await loaded(t), before = headerVector(h), body = text(h);
  await h.advance(30000);
  await h.jsonFailure(reports(h).at(-1), "PRIVATE upstream failure");
  assert.match(node(h, "summary-status").textContent, /Refresh failed · stale report/);
  assert.equal(text(h), body);
  assert.deepEqual(headerVector(h), before);
  assert.doesNotMatch(h.nodes.view.textContent, /PRIVATE/);
  await h.advance(29999); assert.equal(reports(h).length, 2);
  await h.advance(1); assert.equal(reports(h).length, 3);
  const fresh = summaryData(); fresh.summary.generated_at = fresh.summary.window.until = "2026-09-22T16:24:00Z";
  await h.finish(reports(h).at(-1), fresh);
  assert.match(node(h, "summary-status").textContent, /^Recorded snapshot/);
  assert.deepEqual(headerVector(h), before);
});

test("summary initial HTTP failure remains generic and does not substitute self inventory", options, async (t) => {
  const h = fixture(t, { controlledSummary: true });
  const request = await enter(h);
  request.releaseHeaders(503); request.body.resolve({ error: "PRIVATE" });
  await h.advance(0);
  assert.match(node(h, "summary-status").textContent, /Summary unavailable/);
  assert.equal(node(h, "summary-body").querySelectorAll(".summary-row").length, 0);
  assert.doesNotMatch(h.nodes.view.textContent, /PRIVATE|current salient/);
  assert.match(h.nodes["agent-name"].textContent, /current agent/);
});

test("summary pauses hidden polling, resumes due read, and self read cannot replace newer SSE", options, async (t) => {
  const h = await loaded(t);
  h.document.hidden = true; h.app.summaryVisibilityChanged();
  assert.equal(h.timers.size, 0);
  await h.advance(90000); assert.equal(reports(h).length, 1);
  h.document.hidden = false; h.app.summaryVisibilityChanged();
  assert.equal(reports(h).length, 2);
  await h.finish(reports(h).at(-1), summaryData());
  const self = h.begin("#/overview", "/api/self");
  emitSelf(h, selfData("newer SSE", 4));
  const before = headerVector(h);
  await h.finish(self, selfData("late old self", 1));
  assert.deepEqual(headerVector(h), before);
});

for (const malformed of ["-1", -1, 0.5, Number.MAX_SAFE_INTEGER + 1, null, NaN, Infinity]) {
  test("summary malformed quantity becomes unavailable: " + String(malformed), options, async (t) => {
    const data = summaryData();
    data.summary.categories[1].activity.bins[3] = malformed;
    data.summary.categories[2].inventory.count = malformed;
    const h = await loaded(t, data);
    assert.match(node(h, "summary-row-transcripts").querySelector(".summary-amount").textContent, /unavailable/);
    assert.match(node(h, "summary-row-facts").querySelector(".summary-inventory").textContent, /unavailable/);
  });
}

test("summary validates schema, lengths, units, totals and unknown status without rendering raw fields", options, async (t) => {
  const h = fixture(t);
  for (const change of [
    (s) => { s.schema = "unknown"; }, (s) => { s.generated_at = "PRIVATE"; },
    (s) => { s.categories.pop(); }, (s) => { s.categories[0].key = "facts"; },
    (s) => { s.recent = Array(13).fill({}); }, (s) => { s.checkpoints = []; },
    (s) => { s.window.since = "2026-09-21T18:00:00Z"; },
  ]) {
    const data = summaryData(); change(data.summary);
    assert.throws(() => h.app.normalizeSummary(data));
  }
  for (const change of [
    (a) => { a.bins.pop(); }, (a) => { a.total++; }, (a) => { a.unit = "PRIVATE"; }, (a) => { a.status = "PRIVATE"; },
  ]) {
    const data = summaryData(); change(data.summary.categories[1].activity);
    assert.equal(h.app.normalizeSummary(data).categories[1].activity.status, "unavailable");
  }
});

const allowedActions = [
  ["transcripts", "transcript updated"], ["transcripts", "transcript created"],
  ["memories", "memory updated"], ["memories", "memory created"],
  ["secrets", "secret updated"], ["secrets", "secret created"],
  ["email", "email received"], ["messages", "message received"],
];
test("summary admits only matching category/action pairs and orders observed updates", options, async (t) => {
  const data = summaryData();
  data.summary.recent = allowedActions.map(([key, action], i) => ({ key, action, at: `2026-09-22T16:0${i}:00Z` }));
  const h = await loaded(t, data);
  select(h, "recent");
  const rows = node(h, "summary-body").querySelectorAll(".summary-update");
  assert.equal(rows.length, 8);
  rows.forEach((row, i) => assert.ok(row.textContent.endsWith(allowedActions[7 - i][1])));
  for (const category of data.summary.categories) {
    for (const [key, action] of allowedActions) {
      data.summary.recent = [{ key: category.key, action, at: "2026-09-22T16:00:00Z" }];
      assert.equal(h.app.normalizeSummary(data).recent.length, category.key === key ? 1 : 0, `${category.key}: ${action}`);
    }
    for (const action of ["updated", "created", "sent", "received", "accessed", "delivered", "accepted", "recorded", "__proto__", "<img src=x>"]) {
      data.summary.recent = [{ key: category.key, action, at: "2026-09-22T16:00:00Z" }];
      assert.equal(h.app.normalizeSummary(data).recent.length, 0);
    }
  }
});

test("summary validates the metric dimension independently from its unit", options, async (t) => {
  const h = fixture(t);
  const dimensions = ["transcript_entry_write", "fact_returned", "secret_read", "email_sent", "message_sent"];
  const indices = [1, 2, 4, 5, 6];
  indices.forEach((index, i) => {
    const data = summaryData(), activity = data.summary.categories[index].activity;
    assert.equal(h.app.normalizeSummary(data).categories[index].activity.status, "available");
    for (const dimension of [...dimensions.filter((d) => d !== dimensions[i]), "", null, {}, "PRIVATE"]) {
      activity.dimension = dimension;
      assert.equal(h.app.normalizeSummary(data).categories[index].activity.status, "unavailable", `${index}: ${dimension}`);
    }
  });
});

test("summary rejects malformed bucket/window timestamps", options, async (t) => {
  const h = fixture(t);
  for (const change of [
    (s) => { s.window.bucket = "day"; },
    (s) => { s.window.until = "2026-09-22T17:00:00Z"; },
    (s) => { s.generated_at = "2026-02-30T16:23:00Z"; s.window.since = "2026-03-01T17:00:00Z"; s.window.until = "2026-03-02T16:23:00Z"; },
  ]) {
    const data = summaryData(); change(data.summary);
    assert.throws(() => h.app.normalizeSummary(data));
  }
});

test("summary bounds hanging reads and ignores their late completion after recovery", options, async (t) => {
  const h = await loaded(t);
  await h.advance(30000);
  const hung = reports(h).at(-1);
  await h.advance(15000);
  assert.equal(hung.options.signal.aborted, true);
  assert.match(node(h, "summary-status").textContent, /Refresh failed · stale report/);
  await h.advance(29999); assert.equal(reports(h).length, 2);
  await h.advance(1); assert.equal(reports(h).length, 3);
  const fresh = summaryData(); fresh.summary.categories[2].inventory.count = 99;
  await h.finish(reports(h).at(-1), fresh);
  const before = text(h);
  await h.finish(hung, summaryData());
  assert.equal(text(h), before);
  assert.match(text(h), /99facts/);
});

test("summary orders recognized observations and uses fixed checkpoint status labels", options, async (t) => {
  const data = summaryData();
  data.summary.recent = [
    { key: "transcripts", at: "2026-09-22T14:00:00Z", action: "transcript updated" },
    { key: "email", at: "2026-09-22T15:00:00Z", action: "email received" },
  ];
  data.summary.checkpoints.forEach((c, i) => { c.status = ["pending", "clear", "disabled", "PRIVATE"][i]; });
  const h = await loaded(t, data);
  select(h, "recent");
  assert.deepEqual(node(h, "summary-body").querySelectorAll(".summary-name").map((n) => n.textContent), ["EML Email", "TRN Transcripts"]);
  assert.deepEqual(node(h, "summary-checkpoints").querySelectorAll(".summary-checkpoint").map((n) => n.textContent), [
    "Memory curation pending", "Messaging work clear", "Email disabled", "Avatar lifecycle unavailable",
  ]);
});

for (const returned of [false, true]) {
  test(`summary ignores queued disclosure events after ${returned ? "return" : "leave"}`, options, async (t) => {
    const h = await loaded(t);
    const oldDetails = node(h, "workspace-details");
    await currentDetail(h);
    if (returned) {
      await h.finish(await enter(h), summaryData());
      openDetails(h);
    }
    const before = h.nodes.view.textContent;
    oldDetails.open = false; oldDetails.dispatch("toggle");
    oldDetails.open = true; oldDetails.dispatch("toggle");
    assert.equal(h.nodes.view.textContent, before);
  });
}

test("summary never invents transaction inventory or undefined history", options, async (t) => {
  const h = fixture(t);
  for (const status of ["available", "disabled", "unavailable", "PRIVATE"]) {
    const data = summaryData();
    data.summary.categories[0].inventory = { status, count: 9, exact: true };
    for (const index of [0, 3]) {
      data.summary.categories[index].activity = { status, bins: Array(24).fill(1), total: 24, unit: "messages sent", dimension: "message_sent" };
    }
    const report = h.app.normalizeSummary(data);
    assert.equal(report.categories[0].inventory.status, "unavailable");
    assert.equal(report.categories[0].activity.status, "unavailable");
    assert.equal(report.categories[3].activity.status, "unavailable");
  }
});


test("summary respects exact inventory contracts and omits future observations", options, async (t) => {
 const h=fixture(t), data=summaryData();
 data.summary.categories[1].inventory.exact=true;
 data.summary.categories[2].inventory.exact=false;
 data.summary.recent=[{key:"transcripts",action:"transcript updated",at:"2099-01-01T00:00:00Z"}];
 const report=h.app.normalizeSummary(data);
 assert.equal(report.categories[1].inventory.status,"unavailable");
 assert.equal(report.categories[2].inventory.status,"unavailable");
 assert.equal(report.recent.length,0);
 assert.equal(report.categories[3].inventory.label,"active memories");
});
