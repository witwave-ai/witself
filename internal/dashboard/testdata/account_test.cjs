"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const { fixture, context, payload, sections, nextTurn } = require("./account_harness.cjs");
const support = { ticket: { id: "ticket_one", subject: "Question" }, messages: [{ author_kind: "operator", posted_at: "2026-09-24", body: "PRIVATE_THREAD_CANARY <img src=x onerror=alert(1)> https://evil.invalid" }] };
const accountRequests = h => h.requests.filter(r => r.url.startsWith("/api/account/") && r.url !== "/api/account/context");

test("boot reads only context; absent and malformed managers never expose Account or request sections", async t => {
  for (const data of [{ schema_version: "witself.console.account.v1", available: false }, {},
    context("agent"), context("account_owner", { schema_version: "unknown" }),
    context("account_owner", { account_id: "" }), context("account_owner", { operator_id: "bad/id" }),
    context("account_owner", { sections: ["overview", "unknown"] }),
    context("account_owner", { sections: ["overview", "overview"] }),
    context("account_operator", { sections }), context("account_owner", { sections: "overview" })]) {
    const h = fixture(t, "#/account/support/ticket_one");
    assert.equal(h.accountLinks().length, 0);
    h.app.account.start(); h.app.route();
    assert.equal(accountRequests(h).length, 0, "direct hash gated before response");
    await h.pending("/api/account/context").finish(data);
    assert.equal(h.accountLinks().length, 0);
    assert.equal(accountRequests(h).length, 0);
    assert.equal(h.window.location.hash, "#/overview");
  }
  const h = fixture(t);
  await h.boot();
  assert.equal(accountRequests(h).length, 0, "authorized boot does not prefetch");
  assert.equal(h.accountLinks().length, 1);
});

test("owner sees six ordered sections and inert account profile; operator hides Plan and Billing", async t => {
  const h = fixture(t);
  await h.boot();
  await h.section("overview", { account: { id: "acct_fixture", display_name: "<img onerror=evil()>", email: "x@example.invalid" } });
  const nav = h.nodes.view.querySelector(".account-sections");
  assert.deepEqual(nav.querySelectorAll("a").map(a => a.textContent), ["Overview", "Clients on this device", "Plan & limits", "Billing", "Support", "Access"]);
  assert.match(h.text(), /AccountRead-only/);
  assert.match(h.text(), /Account ID: acct_fixture · Manager role: Owner/);
  assert.doesNotMatch(h.text(), /op_fixture/);
  assert.match(h.text(), /<img onerror=evil\(\)>/);
  assert.equal(h.nodes.view.querySelector("img"), null);
  assert.equal(h.nodes.view.querySelectorAll("a").some(a => a.getAttribute("href")?.startsWith("mailto:")), false);
  await h.check(context("account_operator"));
  await h.pending("/api/account/overview").finish(payload({ account: {} }));
  assert.deepEqual(h.nodes.view.querySelector(".account-sections").querySelectorAll("a").map(a => a.textContent), ["Overview", "Clients on this device", "Support", "Access"]);
  const count = accountRequests(h).length;
  await h.navigate("#/account/billing");
  assert.equal(h.window.location.hash, "#/overview");
  assert.equal(accountRequests(h).length, count);
});

test("direct support hash preserves ID only after authority; unsafe routes never issue reads", async t => {
  const h = fixture(t, "#/account/support/ticket_one");
  await h.boot();
  assert.equal(accountRequests(h).length, 1);
  await h.pending("/api/account/support/ticket_one").finish(payload(support));
  assert.match(h.text(), /PRIVATE_THREAD_CANARY/);
  assert.equal(h.nodes.view.querySelector("img"), null);
  assert.equal(h.nodes.view.querySelectorAll("a").some(a => a.getAttribute("href").startsWith("http")), false);
  for (const route of ["#/account/support/a%2Fb", "#/account/support/%E0%A4%A", "#/account/support/a/extra", "#/account/billing/a", "#/account/support/a?q=x"]) {
    const count = accountRequests(h).length;
    await h.navigate(route);
    assert.equal(h.window.location.hash, "#/overview");
    assert.equal(accountRequests(h).length, count);
  }
});

