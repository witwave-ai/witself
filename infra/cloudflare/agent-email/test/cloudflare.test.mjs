import assert from "node:assert/strict";
import test from "node:test";

import { CloudflareAPI, cloudflareEnvironment } from "../scripts/cloudflare.mjs";
import {
  EDGE_METRICS_DATASET,
  routeLookupSummaryQuery,
  runMetrics,
  summaryQuery,
} from "../scripts/metrics.mjs";

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

test("Cloudflare client reads every Email Routing rule page and fails closed on drift", async () => {
  const calls = [];
  const pages = new Map([
    [1, [{ id: "1" }]],
    [2, [{ id: "2" }]],
  ]);
  const fetchAPI = async (url, init) => {
    calls.push({ url, ...init });
    const page = Number(new URL(url).searchParams.get("page"));
    const result = pages.get(page) ?? [];
    return Response.json({
      success: true,
      errors: [],
      messages: [],
      result,
      result_info: {
        page,
        per_page: 50,
        count: result.length,
        total_pages: 2,
        total_count: 2,
      },
    });
  };
  const api = new CloudflareAPI({
    accountID: "a".repeat(32), zoneID: "b".repeat(32), apiToken: "test-token", fetchAPI,
  });

  assert.deepEqual(await api.listRules(), [{ id: "1" }, { id: "2" }]);
  assert.equal(calls.length, 4);
  assert.match(calls[0].url, /\?page=1&per_page=50$/);
  assert.match(calls[1].url, /\?page=2&per_page=50$/);
  assert.match(calls[2].url, /\?page=1&per_page=50$/);
  assert.match(calls[3].url, /\?page=2&per_page=50$/);

  let requestCount = 0;
  const drifting = new CloudflareAPI({
    accountID: "a".repeat(32),
    zoneID: "b".repeat(32),
    apiToken: "test-token",
    fetchAPI: async (url) => {
      const page = Number(new URL(url).searchParams.get("page"));
      const attempt = Math.floor(requestCount / 2);
      requestCount += 1;
      const result = pages.get(page) ?? [];
      return Response.json({
        success: true,
        result,
        result_info: {
          page,
          per_page: 50,
          count: result.length,
          total_pages: 2,
          total_count: attempt === 0 && page === 2 ? 3 : 2,
        },
      });
    },
  });
  assert.deepEqual(await drifting.listRules(), [{ id: "1" }, { id: "2" }]);
  assert.equal(requestCount, 6, "one drifted and two stable inventories are required");

  let duplicateRequests = 0;
  const duplicated = new CloudflareAPI({
    accountID: "a".repeat(32),
    zoneID: "b".repeat(32),
    apiToken: "test-token",
    fetchAPI: async (url) => {
      duplicateRequests += 1;
      const page = Number(new URL(url).searchParams.get("page"));
      return Response.json({
        success: true,
        result: [{ id: "d" }],
        result_info: {
          page,
          per_page: 50,
          count: 1,
          total_pages: 2,
          total_count: 2,
        },
      });
    },
  });
  await assert.rejects(() => duplicated.listRules(), /did not stabilize/);
  assert.equal(duplicateRequests, 8);
});

