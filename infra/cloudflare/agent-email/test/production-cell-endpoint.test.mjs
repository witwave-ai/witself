import assert from "node:assert/strict";
import test from "node:test";
import { isProductionCellHost } from "../scripts/production-cell-endpoint.mjs";

test("production endpoint pins accept exact cell hosts only", () => {
  const host = "api.civo-prod-use1-serving.cells.witself.witwave.ai";
  for (const accepted of [host, "api.11111111-2222-4333-8444-555555555555.k8s.civo.com"]) {
    assert.equal(isProductionCellHost(accepted), true, accepted);
  }
  for (const rejected of [host.slice(4), `${host}.`, host.toUpperCase(), `extra.${host}`, `${host}.example.com`, `x${host}`]) {
    assert.equal(isProductionCellHost(rejected), false, rejected);
  }
});