test("selected thread route is dropped by refresh, hide and pagehide until deliberate reselection", async t => {
  for (const action of ["refresh", "hide", "pagehide"]) {
    const h = fixture(t); await h.boot();
    await h.section("support", support, "ticket_one");
    const threadCount = () => h.requests.filter(r => r.url === "/api/account/support/ticket_one").length;
    const before = threadCount();
    if (action === "refresh") h.document.getElementById("account-refresh").dispatch("click");
    else if (action === "hide") await h.visibility(true);
    else h.windowListeners.get("pagehide")();
    assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
    assert.equal(h.window.location.hash, "#/account/support");
    if (action !== "refresh") {
      if (action === "hide") await h.visibility(false);
      else h.windowListeners.get("pageshow")({ persisted: true });
      await h.pending("/api/account/context").finish(context());
    }
    await h.pending("/api/account/support").finish(payload({ tickets: [{ id: "ticket_one", subject: "Metadata" }] }));
    await h.check();
    assert.equal(threadCount(), before);
    assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
    await h.section("support", support, "ticket_one");
    assert.equal(threadCount(), before + 1);
    assert.match(h.text(), /PRIVATE_THREAD_CANARY/);
    await h.navigate("#/account/support/ticket_one");
    const old = h.pending("/api/account/support/ticket_one");
    await h.visibility(true);
    await old.finish(payload(support));
    assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
  }
});

test("identity and role changes drop selected support route; unchanged checks never fetch bodies", async t => {
  for (const next of [context("account_operator"), context("account_owner", { account_id: "acct_new" }),
    context("account_owner", { operator_id: "op_new" })]) {
    const h = fixture(t); await h.boot();
    await h.section("support", support, "ticket_one");
    await h.check();
    const before = accountRequests(h).length;
    await h.check(next);
    assert.equal(h.window.location.hash, "#/account/support");
    assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
    await h.pending("/api/account/support").finish(payload({ tickets: [] }));
    await h.check(next);
    assert.equal(accountRequests(h).length, before + 1);
    await h.section("support", support, "ticket_one");
    assert.match(h.text(), /PRIVATE_THREAD_CANARY/);
  }
});

test("context polling revokes independently of slow selected read, and old context cannot restore access", async t => {
  const h = fixture(t, "#/account/support/ticket_one");
  await h.boot();
  const slow = h.pending("/api/account/support/ticket_one");
  h.tick(5000);
  await h.pending("/api/account/context").fail();
  assert.equal(slow.options.signal.aborted, true);
  assert.equal(h.accountLinks().length, 0);
  assert.equal(h.window.location.hash, "#/overview");
  await slow.finish(payload(support));
  assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY|PRIVATE_RAW_ERROR/);
  h.app.account.check(); const old = h.pending("/api/account/context");
  h.app.account.check(); const fresh = h.requests.at(-1);
  assert.equal(old.options.signal.aborted, true);
  await fresh.finish(payload({ available: false }));
  await old.finish(context());
  assert.equal(h.accountLinks().length, 0);
});

test("role narrowing and manager/account identity changes cancel old reads and clear selected data", async t => {
  for (const next of [context("account_operator"), context("account_owner", { operator_id: "op_new" }),
    context("account_owner", { account_id: "acct_new" }), context("account_owner", { sections: ["overview", "clients", "access"] })]) {
    const h = fixture(t, "#/account/billing");
    await h.boot();
    const old = h.pending("/api/account/billing");
    await h.check(next);
    assert.equal(old.options.signal.aborted, true);
    await old.finish(payload({ summary: { available: true, subscription_status: "OLD_PRIVATE_BILLING" } }));
    assert.doesNotMatch(h.text(), /OLD_PRIVATE_BILLING/);
    if (next.role === "account_operator" || next.sections.length === 3) assert.equal(h.window.location.hash, "#/overview");
    else {
      await h.pending("/api/account/billing").finish(payload({}));
      assert.doesNotMatch(h.text(), /op_fixture/);
      assert.match(h.text(), new RegExp(next.account_id));
    }
  }
});

