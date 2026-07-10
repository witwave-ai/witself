const REBALANCE_AXIS_ORDER = ["cloud", "region", "channel"];
const ALLOWED_FIELD_BY_AXIS = {
  cloud: "allowed_clouds",
  region: "allowed_regions",
  channel: "allowed_channels",
};
const POLICY_LIST_FIELDS = [
  "preferred_clouds",
  "preferred_regions",
  "preferred_channels",
  "allowed_clouds",
  "allowed_regions",
  "allowed_channels",
  "rebalance_on",
];

export function policyList(policy, field) {
  const values = policy?.[field];
  return Array.isArray(values) ? values.filter((value) => typeof value === "string") : [];
}

export function rescuePlacementPolicy(policy, axes) {
  const next = {};
  for (const field of POLICY_LIST_FIELDS) {
    next[field] = [...policyList(policy, field)];
  }
  for (const axis of axes) {
    next[ALLOWED_FIELD_BY_AXIS[axis]] = [];
  }
  return next;
}

export function cellMatchesPolicy(cell, policy) {
  const allowedClouds = policyList(policy, "allowed_clouds");
  if (allowedClouds.length > 0 && !allowedClouds.includes(cell.cloud || "")) {
    return false;
  }
  const allowedRegions = policyList(policy, "allowed_regions");
  if (allowedRegions.length > 0 && !allowedRegions.includes(cell.region_code || "")) {
    return false;
  }
  const allowedChannels = policyList(policy, "allowed_channels");
  const channel = cell.channel || "experimental";
  if (allowedChannels.length > 0 && !allowedChannels.includes(channel)) {
    return false;
  }
  return true;
}

export function cellMatchesArchivedPlacement(cell, archived, allRegions) {
  const policy = archived?.placement_policy;
  if (policy) {
    return cellMatchesPolicy(cell, policy);
  }
  if (allRegions) {
    return true;
  }
  if (archived?.region_code && cell.region_code) {
    return archived.region_code === cell.region_code;
  }
  return archived?.region === cell.region;
}

function axisValue(cell, axis) {
  if (axis === "cloud") return cell.cloud || "";
  if (axis === "region") return cell.region_code || "";
  if (axis === "channel") return cell.channel || "experimental";
  return "";
}

function axisPreferenceField(axis) {
  if (axis === "cloud") return "preferred_clouds";
  if (axis === "region") return "preferred_regions";
  if (axis === "channel") return "preferred_channels";
  return "";
}

function preferenceRank(policy, field, value) {
  const preferred = policyList(policy, field);
  if (preferred.length === 0) {
    return 0;
  }
  const index = preferred.indexOf(value || "");
  return index >= 0 ? index : preferred.length;
}

export function comparePlacementCells(a, b, archived, counts) {
  const policy = archived?.placement_policy;
  const cloudRank = preferenceRank(policy, "preferred_clouds", a.cloud) -
    preferenceRank(policy, "preferred_clouds", b.cloud);
  if (cloudRank !== 0) return cloudRank;
  const regionRank = preferenceRank(policy, "preferred_regions", a.region_code) -
    preferenceRank(policy, "preferred_regions", b.region_code);
  if (regionRank !== 0) return regionRank;
  const channelRank = preferenceRank(policy, "preferred_channels", a.channel || "experimental") -
    preferenceRank(policy, "preferred_channels", b.channel || "experimental");
  if (channelRank !== 0) return channelRank;
  const countRank = (counts.get(a.name) ?? 0) - (counts.get(b.name) ?? 0);
  if (countRank !== 0) return countRank;
  return a.name.localeCompare(b.name);
}

export function bestPolicyCell(cells, policy, counts) {
  const eligible = cells.filter((cell) => cellMatchesPolicy(cell, policy));
  if (eligible.length === 0) {
    return null;
  }
  eligible.sort((a, b) => comparePlacementCells(
    a,
    b,
    { placement_policy: policy },
    counts,
  ));
  return eligible[0];
}

export function bestPlacementCell(cells, archived, counts, allRegions) {
  const eligible = cells.filter((cell) => cellMatchesArchivedPlacement(cell, archived, allRegions));
  if (eligible.length === 0) {
    return null;
  }
  eligible.sort((a, b) => comparePlacementCells(a, b, archived, counts));
  return eligible[0];
}

function rebalanceRank(policy, axis, cell) {
  const field = axisPreferenceField(axis);
  if (!field) {
    return 0;
  }
  return preferenceRank(policy, field, axisValue(cell, axis));
}

export function rebalanceImproves(policy, current, target) {
  const enabled = new Set(policyList(policy, "rebalance_on"));
  for (const axis of REBALANCE_AXIS_ORDER) {
    if (!enabled.has(axis)) {
      continue;
    }
    const targetRank = rebalanceRank(policy, axis, target);
    const currentRank = rebalanceRank(policy, axis, current);
    if (targetRank !== currentRank) {
      return targetRank < currentRank;
    }
  }
  return false;
}

export function bestRebalanceCell(cells, current, policy, counts) {
  if (!policy) {
    return null;
  }
  if (!cellMatchesPolicy(current, policy)) {
    const hardPinned = bestPolicyCell(cells, policy, counts);
    return hardPinned ? { cell: hardPinned, reason: "hard pin" } : null;
  }
  if (policyList(policy, "rebalance_on").length === 0) {
    return null;
  }
  const candidates = cells.filter((cell) =>
    cell.name !== current.name &&
    cellMatchesPolicy(cell, policy) &&
    rebalanceImproves(policy, current, cell));
  if (candidates.length === 0) {
    return null;
  }
  candidates.sort((a, b) => comparePlacementCells(
    a,
    b,
    { placement_policy: policy },
    counts,
  ));
  return { cell: candidates[0], reason: "preferred placement" };
}
