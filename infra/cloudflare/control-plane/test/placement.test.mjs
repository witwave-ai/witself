import assert from "node:assert/strict";
import test from "node:test";

import {
  accountBackupSchedulingEnabled,
  bestPlacementCell,
  bestRebalanceCell,
  cellHasDestinationCredentials,
  cellIsEligibleDestination,
  cellMatchesArchivedPlacement,
  cellMatchesPolicy,
  rebalanceImproves,
  rescuePlacementPolicy,
} from "../src/placement.mjs";

const cells = [
  {
    name: "gcp-use1-exp",
    cloud: "gcp",
    region_code: "use1",
    channel: "experimental",
    endpoint: "https://gcp-use1.example",
    provision_token: "provision-gcp-use1",
  },
  {
    name: "gcp-usw2-stable",
    cloud: "gcp",
    region_code: "usw2",
    channel: "stable",
    endpoint: "https://gcp-usw2.example",
    provision_token: "provision-gcp-usw2",
  },
  {
    name: "aws-usw2-edge",
    cloud: "aws",
    region_code: "usw2",
    channel: "edge",
    endpoint: "https://aws-usw2.example",
    provision_token: "provision-aws-usw2",
  },
  {
    name: "civo-use1-exp",
    cloud: "civo",
    region_code: "use1",
    channel: "experimental",
    endpoint: "https://civo-use1.example",
    provision_token: "provision-civo-use1",
  },
];

const basePolicy = {
  preferred_clouds: ["gcp", "aws", "azure", "civo"],
  preferred_regions: ["usw2", "use1"],
  preferred_channels: ["stable", "edge", "experimental"],
  allowed_clouds: [],
  allowed_regions: [],
  allowed_channels: [],
  rebalance_on: ["cloud", "channel"],
};

test("backup scheduling gates every direct destination helper on a distinct backup token", () => {
  const legacy = cells[0];
  const capable = {
    ...legacy,
    name: "gcp-use1-backup-capable",
    backup_token: "backup-gcp-use1",
  };
  const sharedToken = {
    ...legacy,
    backup_token: legacy.provision_token,
  };
  const archived = { placement_policy: basePolicy };

  assert.equal(accountBackupSchedulingEnabled({}), false);
  assert.equal(
    accountBackupSchedulingEnabled({
      CP_ACCOUNT_BACKUPS_ENABLED: " TRUE ",
    }),
    true,
  );
  assert.equal(cellHasDestinationCredentials(legacy), true);
  assert.equal(
    cellHasDestinationCredentials(legacy, { backupsEnabled: true }),
    false,
  );
  assert.equal(
    cellHasDestinationCredentials(sharedToken, { backupsEnabled: true }),
    false,
  );
  assert.equal(
    cellIsEligibleDestination(legacy, { backupsEnabled: false }),
    true,
  );
  assert.equal(
    cellIsEligibleDestination(legacy, { backupsEnabled: true }),
    false,
  );
  assert.equal(
    bestPlacementCell(
      [legacy],
      archived,
      new Map(),
      false,
      { backupsEnabled: false },
    )?.name,
    legacy.name,
  );
  assert.equal(
    bestPlacementCell(
      [legacy],
      archived,
      new Map(),
      false,
      { backupsEnabled: true },
    ),
    null,
  );
  assert.equal(
    bestPlacementCell(
      [legacy, capable],
      archived,
      new Map(),
      false,
      { backupsEnabled: true },
    )?.name,
    capable.name,
  );
});

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

test("backup validation targets are excluded from placement and rebalance", () => {
  const validationCell = {
    ...cells[1],
    name: "gcp-usw2-validation",
    backup_validation_target: true,
  };
  const gcpOnly = {
    ...basePolicy,
    allowed_clouds: ["gcp"],
    rebalance_on: [],
  };
  assert.equal(cellMatchesPolicy(validationCell, gcpOnly), false);
  assert.equal(
    cellMatchesArchivedPlacement(
      validationCell,
      { region: validationCell.region_code },
      true,
    ),
    false,
  );
  assert.equal(
    bestPlacementCell(
      [validationCell],
      { placement_policy: gcpOnly },
      new Map(),
      true,
    ),
    null,
  );
  assert.equal(
    bestRebalanceCell(
      [cells[2], validationCell],
      cells[2],
      gcpOnly,
      new Map(),
    ),
    null,
  );
});

test("Civo participates in hard pins and experimental placement", () => {
  const policy = {
    ...basePolicy,
    preferred_clouds: ["civo", "gcp", "aws", "azure"],
    allowed_clouds: ["civo"],
    allowed_channels: ["experimental"],
  };
  assert.equal(cellMatchesPolicy(cells[3], policy), true);
  assert.equal(cellMatchesPolicy(cells[1], policy), false);
  assert.equal(
    bestPlacementCell(cells, { placement_policy: policy }, new Map(), false)?.name,
    "civo-use1-exp",
  );
});

test("Civo can be a preferred rebalance destination", () => {
  const policy = {
    ...basePolicy,
    preferred_clouds: ["civo", "gcp", "aws", "azure"],
  };
  const result = bestRebalanceCell(cells, cells[0], policy, new Map());
  assert.equal(result?.cell.name, "civo-use1-exp");
  assert.equal(result?.reason, "preferred placement");
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