test("explicit device check runs exactly once; no scans at boot/navigation/poll/refresh/retry", async t => {
  const h = fixture(t); await h.boot();
  await h.section("clients", { status: "not_checked", report: null });
  h.tick(5000); await h.pending("/api/account/context").finish(context());
  h.document.getElementById("account-refresh").dispatch("click");
  await h.pending("/api/account/clients").finish(payload({ status: "not_checked" }));
  assert.equal(h.requests.filter(r => r.url.endsWith("/scan")).length, 0);
  const button = h.document.getElementById("account-scan");
  button.dispatch("click"); button.dispatch("click");
  assert.equal(h.requests.filter(r => r.url.endsWith("/scan")).length, 1);
  assert.equal(button.disabled, true);
  await h.pending("/api/account/clients/scan").finish(payload({ status: "checked", report: {
    schema_version: "witself.console.clients.v1", checked_at: "2026-09-24T01:00:00Z", scan_status: "partial", truncated: true,
    entries: [{ runtime: "codex", recorded_version: "1.2.3", configuration_status: "match", configuration_scope: "mcp_registration", effective_verification: "not_run" }],
  } }));
  assert.match(h.text(), /Recorded installation version1.2.3/);
  assert.match(h.text(), /Local checked time2026-09-24/);
  assert.match(h.text(), /Recorded MCP registration only/);
  assert.match(h.text(), /Effective verificationNot run/);
  assert.match(h.text(), /incomplete or unavailable/);
  assert.match(h.text(), /truncated/);
  await h.section("clients", { status: "not_checked" });
  h.document.getElementById("account-scan").dispatch("click");
  const scan = h.pending("/api/account/clients/scan");
  await h.check(payload({ available: false }));
  assert.equal(scan.options.signal.aborted, true);
  await scan.finish(payload({ report: { entries: [{ runtime: "STALE_SCAN" }] } }));
  assert.doesNotMatch(h.text(), /STALE_SCAN/);
  const count = h.requests.length; await nextTurn();
  assert.equal(h.requests.length, count, "no automatic retry");
});

test("billing keeps exact cents and currency, true zero, unsafe/missing values, and independent source errors", async t => {
  const h = fixture(t); await h.boot();
  await h.section("billing", {
    summary: { available: true, next_charge: { amount_cents: 0, currency: "USD" }, pending: { kind: "scheduled", plan: "team" } },
    invoices: { available: true, truncated: true, entries: [
      { number: "zero", amount_cents: 0, currency: "EUR" }, { number: "unknown", amount_cents: null, currency: "CAD" },
      { number: "unsafe", amount_cents: 9007199254740992, currency: "JPY" }, { number: "fraction", amount_cents: 1.5, currency: "GBP" },
      { number: "max", amount_cents: Number.MAX_SAFE_INTEGER, currency: "USD" }, { number: "negative", amount_cents: -123, currency: "EUR" },
    ] }, payments: { available: false, error: "unavailable" },
  });
  assert.match(h.text(), /USD 0.00 \(0 cents\)/);
  assert.match(h.text(), /EUR 0.00 \(0 cents\)/);
  for (const currency of ["CAD", "JPY", "GBP"]) assert.match(h.text(), new RegExp("Unknown amount · " + currency));
  assert.match(h.text(), /USD 90071992547409.91 \(9007199254740991 cents\)/);
  assert.match(h.text(), /EUR -1.23 \(-123 cents\)/);
  assert.match(h.text(), /Source unavailable. This is not an empty history/);
  assert.match(h.text(), /truncated/);
  assert.match(h.text(), /Pending planteam/);
  assert.equal(h.nodes.view.querySelectorAll("button").length, 1, "only section refresh, no billing actions");
  await h.section("billing", { summary: { available: true }, invoices: { available: true, entries: [] }, payments: { available: true, entries: [] } });
  assert.match(h.text(), /Unknown amount · Unknown currency/);
  assert.match(h.text(), /No invoices returned/); assert.match(h.text(), /No payments returned/);
});

