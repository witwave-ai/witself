import assert from "node:assert/strict";
import test from "node:test";

import {
  bestPlacementCell,
  bestRebalanceCell,
  cellMatchesArchivedPlacement,
  cellMatchesPolicy,
  rebalanceImproves,
  rescuePlacementPolicy,
} from "../src/placement.mjs";

const cells = [
  { name: "gcp-use1-exp", cloud: "gcp", region_code: "use1", channel: "experimental" },
  { name: "gcp-usw2-stable", cloud: "gcp", region_code: "usw2", channel: "stable" },
  { name: "aws-usw2-edge", cloud: "aws", region_code: "usw2", channel: "edge" },
];

const basePolicy = {
  preferred_clouds: ["gcp", "aws", "azure"],
  preferred_regions: ["usw2", "use1"],
  preferred_channels: ["stable", "edge", "experimental"],
  allowed_clouds: [],
  allowed_regions: [],
  allowed_channels: [],
  rebalance_on: ["cloud", "channel"],
};

test("hard pins filter every placement dimension", () => {
  const policy = {
    ...basePolicy,
    allowed_clouds: ["gcp"],
    allowed_regions: ["use1"],
    allowed_channels: ["experimental"],
  };
  assert.equal(cellMatchesPolicy(cells[0], policy), true);
  assert.equal(cellMatchesPolicy(cells[1], policy), false);
  assert.equal(cellMatchesPolicy(cells[2], policy), false);
});

test("legacy archives require their native region unless explicitly overridden", () => {
  const archived = { region: "eastus2" };
  assert.equal(cellMatchesArchivedPlacement(cells[0], archived, false), false);
  assert.equal(cellMatchesArchivedPlacement(cells[0], archived, true), true);
});

test("restore ranks preferences before least-loaded tie breaking", () => {
  const counts = new Map(cells.map((cell) => [cell.name, 0]));
  assert.equal(
    bestPlacementCell(cells, { placement_policy: basePolicy }, counts, false)?.name,
    "gcp-usw2-stable",
  );

  const tied = cells.slice(0, 1).concat({ ...cells[0], name: "gcp-use1-exp-2" });
  const tiedCounts = new Map([["gcp-use1-exp", 2], ["gcp-use1-exp-2", 0]]);
  assert.equal(
    bestPlacementCell(tied, { placement_policy: basePolicy }, tiedCounts, false)?.name,
    "gcp-use1-exp-2",
  );
});

test("multi-axis rebalance cannot trade a preferred cloud for a better channel", () => {
  const current = { name: "current", cloud: "gcp", region_code: "use1", channel: "edge" };
  const target = { name: "target", cloud: "aws", region_code: "use1", channel: "stable" };
  assert.equal(rebalanceImproves(basePolicy, current, target), false);
});

test("an unselected axis does not block an explicitly selected improvement", () => {
  const policy = { ...basePolicy, rebalance_on: ["channel"] };
  const current = { name: "current", cloud: "gcp", region_code: "use1", channel: "edge" };
  const target = { name: "target", cloud: "aws", region_code: "use1", channel: "stable" };
  assert.equal(rebalanceImproves(policy, current, target), true);
});

test("hard-pin violations move even when preference rebalancing is disabled", () => {
  const policy = {
    ...basePolicy,
    allowed_clouds: ["gcp"],
    rebalance_on: [],
  };
  const current = cells[2];
  const result = bestRebalanceCell(cells, current, policy, new Map());
  assert.equal(result?.cell.name, "gcp-usw2-stable");
  assert.equal(result?.reason, "hard pin");
});

test("archive rescue clears only selected pins and preserves preferences", () => {
  const policy = {
    ...basePolicy,
    allowed_clouds: ["gcp"],
    allowed_regions: ["use1"],
    allowed_channels: ["stable"],
  };
  const rescued = rescuePlacementPolicy(policy, ["region", "channel"]);
  assert.deepEqual(rescued.allowed_clouds, ["gcp"]);
  assert.deepEqual(rescued.allowed_regions, []);
  assert.deepEqual(rescued.allowed_channels, []);
  assert.deepEqual(rescued.preferred_clouds, basePolicy.preferred_clouds);
  assert.deepEqual(rescued.rebalance_on, basePolicy.rebalance_on);
});

test("archive rescue gives legacy records a complete unpinned policy", () => {
  assert.deepEqual(rescuePlacementPolicy(null, ["cloud", "region", "channel"]), {
    preferred_clouds: [],
    preferred_regions: [],
    preferred_channels: [],
    allowed_clouds: [],
    allowed_regions: [],
    allowed_channels: [],
    rebalance_on: [],
  });
});