test("Cloudflare rule inventory rejects a page-shift hybrid and recovers on two stable snapshots", async () => {
  const id = (value) => value.toString(16).padStart(4, "0");
  const original = Array.from({ length: 100 }, (_, index) => ({
    id: id(index + 1),
    enabled: true,
    name: `rule-${index + 1}`,
  }));
  const current = Array.from({ length: 100 }, (_, index) => ({
    id: id(index + 2),
    enabled: true,
    name: `rule-${index + 2}`,
  }));
  let requestCount = 0;
  const api = new CloudflareAPI({
    accountID: "a".repeat(32),
    zoneID: "b".repeat(32),
    apiToken: "test-token",
    fetchAPI: async (url) => {
      const page = Number(new URL(url).searchParams.get("page"));
      const snapshot = Math.floor(requestCount / 2);
      requestCount += 1;
      // Snapshot zero reads old page one, then current page two. Its 100 unique
      // ids and unchanged metadata conceal a deleted rule and omit a live rule.
      const source = snapshot === 0 && page === 1 ? original : current;
      const result = source.slice((page - 1) * 50, page * 50);
      return Response.json({
        success: true,
        result,
        result_info: {
          page,
          per_page: 50,
          count: result.length,
          total_pages: 2,
          total_count: 100,
        },
      });
    },
  });

  const rules = await api.listRules();
  assert.deepEqual(rules, current);
  assert.equal(requestCount, 6, "hybrid, stable, stable snapshots must all be read");
  assert.equal(rules.some((rule) => rule.id === id(1)), false);
  assert.equal(rules.some((rule) => rule.id === id(51)), true);
});

test("Cloudflare rule inventory compares full content and fails closed on perpetual churn", async () => {
  let requestCount = 0;
  const api = new CloudflareAPI({
    accountID: "a".repeat(32),
    zoneID: "b".repeat(32),
    apiToken: "test-token",
    fetchAPI: async (url) => {
      requestCount += 1;
      const page = Number(new URL(url).searchParams.get("page"));
      const result = [{ id: "a", enabled: true, name: `revision-${requestCount}` }];
      return Response.json({
        success: true,
        result,
        result_info: {
          page,
          per_page: 50,
          count: 1,
          total_pages: 1,
          total_count: 1,
        },
      });
    },
  });

  await assert.rejects(
    () => api.listRules(),
    /did not stabilize after bounded retries/,
  );
  assert.equal(requestCount, 4);
});

test("Cloudflare resource and rule identifiers require true end of input", async () => {
  const valid = {
    CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
    CLOUDFLARE_ZONE_ID: "b".repeat(32),
    EMAIL_DIRECTORY_KV_ID: "c".repeat(32),
    CLOUDFLARE_API_TOKEN: "token",
  };
  assert.equal(cloudflareEnvironment(valid).zoneID, "b".repeat(32));
  assert.deepEqual(cloudflareEnvironment({
    ...valid,
    CF_ACCOUNT_ID: "f".repeat(32),
    CF_API_TOKEN: "conflicting-token",
  }), {
    accountID: valid.CLOUDFLARE_ACCOUNT_ID,
    zoneID: valid.CLOUDFLARE_ZONE_ID,
    namespaceID: valid.EMAIL_DIRECTORY_KV_ID,
    apiToken: valid.CLOUDFLARE_API_TOKEN,
  });
  for (const name of Object.keys(valid)) {
    assert.throws(
      () => cloudflareEnvironment({ ...valid, [name]: `${valid[name]}\n` }),
      /missing or invalid/,
    );
  }
  const api = new CloudflareAPI({
    accountID: valid.CLOUDFLARE_ACCOUNT_ID,
    zoneID: valid.CLOUDFLARE_ZONE_ID,
    apiToken: valid.CLOUDFLARE_API_TOKEN,
    fetchAPI: async () => Response.json({ success: true, result: {} }),
  });
  await assert.rejects(() => api.updateRule(`${"d".repeat(32)}\n`, {}), /invalid/);
  await assert.rejects(() => api.deleteRule(`${"d".repeat(32)}\n`), /invalid/);
});

test("Cloudflare client reads zone Email Routing settings without a mutation path", async () => {
  const calls = [];
  const fetchAPI = async (url, init) => {
    calls.push({ url, ...init });
    return Response.json({
      success: true,
      errors: [],
      messages: [],
      result: { enabled: true, status: "ready", support_subaddress: true },
    });
  };
  const api = new CloudflareAPI({
    accountID: "a".repeat(32), zoneID: "b".repeat(32), apiToken: "settings-token", fetchAPI,
  });

  assert.equal((await api.getEmailRoutingSettings()).support_subaddress, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "GET");
  assert.match(calls[0].url, new RegExp(`/zones/${"b".repeat(32)}/email/routing$`));
  assert.equal(typeof api.updateEmailRoutingSettings, "undefined");
});