test("plan distinguishes current/applied/pending, unknown/zero limits, overrides and absent/indefinite retention", async t => {
  const h = fixture(t); await h.boot();
  await h.section("plan", { plan: "team", applied: "standard", apply_pending: true, pending: { plan: "free", effective: "2026-10-01" },
    limits: { agents: 0, stored_fact: null }, limits_units: { agents: "count" }, limit_defaults: { agents: 3, stored_fact: 9 }, limit_defaults_units: { agents: "count", stored_fact: "count" },
    features: ["memory"], feature_defaults: ["memory", "messaging"], messaging: { enabled: false, default_enabled: true, overridden: true },
    transcript_retention: { effective_days: null, default_days: 30, overridden: true }, email_retention: { effective_days: 90 },
  });
  assert.match(h.text(), /Current planteam/); assert.match(h.text(), /Applied planstandard/); assert.match(h.text(), /Pending planfree/);
  assert.match(h.text(), /Effective limit0 count/); assert.match(h.text(), /Effective limitUnknown/);
  assert.match(h.text(), /EnabledNoPlan defaultYesOverride appliedYes/);
  assert.match(h.text(), /Effective retentionIndefinitePlan default30 days/);
  assert.match(h.text(), /Effective retention90 daysPlan defaultUnknown/);
  assert.match(h.text(), /message retentionEffective retentionUnknown/);
});

test("plan shows unlimited overrides and every supported unlimited dimension using the full core unit-map seam", async t => {
  const catalog = require("../../../web/plans/plans.json");
  const contract = require("../../../infra/cloudflare/control-plane/src/plan-contract.json");
  const free = catalog.plans.find(plan => plan.id === "free");
  const limits = { ...free.limits };
  delete limits.stored_secret;
  const units = Object.fromEntries(Object.keys(contract.limit_maximums).map(key => [key, key.endsWith("_bytes") ? "bytes" : "count"]));
  const h = fixture(t); await h.boot();
  await h.section("plan", { plan: free.id, limits, limits_units: units, limit_defaults: free.limits, limit_defaults_units: units });
  const items = h.nodes.view.querySelectorAll(".account-item");
  for (const key of Object.keys(contract.limit_maximums)) {
    const item = items.find(item => item.querySelector("h3").textContent === key.replace(/_/g, " "));
    assert.ok(item, key + " must remain visible even when absent from both maps");
    const effective = Object.hasOwn(limits, key) ? limits[key] + " " + units[key] : "No plan cap";
    const defaultLimit = Object.hasOwn(free.limits, key) ? free.limits[key] + " " + units[key] : "No plan cap";
    assert.equal(item.textContent, key.replace(/_/g, " ") + "Effective limit" + effective + "Plan default" + defaultLimit);
  }
  assert.match(h.text(), /stored secretEffective limitNo plan capPlan default0 count/);
});

