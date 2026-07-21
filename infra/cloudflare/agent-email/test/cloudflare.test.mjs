import assert from "node:assert/strict";
import test from "node:test";

import { CloudflareAPI } from "../scripts/cloudflare.mjs";

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
