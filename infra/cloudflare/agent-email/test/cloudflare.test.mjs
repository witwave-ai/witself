import assert from "node:assert/strict";
import test from "node:test";

import { CloudflareAPI } from "../scripts/cloudflare.mjs";
import { EDGE_METRICS_DATASET, summaryQuery } from "../scripts/metrics.mjs";

test("Cloudflare client exposes no catch-all mutation and uses exact documented paths", async () => {
  const calls = [];
  const fetchAPI = async (url, init) => {
    calls.push({ url, ...init });
    const result = url.endsWith("/catch_all")
      ? { id: "f".repeat(32), enabled: true, matchers: [{ type: "all" }], actions: [] }
      : null;
    return Response.json({ success: true, errors: [], messages: [], result });
  };
  const api = new CloudflareAPI({
    accountID: "a".repeat(32), zoneID: "b".repeat(32), namespaceID: "c".repeat(32),
    apiToken: "test-token", fetchAPI,
  });
  await api.getCatchAll();
  await api.createRule({ enabled: false });
  await api.updateRule("d".repeat(32), { enabled: true });
  await api.deleteRule("d".repeat(32));
  await api.putKV("pilot:recipient:v1:a@example.com", { enabled: true });

  const catchCalls = calls.filter(({ url }) => url.endsWith("/catch_all"));
  assert.equal(catchCalls.length, 1);
  assert.equal(catchCalls[0].method, "GET");
  assert.equal(typeof api.updateCatchAll, "undefined");
  assert.equal(calls[1].method, "POST");
  assert.match(calls[1].url, /\/email\/routing\/rules$/);
  assert.equal(calls[2].method, "PUT");
  assert.match(calls[2].url, new RegExp(`/email/routing/rules/${"d".repeat(32)}$`));
  assert.equal(calls[3].method, "DELETE");
  assert.match(calls[4].url, /values\/pilot%3Arecipient%3Av1%3Aa%40example\.com$/);
  assert.equal(calls.every(({ headers }) => headers.Authorization === "Bearer test-token"), true);
});

test("Analytics Engine query uses the account-scoped SQL endpoint and remains value-free", async () => {
  const calls = [];
  const fetchAPI = async (url, init) => {
    calls.push({ url, ...init });
    return Response.json({ data: [{ outcome: "accepted", events: 3 }] });
  };
  const api = new CloudflareAPI({
    accountID: "a".repeat(32), apiToken: "analytics-token", fetchAPI,
  });
  const query = summaryQuery(60);
  const result = await api.queryAnalytics(query);
  assert.deepEqual(result.data, [{ outcome: "accepted", events: 3 }]);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "POST");
  assert.match(calls[0].url, /\/accounts\/a{32}\/analytics_engine\/sql$/);
  assert.equal(calls[0].headers.Authorization, "Bearer analytics-token");
  assert.match(calls[0].body, new RegExp(`FROM ${EDGE_METRICS_DATASET}`));
  assert.match(calls[0].body, /INTERVAL '60' MINUTE/);
  assert.match(calls[0].body, /blob1 = 'witself\.agent-email\.edge\.v1'/);
  assert.doesNotMatch(calls[0].body, /address|subject|message_id|agent_id|realm_id/i);
});

test("edge metrics summary window is strictly bounded", () => {
  for (const value of [0, -1, 10_081, 1.5, Number.NaN]) {
    assert.throws(() => summaryQuery(value), /minutes must be an integer/);
  }
});