test("plan distinguishes empty limit maps from missing or malformed maps and invalid entries", async t => {
  const h = fixture(t); await h.boot();
  const units = { stored_secret: "count" };
  for (const invalid of [undefined, null, [], "bad", 0, false]) {
    await h.section("plan", { limits: invalid, limit_defaults: {}, limit_defaults_units: units });
    assert.match(h.text(), /stored secretEffective limitUnknownPlan defaultNo plan cap/);
    await h.section("plan", { limits: {}, limits_units: units, limit_defaults: invalid });
    assert.match(h.text(), /stored secretEffective limitNo plan capPlan defaultUnknown/);
  }
  for (const invalid of [null, "0", -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    await h.section("plan", { limits: { stored_secret: invalid }, limit_defaults: { stored_secret: invalid }, limits_units: units, limit_defaults_units: units });
    assert.match(h.text(), /stored secretEffective limitUnknownPlan defaultUnknown/);
  }
  await h.section("plan", { limits: {}, limit_defaults: {}, limits_units: units, limit_defaults_units: units });
  assert.match(h.text(), /stored secretEffective limitNo plan capPlan defaultNo plan cap/);
});

test("support source failures are not empty; response_too_large keeps authorization and never retries", async t => {
  const h = fixture(t); await h.boot();
  await h.section("support", { tickets: [], truncated: true });
  assert.match(h.text(), /No support tickets returned/); assert.match(h.text(), /truncated/);
  await h.navigate("#/account/support/ticket_one");
  await h.pending("/api/account/support/ticket_one").finish(payload({ available: false, error: "response_too_large" }), 502);
  assert.match(h.text(), /thread exceeds the console read limit \(4 MiB\)/);
  assert.match(h.text(), /existing support CLI/);
  assert.equal(h.accountLinks().length, 1);
  const count = accountRequests(h).length;
  h.tick(5000); await h.pending("/api/account/context").finish(context());
  assert.equal(accountRequests(h).length, count);
  await h.navigate("#/account/support");
  await h.pending("/api/account/support").fail();
  assert.match(h.text(), /section is unavailable/); assert.doesNotMatch(h.text(), /No support tickets|PRIVATE_RAW_ERROR/);
});

test("context timeouts fail closed even when transport ignores abort; hide fences delayed context", async t => {
  const h = fixture(t); await h.boot();
  await h.section("support", support, "ticket_one");
  h.tick(5000); const slow = h.pending("/api/account/context");
  h.tick(10000); await nextTurn();
  assert.equal(slow.options.signal.aborted, true); assert.equal(h.accountLinks().length, 0);
  assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
  await slow.finish(context()); assert.equal(h.accountLinks().length, 0);
  h.app.account.check(); const hidden = h.pending("/api/account/context");
  await h.visibility(true); await hidden.finish(context());
  assert.equal(h.accountLinks().length, 0);
});

test("real browser boot verifies Account before direct section read and preserves focused rail link during polls", async t => {
  const h = fixture(t, "#/account/support/ticket_one", true);
  assert.equal(h.accountLinks().length, 0);
  assert.equal(accountRequests(h).length, 0);
  await h.pending("/api/account/context").finish(context());
  await h.pending("/api/account/support/ticket_one").finish(payload(support));
  const link = h.accountLinks()[0]; link.focus();
  h.tick(5000); await h.pending("/api/account/context").finish(context());
  assert.equal(h.accountLinks()[0], link);
  assert.equal(h.document.activeElement, link);
  assert.match(h.text(), /PRIVATE_THREAD_CANARY/);
  h.windowListeners.get("pagehide")();
  assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
  assert.equal(h.accountLinks().length, 0);
  h.windowListeners.get("pageshow")({ persisted: true });
  assert.equal(h.accountLinks().length, 0);
  await h.pending("/api/account/context").finish(payload({ available: false }));
  assert.equal(h.window.location.hash, "#/overview");
});

test("selected section authorization loss hides Account and clears data before revalidation", async t => {
  const h = fixture(t); await h.boot();
  await h.section("support", support, "ticket_one");
  h.document.getElementById("account-refresh").dispatch("click");
  await h.pending("/api/account/support").finish({}, 403);
  assert.equal(h.accountLinks().length, 0);
  assert.doesNotMatch(h.text(), /PRIVATE_THREAD_CANARY/);
  assert.equal(h.window.location.hash, "#/overview");
  await h.pending("/api/account/context").finish(payload({ available: false }));
  assert.equal(h.accountLinks().length, 0);
});


test("slow agent secret metadata reads cannot replace the selected Account view", async t => {
  for (const route of ["#/secrets", "#/secrets/secret_fixture"]) for (const outcome of ["success", "failure"]) {
    const h = fixture(t); await h.boot();
    await h.navigate(route);
    const old = h.pending(route === "#/secrets" ? "/api/secrets?limit=100" : "/api/secrets/secret_fixture");
    await h.section("support", support, "ticket_one");
    if (outcome === "failure") await old.fail();
    else await old.finish({ secrets: [{ id: "secret_fixture", name: "STALE_AGENT_METADATA" }], secret: { id: "secret_fixture", name: "STALE_AGENT_METADATA" } });
    assert.match(h.text(), /PRIVATE_THREAD_CANARY/);
    assert.doesNotMatch(h.text(), /STALE_AGENT_METADATA|PRIVATE_RAW_ERROR/);
  }
});

test("fresh Access reconciles authority before rendering; malformed Access fails closed", async t => {
  const h = fixture(t); await h.boot();
  await h.section("access", context("account_operator"));
  assert.match(h.text(), /Manager role: Operator/);
  assert.match(h.text(), /Manager role \(raw\)account_operator/);
  assert.match(h.text(), /Current CLI managerop_fixture/);
  assert.doesNotMatch(h.text(), /Owner|account_owner|Billing|Plan & limits/);
  await h.navigate("#/account/billing");
  assert.equal(h.window.location.hash, "#/overview");
  for (const invalid of [payload(), context("unknown"), context("account_operator", { sections }),
    context("account_owner", { sections: null }), context("account_owner", { operator_id: null }),
    context("account_owner", { schema_version: "unknown" })]) {
    const f = fixture(t); await f.boot();
    await f.section("access", invalid);
    assert.equal(f.accountLinks().length, 0);
    assert.equal(f.window.location.hash, "#/overview");
    assert.doesNotMatch(f.text(), /Owner|Billing/);
  }
});

test("Access and context share an ordering fence for delayed success and rejected responses", async t => {
  // A context check begun before Access cannot restore the older owner role.
  for (const outcome of ["success", "failure"]) {
    const h = fixture(t); await h.boot();
    h.app.account.check(); const old = h.pending("/api/account/context");
    await h.section("access", context("account_operator"));
    assert.equal(old.options.signal.aborted, true);
    if (outcome === "success") await old.finish(context()); else await old.fail();
    assert.match(h.text(), /Manager role: Operator/);
    assert.doesNotMatch(h.text(), /Billing|Owner/);
    h.tick(5000); await h.pending("/api/account/context").finish(context("account_operator"));
  }
  // A newer unchanged context check also supersedes Access: test error paths
  // without relying on a changed context to cancel the whole view.
  for (const outcome of ["success", "failure", "forbidden", "malformed"]) {
    const h = fixture(t); await h.boot(context("account_operator"));
    await h.navigate("#/account/access"); const old = h.pending("/api/account/access");
    await h.check(context("account_operator"));
    if (outcome === "failure") await old.fail();
    else await old.finish(outcome === "malformed" ? {} : context(), outcome === "forbidden" ? 403 : 200);
    assert.equal(h.accountLinks().length, 1);
    assert.match(h.text(), /Manager role: Operator/);
    assert.doesNotMatch(h.text(), /Billing|Owner/);
  }
  const h = fixture(t); await h.boot();
  await h.navigate("#/account/access"); const old = h.pending("/api/account/access");
  await h.check(context("account_operator"));
  await h.requests.at(-1).finish(context("account_operator"));
  await old.finish(context());
  assert.match(h.text(), /Manager role: Operator/);
  assert.doesNotMatch(h.text(), /Billing|Owner/);
});

test("scan and refresh preserve attached named-action focus on success/error and suppress duplicates", async t => {
  for (const scan of [false, true]) for (const outcome of ["success", "failure"]) {
    const h = fixture(t); await h.boot();
    await h.section("clients", { status: "not_checked" });
    const id = scan ? "account-scan" : "account-refresh";
    const action = h.document.getElementById(id);
    const before = accountRequests(h).length;
    action.focus(); action.dispatch("click"); action.dispatch("click");
    assert.equal(accountRequests(h).length, before + 1);
    assert.equal(action.isConnected, !scan, "refresh keeps its actual node; scan removes private result content");
    assert.equal(action.disabled, true);
    const request = h.pending("/api/account/clients" + (scan ? "/scan" : ""));
    if (outcome === "success") await request.finish(payload({ status: "not_checked" })); else await request.fail();
    const replacement = h.document.getElementById(id);
    assert.ok(replacement.isConnected);
    assert.equal(h.document.activeElement, replacement);
    assert.equal(Boolean(replacement.disabled), false);
    if (scan) assert.notEqual(replacement, action); else assert.equal(replacement, action);
  }
});

test("action completions never steal focus after another choice, navigation, hide, or authority change", async t => {
  for (const scan of [false, true]) for (const outcome of ["success", "failure"]) {
    for (const change of ["control", "control_then_blur", "navigate", "hide", "authority"]) {
      const h = fixture(t); await h.boot();
      await h.section("clients", { status: "not_checked" });
      const action = h.document.getElementById(scan ? "account-scan" : "account-refresh");
      action.focus(); action.dispatch("click");
      const old = h.pending("/api/account/clients" + (scan ? "/scan" : ""));
      let chosen;
      if (change.startsWith("control")) {
        chosen = h.accountLinks()[0]; chosen.focus();
        if (change === "control_then_blur") { chosen = h.document.body; chosen.focus(); }
      } else if (change === "navigate") {
        await h.section("overview", { account: {} });
        chosen = h.document.getElementById("account-refresh"); chosen.focus();
      } else if (change === "hide") {
        await h.visibility(true); chosen = h.document.body;
      } else {
        await h.check(context("account_operator"));
        await h.requests.at(-1).finish(payload({ status: "not_checked" }));
        chosen = h.document.getElementById("account-refresh"); chosen.focus();
      }
      if (outcome === "success") await old.finish(payload({ status: "not_checked" })); else await old.fail();
      assert.equal(h.document.activeElement, chosen, `${scan}/${outcome}/${change}`);
      assert.ok(chosen.isConnected);
    }
  }
});

test("both-empty valid limit maps use all dimensions from the shared unit maps; missing maps stay unknown", async t => {
  const contract = require("../../../infra/cloudflare/control-plane/src/plan-contract.json");
  const units = Object.fromEntries(Object.keys(contract.limit_maximums).map(key => [key, "count"]));
  const h = fixture(t); await h.boot();
  await h.section("plan", { limits: {}, limits_units: units, limit_defaults: {}, limit_defaults_units: units });
  for (const key of Object.keys(units)) {
    const item = h.nodes.view.querySelectorAll(".account-item").find(item => item.querySelector("h3").textContent === key.replace(/_/g, " "));
    assert.equal(item.textContent, key.replace(/_/g, " ") + "Effective limitNo plan capPlan defaultNo plan cap");
  }
  assert.doesNotMatch(h.text(), /Unlimited/);
  await h.section("plan", {});
  assert.match(h.text(), /Limits unknown/);
  assert.doesNotMatch(h.text(), /No plan cap/);
  await h.section("plan", { limits: { new_dimension: 0, safe_max: Number.MAX_SAFE_INTEGER }, limit_defaults: {} });
  assert.match(h.text(), /new dimensionEffective limit0 UnknownPlan defaultNo plan cap/);
  assert.match(h.text(), /safe maxEffective limit9007199254740991 Unknown/);
  await h.section("plan", { limits: { malformed_only: null }, limit_defaults: {} });
  assert.doesNotMatch(h.text(), /malformed only|No plan cap/);
});