test("Cloudflare client resolves the exact selected zone identity", async () => {
  const calls = [];
  const zoneID = "b".repeat(32);
  const zone = {
    id: zoneID,
    name: "witmail.net",
    status: "active",
    account: { id: "a".repeat(32), name: "Witwave" },
  };
  const api = new CloudflareAPI({
    accountID: zone.account.id,
    zoneID,
    apiToken: "zone-token",
    fetchAPI: async (url, init) => {
      calls.push({ url, ...init });
      return Response.json({ success: true, result: zone });
    },
  });

  assert.deepEqual(await api.getZone(), zone);
  assert.equal(calls.length, 1);
  assert.match(calls[0].url, new RegExp(`/zones/${zoneID}$`));
  assert.equal(calls[0].method, "GET");
});

test("Cloudflare client reads bounded route projections without a KV mutation", async () => {
  const calls = [];
  const projection = { schema_version: 2, state: "applied" };
  const fetchAPI = async (url, init) => {
    calls.push({ url, ...init });
    return new Response(JSON.stringify(projection), {
      headers: { "Content-Length": String(JSON.stringify(projection).length) },
    });
  };
  const api = new CloudflareAPI({
    accountID: "a".repeat(32),
    namespaceID: "c".repeat(32),
    apiToken: "projection-token",
    fetchAPI,
  });

  assert.deepEqual(await api.getKVJSON("email:realm-route:v1:witmail.net:abcdefghijkl2345"), projection);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "GET");
  assert.match(calls[0].url, /\/values\/email%3Arealm-route%3Av1%3Awitmail\.net%3Aabcdefghijkl2345$/);
  assert.equal(typeof api.putKV, "function");
  assert.equal(calls.some(({ method }) => method === "PUT" || method === "DELETE"), false);

  const oversized = new CloudflareAPI({
    accountID: "a".repeat(32),
    namespaceID: "c".repeat(32),
    apiToken: "projection-token",
    fetchAPI: async () => new Response("{}", { headers: { "Content-Length": "20000" } }),
  });
  await assert.rejects(() => oversized.getKVJSON("route"), /exceeded/);
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
    assert.throws(() => routeLookupSummaryQuery(value), /minutes must be an integer/);
  }
});

test("route lookup summary keeps custom domains and dependency errors value-free", async () => {
  const queries = [];
  const fetchAPI = async (_url, init) => {
    queries.push(init.body);
    if (init.body.includes("witself.agent-email.route-lookup.v1")) {
      return Response.json({
        data: [{
          result: "cp_error",
          evidence: "none",
          route_kind: "custom_domain",
          events: 2,
        }],
      });
    }
    return Response.json({ data: [{ outcome: "tempfail_route_lookup", events: 2 }] });
  };
  const output = await runMetrics(
    ["summary", "15"],
    {
      CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
      CLOUDFLARE_API_TOKEN: "analytics-token",
    },
    fetchAPI,
  );

  assert.equal(output.schema, "witself.agent-email.edge-summary.v2");
  assert.deepEqual(output.result.data, [
    { outcome: "tempfail_route_lookup", events: 2 },
  ]);
  assert.deepEqual(output.route_lookup_result.data, [{
    result: "cp_error",
    evidence: "none",
    route_kind: "custom_domain",
    events: 2,
  }]);
  assert.equal(queries.length, 2);
  const routeQuery = queries.find((query) =>
    query.includes("witself.agent-email.route-lookup.v1"));
  assert.match(routeQuery, /blob2 AS result/);
  assert.match(routeQuery, /blob3 AS evidence/);
  assert.match(routeQuery, /blob4 AS route_kind/);
  assert.match(routeQuery, /GROUP BY result, evidence, route_kind/);
  assert.doesNotMatch(
    routeQuery,
    /address|domain_request|account|realm_label|agent_id|message_id|subject/i,
  );
});
