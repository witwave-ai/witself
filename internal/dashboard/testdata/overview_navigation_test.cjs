"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { fixture, valueCopy, headerVector, selfData, headerMatches, navigation, visible, beginOverview, currentOverview, currentDetail, emitSelf, destination, unchangedAfter, baseline, openDetails } = require("./overview_harness.cjs");
const options = { timeout: 5000 };

test("overview navigation renders current inventory and checkpoints", options, async (t) => {
  const h = fixture(t);
  const self = selfData("current", 7);
  const request = beginOverview(h);
  await h.headers(request);
  assert.equal(h.document.getElementById("workspace-content").textContent, "", "pending self JSON does not render details prematurely");
  await h.finish(request, self);
  openDetails(h);
  visible(h, "#/overview", "overview", "Agent summary", self.salient_memories[0].snippet);
  headerMatches(h, self);
  assert.deepEqual(h.nodes.view.querySelectorAll(".card").map((card) => [
    card.querySelector(".label").textContent, card.querySelector(".num").textContent,
  ]), [["facts", "3"], ["memories", "7"], ["secrets", "2"], ["transcripts", "4"]], "current inventory renders all sorted counts");
  assert.deepEqual(h.nodes.view.querySelectorAll("a.card-link").map((node) => node.getAttribute("href")), ["#/facts", "#/memories", "#/secrets"], "current inventory links retain their destination sections");
  const panels = h.nodes.view.querySelectorAll(".panel");
  const salient = panels.find((panel) => panel.querySelector("h2")?.textContent === "Salient memories");
  assert.equal(salient.querySelector("a").getAttribute("href"), "#/memories/mem_current", "current salient memory links to its detail");
  assert.equal(salient.querySelector("a").textContent, "current salient <literal>", "salient snippet remains literal text");
  assert.equal(h.nodes.view.querySelector("literal"), null, "current Overview values stay escaped");
  const checkpoints = panels.find((panel) => panel.querySelector("h2")?.textContent === "Checkpoints");
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
    assert.equal(h.document.getElementById("overview-self-status").textContent, "Workspace details unavailable · self refresh failed", "current self failure remains visible without disclosing errors");
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
  assert.equal(h.requests.length, 3, "normal Overview-to-detail navigation uses self, summary, and transcript reads");
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
    if (scenario.sameURL) visible(h, "#/overview", "overview", "Agent summary", current.salient_memories[0].snippet);
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
